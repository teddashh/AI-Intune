package report

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/state"
)

var now = time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)

func base() Input {
	return Input{
		Now:            now,
		Since:          now.Add(-24 * time.Hour),
		Expected:       5,
		Reporting:      5,
		ReportingKnown: true,
		FleetCounts:    map[state.State]int{state.Online: 5},
	}
}

// 這是整個 package 最重要的測試。
//
// 沒事的日子也必須說話 —— 否則「沒收到早報」同時代表「今天沒事」與
// 「Hub 死了」，而那兩件事需要完全相反的反應。
func TestQuietDayStillSpeaks(t *testing.T) {
	got := Render(base())
	if !strings.Contains(got, "alive; 0 changes") {
		t.Fatalf("沒變化的日子必須送 alive 行，實際輸出：\n%s", got)
	}
	if !strings.Contains(got, "5/5 報到") {
		t.Errorf("第一行應該是名冊分母，實際輸出：\n%s", got)
	}
}

func TestMissingReportingMeasurementIsUnknownNotZero(t *testing.T) {
	in := base()
	in.Reporting, in.ReportingKnown = 0, false
	got := Render(in)
	if !strings.Contains(got, "?/5 報到（freshness 不明）") || strings.Contains(got, "0/5 報到") {
		t.Fatalf("missing reporting measurement was made into a real zero:\n%s", got)
	}
}

// TestNothingWrongIsOnlySaidWhenNothingIsWrong 是這個檔案裡最重要的測試。
//
// ⚠ `alive; 0 changes` 的字面意思是「今天沒事」。它是死人之鐘的心跳 ——
// 存在的理由是讓「沒收到訊息」能明確代表「Hub 死了」。
//
// 所以它有一條硬規則：**只要有任何一台不是 Online，就不准出現這一行。**
//
// 這條規則是實測抓出來的。原本 collect() 最後那一段只收 Degraded，於是
// 一台昨天失聯、今天還失聯的機器（Changed=false、還沒滿 7 天、不是
// Degraded）一行都不產生，整份早報變成：
//
//	1/2 報到 · 失聯 1
//	alive; 0 changes
//
// 一台失聯的機器讓早報說出「沒事」—— 那正是這整個產品存在要防的那件事。
// 身分衝突（最嚴重的狀態）也一樣會消失。
//
// 這個測試掃過每一個非 Online 的狀態，而不是只測當初出事的那一個，
// 因為將來再加新狀態時，作者多半不會回來讀這段註解。
func TestNothingWrongIsOnlySaidWhenNothingIsWrong(t *testing.T) {
	now := time.Now().UTC()
	broken := []state.State{
		state.Degraded, state.Unreachable, state.IdentityConflict, state.NeverReported,
	}
	for _, s := range broken {
		t.Run(string(s), func(t *testing.T) {
			in := Input{
				Now: now, Since: now.Add(-24 * time.Hour),
				Expected:    2,
				FleetCounts: map[state.State]int{state.Online: 1, s: 1},
				Machines: []Machine{
					{ID: "a", DisplayName: "samplehub1", State: state.Online,
						StateSince: now.Add(-72 * time.Hour)},
					// ⚠ 關鍵條件：狀態沒變（不是今天才壞的），而且還沒壞滿 7 天。
					// 這是最容易掉進縫裡的那一格。
					{ID: "b", DisplayName: "sampleagent3", State: s, Reason: "壞掉的理由",
						StateSince: now.Add(-48 * time.Hour), Changed: false,
						PreviousState: state.Online},
				},
			}
			got := Render(in)
			if strings.Contains(got, "alive; 0 changes") {
				t.Errorf("有一台是 %s，早報卻說沒事：\n%s", s, got)
			}
			if !strings.Contains(got, "sampleagent3") {
				t.Errorf("壞掉的那台沒有出現在早報裡（狀態 %s）：\n%s", s, got)
			}
		})
	}
}

// 名冊是分母：沒報到的機器要出現在標題裡，不是從統計上消失。
func TestHeaderShowsDenominatorNotJustReporters(t *testing.T) {
	in := base()
	in.FleetCounts = map[state.State]int{state.Online: 3, state.NeverReported: 2}
	in.Reporting = 3
	got := Render(in)
	if !strings.Contains(got, "3/5 報到") {
		t.Errorf("分母應該是名冊上的 5，不是有回報的 3。實際：\n%s", got)
	}
	if !strings.Contains(got, "從未報到 2") {
		t.Errorf("從未報到的機器必須出現在標題，實際：\n%s", got)
	}
}

