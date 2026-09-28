package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/state"
)

func createPromoteDeployment(t *testing.T, s *Store, channel, version, digest, retryOf string, targets []NewDeploymentTarget) (Deployment, map[string]Job) {
	t.Helper()
	for _, target := range targets {
		if _, err := s.DB().Exec(`UPDATE machine_registry SET channel=? WHERE machine_id=?`, channel, target.MachineID); err != nil {
			t.Fatal(err)
		}
	}
	n := NewDeployment{
		Channel: channel, ResourceKind: "openclaw", ResourceID: "openclaw", BatchSize: len(targets), CreatedBy: "promote-test", RetryOf: retryOf,
		Spec:    `{"kind":"openclaw","version":"` + version + `","artifact":{"sha256":"` + digest + `"}}`,
		Targets: targets, Job: NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	}
	var d Deployment
	var jobs []Job
	var err error
	if channel == "stable" {
		// 這個 fixture 只用來製造「歷史上已經寫入」的 stable 帳本，好測
		// PromoteFacts 會不會看到後來的 digest。正式 caller 仍然無法繞過 gate。
		tx, beginErr := s.db.Begin()
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		d, jobs, err = createDeploymentTx(tx, n, s.now().UTC())
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	} else {
		d, jobs, err = s.CreateDeployment(n)
	}
	if err != nil {
		t.Fatal(err)
	}
	byMachine := make(map[string]Job, len(jobs))
	for _, job := range jobs {
		byMachine[job.MachineID] = job
	}
	return d, byMachine
}

