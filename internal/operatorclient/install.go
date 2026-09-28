package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// InstallReport reads how what the Hub assigned compares with what it sees on
// each machine.
//
// ⚠ 用戶端自己把每一格重數一次。這份報告的每一個數字都是逐格數出來的，所以一份
// 自己對不起來的回應不是拿來顯示的東西，是拿來拒收的：把「沒有被指派過」算進
// 「指派的跟看到的一樣」，畫面上就會出現一個不存在的「都對得起來」。
func (c *Client) InstallReport(ctx context.Context) (operator.InstallReport, error) {
	var out operator.InstallReport
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/install-report", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: install report returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "install report", &out); err != nil {
		return out, err
	}
	if err := validateInstallReport(out); err != nil {
		return out, err
	}
	return out, nil
}

func validateInstallReport(report operator.InstallReport) error {
	if report.SchemaVersion != operator.InstallReportSchemaVersion ||
		report.EvaluatedAt.IsZero() || report.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: install report identity 不一致")
	}
	if report.Machines < 0 || report.Assigned < 0 || report.Differing < 0 ||
		report.Misattributed < 0 {
		return errors.New("operator client: install report 有負數")
	}
	if report.Assigned > report.Machines {
		return fmt.Errorf("operator client: install report 說 %d 台被指派過，分母只有 %d 台",
			report.Assigned, report.Machines)
	}
	// ⚠⚠ 這一句擋的是「Hub 不知道機器上的東西從哪來」這件事被悄悄拿掉。少了那句話，
	// 一列「指派的比看到的舊」會被讀成「有人亂動這台機器」——而正式環境上那正是
	// 預期的樣子。
	if report.Caveat != operator.InstallReportCaveat {
		return errors.New("operator client: install report 沒有帶著它講得出口的極限")
	}
	states := operator.InstallStates()
	if len(report.States) != len(states) {
		return fmt.Errorf("operator client: install report 有 %d 種狀態，這個版本認得 %d 種",
			len(report.States), len(states))
	}
	counted := map[operator.InstallState]int{}
	for index, stateCount := range report.States {
		if stateCount.State != states[index] {
			return fmt.Errorf("operator client: install report 第 %d 種狀態是 %q，這個版本這裡是 %q",
				index, stateCount.State, states[index])
		}
		if stateCount.Count < 0 {
			return fmt.Errorf("operator client: install state %q 的格數是負的", stateCount.State)
		}
		if err := validateInstallStateSentences(stateCount.State, stateCount.Title,
			stateCount.Meaning, stateCount.NextStep); err != nil {
			return err
		}
		counted[stateCount.State] = stateCount.Count
	}
	countedRuntime, err := countedToolRuntimes("install report", report.RuntimeStates)
	if err != nil {
		return err
	}

	seen := map[operator.InstallState]int{}
	seenRuntime := map[operator.ToolRuntime]int{}
	names := map[string]bool{}
	differing, misattributed := 0, 0
	assigned := map[string]bool{}
	for _, resource := range report.Resources {
		if err := validateInstallResource(resource, report.Machines); err != nil {
			return err
		}
		if names[resource.Name] {
			return fmt.Errorf("operator client: install report 有兩列 %q", resource.Name)
		}
		names[resource.Name] = true
		if resource.DifferingOn > 0 {
			differing++
		}
		for _, row := range resource.Rows {
			seen[row.State]++
			// 一台只要有一個資源被指派過，它就被指派過。
			if operator.InstallStateAssigned(row.State) {
				assigned[row.MachineID] = true
			}
			if row.Runtime == nil {
				continue
			}
			seenRuntime[row.Runtime.State]++
			if operator.ToolRuntimeMisattributed(row.Runtime.State) {
				misattributed++
			}
		}
	}
	// ⚠⚠ 這一個數字要在「幾個資源對不上」前面擋下來。它比對不上那個數字更早決定畫面
	// 上該先做什麼：那幾格看到的版號量在一個沒有人在跑的檔案上，「指派的比看到的舊」
	// 比的是一個沒有人在用的檔案。
	if misattributed != report.Misattributed {
		return fmt.Errorf("operator client: install report 說 %d 格看到的版號量的不是正在跑的那一份，"+
			"逐格數出 %d 格", report.Misattributed, misattributed)
	}
	if differing != report.Differing {
		return fmt.Errorf("operator client: install report 說 %d 個資源對不上，逐列數出 %d 個",
			report.Differing, differing)
	}
	if len(assigned) != report.Assigned {
		return fmt.Errorf("operator client: install report 說 %d 台被指派過，逐格數出 %d 台",
			report.Assigned, len(assigned))
	}
	// ⚠ 狀態摘要要逐格數過，不是只檢查它們加得起來。一份把「沒有被指派過」搬進
	// 「指派的跟看到的一樣」的摘要照樣加得起來，而那兩件事的下一步完全不同。
	for stateValue, want := range counted {
		if seen[stateValue] != want {
			return fmt.Errorf("operator client: %q 的摘要說 %d 格，逐格數出 %d 格",
				stateValue, want, seen[stateValue])
		}
	}
	// ⚠ 同樣逐格數過。把「正在跑的是另一個檔案」搬進「沒有找到在跑它的 process」的
	// 摘要照樣加得起來，而前者要人去收掉一份安裝，後者不要人做任何事。
	for stateValue, want := range countedRuntime {
		if seenRuntime[stateValue] != want {
			return fmt.Errorf("operator client: %q 的摘要說 %d 格，逐格數出 %d 格",
				stateValue, want, seenRuntime[stateValue])
		}
	}
	return nil
}

