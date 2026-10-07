package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

func reportCLIServer(t *testing.T, f jobsFixture) (string, machineCommandDeps) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, grantedOperatorRequest(r, true, true))
	}))
	t.Cleanup(server.Close)
	return machineHTTPTestDeps(t, server)
}

func reportCLIExportRows(t *testing.T, body string) [][]string {
	t.Helper()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Fatal("匯出檔沒有 BOM")
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("匯出檔不是合法 CSV：%v", err)
	}
	return rows
}

// CLI 上的清單要跟畫面回答同樣的三件事：這份報告算什麼、看得到多遠、能不能整份
// 帶走。少了任何一件，操作員就得再開一次瀏覽器才知道該讀哪一份。
func TestReportListCLIPrintsWhatEachReportAnswers(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	var out, errOut bytes.Buffer
	if err := runReportListCommandWithDeps(t.Context(), []string{"--hub-url", base},
		&out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "report\t") && !strings.Contains(text, "report  ") {
		t.Fatalf("沒有表頭：\n%s", text)
	}
	index, err := operator.ReportIndexFor(store.DefaultRetention(), hubNow())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range index.Entries {
		for _, want := range []string{entry.Title, entry.Question, entry.HorizonSentence, entry.Path} {
			if !strings.Contains(text, want) {
				t.Fatalf("輸出少了 %q：\n%s", want, text)
			}
		}
	}
	if !strings.Contains(text, "reports, ") || !strings.Contains(text, "can be exported in full") {
		t.Fatalf("沒有總計那一行：\n%s", text)
	}
}

func TestReportListCLIEmitsTheSameDocumentAsTheAPI(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	var out, errOut bytes.Buffer
	if err := runReportListCommandWithDeps(t.Context(), []string{"--hub-url", base, "--json"},
		&out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	var index operator.ReportIndex
	if err := json.Unmarshal(out.Bytes(), &index); err != nil {
		t.Fatalf("--json 不是合法 JSON：%v\n%s", err, out.String())
	}
	if index.Total != len(operator.ReportKinds()) || len(index.Entries) != index.Total {
		t.Fatalf("index=%+v", index)
	}
}

func TestReportListCLIRefusesWhatItCannotRun(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	for name, argv := range map[string][]string{
		"positional 參數": {"--hub-url", base, "extra"},
		"認不得的 flag":     {"--hub-url", base, "--days", "7"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := runReportListCommandWithDeps(t.Context(), argv, &out, &errOut, deps); err == nil {
				t.Fatalf("被接受了：%s", out.String())
			}
		})
	}
}

// 時間軸在 CLI 上要交代的跟畫面一樣多：每一個來源讀到幾列、讀的是什麼。只印事件
// 的時間軸回答不了「這裡面有沒有漏」。
func TestMachineTimelineCLIPrintsEverySourceAndEveryRow(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"timeline", "--hub-url", base, "--machine", f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		"cnode-operator (" + f.machine.id + ") last 7 days, ",
		"SOURCE", "ROWS", "EVIDENCE READ", "TIME (UTC)", "WHAT HAPPENED",
		"判定為 Degraded", "開了一張工作單", "Detail: 磁碟快滿了", "Evidence: /jobs/",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("輸出少了 %q：\n%s", want, text)
		}
	}
	for _, source := range operator.MachineTimelineSources() {
		if !strings.Contains(text, operator.MachineTimelineSourceEvidence(source)) {
			t.Fatalf("少了來源 %s 的交代：\n%s", source, text)
		}
	}
}

// 這也把真的 server 頁面第一次餵進 operator client 的自洽閘；result.Complete 接錯時，
// CLI 雖然不直接讀它，仍會報「operator client: machine timeline 的完整性跟逐個來源對不起來」。
func TestMachineTimelineCLIWarnsWhenStateHistoryDidNotReadTheWholeWindow(t *testing.T) {
	f, now := reportFixture(t)
	for index := 0; index < store.DetailHistoryLimit+1; index++ {
		machineState := state.Online
		if index%2 == 1 {
			machineState = state.Degraded
		}
		at := now.Add(-time.Duration(store.DetailHistoryLimit+1-index) * time.Minute)
		if err := f.store.RecordStateTransition(f.machine.id, machineState, "截斷測試", at); err != nil {
			t.Fatal(err)
		}
	}
	base, deps := reportCLIServer(t, f)
	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"timeline", "--hub-url", base, "--machine", f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("執行時間軸失敗：%v；錯誤輸出：%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "did not finish reading this period. Next step:") {
		t.Fatalf("輸出沒有狀態來源未讀完的警告：\n%s", out.String())
	}
}

