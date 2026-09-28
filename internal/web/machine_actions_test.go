package web

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func actionsPanel(body string) string {
	i := strings.Index(body, `id="actions"`)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := strings.Index(rest, `id="action-channel"`); j > 0 {
		return rest[:j]
	}
	return rest
}

var machineActionHrefPattern = regexp.MustCompile(`href="#(action-[a-z-]+)"`)

// listsAction 只看目錄第一欄那個格子，不看整段文字：blocker 的句子裡也會出現
// 「退役」「恢復管理」這些詞。
func listsAction(panel string, kind operator.MachineActionKind) bool {
	label := operator.MachineActionLabel(kind)
	return strings.Contains(panel, ">"+label+"</a>") || strings.Contains(panel, "<td>"+label+"</td>")
}

// 目錄說得出口的每一件事，操作員都必須在同一頁按得到。一個指向不存在段落的
// 連結，等於目錄列了一個做不到的動作。
func TestEveryAvailableActionLinksToSomewhereOnThisPage(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)
	panel := actionsPanel(body)
	if panel == "" {
		t.Fatalf("單機頁沒有動作那一節：\n%s", tail(body, 400))
	}
	targets := machineActionHrefPattern.FindAllStringSubmatch(panel, -1)
	if len(targets) == 0 {
		t.Fatalf("動作目錄沒有任何可按的連結：\n%s", panel)
	}
	for _, match := range targets {
		if !strings.Contains(body, `id="`+match[1]+`"`) {
			t.Fatalf("動作連到 #%s，但這一頁沒有那個段落", match[1])
		}
	}
	for _, kind := range operator.MachineActionKinds() {
		if machineActionAnchors[kind] == "" {
			t.Fatalf("%s 沒有對應到單機頁上的任何段落", kind)
		}
	}
}

func TestTheActionCatalogueNamesEveryActionItsStateAndItsEffect(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	panel := actionsPanel(get(t, s, "/machines/"+id))
	for _, kind := range []operator.MachineActionKind{
		operator.MachineActionConnect, operator.MachineActionDiagnosticNoop,
		operator.MachineActionRename, operator.MachineActionNotes,
		operator.MachineActionChannel, operator.MachineActionRevokeEnrollToken,
		operator.MachineActionRetire,
	} {
		if !strings.Contains(panel, operator.MachineActionLabel(kind)) ||
			!strings.Contains(panel, operator.MachineActionEffect(kind)) {
			t.Fatalf("目錄少了 %s 的名稱或後果：\n%s", kind, panel)
		}
	}
	if listsAction(panel, operator.MachineActionRestore) {
		t.Fatalf("服役中的機器不該列出恢復管理：\n%s", panel)
	}
	if !strings.Contains(panel, `<span class="st green">可用</span>`) ||
		!strings.Contains(panel, `<span class="st amber">被擋</span>`) {
		t.Fatalf("目錄沒有分出可用與被擋：\n%s", panel)
	}
	if !strings.Contains(panel, "現在可用 <strong>6</strong> 項，被擋 <strong>1</strong> 項。") {
		t.Fatalf("目錄沒有講出可用與被擋的張數：\n%s", panel)
	}
}

func TestABlockedActionSaysWhatIsTrueNowAndWhatToDoNext(t *testing.T) {
	s, st := newServer(t)
	// 只註冊、從沒回報過的機器：連不上、做不了診斷，票也早就兌換掉了。
	id := enroll(t, st, "quiet1", time.Now().UTC().Add(-time.Hour))
	panel := actionsPanel(get(t, s, "/machines/"+id))
	want := "這台沒有還沒兌換的註冊票。"
	if !strings.Contains(panel, want) {
		t.Fatalf("被擋的撤票沒有說明現況：\n%s", panel)
	}
	next := operator.MachineActionBlockerNextStep(operator.MachineActionBlockerNoConnectAddress)
	if !strings.Contains(panel, "這台還沒給出可以連過去的 bat-server 位址。 "+next) {
		t.Fatalf("被擋的連線沒有把下一步接在現況後面：\n%s", panel)
	}
	drill := operator.MachineActionBlockerNextStep(operator.MachineActionBlockerNeverReported)
	if !strings.Contains(panel, "這台從來沒有報到過。 "+drill) {
		t.Fatalf("被擋的診斷沒有把下一步接在現況後面：\n%s", panel)
	}
	if strings.Contains(panel, `href="#action-connect"`) ||
		strings.Contains(panel, `href="#action-diagnostic"`) {
		t.Fatalf("被擋的動作不該是可按的連結：\n%s", panel)
	}
	if !strings.Contains(panel, "現在可用 <strong>4</strong> 項，被擋 <strong>3</strong> 項。") {
		t.Fatalf("張數不對：\n%s", panel)
	}
}

