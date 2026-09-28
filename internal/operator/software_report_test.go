package operator

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

// 正式機隊上 openclaw 的兩份：量版號的那一份，與活著的 process 在跑的那一份。
const (
	softwareLoginCopy   = "/home/example-user/.local/bin/openclaw"
	softwareReleaseCopy = "/home/example-user/.local/share/clawctl/openclaw/releases/2026.6.10/bin/openclaw"
)

type softwareFleet struct {
	store *store.Store
	now   time.Time
	ids   map[string]string
}

// softwareFixture 造出正式環境現在真的有的每一種形狀：一台什麼都最新、一台落後、
// 一台裝了但問不到版號、一台從來沒回報過，加上一台已退役但留著觀測的。
func softwareFixture(t *testing.T) softwareFleet {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	fleet := softwareFleet{store: st, now: now, ids: map[string]string{}}

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
	// ⚠ openclaw 那兩格是正式機隊 2026-09-12 的形狀：兩台都是量人 PATH 上那一份、跑
	// release 目錄底下那一份，而且 shadowed 都是 false。兩台同一種狀態是刻意的——一份
	// 只會數到 1 的摘要，畫面上會少掉一台要人動手的機器。
	observe("samplehub1", []model.CLITool{
		{Name: "claude", Present: true, OnPath: true, PresentEvidence: "path",
			Path: "/home/example-user/.local/bin/claude", VersionReported: "2.1.195",
			RunningPID: 5001, RunningScript: "/home/example-user/.local/bin/claude",
			RunningExe: "/usr/bin/node"},
		{Name: "gemini", Present: false},
		{Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path",
			Path: softwareLoginCopy, RealPath: softwareLoginCopy, PathSource: "login",
			VersionReported: "2026.6.10", RunningPID: 5002,
			RunningScript: softwareReleaseCopy, RunningExe: "/usr/bin/node"},
	})
	observe("sampleagent2", []model.CLITool{
		{Name: "claude", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "2.1.100", DaemonReach: model.DaemonReachShadowed,
			RunningReason: "只掃得到 3 個 process，這台的 /proc 視野被限制了",
			ProcessScan:   model.ProcessScanRestricted},
		{Name: "gemini", Present: false},
		{Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path",
			Path: softwareLoginCopy, RealPath: softwareLoginCopy, PathSource: "login",
			VersionPackageJSON: "2026.6.10", RunningPID: 5003,
			RunningScript: softwareReleaseCopy, RunningExe: "/usr/bin/node"},
	})
	observe("sampleagent3", []model.CLITool{
		// ⚠ 這一格在跑的檔案已經被 unlink 了。那是 /proc 講的事實，跟認不認得出是哪
		// 一份無關——那個 process 在跑的東西，磁碟上已經沒有了。
		{Name: "claude", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReason: "問不到版號", RunningPID: 5004,
			RunningScript: "/home/example-user/.local/bin/claude.old (deleted)"},
		{Name: "gemini", Present: false},
	})
	// ⚠ sampleagent4 刻意一筆觀測都沒有：它在分母裡，而它每一格都必須是「沒回報過」。

	// 已退役的那一台身上有一個比誰都新的版號。它不可以出現在報告裡，也不可以決定
	// 「機隊裡最新的是哪一版」。
	// ⚠ legacy-tool 只有這一台有過。它退役之後，這個工具不可以繼續在報告上佔一列——
	// 那一列上每一台活著的機器都會是「沒回報過」，而那是一個永遠不會有人去處理的
	// 待辦：沒有人少裝了任何東西。
	observe("retired", []model.CLITool{
		{Name: "claude", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "9.9.9"},
		{Name: "legacy-tool", Present: true, OnPath: true, PresentEvidence: "path",
			VersionReported: "0.1.0"},
	})
	// ⚠ 用真的現在退役：名冊列的 created_at 是 Store 自己的鐘寫的，而它一定晚於這個
	// 測試一開始算的 now，拿 now 去退役會被 store 擋下來。
	if err := st.RetireMachine(fleet.ids["retired"], time.Now().UTC()); err != nil {
		t.Fatalf("退役: %v", err)
	}
	return fleet
}

func softwareToolNamed(t *testing.T, report SoftwareReport, name string) SoftwareTool {
	t.Helper()
	for _, tool := range report.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("報告裡沒有 %q：%+v", name, report.Tools)
	return SoftwareTool{}
}

func softwareRowFor(t *testing.T, tool SoftwareTool, displayName string) SoftwareRow {
	t.Helper()
	for _, row := range tool.Rows {
		if row.DisplayName == displayName {
			return row
		}
	}
	t.Fatalf("%s 這個工具沒有 %q 那一列：%+v", tool.Name, displayName, tool.Rows)
	return SoftwareRow{}
}

