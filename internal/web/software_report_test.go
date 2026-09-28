package web

import (
	"encoding/csv"
	"html"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// softwarePageFleet 造出一台最新、一台落後、一台一筆觀測都沒有。
func softwarePageFleet(t *testing.T, st *store.Store) (newest, behind, silent string) {
	t.Helper()
	now := time.Now().UTC()
	newest = enroll(t, st, "samplehub1", now.Add(-time.Hour))
	behind = enroll(t, st, "sampleagent2", now.Add(-time.Hour))
	// ⚠ sampleagent4 刻意一筆觀測都沒有：它在分母裡，而它每一格都必須是「沒回報過」。
	silent = enroll(t, st, "sampleagent4", now.Add(-time.Hour))
	observe := func(id, version string) {
		t.Helper()
		if err := st.RecordObservation(id, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-2 * time.Minute),
			CLITools: []model.CLITool{
				{Name: "claude", Present: true, OnPath: true,
					PresentEvidence: "path", VersionReported: version},
				{Name: "gemini", Present: false},
			},
		}, now.Add(-time.Minute)); err != nil {
			t.Fatalf("記觀測: %v", err)
		}
	}
	observe(newest, "2.1.195")
	observe(behind, "2.1.100")
	return newest, behind, silent
}

// softwarePageRunning 再加一台，形狀就是正式機隊 2026-09-12 的 openclaw：量版號的是人
// PATH 上那一份，活著的 process 跑的是 release 目錄底下那一份，而且 shadowed 是 false。
//
// ⚠ gemini 那一格刻意只有 running_exe=/usr/bin/node：那個欄位對 node CLI 一律是解譯器，
// 它對「跑的是哪一份」一點資訊都沒有。拿它去跟安裝路徑比，會憑一個解譯器的路徑宣布這台
// 上有第二份安裝。
func softwarePageRunning(t *testing.T, st *store.Store) (id, measured, running string) {
	t.Helper()
	now := time.Now().UTC()
	measured = "/home/example-user/.local/bin/claude"
	running = "/home/example-user/.local/share/clawctl/claude/releases/2.1.195/bin/claude"
	id = enroll(t, st, "sampleagent3", now.Add(-time.Hour))
	if err := st.RecordObservation(id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-2 * time.Minute),
		CLITools: []model.CLITool{
			{Name: "claude", Present: true, OnPath: true, PresentEvidence: "path",
				VersionReported: "2.1.195", Path: measured, RealPath: measured,
				PathSource: "login", RunningPID: 4242, RunningScript: running,
				RunningExe: "/usr/bin/node"},
			{Name: "gemini", Present: true, OnPath: true, PresentEvidence: "path",
				VersionReported: "0.9.0", Path: "/home/example-user/.local/bin/gemini",
				RealPath: "/home/example-user/.local/bin/gemini", PathSource: "login",
				RunningPID: 4243, RunningExe: "/usr/bin/node"},
		},
	}, now.Add(-time.Minute)); err != nil {
		t.Fatalf("記觀測: %v", err)
	}
	return id, measured, running
}

// ⚠⚠ 那一節排在版號不一致前面，而且要把兩個檔案都印出來。這一頁其他每一個「誰比較新」
// 都假設那個版號講的是機隊上真的在跑的東西。
func TestTheSoftwarePageLeadsWithTheVersionThatIsNotRunning(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	_, measured, running := softwarePageRunning(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/software").Body.String())
	misattributed := strings.Index(body, `id="misattributed"`)
	drifted := strings.Index(body, `id="drifted"`)
	if misattributed < 0 || drifted < 0 || misattributed > drifted {
		t.Fatalf("錯歸因那一節不在版號不一致前面：misattributed=%d drifted=%d",
			misattributed, drifted)
	}
	section := body[misattributed:drifted]
	for _, want := range []string{
		"sampleagent3", "claude", measured, running,
		operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile),
		operator.ToolRuntimeNextStep(operator.ToolRuntimeOtherFile),
	} {
		if !strings.Contains(section, want) {
			t.Errorf("那一節少了 %q：\n%s", want, section)
		}
	}
}

