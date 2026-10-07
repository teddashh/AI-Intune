package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/sessionid"
)

const (
	OperatorCodeAgentSessionMachineNotFound      = "AGENT_SESSION_MACHINE_NOT_FOUND"
	OperatorCodeAgentSessionMachineRetired       = "AGENT_SESSION_MACHINE_RETIRED"
	OperatorCodeAgentSessionMachineUnassigned    = "AGENT_SESSION_MACHINE_UNASSIGNED"
	OperatorCodeAgentSessionOperatorUserRequired = "AGENT_SESSION_OPERATOR_USER_REQUIRED"
	OperatorCodeAgentSessionAssignedUserMismatch = "AGENT_SESSION_ASSIGNED_USER_MISMATCH"
	OperatorCodeAgentSessionInvalid              = "AGENT_SESSION_INVALID"
	OperatorCodeAgentSessionExists               = "AGENT_SESSION_EXISTS"
	OperatorCodeAgentSessionLimitReached         = "AGENT_SESSION_LIMIT_REACHED"
	OperatorCodeAgentSessionNotFound             = "AGENT_SESSION_NOT_FOUND"
	OperatorCodeAgentSessionClosed               = "AGENT_SESSION_CLOSED"
	OperatorCodeAgentSessionCloseReasonRequired  = "AGENT_SESSION_CLOSE_REASON_REQUIRED"
	AgentSessionCloseReasonAssignedUserChanged   = "機器指派使用者已變更"
	AgentSessionCloseReasonMachineRetired        = "機器已退役"
	AgentSessionCloseReasonHubRestart            = "Hub 已重新啟動"
	AgentSessionCloseReasonAgentConnectionEnded  = "Agent 連線已結束"
	AgentSessionCloseReasonNeverAttached         = "終端開啟後沒有連上"
	AgentSessionCloseReasonViewerClosed          = "終端頁面已關閉或斷線"
	AgentSessionCloseReasonViewerInvalid         = "終端頁面送出無效的內容"
	AgentSessionCloseReasonViewerTooSlow         = "終端頁面接收輸出太慢"
	AgentSessionCloseReasonExited                = "終端程式已結束"
	AgentSessionCloseReasonAccessRevoked         = "終端存取權已撤銷"
	AgentSessionCloseReasonAuthUnavailable       = "無法確認操作者身分"
	AgentSessionCloseReasonMachineError          = "機器端終端發生錯誤"
	AgentSessionCloseReasonTerminalNotLinked     = "開啟時終端連線沒有接上 Hub"
	AgentSessionCloseReasonIdleTimeout           = "終端閒置逾時，已自動關閉"
	AgentSessionCloseReasonLifetimeReached       = "終端已達最長使用時間，已自動關閉"
	// MaxOpenAgentSessionsPerMachine bounds the live PTYs, agent workers, and
	// output queues one machine carries. Four lets an operator keep a second
	// terminal open beside the one they are working in, and still leaves room
	// for another operator-driven session on the same machine.
	MaxOpenAgentSessionsPerMachine  = 4
	agentSessionOpenReplayDetail    = "session 仍開著，沒有建立第二列"
	agentSessionCloseReplayDetail   = "沒有改寫第一次的關閉時間或原因"
	agentSessionInvalidAuditSubject = "無效的 session 識別碼"
	agentSessionInvalidAuditReason  = "session 識別碼未通過驗證"
)