func TestLineCapAndOverflow(t *testing.T) {
	in := base()
	for i := 0; i < 9; i++ {
		in.Machines = append(in.Machines, Machine{
			DisplayName: string(rune('a'+i)) + "-box",
			State:       state.Degraded, Reason: "磁碟 90% 已用",
			StateSince: now.Add(-time.Hour),
		})
	}
	got := Render(in)
	body := strings.Split(strings.TrimSpace(got), "\n")
	// 標題 1 行 + 空行被 TrimSpace/Split 處理後，內容行數不該超過 MaxLines+2
	var content int
	for _, l := range body {
		if strings.HasSuffix(l, "-box 降級：磁碟 90% 已用") {
			content++
		}
	}
	if content > MaxLines {
		t.Errorf("內容行數 %d 超過上限 %d：\n%s", content, MaxLines, got)
	}
	if !strings.Contains(got, "+4 more in hub") {
		t.Errorf("溢出的行必須摺疊成一行，實際：\n%s", got)
	}
}

// 排序：今天才壞的要排在已經壞很久的前面。
//
// 理由：已經壞很久的那條昨天沒被處理，今天多半也不會；而它會擠掉
// 今天才出現、現在處理最便宜的那些。
func TestNewBreakageOutranksOldBreakage(t *testing.T) {
	in := base()
	in.Machines = []Machine{
		{DisplayName: "old-box", State: state.Unreachable, Reason: "失聯",
			StateSince: now.Add(-30 * 24 * time.Hour)},
		{DisplayName: "new-box", State: state.Degraded, Reason: "磁碟 97% 已用",
			StateSince: now.Add(-time.Hour), Changed: true, PreviousState: state.Online},
	}
	got := Render(in)
	iNew := strings.Index(got, "new-box")
	iOld := strings.Index(got, "old-box")
	if iNew < 0 || iOld < 0 {
		t.Fatalf("兩台都該出現：\n%s", got)
	}
	if iNew > iOld {
		t.Errorf("今天才壞的 new-box 應該排在 old-box 前面：\n%s", got)
	}
}

// 壞超過 7 天改成 still broken since，而不是每天重複同一條細節。
// 一直重複同一條就是在訓練人忽略它。
func TestLongBrokenCollapsesToStillBroken(t *testing.T) {
	in := base()
	in.Machines = []Machine{{
		DisplayName: "sampleagent1", State: state.Degraded,
		Reason:     "磁碟 97% 已用，只剩 1.7 GB",
		StateSince: now.Add(-30 * 24 * time.Hour),
	}}
	got := Render(in)
	if !strings.Contains(got, "still broken since") {
		t.Errorf("壞超過 7 天要摺疊成 still broken since，實際：\n%s", got)
	}
	if strings.Contains(got, "只剩 1.7 GB") {
		t.Errorf("摺疊之後不該再重複細節，實際：\n%s", got)
	}
}

// 恢復也要講。否則人不知道昨天那條到底處理完了沒有。
func TestRecoveryIsReported(t *testing.T) {
	in := base()
	in.Machines = []Machine{{
		DisplayName: "sampleagent4", State: state.Online, StateSince: now.Add(-time.Hour),
		Changed: true, PreviousState: state.Unreachable,
	}}
	got := Render(in)
	if !strings.Contains(got, "sampleagent4 恢復") {
		t.Errorf("恢復要出現在早報，實際：\n%s", got)
	}
}

// 憑證過期有明確截止時間，排序上要贏過一般的 Degraded。
func TestCredentialExpiryOutranksGenericDegraded(t *testing.T) {
	exp := now.Add(-13 * 24 * time.Hour)
	in := base()
	in.Machines = []Machine{
		{DisplayName: "z-box", State: state.Degraded, Reason: "磁碟 88% 已用",
			StateSince: now.Add(-2 * time.Hour)},
		{DisplayName: "a-box", State: state.Degraded, Reason: "claude 的登入已過期",
			StateSince:   now.Add(-2 * time.Hour),
			CredExpiries: []CredExpiry{{Provider: "claude", Status: "expired", ExpiresAt: &exp}},
		},
	}
	got := Render(in)
	if strings.Index(got, "a-box 的 claude 登入已過期") > strings.Index(got, "z-box 降級") {
		t.Errorf("憑證過期應該排在一般降級前面：\n%s", got)
	}
}

