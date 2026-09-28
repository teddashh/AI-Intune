package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func softwareCLIFixture(t *testing.T) (string, machineCommandDeps) {
	t.Helper()
	f, now := reportFixture(t)
	// ⚠ reportFixture 那一台有回報過；再開一台從來沒回報過的，因為「沒回報過」
	// 跟「這台上沒有」是這份清查最重要的一個區別。
	if _, _, err := f.store.CreateEnrollTokenFor("never-came", 0); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		CLITools: []model.CLITool{
			{Name: "claude", Present: true, OnPath: true,
				PresentEvidence: "path", VersionReported: "2.1.195"},
			{Name: "gemini", Present: false},
			// ⚠ openclaw 那一格是正式機隊 2026-09-12 的形狀：量版號的是人 PATH 上那一
			// 份，活著的 process 跑的是 release 目錄底下那一份。那件事只有這一軸講得出來。
			{Name: "openclaw", Present: true, OnPath: true,
				PresentEvidence: "path", VersionPackageJSON: "2026.6.10",
				Path: softwareCLIMeasuredFile, RealPath: softwareCLIMeasuredFile,
				PathSource: "login", RunningPID: 9182, RunningScript: softwareCLIRunningFile,
				RunningExe: "/usr/bin/node"},
		},
	}, now); err != nil {
		t.Fatal(err)
	}
	return reportCLIServer(t, f)
}

const (
	softwareCLIMeasuredFile = "/home/example-user/.local/bin/openclaw"
	softwareCLIRunningFile  = "/home/example-user/.local/share/clawctl/openclaw/releases/2026.6.10/bin/openclaw"
)

func runSoftwareCLI(t *testing.T, argv ...string) string {
	t.Helper()
	base, deps := softwareCLIFixture(t)
	var out, errOut bytes.Buffer
	if err := runSoftwareReportCommandWithDeps(t.Context(),
		append([]string{"--hub-url", base}, argv...), &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	return out.String()
}

// CLI 上的那一份要跟畫面回答同一句話：機隊上裝了什麼、各是哪一版、哪幾台沒有。
//
// ⚠⚠ 包含那句限制。terminal 上少了它，一張全綠的表會被讀成「都是最新的」——而這
// 個 Hub 從來不知道上游今天發了什麼。
func TestSoftwareCLIPrintsWhatIsInstalledAndWhatItCannotKnow(t *testing.T) {
	text := runSoftwareCLI(t)
	for _, want := range []string{
		"分母", "回報過", operator.SoftwareReportCaveat,
		"個工具", "工具", "有", "沒有", "沒回報過", "機隊裡最新", "版號",
		"claude", "gemini", "openclaw",
		"2.1.195", "2026.6.10（檔案上讀的）",
		"never-came",
		operator.SoftwareStateTitle(operator.SoftwareUnreported),
		operator.SoftwareStateTitle(operator.SoftwareAbsent),
		operator.SoftwareStateNextStep(operator.SoftwareUnreported),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("輸出少了 %q：\n%s", want, text)
		}
	}
}

// 第一行是操作員看這份清查時唯一不用捲動就看得到的數字，因此要直接釘住分母、
// 回報過的機器數與工具數，而不是只檢查句型。
func TestTheSoftwareCLIFirstLineCountsTheMachinesThatReported(t *testing.T) {
	var report operator.SoftwareReport
	body := runSoftwareCLI(t, "--json")
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("--json 不是合法 JSON：%v\n%s", err, body)
	}
	if report.Reporting >= report.Machines {
		t.Fatalf("測試資料沒有造出完全沉默的機器：分母 %d 台、回報過 %d 台",
			report.Machines, report.Reporting)
	}

	text := runSoftwareCLI(t)
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	wantHeadline := fmt.Sprintf("分母 %d 台，%d 台回報過；%d 個工具",
		report.Machines, report.Reporting, len(report.Tools))
	if len(lines) == 0 || !strings.HasPrefix(lines[0], wantHeadline) {
		t.Fatalf("第一行是 %q，期望以前綴 %q 開頭", lines[0], wantHeadline)
	}
	wantNextStep := fmt.Sprintf("有 %d 台", report.Machines-report.Reporting)
	if !strings.Contains(lines[len(lines)-1], wantNextStep) {
		t.Fatalf("最後一行是 %q，期望包含 %q", lines[len(lines)-1], wantNextStep)
	}
}

