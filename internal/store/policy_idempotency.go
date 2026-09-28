package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Publish-and-assign planes all have the same replay semantics, so the
// idempotency dance lives here once instead of once per plane. It behaves
// exactly like the hand-written copies in the older endpoints: a reused key
// with different material is a conflict, a cached rejection replays as a
// rejection, and a cached success replays without touching state — and every
// one of those three outcomes writes its own audit row, because pressing the
// button is a thing that happened even when nothing changed.

func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// policyIdempotency inspects the key. It returns (replayed result, rejection,
// error); at most one is non-nil, and all-nil means the caller should proceed.
func (s *Store) policyIdempotency(tx *sql.Tx, key, operation, requestDigest string,
	audit *AuditEntry, decode func(string) (any, error), replayDetail string) (any, error, error) {

	var cached operatorCachedRequest
	err := tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail
	 FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("store: inspect policy idempotency key: %w", err)
	}

	if cached.Operation != operation || cached.Digest != requestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK, audit.Detail = false, "idempotency conflict："+detail
		if err := s.recordAuditTx(tx, *audit); err != nil {
			return nil, nil, fmt.Errorf("store: record policy idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("store: commit policy idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return nil, rejection, nil
	}

	if cached.Outcome == "rejected" {
		if !cached.ErrorCode.Valid || cached.ErrorCode.String == "" || !cached.ErrorDetail.Valid {
			return nil, nil, errors.New("store: cached policy rejection is incomplete")
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + cached.ErrorDetail.String
		if err := s.recordAuditTx(tx, *audit); err != nil {
			return nil, nil, fmt.Errorf("store: record rejected policy replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("store: commit rejected policy replay audit: %w", err)
		}
		return nil, &OperatorRequestError{Code: cached.ErrorCode.String,
			Detail: cached.ErrorDetail.String, Replayed: true, Audited: true}, nil
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid {
		return nil, nil, fmt.Errorf("store: invalid cached policy outcome %q", cached.Outcome)
	}

	result, err := decode(cached.ResponseJSON.String)
	if err != nil {
		return nil, nil, fmt.Errorf("store: decode cached policy response: %w", err)
	}
	audit.OK, audit.Detail = true, OperatorIdempotencyReplayPrefix+replayDetail
	if err := s.recordAuditTx(tx, *audit); err != nil {
		return nil, nil, fmt.Errorf("store: record successful policy replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("store: commit successful policy replay audit: %w", err)
	}
	return result, nil, nil
}

// policyReject persists a domain rejection so a retry gets the same verdict
// instead of re-running once the fleet has changed underneath it.
func (s *Store) policyReject(tx *sql.Tx, key, operation, requestDigest string,
	audit *AuditEntry, code, detail string) error {
	audit.OK, audit.Detail = false, detail
	if err := s.recordAuditTx(tx, *audit); err != nil {
		return fmt.Errorf("store: record policy rejection audit: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
	 VALUES (?,?,?,'rejected',?,?,?)`, key, operation, requestDigest, code, detail,
		fmtTime(s.now())); err != nil {
		return fmt.Errorf("store: persist policy rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit policy rejection: %w", err)
	}
	rejection := operatorError(code, detail)
	rejection.Audited = true
	return rejection
}

func (s *Store) policyCommit(tx *sql.Tx, key, operation, requestDigest string,
	audit *AuditEntry, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("store: encode policy response: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, key, operation, requestDigest, string(raw),
		fmtTime(s.now())); err != nil {
		return fmt.Errorf("store: persist policy result: %w", err)
	}
	audit.OK, audit.Detail = true, ""
	if err := s.recordAuditTx(tx, *audit); err != nil {
		return fmt.Errorf("store: record policy success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit policy write: %w", err)
	}
	return nil
}
