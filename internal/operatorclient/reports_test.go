package operatorclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func reportClientIndex(t *testing.T, now time.Time) operator.ReportIndex {
	t.Helper()
	index, err := operator.ReportIndexFor(store.DefaultRetention(), now)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func reportClientTimeline(now time.Time) operator.MachineTimelineResult {
	to := now.Truncate(time.Second)
	if to.Before(now) {
		to = to.Add(time.Second)
	}
	result := operator.MachineTimelineResult{
		SchemaVersion: operator.MachineTimelineSchemaVersion, EvaluatedAt: now,
		MachineID: "m1", DisplayName: "samplehub1", Complete: true,
		Window: operator.MachineTimelineWindow{
			Days: operator.DefaultMachineTimelineDays, To: to,
			From: to.Add(-operator.DefaultMachineTimelineDays * 24 * time.Hour),
		},
		Entries: []operator.MachineTimelineEntry{
			{
				At: to.Add(-time.Hour), Kind: operator.MachineTimelineOperatorAction,
				Source: operator.MachineTimelineSourceAction, Summary: "diagnostic_noop",
				Detail: "ok，operator@example.com", Href: "/audit?action=diagnostic_noop&machine_id=m1",
			},
			{
				At: to.Add(-2 * time.Hour), Kind: operator.MachineTimelineJobOpened,
				Source: operator.MachineTimelineSourceJob, Summary: "開了一張工作單",
				Detail: "openclaw：openclaw", Href: "/jobs/j1",
			},
			{
				At: to.Add(-3 * time.Hour), Kind: operator.MachineTimelineStateEntered,
				Source: operator.MachineTimelineSourceState, Summary: "判定為 Degraded",
			},
			{
				At: to.Add(-4 * time.Hour), Kind: operator.MachineTimelineEnrolled,
				Source: operator.MachineTimelineSourceRegistry, Summary: "進了名冊",
			},
		},
	}
	counts := map[operator.MachineTimelineSource]int{}
	for _, entry := range result.Entries {
		counts[entry.Source]++
	}
	for _, source := range operator.MachineTimelineSources() {
		result.Sources = append(result.Sources, operator.MachineTimelineSourceRead{
			Source: source, Label: operator.MachineTimelineSourceLabel(source),
			Evidence: operator.MachineTimelineSourceEvidence(source),
			Count:    counts[source], Complete: true,
		})
	}
	result.Total = len(result.Entries)
	return result
}

func reportClientDaily(now time.Time, window time.Duration) operator.DailyReportResult {
	return operator.DailyReportResult{
		SchemaVersion: operator.DailyReportSchemaVersion,
		EvaluatedAt:   now,
		Since:         now.Add(-window),
		WindowSeconds: int64(window / time.Second),
		Body:          "4/5 報到\nalive; 0 changes\n",
	}
}

func TestTheDailyReportClientAsksForAndChecksTheExactWindow(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	client, asked := recordingReportServer(t, reportClientDaily(now, time.Hour))
	result, err := client.DailyReport(t.Context(), time.Hour)
	if err != nil {
		t.Fatalf("一致的每日早報被拒絕：%v", err)
	}
	if *asked != "/v1/operator/daily-report?since_seconds=3600" || result.Body == "" {
		t.Fatalf("asked=%q result=%+v", *asked, result)
	}
	if _, err := client.DailyReport(t.Context(), 2*time.Hour); err == nil {
		t.Fatal("問兩小時卻收下一小時的早報")
	}
	for _, window := range []time.Duration{0, time.Millisecond, operator.MaxDailyReportWindow + time.Second} {
		if _, err := client.DailyReport(t.Context(), window); err == nil {
			t.Errorf("window=%s 被送出去了", window)
		}
	}
}

func TestTheDailyReportClientRefusesAnUnsafeOrContradictoryDocument(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for name, edit := range map[string]func(*operator.DailyReportResult){
		"schema 不認得": func(r *operator.DailyReportResult) { r.SchemaVersion++ },
		"沒有評估時間":     func(r *operator.DailyReportResult) { r.EvaluatedAt = time.Time{} },
		"評估時間不是 UTC": func(r *operator.DailyReportResult) {
			r.EvaluatedAt = r.EvaluatedAt.In(time.FixedZone("elsewhere", 3600))
		},
		"評估時間不是整秒": func(r *operator.DailyReportResult) { r.EvaluatedAt = r.EvaluatedAt.Add(time.Nanosecond) },
		"起點不是 UTC": func(r *operator.DailyReportResult) {
			r.Since = r.Since.In(time.FixedZone("elsewhere", 3600))
		},
		"秒數不是 request": func(r *operator.DailyReportResult) { r.WindowSeconds++ },
		"起點與秒數對不起來":    func(r *operator.DailyReportResult) { r.Since = r.Since.Add(time.Second) },
		"沒有本文":         func(r *operator.DailyReportResult) { r.Body = "" },
		"沒有結尾換行":       func(r *operator.DailyReportResult) { r.Body = "alive" },
		"本文過大": func(r *operator.DailyReportResult) {
			r.Body = strings.Repeat("a", operator.MaxDailyReportBodyBytes) + "\n"
		},
		"終端機跳脫序列": func(r *operator.DailyReportResult) { r.Body = "alive\x1b[2J\n" },
		"格式控制字元":  func(r *operator.DailyReportResult) { r.Body = "alive\u202e\n" },
	} {
		t.Run(name, func(t *testing.T) {
			result := reportClientDaily(now, time.Hour)
			edit(&result)
			if _, err := complianceClientServer(t, result).DailyReport(t.Context(), time.Hour); err == nil {
				t.Fatal("不可信的每日早報被接受了")
			}
		})
	}
}

// recordingReportServer 回一份固定的文件，並且記下用戶端真的去問了哪一個網址。
func recordingReportServer(t *testing.T, body any) (*Client, *string) {
	t.Helper()
	asked := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return operatorClientForServer(t, server), &asked
}

func TestTheReportsClientAcceptsACatalogueThatAgreesWithItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	index, err := complianceClientServer(t, reportClientIndex(t, now)).Reports(t.Context())
	if err != nil {
		t.Fatalf("一致的報告清單被拒絕：%v", err)
	}
	if index.Total != len(operator.ReportKinds()) || len(index.Entries) != index.Total {
		t.Fatalf("index=%+v", index)
	}
}

