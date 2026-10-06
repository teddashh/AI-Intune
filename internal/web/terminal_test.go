package web

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/sessionid"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/web/terminalassets"
)

func TestTerminalPageScriptHashesMatchEmittedBytes(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "samplehub1", time.Now().UTC())
	openWebTerminalSession(t, st, machineID, "42", "operator@example.com", "session-owned")

	rec := terminalResponse(t, s, "/machines/"+machineID+"/terminals/session-owned", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "</html>") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q", rec.Header().Get("Cache-Control"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "samplehub1 的終端") ||
		!strings.Contains(body, "正在連線這個終端，目前不能輸入。") {
		t.Fatalf("page did not say which terminal is opening:\n%s", body)
	}
	heading := strings.Index(body, "<h1>")
	impact := strings.Index(body, `id="terminal-impact"`)
	status := strings.Index(body, `id="terminal-status"`)
	if heading < 0 || impact < heading || status < impact ||
		!strings.Contains(body, "關閉或重新整理這一頁，或連線中斷，都會結束這個終端裡正在執行的工作。") {
		t.Fatalf("impact line is not between the heading and the status:\n%s", body)
	}
	scripts := htmlScriptContents(rec.Body.String())
	wantBodies := [][]byte{terminalassets.XTermJS, terminalassets.FitJS, terminalassets.PageJS}
	wantHashes := []string{terminalassets.XTermJSHash, terminalassets.FitJSHash, terminalassets.PageJSHash}
	if len(scripts) != len(wantBodies) {
		t.Fatalf("rendered script count=%d", len(scripts))
	}
	for i, script := range scripts {
		sum := sha256.Sum256([]byte(script))
		got := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
		if got != wantHashes[i] {
			t.Fatalf("script %d hash=%s, package advertises %s", i, got, wantHashes[i])
		}
		if script != string(wantBodies[i]) {
			t.Fatalf("script %d emitted %d bytes, embedded %d", i, len(script), len(wantBodies[i]))
		}
	}
	if !htmlStyleContains(rec.Body.String(), string(terminalassets.XTermCSS)) {
		t.Fatal("rendered style block is not the embedded CSS")
	}
}

func TestTerminalPageNotFoundDoesNotDescribeTheMiss(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	owned := enroll(t, st, "samplehub1", now)
	other := enroll(t, st, "sampleagent1", now)
	openWebTerminalSession(t, st, owned, "42", "operator@example.com", "session-owned")
	openWebTerminalSession(t, st, other, "42", "operator@example.com", "session-other-machine")
	openWebTerminalSession(t, st, owned, "99", "other@example.com", "session-other-user")
	openWebTerminalSession(t, st, owned, "7", "operator@example.com", "session-same-login")
	openWebTerminalSession(t, st, owned, "42", "operator@example.com", "session-closed")
	if _, err := st.CloseAgentSession(store.CloseAgentSessionRequest{
		SessionID: "session-closed", MachineID: owned,
		OperatorTailnetUserID: "42", OperatorTailnetUserLogin: "operator@example.com",
		Reason: "operator 離開終端", IdempotencyKey: "close-session-closed",
		RequestDigest: "sha256:close-session-closed",
		Audit:         store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
	}); err != nil {
		t.Fatal(err)
	}

	// The roster sentence is true only when GetMachine misses. Grammar, a
	// missing principal, a session miss, and the ownership/closed re-check
	// all name a missing session, and they name it with the same sentence.
	for _, test := range []struct {
		name, path, body string
	}{
		{name: "unknown machine", path: "/machines/missing/terminals/session-owned", body: "名冊上沒有這台機器。"},
		{name: "invalid session", path: "/machines/" + owned + "/terminals/" + strings.Repeat("s", sessionid.MaxLength+1), body: "找不到這個終端工作階段。請回到機器頁重新開啟。"},
		{name: "other machine", path: "/machines/" + owned + "/terminals/session-other-machine", body: "找不到這個終端工作階段。請回到機器頁重新開啟。"},
		{name: "closed", path: "/machines/" + owned + "/terminals/session-closed", body: "找不到這個終端工作階段。請回到機器頁重新開啟。"},
		{name: "different tailnet user id", path: "/machines/" + owned + "/terminals/session-other-user", body: "找不到這個終端工作階段。請回到機器頁重新開啟。"},
		{name: "same login different id", path: "/machines/" + owned + "/terminals/session-same-login", body: "找不到這個終端工作階段。請回到機器頁重新開啟。"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := terminalResponse(t, s, test.path, true)
			assertTerminalNotFound(t, rec, test.body)
		})
	}

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	s.Routes(mux)
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/machines/"+owned+"/terminals/session-owned", nil))
	assertTerminalNotFound(t, rec, "找不到這個終端工作階段。請回到機器頁重新開啟。")
}

