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
)

const (
	operatorTailnetPeerIgnoreReceiptVersion = "v1"
	operatorTailnetPeerIgnoreCacheInvalid   = "operator tailnet peer ignore idempotency cache invalid；未回放結果"
)

type TailnetPeerIgnore struct {
	PeerID    string    `json:"peer_id"`
	Hostname  string    `json:"hostname"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expires_at"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by"`
	Active    bool      `json:"active"`
}

func (s *Store) TailnetPeerIgnores(now time.Time) ([]TailnetPeerIgnore, error) {
	rows, err := s.db.Query(`SELECT peer_id,hostname,reason,expires_at,revision,created_at,updated_at,created_by
	 FROM tailnet_peer_ignores WHERE expires_at>? ORDER BY expires_at,lower(hostname),peer_id`, fmtTime(now.UTC()))
	if err != nil {
		return nil, fmt.Errorf("store: list tailnet peer ignores: %w", err)
	}
	defer rows.Close()
	var out []TailnetPeerIgnore
	for rows.Next() {
		item, err := scanTailnetPeerIgnore(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) TailnetPeerIgnore(peerID string, now time.Time) (TailnetPeerIgnore, bool, error) {
	row := s.db.QueryRow(`SELECT peer_id,hostname,reason,expires_at,revision,created_at,updated_at,created_by
	 FROM tailnet_peer_ignores WHERE peer_id=?`, strings.TrimSpace(peerID))
	item, err := scanTailnetPeerIgnore(row, now)
	if errors.Is(err, sql.ErrNoRows) {
		return TailnetPeerIgnore{}, false, nil
	}
	return item, err == nil, err
}

type tailnetIgnoreScanner interface{ Scan(...any) error }

func scanTailnetPeerIgnore(row tailnetIgnoreScanner, now time.Time) (TailnetPeerIgnore, error) {
	var item TailnetPeerIgnore
	var expires, created, updated string
	if err := row.Scan(&item.PeerID, &item.Hostname, &item.Reason, &expires, &item.Revision,
		&created, &updated, &item.CreatedBy); err != nil {
		return item, err
	}
	item.ExpiresAt = parseTime(expires)
	item.CreatedAt = parseTime(created)
	item.UpdatedAt = parseTime(updated)
	if item.ExpiresAt.IsZero() || item.CreatedAt.IsZero() || item.UpdatedAt.IsZero() {
		return item, errors.New("store: tailnet peer ignore contains an invalid timestamp")
	}
	item.Active = item.ExpiresAt.After(now.UTC())
	return item, nil
}

type OperatorTailnetPeerIgnoreRequest struct {
	PeerID               string
	Hostname             string
	Action               string
	ExpiresAt            time.Time
	ExpectedRevision     int64
	ConfirmHostname      string
	PreviewDigest        string
	CurrentPreviewDigest string
	Reason               string
	IdempotencyKey       string
	RequestDigest        string
	CreatedBy            string
	Audit                AuditEntry
}

type OperatorTailnetPeerIgnoreResult struct {
	PeerID          string    `json:"peer_id"`
	Hostname        string    `json:"hostname"`
	Action          string    `json:"action"`
	PreviousIgnored bool      `json:"previous_ignored"`
	Ignored         bool      `json:"ignored"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	Revision        int64     `json:"revision"`
	Replayed        bool      `json:"replayed"`
	Audited         bool      `json:"-"`
}

type operatorTailnetPeerIgnoreReceipt struct {
	ReceiptVersion  string    `json:"receipt_version"`
	PeerID          string    `json:"peer_id"`
	Hostname        string    `json:"hostname"`
	Action          string    `json:"action"`
	PreviousIgnored bool      `json:"previous_ignored"`
	Ignored         bool      `json:"ignored"`
	ExpiresAt       time.Time `json:"expires_at"`
	Revision        int64     `json:"revision"`
	AppliedAt       time.Time `json:"applied_at"`
}

func (s *Store) ApplyOperatorTailnetPeerIgnore(req OperatorTailnetPeerIgnoreRequest) (OperatorTailnetPeerIgnoreResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) != req.IdempotencyKey || req.IdempotencyKey == "" || len(req.IdempotencyKey) > 200 {
		return OperatorTailnetPeerIgnoreResult{}, operatorError(OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if !validArtifactFetchDigest(req.RequestDigest) {
		return OperatorTailnetPeerIgnoreResult{}, operatorError(OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}
	operation := "tailnet-peer-ignore:" + req.PeerID
	audit := req.Audit
	audit.Action = AuditTailnetPeerIgnore
	audit.Subject = req.Hostname
	audit.Reason = req.Reason
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorTailnetPeerIgnoreResult{}, fmt.Errorf("store: begin tailnet peer ignore: %w", err)
	}
	defer tx.Rollback()

	var cached operatorCachedRequest
	err = tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
	 FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	if err == nil {
		if cached.Operation != operation || cached.Digest != req.RequestDigest {
			audit.OK, audit.Detail = false, "idempotency conflict"
			if err := s.recordAuditTx(tx, audit); err != nil {
				return OperatorTailnetPeerIgnoreResult{}, err
			}
			if err := tx.Commit(); err != nil {
				return OperatorTailnetPeerIgnoreResult{}, err
			}
			rejection := operatorError(OperatorCodeIdempotencyConflict, "idempotency key 已被不同 request 使用")
			rejection.Audited = true
			return OperatorTailnetPeerIgnoreResult{}, rejection
		}
		if cached.Outcome == "rejected" {
			if cached.ResponseJSON.Valid || !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid ||
				!canonicalTailnetPeerIgnoreRejection(cached.ErrorCode.String, cached.ErrorDetail.String) ||
				!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
				return s.rejectInvalidTailnetPeerIgnoreCache(tx, audit)
			}
			valid, evidenceErr := validateTailnetPeerIgnoreAuditEvidence(tx, req, cached.CreatedAt,
				false, cached.ErrorDetail.String)
			if evidenceErr != nil || !valid {
				return s.rejectInvalidTailnetPeerIgnoreCache(tx, audit)
			}
			audit.OK, audit.Detail = false, OperatorIdempotencyReplayPrefix+"原判決："+cached.ErrorDetail.String
			if err := s.recordAuditTx(tx, audit); err != nil {
				return OperatorTailnetPeerIgnoreResult{}, err
			}
			if err := tx.Commit(); err != nil {
				return OperatorTailnetPeerIgnoreResult{}, err
			}
			return OperatorTailnetPeerIgnoreResult{}, &OperatorRequestError{Code: cached.ErrorCode.String, Detail: cached.ErrorDetail.String, Replayed: true, Audited: true}
		}
		if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid || cached.ErrorDetail.Valid ||
			!canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
			return s.rejectInvalidTailnetPeerIgnoreCache(tx, audit)
		}
		receipt, receiptErr := decodeTailnetPeerIgnoreReceipt(cached.ResponseJSON.String)
		if receiptErr != nil || validateTailnetPeerIgnoreReceipt(receipt, req, cached.CreatedAt) != nil {
			return s.rejectInvalidTailnetPeerIgnoreCache(tx, audit)
		}
		valid, evidenceErr := validateTailnetPeerIgnoreAuditEvidence(tx, req, cached.CreatedAt,
			true, tailnetPeerIgnoreSuccessAuditDetail(receipt))
		if evidenceErr != nil || !valid {
			return s.rejectInvalidTailnetPeerIgnoreCache(tx, audit)
		}
		result := tailnetPeerIgnoreResult(receipt)
		result.Replayed, result.Audited = true, true
		audit.Subject = receipt.Hostname
		audit.OK, audit.Detail = true, OperatorIdempotencyReplayPrefix+"沒有再次變更 Tailnet 規則"
		if err := s.recordAuditTx(tx, audit); err != nil {
			return result, err
		}
		if err := tx.Commit(); err != nil {
			return result, err
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return OperatorTailnetPeerIgnoreResult{}, fmt.Errorf("store: inspect tailnet idempotency key: %w", err)
	}

	reject := func(code, detail string) (OperatorTailnetPeerIgnoreResult, error) {
		if !canonicalTailnetPeerIgnoreRejection(code, detail) {
			return OperatorTailnetPeerIgnoreResult{}, errors.New("store: invalid tailnet peer ignore rejection")
		}
		rejectedAt := s.now().UTC()
		audit.At = rejectedAt
		audit.OK, audit.Detail = false, detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorTailnetPeerIgnoreResult{}, err
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
			 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operation, req.RequestDigest, code, detail, fmtTime(rejectedAt)); err != nil {
			return OperatorTailnetPeerIgnoreResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorTailnetPeerIgnoreResult{}, err
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorTailnetPeerIgnoreResult{}, rejection
	}

	peerID, hostname, reason := strings.TrimSpace(req.PeerID), strings.TrimSpace(req.Hostname), strings.TrimSpace(req.Reason)
	if peerID == "" || len(peerID) > 128 || hostname == "" || len(hostname) > 255 || reason == "" || len(reason) > 500 ||
		(req.Action != "ignore" && req.Action != "unignore") {
		return reject(OperatorCodeTailnetPeerInvalid, "peer、action 與 reason 必須完整且合法")
	}
	if req.ConfirmHostname != hostname {
		return reject(OperatorCodeConfirmationMismatch, "確認名稱與目前 Tailnet hostname 不同")
	}
	if req.PreviewDigest == "" || req.PreviewDigest != req.CurrentPreviewDigest {
		return reject(OperatorCodeTailnetPeerPreviewStale, "Tailnet peer 或忽略規則已變更；請重新預覽")
	}

	now := s.now().UTC()
	var current TailnetPeerIgnore
	var expires, created, updated string
	err = tx.QueryRow(`SELECT peer_id,hostname,reason,expires_at,revision,created_at,updated_at,created_by
	 FROM tailnet_peer_ignores WHERE peer_id=?`, peerID).Scan(&current.PeerID, &current.Hostname, &current.Reason,
		&expires, &current.Revision, &created, &updated, &current.CreatedBy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OperatorTailnetPeerIgnoreResult{}, err
	}
	found := err == nil
	if found {
		current.ExpiresAt = parseTime(expires)
		current.Active = current.ExpiresAt.After(now)
	}
	if (found && current.Revision != req.ExpectedRevision) || (!found && req.ExpectedRevision != 0) {
		return reject(OperatorCodePreconditionFailed, "忽略規則 revision 已變更；請重新預覽")
	}
	result := OperatorTailnetPeerIgnoreResult{PeerID: peerID, Hostname: hostname, Action: req.Action,
		PreviousIgnored: found && current.Active}
	if req.Action == "ignore" {
		if !req.ExpiresAt.After(now) || req.ExpiresAt.After(now.Add(366*24*time.Hour)) {
			return reject(OperatorCodeTailnetPeerInvalid, "expires_at 必須在現在之後且不超過 366 天")
		}
		if found {
			result.Revision = current.Revision + 1
			_, err = tx.Exec(`UPDATE tailnet_peer_ignores SET hostname=?,reason=?,expires_at=?,revision=?,updated_at=?,created_by=? WHERE peer_id=? AND revision=?`,
				hostname, reason, fmtTime(req.ExpiresAt.UTC()), result.Revision, fmtTime(now), req.CreatedBy, peerID, current.Revision)
		} else {
			result.Revision = 1
			_, err = tx.Exec(`INSERT INTO tailnet_peer_ignores(peer_id,hostname,reason,expires_at,revision,created_at,updated_at,created_by)
			 VALUES(?,?,?,?,1,?,?,?)`, peerID, hostname, reason, fmtTime(req.ExpiresAt.UTC()), fmtTime(now), fmtTime(now), req.CreatedBy)
		}
		if err != nil {
			return result, fmt.Errorf("store: write tailnet peer ignore: %w", err)
		}
		result.Ignored, result.ExpiresAt = true, req.ExpiresAt.UTC()
	} else {
		if !found || !current.Active {
			return reject(OperatorCodeTailnetPeerInvalid, "這台 peer 目前沒有有效的忽略規則")
		}
		if _, err = tx.Exec(`DELETE FROM tailnet_peer_ignores WHERE peer_id=? AND revision=?`, peerID, current.Revision); err != nil {
			return result, fmt.Errorf("store: remove tailnet peer ignore: %w", err)
		}
		result.Revision = current.Revision
	}
	receipt := operatorTailnetPeerIgnoreReceipt{
		ReceiptVersion: operatorTailnetPeerIgnoreReceiptVersion,
		PeerID:         result.PeerID, Hostname: result.Hostname, Action: result.Action,
		PreviousIgnored: result.PreviousIgnored, Ignored: result.Ignored,
		ExpiresAt: result.ExpiresAt, Revision: result.Revision, AppliedAt: now,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return result, err
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency(idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES(?,?,?,'ok',?,?)`, req.IdempotencyKey, operation, req.RequestDigest, string(raw), fmtTime(now)); err != nil {
		return result, err
	}
	audit.At, audit.OK, audit.Detail = now, true, tailnetPeerIgnoreSuccessAuditDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.Audited = true
	return result, nil
}

func canonicalTailnetPeerIgnoreRejection(code, detail string) bool {
	allowed := map[string][]string{
		OperatorCodeTailnetPeerInvalid: {
			"peer、action 與 reason 必須完整且合法",
			"expires_at 必須在現在之後且不超過 366 天",
			"這台 peer 目前沒有有效的忽略規則",
		},
		OperatorCodeConfirmationMismatch:    {"確認名稱與目前 Tailnet hostname 不同"},
		OperatorCodeTailnetPeerPreviewStale: {"Tailnet peer 或忽略規則已變更；請重新預覽"},
		OperatorCodePreconditionFailed:      {"忽略規則 revision 已變更；請重新預覽"},
	}
	for _, candidate := range allowed[code] {
		if detail == candidate {
			return true
		}
	}
	return false
}

func decodeTailnetPeerIgnoreReceipt(raw string) (operatorTailnetPeerIgnoreReceipt, error) {
	var receipt operatorTailnetPeerIgnoreReceipt
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return receipt, errors.New("tailnet peer ignore receipt has trailing JSON")
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || string(canonical) != raw {
		return receipt, errors.New("tailnet peer ignore receipt is not canonical JSON")
	}
	return receipt, nil
}

func validateTailnetPeerIgnoreReceipt(receipt operatorTailnetPeerIgnoreReceipt,
	req OperatorTailnetPeerIgnoreRequest, cachedCreatedAt string,
) error {
	if receipt.ReceiptVersion != operatorTailnetPeerIgnoreReceiptVersion ||
		receipt.PeerID != strings.TrimSpace(req.PeerID) || receipt.Hostname != strings.TrimSpace(req.ConfirmHostname) ||
		receipt.Action != req.Action || receipt.Ignored != (req.Action == "ignore") ||
		receipt.Revision <= 0 || fmtTime(receipt.AppliedAt) != cachedCreatedAt {
		return errors.New("tailnet peer ignore receipt fields are invalid")
	}
	if receipt.Action == "ignore" {
		if !receipt.ExpiresAt.Equal(req.ExpiresAt.UTC()) || receipt.Revision != req.ExpectedRevision+1 {
			return errors.New("tailnet ignore receipt does not match request")
		}
	} else if !receipt.PreviousIgnored || !receipt.ExpiresAt.IsZero() || receipt.Revision != req.ExpectedRevision {
		return errors.New("tailnet unignore receipt does not match request")
	}
	return nil
}

func tailnetPeerIgnoreResult(receipt operatorTailnetPeerIgnoreReceipt) OperatorTailnetPeerIgnoreResult {
	return OperatorTailnetPeerIgnoreResult{
		PeerID: receipt.PeerID, Hostname: receipt.Hostname, Action: receipt.Action,
		PreviousIgnored: receipt.PreviousIgnored, Ignored: receipt.Ignored,
		ExpiresAt: receipt.ExpiresAt, Revision: receipt.Revision,
	}
}

func tailnetPeerIgnoreSuccessAuditDetail(receipt operatorTailnetPeerIgnoreReceipt) string {
	raw, _ := json.Marshal(receipt)
	return fmt.Sprintf("Tailnet peer action=%s ignored=%t revision=%d receipt_sha256=%s",
		receipt.Action, receipt.Ignored, receipt.Revision, sha256Hex(string(raw)))
}

func validateTailnetPeerIgnoreAuditEvidence(tx *sql.Tx, req OperatorTailnetPeerIgnoreRequest,
	at string, ok bool, detail string,
) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND COALESCE(reason,'')=? AND idempotency_key=?
	   AND request_digest=? AND at=? AND outcome=? AND COALESCE(detail,'')=?`,
		string(AuditTailnetPeerIgnore), truncAudit(req.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, at, outcomeOf(ok), detail).Scan(&count)
	return count == 1, err
}

func (s *Store) rejectInvalidTailnetPeerIgnoreCache(tx *sql.Tx, audit AuditEntry) (OperatorTailnetPeerIgnoreResult, error) {
	audit.Subject, audit.Reason, audit.OK, audit.Detail = "Tailnet idempotency cache", "", false, operatorTailnetPeerIgnoreCacheInvalid
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorTailnetPeerIgnoreResult{}, fmt.Errorf("store: record invalid tailnet cache audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorTailnetPeerIgnoreResult{}, fmt.Errorf("store: commit invalid tailnet cache audit: %w", err)
	}
	return OperatorTailnetPeerIgnoreResult{Audited: true}, errors.New("store: operator tailnet peer ignore idempotency cache is invalid")
}
