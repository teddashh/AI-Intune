package operator

// 發佈的 vs 指派的 —— 「這份 profile 發佈了，然後呢」。
//
// ⚠⚠ 每機安裝狀態回答的是「指派的 vs 看到的」，缺的那一半在它前面：一份發佈了卻
// 一台都沒指派的 profile，在那一頁上完全不存在——那一頁的分母是機器，而這份 profile
// 沒有機器。正式庫 2026-09-12 就是這個形狀：machine_profiles 1 列
// （openclaw-standard rev 1，點名 openclaw 2026.9.2），machine_profile_assignments
// 0 列，而 desired_state 從來沒有指派過 2026.9.2，四台機器上也沒有一台是那一版。
// 也就是說 console 上有一份發佈了、點名了一個這個系統裡哪裡都不存在的版本、而且
// 一台都沒指派的 profile，而 /apps 的兩頁都沒有講這三件事的任何一件。
//
// ⚠⚠ 這一頁講得出口的只有 Hub 自己持有的三件事：這份 profile 現在穿在幾台身上、
// 它點名的版本這個 Hub 有沒有指派過、有沒有在任何一台上看到過。它**不**回答
// 「2026.9.2 是不是最新」——那句話需要一個上游版本來源，而這個 Hub 沒有。
//
// ⚠ 分母是名冊，不是指派也不是 profile。讓指派決定分母的話，「一台都沒指派」這件
// 事會變成一個空集合，而空集合在畫面上跟「沒有這個東西」長得一模一樣。

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// ProfileReportSchemaVersion 是這份報告的形狀版號。
//
// ⚠ 加一個欄位就要動它。用戶端的解碼器不收不認識的欄位，所以多一個欄位對舊的用戶端
// 就是整份拒收——版號不動的話，那個拒收會看起來像 Hub 掛了。
const ProfileReportSchemaVersion = 2

// MaxProfileReportProfiles / MaxProfileReportMachines 是一次讀得了多少。
//
// ⚠ 超過就拒絕，不截斷——跟其他報告同一條理由：一份少算的報告看起來就是全部。
const (
	MaxProfileReportProfiles = 500
	MaxProfileReportMachines = 2000
)

var ErrInvalidProfileReport = errors.New("operator: invalid profile report request")

// ProfileState 是一份已發佈的 profile revision 在畫面上那一列的全部。
//
// ⚠ 四種互斥且窮盡，而且全部是正面陳述。最重要的是後面兩種要分開：「沒有機器穿著
// 它，但它已經被同一個 profile 的新版取代」是舊 revision 的預期樣子，「沒有機器穿
// 著它，而它就是最新的那一版」是一個發現。併成一個「沒有指派」的話，唯一值得看的
// 那一列會被埋在一堆歷史版本裡。
type ProfileState string

const (
	// ProfileInUse：名冊上有機器現在身上就是這一版。
	ProfileInUse ProfileState = "in_use"
	// ProfileRetiredOnly：穿著這一版的機器都已經退役。
	ProfileRetiredOnly ProfileState = "retired_only"
	// ProfileReplaced：沒有機器穿著它，而同一個 profile 已經發佈到更新的 revision。
	ProfileReplaced ProfileState = "replaced"
	// ProfileUnassigned：它就是這個 profile 最新的一版，而一台都沒指派。
	ProfileUnassigned ProfileState = "unassigned"
)

var profileStateOrder = []ProfileState{
	ProfileInUse, ProfileRetiredOnly, ProfileReplaced, ProfileUnassigned,
}

// ProfileStates returns every profile state in canonical order.
func ProfileStates() []ProfileState {
	return append([]ProfileState{}, profileStateOrder...)
}

// ProfilePackageState 是一份 profile 點名的一個（套件, 版本）在這個 Hub 身上的樣子。
//
// ⚠⚠ 兩個軸各自獨立：指派過沒有、看到過沒有。它們不是同一件事的兩種說法——一版被
// 指派過卻沒有在任何一台上看到，是工作單那一側的問題；一版沒有被指派過卻看得到，是
// 有人從指派以外的路徑裝上去的。合成一個「有沒有」會把這兩個相反的發現寫成同一格。
type ProfilePackageState string

const (
	// ProfilePackageAssignedAndSeen：指派過這一版，機隊上也看得到。
	ProfilePackageAssignedAndSeen ProfilePackageState = "assigned_and_seen"
	// ProfilePackageAssignedNotSeen：指派過這一版，沒有一台回報它。
	ProfilePackageAssignedNotSeen ProfilePackageState = "assigned_not_seen"
	// ProfilePackageSeenNotAssigned：沒有指派過這一版，機隊上看得到。
	ProfilePackageSeenNotAssigned ProfilePackageState = "seen_not_assigned"
	// ProfilePackageNeither：沒有指派過這一版，也沒有在任何一台上看到。
	ProfilePackageNeither ProfilePackageState = "neither"
)

