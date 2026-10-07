package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

// 正式庫 2026-09-12 量到的那兩個檔案。
const (
	installCLILoginCopy   = "/usr/local/bin/openclaw"
	installCLIReleaseCopy = "/home/example-user/.local/share/clawctl/releases/2026.6.10/openclaw"
)

// installCLIFixture 造一個機隊，讓報告上最難講清楚的那幾格都真的出現：指派的跟看到
// 的一樣、指派的比看到的舊（而且那個版號是從檔案讀的、量在一個沒有人在跑的檔案上）、
// 指派了但這台上沒有、指派了但回報裡沒有這個東西、指派了但這台從來沒回報過、以及從來
// 沒有被指派過。
func installCLIFixture(t *testing.T) (string, machineCommandDeps) {
	t.Helper()
	f, now := reportFixture(t)
	silent, _, err := f.store.CreateEnrollTokenFor("never-came", 0)
	if err != nil {
		t.Fatal(err)
	}
	canary, _, err := f.store.CreateEnrollTokenFor("canary-box", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		CLITools: []model.CLITool{
			{Name: "claude", Present: true, OnPath: true,
				PresentEvidence: "path", VersionReported: "2.1.195"},
			{Name: "gemini", Present: false},
		},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordObservation(canary, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		CLITools: []model.CLITool{
			// ⚠ 正式庫上 samplehub1 就是這個樣子：版號量在 login copy，在跑的是
			// releases/ 那一份。這一列同時是「指派的比看到的舊」與「那個版號講的不是
			// 正在跑的那一份」。
			{Name: "openclaw", Present: true, OnPath: true,
				PresentEvidence: "path", VersionPackageJSON: "2026.6.10",
				Path: installCLILoginCopy, RealPath: installCLILoginCopy,
				RunningPID: 4131, RunningScript: installCLIReleaseCopy},
		},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMachineChannel(canary, "canary"); err != nil {
		t.Fatal(err)
	}
	for _, intent := range []struct {
		scopeType, scopeID, kind, id, spec string
	}{
		{"machine", f.machine.id, "claude-cli", "claude-code", `{"kind":"claude-cli","version":"2.1.195"}`},
		{"machine", f.machine.id, "gemini-cli", "gemini", `{"kind":"gemini-cli","version":"1.0.0"}`},
		{"machine", f.machine.id, "codex-cli", "codex", `{"kind":"codex-cli","version":"0.9.0"}`},
		// ⚠ channel scope 那一筆是這份報告存在的理由：canary-box 自己沒有被指派過
		// openclaw，是它所在的 channel 被指派了，而 agent 收到的就是這一筆。
		{"channel", "canary", "openclaw", "openclaw", `{"kind":"claude-cli","version":"2026.5.26"}`},
		{"machine", silent, "claude-cli", "claude-code", `{"kind":"claude-cli","version":"2.1.195"}`},
	} {
		if _, _, err := f.store.CreateDesiredState(intent.scopeType, intent.scopeID,
			intent.kind, intent.id, intent.spec, "operator@test"); err != nil {
			t.Fatalf("%s/%s：%v", intent.kind, intent.id, err)
		}
	}
	return reportCLIServer(t, f)
}

func runInstallCLI(t *testing.T, argv ...string) string {
	t.Helper()
	base, deps := installCLIFixture(t)
	var out, errOut bytes.Buffer
	if err := runInstallReportCommandWithDeps(t.Context(),
		append([]string{"--hub-url", base}, argv...), &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	return out.String()
}

// CLI 上的那一份要跟畫面回答同一句話：我叫哪幾台裝什麼、我在它們上面看到什麼。
//
// ⚠⚠ 包含那句限制。terminal 上少了它，一列「指派的比看到的舊」會被讀成有人亂動這
// 台——而這個 Hub 看不到指派以外的安裝路徑，它根本沒有資格講那句話。
func TestInstallCLIPrintsWhatWasAssignedAndWhatItSees(t *testing.T) {
	text := runInstallCLI(t)
	for _, want := range []string{
		operator.InstallReportCaveat,
		"resources", "resource", "assigned", "matches", "differs", "unresolved", "unassigned",
		"machine", "state", "assigned", "observed", "assignment source", "next step",
		"claude", "gemini", "codex", "openclaw",
		"2.1.195", "2026.5.26", "2026.6.10 (read from file)",
		"指派給 canary channel", "指派給這台", "never-came",
		operator.InstallStateTitle(operator.InstallMatches),
		operator.InstallStateTitle(operator.InstallAssignedOlder),
		operator.InstallStateTitle(operator.InstallAbsent),
		operator.InstallStateTitle(operator.InstallUnobserved),
		operator.InstallStateTitle(operator.InstallUnreported),
		operator.InstallStateTitle(operator.InstallUnassigned),
		operator.InstallStateNextStep(operator.InstallAssignedOlder),
		operator.InstallStateNextStep(operator.InstallUnassigned),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("輸出少了 %q：\n%s", want, text)
		}
	}
}

// ⚠⚠ 「指派的比看到的舊」不是失敗。samplehub1 上真的跑著比指派新的那一版，因為
// releases/<ver>/ 那條路不經過部署——把它講成落後或裝失敗，會讓人去回滾一台好機器。
func TestInstallCLINeverCallsAnOlderAssignmentAFailure(t *testing.T) {
	text := runInstallCLI(t)
	for _, banned := range []string{"落後", "裝失敗", "安裝失敗", "裝不起來", "升級失敗"} {
		if strings.Contains(text, banned) {
			t.Errorf("輸出出現 %q：\n%s", banned, text)
		}
	}
}

func TestInstallCLIEmitsTheSameDocumentAsTheAPI(t *testing.T) {
	var report operator.InstallReport
	body := runInstallCLI(t, "--json")
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("--json 不是合法 JSON：%v\n%s", err, body)
	}
	if report.SchemaVersion != operator.InstallReportSchemaVersion ||
		report.Caveat != operator.InstallReportCaveat ||
		report.Assigned > report.Machines || len(report.Resources) == 0 {
		t.Fatalf("report=%+v", report)
	}
	for _, resource := range report.Resources {
		if total := resource.AssignedOn + resource.UnassignedOn; total != report.Machines {
			t.Fatalf("%s 的 %d 台，分母是 %d 台", resource.Name, total, report.Machines)
		}
		if len(resource.Rows) != report.Machines {
			t.Fatalf("%s 有 %d 列，分母是 %d 台", resource.Name, len(resource.Rows), report.Machines)
		}
	}
	if len(report.States) != len(operator.InstallStates()) {
		t.Fatalf("狀態有 %d 種，這個版本認得 %d 種", len(report.States), len(operator.InstallStates()))
	}
}

// ⚠ 表上那幾個數字要當場加得起來。被指派過比一樣加不一樣多出來的那幾台是真的存在
// 的，它們就是「對不起來」那一欄；少印那一欄，讀的人只會以為是自己算錯。
func TestInstallCLIResourceCountsAddUpOnScreen(t *testing.T) {
	var report operator.InstallReport
	if err := json.Unmarshal([]byte(runInstallCLI(t, "--json")), &report); err != nil {
		t.Fatal(err)
	}
	text := runInstallCLI(t)
	for _, resource := range report.Resources {
		unknown := resource.AssignedOn - resource.MatchingOn - resource.DifferingOn
		if unknown < 0 {
			t.Fatalf("%s：一樣加不一樣比被指派過還多", resource.Name)
		}
		var found bool
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 6 || fields[0] != resource.Name {
				continue
			}
			want := []string{
				strconv.Itoa(resource.AssignedOn), strconv.Itoa(resource.MatchingOn),
				strconv.Itoa(resource.DifferingOn), strconv.Itoa(unknown),
				strconv.Itoa(resource.MisattributedOn), strconv.Itoa(resource.UnassignedOn),
			}
			if len(fields) > 6 && strings.Join(fields[1:7], " ") == strings.Join(want, " ") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s 那一列不是 %d/%d/%d/%d/%d/%d：\n%s", resource.Name,
				resource.AssignedOn, resource.MatchingOn, resource.DifferingOn,
				unknown, resource.MisattributedOn, resource.UnassignedOn, text)
		}
	}
}

