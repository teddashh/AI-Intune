package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	RestoreDrillReadSchemaVersion = 1
	RestoreDrillReadConsistency   = "live"
	DefaultRestoreDrillReadLimit  = 20
	MaxRestoreDrillReadLimit      = 100
	RestoreDrillMaxErrorBytes     = 1000

	operatorRestoreDrillOperation      = "maintenance-restore-drill-enqueue:v1"
	operatorRestoreDrillReceiptVersion = "v1"
	restoreDrillRejectPrefix           = "operator restore drill rejection code="
	restoreDrillCacheInvalid           = "operator restore drill idempotency cache invalid；未回放 operation"
)

type RestoreDrillState string

const (
	RestoreDrillQueued    RestoreDrillState = "queued"
	RestoreDrillRunning   RestoreDrillState = "running"
	RestoreDrillSucceeded RestoreDrillState = "succeeded"
	RestoreDrillFailed    RestoreDrillState = "failed"
)

type RestoreDrillPhase string

const (
	RestoreDrillPhaseQueued    RestoreDrillPhase = "queued"
	RestoreDrillPhaseVerifying RestoreDrillPhase = "verifying"
	RestoreDrillPhaseComplete  RestoreDrillPhase = "complete"
)

var (
	ErrRestoreDrillNotFound     = errors.New("store: restore drill operation not found")
	ErrRestoreDrillInvalidState = errors.New("store: restore drill operation state transition is invalid")
	ErrRestoreDrillClaimLost    = errors.New("store: restore drill worker claim is no longer current")
	ErrRestoreDrillCorrupt      = errors.New("store: restore drill operation row is corrupt")
	ErrRestoreDrillCacheInvalid = errors.New("store: restore drill idempotency cache is invalid")
)

type RestoreDrillBackup struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
	SHA256     string    `json:"sha256"`
}

type RestoreDrillOperation struct {
	OperationID           string             `json:"operation_id"`
	Backup                RestoreDrillBackup `json:"backup"`
	PreviewDigest         string             `json:"preview_digest"`
	LiveExpectedAtPreview int                `json:"live_expected_at_preview"`
	State                 RestoreDrillState  `json:"state"`
	Phase                 RestoreDrillPhase  `json:"phase"`
	Attempt               int64              `json:"attempt"`
	Machines              *int               `json:"machines"`
	Expected              *int               `json:"expected"`
	LiveExpected          *int               `json:"live_expected"`
	NewestCheckinAt       *time.Time         `json:"newest_checkin_at"`
	DurationMilliseconds  *int64             `json:"duration_milliseconds"`
	ErrorCode             *string            `json:"error_code"`
	ErrorDetail           *string            `json:"error_detail"`
	CreatedAt             time.Time          `json:"created_at"`
	UpdatedAt             time.Time          `json:"updated_at"`
	StartedAt             *time.Time         `json:"started_at"`
	FinishedAt            *time.Time         `json:"finished_at"`
}

type RestoreDrillListResult struct {
	SchemaVersion int                     `json:"schema_version"`
	Consistency   string                  `json:"consistency"`
	EvaluatedAt   time.Time               `json:"evaluated_at"`
	Total         int                     `json:"total"`
	Items         []RestoreDrillOperation `json:"items"`
}

type RestoreDrillPrepared struct {
	Backup                RestoreDrillBackup
	LiveExpectedAtPreview int
	CurrentPreviewDigest  string
}

type OperatorRestoreDrillRequest struct {
	PreviewDigest  string
	Confirm        string
	Reason         string
	IdempotencyKey string
	RequestDigest  string
	Prepared       RestoreDrillPrepared
	Audit          AuditEntry
}

type OperatorRestoreDrillResult struct {
	Operation RestoreDrillOperation `json:"operation"`
	Replayed  bool                  `json:"replayed"`
	Audited   bool                  `json:"-"`
}

type RestoreDrillClaim struct {
	Operation RestoreDrillOperation
	RunToken  string
}

type RestoreDrillWorkerResult struct {
	Backup               RestoreDrillBackup
	Machines             int
	Expected             int
	LiveExpected         int
	NewestCheckinAt      *time.Time
	DurationMilliseconds int64
}