// TestCredentialExpiryPeerTailStaysShort 守的是早報只帶一句短證據，而且沒有
// 同儕時原本的過期句完全不變；詳細判決那整段推論不准被搬進刷牙時讀的一行。
func TestCredentialExpiryPeerTailStaysShort(t *testing.T) {
	exp := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	machine := Machine{
		DisplayName: "sampleagent2", State: state.Degraded, Reason: "claude 的登入已過期",
		StateSince:   now.Add(-2 * time.Hour),
		CredExpiries: []CredExpiry{{Provider: "claude", Status: "expired", ExpiresAt: &exp}},
	}
	in := base()
	in.Machines = []Machine{machine}
	withoutPeer := Render(in)
	wantOriginal := "sampleagent2 的 claude 登入已過期（Sep 1 起）"
	if !strings.Contains(withoutPeer, wantOriginal) {
		t.Fatalf("沒有同儕時原句不准改：\n%s", withoutPeer)
	}
	if strings.Contains(withoutPeer, "；同一家") {
		t.Fatalf("沒有同儕卻多出跨機器尾巴：\n%s", withoutPeer)
	}

	machine.CredExpiries[0].Peers = []state.CredPeer{{DisplayName: "samplehub1", RefreshesSeen: 8}}
	in.Machines = []Machine{machine}
	withPeer := Render(in)
	if !strings.Contains(withPeer, wantOriginal+"；同一家在 samplehub1 有在續") {
		t.Errorf("有同儕時應補短尾巴：\n%s", withPeer)
	}
	if strings.Contains(withPeer, "分不出是不是同一張") {
		t.Errorf("早報不該搬進詳細判決：\n%s", withPeer)
	}
}

// 沉默失敗必須進早報 —— 它是最容易被忽略的一類，因為它不會自己喊。
func TestSilentFailureSurfaces(t *testing.T) {
	in := base()
	in.Machines = []Machine{{
		DisplayName: "sampleagent3", State: state.Degraded,
		Reason: "從來沒跑完過任何東西", StateSince: now.Add(-time.Hour),
		Findings: []state.Finding{{
			Kind: "workload", Severity: 3,
			Message: "裝了 OpenClaw，但找不到任何任務執行紀錄 —— 它從來沒跑完過任何東西",
		}},
	}}
	got := Render(in)
	if !strings.Contains(got, "從來沒跑完過任何東西") {
		t.Errorf("沉默失敗必須出現在早報，實際：\n%s", got)
	}
}

// 早報的順序必須穩定 —— 否則人會以為情況變了，其實只是排序在跳。
func TestStableOrdering(t *testing.T) {
	in := base()
	for _, n := range []string{"m3", "m1", "m2"} {
		in.Machines = append(in.Machines, Machine{
			DisplayName: n, State: state.Degraded, Reason: "磁碟 88% 已用",
			StateSince: now.Add(-time.Hour),
		})
	}
	first := Render(in)
	for i := 0; i < 5; i++ {
		if got := Render(in); got != first {
			t.Fatalf("同樣的輸入產生了不同的早報：\n%s\n---\n%s", first, got)
		}
	}
	if strings.Index(first, "m1") > strings.Index(first, "m2") {
		t.Errorf("同權重的行應該照名字排：\n%s", first)
	}
}

// TestNewMachineIsNotCalledRecovery：第一次報到不是「恢復」。
//
// ⚠ 這條是看著真機輸出補的 —— 原本會印出「samplehub1 恢復（）」。
// 空括號本身就是 bug 的招牌，但更糟的是那句話的意思：把新報到寫成恢復，
// 會讓人以為昨天有一台壞掉而他沒發現。
func TestNewMachineIsNotCalledRecovery(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	out := Render(Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 1,
		FleetCounts: map[state.State]int{state.Online: 1},
		Machines: []Machine{{
			ID: "m1", DisplayName: "samplehub1", State: state.Online,
			Reason: "40 秒前回報", StateSince: now.Add(-time.Minute),
			Changed: true, PreviousState: "", // 之前沒有任何狀態
		}},
	})
	if strings.Contains(out, "（）") {
		t.Errorf("印出了空括號：\n%s", out)
	}
	if strings.Contains(out, "恢復") {
		t.Errorf("把第一次報到講成了恢復：\n%s", out)
	}
	if !strings.Contains(out, "第一次報到") {
		t.Errorf("沒有講出這是第一次報到：\n%s", out)
	}
}

