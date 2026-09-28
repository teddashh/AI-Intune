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
)

const (
	operatorVerifierRevokeVersion = "v2"

	operatorVerifierRevokeCacheInvalid = "operator verifier revocation idempotency cache invalid；未回放結果"
	operatorVerifierRevokeRejectPrefix = "operator verifier revocation rejection code="
)

// operatorVerifierRevocationPolicy is what revocation promises. The row and
// the evidence it already produced both stay: deleting them would make past
// job pages lose the producer that wrote them.
type operatorVerifierRevocationPolicy struct {
	RegistryRowRetained  bool `json:"registry_row_retained"`
	EvidenceRetained     bool `json:"evidence_retained"`
	DisplayNameReusable  bool `json:"display_name_reusable"`
	GrantsDeploymentGate bool `json:"grants_deployment_gate"`
}

func currentOperatorVerifierRevocationPolicy(kind string) operatorVerifierRevocationPolicy {
	return operatorVerifierRevocationPolicy{
		RegistryRowRetained: true, EvidenceRetained: true,
		DisplayNameReusable: false, GrantsDeploymentGate: VerifierKindGrantsDeploymentGate(kind),
	}
}

// OperatorVerifierRevocationPreviewResult states the impact in job terms.
//
// ⚠ EvidenceRows and JobsLosingOnlyProducer are shown but deliberately not in
// the preview digest. A credential that is actively writing would otherwise
// change its own impact between preview and apply and could never be revoked —
// and revoking a compromised credential must always be completable. The
// revision CAS still binds the verifier's own row.
type OperatorVerifierRevocationPreviewResult struct {
	VerifierID             string    `json:"verifier_id"`
	Kind                   string    `json:"kind"`
	DisplayName            string    `json:"display_name"`
	FailureDomain          string    `json:"failure_domain"`
	CreatedAt              time.Time `json:"created_at"`
	Revision               int64     `json:"revision"`
	PreviewedAt            time.Time `json:"previewed_at"`
	EvidenceRows           int64     `json:"evidence_rows"`
	JobsLosingOnlyProducer int64     `json:"jobs_losing_only_producer"`
	RegistryRowRetained    bool      `json:"registry_row_retained"`
	EvidenceRetained       bool      `json:"evidence_retained"`
	DisplayNameReusable    bool      `json:"display_name_reusable"`
	GrantsDeploymentGate   bool      `json:"grants_deployment_gate"`
	PreviewDigest          string    `json:"preview_digest"`
}

type OperatorVerifierRevocationRequest struct {
	VerifierID         string
	ExpectedRevision   *int64
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	RequestDigest      string
	Audit              AuditEntry
}

type OperatorVerifierRevocationResult struct {
	VerifierID             string    `json:"verifier_id"`
	Kind                   string    `json:"kind"`
	DisplayName            string    `json:"display_name"`
	FailureDomain          string    `json:"failure_domain"`
	RevokedAt              time.Time `json:"revoked_at"`
	PreviousRevision       int64     `json:"previous_revision"`
	Revision               int64     `json:"revision"`
	EvidenceRows           int64     `json:"evidence_rows"`
	JobsLosingOnlyProducer int64     `json:"jobs_losing_only_producer"`
	RegistryRowRetained    bool      `json:"registry_row_retained"`
	EvidenceRetained       bool      `json:"evidence_retained"`
	PreviewDigest          string    `json:"preview_digest"`
	Replayed               bool      `json:"replayed"`
	Audited                bool      `json:"-"`
}

type operatorVerifierRevocationReceipt struct {
	VerifierID             string    `json:"verifier_id"`
	Kind                   string    `json:"kind"`
	DisplayName            string    `json:"display_name"`
	FailureDomain          string    `json:"failure_domain"`
	RevokedAt              time.Time `json:"revoked_at"`
	PreviousRevision       int64     `json:"previous_revision"`
	Revision               int64     `json:"revision"`
	EvidenceRows           int64     `json:"evidence_rows"`
	JobsLosingOnlyProducer int64     `json:"jobs_losing_only_producer"`
	PreviewDigest          string    `json:"preview_digest"`
}

