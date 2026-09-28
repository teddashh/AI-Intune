package store

import (
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"
)

// 這一整個檔案的 fixture 是**實測值**，不是編出來的時間。
// 2026-09-03 sampleagent3（machine_id 3c95…）上真的有兩個 agent：
//
//	run A  started 13:21:18Z  心跳 13:21:19Z → 19:45:21Z  共 191 顆
//	run B  started 13:23:02Z  心跳 13:23:02Z → 18:48:20Z  共 165 顆
//
// 重疊 13:23:02Z → 18:48:20Z＝5 小時 25 分 18 秒，期間 Hub 全綠。
// 用真的數字當 fixture 的理由：一組編出來的時間會不小心避開真實資料的形狀
// （例如剛好讓兩段整齊對齊），而這個 bug 活了六小時就是因為它的形狀很平常。

var (
	runAStart = mustTime("2026-09-03T13:21:18Z")
	runAFirst = mustTime("2026-09-03T13:21:19Z")
	runALast  = mustTime("2026-09-03T19:45:21Z")
	runBStart = mustTime("2026-09-03T13:23:02Z")
	runBFirst = mustTime("2026-09-03T13:23:02Z")
	runBLast  = mustTime("2026-09-03T18:48:20Z")
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// mustRegister 直接寫一列名冊。
//
// ⚠ 刻意不走 enroll：RecordCheckin 會擋掉不在名冊上的機器（那是對的），
// 但這個檔案的主題不是 enroll，而 mustEnroll 生出來的是隨機 id ——
// 一個叫 "dirty" 的機器讓測試讀得懂哪一台該被報。
func mustRegister(t *testing.T, s *Store, machineID, name string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO machine_registry (machine_id, display_name, created_at) VALUES (?,?,?)`,
		machineID, name, fmtTime(runAFirst)); err != nil {
		t.Fatalf("register %s: %v", machineID, err)
	}
}

// replayRun 把一次 agent 啟動的心跳灌進 DB。
//
// ⚠ sentOffset 存在的唯一理由是 machine_checkins 的唯一鍵是
// (machine_id, sent_at)。兩個同時跑的 agent 心跳週期差不到 3 秒，
// 不給不同的偏移，run B 的心跳會 UPSERT 掉 run A 的，
// 於是 fixture 會安靜地變成「只有一個 agent」，測試就永遠是綠的。
//
// ⚠⚠ sentOffset 必須是**整秒**。fmtTime 用 time.RFC3339，那個 layout
// 沒有小數秒，所以 500ms 這種偏移會在寫進 DB 的時候被直接抹掉 ——
// 一個「看起來有做事、實際上沒有」的參數。實測代價：web 那邊同樣寫 500ms
// 的 fixture 有 60% 的機率壞掉（20 跑 12 敗），因為偏移有沒有效
// 取決於 time.Now() 的小數部分有沒有跨過整秒。
//
// 下面那段 count 檢查是真正的防線：偏移寫錯的時候它會當場喊，
// 而不是讓 fixture 安靜地變成另一個情境。
func replayRun(t *testing.T, s *Store, machineID string, started, first, last time.Time, n int, sentOffset time.Duration) {
	t.Helper()
	if n < 1 {
		t.Fatalf("replayRun: n=%d", n)
	}
	if sentOffset%time.Second != 0 {
		t.Fatalf("sentOffset 要是整秒，%s 會被 RFC3339 抹掉", sentOffset)
	}
	var step time.Duration
	if n > 1 {
		step = last.Sub(first) / time.Duration(n-1)
	}
	for i := 0; i < n; i++ {
		recv := first.Add(step * time.Duration(i))
		if i == n-1 {
			recv = last
		}
		c := model.Checkin{
			SentAt:         recv.Add(sentOffset),
			BootID:         "76548294-e947-4e97-9a5b-b26d0816cf00", // 同一次開機 —— 兩個 agent 的 boot_id 一樣
			AgentStartedAt: started,
		}
		if err := s.RecordCheckin(machineID, c, recv); err != nil {
			t.Fatalf("RecordCheckin: %v", err)
		}
	}
	// 灌進去的筆數要跟預期一樣。少了就是被另一個 run 的 UPSERT 蓋掉了。
	var got int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM machine_checkins WHERE machine_id = ? AND agent_started_at = ?`,
		machineID, fmtTime(started)).Scan(&got); err != nil {
		t.Fatalf("數心跳: %v", err)
	}
	if got != n {
		t.Fatalf("run %s 要有 %d 顆心跳，DB 裡只有 %d 顆 —— "+
			"被另一個 run 的 sent_at UPSERT 蓋掉了，fixture 已經不是你以為的情境",
			started.Format("15:04:05"), n, got)
	}
}

