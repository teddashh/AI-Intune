package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"
)

func TestObservationProcessScanControlsCanaryFailureReason(t *testing.T) {
	tests := []struct {
		name        string
		processScan string
		reason      string
		want        string
		notWant     string
	}{
		{
			name:        "unavailable 是偵測警告",
			processScan: model.ProcessScanUnavailable,
			reason:      "讀不到 /proc，這台的 process 偵測整個是關的",
			want:        "偵測是關的",
			notWant:     "process 不存在",
		},
		{
			name:        "complete 是缺席判決",
			processScan: model.ProcessScanComplete,
			reason:      "掃了 312 個 process，沒有一個對得上",
			want:        "process 不存在",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			const machineID = "cnode"
			addRolloutMachine(t, s, machineID, "samplehub1", false)
			now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
			batch := healthyBatch(now)
			batch.CLITools = []model.CLITool{{
				Name:          "openclaw",
				Present:       true,
				RunningPID:    0,
				ProcessScan:   tc.processScan,
				RunningReason: tc.reason,
			}}
			projected, _ := s.workloadFactsFromObservation("samplehub1", batch, now)
			if projected.OpenClawProcessScan != tc.processScan {
				t.Fatalf("batch projection process scan = %q，want %q", projected.OpenClawProcessScan, tc.processScan)
			}
			if err := s.RecordObservation(machineID, batch, now); err != nil {
				t.Fatalf("record observation: %v", err)
			}

			failures, err := s.CanarySilentFailuresSince([]string{machineID}, now.Add(-time.Minute), now)
			if err != nil {
				t.Fatalf("read canary silent failures: %v", err)
			}
			if len(failures) != 1 {
				t.Fatalf("failure intervals = %d，want 1：%+v", len(failures), failures)
			}
			if !strings.Contains(failures[0].Reason, tc.want) {
				t.Errorf("failure reason = %q，want 含 %q", failures[0].Reason, tc.want)
			}
			if tc.notWant != "" && strings.Contains(failures[0].Reason, tc.notWant) {
				t.Errorf("failure reason = %q，不該含 %q", failures[0].Reason, tc.notWant)
			}
		})
	}
}