// TestTheSoftwareReportCountsExactlyWhatTheEnrollmentReportCallsTheDenominator 是
// 這份報告的防漂測試。兩邊各自算一次的話，畫面上會出現「註冊報告說 4 台」而
// 「軟體清查說 3 台」，而那兩個數字沒有一個問得出誰是對的。
func TestTheSoftwareReportCountsExactlyWhatTheEnrollmentReportCallsTheDenominator(t *testing.T) {
	fleet := softwareFixture(t)
	service := New(fleet.store)
	software, err := service.SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := service.EnrollmentReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if software.Machines != enrollment.Denominator {
		t.Fatalf("軟體清查算 %d 台，註冊報告的分母是 %d 台",
			software.Machines, enrollment.Denominator)
	}
	for _, tool := range software.Tools {
		if got := tool.InstalledOn + tool.AbsentOn + tool.UnreportedOn; got != software.Machines {
			t.Fatalf("%s 的三種答案加起來 %d，分母是 %d", tool.Name, got, software.Machines)
		}
		if len(tool.Rows) != software.Machines {
			t.Fatalf("%s 有 %d 列，分母是 %d 台", tool.Name, len(tool.Rows), software.Machines)
		}
	}
}

// TestARetiredMachineNeitherAppearsNorDecidesWhatNewestMeans 釘的是一台已經離開分
// 母的機器不可以繼續替機隊決定「最新是哪一版」。
func TestARetiredMachineNeitherAppearsNorDecidesWhatNewestMeans(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	claude := softwareToolNamed(t, report, "claude")
	if claude.Newest != "2.1.195" {
		t.Fatalf("已退役那台的 9.9.9 把最新版蓋掉了：newest=%q", claude.Newest)
	}
	for _, row := range claude.Rows {
		if row.DisplayName == "old-box" {
			t.Fatalf("已退役的機器出現在報告裡：%+v", row)
		}
	}
	for _, version := range claude.Versions {
		if version.Version == "9.9.9" {
			t.Fatalf("已退役那台的版號進了版本分佈：%+v", claude.Versions)
		}
	}
	// ⚠ 只有退役那台有過的工具，整列都不該在：留著它，機隊上每一台都會是「沒回報
	// 過」，而那一列永遠不會變綠——沒有人少裝了任何東西。
	for _, tool := range report.Tools {
		if tool.Name == "legacy-tool" {
			t.Fatalf("只有已退役那台有過的工具還在報告上佔一列：%+v", tool)
		}
	}
}

// TestNotReportedIsNotTheSameAsNotInstalled 是這份報告最重要的一句話。把沉默講成
// 「沒有安裝」，等於讓一台 agent 停掉的機器看起來像一個已知的答案。
func TestNotReportedIsNotTheSameAsNotInstalled(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	gemini := softwareToolNamed(t, report, "gemini")
	absent := softwareRowFor(t, gemini, "samplehub1")
	silent := softwareRowFor(t, gemini, "sampleagent4")
	if absent.State != SoftwareAbsent || silent.State != SoftwareUnreported {
		t.Fatalf("兩種沒有被混成一種：samplehub1=%q sampleagent4=%q", absent.State, silent.State)
	}
	if absent.Meaning == silent.Meaning || absent.NextStep == silent.NextStep {
		t.Fatalf("兩種沒有講同一句話：\n absent=%+v\n silent=%+v", absent, silent)
	}
	if silent.ObservedAt != nil || silent.MeasuredAt != nil {
		t.Fatalf("沒回報過的那一列帶了時刻：%+v", silent)
	}
	// openclaw 在 sampleagent3 上也沒回報過，因為那一台的觀測裡根本沒有這個工具。
	openclaw := softwareToolNamed(t, report, "openclaw")
	if got := softwareRowFor(t, openclaw, "sampleagent3"); got.State != SoftwareUnreported {
		t.Fatalf("sampleagent3 沒回報 openclaw，卻被講成 %q", got.State)
	}
}

// TestTheNewestVersionIsCountedNumericallyNotAsAString 釘住實測踩過的坑：字串比會
// 說 1.0.3 比 1.0.13 新，於是畫面會叫人把一台其實比較新的機器降級。
func TestTheNewestVersionIsCountedNumericallyNotAsAString(t *testing.T) {
	for _, tc := range []struct {
		name, a, b string
		want       int
		comparable bool
	}{
		{"點分數字逐段比", "1.0.13", "1.0.3", 1, true},
		{"段數不同補零", "2.1", "2.1.0", 0, true},
		{"日期版號同一套規則", "2026.6.10", "2026.5.20", 1, true},
		{"pre-release 不猜", "1.0.3-beta", "1.0.3", 0, false},
		{"前面補零不是同一個寫法", "2.01", "2.1", 0, false},
		{"空字串比不出來", "", "1.0.0", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := compareSoftwareVersions(tc.a, tc.b)
			if ok != tc.comparable || (ok && got != tc.want) {
				t.Fatalf("compare(%q,%q)=(%d,%v) want (%d,%v)",
					tc.a, tc.b, got, ok, tc.want, tc.comparable)
			}
		})
	}
}

