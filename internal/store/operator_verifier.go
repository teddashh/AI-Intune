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
	OperatorCodeVerifierInvalid        = "VERIFIER_INVALID"
	OperatorCodeVerifierDomainInvalid  = "VERIFIER_DOMAIN_INVALID"
	OperatorCodeVerifierNameTaken      = "VERIFIER_NAME_TAKEN"
	OperatorCodeVerifierNotFound       = "VERIFIER_NOT_FOUND"
	OperatorCodeVerifierAlreadyRevoked = "VERIFIER_ALREADY_REVOKED"
	OperatorCodeVerifierPreviewStale   = "VERIFIER_PREVIEW_STALE"
)

// OperatorVerifierSeparationRule is the one sentence a registration promises.
// It is part of the preview digest, so a future change to the separation rule
// invalidates every preview taken under the old one.
const (
	OperatorVerifierSeparationRule    = "failure_domain_differs_from_job_machine"
	OperatorVerifierCredentialOneTime = "first-response-only"
)

const (
	operatorVerifierVersion            = "v2"
	operatorVerifierOperation          = "verifier-register:v1"
	operatorVerifierRevokeOperationPfx = "verifier-revocation:v1:"

	operatorVerifierCacheInvalidDetail      = "operator verifier idempotency cache invalid；未回放結果或 credential"
	operatorVerifierRejectionEvidencePrefix = "operator verifier rejection code="
)

// operatorVerifierPolicyIdentity is both the rendered impact and part of the
// preview precondition. Only the measured fleet-peer topology participates in
// the stable promotion gate; registering either other kind still adds evidence
// without changing a deployment decision.
type operatorVerifierPolicyIdentity struct {
	SeparationRule       string `json:"separation_rule"`
	CredentialDelivery   string `json:"credential_delivery"`
	EvidenceRole         string `json:"evidence_role"`
	RevocationKeepsRow   bool   `json:"revocation_keeps_row"`
	GrantsDeploymentGate bool   `json:"grants_deployment_gate"`
}

func currentOperatorVerifierPolicy(kind string) operatorVerifierPolicyIdentity {
	return operatorVerifierPolicyIdentity{
		SeparationRule:       OperatorVerifierSeparationRule,
		CredentialDelivery:   OperatorVerifierCredentialOneTime,
		EvidenceRole:         JobVerificationRoleIndependent,
		RevocationKeepsRow:   true,
		GrantsDeploymentGate: VerifierKindGrantsDeploymentGate(kind),
	}
}

// OperatorVerifierPreviewResult carries no credential and is safe to render.
type OperatorVerifierPreviewResult struct {
	Kind                 string    `json:"kind"`
	DisplayName          string    `json:"display_name"`
	FailureDomain        string    `json:"failure_domain"`
	PreviewedAt          time.Time `json:"previewed_at"`
	SeparationRule       string    `json:"separation_rule"`
	CredentialDelivery   string    `json:"credential_delivery"`
	EvidenceRole         string    `json:"evidence_role"`
	RevocationKeepsRow   bool      `json:"revocation_keeps_row"`
	GrantsDeploymentGate bool      `json:"grants_deployment_gate"`
	PreviewDigest        string    `json:"preview_digest"`
}

type OperatorVerifierCreateRequest struct {
	Kind           string
	DisplayName    string
	FailureDomain  string
	HubMachineID   string
	PreviewDigest  string
	Reason         string
	IdempotencyKey string
	RequestDigest  string
	Audit          AuditEntry
}

// OperatorVerifierCreateResult has two deliberately different success shapes.
// A fresh result carries Credential exactly once; a replay carries only the
// redacted receipt and tells the caller to revoke and register again.
type OperatorVerifierCreateResult struct {
	VerifierID    string    `json:"verifier_id"`
	Kind          string    `json:"kind"`
	DisplayName   string    `json:"display_name"`
	FailureDomain string    `json:"failure_domain"`
	CreatedAt     time.Time `json:"created_at"`
	Revision      int64     `json:"revision"`
	PreviewDigest string    `json:"preview_digest"`
	// Credential is excluded from generic serialization. Only the HTTP
	// adapter's guarded fresh-response DTO may put it on a wire.
	Credential       string `json:"-"`
	SecretAvailable  bool   `json:"secret_available"`
	Replayed         bool   `json:"replayed"`
	RecoveryRequired bool   `json:"recovery_required"`
	RecoveryAction   string `json:"recovery_action,omitempty"`
	Audited          bool   `json:"-"`
}