// PreviewOperatorVerifierRevocation is read-only and consumes no key.
func (s *Store) PreviewOperatorVerifierRevocation(verifierID string) (OperatorVerifierRevocationPreviewResult, error) {
	verifier, impact, err := loadOperatorVerifierRevocationTarget(s.db, verifierID)
	if err != nil {
		return OperatorVerifierRevocationPreviewResult{}, err
	}
	policy := currentOperatorVerifierRevocationPolicy(verifier.Kind)
	return OperatorVerifierRevocationPreviewResult{
		VerifierID: verifier.VerifierID, Kind: verifier.Kind, DisplayName: verifier.DisplayName,
		FailureDomain: verifier.FailureDomain, CreatedAt: verifier.CreatedAt,
		Revision: verifier.Revision, PreviewedAt: s.now().UTC(),
		EvidenceRows: impact.EvidenceRows, JobsLosingOnlyProducer: impact.JobsLosingOnlyProducer,
		RegistryRowRetained: policy.RegistryRowRetained, EvidenceRetained: policy.EvidenceRetained,
		DisplayNameReusable: policy.DisplayNameReusable, GrantsDeploymentGate: policy.GrantsDeploymentGate,
		PreviewDigest: operatorVerifierRevocationPreviewDigest(verifier, policy),
	}, nil
}

type operatorVerifierRevocationImpact struct {
	EvidenceRows           int64
	JobsLosingOnlyProducer int64
}

func loadOperatorVerifierRevocationTarget(q operatorRowQuerier, verifierID string) (
	Verifier, operatorVerifierRevocationImpact, error,
) {
	verifier, err := scanVerifier(q.QueryRow(`SELECT `+verifierColumns+`
 FROM verifiers WHERE verifier_id=?`, verifierID))
	if errors.Is(err, sql.ErrNoRows) {
		return Verifier{}, operatorVerifierRevocationImpact{}, operatorError(
			OperatorCodeVerifierNotFound, "找不到這個 verifier")
	}
	if err != nil {
		return Verifier{}, operatorVerifierRevocationImpact{}, fmt.Errorf("store: load verifier revocation target: %w", err)
	}
	if verifier.RevokedAt != nil {
		return Verifier{}, operatorVerifierRevocationImpact{}, operatorError(
			OperatorCodeVerifierAlreadyRevoked, "這個 verifier 已經撤銷過了")
	}
	impact, err := operatorVerifierRevocationImpactFor(q, verifierID)
	if err != nil {
		return Verifier{}, operatorVerifierRevocationImpact{}, err
	}
	return verifier, impact, nil
}

// operatorVerifierRevocationImpactFor counts what an operator loses. The second
// number is the one that matters: jobs whose only live independent producer is
// this verifier become producer_revoked, not absent and not passed.
func operatorVerifierRevocationImpactFor(q operatorRowQuerier, verifierID string) (
	operatorVerifierRevocationImpact, error,
) {
	var impact operatorVerifierRevocationImpact
	if err := q.QueryRow(`SELECT COUNT(*) FROM verification_results
 WHERE verifier_id=? AND evidence_role=?`, verifierID, JobVerificationRoleIndependent).
		Scan(&impact.EvidenceRows); err != nil {
		return operatorVerifierRevocationImpact{}, fmt.Errorf("store: count verifier evidence rows: %w", err)
	}
	if err := q.QueryRow(`SELECT COUNT(*) FROM (
 SELECT r.job_id FROM verification_results AS r
   JOIN verifiers AS v ON v.verifier_id=r.verifier_id
  WHERE r.evidence_role=? AND v.revoked_at IS NULL
  GROUP BY r.job_id
 HAVING SUM(CASE WHEN r.verifier_id=? THEN 0 ELSE 1 END)=0
    AND SUM(CASE WHEN r.verifier_id=? THEN 1 ELSE 0 END)>0)`,
		JobVerificationRoleIndependent, verifierID, verifierID).
		Scan(&impact.JobsLosingOnlyProducer); err != nil {
		return operatorVerifierRevocationImpact{}, fmt.Errorf("store: count jobs losing their only verifier: %w", err)
	}
	return impact, nil
}

