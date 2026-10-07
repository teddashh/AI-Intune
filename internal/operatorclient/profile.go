package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// ProfileReport reads what this Hub published, what it assigned and what it saw.
//
// ⚠ 用戶端自己把每一格重數一次，而且連「這一列該是哪一種狀態」都自己算：這份報告
// 的四種狀態只差一句話，而那一句話決定操作員要不要動手。一份把「發佈了一台都沒指派」
// 寫成「這個 profile 已經發佈到更新的版本」的回應，在畫面上看起來完全正常——那一列
// 只會被當成歷史版本捲過去，而它正是這份報告存在的理由。
func (c *Client) ProfileReport(ctx context.Context) (operator.ProfileReport, error) {
	var out operator.ProfileReport
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/profile-report", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: profile report returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "profile report", &out); err != nil {
		return out, err
	}
	if err := validateProfileReport(out); err != nil {
		return out, err
	}
	return out, nil
}

func validateProfileReport(report operator.ProfileReport) error {
	if report.SchemaVersion != operator.ProfileReportSchemaVersion ||
		report.EvaluatedAt.IsZero() || report.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: profile report identity is inconsistent")
	}
	if report.Machines < 0 || report.Wearing < 0 || report.Bare < 0 ||
		report.Published < 0 || report.Unassigned < 0 {
		return errors.New("operator client: profile report has negative values")
	}
	// ⚠ 一台機器身上有 profile，或者沒有——兩個數加起來就是分母。少掉一台，畫面上
	// 會出現一個比實際小的「幾台身上有 profile」，而沒被算到的那一台不會有人去找。
	if report.Wearing+report.Bare != report.Machines {
		return fmt.Errorf("operator client: profile report reports %d machines wearing profile / %d bare, total machines is %d",
			report.Wearing, report.Bare, report.Machines)
	}
	if report.Published != len(report.Profiles) {
		return fmt.Errorf("operator client: profile report reports %d published revisions, counted %d rows",
			report.Published, len(report.Profiles))
	}
	// ⚠⚠ 這一句擋的是「Hub 只知道自己發佈過、指派過與看到什麼」這件事被悄悄拿掉。
	// 少了那句話，一格「這個 Hub 沒有指派過這一版，也沒有看到過」會被讀成「這一版
	// 不存在」——而這個 Hub 沒有資格講那句話。
	if report.Caveat != operator.ProfileReportCaveat {
		return errors.New("operator client: profile report lacks known limits")
	}
	newest, err := profileNewestRevisions(report.Profiles)
	if err != nil {
		return err
	}
	counted, err := countProfileRows(report, newest)
	if err != nil {
		return err
	}
	// ⚠ 「幾台身上有 profile」數得出來：每一台在籍機器最多穿著一版，而每一版穿著它
	// 的機器都在那一列上。數不出同一個數字的話，畫面上那個「幾台沒有」是編的。
	if len(counted.wearers) != report.Wearing {
		return fmt.Errorf("operator client: profile report reports %d machines wearing profile, counted %d machines",
			report.Wearing, len(counted.wearers))
	}
	// ⚠⚠ 排在「一台都沒指派」前面，跟畫面與下一步同一個順序。這個數字講的是上面每一
	// 個「看得到」有多硬：少算一格，一份其實只是在硬碟上放著的 profile 會讀成收工。
	if counted.seenMisattributed != report.SeenMisattributed {
		return fmt.Errorf("operator client: profile report reports %d seen misattributed cells, counted %d cells",
			report.SeenMisattributed, counted.seenMisattributed)
	}
	if counted.unassigned != report.Unassigned {
		return fmt.Errorf("operator client: profile report reports %d unassigned published revisions, counted %d rows",
			report.Unassigned, counted.unassigned)
	}
	if err := validateProfileStateCounts(report.States, counted.states); err != nil {
		return err
	}
	return validateProfilePackageStateCounts(report.PackageStates, counted.packages)
}

// profileNewestRevisions 自己從收到的那幾列算出每一個 profile 最新的一版。
//
// ⚠⚠ 不是拿 Hub 講的：「這一版已經被取代了」與「這一版就是最新的而一台都沒指派」
// 差別就在這個數字，而它算得出來——整份報告本來就是每一個已發佈的 revision 一列。
func profileNewestRevisions(rows []operator.ProfileRow) (map[string]int64, error) {
	newest := make(map[string]int64, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Revision <= 0 {
			return nil, fmt.Errorf("operator client: profile %q revision is %d",
				row.ProfileID, row.Revision)
		}
		key := fmt.Sprintf("%s\x00%d", row.ProfileID, row.Revision)
		if seen[key] {
			return nil, fmt.Errorf("operator client: profile report has duplicate row %s rev %d",
				row.ProfileID, row.Revision)
		}
		seen[key] = true
		if row.Revision > newest[row.ProfileID] {
			newest[row.ProfileID] = row.Revision
		}
	}
	return newest, nil
}

