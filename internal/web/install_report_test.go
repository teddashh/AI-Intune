package web

import (
	"encoding/csv"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// 正式庫 2026-09-12 量到的那幾個檔案。
const (
	installPageLoginCopy   = "/usr/local/bin/openclaw"
	installPageReleaseCopy = "/home/example-user/.local/share/clawctl/releases/2026.6.6/openclaw"
	installPageUserCopy    = "/home/ubuntu/.local/bin/openclaw"
)

// installPageFleet 造出這一頁真正要講的三種形狀：一台指派的跟看到的一樣、一台
// 指派的比看到的舊（正式環境上 samplehub1 就是這樣），一台從來沒有被指派過。
//
// ⚠ 前兩台看到的版號都量在一個沒有人在跑的檔案上——samplehub1 在跑的是 releases/ 那一
// 份，sampleagent2 在跑的那個檔案已經被 unlink 了。這一頁最要緊的一句話就是這件事。
func installPageFleet(t *testing.T, st *store.Store) (matching, older, unassigned string) {
	t.Helper()
	now := time.Now().UTC()
	matching = enroll(t, st, "sampleagent2", now.Add(-time.Hour))
	older = enroll(t, st, "samplehub1", now.Add(-time.Hour))
	// ⚠ sampleagent4 一列意圖都沒有：它在分母裡，而它那一格必須是「沒有被指派過」。
	unassigned = enroll(t, st, "sampleagent4", now.Add(-time.Hour))

	observe := func(id, version string, tool model.CLITool) {
		t.Helper()
		tool.Name, tool.Present, tool.OnPath = "openclaw", true, true
		tool.PresentEvidence, tool.VersionReported = "path", version
		if err := st.RecordObservation(id, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-2 * time.Minute),
			CLITools: []model.CLITool{tool},
		}, now.Add(-time.Minute)); err != nil {
			t.Fatalf("記觀測: %v", err)
		}
	}
	observe(matching, "2026.5.20", model.CLITool{
		Path: installPageUserCopy, RealPath: installPageUserCopy,
		RunningPID: 2210, RunningScript: installPageUserCopy + ".1787036247195252617.old (deleted)"})
	observe(older, "2026.6.6", model.CLITool{
		Path: installPageLoginCopy, RealPath: installPageLoginCopy,
		RunningPID: 4131, RunningScript: installPageReleaseCopy})

	assign := func(rev int64, machineID, version string) {
		t.Helper()
		spec := `{"kind":"openclaw","version":"` + version + `"}`
		if _, err := st.DB().Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES(?,'machine',?,'openclaw','openclaw',?,?,?,'operator@test')`,
			"desired-"+version+"-"+machineID[:6], machineID, rev, spec,
			now.Add(-24*time.Hour).Format(time.RFC3339)); err != nil {
			t.Fatalf("寫意圖: %v", err)
		}
	}
	assign(1, matching, "2026.5.20")
	assign(2, older, "2026.5.26")
	return matching, older, unassigned
}

func TestTheInstallPageLeadsWithWhatDisagrees(t *testing.T) {
	s, st := newServer(t)
	_, older, _ := installPageFleet(t, st)
	rec := webRequest(t, s, "/reports/install")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := html.UnescapeString(rec.Body.String())
	differing := strings.Index(body, `id="differing"`)
	unassigned := strings.Index(body, `id="unassigned"`)
	matrix := strings.Index(body, `id="matrix"`)
	if differing < 0 || unassigned < 0 || matrix < 0 || differing > unassigned || unassigned > matrix {
		t.Fatalf("三節的順序不對：differing=%d unassigned=%d matrix=%d",
			differing, unassigned, matrix)
	}
	section := body[differing:unassigned]
	for _, want := range []string{"samplehub1", "2026.5.26", "2026.6.6"} {
		if !strings.Contains(section, want) {
			t.Errorf("不一樣那一節少了 %q：\n%s", want, section)
		}
	}
	// 對得上的那一台沒有事情要做，不該混進這一節。
	if strings.Contains(section, "sampleagent2") {
		t.Error("指派的跟看到的一樣的機器被列進「不一樣」")
	}
	// 反過來也要成立：沒有被指派過的那幾台在它們自己那一節。
	if silence := body[unassigned:matrix]; !strings.Contains(silence, "沒有被指派過") {
		t.Errorf("沒有被指派過的那一節沒有講出來：\n%s", silence)
	}
	if !strings.Contains(body, "/machines/"+older) {
		t.Error("不一樣的那台機器沒有連到它自己那一頁")
	}
}

// ⚠⚠ 這一頁不准把觀測寫成判決。samplehub1 被指派 2026.5.26、實際跑 2026.6.6，
// 那既不是落後也不是裝失敗。
func TestTheInstallPageNeverCallsAnOlderAssignmentAFailure(t *testing.T) {
	s, st := newServer(t)
	installPageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/install").Body.String())
	for _, banned := range []string{"落後", "裝失敗", "安裝失敗", "裝不起來"} {
		if strings.Contains(body, banned) {
			t.Errorf("畫面上出現了判決字眼 %q", banned)
		}
	}
	if !strings.Contains(body, operator.InstallReportCaveat) {
		t.Errorf("這一頁沒有帶著它自己的極限：\n%s", body)
	}
	if !strings.Contains(body, "不是「從 A 升到 B」") {
		t.Error("那張表沒有講清楚「A → B」怎麼讀")
	}
}

// 沒有被指派過必須在畫面上是一個講得出口的狀態，不是一格留白。
func TestTheInstallPageSaysNeverAssignedInsteadOfLeavingABlank(t *testing.T) {
	s, st := newServer(t)
	installPageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/install").Body.String())
	matrix := strings.Index(body, `id="matrix"`)
	states := strings.Index(body, `id="states"`)
	if matrix < 0 || states < 0 || matrix > states {
		t.Fatalf("兩節的順序不對：matrix=%d states=%d", matrix, states)
	}
	if cell := body[matrix:states]; !strings.Contains(cell, "沒有指派") {
		t.Errorf("那張表上沒有被指派過的那一格是留白的：\n%s", cell)
	}
	if !strings.Contains(body[states:], operator.InstallStateTitle(operator.InstallUnassigned)) {
		t.Error("狀態表裡沒有「沒有被指派過」")
	}
}

func TestTheInstallCSVExportsOneRowPerMachinePerResourceOverHTTP(t *testing.T) {
	s, st := newServer(t)
	installPageFleet(t, st)
	rec := webRequest(t, s, "/reports/install.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("content-type=%q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "install") {
		t.Errorf("content-disposition=%q", got)
	}
	rows, err := csv.NewReader(strings.NewReader(
		strings.TrimPrefix(rec.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.operator.InstallReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	cells := 0
	for _, resource := range report.Resources {
		cells += len(resource.Rows)
	}
	if cells == 0 {
		t.Fatal("fixture 一格都沒有，這支測試沒有在測東西")
	}
	if len(rows) != cells+1 {
		t.Fatalf("CSV %d 列（含表頭），畫面上有 %d 格", len(rows), cells)
	}
	// ⚠⚠ 最後一欄是那句限制，而且每一列都帶著它。匯出檔會被帶到 Hub 以外的地方
	// 打開，而一列「指派的比看到的舊」被單獨貼出去的時候，讀它的人沒有辦法知道
	// 這個 Hub 看不到指派以外的安裝路徑——那一列於是變成一句它沒有說過的指控。
	if rows[0][0] != "資源" || rows[0][len(rows[0])-1] != "這份報告的限制" {
		t.Fatalf("表頭=%v", rows[0])
	}
	// ⚠ 欄位照表頭找，不照位置。中間插一欄的時候，按位置寫的斷言會安靜地去檢查隔壁
	// 那一欄——而兩欄剛好都是空的時候，它會通過。
	at := map[string]int{}
	for index, header := range rows[0] {
		at[header] = index
	}
	for index, row := range rows[1:] {
		if row[len(row)-1] != operator.InstallReportCaveat {
			t.Fatalf("第 %d 列沒有帶那句限制：%v", index+1, row)
		}
	}
	// 匯出的檔案要答得出「哪一台沒有被指派過」——壓成統計就答不出來了。
	var found bool
	for _, row := range rows[1:] {
		if row[at["機器"]] != "sampleagent4" {
			continue
		}
		found = true
		if row[at["這一格是什麼"]] != operator.InstallStateTitle(operator.InstallUnassigned) {
			t.Errorf("sampleagent4 那一列是 %q，該是沒有被指派過", row[at["這一格是什麼"]])
		}
		if row[at["指派的版號"]] != "" || row[at["revision"]] != "" {
			t.Errorf("沒有被指派過的那一列帶著指派欄位：assigned=%q revision=%q",
				row[at["指派的版號"]], row[at["revision"]])
		}
		// 沒有這一軸的那一列三欄都留白，不是寫一個看起來像答案的字。
		for _, header := range []string{"版號講的是哪一份", "量版號的那個檔案", "正在跑的那個檔案"} {
			if row[at[header]] != "" {
				t.Errorf("沒有被指派過的那一列 %s 欄寫了 %q", header, row[at[header]])
			}
		}
	}
	if !found {
		t.Errorf("CSV 裡沒有 sampleagent4 那一列：%v", rows)
	}
	// 匯出的檔案要答得出「這一列的版號量錯了檔案」。
	for _, row := range rows[1:] {
		if row[at["機器"]] != "samplehub1" {
			continue
		}
		if row[at["版號講的是哪一份"]] != operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile) {
			t.Errorf("samplehub1 那一列的 runtime 欄是 %q", row[at["版號講的是哪一份"]])
		}
		if row[at["量版號的那個檔案"]] != installPageLoginCopy ||
			row[at["正在跑的那個檔案"]] != installPageReleaseCopy {
			t.Errorf("那兩個檔案沒有各佔一欄：量的是 %q、跑的是 %q",
				row[at["量版號的那個檔案"]], row[at["正在跑的那個檔案"]])
		}
	}
}

// 這份報告只講「現在」，沒有範圍好挑。
func TestTheInstallPageRejectsAQueryItCannotHonour(t *testing.T) {
	s, st := newServer(t)
	installPageFleet(t, st)
	for _, target := range []string{
		"/reports/install?days=7", "/reports/install.csv?resource=openclaw",
	} {
		if rec := webRequest(t, s, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d，應該拒絕", target, rec.Code)
		}
	}
}

// ⚠⚠ 「看到的版號不是正在跑的那一份」那一節必須排在「指派的跟看到的不一樣」前面。
// 反過來排的話，第一眼看到的是「指派的比看到的舊」——照它做的第一件事會是把一台其實
// 沒事的機器部署回舊版。
func TestTheInstallPageAsksWhichFileBeforeItAsksWhoDisagrees(t *testing.T) {
	s, st := newServer(t)
	_, older, _ := installPageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/install").Body.String())
	misattributed := strings.Index(body, `id="misattributed"`)
	differing := strings.Index(body, `id="differing"`)
	if misattributed < 0 || differing < 0 || misattributed > differing {
		t.Fatalf("兩節的順序不對：misattributed=%d differing=%d", misattributed, differing)
	}
	section := body[misattributed:differing]
	for _, want := range []string{
		"samplehub1", "2026.5.26", "2026.6.6",
		installPageLoginCopy, installPageReleaseCopy,
		operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile),
		operator.ToolRuntimeNextStep(operator.ToolRuntimeOtherFile),
	} {
		if !strings.Contains(section, want) {
			t.Errorf("那一節少了 %q：\n%s", want, section)
		}
	}
	if !strings.Contains(section, "/machines/"+older) {
		t.Error("量錯檔案的那台機器沒有連到它自己那一頁")
	}
	// 那一節要講清楚它為什麼排在前面。
	if !strings.Contains(section, "沒有人在用的檔案") {
		t.Errorf("那一節沒有講它為什麼排在前面：\n%s", section)
	}
}

// ⚠⚠ 「指派的跟看到的一樣」是這一頁上最沒有人會回頭看的一格，而 sampleagent2 在跑的那個
// 檔案已經從磁碟上不見了。那一格必須進得了這一節——否則它永遠不會被看到。
func TestTheInstallPageListsTheRowThatLinesUpButRunsAnotherFile(t *testing.T) {
	s, st := newServer(t)
	installPageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/install").Body.String())
	misattributed := strings.Index(body, `id="misattributed"`)
	differing := strings.Index(body, `id="differing"`)
	if misattributed < 0 || differing < 0 {
		t.Fatalf("找不到那兩節：misattributed=%d differing=%d", misattributed, differing)
	}
	section := body[misattributed:differing]
	if !strings.Contains(section, "sampleagent2") {
		t.Errorf("指派的跟看到的一樣、卻量在一個沒在跑的檔案上的那一台沒有被列出來：\n%s", section)
	}
	if !strings.Contains(section, operator.ToolRuntimeTitle(operator.ToolRuntimeGoneFile)) {
		t.Errorf("那一節沒有講出「正在跑的那個檔案不見了」：\n%s", section)
	}
	// 它在「不一樣」那一節裡沒有事情要做——兩個軸是分開的。
	unassigned := strings.Index(body, `id="unassigned"`)
	if unassigned < 0 {
		t.Fatal(`找不到 id="unassigned"`)
	}
	if strings.Contains(body[differing:unassigned], "sampleagent2") {
		t.Error("量錯檔案被算進了「指派的跟看到的不一樣」")
	}
}

// 那個數字要在摘要卡上，而且那張「資源 × 機器」的表要在自己那一格上講出來。那張表
// 是這一頁最多人只看一眼就走的地方。
func TestTheInstallPageCountsTheVersionsThatAreNotRunningOnTheSummaryAndTheMatrix(t *testing.T) {
	s, st := newServer(t)
	installPageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/install").Body.String())
	report, err := s.operator.InstallReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.Misattributed != 2 {
		t.Fatalf("這個機隊有兩格量的不是正在跑的那一份，報告說 %d", report.Misattributed)
	}
	if !strings.Contains(body, "版號不是跑的那一份") {
		t.Errorf("摘要卡上沒有這個數字：\n%s", body)
	}
	matrix := strings.Index(body, `id="matrix"`)
	states := strings.Index(body, `id="states"`)
	if matrix < 0 || states < 0 || matrix > states {
		t.Fatalf("兩節的順序不對：matrix=%d states=%d", matrix, states)
	}
	if section := body[matrix:states]; !strings.Contains(section, "其中 2 格看到的版號量的是沒在跑的那一份") {
		t.Errorf("那張表沒有講出有幾格量錯了檔案：\n%s", section)
	}
	// 每一格自己也要看得出來，不是只有表尾那一句。
	if section := body[matrix:states]; strings.Count(section, "版號不是跑的那一份") < 2 {
		t.Errorf("那張表上量錯檔案的那兩格沒有各自標出來：\n%s", section)
	}
}

// 這一軸的七種狀態一律列出來，包括 0 的那幾種。「沒有一格是這樣」跟「這一頁不講這件
// 事」是兩件事。
func TestTheInstallPageListsEveryWayAVersionCanNameAFile(t *testing.T) {
	s, st := newServer(t)
	installPageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/install").Body.String())
	runtimes := strings.Index(body, `id="runtimes"`)
	if runtimes < 0 {
		t.Fatal(`找不到 id="runtimes"`)
	}
	states := strings.Index(body, `id="states"`)
	if states < 0 || states > runtimes {
		t.Fatalf("狀態表要排在這一軸前面：states=%d runtimes=%d", states, runtimes)
	}
	section := body[runtimes:]
	for _, stateValue := range operator.ToolRuntimes() {
		if !strings.Contains(section, operator.ToolRuntimeTitle(stateValue)) {
			t.Errorf("這一軸少了 %q：\n%s", operator.ToolRuntimeTitle(stateValue), section)
		}
	}
	// 只有那兩種要人動手，別的不要。這一句不在畫面上的話，七種看起來都像待辦事項。
	if !strings.Contains(section, "其餘五種不用人動手") {
		t.Errorf("那一節沒有講清楚只有哪兩種算量錯了檔案：\n%s", section)
	}
}

// 一台從來沒回報過這個東西的機器，畫面上不准出現這一軸的任何一句話：Hub 沒有量過
// 它，也就沒有「量的是不是正在跑的那一份」這個問題。
func TestTheInstallPageLeavesTheRuntimeColumnEmptyWhereNothingWasSeen(t *testing.T) {
	s, st := newServer(t)
	installPageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/install").Body.String())
	report, err := s.operator.InstallReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	resource := report.Resources[0]
	var sampleagent4 operator.InstallRow
	for _, row := range resource.Rows {
		if row.DisplayName == "sampleagent4" {
			sampleagent4 = row
		}
	}
	if sampleagent4.MachineID == "" {
		t.Fatal("報告裡沒有 sampleagent4 那一列")
	}
	if sampleagent4.Runtime != nil {
		t.Fatalf("sampleagent4 從來沒有被指派過，卻帶著這一軸：%+v", sampleagent4.Runtime)
	}
	// 它那一列在最後那張表上要有一格 —— 只是那一格是留白，不是一句看起來像答案的話。
	anchor := strings.Index(body, `id="resource-openclaw-openclaw"`)
	if anchor < 0 {
		t.Fatal("找不到 openclaw 那一節")
	}
	row := body[anchor:]
	cut := strings.Index(row, "sampleagent4")
	if cut < 0 {
		t.Fatalf("最後那張表上沒有 sampleagent4：\n%s", row)
	}
	cell := row[cut:min(cut+600, len(row))]
	for _, banned := range []string{"量：", "跑：", operator.ToolRuntimeTitle(operator.ToolRuntimeIdle)} {
		if strings.Contains(cell, banned) {
			t.Errorf("sampleagent4 那一列出現了這一軸的 %q：\n%s", banned, cell)
		}
	}
}
