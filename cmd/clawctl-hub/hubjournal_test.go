package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func kinds(gs []hubGap) string {
	var ks []string
	for _, g := range gs {
		ks = append(ks, g.kind)
	}
	return strings.Join(ks, ",")
}

// 三種異常各自獨立、門檻方向要對。這是時間相關的判斷，只有純函式測得到。
func TestHubLoopGapsClassifiesEachAnomaly(t *testing.T) {
	const s = time.Second
	cases := []struct {
		name             string
		wall, mono, took time.Duration
		want             string
	}{
		{"正常一輪", 30 * s, 30 * s, 200 * time.Millisecond, ""},
		{"對帳慢一點但沒過節奏", 45 * s, 45 * s, 15 * s, ""},
		{"對帳本身超過節奏", 61 * s, 61 * s, 31 * s, store.HubReconcileSlow},
		{"迴圈 100 秒沒被排到（3×30 以上）", 100 * s, 100 * s, 1 * s, store.HubLoopStall},
		{"迴圈剛好 90 秒不算", 91 * s, 91 * s, 1 * s, ""},
		{"主機睡了 10 分鐘：牆上走了、程序沒走", 600 * s, 30 * s, 1 * s, store.HubClockJump},
		{"時鐘往回調：牆上比程序少走，不記", 0, 30 * s, 1 * s, ""},
		{"卡住又慢：兩筆都記", 200 * s, 200 * s, 40 * s, store.HubLoopStall + "," + store.HubReconcileSlow},
	}
	for _, c := range cases {
		if got := kinds(hubLoopGaps(c.wall, c.mono, c.took)); got != c.want {
			t.Errorf("%s：wall=%v mono=%v took=%v → %q，要 %q", c.name, c.wall, c.mono, c.took, got, c.want)
		}
	}
}

// 記下來的字要有數字：「卡住過」沒用，「4 分鐘沒被排到」才能對早報。
func TestHubLoopGapDetailCarriesTheDuration(t *testing.T) {
	gs := hubLoopGaps(4*time.Minute, 4*time.Minute, 0)
	if len(gs) != 1 || !strings.Contains(gs[0].detail, "4 分鐘") {
		t.Errorf("detail 沒有把長度講出來：%+v", gs)
	}
}

// journalStart 第一次跑要記 previous_alive none；第二次要記 gap。
func TestJournalStartReportsGapSinceLastAlive(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &hub{store: st}
	now := time.Date(2026, 9, 6, 3, 55, 48, 0, time.UTC)

	h.journalStart(now)
	evs, err := st.HubEventsBetween(now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || len(evs) != 1 || evs[0].Kind != store.HubStarted {
		t.Fatalf("第一次起來沒記到 started：%v %+v", err, evs)
	}
	if !strings.Contains(evs[0].Detail, "previous_alive none") {
		t.Errorf("第一次啟動缺少 previous_alive 狀態：%q", evs[0].Detail)
	}

	if err := st.TouchHubAlive(now.Add(-145 * time.Second)); err != nil {
		t.Fatal(err)
	}
	h.journalStart(now.Add(time.Second))
	evs, _ = st.HubEventsBetween(now.Add(-time.Minute), now.Add(time.Minute))
	if len(evs) != 2 || !strings.Contains(evs[1].Detail, "gap=2 分鐘") {
		t.Errorf("第二次啟動缺少 gap：%+v", evs)
	}
}