func TestInstallCLIExportsExactlyTheCellsItPrints(t *testing.T) {
	rows := reportCLIExportRows(t, runInstallCLI(t, "--csv"))
	var report operator.InstallReport
	if err := json.Unmarshal([]byte(runInstallCLI(t, "--json")), &report); err != nil {
		t.Fatal(err)
	}
	cells := 0
	for _, resource := range report.Resources {
		cells += len(resource.Rows)
	}
	if len(rows) != cells+1 {
		t.Fatalf("匯出 %d 列（含表頭），報告有 %d 格", len(rows), cells)
	}
	columns := operator.ReportCSVColumns(operator.InstallReportCSV(report))
	if len(rows[0]) != len(columns) {
		t.Fatalf("表頭 %d 欄，文件說 %d 欄", len(rows[0]), len(columns))
	}
	for index, column := range columns {
		if rows[0][index] != column.Header {
			t.Errorf("第 %d 欄是 %q，文件說 %q", index, rows[0][index], column.Header)
		}
	}
}

func TestInstallCLIRefusesWhatItCannotRun(t *testing.T) {
	base, deps := installCLIFixture(t)
	for name, argv := range map[string][]string{
		"positional 參數":     {"--hub-url", base, "openclaw"},
		"認不得的 flag":         {"--hub-url", base, "--resource", "openclaw"},
		"--json 與 --csv 並用": {"--hub-url", base, "--json", "--csv"},
		"hub-url 裡有控制字元":    {"--hub-url", base + "\x00"},
	} {
		var out, errOut bytes.Buffer
		if err := runInstallReportCommandWithDeps(t.Context(), argv,
			&out, &errOut, deps); err == nil {
			t.Errorf("%s：被接受了\n%s", name, out.String())
		}
	}
}