func validateInstallResource(resource operator.InstallResource, machines int) error {
	for label, value := range map[string]string{
		"install resource name": resource.Name,
		"install resource_kind": resource.ResourceKind,
		"install resource_id":   resource.ResourceID,
	} {
		if err := validateMachineClientText(label, value, 256); err != nil {
			return err
		}
	}
	// ⚠ 名字要是從身分算出來的。一個自己取名字的資源，可以把兩個不同的資源在畫面
	// 上寫成同一列，而那一列上的每一格都會是別人的答案。
	if want := installResourceName(resource.ResourceKind, resource.ResourceID); resource.Name != want {
		return fmt.Errorf("operator client: 資源 %s:%s 的名字是 %q，照身分算出來是 %q",
			resource.ResourceKind, resource.ResourceID, resource.Name, want)
	}
	if resource.AssignedOn < 0 || resource.UnassignedOn < 0 || resource.MatchingOn < 0 ||
		resource.DifferingOn < 0 || resource.MisattributedOn < 0 {
		return fmt.Errorf("operator client: 資源 %q 有負數", resource.Name)
	}
	// ⚠ 兩個加起來就是分母。少掉一台，畫面上會出現一個比實際小的「幾台被指派過」——
	// 而沒被算到的那一台正是最該被看到的那一台。
	if total := resource.AssignedOn + resource.UnassignedOn; total != machines {
		return fmt.Errorf("operator client: 資源 %q 的 %d+%d 台，分母是 %d 台",
			resource.Name, resource.AssignedOn, resource.UnassignedOn, machines)
	}
	if len(resource.Rows) != machines {
		return fmt.Errorf("operator client: 資源 %q 有 %d 列，分母是 %d 台",
			resource.Name, len(resource.Rows), machines)
	}
	if resource.Headline == "" {
		return fmt.Errorf("operator client: 資源 %q 沒有那一行字", resource.Name)
	}

	assignedOn, unassignedOn, matchingOn, differingOn, misattributedOn := 0, 0, 0, 0, 0
	ids := map[string]bool{}
	for _, row := range resource.Rows {
		if err := validateInstallRow(resource.Name, row); err != nil {
			return err
		}
		if ids[row.MachineID] {
			return fmt.Errorf("operator client: 資源 %q 有兩列 %s", resource.Name, row.MachineID)
		}
		ids[row.MachineID] = true
		// ⚠ 這一格在下面那個 if／continue 之前數。量錯檔案跟指派對不對得上是兩個獨立
		// 的軸：一台「指派的跟看到的一樣」也可以是量在一個沒有人在跑的檔案上，而那一格
		// 看起來最安全。
		if row.Runtime != nil && operator.ToolRuntimeMisattributed(row.Runtime.State) {
			misattributedOn++
		}
		if !operator.InstallStateAssigned(row.State) {
			unassignedOn++
			continue
		}
		assignedOn++
		if row.State == operator.InstallMatches {
			matchingOn++
		}
		if operator.InstallStateDiffers(row.State) {
			differingOn++
		}
	}
	if assignedOn != resource.AssignedOn || unassignedOn != resource.UnassignedOn {
		return fmt.Errorf("operator client: 資源 %q 的摘要說指派 %d／沒指派 %d，逐列數出 %d／%d",
			resource.Name, resource.AssignedOn, resource.UnassignedOn, assignedOn, unassignedOn)
	}
	if matchingOn != resource.MatchingOn || differingOn != resource.DifferingOn {
		return fmt.Errorf("operator client: 資源 %q 的摘要說一樣 %d／不一樣 %d，逐列數出 %d／%d",
			resource.Name, resource.MatchingOn, resource.DifferingOn, matchingOn, differingOn)
	}
	if misattributedOn != resource.MisattributedOn {
		return fmt.Errorf("operator client: 資源 %q 的摘要說 %d 台看到的版號量的不是正在跑的那一份，"+
			"逐列數出 %d 台", resource.Name, resource.MisattributedOn, misattributedOn)
	}
	return nil
}

