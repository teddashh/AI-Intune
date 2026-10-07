package web

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/sessionid"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	terminalOpenUserAgent = "terminal-open-test"
	terminalOpenLogin     = "operator@example.com"
	terminalOpenUserID    = "42"
)

type openTerminalLink struct{}

func (openTerminalLink) Send(agentrelay.Downstream) error { return nil }
func (openTerminalLink) Close(string)                     {}

func TestTerminalOpenRedirectsToTheRecordedSession(t *testing.T) {
	s, st := newServer(t)
	machineID := assignedTerminalMachine(t, st, "samplehub1")
	attachTerminalLink(t, s, machineID)
	sessionID := "session-open-1"
	form := terminalOpenForm(sessionID, "open-key-1")

	rec := postTerminalOpen(t, s, machineID, form, nil)
	assertTerminalOpenRedirect(t, rec, machineID, sessionID)
	assertOpenTerminalRow(t, st, machineID, sessionID, terminalOpenUserID)
	assertTerminalOpenAudit(t, st, terminalOpenAuditWant(t, s, machineID, sessionID, "open-key-1", true, ""))

	again := postTerminalOpen(t, s, machineID, form, nil)
	assertTerminalOpenRedirect(t, again, machineID, sessionID)
	if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 1 {
		t.Fatalf("replay wrote %d sessions", got)
	}
}