// 實測那六個小時必須被抓到。這是這個檔案存在的理由。
func TestTheRealDoubleAgentOutageIsCaught(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	replayRun(t, s, "m1", runAStart, runAFirst, runALast, 191, 0)
	replayRun(t, s, "m1", runBStart, runBFirst, runBLast, 165, 7*time.Second)

	got, err := s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("要抓到 1 台有兩個 agent，實際 %d 台：%+v", len(got), got)
	}
	d := got[0]
	if len(d.Runs) != 2 {
		t.Errorf("要指出 2 次啟動，實際 %d", len(d.Runs))
	}
	if !d.From.Equal(runBFirst) {
		t.Errorf("重疊起點要是 %s，實際 %s", runBFirst, d.From)
	}
	if !d.Until.Equal(runBLast) {
		t.Errorf("重疊終點要是 %s，實際 %s", runBLast, d.Until)
	}
	// 實測 5h25m18s。四捨五入到分鐘是 5h25m。
	if want := 5*time.Hour + 25*time.Minute + 18*time.Second; d.Until.Sub(d.From) != want {
		t.Errorf("重疊長度要是 %s，實際 %s", want, d.Until.Sub(d.From))
	}
}

// ⚠⚠ 這一支釘住的是「為什麼需要另外寫一個偵測器」。
//
// 既有的 crash-loop 計數器（AgentRestarts1h＝一小時內幾個不同的 agent_started_at）
// 在那六小時裡讀到的是 **2**，門檻是 3。它整整五個小時都停在門檻下面一格。
// 實測 hourly 取樣：13:30=7（我自己的部署）、14:30~18:30 都是 2、19:30=5。
//
//	一個門檻只看得到高度，看不到形狀。
//
// 一次重啟是尖峰，會在一小時內衰減回 1；兩個 agent 是高原，永遠停在 2。
// 這個計數器有三個區間（1＝健康、持續 2＝兩個 agent、短暫 ≥3＝crash-loop），
// 而程式裡只有一個門檻。
func TestTheCrashLoopCounterAloneWouldHaveMissedThis(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	replayRun(t, s, "m1", runAStart, runAFirst, runALast, 191, 0)
	replayRun(t, s, "m1", runBStart, runBFirst, runBLast, 165, 7*time.Second)

	// 照 store.go 數 crash-loop 的那條 query 一模一樣地數，取樣點在重疊中間。
	at := mustTime("2026-09-03T16:30:00Z")
	var restarts int
	if err := s.db.QueryRow(`
SELECT COUNT(DISTINCT agent_started_at) FROM machine_checkins
 WHERE machine_id = ? AND agent_started_at IS NOT NULL
   AND received_at >= ? AND received_at <= ?`,
		"m1", fmtTime(at.Add(-state.CrashLoopWindow)), fmtTime(at)).Scan(&restarts); err != nil {
		t.Fatalf("數 restart: %v", err)
	}
	if restarts != 2 {
		t.Fatalf("實測是 2，fixture 卻給了 %d —— fixture 跟真實資料不一樣了", restarts)
	}
	if restarts >= state.CrashLoopRestarts {
		t.Fatalf("這一支的前提是計數器抓不到（2 < %d）。門檻改成 %d 之後前提沒了，"+
			"要重新想這兩個偵測器的分工，不是把這行刪掉",
			state.CrashLoopRestarts, state.CrashLoopRestarts)
	}
	// 同一份資料，形狀看得出來。
	got, err := s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("計數器看不到的東西，重疊偵測要看得到，實際抓到 %d 台", len(got))
	}
}

// 正常的部署重啟不可以亮燈。實測 18:49:54 那次與 18:57:11 那次是前後相接的。
func TestANormalRestartIsNotCalledADoubleAgent(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	replayRun(t, s, "m1", mustTime("2026-09-03T18:49:54Z"),
		mustTime("2026-09-03T18:49:55Z"), mustTime("2026-09-03T18:55:54Z"), 4, 0)
	replayRun(t, s, "m1", mustTime("2026-09-03T18:57:11Z"),
		mustTime("2026-09-03T18:57:11Z"), mustTime("2026-09-03T19:04:50Z"), 5, 0)

	got, err := s.DoubleAgents(mustTime("2026-09-03T19:05:00Z"))
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("前後相接的兩次啟動是重啟，不是兩個 agent：%+v", got)
	}
}