// profileTally 是逐列數出來的那一份，拿來跟 Hub 講的那一份對。
type profileTally struct {
	states   map[operator.ProfileState]int
	packages map[operator.ProfilePackageState]int
	// wearers 是在籍的那幾台，去重過：同一台可能穿著的是哪一版由帳本決定，
	// 但它只算一台。
	wearers    map[string]bool
	unassigned int
	// seenMisattributed 數的是格子，不是台數：一格是「一份 revision × 它點名的一個
	// 套件版本」，跟 Hub 那一邊同一個粒度。
	seenMisattributed int
}

func countProfileRows(report operator.ProfileReport, newest map[string]int64) (profileTally, error) {
	counted := profileTally{
		states:   map[operator.ProfileState]int{},
		packages: map[operator.ProfilePackageState]int{},
		wearers:  map[string]bool{},
	}
	for _, row := range report.Profiles {
		if err := validateProfileRow(row, report.Machines, newest[row.ProfileID]); err != nil {
			return profileTally{}, err
		}
		counted.states[row.State]++
		if row.State == operator.ProfileUnassigned {
			counted.unassigned++
		}
		for _, machine := range row.Machines {
			if !machine.Retired {
				counted.wearers[machine.MachineID] = true
			}
		}
		for _, pkg := range row.Packages {
			counted.packages[pkg.State]++
			if pkg.SeenMisattributedOn > 0 {
				counted.seenMisattributed++
			}
		}
	}
	return counted, nil
}

func validateProfileRow(row operator.ProfileRow, machines int, newestRevision int64) error {
	label := fmt.Sprintf("%s rev %d", row.ProfileID, row.Revision)
	for name, value := range map[string]string{
		"profile_id":   row.ProfileID,
		"digest":       row.Digest,
		"published_by": row.PublishedBy,
	} {
		if err := validateMachineClientText("profile "+name, value, 256); err != nil {
			return err
		}
	}
	if row.ProfileID == "" || row.Digest == "" {
		return fmt.Errorf("operator client: profile report row is missing identity: %q rev %d",
			row.ProfileID, row.Revision)
	}
	if row.PublishedAt.IsZero() || row.PublishedAt.Location() != time.UTC {
		return fmt.Errorf("operator client: %s published_at is not a valid UTC timestamp", label)
	}
	if row.Headline == "" {
		return fmt.Errorf("operator client: %s is missing headline", label)
	}
	if row.AssignedOn < 0 || row.RetiredOn < 0 {
		return fmt.Errorf("operator client: %s has negative values", label)
	}
	if row.AssignedOn > machines {
		return fmt.Errorf("operator client: %s reports %d machines wearing profile, total machines is only %d",
			label, row.AssignedOn, machines)
	}
	if err := validateProfileWearers(label, row); err != nil {
		return err
	}
	if err := validateProfilePackages(label, row, machines); err != nil {
		return err
	}
	if err := validateProfileStateSentences(row.State, row.Title, row.Meaning, row.NextStep); err != nil {
		return err
	}
	// ⚠⚠ 這一列該是哪一種狀態算得出來，所以就算出來。四種狀態在畫面上只差一句話，
	// 而「發佈了一台都沒指派」被寫成「已經發佈到更新的版本」的時候，那一列會被當成
	// 歷史版本捲過去。
	if want := expectedProfileState(row, newestRevision); row.State != want {
		return fmt.Errorf("operator client: %s reports %q, but computed state is %q",
			label, row.State, want)
	}
	return nil
}

// expectedProfileState 是四種狀態唯一的判準，照收到的那幾個數字重算一次。
func expectedProfileState(row operator.ProfileRow, newestRevision int64) operator.ProfileState {
	switch {
	case row.AssignedOn > 0:
		return operator.ProfileInUse
	case row.RetiredOn > 0:
		return operator.ProfileRetiredOnly
	case newestRevision > row.Revision:
		return operator.ProfileReplaced
	default:
		return operator.ProfileUnassigned
	}
}

