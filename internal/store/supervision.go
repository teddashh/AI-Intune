package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sort"
	"time"
)

// SupervisionScanLimit bounds historical rows examined, including for last-success
// evidence. Partial summaries explicitly suppress threshold decisions.
const SupervisionScanLimit = 10000
const SupervisionGroupLimit = 200

var ErrInvalidSupervision = errors.New("invalid supervision filter")

type SupervisionFilter struct {
	Kind       string `json:"kind"`
	MachineID  string `json:"machine_id"`
	Window     string `json:"window"`
	PerMachine bool   `json:"per_machine"`
}

type SupervisionCounts struct {
	Succeeded          int `json:"succeeded"`
	Failed             int `json:"failed"`
	Rejected           int `json:"rejected"`
	TimedOut           int `json:"timed_out"`
	Pending            int `json:"pending"`
	ManualIntervention int `json:"manual_intervention"`
	Unknown            int `json:"unknown"`
}

type DurationPercentiles struct {
	Samples    int      `json:"samples"`
	P50Seconds *float64 `json:"p50_seconds"`
	P95Seconds *float64 `json:"p95_seconds"`
}

type JobSummaryWindow struct {
	Window   string              `json:"window"`
	Counts   SupervisionCounts   `json:"counts"`
	Duration DurationPercentiles `json:"duration"`
}

type JobSummaryFlags struct {
	MissedDaily          *bool `json:"missed_daily"`
	FailureRate24h       *bool `json:"failure_rate_24h"`
	LatencyAboveBaseline *bool `json:"latency_above_baseline"`
}

type JobSummary struct {
	Kind           string             `json:"kind"`
	ScriptID       *string            `json:"script_id"`
	MachineID      *string            `json:"machine_id"`
	CadenceSeconds *int64             `json:"cadence_seconds"`
	LastSuccessAt  *time.Time         `json:"last_success_at"`
	Windows        []JobSummaryWindow `json:"windows"`
	Flags          JobSummaryFlags    `json:"flags"`
}

type JobsSummary struct {
	SchemaVersion            int               `json:"schema_version"`
	EvaluatedAt              time.Time         `json:"evaluated_at"`
	Filters                  SupervisionFilter `json:"filters"`
	ScanLimit                int               `json:"scan_limit"`
	Scanned                  int               `json:"scanned"`
	Truncated                bool              `json:"truncated"`
	DurationBasis            string            `json:"duration_basis"`
	CadenceUnavailableReason string            `json:"cadence_unavailable_reason"`
	Items                    []JobSummary      `json:"items"`
}

type jobSummaryAccumulator struct {
	item      JobSummary
	counts    [2]SupervisionCounts
	durations [2][]float64
}

