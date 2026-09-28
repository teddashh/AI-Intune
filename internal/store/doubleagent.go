package store

import (
	"fmt"
	"sort"
	"time"
)

// 一台機器上不只一個 agent。
//
// 2026-09-03 實測：sampleagent2 與 sampleagent3 上各有兩個 clawctl-agent 同時回報了
// **五個半小時**（sampleagent3 是 13:23:02 → 18:48:20），期間 Hub 上每一盞燈都是綠的。
// 心跳正常、觀測有進來、判決全綠 —— 因為每一個 agent 各自都是對的。
//
//	心跳證明「有人在送」，不證明「只有一個人在送」。
//
// 兩個 agent 各自誠實地量、誠實地送，Hub 把兩份都收下，於是同一台機器的
// openclaw 版本在資料庫裡來回跳。兩個都對的事實，接起來是一個錯的解釋
// （docs/PHASE1.md §5.17）。
//
// ⚠⚠ 這件事**只有 Hub 看得到**。agent 問自己「我是不是唯一的那個」是自證：
// 那天兩個 agent 都會回答「是」，而且兩個都不是在說謊 —— 它們各自都不知道
// 對方存在。要分辨，必須站在同時收得到兩份回報的位置上。這正是
// 「自證不算數」在這個專案裡最乾淨的一個實例。
//
// ⚠⚠ 硬規則：**這個檢查只准報壞消息。** 跟 DeadSignals 一樣，空的回傳值
// 不代表「每台都只有一個 agent」。舊版 agent 不送 agent_started_at，
// 那些機器上有幾個 agent 我們**看不出來** —— 而看不出來不可以講成沒問題。
// 那條「看不出來」由 DeadSignals 的 agent_started_at 那一列負責講。

// DoubleAgentWindow：往回看多久。
// ⚠ 用一整天，不是 DeadSignals 的 6 小時。實測那次重疊了 5 小時 25 分，
// 而人是隔天早上看報告才會問「昨天到底怎麼了」—— 窗開得比事件短，
// 報告就會說昨天什麼事都沒有。
const DoubleAgentWindow = 24 * time.Hour

// DoubleAgentMinOverlap：重疊多久才算數。
//
// ⚠ 不可以是 0。systemd 重啟時，舊 agent 最後一顆心跳可能在新 agent 的
// 第一顆之後才**被 Hub 收到**（我們一律用 received_at 圈窗，見 DeadSignals），
// 於是每一次正常重啟都會擦出幾秒鐘的假重疊。一個每次部署都亮的燈，
// 三天後沒有人再看它（§5.1）。
//
// 五分鐘遠大於任何交接殘影，又遠小於實測的 5 小時 25 分 —— 兩邊都有幾十倍
// 的餘裕，所以這個數字不需要調校就能用。
const DoubleAgentMinOverlap = 5 * time.Minute

// doubleAgentOngoingGrace：兩邊都還在講話，才算「現在還在發生」。
// ⚠ 要比 check-in 週期（實測約 120 秒）長，否則剛好卡在兩顆心跳之間
// 就會被講成「已經結束了」。
const doubleAgentOngoingGrace = 10 * time.Minute

// AgentRun 是「某一台機器上，某一次 agent 啟動」的回報區間。
//
// ⚠ 身分是 agent_started_at，不是 boot_id 也不是 agent_seq。
//   - boot_id 同一次開機的兩個 agent 是一樣的 —— 正是要抓的那個情況。
//   - agent_seq 刻意跨重啟連續（schema.sql:77），而且兩個 agent 共用同一個
//     state 檔卻各自在記憶體裡數，互相蓋來蓋去，所以它既不能證明也不能否證。
type AgentRun struct {
	MachineID string    `json:"machine_id"`
	StartedAt time.Time `json:"started_at"`
	// First / Last 是 Hub **收到**這個 run 第一顆與最後一顆心跳的時間。
	// ⚠ 刻意不用機器自報的 sent_at：一台時鐘歪掉的機器會讓兩段區間
	// 在數線上錯開，於是真的重疊反而看起來不重疊。
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
	Checkins int       `json:"checkins"`
}

