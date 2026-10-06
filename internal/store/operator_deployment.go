package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	OperatorCodeDeploymentPreviewStale         = "DEPLOYMENT_PREVIEW_STALE"
	OperatorCodeDeploymentPreconditionFailed   = "DEPLOYMENT_PRECONDITION_FAILED"
	OperatorCodeDeploymentContinueRefused      = "DEPLOYMENT_CONTINUE_REFUSED"
	OperatorCodeDeploymentNotFound             = "DEPLOYMENT_NOT_FOUND"
	OperatorCodeDeploymentNotPaused            = "DEPLOYMENT_NOT_PAUSED"
	OperatorCodeDeploymentActiveJobs           = "DEPLOYMENT_ACTIVE_JOBS"
	OperatorCodeDeploymentNoRetryTargets       = "DEPLOYMENT_NO_RETRY_TARGETS"
	OperatorCodeDeploymentNoIncludedTargets    = "DEPLOYMENT_NO_INCLUDED_TARGETS"
	OperatorCodeDeploymentPromotionBlocked     = "DEPLOYMENT_PROMOTION_BLOCKED"
	OperatorCodeDeploymentConfirmationMismatch = "DEPLOYMENT_CONFIRMATION_MISMATCH"
)

var (
	ErrDeploymentPreviewStale         = errors.New("store: deployment preview is stale")
	ErrDeploymentPreconditionFailed   = errors.New("store: deployment control precondition failed")
	ErrDeploymentActiveJobs           = errors.New("store: deployment has active jobs")
	ErrDeploymentNoRetryTargets       = errors.New("store: deployment has no terminal-failure retry targets")
	ErrDeploymentNoIncludedTargets    = errors.New("store: deployment plan includes no machines")
	ErrDeploymentConfirmationMismatch = errors.New("store: deployment confirmation does not match")
)

const (
	operatorDeploymentReceiptVersion  = "v1"
	operatorDeploymentCreateOperation = "deployment-create:v1"
	operatorDeploymentRejectPrefix    = "operator deployment rejection code="
	operatorDeploymentCacheInvalid    = "operator deployment idempotency cache invalid；未回放結果"
)

type operatorDeploymentAction string

const (
	operatorDeploymentCreate          operatorDeploymentAction = "create"
	operatorDeploymentContinue        operatorDeploymentAction = "continue"
	operatorDeploymentSkipFailedBatch operatorDeploymentAction = "skip_failed_batch"
	operatorDeploymentRetry           operatorDeploymentAction = "retry"
	operatorDeploymentAbandon         operatorDeploymentAction = "abandon"
)

// OperatorDeploymentRequest is the canonical Store-side control envelope.
// ExpectedControlRevision and ExpectedOpenedBatch are required for operations
// against an existing deployment. Typed confirmations are retained here as
// independent cached-success evidence as well as being bound into RequestDigest.
type OperatorDeploymentRequest struct {
	DeploymentID            string
	PreviewDigest           string
	ExpectedControlRevision *int64
	ExpectedOpenedBatch     *int
	ConfirmChannel          string
	ConfirmVersion          string
	ConfirmDeploymentID     string
	IdempotencyKey          string
	RequestDigest           string
	Audit                   AuditEntry
}

// OperatorDeploymentPrepared is intentionally supplied lazily. Apply methods
// inspect the historical idempotency ledger before invoking Prepare, so replay
// never needs to read today's artifact or deployment state. Create and Retry
// fill Deployment; Retry also supplies the exact terminal-failure identity it
// previewed. Continue and Abandon only need CurrentPreviewDigest.
type OperatorDeploymentPrepared struct {
	Deployment                NewDeployment
	CurrentPreviewDigest      string
	TerminalFailureMachineIDs []string
}

type OperatorDeploymentPrepare func() (OperatorDeploymentPrepared, error)

type OperatorDeploymentResult struct {
	Deployment    Deployment `json:"deployment"`
	Jobs          []Job      `json:"jobs,omitempty"`
	OpenedBatch   int        `json:"opened_batch"`
	PreviewDigest string     `json:"preview_digest"`
	Replayed      bool       `json:"replayed"`
	Audited       bool       `json:"-"`
}

type operatorDeploymentReceipt struct {
	Version       string                   `json:"version"`
	Action        operatorDeploymentAction `json:"action"`
	Deployment    Deployment               `json:"deployment"`
	Jobs          []Job                    `json:"jobs,omitempty"`
	OpenedBatch   int                      `json:"opened_batch"`
	PreviewDigest string                   `json:"preview_digest"`
	AppliedAt     time.Time                `json:"applied_at"`
}

func (s *Store) ApplyOperatorDeploymentCreate(req OperatorDeploymentRequest, prepare OperatorDeploymentPrepare) (OperatorDeploymentResult, error) {
	return s.applyOperatorDeployment(operatorDeploymentCreate, req, prepare)
}

func (s *Store) ApplyOperatorDeploymentContinue(req OperatorDeploymentRequest, prepare OperatorDeploymentPrepare) (OperatorDeploymentResult, error) {
	return s.applyOperatorDeployment(operatorDeploymentContinue, req, prepare)
}

func (s *Store) ApplyOperatorDeploymentSkipFailedBatch(req OperatorDeploymentRequest, prepare OperatorDeploymentPrepare) (OperatorDeploymentResult, error) {
	return s.applyOperatorDeployment(operatorDeploymentSkipFailedBatch, req, prepare)
}

