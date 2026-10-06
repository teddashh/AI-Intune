package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/sessionid"
)

func TestAgentSessionIDAcceptedByStoreIsAlwaysAcceptedByWire(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "empty", id: ""},
		{name: "valid ASCII", id: "session-1"},
		{name: "valid multi-byte", id: "終端-session-α"},
		{name: "128 bytes", id: strings.Repeat("s", sessionid.MaxLength)},
		{name: "129 bytes", id: strings.Repeat("s", sessionid.MaxLength+1)},
		{name: "leading space", id: " session"},
		{name: "trailing space", id: "session "},
		{name: "invalid UTF-8", id: string([]byte{'s', 0xff})},
		{name: "NUL", id: "session\x00id"},
		{name: "BEL", id: "session\aid"},
		{name: "CR", id: "session\rid"},
		{name: "LF", id: "session\nid"},
		{name: "DEL", id: "session\x7fid"},
		{name: "line separator", id: "session\u2028id"},
		{name: "paragraph separator", id: "session\u2029id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storeAccepts := validAgentSessionID(tt.id)
			_, err := agentrelay.EncodeDownstream(agentrelay.Downstream{
				Type: agentrelay.DownstreamClose, Session: tt.id,
			})
			wireAccepts := err == nil
			if storeAccepts && !wireAccepts {
				t.Fatalf("store accepted %q but wire rejected it: %v", tt.id, err)
			}
		})
	}
}

