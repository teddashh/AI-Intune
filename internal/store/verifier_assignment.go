package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	OperatorCodeVerificationAssignmentJobNotFound    = "VERIFICATION_ASSIGNMENT_JOB_NOT_FOUND"
	OperatorCodeVerificationAssignmentDomainConflict = "VERIFICATION_ASSIGNMENT_DOMAIN_CONFLICT"
	OperatorCodeVerificationAssignmentPreviewStale   = "VERIFICATION_ASSIGNMENT_PREVIEW_STALE"
)

const (
	operatorVerificationAssignmentVersion   = "v2"
	operatorVerificationAssignmentOperation = "verifier-assignment:v1"

	operatorVerificationAssignmentCacheInvalidDetail      = "operator verification assignment idempotency cache invalid；未回放結果"
	operatorVerificationAssignmentRejectionEvidencePrefix = "operator verification assignment rejection code="

	// VerificationAssignmentSatisfiedBy is the whole definition of "done" for an
	// assignment. It is part of the preview digest so that changing what counts
	// as done invalidates every preview taken under the old rule.
	VerificationAssignmentSatisfiedBy = "complete_independent_report_received_at_or_after_assignment"
)

// operatorVerificationAssignmentPolicy is the rendered impact of handing one
// job to one verifier. CommandsSuppliedByHub is false and is stated explicitly:
// an assignment names a job, never a command. A verifier that ran Hub-supplied
// commands would be a remote execution channel gated by one bearer, not a
// second independent judgement.
type operatorVerificationAssignmentPolicy struct {
	SeparationRule             string `json:"separation_rule"`
	SatisfiedBy                string `json:"satisfied_by"`
	HandoutRequiresTerminalJob bool   `json:"handout_requires_terminal_job"`
	CommandsSuppliedByHub      bool   `json:"commands_supplied_by_hub"`
	GrantsDeploymentGate       bool   `json:"grants_deployment_gate"`
}

func currentOperatorVerificationAssignmentPolicy(verifierKind string) operatorVerificationAssignmentPolicy {
	return operatorVerificationAssignmentPolicy{
		SeparationRule:             OperatorVerifierSeparationRule,
		SatisfiedBy:                VerificationAssignmentSatisfiedBy,
		HandoutRequiresTerminalJob: true,
		CommandsSuppliedByHub:      false,
		GrantsDeploymentGate:       VerifierKindGrantsDeploymentGate(verifierKind),
	}
}

// VerificationAssignment is one operator instruction: this verifier should look
// at this job.
//
// ⚠ There is no state column behind ReportedAt. An assignment is satisfied when
// evidence exists, not when a runner claims to have finished, so ReportedAt is
// derived from verification_results on every read. Nothing a verifier says can
// mark its own assignment done without also leaving the evidence.
type VerificationAssignment struct {
	AssignmentID    string     `json:"assignment_id"`
	JobID           string     `json:"job_id"`
	MachineID       string     `json:"machine_id"`
	MachineName     string     `json:"machine_name"`
	VerifierID      string     `json:"verifier_id"`
	VerifierName    string     `json:"verifier_name"`
	VerifierDomain  string     `json:"verifier_failure_domain"`
	VerifierRevoked bool       `json:"verifier_revoked"`
	AssignedAt      time.Time  `json:"assigned_at"`
	AssignedBy      string     `json:"assigned_by"`
	JobTerminalAt   *time.Time `json:"job_terminal_at,omitempty"`
	ReportedAt      *time.Time `json:"reported_at,omitempty"`
}

// Pending answers the one question the runner asks. A job that has not reached
// a terminal state is deliberately not pending: evidence produced before the
// executor finished is stale by construction (§ EvaluateIndependentVerdict),
// so handing it out would only manufacture stale rows.
func (a VerificationAssignment) Pending() bool {
	return a.ReportedAt == nil && a.JobTerminalAt != nil
}

