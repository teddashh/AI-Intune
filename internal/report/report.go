// Package report 產生每日推播。
//
// 這是這個產品實際的介面。網頁是下鑽用的，早報才是每天會被讀的東西 ——
// 所以「連續 14 天沒開網頁」不算荒廢，「早報看不懂或不準」才算。
//
// 三條硬規則（docs/SPEC.md §8.3）：
//
//  1. 每天固定送一則。沒變化也要送 `alive; 0 changes` ——
//     因為「沒收到訊息」必須能明確代表「Hub 死了」，而不是「今天沒事」。
//  2. 最多 MaxLines 行。超過的寫 `+N more in hub`。
//     黃燈太多，人就會把通知靜音，然後這個產品就死了。
//  3. 同一條壞了超過 7 天，改寫成 `still broken since ...`，
//     ⚠ 不是再加一盞燈。一直重複同一條就是在訓練人忽略它。
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/state"
)

// MaxLines 是早報的行數上限。
//
// 5 是刻意的小。這不是「顯示前 5 名」，這是「一個人在早上刷牙時
// 能讀完並且真的會讀」的行數。
const MaxLines = 5

// StaleAfter：同一個問題持續超過這麼久，改用 "still broken since" 的講法。
const StaleAfter = 7 * 24 * time.Hour

// Input 是產生早報需要的一切。刻意跟 store 解耦 —— 早報的措辭是產品決策，
// 不該被資料層的形狀綁住。
type Input struct {
	Now time.Time
	// Since 是上一則早報的時間。變化以此為基準。
	Since time.Time

	Machines []Machine
	// FleetCounts 是名冊分母的切片，key 是 state。
	FleetCounts map[state.State]int
	// Expected 是名冊上算在分母裡的總數。⚠ 這是分母，不是「有回報的機器數」。
	Expected int
	// Reporting 是 Hub freshness window 內真的有 heartbeat 的分母內機器數。
	// 不可從 state 重建：IdentityConflict 是 durable verdict，可能比心跳活得久。
	Reporting      int
	ReportingKnown bool

	// BaseURL 是 Hub 對外的網址（例如 http://100.x.y.z:8787），沒有尾斜線。
	//
	// ⚠ 空字串的意思是「推不出一個別台連得到的位址」，而那時候**不放連結**。
	// 一個點下去是 127.0.0.1 的連結比沒有連結更糟：它在手機上一定失敗，
	// 而人會學到「早報的連結是壞的」，然後連之後修好的也不點了。
	// 推不出來的理由要在 Hub 開機的 log 裡講出來，不是靜靜地少一行。
	BaseURL string

	// HubEvents 是 Hub 對自己的日誌（重啟、迴圈卡住、時鐘跳動）。
	// 「同時失聯」那一行拿它來說出成因，而不是只說「通常是 Hub」。
	// ⚠ 空的意思是「Hub 沒記到什麼」——不是「Hub 沒問題」。措辭要跟著這樣寫。
	HubEvents []HubEvent

	// RestoreDrill：上一次還原演練。PHASES D 表：「90 天沒做還原演練，不敢動 Hub」。
	RestoreDrill RestoreDrill
}

// RestoreDrill 是還原演練的章。
//
//	Tracked=false  Hub 沒在看章（測試、或沒設定）—— 早報不講
//	Done=false     從來沒做過 —— 這比「太久沒做」更要講：從來沒有人證明過備份打得開
//	At             上一次通過的時刻
type RestoreDrill struct {
	Tracked bool
	Done    bool
	At      time.Time
}

// RestoreDrillEvery 是多久要演練一次。跟 cmd/clawctl-hub 的 restoreDrillEvery 同值；
// 早報這邊自己留一份是因為早報刻意不 import main。
const RestoreDrillEvery = 90 * 24 * time.Hour

// HubEvent 是 store.HubEvent 的鏡像。早報刻意不 import store。
type HubEvent struct {
	At     time.Time
	Kind   string
	Detail string
}

