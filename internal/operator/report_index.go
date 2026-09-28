package operator

import (
	"errors"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// 報告的落地頁。
//
// 它回答的是操作員在點進任何一份報告之前真正想知道的三件事：這份報告算的是什麼、
// 它看得到多遠、能不能把列帶走。前兩件事目前只有讀過原始碼的人知道——保留期會
// 把量測清掉，而一份「最近 30 天」的報告在保留期只剩 14 天的時候，其實只看得到
// 14 天。
//
// ⚠ 這一頁不重算任何一份報告的數字。四個讀取器裡有三個是分頁的、有一個有並行
// 上限，把它們全部跑一遍只為了在落地頁印一個數字，會讓這一頁比它要導向的每一份
// 報告都慢，而且會因為別人正在讀而失敗。範圍與保留期則是直接讀那些報告自己的
// 上限與現行保留期，所以改了哪一個常數，這一頁就跟著改。
const ReportIndexSchemaVersion = 4

var ErrInvalidReportIndex = errors.New("operator: invalid report index request")

type ReportKind string

const (
	ReportChanges         ReportKind = "changes"
	ReportTickets         ReportKind = "tickets"
	ReportAudit           ReportKind = "audit"
	ReportEnrollment      ReportKind = "enrollment"
	ReportSoftware        ReportKind = "software"
	ReportInstall         ReportKind = "install"
	ReportProfile         ReportKind = "profile"
	ReportMachineTimeline ReportKind = "machine_timeline"
	ReportMachineData     ReportKind = "machine_data"
)

var reportOrder = []ReportKind{
	ReportChanges, ReportTickets, ReportEnrollment, ReportSoftware, ReportInstall,
	ReportProfile, ReportAudit, ReportMachineTimeline, ReportMachineData,
}

// ReportKinds returns every report in canonical order.
func ReportKinds() []ReportKind { return append([]ReportKind(nil), reportOrder...) }

// ReportExportState 是「能不能把整份帶走」。
//
// 只有在窗裡是完整的報告才匯得出來。一份分頁讀的報告匯出的是當下那一頁，而那個
// 檔案打開之後看起來跟整份報告一模一樣——它會被當成整份報告引用。
type ReportExportState string

const (
	ReportExportWholeWindow ReportExportState = "whole_window"
	ReportExportPagedOnly   ReportExportState = "paged_only"
)

// ReportScope 是這份報告一次講幾台機器。機隊層的報告在落地頁就點得進去；單機
// 的報告要先挑一台，所以它的匯出連結只存在於那一台的頁面上。
type ReportScope string

const (
	ReportScopeFleet   ReportScope = "fleet"
	ReportScopeMachine ReportScope = "machine"
)

// ReportScopeSentence says what the operator has to pick before reading.
func ReportScopeSentence(scope ReportScope) string {
	switch scope {
	case ReportScopeFleet:
		return "整個機隊一份。"
	case ReportScopeMachine:
		return "一台機器一份，先挑一台。"
	default:
		return ""
	}
}

// ReportExportSentence is what the operator is told about taking rows away.
//
// ⚠ 它同時看範圍與對象。一份不是期間的報告匯不出「這個範圍裡的全部」，而單機的
// 報告不只有時間軸一份——把某一份報告的形狀寫進共用句子，下一份就會在落地頁上
// 被講成它不是的東西。
func ReportExportSentence(state ReportExportState, scope ReportScope, rangeValue ReportRange) string {
	switch state {
	case ReportExportWholeWindow:
		whole := "匯出的就是這個範圍裡的全部"
		if rangeValue.Instant {
			whole = "匯出的就是現在手上的全部"
		}
		if scope == ReportScopeMachine {
			return whole + "，只含那一台。"
		}
		return whole + "。"
	case ReportExportPagedOnly:
		return "這份報告一次讀一頁，沒有整份匯出。"
	default:
		return ""
	}
}

// ReportRange 是這份報告一次看得了多長。DefaultDays 為 0 表示範圍由操作員自己
// 指定，沒有預設的天數；Instant 表示它根本不是一段期間。
type ReportRange struct {
	DefaultDays int  `json:"default_days,omitempty"`
	MaxDays     int  `json:"max_days,omitempty"`
	Instant     bool `json:"instant,omitempty"`
}

// ReportRangeSentence describes the range in one line.
func ReportRangeSentence(value ReportRange) string {
	if value.Instant {
		return "不是一段期間，是 Hub 現在手上的全部。"
	}
	if value.DefaultDays == 0 {
		return "範圍由你指定。"
	}
	return fmt.Sprintf("預設 %d 天，最多 %d 天。", value.DefaultDays, value.MaxDays)
}

// ReportHorizonKind 是「看得回去多遠」的三種答案。
//
// ⚠ 第三種不是偷懶。資料揭露面一次講十三類資料，每一類的保留期都不一樣；挑其中
// 一個數字印在落地頁上，對另外十二類都是假的。
type ReportHorizonKind string

const (
	// ReportHorizonUnbounded：這份報告讀的列沒有任何一條保留期在清。
	ReportHorizonUnbounded ReportHorizonKind = "unbounded"
	// ReportHorizonRetention：保留期會把它讀的列清掉，Days 是現行保留期算出來的。
	ReportHorizonRetention ReportHorizonKind = "retention"
	// ReportHorizonPerCategory：這份報告自己逐類講出各自的保留期。
	ReportHorizonPerCategory ReportHorizonKind = "per_category"
	// ReportHorizonNewestKept：這份報告只讀每一組最新的那一筆，而保留期永遠留著
	// 那一筆。
	//
	// ⚠ 它跟 unbounded 不一樣，而那個差別要講出來：unbounded 是「這張表根本沒有
	// 人在清」，newest_kept 是「這張表天天在清，但它讀的那一列被保護著」。說成
	// unbounded 會讓人以為翻得到版號的歷史；說成 retention 則會印出一個對這份報告
	// 完全不適用的天數。
	ReportHorizonNewestKept ReportHorizonKind = "newest_kept"
)

// ReportHorizon 是這份報告的列還在不在。
type ReportHorizon struct {
	Kind ReportHorizonKind `json:"kind"`
	Days int               `json:"days,omitempty"`
}

// ReportHorizonSentence says how far back this report can still see.
func ReportHorizonSentence(value ReportHorizon) string {
	switch value.Kind {
	case ReportHorizonUnbounded:
		return "從 Hub 的第一天起都還在。"
	case ReportHorizonRetention:
		return fmt.Sprintf("看得到 %d 天，再往前的已經被保留期清掉了。", value.Days)
	case ReportHorizonPerCategory:
		return "每一類資料各自不同，這份報告會逐類講。"
	case ReportHorizonNewestKept:
		return "讀的是每一台最新的那一筆，保留期永遠留著那一筆；再往前的版號已經被清掉了。"
	default:
		return ""
	}
}

type ReportEntry struct {
	Kind     ReportKind  `json:"kind"`
	Title    string      `json:"title"`
	Question string      `json:"question"`
	Evidence string      `json:"evidence"`
	Scope    ReportScope `json:"scope"`
	Path     string      `json:"path"`

	Range           ReportRange   `json:"range"`
	RangeSentence   string        `json:"range_sentence"`
	Horizon         ReportHorizon `json:"horizon"`
	HorizonSentence string        `json:"horizon_sentence"`

	ScopeSentence string `json:"scope_sentence"`

	Export         ReportExportState `json:"export"`
	ExportPath     string            `json:"export_path,omitempty"`
	ExportSentence string            `json:"export_sentence"`
}

type ReportIndex struct {
	SchemaVersion int           `json:"schema_version"`
	EvaluatedAt   time.Time     `json:"evaluated_at"`
	Total         int           `json:"total"`
	Exportable    int           `json:"exportable"`
	Entries       []ReportEntry `json:"entries"`
}

type reportShape struct {
	title, question, evidence, path string
	scope                           ReportScope
	rangeValue                      ReportRange
	// retained 指向保留期政策裡真正管著這份報告的那一個期限。nil 表示這份報告
	// 讀的列沒有任何一條保留期在清。
	retained func(store.RetentionPolicy) time.Duration
	// perCategory 表示這份報告讀的列分屬好幾種保留期，由它自己逐類講。
	perCategory bool
	// newestKept 表示這份報告只讀每一組最新的那一筆，而 pruneJobs 永遠留著那一筆。
	newestKept bool
	exportPath string
	// exportable 讓單機的報告也宣告得了「整份匯得出來」，即使那個連結要先挑一台
	// 機器才生得出來。
	exportable bool
}

var reportShapes = map[ReportKind]reportShape{
	ReportChanges: {
		title:    "變更",
		question: "這段期間哪些機器的什麼東西變了。",
		evidence: "Hub 收下的每一次量測，加上它自己的狀態判定與名冊異動。",
		scope:    ReportScopeFleet,
		path:     "/reports/changes",
		rangeValue: ReportRange{
			DefaultDays: int(DefaultChangeWindow / (24 * time.Hour)),
			MaxDays:     int(MaxChangeWindow / (24 * time.Hour)),
		},
		retained: func(policy store.RetentionPolicy) time.Duration { return policy.Observations },
	},
	ReportTickets: {
		title:      "票證使用量",
		question:   "這段期間每一個 provider 跑了幾回合、錯了幾回合。",
		evidence:   "agent 回報的票證佔用觀測，照 provider 彙總。",
		scope:      ReportScopeFleet,
		path:       "/reports/tickets",
		rangeValue: ReportRange{DefaultDays: DefaultTicketReadDays, MaxDays: MaxTicketReadDays},
		retained:   func(policy store.RetentionPolicy) time.Duration { return policy.Occupancy },
		exportPath: "/reports/tickets.csv",
	},
	ReportEnrollment: {
		title:    "註冊",
		question: "說好要納管的機器，來了沒有。",
		evidence: "名冊上每一列的宣告時刻、票的狀態，以及它報到過沒有。",
		scope:    ReportScopeFleet,
		path:     "/reports/enrollment",
		// 名冊不按時間清，所以這份報告看得到第一天；它問的也不是一段期間，
		// 而是「現在名冊上這些列各自走到哪一步」。
		rangeValue: ReportRange{Instant: true},
		exportPath: "/reports/enrollment.csv",
	},
	ReportSoftware: {
		title:    "軟體清查",
		question: "機隊上裝了什麼、各是哪一版、哪幾台沒有，以及那個版號講的是不是正在跑的那一份。",
		evidence: "每一台最新一筆工具觀測，加上名冊上一台都還沒回報過的那幾台。",
		scope:    ReportScopeFleet,
		path:     "/reports/software",
		// 它問的不是一段期間，而是「現在機隊上有什麼」。
		rangeValue: ReportRange{Instant: true},
		newestKept: true,
		exportPath: "/reports/software.csv",
	},
	ReportInstall: {
		title:    "每機安裝狀態",
		question: "我叫它裝的那一個，跟我看到的一不一樣，以及看到的那個版號講的是不是正在跑的那一份。",
		evidence: "每一台最後一筆安裝意圖，對上它最新一筆工具觀測。",
		scope:    ReportScopeFleet,
		path:     "/reports/install",
		// 它問的不是一段期間，而是「現在指派的跟看到的對不對得上」。
		rangeValue: ReportRange{Instant: true},
		// ⚠ 意圖那一半（desired_state）沒有保留期在清，觀測那一半只讀最新一筆。
		// 兩半之中受限的是觀測，所以這份報告的極限跟軟體清查同一句。
		newestKept: true,
		exportPath: "/reports/install.csv",
	},
	ReportProfile: {
		title:    "發佈與指派",
		question: "這份 profile 發佈了，然後呢。",
		evidence: "已發佈的每一版 profile，對上現在誰身上是它、它點名的版本這個 Hub 指派過沒有與看到過沒有。",
		scope:    ReportScopeFleet,
		path:     "/reports/profile",
		// 它問的不是一段期間，而是「現在發佈的那幾版各自走到哪一步」。
		rangeValue: ReportRange{Instant: true},
		// ⚠ 目錄與指派那兩半（machine_profiles、machine_profile_assignments、
		// desired_state）都沒有保留期在清，受限的只有觀測那一側，而它只讀最新一筆。
		newestKept: true,
		exportPath: "/reports/profile.csv",
	},
	ReportAudit: {
		title:    "稽核記錄",
		question: "誰在什麼時候、透過哪一條路徑、對什麼做了什麼。",
		evidence: "每一次通過 operator 邊界的請求，成功與被拒都在內。",
		scope:    ReportScopeFleet,
		path:     "/audit",
	},
	ReportMachineTimeline: {
		title:      "單機事件時間軸",
		question:   "一台機器在這段期間依序發生了什麼。",
		evidence:   "名冊、健康判定、工作單與操作員動作，照時間排成一條。",
		scope:      ReportScopeMachine,
		path:       "/machines",
		rangeValue: ReportRange{DefaultDays: DefaultMachineTimelineDays, MaxDays: MaxMachineTimelineDays},
		exportable: true,
	},
	ReportMachineData: {
		title:       "這台留了什麼",
		question:    "Hub 現在替一台機器留著哪些資料、各留多久、退役之後還剩什麼。",
		evidence:    "每一張存著 machine_id 的表，逐類的列數與最舊 / 最新。",
		scope:       ReportScopeMachine,
		path:        "/tenant/data",
		rangeValue:  ReportRange{Instant: true},
		perCategory: true,
		exportable:  true,
	},
}

// ReportIndexFor describes every report this Hub can produce.
func ReportIndexFor(policy store.RetentionPolicy, evaluatedAt time.Time) (ReportIndex, error) {
	if evaluatedAt.IsZero() {
		return ReportIndex{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidReportIndex)
	}
	if err := policy.Validate(); err != nil {
		return ReportIndex{}, fmt.Errorf("%w: %v", ErrInvalidReportIndex, err)
	}
	index := ReportIndex{
		SchemaVersion: ReportIndexSchemaVersion,
		EvaluatedAt:   evaluatedAt.UTC(),
		Total:         len(reportOrder),
	}
	for _, kind := range reportOrder {
		shape := reportShapes[kind]
		entry := ReportEntry{
			Kind: kind, Title: shape.title, Question: shape.question,
			Evidence: shape.evidence, Scope: shape.scope, Path: shape.path,
			Range: shape.rangeValue, Export: ReportExportPagedOnly, ExportPath: shape.exportPath,
		}
		entry.Horizon = ReportHorizon{Kind: ReportHorizonUnbounded}
		switch {
		case shape.retained != nil:
			entry.Horizon = ReportHorizon{
				Kind: ReportHorizonRetention,
				Days: reportHorizonDays(shape.retained(policy), shape.rangeValue),
			}
		case shape.perCategory:
			entry.Horizon = ReportHorizon{Kind: ReportHorizonPerCategory}
		case shape.newestKept:
			entry.Horizon = ReportHorizon{Kind: ReportHorizonNewestKept}
		}
		if shape.exportPath != "" || shape.exportable {
			entry.Export = ReportExportWholeWindow
			index.Exportable++
		}
		entry.ScopeSentence = ReportScopeSentence(entry.Scope)
		entry.RangeSentence = ReportRangeSentence(entry.Range)
		entry.HorizonSentence = ReportHorizonSentence(entry.Horizon)
		entry.ExportSentence = ReportExportSentence(entry.Export, entry.Scope, entry.Range)
		index.Entries = append(index.Entries, entry)
	}
	return index, nil
}

// reportHorizonDays is the smaller of what the ledger still holds and what this
// report will read in one go. Printing the retention horizon alone would
// promise a year of tickets from a report that never asks for more than a
// month.
func reportHorizonDays(retained time.Duration, rangeValue ReportRange) int {
	days := int(retained / (24 * time.Hour))
	if rangeValue.MaxDays > 0 && rangeValue.MaxDays < days {
		return rangeValue.MaxDays
	}
	return days
}

// ReportIndexFor is also reachable through the service so every surface reaches
// the report catalogue the same way it reaches everything else.
func (s *Service) ReportIndex(policy store.RetentionPolicy, evaluatedAt time.Time) (ReportIndex, error) {
	return ReportIndexFor(policy, evaluatedAt)
}
