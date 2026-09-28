package operator

import (
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

func timelineOf(t *testing.T, service *Service, machineID string, now time.Time, days int) MachineTimelineResult {
	t.Helper()
	result, err := service.MachineTimeline(MachineTimelineRequest{MachineID: machineID, Days: days}, now)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func entriesOfKind(result MachineTimelineResult, kind MachineTimelineKind) []MachineTimelineEntry {
	out := []MachineTimelineEntry{}
	for _, entry := range result.Entries {
		if entry.Kind == kind {
			out = append(out, entry)
		}
	}
	return out
}

func sourceRead(t *testing.T, result MachineTimelineResult, source MachineTimelineSource) MachineTimelineSourceRead {
	t.Helper()
	for _, read := range result.Sources {
		if read.Source == source {
			return read
		}
	}
	t.Fatalf("時間軸沒有交代來源 %s：%+v", source, result.Sources)
	return MachineTimelineSourceRead{}
}

// 一台機器的故事目前散在四個地方。時間軸的價值就是把它們排成一條，所以四個來源
// 每一個都要有自己的列，而每一列都要說得出它是哪一個來源。
func TestTheTimelineTellsOneStoryOutOfEverySourceTheHubWroteDown(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "timeline-all", enabledCheckin())
	if err := st.RecordStateTransition(machineID, state.Online, "第一次報到", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	startDiagnosticJob(t, service, machineID)

	result := timelineOf(t, service, machineID, now, 0)
	if result.SchemaVersion != MachineTimelineSchemaVersion || result.MachineID != machineID ||
		result.DisplayName != "timeline-all" || result.Window.Days != DefaultMachineTimelineDays {
		t.Fatalf("result=%+v", result)
	}
	for _, source := range machineTimelineSourceOrder {
		read := sourceRead(t, result, source)
		if read.Count == 0 {
			t.Errorf("來源 %s 一列都沒有：%+v", source, result.Entries)
		}
		if read.Label == "" || read.Evidence == "" {
			t.Errorf("來源 %s 沒有說自己是什麼：%+v", source, read)
		}
	}
	counted := 0
	for _, read := range result.Sources {
		counted += read.Count
	}
	if counted != result.Total || result.Total != len(result.Entries) {
		t.Fatalf("逐來源 %d、總數 %d、列數 %d 對不起來", counted, result.Total, len(result.Entries))
	}
}

// 時間軸唯一的排列規則就是時間，由新到舊。排錯的時間軸比沒有時間軸更糟：它會被
// 拿來論證因果。
func TestTheTimelineIsOrderedNewestFirst(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "timeline-order", enabledCheckin())
	for index, moment := range []time.Time{now.Add(-5 * time.Hour), now.Add(-3 * time.Hour), now.Add(-time.Hour)} {
		machineState := state.Online
		if index == 1 {
			machineState = state.Degraded
		}
		if err := st.RecordStateTransition(machineID, machineState, "測試", moment); err != nil {
			t.Fatal(err)
		}
	}
	result := timelineOf(t, service, machineID, now, 0)
	if len(result.Entries) < 3 {
		t.Fatalf("列數 %d", len(result.Entries))
	}
	for index := 1; index < len(result.Entries); index++ {
		if result.Entries[index].At.After(result.Entries[index-1].At) {
			t.Fatalf("第 %d 列比前一列新：%v → %v",
				index, result.Entries[index-1].At, result.Entries[index].At)
		}
	}
}

// 窗外的事情不算在窗裡。一個把更早的列也算進來的「最近 7 天」會讓兩次讀取對不
// 起來，而且沒有任何一個數字看得出來。
func TestTheTimelineLeavesOutWhatHappenedBeforeTheWindow(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "timeline-window", enabledCheckin())
	if err := st.RecordStateTransition(machineID, state.Online, "窗外", now.Add(-20*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordStateTransition(machineID, state.Degraded, "窗內", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	narrow := timelineOf(t, service, machineID, now, 7)
	states := entriesOfKind(narrow, MachineTimelineStateEntered)
	if len(states) != 1 || states[0].Detail != "窗內" {
		t.Fatalf("7 天窗讀到 %+v", states)
	}
	if !narrow.Window.From.Equal(narrow.Window.To.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("window=%+v", narrow.Window)
	}
	wide := timelineOf(t, service, machineID, now, 30)
	if len(entriesOfKind(wide, MachineTimelineStateEntered)) != 2 {
		t.Fatalf("30 天窗讀到 %+v", entriesOfKind(wide, MachineTimelineStateEntered))
	}
}

// 每一個來源各自說它在這段期間有沒有讀完。一個不說這件事的總數，會被當成「這段
// 期間就只發生了這些」——而那正是時間軸要拿來論證的那句話。
func TestEverySourceSaysWhetherItReadTheWholeWindow(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "timeline-complete", enabledCheckin())
	if err := st.RecordStateTransition(machineID, state.Online, "夠短", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	result := timelineOf(t, service, machineID, now, 0)
	if !result.Complete {
		t.Fatalf("這麼小的機隊不該讀不完：%+v", result.Sources)
	}
	for _, read := range result.Sources {
		if !read.Complete || read.NextStep != "" {
			t.Errorf("來源 %s 說沒讀完：%+v", read.Source, read)
		}
	}
}

// 讀取器只給了一頁而最舊那一列還在窗裡面，就表示窗的前半段還有沒讀到的東西。
// 這時候總數會少算，而少算的總數必須自己說出來。
func TestATruncatedSourceThatCannotReachTheWindowStartSaysSo(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	oldestInsideWindow := from.Add(24 * time.Hour)
	if machineTimelineSourceComplete(true, oldestInsideWindow, from) {
		t.Error("讀不到窗開始的截斷讀取被當成完整")
	}
	if !machineTimelineSourceComplete(true, from.Add(-time.Hour), from) {
		t.Error("最舊一列已經在窗之前，這個來源其實是完整的")
	}
	if !machineTimelineSourceComplete(false, time.Time{}, from) {
		t.Error("沒有截斷就是完整")
	}
	if machineTimelineSourceComplete(true, time.Time{}, from) {
		t.Error("截斷了又說不出最舊一列是什麼時候，不能算完整")
	}
	if got := MachineTimelineIncompleteNextStep(MachineTimelineSourceJob); !strings.Contains(got, "工作單") {
		t.Errorf("下一步沒說是哪一個來源讀不完：%q", got)
	}
}

// 純函式已經另有測試；這裡防的是 service 把狀態來源的 Complete 與 NextStep 接錯。
func TestTheTimelineSaysWhenStateHistoryDidNotReadTheWholeWindow(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "timeline-incomplete-state", enabledCheckin())
	for index := 0; index < store.DetailHistoryLimit+1; index++ {
		machineState := state.Online
		if index%2 == 1 {
			machineState = state.Degraded
		}
		at := now.Add(-time.Duration(store.DetailHistoryLimit+1-index) * time.Minute)
		if err := st.RecordStateTransition(machineID, machineState, "截斷測試", at); err != nil {
			t.Fatal(err)
		}
	}

	result := timelineOf(t, service, machineID, now, 0)
	read := sourceRead(t, result, MachineTimelineSourceState)
	wantNextStep := MachineTimelineIncompleteNextStep(MachineTimelineSourceState)
	if read.Complete || read.NextStep != wantNextStep || result.Complete {
		t.Fatalf("狀態來源讀到 Count=%d、Complete=%t、NextStep=%q；整份 Complete=%t",
			read.Count, read.Complete, read.NextStep, result.Complete)
	}
}

// 每一列都必須說得出它自己的來源，而且下鑽連結只在真的有證據頁的時候才給。給一
// 個連不到東西的連結，比不給連結更糟。
func TestEveryEntryNamesItsSourceAndOnlyLinksWhereEvidenceExists(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "timeline-links", enabledCheckin())
	if err := st.RecordStateTransition(machineID, state.Online, "測試", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	startDiagnosticJob(t, service, machineID)
	result := timelineOf(t, service, machineID, now, 0)
	for _, entry := range result.Entries {
		if machineTimelineSourceOf[entry.Kind] != entry.Source {
			t.Errorf("列 %s 說自己是 %s，但那種列屬於 %s",
				entry.Kind, entry.Source, machineTimelineSourceOf[entry.Kind])
		}
		if entry.Summary == "" {
			t.Errorf("列 %s 沒有說發生了什麼", entry.Kind)
		}
		switch entry.Kind {
		case MachineTimelineJobOpened, MachineTimelineJobFinished:
			if !strings.HasPrefix(entry.Href, "/jobs/") {
				t.Errorf("工作單的列沒有指回那張工作單：%q", entry.Href)
			}
		case MachineTimelineOperatorAction:
			if !strings.HasPrefix(entry.Href, "/audit?") || !strings.Contains(entry.Href, "machine_id=") {
				t.Errorf("操作員動作的列沒有指回這台的稽核：%q", entry.Href)
			}
		default:
			if entry.Href != "" {
				t.Errorf("列 %s 給了一個沒有證據頁的連結：%q", entry.Kind, entry.Href)
			}
		}
	}
}

// 操作員動作那一列指向的是「這台機器的這個動作」，不是整本稽核。落在一頁要自己
// 再篩一次的地方，等於沒有下鑽。
func TestAnOperatorActionLinksToExactlyTheRowsBehindIt(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "timeline-drill", enabledCheckin())
	startDiagnosticJob(t, service, machineID)
	result := timelineOf(t, service, machineID, now, 0)
	actions := entriesOfKind(result, MachineTimelineOperatorAction)
	if len(actions) == 0 {
		t.Fatal("開了一張診斷工作單卻沒有留下操作員動作")
	}
	for _, entry := range actions {
		if !strings.Contains(entry.Href, "action="+entry.Summary) {
			t.Errorf("動作 %q 的連結沒有篩到那個動作：%q", entry.Summary, entry.Href)
		}
	}
	unknown := machineTimelineActionHref(machineID, "not-an-action")
	if strings.Contains(unknown, "action=") {
		t.Errorf("認不得的動作被拿去當篩選條件：%q", unknown)
	}
}

// 稽核的時間篩選只收整秒。窗的兩端跟稽核真的讀的期間必須是同一個東西，而且評估的
// 那一刻一定要在窗裡面。
func TestTheWindowIsSomethingTheAuditReaderCanActuallyAccept(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "timeline-second", enabledCheckin())
	result := timelineOf(t, service, machineID, now.Add(123*time.Millisecond), 0)
	if result.Window.From.Nanosecond() != 0 || result.Window.To.Nanosecond() != 0 {
		t.Fatalf("窗不是整秒：%+v", result.Window)
	}
	if result.Window.To.Before(result.EvaluatedAt) {
		t.Fatalf("評估時間 %v 落在窗 %v 外面", result.EvaluatedAt, result.Window.To)
	}
}