var profilePackageStateOrder = []ProfilePackageState{
	ProfilePackageAssignedAndSeen, ProfilePackageAssignedNotSeen,
	ProfilePackageSeenNotAssigned, ProfilePackageNeither,
}

// ProfilePackageStates returns every package state in canonical order.
func ProfilePackageStates() []ProfilePackageState {
	return append([]ProfilePackageState{}, profilePackageStateOrder...)
}

// profilePackageStateAxes 是那兩個軸與四種狀態之間唯一的一張對照表。
//
// ⚠⚠ 兩個方向都走這一張表：算狀態的時候從兩個軸查過來，讀狀態的時候拆回兩個軸。
// 兩邊各寫一次的話，其中一邊多一種說法，「指派過但沒有一台回報它」就會在某一個平面
// 上變成「沒有指派過但看得到」——而那兩件事要人做的事剛好相反。
var profilePackageStateAxes = map[ProfilePackageState]struct{ everAssigned, everSeen bool }{
	ProfilePackageAssignedAndSeen: {everAssigned: true, everSeen: true},
	ProfilePackageAssignedNotSeen: {everAssigned: true},
	ProfilePackageSeenNotAssigned: {everSeen: true},
	ProfilePackageNeither:         {},
}

// ProfilePackageStateAxes 把一個狀態拆回它的兩個軸。known 是 false 表示這個版本
// 不認得這個狀態。
func ProfilePackageStateAxes(stateValue ProfilePackageState) (everAssigned, everSeen, known bool) {
	axes, ok := profilePackageStateAxes[stateValue]
	return axes.everAssigned, axes.everSeen, ok
}

// profilePackageStateFor 是同一張表的另一個方向。
func profilePackageStateFor(everAssigned, everSeen bool) ProfilePackageState {
	for _, candidate := range profilePackageStateOrder {
		axes := profilePackageStateAxes[candidate]
		if axes.everAssigned == everAssigned && axes.everSeen == everSeen {
			return candidate
		}
	}
	return ""
}

type profileSentences struct {
	title    string
	meaning  string
	nextStep string
}

// ⚠ 每一個狀態各自帶一句「這是什麼」與一句「下一步做什麼」，Web、JSON、terminal 與
// CSV 講的是同一組句子。正在管著機器的那一版沒有下一步——硬編一句會讓人以為還有事
// 要做。
var profileStateSentences = map[ProfileState]profileSentences{
	ProfileInUse: {
		title:   "機隊上有機器穿著這一版",
		meaning: "名冊上有機器現在身上就是這一份 profile 的這一版。",
	},
	ProfileRetiredOnly: {
		title:    "只有已退役的機器身上還是這一版",
		meaning:  "穿著這一版的機器都已經退役，名冊上沒有一台在用它。",
		nextStep: "決定這一版還留不留著，或者把它指派給名冊上的機器。",
	},
	ProfileReplaced: {
		title:   "這個 profile 已經發佈到更新的版本",
		meaning: "沒有機器穿著這一版，而同一個 profile 有更新的 revision。",
	},
	ProfileUnassigned: {
		title:    "發佈了，一台都沒指派",
		meaning:  "這是這個 profile 最新的一版，而名冊上沒有一台機器身上是它。",
		nextStep: "把它指派給機器，或者確認它本來就只是備著。",
	},
}

var profilePackageStateSentences = map[ProfilePackageState]profileSentences{
	ProfilePackageAssignedAndSeen: {
		title:   "指派過這一版，機隊上也看得到",
		meaning: "這個 Hub 指派過這個套件的這一版，而且有機器回報它身上就是這一版。",
	},
	ProfilePackageAssignedNotSeen: {
		title:    "指派過這一版，沒有一台回報它",
		meaning:  "這個 Hub 指派過這個套件的這一版，而名冊上沒有一台回報它身上是這一版。",
		nextStep: "到每機安裝狀態看這個資源的工作單走到哪裡了。",
	},
	ProfilePackageSeenNotAssigned: {
		title:    "這個 Hub 沒有指派過這一版，機隊上看得到",
		meaning:  "名冊上有機器回報它身上是這一版，而這個 Hub 從來沒有指派過它。",
		nextStep: "決定這一版是不是要的：它是從指派以外的路徑上去的。",
	},
	ProfilePackageNeither: {
		title:    "這個 Hub 沒有指派過這一版，也沒有看到過",
		meaning:  "這份 profile 點名的這一版，這個 Hub 從來沒有指派過，也沒有在任何一台上看到。",
		nextStep: "確認這一版存不存在，再決定要不要把這份 profile 指派出去。",
	},
}

