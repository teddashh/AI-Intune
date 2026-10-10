package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/agentpty"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/web"
	"github.com/teddashh/AI-Intune/internal/web/terminalassets"
)

const operatorTerminalUser = "user-1"

func useOperatorTerminalSettings(t *testing.T, settings operatorTerminalSettings) {
	t.Helper()
	operatorTerminalSettingsMu.Lock()
	previous := operatorTerminalSettingsOverride
	operatorTerminalSettingsOverride = &settings
	operatorTerminalSettingsMu.Unlock()
	t.Cleanup(func() {
		operatorTerminalSettingsMu.Lock()
		operatorTerminalSettingsOverride = previous
		operatorTerminalSettingsMu.Unlock()
	})
}

type operatorTerminalAuth struct {
	mu     sync.Mutex
	userID string
	calls  int
	later  func(call int, r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision, bool)
}

func (a *operatorTerminalAuth) Authorize(r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision) {
	a.mu.Lock()
	a.calls++
	call := a.calls
	userID := a.userID
	later := a.later
	a.mu.Unlock()
	if later != nil {
		if req, decision, handled := later(call, r, permission); handled {
			return req, decision
		}
	}
	principal := operatorTerminalPrincipal(userID, permission)
	return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
		Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
	}
}

func (a *operatorTerminalAuth) setUser(userID string) {
	a.mu.Lock()
	a.userID = userID
	a.mu.Unlock()
}

func operatorTerminalPrincipal(userID string, permission operatorauth.Permission) operatorauth.Principal {
	principal := boundaryPrincipal(permission)
	principal.SourceAddr = "127.0.0.1"
	principal.TailnetUserID = userID
	return principal
}

type operatorTerminalLink struct {
	mu       sync.Mutex
	frames   []agentrelay.Downstream
	busyLeft map[agentrelay.DownstreamType]int
	busyN    int
	onSend   func(agentrelay.Downstream)
	notify   chan struct{}
}

func newOperatorTerminalLink() *operatorTerminalLink {
	return &operatorTerminalLink{
		busyLeft: map[agentrelay.DownstreamType]int{},
		notify:   make(chan struct{}, 64),
	}
}

func (l *operatorTerminalLink) Send(frame agentrelay.Downstream) error {
	l.mu.Lock()
	if left := l.busyLeft[frame.Type]; left > 0 {
		l.busyLeft[frame.Type] = left - 1
		l.busyN++
		l.mu.Unlock()
		l.signal()
		return agentlink.ErrLinkBusy
	}
	copied := frame
	if frame.Data != nil {
		copied.Data = append([]byte(nil), frame.Data...)
	}
	l.frames = append(l.frames, copied)
	hook := l.onSend
	l.mu.Unlock()
	if hook != nil {
		hook(copied)
	}
	l.signal()
	return nil
}

func (l *operatorTerminalLink) Close(string) {}

func (l *operatorTerminalLink) signal() {
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *operatorTerminalLink) snapshot() []agentrelay.Downstream {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]agentrelay.Downstream, len(l.frames))
	copy(out, l.frames)
	return out
}

func (l *operatorTerminalLink) await(t *testing.T, n int) []agentrelay.Downstream {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		got := l.snapshot()
		if len(got) >= n {
			return got
		}
		select {
		case <-l.notify:
		case <-deadline:
			t.Fatalf("link has %d frames, want %d: %#v", len(l.snapshot()), n, l.snapshot())
		}
	}
}

func (l *operatorTerminalLink) awaitBusy(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		l.mu.Lock()
		got := l.busyN
		l.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-l.notify:
		case <-deadline:
			t.Fatalf("busy attempts = %d, want %d", got, n)
		}
	}
}

type operatorTerminalFixture struct {
	t         *testing.T
	hub       *hub
	ui        *web.Server
	machineID string
	link      *operatorTerminalLink
	auth      *operatorTerminalAuth
	server    *httptest.Server
	authority string
	secrets   []string
}

