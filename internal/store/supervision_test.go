package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSupervisionEmptyAndReadOnly(t *testing.T) {
	s := newDeployTestStore(t)
	ctx := context.Background()
	now := deployTestNow
	jobs, err := s.JobsSummaryContext(ctx, SupervisionFilter{}, now)
	if err != nil || len(jobs.Items) != 0 || jobs.Truncated {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	approvals, err := s.ApprovalsSummaryContext(ctx, "", now)
	if err != nil || len(approvals.Windows) != 2 || approvals.Windows[0].Applied != 0 || approvals.Windows[0].PreviewCreated != nil || approvals.PendingOlderThan4h != nil || approvals.UnavailableReason == "" {
		t.Fatalf("approvals: %+v %v", approvals, err)
	}
	status, err := s.HubStatusContext(ctx, now)
	if err != nil || status.Machines.Active != 0 || status.LastBackup != nil || status.LastRestoreDrill != nil || status.LastLitestreamSyncAt != nil {
		t.Fatalf("status: %+v %v", status, err)
	}
	db, wal, reason := s.SupervisionStorageSizes(ctx)
	if db == nil || *db <= 0 || wal == nil || reason != nil {
		t.Fatalf("storage: %v %v %v", db, wal, reason)
	}
	for _, table := range []string{"audit_log", "operator_idempotency", "jobs", "restore_drill_operations"} {
		var n int
		if err := s.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("read wrote %s: %d %v", table, n, err)
		}
	}
}