// DoubleAgent 是一台機器上「同時有超過一個 agent 在回報」的判定。
type DoubleAgent struct {
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name"`

	// Runs 是有參與重疊的那幾次啟動，照開始時間排。
	Runs []AgentRun `json:"runs"`
	// Peak 是**同一時刻**最多有幾個 agent 在回報。
	//
	// ⚠⚠ 它不等於 len(Runs)，而第一版就是拿 len(Runs) 去講「幾個 agent
	// 同時在回報」。實測 sampleagent3 有五次啟動參與重疊，但其中三次是前後相接的
	// 部署重啟，它們各自都跟那個長命的野 agent 重疊 —— 從來沒有三個同時活著。
	// 那句話會讓人去找五個根本不存在的 process。
	Peak int `json:"peak"`
	// From / Until 是「至少兩個 agent 同時活著」發生過的頭與尾。
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`
	// Overlap 是「至少兩個同時活著」的**總時長**。
	//
	// ⚠ 不是 Until-From。實測 sampleagent3 的 From→Until 是 6h22m，但中間有
	// 三段只有一個 agent 的空隙，真正重疊的是 6h18m。把空隙算進去
	// 等於把「你的資料被污染了多久」講長 —— 那是一個沒必要的誇大。
	Overlap time.Duration `json:"overlap"`
	// Ongoing：兩邊到現在都還在講話。false 代表這件事已經結束了，
	// ⚠ 但**不代表**已經被處理掉 —— 那台機器可能只是兩個 agent 一起死了。
	Ongoing bool   `json:"ongoing"`
	Reason  string `json:"reason"`
}

// AgentRuns 讀窗內每一台 × 每一次啟動的回報區間。
//
// ⚠ agent_started_at 是 NULL 的 check-in 全部**不參加**。舊版 agent 不送這一欄，
// 而「這一欄沒有值」的意思是「這筆觀測早於這個區分」，不是「沒有第二個 agent」。
// 把 NULL 當成一個身分，會讓所有舊資料看起來像另一個 agent —— 一整片假警報。
func (s *Store) AgentRuns(now time.Time) ([]AgentRun, error) {
	since := fmtTime(now.UTC().Add(-DoubleAgentWindow))
	rows, err := s.db.Query(`
SELECT c.machine_id, c.agent_started_at, COUNT(*), MIN(c.received_at), MAX(c.received_at)
  FROM machine_checkins c
  LEFT JOIN machine_registry m ON m.machine_id = c.machine_id
 WHERE c.received_at >= ?
   AND c.agent_started_at IS NOT NULL AND c.agent_started_at != ''
   AND m.retired_at IS NULL
 GROUP BY c.machine_id, c.agent_started_at
 ORDER BY c.machine_id ASC, MIN(c.received_at) ASC`, since)
	if err != nil {
		return nil, fmt.Errorf("store: 讀 agent 啟動區間: %w", err)
	}
	defer rows.Close()

	var out []AgentRun
	for rows.Next() {
		var id, started, first, last string
		var n int
		if err := rows.Scan(&id, &started, &n, &first, &last); err != nil {
			return nil, fmt.Errorf("store: scan agent 啟動區間: %w", err)
		}
		out = append(out, AgentRun{
			MachineID: id, StartedAt: parseTime(started),
			First: parseTime(first), Last: parseTime(last), Checkins: n,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 讀 agent 啟動區間: %w", err)
	}
	return out, nil
}

// DoubleAgents 回傳目前抓得到的「一台機器上不只一個 agent」。
//
// ⚠ 空的回傳值不代表每台都只有一個 agent，只代表這個窗內沒抓到。見檔案開頭。
func (s *Store) DoubleAgents(now time.Time) ([]DoubleAgent, error) {
	runs, err := s.AgentRuns(now)
	if err != nil {
		return nil, err
	}
	names, err := s.displayNames()
	if err != nil {
		return nil, err
	}

	byMachine := map[string][]AgentRun{}
	var order []string
	for _, r := range runs {
		if _, seen := byMachine[r.MachineID]; !seen {
			order = append(order, r.MachineID)
		}
		byMachine[r.MachineID] = append(byMachine[r.MachineID], r)
	}

	var out []DoubleAgent
	for _, id := range order {
		d, ok := overlappingRuns(byMachine[id], DoubleAgentMinOverlap)
		if !ok {
			continue
		}
		d.MachineID = id
		d.DisplayName = id
		if n := names[id]; n != "" {
			d.DisplayName = n
		}
		d.Ongoing = ongoing(d.Runs, now, doubleAgentOngoingGrace)
		d.Reason = doubleAgentReason(d)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DisplayName < out[j].DisplayName })
	return out, nil
}

// displayNames 是 machine_id → 人講的名字。
//
// ⚠ 名冊裡沒有的 machine_id 不會出現在這張表裡，呼叫端要自己 fallback 成 id ——
// 一台還沒登記就開始回報的機器，寧可顯示醜的 id，也不要顯示空白。
func (s *Store) displayNames() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT machine_id, COALESCE(display_name, '') FROM machine_registry`)
	if err != nil {
		return nil, fmt.Errorf("store: 讀機器名字: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("store: scan 機器名字: %w", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}

// overlappingRuns 掃出「同一時刻有兩個以上 agent 在回報」的那幾段。
//
// 用掃描線，不用兩兩比對。兩兩比對答得出「有沒有重疊」，但答不出
// 「最多同時幾個」跟「總共重疊多久」—— 而那兩個數字才是人要的。
//
// ⚠ 同一個時刻的事件要**先減後加**。一個 run 的 Last 剛好等於另一個的 First
// 時（正常重啟就長這樣），先加後減會讓計數短暫變成 2，於是每一次乾淨的重啟
// 都會被算成一瞬間的重疊。長度是 0、進不了門檻，但 Peak 會被汙染成 2。
func overlappingRuns(runs []AgentRun, min time.Duration) (DoubleAgent, bool) {
	type evt struct {
		at time.Time
		d  int
	}
	evts := make([]evt, 0, 2*len(runs))
	for _, r := range runs {
		evts = append(evts, evt{r.First, +1}, evt{r.Last, -1})
	}
	sort.Slice(evts, func(i, j int) bool {
		if !evts[i].at.Equal(evts[j].at) {
			return evts[i].at.Before(evts[j].at)
		}
		return evts[i].d < evts[j].d // -1 先
	})

	// 掃出所有「≥2 個同時活著」的區間。
	type span struct{ from, until time.Time }
	var spans []span
	var d DoubleAgent
	live, start := 0, time.Time{}
	for _, e := range evts {
		was := live
		live += e.d
		if live > d.Peak {
			d.Peak = live
		}
		switch {
		case was < 2 && live >= 2:
			start = e.at
		case was >= 2 && live < 2:
			if e.at.After(start) {
				spans = append(spans, span{start, e.at})
				d.Overlap += e.at.Sub(start)
			}
		}
	}
	// ⚠ 門檻套在**總時長**上，不是套在單一段上。一台被連續重啟三次的機器
	// 每一段都可能只有一兩分鐘，但它們加起來就是「你的資料被污染了多久」。
	if d.Overlap < min {
		return DoubleAgent{}, false
	}
	d.From, d.Until = spans[0].from, spans[len(spans)-1].until

	// 有參與到任何一段重疊的 run 才列出來。
	// ⚠ 閉區間相交（<=／>=）：一個只送過一顆心跳的 run 是一個「點」，
	// 它落在重疊區間裡的時候相交長度是 0，但它確實同時活著。
	for _, r := range runs {
		for _, s := range spans {
			if !r.First.After(s.until) && !r.Last.Before(s.from) {
				d.Runs = append(d.Runs, r)
				break
			}
		}
	}
	return d, true
}

// ongoing：參與重疊的 run 裡，至少兩個到現在都還在講話。
func ongoing(runs []AgentRun, now time.Time, grace time.Duration) bool {
	alive := 0
	for _, r := range runs {
		if now.Sub(r.Last) <= grace {
			alive++
		}
	}
	return alive >= 2
}

func doubleAgentReason(d DoubleAgent) string {
	when := "已經結束"
	if d.Ongoing {
		when = "現在還在發生"
	}
	// ⚠ 講「最多同時幾個」，不講「幾次啟動」。兩個數字不一樣的時候
	// （實測 sampleagent3 是 2 跟 5），後者會讓人去找不存在的 process。
	runs := ""
	if len(d.Runs) > d.Peak {
		runs = fmt.Sprintf("，共 %d 次啟動參與", len(d.Runs))
	}
	return fmt.Sprintf(
		"%s：最多同時 %d 個 agent 在回報（%s–%s 之間，重疊總長 %s%s，%s）。"+
			"兩邊都會送心跳、都會送觀測，所以每一盞燈都是綠的，"+
			"但同一個問題會拿到兩個答案 —— 版本、路徑、占用都可能來回跳。",
		d.DisplayName, d.Peak,
		d.From.Local().Format("15:04"), d.Until.Local().Format("15:04"),
		roundDur(d.Overlap), runs, when)
}

// roundDur 把長度講成人話。⚠ 秒不要顯示 —— 一個「5h25m2.113s」會讓人以為這是量測精度。
func roundDur(d time.Duration) time.Duration {
	if d >= time.Minute {
		return d.Round(time.Minute)
	}
	return d.Round(time.Second)
}
