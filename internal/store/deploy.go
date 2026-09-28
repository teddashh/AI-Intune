package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

var (
	ErrBadScope                = errors.New("store: desired state scope must be machine or channel")
	ErrMachineRetired          = errors.New("store: machine retired")
	ErrJobNotFound             = errors.New("store: job not found")
	ErrLeaseInvalid            = errors.New("store: job lease invalid")
	ErrJobEventConflict        = errors.New("store: job event replay body conflict")
	ErrJobVerificationConflict = errors.New("store: job verification replay body conflict")
	ErrInvalidJobEvidence      = errors.New("store: job evidence metadata is invalid")
	ErrJobIrreversibility      = errors.New("store: job irreversibility does not match failure verdict")
	ErrNoVerification          = errors.New("store: job has no verification results")
	ErrVerificationFailed      = errors.New("store: job verification failed")
	// ⚠ 有通過的證據、但工作單還不在 verifying —— 那不是壞資料，是還沒到時候。
	ErrNotVerifying = errors.New("store: job is not waiting for verification")
	// ⚠ agent 送了一個只有 Hub 能下的判決（例如 VerificationPassed）。
	ErrEventNotForAgent = errors.New("store: agent may not send this event")
	// Hub 的無租約入口只給 reaper 落地它觀測到的 timeout／lease loss。
	// succeeded 只能由 MarkSucceededIfVerified 在同一條 SQL 裡驗完證據後寫入。
	ErrEventNotForHub                   = errors.New("store: hub may not send this event")
	ErrDesiredNotFound                  = errors.New("store: desired state not found")
	ErrDesiredRevisionExists            = errors.New("store: allocated resource revision already exists")
	ErrJobPrerequisiteInvalid           = errors.New("store: job prerequisite list is invalid")
	ErrJobPrerequisiteNotFound          = errors.New("store: prerequisite job not found")
	ErrJobPrerequisiteWrongMachine      = errors.New("store: prerequisite job belongs to another machine")
	ErrBadChannel                       = errors.New("store: machine channel must be canary, stable, or empty")
	ErrNeverObserved                    = errors.New("store: 這台從沒回報過，指派了也沒有 agent 會來領單")
	ErrManagedCatalogDeploymentRequired = errors.New("store: managed catalog writes require a profile assignment or deployment")
	ErrOpenClawDeploymentRequired       = ErrManagedCatalogDeploymentRequired
)

const (
	maxJobEventPayloadBytes        = 64 << 10
	maxJobVerificationCommandBytes = 16 << 10
	maxJobVerificationExcerptBytes = 64 << 10
	maxJobPrerequisites            = appcatalog.MaxDependencies

	JobEventProducerExecutorAgent    = "executor_agent"
	JobEventProducerHubScheduler     = "hub_scheduler"
	JobEventRoleExecutor             = "executor"
	JobEventRoleScheduler            = "scheduler"
	JobEventAuthorityMachineLease    = "machine_bearer_lease"
	JobEventAuthorityDependencyGraph = "dependency_graph"

	JobVerificationProducerExecutorAgent   = "executor_agent"
	JobVerificationRoleExecutor            = "executor"
	JobVerificationAuthorityMachineLease   = "machine_bearer_lease"
	JobVerificationRoleIndependent         = "independent_verifier"
	JobVerificationAuthorityVerifierBearer = "verifier_bearer"
)

const jobEventLegacyProducerTrigger = "tr_job_events_legacy_producer"

// Old Hub binaries omit event provenance columns. This trigger preserves their
// machine identity while provenance_recorded=0 keeps the historical distinction.
func ensureJobEventProvenanceTrigger(db *sql.DB) error {
	_, err := db.Exec(`CREATE TRIGGER IF NOT EXISTS ` + jobEventLegacyProducerTrigger + `
AFTER INSERT ON job_events
FOR EACH ROW
WHEN NEW.producer_kind='executor_agent'
 AND NEW.producer_id=''
 AND NEW.evidence_role='executor'
 AND NEW.authority='machine_bearer_lease'
 AND NEW.provenance_recorded=0
BEGIN
  UPDATE job_events
     SET producer_id=(SELECT machine_id FROM jobs WHERE job_id=NEW.job_id)
   WHERE event_id=NEW.event_id;
END`)
	return err
}

// DesiredState 是帳本裡的一筆期望狀態（跟 schema 同名同欄）。
type DesiredState struct {
	DesiredID    string
	ScopeType    string
	ScopeID      string
	ResourceKind string
	ResourceID   string
	Revision     deploy.Revision
	Spec         string
	CreatedAt    time.Time
	CreatedBy    string
}

// DesiredState 讀一筆期望狀態。工作單只帶 desired_id，agent 要做什麼寫在這裡的 spec。
func (s *Store) DesiredState(desiredID string) (DesiredState, error) {
	var d DesiredState
	var createdAt sql.NullString
	err := s.db.QueryRow(`
SELECT desired_id, scope_type, scope_id, resource_kind, resource_id, revision, spec, created_at, created_by
  FROM desired_state WHERE desired_id = ?`, desiredID).Scan(
		&d.DesiredID, &d.ScopeType, &d.ScopeID, &d.ResourceKind, &d.ResourceID,
		&d.Revision, &d.Spec, &createdAt, &d.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return DesiredState{}, ErrDesiredNotFound
	}
	if err != nil {
		return DesiredState{}, fmt.Errorf("store: read desired state: %w", err)
	}
	d.CreatedAt = parseTimeNull(createdAt)
	return d, nil
}

// Job 是帳本裡的一張工作單。
type Job struct {
	JobID            string          `json:"job_id"`
	MachineID        string          `json:"machine_id"`
	DesiredID        string          `json:"desired_id"`
	Revision         deploy.Revision `json:"revision"`
	State            deploy.JobState `json:"state"`
	LeaseToken       string          `json:"lease_token,omitempty"`
	LeaseExpiresAt   *time.Time      `json:"lease_expires_at,omitempty"`
	ExecutionTimeout int             `json:"execution_timeout"`
	ArtifactDigest   string          `json:"artifact_digest,omitempty"`
	Irreversible     bool            `json:"irreversible"`
	CreatedAt        time.Time       `json:"created_at"`
	TerminalAt       *time.Time      `json:"terminal_at,omitempty"`
}

// JobEvent 是 Hub 保存的一筆工作階段事件。OccurredAt 是事件來源的時間，
// ReceivedAt 是 Hub 寫入帳本的時間；Producer 欄位明確區分 agent 與 Hub scheduler。
type JobEvent struct {
	EventID            string
	JobID              string
	Seq                int
	Phase              string
	OccurredAt         time.Time
	ReceivedAt         time.Time
	Payload            string
	ProducerKind       string
	ProducerID         string
	EvidenceRole       string
	Authority          string
	ProvenanceRecorded bool
}

// JobPrerequisite is one immutable, ordered edge in a machine-local job graph.
type JobPrerequisite struct {
	JobID             string
	PrerequisiteJobID string
	Position          int
}

// JobVerification 是獨立於工作單狀態的驗證證據。
type JobVerification struct {
	VerificationID     string
	JobID              string
	MachineID          string
	RuleID             string
	Command            string
	ExitCode           *int
	StdoutExcerpt      string
	StderrExcerpt      string
	Passed             bool
	VerifiedAt         time.Time
	ProducerKind       string
	ProducerID         string
	EvidenceRole       string
	Authority          string
	ProvenanceRecorded bool
	ReceivedAt         time.Time
	ObservedDigest     string
	ObservedVersion    string
	VerifierID         string
}

// JobCount 是一台機器在一個工作單狀態下的列數。
type JobCount struct {
	MachineID string
	State     deploy.JobState
	Count     int
}

// LastTerminalJob 是一台機器在一個終態最近一次抵達終態的時間。
type LastTerminalJob struct {
	MachineID  string
	State      deploy.JobState
	TerminalAt time.Time
}

