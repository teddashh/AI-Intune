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

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

const (
	operatorCatalogManifestOperation      = "catalog-manifest-publish:v1"
	operatorCatalogManifestReceiptVersion = "v1"
	operatorCatalogManifestRejectPrefix   = "operator catalog manifest rejection code="
	operatorCatalogManifestCacheInvalid   = "catalog manifest idempotency receipt is invalid"
)

var ErrCatalogManifestCacheInvalid = errors.New("store: catalog manifest idempotency receipt is invalid")

type OperatorCatalogManifestRequest struct {
	Manifest       appcatalog.Manifest
	PublishedBy    string
	Reason         string
	IdempotencyKey string
	RequestDigest  string
	Audit          AuditEntry
}

type OperatorCatalogManifestVerify func() error

type OperatorCatalogManifestResult struct {
	Record           CatalogManifestRecord `json:"record"`
	AlreadyPublished bool                  `json:"already_published"`
	Replayed         bool                  `json:"replayed"`
	Audited          bool                  `json:"-"`
}

type operatorCatalogManifestReceipt struct {
	ReceiptVersion   string    `json:"receipt_version"`
	PackageID        string    `json:"package_id"`
	PackageVersion   string    `json:"package_version"`
	ManifestDigest   string    `json:"manifest_digest"`
	ArtifactSHA256   string    `json:"artifact_sha256"`
	PublishedAt      time.Time `json:"published_at"`
	PublishedBy      string    `json:"published_by"`
	AlreadyPublished bool      `json:"already_published"`
	AppliedAt        time.Time `json:"applied_at"`
}

// ApplyOperatorCatalogManifest admits one verified immutable manifest. A
// cache miss verifies external artifact evidence without holding a SQLite
// writer, then atomically commits the manifest, idempotency receipt, and audit.
func (s *Store) ApplyOperatorCatalogManifest(req OperatorCatalogManifestRequest,
	verify OperatorCatalogManifestVerify,
) (OperatorCatalogManifestResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorCatalogManifestResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if !validArtifactFetchDigest(req.RequestDigest) {
		return OperatorCatalogManifestResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 必須是 canonical sha256")
	}
	audit := operatorCatalogManifestAudit(req)

	lookupTx, err := s.db.Begin()
	if err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: begin catalog manifest idempotency lookup: %w", err)
	}
	cached, found, err := loadOperatorCatalogManifestCached(lookupTx, req.IdempotencyKey)
	if err != nil {
		_ = lookupTx.Rollback()
		return OperatorCatalogManifestResult{}, err
	}
	if found {
		defer lookupTx.Rollback()
		return s.replayOperatorCatalogManifest(lookupTx, req, audit, cached)
	}
	if err := lookupTx.Rollback(); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: finish catalog manifest idempotency lookup: %w", err)
	}

	canonical, raw, digest, canonicalErr := canonicalCatalogManifest(req.Manifest)
	rejectCode := ""
	switch {
	case canonicalErr != nil || !validCatalogPublisher(req.PublishedBy) ||
		!validArtifactFetchText(req.Reason, auditMaxReason, false):
		rejectCode = OperatorCodeCatalogManifestInvalid
	case verify == nil:
		return OperatorCatalogManifestResult{}, errors.New("store: catalog manifest verification callback is nil")
	}
	if rejectCode == "" {
		existing, existingErr := s.CatalogManifest(canonical.ID, canonical.Version)
		switch {
		case existingErr == nil && existing.Digest != digest:
			rejectCode = OperatorCodeCatalogManifestConflict
		case existingErr == nil || errors.Is(existingErr, ErrNotFound):
		case existingErr != nil:
			return OperatorCatalogManifestResult{}, existingErr
		}
	}
	if rejectCode == "" {
		if verifyErr := verify(); verifyErr != nil {
			var rejection *OperatorRequestError
			if !errors.As(verifyErr, &rejection) || !isCanonicalOperatorCatalogManifestRejection(rejection.Code) {
				return OperatorCatalogManifestResult{}, verifyErr
			}
			rejectCode = rejection.Code
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: begin catalog manifest publication: %w", err)
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Second)
	audit.At = now
	cached, found, err = loadOperatorCatalogManifestCached(tx, req.IdempotencyKey)
	if err != nil {
		return OperatorCatalogManifestResult{}, err
	}
	if found {
		return s.replayOperatorCatalogManifest(tx, req, audit, cached)
	}
	if rejectCode != "" {
		return s.rejectOperatorCatalogManifestTx(tx, req, audit, rejectCode, now)
	}

	alreadyPublished := false
	record, err := scanCatalogManifest(tx.QueryRow(`SELECT manifest_json,manifest_digest,published_at,published_by
	 FROM catalog_manifests WHERE package_id=? AND package_version=?`, canonical.ID, canonical.Version), canonical.ID, canonical.Version)
	switch {
	case err == nil:
		if record.Digest != digest {
			return s.rejectOperatorCatalogManifestTx(tx, req, audit, OperatorCodeCatalogManifestConflict, now)
		}
		alreadyPublished = true
	case errors.Is(err, ErrNotFound):
		if err := ensureCatalogPublicationCapacity(tx, "catalog_manifests"); err != nil {
			if errors.Is(err, ErrCatalogCapacity) {
				return s.rejectOperatorCatalogManifestTx(tx, req, audit, OperatorCodeCatalogCapacity, now)
			}
			return OperatorCatalogManifestResult{}, err
		}
		if _, err := tx.Exec(`INSERT INTO catalog_manifests
		 (package_id,package_version,manifest_json,manifest_digest,published_at,published_by)
		 VALUES (?,?,?,?,?,?)`, canonical.ID, canonical.Version, raw, digest, fmtTime(now), req.PublishedBy); err != nil {
			return OperatorCatalogManifestResult{}, fmt.Errorf("store: publish operator catalog manifest: %w", err)
		}
		record = CatalogManifestRecord{
			Manifest: canonical, Digest: digest, PublishedAt: now, PublishedBy: req.PublishedBy,
		}
	default:
		return OperatorCatalogManifestResult{}, err
	}

	receipt := operatorCatalogManifestReceipt{
		ReceiptVersion: operatorCatalogManifestReceiptVersion,
		PackageID:      canonical.ID, PackageVersion: canonical.Version,
		ManifestDigest: digest, ArtifactSHA256: canonical.Artifact.SHA256,
		PublishedAt: record.PublishedAt, PublishedBy: record.PublishedBy,
		AlreadyPublished: alreadyPublished, AppliedAt: now,
	}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: encode catalog manifest receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorCatalogManifestOperation,
		req.RequestDigest, string(receiptRaw), fmtTime(now)); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: persist catalog manifest receipt: %w", err)
	}
	audit.Subject = catalogManifestSubject(canonical.ID, canonical.Version)
	audit.OK = true
	audit.Detail = operatorCatalogManifestSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: record catalog manifest audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: commit catalog manifest publication: %w", err)
	}
	return OperatorCatalogManifestResult{
		Record: record, AlreadyPublished: alreadyPublished, Audited: true,
	}, nil
}