func TestOpenMigratesNewestLegacyFailureOpenUntilExplicitRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	older := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	newer := older.Add(2 * time.Hour)
	for _, row := range []struct {
		reason string
		at     time.Time
	}{{"older legacy", older}, {"newest legacy", newer}} {
		if _, err := s.DB().Exec(`INSERT INTO canary_silent_failures(machine_id,reason,first_seen_at,last_seen_at,open)
		 VALUES('cnode',?,?,?,0)`, row.reason, fmtTime(row.at), fmtTime(row.at)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(`ALTER TABLE canary_silent_failures DROP COLUMN open`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("Open 舊 schema：%v", err)
	}
	defer s.Close()
	have, err := columnSet(s.DB(), "canary_silent_failures")
	if err != nil || !have["open"] {
		t.Fatalf("migration 沒補 open：have=%v err=%v", have, err)
	}
	rows, err := s.DB().Query(`SELECT reason,open FROM canary_silent_failures WHERE machine_id='cnode' ORDER BY first_seen_at`)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		reason string
		open   int
	}{{"older legacy", 0}, {"newest legacy", 1}}
	seen := 0
	for ; rows.Next(); seen++ {
		if seen >= len(want) {
			t.Fatal("migration 長出多餘 failure row")
		}
		var reason string
		var open int
		if err := rows.Scan(&reason, &open); err != nil || reason != want[seen].reason || open != want[seen].open {
			t.Fatalf("legacy row %d reason=%q open=%d err=%v", seen, reason, open, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if seen != len(want) {
		t.Fatalf("migration rows=%d，預期 %d", seen, len(want))
	}
	s.SetExpectations(&expect.Set{})
	if err := s.PublishExpectationsPolicy(newer); err != nil {
		t.Fatal(err)
	}

	recovery := newer.Add(time.Hour)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: recovery, AgentStartedAt: recovery}, recovery); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", healthyBatch(recovery), recovery); err != nil {
		t.Fatal(err)
	}
	var last string
	var open int
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures
	 WHERE machine_id='cnode' AND reason='newest legacy'`).Scan(&last, &open); err != nil || last != fmtTime(recovery) || open != 0 {
		t.Fatalf("explicit recovery 沒關 latest legacy span：last=%q open=%d err=%v", last, open, err)
	}
	finished := newer.Add(30 * time.Minute)
	failures, err := s.CanarySilentFailuresSince([]string{"cnode"}, finished, recovery.Add(time.Hour))
	if err != nil || len(failures) != 1 || failures[0].Reason != "newest legacy" {
		t.Fatalf("跨 finished 的 legacy interval 消失：failures=%+v err=%v", failures, err)
	}
}

func TestObservationProjectionFailureRollsBackRawRowsWhenEvidenceTableIsUnavailable(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	var before int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM observed_state WHERE machine_id='cnode'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP TABLE canary_silent_failures`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	err := s.RecordObservation("cnode", model.ObservationBatch{
		MeasuredAt: now,
		OpenClaw:   model.OpenClaw{Present: true},
		CLITools:   []model.CLITool{{Name: "openclaw", Present: true, RunningReason: "掃過 process，沒有找到 OpenClaw"}},
	}, now)
	if err == nil || !strings.Contains(err.Error(), "canary_silent_failures") {
		t.Fatalf("projection table 不可用仍收下 raw observation：%v", err)
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM observed_state WHERE machine_id='cnode'`).Scan(&rows); err != nil || rows != before {
		t.Fatalf("projection 失敗卻留下 raw rows：before=%d rows=%d err=%v", before, rows, err)
	}
}

func TestObservationRecoveryRollsBackRawRowsWhenEvidenceTableIsUnavailable(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCanarySilentFailure("cnode", "existing failure", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM observed_state WHERE machine_id='cnode'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP TABLE canary_silent_failures`); err != nil {
		t.Fatal(err)
	}
	err := s.RecordObservation("cnode", healthyBatch(now), now)
	if err == nil || !strings.Contains(err.Error(), "canary_silent_failures") {
		t.Fatalf("recovery projection table 不可用仍收下 raw observation：%v", err)
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM observed_state WHERE machine_id='cnode'`).Scan(&rows); err != nil || rows != before {
		t.Fatalf("recovery projection 失敗卻留下 raw rows：before=%d rows=%d err=%v", before, rows, err)
	}
}

func TestObservationProjectionUsesBatchArtifactAndEventFactsThenClosesOnRecovery(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	failedAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	s.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{{
		Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof.jsonl",
		MaxAgeSeconds: 3600, Why: "外部驗證器應持續出證據",
		Events: &model.EventSpec{TsField: "at", TypeField: "kind", NotOK: []string{"broken"}, WindowSeconds: 3600},
	}}})
	if err := s.PublishExpectationsPolicy(failedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: failedAt, AgentStartedAt: failedAt}, failedAt); err != nil {
		t.Fatal(err)
	}
	modTime := failedAt.Add(-time.Minute)
	bad := healthyBatch(failedAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &bad)
	bad.Artifacts = []model.ArtifactCheck{{
		Unit: "proof.service", Artifact: "/tmp/proof.jsonl", Exists: true, ModTime: &modTime,
	}}
	bad.Events = []model.EventStream{{
		Unit: "proof.service", Path: "/tmp/proof.jsonl",
		Declared: []model.EventSummary{{Type: "broken", Count: 1}},
	}}
	if err := s.RecordObservation("cnode", bad, failedAt); err != nil {
		t.Fatal(err)
	}
	var reason string
	var open int
	if err := s.DB().QueryRow(`SELECT reason,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&reason, &open); err != nil || open != 1 || !strings.Contains(reason, "broken ×1") {
		t.Fatalf("batch event 沒開 failure span：reason=%q open=%d err=%v", reason, open, err)
	}

	unknownArtifactAt := failedAt.Add(time.Minute)
	unknownArtifact := healthyBatch(unknownArtifactAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &unknownArtifact)
	if err := s.RecordObservation("cnode", unknownArtifact, unknownArtifactAt); err != nil {
		t.Fatal(err)
	}
	var last string
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 1 || last != fmtTime(failedAt) {
		t.Fatalf("缺 Artifact/Event 的 unknown batch 改動了 span：last=%q open=%d err=%v", last, open, err)
	}

	// Artifact 有量到，但 EventSpec 對應的 EventStream 沒量到，仍然是
	// unknown。這單獨擋住 state.EventFact.Measured=false，不讓 artifact 缺列幫它擋過 bug。
	unknownEventAt := unknownArtifactAt.Add(time.Minute)
	modTime = unknownEventAt
	unknownEvent := healthyBatch(unknownEventAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &unknownEvent)
	unknownEvent.Artifacts = []model.ArtifactCheck{{
		Unit: "proof.service", Artifact: "/tmp/proof.jsonl", Exists: true, ModTime: &modTime,
	}}
	facts, complete := s.workloadFactsFromObservation("samplehub1", unknownEvent, unknownEventAt)
	if complete || len(facts.Artifacts) != 1 || facts.Artifacts[0].Events == nil || facts.Artifacts[0].Events.Measured {
		t.Fatalf("缺 EventStream 卻被當成完整量測：complete=%v facts=%+v", complete, facts.Artifacts)
	}
	if err := s.RecordObservation("cnode", unknownEvent, unknownEventAt); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 1 || last != fmtTime(failedAt) {
		t.Fatalf("缺 EventStream 的 unknown batch 改動了 span：last=%q open=%d err=%v", last, open, err)
	}
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: unknownEventAt, AgentStartedAt: unknownEventAt}, unknownEventAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileFleet(unknownEventAt); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("partial batch 後 reconcile 關掉了 span：open=%d err=%v", open, err)
	}

	partialAt := unknownEventAt.Add(time.Minute)
	modTime = partialAt
	coveredFrom := partialAt.Add(-time.Minute)
	partial := healthyBatch(partialAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &partial)
	partial.Artifacts = []model.ArtifactCheck{{
		Unit: "proof.service", Artifact: "/tmp/proof.jsonl", Exists: true, ModTime: &modTime,
	}}
	partial.Events = []model.EventStream{{
		Unit: "proof.service", Path: "/tmp/proof.jsonl", Truncated: true, CoveredFrom: &coveredFrom,
		Declared: []model.EventSummary{{Type: "broken", Count: 0}},
	}}
	if err := s.RecordObservation("cnode", partial, partialAt); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("partial zero-hit event 量測關掉了 span：open=%d err=%v", open, err)
	}

	malformedAt := partialAt.Add(time.Minute)
	modTime = malformedAt
	malformed := healthyBatch(malformedAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &malformed)
	malformed.Artifacts = []model.ArtifactCheck{{
		Unit: "proof.service", Artifact: "/tmp/proof.jsonl", Exists: true, ModTime: &modTime,
	}}
	malformed.Events = []model.EventStream{{
		Unit: "proof.service", Path: "/tmp/proof.jsonl", Malformed: 1,
		Declared: []model.EventSummary{{Type: "broken", Count: 0}},
	}}
	if err := s.RecordObservation("cnode", malformed, malformedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("malformed zero-hit event 量測關掉了 span：open=%d err=%v", open, err)
	}

	recoveredAt := malformedAt.Add(time.Minute)
	modTime = recoveredAt
	good := healthyBatch(recoveredAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &good)
	good.Artifacts = []model.ArtifactCheck{{
		Unit: "proof.service", Artifact: "/tmp/proof.jsonl", Exists: true, ModTime: &modTime,
	}}
	good.Events = []model.EventStream{{
		Unit: "proof.service", Path: "/tmp/proof.jsonl",
		Declared: []model.EventSummary{{Type: "broken", Count: 0}},
	}}
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: recoveredAt, AgentStartedAt: recoveredAt}, recoveredAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", good, recoveredAt); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 0 || last != fmtTime(recoveredAt) {
		t.Fatalf("healthy batch 沒關閉 failure span：last=%q open=%d err=%v", last, open, err)
	}
	finished := unknownArtifactAt
	failures, err := s.CanarySilentFailuresSince([]string{"cnode"}, finished, recoveredAt.Add(time.Hour))
	if err != nil || len(failures) != 1 || !strings.Contains(failures[0].Reason, "broken ×1") {
		t.Fatalf("跨 canary finished 的 failure interval 消失：failures=%+v err=%v", failures, err)
	}
	later, err := s.CanarySilentFailuresSince([]string{"cnode"}, recoveredAt.Add(time.Second), recoveredAt.Add(time.Hour))
	if err != nil || len(later) != 0 {
		t.Fatalf("recovery 後才完成的新 canary 被舊 interval 鎖住：failures=%+v err=%v", later, err)
	}
}