type Machine struct {
	ID          string
	DisplayName string
	State       state.State
	Reason      string
	// StateSince：進入這個狀態的時間。用來判斷是不是該講 "still broken since"。
	StateSince time.Time
	// UnreachableAt：這一輪窗口裡，每一次**進入失聯**的時刻。
	//
	// ⚠⚠ 不要拿 StateSince 當這個用。這正是 §5.24 續集裡那個 bug：
	// StateSince 講的是「現在這個狀態什麼時候開始的」，只有在「失聯是這台
	// 機器最近發生的最後一件事」時，它才剛好等於失聯時刻。實機資料裡
	// sampleagent4 的 StateSince 指著 00:46 那次眨眼，sampleagent2 的指著 08:17 那次
	// —— 兩台在 00:46 明明是**同秒**一起失聯的，中間差了七個半小時。
	//
	// 我當時寫的九支測試全綠，因為每一個 fixture 都被我寫成「失聯是最後
	// 一件事」。那不是驗證，是拿自己造的世界去對自己寫的程式。
	UnreachableAt []time.Time
	// Changed：這個狀態是不是在 Since 之後才變成現在這樣。
	Changed bool
	// PreviousState：Changed 為 true 時的前一個狀態。
	PreviousState state.State
	Findings      []state.Finding
	// CredExpiries：即將或已經過期的憑證。
	CredExpiries []CredExpiry
}

type CredExpiry struct {
	Provider  string
	Status    string
	ExpiresAt *time.Time
	Peers     []state.CredPeer
}

// line 是候選的一行，帶著排序用的權重。
type line struct {
	text string
	// rank 越小越前面。見 Render 裡的排序理由。
	rank int
	// tiebreak 用來讓同 rank 的行有穩定順序（否則每天早報的順序會亂跳，
	// 人會以為情況變了）。
	tiebreak string
}

// Render 產生早報全文。
//
// 排序規則（這回答了 OPEN-QUESTIONS 的 Q8）：
//
//  1. 新出現的變化 —— 昨天還好、今天壞了。這是唯一「今天才需要你」的東西
//  2. 憑證已過期或即將過期 —— 有明確的截止時間，晚一天處理成本就變高
//  3. 沉默失敗與從未跑完 —— 最容易被忽略，因為它不會自己喊
//  4. 其他 Degraded
//  5. 已經壞很久的 —— 降權，而且改成 "still broken since"，不重複刷版面
//
// 為什麼「已經壞很久的」排最後：它昨天沒被處理，今天多半也不會，
// 而它擠掉的是那些今天才出現、現在處理最便宜的東西。
func Render(in Input) string {
	var b strings.Builder

	// --- 第一行永遠是分母。這是死人之鐘的心跳，也是「名冊是分母」的體現。
	b.WriteString(header(in))

	lines := collect(in)
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].rank != lines[j].rank {
			return lines[i].rank < lines[j].rank
		}
		return lines[i].tiebreak < lines[j].tiebreak
	})

	if len(lines) == 0 {
		// ⚠ 這一行是整個產品的死人之鐘。
		// 沒事也要說話，否則「沒收到」就同時代表「今天沒事」與「Hub 死了」，
		// 而那兩件事需要完全相反的反應。
		b.WriteString("\nalive; 0 changes\n")
		return b.String()
	}

	b.WriteString("\n")
	shown := lines
	if len(shown) > MaxLines {
		shown = shown[:MaxLines]
	}
	for _, l := range shown {
		b.WriteString(l.text)
		b.WriteString("\n")
	}
	if n := len(lines) - len(shown); n > 0 {
		fmt.Fprintf(&b, "+%d more in hub\n", n)
	}
	// ⚠ 一行，放在最後，而且只有在有話要說的時候才放。
	//
	// 這一行是 PHASES.md「≤ 兩次點擊 + 一次 Connect」的第一次點擊：
	// 點它到總覽 → 點機器名字到詳細頁 → 按 Connect。
	//
	// 為什麼不是每一行後面各掛一個連結（那樣只要一次點擊）：早報上限是
	// 五行，而那個上限的單位是「人在刷牙時讀得完的行數」。每行後面加一個
	// 六十字元的網址，五行會變成十行，其中一半是同一個網域的雜訊 ——
	// 省下的那一次點擊，代價是這則訊息不再被讀完。
	//
	// 「沒事」的那一天不放連結：`alive; 0 changes` 沒有東西可以點過去，
	// 而每天都出現的連結會退化成版面裝飾。
	if in.BaseURL != "" {
		fmt.Fprintf(&b, "→ %s/\n", in.BaseURL)
	}
	return b.String()
}

