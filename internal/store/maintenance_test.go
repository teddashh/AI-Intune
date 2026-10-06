package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/maintenance"
)

func TestDiskCleanProfileRevisionIsPerScopeAndIdempotent(t *testing.T) {
	s := newTestStore(t)
	shortWriterWait(s)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	machine := readyDiskMachine(t, s, "host-a", "", now)

	first := publishDiskProfile(t, s, "machine", machine, diskProfile(true), "pub-1")
	if first.Revision != 1 || first.Unchanged || first.Replayed || first.ConfigDigest == "" {
		t.Fatalf("first publish: %+v", first)
	}
	replay, err := s.ApplyDiskCleanProfile(diskProfileRequest("machine", machine, diskProfile(true), 1, firstPreviewDigest(t, s, "machine", machine, diskProfile(true)), "pub-1"))
	if err != nil || !replay.Replayed || replay.Revision != 1 || replay.DesiredID != first.DesiredID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM desired_state WHERE resource_kind=? AND resource_id=?`, maintenance.ResourceKind, maintenance.ResourceID); got != 1 {
		t.Fatalf("desired rows=%d", got)
	}

	changed := diskProfile(true)
	changed.TmpAgeDays = 9
	preview := mustDiskPreview(t, s, "machine", machine, changed)
	conflict := diskProfileRequest("machine", machine, changed, preview.CurrentRevision, preview.PreviewDigest, "pub-1")
	conflict.RequestDigest = digestOf("pub-1-other")
	_, err = s.ApplyDiskCleanProfile(conflict)
	if operatorCode(err) != OperatorCodeIdempotencyConflict {
		t.Fatalf("same key different body: %v", err)
	}

	channel := publishDiskProfile(t, s, "channel", "stable", diskProfile(false), "pub-channel")
	if channel.Revision != 2 {
		t.Fatalf("channel revision=%d", channel.Revision)
	}
	machinePreview := mustDiskPreview(t, s, "machine", machine, changed)
	if machinePreview.CurrentRevision != 1 {
		t.Fatalf("scope revision=%d, want the machine's own revision 1", machinePreview.CurrentRevision)
	}
	third := publishDiskProfile(t, s, "machine", machine, changed, "pub-3")
	if third.Revision != 3 {
		t.Fatalf("revision=%d, the shared counter must not be promised as current+1", third.Revision)
	}
	same := publishDiskProfile(t, s, "machine", machine, changed, "pub-same")
	if !same.Unchanged || same.Revision != 3 || same.DesiredID != third.DesiredID {
		t.Fatalf("unchanged republish: %+v", same)
	}

	stale := diskProfileRequest("machine", machine, changed, 1, machinePreview.PreviewDigest, "pub-stale")
	_, err = s.ApplyDiskCleanProfile(stale)
	if operatorCode(err) != OperatorCodeMaintenanceRevisionConflict {
		t.Fatalf("stale expected revision: %v", err)
	}

	bad := diskProfile(true)
	bad.TmpGlobRules = []maintenance.TmpGlobRule{{Glob: "/tmp/*openclaw*", MinAgeDays: 1}}
	_, err = s.PreviewDiskCleanProfile("machine", machine, bad)
	if operatorCode(err) != OperatorCodeMaintenanceProfileInvalid {
		t.Fatalf("protected glob: %v", err)
	}
	bad.TmpDirs = []string{"/var/lib"}
	_, err = s.PreviewDiskCleanProfile("machine", machine, bad)
	if operatorCode(err) != OperatorCodeMaintenanceProfileInvalid {
		t.Fatalf("tmp dir: %v", err)
	}
}

func TestDiskCleanCanaryPausesUntilContinue(t *testing.T) {
	s := newTestStore(t)
	shortWriterWait(s)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	hostA := readyDiskMachine(t, s, "host-a", "stable", now)
	hostB := readyDiskMachine(t, s, "host-b", "stable", now)
	published := publishDiskProfile(t, s, "channel", "stable", diskProfile(false), "pub-fleet")

	dryReq := DiskCleanTargetRequest{
		ScopeType: "channel", ScopeID: "stable", Revision: published.Revision,
		MachineIDs: []string{hostB, hostA},
	}
	dryPreview, err := s.PreviewDiskCleanDryRun(dryReq)
	if err != nil || len(dryPreview.Blockers) != 0 || dryPreview.ConfigDigest != published.ConfigDigest {
		t.Fatalf("dry-run preview: %+v %v", dryPreview, err)
	}
	dryReq.PreviewDigest = dryPreview.PreviewDigest
	dryReq.ConfirmScopeID = "stable"
	dryReq.Reason = "preview the cleanup"
	dryReq.IdempotencyKey = "dry-1"
	dryReq.RequestDigest = digestOf("dry-1")
	dryReq.Audit = AuditEntry{SourceAddr: "127.0.0.1"}
	dry, err := s.ApplyDiskCleanDryRun(dryReq)
	if err != nil || len(dry.JobIDs) != 2 {
		t.Fatalf("dry-run: %+v %v", dry, err)
	}
	replayed, err := s.ApplyDiskCleanDryRun(dryReq)
	if err != nil || !replayed.Replayed || len(replayed.JobIDs) != 2 {
		t.Fatalf("dry-run replay: %+v %v", replayed, err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM jobs WHERE desired_id=? AND irreversible=0`, dry.DesiredID); got != 2 {
		t.Fatalf("dry-run jobs=%d", got)
	}
	for _, jobID := range dry.JobIDs {
		succeedDiskJob(t, s, jobID, diskSummary(t, "dry-run", published.ConfigDigest, ""), now)
	}

	canaryReq := dryReq
	canaryReq.CanaryMachineID = hostA
	canaryReq.IdempotencyKey = "canary-1"
	canaryReq.RequestDigest = digestOf("canary-1")
	canaryReq.Reason = "canary one machine"
	canaryPreview, err := s.PreviewDiskCleanCanary(canaryReq)
	if err != nil || canaryPreview.CanaryMachineID != hostA || canaryPreview.PreviewDigest == dryPreview.PreviewDigest {
		t.Fatalf("canary preview: %+v %v", canaryPreview, err)
	}
	canaryReq.PreviewDigest = canaryPreview.PreviewDigest
	canary, err := s.ApplyDiskCleanCanary(canaryReq)
	if err != nil || canary.State != "canary" || canary.OpenedBatch != 1 || canary.CanaryMachineID != hostA {
		t.Fatalf("canary: %+v %v", canary, err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM jobs WHERE desired_id=? AND irreversible=1`, canary.DesiredID); got != 1 {
		t.Fatalf("canary jobs=%d", got)
	}
	var rest sql.NullString
	if err := s.DB().QueryRow(`SELECT job_id FROM maintenance_rollout_targets WHERE rollout_id=? AND machine_id=?`,
		canary.RolloutID, hostB).Scan(&rest); err != nil || rest.Valid {
		t.Fatalf("rest job=%v err=%v", rest, err)
	}
	if err := s.ReconcileDiskCleanRollouts(now); err != nil {
		t.Fatal(err)
	}
	if state := diskRolloutState(t, s, canary.RolloutID); state != "canary" {
		t.Fatalf("state while canary job is open: %s", state)
	}

	later := now.Add(time.Minute)
	s.nowFn = func() time.Time { return later }
	succeedDiskJob(t, s, canary.CanaryJobID, diskSummary(t, "apply", published.ConfigDigest, ""), later)
	if err := s.ReconcileDiskCleanRollouts(later); err != nil {
		t.Fatal(err)
	}
	if state := diskRolloutState(t, s, canary.RolloutID); state != "paused" {
		t.Fatalf("state after canary: %s", state)
	}

	control, err := s.PreviewDiskCleanContinue(canary.RolloutID)
	if err != nil || control.State != "paused" || control.ControlRevision != 1 || control.OpenedBatch != 1 {
		t.Fatalf("continue preview: %+v %v", control, err)
	}
	continued, err := s.ApplyDiskCleanContinue(DiskCleanControlRequest{
		RolloutID: canary.RolloutID, ExpectedControlRevision: control.ControlRevision,
		ExpectedOpenedBatch: control.OpenedBatch, PreviewDigest: control.PreviewDigest,
		ConfirmRolloutID: canary.RolloutID, Reason: "continue the rest",
		IdempotencyKey: "cont-1", RequestDigest: digestOf("cont-1"),
		Audit: AuditEntry{SourceAddr: "127.0.0.1"},
	})
	if err != nil || continued.State != "expanding" || continued.OpenedBatch != 2 || len(continued.JobIDs) != 1 {
		t.Fatalf("continue: %+v %v", continued, err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM jobs WHERE desired_id=? AND irreversible=1`, canary.DesiredID); got != 2 {
		t.Fatalf("apply jobs=%d", got)
	}
	if err := s.ReconcileDiskCleanRollouts(later); err != nil {
		t.Fatal(err)
	}
	if state := diskRolloutState(t, s, canary.RolloutID); state != "expanding" {
		t.Fatalf("state before the rest finishes: %s", state)
	}
}