// installResourceName 是名字照身分算出來的那條規則。kind 跟 id 一樣的時候只講一次。
func installResourceName(kind, id string) string {
	if kind == id {
		return id
	}
	return kind + "/" + id
}

func validateInstallRow(resourceName string, row operator.InstallRow) error {
	if err := validateMachineClientText("install machine_id", row.MachineID, 256); err != nil {
		return err
	}
	if err := validateMachineClientText("install display_name", row.DisplayName, 256); err != nil {
		return err
	}
	if err := validateInstallStateSentences(row.State, row.Title, row.Meaning, row.NextStep); err != nil {
		return err
	}
	if err := validateInstallRowAssignment(resourceName, row); err != nil {
		return err
	}
	if err := validateInstallRowRuntime(resourceName, row); err != nil {
		return err
	}
	// ⚠⚠ 有「看到的版號」就表示 Hub 真的看到那個東西裝著。一格說「這台上沒有」或
	// 「沒回報過」卻帶著看到的版號，講的是兩件互相矛盾的事，而畫面只會印出其中一件。
	if row.Observed != "" && !operator.InstallStateHasObservedVersion(row.State) {
		return fmt.Errorf("operator client: 資源 %q 在 %s 上是 %q，卻帶著看到的版號 %s",
			resourceName, row.MachineID, row.State, row.Observed)
	}
	// FromDisk 講的是「這個版號是從檔案讀的」——沒有版號就沒有這件事好講。
	if row.FromDisk && row.Observed == "" {
		return fmt.Errorf("operator client: 資源 %q 在 %s 上沒有看到版號，卻說版號是從檔案讀的",
			resourceName, row.MachineID)
	}
	// ⚠ 沒有觀測的那三種不會有時刻——那正是它們的意思。
	switch row.State {
	case operator.InstallUnassigned, operator.InstallUnreported, operator.InstallUnobserved:
		if row.MeasuredAt != nil || row.ObservedAt != nil {
			return fmt.Errorf("operator client: 資源 %q 在 %s 上是 %q，卻給了觀測時刻",
				resourceName, row.MachineID, row.State)
		}
	}
	for name, value := range map[string]*time.Time{
		"measured_at": row.MeasuredAt, "observed_at": row.ObservedAt, "assigned_at": row.AssignedAt,
	} {
		if value != nil && (value.IsZero() || value.Location() != time.UTC) {
			return fmt.Errorf("operator client: 資源 %q 在 %s 上的 %s 不是可用的 UTC 時刻",
				resourceName, row.MachineID, name)
		}
	}
	return nil
}

// validateInstallRowRuntime 釘住這一格的第二軸：看到的那個版號量的是哪一個檔案。
//
// ⚠⚠ 有這一軸就等於「這台回報說它上面有這個東西」。一格說「這台上沒有」卻回答「正在
// 跑的是另一個檔案」，講的是兩件互相矛盾的事，而畫面只會印出其中一件；反過來，一格說
// 這台上有、卻不回答這一軸，那個版號就會替一份沒有人在跑的安裝發言而沒有人知道——然後
// 有人照「指派的比看到的舊」去回滾一台其實沒事的機器。
func validateInstallRowRuntime(resourceName string, row operator.InstallRow) error {
	if operator.InstallStateReportedPresent(row.State) != (row.Runtime != nil) {
		return fmt.Errorf("operator client: 資源 %q 在 %s 上是 %q，「版號講的是哪一份」卻 %s",
			resourceName, row.MachineID, row.State, toolRuntimePresence(row.Runtime != nil))
	}
	if row.Runtime == nil {
		return nil
	}
	return validateToolRuntimeFinding(
		fmt.Sprintf("資源 %q 在 %s 上的「版號講的是哪一份」", resourceName, row.MachineID),
		*row.Runtime)
}

