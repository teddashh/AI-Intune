package operator

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func machineActionsFixture(t *testing.T, name string, jobsEnabled *bool) (*Service, *store.Store, string, time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	machineID, token, err := st.CreateEnrollTokenFor(name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: name, OS: "linux", Arch: "amd64", UnixUser: "operator-test",
	}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now.Add(-30 * time.Second),
		AgentVersion: "v-test", AgentStartedAt: now.Add(-time.Hour), AgentSeq: 1,
		JobsEnabled: jobsEnabled,
	}, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	return New(st), st, machineID, now
}

func enabledCheckin() *bool { value := true; return &value }

func allGrants() MachineActionGrant { return MachineActionGrant{Operate: true, Admin: true} }

func catalogueOf(t *testing.T, service *Service, machineID string, now time.Time,
	grant MachineActionGrant,
) MachineActionCatalogue {
	t.Helper()
	detail, err := service.MachineDetail(machineID, now)
	if err != nil {
		t.Fatal(err)
	}
	connect, err := service.MachineConnect(detail)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := service.MachineLifecycle(machineID)
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := service.MachineActions(MachineActionsRequest{
		Detail: detail, Connect: connect, Lifecycle: lifecycle, Granted: grant,
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalogue
}

func actionIn(t *testing.T, catalogue MachineActionCatalogue, kind MachineActionKind) MachineAction {
	t.Helper()
	for _, action := range catalogue.Actions {
		if action.Kind == kind {
			return action
		}
	}
	t.Fatalf("目錄裡沒有 %s：%+v", kind, catalogue.Actions)
	return MachineAction{}
}

func absentIn(t *testing.T, catalogue MachineActionCatalogue, kind MachineActionKind) {
	t.Helper()
	for _, action := range catalogue.Actions {
		if action.Kind == kind {
			t.Fatalf("目錄不該列出 %s：%+v", kind, action)
		}
	}
}

func TestMachineActionsSchemaVersionIncludesSurface(t *testing.T) {
	if MachineActionsSchemaVersion != 4 {
		t.Fatalf("machine actions schema=%d want=4", MachineActionsSchemaVersion)
	}
	if kinds := MachineActionKinds(); len(kinds) != 8 || kinds[2] != MachineActionRename || kinds[3] != MachineActionNotes {
		t.Fatalf("machine action kinds=%v", kinds)
	}
}

// 開一張真的診斷工作單，讓這台機器留下一張未結束的工作單。
func startDiagnosticJob(t *testing.T, service *Service, machineID string) {
	t.Helper()
	preview, err := service.PreviewDiagnosticNoop(DiagnosticNoopPreviewRequest{
		MachineID: machineID, ExecutionTimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateDiagnosticNoop(DiagnosticNoopRequest{
		MachineID: machineID, ExecutionTimeoutSeconds: 60,
		ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "catalogue test", IdempotencyKey: "catalogue-diagnostic",
		Actor: Actor{SourceKind: SourceKindOperatorAPI, AuthSubject: "test"},
	}); err != nil {
		t.Fatal(err)
	}
}

// 「這個操作員碰得到任何一個動作嗎」只能有一個答案：目錄給的那個。子選單用的是
// 這個函式，目錄用的是同一組 shape，所以兩邊不會對同一個 capability 講不同的話。
func TestWhetherAnyActionIsReachableIsTheSameQuestionTheCatalogueAnswers(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "actions-reachable", enabledCheckin())
	for _, grant := range []MachineActionGrant{
		{}, {Operate: true}, {Admin: true}, {Operate: true, Admin: true},
	} {
		catalogue := catalogueOf(t, service, machineID, now, grant)
		if got := MachineActionsReachable(grant); got != (len(catalogue.Actions) > 0) {
			t.Fatalf("grant=%+v 說碰得到=%t，目錄有 %d 項", grant, got, len(catalogue.Actions))
		}
	}
}

func TestEveryActionSaysWhatItIsWhoMayRunItAndWhereItGoes(t *testing.T) {
	kinds := MachineActionKinds()
	if len(kinds) != len(machineActionShapes) {
		t.Fatalf("kinds=%d shapes=%d", len(kinds), len(machineActionShapes))
	}
	seen := map[string]MachineActionKind{}
	for _, kind := range kinds {
		shape := machineActionShapes[kind]
		if shape.label == "" || shape.effect == "" {
			t.Fatalf("%s 少了名字或後果：%+v", kind, shape)
		}
		switch shape.surface {
		case MachineActionSurfaceOperatorAPI:
			if shape.method == "" || shape.path == "" {
				t.Fatalf("%s 的 operator API surface 沒有方法或路徑：%+v", kind, shape)
			}
		case MachineActionSurfaceWeb:
			if shape.method != "" || shape.path != "" || shape.previewPath != "" {
				t.Fatalf("%s 的 web surface 帶了方法或路徑：%+v", kind, shape)
			}
		default:
			t.Fatalf("%s 的 surface 是 %q", kind, shape.surface)
		}
		if MachineActionLabel(kind) != shape.label || MachineActionEffect(kind) != shape.effect {
			t.Fatalf("%s 的對外名稱與 shape 不一致", kind)
		}
		switch shape.capability {
		case MachineActionCapabilityOperate, MachineActionCapabilityAdmin:
		default:
			t.Fatalf("%s 的 capability 是 %q", kind, shape.capability)
		}
		switch shape.confirm {
		case MachineActionConfirmNone, MachineActionConfirmDisplayName:
		default:
			t.Fatalf("%s 的 confirm 是 %q", kind, shape.confirm)
		}
		if shape.surface == MachineActionSurfaceOperatorAPI {
			key := shape.method + " " + shape.path
			if other, dup := seen[key]; dup && !(kind == MachineActionRestore && other == MachineActionRetire) {
				t.Fatalf("%s 與 %s 指到同一條路徑 %s", kind, other, key)
			}
			seen[key] = kind
		}
	}
}

func TestEveryBlockerSaysWhatIsTrueNow(t *testing.T) {
	blockers := []MachineActionBlocker{
		MachineActionBlockerRetired, MachineActionBlockerNeverReported,
		MachineActionBlockerExecutionUnknown, MachineActionBlockerExecutionDisabled,
		MachineActionBlockerActiveJobs, MachineActionBlockerNoPendingToken,
		MachineActionBlockerNoConnectAddress,
	}
	// 只有「操作員得先去清掉」的 blocker 才有下一步；沒東西可撤銷不是待辦事項。
	withoutNextStep := map[MachineActionBlocker]bool{MachineActionBlockerNoPendingToken: true}
	for _, blocker := range blockers {
		next := MachineActionBlockerNextStep(blocker)
		if withoutNextStep[blocker] != (next == "") {
			t.Fatalf("%s 的下一步是 %q", blocker, next)
		}
	}
	if MachineActionBlockerNextStep("no-such-blocker") != "" {
		t.Fatal("不認得的 blocker 不該生出下一步")
	}
}

func TestTheCatalogueOnlyListsWhatThisOperatorMayRun(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "grants", enabledCheckin())
	full := catalogueOf(t, service, machineID, now, allGrants())
	if len(full.Actions) != 7 || full.Available+full.Blocked != len(full.Actions) {
		t.Fatalf("full=%+v", full)
	}
	operateOnly := catalogueOf(t, service, machineID, now, MachineActionGrant{Operate: true})
	for _, action := range operateOnly.Actions {
		if action.Capability != MachineActionCapabilityOperate {
			t.Fatalf("operate-only 看到 %+v", action)
		}
	}
	absentIn(t, operateOnly, MachineActionRetire)
	absentIn(t, operateOnly, MachineActionChannel)
	if len(operateOnly.Actions) != 2 {
		t.Fatalf("operate-only 目錄=%+v", operateOnly.Actions)
	}
	none := catalogueOf(t, service, machineID, now, MachineActionGrant{})
	if len(none.Actions) != 0 || none.Available != 0 || none.Blocked != 0 {
		t.Fatalf("沒有任何 capability 卻看到 %+v", none)
	}
}

func TestTheCatalogueNamesTheMachineItJustRead(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "identity", enabledCheckin())
	catalogue := catalogueOf(t, service, machineID, now, allGrants())
	if catalogue.SchemaVersion != MachineActionsSchemaVersion || catalogue.MachineID != machineID ||
		catalogue.DisplayName != "identity" || catalogue.State != MachineLifecycleStateActive ||
		!catalogue.EvaluatedAt.Equal(now) {
		t.Fatalf("catalogue=%+v", catalogue)
	}
}

func TestTheCatalogueRefusesSourcesAboutDifferentMachines(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "coherence-a", enabledCheckin())
	otherService, _, otherID, _ := machineActionsFixture(t, "coherence-b", enabledCheckin())
	detail, err := service.MachineDetail(machineID, now)
	if err != nil {
		t.Fatal(err)
	}
	connect, err := service.MachineConnect(detail)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := service.MachineLifecycle(machineID)
	if err != nil {
		t.Fatal(err)
	}
	otherLifecycle, err := otherService.MachineLifecycle(otherID)
	if err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]MachineActionsRequest{
		"沒有 detail":    {Connect: connect, Lifecycle: lifecycle, Granted: allGrants()},
		"另一台的 connect": {Detail: detail, Connect: MachineConnectResult{MachineID: otherID}, Lifecycle: lifecycle, Granted: allGrants()},
		"另一台的生命週期":     {Detail: detail, Connect: connect, Lifecycle: otherLifecycle, Granted: allGrants()},
	} {
		if _, err := service.MachineActions(req); err == nil {
			t.Fatalf("%s 沒有被拒絕", name)
		}
	}
}

