package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// SoftwareReport reads what is installed across the fleet, at which versions,
// and which machines do not have it.
//
// ⚠ 用戶端自己把每一格重數一次。這份報告的每一個數字都是逐格數出來的，所以一份
// 自己對不起來的回應不是拿來顯示的東西，是拿來拒收的：把「沒回報過」算進「這台
// 上沒有」，畫面上就會出現一個不存在的「都裝好了」。
func (c *Client) SoftwareReport(ctx context.Context) (operator.SoftwareReport, error) {
	var out operator.SoftwareReport
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/software-report", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: software report returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "software report", &out); err != nil {
		return out, err
	}
	if err := validateSoftwareReport(out); err != nil {
		return out, err
	}
	return out, nil
}

func validateSoftwareReport(report operator.SoftwareReport) error {
	if report.SchemaVersion != operator.SoftwareReportSchemaVersion ||
		report.EvaluatedAt.IsZero() || report.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: software report identity 不一致")
	}
	if report.Machines < 0 || report.Reporting < 0 || report.Drifted < 0 ||
		report.Misattributed < 0 {
		return errors.New("operator client: software report 有負數")
	}
	if report.Reporting > report.Machines {
		return fmt.Errorf("operator client: software report 說 %d 台回報過，分母只有 %d 台",
			report.Reporting, report.Machines)
	}
	// ⚠⚠ 這一句擋的是「沒有上游版本來源」這件事被悄悄拿掉。一張全綠的表少了那句
	// 話會被讀成「都是最新的」，而這份報告從來不知道上游今天發了什麼。
	if report.Caveat != operator.SoftwareReportCaveat {
		return errors.New("operator client: software report 沒有帶著它講得出口的極限")
	}
	states := operator.SoftwareStates()
	if len(report.States) != len(states) {
		return fmt.Errorf("operator client: software report 有 %d 種狀態，這個版本認得 %d 種",
			len(report.States), len(states))
	}
	counted := map[operator.SoftwareState]int{}
	for index, stateCount := range report.States {
		if stateCount.State != states[index] {
			return fmt.Errorf("operator client: software report 第 %d 種狀態是 %q，這個版本這裡是 %q",
				index, stateCount.State, states[index])
		}
		if stateCount.Count < 0 {
			return fmt.Errorf("operator client: software state %q 的格數是負的", stateCount.State)
		}
		if err := validateSoftwareStateSentences(stateCount.State, stateCount.Title,
			stateCount.Meaning, stateCount.NextStep); err != nil {
			return err
		}
		counted[stateCount.State] = stateCount.Count
	}
	countedRuntime, err := countedToolRuntimes("software report", report.RuntimeStates)
	if err != nil {
		return err
	}

	seen := map[operator.SoftwareState]int{}
	seenRuntime := map[operator.ToolRuntime]int{}
	toolNames := map[string]bool{}
	drifted, misattributed := 0, 0
	reporting := map[string]bool{}
	for _, tool := range report.Tools {
		if err := validateSoftwareTool(tool, report.Machines); err != nil {
			return err
		}
		if toolNames[tool.Name] {
			return fmt.Errorf("operator client: software report 有兩列 %q", tool.Name)
		}
		toolNames[tool.Name] = true
		if tool.Spread > 1 {
			drifted++
		}
		for _, row := range tool.Rows {
			seen[row.State]++
			// 一台只要有一格不是「沒回報過」，它就回報過。
			if row.State != operator.SoftwareUnreported {
				reporting[row.MachineID] = true
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
	if misattributed != report.Misattributed {
		return fmt.Errorf("operator client: software report 說 %d 格的版號講的不是正在跑的那一份，"+
			"逐格數出 %d 格", report.Misattributed, misattributed)
	}
	if drifted != report.Drifted {
		return fmt.Errorf("operator client: software report 說 %d 個工具版號不一致，逐列數出 %d 個",
			report.Drifted, drifted)
	}
	if len(reporting) != report.Reporting {
		return fmt.Errorf("operator client: software report 說 %d 台回報過，逐格數出 %d 台",
			report.Reporting, len(reporting))
	}
	// ⚠ 狀態摘要要逐格數過，不是只檢查它們加得起來。一份把「沒回報過」搬進「這台
	// 上沒有」的摘要照樣加得起來，而那兩件事的下一步完全不同。
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

func validateSoftwareTool(tool operator.SoftwareTool, machines int) error {
	if err := validateMachineClientText("software tool name", tool.Name, 256); err != nil {
		return err
	}
	if tool.InstalledOn < 0 || tool.AbsentOn < 0 || tool.UnreportedOn < 0 || tool.Spread < 0 {
		return fmt.Errorf("operator client: 工具 %q 有負數", tool.Name)
	}
	// ⚠ 三個加起來就是分母。少掉一台，畫面上會出現一個比實際小的「幾台上有」——
	// 而沒被算到的那一台正是最該被看到的那一台。
	if total := tool.InstalledOn + tool.AbsentOn + tool.UnreportedOn; total != machines {
		return fmt.Errorf("operator client: 工具 %q 的 %d+%d+%d 台，分母是 %d 台",
			tool.Name, tool.InstalledOn, tool.AbsentOn, tool.UnreportedOn, machines)
	}
	if len(tool.Rows) != machines {
		return fmt.Errorf("operator client: 工具 %q 有 %d 列，分母是 %d 台",
			tool.Name, len(tool.Rows), machines)
	}
	if tool.Headline == "" {
		return fmt.Errorf("operator client: 工具 %q 沒有那一行字", tool.Name)
	}

	installed, absent, unreported := 0, 0, 0
	versioned := map[string]int{}
	ids := map[string]bool{}
	for _, row := range tool.Rows {
		if err := validateSoftwareRow(tool.Name, row); err != nil {
			return err
		}
		if ids[row.MachineID] {
			return fmt.Errorf("operator client: 工具 %q 有兩列 %s", tool.Name, row.MachineID)
		}
		ids[row.MachineID] = true
		switch {
		case operator.SoftwareStateInstalled(row.State):
			installed++
		case row.State == operator.SoftwareAbsent:
			absent++
		default:
			unreported++
		}
		if row.Version != "" {
			versioned[row.Version]++
		}
	}
	if installed != tool.InstalledOn || absent != tool.AbsentOn || unreported != tool.UnreportedOn {
		return fmt.Errorf("operator client: 工具 %q 的摘要說有 %d／沒有 %d／沒回報 %d，"+
			"逐列數出 %d／%d／%d", tool.Name, tool.InstalledOn, tool.AbsentOn, tool.UnreportedOn,
			installed, absent, unreported)
	}
	if len(versioned) != tool.Spread {
		return fmt.Errorf("operator client: 工具 %q 說看到 %d 種版號，逐列數出 %d 種",
			tool.Name, tool.Spread, len(versioned))
	}
	if len(tool.Versions) != len(versioned) {
		return fmt.Errorf("operator client: 工具 %q 的版號分佈有 %d 項，逐列數出 %d 種",
			tool.Name, len(tool.Versions), len(versioned))
	}
	newest := 0
	for _, version := range tool.Versions {
		if version.Version == "" {
			return fmt.Errorf("operator client: 工具 %q 的版號分佈裡有一項沒有版號", tool.Name)
		}
		if version.Machines != versioned[version.Version] {
			return fmt.Errorf("operator client: 工具 %q 說 %s 裝在 %d 台上，逐列數出 %d 台",
				tool.Name, version.Version, version.Machines, versioned[version.Version])
		}
		if version.Newest {
			newest++
			if version.Version != tool.Newest {
				return fmt.Errorf("operator client: 工具 %q 標了 %s 是最新，摘要說是 %s",
					tool.Name, version.Version, tool.Newest)
			}
		}
	}
	// ⚠ 最新只能有一個。兩個都標成最新的話，畫面上會有兩台互相說對方落後。
	if newest > 1 {
		return fmt.Errorf("operator client: 工具 %q 有 %d 個版號都說自己是最新", tool.Name, newest)
	}
	if tool.Newest != "" && newest != 1 {
		return fmt.Errorf("operator client: 工具 %q 說最新是 %s，版號分佈裡沒有標它",
			tool.Name, tool.Newest)
	}
	return nil
}

func validateSoftwareRow(toolName string, row operator.SoftwareRow) error {
	if err := validateMachineClientText("software machine_id", row.MachineID, 256); err != nil {
		return err
	}
	if err := validateMachineClientText("software display_name", row.DisplayName, 256); err != nil {
		return err
	}
	if err := validateSoftwareStateSentences(row.State, row.Title, row.Meaning, row.NextStep); err != nil {
		return err
	}
	// ⚠⚠ 有版號就一定是裝著的。反過來，一格說「這台上沒有」卻帶著版號，講的是兩件
	// 互相矛盾的事，而畫面只會印出其中一件。
	if row.Version != "" && !operator.SoftwareStateInstalled(row.State) {
		return fmt.Errorf("operator client: 工具 %q 在 %s 上是 %q，卻帶著版號 %s",
			toolName, row.MachineID, row.State, row.Version)
	}
	// FromDisk 講的是「這個版號是從檔案讀的」——沒有版號就沒有這件事好講。
	if row.FromDisk && row.Version == "" {
		return fmt.Errorf("operator client: 工具 %q 在 %s 上沒有版號，卻說版號是從檔案讀的",
			toolName, row.MachineID)
	}
	// Shadowed 講的是「叫到的是另一個檔案」——那件事只有在這台上真的有它的時候成立。
	if row.Shadowed && !operator.SoftwareStateInstalled(row.State) {
		return fmt.Errorf("operator client: 工具 %q 在 %s 上是 %q，卻說它被另一條 PATH 遮住",
			toolName, row.MachineID, row.State)
	}
	if err := validateSoftwareRowRuntime(toolName, row); err != nil {
		return err
	}
	// 沒回報過的那一格不會有時刻——那正是「沒回報過」的意思。
	if row.State == operator.SoftwareUnreported && (row.MeasuredAt != nil || row.ObservedAt != nil) {
		return fmt.Errorf("operator client: 工具 %q 在 %s 上說沒回報過，卻給了時刻",
			toolName, row.MachineID)
	}
	for name, value := range map[string]*time.Time{
		"measured_at": row.MeasuredAt, "observed_at": row.ObservedAt,
	} {
		if value != nil && (value.IsZero() || value.Location() != time.UTC) {
			return fmt.Errorf("operator client: 工具 %q 在 %s 上的 %s 不是可用的 UTC 時刻",
				toolName, row.MachineID, name)
		}
	}
	return nil
}

// validateSoftwareRowRuntime 釘住這一格的第二軸：那個版號講的是哪一份安裝。
func validateSoftwareRowRuntime(toolName string, row operator.SoftwareRow) error {
	// ⚠⚠ 有這一軸就等於「這台上有這個工具」。一格說「這台上沒有」卻回答「正在跑的是
	// 另一個檔案」，講的是兩件互相矛盾的事，而畫面只會印出其中一件；反過來，一格裝著
	// 卻不回答這一軸，會讓那個版號替兩份安裝發言而沒有人知道。
	if operator.SoftwareStateInstalled(row.State) != (row.Runtime != nil) {
		return fmt.Errorf("operator client: 工具 %q 在 %s 上是 %q，「版號講的是哪一份」卻 %s",
			toolName, row.MachineID, row.State, toolRuntimePresence(row.Runtime != nil))
	}
	if row.Runtime == nil {
		return nil
	}
	return validateToolRuntimeFinding(
		fmt.Sprintf("工具 %q 在 %s 上的「版號講的是哪一份」", toolName, row.MachineID), *row.Runtime)
}

// validateSoftwareStateSentences 釘住「這一格是什麼」與「它講的那三句話」是同一件事。
//
// 對面的 Hub 對某一個狀態有第二種說法時，畫面上會出現一個看起來合理、其實指錯下一
// 步的句子——而操作員就是照那一句決定要去裝東西，還是去看那台的 agent。
func validateSoftwareStateSentences(stateValue operator.SoftwareState,
	title, meaning, nextStep string,
) error {
	// ⚠ 這一句擋的是「有一格落在這個版本不認得的狀態上」。逐格的比對只對得到這個
	// 版本認得的那幾種，一格落在認不得的狀態上就會從那個比對裡整個消失。
	if operator.SoftwareStateTitle(stateValue) == "" {
		return fmt.Errorf("operator client: software state %q 這個版本不認得", stateValue)
	}
	if title != operator.SoftwareStateTitle(stateValue) ||
		meaning != operator.SoftwareStateMeaning(stateValue) ||
		nextStep != operator.SoftwareStateNextStep(stateValue) {
		return fmt.Errorf("operator client: software state %q 的句子跟這個版本不一樣", stateValue)
	}
	return nil
}
