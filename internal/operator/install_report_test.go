package operator

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

type installFleet struct {
	store *store.Store
	now   time.Time
	ids   map[string]string
	rev   int64
}

// assign 直接寫一列 desired_state。
//
// ⚠ 不走 CreateDesiredState，因為那個公開包裝擋掉所有 managed catalog kind
// （openclaw 是其中之一），而正式庫裡每一筆 openclaw 意圖都是從部署或 profile 指派
// 那條路寫進去的。revision 由這個 fixture 自己依序配號，是為了讓測試能釘死 samplehub1
// 那個形狀：machine scope 先被指派，之後 channel scope 用一個更大的號碼蓋過去。
func (f *installFleet) assign(t *testing.T, scopeType, scopeID, kind, id, spec string, at time.Time) int64 {
	t.Helper()
	f.rev++
	desiredID := fmt.Sprintf("desired-%02d", f.rev)
	if _, err := f.store.DB().Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES(?,?,?,?,?,?,?,?,?)`, desiredID, scopeType, scopeID, kind, id, f.rev, spec,
		at.UTC().Format(time.RFC3339), "operator@test"); err != nil {
		t.Fatalf("寫意圖 %s:%s %s: %v", scopeType, scopeID, id, err)
	}
	return f.rev
}

// 正式庫 2026-09-12 量到的那三個檔案。
const (
	installLoginCopy   = "/usr/local/bin/openclaw"
	installReleaseCopy = "/home/example-user/.local/share/clawctl/releases/2026.6.6/openclaw"
	installUserCopy    = "/home/ubuntu/.local/bin/openclaw"
	installSystemCopy  = "/usr/bin/openclaw"
)

func installOpenClawSpec(version string) string {
	return `{"kind":"openclaw","version":"` + version + `","artifact":{"sha256":"` +
		"40784b514e5e0e3a82b4da484ca0e3f0e6d8e9b2c1a4f7d3e0b6c9a2f5d8e1b4" + `"}}`
}

// installFixture 造出正式環境 2026-09-12 真的量到的形狀，加上幾種還沒發生但這一頁
// 必須答得出來的形狀。
//
//	samplehub1 canary  machine 意圖 2026.6.10（舊號碼）＋ canary 意圖 2026.5.26（新號碼）
//	                看到 2026.6.6      → 指派的比看到的舊
//	                版號量在 login copy，在跑的是 releases/ 那一份 → 正在跑的是另一個檔案
//	sampleagent2 stable  machine 意圖 2026.5.20、看到 2026.5.20 → 一樣
//	                在跑的那個檔案已經被 unlink 了           → 正在跑的那個檔案不見了
//	sampleagent3 stable  machine 意圖 2026.6.10、看到 2026.6.6  → 指派的比看到的新
//	                在跑的就是量版號的那一個檔案             → 量的就是跑的
//	sampleagent4  stable  machine 意圖 2026.5.26、一筆觀測都沒有  → 指派了，這台沒回報過
//	sampleagent1 （沒有 channel）一列意圖都沒有、也沒回報過      → 沒有被指派過
//	old-box 已退役，身上有一個沒有別人有的資源              → 整列都不可以出現
func installFixture(t *testing.T) installFleet {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	fleet := installFleet{store: st, now: now, ids: map[string]string{}}

	enroll := func(key, name string) {
		t.Helper()
		id, token, err := st.CreateEnrollTokenFor(name, time.Hour)
		if err != nil {
			t.Fatalf("開票 %s: %v", name, err)
		}
		if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token,
			Hostname: name, UnixUser: "example-user", OS: "linux", Arch: "amd64",
		}, now.Add(-time.Hour)); err != nil {
			t.Fatalf("兌換 %s: %v", name, err)
		}
		fleet.ids[key] = id
	}
	enroll("samplehub1", "samplehub1")
	enroll("sampleagent1", "sampleagent1")
	enroll("sampleagent2", "sampleagent2")
	enroll("sampleagent3", "sampleagent3")
	enroll("sampleagent4", "sampleagent4")
	enroll("retired", "old-box")

	observe := func(key string, tools []model.CLITool) {
		t.Helper()
		if err := st.RecordObservation(fleet.ids[key], model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-2 * time.Minute),
			CLITools: tools,
		}, now.Add(-time.Minute)); err != nil {
			t.Fatalf("記觀測 %s: %v", key, err)
		}
	}
	// ⚠ samplehub1 是正式庫 2026-09-12 真的量到的那一格：版號量在 login copy 上，機隊從
	// 2026-09-06 起改從 releases/<ver>/ 跑。所以這一列同時是「指派的比看到的舊」與
	// 「那個版號講的不是正在跑的那一份」——少了後面那一句，有人會去回滾它。
	observe("samplehub1", []model.CLITool{
		{Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "2026.6.6",
			Path:            installLoginCopy, RealPath: installLoginCopy,
			RunningPID: 4131, RunningScript: installReleaseCopy},
	})
	// ⚠ sampleagent2 那一格是「指派的跟看到的一樣」——這一頁上最讓人放下心的一格——而它在
	// 跑的那個檔案已經從磁碟上不見了（正式庫上 agy 就是這個樣子）。
	observe("sampleagent2", []model.CLITool{
		{Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "2026.5.20",
			Path:            installUserCopy, RealPath: installUserCopy,
			RunningPID: 2210, RunningScript: installUserCopy + ".1787036247195252617.old (deleted)"},
	})
	observe("sampleagent3", []model.CLITool{
		{Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "2026.6.6",
			Path:            installSystemCopy, RunningPID: 3310, RunningExe: installSystemCopy},
	})
	// ⚠ sampleagent4 刻意一筆觀測都沒有：它被指派過，而它那一格必須是「指派了，這台沒
	// 回報過」——跟「指派了，這台上沒有」是兩件事。
	// ⚠ 已退役那台的觀測刻意也是「量的不是跑的那一份」。退役那一關要排在這一問前面，
	// 否則一台已經離開機隊的機器會在報告上留下一筆待辦。
	observe("retired", []model.CLITool{
		{Name: "legacy-tool", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "0.1.0",
			Path:            installLoginCopy, RealPath: installLoginCopy,
			RunningPID: 5150, RunningScript: installReleaseCopy},
	})

	// channel 要在有觀測之後才設得下去（SetMachineChannel 擋沒回報過的機器），
	// 所以 sampleagent4 用直接寫名冊的方式——它就是那台「被指派了卻沒回報過」的。
	for key, channel := range map[string]string{"samplehub1": "canary", "sampleagent2": "stable", "sampleagent3": "stable"} {
		if err := st.SetMachineChannel(fleet.ids[key], channel); err != nil {
			t.Fatalf("設 channel %s: %v", key, err)
		}
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel='stable' WHERE machine_id=?`,
		fleet.ids["sampleagent4"]); err != nil {
		t.Fatalf("設 sampleagent4 channel: %v", err)
	}

	at := now.Add(-24 * time.Hour)
	fleet.assign(t, "machine", fleet.ids["samplehub1"], "openclaw", "openclaw", installOpenClawSpec("2026.6.10"), at)
	fleet.assign(t, "machine", fleet.ids["sampleagent2"], "openclaw", "openclaw", installOpenClawSpec("2026.5.20"), at)
	fleet.assign(t, "machine", fleet.ids["sampleagent3"], "openclaw", "openclaw", installOpenClawSpec("2026.6.10"), at)
	fleet.assign(t, "machine", fleet.ids["sampleagent4"], "openclaw", "openclaw", installOpenClawSpec("2026.5.26"), at)
	// ⚠ canary 這一筆的號碼比 samplehub1 自己那一筆大，所以它才是 samplehub1 最後收到的。
	fleet.assign(t, "channel", "canary", "openclaw", "openclaw", installOpenClawSpec("2026.5.26"), at.Add(time.Hour))
	// 診斷用的空工作單排在最後面，號碼最大。它不是安裝意圖。
	fleet.assign(t, "machine", fleet.ids["samplehub1"], "openclaw", "openclaw", `{"kind":"noop"}`, at.Add(2*time.Hour))
	// 已退役那一台身上有一個沒有別人有的資源。它整列都不可以出現。
	fleet.assign(t, "machine", fleet.ids["retired"], "legacy-tool", "legacy-tool",
		`{"kind":"legacy-tool","version":"0.1.0"}`, at)

	// ⚠ 用真的現在退役：名冊列的 created_at 是 Store 自己的鐘寫的，而它一定晚於這個
	// 測試一開始算的 now，拿 now 去退役會被 store 擋下來。
	if err := st.RetireMachine(fleet.ids["retired"], time.Now().UTC()); err != nil {
		t.Fatalf("退役: %v", err)
	}
	return fleet
}

func installResourceNamed(t *testing.T, report InstallReport, name string) InstallResource {
	t.Helper()
	for _, resource := range report.Resources {
		if resource.Name == name {
			return resource
		}
	}
	t.Fatalf("報告裡沒有 %q：%+v", name, report.Resources)
	return InstallResource{}
}

func installRowFor(t *testing.T, resource InstallResource, displayName string) InstallRow {
	t.Helper()
	for _, row := range resource.Rows {
		if row.DisplayName == displayName {
			return row
		}
	}
	t.Fatalf("%s 這個資源沒有 %q 那一列：%+v", resource.Name, displayName, resource.Rows)
	return InstallRow{}
}

func installReportOf(t *testing.T, fleet installFleet) InstallReport {
	t.Helper()
	report, err := New(fleet.store).InstallReport(fleet.now)
	if err != nil {
		t.Fatalf("每機安裝狀態: %v", err)
	}
	return report
}

// 這一頁的分母必須跟註冊報告是同一個數。兩邊各自算一次的話，畫面上會出現
// 「註冊報告說 5 台」而「安裝狀態說 4 台」，而那兩個數字沒有一個問得出誰是對的。
func TestTheInstallReportCountsExactlyWhatTheEnrollmentReportCallsTheDenominator(t *testing.T) {
	fleet := installFixture(t)
	service := New(fleet.store)
	install := installReportOf(t, fleet)
	enrollment, err := service.EnrollmentReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if install.Machines != enrollment.Denominator {
		t.Errorf("兩份報告的分母不一樣：安裝狀態 %d、註冊報告 %d",
			install.Machines, enrollment.Denominator)
	}
	if install.Machines != 5 {
		t.Errorf("分母應該是 5 台（含沒回報過的 sampleagent1），實際 %d", install.Machines)
	}
}

// samplehub1 的形狀：machine scope 指派 2026.6.10，之後 canary channel 用一個更大的
// revision 指派 2026.5.26。最後被叫去裝的是 2026.5.26 —— 誰贏由 revision 決定，
// 不是「machine scope 蓋過 channel scope」。
func TestTheLatestRevisionWinsAcrossScopesNotTheNarrowerScope(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	row := installRowFor(t, resource, "samplehub1")
	if row.Assigned != "2026.5.26" {
		t.Errorf("samplehub1 最後被叫去裝的不是 channel 那一筆：assigned=%q", row.Assigned)
	}
	if row.Scope != "channel" || row.ScopeID != "canary" {
		t.Errorf("沒有講出這一筆指派從哪裡來：scope=%s/%s", row.Scope, row.ScopeID)
	}
	if row.ScopeLabel != "指派給 canary channel" {
		t.Errorf("指派來源那一句不對：%q", row.ScopeLabel)
	}
}

// ⚠⚠ 正式環境上真的長這樣：samplehub1 被指派 2026.5.26，實際跑 2026.6.6——比指派的
// **新**。這不是「落後」也不是「裝失敗」，這一頁只能說出方向。
func TestAnAssignmentOlderThanWhatIsInstalledIsNeitherBehindNorFailed(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	row := installRowFor(t, resource, "samplehub1")
	if row.State != InstallAssignedOlder {
		t.Fatalf("samplehub1 不是「指派的比看到的舊」：state=%s", row.State)
	}
	if row.Assigned != "2026.5.26" || row.Observed != "2026.6.6" {
		t.Errorf("兩個版號沒有都講出來：assigned=%q observed=%q", row.Assigned, row.Observed)
	}
	for _, banned := range []string{"落後", "失敗", "沒裝", "裝不起來"} {
		if strings.Contains(row.Title+row.Meaning+row.NextStep, banned) {
			t.Errorf("這一格把觀測寫成了判決（%q）：%s／%s／%s",
				banned, row.Title, row.Meaning, row.NextStep)
		}
	}
}

func TestAnAssignmentNewerThanWhatIsInstalledSaysSoWithoutCallingItFailed(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	row := installRowFor(t, resource, "sampleagent3")
	if row.State != InstallAssignedNewer {
		t.Fatalf("sampleagent3 不是「指派的比看到的新」：state=%s", row.State)
	}
	if row.Assigned != "2026.6.10" || row.Observed != "2026.6.6" {
		t.Errorf("兩個版號沒有都講出來：assigned=%q observed=%q", row.Assigned, row.Observed)
	}
	for _, banned := range []string{"失敗", "裝不起來"} {
		if strings.Contains(row.Title+row.Meaning+row.NextStep, banned) {
			t.Errorf("這一格把觀測寫成了判決（%q）：%s／%s／%s",
				banned, row.Title, row.Meaning, row.NextStep)
		}
	}
}

func TestAMachineRunningExactlyWhatItWasAssignedSaysSo(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	row := installRowFor(t, resource, "sampleagent2")
	if row.State != InstallMatches {
		t.Fatalf("sampleagent2 不是「指派的跟看到的一樣」：state=%s", row.State)
	}
	if row.NextStep != "" {
		t.Errorf("對得上的那一格不該有下一步：%q", row.NextStep)
	}
	if row.Scope != "machine" || row.ScopeLabel != "指派給這台" {
		t.Errorf("指派來源那一句不對：%s／%q", row.Scope, row.ScopeLabel)
	}
}

// ⚠⚠ 「指派了但這台從來沒回報過」與「指派了但這台說它上面沒有」是兩件事：
// 前者要去看 agent，後者要去看工作單。併成一個「沒有」，就是把沉默講成答案。
func TestAssignedButSilentIsNotTheSameAsAssignedButAbsent(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	silent := installRowFor(t, resource, "sampleagent4")
	if silent.State != InstallUnreported {
		t.Fatalf("sampleagent4 不是「指派了，這台沒回報過」：state=%s", silent.State)
	}
	if silent.Observed != "" {
		t.Errorf("一台沒回報過的機器身上生出了一個版號：%q", silent.Observed)
	}
	if InstallStateMeaning(InstallUnreported) == InstallStateMeaning(InstallAbsent) {
		t.Error("「沒回報過」跟「這台上沒有」講的是同一句話")
	}
	if InstallStateNextStep(InstallUnreported) == InstallStateNextStep(InstallAbsent) {
		t.Error("「沒回報過」跟「這台上沒有」的下一步是同一句話")
	}
}

// sampleagent1 在分母裡，一列意圖都沒有。那一格必須是一個講得出口的狀態，不是留白。
func TestAMachineThatWasNeverAssignedAnythingIsAStateNotABlank(t *testing.T) {
	fleet := installFixture(t)
	report := installReportOf(t, fleet)
	resource := installResourceNamed(t, report, "openclaw")
	row := installRowFor(t, resource, "sampleagent1")
	if row.State != InstallUnassigned {
		t.Fatalf("sampleagent1 不是「沒有被指派過」：state=%s", row.State)
	}
	if row.Title == "" || row.Meaning == "" || row.NextStep == "" {
		t.Errorf("沒有被指派過那一格沒有把話講完：%q／%q／%q", row.Title, row.Meaning, row.NextStep)
	}
	if row.Assigned != "" || row.Scope != "" {
		t.Errorf("沒有指派卻帶著指派欄位：assigned=%q scope=%q", row.Assigned, row.Scope)
	}
	if report.Assigned != 4 {
		t.Errorf("被指派過的台數應該是 4，實際 %d", report.Assigned)
	}
}

// 診斷用的 noop 排在所有安裝意圖後面、號碼最大。把它當意圖的話，samplehub1 那一格會
// 變成「Hub 叫它裝 noop」——一句沒有人下過的指令。
func TestADiagnosticNoopIsNotAnAssignment(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	row := installRowFor(t, resource, "samplehub1")
	if row.Assigned != "2026.5.26" {
		t.Errorf("診斷 noop 被當成 samplehub1 的安裝意圖：assigned=%q", row.Assigned)
	}
}

// 已退役的機器不在分母裡，它身上那個沒有別人有的資源也不可以整列留在報告上——
// 那一列上每一台活著的機器都會是「沒有被指派過」，而那是一個永遠不會有人去處理的
// 待辦：沒有人少指派了任何東西。
func TestARetiredMachineDoesNotLeaveAnEntireResourceBehind(t *testing.T) {
	fleet := installFixture(t)
	report := installReportOf(t, fleet)
	for _, resource := range report.Resources {
		if resource.Name == "legacy-tool" {
			t.Fatalf("退役機器的資源整列留在報告上：%+v", resource)
		}
		for _, row := range resource.Rows {
			if row.DisplayName == "old-box" {
				t.Fatalf("退役機器留在 %s 那一列上", resource.Name)
			}
		}
	}
	if len(report.Resources) != 1 {
		t.Errorf("資源數應該是 1，實際 %d：%+v", len(report.Resources), report.Resources)
	}
}

// 每一列都要查得回帳本，否則畫面上講的「誰在什麼時候指派的」無從查證。
func TestEveryAssignedRowCarriesTheLedgerRowItCameFrom(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	for _, row := range resource.Rows {
		if !InstallStateAssigned(row.State) {
			continue
		}
		if row.Revision <= 0 {
			t.Errorf("%s 那一列沒有 revision", row.DisplayName)
		}
		if row.AssignedAt == nil {
			t.Errorf("%s 那一列沒有指派時間", row.DisplayName)
		}
		if row.AssignedBy != "operator@test" {
			t.Errorf("%s 那一列的指派人是 %q", row.DisplayName, row.AssignedBy)
		}
	}
}

// ⚠ 存活座標一律只用 Hub 的 received_at。agent 的鐘會歪（sampleagent4 快 80 秒），
// 拿它當座標會讓一台停了的機器看起來剛回報過；兩個鐘都要留著。
func TestBothClocksSurviveOnAnObservedRow(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	row := installRowFor(t, resource, "sampleagent2")
	if row.MeasuredAt == nil || row.ObservedAt == nil {
		t.Fatalf("兩個鐘沒有都留著：measured=%v observed=%v", row.MeasuredAt, row.ObservedAt)
	}
	if !row.MeasuredAt.Before(*row.ObservedAt) {
		t.Errorf("agent 的鐘跟 Hub 的鐘被寫成同一個：measured=%s observed=%s",
			row.MeasuredAt.Format(time.RFC3339), row.ObservedAt.Format(time.RFC3339))
	}
}

// 那一行字不可以把「對不起來」算進「沒有不一樣」裡——sampleagent4 沒回報過，它既不是
// 一樣也不是不一樣。
func TestTheResourceHeadlineDoesNotCountSilenceAsAgreement(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	if resource.AssignedOn != 4 || resource.UnassignedOn != 1 {
		t.Fatalf("分母拆錯：assigned=%d unassigned=%d", resource.AssignedOn, resource.UnassignedOn)
	}
	if resource.MatchingOn != 1 || resource.DifferingOn != 2 {
		t.Fatalf("一樣／不一樣算錯：matching=%d differing=%d", resource.MatchingOn, resource.DifferingOn)
	}
	for _, want := range []string{"4/5 台被指派過", "2 台指派的跟看到的不一樣",
		"有 1 台對不起來", "另有 1 台沒有被指派過"} {
		if !strings.Contains(resource.Headline, want) {
			t.Errorf("那一行字少講了 %q：%s", want, resource.Headline)
		}
	}
}

// 整份報告那一行字算的是「幾個資源上對不起來」，而它必須跟每一個資源自己算的一致。
func TestTheReportHeadlineCountsTheResourcesThatDisagree(t *testing.T) {
	fleet := installFixture(t)
	report := installReportOf(t, fleet)
	if report.Differing != 1 {
		t.Errorf("有一個資源上指派的跟看到的不一樣，卻算成 %d", report.Differing)
	}
	for _, want := range []string{"分母 5 台、1 個資源", "4 台被指派過",
		"1 個資源上指派的跟看到的不一樣", "另有 1 台一個資源都沒有被指派過"} {
		if !strings.Contains(report.Headline, want) {
			t.Errorf("那一行字少講了 %q：%s", want, report.Headline)
		}
	}
	if report.NextStep == "" {
		t.Error("有事可做的時候沒有給下一步")
	}
}

// 全部都對得上的時候，那一行字要講得出「都對得上」，而且不留下一步——一頁看起來
// 像沒算完的報告，跟一頁真的沒問題的報告，在畫面上長得一樣。
func TestAFleetWhereEverythingLinesUpSaysSoAndLeavesNoNextStep(t *testing.T) {
	fleet := installEdgeFixture(t)
	at := fleet.now.Add(-24 * time.Hour)
	for _, name := range []string{"absent-box", "muted-box", "other-box", "unversioned-box", "weird-box"} {
		if err := fleet.store.RecordObservation(fleet.ids[name], model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: fleet.now.Add(-time.Minute),
			CLITools: []model.CLITool{{Name: "openclaw", Present: true, OnPath: true,
				PresentEvidence: "path", VersionReported: "2026.7.1"}},
		}, fleet.now.Add(-30*time.Second)); err != nil {
			t.Fatalf("記觀測 %s: %v", name, err)
		}
		fleet.assign(t, "machine", fleet.ids[name], "openclaw", "openclaw",
			installOpenClawSpec("2026.7.1"), at.Add(time.Hour))
	}
	report := installReportOf(t, fleet)
	resource := installResourceNamed(t, report, "openclaw")
	if resource.MatchingOn != 5 || resource.DifferingOn != 0 || resource.UnassignedOn != 0 {
		t.Fatalf("全部對得上卻沒算對：matching=%d differing=%d unassigned=%d",
			resource.MatchingOn, resource.DifferingOn, resource.UnassignedOn)
	}
	if !strings.Contains(resource.Headline, "指派的跟看到的都一樣") {
		t.Errorf("全部對得上的時候那一行字沒有講出來：%s", resource.Headline)
	}
	if resource.NextStep != "" || report.NextStep != "" {
		t.Errorf("沒有事可做卻給了下一步：%q／%q", resource.NextStep, report.NextStep)
	}
	if report.Differing != 0 {
		t.Errorf("全部對得上卻算出 %d 個資源不一樣", report.Differing)
	}
}

// 這份報告講得出口的極限要印在畫面上，不是只寫在原始碼裡。
func TestTheInstallReportCarriesItsOwnLimit(t *testing.T) {
	fleet := installFixture(t)
	report := installReportOf(t, fleet)
	if report.Caveat != InstallReportCaveat {
		t.Errorf("報告沒有帶著它自己的極限：%q", report.Caveat)
	}
	if !strings.Contains(report.Caveat, "指派以外的路徑") {
		t.Errorf("那句極限沒有講到重點：%q", report.Caveat)
	}
}

func TestTheInstallReportRefusesAZeroClock(t *testing.T) {
	fleet := installFixture(t)
	if _, err := New(fleet.store).InstallReport(time.Time{}); !errors.Is(err, ErrInvalidInstallReport) {
		t.Errorf("零值時間沒有被擋下來：%v", err)
	}
}

// 十種狀態每一種都要有題目與說明，而且每一種都是正面陳述。
func TestEveryInstallStateSaysWhatItIsAndWhatToDo(t *testing.T) {
	states := InstallStates()
	if len(states) != 10 {
		t.Fatalf("狀態數變了：%d", len(states))
	}
	titles := map[string]bool{}
	for _, stateValue := range states {
		title, meaning := InstallStateTitle(stateValue), InstallStateMeaning(stateValue)
		if title == "" || meaning == "" {
			t.Errorf("%s 沒有把話講完：%q／%q", stateValue, title, meaning)
		}
		if titles[title] {
			t.Errorf("%s 的題目跟別人一樣：%q", stateValue, title)
		}
		titles[title] = true
		if stateValue != InstallMatches && InstallStateNextStep(stateValue) == "" {
			t.Errorf("%s 沒有下一步", stateValue)
		}
	}
	if InstallStateNextStep(InstallMatches) != "" {
		t.Error("對得上的那一種不該有下一步")
	}
}

// 同一份機隊每次都要算出同一份報告。
func TestTheSameFleetProducesTheSameInstallReportEveryTime(t *testing.T) {
	fleet := installFixture(t)
	first := installReportOf(t, fleet)
	for round := 0; round < 60; round++ {
		again := installReportOf(t, fleet)
		if len(again.Resources) != len(first.Resources) {
			t.Fatalf("第 %d 輪資源數變了：%d → %d", round, len(first.Resources), len(again.Resources))
		}
		for i := range first.Resources {
			if again.Resources[i].Name != first.Resources[i].Name {
				t.Fatalf("第 %d 輪資源順序變了：%s → %s", round,
					first.Resources[i].Name, again.Resources[i].Name)
			}
			for j := range first.Resources[i].Rows {
				want, got := first.Resources[i].Rows[j], again.Resources[i].Rows[j]
				if want.MachineID != got.MachineID || want.State != got.State {
					t.Fatalf("第 %d 輪 %s 第 %d 列變了：%s/%s → %s/%s", round,
						first.Resources[i].Name, j, want.DisplayName, want.State,
						got.DisplayName, got.State)
				}
			}
		}
	}
}

// 每一個資源上，每一台機器都要有一列。少一列的樣子是「那一台沒問題」，而事實是
// 「那一台沒有被算到」。
func TestEveryMachineGetsARowOnEveryResource(t *testing.T) {
	fleet := installFixture(t)
	report := installReportOf(t, fleet)
	for _, resource := range report.Resources {
		if len(resource.Rows) != report.Machines {
			t.Errorf("%s 只有 %d 列，分母是 %d 台", resource.Name, len(resource.Rows), report.Machines)
		}
		seen := map[string]bool{}
		for _, row := range resource.Rows {
			if seen[row.MachineID] {
				t.Errorf("%s 上 %s 出現兩次", resource.Name, row.DisplayName)
			}
			seen[row.MachineID] = true
		}
	}
}

// installEdgeFixture 是「對不起來」的那五種：每一台都被指派過 openclaw，差別只在
// Hub 能不能把它跟觀測對起來。這五種分開講，是因為它們的下一步各不相同——併成一個
// 「對不起來」，就等於把五個不同的問題寫成同一張待辦。
func installEdgeFixture(t *testing.T) installFleet {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	fleet := installFleet{store: st, now: now, ids: map[string]string{}}

	for _, name := range []string{"absent-box", "muted-box", "other-box", "unversioned-box", "weird-box"} {
		id, token, err := st.CreateEnrollTokenFor(name, time.Hour)
		if err != nil {
			t.Fatalf("開票 %s: %v", name, err)
		}
		if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token,
			Hostname: name, UnixUser: "example-user", OS: "linux", Arch: "amd64",
		}, now.Add(-time.Hour)); err != nil {
			t.Fatalf("兌換 %s: %v", name, err)
		}
		fleet.ids[name] = id
	}
	observe := func(key string, tools []model.CLITool) {
		t.Helper()
		if err := st.RecordObservation(fleet.ids[key], model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-2 * time.Minute),
			CLITools: tools,
		}, now.Add(-time.Minute)); err != nil {
			t.Fatalf("記觀測 %s: %v", key, err)
		}
	}
	observe("absent-box", []model.CLITool{{Name: "openclaw", Present: false}})
	observe("muted-box", []model.CLITool{
		{Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path"}})
	// ⚠ other-box 有在回報，只是回報裡沒有 openclaw。它跟一台完全沒回報過的機器
	// 下一步不一樣：這一台要去看它回報了什麼，那一台要去看 agent 還活著沒有。
	observe("other-box", []model.CLITool{
		{Name: "claude", Present: true, OnPath: true, PresentEvidence: "path", VersionReported: "2.1.195"}})
	observe("unversioned-box", []model.CLITool{
		{Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "2026.6.6"}})
	observe("weird-box", []model.CLITool{
		{Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "2026.6.6-rc1"}})

	at := now.Add(-24 * time.Hour)
	for _, name := range []string{"absent-box", "muted-box", "other-box", "weird-box"} {
		fleet.assign(t, "machine", fleet.ids[name], "openclaw", "openclaw",
			installOpenClawSpec("2026.5.26"), at)
	}
	// ⚠ 指派本身沒講版號。這跟「機器答不出版號」是兩件事：這一台要去看那筆指派，
	// 那一台要去看機器。
	fleet.assign(t, "machine", fleet.ids["unversioned-box"], "openclaw", "openclaw",
		`{"kind":"openclaw"}`, at)
	return fleet
}

func TestTheFiveWaysAnAssignmentCanFailToLineUpStayFiveDifferentAnswers(t *testing.T) {
	fleet := installEdgeFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	for name, want := range map[string]InstallState{
		"absent-box":      InstallAbsent,
		"muted-box":       InstallObservedNoVersion,
		"other-box":       InstallUnobserved,
		"unversioned-box": InstallAssignedNoVersion,
		"weird-box":       InstallIncomparable,
	} {
		row := installRowFor(t, resource, name)
		if row.State != want {
			t.Errorf("%s 應該是 %s，實際 %s（assigned=%q observed=%q）",
				name, want, row.State, row.Assigned, row.Observed)
		}
	}
	if resource.MatchingOn != 0 || resource.DifferingOn != 0 {
		t.Errorf("對不起來的五種被算成一樣或不一樣：matching=%d differing=%d",
			resource.MatchingOn, resource.DifferingOn)
	}
	if resource.AssignedOn != 5 {
		t.Errorf("五台都被指派過，卻只算到 %d 台", resource.AssignedOn)
	}
	if !strings.Contains(resource.Headline, "有 5 台對不起來") {
		t.Errorf("那一行字沒有把五台對不起來講出來：%s", resource.Headline)
	}
}

// ⚠⚠ 2026.6.6-rc1 跟 2026.5.26 不准比出先後。猜一個順序就會叫人去動一台其實沒問題
// 的機器——那正是上游用來表達 pre-release 的寫法，而這個 Hub 沒有規則可以判。
func TestAPreReleaseVersionIsNeverCalledOlderOrNewer(t *testing.T) {
	fleet := installEdgeFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	row := installRowFor(t, resource, "weird-box")
	if row.State != InstallIncomparable {
		t.Fatalf("比不出來的版號被排出了先後：state=%s", row.State)
	}
	if row.Assigned != "2026.5.26" || row.Observed != "2026.6.6-rc1" {
		t.Errorf("兩個版號沒有都原樣講出來：assigned=%q observed=%q", row.Assigned, row.Observed)
	}
}

// 一台有在回報、只是回報裡沒有這個東西的機器，跟一台從來沒回報過的機器，
// 下一步不一樣。
func TestReportingWithoutThisResourceIsNotTheSameAsNeverReporting(t *testing.T) {
	if InstallStateNextStep(InstallUnobserved) == InstallStateNextStep(InstallUnreported) {
		t.Error("「看不到這個東西」跟「沒回報過」的下一步是同一句話")
	}
	fleet := installEdgeFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	row := installRowFor(t, resource, "other-box")
	if row.State != InstallUnobserved {
		t.Fatalf("一台有在回報的機器被講成沒回報過：state=%s", row.State)
	}
	if row.ObservedAt != nil {
		t.Errorf("對不到觀測的那一格帶著一個觀測時刻：%v", row.ObservedAt)
	}
}

// kind 跟 id 不一樣的資源，名字要把兩邊都講出來——同一個 executor kind 上可以有好
// 幾個套件，只講 kind 的話它們在畫面上會長成同一列。
func TestAResourceWhoseKindAndIDDifferKeepsBothInItsName(t *testing.T) {
	fleet := installEdgeFixture(t)
	at := fleet.now.Add(-24 * time.Hour)
	fleet.assign(t, "machine", fleet.ids["muted-box"], "node-runtime", "node-24",
		`{"kind":"node-runtime","version":"24.8.0"}`, at)
	fleet.assign(t, "machine", fleet.ids["muted-box"], "node-runtime", "node-22",
		`{"kind":"node-runtime","version":"22.14.0"}`, at)

	report := installReportOf(t, fleet)
	names := make([]string, 0, len(report.Resources))
	for _, resource := range report.Resources {
		names = append(names, resource.Name)
	}
	for _, want := range []string{"node-runtime/node-22", "node-runtime/node-24", "openclaw"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("報告裡沒有 %q：%v", want, names)
		}
	}
	if len(report.Resources) != 3 {
		t.Errorf("同一個 kind 的兩個套件被併成一列：%v", names)
	}
}

// 從檔案讀來的版號照樣拿去比，但要講清楚它不是程式自己答出來的。
func TestAVersionReadFromDiskSaysWhereItCameFrom(t *testing.T) {
	fleet := installEdgeFixture(t)
	if err := fleet.store.RecordObservation(fleet.ids["muted-box"], model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: fleet.now.Add(-time.Minute),
		CLITools: []model.CLITool{{Name: "openclaw", Present: true, OnPath: true,
			PresentEvidence: "path", VersionPackageJSON: "2026.5.26"}},
	}, fleet.now.Add(-30*time.Second)); err != nil {
		t.Fatalf("記觀測: %v", err)
	}
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	row := installRowFor(t, resource, "muted-box")
	if row.Observed != "2026.5.26" {
		t.Fatalf("檔案上的版號沒有被拿來比：observed=%q state=%s", row.Observed, row.State)
	}
	if !row.FromDisk {
		t.Error("一個從檔案讀來的版號被講成它本人答出來的")
	}
	if row.State != InstallMatches {
		t.Errorf("檔案上的版號跟指派的一樣，卻不是「一樣」：state=%s", row.State)
	}
}

// 機器照名字排，資源照名字排。順序不是裝飾：一份每次換位置的表，人沒有辦法用眼睛
// 比較兩次之間差了什麼。
func TestMachinesAndResourcesComeBackInNameOrder(t *testing.T) {
	fleet := installFixture(t)
	resource := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	want := []string{"sampleagent1", "sampleagent2", "sampleagent3", "sampleagent4", "samplehub1"}
	if len(resource.Rows) != len(want) {
		t.Fatalf("列數不對：%d", len(resource.Rows))
	}
	for i, name := range want {
		if resource.Rows[i].DisplayName != name {
			t.Fatalf("第 %d 列應該是 %s，實際 %s", i, name, resource.Rows[i].DisplayName)
		}
	}

	// ⚠ 最後才註冊、名字卻該排第一的那一台。名冊順序剛好等於字母順序的 fixture
	// 釘不住這件事——不排也會過。
	lateID, token, err := fleet.store.CreateEnrollTokenFor("aaa-box", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fleet.store.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "aaa-box", UnixUser: "example-user", OS: "linux", Arch: "amd64",
	}, fleet.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	fleet.ids["aaa-box"] = lateID
	// ⚠ 讓它報到過。名冊投影是照「最該看的排最上面」排的，所以一台健康的機器在
	// 那一份裡排最後——名字排序要是沒有自己做一次，它在這一頁上也會排最後。
	if err := fleet.store.RecordCheckin(lateID, model.Checkin{
		SentAt: fleet.now.Add(-time.Minute), AgentStartedAt: fleet.now.Add(-time.Hour),
	}, fleet.now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	late := installResourceNamed(t, installReportOf(t, fleet), "openclaw")
	if late.Rows[0].DisplayName != "aaa-box" {
		t.Fatalf("最後註冊、名字排第一的那一台沒有排到第一列：%s", late.Rows[0].DisplayName)
	}

	edge := installEdgeFixture(t)
	at := edge.now.Add(-24 * time.Hour)
	edge.assign(t, "machine", edge.ids["muted-box"], "node-runtime", "node-24",
		`{"kind":"node-runtime","version":"24.8.0"}`, at)
	edge.assign(t, "machine", edge.ids["muted-box"], "node-runtime", "node-22",
		`{"kind":"node-runtime","version":"22.14.0"}`, at)
	report := installReportOf(t, edge)
	wantResources := []string{"node-runtime/node-22", "node-runtime/node-24", "openclaw"}
	for i, name := range wantResources {
		if report.Resources[i].Name != name {
			t.Fatalf("第 %d 個資源應該是 %s，實際 %s", i, name, report.Resources[i].Name)
		}
	}
}

// ⚠ 超過就拒絕，不截斷。一份少算的報告看起來就是全部——而這一頁講的是「哪幾台沒有
// 被指派過」，少算的那幾台會剛好長得像「沒有問題」。
func TestTooManyResourcesIsRefusedNotTruncated(t *testing.T) {
	fleet := installEdgeFixture(t)
	at := fleet.now.Add(-24 * time.Hour)
	for i := 0; i <= MaxInstallReportResources; i++ {
		fleet.assign(t, "machine", fleet.ids["muted-box"], "node-runtime",
			fmt.Sprintf("node-%03d", i), `{"kind":"node-runtime","version":"24.8.0"}`, at)
	}
	_, err := New(fleet.store).InstallReport(fleet.now)
	if !errors.Is(err, ErrInvalidInstallReport) {
		t.Fatalf("超過上限沒有被擋下來：%v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(MaxInstallReportResources)) {
		t.Errorf("錯誤訊息沒有講出上限是多少：%v", err)
	}
}

func TestTooManyMachinesIsRefusedNotTruncated(t *testing.T) {
	fleet := installFixture(t)
	created := fleet.now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	for i := 0; i <= MaxInstallReportMachines; i++ {
		if _, err := fleet.store.DB().Exec(`INSERT INTO machine_registry
 (machine_id,display_name,expected,created_at) VALUES(?,?,1,?)`,
			fmt.Sprintf("crowd-%05d", i), fmt.Sprintf("crowd-%05d", i), created); err != nil {
			t.Fatalf("塞名冊: %v", err)
		}
	}
	_, err := New(fleet.store).InstallReport(fleet.now)
	if !errors.Is(err, ErrInvalidInstallReport) {
		t.Fatalf("超過上限沒有被擋下來：%v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(MaxInstallReportMachines)) {
		t.Errorf("錯誤訊息沒有講出上限是多少：%v", err)
	}
}

// 匯出的是畫面上那些列，一列不多一列不少。壓成統計的話，試算表就答不出
// 「哪一台沒有被指派過」——而那正是有人把它開起來要問的問題。
func TestTheInstallCSVExportsOneRowPerMachinePerResource(t *testing.T) {
	fleet := installFixture(t)
	report := installReportOf(t, fleet)
	document := InstallReportCSV(report)

	cells := 0
	for _, resource := range report.Resources {
		cells += len(resource.Rows)
	}
	if len(document.Rows) != cells {
		t.Fatalf("CSV %d 列，畫面上有 %d 格", len(document.Rows), cells)
	}
	for i, row := range document.Rows {
		if len(row) != len(document.Columns) {
			t.Fatalf("第 %d 列有 %d 格，表頭有 %d 欄", i, len(row), len(document.Columns))
		}
	}
	if _, err := ReportCSV(document); err != nil {
		t.Fatalf("這份匯出寫不出來：%v", err)
	}
}

// ⚠ 指派的版號與看到的版號各佔一欄。合併成一個「差異」欄，讀的人就得自己猜方向，
// 而方向正是這一頁唯一講得出口的東西。
func TestTheInstallCSVKeepsBothVersionsInTheirOwnColumns(t *testing.T) {
	fleet := installFixture(t)
	document := InstallReportCSV(installReportOf(t, fleet))
	index := map[string]int{}
	for i, column := range document.Columns {
		index[column.Key] = i
	}
	for _, key := range []string{"assigned", "observed", "scope", "revision", "next_step"} {
		if _, ok := index[key]; !ok {
			t.Fatalf("CSV 少了 %q 這一欄：%+v", key, document.Columns)
		}
	}
	var samplehub1 []string
	for _, row := range document.Rows {
		if row[index["machine"]] == "samplehub1" {
			samplehub1 = row
		}
	}
	if samplehub1 == nil {
		t.Fatalf("CSV 裡沒有 samplehub1 那一列")
	}
	if samplehub1[index["assigned"]] != "2026.5.26" || samplehub1[index["observed"]] != "2026.6.6" {
		t.Errorf("兩個版號沒有各佔一欄：assigned=%q observed=%q",
			samplehub1[index["assigned"]], samplehub1[index["observed"]])
	}
	if samplehub1[index["scope"]] != "指派給 canary channel" {
		t.Errorf("指派來源那一欄不對：%q", samplehub1[index["scope"]])
	}
}

// ⚠ 沒有指派的那一列 revision 要是空格，不是 0。0 在試算表裡會被排序、被加總、
// 被當成一個真的號碼。
func TestTheInstallCSVLeavesRevisionBlankWhereThereIsNoAssignment(t *testing.T) {
	fleet := installFixture(t)
	document := InstallReportCSV(installReportOf(t, fleet))
	index := map[string]int{}
	for i, column := range document.Columns {
		index[column.Key] = i
	}
	for _, row := range document.Rows {
		if row[index["machine"]] != "sampleagent1" {
			continue
		}
		for _, key := range []string{"revision", "assigned", "assigned_at", "assigned_by", "scope"} {
			if row[index[key]] != "" {
				t.Errorf("沒有被指派過的那一列 %s 欄是 %q，不是空的", key, row[index[key]])
			}
		}
		if row[index["state"]] != InstallStateTitle(InstallUnassigned) {
			t.Errorf("沒有被指派過的那一列狀態是 %q", row[index["state"]])
		}
		return
	}
	t.Fatal("CSV 裡沒有 sampleagent1 那一列")
}

// installRuntimeFixture 是專門給「版號講的是哪一份」那一軸的機隊：四台都被指派過、
// 四台看到的版號都跟指派的一樣，而其中兩台的版號量在一個沒有人在跑的檔案上。
//
// ⚠ 它跟 installFixture 分開，因為這裡要的是「同一種狀態有兩格」與「整個資源都對得
// 上卻整個資源都量錯了檔案」——把這兩件事塞進主 fixture 會動到它的分母。
func installRuntimeFixture(t *testing.T) installFleet {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	fleet := installFleet{store: st, now: now, ids: map[string]string{}}

	tools := map[string]model.CLITool{
		// 兩台都是「正在跑的是另一個檔案」，而且跑的是兩份不同的 release。
		"run-other-a": {Path: installLoginCopy, RealPath: installLoginCopy,
			RunningPID: 11, RunningScript: "/opt/openclaw/2026.6.6/openclaw"},
		"run-other-b": {Path: installLoginCopy, RealPath: installLoginCopy,
			RunningPID: 12, RunningScript: "/opt/openclaw/2026.6.7/openclaw"},
		"run-idle":  {Path: installSystemCopy, RunningReason: "no matching process"},
		"run-blind": {Path: installSystemCopy, RunningPID: 13, RunningExe: "/usr/bin/node"},
	}
	names := []string{"run-blind", "run-idle", "run-other-a", "run-other-b"}
	for _, name := range names {
		id, token, err := st.CreateEnrollTokenFor(name, time.Hour)
		if err != nil {
			t.Fatalf("開票 %s: %v", name, err)
		}
		if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token,
			Hostname: name, UnixUser: "example-user", OS: "linux", Arch: "amd64",
		}, now.Add(-time.Hour)); err != nil {
			t.Fatalf("兌換 %s: %v", name, err)
		}
		fleet.ids[name] = id

		tool := tools[name]
		tool.Name, tool.Present, tool.OnPath = "openclaw", true, true
		tool.PresentEvidence, tool.VersionReported = "path", "2026.6.6"
		if err := st.RecordObservation(id, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-2 * time.Minute),
			CLITools: []model.CLITool{tool},
		}, now.Add(-time.Minute)); err != nil {
			t.Fatalf("記觀測 %s: %v", name, err)
		}
		fleet.assign(t, "machine", id, "openclaw", "openclaw",
			installOpenClawSpec("2026.6.6"), now.Add(-24*time.Hour))
	}
	return fleet
}

// 沒有被看到過的那幾列不准帶這一軸。一格 runtime 出現在一台從來沒回報過的機器上，
// 等於 Hub 拿一個它沒有的證據去講「那個版號量錯了檔案」。
func TestOnlyTheRowsTheMachineReportedCarryTheRuntimeAxis(t *testing.T) {
	resource := installResourceNamed(t, installReportOf(t, installFixture(t)), "openclaw")
	for _, row := range resource.Rows {
		want := InstallStateReportedPresent(row.State)
		if got := row.Runtime != nil; got != want {
			t.Errorf("%s（%s）帶了 runtime=%v，這個狀態應該是 %v",
				row.DisplayName, row.State, got, want)
		}
	}
	for _, stateValue := range InstallStates() {
		reported := InstallStateReportedPresent(stateValue)
		switch stateValue {
		case InstallMatches, InstallAssignedNewer, InstallAssignedOlder, InstallIncomparable,
			InstallAssignedNoVersion, InstallObservedNoVersion:
			if !reported {
				t.Errorf("%s 是這台回報說有的狀態，卻沒有這一軸", stateValue)
			}
		default:
			if reported {
				t.Errorf("%s 沒有一個量過版號的檔案，卻帶了這一軸", stateValue)
			}
		}
	}
}

// 「指派的比看到的舊」那一列必須同時講出它量的是哪一個檔案、在跑的是哪一個。少了這
// 兩個檔案名，下一步那句「把這台部署回指派的那一版」會叫人去回滾一台其實沒事的機器。
func TestTheRowThatSaysTheAssignmentIsOlderAlsoSaysWhichFileWasMeasured(t *testing.T) {
	resource := installResourceNamed(t, installReportOf(t, installFixture(t)), "openclaw")
	row := installRowFor(t, resource, "samplehub1")
	if row.State != InstallAssignedOlder {
		t.Fatalf("samplehub1 那一列是 %s，不是指派的比看到的舊", row.State)
	}
	if row.Runtime == nil {
		t.Fatal("samplehub1 那一列沒有講它量的版號是哪一份")
	}
	if row.Runtime.State != ToolRuntimeOtherFile {
		t.Fatalf("在跑的是 releases 那一份，卻算成 %s", row.Runtime.State)
	}
	if row.Runtime.MeasuredFile != installLoginCopy || row.Runtime.RunningFile != installReleaseCopy {
		t.Errorf("那兩個檔案沒有都講出來：量的是 %q、跑的是 %q",
			row.Runtime.MeasuredFile, row.Runtime.RunningFile)
	}
	if SameToolFile(row.Runtime.MeasuredFile, row.Runtime.RunningFile) {
		t.Error("說了「正在跑的是另一個檔案」，而那兩個路徑是同一份")
	}
}

// ⚠⚠ 「指派的跟看到的一樣」是這一頁上最沒有人會回頭看的一格，而它照樣可以量在一個
// 已經從磁碟上不見的檔案上。量錯檔案那一個數字算在 matching/differing 之外，不是它們
// 的一部分——寫成 switch 的一個 case 的話，這一台永遠不會被算進來。
func TestTheRowThatLinesUpCanStillBeMeasuredOnAFileNobodyRuns(t *testing.T) {
	resource := installResourceNamed(t, installReportOf(t, installFixture(t)), "openclaw")
	row := installRowFor(t, resource, "sampleagent2")
	if row.State != InstallMatches {
		t.Fatalf("sampleagent2 那一列是 %s，不是指派的跟看到的一樣", row.State)
	}
	if row.Runtime == nil || row.Runtime.State != ToolRuntimeGoneFile {
		t.Fatalf("在跑的那個檔案已經不見了，卻算成 %+v", row.Runtime)
	}
	if resource.MatchingOn != 1 {
		t.Errorf("量錯檔案把「一樣」那一格吃掉了：matching=%d", resource.MatchingOn)
	}
	if resource.MisattributedOn != 2 {
		t.Errorf("samplehub1 與 sampleagent2 兩格量的都不是正在跑的那一份，卻算成 %d",
			resource.MisattributedOn)
	}
	if resource.MisattributedOn > resource.AssignedOn {
		t.Errorf("量錯檔案的台數比被指派過的台數還多：%d > %d",
			resource.MisattributedOn, resource.AssignedOn)
	}
}

// 在跑的就是量版號的那一個檔案，那一格不算量錯。把「量的就是跑的」算進去，機隊上每
// 一格都會變成待辦事項，然後沒有人再看這個數字。
func TestTheRowWhoseRunningFileIsTheMeasuredOneIsNotCounted(t *testing.T) {
	resource := installResourceNamed(t, installReportOf(t, installFixture(t)), "openclaw")
	row := installRowFor(t, resource, "sampleagent3")
	if row.Runtime == nil || row.Runtime.State != ToolRuntimeSameFile {
		t.Fatalf("sampleagent3 在跑的就是量的那一份，卻算成 %+v", row.Runtime)
	}
	if ToolRuntimeMisattributed(row.Runtime.State) {
		t.Error("「量的就是跑的」被算成量錯了檔案")
	}
}

// 一個資源那一行字不准只講「都一樣」就收尾。整個資源都對得上而整個資源都量在沒在跑
// 的檔案上，是這一頁最會被讀錯的一種樣子。
func TestTheResourceHeadlineSaysWhenAVersionIsNotTheOneRunning(t *testing.T) {
	resource := installResourceNamed(t, installReportOf(t, installRuntimeFixture(t)), "openclaw")
	if resource.MatchingOn != 4 || resource.DifferingOn != 0 {
		t.Fatalf("四台都對得上卻沒算對：matching=%d differing=%d",
			resource.MatchingOn, resource.DifferingOn)
	}
	if resource.MisattributedOn != 2 {
		t.Fatalf("兩台量的不是正在跑的那一份，卻算成 %d", resource.MisattributedOn)
	}
	for _, want := range []string{"指派的跟看到的都一樣", "其中 2 台看到的版號量的是沒在跑的那一份"} {
		if !strings.Contains(resource.Headline, want) {
			t.Errorf("那一行字少講了 %q：%s", want, resource.Headline)
		}
	}
	if resource.NextStep != "先看那幾台看到的版號量的是沒在跑的那一份，再談哪一邊是對的。" {
		t.Errorf("下一步沒有先問在比哪一個檔案：%q", resource.NextStep)
	}
}

// 下一步先問「在比哪一個檔案」，才問「哪一個資源對不起來」。反過來排的話，第一件事
// 會是去回滾一台其實沒事的機器。
func TestTheInstallNextStepAsksWhichFileBeforeItAsksWhichResource(t *testing.T) {
	report := installReportOf(t, installFixture(t))
	if report.Differing == 0 || report.Misattributed == 0 {
		t.Fatalf("這個機隊要同時有兩件事才問得出順序：differing=%d misattributed=%d",
			report.Differing, report.Misattributed)
	}
	if report.NextStep != "先看那幾格看到的版號量的是沒在跑的那一份，再談哪一個資源對不起來。" {
		t.Errorf("下一步沒有先問在比哪一個檔案：%q", report.NextStep)
	}
	if !strings.Contains(report.Headline, "另有 2 格看到的版號量的是沒在跑的那一份") {
		t.Errorf("整份報告那一行字少了量錯檔案那一句：%s", report.Headline)
	}
	// ⚠ 兩句必須在同一行字裡並排。少了後面那一句，前面那一句會被讀成有人動過機器。
	version := strings.Index(report.Headline, "指派的跟看到的不一樣")
	running := strings.Index(report.Headline, "量的是沒在跑的那一份")
	if version < 0 || running < version {
		t.Errorf("量錯檔案那一句沒有緊跟在版號那一句後面：%s", report.Headline)
	}
}

// ⚠ 這一軸的摘要要逐格數過，不是只檢查它們加得起來。同一種狀態在機隊上通常有好幾格
// ——一份只數到 1 的摘要照樣加得起來（別的狀態會多算），而畫面上會少掉一台要人動手的。
func TestEveryInstallRuntimeCountAddsUpToEveryRowThatHasOne(t *testing.T) {
	report := installReportOf(t, installRuntimeFixture(t))
	counted := map[ToolRuntime]int{}
	rows, misattributed := 0, 0
	for _, resource := range report.Resources {
		for _, row := range resource.Rows {
			if row.Runtime == nil {
				continue
			}
			rows++
			counted[row.Runtime.State]++
			if ToolRuntimeMisattributed(row.Runtime.State) {
				misattributed++
			}
		}
	}
	if len(report.RuntimeStates) != len(ToolRuntimes()) {
		t.Fatalf("七種狀態要一律列出來，只列了 %d 種", len(report.RuntimeStates))
	}
	total := 0
	for index, entry := range report.RuntimeStates {
		if entry.State != ToolRuntimes()[index] {
			t.Errorf("第 %d 個是 %s，順序不對", index, entry.State)
		}
		if entry.Count != counted[entry.State] {
			t.Errorf("%s 數了 %d 格，逐格數是 %d", entry.State, entry.Count, counted[entry.State])
		}
		total += entry.Count
	}
	if total != rows {
		t.Errorf("七種狀態加起來 %d 格，有 runtime 的列是 %d", total, rows)
	}
	if report.Misattributed != misattributed || misattributed != 2 {
		t.Errorf("量錯檔案的格數是 %d，逐格數是 %d（該是 2）", report.Misattributed, misattributed)
	}
	if got := counted[ToolRuntimeOtherFile]; got != 2 {
		t.Errorf("兩台在跑的都是另一個檔案，卻數成 %d", got)
	}
}

// ⚠⚠ 加一個欄位就要把版號 +1。對面的 strict decoder 對著一個它不認得的欄位會整份拒
// 收，所以悄悄加欄位會讓舊的用戶端在解碼那一刻才壞——而那時候它已經印了半份畫面。
func TestAFieldAddedToTheInstallReportComesWithANewSchemaVersion(t *testing.T) {
	if InstallReportSchemaVersion != 2 {
		t.Fatalf("版號是 %d；欄位清單跟它必須一起動", InstallReportSchemaVersion)
	}
	for _, shape := range []struct {
		name   string
		fields []string
		value  any
	}{
		{"InstallReport", []string{
			"schema_version", "evaluated_at", "machines", "assigned", "differing",
			"misattributed", "resources", "states", "runtime_states",
			"headline", "caveat", "next_step",
		}, InstallReport{}},
		{"InstallResource", []string{
			"name", "resource_kind", "resource_id", "assigned_on", "unassigned_on",
			"matching_on", "differing_on", "misattributed_on", "rows", "headline", "next_step",
		}, InstallResource{}},
		{"InstallRow", []string{
			"machine_id", "display_name", "state", "title", "meaning", "next_step",
			"assigned", "observed", "from_disk", "runtime",
			"scope", "scope_id", "scope_label", "revision", "assigned_at", "assigned_by",
			"measured_at", "observed_at",
		}, InstallRow{}},
	} {
		got := softwareJSONFields(shape.value)
		if strings.Join(got, ",") != strings.Join(shape.fields, ",") {
			t.Errorf("%s 的欄位是 %v，這支測試認得的是 %v", shape.name, got, shape.fields)
		}
	}
}

// 匯出的檔案要答得出「這一列的版號量錯了檔案」。有人把它開在試算表裡照「看到的版號」
// 排序的時候，那兩個檔案欄位是唯一看得出來的東西。
func TestTheInstallCSVSaysWhichFileEachObservedVersionCameFrom(t *testing.T) {
	document := InstallReportCSV(installReportOf(t, installFixture(t)))
	index := map[string]int{}
	for i, column := range document.Columns {
		index[column.Key] = i
	}
	for _, key := range []string{"runtime", "measured_file", "running_file"} {
		if _, ok := index[key]; !ok {
			t.Fatalf("CSV 少了 %q 這一欄：%+v", key, document.Columns)
		}
	}
	rows := map[string][]string{}
	for _, row := range document.Rows {
		rows[row[index["machine"]]] = row
	}
	samplehub1 := rows["samplehub1"]
	if samplehub1 == nil {
		t.Fatal("CSV 裡沒有 samplehub1 那一列")
	}
	if samplehub1[index["runtime"]] != ToolRuntimeTitle(ToolRuntimeOtherFile) {
		t.Errorf("samplehub1 那一列的 runtime 欄是 %q", samplehub1[index["runtime"]])
	}
	if samplehub1[index["measured_file"]] != installLoginCopy ||
		samplehub1[index["running_file"]] != installReleaseCopy {
		t.Errorf("那兩個檔案沒有各佔一欄：量的是 %q、跑的是 %q",
			samplehub1[index["measured_file"]], samplehub1[index["running_file"]])
	}
	// ⚠ 沒有這一軸的那一列三欄都留白，不是寫一個看起來像答案的字。
	sampleagent1 := rows["sampleagent1"]
	if sampleagent1 == nil {
		t.Fatal("CSV 裡沒有 sampleagent1 那一列")
	}
	for _, key := range []string{"runtime", "measured_file", "running_file"} {
		if sampleagent1[index[key]] != "" {
			t.Errorf("沒有被指派過的那一列 %s 欄寫了 %q", key, sampleagent1[index[key]])
		}
	}
}

// ⚠⚠ 每一種帶得出這一軸的狀態，都必須是一種「Hub 指派過」的狀態。這是資源那一行字
// 「其中 N 台看到的版號量的是沒在跑的那一份」講得通的前提：那個 N 是它前面那個「M/總
// 數台被指派過」的一部分。一種 present 卻不 assigned 的狀態會讓那個 N 跑出分母外面。
func TestEveryStateThatCarriesTheRuntimeAxisIsAStateTheHubAssigned(t *testing.T) {
	present := 0
	for _, stateValue := range InstallStates() {
		if !InstallStateReportedPresent(stateValue) {
			continue
		}
		present++
		if !InstallStateAssigned(stateValue) {
			t.Errorf("%s 帶得出「版號講的是哪一份」，卻不是一種被指派過的狀態", stateValue)
		}
	}
	if present != 6 {
		t.Errorf("有 %d 種狀態帶得出這一軸，這支測試認得的是 6 種", present)
	}
}