func TestTerminalPageFocusesWhenItSaysInputIsAccepted(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "samplehub1", time.Now().UTC())
	openWebTerminalSession(t, st, machineID, "42", "operator@example.com", "session-owned")
	rec := terminalResponse(t, s, "/machines/"+machineID+"/terminals/session-owned", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var src string
	for _, script := range htmlScriptContents(rec.Body.String()) {
		if strings.Contains(script, `message.type === "ready"`) {
			src = script
			break
		}
	}
	if src == "" {
		t.Fatal("emitted page has no ready path")
	}
	openAt := strings.Index(src, "term.open(container)")
	readyAt := strings.Index(src, `message.type === "ready"`)
	outputAt := strings.Index(src, `message.type === "output"`)
	if openAt < 0 || readyAt < openAt || outputAt < readyAt {
		t.Fatal("emitted page script is missing construction or the ready path")
	}
	if strings.Contains(src[:readyAt], "term.focus()") {
		t.Fatal("term.focus() runs at construction, before the ready path")
	}
	readyPath := src[readyAt:outputAt]
	if strings.Count(src, "term.focus()") != 1 || strings.Count(readyPath, "term.focus()") != 1 {
		t.Fatalf("term.focus() calls=%d, ready path=%d", strings.Count(src, "term.focus()"), strings.Count(readyPath, "term.focus()"))
	}
	focusAt := strings.Index(readyPath, "term.focus()")
	interactiveAt := strings.Index(readyPath, "interactive = true")
	showAt := strings.Index(readyPath, "show(openedMessage)")
	if interactiveAt < 0 || focusAt < interactiveAt || showAt < focusAt {
		t.Fatal("term.focus() is not between becoming interactive and stating that input is accepted")
	}
	if !strings.Contains(src, `while (end > offset && (bytes[end] & 0xC0) === 0x80)`) {
		t.Fatal("emitted page does not keep an input frame inside a UTF-8 code point")
	}
	unload := strings.Index(src, `addEventListener("beforeunload"`)
	if unload < 0 {
		t.Fatal("emitted page does not confirm leaving the terminal")
	}
	unloadHandler := src[unload:]
	closeAt := strings.Index(unloadHandler, "});")
	if closeAt < 0 {
		t.Fatal("emitted beforeunload handler does not close")
	}
	unloadHandler = unloadHandler[:closeAt]
	guard := strings.Index(unloadHandler, "if (!interactive)")
	prevent := strings.Index(unloadHandler, "event.preventDefault()")
	if guard < 0 || prevent < guard || !strings.Contains(unloadHandler, `event.returnValue = ""`) {
		t.Fatal("emitted beforeunload confirm is not conditioned on interactive")
	}
}

func openWebTerminalSession(t *testing.T, st *store.Store, machineID, userID, login, sessionID string) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE machine_registry SET assigned_user_id=?, assigned_user_login=? WHERE machine_id=?`,
		userID, login, machineID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.OpenAgentSession(store.OpenAgentSessionRequest{
		SessionID: sessionID, MachineID: machineID,
		OperatorTailnetUserID: userID, OperatorTailnetUserLogin: login,
		IdempotencyKey: "open-" + sessionID, RequestDigest: "sha256:" + sessionID,
		Audit: store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
	}); err != nil {
		t.Fatal(err)
	}
}

func terminalResponse(t *testing.T, s *Server, path string, verified bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if verified {
		req = verifiedWebRequest(req, "example.com/cap/clawctl-operate")
	}
	mux.ServeHTTP(rec, req)
	return rec
}

func assertTerminalNotFound(t *testing.T, rec *httptest.ResponseRecorder, body string) {
	t.Helper()
	if rec.Code != http.StatusNotFound || rec.Body.String() != body+"\n" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	for _, leaked := range []string{"session", "UTF", "128", "grammar", "tailnet", "login"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(leaked)) {
			t.Fatalf("not-found body contains %q: %q", leaked, rec.Body.String())
		}
	}
}

func htmlScriptContents(html string) []string {
	var out []string
	rest := html
	for {
		start := strings.Index(rest, "<script>")
		if start < 0 {
			return out
		}
		rest = rest[start+len("<script>"):]
		end := strings.Index(rest, "</script>")
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		rest = rest[end+len("</script>"):]
	}
}

func htmlStyleContains(html, css string) bool {
	rest := html
	for {
		start := strings.Index(rest, "<style>")
		if start < 0 {
			return false
		}
		rest = rest[start+len("<style>"):]
		end := strings.Index(rest, "</style>")
		if end < 0 {
			return false
		}
		if rest[:end] == css {
			return true
		}
		rest = rest[end+len("</style>"):]
	}
}