// JobsSummaryContext reads a bounded newest-job cohort through the query-only
// pool. The CTE cap precedes spec parsing and joins; machine filtering uses the
// existing machine/created index. Never selects leases, output, or full specs.
func (s *Store) JobsSummaryContext(ctx context.Context, f SupervisionFilter, now time.Time) (JobsSummary, error) {
	result := JobsSummary{SchemaVersion: 1, EvaluatedAt: now.UTC(), Filters: f, ScanLimit: SupervisionScanLimit,
		DurationBasis: "created_at_to_terminal_at", CadenceUnavailableReason: "no persisted scheduler or cadence", Items: []JobSummary{}}
	query := `WITH cohort AS MATERIALIZED (SELECT desired_id,machine_id,state,created_at,terminal_at FROM jobs WHERE created_at <= ?`
	args := []any{fmtTime(now)}
	if f.MachineID != "" {
		query += ` AND machine_id = ?`
		args = append(args, f.MachineID)
	}
	query += ` ORDER BY created_at DESC,revision DESC,job_id DESC LIMIT ?) SELECT
 COALESCE(CASE WHEN json_valid(substr(d.spec,1,16384)) THEN CAST(json_extract(substr(d.spec,1,16384),'$.kind') AS TEXT) END,d.resource_kind,'unknown'),
 CASE WHEN json_valid(substr(d.spec,1,16384)) THEN CAST(json_extract(substr(d.spec,1,16384),'$.script_id') AS TEXT) END,
 j.machine_id,j.state,j.created_at,COALESCE(j.terminal_at,'') FROM cohort j LEFT JOIN desired_state d ON d.desired_id=j.desired_id`
	args = append(args, SupervisionScanLimit+1)
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	type key struct{ kind, script, machine string }
	groups := map[key]*jobSummaryAccumulator{}
	for rows.Next() {
		if result.Scanned == SupervisionScanLimit {
			result.Truncated = true
			break
		}
		result.Scanned++
		var kind, machine, state, created, terminal string
		var script sql.NullString
		if err := rows.Scan(&kind, &script, &machine, &state, &created, &terminal); err != nil {
			return result, err
		}
		if f.Kind != "" && kind != f.Kind {
			continue
		}
		k := key{kind: kind, script: script.String}
		if f.PerMachine || f.MachineID != "" {
			k.machine = machine
		}
		a := groups[k]
		if a == nil {
			if len(groups) == SupervisionGroupLimit {
				result.Truncated = true
				continue
			}
			a = &jobSummaryAccumulator{item: JobSummary{Kind: kind}}
			if script.Valid && script.String != "" {
				v := script.String
				a.item.ScriptID = &v
			}
			if k.machine != "" {
				v := machine
				a.item.MachineID = &v
			}
			groups[k] = a
		}
		at, e := time.Parse(time.RFC3339, created)
		if e != nil {
			return result, errors.New("invalid job creation timestamp")
		}
		var end time.Time
		if terminal != "" {
			end, e = time.Parse(time.RFC3339, terminal)
			if e != nil {
				return result, errors.New("invalid job terminal timestamp")
			}
		}
		if state == "succeeded" && !end.IsZero() && !end.After(now) && (a.item.LastSuccessAt == nil || end.After(*a.item.LastSuccessAt)) {
			v := end
			a.item.LastSuccessAt = &v
		}
		for i, span := range []time.Duration{24 * time.Hour, 7 * 24 * time.Hour} {
			if at.Before(now.Add(-span)) {
				continue
			}
			c := &a.counts[i]
			switch state {
			case "succeeded":
				c.Succeeded++
			case "failed":
				c.Failed++
			case "rejected":
				c.Rejected++
			case "lease_expired":
				c.TimedOut++
			case "not_started", "claimed", "running", "verifying":
				c.Pending++
			case "manual_intervention":
				c.ManualIntervention++
			default:
				c.Unknown++
			}
			if !end.IsZero() && !end.Before(at) && !end.After(now) {
				a.durations[i] = append(a.durations[i], end.Sub(at).Seconds())
			}
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.script != b.script {
			return a.script < b.script
		}
		return a.machine < b.machine
	})
	for _, k := range keys {
		a := groups[k]
		daily, baseline := percentiles(a.durations[0]), percentiles(a.durations[1])
		if !result.Truncated && a.counts[0].Unknown == 0 {
			a.item.Flags = JobThresholdFlags(a.counts[0], daily, baseline, a.item.LastSuccessAt, nil, now)
		}
		for i, w := range []string{"24h", "7d"} {
			if f.Window == "" || f.Window == w {
				p := daily
				if i == 1 {
					p = baseline
				}
				a.item.Windows = append(a.item.Windows, JobSummaryWindow{Window: w, Counts: a.counts[i], Duration: p})
			}
		}
		result.Items = append(result.Items, a.item)
	}
	return result, nil
}

// JobThresholdFlags uses strict thresholds. A missing baseline or unknown
// cadence produces null, rather than inventing a healthy result.
func JobThresholdFlags(c SupervisionCounts, daily, baseline DurationPercentiles, last *time.Time, cadence *int64, now time.Time) JobSummaryFlags {
	f := JobSummaryFlags{}
	if cadence != nil && *cadence == 86400 {
		v := last == nil || now.Sub(*last) > 26*time.Hour
		f.MissedDaily = &v
	}
	failures := c.Failed + c.Rejected + c.TimedOut + c.ManualIntervention
	completed := c.Succeeded + failures
	if completed > 0 {
		v := float64(failures)/float64(completed) > 0.10
		f.FailureRate24h = &v
	}
	if daily.P95Seconds != nil && baseline.P95Seconds != nil {
		v := *daily.P95Seconds > 2**baseline.P95Seconds
		f.LatencyAboveBaseline = &v
	}
	return f
}