func newOperatorTerminalFixture(t *testing.T) *operatorTerminalFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, enrollmentToken, err := st.CreateEnrollTokenFor("opterm-machine", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineID, _, err := st.RedeemEnrollToken(enrollmentToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, Hostname: "opterm-machine",
		UnixUser: "tester", OS: "linux", Arch: "amd64",
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET assigned_user_id=?, assigned_user_login=? WHERE machine_id=?`,
		operatorTerminalUser, "operator@example.com", machineID); err != nil {
		t.Fatal(err)
	}
	link := newOperatorTerminalLink()
	h := &hub{store: st, agentLinks: agentlink.New()}
	if _, err := h.agentLinks.Attach(machineID, link); err != nil {
		t.Fatal(err)
	}
	auth := &operatorTerminalAuth{userID: operatorTerminalUser}
	f := &operatorTerminalFixture{t: t, hub: h, machineID: machineID, link: link, auth: auth}
	f.captureLogs()
	f.server, f.authority, f.ui = startOperatorTerminalServer(t, h, auth)
	f.watch(machineID)
	return f
}

func startOperatorTerminalServer(t *testing.T, h *hub, auth operatorRequestAuthorizer) (*httptest.Server, string, *web.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	authority := ln.Addr().String()
	ui, err := web.New(h.store, "hub")
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	handler, err := newHubHTTPHandler(h, ui, auth, authority)
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	// httptest.Server.Close does not wait for hijacked websocket handlers.
	// Join them before the fixture restores log output and closes its DB.
	var handlers sync.WaitGroup
	tracked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		handler.ServeHTTP(w, r)
	})
	server := &httptest.Server{Listener: ln, Config: &http.Server{Handler: tracked}}
	server.Start()
	t.Cleanup(func() {
		server.Close()
		handlers.Wait()
	})
	return server, authority, ui
}

func (f *operatorTerminalFixture) captureLogs() {
	f.t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	f.t.Cleanup(func() {
		log.SetOutput(previous)
		text := buf.String()
		for _, secret := range f.secrets {
			if secret != "" && strings.Contains(text, secret) {
				f.t.Errorf("log contains %q\n%s", secret, text)
			}
		}
	})
}

func (f *operatorTerminalFixture) watch(secret string) {
	if secret == "" {
		return
	}
	f.secrets = append(f.secrets, secret)
}

func (f *operatorTerminalFixture) watchBytes(raw []byte) {
	f.watch(string(raw))
	f.watch(base64.StdEncoding.EncodeToString(raw))
}

func (f *operatorTerminalFixture) openSession(sessionID string) {
	f.t.Helper()
	if _, err := f.hub.store.OpenAgentSession(store.OpenAgentSessionRequest{
		SessionID: sessionID, MachineID: f.machineID,
		OperatorTailnetUserID: operatorTerminalUser, OperatorTailnetUserLogin: "operator@example.com",
		IdempotencyKey: "open-" + sessionID, RequestDigest: "sha256:" + sessionID,
		Audit: store.AuditEntry{SourceAddr: "127.0.0.1", AuthMethod: "test"},
	}); err != nil {
		f.t.Fatal(err)
	}
	f.watch(sessionID)
}

func (f *operatorTerminalFixture) origin() http.Header {
	header := http.Header{}
	header.Set("Origin", "http://"+f.authority)
	return header
}

func (f *operatorTerminalFixture) dial(sessionID string, header http.Header, protocols []string) (*websocket.Conn, *http.Response, error) {
	if header == nil {
		header = http.Header{}
	}
	endpoint := "ws://" + f.authority + "/machines/" + f.machineID + "/terminals/" + sessionID + "/socket"
	conn, resp, err := websocket.Dial(context.Background(), endpoint, &websocket.DialOptions{
		HTTPHeader: header, Subprotocols: protocols,
	})
	if conn != nil {
		// Registered after server cleanup, so sockets close before joining
		// their handlers, including when a test fails before its own close.
		f.t.Cleanup(func() { _ = conn.CloseNow() })
	}
	return conn, resp, err
}

func (f *operatorTerminalFixture) connect(sessionID string) *websocket.Conn {
	f.t.Helper()
	f.openSession(sessionID)
	conn, resp, err := f.dial(sessionID, f.origin(), []string{operatorTerminalSubprotocol})
	if err != nil {
		f.t.Fatalf("dial %s: %v body=%s", sessionID, err, responseBody(resp))
	}
	return conn
}

func (f *operatorTerminalFixture) session(sessionID string) (open bool, reason string) {
	f.t.Helper()
	var closedAt, closeReason sql.NullString
	err := f.hub.store.DB().QueryRow(`SELECT closed_at, close_reason FROM agent_sessions WHERE session_id=?`, sessionID).Scan(&closedAt, &closeReason)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	if !closedAt.Valid {
		return true, ""
	}
	return false, closeReason.String
}

func (f *operatorTerminalFixture) awaitReason(sessionID, reason string) {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		open, got := f.session(sessionID)
		if !open && got == reason {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	open, got := f.session(sessionID)
	f.t.Fatalf("session open=%t reason=%q, want %q", open, got, reason)
}

func (f *operatorTerminalFixture) awaitRoute(sessionID string) {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.hasRoute(sessionID) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("route was not registered")
}

func (f *operatorTerminalFixture) hasRoute(sessionID string) bool {
	if f.hub.agentLinks == nil {
		return false
	}
	_, ok := f.hub.agentLinks.Snapshot().Sessions[sessionID]
	return ok
}

func (f *operatorTerminalFixture) deliver(sessionID string, frame agentrelay.Upstream) {
	f.t.Helper()
	frame.Session = sessionID
	if err := f.hub.agentLinks.Deliver(f.machineID, frame); err != nil {
		f.t.Fatalf("Deliver: %v", err)
	}
}

func responseBody(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return string(body)
}

func writePage(t *testing.T, conn *websocket.Conn, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func writePageJSON(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writePage(t, conn, payload)
}

func readPage(t *testing.T, conn *websocket.Conn) (map[string]any, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	kind, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	if kind != websocket.MessageText {
		t.Fatalf("page message type %v", kind)
	}
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("page json: %v %s", err, payload)
	}
	return msg, payload
}

func assertErrorFrame(t *testing.T, conn *websocket.Conn, reason string) []byte {
	t.Helper()
	msg, raw := readPage(t, conn)
	if msg["type"] != "error" || msg["reason"] != reason {
		t.Fatalf("frame = %#v, want error %q", msg, reason)
	}
	return raw
}

func assertNoRoute(t *testing.T, f *operatorTerminalFixture, sessionID string) {
	t.Helper()
	if f.hasRoute(sessionID) {
		t.Fatalf("route %s still registered", sessionID)
	}
}

func assertRefusal(t *testing.T, resp *http.Response, err error, status int, sentence string) {
	t.Helper()
	if err == nil {
		t.Fatal("handshake succeeded")
	}
	if resp == nil || resp.StatusCode != status || responseBody(resp) != sentence+"\n" {
		body := ""
		code := 0
		if resp != nil {
			code = resp.StatusCode
			body = responseBody(resp)
		}
		t.Fatalf("status=%d body=%q err=%v, want %d %q", code, body, err, status, sentence)
	}
}

func (f *operatorTerminalFixture) ready(conn *websocket.Conn, sessionID string) {
	f.t.Helper()
	f.deliver(sessionID, agentrelay.Upstream{Type: agentrelay.UpstreamReady})
	msg, _ := readPage(f.t, conn)
	if msg["type"] != "ready" {
		f.t.Fatalf("frame = %#v, want ready", msg)
	}
}

func TestOperatorTerminalOriginMatchesPinnedAuthority(t *testing.T) {
	httpReq := httptest.NewRequest(http.MethodGet, "/socket", nil)
	httpReq.Host = "127.0.0.1:80"
	httpReq.Header.Set("Origin", "http://127.0.0.1")
	if !operatorTerminalOriginAllowed(httpReq, "127.0.0.1:80") {
		t.Fatal("omitted http port did not match the pinned authority")
	}
	httpsReq := httptest.NewRequest(http.MethodGet, "/socket", nil)
	httpsReq.Host = "127.0.0.1:443"
	httpsReq.TLS = &tls.ConnectionState{}
	httpsReq.Header.Set("Origin", "https://127.0.0.1")
	if !operatorTerminalOriginAllowed(httpsReq, "127.0.0.1:443") {
		t.Fatal("omitted https port did not match the pinned authority")
	}
	spelled := httptest.NewRequest(http.MethodGet, "/socket", nil)
	spelled.Host = "127.0.0.1:08787"
	spelled.Header.Set("Origin", "http://127.0.0.1:8787")
	if !operatorTerminalOriginAllowed(spelled, "127.0.0.1:8787") {
		t.Fatal("canonical origin did not match the pinned authority")
	}
	wrongScheme := httptest.NewRequest(http.MethodGet, "/socket", nil)
	wrongScheme.Host = "127.0.0.1:8787"
	wrongScheme.Header.Set("Origin", "https://127.0.0.1:8787")
	if operatorTerminalOriginAllowed(wrongScheme, "127.0.0.1:8787") {
		t.Fatal("https origin was accepted on a cleartext request")
	}
}

func TestOperatorTerminalHandshakeRefusals(t *testing.T) {
	f := newOperatorTerminalFixture(t)
	f.openSession("opterm-refuse")
	beforeOpen, beforeReason := f.session("opterm-refuse")

	cases := []struct {
		name    string
		session string
		header  http.Header
		status  int
		body    string
		setup   func()
	}{
		{name: "no origin", session: "opterm-refuse", header: http.Header{}, status: http.StatusForbidden, body: operatorTerminalOriginRefused},
		{name: "two origins", session: "opterm-refuse", status: http.StatusForbidden, body: operatorTerminalOriginRefused, setup: func() {}},
		{name: "wrong scheme", session: "opterm-refuse", status: http.StatusForbidden, body: operatorTerminalOriginRefused},
		{name: "foreign host", session: "opterm-refuse", status: http.StatusForbidden, body: operatorTerminalOriginRefused},
		{name: "closed row", session: "opterm-closed", status: http.StatusNotFound, body: operatorTerminalSessionMissing},
		{name: "other operator", session: "opterm-refuse", status: http.StatusNotFound, body: operatorTerminalSessionMissing},
		{name: "malformed session", session: strings.Repeat("s", 129), status: http.StatusNotFound, body: operatorTerminalSessionMissing},
		{name: "no registry", session: "opterm-refuse", status: http.StatusServiceUnavailable, body: operatorTerminalUnavailable},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			header := test.header
			switch test.name {
			case "two origins":
				header = http.Header{}
				header.Add("Origin", "http://"+f.authority)
				header.Add("Origin", "http://"+f.authority)
			case "wrong scheme":
				header = http.Header{}
				header.Set("Origin", "https://"+f.authority)
			case "foreign host":
				header = http.Header{}
				header.Set("Origin", "http://8.8.8.8:8787")
			case "closed row":
				f.openSession("opterm-closed")
				if _, err := f.hub.store.CloseAgentSessionsByID([]string{"opterm-closed"}, store.AgentSessionCloseReasonViewerClosed); err != nil {
					t.Fatal(err)
				}
				header = f.origin()
			case "other operator":
				f.auth.setUser("someone-else")
				t.Cleanup(func() { f.auth.setUser(operatorTerminalUser) })
				header = f.origin()
			case "no registry":
				f.hub.agentLinks = nil
				t.Cleanup(func() {
					f.hub.agentLinks = agentlink.New()
					if _, err := f.hub.agentLinks.Attach(f.machineID, f.link); err != nil {
						t.Error(err)
					}
				})
				header = f.origin()
			default:
				if header == nil {
					header = f.origin()
				}
			}
			_, resp, err := f.dial(test.session, header, []string{operatorTerminalSubprotocol})
			assertRefusal(t, resp, err, test.status, test.body)
			assertNoRoute(t, f, test.session)
			if test.name == "closed row" {
				open, reason := f.session("opterm-closed")
				if open || reason != store.AgentSessionCloseReasonViewerClosed {
					t.Fatalf("closed row open=%t reason=%q", open, reason)
				}
				return
			}
			open, reason := f.session("opterm-refuse")
			if open != beforeOpen || reason != beforeReason {
				t.Fatalf("row open=%t reason=%q, was open=%t reason=%q", open, reason, beforeOpen, beforeReason)
			}
		})
	}
}

func TestOperatorTerminalMissingSubprotocol(t *testing.T) {
	f := newOperatorTerminalFixture(t)
	f.openSession("opterm-proto")
	conn, resp, err := f.dial("opterm-proto", f.origin(), nil)
	if err != nil && conn == nil {
		t.Fatalf("dial: %v body=%s", err, responseBody(resp))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, readErr := conn.Read(ctx)
	if websocket.CloseStatus(readErr) != websocket.StatusPolicyViolation {
		t.Fatalf("close = %v, want policy violation", readErr)
	}
	if f.hasRoute("opterm-proto") {
		t.Fatal("missing subprotocol registered a route")
	}
	open, _ := f.session("opterm-proto")
	if !open {
		t.Fatal("missing subprotocol closed the row")
	}
}

func TestOperatorTerminalHappyPath(t *testing.T) {
	f := newOperatorTerminalFixture(t)
	const sessionID = "opterm-happy-9f3c"
	input := []byte("你好-OPTERM-IN-9f3c2b")
	output := []byte("OPTERM-OUT-9f3c2b")
	more := []byte("OPTERM-OUT-MORE-9f3c2b")
	f.watchBytes(input)
	f.watchBytes(output)
	f.watchBytes(more)
	conn := f.connect(sessionID)
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 100, "rows": 30})
	frames := f.link.await(t, 1)
	if frames[0].Type != agentrelay.DownstreamOpen || frames[0].Cols != 100 || frames[0].Rows != 30 || frames[0].Session != sessionID {
		t.Fatalf("open = %#v", frames[0])
	}
	f.ready(conn, sessionID)
	writePageJSON(t, conn, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString(input)})
	frames = f.link.await(t, 2)
	if frames[1].Type != agentrelay.DownstreamInput || !bytes.Equal(frames[1].Data, input) {
		t.Fatalf("input = %#v", frames[1])
	}
	f.deliver(sessionID, agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Data: output})
	msg, _ := readPage(t, conn)
	if msg["type"] != "output" {
		t.Fatalf("frame = %#v", msg)
	}
	got, err := base64.StdEncoding.DecodeString(msg["data"].(string))
	if err != nil || !bytes.Equal(got, output) {
		t.Fatalf("output = %q %v", got, err)
	}
	f.deliver(sessionID, agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Data: more})
	code := 0
	f.deliver(sessionID, agentrelay.Upstream{Type: agentrelay.UpstreamExit, Code: &code})
	msg, _ = readPage(t, conn)
	got, err = base64.StdEncoding.DecodeString(fmt.Sprint(msg["data"]))
	if msg["type"] != "output" || err != nil || !bytes.Equal(got, more) {
		t.Fatalf("queued output = %#v %q %v", msg, got, err)
	}
	msg, _ = readPage(t, conn)
	exitCode, _ := msg["code"].(float64)
	if msg["type"] != "exit" || exitCode != 0 {
		t.Fatalf("exit = %#v", msg)
	}
	f.awaitReason(sessionID, store.AgentSessionCloseReasonExited)
	assertNoRoute(t, f, sessionID)
	frames = f.link.snapshot()
	var closed bool
	for _, frame := range frames {
		if frame.Type == agentrelay.DownstreamClose && frame.Session == sessionID {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("link frames = %#v, want a close", frames)
	}
}

func TestOperatorTerminalOpensFromTheMachinePage(t *testing.T) {
	f := newOperatorTerminalFixture(t)
	f.ui.SetTerminalLinks(f.hub.agentLinks)
	client := f.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	page := f.terminalPage(client, "/machines/"+f.machineID)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("machine page status=%d body=%s", page.StatusCode, responseBody(page))
	}
	body, err := io.ReadAll(page.Body)
	page.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	form := operatorTerminalPageForm(t, string(body))
	if strings.Contains(form, "target=") {
		t.Fatalf("terminal form sets target:\n%s", form)
	}
	sessionID := operatorTerminalHidden(t, form, "session_id")
	key := operatorTerminalHidden(t, form, "idempotency_key")
	f.watch(sessionID)
	input := []byte("page-in-OPTERM-4b")
	output := []byte("page-out-OPTERM-4b")
	f.watchBytes(input)
	f.watchBytes(output)

	posted := url.Values{"session_id": {sessionID}, "idempotency_key": {key}}
	openReq, err := http.NewRequest(http.MethodPost, f.server.URL+"/machines/"+f.machineID+"/terminals", strings.NewReader(posted.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	openReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	openReq.Header.Set("Origin", "http://"+f.authority)
	opened, err := client.Do(openReq)
	if err != nil {
		t.Fatal(err)
	}
	opened.Body.Close()
	wantLocation := "/machines/" + f.machineID + "/terminals/" + sessionID
	if opened.StatusCode != http.StatusSeeOther || opened.Header.Get("Location") != wantLocation {
		t.Fatalf("status=%d location=%q", opened.StatusCode, opened.Header.Get("Location"))
	}

	document := f.terminalPage(client, wantLocation)
	if document.StatusCode != http.StatusOK {
		t.Fatalf("terminal document status=%d body=%s", document.StatusCode, responseBody(document))
	}
	documentBody, err := io.ReadAll(document.Body)
	document.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(documentBody), `id="terminal-impact"`) {
		t.Fatalf("terminal document missing the impact sentence:\n%s", documentBody)
	}

	conn, resp, err := f.dial(sessionID, f.origin(), []string{operatorTerminalSubprotocol})
	if err != nil {
		t.Fatalf("dial: %v body=%s", err, responseBody(resp))
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	frames := f.link.await(t, 1)
	if frames[0].Type != agentrelay.DownstreamOpen || frames[0].Session != sessionID {
		t.Fatalf("open = %#v", frames[0])
	}
	f.ready(conn, sessionID)
	writePageJSON(t, conn, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString(input)})
	frames = f.link.await(t, 2)
	if frames[1].Type != agentrelay.DownstreamInput || !bytes.Equal(frames[1].Data, input) {
		t.Fatalf("input = %#v", frames[1])
	}
	f.deliver(sessionID, agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Data: output})
	msg, _ := readPage(t, conn)
	got, err := base64.StdEncoding.DecodeString(fmt.Sprint(msg["data"]))
	if msg["type"] != "output" || err != nil || !bytes.Equal(got, output) {
		t.Fatalf("output = %#v %q %v", msg, got, err)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	f.awaitReason(sessionID, store.AgentSessionCloseReasonViewerClosed)

	var rows int
	var user string
	if err := f.hub.store.DB().QueryRow(`SELECT COUNT(*) FROM agent_sessions WHERE session_id=?`, sessionID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.store.DB().QueryRow(`SELECT operator_tailnet_user_id FROM agent_sessions WHERE session_id=?`, sessionID).Scan(&user); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := f.hub.store.DB().QueryRow(`SELECT COUNT(*) FROM agent_sessions`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || total != 1 || user != operatorTerminalUser {
		t.Fatalf("ledger rows for session=%d total=%d user=%q", rows, total, user)
	}
}

func (f *operatorTerminalFixture) terminalPage(client *http.Client, path string) *http.Response {
	f.t.Helper()
	resp, err := client.Get(f.server.URL + path)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func operatorTerminalPageForm(t *testing.T, body string) string {
	t.Helper()
	const marker = `id="action-terminal"`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("machine page has no terminal section:\n%s", body)
	}
	rest := body[i:]
	end := strings.Index(rest, "</form>")
	if end < 0 {
		t.Fatalf("terminal section has no form:\n%s", rest)
	}
	return rest[:end+len("</form>")]
}

func operatorTerminalHidden(t *testing.T, form, name string) string {
	t.Helper()
	needle := `name="` + name + `" value="`
	i := strings.Index(form, needle)
	if i < 0 {
		t.Fatalf("form missing %s:\n%s", name, form)
	}
	rest := form[i+len(needle):]
	end := strings.Index(rest, `"`)
	if end <= 0 {
		t.Fatalf("form missing %s value:\n%s", name, form)
	}
	return rest[:end]
}

func TestOperatorTerminalOpenDefaultGeometry(t *testing.T) {
	timeout := make(chan time.Time, 1)
	useOperatorTerminalSettings(t, operatorTerminalSettings{openTimeout: timeout})
	f := newOperatorTerminalFixture(t)
	conn := f.connect("opterm-default")
	defer conn.Close(websocket.StatusNormalClosure, "")
	timeout <- time.Now()
	frames := f.link.await(t, 1)
	if frames[0].Type != agentrelay.DownstreamOpen || frames[0].Cols != 80 || frames[0].Rows != 24 {
		t.Fatalf("open = %#v, want 80x24", frames[0])
	}
	if len(f.link.snapshot()) != 1 {
		t.Fatalf("frames = %#v", f.link.snapshot())
	}
}

func TestOperatorTerminalResizeBeforeReady(t *testing.T) {
	saw := make(chan struct{}, 8)
	useOperatorTerminalSettings(t, operatorTerminalSettings{afterPageCommand: func() { saw <- struct{}{} }})
	f := newOperatorTerminalFixture(t)
	conn := f.connect("opterm-resize-latch")
	defer conn.Close(websocket.StatusNormalClosure, "")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 100, "rows": 30})
	frames := f.link.await(t, 1)
	if frames[0].Type != agentrelay.DownstreamOpen || frames[0].Cols != 100 || frames[0].Rows != 30 {
		t.Fatalf("open = %#v", frames[0])
	}
	drainSignals(saw)
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 120, "rows": 40})
	<-saw
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 90, "rows": 20})
	<-saw
	frames = f.link.snapshot()
	if len(frames) != 1 {
		t.Fatalf("frames before ready = %#v", frames)
	}
	f.deliver("opterm-resize-latch", agentrelay.Upstream{Type: agentrelay.UpstreamReady})
	frames = f.link.await(t, 2)
	if frames[1].Type != agentrelay.DownstreamResize || frames[1].Cols != 90 || frames[1].Rows != 20 {
		t.Fatalf("resize = %#v", frames[1])
	}
	if len(f.link.snapshot()) != 2 {
		t.Fatalf("frames = %#v", f.link.snapshot())
	}
}

