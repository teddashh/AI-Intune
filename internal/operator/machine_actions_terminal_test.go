package operator

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/teddashh/AI-Intune/internal/store"
)

func assignCatalogueUser(t *testing.T, st *store.Store, machineID, userID, login string) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE machine_registry SET assigned_user_id=?, assigned_user_login=? WHERE machine_id=?`,
		userID, login, machineID); err != nil {
		t.Fatal(err)
	}
}

func setCataloguePlatform(t *testing.T, st *store.Store, machineID, osName, arch string) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE machine_registry SET os=?, arch=? WHERE machine_id=?`,
		osName, arch, machineID); err != nil {
		t.Fatal(err)
	}
}

func openCatalogueTerminals(t *testing.T, st *store.Store, machineID, userID string, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("catalogue-term-%02d", i)
		if _, err := st.OpenAgentSession(store.OpenAgentSessionRequest{
			SessionID: ids[i], MachineID: machineID,
			OperatorTailnetUserID: userID, OperatorTailnetUserLogin: "ted@example.com",
			IdempotencyKey: "catalogue-open-" + ids[i], RequestDigest: "sha256:" + ids[i],
			Audit: store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func TestTheAssignedUserOnALinkedLinuxMachineCanOpenATerminal(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "terminal-open", enabledCheckin())
	assignCatalogueUser(t, st, machineID, "42", "ted@example.com")
	base := catalogueOf(t, service, machineID, now, allGrants())
	got := catalogueWith(t, service, machineID, now, allGrants(), "42", "42", true)
	action := actionIn(t, got, MachineActionOpenTerminal)
	if !action.Available || action.Blocker != "" || action.Situation != "" || action.NextStep != "" ||
		action.Label != "開啟終端" || action.Effect != MachineActionEffect(MachineActionOpenTerminal) ||
		action.Capability != MachineActionCapabilityOperate || action.Surface != MachineActionSurfaceWeb ||
		action.Confirm != MachineActionConfirmNone || action.Method != "" || action.Path != "" || action.PreviewPath != "" {
		t.Fatalf("open_terminal=%+v", action)
	}
	if len(got.Actions) != len(base.Actions)+1 || got.Actions[0].Kind != MachineActionConnect ||
		got.Actions[1].Kind != MachineActionOpenTerminal ||
		!reflect.DeepEqual(got.Actions[2:], base.Actions[1:]) {
		t.Fatalf("開啟終端沒有緊接在連線後面：base=%v got=%v", kindsOf(base), kindsOf(got))
	}
}

func TestOpenTerminalIsOmittedForAnyoneButTheAssignedUser(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "terminal-identity", enabledCheckin())
	full := catalogueOf(t, service, machineID, now, allGrants())
	admin := catalogueOf(t, service, machineID, now, MachineActionGrant{Admin: true})
	cases := []struct {
		name     string
		grant    MachineActionGrant
		caller   string
		assigned string
		base     MachineActionCatalogue
	}{
		{name: "unassigned", grant: allGrants(), caller: "42", base: full},
		{name: "unassigned and no caller", grant: allGrants(), base: full},
		{name: "another user", grant: allGrants(), caller: "42", assigned: "7", base: full},
		{name: "empty caller", grant: allGrants(), assigned: "42", base: full},
		{name: "without operate", grant: MachineActionGrant{Admin: true}, caller: "42", assigned: "42", base: admin},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			assignCatalogueUser(t, st, machineID, test.assigned, "someone@example.com")
			got := catalogueWith(t, service, machineID, now, test.grant, test.caller, test.assigned, true)
			absentIn(t, got, MachineActionOpenTerminal)
			if !reflect.DeepEqual(got.Actions, test.base.Actions) {
				t.Fatalf("其餘目錄變了：base=%v got=%v", kindsOf(test.base), kindsOf(got))
			}
		})
	}
}

func TestOpenTerminalIsOmittedOnMachinesThatCannotHoldATerminal(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "terminal-platform", enabledCheckin())
	assignCatalogueUser(t, st, machineID, "42", "ted@example.com")
	base := catalogueOf(t, service, machineID, now, allGrants())
	for _, identity := range []struct{ name, osName, arch string }{
		{name: "windows", osName: "Windows 11", arch: "amd64"},
		{name: "macos", osName: "macOS 15", arch: "arm64"},
		{name: "empty", osName: "", arch: ""},
	} {
		t.Run(identity.name, func(t *testing.T) {
			setCataloguePlatform(t, st, machineID, identity.osName, identity.arch)
			got := catalogueWith(t, service, machineID, now, allGrants(), "42", "42", true)
			absentIn(t, got, MachineActionOpenTerminal)
			if !reflect.DeepEqual(got.Actions, base.Actions) {
				t.Fatalf("其餘目錄變了：base=%v got=%v", kindsOf(base), kindsOf(got))
			}
		})
	}
}

