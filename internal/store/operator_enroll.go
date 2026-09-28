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

// The canonical operator enrollment API deliberately has a narrower TTL
// policy than the legacy Store helper. The wire format is integer seconds so
// every adapter hashes and validates exactly the same value.
const (
	OperatorEnrollTokenMinTTLSeconds int64 = 60
	OperatorEnrollTokenMaxTTLSeconds int64 = 24 * 60 * 60

	OperatorEnrollTokenInitialStateNeverReported = "never_reported"
	OperatorEnrollTokenSecretDeliveryFirstOnly   = "first-response-only"
	OperatorEnrollTokenRecoveryRevokeAndReissue  = "revoke_and_reissue"
)

const (
	OperatorCodeBadDisplayName  = "BAD_DISPLAY_NAME"
	OperatorCodeBadTTL          = "BAD_TTL"
	OperatorCodePreviewRequired = "PREVIEW_REQUIRED"
	OperatorCodePreviewStale    = "PREVIEW_STALE"
)

var (
	ErrBadDisplayName  = errors.New("store: operator enrollment display name is invalid")
	ErrBadEnrollTTL    = errors.New("store: operator enrollment TTL is invalid")
	ErrPreviewRequired = errors.New("store: operator enrollment preview is required")
	ErrPreviewStale    = errors.New("store: operator enrollment preview is stale")
)

const (
	operatorEnrollTokenOperation = "enrollment-token-create:v1"
	operatorEnrollTokenVersion   = "v1"

	operatorEnrollTokenCacheInvalidDetail      = "operator enrollment idempotency cache invalid；未回放結果或 secret"
	operatorEnrollTokenRejectionEvidencePrefix = "operator enrollment rejection code="
)

// operatorEnrollTokenPolicyIdentity is both the rendered impact and part of
// the preview precondition. Keeping each component structured prevents a new
// policy field from changing the human-visible promise without invalidating
// old previews.
type operatorEnrollTokenPolicyIdentity struct {
	MinTTLSeconds              int64  `json:"min_ttl_seconds"`
	MaxTTLSeconds              int64  `json:"max_ttl_seconds"`
	CreatesExpectedMachine     bool   `json:"creates_expected_machine"`
	InitialState               string `json:"initial_state"`
	RevocationKeepsRegistryRow bool   `json:"revocation_keeps_registry_row"`
	SecretDelivery             string `json:"secret_delivery"`
}

func currentOperatorEnrollTokenPolicy() operatorEnrollTokenPolicyIdentity {
	return operatorEnrollTokenPolicyIdentity{
		MinTTLSeconds:              OperatorEnrollTokenMinTTLSeconds,
		MaxTTLSeconds:              OperatorEnrollTokenMaxTTLSeconds,
		CreatesExpectedMachine:     true,
		InitialState:               OperatorEnrollTokenInitialStateNeverReported,
		RevocationKeepsRegistryRow: true,
		SecretDelivery:             OperatorEnrollTokenSecretDeliveryFirstOnly,
	}
}

// OperatorEnrollTokenPreviewResult is safe to render or persist: it contains
// no credential. ExpiresAtIfCreatedNow is explicitly illustrative; the
// authoritative expiry is calculated from the transaction's writer-reserved
// clock when Create commits.
type OperatorEnrollTokenPreviewResult struct {
	DisplayName                string    `json:"display_name"`
	TTLSeconds                 int64     `json:"ttl_seconds"`
	PreviewedAt                time.Time `json:"previewed_at"`
	ExpiresAtIfCreatedNow      time.Time `json:"expires_at_if_created_now"`
	CreatesExpectedMachine     bool      `json:"creates_expected_machine"`
	InitialState               string    `json:"initial_state"`
	RevocationKeepsRegistryRow bool      `json:"revocation_keeps_registry_row"`
	SecretDelivery             string    `json:"secret_delivery"`
	PreviewDigest              string    `json:"preview_digest"`

	// 註冊上限現在的樣子。⚠ 這五個欄位**不**進 preview digest：它們是講給人看的，
	// 真正擋人的那一次數發生在開票的同一筆 writer transaction 裡。
	LimitSet         bool `json:"limit_set"`
	LimitMaxMachines int  `json:"limit_max_machines"`
	InDenominator    int  `json:"in_denominator"`
	Headroom         int  `json:"headroom"`
	AtLimit          bool `json:"at_limit"`
}