// ProfileStateTitle / ProfileStateMeaning / ProfileStateNextStep are the one set
// of sentences every surface prints.
func ProfileStateTitle(stateValue ProfileState) string {
	return profileStateSentences[stateValue].title
}

func ProfileStateMeaning(stateValue ProfileState) string {
	return profileStateSentences[stateValue].meaning
}

func ProfileStateNextStep(stateValue ProfileState) string {
	return profileStateSentences[stateValue].nextStep
}

// ProfilePackageStateTitle / ProfilePackageStateMeaning / ProfilePackageStateNextStep
// are the one set of sentences every surface prints.
func ProfilePackageStateTitle(stateValue ProfilePackageState) string {
	return profilePackageStateSentences[stateValue].title
}

func ProfilePackageStateMeaning(stateValue ProfilePackageState) string {
	return profilePackageStateSentences[stateValue].meaning
}

func ProfilePackageStateNextStep(stateValue ProfilePackageState) string {
	return profilePackageStateSentences[stateValue].nextStep
}

const profilePackageMisattributedNextStep = "版號量的是沒在跑的那一份；到每機安裝狀態核對執行檔與其版號。"

// ProfilePackageNextStep 是一格自己的下一步。
//
// ⚠⚠ 看得到的那幾台只要有一台量的是沒在跑的那一份，「看得到」就不能照狀態那一句收工：
// 先核對執行檔。Hub 與用戶端都走這一個函式，同一格不會有兩種說法。摘要那一列講的是
// 整個狀態，照樣用 ProfilePackageStateNextStep。
func ProfilePackageNextStep(stateValue ProfilePackageState, seenMisattributedOn int) string {
	if seenMisattributedOn > 0 {
		return profilePackageMisattributedNextStep
	}
	return profilePackageStateSentences[stateValue].nextStep
}

// ProfileMachineRef 是一台現在穿著這一版的機器。
type ProfileMachineRef struct {
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name"`
	// Retired：這台已經退役，所以它不在分母裡。⚠ 它照樣列出來：一份「只有退役
	// 機器還穿著它」的 profile，跟一份誰都沒穿的 profile 要做的事不一樣。
	Retired            bool      `json:"retired,omitempty"`
	AssignmentID       string    `json:"assignment_id"`
	AssignmentRevision int64     `json:"assignment_revision"`
	AssignedAt         time.Time `json:"assigned_at"`
	AssignedBy         string    `json:"assigned_by"`
}

// ProfilePackage 是一份 profile 點名的一個套件版本。
type ProfilePackage struct {
	PackageID string `json:"package_id"`
	Version   string `json:"version"`

	State    ProfilePackageState `json:"state"`
	Title    string              `json:"title"`
	Meaning  string              `json:"meaning"`
	NextStep string              `json:"next_step,omitempty"`

	// Intents 是帳本上有幾列安裝意圖點名過這一版，LastAssignedAt 是最後一次。
	// ⚠ 讀的是整條歷史，不是現行意圖：「現在沒有人被叫去裝它」與「從來沒有人被
	// 叫去裝它」是兩句話。
	Intents        int        `json:"intents"`
	LastAssignedAt *time.Time `json:"last_assigned_at,omitempty"`

	// SeenOn 是名冊上有幾台回報它身上就是這一版。
	SeenOn int `json:"seen_on"`
	// SeenMisattributedOn 是那幾台裡面，那個版號量在一個沒有人在跑的檔案上的台數。
	//
	// ⚠⚠ 「看得到」是這一頁唯一一個講得出「它真的上去了」的證據，而它是一個版號字串
	// ——那個字串可以是量在一份沒有人在跑的安裝上的。正式庫 2026-09-12 samplehub1 的
	// openclaw 就是這樣：版號量在 login copy 上，在跑的是 releases/<ver>/ 那一份。
	// 沒有這個數字的話，一句「指派過這一版，機隊上也看得到」會被讀成收工。
	//
	// ⚠ 它是獨立的一軸，不是一種新的狀態。做成第五種狀態的話，四種狀態會變成八種，
	// 而「這個 Hub 有沒有指派過」跟「看到的那個版號是哪一份」是兩個不相干的問題。
	SeenMisattributedOn int `json:"seen_misattributed_on"`
}

// ProfileRow 是一份已發佈的 profile revision。
type ProfileRow struct {
	ProfileID   string    `json:"profile_id"`
	Revision    int64     `json:"revision"`
	Digest      string    `json:"digest"`
	PublishedAt time.Time `json:"published_at"`
	PublishedBy string    `json:"published_by"`

	State    ProfileState `json:"state"`
	Title    string       `json:"title"`
	Meaning  string       `json:"meaning"`
	NextStep string       `json:"next_step,omitempty"`

	// AssignedOn 是名冊上穿著這一版的台數；RetiredOn 是已退役卻還穿著它的台數。
	AssignedOn int `json:"assigned_on"`
	RetiredOn  int `json:"retired_on"`

	Machines []ProfileMachineRef `json:"machines"`
	Packages []ProfilePackage    `json:"packages"`

	Headline string `json:"headline"`
}