// validateInstallRowAssignment 釘住「這一格有沒有被指派過」跟它身上那幾個指派欄位
// 是同一件事。
//
// ⚠⚠ 一格說「沒有被指派過」卻帶著 revision，畫面上會印出一個查得到、卻不屬於這台
// 機器的帳本號碼；反過來，一格說被指派過卻沒有 revision，那句「誰在什麼時候指派的」
// 就無從查證——而這一頁唯一的權威就是帳本。
func validateInstallRowAssignment(resourceName string, row operator.InstallRow) error {
	if !operator.InstallStateAssigned(row.State) {
		if row.Assigned != "" || row.Scope != "" || row.ScopeID != "" || row.ScopeLabel != "" ||
			row.Revision != 0 || row.AssignedAt != nil || row.AssignedBy != "" {
			return fmt.Errorf("operator client: 資源 %q 在 %s 上說沒有被指派過，卻帶著指派欄位",
				resourceName, row.MachineID)
		}
		return nil
	}
	if row.Scope != "machine" && row.Scope != "channel" {
		return fmt.Errorf("operator client: 資源 %q 在 %s 上的指派範圍是 %q，這個版本不認得",
			resourceName, row.MachineID, row.Scope)
	}
	if err := validateMachineClientText("install scope_id", row.ScopeID, 256); err != nil {
		return err
	}
	if row.ScopeLabel == "" {
		return fmt.Errorf("operator client: 資源 %q 在 %s 上沒有講這一筆指派是給誰的",
			resourceName, row.MachineID)
	}
	if row.Revision <= 0 {
		return fmt.Errorf("operator client: 資源 %q 在 %s 上被指派過，卻沒有 revision",
			resourceName, row.MachineID)
	}
	if row.AssignedAt == nil {
		return fmt.Errorf("operator client: 資源 %q 在 %s 上被指派過，卻沒有指派時刻",
			resourceName, row.MachineID)
	}
	// ⚠ 只有「指派的那一筆沒講版號」那一種才可以沒有版號。別的狀態少了版號，畫面上
	// 那一格會變成一個空白的比較。
	if row.Assigned == "" && row.State != operator.InstallAssignedNoVersion {
		return fmt.Errorf("operator client: 資源 %q 在 %s 上是 %q，卻沒有指派的版號",
			resourceName, row.MachineID, row.State)
	}
	if row.Assigned != "" && row.State == operator.InstallAssignedNoVersion {
		return fmt.Errorf("operator client: 資源 %q 在 %s 上說指派沒講版號，卻帶著 %s",
			resourceName, row.MachineID, row.Assigned)
	}
	return nil
}

// validateInstallStateSentences 釘住「這一格是什麼」與「它講的那三句話」是同一件事。
//
// 對面的 Hub 對某一個狀態有第二種說法時，畫面上會出現一個看起來合理、其實指錯下一
// 步的句子——而操作員就是照那一句決定要去重新指派，還是去看那台的 agent。
func validateInstallStateSentences(stateValue operator.InstallState,
	title, meaning, nextStep string,
) error {
	// ⚠ 這一句擋的是「有一格落在這個版本不認得的狀態上」。逐格的比對只對得到這個
	// 版本認得的那幾種，一格落在認不得的狀態上就會從那個比對裡整個消失。
	if operator.InstallStateTitle(stateValue) == "" {
		return fmt.Errorf("operator client: install state %q 這個版本不認得", stateValue)
	}
	if title != operator.InstallStateTitle(stateValue) ||
		meaning != operator.InstallStateMeaning(stateValue) ||
		nextStep != operator.InstallStateNextStep(stateValue) {
		return fmt.Errorf("operator client: install state %q 的句子跟這個版本不一樣", stateValue)
	}
	return nil
}