type operatorRestoreDrillReceipt struct {
	ReceiptVersion        string             `json:"receipt_version"`
	OperationID           string             `json:"operation_id"`
	Backup                RestoreDrillBackup `json:"backup"`
	PreviewDigest         string             `json:"preview_digest"`
	LiveExpectedAtPreview int                `json:"live_expected_at_preview"`
	CreatedAt             time.Time          `json:"created_at"`
}

type restoreDrillRecord struct {
	Operation      RestoreDrillOperation
	IdempotencyKey string
	RequestDigest  string
	RunToken       string
}

const restoreDrillColumns = `operation_id,idempotency_key,request_digest,backup_name,backup_size_bytes,
 backup_modified_at,backup_sha256,preview_digest,live_expected_at_preview,state,phase,attempt,run_token,
 machines,expected,live_expected,newest_checkin_at,duration_milliseconds,error_code,error_detail,
 created_at,updated_at,started_at,finished_at`

func (s *Store) ApplyOperatorRestoreDrill(req OperatorRestoreDrillRequest) (OperatorRestoreDrillResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) != req.IdempotencyKey || req.IdempotencyKey == "" || len(req.IdempotencyKey) > 200 {
		return OperatorRestoreDrillResult{}, operatorError(OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if !validArtifactFetchDigest(req.RequestDigest) {
		return OperatorRestoreDrillResult{}, operatorError(OperatorCodeRequestDigestRequired, "request body digest 必須是 canonical sha256")
	}
	audit := req.Audit
	audit.Action, audit.IdempotencyKey, audit.RequestDigest = AuditRestoreDrill, req.IdempotencyKey, req.RequestDigest
	if validRestoreDrillBackup(req.Prepared.Backup) {
		audit.Subject = req.Prepared.Backup.Name
	} else {
		audit.Subject = "restore drill request"
	}
	if validArtifactFetchText(req.Reason, auditMaxReason, false) {
		audit.Reason = req.Reason
	}

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorRestoreDrillResult{}, fmt.Errorf("store: begin restore drill enqueue: %w", err)
	}
	defer tx.Rollback()
	now, err := canonicalArtifactFetchNow(s.now())
	if err != nil {
		return OperatorRestoreDrillResult{}, err
	}
	audit.At = now
	cached, found, err := loadRestoreDrillCached(tx, req.IdempotencyKey)
	if err != nil {
		return OperatorRestoreDrillResult{}, err
	}
	if found {
		return s.replayRestoreDrill(tx, req, audit, cached)
	}
	code := ""
	switch {
	case !validArtifactFetchText(req.Reason, auditMaxReason, false),
		!validArtifactFetchDigest(req.PreviewDigest),
		!validRestoreDrillPrepared(req.Prepared):
		code = OperatorCodeRestoreDrillInvalid
	case req.Prepared.CurrentPreviewDigest != req.PreviewDigest:
		code = OperatorCodeRestoreDrillPreviewStale
	case req.Confirm != RestoreDrillConfirmation(req.Prepared.Backup.Name):
		code = OperatorCodeConfirmationMismatch
	}
	if code == "" {
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM restore_drill_operations WHERE state IN ('queued','running')`).Scan(&active); err != nil {
			return OperatorRestoreDrillResult{}, fmt.Errorf("store: inspect active restore drill: %w", err)
		}
		if active != 0 {
			code = OperatorCodeRestoreDrillActive
		}
	}
	if code != "" {
		return s.rejectRestoreDrill(tx, req, audit, code, now)
	}

	operationID := newID()
	receipt := operatorRestoreDrillReceipt{
		ReceiptVersion: operatorRestoreDrillReceiptVersion, OperationID: operationID,
		Backup: req.Prepared.Backup, PreviewDigest: req.PreviewDigest,
		LiveExpectedAtPreview: req.Prepared.LiveExpectedAtPreview, CreatedAt: now,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorRestoreDrillResult{}, fmt.Errorf("store: encode restore drill receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorRestoreDrillOperation,
		req.RequestDigest, string(raw), fmtTime(now)); err != nil {
		return OperatorRestoreDrillResult{}, fmt.Errorf("store: persist restore drill receipt: %w", err)
	}
	backup := req.Prepared.Backup
	if _, err := tx.Exec(`INSERT INTO restore_drill_operations
 (operation_id,idempotency_key,request_digest,backup_name,backup_size_bytes,backup_modified_at,
  backup_sha256,preview_digest,live_expected_at_preview,state,phase,attempt,created_at,updated_at)
 VALUES (?,?,?,?,?,?,?,?,?,'queued','queued',0,?,?)`, operationID, req.IdempotencyKey,
		req.RequestDigest, backup.Name, backup.SizeBytes, backup.ModifiedAt.Format(time.RFC3339Nano),
		backup.SHA256, req.PreviewDigest, req.Prepared.LiveExpectedAtPreview, fmtTime(now), fmtTime(now)); err != nil {
		return OperatorRestoreDrillResult{}, fmt.Errorf("store: enqueue restore drill: %w", err)
	}
	audit.OK = true
	audit.Detail = restoreDrillSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorRestoreDrillResult{}, fmt.Errorf("store: record restore drill enqueue audit: %w", err)
	}
	record, err := restoreDrillRecordByID(tx, operationID)
	if err != nil {
		return OperatorRestoreDrillResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorRestoreDrillResult{}, fmt.Errorf("store: commit restore drill enqueue: %w", err)
	}
	return OperatorRestoreDrillResult{Operation: record.Operation, Audited: true}, nil
}

// ⚠ preview 失敗時也必須先問帳本；cache miss 一列都不准寫，否則會消耗那把 key。
func (s *Store) ReplayOperatorRestoreDrill(req OperatorRestoreDrillRequest) (OperatorRestoreDrillResult, bool, error) {
	if strings.TrimSpace(req.IdempotencyKey) != req.IdempotencyKey || req.IdempotencyKey == "" || len(req.IdempotencyKey) > 200 {
		return OperatorRestoreDrillResult{}, false, nil
	}
	if !validArtifactFetchDigest(req.RequestDigest) {
		return OperatorRestoreDrillResult{}, false, nil
	}
	audit := req.Audit
	audit.Action, audit.IdempotencyKey, audit.RequestDigest = AuditRestoreDrill, req.IdempotencyKey, req.RequestDigest
	audit.Subject = "restore drill request"
	if validArtifactFetchText(req.Reason, auditMaxReason, false) {
		audit.Reason = req.Reason
	}

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorRestoreDrillResult{}, false, fmt.Errorf("store: begin restore drill replay: %w", err)
	}
	defer tx.Rollback()
	now, err := canonicalArtifactFetchNow(s.now())
	if err != nil {
		return OperatorRestoreDrillResult{}, false, err
	}
	audit.At = now
	cached, found, err := loadRestoreDrillCached(tx, req.IdempotencyKey)
	if err != nil {
		return OperatorRestoreDrillResult{}, false, err
	}
	if !found {
		return OperatorRestoreDrillResult{}, false, nil
	}
	result, err := s.replayRestoreDrill(tx, req, audit, cached)
	return result, true, err
}

func RestoreDrillConfirmation(name string) string { return "VERIFY " + name }

func loadRestoreDrillCached(q operatorRowQuerier, key string) (operatorCachedRequest, bool, error) {
	var cached operatorCachedRequest
	err := q.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
 FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&cached.Operation, &cached.Digest,
		&cached.Outcome, &cached.ResponseJSON, &cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operatorCachedRequest{}, false, nil
	}
	if err != nil {
		return operatorCachedRequest{}, false, fmt.Errorf("store: inspect restore drill idempotency key: %w", err)
	}
	return cached, true, nil
}

func (s *Store) rejectRestoreDrill(tx *sql.Tx, req OperatorRestoreDrillRequest, audit AuditEntry,
	code string, now time.Time,
) (OperatorRestoreDrillResult, error) {
	detail, ok := restoreDrillRejectionDetail(code)
	if !ok {
		return OperatorRestoreDrillResult{}, errors.New("store: invalid restore drill rejection code")
	}
	stored := restoreDrillRejectPrefix + code + "；" + detail
	audit.OK, audit.Detail = false, stored
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorRestoreDrillResult{}, fmt.Errorf("store: record restore drill rejection audit: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorRestoreDrillOperation,
		req.RequestDigest, code, stored, fmtTime(now)); err != nil {
		return OperatorRestoreDrillResult{}, fmt.Errorf("store: persist restore drill rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorRestoreDrillResult{}, fmt.Errorf("store: commit restore drill rejection: %w", err)
	}
	rejection := operatorError(code, detail)
	rejection.Audited = true
	return OperatorRestoreDrillResult{}, rejection
}

func restoreDrillRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeRestoreDrillInvalid:
		return "還原演練 request 不合法", true
	case OperatorCodeRestoreDrillPreviewStale:
		return "最新備份已改變；請重新預覽", true
	case OperatorCodeRestoreDrillActive:
		return "已有還原演練正在排隊或執行", true
	case OperatorCodeConfirmationMismatch:
		return "確認文字與最新備份不符", true
	default:
		return "", false
	}
}

func (s *Store) replayRestoreDrill(tx *sql.Tx, req OperatorRestoreDrillRequest, audit AuditEntry,
	cached operatorCachedRequest,
) (OperatorRestoreDrillResult, error) {
	if cached.Operation != operatorRestoreDrillOperation || cached.Digest != req.RequestDigest {
		audit.OK = false
		audit.Detail = "idempotency conflict：key 已被不同 request 使用"
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorRestoreDrillResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorRestoreDrillResult{}, err
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, "idempotency key 已被不同 request 使用")
		rejection.Audited = true
		return OperatorRestoreDrillResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		detail, ok := restoreDrillRejectionDetail(cached.ErrorCode.String)
		stored := restoreDrillRejectPrefix + cached.ErrorCode.String + "；" + detail
		if !ok || !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			cached.ErrorDetail.String != stored || !canonicalArtifactFetchRawTime(cached.CreatedAt) {
			return s.invalidRestoreDrillCache(tx, audit)
		}
		valid, err := validateRestoreDrillOriginalAudit(tx, req, cached.CreatedAt, false, stored)
		if err != nil {
			return OperatorRestoreDrillResult{}, err
		}
		if !valid {
			return s.invalidRestoreDrillCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorRestoreDrillResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorRestoreDrillResult{}, err
		}
		return OperatorRestoreDrillResult{Audited: true}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: detail, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid || cached.ErrorDetail.Valid ||
		!canonicalArtifactFetchRawTime(cached.CreatedAt) {
		return s.invalidRestoreDrillCache(tx, audit)
	}
	var receipt operatorRestoreDrillReceipt
	decoder := json.NewDecoder(bytes.NewBufferString(cached.ResponseJSON.String))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return s.invalidRestoreDrillCache(tx, audit)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return s.invalidRestoreDrillCache(tx, audit)
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || string(canonical) != cached.ResponseJSON.String ||
		receipt.ReceiptVersion != operatorRestoreDrillReceiptVersion ||
		receipt.PreviewDigest != req.PreviewDigest || !validRestoreDrillBackup(receipt.Backup) ||
		fmtTime(receipt.CreatedAt) != cached.CreatedAt {
		return s.invalidRestoreDrillCache(tx, audit)
	}
	record, err := restoreDrillRecordByID(tx, receipt.OperationID)
	if err != nil || record.IdempotencyKey != req.IdempotencyKey || record.RequestDigest != req.RequestDigest ||
		record.Operation.Backup != receipt.Backup || record.Operation.PreviewDigest != receipt.PreviewDigest ||
		record.Operation.LiveExpectedAtPreview != receipt.LiveExpectedAtPreview || !record.Operation.CreatedAt.Equal(receipt.CreatedAt) {
		return s.invalidRestoreDrillCache(tx, audit)
	}
	valid, err := validateRestoreDrillOriginalAudit(tx, req, cached.CreatedAt, true,
		restoreDrillSuccessDetail(receipt))
	if err != nil {
		return OperatorRestoreDrillResult{}, err
	}
	if !valid {
		return s.invalidRestoreDrillCache(tx, audit)
	}
	audit.Subject = receipt.Backup.Name
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + fmt.Sprintf("operation_id=%s，current_state=%s", receipt.OperationID, record.Operation.State)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorRestoreDrillResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorRestoreDrillResult{}, err
	}
	return OperatorRestoreDrillResult{Operation: record.Operation, Replayed: true, Audited: true}, nil
}

func restoreDrillSuccessDetail(receipt operatorRestoreDrillReceipt) string {
	return fmt.Sprintf("queued operation_id=%s，backup=%s，sha256=%s", receipt.OperationID, receipt.Backup.Name, receipt.Backup.SHA256)
}

// ⚠ 原始列身分是 key、request digest、寫入時間、判決與 detail；subject 記的是判決當下最新備份，會隨輪替改變，不得讓回放依賴今天的外部世界。
// ⚠ 成功列的 detail 已包含 operation_id、backup 與 sha256。
func validateRestoreDrillOriginalAudit(tx *sql.Tx, req OperatorRestoreDrillRequest, createdAt string,
	ok bool, detail string,
) (bool, error) {
	reason := ""
	if validArtifactFetchText(req.Reason, auditMaxReason, false) {
		reason = req.Reason
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
 WHERE action=? AND COALESCE(reason,'')=? AND idempotency_key=? AND request_digest=?
   AND at=? AND outcome=? AND COALESCE(detail,'')=?`, string(AuditRestoreDrill),
		truncAudit(reason, auditMaxReason), req.IdempotencyKey,
		req.RequestDigest, createdAt, outcomeOf(ok), truncAudit(detail, auditMaxReason)).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("store: inspect original restore drill audit: %w", err)
	}
	return count == 1, nil
}

