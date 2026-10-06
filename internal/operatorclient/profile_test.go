package operatorclient

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// profileClientReport 造一份四種狀態與四種套件狀態各有一列的報告，數字自己對得起來。
//
//	openclaw-standard rev 1  沒有人穿，而 rev 2 是更新的       → 已經發佈到更新的版本
//	                         openclaw 2026.5.20              → 指派過也看得到
//	                         而看到的那一台量的是沒在跑的那一份 → 1 格
//	openclaw-standard rev 2  在籍的 m-worn 身上就是它          → 機隊上有機器穿著這一版
//	                         openclaw 2026.6.6               → 沒有指派過但看得到
//	                         3 台看得到，其中 2 台量錯了檔案    → 1 格
//
// ⚠ 兩格的台數刻意不一樣（1 與 2）：整份報告那個數字算的是格子，不是台數。兩格都放 1
// 的話，把它改成加總台數的實作照樣對得起來，這裡就抓不到了。
//
//	edge-standard rev 1      只有已退役的 m-gone 身上還是它     → 只有已退役的機器還是這一版
//	                         edge-tool 1.0.0                 → 指派過但沒有一台回報它
//	lab-standard rev 1       它就是最新的一版，而一台都沒指派    → 發佈了一台都沒指派
//	                         lab-tool 0.1.0                  → 沒有指派過也沒有看到過
func profileClientReport(now time.Time) operator.ProfileReport {
	publishedAt := now.Add(-72 * time.Hour)
	assignedAt := now.Add(-12 * time.Hour)
	lastIntent := now.Add(-24 * time.Hour)

	wearer := func(id, name string, retired bool) operator.ProfileMachineRef {
		return operator.ProfileMachineRef{
			MachineID: id, DisplayName: name, Retired: retired,
			AssignmentID: "assignment-" + id, AssignmentRevision: 1,
			AssignedAt: assignedAt, AssignedBy: "operator@test",
		}
	}
	pkg := func(id, version string, state operator.ProfilePackageState,
		intents, seenOn, seenMisattributedOn int,
	) operator.ProfilePackage {
		out := operator.ProfilePackage{
			PackageID: id, Version: version, State: state,
			Title:               operator.ProfilePackageStateTitle(state),
			Meaning:             operator.ProfilePackageStateMeaning(state),
			NextStep:            operator.ProfilePackageNextStep(state, seenMisattributedOn),
			Intents:             intents,
			SeenOn:              seenOn,
			SeenMisattributedOn: seenMisattributedOn,
		}
		if intents > 0 {
			at := lastIntent
			out.LastAssignedAt = &at
		}
		return out
	}
	row := func(id string, revision int64, state operator.ProfileState,
		machines []operator.ProfileMachineRef, packages []operator.ProfilePackage,
	) operator.ProfileRow {
		out := operator.ProfileRow{
			ProfileID: id, Revision: revision, Digest: "sha256:" + id,
			PublishedAt: publishedAt, PublishedBy: "operator@test",
			State: state, Title: operator.ProfileStateTitle(state),
			Meaning:  operator.ProfileStateMeaning(state),
			NextStep: operator.ProfileStateNextStep(state),
			Machines: machines, Packages: packages,
			Headline: id + " 那一列",
		}
		for _, machine := range machines {
			if machine.Retired {
				out.RetiredOn++
				continue
			}
			out.AssignedOn++
		}
		return out
	}

	report := operator.ProfileReport{
		SchemaVersion: operator.ProfileReportSchemaVersion, EvaluatedAt: now,
		Machines: 3, Wearing: 1, Bare: 2,
		Published: 4, Unassigned: 1, SeenMisattributed: 2,
		Caveat: operator.ProfileReportCaveat,
		Profiles: []operator.ProfileRow{
			row("openclaw-standard", 1, operator.ProfileReplaced, []operator.ProfileMachineRef{},
				[]operator.ProfilePackage{
					pkg("openclaw", "2026.5.20", operator.ProfilePackageAssignedAndSeen, 2, 1, 1),
				}),
			row("openclaw-standard", 2, operator.ProfileInUse,
				[]operator.ProfileMachineRef{wearer("m-worn", "samplehub1", false)},
				[]operator.ProfilePackage{
					pkg("openclaw", "2026.6.6", operator.ProfilePackageSeenNotAssigned, 0, 3, 2),
				}),
			row("edge-standard", 1, operator.ProfileRetiredOnly,
				[]operator.ProfileMachineRef{wearer("m-gone", "old-box", true)},
				[]operator.ProfilePackage{
					pkg("edge-tool", "1.0.0", operator.ProfilePackageAssignedNotSeen, 1, 0, 0),
				}),
			row("lab-standard", 1, operator.ProfileUnassigned, []operator.ProfileMachineRef{},
				[]operator.ProfilePackage{
					pkg("lab-tool", "0.1.0", operator.ProfilePackageNeither, 0, 0, 0),
				}),
		},
	}
	stateCounts := map[operator.ProfileState]int{}
	packageCounts := map[operator.ProfilePackageState]int{}
	for _, profile := range report.Profiles {
		stateCounts[profile.State]++
		for _, item := range profile.Packages {
			packageCounts[item.State]++
		}
	}
	for _, stateValue := range operator.ProfileStates() {
		report.States = append(report.States, operator.ProfileStateCount{
			State: stateValue, Title: operator.ProfileStateTitle(stateValue),
			Count:    stateCounts[stateValue],
			Meaning:  operator.ProfileStateMeaning(stateValue),
			NextStep: operator.ProfileStateNextStep(stateValue),
		})
	}
	for _, stateValue := range operator.ProfilePackageStates() {
		report.PackageStates = append(report.PackageStates, operator.ProfilePackageStateCount{
			State: stateValue, Title: operator.ProfilePackageStateTitle(stateValue),
			Count:    packageCounts[stateValue],
			Meaning:  operator.ProfilePackageStateMeaning(stateValue),
			NextStep: operator.ProfilePackageStateNextStep(stateValue),
		})
	}
	report.Headline = operator.ProfileReportHeadline(report)
	return report
}