func TestConnectWaitsForAnAddressTheAgentHasNotGivenYet(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "connect-gap", enabledCheckin())
	blocked := actionIn(t, catalogueOf(t, service, machineID, now, allGrants()), MachineActionConnect)
	if blocked.Available || blocked.Blocker != MachineActionBlockerNoConnectAddress ||
		blocked.Situation == "" || blocked.NextStep == "" {
		t.Fatalf("connect=%+v", blocked)
	}
	recordMachineConnectObservation(t, st, machineID, model.Identity{
		Hostname: "connect-gap", OS: "linux", Arch: "amd64", UnixUser: "operator-test",
		TailscaleIP: "100.64.0.21",
	}, model.BAT{Running: true, Port: 4321, ListenAddrs: []string{"100.64.0.21"}},
		now.Add(-2*time.Minute), now.Add(-time.Minute))
	open := actionIn(t, catalogueOf(t, service, machineID, now, allGrants()), MachineActionConnect)
	if !open.Available || open.Blocker != "" || open.Situation != "" || open.NextStep != "" {
		t.Fatalf("connect=%+v", open)
	}
}

func TestARetiredMachineOffersRestoreInsteadOfRetire(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "retire-direction", enabledCheckin())
	active := catalogueOf(t, service, machineID, now, allGrants())
	if retire := actionIn(t, active, MachineActionRetire); !retire.Available {
		t.Fatalf("retire=%+v", retire)
	}
	absentIn(t, active, MachineActionRestore)

	if err := st.RetireMachine(machineID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	retired := catalogueOf(t, service, machineID, now, allGrants())
	if retired.State != MachineLifecycleStateRetired {
		t.Fatalf("state=%q", retired.State)
	}
	restore := actionIn(t, retired, MachineActionRestore)
	if !restore.Available || restore.PreviewPath == "" {
		t.Fatalf("restore=%+v", restore)
	}
	absentIn(t, retired, MachineActionRetire)
}