func addSummaryJob(t *testing.T, s *Store, id, machine, kind, script, state string, created time.Time, duration time.Duration) {
	t.Helper()
	spec, _ := json.Marshal(map[string]string{"kind": kind, "script_id": script})
	desired, rev, err := s.CreateDesiredState("machine", machine, "script", id, string(spec), "operator-a")
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateJob(machine, desired, rev, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	var terminal any
	if state != "not_started" && state != "running" {
		terminal = fmtTime(created.Add(duration))
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,created_at=?,terminal_at=? WHERE job_id=?`, state, fmtTime(created), terminal, job); err != nil {
		t.Fatal(err)
	}
}

func TestJobsSummaryFixturesWindowsAndFlags(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "machine-b")
	now := deployTestNow
	// Twenty slow recent successes and one failure; 400 older fast runs keep
	// the seven-day p95 below the recent p95.
	for i := 0; i < 400; i++ {
		addSummaryJob(t, s, fmt.Sprintf("baseline-%d", i), "machine-a", "script_v1", "daily-check", "succeeded", now.Add(-48*time.Hour), time.Second)
	}
	for i := 0; i < 20; i++ {
		addSummaryJob(t, s, fmt.Sprintf("slow-%d", i), "machine-a", "script_v1", "daily-check", "succeeded", now.Add(-time.Hour), 10*time.Second)
	}
	addSummaryJob(t, s, "failed", "machine-a", "script_v1", "daily-check", "failed", now.Add(-time.Hour), 20*time.Second)
	addSummaryJob(t, s, "old", "machine-b", "diagnostic_noop_v1", "", "succeeded", now.Add(-8*24*time.Hour), time.Second)
	addSummaryJob(t, s, "edge24", "machine-b", "diagnostic_noop_v1", "", "rejected", now.Add(-24*time.Hour), time.Second)
	addSummaryJob(t, s, "edge7", "machine-b", "diagnostic_noop_v1", "", "lease_expired", now.Add(-7*24*time.Hour), time.Second)
	addSummaryJob(t, s, "pending", "machine-b", "diagnostic_noop_v1", "", "not_started", now.Add(-time.Hour), 0)
	addSummaryJob(t, s, "future", "machine-b", "diagnostic_noop_v1", "", "failed", now.Add(time.Hour), time.Second)
	r, err := s.JobsSummaryContext(context.Background(), SupervisionFilter{Kind: "script_v1", MachineID: "machine-a", Window: "24h"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 1 {
		t.Fatalf("groups %+v", r)
	}
	a := r.Items[0]
	// Seven-day p95 is one second; the recent p95 triggers degradation.
	if a.Windows[0].Counts.Succeeded != 20 || a.Windows[0].Counts.Failed != 1 || a.Windows[0].Duration.Samples != 21 || *a.Windows[0].Duration.P95Seconds != 10 || *a.Flags.FailureRate24h || !*a.Flags.LatencyAboveBaseline || a.Flags.MissedDaily != nil || a.LastSuccessAt == nil || a.ScriptID == nil || *a.MachineID != "machine-a" {
		t.Fatalf("item %+v", a)
	}
	r, err = s.JobsSummaryContext(context.Background(), SupervisionFilter{Kind: "diagnostic_noop_v1", PerMachine: true}, now)
	if err != nil {
		t.Fatal(err)
	}
	a = r.Items[0]
	if a.Windows[0].Counts.Rejected != 1 || a.Windows[0].Counts.Pending != 1 || a.Windows[0].Counts.TimedOut != 0 || a.Windows[1].Counts.TimedOut != 1 || a.Windows[1].Counts.Succeeded != 0 || !*a.Flags.FailureRate24h || a.LastSuccessAt == nil {
		t.Fatalf("window boundaries %+v", a)
	}
}

func TestJobSupervisionThresholdBoundaries(t *testing.T) {
	now := deployTestNow
	cadence := int64(86400)
	last := now.Add(-26 * time.Hour)
	p95, base := 2.0, 1.0
	f := JobThresholdFlags(SupervisionCounts{Succeeded: 9, Failed: 1}, DurationPercentiles{P95Seconds: &p95}, DurationPercentiles{P95Seconds: &base}, &last, &cadence, now)
	if *f.MissedDaily || *f.FailureRate24h || *f.LatencyAboveBaseline {
		t.Fatalf("strict boundaries %+v", f)
	}
	last = last.Add(-time.Second)
	p95 = 2.01
	f = JobThresholdFlags(SupervisionCounts{Succeeded: 8, Rejected: 1, TimedOut: 1}, DurationPercentiles{P95Seconds: &p95}, DurationPercentiles{P95Seconds: &base}, &last, &cadence, now)
	if !*f.MissedDaily || !*f.FailureRate24h || !*f.LatencyAboveBaseline {
		t.Fatalf("thresholds %+v", f)
	}
	f = JobThresholdFlags(SupervisionCounts{}, DurationPercentiles{}, DurationPercentiles{}, nil, nil, now)
	if f.MissedDaily != nil || f.FailureRate24h != nil || f.LatencyAboveBaseline != nil {
		t.Fatalf("missing evidence %+v", f)
	}
}

func TestApprovalsSummaryAuditDefinition(t *testing.T) {
	s := newDeployTestStore(t)
	now := deployTestNow
	for i, e := range []AuditEntry{
		{At: now, Action: AuditDiagnosticNoop, OK: true, IdempotencyKey: "key-a", RequestDigest: "digest-a"},
		{At: now.Add(-24 * time.Hour), Action: AuditDeploymentCreate, OK: true, IdempotencyKey: "key-b", RequestDigest: "digest-b"},
		{At: now.Add(-7 * 24 * time.Hour), Action: AuditMachineNotes, OK: true, IdempotencyKey: "key-c", RequestDigest: "digest-c"},
		{At: now.Add(-8 * 24 * time.Hour), Action: AuditDiagnosticNoop, OK: true, IdempotencyKey: "old", RequestDigest: "digest-old"},
		{At: now, Action: AuditDiagnosticNoop, OK: true, IdempotencyKey: "replay", RequestDigest: "digest", Detail: OperatorIdempotencyReplayPrefix},
		{At: now, Action: AuditDiagnosticNoop, OK: true, IdempotencyKey: "transport", RequestDigest: "digest", Detail: OperatorTransportRejectionPrefix},
		{At: now, Action: AuditDiagnosticNoop, OK: false, IdempotencyKey: "failed", RequestDigest: "digest"},
		{At: now, Action: AuditMachineAssignedUser, OK: true, IdempotencyKey: "user", RequestDigest: "digest"},
		{At: now, Action: AuditConnect, OK: true},
		{At: now, Action: AuditDiagnosticNoop, OK: true},
	} {
		e.Subject = fmt.Sprintf("subject-%d", i)
		if err := s.RecordAudit(e); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.ApprovalsSummaryContext(context.Background(), "", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Windows[0].Applied != 2 || r.Windows[1].Applied != 3 || r.OldestPendingAgeSeconds != nil || r.PendingOlderThan4h != nil || r.Windows[0].PreviewToApply.P95Seconds != nil {
		t.Fatalf("approvals %+v", r)
	}
	r, err = s.ApprovalsSummaryContext(context.Background(), "7d", now)
	if err != nil || len(r.Windows) != 1 || r.Windows[0].Window != "7d" {
		t.Fatalf("window %+v %v", r, err)
	}
}

func TestSupervisionCapSuppressesThresholds(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	now := deployTestNow
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO jobs(job_id,machine_id,revision,state,created_at,terminal_at) VALUES (?,'machine-a',1,'failed',?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < SupervisionScanLimit+1; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("job-%d", i), fmtTime(now.Add(-time.Hour)), fmtTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r, err := s.JobsSummaryContext(context.Background(), SupervisionFilter{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || r.Scanned != SupervisionScanLimit || r.Items[0].Flags.FailureRate24h != nil {
		t.Fatalf("cap %+v", r)
	}
	var plan string
	rows, err := s.DB().Query(`EXPLAIN QUERY PLAN SELECT desired_id FROM jobs WHERE machine_id=? AND created_at<=? ORDER BY created_at DESC,revision DESC,job_id DESC LIMIT ?`, "machine-a", fmtTime(now), SupervisionScanLimit+1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if !strings.Contains(plan, "ix_jobs_read_machine_created") {
		t.Fatalf("missing bounded index: %s", plan)
	}
}

func TestHubStatusMachinesAndRestoreEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	now := deployTestNow
	for _, id := range []string{"machine-a", "machine-b", "machine-c", "machine-retired"} {
		registerDeployMachine(t, s, id)
	}
	if err := s.RetireMachine("machine-retired", now); err != nil {
		t.Fatal(err)
	}
	for id, at := range map[string]time.Time{"machine-a": now.Add(-10 * time.Minute), "machine-b": now.Add(-10*time.Minute - time.Second), "machine-retired": now} {
		if _, err := s.DB().Exec(`INSERT INTO machine_checkins(machine_id,sent_at,received_at) VALUES (?,?,?)`, id, fmtTime(at), fmtTime(at)); err != nil {
			t.Fatal(err)
		}
	}
	req := restoreDrillTestRequest("status-drill", "c")
	created, err := s.ApplyOperatorRestoreDrill(req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimRestoreDrillOperation(created.Operation.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SucceedRestoreDrillOperation(created.Operation.OperationID, claim.RunToken, RestoreDrillWorkerResult{Backup: req.Prepared.Backup, Machines: 3, Expected: 3, LiveExpected: 3, DurationMilliseconds: 125}); err != nil {
		t.Fatal(err)
	}
	r, err := s.HubStatusContext(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if r.LastRestoreDrill == nil || r.LastRestoreDrill.Result != "succeeded" || r.LastBackup == nil || r.LastBackup.SizeBytes != 4096 {
		t.Fatalf("restore evidence %+v", r)
	}
	if r.Machines.Active != 3 || r.Machines.Reporting != 1 || r.Machines.Stale != 2 {
		t.Fatalf("machine counts %+v", r.Machines)
	}
}