// ProfileStateCount / ProfilePackageStateCount 是每一種狀態各有幾列。
type ProfileStateCount struct {
	State    ProfileState `json:"state"`
	Title    string       `json:"title"`
	Count    int          `json:"count"`
	Meaning  string       `json:"meaning"`
	NextStep string       `json:"next_step,omitempty"`
}

type ProfilePackageStateCount struct {
	State    ProfilePackageState `json:"state"`
	Title    string              `json:"title"`
	Count    int                 `json:"count"`
	Meaning  string              `json:"meaning"`
	NextStep string              `json:"next_step,omitempty"`
}

// ProfileReport 是整份「發佈的 vs 指派的」。
type ProfileReport struct {
	SchemaVersion int       `json:"schema_version"`
	EvaluatedAt   time.Time `json:"evaluated_at"`

	// Machines 是分母：名冊上沒有退役的台數，跟註冊報告與每機安裝狀態同一個數。
	Machines int `json:"machines"`
	// Wearing 是分母裡現在身上有一份 profile 的台數，Bare 是沒有的。
	Wearing int `json:"wearing"`
	Bare    int `json:"bare"`

	// Published 是已發佈的 profile revision 數，Unassigned 是其中「最新的一版而
	// 一台都沒指派」的份數。
	Published  int `json:"published"`
	Unassigned int `json:"unassigned"`

	// SeenMisattributed 是有幾格「看得到」的證據，量的是一份沒有人在跑的安裝。
	//
	// ⚠ 一格是「一份已發佈的 revision × 它點名的一個套件版本」，跟匯出檔一列同一個
	// 粒度。改成算台數的話，同一台機器會在好幾份 profile 上被重複算進來。
	SeenMisattributed int `json:"seen_misattributed"`

	Profiles      []ProfileRow               `json:"profiles"`
	States        []ProfileStateCount        `json:"states"`
	PackageStates []ProfilePackageStateCount `json:"package_states"`

	Headline string `json:"headline"`
	// Caveat 是那句必須留在畫面上的限制。
	Caveat   string `json:"caveat"`
	NextStep string `json:"next_step,omitempty"`
}

// ProfileReportCaveat 是這份報告講得出口的極限。
//
// ⚠ 它不是免責聲明，是這份報告的判準本身：Hub 只知道自己發佈過什麼、指派過什麼與
// 自己看到什麼。不寫在畫面上的話，「這個 Hub 沒有指派過這一版，也沒有看到過」會被
// 讀成「這一版不存在」——而這個 Hub 沒有資格講那句話。
const ProfileReportCaveat = "這份報告比的是 Hub 發佈過什麼、指派過什麼與看到什麼；它沒有上游版本來源，也看不到指派以外的安裝路徑。"