func TestOpenAndCloseAgentSessionReject129ByteSessionID(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	sessionID := strings.Repeat("s", sessionid.MaxLength+1)

	_, err := st.OpenAgentSession(openAgentSessionRequest(
		sessionID, machineID, "u-1", "ted@example.com", "open-too-long"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionInvalid)

	_, err = st.CloseAgentSession(closeAgentSessionRequest(
		sessionID, machineID, "u-1", "ted@example.com", "operator 離開終端", "close-too-long"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionInvalid)

	if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 0 {
		t.Fatalf("rejected requests wrote %d sessions", got)
	}
}

func TestInvalidAgentSessionIDAuditUsesFixedText(t *testing.T) {
	invalidSessionID := "sess\x1b[2Jion"
	tests := []struct {
		name   string
		action AuditAction
		key    string
		run    func(*Store, string) error
	}{
		{
			name: "open", action: AuditAgentSessionOpen, key: "invalid-session-open",
			run: func(st *Store, machineID string) error {
				_, err := st.OpenAgentSession(openAgentSessionRequest(
					invalidSessionID, machineID, "u-1", "ted@example.com", "invalid-session-open"))
				return err
			},
		},
		{
			name: "close with valid reason", action: AuditAgentSessionClose, key: "invalid-session-close-valid-reason",
			run: func(st *Store, machineID string) error {
				_, err := st.CloseAgentSession(closeAgentSessionRequest(
					invalidSessionID, machineID, "u-1", "ted@example.com", "operator 離開終端",
					"invalid-session-close-valid-reason"))
				return err
			},
		},
		{
			name: "close with invalid reason", action: AuditAgentSessionClose, key: "invalid-session-close-invalid-reason",
			run: func(st *Store, machineID string) error {
				_, err := st.CloseAgentSession(closeAgentSessionRequest(
					invalidSessionID, machineID, "u-1", "ted@example.com", "reason\x1b[2J",
					"invalid-session-close-invalid-reason"))
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newTestStore(t)
			machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
			assertAgentSessionRejection(t, tt.run(st, machineID), OperatorCodeAgentSessionInvalid)

			var subject, reason, outcome string
			if err := st.DB().QueryRow(`SELECT subject,reason,outcome FROM audit_log
			 WHERE action=? AND idempotency_key=?`, tt.action, tt.key).Scan(&subject, &reason, &outcome); err != nil {
				t.Fatal(err)
			}
			if subject != agentSessionInvalidAuditSubject || reason != agentSessionInvalidAuditReason || outcome != "failed" {
				t.Fatalf("subject=%q reason=%q outcome=%q", subject, reason, outcome)
			}
			if strings.Contains(subject, invalidSessionID) || strings.Contains(reason, invalidSessionID) {
				t.Fatalf("audit quoted rejected session ID: subject=%q reason=%q", subject, reason)
			}
		})
	}
}

func TestCloseAgentSessionRejectsUnsafeReasonWithoutSuccessfulWrites(t *testing.T) {
	tests := []struct {
		name   string
		reason string
	}{
		{name: "ANSI escape", reason: "operator\x1b[2J離開"},
		{name: "NUL", reason: "operator\x00離開"},
		{name: "newline", reason: "operator\n離開"},
		{name: "line separator", reason: "operator\u2028離開"},
		{name: "paragraph separator", reason: "operator\u2029離開"},
		{name: "invalid UTF-8", reason: string([]byte{'b', 'a', 'd', 0xff})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newTestStore(t)
			machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
			_, err := st.CloseAgentSession(closeAgentSessionRequest(
				"missing-session", machineID, "u-1", "ted@example.com", tt.reason, "unsafe-reason"))
			assertAgentSessionRejection(t, err, OperatorCodeAgentSessionCloseReasonRequired)

			if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 0 {
				t.Fatalf("rejected reason wrote %d sessions", got)
			}
			if got := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='agent-session-close' AND outcome='ok'`); got != 0 {
				t.Fatalf("rejected reason wrote %d successful audits", got)
			}
		})
	}
}

func TestOpenAgentSessionAllowsAssignedUser(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	result, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-1", machineID, "u-1", "ted@example.com", "open-1"))
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "session-1" || result.MachineID != machineID ||
		result.OperatorTailnetUserID != "u-1" || result.OperatorTailnetUserLogin != "ted@example.com" ||
		result.OpenedAt.IsZero() || result.ClosedAt != nil || result.Replayed || !result.Audited {
		t.Fatalf("result=%+v", result)
	}
	var userID, login, openedAt string
	var closedAt sql.NullString
	if err := st.DB().QueryRow(`SELECT operator_tailnet_user_id,operator_tailnet_user_login,opened_at,closed_at
	 FROM agent_sessions WHERE session_id='session-1'`).Scan(&userID, &login, &openedAt, &closedAt); err != nil {
		t.Fatal(err)
	}
	if userID != "u-1" || login != "ted@example.com" || parseTime(openedAt).IsZero() || closedAt.Valid {
		t.Fatalf("stored user=%q login=%q opened=%q closed=%v", userID, login, openedAt, closedAt)
	}
}

func TestOpenAgentSessionRejectsDifferentUser(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	_, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-b", machineID, "u-2", "other@example.com", "open-b"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionAssignedUserMismatch)
	if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 0 {
		t.Fatalf("rejected open wrote %d sessions", got)
	}
}

func TestOpenAgentSessionRejectsUnassignedMachine(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "", "")
	_, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-1", machineID, "u-1", "ted@example.com", "unassigned"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionMachineUnassigned)
}

func TestOpenAgentSessionRejectsEmptyOperatorUser(t *testing.T) {
	t.Run("assigned machine", func(t *testing.T) {
		st := newTestStore(t)
		machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
		_, err := st.OpenAgentSession(openAgentSessionRequest(
			"session-1", machineID, "", "ted@example.com", "empty-user"))
		assertAgentSessionRejection(t, err, OperatorCodeAgentSessionOperatorUserRequired)
		if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 0 {
			t.Fatalf("empty operator wrote %d sessions", got)
		}
	})
	t.Run("empty never equals unassigned", func(t *testing.T) {
		st := newTestStore(t)
		machineID := agentSessionMachine(t, st, "samplehub1", "", "")
		_, err := st.OpenAgentSession(openAgentSessionRequest(
			"session-1", machineID, "", "", "both-empty"))
		assertAgentSessionRejection(t, err, OperatorCodeAgentSessionMachineUnassigned)
		if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 0 {
			t.Fatalf("empty identities wrote %d sessions", got)
		}
	})
}

func TestOpenAgentSessionRejectsRetiredMachine(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	machineID := agentSessionMachineAt(t, st, "samplehub1", "u-1", "ted@example.com", now)
	if err := st.RetireMachine(machineID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-1", machineID, "u-1", "ted@example.com", "retired"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionMachineRetired)
}

func TestOpenAgentSessionRejectsUnknownMachine(t *testing.T) {
	st := newTestStore(t)
	_, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-1", "missing-machine", "u-1", "ted@example.com", "unknown"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionMachineNotFound)
}

func TestOpenAgentSessionMatchesIDNotLogin(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	_, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-1", machineID, "u-2", "ted@example.com", "same-login"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionAssignedUserMismatch)
	if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 0 {
		t.Fatalf("login match bypass wrote %d sessions", got)
	}
}

func TestChangingAssignedUserClosesOpenSessions(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := agentSessionMachineAt(t, st, "samplehub1", "", "", now)
	if _, err := applyAssignedUser(t, st, machineID, "samplehub1", "1", "a@example.com", "assign-a", 0); err != nil {
		t.Fatal(err)
	}
	openReq := openAgentSessionRequest("session-a", machineID, "1", "a@example.com", "open-a")
	if _, err := st.OpenAgentSession(openReq); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := applyAssignedUser(t, st, machineID, "samplehub1", "2", "b@example.com", "assign-b", 1); err != nil {
		t.Fatal(err)
	}
	closed := mustAgentSession(t, st, "session-a")
	if closed.ClosedAt == nil || !closed.ClosedAt.Equal(now) || closed.CloseReason != AgentSessionCloseReasonAssignedUserChanged {
		t.Fatalf("closed=%+v", closed)
	}
	_, err := st.OpenAgentSession(openReq)
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionAssignedUserMismatch)
	var replay *OperatorRequestError
	if !errors.As(err, &replay) || !replay.Replayed || !replay.Audited {
		t.Fatalf("old successful key was not invalidated safely: %v", err)
	}
	_, err = st.OpenAgentSession(openAgentSessionRequest(
		"session-a-new", machineID, "1", "a@example.com", "open-a-new"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionAssignedUserMismatch)
}

func TestClearingAssignedUserClosesOpenSessions(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := agentSessionMachineAt(t, st, "samplehub1", "", "", now)
	if _, err := applyAssignedUser(t, st, machineID, "samplehub1", "1", "a@example.com", "assign-a", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-a", machineID, "1", "a@example.com", "open-a")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := applyAssignedUser(t, st, machineID, "samplehub1", AssignedUserNone, "", "clear-a", 1); err != nil {
		t.Fatal(err)
	}
	closed := mustAgentSession(t, st, "session-a")
	if closed.ClosedAt == nil || !closed.ClosedAt.Equal(now) || closed.CloseReason != AgentSessionCloseReasonAssignedUserChanged {
		t.Fatalf("closed=%+v", closed)
	}
}

func TestUnchangedAssignedUserKeepsSessions(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := agentSessionMachineAt(t, st, "samplehub1", "", "", now)
	if _, err := applyAssignedUser(t, st, machineID, "samplehub1", "1", "a@example.com", "assign-a", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-a", machineID, "1", "a@example.com", "open-a")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	result, err := applyAssignedUser(t, st, machineID, "samplehub1", "1", "a@example.com", "assign-a-again", 1)
	if err != nil || result.Revision != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if session := mustAgentSession(t, st, "session-a"); session.ClosedAt != nil || session.CloseReason != "" {
		t.Fatalf("unchanged assignment closed session: %+v", session)
	}
}

func TestCloseAgentSessionIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := agentSessionMachineAt(t, st, "samplehub1", "u-1", "ted@example.com", now)
	if _, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-1", machineID, "u-1", "ted@example.com", "open-1")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	first, err := st.CloseAgentSession(closeAgentSessionRequest(
		"session-1", machineID, "u-1", "ted@example.com", "operator 離開終端", "close-1"))
	if err != nil || first.ClosedAt == nil || first.CloseReason != "operator 離開終端" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	firstClosedAt := *first.ClosedAt
	now = now.Add(time.Hour)
	second, err := st.CloseAgentSession(closeAgentSessionRequest(
		"session-1", machineID, "u-1", "ted@example.com", "第二次關閉", "close-2"))
	if err != nil || second.ClosedAt == nil || !second.ClosedAt.Equal(firstClosedAt) ||
		second.CloseReason != "operator 離開終端" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	var closeAudits, successfulCloseAudits int
	if err := st.DB().QueryRow(`SELECT COUNT(*),
	 SUM(CASE WHEN outcome='ok' AND auth_subject='tailscale-user:u-1' AND who_user='ted@example.com'
	  AND subject='session-1' AND reason IN ('operator 離開終端','第二次關閉') THEN 1 ELSE 0 END)
	 FROM audit_log WHERE action=? AND machine_id=?`, AuditAgentSessionClose, machineID).
		Scan(&closeAudits, &successfulCloseAudits); err != nil {
		t.Fatal(err)
	}
	if closeAudits != 2 || successfulCloseAudits != 2 {
		t.Fatalf("close audits=%d successful with identity=%d", closeAudits, successfulCloseAudits)
	}
}

func TestOpenAgentSessionRecordsAudit(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	if _, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-ok", machineID, "u-1", "ted@example.com", "audit-ok")); err != nil {
		t.Fatal(err)
	}
	_, rejectedErr := st.OpenAgentSession(openAgentSessionRequest(
		"session-no", machineID, "u-2", "other@example.com", "audit-no"))
	assertAgentSessionRejection(t, rejectedErr, OperatorCodeAgentSessionAssignedUserMismatch)
	rows, err := st.DB().Query(`SELECT outcome,machine_id,auth_subject,who_user,detail
	 FROM audit_log WHERE action=? ORDER BY audit_id`, AuditAgentSessionOpen)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type auditRow struct{ outcome, machineID, subject, login, detail string }
	var audits []auditRow
	for rows.Next() {
		var row auditRow
		var detail sql.NullString
		if err := rows.Scan(&row.outcome, &row.machineID, &row.subject, &row.login, &detail); err != nil {
			t.Fatal(err)
		}
		row.detail = detail.String
		audits = append(audits, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 {
		t.Fatalf("audits=%+v", audits)
	}
	if audits[0].outcome != "ok" || audits[0].machineID != machineID ||
		audits[0].subject != "tailscale-user:u-1" || audits[0].login != "ted@example.com" || audits[0].detail != "" {
		t.Fatalf("success audit=%+v", audits[0])
	}
	if audits[1].outcome != "failed" || audits[1].machineID != machineID ||
		audits[1].subject != "tailscale-user:u-2" || audits[1].login != "other@example.com" ||
		!strings.Contains(audits[1].detail, "只允許指派使用者") {
		t.Fatalf("rejection audit=%+v", audits[1])
	}
}

func TestOpenAgentSessionRejectionWritesNoSession(t *testing.T) {
	tests := []struct {
		name string
		code string
		run  func(*testing.T, *Store) error
	}{
		{"unknown machine", OperatorCodeAgentSessionMachineNotFound, func(t *testing.T, st *Store) error {
			_, err := st.OpenAgentSession(openAgentSessionRequest("s", "missing", "u-1", "a@example.com", "unknown"))
			return err
		}},
		{"retired machine", OperatorCodeAgentSessionMachineRetired, func(t *testing.T, st *Store) error {
			now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
			machineID := agentSessionMachineAt(t, st, "samplehub1", "u-1", "a@example.com", now)
			if err := st.RetireMachine(machineID, now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			_, err := st.OpenAgentSession(openAgentSessionRequest("s", machineID, "u-1", "a@example.com", "retired"))
			return err
		}},
		{"unassigned machine", OperatorCodeAgentSessionMachineUnassigned, func(t *testing.T, st *Store) error {
			machineID := agentSessionMachine(t, st, "samplehub1", "", "")
			_, err := st.OpenAgentSession(openAgentSessionRequest("s", machineID, "u-1", "a@example.com", "unassigned"))
			return err
		}},
		{"empty operator", OperatorCodeAgentSessionOperatorUserRequired, func(t *testing.T, st *Store) error {
			machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "a@example.com")
			_, err := st.OpenAgentSession(openAgentSessionRequest("s", machineID, "", "a@example.com", "empty"))
			return err
		}},
		{"different operator", OperatorCodeAgentSessionAssignedUserMismatch, func(t *testing.T, st *Store) error {
			machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "a@example.com")
			_, err := st.OpenAgentSession(openAgentSessionRequest("s", machineID, "u-2", "b@example.com", "mismatch"))
			return err
		}},
		{"invalid session id", OperatorCodeAgentSessionInvalid, func(t *testing.T, st *Store) error {
			machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "a@example.com")
			_, err := st.OpenAgentSession(openAgentSessionRequest("", machineID, "u-1", "a@example.com", "invalid"))
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newTestStore(t)
			before := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`)
			assertAgentSessionRejection(t, tt.run(t, st), tt.code)
			if after := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); after != before {
				t.Fatalf("session count %d -> %d", before, after)
			}
		})
	}
}