// operatorVerifierReceipt is the only success representation allowed in
// operator_idempotency.response_json. Credential and delivery state are
// deliberately absent so no refactor can cache the first body.
type operatorVerifierReceipt struct {
	VerifierID    string    `json:"verifier_id"`
	Kind          string    `json:"kind"`
	DisplayName   string    `json:"display_name"`
	FailureDomain string    `json:"failure_domain"`
	CreatedAt     time.Time `json:"created_at"`
	Revision      int64     `json:"revision"`
	PreviewDigest string    `json:"preview_digest"`
}

// ResolveHubMachineID answers "which registry row is the Hub's own host",
// using the same match hubInFleet uses. An empty string means the Hub host is
// not enrolled. Two matching rows is an error rather than a choice: registering
// a hub_prober against the wrong row would silently let it verify its own host.
func (s *Store) ResolveHubMachineID(hubHost string) (string, error) {
	if strings.TrimSpace(hubHost) == "" {
		return "", nil
	}
	rows, err := s.db.Query(`SELECT machine_id FROM machine_registry
 WHERE retired_at IS NULL AND (hostname=? OR display_name=?)`, hubHost, hubHost)
	if err != nil {
		return "", fmt.Errorf("store: resolve hub machine id: %w", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var machineID string
		if err := rows.Scan(&machineID); err != nil {
			return "", fmt.Errorf("store: scan hub machine id: %w", err)
		}
		found = append(found, machineID)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("store: finish hub machine id resolution: %w", err)
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", errors.New("store: hub host matches more than one registry row")
	}
}

// PreviewOperatorVerifier performs no write and consumes no idempotency key.
func (s *Store) PreviewOperatorVerifier(kind, displayName, failureDomain, hubMachineID string) (OperatorVerifierPreviewResult, error) {
	if err := s.validateOperatorVerifierIntent(s.db, kind, displayName, failureDomain, hubMachineID); err != nil {
		return OperatorVerifierPreviewResult{}, err
	}
	policy := currentOperatorVerifierPolicy(kind)
	return OperatorVerifierPreviewResult{
		Kind: kind, DisplayName: displayName, FailureDomain: failureDomain,
		PreviewedAt:        s.now().UTC(),
		SeparationRule:     policy.SeparationRule,
		CredentialDelivery: policy.CredentialDelivery,
		EvidenceRole:       policy.EvidenceRole,
		RevocationKeepsRow: policy.RevocationKeepsRow, GrantsDeploymentGate: policy.GrantsDeploymentGate,
		PreviewDigest: operatorVerifierPreviewDigest(kind, displayName, failureDomain, policy),
	}, nil
}

func operatorVerifierPreviewDigest(kind, displayName, failureDomain string,
	policy operatorVerifierPolicyIdentity,
) string {
	body := struct {
		Version       string                         `json:"version"`
		Kind          string                         `json:"kind"`
		DisplayName   string                         `json:"display_name"`
		FailureDomain string                         `json:"failure_domain"`
		Policy        operatorVerifierPolicyIdentity `json:"policy"`
	}{operatorVerifierVersion, kind, displayName, failureDomain, policy}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// validateOperatorVerifierIntent runs the same checks the create transaction
// runs, against the same reader. The domain check is not a pure function: it
// depends on the registry, so preview and apply must both resolve it.
func (s *Store) validateOperatorVerifierIntent(q verifierQueryRower,
	kind, displayName, failureDomain, hubMachineID string,
) error {
	if !validVerifierKind(kind) ||
		!validVerifierText(displayName, maxVerifierDisplayNameBytes) ||
		!validVerifierText(failureDomain, maxVerifierFailureDomainBytes) {
		return operatorVerifierRejection(OperatorCodeVerifierInvalid)
	}
	if err := validateVerifierFailureDomain(q, kind, failureDomain, hubMachineID); err != nil {
		if errors.Is(err, ErrInvalidVerifier) {
			return operatorVerifierRejection(OperatorCodeVerifierDomainInvalid)
		}
		return err
	}
	if verifierDisplayNameTaken(q, displayName) {
		return operatorVerifierRejection(OperatorCodeVerifierNameTaken)
	}
	return nil
}

func operatorVerifierRejection(code string) *OperatorRequestError {
	detail, ok := canonicalOperatorVerifierRejectionDetail(code)
	if !ok {
		return operatorError(code, "控制面 verifier request 被拒絕")
	}
	return operatorError(code, detail)
}

func canonicalOperatorVerifierRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeVerifierInvalid:
		return fmt.Sprintf("kind 必須是 %s、%s 或 %s；display_name 與 failure_domain 不可為空、不可有首尾空白或控制字元",
			VerifierKindFleetPeerAgent, VerifierKindHubProber, VerifierKindExternalJobRunner), true
	case OperatorCodeVerifierDomainInvalid:
		return "failure_domain 必須是名冊上一台未退役的機器；hub_prober 必須指向 Hub 自己那一台", true
	case OperatorCodeVerifierNameTaken:
		return "已經有 verifier 用這個 display_name", true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodeVerifierPreviewStale:
		return "preview_digest 與目前的 verifier policy 不符；請重新預覽", true
	default:
		return "", false
	}
}

