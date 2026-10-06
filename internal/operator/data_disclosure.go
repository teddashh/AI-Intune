package operator

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// 這個 Hub 對每一台機器留了什麼。
//
// 這一頁存在的理由很窄：保留期那一頁講得出「observed_state 留 30 天」，但沒有
// 任何一頁講得出 observed_state 裡面是什麼、那些字是誰寫的、退役之後還剩下什麼。
// 一個管理平面如果答不出「你手上有我的什麼」，它就不能宣稱自己是可被稽核的。
//
// ⚠ 這份目錄不是文件，是被驗證的宣告。哪些表存著機器的資料，由 SQLite 自己回答
// （`TestEveryMachineScopedTableIsDisclosed`）；哪些表會被時間清掉、看哪一個保留期，
// 由真的在刪東西的 `pruneJobs` 與已驗證的 FK cascade 回答
// （`store.RetentionClassOf`）。這一頁不准自己再抄一份對照表——抄出來的那一份會
// 漂，而漂掉的樣子是產品承諾了一個它不會遵守的保留期。
const DataDisclosureSchemaVersion = 1

var ErrInvalidDataDisclosure = errors.New("operator: invalid data disclosure request")

// DataSource 說這些列是誰產生的。
type DataSource string

const (
	// DataSourceMachine：機器自己送上來的。它可能是錯的，也可能含機器上的自由文字。
	DataSourceMachine DataSource = "machine"
	// DataSourceHub：Hub 自己量到或判定出來的。
	DataSourceHub DataSource = "hub"
	// DataSourceOperator：人輸入的，或人按下去留下的。
	DataSourceOperator DataSource = "operator"
)

// DataSourceSentence 說這一類資料是誰產生的。
func DataSourceSentence(source DataSource) string {
	switch source {
	case DataSourceMachine:
		return "機器自己送上來的。"
	case DataSourceHub:
		return "Hub 自己量到或判定的。"
	case DataSourceOperator:
		return "操作員輸入或按下去留下的。"
	default:
		return ""
	}
}

// DataRetentionKind 說這一類資料會不會被清掉。
type DataRetentionKind string

const (
	// DataRetentionTimed：保留期到了就清，每一組最新的那一列除外。
	DataRetentionTimed DataRetentionKind = "timed"
	// DataRetentionKept：不按時間清。
	DataRetentionKept DataRetentionKind = "kept"
)

// DataRetention 是一類資料留多久。
type DataRetention struct {
	Kind DataRetentionKind `json:"kind"`
	// Class 是保留期政策裡管著它的那一格；Kind 是 timed 時才有值。
	Class string `json:"class,omitempty"`
	// Days 是現行保留期算出來的天數，不是預設值。
	Days int `json:"days,omitempty"`
	// RingRows 大於 0 表示這一類裡有一種列只留最近固定筆數。
	RingRows int `json:"ring_rows,omitempty"`
	// RingSubject 是那一種列的名字。
	RingSubject string `json:"ring_subject,omitempty"`
}

// DataRetentionSentence 說這一類留多久。
func DataRetentionSentence(retention DataRetention) string {
	var head string
	switch retention.Kind {
	case DataRetentionTimed:
		head = fmt.Sprintf("留 %d 天，每一組最新的那一列永遠留著。", retention.Days)
	case DataRetentionKept:
		head = "不按時間清。"
	default:
		return ""
	}
	if retention.RingRows > 0 {
		return fmt.Sprintf("%s%s只留最近 %d 筆。",
			head, retention.RingSubject, retention.RingRows)
	}
	return head
}

// DataRetirementSentence 說退役之後這一類還剩什麼。
//
// 退役不刪任何一列——控制面裡唯一會刪東西的路徑是保留期、註冊票撤銷與兩個
// tailnet 忽略清單。所以退役真正改變的只有一件事：不再有新的一列進來。
func DataRetirementSentence(retention DataRetention) string {
	switch retention.Kind {
	case DataRetentionTimed:
		return "退役不刪列，但不再有新的一列；現有的照上面那個天數自然到期。"
	case DataRetentionKept:
		return "退役不刪列，也不再有新的一列；現有的留著。"
	default:
		return ""
	}
}