// 清單的價值全在於它跟每一份報告講的是同一件事。一份自相矛盾的清單——說看得到
// 比它一次讀得到還遠、說分頁讀卻給了整份匯出——不是拿來顯示的東西，是拿來拒收的。
func TestTheReportsClientRefusesACatalogueThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for name, edit := range map[string]func(*operator.ReportIndex){
		"份數對不起來": func(i *operator.ReportIndex) { i.Total++ },
		"少了一份":   func(i *operator.ReportIndex) { i.Entries = i.Entries[1:] },
		"順序被換過": func(i *operator.ReportIndex) {
			i.Entries[0], i.Entries[1] = i.Entries[1], i.Entries[0]
		},
		"匯得出來的份數對不起來": func(i *operator.ReportIndex) { i.Exportable++ },
		"分頁讀卻給了匯出連結": func(i *operator.ReportIndex) {
			for index := range i.Entries {
				if i.Entries[index].Export == operator.ReportExportPagedOnly {
					i.Entries[index].ExportPath = "/reports/changes.csv"
					return
				}
			}
		},
		"看得到的比讀得到的還遠": func(i *operator.ReportIndex) {
			for index := range i.Entries {
				if i.Entries[index].Range.MaxDays > 0 && i.Entries[index].Horizon.Kind == operator.ReportHorizonRetention {
					i.Entries[index].Horizon.Days = i.Entries[index].Range.MaxDays + 1
					i.Entries[index].HorizonSentence = operator.ReportHorizonSentence(i.Entries[index].Horizon)
					return
				}
			}
		},
		"不受保留期限制卻給了天數": func(i *operator.ReportIndex) {
			for index := range i.Entries {
				if i.Entries[index].Horizon.Kind == operator.ReportHorizonUnbounded {
					i.Entries[index].Horizon.Days = 30
					return
				}
			}
		},
		"換了一種說法": func(i *operator.ReportIndex) {
			i.Entries[0].HorizonSentence = "看得到很久以前。"
		},
		"範圍認不得": func(i *operator.ReportIndex) { i.Entries[0].Scope = "somewhere" },
		"匯出狀態認不得": func(i *operator.ReportIndex) {
			i.Entries[0].Export = "maybe"
		},
		"沒有評估時間": func(i *operator.ReportIndex) { i.EvaluatedAt = time.Time{} },
		"少了一句交代": func(i *operator.ReportIndex) { i.Entries[0].Question = "" },
		"名稱裡有終端機跳脫序列": func(i *operator.ReportIndex) {
			i.Entries[0].Title = "變更\x1b[2J"
		},
	} {
		t.Run(name, func(t *testing.T) {
			index := reportClientIndex(t, now)
			edit(&index)
			if _, err := complianceClientServer(t, index).Reports(t.Context()); err == nil {
				t.Fatal("自相矛盾的報告清單被接受了")
			}
		})
	}
}

func TestTheTimelineClientAcceptsAStoryThatAgreesWithItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	result, err := complianceClientServer(t, reportClientTimeline(now)).
		MachineTimeline(t.Context(), "m1", 0)
	if err != nil {
		t.Fatalf("一致的時間軸被拒絕：%v", err)
	}
	if result.Total != 4 || len(result.Sources) != len(operator.MachineTimelineSources()) {
		t.Fatalf("result=%+v", result)
	}
}

