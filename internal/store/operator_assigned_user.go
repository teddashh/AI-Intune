package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AssignedUserNone clears both assignment columns. An empty user id is not
// this action: a missing field must not replay as a cancellation.
const AssignedUserNone = "none"

type OperatorMachineAssignedUserRequest struct {
	MachineID          string
	UserID             string
	UserLogin          string
	ExpectedRevision   *int64
	ConfirmDisplayName string
	IdempotencyKey     string
	RequestDigest      string
	Audit              AuditEntry
}

type OperatorMachineAssignedUserResult struct {
	MachineID         string   `json:"machine_id"`
	DisplayName       string   `json:"display_name"`
	PreviousUserID    string   `json:"previous_user_id"`
	PreviousUserLogin string   `json:"previous_user_login"`
	UserID            string   `json:"user_id"`
	UserLogin         string   `json:"user_login"`
	Revision          int64    `json:"revision"`
	Replayed          bool     `json:"replayed"`
	Audited           bool     `json:"-"`
	ClosedSessionIDs  []string `json:"-"`
}

// ApplyOperatorMachineAssignedUser is the transactional authority for which
// tailnet user a machine is assigned to. Successful results and domain
// rejections commit with their audit row. This store does not read tailnet;
// the operator service supplies the login after it has checked the directory.
func (s *Store) ApplyOperatorMachineAssignedUser(req OperatorMachineAssignedUserRequest) (OperatorMachineAssignedUserResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return OperatorMachineAssignedUserResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略")
	}
	if len(req.IdempotencyKey) > 200 {
		return OperatorMachineAssignedUserResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorMachineAssignedUserResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	operation := "machine-assigned-user:" + req.MachineID
	audit := req.Audit
	audit.Action = AuditMachineAssignedUser
	audit.MachineID = req.MachineID
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	if audit.Subject == "" {
		audit.Subject = req.MachineID
	}
	tx, err := s.beginWrite(context.Background(), "apply_operator_machine_assigned_user")
	if err != nil {
		return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: begin operator machine assigned user: %w", err)
	}
	defer tx.Rollback()

	requestedID, requestedLogin := requestedAssignedUser(req)
	var cached operatorCachedRequest
	err = tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail
	 FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail)
	switch {
	case err == nil:
		if cached.Operation != operation || cached.Digest != req.RequestDigest {
			detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
			populateMachineAssignedUserAudit(tx, &audit, req.MachineID, requestedID, requestedLogin)
			audit.OK, audit.Detail = false, "idempotency conflict："+detail
			if err := s.recordAuditTx(tx, audit); err != nil {
				return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: record operator idempotency conflict audit: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: commit operator idempotency conflict audit: %w", err)
			}
			rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
			rejection.Audited = true
			return OperatorMachineAssignedUserResult{}, rejection
		}
		if cached.Outcome == "rejected" {
			if !cached.ErrorCode.Valid || cached.ErrorCode.String == "" || !cached.ErrorDetail.Valid {
				return OperatorMachineAssignedUserResult{}, errors.New("store: cached operator rejection is incomplete")
			}
			populateMachineAssignedUserAudit(tx, &audit, req.MachineID, requestedID, requestedLogin)
			audit.OK = false
			audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + cached.ErrorDetail.String
			if err := s.recordAuditTx(tx, audit); err != nil {
				return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: record rejected operator replay audit: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: commit rejected operator replay audit: %w", err)
			}
			return OperatorMachineAssignedUserResult{}, &OperatorRequestError{
				Code: cached.ErrorCode.String, Detail: cached.ErrorDetail.String, Replayed: true, Audited: true,
			}
		}
		if cached.Outcome != "ok" || !cached.ResponseJSON.Valid {
			return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: invalid cached operator outcome %q", cached.Outcome)
		}
		var result OperatorMachineAssignedUserResult
		if err := json.Unmarshal([]byte(cached.ResponseJSON.String), &result); err != nil {
			return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: decode cached machine assigned user response: %w", err)
		}
		result.Replayed = true
		audit.Subject = result.DisplayName
		audit.Reason = auditAssignedUserLabel(result.PreviousUserID, result.PreviousUserLogin) + " → " + auditAssignedUserLabel(result.UserID, result.UserLogin)
		audit.OK = true
		audit.Detail = OperatorIdempotencyReplayPrefix + "沒有再次改 state 或 revision"
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: record successful operator replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: commit successful operator replay audit: %w", err)
		}
		result.Audited = true
		return result, nil
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: inspect operator idempotency key: %w", err)
	}

	// A non-none id without a login is not a domain verdict. The operator
	// service fills the login from the directory, but only after a cached
	// decision has had the chance to replay. Persisting this miss would freeze
	// a retry that only lacked the directory.
	if req.UserID != AssignedUserNone && req.UserID != "" && requestedLogin == "" {
		return OperatorMachineAssignedUserResult{}, ErrAssignedUserLoginUnresolved
	}

	reject := func(code, detail string) (OperatorMachineAssignedUserResult, error) {
		audit.OK, audit.Detail = false, detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: record operator rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operation, req.RequestDigest,
			code, detail, fmtTime(s.now())); err != nil {
			return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: persist operator rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: commit operator rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorMachineAssignedUserResult{}, rejection
	}

	var targetID, targetLogin string
	switch req.UserID {
	case AssignedUserNone:
		targetID, targetLogin = "", ""
	case "":
		return reject(OperatorCodeBadAssignedUser, "指派使用者只接受 tailnet 使用者 ID 或 none")
	default:
		if !canonicalAssignedUserID(req.UserID) || requestedLogin == "" || len(requestedLogin) > 320 || strings.ContainsAny(requestedLogin, "\r\n") {
			return reject(OperatorCodeBadAssignedUser, "指派使用者只接受 tailnet 使用者 ID 或 none")
		}
		targetID, targetLogin = req.UserID, requestedLogin
	}
	if req.ExpectedRevision == nil {
		return reject(OperatorCodePreconditionRequired, "expected_revision 不可省略")
	}

	var displayName string
	var currentID, currentLogin, retiredAt sql.NullString
	var revision int64
	if err := tx.QueryRow(`SELECT display_name,assigned_user_id,assigned_user_login,assigned_user_revision,retired_at
	 FROM machine_registry WHERE machine_id=?`, req.MachineID).Scan(
		&displayName, &currentID, &currentLogin, &revision, &retiredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return reject(OperatorCodeMachineNotFound, "找不到 machine "+req.MachineID)
		}
		return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: inspect operator machine assigned user: %w", err)
	}
	audit.Subject = displayName
	audit.Reason = auditAssignedUserLabel(currentID.String, currentLogin.String) + " → " + auditAssignedUserLabel(targetID, targetLogin)
	if retiredAt.Valid {
		return reject(OperatorCodeMachineRetired,
			fmt.Sprintf("%s 已在 %s 退役；先放回分母，才能變更指派使用者", displayName, retiredAt.String))
	}
	if req.ConfirmDisplayName != displayName {
		return reject(OperatorCodeConfirmationMismatch,
			fmt.Sprintf("確認欄位打的是 %q，不是 %q", req.ConfirmDisplayName, displayName))
	}
	if *req.ExpectedRevision != revision {
		return reject(OperatorCodePreconditionFailed,
			fmt.Sprintf("指派 revision 是 %d，不是 request 預期的 %d；請重新讀取機器後再試", revision, *req.ExpectedRevision))
	}

	result := OperatorMachineAssignedUserResult{
		MachineID: req.MachineID, DisplayName: displayName,
		PreviousUserID: currentID.String, PreviousUserLogin: currentLogin.String,
		UserID: targetID, UserLogin: targetLogin, Revision: revision,
	}
	// Same-value requests create no new authority and therefore no new revision.
	if currentID.String != targetID || currentLogin.String != targetLogin {
		res, err := tx.Exec(`UPDATE machine_registry
		 SET assigned_user_id=?,assigned_user_login=?,assigned_user_revision=assigned_user_revision+1
		 WHERE machine_id=? AND assigned_user_revision=?`,
			nullIfEmpty(targetID), nullIfEmpty(targetLogin), req.MachineID, revision)
		if err != nil {
			return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: apply operator machine assigned user: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: operator machine assigned user revision changed during transaction")
		}
		result.Revision++
		if currentID.String != targetID {
			rows, err := tx.Query(`SELECT session_id FROM agent_sessions
				 WHERE machine_id=? AND closed_at IS NULL ORDER BY session_id`, req.MachineID)
			if err != nil {
				return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: list sessions after assigned user change: %w", err)
			}
			for rows.Next() {
				var sessionID string
				if err := rows.Scan(&sessionID); err != nil {
					rows.Close()
					return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: scan session after assigned user change: %w", err)
				}
				result.ClosedSessionIDs = append(result.ClosedSessionIDs, sessionID)
			}
			if err := rows.Close(); err != nil {
				return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: close session rows after assigned user change: %w", err)
			}
			if err := rows.Err(); err != nil {
				return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: list sessions after assigned user change: %w", err)
			}
			if _, err := tx.Exec(`UPDATE agent_sessions
				 SET closed_at=?,close_reason=?
			 WHERE machine_id=? AND closed_at IS NULL`,
				fmtTime(s.now()), AgentSessionCloseReasonAssignedUserChanged, req.MachineID); err != nil {
				return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: close sessions after assigned user change: %w", err)
			}
		}
	}

	raw, err := json.Marshal(result)
	if err != nil {
		return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: encode operator machine assigned user response: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operation, req.RequestDigest, string(raw), fmtTime(s.now())); err != nil {
		return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: persist operator result: %w", err)
	}
	audit.OK, audit.Detail = true, ""
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: record operator success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineAssignedUserResult{}, fmt.Errorf("store: commit operator machine assigned user: %w", err)
	}
	result.Audited = true
	return result, nil
}

func requestedAssignedUser(req OperatorMachineAssignedUserRequest) (id, login string) {
	if req.UserID == AssignedUserNone {
		return "", ""
	}
	return req.UserID, strings.TrimSpace(req.UserLogin)
}

func canonicalAssignedUserID(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return false
	}
	return strconv.FormatInt(n, 10) == id
}

func populateMachineAssignedUserAudit(tx dbTx, audit *AuditEntry, machineID, targetID, targetLogin string) {
	var display string
	var id, login sql.NullString
	if err := tx.QueryRow(`SELECT display_name,assigned_user_id,assigned_user_login FROM machine_registry WHERE machine_id=?`, machineID).
		Scan(&display, &id, &login); err != nil {
		return
	}
	audit.Subject = display
	audit.Reason = auditAssignedUserLabel(id.String, login.String) + " → " + auditAssignedUserLabel(targetID, targetLogin)
}

func auditAssignedUserLabel(id, login string) string {
	if login != "" {
		return login
	}
	if id != "" {
		return id
	}
	return "未指派"
}