// TestAVersionThatCannotBeComparedIsNeverCalledBehind 釘的是這份報告唯一一個會憑空
// 生出答案的地方：比不出來就說比不出來，不可以講成落後。
func TestAVersionThatCannotBeComparedIsNeverCalledBehind(t *testing.T) {
	fleet := softwareFixture(t)
	if err := fleet.store.RecordObservation(fleet.ids["sampleagent3"], model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: fleet.now.Add(-time.Minute),
		CLITools: []model.CLITool{{Name: "claude", Present: true, OnPath: true,
			PresentEvidence: "path", VersionReported: "2.1.195-rc1"}},
	}, fleet.now); err != nil {
		t.Fatal(err)
	}
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	claude := softwareToolNamed(t, report, "claude")
	row := softwareRowFor(t, claude, "sampleagent3")
	if row.State != SoftwareIncomparable {
		t.Fatalf("比不出來的版號被講成 %q：%+v", row.State, row)
	}
	if claude.Newest != "2.1.195" {
		t.Fatalf("比不出來的版號當選了最新：%q", claude.Newest)
	}
	for _, version := range claude.Versions {
		if version.Version == "2.1.195-rc1" && version.Newest {
			t.Fatalf("比不出來的版號被標成最新：%+v", version)
		}
	}
}

// TestAVersionReadOffDiskSaysSoRatherThanPassingAsSelfReported 釘住「它自己講的」與
// 「我從檔案裡讀的」是兩件事——後者在工具壞掉的時候會跟真正跑起來的版本不一樣。
func TestAVersionReadOffDiskSaysSoRatherThanPassingAsSelfReported(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	openclaw := softwareToolNamed(t, report, "openclaw")
	fromDisk := softwareRowFor(t, openclaw, "sampleagent2")
	selfReported := softwareRowFor(t, openclaw, "samplehub1")
	if !fromDisk.FromDisk || fromDisk.Version != "2026.6.10" {
		t.Fatalf("檔案上讀到的版號沒有標明來源：%+v", fromDisk)
	}
	if selfReported.FromDisk {
		t.Fatalf("它自己講的版號被標成從檔案讀的：%+v", selfReported)
	}
}

// TestInstalledWithoutAVersionIsItsOwnAnswer：裝了但問不到版號，跟沒裝是兩件事。
func TestInstalledWithoutAVersionIsItsOwnAnswer(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	claude := softwareToolNamed(t, report, "claude")
	row := softwareRowFor(t, claude, "sampleagent3")
	if row.State != SoftwareNoVersion || row.Version != "" {
		t.Fatalf("裝了但問不到版號的那一列不對：%+v", row)
	}
	if !SoftwareStateInstalled(row.State) {
		t.Fatal("問不到版號被算成沒裝")
	}
	if claude.InstalledOn != 3 {
		t.Fatalf("claude 裝在 3 台上（samplehub1、sampleagent2、sampleagent3），算出 %d", claude.InstalledOn)
	}
}

// TestTheVersionRollupAddsUpToTheMachinesThatHaveIt 釘住版本分佈不是另一份真相：
// 每一版的台數加起來，必須等於有版號的那幾台。
func TestTheVersionRollupAddsUpToTheMachinesThatHaveIt(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range report.Tools {
		withVersion, total := 0, 0
		for _, row := range tool.Rows {
			if row.Version != "" {
				withVersion++
			}
		}
		newest := 0
		for _, version := range tool.Versions {
			total += version.Machines
			if version.Newest {
				newest++
			}
		}
		if total != withVersion {
			t.Fatalf("%s 的版本分佈加起來 %d，有版號的是 %d 台", tool.Name, total, withVersion)
		}
		if tool.Spread != len(tool.Versions) {
			t.Fatalf("%s 的 spread=%d，版本分佈有 %d 種", tool.Name, tool.Spread, len(tool.Versions))
		}
		if newest > 1 {
			t.Fatalf("%s 有 %d 個版號同時是最新", tool.Name, newest)
		}
	}
}

// TestTheReportSaysWhatItCannotKnow 釘住那句限制留在資料上，不是只留在註解裡。
// 沒有這句話的話，一張全綠的表會被讀成「都是上游最新版」。
func TestTheReportSaysWhatItCannotKnow(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Caveat != SoftwareReportCaveat || report.Caveat == "" {
		t.Fatalf("報告沒有帶著那句限制：%q", report.Caveat)
	}
	for _, banned := range []string{"該升級", "請升級", "最新版本是"} {
		if strings.Contains(report.Headline, banned) || strings.Contains(report.Caveat, banned) {
			t.Fatalf("報告講了它不知道的事：%q / %q", report.Headline, report.Caveat)
		}
		for _, tool := range report.Tools {
			if strings.Contains(tool.Headline, banned) {
				t.Fatalf("%s 那一行講了它不知道的事：%q", tool.Name, tool.Headline)
			}
		}
	}
}

