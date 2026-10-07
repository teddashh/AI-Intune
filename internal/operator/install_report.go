package operator

// 每機安裝狀態 —— 「我叫它裝的那一個，跟我看到的一不一樣」。
//
// ⚠⚠ 這份報告最危險的地方是**把觀測改寫成判決**。「我看到的還是舊版」與「它裝失敗
// 了」是兩件事：前者可能只是還沒回報、可能是工作單還沒跑、也可能那台上的東西根本不
// 是從這一次指派來的。所以這一頁只講兩個事實與它們的方向——「指派的是 X、看到的是
// Y」——每一種狀態都是一句正面陳述，沒有一句是判決。
//
// ⚠⚠ 正式庫 2026-09-12 量到的形狀決定了這件事：samplehub1 被指派 openclaw 2026.5.26，
// 實際跑的是 2026.6.6——**比指派的還新**。機隊從 2026-09-06 起改從 `releases/<ver>/`
// 跑，那條路徑不經過部署。所以「指派的比看到的舊」必須是一個站得住的狀態，不能叫
// 「落後」也不能叫「裝失敗」；而那句限制要印在畫面上，不是只寫在這裡。
//
// ⚠ 它不自己解析意圖。哪些機器算數讀的是註冊報告在用的同一份名冊投影
// （machineSummariesFrom）；「這台最後被叫去裝什麼」讀的是 Store 那一份
// FleetInstallIntents，誰贏由 revision 決定——那是 agent MaxSeen 擋降版的同一條規則。
// 「看到什麼」讀的是軟體清查在用的同一份 FleetTools。三邊各自算一次的話，漂掉的樣子
// 是同一台機器在三頁上有三個答案。

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

// InstallReportSchemaVersion 是這份報告的形狀版號。
//
// ⚠ 加一個欄位就要動它。用戶端的解碼器不收不認識的欄位，所以多一個欄位對舊的用戶端
// 就是整份拒收——版號不動的話，那個拒收會看起來像 Hub 掛了。
const InstallReportSchemaVersion = 2

// MaxInstallReportMachines / MaxInstallReportResources 是一次讀得了多少。
//
// ⚠ 超過就拒絕，不截斷——跟軟體清查同一條理由：一份少算的報告看起來就是全部。
const (
	MaxInstallReportMachines  = 2000
	MaxInstallReportResources = 200
)

var ErrInvalidInstallReport = errors.New("operator: invalid install report request")

// InstallState 是一台機器對一個資源、畫面上那一格的全部。
//
// ⚠ 十種互斥且窮盡，而且全部是正面陳述——沒有一個是「不是 X」。最重要的是後面四種
// 要分開：「指派了但這台說它沒有」「指派了但這台的回報裡沒有這個東西」「指派了但這
// 台從來沒回報過」「從來沒有被指派過」，下一步完全不同。把它們併成一個「沒有」，等
// 於讓沉默看起來像一個已知的答案。
type InstallState string

const (
	// InstallMatches：指派的版號跟看到的一樣。
	InstallMatches InstallState = "matches"
	// InstallAssignedNewer：指派的版號比看到的新。
	InstallAssignedNewer InstallState = "assigned_newer"
	// InstallAssignedOlder：指派的版號比看到的舊。
	InstallAssignedOlder InstallState = "assigned_older"
	// InstallIncomparable：兩邊都有版號，但比不出先後。
	InstallIncomparable InstallState = "incomparable"
	// InstallAssignedNoVersion：指派的那一筆沒有講版號。
	InstallAssignedNoVersion InstallState = "assigned_no_version"
	// InstallObservedNoVersion：這台上有這個東西，但問不到它的版號。
	InstallObservedNoVersion InstallState = "observed_no_version"
	// InstallAbsent：指派過，而這台回報說它上面沒有。
	InstallAbsent InstallState = "absent"
	// InstallUnobserved：指派過，這台有在回報，但回報裡沒有這個東西。
	InstallUnobserved InstallState = "unobserved"
	// InstallUnreported：指派過，但這台從來沒回報過任何東西。
	InstallUnreported InstallState = "unreported"
	// InstallUnassigned：從來沒有被指派過這個東西。
	InstallUnassigned InstallState = "unassigned"
)

var installStateOrder = []InstallState{
	InstallMatches, InstallAssignedNewer, InstallAssignedOlder, InstallIncomparable,
	InstallAssignedNoVersion, InstallObservedNoVersion, InstallAbsent,
	InstallUnobserved, InstallUnreported, InstallUnassigned,
}

// InstallStates returns every state in canonical order.
func InstallStates() []InstallState {
	return append([]InstallState(nil), installStateOrder...)
}