func TestOperatorTerminalInputBeforeReady(t *testing.T) {
	saw := make(chan struct{}, 8)
	useOperatorTerminalSettings(t, operatorTerminalSettings{afterPageCommand: func() { saw <- struct{}{} }})
	f := newOperatorTerminalFixture(t)
	conn := f.connect("opterm-input-early")
	defer conn.Close(websocket.StatusNormalClosure, "")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	drainSignals(saw)
	early := []byte("too-soon")
	writePageJSON(t, conn, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString(early)})
	<-saw
	for _, frame := range f.link.snapshot() {
		if frame.Type == agentrelay.DownstreamInput {
			t.Fatalf("input reached the link before ready: %#v", f.link.snapshot())
		}
	}
	f.ready(conn, "opterm-input-early")
	for _, frame := range f.link.snapshot() {
		if frame.Type == agentrelay.DownstreamInput {
			t.Fatal("dropped input was forwarded after ready")
		}
	}
	writePageJSON(t, conn, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("after"))})
	frames := f.link.await(t, 2)
	if frames[1].Type != agentrelay.DownstreamInput || !bytes.Equal(frames[1].Data, []byte("after")) {
		t.Fatalf("input = %#v", frames[1])
	}
}

func TestOperatorTerminalBusyInputRetried(t *testing.T) {
	useOperatorTerminalSettings(t, operatorTerminalSettings{busyWait: func(context.Context, time.Duration) error { return nil }})
	f := newOperatorTerminalFixture(t)
	f.link.mu.Lock()
	f.link.busyLeft[agentrelay.DownstreamInput] = 3
	f.link.mu.Unlock()
	conn := f.connect("opterm-busy-input")
	defer conn.Close(websocket.StatusNormalClosure, "")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	f.ready(conn, "opterm-busy-input")
	writePageJSON(t, conn, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("first"))})
	writePageJSON(t, conn, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("second"))})
	frames := f.link.await(t, 3)
	if frames[1].Type != agentrelay.DownstreamInput || !bytes.Equal(frames[1].Data, []byte("first")) ||
		frames[2].Type != agentrelay.DownstreamInput || !bytes.Equal(frames[2].Data, []byte("second")) {
		t.Fatalf("frames = %#v", frames)
	}
}

