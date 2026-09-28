package state

import (
	"testing"
	"time"
)

// 一個「過期門檻」不可以等於「更新週期」。
//
// ⚠⚠ 這支測試在防的不是某一個數字，是一整類 bug ——
// 而這一類 bug 已經在這個檔案裡發生過一次，而且是在規則**已經被寫下來之後**：
//
//	// ⚠ 心跳間隔 = 失聯門檻會製造假紅。5 分鐘心跳配 5 分鐘門檻，
//	// 任何一次網路抖動都會亮紅燈，然後第一週人就把通知關掉了。
//	UnreachableGrace = 90 * time.Second
//	...
//	ObservationInterval = 10 * time.Minute
//	ObservationStale    = 10 * time.Minute   // ← 三行之後
//
// 註解是對的，隔壁的常數把它違反了。人寫得出那條規則、也讀得懂那條規則，
// 然後在同一個畫面裡又犯一次 —— 所以它需要一支測試，不是一段註解。
//
// 實測後果（2026-09-04，一天多的真實觀測間隔）：四台機器都有 27～38% 的
// 觀測週期超過 10 分鐘，samplehub1 一天在 Degraded / Online 之間跳 76 次。
func TestAStalenessThresholdIsNeverEqualToItsUpdateInterval(t *testing.T) {
	for _, c := range []struct {
		what     string
		interval time.Duration
		stale    time.Duration
	}{
		{"觀測", ObservationInterval, ObservationStale},
		{"心跳", CheckinInterval, CheckinInterval + UnreachableGrace},
	} {
		if c.stale <= c.interval {
			t.Errorf("%s：門檻 %v ≤ 更新週期 %v —— "+
				"每一份資料都會在下一份到達前的那一瞬間過期，這是保證會震盪的設計",
				c.what, c.stale, c.interval)
		}
	}
}

// 而且餘裕不能只有一點點 —— 抖動是常態，不是例外。
//
// ⚠ 這一條是上面那條的「不要用另一個數字繼續震盪」版本。
// 把 ObservationStale 從 10 分改成 11.5 分（= 週期 + UnreachableGrace）
// 會通過上面那條，但實測 p90 是 11.3 分 —— 它會照樣每天震盪好幾十次。
//
// 25% 這個數字本身不神聖，它只是「明顯大於觀測到的抖動」的一個下限。
// 真正的依據是實測分布：中位 8.4～9.3 分、p90 11.3～11.5 分、最大 16.25 分。
func TestStalenessThresholdsLeaveRoomForRealJitter(t *testing.T) {
	const minMargin = 0.25 // 至少比更新週期多 25%

	// 實測到的最大觀測間隔（sampleagent4，2026-09-04）。門檻必須高過它，
	// 否則我們知道它會誤判，卻還是把它裝上去了。
	const observedWorstGap = 16*time.Minute + 15*time.Second

	if got := float64(ObservationStale-ObservationInterval) / float64(ObservationInterval); got < minMargin {
		t.Errorf("觀測門檻只比週期多 %.0f%%，至少要 %.0f%% —— "+
			"實測 p90 就已經超過週期了，餘裕太薄等於換個數字繼續震盪",
			got*100, minMargin*100)
	}
	if ObservationStale <= observedWorstGap {
		t.Errorf("觀測門檻 %v 沒有高過實測最大間隔 %v —— "+
			"這代表我們**知道**它會誤判，卻還是裝上去了",
			ObservationStale, observedWorstGap)
	}
}

// 判決本身：一份剛好在「週期到 p90 之間」抵達的觀測，不可以被判成過期。
//
// 上面兩條守常數，這一條守**行為** —— 常數對了但比較寫成 `>=`
// 或是拿錯欄位去減，一樣會震盪。
func TestAnObservationArrivingALittleLateIsNotCalledStale(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	for _, age := range []time.Duration{
		ObservationInterval,                  // 剛好一個週期
		ObservationInterval + 90*time.Second, // 心跳那條的餘裕
		11*time.Minute + 30*time.Second,      // 實測 p90
		16*time.Minute + 15*time.Second,      // 實測最大值
		ObservationStale,                     // 門檻本身：等於不算超過
	} {
		f := Facts{
			Now:             now,
			HasObservation:  true,
			LastObservation: now.Add(-age),
		}
		if isObservationStale(f) {
			t.Errorf("觀測晚了 %v 就被判成過期（門檻 %v）—— "+
				"實測有 27～38%% 的週期會落在這個區間，這會變成每天幾十次的假黃燈",
				age, ObservationStale)
		}
	}

	// 反面：真的漏掉一整個週期，必須抓得到。
	// 沒有這一條的話，把門檻設成一年也會讓上面全部變綠。
	f := Facts{
		Now:             now,
		HasObservation:  true,
		LastObservation: now.Add(-(ObservationStale + time.Minute)),
	}
	if !isObservationStale(f) {
		t.Errorf("漏掉一整個週期（%v）卻沒被判成過期 —— "+
			"那這個門檻等於不存在", ObservationStale+time.Minute)
	}
}
