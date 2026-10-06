package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

const (
	OperatorDiagnosticNoopMinTimeoutSeconds = 1
	OperatorDiagnosticNoopMaxTimeoutSeconds = 3600
	OperatorDiagnosticNoopDefaultTimeout    = 600

	OperatorDiagnosticNoopKind           = "noop"
	OperatorDiagnosticNoopResourceKind   = "openclaw"
	OperatorDiagnosticNoopResourceID     = "openclaw"
	OperatorDiagnosticNoopSpec           = `{"kind":"noop"}`
	OperatorDiagnosticNoopWatermarkScope = "resource"

	operatorDiagnosticNoopVersion         = "v3"
	operatorDiagnosticNoopOperationPrefix = "diagnostic-noop-create:v3:"
	operatorDiagnosticNoopCacheInvalid    = "operator diagnostic noop idempotency cache invalid；未回放結果"
	operatorDiagnosticNoopRejectPrefix    = "operator diagnostic noop rejection code="
)

type OperatorDiagnosticNoopBlocker string

const (
	OperatorDiagnosticNoopBlockerRetired           OperatorDiagnosticNoopBlocker = "machine_retired"
	OperatorDiagnosticNoopBlockerNeverReported     OperatorDiagnosticNoopBlocker = "machine_never_reported"
	OperatorDiagnosticNoopBlockerNonterminalJob    OperatorDiagnosticNoopBlocker = "nonterminal_jobs"
	OperatorDiagnosticNoopBlockerExecutionUnknown  OperatorDiagnosticNoopBlocker = "agent_execution_unknown"
	OperatorDiagnosticNoopBlockerExecutionDisabled OperatorDiagnosticNoopBlocker = "agent_execution_disabled"
)

// OperatorDiagnosticNoopImpact is the fixed, non-configuration-changing
// protocol drill contract. ResourceKind/ResourceID keep the diagnostic and
// OpenClaw revisions in one resource scope, matching the work this drill covers.
type OperatorDiagnosticNoopImpact struct {
	Kind                        string                          `json:"kind"`
	ResourceKind                string                          `json:"resource_kind"`
	ResourceID                  string                          `json:"resource_id"`
	AgentWatermarkScope         string                          `json:"agent_watermark_scope"`
	SpecDigest                  string                          `json:"spec_digest"`
	ExecutionTimeoutSeconds     int                             `json:"execution_timeout_seconds"`
	ChangesMachineConfiguration bool                            `json:"changes_machine_configuration"`
	CreatesDesiredState         bool                            `json:"creates_desired_state"`
	CreatesJob                  bool                            `json:"creates_job"`
	DeliveryRequiresJobsEnabled bool                            `json:"delivery_requires_jobs_enabled"`
	JobsEnabled                 *bool                           `json:"jobs_enabled"`
	EverReported                bool                            `json:"ever_reported"`
	ActiveJobCount              int64                           `json:"active_job_count"`
	CurrentResourceRevision     deploy.Revision                 `json:"current_resource_revision"`
	PlannedRevision             deploy.Revision                 `json:"planned_revision"`
	Blockers                    []OperatorDiagnosticNoopBlocker `json:"blockers"`
}

type OperatorDiagnosticNoopPreviewResult struct {
	MachineID         string    `json:"machine_id"`
	DisplayName       string    `json:"display_name"`
	LifecycleRevision int64     `json:"lifecycle_revision"`
	PreviewedAt       time.Time `json:"previewed_at"`
	OperatorDiagnosticNoopImpact
	PreviewDigest string `json:"preview_digest"`
}

type OperatorDiagnosticNoopRequest struct {
	MachineID          string
	ExecutionTimeout   int
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	RequestDigest      string
	CreatedBy          string
	Audit              AuditEntry
}

type OperatorDiagnosticNoopResult struct {
	MachineID         string          `json:"machine_id"`
	DisplayName       string          `json:"display_name"`
	DesiredID         string          `json:"desired_id"`
	JobID             string          `json:"job_id"`
	Revision          deploy.Revision `json:"revision"`
	CreatedAt         time.Time       `json:"created_at"`
	LifecycleRevision int64           `json:"lifecycle_revision"`
	OperatorDiagnosticNoopImpact
	PreviewDigest string `json:"preview_digest"`
	Replayed      bool   `json:"replayed"`
	Audited       bool   `json:"-"`
}