func TestOperatorTerminalBusyResizeRetried(t *testing.T) {
	useOperatorTerminalSettings(t, operatorTerminalSettings{busyWait: func(context.Context, time.Duration) error { return nil }})
	f := newOperatorTerminalFixture(t)
	f.link.mu.Lock()
	f.link.busyLeft[agentrelay.DownstreamResize] = 3
	f.link.mu.Unlock()
	conn := f.connect("opterm-busy-resize")
	defer conn.Close(websocket.StatusNormalClosure, "")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	f.ready(conn, "opterm-busy-resize")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 100, "rows": 40})
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 110, "rows": 50})
	frames := f.link.await(t, 3)
	if frames[1].Type != agentrelay.DownstreamResize || frames[1].Cols != 100 || frames[1].Rows != 40 ||
		frames[2].Type != agentrelay.DownstreamResize || frames[2].Cols != 110 || frames[2].Rows != 50 {
		t.Fatalf("frames = %#v", frames)
	}
}

func TestOperatorTerminalCodecRejection(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		binary  bool
	}{
		{name: "other type", payload: `{"type":"open"}`},
		{name: "missing key", payload: `{"type":"input"}`},
		{name: "session key", payload: `{"type":"input","data":"YQ==","session":"other"}`},
		{name: "duplicate key", payload: `{"type":"input","data":"YQ==","data":"Yg=="}`},
		{name: "non-integer", payload: `{"type":"resize","cols":1.5,"rows":24}`},
		{name: "out of range", payload: `{"type":"resize","cols":10001,"rows":24}`},
		{name: "trailing data", payload: `{"type":"resize","cols":80,"rows":24}{}`},
		{name: "invalid utf-8", payload: `{"type":"input","data":"/w=="}`},
		{name: "binary", binary: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newOperatorTerminalFixture(t)
			sessionID := "opterm-bad-" + strings.ReplaceAll(test.name, " ", "-")
			conn := f.connect(sessionID)
			if test.binary {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := conn.Write(ctx, websocket.MessageBinary, []byte{0, 1, 2}); err != nil {
					t.Fatal(err)
				}
			} else {
				writePage(t, conn, []byte(test.payload))
			}
			assertErrorFrame(t, conn, operatorTerminalInputInvalid)
			f.awaitReason(sessionID, store.AgentSessionCloseReasonViewerInvalid)
			assertNoRoute(t, f, sessionID)
			frames := f.link.await(t, 1)
			if frames[0].Type != agentrelay.DownstreamClose || frames[0].Session != sessionID {
				t.Fatalf("close = %#v", frames[0])
			}
		})
	}
}