// 交接殘影：舊 agent 最後一顆心跳比新 agent 的第一顆**晚被收到**。
// ⚠ 這是每一次重啟都可能發生的事，如果門檻是 0，那這盞燈每次部署都會亮。
func TestADelayedFinalCheckinFromTheOldAgentDoesNotRaiseTheAlarm(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	old := mustTime("2026-09-03T18:40:00Z")
	replayRun(t, s, "m1", old, old, mustTime("2026-09-03T18:50:12Z"), 6, 0)
	// 新 agent 18:50:00 起來，舊的那顆 18:50:12 才進來 —— 重疊 12 秒。
	replayRun(t, s, "m1", mustTime("2026-09-03T18:50:00Z"),
		mustTime("2026-09-03T18:50:00Z"), mustTime("2026-09-03T19:00:00Z"), 6, time.Second)

	got, err := s.DoubleAgents(mustTime("2026-09-03T19:00:30Z"))
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("12 秒的交接殘影不是兩個 agent（門檻 %s）：%+v", DoubleAgentMinOverlap, got)
	}
}

// ⚠⚠ 舊版 agent 不送 agent_started_at。實測 sampleagent2 有 39 筆這種心跳。
// 「這一欄是空的」的意思是「這筆觀測早於這個區分」，不是「沒有第二個 agent」。
// 把 NULL 當成一個身分，整片舊資料會變成假警報。
func TestCheckinsWithoutAgentStartedAtAreNotASecondAgent(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	// 39 筆沒有 agent_started_at 的心跳（AgentStartedAt 零值 → 存成 NULL）。
	first := mustTime("2026-09-03T06:04:58Z")
	last := mustTime("2026-09-03T07:03:39Z")
	step := last.Sub(first) / 38
	for i := 0; i < 39; i++ {
		at := first.Add(step * time.Duration(i))
		if err := s.RecordCheckin("m1", model.Checkin{SentAt: at}, at); err != nil {
			t.Fatalf("RecordCheckin: %v", err)
		}
	}
	// 同一台後來換了新版 agent，只有一個。
	replayRun(t, s, "m1", mustTime("2026-09-03T07:36:50Z"),
		mustTime("2026-09-03T07:36:50Z"), mustTime("2026-09-03T12:26:04Z"), 20, 0)

	got, err := s.DoubleAgents(mustTime("2026-09-03T12:30:00Z"))
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("沒帶 agent_started_at 的舊心跳不是第二個 agent：%+v", got)
	}
}

// 只送過一顆心跳的野 agent 也要抓到 —— 它的區間是一個「點」。
// 實測 sampleagent3 就有一次 n=1 的啟動。
func TestAStrayAgentWithOneCheckinInsideALongRunIsCaught(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	replayRun(t, s, "m1", runAStart, runAFirst, runALast, 20, 0)
	// 落在 run A 中間的孤兒心跳，但一個點的重疊長度是 0，低於門檻。
	// 所以這裡給它兩顆、跨過門檻 —— 重點是它**沒有**跟 run A 前後相接。
	mid := mustTime("2026-09-03T16:00:00Z")
	replayRun(t, s, "m1", mid, mid, mid.Add(6*time.Minute), 2, 2*time.Second)

	got, err := s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("夾在長 run 中間的野 agent 要抓到，實際 %d：%+v", len(got), got)
	}
	if d := got[0]; !d.From.Equal(mid) {
		t.Errorf("重疊要從野 agent 的第一顆心跳算起 %s，實際 %s", mid, d.From)
	}
}

// 重疊要用 Hub 收到的時間量，不是機器自報的時間。
// ⚠ 一台時鐘歪 3 小時的機器，兩段區間在它自己的時間軸上會錯開。
func TestOverlapIsMeasuredOnHubReceiptNotTheMachineClock(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	replayRun(t, s, "m1", runAStart, runAFirst, runALast, 20, 0)
	replayRun(t, s, "m1", runBStart, runBFirst, runBLast, 20, -3*time.Hour)

	got, err := s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("時鐘歪掉不可以讓真的重疊消失，實際抓到 %d 台", len(got))
	}
}

