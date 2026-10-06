package store

import (
	"fmt"
	"time"
)

// 證據體檢。
//
// 2026-09-03：全機隊每 90 秒被 systemd SIGABRT 一次，Hub 全綠。
// 事後回頭看，桌上一直擺著一個直說的證據：`observation_age_seconds`
// 全機隊 **100% 是 NULL**。這個欄位存在的唯一目的就是講
// 「agent 在線但沒在觀測」，它從第一天起就一直在講，而沒有任何畫面在看它。
//
// 同一天還量到 samplehub1 的 task_runs 是 940/940 全 'succeeded' —— 一個常數。
//
//	沒有資料進來的欄位不是訊號。
//
// 一條死掉的證據管線不會產生錯誤，它產生的是**很有說服力的綠燈**。
// 對一個以「我不知道」為存在理由的產品，這是最糟的失效方式。
//
// ⚠⚠ 硬規則一：**只看 NULL，不看「值有沒有變」。**
//
// 第一版還有一條「同一欄從頭到尾只有一個值 = 管線死了」的規則。
// 它在第一份真實資料上就是 5 個誤報、0 個真陽性：samplehub1 與 sampleagent2 的
// clock_skew_seconds 恆為 0（那兩台的時鐘就是準的），每台的 provider
// 只有一個值（那台就真的只用了一個 provider）。
//
// NULL 是「沒有資料進來」，那是關於**管線**的事實。
// 常數是「資料每次都說一樣的話」，那可能就是**真相** —— 而 Hub 分不出來。
// 從分布的形狀去推論語意，跟從摘要文字去推論成敗是同一族的錯。
// 而且一個在正常狀態下就會亮的燈，三天內會被靜音，真訊號跟著陪葬（見 §5.1）。
//
// ⚠⚠ 硬規則二：**這個體檢只准報壞消息。**
// 它永遠不會、也不准輸出「證據健康」之類的話。一個會替自己的資料背書的
// 自我檢查，就是「自證不算數」本身 —— 它只能指出哪一條管線已經死了，
// 不能反過來說剩下的都是活的。所以回傳的是一個「壞掉清單」，
// 空清單的意思是「這幾條沒抓到問題」，不是「一切正常」。

// DeadSignalWindow：往回看多久。
// ⚠ 要比任何一台的 check-in 週期長很多，否則一台剛失聯的機器會被誤判成訊號死掉。
const DeadSignalWindow = 6 * time.Hour

// DeadSignalMinRows：低於這個列數就閉嘴。
// ⚠ 剛裝好的 Hub 只有兩三筆資料，那時候每一欄看起來都像常數。
// 一個在第一天就開始尖叫的體檢，會在第三天被靜音。
const DeadSignalMinRows = 10

type DeadSignal struct {
	// ⚠ 每一台各自算，不是全機隊一起算。
	// 全機隊一起算的話，一台好的機器就能把四台壞的蓋掉 ——
	// 而最壞的那幾台剛好會變成沒有人講話的那幾台。
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name"`

	Table  string `json:"table"`
	Column string `json:"column"`
	Rows   int    `json:"rows"`   // 這台在窗內的列數
	Reason string `json:"reason"` // 給人看的，要講清楚「因此你看不到什麼」
}

// signalCheck 一條要體檢的證據管線。
//
// ⚠ 清單是手寫的，而且只放**判決真的會讀**的欄位。
// 自動掃全部欄位聽起來比較聰明，但那會把 Phase 4 那些故意留空的欄位
// 全部報成死掉，然後這份清單就沒人看了。
type signalCheck struct {
	table, column string
	// what：這一欄壞掉的時候，你**因此看不到什麼**。
	// 寫「欄位是 NULL」沒有用，要寫「所以你不會知道 agent 停止觀測」。
	what string
}

var signalChecks = []signalCheck{
	{
		table: "machine_checkins", column: "observation_age_seconds",
		what: "agent 在線，但 observation_age_seconds 未更新",
	},
	{
		table: "machine_checkins", column: "agent_started_at",
		// ⚠ 這一欄只檢查 NULL。舊版 agent 不送它，而那正是要抓的情況：
		// 那些機器的 crash-loop 偵測是關著的。
		what: "agent_started_at 未更新；重啟迴圈偵測停用",
	},
	{
		table: "machine_checkins", column: "clock_skew_seconds",
		what: "clock_skew_seconds 未更新；時鐘漂移偵測停用",
	},
	{
		table: "ticket_occupancy_observation", column: "provider",
		what: "provider 未更新；額度占用缺少供應商",
	},
}

// DeadSignals 回傳目前抓到已經死掉的證據管線。
//
// ⚠ 空的回傳值**不代表證據健康**，只代表這幾條沒抓到問題。見檔案開頭。
func (s *Store) DeadSignals(now time.Time) ([]DeadSignal, error) {
	since := fmtTime(now.UTC().Add(-DeadSignalWindow))
	var out []DeadSignal

	for _, c := range signalChecks {
		have, err := columnSet(s.rdb, c.table)
		if err != nil {
			return nil, err
		}
		if !have[c.column] {
			continue // 這個 DB 還沒有這一欄（舊 schema）—— 那是 migration 的事，不是訊號的事
		}
		// ⚠ 圈時間窗一律用 Hub 的 received_at，不用上游的 measured_at ——
		// 一台時鐘歪掉的機器，它的 measured_at 可能整批落在窗外，
		// 於是那台機器的證據管線死掉時剛好不會被檢查到。
		if !have["received_at"] {
			return nil, fmt.Errorf("store: dead signal %s 沒有 received_at 欄", c.table)
		}

		rows, err := s.rdb.Query(fmt.Sprintf(`
SELECT t.machine_id, COALESCE(m.display_name, t.machine_id),
       COUNT(*), COUNT(t.%[1]s), COUNT(DISTINCT t.%[1]s)
  FROM %[2]s t LEFT JOIN machine_registry m ON m.machine_id = t.machine_id
 WHERE t.received_at >= ? AND m.retired_at IS NULL
 GROUP BY t.machine_id
 ORDER BY 2`, c.column, c.table), since)
		if err != nil {
			return nil, fmt.Errorf("store: dead signal %s.%s: %w", c.table, c.column, err)
		}
		for rows.Next() {
			var id, name string
			var n, nonNull, distinct int
			_ = distinct
			if err := rows.Scan(&id, &name, &n, &nonNull, &distinct); err != nil {
				rows.Close()
				return nil, err
			}
			if n < DeadSignalMinRows {
				continue // 這台樣本太少，說什麼都是猜的
			}
			// ⚠ 只看 NULL，不看「值有沒有變」。理由見檔案開頭那條硬規則。
			if nonNull > 0 {
				continue
			}
			out = append(out, DeadSignal{
				MachineID: id, DisplayName: name, Table: c.table, Column: c.column, Rows: n,
				Reason: fmt.Sprintf("最近 6 小時的 %d 筆裡，%s 每一筆都是空的。%s",
					n, c.column, c.what),
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	return out, nil
}