type operatorDiagnosticNoopSnapshot struct {
	MachineID               string
	DisplayName             string
	LifecycleRevision       int64
	Retired                 bool
	EverReported            bool
	JobsEnabled             *bool
	ActiveJobCount          int64
	CurrentResourceRevision deploy.Revision
}

type operatorDiagnosticNoopReceipt struct {
	SchemaVersion     string          `json:"schema_version"`
	MachineID         string          `json:"machine_id"`
	DisplayName       string          `json:"display_name"`
	DesiredID         string          `json:"desired_id"`
	JobID             string          `json:"job_id"`
	Revision          deploy.Revision `json:"revision"`
	CreatedAt         time.Time       `json:"created_at"`
	LifecycleRevision int64           `json:"lifecycle_revision"`
	OperatorDiagnosticNoopImpact
	PreviewDigest string `json:"preview_digest"`
}

func operatorDiagnosticNoopSpecDigest() string {
	sum := sha256.Sum256([]byte(OperatorDiagnosticNoopSpec))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func operatorDiagnosticNoopOperation(machineID string) string {
	return operatorDiagnosticNoopOperationPrefix + machineID
}

func (s *Store) PreviewOperatorDiagnosticNoop(machineID string, timeoutSeconds int) (OperatorDiagnosticNoopPreviewResult, error) {
	if !validOperatorDiagnosticNoopTimeout(timeoutSeconds) {
		return OperatorDiagnosticNoopPreviewResult{}, operatorDiagnosticNoopRejection(OperatorCodeBadDiagnosticTimeout)
	}
	now := s.now().UTC().Truncate(time.Second)
	snapshot, err := loadOperatorDiagnosticNoopSnapshot(s.rdb, machineID)
	if err != nil {
		return OperatorDiagnosticNoopPreviewResult{}, err
	}
	return operatorDiagnosticNoopPreview(snapshot, timeoutSeconds, now), nil
}

type diagnosticNoopQueryer interface {
	machineJobsEnabledQueryer
	QueryRow(query string, args ...any) *sql.Row
}

func loadOperatorDiagnosticNoopSnapshot(q diagnosticNoopQueryer, machineID string) (operatorDiagnosticNoopSnapshot, error) {
	var snapshot operatorDiagnosticNoopSnapshot
	var retired sql.NullString
	var everReported int
	err := q.QueryRow(`
SELECT m.display_name,m.lifecycle_revision,m.retired_at,
       EXISTS(SELECT 1 FROM machine_checkins c WHERE c.machine_id=m.machine_id),
       (SELECT COUNT(*) FROM jobs j WHERE j.machine_id=m.machine_id AND j.state NOT IN (?,?,?,?,?)),
       COALESCE((SELECT current_revision FROM revision_counters WHERE resource_scope=?),0)
  FROM machine_registry m
 WHERE m.machine_id=?`,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention,
		OperatorDiagnosticNoopResourceKind+":"+OperatorDiagnosticNoopResourceID, machineID).Scan(
		&snapshot.DisplayName, &snapshot.LifecycleRevision, &retired, &everReported,
		&snapshot.ActiveJobCount, &snapshot.CurrentResourceRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, operatorError(OperatorCodeMachineNotFound, "找不到這台機器")
	}
	if err != nil {
		return snapshot, fmt.Errorf("store: inspect operator diagnostic noop target: %w", err)
	}
	if snapshot.DisplayName == "" || snapshot.LifecycleRevision < 0 || snapshot.ActiveJobCount < 0 ||
		snapshot.CurrentResourceRevision < 0 || snapshot.CurrentResourceRevision >= deploy.Revision(math.MaxInt64) {
		return snapshot, errors.New("store: diagnostic noop target projection is invalid")
	}
	snapshot.MachineID = machineID
	snapshot.Retired = retired.Valid && retired.String != ""
	snapshot.EverReported = everReported != 0
	snapshot.JobsEnabled, err = latestMachineJobsEnabled(q, machineID)
	if errors.Is(err, errMachineJobsEnabledProjectionInvalid) {
		return snapshot, errors.New("store: diagnostic noop target projection is invalid")
	}
	if err != nil {
		return snapshot, fmt.Errorf("store: inspect diagnostic noop check-in jobs state: %w", err)
	}
	return snapshot, nil
}

func operatorDiagnosticNoopPreview(snapshot operatorDiagnosticNoopSnapshot, timeoutSeconds int, now time.Time) OperatorDiagnosticNoopPreviewResult {
	impact := OperatorDiagnosticNoopImpact{
		Kind:                        OperatorDiagnosticNoopKind,
		ResourceKind:                OperatorDiagnosticNoopResourceKind,
		ResourceID:                  OperatorDiagnosticNoopResourceID,
		AgentWatermarkScope:         OperatorDiagnosticNoopWatermarkScope,
		SpecDigest:                  operatorDiagnosticNoopSpecDigest(),
		ExecutionTimeoutSeconds:     timeoutSeconds,
		ChangesMachineConfiguration: false,
		CreatesDesiredState:         true,
		CreatesJob:                  true,
		DeliveryRequiresJobsEnabled: true,
		JobsEnabled:                 snapshot.JobsEnabled,
		EverReported:                snapshot.EverReported,
		ActiveJobCount:              snapshot.ActiveJobCount,
		CurrentResourceRevision:     snapshot.CurrentResourceRevision,
		PlannedRevision:             snapshot.CurrentResourceRevision + 1,
		Blockers:                    make([]OperatorDiagnosticNoopBlocker, 0, 5),
	}
	if snapshot.Retired {
		impact.Blockers = append(impact.Blockers, OperatorDiagnosticNoopBlockerRetired)
	}
	if !snapshot.EverReported {
		impact.Blockers = append(impact.Blockers, OperatorDiagnosticNoopBlockerNeverReported)
	} else if snapshot.JobsEnabled == nil {
		impact.Blockers = append(impact.Blockers, OperatorDiagnosticNoopBlockerExecutionUnknown)
	} else if !*snapshot.JobsEnabled {
		impact.Blockers = append(impact.Blockers, OperatorDiagnosticNoopBlockerExecutionDisabled)
	}
	if snapshot.ActiveJobCount > 0 {
		impact.Blockers = append(impact.Blockers, OperatorDiagnosticNoopBlockerNonterminalJob)
	}
	result := OperatorDiagnosticNoopPreviewResult{
		MachineID: snapshot.MachineID, DisplayName: snapshot.DisplayName,
		LifecycleRevision: snapshot.LifecycleRevision, PreviewedAt: now,
		OperatorDiagnosticNoopImpact: impact,
	}
	result.PreviewDigest = operatorDiagnosticNoopPreviewDigest(snapshot, impact)
	return result
}

func operatorDiagnosticNoopPreviewDigest(snapshot operatorDiagnosticNoopSnapshot, impact OperatorDiagnosticNoopImpact) string {
	body := struct {
		Version           string                       `json:"version"`
		MachineID         string                       `json:"machine_id"`
		DisplayName       string                       `json:"display_name"`
		LifecycleRevision int64                        `json:"lifecycle_revision"`
		Retired           bool                         `json:"retired"`
		Impact            OperatorDiagnosticNoopImpact `json:"impact"`
	}{operatorDiagnosticNoopVersion, snapshot.MachineID, snapshot.DisplayName,
		snapshot.LifecycleRevision, snapshot.Retired, impact}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validOperatorDiagnosticNoopTimeout(seconds int) bool {
	return seconds >= OperatorDiagnosticNoopMinTimeoutSeconds && seconds <= OperatorDiagnosticNoopMaxTimeoutSeconds
}

func operatorDiagnosticNoopRejection(code string) *OperatorRequestError {
	detail, ok := canonicalOperatorDiagnosticNoopRejectionDetail(code)
	if !ok {
		return operatorError(code, "控制面 diagnostic noop request 被拒絕")
	}
	return operatorError(code, detail)
}

func canonicalOperatorDiagnosticNoopRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeBadDiagnosticTimeout:
		return fmt.Sprintf("execution_timeout_seconds 必須介於 %d 與 %d", OperatorDiagnosticNoopMinTimeoutSeconds, OperatorDiagnosticNoopMaxTimeoutSeconds), true
	case OperatorCodeMachineNotFound:
		return "找不到這台機器", true
	case OperatorCodeConfirmationMismatch:
		return "confirm_display_name 必須與目前顯示名稱逐字相同", true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodeDiagnosticPreviewStale:
		return "preview_digest 與目前 machine lifecycle、job occupancy、revision 或 diagnostic policy 不符；請重新預覽", true
	case OperatorCodeMachineRetired:
		return "機器已退役，不建立 diagnostic job", true
	case OperatorCodeNeverObserved:
		return "機器從未回報；diagnostic delivery unavailable", true
	case OperatorCodeMachineActiveJob:
		return "機器已有未終態 job；先讓工作單結束再跑 protocol drill", true
	case OperatorCodeAgentExecutionUnknown:
		return "agent 尚未回報工作單執行狀態；等待下一次 check-in 後重新預覽", true
	case OperatorCodeAgentExecutionDisabled:
		return "agent 工作單執行未啟用；將 jobs_enabled 設為 true 並重新啟動 clawctl-agent", true
	case OperatorCodeReasonRequired:
		return "reason 不可省略或只含空白", true
	default:
		return "", false
	}
}

func historicalOperatorDiagnosticNoopRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeBadDiagnosticTimeout:
		return "原 request 的 execution_timeout_seconds 不符合當時 diagnostic policy", true
	case OperatorCodeMachineNotFound:
		return "原 request 當時找不到目標機器", true
	case OperatorCodeConfirmationMismatch:
		return "原 request 的 typed confirmation 當時不符", true
	case OperatorCodePreviewRequired:
		return "原 request 當時缺少必要的 preview_digest", true
	case OperatorCodeDiagnosticPreviewStale:
		return "原 request 的 preview_digest 當時已失效", true
	case OperatorCodeMachineRetired:
		return "原 request 的目標機器當時已退役", true
	case OperatorCodeNeverObserved:
		return "原 request 的目標機器當時從未回報", true
	case OperatorCodeMachineActiveJob:
		return "原 request 的目標機器當時已有未終態 job", true
	case OperatorCodeAgentExecutionUnknown:
		return "原 request 的 agent 當時尚未回報工作單執行狀態", true
	case OperatorCodeAgentExecutionDisabled:
		return "原 request 的 agent 當時未啟用工作單執行", true
	case OperatorCodeReasonRequired:
		return "原 request 當時缺少操作理由", true
	default:
		return "", false
	}
}

func operatorDiagnosticNoopRejectionAuditDetail(code, detail string) string {
	return truncAudit(operatorDiagnosticNoopRejectPrefix+code+"；"+detail, auditMaxReason)
}