// historicalOperatorVerifierRejectionDetail says what class of decision was
// committed then. It must not rewrite that history with today's kind list.
func historicalOperatorVerifierRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeVerifierInvalid:
		return "原 request 的 verifier 欄位不符合當時的 verifier policy", true
	case OperatorCodeVerifierDomainInvalid:
		return "原 request 的 failure_domain 不符合當時的 verifier policy", true
	case OperatorCodeVerifierNameTaken:
		return "原 request 的 display_name 當時已被佔用", true
	case OperatorCodePreviewRequired:
		return "原 request 當時缺少必要的 preview_digest", true
	case OperatorCodeVerifierPreviewStale:
		return "原 request 的 preview_digest 不符合當時的 verifier policy", true
	default:
		return "", false
	}
}

func operatorVerifierStoredRejectionDetail(code, originalDetail string) string {
	return truncAudit(operatorVerifierRejectionEvidencePrefix+code+"；"+originalDetail, auditMaxReason)
}

func operatorVerifierStoredRejectionMatchesCode(raw, code string) bool {
	return strings.HasPrefix(raw, operatorVerifierRejectionEvidencePrefix+code+"；")
}

func operatorVerifierSuccessDetail(receipt operatorVerifierReceipt) string {
	return fmt.Sprintf("verifier_id=%s，kind=%s，failure_domain=%s，separation_rule=%s",
		receipt.VerifierID, receipt.Kind, receipt.FailureDomain, OperatorVerifierSeparationRule)
}

