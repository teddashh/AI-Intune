package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestLifecycleSubmenuListsActiveAndRetiredRegistryRows(t *testing.T) {
	s, st := newServer(t)
	activeID := onlineMachine(t, st, "lifecycle-active")
	retiredID := onlineMachine(t, st, "lifecycle-retired")
	if err := st.RetireMachine(retiredID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/lifecycle")
	for _, want := range []string{
		"裝置生命週期", "lifecycle-active", "lifecycle-retired",
		`action="/machines/` + activeID + `/lifecycle-preview"`,
		`action="/machines/` + retiredID + `/lifecycle-preview"`,
		`name="desired_state" value="retired"`, `name="desired_state" value="active"`,
		`<span class="st blue">active</span>`, `<span class="st grey">retired</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lifecycle submenu missing %q", want)
		}
	}
	if !strings.Contains(body, ">1</span><span class=\"l\">active 名冊列</span>") ||
		!strings.Contains(body, ">1</span><span class=\"l\">retired 名冊列</span>") {
		t.Fatalf("lifecycle submenu counts are wrong: %s", body)
	}
	if strings.Contains(body, "分母意圖") || strings.Contains(body, ">expected<") ||
		strings.Contains(body, ">not expected<") {
		t.Fatalf("lifecycle submenu exposed the legacy expected axis: %s", body)
	}
}

func TestLifecycleWebPreviewMakesBearerImpactExplicitAndApplyReplays(t *testing.T) {
	s, st := newServer(t)
	ticket, err := st.CreateEnrollToken("lifecycle-bearer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineID, agentToken, err := st.RedeemEnrollToken(ticket, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: ticket,
		Hostname: "lifecycle-bearer-host", OS: "linux", Arch: "amd64", UnixUser: "tester",
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.AuthenticateAgent(agentToken); err != nil || got != machineID {
		t.Fatalf("initial agent authentication=%q err=%v", got, err)
	}

	preview, form := previewMachineLifecycleForm(t, s, machineID, store.MachineLifecycleRetired, "replace hardware")
	detailBase := "/machines/" + machineID
	assertSubNavigationBody(t, s, "生命週期預覽頁", preview.Body.String(), "裝置詳細資料子選單", "動作", map[string]string{
		sectionHref(detailBase, "machine-overview"):     "概觀",
		sectionHref(detailBase, "monitor"):              "監視",
		sectionHref(detailBase, "properties"):           "屬性",
		sectionHref(detailBase, "apps-and-credentials"): "應用與憑證",
		sectionHref(detailBase, "jobs"):                 "工作單",
		sectionHref(detailBase, "actions"):              "動作",
		detailBase + "/timeline":                        "事件時間軸",
		detailBase + "/data":                            "資料",
	})
	for _, want := range []string{
		"active → retired", "有保留的 bearer", "authentication 允許 → 拒絕",
		"名冊列保留", "心跳、觀測與狀態歷史保留", "確認機器名稱",
	} {
		if !strings.Contains(preview.Body.String(), want) {
			t.Errorf("retire preview missing %q: %s", want, preview.Body.String())
		}
	}
	if strings.Contains(preview.Body.String(), "expected=") {
		t.Fatalf("lifecycle preview exposed the legacy expected axis: %s", preview.Body.String())
	}
	if counts := tableCounts(t, st, "audit_log", "operator_idempotency"); counts["audit_log"] != 0 || counts["operator_idempotency"] != 0 {
		t.Fatalf("preview wrote state: %v", counts)
	}

	form.Set("confirm", "lifecycle-bearer")
	first := postForm(t, s, "/machines/"+machineID+"/retire", form)
	second := postForm(t, s, "/machines/"+machineID+"/retire", form)
	if first.Code != http.StatusSeeOther || second.Code != http.StatusSeeOther {
		t.Fatalf("fresh/replay lifecycle apply=%d/%d bodies=%s / %s",
			first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	if _, err := st.AuthenticateAgent(agentToken); err == nil {
		t.Fatal("retired machine's retained bearer still authenticated")
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.RetiredAt == nil || machine.LifecycleRevision != 1 {
		t.Fatalf("retired projection=%+v err=%v", machine, err)
	}
	var transitions int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	entries, err := st.Audit(machineID, 10)
	if err != nil || transitions != 1 || len(entries) != 2 || !entries[0].OK || !entries[1].OK ||
		!entries[0].IsOperatorReplay() {
		t.Fatalf("replay evidence transitions=%d audit=%+v err=%v", transitions, entries, err)
	}

	restorePreview, restoreForm := previewMachineLifecycleForm(t, s, machineID, store.MachineLifecycleActive, "undo retire")
	for _, want := range []string{"retired → active", "authentication 拒絕 → 允許", "保留的 agent credential 與未過期 enrollment ticket 可重新使用"} {
		if !strings.Contains(restorePreview.Body.String(), want) {
			t.Errorf("restore preview missing %q: %s", want, restorePreview.Body.String())
		}
	}
	restoreForm.Set("confirm", "lifecycle-bearer")
	if rec := postForm(t, s, "/machines/"+machineID+"/unretire", restoreForm); rec.Code != http.StatusSeeOther {
		t.Fatalf("restore apply=%d: %s", rec.Code, rec.Body.String())
	}
	if got, err := st.AuthenticateAgent(agentToken); err != nil || got != machineID {
		t.Fatalf("restored retained bearer authentication=%q err=%v", got, err)
	}
}

func TestLifecycleWebPreviewRejectsBlankReasonWithoutWritingDecisionState(t *testing.T) {
	for _, reason := range []string{"", "   "} {
		t.Run(fmt.Sprintf("reason=%q", reason), func(t *testing.T) {
			s, st := newServer(t)
			machineID := onlineMachine(t, st, "lifecycle-empty-reason")
			preview := postForm(t, s, "/machines/"+machineID+"/lifecycle-preview", url.Values{
				"desired_state":     {"retired"},
				"expected_revision": {"0"},
				"reason":            {reason},
			})
			if preview.Code != http.StatusBadRequest ||
				strings.Contains(preview.Body.String(), `name="idempotency_key"`) {
				t.Fatalf("blank-reason lifecycle preview=%d: %s", preview.Code, preview.Body.String())
			}
			if counts := tableCounts(t, st, "audit_log", "operator_idempotency"); counts["audit_log"] != 0 || counts["operator_idempotency"] != 0 {
				t.Fatalf("blank-reason lifecycle preview wrote decision state: %v", counts)
			}
		})
	}
}

func TestLifecycleWebPreviewBlocksRetireWithNonterminalJob(t *testing.T) {
	s, st := newServer(t)
	machineID := onlineMachine(t, st, "lifecycle-busy")
	createWebJob(t, st, machineID, false)
	machine, err := st.GetMachine(machineID)
	if err != nil {
		t.Fatal(err)
	}
	preview := postForm(t, s, "/machines/"+machineID+"/lifecycle-preview", url.Values{
		"desired_state": {"retired"}, "expected_revision": {"0"}, "reason": {"retire busy machine"},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "nonterminal_jobs") ||
		!strings.Contains(preview.Body.String(), "非終態工作單</th><td>1") ||
		strings.Contains(preview.Body.String(), `name="idempotency_key"`) {
		t.Fatalf("blocked lifecycle preview=%d: %s", preview.Code, preview.Body.String())
	}
	after, err := st.GetMachine(machineID)
	if err != nil || after.RetiredAt != nil || after.LifecycleRevision != machine.LifecycleRevision {
		t.Fatalf("blocked preview changed lifecycle before=%+v after=%+v err=%v", machine, after, err)
	}
	if counts := tableCounts(t, st, "audit_log", "operator_idempotency"); counts["audit_log"] != 0 || counts["operator_idempotency"] != 0 {
		t.Fatalf("blocked preview wrote decision state: %v", counts)
	}
}

func TestLifecycleWebRestoreMakesPendingEnrollmentTicketRedeemableAgain(t *testing.T) {
	s, st := newServer(t)
	machineID, ticket, err := st.CreateEnrollTokenFor("lifecycle-pending", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	preview, retireForm := previewMachineLifecycleForm(t, s, machineID, store.MachineLifecycleRetired, "pause enrollment")
	for _, want := range []string{"1 張（其中 0 張已過期）", "redemption 允許 → 拒絕"} {
		if !strings.Contains(preview.Body.String(), want) {
			t.Errorf("pending-ticket retire preview missing %q: %s", want, preview.Body.String())
		}
	}
	retireForm.Set("confirm", "lifecycle-pending")
	if rec := postForm(t, s, "/machines/"+machineID+"/retire", retireForm); rec.Code != http.StatusSeeOther {
		t.Fatalf("pending machine retire=%d: %s", rec.Code, rec.Body.String())
	}
	request := model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: ticket,
		Hostname: "pending-host", OS: "linux", Arch: "amd64", UnixUser: "tester",
	}
	if _, _, err := st.RedeemEnrollToken(ticket, request, time.Now().UTC()); !errors.Is(err, store.ErrTokenRetired) {
		t.Fatalf("retired pending ticket redemption err=%v", err)
	}

	restorePreview, restoreForm := previewMachineLifecycleForm(t, s, machineID, store.MachineLifecycleActive, "resume enrollment")
	if !strings.Contains(restorePreview.Body.String(), "redemption 拒絕 → 允許") {
		t.Fatalf("pending-ticket restore impact not explicit: %s", restorePreview.Body.String())
	}
	restoreForm.Set("confirm", "lifecycle-pending")
	if rec := postForm(t, s, "/machines/"+machineID+"/unretire", restoreForm); rec.Code != http.StatusSeeOther {
		t.Fatalf("pending machine restore=%d: %s", rec.Code, rec.Body.String())
	}
	if gotID, agentToken, err := st.RedeemEnrollToken(ticket, request, time.Now().UTC()); err != nil ||
		gotID != machineID || agentToken == "" {
		t.Fatalf("restored pending ticket redemption id=%q token=%t err=%v", gotID, agentToken != "", err)
	}
}