type installStateShape struct {
	title, meaning, nextStep string
	assigned                 bool
	differs                  bool
	// observedVersion：這一格帶得出「看到的版號」。
	//
	// ⚠ 它不等於「這台上有這個東西」：裝了卻問不到版號的那一格是有東西沒版號。
	// 兩者分開，是因為「有版號可以比」才是這一頁真正的分界。
	observedVersion bool
	// present：這台回報說它上面有這個東西。
	//
	// ⚠ 它決定這一格有沒有「版號講的是哪一份」那一軸。回報說沒有、回報裡沒有、從來
	// 沒回報過、沒有被指派過——這四種都沒有一個量過版號的檔案，所以也沒有「量的是不
	// 是正在跑的那一份」這個問題。
	present bool
}

// ⚠ 每一個狀態各自帶一句「這是什麼」與一句「下一步做什麼」，Web、JSON、terminal 與
// CSV 講的是同一組句子。對得上的那一格沒有下一步——硬編一句會讓人以為還有事要做。
var installStateShapes = map[InstallState]installStateShape{
	InstallMatches: {
		title:           "指派的跟看到的一樣",
		meaning:         "Hub 最後指派的版號，跟這台上看到的版號一樣。",
		assigned:        true,
		observedVersion: true,
		present:         true,
	},
	InstallAssignedNewer: {
		title:           "指派的比看到的新",
		meaning:         "Hub 最後指派的版號，比這台上看到的新。",
		nextStep:        "到這台的裝置頁看這個資源的工作單走到哪裡了。",
		assigned:        true,
		observedVersion: true,
		present:         true,
		differs:         true,
	},
	InstallAssignedOlder: {
		title:           "指派的比看到的舊",
		meaning:         "Hub 最後指派的版號，比這台上看到的舊。",
		nextStep:        "決定哪一邊是對的：重新指派這台目前跑的那一版，或把這台部署回指派的那一版。",
		assigned:        true,
		observedVersion: true,
		present:         true,
		differs:         true,
	},
	InstallIncomparable: {
		title:           "比不出來",
		meaning:         "指派的與看到的都有版號，但它們比不出先後。",
		nextStep:        "到這台的裝置頁看它自報的原字串。",
		assigned:        true,
		observedVersion: true,
		present:         true,
	},
	InstallAssignedNoVersion: {
		title:           "指派的那一筆沒講版號",
		meaning:         "Hub 指派過這台，但那一筆指派沒有講版號，所以對不起來。",
		nextStep:        "到部署頁看那一筆指派的內容。",
		assigned:        true,
		observedVersion: true,
		present:         true,
	},
	InstallObservedNoVersion: {
		title:    "裝了，問不到版號",
		meaning:  "這台上有這個東西，但問不到它的版號，所以跟指派的對不起來。",
		nextStep: "到這台的裝置頁看它為什麼答不出版號。",
		assigned: true,
		present:  true,
	},
	InstallAbsent: {
		title:    "指派了，這台上沒有",
		meaning:  "Hub 指派過這台，而這台回報說它上面沒有這個東西。",
		nextStep: "到這台的裝置頁看這個資源的工作單走到哪裡了。",
		assigned: true,
	},
	InstallUnobserved: {
		title:    "指派了，看不到這個東西",
		meaning:  "Hub 指派過這台，這台也在回報，但它的回報裡沒有這個東西。",
		nextStep: "到這台的裝置頁看它回報了什麼。",
		assigned: true,
	},
	InstallUnreported: {
		title:    "指派了，這台沒回報過",
		meaning:  "Hub 指派過這台，但這台從來沒回報過，所以裝沒裝都還不知道。",
		nextStep: "到這台的裝置頁看 agent 有沒有在回報。",
		assigned: true,
	},
	InstallUnassigned: {
		title:    "沒有被指派過",
		meaning:  "這台從來沒有被指派過這個東西。",
		nextStep: "要讓這台裝它就指派一次。",
	},
}

// InstallStateAssigned says whether this state means the Hub assigned something.
func InstallStateAssigned(stateValue InstallState) bool {
	return installStateShapes[stateValue].assigned
}

// InstallStateDiffers says whether the assignment and the observation are known
// to name different versions.
func InstallStateDiffers(stateValue InstallState) bool {
	return installStateShapes[stateValue].differs
}

// InstallStateHasObservedVersion says whether this state can carry a version the
// Hub saw on the machine.
func InstallStateHasObservedVersion(stateValue InstallState) bool {
	return installStateShapes[stateValue].observedVersion
}

// InstallStateReportedPresent says whether this state means the machine reported
// the resource as being on it.
//
// ⚠ 它是匯出的，因為「這一格有沒有 runtime」有第二個讀者：用戶端重數的時候要判
// 「有版號的那幾格都講了它量的是哪一份」。兩邊各自列一次哪些狀態算，其中一邊漏一種，
// 就會有一格的版號沒有人問過它量的是不是正在跑的那一份。
func InstallStateReportedPresent(stateValue InstallState) bool {
	return installStateShapes[stateValue].present
}

// InstallStateTitle / InstallStateMeaning / InstallStateNextStep are the one set
// of sentences every surface prints.
func InstallStateTitle(stateValue InstallState) string {
	return installStateShapes[stateValue].title
}