func TestObservationWithoutCLIToolDoesNotCloseOrExtendProcessFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	failedAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: failedAt, AgentStartedAt: failedAt}, failedAt); err != nil {
		t.Fatal(err)
	}
	bad := healthyBatch(failedAt)
	bad.CLITools = []model.CLITool{{Name: "openclaw", Present: true, RunningReason: "掃過 process，沒有找到 OpenClaw"}}
	if err := s.RecordObservation("cnode", bad, failedAt); err != nil {
		t.Fatal(err)
	}
	unknownAt := failedAt.Add(time.Minute)
	unknown := healthyBatch(unknownAt)
	unknown.CLITools = nil
	if err := s.RecordObservation("cnode", unknown, unknownAt); err != nil {
		t.Fatal(err)
	}
	var last string
	var open int
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 1 || last != fmtTime(failedAt) {
		t.Fatalf("缺 CLITool 的 unknown batch 改動了 process failure：last=%q open=%d err=%v", last, open, err)
	}
}

func TestIdentityConflictReconcileCannotCloseOpenWorkloadFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	first := healthyBatch(base)
	first.Identity.MachineIDHint = "identity-a"
	if err := s.RecordObservation("cnode", first, base); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	failureAt := base.Add(30 * time.Second)
	if _, err := s.RecordCanarySilentFailure("cnode", "既有 workload failure", failureAt); err != nil {
		t.Fatal(err)
	}
	conflictAt := base.Add(time.Minute)
	conflict := healthyBatch(conflictAt)
	conflict.Identity.MachineIDHint = "identity-b"
	if err := s.RecordObservation("cnode", conflict, conflictAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: conflictAt, AgentStartedAt: base}, conflictAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileFleet(conflictAt); err != nil {
		t.Fatal(err)
	}
	var last string
	var open int
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 1 || last != fmtTime(failureAt) {
		t.Fatalf("IdentityConflict early return 被當成 recovery：last=%q open=%d err=%v", last, open, err)
	}
}

