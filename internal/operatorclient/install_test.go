package operatorclient

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// 這份 fixture 用到的那幾個檔案。⚠ 它們是手寫的，不是叫 operator.ToolRuntimeOf 算出
// 來的：用被測的那一支函式造測資，函式怎麼改測資就跟著改，於是重數永遠對得起來。
const (
	installClientMeasured = "/usr/local/bin/openclaw"
	installClientRelease  = "/opt/openclaw/2026.6.6/openclaw"
	installClientOther    = "/opt/openclaw/2026.6.7/openclaw"
	installClientGone     = "/usr/local/bin/openclaw.1787036247195252617.old"
)

// installClientRuntime 手寫一格「版號講的是哪一份」。
func installClientRuntime(stateValue operator.ToolRuntime, running string) *operator.ToolRuntimeFinding {
	finding := operator.ToolRuntimeFinding{
		State:    stateValue,
		Title:    operator.ToolRuntimeTitle(stateValue),
		Meaning:  operator.ToolRuntimeMeaning(stateValue),
		NextStep: operator.ToolRuntimeNextStep(stateValue),

		MeasuredFile: installClientMeasured,
		RunningFile:  running,
	}
	return &finding
}

// installClientReport 造一份十種狀態都有格子的報告，數字自己對得起來。
//
// ⚠ 「版號講的是哪一份」那一軸刻意讓「正在跑的是另一個檔案」佔兩格。同一種狀態只有
// 一格的話，一份把那一格數成 1 而不是逐格累加的摘要照樣對得起來。
func installClientReport(now time.Time) operator.InstallReport {
	measured := now.Add(-2 * time.Minute)
	observed := now.Add(-time.Minute)
	assignedAt := now.Add(-24 * time.Hour)

	// 一台一種狀態，所以每一種狀態在 openclaw 那一列上剛好一格。
	machines := []struct {
		id, name           string
		state              operator.InstallState
		assigned, observed string
		fromDisk           bool
		runtime            *operator.ToolRuntimeFinding
	}{
		// ⚠ 「指派的跟看到的一樣」這一格在跑的那個檔案已經不見了——這一頁上最讓人放下
		// 心的一格，照樣可以是量在一份沒有人在跑的安裝上。
		{"m-matches", "aa-matches", operator.InstallMatches, "2026.6.10", "2026.6.10", false,
			installClientRuntime(operator.ToolRuntimeGoneFile, installClientGone)},
		{"m-newer", "ab-newer", operator.InstallAssignedNewer, "2026.6.10", "2026.6.6", false,
			installClientRuntime(operator.ToolRuntimeOtherFile, installClientOther)},
		{"m-older", "ac-older", operator.InstallAssignedOlder, "2026.5.26", "2026.6.6", true,
			installClientRuntime(operator.ToolRuntimeOtherFile, installClientRelease)},
		{"m-incomparable", "ad-incomparable", operator.InstallIncomparable, "2026.5.26", "2026.6.6-rc1", false,
			installClientRuntime(operator.ToolRuntimeSameFile, installClientMeasured)},
		{"m-nospec", "ae-nospec", operator.InstallAssignedNoVersion, "", "2026.6.6", false,
			installClientRuntime(operator.ToolRuntimeUnattributed, "")},
		{"m-mute", "af-mute", operator.InstallObservedNoVersion, "2026.5.26", "", false,
			installClientRuntime(operator.ToolRuntimeIdle, "")},
		{"m-absent", "ag-absent", operator.InstallAbsent, "2026.5.26", "", false, nil},
		{"m-unobserved", "ah-unobserved", operator.InstallUnobserved, "2026.5.26", "", false, nil},
		{"m-unreported", "ai-unreported", operator.InstallUnreported, "2026.5.26", "", false, nil},
		{"m-unassigned", "aj-unassigned", operator.InstallUnassigned, "", "", false, nil},
	}
	report := operator.InstallReport{
		SchemaVersion: operator.InstallReportSchemaVersion, EvaluatedAt: now,
		Machines: len(machines), Caveat: operator.InstallReportCaveat,
	}
	resource := operator.InstallResource{
		Name: "openclaw", ResourceKind: "openclaw", ResourceID: "openclaw",
	}
	counts := map[operator.InstallState]int{}
	runtimeCounts := map[operator.ToolRuntime]int{}
	for index, machine := range machines {
		row := operator.InstallRow{
			MachineID: machine.id, DisplayName: machine.name, State: machine.state,
			Title:    operator.InstallStateTitle(machine.state),
			Meaning:  operator.InstallStateMeaning(machine.state),
			NextStep: operator.InstallStateNextStep(machine.state),
			Assigned: machine.assigned, Observed: machine.observed, FromDisk: machine.fromDisk,
			Runtime: machine.runtime,
		}
		if machine.runtime != nil {
			runtimeCounts[machine.runtime.State]++
			if operator.ToolRuntimeMisattributed(machine.runtime.State) {
				resource.MisattributedOn++
				report.Misattributed++
			}
		}
		if operator.InstallStateAssigned(machine.state) {
			row.Scope, row.ScopeID = "machine", machine.id
			row.ScopeLabel = "指派給這台"
			row.Revision = int64(index + 1)
			at := assignedAt
			row.AssignedAt, row.AssignedBy = &at, "operator@test"
			resource.AssignedOn++
			if machine.state == operator.InstallMatches {
				resource.MatchingOn++
			}
			if operator.InstallStateDiffers(machine.state) {
				resource.DifferingOn++
			}
		} else {
			resource.UnassignedOn++
		}
		switch machine.state {
		case operator.InstallUnassigned, operator.InstallUnreported, operator.InstallUnobserved:
		default:
			row.MeasuredAt, row.ObservedAt = &measured, &observed
		}
		if operator.InstallStateAssigned(machine.state) {
			report.Assigned++
		}
		counts[machine.state]++
		resource.Rows = append(resource.Rows, row)
	}
	resource.Headline = operator.InstallResourceHeadline(resource)
	report.Resources = append(report.Resources, resource)
	report.Differing = 1
	for _, stateValue := range operator.InstallStates() {
		report.States = append(report.States, operator.InstallStateCount{
			State: stateValue, Title: operator.InstallStateTitle(stateValue),
			Count:    counts[stateValue],
			Meaning:  operator.InstallStateMeaning(stateValue),
			NextStep: operator.InstallStateNextStep(stateValue),
		})
	}
	for _, stateValue := range operator.ToolRuntimes() {
		report.RuntimeStates = append(report.RuntimeStates, operator.ToolRuntimeCount{
			State: stateValue, Title: operator.ToolRuntimeTitle(stateValue),
			Count:    runtimeCounts[stateValue],
			Meaning:  operator.ToolRuntimeMeaning(stateValue),
			NextStep: operator.ToolRuntimeNextStep(stateValue),
		})
	}
	report.Headline = operator.InstallReportHeadline(report)
	return report
}

