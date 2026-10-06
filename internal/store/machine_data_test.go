package store

import (
	"sort"
	"testing"
	"time"
)

// 揭露面唯一真正危險的失效模式是「少講一張表」。少講的那張表在畫面上跟不存在
// 一模一樣，而它其實正留著資料。所以這支測試不比對一份手寫清單，它問 SQLite。
func TestEveryMachineScopedTableIsDisclosed(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(tables) == 0 {
		t.Fatal("schema 裡一張表都沒有")
	}
	scoped := map[string]bool{}
	for _, table := range tables {
		for _, column := range tableColumns(t, s, table) {
			if column == "machine_id" {
				scoped[table] = true
			}
		}
	}
	declared := map[string]bool{}
	for _, table := range MachineDataTables() {
		if declared[table.Table] {
			t.Fatalf("%s 被宣告了兩次", table.Table)
		}
		declared[table.Table] = true
	}
	for table := range scoped {
		if !declared[table] {
			t.Errorf("%s 存著 machine_id，但揭露面沒有講它留了什麼、留多久", table)
		}
	}
	for table := range declared {
		if !scoped[table] {
			t.Errorf("揭露面宣告了 %s，但這張表裡沒有 machine_id", table)
		}
	}
}

// 每一個宣告的時間欄都必須真的存在。一個欄名打錯的揭露面，是在畫面被打開的那
// 一刻才炸的 —— 而畫面被打開，正是因為有人想知道 Hub 留了他什麼。
func TestEveryDisclosedTimeColumnExists(t *testing.T) {
	s := newTestStore(t)
	for _, table := range MachineDataTables() {
		if table.TimeCol == "" {
			continue
		}
		found := false
		for _, column := range tableColumns(t, s, table.Table) {
			if column == table.TimeCol {
				found = true
			}
		}
		if !found {
			t.Errorf("%s 宣告的時間欄 %q 不存在", table.Table, table.TimeCol)
		}
	}
}

// 會被時間清掉的表一定要有 Hub 時刻，否則畫面沒有辦法說出「最舊的那一列還有
// 多久會被清掉」——而那是保留期唯一對操作員有意義的形式。
func TestEveryPrunedTableDisclosesAHubClock(t *testing.T) {
	s := newTestStore(t)
	declared := map[string]MachineDataTable{}
	for _, table := range MachineDataTables() {
		declared[table.Table] = table
	}
	for _, job := range pruneJobs {
		hasMachine := false
		for _, column := range tableColumns(t, s, job.table) {
			if column == "machine_id" {
				hasMachine = true
			}
		}
		table, ok := declared[job.table]
		// notifications 是 Hub 自己的推播紀錄，沒有 machine_id，不進單機揭露面。
		// 有 machine_id 的表仍必須出現在揭露面，而且要有 Hub 時刻。
		if hasMachine {
			if !ok {
				t.Errorf("%s 會被時間清掉，但揭露面沒有講它", job.table)
				continue
			}
			if table.TimeCol == "" {
				t.Errorf("%s 會被時間清掉，但揭露面說它沒有時間", job.table)
			}
		} else if ok {
			t.Errorf("%s 沒有 machine_id，不該出現在單機揭露面", job.table)
		}
		if job.cutCol == "" {
			t.Errorf("%s 會被時間清掉，但沒有 Hub 時刻欄", job.table)
		}
		class, known := RetentionClassOf(job.table)
		if !known || class != job.class {
			t.Errorf("%s 的保留期類別是 %q，RetentionClassOf 說 %q", job.table, job.class, class)
		}
	}
}

func TestRetentionHorizonFollowsTheLivePolicy(t *testing.T) {
	policy := RetentionPolicy{
		Observations: 31 * 24 * time.Hour,
		Checkins:     13 * 24 * time.Hour,
		Occupancy:    399 * 24 * time.Hour,
	}
	for _, want := range []struct {
		class RetentionClass
		value time.Duration
	}{
		{RetentionObservations, policy.Observations},
		{RetentionCheckins, policy.Checkins},
		{RetentionOccupancy, policy.Occupancy},
	} {
		got, ok := RetentionHorizon(want.class, policy)
		if !ok || got != want.value {
			t.Errorf("RetentionHorizon(%q) = %v/%v, want %v", want.class, got, ok, want.value)
		}
	}
	if _, ok := RetentionHorizon("machine_registry", policy); ok {
		t.Error("不是保留期類別的字串不可以算出保留期")
	}
	if _, ok := RetentionClassOf("machine_registry"); ok {
		t.Error("名冊不按時間清，不該有保留期類別")
	}
	if class, ok := RetentionClassOf("machine_job_capabilities"); !ok || class != RetentionCheckins {
		t.Errorf("工作原語能力跟著 check-in 清，class=%q known=%v", class, ok)
	}
}