type OperatorEnrollTokenCreateRequest struct {
	DisplayName    string
	TTLSeconds     int64
	PreviewDigest  string
	Reason         string
	IdempotencyKey string
	RequestDigest  string
	Audit          AuditEntry
}

// OperatorEnrollTokenCreateResult has two deliberately different success
// representations. A fresh result carries EnrollmentToken exactly once. A
// replay carries only the durable, redacted receipt and tells the caller to
// revoke and reissue; persisting or reconstructing the secret would defeat the
// enrollment-token boundary.
type OperatorEnrollTokenCreateResult struct {
	MachineID     string    `json:"machine_id"`
	DisplayName   string    `json:"display_name"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	TTLSeconds    int64     `json:"ttl_seconds"`
	PreviewDigest string    `json:"preview_digest"`
	// EnrollmentToken is intentionally excluded from generic serialization.
	// Only the HTTP adapter's guarded fresh-response DTO may put it on a wire.
	EnrollmentToken  string `json:"-"`
	SecretAvailable  bool   `json:"secret_available"`
	Replayed         bool   `json:"replayed"`
	RecoveryRequired bool   `json:"recovery_required"`
	RecoveryAction   string `json:"recovery_action,omitempty"`
	Audited          bool   `json:"-"`
}

// operatorEnrollTokenReceipt is the only success representation allowed in
// operator_idempotency.response_json. Keep delivery-state and token fields out
// of this type so a future refactor cannot accidentally cache the first body.
type operatorEnrollTokenReceipt struct {
	MachineID     string    `json:"machine_id"`
	DisplayName   string    `json:"display_name"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	TTLSeconds    int64     `json:"ttl_seconds"`
	PreviewDigest string    `json:"preview_digest"`
}

// PreviewOperatorEnrollToken validates the same canonical intent used by the
// create transaction. It performs no write and does not consume an
// idempotency key.
func (s *Store) PreviewOperatorEnrollToken(displayName string, ttlSeconds int64) (OperatorEnrollTokenPreviewResult, error) {
	if err := validateOperatorEnrollTokenIntent(displayName, ttlSeconds); err != nil {
		return OperatorEnrollTokenPreviewResult{}, err
	}
	now := s.now().UTC()
	preview := operatorEnrollTokenPreview(displayName, ttlSeconds, now)
	// 註冊上限只是講出來給人看，不進 digest。分母每分鐘都在變，把它釘進預覽等於
	// 每一份預覽在按下送出之前就過期了——而把上限「調高」也會作廢一份本來成立的
	// 預覽，那是完全相反的方向。真正擋人的那一次數，發生在開票的同一筆交易裡。
	limit, err := enrollmentLimitState(s.db)
	if err != nil {
		return OperatorEnrollTokenPreviewResult{}, err
	}
	preview.LimitSet, preview.LimitMaxMachines = limit.Set, limit.MaxMachines
	preview.InDenominator, preview.Headroom, preview.AtLimit =
		limit.InDenominator, limit.Headroom, limit.Set && limit.AtLimit
	return preview, nil
}

func operatorEnrollTokenPreview(displayName string, ttlSeconds int64, now time.Time) OperatorEnrollTokenPreviewResult {
	policy := currentOperatorEnrollTokenPolicy()
	return OperatorEnrollTokenPreviewResult{
		DisplayName:                displayName,
		TTLSeconds:                 ttlSeconds,
		PreviewedAt:                now,
		ExpiresAtIfCreatedNow:      now.Add(time.Duration(ttlSeconds) * time.Second),
		CreatesExpectedMachine:     policy.CreatesExpectedMachine,
		InitialState:               policy.InitialState,
		RevocationKeepsRegistryRow: policy.RevocationKeepsRegistryRow,
		SecretDelivery:             policy.SecretDelivery,
		PreviewDigest:              operatorEnrollTokenPreviewDigestForPolicy(displayName, ttlSeconds, policy),
	}
}