// ProfileReport 回答「這份 profile 發佈了，然後呢」。
func (s *Service) ProfileReport(evaluatedAt time.Time) (ProfileReport, error) {
	if evaluatedAt.IsZero() {
		return ProfileReport{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidProfileReport)
	}
	evaluatedAt = evaluatedAt.UTC()
	overview, err := s.store.Overview(evaluatedAt)
	if err != nil {
		return ProfileReport{}, err
	}
	summaries := machineSummariesFrom(overview, evaluatedAt)
	if len(summaries) > MaxProfileReportMachines {
		return ProfileReport{}, fmt.Errorf(
			"%w: 名冊上有 %d 台，超過這份報告一次讀得了的 %d 台",
			ErrInvalidProfileReport, len(summaries), MaxProfileReportMachines)
	}
	// ⚠ 名冊上的每一台都要有名字與退役與否，連退役的都要：一份「只有退役機器還穿
	// 著它」的 profile 要講得出那幾台是誰。
	labels := make(map[string]string, len(summaries))
	retired := make(map[string]bool, len(summaries))
	fleet := 0
	for _, summary := range summaries {
		labels[summary.MachineID] = installMachineLabel(summary)
		if summary.RetiredAt != nil {
			retired[summary.MachineID] = true
			continue
		}
		fleet++
	}

	profiles, err := s.store.MachineProfiles()
	if err != nil {
		return ProfileReport{}, err
	}
	if len(profiles) > MaxProfileReportProfiles {
		return ProfileReport{}, fmt.Errorf(
			"%w: 發佈了 %d 版 profile，超過這份報告一次讀得了的 %d 版",
			ErrInvalidProfileReport, len(profiles), MaxProfileReportProfiles)
	}
	assignments, err := s.store.FleetProfileAssignments()
	if err != nil {
		return ProfileReport{}, err
	}
	assignedVersions, err := s.store.FleetIntentVersions()
	if err != nil {
		return ProfileReport{}, err
	}
	tools, err := s.store.FleetTools()
	if err != nil {
		return ProfileReport{}, err
	}

	wearers := profileWearers(assignments, labels, retired)
	assigned := profileAssignedVersions(assignedVersions)
	seen := profileSeenVersions(tools, retired)
	newest := profileNewestRevisions(profiles)

	report := ProfileReport{
		SchemaVersion: ProfileReportSchemaVersion,
		EvaluatedAt:   evaluatedAt,
		Machines:      fleet,
		Published:     len(profiles),
		Caveat:        ProfileReportCaveat,
		Profiles:      []ProfileRow{},
	}
	for _, assignment := range assignments {
		if !retired[assignment.MachineID] {
			report.Wearing++
		}
	}
	report.Bare = fleet - report.Wearing

	stateCounts := map[ProfileState]int{}
	packageCounts := map[ProfilePackageState]int{}
	for _, record := range profiles {
		row := profileRowFor(record, wearers, assigned, seen, newest)
		stateCounts[row.State]++
		for _, pkg := range row.Packages {
			packageCounts[pkg.State]++
			if pkg.SeenMisattributedOn > 0 {
				report.SeenMisattributed++
			}
		}
		if row.State == ProfileUnassigned {
			report.Unassigned++
		}
		report.Profiles = append(report.Profiles, row)
	}
	for _, stateValue := range profileStateOrder {
		sentences := profileStateSentences[stateValue]
		report.States = append(report.States, ProfileStateCount{
			State: stateValue, Title: sentences.title, Count: stateCounts[stateValue],
			Meaning: sentences.meaning, NextStep: sentences.nextStep,
		})
	}
	for _, stateValue := range profilePackageStateOrder {
		sentences := profilePackageStateSentences[stateValue]
		report.PackageStates = append(report.PackageStates, ProfilePackageStateCount{
			State: stateValue, Title: sentences.title, Count: packageCounts[stateValue],
			Meaning: sentences.meaning, NextStep: sentences.nextStep,
		})
	}
	report.Headline = ProfileReportHeadline(report)
	report.NextStep = profileReportNextStep(report)
	return report, nil
}

// profileVersionKey 是一個（套件, 版本）。
type profileVersionKey struct {
	packageID string
	version   string
}

// profileWearers 把「誰現在穿著哪一版」倒過來索引成「哪一版穿在誰身上」。
//
// ⚠ 索引的鍵是 profile_id 與 revision 兩個。少了 revision，一份發佈了新版卻沒有人
// 換上去的 profile 會看起來滿編。
func profileWearers(assignments []store.ProfileAssignment, labels map[string]string,
	retired map[string]bool,
) map[profileRevisionKey][]ProfileMachineRef {
	out := map[profileRevisionKey][]ProfileMachineRef{}
	for _, assignment := range assignments {
		// ⚠ 這裡沒有「名冊上找不到這台就跳過」那一支。帳本擋著那件事：
		// machine_profile_assignments.machine_id 有一條指到 machine_registry 的
		// 外鍵，而退役只是在名冊那一列蓋一個時間、不會把列拿掉，所以指派永遠
		// 活不過名冊。補一支跳過的分支，等於留一條會默默把 Wearing 算少的路，
		// 而它永遠不會有人驗證過。名字退到 machine_id 的理由跟每機安裝狀態一樣：
		// 一列沒有名字的報告，人沒有辦法知道它講的是哪一台。
		label := labels[assignment.MachineID]
		if label == "" {
			label = assignment.MachineID
		}
		key := profileRevisionKey{id: assignment.ProfileID, revision: assignment.ProfileRevision}
		out[key] = append(out[key], ProfileMachineRef{
			MachineID:          assignment.MachineID,
			DisplayName:        label,
			Retired:            retired[assignment.MachineID],
			AssignmentID:       assignment.AssignmentID,
			AssignmentRevision: assignment.AssignmentRevision,
			AssignedAt:         assignment.AssignedAt.UTC(),
			AssignedBy:         assignment.AssignedBy,
		})
	}
	for key := range out {
		rows := out[key]
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].DisplayName != rows[j].DisplayName {
				return rows[i].DisplayName < rows[j].DisplayName
			}
			return rows[i].MachineID < rows[j].MachineID
		})
	}
	return out
}

type profileRevisionKey struct {
	id       string
	revision int64
}

