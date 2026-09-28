package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	OperatorCodeEnrollTokenNotPending = "ENROLLMENT_TOKEN_NOT_PENDING"

	operatorEnrollTokenRevocationVersion         = "v1"
	operatorEnrollTokenRevocationOperationPrefix = "enrollment-token-revocation:v1:"
	operatorEnrollTokenRevocationCacheInvalid    = "operator enrollment token revocation idempotency cache invalid；未回放結果"
	operatorEnrollTokenRevocationRejectPrefix    = "operator enrollment token revocation rejection code="
)

var ErrEnrollTokenNotPending = errors.New("store: enrollment token is not pending")

// OperatorPendingEnrollTokenResult is the public identity of one unused
// enrollment ticket. Its token hash remains private to the Store.
type OperatorPendingEnrollTokenResult struct {
	MachineID      string    `json:"machine_id"`
	DisplayName    string    `json:"display_name"`
	TokenCreatedAt time.Time `json:"token_created_at"`
	TokenExpiresAt time.Time `json:"token_expires_at"`
	TokenExpired   bool      `json:"token_expired"`
}

type OperatorEnrollTokenRevocationPreviewResult struct {
	MachineID                     string    `json:"machine_id"`
	DisplayName                   string    `json:"display_name"`
	TokenCreatedAt                time.Time `json:"token_created_at"`
	TokenExpiresAt                time.Time `json:"token_expires_at"`
	TokenExpired                  bool      `json:"token_expired"`
	PreviewedAt                   time.Time `json:"previewed_at"`
	RegistryRetained              bool      `json:"registry_retained"`
	DenominatorDelta              int64     `json:"denominator_delta"`
	ActiveAgentCredentialAffected bool      `json:"active_agent_credential_affected"`
	PreviewDigest                 string    `json:"preview_digest"`
}

type OperatorEnrollTokenRevocationRequest struct {
	MachineID      string
	PreviewDigest  string
	Reason         string
	IdempotencyKey string
	RequestDigest  string
	Audit          AuditEntry
}

type OperatorEnrollTokenRevocationResult struct {
	MachineID                     string    `json:"machine_id"`
	DisplayName                   string    `json:"display_name"`
	TokenCreatedAt                time.Time `json:"token_created_at"`
	TokenExpiresAt                time.Time `json:"token_expires_at"`
	TokenWasExpired               bool      `json:"token_was_expired"`
	RevokedAt                     time.Time `json:"revoked_at"`
	RegistryRetained              bool      `json:"registry_retained"`
	DenominatorDelta              int64     `json:"denominator_delta"`
	ActiveAgentCredentialAffected bool      `json:"active_agent_credential_affected"`
	PreviewDigest                 string    `json:"preview_digest"`
	Replayed                      bool      `json:"replayed"`
	Audited                       bool      `json:"-"`
}

type operatorEnrollTokenRevocationPolicy struct {
	RegistryRetained              bool  `json:"registry_retained"`
	DenominatorDelta              int64 `json:"denominator_delta"`
	ActiveAgentCredentialAffected bool  `json:"active_agent_credential_affected"`
}

func currentOperatorEnrollTokenRevocationPolicy() operatorEnrollTokenRevocationPolicy {
	return operatorEnrollTokenRevocationPolicy{
		RegistryRetained: true, DenominatorDelta: 0, ActiveAgentCredentialAffected: false,
	}
}

type operatorPendingEnrollTokenIdentity struct {
	OperatorPendingEnrollTokenResult
	tokenHash string
}

type operatorEnrollTokenRevocationReceipt struct {
	MachineID                     string    `json:"machine_id"`
	DisplayName                   string    `json:"display_name"`
	TokenCreatedAt                time.Time `json:"token_created_at"`
	TokenExpiresAt                time.Time `json:"token_expires_at"`
	TokenWasExpired               bool      `json:"token_was_expired"`
	RevokedAt                     time.Time `json:"revoked_at"`
	RegistryRetained              bool      `json:"registry_retained"`
	DenominatorDelta              int64     `json:"denominator_delta"`
	ActiveAgentCredentialAffected bool      `json:"active_agent_credential_affected"`
	PreviewDigest                 string    `json:"preview_digest"`
}