func InstallStateMeaning(stateValue InstallState) string {
	return installStateShapes[stateValue].meaning
}

func InstallStateNextStep(stateValue InstallState) string {
	return installStateShapes[stateValue].nextStep
}

// InstallRow 是一台機器 × 一個資源。
type InstallRow struct {
	MachineID   string       `json:"machine_id"`
	DisplayName string       `json:"display_name"`
	State       InstallState `json:"state"`
	Title       string       `json:"title"`
	Meaning     string       `json:"meaning"`
	NextStep    string       `json:"next_step,omitempty"`

	// Assigned 是指派的版號，Observed 是看到的版號。空字串表示那一邊沒有版號可講，
	// 而那是一個要照實顯示的狀態，不是可以拿另一邊補上去的空格。
	Assigned string `json:"assigned,omitempty"`
	Observed string `json:"observed,omitempty"`
	// FromDisk：看到的那個版號是從檔案讀的，不是問它本人拿到的。
	//
	// ⚠ 這一格照樣拿去跟指派的比，但畫面上要講清楚它的來源：一個從 package.json
	// 讀來的版號，跟一個程式自己答出來的版號，證據強度不一樣。
	FromDisk bool `json:"from_disk,omitempty"`

	// Runtime 是「看到的那個版號，量的是不是正在跑的那一份」。
	//
	// ⚠⚠ 這一軸在這一頁比在軟體清查上更要緊。這一頁的每一句話都是「指派的 X、看到的
	// Y」，而 Y 量在一個沒有人在跑的檔案上時，「指派的比看到的舊」會叫人去回滾一台其
	// 實沒事的機器。正式庫 2026-09-12 量到 sampleagent2 的 agy：版號量在
	// `~/.local/bin/agy`，在跑的是一個已經從磁碟上不見的 `agy.*.old`。
	//
	// nil 表示這一格沒有這一軸——這台回報說它上面沒有、回報裡沒有、沒回報過，或者
	// 沒有被指派過。那跟「有這個東西但找不到在跑它的 process」是兩件不同的事。
	Runtime *ToolRuntimeFinding `json:"runtime,omitempty"`

	// Scope / ScopeID / ScopeLabel 是這一筆指派從哪個範圍來的。
	//
	// ⚠ 這一欄自己就是一個發現：正式庫上 `stable` 掛著三台機器卻一列 channel-scope
	// 指派都沒有，那三台是靠 machine scope 被指派的。看得到來源，才看得出「channel
	// 這個分組其實沒有在用」。
	Scope      string `json:"scope,omitempty"`
	ScopeID    string `json:"scope_id,omitempty"`
	ScopeLabel string `json:"scope_label,omitempty"`

	// Revision / AssignedAt / AssignedBy 讓畫面上講的每一筆都查得回帳本那一列。
	Revision   int64      `json:"revision,omitempty"`
	AssignedAt *time.Time `json:"assigned_at,omitempty"`
	AssignedBy string     `json:"assigned_by,omitempty"`

	// MeasuredAt 是 agent 自己的鐘，ObservedAt 是 Hub 收到的時刻。
	//
	// ⚠ 兩個都留著，而且「多久以前的事」一律用 ObservedAt 算。agent 的鐘會歪
	// （sampleagent4 快 80 秒），拿它當存活座標會讓一台停了的機器看起來剛回報過。
	MeasuredAt *time.Time `json:"measured_at,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// InstallResource 是一個資源在整個機隊上的樣子。
type InstallResource struct {
	// Name 是畫面上那個名字；ResourceKind/ResourceID 是帳本上的身分。
	Name         string `json:"name"`
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`

	// AssignedOn + UnassignedOn 就是分母。MatchingOn 與 DifferingOn 都只算有指派的。
	AssignedOn   int `json:"assigned_on"`
	UnassignedOn int `json:"unassigned_on"`
	MatchingOn   int `json:"matching_on"`
	DifferingOn  int `json:"differing_on"`

	// MisattributedOn 是有指派的那幾台裡，看到的版號量在一個沒在跑的檔案上的台數。
	//
	// ⚠ 它跟 MatchingOn / DifferingOn 是兩個獨立的軸，不是它們的一部分：一台「指派的
	// 跟看到的一樣」也可以是量錯了檔案，而那一格看起來最安全。
	MisattributedOn int `json:"misattributed_on"`

	Rows []InstallRow `json:"rows"`

	Headline string `json:"headline"`
	NextStep string `json:"next_step,omitempty"`
}

// InstallStateCount 是十種狀態各有幾格。
type InstallStateCount struct {
	State    InstallState `json:"state"`
	Title    string       `json:"title"`
	Count    int          `json:"count"`
	Meaning  string       `json:"meaning"`
	NextStep string       `json:"next_step,omitempty"`
}