// DataCategoryKey 是一類資料的穩定識別字。
type DataCategoryKey string

const (
	DataCategoryRegistry      DataCategoryKey = "registry"
	DataCategoryIdentity      DataCategoryKey = "identity"
	DataCategoryCheckins      DataCategoryKey = "checkins"
	DataCategoryObservations  DataCategoryKey = "observations"
	DataCategoryTickets       DataCategoryKey = "tickets"
	DataCategoryCredentials   DataCategoryKey = "credentials"
	DataCategoryVerifications DataCategoryKey = "verifications"
	DataCategoryState         DataCategoryKey = "state"
	DataCategoryWorkload      DataCategoryKey = "workload"
	DataCategoryCanary        DataCategoryKey = "canary"
	DataCategoryJobs          DataCategoryKey = "jobs"
	DataCategoryDiskClean     DataCategoryKey = "disk_clean"
	DataCategoryAssignments   DataCategoryKey = "assignments"
	DataCategoryAudit         DataCategoryKey = "audit"
)

// DataCategory 是揭露面上的一列。
type DataCategory struct {
	Key      DataCategoryKey `json:"key"`
	Title    string          `json:"title"`
	Holds    string          `json:"holds"`
	Source   DataSource      `json:"source"`
	Tables   []string        `json:"tables"`
	FreeText string          `json:"free_text,omitempty"`
	// Capability 是讀得到這一類所需要的最低權限。
	Capability string `json:"capability"`
	// Path 是機隊層看得到這一類的那一頁。
	Path string `json:"path"`

	Retention DataRetention `json:"retention"`

	SourceSentence     string `json:"source_sentence"`
	RetentionSentence  string `json:"retention_sentence"`
	RetirementSentence string `json:"retirement_sentence"`
	FreeTextSentence   string `json:"free_text_sentence"`
}

// DataFreeTextSentence 說這一類裡有沒有自由文字，有的話是什麼。
//
// 自由文字是這份揭露面唯一真正敏感的部分：其餘欄位都是 Hub 自己的詞彙，長度與
// 取值都受控；自由文字是別人寫的，長度不受控，內容也不受控。
func DataFreeTextSentence(freeText string) string {
	if freeText == "" {
		return "沒有自由文字，每一格都是 Hub 自己的詞彙。"
	}
	return "含自由文字：" + freeText
}

type dataCategoryShape struct {
	title, holds, freeText, path string
	source                       DataSource
	tables                       []string
	ringRows                     int
	ringSubject                  string
}

// 依「先名冊、再機器自己送的、再 Hub 推出來的、最後人留下的」排。
var dataCategoryOrder = []DataCategoryKey{
	DataCategoryRegistry, DataCategoryIdentity,
	DataCategoryCheckins, DataCategoryObservations, DataCategoryTickets,
	DataCategoryCredentials, DataCategoryVerifications,
	DataCategoryState, DataCategoryWorkload, DataCategoryCanary,
	DataCategoryJobs, DataCategoryDiskClean, DataCategoryAssignments, DataCategoryAudit,
}