func header(in Input) string {
	c := in.FleetCounts
	var parts []string
	if in.ReportingKnown {
		parts = append(parts, fmt.Sprintf("%d/%d 報到", in.Reporting, in.Expected))
	} else {
		parts = append(parts, fmt.Sprintf("?/%d 報到（freshness 不明）", in.Expected))
	}
	for _, s := range []state.State{state.Degraded, state.Unreachable, state.NeverReported, state.IdentityConflict} {
		if c[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", labelOf(s), c[s]))
		}
	}
	return strings.Join(parts, " · ")
}

func labelOf(s state.State) string {
	switch s {
	case state.Online:
		return "在線"
	case state.Degraded:
		return "降級"
	case state.Unreachable:
		return "失聯"
	case state.NeverReported:
		return "從未報到"
	case state.IdentityConflict:
		return "身分衝突"
	}
	return string(s)
}

// withinPhrase 產生「在……之內」這一整句，連前面的「在」一起。
//
// ⚠ 連「在」一起回傳，不是為了方便，是因為空格規則跟後面的內容有關：
// 中文夾阿拉伯數字要空格（「在 2 分鐘內」），夾中文不要（「在同一秒」）。
// 拆成 `"在%s"` + humanSpan 的話，這個規則就沒有地方可以住，
// 而它已經被寫錯過一次（「在 3 分鐘 內」）。
//
// ⚠ 0 秒要講成「同一秒」而不是「0 分鐘」——「在 0 分鐘內一起失聯」讀起來
// 像是程式漏填了一個數字，而那正好是最有說服力的那個情況（真的同時）。
func withinPhrase(d time.Duration) string {
	switch {
	case d < time.Second:
		return "在同一秒"
	case d < time.Minute:
		return fmt.Sprintf("在 %d 秒內", int(d.Seconds()))
	default:
		// ⚠ 有餘數才進位，不是無條件 +1。
		// 無條件 +1 會把「剛好 60 秒」講成「2 分鐘內」——那句話沒有錯，
		// 但它比事實鬆，而這一行的全部價值就在於「這幾台幾乎是同時的」。
		// 把證據講得比實際弱，等於自己削弱自己的論點。
		mins := d / time.Minute
		if d%time.Minute != 0 {
			mins++
		}
		return fmt.Sprintf("在 %d 分鐘內", mins)
	}
}

// clockPhrase 把一個時刻講成早報裡讀得懂的樣子（本地時區）。
//
// ⚠⚠ 光印「20:46 起」是不夠的。早報是早上八點送的，窗口往回 24 小時 ——
// 所以任何比現在晚的鐘點都是**昨天**的，而那是窗口的一半。實機第一次
// 印出來就是「20:46 起」，指的是前一天晚上，字面上沒有任何東西這樣說。
//
// 一個看起來精確、實際上少了一半資訊的時刻，比沒有時刻更糟：
// 它會讓人拿著錯的時間去翻 log。
func clockPhrase(t, now time.Time) string {
	t = t.In(now.Location())
	switch d := dayDiff(now, t); {
	case d == 0:
		return t.Format("15:04")
	case d == 1:
		return "昨天 " + t.Format("15:04")
	default:
		// 窗口可以用 --since 調長，所以還是要有一個「更久以前」的講法。
		return t.Format("1/2 15:04")
	}
}

// dayDiff 回傳 a 的日期比 b 的日期晚幾天（兩者都已經在同一時區）。
func dayDiff(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return int(time.Date(ay, am, ad, 0, 0, 0, 0, a.Location()).
		Sub(time.Date(by, bm, bd, 0, 0, 0, 0, a.Location())).Hours() / 24)
}

// SimultaneityWindow：多台機器的失聯轉換落在這段時間內，就當成「同一件事」。
//
// 5 分鐘是量出來的：實測那三次同時失聯，最外面兩台差 90 秒
// （samplehub1 08:15:45、sampleagent2 08:16:45），而 reconcileLoop 的顆粒度是 30 秒、
// 心跳間隔 2 分鐘。5 分鐘容得下這些抖動，又短到不會把真的兩台先後壞掉
// 誤併成一件事。
const SimultaneityWindow = 5 * time.Minute

