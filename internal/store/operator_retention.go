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
	"strings"
	"time"
)

const (
	operatorRetentionSchemaVersion  = 1
	operatorRetentionOperation      = "maintenance-retention-prune:v1"
	operatorRetentionReceiptVersion = "v1"
	operatorRetentionCacheInvalid   = "operator retention idempotency cache invalid；未回放結果"
)

// OperatorRetentionPolicy is the stable wire identity of a RetentionPolicy.
// Durations are integer seconds so JSON, CLI, and HTML cannot disagree about
// duration spelling while hashing a preview.
type OperatorRetentionPolicy struct {
	ObservationsSeconds int64 `json:"observations_seconds"`
	CheckinsSeconds     int64 `json:"checkins_seconds"`
	OccupancySeconds    int64 `json:"occupancy_seconds"`
}

func NewOperatorRetentionPolicy(policy RetentionPolicy) (OperatorRetentionPolicy, error) {
	if err := policy.Validate(); err != nil {
		return OperatorRetentionPolicy{}, operatorError(OperatorCodeRetentionPolicyInvalid,
			"保留期不合法；每一類都必須長於 7 天")
	}
	for _, value := range []time.Duration{policy.Observations, policy.Checkins, policy.Occupancy} {
		if value%time.Second != 0 {
			return OperatorRetentionPolicy{}, operatorError(OperatorCodeRetentionPolicyInvalid,
				"保留期必須使用整數秒")
		}
	}
	return OperatorRetentionPolicy{
		ObservationsSeconds: int64(policy.Observations / time.Second),
		CheckinsSeconds:     int64(policy.Checkins / time.Second),
		OccupancySeconds:    int64(policy.Occupancy / time.Second),
	}, nil
}

func (p OperatorRetentionPolicy) RetentionPolicy() (RetentionPolicy, error) {
	if p.ObservationsSeconds <= 0 || p.CheckinsSeconds <= 0 || p.OccupancySeconds <= 0 {
		return RetentionPolicy{}, operatorError(OperatorCodeRetentionPolicyInvalid,
			"保留期秒數必須是正整數")
	}
	const maxDurationSeconds = int64(math.MaxInt64) / int64(time.Second)
	if p.ObservationsSeconds > maxDurationSeconds || p.CheckinsSeconds > maxDurationSeconds ||
		p.OccupancySeconds > maxDurationSeconds {
		return RetentionPolicy{}, operatorError(OperatorCodeRetentionPolicyInvalid,
			"保留期秒數超出可接受範圍")
	}
	policy := RetentionPolicy{
		Observations: time.Duration(p.ObservationsSeconds) * time.Second,
		Checkins:     time.Duration(p.CheckinsSeconds) * time.Second,
		Occupancy:    time.Duration(p.OccupancySeconds) * time.Second,
	}
	canonical, err := NewOperatorRetentionPolicy(policy)
	if err != nil || canonical != p {
		return RetentionPolicy{}, operatorError(OperatorCodeRetentionPolicyInvalid,
			"保留期秒數超出可接受範圍")
	}
	return policy, nil
}

type OperatorRetentionStatus struct {
	SchemaVersion int                     `json:"schema_version"`
	EvaluatedAt   time.Time               `json:"evaluated_at"`
	Policy        OperatorRetentionPolicy `json:"policy"`
	Revision      int64                   `json:"revision"`
	HasRun        bool                    `json:"has_run"`
	LastPruneAt   *time.Time              `json:"last_prune_at,omitempty"`
	LastPruneRows int64                   `json:"last_prune_rows"`
}

type OperatorPrunePreview struct {
	SchemaVersion    int                     `json:"schema_version"`
	EvaluatedAt      time.Time               `json:"evaluated_at"`
	Policy           OperatorRetentionPolicy `json:"policy"`
	ExpectedRevision int64                   `json:"expected_revision"`
	Counts           []PruneCount            `json:"counts"`
	TotalDeleted     int64                   `json:"total_deleted"`
	KeptNewest       int64                   `json:"kept_newest"`
	Confirmation     string                  `json:"confirmation"`
	PreviewDigest    string                  `json:"preview_digest"`
}

