package web

import (
	"encoding/csv"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

func webRequest(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, verifiedWebRequest(httptest.NewRequest("GET", path, nil),
		"example.com/cap/clawctl-view"))
	return rec
}

func reportIndexNow(t *testing.T) operator.ReportIndex {
	t.Helper()
	index, err := operator.ReportIndexFor(store.DefaultRetention(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return index
}

// timelineFixture 造出四個來源都有列的一台機器：名冊、健康判定、工作單與操作員動作。
// 四個來源同時有東西，時間軸才會真的需要排序。
func timelineFixture(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.RecordStateTransition(id, state.Online, "心跳準時", now.Add(-40*time.Minute)); err != nil {
		t.Fatalf("狀態轉移：%v", err)
	}
	if err := st.RecordStateTransition(id, state.Degraded, "磁碟快滿了", now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("狀態轉移：%v", err)
	}
	preview, err := s.operator.PreviewDiagnosticNoop(operator.DiagnosticNoopPreviewRequest{
		MachineID: id, ExecutionTimeoutSeconds: 60,
	})
	if err != nil {
		t.Fatalf("預覽診斷工作單：%v", err)
	}
	if _, err := s.operator.CreateDiagnosticNoop(operator.DiagnosticNoopRequest{
		MachineID: id, ExecutionTimeoutSeconds: 60,
		ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "時間軸測試", IdempotencyKey: "timeline-diagnostic",
		Actor: operator.Actor{SourceKind: operator.SourceKindWeb, AuthSubject: "operator@example.com"},
	}); err != nil {
		t.Fatalf("開診斷工作單：%v", err)
	}
	return s, st, id
}

func timelineOnScreen(t *testing.T, s *Server, id string, query string) operator.MachineTimelineResult {
	t.Helper()
	days := 0
	if query != "" {
		var err error
		days, err = parseTimelineDays(httptest.NewRequest("GET", "/x?"+query, nil))
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.operator.MachineTimeline(
		operator.MachineTimelineRequest{MachineID: id, Days: days}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

var csvHrefPattern = regexp.MustCompile(`href="([^"]+\.csv)"`)

func exportRows(t *testing.T, body string) [][]string {
	t.Helper()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Fatal("匯出檔沒有 BOM —— Excel 會把中文欄名讀成亂碼")
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("匯出檔不是合法 CSV：%v", err)
	}
	return rows
}

// 落地頁的用處是「在點進去之前就知道該點哪一個」。每一份報告都要在這一頁上說出
// 自己的名字、回答什麼、範圍多大、看得到多遠，而且那個連結要真的存在。
func TestTheReportsPageNamesEveryReportAndWhereItGoes(t *testing.T) {
	s, _ := newServer(t)
	body := get(t, s, "/reports")
	index := reportIndexNow(t)
	for _, entry := range index.Entries {
		if !strings.Contains(body, `href="`+entry.Path+`"`) {
			t.Errorf("%s 沒有連到 %s", entry.Kind, entry.Path)
		}
		for _, want := range []string{
			entry.Title, entry.Question, entry.Evidence,
			entry.ScopeSentence, entry.RangeSentence, entry.HorizonSentence,
		} {
			if !strings.Contains(body, html.EscapeString(want)) {
				t.Errorf("%s 的畫面上少了 %q", entry.Kind, want)
			}
		}
	}
}

// 匯出連結只能出現在整份匯得出來的報告上。一份分頁讀的報告給出 CSV，下載到的是
// 當下那一頁，而那個檔案打開之後看起來就是整份報告。
func TestTheReportsPageOffersCSVOnlyWhereTheWholeRangeComesOut(t *testing.T) {
	s, _ := newServer(t)
	body := get(t, s, "/reports")
	want := map[string]bool{}
	for _, entry := range reportIndexNow(t).Entries {
		if entry.Export == operator.ReportExportWholeWindow && entry.ExportPath != "" {
			want[entry.ExportPath] = true
		}
	}
	got := map[string]bool{}
	for _, match := range csvHrefPattern.FindAllStringSubmatch(body, -1) {
		got[match[1]] = true
	}
	for path := range want {
		if !got[path] {
			t.Errorf("整份匯得出來的報告沒有匯出連結：%s", path)
		}
	}
	for path := range got {
		if !want[path] {
			t.Errorf("落地頁給了一個不該有的匯出連結：%s", path)
		}
	}
}

// 落地頁沒有任何參數。一個接受了參數卻對它視而不見的網址，會讓分享出去的連結
// 看起來像篩過的。
func TestTheReportsPageRefusesAQueryItDoesNotRead(t *testing.T) {
	s, _ := newServer(t)
	if rec := webRequest(t, s, "/reports?days=7"); rec.Code != http.StatusBadRequest {
		t.Fatalf("狀態 %d，內容：%s", rec.Code, rec.Body.String())
	}
}

// 時間軸的四個來源都要在畫面上交代自己讀到幾列、讀的是什麼。一個只印事件不印來源
// 的時間軸，沒有辦法回答「這裡面有沒有漏」。
func TestTheTimelinePageSaysWhatEachSourceRead(t *testing.T) {
	s, _, id := timelineFixture(t)
	body := get(t, s, "/machines/"+id+"/timeline")
	result := timelineOnScreen(t, s, id, "")
	if len(result.Sources) == 0 {
		t.Fatal("時間軸沒有任何來源")
	}
	for _, read := range result.Sources {
		if read.Count == 0 {
			t.Fatalf("這台機器在 %s 上沒有任何一列，這一頁的那一欄就測不到", read.Source)
		}
		if !strings.Contains(body, html.EscapeString(read.Label)) ||
			!strings.Contains(body, html.EscapeString(read.Evidence)) {
			t.Errorf("畫面少了來源 %s 的交代", read.Source)
		}
	}
	for _, entry := range result.Entries {
		if !strings.Contains(body, html.EscapeString(entry.Summary)) {
			t.Errorf("畫面少了這一列：%s %s", entry.At.Format(time.RFC3339), entry.Summary)
		}
	}
	if result.Complete && strings.Contains(body, "沒讀完") {
		t.Error("每個來源都讀完了，畫面卻說有來源沒讀完")
	}
	if !strings.Contains(stripTags(body), "每一個來源都讀完了這段期間") {
		t.Errorf("畫面沒有交代完整性：\n%s", around(stripTags(body), "共"))
	}
}

// 既有測試只防止完整的結果被說成沒讀完；這裡補上沒讀完卻被說成完整的反方向。
func TestTheTimelinePageWarnsWhenStateHistoryDidNotReadTheWholeWindow(t *testing.T) {
	s, st, id := timelineFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for index := 0; index < store.DetailHistoryLimit+1; index++ {
		machineState := state.Online
		if index%2 == 1 {
			machineState = state.Degraded
		}
		at := now.Add(-time.Duration(store.DetailHistoryLimit-index) * time.Minute)
		if err := st.RecordStateTransition(id, machineState, "截斷測試", at); err != nil {
			t.Fatal(err)
		}
	}

	body := get(t, s, "/machines/"+id+"/timeline")
	for _, want := range []string{
		"沒讀完",
		operator.MachineTimelineIncompleteNextStep(operator.MachineTimelineSourceState),
		"有來源沒讀完這段期間，總數會少算。",
	} {
		if !strings.Contains(stripTags(body), want) {
			t.Fatalf("畫面少了 %q：\n%s", want, around(stripTags(body), "共"))
		}
	}
	if strings.Contains(stripTags(body), "每一個來源都讀完了這段期間") {
		t.Fatalf("畫面把沒讀完的來源說成完整：\n%s", around(stripTags(body), "共"))
	}
}

// 最近發生的事排在最上面。一條照時間反過來排的時間軸，會讓操作員把最舊的那件事
// 當成剛剛發生的。
func TestTheTimelinePageIsOrderedNewestFirst(t *testing.T) {
	s, _, id := timelineFixture(t)
	body := get(t, s, "/machines/"+id+"/timeline")
	result := timelineOnScreen(t, s, id, "")
	if len(result.Entries) < 2 {
		t.Fatalf("時間軸只有 %d 列，排序測不出來", len(result.Entries))
	}
	previous := -1
	for _, entry := range result.Entries {
		at := strings.Index(body, html.EscapeString(entry.Summary))
		if at < 0 {
			t.Fatalf("畫面上找不到 %q", entry.Summary)
		}
		if at < previous {
			t.Fatalf("%q 出現在比它舊的列後面", entry.Summary)
		}
		previous = at
	}
}

// 匯出的是畫面上那些列，一列不多一列不少，而且匯出連結帶的是畫面現在這個範圍。
// 匯出另一個範圍的檔案，是這一頁最容易騙人的地方。
func TestTheTimelineExportCarriesTheSameRowsAndTheSameRange(t *testing.T) {
	s, _, id := timelineFixture(t)
	body := get(t, s, "/machines/"+id+"/timeline?days=1")
	if !strings.Contains(body, `href="/machines/`+id+`/timeline.csv?days=1"`) {
		t.Fatalf("匯出連結沒有帶畫面上的範圍：\n%s", around(body, "匯出"))
	}
	rec := webRequest(t, s, "/machines/"+id+"/timeline.csv?days=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("匯出狀態 %d，內容：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type=%q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="ai-intune-timeline-`+id+`.csv"` {
		t.Errorf("Content-Disposition=%q", got)
	}
	rows := exportRows(t, rec.Body.String())
	result := timelineOnScreen(t, s, id, "days=1")
	if len(rows) != result.Total+1 {
		t.Fatalf("畫面上 %d 列，匯出 %d 列（含欄名）", result.Total, len(rows))
	}
	for i, entry := range result.Entries {
		if !strings.Contains(strings.Join(rows[i+1], "\x00"), entry.Summary) {
			t.Errorf("匯出第 %d 列 %v 不是畫面上的 %q", i+1, rows[i+1], entry.Summary)
		}
	}
}

// 範圍選單只提供這一頁真的收得下的天數，而且它記得現在看的是哪一個。一個永遠停在
// 預設值的選單，會讓人以為自己沒有改到。
func TestTheTimelineRangeFormOffersOnlyRangesItAccepts(t *testing.T) {
	s, _, id := timelineFixture(t)
	body := get(t, s, "/machines/"+id+"/timeline?days=30")
	for _, days := range []string{"1", "7", "30"} {
		if !strings.Contains(body, `<option value="`+days+`"`) {
			t.Errorf("範圍選單沒有最近 %s 天", days)
		}
	}
	if !strings.Contains(body, `<option value="30" selected>`) {
		t.Errorf("選單沒有停在畫面現在的範圍：\n%s", around(body, "報告期間"))
	}
	if strings.Contains(body, `<option value="7" selected>`) {
		t.Error("選單同時選了兩個範圍")
	}
}

// 畫面與匯出都拒絕它站不住的範圍，而且拒絕的理由一樣。只有畫面擋、匯出放行，就等於
// 把那條路留在匯出上。
func TestTheTimelineRefusesARangeOrAMachineItCannotStandBehind(t *testing.T) {
	s, _, id := timelineFixture(t)
	for _, test := range []struct {
		name string
		path string
		code int
	}{
		{"零天", "/machines/" + id + "/timeline?days=0", http.StatusBadRequest},
		{"超過上限", "/machines/" + id + "/timeline?days=31", http.StatusBadRequest},
		{"前導零", "/machines/" + id + "/timeline?days=07", http.StatusBadRequest},
		{"不是數字", "/machines/" + id + "/timeline?days=week", http.StatusBadRequest},
		{"重複出現", "/machines/" + id + "/timeline?days=1&days=7", http.StatusBadRequest},
		{"認不得的參數", "/machines/" + id + "/timeline?from=2026-09-01", http.StatusBadRequest},
		{"不在名冊上", "/machines/" + strings.Repeat("0", 32) + "/timeline", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			if rec := webRequest(t, s, test.path); rec.Code != test.code {
				t.Fatalf("畫面狀態 %d，要 %d：%s", rec.Code, test.code, rec.Body.String())
			}
			csvPath := strings.Replace(test.path, "/timeline", "/timeline.csv", 1)
			if rec := webRequest(t, s, csvPath); rec.Code != test.code {
				t.Fatalf("匯出狀態 %d，要 %d：%s", rec.Code, test.code, rec.Body.String())
			}
		})
	}
}
