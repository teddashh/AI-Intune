package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/settingpolicy"
)

// SettingPolicySummary is one policy as the console lists it: its newest
// revision, and how many machines are currently pinned to it.
type SettingPolicySummary struct {
	PolicyID      string                 `json:"policy_id"`
	Revision      int64                  `json:"policy_revision"`
	Digest        string                 `json:"settings_digest"`
	Settings      settingpolicy.Settings `json:"settings"`
	PublishedAt   time.Time              `json:"published_at"`
	RevisionCount int64                  `json:"revision_count"`
	Assignments   int64                  `json:"assignment_count"`
}

// SettingPolicies lists every policy by newest revision first.
func (s *Store) SettingPolicies() ([]SettingPolicySummary, error) {
	rows, err := s.db.Query(`
SELECT p.policy_id, p.policy_revision, p.settings_json, p.settings_digest, p.published_at,
       (SELECT COUNT(*) FROM setting_policies c WHERE c.policy_id = p.policy_id),
       (SELECT COUNT(*) FROM setting_assignments a
         WHERE a.policy_id = p.policy_id
           AND a.assignment_revision = (SELECT MAX(b.assignment_revision)
                                          FROM setting_assignments b
                                         WHERE b.scope_type = a.scope_type
                                           AND b.scope_id   = a.scope_id))
  FROM setting_policies p
 WHERE p.policy_revision = (SELECT MAX(q.policy_revision) FROM setting_policies q
                             WHERE q.policy_id = p.policy_id)
 ORDER BY p.published_at DESC, p.policy_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list setting policies: %w", err)
	}
	defer rows.Close()
	var out []SettingPolicySummary
	for rows.Next() {
		var r SettingPolicySummary
		var raw, at string
		if err := rows.Scan(&r.PolicyID, &r.Revision, &raw, &r.Digest, &at,
			&r.RevisionCount, &r.Assignments); err != nil {
			return nil, fmt.Errorf("store: scan setting policy: %w", err)
		}
		if r.Settings, err = settingpolicy.Parse([]byte(raw)); err != nil {
			return nil, fmt.Errorf("store: stored setting policy %s@%d is unreadable: %w",
				r.PolicyID, r.Revision, err)
		}
		r.PublishedAt = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SettingPolicyRevisions returns every published revision of one policy,
// newest first. The digests of the older revisions are what let the Hub say
// "behind" instead of "wrong" about a machine that has not checked in yet.
func (s *Store) SettingPolicyRevisions(policyID string) ([]SettingPolicyRecord, error) {
	rows, err := s.db.Query(`SELECT policy_id,policy_revision,settings_json,settings_digest,
	 published_at,published_by FROM setting_policies WHERE policy_id=?
	 ORDER BY policy_revision DESC`, policyID)
	if err != nil {
		return nil, fmt.Errorf("store: list setting policy revisions: %w", err)
	}
	defer rows.Close()
	var out []SettingPolicyRecord
	for rows.Next() {
		var r SettingPolicyRecord
		var raw, at string
		if err := rows.Scan(&r.PolicyID, &r.Revision, &raw, &r.Digest, &at, &r.PublishedBy); err != nil {
			return nil, fmt.Errorf("store: scan setting policy revision: %w", err)
		}
		if r.Settings, err = settingpolicy.Parse([]byte(raw)); err != nil {
			return nil, fmt.Errorf("store: stored setting policy %s@%d is unreadable: %w",
				r.PolicyID, r.Revision, err)
		}
		r.PublishedAt = parseTime(at)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrSettingPolicyNotFound
	}
	return out, nil
}

// CurrentSettingAssignments returns the newest assignment for every scope that
// has one. Superseded revisions stay in the table as history but never answer
// "what is in force".
func (s *Store) CurrentSettingAssignments() ([]SettingAssignmentRecord, error) {
	rows, err := s.db.Query(`
SELECT a.assignment_id, a.scope_type, a.scope_id, a.assignment_revision,
       a.policy_id, a.policy_revision, a.settings_digest, a.assigned_at, a.assigned_by,
       p.settings_json
  FROM setting_assignments a
  JOIN setting_policies p
    ON p.policy_id = a.policy_id AND p.policy_revision = a.policy_revision
 WHERE a.assignment_revision = (SELECT MAX(b.assignment_revision) FROM setting_assignments b
                                 WHERE b.scope_type = a.scope_type AND b.scope_id = a.scope_id)
 ORDER BY a.scope_type, a.scope_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list setting assignments: %w", err)
	}
	defer rows.Close()
	var out []SettingAssignmentRecord
	for rows.Next() {
		var r SettingAssignmentRecord
		var raw, at string
		if err := rows.Scan(&r.AssignmentID, &r.Scope, &r.ScopeID, &r.Revision, &r.PolicyID,
			&r.PolicyRev, &r.Digest, &at, &r.AssignedBy, &raw); err != nil {
			return nil, fmt.Errorf("store: scan setting assignment: %w", err)
		}
		if r.Settings, err = settingpolicy.Parse([]byte(raw)); err != nil {
			return nil, fmt.Errorf("store: stored setting policy %s@%d is unreadable: %w",
				r.PolicyID, r.PolicyRev, err)
		}
		r.AssignedAt = parseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveMachineSettings answers what one machine must run under right now.
// It is on the check-in path, so it reads only the two scopes that can match.
//
// A machine the registry does not know gets product defaults rather than an
// error: the caller already authenticated it, and refusing to answer would
// stop its heartbeat over a settings question.
func (s *Store) ResolveMachineSettings(machineID string) (settingpolicy.Effective, error) {
	var channel sql.NullString
	if err := s.db.QueryRow(`SELECT channel FROM machine_registry WHERE machine_id=?`,
		machineID).Scan(&channel); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return settingpolicy.Effective{}, fmt.Errorf("store: read machine channel for settings: %w", err)
	}
	rows, err := s.db.Query(`
SELECT a.scope_type, a.scope_id, a.policy_id, a.policy_revision, a.settings_digest,
       a.assigned_at, p.settings_json
  FROM setting_assignments a
  JOIN setting_policies p
    ON p.policy_id = a.policy_id AND p.policy_revision = a.policy_revision
 WHERE ((a.scope_type='machine' AND a.scope_id=?) OR (a.scope_type='channel' AND a.scope_id=?))
   AND a.assignment_revision = (SELECT MAX(b.assignment_revision) FROM setting_assignments b
                                 WHERE b.scope_type = a.scope_type AND b.scope_id = a.scope_id)`,
		machineID, channel.String)
	if err != nil {
		return settingpolicy.Effective{}, fmt.Errorf("store: resolve machine settings: %w", err)
	}
	defer rows.Close()
	var assignments []settingpolicy.Assignment
	for rows.Next() {
		var a settingpolicy.Assignment
		var raw string
		if err := rows.Scan(&a.Scope, &a.ScopeID, &a.PolicyID, &a.Revision, &a.Digest,
			&a.AssignedAt, &raw); err != nil {
			return settingpolicy.Effective{}, fmt.Errorf("store: scan resolved setting: %w", err)
		}
		if a.Settings, err = settingpolicy.Parse([]byte(raw)); err != nil {
			return settingpolicy.Effective{}, fmt.Errorf(
				"store: stored setting policy %s@%d is unreadable: %w", a.PolicyID, a.Revision, err)
		}
		assignments = append(assignments, a)
	}
	if err := rows.Err(); err != nil {
		return settingpolicy.Effective{}, err
	}
	return settingpolicy.Resolve(machineID, channel.String, assignments), nil
}

// MachineSettingState is one row of the applied-state board.
type MachineSettingState struct {
	MachineID      string                  `json:"machine_id"`
	DisplayName    string                  `json:"display_name"`
	Channel        string                  `json:"channel,omitempty"`
	Effective      settingpolicy.Effective `json:"effective"`
	Verdict        settingpolicy.Verdict   `json:"verdict"`
	ReportedDigest string                  `json:"reported_settings_digest,omitempty"`
	ReportedAt     *time.Time              `json:"reported_at,omitempty"`
}

// MachineSettingStates is the applied-state board for every machine still in
// the denominator.
//
// ⚠ 判決只看 agent 自己回報的 digest。不准用心跳間隔反推「它應該已經套用了」。
func (s *Store) MachineSettingStates() ([]MachineSettingState, error) {
	rows, err := s.db.Query(`
SELECT m.machine_id, m.display_name, COALESCE(m.channel,''),
       c.settings_digest, c.received_at
  FROM machine_registry m
  LEFT JOIN (SELECT machine_id, settings_digest, received_at,
                    ROW_NUMBER() OVER (PARTITION BY machine_id ORDER BY received_at DESC, sent_at DESC) AS rn
               FROM machine_checkins) c
    ON c.machine_id = m.machine_id AND c.rn = 1
 WHERE m.retired_at IS NULL
 ORDER BY m.display_name`)
	if err != nil {
		return nil, fmt.Errorf("store: list machine setting states: %w", err)
	}
	defer rows.Close()

	type pending struct {
		state    MachineSettingState
		everSeen bool
	}
	var queue []pending
	for rows.Next() {
		var p pending
		var digest, at sql.NullString
		if err := rows.Scan(&p.state.MachineID, &p.state.DisplayName, &p.state.Channel,
			&digest, &at); err != nil {
			return nil, fmt.Errorf("store: scan machine setting state: %w", err)
		}
		p.everSeen = at.Valid
		p.state.ReportedDigest = digest.String
		if at.Valid {
			t := parseTime(at.String)
			p.state.ReportedAt = &t
		}
		queue = append(queue, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	known := map[string][]string{}
	out := make([]MachineSettingState, 0, len(queue))
	for _, p := range queue {
		eff, err := s.ResolveMachineSettings(p.state.MachineID)
		if err != nil {
			return nil, err
		}
		p.state.Effective = eff
		digests, err := s.knownSettingDigests(known, eff)
		if err != nil {
			return nil, err
		}
		p.state.Verdict = settingpolicy.Judge(effectiveDigest(eff), digests,
			settingpolicy.Report{EverCheckedIn: p.everSeen, ReportedDigest: p.state.ReportedDigest})
		out = append(out, p.state)
	}
	return out, nil
}

// effectiveDigest is the digest the machine must echo. Defaults carry no
// policy, so their digest is computed from the values themselves.
func effectiveDigest(e settingpolicy.Effective) string {
	if e.Digest != "" {
		return e.Digest
	}
	return settingpolicy.MustDigest(e.Settings)
}

// knownSettingDigests returns every digest this machine could legitimately be
// echoing: the product defaults, plus every published revision of the policy
// it is assigned. Anything else is settings this Hub never sent.
func (s *Store) knownSettingDigests(cache map[string][]string, e settingpolicy.Effective) ([]string, error) {
	base := []string{settingpolicy.MustDigest(settingpolicy.Defaults())}
	if e.PolicyID == "" {
		return base, nil
	}
	if hit, ok := cache[e.PolicyID]; ok {
		return append(base, hit...), nil
	}
	revs, err := s.SettingPolicyRevisions(e.PolicyID)
	if err != nil {
		return nil, err
	}
	digests := make([]string, 0, len(revs))
	for _, r := range revs {
		digests = append(digests, r.Digest)
	}
	cache[e.PolicyID] = digests
	return append(base, digests...), nil
}