func closeOpenAgentSessionsForMachine(tx dbTx, machineID string, closedAt time.Time, reason string) ([]string, error) {
	rows, err := tx.Query(`SELECT session_id FROM agent_sessions
		 WHERE machine_id=? AND closed_at IS NULL ORDER BY session_id`, machineID)
	if err != nil {
		return nil, err
	}
	var sessionIDs []string
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			rows.Close()
			return nil, err
		}
		sessionIDs = append(sessionIDs, sessionID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE agent_sessions SET closed_at=?,close_reason=?
		 WHERE machine_id=? AND closed_at IS NULL`, fmtTime(closedAt), reason, machineID); err != nil {
		return nil, err
	}
	return sessionIDs, nil
}

// CloseAllOpenAgentSessions records the first close for every open session.
func (s *Store) CloseAllOpenAgentSessions(reason string) (int, error) {
	if !validSingleLineText(reason, auditMaxReason, false) {
		return 0, ErrAgentSessionCloseReasonRequired
	}
	result, err := s.execWrite(context.Background(), "close_all_open_agent_sessions", `UPDATE agent_sessions SET closed_at=?,close_reason=?
		 WHERE closed_at IS NULL`, fmtTime(s.now().UTC().Truncate(time.Second)), reason)
	if err != nil {
		return 0, fmt.Errorf("store: close all open agent sessions: %w", err)
	}
	closed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count closed agent sessions: %w", err)
	}
	return int(closed), nil
}

// CloseUnattachedAgentSessions records the first close for open sessions whose
// opened_at is at or before openedAtOrBefore, excluding attached. It returns
// how many rows it closed.
func (s *Store) CloseUnattachedAgentSessions(attached []string, openedAtOrBefore time.Time, reason string) (int, error) {
	if !validSingleLineText(reason, auditMaxReason, false) {
		return 0, ErrAgentSessionCloseReasonRequired
	}
	// opened_at is TEXT from fmtTime: RFC3339, UTC, whole seconds, and the
	// comparison is lexicographic. The cutoff is spelled by the same function.
	// Another spelling of the same instant, with a fraction or a zone offset,
	// sorts on the wrong side of the stored second.
	args := make([]any, 0, len(attached)+3)
	args = append(args, fmtTime(s.now().UTC().Truncate(time.Second)), reason, fmtTime(openedAtOrBefore))
	query := `UPDATE agent_sessions SET closed_at=?,close_reason=?
		 WHERE closed_at IS NULL AND opened_at <= ?`
	if len(attached) != 0 {
		placeholders := make([]string, len(attached))
		for i, sessionID := range attached {
			placeholders[i] = "?"
			args = append(args, sessionID)
		}
		query += ` AND session_id NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	result, err := s.execWrite(context.Background(), "close_unattached_agent_sessions", query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: close unattached agent sessions: %w", err)
	}
	closed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count unattached agent sessions closed: %w", err)
	}
	return int(closed), nil
}