// ApplyOperatorVerifier is the single transactional authority for verifier
// registration. The registry row, credential hash, redacted replay receipt and
// audit evidence either commit together or not at all.
func (s *Store) ApplyOperatorVerifier(req OperatorVerifierCreateRequest) (OperatorVerifierCreateResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorVerifierCreateResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorVerifierCreateResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	audit := req.Audit
	audit.Action = AuditVerifierRegister
	audit.Subject = req.DisplayName
	audit.Reason = req.Reason
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: begin operator verifier registration: %w", err)
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
		return s.replayOperatorVerifier(tx, req, audit, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: inspect operator verifier idempotency key: %w", err)
	}

	reject := func(code string) (OperatorVerifierCreateResult, error) {
		detail, ok := canonicalOperatorVerifierRejectionDetail(code)
		if !ok {
			return OperatorVerifierCreateResult{}, errors.New("store: invalid operator verifier rejection code")
		}
		storedDetail := operatorVerifierStoredRejectionDetail(code, detail)
		audit.OK, audit.Detail = false, storedDetail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorVerifierCreateResult{}, fmt.Errorf("store: record operator verifier rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorVerifierOperation,
			req.RequestDigest, code, storedDetail, fmtTime(writerNow)); err != nil {
			return OperatorVerifierCreateResult{}, fmt.Errorf("store: persist operator verifier rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorVerifierCreateResult{}, fmt.Errorf("store: commit operator verifier rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorVerifierCreateResult{}, rejection
	}

	if validationErr := s.validateOperatorVerifierIntent(tx, req.Kind, req.DisplayName,
		req.FailureDomain, req.HubMachineID); validationErr != nil {
		var rejection *OperatorRequestError
		if !errors.As(validationErr, &rejection) {
			return OperatorVerifierCreateResult{}, validationErr
		}
		return reject(rejection.Code)
	}
	if strings.TrimSpace(req.PreviewDigest) == "" {
		return reject(OperatorCodePreviewRequired)
	}
	expected := operatorVerifierPreviewDigest(req.Kind, req.DisplayName, req.FailureDomain,
		currentOperatorVerifierPolicy(req.Kind))
	if req.PreviewDigest != expected {
		return reject(OperatorCodeVerifierPreviewStale)
	}
	if !validJobEvidenceTime(writerNow) {
		return OperatorVerifierCreateResult{}, errors.New("store: verifier registration clock is out of range")
	}

	// Mint only after replay and domain validation, so a replay never creates
	// even a transient second credential.
	secret, err := newToken()
	if err != nil {
		return OperatorVerifierCreateResult{}, err
	}
	receipt := operatorVerifierReceipt{
		VerifierID: newID(), Kind: req.Kind, DisplayName: req.DisplayName,
		FailureDomain: req.FailureDomain, CreatedAt: writerNow, Revision: 1,
		PreviewDigest: req.PreviewDigest,
	}
	if _, err := tx.Exec(`INSERT INTO verifiers
	 (verifier_id,kind,display_name,failure_domain,credential_hash,created_at,revision)
	 VALUES (?,?,?,?,?,?,?)`, receipt.VerifierID, receipt.Kind, receipt.DisplayName,
		receipt.FailureDomain, hashToken(secret), fmtTime(receipt.CreatedAt), receipt.Revision); err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: insert operator verifier: %w", err)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: encode operator verifier receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorVerifierOperation,
		req.RequestDigest, string(raw), fmtTime(writerNow)); err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: persist operator verifier receipt: %w", err)
	}
	audit.OK = true
	audit.Detail = operatorVerifierSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: record operator verifier success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		// Do not return the credential when Commit is ambiguous. A retry can
		// recover the redacted receipt and direct the operator to revoke.
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: commit operator verifier registration: %w", err)
	}
	return freshOperatorVerifierResult(receipt, secret), nil
}

