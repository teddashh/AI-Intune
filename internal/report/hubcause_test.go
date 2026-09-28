package report

import (
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/state"
)

// 三台在 90 秒內一起從失聯恢復的 fixture（跟 simultaneity_test 同一組）。
// 眨眼時刻 = 最早那個 UnreachableAt = now − 4h − 1min。
func blinkFixture(now time.Time) Input {
	return Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 4,
		Machines: []Machine{
			{ID: "a", DisplayName: "samplehub1", State: state.Degraded, Reason: "grok 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -4*time.Hour),
				UnreachableAt: []time.Time{at(now, -4*time.Hour-time.Minute)}},
			{ID: "b", DisplayName: "sampleagent2", State: state.Degraded, Reason: "claude 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -4*time.Hour+time.Minute),
				UnreachableAt: []time.Time{at(now, -4*time.Hour-30*time.Second)}},
			{ID: "c", DisplayName: "sampleagent4", State: state.Degraded, Reason: "claude 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -4*time.Hour+90*time.Second),
				UnreachableAt: []time.Time{at(now, -4*time.Hour)}},
			{ID: "d", DisplayName: "sampleagent3", State: state.Online, StateSince: at(now, -20*time.Hour)},
		},
	}
}

// 2026-09-06 03:55Z 那次：三台同一秒失聯，Hub 自己的日誌說它 03:53–03:55 不在。
// 早報要把這兩件事接起來講，而不是「通常是 Hub」這種泛話。
func TestBlinkIsExplainedByHubRestartInItsOwnJournal(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	in := blinkFixture(now)
	blinkAt := at(now, -4*time.Hour-time.Minute)
	in.HubEvents = []HubEvent{
		// started 通常在眨眼**之前**幾十秒：失聯轉換是 Hub 回來對帳之後才寫的。
		{At: blinkAt.Add(-40 * time.Second), Kind: "started",
			Detail: "v0.9 起來了；上一次活著是 2.4 分鐘前（09-06 11:52:30）—— 中間這段 Hub 不在"},
	}
	got := Render(in)
	if !strings.Contains(got, "Hub 自己沒在聽：") || !strings.Contains(got, "上一次活著是 2.4 分鐘前") {
		t.Errorf("Hub 的日誌明明記到它重啟過，早報卻沒把成因講出來\n%s", got)
	}
	if strings.Contains(got, "成因還沒有解釋") {
		t.Errorf("有證據還說沒解釋\n%s", got)
	}
}

// 9/4 00:46 那次：Hub 沒重啟、也沒記到卡住。這時候要**明說**沒有解釋，
// 不能講成「Hub 沒問題」—— 日誌看不到網路斷線。
func TestBlinkWithoutJournalEvidenceSaysSoExplicitly(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	in := blinkFixture(now)
	in.HubEvents = []HubEvent{
		// 三小時前的一次重啟，跟這次眨眼無關 —— 不准拿來當解釋。
		{At: at(now, -7*time.Hour), Kind: "started", Detail: "v0.9 起來了；上一次活著是 1 分鐘前"},
		// 「stopping」自己不解釋任何事（跟著的 started 才是）。
		{At: at(now, -4*time.Hour-2*time.Minute), Kind: "stopping", Detail: "收到停止訊號"},
	}
	got := Render(in)
	if !strings.Contains(got, "成因還沒有解釋") {
		t.Errorf("沒有證據卻沒說「沒解釋」—— 那會讓人以為 Hub 被排除了\n%s", got)
	}
	if strings.Contains(got, "上一次活著是 1 分鐘前") {
		t.Errorf("三小時前的重啟被拿來解釋現在的眨眼\n%s", got)
	}
	// 泛話那句仍然要在：它是行動建議（先查 Hub），不是成因。
	if !strings.Contains(got, "Hub 自己沒在聽") {
		t.Errorf("行動建議不見了\n%s", got)
	}
}

// 重啟跟卡住同時落在窗口裡的時候，講解釋力最強的那個（重啟）。
func TestBlinkCausePrefersRestartOverStall(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	in := blinkFixture(now)
	blinkAt := at(now, -4*time.Hour-time.Minute)
	in.HubEvents = []HubEvent{
		{At: blinkAt.Add(-3 * time.Minute), Kind: "reconcile_slow", Detail: "一輪對帳花了 45 秒"},
		{At: blinkAt.Add(-20 * time.Second), Kind: "started", Detail: "v0.9 起來了；上一次活著是 3 分鐘前"},
		{At: blinkAt.Add(-2 * time.Minute), Kind: "loop_stall", Detail: "對帳迴圈 4 分鐘沒被排到"},
	}
	got := Render(in)
	if !strings.Contains(got, "上一次活著是 3 分鐘前") {
		t.Errorf("窗口裡有重啟卻沒優先講它\n%s", got)
	}
	if strings.Contains(got, "一輪對帳花了") || strings.Contains(got, "沒被排到") {
		t.Errorf("同一次眨眼講了兩個成因\n%s", got)
	}
}