// ⚠ 一個解譯器不是第二份安裝。/usr/bin/node 被講成「正在跑的是另一個檔案」的話，畫面
// 會叫人去收掉一份不存在的安裝。
func TestTheSoftwarePageNeverCallsAnInterpreterASecondInstall(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	softwarePageRunning(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/software").Body.String())
	misattributed := strings.Index(body, `id="misattributed"`)
	drifted := strings.Index(body, `id="drifted"`)
	if misattributed < 0 || drifted < 0 {
		t.Fatalf("錯歸因那一節不見了")
	}
	if section := body[misattributed:drifted]; strings.Contains(section, "/usr/bin/node") {
		t.Errorf("解譯器被算成第二份安裝：\n%s", section)
	}
	report, err := s.operator.SoftwareReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range report.Tools {
		if tool.Name != "gemini" {
			continue
		}
		for _, row := range tool.Rows {
			if row.DisplayName != "sampleagent3" {
				continue
			}
			if row.Runtime == nil || row.Runtime.State != operator.ToolRuntimeUnattributed {
				t.Fatalf("gemini × sampleagent3 的那一軸是 %+v", row.Runtime)
			}
			if row.Runtime.RunningFile != "" {
				t.Errorf("說不出跑的是哪一個檔案，卻給了 %q", row.Runtime.RunningFile)
			}
		}
	}
}

// ⚠⚠ 這一頁的空狀態講的是「沒有一格的版號量的是沒在跑的那一份」，不是「每一格量的
// 都是正在跑的那一份」。機隊上大多數的格子根本沒有找到在跑它的 process——後面那句話
// 會把一堆「不知道」講成「確認過了」。
func TestTheSoftwarePageDoesNotTurnSilenceIntoConfirmation(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/software").Body.String())
	if !strings.Contains(body, "沒有一格的版號量的是沒在跑的那一份") {
		t.Errorf("空狀態那一句不在畫面上")
	}
	if strings.Contains(body, "每一格的版號量的都是正在跑的那一份") {
		t.Errorf("畫面把沉默講成確認過了")
	}
	report, err := s.operator.SoftwareReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.Misattributed != 0 {
		t.Fatalf("fixture 造出了錯歸因，這支測試沒有在測空狀態：%d", report.Misattributed)
	}
	for _, tool := range report.Tools {
		for _, row := range tool.Rows {
			if row.Runtime != nil && row.Runtime.State == operator.ToolRuntimeSameFile {
				t.Fatalf("%s × %s 被講成量的就是跑的", tool.Name, row.DisplayName)
			}
		}
	}
}

// ⚠⚠ 總覽是最多人看的那一頁。一張「工具版本一致」的表講的是一批沒有人在跑的檔案時，
// 那一頁就變成了一個讓人放心的謊。
func TestTheDashboardDoesNotCallTheFleetConsistentWhenTheVersionIsNotRunning(t *testing.T) {
	s, st := newServer(t)
	softwarePageRunning(t, st)
	report, err := s.operator.SoftwareReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.Drifted != 0 || report.Misattributed == 0 {
		t.Fatalf("fixture 要的是「版號一致但有錯歸因」：drifted=%d misattributed=%d",
			report.Drifted, report.Misattributed)
	}
	body := html.UnescapeString(webRequest(t, s, "/").Body.String())
	if !strings.Contains(body, "版號不是跑的那一份") {
		t.Errorf("總覽上那張表沒有標出那一格")
	}
	if !strings.Contains(body, strconv.Itoa(report.Misattributed)+" 格的版號量的是沒在跑的那一份") {
		t.Errorf("總覽的結論那一句沒有講出那幾格")
	}
}

func TestTheSoftwarePageLeadsWithTheToolsThatDisagree(t *testing.T) {
	s, st := newServer(t)
	_, behind, _ := softwarePageFleet(t, st)
	rec := webRequest(t, s, "/reports/software")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := html.UnescapeString(rec.Body.String())
	drifted := strings.Index(body, `id="drifted"`)
	missing := strings.Index(body, `id="missing"`)
	matrix := strings.Index(body, `id="matrix"`)
	if drifted < 0 || missing < 0 || matrix < 0 || drifted > missing || missing > matrix {
		t.Fatalf("三節的順序不對：drifted=%d missing=%d matrix=%d", drifted, missing, matrix)
	}
	section := body[drifted:missing]
	if !strings.Contains(section, "claude") {
		t.Errorf("版號不一致的工具不在那一節裡：\n%s", section)
	}
	// gemini 三台都沒裝，版號沒有不一致，不該混進這一節。
	if strings.Contains(section, "gemini") {
		t.Error("每一台都沒裝的工具被列進「版號不一致」")
	}
	// 反過來也要成立：「還沒回報過」那一節講的是沉默，不是版號差距。
	if silence := body[missing:matrix]; !strings.Contains(silence, "gemini") {
		t.Errorf("有機器沒回報過的工具不在「還沒回報過」那一節：\n%s", silence)
	}
	if !strings.Contains(body, "/machines/"+behind) {
		t.Error("落後的機器沒有連到它自己那一頁")
	}
}

// ⚠⚠ 這一頁沒有上游的版本來源。那句限制必須在畫面上，不是只在程式碼的註解裡：
// 一張全綠的表沒有那句話會被讀成「都是最新的」。
func TestTheSoftwarePageSaysWhatItCannotKnow(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/software").Body.String())
	if !strings.Contains(body, operator.SoftwareReportCaveat) {
		t.Errorf("這一頁沒有講出它講得出口的極限：\n%s", body)
	}
	dashboard := html.UnescapeString(webRequest(t, s, "/").Body.String())
	if !strings.Contains(dashboard, operator.SoftwareReportCaveat) {
		t.Errorf("總覽上那張表沒有帶著同一句限制：\n%s", dashboard)
	}
}

// ⚠ 「沒回報過」跟「這台上沒有」下一步完全不同，畫面上要看得出是兩件事。
func TestTheSoftwarePageKeepsSilenceApartFromAbsence(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/software").Body.String())
	for _, want := range []string{
		operator.SoftwareStateTitle(operator.SoftwareUnreported),
		operator.SoftwareStateTitle(operator.SoftwareAbsent),
		operator.SoftwareStateNextStep(operator.SoftwareUnreported),
		operator.SoftwareStateNextStep(operator.SoftwareAbsent),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("畫面上沒有 %q：\n%s", want, body)
		}
	}
	if !strings.Contains(body, "「沒回報過」跟「這台上沒有」是兩件事") {
		t.Error("這一頁沒有講出沉默與「沒有」的差別")
	}
}

