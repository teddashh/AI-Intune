package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// 週報：票的占用帳。
//
// 這份報表要回答的是 docs/PHASES.md「每個週日 20 分鐘」那張表。
// 原本設計的欄位是：
//
//	profile  指派  見過  尖峰/時  429  401  距上次成功  閒置指派  結論
//
// ⚠⚠ 這裡面有四欄**填不出來**，而這份報表最重要的工作就是把它們
// 明明白白地留白並且說明原因，而不是填一個看起來合理的數字。
//
//	profile   → 上游只寫到 provider，票的名冊還沒建立。填了就是憑空指認。
//	429 / 401 → 我們有錯誤原文，沒有分類。把自由文字對到固定分類就是
//	            「掃 log 關鍵字」的變形，地基二否決過。而且實測最常見的
//	            那一句 "cron: job interrupted by gateway restart"
//	            不屬於任何一類 —— 硬分類只會分錯。
//	距上次成功 → 上游的 status='ok' 只代表這回合正常結束，實測 64% 的 ok
//	            其 summary 在描述失敗。所以這一欄叫「距上次**跑完**」。
//	閒置指派   → 需要指派名冊，而 ticket_static_assignment 是空的。
//
// 一份把四個空欄填滿的報表，會讓人在星期天早上做出四個有根據的錯誤決定。

// TicketWindow：週報預設看多久。
const TicketWindow = 7 * 24 * time.Hour

const (
	// TicketReadMaxWindow keeps interactive ticket reads inside a deliberate
	// operational horizon. Callers must make a narrower request instead of
	// turning this endpoint into an unbounded ledger export.
	TicketReadMaxWindow = 30 * 24 * time.Hour
	TicketReadMaxRows   = 250_000
	TicketReadMaxBytes  = 32 << 20
)

var (
	ErrInvalidTicketRead  = errors.New("invalid ticket read")
	ErrTicketReadTooBroad = errors.New("ticket read exceeds bounded evidence budget")
)

// ticketMaxErrors：每個 provider 最多列幾種錯誤原文。
// ⚠ 原文不合併、不歸類，只數同一句話出現幾次。
const ticketMaxErrors = 5

type TicketErrorCount struct {
	Text  string `json:"text"` // 原文，一字不改
	Count int    `json:"count"`
}

type TicketRow struct {
	Provider string `json:"provider"` // 上游原文，不併也不拆

	Machines []string `json:"machines"`
	Agents   []string `json:"agents"`

	Runs      int       `json:"runs"`
	LastRunAt time.Time `json:"last_run_at"`

	// PeakPerHour 是這個 provider 在窗內任何一個自然小時裡跑完最多幾回合。
	// ⚠ 這是唯一能回答「會不會撞到速率上限」的形狀 —— 平均值答不出來。
	PeakPerHour int       `json:"peak_per_hour"`
	PeakHour    time.Time `json:"peak_hour"`

	ErrorRuns int                `json:"error_runs"`
	Errors    []TicketErrorCount `json:"errors"`

	// ErrorVariantsTotal is counted before the top-error list is truncated.
	ErrorVariantsTotal int `json:"error_variants_total"`
}

type TicketReport struct {
	From, To time.Time `json:"-"`

	CandidateRows  int   `json:"candidate_rows"`
	CandidateBytes int64 `json:"candidate_bytes"`

	// MalformedMeasuredAt is excluded from time-shaped fields (last/peak), but
	// remains included in run and error counts.
	MalformedMeasuredAt int `json:"malformed_measured_at"`
	InvalidOpenClawJSON int `json:"invalid_openclaw_json"`

	// RosterSize 是分母 —— 名冊上有幾台（未退役）。
	// ReportingSize 是窗內真的有 check-in 的台數。
	RosterSize    int `json:"roster_size"`
	ReportingSize int `json:"reporting_size"`

	Rows []TicketRow `json:"rows"`

	// SkippedNoProvider：窗內有幾個跑完的回合沒有帶 provider，因此進不了這本帳。
	// ⚠ 實測缺的那些多半正好是失敗的回合 —— 也就是最想知道用了哪張票的那些。
	SkippedNoProvider int `json:"skipped_no_provider"`
}

