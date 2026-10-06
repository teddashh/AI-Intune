package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

const operatorTerminalRevokedSentence = "這個終端的存取權已撤銷，終端已結束。請回到機器頁。"

type terminalCloseLink struct {
	mu     sync.Mutex
	frames []agentrelay.Downstream
}

func (l *terminalCloseLink) Send(frame agentrelay.Downstream) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	copied := frame
	l.frames = append(l.frames, copied)
	return nil
}

func (l *terminalCloseLink) Close(string) {}

func (l *terminalCloseLink) snapshot() []agentrelay.Downstream {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]agentrelay.Downstream, len(l.frames))
	copy(out, l.frames)
	return out
}

type terminalCloseSink struct{}

func (terminalCloseSink) Deliver(agentrelay.Upstream) error { return nil }

func TestLifecycleAPIClosesRoutedSessionWithoutReconciler(t *testing.T) {
	h, machineID, link := newTerminalCloseHub(t, "retire-route")
	const sessionID = "retire-route-session"
	openRoutedTerminalSession(t, h, machineID, sessionID, "42")

	preview := postLifecyclePreview(t, h, machineID, 0)
	rec := putLifecycle(t, h, machineID, "retire-route-key", lifecycleApplyBody(
		"retired", preview.LifecycleRevision, preview.DisplayName, preview.PreviewDigest, "retire the live terminal"))
	if rec.Code != http.StatusOK {
		t.Fatalf("retire=%d %s", rec.Code, rec.Body.String())
	}
	assertDownstreamClose(t, link, sessionID)
}

func TestAssignedUserAPIClosesRoutedSessionWithoutReconciler(t *testing.T) {
	h, machineID, link := newTerminalCloseHub(t, "assign-route")
	cache := tailnet.NewCache()
	cache.SetStatus(tailnet.Status{
		Available: true,
		Self:      tailnet.Peer{UserID: "99"},
		TailnetUsers: []tailnet.TailnetUser{{
			UserID: "99", Login: "next@example.com", DisplayName: "Next",
		}},
	})
	h.tailnet = cache
	h.operatorService = operator.NewWithTailnet(h.store, cache)
	installOperatorTerminalSessionCloser(h)
	const sessionID = "assign-route-session"
	openRoutedTerminalSession(t, h, machineID, sessionID, "42")
	var revision int64
	if err := h.store.DB().QueryRow(`SELECT assigned_user_revision FROM machine_registry WHERE machine_id=?`, machineID).Scan(&revision); err != nil {
		t.Fatal(err)
	}

	body := fmt.Sprintf(`{"user_id":"99","expected_revision":%d,"confirm_display_name":%q}`, revision, "assign-route")
	rec := putAssignedUser(t, h, machineID, "assign-route-key", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("reassign=%d %s", rec.Code, rec.Body.String())
	}
	assertDownstreamClose(t, link, sessionID)
}

func TestOpenTerminalPageReceivesRevokedSentenceWhenHubRetiresMachine(t *testing.T) {
	f, conn := openReadyTerminal(t, "opterm-retire-page")
	f.hub.operatorService = operator.New(f.hub.store)
	installOperatorTerminalSessionCloser(f.hub)

	preview := postLifecyclePreview(t, f.hub, f.machineID, 0)
	rec := putLifecycle(t, f.hub, f.machineID, "retire-page-key", lifecycleApplyBody(
		"retired", preview.LifecycleRevision, preview.DisplayName, preview.PreviewDigest, "retire the open page"))
	if rec.Code != http.StatusOK {
		t.Fatalf("retire=%d %s", rec.Code, rec.Body.String())
	}
	assertErrorFrame(t, conn, operatorTerminalRevokedSentence)
	if operatorTerminalRevoked != operatorTerminalRevokedSentence {
		t.Fatalf("revoked sentence = %q", operatorTerminalRevoked)
	}
}

func newTerminalCloseHub(t *testing.T, name string) (*hub, string, *terminalCloseLink) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, enrollmentToken, err := st.CreateEnrollTokenFor(name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineID, _, err := st.RedeemEnrollToken(enrollmentToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, Hostname: name,
		UnixUser: "tester", OS: "linux", Arch: "amd64",
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	link := &terminalCloseLink{}
	h := &hub{store: st, agentLinks: agentlink.New(), operatorService: operator.New(st)}
	installOperatorTerminalSessionCloser(h)
	if _, err := h.agentLinks.Attach(machineID, link); err != nil {
		t.Fatal(err)
	}
	return h, machineID, link
}

func openRoutedTerminalSession(t *testing.T, h *hub, machineID, sessionID, userID string) {
	t.Helper()
	if _, err := h.store.DB().Exec(`UPDATE machine_registry
		SET assigned_user_id=?, assigned_user_login=? WHERE machine_id=?`,
		userID, "operator@example.com", machineID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.OpenAgentSession(store.OpenAgentSessionRequest{
		SessionID: sessionID, MachineID: machineID,
		OperatorTailnetUserID: userID, OperatorTailnetUserLogin: "operator@example.com",
		IdempotencyKey: "open-" + sessionID, RequestDigest: "sha256:" + sessionID,
		Audit: store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.agentLinks.OpenSession(sessionID, machineID, terminalCloseSink{}); err != nil {
		t.Fatal(err)
	}
}

func postLifecyclePreview(t *testing.T, h *hub, machineID string, revision int64) store.OperatorMachineLifecyclePreviewResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/operator/machines/"+machineID+"/lifecycle-preview",
		strings.NewReader(fmt.Sprintf(`{"desired_state":"retired","expected_revision":%d}`, revision)))
	req.SetPathValue("id", machineID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handlePreviewOperatorMachineLifecycle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview=%d %s", rec.Code, rec.Body.String())
	}
	var result store.OperatorMachineLifecyclePreviewResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func putLifecycle(t *testing.T, h *hub, machineID, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/operator/machines/"+machineID+"/lifecycle", strings.NewReader(body))
	req.SetPathValue("id", machineID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.RemoteAddr = "100.64.0.7:41234"
	req = verifiedOperatorRequest(req, operatorauth.Admin)
	rec := httptest.NewRecorder()
	h.handlePutOperatorMachineLifecycle(rec, req)
	return rec
}

func putAssignedUser(t *testing.T, h *hub, machineID, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/v1/operator/machines/"+machineID+"/assigned-user", strings.NewReader(body))
	req.SetPathValue("id", machineID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.RemoteAddr = "100.64.0.7:41234"
	req = verifiedOperatorRequest(req, operatorauth.Admin)
	rec := httptest.NewRecorder()
	h.handlePutOperatorMachineAssignedUser(rec, req)
	return rec
}

func assertDownstreamClose(t *testing.T, link *terminalCloseLink, sessionID string) {
	t.Helper()
	frames := link.snapshot()
	if len(frames) != 1 || frames[0].Type != agentrelay.DownstreamClose || frames[0].Session != sessionID {
		t.Fatalf("link frames=%#v, want one close for %s", frames, sessionID)
	}
}