func (s *Store) ApplyOperatorDeploymentRetry(req OperatorDeploymentRequest, prepare OperatorDeploymentPrepare) (OperatorDeploymentResult, error) {
	return s.applyOperatorDeployment(operatorDeploymentRetry, req, prepare)
}

func (s *Store) ApplyOperatorDeploymentAbandon(req OperatorDeploymentRequest, prepare OperatorDeploymentPrepare) (OperatorDeploymentResult, error) {
	return s.applyOperatorDeployment(operatorDeploymentAbandon, req, prepare)
}

func (s *Store) applyOperatorDeployment(action operatorDeploymentAction, req OperatorDeploymentRequest,
	prepare OperatorDeploymentPrepare,
) (OperatorDeploymentResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorDeploymentResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorDeploymentResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}
	operation := operatorDeploymentOperation(action, req.DeploymentID)
	audit := operatorDeploymentAudit(action, req)

	// Phase one reads only historical request identity. A hit is replayed and
	// audited here, before Prepare can touch today's artifact or deployment.
	lookupTx, err := s.db.Begin()
	if err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: begin operator deployment idempotency lookup: %w", err)
	}
	cached, found, err := loadOperatorDeploymentCached(lookupTx, req.IdempotencyKey)
	if err != nil {
		_ = lookupTx.Rollback()
		return OperatorDeploymentResult{}, err
	}
	if found {
		defer lookupTx.Rollback()
		return s.replayOperatorDeployment(lookupTx, action, operation, req, audit, cached)
	}
	if err := lookupTx.Rollback(); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: finish operator deployment idempotency lookup: %w", err)
	}

	// Re-reserve the writer and recheck the global key. This closes the race in
	// which another process commits the same key between the historical lookup
	// and our current-state phase. Prepare then runs under this writer reservation.
	tx, err := s.db.Begin()
	if err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: begin operator deployment apply: %w", err)
	}
	defer tx.Rollback()
	writerNow := s.now().UTC().Truncate(time.Second)
	audit.At = writerNow
	cached, found, err = loadOperatorDeploymentCached(tx, req.IdempotencyKey)
	if err != nil {
		return OperatorDeploymentResult{}, err
	}
	if found {
		return s.replayOperatorDeployment(tx, action, operation, req, audit, cached)
	}

	reject := func(code string) (OperatorDeploymentResult, error) {
		return s.rejectOperatorDeploymentTx(tx, action, operation, req, audit, code, writerNow)
	}
	if !validOperatorDeploymentPreviewDigest(req.PreviewDigest) {
		return reject(OperatorCodePreviewRequired)
	}
	if action != operatorDeploymentCreate {
		if req.ExpectedControlRevision == nil || req.ExpectedOpenedBatch == nil {
			return reject(OperatorCodePreconditionRequired)
		}
		if *req.ExpectedControlRevision < 0 || *req.ExpectedControlRevision >= MaxDeploymentControlRevision ||
			*req.ExpectedOpenedBatch <= 0 {
			return reject(OperatorCodeDeploymentPreconditionFailed)
		}
	}
	if action == operatorDeploymentAbandon && req.ConfirmDeploymentID != req.DeploymentID {
		return reject(OperatorCodeDeploymentConfirmationMismatch)
	}
	if prepare == nil {
		return OperatorDeploymentResult{}, errors.New("store: operator deployment prepare callback is nil")
	}
	prepared, prepareErr := prepare()
	if prepareErr != nil {
		var rejection *OperatorRequestError
		if errors.As(prepareErr, &rejection) && isCanonicalOperatorDeploymentRejection(rejection.Code) {
			return reject(rejection.Code)
		}
		return OperatorDeploymentResult{}, fmt.Errorf("store: prepare operator deployment: %w", prepareErr)
	}
	if prepared.CurrentPreviewDigest == "" || prepared.CurrentPreviewDigest != req.PreviewDigest {
		return reject(OperatorCodeDeploymentPreviewStale)
	}

	if _, err := tx.Exec(`SAVEPOINT operator_deployment_mutation`); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: savepoint operator deployment mutation: %w", err)
	}
	d, jobs, mutationErr := s.mutateOperatorDeploymentTx(tx, action, req, prepared, writerNow)
	if mutationErr != nil {
		code, domain := operatorDeploymentCodeForError(mutationErr)
		if !domain {
			return OperatorDeploymentResult{}, mutationErr
		}
		if _, err := tx.Exec(`ROLLBACK TO operator_deployment_mutation`); err != nil {
			return OperatorDeploymentResult{}, fmt.Errorf("store: rollback rejected operator deployment mutation: %w", err)
		}
		if _, err := tx.Exec(`RELEASE operator_deployment_mutation`); err != nil {
			return OperatorDeploymentResult{}, fmt.Errorf("store: release rejected operator deployment mutation: %w", err)
		}
		return reject(code)
	}
	if _, err := tx.Exec(`RELEASE operator_deployment_mutation`); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: release operator deployment mutation: %w", err)
	}
	opened, err := deploymentOpenedBatchTx(tx, d.DeploymentID)
	if err != nil {
		return OperatorDeploymentResult{}, err
	}
	receipt := operatorDeploymentReceipt{
		Version: operatorDeploymentReceiptVersion, Action: action, Deployment: d,
		Jobs: jobs, OpenedBatch: opened, PreviewDigest: req.PreviewDigest, AppliedAt: writerNow,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: encode operator deployment receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operation, req.RequestDigest,
		string(raw), fmtTime(writerNow)); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: persist operator deployment receipt: %w", err)
	}
	audit.Subject = d.DeploymentID
	audit.OK = true
	audit.Detail = operatorDeploymentSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: record operator deployment success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: commit operator deployment: %w", err)
	}
	result := operatorDeploymentResult(receipt)
	result.Audited = true
	return result, nil
}