// 退役的機器離開分母 —— 它不該再發警報。
func TestARetiredMachineDoesNotRaiseTheAlarm(t *testing.T) {
	s := newTestStore(t)
	id := mustEnroll(t, s, "sampleagent3", runAFirst)
	replayRun(t, s, id, runAStart, runAFirst, runALast, 20, 0)
	replayRun(t, s, id, runBStart, runBFirst, runBLast, 20, 7*time.Second)

	if got, err := s.DoubleAgents(runALast); err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	} else if len(got) != 1 {
		t.Fatalf("退役前要抓得到，實際 %d", len(got))
	}
	if err := s.RetireMachine(id, runALast); err != nil {
		t.Fatalf("RetireMachine: %v", err)
	}
	got, err := s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("退役的機器不該再發警報：%+v", got)
	}
}

// 「現在還在發生」跟「已經結束」是兩件事，畫面上要分得出來。
func TestTheFindingSaysWhetherItIsStillHappening(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	replayRun(t, s, "m1", runAStart, runAFirst, runALast, 20, 0)
	replayRun(t, s, "m1", runBStart, runBFirst, runBLast, 20, 7*time.Second)

	// 兩邊最後一顆心跳分別在 19:45:21 與 18:48:20。
	// 站在 18:50 看：兩邊都還在講話。
	got, err := s.DoubleAgents(mustTime("2026-09-03T18:50:00Z"))
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 || !got[0].Ongoing {
		t.Fatalf("兩邊都還在送心跳時要說「現在還在發生」：%+v", got)
	}
	// 站在 19:45 看：run B 已經一小時沒講話了，只剩一個。
	got, err = s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("事情發生過就要留在畫面上，實際 %d", len(got))
	}
	if got[0].Ongoing {
		t.Errorf("只剩一個 agent 在講話了，不該說「現在還在發生」")
	}
}

// 一台一整天只有一個 agent 的機器，不可以有任何發現。
func TestAMachineWithOneAgentAllDayHasNoFinding(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	replayRun(t, s, "m1", runAStart, runAFirst, runALast, 191, 0)

	got, err := s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("只有一個 agent 的機器不該有發現：%+v", got)
	}
}

// 訊息要講得出「哪一台、什麼時候、為什麼你之前沒看到」。
// ⚠ 一個只說「偵測到重複 agent」的訊息，等於要人自己去翻資料庫。
func TestTheReasonNamesTheMachineAndTheWindow(t *testing.T) {
	s := newTestStore(t)
	id := mustEnroll(t, s, "sampleagent3", runAFirst)
	replayRun(t, s, id, runAStart, runAFirst, runALast, 20, 0)
	replayRun(t, s, id, runBStart, runBFirst, runBLast, 20, 7*time.Second)

	got, err := s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("要抓到 1 台，實際 %d", len(got))
	}
	r := got[0].Reason
	for _, want := range []string{"sampleagent3", "2 個 agent", "5h25m"} {
		if !strings.Contains(r, want) {
			t.Errorf("訊息要提到 %q，實際是：%s", want, r)
		}
	}
	if got[0].DisplayName != "sampleagent3" {
		t.Errorf("要用人講的名字，實際 %q", got[0].DisplayName)
	}
}

// 一台機器上有兩個 agent，不可以讓另一台乾淨的機器被連坐。
func TestOnlyTheMachineWithTwoAgentsIsReported(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "dirty", "sampleagent3")
	mustRegister(t, s, "clean", "sampleagent2")
	replayRun(t, s, "dirty", runAStart, runAFirst, runALast, 20, 0)
	replayRun(t, s, "dirty", runBStart, runBFirst, runBLast, 20, 7*time.Second)
	replayRun(t, s, "clean", runAStart, runAFirst, runALast, 20, 0)

	got, err := s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("只有一台該被報，實際 %d：%+v", len(got), got)
	}
	if got[0].MachineID != "dirty" {
		t.Errorf("報錯機器了：%s", got[0].MachineID)
	}
}