func TestOperatorTerminalPageClosed(t *testing.T) {
	f := newOperatorTerminalFixture(t)
	// A reloaded page can attach as soon as the route is gone, and its
	// post-attach read has to find the row closed. So the agent's close
	// must find the ledger already written.
	rowOpenAtClose := make(chan bool, 1)
	f.link.onSend = func(frame agentrelay.Downstream) {
		if frame.Type != agentrelay.DownstreamClose {
			return
		}
		var closedAt sql.NullString
		err := f.hub.store.DB().QueryRow(`SELECT closed_at FROM agent_sessions WHERE session_id=?`, frame.Session).Scan(&closedAt)
		rowOpenAtClose <- err != nil || !closedAt.Valid
	}
	conn := f.connect("opterm-page-closed")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case open := <-rowOpenAtClose:
		if open {
			t.Fatal("the agent got close before the ledger row was closed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the agent never got close")
	}
	f.awaitReason("opterm-page-closed", store.AgentSessionCloseReasonViewerClosed)
	assertNoRoute(t, f, "opterm-page-closed")
	frames := f.link.await(t, 2)
	if frames[len(frames)-1].Type != agentrelay.DownstreamClose {
		t.Fatalf("frames = %#v", frames)
	}
}

func TestOperatorTerminalOutputBudget(t *testing.T) {
	atGate := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		outputBudget:  8,
		pageWriteGate: make(chan struct{}),
		onWriteGated: func() {
			once.Do(func() { close(atGate) })
			<-release
		},
	})
	f := newOperatorTerminalFixture(t)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	conn := f.connect("opterm-slow")
	defer conn.Close(websocket.StatusNormalClosure, "")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	returned := make(chan struct{})
	go func() {
		f.deliver("opterm-slow", agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Data: []byte("1234")})
		close(returned)
	}()
	select {
	case <-atGate:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not reach the gate")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("first Deliver blocked")
	}
	// The writer is held at the gate with its wake already taken, so the
	// second chunk fills the wake slot and the third finds it full.
	for _, chunk := range []string{"5", "6"} {
		done := make(chan struct{})
		go func() {
			f.deliver("opterm-slow", agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Data: []byte(chunk)})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("Deliver of %q blocked while the page was not reading", chunk)
		}
	}
	overflowDone := make(chan struct{})
	go func() {
		f.deliver("opterm-slow", agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Data: bytes.Repeat([]byte{'x'}, 8)})
		close(overflowDone)
	}()
	select {
	case <-overflowDone:
	case <-time.After(200 * time.Millisecond):
		t.Error("overflow Deliver blocked")
	}
	releaseOnce.Do(func() { close(release) })
	f.awaitReason("opterm-slow", store.AgentSessionCloseReasonViewerTooSlow)
	assertNoRoute(t, f, "opterm-slow")
}

func TestOperatorTerminalDeliverDuringEnd(t *testing.T) {
	f := newOperatorTerminalFixture(t)
	f.link.onSend = func(frame agentrelay.Downstream) {
		if frame.Type != agentrelay.DownstreamClose {
			return
		}
		done := make(chan struct{})
		go func() {
			_ = f.hub.agentLinks.Deliver(f.machineID, agentrelay.Upstream{
				Type: agentrelay.UpstreamOutput, Session: frame.Session, Data: []byte("during-end"),
			})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Errorf("Deliver during end did not return")
		}
	}
	conn := f.connect("opterm-reenter")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	f.ready(conn, "opterm-reenter")
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	f.awaitReason("opterm-reenter", store.AgentSessionCloseReasonViewerClosed)
}