func TestFreshCheckinWithStaleHealthyObservationCannotCloseFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordObservation("cnode", healthyBatch(base), base); err != nil {
		t.Fatal(err)
	}
	failureAt := base.Add(time.Minute)
	if _, err := s.RecordCanarySilentFailure("cnode", "既有 workload failure", failureAt); err != nil {
		t.Fatal(err)
	}
	now := base.Add(state.ObservationStale + time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: now, AgentStartedAt: base}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileFleet(now); err != nil {
		t.Fatal(err)
	}
	var open int
	if err := s.DB().QueryRow(`SELECT open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("fresh heartbeat 讓 stale observation 冒充 recovery：open=%d err=%v", open, err)
	}
}

func TestFutureMeasuredHealthyRowsAndNewerReceivedUnknownCannotCloseFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	futureHealthy := healthyBatch(base.Add(24 * time.Hour))
	if err := s.RecordObservation("cnode", futureHealthy, base); err != nil {
		t.Fatal(err)
	}
	failureAt := base.Add(time.Minute)
	if _, err := s.RecordCanarySilentFailure("cnode", "manual/unreachable failure", failureAt); err != nil {
		t.Fatal(err)
	}
	unknownAt := base.Add(2 * time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: unknownAt, AgentStartedAt: base}, unknownAt); err != nil {
		t.Fatal(err)
	}
	unknown := healthyBatch(base)
	unknown.CLITools = nil
	if err := s.RecordObservation("cnode", unknown, unknownAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileFleet(unknownAt); err != nil {
		t.Fatal(err)
	}
	var last string
	var open int
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 1 || last != fmtTime(failureAt) {
		t.Fatalf("measured/received 拼接把 unknown 當 recovery：last=%q open=%d err=%v", last, open, err)
	}
}

func TestPreexistingHealthyBatchAndHeartbeatCannotCloseButLaterCoherentHealthyBatchCan(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", healthyBatch(base), base); err != nil {
		t.Fatal(err)
	}
	failureAt := base.Add(time.Minute)
	if _, err := s.RecordCanarySilentFailure("cnode", "manual/unreachable failure", failureAt); err != nil {
		t.Fatal(err)
	}
	heartbeatAt := base.Add(2 * time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: heartbeatAt, AgentStartedAt: base}, heartbeatAt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReconcileFleet(heartbeatAt); err != nil {
		t.Fatal(err)
	}
	var open int
	if err := s.DB().QueryRow(`SELECT open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("preexisting healthy rows/heartbeat 關掉新 failure：open=%d err=%v", open, err)
	}
	recoveryAt := base.Add(3 * time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: recoveryAt, AgentStartedAt: base}, recoveryAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", healthyBatch(recoveryAt), recoveryAt); err != nil {
		t.Fatal(err)
	}
	var last string
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 0 || last != fmtTime(recoveryAt) {
		t.Fatalf("later coherent healthy batch 沒關 failure：last=%q open=%d err=%v", last, open, err)
	}
}