// ReportingRate 是報到率。⚠ 低於 80% 就不准拿這份報表討論調度器：
// 分母裡有五分之一的機器沒講話，任何「誰在吃額度」的結論都是抽樣偏誤。
func (r TicketReport) ReportingRate() int {
	if r.RosterSize == 0 {
		return 0
	}
	return r.ReportingSize * 100 / r.RosterSize
}

// EnoughToDiscussScheduling 直接把 PHASES.md 的那條規則寫成程式。
// ⚠ 它回 false 的時候，報表上要印出來，不是靜靜地照常顯示。
func (r TicketReport) EnoughToDiscussScheduling() bool {
	return r.ReportingRate() >= 80
}

// TicketLedger 產生週報。
//
// ⚠ 這裡沒有任何一個欄位是推導出來的。每一格都對得回
// ticket_occupancy_observation 的某幾列，而那幾列的 occupant_evidence
// 又對得回上游的 session_key。要質疑任何一個數字，都查得下去。
func (s *Store) TicketLedger(now time.Time, window time.Duration) (TicketReport, error) {
	return s.TicketLedgerContext(context.Background(), now, window)
}

// TicketLedgerContext evaluates one fixed, inclusive [from,to] snapshot. All
// reads share one read-only transaction, including roster and coverage data.
func (s *Store) TicketLedgerContext(ctx context.Context, now time.Time, window time.Duration) (TicketReport, error) {
	if now.IsZero() || window <= 0 || window > TicketReadMaxWindow {
		return TicketReport{}, fmt.Errorf("%w: window must be within (0,%s]", ErrInvalidTicketRead, TicketReadMaxWindow)
	}
	to := now.UTC()
	from := to.Add(-window)
	rep := TicketReport{From: from, To: to}

	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TicketReport{}, fmt.Errorf("store: ticket snapshot: %w", err)
	}
	defer tx.Rollback()

	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM machine_registry WHERE retired_at IS NULL`,
	).Scan(&rep.RosterSize); err != nil {
		return TicketReport{}, fmt.Errorf("store: ticket roster: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT c.machine_id)
  FROM machine_checkins c JOIN machine_registry m USING(machine_id)
 WHERE c.received_at >= ? AND c.received_at <= ?
   AND m.retired_at IS NULL`,
		fmtTime(from), fmtTime(to)).Scan(&rep.ReportingSize); err != nil {
		return TicketReport{}, fmt.Errorf("store: ticket reporting: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(
         length(CAST(o.provider AS BLOB)) +
         length(CAST(COALESCE(m.display_name, o.machine_id) AS BLOB)) +
         length(CAST(COALESCE(o.agent_id, '') AS BLOB)) +
         length(CAST(o.measured_at AS BLOB)) +
         length(CAST(COALESCE(o.last_error_text, '') AS BLOB))
       ), 0)
  FROM ticket_occupancy_observation o
  LEFT JOIN machine_registry m ON m.machine_id = o.machine_id
 WHERE o.received_at >= ? AND o.received_at <= ?`,
		fmtTime(from), fmtTime(to)).Scan(&rep.CandidateRows, &rep.CandidateBytes); err != nil {
		return TicketReport{}, fmt.Errorf("store: ticket evidence budget: %w", err)
	}
	if rep.CandidateRows > TicketReadMaxRows || rep.CandidateBytes > TicketReadMaxBytes {
		return TicketReport{}, fmt.Errorf("%w: rows=%d/%d bytes=%d/%d",
			ErrTicketReadTooBroad, rep.CandidateRows, TicketReadMaxRows, rep.CandidateBytes, TicketReadMaxBytes)
	}

	rows, err := tx.QueryContext(ctx, `