func operatorDeploymentOperation(action operatorDeploymentAction, deploymentID string) string {
	if action == operatorDeploymentCreate {
		return operatorDeploymentCreateOperation
	}
	return "deployment-" + string(action) + ":v1:" + deploymentID
}

func operatorDeploymentAudit(action operatorDeploymentAction, req OperatorDeploymentRequest) AuditEntry {
	audit := req.Audit
	switch action {
	case operatorDeploymentCreate:
		audit.Action = AuditDeploymentCreate
	case operatorDeploymentContinue:
		audit.Action = AuditDeploymentContinue
	case operatorDeploymentSkipFailedBatch:
		audit.Action = AuditDeploymentSkipFailedBatch
	case operatorDeploymentRetry:
		audit.Action = AuditDeploymentRetry
	case operatorDeploymentAbandon:
		audit.Action = AuditDeploymentAbandon
	}
	audit.Subject = req.DeploymentID
	if audit.Subject == "" {
		audit.Subject = "deployment create"
	}
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	return audit
}

func loadOperatorDeploymentCached(q operatorRowQuerier, key string) (operatorCachedRequest, bool, error) {
	var cached operatorCachedRequest
	err := q.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
 FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operatorCachedRequest{}, false, nil
	}
	if err != nil {
		return operatorCachedRequest{}, false, fmt.Errorf("store: inspect operator deployment idempotency key: %w", err)
	}
	return cached, true, nil
}

func (s *Store) rejectOperatorDeploymentTx(tx *sql.Tx, action operatorDeploymentAction,
	operation string, req OperatorDeploymentRequest, audit AuditEntry, code string, writerNow time.Time,
) (OperatorDeploymentResult, error) {
	detail, ok := canonicalOperatorDeploymentRejectionDetail(code)
	if !ok {
		return OperatorDeploymentResult{}, errors.New("store: invalid operator deployment rejection code")
	}
	storedDetail := truncAudit(operatorDeploymentRejectPrefix+code+"；"+detail, auditMaxReason)
	audit.OK = false
	audit.Detail = storedDetail
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: record operator deployment rejection audit: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operation, req.RequestDigest,
		code, storedDetail, fmtTime(writerNow)); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: persist operator deployment rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: commit operator deployment rejection: %w", err)
	}
	rejection := operatorError(code, detail)
	rejection.Audited = true
	return OperatorDeploymentResult{}, rejection
}

func (s *Store) replayOperatorDeployment(tx *sql.Tx, action operatorDeploymentAction,
	operation string, req OperatorDeploymentRequest, audit AuditEntry, cached operatorCachedRequest,
) (OperatorDeploymentResult, error) {
	audit.At = s.now().UTC().Truncate(time.Second)
	if cached.Operation != operation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK = false
		audit.Detail = "idempotency conflict：" + detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorDeploymentResult{}, fmt.Errorf("store: record deployment idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorDeploymentResult{}, fmt.Errorf("store: commit deployment idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorDeploymentResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		detail, allowed := historicalOperatorDeploymentRejectionDetail(cached.ErrorCode.String)
		canonical, canonicalOK := canonicalOperatorDeploymentRejectionDetail(cached.ErrorCode.String)
		wantStored := operatorDeploymentRejectPrefix + cached.ErrorCode.String + "；" + canonical
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			!allowed || !canonicalOK || cached.ErrorDetail.String != wantStored ||
			!canonicalOperatorDeploymentCacheTime(cached.CreatedAt) {
			return s.rejectInvalidOperatorDeploymentCache(tx, audit)
		}
		valid, err := validateOperatorDeploymentRejectionEvidence(tx, action, req, cached)
		if err != nil {
			return OperatorDeploymentResult{}, fmt.Errorf("store: inspect cached operator deployment rejection evidence: %w", err)
		}
		if !valid {
			return s.rejectInvalidOperatorDeploymentCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorDeploymentResult{}, fmt.Errorf("store: record rejected deployment replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorDeploymentResult{}, fmt.Errorf("store: commit rejected deployment replay audit: %w", err)
		}
		return OperatorDeploymentResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: detail, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid ||
		cached.ErrorDetail.Valid || !canonicalOperatorDeploymentCacheTime(cached.CreatedAt) {
		return s.rejectInvalidOperatorDeploymentCache(tx, audit)
	}
	receipt, err := decodeOperatorDeploymentReceipt(cached.ResponseJSON.String)
	if err != nil || !validOperatorDeploymentReceipt(receipt, action, req, cached.CreatedAt) {
		return s.rejectInvalidOperatorDeploymentCache(tx, audit)
	}
	valid, err := validateOperatorDeploymentSuccessEvidence(tx, receipt, req)
	if err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: inspect cached operator deployment evidence: %w", err)
	}
	if !valid {
		return s.rejectInvalidOperatorDeploymentCache(tx, audit)
	}
	audit.Subject = receipt.Deployment.DeploymentID
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + operatorDeploymentSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: record successful deployment replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: commit successful deployment replay audit: %w", err)
	}
	result := operatorDeploymentResult(receipt)
	result.Replayed = true
	result.Audited = true
	return result, nil
}