func TestOperatorTerminalUpstreamErrors(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		f, conn := openReadyTerminal(t, "opterm-revoked")
		if operatorTerminalRevoked != "這個終端的存取權已撤銷，終端已結束。請回到機器頁。" {
			t.Fatalf("revoked sentence = %q", operatorTerminalRevoked)
		}
		f.hub.agentLinks.CloseSessions([]string{"opterm-revoked"}, agentlink.ReasonSessionRevoked)
		raw := assertErrorFrame(t, conn, "這個終端的存取權已撤銷，終端已結束。請回到機器頁。")
		if strings.Contains(string(raw), agentlink.ReasonSessionRevoked.String()) {
			t.Fatalf("registry reason reached the page: %s", raw)
		}
		f.awaitReason("opterm-revoked", store.AgentSessionCloseReasonAccessRevoked)
	})
	t.Run("detached", func(t *testing.T) {
		f, conn := openReadyTerminal(t, "opterm-detached")
		f.hub.agentLinks.Detach(f.machineID, f.link)
		raw := assertErrorFrame(t, conn, operatorTerminalDisconnectedCopy)
		if strings.Contains(string(raw), agentlink.ReasonAgentDisconnected.String()) {
			t.Fatalf("registry reason reached the page: %s", raw)
		}
		f.awaitReason("opterm-detached", store.AgentSessionCloseReasonAgentConnectionEnded)
	})
	t.Run("other agent reason", func(t *testing.T) {
		f, conn := openReadyTerminal(t, "opterm-machine")
		f.deliver("opterm-machine", agentrelay.Upstream{Type: agentrelay.UpstreamError, Reason: agentpty.ReasonInputInvalidUTF8})
		raw := assertErrorFrame(t, conn, operatorTerminalMachineFailed)
		if strings.Contains(string(raw), agentpty.ReasonInputInvalidUTF8) || strings.Contains(string(raw), "UTF-8") {
			t.Fatalf("agent reason reached the page: %s", raw)
		}
		f.awaitReason("opterm-machine", store.AgentSessionCloseReasonMachineError)
		assertNoRoute(t, f, "opterm-machine")
	})
}

func TestOperatorTerminalReauthorization(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		later  func(r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision)
	}{
		{
			name: "denied", reason: store.AgentSessionCloseReasonAccessRevoked,
			later: func(*http.Request, operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				return nil, operatorauth.Decision{Allowed: false, HTTPStatus: http.StatusForbidden, Code: operatorauth.CapabilityRequired, Detail: "revoked"}
			},
		},
		{
			name: "unavailable", reason: store.AgentSessionCloseReasonAuthUnavailable,
			later: func(*http.Request, operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				return nil, operatorauth.Decision{Allowed: false, HTTPStatus: http.StatusServiceUnavailable, Code: operatorauth.AuthSourceUnavailable, Detail: "down"}
			},
		},
		{
			name: "configuration", reason: store.AgentSessionCloseReasonAuthUnavailable,
			later: func(*http.Request, operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				return nil, operatorauth.Decision{Allowed: false, HTTPStatus: http.StatusServiceUnavailable, Code: operatorauth.AuthConfigurationInvalid, Detail: "bad"}
			},
		},
		{
			name: "other user", reason: store.AgentSessionCloseReasonAccessRevoked,
			later: func(r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision) {
				principal := operatorTerminalPrincipal("someone-else", permission)
				return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
					Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ticks := make(chan time.Time, 1)
			useOperatorTerminalSettings(t, operatorTerminalSettings{reauthTicks: ticks})
			f := newOperatorTerminalFixture(t)
			f.auth.later = func(call int, r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision, bool) {
				if call == 1 {
					return nil, operatorauth.Decision{}, false
				}
				req, decision := test.later(r, permission)
				return req, decision, true
			}
			sessionID := "opterm-reauth-" + test.name
			conn := f.connect(sessionID)
			defer conn.Close(websocket.StatusNormalClosure, "")
			writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
			f.link.await(t, 1)
			f.ready(conn, sessionID)
			ticks <- time.Now()
			f.awaitReason(sessionID, test.reason)
			assertNoRoute(t, f, sessionID)
		})
	}
}

func TestOperatorTerminalDuplicate(t *testing.T) {
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		duplicateWait: func(context.Context, func() bool) bool { return false },
	})
	f := newOperatorTerminalFixture(t)
	first := f.connect("opterm-dup")
	defer first.Close(websocket.StatusNormalClosure, "")
	writePageJSON(t, first, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	f.ready(first, "opterm-dup")
	second, _, err := f.dial("opterm-dup", f.origin(), []string{operatorTerminalSubprotocol})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(websocket.StatusNormalClosure, "")
	assertErrorFrame(t, second, operatorTerminalDuplicateCopy)
	writePageJSON(t, first, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("still-here"))})
	frames := f.link.await(t, 2)
	if !bytes.Equal(frames[1].Data, []byte("still-here")) {
		t.Fatalf("first page input = %#v", frames[1])
	}
	open, _ := f.session("opterm-dup")
	if !open || !f.hasRoute("opterm-dup") {
		t.Fatal("duplicate closed the row or the route")
	}
}

func TestOperatorTerminalDuplicateRowCloses(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		duplicateWait: func(_ context.Context, closed func() bool) bool {
			once.Do(func() { close(entered) })
			<-release
			return closed()
		},
	})
	f := newOperatorTerminalFixture(t)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	first := f.connect("opterm-reload")
	defer first.Close(websocket.StatusNormalClosure, "")
	writePageJSON(t, first, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	second, _, err := f.dial("opterm-reload", f.origin(), []string{operatorTerminalSubprotocol})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(websocket.StatusNormalClosure, "")
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("duplicate wait did not start")
	}
	if _, err := f.hub.store.CloseAgentSessionsByID([]string{"opterm-reload"}, store.AgentSessionCloseReasonViewerClosed); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	assertErrorFrame(t, second, operatorTerminalEnded)
	if !f.hasRoute("opterm-reload") {
		t.Fatal("duplicate path removed the attached route")
	}
	open, reason := f.session("opterm-reload")
	if open || reason != store.AgentSessionCloseReasonViewerClosed {
		t.Fatalf("row open=%t reason=%q", open, reason)
	}
}

func TestOperatorTerminalNoLink(t *testing.T) {
	f := newOperatorTerminalFixture(t)
	f.hub.agentLinks.Detach(f.machineID, f.link)
	conn := f.connect("opterm-nolink")
	defer conn.Close(websocket.StatusNormalClosure, "")
	assertErrorFrame(t, conn, operatorTerminalNoLinkCopy)
	f.awaitReason("opterm-nolink", store.AgentSessionCloseReasonTerminalNotLinked)
	assertNoRoute(t, f, "opterm-nolink")
	if frames := f.link.snapshot(); len(frames) != 0 {
		t.Fatalf("link frames = %#v", frames)
	}
}

func TestOperatorTerminalRowClosesBeforeAttach(t *testing.T) {
	const sessionID = "opterm-before-attach"
	f := newOperatorTerminalFixture(t)
	f.openSession(sessionID)
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		beforeOpenSession: func() {
			if _, err := f.hub.store.CloseAgentSessionsByID([]string{sessionID}, store.AgentSessionCloseReasonViewerClosed); err != nil {
				t.Errorf("close row: %v", err)
			}
		},
	})
	conn, _, err := f.dial(sessionID, f.origin(), []string{operatorTerminalSubprotocol})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	assertErrorFrame(t, conn, operatorTerminalEnded)
	assertNoRoute(t, f, sessionID)
	for _, frame := range f.link.snapshot() {
		if frame.Type == agentrelay.DownstreamOpen {
			t.Fatal("open was sent after the row closed")
		}
	}
	open, reason := f.session(sessionID)
	if open || reason != store.AgentSessionCloseReasonViewerClosed {
		t.Fatalf("row open=%t reason=%q", open, reason)
	}
}