type operatorRowQuerier interface {
	QueryRow(string, ...any) *sql.Row
}

// OperatorPendingEnrollToken returns only non-secret ticket metadata. An
// expired but unused ticket is still pending and remains visible.
func (s *Store) OperatorPendingEnrollToken(machineID string) (OperatorPendingEnrollTokenResult, error) {
	identity, err := loadOperatorPendingEnrollToken(s.db, machineID, s.now().UTC())
	if err != nil {
		return OperatorPendingEnrollTokenResult{}, err
	}
	return identity.OperatorPendingEnrollTokenResult, nil
}

// PreviewOperatorEnrollTokenRevocation is read-only. Its digest binds the
// hidden exact ticket identity and every promised impact.
func (s *Store) PreviewOperatorEnrollTokenRevocation(machineID string) (OperatorEnrollTokenRevocationPreviewResult, error) {
	now := s.now().UTC()
	identity, err := loadOperatorPendingEnrollToken(s.db, machineID, now)
	if err != nil {
		return OperatorEnrollTokenRevocationPreviewResult{}, err
	}
	policy := currentOperatorEnrollTokenRevocationPolicy()
	return OperatorEnrollTokenRevocationPreviewResult{
		MachineID: identity.MachineID, DisplayName: identity.DisplayName,
		TokenCreatedAt: identity.TokenCreatedAt, TokenExpiresAt: identity.TokenExpiresAt,
		TokenExpired: identity.TokenExpired, PreviewedAt: now,
		RegistryRetained: policy.RegistryRetained, DenominatorDelta: policy.DenominatorDelta,
		ActiveAgentCredentialAffected: policy.ActiveAgentCredentialAffected,
		PreviewDigest:                 operatorEnrollTokenRevocationPreviewDigest(identity, policy),
	}, nil
}

func loadOperatorPendingEnrollToken(q operatorRowQuerier, machineID string, now time.Time) (operatorPendingEnrollTokenIdentity, error) {
	var machineName, tokenHash, tokenName, createdRaw, expiresRaw string
	var pendingCount int
	err := q.QueryRow(`SELECT m.display_name,
	 COALESCE(t.token_hash,''),COALESCE(t.display_name,''),
	 COALESCE(t.created_at,''),COALESCE(t.expires_at,''),
	 (SELECT COUNT(*) FROM enrollment_tokens c
	   WHERE c.used_by=m.machine_id AND c.used_at IS NULL)
	 FROM machine_registry m
	 LEFT JOIN enrollment_tokens t ON t.token_hash=(
	   SELECT e.token_hash FROM enrollment_tokens e
	    WHERE e.used_by=m.machine_id AND e.used_at IS NULL
	    ORDER BY e.created_at DESC,e.token_hash DESC LIMIT 1)
	 WHERE m.machine_id=?`, machineID).Scan(
		&machineName, &tokenHash, &tokenName, &createdRaw, &expiresRaw, &pendingCount)
	if errors.Is(err, sql.ErrNoRows) {
		return operatorPendingEnrollTokenIdentity{}, operatorError(
			OperatorCodeMachineNotFound, "找不到這台機器")
	}
	if err != nil {
		return operatorPendingEnrollTokenIdentity{}, fmt.Errorf("store: inspect pending operator enrollment token: %w", err)
	}
	if pendingCount == 0 && tokenHash == "" {
		return operatorPendingEnrollTokenIdentity{}, operatorError(
			OperatorCodeEnrollTokenNotPending, "這台機器沒有尚未兌換的 enroll token")
	}
	// Issuance currently creates one registry row per ticket, but the schema did
	// not historically enforce that invariant. Never delete one selected row
	// and claim the machine has no pending access while another credential is
	// still redeemable.
	if pendingCount != 1 {
		return operatorPendingEnrollTokenIdentity{}, errors.New("store: machine has ambiguous pending enrollment token identity")
	}
	createdAt, expiresAt := parseTime(createdRaw), parseTime(expiresRaw)
	if machineID == "" || machineName == "" || tokenName != machineName ||
		!validLowerSHA256(tokenHash) || !canonicalOperatorEnrollCacheTime(createdRaw) ||
		!canonicalOperatorEnrollCacheTime(expiresRaw) || !expiresAt.After(createdAt) {
		return operatorPendingEnrollTokenIdentity{}, errors.New("store: pending enrollment token identity is invalid")
	}
	return operatorPendingEnrollTokenIdentity{
		OperatorPendingEnrollTokenResult: OperatorPendingEnrollTokenResult{
			MachineID: machineID, DisplayName: machineName,
			TokenCreatedAt: createdAt, TokenExpiresAt: expiresAt,
			TokenExpired: !now.Before(expiresAt),
		},
		tokenHash: tokenHash,
	}, nil
}

func validLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func operatorEnrollTokenRevocationPreviewDigest(identity operatorPendingEnrollTokenIdentity,
	policy operatorEnrollTokenRevocationPolicy,
) string {
	body := struct {
		Version        string                              `json:"version"`
		MachineID      string                              `json:"machine_id"`
		DisplayName    string                              `json:"display_name"`
		TokenHash      string                              `json:"token_hash"`
		TokenCreatedAt string                              `json:"token_created_at"`
		TokenExpiresAt string                              `json:"token_expires_at"`
		Policy         operatorEnrollTokenRevocationPolicy `json:"policy"`
	}{
		Version: operatorEnrollTokenRevocationVersion, MachineID: identity.MachineID,
		DisplayName: identity.DisplayName, TokenHash: identity.tokenHash,
		TokenCreatedAt: fmtTime(identity.TokenCreatedAt), TokenExpiresAt: fmtTime(identity.TokenExpiresAt),
		Policy: policy,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func operatorEnrollTokenRevocationOperation(machineID string) string {
	return operatorEnrollTokenRevocationOperationPrefix + machineID
}

func operatorEnrollTokenRevocationSuccessDetail(receipt operatorEnrollTokenRevocationReceipt) string {
	return fmt.Sprintf("pending enroll token revoked；token_created_at=%s token_expires_at=%s registry_retained=true denominator_delta=0 active_agent_credential_affected=false",
		fmtTime(receipt.TokenCreatedAt), fmtTime(receipt.TokenExpiresAt))
}

// ApplyOperatorEnrollTokenRevocation commits the exact token DELETE, durable
// receipt, idempotency decision, and original audit evidence together.
func (s *Store) ApplyOperatorEnrollTokenRevocation(req OperatorEnrollTokenRevocationRequest) (OperatorEnrollTokenRevocationResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorEnrollTokenRevocationResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorEnrollTokenRevocationResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	audit := req.Audit
	audit.Action = AuditRevokeToken
	audit.MachineID = req.MachineID
	audit.Subject = req.MachineID
	audit.Reason = req.Reason
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: begin operator enrollment token revocation: %w", err)
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
		return s.replayOperatorEnrollTokenRevocation(tx, req, audit, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: inspect enrollment token revocation idempotency key: %w", err)
	}

	reject := func(code string) (OperatorEnrollTokenRevocationResult, error) {
		detail, ok := canonicalOperatorEnrollTokenRevocationRejectionDetail(code)
		if !ok {
			return OperatorEnrollTokenRevocationResult{}, errors.New("store: invalid enrollment token revocation rejection code")
		}
		storedDetail := truncAudit(operatorEnrollTokenRevocationRejectPrefix+code+"；"+detail, auditMaxReason)
		audit.OK, audit.Detail = false, storedDetail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: record enrollment token revocation rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey,
			operatorEnrollTokenRevocationOperation(req.MachineID), req.RequestDigest,
			code, storedDetail, fmtTime(writerNow)); err != nil {
			return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: persist enrollment token revocation rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: commit enrollment token revocation rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorEnrollTokenRevocationResult{}, rejection
	}

	if strings.TrimSpace(req.PreviewDigest) == "" {
		return reject(OperatorCodePreviewRequired)
	}
	identity, err := loadOperatorPendingEnrollToken(tx, req.MachineID, writerNow)
	if err != nil {
		var rejection *OperatorRequestError
		if errors.As(err, &rejection) && (rejection.Code == OperatorCodeMachineNotFound ||
			rejection.Code == OperatorCodeEnrollTokenNotPending) {
			return reject(rejection.Code)
		}
		return OperatorEnrollTokenRevocationResult{}, err
	}
	policy := currentOperatorEnrollTokenRevocationPolicy()
	expectedPreview := operatorEnrollTokenRevocationPreviewDigest(identity, policy)
	if req.PreviewDigest != expectedPreview {
		return reject(OperatorCodePreviewStale)
	}
	if writerNow.Before(identity.TokenCreatedAt) {
		return OperatorEnrollTokenRevocationResult{}, errors.New("store: revocation writer clock predates enrollment ticket")
	}
	res, err := tx.Exec(`DELETE FROM enrollment_tokens
	 WHERE token_hash=? AND used_by=? AND used_at IS NULL`, identity.tokenHash, req.MachineID)
	if err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: revoke exact pending enrollment token: %w", err)
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return OperatorEnrollTokenRevocationResult{}, errors.New("store: pending enrollment token changed during revocation")
	}
	receipt := operatorEnrollTokenRevocationReceipt{
		MachineID: identity.MachineID, DisplayName: identity.DisplayName,
		TokenCreatedAt: identity.TokenCreatedAt, TokenExpiresAt: identity.TokenExpiresAt,
		TokenWasExpired: !writerNow.Before(identity.TokenExpiresAt), RevokedAt: writerNow,
		RegistryRetained: policy.RegistryRetained, DenominatorDelta: policy.DenominatorDelta,
		ActiveAgentCredentialAffected: policy.ActiveAgentCredentialAffected,
		PreviewDigest:                 req.PreviewDigest,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: encode enrollment token revocation receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey,
		operatorEnrollTokenRevocationOperation(req.MachineID), req.RequestDigest,
		string(raw), fmtTime(writerNow)); err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: persist enrollment token revocation receipt: %w", err)
	}
	audit.MachineID = identity.MachineID
	audit.Subject = identity.DisplayName
	audit.OK = true
	audit.Detail = operatorEnrollTokenRevocationSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: record enrollment token revocation success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: commit enrollment token revocation: %w", err)
	}
	result := operatorEnrollTokenRevocationResult(receipt)
	result.Audited = true
	return result, nil
}

func canonicalOperatorEnrollTokenRevocationRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeMachineNotFound:
		return "找不到這台機器", true
	case OperatorCodeEnrollTokenNotPending:
		return "這台機器沒有尚未兌換的 enroll token", true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodePreviewStale:
		return "preview_digest 與目前的 pending enroll token 或 revocation policy 不符；請重新預覽", true
	default:
		return "", false
	}
}

func historicalOperatorEnrollTokenRevocationRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeMachineNotFound:
		return "原 request 指定的 machine 當時不存在", true
	case OperatorCodeEnrollTokenNotPending:
		return "原 request 指定的 machine 當時沒有尚未兌換的 enroll token", true
	case OperatorCodePreviewRequired:
		return "原 request 當時缺少必要的 preview_digest", true
	case OperatorCodePreviewStale:
		return "原 request 的 preview_digest 不符合當時的 ticket 或 policy", true
	default:
		return "", false
	}
}

func operatorEnrollTokenRevocationResult(receipt operatorEnrollTokenRevocationReceipt) OperatorEnrollTokenRevocationResult {
	return OperatorEnrollTokenRevocationResult{
		MachineID: receipt.MachineID, DisplayName: receipt.DisplayName,
		TokenCreatedAt: receipt.TokenCreatedAt, TokenExpiresAt: receipt.TokenExpiresAt,
		TokenWasExpired: receipt.TokenWasExpired, RevokedAt: receipt.RevokedAt,
		RegistryRetained: receipt.RegistryRetained, DenominatorDelta: receipt.DenominatorDelta,
		ActiveAgentCredentialAffected: receipt.ActiveAgentCredentialAffected,
		PreviewDigest:                 receipt.PreviewDigest,
	}
}

