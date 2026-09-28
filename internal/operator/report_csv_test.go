package operator

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func sampleReportCSVDocument() ReportCSVDocument {
	return ReportCSVDocument{
		Filename: "ai-intune-sample.csv",
		Columns: []ReportCSVColumn{
			{Key: "at", Header: "時間（UTC）"},
			{Key: "subject", Header: "對象"},
			{Key: "note", Header: "說明"},
		},
		Rows: [][]string{{"2026-09-12T00:00:00Z", "samplehub1", "ok"}},
	}
}

func parseExport(t *testing.T, body string) [][]string {
	t.Helper()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Fatalf("匯出檔沒有 BOM，試算表會把中文標題讀成亂碼：%q", firstExportLine(body))
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatalf("匯出檔不是合法的 CSV：%v", err)
	}
	return records
}

func firstExportLine(body string) string {
	if index := strings.IndexByte(body, '\n'); index >= 0 {
		return body[:index]
	}
	return body
}

// 匯出檔幾乎只會在試算表裡被打開，而試算表會把 = + - @ 開頭的儲存格當成公式
// 執行。這些欄位裝的是上游原文——provider 名稱、機器名字、錯誤訊息都由別人
// 決定，所以防護必須每一格都過，不是挑「看起來危險的那幾欄」。
func TestAnExportCannotSmuggleAFormulaIntoAnyColumn(t *testing.T) {
	doc := sampleReportCSVDocument()
	doc.Rows = [][]string{
		{"=1+1", "+cmd", "-at"},
		{"@here", " =lead", "\t-tab"},
	}
	records := parseExport(t, mustReportCSV(t, doc))
	if len(records) != 3 {
		t.Fatalf("列數 %d", len(records))
	}
	for row := 1; row < len(records); row++ {
		for cell, value := range records[row] {
			if !strings.HasPrefix(value, "'") {
				t.Errorf("第 %d 列第 %d 格 %q 沒有被擋成文字", row, cell+1, value)
			}
		}
	}
}

// 標題列講的必須就是欄位定義講的。標題文案改了而欄位順序沒跟著改，是最安靜的
// 一種錯：檔案還是打得開，只是每一欄的意思都錯位了。
func TestAnExportHeaderSaysExactlyWhatTheColumnsSay(t *testing.T) {
	doc := sampleReportCSVDocument()
	records := parseExport(t, mustReportCSV(t, doc))
	if len(records) == 0 {
		t.Fatal("匯出檔沒有標題列")
	}
	columns := ReportCSVColumns(doc)
	if len(records[0]) != len(columns) {
		t.Fatalf("標題列 %d 欄，欄位定義 %d 欄", len(records[0]), len(columns))
	}
	for index, column := range columns {
		if records[0][index] != column.Header {
			t.Errorf("第 %d 欄標題是 %q，欄位定義說是 %q", index+1, records[0][index], column.Header)
		}
	}
}

// 一份沒有資料的報告匯出的是「只有標題的檔案」，不是空檔。打開一個 0 byte 的
// 檔案，分不出「這段期間什麼都沒發生」與「匯出壞了」。
func TestAnEmptyReportStillExportsItsHeader(t *testing.T) {
	doc := sampleReportCSVDocument()
	doc.Rows = nil
	records := parseExport(t, mustReportCSV(t, doc))
	if len(records) != 1 || len(records[0]) != len(ReportCSVColumns(doc)) {
		t.Fatalf("空報告的匯出=%v", records)
	}
}

