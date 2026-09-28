package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	operatorDeviceSyncVersion         = "v1"
	operatorDeviceSyncOperationPrefix = "device-sync-create:v1:"
	operatorDeviceSyncCacheInvalid    = "operator device sync idempotency cache invalid；未回放結果"
	operatorDeviceSyncRejectPrefix    = "operator device sync rejection code="
)

type OperatorDeviceSyncBlocker string

const (
	OperatorDeviceSyncBlockerRetired               OperatorDeviceSyncBlocker = "machine_retired"
	OperatorDeviceSyncBlockerCapabilityUnconfirmed OperatorDeviceSyncBlocker = "device_sync_capability_unconfirmed"
	OperatorDeviceSyncBlockerExecutionUnknown      OperatorDeviceSyncBlocker = "agent_execution_unknown"
	OperatorDeviceSyncBlockerExecutionDisabled     OperatorDeviceSyncBlocker = "agent_execution_disabled"
	OperatorDeviceSyncBlockerNonterminalJob        OperatorDeviceSyncBlocker = "nonterminal_jobs"
)

type OperatorDeviceSyncImpact struct {
	Kind                        string                      `json:"kind"`
	ResourceKind                string                      `json:"resource_kind"`
	ResourceID                  string                      `json:"resource_id"`
	SpecDigest                  string                      `json:"spec_digest"`
	ExecutionTimeoutSeconds     int                         `json:"execution_timeout_seconds"`
	ChangesMachineConfiguration bool                        `json:"changes_machine_configuration"`
	CreatesDesiredState         bool                        `json:"creates_desired_state"`
	CreatesJob                  bool                        `json:"creates_job"`
	DeliveryRequiresJobsEnabled bool                        `json:"delivery_requires_jobs_enabled"`
	JobsEnabled                 *bool                       `json:"jobs_enabled"`
	DeviceSyncV1                *bool                       `json:"device_sync_v1"`
	LatestCheckinSentAt         *time.Time                  `json:"latest_checkin_sent_at"`
	LatestCheckinReceivedAt     *time.Time                  `json:"latest_checkin_received_at"`
	ActiveJobCount              int64                       `json:"active_job_count"`
	CurrentResourceRevision     deploy.Revision             `json:"current_resource_revision"`
	PlannedRevision             deploy.Revision             `json:"planned_revision"`
	Blockers                    []OperatorDeviceSyncBlocker `json:"blockers"`
}

type OperatorDeviceSyncPreviewResult struct {
	MachineID         string    `json:"machine_id"`
	DisplayName       string    `json:"display_name"`
	LifecycleRevision int64     `json:"lifecycle_revision"`
	PreviewedAt       time.Time `json:"previewed_at"`
	OperatorDeviceSyncImpact
	PreviewDigest string `json:"preview_digest"`
}