func TestRetirementClosesChannelAndDiagnosticsAndSaysWhen(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "retire-effects", enabledCheckin())
	retiredAt := time.Now().UTC()
	if err := st.RetireMachine(machineID, retiredAt); err != nil {
		t.Fatal(err)
	}
	catalogue := catalogueOf(t, service, machineID, now, allGrants())
	channel := actionIn(t, catalogue, MachineActionChannel)
	if channel.Available || channel.Blocker != MachineActionBlockerRetired ||
		channel.NextStep != MachineActionBlockerNextStep(MachineActionBlockerRetired) {
		t.Fatalf("channel=%+v", channel)
	}
	if want := retiredAt.Local().Format("2006-01-02 15:04"); channel.Situation != "這台在 "+want+" 退役。" {
		t.Fatalf("channel situation=%q", channel.Situation)
	}
	diagnostic := actionIn(t, catalogue, MachineActionDiagnosticNoop)
	if diagnostic.Available || diagnostic.Blocker != MachineActionBlockerRetired {
		t.Fatalf("diagnostic=%+v", diagnostic)
	}
}

func TestAnUnfinishedJobHoldsBackBothTheDrillAndRetirement(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "active-job", enabledCheckin())
	startDiagnosticJob(t, service, machineID)
	catalogue := catalogueOf(t, service, machineID, now, allGrants())
	for _, kind := range []MachineActionKind{MachineActionDiagnosticNoop, MachineActionRetire} {
		action := actionIn(t, catalogue, kind)
		if action.Available || action.Blocker != MachineActionBlockerActiveJobs ||
			action.Situation != "這台還有 1 張未結束的工作單。" || action.NextStep == "" {
			t.Fatalf("%s=%+v", kind, action)
		}
	}
	if channel := actionIn(t, catalogue, MachineActionChannel); !channel.Available {
		t.Fatalf("未結束的工作單不該擋住改 channel：%+v", channel)
	}
}