func (s *Store) replayOperatorEnrollTokenRevocation(tx *sql.Tx,
	req OperatorEnrollTokenRevocationRequest, audit AuditEntry, cached operatorCachedRequest,
) (OperatorEnrollTokenRevocationResult, error) {
	wantOperation := operatorEnrollTokenRevocationOperation(req.MachineID)
	if cached.Operation != wantOperation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK = false
		audit.Detail = "idempotency conflict：" + detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: record revocation idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: commit revocation idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorEnrollTokenRevocationResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		historical, allowed := historicalOperatorEnrollTokenRevocationRejectionDetail(cached.ErrorCode.String)
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid || !allowed ||
			!strings.HasPrefix(cached.ErrorDetail.String,
				operatorEnrollTokenRevocationRejectPrefix+cached.ErrorCode.String+"；") ||
			!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
			return s.rejectInvalidOperatorEnrollTokenRevocationCache(tx, audit)
		}
		valid, err := validateOperatorEnrollTokenRevocationRejectionEvidence(tx, cached, req)
		if err != nil {
			return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: inspect revocation rejection evidence: %w", err)
		}
		if !valid {
			return s.rejectInvalidOperatorEnrollTokenRevocationCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: record revocation rejection replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: commit revocation rejection replay audit: %w", err)
		}
		rejection := operatorError(cached.ErrorCode.String, historical)
		rejection.Replayed, rejection.Audited = true, true
		return OperatorEnrollTokenRevocationResult{}, rejection
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid ||
		cached.ErrorDetail.Valid || !canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return s.rejectInvalidOperatorEnrollTokenRevocationCache(tx, audit)
	}
	receipt, err := decodeOperatorEnrollTokenRevocationReceipt(cached.ResponseJSON.String)
	if err != nil || validateOperatorEnrollTokenRevocationReceipt(receipt, req, cached.CreatedAt) != nil {
		return s.rejectInvalidOperatorEnrollTokenRevocationCache(tx, audit)
	}
	valid, err := validateOperatorEnrollTokenRevocationSuccessEvidence(tx, receipt, req)
	if err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: inspect revocation success evidence: %w", err)
	}
	if !valid {
		return s.rejectInvalidOperatorEnrollTokenRevocationCache(tx, audit)
	}
	audit.MachineID = receipt.MachineID
	audit.Subject = receipt.DisplayName
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + "原 pending enrollment ticket 已撤銷；沒有再次刪除 pending enrollment ticket；active agent credential 未受影響"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: record revocation success replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: commit revocation success replay audit: %w", err)
	}
	result := operatorEnrollTokenRevocationResult(receipt)
	result.Replayed, result.Audited = true, true
	return result, nil
}

func (s *Store) rejectInvalidOperatorEnrollTokenRevocationCache(tx *sql.Tx,
	audit AuditEntry,
) (OperatorEnrollTokenRevocationResult, error) {
	audit.Reason = ""
	audit.OK = false
	audit.Detail = operatorEnrollTokenRevocationCacheInvalid
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: record invalid revocation cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorEnrollTokenRevocationResult{}, fmt.Errorf("store: commit invalid revocation cache audit: %w", err)
	}
	return OperatorEnrollTokenRevocationResult{Audited: true},
		errors.New("store: operator enrollment token revocation idempotency cache is invalid")
}