func TestTerminalOpenRefusalsNameTheNextStep(t *testing.T) {
	limitDetail := fmt.Sprintf("這台機器已有 %d 個開啟中的終端，已達上限。請先關閉其中一個終端的分頁，再回到機器頁開啟。", store.MaxOpenAgentSessionsPerMachine)
	incomplete := "開啟終端的請求不完整。請重新整理機器頁再開啟。"

	t.Run("no link", func(t *testing.T) {
		s, st := newServer(t)
		machineID := assignedTerminalMachine(t, st, "samplehub1")
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm("session-unlinked", "key-unlinked"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusConflict,
			"這台的終端連線目前沒有接上 Hub。請確認機器上的 agent 與 bat-server 都在執行，再回到機器頁開啟。")
		assertNoTerminalLedger(t, st)
	})
	t.Run("nil links", func(t *testing.T) {
		s, st := newServer(t)
		machineID := assignedTerminalMachine(t, st, "samplehub1")
		s.SetTerminalLinks(nil)
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm("session-nil-link", "key-nil-link"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusConflict,
			"這台的終端連線目前沒有接上 Hub。請確認機器上的 agent 與 bat-server 都在執行，再回到機器頁開啟。")
		assertNoTerminalLedger(t, st)
	})
	t.Run("link false", func(t *testing.T) {
		s, st := newServer(t)
		machineID := assignedTerminalMachine(t, st, "samplehub1")
		s.SetTerminalLinks(agentlink.New())
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm("session-false-link", "key-false-link"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusConflict,
			"這台的終端連線目前沒有接上 Hub。請確認機器上的 agent 與 bat-server 都在執行，再回到機器頁開啟。")
		assertNoTerminalLedger(t, st)
	})
	t.Run("machine not on roster", func(t *testing.T) {
		s, st := newServer(t)
		const machineID = "missing-machine"
		attachTerminalLink(t, s, machineID)
		const sessionID = "session-missing-machine"
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm(sessionID, "key-missing-machine"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusNotFound, "名冊上沒有這台機器。")
		if strings.Contains(rec.Body.String(), "找不到 machine") {
			t.Fatalf("page copied the ledger detail:\n%s", rec.Body.String())
		}
		assertTerminalOpenAudit(t, st, terminalOpenAuditWant(t, s, machineID, sessionID, "key-missing-machine", false, "找不到 machine "+machineID))
		if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 0 {
			t.Fatalf("missing machine wrote %d sessions", got)
		}
	})
	t.Run("retired", func(t *testing.T) {
		s, st := newServer(t)
		machineID := assignedTerminalMachine(t, st, "samplehub1")
		if err := st.RetireMachine(machineID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		attachTerminalLink(t, s, machineID)
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm("session-retired", "key-retired"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusConflict, "這台機器已退役，不能開啟終端。")
	})
	t.Run("unassigned", func(t *testing.T) {
		s, st := newServer(t)
		machineID := enroll(t, st, "samplehub1", time.Now().UTC())
		attachTerminalLink(t, s, machineID)
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm("session-unassigned", "key-unassigned"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusForbidden, "這台機器沒有指派使用者，不能開啟終端。")
		if strings.Contains(rec.Body.String(), "不能開啟 session") {
			t.Fatalf("page copied the ledger detail:\n%s", rec.Body.String())
		}
	})
	t.Run("different assigned user", func(t *testing.T) {
		s, st := newServer(t)
		machineID := enroll(t, st, "samplehub1", time.Now().UTC())
		assignTerminalUser(t, st, machineID, "99", "other@example.com")
		attachTerminalLink(t, s, machineID)
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm("session-other-user", "key-other-user"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusForbidden, "只有這台機器的指派使用者可以開啟它的終端。")
	})
	t.Run("no tailnet user", func(t *testing.T) {
		s, st := newServer(t)
		machineID := assignedTerminalMachine(t, st, "samplehub1")
		attachTerminalLink(t, s, machineID)
		principal := terminalOpenPrincipal()
		principal.TailnetUserID = ""
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm("session-no-user", "key-no-user"), &principal)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusForbidden, "這個連線來源沒有 tailnet 使用者身分，不能開啟終端。")
	})
	t.Run("limit reached", func(t *testing.T) {
		s, st := newServer(t)
		machineID := assignedTerminalMachine(t, st, "samplehub1")
		attachTerminalLink(t, s, machineID)
		for i := 0; i < store.MaxOpenAgentSessionsPerMachine; i++ {
			sessionID := fmt.Sprintf("slot-%02d", i)
			if _, err := st.OpenAgentSession(store.OpenAgentSessionRequest{
				SessionID: sessionID, MachineID: machineID,
				OperatorTailnetUserID: terminalOpenUserID, OperatorTailnetUserLogin: terminalOpenLogin,
				IdempotencyKey: "fill-" + sessionID, RequestDigest: "sha256:fill-" + sessionID,
				Audit: store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
			}); err != nil {
				t.Fatal(err)
			}
		}
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm("session-over-limit", "key-over-limit"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusConflict, limitDetail)
		if strings.Contains(rec.Body.String(), "目前無法開啟終端") {
			t.Fatalf("limit reached used the generic failure:\n%s", rec.Body.String())
		}
	})
	t.Run("session id already used", func(t *testing.T) {
		s, st := newServer(t)
		machineID := assignedTerminalMachine(t, st, "samplehub1")
		attachTerminalLink(t, s, machineID)
		sessionID := "session-used"
		first := postTerminalOpen(t, s, machineID, terminalOpenForm(sessionID, "key-used-first"), nil)
		assertTerminalOpenRedirect(t, first, machineID, sessionID)
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm(sessionID, "key-used-second"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusConflict, "這次開啟已經用過。請重新整理機器頁再開啟。")
		if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 1 {
			t.Fatalf("reused session id wrote %d sessions", got)
		}
	})
	t.Run("closed replay", func(t *testing.T) {
		s, st := newServer(t)
		machineID := assignedTerminalMachine(t, st, "samplehub1")
		attachTerminalLink(t, s, machineID)
		sessionID := "session-closed-replay"
		form := terminalOpenForm(sessionID, "key-closed-replay")
		opened := postTerminalOpen(t, s, machineID, form, nil)
		assertTerminalOpenRedirect(t, opened, machineID, sessionID)
		if _, err := st.CloseAgentSession(store.CloseAgentSessionRequest{
			SessionID: sessionID, MachineID: machineID,
			OperatorTailnetUserID: terminalOpenUserID, OperatorTailnetUserLogin: terminalOpenLogin,
			Reason: "operator 離開終端", IdempotencyKey: "close-closed-replay",
			RequestDigest: "sha256:close-closed-replay",
			Audit:         store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
		}); err != nil {
			t.Fatal(err)
		}
		rec := postTerminalOpen(t, s, machineID, form, nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusConflict, "這次開啟已經用過，它開的終端已結束。請重新整理機器頁再開啟。")
		if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 1 {
			t.Fatalf("closed replay wrote %d sessions", got)
		}
	})
	for _, test := range []struct {
		name, sessionID, key string
	}{
		{name: "missing key", sessionID: "session-missing-key", key: ""},
		{name: "blank key", sessionID: "session-blank-key", key: "  "},
		{name: "key too long", sessionID: "session-long-key", key: strings.Repeat("k", 201)},
		{name: "invalid session id", sessionID: strings.Repeat("s", sessionid.MaxLength+1), key: "key-invalid-session"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, st := newServer(t)
			machineID := assignedTerminalMachine(t, st, "samplehub1")
			attachTerminalLink(t, s, machineID)
			rec := postTerminalOpen(t, s, machineID, terminalOpenForm(test.sessionID, test.key), nil)
			assertTerminalOpenRefusal(t, rec, machineID, http.StatusBadRequest, incomplete)
			if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 0 {
				t.Fatalf("%s wrote %d sessions", test.name, got)
			}
		})
	}
	t.Run("idempotency conflict", func(t *testing.T) {
		s, st := newServer(t)
		machineID := assignedTerminalMachine(t, st, "samplehub1")
		attachTerminalLink(t, s, machineID)
		first := postTerminalOpen(t, s, machineID, terminalOpenForm("session-conflict-a", "key-conflict"), nil)
		assertTerminalOpenRedirect(t, first, machineID, "session-conflict-a")
		rec := postTerminalOpen(t, s, machineID, terminalOpenForm("session-conflict-b", "key-conflict"), nil)
		assertTerminalOpenRefusal(t, rec, machineID, http.StatusBadRequest, incomplete)
		if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM agent_sessions WHERE session_id=?`, "session-conflict-b"); got != 0 {
			t.Fatal("conflicting open wrote a second session")
		}
	})
}

func TestTerminalOpenUnexpectedFailureStaysOnTheMachinePage(t *testing.T) {
	s, st := newServer(t)
	machineID := assignedTerminalMachine(t, st, "samplehub1")
	attachTerminalLink(t, s, machineID)
	if _, err := st.DB().Exec(`DROP TABLE agent_sessions`); err != nil {
		t.Fatal(err)
	}
	const sessionID = "session-do-not-log"
	const key = "key-do-not-log"
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	rec := postTerminalOpen(t, s, machineID, terminalOpenForm(sessionID, key), nil)
	assertTerminalOpenRefusal(t, rec, machineID, http.StatusInternalServerError, "目前無法開啟終端。請回到機器頁再試一次。")
	body := rec.Body.String()
	for _, leaked := range []string{sessionID, key, "agent_sessions", "store:"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("failure page contains %q", leaked)
		}
	}
	logged := logs.String()
	if !strings.Contains(logged, "operator terminal open failed machine="+machineID) {
		t.Fatalf("failure was not logged: %q", logged)
	}
	for _, leaked := range []string{sessionID, key} {
		if strings.Contains(logged, leaked) {
			t.Fatalf("log contains %q: %s", leaked, logged)
		}
	}
}

func TestTerminalOpenCopyUsesCurrentStatesAndActions(t *testing.T) {
	for _, path := range []string{"terminal_open.go", "templates/terminal.html"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"暫時", "尚未實作", "未接線", "遷移", "防禦", "免責", "fallback", "placeholder=", "disabled"} {
			if strings.Contains(string(raw), banned) {
				t.Fatalf("%s 含禁用文案 %s", path, banned)
			}
		}
	}
}

func assignedTerminalMachine(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	machineID := enroll(t, st, name, time.Now().UTC())
	assignTerminalUser(t, st, machineID, terminalOpenUserID, terminalOpenLogin)
	return machineID
}

func assignTerminalUser(t *testing.T, st *store.Store, machineID, userID, login string) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE machine_registry SET assigned_user_id=?, assigned_user_login=? WHERE machine_id=?`,
		userID, login, machineID); err != nil {
		t.Fatal(err)
	}
}