func TestTheDrillRepeatsItsOwnPreviewsVerdictAtEveryTimeout(t *testing.T) {
	disabled := false
	for _, tc := range []struct {
		name    string
		enabled *bool
		want    MachineActionBlocker
	}{
		{"還不知道 agent 有沒有開執行", nil, MachineActionBlockerExecutionUnknown},
		{"agent 關掉了執行", &disabled, MachineActionBlockerExecutionDisabled},
		{"agent 開著執行", enabledCheckin(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, st, machineID, now := machineActionsFixture(t, "drill", tc.enabled)
			action := actionIn(t, catalogueOf(t, service, machineID, now, allGrants()),
				MachineActionDiagnosticNoop)
			if action.Blocker != tc.want || action.Available != (tc.want == "") {
				t.Fatalf("action=%+v want=%q", action, tc.want)
			}
			// 目錄用預設 timeout 問 preview。合法範圍的兩端都必須給同一個答案，
			// 否則操作員填的秒數會讓目錄跟寫入路徑講出不同的話。
			for _, timeout := range []int{
				store.OperatorDiagnosticNoopMinTimeoutSeconds,
				store.OperatorDiagnosticNoopDefaultTimeout,
				store.OperatorDiagnosticNoopMaxTimeoutSeconds,
			} {
				preview, err := st.PreviewOperatorDiagnosticNoop(machineID, timeout)
				if err != nil {
					t.Fatal(err)
				}
				if action.Available != (len(preview.Blockers) == 0) {
					t.Fatalf("timeout=%d preview blockers=%v 目錄說 available=%t",
						timeout, preview.Blockers, action.Available)
				}
			}
		})
	}
}

func TestAMachineThatNeverReportedCanOnlyHaveItsTicketRevoked(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	machineID, _, err := st.CreateEnrollTokenFor("never-reported", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := New(st)
	now := time.Now().UTC().Truncate(time.Second)
	catalogue := catalogueOf(t, service, machineID, now, allGrants())
	drill := actionIn(t, catalogue, MachineActionDiagnosticNoop)
	if drill.Available || drill.Blocker != MachineActionBlockerNeverReported ||
		drill.Situation != "這台從來沒有報到過。" || drill.NextStep == "" {
		t.Fatalf("drill=%+v", drill)
	}
	revoke := actionIn(t, catalogue, MachineActionRevokeEnrollToken)
	if !revoke.Available || revoke.PreviewPath == "" {
		t.Fatalf("revoke=%+v", revoke)
	}
}

func TestARedeemedTicketLeavesNothingToRevoke(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "redeemed", enabledCheckin())
	revoke := actionIn(t, catalogueOf(t, service, machineID, now, allGrants()),
		MachineActionRevokeEnrollToken)
	if revoke.Available || revoke.Blocker != MachineActionBlockerNoPendingToken ||
		revoke.Situation == "" || revoke.NextStep != "" {
		t.Fatalf("revoke=%+v", revoke)
	}
}