// ⚠⚠ 錯歸因那一段印在工具那張表前面，而且兩個檔案都要印出來。後面每一句「機隊裡最新
// 是 X，這一台是 Y」都假設那個版號講的是機隊上真的在跑的東西。
func TestSoftwareCLILeadsWithTheVersionThatIsNotRunning(t *testing.T) {
	text := runSoftwareCLI(t)
	head := strings.Index(text, "格的版號講的不是正在跑的那一份")
	tools := strings.Index(text, "個工具\n")
	if head < 0 || tools < 0 || head > tools {
		t.Fatalf("錯歸因那一段不在工具那張表前面：head=%d tools=%d\n%s", head, tools, text)
	}
	section := text[head:tools]
	for _, want := range []string{
		"openclaw", softwareCLIMeasuredFile, softwareCLIRunningFile,
		operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile),
		operator.ToolRuntimeNextStep(operator.ToolRuntimeOtherFile),
	} {
		if !strings.Contains(section, want) {
			t.Errorf("那一段少了 %q：\n%s", want, section)
		}
	}
	// ⚠ 一個解譯器不是第二份安裝。/usr/bin/node 出現在這一段裡，等於叫人去收掉一份
	// 不存在的安裝。
	if strings.Contains(section, "/usr/bin/node") {
		t.Errorf("解譯器被算成第二份安裝：\n%s", section)
	}
}

// ⚠ 逐台那張表上只放那一句標題，不放路徑。對齊欄位裡放一個沒有長度上限的路徑，會把
// 每一台的每一列都推寬——包含那些不必看的列。
func TestSoftwareCLIKeepsThePathsOutOfThePerMachineTable(t *testing.T) {
	text := runSoftwareCLI(t)
	head := strings.Index(text, "openclaw：")
	if head < 0 {
		t.Fatalf("輸出沒有 openclaw 那一段：\n%s", text)
	}
	section := text[head:]
	if !strings.Contains(section, operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile)) {
		t.Errorf("逐台那張表沒有那一軸：\n%s", section)
	}
	if strings.Contains(section, softwareCLIRunningFile) {
		t.Errorf("路徑進了逐台那張表的對齊欄位：\n%s", section)
	}
	// ⚠ 沒有這一軸的那一格留白，而不是「沒有找到在跑它的 process」——那台從來沒回報過
	// 這個工具，所以沒有版號，也就沒有「這個版號講的是哪一份」可問。把它講成「沒找到
	// process」，等於憑一份沒收到的觀測宣布那台上沒有東西在跑。
	silent := ""
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "never-came") {
			silent = line
		}
	}
	if silent == "" {
		t.Fatalf("openclaw 那一段裡沒有 never-came 那一列：\n%s", section)
	}
	for _, stateValue := range operator.ToolRuntimes() {
		if strings.Contains(silent, operator.ToolRuntimeTitle(stateValue)) {
			t.Errorf("沒回報過的那一列被講成 %q：%q",
				operator.ToolRuntimeTitle(stateValue), silent)
		}
	}
	if !strings.Contains(silent, "—") {
		t.Errorf("沒回報過的那一列沒有留白：%q", silent)
	}
}