// blink 是一次「觀測者眨眼」：一群機器在同一個窗口裡一起進入失聯。
type blink struct {
	at    time.Time
	names []string
	ids   []string
	span  time.Duration
}

// 一次失聯事件：哪一台、什麼時候進去的。
type outage struct {
	id, name string
	at       time.Time
}

// hubProbablyBlinked 找出「一起失聯」的那幾群機器。
//
// ⚠⚠ 這個函式在修的是一個真實的誤導（§5.24）：2026-09-04 的早報寫著
// 「sampleagent2 失聯 → 降級」「sampleagent4 失聯 → 降級」，而那兩筆的成因是 Hub
// 自己在 08:11 重啟。同一天 00:46 還有一次三台同秒失聯，那次 Hub 根本
// 沒有重啟 —— 它只是有幾分鐘沒在聽。
//
// **獨立的機器不會在同一分鐘一起壞掉。** 同時發生的「獨立」故障，
// 絕大多數時候講的是觀測者，不是被觀測的東西。
//
// 判準是比例而不是固定台數：2 台裡的 2 台是強證據，30 台裡的 2 台不是。
// 所以要求這一群至少占「曾報到、且目前仍在名冊分母內的機器」的一半，
// 而且至少 2 台。這是歷史 outage correlation 的候選分母，不是 header 的
// 當下 heartbeat freshness 分子。
//
// ⚠⚠ 它讀的是 UnreachableAt（真的失聯時刻），不是 StateSince。
// 第一版讀 StateSince，在實機上一次都沒有觸發過 —— 而它的九支測試全綠。
// 見 Machine.UnreachableAt 上面那段。
//
// ⚠ 回傳多群，不是一群。一天裡可以眨兩次眼（實機當天就是 00:46 跟 08:16），
// 把它們併成一件事會在時間上說謊。
//
// ⚠ 它**不隱藏**任何東西。名字全列，只是把 N 行改寫成 1 行、
// 並且把矛頭指向該先看的地方。一個把機器藏起來的早報就是這個產品要修的那個 bug。
func hubProbablyBlinked(ms []Machine, rosterSize int) []blink {
	var all []outage
	for _, m := range ms {
		for _, t := range m.UnreachableAt {
			all = append(all, outage{id: m.ID, name: m.DisplayName, at: t})
		}
	}
	// ⚠ 同秒失聯要有穩定的順序。實機上 sampleagent2 跟 sampleagent4 就是**同一秒**
	// 進入失聯的，而 sort.Slice 不保證相等元素的相對順序 —— 那會讓每天的
	// 早報在「sampleagent4、sampleagent2」跟「sampleagent2、sampleagent4」之間亂跳，
	// 而一則每天換句話說的早報，讀的人會以為機隊每天都在變。
	sort.Slice(all, func(i, j int) bool {
		if !all[i].at.Equal(all[j].at) {
			return all[i].at.Before(all[j].at)
		}
		return all[i].name < all[j].name
	})

	var out []blink
	used := map[int]bool{}
	for i := range all {
		if used[i] {
			continue
		}
		// 這個窗口裡有哪些**不同的機器**。
		// ⚠ 同一台機器在 5 分鐘內失聯兩次是它自己在抖，不是三台一起 ——
		// 所以按 id 去重，不是按事件數。
		seen := map[string]bool{}
		var grp []outage
		for j := i; j < len(all); j++ {
			if used[j] || all[j].at.Sub(all[i].at) > SimultaneityWindow {
				if all[j].at.Sub(all[i].at) > SimultaneityWindow {
					break
				}
				continue
			}
			if seen[all[j].id] {
				continue
			}
			seen[all[j].id] = true
			grp = append(grp, all[j])
		}
		if len(grp) < 2 || len(grp)*2 < rosterSize {
			continue
		}
		for j := i; j < len(all); j++ {
			if all[j].at.Sub(all[i].at) > SimultaneityWindow {
				break
			}
			used[j] = true
		}
		b := blink{at: grp[0].at, span: grp[len(grp)-1].at.Sub(grp[0].at)}
		for _, o := range grp {
			b.names = append(b.names, o.name)
			b.ids = append(b.ids, o.id)
		}
		out = append(out, b)
	}
	return out
}