// 摘要卡上的數字必須跟下面的表數得出來的一樣。
func TestTheSoftwareSummaryMatchesTheRowsBelowIt(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/software").Body.String())
	report, err := s.operator.SoftwareReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.Machines == 0 || report.Drifted == 0 || report.Reporting == report.Machines {
		t.Fatalf("fixture 沒有造出該有的狀態：%+v", report)
	}
	if !strings.Contains(body, metricValue(report.Machines)) {
		t.Errorf("摘要卡上沒有分母 %d", report.Machines)
	}
	if want := metricValue(report.Drifted); !strings.Contains(body, want) {
		t.Errorf("摘要卡上沒有版號不一致的工具數 %d", report.Drifted)
	}
	if want := strconv.Itoa(report.Reporting) + " / " + strconv.Itoa(report.Machines); !strings.Contains(body, want) {
		t.Errorf("摘要卡上沒有 %q", want)
	}
	for _, tool := range report.Tools {
		for _, row := range tool.Rows {
			if !strings.Contains(body, row.DisplayName) {
				t.Errorf("%s 這個工具的表上少了 %s", tool.Name, row.DisplayName)
			}
		}
	}
}

// ⚠⚠ 總覽上那張表跟這一頁讀的必須是同一份報告。兩邊各算一次的話，漂掉的樣子是
// 同一台機器在兩頁上有兩個版本答案，而那種分岔沒有人會發現。
func TestTheDashboardMatrixAndTheReportAgreeOnEveryCell(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	softwarePageRunning(t, st)
	report, err := s.operator.SoftwareReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	matrix := s.softwareMatrix(time.Now().UTC())
	if len(matrix.Rows) != len(report.Tools) {
		t.Fatalf("總覽列了 %d 個工具，報告有 %d 個", len(matrix.Rows), len(report.Tools))
	}
	if len(matrix.Labels) != report.Machines {
		t.Fatalf("總覽有 %d 欄，分母是 %d 台", len(matrix.Labels), report.Machines)
	}
	if matrix.Misattributed != report.Misattributed || matrix.Misattributed == 0 {
		t.Fatalf("總覽說 %d 格錯歸因，報告說 %d 格", matrix.Misattributed, report.Misattributed)
	}
	for i, row := range matrix.Rows {
		tool := report.Tools[i]
		if row.Name != tool.Name || row.Newest != tool.Newest || row.Spread != tool.Spread {
			t.Errorf("第 %d 列：總覽說 %+v，報告說 %s/%s/%d",
				i, row, tool.Name, tool.Newest, tool.Spread)
		}
		if len(row.Cells) != len(matrix.Labels) {
			t.Fatalf("%s 有 %d 格，表頭有 %d 欄", row.Name, len(row.Cells), len(matrix.Labels))
		}
		for j, cell := range row.Cells {
			// ⚠ 第 j 格永遠是表頭第 j 欄那一台。一張對不上欄位的表等於沒有。
			if cell.DisplayName != matrix.Labels[j] {
				t.Errorf("%s 第 %d 格是 %s，表頭那一欄是 %s",
					row.Name, j, cell.DisplayName, matrix.Labels[j])
			}
			if cell.State != tool.Rows[j].State || cell.Version != tool.Rows[j].Version {
				t.Errorf("%s/%s：總覽 %s %q，報告 %s %q", row.Name, cell.DisplayName,
					cell.State, cell.Version, tool.Rows[j].State, tool.Rows[j].Version)
			}
			// ⚠ 第二軸也要對得上。總覽上少標一格「版號不是跑的那一份」，等於那一頁
			// 上有一個版號替另一份安裝發言。
			if softwareCellRuntime(cell) != softwareCellRuntime(tool.Rows[j]) {
				t.Errorf("%s/%s：總覽那一軸是 %q，報告是 %q", row.Name, cell.DisplayName,
					softwareCellRuntime(cell), softwareCellRuntime(tool.Rows[j]))
			}
		}
	}
}