// 匯出的是這次讀到的全部，一列不多一列不少。
func TestMachineTimelineCLIExportsExactlyWhatItPrinted(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	var jsonOut, csvOut, errOut bytes.Buffer
	argv := []string{"timeline", "--hub-url", base, "--machine", f.machine.id, "--days", "1"}
	if err := runMachineCommandWithDeps(t.Context(), append(argv, "--json"),
		&jsonOut, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	var result operator.MachineTimelineResult
	if err := json.Unmarshal(jsonOut.Bytes(), &result); err != nil {
		t.Fatalf("--json 不是合法 JSON：%v", err)
	}
	if result.Window.Days != 1 || result.Total == 0 {
		t.Fatalf("result=%+v", result)
	}
	if err := runMachineCommandWithDeps(t.Context(), append(argv, "--csv"),
		&csvOut, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	rows := reportCLIExportRows(t, csvOut.String())
	if len(rows) != result.Total+1 {
		t.Fatalf("讀到 %d 列，匯出 %d 列（含欄名）", result.Total, len(rows))
	}
	for index, entry := range result.Entries {
		if !strings.Contains(strings.Join(rows[index+1], "\x00"), entry.Summary) {
			t.Fatalf("匯出第 %d 列 %v 不是讀到的 %q", index+1, rows[index+1], entry.Summary)
		}
	}
}

// 一列很長的判定理由不能把整張表撐開。撐開之後先後順序就讀不出來了，而先後順序
// 正是這一頁存在的理由。
func TestALongReasonDoesNotWidenTheTimelineColumns(t *testing.T) {
	render := func(detail string) string {
		at := time.Date(2026, 9, 12, 3, 15, 55, 0, time.UTC)
		result := operator.MachineTimelineResult{
			SchemaVersion: operator.MachineTimelineSchemaVersion, EvaluatedAt: at,
			MachineID: "m1", DisplayName: "samplehub1", Complete: true, Total: 1,
			Window: operator.MachineTimelineWindow{Days: 7, From: at.Add(-7 * 24 * time.Hour), To: at},
			Entries: []operator.MachineTimelineEntry{{
				At: at, Kind: operator.MachineTimelineStateEntered,
				Source:  operator.MachineTimelineSourceState,
				Summary: "判定為 Degraded", Detail: detail,
			}},
		}
		for _, source := range operator.MachineTimelineSources() {
			result.Sources = append(result.Sources, operator.MachineTimelineSourceRead{
				Source: source, Label: operator.MachineTimelineSourceLabel(source),
				Evidence: operator.MachineTimelineSourceEvidence(source), Complete: true,
			})
		}
		var out bytes.Buffer
		if err := writeMachineTimeline(&out, result); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	short := render("磁碟快滿了")
	long := render(strings.Repeat("這條判定的理由很長。", 200))
	shortRow := timelineLineContaining(t, short, "判定為 Degraded")
	longRow := timelineLineContaining(t, long, "判定為 Degraded")
	if shortRow != longRow {
		t.Fatalf("長理由把事件那一列撐開了：\n%q\n%q", shortRow, longRow)
	}
	if !strings.Contains(long, "Detail: 這條判定的理由很長。") {
		t.Fatalf("長理由沒有印出來：\n%s", long)
	}
}

func timelineLineContaining(t *testing.T, text, want string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("輸出裡沒有含 %q 的那一列：\n%s", want, text)
	return ""
}

func TestMachineTimelineCLIRefusesWhatItCannotRun(t *testing.T) {
	f, _ := reportFixture(t)
	base, deps := reportCLIServer(t, f)
	machine := []string{"timeline", "--hub-url", base, "--machine", f.machine.id}
	var missing, missingErr bytes.Buffer
	// 這一句要由 CLI 自己說。讓它掉到用戶端才擋，操作員讀到的是一句在講 machine_id
	// 格式的話，而他漏掉的是一個 flag。
	if err := runMachineCommandWithDeps(t.Context(), []string{"timeline", "--hub-url", base},
		&missing, &missingErr, deps); err == nil ||
		!strings.Contains(err.Error(), "machine timeline: --machine is required") {
		t.Fatalf("沒有 --machine 時說的是：%v", err)
	}
	for name, argv := range map[string][]string{
		"零天":            append(append([]string{}, machine...), "--days", "0"),
		"超過上限":          append(append([]string{}, machine...), "--days", "31"),
		"前導零":           append(append([]string{}, machine...), "--days", "07"),
		"兩種輸出":          append(append([]string{}, machine...), "--json", "--csv"),
		"positional 參數": append(append([]string{}, machine...), "extra"),
		"不在名冊上":         {"timeline", "--hub-url", base, "--machine", strings.Repeat("0", 32)},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := runMachineCommandWithDeps(t.Context(), argv, &out, &errOut, deps); err == nil {
				t.Fatalf("被接受了：%s", out.String())
			}
		})
	}
}