func TestTheProfileClientAsksTheCanonicalPath(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	client, asked := recordingReportServer(t, profileClientReport(now))
	report, err := client.ProfileReport(t.Context())
	if err != nil {
		t.Fatalf("一致的發佈與指派對照被拒絕：%v", err)
	}
	if *asked != "/v1/operator/profile-report" {
		t.Fatalf("用戶端問的是 %q", *asked)
	}
	// fixture 要真的把四種狀態與四種套件狀態都放進去，否則下面那一長串 mutation
	// 有一半沒有列可以改。
	states := map[operator.ProfileState]bool{}
	packages := map[operator.ProfilePackageState]bool{}
	for _, row := range report.Profiles {
		states[row.State] = true
		for _, pkg := range row.Packages {
			packages[pkg.State] = true
		}
	}
	for _, stateValue := range operator.ProfileStates() {
		if !states[stateValue] {
			t.Errorf("fixture 沒有 %s 那一列", stateValue)
		}
	}
	for _, stateValue := range operator.ProfilePackageStates() {
		if !packages[stateValue] {
			t.Errorf("fixture 沒有 %s 那一格", stateValue)
		}
	}
	// 「看得到的版號量的是沒在跑的那一份」也要真的有格子，否則下面那幾個 mutation
	// 改的是一份沒有這一軸的報告。
	cells, machines := 0, 0
	for _, row := range report.Profiles {
		for _, pkg := range row.Packages {
			if pkg.SeenMisattributedOn > 0 {
				cells++
				machines += pkg.SeenMisattributedOn
			}
		}
	}
	if cells == 0 {
		t.Fatal("fixture 一格量錯檔案都沒有")
	}
	if cells != report.SeenMisattributed {
		t.Errorf("報告說 %d 格，逐格數出 %d 格", report.SeenMisattributed, cells)
	}
	// 格數跟台數一樣的 fixture 分不出那個數字算的是哪一個。
	if cells == machines {
		t.Errorf("fixture 的格數與台數都是 %d", cells)
	}
}

func profileClientRow(report *operator.ProfileReport, id string, revision int64) *operator.ProfileRow {
	for index := range report.Profiles {
		if report.Profiles[index].ProfileID == id && report.Profiles[index].Revision == revision {
			return &report.Profiles[index]
		}
	}
	return nil
}

func profileClientSaySo(row *operator.ProfileRow, state operator.ProfileState) {
	row.State = state
	row.Title = operator.ProfileStateTitle(state)
	row.Meaning = operator.ProfileStateMeaning(state)
	row.NextStep = operator.ProfileStateNextStep(state)
}

