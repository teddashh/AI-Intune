package web

import (
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/sessionid"
	"github.com/teddashh/AI-Intune/internal/store"
)

type machinePageLink struct{}

func (machinePageLink) Send(agentrelay.Downstream) error { return nil }
func (machinePageLink) Close(string)                     {}

func assignMachinePageUser(t *testing.T, st *store.Store, machineID, userID, login string) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE machine_registry SET assigned_user_id=?, assigned_user_login=? WHERE machine_id=?`,
		userID, login, machineID); err != nil {
		t.Fatal(err)
	}
}

func linkMachinePage(t *testing.T, s *Server, machineID string) {
	t.Helper()
	links := agentlink.New()
	if _, err := links.Attach(machineID, machinePageLink{}); err != nil {
		t.Fatal(err)
	}
	s.SetTerminalLinks(links)
}

func terminalForm(t *testing.T, body string) string {
	t.Helper()
	const marker = `id="action-terminal"`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("單機頁沒有終端段落：\n%s", tail(body, 500))
	}
	rest := body[i:]
	end := strings.Index(rest, "</form>")
	if end < 0 {
		t.Fatalf("終端段落沒有表單：\n%s", rest)
	}
	return rest[:end+len("</form>")]
}

func TestTheAssignedUserOnALinkedMachineGetsAnOpenTerminalForm(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	assignMachinePageUser(t, st, id, "42", "owner@example.com")
	linkMachinePage(t, s, id)

	first := get(t, s, "/machines/"+id)
	panel := actionsPanel(first)
	if !strings.Contains(panel, `href="#action-terminal">開啟終端</a>`) {
		t.Fatalf("動作目錄沒有把開啟終端連到終端段落：\n%s", panel)
	}
	form := terminalForm(t, first)
	if strings.Count(first, `action="/machines/`+id+`/terminals"`) != 1 ||
		!strings.Contains(form, `action="/machines/`+id+`/terminals"`) {
		t.Fatalf("終端表單不是唯一一個貼到這台的開啟路徑：\n%s", form)
	}
	if strings.Contains(form, "target=") {
		t.Fatalf("終端表單開了新分頁：\n%s", form)
	}
	sessionID := hiddenFormValue(t, form, "session_id")
	key := hiddenFormValue(t, form, "idempotency_key")
	if !sessionid.Valid(sessionID) {
		t.Fatalf("session_id 無效：%q", sessionID)
	}
	if !strings.HasPrefix(key, "web-machine-terminal-") {
		t.Fatalf("idempotency_key=%q", key)
	}
	if !strings.Contains(form, operator.MachineActionEffect(operator.MachineActionOpenTerminal)) ||
		!strings.Contains(form, ">開啟終端</button>") {
		t.Fatalf("終端段落少了後果或按鈕：\n%s", form)
	}

	second := terminalForm(t, get(t, s, "/machines/"+id))
	if hiddenFormValue(t, second, "session_id") == sessionID || hiddenFormValue(t, second, "idempotency_key") == key {
		t.Fatal("兩次渲染用了同一組開啟識別碼")
	}
}

func TestAnUnlinkedMachineShowsOpenTerminalBlockedAndNoForm(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	assignMachinePageUser(t, st, id, "42", "owner@example.com")
	s.SetTerminalLinks(agentlink.New())
	assertOpenTerminalBlockedWithoutAForm(t, s, id)
}

func TestAMachinePageWithNoTerminalRegistryStillRendersTheBlockedRow(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	assignMachinePageUser(t, st, id, "42", "owner@example.com")
	assertOpenTerminalBlockedWithoutAForm(t, s, id)
}

func assertOpenTerminalBlockedWithoutAForm(t *testing.T, s *Server, id string) {
	t.Helper()
	body := get(t, s, "/machines/"+id)
	panel := actionsPanel(body)
	if !strings.Contains(panel, "<td>開啟終端</td>") || strings.Contains(panel, `href="#action-terminal"`) {
		t.Fatalf("被擋的開啟終端不該是連結：\n%s", panel)
	}
	if !strings.Contains(panel, "這台的終端連線目前沒有接上 Hub。 "+
		operator.MachineActionBlockerNextStep(operator.MachineActionBlockerTerminalNotLinked)) {
		t.Fatalf("被擋的開啟終端沒有講出現況與下一步：\n%s", panel)
	}
	if strings.Contains(body, `id="action-terminal"`) || strings.Contains(body, `/terminals`) {
		t.Fatalf("被擋的開啟終端還是畫了表單：\n%s", body)
	}
}

func TestAnotherUserSeesNoOpenTerminalRow(t *testing.T) {
	s, st := newServer(t)
	plain := onlineMachine(t, st, "samplehub1")
	id := onlineMachine(t, st, "sampleagent1")
	assignMachinePageUser(t, st, id, "99", "other@example.com")
	body := get(t, s, "/machines/"+id)
	if strings.Contains(body, "開啟終端") || strings.Contains(body, `id="action-terminal"`) {
		t.Fatalf("別人的機器列出了開啟終端：\n%s", actionsTable(body))
	}
	plainBody := get(t, s, "/machines/"+plain)
	if actionsTable(body) != actionsTable(plainBody) {
		t.Fatalf("其餘動作表變了：\n%s", actionsTable(body))
	}
	if !strings.Contains(actionsPanel(body), "現在可用 <strong>6</strong> 項，被擋 <strong>1</strong> 項。") ||
		!strings.Contains(actionsPanel(plainBody), "現在可用 <strong>6</strong> 項，被擋 <strong>1</strong> 項。") {
		t.Fatalf("動作張數變了：\n%s", actionsPanel(body))
	}
}

func actionsTable(body string) string {
	panel := actionsPanel(body)
	start := strings.Index(panel, "<table>")
	end := strings.Index(panel, "</table>")
	if start < 0 || end < 0 {
		return panel
	}
	return panel[start : end+len("</table>")]
}
