package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

func tailnetAdminGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	req := verifiedWebRequest(httptest.NewRequest(http.MethodGet, path, nil), "example.com/cap/clawctl-admin")
	mux.ServeHTTP(rec, req)
	return rec
}

func TestTailnetSettingsPreviewApplyAndUnignore(t *testing.T) {
	s, st := newServer(t)
	s.SetTailnetStatus(tailnet.Status{
		Available: true,
		Self:      tailnet.Peer{StableID: "node-hub", Hostname: "hub", IP: "100.64.0.1", Online: true},
		Peers:     []tailnet.Peer{{StableID: "node-phone", Hostname: "phone", IP: "100.64.0.2", OS: "iOS"}},
	})
	admin := tailnetAdminGet(t, s, "/settings/tailnet")
	if admin.Code != http.StatusOK || !strings.Contains(admin.Body.String(), "node-phone") ||
		!strings.Contains(admin.Body.String(), `action="/settings/tailnet/peer-ignore-preview"`) {
		t.Fatalf("admin page=%d body=%s", admin.Code, admin.Body.String())
	}

	preview := postForm(t, s, "/settings/tailnet/peer-ignore-preview", url.Values{
		"peer_id": {"node-phone"}, "action": {"ignore"}, "days": {"30"}, "reason": {"personal device"},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "確認 Tailnet 規則") ||
		!strings.Contains(preview.Body.String(), "phone") {
		t.Fatalf("preview=%d body=%s", preview.Code, preview.Body.String())
	}
	form := url.Values{
		"peer_id":           {"node-phone"},
		"action":            {"ignore"},
		"expires_at":        {hiddenFormValue(t, preview.Body.String(), "expires_at")},
		"expected_revision": {hiddenFormValue(t, preview.Body.String(), "expected_revision")},
		"preview_digest":    {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"reason":            {hiddenFormValue(t, preview.Body.String(), "reason")},
		"idempotency_key":   {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
		"confirm_hostname":  {"phone"},
	}
	applied := postForm(t, s, "/settings/tailnet/peer-ignores", form)
	if applied.Code != http.StatusSeeOther || applied.Header().Get("Location") != "/settings/tailnet" {
		t.Fatalf("apply=%d headers=%v body=%s", applied.Code, applied.Header(), applied.Body.String())
	}
	s.SetTailnetStatus(tailnet.Status{Unavailable: "tailscaled unavailable"})
	settings := tailnetAdminGet(t, s, "/settings/tailnet")
	if !strings.Contains(settings.Body.String(), "清單無法取得") || !strings.Contains(settings.Body.String(), "有效忽略規則") ||
		!strings.Contains(settings.Body.String(), "personal device") ||
		!strings.Contains(settings.Body.String(), `action="/settings/tailnet/peer-ignore-preview"`) {
		t.Fatalf("settings after apply=%s", settings.Body.String())
	}

	unignorePreview := postForm(t, s, "/settings/tailnet/peer-ignore-preview", url.Values{
		"peer_id": {"node-phone"}, "action": {"unignore"}, "reason": {"manage this device"},
	})
	if unignorePreview.Code != http.StatusOK {
		t.Fatalf("unignore preview=%d body=%s", unignorePreview.Code, unignorePreview.Body.String())
	}
	unignoreForm := url.Values{
		"peer_id":           {"node-phone"},
		"action":            {"unignore"},
		"expires_at":        {hiddenFormValue(t, unignorePreview.Body.String(), "expires_at")},
		"expected_revision": {hiddenFormValue(t, unignorePreview.Body.String(), "expected_revision")},
		"preview_digest":    {hiddenFormValue(t, unignorePreview.Body.String(), "preview_digest")},
		"reason":            {hiddenFormValue(t, unignorePreview.Body.String(), "reason")},
		"idempotency_key":   {hiddenFormValue(t, unignorePreview.Body.String(), "idempotency_key")},
		"confirm_hostname":  {"phone"},
	}
	removed := postForm(t, s, "/settings/tailnet/peer-ignores", unignoreForm)
	if removed.Code != http.StatusSeeOther {
		t.Fatalf("unignore=%d body=%s", removed.Code, removed.Body.String())
	}
	if rules, err := st.TailnetPeerIgnores(time.Now().UTC()); err != nil || len(rules) != 0 {
		t.Fatalf("rules=%+v err=%v", rules, err)
	}
	entries, err := st.ListAuditReads(store.AuditReadFilter{Actions: []store.AuditAction{store.AuditTailnetPeerIgnore}, Limit: 10})
	if err != nil || len(entries.Items) != 2 || entries.Items[0].SourceKind != "web" {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}