func (s *Store) replayOperatorVerifier(tx *sql.Tx, req OperatorVerifierCreateRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorVerifierCreateResult, error) {
	if cached.Operation != operatorVerifierOperation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK, audit.Detail = false, "idempotency conflict："+detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorVerifierCreateResult{}, fmt.Errorf("store: record operator verifier idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorVerifierCreateResult{}, fmt.Errorf("store: commit operator verifier idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorVerifierCreateResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		historical, allowed := historicalOperatorVerifierRejectionDetail(cached.ErrorCode.String)
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			!allowed || !operatorVerifierStoredRejectionMatchesCode(
			cached.ErrorDetail.String, cached.ErrorCode.String) ||
			!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
			return s.rejectInvalidOperatorVerifierCache(tx, audit)
		}
		valid, err := validateOperatorVerifierRejectionEvidence(tx, cached, req)
		if err != nil {
			return OperatorVerifierCreateResult{}, fmt.Errorf("store: inspect cached operator verifier rejection evidence: %w", err)
		}
		if !valid {
			return s.rejectInvalidOperatorVerifierCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorVerifierCreateResult{}, fmt.Errorf("store: record rejected operator verifier replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorVerifierCreateResult{}, fmt.Errorf("store: commit rejected operator verifier replay audit: %w", err)
		}
		return OperatorVerifierCreateResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid ||
		cached.ErrorDetail.Valid || !canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return s.rejectInvalidOperatorVerifierCache(tx, audit)
	}
	receipt, err := decodeOperatorVerifierReceipt(cached.ResponseJSON.String)
	if err != nil {
		return s.rejectInvalidOperatorVerifierCache(tx, audit)
	}
	if err := validateOperatorVerifierReceipt(receipt, req, cached.CreatedAt); err != nil {
		return s.rejectInvalidOperatorVerifierCache(tx, audit)
	}
	valid, err := validateOperatorVerifierSuccessEvidence(tx, receipt, req)
	if err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: inspect cached operator verifier evidence: %w", err)
	}
	if !valid {
		return s.rejectInvalidOperatorVerifierCache(tx, audit)
	}
	audit.Subject = receipt.DisplayName
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + "原 verifier 已建立；credential 明文不會重顯，必須撤銷原 verifier，再用新 key 註冊新的"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: record successful operator verifier replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: commit successful operator verifier replay audit: %w", err)
	}
	result := redactedOperatorVerifierResult(receipt)
	result.Audited = true
	return result, nil
}

func (s *Store) rejectInvalidOperatorVerifierCache(tx *sql.Tx,
	audit AuditEntry,
) (OperatorVerifierCreateResult, error) {
	// Never copy a corrupt cached value into the returned error or the audit:
	// a forged error_detail or JSON field could itself contain a credential.
	audit.MachineID, audit.Reason = "", ""
	audit.OK, audit.Detail = false, operatorVerifierCacheInvalidDetail
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: record invalid operator verifier cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorVerifierCreateResult{}, fmt.Errorf("store: commit invalid operator verifier cache audit: %w", err)
	}
	return OperatorVerifierCreateResult{Audited: true},
		errors.New("store: operator verifier idempotency cache is invalid")
}

func validateOperatorVerifierRejectionEvidence(tx *sql.Tx, cached operatorCachedRequest,
	req OperatorVerifierCreateRequest,
) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id IS NULL AND subject=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='failed'
	   AND COALESCE(detail,'')=?`,
		string(AuditVerifierRegister), truncAudit(req.DisplayName, auditMaxReason),
		truncAudit(req.Reason, auditMaxReason), req.IdempotencyKey, req.RequestDigest,
		cached.CreatedAt, cached.ErrorDetail.String).Scan(&count)
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

func validateOperatorVerifierSuccessEvidence(tx *sql.Tx, receipt operatorVerifierReceipt,
	req OperatorVerifierCreateRequest,
) (bool, error) {
	var kind, displayName, failureDomain, createdAt, credentialHash string
	err := tx.QueryRow(`SELECT kind,display_name,failure_domain,created_at,credential_hash
	 FROM verifiers WHERE verifier_id=?`, receipt.VerifierID).Scan(
		&kind, &displayName, &failureDomain, &createdAt, &credentialHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if kind != receipt.Kind || displayName != receipt.DisplayName ||
		failureDomain != receipt.FailureDomain || createdAt != fmtTime(receipt.CreatedAt) ||
		!validLowerSHA256(credentialHash) {
		return false, nil
	}
	var count int
	err = tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id IS NULL AND subject=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='ok'
	   AND COALESCE(detail,'')=?`,
		string(AuditVerifierRegister), truncAudit(receipt.DisplayName, auditMaxReason),
		truncAudit(req.Reason, auditMaxReason), req.IdempotencyKey, req.RequestDigest,
		fmtTime(receipt.CreatedAt), operatorVerifierSuccessDetail(receipt)).Scan(&count)
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