// ApplyOperatorDiagnosticNoop atomically creates the desired state, job,
// immutable replay receipt and audit row. It never leaves the orphan desired
// state that the historical two-transaction CLI path could leave behind.
func (s *Store) ApplyOperatorDiagnosticNoop(req OperatorDiagnosticNoopRequest) (OperatorDiagnosticNoopResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return OperatorDiagnosticNoopResult{}, operatorError(OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略")
	}
	if len(req.IdempotencyKey) > 200 {
		return OperatorDiagnosticNoopResult{}, operatorError(OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorDiagnosticNoopResult{}, operatorError(OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}
	audit := req.Audit
	audit.Action, audit.MachineID, audit.Subject = AuditDiagnosticNoop, req.MachineID, req.MachineID
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = req.Reason, req.IdempotencyKey, req.RequestDigest
	tx, err := s.beginWrite(context.Background(), "apply_operator_diagnostic_noop")
	if err != nil {
		return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: begin operator diagnostic noop: %w", err)
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
		return s.replayOperatorDiagnosticNoop(tx, req, audit, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: inspect diagnostic noop idempotency key: %w", err)
	}

	reject := func(code string) (OperatorDiagnosticNoopResult, error) {
		detail, ok := canonicalOperatorDiagnosticNoopRejectionDetail(code)
		if !ok {
			return OperatorDiagnosticNoopResult{}, errors.New("store: invalid diagnostic noop rejection code")
		}
		audit.OK, audit.Detail = false, operatorDiagnosticNoopRejectionAuditDetail(code, detail)
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: record diagnostic noop rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey,
			operatorDiagnosticNoopOperation(req.MachineID), req.RequestDigest,
			code, detail, fmtTime(writerNow)); err != nil {
			return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: persist diagnostic noop rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: commit diagnostic noop rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorDiagnosticNoopResult{}, rejection
	}

	if !validOperatorDiagnosticNoopTimeout(req.ExecutionTimeout) {
		return reject(OperatorCodeBadDiagnosticTimeout)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return reject(OperatorCodeReasonRequired)
	}
	if strings.TrimSpace(req.PreviewDigest) == "" {
		return reject(OperatorCodePreviewRequired)
	}
	snapshot, err := loadOperatorDiagnosticNoopSnapshot(tx, req.MachineID)
	if err != nil {
		var rejection *OperatorRequestError
		if errors.As(err, &rejection) && rejection.Code == OperatorCodeMachineNotFound {
			return reject(rejection.Code)
		}
		return OperatorDiagnosticNoopResult{}, err
	}
	audit.Subject = snapshot.DisplayName
	if req.ConfirmDisplayName != snapshot.DisplayName {
		return reject(OperatorCodeConfirmationMismatch)
	}
	preview := operatorDiagnosticNoopPreview(snapshot, req.ExecutionTimeout, writerNow)
	if req.PreviewDigest != preview.PreviewDigest {
		return reject(OperatorCodeDiagnosticPreviewStale)
	}
	for _, blocker := range preview.Blockers {
		switch blocker {
		case OperatorDiagnosticNoopBlockerRetired:
			return reject(OperatorCodeMachineRetired)
		case OperatorDiagnosticNoopBlockerNeverReported:
			return reject(OperatorCodeNeverObserved)
		case OperatorDiagnosticNoopBlockerNonterminalJob:
			return reject(OperatorCodeMachineActiveJob)
		case OperatorDiagnosticNoopBlockerExecutionUnknown:
			return reject(OperatorCodeAgentExecutionUnknown)
		case OperatorDiagnosticNoopBlockerExecutionDisabled:
			return reject(OperatorCodeAgentExecutionDisabled)
		default:
			return OperatorDiagnosticNoopResult{}, errors.New("store: unrecognized diagnostic noop blocker")
		}
	}
	if strings.TrimSpace(req.CreatedBy) == "" || len(req.CreatedBy) > auditMaxReason {
		return OperatorDiagnosticNoopResult{}, errors.New("store: diagnostic noop creator provenance is invalid")
	}
	desiredID, revision, err := createDesiredStateTx(tx, "machine", snapshot.MachineID,
		OperatorDiagnosticNoopResourceKind, OperatorDiagnosticNoopResourceID,
		OperatorDiagnosticNoopSpec, req.CreatedBy, writerNow)
	if err != nil {
		return OperatorDiagnosticNoopResult{}, err
	}
	if revision != preview.PlannedRevision {
		return OperatorDiagnosticNoopResult{}, errors.New("store: diagnostic noop revision changed during transaction")
	}
	jobID := newID()
	if _, err := tx.Exec(`INSERT INTO jobs
	 (job_id,machine_id,desired_id,revision,state,created_at,artifact_digest,irreversible,execution_timeout)
	 VALUES (?,?,?,?,?,?,?,?,?)`, jobID, snapshot.MachineID, desiredID, revision,
		deploy.NotStarted, fmtTime(writerNow), operatorDiagnosticNoopSpecDigest(), false, req.ExecutionTimeout); err != nil {
		return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: create diagnostic noop job: %w", err)
	}
	receipt := operatorDiagnosticNoopReceipt{
		SchemaVersion: operatorDiagnosticNoopVersion,
		MachineID:     snapshot.MachineID, DisplayName: snapshot.DisplayName,
		DesiredID: desiredID, JobID: jobID, Revision: revision, CreatedAt: writerNow,
		LifecycleRevision:            snapshot.LifecycleRevision,
		OperatorDiagnosticNoopImpact: preview.OperatorDiagnosticNoopImpact,
		PreviewDigest:                req.PreviewDigest,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: encode diagnostic noop receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorDiagnosticNoopOperation(req.MachineID),
		req.RequestDigest, string(raw), fmtTime(writerNow)); err != nil {
		return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: persist diagnostic noop receipt: %w", err)
	}
	audit.OK, audit.Detail = true, operatorDiagnosticNoopSuccessAuditDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: record diagnostic noop success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: commit operator diagnostic noop: %w", err)
	}
	result := operatorDiagnosticNoopResult(receipt)
	result.Audited = true
	return result, nil
}

func operatorDiagnosticNoopSuccessAuditDetail(receipt operatorDiagnosticNoopReceipt) string {
	raw, _ := json.Marshal(receipt)
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("diagnostic noop job=%s desired=%s revision=%d timeout=%ds config_changed=false jobs_enabled=true receipt_sha256=%x",
		receipt.JobID, receipt.DesiredID, receipt.Revision, receipt.ExecutionTimeoutSeconds, sum)
}

func operatorDiagnosticNoopResult(receipt operatorDiagnosticNoopReceipt) OperatorDiagnosticNoopResult {
	return OperatorDiagnosticNoopResult{
		MachineID: receipt.MachineID, DisplayName: receipt.DisplayName,
		DesiredID: receipt.DesiredID, JobID: receipt.JobID, Revision: receipt.Revision,
		CreatedAt: receipt.CreatedAt, LifecycleRevision: receipt.LifecycleRevision,
		OperatorDiagnosticNoopImpact: receipt.OperatorDiagnosticNoopImpact,
		PreviewDigest:                receipt.PreviewDigest,
	}
}

func (s *Store) replayOperatorDiagnosticNoop(tx dbTx, req OperatorDiagnosticNoopRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorDiagnosticNoopResult, error) {
	if cached.Operation != operatorDiagnosticNoopOperation(req.MachineID) || cached.Digest != req.RequestDigest {
		audit.OK = false
		audit.Detail = "idempotency conflict：idempotency key 已被不同的 operation 或 canonical request body 使用"
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: record diagnostic noop idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: commit diagnostic noop idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict,
			"idempotency key 已被不同的 operation 或 canonical request body 使用")
		rejection.Audited = true
		return OperatorDiagnosticNoopResult{}, rejection
	}
	if !canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return OperatorDiagnosticNoopResult{}, errors.New(operatorDiagnosticNoopCacheInvalid)
	}
	if cached.Outcome == "rejected" {
		if cached.ResponseJSON.Valid || !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid {
			return OperatorDiagnosticNoopResult{}, errors.New(operatorDiagnosticNoopCacheInvalid)
		}
		canonical, ok := canonicalOperatorDiagnosticNoopRejectionDetail(cached.ErrorCode.String)
		historical, historicalOK := historicalOperatorDiagnosticNoopRejectionDetail(cached.ErrorCode.String)
		if !ok || !historicalOK || cached.ErrorDetail.String != canonical {
			return OperatorDiagnosticNoopResult{}, errors.New(operatorDiagnosticNoopCacheInvalid)
		}
		valid, err := validateOperatorDiagnosticNoopAudit(tx, req, cached.CreatedAt, false,
			operatorDiagnosticNoopRejectionAuditDetail(cached.ErrorCode.String, canonical))
		if err != nil || !valid {
			return OperatorDiagnosticNoopResult{}, errors.New(operatorDiagnosticNoopCacheInvalid)
		}
		audit.OK, audit.Detail = false, OperatorIdempotencyReplayPrefix+"原判決："+historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: record rejected diagnostic noop replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: commit rejected diagnostic noop replay audit: %w", err)
		}
		return OperatorDiagnosticNoopResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid || cached.ErrorDetail.Valid {
		return OperatorDiagnosticNoopResult{}, errors.New(operatorDiagnosticNoopCacheInvalid)
	}
	receipt, err := decodeOperatorDiagnosticNoopReceipt(cached.ResponseJSON.String)
	if err != nil || validateOperatorDiagnosticNoopReceipt(receipt, req, cached.CreatedAt) != nil {
		return OperatorDiagnosticNoopResult{}, errors.New(operatorDiagnosticNoopCacheInvalid)
	}
	valid, err := validateOperatorDiagnosticNoopSuccessEvidence(tx, receipt, req)
	if err != nil || !valid {
		return OperatorDiagnosticNoopResult{}, errors.New(operatorDiagnosticNoopCacheInvalid)
	}
	audit.Subject, audit.OK = receipt.DisplayName, true
	audit.Detail = OperatorIdempotencyReplayPrefix + "沒有再次建立 desired state、job 或 revision"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: record successful diagnostic noop replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorDiagnosticNoopResult{}, fmt.Errorf("store: commit successful diagnostic noop replay audit: %w", err)
	}
	result := operatorDiagnosticNoopResult(receipt)
	result.Replayed, result.Audited = true, true
	return result, nil
}

