package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/artifact"
)

// Artifact fetch is an asynchronous operator operation.  The Store owns only
// durable intent, progress, replay, and audit evidence; registry and filesystem
// I/O belongs to the worker supplied by higher layers.
const (
	ArtifactFetchReadSchemaVersion       = 2
	ArtifactFetchReadConsistency         = "live"
	DefaultArtifactFetchReadLimit        = 50
	MaxArtifactFetchReadLimit            = 100
	MaxArtifactFetchBytes          int64 = 3 << 30
	ArtifactFetchMaxErrorBytes           = 1000

	operatorArtifactFetchOperation      = "artifact-fetch-enqueue:v1"
	operatorArtifactFetchReceiptVersion = "v1"
	operatorArtifactFetchRejectPrefix   = "operator artifact fetch rejection code="
	operatorArtifactFetchCacheInvalid   = "operator artifact fetch idempotency cache invalid；未回放 operation"
)

const (
	OperatorCodeArtifactFetchInvalid       = "ARTIFACT_FETCH_INVALID"
	OperatorCodeArtifactFetchPrepareFailed = "ARTIFACT_FETCH_PREPARE_FAILED"
	OperatorCodeArtifactFetchPreviewStale  = "ARTIFACT_FETCH_PREVIEW_STALE"
	OperatorCodeArtifactFetchActive        = "ARTIFACT_FETCH_ACTIVE"
)

var (
	ErrArtifactFetchInvalid         = errors.New("store: artifact fetch request is invalid")
	ErrArtifactFetchPrepareFailed   = errors.New("store: artifact fetch metadata preparation failed")
	ErrArtifactFetchPreviewStale    = errors.New("store: artifact fetch preview is stale")
	ErrArtifactFetchActive          = errors.New("store: artifact fetch already active")
	ErrArtifactFetchNotFound        = errors.New("store: artifact fetch operation not found")
	ErrArtifactFetchInvalidState    = errors.New("store: artifact fetch operation state transition is invalid")
	ErrArtifactFetchClaimLost       = errors.New("store: artifact fetch worker claim is no longer current")
	ErrArtifactFetchProgress        = errors.New("store: artifact fetch progress is invalid")
	ErrArtifactFetchCorrupt         = errors.New("store: artifact fetch operation row is corrupt")
	ErrArtifactFetchCacheInvalid    = errors.New("store: artifact fetch idempotency cache is invalid")
	ErrArtifactFetchClockRegression = errors.New("store: artifact fetch clock moved backwards")
)

type ArtifactFetchState string

const (
	ArtifactFetchQueued    ArtifactFetchState = "queued"
	ArtifactFetchRunning   ArtifactFetchState = "running"
	ArtifactFetchSucceeded ArtifactFetchState = "succeeded"
	ArtifactFetchFailed    ArtifactFetchState = "failed"
)

type ArtifactFetchPhase string

const (
	ArtifactFetchPhaseQueued      ArtifactFetchPhase = "queued"
	ArtifactFetchPhaseDownloading ArtifactFetchPhase = "downloading"
	ArtifactFetchPhaseVerifying   ArtifactFetchPhase = "verifying"
	ArtifactFetchPhasePublishing  ArtifactFetchPhase = "publishing"
	ArtifactFetchPhaseComplete    ArtifactFetchPhase = "complete"
)

var artifactFetchPhaseRanks = map[ArtifactFetchPhase]int{
	ArtifactFetchPhaseQueued:      0,
	ArtifactFetchPhaseDownloading: 1,
	ArtifactFetchPhaseVerifying:   2,
	ArtifactFetchPhasePublishing:  3,
	ArtifactFetchPhaseComplete:    4,
}