// TestTheReportSaysHowManyMachinesHaveNotReportedAnythingAtAll：一台一個工具都沒回
// 報過的機器，要在最上面那一句裡被數出來，不是只出現在每一列裡。
func TestTheReportSaysHowManyMachinesHaveNotReportedAnythingAtAll(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Machines != 4 || report.Reporting != 3 {
		t.Fatalf("分母 %d、回報過 %d，應該是 4 與 3", report.Machines, report.Reporting)
	}
	wantHeadline := fmt.Sprintf("分母 %d 台，%d 台回報過；%d 個工具",
		report.Machines, report.Reporting, len(report.Tools))
	if !strings.HasPrefix(report.Headline, wantHeadline) {
		t.Fatalf("摘要是 %q，期望以前綴 %q 開頭", report.Headline, wantHeadline)
	}
	wantNextStep := fmt.Sprintf("有 %d 台一個工具都沒回報過",
		report.Machines-report.Reporting)
	if !strings.Contains(report.NextStep, wantNextStep) {
		t.Fatalf("下一步是 %q，期望包含 %q", report.NextStep, wantNextStep)
	}
}

// TestEveryStateCountAddsUpToEveryCell 釘住彙總不是另外算一次：六種狀態的格數加起
// 來，必須剛好是工具數 × 分母。
func TestEveryStateCountAddsUpToEveryCell(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	seen := map[SoftwareState]bool{}
	for _, count := range report.States {
		if seen[count.State] {
			t.Fatalf("狀態 %q 出現兩次", count.State)
		}
		seen[count.State] = true
		if count.Title == "" || count.Meaning == "" {
			t.Fatalf("狀態 %q 沒有句子：%+v", count.State, count)
		}
		total += count.Count
	}
	if len(report.States) != len(SoftwareStates()) {
		t.Fatalf("彙總有 %d 種狀態，canonical 有 %d 種", len(report.States), len(SoftwareStates()))
	}
	if want := len(report.Tools) * report.Machines; total != want {
		t.Fatalf("狀態格數加起來 %d，工具數 × 分母是 %d", total, want)
	}
}

// TestAReportWithoutATimeIsRefusedRatherThanDatedNow：沒有評估時刻就拒絕，不要自己
// 挑一個——一份標著錯時間的報告比沒有報告更難察覺。
// ⚠ 只有要人動手的那兩種算「錯歸因」。把「說不出來」與「沒有找到在跑它的 process」
// 也算進去的話，機隊上每一格都會變成待辦事項，然後沒有人再看這個數字——而正式機隊
// 24 格裡有 20 格落在那兩種「我不知道」上。
func TestOnlyTheTwoStatesThatNeedAHandAreCalledMisattributed(t *testing.T) {
	want := map[ToolRuntime]bool{
		ToolRuntimeOtherFile:    true,
		ToolRuntimeGoneFile:     true,
		ToolRuntimeSameFile:     false,
		ToolRuntimeUnattributed: false,
		ToolRuntimeUnscanned:    false,
		ToolRuntimeIdle:         false,
		ToolRuntimeUnstated:     false,
	}
	if len(want) != len(ToolRuntimes()) {
		t.Fatalf("這張表有 %d 種，這個版本認得 %d 種", len(want), len(ToolRuntimes()))
	}
	for _, stateValue := range ToolRuntimes() {
		expected, listed := want[stateValue]
		if !listed {
			t.Fatalf("%q 沒有在這張表上", stateValue)
		}
		if got := ToolRuntimeMisattributed(stateValue); got != expected {
			t.Errorf("%q 算不算錯歸因：%v，該是 %v", stateValue, got, expected)
		}
	}
}

// ⚠⚠ 那一句不能省。上面那個「都一致」比的是一批檔案的版號；有格子的版號量的是沒在
// 跑的那一份時，那個「一致」講的是一批沒有人在用的檔案，而一張全綠的表少了這一句會
// 被讀成機隊上跑的東西都對得起來。
func TestTheHeadlineSaysWhenAVersionIsNotTheOneRunning(t *testing.T) {
	consistent := SoftwareReport{Machines: 4, Reporting: 4, Tools: []SoftwareTool{{Name: "openclaw"}}}
	clean := SoftwareReportHeadline(consistent)
	if !strings.HasSuffix(clean, "的版號在機隊上都一致。") {
		t.Fatalf("沒有錯歸因的那一句是 %q", clean)
	}
	if strings.Contains(clean, "正在跑的") {
		t.Errorf("沒有錯歸因卻提了它：%q", clean)
	}
	consistent.Misattributed = 2
	dirty := SoftwareReportHeadline(consistent)
	if !strings.Contains(dirty, "另有 2 格的版號講的不是正在跑的那一份") {
		t.Errorf("有錯歸因卻沒講：%q", dirty)
	}
	if strings.HasSuffix(dirty, "都一致。") {
		t.Errorf("那一句停在「都一致。」：%q", dirty)
	}
	drifted := SoftwareReport{Machines: 4, Reporting: 4, Drifted: 1, Misattributed: 1,
		Tools: []SoftwareTool{{Name: "openclaw"}, {Name: "claude"}}}
	both := SoftwareReportHeadline(drifted)
	if !strings.Contains(both, "1 個的版號在機隊上不一致") ||
		!strings.Contains(both, "另有 1 格的版號講的不是正在跑的那一份") {
		t.Errorf("兩件事只講了一件：%q", both)
	}
}

