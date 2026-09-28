package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// DailyReport reads the exact notification body rendered by the running Hub.
// It does not send the notification or create a notification receipt.
func (c *Client) DailyReport(ctx context.Context, window time.Duration) (operator.DailyReportResult, error) {
	var out operator.DailyReportResult
	if window < time.Second || window > operator.MaxDailyReportWindow || window%time.Second != 0 {
		return out, fmt.Errorf("operator client: daily report window 必須是 1s 到 %s 的整秒", operator.MaxDailyReportWindow)
	}
	query := url.Values{"since_seconds": []string{strconv.FormatInt(int64(window/time.Second), 10)}}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/daily-report?"+query.Encode(), nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: daily report returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "daily report", &out); err != nil {
		return out, err
	}
	if err := validateDailyReport(out, window); err != nil {
		return out, err
	}
	return out, nil
}

func validateDailyReport(result operator.DailyReportResult, requested time.Duration) error {
	if result.SchemaVersion != operator.DailyReportSchemaVersion || result.EvaluatedAt.IsZero() ||
		result.EvaluatedAt.Location() != time.UTC || result.EvaluatedAt.Nanosecond() != 0 ||
		result.Since.IsZero() || result.Since.Location() != time.UTC || result.Since.Nanosecond() != 0 {
		return errors.New("operator client: daily report identity 不一致")
	}
	if result.WindowSeconds != int64(requested/time.Second) ||
		!result.Since.Equal(result.EvaluatedAt.Add(-requested)) {
		return errors.New("operator client: daily report window 與 request 不一致")
	}
	if result.Body == "" || len(result.Body) > operator.MaxDailyReportBodyBytes || !utf8.ValidString(result.Body) ||
		!strings.HasSuffix(result.Body, "\n") {
		return errors.New("operator client: daily report 本文為空、過大、編碼不合法或沒有結尾換行")
	}
	for _, char := range result.Body {
		if char != '\n' && (unicode.IsControl(char) || unicode.Is(unicode.Cf, char)) {
			return errors.New("operator client: daily report 本文含控制或格式字元")
		}
	}
	return nil
}

// Reports reads the report catalogue: what each report answers, how far back it
// can still see under the retention policy in force, and whether it can be
// taken away whole.
//
// 用戶端自己再驗一次這份清單的自洽性。落地頁的每一句話都是由同一組函式產生的，
// 所以一份說了別的句子的回應不是拿來顯示的東西，是拿來拒收的：那表示對面的
// Hub 對「這份報告看得到多遠」有第二種說法。
func (c *Client) Reports(ctx context.Context) (operator.ReportIndex, error) {
	var out operator.ReportIndex
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/reports", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: reports returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "reports", &out); err != nil {
		return out, err
	}
	if err := validateReportIndex(out); err != nil {
		return out, err
	}
	return out, nil
}

// MachineTimeline reads one machine's ordered story. days=0 asks for the
// Hub's default window.
func (c *Client) MachineTimeline(ctx context.Context, machineID string, days int) (
	operator.MachineTimelineResult, error,
) {
	var out operator.MachineTimelineResult
	if days < 0 || days > operator.MaxMachineTimelineDays {
		return out, fmt.Errorf("operator client: 時間軸範圍 %d 天超出 1–%d",
			days, operator.MaxMachineTimelineDays)
	}
	suffix := "/timeline"
	if days > 0 {
		suffix += "?" + url.Values{"days": []string{strconv.Itoa(days)}}.Encode()
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodGet, machineID, suffix, nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: machine timeline returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "machine timeline", &out); err != nil {
		return out, err
	}
	if err := validateMachineTimeline(out, machineID, days); err != nil {
		return out, err
	}
	return out, nil
}

func validateReportIndex(index operator.ReportIndex) error {
	kinds := operator.ReportKinds()
	if index.SchemaVersion != operator.ReportIndexSchemaVersion || index.EvaluatedAt.IsZero() ||
		index.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: report index identity 不一致")
	}
	if index.Total != len(kinds) || len(index.Entries) != index.Total {
		return fmt.Errorf("operator client: report index 說有 %d 份，清單有 %d 份，這個 Hub 做得出 %d 份",
			index.Total, len(index.Entries), len(kinds))
	}
	exportable := 0
	for position, entry := range index.Entries {
		if entry.Kind != kinds[position] {
			return fmt.Errorf("operator client: report index 第 %d 份是 %q，固定順序上是 %q",
				position+1, entry.Kind, kinds[position])
		}
		if err := validateReportEntry(entry); err != nil {
			return err
		}
		if entry.Export == operator.ReportExportWholeWindow {
			exportable++
		}
	}
	if exportable != index.Exportable {
		return fmt.Errorf("operator client: report index 說 %d 份匯得出整份，逐項數出 %d 份",
			index.Exportable, exportable)
	}
	return nil
}