// installCLIResourceRow 印一份兩個資源的報告，回 short 那一列印出來的樣子。
func installCLIResourceRow(t *testing.T, otherNextStep string) string {
	t.Helper()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	resource := func(name, nextStep string) operator.InstallResource {
		return operator.InstallResource{
			Name: name, ResourceKind: name, ResourceID: name,
			AssignedOn: 1, MatchingOn: 1,
			Rows: []operator.InstallRow{{
				MachineID: "m1", DisplayName: "samplehub1", State: operator.InstallMatches,
				Title:   operator.InstallStateTitle(operator.InstallMatches),
				Meaning: operator.InstallStateMeaning(operator.InstallMatches),
			}},
			Headline: name + "：1/1 台被指派過。", NextStep: nextStep,
		}
	}
	report := operator.InstallReport{
		SchemaVersion: operator.InstallReportSchemaVersion, EvaluatedAt: now,
		Machines: 1, Assigned: 1, Caveat: operator.InstallReportCaveat,
		Resources: []operator.InstallResource{
			resource("short", "指派一次就好。"), resource("other", otherNextStep),
		},
	}
	var out bytes.Buffer
	if err := writeInstallReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "short") && strings.Contains(line, "指派一次就好。") {
			return line
		}
	}
	t.Fatalf("輸出沒有 short 那一列：\n%s", out.String())
	return ""
}

// 「下一步」沒有長度上限。它一旦進了對齊的欄位，一個資源的長句子就會把每一列都推
// 寬——包含那些跟它無關的列。
func TestTheInstallNextStepDoesNotWidenTheAlignedColumns(t *testing.T) {
	short := installCLIResourceRow(t, "指派一次就好。")
	long := installCLIResourceRow(t,
		strings.Repeat("決定哪一邊是對的：重新指派這台目前跑的那一版，或把這台部署回指派的那一版。", 40))
	if short != long {
		t.Fatalf("另一列的長下一步把這一列推寬了：\n%q\n%q", short, long)
	}
}

// installCLICellRow 印一份兩台機器的報告，回 short 那一台印出來的樣子。
func installCLICellRow(t *testing.T, otherNextStep string) string {
	t.Helper()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	row := func(name, nextStep string) operator.InstallRow {
		return operator.InstallRow{
			MachineID: name, DisplayName: name, State: operator.InstallUnassigned,
			Title:    operator.InstallStateTitle(operator.InstallUnassigned),
			Meaning:  operator.InstallStateMeaning(operator.InstallUnassigned),
			NextStep: nextStep,
		}
	}
	report := operator.InstallReport{
		SchemaVersion: operator.InstallReportSchemaVersion, EvaluatedAt: now,
		Machines: 2, Caveat: operator.InstallReportCaveat,
		Resources: []operator.InstallResource{{
			Name: "openclaw", ResourceKind: "openclaw", ResourceID: "openclaw",
			UnassignedOn: 2, Headline: "openclaw：0/2 台被指派過。",
			Rows: []operator.InstallRow{row("short", "指派一次。"), row("other", otherNextStep)},
		}},
	}
	var out bytes.Buffer
	if err := writeInstallReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "short") && strings.Contains(line, "指派一次。") {
			return line
		}
	}
	t.Fatalf("輸出沒有 short 那一列：\n%s", out.String())
	return ""
}

