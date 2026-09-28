package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type pingLog struct {
	mu   sync.Mutex
	urls []string
	err  error
}

func (p *pingLog) install(t *testing.T) {
	t.Helper()
	old := pinger
	pinger = func(_ context.Context, url string) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.urls = append(p.urls, url)
		return p.err
	}
	t.Cleanup(func() { pinger = old })
}

func (p *pingLog) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.urls...)
}

// ⚠ 這是這一組裡最重要的一個。
//
// 差一點就會寫成「reportLoop 跑到就 ping」——那樣 ping 的意思會變成
// 「Hub 的迴圈還在轉」，於是 token 被撤銷、notify-cmd 壞掉、Telegram 停用
// 這三種情況下，死人之鐘全部維持綠燈，而你連續好幾天收不到早報。
//
// 跟 report-stamp 同一條規則：Hub 唯一有資格宣告的，是
// 「有一則早報離開了這台機器並且對方收下了」。
func TestSuccessPingOnlyWhenTheReportActuallyLanded(t *testing.T) {
	p := &pingLog{}
	p.install(t)
	h := newHub(t, "")
	h.reportPingURL = "https://hc.example/abc"

	// notify-cmd 失敗 = 早報沒送出去。
	h.notifyCmd = "exit 3"
	h.deliver("daily", "早報內容", time.Now())

	got := p.got()
	if len(got) != 1 {
		t.Fatalf("ping 次數 = %d，want 1（失敗也要通知）：%v", len(got), got)
	}
	if !strings.HasSuffix(got[0], "/fail") {
		t.Errorf("早報沒送出去卻 ping 了成功端點 %q —— "+
			"死人之鐘會維持綠燈，而你連續好幾天收不到早報", got[0])
	}
}

func TestDeliveredReportPingsTheSuccessEndpoint(t *testing.T) {
	p := &pingLog{}
	p.install(t)
	h := newHub(t, "")
	h.reportPingURL = "https://hc.example/abc"
	h.notifyCmd = "cat > /dev/null"

	h.deliver("daily", "早報內容", time.Now())

	got := p.got()
	if len(got) != 1 || got[0] != "https://hc.example/abc" {
		t.Fatalf("送達之後應該 ping 成功端點一次，得到 %v", got)
	}
}

// 只有 daily。臨時通知不准碰死人之鐘 ——
// 一則半夜的告警會把死人之鐘餵飽，而隔天早上早報根本沒送出去這件事就被蓋掉了。
func TestOnlyTheDailyReportFeedsTheDeadMan(t *testing.T) {
	p := &pingLog{}
	p.install(t)
	h := newHub(t, "")
	h.reportPingURL = "https://hc.example/abc"
	h.notifyCmd = "cat > /dev/null"

	h.deliver("alert", "半夜的臨時告警", time.Now())

	if got := p.got(); len(got) != 0 {
		t.Errorf("臨時通知餵了死人之鐘 %v —— 隔天早報沒送出去就會被蓋掉", got)
	}
}

// ⚠ Uptime Kuma 的 push URL 長成 /api/push/<token>?status=up。
// 在後面接 /fail 會變成一個不存在的路徑，而那個錯誤只會出現在 log 裡，
// 沒有人會發現「失敗告警」其實從來沒有送出去過。
func TestQueryStringURLDoesNotGetAFabricatedFailEndpoint(t *testing.T) {
	if got := failPingURL("https://kuma.example/api/push/tok?status=up"); got != "" {
		t.Errorf("替帶 query string 的 URL 掰了一個失敗端點 %q", got)
	}
	if got := failPingURL("https://hc.example/abc/"); got != "https://hc.example/abc/fail" {
		t.Errorf("healthchecks 慣例推導錯了：%q", got)
	}

	p := &pingLog{}
	p.install(t)
	h := newHub(t, "")
	h.reportPingURL = "https://kuma.example/api/push/tok?status=up"
	h.notifyCmd = "exit 3"

	h.deliver("daily", "早報內容", time.Now())
	if got := p.got(); len(got) != 0 {
		t.Errorf("推不出失敗端點時不該亂送：%v", got)
	}
}

// ping 失敗不准影響早報的紀錄。早報送到了就是送到了。
func TestAFailedPingDoesNotRewriteHistory(t *testing.T) {
	p := &pingLog{err: errors.New("connection refused")}
	p.install(t)
	h := newHub(t, "")
	h.reportPingURL = "https://hc.example/abc"
	h.notifyCmd = "cat > /dev/null"
	now := time.Now()

	h.deliver("daily", "早報內容", now)

	at, ok, err := h.store.LastNotification("daily")
	if err != nil {
		t.Fatalf("notifications: %v", err)
	}
	if !ok {
		t.Fatal("ping 失敗把「早報已送達」這件事抹掉了 —— 早報明明送到了")
	}
	if at.Before(now.Add(-time.Minute)) {
		t.Errorf("記到的送達時間不對：%v", at)
	}
}

// 沒設 URL 就完全不做事（大多數人不會設）。
func TestNoPingURLConfiguredIsFine(t *testing.T) {
	p := &pingLog{}
	p.install(t)
	h := newHub(t, "")
	h.notifyCmd = "cat > /dev/null"
	h.deliver("daily", "早報內容", time.Now())
	if got := p.got(); len(got) != 0 {
		t.Errorf("沒設 URL 卻送了 ping：%v", got)
	}
}