// A fleet-peer assignment remains pending until all three gate-rule identities
// have arrived after this exact assignment. This keeps a partial sequence of
// per-row POSTs retryable. Other verifier kinds do not participate in the gate
// and retain the generic one-row completion rule.
var verificationAssignmentReportedAt = fmt.Sprintf(`(CASE WHEN v.kind='%s' THEN max(
    (SELECT MIN(r.received_at) FROM verification_results AS r
      WHERE r.job_id=a.job_id AND r.verifier_id=a.verifier_id
        AND r.evidence_role='%s' AND r.received_at>=a.assigned_at AND r.rule_id='%s'),
    (SELECT MIN(r.received_at) FROM verification_results AS r
      WHERE r.job_id=a.job_id AND r.verifier_id=a.verifier_id
        AND r.evidence_role='%s' AND r.received_at>=a.assigned_at AND r.rule_id='%s'),
    (SELECT MIN(r.received_at) FROM verification_results AS r
      WHERE r.job_id=a.job_id AND r.verifier_id=a.verifier_id
        AND r.evidence_role='%s' AND r.received_at>=a.assigned_at AND r.rule_id='%s')
  ) ELSE (SELECT MIN(r.received_at) FROM verification_results AS r
    WHERE r.job_id=a.job_id AND r.verifier_id=a.verifier_id
      AND r.evidence_role='%s' AND r.received_at>=a.assigned_at) END)`,
	VerifierKindFleetPeerAgent,
	JobVerificationRoleIndependent, model.IndependentRuleOpenClawCurrentRelease,
	JobVerificationRoleIndependent, model.IndependentRuleOpenClawGatewayHTTP,
	JobVerificationRoleIndependent, model.IndependentRuleOpenClawUnitState,
	JobVerificationRoleIndependent)

// PendingVerificationAssignments is the verifier plane's only read. It is
// scoped by the caller's own credential and returns nothing that was not
// explicitly assigned to it: a stolen verifier bearer can enumerate what the
// operator already decided to hand it, and nothing else.
//
// ⚠ Never widen this to "every job this verifier would be eligible for". The
// separation rule alone admits the whole fleet minus one machine, which is the
// enumeration this table exists to avoid.
func (s *Store) PendingVerificationAssignments(verifierID string) ([]VerificationAssignment, error) {
	rows, err := s.db.Query(`
SELECT a.assignment_id,a.job_id,j.machine_id,COALESCE(m.display_name,''),
       a.verifier_id,v.display_name,v.failure_domain,
       CASE WHEN v.revoked_at IS NULL THEN 0 ELSE 1 END,
       a.assigned_at,a.assigned_by,j.terminal_at
  FROM verification_assignments AS a
  JOIN jobs AS j ON j.job_id=a.job_id
  JOIN verifiers AS v ON v.verifier_id=a.verifier_id
  LEFT JOIN machine_registry AS m ON m.machine_id=j.machine_id
 WHERE a.verifier_id=?
   AND v.revoked_at IS NULL
   AND v.failure_domain<>j.machine_id
   AND j.terminal_at IS NOT NULL AND j.terminal_at<>''
   AND `+verificationAssignmentReportedAt+` IS NULL
 ORDER BY a.assigned_at, a.assignment_id`, verifierID)
	if err != nil {
		return nil, fmt.Errorf("store: read pending verification assignments: %w", err)
	}
	defer rows.Close()
	return scanVerificationAssignments(rows, nil)
}