func (s *Store) invalidRestoreDrillCache(tx *sql.Tx, audit AuditEntry) (OperatorRestoreDrillResult, error) {
	audit.Subject, audit.Reason, audit.OK, audit.Detail = "restore drill idempotency cache", "", false, restoreDrillCacheInvalid
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorRestoreDrillResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorRestoreDrillResult{}, err
	}
	return OperatorRestoreDrillResult{Audited: true}, ErrRestoreDrillCacheInvalid
}

func (s *Store) GetRestoreDrillOperation(id string) (RestoreDrillOperation, error) {
	record, err := restoreDrillRecordByID(s.db, id)
	return record.Operation, err
}

func (s *Store) ListRestoreDrillOperations(limit int) (RestoreDrillListResult, error) {
	if limit == 0 {
		limit = DefaultRestoreDrillReadLimit
	}
	if limit < 1 || limit > MaxRestoreDrillReadLimit {
		return RestoreDrillListResult{}, ErrRestoreDrillInvalid
	}
	now, err := canonicalArtifactFetchNow(s.now())
	if err != nil {
		return RestoreDrillListResult{}, err
	}
	result := RestoreDrillListResult{SchemaVersion: RestoreDrillReadSchemaVersion,
		Consistency: RestoreDrillReadConsistency, EvaluatedAt: now, Items: []RestoreDrillOperation{}}
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := tx.QueryRow(`SELECT COUNT(*) FROM restore_drill_operations`).Scan(&result.Total); err != nil {
		return result, err
	}
	rows, err := tx.Query("SELECT "+restoreDrillColumns+` FROM restore_drill_operations ORDER BY created_at DESC,operation_id DESC LIMIT ?`, limit)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanRestoreDrillRecord(rows)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, record.Operation)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) ListRestoreDrillOperationIDsForWorker(state RestoreDrillState) ([]string, error) {
	if state != RestoreDrillQueued && state != RestoreDrillRunning {
		return nil, ErrRestoreDrillInvalid
	}
	rows, err := s.db.Query(`SELECT operation_id FROM restore_drill_operations WHERE state=? ORDER BY created_at,operation_id`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !validArtifactFetchIdentifier(id, 128) {
			return nil, ErrRestoreDrillCorrupt
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) FenceRunningRestoreDrillOperations() (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT operation_id,run_token FROM restore_drill_operations WHERE state='running'`)
	if err != nil {
		return 0, err
	}
	type authority struct{ id, token string }
	authorities := []authority{}
	for rows.Next() {
		var item authority
		if err := rows.Scan(&item.id, &item.token); err != nil {
			_ = rows.Close()
			return 0, err
		}
		authorities = append(authorities, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, item := range authorities {
		if !validArtifactFetchIdentifier(item.id, 128) || !validArtifactFetchIdentifier(item.token, 200) {
			return 0, ErrRestoreDrillCorrupt
		}
		token, err := newToken()
		if err != nil {
			return 0, err
		}
		result, err := tx.Exec(`UPDATE restore_drill_operations SET run_token=? WHERE operation_id=? AND state='running' AND run_token=?`, token, item.id, item.token)
		if err != nil {
			return 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return 0, ErrRestoreDrillClaimLost
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(authorities), nil
}

func (s *Store) ClaimRestoreDrillOperation(id string, reclaim bool) (RestoreDrillClaim, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return RestoreDrillClaim{}, err
	}
	defer tx.Rollback()
	record, err := restoreDrillRecordByID(tx, id)
	if err != nil {
		return RestoreDrillClaim{}, err
	}
	if record.Operation.State != RestoreDrillQueued && !(reclaim && record.Operation.State == RestoreDrillRunning) {
		return RestoreDrillClaim{}, ErrRestoreDrillInvalidState
	}
	if record.Operation.Attempt == math.MaxInt64 {
		return RestoreDrillClaim{}, ErrRestoreDrillInvalidState
	}
	token, err := newToken()
	if err != nil {
		return RestoreDrillClaim{}, err
	}
	now, err := artifactFetchTransitionTime(s.now(), record.Operation.UpdatedAt)
	if err != nil {
		return RestoreDrillClaim{}, err
	}
	started := record.Operation.StartedAt
	if started == nil {
		started = &now
	}
	result, err := tx.Exec(`UPDATE restore_drill_operations SET state='running',phase='verifying',attempt=attempt+1,
 run_token=?,started_at=?,updated_at=? WHERE operation_id=? AND state=? AND COALESCE(run_token,'')=?`,
		token, fmtTimePtr(started), fmtTime(now), id, record.Operation.State, record.RunToken)
	if err != nil {
		return RestoreDrillClaim{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return RestoreDrillClaim{}, ErrRestoreDrillClaimLost
	}
	record, err = restoreDrillRecordByID(tx, id)
	if err != nil {
		return RestoreDrillClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return RestoreDrillClaim{}, err
	}
	return RestoreDrillClaim{Operation: record.Operation, RunToken: token}, nil
}

func (s *Store) SucceedRestoreDrillOperation(id, token string, result RestoreDrillWorkerResult) (RestoreDrillOperation, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	defer tx.Rollback()
	record, err := restoreDrillRecordByID(tx, id)
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	if record.Operation.State != RestoreDrillRunning || record.RunToken != token {
		return RestoreDrillOperation{}, ErrRestoreDrillClaimLost
	}
	if result.Backup != record.Operation.Backup || result.Machines <= 0 || result.Expected < 0 ||
		result.Expected > result.Machines || result.LiveExpected < -1 || result.DurationMilliseconds < 0 {
		return RestoreDrillOperation{}, ErrRestoreDrillInvalidState
	}
	now, err := artifactFetchTransitionTime(s.now(), record.Operation.UpdatedAt)
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	updated, err := tx.Exec(`UPDATE restore_drill_operations SET state='succeeded',phase='complete',run_token=NULL,
 machines=?,expected=?,live_expected=?,newest_checkin_at=?,duration_milliseconds=?,finished_at=?,updated_at=?
 WHERE operation_id=? AND state='running' AND run_token=?`, result.Machines, result.Expected, result.LiveExpected, fmtTimePtr(result.NewestCheckinAt),
		result.DurationMilliseconds, fmtTime(now), fmtTime(now), id, token)
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil || changed != 1 {
		return RestoreDrillOperation{}, ErrRestoreDrillClaimLost
	}
	record, err = restoreDrillRecordByID(tx, id)
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return RestoreDrillOperation{}, err
	}
	return record.Operation, nil
}

func (s *Store) FailRestoreDrillOperation(id, token, code, detail string) (RestoreDrillOperation, error) {
	if !validArtifactFetchErrorCode(code) || !validArtifactFetchText(detail, RestoreDrillMaxErrorBytes, false) {
		return RestoreDrillOperation{}, ErrRestoreDrillInvalidState
	}
	tx, err := s.db.Begin()
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	defer tx.Rollback()
	record, err := restoreDrillRecordByID(tx, id)
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	if record.Operation.State != RestoreDrillRunning || record.RunToken != token {
		return RestoreDrillOperation{}, ErrRestoreDrillClaimLost
	}
	now, err := artifactFetchTransitionTime(s.now(), record.Operation.UpdatedAt)
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	updated, err := tx.Exec(`UPDATE restore_drill_operations SET state='failed',run_token=NULL,error_code=?,error_detail=?,
 finished_at=?,updated_at=? WHERE operation_id=? AND state='running' AND run_token=?`, code, detail, fmtTime(now), fmtTime(now), id, token)
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	changed, err := updated.RowsAffected()
	if err != nil || changed != 1 {
		return RestoreDrillOperation{}, ErrRestoreDrillClaimLost
	}
	record, err = restoreDrillRecordByID(tx, id)
	if err != nil {
		return RestoreDrillOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return RestoreDrillOperation{}, err
	}
	return record.Operation, nil
}

func restoreDrillRecordByID(q operatorRowQuerier, id string) (restoreDrillRecord, error) {
	if !validArtifactFetchIdentifier(id, 128) {
		return restoreDrillRecord{}, ErrRestoreDrillNotFound
	}
	record, err := scanRestoreDrillRecord(q.QueryRow("SELECT "+restoreDrillColumns+" FROM restore_drill_operations WHERE operation_id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return restoreDrillRecord{}, ErrRestoreDrillNotFound
	}
	return record, err
}

type restoreDrillScanner interface{ Scan(...any) error }

func scanRestoreDrillRecord(scanner restoreDrillScanner) (restoreDrillRecord, error) {
	var record restoreDrillRecord
	var modified, state, phase, created, updated string
	var runToken, newest, errorCode, errorDetail, started, finished sql.NullString
	var machines, expected, liveExpected, duration sql.NullInt64
	err := scanner.Scan(&record.Operation.OperationID, &record.IdempotencyKey, &record.RequestDigest,
		&record.Operation.Backup.Name, &record.Operation.Backup.SizeBytes, &modified,
		&record.Operation.Backup.SHA256, &record.Operation.PreviewDigest, &record.Operation.LiveExpectedAtPreview,
		&state, &phase, &record.Operation.Attempt, &runToken, &machines, &expected, &liveExpected, &newest,
		&duration, &errorCode, &errorDetail, &created, &updated, &started, &finished)
	if err != nil {
		return record, err
	}
	record.Operation.Backup.ModifiedAt, err = time.Parse(time.RFC3339Nano, modified)
	if err != nil {
		return record, ErrRestoreDrillCorrupt
	}
	record.Operation.State, record.Operation.Phase = RestoreDrillState(state), RestoreDrillPhase(phase)
	record.RunToken = runToken.String
	if err := assignRestoreDrillTimes(&record.Operation, created, updated, started, finished, newest); err != nil {
		return record, err
	}
	record.Operation.Machines = restoreDrillNullInt(machines)
	record.Operation.Expected = restoreDrillNullInt(expected)
	record.Operation.LiveExpected = restoreDrillNullInt(liveExpected)
	record.Operation.DurationMilliseconds = restoreDrillNullInt64(duration)
	record.Operation.ErrorCode = restoreDrillNullString(errorCode)
	record.Operation.ErrorDetail = restoreDrillNullString(errorDetail)
	if !validRestoreDrillRecord(record) {
		return record, ErrRestoreDrillCorrupt
	}
	return record, nil
}

func assignRestoreDrillTimes(op *RestoreDrillOperation, created, updated string,
	started, finished, newest sql.NullString,
) error {
	var err error
	if op.CreatedAt, err = time.Parse(time.RFC3339, created); err != nil {
		return ErrRestoreDrillCorrupt
	}
	if op.UpdatedAt, err = time.Parse(time.RFC3339, updated); err != nil {
		return ErrRestoreDrillCorrupt
	}
	parse := func(value sql.NullString) (*time.Time, error) {
		if !value.Valid {
			return nil, nil
		}
		parsed, err := time.Parse(time.RFC3339Nano, value.String)
		if err != nil {
			return nil, ErrRestoreDrillCorrupt
		}
		return &parsed, nil
	}
	if op.StartedAt, err = parse(started); err != nil {
		return err
	}
	if op.FinishedAt, err = parse(finished); err != nil {
		return err
	}
	if op.NewestCheckinAt, err = parse(newest); err != nil {
		return err
	}
	return nil
}

func restoreDrillNullInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	converted := int(value.Int64)
	return &converted
}

func restoreDrillNullInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func restoreDrillNullString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func validRestoreDrillPrepared(prepared RestoreDrillPrepared) bool {
	return validRestoreDrillBackup(prepared.Backup) && prepared.LiveExpectedAtPreview >= -1 &&
		validArtifactFetchDigest(prepared.CurrentPreviewDigest)
}

func validRestoreDrillBackup(backup RestoreDrillBackup) bool {
	_, offset := backup.ModifiedAt.Zone()
	return validRestoreDrillBackupName(backup.Name) && backup.SizeBytes > 0 &&
		!backup.ModifiedAt.IsZero() && offset == 0 &&
		validArtifactFetchDigest(backup.SHA256)
}

func validRestoreDrillBackupName(name string) bool {
	if len(name) < 1 || len(name) > 255 || !utf8.ValidString(name) || filepath.Base(name) != name ||
		!strings.HasPrefix(name, "clawctl-") || !strings.HasSuffix(name, ".sqlite") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

func validRestoreDrillRecord(record restoreDrillRecord) bool {
	op := record.Operation
	if !validArtifactFetchIdentifier(op.OperationID, 128) || !validArtifactFetchIdentifier(record.IdempotencyKey, 200) ||
		!validArtifactFetchDigest(record.RequestDigest) || !validRestoreDrillBackup(op.Backup) ||
		!validArtifactFetchDigest(op.PreviewDigest) || op.LiveExpectedAtPreview < -1 || op.Attempt < 0 ||
		op.CreatedAt.IsZero() || op.UpdatedAt.Before(op.CreatedAt) ||
		(op.StartedAt != nil && (op.StartedAt.Before(op.CreatedAt) || op.StartedAt.After(op.UpdatedAt))) ||
		(op.FinishedAt != nil && (op.StartedAt == nil || op.FinishedAt.Before(*op.StartedAt) || !op.FinishedAt.Equal(op.UpdatedAt))) ||
		(op.NewestCheckinAt != nil && !canonicalRestoreDrillEvidenceTime(*op.NewestCheckinAt)) {
		return false
	}
	for _, value := range []*time.Time{op.StartedAt, op.FinishedAt} {
		if value != nil && !canonicalRestoreDrillTransitionTime(*value) {
			return false
		}
	}
	switch op.State {
	case RestoreDrillQueued:
		return op.Phase == RestoreDrillPhaseQueued && op.Attempt == 0 && record.RunToken == "" &&
			op.StartedAt == nil && op.FinishedAt == nil && op.Machines == nil && op.Expected == nil &&
			op.LiveExpected == nil && op.NewestCheckinAt == nil && op.DurationMilliseconds == nil &&
			op.ErrorCode == nil && op.ErrorDetail == nil
	case RestoreDrillRunning:
		return op.Phase == RestoreDrillPhaseVerifying && op.Attempt > 0 && record.RunToken != "" &&
			op.StartedAt != nil && op.FinishedAt == nil && op.Machines == nil && op.Expected == nil &&
			op.LiveExpected == nil && op.NewestCheckinAt == nil && op.DurationMilliseconds == nil &&
			op.ErrorCode == nil && op.ErrorDetail == nil
	case RestoreDrillSucceeded:
		return op.Phase == RestoreDrillPhaseComplete && op.Attempt > 0 && record.RunToken == "" &&
			op.StartedAt != nil && op.FinishedAt != nil && op.Machines != nil && *op.Machines > 0 &&
			op.Expected != nil && *op.Expected >= 0 && *op.Expected <= *op.Machines && op.LiveExpected != nil &&
			*op.LiveExpected >= -1 && op.DurationMilliseconds != nil && *op.DurationMilliseconds >= 0 &&
			op.ErrorCode == nil && op.ErrorDetail == nil
	case RestoreDrillFailed:
		return op.Phase == RestoreDrillPhaseVerifying && op.Attempt > 0 && record.RunToken == "" &&
			op.StartedAt != nil && op.FinishedAt != nil && op.Machines == nil && op.Expected == nil &&
			op.LiveExpected == nil && op.NewestCheckinAt == nil && op.DurationMilliseconds == nil &&
			op.ErrorCode != nil && validArtifactFetchErrorCode(*op.ErrorCode) && op.ErrorDetail != nil &&
			validArtifactFetchText(*op.ErrorDetail, RestoreDrillMaxErrorBytes, false)
	default:
		return false
	}
}

func canonicalRestoreDrillTransitionTime(value time.Time) bool {
	_, offset := value.Zone()
	return !value.IsZero() && offset == 0 && value.Nanosecond() == 0
}

func canonicalRestoreDrillEvidenceTime(value time.Time) bool {
	_, offset := value.Zone()
	return !value.IsZero() && offset == 0
}
