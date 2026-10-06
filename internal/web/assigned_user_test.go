package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

func assignedUserWebStatus() tailnet.Status {
	return tailnet.Status{Available: true, Self: tailnet.Peer{UserID: "42"}, TailnetUsers: []tailnet.TailnetUser{{UserID: "42", Login: "user@example.com"}}}
}

func assignedUserPage(t *testing.T, s *Server, path, capability string) string {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	granted := operatorauth.CapabilityNames{}
	switch {
	case strings.HasSuffix(capability, "-admin"):
		granted.Admin = capability
	case strings.HasSuffix(capability, "-operate"):
		granted.Operate = capability
	default:
		granted.View = capability
	}
	mux.ServeHTTP(rec, requestWithCapabilities("GET", path, granted))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "</html>") {
		t.Fatalf("頁面=%d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestAssignedUserWebStatesAndAvailableActions(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "指派測試")
	s.SetTailnetStatus(tailnet.Status{})
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "指派使用者：未指派") || !strings.Contains(body, "使用者名冊：來源不可用") || strings.Contains(body, "使用者名冊：沒有可指派的使用者") {
		t.Fatal("來源不可用與空名冊未分開")
	}
	s.SetTailnetStatus(tailnet.Status{Available: true})
	body = get(t, s, "/machines/"+id)
	if !strings.Contains(body, "使用者名冊：沒有可指派的使用者") || strings.Contains(body, "使用者名冊：來源不可用") {
		t.Fatal("空名冊狀態不符")
	}
	s.SetTailnetStatus(assignedUserWebStatus())
	for _, capability := range []string{"view", "operate", "admin"} {
		body = assignedUserPage(t, s, "/machines/"+id, "example.com/cap/clawctl-"+capability)
		if strings.Contains(body, "/assigned-user-preview") != (capability == "admin") {
			t.Fatalf("%s 的表單可見性不符", capability)
		}
	}
	body = get(t, s, "/machines")
	if !strings.Contains(body, "<th>指派使用者</th>") || !strings.Contains(body, "未指派") {
		t.Fatal("清單缺少指派使用者")
	}
}

func TestAssignedUserWebPreviewConfirmStaleAndClear(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "指派測試")
	s.SetTailnetStatus(assignedUserWebStatus())
	path := "/machines/" + id
	preview := postForm(t, s, path+"/assigned-user-preview", url.Values{"user_id": {"42"}, "expected_revision": {"0"}})
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), "未指派 → user@example.com") || !strings.Contains(preview.Body.String(), `name="confirm_display_name"`) {
		t.Fatalf("預覽=%d %s", preview.Code, preview.Body.String())
	}
	match := regexp.MustCompile(`name="idempotency_key" value="([^"]+)"`).FindStringSubmatch(preview.Body.String())
	if len(match) != 2 {
		t.Fatal("缺少請求金鑰")
	}
	m, _ := st.GetMachine(id)
	if m.AssignedUserID != "" {
		t.Fatal("預覽改變指派")
	}
	form := url.Values{"user_id": {"42"}, "expected_revision": {"0"}, "confirm_display_name": {"指派測試"}, "idempotency_key": {match[1]}}
	applied := postForm(t, s, path+"/assigned-user", form)
	if applied.Code != 303 {
		t.Fatalf("指派=%d %s", applied.Code, applied.Body.String())
	}
	replay := postForm(t, s, path+"/assigned-user", form)
	if replay.Code != 303 {
		t.Fatalf("重放=%d", replay.Code)
	}
	form.Set("idempotency_key", "stale")
	rejected := postForm(t, s, path+"/assigned-user", form)
	if rejected.Code != 412 {
		t.Fatalf("過期版本=%d", rejected.Code)
	}
	form.Set("expected_revision", "1")
	form.Set("confirm_display_name", "別台")
	form.Set("idempotency_key", "wrong-name")
	rejected = postForm(t, s, path+"/assigned-user", form)
	if rejected.Code != 400 {
		t.Fatalf("名稱不符=%d", rejected.Code)
	}
	body := get(t, s, "/machines")
	if !strings.Contains(body, "user@example.com") {
		t.Fatal("清單缺少已指派使用者")
	}
	s.SetTailnetStatus(tailnet.Status{})
	body = assignedUserPage(t, s, path, "example.com/cap/clawctl-admin")
	if !strings.Contains(body, "來源不可用") || !strings.Contains(body, `value="none"`) {
		t.Fatal("來源不可用時缺少取消指派")
	}
	form.Set("user_id", "none")
	form.Set("confirm_display_name", "指派測試")
	form.Set("idempotency_key", "clear")
	if rec := postForm(t, s, path+"/assigned-user", form); rec.Code != 303 {
		t.Fatalf("取消=%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(get(t, s, path), "指派使用者：未指派") {
		t.Fatal("取消後未顯示未指派")
	}
}

func TestAssignedUserCopyUsesCurrentStatesAndActions(t *testing.T) {
	machineTemplate, err := os.ReadFile("templates/machine.html")
	if err != nil {
		t.Fatal(err)
	}
	section := strings.SplitN(strings.SplitN(string(machineTemplate), `<h2 id="action-assigned-user"`, 2)[1], `<h2 id="action-channel"`, 2)[0]
	for _, banned := range []string{"暫時", "尚未實作", "未接線", "遷移", "防禦", "免責", "fallback", "placeholder=", "disabled"} {
		if strings.Contains(section, banned) {
			t.Fatalf("指派表單含禁用文案 %s", banned)
		}
	}

	for _, path := range []string{"templates/machine_assigned_user_review.html", "assigned_user.go", "../../cmd/clawctl-hub/machineassignedusercmd.go", "../../internal/operatorclient/assigned_user.go"} {
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
