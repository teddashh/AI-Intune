package operator

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func reportIndexAt(t *testing.T, policy store.RetentionPolicy) ReportIndex {
	t.Helper()
	index, err := ReportIndexFor(policy, time.Date(2026, 9, 12, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func reportEntry(t *testing.T, index ReportIndex, kind ReportKind) ReportEntry {
	t.Helper()
	for _, entry := range index.Entries {
		if entry.Kind == kind {
			return entry
		}
	}
	t.Fatalf("落地頁沒有 %s：%+v", kind, index.Entries)
	return ReportEntry{}
}

// 落地頁在點進去之前就要答完三件事：這份報告算的是什麼、看得到多遠、能不能把
// 列帶走。少答任何一件，這一頁就只是一個選單。
func TestEveryReportSaysWhatItAnswersHowFarBackAndWhetherItExports(t *testing.T) {
	index := reportIndexAt(t, store.DefaultRetention())
	if index.SchemaVersion != ReportIndexSchemaVersion || index.Total != len(ReportKinds()) ||
		len(index.Entries) != index.Total {
		t.Fatalf("index=%+v", index)
	}
	seen := map[ReportKind]bool{}
	for _, entry := range index.Entries {
		if seen[entry.Kind] {
			t.Fatalf("%s 出現兩次", entry.Kind)
		}
		seen[entry.Kind] = true
		if entry.Title == "" || entry.Question == "" || entry.Evidence == "" || entry.Path == "" {
			t.Errorf("%s 沒有說清楚自己是什麼：%+v", entry.Kind, entry)
		}
		if entry.ScopeSentence == "" || entry.RangeSentence == "" ||
			entry.HorizonSentence == "" || entry.ExportSentence == "" {
			t.Errorf("%s 少了一句交代：%+v", entry.Kind, entry)
		}
		if entry.Scope != ReportScopeFleet && entry.Scope != ReportScopeMachine {
			t.Errorf("%s 的範圍 %q 不是認得的", entry.Kind, entry.Scope)
		}
	}
	for _, kind := range ReportKinds() {
		if !seen[kind] {
			t.Errorf("落地頁漏了 %s", kind)
		}
	}
}

// 保留期會把量測清掉，所以「看得到多遠」是現行保留期的函數，不是寫死的一句話。
// 把保留期調短，這一頁必須跟著改。
func TestTheHorizonFollowsTheRetentionPolicyInForce(t *testing.T) {
	policy := store.DefaultRetention()
	wide := reportEntry(t, reportIndexAt(t, policy), ReportChanges)
	if wide.Horizon.Kind != ReportHorizonRetention || wide.Horizon.Days != 30 {
		t.Fatalf("預設保留期下的變更報告=%+v", wide.Horizon)
	}
	policy.Observations = 10 * 24 * time.Hour
	narrow := reportEntry(t, reportIndexAt(t, policy), ReportChanges)
	if narrow.Horizon.Kind != ReportHorizonRetention || narrow.Horizon.Days != 10 {
		t.Fatalf("保留期縮短之後=%+v", narrow.Horizon)
	}
	if !strings.Contains(narrow.HorizonSentence, "10") {
		t.Errorf("文案沒有跟著保留期走：%q", narrow.HorizonSentence)
	}
}

// 資料揭露不是一段期間，而且它一次講十三類各自不同的保留期。借其中一個天數印在
// 落地頁上，對另外十二類都是假的；說「範圍由你指定」則是在承諾一個它沒有的旋鈕。
func TestTheInventoryReportIsNeitherAWindowNorOneRetention(t *testing.T) {
	entry := reportEntry(t, reportIndexAt(t, store.DefaultRetention()), ReportMachineData)
	if !entry.Range.Instant || entry.Range.DefaultDays != 0 || entry.Range.MaxDays != 0 {
		t.Fatalf("range=%+v", entry.Range)
	}
	if strings.Contains(entry.RangeSentence, "指定") || !strings.Contains(entry.RangeSentence, "現在手上") {
		t.Errorf("範圍的文案=%q", entry.RangeSentence)
	}
	if entry.Horizon.Kind != ReportHorizonPerCategory || entry.Horizon.Days != 0 {
		t.Fatalf("horizon=%+v", entry.Horizon)
	}
	if !strings.Contains(entry.HorizonSentence, "逐類") {
		t.Errorf("看得回去多遠的文案=%q", entry.HorizonSentence)
	}
	if entry.Export != ReportExportWholeWindow || entry.Scope != ReportScopeMachine {
		t.Errorf("entry=%+v", entry)
	}
}

// 票證的帳本留 400 天，但票證報告一次最多問 30 天。印保留期那個數字會承諾一份
// 這份報告從來給不出來的資料。
func TestAReportNeverPromisesMoreThanItWillEverRead(t *testing.T) {
	tickets := reportEntry(t, reportIndexAt(t, store.DefaultRetention()), ReportTickets)
	if tickets.Horizon.Days != MaxTicketReadDays {
		t.Fatalf("票證報告說看得到 %d 天，但它一次最多問 %d 天",
			tickets.Horizon.Days, MaxTicketReadDays)
	}
	if tickets.Range.MaxDays != MaxTicketReadDays || tickets.Range.DefaultDays != DefaultTicketReadDays {
		t.Fatalf("票證報告的範圍=%+v", tickets.Range)
	}
}

// 稽核與時間軸讀的那些列沒有任何一條保留期在清，所以它們的答案是「都還在」，不是
// 一個借來的天數。
func TestReportsNothingPrunesSayTheirRowsAreStillThere(t *testing.T) {
	index := reportIndexAt(t, store.DefaultRetention())
	for _, kind := range []ReportKind{ReportAudit, ReportMachineTimeline} {
		entry := reportEntry(t, index, kind)
		if entry.Horizon.Kind != ReportHorizonUnbounded || entry.Horizon.Days != 0 {
			t.Errorf("%s 說自己受保留期限制：%+v", kind, entry.Horizon)
		}
		if strings.Contains(entry.HorizonSentence, "清掉") {
			t.Errorf("%s 的文案講了保留期：%q", kind, entry.HorizonSentence)
		}
	}
}

// 只有在範圍裡是完整的報告才匯得出來。一份分頁讀的報告匯出的是當下那一頁，而那
// 個檔案打開之後看起來跟整份報告一模一樣。
func TestOnlyAReportThatIsWholeInsideItsRangeOffersAnExport(t *testing.T) {
	index := reportIndexAt(t, store.DefaultRetention())
	exportable := 0
	for _, entry := range index.Entries {
		switch entry.Export {
		case ReportExportWholeWindow:
			exportable++
			if entry.Scope == ReportScopeFleet && entry.ExportPath == "" {
				t.Errorf("%s 說匯得出來卻沒有連結", entry.Kind)
			}
		case ReportExportPagedOnly:
			if entry.ExportPath != "" {
				t.Errorf("%s 說沒有整份匯出卻給了連結 %q", entry.Kind, entry.ExportPath)
			}
		default:
			t.Errorf("%s 的匯出狀態 %q 不是認得的", entry.Kind, entry.Export)
		}
	}
	if exportable != index.Exportable {
		t.Fatalf("逐項數出 %d 份匯得出來，落地頁說 %d 份", exportable, index.Exportable)
	}
	if paged := reportEntry(t, index, ReportChanges); paged.Export != ReportExportPagedOnly {
		t.Errorf("分頁讀的變更報告說它匯得出整份：%+v", paged)
	}
	if audit := reportEntry(t, index, ReportAudit); audit.Export != ReportExportPagedOnly {
		t.Errorf("分頁讀的稽核說它匯得出整份：%+v", audit)
	}
}

// 每一句匯出說明講的必須是它自己那一份報告：不是一段期間的報告不能說「這個範圍
// 裡的全部」，整個機隊的報告不能說「只含那一台」。一句借來的說明會在落地頁上把
// 一份報告講成它不是的形狀，而操作員是照那一句決定要不要點下去的。
func TestEveryExportSentenceDescribesItsOwnReport(t *testing.T) {
	index := reportIndexAt(t, store.DefaultRetention())
	instant, machine := 0, 0
	for _, entry := range index.Entries {
		if entry.Export != ReportExportWholeWindow {
			continue
		}
		if entry.Range.Instant {
			instant++
			if strings.Contains(entry.ExportSentence, "範圍") {
				t.Errorf("%s 不是一段期間，匯出說明卻說範圍：%q", entry.Kind, entry.ExportSentence)
			}
		} else if !strings.Contains(entry.ExportSentence, "範圍") {
			t.Errorf("%s 是一段期間，匯出說明卻沒說範圍：%q", entry.Kind, entry.ExportSentence)
		}
		switch entry.Scope {
		case ReportScopeMachine:
			machine++
			if !strings.Contains(entry.ExportSentence, "只含那一台") {
				t.Errorf("%s 一次只匯一台，說明卻沒講：%q", entry.Kind, entry.ExportSentence)
			}
		case ReportScopeFleet:
			if strings.Contains(entry.ExportSentence, "那一台") {
				t.Errorf("%s 是整個機隊一份，說明卻在講某一台：%q", entry.Kind, entry.ExportSentence)
			}
		}
	}
	if instant == 0 || machine == 0 {
		t.Fatalf("沒有 instant（%d）或單機（%d）的匯出，這支測試沒有在測東西", instant, machine)
	}
}

// 單機的報告要先挑一台，所以落地頁上它指向的是機器清單，而它的匯出連結只存在於
// 那一台的頁面上。指向一個要先填空才成立的網址是一條死連結。
func TestAPerMachineReportSendsYouToPickAMachineFirst(t *testing.T) {
	timeline := reportEntry(t, reportIndexAt(t, store.DefaultRetention()), ReportMachineTimeline)
	if timeline.Scope != ReportScopeMachine || timeline.Path != "/machines" {
		t.Fatalf("timeline=%+v", timeline)
	}
	if timeline.ExportPath != "" {
		t.Errorf("單機報告在落地頁給了一個沒有機器的匯出連結：%q", timeline.ExportPath)
	}
	if timeline.Export != ReportExportWholeWindow {
		t.Errorf("單機報告說它匯不出整份：%+v", timeline.Export)
	}
}

// 一份說不出自己讀得到多遠的落地頁沒有價值，所以它拒絕一個它站不住的保留期。
func TestTheIndexRefusesAPolicyItCannotDescribe(t *testing.T) {
	if _, err := ReportIndexFor(store.DefaultRetention(), time.Time{}); err == nil {
		t.Error("沒有評估時間也給了落地頁")
	}
	broken := store.DefaultRetention()
	broken.Observations = time.Minute
	if _, err := ReportIndexFor(broken, time.Now().UTC()); err == nil {
		t.Error("一個會切進畫面讀取範圍的保留期被接受了")
	}
}

// 落地頁上的每一條路都要走得到，而且匯出的那一條要真的匯得出東西。一個指錯地方
// 的入口，跟沒有那份報告是同一件事。
func TestTheInstallReportIsOnTheLandingPageAndItsLinksAreTheRealOnes(t *testing.T) {
	entry := reportEntry(t, reportIndexAt(t, store.DefaultRetention()), ReportInstall)
	if entry.Path != "/reports/install" {
		t.Errorf("落地頁指到 %q", entry.Path)
	}
	if entry.ExportPath != InstallReportExportPath() {
		t.Errorf("匯出指到 %q，實際的匯出路徑是 %q", entry.ExportPath, InstallReportExportPath())
	}
	// ⚠ 它讀的是每一台最新那一筆觀測，跟軟體清查受同一個保留期的限制。
	if entry.Horizon.Kind != ReportHorizonNewestKept {
		t.Errorf("看得到多遠講錯了：%+v", entry.Horizon)
	}
	if entry.Scope != ReportScopeFleet {
		t.Errorf("範圍講錯了：%q", entry.Scope)
	}
}

func TestTheReportIndexSentencesSayTheseExactWords(t *testing.T) {
	t.Run("scope", func(t *testing.T) {
		tests := []struct {
			name  string
			scope ReportScope
			want  string
		}{
			{name: "fleet", scope: ReportScopeFleet, want: "整個機隊一份。"},
			{name: "machine", scope: ReportScopeMachine, want: "一台機器一份，先挑一台。"},
			{name: "unknown", scope: ReportScope("nope"), want: ""},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got := ReportScopeSentence(test.scope)
				if got != test.want {
					t.Errorf("範圍句實際是 %q，期望是 %q", got, test.want)
				}
			})
		}
	})

	t.Run("range", func(t *testing.T) {
		tests := []struct {
			name  string
			value ReportRange
			want  string
		}{
			{name: "instant", value: ReportRange{Instant: true}, want: "不是一段期間，是 Hub 現在手上的全部。"},
			{name: "operator specified", value: ReportRange{}, want: "範圍由你指定。"},
			{name: "default and maximum", value: ReportRange{DefaultDays: 7, MaxDays: 30}, want: "預設 7 天，最多 30 天。"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got := ReportRangeSentence(test.value)
				if got != test.want {
					t.Errorf("期間句實際是 %q，期望是 %q", got, test.want)
				}
			})
		}
	})

	t.Run("horizon", func(t *testing.T) {
		tests := []struct {
			name  string
			value ReportHorizon
			want  string
		}{
			{name: "unbounded", value: ReportHorizon{Kind: ReportHorizonUnbounded}, want: "從 Hub 的第一天起都還在。"},
			{name: "retention", value: ReportHorizon{Kind: ReportHorizonRetention, Days: 90}, want: "看得到 90 天，再往前的已經被保留期清掉了。"},
			{name: "per category", value: ReportHorizon{Kind: ReportHorizonPerCategory}, want: "每一類資料各自不同，這份報告會逐類講。"},
			{name: "newest kept", value: ReportHorizon{Kind: ReportHorizonNewestKept}, want: "讀的是每一台最新的那一筆，保留期永遠留著那一筆；再往前的版號已經被清掉了。"},
			{name: "unknown", value: ReportHorizon{Kind: ReportHorizonKind("nope")}, want: ""},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got := ReportHorizonSentence(test.value)
				if got != test.want {
					t.Errorf("可追溯期間句實際是 %q，期望是 %q", got, test.want)
				}
			})
		}
	})

	t.Run("export", func(t *testing.T) {
		tests := []struct {
			name       string
			state      ReportExportState
			scope      ReportScope
			rangeValue ReportRange
			want       string
		}{
			{name: "fleet window", state: ReportExportWholeWindow, scope: ReportScopeFleet, want: "匯出的就是這個範圍裡的全部。"},
			{name: "machine window", state: ReportExportWholeWindow, scope: ReportScopeMachine, want: "匯出的就是這個範圍裡的全部，只含那一台。"},
			{name: "fleet instant", state: ReportExportWholeWindow, scope: ReportScopeFleet, rangeValue: ReportRange{Instant: true}, want: "匯出的就是現在手上的全部。"},
			{name: "machine instant", state: ReportExportWholeWindow, scope: ReportScopeMachine, rangeValue: ReportRange{Instant: true}, want: "匯出的就是現在手上的全部，只含那一台。"},
			{name: "paged only", state: ReportExportPagedOnly, scope: ReportScopeFleet, want: "這份報告一次讀一頁，沒有整份匯出。"},
			{name: "unknown", state: ReportExportState("nope"), scope: ReportScopeFleet, want: ""},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got := ReportExportSentence(test.state, test.scope, test.rangeValue)
				if got != test.want {
					t.Errorf("匯出句實際是 %q，期望是 %q", got, test.want)
				}
			})
		}
	})
}