type OperatorPruneRequest struct {
	EvaluatedAt      time.Time
	Policy           OperatorRetentionPolicy
	ExpectedRevision int64
	Confirm          string
	PreviewDigest    string
	Reason           string
	IdempotencyKey   string
	RequestDigest    string
	Audit            AuditEntry
}

type OperatorPruneResult struct {
	EvaluatedAt  time.Time               `json:"evaluated_at"`
	AppliedAt    time.Time               `json:"applied_at"`
	Policy       OperatorRetentionPolicy `json:"policy"`
	Counts       []PruneCount            `json:"counts"`
	TotalDeleted int64                   `json:"total_deleted"`
	KeptNewest   int64                   `json:"kept_newest"`
	Revision     int64                   `json:"revision"`
	Replayed     bool                    `json:"replayed"`
	Audited      bool                    `json:"-"`
}

type operatorRetentionReceipt struct {
	ReceiptVersion string                  `json:"receipt_version"`
	EvaluatedAt    time.Time               `json:"evaluated_at"`
	AppliedAt      time.Time               `json:"applied_at"`
	Policy         OperatorRetentionPolicy `json:"policy"`
	Counts         []PruneCount            `json:"counts"`
	TotalDeleted   int64                   `json:"total_deleted"`
	KeptNewest     int64                   `json:"kept_newest"`
	Revision       int64                   `json:"revision"`
	PreviewDigest  string                  `json:"preview_digest"`
}

func (s *Store) OperatorRetentionStatus(now time.Time, policy RetentionPolicy) (OperatorRetentionStatus, error) {
	wire, err := NewOperatorRetentionPolicy(policy)
	if err != nil {
		return OperatorRetentionStatus{}, err
	}
	tx, err := s.beginWrite(context.Background(), "operator_retention_status")
	if err != nil {
		return OperatorRetentionStatus{}, fmt.Errorf("store: begin retention status: %w", err)
	}
	defer tx.Rollback()
	revision, err := retentionRevisionTx(tx)
	if err != nil {
		return OperatorRetentionStatus{}, err
	}
	status := OperatorRetentionStatus{
		SchemaVersion: operatorRetentionSchemaVersion,
		EvaluatedAt:   now.UTC().Truncate(time.Second), Policy: wire, Revision: revision,
	}
	var lastRaw string
	err = tx.QueryRow(`SELECT at,SUM(rows_deleted) FROM retention_log
 WHERE at=(SELECT MAX(at) FROM retention_log) GROUP BY at`).Scan(&lastRaw, &status.LastPruneRows)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return OperatorRetentionStatus{}, fmt.Errorf("store: read retention status: %w", err)
	default:
		last := parseTime(lastRaw)
		if last.IsZero() || fmtTime(last) != lastRaw {
			return OperatorRetentionStatus{}, errors.New("store: retention status contains invalid timestamp")
		}
		status.HasRun, status.LastPruneAt = true, &last
	}
	if err := tx.Commit(); err != nil {
		return OperatorRetentionStatus{}, fmt.Errorf("store: finish retention status: %w", err)
	}
	return status, nil
}