// validateProfileWearers 釘住「穿著它的那幾台」跟那兩個數字是同一件事。
//
// ⚠ 退役的那幾台照樣列出來，但它們不算在「機隊上穿著它的台數」裡：一份只有退役機器
// 還穿著它的 profile，跟一份誰都沒穿的 profile 要做的事不一樣。
func validateProfileWearers(label string, row operator.ProfileRow) error {
	assignedOn, retiredOn := 0, 0
	ids := map[string]bool{}
	for _, machine := range row.Machines {
		for name, value := range map[string]string{
			"machine_id":    machine.MachineID,
			"display_name":  machine.DisplayName,
			"assignment_id": machine.AssignmentID,
			"assigned_by":   machine.AssignedBy,
		} {
			if err := validateMachineClientText("profile wearer "+name, value, 256); err != nil {
				return err
			}
		}
		if machine.MachineID == "" || machine.DisplayName == "" || machine.AssignmentID == "" {
			return fmt.Errorf("operator client: %s has a wearing machine without identity", label)
		}
		if ids[machine.MachineID] {
			return fmt.Errorf("operator client: %s has duplicate row %s", label, machine.MachineID)
		}
		ids[machine.MachineID] = true
		// 指派是帳本上的一列，所以它一定有 revision 與時刻——這一頁唯一的權威就是帳本。
		if machine.AssignmentRevision <= 0 {
			return fmt.Errorf("operator client: %s on %s assignment lacks revision",
				label, machine.MachineID)
		}
		if machine.AssignedAt.IsZero() || machine.AssignedAt.Location() != time.UTC {
			return fmt.Errorf("operator client: %s on %s assignment timestamp is not a valid UTC timestamp",
				label, machine.MachineID)
		}
		if machine.Retired {
			retiredOn++
			continue
		}
		assignedOn++
	}
	if assignedOn != row.AssignedOn || retiredOn != row.RetiredOn {
		return fmt.Errorf("operator client: %s summary reports %d fleet machines / %d retired machines, counted %d / %d",
			label, row.AssignedOn, row.RetiredOn, assignedOn, retiredOn)
	}
	return nil
}

// validateProfilePackages 釘住每一格那兩個軸跟它的狀態是同一件事。
//
// ⚠⚠ 「指派過沒有」與「看到過沒有」是兩個獨立的軸，而每一種狀態就是那兩個軸的一種
// 組合。一格說「這個 Hub 沒有指派過這一版」卻帶著指派次數，講的是兩件互相矛盾的事，
// 而畫面只會印出其中一件——然後操作員會去找一個不存在的安裝路徑。
func validateProfilePackages(label string, row operator.ProfileRow, machines int) error {
	keys := map[string]bool{}
	for _, pkg := range row.Packages {
		for name, value := range map[string]string{
			"package_id": pkg.PackageID,
			"version":    pkg.Version,
		} {
			if err := validateMachineClientText("profile package "+name, value, 256); err != nil {
				return err
			}
		}
		if pkg.PackageID == "" || pkg.Version == "" {
			return fmt.Errorf("operator client: %s specifies a package version without identity", label)
		}
		key := pkg.PackageID + "\x00" + pkg.Version
		if keys[key] {
			return fmt.Errorf("operator client: %s has duplicate row %s %s", label, pkg.PackageID, pkg.Version)
		}
		keys[key] = true
		if err := validateProfilePackageStateSentences(pkg.State, pkg.Title, pkg.Meaning,
			pkg.NextStep, operator.ProfilePackageNextStep(pkg.State, pkg.SeenMisattributedOn)); err != nil {
			return err
		}
		if pkg.Intents < 0 || pkg.SeenOn < 0 || pkg.SeenMisattributedOn < 0 {
			return fmt.Errorf("operator client: %s %s %s has negative values", label, pkg.PackageID, pkg.Version)
		}
		if pkg.SeenOn > machines {
			return fmt.Errorf("operator client: %s %s %s reports %d machines seen, total machines is only %d",
				label, pkg.PackageID, pkg.Version, pkg.SeenOn, machines)
		}
		// ⚠⚠ 量錯檔案的台數是「看得到的那幾台」的一個子集，而那兩個數字在線路上彼此
		// 獨立——狀態那一軸只問 SeenOn 是不是大於 0，所以一格說「0 台看得到、其中 3 台
		// 量錯檔案」照樣過得了下面那一關。那一格在畫面上會是一句自相矛盾的話。
		if pkg.SeenMisattributedOn > pkg.SeenOn {
			return fmt.Errorf(
				"operator client: %s %s %s reports %d machines seen, with %d machines measuring non-running file",
				label, pkg.PackageID, pkg.Version, pkg.SeenOn, pkg.SeenMisattributedOn)
		}
		everAssigned, everSeen, _ := operator.ProfilePackageStateAxes(pkg.State)
		if everAssigned != (pkg.Intents > 0) || everSeen != (pkg.SeenOn > 0) {
			return fmt.Errorf(
				"operator client: %s %s %s is %q, but reports %d intents and %d machines seen",
				label, pkg.PackageID, pkg.Version, pkg.State, pkg.Intents, pkg.SeenOn)
		}
		// 指派過就有最後一次；沒有指派過就沒有這件事好講。
		if everAssigned == (pkg.LastAssignedAt == nil) {
			return fmt.Errorf("operator client: %s %s %s is %q, last assigned timestamp is inconsistent",
				label, pkg.PackageID, pkg.Version, pkg.State)
		}
		if pkg.LastAssignedAt != nil &&
			(pkg.LastAssignedAt.IsZero() || pkg.LastAssignedAt.Location() != time.UTC) {
			return fmt.Errorf("operator client: %s %s %s last assigned timestamp is not a valid UTC timestamp",
				label, pkg.PackageID, pkg.Version)
		}
	}
	return nil
}