// InstallReport 是整份每機安裝狀態。
type InstallReport struct {
	SchemaVersion int       `json:"schema_version"`
	EvaluatedAt   time.Time `json:"evaluated_at"`

	// Machines 是分母：名冊上沒有退役的台數，跟註冊報告的分母是同一個數。
	Machines int `json:"machines"`
	// Assigned 是分母裡至少被指派過一個資源的台數。
	Assigned int `json:"assigned"`
	// Differing 是指派跟看到明確不一樣的資源數。
	Differing int `json:"differing"`

	// Misattributed 是「看到的版號量的不是正在跑的那一份」的格數：正在跑的是另一個
	// 檔案，或者正在跑的那個檔案已經不在磁碟上。
	//
	// ⚠⚠ 它排在 Differing 前面。這幾格的版號量的是沒在跑的那一份，所以拿它們去跟指派
	// 的比方向，比的是一個沒有人在用的檔案——「指派的比看到的舊」本身就還不能信。
	Misattributed int `json:"misattributed"`

	Resources     []InstallResource   `json:"resources"`
	States        []InstallStateCount `json:"states"`
	RuntimeStates []ToolRuntimeCount  `json:"runtime_states"`

	Headline string `json:"headline"`
	// Caveat 是那句必須留在畫面上的限制。
	Caveat   string `json:"caveat"`
	NextStep string `json:"next_step,omitempty"`
}

// InstallReportCaveat 是這份報告講得出口的極限。
//
// ⚠ 它不是免責聲明，是這份報告的判準本身：Hub 只知道自己指派過什麼與自己看到什麼，
// 不知道機器上的東西是從哪條路徑來的。不寫在畫面上的話，一列「指派的比看到的舊」會
// 被讀成「有人亂動這台機器」——而正式環境上那正是預期的樣子。
const InstallReportCaveat = "這份報告比的是 Hub 指派過什麼與 Hub 看到什麼；機器上的東西可以從指派以外的路徑裝上去。"

// InstallReport 回答「我叫它裝的那一個，跟我看到的一不一樣」。
func (s *Service) InstallReport(evaluatedAt time.Time) (InstallReport, error) {
	if evaluatedAt.IsZero() {
		return InstallReport{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidInstallReport)
	}
	evaluatedAt = evaluatedAt.UTC()
	overview, err := s.store.Overview(evaluatedAt)
	if err != nil {
		return InstallReport{}, err
	}
	// ⚠ 機器來自名冊，不是來自意圖也不是來自觀測。讓它們決定有哪些機器，等於讓
	// 「被指派過的」或「有回報的」變成分母——而一台從來沒有被指派過的機器，正是
	// 這份報告最該讓人看見的那一台。
	machines := installMachinesFrom(machineSummariesFrom(overview, evaluatedAt))
	if len(machines) > MaxInstallReportMachines {
		return InstallReport{}, fmt.Errorf(
			"%w: denominator has %d machines, exceeding the %d machines this report can read at once",
			ErrInvalidInstallReport, len(machines), MaxInstallReportMachines)
	}
	intents, err := s.store.FleetInstallIntents()
	if err != nil {
		return InstallReport{}, err
	}
	tools, err := s.store.FleetTools()
	if err != nil {
		return InstallReport{}, err
	}

	inFleet := make(map[string]bool, len(machines))
	for _, machine := range machines {
		inFleet[machine.MachineID] = true
	}
	// ⚠ 退役機器的觀測不進來。它已經離開分母，而這份報告問的是「我的機隊上，我叫
	// 它裝的東西怎麼樣了」。
	observations := make(map[string]map[string]installObservation, len(machines))
	for _, row := range tools {
		if !inFleet[row.MachineID] {
			continue
		}
		if observations[row.MachineID] == nil {
			observations[row.MachineID] = map[string]installObservation{}
		}
		// 登錄表有宣告的探針名稱改記在套件 ID 上。沒有宣告的名稱維持原樣，不猜。
		name := row.Name
		if packageID, ok := agentadapter.PackageForProbeTool(row.Name); ok {
			name = packageID
		}
		observations[row.MachineID][name] = installObservation{
			tool:       row.CLITool,
			measuredAt: row.MeasuredAt,
			observedAt: row.ReceivedAt,
		}
	}

	resolved := resolveInstallIntents(machines, intents)
	resources := installResourceKeys(resolved)
	if len(resources) > MaxInstallReportResources {
		return InstallReport{}, fmt.Errorf(
			"%w: fleet has %d assigned resources, exceeding the %d resources this report can read at once",
			ErrInvalidInstallReport, len(resources), MaxInstallReportResources)
	}

	report := InstallReport{
		SchemaVersion: InstallReportSchemaVersion,
		EvaluatedAt:   evaluatedAt,
		Machines:      len(machines),
		Caveat:        InstallReportCaveat,
		// ⚠ 空資源是這份報告有專屬句子的設計狀態；留成 nil 會輸出 JSON null，讓 Hub 自己的 strict client 拒收整頁。
		Resources: []InstallResource{},
	}
	for _, machine := range machines {
		if len(resolved[machine.MachineID]) > 0 {
			report.Assigned++
		}
	}
	counts := map[InstallState]int{}
	runtimeCounts := map[ToolRuntime]int{}
	for _, key := range resources {
		resource := installResourceFor(key, machines, resolved, observations)
		for _, row := range resource.Rows {
			counts[row.State]++
			if row.Runtime == nil {
				continue
			}
			runtimeCounts[row.Runtime.State]++
			if ToolRuntimeMisattributed(row.Runtime.State) {
				report.Misattributed++
			}
		}
		if resource.DifferingOn > 0 {
			report.Differing++
		}
		report.Resources = append(report.Resources, resource)
	}
	for _, stateValue := range installStateOrder {
		shape := installStateShapes[stateValue]
		report.States = append(report.States, InstallStateCount{
			State: stateValue, Title: shape.title, Count: counts[stateValue],
			Meaning: shape.meaning, NextStep: shape.nextStep,
		})
	}
	// ⚠ 七種狀態一律列出來，包括 0 的那幾種。少列的話，讀的人會以為那一種不存在，
	// 而「沒有一格是這樣」跟「這一頁不講這件事」是兩件事。
	for _, stateValue := range toolRuntimeOrder {
		report.RuntimeStates = append(report.RuntimeStates, ToolRuntimeCount{
			State: stateValue, Title: ToolRuntimeTitle(stateValue),
			Count: runtimeCounts[stateValue], Meaning: ToolRuntimeMeaning(stateValue),
			NextStep: ToolRuntimeNextStep(stateValue),
		})
	}
	report.Headline = InstallReportHeadline(report)
	report.NextStep = installReportNextStep(report)
	return report, nil
}