func (s *Store) PreviewOperatorPrune(evaluatedAt time.Time, policy RetentionPolicy) (OperatorPrunePreview, error) {
	wire, err := NewOperatorRetentionPolicy(policy)
	if err != nil {
		return OperatorPrunePreview{}, err
	}
	evaluatedAt = evaluatedAt.UTC()
	if evaluatedAt.IsZero() || !evaluatedAt.Equal(evaluatedAt.Truncate(time.Second)) {
		return OperatorPrunePreview{}, operatorError(OperatorCodeRetentionPolicyInvalid,
			"evaluated_at 必須是 UTC 秒級時間")
	}
	tx, err := s.beginWrite(context.Background(), "preview_operator_prune")
	if err != nil {
		return OperatorPrunePreview{}, fmt.Errorf("store: begin retention preview: %w", err)
	}
	defer tx.Rollback()
	preview, err := operatorPrunePreviewTx(tx, evaluatedAt, policy, wire)
	if err != nil {
		return OperatorPrunePreview{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorPrunePreview{}, fmt.Errorf("store: finish retention preview: %w", err)
	}
	return preview, nil
}

func operatorPrunePreviewTx(tx dbTx, evaluatedAt time.Time, policy RetentionPolicy,
	wire OperatorRetentionPolicy,
) (OperatorPrunePreview, error) {
	revision, err := retentionRevisionTx(tx)
	if err != nil {
		return OperatorPrunePreview{}, err
	}
	report, err := pruneReportTx(tx, evaluatedAt, policy, true)
	if err != nil {
		return OperatorPrunePreview{}, err
	}
	preview := OperatorPrunePreview{
		SchemaVersion: operatorRetentionSchemaVersion, EvaluatedAt: evaluatedAt,
		Policy: wire, ExpectedRevision: revision, Counts: report.Counts,
		TotalDeleted: report.Total(), KeptNewest: report.KeptNewest,
	}
	if preview.Counts == nil {
		preview.Counts = []PruneCount{}
	}
	preview.Confirmation = operatorPruneConfirmation(preview.TotalDeleted)
	preview.PreviewDigest = operatorPrunePreviewDigest(preview)
	return preview, nil
}

func retentionRevisionTx(tx dbTx) (int64, error) {
	var revision int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(prune_id),0) FROM retention_log`).Scan(&revision); err != nil {
		return 0, fmt.Errorf("store: read retention revision: %w", err)
	}
	return revision, nil
}

func operatorPruneConfirmation(total int64) string {
	return fmt.Sprintf("DELETE %d ROWS", total)
}

func operatorPrunePreviewDigest(preview OperatorPrunePreview) string {
	copy := preview
	copy.PreviewDigest = ""
	raw, _ := json.Marshal(copy)
	return sha256Digest(raw)
}

func OperatorPruneSemanticDigest(req OperatorPruneRequest) string {
	body := struct {
		EvaluatedAt      time.Time               `json:"evaluated_at"`
		Policy           OperatorRetentionPolicy `json:"policy"`
		ExpectedRevision int64                   `json:"expected_revision"`
		Confirm          string                  `json:"confirm"`
		PreviewDigest    string                  `json:"preview_digest"`
		Reason           string                  `json:"reason"`
	}{req.EvaluatedAt.UTC(), req.Policy, req.ExpectedRevision, req.Confirm, req.PreviewDigest, req.Reason}
	raw, _ := json.Marshal(body)
	return sha256Digest(raw)
}

func (s *Store) ApplyOperatorPrune(req OperatorPruneRequest) (OperatorPruneResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) != req.IdempotencyKey || req.IdempotencyKey == "" || len(req.IdempotencyKey) > 200 {
		return OperatorPruneResult{}, operatorError(OperatorCodeIdempotencyKeyRequired,
			"Idempotency-Key 不可省略且最多 200 bytes")
	}
	if !validArtifactFetchDigest(req.RequestDigest) {
		return OperatorPruneResult{}, operatorError(OperatorCodeRequestDigestRequired,
			"request body digest 必須是 canonical sha256")
	}
	audit := req.Audit
	audit.Action, audit.Subject = AuditRetentionPrune, "retention"
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = req.Reason, req.IdempotencyKey, req.RequestDigest

	tx, err := s.beginWrite(context.Background(), "apply_operator_prune")
	if err != nil {
		return OperatorPruneResult{}, fmt.Errorf("store: begin operator retention prune: %w", err)
	}
	defer tx.Rollback()
	cached, found, err := loadOperatorRetentionCached(tx, req.IdempotencyKey)
	if err != nil {
		return OperatorPruneResult{}, err
	}
	if found {
		return s.replayOperatorPrune(tx, req, audit, cached)
	}

	reject := func(code string) (OperatorPruneResult, error) {
		return s.rejectOperatorPruneTx(tx, req, audit, code, s.now().UTC().Truncate(time.Second))
	}
	if strings.TrimSpace(req.Reason) == "" || req.Reason != strings.TrimSpace(req.Reason) || len(req.Reason) > auditMaxReason {
		return reject(OperatorCodeReasonRequired)
	}
	policy, err := req.Policy.RetentionPolicy()
	if err != nil {
		return reject(OperatorCodeRetentionPolicyInvalid)
	}
	if req.EvaluatedAt.IsZero() || !req.EvaluatedAt.Equal(req.EvaluatedAt.UTC().Truncate(time.Second)) ||
		req.EvaluatedAt.After(s.now().UTC()) {
		return reject(OperatorCodeRetentionPolicyInvalid)
	}
	current, err := operatorPrunePreviewTx(tx, req.EvaluatedAt.UTC(), policy, req.Policy)
	if err != nil {
		return OperatorPruneResult{}, err
	}
	if req.ExpectedRevision != current.ExpectedRevision {
		return reject(OperatorCodePreconditionFailed)
	}
	if req.PreviewDigest == "" || req.PreviewDigest != current.PreviewDigest {
		return reject(OperatorCodeRetentionPreviewStale)
	}
	if req.Confirm != current.Confirmation {
		return reject(OperatorCodeConfirmationMismatch)
	}
	if current.TotalDeleted == 0 {
		return reject(OperatorCodeRetentionNothingToPrune)
	}
	report, err := pruneReportTx(tx, current.EvaluatedAt, policy, false)
	if err != nil {
		return OperatorPruneResult{}, err
	}
	if report.Total() != current.TotalDeleted || report.KeptNewest != current.KeptNewest ||
		!equalPruneCounts(report.Counts, current.Counts) {
		return OperatorPruneResult{}, errors.New("store: retention delete set changed within transaction")
	}
	if err := writeRetentionLogTx(tx, report); err != nil {
		return OperatorPruneResult{}, err
	}
	revision, err := retentionRevisionTx(tx)
	if err != nil {
		return OperatorPruneResult{}, err
	}
	appliedAt := s.now().UTC().Truncate(time.Second)
	receipt := operatorRetentionReceipt{
		ReceiptVersion: operatorRetentionReceiptVersion, EvaluatedAt: current.EvaluatedAt,
		AppliedAt: appliedAt, Policy: current.Policy, Counts: current.Counts,
		TotalDeleted: current.TotalDeleted, KeptNewest: current.KeptNewest,
		Revision: revision, PreviewDigest: current.PreviewDigest,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorPruneResult{}, fmt.Errorf("store: encode retention receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorRetentionOperation,
		req.RequestDigest, string(raw), fmtTime(appliedAt)); err != nil {
		return OperatorPruneResult{}, fmt.Errorf("store: persist retention receipt: %w", err)
	}
	audit.At, audit.OK, audit.Detail = appliedAt, true, operatorPruneSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorPruneResult{}, fmt.Errorf("store: record retention audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorPruneResult{}, fmt.Errorf("store: commit operator retention prune: %w", err)
	}
	result := operatorPruneResult(receipt)
	result.Audited = true
	return result, nil
}

func loadOperatorRetentionCached(tx dbTx, key string) (operatorCachedRequest, bool, error) {
	var cached operatorCachedRequest
	err := tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
 FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operatorCachedRequest{}, false, nil
	}
	if err != nil {
		return operatorCachedRequest{}, false, fmt.Errorf("store: inspect retention idempotency key: %w", err)
	}
	return cached, true, nil
}

func (s *Store) rejectOperatorPruneTx(tx dbTx, req OperatorPruneRequest, audit AuditEntry,
	code string, at time.Time,
) (OperatorPruneResult, error) {
	detail, ok := canonicalOperatorRetentionRejectionDetail(code)
	if !ok {
		return OperatorPruneResult{}, errors.New("store: invalid retention rejection code")
	}
	audit.At, audit.OK, audit.Detail = at, false, detail
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorPruneResult{}, fmt.Errorf("store: record retention rejection audit: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorRetentionOperation,
		req.RequestDigest, code, detail, fmtTime(at)); err != nil {
		return OperatorPruneResult{}, fmt.Errorf("store: persist retention rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorPruneResult{}, fmt.Errorf("store: commit retention rejection: %w", err)
	}
	rejection := operatorError(code, detail)
	rejection.Audited = true
	return OperatorPruneResult{}, rejection
}

func (s *Store) replayOperatorPrune(tx dbTx, req OperatorPruneRequest, audit AuditEntry,
	cached operatorCachedRequest,
) (OperatorPruneResult, error) {
	if cached.Operation != operatorRetentionOperation || cached.Digest != req.RequestDigest {
		audit.At, audit.OK, audit.Detail = s.now().UTC().Truncate(time.Second), false, "idempotency conflict"
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorPruneResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorPruneResult{}, err
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict,
			"idempotency key 已被不同 request 使用")
		rejection.Audited = true
		return OperatorPruneResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		detail, ok := canonicalOperatorRetentionRejectionDetail(cached.ErrorCode.String)
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid || !ok ||
			cached.ErrorDetail.String != detail || !canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
			return s.rejectInvalidOperatorRetentionCache(tx, audit)
		}
		valid, err := validateOperatorRetentionAudit(tx, req, cached.CreatedAt, false, detail)
		if err != nil || !valid {
			return s.rejectInvalidOperatorRetentionCache(tx, audit)
		}
		audit.At, audit.OK, audit.Detail = s.now().UTC().Truncate(time.Second), false,
			OperatorIdempotencyReplayPrefix+"原判決："+detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorPruneResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorPruneResult{}, err
		}
		return OperatorPruneResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: detail, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid ||
		cached.ErrorDetail.Valid || !canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return s.rejectInvalidOperatorRetentionCache(tx, audit)
	}
	receipt, err := decodeOperatorRetentionReceipt(cached.ResponseJSON.String)
	if err != nil || !validOperatorRetentionReceipt(receipt, req, cached.CreatedAt) {
		return s.rejectInvalidOperatorRetentionCache(tx, audit)
	}
	valid, err := validateOperatorRetentionSuccessEvidence(tx, receipt, req)
	if err != nil || !valid {
		return s.rejectInvalidOperatorRetentionCache(tx, audit)
	}
	audit.At, audit.OK, audit.Detail = s.now().UTC().Truncate(time.Second), true,
		OperatorIdempotencyReplayPrefix+"沒有再次刪除資料"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorPruneResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorPruneResult{}, err
	}
	result := operatorPruneResult(receipt)
	result.Replayed, result.Audited = true, true
	return result, nil
}

func (s *Store) rejectInvalidOperatorRetentionCache(tx dbTx, audit AuditEntry) (OperatorPruneResult, error) {
	audit.At, audit.Subject, audit.Reason, audit.OK, audit.Detail =
		s.now().UTC().Truncate(time.Second), "retention idempotency cache", "", false, operatorRetentionCacheInvalid
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorPruneResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorPruneResult{}, err
	}
	return OperatorPruneResult{Audited: true}, errors.New("store: operator retention idempotency cache is invalid")
}

func canonicalOperatorRetentionRejectionDetail(code string) (string, bool) {
	details := map[string]string{
		OperatorCodeReasonRequired:          "reason 不可省略、前後不可有空白，且最多 500 字",
		OperatorCodeRetentionPolicyInvalid:  "保留期或評估時間不合法",
		OperatorCodePreconditionFailed:      "retention revision 已變更；請重新預覽",
		OperatorCodeRetentionPreviewStale:   "清理範圍已變更；請重新預覽",
		OperatorCodeConfirmationMismatch:    "確認字串與預覽不符",
		OperatorCodeRetentionNothingToPrune: "目前沒有超過保留期且可刪除的資料",
	}
	detail, ok := details[code]
	return detail, ok
}

func decodeOperatorRetentionReceipt(raw string) (operatorRetentionReceipt, error) {
	var receipt operatorRetentionReceipt
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return receipt, errors.New("operator retention receipt has trailing JSON")
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || string(canonical) != raw {
		return receipt, errors.New("operator retention receipt is not canonical JSON")
	}
	return receipt, nil
}

func validOperatorRetentionReceipt(receipt operatorRetentionReceipt, req OperatorPruneRequest,
	createdAt string,
) bool {
	policy, err := receipt.Policy.RetentionPolicy()
	return err == nil && receipt.ReceiptVersion == operatorRetentionReceiptVersion &&
		receipt.EvaluatedAt.Equal(req.EvaluatedAt.UTC()) && receipt.AppliedAt.Equal(parseTime(createdAt)) &&
		receipt.Policy == req.Policy && receipt.PreviewDigest == req.PreviewDigest &&
		validArtifactFetchDigest(receipt.PreviewDigest) && receipt.TotalDeleted > 0 &&
		receipt.TotalDeleted == pruneCountsTotal(receipt.Counts) &&
		receipt.KeptNewest == pruneCountsKept(receipt.Counts) && receipt.Revision > req.ExpectedRevision &&
		validOperatorRetentionCounts(receipt.Counts, receipt.EvaluatedAt, policy)
}

func validOperatorRetentionCounts(counts []PruneCount, evaluatedAt time.Time, policy RetentionPolicy) bool {
	if len(counts) != len(pruneJobs) {
		return false
	}
	for index, item := range counts {
		job := pruneJobs[index]
		if item.Table != job.table || item.Deleted < 0 || item.Kept < 0 ||
			!item.Older.Equal(evaluatedAt.Add(-job.class.horizon(policy))) {
			return false
		}
	}
	return true
}

func validateOperatorRetentionAudit(tx dbTx, req OperatorPruneRequest, at string, ok bool,
	detail string,
) (bool, error) {
	outcome := "failed"
	if ok {
		outcome = "ok"
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND subject='retention'
 AND COALESCE(reason,'')=? AND idempotency_key=? AND request_digest=? AND at=? AND outcome=?
 AND COALESCE(detail,'')=?`, string(AuditRetentionPrune), req.Reason, req.IdempotencyKey,
		req.RequestDigest, at, outcome, detail).Scan(&count)
	return count == 1, err
}