func operatorCatalogManifestAudit(req OperatorCatalogManifestRequest) AuditEntry {
	audit := req.Audit
	audit.Action = AuditCatalogManifest
	audit.Subject = catalogManifestSubject(req.Manifest.ID, req.Manifest.Version)
	if validArtifactFetchText(req.Reason, auditMaxReason, false) {
		audit.Reason = req.Reason
	} else {
		audit.Reason = ""
	}
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	return audit
}

func catalogManifestSubject(packageID, version string) string {
	if !validArtifactFetchIdentifier(packageID, 128) || !validArtifactFetchIdentifier(version, 128) {
		return "invalid catalog manifest request"
	}
	return packageID + "@" + version
}

func loadOperatorCatalogManifestCached(q operatorRowQuerier, key string) (operatorCachedRequest, bool, error) {
	var cached operatorCachedRequest
	err := q.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
	 FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operatorCachedRequest{}, false, nil
	}
	if err != nil {
		return operatorCachedRequest{}, false, fmt.Errorf("store: inspect catalog manifest idempotency key: %w", err)
	}
	return cached, true, nil
}

func (s *Store) rejectOperatorCatalogManifestTx(tx *sql.Tx, req OperatorCatalogManifestRequest,
	audit AuditEntry, code string, now time.Time,
) (OperatorCatalogManifestResult, error) {
	detail, ok := canonicalOperatorCatalogManifestRejectionDetail(code)
	if !ok {
		return OperatorCatalogManifestResult{}, errors.New("store: invalid catalog manifest rejection code")
	}
	stored := operatorCatalogManifestRejectPrefix + code + "；" + detail
	audit.OK = false
	audit.Detail = stored
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: record catalog manifest rejection audit: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
	 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorCatalogManifestOperation,
		req.RequestDigest, code, stored, fmtTime(now)); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: persist catalog manifest rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: commit catalog manifest rejection: %w", err)
	}
	rejection := operatorError(code, detail)
	rejection.Audited = true
	return OperatorCatalogManifestResult{}, rejection
}

func canonicalOperatorCatalogManifestRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeCatalogManifestInvalid:
		return "manifest、publisher 或 reason 不合法", true
	case OperatorCodeCatalogAdapterUnsupported:
		return "manifest adapter 無法執行；選擇支援的 package manifest", true
	case OperatorCodeCatalogArtifactUnavailable:
		return "artifact bytes 未通過完整驗證；重新取得相同 digest 的 artifact", true
	case OperatorCodeCatalogArtifactMismatch:
		return "artifact sidecar 與 manifest identity 不一致；修正 manifest 後重新發布", true
	case OperatorCodeCatalogManifestConflict:
		return "package identity 已綁定另一份 immutable manifest；改用新版本", true
	case OperatorCodeCatalogCapacity:
		return "catalog manifest ledger 已達容量上限；停止發布", true
	case OperatorCodeCatalogPreviewStale:
		return "catalog preview 已改變；重新預覽", true
	case OperatorCodeCatalogConfirmationMismatch:
		return "package confirmation 與 manifest 不一致", true
	case OperatorCodeCatalogDependencyInvalid:
		return "選擇已發布且符合 engines.node 的 Node runtime", true
	default:
		return "", false
	}
}

func historicalOperatorCatalogManifestRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeCatalogManifestInvalid:
		return "原 manifest publication request 不合法", true
	case OperatorCodeCatalogAdapterUnsupported:
		return "原 manifest 的 typed adapter 未受支援", true
	case OperatorCodeCatalogArtifactUnavailable:
		return "原 manifest 對應的 artifact bytes 未通過完整驗證", true
	case OperatorCodeCatalogArtifactMismatch:
		return "原 artifact sidecar 與 manifest identity 不一致", true
	case OperatorCodeCatalogManifestConflict:
		return "原 package identity 已綁定另一份 immutable manifest", true
	case OperatorCodeCatalogCapacity:
		return "原 publication 當時 catalog manifest ledger 已滿", true
	case OperatorCodeCatalogPreviewStale:
		return "原 catalog preview 與 publication request 不一致", true
	case OperatorCodeCatalogConfirmationMismatch:
		return "原 package confirmation 與 manifest 不一致", true
	case OperatorCodeCatalogDependencyInvalid:
		return "原 Node runtime dependency 不符合 package contract", true
	default:
		return "", false
	}
}

func isCanonicalOperatorCatalogManifestRejection(code string) bool {
	_, ok := canonicalOperatorCatalogManifestRejectionDetail(code)
	return ok
}

func (s *Store) replayOperatorCatalogManifest(tx *sql.Tx, req OperatorCatalogManifestRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorCatalogManifestResult, error) {
	audit.At = s.now().UTC().Truncate(time.Second)
	if cached.Operation != operatorCatalogManifestOperation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK = false
		audit.Detail = "idempotency conflict：" + detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorCatalogManifestResult{}, fmt.Errorf("store: record catalog manifest idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorCatalogManifestResult{}, fmt.Errorf("store: commit catalog manifest idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorCatalogManifestResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		canonical, canonicalOK := canonicalOperatorCatalogManifestRejectionDetail(cached.ErrorCode.String)
		historical, historicalOK := historicalOperatorCatalogManifestRejectionDetail(cached.ErrorCode.String)
		wantStored := operatorCatalogManifestRejectPrefix + cached.ErrorCode.String + "；" + canonical
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			!canonicalOK || !historicalOK || cached.ErrorDetail.String != wantStored ||
			!canonicalArtifactFetchRawTime(cached.CreatedAt) {
			return s.rejectInvalidOperatorCatalogManifestCache(tx, audit)
		}
		valid, err := validateOperatorCatalogManifestOriginalAudit(tx, req, cached.CreatedAt, false, wantStored)
		if err != nil {
			return OperatorCatalogManifestResult{}, err
		}
		if !valid {
			return s.rejectInvalidOperatorCatalogManifestCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorCatalogManifestResult{}, fmt.Errorf("store: record rejected catalog manifest replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorCatalogManifestResult{}, fmt.Errorf("store: commit rejected catalog manifest replay audit: %w", err)
		}
		return OperatorCatalogManifestResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid ||
		cached.ErrorDetail.Valid || !canonicalArtifactFetchRawTime(cached.CreatedAt) {
		return s.rejectInvalidOperatorCatalogManifestCache(tx, audit)
	}
	receipt, err := decodeOperatorCatalogManifestReceipt(cached.ResponseJSON.String)
	if err != nil || !validOperatorCatalogManifestReceipt(receipt, cached.CreatedAt) {
		return s.rejectInvalidOperatorCatalogManifestCache(tx, audit)
	}
	record, err := scanCatalogManifest(tx.QueryRow(`SELECT manifest_json,manifest_digest,published_at,published_by
	 FROM catalog_manifests WHERE package_id=? AND package_version=?`, receipt.PackageID, receipt.PackageVersion),
		receipt.PackageID, receipt.PackageVersion)
	if err != nil || !catalogManifestReceiptMatchesRecord(receipt, record) {
		if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrCatalogRecordCorrupt) {
			return OperatorCatalogManifestResult{}, err
		}
		return s.rejectInvalidOperatorCatalogManifestCache(tx, audit)
	}
	valid, err := validateOperatorCatalogManifestOriginalAudit(tx, req, cached.CreatedAt, true,
		operatorCatalogManifestSuccessDetail(receipt))
	if err != nil {
		return OperatorCatalogManifestResult{}, err
	}
	if !valid {
		return s.rejectInvalidOperatorCatalogManifestCache(tx, audit)
	}
	audit.Subject = catalogManifestSubject(receipt.PackageID, receipt.PackageVersion)
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + "manifest_digest=" + receipt.ManifestDigest
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: record successful catalog manifest replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: commit successful catalog manifest replay audit: %w", err)
	}
	return OperatorCatalogManifestResult{
		Record: record, AlreadyPublished: receipt.AlreadyPublished, Replayed: true, Audited: true,
	}, nil
}