// 這份報告的價值全在於那幾個數字跟那幾句話。一份自相矛盾的回應——把「發佈了一台都
// 沒指派」寫成歷史版本、把兩個獨立的軸攪在一起、把那句「這個 Hub 沒有上游版本來源」
// 拿掉——不是拿來顯示的東西，是拿來拒收的。
func TestTheProfileClientRefusesAReportThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	other := time.FixedZone("CST", 8*3600)
	for name, edit := range map[string]func(*operator.ProfileReport){
		"沒講 schema 版本": func(r *operator.ProfileReport) { r.SchemaVersion++ },
		"沒有讀取時刻":       func(r *operator.ProfileReport) { r.EvaluatedAt = time.Time{} },
		"讀取時刻不是 UTC": func(r *operator.ProfileReport) {
			r.EvaluatedAt = r.EvaluatedAt.In(other)
		},
		"有負數": func(r *operator.ProfileReport) { r.Machines, r.Wearing = -1, -1 },
		"身上有 profile 加沒有不等於分母": func(r *operator.ProfileReport) { r.Bare++ },
		"身上有 profile 的台數對不上": func(r *operator.ProfileReport) {
			r.Wearing, r.Bare = 2, 1
		},
		"發佈的版數對不上":     func(r *operator.ProfileReport) { r.Published++ },
		"一台都沒指派的版數對不上": func(r *operator.ProfileReport) { r.Unassigned = 0 },
		// ⚠⚠ 那句限制被拿掉的話，一格「這個 Hub 沒有指派過這一版，也沒有看到過」
		// 會被讀成「這一版不存在」——而這個 Hub 沒有資格講那句話。
		"那句限制被拿掉了": func(r *operator.ProfileReport) { r.Caveat = "" },
		"那句限制被換了一種說法": func(r *operator.ProfileReport) {
			r.Caveat = "profile 資訊僅供參考。"
		},
		"狀態少一種": func(r *operator.ProfileReport) { r.States = r.States[1:] },
		"狀態順序被換過": func(r *operator.ProfileReport) {
			r.States[0], r.States[1] = r.States[1], r.States[0]
		},
		"摘要說的版數跟逐列數的不一樣": func(r *operator.ProfileReport) {
			r.States[0].Count, r.States[2].Count = 2, 0
		},
		"狀態的說明被改過": func(r *operator.ProfileReport) {
			r.States[0].Meaning = "這一版沒事。"
		},
		"狀態的下一步被改過": func(r *operator.ProfileReport) {
			r.States[3].NextStep = "重新發佈一次。"
		},
		"套件狀態少一種": func(r *operator.ProfileReport) { r.PackageStates = r.PackageStates[1:] },
		"套件狀態順序被換過": func(r *operator.ProfileReport) {
			r.PackageStates[0], r.PackageStates[3] = r.PackageStates[3], r.PackageStates[0]
		},
		"摘要說的格數跟逐格數的不一樣": func(r *operator.ProfileReport) {
			r.PackageStates[0].Count, r.PackageStates[3].Count = 2, 0
		},
		"套件狀態的說明被改過": func(r *operator.ProfileReport) {
			r.PackageStates[3].Meaning = "這個套件沒事。"
		},
		// ⚠⚠ 這是這份報告存在的理由那一列。講成歷史版本的話，它會被當成舊
		// revision 捲過去——而摘要在這一版裡也一起圓過去了，只剩下那個算得出來
		// 的判準抓得到。
		"一台都沒指派被寫成已經發佈到更新的版本": func(r *operator.ProfileReport) {
			profileClientSaySo(profileClientRow(r, "lab-standard", 1), operator.ProfileReplaced)
			r.Unassigned = 0
			for index := range r.States {
				switch r.States[index].State {
				case operator.ProfileReplaced:
					r.States[index].Count = 2
				case operator.ProfileUnassigned:
					r.States[index].Count = 0
				}
			}
		},
		// ⚠ 只有退役的機器還穿著它，跟機隊上有機器穿著它，要做的事不一樣。
		"只有退役的還穿著它被寫成機隊上有機器穿著它": func(r *operator.ProfileReport) {
			profileClientSaySo(profileClientRow(r, "edge-standard", 1), operator.ProfileInUse)
			for index := range r.States {
				switch r.States[index].State {
				case operator.ProfileInUse:
					r.States[index].Count = 2
				case operator.ProfileRetiredOnly:
					r.States[index].Count = 0
				}
			}
		},
		"退役的那台被算進機隊": func(r *operator.ProfileReport) {
			profileClientRow(r, "edge-standard", 1).Machines[0].Retired = false
		},
		"機隊上的台數對不上": func(r *operator.ProfileReport) {
			profileClientRow(r, "openclaw-standard", 2).AssignedOn++
		},
		"已退役的台數對不上": func(r *operator.ProfileReport) {
			profileClientRow(r, "edge-standard", 1).RetiredOn = 0
		},
		"穿著它的台數比分母多": func(r *operator.ProfileReport) {
			r.Machines, r.Wearing, r.Bare = 0, 0, 0
		},
		"這個版本不認得的狀態": func(r *operator.ProfileReport) {
			r.Profiles[0].State = operator.ProfileState("probably_fine")
		},
		"這個版本不認得的套件狀態": func(r *operator.ProfileReport) {
			r.Profiles[0].Packages[0].State = operator.ProfilePackageState("probably_fine")
		},
		"一列的句子跟狀態不一樣": func(r *operator.ProfileReport) {
			r.Profiles[0].Title = "看起來沒事"
		},
		"一格的句子跟套件狀態不一樣": func(r *operator.ProfileReport) {
			r.Profiles[0].Packages[0].NextStep = "重開機。"
		},
		// ⚠⚠ 看得到的版號量的是沒在跑的那一份，這一格卻照狀態那一句收工：畫面上會是一格
		// 指派過也看得到、沒有下一步的套件，而它其實只是在硬碟上放著。
		"量錯檔案的那一格照狀態那一句收工": func(r *operator.ProfileReport) {
			pkg := &profileClientRow(r, "openclaw-standard", 1).Packages[0]
			pkg.NextStep = operator.ProfilePackageStateNextStep(pkg.State)
		},
		"沒有量錯檔案的一格叫人去核對執行檔": func(r *operator.ProfileReport) {
			pkg := &profileClientRow(r, "edge-standard", 1).Packages[0]
			pkg.NextStep = operator.ProfilePackageNextStep(pkg.State, 1)
		},
		"有兩列同一版 profile": func(r *operator.ProfileReport) {
			r.Profiles[1].Revision = r.Profiles[0].Revision
		},
		"revision 是 0": func(r *operator.ProfileReport) { r.Profiles[0].Revision = 0 },
		"一列沒有 digest":  func(r *operator.ProfileReport) { r.Profiles[0].Digest = "" },
		"一列沒有 profile 名字": func(r *operator.ProfileReport) {
			r.Profiles[0].ProfileID = ""
		},
		"沒有發佈時刻": func(r *operator.ProfileReport) {
			r.Profiles[0].PublishedAt = time.Time{}
		},
		"發佈時刻不是 UTC": func(r *operator.ProfileReport) {
			r.Profiles[0].PublishedAt = r.Profiles[0].PublishedAt.In(other)
		},
		"一列沒有那一行字": func(r *operator.ProfileReport) { r.Profiles[0].Headline = "" },
		"一列有負數": func(r *operator.ProfileReport) {
			r.Profiles[0].AssignedOn, r.Profiles[0].RetiredOn = -1, 1
		},
		"有兩台同一部機器穿著它": func(r *operator.ProfileReport) {
			row := profileClientRow(r, "openclaw-standard", 2)
			row.Machines = append(row.Machines, row.Machines[0])
			row.AssignedOn++
		},
		"穿著它的機器沒有指派 revision": func(r *operator.ProfileReport) {
			profileClientRow(r, "openclaw-standard", 2).Machines[0].AssignmentRevision = 0
		},
		"穿著它的機器沒有指派時刻": func(r *operator.ProfileReport) {
			profileClientRow(r, "openclaw-standard", 2).Machines[0].AssignedAt = time.Time{}
		},
		"指派時刻不是 UTC": func(r *operator.ProfileReport) {
			row := profileClientRow(r, "openclaw-standard", 2)
			row.Machines[0].AssignedAt = row.Machines[0].AssignedAt.In(other)
		},
		"穿著它的機器沒有帳本號碼": func(r *operator.ProfileReport) {
			profileClientRow(r, "openclaw-standard", 2).Machines[0].AssignmentID = ""
		},
		// ⚠⚠ 兩個軸攪在一起的那四種。每一種在畫面上都只差一句話，而那一句話決定
		// 操作員是去看工作單，還是去找一條指派以外的安裝路徑。
		"說沒有指派過卻帶著指派次數": func(r *operator.ProfileReport) {
			profileClientRow(r, "lab-standard", 1).Packages[0].Intents = 3
		},
		"說指派過卻說指派過 0 次": func(r *operator.ProfileReport) {
			profileClientRow(r, "edge-standard", 1).Packages[0].Intents = 0
		},
		// ⚠ 量錯檔案那個數字要一起歸零：留著的話，先被「量錯檔案的台數比看得到的台數
		// 多」那一關擋下來，而這一格要釘的是兩個軸與狀態的對照。
		"說看得到卻說 0 台看得到": func(r *operator.ProfileReport) {
			pkg := &profileClientRow(r, "openclaw-standard", 2).Packages[0]
			pkg.SeenOn, pkg.SeenMisattributedOn = 0, 0
			pkg.NextStep = operator.ProfilePackageNextStep(pkg.State, 0)
			r.SeenMisattributed = 1
		},
		"說沒有看到過卻說有機器看得到": func(r *operator.ProfileReport) {
			profileClientRow(r, "lab-standard", 1).Packages[0].SeenOn = 1
		},
		"指派過卻沒有最後一次指派的時刻": func(r *operator.ProfileReport) {
			profileClientRow(r, "edge-standard", 1).Packages[0].LastAssignedAt = nil
		},
		"沒有指派過卻帶著最後一次指派的時刻": func(r *operator.ProfileReport) {
			at := r.EvaluatedAt.Add(-time.Hour)
			profileClientRow(r, "lab-standard", 1).Packages[0].LastAssignedAt = &at
		},
		"最後一次指派的時刻不是 UTC": func(r *operator.ProfileReport) {
			at := r.EvaluatedAt.Add(-time.Hour).In(other)
			profileClientRow(r, "edge-standard", 1).Packages[0].LastAssignedAt = &at
		},
		"量錯檔案的格數對不上": func(r *operator.ProfileReport) { r.SeenMisattributed = 1 },
		// ⚠⚠ 這一格是狀態那一軸抓不到的：SeenOn 還是 0，所以「沒有指派過也沒有看到過」
		// 照樣成立，而那一格會在畫面上寫成「0 台看得到，其中 1 台量的是沒在跑的那一份」。
		// 整份的格數一起加一，讓這個案例只剩下「它不是 SeenOn 的子集」那一關能擋。
		"量錯檔案的台數比看得到的台數多": func(r *operator.ProfileReport) {
			pkg := &profileClientRow(r, "lab-standard", 1).Packages[0]
			pkg.SeenMisattributedOn = 1
			pkg.NextStep = operator.ProfilePackageNextStep(pkg.State, 1)
			r.SeenMisattributed++
		},
		"量錯檔案的台數是負數": func(r *operator.ProfileReport) {
			pkg := &r.Profiles[0].Packages[0]
			pkg.SeenMisattributedOn = -1
			pkg.NextStep = operator.ProfilePackageNextStep(pkg.State, -1)
			// 負數不會被逐格重數；總數一起扣一，讓這個案例只剩負數 guard 能擋。
			r.SeenMisattributed--
		},
		"整份報告的量錯檔案格數是負數": func(r *operator.ProfileReport) {
			r.SeenMisattributed = -1
		},
		"看得到的台數比分母多": func(r *operator.ProfileReport) {
			profileClientRow(r, "openclaw-standard", 2).Packages[0].SeenOn = r.Machines + 1
		},
		"有兩列同一個套件版本": func(r *operator.ProfileReport) {
			row := profileClientRow(r, "lab-standard", 1)
			row.Packages = append(row.Packages, row.Packages[0])
			r.PackageStates[3].Count = 2
		},
		"套件沒有版號": func(r *operator.ProfileReport) {
			r.Profiles[0].Packages[0].Version = ""
		},
		"profile 名字裡有終端機跳脫序列": func(r *operator.ProfileReport) {
			r.Profiles[0].ProfileID = "openclaw-standard\x1b[2J"
		},
		"套件名字裡有終端機跳脫序列": func(r *operator.ProfileReport) {
			r.Profiles[0].Packages[0].PackageID = "openclaw\x1b[2J"
		},
		"機器名稱裡有終端機跳脫序列": func(r *operator.ProfileReport) {
			profileClientRow(r, "openclaw-standard", 2).Machines[0].DisplayName = "samplehub1\x1b[2J"
		},
	} {
		report := profileClientReport(now)
		edit(&report)
		if _, err := complianceClientServer(t, report).ProfileReport(t.Context()); err == nil {
			t.Errorf("%s：自相矛盾的發佈與指派對照被接受了", name)
		}
	}
}