func setPromoteJobState(t *testing.T, s *Store, job Job, status deploy.JobState, at time.Time) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`, status, fmtTime(at), job.JobID); err != nil {
		t.Fatal(err)
	}
	if status == deploy.Succeeded {
		seedPromoteIndependentPass(t, s, job.JobID, at.Add(2*time.Second))
	}
}

// seedPromoteIndependentPass is the common historical-ledger fixture for gate
// tests that are about another promotion invariant. Tests dedicated to the
// independent gate remove or mutate these rows explicitly.
func seedPromoteIndependentPass(t *testing.T, s *Store, jobID string, receivedAt time.Time) string {
	t.Helper()
	var desired DesiredState
	if err := s.DB().QueryRow(`SELECT d.resource_kind,d.resource_id,d.spec
  FROM jobs AS j JOIN desired_state AS d ON d.desired_id=j.desired_id
 WHERE j.job_id=?`, jobID).Scan(&desired.ResourceKind, &desired.ResourceID, &desired.Spec); err != nil {
		t.Fatal(err)
	}
	expectedVersion := jobIndependentExpectedVersion(desired)
	verifierID := newID()
	if _, err := s.DB().Exec(`INSERT INTO verifiers
 (verifier_id,kind,display_name,failure_domain,credential_hash,created_at,revision)
 VALUES (?,?,?,?,?,?,1)`, verifierID, VerifierKindFleetPeerAgent, "promote-peer-"+verifierID,
		"promote-peer-domain-"+verifierID, hashToken("promote-secret-"+verifierID),
		fmtTime(receivedAt.Add(-2*time.Second))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO verification_assignments
 (assignment_id,job_id,verifier_id,assigned_at,assigned_by) VALUES (?,?,?,?,?)`,
		newID(), jobID, verifierID, fmtTime(receivedAt.Add(-time.Second)), "promote-test"); err != nil {
		t.Fatal(err)
	}
	priorNowFn := s.nowFn
	s.nowFn = func() time.Time { return receivedAt }
	defer func() { s.nowFn = priorNowFn }()
	for _, ruleID := range []string{
		model.IndependentRuleOpenClawCurrentRelease,
		model.IndependentRuleOpenClawGatewayHTTP,
		model.IndependentRuleOpenClawUnitState,
	} {
		observedVersion := ""
		if ruleID == model.IndependentRuleOpenClawCurrentRelease {
			observedVersion = expectedVersion
		}
		if err := s.RecordIndependentVerification(IndependentVerificationRequest{
			VerifierID: verifierID, JobID: jobID, RuleID: ruleID,
			Command: "promote-test " + ruleID, ObservedVersion: observedVersion,
			Passed: true, VerifiedAt: receivedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return verifierID
}

func finishPromoteDeployment(t *testing.T, s *Store, id, from string, at time.Time) {
	t.Helper()
	// Production finish paths deliberately ignore caller wall time and read the
	// Store clock under their writer transaction.  Pin that trusted clock for
	// deterministic historical fixtures, then restore the fixture's current time.
	priorNowFn := s.nowFn
	s.nowFn = func() time.Time { return at }
	defer func() { s.nowFn = priorNowFn }()
	changed, err := s.SetDeploymentState(id, from, DeploymentFinished, at)
	if errors.Is(err, ErrDeploymentFinishNotReady) && from == DeploymentRunning {
		// Failed canary attempts are legitimately closed through the public
		// failure-stop path: pause, then Continue with no remaining batch. Direct
		// running→finished is reserved for complete all-success deployments.
		if changed, err = s.SetDeploymentState(id, DeploymentRunning, DeploymentPaused, at); err != nil || !changed {
			t.Fatalf("pause failed promote fixture %s changed=%v err=%v", id, changed, err)
		}
		var d Deployment
		var jobs []Job
		d, jobs, err = s.ContinueDeployment(id, at)
		changed = err == nil && d.State == DeploymentFinished && len(jobs) == 0
	}
	if err != nil || !changed {
		t.Fatalf("finish %s changed=%v err=%v", id, changed, err)
	}
}

func seedPromoteSoakBoundary(t *testing.T, s *Store, deploymentID string) {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := recordDeploymentSoakBoundaryTx(tx, deploymentID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func observePromoteMachine(t *testing.T, s *Store, id, version string, at time.Time) {
	t.Helper()
	lastTask := at.Add(-time.Minute)
	matched := true
	if err := s.RecordCheckin(id, model.Checkin{SentAt: at, AgentStartedAt: at}, at); err != nil {
		t.Fatal(err)
	}
	b := model.ObservationBatch{
		MeasuredAt: at,
		OpenClaw: model.OpenClaw{Present: true,
			Install: &model.OpenClawInstall{UnitFound: true, MainPID: 42, ProcessMatchesUnit: &matched,
				RunningDirVersion: version, NodeVersion: "24.15.0"},
			DB: &model.OpenClawDB{Present: true, TaskRunRows: 1, LastTaskEndedAt: &lastTask}},
		CLITools: []model.CLITool{{Name: "openclaw", Present: true, RunningPID: 42}},
	}
	stampCurrentWorkloadPolicy(t, s, s.DisplayName(id), &b)
	if err := s.RecordObservation(id, b, at); err != nil {
		t.Fatal(err)
	}
}

func createSucceededPromoteWitness(t *testing.T, s *Store, machineID, spec, digest string, at time.Time) {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	desiredID, revision, err := createDesiredStateTx(tx, "machine", machineID, "openclaw", "openclaw", spec, "promote witness test", s.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,artifact_digest,irreversible,execution_timeout,terminal_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, newID(), machineID, desiredID, revision, deploy.Succeeded, fmtTime(s.now()),
		nullIfEmpty(digest), false, 900, fmtTime(at)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCanarySilentFailureExtendsAnIntervalAndStartsANewOneAfterAGap(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for i, tc := range []struct {
		at       time.Time
		recorded bool
	}{{start, true}, {start.Add(59 * time.Minute), false}, {start.Add(2 * time.Hour), true}} {
		got, err := s.RecordCanarySilentFailure("cnode", "8 小時無合格完成事件", tc.at)
		if err != nil || got != tc.recorded {
			t.Fatalf("case %d recorded=%v err=%v", i, got, err)
		}
	}
	got, err := s.CanarySilentFailuresSince([]string{"cnode"}, start.Add(-time.Second), start.Add(2*time.Hour))
	if err != nil || len(got) != 2 || got[0].DisplayName != "samplehub1" ||
		!got[0].FirstSeenAt.Equal(start) || !got[0].LastSeenAt.Equal(start.Add(59*time.Minute)) ||
		!got[1].FirstSeenAt.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("failures=%+v err=%v", got, err)
	}
}

func TestCanarySilentFailureIntervalsOverlapFinishedAtInclusively(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	addRolloutMachine(t, s, "onode", "sampleagent2", true)
	finished := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	if recorded, err := s.RecordCanarySilentFailure("cnode", "持續失聯", finished.Add(-30*time.Minute)); err != nil || !recorded {
		t.Fatalf("first recorded=%v err=%v", recorded, err)
	}
	// 同一個 failure 在 deployment 完成後仍存在；更新 last_seen_at，不能被「每小時一列」吞掉。
	if recorded, err := s.RecordCanarySilentFailure("cnode", "持續失聯", finished.Add(20*time.Minute)); err != nil || recorded {
		t.Fatalf("extend recorded=%v err=%v", recorded, err)
	}
	if recorded, err := s.RecordCanarySilentFailure("onode", "剛好同秒", finished); err != nil || !recorded {
		t.Fatalf("boundary recorded=%v err=%v", recorded, err)
	}
	got, err := s.CanarySilentFailuresSince([]string{"cnode", "onode"}, finished, finished.Add(time.Hour))
	if err != nil || len(got) != 2 {
		t.Fatalf("inclusive overlap failures=%+v err=%v", got, err)
	}
	if !got[0].LastSeenAt.Equal(finished.Add(20*time.Minute)) || !got[1].FirstSeenAt.Equal(finished) {
		t.Fatalf("區間端點沒保留：%+v", got)
	}
}

func TestPromoteGateIncludesFutureDatedOpenFailureButNotClosedInterval(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	if baseline := promoteDecision(t, s, n, now); !baseline.Allowed {
		t.Fatalf("test setup: baseline locked: %s", baseline.Summary())
	}

	// A Hub clock jump can append a failure with a wall timestamp later than the
	// corrected clock. Append order says this unresolved fact already exists, so
	// first_seen_at > now must not create a temporary all-green promotion window.
	future := now.Add(time.Hour)
	if recorded, err := s.RecordCanarySilentFailure("cnode", "clock jumped forward", future); err != nil || !recorded {
		t.Fatalf("record future failure=%v err=%v", recorded, err)
	}
	decision := promoteDecision(t, s, n, now)
	if decision.Allowed || !strings.Contains(decision.Summary(), "clock jumped forward") {
		t.Fatalf("future-dated open failure did not fail closed: %s", decision.Summary())
	}

	// Once explicit newer recovery closes it, the resulting entirely-future
	// closed interval does not overlap [canary finished, now] and must stay out.
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	recovered := future.Add(time.Minute)
	if err := closeCanarySilentFailureWithEvidenceTx(tx, "cnode", recovered, recovered); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if after := promoteDecision(t, s, n, now); !after.Allowed {
		t.Fatalf("future closed interval incorrectly overlapped current window: %s", after.Summary())
	}
}

func TestPruneCanarySilentFailuresUsesObservationRetentionAndLogsIt(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{now.Add(-40 * 24 * time.Hour), now.Add(-39 * 24 * time.Hour), now.Add(-time.Hour)} {
		if recorded, err := s.RecordCanarySilentFailure("cnode", "沉默", at); err != nil || !recorded {
			t.Fatalf("record %v: recorded=%v err=%v", at, recorded, err)
		}
	}
	rep, err := s.Prune(now, DefaultRetention(), false)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, count := range rep.Counts {
		if count.Table == "canary_silent_failures" {
			found = true
			if count.Deleted != 2 {
				t.Fatalf("silent failures deleted=%d，預期 2", count.Deleted)
			}
		}
	}
	if !found {
		t.Fatal("prune report 沒有 canary_silent_failures")
	}
	var rows, logged int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM canary_silent_failures`).Scan(&rows)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM retention_log WHERE table_name='canary_silent_failures'`).Scan(&logged)
	if rows != 1 || logged != 1 {
		t.Fatalf("rows=%d retention_log=%d", rows, logged)
	}
}

func TestPromoteFactsUsesFinishedCanarySnapshotNotCurrentChannel(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMachineChannel("cnode", "canary"); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", BatchSize: 1, CreatedBy: "test",
		Spec:    `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
		Job:     NewJob{ArtifactDigest: "sha256:" + digest},
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := now.Add(-3 * 24 * time.Hour)
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?, terminal_at=? WHERE job_id=?`, deploy.Succeeded, fmtTime(finished), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	finishPromoteDeployment(t, s, d.DeploymentID, DeploymentRunning, finished)
	// ⚠ promote 問的是當時 canary 的快照；之後換到 stable 不能讓證人消失。
	if err := s.SetMachineChannel("cnode", "stable"); err != nil {
		t.Fatal(err)
	}
	if recorded, err := s.RecordCanarySilentFailure("cnode", "第 10 小時沉默", finished.Add(10*time.Hour)); err != nil || !recorded {
		t.Fatalf("recorded=%v err=%v", recorded, err)
	}
	facts, err := s.PromoteFacts("2026.9.2", "sha256:"+digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || facts.Canary.DeploymentID != d.DeploymentID || facts.Canary.Stuck != 0 || len(facts.Canary.Targets) != 1 {
		t.Fatalf("canary=%+v", facts.Canary)
	}
	target := facts.Canary.Targets[0]
	if target.MachineID != "cnode" || !target.Judged || target.Retired || target.Unreachable {
		t.Fatalf("snapshot target=%+v", target)
	}
	if len(facts.Silent) != 1 || facts.Silent[0].Reason != "第 10 小時沉默" {
		t.Fatalf("silent=%+v", facts.Silent)
	}
}

func TestPromoteFactsReportsTargetWithoutWorkloadObservationHonestly(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("a", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, finished)

	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || len(facts.Canary.Targets) != 1 || facts.Canary.Targets[0].WorkloadObserved {
		t.Fatalf("沒有 observation 的 target 竟有 workload witness：%+v", facts.Canary)
	}
	decision := rollout.PromoteGate(facts, now, time.UTC)
	summary := decision.Summary()
	if strings.Contains(summary, "現在回報 OpenClaw 不存在") {
		t.Fatalf("沒有 workload witness 卻宣稱機器回報不存在：%s", summary)
	}
	if !strings.Contains(summary, "還沒有同一批證據完整的 workload observation") {
		t.Fatalf("沒有說出 workload witness 缺口：%s", summary)
	}
	if decision.Allowed {
		t.Fatalf("缺少 workload witness 竟可 promote：%+v", decision)
	}
}

func TestPromoteFactsDoesNotTreatAnotherDigestAsCanary(t *testing.T) {
	s := rolloutStore(t)
	facts, err := s.PromoteFacts("2026.9.2", strings.Repeat("f", 64), time.Now())
	if err != nil || facts.Canary != nil {
		t.Fatalf("facts=%+v err=%v", facts, err)
	}
}

func TestPromoteFactsAggregatesRetryAncestryAndOnlySuccessClearsFailure(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("b", 64)
	for _, id := range []string{"already-good", "needs-retry"} {
		addRolloutMachine(t, s, id, id, false)
	}
	root, rootJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "", []NewDeploymentTarget{
		{MachineID: "already-good", BatchNo: 1}, {MachineID: "needs-retry", BatchNo: 1},
	})
	setPromoteJobState(t, s, rootJobs["already-good"], deploy.Succeeded, finished)
	setPromoteJobState(t, s, rootJobs["needs-retry"], deploy.Failed, finished)
	finishPromoteDeployment(t, s, root.DeploymentID, DeploymentRunning, finished)

	failedRetry, failedJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, root.DeploymentID,
		[]NewDeploymentTarget{{MachineID: "needs-retry", BatchNo: 1}})
	setPromoteJobState(t, s, failedJobs["needs-retry"], deploy.Failed, finished.Add(time.Minute))
	finishPromoteDeployment(t, s, failedRetry.DeploymentID, DeploymentRunning, finished.Add(time.Minute))
	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || len(facts.Canary.Targets) != 2 || facts.Canary.Stuck != 1 {
		t.Fatalf("失敗 retry 洗掉舊 target/stuck：%+v", facts.Canary)
	}
	if d := rollout.PromoteGate(facts, now, time.UTC); d.Allowed {
		t.Fatalf("失敗 retry 竟解鎖：%+v", d)
	}

	successRetry, successJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, failedRetry.DeploymentID,
		[]NewDeploymentTarget{{MachineID: "needs-retry", BatchNo: 1}})
	setPromoteJobState(t, s, successJobs["needs-retry"], deploy.Succeeded, finished.Add(2*time.Minute))
	finishPromoteDeployment(t, s, successRetry.DeploymentID, DeploymentRunning, finished.Add(2*time.Minute))
	for _, id := range []string{"already-good", "needs-retry"} {
		establishPromoteCoverage(t, s, id, id, "2026.9.2", finished.Add(2*time.Minute), now)
	}
	facts, err = s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || facts.Canary.DeploymentID != successRetry.DeploymentID || len(facts.Canary.Targets) != 2 || facts.Canary.Stuck != 0 {
		t.Fatalf("成功 retry 沒聚合成兩台全綠：%+v", facts.Canary)
	}
	for _, target := range facts.Canary.Targets {
		if !target.Succeeded || !target.Judged || !target.LastObservation.Equal(now) ||
			target.AppliedDigest != digest || target.RunningVersion != "2026.9.2" {
			t.Errorf("target 證據不完整：%+v", target)
		}
	}
	if d := rollout.PromoteGate(facts, now, time.UTC); !d.Allowed {
		t.Fatalf("成功 retry 後仍鎖著：%s", d.Summary())
	}
	if recorded, err := s.RecordCanarySilentFailure("already-good", "retry 後第 10 小時沉默", finished.Add(10*time.Hour)); err != nil || !recorded {
		t.Fatalf("record prior-success failure=%v err=%v", recorded, err)
	}
	facts, err = s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Silent) != 1 || facts.Silent[0].DisplayName != "already-good" {
		t.Fatalf("retry 丟掉前一輪成功 target 的監看：%+v", facts.Silent)
	}
	if d := rollout.PromoteGate(facts, now, time.UTC); d.Allowed || !strings.Contains(d.Summary(), "retry 後第 10 小時沉默") {
		t.Fatalf("前一輪成功 target 的 silent failure 沒鎖住 lineage：%s", d.Summary())
	}
}

func TestPromoteFactsStartsBusinessDayWindowAfterEveryEffectiveSuccess(t *testing.T) {
	loc := time.UTC
	monday := time.Date(2026, 9, 7, 10, 0, 0, 0, loc)
	tuesday := time.Date(2026, 9, 8, 15, 0, 0, 0, loc)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, loc)
	s := rolloutStore(t)
	// rolloutStore publishes the evidence epoch from the real wall clock when
	// it opens the DB. This fixture intentionally replays a fixed historical
	// Monday/Tuesday calendar, so its epoch must be fixed too; otherwise the
	// production gate correctly starts rejecting the canary once wall time
	// passes 2026-09-07 10:00Z. Keep it strictly before every fixture success.
	if _, err := s.DB().Exec(`UPDATE canary_evidence_epoch SET started_at=? WHERE singleton=1`,
		fmtTime(monday.Add(-24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("7", 64)
	for _, id := range []string{"failed-first", "late-success"} {
		addRolloutMachine(t, s, id, id, false)
		observePromoteMachine(t, s, id, "2026.9.2", now)
	}
	root, rootJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "", []NewDeploymentTarget{
		{MachineID: "failed-first", BatchNo: 1}, {MachineID: "late-success", BatchNo: 1},
	})
	setPromoteJobState(t, s, rootJobs["failed-first"], deploy.Failed, monday.Add(-time.Hour))
	setPromoteJobState(t, s, rootJobs["late-success"], deploy.Failed, monday.Add(-time.Hour))
	// 新 store 不允許有 active job 的 paused parent 開 retry；先走合法 create，再還原成舊版可能留下的帳本。
	finishPromoteDeployment(t, s, root.DeploymentID, DeploymentRunning, monday.Add(-time.Hour))
	retry, retryJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, root.DeploymentID,
		[]NewDeploymentTarget{{MachineID: "failed-first", BatchNo: 1}})
	// This test deliberately recreates a pre-owner-index ledger where an older
	// paused attempt and its retry were simultaneously active. New writes cannot
	// produce it; PromoteFacts must still fail/aggregate deterministically if such
	// a legacy DB is inspected before operators resolve the migration blocker.
	if _, err := s.DB().Exec(`DROP INDEX IF EXISTS ` + activeDeploymentResourceIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE deployments SET state=?,finished_at=NULL WHERE deployment_id=?`, DeploymentPaused, root.DeploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=NULL WHERE job_id=?`, deploy.Running, rootJobs["late-success"].JobID); err != nil {
		t.Fatal(err)
	}
	setPromoteJobState(t, s, retryJobs["failed-first"], deploy.Succeeded, monday)
	// The legacy duplicate-owner fixture above cannot use the public finish CAS:
	// it intentionally violates the sole-active-owner invariant that CAS checks.
	if _, err := s.DB().Exec(`UPDATE deployments SET state=?,finished_at=? WHERE deployment_id=?`,
		DeploymentFinished, fmtTime(monday), retry.DeploymentID); err != nil {
		t.Fatal(err)
	}
	seedPromoteSoakBoundary(t, s, retry.DeploymentID)
	setPromoteJobState(t, s, rootJobs["late-success"], deploy.Succeeded, tuesday)

	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || facts.Canary.Stuck != 0 || !facts.Canary.FinishedAt.Equal(tuesday) {
		t.Fatalf("cohort finish 沒取所有 effective success 的最晚時間：%+v", facts.Canary)
	}
	decision := rollout.PromoteGate(facts, now, loc)
	wantEarliest := time.Date(2026, 9, 10, 0, 0, 0, 0, loc)
	if decision.Allowed || !decision.EarliestAt.Equal(wantEarliest) {
		t.Fatalf("late ancestor success 偷吃工作天：decision=%+v want earliest=%v", decision, wantEarliest)
	}
}

func TestPromoteFactsFailsClosedOnLegacyRunningAncestor(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("8", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	root, rootJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, rootJobs["cnode"], deploy.Failed, now.Add(-48*time.Hour))
	finishPromoteDeployment(t, s, root.DeploymentID, DeploymentRunning, now.Add(-48*time.Hour))
	retry, retryJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, root.DeploymentID,
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, retryJobs["cnode"], deploy.Succeeded, now.Add(-47*time.Hour))
	finishPromoteDeployment(t, s, retry.DeploymentID, DeploymentRunning, now.Add(-47*time.Hour))
	if _, err := s.DB().Exec(`UPDATE deployments SET state=?,finished_at=NULL WHERE deployment_id=?`, DeploymentRunning, root.DeploymentID); err != nil {
		t.Fatal(err)
	}
	// 即使 ancestor job 後來成功，running ledger 仍可再變，不能拿來 promote。
	setPromoteJobState(t, s, rootJobs["cnode"], deploy.Succeeded, now.Add(-24*time.Hour))

	if facts, err := s.PromoteFacts("2026.9.2", digest, now); !errors.Is(err, ErrDeploymentRetryParentRunning) {
		t.Fatalf("legacy running ancestor 沒 fail closed：facts=%+v err=%v", facts, err)
	}
}