// ⚠⚠ 先問在比哪一個檔案，再問誰落後。那幾格的版號量的是沒在跑的那一份，所以「誰比較
// 新」比的是一個沒有人在用的檔案——先叫人去升級，會升到一台其實沒事的機器上。
func TestTheNextStepAsksWhichFileBeforeItAsksWhoIsBehind(t *testing.T) {
	both := softwareReportNextStep(SoftwareReport{
		Machines: 4, Reporting: 4, Drifted: 1, Misattributed: 1,
	})
	if both != "先看那幾格版號講的不是正在跑的那一份，再談哪一台落後。" {
		t.Fatalf("兩件事都有的時候那一句是 %q", both)
	}
	onlyDrifted := softwareReportNextStep(SoftwareReport{Machines: 4, Reporting: 4, Drifted: 1})
	if onlyDrifted != "版號不一致的工具點開來看是哪幾台落後。" {
		t.Errorf("只有版號不一致的時候那一句是 %q", onlyDrifted)
	}
	// ⚠ 有機器一台工具都沒回報過的時候，那一句還是排在最前面：一個問不到的分母比
	// 兩件比較的事都嚴重。
	const machines, reporting = 4, 3
	silent := softwareReportNextStep(SoftwareReport{
		Machines: machines, Reporting: reporting, Misattributed: 1,
	})
	wantSilent := fmt.Sprintf("有 %d 台一個工具都沒回報過，先去看它們的 agent 有沒有在回報。",
		machines-reporting)
	if silent != wantSilent {
		t.Errorf("有機器完全沉默的時候那一句是 %q，期望是 %q", silent, wantSilent)
	}
}

// ⚠⚠ 加一個欄位就要把版號 +1。對面的 strict decoder 對著一個它不認得的欄位會整份拒收，
// 所以悄悄加欄位會讓舊的用戶端在解碼那一刻才壞——而那時候它已經印了半份畫面。改了下面
// 任何一張清單，就把 SoftwareReportSchemaVersion 一起改。
func TestAFieldAddedToTheSoftwareReportComesWithANewSchemaVersion(t *testing.T) {
	if SoftwareReportSchemaVersion != 2 {
		t.Fatalf("版號是 %d；欄位清單跟它必須一起動", SoftwareReportSchemaVersion)
	}
	for _, shape := range []struct {
		name   string
		fields []string
		value  any
	}{
		{"SoftwareReport", []string{
			"schema_version", "evaluated_at", "machines", "reporting", "drifted",
			"misattributed", "tools", "states", "runtime_states", "headline", "caveat", "next_step",
		}, SoftwareReport{}},
		{"SoftwareRow", []string{
			"machine_id", "display_name", "state", "title", "meaning", "next_step",
			"version", "from_disk", "shadowed", "runtime", "measured_at", "observed_at",
		}, SoftwareRow{}},
		{"ToolRuntimeCount", []string{
			"state", "title", "count", "meaning", "next_step",
		}, ToolRuntimeCount{}},
		{"ToolRuntimeFinding", []string{
			"state", "title", "meaning", "next_step", "measured_file", "running_file",
		}, ToolRuntimeFinding{}},
	} {
		got := softwareJSONFields(shape.value)
		if strings.Join(got, ",") != strings.Join(shape.fields, ",") {
			t.Errorf("%s 的欄位是 %v，這支測試認得的是 %v", shape.name, got, shape.fields)
		}
	}
}

// softwareJSONFields 照宣告順序列出一個 DTO 的 JSON 欄位名。omitempty 不影響它：
// 一個欄位在零值的時候不出現，照樣是對面 decoder 認不認得的那個欄位。
func softwareJSONFields(value any) []string {
	shape := reflect.TypeOf(value)
	names := make([]string, 0, shape.NumField())
	for index := range shape.NumField() {
		tag, _, _ := strings.Cut(shape.Field(index).Tag.Get("json"), ",")
		names = append(names, tag)
	}
	return names
}