func operatorEnrollTokenPreviewDigest(displayName string, ttlSeconds int64) string {
	return operatorEnrollTokenPreviewDigestForPolicy(displayName, ttlSeconds, currentOperatorEnrollTokenPolicy())
}

func operatorEnrollTokenPreviewDigestForPolicy(displayName string, ttlSeconds int64,
	policy operatorEnrollTokenPolicyIdentity,
) string {
	return operatorEnrollTokenPreviewDigestForIdentity(
		operatorEnrollTokenVersion, displayName, ttlSeconds, policy)
}

func operatorEnrollTokenPreviewDigestForIdentity(version, displayName string, ttlSeconds int64,
	policy operatorEnrollTokenPolicyIdentity,
) string {
	body := struct {
		Version     string                            `json:"version"`
		DisplayName string                            `json:"display_name"`
		TTLSeconds  int64                             `json:"ttl_seconds"`
		Policy      operatorEnrollTokenPolicyIdentity `json:"policy"`
	}{
		Version: version, DisplayName: displayName, TTLSeconds: ttlSeconds,
		Policy: policy,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateOperatorEnrollTokenIntent(displayName string, ttlSeconds int64) error {
	if strings.TrimSpace(displayName) == "" || displayName != strings.TrimSpace(displayName) {
		return operatorEnrollTokenRejection(OperatorCodeBadDisplayName)
	}
	if ttlSeconds < OperatorEnrollTokenMinTTLSeconds || ttlSeconds > OperatorEnrollTokenMaxTTLSeconds {
		return operatorEnrollTokenRejection(OperatorCodeBadTTL)
	}
	return nil
}

func operatorEnrollTokenRejection(code string) *OperatorRequestError {
	detail, ok := canonicalOperatorEnrollTokenRejectionDetail(code)
	if !ok {
		return operatorError(code, "控制面 enrollment request 被拒絕")
	}
	return operatorError(code, detail)
}

func canonicalOperatorEnrollTokenRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeBadDisplayName:
		return "display_name 不可為空，且開頭或結尾不可有空白", true
	case OperatorCodeBadTTL:
		return fmt.Sprintf("ttl_seconds 必須介於 %d 與 %d",
			OperatorEnrollTokenMinTTLSeconds, OperatorEnrollTokenMaxTTLSeconds), true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodePreviewStale:
		return "preview_digest 與目前的 enrollment policy 不符；請重新預覽", true
	case OperatorCodeEnrollmentLimitReached:
		return "名冊已達註冊上限；請先退役不用的機器，或把上限調高", true
	default:
		return "", false
	}
}

// historicalOperatorEnrollTokenRejectionDetail is deliberately independent
// of today's policy values. A replay says what class of decision was committed
// then; it must not rewrite that history using today's TTL range or preview
// policy, and it must never copy cached free text to the caller.
func historicalOperatorEnrollTokenRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeBadDisplayName:
		return "原 request 的 display_name 不符合當時 enrollment policy", true
	case OperatorCodeBadTTL:
		return "原 request 的 ttl_seconds 不符合當時 enrollment policy", true
	case OperatorCodePreviewRequired:
		return "原 request 當時缺少必要的 preview_digest", true
	case OperatorCodePreviewStale:
		return "原 request 的 preview_digest 不符合當時 enrollment policy", true
	case OperatorCodeEnrollmentLimitReached:
		return "原 request 當時已達當時的註冊上限", true
	default:
		return "", false
	}
}

func operatorEnrollTokenStoredRejectionDetail(code, originalDetail string) string {
	return truncAudit(operatorEnrollTokenRejectionEvidencePrefix+code+"；"+originalDetail, auditMaxReason)
}