func TestPromoteFactsFailsClosedOnLegacyAncestorWithActiveJob(t *testing.T) {
	for _, ancestorState := range []string{DeploymentPaused, DeploymentFinished} {
		t.Run(ancestorState, func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			digest := strings.Repeat("a", 64)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			observePromoteMachine(t, s, "cnode", "2026.9.2", now)
			root, rootJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
				[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
			setPromoteJobState(t, s, rootJobs["cnode"], deploy.Failed, now.Add(-48*time.Hour))
			finishPromoteDeployment(t, s, root.DeploymentID, DeploymentRunning, now.Add(-48*time.Hour))
			retry, retryJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, root.DeploymentID,
				[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
			setPromoteJobState(t, s, retryJobs["cnode"], deploy.Succeeded, now.Add(-47*time.Hour))
			finishPromoteDeployment(t, s, retry.DeploymentID, DeploymentRunning, now.Add(-47*time.Hour))

			finishedAt := any(nil)
			if ancestorState == DeploymentFinished {
				finishedAt = fmtTime(now.Add(-48 * time.Hour))
			}
			if _, err := s.DB().Exec(`UPDATE deployments SET state=?,finished_at=? WHERE deployment_id=?`,
				ancestorState, finishedAt, root.DeploymentID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=NULL WHERE job_id=?`,
				deploy.Running, rootJobs["cnode"].JobID); err != nil {
				t.Fatal(err)
			}

			if facts, err := s.PromoteFacts("2026.9.2", digest, now); !errors.Is(err, ErrDeploymentRetryParentActive) {
				t.Fatalf("legacy %s ancestor 的 active job 被 retry success 遮掉：facts=%+v err=%v", ancestorState, facts, err)
			}
		})
	}
}

func TestStablePromotionRejectsActiveAncestorJobOrphanedByCompatibleReplacement(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("a", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	root, rootJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, rootJobs["cnode"], deploy.Failed, now.Add(-48*time.Hour))
	finishPromoteDeployment(t, s, root.DeploymentID, DeploymentRunning, now.Add(-48*time.Hour))
	retry, retryJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, root.DeploymentID,
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, retryJobs["cnode"], deploy.Succeeded, now.Add(-47*time.Hour))
	finishPromoteDeployment(t, s, retry.DeploymentID, DeploymentRunning, now.Add(-47*time.Hour))

	replacementID := newID()
	if _, err := s.DB().Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,artifact_digest,irreversible,execution_timeout,terminal_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, replacementID, "cnode", root.DesiredID, root.Revision, deploy.Failed,
		fmtTime(now.Add(-48*time.Hour)), "sha256:"+digest, false, 600, fmtTime(now.Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE deployment_targets SET job_id=? WHERE deployment_id=? AND machine_id=?`,
		replacementID, root.DeploymentID, "cnode"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=NULL WHERE job_id=?`,
		deploy.Running, rootJobs["cnode"].JobID); err != nil {
		t.Fatal(err)
	}

	if decision, err := s.PreviewStableOpenClawPromotion("2026.9.2", digest, now); !errors.Is(err, ErrDeploymentJobGraphMismatch) {
		t.Fatalf("orphaned active ancestor became promotion evidence: decision=%+v err=%v", decision, err)
	}
}

func TestStablePromotionRejectsStoredCanaryBatchAboveBlastRadius(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("8", 64)
	machines := []string{"canary-1", "canary-2", "canary-3", "canary-4", "canary-5", "canary-6"}
	targets := make([]NewDeploymentTarget, 0, len(machines))
	for i, machineID := range machines {
		addRolloutMachine(t, s, machineID, machineID, false)
		observePromoteMachine(t, s, machineID, "2026.9.2", now)
		batch := 1
		if i == len(machines)-1 {
			batch = 2
		}
		targets = append(targets, NewDeploymentTarget{MachineID: machineID, BatchNo: batch})
	}
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", BatchSize: MaxDeploymentBatchSize,
		CreatedBy: "promotion-plan-test", Targets: targets,
		Spec: `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		Job:  NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	})
	if err != nil || len(jobs) != MaxDeploymentBatchSize {
		t.Fatalf("create canary jobs=%d err=%v", len(jobs), err)
	}
	for _, job := range jobs {
		setPromoteJobState(t, s, job, deploy.Succeeded, now.Add(-48*time.Hour))
	}
	last, err := s.OpenDeploymentBatch(d.DeploymentID, 2, now.Add(-48*time.Hour))
	if err != nil || len(last) != 1 {
		t.Fatalf("open final batch jobs=%+v err=%v", last, err)
	}
	setPromoteJobState(t, s, last[0], deploy.Succeeded, now.Add(-47*time.Hour))
	finishPromoteDeployment(t, s, d.DeploymentID, DeploymentRunning, now.Add(-47*time.Hour))

	if _, err := s.DB().Exec(`UPDATE deployment_targets SET batch_no=1
 WHERE deployment_id=? AND machine_id=?`, d.DeploymentID, machines[len(machines)-1]); err != nil {
		t.Fatal(err)
	}
	if facts, err := s.PromoteFacts("2026.9.2", digest, now); !errors.Is(err, ErrDeploymentInvalidBatchPlan) {
		t.Fatalf("oversized stored canary batch became promotion evidence: facts=%+v err=%v", facts, err)
	}
}

func TestPromoteFactsRejectsSucceededTargetWithoutTerminalTime(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("9", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	d, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, now.Add(-48*time.Hour))
	finishPromoteDeployment(t, s, d.DeploymentID, DeploymentRunning, now.Add(-48*time.Hour))
	if _, err := s.DB().Exec(`UPDATE jobs SET terminal_at=NULL WHERE job_id=?`, jobs["cnode"].JobID); err != nil {
		t.Fatal(err)
	}

	if facts, err := s.PromoteFacts("2026.9.2", digest, now); !errors.Is(err, ErrDeploymentJobGraphMismatch) {
		t.Fatalf("succeeded/terminal_at NULL was accepted: facts=%+v err=%v", facts, err)
	}
}

func TestPromoteFactsRejectsIdentityConflictAsCurrentWitness(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("6", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now.Add(-3*time.Minute))
	d, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, now.Add(-48*time.Hour))
	finishPromoteDeployment(t, s, d.DeploymentID, DeploymentRunning, now.Add(-48*time.Hour))
	establishPromoteCoverage(t, s, "cnode", "samplehub1", "2026.9.2", now.Add(-48*time.Hour), now.Add(-3*time.Minute))
	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil || !rollout.PromoteGate(facts, now, time.UTC).Allowed {
		t.Fatalf("注入 identity conflict 前 fixture 不綠：facts=%+v err=%v", facts, err)
	}
	for i, hint := range []string{"machine-id-a", "machine-id-b"} {
		at := now.Add(time.Duration(i-2) * time.Minute)
		batch := model.ObservationBatch{
			MeasuredAt: at,
			OpenClaw: model.OpenClaw{Install: &model.OpenClawInstall{
				RunningDirVersion: "2026.9.2", NodeVersion: "24.15.0",
			}},
		}
		batch.Identity.MachineIDHint = hint
		if err := s.RecordObservation("cnode", batch, at); err != nil {
			t.Fatal(err)
		}
	}
	facts, err = s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	decision := rollout.PromoteGate(facts, now, time.UTC)
	if decision.Allowed || !strings.Contains(decision.Summary(), "身分衝突") {
		t.Fatalf("IdentityConflict 拼接 shared machine_id 證據解鎖：%+v", decision)
	}
}

func TestPromoteFactsRejectsUntrustedClockAsCurrentWitness(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("b", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, finished)
	establishPromoteCoverage(t, s, "cnode", "samplehub1", "2026.9.2", finished, now)
	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil || !rollout.PromoteGate(facts, now, time.UTC).Allowed {
		t.Fatalf("baseline facts=%+v err=%v", facts, err)
	}
	if err := s.RecordCheckin("cnode", model.Checkin{
		SentAt: now.Add(state.ClockSkewTolerance + time.Second), AgentStartedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}

	facts, err = s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || len(facts.Canary.Targets) != 1 || !facts.Canary.Targets[0].ClockUntrusted {
		t.Fatalf("clock trust 沒投影到 canary witness：%+v", facts.Canary)
	}
	decision := rollout.PromoteGate(facts, now, time.UTC)
	if decision.Allowed || !strings.Contains(decision.Summary(), "時鐘") {
		t.Fatalf("超過 clock skew tolerance 仍解鎖：%s", decision.Summary())
	}
}

func TestPromoteFactsRejectsAbsentOpenClawEvenWithExactInstallMetadata(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("c", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, finished)
	establishPromoteCoverage(t, s, "cnode", "samplehub1", "2026.9.2", finished, now)
	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil || !rollout.PromoteGate(facts, now, time.UTC).Allowed {
		t.Fatalf("baseline facts=%+v err=%v", facts, err)
	}
	// Probe 可以保留 install metadata 卻明確說 Present=false；兩個欄位必須
	// 來自同一筆 selected OpenClaw observation，不能只看版本字串。
	if err := s.RecordObservation("cnode", model.ObservationBatch{
		MeasuredAt: now,
		OpenClaw: model.OpenClaw{Present: false,
			Install: &model.OpenClawInstall{RunningDirVersion: "2026.9.2", NodeVersion: "24.15.0"}},
	}, now); err != nil {
		t.Fatal(err)
	}

	facts, err = s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || len(facts.Canary.Targets) != 1 || facts.Canary.Targets[0].OpenClawPresent {
		t.Fatalf("Present=false 沒投影到 canary witness：%+v", facts.Canary)
	}
	decision := rollout.PromoteGate(facts, now, time.UTC)
	if decision.Allowed || !strings.Contains(decision.Summary(), "OpenClaw 不存在") {
		t.Fatalf("Present=false + exact install metadata 仍解鎖：%s", decision.Summary())
	}
}

func TestPromoteFactsKeepsExcludedAndUnopenedTargetsInSnapshot(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("c", 64)
	for _, id := range []string{"good", "unopened", "excluded"} {
		addRolloutMachine(t, s, id, id, false)
		observePromoteMachine(t, s, id, "2026.9.2", now)
	}
	d, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "", []NewDeploymentTarget{
		{MachineID: "good", BatchNo: 1}, {MachineID: "unopened", BatchNo: 2},
		{MachineID: "excluded", ExcludedReason: "missing_package"},
	})
	setPromoteJobState(t, s, jobs["good"], deploy.Succeeded, finished)
	// Recreate a legacy impossible state to verify PromoteFacts fails closed on
	// an unopened target. The public running→finished transition now rejects it.
	if _, err := s.DB().Exec(`UPDATE deployments SET state=?,finished_at=? WHERE deployment_id=?`,
		DeploymentFinished, fmtTime(finished), d.DeploymentID); err != nil {
		t.Fatal(err)
	}
	seedPromoteSoakBoundary(t, s, d.DeploymentID)
	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || len(facts.Canary.Targets) != 3 {
		t.Fatalf("snapshot=%+v", facts.Canary)
	}
	decision := rollout.PromoteGate(facts, now, time.UTC)
	if decision.Allowed || !strings.Contains(decision.Summary(), "excluded 在 canary 被排除") ||
		!strings.Contains(decision.Summary(), "unopened 沒有跑到 canary 工作單") {
		t.Fatalf("排除／未開單沒擋住：%s", decision.Summary())
	}
}

func TestPromoteFactsOnlyUsesLatestCanaryAttempt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		newVersion string
		newDigest  string
		newState   string
	}{
		{"新版已完成", "2026.9.3", strings.Repeat("e", 64), DeploymentFinished},
		{"同版 retry 暫停", "2026.9.2", strings.Repeat("d", 64), DeploymentPaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			finished := now.Add(-48 * time.Hour)
			oldDigest := strings.Repeat("d", 64)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			observePromoteMachine(t, s, "cnode", "2026.9.2", now)
			old, oldJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", oldDigest, "",
				[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
			setPromoteJobState(t, s, oldJobs["cnode"], deploy.Succeeded, finished)
			finishPromoteDeployment(t, s, old.DeploymentID, DeploymentRunning, finished)

			retryOf := old.DeploymentID
			if tc.newVersion != "2026.9.2" || tc.newDigest != oldDigest {
				retryOf = ""
			}
			newer, newerJobs := createPromoteDeployment(t, s, "canary", tc.newVersion, tc.newDigest, retryOf,
				[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
			if tc.newState == DeploymentFinished {
				setPromoteJobState(t, s, newerJobs["cnode"], deploy.Succeeded, finished.Add(time.Hour))
				finishPromoteDeployment(t, s, newer.DeploymentID, DeploymentRunning, finished.Add(time.Hour))
			} else if changed, err := s.SetDeploymentState(newer.DeploymentID, DeploymentRunning, DeploymentPaused, finished.Add(time.Hour)); err != nil || !changed {
				t.Fatalf("pause newer changed=%v err=%v", changed, err)
			}
			facts, err := s.PromoteFacts("2026.9.2", oldDigest, now)
			if err != nil || facts.Canary != nil || !strings.Contains(facts.CanaryBlock, "最新 canary 部署") {
				t.Fatalf("舊 canary 被較新的 attempt 穿透：facts=%+v err=%v", facts, err)
			}
		})
	}
}

func TestPromoteFactsLatestCanaryUsesAppendChronologyNotCreatedAt(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("d", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	old, oldJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, oldJobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, old.DeploymentID, DeploymentRunning, finished)

	// 這張 attempt 是後 append 的真實邊界，但模擬回撥的 Hub 時鐘讓
	// created_at 比舊成功還早。時間字串不能把它藏在舊成功後面。
	newer, _ := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, old.DeploymentID,
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	if changed, err := s.SetDeploymentState(newer.DeploymentID, DeploymentRunning, DeploymentPaused, finished); err != nil || !changed {
		t.Fatalf("pause newer changed=%v err=%v", changed, err)
	}
	if _, err := s.DB().Exec(`UPDATE deployments SET created_at=? WHERE deployment_id=?`,
		fmtTime(old.CreatedAt.Add(-time.Hour)), newer.DeploymentID); err != nil {
		t.Fatal(err)
	}

	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary != nil || !strings.Contains(facts.CanaryBlock, shortStoreID(newer.DeploymentID, 8)) {
		t.Fatalf("回撥 created_at 讓舊 canary 遮住後 append attempt：%+v", facts)
	}
}

func TestPromoteFactsNewerMislabeledOpenClawCanaryMasksOlderWitness(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("e", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	old, oldJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, oldJobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, old.DeploymentID, DeploymentRunning, finished)
	newer, _ := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	// revision counter 是 resource-scope-local：舊 canonical 可以已到 rev 10，新的
	// 錯標 resource 卻從 rev 1 開始，不能拿 revision 當全域時間。
	if _, err := s.DB().Exec(`UPDATE deployments SET revision=10,created_at=? WHERE deployment_id=?`,
		fmtTime(now.Add(-2*time.Hour)), old.DeploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE deployments SET resource_kind='app',resource_id='x',revision=1,created_at=? WHERE deployment_id=?`,
		fmtTime(now.Add(-time.Hour)), newer.DeploymentID); err != nil {
		t.Fatal(err)
	}

	if facts, err := s.PromoteFacts("2026.9.2", digest, now); !errors.Is(err, ErrDeploymentMaterialMismatch) {
		t.Fatalf("較新的錯標 OpenClaw canary 沒遮住舊 witness：facts=%+v err=%v", facts, err)
	}
}

func TestPromoteFactsDetectsSameVersionDifferentDigestAppliedLater(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	oldDigest, newDigest := strings.Repeat("1", 64), strings.Repeat("2", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	canary, canaryJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", oldDigest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, canaryJobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, finished)
	establishPromoteCoverage(t, s, "cnode", "samplehub1", "2026.9.2", finished, now)
	stable, stableJobs := createPromoteDeployment(t, s, "stable", "2026.9.2", newDigest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, stableJobs["cnode"], deploy.Succeeded, finished.Add(time.Hour))
	finishPromoteDeployment(t, s, stable.DeploymentID, DeploymentRunning, finished.Add(time.Hour))

	facts, err := s.PromoteFacts("2026.9.2", oldDigest, now)
	if err != nil || facts.Canary == nil || len(facts.Canary.Targets) != 1 {
		t.Fatalf("facts=%+v err=%v", facts, err)
	}
	if got := facts.Canary.Targets[0].AppliedDigest; got != newDigest {
		t.Fatalf("latest applied digest=%q，預期 %q", got, newDigest)
	}
	if d := rollout.PromoteGate(facts, now, time.UTC); d.Allowed ||
		!strings.Contains(d.Summary(), "lineage 外重套") ||
		!strings.Contains(d.Summary(), "重跑 canary") {
		t.Fatalf("同版換 artifact 沒擋住：%s", d.Summary())
	}
}

func TestPromoteFactsLatestAppliedAttemptUsesAppendChronology(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      deploy.JobState
		newDigest  string
		wantReason string
	}{
		{name: "later failure with rolled-back terminal", state: deploy.Failed, newDigest: strings.Repeat("1", 64), wantReason: "failed"},
		{name: "later different digest with rolled-back terminal", state: deploy.Succeeded, newDigest: strings.Repeat("2", 64), wantReason: "digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			n, canaryFinished := eligiblePromotionFixture(t, s)
			now := s.now()
			canaryDigest := strings.TrimPrefix(n.Job.ArtifactDigest, "sha256:")
			spec := `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + tc.newDigest + `"}}`
			createSucceededPromoteWitness(t, s, "cnode", spec, "sha256:"+tc.newDigest, canaryFinished.Add(-time.Hour))
			if tc.state != deploy.Succeeded {
				if _, err := s.DB().Exec(`UPDATE jobs SET state=?
				 WHERE job_id=(SELECT job_id FROM jobs WHERE machine_id='cnode' ORDER BY rowid DESC LIMIT 1)`, tc.state); err != nil {
					t.Fatal(err)
				}
			}

			facts, err := s.PromoteFacts("2026.9.2", canaryDigest, now)
			if err != nil {
				t.Fatal(err)
			}
			decision := rollout.PromoteGate(facts, now, time.UTC)
			if decision.Allowed || !strings.Contains(strings.ToLower(decision.Summary()), tc.wantReason) {
				t.Fatalf("後 append attempt 被較大的 terminal_at 藏住：%s", decision.Summary())
			}
			if tc.state == deploy.Succeeded && (facts.Canary == nil || facts.Canary.Targets[0].AppliedDigest != tc.newDigest) {
				t.Fatalf("後 append digest 沒成為 current applied boundary：%+v", facts.Canary)
			}
		})
	}
}

