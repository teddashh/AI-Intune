package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/state"
)

const machineChannelRevisionTrigger = "tr_machine_registry_channel_revision"
const machineAssignedUserRevisionTrigger = "tr_machine_registry_assigned_user_revision"

const (
	machineLifecycleRevisionGuardTrigger       = "tr_machine_registry_lifecycle_revision_guard"
	machineLifecycleRevisionTrigger            = "tr_machine_registry_lifecycle_revision"
	machineLifecycleRevisionOnlyTrigger        = "tr_machine_registry_lifecycle_revision_only_guard"
	MaxMachineLifecycleRevision          int64 = int64(^uint64(0) >> 1)
)

// ensureMachineChannelRevisionTrigger fences old/rollback writers that know
// about channel but not channel_revision. New writers increment explicitly;
// the WHEN clause therefore leaves their revision alone.
func ensureMachineChannelRevisionTrigger(db *sql.DB) error {
	_, err := db.Exec(`CREATE TRIGGER IF NOT EXISTS ` + machineChannelRevisionTrigger + `
AFTER UPDATE OF channel ON machine_registry
FOR EACH ROW
WHEN NEW.channel IS NOT OLD.channel
 AND NEW.channel_revision = OLD.channel_revision
BEGIN
  UPDATE machine_registry
     SET channel_revision = OLD.channel_revision + 1
   WHERE machine_id = NEW.machine_id;
END`)
	if err != nil {
		return fmt.Errorf("create machine channel revision trigger: %w", err)
	}
	return nil
}