// 每一格自己的下一步也沒有長度上限，而且它跟資源那一層走的是不同的表。
func TestTheInstallCellNextStepDoesNotWidenTheAlignedColumns(t *testing.T) {
	short := installCLICellRow(t, "指派一次。")
	long := installCLICellRow(t,
		strings.Repeat("要讓這台裝它就指派一次；這個 Hub 只知道它沒有被指派過。", 40))
	if short != long {
		t.Fatalf("另一列的長下一步把這一列推寬了：\n%q\n%q", short, long)
	}
}

// ⚠⚠ 「看到的版號量的不是正在跑的那一份」那一段印在資源那張表前面。那張表上「不一
// 樣」那一欄是有人會直接照著動手的數字，而它可能比的是一個沒有人在跑的檔案。
func TestInstallCLIAsksWhichFileBeforeItPrintsTheResourceTable(t *testing.T) {
	text := runInstallCLI(t)
	misattributed := strings.Index(text, "visible versions measure an installation that is not running")
	resources := strings.Index(text, "resources\n")
	if misattributed < 0 {
		t.Fatalf("沒有印出量錯檔案那一段：\n%s", text)
	}
	if resources < 0 || misattributed > resources {
		t.Fatalf("兩段的順序不對：misattributed=%d resources=%d\n%s",
			misattributed, resources, text)
	}
	section := text[misattributed:resources]
	for _, want := range []string{
		"canary-box", "2026.5.26", "2026.6.10",
		installCLILoginCopy, installCLIReleaseCopy,
		operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile),
		operator.ToolRuntimeNextStep(operator.ToolRuntimeOtherFile),
	} {
		if !strings.Contains(section, want) {
			t.Errorf("那一段少了 %q：\n%s", want, section)
		}
	}
}

// ⚠ 路徑只出現在那一段專門的表上，不准進那張對齊的全表。一個路徑沒有長度上限——
// 進了對齊的欄位，一台機器的安裝路徑就會把每一列都推寬，然後整張表要橫向捲。
func TestInstallCLIKeepsThePathsOutOfThePerMachineTable(t *testing.T) {
	text := runInstallCLI(t)
	anchor := strings.Index(text, "openclaw：")
	if anchor < 0 {
		t.Fatalf("找不到 openclaw 那一段：\n%s", text)
	}
	section := text[anchor:]
	for _, banned := range []string{installCLILoginCopy, installCLIReleaseCopy} {
		if strings.Contains(section, banned) {
			t.Errorf("對齊的那張表上出現了路徑 %q：\n%s", banned, section)
		}
	}
	// 那一格照樣要講出這一列的版號講的是哪一份——只是講標題，不講路徑。
	var canary string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "canary-box") {
			canary = line
		}
	}
	if canary == "" {
		t.Fatalf("openclaw 那一段沒有 canary-box 那一列：\n%s", section)
	}
	if !strings.Contains(canary, operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile)) {
		t.Errorf("canary-box 那一列沒有講版號講的是哪一份：%q", canary)
	}
	// 沒有被量到的那一列在那一欄上要是破折號，不是一句看起來像答案的話。
	var never string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "never-came") {
			never = line
		}
	}
	if never == "" {
		t.Fatalf("openclaw 那一段沒有 never-came 那一列：\n%s", section)
	}
	for _, stateValue := range operator.ToolRuntimes() {
		if strings.Contains(never, operator.ToolRuntimeTitle(stateValue)) {
			t.Errorf("never-came 那一列印了 %q，它從來沒有被量到過：%q",
				operator.ToolRuntimeTitle(stateValue), never)
		}
	}
	if !strings.Contains(never, "—") {
		t.Errorf("never-came 那一列沒有留白：%q", never)
	}
}