SELECT o.provider,
       COALESCE(m.display_name, o.machine_id),
       COALESCE(o.agent_id, ''),
       o.measured_at,
       COALESCE(o.last_error_text, '')
  FROM ticket_occupancy_observation o
  LEFT JOIN machine_registry m ON m.machine_id = o.machine_id
 WHERE o.received_at >= ? AND o.received_at <= ?
 ORDER BY o.observation_id`, fmtTime(from), fmtTime(to))
	if err != nil {
		return TicketReport{}, fmt.Errorf("store: ticket ledger: %w", err)
	}
	defer rows.Close()

	type acc struct {
		machines, agents map[string]bool
		hours            map[string]int
		errs             map[string]int
		runs, errorRuns  int
		last             time.Time
	}
	byProvider := map[string]*acc{}
	for rows.Next() {
		var provider, machine, agent, measured, errText string
		if err := rows.Scan(&provider, &machine, &agent, &measured, &errText); err != nil {
			return TicketReport{}, err
		}
		a := byProvider[provider]
		if a == nil {
			a = &acc{machines: map[string]bool{}, agents: map[string]bool{},
				hours: map[string]int{}, errs: map[string]int{}}
			byProvider[provider] = a
		}
		a.runs++
		a.machines[machine] = true
		if agent != "" {
			a.agents[agent] = true
		}
		if errText != "" {
			a.errorRuns++
			a.errs[errText]++
		}
		if t, err := time.Parse(time.RFC3339, measured); err == nil {
			t = t.UTC()
			a.hours[t.Truncate(time.Hour).Format(time.RFC3339)]++
			if t.After(a.last) {
				a.last = t
			}
		} else {
			rep.MalformedMeasuredAt++
		}
	}
	if err := rows.Err(); err != nil {
		return TicketReport{}, err
	}
	if err := rows.Close(); err != nil {
		return TicketReport{}, fmt.Errorf("store: close ticket ledger rows: %w", err)
	}

	for provider, a := range byProvider {
		r := TicketRow{
			Provider:  provider,
			Machines:  sortedKeys(a.machines),
			Agents:    sortedKeys(a.agents),
			Runs:      a.runs,
			ErrorRuns: a.errorRuns,
			LastRunAt: a.last,
		}
		for h, n := range a.hours {
			if n > r.PeakPerHour {
				r.PeakPerHour = n
				r.PeakHour, _ = time.Parse(time.RFC3339, h)
			}
		}
		for text, n := range a.errs {
			r.Errors = append(r.Errors, TicketErrorCount{Text: text, Count: n})
		}
		r.ErrorVariantsTotal = len(r.Errors)
		// ⚠ 依次數排序後截斷，而且截斷這件事本身要在畫面上講出來。
		sort.Slice(r.Errors, func(i, j int) bool {
			if r.Errors[i].Count != r.Errors[j].Count {
				return r.Errors[i].Count > r.Errors[j].Count
			}
			return r.Errors[i].Text < r.Errors[j].Text
		})
		if len(r.Errors) > ticketMaxErrors {
			r.Errors = r.Errors[:ticketMaxErrors]
		}
		rep.Rows = append(rep.Rows, r)
	}
	sort.Slice(rep.Rows, func(i, j int) bool {
		if rep.Rows[i].Runs != rep.Rows[j].Runs {
			return rep.Rows[i].Runs > rep.Rows[j].Runs
		}
		return rep.Rows[i].Provider < rep.Rows[j].Provider
	})

	// ⚠ 只取每台**最近一次**觀測，不是把窗內所有觀測加起來。
	// agent 每 10 分鐘重掃一次同一批上游紀錄，加總會把同一筆算上幾百次，
	// 然後這個數字會大得像一場災難 —— 而它其實只是同一件事被數了很多遍。
	if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(SUM(
         CASE WHEN json_valid(o.payload) THEN
           CASE WHEN json_type(o.payload,'$.db.occupancy_rows_no_provider') = 'integer'
                THEN CAST(json_extract(o.payload,'$.db.occupancy_rows_no_provider') AS INTEGER)
                ELSE 0 END
         ELSE 0 END), 0),
       COALESCE(SUM(CASE WHEN json_valid(o.payload) THEN 0 ELSE 1 END), 0)
  FROM observed_state o
 WHERE o.kind = ?
   AND o.received_at >= ? AND o.received_at <= ?
   AND o.rowid = (SELECT x.rowid FROM observed_state x
                   WHERE x.machine_id = o.machine_id
                     AND x.kind = o.kind
                     AND x.received_at <= ?
                   ORDER BY x.received_at DESC, x.rowid DESC LIMIT 1)`,
		KindOpenClaw, fmtTime(from), fmtTime(to), fmtTime(to)).
		Scan(&rep.SkippedNoProvider, &rep.InvalidOpenClawJSON); err != nil {
		return TicketReport{}, fmt.Errorf("store: ticket skipped: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return TicketReport{}, fmt.Errorf("store: ticket snapshot commit: %w", err)
	}
	return rep, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
