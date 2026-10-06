package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
)

// CompliancePolicySummary is one policy as the console lists it: its newest
// revision, and how many scopes are currently pinned to it.
type CompliancePolicySummary struct {
	PolicyID      string            `json:"policy_id"`
	Revision      int64             `json:"policy_revision"`
	Digest        string            `json:"rules_digest"`
	Policy        compliance.Policy `json:"policy"`
	PublishedAt   time.Time         `json:"published_at"`
	RevisionCount int64             `json:"revision_count"`
	Assignments   int64             `json:"assignment_count"`
}

// CompliancePolicies lists every policy by newest revision first.
func (s *Store) CompliancePolicies() ([]CompliancePolicySummary, error) {
	rows, err := s.rdb.Query(`
SELECT p.policy_id, p.policy_revision, p.rules_json, p.rules_digest, p.published_at,
       (SELECT COUNT(*) FROM compliance_policies c WHERE c.policy_id = p.policy_id),
       (SELECT COUNT(*) FROM compliance_assignments a
         WHERE a.policy_id = p.policy_id
           AND a.assignment_revision = (SELECT MAX(b.assignment_revision)
                                          FROM compliance_assignments b
                                         WHERE b.scope_type = a.scope_type
                                           AND b.scope_id   = a.scope_id))
  FROM compliance_policies p
 WHERE p.policy_revision = (SELECT MAX(q.policy_revision) FROM compliance_policies q
                             WHERE q.policy_id = p.policy_id)
 ORDER BY p.published_at DESC, p.policy_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list compliance policies: %w", err)
	}
	defer rows.Close()
	var out []CompliancePolicySummary
	for rows.Next() {
		var r CompliancePolicySummary
		var raw, at string
		if err := rows.Scan(&r.PolicyID, &r.Revision, &raw, &r.Digest, &at,
			&r.RevisionCount, &r.Assignments); err != nil {
			return nil, fmt.Errorf("store: scan compliance policy: %w", err)
		}
		if r.Policy, err = compliance.Parse([]byte(raw)); err != nil {
			return nil, fmt.Errorf("store: stored compliance policy %s@%d is unreadable: %w",
				r.PolicyID, r.Revision, err)
		}
		r.PublishedAt = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CompliancePolicyRevisions returns every published revision of one policy,
// newest first. Older revisions stay readable because a machine judged under
// one of them is entitled to an answer about which rules judged it.
func (s *Store) CompliancePolicyRevisions(policyID string) ([]CompliancePolicyRecord, error) {
	rows, err := s.rdb.Query(`SELECT policy_id,policy_revision,rules_json,rules_digest,
	 published_at,published_by FROM compliance_policies WHERE policy_id=?
	 ORDER BY policy_revision DESC`, policyID)
	if err != nil {
		return nil, fmt.Errorf("store: list compliance policy revisions: %w", err)
	}
	defer rows.Close()
	var out []CompliancePolicyRecord
	for rows.Next() {
		var r CompliancePolicyRecord
		var raw, at string
		if err := rows.Scan(&r.PolicyID, &r.Revision, &raw, &r.Digest, &at, &r.PublishedBy); err != nil {
			return nil, fmt.Errorf("store: scan compliance policy revision: %w", err)
		}
		if r.Policy, err = compliance.Parse([]byte(raw)); err != nil {
			return nil, fmt.Errorf("store: stored compliance policy %s@%d is unreadable: %w",
				r.PolicyID, r.Revision, err)
		}
		r.PublishedAt = parseTime(at)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrCompliancePolicyNotFound
	}
	return out, nil
}

// CurrentComplianceAssignments returns the newest assignment for every scope
// that has one. Superseded revisions stay in the table as history but never
// answer "which rules are judging this scope".
func (s *Store) CurrentComplianceAssignments() ([]ComplianceAssignmentRecord, error) {
	rows, err := s.rdb.Query(`
SELECT a.assignment_id, a.scope_type, a.scope_id, a.assignment_revision,
       a.policy_id, a.policy_revision, a.rules_digest, a.assigned_at, a.assigned_by,
       p.rules_json
  FROM compliance_assignments a
  JOIN compliance_policies p
    ON p.policy_id = a.policy_id AND p.policy_revision = a.policy_revision
 WHERE a.assignment_revision = (SELECT MAX(b.assignment_revision) FROM compliance_assignments b
                                 WHERE b.scope_type = a.scope_type AND b.scope_id = a.scope_id)
 ORDER BY a.scope_type, a.scope_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list compliance assignments: %w", err)
	}
	defer rows.Close()
	var out []ComplianceAssignmentRecord
	for rows.Next() {
		var r ComplianceAssignmentRecord
		var raw, at string
		if err := rows.Scan(&r.AssignmentID, &r.Scope, &r.ScopeID, &r.Revision, &r.PolicyID,
			&r.PolicyRev, &r.Digest, &at, &r.AssignedBy, &raw); err != nil {
			return nil, fmt.Errorf("store: scan compliance assignment: %w", err)
		}
		if r.Policy, err = compliance.Parse([]byte(raw)); err != nil {
			return nil, fmt.Errorf("store: stored compliance policy %s@%d is unreadable: %w",
				r.PolicyID, r.PolicyRev, err)
		}
		r.AssignedAt = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// complianceAssignments loads every current assignment as the pure resolver
// wants them. It is the one query the board and the per-machine read share.
func (s *Store) complianceAssignments() ([]compliance.Assignment, error) {
	rows, err := s.rdb.Query(`
SELECT a.scope_type, a.scope_id, a.policy_id, a.policy_revision, a.rules_digest,
       a.assigned_at, p.rules_json
  FROM compliance_assignments a
  JOIN compliance_policies p
    ON p.policy_id = a.policy_id AND p.policy_revision = a.policy_revision
 WHERE a.assignment_revision = (SELECT MAX(b.assignment_revision) FROM compliance_assignments b
                                 WHERE b.scope_type = a.scope_type AND b.scope_id = a.scope_id)`)
	if err != nil {
		return nil, fmt.Errorf("store: load compliance assignments: %w", err)
	}
	defer rows.Close()
	var out []compliance.Assignment
	for rows.Next() {
		var a compliance.Assignment
		var raw string
		if err := rows.Scan(&a.Scope, &a.ScopeID, &a.PolicyID, &a.Revision, &a.Digest,
			&a.AssignedAt, &raw); err != nil {
			return nil, fmt.Errorf("store: scan compliance assignment row: %w", err)
		}
		if a.Policy, err = compliance.Parse([]byte(raw)); err != nil {
			return nil, fmt.Errorf("store: stored compliance policy %s@%d is unreadable: %w",
				a.PolicyID, a.Revision, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MachineComplianceState is one row of the compliance board.
type MachineComplianceState struct {
	MachineID   string                  `json:"machine_id"`
	DisplayName string                  `json:"display_name"`
	Channel     string                  `json:"channel,omitempty"`
	Effective   compliance.Effective    `json:"effective"`
	Verdict     compliance.Verdict      `json:"verdict"`
	Results     []compliance.RuleResult `json:"results"`
	// Actions is what the policy's consequences are doing to this machine
	// right now. Empty when the policy carries no actions.
	Actions     []compliance.ActionOutcome `json:"actions,omitempty"`
	EvaluatedAt time.Time                  `json:"evaluated_at"`
	ReportedAt  *time.Time                 `json:"reported_at,omitempty"`
}

// MachineComplianceStates judges every machine still in the denominator.
//
// ⚠ 判決是現算的，不是存下來的。它依賴「現在幾點」，所以同一列在兩分鐘後
// 可以合法地變成另一個判決 —— 把判決寫進表裡就會有一列說某台機器合規，而它
// 其實已經三天沒報到。
//
// ⚠ 沒有任何一條規則讀機器自己對合規的主張。機器能做的只有回報事實。
func (s *Store) MachineComplianceStates() ([]MachineComplianceState, error) {
	assignments, err := s.complianceAssignments()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.Query(`
SELECT m.machine_id, m.display_name, COALESCE(m.channel,''),
       c.received_at, c.agent_version, c.disk_free_bytes, c.disk_total_bytes,
       c.jobs_enabled, c.settings_digest
  FROM machine_registry m
  LEFT JOIN (SELECT machine_id, received_at, agent_version, disk_free_bytes, disk_total_bytes,
                    jobs_enabled, settings_digest,
                    ROW_NUMBER() OVER (PARTITION BY machine_id ORDER BY received_at DESC, sent_at DESC) AS rn
               FROM machine_checkins) c
    ON c.machine_id = m.machine_id AND c.rn = 1
 WHERE m.retired_at IS NULL
 ORDER BY m.display_name`)
	if err != nil {
		return nil, fmt.Errorf("store: list machine compliance states: %w", err)
	}
	defer rows.Close()

	type row struct {
		state          MachineComplianceState
		facts          compliance.Facts
		reportedDigest string
	}
	now := s.now().UTC()
	var queue []row
	for rows.Next() {
		var r row
		var at, version, digest sql.NullString
		var free, total, jobs sql.NullInt64
		if err := rows.Scan(&r.state.MachineID, &r.state.DisplayName, &r.state.Channel,
			&at, &version, &free, &total, &jobs, &digest); err != nil {
			return nil, fmt.Errorf("store: scan machine compliance state: %w", err)
		}
		r.facts.EverCheckedIn = at.Valid
		if at.Valid {
			t := parseTime(at.String)
			r.state.ReportedAt = &t
			// Hub 自己的時鐘，不是機器說它幾點送出的。
			if age := now.Sub(t); age > 0 {
				r.facts.CheckinAge = age
			}
		}
		r.facts.AgentVersion = version.String
		r.facts.DiskFreeBytes, r.facts.DiskTotalBytes = free.Int64, total.Int64
		if jobs.Valid {
			enabled := jobs.Int64 == 1
			r.facts.JobsEnabled = &enabled
		}
		r.reportedDigest = digest.String
		queue = append(queue, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	known := map[string][]string{}
	out := make([]MachineComplianceState, 0, len(queue))
	for _, r := range queue {
		applied, err := s.settingsAppliedFact(known, r.state.MachineID, r.facts.EverCheckedIn, r.reportedDigest)
		if err != nil {
			return nil, err
		}
		r.facts.SettingsApplied = applied
		r.state.Effective = compliance.Resolve(r.state.MachineID, r.state.Channel, assignments)
		if r.state.Effective.Assigned() {
			r.state.Results = compliance.Evaluate(r.state.Effective.Policy, r.facts)
		}
		r.state.Verdict = compliance.Judge(r.state.Effective.Assigned(),
			r.facts.EverCheckedIn, r.state.Results)
		if len(r.state.Effective.Policy.Actions) > 0 {
			judge, err := s.settingsAppliedJudge(known, r.state.MachineID)
			if err != nil {
				return nil, err
			}
			r.state.Actions, err = s.complianceActions(r.state.MachineID,
				r.state.Effective.Policy, r.state.Verdict, judge, now)
			if err != nil {
				return nil, err
			}
		}
		r.state.EvaluatedAt = now
		out = append(out, r.state)
	}
	return out, nil
}

// settingsAppliedFact reuses the settings plane's own verdict rather than
// re-deciding what "applied" means. Anything short of 已套用 is not a fact the
// compliance rule may treat as measured: 落後一版 and 還沒回報 are different
// kinds of not-yet, and neither of them is false.
func (s *Store) settingsAppliedFact(cache map[string][]string, machineID string,
	everCheckedIn bool, reportedDigest string) (*bool, error) {
	if !everCheckedIn {
		return nil, nil
	}
	judge, err := s.settingsAppliedJudge(cache, machineID)
	if err != nil {
		return nil, err
	}
	return judge(reportedDigest), nil
}

// settingsAppliedJudge fixes one machine's settings material once so the same
// verdict can be asked of many reported digests without re-reading the policy.
// The board asks it about the newest check-in; the action window asks it about
// every check-in inside the grace period.
func (s *Store) settingsAppliedJudge(cache map[string][]string, machineID string) (func(string) *bool, error) {
	eff, err := s.ResolveMachineSettings(machineID)
	if err != nil {
		return nil, err
	}
	digests, err := s.knownSettingDigests(cache, eff)
	if err != nil {
		return nil, err
	}
	want := effectiveDigest(eff)
	return func(reportedDigest string) *bool {
		switch settingpolicy.Judge(want, digests,
			settingpolicy.Report{EverCheckedIn: true, ReportedDigest: reportedDigest}) {
		case settingpolicy.VerdictApplied:
			yes := true
			return &yes
		case settingpolicy.VerdictMismatch:
			// 它在跑的是這個 Hub 從來沒有送出去的設定。那是確定的不符合。
			no := false
			return &no
		default:
			return nil
		}
	}, nil
}

// ResolveMachineCompliance answers which rules judge one machine and how it
// does under them right now.
func (s *Store) ResolveMachineCompliance(machineID string) (MachineComplianceState, error) {
	states, err := s.MachineComplianceStates()
	if err != nil {
		return MachineComplianceState{}, err
	}
	for _, state := range states {
		if state.MachineID == machineID {
			return state, nil
		}
	}
	return MachineComplianceState{}, ErrNotFound
}