func decodeOperatorDiagnosticNoopReceipt(raw string) (operatorDiagnosticNoopReceipt, error) {
	var receipt operatorDiagnosticNoopReceipt
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&receipt); err != nil {
		return receipt, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return receipt, errors.New("cached diagnostic noop receipt has trailing JSON")
	}
	return receipt, nil
}

func validateOperatorDiagnosticNoopReceipt(receipt operatorDiagnosticNoopReceipt,
	req OperatorDiagnosticNoopRequest, cachedCreatedAt string,
) error {
	if receipt.SchemaVersion != operatorDiagnosticNoopVersion || receipt.MachineID != req.MachineID ||
		receipt.MachineID == "" || receipt.DisplayName == "" || receipt.DesiredID == "" || receipt.JobID == "" ||
		receipt.Revision <= 0 || receipt.LifecycleRevision < 0 || receipt.PreviewDigest != req.PreviewDigest ||
		!canonicalOperatorTime(receipt.CreatedAt) || fmtTime(receipt.CreatedAt) != cachedCreatedAt ||
		receipt.Kind != OperatorDiagnosticNoopKind || receipt.ResourceKind != OperatorDiagnosticNoopResourceKind ||
		receipt.ResourceID != OperatorDiagnosticNoopResourceID || receipt.AgentWatermarkScope != OperatorDiagnosticNoopWatermarkScope ||
		receipt.SpecDigest != operatorDiagnosticNoopSpecDigest() || receipt.ExecutionTimeoutSeconds != req.ExecutionTimeout ||
		receipt.ChangesMachineConfiguration || !receipt.CreatesDesiredState || !receipt.CreatesJob ||
		!receipt.DeliveryRequiresJobsEnabled || !receipt.EverReported ||
		receipt.JobsEnabled == nil || !*receipt.JobsEnabled ||
		receipt.ActiveJobCount != 0 || receipt.CurrentResourceRevision+1 != receipt.PlannedRevision ||
		receipt.PlannedRevision != receipt.Revision || len(receipt.Blockers) != 0 {
		return errors.New("cached diagnostic noop receipt fields are invalid")
	}
	return nil
}