func TestARetiredMachineIsOfferedRestoreNotRetirement(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	if err := st.RetireMachine(id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/machines/"+id)
	panel := actionsPanel(body)
	if !listsAction(panel, operator.MachineActionRestore) || listsAction(panel, operator.MachineActionRetire) {
		t.Fatalf("退役後的目錄=%s", panel)
	}
	if !strings.Contains(panel, `href="#action-lifecycle"`) ||
		!strings.Contains(body, `id="action-lifecycle"`) {
		t.Fatalf("恢復管理沒有連到生命週期那一段：\n%s", panel)
	}
	if !strings.Contains(panel, operator.MachineActionBlockerNextStep(operator.MachineActionBlockerRetired)) {
		t.Fatalf("退役擋住的動作沒有講出下一步：\n%s", panel)
	}
}

func TestAViewOnlyOperatorHasNoActionSectionAtAll(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	body := renderWithCapabilities(t, s, "/machines/"+id,
		operatorauth.CapabilityNames{View: names.View})
	if strings.Contains(body, `id="actions"`) || strings.Contains(body, "#actions") {
		t.Fatalf("view-only 還是看到了動作那一節或它的選單項")
	}
	if !strings.Contains(body, "#monitor") {
		t.Fatalf("view-only 的子選單整個不見了：\n%s", tail(body, 400))
	}
	operateBody := renderWithCapabilities(t, s, "/machines/"+id,
		operatorauth.CapabilityNames{View: names.View, Operate: names.Operate})
	panel := actionsPanel(operateBody)
	if !listsAction(panel, operator.MachineActionDiagnosticNoop) ||
		listsAction(panel, operator.MachineActionRetire) {
		t.Fatalf("operate-only 的目錄=%s", panel)
	}
}

func TestTheDiagnosticDrillCanBeStartedFromTheMachineItself(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, `action="/machines/`+id+`/diagnostic-noop-preview"`) ||
		!strings.Contains(body, `id="action-diagnostic"`) {
		t.Fatalf("單機頁上沒有診斷表單：\n%s", tail(body, 600))
	}
	if !strings.Contains(body, `name="timeout"`) || !strings.Contains(body, `name="reason"`) {
		t.Fatalf("診斷表單少了逾時或理由欄位")
	}
}

func TestTheCataloguePageAgreesWithTheServiceItProjects(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	now := time.Now().UTC()
	detail, err := s.operator.MachineDetail(id, now)
	if err != nil {
		t.Fatal(err)
	}
	connect, err := s.operator.MachineConnect(detail)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := s.operator.MachineLifecycle(id)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := s.operator.MachineActions(operator.MachineActionsRequest{
		Detail: detail, Connect: connect, Lifecycle: lifecycle,
		Granted: operator.MachineActionGrant{Operate: true, Admin: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	view := machineActionsViewFrom(catalogue)
	if view == nil || len(view.Rows) != len(catalogue.Actions) {
		t.Fatalf("view=%+v", view)
	}
	panel := actionsPanel(get(t, s, "/machines/"+id))
	for _, row := range view.Rows {
		if row.Available && !strings.Contains(panel, `href="#`+row.Anchor+`"`) {
			t.Fatalf("%s 在服務裡可用，畫面卻沒給連結：\n%s", row.Kind, panel)
		}
		if !row.Available && !strings.Contains(panel, row.Situation) {
			t.Fatalf("%s 被擋，畫面沒說現況：\n%s", row.Kind, panel)
		}
	}
	if machineActionsViewFrom(operator.MachineActionCatalogue{
		MachineID: id, State: store.MachineLifecycleActive,
	}) != nil {
		t.Fatal("空目錄應該整個不出現")
	}
}
