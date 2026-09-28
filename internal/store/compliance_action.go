package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
)

// complianceActions decides what one policy's consequences are doing to one
// machine right now. Nothing is written: the effect is a function of the
// published policy and the reported facts, so it is recomputed on every read.
func (s *Store) complianceActions(machineID string, p compliance.Policy,
	verdict compliance.Verdict, applied func(string) *bool, now time.Time,
) ([]compliance.ActionOutcome, error) {
	if verdict != compliance.VerdictNoncompliant {
		// 不是「確定有一條規則沒過」就沒有連續不符合可以量，證據窗不必讀。
		// DecideActions 自己也會擋下來，這裡只是不去問那個問不出東西的問題。
		return compliance.DecideActions(p, verdict, time.Time{}, false, now), nil
	}
	since, floor, err := s.complianceRunStart(machineID, p, applied, now)
	if err != nil {
		return nil, err
	}
	return compliance.DecideActions(p, verdict, since, floor, now), nil
}

// complianceRunStart proves how long this machine has been noncompliant
// without interruption, from reported facts alone.
//
// ⚠ 這裡沒有「上次判決是什麼」這種欄位可以讀，因為判決不存。要證明「這段期間
// 一直不符合」，唯一的辦法是把這段期間的每一次 check-in 都重判一次：只要中間
// 有任何一次不算不符合，連續就斷了，寬限期要從斷點之後重新起算。
//
// 要讀的窗就是最長的那個寬限期，再往前多讀一列 —— 窗開頭那一刻管事的是窗外
// 最後一次回報，不讀它就分不出「整個窗都在壞」與「壞是從窗裡某一列才開始的」。
//
// 回看用的是**現在這一份**原則與**現在這一份**設定指派。問的是「照現在的規則，
// 那一刻算不算不符合」，不是「當時的規則怎麼判」——當時的規則可能還沒發佈。
func (s *Store) complianceRunStart(machineID string, p compliance.Policy,
	applied func(string) *bool, now time.Time,
) (time.Time, bool, error) {
	windowStart := now.Add(-p.MaxGrace())
	lower := windowStart
	var boundary sql.NullString
	if err := s.db.QueryRow(`SELECT MAX(received_at) FROM machine_checkins
 WHERE machine_id = ? AND received_at < ?`, machineID, fmtTime(windowStart)).Scan(&boundary); err != nil {
		return time.Time{}, false, fmt.Errorf("store: compliance window boundary: %w", err)
	}
	if boundary.Valid {
		lower = parseTime(boundary.String)
	}

	rows, err := s.db.Query(`
SELECT received_at, agent_version, disk_free_bytes, disk_total_bytes, jobs_enabled, settings_digest
  FROM machine_checkins
 WHERE machine_id = ? AND received_at >= ?
 ORDER BY received_at DESC, sent_at DESC`, machineID, fmtTime(lower))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: compliance window: %w", err)
	}
	defer rows.Close()

	var newer time.Time
	for rows.Next() {
		var at string
		var version, digest sql.NullString
		var free, total, jobs sql.NullInt64
		if err := rows.Scan(&at, &version, &free, &total, &jobs, &digest); err != nil {
			return time.Time{}, false, fmt.Errorf("store: scan compliance window row: %w", err)
		}
		received := parseTime(at)
		// 在一次 check-in 的那一刻，報到新鮮度必然是通過的：age 就是 0。
		// 其餘每一條規則讀的都是那一列自己回報的值。
		facts := compliance.Facts{
			EverCheckedIn: true,
			AgentVersion:  version.String,
			DiskFreeBytes: free.Int64, DiskTotalBytes: total.Int64,
			SettingsApplied: applied(digest.String),
		}
		if jobs.Valid {
			enabled := jobs.Int64 == 1
			facts.JobsEnabled = &enabled
		}
		if compliance.Judge(true, true, compliance.Evaluate(p, facts)) == compliance.VerdictNoncompliant {
			newer = received
			continue
		}
		if !newer.IsZero() {
			// 這是最新一次「不算不符合」的回報，連續不符合從它的下一列開始。
			return newer, false, rows.Err()
		}
		// 最新的一列自己就通過了，而現在是不符合 —— 只可能是時間走過去，
		// 報到新鮮度那一條在 received + 上限 的那一刻開始不過。
		return received.Add(checkinMaxAge(p)), false, rows.Err()
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, false, err
	}
	// 讀到的每一列都是不符合，連最早那一列（窗外那一次回報）也是：連續不符合
	// 在窗開始之前就已經在跑，所以每一個寬限期都已經走完。
	return lower, true, nil
}

// checkinMaxAge is the freshness bound in this policy, or zero when it does
// not ask about freshness.
func checkinMaxAge(p compliance.Policy) time.Duration {
	for _, rule := range p.Rules {
		if rule.Kind == compliance.RuleCheckinMaxAge {
			return time.Duration(rule.MaxAgeSeconds) * time.Second
		}
	}
	return 0
}

// ComplianceBlocksJobs answers the dispatch path's only question: may this
// machine be handed work right now?
//
// ⚠ 這條路徑在每次領單時都會走到，所以它先問最便宜的問題：有沒有指派原則、原則
// 裡有沒有動作。兩個提早返回都只是省成本 —— 它們的答案與完整判斷一致，因為沒有
// 動作就沒有東西會生效。今天機隊沒有指派任何合規性原則，它就只是一次查詢。
func (s *Store) ComplianceBlocksJobs(machineID string) (bool, error) {
	assignments, err := s.complianceAssignments()
	if err != nil {
		return false, err
	}
	effective := compliance.Resolve(machineID, s.machineChannel(machineID), assignments)
	if !effective.Assigned() {
		return false, nil
	}
	if len(effective.Policy.Actions) == 0 {
		return false, nil
	}
	state, err := s.ResolveMachineCompliance(machineID)
	if err != nil {
		return false, err
	}
	return compliance.Blocks(state.Actions), nil
}

// machineChannel reads the channel the resolver needs, and says "" when the
// machine has none. A missing machine has no channel and therefore no
// channel-scoped policy; the caller's own lookup decides whether it exists.
func (s *Store) machineChannel(machineID string) string {
	var channel sql.NullString
	if err := s.db.QueryRow(`SELECT channel FROM machine_registry WHERE machine_id = ?`,
		machineID).Scan(&channel); err != nil {
		return ""
	}
	return channel.String
}

// ComplianceBlockedMachines lists every machine a compliance action is
// currently withholding work from. It answers nothing at all when no policy
// carries an action, which is the fleet's state until an operator publishes one.
func (s *Store) ComplianceBlockedMachines() (map[string]bool, error) {
	assignments, err := s.complianceAssignments()
	if err != nil {
		return nil, err
	}
	withActions := false
	for _, a := range assignments {
		if len(a.Policy.Actions) > 0 {
			withActions = true
			break
		}
	}
	if !withActions {
		return map[string]bool{}, nil
	}
	states, err := s.MachineComplianceStates()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, st := range states {
		if compliance.Blocks(st.Actions) {
			out[st.MachineID] = true
		}
	}
	return out, nil
}