func decodeOperatorVerifierReceipt(raw string) (operatorVerifierReceipt, error) {
	var receipt operatorVerifierReceipt
	err := decodeStrictCachedObject(raw, map[string]any{
		"verifier_id":    &receipt.VerifierID,
		"kind":           &receipt.Kind,
		"display_name":   &receipt.DisplayName,
		"failure_domain": &receipt.FailureDomain,
		"created_at":     &receipt.CreatedAt,
		"revision":       &receipt.Revision,
		"preview_digest": &receipt.PreviewDigest,
	})
	if err != nil {
		return operatorVerifierReceipt{}, err
	}
	return receipt, nil
}

func validateOperatorVerifierReceipt(receipt operatorVerifierReceipt,
	req OperatorVerifierCreateRequest, cachedCreatedAt string,
) error {
	if receipt.VerifierID == "" || receipt.Kind != req.Kind ||
		receipt.DisplayName != req.DisplayName || receipt.FailureDomain != req.FailureDomain ||
		receipt.PreviewDigest != req.PreviewDigest || receipt.Revision != 1 ||
		!canonicalOperatorTime(receipt.CreatedAt) ||
		fmtTime(receipt.CreatedAt) != cachedCreatedAt {
		return errors.New("store: cached operator verifier receipt is inconsistent")
	}
	return nil
}

func freshOperatorVerifierResult(receipt operatorVerifierReceipt, secret string) OperatorVerifierCreateResult {
	return OperatorVerifierCreateResult{
		VerifierID: receipt.VerifierID, Kind: receipt.Kind, DisplayName: receipt.DisplayName,
		FailureDomain: receipt.FailureDomain, CreatedAt: receipt.CreatedAt,
		Revision: receipt.Revision, PreviewDigest: receipt.PreviewDigest,
		Credential: secret, SecretAvailable: true, Audited: true,
	}
}

func redactedOperatorVerifierResult(receipt operatorVerifierReceipt) OperatorVerifierCreateResult {
	return OperatorVerifierCreateResult{
		VerifierID: receipt.VerifierID, Kind: receipt.Kind, DisplayName: receipt.DisplayName,
		FailureDomain: receipt.FailureDomain, CreatedAt: receipt.CreatedAt,
		Revision: receipt.Revision, PreviewDigest: receipt.PreviewDigest,
		Replayed: true, RecoveryRequired: true,
		RecoveryAction: OperatorVerifierRecoveryRevokeAndRegister,
	}
}

// OperatorVerifierRecoveryRevokeAndRegister is the only recovery an operator
// who lost the credential has: the plaintext is never reconstructible.
const OperatorVerifierRecoveryRevokeAndRegister = "revoke_and_register"

// decodeStrictCachedObject decodes exactly the named fields of a cached JSON
// object: no unknown field, no duplicate, no null, no missing, no trailing
// bytes. A cached receipt is attacker-shaped input the moment the ledger is.
func decodeStrictCachedObject(raw string, fields map[string]any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	opening, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return errors.New("cached receipt is not a JSON object")
	}
	seen := make(map[string]bool, len(fields))
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("cached receipt field name is not a string")
		}
		if seen[name] {
			return fmt.Errorf("cached receipt has duplicate field %q", name)
		}
		seen[name] = true
		dst, known := fields[name]
		if !known {
			return fmt.Errorf("cached receipt has unknown field %q", name)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return fmt.Errorf("decode cached receipt field %q: %w", name, err)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("cached receipt field %q cannot be null", name)
		}
		if err := json.Unmarshal(value, dst); err != nil {
			return fmt.Errorf("decode cached receipt field %q: %w", name, err)
		}
	}
	closing, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := closing.(json.Delim); !ok || delim != '}' {
		return errors.New("cached receipt object did not close")
	}
	for name := range fields {
		if !seen[name] {
			return fmt.Errorf("cached receipt is missing field %q", name)
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("cached receipt contains trailing JSON")
		}
		return err
	}
	return nil
}