type OperatorDeviceSyncRequest struct {
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

type OperatorDeviceSyncResult struct {
	MachineID         string          `json:"machine_id"`
	DisplayName       string          `json:"display_name"`
	CreatedBy         string          `json:"created_by"`
	DesiredID         string          `json:"desired_id"`
	JobID             string          `json:"job_id"`
	Revision          deploy.Revision `json:"revision"`
	CreatedAt         time.Time       `json:"created_at"`
	LifecycleRevision int64           `json:"lifecycle_revision"`
	OperatorDeviceSyncImpact
	PreviewDigest string `json:"preview_digest"`
	Replayed      bool   `json:"replayed"`
	Audited       bool   `json:"-"`
}

type operatorDeviceSyncSnapshot struct {
	MachineID               string
	DisplayName             string
	LifecycleRevision       int64
	Retired                 bool
	JobsEnabled             *bool
	DeviceSyncV1            *bool
	LatestCheckinSentAt     *time.Time
	LatestCheckinReceivedAt *time.Time
	ActiveJobCount          int64
	CurrentResourceRevision deploy.Revision
}

type operatorDeviceSyncReceipt struct {
	SchemaVersion     string          `json:"schema_version"`
	MachineID         string          `json:"machine_id"`
	DisplayName       string          `json:"display_name"`
	CreatedBy         string          `json:"created_by"`
	DesiredID         string          `json:"desired_id"`
	JobID             string          `json:"job_id"`
	Revision          deploy.Revision `json:"revision"`
	CreatedAt         time.Time       `json:"created_at"`
	LifecycleRevision int64           `json:"lifecycle_revision"`
	OperatorDeviceSyncImpact
	PreviewDigest string `json:"preview_digest"`
}

func operatorDeviceSyncSpecDigest() string {
	sum := sha256.Sum256([]byte(model.DeviceSyncSpecJSON))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func operatorDeviceSyncOperation(machineID string) string {
	return operatorDeviceSyncOperationPrefix + machineID
}

func (s *Store) PreviewOperatorDeviceSync(machineID string, timeoutSeconds int) (OperatorDeviceSyncPreviewResult, error) {
	if !validOperatorDeviceSyncTimeout(timeoutSeconds) {
		return OperatorDeviceSyncPreviewResult{}, operatorDeviceSyncRejection(OperatorCodeBadDeviceSyncTimeout)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return OperatorDeviceSyncPreviewResult{}, fmt.Errorf("store: begin operator device sync preview: %w", err)
	}
	defer tx.Rollback()
	snapshot, err := loadOperatorDeviceSyncSnapshot(tx, machineID)
	if err != nil {
		return OperatorDeviceSyncPreviewResult{}, err
	}
	return operatorDeviceSyncPreview(snapshot, timeoutSeconds, s.now().UTC().Truncate(time.Second)), nil
}

type deviceSyncQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func loadOperatorDeviceSyncSnapshot(q deviceSyncQueryer, machineID string) (operatorDeviceSyncSnapshot, error) {
	var snapshot operatorDeviceSyncSnapshot
	rows, err := q.Query(`SELECT received_at FROM machine_checkins WHERE machine_id=?`, machineID)
	if err != nil {
		return snapshot, fmt.Errorf("store: inspect operator device sync check-in times: %w", err)
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return snapshot, fmt.Errorf("store: inspect operator device sync check-in time: %w", err)
		}
		if _, canonical := parseCanonicalOperatorDeviceSyncStoredTime(raw); !canonical {
			rows.Close()
			return snapshot, errors.New("store: device sync target projection is invalid")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return snapshot, fmt.Errorf("store: inspect operator device sync check-in times: %w", err)
	}
	if err := rows.Close(); err != nil {
		return snapshot, fmt.Errorf("store: close operator device sync check-in times: %w", err)
	}
	var retired, sentAt, receivedAt sql.NullString
	var jobsEnabled, capability sql.NullBool
	err = q.QueryRow(`
SELECT m.display_name,m.lifecycle_revision,m.retired_at,c.sent_at,c.received_at,c.jobs_enabled,cap.supported,
       (SELECT COUNT(*) FROM jobs j WHERE j.machine_id=m.machine_id AND j.state NOT IN (?,?,?,?,?)),
       COALESCE((SELECT current_revision FROM revision_counters WHERE resource_scope=?),0)
  FROM machine_registry m
  LEFT JOIN machine_checkins c ON c.rowid=(
    SELECT rowid FROM machine_checkins WHERE machine_id=m.machine_id
     ORDER BY received_at DESC,rowid DESC LIMIT 1)
  LEFT JOIN machine_job_capabilities cap
    ON cap.machine_id=c.machine_id AND cap.sent_at=c.sent_at AND cap.capability=?
 WHERE m.machine_id=?`,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention,
		model.DeviceSyncResourceKind+":"+model.DeviceSyncResourceID, model.DeviceSyncJobKind, machineID).Scan(
		&snapshot.DisplayName, &snapshot.LifecycleRevision, &retired, &sentAt, &receivedAt,
		&jobsEnabled, &capability, &snapshot.ActiveJobCount, &snapshot.CurrentResourceRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, operatorError(OperatorCodeMachineNotFound, "找不到這台機器")
	}
	if err != nil {
		return snapshot, fmt.Errorf("store: inspect operator device sync target: %w", err)
	}
	if snapshot.DisplayName == "" || snapshot.LifecycleRevision < 0 || snapshot.ActiveJobCount < 0 ||
		snapshot.CurrentResourceRevision < 0 || snapshot.CurrentResourceRevision >= deploy.Revision(math.MaxInt64) {
		return snapshot, errors.New("store: device sync target projection is invalid")
	}
	snapshot.MachineID = machineID
	snapshot.Retired = retired.Valid && retired.String != ""
	if jobsEnabled.Valid {
		value := jobsEnabled.Bool
		snapshot.JobsEnabled = &value
	}
	if capability.Valid {
		value := capability.Bool
		snapshot.DeviceSyncV1 = &value
	}
	if sentAt.Valid {
		value, canonical := parseCanonicalOperatorDeviceSyncStoredTime(sentAt.String)
		if !canonical {
			return snapshot, errors.New("store: device sync target projection is invalid")
		}
		snapshot.LatestCheckinSentAt = &value
	}
	if receivedAt.Valid {
		value, canonical := parseCanonicalOperatorDeviceSyncStoredTime(receivedAt.String)
		if !canonical {
			return snapshot, errors.New("store: device sync target projection is invalid")
		}
		snapshot.LatestCheckinReceivedAt = &value
	}
	return snapshot, nil
}

func operatorDeviceSyncPreview(snapshot operatorDeviceSyncSnapshot, timeoutSeconds int, now time.Time) OperatorDeviceSyncPreviewResult {
	impact := OperatorDeviceSyncImpact{
		Kind: model.DeviceSyncJobKind, ResourceKind: model.DeviceSyncResourceKind, ResourceID: model.DeviceSyncResourceID,
		SpecDigest: operatorDeviceSyncSpecDigest(), ExecutionTimeoutSeconds: timeoutSeconds,
		ChangesMachineConfiguration: false, CreatesDesiredState: true, CreatesJob: true,
		DeliveryRequiresJobsEnabled: true, JobsEnabled: snapshot.JobsEnabled, DeviceSyncV1: snapshot.DeviceSyncV1,
		LatestCheckinSentAt: snapshot.LatestCheckinSentAt, LatestCheckinReceivedAt: snapshot.LatestCheckinReceivedAt,
		ActiveJobCount: snapshot.ActiveJobCount, CurrentResourceRevision: snapshot.CurrentResourceRevision,
		PlannedRevision: snapshot.CurrentResourceRevision + 1, Blockers: make([]OperatorDeviceSyncBlocker, 0, 5),
	}
	if snapshot.Retired {
		impact.Blockers = append(impact.Blockers, OperatorDeviceSyncBlockerRetired)
	}
	if snapshot.DeviceSyncV1 == nil || !*snapshot.DeviceSyncV1 {
		impact.Blockers = append(impact.Blockers, OperatorDeviceSyncBlockerCapabilityUnconfirmed)
	}
	if snapshot.JobsEnabled == nil {
		impact.Blockers = append(impact.Blockers, OperatorDeviceSyncBlockerExecutionUnknown)
	} else if !*snapshot.JobsEnabled {
		impact.Blockers = append(impact.Blockers, OperatorDeviceSyncBlockerExecutionDisabled)
	}
	if snapshot.ActiveJobCount > 0 {
		impact.Blockers = append(impact.Blockers, OperatorDeviceSyncBlockerNonterminalJob)
	}
	result := OperatorDeviceSyncPreviewResult{
		MachineID: snapshot.MachineID, DisplayName: snapshot.DisplayName,
		LifecycleRevision: snapshot.LifecycleRevision, PreviewedAt: now, OperatorDeviceSyncImpact: impact,
	}
	result.PreviewDigest = operatorDeviceSyncPreviewDigest(snapshot, impact)
	return result
}

func operatorDeviceSyncPreviewDigest(snapshot operatorDeviceSyncSnapshot, impact OperatorDeviceSyncImpact) string {
	body := struct {
		Version           string                   `json:"version"`
		MachineID         string                   `json:"machine_id"`
		DisplayName       string                   `json:"display_name"`
		LifecycleRevision int64                    `json:"lifecycle_revision"`
		Retired           bool                     `json:"retired"`
		Impact            OperatorDeviceSyncImpact `json:"impact"`
	}{operatorDeviceSyncVersion, snapshot.MachineID, snapshot.DisplayName, snapshot.LifecycleRevision, snapshot.Retired, impact}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validOperatorDeviceSyncTimeout(seconds int) bool {
	return seconds >= model.DeviceSyncMinTimeoutSeconds && seconds <= model.DeviceSyncMaxTimeoutSeconds
}

func operatorDeviceSyncRejection(code string) *OperatorRequestError {
	detail, ok := canonicalOperatorDeviceSyncRejectionDetail(code)
	if !ok {
		return operatorError(code, "同步裝置資料 request 被拒絕")
	}
	return operatorError(code, detail)
}

func canonicalOperatorDeviceSyncRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeBadDeviceSyncTimeout:
		return fmt.Sprintf("execution_timeout_seconds 必須介於 %d 與 %d", model.DeviceSyncMinTimeoutSeconds, model.DeviceSyncMaxTimeoutSeconds), true
	case OperatorCodeMachineNotFound:
		return "找不到這台機器", true
	case OperatorCodeConfirmationMismatch:
		return "confirm_display_name 必須與目前顯示名稱逐字相同", true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodeDeviceSyncPreviewStale:
		return "機器、最新 check-in、工作單占用或 device:sync revision 已變更；請重新預覽", true
	case OperatorCodeMachineRetired:
		return "機器已退役，不建立同步工作單", true
	case OperatorCodeDeviceSyncCapabilityUnconfirmed:
		return "最新 check-in 未確認 device-sync v1；等待支援此能力的 agent 回報後重新預覽", true
	case OperatorCodeAgentExecutionUnknown:
		return "agent 尚未回報工作單執行狀態；等待下一次 check-in 後重新預覽", true
	case OperatorCodeAgentExecutionDisabled:
		return "agent 工作單執行未啟用；將 jobs_enabled 設為 true 並重新啟動 clawctl-agent", true
	case OperatorCodeMachineActiveJob:
		return "機器已有未終態 job；先讓工作單結束再重新預覽", true
	case OperatorCodeReasonRequired:
		return "reason 不可省略或只含空白", true
	default:
		return "", false
	}
}

func historicalOperatorDeviceSyncRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeBadDeviceSyncTimeout:
		return "原 request 的 execution_timeout_seconds 不符合 device-sync policy", true
	case OperatorCodeMachineNotFound:
		return "原 request 當時找不到目標機器", true
	case OperatorCodeConfirmationMismatch:
		return "原 request 的 typed confirmation 當時不符", true
	case OperatorCodePreviewRequired:
		return "原 request 當時缺少必要的 preview_digest", true
	case OperatorCodeDeviceSyncPreviewStale:
		return "原 request 的 preview_digest 當時已失效", true
	case OperatorCodeMachineRetired:
		return "原 request 的目標機器當時已退役", true
	case OperatorCodeDeviceSyncCapabilityUnconfirmed:
		return "原 request 的最新 check-in 當時未確認 device-sync v1", true
	case OperatorCodeAgentExecutionUnknown:
		return "原 request 的 agent 當時尚未回報工作單執行狀態", true
	case OperatorCodeAgentExecutionDisabled:
		return "原 request 的 agent 當時未啟用工作單執行", true
	case OperatorCodeMachineActiveJob:
		return "原 request 的目標機器當時已有未終態 job", true
	case OperatorCodeReasonRequired:
		return "原 request 當時缺少操作理由", true
	default:
		return "", false
	}
}