func TestMachineDataHoldingsCountsWhatIsActuallyThere(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now)

	holdings, err := s.MachineDataHoldings(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(holdings) != len(MachineDataTables()) {
		t.Fatalf("holdings=%d tables=%d", len(holdings), len(MachineDataTables()))
	}
	for index, holding := range holdings {
		if holding.Table != MachineDataTables()[index].Table {
			t.Fatalf("第 %d 列是 %q，宣告的順序是 %q",
				index, holding.Table, MachineDataTables()[index].Table)
		}
	}
	registry := holdingFor(t, holdings, "machine_registry")
	if registry.Rows != 1 || registry.Dated != 1 {
		t.Fatalf("名冊 rows=%d dated=%d, want 1/1", registry.Rows, registry.Dated)
	}
	if registry.Oldest == nil || !registry.Oldest.Equal(now) {
		t.Fatalf("名冊最舊 = %v, want %v", registry.Oldest, now)
	}
	if checkins := holdingFor(t, holdings, "machine_checkins"); checkins.Rows != 0 {
		t.Fatalf("還沒報到過，machine_checkins 卻有 %d 列", checkins.Rows)
	}
	if capabilities := holdingFor(t, holdings, "machine_job_capabilities"); capabilities.Rows != 0 {
		t.Fatalf("還沒報到過，machine_job_capabilities 卻有 %d 列", capabilities.Rows)
	}
}

// 一列沒有 Hub 時刻的資料仍然被留著。它必須被算進列數，而且不可以被擺到最舊
// 那一格 —— 那會憑空造出一段歷史。生產庫裡真的有這種列。
func TestARowWithoutAHubClockStillCounts(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now)
	dated := now.Add(2 * time.Hour)
	for _, at := range []string{"", fmtTime(dated)} {
		if _, err := s.db.Exec(
			`INSERT INTO machine_state_history(machine_id, state, reason, entered_at) VALUES(?,?,?,?)`,
			id, "Degraded", "磁碟快滿了", at); err != nil {
			t.Fatal(err)
		}
	}
	holdings, err := s.MachineDataHoldings(id)
	if err != nil {
		t.Fatal(err)
	}
	history := holdingFor(t, holdings, "machine_state_history")
	if history.Rows != 2 {
		t.Fatalf("rows=%d, want 2 —— 沒有時刻的那一列也還留著", history.Rows)
	}
	if history.Dated != 1 {
		t.Fatalf("dated=%d, want 1", history.Dated)
	}
	if history.Oldest == nil || !history.Oldest.Equal(dated) {
		t.Fatalf("最舊 = %v, want %v —— 空字串不是一個很久以前的時刻", history.Oldest, dated)
	}
	if history.Newest == nil || !history.Newest.Equal(dated) {
		t.Fatalf("最新 = %v, want %v", history.Newest, dated)
	}
}

func TestMachineDataHoldingsRefusesAMachineItDoesNotHave(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.MachineDataHoldings("不在名冊上"); err != ErrNotFound {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
	if _, err := s.MachineDataHoldings(""); err == nil {
		t.Fatal("空的 machine id 應該被拒絕")
	}
}

func tableColumns(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var (
			cid        int
			name, kind string
			notNull    int
			dflt       any
			pk         int
		)
		if err := rows.Scan(&cid, &name, &kind, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(columns)
	return columns
}

func holdingFor(t *testing.T, holdings []MachineDataHolding, table string) MachineDataHolding {
	t.Helper()
	for _, holding := range holdings {
		if holding.Table == table {
			return holding
		}
	}
	t.Fatalf("holdings 裡沒有 %s", table)
	return MachineDataHolding{}
}