func validateReportEntry(entry operator.ReportEntry) error {
	if strings.TrimSpace(entry.Title) == "" || strings.TrimSpace(entry.Question) == "" ||
		strings.TrimSpace(entry.Evidence) == "" || !strings.HasPrefix(entry.Path, "/") {
		return fmt.Errorf("operator client: report %q 少了名稱、回答什麼、證據或路徑", entry.Kind)
	}
	if entry.Scope != operator.ReportScopeFleet && entry.Scope != operator.ReportScopeMachine {
		return fmt.Errorf("operator client: report %q 的範圍是 %q", entry.Kind, entry.Scope)
	}
	// 這些字串會原樣印到終端機。控制字元在那裡是跳脫序列，不是文字。
	for name, value := range map[string]string{
		"report title": entry.Title, "report question": entry.Question,
		"report evidence": entry.Evidence, "report path": entry.Path,
	} {
		if err := validateMachineClientText(name, value, 512); err != nil {
			return err
		}
	}
	switch entry.Export {
	case operator.ReportExportWholeWindow:
	case operator.ReportExportPagedOnly:
		if entry.ExportPath != "" {
			return fmt.Errorf("operator client: report %q 說沒有整份匯出卻給了 %q", entry.Kind, entry.ExportPath)
		}
	default:
		return fmt.Errorf("operator client: report %q 的匯出狀態是 %q", entry.Kind, entry.Export)
	}
	if entry.ExportPath != "" && !strings.HasPrefix(entry.ExportPath, "/") {
		return fmt.Errorf("operator client: report %q 的匯出路徑是 %q", entry.Kind, entry.ExportPath)
	}
	if entry.Range.DefaultDays < 0 || entry.Range.MaxDays < 0 ||
		entry.Range.MaxDays > 0 && (entry.Range.DefaultDays < 1 || entry.Range.DefaultDays > entry.Range.MaxDays) {
		return fmt.Errorf("operator client: report %q 的範圍 %+v 站不住", entry.Kind, entry.Range)
	}
	switch entry.Horizon.Kind {
	case operator.ReportHorizonUnbounded, operator.ReportHorizonPerCategory,
		operator.ReportHorizonNewestKept:
		if entry.Horizon.Days != 0 {
			return fmt.Errorf("operator client: report %q 說看得回去多遠是 %q 卻給了 %d 天",
				entry.Kind, entry.Horizon.Kind, entry.Horizon.Days)
		}
	case operator.ReportHorizonRetention:
		if entry.Horizon.Days < 1 ||
			entry.Range.MaxDays > 0 && entry.Horizon.Days > entry.Range.MaxDays {
			return fmt.Errorf("operator client: report %q 說看得到 %d 天，但它一次最多問 %d 天",
				entry.Kind, entry.Horizon.Days, entry.Range.MaxDays)
		}
	default:
		return fmt.Errorf("operator client: report %q 的保留期交代是 %q", entry.Kind, entry.Horizon.Kind)
	}
	// 一份不是時間窗的報告不可以同時宣稱一個天數。
	if entry.Range.Instant && (entry.Range.DefaultDays != 0 || entry.Range.MaxDays != 0) {
		return fmt.Errorf("operator client: report %q 說不是一段期間卻給了天數 %+v",
			entry.Kind, entry.Range)
	}
	// 這四句是由同一組函式產生的。對面換了一種說法，就是對同一件事有第二種解釋。
	if entry.ScopeSentence != operator.ReportScopeSentence(entry.Scope) ||
		entry.RangeSentence != operator.ReportRangeSentence(entry.Range) ||
		entry.HorizonSentence != operator.ReportHorizonSentence(entry.Horizon) ||
		entry.ExportSentence != operator.ReportExportSentence(entry.Export, entry.Scope, entry.Range) {
		return fmt.Errorf("operator client: report %q 的交代跟它自己的欄位對不起來", entry.Kind)
	}
	return nil
}

func validateMachineTimeline(result operator.MachineTimelineResult, machineID string, days int) error {
	if result.SchemaVersion != operator.MachineTimelineSchemaVersion || result.MachineID != machineID ||
		strings.TrimSpace(result.DisplayName) == "" || result.EvaluatedAt.IsZero() ||
		result.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: machine timeline identity 不一致")
	}
	if err := validateMachineClientText("machine timeline display_name", result.DisplayName, 256); err != nil {
		return err
	}
	if err := validateMachineTimelineWindow(result, days); err != nil {
		return err
	}
	counted, err := validateMachineTimelineSources(result)
	if err != nil {
		return err
	}
	if result.Total != len(result.Entries) || counted != result.Total {
		return fmt.Errorf("operator client: machine timeline 說 %d 列，清單有 %d 列，各來源加起來 %d 列",
			result.Total, len(result.Entries), counted)
	}
	return validateMachineTimelineEntries(result)
}