// profileAssignedVersions 取「這個 Hub 指派過哪些（套件, 版本）」。
//
// ⚠ 資源身分用 resource_id 對 package_id，跟每機安裝狀態拿 resource_id 對 cli_tool
// 的 subject 是同一條規則。不准退而求其次去試 resource_kind——猜出來的那一格會讓人
// 以為 Hub 指派過一個它沒有指派過的版本。
func profileAssignedVersions(rows []store.InstallIntentVersion) map[profileVersionKey]store.InstallIntentVersion {
	out := make(map[profileVersionKey]store.InstallIntentVersion, len(rows))
	for _, row := range rows {
		key := profileVersionKey{packageID: row.ResourceID, version: row.Version}
		current, exists := out[key]
		if !exists {
			out[key] = row
			continue
		}
		// 同一個 package_id 在兩種 resource_kind 底下被指派過：兩邊都算數，
		// 次數相加，時間取兩端。
		current.Intents += row.Intents
		if !row.FirstAt.IsZero() && (current.FirstAt.IsZero() || row.FirstAt.Before(current.FirstAt)) {
			current.FirstAt = row.FirstAt
		}
		if row.LastAt.After(current.LastAt) {
			current.LastAt = row.LastAt
		}
		out[key] = current
	}
	return out
}

// profileSeenCount 是一個（套件, 版本）被看到的樣子：幾台回報它，以及那幾台裡有幾台
// 那個版號量在一個沒有人在跑的檔案上。
type profileSeenCount struct {
	on            int
	misattributed int
}

// profileSeenVersions 數「名冊上有幾台回報它身上就是這一版」，以及那幾台裡有幾台那個
// 版號量的不是正在跑的那一份。
//
// ⚠ 退役的機器不算。它已經離開分母，而這份報告問的是「我的機隊上」。
//
// ⚠⚠ 量錯檔案的那幾台**照樣算進 on**。它們身上真的有那個版號的檔案——「看到過」這件
// 事成立，站不住的是「所以它正在跑那一版」。把它們從 on 裡拿掉，一份其實裝上去了的
// profile 會變成「沒有一台回報它」，然後有人會再指派一次。
func profileSeenVersions(tools []store.FleetToolRow, retired map[string]bool,
) map[profileVersionKey]profileSeenCount {
	out := map[profileVersionKey]profileSeenCount{}
	for _, row := range tools {
		if retired[row.MachineID] {
			continue
		}
		if !row.CLITool.Present {
			continue
		}
		version := row.CLITool.VersionReported
		if version == "" {
			// 檔案上讀到的算數，理由跟每機安裝狀態一樣。
			version = row.CLITool.VersionPackageJSON
		}
		if version == "" {
			continue
		}
		key := profileVersionKey{packageID: row.Name, version: version}
		count := out[key]
		count.on++
		// ⚠ 判準跟軟體清查與每機安裝狀態共用同一份 ToolRuntimeOf。這一頁自己再判一次
		// 的話，同一台機器的同一個工具會在三頁上有三個答案。
		if ToolRuntimeMisattributed(ToolRuntimeOf(row.CLITool).State) {
			count.misattributed++
		}
		out[key] = count
	}
	return out
}

// profileNewestRevisions 取每一個 profile_id 現在發佈到第幾版。
func profileNewestRevisions(profiles []store.MachineProfileRecord) map[string]int64 {
	out := make(map[string]int64, len(profiles))
	for _, record := range profiles {
		if current, ok := out[record.Profile.ID]; ok && current >= record.Profile.Revision {
			continue
		}
		out[record.Profile.ID] = record.Profile.Revision
	}
	return out
}

// profileRowFor 把一份已發佈的 profile revision 算成畫面上那一列。
func profileRowFor(record store.MachineProfileRecord,
	wearers map[profileRevisionKey][]ProfileMachineRef,
	assigned map[profileVersionKey]store.InstallIntentVersion,
	seen map[profileVersionKey]profileSeenCount,
	newest map[string]int64,
) ProfileRow {
	row := ProfileRow{
		ProfileID:   record.Profile.ID,
		Revision:    record.Profile.Revision,
		Digest:      record.Digest,
		PublishedAt: record.PublishedAt.UTC(),
		PublishedBy: record.PublishedBy,
		Machines:    []ProfileMachineRef{},
		Packages:    []ProfilePackage{},
	}
	for _, machine := range wearers[profileRevisionKey{id: record.Profile.ID, revision: record.Profile.Revision}] {
		if machine.Retired {
			row.RetiredOn++
		} else {
			row.AssignedOn++
		}
		row.Machines = append(row.Machines, machine)
	}
	for _, ref := range record.Profile.Packages {
		row.Packages = append(row.Packages, profilePackageFor(ref.PackageID, ref.Version, assigned, seen))
	}

	switch {
	case row.AssignedOn > 0:
		row.State = ProfileInUse
	case row.RetiredOn > 0:
		row.State = ProfileRetiredOnly
	case newest[record.Profile.ID] > record.Profile.Revision:
		row.State = ProfileReplaced
	default:
		row.State = ProfileUnassigned
	}
	sentences := profileStateSentences[row.State]
	row.Title, row.Meaning, row.NextStep = sentences.title, sentences.meaning, sentences.nextStep
	row.Headline = profileRowHeadline(row)
	return row
}