// ReapableJob 是 Hub 判斷租約與執行逾時需要的全部材料。
type ReapableJob struct {
	JobID            string
	MachineID        string
	State            deploy.JobState
	Irreversible     bool
	ExecutionTimeout int
	LeaseExpiresAt   *time.Time
	StartedAt        *time.Time
}

// AllocateRevision 替同一個資源範圍配發下一個單調遞增的 revision。
func (s *Store) AllocateRevision(scope string) (deploy.Revision, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin allocating revision: %w", err)
	}
	defer tx.Rollback()

	rev, err := allocateRevision(tx, scope)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit revision: %w", err)
	}
	return rev, nil
}

// allocateRevision 用一個 SQL 敘述同時建立或遞增計數器並取回號碼。
//
// ⚠ 這擋的是兩個併發呼叫先讀到同一個舊值，再各自寫回同一個 revision。
func allocateRevision(tx *sql.Tx, scope string) (deploy.Revision, error) {
	var rev deploy.Revision
	if err := tx.QueryRow(`
INSERT INTO revision_counters (resource_scope, current_revision)
VALUES (?, 1)
ON CONFLICT(resource_scope) DO UPDATE
SET current_revision = revision_counters.current_revision + 1
RETURNING current_revision`, scope).Scan(&rev); err != nil {
		return 0, fmt.Errorf("store: allocate revision: %w", err)
	}
	return rev, nil
}

// CreateDesiredState 配號並把一筆期望狀態寫入帳本。
func (s *Store) CreateDesiredState(scopeType, scopeID, resourceKind, resourceID, spec, createdBy string) (desiredID string, rev deploy.Revision, err error) {
	// ⚠ 這擋的是把重疊的 tag 當成部署範圍，導致無法計算實際爆炸半徑。
	if scopeType != "machine" && scopeType != "channel" {
		return "", 0, ErrBadScope
	}
	if directManagedCatalogSpec(spec) {
		return "", 0, ErrManagedCatalogDeploymentRequired
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", 0, fmt.Errorf("store: begin desired state: %w", err)
	}
	defer tx.Rollback()

	desiredID, rev, err = createDesiredStateTx(tx, scopeType, scopeID, resourceKind, resourceID, spec, createdBy, s.now())
	if err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("store: commit desired state: %w", err)
	}
	return desiredID, rev, nil
}

func createDesiredStateTx(tx *sql.Tx, scopeType, scopeID, resourceKind, resourceID, spec, createdBy string, now time.Time) (string, deploy.Revision, error) {
	if scopeType != "machine" && scopeType != "channel" {
		return "", 0, ErrBadScope
	}
	// ⚠ 計數器的 key 是「資源」，**不是**「這一次的 scope」。
	// 同一個資源在整個機隊上共用一條號碼帶，於是 machine 與 channel scope
	// 永遠拿得到可比較的先後；否則 agent 的 MaxSeen 擋不住跨 scope 降版。
	rev, err := allocateRevision(tx, resourceKind+":"+resourceID)
	if err != nil {
		return "", 0, err
	}
	// ⚠ 計數器缺失或被改成 0 時，配號會再從 1 起跳。同一資源若已有該
	// revision，再寫一筆會讓帳本出現歧義號碼；回放端只能 fail closed。
	var taken bool
	if err := tx.QueryRow(`
SELECT EXISTS (
  SELECT 1 FROM desired_state
  WHERE resource_kind=? AND resource_id=? AND revision=?
)`, resourceKind, resourceID, rev).Scan(&taken); err != nil {
		return "", 0, fmt.Errorf("store: inspect desired state revision: %w", err)
	}
	if taken {
		return "", 0, ErrDesiredRevisionExists
	}
	desiredID := newID()
	if _, err := tx.Exec(`
INSERT INTO desired_state
  (desired_id, scope_type, scope_id, resource_kind, resource_id, revision, spec, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, desiredID, scopeType, scopeID, resourceKind, resourceID,
		rev, spec, fmtTime(now), createdBy); err != nil {
		return "", 0, fmt.Errorf("store: create desired state: %w", err)
	}
	return desiredID, rev, nil
}

// NewJob 是開一張工作單需要的、除了「給誰、做哪一版」以外的東西。
//
// ⚠ ArtifactDigest 由 **Hub** 在這裡釘死，agent 只負責驗。這是不做 mTLS
// 那筆債的償還之二（docs/PHASE1.md §4 不變量 2）—— agent 在 activation
// 之前對不上就 ARTIFACT_HASH_MISMATCH，symlink 不准動。
//
// ⚠ Irreversible 必須在開單的時候就決定，不能等失敗了再問。
// 它決定的是失敗之後對人講哪一句話：failed 說「已經回到舊版」、
// manual_intervention 說「沒有回退，機器停在中間」。
// 事後才補的旗標，預設值是 false —— 於是不可逆的失敗會被說成回退過了。
type NewJob struct {
	ArtifactDigest     string
	Irreversible       bool
	ExecutionTimeout   int // 秒；0 表示用 schema 的預設值 900
	PrerequisiteJobIDs []string
}

// CreateJob 替一台未退役的機器建立尚未開始的工作單。
func (s *Store) CreateJob(machineID, desiredID string, revision deploy.Revision, n NewJob) (jobID string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("store: begin job: %w", err)
	}
	defer tx.Rollback()
	jobID, err = createJobTx(tx, machineID, desiredID, revision, n, s.now())
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit job: %w", err)
	}
	return jobID, nil
}

func createJobTx(tx *sql.Tx, machineID, desiredID string, revision deploy.Revision, n NewJob, now time.Time) (string, error) {
	return createJobTxWithPolicy(tx, machineID, desiredID, revision, n, now, false)
}

// createManagedJobTx is reserved for Store mutations that have already bound
// an executable typed adapter and exact artifact material into their own
// atomic receipt. Public/direct job creation continues to reject managed
// catalog executors.
func createManagedJobTx(tx *sql.Tx, machineID, desiredID string, revision deploy.Revision, n NewJob, now time.Time) (string, error) {
	return createJobTxWithPolicy(tx, machineID, desiredID, revision, n, now, true)
}

func createJobTxWithPolicy(tx *sql.Tx, machineID, desiredID string, revision deploy.Revision,
	n NewJob, now time.Time, managedAdapter bool,
) (string, error) {
	if n.ExecutionTimeout <= 0 {
		n.ExecutionTimeout = 900
	}
	if len(n.PrerequisiteJobIDs) > maxJobPrerequisites {
		return "", fmt.Errorf("%w: at most %d prerequisites", ErrJobPrerequisiteInvalid, maxJobPrerequisites)
	}
	seen := make(map[string]struct{}, len(n.PrerequisiteJobIDs))
	for _, prerequisiteID := range n.PrerequisiteJobIDs {
		if !validJobReadRouteIdentifier(prerequisiteID, 256) {
			return "", fmt.Errorf("%w: malformed prerequisite job ID", ErrJobPrerequisiteInvalid)
		}
		if _, exists := seen[prerequisiteID]; exists {
			return "", fmt.Errorf("%w: duplicate prerequisite job ID", ErrJobPrerequisiteInvalid)
		}
		seen[prerequisiteID] = struct{}{}
		var owner string
		err := tx.QueryRow(`SELECT machine_id FROM jobs WHERE job_id=?`, prerequisiteID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrJobPrerequisiteNotFound, prerequisiteID)
		}
		if err != nil {
			return "", fmt.Errorf("store: inspect prerequisite job: %w", err)
		}
		if owner != machineID {
			return "", fmt.Errorf("%w: %s", ErrJobPrerequisiteWrongMachine, prerequisiteID)
		}
	}

	var spec string
	if err := tx.QueryRow(`SELECT spec FROM desired_state WHERE desired_id=?`, desiredID).Scan(&spec); errors.Is(err, sql.ErrNoRows) {
		return "", ErrDesiredNotFound
	} else if err != nil {
		return "", fmt.Errorf("store: inspect desired state before job create: %w", err)
	}
	if directManagedCatalogSpec(spec) && !managedAdapter {
		return "", ErrManagedCatalogDeploymentRequired
	}

	jobID := newID()
	// ⚠ WHERE 的 retired_at 守衛擋的是替已退役、再也不會來領單的機器建立永久 pending 工作單。
	err := tx.QueryRow(`
INSERT INTO jobs (job_id, machine_id, desired_id, revision, state, created_at,
                  artifact_digest, irreversible, execution_timeout)
SELECT ?, machine_id, ?, ?, ?, ?, ?, ?, ?
  FROM machine_registry
 WHERE machine_id = ? AND retired_at IS NULL
RETURNING job_id`, jobID, desiredID, revision, deploy.NotStarted, fmtTime(now),
		nullIfEmpty(n.ArtifactDigest), n.Irreversible, n.ExecutionTimeout, machineID).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		var retiredAt sql.NullString
		lookupErr := tx.QueryRow(
			`SELECT retired_at FROM machine_registry WHERE machine_id = ?`, machineID).Scan(&retiredAt)
		// ⚠ 這個守衛把上述拒絕辨認成退役，擋的是用一般找不到錯誤掩蓋真正原因。
		if lookupErr == nil && retiredAt.Valid && retiredAt.String != "" {
			return "", ErrMachineRetired
		}
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		if lookupErr != nil {
			return "", fmt.Errorf("store: inspect job machine: %w", lookupErr)
		}
		return "", fmt.Errorf("store: machine %s could not receive job", machineID)
	}
	if err != nil {
		return "", fmt.Errorf("store: create job: %w", err)
	}
	for position, prerequisiteID := range n.PrerequisiteJobIDs {
		if _, err := tx.Exec(`INSERT INTO job_dependencies
 (job_id,prerequisite_job_id,position) VALUES (?,?,?)`, jobID, prerequisiteID, position); err != nil {
			return "", fmt.Errorf("store: create job dependency: %w", err)
		}
	}
	return jobID, nil
}

func directManagedCatalogSpec(raw string) bool {
	kind, _, err := model.ParseJobSpec([]byte(raw))
	if err != nil {
		return false
	}
	for _, registered := range agentadapter.ExecutorKinds() {
		if kind == registered {
			return true
		}
	}
	return false
}

// JobForMachine 只回傳同時符合工作單與機器身分的那一列。
func (s *Store) JobForMachine(jobID, machineID string) (Job, error) {
	// ⚠ machine_id 直接進 WHERE，擋的是呼叫端漏做事後比對而把別人的工作單交出去。
	job, err := scanJob(s.db.QueryRow(`
SELECT job_id, machine_id, desired_id, revision, state,
       lease_token, lease_expires_at, execution_timeout, artifact_digest,
       irreversible, created_at, terminal_at
  FROM jobs
 WHERE job_id = ? AND machine_id = ?`, jobID, machineID))
	// ⚠ 不存在與不屬於這台機器共用同一個錯誤，擋的是藉錯誤差異列舉 job_id。
	if errors.Is(err, sql.ErrNoRows) {
		s.logJobLookupMiss(jobID, machineID)
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: get job for machine: %w", err)
	}
	return job, nil
}

// Job 依 job_id 讀一張工作單，不做機器歸屬過濾。這條路只給人看的 Hub 頁面使用；
// agent 的協定路徑仍然只能呼叫 JobForMachine。
func (s *Store) Job(jobID string) (Job, error) {
	job, err := scanJob(s.db.QueryRow(`
SELECT job_id, machine_id, desired_id, revision, state,
       lease_token, lease_expires_at, execution_timeout, artifact_digest,
       irreversible, created_at, terminal_at
  FROM jobs
 WHERE job_id = ?`, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: get job: %w", err)
	}
	return job, nil
}

// JobEvents 依 seq 列出一張工作單的所有事件。
func (s *Store) JobEvents(jobID string) ([]JobEvent, error) {
	rows, err := s.db.Query(`
SELECT event_id, job_id, seq, phase, occurred_at, received_at, payload,
       producer_kind,producer_id,evidence_role,authority,provenance_recorded
  FROM job_events
 WHERE job_id = ?
 ORDER BY seq ASC, received_at ASC, event_id ASC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: list job events: %w", err)
	}
	defer rows.Close()
	var out []JobEvent
	for rows.Next() {
		var e JobEvent
		var occurredAt, receivedAt string
		if err := rows.Scan(&e.EventID, &e.JobID, &e.Seq, &e.Phase, &occurredAt, &receivedAt, &e.Payload,
			&e.ProducerKind, &e.ProducerID, &e.EvidenceRole, &e.Authority, &e.ProvenanceRecorded); err != nil {
			return nil, fmt.Errorf("store: scan job event: %w", err)
		}
		e.OccurredAt, e.ReceivedAt = parseTime(occurredAt), parseTime(receivedAt)
		out = append(out, e)
	}
	return out, rows.Err()
}