// softwareCLIMisattributedRow 印一份兩格錯歸因的報告，回 short 那一列印出來的樣子。
func softwareCLIMisattributedRow(t *testing.T, otherNextStep string) string {
	t.Helper()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	row := func(name, nextStep string) operator.SoftwareRow {
		finding := operator.ToolRuntimeFinding{
			State:        operator.ToolRuntimeOtherFile,
			Title:        operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile),
			Meaning:      operator.ToolRuntimeMeaning(operator.ToolRuntimeOtherFile),
			NextStep:     nextStep,
			MeasuredFile: softwareCLIMeasuredFile, RunningFile: softwareCLIRunningFile,
		}
		return operator.SoftwareRow{
			MachineID: name, DisplayName: name, State: operator.SoftwareNewest,
			Title:   operator.SoftwareStateTitle(operator.SoftwareNewest),
			Meaning: operator.SoftwareStateMeaning(operator.SoftwareNewest),
			Version: "2026.6.10", Runtime: &finding,
		}
	}
	report := operator.SoftwareReport{
		SchemaVersion: operator.SoftwareReportSchemaVersion, EvaluatedAt: now,
		Machines: 2, Reporting: 2, Misattributed: 2, Caveat: operator.SoftwareReportCaveat,
		Tools: []operator.SoftwareTool{{
			Name: "openclaw", InstalledOn: 2, Newest: "2026.6.10", Spread: 1,
			Versions: []operator.SoftwareVersion{{Version: "2026.6.10", Machines: 2, Newest: true}},
			Headline: "openclaw：2/2 台上有。",
			Rows:     []operator.SoftwareRow{row("short", "收掉一份。"), row("other", otherNextStep)},
		}},
	}
	var out bytes.Buffer
	if err := writeSoftwareReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "openclaw  short") && strings.Contains(line, "收掉一份。") {
			return line
		}
	}
	t.Fatalf("輸出沒有 short 那一列：\n%s", out.String())
	return ""
}

// 錯歸因那張表上的「下一步」同樣沒有長度上限，而且它跟前兩張表走的是第三張表。
func TestTheSoftwareRuntimeNextStepDoesNotWidenTheAlignedColumns(t *testing.T) {
	short := softwareCLIMisattributedRow(t, "收掉一份。")
	long := softwareCLIMisattributedRow(t,
		strings.Repeat("決定要留哪一份，再把另一份收掉；這個 Hub 不知道哪一份是它自己放的。", 40))
	if short != long {
		t.Fatalf("另一列的長下一步把這一列推寬了：\n%q\n%q", short, long)
	}
}

func TestSoftwareCLIEmitsTheSameDocumentAsTheAPI(t *testing.T) {
	var report operator.SoftwareReport
	body := runSoftwareCLI(t, "--json")
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("--json 不是合法 JSON：%v\n%s", err, body)
	}
	if report.SchemaVersion != operator.SoftwareReportSchemaVersion ||
		report.Caveat != operator.SoftwareReportCaveat ||
		report.Reporting > report.Machines || len(report.Tools) == 0 {
		t.Fatalf("report=%+v", report)
	}
	for _, tool := range report.Tools {
		if total := tool.InstalledOn + tool.AbsentOn + tool.UnreportedOn; total != report.Machines {
			t.Fatalf("%s 的 %d 台，分母是 %d 台", tool.Name, total, report.Machines)
		}
		if len(tool.Rows) != report.Machines {
			t.Fatalf("%s 有 %d 列，分母是 %d 台", tool.Name, len(tool.Rows), report.Machines)
		}
	}
}

func TestSoftwareCLIExportsExactlyTheCellsItPrints(t *testing.T) {
	rows := reportCLIExportRows(t, runSoftwareCLI(t, "--csv"))
	var report operator.SoftwareReport
	if err := json.Unmarshal([]byte(runSoftwareCLI(t, "--json")), &report); err != nil {
		t.Fatal(err)
	}
	cells := 0
	for _, tool := range report.Tools {
		cells += len(tool.Rows)
	}
	if len(rows) != cells+1 {
		t.Fatalf("匯出 %d 列（含表頭），報告有 %d 格", len(rows), cells)
	}
	columns := operator.ReportCSVColumns(operator.SoftwareReportCSV(report))
	if len(rows[0]) != len(columns) {
		t.Fatalf("表頭 %d 欄，文件說 %d 欄", len(rows[0]), len(columns))
	}
	for index, column := range columns {
		if rows[0][index] != column.Header {
			t.Errorf("第 %d 欄是 %q，文件說 %q", index, rows[0][index], column.Header)
		}
	}
}

func TestSoftwareCLIRefusesWhatItCannotRun(t *testing.T) {
	base, deps := softwareCLIFixture(t)
	for name, argv := range map[string][]string{
		"positional 參數":     {"--hub-url", base, "claude"},
		"認不得的 flag":         {"--hub-url", base, "--tool", "claude"},
		"--json 與 --csv 並用": {"--hub-url", base, "--json", "--csv"},
		"hub-url 裡有控制字元":    {"--hub-url", base + "\x00"},
	} {
		var out, errOut bytes.Buffer
		if err := runSoftwareReportCommandWithDeps(t.Context(), argv,
			&out, &errOut, deps); err == nil {
			t.Errorf("%s：被接受了\n%s", name, out.String())
		}
	}
}