func attachTerminalLink(t *testing.T, s *Server, machineID string) {
	t.Helper()
	links := agentlink.New()
	if _, err := links.Attach(machineID, openTerminalLink{}); err != nil {
		t.Fatal(err)
	}
	s.SetTerminalLinks(links)
}

func terminalOpenForm(sessionID, key string) url.Values {
	return url.Values{"session_id": {sessionID}, "idempotency_key": {key}}
}

func terminalOpenPrincipal() operatorauth.Principal {
	names, _ := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	return operatorauth.Principal{
		SourceAddr: "100.64.200.2", NodeStableID: "node-stable-1",
		DeviceName: "cnoderidge-ai1", TailnetUserID: terminalOpenUserID,
		TailnetUserLogin: terminalOpenLogin, AuthMethod: operatorauth.AuthMethodLocalAPI,
		AuthorizedCapability: "example.com/cap/clawctl-operate",
		GrantedCapabilities: operatorauth.CapabilityNames{
			View: names.View, Operate: names.Operate, Admin: names.Admin,
		},
	}
}

func postTerminalOpen(t *testing.T, s *Server, machineID string, form url.Values, principal *operatorauth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/machines/"+machineID+"/terminals", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", terminalOpenUserAgent)
	req.RemoteAddr = "100.64.200.2:54321"
	if principal == nil {
		copied := terminalOpenPrincipal()
		principal = &copied
	}
	mux.ServeHTTP(rec, operatorauth.WithPrincipal(req, *principal))
	return rec
}