// blinkCauseWindow：眨眼時刻前後多久內的 Hub 事件算數。
//
// 失聯轉換是 Hub **恢復對帳之後**才寫下的（Hub 不在的時候什麼都寫不了），
// 所以 started / loop_stall 那一筆通常在眨眼時刻**之前**幾十秒；
// 往前拉 10 分鐘，往後留 5 分鐘給對帳顆粒度。
const blinkCauseWindow = 10 * time.Minute

// blinkCause 從 Hub 自己的日誌裡找這次眨眼的成因。
//
// ⚠ 找不到的時候要說「找不到」，不能說「Hub 沒問題」——
// 日誌只記得到它自己看得到的那幾種事，網路斷線它就看不到。
// 2026-09-04 00:46 那次到現在都沒有解釋，正是因為當時連這份日誌都沒有。
func blinkCause(b blink, evs []HubEvent, now time.Time) string {
	// 優先序：重啟 > 迴圈卡住 > 時鐘跳 > 對帳慢。前面的解釋力比後面強。
	rank := map[string]int{"started": 0, "loop_stall": 1, "clock_jump": 2, "reconcile_slow": 3}
	var best *HubEvent
	for i := range evs {
		e := &evs[i]
		r, known := rank[e.Kind]
		if !known {
			continue
		}
		if e.At.Before(b.at.Add(-blinkCauseWindow)) || e.At.After(b.at.Add(5*time.Minute)) {
			continue
		}
		if best == nil || r < rank[best.Kind] {
			best = e
		}
	}
	if best == nil {
		return "同時失聯通常是 Hub 自己沒在聽，但 Hub 這段時間沒記到自己重啟或卡住 —— 成因還沒有解釋，先查 Hub 的網路再查這幾台"
	}
	return fmt.Sprintf("Hub 自己沒在聽：%s %s", clockPhrase(best.At, now), best.Detail)
}

// restoreDrillLine：該催的時候催一行。
//
// rank 4（跟「還在降級」同一級）：它不是今天的事故，但它是「不敢動 Hub」的根。
// ⚠ 不放 rank 5：那一級會被五行上限擠掉，而一件 90 天沒做的事在忙的日子被擠掉，
// 就是它變成 180 天的方式。
func restoreDrillLine(in Input) (line, bool) {
	rd := in.RestoreDrill
	if !rd.Tracked {
		return line{}, false
	}
	if !rd.Done {
		return line{rank: 4, tiebreak: "\x00restore-drill", text: fmt.Sprintf(
			"restore drill overdue：尚無還原演練紀錄；週期 %s",
			state.HumanDur(RestoreDrillEvery))}, true
	}
	age := in.Now.Sub(rd.At)
	if age <= RestoreDrillEvery {
		return line{}, false
	}
	return line{rank: 4, tiebreak: "\x00restore-drill", text: fmt.Sprintf(
		"restore drill overdue：上一次還原演練是 %s前（%s）；週期 %s",
		state.HumanDur(age), rd.At.Local().Format("01-02"), state.HumanDur(RestoreDrillEvery))}, true
}