// TestRealRecoveryStillSaysWhatItRecoveredFrom：真的恢復要講出原本是什麼，
// 否則人不知道昨天那條到底是不是同一條。
func TestRealRecoveryStillSaysWhatItRecoveredFrom(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	out := Render(Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 1,
		FleetCounts: map[state.State]int{state.Online: 1},
		Machines: []Machine{{
			ID: "m1", DisplayName: "sampleagent3", State: state.Online,
			Reason: "30 秒前回報", StateSince: now.Add(-time.Hour),
			Changed: true, PreviousState: state.Unreachable,
		}},
	})
	if !strings.Contains(out, "恢復") || !strings.Contains(out, "失聯") {
		t.Errorf("沒有講出從什麼狀態恢復的：\n%s", out)
	}
}

// TestNeverReportedMachinesCollapseToOneLine：五台裡四台沒報到時，
// 早報不准用掉四行去講同一件事。
//
// ⚠ 這條是看著真機輸出補的。原本印出來是：
//
//	1/5 報到 · 從未報到 4
//	sampleagent1 新報到就是 從未報到：從未報到 —— 它在名冊上…
//	sampleagent2 新報到就是 從未報到：從未報到 —— 它在名冊上…
//	sampleagent3 新報到就是 從未報到：從未報到 —— 它在名冊上…
//	sampleagent4  新報到就是 從未報到：從未報到 —— 它在名冊上…
//
// 五行裡四行一樣。這種早報第三天就會被整則跳過，而那是這個產品最致命的
// 失效模式 —— 它唯一的介面失去讀者，等於它死了。
func TestNeverReportedMachinesCollapseToOneLine(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 5,
		FleetCounts: map[state.State]int{state.Online: 1, state.NeverReported: 4},
		Machines: []Machine{{
			ID: "m0", DisplayName: "samplehub1", State: state.Online,
			Reason: "6 秒前回報", StateSince: now.Add(-10 * time.Minute),
		}},
	}
	for _, n := range []string{"sampleagent1", "sampleagent2", "sampleagent3", "sampleagent4"} {
		in.Machines = append(in.Machines, Machine{
			ID: n, DisplayName: n, State: state.NeverReported,
			Reason:     "從未報到 —— 它在名冊上，所以它算在分母裡",
			StateSince: now.Add(-20 * time.Minute), Changed: true,
		})
	}
	out := Render(in)

	body := strings.Split(strings.TrimSpace(out), "\n")[1:] // 去掉開頭的分母那行
	dead := 0
	for _, l := range body {
		if strings.Contains(l, "從未報到") {
			dead++
		}
	}
	if dead != 1 {
		t.Errorf("從未報到用掉了 %d 行，應該只有 1 行：\n%s", dead, out)
	}

	// ⚠ 但名字不能省。「4 台沒報到」不能行動，名字可以。
	for _, n := range []string{"sampleagent1", "sampleagent2", "sampleagent3", "sampleagent4"} {
		if !strings.Contains(out, n) {
			t.Errorf("%s 的名字不見了 —— 只給數字的話人不知道要去修哪台：\n%s", n, out)
		}
	}
	if strings.Contains(out, "新報到就是") {
		t.Errorf("出現了「新報到就是 從未報到」這種廢話：\n%s", out)
	}
}

// TestManyNeverReportedTruncatesNames：名字太多要截斷，
// ⚠ 一行塞十二個名字等於沒有名字。
func TestManyNeverReportedTruncatesNames(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 12,
		FleetCounts: map[state.State]int{state.NeverReported: 12},
	}
	for i := 0; i < 12; i++ {
		in.Machines = append(in.Machines, Machine{
			DisplayName: fmt.Sprintf("box%02d", i), State: state.NeverReported,
			StateSince: now.Add(-time.Hour), Changed: true,
		})
	}
	out := Render(in)
	if !strings.Contains(out, "等 12 台") {
		t.Errorf("名字沒有截斷：\n%s", out)
	}
	if len(strings.Split(strings.TrimSpace(out), "\n")) > MaxLines+1 {
		t.Errorf("超過行數上限：\n%s", out)
	}
}