func operatorVerifierRevocationPreviewDigest(verifier Verifier,
	policy operatorVerifierRevocationPolicy,
) string {
	body := struct {
		Version       string                           `json:"version"`
		VerifierID    string                           `json:"verifier_id"`
		Kind          string                           `json:"kind"`
		DisplayName   string                           `json:"display_name"`
		FailureDomain string                           `json:"failure_domain"`
		CreatedAt     string                           `json:"created_at"`
		Revision      int64                            `json:"revision"`
		Policy        operatorVerifierRevocationPolicy `json:"policy"`
	}{
		operatorVerifierRevokeVersion, verifier.VerifierID, verifier.Kind, verifier.DisplayName,
		verifier.FailureDomain, fmtTime(verifier.CreatedAt), verifier.Revision, policy,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func operatorVerifierRevokeOperation(verifierID string) string {
	return operatorVerifierRevokeOperationPfx + verifierID
}

func canonicalOperatorVerifierRevocationRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeVerifierNotFound:
		return "找不到這個 verifier", true
	case OperatorCodeVerifierAlreadyRevoked:
		return "這個 verifier 已經撤銷過了", true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodeVerifierPreviewStale:
		return "preview_digest 與這個 verifier 目前的狀態不符；請重新預覽", true
	case OperatorCodePreconditionRequired:
		return "expected_revision 不可省略", true
	case OperatorCodePreconditionFailed:
		return "expected_revision 與這個 verifier 目前的 revision 不符；請重新讀取", true
	case OperatorCodeConfirmationMismatch:
		return "confirm_display_name 必須與這個 verifier 的 display_name 完全一致", true
	default:
		return "", false
	}
}

func historicalOperatorVerifierRevocationRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeVerifierNotFound:
		return "原 request 當時找不到這個 verifier", true
	case OperatorCodeVerifierAlreadyRevoked:
		return "原 request 當時這個 verifier 已經撤銷", true
	case OperatorCodePreviewRequired:
		return "原 request 當時缺少必要的 preview_digest", true
	case OperatorCodeVerifierPreviewStale:
		return "原 request 的 preview_digest 不符合當時的 verifier 狀態", true
	case OperatorCodePreconditionRequired:
		return "原 request 當時缺少必要的 expected_revision", true
	case OperatorCodePreconditionFailed:
		return "原 request 的 expected_revision 不符合當時的 revision", true
	case OperatorCodeConfirmationMismatch:
		return "原 request 的 confirm_display_name 與當時的 display_name 不符", true
	default:
		return "", false
	}
}

func operatorVerifierRevocationSuccessDetail(receipt operatorVerifierRevocationReceipt) string {
	return fmt.Sprintf("verifier_id=%s，kind=%s，failure_domain=%s，revision=%d→%d，evidence_rows=%d，jobs_losing_only_producer=%d，registry_row_retained=true，evidence_retained=true",
		receipt.VerifierID, receipt.Kind, receipt.FailureDomain,
		receipt.PreviousRevision, receipt.Revision, receipt.EvidenceRows, receipt.JobsLosingOnlyProducer)
}

