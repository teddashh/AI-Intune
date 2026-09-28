package operatorclient

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// softwareClientReport 造一份六種狀態、七種「版號講的是哪一份」都有摘要格子的報告，
// 數字自己對得起來。
func softwareClientReport(now time.Time) operator.SoftwareReport {
	measured := now.Add(-2 * time.Minute)
	observed := now.Add(-time.Minute)

	// 每一種狀態、每一種「版號講的是哪一份」在 claude 那一列上都剛好一格。
	//
	// ⚠ m-newest 是正式機隊上 openclaw 的形狀：shadowed 是 false，正在跑的卻是另一個
	// 檔案。兩軸獨立這件事在對面被合成一個布林的時候，這一格就會說一切正常。
	machines := []struct {
		id, name string
		state    operator.SoftwareState
		version  string
		fromDisk bool
		shadowed bool
		runtime  operator.ToolRuntime
		running  string
	}{
		{"m-newest", "samplehub1", operator.SoftwareNewest, "2.1.195", false, false,
			operator.ToolRuntimeOtherFile, "/home/example-user/.local/share/clawctl/claude/releases/2.1.195/bin/claude"},
		{"m-behind", "sampleagent2", operator.SoftwareBehind, "2.1.100", false, true,
			operator.ToolRuntimeSameFile, softwareClientToolFile},
		{"m-incomparable", "sampleagent3", operator.SoftwareIncomparable, "2.1.195-rc1", false, false,
			operator.ToolRuntimeGoneFile, "/home/example-user/.local/bin/claude.old"},
		{"m-noversion", "sampleagent1", operator.SoftwareNoVersion, "", false, false,
			operator.ToolRuntimeUnattributed, ""},
		{"m-idle", "sampleagent4", operator.SoftwareNewest, "2.1.195", false, false,
			operator.ToolRuntimeIdle, ""},
		{"m-unstated", "sampleagent5", operator.SoftwareBehind, "2.1.100", false, false,
			operator.ToolRuntimeUnstated, ""},
		{"m-absent", "raspi1", operator.SoftwareAbsent, "", false, false, "", ""},
		{"m-unreported", "new-box", operator.SoftwareUnreported, "", false, false, "", ""},
	}
	report := operator.SoftwareReport{
		SchemaVersion: operator.SoftwareReportSchemaVersion, EvaluatedAt: now,
		Machines: len(machines), Caveat: operator.SoftwareReportCaveat,
	}
	tool := operator.SoftwareTool{Name: "claude", Newest: "2.1.195"}
	versioned := map[string]int{}
	counts := map[operator.SoftwareState]int{}
	runtimeCounts := map[operator.ToolRuntime]int{}
	for _, machine := range machines {
		row := operator.SoftwareRow{
			MachineID: machine.id, DisplayName: machine.name, State: machine.state,
			Title:    operator.SoftwareStateTitle(machine.state),
			Meaning:  operator.SoftwareStateMeaning(machine.state),
			NextStep: operator.SoftwareStateNextStep(machine.state),
			Version:  machine.version, FromDisk: machine.fromDisk, Shadowed: machine.shadowed,
		}
		if machine.state != operator.SoftwareUnreported {
			row.MeasuredAt, row.ObservedAt = &measured, &observed
			report.Reporting++
		}
		if machine.runtime != "" {
			finding := softwareClientRuntime(machine.runtime, machine.running)
			row.Runtime = &finding
			runtimeCounts[machine.runtime]++
			if operator.ToolRuntimeMisattributed(machine.runtime) {
				report.Misattributed++
			}
		}
		switch {
		case operator.SoftwareStateInstalled(machine.state):
			tool.InstalledOn++
		case machine.state == operator.SoftwareAbsent:
			tool.AbsentOn++
		default:
			tool.UnreportedOn++
		}
		if machine.version != "" {
			versioned[machine.version]++
		}
		counts[machine.state]++
		tool.Rows = append(tool.Rows, row)
	}
	for _, version := range []string{"2.1.195", "2.1.195-rc1", "2.1.100"} {
		tool.Versions = append(tool.Versions, operator.SoftwareVersion{
			Version: version, Machines: versioned[version], Newest: version == tool.Newest,
		})
	}
	tool.Spread = len(versioned)
	tool.Headline = operator.SoftwareToolHeadline(tool)
	report.Tools = append(report.Tools, tool)
	report.Drifted = 1
	for _, stateValue := range operator.SoftwareStates() {
		report.States = append(report.States, operator.SoftwareStateCount{
			State: stateValue, Title: operator.SoftwareStateTitle(stateValue),
			Count:    counts[stateValue],
			Meaning:  operator.SoftwareStateMeaning(stateValue),
			NextStep: operator.SoftwareStateNextStep(stateValue),
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
	report.Headline = operator.SoftwareReportHeadline(report)
	return report
}

// softwareClientToolFile 是這份 fixture 裡「量版號的那個檔案」。
const softwareClientToolFile = "/home/example-user/.local/bin/claude"

// softwareClientRuntime 手寫一格「版號講的是哪一份」。
//
// ⚠ 手寫，不呼叫 operator.ToolRuntimeOf。用戶端要擋的就是一份對面自己算錯的報告——
// 讓 fixture 走對面那條產生路徑，等於拿被測的那套邏輯來證明它自己。
func softwareClientRuntime(stateValue operator.ToolRuntime, running string) operator.ToolRuntimeFinding {
	return operator.ToolRuntimeFinding{
		State: stateValue, Title: operator.ToolRuntimeTitle(stateValue),
		Meaning:      operator.ToolRuntimeMeaning(stateValue),
		NextStep:     operator.ToolRuntimeNextStep(stateValue),
		MeasuredFile: softwareClientToolFile, RunningFile: running,
	}
}

// softwareClientEditRuntime 改掉第一格落在某一種「版號講的是哪一份」上的答案。
func softwareClientEditRuntime(report *operator.SoftwareReport, stateValue operator.ToolRuntime,
	edit func(*operator.ToolRuntimeFinding),
) {
	softwareClientEditRow(report, stateValue, func(row *operator.SoftwareRow) { edit(row.Runtime) })
}

func softwareClientEditRow(report *operator.SoftwareReport, stateValue operator.ToolRuntime,
	edit func(*operator.SoftwareRow),
) {
	for toolIndex := range report.Tools {
		for rowIndex := range report.Tools[toolIndex].Rows {
			row := &report.Tools[toolIndex].Rows[rowIndex]
			if row.Runtime != nil && row.Runtime.State == stateValue {
				edit(row)
				return
			}
		}
	}
}

// softwareClientRuntimeCells 是有回答那一軸的格數。
func softwareClientRuntimeCells(report operator.SoftwareReport) int {
	cells := 0
	for _, tool := range report.Tools {
		for _, row := range tool.Rows {
			if row.Runtime != nil {
				cells++
			}
		}
	}
	return cells
}

func softwareRuntimeCountIndex(report *operator.SoftwareReport, stateValue operator.ToolRuntime) int {
	for index := range report.RuntimeStates {
		if report.RuntimeStates[index].State == stateValue {
			return index
		}
	}
	return -1
}

func softwareRuntimeCount(report *operator.SoftwareReport,
	stateValue operator.ToolRuntime,
) *operator.ToolRuntimeCount {
	index := softwareRuntimeCountIndex(report, stateValue)
	if index < 0 {
		return nil
	}
	return &report.RuntimeStates[index]
}

func TestTheSoftwareClientAsksTheCanonicalPath(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	client, asked := recordingReportServer(t, softwareClientReport(now))
	report, err := client.SoftwareReport(t.Context())
	if err != nil {
		t.Fatalf("一致的軟體清查被拒絕：%v", err)
	}
	if *asked != "/v1/operator/software-report" {
		t.Fatalf("用戶端問的是 %q", *asked)
	}
	if len(report.Tools) != 1 || len(report.Tools[0].Rows) != report.Machines {
		t.Fatalf("report=%+v", report)
	}
}

// 這份報告的價值全在於那幾個數字跟那幾句話。一份自相矛盾的回應——把沉默算成
// 「沒有」、兩個版號都說自己最新、把那句「不是上游最新」拿掉——不是拿來顯示的
// 東西，是拿來拒收的。
func TestTheSoftwareClientRefusesAReportThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	other := time.FixedZone("CST", 8*3600)
	for name, edit := range map[string]func(*operator.SoftwareReport){
		"沒講 schema 版本": func(r *operator.SoftwareReport) { r.SchemaVersion++ },
		"沒有讀取時刻":       func(r *operator.SoftwareReport) { r.EvaluatedAt = time.Time{} },
		"讀取時刻不是 UTC": func(r *operator.SoftwareReport) {
			r.EvaluatedAt = r.EvaluatedAt.In(other)
		},
		"有負數":       func(r *operator.SoftwareReport) { r.Machines, r.Reporting = -1, -1 },
		"回報過的比分母多":  func(r *operator.SoftwareReport) { r.Reporting = r.Machines + 1 },
		"回報過的台數對不上": func(r *operator.SoftwareReport) { r.Reporting-- },
		// ⚠⚠ 沒有上游版本來源這件事被拿掉的話，一張全綠的表會被讀成「都是最新的」。
		"那句限制被拿掉了": func(r *operator.SoftwareReport) { r.Caveat = "" },
		"那句限制被換了一種說法": func(r *operator.SoftwareReport) {
			r.Caveat = "版本資訊僅供參考。"
		},
		"狀態少一種": func(r *operator.SoftwareReport) { r.States = r.States[1:] },
		"狀態順序被換過": func(r *operator.SoftwareReport) {
			r.States[0], r.States[1] = r.States[1], r.States[0]
		},
		"狀態的格數是負的": func(r *operator.SoftwareReport) {
			r.States[0].Count, r.States[1].Count = -1, 2
		},
		"摘要說的格數跟逐格數的不一樣": func(r *operator.SoftwareReport) {
			r.States[0].Count, r.States[1].Count = 2, 0
		},
		// ⚠ 這兩件事的下一步完全不同：一個要去裝東西，一個要去看那台的 agent。
		"沉默被算成「這台上沒有」": func(r *operator.SoftwareReport) {
			for index := range r.Tools[0].Rows {
				if r.Tools[0].Rows[index].State == operator.SoftwareUnreported {
					row := &r.Tools[0].Rows[index]
					row.State = operator.SoftwareAbsent
					row.Title = operator.SoftwareStateTitle(operator.SoftwareAbsent)
					row.Meaning = operator.SoftwareStateMeaning(operator.SoftwareAbsent)
					row.NextStep = operator.SoftwareStateNextStep(operator.SoftwareAbsent)
					r.Tools[0].AbsentOn, r.Tools[0].UnreportedOn = r.Tools[0].AbsentOn+1, r.Tools[0].UnreportedOn-1
					return
				}
			}
		},
		"狀態的意思換了一種說法": func(r *operator.SoftwareReport) {
			r.States[0].Meaning += "（大概）"
		},
		"狀態的下一步換了一種說法": func(r *operator.SoftwareReport) {
			r.States[1].NextStep = "去升級吧。"
		},
		"列上的標題跟狀態對不上": func(r *operator.SoftwareReport) {
			r.Tools[0].Rows[0].Title = operator.SoftwareStateTitle(operator.SoftwareAbsent)
		},
		"不認得的狀態": func(r *operator.SoftwareReport) {
			r.Tools[0].Rows[0].State, r.States[0].State = "unknown", "unknown"
		},
		"不認得的狀態連句子都不給": func(r *operator.SoftwareReport) {
			row := &r.Tools[0].Rows[0]
			row.State, row.Title, row.Meaning, row.NextStep = "unknown", "", "", ""
		},
		"兩列同一台": func(r *operator.SoftwareReport) {
			r.Tools[0].Rows[1].MachineID = r.Tools[0].Rows[0].MachineID
		},
		"兩列同一個工具": func(r *operator.SoftwareReport) {
			r.Tools = append(r.Tools, r.Tools[0])
			r.Drifted++
		},
		"少送一列":         func(r *operator.SoftwareReport) { r.Tools[0].Rows = r.Tools[0].Rows[1:] },
		"三個台數加起來不是分母":  func(r *operator.SoftwareReport) { r.Tools[0].InstalledOn++ },
		"版號不一致的工具數對不上": func(r *operator.SoftwareReport) { r.Drifted++ },
		"版號分佈少一項": func(r *operator.SoftwareReport) {
			r.Tools[0].Versions, r.Tools[0].Spread = r.Tools[0].Versions[1:], r.Tools[0].Spread-1
		},
		"版號分佈的台數對不上": func(r *operator.SoftwareReport) { r.Tools[0].Versions[0].Machines++ },
		// ⚠ 最新只能有一個，不然畫面上會有兩台互相說對方落後。
		"兩個版號都說自己最新": func(r *operator.SoftwareReport) { r.Tools[0].Versions[1].Newest = true },
		"說有最新卻沒標":    func(r *operator.SoftwareReport) { r.Tools[0].Versions[0].Newest = false },
		"標的最新跟摘要不一樣": func(r *operator.SoftwareReport) { r.Tools[0].Newest = "2.1.100" },
		"沒有那一行字":     func(r *operator.SoftwareReport) { r.Tools[0].Headline = "" },
		// ⚠⚠ 一格說「這台上沒有」卻帶著版號，講的是兩件互相矛盾的事。
		"說沒有卻給了版號": func(r *operator.SoftwareReport) {
			for index := range r.Tools[0].Rows {
				if r.Tools[0].Rows[index].State == operator.SoftwareAbsent {
					r.Tools[0].Rows[index].Version = "2.1.195"
					return
				}
			}
		},
		"沒有版號卻說是從檔案讀的": func(r *operator.SoftwareReport) {
			for index := range r.Tools[0].Rows {
				if r.Tools[0].Rows[index].State == operator.SoftwareNoVersion {
					r.Tools[0].Rows[index].FromDisk = true
					return
				}
			}
		},
		"沒回報過卻說被另一條 PATH 遮住": func(r *operator.SoftwareReport) {
			for index := range r.Tools[0].Rows {
				if r.Tools[0].Rows[index].State == operator.SoftwareUnreported {
					r.Tools[0].Rows[index].Shadowed = true
					return
				}
			}
		},
		"沒回報過卻給了時刻": func(r *operator.SoftwareReport) {
			for index := range r.Tools[0].Rows {
				if r.Tools[0].Rows[index].State == operator.SoftwareUnreported {
					at := r.EvaluatedAt.Add(-time.Minute)
					r.Tools[0].Rows[index].ObservedAt = &at
					return
				}
			}
		},
		"量測時刻不是 UTC": func(r *operator.SoftwareReport) {
			local := r.EvaluatedAt.Add(-time.Minute).In(other)
			r.Tools[0].Rows[0].MeasuredAt = &local
		},
		"機器名稱裡有終端機跳脫序列": func(r *operator.SoftwareReport) {
			r.Tools[0].Rows[0].DisplayName = "samplehub1\x1b[2J"
		},
		"工具名稱裡有終端機跳脫序列": func(r *operator.SoftwareReport) {
			r.Tools[0].Name = "claude\x1b[2J"
		},
		// —— 版號講的是哪一份 ——
		"那一軸少一種": func(r *operator.SoftwareReport) {
			r.RuntimeStates = r.RuntimeStates[1:]
		},
		// ⚠ 要人動手的那兩種刻意排在最前面，重排過會把它們埋在最多的那一種後面。
		"那一軸的順序被換過": func(r *operator.SoftwareReport) {
			idle := softwareRuntimeCountIndex(r, operator.ToolRuntimeIdle)
			r.RuntimeStates[0], r.RuntimeStates[idle] = r.RuntimeStates[idle], r.RuntimeStates[0]
		},
		"那一軸的格數是負的": func(r *operator.SoftwareReport) {
			r.RuntimeStates[0].Count, r.RuntimeStates[2].Count = -1, 2
		},
		"那一軸的摘要跟逐格數的不一樣": func(r *operator.SoftwareReport) {
			r.RuntimeStates[0].Count, r.RuntimeStates[2].Count = 0, 2
		},
		"那一軸有不認得的狀態": func(r *operator.SoftwareReport) {
			r.RuntimeStates[0].State = "mismatch"
		},
		"那一軸的意思換了一種說法": func(r *operator.SoftwareReport) {
			r.RuntimeStates[0].Meaning += "（大概）"
		},
		"那一軸的下一步換了一種說法": func(r *operator.SoftwareReport) {
			r.RuntimeStates[0].NextStep = "重裝一次就好了。"
		},
		"量的就是跑的那一種被硬塞了一個下一步": func(r *operator.SoftwareReport) {
			for index := range r.RuntimeStates {
				if r.RuntimeStates[index].State == operator.ToolRuntimeSameFile {
					r.RuntimeStates[index].NextStep = "去確認一下。"
					return
				}
			}
		},
		"錯歸因的格數對不上": func(r *operator.SoftwareReport) { r.Misattributed++ },
		// ⚠ 那兩種不是發現。把「我不知道」算進去，機隊上每一格都會變成待辦事項。
		"說不出來的那幾格被算成錯歸因": func(r *operator.SoftwareReport) {
			r.Misattributed = softwareClientRuntimeCells(*r)
		},
		"列上那一軸的標題跟狀態對不上": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeSameFile, func(f *operator.ToolRuntimeFinding) {
				f.Title = operator.ToolRuntimeTitle(operator.ToolRuntimeOtherFile)
			})
		},
		// ⚠⚠ 一格說「這台上沒有」卻回答「版號講的是哪一份」，講的是兩件互相矛盾的事。
		"說沒有卻回答了版號講的是哪一份": func(r *operator.SoftwareReport) {
			for index := range r.Tools[0].Rows {
				if r.Tools[0].Rows[index].State == operator.SoftwareAbsent {
					finding := softwareClientRuntime(operator.ToolRuntimeIdle, "")
					r.Tools[0].Rows[index].Runtime = &finding
					softwareRuntimeCount(r, operator.ToolRuntimeIdle).Count++
					return
				}
			}
		},
		"裝著卻不回答版號講的是哪一份": func(r *operator.SoftwareReport) {
			softwareClientEditRow(r, operator.ToolRuntimeSameFile, func(row *operator.SoftwareRow) {
				row.Runtime = nil
				r.RuntimeStates[2].Count--
			})
		},
		// ⚠ 不講是哪一個檔案的話，讀的人沒辦法知道要去收掉哪一份。
		"說正在跑的是另一個檔案卻不講是哪一個": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeOtherFile, func(f *operator.ToolRuntimeFinding) {
				f.RunningFile = ""
			})
		},
		"說那個檔案不在磁碟上卻不講是哪一個": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeGoneFile, func(f *operator.ToolRuntimeFinding) {
				f.RunningFile = ""
			})
		},
		"說不出跑的是哪一個檔案卻給了一個": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeUnattributed, func(f *operator.ToolRuntimeFinding) {
				f.RunningFile = "/usr/bin/node"
			})
		},
		"沒找到 process 卻給了正在跑的檔案": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeIdle, func(f *operator.ToolRuntimeFinding) {
				f.RunningFile = softwareClientToolFile
			})
		},
		"說量的就是跑的卻不講量的是哪一個檔案": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeSameFile, func(f *operator.ToolRuntimeFinding) {
				f.MeasuredFile = ""
			})
		},
		// ⚠⚠ 兩邊是同一個路徑的話，那句「把另一份收掉」指的就是唯一那一份。
		"說正在跑的是另一個檔案卻兩邊同一個": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeOtherFile, func(f *operator.ToolRuntimeFinding) {
				f.RunningFile = f.MeasuredFile
			})
		},
		"說正在跑的是另一個檔案卻只差一段 ..": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeOtherFile, func(f *operator.ToolRuntimeFinding) {
				f.RunningFile = "/home/example-user/.local/bin/../bin/claude"
			})
		},
		"正在跑的檔案裡有終端機跳脫序列": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeOtherFile, func(f *operator.ToolRuntimeFinding) {
				f.RunningFile = "/opt/claude\x1b[2J"
			})
		},
		"量版號的檔案裡有終端機跳脫序列": func(r *operator.SoftwareReport) {
			softwareClientEditRuntime(r, operator.ToolRuntimeSameFile, func(f *operator.ToolRuntimeFinding) {
				f.MeasuredFile = "/home/example-user/.local/bin/claude\x1b[2J"
			})
		},
	} {
		report := softwareClientReport(now)
		edit(&report)
		if _, err := complianceClientServer(t, report).SoftwareReport(t.Context()); err == nil {
			t.Errorf("%s：自相矛盾的軟體清查被接受了", name)
		}
	}
}