// JobPrerequisites returns the graph edges in the resolver's stable order.
func (s *Store) JobPrerequisites(jobID string) ([]JobPrerequisite, error) {
	rows, err := s.db.Query(`SELECT job_id,prerequisite_job_id,position
 FROM job_dependencies WHERE job_id=? ORDER BY position`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: list job prerequisites: %w", err)
	}
	defer rows.Close()
	var result []JobPrerequisite
	for rows.Next() {
		var edge JobPrerequisite
		if err := rows.Scan(&edge.JobID, &edge.PrerequisiteJobID, &edge.Position); err != nil {
			return nil, fmt.Errorf("store: scan job prerequisite: %w", err)
		}
		result = append(result, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: finish job prerequisites: %w", err)
	}
	return result, nil
}

// JobVerifications 依回報的驗證時間列出一張工作單的驗證結果。
// 新列保存 producer/role/Hub received_at；升級前列保留 provenance_recorded=false。
// Independent rows additionally carry verifier_id and the digest/version actually observed.
func (s *Store) JobVerifications(jobID string) ([]JobVerification, error) {
	rows, err := s.db.Query(`
SELECT verification_id, job_id, machine_id, rule_id, command, exit_code,
       stdout_excerpt, stderr_excerpt, passed, verified_at,producer_kind,producer_id,
       evidence_role,authority,provenance_recorded,received_at,observed_digest,observed_version,verifier_id
  FROM verification_results
 WHERE job_id = ?
 ORDER BY verified_at ASC, verification_id ASC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: list job verifications: %w", err)
	}
	defer rows.Close()
	var out []JobVerification
	for rows.Next() {
		var v JobVerification
		var exitCode sql.NullInt64
		var stdout, stderr, verifiedAt, receivedAt sql.NullString
		if err := rows.Scan(&v.VerificationID, &v.JobID, &v.MachineID, &v.RuleID, &v.Command,
			&exitCode, &stdout, &stderr, &v.Passed, &verifiedAt, &v.ProducerKind, &v.ProducerID,
			&v.EvidenceRole, &v.Authority, &v.ProvenanceRecorded, &receivedAt,
			&v.ObservedDigest, &v.ObservedVersion, &v.VerifierID); err != nil {
			return nil, fmt.Errorf("store: scan job verification: %w", err)
		}
		if exitCode.Valid {
			n := int(exitCode.Int64)
			v.ExitCode = &n
		}
		v.StdoutExcerpt, v.StderrExcerpt = stdout.String, stderr.String
		v.VerifiedAt = parseTimeNull(verifiedAt)
		v.ReceivedAt = parseTimeNull(receivedAt)
		out = append(out, v)
	}
	return out, rows.Err()
}

// JobCounts 回傳帳本裡每台機器、每個實際出現狀態的列數。
func (s *Store) JobCounts() ([]JobCount, error) {
	rows, err := s.db.Query(`
SELECT machine_id, state, COUNT(*)
  FROM jobs
 GROUP BY machine_id, state
 ORDER BY machine_id ASC, state ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: count jobs: %w", err)
	}
	defer rows.Close()
	var out []JobCount
	for rows.Next() {
		var c JobCount
		if err := rows.Scan(&c.MachineID, &c.State, &c.Count); err != nil {
			return nil, fmt.Errorf("store: scan job count: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LastTerminalPerMachine 回傳每台機器、每個終態最近一次的 terminal_at。
func (s *Store) LastTerminalPerMachine() ([]LastTerminalJob, error) {
	rows, err := s.db.Query(`
SELECT machine_id, state, MAX(terminal_at)
  FROM jobs
 WHERE state IN (?, ?, ?, ?, ?) AND terminal_at IS NOT NULL
 GROUP BY machine_id, state
 ORDER BY machine_id ASC, state ASC`, deploy.Succeeded, deploy.Failed, deploy.Rejected,
		deploy.LeaseExpired, deploy.ManualIntervention)
	if err != nil {
		return nil, fmt.Errorf("store: last terminal jobs: %w", err)
	}
	defer rows.Close()
	var out []LastTerminalJob
	for rows.Next() {
		var r LastTerminalJob
		var terminalAt string
		if err := rows.Scan(&r.MachineID, &r.State, &terminalAt); err != nil {
			return nil, fmt.Errorf("store: scan last terminal job: %w", err)
		}
		r.TerminalAt = parseTime(terminalAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReapableJobs 回傳所有非終態工作單。started_at 取 start 事件的 received_at；
// Hub 的逾時判決不可採信 agent 的 occurred_at。
func (s *Store) ReapableJobs() ([]ReapableJob, error) {
	rows, err := s.db.Query(`
SELECT j.job_id, j.machine_id, j.state, j.irreversible, j.execution_timeout,
       j.lease_expires_at,
       (SELECT e.received_at
          FROM job_events e
         WHERE e.job_id = j.job_id AND e.seq = 1 AND e.phase = 'start'
         LIMIT 1) AS started_at
  FROM jobs j
 WHERE j.state NOT IN (?, ?, ?, ?, ?)
 ORDER BY j.created_at ASC, j.job_id ASC`, deploy.Succeeded, deploy.Failed, deploy.Rejected,
		deploy.LeaseExpired, deploy.ManualIntervention)
	if err != nil {
		return nil, fmt.Errorf("store: reapable jobs: %w", err)
	}
	defer rows.Close()
	var out []ReapableJob
	for rows.Next() {
		var j ReapableJob
		var leaseExpiresAt, startedAt sql.NullString
		if err := rows.Scan(&j.JobID, &j.MachineID, &j.State, &j.Irreversible,
			&j.ExecutionTimeout, &leaseExpiresAt, &startedAt); err != nil {
			return nil, fmt.Errorf("store: scan reapable job: %w", err)
		}
		j.LeaseExpiresAt, j.StartedAt = parseTimePtr(leaseExpiresAt), parseTimePtr(startedAt)
		out = append(out, j)
	}
	return out, rows.Err()
}

// logJobLookupMiss 把對外相同的查詢失敗，在 Hub log 裡分成不存在與歸屬不符。
func (s *Store) logJobLookupMiss(jobID, machineID string) {
	var owner string
	err := s.db.QueryRow(`SELECT machine_id FROM jobs WHERE job_id = ?`, jobID).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		log.Printf("找不到工作單 job=%s（查詢機器=%s）", jobID, machineID)
	case err != nil:
		log.Printf("無法判斷工作單查詢失敗原因 job=%s machine=%s：%v", jobID, machineID, err)
	default:
		log.Printf("工作單歸屬不符 job=%s owner=%s requester=%s", jobID, owner, machineID)
	}
}

// NextJobForMachine returns the oldest runnable resource head for a machine.
// ListJobs 列出工作單，最新的在前。machineID 空字串 = 全部機器。
// 這是給 CLI 看狀態用的，不是協定的一部分：協定只認 JobForMachine / NextJobForMachine。
func (s *Store) ListJobs(machineID string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
SELECT job_id, machine_id, desired_id, revision, state,
       lease_token, lease_expires_at, execution_timeout, artifact_digest,
       irreversible, created_at, terminal_at
  FROM jobs
 WHERE (? = '' OR machine_id = ?)
 ORDER BY created_at DESC, revision DESC, job_id DESC
 LIMIT ?`, machineID, machineID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// LatestSucceededJobForResource 回一台機器最近成功且屬於指定資源的工作單。
func (s *Store) LatestSucceededJobForResource(machineID, resourceKind, resourceID string) (Job, bool, error) {
	job, err := scanJob(s.db.QueryRow(`
SELECT j.job_id, j.machine_id, j.desired_id, j.revision, j.state,
       j.lease_token, j.lease_expires_at, j.execution_timeout, j.artifact_digest,
       j.irreversible, j.created_at, j.terminal_at
  FROM jobs j JOIN desired_state d ON d.desired_id = j.desired_id
 WHERE j.machine_id = ? AND j.state = ? AND d.resource_kind = ? AND d.resource_id = ?
 ORDER BY j.terminal_at DESC, j.created_at DESC, j.job_id DESC
 LIMIT 1`, machineID, deploy.Succeeded, resourceKind, resourceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("store: latest succeeded resource job: %w", err)
	}
	return job, true, nil
}

func (s *Store) NextJobForMachine(machineID string) (Job, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, false, fmt.Errorf("store: begin next job read: %w", err)
	}
	defer tx.Rollback()

	if err := rejectDependencyBlockedJobsTx(tx, machineID, s.now()); err != nil {
		return Job{}, false, err
	}

	// Revision only orders jobs for the same resource. Revisions from unrelated
	// resources are independent counters, so creation rowid chooses among their
	// runnable heads.
	job, err := scanJob(tx.QueryRow(`
SELECT j.job_id, j.machine_id, j.desired_id, j.revision, j.state,
       j.lease_token, j.lease_expires_at, j.execution_timeout, j.artifact_digest,
       j.irreversible, j.created_at, j.terminal_at
  FROM jobs j
  JOIN desired_state desired ON desired.desired_id = j.desired_id
 WHERE j.machine_id = ? AND j.state = ?
   AND NOT EXISTS (
       SELECT 1
         FROM job_dependencies edge
         LEFT JOIN jobs prerequisite ON prerequisite.job_id = edge.prerequisite_job_id
        WHERE edge.job_id = j.job_id
          AND (prerequisite.job_id IS NULL OR prerequisite.state <> ?)
   )
   AND NOT EXISTS (
       SELECT 1
         FROM jobs earlier
         JOIN desired_state earlier_desired ON earlier_desired.desired_id = earlier.desired_id
        WHERE earlier.machine_id = j.machine_id
          AND earlier.state NOT IN (?, ?, ?, ?, ?)
          AND earlier_desired.resource_kind = desired.resource_kind
          AND earlier_desired.resource_id = desired.resource_id
          AND earlier.revision < j.revision
   )
 ORDER BY j.rowid ASC
	LIMIT 1`, machineID, deploy.NotStarted, deploy.Succeeded, deploy.Succeeded,
		deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention))
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return Job{}, false, fmt.Errorf("store: finish empty next job read: %w", err)
		}
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("store: get next job for machine: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, fmt.Errorf("store: finish next job read: %w", err)
	}
	return job, true, nil
}

func rejectDependencyBlockedJobsTx(tx *sql.Tx, machineID string, now time.Time) error {
	now = now.UTC()
	for {
		var childID, prerequisiteID string
		var prerequisiteState deploy.JobState
		err := tx.QueryRow(`
SELECT child.job_id, prerequisite.job_id, prerequisite.state
  FROM job_dependencies edge
  JOIN jobs child ON child.job_id = edge.job_id
  JOIN jobs prerequisite ON prerequisite.job_id = edge.prerequisite_job_id
 WHERE child.machine_id = ?
   AND child.state = ?
   AND prerequisite.state IN (?, ?, ?, ?)
 ORDER BY child.rowid ASC, edge.position ASC, prerequisite.rowid ASC
 LIMIT 1`, machineID, deploy.NotStarted, deploy.Failed, deploy.Rejected,
			deploy.LeaseExpired, deploy.ManualIntervention).Scan(
			&childID, &prerequisiteID, &prerequisiteState)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: find dependency-blocked job: %w", err)
		}

		payloadBytes, err := json.Marshal(model.JobRejectEventPayload{
			RejectionCode: string(deploy.DependencyFailed),
			Detail:        fmt.Sprintf("prerequisite job %s ended in %s", prerequisiteID, prerequisiteState),
		})
		if err != nil {
			return fmt.Errorf("store: encode dependency rejection: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO job_events
 (event_id,job_id,seq,phase,occurred_at,received_at,payload,
  producer_kind,producer_id,evidence_role,authority,provenance_recorded)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,1)`, newID(), childID, 1, "rejected", fmtTime(now),
			fmtTime(now), string(payloadBytes), JobEventProducerHubScheduler, "hub",
			JobEventRoleScheduler, JobEventAuthorityDependencyGraph); err != nil {
			return fmt.Errorf("store: record dependency rejection: %w", err)
		}
		res, err := tx.Exec(`UPDATE jobs
 SET state=?,terminal_at=?,lease_token=NULL,lease_expires_at=NULL
 WHERE job_id=? AND state=?`, deploy.Rejected, fmtTime(now), childID, deploy.NotStarted)
		if err != nil {
			return fmt.Errorf("store: reject dependency-blocked job: %w", err)
		}
		if affected, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: count dependency-blocked job rejection: %w", err)
		} else if affected != 1 {
			return errors.New("store: dependency-blocked job changed during scheduler transaction")
		}
	}
}

// ClaimJob 只允許領取從未開始的工作單，並核發新的 fencing token。
func (s *Store) ClaimJob(jobID, machineID string, now time.Time, lease time.Duration) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	now = now.UTC()

	// ⚠⚠ 狀態與歸屬守衛全在同一個 UPDATE，擋的是兩個 agent
	// 先各自 SELECT 到尚未領取，再各自寫入並同時以為自己領取成功。
	// ⚠ 這裡刻意不讓過期租約重新認領：同一個 job_id 重跑時 agent 的 seq
	// 會從 1 重新開始，跟第一次執行的事件發生 replay conflict，兩次執行的
	// 證據也無法再只靠 seq 分清。過期單由 reaper 收成 lease_expired；
	// 要重做同一個期望狀態必須另開新 job_id。
	res, err := s.db.Exec(`
UPDATE jobs
   SET state = ?, lease_token = ?, lease_expires_at = ?
 WHERE job_id = ?
   AND machine_id = ?
   AND state = ?
   AND NOT EXISTS (
       SELECT 1 FROM job_dependencies edge
       LEFT JOIN jobs prerequisite ON prerequisite.job_id=edge.prerequisite_job_id
       WHERE edge.job_id=jobs.job_id
         AND (prerequisite.job_id IS NULL OR prerequisite.state<>?)
   )`, deploy.Claimed, token, fmtTime(now.Add(lease)), jobID, machineID,
		deploy.NotStarted, deploy.Succeeded)
	if err != nil {
		return "", fmt.Errorf("store: claim job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("store: count claimed jobs: %w", err)
	}
	if n != 1 {
		// ⚠ 歸屬不符與不存在共用同一個錯誤，避免用領單結果列舉 job_id。
		return "", ErrJobNotFound
	}
	return token, nil
}

// RenewLease 只替目前 token 的持有者延長尚未過期的租約。
func (s *Store) RenewLease(jobID, machineID, leaseToken string, now time.Time, lease time.Duration) error {
	now = now.UTC()
	// ⚠⚠ lease_token 直接放在 WHERE；重新領單一換 token，舊持有者便無法續租。
	res, err := s.db.Exec(`
UPDATE jobs
   SET lease_expires_at = ?
 WHERE job_id = ?
   AND machine_id = ?
   AND lease_token = ?
   AND lease_expires_at >= ?
   AND state IN (?, ?, ?)`, fmtTime(now.Add(lease)), jobID, machineID, leaseToken,
		fmtTime(now), deploy.Claimed, deploy.Running, deploy.Verifying)
	if err != nil {
		return fmt.Errorf("store: renew job lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: count renewed leases: %w", err)
	}
	if n != 1 {
		return ErrLeaseInvalid
	}
	return nil
}

// AppendJobEvent 追加一筆 agent 階段事件。同序號、同 canonical body 的重送
// 是冪等成功；同序號卻不同內容是協定衝突，不能安靜吃掉。
func (s *Store) AppendJobEvent(jobID, machineID, leaseToken string, seq int, phase, payload string, occurredAt, receivedAt time.Time) error {
	_, err := s.AppendJobEventWithReplay(jobID, machineID, leaseToken, seq, phase, payload, occurredAt, receivedAt)
	return err
}

// AppendJobEventWithReplay has the same persistence contract as
// AppendJobEvent and additionally tells an HTTP adapter whether the row was
// already present. That bit matters for phase events: a replay must not apply
// the state transition a second time, especially after the job is terminal.
func (s *Store) AppendJobEventWithReplay(jobID, machineID, leaseToken string, seq int, phase, payload string, occurredAt, receivedAt time.Time) (bool, error) {
	replayOnly := seq < 0 ||
		!validJobEvidenceTime(occurredAt) ||
		!validJobEvidenceTime(receivedAt) ||
		!validJobReadStoredIdentifier(phase, 256) ||
		len(payload) > maxJobEventPayloadBytes
	var incomingPayload string
	var err error
	if !replayOnly {
		incomingPayload, err = canonicalJobEventPayload(payload)
		if err != nil {
			return false, fmt.Errorf("%w: canonicalize job event payload: %v", ErrInvalidJobEvidence, err)
		}
	}
	// ⚠ INSERT 的來源列同時驗歸屬與 token，擋的是驗完 token 後租約被重新領走，
	// 舊持有者卻仍在另一個 SQL 敘述中把事件寫進來。
	inserted := false
	if !replayOnly {
		res, err := s.db.Exec(`
INSERT INTO job_events (event_id, job_id, seq, phase, occurred_at, received_at, payload,
                        producer_kind,producer_id,evidence_role,authority,provenance_recorded)
SELECT ?, job_id, ?, ?, ?, ?, ?, ?, machine_id, ?, ?, 1
  FROM jobs
 WHERE job_id = ? AND machine_id = ? AND lease_token = ?
   AND state IN (?, ?, ?)
ON CONFLICT(job_id, seq) DO NOTHING`, newID(), seq, phase, fmtTime(occurredAt),
			fmtTime(receivedAt), payload, JobEventProducerExecutorAgent, JobEventRoleExecutor,
			JobEventAuthorityMachineLease, jobID, machineID, leaseToken,
			deploy.Claimed, deploy.Running, deploy.Verifying)
		if err != nil {
			return false, fmt.Errorf("store: append job event: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("store: count appended job events: %w", err)
		}
		inserted = n == 1
	}
	if inserted {
		return false, nil
	}

	// RowsAffected=0 可能是合法回放，也可能是租約不符。Active job 仍要求
	// fencing token；terminal job 則只允許該 machine 回放已存在的事件，讓
	// response 丟失後的重送仍可核對 exact body。這也避免 /reject 在終態後把
	// 同 seq、不同內容錯當成原結果回放。received_at 是 Hub 每次收件的時間，
	// 不是 request identity 的一部分。
	var existingState deploy.JobState
	var existingToken, existingPhase, existingOccurredAt, existingPayload string
	err = s.db.QueryRow(`
SELECT job.state, COALESCE(job.lease_token,''), event.phase, event.occurred_at, event.payload
  FROM jobs AS job
  JOIN job_events AS event ON event.job_id = job.job_id AND event.seq = ?
 WHERE job.job_id = ? AND job.machine_id = ?`, seq, jobID, machineID).
		Scan(&existingState, &existingToken, &existingPhase, &existingOccurredAt, &existingPayload)
	if errors.Is(err, sql.ErrNoRows) {
		if replayOnly {
			return false, ErrInvalidJobEvidence
		}
		return false, ErrLeaseInvalid
	}
	if err != nil {
		return false, fmt.Errorf("store: validate replayed job event lease: %w", err)
	}
	if !deploy.IsTerminal(existingState) && existingToken != leaseToken {
		return false, ErrLeaseInvalid
	}
	// A payload/phase beyond today's ingress bounds is parsed only after an
	// existing row proves this can be a retry accepted by an older Hub. New
	// oversized input therefore fails without spending work canonicalizing it.
	if replayOnly {
		incomingPayload, err = canonicalJobEventPayload(payload)
		if err != nil {
			return false, fmt.Errorf("%w: canonicalize legacy job event payload: %v", ErrInvalidJobEvidence, err)
		}
	}
	existingCanonicalPayload, err := canonicalJobEventPayload(existingPayload)
	if err != nil {
		return false, fmt.Errorf("store: canonicalize saved job event payload: %w", err)
	}
	if existingPhase != phase || existingOccurredAt != fmtTime(occurredAt) ||
		existingCanonicalPayload != incomingPayload {
		return false, fmt.Errorf("%w: job_id=%s seq=%d", ErrJobEventConflict, jobID, seq)
	}
	return true, nil
}

// canonicalJobEventPayload 將 JSON 物件鍵順序與無意義空白正規化，同時用
// json.Number 保留數值原文，避免大整數經 float64 後被誤判成相同。
func canonicalJobEventPayload(payload string) (string, error) {
	if !json.Valid([]byte(payload)) {
		return "", errors.New("invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(payload)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

// RecordVerification 保存 executor agent 實際執行的驗證命令與輸出證據。
// producer/role 是 provenance，不是獨立 verifier；received_at 才是 Hub clock。
// ⚠ agent 是 at-least-once；完全相同的重送不得在帳本上多出一列執行敘事。
func (s *Store) RecordVerification(jobID, machineID, leaseToken, ruleID, command string, exitCode int, stdoutExcerpt, stderrExcerpt string, passed bool, verifiedAt time.Time) error {
	receivedAt := s.now().UTC().Truncate(time.Second)
	if !validJobReadStoredIdentifier(ruleID, 256) ||
		len(command) > maxJobVerificationCommandBytes ||
		len(stdoutExcerpt) > maxJobVerificationExcerptBytes ||
		len(stderrExcerpt) > maxJobVerificationExcerptBytes ||
		!validJobEvidenceTime(verifiedAt) || !validJobEvidenceTime(receivedAt) {
		return ErrInvalidJobEvidence
	}
	// ⚠ command 不做整理或改寫，讓人可以從證據列原樣複製後重跑。
	res, err := s.db.Exec(`
INSERT INTO verification_results
  (verification_id, job_id, machine_id, rule_id, command, exit_code,
   stdout_excerpt, stderr_excerpt, passed, verified_at, producer_kind,
   producer_id, evidence_role, authority, provenance_recorded, received_at)
SELECT ?, job_id, machine_id, ?, ?, ?, ?, ?, ?, ?, ?, machine_id, ?, ?, 1, ?
 FROM jobs
 WHERE job_id = ? AND machine_id = ? AND lease_token = ?
   AND state IN (?, ?, ?)
   AND NOT EXISTS (
       SELECT 1 FROM verification_results AS existing
        WHERE existing.job_id = jobs.job_id
          AND existing.rule_id = ?
          AND existing.evidence_role = ?
          AND existing.producer_id = jobs.machine_id
   )`, newID(), ruleID, command,
		exitCode, stdoutExcerpt, stderrExcerpt, passed, fmtTime(verifiedAt),
		JobVerificationProducerExecutorAgent, JobVerificationRoleExecutor,
		JobVerificationAuthorityMachineLease, fmtTime(receivedAt),
		jobID, machineID, leaseToken, deploy.Claimed, deploy.Running, deploy.Verifying,
		ruleID, JobVerificationRoleExecutor)
	if err != nil {
		return fmt.Errorf("store: record verification: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: count verification results: %w", err)
	}
	if n == 1 {
		return nil
	}
	var existingCommand, existingStdout, existingStderr, existingVerifiedAt string
	var existingExitCode int
	var existingPassed bool
	err = s.db.QueryRow(`
SELECT command, exit_code, stdout_excerpt, stderr_excerpt, passed, verified_at
  FROM verification_results
 WHERE job_id = ? AND machine_id = ? AND rule_id = ?
   AND evidence_role = ? AND producer_id = ?`, jobID, machineID, ruleID,
		JobVerificationRoleExecutor, machineID).Scan(&existingCommand, &existingExitCode,
		&existingStdout, &existingStderr, &existingPassed, &existingVerifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseInvalid
	}
	if err != nil {
		return fmt.Errorf("store: validate replayed job verification: %w", err)
	}
	if existingCommand != command || existingExitCode != exitCode ||
		existingStdout != stdoutExcerpt || existingStderr != stderrExcerpt ||
		existingPassed != passed || existingVerifiedAt != fmtTime(verifiedAt) {
		return fmt.Errorf("%w: job_id=%s rule_id=%s", ErrJobVerificationConflict, jobID, ruleID)
	}
	return nil
}

// MarkSucceededIfVerified 只依 Hub 已保存的驗證證據判定工作單成功。
// ⚠ 回傳的是判決後工作單真正停在的狀態；呼叫端不得假設 nil 就等於 succeeded。
func (s *Store) MarkSucceededIfVerified(jobID string, now time.Time) (deploy.JobState, error) {
	required, ready, err := s.darwinNodeRuntimeEvidenceReady(jobID)
	if err != nil {
		return "", err
	}
	// ⚠⚠ EXISTS 與 NOT EXISTS 跟狀態更新在同一個 SQL 敘述中，擋的是
	// 沒有證據便成功，以及檢查後才插入失敗證據的競態。
	//
	// ⚠⚠ 兩個子查詢都限定 evidence_role='executor'。independent verifier 的
	// 證據**不參與這個判決**：它既不能讓一張沒有 executor 證據的單成功，
	// 也不能用一筆 passed=0 讓一張單失敗。第二個方向才是真正的風險 ——
	// 否則一個第三方 producer 就能單方面擋住整個機隊的部署。
	// 要改這條界線必須是另一個明確決定，見 docs/VERIFIER-TOPOLOGY.md §6。
	//
	// ⚠⚠ `state = verifying` 不是多餘的守衛。internal/deploy 那組窮舉測試
	// 釘住了「succeeded 只有一個入口：verifying + VerificationPassed」，
	// 而這裡是**唯一**能寫進 succeeded 的 SQL —— 如果它允許從 claimed 直接
	// 跳過去，那組測試就只是在測一個沒有人用的函式，
	// 而一台根本沒開始裝的機器會被判成裝好了。
	res, err := s.db.Exec(`
UPDATE jobs
   SET state = ?, terminal_at = ?, lease_token = NULL, lease_expires_at = NULL
 WHERE job_id = ?
   AND state = ?
   AND EXISTS (
       SELECT 1 FROM verification_results
        WHERE verification_results.job_id = jobs.job_id
          AND verification_results.machine_id = jobs.machine_id
          AND verification_results.producer_kind = ?
          AND verification_results.evidence_role = ?
          AND verification_results.authority = ?
          AND verification_results.producer_id = jobs.machine_id
   )
   AND NOT EXISTS (
       SELECT 1 FROM verification_results
        WHERE verification_results.job_id = jobs.job_id AND passed = 0
          AND verification_results.evidence_role = ?
   )
   AND ?`, deploy.Succeeded, fmtTime(now), jobID, deploy.Verifying,
		JobVerificationProducerExecutorAgent, JobVerificationRoleExecutor,
		JobVerificationAuthorityMachineLease, JobVerificationRoleExecutor,
		!required || ready)
	if err != nil {
		return "", fmt.Errorf("store: mark job succeeded: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("store: count succeeded jobs: %w", err)
	}
	if n == 1 {
		return deploy.Succeeded, nil
	}

	var state deploy.JobState
	var total, failed, trustedExecutor int
	err = s.db.QueryRow(`
SELECT jobs.state,
       COALESCE(SUM(CASE WHEN verification_results.evidence_role = ?
         THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN verification_results.passed = 0
          AND verification_results.evidence_role = ? THEN 1 ELSE 0 END), 0),
       COALESCE(SUM(CASE
         WHEN verification_results.machine_id=jobs.machine_id
          AND verification_results.producer_kind=?
          AND verification_results.evidence_role=?
          AND verification_results.authority=?
          AND verification_results.producer_id=jobs.machine_id
         THEN 1 ELSE 0 END), 0)
  FROM jobs
  LEFT JOIN verification_results ON verification_results.job_id = jobs.job_id
 WHERE jobs.job_id = ?
 GROUP BY jobs.job_id, jobs.state`, JobVerificationRoleExecutor, JobVerificationRoleExecutor,
		JobVerificationProducerExecutorAgent, JobVerificationRoleExecutor,
		JobVerificationAuthorityMachineLease,
		jobID).Scan(&state, &total, &failed, &trustedExecutor)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrJobNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: inspect job verification: %w", err)
	}
	if deploy.IsTerminal(state) {
		return state, nil
	}
	if total == 0 || trustedExecutor == 0 {
		return "", ErrNoVerification
	}
	if failed > 0 {
		return "", ErrVerificationFailed
	}
	// Darwin 的完整證據只限制成功，不能蓋過已保存的 executor 失敗。
	if required && !ready {
		return "", ErrNoVerification
	}
	if state != deploy.Verifying {
		// ⚠ 有通過的證據、卻還不在 verifying —— 那代表 agent 還沒說「我做完了」。
		// 這不是錯誤資料，是還沒到時候。
		return "", ErrNotVerifying
	}
	return "", fmt.Errorf("store: 工作單 %s 未能寫入成功狀態", jobID)
}

func (s *Store) darwinNodeRuntimeEvidenceReady(jobID string) (required, ready bool, err error) {
	var state deploy.JobState
	var resourceKind, resourceID, rawSpec string
	err = s.db.QueryRow(`
SELECT jobs.state,desired_state.resource_kind,desired_state.resource_id,desired_state.spec
  FROM jobs JOIN desired_state ON desired_state.desired_id=jobs.desired_id
 WHERE jobs.job_id=?`, jobID).Scan(&state, &resourceKind, &resourceID, &rawSpec)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, ErrJobNotFound
	}
	if err != nil {
		return false, false, fmt.Errorf("store: inspect Darwin Node runtime job: %w", err)
	}
	if deploy.IsTerminal(state) || resourceKind != agentadapter.ExecutorKindNodeRuntime ||
		resourceID != agentadapter.ExecutorKindNodeRuntime {
		return false, false, nil
	}
	var spec model.NodeRuntimeSpec
	if json.Unmarshal([]byte(rawSpec), &spec) != nil || spec.Kind != agentadapter.ExecutorKindNodeRuntime ||
		spec.TargetOS != "darwin" {
		return false, false, nil
	}
	required = true
	if !validMeasuredNodeRuntimeVersion(spec.Version) || spec.Artifact == nil ||
		!validLowerSHA256(spec.Artifact.SHA256) {
		return true, false, nil
	}
	type evidencePair struct {
		releasePath string
		nodePath    string
		npmCommand  string
		artifact    bool
		node        bool
		npm         bool
	}
	pairs := map[string]*evidencePair{
		"node-runtime-activate": {},
		"node-runtime-current":  {},
	}
	rows, err := s.db.Query(`
SELECT verification_results.rule_id,verification_results.command,
       COALESCE(verification_results.stdout_excerpt,'')
  FROM verification_results JOIN jobs ON jobs.job_id=verification_results.job_id
 WHERE verification_results.job_id=?
   AND verification_results.exit_code=0
   AND verification_results.passed=1
   AND verification_results.producer_kind=?
   AND verification_results.producer_id=jobs.machine_id
   AND verification_results.evidence_role=?
   AND verification_results.authority=?
   AND verification_results.provenance_recorded=1
   AND verification_results.received_at<>''
   AND verification_results.observed_digest=''
   AND verification_results.observed_version=''
   AND verification_results.verifier_id=''
 ORDER BY verification_results.rowid`, jobID,
		JobVerificationProducerExecutorAgent, JobVerificationRoleExecutor,
		JobVerificationAuthorityMachineLease)
	if err != nil {
		return true, false, fmt.Errorf("store: read Darwin Node runtime evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ruleID, command, stdout string
		if err := rows.Scan(&ruleID, &command, &stdout); err != nil {
			return true, false, fmt.Errorf("store: scan Darwin Node runtime evidence: %w", err)
		}
		for prefix, pair := range pairs {
			switch ruleID {
			case prefix + "-artifact":
				suffix := "/.local/share/clawctl/node-runtime/releases/" + spec.Version +
					"/.clawctl-artifact-sha256"
				if strings.HasPrefix(command, "cat /") && strings.HasSuffix(command, suffix) &&
					!strings.ContainsAny(command, "\r\n") &&
					stdout == "sha256:"+spec.Artifact.SHA256+"\n" {
					pair.releasePath = strings.TrimSuffix(strings.TrimPrefix(command, "cat "),
						"/.clawctl-artifact-sha256")
					pair.artifact = true
				}
			case prefix + "-node":
				suffix := "/.local/share/clawctl/node-runtime/releases/" + spec.Version + "/bin/node --version"
				if strings.HasPrefix(command, "/") && strings.HasSuffix(command, suffix) &&
					!strings.ContainsAny(command, "\r\n") && strings.TrimSpace(stdout) == "v"+spec.Version {
					pair.nodePath = strings.TrimSuffix(command, " --version")
					pair.node = true
				}
			case prefix + "-npm":
				if !strings.ContainsAny(command, "\r\n") &&
					validMeasuredNodeRuntimeVersion(strings.TrimSpace(stdout)) {
					pair.npmCommand = command
					pair.npm = true
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return true, false, fmt.Errorf("store: iterate Darwin Node runtime evidence: %w", err)
	}
	for _, pair := range pairs {
		releasePath := strings.TrimSuffix(pair.nodePath, "/bin/node")
		if pair.artifact && pair.node && pair.npm && pair.releasePath == releasePath &&
			pair.npmCommand == pair.nodePath+" "+releasePath+
				"/lib/node_modules/npm/bin/npm-cli.js --version" {
			return true, true, nil
		}
	}
	return true, false, nil
}

func validMeasuredNodeRuntimeVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

// agentEvents 是 agent 被允許送出的事件。
//
// ⚠⚠ VerificationPassed **不在**這裡，而且永遠不准加進來。
// 它在狀態機上是 succeeded 的唯一入口 —— 讓 agent 送得出這個事件，
// 等於讓被測對象自己宣布通過，證據那張表就沒有意義了。
// Timeout 與 LeaseLost 也不在：那兩個是 Hub 觀察到的，不是機器自己說的。
var agentEvents = map[deploy.Event]bool{
	deploy.Start:      true,
	deploy.FinishWork: true,
	deploy.Reject:     true,
}

// hubReaperEvents 是無租約 Hub 入口的完整白名單。
// ⚠⚠ VerificationPassed 不能因為 caller 住在 Hub process 就被信任；
// 它必須走 MarkSucceededIfVerified，否則公開 Store API 會讓未來的 caller
// 繞過 verification_results，製造給 promote gate 用的假 succeeded witness。
var hubReaperEvents = map[deploy.Event]bool{
	deploy.Timeout:   true,
	deploy.LeaseLost: true,
}

// AdvanceJobByAgent 讓 agent 送來的事件推進工作單，必須持有目前的租約。
func (s *Store) AdvanceJobByAgent(jobID, machineID, leaseToken string, ev deploy.Event, now time.Time) (deploy.JobState, error) {
	if !agentEvents[ev] {
		return "", ErrEventNotForAgent
	}
	return s.advanceJob(jobID, machineID, leaseToken, true, ev, now)
}

// AdvanceJobByHub 讓 Hub 自己觀察到的事件推進工作單（逾時、租約消失）。
// expectedState 是 reaper 查到材料時的舊狀態；省略只供不會跨查詢競態的呼叫使用。
func (s *Store) AdvanceJobByHub(jobID string, ev deploy.Event, now time.Time, expectedState ...deploy.JobState) (deploy.JobState, error) {
	if !hubReaperEvents[ev] {
		return "", ErrEventNotForHub
	}
	return s.advanceJob(jobID, "", "", false, ev, now, expectedState...)
}

// advanceJob 是**唯一**的狀態推進入口，而且一律經過 deploy.Advance。
//
// ⚠⚠ 這裡不准自己寫 `SET state = 'running'` 之類的東西。
// internal/deploy 那個狀態機如果只被測試呼叫、沒有被真正的寫入路徑呼叫，
// 它就不是規則，只是一份文件 —— 而文件擋不住任何事情。
//
// ⚠ 失敗的終態一律由 deploy.Advance 配合這張單的 irreversible 決定，
// 所以「不可逆的失敗不准被說成已回退」在這條路上也成立。
func (s *Store) advanceJob(jobID, machineID, leaseToken string, requireLease bool, ev deploy.Event, now time.Time, expectedState ...deploy.JobState) (deploy.JobState, error) {
	now = now.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("store: begin advancing job: %w", err)
	}
	defer tx.Rollback()

	q := `SELECT state, irreversible FROM jobs WHERE job_id = ?`
	args := []any{jobID}
	if requireLease {
		// ⚠ 歸屬與 token 進 WHERE，不是查出來再比對（不變量 1）。
		q += ` AND machine_id = ? AND lease_token = ? AND lease_expires_at >= ?`
		args = append(args, machineID, leaseToken, fmtTime(now))
	} else if len(expectedState) > 0 {
		// ⚠ reaper 的 SELECT 與判決之間 agent 可能剛好前進；把舊狀態放進
		// SELECT/UPDATE 的同一條交易，擋的是 Hub 用過期快照蓋掉 agent 的新狀態。
		q += ` AND state = ?`
		args = append(args, expectedState[0])
	}
	var from deploy.JobState
	var irreversible bool
	switch err := tx.QueryRow(q, args...).Scan(&from, &irreversible); {
	case errors.Is(err, sql.ErrNoRows) && requireLease:
		// ⚠ 租約不符、不是你的單、單不存在 —— 對外都是同一個答案。
		return "", ErrLeaseInvalid
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrJobNotFound
	case err != nil:
		return "", fmt.Errorf("store: read job state: %w", err)
	}

	to, err := deploy.Advance(deploy.Job{State: from, Irreversible: irreversible}, ev)
	if err != nil {
		return from, err
	}

	var terminalAt any
	if deploy.IsTerminal(to) {
		terminalAt = fmtTime(now)
	}
	// ⚠ WHERE 帶上讀到的舊狀態：中間有人動過就不寫，不會蓋掉別人的轉移。
	query := `UPDATE jobs SET state = ?, terminal_at = COALESCE(?, terminal_at) WHERE job_id = ? AND state = ?`
	if deploy.IsTerminal(to) {
		// ⚠ 終態跟清租約在同一個 UPDATE，擋的是判決已經落地，舊 token
		// 卻仍留在帳本上，看起來還有一個有效持有者。
		query = `UPDATE jobs
   SET state = ?, terminal_at = COALESCE(?, terminal_at),
       lease_token = NULL, lease_expires_at = NULL
 WHERE job_id = ? AND state = ?`
	}
	res, err := tx.Exec(query, to, terminalAt, jobID, from)
	if err != nil {
		return "", fmt.Errorf("store: advance job: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return "", fmt.Errorf("store: count advanced jobs: %w", err)
	} else if n != 1 {
		return from, ErrJobNotFound
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit advanced job: %w", err)
	}
	return to, nil
}

// FailJob 依變更是否可回退，讓判決層選擇正確的失敗終態。
func (s *Store) FailJob(jobID string, irreversible bool, now time.Time) error {
	terminalState := deploy.OnFailure(irreversible)
	// ⚠ 終態一律取自 deploy.OnFailure，而且 caller 帶來的旗標必須和開單時
	// 保存的 irreversible 相同，擋的是 stale/錯誤 caller 把兩個失敗判決互換。
	res, err := s.db.Exec(`
UPDATE jobs
   SET state = ?, terminal_at = ?, lease_token = NULL, lease_expires_at = NULL
 WHERE job_id = ?
   AND irreversible = ?
   AND state NOT IN (?, ?, ?, ?, ?)`, terminalState, fmtTime(now), jobID,
		irreversible,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention)
	if err != nil {
		return fmt.Errorf("store: fail job: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: count failed jobs: %w", err)
	}
	if n == 1 {
		return nil
	}
	var savedIrreversible bool
	if err := s.db.QueryRow(`SELECT irreversible FROM jobs WHERE job_id = ?`, jobID).Scan(&savedIrreversible); errors.Is(err, sql.ErrNoRows) {
		return ErrJobNotFound
	} else if err != nil {
		return fmt.Errorf("store: inspect failed job: %w", err)
	}
	if savedIrreversible != irreversible {
		return ErrJobIrreversibility
	}
	return nil
}

func validJobEvidenceTime(value time.Time) bool {
	return !value.IsZero() && value.Year() >= 0 && value.Year() <= 9999
}

type jobRowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row jobRowScanner) (Job, error) {
	var (
		job                                   Job
		leaseToken, leaseExpiresAt            sql.NullString
		artifactDigest, createdAt, terminalAt sql.NullString
	)
	if err := row.Scan(&job.JobID, &job.MachineID, &job.DesiredID, &job.Revision, &job.State,
		&leaseToken, &leaseExpiresAt, &job.ExecutionTimeout, &artifactDigest,
		&job.Irreversible, &createdAt, &terminalAt); err != nil {
		return Job{}, err
	}
	job.LeaseToken = leaseToken.String
	job.LeaseExpiresAt = parseTimePtr(leaseExpiresAt)
	job.ArtifactDigest = artifactDigest.String
	job.CreatedAt = parseTimeNull(createdAt)
	job.TerminalAt = parseTimePtr(terminalAt)
	return job, nil
}

// nullIfEmpty 讓空字串進資料庫時是 NULL，不是 ""。
//
// ⚠ artifact_digest 分得出「沒有指定」跟「指定成空的」很重要：
// 一個 "" 的 digest 如果被當成有效值拿去比對，任何產物都會對不上；
// 如果被當成「不用驗」，那不變量 2 就整條沒了。NULL 讓下一層必須明確處理。
func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