func TestPromoteFactsCurrentLineageSuccessDoesNotNeedLaterTerminalClock(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("4", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	root, rootJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, rootJobs["cnode"], deploy.Failed, finished.Add(time.Hour))
	finishPromoteDeployment(t, s, root.DeploymentID, DeploymentRunning, finished.Add(time.Hour))
	retry, retryJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, root.DeploymentID,
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	// Current lineage success 是後 append 的 authoritative attempt。它的 terminal
	// clock 即使回撥，也不該被 ancestor 較大的 terminal_at 重新遮住。
	setPromoteJobState(t, s, retryJobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, retry.DeploymentID, DeploymentRunning, finished)
	establishPromoteCoverage(t, s, "cnode", "samplehub1", "2026.9.2", finished, now)

	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if decision := rollout.PromoteGate(facts, now, time.UTC); !decision.Allowed {
		t.Fatalf("current lineage success 被 ancestor terminal clock 遮住：%s", decision.Summary())
	}
}

func TestPromoteFactsExternalSameDigestAlwaysRequiresFreshCanary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta time.Duration
	}{
		{name: "rolled back", delta: -time.Second},
		{name: "same second", delta: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			n, canaryFinished := eligiblePromotionFixture(t, s)
			now := s.now()
			digest := strings.TrimPrefix(n.Job.ArtifactDigest, "sha256:")
			spec := `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`
			createSucceededPromoteWitness(t, s, "cnode", spec, "sha256:"+digest, canaryFinished.Add(tc.delta))

			facts, err := s.PromoteFacts("2026.9.2", digest, now)
			if err != nil {
				t.Fatal(err)
			}
			decision := rollout.PromoteGate(facts, now, time.UTC)
			if decision.Allowed || !strings.Contains(decision.Summary(), "lineage 外重套") ||
				!strings.Contains(decision.Summary(), "重跑 canary") {
				t.Fatalf("lineage 外同 digest 沒有要求重跑 canary：%s", decision.Summary())
			}
		})
	}
}