// profilePackageFor 算一個（套件, 版本）在這個 Hub 身上的樣子。
func profilePackageFor(packageID, version string,
	assigned map[profileVersionKey]store.InstallIntentVersion,
	seen map[profileVersionKey]profileSeenCount,
) ProfilePackage {
	key := profileVersionKey{packageID: packageID, version: version}
	pkg := ProfilePackage{PackageID: packageID, Version: version}
	intent, everAssigned := assigned[key]
	if everAssigned {
		pkg.Intents = intent.Intents
		if !intent.LastAt.IsZero() {
			lastAt := intent.LastAt.UTC()
			pkg.LastAssignedAt = &lastAt
		}
	}
	pkg.SeenOn, pkg.SeenMisattributedOn = seen[key].on, seen[key].misattributed
	pkg.State = profilePackageStateFor(everAssigned, pkg.SeenOn > 0)
	sentences := profilePackageStateSentences[pkg.State]
	pkg.Title, pkg.Meaning = sentences.title, sentences.meaning
	pkg.NextStep = ProfilePackageNextStep(pkg.State, pkg.SeenMisattributedOn)
	return pkg
}

// profileRowHeadline 是一列自己的一句話。
func profileRowHeadline(row ProfileRow) string {
	headline := fmt.Sprintf("%s rev %d：", row.ProfileID, row.Revision)
	switch {
	case row.AssignedOn > 0 && row.RetiredOn > 0:
		headline += fmt.Sprintf("機隊上 %d 台穿著它，另有 %d 台已退役的還是它",
			row.AssignedOn, row.RetiredOn)
	case row.AssignedOn > 0:
		headline += fmt.Sprintf("機隊上 %d 台穿著它", row.AssignedOn)
	case row.RetiredOn > 0:
		headline += fmt.Sprintf("機隊上沒有機器穿著它，%d 台已退役的還是它", row.RetiredOn)
	default:
		headline += "沒有機器穿著它"
	}
	if len(row.Packages) == 0 {
		return headline + "；它一個套件都沒點名。"
	}
	unknown, misattributed := 0, 0
	for _, pkg := range row.Packages {
		if pkg.State == ProfilePackageNeither {
			unknown++
		}
		if pkg.SeenMisattributedOn > 0 {
			misattributed++
		}
	}
	headline += fmt.Sprintf("；它點名 %d 個套件版本", len(row.Packages))
	if unknown > 0 {
		headline += fmt.Sprintf("，其中 %d 個這個 Hub 沒有指派過也沒有看到過", unknown)
	}
	// ⚠⚠ 這一句不能省。「看得到」是這一列唯一講得出「它真的上去了」的證據，而它可以
	// 整句講的是一批沒有人在跑的檔案——少了這一句，一列看起來收工的 profile 其實只是
	// 在機器的硬碟上放著。
	if misattributed > 0 {
		headline += fmt.Sprintf("；另有 %d 個看得到的版號量的是沒在跑的那一份", misattributed)
	}
	return headline + "。"
}

// ProfileReportHeadline 是整份報告的一句話。
func ProfileReportHeadline(report ProfileReport) string {
	if report.Published == 0 {
		return "還沒有發佈任何 profile。"
	}
	headline := fmt.Sprintf("發佈了 %d 版 profile，分母 %d 台：%d 台身上有 profile",
		report.Published, report.Machines, report.Wearing)
	if report.Bare > 0 {
		headline += fmt.Sprintf("，%d 台身上沒有", report.Bare)
	}
	if report.Unassigned > 0 {
		headline += fmt.Sprintf("；%d 份是最新的一版卻一台都沒指派", report.Unassigned)
	}
	// ⚠⚠ 排在最後但不准省。它講的是這一頁的證據本身有多硬：上面那幾個數字裡的「看得
	// 到」有幾格量的是一份沒有人在跑的安裝。
	if report.SeenMisattributed > 0 {
		headline += fmt.Sprintf("；另有 %d 格看得到的版號量的是沒在跑的那一份",
			report.SeenMisattributed)
	}
	return headline + "。"
}