// ApplyOperatorVerifierRevocation commits the revocation, receipt, idempotency
// decision and audit evidence together.
func (s *Store) ApplyOperatorVerifierRevocation(req OperatorVerifierRevocationRequest) (OperatorVerifierRevocationResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorVerifierRevocationResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorVerifierRevocationResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	audit := req.Audit
	audit.Action = AuditVerifierRevoke
	audit.Subject = req.VerifierID
	audit.Reason = req.Reason
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: begin operator verifier revocation: %w", err)
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
		return s.replayOperatorVerifierRevocation(tx, req, audit, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: inspect verifier revocation idempotency key: %w", err)
	}

	reject := func(code string) (OperatorVerifierRevocationResult, error) {
		detail, ok := canonicalOperatorVerifierRevocationRejectionDetail(code)
		if !ok {
			return OperatorVerifierRevocationResult{}, errors.New("store: invalid verifier revocation rejection code")
		}
		storedDetail := truncAudit(operatorVerifierRevokeRejectPrefix+code+"；"+detail, auditMaxReason)
		audit.OK, audit.Detail = false, storedDetail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorVerifierRevocationResult{}, fmt.Errorf("store: record verifier revocation rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey,
			operatorVerifierRevokeOperation(req.VerifierID), req.RequestDigest,
			code, storedDetail, fmtTime(writerNow)); err != nil {
			return OperatorVerifierRevocationResult{}, fmt.Errorf("store: persist verifier revocation rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorVerifierRevocationResult{}, fmt.Errorf("store: commit verifier revocation rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorVerifierRevocationResult{}, rejection
	}

	if req.ExpectedRevision == nil {
		return reject(OperatorCodePreconditionRequired)
	}
	if strings.TrimSpace(req.PreviewDigest) == "" {
		return reject(OperatorCodePreviewRequired)
	}
	verifier, impact, err := loadOperatorVerifierRevocationTarget(tx, req.VerifierID)
	if err != nil {
		var rejection *OperatorRequestError
		if errors.As(err, &rejection) && (rejection.Code == OperatorCodeVerifierNotFound ||
			rejection.Code == OperatorCodeVerifierAlreadyRevoked) {
			return reject(rejection.Code)
		}
		return OperatorVerifierRevocationResult{}, err
	}
	audit.Subject = verifier.DisplayName
	policy := currentOperatorVerifierRevocationPolicy(verifier.Kind)
	if req.PreviewDigest != operatorVerifierRevocationPreviewDigest(verifier, policy) {
		return reject(OperatorCodeVerifierPreviewStale)
	}
	if *req.ExpectedRevision != verifier.Revision {
		return reject(OperatorCodePreconditionFailed)
	}
	if req.ConfirmDisplayName != verifier.DisplayName {
		return reject(OperatorCodeConfirmationMismatch)
	}
	if !validJobEvidenceTime(writerNow) {
		return OperatorVerifierRevocationResult{}, errors.New("store: verifier revocation clock is out of range")
	}
	res, err := tx.Exec(`UPDATE verifiers SET revoked_at=?,revision=revision+1
	 WHERE verifier_id=? AND revision=? AND revoked_at IS NULL`,
		fmtTime(writerNow), verifier.VerifierID, verifier.Revision)
	if err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: revoke operator verifier: %w", err)
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return OperatorVerifierRevocationResult{}, errors.New("store: verifier changed during revocation")
	}
	receipt := operatorVerifierRevocationReceipt{
		VerifierID: verifier.VerifierID, Kind: verifier.Kind, DisplayName: verifier.DisplayName,
		FailureDomain: verifier.FailureDomain, RevokedAt: writerNow,
		PreviousRevision: verifier.Revision, Revision: verifier.Revision + 1,
		EvidenceRows: impact.EvidenceRows, JobsLosingOnlyProducer: impact.JobsLosingOnlyProducer,
		PreviewDigest: req.PreviewDigest,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: encode verifier revocation receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey,
		operatorVerifierRevokeOperation(req.VerifierID), req.RequestDigest,
		string(raw), fmtTime(writerNow)); err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: persist verifier revocation receipt: %w", err)
	}
	audit.OK = true
	audit.Detail = operatorVerifierRevocationSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: record verifier revocation audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: commit verifier revocation: %w", err)
	}
	return operatorVerifierRevocationResultFrom(receipt, policy, false), nil
}