func TestPromoteFactsFreshnessBelongsToTheSelectedOpenClawVersionEvidence(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("3", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	// 被選中的目標版本 evidence 是 48 小時前收到的；上面的 fresh check-in 與另一批更早收到的
	// 不同版本觀測，都不能替它冒充 freshness。
	if err := s.RecordObservation("cnode", model.ObservationBatch{
		MeasuredAt: now.Add(-72 * time.Hour),
		OpenClaw:   model.OpenClaw{Install: &model.OpenClawInstall{RunningDirVersion: "2026.9.1"}},
	}, now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", model.ObservationBatch{
		MeasuredAt: now.Add(-48 * time.Hour),
		OpenClaw:   model.OpenClaw{Install: &model.OpenClawInstall{RunningDirVersion: "2026.9.2"}},
	}, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, finished)
	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	decision := rollout.PromoteGate(facts, now, time.UTC)
	if decision.Allowed || !strings.Contains(decision.Summary(), "OpenClaw 不存在") {
		t.Fatalf("fresh check-in 與更早的不同版本觀測洗亮舊目標版本 evidence：%s", decision.Summary())
	}
}

func TestPromoteFactsRejectsNonCanonicalLatestCanaryDigest(t *testing.T) {
	canonical := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name   string
		digest string
	}{
		{"大寫", strings.ToUpper(canonical)},
		{"太短", canonical[:63]},
		{"帶 sha256 prefix", "sha256:" + canonical},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			d, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", tc.digest, "",
				[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
			setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, now.Add(-48*time.Hour))
			finishPromoteDeployment(t, s, d.DeploymentID, DeploymentRunning, now.Add(-48*time.Hour))

			if facts, err := s.PromoteFacts("2026.9.2", canonical, now); err == nil {
				t.Fatalf("非 canonical latest canary digest 沒拒絕：facts=%+v", facts)
			}
		})
	}
}