// Nearest-rank percentiles, including a one-sample cohort.
func percentiles(values []float64) DurationPercentiles {
	p := DurationPercentiles{Samples: len(values)}
	if len(values) == 0 {
		return p
	}
	sort.Float64s(values)
	a, b := values[(len(values)*50+99)/100-1], values[(len(values)*95+99)/100-1]
	p.P50Seconds = &a
	p.P95Seconds = &b
	return p
}

type ApprovalSummaryWindow struct {
	Window         string              `json:"window"`
	PreviewCreated *int                `json:"preview_created"`
	Applied        int                 `json:"applied"`
	Expired        *int                `json:"expired"`
	Abandoned      *int                `json:"abandoned"`
	PreviewToApply DurationPercentiles `json:"preview_to_apply"`
}

type ApprovalsSummary struct {
	SchemaVersion           int                     `json:"schema_version"`
	EvaluatedAt             time.Time               `json:"evaluated_at"`
	ScanLimit               int                     `json:"scan_limit"`
	Scanned                 int                     `json:"scanned"`
	Truncated               bool                    `json:"truncated"`
	Definition              string                  `json:"definition"`
	UnavailableReason       string                  `json:"unavailable_reason"`
	Windows                 []ApprovalSummaryWindow `json:"windows"`
	OldestPendingAgeSeconds *float64                `json:"oldest_pending_age_seconds"`
	PendingOlderThan4h      *bool                   `json:"pending_older_than_4h"`
}

// Successful previews are pure reads and never persisted in this schema.
// Only correlated successful canonical mutations are observable approvals.
// Replays, transport failures, and non-preview session/user operations do not
// represent a new approval. No audit details or correlation digests leave here.
func (s *Store) ApprovalsSummaryContext(ctx context.Context, window string, now time.Time) (ApprovalsSummary, error) {
	r := ApprovalsSummary{SchemaVersion: 1, EvaluatedAt: now.UTC(), ScanLimit: SupervisionScanLimit,
		Definition:        "applied = successful canonical preview-gated audit mutation with idempotency key and request digest; excludes replays and transport rejections",
		UnavailableReason: "successful previews and pending approvals are not persisted; created, expired, abandoned, pending age and preview-to-apply timing are unobservable", Windows: []ApprovalSummaryWindow{}}
	var counts [2]int
	rows, err := s.rdb.QueryContext(ctx, `SELECT at,action,outcome,COALESCE(idempotency_key,''),COALESCE(request_digest,''),substr(COALESCE(detail,''),1,128) FROM audit_log WHERE at >= ? AND at <= ? ORDER BY at DESC,audit_id DESC LIMIT ?`, fmtTime(now.Add(-7*24*time.Hour)), fmtTime(now), SupervisionScanLimit+1)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		if r.Scanned == SupervisionScanLimit {
			r.Truncated = true
			break
		}
		r.Scanned++
		var at, action, outcome, key, digest, detail string
		if err := rows.Scan(&at, &action, &outcome, &key, &digest, &detail); err != nil {
			return r, err
		}
		e := AuditEntry{Action: AuditAction(action), Detail: detail}
		if !IsCanonicalOperatorAction(e.Action) || e.Action == AuditMachineAssignedUser || e.Action == AuditAgentSessionOpen || e.Action == AuditAgentSessionClose || outcome != "ok" || key == "" || digest == "" || e.IsOperatorReplay() || e.IsOperatorTransportRejection() {
			continue
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return r, errors.New("invalid audit timestamp")
		}
		counts[1]++
		if !t.Before(now.Add(-24 * time.Hour)) {
			counts[0]++
		}
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	for i, w := range []string{"24h", "7d"} {
		if window == "" || window == w {
			r.Windows = append(r.Windows, ApprovalSummaryWindow{Window: w, Applied: counts[i]})
		}
	}
	return r, nil
}