// CloseAgentSessionsByID records the first close for the listed open sessions.
func (s *Store) CloseAgentSessionsByID(sessionIDs []string, reason string) (int, error) {
	if len(sessionIDs) == 0 {
		return 0, nil
	}
	if !validSingleLineText(reason, auditMaxReason, false) {
		return 0, ErrAgentSessionCloseReasonRequired
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, 0, len(sessionIDs)+2)
	args = append(args, fmtTime(s.now().UTC().Truncate(time.Second)), reason)
	for i, sessionID := range sessionIDs {
		placeholders[i] = "?"
		args = append(args, sessionID)
	}
	result, err := s.execWrite(context.Background(), "close_agent_sessions_by_i_d", `UPDATE agent_sessions SET closed_at=?,close_reason=?
		 WHERE closed_at IS NULL AND session_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("store: close agent sessions by ID: %w", err)
	}
	closed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: count agent sessions closed by ID: %w", err)
	}
	return int(closed), nil
}

// RetiredMachineIDs returns the listed machine IDs that are currently retired.
func (s *Store) RetiredMachineIDs(machineIDs []string) ([]string, error) {
	if len(machineIDs) == 0 {
		return []string{}, nil
	}
	placeholders := make([]string, len(machineIDs))
	args := make([]any, len(machineIDs))
	for i, machineID := range machineIDs {
		placeholders[i] = "?"
		args[i] = machineID
	}
	rows, err := s.rdb.Query(`SELECT machine_id FROM machine_registry
		 WHERE retired_at IS NOT NULL AND machine_id IN (`+strings.Join(placeholders, ",")+`)
		 ORDER BY machine_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: find retired machines: %w", err)
	}
	defer rows.Close()
	retired := make([]string, 0)
	for rows.Next() {
		var machineID string
		if err := rows.Scan(&machineID); err != nil {
			return nil, fmt.Errorf("store: scan retired machine: %w", err)
		}
		retired = append(retired, machineID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read retired machines: %w", err)
	}
	return retired, nil
}

// UnauthorizedAgentSessionIDs returns listed session IDs that are closed or
// absent. A missing row is deliberately unauthorized: callers must commit the
// authorizing row before registering a live session.
func (s *Store) UnauthorizedAgentSessionIDs(sessionIDs []string) ([]string, error) {
	if len(sessionIDs) == 0 {
		return []string{}, nil
	}
	unique := make(map[string]struct{}, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		unique[sessionID] = struct{}{}
	}
	listed := make([]string, 0, len(unique))
	for sessionID := range unique {
		listed = append(listed, sessionID)
	}
	sort.Strings(listed)

	placeholders := make([]string, len(listed))
	args := make([]any, len(listed))
	for i, sessionID := range listed {
		placeholders[i] = "?"
		args[i] = sessionID
	}
	rows, err := s.rdb.Query(`SELECT session_id FROM agent_sessions
		 WHERE closed_at IS NULL AND session_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: find authorized agent sessions: %w", err)
	}
	defer rows.Close()
	authorized := make(map[string]struct{}, len(listed))
	for rows.Next() {
		var sessionID string
		if err := rows.Scan(&sessionID); err != nil {
			return nil, fmt.Errorf("store: scan authorized agent session: %w", err)
		}
		authorized[sessionID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read authorized agent sessions: %w", err)
	}
	unauthorized := make([]string, 0, len(listed)-len(authorized))
	for _, sessionID := range listed {
		if _, ok := authorized[sessionID]; !ok {
			unauthorized = append(unauthorized, sessionID)
		}
	}
	return unauthorized, nil
}

var (
	ErrAgentSessionMachineUnassigned    = errors.New("store: agent session machine has no assigned user")
	ErrAgentSessionOperatorUserRequired = errors.New("store: agent session operator tailnet user id is required")
	ErrAgentSessionAssignedUserMismatch = errors.New("store: agent session operator is not the assigned user")
	ErrAgentSessionInvalid              = errors.New("store: agent session request is invalid")
	ErrAgentSessionExists               = errors.New("store: agent session already exists")
	ErrAgentSessionLimitReached         = errors.New("store: agent session open limit reached")
	ErrAgentSessionNotFound             = errors.New("store: agent session was not found")
	ErrAgentSessionClosed               = errors.New("store: agent session is closed")
	ErrAgentSessionCloseReasonRequired  = errors.New("store: agent session close reason is required")
)

type OpenAgentSessionRequest struct {
	SessionID                string
	MachineID                string
	OperatorTailnetUserID    string
	OperatorTailnetUserLogin string
	IdempotencyKey           string
	RequestDigest            string
	Audit                    AuditEntry
}

type CloseAgentSessionRequest struct {
	SessionID                string
	MachineID                string
	OperatorTailnetUserID    string
	OperatorTailnetUserLogin string
	Reason                   string
	IdempotencyKey           string
	RequestDigest            string
	Audit                    AuditEntry
}

type AgentSessionResult struct {
	SessionID                string     `json:"session_id"`
	MachineID                string     `json:"machine_id"`
	OperatorTailnetUserID    string     `json:"operator_tailnet_user_id"`
	OperatorTailnetUserLogin string     `json:"operator_tailnet_user_login"`
	OpenedAt                 time.Time  `json:"opened_at"`
	ClosedAt                 *time.Time `json:"closed_at,omitempty"`
	CloseReason              string     `json:"close_reason,omitempty"`
	Replayed                 bool       `json:"replayed"`
	Audited                  bool       `json:"-"`
}

// OpenAgentSession is the transactional authorization boundary for starting
// an agent session. The registry assignment is read and the session row is
// inserted in the same database transaction.
func (s *Store) OpenAgentSession(req OpenAgentSessionRequest) (AgentSessionResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return AgentSessionResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略")
	}
	if len(req.IdempotencyKey) > 200 {
		return AgentSessionResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return AgentSessionResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	operation := "agent-session-open:" + req.SessionID
	audit := agentSessionAudit(req.Audit, AuditAgentSessionOpen, req.MachineID,
		req.SessionID, req.OperatorTailnetUserID, req.OperatorTailnetUserLogin,
		req.IdempotencyKey, req.RequestDigest)
	tx, err := s.beginWrite(context.Background(), "open_agent_session")
	if err != nil {
		return AgentSessionResult{}, fmt.Errorf("store: begin agent session open: %w", err)
	}
	defer tx.Rollback()
	writerNow := s.now().UTC().Truncate(time.Second)
	audit.At = writerNow

	cached, err := s.agentSessionOpenIdempotency(tx, req, operation, &audit)
	if err != nil {
		return AgentSessionResult{}, err
	}

	var displayName string
	var assignedUserID, retiredAt sql.NullString
	if err := tx.QueryRow(`SELECT display_name,assigned_user_id,retired_at
	 FROM machine_registry WHERE machine_id=?`, req.MachineID).Scan(
		&displayName, &assignedUserID, &retiredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, cached,
				OperatorCodeAgentSessionMachineNotFound, "找不到 machine "+req.MachineID)
		}
		return AgentSessionResult{}, fmt.Errorf("store: inspect agent session machine: %w", err)
	}
	if validAgentSessionID(req.SessionID) {
		audit.Subject = displayName
	}
	if retiredAt.Valid {
		return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, cached,
			OperatorCodeAgentSessionMachineRetired, displayName+" 已退役，不能開啟 session")
	}
	if assignedUserID.String == "" {
		return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, cached,
			OperatorCodeAgentSessionMachineUnassigned, displayName+" 沒有指派使用者，不能開啟 session")
	}
	if req.OperatorTailnetUserID == "" {
		return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, cached,
			OperatorCodeAgentSessionOperatorUserRequired, "operator tailnet 使用者 ID 不可省略")
	}
	if req.OperatorTailnetUserID != assignedUserID.String {
		return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, cached,
			OperatorCodeAgentSessionAssignedUserMismatch, "這台機器只允許指派使用者開啟 session")
	}
	if !validAgentSessionID(req.SessionID) || len(req.OperatorTailnetUserLogin) > 320 ||
		strings.ContainsAny(req.OperatorTailnetUserLogin, "\r\n") {
		return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, cached,
			OperatorCodeAgentSessionInvalid, "session 識別碼或 operator login 無效")
	}

	if cached != nil {
		live, err := agentSessionByID(tx, req.SessionID, req.MachineID)
		if errors.Is(err, ErrAgentSessionNotFound) {
			return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, cached,
				OperatorCodeAgentSessionClosed, "原 session 已不存在，不能重新開啟")
		}
		if err != nil {
			return AgentSessionResult{}, err
		}
		if live.ClosedAt != nil {
			return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, cached,
				OperatorCodeAgentSessionClosed, "原 session 已關閉，不能重新開啟")
		}
		if live.OperatorTailnetUserID != req.OperatorTailnetUserID ||
			cached.OperatorTailnetUserID != req.OperatorTailnetUserID ||
			cached.SessionID != req.SessionID || cached.MachineID != req.MachineID ||
			cached.OpenedAt.IsZero() || cached.ClosedAt != nil || cached.Replayed {
			return AgentSessionResult{}, errors.New("store: cached agent session open receipt is invalid")
		}
		audit.OK = true
		audit.Detail = OperatorIdempotencyReplayPrefix + agentSessionOpenReplayDetail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return AgentSessionResult{}, fmt.Errorf("store: record agent session open replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return AgentSessionResult{}, fmt.Errorf("store: commit agent session open replay audit: %w", err)
		}
		live.Replayed, live.Audited = true, true
		return live, nil
	}

	if _, err := agentSessionByID(tx, req.SessionID, ""); err == nil {
		return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, nil,
			OperatorCodeAgentSessionExists, "session 識別碼已存在")
	} else if !errors.Is(err, ErrAgentSessionNotFound) {
		return AgentSessionResult{}, err
	}
	// Count only after the replay above has returned. That replay gives back
	// the live session already holding its slot; checking the cap first would
	// reject the second delivery of the open that occupies the last place.
	//
	// The duplicate-ID check stays ahead of this count. A session ID is a
	// primary key that closed rows keep, so freeing a slot cannot make a
	// reused ID insertable, and that request must keep reporting
	// AGENT_SESSION_EXISTS.
	openCount, err := openAgentSessionCount(tx, req.MachineID)
	if err != nil {
		return AgentSessionResult{}, fmt.Errorf("store: count open agent sessions: %w", err)
	}
	if openCount >= MaxOpenAgentSessionsPerMachine {
		return AgentSessionResult{}, s.rejectAgentSessionOpen(tx, req, operation, &audit, nil,
			OperatorCodeAgentSessionLimitReached, agentSessionLimitReachedDetail(openCount))
	}
	result := AgentSessionResult{
		SessionID: req.SessionID, MachineID: req.MachineID,
		OperatorTailnetUserID:    req.OperatorTailnetUserID,
		OperatorTailnetUserLogin: req.OperatorTailnetUserLogin,
		OpenedAt:                 writerNow,
	}
	if _, err := tx.Exec(`INSERT INTO agent_sessions
	 (session_id,machine_id,operator_tailnet_user_id,operator_tailnet_user_login,opened_at)
	 VALUES (?,?,?,?,?)`, result.SessionID, result.MachineID, result.OperatorTailnetUserID,
		result.OperatorTailnetUserLogin, fmtTime(result.OpenedAt)); err != nil {
		return AgentSessionResult{}, fmt.Errorf("store: insert agent session: %w", err)
	}
	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, result); err != nil {
		return AgentSessionResult{}, err
	}
	result.Audited = true
	return result, nil
}

// CloseAgentSession closes an open session. A session already closed is a
// successful no-op and retains its first close timestamp and reason.
func (s *Store) CloseAgentSession(req CloseAgentSessionRequest) (AgentSessionResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return AgentSessionResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略")
	}
	if len(req.IdempotencyKey) > 200 {
		return AgentSessionResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return AgentSessionResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	operation := "agent-session-close:" + req.SessionID
	audit := agentSessionAudit(req.Audit, AuditAgentSessionClose, req.MachineID,
		req.SessionID, req.OperatorTailnetUserID, req.OperatorTailnetUserLogin,
		req.IdempotencyKey, req.RequestDigest)
	sessionIDValid := validAgentSessionID(req.SessionID)
	reasonValid := validSingleLineText(req.Reason, auditMaxReason, false)
	if sessionIDValid && reasonValid {
		audit.Reason = req.Reason
	}
	tx, err := s.beginWrite(context.Background(), "close_agent_session")
	if err != nil {
		return AgentSessionResult{}, fmt.Errorf("store: begin agent session close: %w", err)
	}
	defer tx.Rollback()
	writerNow := s.now().UTC().Truncate(time.Second)
	audit.At = writerNow

	replayed, rejection, err := s.policyIdempotency(tx, req.IdempotencyKey, operation,
		req.RequestDigest, &audit, func(raw string) (any, error) {
			var result AgentSessionResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return nil, err
			}
			if result.SessionID != req.SessionID || result.MachineID != req.MachineID ||
				result.OpenedAt.IsZero() || result.ClosedAt == nil || result.CloseReason == "" || result.Replayed {
				return nil, errors.New("cached agent session close receipt is invalid")
			}
			return result, nil
		}, agentSessionCloseReplayDetail)
	if err != nil {
		return AgentSessionResult{}, err
	}
	if rejection != nil {
		return AgentSessionResult{}, rejection
	}
	if replayed != nil {
		result := replayed.(AgentSessionResult)
		result.Replayed, result.Audited = true, true
		return result, nil
	}

	reject := func(code, detail string) (AgentSessionResult, error) {
		return AgentSessionResult{}, s.policyReject(tx, req.IdempotencyKey, operation,
			req.RequestDigest, &audit, code, detail)
	}
	if req.OperatorTailnetUserID == "" {
		return reject(OperatorCodeAgentSessionOperatorUserRequired, "operator tailnet 使用者 ID 不可省略")
	}
	if !sessionIDValid || req.MachineID == "" {
		return reject(OperatorCodeAgentSessionInvalid, "session 識別碼與 machine ID 不可省略")
	}
	if !reasonValid {
		return reject(OperatorCodeAgentSessionCloseReasonRequired, "關閉原因必須是單行純文字、前後不可有空白，且最多 500 bytes")
	}

	result, err := agentSessionByID(tx, req.SessionID, req.MachineID)
	if errors.Is(err, ErrAgentSessionNotFound) {
		return reject(OperatorCodeAgentSessionNotFound, "找不到這個 machine 的 session")
	}
	if err != nil {
		return AgentSessionResult{}, err
	}
	if result.ClosedAt == nil {
		closedAt := writerNow
		updated, err := tx.Exec(`UPDATE agent_sessions SET closed_at=?,close_reason=?
		 WHERE session_id=? AND machine_id=? AND closed_at IS NULL`,
			fmtTime(closedAt), req.Reason, req.SessionID, req.MachineID)
		if err != nil {
			return AgentSessionResult{}, fmt.Errorf("store: close agent session: %w", err)
		}
		if n, _ := updated.RowsAffected(); n != 1 {
			return AgentSessionResult{}, fmt.Errorf("store: agent session changed during close")
		}
		result.ClosedAt, result.CloseReason = &closedAt, req.Reason
	}
	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, result); err != nil {
		return AgentSessionResult{}, err
	}
	result.Audited = true
	return result, nil
}

// ListOpenAgentSessionsForMachine returns only sessions whose first close has
// not been recorded.
func (s *Store) ListOpenAgentSessionsForMachine(machineID string) ([]AgentSessionResult, error) {
	var exists bool
	if err := s.rdb.QueryRow(`SELECT EXISTS(SELECT 1 FROM machine_registry WHERE machine_id=?)`, machineID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("store: inspect machine for open sessions: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.rdb.Query(`SELECT session_id,machine_id,operator_tailnet_user_id,
	 operator_tailnet_user_login,opened_at,closed_at,close_reason
	 FROM agent_sessions WHERE machine_id=? AND closed_at IS NULL
	 ORDER BY opened_at,session_id`, machineID)
	if err != nil {
		return nil, fmt.Errorf("store: list open agent sessions: %w", err)
	}
	defer rows.Close()
	var result []AgentSessionResult
	for rows.Next() {
		session, err := scanAgentSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list open agent sessions: %w", err)
	}
	return result, nil
}

// AgentSessionForOperator returns the open session when it belongs to
// machineID and the operator's stable tailnet user id.
//
// Every miss is ErrNotFound. The caller renders one not-found answer, so this
// read must not say whether the machine, the session, the closed state, or
// the operator id was the miss. Login is not an argument: a matching login
// with a different stable id is a different operator.
func (s *Store) AgentSessionForOperator(machineID, sessionID, operatorTailnetUserID string) (AgentSessionResult, error) {
	if machineID == "" || operatorTailnetUserID == "" || !validAgentSessionID(sessionID) {
		return AgentSessionResult{}, ErrNotFound
	}
	var exists bool
	if err := s.rdb.QueryRow(`SELECT EXISTS(SELECT 1 FROM machine_registry WHERE machine_id=?)`, machineID).Scan(&exists); err != nil {
		return AgentSessionResult{}, fmt.Errorf("store: inspect machine for agent session: %w", err)
	}
	if !exists {
		return AgentSessionResult{}, ErrNotFound
	}
	result, err := agentSessionByID(s.rdb, sessionID, machineID)
	if errors.Is(err, ErrAgentSessionNotFound) {
		return AgentSessionResult{}, ErrNotFound
	}
	if err != nil {
		return AgentSessionResult{}, err
	}
	if result.ClosedAt != nil || result.OperatorTailnetUserID != operatorTailnetUserID {
		return AgentSessionResult{}, ErrNotFound
	}
	return result, nil
}

func (s *Store) agentSessionOpenIdempotency(tx dbTx, req OpenAgentSessionRequest,
	operation string, audit *AuditEntry,
) (*AgentSessionResult, error) {
	var cached operatorCachedRequest
	err := tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail
	 FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: inspect agent session open idempotency key: %w", err)
	}
	if cached.Operation != operation || cached.Digest != req.RequestDigest {
		detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
		audit.OK, audit.Detail = false, "idempotency conflict："+detail
		if err := s.recordAuditTx(tx, *audit); err != nil {
			return nil, fmt.Errorf("store: record agent session idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: commit agent session idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
		rejection.Audited = true
		return nil, rejection
	}
	if cached.Outcome == "rejected" {
		if !cached.ErrorCode.Valid || cached.ErrorCode.String == "" || !cached.ErrorDetail.Valid {
			return nil, errors.New("store: cached agent session rejection is incomplete")
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + cached.ErrorDetail.String
		if err := s.recordAuditTx(tx, *audit); err != nil {
			return nil, fmt.Errorf("store: record rejected agent session replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("store: commit rejected agent session replay audit: %w", err)
		}
		return nil, &OperatorRequestError{Code: cached.ErrorCode.String,
			Detail: cached.ErrorDetail.String, Replayed: true, Audited: true}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid {
		return nil, fmt.Errorf("store: invalid cached agent session outcome %q", cached.Outcome)
	}
	var result AgentSessionResult
	if err := json.Unmarshal([]byte(cached.ResponseJSON.String), &result); err != nil {
		return nil, fmt.Errorf("store: decode cached agent session open response: %w", err)
	}
	return &result, nil
}

func agentSessionLimitReachedDetail(openCount int) string {
	return fmt.Sprintf("這台機器已有 %d 個開啟中的 session，上限 %d 個；請先關閉一個，再開新的 session",
		openCount, MaxOpenAgentSessionsPerMachine)
}

func (s *Store) rejectAgentSessionOpen(tx dbTx, req OpenAgentSessionRequest, operation string,
	audit *AuditEntry, cached *AgentSessionResult, code, detail string,
) error {
	if cached == nil {
		return s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, audit, code, detail)
	}
	audit.OK = false
	audit.Detail = OperatorIdempotencyReplayPrefix + "目前判決：" + detail
	if err := s.recordAuditTx(tx, *audit); err != nil {
		return fmt.Errorf("store: record invalidated agent session replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit invalidated agent session replay audit: %w", err)
	}
	rejection := operatorError(code, detail)
	rejection.Replayed, rejection.Audited = true, true
	return rejection
}

func agentSessionAudit(audit AuditEntry, action AuditAction, machineID, sessionID,
	operatorUserID, operatorLogin, idempotencyKey, requestDigest string,
) AuditEntry {
	audit.Action = action
	audit.MachineID = machineID
	if validAgentSessionID(sessionID) {
		audit.Subject = machineID
		if action == AuditAgentSessionClose {
			audit.Subject = sessionID
		}
		audit.Reason = "session " + sessionID
	} else {
		audit.Subject = agentSessionInvalidAuditSubject
		audit.Reason = agentSessionInvalidAuditReason
	}
	audit.IdempotencyKey = idempotencyKey
	audit.RequestDigest = requestDigest
	audit.WhoUser = operatorLogin
	audit.AuthSubject = ""
	if operatorUserID != "" {
		audit.AuthSubject = "tailscale-user:" + operatorUserID
	}
	return audit
}

func validAgentSessionID(sessionID string) bool {
	return sessionid.Valid(sessionID) && sessionID == strings.TrimSpace(sessionID)
}

type agentSessionScanner interface {
	Scan(dest ...any) error
}

func scanAgentSession(scanner agentSessionScanner) (AgentSessionResult, error) {
	var result AgentSessionResult
	var openedAt string
	var closedAt, closeReason sql.NullString
	if err := scanner.Scan(&result.SessionID, &result.MachineID, &result.OperatorTailnetUserID,
		&result.OperatorTailnetUserLogin, &openedAt, &closedAt, &closeReason); err != nil {
		return AgentSessionResult{}, err
	}
	result.OpenedAt = parseTime(openedAt)
	result.ClosedAt = parseTimePtr(closedAt)
	result.CloseReason = closeReason.String
	if result.SessionID == "" || result.MachineID == "" || result.OperatorTailnetUserID == "" ||
		result.OpenedAt.IsZero() || (result.ClosedAt == nil) != (result.CloseReason == "") {
		return AgentSessionResult{}, errors.New("store: agent session row is invalid")
	}
	return result, nil
}

func openAgentSessionCount(q interface{ QueryRow(string, ...any) *sql.Row }, machineID string) (int, error) {
	var openCount int
	err := q.QueryRow(`SELECT COUNT(*) FROM agent_sessions
		 WHERE machine_id=? AND closed_at IS NULL`, machineID).Scan(&openCount)
	return openCount, err
}

// OpenAgentSessionCount is the count OpenAgentSession compares with
// MaxOpenAgentSessionsPerMachine, read outside the write transaction, so the
// write re-checks it.
func (s *Store) OpenAgentSessionCount(machineID string) (int, error) {
	count, err := openAgentSessionCount(s.rdb, machineID)
	if err != nil {
		return 0, fmt.Errorf("store: count open agent sessions: %w", err)
	}
	return count, nil
}

func agentSessionByID(q interface {
	QueryRow(query string, args ...any) *sql.Row
}, sessionID, machineID string,
) (AgentSessionResult, error) {
	query := `SELECT session_id,machine_id,operator_tailnet_user_id,
	 operator_tailnet_user_login,opened_at,closed_at,close_reason
	 FROM agent_sessions WHERE session_id=?`
	args := []any{sessionID}
	if machineID != "" {
		query += ` AND machine_id=?`
		args = append(args, machineID)
	}
	result, err := scanAgentSession(q.QueryRow(query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentSessionResult{}, ErrAgentSessionNotFound
	}
	if err != nil {
		return AgentSessionResult{}, fmt.Errorf("store: read agent session: %w", err)
	}
	return result, nil
}