func operatorEnrollTokenStoredRejectionMatchesCode(raw, code string) bool {
	return strings.HasPrefix(raw, operatorEnrollTokenRejectionEvidencePrefix+code+"；")
}

// ApplyOperatorEnrollToken is the single transactional authority for
// canonical operator token issuance. The registry row, token hash, redacted
// replay receipt, and audit evidence either commit together or not at all.
func (s *Store) ApplyOperatorEnrollToken(req OperatorEnrollTokenCreateRequest) (OperatorEnrollTokenCreateResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return OperatorEnrollTokenCreateResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略")
	}
	if len(req.IdempotencyKey) > 200 {
		return OperatorEnrollTokenCreateResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorEnrollTokenCreateResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	audit := req.Audit
	audit.Action = AuditEnrollToken
	audit.Subject = req.DisplayName
	audit.Reason = req.Reason
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: begin operator enroll token: %w", err)
	}
	defer tx.Rollback()
	// Store timestamps are second-precision RFC3339. Take the clock only after
	// BEGIN IMMEDIATE has reserved the writer, then use this exact instant for
	// the receipt, every authoritative row, and the original success audit.
	writerNow := s.now().UTC().Truncate(time.Second)
	audit.At = writerNow

	var cached operatorCachedRequest
	err = tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
	 FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	switch {
	case err == nil:
		return s.replayOperatorEnrollToken(tx, req, audit, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: inspect operator enrollment idempotency key: %w", err)
	}

	// detail 為空時用這個 code 的固定說法。註冊上限那一種要帶當下的台數，因為
	// 「已納管 5 台、上限 5 台」本身就是那個判決的證據；回放走的是不帶數字的歷史說法，
	// 所以舊的數字永遠不會被當成現在的。
	reject := func(code, detail string) (OperatorEnrollTokenCreateResult, error) {
		canonical, ok := canonicalOperatorEnrollTokenRejectionDetail(code)
		if !ok {
			return OperatorEnrollTokenCreateResult{}, errors.New("store: invalid operator enrollment rejection code")
		}
		if detail == "" {
			detail = canonical
		}
		storedDetail := operatorEnrollTokenStoredRejectionDetail(code, detail)
		audit.OK, audit.Detail = false, storedDetail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: record operator enrollment rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorEnrollTokenOperation,
			req.RequestDigest, code, storedDetail, fmtTime(writerNow)); err != nil {
			return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: persist operator enrollment rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: commit operator enrollment rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorEnrollTokenCreateResult{}, rejection
	}

	if validationErr := validateOperatorEnrollTokenIntent(req.DisplayName, req.TTLSeconds); validationErr != nil {
		var rejection *OperatorRequestError
		if !errors.As(validationErr, &rejection) {
			return OperatorEnrollTokenCreateResult{}, validationErr
		}
		return reject(rejection.Code, "")
	}
	if strings.TrimSpace(req.PreviewDigest) == "" {
		return reject(OperatorCodePreviewRequired, "")
	}
	expectedPreviewDigest := operatorEnrollTokenPreviewDigest(req.DisplayName, req.TTLSeconds)
	if req.PreviewDigest != expectedPreviewDigest {
		return reject(OperatorCodePreviewStale, "")
	}

	// 註冊上限在同一個 writer transaction 裡數，因為開票會加一列名冊。分兩次讀寫的話，
	// 兩張同時進來的票會各自看到「還有一個位子」，然後一起用掉它。
	//
	// ⚠ 這一句今天就算改成在交易外面數，測試也抓不到：_txlock=immediate 讓兩個寫入
	// 者互相排隊，排在後面的那一個在另一條連線上讀，看到的仍然是已經 commit 的名冊。
	// 擋住那個 race 的是那把寫鎖，不是這一行——但這一行要留著，因為它是唯一一個不必
	// 先知道 SQLite 鎖語意就看得懂「為什麼這樣是對的」的寫法。
	limit, err := enrollmentLimitState(tx)
	if err != nil {
		return OperatorEnrollTokenCreateResult{}, err
	}
	if limit.Set && limit.AtLimit {
		return reject(OperatorCodeEnrollmentLimitReached,
			enrollmentLimitReachedDetail(limit))
	}

	// Generate only after replay and domain validation. A replay must never
	// create even a transient second credential, and a random-source failure
	// must not hide an already committed receipt.
	token, err := newToken()
	if err != nil {
		return OperatorEnrollTokenCreateResult{}, err
	}
	now := writerNow
	machineID := newID()
	expiresAt := now.Add(time.Duration(req.TTLSeconds) * time.Second)
	if _, err := tx.Exec(`INSERT INTO machine_registry
	 (machine_id,display_name,expected,created_at) VALUES (?,?,1,?)`,
		machineID, req.DisplayName, fmtTime(now)); err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: pre-register operator enrollment machine: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO enrollment_tokens
	 (token_hash,display_name,created_at,expires_at,used_by) VALUES (?,?,?,?,?)`,
		hashToken(token), req.DisplayName, fmtTime(now), fmtTime(expiresAt), machineID); err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: create operator enroll token: %w", err)
	}
	receipt := operatorEnrollTokenReceipt{
		MachineID: machineID, DisplayName: req.DisplayName, CreatedAt: now,
		ExpiresAt: expiresAt, TTLSeconds: req.TTLSeconds, PreviewDigest: req.PreviewDigest,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: encode operator enrollment receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorEnrollTokenOperation,
		req.RequestDigest, string(raw), fmtTime(now)); err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: persist operator enrollment receipt: %w", err)
	}
	audit.MachineID = machineID
	audit.OK = true
	audit.Detail = operatorEnrollTokenSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: record operator enrollment success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		// In particular, do not return token when Commit is ambiguous. A retry
		// can recover the redacted receipt and direct the operator to revoke.
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: commit operator enroll token: %w", err)
	}
	return freshOperatorEnrollTokenResult(receipt, token), nil
}

func (s *Store) replayOperatorEnrollToken(tx *sql.Tx, req OperatorEnrollTokenCreateRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorEnrollTokenCreateResult, error) {
	if cached.Operation != operatorEnrollTokenOperation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK = false
		audit.Detail = "idempotency conflict：" + detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: record operator enrollment idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: commit operator enrollment idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorEnrollTokenCreateResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		historicalDetail, allowed := historicalOperatorEnrollTokenRejectionDetail(cached.ErrorCode.String)
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			!allowed || !operatorEnrollTokenStoredRejectionMatchesCode(
			cached.ErrorDetail.String, cached.ErrorCode.String) ||
			!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
			return s.rejectInvalidOperatorEnrollTokenCache(tx, audit)
		}
		valid, err := validateOperatorEnrollTokenRejectionEvidence(tx, cached, req)
		if err != nil {
			return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: inspect cached operator enrollment rejection evidence: %w", err)
		}
		if !valid {
			return s.rejectInvalidOperatorEnrollTokenCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + historicalDetail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: record rejected operator enrollment replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: commit rejected operator enrollment replay audit: %w", err)
		}
		return OperatorEnrollTokenCreateResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historicalDetail,
			Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid || cached.ErrorDetail.Valid ||
		!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return s.rejectInvalidOperatorEnrollTokenCache(tx, audit)
	}
	receipt, err := decodeOperatorEnrollTokenReceipt(cached.ResponseJSON.String)
	if err != nil {
		return s.rejectInvalidOperatorEnrollTokenCache(tx, audit)
	}
	if err := validateOperatorEnrollTokenReceipt(receipt, req, cached.CreatedAt); err != nil {
		return s.rejectInvalidOperatorEnrollTokenCache(tx, audit)
	}
	valid, err := validateOperatorEnrollTokenSuccessEvidence(tx, receipt, req)
	if err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: inspect cached operator enrollment evidence: %w", err)
	}
	if !valid {
		return s.rejectInvalidOperatorEnrollTokenCache(tx, audit)
	}
	audit.MachineID = receipt.MachineID
	audit.Subject = receipt.DisplayName
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + "原票已建立；明文不會重顯，必須撤銷原票、退役原本未報到的名冊列，再用新 key 建立新的名冊列與票"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: record successful operator enrollment replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: commit successful operator enrollment replay audit: %w", err)
	}
	result := redactedOperatorEnrollTokenResult(receipt)
	result.Audited = true
	return result, nil
}

func (s *Store) rejectInvalidOperatorEnrollTokenCache(tx *sql.Tx,
	audit AuditEntry,
) (OperatorEnrollTokenCreateResult, error) {
	// Never copy a corrupt cached value into either the returned error or audit.
	// In particular, a forged error_detail or JSON field could itself contain a
	// credential. This row records only the fixed fact that no replay occurred.
	audit.MachineID = ""
	audit.Reason = ""
	audit.OK = false
	audit.Detail = operatorEnrollTokenCacheInvalidDetail
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: record invalid operator enrollment cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorEnrollTokenCreateResult{}, fmt.Errorf("store: commit invalid operator enrollment cache audit: %w", err)
	}
	return OperatorEnrollTokenCreateResult{Audited: true},
		errors.New("store: operator enrollment idempotency cache is invalid")
}

func canonicalOperatorEnrollCacheTime(raw string) bool {
	parsed := parseTime(raw)
	return !parsed.IsZero() && fmtTime(parsed) == raw && parsed.Equal(parsed.UTC().Truncate(time.Second))
}

func validateOperatorEnrollTokenRejectionEvidence(tx *sql.Tx, cached operatorCachedRequest,
	req OperatorEnrollTokenCreateRequest,
) (bool, error) {
	var originalRejectionAudits int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id IS NULL AND subject=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='failed'
	   AND COALESCE(detail,'')=?`,
		string(AuditEnrollToken), truncAudit(req.DisplayName, auditMaxReason),
		truncAudit(req.Reason, auditMaxReason), req.IdempotencyKey, req.RequestDigest,
		cached.CreatedAt, cached.ErrorDetail.String).Scan(&originalRejectionAudits)
	if err != nil {
		return false, err
	}
	return originalRejectionAudits == 1, nil
}