// 匯出會被帶到 Hub 以外的地方當證據引用，所以寧可不給檔案，也不給一份「大致上
// 對」的檔案。
func TestAnExportRefusesToShipAFileItCannotStandBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ReportCSVDocument)
	}{
		{"檔名不是 csv", func(d *ReportCSVDocument) { d.Filename = "report" }},
		{"檔名帶路徑", func(d *ReportCSVDocument) { d.Filename = "../ai-intune.csv" }},
		{"一欄都沒有", func(d *ReportCSVDocument) { d.Columns, d.Rows = nil, nil }},
		{"欄位代號重複", func(d *ReportCSVDocument) { d.Columns[1].Key = d.Columns[0].Key }},
		{"欄位代號不是 canonical", func(d *ReportCSVDocument) { d.Columns[0].Key = "At-Time" }},
		{"欄位沒有標題", func(d *ReportCSVDocument) { d.Columns[2].Header = "  " }},
		{"某一列少一格", func(d *ReportCSVDocument) { d.Rows[0] = d.Rows[0][:2] }},
		{"某一列多一格", func(d *ReportCSVDocument) { d.Rows[0] = append(d.Rows[0], "extra") }},
		{"儲存格裡還有控制字元", func(d *ReportCSVDocument) { d.Rows[0][2] = "ok\x01" }},
		{"限制那一句裡有控制字元", func(d *ReportCSVDocument) { d.Caveat = "這個 Hub 看不到\x01" }},
		{"限制那一欄撞到已經有的欄位", func(d *ReportCSVDocument) {
			d.Columns[0].Key = "caveat"
			d.Caveat = "這個 Hub 看不到指派以外的安裝路徑。"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := sampleReportCSVDocument()
			tc.mutate(&doc)
			if _, err := ReportCSV(doc); err == nil {
				t.Fatal("這份匯出被接受了")
			}
		})
	}
}

// 換行與 tab 留著：多行的錯誤原文本來就是多行，而 RFC 4180 的引號裡放得下它們。
func TestAnExportKeepsAMultilineErrorIntact(t *testing.T) {
	doc := sampleReportCSVDocument()
	doc.Rows = [][]string{{"2026-09-12T00:00:00Z", "samplehub1", "first\nsecond\tthird"}}
	records := parseExport(t, mustReportCSV(t, doc))
	if len(records) != 2 || records[1][2] != "first\nsecond\tthird" {
		t.Fatalf("多行證據被改掉了：%q", records)
	}
}

// 一份報告講得出口的極限要跟著每一列走，不是放在檔首。
//
// ⚠⚠ 前言只活到有人排序、篩選或把其中一列貼進信裡為止，而「其中一列被單獨引
// 用」正是這句話要擋的那件事：一列寫著「指派的比看到的舊」的資料離開 Hub 之
// 後，讀它的人沒有辦法知道這個 Hub 看不到指派以外的安裝路徑——那一列於是變成
// 一句它沒有說過的指控。所以它是一欄，而且每一列都帶著它。
func TestAnExportCarriesItsLimitOnEveryRowNotJustTheTop(t *testing.T) {
	doc := sampleReportCSVDocument()
	doc.Caveat = "這個 Hub 看不到指派以外的安裝路徑。"
	doc.Rows = [][]string{
		{"2026-09-12T00:00:00Z", "samplehub1", "指派的比看到的舊"},
		{"2026-09-12T00:00:00Z", "sampleagent1", "沒有被指派過"},
	}
	records := parseExport(t, mustReportCSV(t, doc))
	if len(records) != 3 {
		t.Fatalf("列數 %d", len(records))
	}
	if last := records[0][len(records[0])-1]; last != "這份報告的限制" {
		t.Fatalf("最後一欄的標題是 %q", last)
	}
	for index, row := range records[1:] {
		if len(row) != len(records[0]) {
			t.Fatalf("第 %d 列 %d 格，標題 %d 欄", index+1, len(row), len(records[0]))
		}
		if got := row[len(row)-1]; got != doc.Caveat {
			t.Errorf("第 %d 列的最後一格是 %q，不是那句限制", index+1, got)
		}
	}
}