// 一列沒有時間的稽核不在任何一段期間裡：時間篩選本身就讀不到它。時間軸要說的
// 數字因此只能是它真的排上去的那些——來源說的列數與畫面上的列數必須一樣，否則
// 「這段期間只發生了這些」就是一句沒有根據的話。
func TestARowTheHubCannotPlaceInTimeIsNotCountedAsIfItWere(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "timeline-unplaced", enabledCheckin())
	startDiagnosticJob(t, service, machineID)
	before := sourceRead(t, timelineOf(t, service, machineID, now, 0), MachineTimelineSourceAction)
	if before.Count == 0 {
		t.Fatal("帳本上沒有操作員動作，這個測試測不到東西")
	}
	if _, err := st.DB().Exec(`UPDATE audit_log SET at = 'not-a-time' WHERE machine_id = ?`,
		machineID); err != nil {
		t.Fatal(err)
	}
	result := timelineOf(t, service, machineID, now, 0)
	read := sourceRead(t, result, MachineTimelineSourceAction)
	if read.Count != 0 {
		t.Fatalf("排不上時間軸的列還是被算進來了：%+v", read)
	}
	if entries := len(entriesOfKind(result, MachineTimelineOperatorAction)); entries != read.Count {
		t.Fatalf("操作員動作列 %d 筆，來源說 %d 筆", entries, read.Count)
	}
	if !read.Complete {
		t.Errorf("讀完了卻說沒讀完：%+v", read)
	}
}