// operatorVerifierRevocationResultFrom is only reached after the audit row has
// been written inside this transaction, so Audited belongs to the shape. A
// caller that forgot it did not lose a row — it made the service-level fallback
// write a second, identical one.
func operatorVerifierRevocationResultFrom(receipt operatorVerifierRevocationReceipt,
	policy operatorVerifierRevocationPolicy, replayed bool,
) OperatorVerifierRevocationResult {
	return OperatorVerifierRevocationResult{
		VerifierID: receipt.VerifierID, Kind: receipt.Kind, DisplayName: receipt.DisplayName,
		FailureDomain: receipt.FailureDomain, RevokedAt: receipt.RevokedAt,
		PreviousRevision: receipt.PreviousRevision, Revision: receipt.Revision,
		EvidenceRows: receipt.EvidenceRows, JobsLosingOnlyProducer: receipt.JobsLosingOnlyProducer,
		RegistryRowRetained: policy.RegistryRowRetained, EvidenceRetained: policy.EvidenceRetained,
		PreviewDigest: receipt.PreviewDigest, Replayed: replayed, Audited: true,
	}
}

func (s *Store) replayOperatorVerifierRevocation(tx *sql.Tx, req OperatorVerifierRevocationRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorVerifierRevocationResult, error) {
	if cached.Operation != operatorVerifierRevokeOperation(req.VerifierID) ||
		cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK, audit.Detail = false, "idempotency conflict："+detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorVerifierRevocationResult{}, fmt.Errorf("store: record verifier revocation conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorVerifierRevocationResult{}, fmt.Errorf("store: commit verifier revocation conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorVerifierRevocationResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		historical, allowed := historicalOperatorVerifierRevocationRejectionDetail(cached.ErrorCode.String)
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			!allowed || !strings.HasPrefix(cached.ErrorDetail.String,
			operatorVerifierRevokeRejectPrefix+cached.ErrorCode.String+"；") ||
			!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
			return s.rejectInvalidOperatorVerifierRevocationCache(tx, audit)
		}
		valid, err := validateOperatorVerifierRevocationRejectionEvidence(tx, cached, req)
		if err != nil {
			return OperatorVerifierRevocationResult{}, fmt.Errorf("store: inspect cached verifier revocation rejection evidence: %w", err)
		}
		if !valid {
			return s.rejectInvalidOperatorVerifierRevocationCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorVerifierRevocationResult{}, fmt.Errorf("store: record rejected verifier revocation replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorVerifierRevocationResult{}, fmt.Errorf("store: commit rejected verifier revocation replay audit: %w", err)
		}
		return OperatorVerifierRevocationResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid ||
		cached.ErrorDetail.Valid || !canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return s.rejectInvalidOperatorVerifierRevocationCache(tx, audit)
	}
	receipt, err := decodeOperatorVerifierRevocationReceipt(cached.ResponseJSON.String)
	if err != nil {
		return s.rejectInvalidOperatorVerifierRevocationCache(tx, audit)
	}
	if err := validateOperatorVerifierRevocationReceipt(receipt, req, cached.CreatedAt); err != nil {
		return s.rejectInvalidOperatorVerifierRevocationCache(tx, audit)
	}
	valid, err := validateOperatorVerifierRevocationSuccessEvidence(tx, receipt, req)
	if err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: inspect cached verifier revocation evidence: %w", err)
	}
	if !valid {
		return s.rejectInvalidOperatorVerifierRevocationCache(tx, audit)
	}
	audit.Subject = receipt.DisplayName
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + "沒有再次撤銷；credential 自原判決起就已失效，名冊列與既有證據都還在"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: record successful verifier revocation replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: commit successful verifier revocation replay audit: %w", err)
	}
	return operatorVerifierRevocationResultFrom(receipt, currentOperatorVerifierRevocationPolicy(receipt.Kind), true), nil
}

func (s *Store) rejectInvalidOperatorVerifierRevocationCache(tx *sql.Tx,
	audit AuditEntry,
) (OperatorVerifierRevocationResult, error) {
	audit.MachineID, audit.Reason = "", ""
	audit.OK, audit.Detail = false, operatorVerifierRevokeCacheInvalid
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: record invalid verifier revocation cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorVerifierRevocationResult{}, fmt.Errorf("store: commit invalid verifier revocation cache audit: %w", err)
	}
	return OperatorVerifierRevocationResult{Audited: true},
		errors.New("store: operator verifier revocation idempotency cache is invalid")
}