// ArtifactFetchOperation is the safe read projection.  It deliberately has no
// tarball URL, worker token, idempotency key, request digest, or caller-supplied
// provenance.  Those values are either secret-bearing worker material or audit
// correlation, not list/detail presentation data.
type ArtifactFetchOperation struct {
	OperationID     string             `json:"operation_id"`
	Name            string             `json:"name"`
	Version         string             `json:"version"`
	SourceKind      string             `json:"source_kind"`
	RegistryOrigin  string             `json:"registry_origin"`
	SHA512Integrity string             `json:"sha512_integrity"`
	EnginesNode     string             `json:"engines_node"`
	IdentityDigest  string             `json:"identity_digest"`
	PreviewDigest   string             `json:"preview_digest"`
	MaxBytes        int64              `json:"max_bytes"`
	State           ArtifactFetchState `json:"state"`
	Phase           ArtifactFetchPhase `json:"phase"`
	ProgressBytes   int64              `json:"progress_bytes"`
	Attempt         int64              `json:"attempt"`
	ResultSHA256    *string            `json:"result_sha256"`
	ResultSizeBytes *int64             `json:"result_size_bytes"`
	ErrorCode       *string            `json:"error_code"`
	ErrorDetail     *string            `json:"error_detail"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
	StartedAt       *time.Time         `json:"started_at"`
	FinishedAt      *time.Time         `json:"finished_at"`
}

type ArtifactFetchListRequest struct {
	State   ArtifactFetchState
	Name    string
	Version string
	Limit   int
}

type ArtifactFetchListResult struct {
	SchemaVersion int                      `json:"schema_version"`
	Consistency   string                   `json:"consistency"`
	EvaluatedAt   time.Time                `json:"evaluated_at"`
	Total         int                      `json:"total"`
	Items         []ArtifactFetchOperation `json:"items"`
}

type OperatorArtifactFetchRequest struct {
	Name           string
	Version        string
	PreviewDigest  string
	Reason         string
	IdempotencyKey string
	RequestDigest  string
	Audit          AuditEntry
}

// ArtifactFetchPrepared is the exact metadata identity returned by the
// external prepare callback.  TarballURL is persisted for a restartable worker
// but never appears in ArtifactFetchOperation.
type ArtifactFetchPrepared struct {
	Name                 string
	Version              string
	SourceKind           string
	SourcePlan           string
	RegistryOrigin       string
	TarballURL           string
	SHA512Integrity      string
	EnginesNode          string
	MaxBytes             int64
	CurrentPreviewDigest string
}

type OperatorArtifactFetchPrepare func() (ArtifactFetchPrepared, error)

type OperatorArtifactFetchResult struct {
	Operation ArtifactFetchOperation `json:"operation"`
	Replayed  bool                   `json:"replayed"`
	Audited   bool                   `json:"-"`
}

// ArtifactFetchClaim is worker-only material.  JSON tags make accidental
// serialization omit both the capability token and the potentially
// credential-bearing upstream URL.
type ArtifactFetchClaim struct {
	Operation  ArtifactFetchOperation `json:"operation"`
	TarballURL string                 `json:"-"`
	SourcePlan string                 `json:"-"`
	RunToken   string                 `json:"-"`
}

type operatorArtifactFetchReceipt struct {
	ReceiptVersion string    `json:"receipt_version"`
	OperationID    string    `json:"operation_id"`
	Name           string    `json:"name"`
	Version        string    `json:"version"`
	RegistryOrigin string    `json:"registry_origin"`
	IdentityDigest string    `json:"identity_digest"`
	PreviewDigest  string    `json:"preview_digest"`
	MaxBytes       int64     `json:"max_bytes"`
	CreatedAt      time.Time `json:"created_at"`
}

type artifactFetchRecord struct {
	Operation      ArtifactFetchOperation
	TarballURL     string
	SourcePlan     string
	RunToken       string
	IdempotencyKey string
	RequestDigest  string
}

const artifactFetchColumns = `operation_id,idempotency_key,request_digest,name,version,source_kind,source_plan,
 registry_origin,tarball_url,sha512_integrity,engines_node,identity_digest,preview_digest,max_bytes,
 state,phase,phase_rank,progress_bytes,attempt,run_token,result_sha256,result_size_bytes,
 error_code,error_detail,created_at,updated_at,started_at,finished_at`

// ApplyOperatorArtifactFetch performs a two-stage idempotency check.  On a
// miss it closes the lookup transaction before Prepare, so slow registry I/O
// cannot hold SQLite's BEGIN IMMEDIATE writer reservation.  The enqueue phase
// then reserves the writer, rechecks the global key, and commits operation,
// immutable receipt, and original audit together.
func (s *Store) ApplyOperatorArtifactFetch(req OperatorArtifactFetchRequest,
	prepare OperatorArtifactFetchPrepare,
) (OperatorArtifactFetchResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorArtifactFetchResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if !validArtifactFetchDigest(req.RequestDigest) {
		return OperatorArtifactFetchResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 必須是 canonical sha256")
	}
	audit := operatorArtifactFetchAudit(req)

	lookupTx, err := s.beginWrite(context.Background(), "apply_operator_artifact_fetch")
	if err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: begin artifact fetch idempotency lookup: %w", err)
	}
	cached, found, err := loadOperatorArtifactFetchCached(lookupTx, req.IdempotencyKey)
	if err != nil {
		_ = lookupTx.Rollback()
		return OperatorArtifactFetchResult{}, err
	}
	if found {
		defer lookupTx.Rollback()
		return s.replayOperatorArtifactFetch(lookupTx, req, audit, cached)
	}
	if err := lookupTx.Rollback(); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: finish artifact fetch idempotency lookup: %w", err)
	}

	// Request-shape rejections do not call external prepare, but still enter the
	// same second writer phase and become durable idempotent decisions.
	rejectCode := ""
	if !validArtifactFetchIdentifier(req.Name, 128) || !validArtifactFetchIdentifier(req.Version, 128) ||
		!validArtifactFetchText(req.Reason, auditMaxReason, false) {
		rejectCode = OperatorCodeArtifactFetchInvalid
	} else if strings.TrimSpace(req.PreviewDigest) == "" {
		rejectCode = OperatorCodePreviewRequired
	} else if !validArtifactFetchDigest(req.PreviewDigest) {
		rejectCode = OperatorCodeArtifactFetchInvalid
	}

	var prepared ArtifactFetchPrepared
	if rejectCode == "" {
		if prepare == nil {
			return OperatorArtifactFetchResult{}, errors.New("store: artifact fetch prepare callback is nil")
		}
		prepared, err = prepare()
		if err != nil {
			var rejection *OperatorRequestError
			if !errors.As(err, &rejection) || !isCanonicalOperatorArtifactFetchRejection(rejection.Code) {
				// Transient registry/network/storage failures do not consume the
				// idempotency key. The adapter may best-effort audit the unavailable
				// dependency, and an identical retry is allowed to prepare again.
				return OperatorArtifactFetchResult{}, ErrArtifactFetchPrepareFailed
			}
			rejectCode = rejection.Code
		} else if prepared.CurrentPreviewDigest != req.PreviewDigest {
			rejectCode = OperatorCodeArtifactFetchPreviewStale
		} else if err := validateArtifactFetchPrepared(req, prepared); err != nil {
			rejectCode = OperatorCodeArtifactFetchInvalid
		}
	}

	tx, err := s.beginWrite(context.Background(), "apply_operator_artifact_fetch_2")
	if err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: begin artifact fetch enqueue: %w", err)
	}
	defer tx.Rollback()
	writerNow, err := canonicalArtifactFetchNow(s.now())
	if err != nil {
		return OperatorArtifactFetchResult{}, err
	}
	audit.At = writerNow
	cached, found, err = loadOperatorArtifactFetchCached(tx, req.IdempotencyKey)
	if err != nil {
		return OperatorArtifactFetchResult{}, err
	}
	if found {
		return s.replayOperatorArtifactFetch(tx, req, audit, cached)
	}
	if rejectCode != "" {
		return s.rejectOperatorArtifactFetchTx(tx, req, audit, rejectCode, writerNow)
	}

	identityDigest := artifactFetchIdentityDigest(prepared)
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM artifact_fetch_operations
 WHERE state IN ('queued','running') AND ((name=? AND version=?) OR identity_digest=?)`,
		prepared.Name, prepared.Version, identityDigest).Scan(&active); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: inspect active artifact fetch: %w", err)
	}
	if active != 0 {
		return s.rejectOperatorArtifactFetchTx(tx, req, audit,
			OperatorCodeArtifactFetchActive, writerNow)
	}

	operationID := newID()
	receipt := operatorArtifactFetchReceipt{
		ReceiptVersion: operatorArtifactFetchReceiptVersion,
		OperationID:    operationID, Name: prepared.Name, Version: prepared.Version,
		RegistryOrigin: prepared.RegistryOrigin, IdentityDigest: identityDigest,
		PreviewDigest: req.PreviewDigest, MaxBytes: prepared.MaxBytes, CreatedAt: writerNow,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: encode artifact fetch receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorArtifactFetchOperation,
		req.RequestDigest, string(raw), fmtTime(writerNow)); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: persist artifact fetch receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO artifact_fetch_operations
 (operation_id,idempotency_key,request_digest,name,version,source_kind,source_plan,registry_origin,tarball_url,
  sha512_integrity,engines_node,identity_digest,preview_digest,max_bytes,state,phase,
  phase_rank,progress_bytes,attempt,created_at,updated_at)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,'queued','queued',0,0,0,?,?)`,
		operationID, req.IdempotencyKey, req.RequestDigest, prepared.Name, prepared.Version,
		prepared.SourceKind, prepared.SourcePlan,
		prepared.RegistryOrigin, prepared.TarballURL, prepared.SHA512Integrity, prepared.EnginesNode,
		identityDigest, req.PreviewDigest, prepared.MaxBytes, fmtTime(writerNow), fmtTime(writerNow)); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: enqueue artifact fetch: %w", err)
	}
	audit.Subject = artifactFetchSubject(prepared.Name, prepared.Version)
	audit.OK = true
	audit.Detail = operatorArtifactFetchSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: record artifact fetch enqueue audit: %w", err)
	}
	// Capture the response while this transaction still owns the newly inserted
	// row. Once commit makes it visible, a worker may immediately claim it; the
	// fresh enqueue response must nevertheless describe the queued decision that
	// this transaction durably created.
	record, err := artifactFetchRecordByID(tx, operationID)
	if err != nil {
		return OperatorArtifactFetchResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: commit artifact fetch enqueue: %w", err)
	}
	if s.afterArtifactFetchEnqueueCommit != nil {
		s.afterArtifactFetchEnqueueCommit(operationID)
	}
	return OperatorArtifactFetchResult{Operation: record.Operation, Audited: true}, nil
}

func operatorArtifactFetchAudit(req OperatorArtifactFetchRequest) AuditEntry {
	audit := req.Audit
	audit.Action = AuditArtifactFetch
	audit.Subject = artifactFetchSubject(req.Name, req.Version)
	// A malformed reason is still bound by RequestDigest, but must not be copied
	// into presentation/audit text. Canonical reasons are required for every
	// accepted fetch and preserved verbatim.
	if validArtifactFetchText(req.Reason, auditMaxReason, false) {
		audit.Reason = req.Reason
	} else {
		audit.Reason = ""
	}
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	return audit
}

func artifactFetchSubject(name, version string) string {
	if !validArtifactFetchIdentifier(name, 128) || !validArtifactFetchIdentifier(version, 128) {
		return "invalid artifact fetch request"
	}
	return name + "@" + version
}

func loadOperatorArtifactFetchCached(q operatorRowQuerier, key string) (operatorCachedRequest, bool, error) {
	var cached operatorCachedRequest
	err := q.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
 FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operatorCachedRequest{}, false, nil
	}
	if err != nil {
		return operatorCachedRequest{}, false, fmt.Errorf("store: inspect artifact fetch idempotency key: %w", err)
	}
	return cached, true, nil
}

func (s *Store) rejectOperatorArtifactFetchTx(tx dbTx, req OperatorArtifactFetchRequest,
	audit AuditEntry, code string, now time.Time,
) (OperatorArtifactFetchResult, error) {
	detail, ok := canonicalOperatorArtifactFetchRejectionDetail(code)
	if !ok {
		return OperatorArtifactFetchResult{}, errors.New("store: invalid artifact fetch rejection code")
	}
	stored := operatorArtifactFetchRejectPrefix + code + "；" + detail
	audit.OK = false
	audit.Detail = stored
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: record artifact fetch rejection audit: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorArtifactFetchOperation,
		req.RequestDigest, code, stored, fmtTime(now)); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: persist artifact fetch rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: commit artifact fetch rejection: %w", err)
	}
	rejection := operatorError(code, detail)
	rejection.Audited = true
	return OperatorArtifactFetchResult{}, rejection
}

func canonicalOperatorArtifactFetchRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeArtifactFetchInvalid:
		return "artifact fetch request 或 prepared metadata 不合法", true
	case OperatorCodeArtifactFetchPrepareFailed:
		return "artifact metadata 無法準備；請使用新的 idempotency key 重試", true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodeArtifactFetchPreviewStale:
		return "registry metadata identity 已改變；請重新預覽", true
	case OperatorCodeArtifactFetchActive:
		return "同一 artifact 版本或 metadata identity 已有 active fetch", true
	default:
		return "", false
	}
}

func historicalOperatorArtifactFetchRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeArtifactFetchInvalid:
		return "原 request 或 prepared metadata 當時不合法", true
	case OperatorCodeArtifactFetchPrepareFailed:
		return "原 request 當時無法準備 artifact metadata", true
	case OperatorCodePreviewRequired:
		return "原 request 當時缺少必要的 preview_digest", true
	case OperatorCodeArtifactFetchPreviewStale:
		return "原 request 的 preview 與當時 registry metadata identity 不符", true
	case OperatorCodeArtifactFetchActive:
		return "原 request 當時已有同版本或同 identity 的 active fetch", true
	default:
		return "", false
	}
}

func isCanonicalOperatorArtifactFetchRejection(code string) bool {
	_, ok := canonicalOperatorArtifactFetchRejectionDetail(code)
	return ok
}

func (s *Store) replayOperatorArtifactFetch(tx dbTx, req OperatorArtifactFetchRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorArtifactFetchResult, error) {
	requestAt, err := canonicalArtifactFetchNow(s.now())
	if err != nil {
		return OperatorArtifactFetchResult{}, err
	}
	audit.At = requestAt
	if cached.Operation != operatorArtifactFetchOperation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK = false
		audit.Detail = "idempotency conflict：" + detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorArtifactFetchResult{}, fmt.Errorf("store: record artifact fetch idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorArtifactFetchResult{}, fmt.Errorf("store: commit artifact fetch idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorArtifactFetchResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		historical, historicalOK := historicalOperatorArtifactFetchRejectionDetail(cached.ErrorCode.String)
		canonical, canonicalOK := canonicalOperatorArtifactFetchRejectionDetail(cached.ErrorCode.String)
		wantStored := operatorArtifactFetchRejectPrefix + cached.ErrorCode.String + "；" + canonical
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			!historicalOK || !canonicalOK || cached.ErrorDetail.String != wantStored ||
			!canonicalArtifactFetchRawTime(cached.CreatedAt) {
			return s.rejectInvalidOperatorArtifactFetchCache(tx, audit)
		}
		valid, err := validateOperatorArtifactFetchOriginalAudit(tx, req, cached.CreatedAt, false, wantStored)
		if err != nil {
			return OperatorArtifactFetchResult{}, err
		}
		if !valid {
			return s.rejectInvalidOperatorArtifactFetchCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorArtifactFetchResult{}, fmt.Errorf("store: record rejected artifact fetch replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorArtifactFetchResult{}, fmt.Errorf("store: commit rejected artifact fetch replay audit: %w", err)
		}
		return OperatorArtifactFetchResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid ||
		cached.ErrorDetail.Valid || !canonicalArtifactFetchRawTime(cached.CreatedAt) {
		return s.rejectInvalidOperatorArtifactFetchCache(tx, audit)
	}
	receipt, err := decodeOperatorArtifactFetchReceipt(cached.ResponseJSON.String)
	if err != nil || !validOperatorArtifactFetchReceipt(receipt, req, cached.CreatedAt) {
		return s.rejectInvalidOperatorArtifactFetchCache(tx, audit)
	}
	record, err := artifactFetchRecordByID(tx, receipt.OperationID)
	if errors.Is(err, ErrArtifactFetchNotFound) || errors.Is(err, ErrArtifactFetchCorrupt) {
		return s.rejectInvalidOperatorArtifactFetchCache(tx, audit)
	}
	if err != nil {
		return OperatorArtifactFetchResult{}, err
	}
	if !artifactFetchReceiptMatchesRecord(receipt, req, record) {
		return s.rejectInvalidOperatorArtifactFetchCache(tx, audit)
	}
	valid, err := validateOperatorArtifactFetchOriginalAudit(tx, req, cached.CreatedAt, true,
		operatorArtifactFetchSuccessDetail(receipt))
	if err != nil {
		return OperatorArtifactFetchResult{}, err
	}
	if !valid {
		return s.rejectInvalidOperatorArtifactFetchCache(tx, audit)
	}
	audit.Subject = artifactFetchSubject(receipt.Name, receipt.Version)
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + fmt.Sprintf("operation_id=%s，current_state=%s",
		receipt.OperationID, record.Operation.State)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: record successful artifact fetch replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: commit successful artifact fetch replay audit: %w", err)
	}
	return OperatorArtifactFetchResult{Operation: record.Operation, Replayed: true, Audited: true}, nil
}

func (s *Store) rejectInvalidOperatorArtifactFetchCache(tx dbTx,
	audit AuditEntry,
) (OperatorArtifactFetchResult, error) {
	audit.Subject = "artifact fetch idempotency cache"
	audit.Reason = ""
	audit.OK = false
	audit.Detail = operatorArtifactFetchCacheInvalid
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: record invalid artifact fetch cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorArtifactFetchResult{}, fmt.Errorf("store: commit invalid artifact fetch cache audit: %w", err)
	}
	return OperatorArtifactFetchResult{Audited: true}, ErrArtifactFetchCacheInvalid
}

func validateOperatorArtifactFetchOriginalAudit(tx dbTx, req OperatorArtifactFetchRequest,
	createdAt string, ok bool, detail string,
) (bool, error) {
	audit := operatorArtifactFetchAudit(req)
	outcome := "failed"
	if ok {
		outcome = "ok"
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
 WHERE action=? AND COALESCE(machine_id,'')=? AND subject=? AND COALESCE(reason,'')=?
   AND idempotency_key=? AND request_digest=? AND at=? AND outcome=?
   AND COALESCE(detail,'')=?`, string(AuditArtifactFetch), audit.MachineID,
		truncAudit(audit.Subject, auditMaxReason), truncAudit(audit.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, createdAt, outcome, detail).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("store: inspect original artifact fetch audit: %w", err)
	}
	return count == 1, nil
}