func TestOperatorTerminalPingTimeout(t *testing.T) {
	ticks := make(chan time.Time, 1)
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		heartbeatTicks: ticks,
		heartbeatWait:  100 * time.Millisecond,
	})
	f := newOperatorTerminalFixture(t)
	conn := f.connect("opterm-ping")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	f.ready(conn, "opterm-ping")
	ticks <- time.Now()
	f.awaitReason("opterm-ping", store.AgentSessionCloseReasonViewerClosed)
	assertNoRoute(t, f, "opterm-ping")
}

// Force the page reader to enqueue input after ready, while the producer
// has not yet entered waitReady. No socket scheduling or sleeps are needed.
func TestOperatorTerminalInputAfterReadyQueued(t *testing.T) {
	for _, kind := range []string{"input", "resize"} {
		t.Run(kind, func(t *testing.T) {
			link := newOperatorTerminalLink()
			h := &hub{agentLinks: agentlink.New()}
			if _, err := h.agentLinks.Attach("machine", link); err != nil {
				t.Fatal(err)
			}
			s := newOperatorTerminalSession(h, nil, store.AgentSessionResult{
				SessionID: "session", MachineID: "machine",
			}, defaultOperatorTerminalSettings(h))
			if err := h.agentLinks.OpenSession("session", "machine", s); err != nil {
				t.Fatal(err)
			}
			// Model commands read before and after Deliver closes readyCh,
			// all queued before the producer gets another turn.
			s.frames = make(chan operatorTerminalCommand, 4)
			s.frames <- operatorTerminalCommand{resize: true, cols: 90, rows: 30}
			s.frames <- operatorTerminalCommand{data: []byte("early")}
			if err := s.Deliver(agentrelay.Upstream{Type: agentrelay.UpstreamReady}); err != nil {
				t.Fatal(err)
			}
			cmd := operatorTerminalCommand{ready: s.readyFired(), data: []byte("after-ready")}
			if kind == "resize" {
				cmd.resize, cmd.cols, cmd.rows = true, 100, 40
			}
			s.frames <- cmd
			cols, rows := 80, 24
			first, ok := s.waitReady(context.Background(), &cols, &rows)
			if !ok || first == nil {
				t.Fatal("command received after ready was discarded by readiness drain")
			}
			if first.ready != cmd.ready || first.resize != cmd.resize || !bytes.Equal(first.data, cmd.data) {
				t.Fatalf("first command = %#v, want %#v", first, cmd)
			}
			if cols != 90 || rows != 30 {
				t.Fatalf("pre-ready geometry = %dx%d, want 90x30", cols, rows)
			}
			// Late-enqueued early input must also be dropped by forward.
			s.frames <- operatorTerminalCommand{data: []byte("late-early")}
			close(s.frames)
			s.forward(context.Background(), first)
			frames := link.snapshot()
			if len(frames) != 1 {
				t.Fatalf("forwarded frames = %#v, want one post-ready command", frames)
			}
			if kind == "input" && (frames[0].Type != agentrelay.DownstreamInput || !bytes.Equal(frames[0].Data, cmd.data)) {
				t.Fatalf("forwarded input = %#v", frames[0])
			}
			if kind == "resize" && (frames[0].Type != agentrelay.DownstreamResize || frames[0].Cols != 100 || frames[0].Rows != 40) {
				t.Fatalf("forwarded resize = %#v", frames[0])
			}
		})
	}
}

func TestOperatorTerminalPingDuringBusy(t *testing.T) {
	ticks := make(chan time.Time, 1)
	started := make(chan struct{})
	busyEntered := make(chan struct{})
	releaseBusy := make(chan struct{})
	blocked := make(chan struct{})
	releaseBlock := make(chan struct{})
	pingDone := make(chan error, 1)
	var busyOnce, blockOnce, startOnce, releaseBusyOnce, releaseBlockOnce sync.Once
	unblockBusy := func() { releaseBusyOnce.Do(func() { close(releaseBusy) }) }
	unblockReader := func() { releaseBlockOnce.Do(func() { close(releaseBlock) }) }
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		heartbeatTicks: ticks,
		heartbeatWait:  150 * time.Millisecond,
		onPingStart: func() {
			startOnce.Do(func() { close(started) })
			// Start the ping deadline only once the producer holds the
			// busy frame; CI scheduling before that point is irrelevant.
			select {
			case <-busyEntered:
			case <-releaseBusy: // Also release the hook on test failure.
			}
		},
		onPingDone: func(err error) { pingDone <- err },
		busyWait: func(ctx context.Context, _ time.Duration) error {
			busyOnce.Do(func() { close(busyEntered) })
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-releaseBusy:
				return nil
			}
		},
		onReaderBlocked: func() {
			blockOnce.Do(func() { close(blocked) })
			<-releaseBlock
		},
	})
	f := newOperatorTerminalFixture(t)
	t.Cleanup(unblockBusy)
	t.Cleanup(unblockReader)
	f.link.mu.Lock()
	f.link.busyLeft[agentrelay.DownstreamInput] = 1
	f.link.mu.Unlock()
	conn := f.connect("opterm-ping-busy")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	f.ready(conn, "opterm-ping-busy")
	ticks <- time.Now()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("ping did not start")
	}
	writePageJSON(t, conn, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("held"))})
	select {
	case <-busyEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("busy wait did not start")
	}
	writePageJSON(t, conn, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("fill"))})
	writePageJSON(t, conn, map[string]any{"type": "input", "data": base64.StdEncoding.EncodeToString([]byte("block"))})
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not block")
	}
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _, err := conn.Read(ctx)
			cancel()
			if err != nil {
				return
			}
		}
	}()
	select {
	case err := <-pingDone:
		if err == nil {
			t.Fatal("ping succeeded while the reader was blocked")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ping did not finish")
	}
	open, _ := f.session("opterm-ping-busy")
	if !open {
		t.Fatal("ping during a held frame closed the session")
	}
	unblockReader()
	unblockBusy()
	frames := f.link.await(t, 2)
	if frames[1].Type != agentrelay.DownstreamInput || !bytes.Equal(frames[1].Data, []byte("held")) {
		t.Fatalf("held frame = %#v", frames[1])
	}
	open, _ = f.session("opterm-ping-busy")
	if !open {
		t.Fatal("session closed after the held frame was sent")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	f.awaitReason("opterm-ping-busy", store.AgentSessionCloseReasonViewerClosed)
}

func TestOperatorTerminalExitWithoutCode(t *testing.T) {
	f := newOperatorTerminalFixture(t)
	conn := f.connect("opterm-exit-nocode")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	f.ready(conn, "opterm-exit-nocode")
	f.deliver("opterm-exit-nocode", agentrelay.Upstream{Type: agentrelay.UpstreamExit})
	msg, _ := readPage(t, conn)
	if msg["type"] != "error" || msg["reason"] != operatorTerminalEnded {
		t.Fatalf("frame = %#v, want ended error", msg)
	}
	f.awaitReason("opterm-exit-nocode", store.AgentSessionCloseReasonExited)
}

func TestOperatorTerminalExitDrainsOutput(t *testing.T) {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	gated := make(chan struct{}, 4)
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		pageWriteGate: gate,
		onWriteGated:  func() { gated <- struct{}{} },
	})
	f := newOperatorTerminalFixture(t)
	conn := f.connect("opterm-drain")
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	f.deliver("opterm-drain", agentrelay.Upstream{Type: agentrelay.UpstreamReady})
	select {
	case <-gated:
	case <-time.After(2 * time.Second):
		t.Fatal("ready write was not gated")
	}
	msg, _ := readPage(t, conn)
	if msg["type"] != "ready" {
		t.Fatalf("frame = %#v", msg)
	}
	output := []byte("before-exit")
	f.deliver("opterm-drain", agentrelay.Upstream{Type: agentrelay.UpstreamOutput, Data: output})
	select {
	case <-gated:
	case <-time.After(2 * time.Second):
		t.Fatal("output write was not gated")
	}
	code := 7
	f.deliver("opterm-drain", agentrelay.Upstream{Type: agentrelay.UpstreamExit, Code: &code})
	msg, _ = readPage(t, conn)
	got, err := base64.StdEncoding.DecodeString(fmt.Sprint(msg["data"]))
	if msg["type"] != "output" || err != nil || !bytes.Equal(got, output) {
		t.Fatalf("first frame = %#v %q %v", msg, got, err)
	}
	msg, _ = readPage(t, conn)
	exitCode, _ := msg["code"].(float64)
	if msg["type"] != "exit" || exitCode != 7 {
		t.Fatalf("second frame = %#v", msg)
	}
}