// 範圍是有邊界的。沒有邊界的範圍會讓一次讀取掃過整本帳本，而那一次讀取會擋住
// 別人。
func TestTheTimelineRefusesAWindowItCannotStandBehind(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "timeline-bounds", enabledCheckin())
	for _, days := range []int{-1, 0 - 1, MaxMachineTimelineDays + 1} {
		if _, err := service.MachineTimeline(MachineTimelineRequest{MachineID: machineID, Days: days}, now); err == nil {
			t.Errorf("days=%d 被接受了", days)
		}
	}
	if _, err := service.MachineTimeline(MachineTimelineRequest{Days: 7}, now); err == nil {
		t.Error("沒有機器也給了時間軸")
	}
	if _, err := service.MachineTimeline(MachineTimelineRequest{MachineID: machineID}, time.Time{}); err == nil {
		t.Error("沒有評估時間也給了時間軸")
	}
	normalized, err := NormalizeMachineTimelineRequest(MachineTimelineRequest{MachineID: machineID})
	if err != nil || normalized.Days != DefaultMachineTimelineDays {
		t.Fatalf("normalized=%+v err=%v", normalized, err)
	}
}

// 時間軸講的工作單開單時刻，必須就是工作單頁講的那一個。自己重查一次就會漂，而
// 漂掉的樣子是兩頁對同一件事講不同的時間。收尾時刻由
// TestTheTimelineSaysTheSameOutcomeTheJobPageSays 負責。
func TestTheTimelineRepeatsWhatTheJobPageAlreadySays(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "timeline-agree", enabledCheckin())
	startDiagnosticJob(t, service, machineID)
	jobs, err := service.ListJobs(JobListRequest{MachineID: machineID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) == 0 {
		t.Fatal("沒有工作單")
	}
	result := timelineOf(t, service, machineID, now, 0)
	opened := entriesOfKind(result, MachineTimelineJobOpened)
	if len(opened) != len(jobs.Items) {
		t.Fatalf("工作單頁 %d 張，時間軸 %d 列", len(jobs.Items), len(opened))
	}
	for _, job := range jobs.Items {
		found := false
		for _, entry := range opened {
			if entry.Href == "/jobs/"+job.JobID {
				found = true
				if !entry.At.Equal(job.CreatedAt.UTC()) {
					t.Errorf("工作單 %s 開單時間：工作單頁 %v，時間軸 %v", job.JobID, job.CreatedAt, entry.At)
				}
			}
		}
		if !found {
			t.Errorf("時間軸漏了工作單 %s", job.JobID)
		}
	}
}