func decodeOperatorArtifactFetchReceipt(raw string) (operatorArtifactFetchReceipt, error) {
	var receipt operatorArtifactFetchReceipt
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&receipt); err != nil {
		return receipt, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return receipt, errors.New("artifact fetch receipt has trailing JSON")
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || string(canonical) != raw {
		return receipt, errors.New("artifact fetch receipt is not canonical JSON")
	}
	return receipt, nil
}

func validOperatorArtifactFetchReceipt(receipt operatorArtifactFetchReceipt,
	req OperatorArtifactFetchRequest, cachedCreatedAt string,
) bool {
	return receipt.ReceiptVersion == operatorArtifactFetchReceiptVersion &&
		validArtifactFetchIdentifier(receipt.OperationID, 128) &&
		receipt.Name == req.Name && receipt.Version == req.Version &&
		validArtifactFetchRegistryOrigin(receipt.RegistryOrigin) &&
		validArtifactFetchDigest(receipt.IdentityDigest) &&
		receipt.PreviewDigest == req.PreviewDigest && validArtifactFetchDigest(receipt.PreviewDigest) &&
		receipt.MaxBytes > 0 && receipt.MaxBytes <= MaxArtifactFetchBytes &&
		canonicalOperatorTime(receipt.CreatedAt) && fmtTime(receipt.CreatedAt) == cachedCreatedAt
}