// ⚠ 這一軸的摘要也要逐格數過，不是只檢查它們加得起來。正式機隊上同一種狀態通常有好
// 幾格——一份只數到 1 的摘要照樣加得起來（其他狀態會多算），而畫面上會少掉一台要人動
// 手的機器。
func TestEveryRuntimeCountAddsUpToEveryCellThatHasAVersion(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	counted := map[ToolRuntime]int{}
	cells, misattributed := 0, 0
	for _, tool := range report.Tools {
		for _, row := range tool.Rows {
			// ⚠⚠ 有這一軸就等於這台上有這個工具。沒有版號的那兩格沒有「這個版號講的
			// 是哪一份」可問，把它們算進來會讓分母變成工具數 × 分母。
			if SoftwareStateInstalled(row.State) != (row.Runtime != nil) {
				t.Fatalf("%s × %s 是 %q，這一軸卻是 %+v",
					tool.Name, row.DisplayName, row.State, row.Runtime)
			}
			if row.Runtime == nil {
				continue
			}
			cells++
			counted[row.Runtime.State]++
			if ToolRuntimeMisattributed(row.Runtime.State) {
				misattributed++
			}
		}
	}
	if len(report.RuntimeStates) != len(ToolRuntimes()) {
		t.Fatalf("彙總有 %d 種，canonical 有 %d 種",
			len(report.RuntimeStates), len(ToolRuntimes()))
	}
	total := 0
	for index, stateCount := range report.RuntimeStates {
		if stateCount.State != ToolRuntimes()[index] {
			t.Fatalf("第 %d 種是 %q，canonical 那裡是 %q",
				index, stateCount.State, ToolRuntimes()[index])
		}
		if stateCount.Title == "" || stateCount.Meaning == "" {
			t.Fatalf("%q 沒有句子：%+v", stateCount.State, stateCount)
		}
		if stateCount.Count != counted[stateCount.State] {
			t.Errorf("%q 的摘要說 %d 格，逐格數出 %d 格",
				stateCount.State, stateCount.Count, counted[stateCount.State])
		}
		total += stateCount.Count
	}
	if total != cells {
		t.Fatalf("這一軸加起來 %d 格，有版號可問的是 %d 格", total, cells)
	}
	if report.Misattributed != misattributed {
		t.Errorf("報告說 %d 格錯歸因，逐格數出 %d 格", report.Misattributed, misattributed)
	}
	// fixture 要真的造出「同一種狀態有兩格」，不然這支測試證明不了摘要有在累加。
	if counted[ToolRuntimeOtherFile] < 2 {
		t.Fatalf("fixture 只造出 %d 格「正在跑的是另一個檔案」", counted[ToolRuntimeOtherFile])
	}
}

// restricted 是掃描邊界，不是「掃完而且沒找到」。這支從 store 裡的線上 payload 一路走到
// 報告逐格與摘要，釘住兩者都落在 unscanned，不會被舊的 RunningReason 分支收進 idle。
func TestARestrictedProcessScanIsCountedAsUnscannedInTheSoftwareReport(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tool := range report.Tools {
		if tool.Name != "claude" {
			continue
		}
		for _, row := range tool.Rows {
			if row.MachineID == fleet.ids["sampleagent2"] {
				found = true
				if row.Runtime == nil || row.Runtime.State != ToolRuntimeUnscanned {
					t.Fatalf("restricted 那格 runtime=%+v", row.Runtime)
				}
			}
		}
	}
	if !found {
		t.Fatal("報告裡找不到 sampleagent2 × claude")
	}
	counts := map[ToolRuntime]int{}
	for _, count := range report.RuntimeStates {
		counts[count.State] = count.Count
	}
	if counts[ToolRuntimeUnscanned] != 1 {
		t.Errorf("unscanned 摘要=%d，該是 1", counts[ToolRuntimeUnscanned])
	}
	if counts[ToolRuntimeIdle] != 0 {
		t.Errorf("idle 摘要=%d，restricted 不該算進去", counts[ToolRuntimeIdle])
	}
}

// ⚠⚠ 一個已經被 unlink 的檔案是 /proc 講的事實，它自己就是一個發現：那個 process 還
// 活著，而它在跑的檔案磁碟上已經沒有了。把它跟「正在跑的是另一個檔案」併在一起的話，
// 那一格的下一步會變成「收掉另一份」，而該做的是重啟它。
func TestARunningFileThatIsGoneIsItsOwnFindingInTheReport(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	row := softwareRowFor(t, softwareToolNamed(t, report, "claude"), "sampleagent3")
	if row.Runtime == nil || row.Runtime.State != ToolRuntimeGoneFile {
		t.Fatalf("sampleagent3 的 claude 那一軸是 %+v", row.Runtime)
	}
	if row.Runtime.RunningFile != "/home/example-user/.local/bin/claude.old" {
		t.Errorf("正在跑的那個檔案是 %q，該把 (deleted) 收掉", row.Runtime.RunningFile)
	}
	if row.Runtime.NextStep != ToolRuntimeNextStep(ToolRuntimeGoneFile) {
		t.Errorf("下一步是 %q", row.Runtime.NextStep)
	}
	if !ToolRuntimeMisattributed(row.Runtime.State) {
		t.Errorf("那個版號量的不是正在跑的那一份，卻沒算進錯歸因")
	}
}

func TestAReportWithoutATimeIsRefusedRatherThanDatedNow(t *testing.T) {
	fleet := softwareFixture(t)
	if _, err := New(fleet.store).SoftwareReport(time.Time{}); !errors.Is(err, ErrInvalidSoftwareReport) {
		t.Fatalf("沒有評估時刻卻產得出報告：%v", err)
	}
}