func TestDiskCleanAbandonRefusesRunningJob(t *testing.T) {
	s := newTestStore(t)
	shortWriterWait(s)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	hostA := readyDiskMachine(t, s, "host-a", "stable", now)
	hostB := readyDiskMachine(t, s, "host-b", "stable", now)
	published := publishDiskProfile(t, s, "channel", "stable", diskProfile(false), "pub-abandon")
	dry := applyDiskDry(t, s, "stable", published, []string{hostA, hostB}, "dry-abandon")
	for _, jobID := range dry.JobIDs {
		succeedDiskJob(t, s, jobID, diskSummary(t, "dry-run", published.ConfigDigest, ""), now)
	}
	canary := applyDiskCanary(t, s, "stable", published, hostA, []string{hostA, hostB}, "canary-abandon")
	control, err := s.PreviewDiskCleanAbandon(canary.RolloutID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyDiskCleanAbandon(diskControlRequest(canary.RolloutID, control, "abandon-running"))
	if operatorCode(err) != OperatorCodeMaintenanceRolloutConflict {
		t.Fatalf("abandon while running: %v", err)
	}
	if state := diskRolloutState(t, s, canary.RolloutID); state != "canary" {
		t.Fatalf("state=%s", state)
	}

	later := now.Add(time.Minute)
	s.nowFn = func() time.Time { return later }
	succeedDiskJob(t, s, canary.CanaryJobID, diskSummary(t, "apply", published.ConfigDigest, ""), later)
	if err := s.ReconcileDiskCleanRollouts(later); err != nil {
		t.Fatal(err)
	}
	control, err = s.PreviewDiskCleanAbandon(canary.RolloutID)
	if err != nil || control.State != "paused" {
		t.Fatalf("abandon preview: %+v %v", control, err)
	}
	abandoned, err := s.ApplyDiskCleanAbandon(diskControlRequest(canary.RolloutID, control, "abandon-paused"))
	if err != nil || abandoned.State != "abandoned" || abandoned.ControlRevision != 2 {
		t.Fatalf("abandon: %+v %v", abandoned, err)
	}
	var rest sql.NullString
	if err := s.DB().QueryRow(`SELECT job_id FROM maintenance_rollout_targets WHERE rollout_id=? AND machine_id=?`,
		canary.RolloutID, hostB).Scan(&rest); err != nil || rest.Valid {
		t.Fatalf("rest job after abandon=%v %v", rest, err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM maintenance_assignments WHERE machine_id=?`, hostB); got != 1 {
		t.Fatalf("assignment after abandon=%d", got)
	}
}

func TestDiskCleanAlertsDedupeAndDiskIsIndependent(t *testing.T) {
	s := newTestStore(t)
	shortWriterWait(s)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	machine := readyDiskMachine(t, s, "host-a", "", now)
	assignDiskRule(t, s, machine, 20, now)
	writeDiskObservation(t, s, machine, "host-a", 5, 100, now)
	published := publishDiskProfile(t, s, "machine", machine, diskProfile(false), "pub-alert")

	dry := applyDiskDry(t, s, machine, published, []string{machine}, "dry-alert")
	sends := sweepDisk(t, s, now)
	if _, ok := sends[maintenance.AlertStale]; !ok || len(sends) != 1 {
		t.Fatalf("missing summary should alert stale only: %+v", sends)
	}
	markDiskSends(t, s, sends)
	if again := sweepDisk(t, s, now); len(again) != 0 {
		t.Fatalf("stale re-alerted: %+v", again)
	}

	succeedDiskJob(t, s, dry.JobIDs[0], diskSummary(t, "dry-run", published.ConfigDigest, "cache is large"), now)
	sends = sweepDisk(t, s, now)
	if _, ok := sends[maintenance.AlertAttention]; !ok {
		t.Fatalf("attention missing: %+v", sends)
	}
	if _, ok := sends[maintenance.AlertDiskOverThreshold]; ok {
		t.Fatalf("dry-run must not raise disk_over_threshold: %+v", sends)
	}
	markDiskSends(t, s, sends)
	if again := sweepDisk(t, s, now); len(again) != 0 {
		t.Fatalf("attention re-alerted: %+v", again)
	}

	canary := applyDiskCanary(t, s, machine, published, machine, []string{machine}, "canary-alert")
	appliedAt := now.Add(time.Minute)
	s.nowFn = func() time.Time { return appliedAt }
	succeedDiskJob(t, s, canary.CanaryJobID, diskSummary(t, "apply", published.ConfigDigest, ""), appliedAt)
	// The only observation predates the apply: it is a pre-clean reading and
	// must not raise "still over threshold after cleaning".
	sends = sweepDisk(t, s, appliedAt)
	if _, ok := sends[maintenance.AlertDiskOverThreshold]; ok {
		t.Fatalf("pre-clean observation raised disk_over_threshold: %+v", sends)
	}
	if view, err := s.DiskCleanSummary(machine, appliedAt); err != nil || view.Disk.Outcome != maintenance.OutcomeUnmeasured {
		t.Fatalf("pre-clean disk view: %+v %v", view.Disk, err)
	}
	writeDiskObservation(t, s, machine, "host-a", 5, 100, appliedAt)
	sends = sweepDisk(t, s, appliedAt)
	diskSend, ok := sends[maintenance.AlertDiskOverThreshold]
	if !ok || diskSend.Fingerprint != "below:20" {
		t.Fatalf("disk alert: %+v", sends)
	}
	if _, ok := sends[maintenance.AlertAttention]; ok {
		t.Fatalf("cleared attention re-alerted: %+v", sends)
	}
	markDiskSends(t, s, sends)
	if again := sweepDisk(t, s, appliedAt); len(again) != 0 {
		t.Fatalf("disk re-alerted: %+v", again)
	}

	writeDiskObservation(t, s, machine, "host-a", 80, 100, appliedAt.Add(time.Minute))
	if again := sweepDisk(t, s, appliedAt.Add(time.Minute)); len(again) != 0 {
		t.Fatalf("cleared disk alerted: %+v", again)
	}
	writeDiskObservation(t, s, machine, "host-a", 5, 100, appliedAt.Add(2*time.Minute))
	sends = sweepDisk(t, s, appliedAt.Add(2*time.Minute))
	if _, ok := sends[maintenance.AlertDiskOverThreshold]; !ok {
		t.Fatalf("disk should alert again after it recovered: %+v", sends)
	}

	staleAt := appliedAt.Add(49 * time.Hour)
	sends = sweepDisk(t, s, staleAt)
	if _, ok := sends[maintenance.AlertStale]; !ok {
		t.Fatalf("old summary should alert stale: %+v", sends)
	}
}

func TestDiskCleanSummaryVerdictAndDigestMismatch(t *testing.T) {
	s := newTestStore(t)
	shortWriterWait(s)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	if views, err := s.ListDiskCleanSummaries(now); err != nil || len(views) != 0 {
		t.Fatalf("empty list: %+v %v", views, err)
	}
	machine := readyDiskMachine(t, s, "host-a", "", now)
	if _, err := s.DiskCleanSummary("missing", now); operatorCode(err) != OperatorCodeMachineNotFound {
		t.Fatalf("missing machine: %v", err)
	}
	unassigned, err := s.DiskCleanSummary(machine, now)
	if err != nil || unassigned.Assigned || unassigned.Verdict != maintenance.OutcomeStale {
		t.Fatalf("unassigned: %+v %v", unassigned, err)
	}

	published := publishDiskProfile(t, s, "machine", machine, diskProfile(false), "pub-view")
	dry := applyDiskDry(t, s, machine, published, []string{machine}, "dry-view")
	wrong := digestOf("not-the-conf")
	succeedDiskJob(t, s, dry.JobIDs[0], diskSummary(t, "dry-run", wrong, "check me"), now)
	if got := countRows(t, s, `SELECT COUNT(*) FROM maintenance_summaries WHERE machine_id=?`, machine); got != 1 {
		t.Fatalf("summaries=%d", got)
	}
	view, err := s.DiskCleanSummary(machine, now)
	if err != nil || view.Verdict != maintenance.OutcomeFail || view.DigestMatches || view.Attention != "check me" {
		t.Fatalf("mismatch view: %+v %v", view, err)
	}
	if view.Summary == nil || view.Summary.Root != nil {
		t.Fatalf("summary projection: %+v", view.Summary)
	}
	views, err := s.ListDiskCleanSummaries(now)
	if err != nil || len(views) != 1 || views[0].MachineID != machine || views[0].DisplayName != "host-a" {
		t.Fatalf("list: %+v %v", views, err)
	}
}

func readyDiskMachine(t *testing.T, s *Store, name, channel string, now time.Time) string {
	t.Helper()
	id := mustEnroll(t, s, name, now)
	checkin := healthyCheckin(now)
	checkin.MaintenanceDiskCleanV1 = true
	if err := s.RecordCheckin(id, checkin, now); err != nil {
		t.Fatal(err)
	}
	writeDiskObservation(t, s, id, name, 50<<30, 100<<30, now)
	if channel != "" {
		if err := s.SetMachineChannel(id, channel); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func writeDiskObservation(t *testing.T, s *Store, machineID, name string, free, total int64, at time.Time) {
	t.Helper()
	batch := healthyBatch(at)
	batch.Identity.Hostname = name
	batch.Resources.DiskFreeBytes = free
	batch.Resources.DiskTotalBytes = total
	if err := s.RecordObservation(machineID, batch, at); err != nil {
		t.Fatal(err)
	}
}

func diskProfile(dry bool) maintenance.Profile {
	npm, pip, goc := 0, 0, 0
	thumb, trash := 30, 30
	timeout, depth := 30, 2
	return maintenance.Profile{
		SchemaVersion: maintenance.SchemaVersion, Scope: maintenance.ScopeUser, DryRun: &dry,
		Categories: []string{"user_tmp"}, TmpDirs: []string{"/tmp"}, TmpAgeDays: 7,
		NpmCleanMinMB: &npm, PipCacheMinMB: &pip, GoCacheMinMB: &goc,
		ThumbAgeDays: &thumb, TrashAgeDays: &trash, AttentionPct: 90, Mount: "/",
		DuTimeoutS: &timeout, DuDepth: &depth,
	}
}

func publishDiskProfile(t *testing.T, s *Store, scopeType, scopeID string, profile maintenance.Profile, key string) DiskCleanProfileResult {
	t.Helper()
	preview := mustDiskPreview(t, s, scopeType, scopeID, profile)
	result, err := s.ApplyDiskCleanProfile(diskProfileRequest(scopeType, scopeID, profile, preview.CurrentRevision, preview.PreviewDigest, key))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustDiskPreview(t *testing.T, s *Store, scopeType, scopeID string, profile maintenance.Profile) DiskCleanProfilePreview {
	t.Helper()
	preview, err := s.PreviewDiskCleanProfile(scopeType, scopeID, profile)
	if err != nil {
		t.Fatal(err)
	}
	return preview
}

func firstPreviewDigest(t *testing.T, s *Store, scopeType, scopeID string, profile maintenance.Profile) string {
	t.Helper()
	return mustDiskPreview(t, s, scopeType, scopeID, profile).PreviewDigest
}

func diskProfileRequest(scopeType, scopeID string, profile maintenance.Profile, expected int64, preview, key string) DiskCleanProfileRequest {
	return DiskCleanProfileRequest{
		ScopeType: scopeType, ScopeID: scopeID, Profile: profile, ExpectedRevision: &expected,
		PreviewDigest: preview, ConfirmScopeID: scopeID, Reason: "publish disk-clean",
		IdempotencyKey: key, RequestDigest: digestOf(key), Audit: AuditEntry{SourceAddr: "127.0.0.1"},
	}
}

func applyDiskDry(t *testing.T, s *Store, scopeID string, published DiskCleanProfileResult, machines []string, key string) DiskCleanDryRunResult {
	t.Helper()
	scopeType := "channel"
	if scopeID != "canary" && scopeID != "stable" {
		scopeType = "machine"
	}
	req := DiskCleanTargetRequest{ScopeType: scopeType, ScopeID: scopeID, Revision: published.Revision, MachineIDs: machines}
	preview, err := s.PreviewDiskCleanDryRun(req)
	if err != nil || len(preview.Blockers) != 0 {
		t.Fatalf("dry-run preview: %+v %v", preview, err)
	}
	req.PreviewDigest = preview.PreviewDigest
	req.ConfirmScopeID = scopeID
	req.Reason = "preview the cleanup"
	req.IdempotencyKey = key
	req.RequestDigest = digestOf(key)
	req.Audit = AuditEntry{SourceAddr: "127.0.0.1"}
	result, err := s.ApplyDiskCleanDryRun(req)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func applyDiskCanary(t *testing.T, s *Store, scopeID string, published DiskCleanProfileResult, canary string, machines []string, key string) DiskCleanCanaryResult {
	t.Helper()
	scopeType := "channel"
	if scopeID != "canary" && scopeID != "stable" {
		scopeType = "machine"
	}
	req := DiskCleanTargetRequest{
		ScopeType: scopeType, ScopeID: scopeID, Revision: published.Revision,
		MachineIDs: machines, CanaryMachineID: canary,
	}
	preview, err := s.PreviewDiskCleanCanary(req)
	if err != nil || len(preview.Blockers) != 0 {
		t.Fatalf("canary preview: %+v %v", preview, err)
	}
	req.PreviewDigest = preview.PreviewDigest
	req.ConfirmScopeID = scopeID
	req.Reason = "canary one machine"
	req.IdempotencyKey = key
	req.RequestDigest = digestOf(key)
	req.Audit = AuditEntry{SourceAddr: "127.0.0.1"}
	result, err := s.ApplyDiskCleanCanary(req)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func diskControlRequest(rolloutID string, preview DiskCleanControlPreview, key string) DiskCleanControlRequest {
	return DiskCleanControlRequest{
		RolloutID: rolloutID, ExpectedControlRevision: preview.ControlRevision,
		ExpectedOpenedBatch: preview.OpenedBatch, PreviewDigest: preview.PreviewDigest,
		ConfirmRolloutID: rolloutID, Reason: "stop the rollout",
		IdempotencyKey: key, RequestDigest: digestOf(key), Audit: AuditEntry{SourceAddr: "127.0.0.1"},
	}
}

func succeedDiskJob(t *testing.T, s *Store, jobID, stdout string, now time.Time) {
	t.Helper()
	job, err := s.Job(jobID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.ClaimJob(job.JobID, job.MachineID, now.Add(-time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceJobByAgent(job.JobID, job.MachineID, token, deploy.Start, now.Add(-40*time.Second)); err != nil {
		t.Fatal(err)
	}
	record := func() error {
		return s.RecordVerification(job.JobID, job.MachineID, token, maintenance.VerificationRuleID,
			"disk-clean --scope user --conf /private/disk-clean.conf", 0, stdout, "", true, now.Add(-time.Second))
	}
	if err := record(); err != nil {
		t.Fatal(err)
	}
	if err := record(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceJobByAgent(job.JobID, job.MachineID, token, deploy.FinishWork, now); err != nil {
		t.Fatal(err)
	}
	state, err := s.MarkSucceededIfVerified(job.JobID, now)
	if err != nil || state != deploy.Succeeded {
		t.Fatalf("succeed %s: %s %v", jobID, state, err)
	}
}

func diskSummary(t *testing.T, mode, digest, attention string) string {
	t.Helper()
	categoryMode := "dry-run"
	if mode == "apply" || mode == "mixed" {
		categoryMode = "apply"
	}
	raw, err := json.Marshal(maintenance.Summary{
		Schema: maintenance.SummarySchema, Version: maintenance.ScriptVersion, Scope: maintenance.ScopeUser,
		Host: "host-a", User: "agent", Mode: mode, TS: "2026-10-06T12:00:00Z",
		ConfigDigest: digest, Attention: attention,
		Disk: maintenance.SummaryDisk{Mount: "/", PctBefore: 10, PctAfter: 10, AvailBytesBefore: 1, AvailBytesAfter: 1},
		Categories: map[string]maintenance.SummaryCategory{
			"user_tmp": {Mode: categoryMode, Status: "ok", Note: "age>=7d"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.ParseSummary(raw); err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func diskRolloutState(t *testing.T, s *Store, rolloutID string) string {
	t.Helper()
	var state string
	if err := s.DB().QueryRow(`SELECT state FROM maintenance_rollouts WHERE rollout_id=?`, rolloutID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func assignDiskRule(t *testing.T, s *Store, machineID string, min int, now time.Time) {
	t.Helper()
	policy := compliance.Policy{SchemaVersion: compliance.SchemaVersion, Rules: []compliance.Rule{{
		Kind: compliance.RuleDiskFreeMinPercent, MinFreePercent: min,
	}}}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compliance.Parse(raw); err != nil {
		t.Fatal(err)
	}
	digest := digestOf(string(raw))
	if _, err := s.DB().Exec(`INSERT INTO compliance_policies
	 (policy_id, policy_revision, rules_json, rules_digest, published_at, published_by)
	 VALUES ('disk', 1, ?, ?, ?, 'test')`, string(raw), digest, fmtTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO compliance_assignments
	 (assignment_id, scope_type, scope_id, assignment_revision, policy_id, policy_revision,
	  rules_digest, assigned_at, assigned_by)
	 VALUES ('disk-assign', 'machine', ?, 1, 'disk', 1, ?, ?, 'test')`,
		machineID, digest, fmtTime(now)); err != nil {
		t.Fatal(err)
	}
}

func sweepDisk(t *testing.T, s *Store, now time.Time) map[string]DiskCleanAlertSend {
	t.Helper()
	sends, err := s.SweepDiskCleanAlerts(now)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]DiskCleanAlertSend{}
	for _, send := range sends {
		out[send.Condition] = send
	}
	return out
}

func markDiskSends(t *testing.T, s *Store, sends map[string]DiskCleanAlertSend) {
	t.Helper()
	for _, send := range sends {
		if err := s.MarkDiskCleanAlertDelivered(send.MachineID, send.Condition, send.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}
}

func operatorCode(err error) string {
	var op *OperatorRequestError
	if errors.As(err, &op) {
		return op.Code
	}
	return ""
}

// shortWriterWait makes a nested writer (a beginWrite, execWrite, or s.db call
// made while this goroutine already holds the single writer) fail in 250ms with
// ErrWriterBusy instead of hanging, so every disk-clean flow test doubles as a
// deadlock check.
func shortWriterWait(s *Store) { s.SetWriterWait(250 * time.Millisecond) }