// 工作單頁與時間軸必須用同一個收尾時刻，也必須把每一個終態翻成同一句話；時間軸
// 沒有保留 raw state，所以這張表刻意獨立寫出操作員最後會看到的標籤。
func TestTheTimelineSaysTheSameOutcomeTheJobPageSays(t *testing.T) {
	tests := []struct {
		state deploy.JobState
		label string
		end   func(*testing.T, *store.Store, string, string, time.Time)
	}{
		{deploy.Succeeded, "成功", func(t *testing.T, st *store.Store, machineID, jobID string, terminalAt time.Time) {
			token, err := st.ClaimJob(jobID, machineID, terminalAt.Add(-4*time.Second), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			for index, event := range []deploy.Event{deploy.Start, deploy.FinishWork} {
				if _, err := st.AdvanceJobByAgent(jobID, machineID, token, event,
					terminalAt.Add(time.Duration(index-3)*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.RecordVerification(jobID, machineID, token, "timeline-outcome",
				"true", 0, "passed", "", true, terminalAt.Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			if state, err := st.MarkSucceededIfVerified(jobID, terminalAt); err != nil || state != deploy.Succeeded {
				t.Fatalf("mark succeeded: state=%q err=%v", state, err)
			}
		}},
		{deploy.Failed, "失敗", func(t *testing.T, st *store.Store, _, jobID string, terminalAt time.Time) {
			if err := st.FailJob(jobID, false, terminalAt); err != nil {
				t.Fatal(err)
			}
		}},
		{deploy.Rejected, "被拒絕", func(t *testing.T, st *store.Store, machineID, jobID string, terminalAt time.Time) {
			token, err := st.ClaimJob(jobID, machineID, terminalAt.Add(-time.Second), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AdvanceJobByAgent(jobID, machineID, token, deploy.Reject, terminalAt); err != nil {
				t.Fatal(err)
			}
		}},
		{deploy.LeaseExpired, "租約過期", func(t *testing.T, st *store.Store, machineID, jobID string, terminalAt time.Time) {
			if _, err := st.ClaimJob(jobID, machineID, terminalAt.Add(-time.Second), time.Hour); err != nil {
				t.Fatal(err)
			}
			if _, err := st.AdvanceJobByHub(jobID, deploy.LeaseLost, terminalAt); err != nil {
				t.Fatal(err)
			}
		}},
		{deploy.ManualIntervention, "需要人介入", func(t *testing.T, st *store.Store, machineID, jobID string, terminalAt time.Time) {
			original, err := st.JobForMachine(jobID, machineID)
			if err != nil {
				t.Fatal(err)
			}
			jobID, err = st.CreateJob(machineID, original.DesiredID, original.Revision,
				store.NewJob{Irreversible: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.FailJob(jobID, true, terminalAt); err != nil {
				t.Fatal(err)
			}
		}},
	}

	labels := make(map[deploy.JobState]string, len(tests))
	for _, test := range tests {
		labels[test.state] = test.label
	}
	for _, state := range deploy.AllJobStates {
		if deploy.IsTerminal(state) && labels[state] == "" {
			t.Errorf("終態 %q 沒有具名的時間軸標籤", state)
		}
	}

	for _, test := range tests {
		t.Run(string(test.state), func(t *testing.T) {
			service, st, machineID, now := machineActionsFixture(t, "timeline-outcome-"+string(test.state), enabledCheckin())
			startDiagnosticJob(t, service, machineID)
			jobs, err := service.ListJobs(JobListRequest{MachineID: machineID}, now)
			if err != nil || len(jobs.Items) != 1 {
				t.Fatalf("initial jobs=%+v err=%v", jobs.Items, err)
			}
			jobID := jobs.Items[0].JobID
			terminalAt := now.Add(5 * time.Second)
			test.end(t, st, machineID, jobID, terminalAt)

			jobs, err = service.ListJobs(JobListRequest{MachineID: machineID}, terminalAt.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			var job *JobSummary
			for index := range jobs.Items {
				if jobs.Items[index].State == test.state {
					job = &jobs.Items[index]
				}
			}
			if job == nil || job.TerminalAt == nil {
				t.Fatalf("工作單頁沒有終態 %q 與收尾時刻：%+v", test.state, jobs.Items)
			}

			result := timelineOf(t, service, machineID, terminalAt.Add(time.Second), 0)
			finished := entriesOfKind(result, MachineTimelineJobFinished)
			if len(finished) != 1 {
				t.Fatalf("終態 %q 的收尾列有 %d 筆：%+v", test.state, len(finished), finished)
			}
			entry := finished[0]
			if !entry.At.Equal(job.TerminalAt.UTC()) {
				t.Errorf("終態 %q 收尾時間：工作單頁 %v，時間軸 %v", test.state, job.TerminalAt, entry.At)
			}
			if want := "工作單" + test.label; entry.Summary != want {
				t.Errorf("終態 %q 摘要=%q want=%q", test.state, entry.Summary, want)
			}
			if want := "/jobs/" + job.JobID; entry.Href != want {
				t.Errorf("終態 %q 連結=%q want=%q", test.state, entry.Href, want)
			}
		})
	}
}

// 匯出檔裡的列數就是畫面上的列數。一份少了幾列的匯出打開之後看起來跟完整的一模
// 一樣，而它會被當成完整的引用。
func TestTheTimelineExportCarriesEveryRowTheScreenShows(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "timeline-export", enabledCheckin())
	if err := st.RecordStateTransition(machineID, state.Online, "第一次報到", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	startDiagnosticJob(t, service, machineID)
	result := timelineOf(t, service, machineID, now, 0)
	doc := MachineTimelineCSV(result)
	if len(doc.Rows) != result.Total {
		t.Fatalf("畫面 %d 列，匯出 %d 列", result.Total, len(doc.Rows))
	}
	records := parseExport(t, mustReportCSV(t, doc))
	if len(records) != result.Total+1 {
		t.Fatalf("匯出檔 %d 列（含標題），時間軸 %d 列", len(records), result.Total)
	}
	for index, entry := range result.Entries {
		row := records[index+1]
		if row[0] != ReportCSVTime(entry.At) || row[2] != string(entry.Kind) || row[3] != entry.Summary {
			t.Fatalf("第 %d 列匯出成 %v，時間軸說 %+v", index+1, row, entry)
		}
	}
}

// 檔名帶的是機器代號，不是顯示名稱。顯示名稱是上游文字，出現在檔名裡就會出現在
// 別人的檔案系統裡。
func TestTheTimelineExportIsNamedAfterTheMachineIDNotItsDisplayName(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "報告 匯出/測試", enabledCheckin())
	result := timelineOf(t, service, machineID, now, 0)
	doc := MachineTimelineCSV(result)
	if doc.Filename != "ai-intune-timeline-"+machineID+".csv" {
		t.Fatalf("檔名=%q", doc.Filename)
	}
	if _, err := ReportCSV(doc); err != nil {
		t.Fatalf("匯出契約拒絕了這份檔案：%v", err)
	}
	if MachineTimelineExportPath(machineID) != "/machines/"+machineID+"/timeline.csv" ||
		MachineTimelinePath(machineID) != "/machines/"+machineID+"/timeline" {
		t.Fatalf("路徑=%q %q", MachineTimelinePath(machineID), MachineTimelineExportPath(machineID))
	}
}

var pinnedTimelineSourceLabels = map[MachineTimelineSource]string{
	MachineTimelineSourceRegistry: "名冊",
	MachineTimelineSourceState:    "健康判定",
	MachineTimelineSourceJob:      "工作單",
	MachineTimelineSourceAction:   "操作員動作",
	// 不認得的來源刻意原樣回傳 token，不是漏寫。
	MachineTimelineSource("nope"): "nope",
}

var pinnedTimelineSourceEvidence = map[MachineTimelineSource]string{
	MachineTimelineSourceRegistry: "名冊自己記下的兩個時刻：進名冊與退役。",
	MachineTimelineSourceState:    "Hub 自己判定這台進入哪一個狀態，以及判定的理由。",
	MachineTimelineSourceJob:      "派給這台的工作單什麼時候開、什麼時候收尾。",
	MachineTimelineSourceAction:   "有人透過 Hub 對這台做過什麼。",
	MachineTimelineSource("nope"): "",
}

var pinnedTimelineSourceIncompleteNextSteps = map[MachineTimelineSource]string{
	MachineTimelineSourceRegistry: "把範圍縮短再看一次，名冊才讀得完這段期間。",
	MachineTimelineSourceState:    "把範圍縮短再看一次，健康判定才讀得完這段期間。",
	MachineTimelineSourceJob:      "把範圍縮短再看一次，工作單才讀得完這段期間。",
	MachineTimelineSourceAction:   "把範圍縮短再看一次，操作員動作才讀得完這段期間。",
}

func TestTheTimelineSourceVocabularySaysTheseExactWords(t *testing.T) {
	t.Run("來源名字", func(t *testing.T) {
		for source, want := range pinnedTimelineSourceLabels {
			got := MachineTimelineSourceLabel(source)
			if got != want {
				t.Errorf("來源 %q 的名字實際是 %q，期望是 %q", source, got, want)
			}
		}
	})

	t.Run("來源證據說明", func(t *testing.T) {
		for source, want := range pinnedTimelineSourceEvidence {
			got := MachineTimelineSourceEvidence(source)
			if got != want {
				t.Errorf("來源 %q 的證據說明實際是 %q，期望是 %q", source, got, want)
			}
		}
	})

	t.Run("讀不完的下一步", func(t *testing.T) {
		for source, want := range pinnedTimelineSourceIncompleteNextSteps {
			got := MachineTimelineIncompleteNextStep(source)
			if got != want {
				t.Errorf("來源 %q 讀不完的下一步實際是 %q，期望是 %q", source, got, want)
			}
		}
	})

	for _, source := range MachineTimelineSources() {
		_, hasLabel := pinnedTimelineSourceLabels[source]
		_, hasEvidence := pinnedTimelineSourceEvidence[source]
		_, hasNextStep := pinnedTimelineSourceIncompleteNextSteps[source]
		if !hasLabel || !hasEvidence || !hasNextStep {
			t.Errorf("時間軸多了一個來源，但沒有人逐字讀過它對操作員說的話：實際來源 %q，期望三張詞彙表都有它", source)
		}
	}
}

func TestEveryTimelineSourceReadCarriesThePinnedWords(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "timeline-vocabulary", enabledCheckin())
	result := timelineOf(t, service, machineID, now, 0)
	for _, read := range result.Sources {
		read := read
		t.Run(string(read.Source), func(t *testing.T) {
			wantLabel, hasLabel := pinnedTimelineSourceLabels[read.Source]
			wantEvidence, hasEvidence := pinnedTimelineSourceEvidence[read.Source]
			if !hasLabel || !hasEvidence {
				t.Errorf("時間軸來源實際是 %q，期望是已逐字釘住的來源", read.Source)
				return
			}
			if read.Label != wantLabel {
				t.Errorf("來源 %q 的名字實際是 %q，期望是 %q", read.Source, read.Label, wantLabel)
			}
			if read.Evidence != wantEvidence {
				t.Errorf("來源 %q 的證據說明實際是 %q，期望是 %q", read.Source, read.Evidence, wantEvidence)
			}
		})
	}
}

func TestTheTimelineActionDetailSaysTheseExactWords(t *testing.T) {
	ok := store.AuditOutcomeOK
	failed := store.AuditOutcomeFailed
	tests := []struct {
		name  string
		event AuditEvent
		want  string
	}{
		{
			name: "結果與歸因都有",
			event: AuditEvent{
				Outcome: &ok,
				WhoNode: auditWhoPtr("operator-laptop"),
				WhoUser: auditWhoPtr("owner@example.com"),
			},
			want: "ok，operator-laptop（owner@example.com）",
		},
		{
			name: "結果沒記錄但歸因還在",
			event: AuditEvent{
				Outcome: nil,
				WhoNode: auditWhoPtr("operator-laptop"),
				WhoUser: auditWhoPtr("owner@example.com"),
			},
			want: "結果未記錄，operator-laptop（owner@example.com）",
		},
		{
			name: "一筆什麼來源都沒有的紀錄仍然講得出歸因",
			event: AuditEvent{
				Outcome: &failed,
			},
			want: "failed，連來源位址都沒有",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := machineTimelineActionDetail(test.event); got != test.want {
				t.Errorf("操作員動作詳情實際是 %q，期望是 %q", got, test.want)
			}
		})
	}
}