type HubMachineCounts struct {
	Active    int `json:"active"`
	Reporting int `json:"reporting"`
	Stale     int `json:"stale"`
}
type HubRestoreDrill struct {
	Result     string    `json:"result"`
	FinishedAt time.Time `json:"finished_at"`
}
type HubBackup struct {
	ModifiedAt time.Time `json:"modified_at"`
	SizeBytes  int64     `json:"size_bytes"`
	Source     string    `json:"source"`
}
type HubStatus struct {
	SchemaVersion               int              `json:"schema_version"`
	EvaluatedAt                 time.Time        `json:"evaluated_at"`
	Version                     string           `json:"version"`
	UptimeSeconds               *float64         `json:"uptime_seconds"`
	DBSizeBytes                 *int64           `json:"db_size_bytes"`
	WALSizeBytes                *int64           `json:"wal_size_bytes"`
	StorageUnavailableReason    *string          `json:"storage_unavailable_reason"`
	LastLitestreamSyncAt        *time.Time       `json:"last_litestream_sync_at"`
	LitestreamUnavailableReason string           `json:"litestream_unavailable_reason"`
	LastRestoreDrill            *HubRestoreDrill `json:"last_restore_drill"`
	LastBackup                  *HubBackup       `json:"last_backup"`
	BackupUnavailableReason     *string          `json:"backup_unavailable_reason"`
	Machines                    HubMachineCounts `json:"machines"`
}

func (s *Store) HubStatusContext(ctx context.Context, now time.Time) (HubStatus, error) {
	r := HubStatus{SchemaVersion: 1, EvaluatedAt: now.UTC(), LitestreamUnavailableReason: "no local Litestream sync telemetry is recorded"}
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN
 (SELECT received_at FROM machine_checkins c WHERE c.machine_id=m.machine_id AND received_at <= ? ORDER BY received_at DESC LIMIT 1) >= ? THEN 1 ELSE 0 END),0)
 FROM machine_registry m WHERE retired_at IS NULL`, fmtTime(now), fmtTime(now.Add(-10*time.Minute))).Scan(&r.Machines.Active, &r.Machines.Reporting)
	if err != nil {
		return r, err
	}
	r.Machines.Stale = r.Machines.Active - r.Machines.Reporting
	var state, finished string
	err = s.rdb.QueryRowContext(ctx, `SELECT state,finished_at FROM restore_drill_operations WHERE state IN ('succeeded','failed') ORDER BY created_at DESC,operation_id DESC LIMIT 1`).Scan(&state, &finished)
	if err == nil {
		at, e := time.Parse(time.RFC3339, finished)
		if e != nil {
			return r, e
		}
		r.LastRestoreDrill = &HubRestoreDrill{Result: state, FinishedAt: at}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	var modified string
	var size int64
	err = s.rdb.QueryRowContext(ctx, `SELECT backup_modified_at,backup_size_bytes FROM restore_drill_operations ORDER BY created_at DESC,operation_id DESC LIMIT 1`).Scan(&modified, &size)
	if err == nil {
		at, e := time.Parse(time.RFC3339, modified)
		if e != nil {
			return r, e
		}
		r.LastBackup = &HubBackup{ModifiedAt: at, SizeBytes: size, Source: "restore_drill_operations"}
	} else if errors.Is(err, sql.ErrNoRows) {
		reason := "no backup evidence in restore drill operations"
		r.BackupUnavailableReason = &reason
	} else {
		return r, err
	}
	return r, nil
}

// The database filename is used locally only and is never returned or included
// in an error. Missing WAL is a known zero; unobservable storage is null.
func (s *Store) SupervisionStorageSizes(ctx context.Context) (*int64, *int64, *string) {
	unknown := "database storage size is not observable locally"
	rows, err := s.rdb.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return nil, nil, &unknown
	}
	var path string
	for rows.Next() {
		var sequence int
		var name, file string
		if err = rows.Scan(&sequence, &name, &file); err != nil {
			break
		}
		if name == "main" {
			path = file
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil || rowErr != nil || path == "" {
		return nil, nil, &unknown
	}
	db, err := os.Stat(path)
	if err != nil || !db.Mode().IsRegular() {
		return nil, nil, &unknown
	}
	size := db.Size()
	walSize := int64(0)
	wal, err := os.Stat(path + "-wal")
	if err == nil && wal.Mode().IsRegular() {
		walSize = wal.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return &size, nil, &unknown
	}
	return &size, &walSize, nil
}