func (s *Store) rejectInvalidOperatorCatalogManifestCache(tx *sql.Tx,
	audit AuditEntry,
) (OperatorCatalogManifestResult, error) {
	audit.Subject = "catalog manifest idempotency receipt"
	audit.Reason = ""
	audit.OK = false
	audit.Detail = operatorCatalogManifestCacheInvalid
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: record invalid catalog manifest cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorCatalogManifestResult{}, fmt.Errorf("store: commit invalid catalog manifest cache audit: %w", err)
	}
	return OperatorCatalogManifestResult{Audited: true}, ErrCatalogManifestCacheInvalid
}

func decodeOperatorCatalogManifestReceipt(raw string) (operatorCatalogManifestReceipt, error) {
	var receipt operatorCatalogManifestReceipt
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return receipt, errors.New("store: catalog manifest receipt has trailing JSON")
	}
	return receipt, nil
}

func validOperatorCatalogManifestReceipt(receipt operatorCatalogManifestReceipt, createdAt string) bool {
	return receipt.ReceiptVersion == operatorCatalogManifestReceiptVersion &&
		validArtifactFetchIdentifier(receipt.PackageID, 128) &&
		validArtifactFetchIdentifier(receipt.PackageVersion, 128) &&
		validArtifactFetchDigest(receipt.ManifestDigest) &&
		validArtifactFetchDigest("sha256:"+receipt.ArtifactSHA256) && validCatalogPublisher(receipt.PublishedBy) &&
		!receipt.PublishedAt.IsZero() && receipt.PublishedAt.Location() == time.UTC &&
		!receipt.AppliedAt.IsZero() && receipt.AppliedAt.Location() == time.UTC &&
		fmtTime(receipt.AppliedAt) == createdAt && !receipt.PublishedAt.After(receipt.AppliedAt)
}

func catalogManifestReceiptMatchesRecord(receipt operatorCatalogManifestReceipt, record CatalogManifestRecord) bool {
	return receipt.PackageID == record.Manifest.ID && receipt.PackageVersion == record.Manifest.Version &&
		receipt.ManifestDigest == record.Digest && receipt.ArtifactSHA256 == record.Manifest.Artifact.SHA256 &&
		receipt.PublishedAt.Equal(record.PublishedAt) && receipt.PublishedBy == record.PublishedBy
}

func operatorCatalogManifestSuccessDetail(receipt operatorCatalogManifestReceipt) string {
	return fmt.Sprintf("manifest_digest=%s，artifact_sha256=%s，already_published=%t",
		receipt.ManifestDigest, receipt.ArtifactSHA256, receipt.AlreadyPublished)
}