func TestEveryReportEntryCarriesThePinnedSentences(t *testing.T) {
	index := reportIndexAt(t, store.DefaultRetention())
	scopeSentences := map[ReportScope]string{
		ReportScopeFleet:   "整個機隊一份。",
		ReportScopeMachine: "一台機器一份，先挑一台。",
	}
	horizonSentences := map[ReportHorizonKind]string{
		ReportHorizonUnbounded:   "從 Hub 的第一天起都還在。",
		ReportHorizonPerCategory: "每一類資料各自不同，這份報告會逐類講。",
		ReportHorizonNewestKept:  "讀的是每一台最新的那一筆，保留期永遠留著那一筆；再往前的版號已經被清掉了。",
	}

	for _, entry := range index.Entries {
		t.Run(string(entry.Kind), func(t *testing.T) {
			wantScope, ok := scopeSentences[entry.Scope]
			if !ok {
				t.Errorf("%s 這份報告帶了一個沒有人逐字讀過的句子種類：scope=%q（實際句子 %q，期望句子無法決定）", entry.Kind, entry.Scope, entry.ScopeSentence)
			} else if entry.ScopeSentence != wantScope {
				t.Errorf("%s 的範圍句實際是 %q，期望是 %q", entry.Kind, entry.ScopeSentence, wantScope)
			}

			var wantHorizon string
			if entry.Horizon.Kind == ReportHorizonRetention {
				wantHorizon = fmt.Sprintf("看得到 %d 天，再往前的已經被保留期清掉了。", entry.Horizon.Days)
			} else {
				var known bool
				wantHorizon, known = horizonSentences[entry.Horizon.Kind]
				if !known {
					t.Errorf("%s 這份報告帶了一個沒有人逐字讀過的句子種類：horizon=%q（實際句子 %q，期望句子無法決定）", entry.Kind, entry.Horizon.Kind, entry.HorizonSentence)
				}
			}
			if wantHorizon != "" && entry.HorizonSentence != wantHorizon {
				t.Errorf("%s 的可追溯期間句實際是 %q，期望是 %q", entry.Kind, entry.HorizonSentence, wantHorizon)
			}

			var wantRange string
			switch {
			case entry.Range.Instant:
				wantRange = "不是一段期間，是 Hub 現在手上的全部。"
			case entry.Range.DefaultDays == 0:
				wantRange = "範圍由你指定。"
			default:
				wantRange = fmt.Sprintf("預設 %d 天，最多 %d 天。", entry.Range.DefaultDays, entry.Range.MaxDays)
			}
			if entry.RangeSentence != wantRange {
				t.Errorf("%s 的期間句實際是 %q，期望是 %q", entry.Kind, entry.RangeSentence, wantRange)
			}

			var wantExport string
			switch entry.Export {
			case ReportExportPagedOnly:
				wantExport = "這份報告一次讀一頁，沒有整份匯出。"
			case ReportExportWholeWindow:
				switch entry.Scope {
				case ReportScopeFleet:
					if entry.Range.Instant {
						wantExport = "匯出的就是現在手上的全部。"
					} else {
						wantExport = "匯出的就是這個範圍裡的全部。"
					}
				case ReportScopeMachine:
					if entry.Range.Instant {
						wantExport = "匯出的就是現在手上的全部，只含那一台。"
					} else {
						wantExport = "匯出的就是這個範圍裡的全部，只含那一台。"
					}
				default:
					t.Errorf("%s 這份報告帶了一個沒有人逐字讀過的句子種類：export scope=%q（實際句子 %q，期望句子無法決定）", entry.Kind, entry.Scope, entry.ExportSentence)
				}
			default:
				t.Errorf("%s 這份報告帶了一個沒有人逐字讀過的句子種類：export=%q（實際句子 %q，期望句子無法決定）", entry.Kind, entry.Export, entry.ExportSentence)
			}
			if wantExport != "" && entry.ExportSentence != wantExport {
				t.Errorf("%s 的匯出句實際是 %q，期望是 %q", entry.Kind, entry.ExportSentence, wantExport)
			}
		})
	}
}