// 沒有極限要交代的報告不會多一欄空白。每一列補一欄空白，等於教人把這一欄當成
// 雜訊略過——下一份真的有話要說的報告就再也沒有人讀那一欄了。
func TestAReportWithNothingToQualifyHasNoSuchColumn(t *testing.T) {
	doc := sampleReportCSVDocument()
	records := parseExport(t, mustReportCSV(t, doc))
	for _, cell := range records[0] {
		if cell == "這份報告的限制" {
			t.Fatalf("沒有限制的報告也長出了那一欄：%v", records[0])
		}
	}
	if len(records[0]) != len(doc.Columns) {
		t.Fatalf("標題列 %d 欄，欄位定義 %d 欄", len(records[0]), len(doc.Columns))
	}
}

// 呼叫端要對照表頭只能問 ReportCSVColumns。直接讀 Document.Columns 的檢查會在
// 限制被加進來的那一天對著兩份不同的東西比較，然後說檔案壞了。
func TestTheColumnsAReaderIsToldAboutAreTheColumnsInTheFile(t *testing.T) {
	doc := sampleReportCSVDocument()
	doc.Caveat = "這個 Hub 看不到指派以外的安裝路徑。"
	records := parseExport(t, mustReportCSV(t, doc))
	columns := ReportCSVColumns(doc)
	if len(columns) != len(doc.Columns)+1 {
		t.Fatalf("ReportCSVColumns 回了 %d 欄，欄位定義 %d 欄", len(columns), len(doc.Columns))
	}
	if len(records[0]) != len(columns) {
		t.Fatalf("標題列 %d 欄，ReportCSVColumns 說 %d 欄", len(records[0]), len(columns))
	}
	for index, column := range columns {
		if records[0][index] != column.Header {
			t.Errorf("第 %d 欄標題是 %q，ReportCSVColumns 說是 %q", index+1, records[0][index], column.Header)
		}
	}
	if columns[len(columns)-1].Key != "caveat" {
		t.Errorf("限制那一欄的代號是 %q", columns[len(columns)-1].Key)
	}
}

// 那句限制跟其他每一格走同一條路。它是這份程式自己寫的字串，但匯出檔的規則不
// 分來源——一條「看寫的人是誰」的例外，是下一個繞過它的人的入口。
func TestTheLimitGoesThroughTheSameSpreadsheetGuardAsEveryOtherCell(t *testing.T) {
	doc := sampleReportCSVDocument()
	doc.Caveat = "=cmd|' /c calc'!A1"
	records := parseExport(t, mustReportCSV(t, doc))
	if got := records[1][len(records[1])-1]; !strings.HasPrefix(got, "'") {
		t.Fatalf("限制那一格 %q 沒有被擋成文字", got)
	}
}

// 匯出檔可能在另一個時區被打開。落地成本地時間字串之後就再也分不出「這是哪一
// 刻」，所以匯出裡只有一種時間寫法。
func TestEveryExportWritesTheSameKindOfTime(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	moment := time.Date(2026, 9, 12, 20, 30, 0, 123456789, zone)
	want := "2026-09-12T12:30:00.123456789Z"
	if got := ReportCSVTime(moment); got != want {
		t.Errorf("ReportCSVTime=%q want %q", got, want)
	}
	if got := ReportCSVOptionalTime(&moment); got != want {
		t.Errorf("ReportCSVOptionalTime=%q want %q", got, want)
	}
	if got := ReportCSVOptionalTime(nil); got != "" {
		t.Errorf("沒有值的時間應該是空格，不是 %q", got)
	}
}

// 下載標頭用的就是這份匯出自己宣告的檔名，不是呼叫端另外打一次的字串。
func TestAnExportNamesItselfInTheDownloadHeader(t *testing.T) {
	doc := sampleReportCSVDocument()
	want := `attachment; filename="ai-intune-sample.csv"`
	if got := ReportCSVContentDisposition(doc); got != want {
		t.Fatalf("Content-Disposition=%q want %q", got, want)
	}
}

func mustReportCSV(t *testing.T, doc ReportCSVDocument) string {
	t.Helper()
	body, err := ReportCSV(doc)
	if err != nil {
		t.Fatalf("ReportCSV: %v", err)
	}
	return body
}