func TestTheInstallClientAsksTheCanonicalPath(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	client, asked := recordingReportServer(t, installClientReport(now))
	report, err := client.InstallReport(t.Context())
	if err != nil {
		t.Fatalf("一致的每機安裝狀態被拒絕：%v", err)
	}
	if *asked != "/v1/operator/install-report" {
		t.Fatalf("用戶端問的是 %q", *asked)
	}
	if len(report.Resources) != 1 || len(report.Resources[0].Rows) != report.Machines {
		t.Fatalf("report=%+v", report)
	}
	// fixture 要真的把十種狀態都放進去，否則下面那一長串 mutation 有一半沒有格子可改。
	seen := map[operator.InstallState]bool{}
	for _, row := range report.Resources[0].Rows {
		seen[row.State] = true
	}
	for _, stateValue := range operator.InstallStates() {
		if !seen[stateValue] {
			t.Errorf("fixture 沒有 %s 那一格", stateValue)
		}
	}
	// ⚠ 這一軸也要真的有格子，而且「正在跑的是另一個檔案」要有兩格。沒有格子的話，
	// 下面那一長串 mutation 有一整段改的是空氣。
	seenRuntime := map[operator.ToolRuntime]int{}
	for _, row := range report.Resources[0].Rows {
		if row.Runtime != nil {
			seenRuntime[row.Runtime.State]++
		}
	}
	if seenRuntime[operator.ToolRuntimeOtherFile] != 2 {
		t.Errorf("fixture 的「正在跑的是另一個檔案」有 %d 格，要有 2 格",
			seenRuntime[operator.ToolRuntimeOtherFile])
	}
	for _, stateValue := range []operator.ToolRuntime{
		operator.ToolRuntimeGoneFile, operator.ToolRuntimeSameFile,
		operator.ToolRuntimeUnattributed, operator.ToolRuntimeIdle,
	} {
		if seenRuntime[stateValue] == 0 {
			t.Errorf("fixture 沒有 %s 那一格", stateValue)
		}
	}
	if report.Misattributed != 3 {
		t.Errorf("fixture 有三格量的不是正在跑的那一份，報告說 %d", report.Misattributed)
	}
}