func assertTerminalOpenRedirect(t *testing.T, rec *httptest.ResponseRecorder, machineID, sessionID string) {
	t.Helper()
	want := "/machines/" + machineID + "/terminals/" + sessionID
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
		t.Fatalf("status=%d location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", rec.Header().Get("Cache-Control"))
	}
}

func assertTerminalOpenRefusal(t *testing.T, rec *httptest.ResponseRecorder, machineID string, status int, detail string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status=%d want %d body=%s", rec.Code, status, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", rec.Header().Get("Cache-Control"))
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("refusal redirected to %s", loc)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<h1>"+terminalOpenRefused+"</h1>") || !strings.Contains(body, detail) {
		t.Fatalf("refusal missing headline or %q:\n%s", detail, body)
	}
	back := `href="/machines/` + machineID + `"`
	if !strings.Contains(body, back) || !strings.Contains(body, "回那台機器") {
		t.Fatalf("back link missing %s:\n%s", back, body)
	}
	if !strings.Contains(body, "</html>") {
		t.Fatalf("refusal was not the action page:\n%s", body)
	}
}

func assertOpenTerminalRow(t *testing.T, st *store.Store, machineID, sessionID, userID string) {
	t.Helper()
	var gotMachine, gotUser string
	var closedAt *string
	if err := st.DB().QueryRow(`SELECT machine_id, operator_tailnet_user_id, closed_at
		FROM agent_sessions WHERE session_id=?`, sessionID).Scan(&gotMachine, &gotUser, &closedAt); err != nil {
		t.Fatal(err)
	}
	if gotMachine != machineID || gotUser != userID || closedAt != nil {
		t.Fatalf("session machine=%s user=%s closed=%v", gotMachine, gotUser, closedAt)
	}
	if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 1 {
		t.Fatalf("open sessions=%d", got)
	}
}

func assertNoTerminalLedger(t *testing.T, st *store.Store) {
	t.Helper()
	if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM agent_sessions`); got != 0 {
		t.Fatalf("no-link refusal wrote %d sessions", got)
	}
	if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action=?`, store.AuditAgentSessionOpen); got != 0 {
		t.Fatalf("no-link refusal wrote %d open audits", got)
	}
	if got := countTerminalRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`); got != 0 {
		t.Fatalf("no-link refusal wrote %d idempotency rows", got)
	}
}

func countTerminalRows(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func terminalOpenAuditWant(t *testing.T, s *Server, machineID, sessionID, key string, ok bool, detail string) store.AuditEntry {
	t.Helper()
	principal := terminalOpenPrincipal()
	machine, err := s.store.GetMachine(machineID)
	subject := machineID
	if err == nil {
		subject = machine.DisplayName
	}
	return store.AuditEntry{
		Action: store.AuditAgentSessionOpen, MachineID: machineID, Subject: subject,
		Reason: "session " + sessionID, IdempotencyKey: key,
		RequestDigest: operator.AgentSessionOpenSemanticDigest(operator.AgentSessionOpenRequest{
			SessionID: sessionID, MachineID: machineID, Principal: principal,
		}),
		SourceAddr: principal.SourceAddr, WhoNode: principal.DeviceName, WhoUser: principal.TailnetUserLogin,
		UserAgent: terminalOpenUserAgent, AuthSubject: "tailscale-user:" + principal.TailnetUserID,
		AuthNodeID: principal.NodeStableID, AuthCapability: principal.AuthorizedCapability,
		AuthMethod: principal.AuthMethod, AuthDecision: string(operatorauth.Authorized),
		SourceKind: operator.SourceKindWeb, OK: ok, Detail: detail,
	}
}

func assertTerminalOpenAudit(t *testing.T, st *store.Store, want store.AuditEntry) {
	t.Helper()
	entries, err := st.Audit("", 50)
	if err != nil {
		t.Fatal(err)
	}
	var got []store.AuditEntry
	for _, entry := range entries {
		if entry.Action == store.AuditAgentSessionOpen {
			got = append(got, entry)
		}
	}
	if len(got) != 1 {
		t.Fatalf("agent-session-open audits=%d %+v", len(got), got)
	}
	got[0].ID, got[0].At = 0, time.Time{}
	if got[0] != want {
		t.Fatalf("audit=\n%+v\nwant=\n%+v", got[0], want)
	}
}