func TestTheActionCatalogueSaysTheseExactWords(t *testing.T) {
	wants := []struct {
		kind   MachineActionKind
		label  string
		effect string
	}{
		{MachineActionConnect, "連到這台的 BAT", "取得這台的 bat-server 位址並留下一筆動作紀錄；機器本身不變。"},
		{MachineActionDiagnosticNoop, "開一張診斷工作單", "開一張不改任何設定的工作單，走完整條派工與回報路徑。"},
		{MachineActionRename, "重新命名", "改 Hub 名冊名稱與名稱型 expectations；machine ID、agent 與機器上的 hostname 都不變。"},
		{MachineActionNotes, "編輯名冊備註", "更新 Hub 名冊中的人工備註；機器設定與 agent 不變。"},
		{MachineActionChannel, "指派部署通道", "改變這台會領到哪一批部署；已在跑的工作單不受影響。"},
		{MachineActionRevokeEnrollToken, "撤銷註冊票", "讓還沒兌換的註冊票失效；名冊列與已發出的 agent 憑證都不動。"},
		{MachineActionRetire, "退役", "退出分母、拒絕這台的 agent 驗證；名冊、歷史、通道與憑證都保留。"},
		{MachineActionRestore, "恢復管理", "放回分母、恢復這台的 agent 驗證；退役期間的歷史都還在。"},
	}
	covered := make(map[MachineActionKind]bool, len(wants))
	for _, want := range wants {
		covered[want.kind] = true
		t.Run(string(want.kind), func(t *testing.T) {
			if got := MachineActionLabel(want.kind); got != want.label {
				t.Errorf("動作名稱實際是 %q，期望是 %q", got, want.label)
			}
			if got := MachineActionEffect(want.kind); got != want.effect {
				t.Errorf("動作效果實際是 %q，期望是 %q", got, want.effect)
			}
		})
	}
	for _, kind := range MachineActionKinds() {
		if !covered[kind] {
			t.Errorf("目錄多了一個動作 %q，但沒有人逐字讀過它對操作員說的話", kind)
		}
	}
}

// 這張表是手寫的，新增 blocker 常數時要自己回來加一列。
func TestABlockedActionSaysTheseExactNextSteps(t *testing.T) {
	wants := []struct {
		blocker MachineActionBlocker
		next    string
	}{
		{MachineActionBlockerRetired, "先恢復管理，這台才會回到分母裡。"},
		{MachineActionBlockerNeverReported, "等它第一次報到。"},
		{MachineActionBlockerExecutionUnknown, "等下一次報到帶回 agent 的工作單開關。"},
		{MachineActionBlockerExecutionDisabled, "在那台上開啟 agent 的工作單執行。"},
		{MachineActionBlockerActiveJobs, "等未結束的工作單收尾，或到工作單頁處理掉。"},
		{MachineActionBlockerNoConnectAddress, "等 agent 回報 bat-server 的監聽位址。"},
	}
	for _, want := range wants {
		t.Run(string(want.blocker), func(t *testing.T) {
			if got := MachineActionBlockerNextStep(want.blocker); got != want.next {
				t.Errorf("blocker 下一步實際是 %q，期望是 %q", got, want.next)
			}
		})
	}

	// 沒東西可撤銷不是待辦事項。
	if got := MachineActionBlockerNextStep(MachineActionBlockerNoPendingToken); got != "" {
		t.Errorf("沒有待撤銷註冊票時，下一步實際是 %q，期望是 %q", got, "")
	}
}