// ensureMachineLifecycleRevisionTriggers fences legacy writers which only
// know retired_at. They must not change the projection while silently omitting
// append-only lifecycle evidence. Canonical writers change retired_at and its
// revision by exactly one, then append the event in the same transaction.
func ensureMachineLifecycleRevisionTriggers(db *sql.DB) error {
	// A development build briefly used this name for an AFTER auto-increment
	// trigger. Drop it before installing the fail-closed invariant below.
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS ` + machineLifecycleRevisionTrigger); err != nil {
		return fmt.Errorf("drop obsolete machine lifecycle revision trigger: %w", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER IF NOT EXISTS ` + machineLifecycleRevisionGuardTrigger + `
BEFORE UPDATE OF retired_at ON machine_registry
FOR EACH ROW
WHEN NEW.retired_at IS NOT OLD.retired_at
 AND (OLD.lifecycle_revision < 0 OR OLD.lifecycle_revision >= 9223372036854775806
      OR NEW.lifecycle_revision != OLD.lifecycle_revision + 1)
BEGIN
  SELECT RAISE(ABORT, 'machine lifecycle update requires canonical revision and event writer');
END`); err != nil {
		return fmt.Errorf("create machine lifecycle revision guard trigger: %w", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER IF NOT EXISTS ` + machineLifecycleRevisionOnlyTrigger + `
BEFORE UPDATE OF lifecycle_revision ON machine_registry
FOR EACH ROW
WHEN NEW.retired_at IS OLD.retired_at
 AND NEW.lifecycle_revision != OLD.lifecycle_revision
BEGIN
  SELECT RAISE(ABORT, 'machine lifecycle revision cannot change without lifecycle state');
END`); err != nil {
		return fmt.Errorf("create machine lifecycle revision-only guard trigger: %w", err)
	}
	return nil
}

// ensureMachineAssignedUserRevisionTrigger fences writers that change the
// assigned user without its revision. New writers increment explicitly; the
// WHEN clause therefore leaves their revision alone.
func ensureMachineAssignedUserRevisionTrigger(db *sql.DB) error {
	_, err := db.Exec(`CREATE TRIGGER IF NOT EXISTS ` + machineAssignedUserRevisionTrigger + `
AFTER UPDATE OF assigned_user_id, assigned_user_login ON machine_registry
FOR EACH ROW
WHEN (NEW.assigned_user_id IS NOT OLD.assigned_user_id
   OR NEW.assigned_user_login IS NOT OLD.assigned_user_login)
 AND NEW.assigned_user_revision = OLD.assigned_user_revision
BEGIN
  UPDATE machine_registry
     SET assigned_user_revision = OLD.assigned_user_revision + 1
   WHERE machine_id = NEW.machine_id;
END`)
	if err != nil {
		return fmt.Errorf("create machine assigned user revision trigger: %w", err)
	}
	return nil
}

const (
	DeploymentRunning             = "running"
	DeploymentPaused              = "paused"
	DeploymentFinished            = "finished"
	maxDeploymentRetryLineage     = 256
	MaxDeploymentControlRevision  = int64(^uint64(0) >> 1)
	MaxDeploymentBatchSize        = 5
	activeDeploymentResourceIndex = "ux_deployments_one_active_resource"

	// StuckThreshold 逐字來自 docs/SPEC.md §2：非終態且 15 分鐘沒有事件。
	StuckThreshold = 15 * time.Minute
)

var (
	ErrDeploymentNotFound             = errors.New("store: deployment not found")
	ErrDeploymentNotPaused            = errors.New("store: deployment is not paused")
	ErrDeploymentRetryParentNotFound  = errors.New("store: deployment retry parent not found")
	ErrDeploymentRetryCycle           = errors.New("store: deployment retry lineage has a cycle")
	ErrDeploymentRetryTooDeep         = errors.New("store: deployment retry lineage is too deep")
	ErrDeploymentRetryMismatch        = errors.New("store: deployment retry lineage does not match")
	ErrDeploymentRetryParentRunning   = errors.New("store: deployment retry parent is running")
	ErrDeploymentRetryParentActive    = errors.New("store: deployment retry parent has active jobs")
	ErrDeploymentTargetChannelChanged = errors.New("store: deployment target channel changed; re-preview")
	ErrDeploymentMaterialMismatch     = errors.New("store: deployment spec kind does not match resource")
	ErrDeploymentInvalidBatchPlan     = errors.New("store: invalid deployment batch plan")
	ErrDeploymentJobGraphMismatch     = fmt.Errorf("%w: deployment target job graph is invalid", ErrDeploymentInvalidBatchPlan)
	ErrDeploymentBatchNotReady        = errors.New("store: deployment batch is not ready to open")
	ErrDeploymentConflict             = errors.New("store: deployment target has an active resource job")
	ErrDeploymentActiveResource       = errors.New("store: resource already has an active deployment")
	ErrDeploymentStaleRevision        = errors.New("store: deployment revision is stale for target")
	ErrDeploymentBadTransition        = errors.New("store: deployment state transition is not allowed")
	ErrDeploymentControlRevisionLimit = errors.New("store: deployment control revision cannot be incremented")
	ErrDeploymentFinishNotReady       = errors.New("store: deployment is not ready to finish")
	ErrDeploymentContinueRefused      = errors.New("store: plain Continue refuses a failed batch")
	ErrDeploymentAbandonActiveJobs    = errors.New("store: deployment has nonterminal jobs; cannot abandon")
	ErrMachineActiveJob               = errors.New("store: machine has a nonterminal job; cannot change channel")
	ErrStablePromoteGateRequired      = errors.New("store: stable deployment requires promote gate")
	ErrPromoteLocked                  = errors.New("store: promote locked")
)

const (
	DeploymentPausePromoteLocked  = "promote_locked"
	DeploymentPauseConflict       = "target_conflict"
	DeploymentPauseStaleRevision  = "stale_revision"
	DeploymentPauseBatchNotReady  = "batch_not_ready"
	DeploymentPauseMaterial       = "material_mismatch"
	DeploymentPauseInvalidPlan    = "invalid_batch_plan"
	DeploymentPauseTargetChanged  = "target_changed"
	DeploymentPauseMachineRetired = "machine_retired"
)

// DeploymentBoundaryPauseKind 只分類「重試同一個 batch 不會自己變好」的拒絕。
// SQLite busy／I/O／query 等暫時錯誤刻意不在這裡；driver 對它們只 log 並重試。
func DeploymentBoundaryPauseKind(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrPromoteLocked):
		return DeploymentPausePromoteLocked, true
	case errors.Is(err, ErrDeploymentConflict), errors.Is(err, ErrDeploymentActiveResource):
		return DeploymentPauseConflict, true
	case errors.Is(err, ErrDeploymentStaleRevision):
		return DeploymentPauseStaleRevision, true
	case errors.Is(err, ErrDeploymentBatchNotReady):
		return DeploymentPauseBatchNotReady, true
	case errors.Is(err, ErrDeploymentMaterialMismatch):
		return DeploymentPauseMaterial, true
	case errors.Is(err, ErrDeploymentInvalidBatchPlan), errors.Is(err, ErrDeploymentJobGraphMismatch):
		return DeploymentPauseInvalidPlan, true
	case errors.Is(err, ErrDeploymentTargetChannelChanged):
		return DeploymentPauseTargetChanged, true
	case errors.Is(err, ErrMachineRetired):
		return DeploymentPauseMachineRetired, true
	default:
		return "", false
	}
}

// SetMachineChannel 只讓真的送過 observation 的 agent 進部署通道。
func (s *Store) SetMachineChannel(machineID, channel string) error {
	if channel != "" && channel != "canary" && channel != "stable" {
		return ErrBadChannel
	}
	tx, err := s.beginWrite(context.Background(), "set_machine_channel")
	if err != nil {
		return fmt.Errorf("store: begin setting channel: %w", err)
	}
	defer tx.Rollback()
	var observed bool
	err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM observed_state WHERE machine_id = ?)`, machineID).Scan(&observed)
	if err != nil {
		return fmt.Errorf("store: inspect machine observations: %w", err)
	}
	if !observed {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM machine_registry WHERE machine_id = ?)`, machineID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return ErrNeverObserved
	}
	var current sql.NullString
	var revision int64
	if err := tx.QueryRow(`SELECT channel, channel_revision FROM machine_registry WHERE machine_id = ?`, machineID).Scan(&current, &revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("store: inspect machine channel: %w", err)
	}
	currentChannel := ""
	if current.Valid {
		currentChannel = current.String
	}
	// An idempotent form/CLI replay changes no authority and must remain safe
	// even while this machine owns a live job.
	if currentChannel == channel {
		return tx.Commit()
	}
	// The agent pulls by machine_id, not by its current channel. Therefore a
	// committed canary job followed by canary→stable would otherwise execute on
	// a stable member without ever passing the stable promote gate. Check under
	// the same BEGIN IMMEDIATE writer reservation as deployment job insertion:
	// create wins => this move fails; move wins => create's snapshot recheck fails.
	var active bool
	if err := tx.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM jobs WHERE machine_id=? AND state NOT IN (?,?,?,?,?))`, machineID,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention).Scan(&active); err != nil {
		return fmt.Errorf("store: inspect machine active jobs: %w", err)
	}
	if active {
		return fmt.Errorf("%w: %s has active jobs; wait for jobs to complete before changing channel from %q to %q",
			ErrMachineActiveJob, machineID, currentChannel, channel)
	}
	res, err := tx.Exec(`UPDATE machine_registry
	 SET channel = ?, channel_revision = channel_revision + 1
	 WHERE machine_id = ? AND channel_revision = ?`, nullIfEmpty(channel), machineID, revision)
	if err != nil {
		return fmt.Errorf("store: set machine channel: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *Store) MachinesInChannel(channel string) ([]Machine, error) {
	if channel != "canary" && channel != "stable" {
		return nil, ErrBadChannel
	}
	rows, err := s.rdb.Query(`SELECT `+machineCols+` FROM machine_registry
 WHERE channel = ? AND retired_at IS NULL ORDER BY display_name, machine_id`, channel)
	if err != nil {
		return nil, fmt.Errorf("store: machines in channel: %w", err)
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan channel machine: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PlanChannelDeployment 用通道當下的未退場成員建立預覽材料。
func (s *Store) PlanChannelDeployment(channel, engines string, batchSize int, now time.Time) (rollout.DeploymentPlan, []Machine, error) {
	members, err := s.MachinesInChannel(channel)
	if err != nil {
		return rollout.DeploymentPlan{}, nil, err
	}
	plan, err := s.PlanMachinesDeployment(members, engines, batchSize, now)
	return plan, members, err
}

// PlanMachinesDeployment 是 retry 對原 deployment 的 stuck 快照重新預覽時共用的入口。
func (s *Store) PlanMachinesDeployment(members []Machine, engines string, batchSize int, now time.Time) (rollout.DeploymentPlan, error) {
	facts, err := s.DeploymentFacts(members, now)
	if err != nil {
		return rollout.DeploymentPlan{}, err
	}
	return rollout.Plan(facts, engines, batchSize), nil
}

// DeploymentFacts 收一批成員的預覽材料（總覽判決、node 版本、衝突）。
// Updates 頁對同一個 channel 要對每個已收版本各印一句預覽：材料只收一次（Overview 是整個機隊的判決，
// 不該每個版本重算一遍），engines 不同只是 rollout.Plan 再跑一次。
func (s *Store) DeploymentFacts(members []Machine, now time.Time) ([]rollout.MachineFacts, error) {
	ov, err := s.Overview(now)
	if err != nil {
		return nil, err
	}
	judged := make(map[string]state.State, len(ov.Machines))
	for _, row := range ov.Machines {
		judged[row.MachineID] = row.State
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.MachineID)
	}
	installs, err := s.LatestOpenClawInstallObservationsAt(ids, now)
	if err != nil {
		return nil, err
	}
	conflicts, err := s.ActiveJobMachines("openclaw", "openclaw")
	if err != nil {
		return nil, err
	}
	// 領不到工作單的機器不該被排進批次：開一張沒有人領得走的單，之後只會以
	// 「lease 過期」收場，而那句話講的不是真正的原因。
	blocked, err := s.ComplianceBlockedMachines()
	if err != nil {
		return nil, err
	}
	facts := make([]rollout.MachineFacts, 0, len(members))
	for _, m := range members {
		node := ""
		if observation, ok := installs[m.MachineID]; ok && observation.Install != nil {
			node = observation.Install.NodeVersion
		}
		// ⚠ 這裡拿的是總覽同一條 state.Derive 的判決，不是另外算「多久沒回報」：
		// 兩條算法遲早會對同一台講出不同的話。IdentityConflict 仍算 reachable ——
		// 單是以 bearer 對應的 machine_id 開的，名字撞了不影響它領得到哪一張。
		judgement := judged[m.MachineID]
		facts = append(facts, rollout.MachineFacts{
			MachineID: m.MachineID, DisplayName: m.DisplayName, NodeVersion: node,
			Reachable:    judgement != state.Unreachable && judgement != state.NeverReported,
			Conflict:     conflicts[m.MachineID],
			Noncompliant: blocked[m.MachineID],
		})
	}
	return facts, nil
}

// ActiveJobMachines 回報指定資源目前有非終態單的機器。
func (s *Store) ActiveJobMachines(resourceKind, resourceID string) (map[string]bool, error) {
	rows, err := s.rdb.Query(`
SELECT DISTINCT j.machine_id
  FROM jobs j JOIN desired_state d ON d.desired_id = j.desired_id
 WHERE d.resource_kind = ? AND d.resource_id = ?
   AND j.state NOT IN (?, ?, ?, ?, ?)`, resourceKind, resourceID,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention)
	if err != nil {
		return nil, fmt.Errorf("store: active resource jobs: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

type Deployment struct {
	DeploymentID string
	Channel      string
	DesiredID    string
	ResourceKind string
	ResourceID   string
	Revision     deploy.Revision
	// ControlRevision changes for lifecycle transitions and newly-opened
	// batches. It is independent from the desired-state Revision agents see.
	ControlRevision int64
	BatchSize       int
	// PauseAfterCanary is set on operator creates. Existing rows and direct
	// store creates stay false and keep the auto-open driver.
	PauseAfterCanary bool
	State            string
	CreatedAt        time.Time
	CreatedBy        string
	PausedAt         *time.Time
	FinishedAt       *time.Time
	RetryOf          string
	Spec             string
}

type NewDeploymentTarget struct {
	MachineID      string
	BatchNo        int
	ExcludedReason string
}

type NewDeployment struct {
	Channel          string
	ResourceKind     string
	ResourceID       string
	Spec             string
	BatchSize        int
	PauseAfterCanary bool
	CreatedBy        string
	RetryOf          string
	Targets          []NewDeploymentTarget
	Job              NewJob
}

type DeploymentTarget struct {
	MachineID      string
	DisplayName    string
	BatchNo        int
	JobID          string
	JobMachineID   string
	JobDesiredID   string
	JobRevision    deploy.Revision
	JobReferences  int
	ExcludedReason string
	JobState       deploy.JobState
	CreatedAt      time.Time
	TerminalAt     *time.Time
	LastActivity   time.Time
	StuckKind      string // terminal_failure | no_event | 空字串
}

type DeploymentView struct {
	Deployment
	Targets         []DeploymentTarget
	DesiredJobCount int
	Counts          map[deploy.JobState]int
	OpenedBatch     int
	TotalBatches    int
	Stuck           int
	TerminalStuck   int
	SilentStuck     int
	Attempt         int
	BoundaryPause   *DeploymentBoundaryPause
}

type DeploymentBoundaryPause struct {
	OpenedBatch int
	Kind        string
	Reason      string
	PausedAt    time.Time
}

func validateNewDeployment(n NewDeployment) error {
	if n.Channel != "canary" && n.Channel != "stable" {
		return ErrBadChannel
	}
	if n.BatchSize <= 0 || n.BatchSize > MaxDeploymentBatchSize {
		return fmt.Errorf("%w: batch_size must be between 1 and %d", ErrDeploymentInvalidBatchPlan, MaxDeploymentBatchSize)
	}
	if len(n.Targets) == 0 {
		return fmt.Errorf("%w: deployment needs targets", ErrDeploymentInvalidBatchPlan)
	}
	counts := make(map[int]int)
	maxBatch := 0
	for _, target := range n.Targets {
		switch target.ExcludedReason {
		case "", "conflict", "missing_package", "unknown_node", "noncompliant":
		default:
			return fmt.Errorf("store: bad deployment exclusion %q", target.ExcludedReason)
		}
		if target.ExcludedReason != "" {
			if target.BatchNo != 0 {
				return fmt.Errorf("%w: excluded target %s batch_no must be 0", ErrDeploymentInvalidBatchPlan, target.MachineID)
			}
			continue
		}
		if target.BatchNo <= 0 {
			return fmt.Errorf("%w: included target %s needs a positive batch", ErrDeploymentInvalidBatchPlan, target.MachineID)
		}
		counts[target.BatchNo]++
		if counts[target.BatchNo] > n.BatchSize {
			return fmt.Errorf("%w: batch %d has %d machines, exceeding batch_size %d",
				ErrDeploymentInvalidBatchPlan, target.BatchNo, counts[target.BatchNo], n.BatchSize)
		}
		if target.BatchNo > maxBatch {
			maxBatch = target.BatchNo
		}
	}
	if maxBatch == 0 {
		return fmt.Errorf("%w: deployment needs at least one included target", ErrDeploymentInvalidBatchPlan)
	}
	// 全是正整數時，distinct batch 數等於最大值，才可能恰好是 1..max；
	// 不逐號走，避免惡意 MaxInt batch_no 把 admission 卡死。
	if len(counts) != maxBatch {
		return fmt.Errorf("%w: positive batch numbers must be contiguous from 1", ErrDeploymentInvalidBatchPlan)
	}
	return nil
}

// CreateDeployment 在同一個 SQLite transaction 裡建立 desired state、快照與第一批 jobs。
// stable 不接受這條通用入口：它必須走 CreateStableOpenClawDeployment，
// 否則只要有一個新 caller 忘了先問 gate，「硬鎖」就只是 CLI 慣例。
func (s *Store) CreateDeployment(n NewDeployment) (Deployment, []Job, error) {
	if err := validateNewDeployment(n); err != nil {
		return Deployment{}, nil, err
	}
	if n.Channel == "stable" {
		return Deployment{}, nil, ErrStablePromoteGateRequired
	}
	tx, err := s.beginWrite(context.Background(), "create_deployment")
	if err != nil {
		return Deployment{}, nil, fmt.Errorf("store: begin deployment: %w", err)
	}
	defer tx.Rollback()
	now := s.now().UTC()
	d, jobs, err := createDeploymentTx(tx, n, now)
	if err != nil {
		return Deployment{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, nil, fmt.Errorf("store: commit deployment: %w", err)
	}
	return d, jobs, nil
}

// CreateStableOpenClawDeployment 先在 SQLite 拿到 writer reservation，然後才重讀
// promote 的全部證據並開單。工作天一律直接讀 Hub 主機的 /etc/localtime；不能用
// process time.Local，因為另一支 CLI 的 caller 可以用 TZ 改掉它、縮短等待窗口。
// PromoteFacts 目前是純讀、會透過 Store 的另一條
// pooled connection 讀取；WAL 允許這個 reader，而 BEGIN IMMEDIATE 已擋住所有
// 其他 writer，所以這整段看到的 committed facts 不會在 create 前被改掉。
func (s *Store) CreateStableOpenClawDeployment(n NewDeployment) (Deployment, []Job, rollout.PromoteDecision, error) {
	return s.createStableOpenClawDeploymentWithPreview(n, s.PreviewStableOpenClawPromotion)
}

// createStableOpenClawDeployment 保留 loc injection，僅供同 package 的 deterministic
// tests 驗證工作天邊界；production caller 必須走固定 Hub local timezone 的 exported API。
func (s *Store) createStableOpenClawDeployment(n NewDeployment, loc *time.Location) (Deployment, []Job, rollout.PromoteDecision, error) {
	return s.createStableOpenClawDeploymentWithPreview(n,
		func(version, digest string, now time.Time) (rollout.PromoteDecision, error) {
			return s.previewStableOpenClawPromotionAt(version, digest, now, loc)
		})
}

type stablePromotionPreviewer func(version, digest string, now time.Time) (rollout.PromoteDecision, error)

func (s *Store) createStableOpenClawDeploymentWithPreview(n NewDeployment, preview stablePromotionPreviewer) (Deployment, []Job, rollout.PromoteDecision, error) {
	version, digest, err := stableOpenClawMaterial(n)
	if err != nil {
		return Deployment{}, nil, rollout.PromoteDecision{}, err
	}
	tx, err := s.beginWrite(context.Background(), "create_stable_open_claw_deployment_with_preview")
	if err != nil {
		return Deployment{}, nil, rollout.PromoteDecision{}, fmt.Errorf("store: begin stable deployment: %w", err)
	}
	defer tx.Rollback()

	// 時間也是 gate 的證據；若 BEGIN 等過前一個 writer，不能還用等鎖前的舊 now。
	now := s.now().UTC()
	decision, err := preview(version, digest, now)
	if err != nil {
		return Deployment{}, nil, decision, err
	}
	if !decision.Allowed {
		return Deployment{}, nil, decision, fmt.Errorf("%w: %s", ErrPromoteLocked, decision.Summary())
	}
	d, jobs, err := createDeploymentTx(tx, n, now)
	if err != nil {
		return Deployment{}, nil, decision, err
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, nil, decision, fmt.Errorf("store: commit stable deployment: %w", err)
	}
	return d, jobs, decision, nil
}

func stableOpenClawMaterial(n NewDeployment) (string, string, error) {
	if err := validateNewDeployment(n); err != nil {
		return "", "", err
	}
	return stableOpenClawMaterialParts(n.Channel, n.ResourceKind, n.ResourceID, n.Spec, n.Job.ArtifactDigest)
}

func stableOpenClawMaterialParts(channel, resourceKind, resourceID, rawSpec, jobDigest string) (string, string, error) {
	if channel != "stable" || resourceKind != "openclaw" || resourceID != "openclaw" {
		return "", "", fmt.Errorf("%w: stable promote gate only supports openclaw:openclaw", ErrDeploymentMaterialMismatch)
	}
	var spec model.OpenClawSpec
	if err := json.Unmarshal([]byte(rawSpec), &spec); err != nil || spec.Kind != "openclaw" || strings.TrimSpace(spec.Version) == "" || spec.Artifact == nil {
		return "", "", fmt.Errorf("%w: stable deployment needs a complete OpenClaw spec", ErrDeploymentMaterialMismatch)
	}
	want := spec.Artifact.SHA256
	if !artifact.ValidSHA256Hex(want) {
		return "", "", fmt.Errorf("%w: stable deployment spec digest %q is not canonical lowercase SHA-256", ErrDeploymentMaterialMismatch, want)
	}
	if jobDigest != "sha256:"+want {
		return "", "", fmt.Errorf("%w: stable deployment spec digest %q does not match job digest %q", ErrDeploymentMaterialMismatch, want, jobDigest)
	}
	return spec.Version, want, nil
}

const hubLocaltimePath = "/etc/localtime"

type hubLocaltimeReader func(string) ([]byte, error)

func loadHubCalendarLocation(readFile hubLocaltimeReader) (*time.Location, error) {
	if readFile == nil {
		return nil, errors.New("store: Hub local timezone reader is nil")
	}
	data, err := readFile(hubLocaltimePath)
	if err != nil {
		return nil, fmt.Errorf("store: read Hub local timezone: %w", err)
	}
	loc, err := time.LoadLocationFromTZData("Hub local", data)
	if err != nil {
		return nil, fmt.Errorf("store: parse Hub local timezone: %w", err)
	}
	return loc, nil
}

func hubCalendarLocation() (*time.Location, error) {
	return loadHubCalendarLocation(os.ReadFile)
}

// PreviewStableOpenClawPromotion 是 CLI、Web 與所有 stable 寫入邊界共用的
// production 判決。它刻意不把「鎖著」當 error，讓唯讀 preview 能把完整原因顯示；
// 寫入 caller 再把 Allowed=false materialize 成 ErrPromoteLocked。
func (s *Store) PreviewStableOpenClawPromotion(version, digest string, now time.Time) (rollout.PromoteDecision, error) {
	return s.previewStableOpenClawPromotion(version, digest, now, hubCalendarLocation)
}

type hubCalendarLocationLoader func() (*time.Location, error)

func (s *Store) previewStableOpenClawPromotion(version, digest string, now time.Time,
	loadLocation hubCalendarLocationLoader,
) (rollout.PromoteDecision, error) {
	loc, err := loadLocation()
	if err != nil {
		return rollout.PromoteDecision{Reasons: []string{
			fmt.Sprintf("Hub 主機時區不可用，不能判斷完整工作天：%v", err),
		}}, nil
	}
	return s.previewStableOpenClawPromotionAt(version, digest, now, loc)
}

func (s *Store) previewStableOpenClawPromotionAt(version, digest string, now time.Time, loc *time.Location) (rollout.PromoteDecision, error) {
	facts, err := s.PromoteFacts(version, digest, now)
	if err != nil {
		return rollout.PromoteDecision{}, fmt.Errorf("store: read promote facts: %w", err)
	}
	return rollout.PromoteGate(facts, now, loc), nil
}

func createDeploymentTx(tx dbTx, n NewDeployment, now time.Time) (Deployment, []Job, error) {
	lineage, err := validateDeploymentRetryLineage(tx, n)
	if err != nil {
		return Deployment{}, nil, err
	}
	if err := validateDeploymentMaterial(n.ResourceKind, n.ResourceID, n.Spec, n.Job.ArtifactDigest); err != nil {
		return Deployment{}, nil, err
	}
	if err := validateDeploymentSnapshotTargets(tx, n); err != nil {
		return Deployment{}, nil, err
	}
	// Claim the resource before allocating a revision or writing any ledger row.
	// BEGIN IMMEDIATE serializes this check with every other Store writer; the
	// partial UNIQUE index is the backstop for old binaries and raw SQL.
	if err := prepareDeploymentResourceOwnerTx(tx, n, lineage, now); err != nil {
		return Deployment{}, nil, err
	}
	desiredID, rev, err := createDesiredStateTx(tx, "channel", n.Channel, n.ResourceKind, n.ResourceID, n.Spec, n.CreatedBy, now)
	if err != nil {
		return Deployment{}, nil, err
	}
	d := Deployment{
		DeploymentID: newID(), Channel: n.Channel, DesiredID: desiredID, ResourceKind: n.ResourceKind,
		ResourceID: n.ResourceID, Revision: rev, BatchSize: n.BatchSize, PauseAfterCanary: n.PauseAfterCanary,
		State: DeploymentRunning, CreatedAt: now, CreatedBy: n.CreatedBy, RetryOf: n.RetryOf, Spec: n.Spec,
	}
	pauseAfterCanary := 0
	if d.PauseAfterCanary {
		pauseAfterCanary = 1
	}
	if _, err := tx.Exec(`INSERT INTO deployments
 (deployment_id,channel,desired_id,resource_kind,resource_id,revision,control_revision,batch_size,pause_after_canary,state,created_at,created_by,retry_of)
 VALUES (?,?,?,?,?,?,0,?,?,?,?,?,?)`, d.DeploymentID, d.Channel, d.DesiredID, d.ResourceKind, d.ResourceID,
		d.Revision, d.BatchSize, pauseAfterCanary, d.State, fmtTime(now), d.CreatedBy, nullIfEmpty(d.RetryOf)); err != nil {
		return Deployment{}, nil, fmt.Errorf("store: create deployment: %w", err)
	}
	for _, target := range n.Targets {
		batch := target.BatchNo
		if target.ExcludedReason != "" {
			batch = 0
		} else {
			// ⚠ 預覽與 create 之間另一個 process 可能開單；交易裡再查一次，
			// 擋的是把預覽時不存在的衝突悄悄帶進正式計畫。
			var conflict bool
			if err := tx.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM jobs j JOIN desired_state ds ON ds.desired_id=j.desired_id
 WHERE j.machine_id=? AND ds.resource_kind=? AND ds.resource_id=?
 AND j.state NOT IN (?,?,?,?,?))`, target.MachineID, n.ResourceKind, n.ResourceID,
				deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention).Scan(&conflict); err != nil {
				return Deployment{}, nil, err
			}
			if conflict {
				return Deployment{}, nil, fmt.Errorf("%w: target %s gained an active %s job after preview",
					ErrDeploymentConflict, target.MachineID, n.ResourceKind)
			}
		}
		if _, err := tx.Exec(`INSERT INTO deployment_targets
 (deployment_id,machine_id,batch_no,excluded_reason) VALUES (?,?,?,?)`,
			d.DeploymentID, target.MachineID, batch, nullIfEmpty(target.ExcludedReason)); err != nil {
			return Deployment{}, nil, fmt.Errorf("store: create deployment target %s: %w", target.MachineID, err)
		}
	}
	jobs, err := createDeploymentBatchTx(tx, d, 1, n.Job, now)
	if err != nil {
		return Deployment{}, nil, err
	}
	if len(jobs) == 0 {
		return Deployment{}, nil, errors.New("store: deployment first batch is empty")
	}
	kind := HubDeploymentCreated
	detail := fmt.Sprintf("deployment %s created；channel=%s；第一批 %d 張單", d.DeploymentID, d.Channel, len(jobs))
	if d.RetryOf != "" {
		kind = HubDeploymentRetried
		detail = fmt.Sprintf("deployment %s retried from %s；第一批 %d 張新單", d.DeploymentID, d.RetryOf, len(jobs))
	}
	if _, err := tx.Exec(`INSERT INTO hub_events (at,kind,detail) VALUES (?,?,?)`,
		fmtTime(now), kind, detail); err != nil {
		return Deployment{}, nil, fmt.Errorf("store: record deployment create event: %w", err)
	}
	return d, jobs, nil
}

func createDeploymentBatchTx(tx dbTx, d Deployment, batchNo int, n NewJob, now time.Time) ([]Job, error) {
	// Continue 也可能讀到升級前或手工寫入的 ledger；不能只靠 initial create
	// 的檢查，否則錯標 material 的第二批仍能長出 OpenClaw job。
	if err := validateDeploymentMaterial(d.ResourceKind, d.ResourceID, d.Spec, n.ArtifactDigest); err != nil {
		return nil, err
	}
	if n.ExecutionTimeout <= 0 {
		n.ExecutionTimeout = 900
	}
	rows, err := tx.Query(`SELECT machine_id, job_id FROM deployment_targets
 WHERE deployment_id = ? AND batch_no = ? ORDER BY machine_id`, d.DeploymentID, batchNo)
	if err != nil {
		return nil, err
	}
	type row struct{ machineID, jobID string }
	var targets []row
	for rows.Next() {
		var machineID string
		var jobID sql.NullString
		if err := rows.Scan(&machineID, &jobID); err != nil {
			rows.Close()
			return nil, err
		}
		targets = append(targets, row{machineID, jobID.String})
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// Initial create 檢查的是整張 snapshot；延後批次還要在真正開 job 的
	// writer transaction 裡重查。active job 會造成同資源並行，而任何較高
	// revision（即使已終態）都表示 agent 之後必定以 STALE_REVISION 拒絕舊批次。
	for _, target := range targets {
		if target.jobID != "" {
			continue
		}
		// Channel membership is part of the deployment snapshot's authority. A
		// target can move while an earlier batch is running; without this check a
		// canary deployment could allocate a new job after that target had become
		// stable, bypassing the stable promote gate. Recheck under the same writer
		// reservation that will insert the job so channel drift cannot race this
		// decision.
		var currentChannel, retiredAt sql.NullString
		err := tx.QueryRow(`SELECT channel,retired_at FROM machine_registry WHERE machine_id=?`, target.machineID).
			Scan(&currentChannel, &retiredAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: target %s is not in registry, preview again", ErrDeploymentTargetChannelChanged, target.machineID)
		}
		if err != nil {
			return nil, fmt.Errorf("store: recheck delayed deployment target %s: %w", target.machineID, err)
		}
		if retiredAt.Valid || !currentChannel.Valid || currentChannel.String != d.Channel {
			return nil, fmt.Errorf("%w: target %s is no longer an active %s member, preview again",
				ErrDeploymentTargetChannelChanged, target.machineID, d.Channel)
		}
		var active bool
		if err := tx.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM jobs j JOIN desired_state ds ON ds.desired_id=j.desired_id
 WHERE j.machine_id=? AND ds.resource_kind=? AND ds.resource_id=?
 AND j.state NOT IN (?,?,?,?,?)
 AND NOT EXISTS (SELECT 1 FROM deployment_targets own
                 WHERE own.deployment_id=? AND own.job_id=j.job_id))`,
			target.machineID, d.ResourceKind, d.ResourceID,
			deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention,
			d.DeploymentID).Scan(&active); err != nil {
			return nil, fmt.Errorf("store: recheck delayed deployment conflict for %s: %w", target.machineID, err)
		}
		if active {
			return nil, fmt.Errorf("%w: target %s already has another active %s job, preview again or retry",
				ErrDeploymentConflict, target.machineID, d.ResourceKind)
		}
		var newest deploy.Revision
		if err := tx.QueryRow(`SELECT COALESCE(MAX(j.revision),0)
 FROM jobs j JOIN desired_state ds ON ds.desired_id=j.desired_id
 WHERE j.machine_id=? AND ds.resource_kind=? AND ds.resource_id=?`,
			target.machineID, d.ResourceKind, d.ResourceID).Scan(&newest); err != nil {
			return nil, fmt.Errorf("store: recheck delayed deployment revision for %s: %w", target.machineID, err)
		}
		if newest > d.Revision {
			return nil, fmt.Errorf("%w: target %s already has revision %d, higher than this deployment revision %d; retry to get a newer revision",
				ErrDeploymentStaleRevision, target.machineID, newest, d.Revision)
		}
	}
	var out []Job
	for _, target := range targets {
		if target.jobID != "" {
			j, err := jobTx(tx, target.jobID)
			if err != nil {
				return nil, err
			}
			out = append(out, j)
			continue
		}
		jobID := newID()
		res, err := tx.Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,artifact_digest,irreversible,execution_timeout)
 SELECT ?,machine_id,?,?,?,?,?,?,? FROM machine_registry
 WHERE machine_id = ? AND retired_at IS NULL`, jobID, d.DesiredID, d.Revision, deploy.NotStarted,
			fmtTime(now), nullIfEmpty(n.ArtifactDigest), n.Irreversible, n.ExecutionTimeout, target.machineID)
		if err != nil {
			return nil, fmt.Errorf("store: create deployment job: %w", err)
		}
		if affected, _ := res.RowsAffected(); affected != 1 {
			return nil, fmt.Errorf("%w: target %s cannot receive a deployment job", ErrMachineRetired, target.machineID)
		}
		if _, err := tx.Exec(`UPDATE deployment_targets SET job_id = ?
 WHERE deployment_id = ? AND machine_id = ? AND job_id IS NULL`, jobID, d.DeploymentID, target.machineID); err != nil {
			return nil, err
		}
		out = append(out, Job{JobID: jobID, MachineID: target.machineID, DesiredID: d.DesiredID,
			Revision: d.Revision, State: deploy.NotStarted, ArtifactDigest: n.ArtifactDigest,
			Irreversible: n.Irreversible, ExecutionTimeout: n.ExecutionTimeout, CreatedAt: now})
	}
	return out, nil
}

func validateDeploymentMaterial(resourceKind, resourceID, raw, jobDigest string) error {
	kind, ref, err := model.ParseJobSpec([]byte(raw))
	if err != nil || strings.TrimSpace(kind) == "" || kind == "noop" {
		return fmt.Errorf("%w: resource %s:%s has invalid deployment spec", ErrDeploymentMaterialMismatch, resourceKind, resourceID)
	}
	if kind != resourceKind || (kind == "openclaw" && resourceID != "openclaw") ||
		(resourceKind == "openclaw" && resourceID == "openclaw" && kind != "openclaw") {
		return fmt.Errorf("%w: spec kind %q cannot target %s:%s", ErrDeploymentMaterialMismatch, kind, resourceKind, resourceID)
	}
	if kind == "openclaw" {
		// Older non-production fixtures and ledgers can have neither an artifact
		// reference nor a job digest. There is no split authority in that shape,
		// and the agent already rejects the unpinned job. Once either side declares
		// deployable material, however, the spec and job must name exactly the same
		// bytes. This check runs both before initial create and again in
		// every transaction that can materialize a delayed batch.
		if ref == nil {
			if jobDigest == "" {
				return nil
			}
			return fmt.Errorf("%w: OpenClaw job digest %q has no matching spec artifact", ErrDeploymentMaterialMismatch, jobDigest)
		}
		if ref.SHA256 == "" {
			return fmt.Errorf("%w: OpenClaw spec artifact digest is empty", ErrDeploymentMaterialMismatch)
		}
		want := "sha256:" + ref.SHA256
		if jobDigest != want {
			return fmt.Errorf("%w: OpenClaw spec digest %q does not match job digest %q",
				ErrDeploymentMaterialMismatch, want, jobDigest)
		}
	}
	return nil
}

func validateDeploymentSnapshotTargets(tx dbTx, n NewDeployment) error {
	seen := make(map[string]struct{}, len(n.Targets))
	for _, target := range n.Targets {
		if _, duplicate := seen[target.MachineID]; duplicate {
			return fmt.Errorf("%w: target %s is duplicated, preview again", ErrDeploymentTargetChannelChanged, target.MachineID)
		}
		seen[target.MachineID] = struct{}{}
		var channel, retiredAt sql.NullString
		err := tx.QueryRow(`SELECT channel,retired_at FROM machine_registry WHERE machine_id=?`, target.MachineID).
			Scan(&channel, &retiredAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: target %s is not in registry, preview again", ErrDeploymentTargetChannelChanged, target.MachineID)
		}
		if err != nil {
			return fmt.Errorf("store: recheck deployment target %s: %w", target.MachineID, err)
		}
		if retiredAt.Valid || !channel.Valid || channel.String != n.Channel {
			return fmt.Errorf("%w: target %s is no longer an active %s member, preview again", ErrDeploymentTargetChannelChanged, target.MachineID, n.Channel)
		}
	}
	if n.RetryOf != "" {
		return nil
	}
	rows, err := tx.Query(`SELECT machine_id FROM machine_registry WHERE channel=? AND retired_at IS NULL`, n.Channel)
	if err != nil {
		return fmt.Errorf("store: recheck deployment channel members: %w", err)
	}
	defer rows.Close()
	members := make(map[string]struct{}, len(seen))
	for rows.Next() {
		var machineID string
		if err := rows.Scan(&machineID); err != nil {
			return fmt.Errorf("store: scan deployment channel member: %w", err)
		}
		members[machineID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: scan deployment channel members: %w", err)
	}
	if len(seen) != len(members) {
		return fmt.Errorf("%w: %s snapshot has %d machines, current channel has %d machines, preview again",
			ErrDeploymentTargetChannelChanged, n.Channel, len(seen), len(members))
	}
	for machineID := range members {
		if _, ok := seen[machineID]; !ok {
			return fmt.Errorf("%w: %s snapshot missing target %s, preview again",
				ErrDeploymentTargetChannelChanged, n.Channel, machineID)
		}
	}
	return nil
}

func validateDeploymentRetryLineage(tx dbTx, n NewDeployment) ([]Deployment, error) {
	if n.RetryOf == "" {
		return nil, nil
	}
	lineage, err := deploymentRetryLineageFromID(tx, n.RetryOf)
	if err != nil {
		return nil, err
	}
	// 把新 attempt 算進上限；不能讓一條剛好到頂的舊 lineage 再長一節。
	if len(lineage) >= maxDeploymentRetryLineage {
		return nil, fmt.Errorf("%w: retry parent %s", ErrDeploymentRetryTooDeep, n.RetryOf)
	}
	for _, ancestor := range lineage {
		if err := validateDeploymentJobGraph(tx, ancestor.DeploymentID); err != nil {
			return nil, err
		}
		if err := validateStoredDeploymentBatchPlan(tx, ancestor); err != nil {
			return nil, err
		}
		if ancestor.Channel != n.Channel || ancestor.ResourceKind != n.ResourceKind ||
			ancestor.ResourceID != n.ResourceID || ancestor.Spec != n.Spec {
			return nil, fmt.Errorf("%w: ancestor %s", ErrDeploymentRetryMismatch, ancestor.DeploymentID)
		}
		if err := validateDeploymentJobMaterialGraph(tx, ancestor.DeploymentID); err != nil {
			return nil, err
		}
		switch ancestor.State {
		case DeploymentPaused, DeploymentFinished:
			active, err := deploymentHasNonTerminalJobs(tx, ancestor.DeploymentID)
			if err != nil {
				return nil, err
			}
			if active {
				return nil, fmt.Errorf("%w: ancestor %s", ErrDeploymentRetryParentActive, ancestor.DeploymentID)
			}
		case DeploymentRunning:
			return nil, fmt.Errorf("%w: ancestor %s", ErrDeploymentRetryParentRunning, ancestor.DeploymentID)
		default:
			return nil, fmt.Errorf("%w: ancestor %s has state %q", ErrDeploymentRetryParentRunning, ancestor.DeploymentID, ancestor.State)
		}
	}
	return lineage, nil
}

func activeDeploymentResourceOwnersTx(tx dbTx, resourceKind, resourceID string) ([]string, error) {
	rows, err := tx.Query(`SELECT deployment_id FROM deployments
 WHERE resource_kind=? AND resource_id=? AND state IN (?,?)
 ORDER BY created_at,deployment_id`, resourceKind, resourceID, DeploymentRunning, DeploymentPaused)
	if err != nil {
		return nil, fmt.Errorf("store: inspect active deployment resource owner: %w", err)
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan active deployment resource owner: %w", err)
		}
		owners = append(owners, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: scan active deployment resource owner: %w", err)
	}
	return owners, nil
}

func deploymentActiveResourceConflict(resourceKind, resourceID string, owners []string) error {
	return fmt.Errorf("%w: %s:%s active deployments=[%s]; only one running/paused deployment per resource across channels is allowed at a time",
		ErrDeploymentActiveResource, resourceKind, resourceID, strings.Join(owners, ","))
}

// prepareDeploymentResourceOwnerTx reserves the resource for a new deployment.
// A paused retry parent is terminalized in this same transaction before the
// child is inserted, so Continue(parent) can never race into a disjoint batch.
// Retrying an already-finished parent remains safe and preserves the established
// CLI workflow: it owns no active slot and cannot be continued.
func prepareDeploymentResourceOwnerTx(tx dbTx, n NewDeployment, lineage []Deployment, now time.Time) error {
	owners, err := activeDeploymentResourceOwnersTx(tx, n.ResourceKind, n.ResourceID)
	if err != nil {
		return err
	}
	activeJobs, err := deploymentResourceHasNonTerminalJobsTx(tx, n.ResourceKind, n.ResourceID)
	if err != nil {
		return err
	}
	if activeJobs {
		return fmt.Errorf("%w: %s:%s has nonterminal jobs owned by an existing deployment",
			ErrDeploymentActiveResource, n.ResourceKind, n.ResourceID)
	}
	if n.RetryOf == "" {
		if len(owners) != 0 {
			return deploymentActiveResourceConflict(n.ResourceKind, n.ResourceID, owners)
		}
		return nil
	}
	if len(lineage) == 0 || lineage[0].DeploymentID != n.RetryOf {
		return fmt.Errorf("%w: retry parent %s", ErrDeploymentRetryParentNotFound, n.RetryOf)
	}
	parent := lineage[0]
	switch parent.State {
	case DeploymentPaused:
		if len(owners) != 1 || owners[0] != parent.DeploymentID {
			return deploymentActiveResourceConflict(n.ResourceKind, n.ResourceID, owners)
		}
		if err := validateDeploymentControlRevisionIncrement(parent); err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE deployments SET state=?,finished_at=?,control_revision=control_revision+1
 WHERE deployment_id=? AND state=?`, DeploymentFinished, fmtTime(now), parent.DeploymentID, DeploymentPaused)
		if err != nil {
			return fmt.Errorf("store: finish retry parent %s: %w", parent.DeploymentID, err)
		}
		if changed, _ := res.RowsAffected(); changed != 1 {
			return fmt.Errorf("%w: retry parent %s changed while reserving resource", ErrDeploymentRetryParentRunning, parent.DeploymentID)
		}
		if err := recordDeploymentSoakBoundaryTx(tx, parent.DeploymentID); err != nil {
			return err
		}
	case DeploymentFinished:
		if len(owners) != 0 {
			return deploymentActiveResourceConflict(n.ResourceKind, n.ResourceID, owners)
		}
	default:
		return fmt.Errorf("%w: ancestor %s has state %q", ErrDeploymentRetryParentRunning, parent.DeploymentID, parent.State)
	}
	return nil
}

func deploymentResourceHasNonTerminalJobsTx(tx dbTx, resourceKind, resourceID string) (bool, error) {
	var active bool
	err := tx.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM deployments p
 LEFT JOIN desired_state d ON d.desired_id=p.desired_id
 WHERE ((p.resource_kind=? AND p.resource_id=?) OR (d.resource_kind=? AND d.resource_id=?))
	AND (
	  EXISTS (SELECT 1 FROM jobs j WHERE j.desired_id=p.desired_id AND j.state NOT IN (?,?,?,?,?))
	  OR EXISTS (SELECT 1 FROM deployment_targets t JOIN jobs j ON j.job_id=t.job_id
	             WHERE t.deployment_id=p.deployment_id AND j.state NOT IN (?,?,?,?,?))
	))`, resourceKind, resourceID,
		resourceKind, resourceID,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("store: inspect deployment-owned active resource jobs: %w", err)
	}
	return active, nil
}

func validateDeploymentControlRevisionIncrement(d Deployment) error {
	if d.ControlRevision < 0 || d.ControlRevision >= MaxDeploymentControlRevision {
		return fmt.Errorf("%w: deployment %s has control_revision=%d",
			ErrDeploymentControlRevisionLimit, d.DeploymentID, d.ControlRevision)
	}
	return nil
}

func validateActiveDeploymentResourceOwnerTx(tx dbTx, d Deployment) error {
	owners, err := activeDeploymentResourceOwnersTx(tx, d.ResourceKind, d.ResourceID)
	if err != nil {
		return err
	}
	if len(owners) != 1 || owners[0] != d.DeploymentID {
		return deploymentActiveResourceConflict(d.ResourceKind, d.ResourceID, owners)
	}
	return nil
}

// rejectLegacyDuplicateActiveDeploymentResources is deliberately read-only.
// It runs before schema migration so a legacy DB with multiple owners is not
// silently repaired by choosing a winner (and is not partially migrated first).
func rejectLegacyDuplicateActiveDeploymentResources(db *sql.DB) error {
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM sqlite_master WHERE type='table' AND name='deployments')`).Scan(&exists); err != nil {
		return fmt.Errorf("store: inspect deployments table before active-owner migration: %w", err)
	}
	if !exists {
		return nil
	}
	rows, err := db.Query(`SELECT resource_kind,resource_id,deployment_id FROM deployments
 WHERE state IN (?,?) ORDER BY resource_kind,resource_id,deployment_id`, DeploymentRunning, DeploymentPaused)
	if err != nil {
		return fmt.Errorf("store: inspect legacy active deployment owners: %w", err)
	}
	defer rows.Close()
	type resource struct{ kind, id string }
	groups := make(map[resource][]string)
	var order []resource
	for rows.Next() {
		var key resource
		var deploymentID string
		if err := rows.Scan(&key.kind, &key.id, &deploymentID); err != nil {
			return fmt.Errorf("store: scan legacy active deployment owner: %w", err)
		}
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], deploymentID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: scan legacy active deployment owner: %w", err)
	}
	for _, key := range order {
		if len(groups[key]) > 1 {
			return fmt.Errorf("%w: %s:%s currently has %d running/paused deployments [%s]; this Hub cannot open this ledger, use the previous Hub to restore service",
				ErrDeploymentActiveResource, key.kind, key.id, len(groups[key]), strings.Join(groups[key], ","))
		}
	}
	return nil
}