func validateMachineTimelineWindow(result operator.MachineTimelineResult, days int) error {
	window := result.Window
	if days > 0 && window.Days != days {
		return fmt.Errorf("operator client: 問的是 %d 天，回來的是 %d 天", days, window.Days)
	}
	if window.Days < 1 || window.Days > operator.MaxMachineTimelineDays {
		return fmt.Errorf("operator client: machine timeline 的範圍是 %d 天", window.Days)
	}
	if window.From.Location() != time.UTC || window.To.Location() != time.UTC {
		return errors.New("operator client: machine timeline 的窗不是 UTC")
	}
	if window.To.Sub(window.From) != time.Duration(window.Days)*24*time.Hour {
		return fmt.Errorf("operator client: machine timeline 說 %d 天，窗是 %v",
			window.Days, window.To.Sub(window.From))
	}
	// 評估的那一刻必須在窗裡面，否則「最近 N 天」少掉的正好是剛剛發生的事。
	if window.To.Before(result.EvaluatedAt) || window.To.Sub(result.EvaluatedAt) >= time.Second {
		return fmt.Errorf("operator client: machine timeline 的窗結束在 %v，評估時間是 %v",
			window.To, result.EvaluatedAt)
	}
	return nil
}

func validateMachineTimelineSources(result operator.MachineTimelineResult) (int, error) {
	sources := operator.MachineTimelineSources()
	if len(result.Sources) != len(sources) {
		return 0, fmt.Errorf("operator client: machine timeline 交代了 %d 個來源，應該有 %d 個",
			len(result.Sources), len(sources))
	}
	counted, complete := 0, true
	for position, read := range result.Sources {
		if read.Source != sources[position] {
			return 0, fmt.Errorf("operator client: machine timeline 第 %d 個來源是 %q，固定順序上是 %q",
				position+1, read.Source, sources[position])
		}
		if read.Label != operator.MachineTimelineSourceLabel(read.Source) ||
			read.Evidence != operator.MachineTimelineSourceEvidence(read.Source) {
			return 0, fmt.Errorf("operator client: 來源 %q 的名字或證據跟它自己對不起來", read.Source)
		}
		if read.Count < 0 {
			return 0, fmt.Errorf("operator client: 來源 %q 說讀到 %d 列", read.Source, read.Count)
		}
		wantNextStep := ""
		if !read.Complete {
			wantNextStep = operator.MachineTimelineIncompleteNextStep(read.Source)
			complete = false
		}
		if read.NextStep != wantNextStep {
			return 0, fmt.Errorf("operator client: 來源 %q 的下一步跟它讀完沒讀完對不起來", read.Source)
		}
		counted += read.Count
	}
	if result.Complete != complete {
		return 0, errors.New("operator client: machine timeline 的完整性跟逐個來源對不起來")
	}
	return counted, nil
}

func validateMachineTimelineEntries(result operator.MachineTimelineResult) error {
	counts := map[operator.MachineTimelineSource]int{}
	previous := time.Time{}
	for index, entry := range result.Entries {
		source, known := operator.MachineTimelineSourceOf(entry.Kind)
		if !known || source != entry.Source {
			return fmt.Errorf("operator client: 第 %d 列的種類 %q 與來源 %q 對不起來",
				index+1, entry.Kind, entry.Source)
		}
		if entry.At.IsZero() || entry.At.Location() != time.UTC {
			return fmt.Errorf("operator client: 第 %d 列沒有 UTC 時間", index+1)
		}
		if entry.At.Before(result.Window.From) || entry.At.After(result.Window.To) {
			return fmt.Errorf("operator client: 第 %d 列的時間 %v 在窗 %v–%v 外面",
				index+1, entry.At, result.Window.From, result.Window.To)
		}
		if index > 0 && entry.At.After(previous) {
			return fmt.Errorf("operator client: 第 %d 列比它前面那一列新", index+1)
		}
		previous = entry.At
		if strings.TrimSpace(entry.Summary) == "" {
			return fmt.Errorf("operator client: 第 %d 列沒有說發生了什麼", index+1)
		}
		if err := validateMachineClientText("machine timeline summary", entry.Summary, 512); err != nil {
			return err
		}
		if entry.Detail != "" {
			if err := validateMachineClientText("machine timeline detail", entry.Detail, 2048); err != nil {
				return err
			}
		}
		if entry.Href != "" && !strings.HasPrefix(entry.Href, "/") {
			return fmt.Errorf("operator client: 第 %d 列的證據連結是 %q", index+1, entry.Href)
		}
		counts[entry.Source]++
	}
	for _, read := range result.Sources {
		if counts[read.Source] != read.Count {
			return fmt.Errorf("operator client: 來源 %q 說 %d 列，清單上有 %d 列",
				read.Source, read.Count, counts[read.Source])
		}
	}
	return nil
}