func decodeOperatorVerifierRevocationReceipt(raw string) (operatorVerifierRevocationReceipt, error) {
	var receipt operatorVerifierRevocationReceipt
	err := decodeStrictCachedObject(raw, map[string]any{
		"verifier_id":               &receipt.VerifierID,
		"kind":                      &receipt.Kind,
		"display_name":              &receipt.DisplayName,
		"failure_domain":            &receipt.FailureDomain,
		"revoked_at":                &receipt.RevokedAt,
		"previous_revision":         &receipt.PreviousRevision,
		"revision":                  &receipt.Revision,
		"evidence_rows":             &receipt.EvidenceRows,
		"jobs_losing_only_producer": &receipt.JobsLosingOnlyProducer,
		"preview_digest":            &receipt.PreviewDigest,
	})
	if err != nil {
		return operatorVerifierRevocationReceipt{}, err
	}
	return receipt, nil
}

func validateOperatorVerifierRevocationReceipt(receipt operatorVerifierRevocationReceipt,
	req OperatorVerifierRevocationRequest, cachedCreatedAt string,
) error {
	if receipt.VerifierID != req.VerifierID || receipt.DisplayName == "" ||
		receipt.PreviewDigest != req.PreviewDigest ||
		receipt.PreviousRevision < 1 || receipt.Revision != receipt.PreviousRevision+1 ||
		receipt.EvidenceRows < 0 || receipt.JobsLosingOnlyProducer < 0 ||
		!canonicalOperatorTime(receipt.RevokedAt) ||
		fmtTime(receipt.RevokedAt) != cachedCreatedAt {
		return errors.New("store: cached verifier revocation receipt is inconsistent")
	}
	if req.ExpectedRevision != nil && *req.ExpectedRevision != receipt.PreviousRevision {
		return errors.New("store: cached verifier revocation receipt has another expected revision")
	}
	if req.ConfirmDisplayName != receipt.DisplayName {
		return errors.New("store: cached verifier revocation receipt has another display name")
	}
	return nil
}

func validateOperatorVerifierRevocationRejectionEvidence(tx *sql.Tx, cached operatorCachedRequest,
	req OperatorVerifierRevocationRequest,
) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id IS NULL AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='failed'
	   AND COALESCE(detail,'')=?`,
		string(AuditVerifierRevoke), truncAudit(req.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, cached.CreatedAt,
		cached.ErrorDetail.String).Scan(&count)
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

func validateOperatorVerifierRevocationSuccessEvidence(tx *sql.Tx,
	receipt operatorVerifierRevocationReceipt, req OperatorVerifierRevocationRequest,
) (bool, error) {
	var kind, displayName, failureDomain string
	var revision int64
	var revokedAt sql.NullString
	err := tx.QueryRow(`SELECT kind,display_name,failure_domain,revoked_at,revision
	 FROM verifiers WHERE verifier_id=?`, receipt.VerifierID).Scan(
		&kind, &displayName, &failureDomain, &revokedAt, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if kind != receipt.Kind || displayName != receipt.DisplayName ||
		failureDomain != receipt.FailureDomain || !revokedAt.Valid ||
		revokedAt.String != fmtTime(receipt.RevokedAt) || revision < receipt.Revision {
		return false, nil
	}
	var count int
	err = tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id IS NULL AND subject=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='ok'
	   AND COALESCE(detail,'')=?`,
		string(AuditVerifierRevoke), truncAudit(receipt.DisplayName, auditMaxReason),
		truncAudit(req.Reason, auditMaxReason), req.IdempotencyKey, req.RequestDigest,
		fmtTime(receipt.RevokedAt), operatorVerifierRevocationSuccessDetail(receipt)).Scan(&count)
	if err != nil {
		return false, err
	}
	return count == 1, nil
}