var dataCategoryShapes = map[DataCategoryKey]dataCategoryShape{
	DataCategoryRegistry: {
		title:  "名冊",
		holds:  "這台機器的顯示名稱、主機名稱、作業系統、架構、Tailscale 位址、執行身分、通道，以及進名冊與退役的時刻。",
		source: DataSourceOperator,
		tables: []string{"machine_registry", "machine_registry_lifecycle_events"},
		// notes 是人自己打的備忘。
		freeText: "名冊備註（人自己打的）。",
		path:     "/machines",
	},
	DataCategoryIdentity: {
		title:  "機器身分線索",
		holds:  "機器自報的 machine-id 線索，以及它被看到幾次、第一次與最後一次是什麼時候。",
		source: DataSourceMachine,
		tables: []string{"machine_identity_hints"},
		path:   "/machines",
	},
	DataCategoryCheckins: {
		title:  "報到",
		holds:  "每兩分鐘一筆：agent 版本、開機識別、序號、開機時長、磁碟剩餘與總量、觀測年齡、時鐘偏移、設定摘要，以及該次報到明示的固定工作原語能力。",
		source: DataSourceMachine,
		tables: []string{"machine_checkins", "machine_job_capabilities"},
		path:   "/machines",
	},
	DataCategoryObservations: {
		title:    "觀測",
		holds:    "agent 量到的每一件事，照 kind 與 subject 分格；payload 是機器送上來的原文。",
		source:   DataSourceMachine,
		tables:   []string{"observed_state"},
		freeText: "觀測 payload（機器送上來的原文，內容與長度都不受 Hub 控制）。",
		path:     "/reports/changes",
	},
	DataCategoryTickets: {
		title:    "票證使用量",
		holds:    "每一個 provider 的佔用觀測：回合、token 量、最後一次成功與失敗、模型名稱。",
		source:   DataSourceMachine,
		tables:   []string{"ticket_occupancy_observation"},
		freeText: "佔用證據、最後一次的錯誤原文與模型名稱。",
		path:     "/reports/tickets",
	},
	DataCategoryCredentials: {
		title:    "憑證狀態",
		holds:    "每一個 provider 的憑證狀態、回報的到期時刻、最後一次續期與真正用到的時刻、驗證方式。",
		source:   DataSourceMachine,
		tables:   []string{"credential_on_machine", "credential_ledger_event"},
		freeText: "最後一次的錯誤原文與人留的備註。⚠ 憑證本身不存，只存狀態。",
		path:     "/machines",
	},
	DataCategoryVerifications: {
		title:    "驗證證據",
		holds:    "每一次驗證的規則、離開碼、通過與否、產生它的是誰、Hub 什麼時候收到。",
		source:   DataSourceMachine,
		tables:   []string{"verification_results"},
		freeText: "驗證指令，以及 stdout 與 stderr 的摘錄。",
		path:     "/jobs",
	},
	DataCategoryState: {
		title:    "狀態判定",
		holds:    "Hub 判定這台進入哪一個狀態、什麼時候進去、什麼時候離開。",
		source:   DataSourceHub,
		tables:   []string{"machine_state_history", "machine_state_transition_events"},
		freeText: "判定理由（Hub 自己寫的句子，長度不設上限）。",
		path:     "/machines",
	},
	DataCategoryWorkload: {
		title:  "工作負載證據",
		holds:  "Hub 從觀測推出的工作負載判決：OpenClaw 在不在、跑的是哪一版、對到哪一份原則。",
		source: DataSourceHub,
		tables: []string{"workload_observation_witness", "workload_observation_evidence"},
		path:   "/machines",
	},
	DataCategoryCanary: {
		title:    "沉默失敗",
		holds:    "Hub 判定這台在活著的情況下沒有把事情做完，以及它從什麼時候開始這樣。",
		source:   DataSourceHub,
		tables:   []string{"canary_silent_failures"},
		freeText: "判定理由（Hub 自己寫的句子）。",
		path:     "/deployments",
	},
	DataCategoryJobs: {
		title:  "工作單",
		holds:  "派給這台的每一張工作單：狀態、修訂號、租約、逾時、產出物摘要，以及它屬於哪一次部署的哪一批。",
		source: DataSourceOperator,
		tables: []string{"jobs", "deployment_targets"},
		path:   "/jobs",
	},
	DataCategoryDiskClean: {
		title:    "磁碟清理",
		holds:    "disk-clean 的最新摘要、指派的設定摘要、rollout 目標，以及 Hub 的告警狀態。摘要是機器送回的那一行；指派與 rollout 是操作員按下的；告警狀態是 Hub 判定後留下的。這些列不按時間清。",
		source:   DataSourceHub,
		tables:   []string{"maintenance_summaries", "maintenance_rollout_targets", "maintenance_assignments", "maintenance_alert_state"},
		freeText: "摘要裡的主機名稱、使用者、attention、分類備註與報告路徑。",
		path:     "/tenant/maintenance",
	},
	DataCategoryAssignments: {
		title:  "指派",
		holds:  "指到這台的設定檔與票證：指派時刻、指派的人、設定檔摘要。",
		source: DataSourceOperator,
		tables: []string{"machine_profile_assignments", "ticket_static_assignment"},
		path:   "/machines/configuration",
	},
	DataCategoryAudit: {
		title:       "稽核記錄",
		holds:       "每一次對這台機器的操作：動作、結果、來源位址、Tailscale 節點與使用者、權限判定。",
		source:      DataSourceOperator,
		tables:      []string{"audit_log"},
		freeText:    "操作理由、請求細節與 User-Agent。⚠ 這一類存的是操作員的身分，不是機器的。",
		path:        "/audit",
		ringRows:    store.OperatorDenialAuditLimit,
		ringSubject: "被拒絕的請求",
	},
}