func TestCoherentHealthyObservationCannotCloseWhileMachineIsUnreachable(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	failureAt := base.Add(state.CheckinInterval + state.UnreachableGrace + time.Minute)
	if _, err := s.RecordCanarySilentFailure("cnode", "unreachable failure", failureAt); err != nil {
		t.Fatal(err)
	}
	staleRecoveryAt := failureAt.Add(time.Minute)
	if err := s.RecordObservation("cnode", healthyBatch(staleRecoveryAt), staleRecoveryAt); err != nil {
		t.Fatal(err)
	}
	var last string
	var open int
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 1 || last != fmtTime(failureAt) {
		t.Fatalf("unreachable 機器的 healthy workload 洗掉 failure：last=%q open=%d err=%v", last, open, err)
	}

	recoveryAt := staleRecoveryAt.Add(time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: recoveryAt, AgentStartedAt: base}, recoveryAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", healthyBatch(recoveryAt), recoveryAt); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 0 || last != fmtTime(recoveryAt) {
		t.Fatalf("新鮮心跳後的 coherent recovery 沒關 span：last=%q open=%d err=%v", last, open, err)
	}
}

func TestNewDatabaseDoesNotInventOpenFailure(t *testing.T) {
	s := rolloutStore(t)
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM canary_silent_failures WHERE open=1`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("新 DB 憑空有 open failure：rows=%d err=%v", rows, err)
	}
}

func TestOlderRecoveryCannotCloseANewerOpenFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	failureAt := time.Date(2026, 9, 6, 12, 1, 0, 0, time.UTC)
	if _, err := s.RecordCanarySilentFailure("cnode", "newer failure", failureAt); err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := closeCanarySilentFailureTx(tx, "cnode", failureAt.Add(-time.Minute)); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := closeCanarySilentFailureTx(tx, "cnode", failureAt); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var open int
	if err := s.DB().QueryRow(`SELECT open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("不晚於 failure 的 recovery 關掉了 span：open=%d err=%v", open, err)
	}
}
