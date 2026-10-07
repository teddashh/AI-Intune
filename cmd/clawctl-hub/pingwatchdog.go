package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// 外部死人之鐘的 ping（docs/SPEC.md §7.1：healthchecks.io 或等價物）。
//
// ⚠ 為什麼還要一個 —— 已經有 ops/deadman.sh 了：
// deadman.sh 住在 sampleagent2，它擋得住 samplehub1 掛掉，但擋不住 sampleagent2 自己掛掉，
// 也擋不住整個機隊一起掛掉（停電、網路、Oracle Cloud 出事）。
// healthchecks 這一條**完全在機隊之外**，那是它唯一的價值。
// 兩條不是重複，是不同的失效面。
//
// ⚠⚠ ping 的語意必須跟 report-stamp 一模一樣：
// **只有 daily、而且只有推播真的送達之後**才 ping。
//
// 差一點就會寫成「reportLoop 跑到就 ping」——那樣 ping 的意思會變成
// 「Hub 的迴圈還在轉」，於是 token 被撤銷、notify-cmd 壞掉、Telegram 停用
// 這三種情況下，死人之鐘全部維持綠燈，而你連續好幾天收不到早報。
//
// 相容：healthchecks.io、自架 healthchecks、Uptime Kuma push、cronitor ——
// 它們的介面都是「定期打這個 URL」。

// pinger 是給測試換掉的。
var pinger = func(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "clawctl-hub")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode >= 300 {
		return &pingError{code: resp.StatusCode}
	}
	return nil
}

type pingError struct{ code int }

func (e *pingError) Error() string { return http.StatusText(e.code) }

// failPingURL 推導「失敗」那個端點。
//
// healthchecks 的慣例是 <url>/fail。⚠ 但只有在 URL 沒有 query string 時才推 ——
// Uptime Kuma 的 push URL 長成 /api/push/<token>?status=up，在後面接 /fail
// 會變成一個不存在的路徑，而那個錯誤只會出現在 log 裡，
// 沒有人會發現「失敗告警」其實從來沒有送出去過。
// 推不出來就回空字串，呼叫端會講清楚為什麼沒送。
func failPingURL(base string) string {
	if base == "" || strings.Contains(base, "?") {
		return ""
	}
	return strings.TrimRight(base, "/") + "/fail"
}

// pingReportWatchdog 通知外部死人之鐘「今天的早報怎麼了」。
//
// ⚠ ping 失敗只記 log。早報送不送得出去，跟能不能通知外部服務是兩件事，
// 而且 ping 失敗本身就會讓死人之鐘超時告警 —— 那正是它該做的。
func (h *hub) pingReportWatchdog(kind string, delivered bool) {
	if h.reportPingURL == "" || kind != "daily" {
		return
	}
	url := h.reportPingURL
	if !delivered {
		url = failPingURL(h.reportPingURL)
		if url == "" {
			log.Printf("daily report was not delivered, but this ping URL contains a query string " +
				"and cannot derive a /fail endpoint -- dead man's switch can only time out on its own; " +
				"set CLAWCTL_REPORT_PING_FAIL_URL to alert immediately")
			return
		}
	}
	if h.reportPingFailURL != "" && !delivered {
		url = h.reportPingFailURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := pinger(ctx, url); err != nil {
		// ⚠ 不印 url —— 它本身就是憑證（誰知道這個 URL 誰就能讓死人之鐘閉嘴）。
		log.Printf("external dead man's switch ping failed: %v (daily report itself delivered=%v)", err, delivered)
		return
	}
	if delivered {
		log.Print("external dead man's switch received today's daily report ping")
	} else {
		log.Print("external dead man's switch received \"report delivery failure\" alert ping")
	}
}