func validateOperatorEnrollTokenSuccessEvidence(tx *sql.Tx, receipt operatorEnrollTokenReceipt,
	req OperatorEnrollTokenCreateRequest,
) (bool, error) {
	var registryCreatedAt string
	err := tx.QueryRow(`SELECT created_at FROM machine_registry WHERE machine_id=?`,
		receipt.MachineID).Scan(&registryCreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if registryCreatedAt != fmtTime(receipt.CreatedAt) {
		return false, nil
	}

	var originalSuccessAudits int
	err = tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id=? AND subject=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome='ok'
	   AND COALESCE(detail,'')=?`,
		string(AuditEnrollToken), receipt.MachineID, truncAudit(receipt.DisplayName, auditMaxReason),
		truncAudit(req.Reason, auditMaxReason), req.IdempotencyKey, req.RequestDigest,
		fmtTime(receipt.CreatedAt), operatorEnrollTokenSuccessDetail(receipt)).Scan(&originalSuccessAudits)
	if err != nil {
		return false, err
	}
	return originalSuccessAudits == 1, nil
}

func decodeOperatorEnrollTokenReceipt(raw string) (operatorEnrollTokenReceipt, error) {
	var receipt operatorEnrollTokenReceipt
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	opening, err := dec.Token()
	if err != nil {
		return operatorEnrollTokenReceipt{}, err
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return operatorEnrollTokenReceipt{}, errors.New("cached receipt is not a JSON object")
	}
	seen := make(map[string]bool, 6)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return operatorEnrollTokenReceipt{}, err
		}
		name, ok := token.(string)
		if !ok {
			return operatorEnrollTokenReceipt{}, errors.New("cached receipt field name is not a string")
		}
		if seen[name] {
			return operatorEnrollTokenReceipt{}, fmt.Errorf("cached receipt has duplicate field %q", name)
		}
		seen[name] = true
		var dst any
		switch name {
		case "machine_id":
			dst = &receipt.MachineID
		case "display_name":
			dst = &receipt.DisplayName
		case "created_at":
			dst = &receipt.CreatedAt
		case "expires_at":
			dst = &receipt.ExpiresAt
		case "ttl_seconds":
			dst = &receipt.TTLSeconds
		case "preview_digest":
			dst = &receipt.PreviewDigest
		default:
			return operatorEnrollTokenReceipt{}, fmt.Errorf("cached receipt has unknown field %q", name)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return operatorEnrollTokenReceipt{}, fmt.Errorf("decode cached receipt field %q: %w", name, err)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return operatorEnrollTokenReceipt{}, fmt.Errorf("cached receipt field %q cannot be null", name)
		}
		if err := json.Unmarshal(value, dst); err != nil {
			return operatorEnrollTokenReceipt{}, fmt.Errorf("decode cached receipt field %q: %w", name, err)
		}
	}
	closing, err := dec.Token()
	if err != nil {
		return operatorEnrollTokenReceipt{}, err
	}
	if delim, ok := closing.(json.Delim); !ok || delim != '}' {
		return operatorEnrollTokenReceipt{}, errors.New("cached receipt object did not close")
	}
	for _, required := range []string{
		"machine_id", "display_name", "created_at", "expires_at", "ttl_seconds", "preview_digest",
	} {
		if !seen[required] {
			return operatorEnrollTokenReceipt{}, fmt.Errorf("cached receipt is missing field %q", required)
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return operatorEnrollTokenReceipt{}, errors.New("cached receipt contains trailing JSON")
		}
		return operatorEnrollTokenReceipt{}, err
	}
	return receipt, nil
}

func validateOperatorEnrollTokenReceipt(receipt operatorEnrollTokenReceipt, req OperatorEnrollTokenCreateRequest,
	cachedCreatedAt string,
) error {
	if receipt.MachineID == "" || receipt.DisplayName != req.DisplayName ||
		receipt.TTLSeconds != req.TTLSeconds || receipt.PreviewDigest != req.PreviewDigest ||
		!canonicalOperatorTime(receipt.CreatedAt) ||
		!canonicalOperatorTime(receipt.ExpiresAt) ||
		fmtTime(receipt.CreatedAt) != cachedCreatedAt ||
		!receipt.ExpiresAt.Equal(receipt.CreatedAt.Add(time.Duration(receipt.TTLSeconds)*time.Second)) {
		return errors.New("store: cached operator enrollment receipt is inconsistent")
	}
	return nil
}

func operatorEnrollTokenSuccessDetail(receipt operatorEnrollTokenReceipt) string {
	return fmt.Sprintf("machine_id=%s，ttl=%ds，expires_at=%s",
		receipt.MachineID, receipt.TTLSeconds, fmtTime(receipt.ExpiresAt))
}

func freshOperatorEnrollTokenResult(receipt operatorEnrollTokenReceipt, token string) OperatorEnrollTokenCreateResult {
	return OperatorEnrollTokenCreateResult{
		MachineID: receipt.MachineID, DisplayName: receipt.DisplayName,
		CreatedAt: receipt.CreatedAt, ExpiresAt: receipt.ExpiresAt, TTLSeconds: receipt.TTLSeconds,
		PreviewDigest:   receipt.PreviewDigest,
		EnrollmentToken: token, SecretAvailable: true, Audited: true,
	}
}

func redactedOperatorEnrollTokenResult(receipt operatorEnrollTokenReceipt) OperatorEnrollTokenCreateResult {
	return OperatorEnrollTokenCreateResult{
		MachineID: receipt.MachineID, DisplayName: receipt.DisplayName,
		CreatedAt: receipt.CreatedAt, ExpiresAt: receipt.ExpiresAt, TTLSeconds: receipt.TTLSeconds,
		PreviewDigest: receipt.PreviewDigest,
		Replayed:      true, RecoveryRequired: true,
		RecoveryAction: OperatorEnrollTokenRecoveryRevokeAndReissue,
	}
}