func TestARetiredMachineBlocksOpenTerminalBeforeTheLink(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "terminal-retired", enabledCheckin())
	assignCatalogueUser(t, st, machineID, "42", "ted@example.com")
	if err := st.RetireMachine(machineID, now); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := service.MachineLifecycle(machineID)
	if err != nil {
		t.Fatal(err)
	}
	action := actionIn(t, catalogueWith(t, service, machineID, now, allGrants(), "42", "42", false),
		MachineActionOpenTerminal)
	if action.Available || action.Blocker != MachineActionBlockerRetired ||
		action.Situation != machineRetiredSituation(lifecycle) ||
		action.NextStep != MachineActionBlockerNextStep(MachineActionBlockerRetired) {
		t.Fatalf("retired open_terminal=%+v", action)
	}
}

func TestAnUnlinkedTerminalBlocksOpenTerminalBeforeTheCap(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "terminal-unlinked", enabledCheckin())
	assignCatalogueUser(t, st, machineID, "42", "ted@example.com")
	openCatalogueTerminals(t, st, machineID, "42", store.MaxOpenAgentSessionsPerMachine)
	action := actionIn(t, catalogueWith(t, service, machineID, now, allGrants(), "42", "42", false),
		MachineActionOpenTerminal)
	if action.Available || action.Blocker != MachineActionBlockerTerminalNotLinked ||
		action.Situation != "這台的終端連線目前沒有接上 Hub。" ||
		action.NextStep != MachineActionBlockerNextStep(MachineActionBlockerTerminalNotLinked) {
		t.Fatalf("unlinked open_terminal=%+v", action)
	}
}

func TestOpenTerminalBlocksAtFourOpenRowsAndIgnoresClosedOnes(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "terminal-cap", enabledCheckin())
	assignCatalogueUser(t, st, machineID, "42", "ted@example.com")
	openCatalogueTerminals(t, st, machineID, "42", store.MaxOpenAgentSessionsPerMachine-1)
	below := actionIn(t, catalogueWith(t, service, machineID, now, allGrants(), "42", "42", true),
		MachineActionOpenTerminal)
	if !below.Available {
		t.Fatalf("三個開啟中的終端卻被擋：%+v", below)
	}
	if _, err := st.OpenAgentSession(store.OpenAgentSessionRequest{
		SessionID: "catalogue-term-full", MachineID: machineID,
		OperatorTailnetUserID: "42", OperatorTailnetUserLogin: "ted@example.com",
		IdempotencyKey: "catalogue-open-full", RequestDigest: "sha256:catalogue-term-full",
		Audit: store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	full := actionIn(t, catalogueWith(t, service, machineID, now, allGrants(), "42", "42", true),
		MachineActionOpenTerminal)
	want := fmt.Sprintf("這台已有 %d 個開啟中的終端，已達上限。", store.MaxOpenAgentSessionsPerMachine)
	if full.Available || full.Blocker != MachineActionBlockerTerminalLimitReached ||
		full.Situation != want ||
		full.NextStep != MachineActionBlockerNextStep(MachineActionBlockerTerminalLimitReached) {
		t.Fatalf("滿額 open_terminal=%+v", full)
	}
	if _, err := st.CloseAgentSessionsByID([]string{"catalogue-term-full"}, store.AgentSessionCloseReasonViewerClosed); err != nil {
		t.Fatal(err)
	}
	after := actionIn(t, catalogueWith(t, service, machineID, now, allGrants(), "42", "42", true),
		MachineActionOpenTerminal)
	if !after.Available {
		t.Fatalf("關掉一列之後仍被擋：%+v", after)
	}
}

func kindsOf(catalogue MachineActionCatalogue) []MachineActionKind {
	out := make([]MachineActionKind, len(catalogue.Actions))
	for i, action := range catalogue.Actions {
		out[i] = action.Kind
	}
	return out
}