func (s *Store) rejectInvalidOperatorDeploymentCache(tx *sql.Tx, audit AuditEntry) (OperatorDeploymentResult, error) {
	audit.Subject = "deployment idempotency cache"
	audit.Reason = ""
	audit.OK = false
	audit.Detail = operatorDeploymentCacheInvalid
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: record invalid deployment cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorDeploymentResult{}, fmt.Errorf("store: commit invalid deployment cache audit: %w", err)
	}
	return OperatorDeploymentResult{}, errors.New("store: operator deployment idempotency cache is invalid")
}

func decodeOperatorDeploymentReceipt(raw string) (operatorDeploymentReceipt, error) {
	var receipt operatorDeploymentReceipt
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&receipt); err != nil {
		return operatorDeploymentReceipt{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return operatorDeploymentReceipt{}, errors.New("operator deployment receipt has trailing JSON")
	}
	canonical, err := json.Marshal(receipt)
	if err != nil {
		return operatorDeploymentReceipt{}, fmt.Errorf("encode canonical operator deployment receipt: %w", err)
	}
	// Receipts are written only by json.Marshal above. Requiring those exact
	// bytes rejects duplicate/case-aliased fields, null-vs-omitted variants,
	// reordering, and alternate timestamp/number spellings before any cached
	// value can become a successful replay or an audit subject.
	if string(canonical) != raw {
		return operatorDeploymentReceipt{}, errors.New("operator deployment receipt is not canonical JSON")
	}
	return receipt, nil
}

func validOperatorDeploymentReceipt(receipt operatorDeploymentReceipt, action operatorDeploymentAction,
	req OperatorDeploymentRequest, createdAt string,
) bool {
	if action != operatorDeploymentCreate && (req.ExpectedControlRevision == nil ||
		req.ExpectedOpenedBatch == nil || *req.ExpectedControlRevision < 0 ||
		*req.ExpectedControlRevision >= MaxDeploymentControlRevision || *req.ExpectedOpenedBatch < 1) {
		return false
	}
	if action == operatorDeploymentAbandon && req.ConfirmDeploymentID != req.DeploymentID {
		return false
	}
	d := receipt.Deployment
	if receipt.Version != operatorDeploymentReceiptVersion || receipt.Action != action ||
		d.DeploymentID == "" || d.Channel == "" || d.DesiredID == "" ||
		d.ResourceKind == "" || d.ResourceID == "" || d.Revision <= 0 ||
		d.ControlRevision < 0 || d.BatchSize < 1 || d.BatchSize > MaxDeploymentBatchSize ||
		!canonicalOperatorTime(d.CreatedAt) ||
		d.CreatedBy == "" || strings.TrimSpace(d.Spec) == "" ||
		receipt.OpenedBatch < 1 || !validOperatorDeploymentPreviewDigest(req.PreviewDigest) ||
		!validOperatorDeploymentPreviewDigest(receipt.PreviewDigest) ||
		receipt.PreviewDigest != req.PreviewDigest || !canonicalOperatorTime(receipt.AppliedAt) ||
		fmtTime(receipt.AppliedAt) != createdAt || receipt.AppliedAt.Before(d.CreatedAt) ||
		(d.PausedAt != nil && !canonicalOperatorTime(*d.PausedAt)) ||
		(d.FinishedAt != nil && (!canonicalOperatorTime(*d.FinishedAt) ||
			d.FinishedAt.Before(d.CreatedAt) || d.FinishedAt.Before(receipt.AppliedAt))) ||
		(d.PausedAt != nil && (d.PausedAt.Before(d.CreatedAt) ||
			(d.FinishedAt != nil && d.FinishedAt.Before(*d.PausedAt)))) {
		return false
	}
	if action == operatorDeploymentCreate || action == operatorDeploymentContinue || action == operatorDeploymentSkipFailedBatch || action == operatorDeploymentRetry {
		if req.ConfirmChannel != d.Channel {
			return false
		}
	}
	if action == operatorDeploymentCreate || action == operatorDeploymentRetry {
		version, ok := operatorDeploymentOpenClawVersion(d)
		if !ok || req.ConfirmVersion != version {
			return false
		}
	}
	seenJobs := make(map[string]struct{}, len(receipt.Jobs))
	seenMachines := make(map[string]struct{}, len(receipt.Jobs))
	for _, job := range receipt.Jobs {
		if job.JobID == "" || job.MachineID == "" || job.DesiredID != d.DesiredID ||
			job.Revision != d.Revision || job.State != deploy.NotStarted || job.LeaseToken != "" ||
			job.LeaseExpiresAt != nil || job.TerminalAt != nil || job.ExecutionTimeout <= 0 ||
			!canonicalOperatorTime(job.CreatedAt) ||
			!job.CreatedAt.Equal(receipt.AppliedAt) {
			return false
		}
		if _, duplicate := seenJobs[job.JobID]; duplicate {
			return false
		}
		if _, duplicate := seenMachines[job.MachineID]; duplicate {
			return false
		}
		seenJobs[job.JobID] = struct{}{}
		seenMachines[job.MachineID] = struct{}{}
	}
	switch action {
	case operatorDeploymentCreate:
		return d.RetryOf == "" && d.ControlRevision == 0 && receipt.OpenedBatch == 1 &&
			d.State == DeploymentRunning && d.PausedAt == nil && d.FinishedAt == nil &&
			d.CreatedAt.Equal(receipt.AppliedAt) && len(receipt.Jobs) > 0 && len(receipt.Jobs) <= d.BatchSize
	case operatorDeploymentRetry:
		return d.DeploymentID != req.DeploymentID && d.RetryOf == req.DeploymentID &&
			d.ControlRevision == 0 && receipt.OpenedBatch == 1 && d.State == DeploymentRunning &&
			d.PausedAt == nil && d.FinishedAt == nil && d.CreatedAt.Equal(receipt.AppliedAt) &&
			len(receipt.Jobs) > 0 && len(receipt.Jobs) <= d.BatchSize
	case operatorDeploymentContinue:
		if d.DeploymentID != req.DeploymentID || req.ExpectedControlRevision == nil ||
			req.ExpectedOpenedBatch == nil || d.ControlRevision != *req.ExpectedControlRevision+1 ||
			d.PausedAt != nil {
			return false
		}
		openedNext := receipt.OpenedBatch == *req.ExpectedOpenedBatch+1 &&
			d.State == DeploymentRunning && d.FinishedAt == nil && len(receipt.Jobs) > 0 &&
			len(receipt.Jobs) <= d.BatchSize
		finished := receipt.OpenedBatch == *req.ExpectedOpenedBatch &&
			d.State == DeploymentFinished && d.FinishedAt != nil && len(receipt.Jobs) == 0
		return openedNext || finished
	case operatorDeploymentSkipFailedBatch:
		if d.DeploymentID != req.DeploymentID || req.ExpectedControlRevision == nil ||
			req.ExpectedOpenedBatch == nil || d.ControlRevision != *req.ExpectedControlRevision+1 ||
			d.PausedAt != nil {
			return false
		}
		return receipt.OpenedBatch == *req.ExpectedOpenedBatch+1 &&
			d.State == DeploymentRunning && d.FinishedAt == nil && len(receipt.Jobs) > 0 &&
			len(receipt.Jobs) <= d.BatchSize
	case operatorDeploymentAbandon:
		return d.DeploymentID == req.DeploymentID && req.ExpectedControlRevision != nil &&
			req.ExpectedOpenedBatch != nil && d.ControlRevision == *req.ExpectedControlRevision+1 &&
			receipt.OpenedBatch == *req.ExpectedOpenedBatch && d.State == DeploymentFinished &&
			d.PausedAt != nil && d.FinishedAt != nil && len(receipt.Jobs) == 0
	default:
		return false
	}
}

