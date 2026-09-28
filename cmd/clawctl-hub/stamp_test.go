package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// 這一組測的是外部死人之鐘的唯一輸入：那個時間戳檔。
//
// ⚠ 它們看起來很小題大作 —— 四個測試就為了一個寫檔。
// 但這個檔是整條監看鏈上唯一沒有人在看的環節：Hub 監看機隊，死人之鐘監看 Hub，
// 而死人之鐘只讀這一個數字。這個數字一旦在不該蓋章的時候被蓋，
// 整條鏈就變成裝飾品，而且是安靜地變成裝飾品 —— 沒有任何告警會告訴你這件事。
//
// 這正好是 docs/PHASE1.md 記下的那一課：bug 住在零件之間。

func newHub(t *testing.T, stamp string) *hub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatalf("開不了測試資料庫：%v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &hub{store: st, reportStamp: stamp}
}

func readStamp(t *testing.T, path string) (int64, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("時間戳的內容不是一個 unix 時間：%q —— deadman.sh 會把它當成告警", b)
	}
	return n, true
}

func TestStampIsWrittenOnlyAfterTheReportActuallyLands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.stamp")
	h := newHub(t, path)
	h.notifyCmd = "cat >/dev/null" // 收下了
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)

	h.deliver("daily", "4/5 報到\nalive; 0 changes", now)

	got, ok := readStamp(t, path)
	if !ok {
		t.Fatal("早報送達了，但沒有蓋章 —— 死人之鐘明天會誤報 Hub 死掉")
	}
	if got != now.Unix() {
		t.Fatalf("蓋的時間不對：got %d want %d", got, now.Unix())
	}
}

// ⚠ 這是這一組裡最重要的一個。
//
// 推播管道壞掉（token 被撤銷、網路不通、腳本被刪）是死人之鐘存在的主要理由。
// 如果送失敗還照樣蓋章，那麼 operator 會在完全沒有收到早報的情況下，
// 有一個一直顯示綠燈的死人之鐘。那比沒有死人之鐘更糟 ——
// 沒有的時候他至少知道自己沒有。
func TestFailedDeliveryLeavesNoStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.stamp")
	h := newHub(t, path)
	h.notifyCmd = "exit 1" // 送不出去
	now := time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)

	h.deliver("daily", "4/5 報到", now)

	if _, ok := readStamp(t, path); ok {
		t.Fatal("推播失敗卻蓋了章 —— 死人之鐘會在早報完全沒送出去的日子維持綠燈")
	}
}

// 沒設 notify-cmd 時 deliver 只印到 stdout，那不算送達。
// 一個「印到 log 就當送到」的 Hub 會讓死人之鐘在推播根本沒接上的環境裡永遠綠燈。
func TestPrintingToTheLogIsNotDelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.stamp")
	h := newHub(t, path) // notifyCmd 是空的
	h.deliver("daily", "4/5 報到", time.Now().UTC())

	if _, ok := readStamp(t, path); ok {
		t.Fatal("沒有設定推播卻蓋了章 —— log 裡有一行不代表有人收到")
	}
}

// ⚠ 只有 daily 能蓋章。
//
// 未來一定會加即時告警（憑證今天過期、某台剛剛失聯）。那些通知走同一個
// deliver()，如果它們也蓋章，那麼一則半夜兩點的告警會把時間戳刷新，
// 而隔天早上「早報根本沒送出去」這件事就被蓋掉了。
// 死人之鐘看的是「每天那一則」，不是「有沒有任何東西送出去過」。
func TestOnlyTheDailyReportStamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.stamp")
	h := newHub(t, path)
	h.notifyCmd = "cat >/dev/null"

	h.deliver("alert", "sampleagent3 剛剛失聯", time.Now().UTC())

	if _, ok := readStamp(t, path); ok {
		t.Fatal("臨時告警蓋了章 —— 它會遮住隔天早報沒送出去這件事")
	}
}

// 沒設 report-stamp 的部署不該炸掉，也不該留下半個暫存檔。
func TestNoStampConfiguredIsFine(t *testing.T) {
	dir := t.TempDir()
	h := newHub(t, "")
	h.notifyCmd = "cat >/dev/null"
	h.deliver("daily", "alive; 0 changes", time.Now().UTC())

	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("沒設定時間戳卻寫了東西出來：%v", ents)
	}
}