func deploymentHasNonTerminalJobs(q deploymentQueryRower, deploymentID string) (bool, error) {
	var active bool
	err := q.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM deployment_targets t JOIN jobs j ON j.job_id=t.job_id
 WHERE t.deployment_id=? AND j.state NOT IN (?,?,?,?,?))`, deploymentID,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("store: inspect deployment active jobs: %w", err)
	}
	return active, nil
}

// validateDeploymentJobGraph rejects malformed or legacy target links before
// any lifecycle transition can hide a live job, release a resource owner, or
// create another batch/retry. A job is authority for exactly one deployment
// target across the whole ledger, and its immutable execution identity must be
// the identity assigned by that target's deployment.
func validateDeploymentJobGraph(q deploymentQueryRower, deploymentID string) error {
	var malformed bool
	if err := q.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM deployments p LEFT JOIN desired_state d ON d.desired_id=p.desired_id
 WHERE p.deployment_id=? AND (
	   p.channel NOT IN ('canary','stable')
	OR p.revision<=0
	OR d.desired_id IS NULL
    OR d.scope_type IS NOT 'channel'
    OR d.scope_id IS NOT p.channel
    OR d.resource_kind IS NOT p.resource_kind
    OR d.resource_id IS NOT p.resource_id
    OR d.revision IS NOT p.revision
 ))`, deploymentID).Scan(&malformed); err != nil {
		return fmt.Errorf("store: inspect deployment desired-state identity: %w", err)
	}
	if malformed {
		return fmt.Errorf("%w: deployment %s has an identity-mismatched desired state",
			ErrDeploymentJobGraphMismatch, deploymentID)
	}
	if err := q.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM deployment_targets t LEFT JOIN machine_registry m ON m.machine_id=t.machine_id
 WHERE t.deployment_id=? AND m.machine_id IS NULL
 )`, deploymentID).Scan(&malformed); err != nil {
		return fmt.Errorf("store: inspect deployment target registry identities: %w", err)
	}
	if malformed {
		return fmt.Errorf("%w: deployment %s has a target missing from the machine registry",
			ErrDeploymentJobGraphMismatch, deploymentID)
	}
	var linked, opened int
	if err := q.QueryRow(`SELECT
 COALESCE(SUM(CASE WHEN job_id IS NOT NULL THEN 1 ELSE 0 END),0),
 COALESCE(MAX(CASE WHEN job_id IS NOT NULL THEN batch_no ELSE 0 END),0)
 FROM deployment_targets WHERE deployment_id=?`, deploymentID).Scan(&linked, &opened); err != nil {
		return fmt.Errorf("store: inspect deployment opened job prefix: %w", err)
	}
	if linked == 0 || opened <= 0 {
		return fmt.Errorf("%w: deployment %s has no opened job prefix", ErrDeploymentJobGraphMismatch, deploymentID)
	}
	var prefixBatches, missing int
	if err := q.QueryRow(`SELECT COUNT(DISTINCT batch_no),
 COALESCE(SUM(CASE WHEN job_id IS NULL THEN 1 ELSE 0 END),0)
 FROM deployment_targets
 WHERE deployment_id=? AND batch_no BETWEEN 1 AND ? AND COALESCE(excluded_reason,'')=''`,
		deploymentID, opened).Scan(&prefixBatches, &missing); err != nil {
		return fmt.Errorf("store: inspect deployment linked job prefix: %w", err)
	}
	if prefixBatches != opened || missing != 0 {
		return fmt.Errorf("%w: deployment %s has a partial or non-contiguous opened job prefix",
			ErrDeploymentJobGraphMismatch, deploymentID)
	}
	err := q.QueryRow(`SELECT EXISTS(
 SELECT 1
 FROM deployment_targets t
 JOIN deployments p ON p.deployment_id=t.deployment_id
 LEFT JOIN jobs j ON j.job_id=t.job_id
 WHERE t.deployment_id=? AND t.job_id IS NOT NULL AND (
       j.job_id IS NULL
    OR j.machine_id IS NOT t.machine_id
    OR j.desired_id IS NOT p.desired_id
    OR j.revision IS NOT p.revision
    OR t.batch_no<=0
    OR COALESCE(t.excluded_reason,'')<>''
    OR (SELECT COUNT(*) FROM deployment_targets refs WHERE refs.job_id=t.job_id)<>1
 ))`, deploymentID).Scan(&malformed)
	if err != nil {
		return fmt.Errorf("store: inspect deployment target job graph: %w", err)
	}
	if malformed {
		return fmt.Errorf("%w: deployment %s has a missing, shared, or identity-mismatched linked job",
			ErrDeploymentJobGraphMismatch, deploymentID)
	}
	err = q.QueryRow(`SELECT EXISTS(
 SELECT 1
 FROM deployments p JOIN jobs j ON j.desired_id=p.desired_id
 WHERE p.deployment_id=? AND NOT EXISTS (
   SELECT 1 FROM deployment_targets t
   WHERE t.deployment_id=p.deployment_id AND t.job_id=j.job_id
     AND t.machine_id=j.machine_id AND j.revision=p.revision
 ))`, deploymentID).Scan(&malformed)
	if err != nil {
		return fmt.Errorf("store: inspect deployment orphan jobs: %w", err)
	}
	if malformed {
		return fmt.Errorf("%w: deployment %s has an unlinked or identity-mismatched job for its desired state",
			ErrDeploymentJobGraphMismatch, deploymentID)
	}
	rows, err := q.Query(`SELECT j.state,j.terminal_at
	 FROM deployment_targets t JOIN jobs j ON j.job_id=t.job_id
	 WHERE t.deployment_id=?`, deploymentID)
	if err != nil {
		return fmt.Errorf("store: inspect deployment job lifecycle facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state deploy.JobState
		var terminalAt sql.NullString
		if err := rows.Scan(&state, &terminalAt); err != nil {
			return fmt.Errorf("store: scan deployment job lifecycle facts: %w", err)
		}
		if !deploy.IsKnownJobState(state) || deploy.IsTerminal(state) != terminalAt.Valid ||
			(terminalAt.Valid && (parseTime(terminalAt.String).IsZero() ||
				fmtTime(parseTime(terminalAt.String)) != terminalAt.String)) {
			return fmt.Errorf("%w: deployment %s has an incoherent linked job lifecycle",
				ErrDeploymentJobGraphMismatch, deploymentID)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate deployment job lifecycle facts: %w", err)
	}
	return nil
}

// validateDeploymentJobMaterialGraph is deliberately separate from the
// lifecycle graph. Opening a delayed batch, retrying, promotion, and cached
// success evidence depend on the bytes named by prior jobs. Finish-only
// Continue and Abandon do not materialize work and must remain usable even if
// an old artifact record is unavailable or malformed.
func validateDeploymentJobMaterialGraph(q deploymentQueryRower, deploymentID string) error {
	d, err := deploymentByID(q, deploymentID)
	if err != nil {
		return err
	}
	rows, err := q.Query(`SELECT j.artifact_digest
	 FROM deployment_targets t JOIN jobs j ON j.job_id=t.job_id
	 WHERE t.deployment_id=?`, deploymentID)
	if err != nil {
		return fmt.Errorf("store: inspect deployment job material: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var digest sql.NullString
		if err := rows.Scan(&digest); err != nil {
			return fmt.Errorf("store: scan deployment job material: %w", err)
		}
		if err := validateDeploymentMaterial(d.ResourceKind, d.ResourceID, d.Spec, digest.String); err != nil {
			return fmt.Errorf("deployment %s has incoherent linked job material: %w", deploymentID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate deployment job material: %w", err)
	}
	return nil
}

func jobTx(tx dbTx, jobID string) (Job, error) {
	return scanJob(tx.QueryRow(`SELECT job_id,machine_id,desired_id,revision,state,lease_token,
 lease_expires_at,execution_timeout,artifact_digest,irreversible,created_at,terminal_at
 FROM jobs WHERE job_id = ?`, jobID))
}

type deploymentQueryRower interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

func deploymentByID(q deploymentQueryRower, id string) (Deployment, error) {
	return scanDeployment(q.QueryRow(`SELECT p.deployment_id,p.channel,p.desired_id,p.resource_kind,p.resource_id,
	 p.revision,p.control_revision,p.batch_size,p.pause_after_canary,p.state,p.created_at,p.created_by,p.paused_at,p.finished_at,p.retry_of,d.spec
 FROM deployments p JOIN desired_state d ON d.desired_id=p.desired_id WHERE p.deployment_id=?`, id))
}

func (s *Store) Deployment(id string) (Deployment, error) {
	return deploymentByID(s.rdb, id)
}

// deploymentRetryLineage 回傳 root → 最老 ancestor。DB 與 Tx 共用同一條 walker；
// seen 擋 cycle，硬上限則擋住損壞帳本造成無限／無界查詢。
func deploymentRetryLineage(q deploymentQueryRower, root Deployment) ([]Deployment, error) {
	lineage := []Deployment{root}
	seen := map[string]struct{}{root.DeploymentID: {}}
	for current := root; current.RetryOf != ""; {
		parentID := current.RetryOf
		if _, duplicate := seen[parentID]; duplicate {
			return nil, fmt.Errorf("%w: %s", ErrDeploymentRetryCycle, parentID)
		}
		if len(lineage) >= maxDeploymentRetryLineage {
			return nil, fmt.Errorf("%w: %s", ErrDeploymentRetryTooDeep, parentID)
		}
		parent, err := deploymentByID(q, parentID)
		if errors.Is(err, ErrDeploymentNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrDeploymentRetryParentNotFound, parentID)
		}
		if err != nil {
			return nil, err
		}
		seen[parentID] = struct{}{}
		lineage = append(lineage, parent)
		current = parent
	}
	return lineage, nil
}

func deploymentRetryLineageFromID(q deploymentQueryRower, id string) ([]Deployment, error) {
	root, err := deploymentByID(q, id)
	if errors.Is(err, ErrDeploymentNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrDeploymentRetryParentNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return deploymentRetryLineage(q, root)
}

func scanDeployment(row rowScanner) (Deployment, error) {
	var d Deployment
	var created string
	var pauseAfterCanary int
	var paused, finished, retry sql.NullString
	err := row.Scan(&d.DeploymentID, &d.Channel, &d.DesiredID, &d.ResourceKind, &d.ResourceID,
		&d.Revision, &d.ControlRevision, &d.BatchSize, &pauseAfterCanary, &d.State, &created, &d.CreatedBy, &paused, &finished, &retry, &d.Spec)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrDeploymentNotFound
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("store: scan deployment: %w", err)
	}
	d.PauseAfterCanary = pauseAfterCanary != 0
	d.CreatedAt, d.PausedAt, d.FinishedAt, d.RetryOf = parseTime(created), parseTimePtr(paused), parseTimePtr(finished), retry.String
	return d, nil
}

func (s *Store) DeploymentTargets(id string) ([]DeploymentTarget, error) {
	rows, err := s.rdb.Query(`SELECT t.machine_id,m.display_name,t.batch_no,t.job_id,t.excluded_reason,
	 j.machine_id,j.desired_id,j.revision,
	 CASE WHEN t.job_id IS NULL THEN 0 ELSE (SELECT COUNT(*) FROM deployment_targets refs WHERE refs.job_id=t.job_id) END,
	 j.state,j.created_at,j.terminal_at,
	 COALESCE((SELECT MAX(e.received_at) FROM job_events e WHERE e.job_id=j.job_id),j.created_at)
 FROM deployment_targets t JOIN machine_registry m ON m.machine_id=t.machine_id
 LEFT JOIN jobs j ON j.job_id=t.job_id WHERE t.deployment_id=?
 ORDER BY m.display_name,t.machine_id`, id)
	if err != nil {
		return nil, fmt.Errorf("store: deployment targets: %w", err)
	}
	defer rows.Close()
	var out []DeploymentTarget
	for rows.Next() {
		var t DeploymentTarget
		var jobID, excluded, jobMachineID, jobDesiredID, state, created, terminal, activity sql.NullString
		var jobRevision sql.NullInt64
		if err := rows.Scan(&t.MachineID, &t.DisplayName, &t.BatchNo, &jobID, &excluded,
			&jobMachineID, &jobDesiredID, &jobRevision, &t.JobReferences,
			&state, &created, &terminal, &activity); err != nil {
			return nil, fmt.Errorf("store: scan deployment target: %w", err)
		}
		t.JobID, t.ExcludedReason, t.JobState = jobID.String, excluded.String, deploy.JobState(state.String)
		t.JobMachineID, t.JobDesiredID, t.JobRevision = jobMachineID.String, jobDesiredID.String, deploy.Revision(jobRevision.Int64)
		t.CreatedAt, t.TerminalAt, t.LastActivity = parseTimeNull(created), parseTimePtr(terminal), parseTimeNull(activity)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeploymentView(id string, now time.Time) (DeploymentView, error) {
	d, err := s.Deployment(id)
	if err != nil {
		return DeploymentView{}, err
	}
	if err := validateDeploymentJobGraph(s.rdb, id); err != nil {
		return DeploymentView{}, err
	}
	lineage, err := deploymentRetryLineage(s.rdb, d)
	if err != nil {
		return DeploymentView{}, err
	}
	targets, err := s.DeploymentTargets(id)
	if err != nil {
		return DeploymentView{}, err
	}
	var desiredJobCount int
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM jobs WHERE desired_id=?`, d.DesiredID).Scan(&desiredJobCount); err != nil {
		return DeploymentView{}, fmt.Errorf("store: count deployment desired-state jobs: %w", err)
	}
	boundaryPause, err := s.deploymentBoundaryPause(id)
	if err != nil {
		return DeploymentView{}, err
	}
	v := DeploymentView{Deployment: d, Targets: targets, DesiredJobCount: desiredJobCount,
		Counts: map[deploy.JobState]int{}, Attempt: len(lineage), BoundaryPause: boundaryPause}
	for i := range v.Targets {
		t := &v.Targets[i]
		if t.BatchNo > v.TotalBatches {
			v.TotalBatches = t.BatchNo
		}
		if t.JobID == "" {
			continue
		}
		if t.BatchNo > v.OpenedBatch {
			v.OpenedBatch = t.BatchNo
		}
		v.Counts[t.JobState]++
		if deploy.IsTerminal(t.JobState) && t.JobState != deploy.Succeeded {
			t.StuckKind = "terminal_failure"
			v.TerminalStuck++
		} else if !deploy.IsTerminal(t.JobState) && !t.LastActivity.IsZero() && now.Sub(t.LastActivity) > StuckThreshold {
			t.StuckKind = "no_event"
			v.SilentStuck++
		}
	}
	v.Stuck = v.TerminalStuck + v.SilentStuck
	return v, nil
}