// TestLeavingNeverReportedReadsAsFirstAppearance：NeverReported → X 要講成「第一次報到」。
//
// ⚠ 這條是看真的輸出改的。原本印「sampleagent2 從未報到 → 降級」，字面上完全正確
// （它的上一個狀態確實是 NeverReported），但讀起來像在說它現在沒報到 ——
// 而事實正好相反：它剛剛才第一次講話。
//
// NeverReported 的定義是「在名冊上但從來沒成功 check-in 過」，所以離開 NeverReported 就回不去。
// NeverReported → X 永遠代表「它終於出現了」。
func TestLeavingNeverReportedReadsAsFirstAppearance(t *testing.T) {
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)
	out := Render(Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 1,
		FleetCounts: map[state.State]int{state.Degraded: 1},
		Machines: []Machine{{
			ID: "m1", DisplayName: "sampleagent2", State: state.Degraded,
			Reason: "grok 的登入在 44.6 小時前就過期了", StateSince: now.Add(-time.Minute),
			Changed: true, PreviousState: state.NeverReported,
		}},
	})
	if strings.Contains(out, "從未報到 →") {
		t.Errorf("讀起來像在說它現在沒報到：\n%s", out)
	}
	if !strings.Contains(out, "第一次報到就是降級") {
		t.Errorf("沒有講成第一次出現：\n%s", out)
	}
	// 理由要留著 —— 沒有理由的狀態轉移不能行動。
	if !strings.Contains(out, "grok") {
		t.Errorf("理由不見了：\n%s", out)
	}
}

// 早報最後那個連結。
//
// ⚠ 三條規則各自對應一種真的會發生的爛掉方式：
//
//  1. 沒有 BaseURL 就沒有連結 —— 寧可沒有，也不要一個點下去是 127.0.0.1 的。
//  2. 有話要說的時候一定要有連結 —— 那是「≤ 兩次點擊」的第一次點擊。
//  3. `alive; 0 changes` 不放連結 —— 沒有東西可以點過去，而每天都出現的
//     連結會退化成版面裝飾，然後真的需要點的那天它就被跳過了。
func TestReportLinksToHubOnlyWhenItCanAndShould(t *testing.T) {
	const hub = "http://100.64.0.1:8787"

	withProblem := func(in Input) Input {
		in.FleetCounts = map[state.State]int{state.Online: 4, state.Degraded: 1}
		in.Machines = []Machine{{
			ID: "m1", DisplayName: "samplehub1", State: state.Degraded,
			Reason: "grok 的登入過期了", StateSince: now.Add(-time.Hour), Changed: true,
			PreviousState: state.Online,
		}}
		return in
	}

	t.Run("沒有 BaseURL 就完全不提網址", func(t *testing.T) {
		got := Render(withProblem(base()))
		if strings.Contains(got, "http") {
			t.Errorf("推不出位址時不該出現任何網址，實際輸出：\n%s", got)
		}
	})

	t.Run("有問題就帶連結", func(t *testing.T) {
		in := withProblem(base())
		in.BaseURL = hub
		got := Render(in)
		if !strings.Contains(got, hub+"/") {
			t.Errorf("有話要說的早報必須帶連結，實際輸出：\n%s", got)
		}
		// ⚠ 連結是一行，不是每台一行。五行早報變成十行就沒有人讀完了。
		if n := strings.Count(got, hub); n != 1 {
			t.Errorf("連結應該只出現一次，實際 %d 次：\n%s", n, got)
		}
		// 連結在最後。放中間會把它插進「今天要處理什麼」的列表裡。
		lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
		if last := lines[len(lines)-1]; !strings.HasSuffix(last, hub+"/") {
			t.Errorf("連結應該是最後一行，實際最後一行是 %q", last)
		}
	})

	t.Run("沒事的日子不帶連結", func(t *testing.T) {
		in := base()
		in.BaseURL = hub
		got := Render(in)
		if !strings.Contains(got, "alive; 0 changes") {
			t.Fatalf("前提錯了，這應該是沒事的日子：\n%s", got)
		}
		if strings.Contains(got, hub) {
			t.Errorf("沒事的日子不該有連結，實際輸出：\n%s", got)
		}
	})

	// 被截掉的那幾行只寫 `+N more in hub`，而 "hub" 在沒有網址的時候
	// 是一個人得自己想辦法的名詞。有連結時這兩行必須同時出現。
	t.Run("超出行數上限時，連結讓 more in hub 變成可以按的", func(t *testing.T) {
		in := base()
		in.BaseURL = hub
		in.FleetCounts = map[state.State]int{state.Degraded: 8}
		in.Expected = 8
		for i := 0; i < 8; i++ {
			in.Machines = append(in.Machines, Machine{
				ID: fmt.Sprint(i), DisplayName: fmt.Sprintf("box%d", i),
				State: state.Degraded, Reason: "壞了", StateSince: now.Add(-time.Hour),
				Changed: true, PreviousState: state.Online,
			})
		}
		got := Render(in)
		if !strings.Contains(got, "more in hub") || !strings.Contains(got, hub+"/") {
			t.Errorf("截斷行與連結必須同時出現，實際輸出：\n%s", got)
		}
	})
}