func artifactFetchReceiptMatchesRecord(receipt operatorArtifactFetchReceipt,
	req OperatorArtifactFetchRequest, record artifactFetchRecord,
) bool {
	op := record.Operation
	return op.OperationID == receipt.OperationID && op.Name == receipt.Name &&
		op.Version == receipt.Version && op.RegistryOrigin == receipt.RegistryOrigin &&
		op.IdentityDigest == receipt.IdentityDigest && op.PreviewDigest == receipt.PreviewDigest &&
		op.MaxBytes == receipt.MaxBytes && op.CreatedAt.Equal(receipt.CreatedAt) &&
		record.IdempotencyKey == req.IdempotencyKey && record.RequestDigest == req.RequestDigest &&
		artifactFetchIdentityDigest(ArtifactFetchPrepared{
			Name: op.Name, Version: op.Version, SourceKind: op.SourceKind, SourcePlan: record.SourcePlan,
			RegistryOrigin: op.RegistryOrigin,
			TarballURL:     record.TarballURL, SHA512Integrity: op.SHA512Integrity,
			EnginesNode: op.EnginesNode, MaxBytes: op.MaxBytes,
		}) == op.IdentityDigest
}

func operatorArtifactFetchSuccessDetail(receipt operatorArtifactFetchReceipt) string {
	return fmt.Sprintf("queued operation_id=%s，artifact=%s，identity=%s",
		receipt.OperationID, artifactFetchSubject(receipt.Name, receipt.Version), receipt.IdentityDigest)
}

// GetArtifactFetchOperation returns only the safe projection.
func (s *Store) GetArtifactFetchOperation(operationID string) (ArtifactFetchOperation, error) {
	record, err := s.getArtifactFetchRecord(operationID)
	return record.Operation, err
}

func (s *Store) getArtifactFetchRecord(operationID string) (artifactFetchRecord, error) {
	if !validArtifactFetchIdentifier(operationID, 128) {
		return artifactFetchRecord{}, ErrArtifactFetchNotFound
	}
	return artifactFetchRecordByID(s.rdb, operationID)
}

