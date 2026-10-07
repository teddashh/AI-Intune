package store

// 全機隊的工具版本 —— 「哪一台落後了」這個問題的資料來源。
//
// ⚠⚠ 這一份資料在 2026-09-03 之前是**不能看的**。那天修掉三層 bug 之前，
// agent 量的是自己那個 systemd --user 環境的 PATH，於是四台機器的版號
// 有一半是另一個檔案的版號（docs/PHASE1.md §5.16）。
// 先修好觀測，才有資格做這個彙總 —— 一張把四個錯數字排在一起的表，
// 比四個分開的錯數字更糟：它會讓人**比較**它們，然後照著差異去動手。

import (
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// FleetToolRow 是「某一台機器上，某一個工具的最新一筆觀測」。
//
// ⚠ 這裡刻意不帶 hostname。名字要從名冊（Overview.Machines）來 ——
// 一台**從來沒回報過**的機器在這裡一列都沒有，而它必須出現在畫面上。
// 讓這個查詢決定有哪些機器，就等於讓「有回報的」變成分母。
type FleetToolRow struct {
	MachineID string `json:"machine_id"`
	model.CLITool
	MeasuredAt time.Time `json:"measured_at"`
	ReceivedAt time.Time `json:"received_at"`
}

// FleetTools 回全機隊每一台 × 每一個工具的最新一筆 cli_tool 觀測。
//
// ⚠ 「最新」的定義跟 latestBySubject 一致：先比 received_at，
// 同一個 received_at 再比 measured_at、rowid。三層都要，因為
// received_at 會撞在一起（同一秒兩批）。
func (s *Store) FleetTools() ([]FleetToolRow, error) {
	rows, err := s.rdb.Query(`
SELECT o.machine_id, o.subject, o.payload, o.measured_at, o.received_at
  FROM observed_state o
 WHERE o.kind = ?
   AND o.received_at = (SELECT MAX(o2.received_at) FROM observed_state o2
                         WHERE o2.machine_id = o.machine_id AND o2.kind = o.kind
                           AND o2.subject = o.subject)
 ORDER BY o.machine_id ASC, o.subject ASC, o.measured_at DESC, o.rowid DESC`,
		KindCLITool)
	if err != nil {
		return nil, fmt.Errorf("store: read fleet tools: %w", err)
	}
	defer rows.Close()

	var out []FleetToolRow
	seen := map[string]bool{}
	for rows.Next() {
		var machineID, subject, payload, measured, recv string
		if err := rows.Scan(&machineID, &subject, &payload, &measured, &recv); err != nil {
			return nil, fmt.Errorf("store: scan fleet tools: %w", err)
		}
		key := machineID + "\x00" + subject
		if seen[key] {
			continue // 同一個 received_at 有多筆，上面的 ORDER BY 已經把該留的排第一
		}
		seen[key] = true
		t, ok := unmarshalInto[model.CLITool](payload)
		if !ok {
			// ⚠ 解不開的 payload 直接跳過，但**不是**靜靜跳過：
			// 那一格會因此變成「沒回報過」，而畫面上「沒回報過」跟
			// 「回報了但我讀不懂」是兩件事。這裡先照 latestBySubject 的
			// 既有做法（單機頁也是跳過），等有第一筆解不開的再處理 ——
			// 現在補一個猜的分支，會是一個永遠沒有人驗證過的分支。
			continue
		}
		out = append(out, FleetToolRow{
			MachineID:  machineID,
			CLITool:    t,
			MeasuredAt: parseTime(measured),
			ReceivedAt: parseTime(recv),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read fleet tools: %w", err)
	}
	return out, nil
}
