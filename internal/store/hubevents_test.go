package store

import (
	"testing"
	"time"
)

// Hub 自己的日誌：記得進去、按時間讀得出來、窗口外的不混進來。
func TestHubEventsRoundTripInOrder(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 6, 3, 50, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// 故意亂序寫入：讀出來要照 at 排，不是照寫入順序。
	must(s.RecordHubEvent(HubStarted, "起來了", base.Add(5*time.Minute)))
	must(s.RecordHubEvent(HubStopping, "停了", base.Add(3*time.Minute)))
	must(s.RecordHubEvent(HubLoopStall, "太早，窗口外", base.Add(-2*time.Hour)))
	must(s.RecordHubEvent(HubClockJump, "太晚，窗口外", base.Add(3*time.Hour)))

	got, err := s.HubEventsBetween(base, base.Add(time.Hour))
	must(err)
	if len(got) != 2 {
		t.Fatalf("窗口內應該剛好 2 筆，拿到 %d：%+v", len(got), got)
	}
	if got[0].Kind != HubStopping || got[1].Kind != HubStarted {
		t.Errorf("沒有照時間排：%+v", got)
	}
	if !got[1].At.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("時間讀回來走樣了：%v", got[1].At)
	}
	if got[0].Detail != "停了" {
		t.Errorf("detail 走樣：%q", got[0].Detail)
	}
}

// 「上一次活著」是一個章，蓋了就蓋掉舊的；新資料庫沒有章。
func TestHubAliveStampIsUpsertedNotAppended(t *testing.T) {
	s := newTestStore(t)
	if _, ok, err := s.LastHubAlive(); err != nil || ok {
		t.Fatalf("新資料庫不該有章：ok=%v err=%v", ok, err)
	}
	t1 := time.Date(2026, 9, 6, 3, 53, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)
	for _, tt := range []time.Time{t1, t2} {
		if err := s.TouchHubAlive(tt); err != nil {
			t.Fatal(err)
		}
	}
	last, ok, err := s.LastHubAlive()
	if err != nil || !ok {
		t.Fatalf("蓋了兩次章卻讀不到：ok=%v err=%v", ok, err)
	}
	if !last.Equal(t2) {
		t.Errorf("讀到的不是最後一次：%v（要 %v）", last, t2)
	}
	// schema_meta 只能有一列這個 key —— 這是 UPSERT 不是 INSERT。
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_meta WHERE key = ?`, hubAliveKey).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("章蓋出了 %d 列", n)
	}
}