// 時間軸唯一的用處是「照時間排好的一條」。排錯、算錯、或者說自己讀完了卻有來源
// 沒讀完——每一個都會讓操作員把一段沒有根據的話當成事件的全部。
func TestTheTimelineClientRefusesAStoryThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for name, edit := range map[string]func(*operator.MachineTimelineResult){
		"回的是另一台": func(r *operator.MachineTimelineResult) { r.MachineID = "m2" },
		"窗的長度跟天數對不起來": func(r *operator.MachineTimelineResult) {
			r.Window.From = r.Window.From.Add(time.Hour)
		},
		"窗在評估之前就結束": func(r *operator.MachineTimelineResult) {
			r.Window.To = r.Window.To.Add(-time.Hour)
			r.Window.From = r.Window.From.Add(-time.Hour)
		},
		"有一列在窗外面": func(r *operator.MachineTimelineResult) {
			r.Entries[3].At = r.Window.From.Add(-time.Hour)
		},
		"排序反過來": func(r *operator.MachineTimelineResult) {
			r.Entries[0], r.Entries[3] = r.Entries[3], r.Entries[0]
		},
		"總數跟清單對不起來": func(r *operator.MachineTimelineResult) { r.Total++ },
		"來源的列數跟清單對不起來": func(r *operator.MachineTimelineResult) {
			r.Sources[0].Count++
		},
		"總數對得起來但來源之間分錯": func(r *operator.MachineTimelineResult) {
			r.Sources[0].Count++
			r.Sources[1].Count--
		},
		"種類跟來源對不起來但列數還對得起來": func(r *operator.MachineTimelineResult) {
			r.Entries[0].Kind = operator.MachineTimelineJobOpened
		},
		"說讀完了卻有來源沒讀完": func(r *operator.MachineTimelineResult) {
			r.Sources[0].Complete = false
			r.Sources[0].NextStep = operator.MachineTimelineIncompleteNextStep(r.Sources[0].Source)
		},
		"沒讀完卻不說下一步": func(r *operator.MachineTimelineResult) {
			r.Sources[0].Complete, r.Complete = false, false
		},
		"種類跟來源對不起來": func(r *operator.MachineTimelineResult) {
			r.Entries[0].Source = operator.MachineTimelineSourceRegistry
		},
		"種類認不得":  func(r *operator.MachineTimelineResult) { r.Entries[0].Kind = "something" },
		"來源少了一個": func(r *operator.MachineTimelineResult) { r.Sources = r.Sources[1:] },
		"來源順序被換過": func(r *operator.MachineTimelineResult) {
			r.Sources[0], r.Sources[1] = r.Sources[1], r.Sources[0]
		},
		"來源換了名字":     func(r *operator.MachineTimelineResult) { r.Sources[0].Label = "註冊表" },
		"有一列沒說發生了什麼": func(r *operator.MachineTimelineResult) { r.Entries[0].Summary = " " },
		"證據連結不是這個 Hub 上的路徑": func(r *operator.MachineTimelineResult) {
			r.Entries[0].Href = "https://example.com/audit"
		},
		"沒有顯示名稱": func(r *operator.MachineTimelineResult) { r.DisplayName = "" },
	} {
		t.Run(name, func(t *testing.T) {
			result := reportClientTimeline(now)
			edit(&result)
			if _, err := complianceClientServer(t, result).MachineTimeline(t.Context(), "m1", 0); err == nil {
				t.Fatal("自相矛盾的時間軸被接受了")
			}
		})
	}
}

// 問的是哪一個範圍，回來的就要是哪一個範圍。回另一個範圍而用戶端照收，操作員會
// 拿一份 7 天的資料當成 30 天的來看。
func TestTheTimelineClientAsksForTheRangeItWasGivenAndChecksWhatComesBack(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	client, asked := recordingReportServer(t, reportClientTimeline(now))
	if _, err := client.MachineTimeline(t.Context(), "m1", 0); err != nil {
		t.Fatalf("預設範圍被拒絕：%v", err)
	}
	if *asked != "/v1/operator/machines/m1/timeline" {
		t.Errorf("預設範圍問了 %q", *asked)
	}
	if _, err := client.MachineTimeline(t.Context(), "m1", operator.DefaultMachineTimelineDays); err != nil {
		t.Fatalf("同一個範圍被拒絕：%v", err)
	}
	if *asked != "/v1/operator/machines/m1/timeline?days=7" {
		t.Errorf("指定範圍問了 %q", *asked)
	}
	if _, err := client.MachineTimeline(t.Context(), "m1", 30); err == nil {
		t.Error("問 30 天回 7 天被接受了")
	}
	for _, days := range []int{-1, operator.MaxMachineTimelineDays + 1} {
		if _, err := client.MachineTimeline(t.Context(), "m1", days); err == nil {
			t.Errorf("days=%d 被送出去了", days)
		}
	}
	if _, err := client.MachineTimeline(t.Context(), "", 0); err == nil {
		t.Error("沒有機器代號也送出去了")
	}
}