// installObservation 是一台機器對一個工具的最新一筆。
type installObservation struct {
	tool       model.CLITool
	measuredAt time.Time
	observedAt time.Time
}

// installMachine 是這份報告需要知道的關於一台機器的全部：id、名字，跟它在哪個
// channel 上——channel 決定哪一個 channel-scope 的指派對它適用。
//
// ⚠ 刻意不帶判決與心跳。讓它看得到那些，下一個人就會忍不住在這裡再寫一次「健康」
// 的判斷，然後兩處判斷會在某一天分岔。
type installMachine struct {
	MachineID   string
	DisplayName string
	Channel     string
}

// installMachinesFrom 取分母：名冊上沒有退役的那些。
func installMachinesFrom(summaries []MachineSummary) []installMachine {
	out := make([]installMachine, 0, len(summaries))
	for _, summary := range summaries {
		if summary.RetiredAt != nil {
			continue
		}
		channel := ""
		if summary.Channel != nil {
			channel = *summary.Channel
		}
		out = append(out, installMachine{
			MachineID:   summary.MachineID,
			DisplayName: installMachineLabel(summary),
			Channel:     channel,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return out[i].DisplayName < out[j].DisplayName
		}
		return out[i].MachineID < out[j].MachineID
	})
	return out
}

// installMachineLabel 寧可退到 machine_id 也不留白——一列沒有名字的報告，人沒有辦法
// 知道它講的是哪一台。
func installMachineLabel(summary MachineSummary) string {
	if summary.DisplayName != "" {
		return summary.DisplayName
	}
	return summary.MachineID
}

// installResourceKey 是帳本上的資源身分。
type installResourceKey struct {
	Kind string
	ID   string
}

// Name 是畫面上那個名字。kind 跟 id 一樣的時候（openclaw:openclaw）只講一次。
func (k installResourceKey) Name() string {
	if k.Kind == k.ID {
		return k.ID
	}
	return k.Kind + "/" + k.ID
}

// resolveInstallIntents 算出每一台機器對每一個資源，最後被叫去裝的是哪一筆。
//
// ⚠⚠ 誰贏由 **revision** 決定，不是「machine scope 蓋過 channel scope」。
// createDesiredStateTx 的計數器 key 是資源不是 scope，就是為了讓兩個 scope 永遠比得
// 出先後；agent 的 MaxSeen 擋降版用的是同一條規則。這裡再發明一套優先序的話，畫面上
// 講的那一筆會跟機器實際收到的那一筆是兩個答案。
func resolveInstallIntents(machines []installMachine, intents []store.InstallIntent,
) map[string]map[installResourceKey]store.InstallIntent {
	byScope := make(map[string][]store.InstallIntent, len(intents))
	for _, intent := range intents {
		key := intent.ScopeType + "\x00" + intent.ScopeID
		byScope[key] = append(byScope[key], intent)
	}
	out := make(map[string]map[installResourceKey]store.InstallIntent, len(machines))
	for _, machine := range machines {
		candidates := byScope["machine\x00"+machine.MachineID]
		if machine.Channel != "" {
			candidates = append(candidates, byScope["channel\x00"+machine.Channel]...)
		}
		for _, intent := range candidates {
			key := installResourceKey{Kind: intent.ResourceKind, ID: intent.ResourceID}
			if out[machine.MachineID] == nil {
				out[machine.MachineID] = map[installResourceKey]store.InstallIntent{}
			}
			if current, ok := out[machine.MachineID][key]; ok && current.Revision >= intent.Revision {
				continue
			}
			out[machine.MachineID][key] = intent
		}
	}
	return out
}

