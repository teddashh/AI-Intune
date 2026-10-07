package operator

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// 匯出檔的契約。
//
// 每一份匯得出來的報告都走這裡，理由是匯出檔會被帶到 Hub 以外的地方打開：
// 欄位跟標題對不上、時間換了一種寫法、或者某一欄忘了擋試算表公式，發現的時候
// 檔案已經在別人的硬碟裡了，而那份檔案不會再回來改。所以「一份匯出長什麼樣子」
// 只能有一個答案，不是每支匯出各自寫一次。
const ReportCSVSchemaVersion = 1

// byte order mark。這些檔案幾乎只會在試算表裡被打開，沒有 BOM 中文標題會變成
// 亂碼。它必須由這裡加：由呼叫端各自決定的話，網頁下載到的跟 CLI 匯出到的就會
// 是兩個不同的檔案。
const reportCSVByteOrderMark = "\ufeff"

var ErrInvalidReportCSV = errors.New("operator: invalid report CSV document")

var (
	reportCSVFilenamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*\.csv$`)
	reportCSVKeyPattern      = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)
)

// ReportCSVColumn 是一欄。Key 是這一欄在程式裡的名字，Header 是操作員在試算表
// 裡看到的那一格。兩個都要有：只有 Header 的話，改標題文案就會默默改掉別人已經
// 寫好的公式所依賴的欄位順序，而沒有任何東西會紅。
type ReportCSVColumn struct {
	Key    string
	Header string
}

// ReportCSVDocument 是一份完整的匯出。
type ReportCSVDocument struct {
	Filename string
	Columns  []ReportCSVColumn
	Rows     [][]string

	// Caveat 是這份報告講得出口的極限，會變成最後一欄、每一列都帶著同一句。
	//
	// ⚠⚠ 它是一欄而不是檔首的一行，因為一行前言只活到有人排序、篩選或把其中
	// 一列貼進信裡為止——而「其中一列被單獨引用」正是這句話要擋的那件事。
	// 一列寫著「指派的比看到的舊」的資料離開 Hub 之後，讀它的人沒有辦法知道
	// 這個 Hub 看不到指派以外的安裝路徑；那一列於是變成一句它沒有說過的指控。
	//
	// 沒有極限要交代的報告留空，那一欄就不存在——每一列補一欄空白，等於教人
	// 把這一欄當成雜訊略過。
	Caveat string
}

// reportCSVCaveatColumn 是那一欄的固定身分。
var reportCSVCaveatColumn = ReportCSVColumn{Key: "caveat", Header: "這份報告的限制"}

// ReportCSVColumns 是這份匯出**實際寫出來**的欄位。
//
// ⚠ 呼叫端要對照表頭的話只能問這裡，不能直接讀 Document.Columns：那一份不含
// 限制那一欄，於是「表頭跟文件說的一樣嗎」這個檢查會在限制被加進來的那一天
// 對著兩份不同的東西比較，然後說檔案壞了。
func ReportCSVColumns(doc ReportCSVDocument) []ReportCSVColumn {
	return reportCSVColumns(doc)
}

// reportCSVColumns 是這份匯出實際會寫出來的欄位。
func reportCSVColumns(doc ReportCSVDocument) []ReportCSVColumn {
	if doc.Caveat == "" {
		return doc.Columns
	}
	return append(append([]ReportCSVColumn{}, doc.Columns...), reportCSVCaveatColumn)
}

// ReportCSV 把一份匯出寫成檔案內容。
//
// ⚠ 它會拒絕，不會將就。欄數對不上、儲存格裡還有控制字元——這些都表示上游的
// 投影沒有做完，而一份「大致上對」的匯出比沒有匯出更糟：它會被當成證據引用。
func ReportCSV(doc ReportCSVDocument) (string, error) {
	if err := validateReportCSVDocument(doc); err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	buffer.WriteString(reportCSVByteOrderMark)
	writer := csv.NewWriter(&buffer)
	columns := reportCSVColumns(doc)
	header := make([]string, 0, len(columns))
	for _, column := range columns {
		header = append(header, column.Header)
	}
	if err := writer.Write(header); err != nil {
		return "", err
	}
	for _, row := range doc.Rows {
		cells := make([]string, 0, len(columns))
		for _, cell := range row {
			cells = append(cells, reportCSVCell(cell))
		}
		if doc.Caveat != "" {
			cells = append(cells, reportCSVCell(doc.Caveat))
		}
		if err := writer.Write(cells); err != nil {
			return "", err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

// ReportCSVContentDisposition 是這份匯出的下載標頭。
func ReportCSVContentDisposition(doc ReportCSVDocument) string {
	return fmt.Sprintf("attachment; filename=%q", doc.Filename)
}

// ReportCSVTime 是匯出檔裡唯一一種時間寫法：UTC、RFC 3339、到奈秒。
//
// 本地時間是畫面的事。一份匯出可能在另一個時區被打開，落地成另一個時區的字串
// 之後就再也分不出「這是哪一刻」。
func ReportCSVTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

// ReportCSVOptionalTime 把「沒有這個時間」寫成空格。
//
// 空格就是匯出檔裡「沒有值」的寫法。畫面上的破折號在試算表裡是一段文字，會讓
// 一整欄變成文字欄，也會讓「有幾列有值」數錯。
func ReportCSVOptionalTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return ReportCSVTime(*value)
}

func validateReportCSVDocument(doc ReportCSVDocument) error {
	if !reportCSVFilenamePattern.MatchString(doc.Filename) {
		return fmt.Errorf("%w: filename %q is not a canonical .csv filename", ErrInvalidReportCSV, doc.Filename)
	}
	if len(doc.Columns) == 0 {
		return fmt.Errorf("%w: an export must have at least one column", ErrInvalidReportCSV)
	}
	if position, bad := firstReportCSVControlRune(doc.Caveat); bad {
		return fmt.Errorf("%w: character %d of the caveat is a control character", ErrInvalidReportCSV, position+1)
	}
	seen := make(map[string]bool, len(doc.Columns))
	for _, column := range reportCSVColumns(doc) {
		if !reportCSVKeyPattern.MatchString(column.Key) {
			return fmt.Errorf("%w: column key %q is not canonical", ErrInvalidReportCSV, column.Key)
		}
		if seen[column.Key] {
			return fmt.Errorf("%w: column key %q appears twice", ErrInvalidReportCSV, column.Key)
		}
		seen[column.Key] = true
		if strings.TrimSpace(column.Header) == "" {
			return fmt.Errorf("%w: column %q has no header", ErrInvalidReportCSV, column.Key)
		}
	}
	for index, row := range doc.Rows {
		if len(row) != len(doc.Columns) {
			return fmt.Errorf("%w: row %d has %d cells; column count is %d",
				ErrInvalidReportCSV, index+1, len(row), len(doc.Columns))
		}
		for cell, value := range row {
			if position, bad := firstReportCSVControlRune(value); bad {
				return fmt.Errorf("%w: row %d cell %d character %d is a control character",
					ErrInvalidReportCSV, index+1, cell+1, position+1)
			}
		}
	}
	return nil
}

// firstReportCSVControlRune 找出第一個不該出現在匯出檔裡的控制字元。
//
// 換行與 tab 是合法的：RFC 4180 的引號裡放得下它們，而多行的錯誤原文本來就該
// 保持多行。其餘的控制字元表示上游沒有把證據洗乾淨——那是上游的錯，不是這裡
// 該默默吸收的事。
func firstReportCSVControlRune(value string) (int, bool) {
	for index, r := range value {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return index, true
		}
	}
	return 0, false
}

// reportCSVCell 擋掉試算表公式。
//
// 試算表會把 `=`、`+`、`-`、`@` 開頭的儲存格當成公式執行，而這些欄位裝的是上游
// 原文——provider 名稱、錯誤訊息、機器名字都由別人決定。前面補一個單引號讓它
// 留在文字欄位裡。**每一格都要過**：只記得處理「看起來危險的那幾欄」，等於在等
// 下一個新欄位被忘記。
func reportCSVCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}