func TestPromoteFactsRejectsNonCanonicalDigestInRetryLineage(t *testing.T) {
	canonical := strings.Repeat("b", 64)
	for _, tc := range []struct {
		name   string
		digest string
	}{
		{"大寫", strings.ToUpper(canonical)},
		{"太短", canonical[:63]},
		{"帶 sha256 prefix", "sha256:" + canonical},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			finished := now.Add(-48 * time.Hour)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			root, rootJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", canonical, "",
				[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
			setPromoteJobState(t, s, rootJobs["cnode"], deploy.Succeeded, finished)
			finishPromoteDeployment(t, s, root.DeploymentID, DeploymentRunning, finished)
			retry, retryJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", canonical, root.DeploymentID,
				[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
			setPromoteJobState(t, s, retryJobs["cnode"], deploy.Succeeded, finished.Add(time.Minute))
			finishPromoteDeployment(t, s, retry.DeploymentID, DeploymentRunning, finished.Add(time.Minute))
			// 模擬舊版 store 曾留下、或 DB 被事後改壞的 ancestry；新 create path 已會先擋。
			if _, err := s.DB().Exec(`UPDATE desired_state SET spec=? WHERE desired_id=?`,
				`{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"`+tc.digest+`"}}`, root.DesiredID); err != nil {
				t.Fatal(err)
			}

			if facts, err := s.PromoteFacts("2026.9.2", canonical, now); err == nil {
				t.Fatalf("lineage ancestor 的非 canonical digest 沒拒絕：facts=%+v", facts)
			}
		})
	}
}