func (s *Store) ListArtifactFetchOperations(req ArtifactFetchListRequest) (ArtifactFetchListResult, error) {
	if req.Limit == 0 {
		req.Limit = DefaultArtifactFetchReadLimit
	}
	if req.Limit < 1 || req.Limit > MaxArtifactFetchReadLimit ||
		(req.State != "" && !validArtifactFetchState(req.State)) ||
		(req.Name != "" && !validArtifactFetchIdentifier(req.Name, 128)) ||
		(req.Version != "" && !validArtifactFetchIdentifier(req.Version, 128)) {
		return ArtifactFetchListResult{}, ErrArtifactFetchInvalid
	}
	where := []string{"1=1"}
	args := []any{}
	if req.State != "" {
		where = append(where, "state=?")
		args = append(args, req.State)
	}
	if req.Name != "" {
		where = append(where, "name=?")
		args = append(args, req.Name)
	}
	if req.Version != "" {
		where = append(where, "version=?")
		args = append(args, req.Version)
	}
	clause := strings.Join(where, " AND ")
	evaluatedAt, err := canonicalArtifactFetchNow(s.now())
	if err != nil {
		return ArtifactFetchListResult{}, err
	}
	result := ArtifactFetchListResult{
		SchemaVersion: ArtifactFetchReadSchemaVersion,
		Consistency:   ArtifactFetchReadConsistency,
		EvaluatedAt:   evaluatedAt,
		Items:         []ArtifactFetchOperation{},
	}
	// COUNT and page rows are one read snapshot. ReadOnly asks modernc SQLite for
	// a deferred transaction even though write transactions use _txlock=immediate,
	// allowing WAL writers to proceed without splitting this result across two
	// database versions.
	tx, err := s.rdb.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ArtifactFetchListResult{}, fmt.Errorf("store: begin artifact fetch list snapshot: %w", err)
	}
	defer tx.Rollback()
	if err := tx.QueryRow("SELECT COUNT(*) FROM artifact_fetch_operations WHERE "+clause, args...).Scan(&result.Total); err != nil {
		return ArtifactFetchListResult{}, fmt.Errorf("store: count artifact fetch operations: %w", err)
	}
	if s.afterArtifactFetchListCount != nil {
		s.afterArtifactFetchListCount()
	}
	args = append(args, req.Limit)
	rows, err := tx.Query("SELECT "+artifactFetchColumns+" FROM artifact_fetch_operations WHERE "+clause+
		" ORDER BY created_at DESC,operation_id DESC LIMIT ?", args...)
	if err != nil {
		return ArtifactFetchListResult{}, fmt.Errorf("store: list artifact fetch operations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		record, err := scanArtifactFetchRecord(rows)
		if err != nil {
			return ArtifactFetchListResult{}, err
		}
		result.Items = append(result.Items, record.Operation)
	}
	if err := rows.Err(); err != nil {
		return ArtifactFetchListResult{}, fmt.Errorf("store: read artifact fetch operations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return ArtifactFetchListResult{}, fmt.Errorf("store: close artifact fetch operations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ArtifactFetchListResult{}, fmt.Errorf("store: commit artifact fetch list snapshot: %w", err)
	}
	return result, nil
}

// ListArtifactFetchOperationIDsForWorker returns the oldest durable work first.
// It is deliberately separate from the newest-first operator read contract: a
// LIMIT applied before reversing a presentation page would starve intents older
// than that page.
func (s *Store) ListArtifactFetchOperationIDsForWorker(state ArtifactFetchState,
	limit int,
) ([]string, error) {
	if (state != ArtifactFetchQueued && state != ArtifactFetchRunning) ||
		limit < 1 || limit > MaxArtifactFetchReadLimit {
		return nil, ErrArtifactFetchInvalid
	}
	rows, err := s.rdb.Query(`SELECT operation_id FROM artifact_fetch_operations
 WHERE state=? ORDER BY created_at ASC,operation_id ASC LIMIT ?`, state, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list artifact fetch worker queue: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var operationID string
		if err := rows.Scan(&operationID); err != nil {
			return nil, fmt.Errorf("store: scan artifact fetch worker queue: %w", err)
		}
		if !validArtifactFetchIdentifier(operationID, 128) {
			return nil, fmt.Errorf("%w: invalid worker queue operation id", ErrArtifactFetchCorrupt)
		}
		ids = append(ids, operationID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read artifact fetch worker queue: %w", err)
	}
	return ids, nil
}

// FenceRunningArtifactFetchOperations durably revokes every pre-startup worker
// capability without doing external work or discarding attempt/progress
// evidence. A READY-time worker may subsequently reclaim these running rows;
// every progress/terminal write carrying an older token will fail closed.
func (s *Store) FenceRunningArtifactFetchOperations() (int, error) {
	tx, err := s.beginWrite(context.Background(), "fence_running_artifact_fetch_operations")
	if err != nil {
		return 0, fmt.Errorf("store: begin artifact fetch startup fence: %w", err)
	}
	defer tx.Rollback()
	type authority struct {
		operationID string
		runToken    string
	}
	rows, err := tx.Query(`SELECT operation_id,run_token FROM artifact_fetch_operations
 WHERE state='running' ORDER BY created_at ASC,operation_id ASC`)
	if err != nil {
		return 0, fmt.Errorf("store: list artifact fetch startup authorities: %w", err)
	}
	authorities := []authority{}
	for rows.Next() {
		var current authority
		if err := rows.Scan(&current.operationID, &current.runToken); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("store: scan artifact fetch startup authority: %w", err)
		}
		if !validArtifactFetchIdentifier(current.operationID, 128) ||
			!validArtifactFetchIdentifier(current.runToken, 200) {
			_ = rows.Close()
			return 0, fmt.Errorf("%w: invalid running worker authority", ErrArtifactFetchCorrupt)
		}
		authorities = append(authorities, current)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("store: read artifact fetch startup authorities: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("store: close artifact fetch startup authorities: %w", err)
	}
	for _, current := range authorities {
		replacement, err := newToken()
		if err != nil {
			return 0, err
		}
		result, err := tx.Exec(`UPDATE artifact_fetch_operations SET run_token=?
 WHERE operation_id=? AND state='running' AND run_token=?`, replacement,
			current.operationID, current.runToken)
		if err != nil {
			return 0, fmt.Errorf("store: fence artifact fetch startup authority: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return 0, fmt.Errorf("%w: running worker authority changed during startup fence", ErrArtifactFetchClaimLost)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit artifact fetch startup fence: %w", err)
	}
	return len(authorities), nil
}

// ClaimArtifactFetchOperation transfers one queued operation to a worker.
// reclaimRunning is used after startup fencing and by the serial runtime drain
// after a worker returned without being able to persist its terminal result. It
// rotates the run token while retaining phase and progress.
func (s *Store) ClaimArtifactFetchOperation(operationID string, reclaimRunning bool) (ArtifactFetchClaim, error) {
	if !validArtifactFetchIdentifier(operationID, 128) {
		return ArtifactFetchClaim{}, ErrArtifactFetchNotFound
	}
	tx, err := s.beginWrite(context.Background(), "claim_artifact_fetch_operation")
	if err != nil {
		return ArtifactFetchClaim{}, fmt.Errorf("store: begin artifact fetch claim: %w", err)
	}
	defer tx.Rollback()
	record, err := artifactFetchRecordByID(tx, operationID)
	if err != nil {
		return ArtifactFetchClaim{}, err
	}
	if record.Operation.State != ArtifactFetchQueued &&
		!(record.Operation.State == ArtifactFetchRunning && reclaimRunning) {
		return ArtifactFetchClaim{}, ErrArtifactFetchInvalidState
	}
	if record.Operation.Attempt == math.MaxInt64 {
		return ArtifactFetchClaim{}, ErrArtifactFetchInvalidState
	}
	token, err := newToken()
	if err != nil {
		return ArtifactFetchClaim{}, err
	}
	now, err := artifactFetchTransitionTime(s.now(), record.Operation.UpdatedAt)
	if err != nil {
		return ArtifactFetchClaim{}, err
	}
	phase, rank := record.Operation.Phase, artifactFetchPhaseRanks[record.Operation.Phase]
	if record.Operation.State == ArtifactFetchQueued {
		phase, rank = ArtifactFetchPhaseDownloading, 1
	}
	startedAt := record.Operation.StartedAt
	if record.Operation.State == ArtifactFetchQueued {
		startedAt = &now
	}
	if _, err := tx.Exec(`UPDATE artifact_fetch_operations
 SET state='running',phase=?,phase_rank=?,attempt=attempt+1,run_token=?,started_at=?,updated_at=?
	WHERE operation_id=?`, phase, rank, token, fmtTimePtr(startedAt), fmtTime(now), operationID); err != nil {
		return ArtifactFetchClaim{}, fmt.Errorf("store: claim artifact fetch operation: %w", err)
	}
	record, err = artifactFetchRecordByID(tx, operationID)
	if err != nil {
		return ArtifactFetchClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArtifactFetchClaim{}, fmt.Errorf("store: commit artifact fetch claim: %w", err)
	}
	return ArtifactFetchClaim{
		Operation: record.Operation, TarballURL: record.TarballURL,
		SourcePlan: record.SourcePlan, RunToken: token,
	}, nil
}

// AdvanceArtifactFetchOperation monotonically advances worker phase and byte
// progress.  The current run token fences a worker that survived a restart.
func (s *Store) AdvanceArtifactFetchOperation(operationID, runToken string,
	phase ArtifactFetchPhase, progressBytes int64,
) (ArtifactFetchOperation, error) {
	rank, ok := artifactFetchPhaseRanks[phase]
	if !ok || rank < 1 || rank > 3 || progressBytes < 0 {
		return ArtifactFetchOperation{}, ErrArtifactFetchProgress
	}
	tx, err := s.beginWrite(context.Background(), "advance_artifact_fetch_operation")
	if err != nil {
		return ArtifactFetchOperation{}, fmt.Errorf("store: begin artifact fetch progress: %w", err)
	}
	defer tx.Rollback()
	record, err := artifactFetchRecordByID(tx, operationID)
	if err != nil {
		return ArtifactFetchOperation{}, err
	}
	if record.Operation.State != ArtifactFetchRunning {
		return ArtifactFetchOperation{}, ErrArtifactFetchInvalidState
	}
	if !sameArtifactFetchToken(record.RunToken, runToken) {
		return ArtifactFetchOperation{}, ErrArtifactFetchClaimLost
	}
	currentRank := artifactFetchPhaseRanks[record.Operation.Phase]
	if rank < currentRank || progressBytes < record.Operation.ProgressBytes ||
		progressBytes > record.Operation.MaxBytes {
		return ArtifactFetchOperation{}, ErrArtifactFetchProgress
	}
	now, err := artifactFetchTransitionTime(s.now(), record.Operation.UpdatedAt)
	if err != nil {
		return ArtifactFetchOperation{}, err
	}
	if _, err := tx.Exec(`UPDATE artifact_fetch_operations
 SET phase=?,phase_rank=?,progress_bytes=?,updated_at=? WHERE operation_id=?`,
		phase, rank, progressBytes, fmtTime(now), operationID); err != nil {
		return ArtifactFetchOperation{}, fmt.Errorf("store: advance artifact fetch progress: %w", err)
	}
	record, err = artifactFetchRecordByID(tx, operationID)
	if err != nil {
		return ArtifactFetchOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArtifactFetchOperation{}, fmt.Errorf("store: commit artifact fetch progress: %w", err)
	}
	return record.Operation, nil
}

func (s *Store) SucceedArtifactFetchOperation(operationID, runToken, sha256Hex string,
	sizeBytes int64,
) (ArtifactFetchOperation, error) {
	if !validArtifactFetchSHA256(sha256Hex) || sizeBytes < 0 {
		return ArtifactFetchOperation{}, ErrArtifactFetchProgress
	}
	tx, err := s.beginWrite(context.Background(), "succeed_artifact_fetch_operation")
	if err != nil {
		return ArtifactFetchOperation{}, fmt.Errorf("store: begin artifact fetch success: %w", err)
	}
	defer tx.Rollback()
	record, err := artifactFetchRecordByID(tx, operationID)
	if err != nil {
		return ArtifactFetchOperation{}, err
	}
	if record.Operation.State != ArtifactFetchRunning || record.Operation.Phase != ArtifactFetchPhasePublishing {
		return ArtifactFetchOperation{}, ErrArtifactFetchInvalidState
	}
	if !sameArtifactFetchToken(record.RunToken, runToken) {
		return ArtifactFetchOperation{}, ErrArtifactFetchClaimLost
	}
	if sizeBytes < record.Operation.ProgressBytes || sizeBytes > record.Operation.MaxBytes {
		return ArtifactFetchOperation{}, ErrArtifactFetchProgress
	}
	now, err := artifactFetchTransitionTime(s.now(), record.Operation.UpdatedAt)
	if err != nil {
		return ArtifactFetchOperation{}, err
	}
	if _, err := tx.Exec(`UPDATE artifact_fetch_operations
 SET state='succeeded',phase='complete',phase_rank=4,progress_bytes=?,run_token=NULL,
     result_sha256=?,result_size_bytes=?,finished_at=?,updated_at=? WHERE operation_id=?`,
		sizeBytes, sha256Hex, sizeBytes, fmtTime(now), fmtTime(now), operationID); err != nil {
		return ArtifactFetchOperation{}, fmt.Errorf("store: complete artifact fetch: %w", err)
	}
	record, err = artifactFetchRecordByID(tx, operationID)
	if err != nil {
		return ArtifactFetchOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArtifactFetchOperation{}, fmt.Errorf("store: commit artifact fetch success: %w", err)
	}
	return record.Operation, nil
}

func (s *Store) FailArtifactFetchOperation(operationID, runToken, code, detail string) (ArtifactFetchOperation, error) {
	if !validArtifactFetchErrorCode(code) || !validArtifactFetchText(detail, ArtifactFetchMaxErrorBytes, false) {
		return ArtifactFetchOperation{}, ErrArtifactFetchProgress
	}
	tx, err := s.beginWrite(context.Background(), "fail_artifact_fetch_operation")
	if err != nil {
		return ArtifactFetchOperation{}, fmt.Errorf("store: begin artifact fetch failure: %w", err)
	}
	defer tx.Rollback()
	record, err := artifactFetchRecordByID(tx, operationID)
	if err != nil {
		return ArtifactFetchOperation{}, err
	}
	if record.Operation.State != ArtifactFetchRunning {
		return ArtifactFetchOperation{}, ErrArtifactFetchInvalidState
	}
	if !sameArtifactFetchToken(record.RunToken, runToken) {
		return ArtifactFetchOperation{}, ErrArtifactFetchClaimLost
	}
	now, err := artifactFetchTransitionTime(s.now(), record.Operation.UpdatedAt)
	if err != nil {
		return ArtifactFetchOperation{}, err
	}
	if _, err := tx.Exec(`UPDATE artifact_fetch_operations
 SET state='failed',run_token=NULL,error_code=?,error_detail=?,finished_at=?,updated_at=?
 WHERE operation_id=?`, code, detail, fmtTime(now), fmtTime(now), operationID); err != nil {
		return ArtifactFetchOperation{}, fmt.Errorf("store: fail artifact fetch: %w", err)
	}
	record, err = artifactFetchRecordByID(tx, operationID)
	if err != nil {
		return ArtifactFetchOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArtifactFetchOperation{}, fmt.Errorf("store: commit artifact fetch failure: %w", err)
	}
	return record.Operation, nil
}

func artifactFetchRecordByID(q operatorRowQuerier, operationID string) (artifactFetchRecord, error) {
	row := q.QueryRow("SELECT "+artifactFetchColumns+" FROM artifact_fetch_operations WHERE operation_id=?", operationID)
	record, err := scanArtifactFetchRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return artifactFetchRecord{}, ErrArtifactFetchNotFound
	}
	return record, err
}

type artifactFetchScanner interface {
	Scan(dest ...any) error
}

func scanArtifactFetchRecord(scanner artifactFetchScanner) (artifactFetchRecord, error) {
	var record artifactFetchRecord
	var state, phase, created, updated string
	var phaseRank int
	var runToken, resultSHA, errorCode, errorDetail, started, finished sql.NullString
	var resultSize sql.NullInt64
	err := scanner.Scan(
		&record.Operation.OperationID, &record.IdempotencyKey, &record.RequestDigest,
		&record.Operation.Name, &record.Operation.Version, &record.Operation.SourceKind, &record.SourcePlan,
		&record.Operation.RegistryOrigin,
		&record.TarballURL, &record.Operation.SHA512Integrity, &record.Operation.EnginesNode,
		&record.Operation.IdentityDigest, &record.Operation.PreviewDigest, &record.Operation.MaxBytes,
		&state, &phase, &phaseRank, &record.Operation.ProgressBytes, &record.Operation.Attempt,
		&runToken, &resultSHA, &resultSize, &errorCode, &errorDetail,
		&created, &updated, &started, &finished)
	if err != nil {
		return artifactFetchRecord{}, err
	}
	record.Operation.State = ArtifactFetchState(state)
	record.Operation.Phase = ArtifactFetchPhase(phase)
	record.RunToken = runToken.String
	record.Operation.CreatedAt = parseCanonicalArtifactFetchTime(created)
	record.Operation.UpdatedAt = parseCanonicalArtifactFetchTime(updated)
	record.Operation.StartedAt = parseCanonicalArtifactFetchTimePtr(started)
	record.Operation.FinishedAt = parseCanonicalArtifactFetchTimePtr(finished)
	if resultSHA.Valid {
		value := resultSHA.String
		record.Operation.ResultSHA256 = &value
	}
	if resultSize.Valid {
		value := resultSize.Int64
		record.Operation.ResultSizeBytes = &value
	}
	if errorCode.Valid {
		value := errorCode.String
		record.Operation.ErrorCode = &value
	}
	if errorDetail.Valid {
		value := errorDetail.String
		record.Operation.ErrorDetail = &value
	}
	if err := validateArtifactFetchRecord(record, phaseRank, runToken.Valid); err != nil {
		return artifactFetchRecord{}, err
	}
	return record, nil
}

func validateArtifactFetchRecord(record artifactFetchRecord, storedPhaseRank int, runTokenValid bool) error {
	op := record.Operation
	rank, phaseOK := artifactFetchPhaseRanks[op.Phase]
	bad := func() error { return fmt.Errorf("%w: operation %s", ErrArtifactFetchCorrupt, op.OperationID) }
	if !validArtifactFetchIdentifier(op.OperationID, 128) ||
		!validArtifactFetchIdentifier(record.IdempotencyKey, 200) ||
		!validArtifactFetchDigest(record.RequestDigest) ||
		!validArtifactFetchIdentifier(op.Name, 128) || !validArtifactFetchIdentifier(op.Version, 128) ||
		!validArtifactFetchSource(op.Name, op.SourceKind, record.SourcePlan, op.EnginesNode) ||
		!validArtifactFetchRegistryOrigin(op.RegistryOrigin) ||
		!validArtifactFetchTarballURL(record.TarballURL) ||
		!validArtifactFetchSHA512(op.SHA512Integrity) ||
		!validArtifactFetchText(op.EnginesNode, 512, true) ||
		!validArtifactFetchDigest(op.IdentityDigest) || !validArtifactFetchDigest(op.PreviewDigest) ||
		op.MaxBytes <= 0 || op.MaxBytes > MaxArtifactFetchBytes || op.ProgressBytes < 0 ||
		op.ProgressBytes > op.MaxBytes || op.Attempt < 0 || !validArtifactFetchState(op.State) ||
		!phaseOK || rank != storedPhaseRank || !canonicalOperatorTime(op.CreatedAt) ||
		!canonicalOperatorTime(op.UpdatedAt) || op.UpdatedAt.Before(op.CreatedAt) ||
		artifactFetchIdentityDigest(ArtifactFetchPrepared{
			Name: op.Name, Version: op.Version, SourceKind: op.SourceKind, SourcePlan: record.SourcePlan,
			RegistryOrigin: op.RegistryOrigin,
			TarballURL:     record.TarballURL, SHA512Integrity: op.SHA512Integrity,
			EnginesNode: op.EnginesNode, MaxBytes: op.MaxBytes,
		}) != op.IdentityDigest {
		return bad()
	}
	if op.StartedAt != nil && (!canonicalOperatorTime(*op.StartedAt) ||
		op.StartedAt.Before(op.CreatedAt) || op.StartedAt.After(op.UpdatedAt)) {
		return bad()
	}
	if op.FinishedAt != nil && (!canonicalOperatorTime(*op.FinishedAt) ||
		op.FinishedAt.Before(op.CreatedAt) || !op.FinishedAt.Equal(op.UpdatedAt)) {
		return bad()
	}
	switch op.State {
	case ArtifactFetchQueued:
		if op.Phase != ArtifactFetchPhaseQueued || op.ProgressBytes != 0 || op.Attempt != 0 ||
			runTokenValid || op.StartedAt != nil || op.FinishedAt != nil ||
			op.ResultSHA256 != nil || op.ResultSizeBytes != nil || op.ErrorCode != nil || op.ErrorDetail != nil {
			return bad()
		}
	case ArtifactFetchRunning:
		if rank < 1 || rank > 3 || op.Attempt < 1 || !runTokenValid || record.RunToken == "" ||
			op.StartedAt == nil || op.FinishedAt != nil || op.ResultSHA256 != nil ||
			op.ResultSizeBytes != nil || op.ErrorCode != nil || op.ErrorDetail != nil {
			return bad()
		}
	case ArtifactFetchSucceeded:
		if op.Phase != ArtifactFetchPhaseComplete || op.Attempt < 1 || runTokenValid ||
			op.StartedAt == nil || op.FinishedAt == nil || op.ResultSHA256 == nil ||
			!validArtifactFetchSHA256(*op.ResultSHA256) || op.ResultSizeBytes == nil ||
			*op.ResultSizeBytes != op.ProgressBytes || op.ErrorCode != nil || op.ErrorDetail != nil {
			return bad()
		}
	case ArtifactFetchFailed:
		if rank < 1 || rank > 3 || op.Attempt < 1 || runTokenValid || op.StartedAt == nil ||
			op.FinishedAt == nil || op.ResultSHA256 != nil || op.ResultSizeBytes != nil ||
			op.ErrorCode == nil || !validArtifactFetchErrorCode(*op.ErrorCode) || op.ErrorDetail == nil ||
			!validArtifactFetchText(*op.ErrorDetail, ArtifactFetchMaxErrorBytes, false) {
			return bad()
		}
	}
	return nil
}

func validateArtifactFetchPrepared(req OperatorArtifactFetchRequest, prepared ArtifactFetchPrepared) error {
	if prepared.Name != req.Name || prepared.Version != req.Version ||
		!validArtifactFetchIdentifier(prepared.Name, 128) ||
		!validArtifactFetchIdentifier(prepared.Version, 128) ||
		!validArtifactFetchSource(prepared.Name, prepared.SourceKind, prepared.SourcePlan, prepared.EnginesNode) ||
		!validArtifactFetchRegistryOrigin(prepared.RegistryOrigin) ||
		!validArtifactFetchTarballURL(prepared.TarballURL) ||
		!validArtifactFetchSHA512(prepared.SHA512Integrity) ||
		!validArtifactFetchText(prepared.EnginesNode, 512, true) ||
		prepared.MaxBytes <= 0 || prepared.MaxBytes > MaxArtifactFetchBytes ||
		!validArtifactFetchDigest(prepared.CurrentPreviewDigest) {
		return ErrArtifactFetchInvalid
	}
	return nil
}

func artifactFetchIdentityDigest(prepared ArtifactFetchPrepared) string {
	body := struct {
		Version         string `json:"version"`
		Name            string `json:"name"`
		ArtifactVersion string `json:"artifact_version"`
		RegistryOrigin  string `json:"registry_origin"`
		TarballURL      string `json:"tarball_url"`
		SHA512          string `json:"sha512_integrity"`
		EnginesNode     string `json:"engines_node"`
		MaxBytes        int64  `json:"max_bytes"`
		SourceKind      string `json:"source_kind,omitempty"`
		SourcePlan      string `json:"source_plan,omitempty"`
	}{
		Version: operatorArtifactFetchReceiptVersion, Name: prepared.Name,
		ArtifactVersion: prepared.Version, RegistryOrigin: prepared.RegistryOrigin,
		TarballURL: prepared.TarballURL, SHA512: prepared.SHA512Integrity,
		EnginesNode: prepared.EnginesNode, MaxBytes: prepared.MaxBytes,
		SourceKind: prepared.SourceKind, SourcePlan: prepared.SourcePlan,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validArtifactFetchIdentifier(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value || value == "." || value == ".." ||
		strings.ContainsAny(value, "/\\") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func validArtifactFetchText(value string, maxBytes int, allowEmpty bool) bool {
	if len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value ||
		(!allowEmpty && value == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func validArtifactFetchDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validArtifactFetchSHA256(strings.TrimPrefix(value, "sha256:"))
}

func validArtifactFetchSHA256(value string) bool {
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

func validArtifactFetchSource(name, kind, plan, enginesNode string) bool {
	switch kind {
	case "":
		return name == "openclaw" && plan == ""
	case artifact.ArtifactSourceNPM:
		return name == "openclaw" && plan == ""
	case artifact.ArtifactSourceNode:
		if name != "node-runtime" || enginesNode != "" || len(plan) == 0 ||
			len(plan) > artifact.MaxArtifactSourcePlanBytes || !artifact.ValidNodeRuntimeSourcePlan(plan) {
			return false
		}
		return true
	case artifact.ArtifactSourceHermesImage:
		if name != "hermes-agent" || enginesNode != "" || len(plan) == 0 ||
			len(plan) > artifact.MaxArtifactSourcePlanBytes || !artifact.ValidHermesImageSourcePlan(plan) {
			return false
		}
		return true
	default:
		return false
	}
}

func validArtifactFetchSHA512(value string) bool {
	encoded, ok := strings.CutPrefix(value, "sha512-")
	if !ok || encoded == "" {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	return err == nil && len(raw) == sha512.Size && base64.StdEncoding.EncodeToString(raw) == encoded
}

func validArtifactFetchRegistryOrigin(raw string) bool {
	if !validArtifactFetchText(raw, 2048, false) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.IsAbs() && u.Opaque == "" &&
		(u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.Hostname() != "" &&
		u.User == nil && u.Path == "" && u.RawPath == "" && !u.ForceQuery &&
		u.RawQuery == "" && u.Fragment == "" && u.RawFragment == "" && u.String() == raw
}

func validArtifactFetchTarballURL(raw string) bool {
	if !validArtifactFetchText(raw, artifact.MaxTarballURLBytes, false) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.IsAbs() && u.Opaque == "" &&
		(u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.Hostname() != "" &&
		u.User == nil && u.Path != "" && !u.ForceQuery && u.RawQuery == "" &&
		u.Fragment == "" && u.RawFragment == "" && u.String() == raw
}

func validArtifactFetchState(state ArtifactFetchState) bool {
	return state == ArtifactFetchQueued || state == ArtifactFetchRunning ||
		state == ArtifactFetchSucceeded || state == ArtifactFetchFailed
}

func validArtifactFetchErrorCode(code string) bool {
	if len(code) < 1 || len(code) > 64 || code[0] < 'A' || code[0] > 'Z' {
		return false
	}
	for _, c := range code {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func canonicalArtifactFetchRawTime(raw string) bool {
	value := parseCanonicalArtifactFetchTime(raw)
	return canonicalOperatorTime(value) && fmtTime(value) == raw
}

func parseCanonicalArtifactFetchTime(raw string) time.Time {
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil || fmtTime(value) != raw || !value.Equal(value.UTC().Truncate(time.Second)) {
		return time.Time{}
	}
	return value
}

func parseCanonicalArtifactFetchTimePtr(raw sql.NullString) *time.Time {
	if !raw.Valid {
		return nil
	}
	value := parseCanonicalArtifactFetchTime(raw.String)
	return &value
}

func artifactFetchTransitionTime(now, previous time.Time) (time.Time, error) {
	now, err := canonicalArtifactFetchNow(now)
	if err != nil || now.Before(previous) {
		return time.Time{}, ErrArtifactFetchClockRegression
	}
	return now, nil
}

func canonicalArtifactFetchNow(now time.Time) (time.Time, error) {
	now = now.UTC().Truncate(time.Second)
	if !canonicalOperatorTime(now) {
		return time.Time{}, ErrArtifactFetchClockRegression
	}
	return now, nil
}

func sameArtifactFetchToken(want, got string) bool {
	if want == "" || got == "" || len(want) != len(got) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