// DataCategoryKeys returns every category in canonical order.
func DataCategoryKeys() []DataCategoryKey {
	return append([]DataCategoryKey(nil), dataCategoryOrder...)
}

// DataDisclosure 是整份揭露面。
type DataDisclosure struct {
	SchemaVersion int       `json:"schema_version"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
	// Categories 是每一類資料。
	Categories []DataCategory `json:"categories"`
	// Tables 是這些類別合起來涵蓋的表數。
	Tables int `json:"tables"`
	// Timed 是其中會被保留期清掉的類別數。
	Timed int `json:"timed"`
}

// DataDisclosureFor 用現行保留期算出整份揭露面。
func DataDisclosureFor(policy store.RetentionPolicy, evaluatedAt time.Time) (DataDisclosure, error) {
	if evaluatedAt.IsZero() {
		return DataDisclosure{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidDataDisclosure)
	}
	if err := policy.Validate(); err != nil {
		return DataDisclosure{}, fmt.Errorf("%w: %v", ErrInvalidDataDisclosure, err)
	}
	disclosure := DataDisclosure{
		SchemaVersion: DataDisclosureSchemaVersion,
		EvaluatedAt:   evaluatedAt.UTC(),
	}
	for _, key := range dataCategoryOrder {
		category, err := dataCategoryFor(key, policy)
		if err != nil {
			return DataDisclosure{}, err
		}
		disclosure.Categories = append(disclosure.Categories, category)
		disclosure.Tables += len(category.Tables)
		if category.Retention.Kind == DataRetentionTimed {
			disclosure.Timed++
		}
	}
	return disclosure, nil
}

func dataCategoryFor(key DataCategoryKey, policy store.RetentionPolicy) (DataCategory, error) {
	shape, known := dataCategoryShapes[key]
	if !known {
		return DataCategory{}, fmt.Errorf("%w: 不認得的資料類別 %q", ErrInvalidDataDisclosure, key)
	}
	retention, err := dataRetentionFor(shape, policy)
	if err != nil {
		return DataCategory{}, err
	}
	category := DataCategory{
		Key: key, Title: shape.title, Holds: shape.holds, Source: shape.source,
		Tables: append([]string(nil), shape.tables...), FreeText: shape.freeText,
		Capability: "view", Path: shape.path, Retention: retention,
	}
	category.SourceSentence = DataSourceSentence(category.Source)
	category.RetentionSentence = DataRetentionSentence(category.Retention)
	category.RetirementSentence = DataRetirementSentence(category.Retention)
	category.FreeTextSentence = DataFreeTextSentence(category.FreeText)
	return category, nil
}

// dataRetentionFor 問真的在刪東西的那張表，這一類會不會被時間清掉。
//
// ⚠ 一個類別裡的表必須全部同一種待遇。半數被清、半數留著的類別，在畫面上只能給
// 一句保留期，而那一句對其中一半是假的——那種類別要拆開，不是挑一邊講。
func dataRetentionFor(shape dataCategoryShape, policy store.RetentionPolicy) (DataRetention, error) {
	retention := DataRetention{
		Kind: DataRetentionKept, RingRows: shape.ringRows, RingSubject: shape.ringSubject,
	}
	var classes []store.RetentionClass
	for _, table := range shape.tables {
		class, timed := store.RetentionClassOf(table)
		if !timed {
			continue
		}
		classes = append(classes, class)
	}
	if len(classes) == 0 {
		return retention, nil
	}
	if len(classes) != len(shape.tables) {
		return DataRetention{}, fmt.Errorf(
			"%w: %q 裡有的表被時間清、有的不被清，一句保留期講不了兩種待遇",
			ErrInvalidDataDisclosure, shape.title)
	}
	for _, class := range classes[1:] {
		if class != classes[0] {
			return DataRetention{}, fmt.Errorf(
				"%w: %q 裡的表看的是兩個不同的保留期（%q 與 %q）",
				ErrInvalidDataDisclosure, shape.title, classes[0], class)
		}
	}
	horizon, known := store.RetentionHorizon(classes[0], policy)
	if !known {
		return DataRetention{}, fmt.Errorf(
			"%w: 不認得的保留期類別 %q", ErrInvalidDataDisclosure, classes[0])
	}
	retention.Kind = DataRetentionTimed
	retention.Class = string(classes[0])
	retention.Days = int(horizon / (24 * time.Hour))
	return retention, nil
}

// MachineDataResult 是一台機器的揭露面：同一份目錄，加上這台實際留著多少列。
type MachineDataResult struct {
	SchemaVersion int       `json:"schema_version"`
	EvaluatedAt   time.Time `json:"evaluated_at"`
	MachineID     string    `json:"machine_id"`
	DisplayName   string    `json:"display_name"`
	// Retired 為 true 時不會再有新的一列進來。
	Retired    bool                  `json:"retired"`
	RetiredAt  *time.Time            `json:"retired_at,omitempty"`
	Categories []MachineDataCategory `json:"categories"`
	// Rows 是這台機器在 Hub 裡的總列數。
	Rows int64 `json:"rows"`
	// Undated 是其中沒有 Hub 時刻的列數。
	Undated int64 `json:"undated_rows"`
}

// MachineDataCategory 是一類資料對這一台的實際留存量。
type MachineDataCategory struct {
	Category DataCategory `json:"category"`
	Rows     int64        `json:"rows"`
	Undated  int64        `json:"undated_rows"`
	Oldest   *time.Time   `json:"oldest,omitempty"`
	Newest   *time.Time   `json:"newest,omitempty"`
	// CutoffAt 是現行保留期現在會清掉的界線；Kind 是 timed 時才有值。
	CutoffAt *time.Time `json:"cutoff_at,omitempty"`
}

// MachineData 量這個 Hub 現在替一台機器留著什麼。
func (s *Service) MachineData(machineID string, policy store.RetentionPolicy, evaluatedAt time.Time) (MachineDataResult, error) {
	trimmed := strings.TrimSpace(machineID)
	if trimmed == "" || trimmed != machineID {
		return MachineDataResult{}, fmt.Errorf("%w: machine id 必須是 canonical", ErrInvalidDataDisclosure)
	}
	disclosure, err := DataDisclosureFor(policy, evaluatedAt)
	if err != nil {
		return MachineDataResult{}, err
	}
	detail, err := s.MachineDetail(machineID, evaluatedAt)
	if err != nil {
		return MachineDataResult{}, err
	}
	holdings, err := s.store.MachineDataHoldings(machineID)
	if err != nil {
		return MachineDataResult{}, err
	}
	byTable := make(map[string]store.MachineDataHolding, len(holdings))
	for _, holding := range holdings {
		byTable[holding.Table] = holding
	}
	result := MachineDataResult{
		SchemaVersion: DataDisclosureSchemaVersion,
		EvaluatedAt:   disclosure.EvaluatedAt,
		MachineID:     detail.Item.MachineID,
		DisplayName:   detail.Item.DisplayName,
		Retired:       detail.Item.RetiredAt != nil,
		RetiredAt:     detail.Item.RetiredAt,
	}
	for _, category := range disclosure.Categories {
		measured, err := machineDataCategory(category, byTable, disclosure.EvaluatedAt)
		if err != nil {
			return MachineDataResult{}, err
		}
		result.Categories = append(result.Categories, measured)
		result.Rows += measured.Rows
		result.Undated += measured.Undated
	}
	return result, nil
}

func machineDataCategory(category DataCategory, byTable map[string]store.MachineDataHolding,
	evaluatedAt time.Time) (MachineDataCategory, error) {
	measured := MachineDataCategory{Category: category}
	for _, table := range category.Tables {
		holding, known := byTable[table]
		if !known {
			return MachineDataCategory{}, fmt.Errorf(
				"%w: 揭露面講了 %q，但沒有人量它", ErrInvalidDataDisclosure, table)
		}
		measured.Rows += holding.Rows
		measured.Undated += holding.Rows - holding.Dated
		measured.Oldest = earlierTime(measured.Oldest, holding.Oldest)
		measured.Newest = laterTime(measured.Newest, holding.Newest)
	}
	if category.Retention.Kind == DataRetentionTimed {
		cutoff := evaluatedAt.Add(-time.Duration(category.Retention.Days) * 24 * time.Hour)
		measured.CutoffAt = &cutoff
	}
	return measured, nil
}

func earlierTime(current, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.Before(*current) {
		return candidate
	}
	return current
}

func laterTime(current, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.After(*current) {
		return candidate
	}
	return current
}

// MachineDataPath is where one machine's disclosure lives.
func MachineDataPath(machineID string) string { return "/machines/" + machineID + "/data" }

// MachineDataExportPath is where that page's CSV lives.
func MachineDataExportPath(machineID string) string {
	return "/machines/" + machineID + "/data.csv"
}

// MachineDataCSV 匯出的就是畫面上那些列，一列不多一列不少。
func MachineDataCSV(result MachineDataResult) ReportCSVDocument {
	document := ReportCSVDocument{
		Filename: "ai-intune-data-" + result.MachineID + ".csv",
		Columns: []ReportCSVColumn{
			{Key: "category", Header: "類別"},
			{Key: "source", Header: "來源"},
			{Key: "tables", Header: "表"},
			{Key: "rows", Header: "列數"},
			{Key: "undated_rows", Header: "沒有時刻的列數"},
			{Key: "oldest", Header: "最舊"},
			{Key: "newest", Header: "最新"},
			{Key: "retention", Header: "留多久"},
			{Key: "cutoff_at", Header: "現在的清除界線"},
			{Key: "retirement", Header: "退役之後"},
			{Key: "free_text", Header: "自由文字"},
			{Key: "holds", Header: "留的是什麼"},
		},
	}
	for _, measured := range result.Categories {
		tables := append([]string(nil), measured.Category.Tables...)
		sort.Strings(tables)
		document.Rows = append(document.Rows, []string{
			measured.Category.Title,
			DataSourceSentence(measured.Category.Source),
			strings.Join(tables, " "),
			fmt.Sprintf("%d", measured.Rows),
			fmt.Sprintf("%d", measured.Undated),
			ReportCSVOptionalTime(measured.Oldest),
			ReportCSVOptionalTime(measured.Newest),
			measured.Category.RetentionSentence,
			ReportCSVOptionalTime(measured.CutoffAt),
			measured.Category.RetirementSentence,
			measured.Category.FreeTextSentence,
			measured.Category.Holds,
		})
	}
	return document
}