func (s *Store) deploymentBoundaryPause(id string) (*DeploymentBoundaryPause, error) {
	var pause DeploymentBoundaryPause
	var pausedAt string
	err := s.rdb.QueryRow(`SELECT opened_batch,kind,reason,paused_at
	 FROM deployment_boundary_pauses WHERE deployment_id=?`, id).
		Scan(&pause.OpenedBatch, &pause.Kind, &pause.Reason, &pausedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: deployment boundary pause: %w", err)
	}
	pause.PausedAt = parseTime(pausedAt)
	return &pause, nil
}

func (s *Store) ListDeployments(now time.Time) ([]DeploymentView, error) {
	rows, err := s.rdb.Query(`SELECT deployment_id FROM deployments ORDER BY created_at DESC,deployment_id DESC`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []DeploymentView
	for _, id := range ids {
		v, err := s.DeploymentView(id, now)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) RunningDeployments(now time.Time) ([]DeploymentView, error) {
	all, err := s.ListDeployments(now)
	if err != nil {
		return nil, err
	}
	var out []DeploymentView
	for _, d := range all {
		if d.State == DeploymentRunning {
			out = append(out, d)
		}
	}
	return out, nil
}

func deploymentJobTemplateTx(tx dbTx, deploymentID string) (NewJob, error) {
	var n NewJob
	var digest sql.NullString
	err := tx.QueryRow(`SELECT j.artifact_digest,j.irreversible,j.execution_timeout
 FROM deployment_targets t JOIN jobs j ON j.job_id=t.job_id
 WHERE t.deployment_id=? ORDER BY t.batch_no,t.machine_id LIMIT 1`, deploymentID).
		Scan(&digest, &n.Irreversible, &n.ExecutionTimeout)
	if err != nil {
		return NewJob{}, fmt.Errorf("store: deployment job template: %w", err)
	}
	n.ArtifactDigest = digest.String
	return n, nil
}

func (s *Store) DeploymentJobTemplate(deploymentID string) (NewJob, error) {
	var n NewJob
	var digest sql.NullString
	err := s.rdb.QueryRow(`SELECT j.artifact_digest,j.irreversible,j.execution_timeout
 FROM deployment_targets t JOIN jobs j ON j.job_id=t.job_id
 WHERE t.deployment_id=? ORDER BY t.batch_no,t.machine_id LIMIT 1`, deploymentID).
		Scan(&digest, &n.Irreversible, &n.ExecutionTimeout)
	if err != nil {
		return NewJob{}, fmt.Errorf("store: deployment job template: %w", err)
	}
	n.ArtifactDigest = digest.String
	return n, nil
}

type deploymentBatchStatus struct {
	targets int
	opened  int
}

// validateStoredDeploymentBatchPlan 重驗可能由舊版 Hub／手工 ledger 留下的計畫。
// Initial create 的 validation 不能替數小時後才真正開 job 的批次保證 blast radius。
func validateStoredDeploymentBatchPlan(q deploymentQueryRower, d Deployment) error {
	if d.BatchSize <= 0 || d.BatchSize > MaxDeploymentBatchSize {
		return fmt.Errorf("%w: deployment %s batch_size=%d, not in 1..%d",
			ErrDeploymentInvalidBatchPlan, d.DeploymentID, d.BatchSize, MaxDeploymentBatchSize)
	}
	rows, err := q.Query(`SELECT batch_no,excluded_reason FROM deployment_targets WHERE deployment_id=?`, d.DeploymentID)
	if err != nil {
		return fmt.Errorf("store: inspect stored deployment batch plan: %w", err)
	}
	defer rows.Close()
	counts := map[int]int{}
	maxBatch := 0
	targets := 0
	for rows.Next() {
		var batchNo int
		var excluded sql.NullString
		if err := rows.Scan(&batchNo, &excluded); err != nil {
			return fmt.Errorf("store: scan stored deployment batch plan: %w", err)
		}
		targets++
		if excluded.Valid && excluded.String != "" {
			switch excluded.String {
			case "conflict", "missing_package", "unknown_node", "noncompliant":
			default:
				return fmt.Errorf("%w: deployment %s has unknown exclusion %q",
					ErrDeploymentInvalidBatchPlan, d.DeploymentID, excluded.String)
			}
			if batchNo != 0 {
				return fmt.Errorf("%w: excluded target in deployment %s has batch_no=%d",
					ErrDeploymentInvalidBatchPlan, d.DeploymentID, batchNo)
			}
			continue
		}
		if batchNo <= 0 {
			return fmt.Errorf("%w: included target in deployment %s has batch_no=%d",
				ErrDeploymentInvalidBatchPlan, d.DeploymentID, batchNo)
		}
		counts[batchNo]++
		if counts[batchNo] > d.BatchSize || counts[batchNo] > MaxDeploymentBatchSize {
			return fmt.Errorf("%w: deployment %s batch %d has %d targets (batch_size=%d, max=%d)",
				ErrDeploymentInvalidBatchPlan, d.DeploymentID, batchNo, counts[batchNo], d.BatchSize, MaxDeploymentBatchSize)
		}
		if batchNo > maxBatch {
			maxBatch = batchNo
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: scan stored deployment batch plan: %w", err)
	}
	if targets == 0 || maxBatch == 0 || len(counts) != maxBatch {
		return fmt.Errorf("%w: deployment %s has no included target or non-contiguous batches",
			ErrDeploymentInvalidBatchPlan, d.DeploymentID)
	}
	return nil
}

func deploymentBatchStatusTx(tx dbTx, deploymentID string, batchNo int) (deploymentBatchStatus, error) {
	var status deploymentBatchStatus
	if err := tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN job_id IS NOT NULL THEN 1 ELSE 0 END),0)
	 FROM deployment_targets WHERE deployment_id=? AND batch_no=?`, deploymentID, batchNo).
		Scan(&status.targets, &status.opened); err != nil {
		return status, fmt.Errorf("store: inspect deployment batch %d: %w", batchNo, err)
	}
	return status, nil
}

func deploymentOpenedBatchTx(tx dbTx, deploymentID string) (int, error) {
	var opened int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(CASE WHEN job_id IS NOT NULL THEN batch_no ELSE 0 END),0)
	 FROM deployment_targets WHERE deployment_id=?`, deploymentID).Scan(&opened); err != nil {
		return 0, fmt.Errorf("store: inspect opened deployment batch: %w", err)
	}
	return opened, nil
}