// TestAShadowedToolSaysSoOnItsOwnRow：clawctl 自己那條 PATH 解到另一個檔案，是這一
// 列上要講的事實——它決定「以後 clawctl 自己去執行它」會不會跑到另一個版本。
func TestAShadowedToolSaysSoOnItsOwnRow(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	claude := softwareToolNamed(t, report, "claude")
	if !softwareRowFor(t, claude, "sampleagent2").Shadowed {
		t.Fatal("被遮住的那一列沒有講出來")
	}
	if softwareRowFor(t, claude, "samplehub1").Shadowed {
		t.Fatal("沒有被遮住的那一列卻說它被遮住了")
	}
}

// TestARunningFileFromAnotherDirectoryIsNotCalledShadowed 釘的是 software_report.go:155
// 的「兩個獨立的軸，不准合起來」。
//
// fixture 第 70-72 行那兩格 openclaw 就是正式機隊 2026-09-12 的形狀：量人 PATH
// 上那一份、跑 release 目錄底下那一份，shadowed 都是 false。形狀已經在 fixture 裡，
// 缺的是斷言。
//
// ⚠ 實測四臂（這一刀落地後，全樹 go test ./... -count=1）：
//   - 把 Runtime == ToolRuntimeOtherFile 併進 row.Shadowed 那個布林：全樹只有這一支紅。
//     下這一刀之前那一臂是全樹全綠——這個方向本來一個看守者都沒有。
//   - 反方向（Runtime 只在 Shadowed 為真時才給）：這一支加另外 13 支紅。那個方向早就守滿了。
//   - 對照，row.Shadowed 永遠 false：這一支**維持綠**，只有 TestAShadowedToolSaysSoOnItsOwnRow
//     紅。這一支沒有在 Shadowed 軸上白搭別人的覆蓋。
//   - 對照，toolRuntimeForFile 永遠說 same_file：這一支加另外 36 支紅。Runtime 軸本身也守滿了。
//
// 兩軸各自都有人守，缺的一直是「同一格上兩軸互相不准推導」。
//
// 合起來的傷害不是漏報，是報錯方向：一台 PATH 沒有分歧的機器會被畫成「另有一份在
// 別的 PATH 上」，operator 會去找一個不存在的檔案——正是 tool_runtime.go:155-158
// 警告的那件事。
func TestARunningFileFromAnotherDirectoryIsNotCalledShadowed(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}

	openclaw := softwareToolNamed(t, report, "openclaw")
	for _, machine := range []string{"samplehub1", "sampleagent2"} {
		row := softwareRowFor(t, openclaw, machine)
		if row.Runtime == nil || row.Runtime.State != ToolRuntimeOtherFile {
			t.Errorf("%s 的 openclaw runtime=%+v；PathSource 是 login、沒有 DaemonReach，但量版號的檔案和正在跑的檔案不同", machine, row.Runtime)
		}
		if row.Shadowed {
			t.Errorf("%s 的 openclaw PathSource 是 login、沒有 DaemonReach，人 PATH 與 daemon PATH 沒有分歧；說它 shadowed 會讓 operator 去找不存在的第二安裝，畫成『另有一份在別的 PATH 上』", machine)
		}
	}

	claude := softwareToolNamed(t, report, "claude")
	row := softwareRowFor(t, claude, "sampleagent2")
	if row.Runtime == nil || row.Runtime.State == ToolRuntimeOtherFile {
		t.Fatalf("sampleagent2 的 claude 確實 shadowed，但 RunningPID 是 0、ProcessScan 是 restricted，『正在跑的是哪一份』沒有答案；runtime=%+v。兩個象限同時存在，證明 Shadowed 與 Runtime 不能互相推導", row.Runtime)
	}
}