func TestLatestSucceededOpenClawDigestSkipsNoiseAndFindsOlderLegitimateWitness(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	digest := strings.Repeat("c", 64)
	base := time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC)
	createSucceededPromoteWitness(t, s, "cnode",
		`{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"`+digest+`"}}`, "sha256:"+digest, base)
	createSucceededPromoteWitness(t, s, "cnode", `{"kind":"noop"}`, "sha256:"+digest, base.Add(time.Minute))
	createSucceededPromoteWitness(t, s, "cnode", `{壞 JSON`, "sha256:"+digest, base.Add(2*time.Minute))

	got, err := s.latestSucceededOpenClawDigests([]string{"cnode"})
	if err != nil || got["cnode"] != digest {
		t.Fatalf("noop／壞 JSON 不該遮住較舊合法 OpenClaw witness：got=%+v err=%v", got, err)
	}
}

func TestLatestSucceededOpenClawDigestMislabeledExplicitSpecMasksOlderWitness(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	digest := strings.Repeat("f", 64)
	base := time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC)
	createSucceededPromoteWitness(t, s, "cnode",
		`{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"`+digest+`"}}`, "sha256:"+digest, base)
	createSucceededPromoteWitness(t, s, "cnode",
		`{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"`+digest+`"}}`, "sha256:"+digest, base.Add(time.Minute))
	if _, err := s.DB().Exec(`UPDATE desired_state SET resource_kind='app',resource_id='x'
	 WHERE desired_id=(SELECT desired_id FROM jobs WHERE machine_id='cnode' ORDER BY terminal_at DESC,rowid DESC LIMIT 1)`); err != nil {
		t.Fatal(err)
	}

	got, err := s.latestSucceededOpenClawDigests([]string{"cnode"})
	if err != nil {
		t.Fatal(err)
	}
	if got["cnode"] != "" {
		t.Fatalf("較新的錯標 explicit OpenClaw success 沒遮住舊 witness：%q", got["cnode"])
	}
}

func TestLatestSucceededOpenClawDigestFailsClosedOnAnyNullTerminal(t *testing.T) {
	for _, tc := range []struct {
		name             string
		nullCreatedFirst bool
	}{
		{"older-created NULL then newer terminal", true},
		{"newer-created NULL after older terminal", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			validDigest := strings.Repeat("1", 64)
			nullDigest := strings.Repeat("2", 64)
			base := time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC)
			create := func(digest string, terminal time.Time) {
				createSucceededPromoteWitness(t, s, "cnode",
					`{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"`+digest+`"}}`,
					"sha256:"+digest, terminal)
			}
			if tc.nullCreatedFirst {
				create(nullDigest, base)
				create(validDigest, base.Add(2*time.Hour))
			} else {
				create(validDigest, base.Add(2*time.Hour))
				create(nullDigest, base)
			}
			nullCreated := base
			validCreated := base.Add(time.Hour)
			if !tc.nullCreatedFirst {
				nullCreated, validCreated = base.Add(time.Hour), base
			}
			if _, err := s.DB().Exec(`UPDATE jobs SET terminal_at=NULL,created_at=? WHERE machine_id='cnode' AND artifact_digest=?`,
				fmtTime(nullCreated), "sha256:"+nullDigest); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`UPDATE jobs SET created_at=? WHERE machine_id='cnode' AND artifact_digest=?`,
				fmtTime(validCreated), "sha256:"+validDigest); err != nil {
				t.Fatal(err)
			}

			got, err := s.latestSucceededOpenClawDigests([]string{"cnode"})
			if err != nil {
				t.Fatal(err)
			}
			if got["cnode"] != "" {
				t.Fatalf("succeeded OpenClaw terminal_at=NULL 仍產生 witness：%q", got["cnode"])
			}
		})
	}
}