// validateProfileStateCounts / validateProfilePackageStateCounts 逐格數過那兩份摘要。
//
// ⚠ 不是只檢查它們加得起來。一份把「發佈了一台都沒指派」搬進「已經發佈到更新的版本」
// 的摘要照樣加得起來，而那兩件事只有一件要人動手。
func validateProfileStateCounts(summary []operator.ProfileStateCount,
	counted map[operator.ProfileState]int,
) error {
	states := operator.ProfileStates()
	if len(summary) != len(states) {
		return fmt.Errorf("operator client: profile report has %d states, this version recognizes %d states",
			len(summary), len(states))
	}
	for index, stateCount := range summary {
		if stateCount.State != states[index] {
			return fmt.Errorf("operator client: profile report state %d is %q, this version expects %q",
				index, stateCount.State, states[index])
		}
		if err := validateProfileStateSentences(stateCount.State, stateCount.Title,
			stateCount.Meaning, stateCount.NextStep); err != nil {
			return err
		}
		if stateCount.Count != counted[stateCount.State] {
			return fmt.Errorf("operator client: %q summary reports %d revisions, counted %d revisions",
				stateCount.State, stateCount.Count, counted[stateCount.State])
		}
	}
	return nil
}

func validateProfilePackageStateCounts(summary []operator.ProfilePackageStateCount,
	counted map[operator.ProfilePackageState]int,
) error {
	states := operator.ProfilePackageStates()
	if len(summary) != len(states) {
		return fmt.Errorf("operator client: profile report has %d package states, this version recognizes %d states",
			len(summary), len(states))
	}
	for index, stateCount := range summary {
		if stateCount.State != states[index] {
			return fmt.Errorf("operator client: profile report package state %d is %q, this version expects %q",
				index, stateCount.State, states[index])
		}
		if err := validateProfilePackageStateSentences(stateCount.State, stateCount.Title,
			stateCount.Meaning, stateCount.NextStep,
			operator.ProfilePackageStateNextStep(stateCount.State)); err != nil {
			return err
		}
		if stateCount.Count != counted[stateCount.State] {
			return fmt.Errorf("operator client: %q summary reports %d cells, counted %d cells",
				stateCount.State, stateCount.Count, counted[stateCount.State])
		}
	}
	return nil
}

// validateProfileStateSentences / validateProfilePackageStateSentences 釘住「這一列
// 是什麼」與它講的那三句話是同一件事。
//
// 對面的 Hub 對某一個狀態有第二種說法時，畫面上會出現一個看起來合理、其實指錯下一步
// 的句子——而操作員就是照那一句決定要去指派，還是去確認那一版存不存在。
//
// 一格的下一步還看它自己量錯檔案的台數，所以由呼叫的那一邊把該是哪一句傳進來。
func validateProfileStateSentences(stateValue operator.ProfileState,
	title, meaning, nextStep string,
) error {
	if operator.ProfileStateTitle(stateValue) == "" {
		return fmt.Errorf("operator client: profile state %q is unrecognized by this version", stateValue)
	}
	if title != operator.ProfileStateTitle(stateValue) ||
		meaning != operator.ProfileStateMeaning(stateValue) ||
		nextStep != operator.ProfileStateNextStep(stateValue) {
		return fmt.Errorf("operator client: profile state %q sentences do not match this version", stateValue)
	}
	return nil
}

func validateProfilePackageStateSentences(stateValue operator.ProfilePackageState,
	title, meaning, nextStep, wantNextStep string,
) error {
	// ⚠ 一格落在這個版本不認得的狀態上，就會從逐格的比對裡整個消失。
	if _, _, known := operator.ProfilePackageStateAxes(stateValue); !known {
		return fmt.Errorf("operator client: profile package state %q is unrecognized by this version", stateValue)
	}
	if title != operator.ProfilePackageStateTitle(stateValue) ||
		meaning != operator.ProfilePackageStateMeaning(stateValue) ||
		nextStep != wantNextStep {
		return fmt.Errorf("operator client: profile package state %q sentences do not match this version", stateValue)
	}
	return nil
}
