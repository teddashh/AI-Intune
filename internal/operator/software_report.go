package operator

// 軟體清查 —— 「機隊上裝了什麼、各是哪一版、哪幾台沒有」。
//
// ⚠⚠ 這份報告最危險的地方不是算錯，是**講太多**。這個 Hub 手上沒有任何上游的版本
// 來源：不知道 claude 最新是幾版、不知道 openclaw 今天發了什麼。所以它講得出口的
// 只有「這個機隊裡最新的是 X，這一台是 Y」，而**不是**「這一台該升級了」——後面那
// 句話需要一個我們沒有的事實。四台都落後兩個月的時候這份報告全綠，那不是 bug，那
// 是它能誠實說出的極限，而那句限制要寫在畫面上，不是只寫在這裡。
//
// ⚠ 它也不判斷任何一件事實。哪些機器算數，讀的是註冊報告在用的同一份名冊投影
// （machineSummariesFrom）；每一台每一個工具的最新一筆，讀的是 Store 那一份
// FleetTools。兩邊各自算一次的話，漂掉的樣子是同一台機器在兩頁上有兩個答案。

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// ⚠ 2 是因為每一列多了 Runtime：一個 strict decoder 對著沒有那個欄位的版本會整份
// 拒收，所以兩邊必須一起動。悄悄加欄位會讓舊的用戶端在解碼那一刻才壞，而那時候它
// 已經印了半份畫面。
const SoftwareReportSchemaVersion = 2

// MaxSoftwareReportMachines / MaxSoftwareReportTools 是一次讀得了多少。
//
// ⚠ 超過就拒絕，不截斷——跟註冊報告同一條理由：一份少算的清查看起來就是全部。
const (
	MaxSoftwareReportMachines = 2000
	MaxSoftwareReportTools    = 200
)

var ErrInvalidSoftwareReport = errors.New("operator: invalid software report request")

// SoftwareState 是一台機器對一個工具、畫面上那一格的全部。
//
// ⚠ 六種互斥且窮盡，而且全部是正面陳述——沒有一個是「不是 X」。最重要的是最後
// 兩種要分開：「有回報，它說這台上沒有」與「這台從來沒回報過這個工具」下一步完全
// 不同，前者要去裝東西，後者要去看那台的 agent。把兩者併成一個「沒有」，等於讓
// 沉默看起來像一個已知的答案。
type SoftwareState string

const (
	// SoftwareNewest：跟這個機隊裡看到的最新版一樣。
	SoftwareNewest SoftwareState = "newest"
	// SoftwareBehind：比這個機隊裡看到的最新版舊。
	SoftwareBehind SoftwareState = "behind"
	// SoftwareIncomparable：有版號，但跟機隊裡最新的那個比不出來。
	SoftwareIncomparable SoftwareState = "incomparable"
	// SoftwareNoVersion：裝了，但問不到版號。
	SoftwareNoVersion SoftwareState = "no_version"
	// SoftwareAbsent：有回報，而且說這台上沒有。
	SoftwareAbsent SoftwareState = "absent"
	// SoftwareUnreported：這台從來沒回報過這個工具。
	SoftwareUnreported SoftwareState = "unreported"
)

var softwareStateOrder = []SoftwareState{
	SoftwareNewest, SoftwareBehind, SoftwareIncomparable,
	SoftwareNoVersion, SoftwareAbsent, SoftwareUnreported,
}

// SoftwareStates returns every state in canonical order.
func SoftwareStates() []SoftwareState {
	return append([]SoftwareState(nil), softwareStateOrder...)
}

type softwareStateShape struct {
	title, meaning, nextStep string
	installed                bool
}