func validateOperatorDiagnosticNoopSuccessEvidence(tx dbTx, receipt operatorDiagnosticNoopReceipt,
	req OperatorDiagnosticNoopRequest,
) (bool, error) {
	var rows int
	err := tx.QueryRow(`SELECT COUNT(*)
	 FROM jobs j JOIN desired_state d ON d.desired_id=j.desired_id
	 WHERE j.job_id=? AND j.machine_id=? AND j.desired_id=? AND j.revision=?
	   AND j.created_at=? AND j.artifact_digest=? AND j.irreversible=0 AND j.execution_timeout=?
	   AND d.scope_type='machine' AND d.scope_id=? AND d.resource_kind=? AND d.resource_id=?
	   AND d.revision=? AND d.spec=? AND d.created_at=?`,
		receipt.JobID, receipt.MachineID, receipt.DesiredID, receipt.Revision,
		fmtTime(receipt.CreatedAt), receipt.SpecDigest, receipt.ExecutionTimeoutSeconds,
		receipt.MachineID, receipt.ResourceKind, receipt.ResourceID, receipt.Revision,
		OperatorDiagnosticNoopSpec, fmtTime(receipt.CreatedAt)).Scan(&rows)
	if err != nil {
		return false, err
	}
	if rows != 1 {
		return false, nil
	}
	return validateOperatorDiagnosticNoopAudit(tx, req, fmtTime(receipt.CreatedAt), true,
		operatorDiagnosticNoopSuccessAuditDetail(receipt))
}

func validateOperatorDiagnosticNoopAudit(tx dbTx, req OperatorDiagnosticNoopRequest,
	createdAt string, ok bool, detail string,
) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id=? AND COALESCE(reason,'')=? AND idempotency_key=? AND request_digest=?
	   AND at=? AND outcome=? AND COALESCE(detail,'')=?`,
		string(AuditDiagnosticNoop), req.MachineID, truncAudit(req.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, createdAt, outcomeOf(ok), truncAudit(detail, auditMaxReason)).Scan(&count)
	return count == 1, err
}