func validateOperatorEnrollTokenRevocationRejectionEvidence(tx *sql.Tx,
	cached operatorCachedRequest, req OperatorEnrollTokenRevocationRequest,
) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id=? AND subject=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='failed'
	   AND COALESCE(detail,'')=?`, string(AuditRevokeToken), req.MachineID, truncAudit(req.MachineID, auditMaxReason),
		truncAudit(req.Reason, auditMaxReason), req.IdempotencyKey, req.RequestDigest,
		cached.CreatedAt, cached.ErrorDetail.String).Scan(&count)
	return count == 1, err
}

func validateOperatorEnrollTokenRevocationSuccessEvidence(tx *sql.Tx,
	receipt operatorEnrollTokenRevocationReceipt, req OperatorEnrollTokenRevocationRequest,
) (bool, error) {
	var machineCount, pendingCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM machine_registry WHERE machine_id=?`,
		receipt.MachineID).Scan(&machineCount); err != nil || machineCount != 1 {
		return false, err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens
	 WHERE used_by=? AND used_at IS NULL`, receipt.MachineID).Scan(&pendingCount); err != nil || pendingCount != 0 {
		return false, err
	}
	var auditCount int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id=? AND subject=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='ok'
	   AND COALESCE(detail,'')=?`, string(AuditRevokeToken), receipt.MachineID,
		truncAudit(receipt.DisplayName, auditMaxReason), truncAudit(req.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, fmtTime(receipt.RevokedAt),
		operatorEnrollTokenRevocationSuccessDetail(receipt)).Scan(&auditCount)
	return auditCount == 1, err
}

func validateOperatorEnrollTokenRevocationReceipt(receipt operatorEnrollTokenRevocationReceipt,
	req OperatorEnrollTokenRevocationRequest, cachedCreatedAt string,
) error {
	policy := currentOperatorEnrollTokenRevocationPolicy()
	if receipt.MachineID != req.MachineID || receipt.MachineID == "" || receipt.DisplayName == "" ||
		receipt.PreviewDigest != req.PreviewDigest || receipt.PreviewDigest == "" ||
		!receipt.TokenExpiresAt.After(receipt.TokenCreatedAt) || receipt.RevokedAt.Before(receipt.TokenCreatedAt) ||
		fmtTime(receipt.RevokedAt) != cachedCreatedAt ||
		!canonicalOperatorTime(receipt.TokenCreatedAt) ||
		!canonicalOperatorTime(receipt.TokenExpiresAt) ||
		!canonicalOperatorTime(receipt.RevokedAt) ||
		receipt.TokenWasExpired != !receipt.RevokedAt.Before(receipt.TokenExpiresAt) ||
		receipt.RegistryRetained != policy.RegistryRetained || receipt.DenominatorDelta != policy.DenominatorDelta ||
		receipt.ActiveAgentCredentialAffected != policy.ActiveAgentCredentialAffected {
		return errors.New("cached revocation receipt does not match request or policy")
	}
	return nil
}

func decodeOperatorEnrollTokenRevocationReceipt(raw string) (operatorEnrollTokenRevocationReceipt, error) {
	var receipt operatorEnrollTokenRevocationReceipt
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	opening, err := dec.Token()
	if err != nil {
		return receipt, err
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return receipt, errors.New("cached revocation receipt is not an object")
	}
	seen := make(map[string]bool, 10)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return receipt, err
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return receipt, errors.New("cached revocation receipt has invalid or duplicate field")
		}
		seen[name] = true
		var dst any
		switch name {
		case "machine_id":
			dst = &receipt.MachineID
		case "display_name":
			dst = &receipt.DisplayName
		case "token_created_at":
			dst = &receipt.TokenCreatedAt
		case "token_expires_at":
			dst = &receipt.TokenExpiresAt
		case "token_was_expired":
			dst = &receipt.TokenWasExpired
		case "revoked_at":
			dst = &receipt.RevokedAt
		case "registry_retained":
			dst = &receipt.RegistryRetained
		case "denominator_delta":
			dst = &receipt.DenominatorDelta
		case "active_agent_credential_affected":
			dst = &receipt.ActiveAgentCredentialAffected
		case "preview_digest":
			dst = &receipt.PreviewDigest
		default:
			return receipt, errors.New("cached revocation receipt has unknown field")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return receipt, errors.New("cached revocation receipt has invalid field value")
		}
		if err := json.Unmarshal(value, dst); err != nil {
			return receipt, err
		}
	}
	if closing, err := dec.Token(); err != nil || closing != json.Delim('}') {
		return receipt, errors.New("cached revocation receipt has invalid closing token")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return receipt, errors.New("cached revocation receipt has trailing JSON")
	}
	if len(seen) != 10 {
		return receipt, errors.New("cached revocation receipt is incomplete")
	}
	return receipt, nil
}