func operatorDeploymentOpenClawVersion(d Deployment) (string, bool) {
	if d.ResourceKind != "openclaw" || d.ResourceID != "openclaw" {
		return "", false
	}
	var spec model.OpenClawSpec
	if err := json.Unmarshal([]byte(d.Spec), &spec); err != nil || spec.Kind != "openclaw" ||
		!validJobReadRouteIdentifier(spec.Version, 128) {
		return "", false
	}
	return spec.Version, true
}

func validOperatorDeploymentPreviewDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && artifact.ValidSHA256Hex(strings.TrimPrefix(value, "sha256:"))
}

func validateOperatorDeploymentRejectionEvidence(tx *sql.Tx, action operatorDeploymentAction,
	req OperatorDeploymentRequest, cached operatorCachedRequest,
) (bool, error) {
	audit := operatorDeploymentAudit(action, req)
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
 WHERE action=? AND COALESCE(machine_id,'')=? AND subject=? AND COALESCE(reason,'')=?
   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='failed'
   AND COALESCE(detail,'')=?`, string(audit.Action), audit.MachineID,
		truncAudit(audit.Subject, auditMaxReason), truncAudit(audit.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, cached.CreatedAt, cached.ErrorDetail.String).Scan(&count)
	return count == 1, err
}

func validateOperatorDeploymentSuccessEvidence(tx *sql.Tx, receipt operatorDeploymentReceipt,
	req OperatorDeploymentRequest,
) (bool, error) {
	stored, err := deploymentByID(tx, receipt.Deployment.DeploymentID)
	if errors.Is(err, ErrDeploymentNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameOperatorDeploymentImmutable(stored, receipt.Deployment) {
		return false, nil
	}
	valid, err := validateOperatorDeploymentSuccessGraph(tx, stored, receipt.Action, req)
	if err != nil || !valid {
		return valid, err
	}
	// finished is terminal: neither canonical Continue-finish nor Abandon has
	// another legal lifecycle transition. Bind those receipt-only timestamps
	// and the terminal control revision back to the durable row; otherwise an
	// attacker could forge plausible canonical dates without changing the
	// success-detail string (which intentionally contains no timestamps).
	if receipt.Deployment.State == DeploymentFinished &&
		!sameOperatorDeploymentTerminalLifecycle(stored, receipt.Deployment) {
		return false, nil
	}
	if receipt.Deployment.State == DeploymentFinished {
		opened, err := deploymentOpenedBatchTx(tx, receipt.Deployment.DeploymentID)
		if err != nil {
			return false, err
		}
		if opened != receipt.OpenedBatch {
			return false, nil
		}
	}

	jobIDs := make(map[string]struct{}, len(receipt.Jobs))
	for _, receiptJob := range receipt.Jobs {
		storedJob, err := jobTx(tx, receiptJob.JobID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !sameOperatorDeploymentJobImmutable(storedJob, receiptJob) {
			return false, nil
		}
		var links int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM deployment_targets
 WHERE deployment_id=? AND machine_id=? AND job_id=? AND batch_no=?`,
			receipt.Deployment.DeploymentID, receiptJob.MachineID, receiptJob.JobID,
			receipt.OpenedBatch).Scan(&links); err != nil {
			return false, err
		}
		if links != 1 {
			return false, nil
		}
		jobIDs[receiptJob.JobID] = struct{}{}
	}
	if len(receipt.Jobs) > 0 {
		var durableJobs int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM deployment_targets
 WHERE deployment_id=? AND batch_no=? AND job_id IS NOT NULL`,
			receipt.Deployment.DeploymentID, receipt.OpenedBatch).Scan(&durableJobs); err != nil {
			return false, err
		}
		if durableJobs != len(jobIDs) {
			return false, nil
		}
	}

	audit := operatorDeploymentAudit(receipt.Action, req)
	var originalSuccessAudits int
	err = tx.QueryRow(`SELECT COUNT(*) FROM audit_log
 WHERE action=? AND COALESCE(machine_id,'')=? AND subject=? AND COALESCE(reason,'')=?
   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='ok'
   AND COALESCE(detail,'')=?`, string(audit.Action), audit.MachineID,
		truncAudit(receipt.Deployment.DeploymentID, auditMaxReason), truncAudit(audit.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, fmtTime(receipt.AppliedAt),
		operatorDeploymentSuccessDetail(receipt)).Scan(&originalSuccessAudits)
	return originalSuccessAudits == 1, err
}

// validateOperatorDeploymentSuccessGraph binds a cached success receipt to the
// durable target/job authority graph without re-evaluating mutable eligibility.
// Retry is wider: its mutation consumed and released the immediate parent's
// authority, so every durable ancestor that justified that retry must still be
// a complete, terminal, material-coherent graph.
func validateOperatorDeploymentSuccessGraph(tx *sql.Tx, child Deployment, action operatorDeploymentAction,
	req OperatorDeploymentRequest,
) (bool, error) {
	lineage := []Deployment{child}
	if action == operatorDeploymentRetry {
		var err error
		lineage, err = deploymentRetryLineage(tx, child)
		if err != nil {
			if errors.Is(err, ErrDeploymentRetryParentNotFound) || errors.Is(err, ErrDeploymentRetryCycle) ||
				errors.Is(err, ErrDeploymentRetryTooDeep) {
				return false, nil
			}
			return false, err
		}
		if len(lineage) < 2 || req.ExpectedControlRevision == nil || req.ExpectedOpenedBatch == nil {
			return false, nil
		}
	}
	for i, deployment := range lineage {
		if err := validateDeploymentJobGraph(tx, deployment.DeploymentID); err != nil {
			if errors.Is(err, ErrDeploymentJobGraphMismatch) {
				return false, nil
			}
			return false, err
		}
		if err := validateStoredDeploymentBatchPlan(tx, deployment); err != nil {
			if errors.Is(err, ErrDeploymentInvalidBatchPlan) {
				return false, nil
			}
			return false, err
		}
		if err := validateDeploymentJobMaterialGraph(tx, deployment.DeploymentID); err != nil {
			if errors.Is(err, ErrDeploymentMaterialMismatch) {
				return false, nil
			}
			return false, err
		}
		if i == 0 {
			continue
		}
		if deployment.Channel != child.Channel || deployment.ResourceKind != child.ResourceKind ||
			deployment.ResourceID != child.ResourceID || deployment.Spec != child.Spec ||
			deployment.State != DeploymentFinished || deployment.FinishedAt == nil {
			return false, nil
		}
		active, err := deploymentHasNonTerminalJobs(tx, deployment.DeploymentID)
		if err != nil {
			return false, err
		}
		if active {
			return false, nil
		}
	}
	if action == operatorDeploymentRetry {
		parent := lineage[1]
		opened, err := deploymentOpenedBatchTx(tx, parent.DeploymentID)
		if err != nil {
			return false, err
		}
		expectedRevision := *req.ExpectedControlRevision
		if opened != *req.ExpectedOpenedBatch ||
			(parent.ControlRevision != expectedRevision && parent.ControlRevision-1 != expectedRevision) {
			return false, nil
		}
	}
	return true, nil
}

func sameOperatorDeploymentImmutable(a, b Deployment) bool {
	return a.DeploymentID == b.DeploymentID && a.Channel == b.Channel && a.DesiredID == b.DesiredID &&
		a.ResourceKind == b.ResourceKind && a.ResourceID == b.ResourceID && a.Revision == b.Revision &&
		a.BatchSize == b.BatchSize && a.CreatedAt.Equal(b.CreatedAt) && a.CreatedBy == b.CreatedBy &&
		a.RetryOf == b.RetryOf && a.Spec == b.Spec
}

func sameOperatorDeploymentTerminalLifecycle(a, b Deployment) bool {
	return a.State == b.State && a.ControlRevision == b.ControlRevision &&
		sameOperatorDeploymentTimePtr(a.PausedAt, b.PausedAt) &&
		sameOperatorDeploymentTimePtr(a.FinishedAt, b.FinishedAt)
}

func sameOperatorDeploymentTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func sameOperatorDeploymentJobImmutable(a, b Job) bool {
	return a.JobID == b.JobID && a.MachineID == b.MachineID && a.DesiredID == b.DesiredID &&
		a.Revision == b.Revision && a.ExecutionTimeout == b.ExecutionTimeout &&
		a.ArtifactDigest == b.ArtifactDigest && a.Irreversible == b.Irreversible &&
		a.CreatedAt.Equal(b.CreatedAt)
}

func canonicalOperatorDeploymentCacheTime(raw string) bool {
	t := parseTime(raw)
	return !t.IsZero() && fmtTime(t) == raw
}

func operatorDeploymentResult(receipt operatorDeploymentReceipt) OperatorDeploymentResult {
	return OperatorDeploymentResult{
		Deployment: receipt.Deployment, Jobs: receipt.Jobs, OpenedBatch: receipt.OpenedBatch,
		PreviewDigest: receipt.PreviewDigest,
	}
}

func operatorDeploymentSuccessDetail(receipt operatorDeploymentReceipt) string {
	return fmt.Sprintf("operation=deployment-%s；deployment_id=%s；state=%s；control_revision=%d；opened_batch=%d；jobs=%d",
		receipt.Action, receipt.Deployment.DeploymentID, receipt.Deployment.State,
		receipt.Deployment.ControlRevision, receipt.OpenedBatch, len(receipt.Jobs))
}

func canonicalOperatorDeploymentRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodeDeploymentPreviewStale:
		return "preview_digest 或 deployment/material/target snapshot 已變更；請重新預覽", true
	case OperatorCodePreconditionRequired:
		return "expected_control_revision 與 expected_opened_batch 不可省略；請重新讀取 deployment", true
	case OperatorCodeDeploymentPreconditionFailed:
		return "deployment control revision 或 opened batch 不符合 request 預期；請重新讀取並預覽", true
	case OperatorCodeDeploymentContinueRefused:
		return "plain Continue refuses a failed batch. Use the separately labelled skip failed batch action and record a reason.", true
	case OperatorCodeDeploymentNotFound:
		return "找不到指定的 deployment", true
	case OperatorCodeDeploymentNotPaused:
		return "deployment 不是可 continue/retry/abandon 的 paused 狀態", true
	case OperatorCodeDeploymentActiveJobs:
		return "deployment 仍有未終態 job；不能執行這個控制動作", true
	case OperatorCodeDeploymentNoRetryTargets:
		return "deployment 沒有可 retry 的終態失敗 targets", true
	case OperatorCodeDeploymentNoIncludedTargets:
		return "目前沒有可排進計畫的機器；請處理排除原因後再預覽", true
	case OperatorCodeDeploymentPromotionBlocked:
		return "stable promotion safety gate 目前拒絕這個 deployment 動作", true
	case OperatorCodeDeploymentConfirmationMismatch:
		return "deployment typed confirmation 與操作目標不符", true
	case OperatorCodeBadChannel:
		return "deployment channel 只接受 canary 或 stable", true
	default:
		return "", false
	}
}

func historicalOperatorDeploymentRejectionDetail(code string) (string, bool) {
	if _, ok := canonicalOperatorDeploymentRejectionDetail(code); !ok {
		return "", false
	}
	return "原 deployment request 當時以 " + code + " 拒絕", true
}

func isCanonicalOperatorDeploymentRejection(code string) bool {
	_, ok := canonicalOperatorDeploymentRejectionDetail(code)
	return ok
}

func operatorDeploymentCodeForError(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrDeploymentNotFound), errors.Is(err, ErrDeploymentRetryParentNotFound):
		return OperatorCodeDeploymentNotFound, true
	case errors.Is(err, ErrDeploymentNotPaused), errors.Is(err, ErrDeploymentRetryParentRunning):
		return OperatorCodeDeploymentNotPaused, true
	case errors.Is(err, ErrDeploymentActiveJobs), errors.Is(err, ErrDeploymentRetryParentActive),
		errors.Is(err, ErrDeploymentAbandonActiveJobs):
		return OperatorCodeDeploymentActiveJobs, true
	case errors.Is(err, ErrDeploymentNoRetryTargets):
		return OperatorCodeDeploymentNoRetryTargets, true
	case errors.Is(err, ErrPromoteLocked), errors.Is(err, ErrStablePromoteGateRequired):
		return OperatorCodeDeploymentPromotionBlocked, true
	case errors.Is(err, ErrBadChannel):
		return OperatorCodeBadChannel, true
	case errors.Is(err, ErrDeploymentContinueRefused):
		return OperatorCodeDeploymentContinueRefused, true
	case errors.Is(err, ErrDeploymentPreconditionFailed), errors.Is(err, ErrDeploymentBatchNotReady),
		errors.Is(err, ErrDeploymentFinishNotReady), errors.Is(err, ErrDeploymentBadTransition):
		return OperatorCodeDeploymentPreconditionFailed, true
	case errors.Is(err, ErrDeploymentPreviewStale), errors.Is(err, ErrDeploymentTargetChannelChanged),
		errors.Is(err, ErrDeploymentConflict), errors.Is(err, ErrDeploymentActiveResource),
		errors.Is(err, ErrDeploymentStaleRevision), errors.Is(err, ErrDeploymentMaterialMismatch),
		errors.Is(err, ErrDeploymentInvalidBatchPlan), errors.Is(err, ErrDeploymentJobGraphMismatch),
		errors.Is(err, ErrDeploymentRetryMismatch), errors.Is(err, ErrDeploymentControlRevisionLimit),
		errors.Is(err, ErrDeploymentRetryCycle), errors.Is(err, ErrDeploymentRetryTooDeep),
		errors.Is(err, ErrMachineRetired):
		return OperatorCodeDeploymentPreviewStale, true
	default:
		return "", false
	}
}

func (s *Store) mutateOperatorDeploymentTx(tx *sql.Tx, action operatorDeploymentAction,
	req OperatorDeploymentRequest, prepared OperatorDeploymentPrepared, now time.Time,
) (Deployment, []Job, error) {
	if action != operatorDeploymentCreate {
		current, err := deploymentByID(tx, req.DeploymentID)
		if err != nil {
			return Deployment{}, nil, err
		}
		opened, err := deploymentOpenedBatchTx(tx, req.DeploymentID)
		if err != nil {
			return Deployment{}, nil, err
		}
		if current.ControlRevision != *req.ExpectedControlRevision || opened != *req.ExpectedOpenedBatch {
			return Deployment{}, nil, ErrDeploymentPreconditionFailed
		}
	}

	switch action {
	case operatorDeploymentCreate:
		if prepared.Deployment.RetryOf != "" {
			return Deployment{}, nil, ErrDeploymentPreviewStale
		}
		return s.createOperatorDeploymentTx(tx, prepared.Deployment, now)
	case operatorDeploymentRetry:
		if prepared.Deployment.RetryOf != req.DeploymentID {
			return Deployment{}, nil, ErrDeploymentPreviewStale
		}
		if err := validateOperatorDeploymentRetryTargetsTx(tx, req.DeploymentID, prepared); err != nil {
			return Deployment{}, nil, err
		}
		return s.createOperatorDeploymentTx(tx, prepared.Deployment, now)
	case operatorDeploymentContinue:
		return s.continueDeploymentTx(tx, req.DeploymentID, now, deploymentContinuePolicy{RefuseFailedBatch: true})
	case operatorDeploymentSkipFailedBatch:
		return s.continueDeploymentTx(tx, req.DeploymentID, now, deploymentContinuePolicy{
			SkipFailedBatch: true, SkipReason: req.Audit.Reason,
		})
	case operatorDeploymentAbandon:
		d, err := s.abandonDeploymentTx(tx, req.DeploymentID, now)
		return d, nil, err
	default:
		return Deployment{}, nil, errors.New("store: unknown operator deployment action")
	}
}

func (s *Store) createOperatorDeploymentTx(tx *sql.Tx, n NewDeployment, now time.Time) (Deployment, []Job, error) {
	if err := validateNewDeployment(n); err != nil {
		return Deployment{}, nil, err
	}
	if n.Channel == "stable" {
		version, digest, err := stableOpenClawMaterial(n)
		if err != nil {
			return Deployment{}, nil, err
		}
		decision, err := s.PreviewStableOpenClawPromotion(version, digest, now)
		if err != nil {
			return Deployment{}, nil, err
		}
		if !decision.Allowed {
			return Deployment{}, nil, fmt.Errorf("%w: %s", ErrPromoteLocked, decision.Summary())
		}
	}
	return createDeploymentTx(tx, n, now)
}

func validateOperatorDeploymentRetryTargetsTx(tx *sql.Tx, parentID string,
	prepared OperatorDeploymentPrepared,
) error {
	if err := validateDeploymentJobGraph(tx, parentID); err != nil {
		return err
	}
	active, err := deploymentHasNonTerminalJobs(tx, parentID)
	if err != nil {
		return err
	}
	if active {
		return ErrDeploymentActiveJobs
	}
	provided, valid := exactMachineIDSet(prepared.TerminalFailureMachineIDs)
	if !valid || len(provided) == 0 {
		return ErrDeploymentNoRetryTargets
	}
	plannedIDs := make([]string, 0, len(prepared.Deployment.Targets))
	for _, target := range prepared.Deployment.Targets {
		plannedIDs = append(plannedIDs, target.MachineID)
	}
	planned, valid := exactMachineIDSet(plannedIDs)
	if !valid || !sameMachineIDSet(provided, planned) {
		return ErrDeploymentPreviewStale
	}

	rows, err := tx.Query(`SELECT t.machine_id
	 FROM deployment_targets t JOIN deployments p ON p.deployment_id=t.deployment_id
	 JOIN jobs j ON j.job_id=t.job_id AND j.machine_id=t.machine_id
	             AND j.desired_id=p.desired_id AND j.revision=p.revision
	 WHERE t.deployment_id=? AND j.state IN (?,?,?,?)
	 ORDER BY t.machine_id`, parentID, deploy.Failed, deploy.Rejected,
		deploy.LeaseExpired, deploy.ManualIntervention)
	if err != nil {
		return fmt.Errorf("store: inspect deployment terminal-failure retry targets: %w", err)
	}
	defer rows.Close()
	actual := make(map[string]struct{})
	for rows.Next() {
		var machineID string
		if err := rows.Scan(&machineID); err != nil {
			return fmt.Errorf("store: scan deployment terminal-failure retry target: %w", err)
		}
		actual[machineID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read deployment terminal-failure retry targets: %w", err)
	}
	if len(actual) == 0 {
		return ErrDeploymentNoRetryTargets
	}
	if !sameMachineIDSet(provided, actual) {
		return ErrDeploymentPreviewStale
	}
	return nil
}

func exactMachineIDSet(ids []string) (map[string]struct{}, bool) {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, false
		}
		if _, duplicate := out[id]; duplicate {
			return nil, false
		}
		out[id] = struct{}{}
	}
	return out, true
}

func sameMachineIDSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}