// ⚠⚠ 實測 sampleagent3 一整天的完整形狀 —— 五次啟動，但**從來沒有三個同時活著**。
//
// 這一支是把偵測器接到真實資料上之後才長出來的。第一版拿 len(Runs) 去講
// 「幾個 agent 同時在回報」，畫面於是說 sampleagent3 有「5 個 agent 同時在回報」。
// 那是假的：其中三次（18:49:54 / 18:57:11 / 19:06:12）是前後相接的部署重啟，
// 它們各自都跟那個長命的野 agent 重疊，但彼此沒有重疊。
//
//	一個把「涉及幾次」講成「同時幾個」的數字，會讓人去找不存在的 process。
//
// 同理，From→Until 是 6h22m，但中間有三段只有一個 agent 的空隙，
// 真正被污染的時間是 6h18m4s。
func TestFiveRunsButNeverThreeAtOnce(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	for i, r := range []struct {
		started, first, last string
		n                    int
	}{
		{"2026-09-03T13:21:18Z", "2026-09-03T13:21:19Z", "2026-09-03T19:45:21Z", 191},
		{"2026-09-03T13:23:02Z", "2026-09-03T13:23:02Z", "2026-09-03T18:48:20Z", 165},
		{"2026-09-03T18:49:54Z", "2026-09-03T18:49:55Z", "2026-09-03T18:55:54Z", 4},
		{"2026-09-03T18:57:11Z", "2026-09-03T18:57:11Z", "2026-09-03T19:04:50Z", 5},
		{"2026-09-03T19:06:12Z", "2026-09-03T19:06:13Z", "2026-09-03T19:46:45Z", 21},
	} {
		replayRun(t, s, "m1", mustTime(r.started), mustTime(r.first), mustTime(r.last),
			r.n, time.Duration(i)*7*time.Second)
	}

	got, err := s.DoubleAgents(mustTime("2026-09-03T19:47:00Z"))
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("要抓到 1 台，實際 %d", len(got))
	}
	d := got[0]
	if d.Peak != 2 {
		t.Errorf("最多同時 2 個，實際說 %d —— 五次啟動不等於五個同時在跑", d.Peak)
	}
	if len(d.Runs) != 5 {
		t.Errorf("五次啟動都該列出來（人要知道殺哪一個），實際 %d", len(d.Runs))
	}
	// 5h25m18s + 5m59s + 7m39s + 39m8s
	if want := 6*time.Hour + 18*time.Minute + 4*time.Second; d.Overlap != want {
		t.Errorf("重疊總長要是 %s，實際 %s", want, d.Overlap)
	}
	// From→Until 比重疊總長還久，因為中間有只剩一個 agent 的空隙。
	if span := d.Until.Sub(d.From); span <= d.Overlap {
		t.Errorf("From→Until (%s) 該比重疊總長 (%s) 久", span, d.Overlap)
	}
	if !strings.Contains(d.Reason, "最多同時 2 個") {
		t.Errorf("訊息要講「最多同時 2 個」，實際：%s", d.Reason)
	}
	if !strings.Contains(d.Reason, "5 次啟動") {
		t.Errorf("訊息也要講清楚涉及 5 次啟動，實際：%s", d.Reason)
	}
}

// 一次乾淨的重啟：舊 run 的 Last 剛好等於新 run 的 First。
// ⚠ 掃描線在同一時刻先加後減的話，計數會短暫變成 2，Peak 就被污染成 2 ——
// 長度 0 進不了門檻，但只要旁邊還有別的重疊，那個假的 Peak 就會被印出來。
func TestABackToBackRestartNeverCountsAsTwoAtOnce(t *testing.T) {
	s := newTestStore(t)
	mustRegister(t, s, "m1", "sampleagent3")
	// 一個長命的野 agent，加上兩個「首尾剛好相接」的重啟。
	replayRun(t, s, "m1", runAStart, runAFirst, runALast, 20, 0)
	touch := mustTime("2026-09-03T16:00:00Z")
	replayRun(t, s, "m1", mustTime("2026-09-03T15:00:00Z"),
		mustTime("2026-09-03T15:00:00Z"), touch, 6, 7*time.Second)
	replayRun(t, s, "m1", touch, touch, mustTime("2026-09-03T17:00:00Z"), 6, 14*time.Second)

	got, err := s.DoubleAgents(runALast)
	if err != nil {
		t.Fatalf("DoubleAgents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("要抓到 1 台，實際 %d", len(got))
	}
	if got[0].Peak != 2 {
		t.Errorf("首尾相接不算同時活著，Peak 該是 2，實際 %d", got[0].Peak)
	}
}