// TestATooLargeFleetIsRefusedRatherThanTruncated：一份少算的清查看起來就是全部。
func TestATooLargeFleetIsRefusedRatherThanTruncated(t *testing.T) {
	fleet := softwareFixture(t)
	for index := 0; index <= MaxSoftwareReportMachines; index++ {
		if _, err := fleet.store.DB().Exec(`
INSERT INTO machine_registry (machine_id, display_name, expected, created_at)
VALUES (?,?,1,?)`, "bulk-"+strconv.Itoa(index), "bulk-"+strconv.Itoa(index),
			fleet.now.Format("2006-01-02T15:04:05Z")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(fleet.store).SoftwareReport(fleet.now); !errors.Is(err, ErrInvalidSoftwareReport) {
		t.Fatalf("分母讀不完卻回了一份報告：%v", err)
	}
}

// TestTheSameFleetProducesTheSameNewestEveryTime 是一個實測過的迴歸。
//
// ⚠⚠ 第一版的選舉是「第一個拿到的先當最新，後面比得贏才換」，而候選人是直接
// range 一個 map。Go 的 map 迭代順序是隨機的，所以當 map 剛好先吐出一個比不出來
// 的版號（實測 "1.0.3-beta"）時，它就佔住了位子、之後每一次比較都回 ok=false、
// 永遠換不掉——整列的基準變成空的，一張本來該說「1.0.13 最新、1.0.3 落後」的表
// 會變成兩格都是「比不出來」。30 跑 1 敗。
//
// 一個依賴 map 順序的判斷，錯的時候不會每次都錯——它會偶爾錯，而偶爾錯的燈會被
// 當成雜訊。所以這個測試跑很多次，而且斷言的是**每一次都一樣**。
func TestTheSameFleetProducesTheSameNewestEveryTime(t *testing.T) {
	fleet := softwareFixture(t)
	for key, version := range map[string]string{
		"samplehub1": "1.0.3", "sampleagent2": "1.0.13", "sampleagent3": "1.0.3-beta",
	} {
		if err := fleet.store.RecordObservation(fleet.ids[key], model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: fleet.now.Add(-time.Minute),
			CLITools: []model.CLITool{{Name: "grok", Present: true, OnPath: true,
				PresentEvidence: "path", VersionReported: version}},
		}, fleet.now); err != nil {
			t.Fatal(err)
		}
	}
	service := New(fleet.store)
	for round := 0; round < 60; round++ {
		report, err := service.SoftwareReport(fleet.now)
		if err != nil {
			t.Fatal(err)
		}
		grok := softwareToolNamed(t, report, "grok")
		if grok.Newest != "1.0.13" {
			t.Fatalf("第 %d 次跑出來的最新是 %q，該是 1.0.13——"+
				"一個會隨 map 順序改變的基準，錯的時候只會偶爾錯", round, grok.Newest)
		}
		if state := softwareRowFor(t, grok, "sampleagent2").State; state != SoftwareNewest {
			t.Fatalf("第 %d 次 sampleagent2（1.0.13）是 %q，該是最新", round, state)
		}
		if state := softwareRowFor(t, grok, "samplehub1").State; state != SoftwareBehind {
			t.Fatalf("第 %d 次 samplehub1（1.0.3）是 %q，該是落後——"+
				"字串比大小會說 1.0.3 比 1.0.13 新，然後叫人把 sampleagent2 降級", round, state)
		}
		if state := softwareRowFor(t, grok, "sampleagent3").State; state != SoftwareIncomparable {
			t.Fatalf("第 %d 次 sampleagent3（1.0.3-beta）是 %q，該是比不出來", round, state)
		}
	}
}

// TestNoOneHasItDoesNotSwallowTheMachinesThatNeverReported 是一個在正式機隊上看到的
// 缺陷。
//
// ⚠⚠ 一個沒有任何一台裝著的工具，那一行字寫的是「5 台上都沒有」——但那 5 台裡有
// 一台（sampleagent1）從來沒回報過。那一句把沉默講成了一個已知的答案，而這整份報告存在
// 的理由就是那兩件事要分開：一台沒裝的要去裝東西，一台沒回報的要去看它的 agent。
// 那一行字是最多人只讀那一行就走的地方，所以它不可以是六種狀態裡唯一講錯的那句。
func TestNoOneHasItDoesNotSwallowTheMachinesThatNeverReported(t *testing.T) {
	fleet := softwareFixture(t)
	report, err := New(fleet.store).SoftwareReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	gemini := softwareToolNamed(t, report, "gemini")
	if gemini.InstalledOn != 0 || gemini.AbsentOn == 0 || gemini.UnreportedOn == 0 {
		t.Fatalf("fixture 沒有造出「沒有一台裝著，但有一台沒回報過」：%+v", gemini)
	}
	if strings.Contains(gemini.Headline, strconv.Itoa(report.Machines)+" 台上都沒有") {
		t.Fatalf("那一行字把沒回報過的那台算成「沒有」：%q", gemini.Headline)
	}
	for _, want := range []string{
		strconv.Itoa(gemini.AbsentOn) + " 台上沒有",
		strconv.Itoa(gemini.UnreportedOn) + " 台沒回報過",
	} {
		if !strings.Contains(gemini.Headline, want) {
			t.Errorf("那一行字少了 %q：%q", want, gemini.Headline)
		}
	}
	// 每一台都沒回報過的時候，那一行字要講的是沉默，不是「都沒有」。
	silent := SoftwareTool{Name: "nobody-asked", UnreportedOn: 4}
	if headline := SoftwareToolHeadline(silent); !strings.Contains(headline, "都還沒回報過") {
		t.Errorf("四台都沒回報過，那一行字卻是 %q", headline)
	}
	// 真的每一台都回報過而且都說沒有的時候，「都沒有」才是實話。
	absent := SoftwareTool{Name: "nobody-has-it", AbsentOn: 4}
	if headline := SoftwareToolHeadline(absent); !strings.Contains(headline, "4 台上都沒有") {
		t.Errorf("四台都說沒有，那一行字卻是 %q", headline)
	}
}