// ⚠ 每一個狀態各自帶一句「這是什麼」與一句「下一步做什麼」，Web、JSON、terminal
// 與 CSV 講的是同一組句子。沒有下一步的狀態不硬編一句——一台已經在最新版上的機器
// 沒有下一步。
var softwareStateShapes = map[SoftwareState]softwareStateShape{
	SoftwareNewest: {
		title:     "跟機隊裡最新的一樣",
		meaning:   "這台裝的版號，跟這個機隊裡看到的最新版一樣。",
		installed: true,
	},
	SoftwareBehind: {
		title:     "比機隊裡最新的舊",
		meaning:   "這台裝的版號，比這個機隊裡看到的最新版舊。",
		nextStep:  "要追上就在這台上升級；這個 Hub 不知道上游今天發了什麼，所以它只講機隊內部的差距。",
		installed: true,
	},
	SoftwareIncomparable: {
		title:     "比不出來",
		meaning:   "這台有版號，但它跟機隊裡最新的那個比不出先後。",
		nextStep:  "到這台的裝置頁看它自報的原字串。",
		installed: true,
	},
	SoftwareNoVersion: {
		title:     "裝了，問不到版號",
		meaning:   "這台上有這個工具，但問不到它的版號。",
		nextStep:  "到這台的裝置頁看它為什麼答不出版號。",
		installed: true,
	},
	SoftwareAbsent: {
		title:    "這台上沒有",
		meaning:  "這台回報過，而且說它上面沒有這個工具。",
		nextStep: "要用就在這台上安裝。",
	},
	SoftwareUnreported: {
		title:    "沒回報過",
		meaning:  "這台從來沒回報過這個工具，所以裝沒裝都還不知道。",
		nextStep: "到這台的裝置頁看 agent 有沒有在回報。",
	},
}

// SoftwareStateInstalled says whether this state means the tool is on the machine.
func SoftwareStateInstalled(stateValue SoftwareState) bool {
	return softwareStateShapes[stateValue].installed
}

// SoftwareStateTitle / SoftwareStateMeaning / SoftwareStateNextStep are the one
// set of sentences every surface prints.
func SoftwareStateTitle(stateValue SoftwareState) string {
	return softwareStateShapes[stateValue].title
}

func SoftwareStateMeaning(stateValue SoftwareState) string {
	return softwareStateShapes[stateValue].meaning
}

func SoftwareStateNextStep(stateValue SoftwareState) string {
	return softwareStateShapes[stateValue].nextStep
}