func deploymentOpenedPrefixCompleteTx(tx dbTx, deploymentID string, opened int) (bool, error) {
	if opened <= 0 {
		return false, nil
	}
	var batches, missingJobs int
	if err := tx.QueryRow(`SELECT COUNT(DISTINCT batch_no),
 COALESCE(SUM(CASE WHEN job_id IS NULL THEN 1 ELSE 0 END),0)
 FROM deployment_targets WHERE deployment_id=? AND batch_no BETWEEN 1 AND ?`, deploymentID, opened).
		Scan(&batches, &missingJobs); err != nil {
		return false, fmt.Errorf("store: inspect opened deployment prefix: %w", err)
	}
	return batches == opened && missingJobs == 0, nil
}

func deploymentOpenedPrefixAllSucceededTx(tx dbTx, d Deployment, opened int) (bool, error) {
	var targets, succeeded int
	if err := tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN j.state=?
		AND j.machine_id=t.machine_id AND j.desired_id=? AND j.revision=? THEN 1 ELSE 0 END),0)
	 FROM deployment_targets t LEFT JOIN jobs j ON j.job_id=t.job_id
	 WHERE t.deployment_id=? AND t.batch_no BETWEEN 1 AND ?`, deploy.Succeeded,
		d.DesiredID, d.Revision, d.DeploymentID, opened).
		Scan(&targets, &succeeded); err != nil {
		return false, fmt.Errorf("store: inspect prior deployment batches: %w", err)
	}
	return targets > 0 && succeeded == targets, nil
}

func deploymentBatchAllTerminalTx(tx dbTx, deploymentID string, batchNo int) (bool, error) {
	var targets, terminal int
	if err := tx.QueryRow(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN j.state IN (?,?,?,?,?) THEN 1 ELSE 0 END),0)
	 FROM deployment_targets t LEFT JOIN jobs j ON j.job_id=t.job_id
	 WHERE t.deployment_id=? AND t.batch_no=?`,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention,
		deploymentID, batchNo).Scan(&targets, &terminal); err != nil {
		return false, fmt.Errorf("store: inspect current deployment batch: %w", err)
	}
	return targets > 0 && terminal == targets, nil
}