func validateOperatorCatalogManifestOriginalAudit(tx *sql.Tx, req OperatorCatalogManifestRequest,
	createdAt string, ok bool, detail string,
) (bool, error) {
	audit := operatorCatalogManifestAudit(req)
	outcome := "failed"
	if ok {
		outcome = "ok"
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND COALESCE(machine_id,'')=? AND subject=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome=?
	   AND COALESCE(detail,'')=?`, string(AuditCatalogManifest), audit.MachineID,
		truncAudit(audit.Subject, auditMaxReason), truncAudit(audit.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, createdAt, outcome, detail).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("store: inspect original catalog manifest audit: %w", err)
	}
	return count == 1, nil
}

const (
	operatorMachineProfileOperation      = "machine-profile-publish:v1"
	operatorMachineProfileReceiptVersion = "v1"
	operatorMachineProfileRejectPrefix   = "operator machine profile rejection code="
	operatorMachineProfileCacheInvalid   = "machine profile idempotency receipt is invalid"
)

var ErrMachineProfileCacheInvalid = errors.New("store: machine profile idempotency receipt is invalid")

type OperatorMachineProfileRequest struct {
	Profile        appcatalog.MachineProfile
	PublishedBy    string
	Reason         string
	IdempotencyKey string
	RequestDigest  string
	Audit          AuditEntry
}

type OperatorMachineProfileVerify func(appcatalog.MachineProfile) error

type OperatorMachineProfileResult struct {
	Record           MachineProfileRecord `json:"record"`
	AlreadyPublished bool                 `json:"already_published"`
	Replayed         bool                 `json:"replayed"`
	Audited          bool                 `json:"-"`
}

type operatorMachineProfileReceipt struct {
	ReceiptVersion   string    `json:"receipt_version"`
	ProfileID        string    `json:"profile_id"`
	ProfileRevision  int64     `json:"profile_revision"`
	ProfileDigest    string    `json:"profile_digest"`
	PublishedAt      time.Time `json:"published_at"`
	PublishedBy      string    `json:"published_by"`
	AlreadyPublished bool      `json:"already_published"`
	AppliedAt        time.Time `json:"applied_at"`
}

// ApplyOperatorMachineProfile publishes one exact resolvable profile and
// commits its immutable row, global idempotency receipt, and audit together.
func (s *Store) ApplyOperatorMachineProfile(req OperatorMachineProfileRequest,
	verify OperatorMachineProfileVerify,
) (OperatorMachineProfileResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorMachineProfileResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if !validArtifactFetchDigest(req.RequestDigest) {
		return OperatorMachineProfileResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 必須是 canonical sha256")
	}
	audit := operatorMachineProfileAudit(req)

	lookupTx, err := s.db.Begin()
	if err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: begin machine profile idempotency lookup: %w", err)
	}
	cached, found, err := loadOperatorMachineProfileCached(lookupTx, req.IdempotencyKey)
	if err != nil {
		_ = lookupTx.Rollback()
		return OperatorMachineProfileResult{}, err
	}
	if found {
		defer lookupTx.Rollback()
		return s.replayOperatorMachineProfile(lookupTx, req, audit, cached)
	}
	if err := lookupTx.Rollback(); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: finish machine profile idempotency lookup: %w", err)
	}

	canonical, raw, digest, canonicalErr := canonicalMachineProfile(req.Profile)
	rejectCode := ""
	switch {
	case canonicalErr != nil || !validCatalogPublisher(req.PublishedBy) ||
		!validArtifactFetchText(req.Reason, auditMaxReason, false):
		rejectCode = OperatorCodeMachineProfileInvalid
	case verify == nil:
		return OperatorMachineProfileResult{}, errors.New("store: machine profile verification callback is nil")
	}
	if rejectCode == "" {
		existing, existingErr := s.MachineProfile(canonical.ID, canonical.Revision)
		switch {
		case existingErr == nil && existing.Digest != digest:
			rejectCode = OperatorCodeMachineProfileConflict
		case existingErr == nil || errors.Is(existingErr, ErrNotFound):
		case existingErr != nil:
			return OperatorMachineProfileResult{}, existingErr
		}
	}
	if rejectCode == "" {
		if verifyErr := verify(canonical); verifyErr != nil {
			var rejection *OperatorRequestError
			if !errors.As(verifyErr, &rejection) || !isCanonicalOperatorMachineProfileRejection(rejection.Code) {
				return OperatorMachineProfileResult{}, verifyErr
			}
			rejectCode = rejection.Code
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: begin machine profile publication: %w", err)
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Second)
	audit.At = now
	cached, found, err = loadOperatorMachineProfileCached(tx, req.IdempotencyKey)
	if err != nil {
		return OperatorMachineProfileResult{}, err
	}
	if found {
		return s.replayOperatorMachineProfile(tx, req, audit, cached)
	}
	if rejectCode != "" {
		return s.rejectOperatorMachineProfileTx(tx, req, audit, rejectCode, now)
	}

	alreadyPublished := false
	record, err := scanMachineProfile(tx.QueryRow(`SELECT profile_json,profile_digest,published_at,published_by
	 FROM machine_profiles WHERE profile_id=? AND profile_revision=?`, canonical.ID, canonical.Revision), canonical.ID, canonical.Revision)
	switch {
	case err == nil:
		if record.Digest != digest {
			return s.rejectOperatorMachineProfileTx(tx, req, audit, OperatorCodeMachineProfileConflict, now)
		}
		alreadyPublished = true
	case errors.Is(err, ErrNotFound):
		manifests, listErr := catalogManifestRecordsTx(tx)
		if listErr != nil {
			return OperatorMachineProfileResult{}, listErr
		}
		if err := validateStoredProfile(canonical, manifests); err != nil {
			return s.rejectOperatorMachineProfileTx(tx, req, audit, OperatorCodeMachineProfileUnresolvable, now)
		}
		if err := ensureCatalogPublicationCapacity(tx, "machine_profiles"); err != nil {
			if errors.Is(err, ErrCatalogCapacity) {
				return s.rejectOperatorMachineProfileTx(tx, req, audit, OperatorCodeMachineProfileCapacity, now)
			}
			return OperatorMachineProfileResult{}, err
		}
		if _, err := tx.Exec(`INSERT INTO machine_profiles
		 (profile_id,profile_revision,profile_json,profile_digest,published_at,published_by)
		 VALUES (?,?,?,?,?,?)`, canonical.ID, canonical.Revision, raw, digest, fmtTime(now), req.PublishedBy); err != nil {
			return OperatorMachineProfileResult{}, fmt.Errorf("store: publish operator machine profile: %w", err)
		}
		record = MachineProfileRecord{
			Profile: canonical, Digest: digest, PublishedAt: now, PublishedBy: req.PublishedBy,
		}
	default:
		return OperatorMachineProfileResult{}, err
	}

	receipt := operatorMachineProfileReceipt{
		ReceiptVersion: operatorMachineProfileReceiptVersion,
		ProfileID:      canonical.ID, ProfileRevision: canonical.Revision, ProfileDigest: digest,
		PublishedAt: record.PublishedAt, PublishedBy: record.PublishedBy,
		AlreadyPublished: alreadyPublished, AppliedAt: now,
	}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: encode machine profile receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorMachineProfileOperation,
		req.RequestDigest, string(receiptRaw), fmtTime(now)); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: persist machine profile receipt: %w", err)
	}
	audit.Subject = machineProfileSubject(canonical.ID, canonical.Revision)
	audit.OK = true
	audit.Detail = operatorMachineProfileSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: record machine profile audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: commit machine profile publication: %w", err)
	}
	return OperatorMachineProfileResult{
		Record: record, AlreadyPublished: alreadyPublished, Audited: true,
	}, nil
}

func catalogManifestRecordsTx(tx *sql.Tx) ([]CatalogManifestRecord, error) {
	rows, err := tx.Query(`SELECT package_id,package_version,manifest_json,manifest_digest,published_at,published_by
	 FROM catalog_manifests ORDER BY package_id,package_version LIMIT ?`, maxStoredCatalogRecords+1)
	if err != nil {
		return nil, fmt.Errorf("store: list transaction catalog manifests: %w", err)
	}
	defer rows.Close()
	records := make([]CatalogManifestRecord, 0)
	for rows.Next() {
		var packageID, version, raw, digest, publishedAt, publishedBy string
		if err := rows.Scan(&packageID, &version, &raw, &digest, &publishedAt, &publishedBy); err != nil {
			return nil, fmt.Errorf("store: scan transaction catalog manifest: %w", err)
		}
		record, err := decodeCatalogManifestRecord(packageID, version, raw, digest, publishedAt, publishedBy)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate transaction catalog manifests: %w", err)
	}
	if len(records) > maxStoredCatalogRecords {
		return nil, fmt.Errorf("%w: catalog manifest count exceeds %d", ErrCatalogRecordCorrupt, maxStoredCatalogRecords)
	}
	return records, nil
}

func operatorMachineProfileAudit(req OperatorMachineProfileRequest) AuditEntry {
	audit := req.Audit
	audit.Action = AuditMachineProfile
	audit.Subject = machineProfileSubject(req.Profile.ID, req.Profile.Revision)
	if validArtifactFetchText(req.Reason, auditMaxReason, false) {
		audit.Reason = req.Reason
	} else {
		audit.Reason = ""
	}
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	return audit
}

func machineProfileSubject(profileID string, revision int64) string {
	if !validArtifactFetchIdentifier(profileID, 128) || revision <= 0 {
		return "invalid machine profile request"
	}
	return fmt.Sprintf("%s@%d", profileID, revision)
}

func loadOperatorMachineProfileCached(q operatorRowQuerier, key string) (operatorCachedRequest, bool, error) {
	var cached operatorCachedRequest
	err := q.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
	 FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operatorCachedRequest{}, false, nil
	}
	if err != nil {
		return operatorCachedRequest{}, false, fmt.Errorf("store: inspect machine profile idempotency key: %w", err)
	}
	return cached, true, nil
}

func (s *Store) rejectOperatorMachineProfileTx(tx *sql.Tx, req OperatorMachineProfileRequest,
	audit AuditEntry, code string, now time.Time,
) (OperatorMachineProfileResult, error) {
	detail, ok := canonicalOperatorMachineProfileRejectionDetail(code)
	if !ok {
		return OperatorMachineProfileResult{}, errors.New("store: invalid machine profile rejection code")
	}
	stored := operatorMachineProfileRejectPrefix + code + "；" + detail
	audit.OK = false
	audit.Detail = stored
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: record machine profile rejection audit: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
	 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorMachineProfileOperation,
		req.RequestDigest, code, stored, fmtTime(now)); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: persist machine profile rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: commit machine profile rejection: %w", err)
	}
	rejection := operatorError(code, detail)
	rejection.Audited = true
	return OperatorMachineProfileResult{}, rejection
}

func canonicalOperatorMachineProfileRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeMachineProfileInvalid:
		return "profile、publisher 或 reason 不合法；修正後重新發布", true
	case OperatorCodeMachineProfileUnresolvable:
		return "profile package graph 無法解析；修正 package selection", true
	case OperatorCodeMachineProfileConflict:
		return "profile identity 已綁定另一份 immutable profile；增加 revision", true
	case OperatorCodeMachineProfileCapacity:
		return "machine profile ledger 已達容量上限；停止發布", true
	case OperatorCodeCatalogAdapterUnsupported:
		return "profile 包含無法執行的 manifest adapter；選擇支援的 package", true
	case OperatorCodeCatalogArtifactUnavailable:
		return "profile artifact bytes 未通過完整驗證；重新取得相同 digest 的 artifact", true
	case OperatorCodeCatalogArtifactMismatch:
		return "profile artifact sidecar 與 manifest identity 不一致；修正 package manifest", true
	case OperatorCodeMachineProfilePreviewStale:
		return "machine profile preview 已改變；重新預覽", true
	case OperatorCodeMachineProfileConfirmationMismatch:
		return "profile confirmation 與 publication 不一致", true
	default:
		return "", false
	}
}

func historicalOperatorMachineProfileRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeMachineProfileInvalid:
		return "原 machine profile publication request 不合法", true
	case OperatorCodeMachineProfileUnresolvable:
		return "原 profile package graph 無法解析", true
	case OperatorCodeMachineProfileConflict:
		return "原 profile identity 已綁定另一份 immutable profile", true
	case OperatorCodeMachineProfileCapacity:
		return "原 publication 當時 machine profile ledger 已滿", true
	case OperatorCodeCatalogAdapterUnsupported:
		return "原 profile 包含無法執行的 manifest adapter", true
	case OperatorCodeCatalogArtifactUnavailable:
		return "原 profile artifact bytes 未通過完整驗證", true
	case OperatorCodeCatalogArtifactMismatch:
		return "原 profile artifact sidecar 與 manifest identity 不一致", true
	case OperatorCodeMachineProfilePreviewStale:
		return "原 machine profile preview 與 publication request 不一致", true
	case OperatorCodeMachineProfileConfirmationMismatch:
		return "原 profile confirmation 與 publication 不一致", true
	default:
		return "", false
	}
}

func isCanonicalOperatorMachineProfileRejection(code string) bool {
	_, ok := canonicalOperatorMachineProfileRejectionDetail(code)
	return ok
}

func (s *Store) replayOperatorMachineProfile(tx *sql.Tx, req OperatorMachineProfileRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorMachineProfileResult, error) {
	audit.At = s.now().UTC().Truncate(time.Second)
	if cached.Operation != operatorMachineProfileOperation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK = false
		audit.Detail = "idempotency conflict：" + detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineProfileResult{}, fmt.Errorf("store: record machine profile idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineProfileResult{}, fmt.Errorf("store: commit machine profile idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return OperatorMachineProfileResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		canonical, canonicalOK := canonicalOperatorMachineProfileRejectionDetail(cached.ErrorCode.String)
		historical, historicalOK := historicalOperatorMachineProfileRejectionDetail(cached.ErrorCode.String)
		wantStored := operatorMachineProfileRejectPrefix + cached.ErrorCode.String + "；" + canonical
		if !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid || cached.ResponseJSON.Valid ||
			!canonicalOK || !historicalOK || cached.ErrorDetail.String != wantStored ||
			!canonicalArtifactFetchRawTime(cached.CreatedAt) {
			return s.rejectInvalidOperatorMachineProfileCache(tx, audit)
		}
		valid, err := validateOperatorMachineProfileOriginalAudit(tx, req, cached.CreatedAt, false, wantStored)
		if err != nil {
			return OperatorMachineProfileResult{}, err
		}
		if !valid {
			return s.rejectInvalidOperatorMachineProfileCache(tx, audit)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineProfileResult{}, fmt.Errorf("store: record rejected machine profile replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineProfileResult{}, fmt.Errorf("store: commit rejected machine profile replay audit: %w", err)
		}
		return OperatorMachineProfileResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid ||
		cached.ErrorDetail.Valid || !canonicalArtifactFetchRawTime(cached.CreatedAt) {
		return s.rejectInvalidOperatorMachineProfileCache(tx, audit)
	}
	receipt, err := decodeOperatorMachineProfileReceipt(cached.ResponseJSON.String)
	if err != nil || !validOperatorMachineProfileReceipt(receipt, cached.CreatedAt) {
		return s.rejectInvalidOperatorMachineProfileCache(tx, audit)
	}
	record, err := scanMachineProfile(tx.QueryRow(`SELECT profile_json,profile_digest,published_at,published_by
	 FROM machine_profiles WHERE profile_id=? AND profile_revision=?`, receipt.ProfileID, receipt.ProfileRevision),
		receipt.ProfileID, receipt.ProfileRevision)
	if err != nil || !machineProfileReceiptMatchesRecord(receipt, record) {
		if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrCatalogRecordCorrupt) {
			return OperatorMachineProfileResult{}, err
		}
		return s.rejectInvalidOperatorMachineProfileCache(tx, audit)
	}
	valid, err := validateOperatorMachineProfileOriginalAudit(tx, req, cached.CreatedAt, true,
		operatorMachineProfileSuccessDetail(receipt))
	if err != nil {
		return OperatorMachineProfileResult{}, err
	}
	if !valid {
		return s.rejectInvalidOperatorMachineProfileCache(tx, audit)
	}
	audit.Subject = machineProfileSubject(receipt.ProfileID, receipt.ProfileRevision)
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + "profile_digest=" + receipt.ProfileDigest
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: record successful machine profile replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: commit successful machine profile replay audit: %w", err)
	}
	return OperatorMachineProfileResult{
		Record: record, AlreadyPublished: receipt.AlreadyPublished, Replayed: true, Audited: true,
	}, nil
}

func (s *Store) rejectInvalidOperatorMachineProfileCache(tx *sql.Tx,
	audit AuditEntry,
) (OperatorMachineProfileResult, error) {
	audit.Subject = "machine profile idempotency receipt"
	audit.Reason = ""
	audit.OK = false
	audit.Detail = operatorMachineProfileCacheInvalid
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: record invalid machine profile cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineProfileResult{}, fmt.Errorf("store: commit invalid machine profile cache audit: %w", err)
	}
	return OperatorMachineProfileResult{Audited: true}, ErrMachineProfileCacheInvalid
}

func decodeOperatorMachineProfileReceipt(raw string) (operatorMachineProfileReceipt, error) {
	var receipt operatorMachineProfileReceipt
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return receipt, errors.New("store: machine profile receipt has trailing JSON")
	}
	return receipt, nil
}

func validOperatorMachineProfileReceipt(receipt operatorMachineProfileReceipt, createdAt string) bool {
	return receipt.ReceiptVersion == operatorMachineProfileReceiptVersion &&
		validArtifactFetchIdentifier(receipt.ProfileID, 128) && receipt.ProfileRevision > 0 &&
		validArtifactFetchDigest(receipt.ProfileDigest) && validCatalogPublisher(receipt.PublishedBy) &&
		!receipt.PublishedAt.IsZero() && receipt.PublishedAt.Location() == time.UTC &&
		!receipt.AppliedAt.IsZero() && receipt.AppliedAt.Location() == time.UTC &&
		fmtTime(receipt.AppliedAt) == createdAt && !receipt.PublishedAt.After(receipt.AppliedAt)
}

func machineProfileReceiptMatchesRecord(receipt operatorMachineProfileReceipt, record MachineProfileRecord) bool {
	return receipt.ProfileID == record.Profile.ID && receipt.ProfileRevision == record.Profile.Revision &&
		receipt.ProfileDigest == record.Digest && receipt.PublishedAt.Equal(record.PublishedAt) &&
		receipt.PublishedBy == record.PublishedBy
}

func operatorMachineProfileSuccessDetail(receipt operatorMachineProfileReceipt) string {
	return fmt.Sprintf("profile_digest=%s，already_published=%t", receipt.ProfileDigest, receipt.AlreadyPublished)
}

func validateOperatorMachineProfileOriginalAudit(tx *sql.Tx, req OperatorMachineProfileRequest,
	createdAt string, ok bool, detail string,
) (bool, error) {
	audit := operatorMachineProfileAudit(req)
	outcome := "failed"
	if ok {
		outcome = "ok"
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND COALESCE(machine_id,'')=? AND subject=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome=?
	   AND COALESCE(detail,'')=?`, string(AuditMachineProfile), audit.MachineID,
		truncAudit(audit.Subject, auditMaxReason), truncAudit(audit.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, createdAt, outcome, detail).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("store: inspect original machine profile audit: %w", err)
	}
	return count == 1, nil
}