func operatorDeviceSyncRejectionAuditDetail(code, detail string) string {
	return truncAudit(operatorDeviceSyncRejectPrefix+code+"；"+detail, auditMaxReason)
}

// ApplyOperatorDeviceSync atomically creates the fixed desired state, job,
// immutable idempotency receipt and audit evidence. It has no transport route;
// callers must first obtain a preview from the same Store authority.
func (s *Store) ApplyOperatorDeviceSync(req OperatorDeviceSyncRequest) (OperatorDeviceSyncResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorDeviceSyncResult{}, operatorError(OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if !validArtifactFetchDigest(req.RequestDigest) {
		return OperatorDeviceSyncResult{}, operatorError(OperatorCodeRequestDigestRequired, "request body digest 必須是 canonical sha256")
	}
	audit := req.Audit
	audit.Action, audit.MachineID, audit.Subject = AuditDeviceSync, req.MachineID, req.MachineID
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = req.Reason, req.IdempotencyKey, req.RequestDigest
	tx, err := s.db.Begin()
	if err != nil {
		return OperatorDeviceSyncResult{}, fmt.Errorf("store: begin operator device sync: %w", err)
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
		return s.replayOperatorDeviceSync(tx, req, audit, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorDeviceSyncResult{}, fmt.Errorf("store: inspect device sync idempotency key: %w", err)
	}

	reject := func(code string) (OperatorDeviceSyncResult, error) {
		detail, ok := canonicalOperatorDeviceSyncRejectionDetail(code)
		if !ok {
			return OperatorDeviceSyncResult{}, errors.New("store: invalid device sync rejection code")
		}
		audit.OK, audit.Detail = false, operatorDeviceSyncRejectionAuditDetail(code, detail)
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorDeviceSyncResult{}, fmt.Errorf("store: record device sync rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorDeviceSyncOperation(req.MachineID),
			req.RequestDigest, code, detail, fmtTime(writerNow)); err != nil {
			return OperatorDeviceSyncResult{}, fmt.Errorf("store: persist device sync rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorDeviceSyncResult{}, fmt.Errorf("store: commit device sync rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorDeviceSyncResult{}, rejection
	}

	if !validOperatorDeviceSyncTimeout(req.ExecutionTimeout) {
		return reject(OperatorCodeBadDeviceSyncTimeout)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return reject(OperatorCodeReasonRequired)
	}
	if strings.TrimSpace(req.PreviewDigest) == "" {
		return reject(OperatorCodePreviewRequired)
	}
	snapshot, err := loadOperatorDeviceSyncSnapshot(tx, req.MachineID)
	if err != nil {
		var rejection *OperatorRequestError
		if errors.As(err, &rejection) && rejection.Code == OperatorCodeMachineNotFound {
			return reject(rejection.Code)
		}
		return OperatorDeviceSyncResult{}, err
	}
	audit.Subject = snapshot.DisplayName
	if req.ConfirmDisplayName != snapshot.DisplayName {
		return reject(OperatorCodeConfirmationMismatch)
	}
	preview := operatorDeviceSyncPreview(snapshot, req.ExecutionTimeout, writerNow)
	if req.PreviewDigest != preview.PreviewDigest {
		return reject(OperatorCodeDeviceSyncPreviewStale)
	}
	for _, blocker := range preview.Blockers {
		switch blocker {
		case OperatorDeviceSyncBlockerRetired:
			return reject(OperatorCodeMachineRetired)
		case OperatorDeviceSyncBlockerCapabilityUnconfirmed:
			return reject(OperatorCodeDeviceSyncCapabilityUnconfirmed)
		case OperatorDeviceSyncBlockerExecutionUnknown:
			return reject(OperatorCodeAgentExecutionUnknown)
		case OperatorDeviceSyncBlockerExecutionDisabled:
			return reject(OperatorCodeAgentExecutionDisabled)
		case OperatorDeviceSyncBlockerNonterminalJob:
			return reject(OperatorCodeMachineActiveJob)
		default:
			return OperatorDeviceSyncResult{}, errors.New("store: unrecognized device sync blocker")
		}
	}
	if strings.TrimSpace(req.CreatedBy) == "" || len(req.CreatedBy) > auditMaxReason {
		return OperatorDeviceSyncResult{}, errors.New("store: device sync creator provenance is invalid")
	}
	desiredID, revision, err := createDesiredStateTx(tx, "machine", snapshot.MachineID,
		model.DeviceSyncResourceKind, model.DeviceSyncResourceID, model.DeviceSyncSpecJSON, req.CreatedBy, writerNow)
	if err != nil {
		return OperatorDeviceSyncResult{}, err
	}
	if revision != preview.PlannedRevision {
		return OperatorDeviceSyncResult{}, errors.New("store: device sync revision changed during transaction")
	}
	jobID, err := createJobTx(tx, snapshot.MachineID, desiredID, revision, NewJob{
		ArtifactDigest: operatorDeviceSyncSpecDigest(), ExecutionTimeout: req.ExecutionTimeout,
	}, writerNow)
	if err != nil {
		return OperatorDeviceSyncResult{}, fmt.Errorf("store: create device sync job: %w", err)
	}
	receipt := operatorDeviceSyncReceipt{
		SchemaVersion: operatorDeviceSyncVersion, MachineID: snapshot.MachineID, DisplayName: snapshot.DisplayName,
		CreatedBy: req.CreatedBy, DesiredID: desiredID, JobID: jobID, Revision: revision, CreatedAt: writerNow,
		LifecycleRevision: snapshot.LifecycleRevision, OperatorDeviceSyncImpact: preview.OperatorDeviceSyncImpact,
		PreviewDigest: req.PreviewDigest,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorDeviceSyncResult{}, fmt.Errorf("store: encode device sync receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorDeviceSyncOperation(req.MachineID),
		req.RequestDigest, string(raw), fmtTime(writerNow)); err != nil {
		return OperatorDeviceSyncResult{}, fmt.Errorf("store: persist device sync receipt: %w", err)
	}
	audit.OK, audit.Detail = true, operatorDeviceSyncSuccessAuditDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorDeviceSyncResult{}, fmt.Errorf("store: record device sync success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorDeviceSyncResult{}, fmt.Errorf("store: commit operator device sync: %w", err)
	}
	result := operatorDeviceSyncResult(receipt)
	result.Audited = true
	return result, nil
}

func operatorDeviceSyncSuccessAuditDetail(receipt operatorDeviceSyncReceipt) string {
	raw, _ := json.Marshal(receipt)
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("device sync job=%s desired=%s revision=%d timeout=%ds config_changed=false capability=device-sync-v1 receipt_sha256=%x",
		receipt.JobID, receipt.DesiredID, receipt.Revision, receipt.ExecutionTimeoutSeconds, sum)
}

func operatorDeviceSyncResult(receipt operatorDeviceSyncReceipt) OperatorDeviceSyncResult {
	return OperatorDeviceSyncResult{
		MachineID: receipt.MachineID, DisplayName: receipt.DisplayName, CreatedBy: receipt.CreatedBy, DesiredID: receipt.DesiredID,
		JobID: receipt.JobID, Revision: receipt.Revision, CreatedAt: receipt.CreatedAt,
		LifecycleRevision: receipt.LifecycleRevision, OperatorDeviceSyncImpact: receipt.OperatorDeviceSyncImpact,
		PreviewDigest: receipt.PreviewDigest,
	}
}

func (s *Store) replayOperatorDeviceSync(tx *sql.Tx, req OperatorDeviceSyncRequest, audit AuditEntry,
	cached operatorCachedRequest,
) (OperatorDeviceSyncResult, error) {
	if cached.Operation != operatorDeviceSyncOperation(req.MachineID) || cached.Digest != req.RequestDigest {
		audit.OK, audit.Detail = false, "idempotency conflict：idempotency key 已被不同的 operation 或 canonical request body 使用"
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorDeviceSyncResult{}, fmt.Errorf("store: record device sync idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorDeviceSyncResult{}, fmt.Errorf("store: commit device sync idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, "idempotency key 已被不同的 operation 或 canonical request body 使用")
		rejection.Audited = true
		return OperatorDeviceSyncResult{}, rejection
	}
	if !canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return OperatorDeviceSyncResult{}, errors.New(operatorDeviceSyncCacheInvalid)
	}
	if cached.Outcome == "rejected" {
		if cached.ResponseJSON.Valid || !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid {
			return OperatorDeviceSyncResult{}, errors.New(operatorDeviceSyncCacheInvalid)
		}
		canonical, ok := canonicalOperatorDeviceSyncRejectionDetail(cached.ErrorCode.String)
		historical, historicalOK := historicalOperatorDeviceSyncRejectionDetail(cached.ErrorCode.String)
		if !ok || !historicalOK || cached.ErrorDetail.String != canonical {
			return OperatorDeviceSyncResult{}, errors.New(operatorDeviceSyncCacheInvalid)
		}
		valid, err := validateOperatorDeviceSyncAudit(tx, req, cached.CreatedAt, "", false,
			operatorDeviceSyncRejectionAuditDetail(cached.ErrorCode.String, canonical))
		if err != nil || !valid {
			return OperatorDeviceSyncResult{}, errors.New(operatorDeviceSyncCacheInvalid)
		}
		audit.OK, audit.Detail = false, OperatorIdempotencyReplayPrefix+"原判決："+historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorDeviceSyncResult{}, fmt.Errorf("store: record rejected device sync replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorDeviceSyncResult{}, fmt.Errorf("store: commit rejected device sync replay audit: %w", err)
		}
		return OperatorDeviceSyncResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid || cached.ErrorDetail.Valid {
		return OperatorDeviceSyncResult{}, errors.New(operatorDeviceSyncCacheInvalid)
	}
	receipt, err := decodeOperatorDeviceSyncReceipt(cached.ResponseJSON.String)
	if err != nil || validateOperatorDeviceSyncReceipt(receipt, req, cached.CreatedAt) != nil {
		return OperatorDeviceSyncResult{}, errors.New(operatorDeviceSyncCacheInvalid)
	}
	valid, err := validateOperatorDeviceSyncSuccessEvidence(tx, receipt, req)
	if err != nil || !valid {
		return OperatorDeviceSyncResult{}, errors.New(operatorDeviceSyncCacheInvalid)
	}
	audit.Subject, audit.OK = receipt.DisplayName, true
	audit.Detail = OperatorIdempotencyReplayPrefix + "沒有再次建立 desired state、job 或 revision"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorDeviceSyncResult{}, fmt.Errorf("store: record successful device sync replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorDeviceSyncResult{}, fmt.Errorf("store: commit successful device sync replay audit: %w", err)
	}
	result := operatorDeviceSyncResult(receipt)
	result.Replayed, result.Audited = true, true
	return result, nil
}

func decodeOperatorDeviceSyncReceipt(raw string) (operatorDeviceSyncReceipt, error) {
	var receipt operatorDeviceSyncReceipt
	if err := rejectDuplicateOperatorDeviceSyncReceiptFields(raw); err != nil {
		return receipt, err
	}
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&receipt); err != nil {
		return receipt, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return receipt, errors.New("cached device sync receipt has trailing JSON")
	}
	canonical, err := json.Marshal(receipt)
	if err != nil {
		return receipt, fmt.Errorf("encode canonical device sync receipt: %w", err)
	}
	if string(canonical) != raw {
		return receipt, errors.New("cached device sync receipt is not canonical JSON")
	}
	return receipt, nil
}

func rejectDuplicateOperatorDeviceSyncReceiptFields(raw string) error {
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	opening, err := dec.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("cached device sync receipt is not an object")
	}
	seen := make(map[string]struct{}, 32)
	for dec.More() {
		nameToken, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := nameToken.(string)
		if !ok {
			return errors.New("cached device sync receipt has an invalid field name")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("cached device sync receipt has duplicate field %q", name)
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
	}
	if closing, err := dec.Token(); err != nil || closing != json.Delim('}') {
		return errors.New("cached device sync receipt has invalid closing token")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("cached device sync receipt has trailing JSON")
	}
	return nil
}

func validateOperatorDeviceSyncReceipt(receipt operatorDeviceSyncReceipt, req OperatorDeviceSyncRequest,
	cachedCreatedAt string,
) error {
	if receipt.SchemaVersion != operatorDeviceSyncVersion || receipt.MachineID != req.MachineID ||
		receipt.MachineID == "" || receipt.DisplayName == "" || receipt.DisplayName != req.ConfirmDisplayName ||
		strings.TrimSpace(receipt.CreatedBy) == "" || receipt.CreatedBy != req.CreatedBy ||
		len(receipt.CreatedBy) > auditMaxReason || receipt.DesiredID == "" || receipt.JobID == "" ||
		receipt.Revision <= 0 || receipt.LifecycleRevision < 0 || receipt.PreviewDigest != req.PreviewDigest ||
		!canonicalOperatorTime(receipt.CreatedAt) || fmtTime(receipt.CreatedAt) != cachedCreatedAt ||
		receipt.Kind != model.DeviceSyncJobKind || receipt.ResourceKind != model.DeviceSyncResourceKind ||
		receipt.ResourceID != model.DeviceSyncResourceID || receipt.SpecDigest != operatorDeviceSyncSpecDigest() ||
		receipt.ExecutionTimeoutSeconds != req.ExecutionTimeout || receipt.ChangesMachineConfiguration ||
		!receipt.CreatesDesiredState || !receipt.CreatesJob || !receipt.DeliveryRequiresJobsEnabled ||
		receipt.JobsEnabled == nil || !*receipt.JobsEnabled || receipt.DeviceSyncV1 == nil || !*receipt.DeviceSyncV1 ||
		receipt.LatestCheckinSentAt == nil || !canonicalOperatorTime(*receipt.LatestCheckinSentAt) ||
		receipt.LatestCheckinReceivedAt == nil || !canonicalOperatorTime(*receipt.LatestCheckinReceivedAt) ||
		receipt.LatestCheckinReceivedAt.After(receipt.CreatedAt) ||
		receipt.ActiveJobCount != 0 || receipt.CurrentResourceRevision+1 != receipt.PlannedRevision ||
		receipt.PlannedRevision != receipt.Revision || receipt.Blockers == nil || len(receipt.Blockers) != 0 {
		return errors.New("cached device sync receipt fields are invalid")
	}
	previewSnapshot := operatorDeviceSyncSnapshot{
		MachineID: receipt.MachineID, DisplayName: receipt.DisplayName,
		LifecycleRevision: receipt.LifecycleRevision, JobsEnabled: receipt.JobsEnabled,
		DeviceSyncV1: receipt.DeviceSyncV1, LatestCheckinSentAt: receipt.LatestCheckinSentAt,
		LatestCheckinReceivedAt: receipt.LatestCheckinReceivedAt, ActiveJobCount: receipt.ActiveJobCount,
		CurrentResourceRevision: receipt.CurrentResourceRevision,
	}
	if operatorDeviceSyncPreviewDigest(previewSnapshot, receipt.OperatorDeviceSyncImpact) != receipt.PreviewDigest {
		return errors.New("cached device sync receipt preview facts are invalid")
	}
	return nil
}

func validateOperatorDeviceSyncSuccessEvidence(tx *sql.Tx, receipt operatorDeviceSyncReceipt,
	req OperatorDeviceSyncRequest,
) (bool, error) {
	var rows int
	err := tx.QueryRow(`SELECT COUNT(*)
	 FROM jobs j JOIN desired_state d ON d.desired_id=j.desired_id
	 WHERE j.job_id=? AND j.machine_id=? AND j.desired_id=? AND j.revision=?
	   AND j.created_at=? AND j.artifact_digest=? AND j.irreversible=0 AND j.execution_timeout=?
	   AND ((j.state=? AND j.lease_token IS NULL AND j.lease_expires_at IS NULL AND j.terminal_at IS NULL)
	     OR (j.state IN (?,?,?) AND j.lease_token IS NOT NULL AND j.lease_token<>''
	         AND j.lease_expires_at IS NOT NULL AND j.lease_expires_at<>'' AND j.terminal_at IS NULL)
	     OR (j.state IN (?,?,?,?) AND j.lease_token IS NULL AND j.lease_expires_at IS NULL
	         AND j.terminal_at IS NOT NULL AND j.terminal_at<>''))
	   AND d.scope_type='machine' AND d.scope_id=? AND d.resource_kind=? AND d.resource_id=?
	   AND d.revision=? AND d.spec=? AND d.created_at=? AND d.created_by=?
	   AND EXISTS (SELECT 1 FROM machine_registry machine
	                WHERE machine.machine_id=? AND machine.lifecycle_revision>=?)
	   AND EXISTS (SELECT 1 FROM revision_counters counter
	                WHERE counter.resource_scope=? AND counter.current_revision>=?)
	   AND NOT EXISTS (SELECT 1 FROM desired_state other_desired
	                    WHERE other_desired.resource_kind=d.resource_kind
	                      AND other_desired.resource_id=d.resource_id
	                      AND other_desired.revision=d.revision
	                      AND other_desired.desired_id<>d.desired_id)
	   AND NOT EXISTS (SELECT 1 FROM jobs other_job
	                    WHERE other_job.desired_id=d.desired_id AND other_job.job_id<>j.job_id)
	   AND NOT EXISTS (SELECT 1 FROM job_dependencies dependency
	                   WHERE dependency.job_id=j.job_id OR dependency.prerequisite_job_id=j.job_id)`,
		receipt.JobID, receipt.MachineID, receipt.DesiredID, receipt.Revision, fmtTime(receipt.CreatedAt),
		receipt.SpecDigest, receipt.ExecutionTimeoutSeconds,
		deploy.NotStarted, deploy.Claimed, deploy.Running, deploy.Verifying, deploy.Succeeded,
		deploy.Failed, deploy.Rejected, deploy.LeaseExpired,
		receipt.MachineID, receipt.ResourceKind,
		receipt.ResourceID, receipt.Revision, model.DeviceSyncSpecJSON, fmtTime(receipt.CreatedAt), receipt.CreatedBy,
		receipt.MachineID, receipt.LifecycleRevision,
		receipt.ResourceKind+":"+receipt.ResourceID, receipt.Revision).Scan(&rows)
	if err != nil || rows != 1 {
		return false, err
	}
	validLifecycle, err := validateOperatorDeviceSyncJobLifecycle(tx, receipt.JobID, receipt.CreatedAt)
	if err != nil || !validLifecycle {
		return false, err
	}
	return validateOperatorDeviceSyncAudit(tx, req, fmtTime(receipt.CreatedAt), receipt.DisplayName, true,
		operatorDeviceSyncSuccessAuditDetail(receipt))
}

func validateOperatorDeviceSyncJobLifecycle(tx *sql.Tx, jobID string, createdAt time.Time) (bool, error) {
	var state deploy.JobState
	var leaseToken, leaseExpiresAt, terminalAt sql.NullString
	if err := tx.QueryRow(`SELECT state,lease_token,lease_expires_at,terminal_at FROM jobs WHERE job_id=?`, jobID).
		Scan(&state, &leaseToken, &leaseExpiresAt, &terminalAt); err != nil {
		return false, err
	}
	switch state {
	case deploy.NotStarted:
		if leaseToken.Valid || leaseExpiresAt.Valid || terminalAt.Valid {
			return false, nil
		}
		// ⚠ 未開始的工作單若帶有 executor 執行證據，會讓互相矛盾的帳本被當成可信依據。
		var count int
		err := tx.QueryRow(`SELECT COUNT(*) FROM jobs job
		 WHERE job.job_id=?
		   AND NOT EXISTS (SELECT 1 FROM job_events event WHERE event.job_id=job.job_id)
		   AND NOT EXISTS (SELECT 1 FROM verification_results verification
		                   WHERE verification.job_id=job.job_id
		                     AND verification.evidence_role=?)`, jobID,
			JobVerificationRoleExecutor).Scan(&count)
		if err != nil || count != 1 {
			return false, err
		}
		return true, nil
	case deploy.Claimed, deploy.Running, deploy.Verifying:
		if !leaseToken.Valid || !canonicalOperatorDeviceSyncLeaseToken(leaseToken.String) ||
			!leaseExpiresAt.Valid || terminalAt.Valid {
			return false, nil
		}
		leaseExpiry, canonical := parseCanonicalOperatorDeviceSyncStoredTime(leaseExpiresAt.String)
		return canonical && leaseExpiry.After(createdAt), nil
	// device-sync is fixed irreversible=false, so its failure verdict is Failed;
	// ManualIntervention would claim an irreversible effect this primitive cannot make.
	case deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired:
		if leaseToken.Valid || leaseExpiresAt.Valid || !terminalAt.Valid {
			return false, nil
		}
		terminalTime, canonical := parseCanonicalOperatorDeviceSyncStoredTime(terminalAt.String)
		if !canonical || terminalTime.Before(createdAt) {
			return false, nil
		}
		if state == deploy.Succeeded {
			return validateOperatorDeviceSyncSucceededVerification(tx, jobID, createdAt, terminalTime)
		}
		return true, nil
	default:
		return false, nil
	}
}

func validateOperatorDeviceSyncSucceededVerification(tx *sql.Tx, jobID string,
	createdAt, terminalAt time.Time,
) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM jobs job
	 WHERE job.job_id=?
	   AND EXISTS (SELECT 1 FROM verification_results verification
	               WHERE verification.job_id=job.job_id
	                 AND verification.machine_id=job.machine_id
	                 AND verification.producer_kind=?
	                 AND verification.evidence_role=?
	                 AND verification.authority=?
	                 AND verification.producer_id=job.machine_id
	                 AND verification.provenance_recorded=1
	                 AND verification.received_at<>''
	                 AND verification.rule_id=?
	                 AND verification.command=?
	                 AND verification.exit_code=0
	                 AND verification.stdout_excerpt=?
	                 AND verification.stderr_excerpt=''
	                 AND verification.passed=1
	                 AND verification.observed_digest=''
	                 AND verification.observed_version=''
	                 AND verification.verifier_id='')
	   AND NOT EXISTS (SELECT 1 FROM verification_results verification
	                   WHERE verification.job_id=job.job_id
	                     AND verification.evidence_role=?
	                     AND (verification.machine_id<>job.machine_id
	                       OR verification.producer_kind<>?
	                       OR verification.authority<>?
	                       OR verification.producer_id<>job.machine_id
	                       OR verification.provenance_recorded<>1
	                       OR verification.received_at=''
	                       OR verification.rule_id<>?
	                       OR verification.command<>?
	                       OR verification.exit_code IS NULL OR verification.exit_code<>0
	                       OR verification.stdout_excerpt IS NULL OR verification.stdout_excerpt<>?
	                       OR verification.stderr_excerpt IS NULL OR verification.stderr_excerpt<>''
	                       OR verification.passed<>1
	                       OR verification.observed_digest<>''
	                       OR verification.observed_version<>''
	                       OR verification.verifier_id<>''))`, jobID,
		JobVerificationProducerExecutorAgent, JobVerificationRoleExecutor,
		JobVerificationAuthorityMachineLease, model.DeviceSyncVerificationRuleID,
		model.DeviceSyncVerificationCommand, model.DeviceSyncVerificationStdout,
		JobVerificationRoleExecutor, JobVerificationProducerExecutorAgent,
		JobVerificationAuthorityMachineLease, model.DeviceSyncVerificationRuleID,
		model.DeviceSyncVerificationCommand, model.DeviceSyncVerificationStdout).Scan(&count)
	if err != nil || count != 1 {
		return false, err
	}
	rows, err := tx.Query(`SELECT verification_id,verified_at,received_at FROM verification_results
	 WHERE job_id=? AND evidence_role=?`, jobID, JobVerificationRoleExecutor)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	executorEvidenceCount := 0
	var verificationReceivedAt time.Time
	for rows.Next() {
		var verificationID, verifiedAtRaw, receivedAtRaw string
		if err := rows.Scan(&verificationID, &verifiedAtRaw, &receivedAtRaw); err != nil {
			return false, err
		}
		_, verifiedCanonical := parseCanonicalOperatorDeviceSyncStoredTime(verifiedAtRaw)
		receivedAt, receivedCanonical := parseCanonicalOperatorDeviceSyncStoredTime(receivedAtRaw)
		if !validJobReadStoredIdentifier(verificationID, 256) || !verifiedCanonical || !receivedCanonical ||
			receivedAt.Before(createdAt) || receivedAt.After(terminalAt) {
			return false, nil
		}
		verificationReceivedAt = receivedAt
		executorEvidenceCount++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if executorEvidenceCount != 1 {
		return false, nil
	}
	return validateOperatorDeviceSyncSucceededEvents(tx, jobID, createdAt, terminalAt, verificationReceivedAt)
}

func validateOperatorDeviceSyncSucceededEvents(tx *sql.Tx, jobID string,
	createdAt, terminalAt, verificationReceivedAt time.Time,
) (bool, error) {
	rows, err := tx.Query(`SELECT event.event_id,event.seq,event.phase,event.payload,event.occurred_at,event.received_at,
	 event.producer_kind,event.producer_id,event.evidence_role,event.authority,event.provenance_recorded,
	 job.machine_id
	 FROM job_events event JOIN jobs job ON job.job_id=event.job_id
	 WHERE event.job_id=? ORDER BY event.seq`, jobID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	expectedPhases := [...]string{"start", "finish"}
	position := 0
	for rows.Next() {
		if position >= len(expectedPhases) {
			return false, nil
		}
		var seq int
		var eventID, phase, payload, occurredAtRaw, receivedAtRaw string
		var producerKind, producerID, evidenceRole, authority, machineID string
		var provenanceRecorded bool
		if err := rows.Scan(&eventID, &seq, &phase, &payload, &occurredAtRaw, &receivedAtRaw,
			&producerKind, &producerID, &evidenceRole, &authority, &provenanceRecorded, &machineID); err != nil {
			return false, err
		}
		_, occurredCanonical := parseCanonicalOperatorDeviceSyncStoredTime(occurredAtRaw)
		receivedAt, receivedCanonical := parseCanonicalOperatorDeviceSyncStoredTime(receivedAtRaw)
		if !validJobReadStoredIdentifier(eventID, 256) || seq != position+1 ||
			phase != expectedPhases[position] || payload != `{}` ||
			producerKind != JobEventProducerExecutorAgent || evidenceRole != JobEventRoleExecutor ||
			authority != JobEventAuthorityMachineLease || producerID != machineID || !provenanceRecorded ||
			!occurredCanonical || !receivedCanonical || receivedAt.Before(createdAt) || receivedAt.After(terminalAt) ||
			position == 0 && receivedAt.After(verificationReceivedAt) ||
			position == 1 && receivedAt.Before(verificationReceivedAt) {
			return false, nil
		}
		position++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return position == len(expectedPhases), nil
}

func canonicalOperatorDeviceSyncLeaseToken(raw string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == raw
}

func parseCanonicalOperatorDeviceSyncStoredTime(raw string) (time.Time, bool) {
	value, err := time.Parse(time.RFC3339, raw)
	return value, err == nil && raw == fmtTime(value) && canonicalOperatorTime(value)
}

func validateOperatorDeviceSyncAudit(tx *sql.Tx, req OperatorDeviceSyncRequest, createdAt, subject string,
	ok bool, detail string,
) (bool, error) {
	sourceAddr := req.Audit.SourceAddr
	if sourceAddr == "" {
		sourceAddr = "(呼叫端沒有傳來源位址)"
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id=? AND COALESCE(reason,'')=? AND idempotency_key=? AND request_digest=?
	   AND at=? AND (?='' OR subject=?) AND outcome=? AND COALESCE(detail,'')=?
	   AND source_addr=? AND COALESCE(who_node,'')=? AND COALESCE(who_user,'')=?
	   AND COALESCE(who_unavailable,'')=? AND COALESCE(user_agent,'')=?
	   AND COALESCE(auth_subject,'')=? AND COALESCE(auth_node_id,'')=?
	   AND COALESCE(auth_capability,'')=? AND COALESCE(auth_method,'')=?
	   AND COALESCE(auth_decision,'')=? AND COALESCE(boundary_decision,'')=?
	   AND COALESCE(source_kind,'')=?`,
		string(AuditDeviceSync), req.MachineID, truncAudit(req.Reason, auditMaxReason), req.IdempotencyKey,
		req.RequestDigest, createdAt, subject, truncAudit(subject, auditMaxReason), outcomeOf(ok),
		truncAudit(detail, auditMaxReason), sourceAddr, req.Audit.WhoNode, req.Audit.WhoUser,
		req.Audit.WhoUnavailable, truncAudit(req.Audit.UserAgent, 200), req.Audit.AuthSubject,
		req.Audit.AuthNodeID, req.Audit.AuthCapability, req.Audit.AuthMethod, req.Audit.AuthDecision,
		req.Audit.BoundaryDecision, req.Audit.SourceKind).Scan(&count)
	return count == 1, err
}
