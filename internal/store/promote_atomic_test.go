package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/state"
)

// seedPromoteCoverage 是測試專用的歷史 ledger fixture。真正的 ingest 路徑由
// promote_coverage_test 逐批 RecordObservation 覆蓋；共用 eligible fixture 直接
// 塞舊 cadence，避免每支 gate test 都重複寫數千列 raw observation。
func seedPromoteCoverage(t *testing.T, s *Store, machineID, displayName, version string, from, until time.Time) {
	t.Helper()
	token, err := s.CurrentWorkloadPolicyToken(displayName)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for at := from.UTC().Truncate(time.Second); at.Before(until); at = at.Add(state.ObservationStale) {
		if _, err := tx.Exec(`INSERT INTO workload_observation_evidence
		 (machine_id,received_at,evidence_at,verdict,openclaw_present,running_version,workload_policy_token,policy_valid)
		 VALUES(?,?,?,?,?,?,?,?)`, machineID, fmtTime(at), fmtTime(at), workloadWitnessHealthy,
			true, version, token, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// establishPromoteCoverage 把舊 gate fixture 的「只在現在觀測一次」改成明確的
// post-canary coverage。先清掉 pre-canary test projection，才能讓 evidence_id 與
// Hub received_at 都按時間前進；production 沒有這個 helper。
func establishPromoteCoverage(t *testing.T, s *Store, machineID, displayName, version string, from, until time.Time) {
	t.Helper()
	if _, err := s.DB().Exec(`DELETE FROM workload_observation_evidence WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DELETE FROM workload_observation_witness WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	seedPromoteCoverage(t, s, machineID, displayName, version, from, until)
	observePromoteMachine(t, s, machineID, version, until)
}

func eligiblePromotionFixture(t *testing.T, s *Store) (NewDeployment, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	digest := strings.Repeat("a", 64)
	s.nowFn = func() time.Time { return now }
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, finished)
	establishPromoteCoverage(t, s, "cnode", "samplehub1", "2026.9.2", finished, now)
	if err := s.SetMachineChannel("cnode", "stable"); err != nil {
		t.Fatal(err)
	}
	n := NewDeployment{
		Channel: "stable", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		BatchSize: 1, CreatedBy: "atomic-test",
		Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
		Job:     NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	}
	return n, finished
}

// eligibleTwoBatchStableFixture 先用 onode 開第一批，避免把 canary 證人
// cnode 的 applied terminal time 推到現在；這樣第二批測到的鎖確實來自新證據，
// 不是完整工作天尚未走完。
func eligibleTwoBatchStableFixture(t *testing.T, s *Store) (Deployment, []Job, time.Time) {
	t.Helper()
	n, finished := eligiblePromotionFixture(t, s)
	addRolloutMachine(t, s, "onode", "sampleagent2", false)
	observePromoteMachine(t, s, "onode", "2026.9.2", s.now())
	if err := s.SetMachineChannel("onode", "stable"); err != nil {
		t.Fatal(err)
	}
	n.Targets = []NewDeploymentTarget{
		{MachineID: "onode", BatchNo: 1},
		{MachineID: "cnode", BatchNo: 2},
	}
	d, jobs, decision, err := s.createStableOpenClawDeployment(n, time.UTC)
	if err != nil || !decision.Allowed {
		t.Fatalf("create stable fixture err=%v decision=%s", err, decision.Summary())
	}
	if len(jobs) != 1 || jobs[0].MachineID != "onode" {
		t.Fatalf("initial stable jobs=%+v", jobs)
	}
	setPromoteJobState(t, s, jobs[0], deploy.Succeeded, s.now())
	return d, jobs, finished
}

func deploymentWriteCounts(t *testing.T, s *Store) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"revision_counters", "desired_state", "deployments", "deployment_targets", "jobs"} {
		var count int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = count
	}
	return out
}

func assertDeploymentWriteCounts(t *testing.T, s *Store, want map[string]int) {
	t.Helper()
	got := deploymentWriteCounts(t, s)
	for table, n := range want {
		if got[table] != n {
			t.Errorf("%s writes=%d, want %d", table, got[table], n)
		}
	}
}

func TestGenericCreateDeploymentCannotBypassStablePromoteGate(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	digest := strings.Repeat("a", 64)
	n := NewDeployment{
		Channel: "stable", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		BatchSize: 1, CreatedBy: "bypass-test",
		Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
		Job:     NewJob{ArtifactDigest: "sha256:" + digest},
	}
	before := deploymentWriteCounts(t, s)
	if _, _, err := s.CreateDeployment(n); !errors.Is(err, ErrStablePromoteGateRequired) {
		t.Fatalf("raw stable create error=%v, want ErrStablePromoteGateRequired", err)
	}
	assertDeploymentWriteCounts(t, s, before)
}

func TestStableCreateRechecksAfterPreviewAndSeesSilentFailure(t *testing.T) {
	s := rolloutStore(t)
	n, finished := eligiblePromotionFixture(t, s)
	now := s.now()
	facts, err := s.PromoteFacts("2026.9.2", n.Job.ArtifactDigest, now)
	if err != nil || !rollout.PromoteGate(facts, now, time.UTC).Allowed {
		t.Fatalf("initial preview facts=%+v err=%v", facts, err)
	}
	if recorded, err := s.RecordCanarySilentFailure("cnode", "preview 後才看到的沉默失敗", finished.Add(time.Hour)); err != nil || !recorded {
		t.Fatalf("record failure=%v err=%v", recorded, err)
	}
	before := deploymentWriteCounts(t, s)
	_, _, decision, err := s.createStableOpenClawDeployment(n, time.UTC)
	if !errors.Is(err, ErrPromoteLocked) || decision.Allowed || !strings.Contains(decision.Summary(), "preview 後才看到") {
		t.Fatalf("create err=%v decision=%s", err, decision.Summary())
	}
	assertDeploymentWriteCounts(t, s, before)
}

// 一個壞掉的完整觀測可能在 30 秒 periodic reconcile 前就被下一個恢復觀測
// 蓋過。promote 不能只看 latest，也不能指望 reconcile 剛好來得及看到中間那格。
func TestObservationFailureThenRecoveryBeforeReconcileStillLocksStableCreate(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	failureAt := s.now().Add(time.Minute)
	recoveryAt := failureAt.Add(time.Minute)
	lastTask := recoveryAt.Add(-time.Minute)
	matched := true

	failure := model.ObservationBatch{
		MeasuredAt: failureAt,
		OpenClaw: model.OpenClaw{
			Present: true,
			Install: &model.OpenClawInstall{UnitFound: true, MainPID: 41, ProcessMatchesUnit: &matched,
				RunningDirVersion: "2026.9.2", NodeVersion: "24.15.0"},
			DB: &model.OpenClawDB{Present: true, TaskRunRows: 1, LastTaskEndedAt: &lastTask},
		},
		CLITools: []model.CLITool{
			{Name: "openclaw", Present: true, RunningPID: 41},
			{Name: "openclaw", Present: true, RunningReason: "掃過 process，沒有找到 OpenClaw"},
		},
	}
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &failure)
	if err := s.RecordObservation("cnode", failure, failureAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: recoveryAt, AgentStartedAt: recoveryAt}, recoveryAt); err != nil {
		t.Fatal(err)
	}
	recovery := model.ObservationBatch{
		MeasuredAt: recoveryAt,
		OpenClaw: model.OpenClaw{
			Present: true,
			Install: &model.OpenClawInstall{UnitFound: true, MainPID: 42, ProcessMatchesUnit: &matched,
				RunningDirVersion: "2026.9.2", NodeVersion: "24.15.0"},
			DB: &model.OpenClawDB{Present: true, TaskRunRows: 1, LastTaskEndedAt: &lastTask},
		},
		CLITools: []model.CLITool{{Name: "openclaw", Present: true, RunningPID: 42}},
	}
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &recovery)
	if err := s.RecordObservation("cnode", recovery, recoveryAt); err != nil {
		t.Fatal(err)
	}
	var open int
	if err := s.DB().QueryRow(`SELECT open FROM canary_silent_failures WHERE machine_id='cnode' ORDER BY first_seen_at DESC LIMIT 1`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("coherent recovery 沒在 observation transaction 內關 span：open=%d err=%v", open, err)
	}
	decisionAt := recoveryAt.Add(time.Minute)
	s.nowFn = func() time.Time { return decisionAt }

	before := deploymentWriteCounts(t, s)
	_, _, decision, err := s.createStableOpenClawDeployment(n, time.UTC)
	if !errors.Is(err, ErrPromoteLocked) || decision.Allowed || !strings.Contains(decision.Summary(), "process 不存在") {
		t.Fatalf("create err=%v decision=%s", err, decision.Summary())
	}
	assertDeploymentWriteCounts(t, s, before)
}

func TestStableCreateRechecksAfterPreviewAndSeesNewerCanary(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	facts, err := s.PromoteFacts("2026.9.2", n.Job.ArtifactDigest, now)
	if err != nil || !rollout.PromoteGate(facts, now, time.UTC).Allowed {
		t.Fatalf("initial preview facts=%+v err=%v", facts, err)
	}
	newDigest := strings.Repeat("b", 64)
	_, _ = createPromoteDeployment(t, s, "canary", "2026.9.3", newDigest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	before := deploymentWriteCounts(t, s)
	_, _, decision, err := s.createStableOpenClawDeployment(n, time.UTC)
	if !errors.Is(err, ErrPromoteLocked) || decision.Allowed || !strings.Contains(decision.Summary(), "最新 canary 部署") {
		t.Fatalf("create err=%v decision=%s", err, decision.Summary())
	}
	assertDeploymentWriteCounts(t, s, before)
}

func TestStableCreateFailsClosedOnNewestLegacyOpenClawResourceWithInvalidSpec(t *testing.T) {
	canonical := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name string
		spec string
	}{
		{name: "malformed", spec: `{broken`},
		{name: "noop", spec: `{"kind":"noop"}`},
		{name: "noncanonical digest", spec: `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + strings.ToUpper(canonical) + `"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			n, _ := eligiblePromotionFixture(t, s)
			now := s.now()
			// This test isolates attempt selection. Other promote-evidence tests own
			// the fixture's historical silent-failure rows.
			if _, err := s.DB().Exec(`DELETE FROM canary_silent_failures`); err != nil {
				t.Fatal(err)
			}
			baseline, err := s.PromoteFacts("2026.9.2", canonical, now)
			baselineDecision := rollout.PromoteGate(baseline, now, time.UTC)
			if err != nil || !baselineDecision.Allowed {
				t.Fatalf("test setup: old canary is not eligible: %s facts=%+v err=%v", baselineDecision.Summary(), baseline, err)
			}

			// 8d761f5's exported CreateDeployment accepted this shape. Create a real
			// later canary row first, then reproduce the legacy material it could leave.
			newer, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", canonical, "",
				[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
			setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, now.Add(-time.Hour))
			finishPromoteDeployment(t, s, newer.DeploymentID, DeploymentRunning, now.Add(-time.Hour))
			if _, err := s.DB().Exec(`UPDATE desired_state SET spec=? WHERE desired_id=?`, tc.spec, newer.DesiredID); err != nil {
				t.Fatal(err)
			}
			if err := s.SetMachineChannel("cnode", "stable"); err != nil {
				t.Fatal(err)
			}

			if facts, err := s.PromoteFacts("2026.9.2", canonical, now); !errors.Is(err, ErrDeploymentMaterialMismatch) {
				t.Fatalf("newest legacy OpenClaw material fell through to old green: facts=%+v err=%v", facts, err)
			}
			before := deploymentWriteCounts(t, s)
			if _, _, _, err := s.createStableOpenClawDeployment(n, time.UTC); !errors.Is(err, ErrDeploymentMaterialMismatch) {
				t.Fatalf("actual stable create did not fail closed: err=%v", err)
			}
			assertDeploymentWriteCounts(t, s, before)
		})
	}
}

func TestPromoteFactsSkipsNewerUnrelatedCanaryResource(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	if _, err := s.DB().Exec(`DELETE FROM canary_silent_failures`); err != nil {
		t.Fatal(err)
	}
	baseline, err := s.PromoteFacts("2026.9.2", n.Job.ArtifactDigest, now)
	baselineDecision := rollout.PromoteGate(baseline, now, time.UTC)
	if err != nil || baseline.Canary == nil || !baselineDecision.Allowed {
		t.Fatalf("test setup: %s facts=%+v err=%v", baselineDecision.Summary(), baseline, err)
	}
	oldCanaryID := baseline.Canary.DeploymentID
	if err := s.SetMachineChannel("cnode", "canary"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "app", ResourceID: "unrelated", Spec: `{"kind":"app"}`,
		BatchSize: 1, CreatedBy: "unrelated-test",
		Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
		Job:     NewJob{ExecutionTimeout: 600},
	}); err != nil {
		t.Fatal(err)
	}

	facts, err := s.PromoteFacts("2026.9.2", n.Job.ArtifactDigest, now)
	if err != nil || facts.Canary == nil || facts.Canary.DeploymentID != oldCanaryID ||
		!rollout.PromoteGate(facts, now, time.UTC).Allowed {
		t.Fatalf("unrelated newer canary masked canonical OpenClaw witness: facts=%+v err=%v", facts, err)
	}
}

func TestStableCreateAtomicallySucceedsWithEligibleEvidence(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	d, jobs, decision, err := s.createStableOpenClawDeployment(n, time.UTC)
	if err != nil || !decision.Allowed {
		t.Fatalf("create err=%v decision=%s", err, decision.Summary())
	}
	if d.Channel != "stable" || len(jobs) != 1 || jobs[0].MachineID != "cnode" {
		t.Fatalf("deployment=%+v jobs=%+v", d, jobs)
	}
	var stable int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM deployments WHERE channel='stable'`).Scan(&stable); err != nil || stable != 1 {
		t.Fatalf("stable deployments=%d err=%v", stable, err)
	}
}

func TestStableCreateUsesHubLocalTimezoneRatherThanCallerChoice(t *testing.T) {
	s := rolloutStore(t)
	zoneData, err := os.ReadFile("/etc/localtime")
	if err != nil {
		t.Fatal(err)
	}
	hubLocal, err := time.LoadLocationFromTZData("Hub local", zoneData)
	if err != nil {
		t.Fatal(err)
	}
	// A caller can replace process-local calendar policy with TZ. +23 is
	// deliberately extreme so this regression stays independent of the host's
	// actual zone while demonstrating the earlier business-day boundary.
	callerChosen := time.FixedZone("caller-UTC+23", 23*60*60)
	oldLocal := time.Local
	time.Local = callerChosen
	t.Cleanup(func() { time.Local = oldLocal })
	t.Setenv("TZ", "caller-controlled")

	finished := time.Date(2026, 9, 4, 16, 0, 0, 0, hubLocal) // Friday afternoon at the Hub.
	now := time.Date(2026, 9, 7, 16, 0, 0, 0, hubLocal)      // Monday afternoon at the Hub.
	// This regression isolates calendar-zone authority; make the evidence recorder
	// explicitly predate its canary rather than inheriting rolloutStore's wall clock.
	if _, err := s.DB().Exec(`UPDATE canary_evidence_epoch SET started_at=? WHERE singleton=1`,
		fmtTime(finished.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	s.nowFn = func() time.Time { return finished }
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	digest := strings.Repeat("a", 64)
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, finished)

	s.nowFn = func() time.Time { return now }
	establishPromoteCoverage(t, s, "cnode", "samplehub1", "2026.9.2", finished, now)
	if err := s.SetMachineChannel("cnode", "stable"); err != nil {
		t.Fatal(err)
	}
	n := NewDeployment{
		Channel: "stable", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		BatchSize: 1, CreatedBy: "timezone-test",
		Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
		Job:     NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	}

	// The caller zone counts the Friday Hub completion as a later calendar day and
	// permits this before the host's own full workday has ended. The exported
	// boundary must ignore both time.Local and TZ and read the host calendar.
	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if onode := rollout.PromoteGate(facts, now, callerChosen); !onode.Allowed {
		t.Fatalf("test setup: caller-chosen timezone should bypass: %s", onode.Summary())
	}
	hostOracle := rollout.PromoteGate(facts, now, hubLocal)
	if hostOracle.Allowed {
		t.Fatalf("test setup: host calendar should still be locked: %s", hostOracle.Summary())
	}
	preview, err := s.PreviewStableOpenClawPromotion("2026.9.2", digest, now)
	if err != nil || preview.Allowed || !preview.EarliestAt.Equal(hostOracle.EarliestAt) {
		t.Fatalf("production preview did not use /etc/localtime: preview=%s earliest=%s want=%s err=%v",
			preview.Summary(), preview.EarliestAt, hostOracle.EarliestAt, err)
	}
	before := deploymentWriteCounts(t, s)
	_, _, decision, err := s.CreateStableOpenClawDeployment(n)
	if !errors.Is(err, ErrPromoteLocked) || decision.Allowed {
		t.Fatalf("public stable create err=%v decision=%s", err, decision.Summary())
	}
	assertDeploymentWriteCounts(t, s, before)
}

func TestStablePromotionPreviewFailsClosedWhenHostTimezoneCannotBeLoaded(t *testing.T) {
	s := rolloutStore(t)
	for _, tc := range []struct {
		name string
		read hubLocaltimeReader
	}{
		{"read failure", func(string) ([]byte, error) { return nil, os.ErrNotExist }},
		{"invalid tzdata", func(string) ([]byte, error) { return []byte("not tzdata"), nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := s.previewStableOpenClawPromotion("2026.9.2", strings.Repeat("a", 64), time.Now(),
				func() (*time.Location, error) { return loadHubCalendarLocation(tc.read) })
			if err != nil {
				t.Fatalf("timezone failure should materialize as a locked decision: %v", err)
			}
			if decision.Allowed || !strings.Contains(decision.Summary(), "Hub 主機時區不可用") ||
				!strings.Contains(decision.Summary(), "完整工作天") {
				t.Fatalf("timezone failure did not fail closed: %s", decision.Summary())
			}
		})
	}
}

func TestStableNextBatchRechecksGateAndSeesNewSilentFailure(t *testing.T) {
	s := rolloutStore(t)
	d, _, finished := eligibleTwoBatchStableFixture(t, s)
	const reason = "第一批後才看到的沉默失敗"
	if recorded, err := s.RecordCanarySilentFailure("cnode", reason, finished.Add(time.Hour)); err != nil || !recorded {
		t.Fatalf("record failure=%v err=%v", recorded, err)
	}
	before := deploymentWriteCounts(t, s)
	jobs, err := s.OpenDeploymentBatch(d.DeploymentID, 2, s.now())
	if !errors.Is(err, ErrPromoteLocked) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("open next batch jobs=%+v err=%v", jobs, err)
	}
	assertDeploymentWriteCounts(t, s, before)
}

func TestStableCreateRejectsSpecAndJobDigestMismatchWithoutWrites(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	digest := strings.Repeat("c", 64)
	n := NewDeployment{
		Channel: "stable", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		BatchSize: 1, CreatedBy: "mismatch-test",
		Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
		Job:     NewJob{ArtifactDigest: "sha256:" + strings.Repeat("d", 64)},
	}
	before := deploymentWriteCounts(t, s)
	if _, _, _, err := s.createStableOpenClawDeployment(n, time.UTC); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("digest mismatch err=%v", err)
	}
	assertDeploymentWriteCounts(t, s, before)
}

func TestStableCreateRejectsNonCanonicalDigestWithoutWrites(t *testing.T) {
	lower := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, specDigest, jobDigest, want string
	}{
		{"uppercase spec", strings.ToUpper(lower), "sha256:" + strings.ToUpper(lower), "not canonical"},
		{"space-padded spec", " " + lower + " ", "sha256:" + lower, "not canonical"},
		{"bare job digest", lower, lower, "does not match"},
		{"space-padded job digest", lower, " sha256:" + lower, "does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", true)
			n := NewDeployment{
				Channel: "stable", ResourceKind: "openclaw", ResourceID: "openclaw",
				Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + tc.specDigest + `"}}`,
				BatchSize: 1, CreatedBy: "digest-test",
				Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
				Job:     NewJob{ArtifactDigest: tc.jobDigest},
			}
			before := deploymentWriteCounts(t, s)
			if _, _, _, err := s.createStableOpenClawDeployment(n, time.UTC); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("non-canonical digest err=%v", err)
			}
			assertDeploymentWriteCounts(t, s, before)
		})
	}
}