// installRuntimeCount 拿這一軸某一種狀態的那一格摘要。
func installRuntimeCount(report *operator.InstallReport,
	stateValue operator.ToolRuntime,
) *operator.ToolRuntimeCount {
	for index := range report.RuntimeStates {
		if report.RuntimeStates[index].State == stateValue {
			return &report.RuntimeStates[index]
		}
	}
	return nil
}

func installRowWithState(report *operator.InstallReport, state operator.InstallState) *operator.InstallRow {
	for index := range report.Resources[0].Rows {
		if report.Resources[0].Rows[index].State == state {
			return &report.Resources[0].Rows[index]
		}
	}
	return nil
}

// 這份報告的價值全在於那幾個數字跟那幾句話。一份自相矛盾的回應——把沒有被指派過
// 算成對得起來、一格說沒被指派過卻帶著帳本號碼、把那句「東西可以從指派以外的路徑
// 裝上去」拿掉——不是拿來顯示的東西，是拿來拒收的。
func TestTheInstallClientRefusesAReportThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	other := time.FixedZone("CST", 8*3600)
	for name, edit := range map[string]func(*operator.InstallReport){
		"沒講 schema 版本": func(r *operator.InstallReport) { r.SchemaVersion++ },
		"沒有讀取時刻":       func(r *operator.InstallReport) { r.EvaluatedAt = time.Time{} },
		"讀取時刻不是 UTC": func(r *operator.InstallReport) {
			r.EvaluatedAt = r.EvaluatedAt.In(other)
		},
		"有負數":        func(r *operator.InstallReport) { r.Machines, r.Assigned = -1, -1 },
		"被指派過的比分母多":  func(r *operator.InstallReport) { r.Assigned = r.Machines + 1 },
		"被指派過的台數對不上": func(r *operator.InstallReport) { r.Assigned-- },
		"對不上的資源數對不上": func(r *operator.InstallReport) { r.Differing = 0 },
		// ⚠⚠ 那句限制被拿掉的話，一列「指派的比看到的舊」會被讀成「有人亂動這台」。
		"那句限制被拿掉了": func(r *operator.InstallReport) { r.Caveat = "" },
		"那句限制被換了一種說法": func(r *operator.InstallReport) {
			r.Caveat = "安裝資訊僅供參考。"
		},
		"狀態少一種": func(r *operator.InstallReport) { r.States = r.States[1:] },
		"狀態順序被換過": func(r *operator.InstallReport) {
			r.States[0], r.States[1] = r.States[1], r.States[0]
		},
		"狀態的格數是負的": func(r *operator.InstallReport) {
			r.States[0].Count, r.States[1].Count = -1, 2
		},
		"摘要說的格數跟逐格數的不一樣": func(r *operator.InstallReport) {
			r.States[0].Count, r.States[1].Count = 2, 0
		},
		"狀態的說明被改過": func(r *operator.InstallReport) {
			r.States[0].Meaning = "這台沒事。"
		},
		"狀態的下一步被改過": func(r *operator.InstallReport) {
			r.States[1].NextStep = "重開機。"
		},
		// ⚠ 這兩件事的下一步完全不同：一個要先指派一次，一個要去看工作單。
		"沒有被指派過被算成對得起來": func(r *operator.InstallReport) {
			row := installRowWithState(r, operator.InstallUnassigned)
			row.State = operator.InstallMatches
			row.Title = operator.InstallStateTitle(operator.InstallMatches)
			row.Meaning = operator.InstallStateMeaning(operator.InstallMatches)
			row.NextStep = operator.InstallStateNextStep(operator.InstallMatches)
		},
		"這個版本不認得的狀態": func(r *operator.InstallReport) {
			r.Resources[0].Rows[0].State = operator.InstallState("probably_fine")
		},
		"一格的句子跟狀態不一樣": func(r *operator.InstallReport) {
			r.Resources[0].Rows[0].Title = "看起來沒事"
		},
		"資源的名字不是從身分算出來的": func(r *operator.InstallReport) {
			r.Resources[0].Name = "OpenClaw"
		},
		"資源有兩列同一台機器": func(r *operator.InstallReport) {
			r.Resources[0].Rows[1].MachineID = r.Resources[0].Rows[0].MachineID
		},
		"資源的列數比分母少": func(r *operator.InstallReport) {
			r.Resources[0].Rows = r.Resources[0].Rows[1:]
		},
		"指派加沒指派不等於分母": func(r *operator.InstallReport) {
			r.Resources[0].AssignedOn--
		},
		"一樣的台數對不上":  func(r *operator.InstallReport) { r.Resources[0].MatchingOn++ },
		"不一樣的台數對不上": func(r *operator.InstallReport) { r.Resources[0].DifferingOn = 0 },
		"資源沒有那一行字":  func(r *operator.InstallReport) { r.Resources[0].Headline = "" },
		"有負數的資源統計": func(r *operator.InstallReport) {
			r.Resources[0].MatchingOn = -1
		},
		// ⚠⚠ 一格說沒有被指派過卻帶著帳本號碼，畫面會印出一個不屬於這台機器的 revision。
		"沒被指派過卻帶著 revision": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallUnassigned).Revision = 7
		},
		"沒被指派過卻帶著版號": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallUnassigned).Assigned = "2026.6.10"
		},
		"沒被指派過卻帶著指派時刻": func(r *operator.InstallReport) {
			at := r.EvaluatedAt.Add(-time.Hour)
			installRowWithState(r, operator.InstallUnassigned).AssignedAt = &at
		},
		"被指派過卻沒有 revision": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).Revision = 0
		},
		"被指派過卻沒有指派時刻": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).AssignedAt = nil
		},
		"被指派過卻沒有指派來源": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).ScopeLabel = ""
		},
		"指派範圍這個版本不認得": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).Scope = "tag"
		},
		"被指派過卻沒有指派的版號": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).Assigned = ""
		},
		"說指派沒講版號卻帶著版號": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallAssignedNoVersion).Assigned = "2026.6.10"
		},
		// ⚠⚠ 一格說「這台上沒有」卻帶著看到的版號，講的是兩件互相矛盾的事。
		"這台上沒有卻帶著看到的版號": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallAbsent).Observed = "2026.6.6"
		},
		"沒回報過卻帶著看到的版號": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallUnreported).Observed = "2026.6.6"
		},
		"看不到版號卻說版號是從檔案讀的": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallAbsent).FromDisk = true
		},
		"沒回報過卻給了觀測時刻": func(r *operator.InstallReport) {
			at := r.EvaluatedAt.Add(-time.Minute)
			installRowWithState(r, operator.InstallUnreported).ObservedAt = &at
		},
		"沒有被指派過卻給了觀測時刻": func(r *operator.InstallReport) {
			at := r.EvaluatedAt.Add(-time.Minute)
			installRowWithState(r, operator.InstallUnassigned).MeasuredAt = &at
		},
		"量測時刻不是 UTC": func(r *operator.InstallReport) {
			local := r.EvaluatedAt.Add(-time.Minute).In(other)
			r.Resources[0].Rows[0].MeasuredAt = &local
		},
		"指派時刻不是 UTC": func(r *operator.InstallReport) {
			local := r.EvaluatedAt.Add(-time.Hour).In(other)
			r.Resources[0].Rows[0].AssignedAt = &local
		},
		"機器名稱裡有終端機跳脫序列": func(r *operator.InstallReport) {
			r.Resources[0].Rows[0].DisplayName = "samplehub1\x1b[2J"
		},
		"資源名稱裡有終端機跳脫序列": func(r *operator.InstallReport) {
			r.Resources[0].ResourceID = "openclaw\x1b[2J"
			r.Resources[0].Name = "openclaw\x1b[2J"
		},
		"指派範圍裡有終端機跳脫序列": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).ScopeID = "canary\x1b[2J"
		},

		// 「版號講的是哪一份」那一軸。
		"量錯檔案的格數是負的": func(r *operator.InstallReport) { r.Misattributed = -1 },
		// ⚠⚠ 這個數字被壓成 0 的話，畫面上「指派的比看到的舊」就成了唯一一句話，而它比
		// 的是一個沒有人在跑的檔案——照它做的第一件事會是回滾一台其實沒事的機器。
		"量錯檔案的格數被壓成 0": func(r *operator.InstallReport) { r.Misattributed = 0 },
		"量錯檔案的格數被多算了":  func(r *operator.InstallReport) { r.Misattributed++ },
		"資源上量錯檔案的台數對不上": func(r *operator.InstallReport) {
			r.Resources[0].MisattributedOn = 0
		},
		"資源上量錯檔案的台數是負的": func(r *operator.InstallReport) {
			r.Resources[0].MisattributedOn = -1
		},
		"這一軸少一種狀態": func(r *operator.InstallReport) {
			r.RuntimeStates = r.RuntimeStates[1:]
		},
		"這一軸的順序被換過": func(r *operator.InstallReport) {
			r.RuntimeStates[0], r.RuntimeStates[1] = r.RuntimeStates[1], r.RuntimeStates[0]
		},
		"這一軸的格數是負的": func(r *operator.InstallReport) {
			r.RuntimeStates[0].Count, r.RuntimeStates[2].Count = -1, 3
		},
		// ⚠ 把「正在跑的是另一個檔案」搬進「沒有找到在跑它的 process」，兩邊加起來一樣。
		"這一軸的摘要跟逐格數的不一樣": func(r *operator.InstallReport) {
			r.RuntimeStates[0].Count = 0
			installRuntimeCount(r, operator.ToolRuntimeIdle).Count = 2
		},
		"這一軸只數到 1": func(r *operator.InstallReport) {
			installRuntimeCount(r, operator.ToolRuntimeOtherFile).Count = 1
		},
		"這一軸的說明被改過": func(r *operator.InstallReport) {
			r.RuntimeStates[0].Meaning = "這台沒事。"
		},
		"這一軸的下一步被改過": func(r *operator.InstallReport) {
			r.RuntimeStates[0].NextStep = "重開機。"
		},
		"這一軸有這個版本不認得的狀態": func(r *operator.InstallReport) {
			r.RuntimeStates[0].State = operator.ToolRuntime("probably_fine")
		},
		// ⚠⚠ 這一格是重數擋不住的那一種：把那一軸整個從「裝了問不到版號」搬到「這台上
		// 沒有」，六種狀態的格數與量錯檔案的格數全都一模一樣，加起來照樣對得起來——而畫
		// 面上會出現一台說「它上面沒有這個東西」卻同時回答「沒有找到在跑它的 process」
		// 的機器。只有「有這一軸等於這台回報說它上面有」那一句擋得住。
		"這一軸被搬到一台說它上面沒有的機器上": func(r *operator.InstallReport) {
			moved := installRowWithState(r, operator.InstallObservedNoVersion).Runtime
			installRowWithState(r, operator.InstallObservedNoVersion).Runtime = nil
			installRowWithState(r, operator.InstallAbsent).Runtime = moved
		},
		// ⚠⚠ 一格說「這台上沒有」卻回答「正在跑的是另一個檔案」，講的是兩件互相矛盾的事。
		"這台上沒有卻回答了這一軸": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallAbsent).Runtime =
				installClientRuntime(operator.ToolRuntimeOtherFile, installClientRelease)
		},
		"沒回報過卻回答了這一軸": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallUnreported).Runtime =
				installClientRuntime(operator.ToolRuntimeIdle, "")
		},
		"沒有被指派過卻回答了這一軸": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallUnassigned).Runtime =
				installClientRuntime(operator.ToolRuntimeIdle, "")
		},
		// ⚠⚠ 反過來：一格說這台上有這個東西，卻不講那個版號量的是哪一份。
		"看到了卻不回答這一軸": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).Runtime = nil
		},
		"裝了問不到版號卻不回答這一軸": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallObservedNoVersion).Runtime = nil
		},
		"一格的句子跟這一軸的狀態不一樣": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).Runtime.Title = "看起來沒事"
		},
		"一格落在這一軸不認得的狀態上": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).Runtime.State =
				operator.ToolRuntime("probably_fine")
		},
		// ⚠ 說得出跑的是哪一個檔案的那三種一定要講出來，說不出來的那三種一定要留白。
		"說正在跑的是另一個檔案卻不講是哪一個": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallAssignedOlder).Runtime.RunningFile = ""
		},
		"沒找到 process 卻講了一個在跑的檔案": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallObservedNoVersion).Runtime.RunningFile =
				installClientRelease
		},
		"說量的就是跑的卻不講量的是哪一個": func(r *operator.InstallReport) {
			row := installRowWithState(r, operator.InstallIncomparable)
			row.Runtime.MeasuredFile = ""
		},
		// ⚠⚠ 兩邊寫同一個路徑的話，那句「把另一份收掉」指的就是唯一那一份。
		"說正在跑的是另一個檔案而兩邊是同一份": func(r *operator.InstallReport) {
			row := installRowWithState(r, operator.InstallAssignedOlder)
			row.Runtime.RunningFile = row.Runtime.MeasuredFile
		},
		"兩邊是同一份只差一個點": func(r *operator.InstallReport) {
			row := installRowWithState(r, operator.InstallAssignedOlder)
			row.Runtime.RunningFile = "/usr/local/bin/./openclaw"
			row.Runtime.MeasuredFile = installClientMeasured
		},
		"在跑的那個檔案路徑裡有終端機跳脫序列": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallAssignedOlder).Runtime.RunningFile =
				installClientRelease + "\x1b[2J"
		},
		"量版號的那個檔案路徑裡有終端機跳脫序列": func(r *operator.InstallReport) {
			installRowWithState(r, operator.InstallMatches).Runtime.MeasuredFile =
				installClientMeasured + "\x1b[2J"
		},
	} {
		report := installClientReport(now)
		edit(&report)
		if _, err := complianceClientServer(t, report).InstallReport(t.Context()); err == nil {
			t.Errorf("%s：自相矛盾的每機安裝狀態被接受了", name)
		}
	}
}
