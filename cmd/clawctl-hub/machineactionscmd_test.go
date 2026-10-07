package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func machineActionsCLIServer(t *testing.T, f jobsFixture, operate, admin bool) (string, machineCommandDeps) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, grantedOperatorRequest(r, operate, admin))
	}))
	t.Cleanup(server.Close)
	return machineHTTPTestDeps(t, server)
}

func TestMachineActionsCLIPrintsWhatIsAvailableAndWhatIsBlocked(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	base, deps := machineActionsCLIServer(t, f, true, true)
	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"actions", "--hub-url", base, "--machine", f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"cnode-operator (" + f.machine.id + ") active",
		"ACTION", "STATE", "EFFECT",
		operator.MachineActionLabel(operator.MachineActionDiagnosticNoop),
		operator.MachineActionLabel(operator.MachineActionNotes),
		operator.MachineActionLabel(operator.MachineActionRetire),
		"撤銷註冊票: 這台沒有還沒兌換的註冊票。",
		"5 available, 2 blocked.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("輸出少了 %q：\n%s", want, text)
		}
	}
	if strings.Contains(text, operator.MachineActionLabel(operator.MachineActionRestore)) {
		t.Fatalf("服役中的機器不該印出恢復管理：\n%s", text)
	}
	// 表格那一欄要逐列講對，不能靠底下那句總計裡剛好也有「被擋」兩個字。
	for label, want := range map[string]string{
		operator.MachineActionLabel(operator.MachineActionDiagnosticNoop):    "available",
		operator.MachineActionLabel(operator.MachineActionRevokeEnrollToken): "blocked",
		operator.MachineActionLabel(operator.MachineActionConnect):           "blocked",
	} {
		row := machineActionsTableRow(t, text, label)
		if !strings.Contains(row, want) {
			t.Fatalf("%s 那一列應該是 %s：%q", label, want, row)
		}
	}
}

// machineActionsTableRow 取出表格裡以這個動作名稱開頭的那一列。
func machineActionsTableRow(t *testing.T, text, label string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, label+" ") {
			return line
		}
	}
	t.Fatalf("表格裡沒有 %q 那一列：\n%s", label, text)
	return ""
}

func TestMachineActionsCLIBlockedLinesCarryTheirNextStep(t *testing.T) {
	f := observedOperatorFixture(t)
	base, deps := machineActionsCLIServer(t, f, true, true)
	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"actions", "--hub-url", base, "--machine", f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	want := operator.MachineActionLabel(operator.MachineActionDiagnosticNoop) + ": 這台從來沒有報到過。  Next step: " +
		operator.MachineActionBlockerNextStep(operator.MachineActionBlockerNeverReported)
	if !strings.Contains(out.String(), want) {
		t.Fatalf("輸出少了 %q：\n%s", want, out.String())
	}
}

func TestMachineActionsCLIGivesAViewOnlyOperatorNoList(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	base, deps := machineActionsCLIServer(t, f, false, false)
	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"actions", "--hub-url", base, "--machine", f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "This credential does not cover any device actions.") ||
		strings.Contains(out.String(), "ACTION") {
		t.Fatalf("view-only 輸出=%s", out.String())
	}
}

func TestMachineActionsCLIJSONIsTheOperatorCatalogue(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	base, deps := machineActionsCLIServer(t, f, true, true)
	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"actions", "--hub-url", base, "--machine", f.machine.id, "--json",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	var catalogue operator.MachineActionCatalogue
	if err := json.Unmarshal(out.Bytes(), &catalogue); err != nil {
		t.Fatalf("decode: %v body=%s", err, out.String())
	}
	if catalogue.MachineID != f.machine.id || len(catalogue.Actions) != 7 ||
		catalogue.SchemaVersion != operator.MachineActionsSchemaVersion {
		t.Fatalf("catalogue=%+v", catalogue)
	}
}

func TestMachineActionsCLIShowsOpenTerminalBlockedUntilTheLinkIsUp(t *testing.T) {
	f := observedOperatorFixture(t)
	if _, err := f.store.DB().Exec(`UPDATE machine_registry SET assigned_user_id=? WHERE machine_id=?`,
		"42", f.machine.id); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: jobsTestNow.Add(time.Minute),
		Identity: model.Identity{Hostname: "cnode-operator", OS: "linux", Arch: "amd64", UnixUser: "example-user"},
	}, jobsTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	base, deps := machineActionsCLIServer(t, f, true, true)
	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"actions", "--hub-url", base, "--machine", f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	text := out.String()
	row := machineActionsTableRow(t, text, "開啟終端")
	if !strings.Contains(row, "blocked") {
		t.Fatalf("開啟終端那一列應該被擋：%q", row)
	}
	want := "開啟終端: 這台的終端連線目前沒有接上 Hub。  Next step: 確認這台機器上的 agent 與 bat-server 都在執行。"
	if !strings.Contains(text, want) {
		t.Fatalf("輸出少了 %q：\n%s", want, text)
	}
}

func TestMachineActionsCLINeedsAMachine(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{"actions"},
		&out, &errOut, productionMachineCommandDeps())
	if err == nil || !strings.Contains(err.Error(), "--machine is required") {
		t.Fatalf("err=%v", err)
	}
}