func TestImmediateTransactionsSerializeAcrossStoreInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	firstTx, err := first.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer firstTx.Rollback()
	type result struct {
		tx  *sql.Tx
		err error
	}
	started := make(chan struct{})
	done := make(chan result, 1)
	go func() {
		close(started)
		tx, err := second.DB().Begin()
		done <- result{tx: tx, err: err}
	}()
	<-started
	select {
	case got := <-done:
		if got.tx != nil {
			_ = got.tx.Rollback()
		}
		t.Fatalf("second Store did not wait for the first writer: %v", got.err)
	case <-time.After(250 * time.Millisecond):
		// 還在等才對；deferred BEGIN 會在這裡立即成功。
	}
	if err := firstTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("second Store could not acquire writer after release: %v", got.err)
		}
		if err := got.tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Store stayed blocked after the first writer released")
	}
}

// Reconcile 的 Overview 與證據落地必須在同一把跨 Store writer reservation 裡。
// 否則 stable create 可以卡進「已算出壞、還沒寫入歷史」的縫裡。
func TestReconcileJudgementAndStableCreateSerializeAcrossStoreInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first.SetExpectations(&expect.Set{})
	if err := first.PublishExpectationsPolicy(rolloutTestEvidenceEpoch); err != nil {
		t.Fatal(err)
	}
	n, _ := eligiblePromotionFixture(t, first)
	second.nowFn = first.nowFn

	// 讓 latest raw state 是 workload failure，再刻意清掉 observation ingest 已經
	// 投影的 span；這樣本測試只證明 periodic reconcile 自己的交易邊界。
	// The observation must already belong to the Hub ledger at the reconcile
	// evaluation instant. Overview now enforces that received_at cutoff.
	failureAt := first.now()
	if err := first.RecordObservation("cnode", model.ObservationBatch{
		MeasuredAt: failureAt,
		OpenClaw: model.OpenClaw{
			Present: true,
			Install: &model.OpenClawInstall{RunningDirVersion: "2026.9.2", NodeVersion: "24.15.0"},
		},
		CLITools: []model.CLITool{{Name: "openclaw", Present: true, RunningReason: "掃過 process，沒有找到 OpenClaw"}},
	}, failureAt); err != nil {
		t.Fatal(err)
	}
	if _, err := first.DB().Exec(`DELETE FROM canary_silent_failures WHERE machine_id='cnode'`); err != nil {
		t.Fatal(err)
	}

	overviewDone := make(chan struct{})
	release := make(chan struct{})
	first.afterReconcileOverview = func() {
		close(overviewDone)
		<-release
	}
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- first.ReconcileFleet(first.now()) }()
	<-overviewDone

	type createResult struct {
		decision rollout.PromoteDecision
		err      error
	}
	createDone := make(chan createResult, 1)
	go func() {
		_, _, decision, err := second.createStableOpenClawDeployment(n, time.UTC)
		createDone <- createResult{decision: decision, err: err}
	}()
	select {
	case got := <-createDone:
		t.Fatalf("stable create crossed reconcile Overview/materialize boundary: err=%v decision=%s", got.err, got.decision.Summary())
	case <-time.After(250 * time.Millisecond):
		// 還在等 reconcile 的 BEGIN IMMEDIATE writer reservation 才對。
	}
	close(release)
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-createDone:
		if !errors.Is(got.err, ErrPromoteLocked) || got.decision.Allowed {
			t.Fatalf("stable create after reconcile err=%v decision=%s", got.err, got.decision.Summary())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stable create stayed blocked after reconcile committed")
	}
	var failures int
	if err := first.DB().QueryRow(`SELECT COUNT(*) FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&failures); err != nil || failures != 1 {
		t.Fatalf("reconcile failure facts=%d err=%v", failures, err)
	}
}

// 第二批 stable 的 gate judgement 與 job insert 必須共用同一把 SQLite writer
// reservation。另一個 Store 在中間寫入 failure，只能完整排在這批 job 前或後。
func TestStableBatchGateAndJobInsertSerializeAcrossStoreInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first.SetExpectations(&expect.Set{})
	if err := first.PublishExpectationsPolicy(rolloutTestEvidenceEpoch); err != nil {
		t.Fatal(err)
	}
	d, _, finished := eligibleTwoBatchStableFixture(t, first)
	second.nowFn = first.nowFn

	gateDone := make(chan struct{})
	releaseGate := make(chan struct{})
	first.afterBatchPromoteGate = func() {
		close(gateDone)
		<-releaseGate
	}
	type openResult struct {
		jobs []Job
		err  error
	}
	openDone := make(chan openResult, 1)
	go func() {
		jobs, err := first.OpenDeploymentBatch(d.DeploymentID, 2, first.now())
		openDone <- openResult{jobs: jobs, err: err}
	}()
	<-gateDone

	writerStarted := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		close(writerStarted)
		_, err := second.RecordCanarySilentFailure("cnode", "gate 後併發的沉默失敗", finished.Add(time.Hour))
		writerDone <- err
	}()
	<-writerStarted
	select {
	case err := <-writerDone:
		t.Fatalf("failure writer 穿過 gate 與 job insert：%v", err)
	case <-time.After(250 * time.Millisecond):
		// 還在等第一個 Store 的 BEGIN IMMEDIATE writer reservation 才對。
	}
	close(releaseGate)
	select {
	case got := <-openDone:
		if got.err != nil || len(got.jobs) != 1 || got.jobs[0].MachineID != "cnode" {
			t.Fatalf("open batch jobs=%+v err=%v", got.jobs, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stable batch 在 release 後仍未完成")
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("failure writer 在 stable batch commit 後仍失敗：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failure writer 在 stable batch commit 後仍被擋住")
	}
	var jobs int
	if err := first.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE desired_id=?`, d.DesiredID).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("stable deployment jobs=%d err=%v", jobs, err)
	}
}