// 這一軸的摘要要印出來，而且要講清楚只有哪兩種算量錯了檔案。六種都印成一列的話，
// 看起來像六件都要人動手。
func TestInstallCLIPrintsTheRuntimeSummary(t *testing.T) {
	text := runInstallCLI(t)
	anchor := strings.Index(text, "Observed version source:")
	if anchor < 0 {
		t.Fatalf("沒有印出這一軸的摘要：\n%s", text)
	}
	var report operator.InstallReport
	if err := json.Unmarshal([]byte(runInstallCLI(t, "--json")), &report); err != nil {
		t.Fatal(err)
	}
	// 只印真的有格子的那幾種，而有格子的那幾種一種都不准漏。
	printed := text[anchor:]
	for _, entry := range report.RuntimeStates {
		got := strings.Contains(printed, entry.Title)
		if want := entry.Count > 0; got != want {
			t.Errorf("%s 有 %d 格，印出來 %v", entry.Title, entry.Count, got)
		}
	}
	if report.Misattributed != 1 {
		t.Fatalf("這個機隊有一格量的不是正在跑的那一份，報告說 %d", report.Misattributed)
	}
}

// installCLIMisattributedTables 印一份兩台機器都量錯了檔案的報告，回另外兩張表上
// short 那一列印出來的樣子。
func installCLIMisattributedTables(t *testing.T, running string) (summary, cell string) {
	t.Helper()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	runtime := func(file string) *operator.ToolRuntimeFinding {
		return &operator.ToolRuntimeFinding{
			State:    operator.ToolRuntimeOtherFile,
			Title:    operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile),
			Meaning:  operator.ToolRuntimeMeaning(operator.ToolRuntimeOtherFile),
			NextStep: operator.ToolRuntimeNextStep(operator.ToolRuntimeOtherFile),

			MeasuredFile: installCLILoginCopy, RunningFile: file,
		}
	}
	row := func(name, file string) operator.InstallRow {
		return operator.InstallRow{
			MachineID: name, DisplayName: name, State: operator.InstallMatches,
			Title:    operator.InstallStateTitle(operator.InstallMatches),
			Meaning:  operator.InstallStateMeaning(operator.InstallMatches),
			Assigned: "2026.6.10", Observed: "2026.6.10", Runtime: runtime(file),
		}
	}
	report := operator.InstallReport{
		SchemaVersion: operator.InstallReportSchemaVersion, EvaluatedAt: now,
		Machines: 2, Assigned: 2, Misattributed: 2, Caveat: operator.InstallReportCaveat,
		Resources: []operator.InstallResource{{
			Name: "openclaw", ResourceKind: "openclaw", ResourceID: "openclaw",
			AssignedOn: 2, MatchingOn: 2, MisattributedOn: 2,
			Headline: "openclaw：2/2 台被指派過。",
			Rows: []operator.InstallRow{
				row("short", installCLIReleaseCopy), row("other", running),
			},
		}},
	}
	var out bytes.Buffer
	if err := writeInstallReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Fields(line)
		switch {
		// 資源統計那一列：資源名字後面全是數字。
		case len(fields) == 7 && fields[0] == "openclaw" && fields[1] == "2":
			summary = line
		case strings.HasPrefix(line, "short") && strings.Contains(line, "指派的跟看到的一樣"):
			cell = line
		}
	}
	if summary == "" || cell == "" {
		t.Fatalf("輸出少了那兩列：summary=%q cell=%q\n%s", summary, cell, out.String())
	}
	return summary, cell
}

// ⚠ 路徑沒有長度上限，而這一頁有三張表。它們各走自己的 tabwriter——併成一張的話，
// 一台機器上的一條長路徑會把資源統計那一列與每一台那一列全部推寬，然後整頁要橫向捲。
func TestALongRunningFileOnlyWidensTheTableItIsIn(t *testing.T) {
	shortSummary, shortCell := installCLIMisattributedTables(t, "/opt/openclaw/2026.6.10/openclaw")
	longSummary, longCell := installCLIMisattributedTables(t,
		"/opt/"+strings.Repeat("openclaw-with-a-very-long-release-directory/", 20)+"openclaw")
	if shortSummary != longSummary {
		t.Errorf("長路徑把資源統計那一列推寬了：\n%q\n%q", shortSummary, longSummary)
	}
	if shortCell != longCell {
		t.Errorf("長路徑把每一台那一列推寬了：\n%q\n%q", shortCell, longCell)
	}
}