func collect(in Input) []line {
	var out []line
	if l, ok := restoreDrillLine(in); ok {
		out = append(out, l)
	}

	// ⚠ 從未報到的機器**先集合成一行**再說。
	//
	// 五台裡有四台沒報到時，逐台各寫一行會吃掉早報五行額度裡的四行，
	// 而那四行講的是同一件事。一則五行裡有四行重複的早報，第三天就會被
	// 整則跳過 —— 那是這個產品最致命的失效模式（見 docs/PHASES.md 失效模式 D1）。
	//
	// 但名字不能省。「4 台沒報到」不能行動，「sampleagent1、sampleagent2 沒報到」可以。
	// 所以是合併成一行、名字全列，不是只給一個數字。
	// ⚠ 先問「這些失聯是不是同一件事」，再逐台講話。
	//
	// 同時發生的獨立故障通常不是獨立故障 —— 它講的是觀測者（§5.24）。
	// 這一段把 N 行「X 失聯 → 降級」改寫成 1 行，名字全列，
	// 並把矛頭指向該先看的地方。省下來的行數還給真正的壞消息。
	// ⚠⚠ 這個分母跟 header() 的 `Input.Reporting` **不是同一個東西**，不要合併。
	//
	// header 的 Reporting 由 state.IsReportingAt 判斷：EverCheckedIn 且 Hub 的
	// `at` 尚未嚴格晚於 LastCheckinReceived + effective interval + grace；等號
	// 仍算 reporting。這裡則是歷史 outage correlation 的候選名冊，所以已失聯的
	// 機器必須留在分母裡：它們正是被指控的那一群。
	//
	// 用 header 的定義會壞在中間地帶：10 台裡有 4 台同時黑掉時，分子漲、
	// 分母同時縮到 6，4*2 >= 6 成立 —— 於是 4/10 被當成全機隊事件。
	// 分子在分母裡的時候，比例就不是比例了。
	//
	// 也不能直接用 in.Expected：NeverReported 是目前名冊分母內、但從未報到的
	// 機器；它沒有歷史 outage 可供相關。退役機器已在 buildReport 進來前排除。
	// 因此這裡只數曾報到的候選機器。
	rosterSize := 0
	for _, m := range in.Machines {
		if m.State != state.NeverReported {
			rosterSize++
		}
	}
	nowUnreachable := map[string]bool{}
	for _, m := range in.Machines {
		if m.State == state.Unreachable {
			nowUnreachable[m.ID] = true
		}
	}
	blinked := map[string]bool{}
	for bi, b := range hubProbablyBlinked(in.Machines, rosterSize) {
		var stillDown []string
		for i, id := range b.ids {
			blinked[id] = true
			if nowUnreachable[id] {
				stillDown = append(stillDown, b.names[i])
			}
		}
		// ⚠⚠ 這一段的第一版寫死了「一起失聯又恢復」。
		//
		// 那句話對「已經回來了」的情況是對的，對「現在還是黑的」就是**謊話**
		// —— 而且是最糟的那一種：它告訴讀的人事情過去了，而機隊正在黑著。
		// 實測那一版的輸出是：
		//
		//	0/4 報到
		//	samplehub1、sampleagent2、sampleagent4 在 2 分鐘內一起失聯又恢復 —— …
		//
		// 整則早報只有兩行，第一行說沒有半台在報到，第二行說已經恢復了。
		// 一個只在「事情已經結束」時才成立的措辭，被用在了一個
		// 「事情正在發生」也會走到的路徑上。
		what := "一起失聯又恢復"
		switch {
		case len(stillDown) == len(b.ids):
			what = "一起失聯，到現在都還連不上"
		case len(stillDown) > 0:
			what = fmt.Sprintf("一起失聯，其中 %s 到現在還連不上",
				strings.Join(stillDown, "、"))
		}
		// ⚠ 時刻要印出來。一天可以眨兩次眼（實機當天就是 00:46 跟 08:16），
		// 沒有時刻的話兩行長得一模一樣，讀的人會以為早報自己在重複。
		at := clockPhrase(b.at, in.Now)
		// tiebreak 的 \x00 前綴讓這幾行排在同 rank 的最前面 —— 排在「新的
		// 死亡」之前。理由不是它比較嚴重，是它比較**根本**：如果 Hub 那幾分鐘
		// 沒在聽，這則早報後面每一行都是它沒在聽的時候記下來的，先讀到這句話
		// 的人才知道要用什麼態度讀下面。
		out = append(out, line{
			rank: 1, tiebreak: fmt.Sprintf("\x00blink%02d", bi),
			text: fmt.Sprintf(
				"%s %s（%s 起）%s —— %s",
				strings.Join(b.names, "、"), withinPhrase(b.span), at, what,
				blinkCause(b, in.HubEvents, in.Now)),
		})
	}

	rest := in.Machines[:0:0]
	var deadNew, deadOld []Machine
	for _, m := range in.Machines {
		if m.State != state.NeverReported {
			rest = append(rest, m)
			continue
		}
		if in.Now.Sub(m.StateSince) > StaleAfter && !m.Changed {
			deadOld = append(deadOld, m)
		} else {
			deadNew = append(deadNew, m)
		}
	}
	if len(deadNew) > 0 {
		out = append(out, line{
			rank: 1, tiebreak: "\x00dead-new",
			text: fmt.Sprintf("%s 在名冊上但從未報到", nameList(deadNew)),
		})
	}
	if len(deadOld) > 0 {
		out = append(out, line{
			rank: 5, tiebreak: "\x00dead-old",
			text: fmt.Sprintf("%s still never reported since %s",
				nameList(deadOld), earliest(deadOld).Format("Jan 2")),
		})
	}

	for _, m := range rest {
		age := in.Now.Sub(m.StateSince)

		// --- 已經壞很久：一行帶過，降到最後，不重複刷細節。
		if m.State != state.Online && !m.Changed && age > StaleAfter {
			out = append(out, line{
				rank:     5,
				tiebreak: m.DisplayName,
				text: fmt.Sprintf("%s still broken since %s（%s）",
					m.DisplayName, m.StateSince.Format("Jan 2"), labelOf(m.State)),
			})
			continue
		}

		// --- 新出現的變化：最高優先。
		//
		// ⚠ PreviousState 是空的代表「這台之前沒有狀態」—— 它是新報到的，
		// 不是從別的狀態轉過來的。這兩件事在早報裡要講不同的話：
		// 把第一次上線寫成「恢復」，會讓人以為昨天有一台壞掉而他沒發現。
		// （這是看著真機的輸出改的：原本會印出「samplehub1 恢復（）」，
		// 一個空括號 —— 空括號是這種 bug 的招牌。）
		// ⚠ PreviousState 是 NeverReported 跟是空字串，意思一樣：這台是第一次出現。
		//
		// NeverReported 的定義是「在名冊上但從來沒有成功 check-in 過」，所以一台機器
		// 一旦離開 NeverReported 就再也回不去。NeverReported → X 永遠代表「它終於報到了」。
		//
		// 這一條是看真的輸出改的：原本印出「sampleagent2 從未報到 → 降級」，
		// 字面上完全正確，但讀起來像在說它「現在沒報到」——
		// 而那跟事實正好相反，它剛剛才第一次講話。
		newcomer := m.PreviousState == "" || m.PreviousState == state.NeverReported
		// ⚠⚠ 這一台的「失聯」上面已經算在某一次 Hub 眨眼裡了。
		//
		// 第一版寫的是 `if blinked[m.ID] { continue }` —— 整台跳過。那是過頭了：
		// sampleagent2 的 grok 憑證是真的過期了，那件事跟 Hub 有沒有在聽無關，
		// 而跳過它等於**因為觀測者出過包，就把一件真的壞消息吞掉**。
		//
		// 誤導人的從來不是「sampleagent2 降級」，是「sampleagent2 **失聯 →** 降級」——
		// 那個箭頭把 Hub 的失明講成了這台機器的病史。所以只拿掉箭頭，
		// 理由留著。
		fromBlink := blinked[m.ID] && m.PreviousState == state.Unreachable
		if m.Changed && m.State != state.Online {
			var txt string
			switch {
			case newcomer:
				txt = fmt.Sprintf("%s 第一次報到就是%s：%s",
					m.DisplayName, labelOf(m.State), m.Reason)
			case fromBlink:
				txt = fmt.Sprintf("%s %s：%s",
					m.DisplayName, labelOf(m.State), m.Reason)
			default:
				txt = fmt.Sprintf("%s %s → %s：%s",
					m.DisplayName, labelOf(m.PreviousState), labelOf(m.State), m.Reason)
			}
			out = append(out, line{rank: 1, tiebreak: m.DisplayName, text: txt})
			continue
		}
		// 修好了也要講 —— 否則人不知道昨天那條到底處理完了沒有。
		if m.Changed && m.State == state.Online {
			// ⚠ 但「從失聯恢復」而且那次失聯是 Hub 眨眼的話，這行不要寫：
			// 上面那行已經講了它失聯又恢復，這裡再寫一次「恢復（原本是失聯）」
			// 就是把同一件事講兩遍，而且第二遍的主詞是錯的。
			if fromBlink {
				continue
			}
			txt := fmt.Sprintf("%s 恢復（原本是%s）", m.DisplayName, labelOf(m.PreviousState))
			if newcomer {
				txt = fmt.Sprintf("%s 第一次報到", m.DisplayName)
			}
			out = append(out, line{rank: 4, tiebreak: m.DisplayName, text: txt})
			continue
		}

		// --- 憑證：有明確截止時間，值得單獨一行。
		for _, c := range m.CredExpiries {
			switch c.Status {
			case "expired":
				txt := fmt.Sprintf("%s 的 %s 登入已過期", m.DisplayName, c.Provider)
				if c.ExpiresAt != nil {
					txt += fmt.Sprintf("（%s 起）", c.ExpiresAt.Format("Jan 2"))
				}
				if peer, _, ok := state.MostActiveCredPeer(c.Peers); ok {
					txt += fmt.Sprintf("；同一家在 %s 有在續", peer.DisplayName)
				}
				out = append(out, line{rank: 2, tiebreak: m.DisplayName + c.Provider, text: txt})
			case "expires_soon":
				txt := fmt.Sprintf("%s 的 %s 登入即將過期", m.DisplayName, c.Provider)
				if c.ExpiresAt != nil {
					txt = fmt.Sprintf("%s 的 %s 登入 %s 到期",
						m.DisplayName, c.Provider, c.ExpiresAt.Format("Jan 2 15:04"))
				}
				out = append(out, line{rank: 2, tiebreak: m.DisplayName + c.Provider, text: txt})
			}
		}

		// --- 沉默失敗：最容易被忽略的一類，因為它不會自己喊。
		for _, f := range m.Findings {
			if f.Kind == "workload" && !f.Advisory {
				out = append(out, line{
					rank: 3, tiebreak: m.DisplayName,
					text: fmt.Sprintf("%s %s", m.DisplayName, f.Message),
				})
			}
		}

		// --- 其他還沒被上面收走的、任何不是 Online 的狀態。
		//
		// ⚠⚠ 這裡原本寫的是 `m.State == state.Degraded`，那是一個很嚴重的漏洞。
		//
		// 一台昨天失聯、今天還失聯的機器：Changed 是 false（狀態沒變）、
		// age 還沒到 7 天（進不了 "still broken since"）、不是 Degraded ——
		// 於是它一行都不產生。整份早報變成 `alive; 0 changes`。
		//
		// 而 `alive; 0 changes` 的意思是「今天沒事」。一台失聯的機器讓早報
		// 說出「沒事」，正好是這整個產品存在要防的那件事。身分衝突（最嚴重的
		// 狀態）也一樣會消失。實測輸出：
		//
		//     1/2 報到 · 失聯 1
		//     alive; 0 changes
		//
		// 標頭那一行是唯一的線索，而標頭正是人最先停止閱讀的部分。
		//
		// 現在的規則簡單得多：**只要不是 Online，就一定要有一行帶名字的話。**
		if m.State != state.Online && !hasRank(out, m.DisplayName) {
			// 失聯與身分衝突排在一般降級之前：你已經失去這台機器的消息了，
			// 那比「它還在講話但有東西過期」更該先看。
			rank := 4
			if m.State == state.Unreachable || m.State == state.IdentityConflict {
				rank = 2
			}
			out = append(out, line{
				rank: rank, tiebreak: m.DisplayName,
				text: fmt.Sprintf("%s %s：%s", m.DisplayName, labelOf(m.State), m.Reason),
			})
		}
	}
	return out
}

func hasRank(ls []line, name string) bool {
	for _, l := range ls {
		if strings.HasPrefix(l.text, name+" ") {
			return true
		}
	}
	return false
}

// nameList 把機器名字串起來。超過 4 個就截斷 ——
// ⚠ 一行早報塞十二個名字，等於沒有名字。
func nameList(ms []Machine) string {
	const maxNames = 4
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		names = append(names, m.DisplayName)
	}
	sort.Strings(names)
	if len(names) <= maxNames {
		return strings.Join(names, "、")
	}
	return fmt.Sprintf("%s 等 %d 台", strings.Join(names[:maxNames-1], "、"), len(names))
}

func earliest(ms []Machine) time.Time {
	var t time.Time
	for _, m := range ms {
		if t.IsZero() || (!m.StateSince.IsZero() && m.StateSince.Before(t)) {
			t = m.StateSince
		}
	}
	return t
}