func (s *Store) gateStableBatchTx(d Deployment, n NewJob) error {
	if d.Channel != "stable" {
		return nil
	}
	version, digest, err := stableOpenClawMaterialParts(d.Channel, d.ResourceKind, d.ResourceID, d.Spec, n.ArtifactDigest)
	if err != nil {
		return err
	}
	// 必須在 BEGIN IMMEDIATE 已取得 writer reservation 之後才讀時鐘與 facts；
	// caller 傳入的 now 可能是在等鎖之前取得，不能拿來判完整工作天。
	decision, err := s.PreviewStableOpenClawPromotion(version, digest, s.now().UTC())
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fmt.Errorf("%w: %s", ErrPromoteLocked, decision.Summary())
	}
	if s.afterBatchPromoteGate != nil {
		s.afterBatchPromoteGate()
	}
	return nil
}

// OpenDeploymentBatch 對已完整開過的批次只讀回原 job；真的要新增 job 時，
// 只准開 next batch、前一批必須全成功，stable 還要在同一 writer transaction 重跑 gate。
func (s *Store) OpenDeploymentBatch(id string, batchNo int, now time.Time) ([]Job, error) {
	tx, err := s.beginWrite(context.Background(), "open_deployment_batch")
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	d, err := scanDeployment(tx.QueryRow(`SELECT p.deployment_id,p.channel,p.desired_id,p.resource_kind,p.resource_id,
	 p.revision,p.control_revision,p.batch_size,p.pause_after_canary,p.state,p.created_at,p.created_by,p.paused_at,p.finished_at,p.retry_of,d.spec
 FROM deployments p JOIN desired_state d ON d.desired_id=p.desired_id WHERE p.deployment_id=? AND p.state=?`, id, DeploymentRunning))
	if err != nil {
		return nil, err
	}
	if err := validateActiveDeploymentResourceOwnerTx(tx, d); err != nil {
		return nil, err
	}
	if err := validateDeploymentJobGraph(tx, d.DeploymentID); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDeploymentBatchNotReady, err)
	}
	if err := validateStoredDeploymentBatchPlan(tx, d); err != nil {
		return nil, err
	}
	status, err := deploymentBatchStatusTx(tx, id, batchNo)
	if err != nil {
		return nil, err
	}
	if status.targets == 0 {
		return nil, fmt.Errorf("%w: deployment %s has no batch %d", ErrDeploymentBatchNotReady, id, batchNo)
	}
	if status.opened != 0 && status.opened != status.targets {
		return nil, fmt.Errorf("%w: deployment %s batch %d is only partially opened", ErrDeploymentBatchNotReady, id, batchNo)
	}
	if status.opened == 0 {
		if err := validateDeploymentJobMaterialGraph(tx, d.DeploymentID); err != nil {
			return nil, err
		}
		if err := validateDeploymentControlRevisionIncrement(d); err != nil {
			return nil, err
		}
	}
	n, err := deploymentJobTemplateTx(tx, id)
	if err != nil {
		return nil, err
	}
	if status.opened == 0 {
		opened, err := deploymentOpenedBatchTx(tx, id)
		if err != nil {
			return nil, err
		}
		prefixComplete, err := deploymentOpenedPrefixCompleteTx(tx, id, opened)
		if err != nil {
			return nil, err
		}
		if !prefixComplete {
			return nil, fmt.Errorf("%w: deployment %s batches 1..%d have unopened gaps",
				ErrDeploymentBatchNotReady, id, opened)
		}
		if opened == 0 || batchNo != opened+1 {
			return nil, fmt.Errorf("%w: deployment %s is currently opened through batch %d, cannot open batch %d",
				ErrDeploymentBatchNotReady, id, opened, batchNo)
		}
		ready, err := deploymentOpenedPrefixAllSucceededTx(tx, d, opened)
		if err != nil {
			return nil, err
		}
		if !ready {
			return nil, fmt.Errorf("%w: deployment %s batches 1..%d have not all succeeded", ErrDeploymentBatchNotReady, id, opened)
		}
		activeResourceJobs, err := deploymentResourceHasNonTerminalJobsTx(tx, d.ResourceKind, d.ResourceID)
		if err != nil {
			return nil, err
		}
		if activeResourceJobs {
			return nil, fmt.Errorf("%w: %s:%s has nonterminal jobs owned by another deployment",
				ErrDeploymentActiveResource, d.ResourceKind, d.ResourceID)
		}
		if err := s.gateStableBatchTx(d, n); err != nil {
			return nil, err
		}
	}
	jobs, err := createDeploymentBatchTx(tx, d, batchNo, n, now.UTC())
	if err != nil {
		return nil, err
	}
	if status.opened == 0 {
		res, err := tx.Exec(`UPDATE deployments SET control_revision=control_revision+1
 WHERE deployment_id=? AND state=? AND control_revision=?`, d.DeploymentID, DeploymentRunning, d.ControlRevision)
		if err != nil {
			return nil, fmt.Errorf("store: advance deployment control revision after opening batch: %w", err)
		}
		if changed, _ := res.RowsAffected(); changed != 1 {
			return nil, fmt.Errorf("store: deployment control revision changed while opening batch")
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) SetDeploymentState(id, from, to string, now time.Time) (bool, error) {
	// Exported driver 只需要這兩條邊；paused 的 override/收尾只能走
	// ContinueDeployment，否則 caller 可把 failure-stop 自己改回 running。
	if from != DeploymentRunning || (to != DeploymentPaused && to != DeploymentFinished) {
		return false, fmt.Errorf("%w: %q -> %q", ErrDeploymentBadTransition, from, to)
	}
	tx, err := s.beginWrite(context.Background(), "set_deployment_state")
	if err != nil {
		return false, fmt.Errorf("store: begin deployment state transition: %w", err)
	}
	defer tx.Rollback()
	d, err := deploymentByID(tx, id)
	if errors.Is(err, ErrDeploymentNotFound) || (err == nil && d.State != from) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := validateDeploymentControlRevisionIncrement(d); err != nil {
		return false, err
	}
	if to == DeploymentFinished {
		// Finishing releases the cross-channel resource owner. It is therefore an
		// admission boundary, not cosmetic state: an arbitrary caller must not
		// hide live/unopened work and open a second deployment on disjoint targets.
		if err := validateActiveDeploymentResourceOwnerTx(tx, d); err != nil {
			return false, err
		}
		if err := validateDeploymentJobGraph(tx, d.DeploymentID); err != nil {
			return false, err
		}
		if err := validateStoredDeploymentBatchPlan(tx, d); err != nil {
			return false, err
		}
		active, err := deploymentHasNonTerminalJobs(tx, d.DeploymentID)
		if err != nil {
			return false, err
		}
		if active {
			return false, fmt.Errorf("%w: deployment %s has active jobs remaining",
				ErrDeploymentFinishNotReady, d.DeploymentID)
		}
		var included, opened, succeeded int
		if err := tx.QueryRow(`SELECT COUNT(*),
 COALESCE(SUM(CASE WHEN t.job_id IS NOT NULL THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN j.state=? AND j.machine_id=t.machine_id
                        AND j.desired_id=? AND j.revision=? THEN 1 ELSE 0 END),0)
 FROM deployment_targets t LEFT JOIN jobs j ON j.job_id=t.job_id
 WHERE t.deployment_id=? AND t.batch_no>0 AND COALESCE(t.excluded_reason,'')=''`,
			deploy.Succeeded, d.DesiredID, d.Revision, d.DeploymentID).
			Scan(&included, &opened, &succeeded); err != nil {
			return false, fmt.Errorf("store: inspect deployment finish readiness: %w", err)
		}
		if included == 0 || opened != included || succeeded != included {
			return false, fmt.Errorf("%w: deployment %s included=%d opened=%d succeeded=%d; running can only transition to finished after all targets are opened and succeeded",
				ErrDeploymentFinishNotReady, id, included, opened, succeeded)
		}
		// finished_at is the business-day authority used by the stable promote
		// gate.  The caller's timestamp is useful for a pause/event narrative but
		// must not be able to move this authority across a calendar boundary.
		// Read the Store clock only after BEGIN IMMEDIATE has reserved the writer.
		now = s.now().UTC()
	}
	sets := "state = ?, control_revision = control_revision + 1"
	args := []any{to}
	if to == DeploymentPaused {
		sets += ", paused_at = ?"
		args = append(args, fmtTime(now))
	}
	if to == DeploymentFinished {
		sets += ", finished_at = ?"
		args = append(args, fmtTime(now))
	}
	args = append(args, id, from)
	res, err := tx.Exec(`UPDATE deployments SET `+sets+` WHERE deployment_id=? AND state=?`, args...)
	if err != nil {
		return false, fmt.Errorf("store: set deployment state: %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return false, nil
	}
	if to == DeploymentFinished {
		if err := recordDeploymentSoakBoundaryTx(tx, d.DeploymentID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit deployment state: %w", err)
	}
	return true, nil
}

// AbandonDeployment is the explicit recovery hatch for a paused deployment
// whose boundary guard cannot become true without running a fresh deployment
// (notably a promote-locked stable attempt that owns the OpenClaw slot). It
// never changes job state and never relaxes a promote gate: only a paused
// deployment whose opened jobs are already terminal can release ownership.
func (s *Store) AbandonDeployment(id string, now time.Time) (Deployment, error) {
	tx, err := s.beginWrite(context.Background(), "abandon_deployment")
	if err != nil {
		return Deployment{}, fmt.Errorf("store: begin abandon deployment: %w", err)
	}
	defer tx.Rollback()
	d, err := s.abandonDeploymentTx(tx, id, now)
	if err != nil {
		return Deployment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, fmt.Errorf("store: commit abandon deployment: %w", err)
	}
	return d, nil
}

// abandonDeploymentTx is shared by the legacy Store call and the canonical
// operator authority. The caller owns commit so lifecycle, event, receipt and
// audit can be one atomic writer transaction.
func (s *Store) abandonDeploymentTx(tx dbTx, id string, now time.Time) (Deployment, error) {
	d, err := deploymentByID(tx, id)
	if err != nil {
		return Deployment{}, err
	}
	if d.State != DeploymentPaused {
		return Deployment{}, fmt.Errorf("%w: deployment %s is %s", ErrDeploymentNotPaused, id, d.State)
	}
	if err := validateDeploymentControlRevisionIncrement(d); err != nil {
		return Deployment{}, err
	}
	if err := validateDeploymentJobGraph(tx, d.DeploymentID); err != nil {
		return Deployment{}, err
	}
	if err := validateStoredDeploymentBatchPlan(tx, d); err != nil {
		return Deployment{}, err
	}
	if err := validateActiveDeploymentResourceOwnerTx(tx, d); err != nil {
		return Deployment{}, err
	}
	active, err := deploymentHasNonTerminalJobs(tx, id)
	if err != nil {
		return Deployment{}, err
	}
	if active {
		return Deployment{}, fmt.Errorf("%w: deployment %s has active jobs remaining", ErrDeploymentAbandonActiveJobs, id)
	}
	// Abandon also releases the active resource owner and materializes a
	// finished_at.  Use the Store clock after the writer transaction begins;
	// caller-provided wall time is not promotion evidence.
	now = s.now().UTC().Truncate(time.Second)
	res, err := tx.Exec(`UPDATE deployments
 SET state=?,finished_at=?,control_revision=control_revision+1
 WHERE deployment_id=? AND state=? AND control_revision=?`,
		DeploymentFinished, fmtTime(now), id, DeploymentPaused, d.ControlRevision)
	if err != nil {
		return Deployment{}, fmt.Errorf("store: abandon deployment: %w", err)
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return Deployment{}, fmt.Errorf("%w: deployment %s changed while abandoning", ErrDeploymentNotPaused, id)
	}
	if err := recordDeploymentSoakBoundaryTx(tx, d.DeploymentID); err != nil {
		return Deployment{}, err
	}
	detail := fmt.Sprintf("deployment %s 明確 abandon；paused → finished；未開批次不再開", id)
	if _, err := tx.Exec(`INSERT INTO hub_events (at,kind,detail) VALUES (?,?,?)`,
		fmtTime(now), HubDeploymentAbandoned, detail); err != nil {
		return Deployment{}, fmt.Errorf("store: record deployment abandon event: %w", err)
	}
	d.State = DeploymentFinished
	d.FinishedAt = &now
	d.ControlRevision++
	return d, nil
}

// PauseDeploymentAtBoundary materializes a deterministic OpenDeploymentBatch refusal.
// The expected opened batch is a CAS token: if another Store already opened the next
// batch, this call is a no-op and must not pause the newly-expanded deployment.
// State, reason, and the single Hub event commit together.
func (s *Store) PauseDeploymentAtBoundary(id string, expectedOpenedBatch int, kind, reason string, now time.Time) (bool, error) {
	if expectedOpenedBatch <= 0 || strings.TrimSpace(reason) == "" {
		return false, errors.New("store: deployment boundary pause needs opened batch and reason")
	}
	allowedKind := false
	for _, candidate := range []string{
		DeploymentPausePromoteLocked, DeploymentPauseConflict, DeploymentPauseStaleRevision,
		DeploymentPauseBatchNotReady, DeploymentPauseMaterial, DeploymentPauseInvalidPlan,
		DeploymentPauseTargetChanged, DeploymentPauseMachineRetired,
	} {
		if kind == candidate {
			allowedKind = true
			break
		}
	}
	if !allowedKind {
		return false, fmt.Errorf("store: unknown deployment boundary pause kind %q", kind)
	}
	now = now.UTC()
	tx, err := s.beginWrite(context.Background(), "pause_deployment_at_boundary")
	if err != nil {
		return false, fmt.Errorf("store: begin deployment boundary pause: %w", err)
	}
	defer tx.Rollback()
	d, err := deploymentByID(tx, id)
	if errors.Is(err, ErrDeploymentNotFound) || (err == nil && d.State != DeploymentRunning) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := validateDeploymentControlRevisionIncrement(d); err != nil {
		return false, err
	}

	opened, err := deploymentOpenedBatchTx(tx, id)
	if err != nil {
		return false, err
	}
	var jobsBeyond int
	if err := tx.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM deployment_targets
		WHERE deployment_id=? AND batch_no>? AND job_id IS NOT NULL
	)`, id, expectedOpenedBatch).Scan(&jobsBeyond); err != nil {
		return false, fmt.Errorf("store: inspect deployment boundary expansion: %w", err)
	}
	// The missing/malformed next target can itself be the deterministic reason we
	// are materializing.  Only a job beyond the caller's observed boundary proves
	// that another writer already expanded the deployment and makes this CAS stale.
	if opened != expectedOpenedBatch || jobsBeyond != 0 {
		return false, nil
	}
	res, err := tx.Exec(`UPDATE deployments SET state=?,paused_at=?,control_revision=control_revision+1
	 WHERE deployment_id=? AND state=?`, DeploymentPaused, fmtTime(now), id, DeploymentRunning)
	if err != nil {
		return false, fmt.Errorf("store: pause deployment at boundary: %w", err)
	}
	changed, _ := res.RowsAffected()
	if changed != 1 {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO deployment_boundary_pauses
	 (deployment_id,opened_batch,kind,reason,paused_at) VALUES(?,?,?,?,?)
	 ON CONFLICT(deployment_id) DO UPDATE SET
	 opened_batch=excluded.opened_batch,kind=excluded.kind,reason=excluded.reason,paused_at=excluded.paused_at`,
		id, expectedOpenedBatch, kind, reason, fmtTime(now)); err != nil {
		return false, fmt.Errorf("store: record deployment boundary pause: %w", err)
	}
	detail := fmt.Sprintf("deployment %s 在 batch %d 後被安全閘門停住；kind=%s；%s",
		id, expectedOpenedBatch, kind, reason)
	if _, err := tx.Exec(`INSERT INTO hub_events (at,kind,detail) VALUES (?,?,?)`,
		fmtTime(now), HubDeploymentBoundaryPaused, detail); err != nil {
		return false, fmt.Errorf("store: record deployment boundary pause event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit deployment boundary pause: %w", err)
	}
	return true, nil
}

func deploymentBatchHasFailureTerminalTx(tx dbTx, id string, batch int) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM deployment_targets t
 JOIN jobs j ON j.job_id=t.job_id
 WHERE t.deployment_id=? AND t.batch_no=? AND j.state IN (?,?,?,?)`,
		id, batch, deploy.Failed, deploy.ManualIntervention, deploy.LeaseExpired, deploy.Rejected).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: inspect deployment batch failures: %w", err)
	}
	return n > 0, nil
}

// deploymentContinuePolicy separates the product Continue from the explicit
// skip. The zero value is the historical store transition: it may open the
// next batch or finish after a failed batch. Operator Continue sets
// RefuseFailedBatch. The skip action sets SkipFailedBatch and a reason.
type deploymentContinuePolicy struct {
	RefuseFailedBatch bool
	SkipFailedBatch   bool
	SkipReason        string
}

// ContinueDeployment is the mechanical ledger transition used by store tests
// and older callers. Operator Continue does not use it: that path refuses a
// failed batch. Skip failed batch is a separate operator action.
func (s *Store) ContinueDeployment(id string, now time.Time) (Deployment, []Job, error) {
	tx, err := s.beginWrite(context.Background(), "continue_deployment")
	if err != nil {
		return Deployment{}, nil, err
	}
	defer tx.Rollback()
	d, jobs, err := s.continueDeploymentTx(tx, id, now, deploymentContinuePolicy{})
	if err != nil {
		return Deployment{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, nil, err
	}
	return d, jobs, nil
}

// continueDeploymentTx is the sole state/job/event transition. The caller
// owns commit so canonical operator idempotency and audit evidence can join it.
func (s *Store) continueDeploymentTx(tx dbTx, id string, now time.Time, policy deploymentContinuePolicy) (Deployment, []Job, error) {
	d, err := scanDeployment(tx.QueryRow(`SELECT p.deployment_id,p.channel,p.desired_id,p.resource_kind,p.resource_id,
	 p.revision,p.control_revision,p.batch_size,p.pause_after_canary,p.state,p.created_at,p.created_by,p.paused_at,p.finished_at,p.retry_of,d.spec
 FROM deployments p JOIN desired_state d ON d.desired_id=p.desired_id WHERE p.deployment_id=?`, id))
	if err != nil {
		return Deployment{}, nil, err
	}
	if d.State != DeploymentPaused {
		return Deployment{}, nil, ErrDeploymentNotPaused
	}
	if err := validateDeploymentControlRevisionIncrement(d); err != nil {
		return Deployment{}, nil, err
	}
	if err := validateDeploymentJobGraph(tx, d.DeploymentID); err != nil {
		return Deployment{}, nil, err
	}
	if err := validateActiveDeploymentResourceOwnerTx(tx, d); err != nil {
		return Deployment{}, nil, err
	}
	if err := validateStoredDeploymentBatchPlan(tx, d); err != nil {
		return Deployment{}, nil, err
	}
	var opened, total int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(CASE WHEN job_id IS NOT NULL THEN batch_no ELSE 0 END),0),COALESCE(MAX(batch_no),0)
 FROM deployment_targets WHERE deployment_id=?`, id).Scan(&opened, &total); err != nil {
		return Deployment{}, nil, err
	}
	if opened == 0 {
		return Deployment{}, nil, fmt.Errorf("%w: paused deployment %s has no opened batches", ErrDeploymentBatchNotReady, id)
	}
	prefixComplete, err := deploymentOpenedPrefixCompleteTx(tx, id, opened)
	if err != nil {
		return Deployment{}, nil, err
	}
	if !prefixComplete {
		return Deployment{}, nil, fmt.Errorf("%w: paused deployment %s batches 1..%d have unopened gaps",
			ErrDeploymentBatchNotReady, id, opened)
	}
	// Continue 是唯一能越過已終態失敗、把 paused deployment 往前推的入口；
	// 它不能同時越過任何仍會變動的舊批次。只驗最高 batch 會讓 legacy ledger
	// 中較早的 running job 被藏在後來的 terminal batch 後面，甚至在收尾時釋放
	// resource owner，讓另一張 deployment 與舊 job 並行。
	active, err := deploymentHasNonTerminalJobs(tx, id)
	if err != nil {
		return Deployment{}, nil, err
	}
	if active {
		return Deployment{}, nil, fmt.Errorf("%w: paused deployment %s has active jobs remaining",
			ErrDeploymentBatchNotReady, id)
	}
	currentTerminal, err := deploymentBatchAllTerminalTx(tx, id, opened)
	if err != nil {
		return Deployment{}, nil, err
	}
	if !currentTerminal {
		return Deployment{}, nil, fmt.Errorf("%w: paused deployment %s batch %d still has non-terminal jobs",
			ErrDeploymentBatchNotReady, id, opened)
	}
	failedBatch, err := deploymentBatchHasFailureTerminalTx(tx, id, opened)
	if err != nil {
		return Deployment{}, nil, err
	}
	if policy.RefuseFailedBatch && failedBatch {
		return Deployment{}, nil, fmt.Errorf("%w: plain Continue refuses a failed batch; use the separately labelled skip failed batch action", ErrDeploymentContinueRefused)
	}
	if policy.SkipFailedBatch {
		if !failedBatch {
			return Deployment{}, nil, fmt.Errorf("%w: skip failed batch requires a failure terminal on the opened batch", ErrDeploymentContinueRefused)
		}
		if strings.TrimSpace(policy.SkipReason) == "" {
			return Deployment{}, nil, fmt.Errorf("%w: skip failed batch requires a reason", ErrDeploymentContinueRefused)
		}
		if opened >= total {
			return Deployment{}, nil, fmt.Errorf("%w: skip failed batch does not finish a deployment; abandon stops without opening more jobs", ErrDeploymentContinueRefused)
		}
	}
	d.PausedAt = nil
	var jobs []Job
	if opened >= total {
		d.State = DeploymentFinished
		// This timestamp can later become the canary business-day anchor.  Take it
		// from the Store clock under the same writer transaction, never from the
		// caller that requested Continue.
		finished := s.now().UTC().Truncate(time.Second)
		d.FinishedAt = &finished
		var res sql.Result
		res, err = tx.Exec(`UPDATE deployments
 SET state=?,paused_at=NULL,finished_at=?,control_revision=control_revision+1
 WHERE deployment_id=? AND state=? AND control_revision=?`,
			d.State, fmtTime(finished), id, DeploymentPaused, d.ControlRevision)
		if err == nil {
			if changed, _ := res.RowsAffected(); changed != 1 {
				return Deployment{}, nil, fmt.Errorf("%w: deployment %s control revision changed while continuing", ErrDeploymentNotPaused, id)
			}
			err = recordDeploymentSoakBoundaryTx(tx, d.DeploymentID)
		}
	} else {
		if err := validateDeploymentJobMaterialGraph(tx, d.DeploymentID); err != nil {
			return Deployment{}, nil, err
		}
		activeResourceJobs, scanErr := deploymentResourceHasNonTerminalJobsTx(tx, d.ResourceKind, d.ResourceID)
		if scanErr != nil {
			return Deployment{}, nil, scanErr
		}
		if activeResourceJobs {
			return Deployment{}, nil, fmt.Errorf("%w: %s:%s has nonterminal jobs owned by another deployment",
				ErrDeploymentActiveResource, d.ResourceKind, d.ResourceID)
		}
		nextBatch := opened + 1
		status, statusErr := deploymentBatchStatusTx(tx, id, nextBatch)
		if statusErr != nil {
			return Deployment{}, nil, statusErr
		}
		if status.targets == 0 || status.opened != 0 {
			return Deployment{}, nil, fmt.Errorf("%w: paused deployment %s next batch %d is not in an unopened state",
				ErrDeploymentBatchNotReady, id, nextBatch)
		}
		var n NewJob
		n, err = deploymentJobTemplateTx(tx, id)
		if err == nil {
			err = s.gateStableBatchTx(d, n)
		}
		if err != nil {
			return Deployment{}, nil, err
		}
		d.State = DeploymentRunning
		var res sql.Result
		res, err = tx.Exec(`UPDATE deployments
 SET state=?,paused_at=NULL,control_revision=control_revision+1
 WHERE deployment_id=? AND state=? AND control_revision=?`,
			d.State, id, DeploymentPaused, d.ControlRevision)
		if err == nil {
			if changed, _ := res.RowsAffected(); changed != 1 {
				return Deployment{}, nil, fmt.Errorf("%w: deployment %s control revision changed while continuing", ErrDeploymentNotPaused, id)
			}
			jobs, err = createDeploymentBatchTx(tx, d, nextBatch, n, now.UTC())
		}
	}
	if err != nil {
		return Deployment{}, nil, err
	}
	if _, err := tx.Exec(`DELETE FROM deployment_boundary_pauses WHERE deployment_id=?`, id); err != nil {
		return Deployment{}, nil, fmt.Errorf("store: clear deployment boundary pause: %w", err)
	}
	d.ControlRevision++
	kind := HubDeploymentContinued
	detail := fmt.Sprintf("deployment %s Continue next batch；開了 %d 張單", id, len(jobs))
	if policy.SkipFailedBatch {
		kind = HubDeploymentSkippedFailedBatch
		detail = fmt.Sprintf("deployment %s skip failed batch；reason=%s；開了 %d 張單；失敗的工作單不重開", id, policy.SkipReason, len(jobs))
	}
	eventAt := now.UTC()
	if d.FinishedAt != nil {
		eventAt = d.FinishedAt.UTC()
	}
	if _, err := tx.Exec(`INSERT INTO hub_events (at,kind,detail) VALUES (?,?,?)`,
		fmtTime(eventAt), kind, detail); err != nil {
		return Deployment{}, nil, fmt.Errorf("store: record deployment continue event: %w", err)
	}
	return d, jobs, nil
}

// recordDeploymentSoakBoundaryTx ties a deployment's finished transition to
// the append order of workload evidence under the same SQLite writer lock.
// Wall-clock timestamps alone cannot distinguish evidence appended before a
// canary from evidence received afterwards when the Hub clock moves backward.
func recordDeploymentSoakBoundaryTx(tx dbTx, deploymentID string) error {
	var evidenceID int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(evidence_id),0) FROM workload_observation_evidence`).Scan(&evidenceID); err != nil {
		return fmt.Errorf("store: capture deployment soak evidence boundary: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO deployment_soak_boundaries(deployment_id,evidence_id)
	 VALUES(?,?) ON CONFLICT(deployment_id) DO NOTHING`, deploymentID, evidenceID); err != nil {
		return fmt.Errorf("store: record deployment soak evidence boundary: %w", err)
	}
	return nil
}
