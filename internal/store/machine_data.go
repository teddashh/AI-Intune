package store

import (
	"database/sql"
	"fmt"
	"time"
)

// 這個 Hub 對一台機器留下了什麼。
//
// ⚠ 下面這張表就是揭露面本身。一張存著 machine_id 的表如果不在這裡，產品就會對
// 操作員少講一件它其實留著的東西 —— 而那正好是揭露面存在要防的那一件事。所以
// 這張表不是文件，是被驗證的宣告：`TestEveryMachineScopedTableIsDisclosed` 直接
// 問 SQLite 要所有帶 machine_id 的表，少一張或多一張都會紅。
//
// ⚠ timeCol 一律是 **Hub 自己的鐘**。理由跟 retention.go 的 cutCol 一樣：一台時鐘
// 歪掉的機器，用它自己的鐘去排「最舊 / 最新」，會在畫面上得到一段從來沒有發生過的
// 歷史。沒有 Hub 時刻的表就留空，畫面說它沒有自己的時間，不挑一個代替。

// MachineDataTable 是一張存著某台機器資料的表。
type MachineDataTable struct {
	// Table 是 SQLite 裡的表名。
	Table string
	// TimeCol 是 Hub 寫下這一列的時刻；空字串表示這張表沒有自己的時間。
	TimeCol string
}

// machineDataTables 依「先名冊、再機器自己送的、再 Hub 推出來的、最後人留下的」排。
var machineDataTables = []MachineDataTable{
	{Table: "machine_registry", TimeCol: "created_at"},
	{Table: "machine_registry_lifecycle_events", TimeCol: "occurred_at"},
	{Table: "machine_identity_hints", TimeCol: "last_seen_at"},
	{Table: "machine_checkins", TimeCol: "received_at"},
	// capability 沒有自己的 Hub 時刻；它跟 exact check-in 同生同滅，保留期由 FK cascade 繼承。
	{Table: "machine_job_capabilities", TimeCol: ""},
	{Table: "observed_state", TimeCol: "received_at"},
	{Table: "ticket_occupancy_observation", TimeCol: "received_at"},
	{Table: "credential_on_machine", TimeCol: "updated_at"},
	{Table: "verification_results", TimeCol: "received_at"},
	{Table: "machine_state_history", TimeCol: "entered_at"},
	{Table: "machine_state_transition_events", TimeCol: "entered_at"},
	{Table: "workload_observation_witness", TimeCol: "received_at"},
	{Table: "workload_observation_evidence", TimeCol: "received_at"},
	{Table: "canary_silent_failures", TimeCol: "last_seen_at"},
	{Table: "jobs", TimeCol: "created_at"},
	{Table: "deployment_targets", TimeCol: ""},
	{Table: "machine_profile_assignments", TimeCol: "assigned_at"},
	{Table: "ticket_static_assignment", TimeCol: "assigned_at"},
	{Table: "credential_ledger_event", TimeCol: "received_at"},
	{Table: "audit_log", TimeCol: "at"},
}

// MachineDataTables 是宣告過的那張表，複製一份出去。
func MachineDataTables() []MachineDataTable {
	return append([]MachineDataTable(nil), machineDataTables...)
}

// MachineDataHolding 是一張表對一台機器實際存著的量。
type MachineDataHolding struct {
	Table string `json:"table"`
	// Rows 是這台機器在這張表裡的列數。
	Rows int64 `json:"rows"`
	// Dated 是其中帶著可用 Hub 時刻的列數。
	//
	// ⚠ Rows 減 Dated 不是零的時候，那些列真的存在、也真的留著，只是沒有人知道
	// 它們是什麼時候收到的。生產庫裡就有這種列（早期的 verification_results），
	// 把它們算進最舊那一格會憑空造出一段歷史，不算進列數又會少講留著的東西。
	Dated  int64      `json:"dated_rows"`
	Oldest *time.Time `json:"oldest,omitempty"`
	Newest *time.Time `json:"newest,omitempty"`
}

// MachineDataHoldings 量這個 Hub 現在替這台機器留著多少列。
//
// 每一張宣告過的表各一次彙總查詢，全部走 machine_id 的索引；samplehub1 的生產庫
// （observed_state 3.6 萬列）實測 19 張表一輪 8 毫秒。
func (s *Store) MachineDataHoldings(machineID string) ([]MachineDataHolding, error) {
	if machineID == "" {
		return nil, fmt.Errorf("store: machine data holdings 需要 machine id")
	}
	var exists bool
	if err := s.rdb.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM machine_registry WHERE machine_id = ?)`,
		machineID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	holdings := make([]MachineDataHolding, 0, len(machineDataTables))
	for _, table := range machineDataTables {
		holding, err := s.machineDataHolding(table, machineID)
		if err != nil {
			return nil, fmt.Errorf("store: 讀 %s 的留存量: %w", table.Table, err)
		}
		holdings = append(holdings, holding)
	}
	return holdings, nil
}

func (s *Store) machineDataHolding(table MachineDataTable, machineID string) (MachineDataHolding, error) {
	holding := MachineDataHolding{Table: table.Table}
	if table.TimeCol == "" {
		err := s.rdb.QueryRow(
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE machine_id = ?`, table.Table),
			machineID).Scan(&holding.Rows)
		return holding, err
	}
	// NULLIF 把空字串當成「沒記」，不是當成一個比任何時刻都早的時刻。少了它，
	// min() 會回一個空字串，畫面上那格就變成一個看起來很久以前的空白。
	var oldest, newest sql.NullString
	err := s.rdb.QueryRow(fmt.Sprintf(`
SELECT count(*),
       coalesce(sum(CASE WHEN %[1]s IS NOT NULL AND %[1]s <> '' THEN 1 ELSE 0 END), 0),
       min(NULLIF(%[1]s, '')),
       max(NULLIF(%[1]s, ''))
  FROM %[2]s WHERE machine_id = ?`, table.TimeCol, table.Table),
		machineID).Scan(&holding.Rows, &holding.Dated, &oldest, &newest)
	if err != nil {
		return MachineDataHolding{}, err
	}
	holding.Oldest = parseTimePtr(oldest)
	holding.Newest = parseTimePtr(newest)
	return holding, nil
}