func TestPromoteFactsAppliedWitnessUsesLatestOpenClawAttemptState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   deploy.JobState
		allowed bool
	}{
		{"failed masks success", deploy.Failed, false},
		{"manual intervention masks success", deploy.ManualIntervention, false},
		{"lease expired masks success", deploy.LeaseExpired, false},
		{"nonterminal masks success", deploy.Running, false},
		{"rejected pre-mutation attempt is skipped", deploy.Rejected, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			finished := now.Add(-48 * time.Hour)
			digest := strings.Repeat("3", 64)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			observePromoteMachine(t, s, "cnode", "2026.9.2", now)
			canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
				[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
			setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, finished)
			finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, finished)
			establishPromoteCoverage(t, s, "cnode", "samplehub1", "2026.9.2", finished, now)
			createSucceededPromoteWitness(t, s, "cnode",
				`{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"`+digest+`"}}`,
				"sha256:"+digest, finished.Add(time.Hour))
			terminalAt := any(fmtTime(finished.Add(time.Hour)))
			if !deploy.IsTerminal(tc.state) {
				terminalAt = nil
			}
			if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=?
			 WHERE job_id=(SELECT job_id FROM jobs WHERE machine_id='cnode' ORDER BY rowid DESC LIMIT 1)`, tc.state, terminalAt); err != nil {
				t.Fatal(err)
			}

			facts, err := s.PromoteFacts("2026.9.2", digest, now)
			if err != nil {
				t.Fatal(err)
			}
			decision := rollout.PromoteGate(facts, now, time.UTC)
			if decision.Allowed != tc.allowed {
				t.Fatalf("latest state=%s allowed=%v want=%v，fresh same-version observation 不該洗亮：%s",
					tc.state, decision.Allowed, tc.allowed, decision.Summary())
			}
		})
	}
}

func TestLatestSucceededOpenClawDigestRequiresCanonicalSpecAndExactJobDigest(t *testing.T) {
	canonical := strings.Repeat("d", 64)
	for _, tc := range []struct {
		name       string
		specDigest string
		jobDigest  string
	}{
		{"job digest 沒 prefix", canonical, canonical},
		{"job digest 大寫", canonical, "sha256:" + strings.ToUpper(canonical)},
		{"job digest 太短", canonical, "sha256:" + canonical[:63]},
		{"spec digest 大寫", strings.ToUpper(canonical), "sha256:" + strings.ToUpper(canonical)},
		{"spec digest 太短", canonical[:63], "sha256:" + canonical[:63]},
		{"spec digest 帶 prefix", "sha256:" + canonical, "sha256:sha256:" + canonical},
		{"spec 缺 artifact", "", "sha256:" + canonical},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			base := time.Date(2026, 9, 9, 9, 0, 0, 0, time.UTC)
			createSucceededPromoteWitness(t, s, "cnode",
				`{"kind":"openclaw","version":"2026.9.1","artifact":{"sha256":"`+canonical+`"}}`, "sha256:"+canonical, base)
			latestSpec := `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + tc.specDigest + `"}}`
			if tc.specDigest == "" {
				latestSpec = `{"kind":"openclaw","version":"2026.9.2"}`
			}
			createSucceededPromoteWitness(t, s, "cnode", latestSpec, tc.jobDigest, base.Add(time.Minute))

			got, err := s.latestSucceededOpenClawDigests([]string{"cnode"})
			if err != nil {
				t.Fatal(err)
			}
			if got["cnode"] != "" {
				t.Fatalf("非 canonical／不精確 digest 產生 AppliedDigest：%q", got["cnode"])
			}
		})
	}
}

// 歷史區間與現在時的沉默失敗規則使用不同時間軸，因此會有一段現在時
// 已紅、帳本還沒寫的窗；在這段窗裡，現在時判決是唯一能阻擋 promotion 的閘。
func TestPromotionIsLockedByASilenceThatOnlyTheCurrentJudgementSees(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	if baseline := promoteDecision(t, s, n, now); !baseline.Allowed {
		t.Fatalf("baseline allowed=%v want=true：baseline 不放行時，後續鎖定無法歸因於現在時沉默失敗；summary=%q",
			baseline.Allowed, baseline.Summary())
	}

	// 這一批必須嚴格晚於 fixture 自己在 now 送的那批：evidence watermark
	// 只收比上一批新的量測時間，同一秒重送會被判成證據不完整的 unknown，
	// 連帶讓 workload witness 失效，測不到只有現在時沉默失敗擋下的那段窗。
	observeAt := now.Add(time.Second)
	lastTask := observeAt.Add(-state.SilentFailure).Add(time.Minute)
	matched := true
	if err := s.RecordCheckin("cnode", model.Checkin{
		SentAt: observeAt, AgentStartedAt: observeAt,
	}, observeAt); err != nil {
		t.Fatal(err)
	}
	b := model.ObservationBatch{
		MeasuredAt: observeAt,
		OpenClaw: model.OpenClaw{
			Present: true,
			Install: &model.OpenClawInstall{
				UnitFound: true, MainPID: 42, ProcessMatchesUnit: &matched,
				RunningDirVersion: "2026.9.2", NodeVersion: "24.15.0",
			},
			DB: &model.OpenClawDB{
				Present: true, TaskRunRows: 1, LastTaskEndedAt: &lastTask,
			},
		},
		CLITools: []model.CLITool{{
			Name: "openclaw", Present: true, RunningPID: 42,
		}},
	}
	stampCurrentWorkloadPolicy(t, s, s.DisplayName("cnode"), &b)
	if err := s.RecordObservation("cnode", b, observeAt); err != nil {
		t.Fatal(err)
	}

	decisionAt := observeAt.Add(61 * time.Second)
	if err := s.RecordCheckin("cnode", model.Checkin{
		SentAt: decisionAt, AgentStartedAt: decisionAt,
	}, decisionAt); err != nil {
		t.Fatal(err)
	}
	facts, err := s.PromoteFacts("2026.9.2", n.Job.ArtifactDigest, decisionAt)
	if err != nil {
		t.Fatal(err)
	}
	decision := rollout.PromoteGate(facts, decisionAt, time.UTC)

	if len(facts.Silent) != 0 {
		t.Fatalf("facts.Silent length=%d want=0：歷史區間臂若已開火，就無法證明 writer lock 阻止 ReconcileFleet 寫入時，現在時判決是唯一防線",
			len(facts.Silent))
	}
	if facts.Canary == nil {
		t.Fatalf("facts.Canary nil=%v want=false：缺少 canary 判決資料時，無法排除非現在時沉默失敗的 promotion 鎖定來源",
			facts.Canary == nil)
	}
	var target *rollout.CanaryTarget
	for i := range facts.Canary.Targets {
		if facts.Canary.Targets[i].MachineID == "cnode" {
			target = &facts.Canary.Targets[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("cnode target present=%v want=true：唯一 canary 證人不在判決資料時，無法量測它超過八小時未完成任務所觸發的現在時安全閘",
			target != nil)
	}
	if !target.Judged {
		t.Fatalf("target.Judged=%v want=true：未完成現在時判斷會讓 operator 無法確認 promotion 是被沉默失敗而非缺少判決擋下",
			target.Judged)
	}
	if target.Unreachable {
		t.Fatalf("target.Unreachable=%v want=false：失聯臂若開火，就無法證明持續 check-in 的 canary 是由現在時沉默失敗獨自擋下",
			target.Unreachable)
	}
	if !target.WorkloadReady {
		t.Fatalf("target.WorkloadReady=%v want=true workloadReason=%q：witness 臂若開火，就無法證明 workload 證據仍有效時只有現在時沉默失敗阻止 stable 開單",
			target.WorkloadReady, target.WorkloadReason)
	}

	if !target.SilentNow {
		t.Errorf("target.SilentNow=%v want=true：此臂失效會讓 Allowed 從 false 變成 true，Hub 在 writer lock 令 ReconcileFleet 無法補寫歷史區間時，仍替全機隊開出 stable deployment，儘管唯一 canary 已超過八小時未完成任務",
			target.SilentNow)
	}
	if decision.Allowed {
		t.Errorf("decision.Allowed=%v want=false：錯誤放行會讓 Hub 真的替全機隊開出 stable deployment；create 持有 writer lock 時歷史臂無法救回這次開單，而唯一 canary 已沉默超過八小時",
			decision.Allowed)
	}
	wantSummary := "samplehub1 現在就有沉默失敗"
	if !strings.Contains(decision.Summary(), wantSummary) {
		t.Errorf("decision.Summary()=%q want substring=%q：operator 必須讀到是哪台機器現在沉默；若只說期間內有沉默失敗，會誤導他查找 writer lock 下尚未存在的歷史區間",
			decision.Summary(), wantSummary)
	}
}