// softwareCLIToolRow 印一份兩個工具的報告，回 short 那一列印出來的樣子。
func softwareCLIToolRow(t *testing.T, otherNextStep string) string {
	t.Helper()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	tool := func(name, nextStep string) operator.SoftwareTool {
		return operator.SoftwareTool{
			Name: name, InstalledOn: 1, Newest: "1.0.0", Spread: 1,
			Versions: []operator.SoftwareVersion{{Version: "1.0.0", Machines: 1, Newest: true}},
			Rows: []operator.SoftwareRow{{
				MachineID: "m1", DisplayName: "samplehub1", State: operator.SoftwareNewest,
				Title:   operator.SoftwareStateTitle(operator.SoftwareNewest),
				Meaning: operator.SoftwareStateMeaning(operator.SoftwareNewest),
				Version: "1.0.0",
			}},
			Headline: name + "：1/1 台上有。", NextStep: nextStep,
		}
	}
	report := operator.SoftwareReport{
		SchemaVersion: operator.SoftwareReportSchemaVersion, EvaluatedAt: now,
		Machines: 1, Reporting: 1, Caveat: operator.SoftwareReportCaveat,
		Tools: []operator.SoftwareTool{tool("short", "升上去就好。"), tool("other", otherNextStep)},
	}
	var out bytes.Buffer
	if err := writeSoftwareReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "short") && strings.Contains(line, "升上去就好。") {
			return line
		}
	}
	t.Fatalf("輸出沒有 short 那一列：\n%s", out.String())
	return ""
}

// 「下一步」沒有長度上限。它一旦進了對齊的欄位，一個工具的長句子就會把每一列都
// 推寬——包含那些跟它無關的列。
func TestTheSoftwareNextStepDoesNotWidenTheAlignedColumns(t *testing.T) {
	short := softwareCLIToolRow(t, "升上去就好。")
	long := softwareCLIToolRow(t,
		strings.Repeat("要拉齊就把落後的那幾台升到 2026.6.10；這個 Hub 不知道上游今天發了什麼。", 40))
	if short != long {
		t.Fatalf("另一列的長下一步把這一列推寬了：\n%q\n%q", short, long)
	}
}

// softwareCLICellRow 印一份兩台機器的報告，回 short 那一台印出來的樣子。
func softwareCLICellRow(t *testing.T, otherNextStep string) string {
	t.Helper()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	row := func(name, nextStep string) operator.SoftwareRow {
		return operator.SoftwareRow{
			MachineID: name, DisplayName: name, State: operator.SoftwareAbsent,
			Title:    operator.SoftwareStateTitle(operator.SoftwareAbsent),
			Meaning:  operator.SoftwareStateMeaning(operator.SoftwareAbsent),
			NextStep: nextStep,
		}
	}
	report := operator.SoftwareReport{
		SchemaVersion: operator.SoftwareReportSchemaVersion, EvaluatedAt: now,
		Machines: 2, Reporting: 2, Caveat: operator.SoftwareReportCaveat,
		Tools: []operator.SoftwareTool{{
			Name: "claude", AbsentOn: 2, Headline: "claude：2 台上都沒有。",
			Rows: []operator.SoftwareRow{row("short", "裝上去。"), row("other", otherNextStep)},
		}},
	}
	var out bytes.Buffer
	if err := writeSoftwareReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "short") && strings.Contains(line, "裝上去。") {
			return line
		}
	}
	t.Fatalf("輸出沒有 short 那一列：\n%s", out.String())
	return ""
}

// 每一格自己的下一步也沒有長度上限，而且它跟工具那一層走的是不同的表。
func TestTheSoftwareCellNextStepDoesNotWidenTheAlignedColumns(t *testing.T) {
	short := softwareCLICellRow(t, "裝上去。")
	long := softwareCLICellRow(t,
		strings.Repeat("要用就在這台上安裝；這個 Hub 只知道它現在沒有，不知道它該不該有。", 40))
	if short != long {
		t.Fatalf("另一列的長下一步把這一列推寬了：\n%q\n%q", short, long)
	}
}