func validateOperatorRetentionSuccessEvidence(tx dbTx, receipt operatorRetentionReceipt,
	req OperatorPruneRequest,
) (bool, error) {
	valid, err := validateOperatorRetentionAudit(tx, req, fmtTime(receipt.AppliedAt), true,
		operatorPruneSuccessDetail(receipt))
	if err != nil || !valid {
		return valid, err
	}
	for _, item := range receipt.Counts {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM retention_log WHERE at=? AND table_name=?
 AND rows_deleted=? AND older_than=? AND kept_newest=?`, fmtTime(receipt.EvaluatedAt), item.Table,
			item.Deleted, fmtTime(item.Older), item.Kept).Scan(&count); err != nil || count != 1 {
			return false, err
		}
	}
	return true, nil
}

func operatorPruneSuccessDetail(receipt operatorRetentionReceipt) string {
	return fmt.Sprintf("deleted=%d kept_newest=%d revision=%d",
		receipt.TotalDeleted, receipt.KeptNewest, receipt.Revision)
}

func operatorPruneResult(receipt operatorRetentionReceipt) OperatorPruneResult {
	return OperatorPruneResult{
		EvaluatedAt: receipt.EvaluatedAt, AppliedAt: receipt.AppliedAt, Policy: receipt.Policy,
		Counts: receipt.Counts, TotalDeleted: receipt.TotalDeleted,
		KeptNewest: receipt.KeptNewest, Revision: receipt.Revision,
	}
}

func equalPruneCounts(a, b []PruneCount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Table != b[i].Table || a[i].Deleted != b[i].Deleted || a[i].Kept != b[i].Kept ||
			!a[i].Older.Equal(b[i].Older) {
			return false
		}
	}
	return true
}

func pruneCountsTotal(counts []PruneCount) int64 {
	var total int64
	for _, item := range counts {
		total += item.Deleted
	}
	return total
}

func pruneCountsKept(counts []PruneCount) int64 {
	var total int64
	for _, item := range counts {
		total += item.Kept
	}
	return total
}