// softwareCellRuntime 把一格的第二軸壓成一個可以比的字串。沒有那一軸跟落在某一種
// 狀態上是兩件事，所以留白有自己的寫法。
func softwareCellRuntime(row operator.SoftwareRow) string {
	if row.Runtime == nil {
		return "（沒有這一軸）"
	}
	return string(row.Runtime.State) + " " + row.Runtime.MeasuredFile + " " + row.Runtime.RunningFile
}

func TestTheSoftwareCSVExportsOneRowPerMachinePerTool(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	rec := webRequest(t, s, "/reports/software.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("content-type=%q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "software") {
		t.Errorf("content-disposition=%q", got)
	}
	rows, err := csv.NewReader(strings.NewReader(
		strings.TrimPrefix(rec.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.operator.SoftwareReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	cells := 0
	for _, tool := range report.Tools {
		cells += len(tool.Rows)
	}
	if cells == 0 {
		t.Fatal("fixture 一格都沒有，這支測試沒有在測東西")
	}
	if len(rows) != cells+1 {
		t.Fatalf("CSV %d 列（含表頭），畫面上有 %d 格", len(rows), cells)
	}
	// ⚠⚠ 最後一欄是那句限制，而且每一列都帶著它。一列「比機隊裡最新的舊」離開
	// Hub 之後，讀它的人會以為那是「該升級了」——而這個 Hub 沒有上游版本來源，
	// 從來沒有資格講那句話。
	if rows[0][0] != "工具" || rows[0][len(rows[0])-1] != "這份報告的限制" {
		t.Fatalf("表頭=%v", rows[0])
	}
	for index, row := range rows[1:] {
		if row[len(row)-1] != operator.SoftwareReportCaveat {
			t.Fatalf("第 %d 列沒有帶那句限制：%v", index+1, row)
		}
	}
	// 匯出的檔案要答得出「哪一台落後」——把版本分佈壓成一格就答不出來了。
	found := false
	for _, row := range rows[1:] {
		if row[0] == "claude" && row[1] == "sampleagent2" {
			found = true
			if row[4] != "2.1.100" {
				t.Errorf("sampleagent2 那一列的版號是 %q", row[4])
			}
			if row[3] != operator.SoftwareStateTitle(operator.SoftwareBehind) {
				t.Errorf("sampleagent2 那一列是 %q，該是落後", row[3])
			}
		}
	}
	if !found {
		t.Errorf("CSV 裡沒有 claude × sampleagent2 那一列：%v", rows)
	}
}

// ⚠⚠ 匯出的那一列要自己講得出「這個版號是從哪一個檔案量的」。一份只有版號的 CSV 離開
// Hub 之後，讀它的人沒有辦法知道那個版號講的是機隊上在跑的那一份，還是旁邊那一份。
func TestTheSoftwareCSVSaysWhichFileEachVersionCameFrom(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	_, measured, running := softwarePageRunning(t, st)
	rec := webRequest(t, s, "/reports/software.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(
		strings.TrimPrefix(rec.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for index, header := range map[int]string{
		7: "版號講的是哪一份", 8: "量版號的那個檔案", 9: "正在跑的那個檔案",
	} {
		if rows[0][index] != header {
			t.Fatalf("第 %d 欄是 %q，該是 %q", index, rows[0][index], header)
		}
	}
	wanted := map[string][]string{
		// 那一台有兩份，而版號量的是沒在跑的那一份。
		"sampleagent3": {operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile), measured, running},
		// sampleagent4 一筆觀測都沒有，所以沒有版號，也就沒有這一軸——三格留白。
		"sampleagent4": {"", "", ""},
	}
	seen := map[string]bool{}
	for _, row := range rows[1:] {
		if row[0] != "claude" {
			continue
		}
		want, ok := wanted[row[1]]
		if !ok {
			continue
		}
		seen[row[1]] = true
		if got := row[7:10]; got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("claude × %s 的那三欄是 %v，該是 %v", row[1], got, want)
		}
	}
	for machine := range wanted {
		if !seen[machine] {
			t.Errorf("CSV 裡沒有 claude × %s 那一列", machine)
		}
	}
}

// 這份報告只講「現在」，沒有範圍好挑。
func TestTheSoftwarePageRejectsAQueryItCannotHonour(t *testing.T) {
	s, st := newServer(t)
	softwarePageFleet(t, st)
	for _, target := range []string{
		"/reports/software?days=7", "/reports/software.csv?tool=claude",
	} {
		if rec := webRequest(t, s, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d，應該拒絕", target, rec.Code)
		}
	}
}