// ⚠⚠ 這一條回答的是 Ted 那句「一堆死掉的 error log 也報不出來，
// 都要透過我們其他的機制溝通？」——答案是不用另建機制：
// 事件流的判決走的就是既有的「沉默失敗」那條管線。
// 這個測試把那件事釘住，不然哪天有人改了 Kind，這條路會安靜地斷掉。
func TestEventStreamFindingReachesTheDailyReport(t *testing.T) {
	in := base()
	in.Machines = []Machine{{
		DisplayName: "sampleagent2", State: state.Degraded,
		Reason: "openclaw-watcher.service 在過去 1 小時內回報了 baseline_hash_mismatch ×48",
		// ⚠ StateSince 是**這台變成 Degraded 的時間**，不是那個問題存在多久。
		// sampleagent2 的檔案被改了 81 天，但在這個功能上線以前它一直是綠的 ——
		// 所以上線當天它是「今天才降級」。
		StateSince: now.Add(-2 * time.Hour),
		Findings: []state.Finding{
			{Kind: "workload", Severity: 3,
				Message: "openclaw-watcher.service 在過去 1 小時內回報了 baseline_hash_mismatch ×48 —— 沒有人聽得見它喊什麼"},
			// advisory 的不該被送出去 —— 早報要留給需要動作的東西。
			{Kind: "workload", Severity: 1, Advisory: true,
				Message: "openclaw-watcher.service 還寫出了沒有人宣告過的事件：watcher_heartbeat ×60"},
		},
	}}
	got := Render(in)
	if !strings.Contains(got, "baseline_hash_mismatch") {
		t.Errorf("事件流的判決必須進早報，實際：\n%s", got)
	}
	if !strings.Contains(got, "沒有人聽得見它喊什麼") {
		t.Errorf("宣告時寫的 why 要一路帶到早報，實際：\n%s", got)
	}
	if strings.Contains(got, "watcher_heartbeat") {
		t.Errorf("advisory 不該進早報 —— 那會把需要動作的東西淹掉，實際：\n%s", got)
	}
}

// ⚠⚠ 已知且刻意的行為，釘在這裡免得哪天有人以為是 bug：
// 同一台降級**超過 7 天**之後，早報會把它收成一行 "still broken since"，
// 事件名就不再出現在早報裡了（SPEC §8.3 第 3 條：一直重複同一條
// 就是在訓練人忽略它）。
//
// 這**不是**讓那個問題消失 —— 分工是這樣的：
//   - 早報回答「今天有什麼需要我動手」，所以會降權
//   - 詳細頁那個狀態機回答「現在這台身上還掛著什麼」，所以永遠紅著
//
// Ted 要的「永遠就能看到一些 error 在那裏」，是後者在扛。
func TestAStaleEventFindingIsDemotedNotRepeated(t *testing.T) {
	in := base()
	in.Machines = []Machine{{
		DisplayName: "sampleagent2", State: state.Degraded,
		Reason:     "openclaw-watcher.service 在過去 1 小時內回報了 baseline_hash_mismatch ×48",
		StateSince: now.Add(-30 * 24 * time.Hour),
		Findings: []state.Finding{{Kind: "workload", Severity: 3,
			Message: "openclaw-watcher.service 在過去 1 小時內回報了 baseline_hash_mismatch ×48"}},
	}}
	got := Render(in)
	if !strings.Contains(got, "still broken since") {
		t.Errorf("降級超過 %v 要收成一行，實際：\n%s", StaleAfter, got)
	}
	if strings.Contains(got, "×48") {
		t.Errorf("收成一行之後不該再重複細節，實際：\n%s", got)
	}
	// ⚠ 但它**不准消失**。收成一行 ≠ 不講。
	if !strings.Contains(got, "sampleagent2") {
		t.Errorf("降權不等於靜音，這台還是要出現，實際：\n%s", got)
	}
}