// JobVerificationAssignments answers "who was asked to look at this job", which
// is what makes an absent verdict legible: nobody was asked, the job has not
// finished yet, or somebody was asked and has not reported.
func (s *Store) JobVerificationAssignments(jobID string) ([]VerificationAssignment, error) {
	rows, err := s.db.Query(`
SELECT a.assignment_id,a.job_id,j.machine_id,COALESCE(m.display_name,''),
       a.verifier_id,v.display_name,v.failure_domain,
       CASE WHEN v.revoked_at IS NULL THEN 0 ELSE 1 END,
       a.assigned_at,a.assigned_by,j.terminal_at,
       `+verificationAssignmentReportedAt+`
  FROM verification_assignments AS a
  JOIN jobs AS j ON j.job_id=a.job_id
  JOIN verifiers AS v ON v.verifier_id=a.verifier_id
  LEFT JOIN machine_registry AS m ON m.machine_id=j.machine_id
 WHERE a.job_id=?
 ORDER BY a.assigned_at, a.assignment_id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: read job verification assignments: %w", err)
	}
	defer rows.Close()
	reported := new(sql.NullString)
	return scanVerificationAssignments(rows, reported)
}

func scanVerificationAssignments(rows *sql.Rows, reported *sql.NullString) ([]VerificationAssignment, error) {
	assignments := []VerificationAssignment{}
	for rows.Next() {
		var a VerificationAssignment
		var assignedAt string
		var terminalAt sql.NullString
		targets := []any{&a.AssignmentID, &a.JobID, &a.MachineID, &a.MachineName,
			&a.VerifierID, &a.VerifierName, &a.VerifierDomain, &a.VerifierRevoked,
			&assignedAt, &a.AssignedBy, &terminalAt}
		if reported != nil {
			targets = append(targets, reported)
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("store: scan verification assignment: %w", err)
		}
		a.AssignedAt = parseTime(assignedAt)
		a.JobTerminalAt = parseTimePtr(terminalAt)
		if reported != nil {
			a.ReportedAt = parseTimePtr(*reported)
		}
		assignments = append(assignments, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: finish verification assignments: %w", err)
	}
	return assignments, nil
}

// OperatorVerificationAssignmentPreviewResult carries the two identities and
// the one rule that decides eligibility. It is safe to render.
type OperatorVerificationAssignmentPreviewResult struct {
	VerifierID    string     `json:"verifier_id"`
	VerifierName  string     `json:"verifier_name"`
	VerifierKind  string     `json:"verifier_kind"`
	FailureDomain string     `json:"failure_domain"`
	JobID         string     `json:"job_id"`
	MachineID     string     `json:"machine_id"`
	MachineName   string     `json:"machine_name"`
	JobState      string     `json:"job_state"`
	JobTerminalAt *time.Time `json:"job_terminal_at,omitempty"`
	PreviewedAt   time.Time  `json:"previewed_at"`

	SeparationRule             string `json:"separation_rule"`
	SatisfiedBy                string `json:"satisfied_by"`
	HandoutRequiresTerminalJob bool   `json:"handout_requires_terminal_job"`
	CommandsSuppliedByHub      bool   `json:"commands_supplied_by_hub"`
	GrantsDeploymentGate       bool   `json:"grants_deployment_gate"`
	PreviewDigest              string `json:"preview_digest"`
}

type OperatorVerificationAssignmentRequest struct {
	VerifierID string
	JobID      string
	// ConfirmVerifierName is the operator retyping the verifier's display name.
	// It is compared against the registry inside this transaction, so a caller
	// that echoed its own earlier reading of the name cannot satisfy it.
	ConfirmVerifierName string
	PreviewDigest       string
	Reason              string
	AssignedBy          string
	IdempotencyKey      string
	RequestDigest       string
	Audit               AuditEntry
}

type OperatorVerificationAssignmentResult struct {
	AssignmentID  string    `json:"assignment_id"`
	VerifierID    string    `json:"verifier_id"`
	VerifierName  string    `json:"verifier_name"`
	JobID         string    `json:"job_id"`
	MachineID     string    `json:"machine_id"`
	AssignedAt    time.Time `json:"assigned_at"`
	AssignedBy    string    `json:"assigned_by"`
	PreviewDigest string    `json:"preview_digest"`
	Replayed      bool      `json:"replayed"`
	Audited       bool      `json:"-"`
}

// operatorVerificationAssignmentReceipt is the only success representation
// allowed in operator_idempotency.response_json.
type operatorVerificationAssignmentReceipt struct {
	AssignmentID  string    `json:"assignment_id"`
	VerifierID    string    `json:"verifier_id"`
	VerifierName  string    `json:"verifier_name"`
	JobID         string    `json:"job_id"`
	MachineID     string    `json:"machine_id"`
	AssignedAt    time.Time `json:"assigned_at"`
	AssignedBy    string    `json:"assigned_by"`
	PreviewDigest string    `json:"preview_digest"`
}

type verificationAssignmentIntent struct {
	VerifierID    string
	VerifierName  string
	VerifierKind  string
	FailureDomain string
	JobID         string
	MachineID     string
	MachineName   string
	JobState      string
	JobTerminalAt *time.Time
}

// PreviewOperatorVerificationAssignment performs no write and consumes no
// idempotency key.
func (s *Store) PreviewOperatorVerificationAssignment(verifierID, jobID string) (
	OperatorVerificationAssignmentPreviewResult, error,
) {
	intent, err := resolveVerificationAssignmentIntent(s.db, verifierID, jobID)
	if err != nil {
		return OperatorVerificationAssignmentPreviewResult{}, err
	}
	policy := currentOperatorVerificationAssignmentPolicy(intent.VerifierKind)
	return OperatorVerificationAssignmentPreviewResult{
		VerifierID: intent.VerifierID, VerifierName: intent.VerifierName,
		VerifierKind: intent.VerifierKind, FailureDomain: intent.FailureDomain,
		JobID: intent.JobID, MachineID: intent.MachineID, MachineName: intent.MachineName,
		JobState: intent.JobState, JobTerminalAt: intent.JobTerminalAt,
		PreviewedAt:                s.now().UTC(),
		SeparationRule:             policy.SeparationRule,
		SatisfiedBy:                policy.SatisfiedBy,
		HandoutRequiresTerminalJob: policy.HandoutRequiresTerminalJob,
		CommandsSuppliedByHub:      policy.CommandsSuppliedByHub,
		GrantsDeploymentGate:       policy.GrantsDeploymentGate,
		PreviewDigest:              operatorVerificationAssignmentPreviewDigest(intent, policy),
	}, nil
}

// operatorVerificationAssignmentPreviewDigest covers the immutable facts that
// decide eligibility plus the policy. Job state is deliberately excluded: a job
// finishing between preview and apply is the expected case, not a stale
// preview, and the assignment is legitimate either way.
func operatorVerificationAssignmentPreviewDigest(intent verificationAssignmentIntent,
	policy operatorVerificationAssignmentPolicy,
) string {
	body := struct {
		Version       string                               `json:"version"`
		VerifierID    string                               `json:"verifier_id"`
		FailureDomain string                               `json:"failure_domain"`
		JobID         string                               `json:"job_id"`
		MachineID     string                               `json:"machine_id"`
		Policy        operatorVerificationAssignmentPolicy `json:"policy"`
	}{operatorVerificationAssignmentVersion, intent.VerifierID, intent.FailureDomain,
		intent.JobID, intent.MachineID, policy}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// resolveVerificationAssignmentIntent runs the same checks the apply
// transaction runs, against the same reader, so preview and apply cannot drift.
func resolveVerificationAssignmentIntent(q verifierQueryRower, verifierID, jobID string) (
	verificationAssignmentIntent, error,
) {
	var intent verificationAssignmentIntent
	var revokedAt sql.NullString
	err := q.QueryRow(`SELECT verifier_id,kind,display_name,failure_domain,revoked_at
 FROM verifiers WHERE verifier_id=?`, verifierID).Scan(&intent.VerifierID, &intent.VerifierKind,
		&intent.VerifierName, &intent.FailureDomain, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return intent, operatorVerificationAssignmentRejection(OperatorCodeVerifierNotFound)
	}
	if err != nil {
		return intent, fmt.Errorf("store: resolve assignment verifier: %w", err)
	}
	if revokedAt.Valid && revokedAt.String != "" {
		return intent, operatorVerificationAssignmentRejection(OperatorCodeVerifierAlreadyRevoked)
	}

	var terminalAt sql.NullString
	err = q.QueryRow(`SELECT j.job_id,j.machine_id,COALESCE(m.display_name,''),j.state,j.terminal_at
 FROM jobs AS j LEFT JOIN machine_registry AS m ON m.machine_id=j.machine_id
 WHERE j.job_id=?`, jobID).Scan(&intent.JobID, &intent.MachineID, &intent.MachineName,
		&intent.JobState, &terminalAt)
	if errors.Is(err, sql.ErrNoRows) {
		return intent, operatorVerificationAssignmentRejection(OperatorCodeVerificationAssignmentJobNotFound)
	}
	if err != nil {
		return intent, fmt.Errorf("store: resolve assignment job: %w", err)
	}
	intent.JobTerminalAt = parseTimePtr(terminalAt)

	// The same one comparison the evidence write enforces. Checking it here
	// turns a write that would always be refused into an operator-visible
	// rejection, and keeps the rule expressed exactly once in words.
	if intent.FailureDomain == intent.MachineID {
		return intent, operatorVerificationAssignmentRejection(OperatorCodeVerificationAssignmentDomainConflict)
	}
	return intent, nil
}

func operatorVerificationAssignmentRejection(code string) *OperatorRequestError {
	detail, ok := canonicalOperatorVerificationAssignmentRejectionDetail(code)
	if !ok {
		return operatorError(code, "控制面 verification assignment request 被拒絕")
	}
	return operatorError(code, detail)
}

func canonicalOperatorVerificationAssignmentRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeVerifierNotFound:
		return "找不到這個 verifier", true
	case OperatorCodeVerifierAlreadyRevoked:
		return "這個 verifier 已經撤銷，不能再指派工作單", true
	case OperatorCodeVerificationAssignmentJobNotFound:
		return "找不到這張工作單", true
	case OperatorCodeVerificationAssignmentDomainConflict:
		return "這個 verifier 的 failure domain 就是這張工作單的機器；換一個 failure domain 不同的 verifier", true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodeVerificationAssignmentPreviewStale:
		return "preview_digest 與目前的 assignment policy 不符；請重新預覽", true
	case OperatorCodeConfirmationMismatch:
		return "確認欄位打的名稱不是這個 verifier 的名稱", true
	default:
		return "", false
	}
}

// historicalOperatorVerificationAssignmentRejectionDetail says what class of
// decision was committed then. It must not rewrite that history with today's
// wording.
func historicalOperatorVerificationAssignmentRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeVerifierNotFound:
		return "原 request 指定的 verifier 當時不存在", true
	case OperatorCodeVerifierAlreadyRevoked:
		return "原 request 指定的 verifier 當時已撤銷", true
	case OperatorCodeVerificationAssignmentJobNotFound:
		return "原 request 指定的工作單當時不存在", true
	case OperatorCodeVerificationAssignmentDomainConflict:
		return "原 request 的 verifier 與工作單當時在同一個 failure domain", true
	case OperatorCodePreviewRequired:
		return "原 request 當時缺少必要的 preview_digest", true
	case OperatorCodeVerificationAssignmentPreviewStale:
		return "原 request 的 preview_digest 不符合當時的 assignment policy", true
	case OperatorCodeConfirmationMismatch:
		return "原 request 的確認欄位當時打的不是該 verifier 的名稱", true
	default:
		return "", false
	}
}

func operatorVerificationAssignmentStoredRejectionDetail(code, originalDetail string) string {
	return truncAudit(operatorVerificationAssignmentRejectionEvidencePrefix+code+"；"+originalDetail, auditMaxReason)
}

func operatorVerificationAssignmentSuccessDetail(receipt operatorVerificationAssignmentReceipt) string {
	return fmt.Sprintf("assignment_id=%s，verifier_id=%s，job_id=%s，machine_id=%s，satisfied_by=%s",
		receipt.AssignmentID, receipt.VerifierID, receipt.JobID, receipt.MachineID,
		VerificationAssignmentSatisfiedBy)
}

// ApplyOperatorVerificationAssignment is the single transactional authority for
// creating an assignment. The row, the replay receipt and the audit evidence
// either commit together or not at all.
func (s *Store) ApplyOperatorVerificationAssignment(req OperatorVerificationAssignmentRequest) (
	OperatorVerificationAssignmentResult, error,
) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorVerificationAssignmentResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorVerificationAssignmentResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}
	if !validCatalogPublisher(req.AssignedBy) {
		return OperatorVerificationAssignmentResult{}, operatorError(
			OperatorCodeVerificationAssignmentJobNotFound, "assigned_by 不可為空")
	}

	audit := req.Audit
	audit.Action = AuditVerificationAssign
	audit.Subject = req.JobID
	audit.Reason = req.Reason
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf("store: begin verification assignment: %w", err)
	}
	defer tx.Rollback()
	writerNow := s.now().UTC().Truncate(time.Second)
	audit.At = writerNow

	var cached operatorCachedRequest
	err = tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
	 FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	switch {
	case err == nil:
		return s.replayOperatorVerificationAssignment(tx, req, audit, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: inspect verification assignment idempotency key: %w", err)
	}

	reject := func(code string) (OperatorVerificationAssignmentResult, error) {
		detail, ok := canonicalOperatorVerificationAssignmentRejectionDetail(code)
		if !ok {
			return OperatorVerificationAssignmentResult{}, errors.New(
				"store: invalid verification assignment rejection code")
		}
		storedDetail := operatorVerificationAssignmentStoredRejectionDetail(code, detail)
		audit.OK, audit.Detail = false, storedDetail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorVerificationAssignmentResult{}, fmt.Errorf(
				"store: record verification assignment rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorVerificationAssignmentOperation,
			req.RequestDigest, code, storedDetail, fmtTime(writerNow)); err != nil {
			return OperatorVerificationAssignmentResult{}, fmt.Errorf(
				"store: persist verification assignment rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorVerificationAssignmentResult{}, fmt.Errorf(
				"store: commit verification assignment rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorVerificationAssignmentResult{}, rejection
	}

	intent, intentErr := resolveVerificationAssignmentIntent(tx, req.VerifierID, req.JobID)
	if intentErr != nil {
		var rejection *OperatorRequestError
		if !errors.As(intentErr, &rejection) {
			return OperatorVerificationAssignmentResult{}, intentErr
		}
		return reject(rejection.Code)
	}
	if strings.TrimSpace(req.PreviewDigest) == "" {
		return reject(OperatorCodePreviewRequired)
	}
	expected := operatorVerificationAssignmentPreviewDigest(intent,
		currentOperatorVerificationAssignmentPolicy(intent.VerifierKind))
	if req.PreviewDigest != expected {
		return reject(OperatorCodeVerificationAssignmentPreviewStale)
	}
	if req.ConfirmVerifierName != intent.VerifierName {
		return reject(OperatorCodeConfirmationMismatch)
	}
	if !validJobEvidenceTime(writerNow) {
		return OperatorVerificationAssignmentResult{}, errors.New(
			"store: verification assignment clock is out of range")
	}

	receipt := operatorVerificationAssignmentReceipt{
		AssignmentID: newID(), VerifierID: intent.VerifierID, VerifierName: intent.VerifierName,
		JobID:     intent.JobID,
		MachineID: intent.MachineID, AssignedAt: writerNow, AssignedBy: req.AssignedBy,
		PreviewDigest: req.PreviewDigest,
	}
	if _, err := tx.Exec(`INSERT INTO verification_assignments
	 (assignment_id,job_id,verifier_id,assigned_at,assigned_by)
	 VALUES (?,?,?,?,?)`, receipt.AssignmentID, receipt.JobID, receipt.VerifierID,
		fmtTime(receipt.AssignedAt), receipt.AssignedBy); err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: insert verification assignment: %w", err)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: encode verification assignment receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorVerificationAssignmentOperation,
		req.RequestDigest, string(raw), fmtTime(writerNow)); err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: persist verification assignment receipt: %w", err)
	}
	audit.OK = true
	audit.MachineID = receipt.MachineID
	audit.Detail = operatorVerificationAssignmentSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: record verification assignment audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: commit verification assignment: %w", err)
	}
	return operatorVerificationAssignmentResultFrom(receipt, false), nil
}

// operatorVerificationAssignmentResultFrom is only reached after this writer
// has already recorded the audit row inside the transaction, so Audited is part
// of the shape rather than something each caller remembers. Forgetting it on
// one path is not a missing row — it is a second, duplicate row written by the
// service-level fallback, which is what the live ledger showed for every
// successful assignment written before 2026-09-12.
func operatorVerificationAssignmentResultFrom(receipt operatorVerificationAssignmentReceipt,
	replayed bool,
) OperatorVerificationAssignmentResult {
	return OperatorVerificationAssignmentResult{
		AssignmentID: receipt.AssignmentID, VerifierID: receipt.VerifierID,
		VerifierName: receipt.VerifierName, JobID: receipt.JobID,
		MachineID: receipt.MachineID, AssignedAt: receipt.AssignedAt,
		AssignedBy: receipt.AssignedBy, PreviewDigest: receipt.PreviewDigest,
		Replayed: replayed, Audited: true,
	}
}

func (s *Store) replayOperatorVerificationAssignment(tx *sql.Tx,
	req OperatorVerificationAssignmentRequest, audit AuditEntry, cached operatorCachedRequest,
) (OperatorVerificationAssignmentResult, error) {
	if cached.Operation != operatorVerificationAssignmentOperation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK, audit.Detail = false, "idempotency conflict："+detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorVerificationAssignmentResult{}, fmt.Errorf(
				"store: record verification assignment idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorVerificationAssignmentResult{}, fmt.Errorf(
				"store: commit verification assignment idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorVerificationAssignmentResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		historical, allowed := historicalOperatorVerificationAssignmentRejectionDetail(cached.ErrorCode.String)
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			!allowed || !strings.HasPrefix(cached.ErrorDetail.String,
			operatorVerificationAssignmentRejectionEvidencePrefix+cached.ErrorCode.String+"；") ||
			!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
			return s.rejectInvalidVerificationAssignmentCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorVerificationAssignmentResult{}, fmt.Errorf(
				"store: record rejected verification assignment replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorVerificationAssignmentResult{}, fmt.Errorf(
				"store: commit rejected verification assignment replay audit: %w", err)
		}
		return OperatorVerificationAssignmentResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid ||
		cached.ErrorDetail.Valid || !canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return s.rejectInvalidVerificationAssignmentCache(tx, audit)
	}
	var receipt operatorVerificationAssignmentReceipt
	decoder := json.NewDecoder(strings.NewReader(cached.ResponseJSON.String))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil || decoder.More() {
		return s.rejectInvalidVerificationAssignmentCache(tx, audit)
	}
	if receipt.VerifierID != req.VerifierID || receipt.JobID != req.JobID ||
		receipt.AssignmentID == "" || receipt.PreviewDigest == "" ||
		receipt.VerifierName == "" || receipt.VerifierName != req.ConfirmVerifierName ||
		fmtTime(receipt.AssignedAt) != cached.CreatedAt {
		return s.rejectInvalidVerificationAssignmentCache(tx, audit)
	}
	// The cached receipt only means "this decision was made". The row is what
	// makes it true, so the replay refuses to confirm an assignment that is not
	// in the ledger.
	valid, err := verificationAssignmentReceiptMatchesRow(tx, receipt)
	if err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: inspect cached verification assignment evidence: %w", err)
	}
	if !valid {
		return s.rejectInvalidVerificationAssignmentCache(tx, audit)
	}
	audit.OK = true
	audit.MachineID = receipt.MachineID
	audit.Detail = OperatorIdempotencyReplayPrefix + operatorVerificationAssignmentSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: record verification assignment replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: commit verification assignment replay audit: %w", err)
	}
	return operatorVerificationAssignmentResultFrom(receipt, true), nil
}

func verificationAssignmentReceiptMatchesRow(tx *sql.Tx,
	receipt operatorVerificationAssignmentReceipt,
) (bool, error) {
	var jobID, verifierID, assignedAt, assignedBy string
	err := tx.QueryRow(`SELECT job_id,verifier_id,assigned_at,assigned_by
 FROM verification_assignments WHERE assignment_id=?`, receipt.AssignmentID).Scan(
		&jobID, &verifierID, &assignedAt, &assignedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return jobID == receipt.JobID && verifierID == receipt.VerifierID &&
		assignedAt == fmtTime(receipt.AssignedAt) && assignedBy == receipt.AssignedBy, nil
}

func (s *Store) rejectInvalidVerificationAssignmentCache(tx *sql.Tx, audit AuditEntry) (
	OperatorVerificationAssignmentResult, error,
) {
	audit.OK = false
	audit.Detail = operatorVerificationAssignmentCacheInvalidDetail
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: record invalid verification assignment cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorVerificationAssignmentResult{}, fmt.Errorf(
			"store: commit invalid verification assignment cache audit: %w", err)
	}
	rejection := operatorError(OperatorCodeIdempotencyConflict,
		operatorVerificationAssignmentCacheInvalidDetail)
	rejection.Audited = true
	return OperatorVerificationAssignmentResult{}, rejection
}