// SoftwareRow 是一台機器 × 一個工具。
type SoftwareRow struct {
	MachineID   string        `json:"machine_id"`
	DisplayName string        `json:"display_name"`
	State       SoftwareState `json:"state"`
	Title       string        `json:"title"`
	Meaning     string        `json:"meaning"`
	NextStep    string        `json:"next_step,omitempty"`

	// Version 是畫面上那個版號。空字串表示這一格沒有版號可講。
	Version string `json:"version,omitempty"`
	// FromDisk：這個版號是從檔案讀的，不是問它本人拿到的。
	FromDisk bool `json:"from_disk,omitempty"`
	// Shadowed：clawctl 自己那條 PATH 解到的是另一個檔案。
	Shadowed bool `json:"shadowed,omitempty"`

	// Runtime 是「上面那個版號，講的是不是正在跑的那一份」。
	//
	// ⚠⚠ 它跟 Shadowed 是兩個獨立的軸，不准合起來。Shadowed 問的是「人那條 PATH
	// 跟 daemon 那條 PATH 解到的是不是同一個檔案」，Runtime 問的是「我量版號的那個
	// 檔案跟現在活著的 process 在跑的是不是同一個」。正式機隊上 openclaw 四台
	// shadowed 是 false、Runtime 卻是「正在跑的是另一個檔案」——合成一個布林，那四
	// 格會說一切正常。
	//
	// ⚠ 只有這台上真的有這個工具的時候才有。沒回報過與這台上沒有的那兩格沒有版號，
	// 也就沒有「這個版號講的是哪一份」可問。
	Runtime *ToolRuntimeFinding `json:"runtime,omitempty"`

	// MeasuredAt 是 agent 自己的鐘，ObservedAt 是 Hub 收到的時刻。
	//
	// ⚠ 兩個都留著，而且「多久以前的事」一律用 ObservedAt 算。agent 的鐘會歪
	// （sampleagent4 快 80 秒），拿它當存活座標會讓一台停了的機器看起來剛回報過。
	MeasuredAt *time.Time `json:"measured_at,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// SoftwareVersion 是「這一版裝在幾台上」。
type SoftwareVersion struct {
	Version  string `json:"version"`
	Machines int    `json:"machines"`
	// Newest：這是這個機隊裡看到的最新版。比不出來的版號不會是 true。
	Newest bool `json:"newest"`
}

// SoftwareTool 是一個工具在整個機隊上的樣子。
type SoftwareTool struct {
	Name string `json:"name"`

	// InstalledOn / AbsentOn / UnreportedOn 三個加起來就是分母。
	InstalledOn  int `json:"installed_on"`
	AbsentOn     int `json:"absent_on"`
	UnreportedOn int `json:"unreported_on"`

	// Newest 是這個機隊裡看到的最新版號；一個都比不出來時是空字串。
	Newest string `json:"newest,omitempty"`
	// Spread 是看到幾種不同的版號。> 1 表示這個工具在機隊上不一致。
	Spread int `json:"spread"`

	Versions []SoftwareVersion `json:"versions"`
	Rows     []SoftwareRow     `json:"rows"`

	Headline string `json:"headline"`
	NextStep string `json:"next_step,omitempty"`
}

// SoftwareStateCount 是六種狀態各有幾格。
type SoftwareStateCount struct {
	State    SoftwareState `json:"state"`
	Title    string        `json:"title"`
	Count    int           `json:"count"`
	Meaning  string        `json:"meaning"`
	NextStep string        `json:"next_step,omitempty"`
}

// SoftwareReport 是整份清查。
type SoftwareReport struct {
	SchemaVersion int       `json:"schema_version"`
	EvaluatedAt   time.Time `json:"evaluated_at"`

	// Machines 是分母：名冊上沒有退役的台數，跟註冊報告的分母是同一個數。
	Machines int `json:"machines"`
	// Reporting 是分母裡至少回報過一個工具的台數。
	Reporting int `json:"reporting"`
	// Drifted 是機隊上版號不一致的工具數。
	Drifted int `json:"drifted"`

	// Misattributed 是「版號講的不是正在跑的那一份」的格數：正在跑的是另一個檔案，
	// 或者正在跑的那個檔案已經不在磁碟上。
	//
	// ⚠⚠ 它排在 Drifted 前面。這幾格的版號量的是沒在跑的那一份，所以拿它們去比
	// 「誰比較新」，比的是一個沒有人在用的檔案——版號不一致的判定本身就還不能信。
	Misattributed int `json:"misattributed"`

	Tools         []SoftwareTool       `json:"tools"`
	States        []SoftwareStateCount `json:"states"`
	RuntimeStates []ToolRuntimeCount   `json:"runtime_states"`

	Headline string `json:"headline"`
	// Caveat 是那句必須留在畫面上的限制。
	Caveat   string `json:"caveat"`
	NextStep string `json:"next_step,omitempty"`
}

// SoftwareReportCaveat 是這份報告講得出口的極限。
//
// ⚠ 它不是免責聲明，是這份報告的判準本身：沒有上游版本來源的時候，「最新」只可能
// 是「這個機隊裡最新」。不寫在畫面上的話，一張全綠的表會被讀成「都是最新的」。
const SoftwareReportCaveat = "「最新」指的是這個機隊裡看到的最新版，不是上游發布的最新版。"

// SoftwareReport 回答「機隊上裝了什麼、各是哪一版、哪幾台沒有」。
func (s *Service) SoftwareReport(evaluatedAt time.Time) (SoftwareReport, error) {
	if evaluatedAt.IsZero() {
		return SoftwareReport{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidSoftwareReport)
	}
	evaluatedAt = evaluatedAt.UTC()
	overview, err := s.store.Overview(evaluatedAt)
	if err != nil {
		return SoftwareReport{}, err
	}
	// ⚠ 機器來自名冊，不是來自觀測。讓觀測決定有哪些機器，等於讓「有回報的」變成
	// 分母——而一台從來沒回報過的機器正是最該被看到的那一台。
	machines := softwareMachinesFrom(machineSummariesFrom(overview, evaluatedAt))
	if len(machines) > MaxSoftwareReportMachines {
		return SoftwareReport{}, fmt.Errorf(
			"%w: denominator has %d machines, exceeding the %d machines this report can read at once",
			ErrInvalidSoftwareReport, len(machines), MaxSoftwareReportMachines)
	}
	rows, err := s.store.FleetTools()
	if err != nil {
		return SoftwareReport{}, err
	}

	inFleet := make(map[string]bool, len(machines))
	for _, machine := range machines {
		inFleet[machine.MachineID] = true
	}
	// ⚠ 工具清單取自觀測裡出現過的 subject 聯集，不是一份寫死的表。那份表住在
	// agent 那一側，Hub 不該依賴它；而真正的問題本來就是「機隊回報了什麼」。某一
	// 台的 agent 舊到還不認識某個工具時，它那一格會是「沒回報過」，那正是事實。
	observations := make(map[string]map[string]softwareObservation, len(machines))
	toolNames := map[string]bool{}
	for _, row := range rows {
		// ⚠ 退役機器的觀測不進來。它已經離開分母，而這份報告問的是「我的機隊上
		// 有什麼」。留著它會讓一台已經退役的機器繼續決定「最新版是哪一版」。
		if !inFleet[row.MachineID] {
			continue
		}
		toolNames[row.Name] = true
		if observations[row.MachineID] == nil {
			observations[row.MachineID] = map[string]softwareObservation{}
		}
		observations[row.MachineID][row.Name] = softwareObservation{
			tool:       row.CLITool,
			measuredAt: row.MeasuredAt,
			observedAt: row.ReceivedAt,
		}
	}
	if len(toolNames) > MaxSoftwareReportTools {
		return SoftwareReport{}, fmt.Errorf(
			"%w: fleet reported %d tools, exceeding the %d tools this report can read at once",
			ErrInvalidSoftwareReport, len(toolNames), MaxSoftwareReportTools)
	}

	report := SoftwareReport{
		SchemaVersion: SoftwareReportSchemaVersion,
		EvaluatedAt:   evaluatedAt,
		Machines:      len(machines),
		Caveat:        SoftwareReportCaveat,
		// ⚠ 空工具是這份報告有專屬句子的設計狀態；留成 nil 會輸出 JSON null，讓 Hub 自己的 strict client 拒收整頁。
		Tools: []SoftwareTool{},
	}
	for _, machine := range machines {
		if len(observations[machine.MachineID]) > 0 {
			report.Reporting++
		}
	}
	counts := map[SoftwareState]int{}
	runtimeCounts := map[ToolRuntime]int{}
	for _, name := range sortedSoftwareNames(toolNames) {
		tool := softwareToolFor(name, machines, observations)
		for _, row := range tool.Rows {
			counts[row.State]++
			if row.Runtime == nil {
				continue
			}
			runtimeCounts[row.Runtime.State]++
			if ToolRuntimeMisattributed(row.Runtime.State) {
				report.Misattributed++
			}
		}
		if tool.Spread > 1 {
			report.Drifted++
		}
		report.Tools = append(report.Tools, tool)
	}
	for _, stateValue := range softwareStateOrder {
		shape := softwareStateShapes[stateValue]
		report.States = append(report.States, SoftwareStateCount{
			State: stateValue, Title: shape.title, Count: counts[stateValue],
			Meaning: shape.meaning, NextStep: shape.nextStep,
		})
	}
	for _, stateValue := range ToolRuntimes() {
		report.RuntimeStates = append(report.RuntimeStates, ToolRuntimeCount{
			State: stateValue, Title: ToolRuntimeTitle(stateValue),
			Count: runtimeCounts[stateValue], Meaning: ToolRuntimeMeaning(stateValue),
			NextStep: ToolRuntimeNextStep(stateValue),
		})
	}
	report.Headline = SoftwareReportHeadline(report)
	report.NextStep = softwareReportNextStep(report)
	return report, nil
}

// softwareObservation 是一台機器對一個工具的最新一筆。
type softwareObservation struct {
	tool       model.CLITool
	measuredAt time.Time
	observedAt time.Time
}

// softwareMachine 是這份報告需要知道的關於一台機器的全部：它的 id 跟名字。
//
// ⚠ 刻意不帶判決與心跳。讓它看得到那些，下一個人就會忍不住在這裡再寫一次「健康」
// 的判斷，然後兩處判斷會在某一天分岔。
type softwareMachine struct {
	MachineID   string
	DisplayName string
}

// softwareMachinesFrom 取分母：名冊上沒有退役的那些。
func softwareMachinesFrom(summaries []MachineSummary) []softwareMachine {
	out := make([]softwareMachine, 0, len(summaries))
	for _, summary := range summaries {
		if summary.RetiredAt != nil {
			continue
		}
		out = append(out, softwareMachine{
			MachineID:   summary.MachineID,
			DisplayName: softwareMachineLabel(summary),
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

// softwareMachineLabel 寧可退到 machine_id 也不留白——一列沒有名字的報告，人沒有
// 辦法知道它講的是哪一台。
func softwareMachineLabel(summary MachineSummary) string {
	if summary.DisplayName != "" {
		return summary.DisplayName
	}
	return summary.MachineID
}

// softwareToolFor 把一個工具在整個機隊上的樣子算出來。
func softwareToolFor(name string, machines []softwareMachine,
	observations map[string]map[string]softwareObservation,
) SoftwareTool {
	tool := SoftwareTool{Name: name, Versions: []SoftwareVersion{}, Rows: []SoftwareRow{}}
	distinct := map[string]int{}
	for _, machine := range machines {
		row := SoftwareRow{MachineID: machine.MachineID, DisplayName: machine.DisplayName}
		observation, reported := observations[machine.MachineID][name]
		switch {
		case !reported:
			row.State = SoftwareUnreported
		case !observation.tool.Present:
			row.State = SoftwareAbsent
			row.MeasuredAt, row.ObservedAt = softwareTimes(observation)
		default:
			row.MeasuredAt, row.ObservedAt = softwareTimes(observation)
			row.Shadowed = observation.tool.DaemonReach == model.DaemonReachShadowed
			runtime := ToolRuntimeOf(observation.tool)
			row.Runtime = &runtime
			row.Version = observation.tool.VersionReported
			if row.Version == "" && observation.tool.VersionPackageJSON != "" {
				// 檔案上讀到的算數，但要說清楚它不是它本人講的。
				row.Version, row.FromDisk = observation.tool.VersionPackageJSON, true
			}
			if row.Version == "" {
				row.State = SoftwareNoVersion
			} else {
				// 先記成落後，等知道誰是最新的再定案。
				row.State = SoftwareBehind
				distinct[row.Version]++
			}
		}
		tool.Rows = append(tool.Rows, row)
	}

	tool.Newest = newestSoftwareVersion(distinct)
	for i := range tool.Rows {
		row := &tool.Rows[i]
		if row.State != SoftwareBehind {
			continue
		}
		switch cmp, ok := compareSoftwareVersions(row.Version, tool.Newest); {
		case !ok:
			row.State = SoftwareIncomparable
		case cmp >= 0:
			row.State = SoftwareNewest
		}
	}
	for i := range tool.Rows {
		row := &tool.Rows[i]
		shape := softwareStateShapes[row.State]
		row.Title, row.Meaning, row.NextStep = shape.title, shape.meaning, shape.nextStep
		if SoftwareStateInstalled(row.State) {
			tool.InstalledOn++
			continue
		}
		if row.State == SoftwareAbsent {
			tool.AbsentOn++
			continue
		}
		tool.UnreportedOn++
	}

	for _, version := range sortedSoftwareVersions(distinct) {
		tool.Versions = append(tool.Versions, SoftwareVersion{
			Version: version, Machines: distinct[version], Newest: version == tool.Newest,
		})
	}
	tool.Spread = len(distinct)
	tool.Headline = SoftwareToolHeadline(tool)
	tool.NextStep = softwareToolNextStep(tool)
	return tool
}

func softwareTimes(observation softwareObservation) (*time.Time, *time.Time) {
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

// newestSoftwareVersion 挑出機隊裡最新的那一版。
//
// ⚠ 比不出來的不參加選舉：連跟自己都比不出來的東西不能當基準。先排序只是為了
// 決定性——同一版不同寫法（2.1 / 2.1.0）每次都要選到同一個。
func newestSoftwareVersion(distinct map[string]int) string {
	candidates := make([]string, 0, len(distinct))
	for version := range distinct {
		if _, ok := compareSoftwareVersions(version, version); !ok {
			continue
		}
		candidates = append(candidates, version)
	}
	sort.Strings(candidates)
	newest := ""
	for _, version := range candidates {
		if newest == "" {
			newest = version
			continue
		}
		if cmp, ok := compareSoftwareVersions(version, newest); ok && cmp > 0 {
			newest = version
		}
	}
	return newest
}

// sortedSoftwareVersions 讓版號清單每次的順序一樣：新的在前，比不出來的排在後面，
// 同一級再照字串排。
func sortedSoftwareVersions(distinct map[string]int) []string {
	out := make([]string, 0, len(distinct))
	for version := range distinct {
		out = append(out, version)
	}
	sort.Slice(out, func(i, j int) bool {
		cmp, ok := compareSoftwareVersions(out[i], out[j])
		if ok && cmp != 0 {
			return cmp > 0
		}
		_, iComparable := compareSoftwareVersions(out[i], out[i])
		_, jComparable := compareSoftwareVersions(out[j], out[j])
		if iComparable != jComparable {
			return iComparable
		}
		return out[i] < out[j]
	})
	return out
}

func sortedSoftwareNames(names map[string]bool) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// compareSoftwareVersions 比兩個點分數字版號，回 (-1/0/+1, 比得出來嗎)。
//
// ⚠⚠ 不可以用字串比大小。實測 samplehub1 的 grok 自報 1.0.3、sampleagent2 是 1.0.13——
// 字串比會說 1.0.3 比較新，於是畫面會叫人把 sampleagent2 那台**降級**。openclaw 用日期
// 版號（2026.6.10、2026.5.20），同一套數字比法剛好也對。
//
// ⚠⚠ 比不出來就回 false，不准挑一個順序。這裡是整份報告唯一「憑空生出一個答案」
// 的機會，而一個猜出來的「落後」會讓人動手改一台其實沒問題的機器。
func compareSoftwareVersions(a, b string) (int, bool) {
	as, aok := softwareVersionParts(a)
	bs, bok := softwareVersionParts(b)
	if !aok || !bok {
		return 0, false
	}
	for i := 0; i < len(as) || i < len(bs); i++ {
		// ⚠ 段數不同時把缺的當 0：2.1 跟 2.1.0 是同一版。
		x, y := 0, 0
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// softwareVersionParts 把 "2.1.258" 拆成 [2 1 258]。有任何一段不是純數字就不算數。
//
// ⚠ 刻意嚴格。"1.0.3-beta" 這種東西**不要**去猜它跟 1.0.3 誰新——那正是上游會拿來
// 表達「這是 pre-release」的方式，而這個 Hub 沒有規則可以判。
func softwareVersionParts(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	segments := strings.Split(v, ".")
	out := make([]int, 0, len(segments))
	for _, segment := range segments {
		n, err := strconv.Atoi(segment)
		if err != nil || n < 0 || strconv.Itoa(n) != segment {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// SoftwareToolHeadline 是一個工具那一行字。
func SoftwareToolHeadline(tool SoftwareTool) string {
	total := tool.InstalledOn + tool.AbsentOn + tool.UnreportedOn
	if tool.InstalledOn == 0 {
		if tool.UnreportedOn == total {
			return fmt.Sprintf("%s：%d 台都還沒回報過這個工具。", tool.Name, total)
		}
		// ⚠⚠ 「都沒有」只有在每一台都回報過的時候才是實話。把沒回報過的那幾台算進
		// 去，這一行字就把沉默講成了一個已知的答案——而這一行正是最多人只讀它就走的
		// 地方。一台沒裝的要去裝東西，一台沒回報的要去看它的 agent。
		if tool.UnreportedOn > 0 {
			return fmt.Sprintf("%s：%d 台上沒有；另有 %d 台沒回報過。",
				tool.Name, tool.AbsentOn, tool.UnreportedOn)
		}
		return fmt.Sprintf("%s：%d 台上都沒有。", tool.Name, total)
	}
	headline := fmt.Sprintf("%s：%d/%d 台上有", tool.Name, tool.InstalledOn, total)
	switch {
	case tool.Spread > 1:
		headline += fmt.Sprintf("，%d 種版號不一致", tool.Spread)
	case tool.Newest != "":
		headline += fmt.Sprintf("，都是 %s", tool.Newest)
	}
	if tool.UnreportedOn > 0 {
		headline += fmt.Sprintf("；另有 %d 台沒回報過", tool.UnreportedOn)
	}
	return headline + "。"
}

// softwareToolNextStep 只有在有事可做的時候才給一句。
func softwareToolNextStep(tool SoftwareTool) string {
	if tool.Spread > 1 && tool.Newest != "" {
		return fmt.Sprintf("要拉齊就把落後的那幾台升到 %s。", tool.Newest)
	}
	if tool.UnreportedOn > 0 {
		return "沒回報過的那幾台先去看 agent 有沒有在回報。"
	}
	return ""
}

// SoftwareReportHeadline 是整份清查那一行字。
func SoftwareReportHeadline(report SoftwareReport) string {
	if report.Machines == 0 {
		return "名冊上沒有機器，沒有東西可以清查。"
	}
	if len(report.Tools) == 0 {
		return fmt.Sprintf("分母 %d 台，還沒有任何一台回報過工具。", report.Machines)
	}
	headline := fmt.Sprintf("分母 %d 台，%d 台回報過；%d 個工具",
		report.Machines, report.Reporting, len(report.Tools))
	if report.Drifted == 0 {
		headline += "的版號在機隊上都一致"
	} else {
		headline += fmt.Sprintf("裡有 %d 個的版號在機隊上不一致", report.Drifted)
	}
	// ⚠⚠ 這一句跟在版號那一句後面，而且不能省。有格子的版號量的是沒在跑的那一份時，
	// 上面那個「都一致」講的是一批沒有人在用的檔案——一張全綠的表少了這一句，會被讀成
	// 機隊上跑的東西都對得起來。
	if report.Misattributed > 0 {
		return headline + fmt.Sprintf("；另有 %d 格的版號講的不是正在跑的那一份。",
			report.Misattributed)
	}
	return headline + "。"
}

// softwareReportNextStep 只有在有事可做的時候才給一句。
func softwareReportNextStep(report SoftwareReport) string {
	if report.Machines > 0 && report.Reporting < report.Machines {
		return fmt.Sprintf("有 %d 台一個工具都沒回報過，先去看它們的 agent 有沒有在回報。",
			report.Machines-report.Reporting)
	}
	// ⚠⚠ 排在版號不一致前面。那幾格的版號量的是沒在跑的那一份，所以「誰比較新」比的
	// 是一個沒有人在用的檔案——先確定在比哪一個檔案，再談哪一台落後。
	if report.Misattributed > 0 {
		return "先看那幾格版號講的不是正在跑的那一份，再談哪一台落後。"
	}
	if report.Drifted > 0 {
		return "版號不一致的工具點開來看是哪幾台落後。"
	}
	return ""
}

// SoftwareReportExportPath is where the whole inventory can be taken away.
func SoftwareReportExportPath() string { return "/reports/software.csv" }

// SoftwareReportCSV 匯出的就是畫面上那些列，一列不多一列不少。
//
// ⚠ 一列是「一台機器 × 一個工具」，不是一個工具一列。把版本分佈壓成一格，匯出的
// 檔案就答不出「哪一台落後」——而那正是有人把它開在試算表裡要問的問題。
func SoftwareReportCSV(report SoftwareReport) ReportCSVDocument {
	document := ReportCSVDocument{
		Filename: "ai-intune-software.csv",
		Caveat:   SoftwareReportCaveat,
		Columns: []ReportCSVColumn{
			{Key: "tool", Header: "工具"},
			{Key: "machine", Header: "機器"},
			{Key: "machine_id", Header: "machine_id"},
			{Key: "state", Header: "這一格是什麼"},
			{Key: "version", Header: "版號"},
			{Key: "from_disk", Header: "版號讀自檔案"},
			{Key: "shadowed", Header: "被另一條 PATH 遮住"},
			{Key: "runtime", Header: "版號講的是哪一份"},
			{Key: "measured_file", Header: "量版號的那個檔案"},
			{Key: "running_file", Header: "正在跑的那個檔案"},
			{Key: "fleet_newest", Header: "機隊裡最新"},
			{Key: "measured_at", Header: "機器量的時刻"},
			{Key: "observed_at", Header: "Hub 收到的時刻"},
			{Key: "next_step", Header: "下一步"},
		},
	}
	for _, tool := range report.Tools {
		for _, row := range tool.Rows {
			document.Rows = append(document.Rows, []string{
				tool.Name,
				row.DisplayName,
				row.MachineID,
				row.Title,
				row.Version,
				softwareCSVBool(row.FromDisk),
				softwareCSVBool(row.Shadowed),
				toolRuntimeCSV(row.Runtime, func(f ToolRuntimeFinding) string { return f.Title }),
				toolRuntimeCSV(row.Runtime, func(f ToolRuntimeFinding) string { return f.MeasuredFile }),
				toolRuntimeCSV(row.Runtime, func(f ToolRuntimeFinding) string { return f.RunningFile }),
				tool.Newest,
				ReportCSVOptionalTime(row.MeasuredAt),
				ReportCSVOptionalTime(row.ObservedAt),
				row.NextStep,
			})
		}
	}
	return document
}

func softwareCSVBool(value bool) string {
	if value {
		return "是"
	}
	return "否"
}
