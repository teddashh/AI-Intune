package main

import (
	"fmt"
	"log"
	"time"

	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Hub 對自己的日誌。理由在 schema.sql 的 hub_events 上面。
//
// ⚠ 這裡記的每一件事都只有 Hub 自己看得到：它什麼時候起來、對帳迴圈
// 有沒有被排到、主機時鐘有沒有比程序多走。它們不是健康判定（那是外部
// 死人之鐘的事），是**事後對帳的證據**——早報說「三台在同一秒失聯」的時候，
// 這裡要答得出「那一分鐘 Hub 在幹嘛」。2026-09-04 有兩次眨眼，一次是重啟、
// 另一次到今天都沒有解釋，因為當時沒有這份日誌。

// reconcileEvery 是對帳迴圈的節奏。⚠ 失聯判定的顆粒度也是它。
const reconcileEvery = 30 * time.Second

// clockJumpTolerance：牆上時鐘比單調時鐘多走超過這麼多，就記一筆。
// 主機睡著、程序被 SIGSTOP、或 NTP 把時鐘調了一大步，都長這樣。
const clockJumpTolerance = 30 * time.Second

type hubGap struct{ kind, detail string }

// hubLoopGaps 從一輪對帳的三個量，找出值得記下來的異常：
//
//	wall  這一輪跟上一輪之間，牆上時鐘走了多久
//	mono  同一段，程序的單調時鐘走了多久（time.Now().Sub 用的就是它）
//	took  這一輪 reconcile() 本身花了多久
//
// 三種各自獨立，可以同時成立：
//   - clock_jump      wall − mono 太大：程序沒在跑的時候，世界還在走
//   - loop_stall      mono − took 比節奏長太多：迴圈很久沒被排到（整個程序卡住）
//   - reconcile_slow  took 比節奏還長：對帳本身慢（通常是資料庫鎖住）
//
// ⚠ 拆成純函式是為了讓它可以被測。時間相關的判斷寫在迴圈裡就永遠測不到，
// 而測不到的門檻遲早會寫錯方向（PHASE1 §5.24 那一課）。
func hubLoopGaps(wall, mono, took time.Duration) []hubGap {
	var out []hubGap
	if d := wall - mono; d > clockJumpTolerance {
		out = append(out, hubGap{store.HubClockJump,
			fmt.Sprintf("clock_jump=%s", state.HumanDur(d))})
	}
	if idle := mono - took; idle > 3*reconcileEvery {
		out = append(out, hubGap{store.HubLoopStall,
			fmt.Sprintf("loop_stall=%s；interval=%s",
				state.HumanDur(idle), state.HumanDur(reconcileEvery))})
	}
	if took > reconcileEvery {
		out = append(out, hubGap{store.HubReconcileSlow,
			fmt.Sprintf("reconcile_duration=%s；interval=%s",
				state.HumanDur(took), state.HumanDur(reconcileEvery))})
	}
	return out
}

// journal 寫一筆到 hub_events。⚠ 寫不進去只記 log，不能讓 Hub 因為
// 寫不了自己的日誌而停下來 —— 那會讓「記不了」變成「不在了」。
func (h *hub) journal(kind, detail string, at time.Time) {
	if err := h.store.RecordHubEvent(kind, detail, at.UTC()); err != nil {
		log.Printf("failed to write hub journal (%s): %v", kind, err)
	}
}

// journalStart 記「我起來了」，並且說出上一次活著是多久前 ——
// 那個差就是這次不在的長度，早報的「同時失聯」要拿它來對。
func (h *hub) journalStart(now time.Time) {
	detail := fmt.Sprintf("%s started；", version)
	last, ok, err := h.store.LastHubAlive()
	switch {
	case err != nil:
		detail += "previous_alive unavailable：" + err.Error()
	case !ok:
		detail += "previous_alive none"
	default:
		detail += fmt.Sprintf("gap=%s；previous_alive=%s",
			state.HumanDur(now.Sub(last)), last.Local().Format("01-02 15:04:05"))
	}
	h.journal(store.HubStarted, detail, now)
}