// installResourceKeys 取所有被指派過的資源，排出一個每次都一樣的順序。
//
// ⚠ 資源來自「解析之後真的落在某一台身上的意圖」。一個 channel 上有意圖卻沒有機器
// 的時候不會生出一個沒有任何一列的資源——那一頁上不會有人能對它做任何事。
func installResourceKeys(resolved map[string]map[installResourceKey]store.InstallIntent) []installResourceKey {
	seen := map[installResourceKey]bool{}
	for _, byResource := range resolved {
		for key := range byResource {
			seen[key] = true
		}
	}
	out := make([]installResourceKey, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name() != out[j].Name() {
			return out[i].Name() < out[j].Name()
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// installResourceFor 把一個資源在整個機隊上的樣子算出來。
func installResourceFor(key installResourceKey, machines []installMachine,
	resolved map[string]map[installResourceKey]store.InstallIntent,
	observations map[string]map[string]installObservation,
) InstallResource {
	resource := InstallResource{
		Name: key.Name(), ResourceKind: key.Kind, ResourceID: key.ID, Rows: []InstallRow{},
	}
	for _, machine := range machines {
		row := InstallRow{MachineID: machine.MachineID, DisplayName: machine.DisplayName}
		intent, assigned := resolved[machine.MachineID][key]
		if !assigned {
			row.State = InstallUnassigned
			resource.Rows = append(resource.Rows, installRowSentences(row))
			continue
		}
		row.Assigned = intent.Version
		row.Scope, row.ScopeID = intent.ScopeType, intent.ScopeID
		row.ScopeLabel = installScopeLabel(intent.ScopeType, intent.ScopeID)
		row.Revision, row.AssignedBy = intent.Revision, intent.CreatedBy
		if !intent.CreatedAt.IsZero() {
			assignedAt := intent.CreatedAt.UTC()
			row.AssignedAt = &assignedAt
		}

		// ⚠ 觀測用 resource_id 對 cli_tool 的 subject。對不到就是「看不到這個東西」，
		// 不准退而求其次去猜別的 subject——猜出來的那一格會讓人以為 Hub 看得到它。
		observation, reported := observations[machine.MachineID][key.ID]
		switch {
		case !reported && len(observations[machine.MachineID]) == 0:
			row.State = InstallUnreported
		case !reported:
			row.State = InstallUnobserved
		case !observation.tool.Present:
			row.State = InstallAbsent
			row.MeasuredAt, row.ObservedAt = installTimes(observation)
		default:
			row.MeasuredAt, row.ObservedAt = installTimes(observation)
			// ⚠ 跟軟體清查讀的是同一份觀測的同一個判準（ToolRuntimeOf）。這一頁自己
			// 再判一次的話，同一台機器的同一個工具會在兩頁上有兩個答案。
			runtime := ToolRuntimeOf(observation.tool)
			row.Runtime = &runtime
			row.Observed = observation.tool.VersionReported
			if row.Observed == "" && observation.tool.VersionPackageJSON != "" {
				// 檔案上讀到的算數，但要說清楚它不是它本人講的。
				row.Observed, row.FromDisk = observation.tool.VersionPackageJSON, true
			}
			row.State = installComparison(row.Assigned, row.Observed)
		}
		resource.Rows = append(resource.Rows, installRowSentences(row))
	}

	for _, row := range resource.Rows {
		// ⚠ 這一個 if 在 switch 外面，不是它的一個 case。量錯檔案跟指派對不對得上是兩
		// 個獨立的軸：寫成一個 case 的話，一台「指派的跟看到的一樣」而其實量錯了檔案的
		// 機器就不會被算進來——而那一格正是最沒有人會回頭看的一格。
		if row.Runtime != nil && ToolRuntimeMisattributed(row.Runtime.State) {
			resource.MisattributedOn++
		}
		switch {
		case !InstallStateAssigned(row.State):
			resource.UnassignedOn++
		case row.State == InstallMatches:
			resource.AssignedOn++
			resource.MatchingOn++
		case InstallStateDiffers(row.State):
			resource.AssignedOn++
			resource.DifferingOn++
		default:
			resource.AssignedOn++
		}
	}
	resource.Headline = InstallResourceHeadline(resource)
	resource.NextStep = installResourceNextStep(resource)
	return resource
}

// installComparison 是「指派的是 X、看到的是 Y」那一句話的全部。
//
// ⚠ 兩邊都要有版號才比。指派沒講版號是一種狀態，看到的沒有版號是另一種——都不准
// 拿另一邊補上去。版號怎麼比走的是軟體清查那一份 compareSoftwareVersions：字串比
// 大小會說 1.0.3 比 1.0.13 新，然後叫人把一台其實比較新的機器降級。
func installComparison(assigned, observed string) InstallState {
	if assigned == "" {
		return InstallAssignedNoVersion
	}
	if observed == "" {
		return InstallObservedNoVersion
	}
	cmp, ok := compareSoftwareVersions(assigned, observed)
	switch {
	case !ok:
		return InstallIncomparable
	case cmp > 0:
		return InstallAssignedNewer
	case cmp < 0:
		return InstallAssignedOlder
	}
	return InstallMatches
}

func installRowSentences(row InstallRow) InstallRow {
	shape := installStateShapes[row.State]
	row.Title, row.Meaning, row.NextStep = shape.title, shape.meaning, shape.nextStep
	return row
}

// installScopeLabel 把「這一筆指派是給誰的」講成一句人看得懂的話。
func installScopeLabel(scopeType, scopeID string) string {
	if scopeType == "channel" {
		return fmt.Sprintf("指派給 %s channel", scopeID)
	}
	return "指派給這台"
}

func installTimes(observation installObservation) (*time.Time, *time.Time) {
	measured, observed := observation.measuredAt.UTC(), observation.observedAt.UTC()
	var measuredPtr, observedPtr *time.Time
	if !observation.measuredAt.IsZero() {
		measuredPtr = &measured
	}
	if !observation.observedAt.IsZero() {
		observedPtr = &observed
	}
	return measuredPtr, observedPtr
}

// InstallResourceHeadline 是一個資源那一行字。
func InstallResourceHeadline(resource InstallResource) string {
	total := resource.AssignedOn + resource.UnassignedOn
	if resource.AssignedOn == 0 {
		return fmt.Sprintf("%s：%d 台都沒有被指派過。", resource.Name, total)
	}
	headline := fmt.Sprintf("%s：%d/%d 台被指派過", resource.Name, resource.AssignedOn, total)
	// ⚠⚠ 只講「幾台不一樣」而不講「幾台對不起來」的話，一台沒回報過的機器會被算進
	// 「沒有不一樣」裡——而這一行正是最多人只讀它就走的地方。沉默不是一個對得上。
	unknown := resource.AssignedOn - resource.MatchingOn - resource.DifferingOn
	switch {
	case resource.DifferingOn > 0:
		headline += fmt.Sprintf("，其中 %d 台指派的跟看到的不一樣", resource.DifferingOn)
	case resource.MatchingOn == resource.AssignedOn:
		headline += "，指派的跟看到的都一樣"
	}
	// ⚠⚠ 緊跟在上面那一句後面，而且兩個分支都要有。「指派的跟看到的都一樣」是這一行
	// 最讓人放下心的一句，而它可以整句講的是一批沒有人在跑的檔案。
	if resource.MisattributedOn > 0 {
		headline += fmt.Sprintf("；其中 %d 台看到的版號量的是沒在跑的那一份", resource.MisattributedOn)
	}
	if unknown > 0 {
		headline += fmt.Sprintf("；有 %d 台對不起來", unknown)
	}
	if resource.UnassignedOn > 0 {
		headline += fmt.Sprintf("；另有 %d 台沒有被指派過", resource.UnassignedOn)
	}
	return headline + "。"
}

// installResourceNextStep 只有在有事可做的時候才給一句。
func installResourceNextStep(resource InstallResource) string {
	// ⚠⚠ 排在「不一樣」前面。那幾台看到的版號量在一個沒在跑的檔案上，所以「哪一邊是
	// 對的」問的是一個沒有人在用的檔案——先確定在比哪一個檔案，再談要不要回滾。
	if resource.MisattributedOn > 0 {
		return "先看那幾台看到的版號量的是沒在跑的那一份，再談哪一邊是對的。"
	}
	if resource.DifferingOn > 0 {
		return "先看指派的跟看到的不一樣的那幾台，決定哪一邊是對的。"
	}
	if resource.UnassignedOn > 0 {
		return "沒有被指派過的那幾台，要讓它們裝就指派一次。"
	}
	return ""
}

// InstallReportHeadline 是整份報告那一行字。
func InstallReportHeadline(report InstallReport) string {
	if report.Machines == 0 {
		return "名冊上沒有機器，沒有東西可以對。"
	}
	if len(report.Resources) == 0 {
		return fmt.Sprintf("分母 %d 台，還沒有任何一台被指派過任何東西。", report.Machines)
	}
	headline := fmt.Sprintf("分母 %d 台、%d 個資源：%d 台被指派過",
		report.Machines, len(report.Resources), report.Assigned)
	if report.Differing > 0 {
		headline += fmt.Sprintf("，%d 個資源上指派的跟看到的不一樣", report.Differing)
	}
	// ⚠⚠ 緊跟在版號那一句後面，而且不能省。有格子的版號量的是沒在跑的那一份時，上面
	// 那句「不一樣」比的是一個沒有人在用的檔案——少了這一句，它會被讀成有人動過機器。
	if report.Misattributed > 0 {
		headline += fmt.Sprintf("；另有 %d 格看到的版號量的是沒在跑的那一份", report.Misattributed)
	}
	unassigned := report.Machines - report.Assigned
	if unassigned > 0 {
		headline += fmt.Sprintf("；另有 %d 台一個資源都沒有被指派過", unassigned)
	}
	return headline + "。"
}

// installReportNextStep 只有在有事可做的時候才給一句。
func installReportNextStep(report InstallReport) string {
	// ⚠⚠ 排在「不一樣」前面，理由跟 installResourceNextStep 那一句一樣。
	if report.Misattributed > 0 {
		return "先看那幾格看到的版號量的是沒在跑的那一份，再談哪一個資源對不起來。"
	}
	if report.Differing > 0 {
		return "先看指派的跟看到的不一樣的那幾個資源。"
	}
	if report.Machines > report.Assigned {
		return "一個資源都沒有被指派過的那幾台，要讓它們裝東西就指派一次。"
	}
	return ""
}

// InstallReportExportPath is where the whole comparison can be taken away.
func InstallReportExportPath() string { return "/reports/install.csv" }

// InstallReportCSV 匯出的就是畫面上那些列，一列不多一列不少。
//
// ⚠ 一列是「一台機器 × 一個資源」，不是一個資源一列。把每一台壓成一格統計，匯出
// 的檔案就答不出「哪一台沒有被指派過」——而那正是有人把它開在試算表裡要問的問題。
//
// ⚠ 指派的版號與看到的版號各佔一欄，不合併成一個「差異」欄。合併之後那一欄會逼著
// 讀的人自己去猜方向，而方向正是這一頁唯一講得出口的東西。
//
// ⚠ 量版號的那個檔案與正在跑的那個檔案也各佔一欄。有人把這個檔案開在試算表裡排序
// 「看到的版號」的時候，那兩欄是唯一看得出「這一列的版號量錯了檔案」的東西。
func InstallReportCSV(report InstallReport) ReportCSVDocument {
	document := ReportCSVDocument{
		Filename: "ai-intune-install.csv",
		Caveat:   InstallReportCaveat,
		Columns: []ReportCSVColumn{
			{Key: "resource", Header: "資源"},
			{Key: "resource_kind", Header: "resource_kind"},
			{Key: "resource_id", Header: "resource_id"},
			{Key: "machine", Header: "機器"},
			{Key: "machine_id", Header: "machine_id"},
			{Key: "state", Header: "這一格是什麼"},
			{Key: "assigned", Header: "指派的版號"},
			{Key: "observed", Header: "看到的版號"},
			{Key: "from_disk", Header: "看到的版號讀自檔案"},
			{Key: "runtime", Header: "版號講的是哪一份"},
			{Key: "measured_file", Header: "量版號的那個檔案"},
			{Key: "running_file", Header: "正在跑的那個檔案"},
			{Key: "scope", Header: "指派給誰"},
			{Key: "revision", Header: "revision"},
			{Key: "assigned_at", Header: "指派的時刻"},
			{Key: "assigned_by", Header: "指派的人"},
			{Key: "measured_at", Header: "機器量的時刻"},
			{Key: "observed_at", Header: "Hub 收到的時刻"},
			{Key: "next_step", Header: "下一步"},
		},
	}
	for _, resource := range report.Resources {
		for _, row := range resource.Rows {
			document.Rows = append(document.Rows, []string{
				resource.Name,
				resource.ResourceKind,
				resource.ResourceID,
				row.DisplayName,
				row.MachineID,
				row.Title,
				row.Assigned,
				row.Observed,
				installCSVBool(row.FromDisk),
				toolRuntimeCSV(row.Runtime, func(f ToolRuntimeFinding) string { return f.Title }),
				toolRuntimeCSV(row.Runtime, func(f ToolRuntimeFinding) string { return f.MeasuredFile }),
				toolRuntimeCSV(row.Runtime, func(f ToolRuntimeFinding) string { return f.RunningFile }),
				row.ScopeLabel,
				installCSVRevision(row.Revision),
				ReportCSVOptionalTime(row.AssignedAt),
				row.AssignedBy,
				ReportCSVOptionalTime(row.MeasuredAt),
				ReportCSVOptionalTime(row.ObservedAt),
				row.NextStep,
			})
		}
	}
	return document
}

func installCSVBool(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

// installCSVRevision 沒有指派的那一列是空格，不是 0。
//
// ⚠ 0 在試算表裡會被排序、被加總、被當成一個真的號碼；「這一列沒有指派」要看得出
// 來是沒有，不是一個號碼剛好是零的指派。
func installCSVRevision(revision int64) string {
	if revision <= 0 {
		return ""
	}
	return strconv.FormatInt(revision, 10)
}