func TestListOpenSessionsForMachine(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	otherID := agentSessionMachine(t, st, "sampleagent1", "u-1", "ted@example.com")
	for _, item := range []struct{ sessionID, machineID, key string }{
		{"session-open", machineID, "open-1"},
		{"session-closed", machineID, "open-2"},
		{"session-other", otherID, "open-3"},
	} {
		if _, err := st.OpenAgentSession(openAgentSessionRequest(
			item.sessionID, item.machineID, "u-1", "ted@example.com", item.key)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CloseAgentSession(closeAgentSessionRequest(
		"session-closed", machineID, "u-1", "ted@example.com", "測試關閉", "close-2")); err != nil {
		t.Fatal(err)
	}
	open, err := st.ListOpenAgentSessionsForMachine(machineID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].SessionID != "session-open" || open[0].ClosedAt != nil {
		t.Fatalf("open=%+v", open)
	}
}

func TestCloseAllOpenAgentSessionsClosesAcrossMachinesAndPreservesFirstClose(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	firstMachine := agentSessionMachineAt(t, st, "samplehub1", "u-1", "ted@example.com", now)
	secondMachine := agentSessionMachineAt(t, st, "sampleagent1", "u-1", "ted@example.com", now)
	for _, item := range []struct{ sessionID, machineID, key string }{
		{"first-a", firstMachine, "open-first-a"},
		{"first-b", firstMachine, "open-first-b"},
		{"second-a", secondMachine, "open-second-a"},
		{"already-closed", secondMachine, "open-already-closed"},
	} {
		if _, err := st.OpenAgentSession(openAgentSessionRequest(
			item.sessionID, item.machineID, "u-1", "ted@example.com", item.key)); err != nil {
			t.Fatal(err)
		}
	}
	firstCloseReason := "operator 已關閉終端"
	firstCloseAt := now.Add(time.Minute)
	now = firstCloseAt
	if _, err := st.CloseAgentSession(closeAgentSessionRequest(
		"already-closed", secondMachine, "u-1", "ted@example.com", firstCloseReason, "close-first")); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Hour)
	closed, err := st.CloseAllOpenAgentSessions(AgentSessionCloseReasonHubRestart)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 3 {
		t.Fatalf("CloseAllOpenAgentSessions() closed = %d, want 3", closed)
	}
	for _, sessionID := range []string{"first-a", "first-b", "second-a"} {
		session := mustAgentSession(t, st, sessionID)
		if session.ClosedAt == nil || !session.ClosedAt.Equal(now) ||
			session.CloseReason != AgentSessionCloseReasonHubRestart {
			t.Errorf("session %q = %+v", sessionID, session)
		}
	}
	previouslyClosed := mustAgentSession(t, st, "already-closed")
	if previouslyClosed.ClosedAt == nil || !previouslyClosed.ClosedAt.Equal(firstCloseAt) ||
		previouslyClosed.CloseReason != firstCloseReason {
		t.Fatalf("already-closed session was rewritten: %+v", previouslyClosed)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions
		WHERE (closed_at IS NULL) <> (close_reason IS NULL)
		   OR (closed_at IS NOT NULL AND close_reason='')`); got != 0 {
		t.Fatalf("agent session close CHECK violations = %d", got)
	}
}

func TestCloseAgentSessionsByIDClosesOnlyListedOpenRows(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := agentSessionMachineAt(t, st, "samplehub1", "u-1", "ted@example.com", now)
	for _, sessionID := range []string{"listed-a", "listed-b", "unlisted", "already-closed"} {
		if _, err := st.OpenAgentSession(openAgentSessionRequest(
			sessionID, machineID, "u-1", "ted@example.com", "open-"+sessionID)); err != nil {
			t.Fatal(err)
		}
	}
	firstCloseReason := "operator 已關閉終端"
	firstCloseAt := now.Add(time.Minute)
	now = firstCloseAt
	if _, err := st.CloseAgentSession(closeAgentSessionRequest(
		"already-closed", machineID, "u-1", "ted@example.com", firstCloseReason, "close-already")); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Hour)
	closed, err := st.CloseAgentSessionsByID(
		[]string{"listed-b", "missing", "already-closed", "listed-a"},
		AgentSessionCloseReasonAgentConnectionEnded)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 2 {
		t.Fatalf("CloseAgentSessionsByID() closed = %d, want 2", closed)
	}
	for _, sessionID := range []string{"listed-a", "listed-b"} {
		session := mustAgentSession(t, st, sessionID)
		if session.ClosedAt == nil || !session.ClosedAt.Equal(now) ||
			session.CloseReason != AgentSessionCloseReasonAgentConnectionEnded {
			t.Errorf("session %q = %+v", sessionID, session)
		}
	}
	if session := mustAgentSession(t, st, "unlisted"); session.ClosedAt != nil || session.CloseReason != "" {
		t.Fatalf("unlisted session was closed: %+v", session)
	}
	previouslyClosed := mustAgentSession(t, st, "already-closed")
	if previouslyClosed.ClosedAt == nil || !previouslyClosed.ClosedAt.Equal(firstCloseAt) ||
		previouslyClosed.CloseReason != firstCloseReason {
		t.Fatalf("already-closed session was rewritten: %+v", previouslyClosed)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions
		WHERE (closed_at IS NULL) <> (close_reason IS NULL)
		   OR (closed_at IS NOT NULL AND close_reason='')`); got != 0 {
		t.Fatalf("agent session close CHECK violations = %d", got)
	}
}

func TestCloseAgentSessionsByIDEmptyListExecutesNoStatement(t *testing.T) {
	st := newTestStore(t)
	if err := st.DB().Close(); err != nil {
		t.Fatal(err)
	}
	closed, err := st.CloseAgentSessionsByID(nil, AgentSessionCloseReasonAgentConnectionEnded)
	if err != nil || closed != 0 {
		t.Fatalf("CloseAgentSessionsByID(nil) = %d, %v; want 0, nil", closed, err)
	}
}

func TestRetiredMachineIDsHandlesUnknownAndMixedSets(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	activeID := agentSessionMachineAt(t, st, "active", "u-1", "ted@example.com", now)
	retiredB := agentSessionMachineAt(t, st, "retired-b", "u-1", "ted@example.com", now)
	retiredA := agentSessionMachineAt(t, st, "retired-a", "u-1", "ted@example.com", now)
	if err := st.RetireMachine(retiredA, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(retiredB, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	got, err := st.RetiredMachineIDs([]string{retiredB, "unknown", activeID, retiredA, retiredA})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{retiredA, retiredB}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("RetiredMachineIDs() = %v, want %v", got, want)
	}
}

func TestRetiredMachineIDsEmptyExecutesNoStatement(t *testing.T) {
	st := newTestStore(t)
	if err := st.DB().Close(); err != nil {
		t.Fatal(err)
	}
	got, err := st.RetiredMachineIDs(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("RetiredMachineIDs(nil) = %v, %v; want empty, nil", got, err)
	}
}

func TestUnauthorizedAgentSessionIDsHandlesUnknownClosedAndMixedSets(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := agentSessionMachineAt(t, st, "session-set", "u-1", "ted@example.com", now)
	for _, sessionID := range []string{"authorized", "closed"} {
		if _, err := st.OpenAgentSession(openAgentSessionRequest(
			sessionID, machineID, "u-1", "ted@example.com", "open-set-"+sessionID)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CloseAgentSessionsByID([]string{"closed"}, AgentSessionCloseReasonAgentConnectionEnded); err != nil {
		t.Fatal(err)
	}

	got, err := st.UnauthorizedAgentSessionIDs([]string{"unknown", "authorized", "closed", "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"closed", "unknown"}; !slices.Equal(got, want) {
		t.Fatalf("UnauthorizedAgentSessionIDs() = %v, want %v", got, want)
	}
}

func TestUnauthorizedAgentSessionIDsEmptyExecutesNoStatement(t *testing.T) {
	st := newTestStore(t)
	if err := st.DB().Close(); err != nil {
		t.Fatal(err)
	}
	got, err := st.UnauthorizedAgentSessionIDs(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("UnauthorizedAgentSessionIDs(nil) = %v, %v; want empty, nil", got, err)
	}
}

func TestBulkAgentSessionCloseMethodsRejectInvalidReasons(t *testing.T) {
	invalidReasons := []string{"", " surrounding ", "two\nlines", strings.Repeat("x", auditMaxReason+1), string([]byte{0xff})}
	for _, reason := range invalidReasons {
		t.Run(fmt.Sprintf("%q", reason), func(t *testing.T) {
			st := newTestStore(t)
			if _, err := st.CloseAllOpenAgentSessions(reason); !errors.Is(err, ErrAgentSessionCloseReasonRequired) {
				t.Fatalf("CloseAllOpenAgentSessions(%q) error = %v", reason, err)
			}
			if _, err := st.CloseAgentSessionsByID([]string{"session"}, reason); !errors.Is(err, ErrAgentSessionCloseReasonRequired) {
				t.Fatalf("CloseAgentSessionsByID(%q) error = %v", reason, err)
			}
			if _, err := st.CloseUnattachedAgentSessions([]string{"session"}, time.Now(), reason); !errors.Is(err, ErrAgentSessionCloseReasonRequired) {
				t.Fatalf("CloseUnattachedAgentSessions(%q) error = %v", reason, err)
			}
		})
	}
}

func TestCloseUnattachedAgentSessionsRespectsCutoffAndAttachment(t *testing.T) {
	st := newTestStore(t)
	base := time.Date(2026, 9, 25, 16, 2, 3, 0, time.UTC)
	now := base
	st.nowFn = func() time.Time { return now }
	machineID := agentSessionMachineAt(t, st, "samplehub1", "u-1", "ted@example.com", base)
	for _, sessionID := range []string{"exact", "younger", "attached", "already"} {
		switch sessionID {
		case "younger":
			now = base.Add(time.Second)
		case "attached", "already":
			now = base.Add(-time.Hour)
		default:
			now = base
		}
		if _, err := st.OpenAgentSession(openAgentSessionRequest(
			sessionID, machineID, "u-1", "ted@example.com", "open-"+sessionID)); err != nil {
			t.Fatal(err)
		}
	}
	firstCloseAt := base.Add(3 * time.Hour)
	now = firstCloseAt
	firstCloseReason := "operator 已關閉終端"
	if _, err := st.CloseAgentSessionsByID([]string{"already"}, firstCloseReason); err != nil {
		t.Fatal(err)
	}

	// A tick carries a fraction and may be in any zone. This one is base plus
	// a fraction, spelled at UTC+9. The row opened on that whole UTC second is
	// at or before it; the row one second later is not.
	loc := time.FixedZone("UTC+9", 9*3600)
	cutoff := time.Date(2026, 9, 26, 1, 2, 3, 123456789, loc)
	if !cutoff.UTC().Truncate(time.Second).Equal(base) {
		t.Fatalf("cutoff truncates to %s, want %s", cutoff.UTC().Truncate(time.Second), base)
	}
	if _, err := st.CloseUnattachedAgentSessions(nil, cutoff, " bad "); !errors.Is(err, ErrAgentSessionCloseReasonRequired) {
		t.Fatalf("invalid reason error = %v", err)
	}

	closeAt := base.Add(4 * time.Hour)
	now = closeAt
	closed, err := st.CloseUnattachedAgentSessions([]string{"attached"}, cutoff, AgentSessionCloseReasonNeverAttached)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("CloseUnattachedAgentSessions() closed = %d, want 1", closed)
	}
	exact := mustAgentSession(t, st, "exact")
	if exact.ClosedAt == nil || !exact.ClosedAt.Equal(closeAt) || exact.CloseReason != AgentSessionCloseReasonNeverAttached ||
		!exact.OpenedAt.Equal(base) {
		t.Fatalf("exact session = %+v", exact)
	}
	if session := mustAgentSession(t, st, "younger"); session.ClosedAt != nil || !session.OpenedAt.Equal(base.Add(time.Second)) {
		t.Fatalf("younger session = %+v", session)
	}
	if session := mustAgentSession(t, st, "attached"); session.ClosedAt != nil {
		t.Fatalf("attached session was closed: %+v", session)
	}
	previouslyClosed := mustAgentSession(t, st, "already")
	if previouslyClosed.ClosedAt == nil || !previouslyClosed.ClosedAt.Equal(firstCloseAt) ||
		previouslyClosed.CloseReason != firstCloseReason {
		t.Fatalf("already-closed session was rewritten: %+v", previouslyClosed)
	}

	closed, err = st.CloseUnattachedAgentSessions([]string{}, cutoff, AgentSessionCloseReasonNeverAttached)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("empty attachment list closed = %d, want 1", closed)
	}
	attached := mustAgentSession(t, st, "attached")
	if attached.ClosedAt == nil || !attached.ClosedAt.Equal(closeAt) || attached.CloseReason != AgentSessionCloseReasonNeverAttached {
		t.Fatalf("attached session after empty exclusion list = %+v", attached)
	}
	if session := mustAgentSession(t, st, "younger"); session.ClosedAt != nil {
		t.Fatalf("younger session was closed: %+v", session)
	}
	exact = mustAgentSession(t, st, "exact")
	if exact.ClosedAt == nil || !exact.ClosedAt.Equal(closeAt) || exact.CloseReason != AgentSessionCloseReasonNeverAttached {
		t.Fatalf("exact session was rewritten: %+v", exact)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions
		WHERE (closed_at IS NULL) <> (close_reason IS NULL)
		   OR (closed_at IS NOT NULL AND close_reason='')`); got != 0 {
		t.Fatalf("agent session close CHECK violations = %d", got)
	}
}

func TestOpenAgentSessionCountCountsOnlyThisMachinesOpenRows(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	otherID := agentSessionMachine(t, st, "sampleagent1", "u-1", "ted@example.com")
	if n, err := st.OpenAgentSessionCount(machineID); err != nil || n != 0 {
		t.Fatalf("empty count=%d err=%v", n, err)
	}
	openAgentSessions(t, st, machineID, "here", 2)
	openAgentSessions(t, st, otherID, "there", 1)
	if n, err := st.OpenAgentSessionCount(machineID); err != nil || n != 2 {
		t.Fatalf("open count=%d err=%v", n, err)
	}
	if n, err := st.OpenAgentSessionCount(otherID); err != nil || n != 1 {
		t.Fatalf("other count=%d err=%v", n, err)
	}
	if _, err := st.CloseAgentSessionsByID([]string{"here-00"}, AgentSessionCloseReasonViewerClosed); err != nil {
		t.Fatal(err)
	}
	if n, err := st.OpenAgentSessionCount(machineID); err != nil || n != 1 {
		t.Fatalf("after close count=%d err=%v", n, err)
	}
	if n, err := st.OpenAgentSessionCount(otherID); err != nil || n != 1 {
		t.Fatalf("other count after close=%d err=%v", n, err)
	}
}

func TestOpenAgentSessionCapRejectsAtLimitAndAllowsOneBelow(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	openAgentSessions(t, st, machineID, "below", MaxOpenAgentSessionsPerMachine-1)

	last, err := st.OpenAgentSession(openAgentSessionRequest(
		"at-cap", machineID, "u-1", "ted@example.com", "open-at-cap"))
	if err != nil {
		t.Fatalf("open one below the cap: %v", err)
	}
	if last.SessionID != "at-cap" || last.Replayed || !last.Audited {
		t.Fatalf("open one below the cap = %+v", last)
	}

	before := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions WHERE machine_id=?`, machineID)
	_, err = st.OpenAgentSession(openAgentSessionRequest(
		"over-cap", machineID, "u-1", "ted@example.com", "open-over-cap"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionLimitReached)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || !errors.Is(err, ErrAgentSessionLimitReached) {
		t.Fatalf("limit rejection = %v", err)
	}
	message := agentSessionLimitReachedDetail(MaxOpenAgentSessionsPerMachine)
	if rejection.Detail != message {
		t.Fatalf("detail = %q, want %q", rejection.Detail, message)
	}
	if after := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions WHERE machine_id=?`, machineID); after != before {
		t.Fatalf("rejected open changed session count %d -> %d", before, after)
	}
	assertAgentSessionLimitAudit(t, st, "open-over-cap", machineID, "over-cap", message)
}

func TestOpenAgentSessionCloseFreesCapSlot(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	reqs := openAgentSessions(t, st, machineID, "full", MaxOpenAgentSessionsPerMachine)
	_, err := st.OpenAgentSession(openAgentSessionRequest(
		"blocked", machineID, "u-1", "ted@example.com", "open-blocked"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionLimitReached)
	if _, err := st.CloseAgentSession(closeAgentSessionRequest(
		reqs[0].SessionID, machineID, "u-1", "ted@example.com", "operator 離開終端", "close-slot")); err != nil {
		t.Fatal(err)
	}
	opened, err := st.OpenAgentSession(openAgentSessionRequest(
		"after-close", machineID, "u-1", "ted@example.com", "open-after-close"))
	if err != nil {
		t.Fatalf("open after close: %v", err)
	}
	if opened.SessionID != "after-close" || opened.ClosedAt != nil {
		t.Fatalf("opened = %+v", opened)
	}
}

func TestOpenAgentSessionReplaySucceedsAtCap(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	openAgentSessions(t, st, machineID, "held", MaxOpenAgentSessionsPerMachine-1)
	holding := openAgentSessionRequest("holds-last", machineID, "u-1", "ted@example.com", "open-holds-last")
	if _, err := st.OpenAgentSession(holding); err != nil {
		t.Fatal(err)
	}
	replayed, err := st.OpenAgentSession(holding)
	if err != nil {
		t.Fatalf("replay at cap: %v", err)
	}
	if !replayed.Replayed || replayed.SessionID != holding.SessionID || replayed.ClosedAt != nil {
		t.Fatalf("replay = %+v", replayed)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions WHERE machine_id=? AND closed_at IS NULL`, machineID); got != MaxOpenAgentSessionsPerMachine {
		t.Fatalf("open sessions = %d, want %d", got, MaxOpenAgentSessionsPerMachine)
	}
}

func TestOpenAgentSessionCapIsPerMachine(t *testing.T) {
	st := newTestStore(t)
	fullID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	emptyID := agentSessionMachine(t, st, "sampleagent1", "u-1", "ted@example.com")
	openAgentSessions(t, st, fullID, "full-machine", MaxOpenAgentSessionsPerMachine)
	opened, err := st.OpenAgentSession(openAgentSessionRequest(
		"other-machine", emptyID, "u-1", "ted@example.com", "open-other-machine"))
	if err != nil {
		t.Fatalf("open on an empty machine while another is full: %v", err)
	}
	if opened.MachineID != emptyID || opened.Replayed {
		t.Fatalf("opened = %+v", opened)
	}
	_, err = st.OpenAgentSession(openAgentSessionRequest(
		"still-full", fullID, "u-1", "ted@example.com", "open-still-full"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionLimitReached)
}

func TestOpenAgentSessionCapDoesNotMaskMachineChecks(t *testing.T) {
	// RetireMachine closes the open rows, which would take the machine off the
	// cap, and a retired_at-only update is rejected by the lifecycle guard.
	// Retire through the canonical writer, then mark those rows open again, so
	// the machine is retired and at the cap together. Clearing the assignee is
	// a direct registry update for the same reason: an unassigned machine
	// cannot be filled through OpenAgentSession. Those reasons have to win.
	tests := []struct {
		name   string
		prefix string
		code   string
		userID string
		login  string
		alter  func(t *testing.T, st *Store, machineID string)
	}{
		{
			name: "retired", prefix: "retired", code: OperatorCodeAgentSessionMachineRetired,
			userID: "u-1", login: "ted@example.com",
			alter: func(t *testing.T, st *Store, machineID string) {
				if err := st.RetireMachine(machineID, time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB().Exec(`UPDATE agent_sessions
					 SET closed_at=NULL, close_reason=NULL WHERE machine_id=?`, machineID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unassigned", prefix: "unassigned", code: OperatorCodeAgentSessionMachineUnassigned,
			userID: "u-1", login: "ted@example.com",
			alter: func(t *testing.T, st *Store, machineID string) {
				if _, err := st.DB().Exec(`UPDATE machine_registry
					 SET assigned_user_id=NULL, assigned_user_login=NULL WHERE machine_id=?`, machineID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "assigned user mismatch", prefix: "mismatch", code: OperatorCodeAgentSessionAssignedUserMismatch,
			userID: "u-2", login: "other@example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newTestStore(t)
			machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
			openAgentSessions(t, st, machineID, tt.prefix, MaxOpenAgentSessionsPerMachine)
			if tt.alter != nil {
				tt.alter(t, st, machineID)
			}
			before := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`)
			_, err := st.OpenAgentSession(openAgentSessionRequest(
				"masked", machineID, tt.userID, tt.login, "open-masked-"+tt.name))
			assertAgentSessionRejection(t, err, tt.code)
			if after := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); after != before {
				t.Fatalf("session count %d -> %d", before, after)
			}
		})
	}
}

func TestOpenAgentSessionClosedSessionsDoNotCountTowardCap(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	for i := 0; i < MaxOpenAgentSessionsPerMachine+2; i++ {
		sessionID := fmt.Sprintf("closed-%02d", i)
		if _, err := st.OpenAgentSession(openAgentSessionRequest(
			sessionID, machineID, "u-1", "ted@example.com", fmt.Sprintf("open-closed-%02d", i))); err != nil {
			t.Fatalf("open %s: %v", sessionID, err)
		}
		if _, err := st.CloseAgentSession(closeAgentSessionRequest(
			sessionID, machineID, "u-1", "ted@example.com", "operator 離開終端",
			fmt.Sprintf("close-closed-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	openAgentSessions(t, st, machineID, "live", MaxOpenAgentSessionsPerMachine)
	if got := countRows(t, st, `SELECT COUNT(*) FROM agent_sessions WHERE machine_id=? AND closed_at IS NULL`, machineID); got != MaxOpenAgentSessionsPerMachine {
		t.Fatalf("open sessions = %d, want %d", got, MaxOpenAgentSessionsPerMachine)
	}
	_, err := st.OpenAgentSession(openAgentSessionRequest(
		"one-more", machineID, "u-1", "ted@example.com", "open-one-more"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionLimitReached)
}

func TestOpenAgentSessionDuplicateIDWinsOverCap(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	reqs := openAgentSessions(t, st, machineID, "dup", MaxOpenAgentSessionsPerMachine)
	if _, err := st.CloseAgentSession(closeAgentSessionRequest(
		reqs[0].SessionID, machineID, "u-1", "ted@example.com", "operator 離開終端", "close-dup")); err != nil {
		t.Fatal(err)
	}
	// Closing freed a slot. Filling it again leaves the machine at the cap
	// while the closed ID still holds the primary key. Both checks would
	// reject a reuse; the ID collision has to be the one reported.
	openAgentSessions(t, st, machineID, "dup-live", 1)
	_, err := st.OpenAgentSession(openAgentSessionRequest(
		reqs[0].SessionID, machineID, "u-1", "ted@example.com", "open-duplicate-at-cap"))
	assertAgentSessionRejection(t, err, OperatorCodeAgentSessionExists)
}

func openAgentSessions(t *testing.T, st *Store, machineID, prefix string, n int) []OpenAgentSessionRequest {
	t.Helper()
	reqs := make([]OpenAgentSessionRequest, n)
	for i := 0; i < n; i++ {
		reqs[i] = openAgentSessionRequest(
			fmt.Sprintf("%s-%02d", prefix, i), machineID, "u-1", "ted@example.com",
			fmt.Sprintf("%s-key-%02d", prefix, i))
		if _, err := st.OpenAgentSession(reqs[i]); err != nil {
			t.Fatalf("open %s: %v", reqs[i].SessionID, err)
		}
	}
	return reqs
}

func assertAgentSessionLimitAudit(t *testing.T, st *Store, key, machineID, sessionID, message string) {
	t.Helper()
	var action, outcome, detail, gotMachine, reason string
	if err := st.DB().QueryRow(`SELECT action, outcome, COALESCE(detail,''), COALESCE(machine_id,''), COALESCE(reason,'')
		 FROM audit_log WHERE idempotency_key=?`, key).Scan(
		&action, &outcome, &detail, &gotMachine, &reason); err != nil {
		t.Fatal(err)
	}
	if action != string(AuditAgentSessionOpen) || outcome != "failed" || detail != message ||
		gotMachine != machineID || reason != "session "+sessionID {
		t.Fatalf("audit action=%q outcome=%q detail=%q machine=%q reason=%q",
			action, outcome, detail, gotMachine, reason)
	}
	var idemOutcome, code, errorDetail string
	if err := st.DB().QueryRow(`SELECT outcome, error_code, error_detail
		 FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(
		&idemOutcome, &code, &errorDetail); err != nil {
		t.Fatal(err)
	}
	if idemOutcome != "rejected" || code != OperatorCodeAgentSessionLimitReached || errorDetail != message {
		t.Fatalf("idempotency outcome=%q code=%q detail=%q", idemOutcome, code, errorDetail)
	}
}

func openAgentSessionRequest(sessionID, machineID, userID, login, key string) OpenAgentSessionRequest {
	return OpenAgentSessionRequest{
		SessionID: sessionID, MachineID: machineID,
		OperatorTailnetUserID: userID, OperatorTailnetUserLogin: login,
		IdempotencyKey: key, RequestDigest: "sha256:" + key,
		Audit: AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
	}
}

func closeAgentSessionRequest(sessionID, machineID, userID, login, reason, key string) CloseAgentSessionRequest {
	return CloseAgentSessionRequest{
		SessionID: sessionID, MachineID: machineID,
		OperatorTailnetUserID: userID, OperatorTailnetUserLogin: login,
		Reason: reason, IdempotencyKey: key, RequestDigest: "sha256:" + key,
		Audit: AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
	}
}

func agentSessionMachine(t *testing.T, st *Store, name, userID, login string) string {
	t.Helper()
	return agentSessionMachineAt(t, st, name, userID, login,
		time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC))
}

func agentSessionMachineAt(t *testing.T, st *Store, name, userID, login string, now time.Time) string {
	t.Helper()
	machineID := mustEnroll(t, st, name, now)
	if userID != "" {
		if _, err := st.DB().Exec(`UPDATE machine_registry
		 SET assigned_user_id=?,assigned_user_login=? WHERE machine_id=?`, userID, login, machineID); err != nil {
			t.Fatal(err)
		}
	}
	return machineID
}

func assertAgentSessionRejection(t *testing.T, err error, code string) {
	t.Helper()
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != code || !rejection.Audited {
		t.Fatalf("rejection code=%q err=%v", code, err)
	}
}

func mustAgentSession(t *testing.T, st *Store, sessionID string) AgentSessionResult {
	t.Helper()
	result, err := agentSessionByID(st.DB(), sessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAgentSessionForOperatorReadsOnlyTheOpenOwner(t *testing.T) {
	st := newTestStore(t)
	machineID := agentSessionMachine(t, st, "samplehub1", "u-1", "ted@example.com")
	otherID := agentSessionMachine(t, st, "sampleagent1", "u-1", "ted@example.com")
	if _, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-1", machineID, "u-1", "ted@example.com", "open-owned")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-other", otherID, "u-1", "ted@example.com", "open-other")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-login", machineID, "u-1", "shared@example.com", "open-login")); err != nil {
		t.Fatal(err)
	}
	closed, err := st.OpenAgentSession(openAgentSessionRequest(
		"session-closed", machineID, "u-1", "ted@example.com", "open-closed"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CloseAgentSession(closeAgentSessionRequest(
		closed.SessionID, machineID, "u-1", "ted@example.com", "operator 離開終端", "close-owned")); err != nil {
		t.Fatal(err)
	}

	got, err := st.AgentSessionForOperator(machineID, "session-1", "u-1")
	if err != nil || got.SessionID != "session-1" || got.MachineID != machineID ||
		got.OperatorTailnetUserID != "u-1" || got.ClosedAt != nil {
		t.Fatalf("session=%+v err=%v", got, err)
	}

	for _, test := range []struct {
		name, machineID, sessionID, userID string
	}{
		{name: "unknown machine", machineID: "missing", sessionID: "session-1", userID: "u-1"},
		{name: "missing session", machineID: machineID, sessionID: "missing-session", userID: "u-1"},
		{name: "other machine", machineID: machineID, sessionID: "session-other", userID: "u-1"},
		{name: "closed", machineID: machineID, sessionID: "session-closed", userID: "u-1"},
		{name: "different tailnet user id", machineID: machineID, sessionID: "session-login", userID: "u-2"},
		{name: "invalid session id", machineID: machineID, sessionID: strings.Repeat("s", sessionid.MaxLength+1), userID: "u-1"},
		{name: "padded session id", machineID: machineID, sessionID: " session-1", userID: "u-1"},
		{name: "empty operator id", machineID: machineID, sessionID: "session-1", userID: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := st.AgentSessionForOperator(test.machineID, test.sessionID, test.userID)
			if !errors.Is(err, ErrNotFound) || result.SessionID != "" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
