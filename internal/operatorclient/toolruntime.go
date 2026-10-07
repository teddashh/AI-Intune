package operatorclient

import (
	"fmt"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// 「版號講的是哪一份」那一軸的重數，軟體清查與每機安裝狀態共用這一份。
//
// ⚠⚠ 共用不是為了少打字。兩頁各寫一份的話，其中一頁哪天漏掉「正在跑的是另一個檔案
// 必須真的是另一個檔案」那一句，那一頁就會印出一句「把另一份收掉」而那兩個路徑其實
// 是同一份安裝——而操作員照那一句刪掉的，會是機器上唯一的那一份。

// countedToolRuntimes 把那六格摘要收成一張表。
//
// ⚠ 順序也要比。這一軸刻意把要人動手的兩種排在最前面，對面照字母重排過的話，畫面上
// 最先看到的會是機隊上永遠最多的那一種「沒有找到在跑它的 process」。
func countedToolRuntimes(where string, states []operator.ToolRuntimeCount,
) (map[operator.ToolRuntime]int, error) {
	runtimes := operator.ToolRuntimes()
	if len(states) != len(runtimes) {
		return nil, fmt.Errorf("operator client: %s has %d runtime attribution states, this version recognizes %d",
			where, len(states), len(runtimes))
	}
	counted := map[operator.ToolRuntime]int{}
	for index, stateCount := range states {
		if stateCount.State != runtimes[index] {
			return nil, fmt.Errorf("operator client: %s runtime attribution state %d is %q, "+
				"this version expects %q", where, index, stateCount.State, runtimes[index])
		}
		if stateCount.Count < 0 {
			return nil, fmt.Errorf("operator client: tool runtime %q count is negative", stateCount.State)
		}
		if err := validateToolRuntimeSentences(stateCount.State, stateCount.Title,
			stateCount.Meaning, stateCount.NextStep); err != nil {
			return nil, err
		}
		counted[stateCount.State] = stateCount.Count
	}
	return counted, nil
}

// validateToolRuntimeSentences 釘住「這一格是什麼」與「它講的那三句話」是同一件事。
//
// 對面對某一個狀態有第二種說法時，畫面上會出現一句看起來合理、其實指錯下一步的話：
// 「決定要留哪一份，再把另一份收掉」印在一格其實只是沒找到 process 的位置上，就會
// 有人去刪一台機器上唯一的那一份安裝。
func validateToolRuntimeSentences(stateValue operator.ToolRuntime,
	title, meaning, nextStep string,
) error {
	// ⚠ 逐格的比對只對得到這個版本認得的那幾種，一格落在認不得的狀態上會從那個比對
	// 裡整個消失。
	if operator.ToolRuntimeTitle(stateValue) == "" {
		return fmt.Errorf("operator client: tool runtime %q is unrecognized by this version", stateValue)
	}
	if title != operator.ToolRuntimeTitle(stateValue) ||
		meaning != operator.ToolRuntimeMeaning(stateValue) ||
		nextStep != operator.ToolRuntimeNextStep(stateValue) {
		return fmt.Errorf("operator client: tool runtime %q sentences do not match this version", stateValue)
	}
	return nil
}

// validateToolRuntimeFinding 釘住一格的那一軸自己講得通。where 是這一格在畫面上的
// 位置，因為一句「兩邊都是同一個路徑」不講是哪一台的哪一個東西，查不回去。
func validateToolRuntimeFinding(where string, finding operator.ToolRuntimeFinding) error {
	if err := validateToolRuntimeSentences(finding.State, finding.Title,
		finding.Meaning, finding.NextStep); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"runtime measured_file": finding.MeasuredFile, "runtime running_file": finding.RunningFile,
	} {
		if value == "" {
			continue
		}
		if err := validateMachineClientText(name, value, 1024); err != nil {
			return err
		}
	}
	named := operator.ToolRuntimeNamesRunningFile(finding.State)
	// ⚠ 說得出跑的是哪一個檔案的那三種一定要把那個檔案講出來，說不出來的那三種一定
	// 要留白。一句「正在跑的是另一個檔案」不講是哪一個，讀的人沒辦法知道要去收掉哪一
	// 份；一格留白卻被算成一個發現，人會去找一個這份報告從來沒量到的檔案。
	if named != (finding.RunningFile != "") {
		return fmt.Errorf("operator client: %s is %q, but running file is %s",
			where, finding.State, toolRuntimePresence(finding.RunningFile != ""))
	}
	if finding.State == operator.ToolRuntimeSameFile && finding.MeasuredFile == "" {
		return fmt.Errorf("operator client: %s claims measured is running, but did not specify measured file", where)
	}
	// ⚠⚠ 「正在跑的是另一個檔案」必須真的是另一個檔案。同一個路徑寫在兩邊，那句
	// 「把另一份收掉」指的就是唯一那一份。
	if finding.State == operator.ToolRuntimeOtherFile &&
		operator.SameToolFile(finding.MeasuredFile, finding.RunningFile) {
		return fmt.Errorf("operator client: %s claims running file is another file, but both are %s",
			where, finding.RunningFile)
	}
	return nil
}

func toolRuntimePresence(present bool) string {
	if present {
		return "present"
	}
	return "omitted"
}