func TestOperatorTerminalReadLimit(t *testing.T) {
	t.Run("largest page frame", func(t *testing.T) {
		n := operatorTerminalPageInputBudget(t)
		raw := bytes.Repeat([]byte("A"), n)
		payload, err := json.Marshal(struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{Type: "input", Data: base64.StdEncoding.EncodeToString(raw)})
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) <= 32768 || len(payload) > 64*1024 {
			t.Fatalf("largest page frame is %d bytes", len(payload))
		}
		f := newOperatorTerminalFixture(t)
		conn := f.connect("opterm-limit-fit")
		defer conn.Close(websocket.StatusNormalClosure, "")
		writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
		f.link.await(t, 1)
		f.ready(conn, "opterm-limit-fit")
		writePage(t, conn, payload)
		frames := f.link.await(t, 2)
		if frames[1].Type != agentrelay.DownstreamInput || !bytes.Equal(frames[1].Data, raw) {
			t.Fatalf("forwarded %d bytes, want %d", len(frames[1].Data), len(raw))
		}
	})
	t.Run("over limit", func(t *testing.T) {
		f := newOperatorTerminalFixture(t)
		conn := f.connect("opterm-limit-over")
		writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
		f.link.await(t, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, bytes.Repeat([]byte("x"), 64*1024+1)); err != nil {
			t.Fatal(err)
		}
		f.awaitReason("opterm-limit-over", store.AgentSessionCloseReasonViewerInvalid)
		assertNoRoute(t, f, "opterm-limit-over")
	})
}

func operatorTerminalPageInputBudget(t *testing.T) int {
	t.Helper()
	src := string(terminalassets.PageJS)
	const marker = "var maxInputBytes = "
	at := strings.Index(src, marker)
	if at < 0 {
		t.Fatal("terminal.js has no maxInputBytes")
	}
	var left, right int
	if _, err := fmt.Sscanf(src[at+len(marker):], "%d * %d", &left, &right); err != nil {
		t.Fatal(err)
	}
	return left * right
}

func openReadyTerminal(t *testing.T, sessionID string) (*operatorTerminalFixture, *websocket.Conn) {
	t.Helper()
	f := newOperatorTerminalFixture(t)
	conn := f.connect(sessionID)
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	writePageJSON(t, conn, map[string]any{"type": "resize", "cols": 80, "rows": 24})
	f.link.await(t, 1)
	f.ready(conn, sessionID)
	return f, conn
}

func drainSignals(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func TestOperatorTerminalIdleTimeout(t *testing.T) {
	ticks := make(chan time.Time)
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		idleTicks: ticks,
	})

	f, conn := openReadyTerminal(t, "opterm-idle")
	f.secrets = nil
	defer conn.Close(websocket.StatusNormalClosure, "")
	ticks <- time.Now()
	f.awaitReason("opterm-idle", store.AgentSessionCloseReasonIdleTimeout)
	assertErrorFrame(t, conn, operatorTerminalIdleTimeoutCopy)
}

func TestOperatorTerminalIdleResetOnInput(t *testing.T) {
	resetCh := make(chan struct{}, 1)
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		onIdleReset: func() {
			select {
			case resetCh <- struct{}{}:
			default:
			}
		},
	})

	f, conn := openReadyTerminal(t, "opterm-idle-reset")
	f.secrets = nil
	defer conn.Close(websocket.StatusNormalClosure, "")

	// send input
	writePageJSON(t, conn, map[string]any{"type": "input", "data": "eHh4"}) // "xxx"
	select {
	case <-resetCh:
	case <-time.After(2 * time.Second):
		t.Fatal("idle timer not reset on input")
	}

	time.Sleep(50 * time.Millisecond)
	drainSignals(resetCh)

	// Output does NOT trigger reset; we ensure no reset happens
	// Deliver output
	f.deliver("opterm-idle-reset", agentrelay.Upstream{
		Type: agentrelay.UpstreamOutput, Data: []byte("out"),
	})
	select {
	case <-resetCh:
		t.Fatal("idle timer should not reset on output")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOperatorTerminalLifetimeReached(t *testing.T) {
	ticks := make(chan time.Time)
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		lifetimeTicks: ticks,
	})

	f, conn := openReadyTerminal(t, "opterm-lifetime")
	f.secrets = nil
	defer conn.Close(websocket.StatusNormalClosure, "")
	ticks <- time.Now()
	f.awaitReason("opterm-lifetime", store.AgentSessionCloseReasonLifetimeReached)
	assertErrorFrame(t, conn, operatorTerminalLifetimeReachedCopy)
}

func TestOperatorTerminalLifetimeAttachExpired(t *testing.T) {
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		maxLifetime: time.Millisecond,
	})

	f := newOperatorTerminalFixture(t)
	conn := f.connect("opterm-lifetime-expired")
	f.secrets = nil
	defer conn.Close(websocket.StatusNormalClosure, "")
	f.awaitReason("opterm-lifetime-expired", store.AgentSessionCloseReasonLifetimeReached)
	assertErrorFrame(t, conn, operatorTerminalLifetimeReachedCopy)
}

func TestOperatorTerminalRaceViewerClose(t *testing.T) {
	ticks := make(chan time.Time)
	useOperatorTerminalSettings(t, operatorTerminalSettings{
		idleTicks: ticks,
	})

	f, conn := openReadyTerminal(t, "opterm-race")
	f.secrets = nil

	// Close viewer at the same time as idle timeout fires
	ticks <- time.Now()
	conn.Close(websocket.StatusNormalClosure, "")

	// Wait for closure by polling f.session
	deadline := time.Now().Add(2 * time.Second)
	var reason string
	for time.Now().Before(deadline) {
		open, r := f.session("opterm-race")
		if !open && r != "" {
			reason = r
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if reason != store.AgentSessionCloseReasonIdleTimeout && reason != store.AgentSessionCloseReasonViewerClosed {
		t.Fatalf("unexpected reason: %q", reason)
	}
}