// profileReportNextStep 只在有事可做的時候講話。
func profileReportNextStep(report ProfileReport) string {
	if report.Published == 0 {
		return "要讓機器裝東西，先發佈一份 profile。"
	}
	for _, count := range report.PackageStates {
		if count.State == ProfilePackageNeither && count.Count > 0 {
			return "先看那幾個這個 Hub 沒有指派過也沒有看到過的套件版本。"
		}
	}
	// ⚠⚠ 排在「一台都沒指派」前面。那幾格的「看得到」量的是沒在跑的那一份，所以一份
	// 看起來已經上去了的 profile 可能只是在硬碟上放著——先確定看到的是哪一份檔案，再
	// 談哪一份還沒指派。
	if report.SeenMisattributed > 0 {
		return "先看那幾格看得到的版號量的是沒在跑的那一份，再談哪一份還沒指派。"
	}
	if report.Unassigned > 0 {
		return "先看那幾份發佈了卻一台都沒指派的 profile。"
	}
	return ""
}

// ProfileReportExportPath is where the browser downloads this report.
func ProfileReportExportPath() string { return "/reports/profile.csv" }

// ProfileReportCSV 匯出的就是畫面上那些列，一列不多一列不少。
//
// ⚠⚠ 一列是「一份已發佈的 revision × 它點名的一個套件版本」，不是「一份 revision ×
// 穿著它的一台機器」。後者會讓這份報告存在的理由整個消失：一份一台都沒指派的
// profile 在那個形狀裡一列都沒有，而 0 列跟「這份 profile 不存在」在試算表裡長得
// 一模一樣。什麼都沒點名的 profile 同理，照樣有一列，套件那幾欄留白。
//
// ⚠ 穿著它的機器擠在同一格裡。這是這份匯出唯一一個壓扁的欄位，理由是主要問題
// （「哪幾份 profile 點名了一個誰都沒有的版本」）落在套件那個粒度上；把機器攤成
// 自己的列會讓那個問題變成要先去重才問得出來。要逐台看的人讀的是 JSON 或畫面。
func ProfileReportCSV(report ProfileReport) ReportCSVDocument {
	document := ReportCSVDocument{
		Filename: "ai-intune-profile.csv",
		Caveat:   ProfileReportCaveat,
		Columns: []ReportCSVColumn{
			{Key: "profile", Header: "profile"},
			{Key: "revision", Header: "revision"},
			{Key: "state", Header: "這一版現在是什麼"},
			{Key: "assigned_on", Header: "機隊上穿著它的台數"},
			{Key: "retired_on", Header: "已退役還穿著它的台數"},
			{Key: "machines", Header: "穿著它的機器"},
			{Key: "published_at", Header: "發佈的時刻"},
			{Key: "published_by", Header: "發佈的人"},
			{Key: "digest", Header: "digest"},
			{Key: "profile_next_step", Header: "這一版的下一步"},
			{Key: "package_id", Header: "套件"},
			{Key: "version", Header: "版號"},
			{Key: "package_state", Header: "這一版在這個 Hub 身上是什麼"},
			{Key: "intents", Header: "指派過幾次"},
			{Key: "last_assigned_at", Header: "最後一次指派的時刻"},
			{Key: "seen_on", Header: "機隊上看到幾台"},
			{Key: "seen_misattributed_on", Header: "其中版號不是跑的那一份的台數"},
			{Key: "package_next_step", Header: "這個套件版本的下一步"},
		},
	}
	for _, row := range report.Profiles {
		head := []string{
			row.ProfileID,
			strconv.FormatInt(row.Revision, 10),
			row.Title,
			strconv.Itoa(row.AssignedOn),
			strconv.Itoa(row.RetiredOn),
			profileCSVMachines(row.Machines),
			ReportCSVTime(row.PublishedAt),
			row.PublishedBy,
			row.Digest,
			row.NextStep,
		}
		if len(row.Packages) == 0 {
			document.Rows = append(document.Rows, append(append([]string{}, head...),
				"", "", "", "", "", "", "", ""))
			continue
		}
		for _, pkg := range row.Packages {
			document.Rows = append(document.Rows, append(append([]string{}, head...),
				pkg.PackageID,
				pkg.Version,
				pkg.Title,
				strconv.Itoa(pkg.Intents),
				ReportCSVOptionalTime(pkg.LastAssignedAt),
				strconv.Itoa(pkg.SeenOn),
				strconv.Itoa(pkg.SeenMisattributedOn),
				pkg.NextStep,
			))
		}
	}
	return document
}

// profileCSVMachines 把穿著這一版的機器排成一格。已退役的那幾台標出來——少了那個
// 標記，一份「只有已退役的機器身上還是這一版」的 profile 在試算表裡看起來還在用。
func profileCSVMachines(machines []ProfileMachineRef) string {
	if len(machines) == 0 {
		return ""
	}
	names := make([]string, 0, len(machines))
	for _, machine := range machines {
		name := machine.DisplayName
		if machine.Retired {
			name += "（已退役）"
		}
		names = append(names, name)
	}
	return strings.Join(names, "、")
}
