package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
)

const rolloutTestOpenClawSpec = `{"kind":"openclaw","version":"2026.9.2"}`

// rolloutStore models a Hub whose evidence recorder was already active before
// every dated deployment fixture in this package. Never use wall clock here:
// the earliest canary finishes at a fixed 2026 timestamp, so time.Now made the
// suite start failing merely when the real calendar crossed that fixture.
var rolloutTestEvidenceEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func rolloutStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetExpectations(&expect.Set{})
	if err := s.PublishExpectationsPolicy(rolloutTestEvidenceEpoch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func addRolloutMachine(t *testing.T, s *Store, id, name string, observed bool) {
	t.Helper()
	if err := s.UpsertMachine(Machine{MachineID: id, DisplayName: name, Expected: true}); err != nil {
		t.Fatal(err)
	}
	// Deployment tests start from a canary preview snapshot unless they explicitly
	// move the machine elsewhere. Production channel assignment still goes through
	// SetMachineChannel; this fixture also needs to support never-observed machines.
	if _, err := s.DB().Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if observed {
		_, err := s.DB().Exec(`INSERT INTO observed_state
 (observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
 VALUES (?,?,?,?,?,?,?,?)`, "obs-"+id, id, "2026-09-06T12:00:00Z", "2026-09-06T12:00:00Z", "identity", "identity", `{}`, "agent_measurement")
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestMachineChannelRequiresObservationAndValidValue(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "sampleagent1", "sampleagent1", false)
	if err := s.SetMachineChannel("sampleagent1", "stable"); !errors.Is(err, ErrNeverObserved) || !strings.Contains(err.Error(), "從沒回報過") || !strings.Contains(err.Error(), "沒有 agent 會來領單") {
		t.Fatalf("未觀測機器錯誤不清楚：%v", err)
	}
	if err := s.SetMachineChannel("sampleagent1", "beta"); !errors.Is(err, ErrBadChannel) {
		t.Fatalf("非法 channel = %v", err)
	}
	addRolloutMachine(t, s, "samplehub1", "samplehub1", true)
	if err := s.SetMachineChannel("samplehub1", "canary"); err != nil {
		t.Fatal(err)
	}
	m, _ := s.GetMachine("samplehub1")
	if m.Channel != "canary" || m.ChannelRevision != 1 {
		t.Fatalf("same-value channel assignment changed revision: %+v", m)
	}
	if err := s.SetMachineChannel("samplehub1", ""); err != nil {
		t.Fatal(err)
	}
	m, _ = s.GetMachine("samplehub1")
	if m.Channel != "" || m.ChannelRevision != 2 {
		t.Fatalf("清除後 machine=%+v", m)
	}
}

func TestMachineChannelChangeRejectsNonterminalJobButAllowsNoop(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "channel-lock-test", Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
	})
	if err != nil || d.DeploymentID == "" || len(jobs) != 1 {
		t.Fatalf("create canary deployment: deployment=%+v jobs=%+v err=%v", d, jobs, err)
	}

	// Re-posting the current value is idempotent and must not be blocked by the
	// job it already owns. Only an actual authority-boundary change is unsafe.
	if err := s.SetMachineChannel("cnode", "canary"); err != nil {
		t.Fatalf("same-value channel no-op was blocked: %v", err)
	}
	if err := s.SetMachineChannel("cnode", "stable"); !errors.Is(err, ErrMachineActiveJob) {
		t.Fatalf("active canary job did not block canary→stable: %v", err)
	}
	m, err := s.GetMachine("cnode")
	if err != nil || m.Channel != "canary" {
		t.Fatalf("rejected channel move changed registry: machine=%+v err=%v", m, err)
	}

	setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
	if err := s.SetMachineChannel("cnode", "stable"); err != nil {
		t.Fatalf("terminal job still blocked channel move: %v", err)
	}
	m, err = s.GetMachine("cnode")
	if err != nil || m.Channel != "stable" {
		t.Fatalf("completed channel move: machine=%+v err=%v", m, err)
	}
}

func TestMachineChannelMoveBeforeDeploymentCreateInvalidatesSnapshot(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	if err := s.SetMachineChannel("cnode", "stable"); err != nil {
		t.Fatal(err)
	}
	before := snapshotRetryLedger(t, s)
	_, _, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "stale-channel-snapshot", Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
	})
	if !errors.Is(err, ErrDeploymentTargetChannelChanged) {
		t.Fatalf("move→create stale canary snapshot err=%v", err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("move→create wrote ledger: before=%+v after=%+v", before, after)
	}
}

func TestMachineChannelMoveRacesDeploymentCreateFailClosedAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	creator, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer creator.Close()
	mover, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer mover.Close()
	addRolloutMachine(t, creator, "cnode", "samplehub1", true)

	type result struct {
		op  string
		err error
	}
	start := make(chan struct{})
	done := make(chan result, 2)
	go func() {
		<-start
		_, _, err := creator.CreateDeployment(NewDeployment{
			Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
			BatchSize: 1, CreatedBy: "channel-race", Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
		})
		done <- result{op: "create", err: err}
	}()
	go func() {
		<-start
		done <- result{op: "move", err: mover.SetMachineChannel("cnode", "stable")}
	}()
	close(start)
	results := []result{<-done, <-done}

	successes := 0
	for _, got := range results {
		if got.err == nil {
			successes++
			continue
		}
		if got.op == "create" && !errors.Is(got.err, ErrDeploymentTargetChannelChanged) {
			t.Fatalf("create race loser err=%v; results=%+v", got.err, results)
		}
		if got.op == "move" && !errors.Is(got.err, ErrMachineActiveJob) {
			t.Fatalf("move race loser err=%v; results=%+v", got.err, results)
		}
	}
	if successes != 1 {
		t.Fatalf("create/channel race successes=%d; results=%+v", successes, results)
	}
	var jobs int
	if err := creator.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	m, err := creator.GetMachine("cnode")
	if err != nil {
		t.Fatal(err)
	}
	if (jobs == 1 && m.Channel != "canary") || (jobs == 0 && m.Channel != "stable") {
		t.Fatalf("race committed unsafe mixed state: jobs=%d channel=%q results=%+v", jobs, m.Channel, results)
	}
}

func TestOpenAddsChannelColumnToExistingRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an old schema, which predates both the column and the trigger
	// that references it. SQLite correctly refuses to leave a broken trigger.
	if _, err := s.DB().Exec(`DROP TRIGGER ` + machineChannelRevisionTrigger); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`ALTER TABLE machine_registry DROP COLUMN channel`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("舊 DB 補 channel 失敗：%v", err)
	}
	defer s.Close()
	have, err := columnSet(s.DB(), "machine_registry")
	if err != nil || !have["channel"] {
		t.Fatalf("channel 欄沒有補回：have=%v err=%v", have, err)
	}
	var triggerCount int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?`,
		machineChannelRevisionTrigger).Scan(&triggerCount); err != nil || triggerCount != 1 {
		t.Fatalf("channel revision trigger 沒有重建：count=%d err=%v", triggerCount, err)
	}
}

func TestOpenAddsChannelRevisionToExistingRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP TRIGGER ` + machineChannelRevisionTrigger); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`ALTER TABLE machine_registry DROP COLUMN channel_revision`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("舊 DB 補 channel_revision 失敗：%v", err)
	}
	defer s.Close()
	have, err := columnSet(s.DB(), "machine_registry")
	if err != nil || !have["channel_revision"] {
		t.Fatalf("channel_revision 欄沒有補回：have=%v err=%v", have, err)
	}
	var triggerCount int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?`,
		machineChannelRevisionTrigger).Scan(&triggerCount); err != nil || triggerCount != 1 {
		t.Fatalf("channel revision trigger 沒有重建：count=%d err=%v", triggerCount, err)
	}
	var revision int64
	if err := s.DB().QueryRow(`SELECT channel_revision FROM machine_registry LIMIT 1`).Scan(&revision); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("channel_revision 不能讀：%v", err)
	}
}

func TestMachinesInChannelExcludesRetired(t *testing.T) {
	s := rolloutStore(t)
	for _, id := range []string{"a", "b"} {
		addRolloutMachine(t, s, id, id, true)
		if err := s.SetMachineChannel(id, "stable"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RetireMachine("b", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.MachinesInChannel("stable")
	if err != nil || len(got) != 1 || got[0].MachineID != "a" {
		t.Fatalf("members=%+v err=%v", got, err)
	}
}

func TestPlanChannelDeploymentUsesOverviewJudgementAndSeparatesUnknownNode(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	ids := map[string]string{}
	for _, name := range []string{"eligible", "conflict", "missing", "unknown", "unreachable"} {
		id := "machine-" + name
		ids[name] = id
		addRolloutMachine(t, s, id, name, false)
		node := "24.15.0"
		if name == "missing" {
			node = "22.22.2"
		}
		if name == "unknown" {
			node = ""
		}
		at := now
		if name == "unreachable" {
			at = now.Add(-10 * time.Minute)
		}
		if err := s.RecordCheckin(id, model.Checkin{SentAt: at, AgentStartedAt: at}, at); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordObservation(id, model.ObservationBatch{
			MeasuredAt: at, OpenClaw: model.OpenClaw{Install: &model.OpenClawInstall{NodeVersion: node}},
		}, at); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMachineChannel(id, "stable"); err != nil {
			t.Fatal(err)
		}
	}
	desired, revision, err := s.CreateDesiredState("machine", ids["conflict"], "openclaw", "openclaw", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob(ids["conflict"], desired, revision, NewJob{}); err != nil {
		t.Fatal(err)
	}

	plan, members, err := s.PlanChannelDeployment("stable", ">=22.22.3 <23 || >=24.15.0 <25", 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 5 {
		t.Fatalf("members=%d", len(members))
	}
	if got := plan.Summary(); got != "影響 2 台，衝突 1，缺套件 1，unreachable 1，node 版本未知 1" {
		t.Fatalf("summary=%q targets=%+v", got, plan.Targets)
	}
}

func TestCreateDeploymentIsAtomicAndOnlyOpensFirstBatch(t *testing.T) {
	s := rolloutStore(t)
	for _, id := range []string{"a", "b", "excluded"} {
		addRolloutMachine(t, s, id, id, true)
	}
	digest := strings.Repeat("a", 64)
	n := NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec: `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`, BatchSize: 1, CreatedBy: "test",
		Job: NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
		Targets: []NewDeploymentTarget{
			{MachineID: "a", BatchNo: 1}, {MachineID: "b", BatchNo: 2},
			{MachineID: "excluded", ExcludedReason: "missing_package"},
		},
	}
	d, jobs, err := s.CreateDeployment(n)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != DeploymentRunning || d.Revision != 1 || len(jobs) != 1 || jobs[0].MachineID != "a" {
		t.Fatalf("deployment=%+v jobs=%+v", d, jobs)
	}
	ds, err := s.DesiredState(d.DesiredID)
	if err != nil || ds.ScopeType != "channel" || ds.ScopeID != "canary" {
		t.Fatalf("desired=%+v err=%v", ds, err)
	}
	targets, err := s.DeploymentTargets(d.DeploymentID)
	if err != nil || len(targets) != 3 || targets[0].JobID == "" || targets[1].JobID != "" || targets[2].BatchNo != 0 {
		t.Fatalf("targets=%+v err=%v", targets, err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, fmtTime(time.Now().UTC()), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}

	first, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now())
	if err != nil || len(first) != 1 || first[0].MachineID != "b" {
		t.Fatalf("open second=%+v err=%v", first, err)
	}
	second, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now())
	if err != nil || len(second) != 1 || second[0].JobID != first[0].JobID {
		t.Fatalf("冪等重跑另開了單：first=%+v second=%+v err=%v", first, second, err)
	}
}

func createBatchGuardFixture(t *testing.T, s *Store, batches int) (Deployment, []Job) {
	t.Helper()
	digest := strings.Repeat("5", 64)
	targets := make([]NewDeploymentTarget, 0, batches)
	for batch := 1; batch <= batches; batch++ {
		machineID := fmt.Sprintf("machine-%d", batch)
		addRolloutMachine(t, s, machineID, machineID, true)
		targets = append(targets, NewDeploymentTarget{MachineID: machineID, BatchNo: batch})
	}
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		BatchSize: 1, CreatedBy: "batch-guard", Targets: targets,
		Job: NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, jobs
}

func relabelDeploymentStable(t *testing.T, s *Store, d Deployment) {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE deployments SET channel='stable' WHERE deployment_id=?`, d.DeploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE desired_state SET scope_id='stable' WHERE desired_id=?`, d.DesiredID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func setRolloutJobTerminal(t *testing.T, s *Store, job Job, jobState deploy.JobState) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		jobState, fmtTime(time.Now().UTC()), job.JobID); err != nil {
		t.Fatal(err)
	}
}

func attachLegacyDeploymentJob(t *testing.T, s *Store, d Deployment, machineID string, state deploy.JobState) Job {
	t.Helper()
	now := time.Now().UTC()
	template, err := s.DeploymentJobTemplate(d.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	job := Job{
		JobID: newID(), MachineID: machineID, DesiredID: d.DesiredID, Revision: d.Revision,
		State: state, CreatedAt: now, ArtifactDigest: template.ArtifactDigest, ExecutionTimeout: 600,
	}
	var terminalAt any
	if deploy.IsTerminal(state) {
		terminalAt = fmtTime(now)
		finished := now
		job.TerminalAt = &finished
	}
	if _, err := s.DB().Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,artifact_digest,irreversible,execution_timeout,terminal_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, job.JobID, job.MachineID, job.DesiredID, job.Revision,
		job.State, fmtTime(job.CreatedAt), job.ArtifactDigest, false, job.ExecutionTimeout, terminalAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE deployment_targets SET job_id=?
 WHERE deployment_id=? AND machine_id=?`, job.JobID, d.DeploymentID, machineID); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestOpenDeploymentBatchOnlyOpensReadyNextBatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		batches   int
		requested int
		prior     deploy.JobState
	}{
		{"cannot skip a batch", 3, 3, deploy.Succeeded},
		{"prior batch still running", 2, 2, deploy.Running},
		{"prior batch failed", 2, 2, deploy.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			d, jobs := createBatchGuardFixture(t, s, tc.batches)
			if tc.prior == deploy.Succeeded || tc.prior == deploy.Failed {
				setRolloutJobTerminal(t, s, jobs[0], tc.prior)
			} else if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=NULL WHERE job_id=?`, tc.prior, jobs[0].JobID); err != nil {
				t.Fatal(err)
			}
			before := snapshotRetryLedger(t, s)
			if _, err := s.OpenDeploymentBatch(d.DeploymentID, tc.requested, time.Now().UTC()); !errors.Is(err, ErrDeploymentBatchNotReady) {
				t.Fatalf("case=%s err=%v", tc.name, err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("not-ready batch 寫了 ledger：before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestOpenDeploymentBatchRefusesAPausedDeployment(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		pause                    bool
		wantErr                  error
		wantNewJobs              int
		wantControlRevisionDelta int
		errConsequence           string
		jobsConsequence          string
		revisionConsequence      string
	}{
		{
			name:                     "沒暫停時第二批開得起來",
			pause:                    false,
			wantErr:                  nil,
			wantNewJobs:              1,
			wantControlRevisionDelta: 1,
			errConsequence:           "對照組在沒暫停時就開不起來，代表這個 fixture 的第二批本來就還沒準備好，暫停那一列的拒絕證明不了任何事",
			jobsConsequence:          "對照組沒有開出新工作單，代表這個 fixture 根本推不動批次，暫停那一列的「沒有新單」是假的證據",
			revisionConsequence:      "對照組的 control_revision 沒有前進，代表這個 fixture 沒有真的開過一批，暫停那一列的「沒有前進」是假的證據",
		},
		{
			name:                     "暫停之後第二批不准開",
			pause:                    true,
			wantErr:                  ErrDeploymentNotFound,
			wantNewJobs:              0,
			wantControlRevisionDelta: 0,
			errConsequence:           "回的不是 ErrDeploymentNotFound，操作員不會去看是不是被暫停了；他會把這個 ID 當成已經消失，去重開一筆部署或追查誰刪了列",
			jobsConsequence:          "暫停之後仍然開出新工作單，操作員以為已經喊停，agent 卻還會把它收去改機器，他會對一個已喊停的目標繼續推下一版",
			revisionConsequence:      "暫停之後 control_revision 仍然前進，操作員按恢復時會撞上對不上的版本，他會當成有別人在搶這筆部署的控制權",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			d, jobs := createBatchGuardFixture(t, s, 2)
			setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
			if tc.pause {
				changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				if !changed {
					t.Fatal("fixture 沒有真的把部署暫停")
				}
			}
			jobsBefore := countRows(t, s, `SELECT COUNT(*) FROM jobs`)
			revisionBefore := countRows(t, s, `SELECT control_revision FROM deployments WHERE deployment_id=?`, d.DeploymentID)

			_, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())

			jobsAfter := countRows(t, s, `SELECT COUNT(*) FROM jobs`)
			revisionAfter := countRows(t, s, `SELECT control_revision FROM deployments WHERE deployment_id=?`, d.DeploymentID)

			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s 的錯誤是 %v，預期 %v —— %s", tc.name, err, tc.wantErr, tc.errConsequence)
			}
			if got := jobsAfter - jobsBefore; got != tc.wantNewJobs {
				t.Errorf("%s 多出了 %d 張工作單，預期 %d —— %s", tc.name, got, tc.wantNewJobs, tc.jobsConsequence)
			}
			if got := revisionAfter - revisionBefore; got != tc.wantControlRevisionDelta {
				t.Errorf("%s 的 control_revision 前進了 %d，預期 %d —— %s", tc.name, got, tc.wantControlRevisionDelta, tc.revisionConsequence)
			}
		})
	}
}

func TestOpenDeploymentBatchRejectsLegacyEarlierBatchThatDidNotSucceed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state deploy.JobState
	}{
		{name: "earlier batch active", state: deploy.NotStarted},
		{name: "earlier batch failed", state: deploy.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			d, first := createBatchGuardFixture(t, s, 3)
			if tc.state == deploy.Failed {
				setRolloutJobTerminal(t, s, first[0], deploy.Failed)
			}
			attachLegacyDeploymentJob(t, s, d, "machine-2", deploy.Succeeded)

			before := snapshotRetryLedger(t, s)
			opened, err := s.OpenDeploymentBatch(d.DeploymentID, 3, time.Now().UTC())
			if len(opened) != 0 || !errors.Is(err, ErrDeploymentBatchNotReady) {
				t.Fatalf("legacy earlier %s still opened batch 3: jobs=%+v err=%v", tc.state, opened, err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("rejected legacy prefix wrote ledger: before=%+v after=%+v", before, after)
			}
			view, viewErr := s.DeploymentView(d.DeploymentID, time.Now().UTC())
			if viewErr != nil || view.State != DeploymentRunning || view.OpenedBatch != 2 {
				t.Fatalf("rejected legacy prefix changed deployment: view=%+v err=%v", view, viewErr)
			}
		})
	}
}

func TestOpenDeploymentBatchRejectsDeferredTargetChannelDriftWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name          string
		from, to      string
		makeFixture   func(*testing.T, *Store) (Deployment, []Job)
		changedTarget string
	}{
		{
			name: "canary target moved to stable", from: "canary", to: "stable",
			makeFixture: func(t *testing.T, s *Store) (Deployment, []Job) {
				return createBatchGuardFixture(t, s, 2)
			},
			changedTarget: "machine-2",
		},
		{
			name: "stable target moved to canary", from: "stable", to: "canary",
			makeFixture: func(t *testing.T, s *Store) (Deployment, []Job) {
				d, jobs, _ := eligibleTwoBatchStableFixture(t, s)
				return d, jobs
			},
			changedTarget: "cnode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			d, jobs := tc.makeFixture(t, s)
			if len(jobs) != 1 {
				t.Fatalf("first batch jobs=%+v", jobs)
			}
			// The canary fixture still needs its first batch made terminal. The
			// stable fixture already does this while constructing eligible evidence.
			if tc.from == "canary" {
				setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
			}
			if err := s.SetMachineChannel(tc.changedTarget, tc.to); err != nil {
				t.Fatal(err)
			}
			before := snapshotRetryLedger(t, s)
			opened, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
			if len(opened) != 0 || !errors.Is(err, ErrDeploymentTargetChannelChanged) {
				t.Fatalf("%s→%s drift opened=%+v err=%v", tc.from, tc.to, opened, err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("channel-drift batch 寫了 ledger：before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestOpenDeploymentBatchRejectsLegacyOpenedBatchGap(t *testing.T) {
	s := rolloutStore(t)
	d, _ := createBatchGuardFixture(t, s, 4)
	jobID := newID()
	now := time.Now().UTC()
	if _, err := s.DB().Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,irreversible,execution_timeout,terminal_at)
 VALUES (?,?,?,?,?,?,?,?,?)`, jobID, "machine-3", d.DesiredID, d.Revision, deploy.Succeeded,
		fmtTime(now), false, 600, fmtTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE deployment_targets SET job_id=?
 WHERE deployment_id=? AND machine_id='machine-3'`, jobID, d.DeploymentID); err != nil {
		t.Fatal(err)
	}
	before := snapshotRetryLedger(t, s)
	if _, err := s.OpenDeploymentBatch(d.DeploymentID, 4, now); !errors.Is(err, ErrDeploymentBatchNotReady) {
		t.Fatalf("legacy batch 2 空洞仍開 batch 4：%v", err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("legacy gap 寫了 ledger：before=%+v after=%+v", before, after)
	}
}

func TestOpenDeploymentBatchAlreadyOpenedIsPureIdempotentRead(t *testing.T) {
	s := rolloutStore(t)
	d, jobs := createBatchGuardFixture(t, s, 2)
	setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
	opened, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
	if err != nil || len(opened) != 1 {
		t.Fatalf("first open jobs=%+v err=%v", opened, err)
	}
	relabelDeploymentStable(t, s, d)
	before := snapshotRetryLedger(t, s)
	again, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
	if err != nil || len(again) != 1 || again[0].JobID != opened[0].JobID {
		t.Fatalf("stable gate 後來鎖住卻破壞已開批冪等讀：first=%+v again=%+v err=%v", opened, again, err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("冪等 batch 重送寫了 ledger：before=%+v after=%+v", before, after)
	}
}

func TestStableOpenAndContinueRecheckPromoteGateBeforeNewJobs(t *testing.T) {
	t.Run("OpenDeploymentBatch", func(t *testing.T) {
		s := rolloutStore(t)
		d, jobs := createBatchGuardFixture(t, s, 2)
		setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
		relabelDeploymentStable(t, s, d)
		before := snapshotRetryLedger(t, s)
		_, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
		if !errors.Is(err, ErrPromoteLocked) || !strings.Contains(err.Error(), "promote 鎖著") {
			t.Fatalf("legacy stable OpenBatch 沒有人話 gate error：%v", err)
		}
		if after := snapshotRetryLedger(t, s); after != before {
			t.Fatalf("locked stable OpenBatch 寫了 job：before=%+v after=%+v", before, after)
		}
	})

	t.Run("ContinueDeployment", func(t *testing.T) {
		s := rolloutStore(t)
		d, jobs := createBatchGuardFixture(t, s, 2)
		setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
		if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
			t.Fatalf("pause changed=%v err=%v", changed, err)
		}
		relabelDeploymentStable(t, s, d)
		before := snapshotRetryLedger(t, s)
		_, _, err := s.ContinueDeployment(d.DeploymentID, time.Now().UTC())
		if !errors.Is(err, ErrPromoteLocked) || !strings.Contains(err.Error(), "promote 鎖著") {
			t.Fatalf("legacy stable Continue 沒有人話 gate error：%v", err)
		}
		if after := snapshotRetryLedger(t, s); after != before {
			t.Fatalf("locked stable Continue 寫了 job：before=%+v after=%+v", before, after)
		}
	})
}

func TestStableContinueMayOnlyCloseAnAlreadyCompleteLedger(t *testing.T) {
	s := rolloutStore(t)
	d, jobs := createBatchGuardFixture(t, s, 1)
	setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause changed=%v err=%v", changed, err)
	}
	relabelDeploymentStable(t, s, d)
	beforeJobs := snapshotRetryLedger(t, s).jobs
	continued, opened, err := s.ContinueDeployment(d.DeploymentID, time.Now().UTC())
	if err != nil || continued.State != DeploymentFinished || len(opened) != 0 {
		t.Fatalf("pure stable close continued=%+v opened=%+v err=%v", continued, opened, err)
	}
	if afterJobs := snapshotRetryLedger(t, s).jobs; afterJobs != beforeJobs {
		t.Fatalf("pure stable close 新增 jobs：before=%d after=%d", beforeJobs, afterJobs)
	}
}

func TestContinueRejectsNonTerminalCurrentBatchWithoutWriting(t *testing.T) {
	s := rolloutStore(t)
	d, _ := createBatchGuardFixture(t, s, 2)
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause changed=%v err=%v", changed, err)
	}
	before := snapshotRetryLedger(t, s)
	if _, _, err := s.ContinueDeployment(d.DeploymentID, time.Now().UTC()); !errors.Is(err, ErrDeploymentBatchNotReady) {
		t.Fatalf("nonterminal batch Continue err=%v", err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("nonterminal Continue 寫了 ledger：before=%+v after=%+v", before, after)
	}
	got, err := s.Deployment(d.DeploymentID)
	if err != nil || got.State != DeploymentPaused {
		t.Fatalf("rejected Continue state=%q err=%v", got.State, err)
	}
}

func TestContinueRejectsLegacyEarlierNonTerminalBatchWithoutWriting(t *testing.T) {
	for _, batches := range []int{2, 3} {
		t.Run(fmt.Sprintf("total batches %d", batches), func(t *testing.T) {
			s := rolloutStore(t)
			d, _ := createBatchGuardFixture(t, s, batches)
			attachLegacyDeploymentJob(t, s, d, "machine-2", deploy.Succeeded)
			if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
				t.Fatalf("pause legacy deployment changed=%v err=%v", changed, err)
			}

			before := snapshotRetryLedger(t, s)
			continued, opened, err := s.ContinueDeployment(d.DeploymentID, time.Now().UTC())
			if len(opened) != 0 || !errors.Is(err, ErrDeploymentBatchNotReady) {
				t.Fatalf("legacy active earlier batch continued=%+v jobs=%+v err=%v", continued, opened, err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("rejected legacy Continue wrote ledger: before=%+v after=%+v", before, after)
			}
			view, viewErr := s.DeploymentView(d.DeploymentID, time.Now().UTC())
			if viewErr != nil || view.State != DeploymentPaused || view.OpenedBatch != 2 {
				t.Fatalf("rejected legacy Continue changed deployment: view=%+v err=%v", view, viewErr)
			}
		})
	}
}

func TestSetDeploymentStateRejectsNonDriverTransitionsWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name, current, from, to string
	}{
		{"running to running", DeploymentRunning, DeploymentRunning, DeploymentRunning},
		{"paused to running", DeploymentPaused, DeploymentPaused, DeploymentRunning},
		{"paused to finished", DeploymentPaused, DeploymentPaused, DeploymentFinished},
		{"finished to running", DeploymentFinished, DeploymentFinished, DeploymentRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			d, _ := createBatchGuardFixture(t, s, 1)
			if tc.current != DeploymentRunning {
				if _, err := s.DB().Exec(`UPDATE deployments SET state=? WHERE deployment_id=?`, tc.current, d.DeploymentID); err != nil {
					t.Fatal(err)
				}
			}
			var beforeState string
			var beforePaused, beforeFinished sql.NullString
			if err := s.DB().QueryRow(`SELECT state,paused_at,finished_at FROM deployments WHERE deployment_id=?`, d.DeploymentID).
				Scan(&beforeState, &beforePaused, &beforeFinished); err != nil {
				t.Fatal(err)
			}
			changed, err := s.SetDeploymentState(d.DeploymentID, tc.from, tc.to, time.Now().UTC())
			if changed || !errors.Is(err, ErrDeploymentBadTransition) {
				t.Fatalf("transition %s->%s changed=%v err=%v", tc.from, tc.to, changed, err)
			}
			var afterState string
			var afterPaused, afterFinished sql.NullString
			if err := s.DB().QueryRow(`SELECT state,paused_at,finished_at FROM deployments WHERE deployment_id=?`, d.DeploymentID).
				Scan(&afterState, &afterPaused, &afterFinished); err != nil {
				t.Fatal(err)
			}
			if afterState != beforeState || afterPaused != beforePaused || afterFinished != beforeFinished {
				t.Fatalf("rejected transition 寫了 state：before=%q/%v/%v after=%q/%v/%v",
					beforeState, beforePaused, beforeFinished, afterState, afterPaused, afterFinished)
			}
		})
	}
}

func requireDeploymentSoakBoundary(t *testing.T, s *Store, deploymentID string) int64 {
	t.Helper()
	var evidenceID int64
	if err := s.DB().QueryRow(`SELECT evidence_id FROM deployment_soak_boundaries WHERE deployment_id=?`,
		deploymentID).Scan(&evidenceID); err != nil {
		t.Fatalf("deployment %s soak boundary: %v", deploymentID, err)
	}
	if evidenceID < 0 {
		t.Fatalf("deployment %s soak boundary=%d", deploymentID, evidenceID)
	}
	return evidenceID
}

func TestEveryFinishedTransitionCapturesSoakBoundary(t *testing.T) {
	t.Run("driver finish", func(t *testing.T) {
		s := rolloutStore(t)
		d, jobs := createBatchGuardFixture(t, s, 1)
		setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
		if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentFinished, time.Now().UTC()); err != nil || !changed {
			t.Fatalf("finish changed=%v err=%v", changed, err)
		}
		requireDeploymentSoakBoundary(t, s, d.DeploymentID)
	})

	t.Run("continue finish", func(t *testing.T) {
		s := rolloutStore(t)
		d, jobs := createBatchGuardFixture(t, s, 1)
		setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
		if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
			t.Fatalf("pause changed=%v err=%v", changed, err)
		}
		if continued, opened, err := s.ContinueDeployment(d.DeploymentID, time.Now().UTC()); err != nil ||
			continued.State != DeploymentFinished || len(opened) != 0 {
			t.Fatalf("continue finish=%+v jobs=%+v err=%v", continued, opened, err)
		}
		requireDeploymentSoakBoundary(t, s, d.DeploymentID)
	})

	t.Run("abandon", func(t *testing.T) {
		s := rolloutStore(t)
		d, jobs := createBatchGuardFixture(t, s, 1)
		setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
		if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
			t.Fatalf("pause changed=%v err=%v", changed, err)
		}
		if abandoned, err := s.AbandonDeployment(d.DeploymentID, time.Now().UTC()); err != nil || abandoned.State != DeploymentFinished {
			t.Fatalf("abandon=%+v err=%v", abandoned, err)
		}
		requireDeploymentSoakBoundary(t, s, d.DeploymentID)
	})

	t.Run("retry finishes paused parent", func(t *testing.T) {
		s := rolloutStore(t)
		parent, jobs := createBatchGuardFixture(t, s, 1)
		setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
		if changed, err := s.SetDeploymentState(parent.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
			t.Fatalf("pause changed=%v err=%v", changed, err)
		}
		_, _, err := s.CreateDeployment(NewDeployment{
			Channel: parent.Channel, ResourceKind: parent.ResourceKind, ResourceID: parent.ResourceID,
			Spec: parent.Spec, BatchSize: 1, CreatedBy: "soak-boundary-test", RetryOf: parent.DeploymentID,
			Targets: []NewDeploymentTarget{{MachineID: "machine-1", BatchNo: 1}},
			Job:     NewJob{ArtifactDigest: "sha256:" + strings.Repeat("5", 64)},
		})
		if err != nil {
			t.Fatal(err)
		}
		requireDeploymentSoakBoundary(t, s, parent.DeploymentID)
	})
}

func TestFinishedTransitionRollsBackWhenSoakBoundaryCannotCommit(t *testing.T) {
	s := rolloutStore(t)
	d, jobs := createBatchGuardFixture(t, s, 1)
	setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
	if _, err := s.DB().Exec(`DROP TABLE deployment_soak_boundaries`); err != nil {
		t.Fatal(err)
	}
	changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentFinished, time.Now().UTC())
	if changed || err == nil || !strings.Contains(err.Error(), "soak evidence boundary") {
		t.Fatalf("finish without atomic boundary changed=%v err=%v", changed, err)
	}
	got, getErr := s.Deployment(d.DeploymentID)
	if getErr != nil || got.State != DeploymentRunning || got.FinishedAt != nil {
		t.Fatalf("failed boundary left partial finish: deployment=%+v err=%v", got, getErr)
	}
}

func TestCreateDeploymentRollbackIncludesDesiredState(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "a", "a", true)
	_, _, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 5, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "missing", BatchNo: 1}},
	})
	if err == nil {
		t.Fatal("不存在 target 應讓整個交易失敗")
	}
	for _, table := range []string{"desired_state", "deployments", "deployment_targets", "jobs", "revision_counters"} {
		var n int
		if qerr := s.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); qerr != nil || n != 0 {
			t.Errorf("%s 留下 %d 列，err=%v", table, n, qerr)
		}
	}
}

func TestCreateDeploymentRejectsTargetsOutsideSnapshotChannelWithoutWriting(t *testing.T) {
	t.Run("stable machine labeled canary", func(t *testing.T) {
		s := rolloutStore(t)
		addRolloutMachine(t, s, "stable-box", "stable-box", true)
		if err := s.SetMachineChannel("stable-box", "stable"); err != nil {
			t.Fatal(err)
		}
		n := NewDeployment{
			Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
			BatchSize: 1, CreatedBy: "channel-invariant", Targets: []NewDeploymentTarget{{MachineID: "stable-box", BatchNo: 1}},
		}
		before := snapshotRetryLedger(t, s)
		if _, _, err := s.CreateDeployment(n); !errors.Is(err, ErrDeploymentTargetChannelChanged) {
			t.Fatalf("stable target 冒充 canary err=%v", err)
		}
		if after := snapshotRetryLedger(t, s); after != before {
			t.Fatalf("channel mismatch 寫了 ledger：before=%+v after=%+v", before, after)
		}
	})

	t.Run("cross-store preview becomes stale", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.db")
		planner, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer planner.Close()
		writer, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		addRolloutMachine(t, planner, "cnode", "samplehub1", true)
		_, members, err := planner.PlanChannelDeployment("canary", ">=24 <25", 1, time.Now().UTC())
		if err != nil || len(members) != 1 {
			t.Fatalf("preview members=%+v err=%v", members, err)
		}
		if err := writer.SetMachineChannel("cnode", "stable"); err != nil {
			t.Fatal(err)
		}
		n := NewDeployment{
			Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
			BatchSize: 1, CreatedBy: "stale-preview", Targets: []NewDeploymentTarget{{MachineID: members[0].MachineID, BatchNo: 1}},
		}
		before := snapshotRetryLedger(t, planner)
		if _, _, err := planner.CreateDeployment(n); !errors.Is(err, ErrDeploymentTargetChannelChanged) {
			t.Fatalf("stale cross-store plan err=%v", err)
		}
		if after := snapshotRetryLedger(t, planner); after != before {
			t.Fatalf("stale preview 寫了 ledger：before=%+v after=%+v", before, after)
		}
	})
}

func TestCreateDeploymentRequiresCompleteInitialChannelSnapshotWithoutWriting(t *testing.T) {
	s := rolloutStore(t)
	for _, id := range []string{"easy", "hard"} {
		addRolloutMachine(t, s, id, id, true)
	}
	n := NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "partial-snapshot", Targets: []NewDeploymentTarget{{MachineID: "easy", BatchNo: 1}},
	}
	before := snapshotRetryLedger(t, s)
	if _, _, err := s.CreateDeployment(n); !errors.Is(err, ErrDeploymentTargetChannelChanged) {
		t.Fatalf("漏掉 hard canary 的 partial snapshot err=%v", err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("partial snapshot 寫了 ledger：before=%+v after=%+v", before, after)
	}
}

func TestCreateDeploymentRejectsDuplicateExtraAndEmptyInitialSnapshotsWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targets []NewDeploymentTarget
	}{
		{"duplicate", []NewDeploymentTarget{{MachineID: "easy", BatchNo: 1}, {MachineID: "hard", BatchNo: 1}, {MachineID: "easy", BatchNo: 1}}},
		{"extra", []NewDeploymentTarget{{MachineID: "easy", BatchNo: 1}, {MachineID: "hard", BatchNo: 1}, {MachineID: "outsider", BatchNo: 1}}},
		{"empty", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			for _, id := range []string{"easy", "hard", "outsider"} {
				addRolloutMachine(t, s, id, id, true)
			}
			if err := s.SetMachineChannel("outsider", "stable"); err != nil {
				t.Fatal(err)
			}
			n := NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
				BatchSize: 3, CreatedBy: "bad-snapshot", Targets: tc.targets,
			}
			before := snapshotRetryLedger(t, s)
			_, _, err := s.CreateDeployment(n)
			if err == nil || (tc.name != "empty" && !errors.Is(err, ErrDeploymentTargetChannelChanged)) {
				t.Fatalf("%s snapshot err=%v", tc.name, err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("%s snapshot 寫了 ledger：before=%+v after=%+v", tc.name, before, after)
			}
		})
	}
}

func TestCreateDeploymentBindsSpecKindToResourceBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name         string
		resourceKind string
		resourceID   string
		spec         string
	}{
		{"OpenClaw spec under app resource", "app", "x", rolloutTestOpenClawSpec},
		{"noop spec under OpenClaw resource", "openclaw", "openclaw", `{"kind":"noop"}`},
		{"malformed spec under OpenClaw resource", "openclaw", "openclaw", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", true)
			n := NewDeployment{
				Channel: "canary", ResourceKind: tc.resourceKind, ResourceID: tc.resourceID, Spec: tc.spec,
				BatchSize: 1, CreatedBy: "material-invariant", Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
			}
			before := snapshotRetryLedger(t, s)
			if _, _, err := s.CreateDeployment(n); !errors.Is(err, ErrDeploymentMaterialMismatch) {
				t.Fatalf("mislabeled material err=%v", err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("material mismatch 寫了 ledger：before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestCreateDeploymentBindsOpenClawSpecArtifactToJobDigestBeforeWriting(t *testing.T) {
	canonical := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	for _, tc := range []struct {
		name      string
		spec      string
		jobDigest string
	}{
		{
			name:      "different canonical digests",
			spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + canonical + `"}}`,
			jobDigest: "sha256:" + other,
		},
		{
			name:      "spec artifact without job digest",
			spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + canonical + `"}}`,
			jobDigest: "",
		},
		{
			name:      "job digest without spec artifact",
			spec:      rolloutTestOpenClawSpec,
			jobDigest: "sha256:" + canonical,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "material-target", "material-target", true)
			before := snapshotRetryLedger(t, s)
			_, _, err := s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
				Spec: tc.spec, BatchSize: 1, CreatedBy: "material-binding-test",
				Targets: []NewDeploymentTarget{{MachineID: "material-target", BatchNo: 1}},
				Job:     NewJob{ArtifactDigest: tc.jobDigest, ExecutionTimeout: 600},
			})
			if !errors.Is(err, ErrDeploymentMaterialMismatch) {
				t.Fatalf("material mismatch err=%v", err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("material mismatch wrote ledger: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestContinueDeploymentRejectsLegacyOpenClawTemplateDigestMismatchAtomically(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 8, 22, 0, 0, 0, time.UTC)
	d, jobs := createBatchGuardFixture(t, s, 2)
	setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
	if _, err := s.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`,
		"sha256:"+strings.Repeat("6", 64), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.PauseDeploymentAtBoundary(d.DeploymentID, 1,
		DeploymentPauseMaterial, "legacy template no longer matches spec", now); err != nil || !changed {
		t.Fatalf("pause changed=%t err=%v", changed, err)
	}
	beforeLedger := snapshotRetryLedger(t, s)
	beforeDeployment, err := s.Deployment(d.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	beforeView, err := s.DeploymentView(d.DeploymentID, now)
	if err != nil || beforeView.BoundaryPause == nil {
		t.Fatalf("before view=%+v err=%v", beforeView, err)
	}
	var beforeEvents int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM hub_events`).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}

	continued, opened, err := s.ContinueDeployment(d.DeploymentID, now.Add(time.Minute))
	if !errors.Is(err, ErrDeploymentMaterialMismatch) || continued.DeploymentID != "" || len(opened) != 0 {
		t.Fatalf("continued=%+v opened=%+v err=%v", continued, opened, err)
	}
	if after := snapshotRetryLedger(t, s); after != beforeLedger {
		t.Fatalf("rejected Continue changed ledger counts: before=%+v after=%+v", beforeLedger, after)
	}
	afterDeployment, err := s.Deployment(d.DeploymentID)
	if err != nil || afterDeployment.State != beforeDeployment.State ||
		afterDeployment.ControlRevision != beforeDeployment.ControlRevision ||
		(afterDeployment.PausedAt == nil) != (beforeDeployment.PausedAt == nil) || afterDeployment.FinishedAt != nil {
		t.Fatalf("rejected Continue changed deployment: before=%+v after=%+v err=%v",
			beforeDeployment, afterDeployment, err)
	}
	afterTargets, err := s.DeploymentTargets(d.DeploymentID)
	if err != nil || len(afterTargets) != 2 || afterTargets[1].JobID != "" {
		t.Fatalf("rejected Continue opened next target: targets=%+v err=%v", afterTargets, err)
	}
	afterView, err := s.DeploymentView(d.DeploymentID, now.Add(time.Minute))
	if err != nil || afterView.BoundaryPause == nil ||
		afterView.BoundaryPause.Kind != beforeView.BoundaryPause.Kind ||
		afterView.BoundaryPause.OpenedBatch != beforeView.BoundaryPause.OpenedBatch {
		t.Fatalf("rejected Continue changed boundary pause: before=%+v after=%+v err=%v",
			beforeView.BoundaryPause, afterView.BoundaryPause, err)
	}
	var afterEvents int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM hub_events`).Scan(&afterEvents); err != nil || afterEvents != beforeEvents {
		t.Fatalf("rejected Continue events=%d, want %d err=%v", afterEvents, beforeEvents, err)
	}
}

func TestBatchExpansionRejectsNonFirstPriorJobMaterialDriftAtomically(t *testing.T) {
	for _, action := range []string{"open", "continue"} {
		t.Run(action, func(t *testing.T) {
			s := rolloutStore(t)
			for _, machineID := range []string{"material-a", "material-b", "material-c"} {
				addRolloutMachine(t, s, machineID, machineID, true)
			}
			digest := strings.Repeat("5", 64)
			d, jobs, err := s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", BatchSize: 2,
				CreatedBy: "prior-material-test",
				Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
				Targets: []NewDeploymentTarget{
					{MachineID: "material-a", BatchNo: 1}, {MachineID: "material-b", BatchNo: 1},
					{MachineID: "material-c", BatchNo: 2},
				},
				Job: NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
			})
			if err != nil || len(jobs) != 2 {
				t.Fatalf("create jobs=%+v err=%v", jobs, err)
			}
			for _, job := range jobs {
				setRolloutJobTerminal(t, s, job, deploy.Succeeded)
			}
			if action == "continue" {
				if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
					t.Fatalf("pause changed=%t err=%v", changed, err)
				}
			}
			if _, err := s.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`,
				"sha256:"+strings.Repeat("6", 64), jobs[1].JobID); err != nil {
				t.Fatal(err)
			}
			before := snapshotDeploymentMutationLedger(t, s, d.DeploymentID)
			if action == "open" {
				_, err = s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
			} else {
				_, _, err = s.ContinueDeployment(d.DeploymentID, time.Now().UTC())
			}
			if !errors.Is(err, ErrDeploymentMaterialMismatch) {
				t.Fatalf("non-first material drift accepted: %v", err)
			}
			if after := snapshotDeploymentMutationLedger(t, s, d.DeploymentID); after != before {
				t.Fatalf("material rejection mutated ledger: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestBatchExpansionRejectsOtherFinishedDeploymentActiveJobAtomically(t *testing.T) {
	for _, action := range []string{"open", "continue"} {
		t.Run(action, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "hidden-old", "hidden-old", true)
			old, oldJobs, err := s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
				BatchSize: 1, CreatedBy: "hidden-old", Targets: []NewDeploymentTarget{{MachineID: "hidden-old", BatchNo: 1}},
			})
			if err != nil || len(oldJobs) != 1 {
				t.Fatalf("create old jobs=%+v err=%v", oldJobs, err)
			}
			setRolloutJobTerminal(t, s, oldJobs[0], deploy.Succeeded)
			if changed, err := s.SetDeploymentState(old.DeploymentID, DeploymentRunning, DeploymentFinished, time.Now().UTC()); err != nil || !changed {
				t.Fatalf("finish old changed=%t err=%v", changed, err)
			}

			addRolloutMachine(t, s, "current-a", "current-a", true)
			addRolloutMachine(t, s, "current-b", "current-b", true)
			current, currentJobs, err := s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
				BatchSize: 1, CreatedBy: "current", Targets: []NewDeploymentTarget{
					{MachineID: "hidden-old", BatchNo: 0, ExcludedReason: "conflict"},
					{MachineID: "current-a", BatchNo: 1}, {MachineID: "current-b", BatchNo: 2},
				},
			})
			if err != nil || len(currentJobs) != 1 {
				t.Fatalf("create current jobs=%+v err=%v", currentJobs, err)
			}
			setRolloutJobTerminal(t, s, currentJobs[0], deploy.Succeeded)
			if action == "continue" {
				if changed, err := s.SetDeploymentState(current.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
					t.Fatalf("pause current changed=%t err=%v", changed, err)
				}
			}
			if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=NULL WHERE job_id=?`,
				deploy.Running, oldJobs[0].JobID); err != nil {
				t.Fatal(err)
			}
			before := snapshotDeploymentMutationLedger(t, s, current.DeploymentID)
			if action == "open" {
				_, err = s.OpenDeploymentBatch(current.DeploymentID, 2, time.Now().UTC())
			} else {
				_, _, err = s.ContinueDeployment(current.DeploymentID, time.Now().UTC())
			}
			if !errors.Is(err, ErrDeploymentActiveResource) {
				t.Fatalf("hidden other deployment job did not block %s: %v", action, err)
			}
			if after := snapshotDeploymentMutationLedger(t, s, current.DeploymentID); after != before {
				t.Fatalf("rejected %s mutated ledger: before=%+v after=%+v", action, before, after)
			}
		})
	}
}

func TestTerminalDeploymentControlsRemainIndependentOfArtifactTemplate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		batches int
		apply   func(*Store, string, time.Time) (Deployment, []Job, error)
	}{
		{
			name: "finish-only Continue", batches: 1,
			apply: func(s *Store, id string, now time.Time) (Deployment, []Job, error) {
				return s.ContinueDeployment(id, now)
			},
		},
		{
			name: "Abandon", batches: 2,
			apply: func(s *Store, id string, now time.Time) (Deployment, []Job, error) {
				d, err := s.AbandonDeployment(id, now)
				return d, nil, err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 8, 23, 0, 0, 0, time.UTC)
			storeFinishedAt := now.Add(30*time.Second + 987*time.Nanosecond)
			s.nowFn = func() time.Time { return storeFinishedAt }
			d, jobs := createBatchGuardFixture(t, s, tc.batches)
			setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
			if _, err := s.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`,
				"sha256:"+strings.Repeat("7", 64), jobs[0].JobID); err != nil {
				t.Fatal(err)
			}
			if changed, err := s.PauseDeploymentAtBoundary(d.DeploymentID, 1,
				DeploymentPauseMaterial, "terminal recovery must not need artifact", now); err != nil || !changed {
				t.Fatalf("pause changed=%t err=%v", changed, err)
			}
			result, opened, err := tc.apply(s, d.DeploymentID, now.Add(time.Minute))
			if err != nil || result.State != DeploymentFinished || result.ControlRevision != 2 || len(opened) != 0 ||
				result.FinishedAt == nil || !result.FinishedAt.Equal(storeFinishedAt.Truncate(time.Second)) ||
				result.FinishedAt.Nanosecond() != 0 {
				t.Fatalf("result=%+v opened=%+v err=%v", result, opened, err)
			}
			targets, err := s.DeploymentTargets(d.DeploymentID)
			if err != nil || len(targets) != tc.batches || (tc.batches > 1 && targets[1].JobID != "") {
				t.Fatalf("terminal control targets=%+v err=%v", targets, err)
			}
		})
	}
}

func TestCreateDeploymentRejectsUnsafeBatchPlansBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		channel   string
		batchSize int
		targets   []NewDeploymentTarget
	}{
		{"canary batch overflow", "canary", 1, []NewDeploymentTarget{{MachineID: "a", BatchNo: 1}, {MachineID: "b", BatchNo: 1}}},
		{"stable batch overflow", "stable", 1, []NewDeploymentTarget{{MachineID: "a", BatchNo: 1}, {MachineID: "b", BatchNo: 1}}},
		{"batch number gap", "canary", 1, []NewDeploymentTarget{{MachineID: "a", BatchNo: 1}, {MachineID: "b", BatchNo: 3}}},
		{"huge batch number", "canary", 1, []NewDeploymentTarget{{MachineID: "a", BatchNo: 1}, {MachineID: "b", BatchNo: int(^uint(0) >> 1)}}},
		{"excluded target has a batch", "canary", 1, []NewDeploymentTarget{{MachineID: "a", BatchNo: 1}, {MachineID: "b", BatchNo: 2, ExcludedReason: "conflict"}}},
		{"Store batch size exceeds blast radius", "canary", MaxDeploymentBatchSize + 1, []NewDeploymentTarget{{MachineID: "a", BatchNo: 1}, {MachineID: "b", BatchNo: 1}}},
		{"no positive batch", "canary", 1, []NewDeploymentTarget{{MachineID: "a", ExcludedReason: "conflict"}, {MachineID: "b", ExcludedReason: "unknown_node"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			for _, id := range []string{"a", "b"} {
				addRolloutMachine(t, s, id, id, true)
				if err := s.SetMachineChannel(id, tc.channel); err != nil {
					t.Fatal(err)
				}
			}
			digest := strings.Repeat("4", 64)
			n := NewDeployment{
				Channel: tc.channel, ResourceKind: "openclaw", ResourceID: "openclaw",
				Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
				BatchSize: tc.batchSize, CreatedBy: "batch-invariant", Targets: tc.targets,
				Job: NewJob{ArtifactDigest: "sha256:" + digest},
			}
			before := snapshotRetryLedger(t, s)
			var err error
			if tc.channel == "stable" {
				_, _, _, err = s.createStableOpenClawDeployment(n, time.UTC)
			} else {
				_, _, err = s.CreateDeployment(n)
			}
			if !errors.Is(err, ErrDeploymentInvalidBatchPlan) {
				t.Fatalf("unsafe batch plan err=%v", err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("unsafe batch plan 寫了 ledger：before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestOpenDeploymentBatchRejectsLegacyMislabeledMaterial(t *testing.T) {
	s := rolloutStore(t)
	for _, id := range []string{"first", "later"} {
		addRolloutMachine(t, s, id, id, true)
	}
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "legacy-material", Targets: []NewDeploymentTarget{
			{MachineID: "first", BatchNo: 1}, {MachineID: "later", BatchNo: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
	if _, err := s.DB().Exec(`UPDATE deployments SET resource_kind='app',resource_id='x' WHERE deployment_id=?`, d.DeploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE desired_state SET resource_kind='app',resource_id='x' WHERE desired_id=?`, d.DesiredID); err != nil {
		t.Fatal(err)
	}
	before := snapshotRetryLedger(t, s)
	if _, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC()); !errors.Is(err, ErrDeploymentMaterialMismatch) {
		t.Fatalf("legacy mislabeled batch err=%v", err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("legacy mislabeled batch 寫了 job：before=%+v after=%+v", before, after)
	}
}

func TestDelayedDeploymentBatchRejectsUnsafeLegacyBatchPlan(t *testing.T) {
	for _, continueDeployment := range []bool{false, true} {
		name := "OpenDeploymentBatch"
		if continueDeployment {
			name = "ContinueDeployment"
		}
		t.Run(name, func(t *testing.T) {
			s := rolloutStore(t)
			d, first := createBatchGuardFixture(t, s, 2)
			if continueDeployment {
				setRolloutJobTerminal(t, s, first[0], deploy.Failed)
				if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
					t.Fatalf("pause changed=%v err=%v", changed, err)
				}
			} else {
				setRolloutJobTerminal(t, s, first[0], deploy.Succeeded)
			}
			if _, err := s.DB().Exec(`UPDATE deployments SET batch_size=? WHERE deployment_id=?`, MaxDeploymentBatchSize+1, d.DeploymentID); err != nil {
				t.Fatal(err)
			}
			before := snapshotRetryLedger(t, s)
			var err error
			if continueDeployment {
				_, _, err = s.ContinueDeployment(d.DeploymentID, time.Now().UTC())
			} else {
				_, err = s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
			}
			if !errors.Is(err, ErrDeploymentInvalidBatchPlan) {
				t.Fatalf("unsafe legacy plan err=%v", err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("unsafe legacy plan wrote ledger: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestDeploymentFinishRejectsUnknownStoredExclusionAtomically(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "included", "included", true)
	addRolloutMachine(t, s, "excluded", "excluded", true)
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "exclusion-test", Targets: []NewDeploymentTarget{
			{MachineID: "included", BatchNo: 1},
			{MachineID: "excluded", BatchNo: 0, ExcludedReason: "conflict"},
		},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("create exclusion fixture jobs=%+v err=%v", jobs, err)
	}
	setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
	if _, err := s.DB().Exec(`UPDATE deployment_targets SET excluded_reason='forged-secret-reason'
 WHERE deployment_id=? AND machine_id='excluded'`, d.DeploymentID); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeploymentMutationLedger(t, s, d.DeploymentID)
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentFinished, time.Now().UTC()); changed || !errors.Is(err, ErrDeploymentInvalidBatchPlan) {
		t.Fatalf("unknown stored exclusion changed=%t err=%v", changed, err)
	}
	if after := snapshotDeploymentMutationLedger(t, s, d.DeploymentID); after != before {
		t.Fatalf("unknown-exclusion rejection mutated ledger: before=%+v after=%+v", before, after)
	}
}

func TestCreateDeploymentRechecksConflictInsideTransaction(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "a", "a", true)
	desired, rev, err := s.CreateDesiredState("machine", "a", "openclaw", "openclaw", `{}`, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob("a", desired, rev, NewJob{}); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 5, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "a", BatchNo: 1}},
	})
	if !errors.Is(err, ErrDeploymentConflict) || !strings.Contains(err.Error(), "gained an active") {
		t.Fatalf("preview 後長出的 conflict 沒擋：%v", err)
	}
	var deployments int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&deployments); err != nil || deployments != 0 {
		t.Fatalf("衝突後留下 deployment：count=%d err=%v", deployments, err)
	}
}

func TestDelayedDeploymentBatchRechecksActiveResourceConflict(t *testing.T) {
	for _, continueDeployment := range []bool{false, true} {
		name := "OpenDeploymentBatch"
		if continueDeployment {
			name = "ContinueDeployment"
		}
		t.Run(name, func(t *testing.T) {
			s := rolloutStore(t)
			d, first := createBatchGuardFixture(t, s, 2)
			desiredID, revision, err := s.CreateDesiredState("machine", "machine-2", d.ResourceKind, d.ResourceID,
				`{"kind":"noop"}`, "competing-job")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateJob("machine-2", desiredID, revision, NewJob{}); err != nil {
				t.Fatalf("create competing job: %v", err)
			}
			if continueDeployment {
				setRolloutJobTerminal(t, s, first[0], deploy.Failed)
				if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
					t.Fatalf("pause changed=%v err=%v", changed, err)
				}
			} else {
				setRolloutJobTerminal(t, s, first[0], deploy.Succeeded)
			}
			before := snapshotRetryLedger(t, s)
			if continueDeployment {
				_, _, err = s.ContinueDeployment(d.DeploymentID, time.Now().UTC())
			} else {
				_, err = s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
			}
			if !errors.Is(err, ErrDeploymentConflict) {
				t.Fatalf("delayed batch conflict err=%v", err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("conflicting delayed batch 寫了 ledger：before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestDelayedDeploymentConflictRecheckSerializesAcrossStores(t *testing.T) {
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
	d, firstJobs := createBatchGuardFixture(t, first, 2)
	setRolloutJobTerminal(t, first, firstJobs[0], deploy.Succeeded)

	// D2 先拿到 writer reservation、在 D1 的未開 target 建立同資源 job，
	// 但先不 commit。D1 必須等它完整落地後重查，不能用等待前的結果開單。
	tx, err := second.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := createDesiredStateTx(tx, "machine", "machine-2", d.ResourceKind, d.ResourceID,
		`{"kind":"noop"}`, "competing-writer", second.now().UTC())
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,execution_timeout)
 VALUES (?,?,?,?,?,?,?)`, newID(), "machine-2", desiredID, revision, deploy.NotStarted,
		fmtTime(second.now().UTC()), 900); err != nil {
		_ = tx.Rollback()
		t.Fatalf("stage competing job: %v", err)
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := first.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		_ = tx.Rollback()
		t.Fatalf("D1 穿過未提交的 competing writer：%v", err)
	case <-time.After(250 * time.Millisecond):
		// BEGIN IMMEDIATE 應在這裡等待 D2。
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrDeploymentConflict) {
			t.Fatalf("D2 commit 後 D1 沒有 fail closed：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("D2 commit 後 D1 仍被擋住")
	}
	var opened bool
	if err := first.DB().QueryRow(`SELECT job_id IS NOT NULL FROM deployment_targets
 WHERE deployment_id=? AND machine_id='machine-2'`, d.DeploymentID).Scan(&opened); err != nil || opened {
		t.Fatalf("D1 conflicting batch opened=%v err=%v", opened, err)
	}
}

func TestDelayedDeploymentBatchRejectsHigherTerminalRevision(t *testing.T) {
	s := rolloutStore(t)
	d, first := createBatchGuardFixture(t, s, 2)
	setRolloutJobTerminal(t, s, first[0], deploy.Succeeded)
	desiredID, revision, err := s.CreateDesiredState("machine", "machine-2", "openclaw", "openclaw", `{"kind":"noop"}`, "newer-terminal")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := s.CreateJob("machine-2", desiredID, revision, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	setRolloutJobTerminal(t, s, Job{JobID: jobID}, deploy.Succeeded)
	before := snapshotRetryLedger(t, s)
	if _, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC()); !errors.Is(err, ErrDeploymentStaleRevision) {
		t.Fatalf("higher terminal revision 沒擋舊 batch：%v", err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("stale delayed batch 寫了 ledger：before=%+v after=%+v", before, after)
	}
}

func TestDeploymentStuckSeparatesTerminalAndNoEvent(t *testing.T) {
	s := rolloutStore(t)
	for _, id := range []string{"failed", "silent"} {
		addRolloutMachine(t, s, id, id, true)
	}
	now := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now.Add(-StuckThreshold - time.Second) }
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 5, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "failed", BatchNo: 1}, {MachineID: "silent", BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var failedJob string
	for _, j := range jobs {
		if j.MachineID == "failed" {
			failedJob = j.JobID
		}
	}
	if _, err := s.ClaimJob(failedJob, "failed", now.Add(-time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceJobByHub(failedJob, deploy.LeaseLost, now); err != nil {
		t.Fatal(err)
	}
	view, err := s.DeploymentView(d.DeploymentID, now)
	if err != nil {
		t.Fatal(err)
	}
	if view.TerminalStuck != 1 || view.SilentStuck != 1 || view.Stuck != 2 {
		t.Fatalf("stuck 沒分開：%+v", view)
	}
}

func TestContinueOpensNextBatchWithoutReopeningFailedJob(t *testing.T) {
	s := rolloutStore(t)
	for _, id := range []string{"bad", "next"} {
		addRolloutMachine(t, s, id, id, true)
	}
	now := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}, {MachineID: "next", BatchNo: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.ClaimJob(jobs[0].JobID, "bad", now.Add(-time.Minute), time.Hour)
	if err != nil || token == "" {
		t.Fatal(err)
	}
	if _, err := s.AdvanceJobByHub(jobs[0].JobID, deploy.Timeout, now); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
		t.Fatalf("pause changed=%v err=%v", changed, err)
	}
	continued, next, err := s.ContinueDeployment(d.DeploymentID, now.Add(time.Minute))
	if err != nil || continued.State != DeploymentRunning || len(next) != 1 || next[0].MachineID != "next" {
		t.Fatalf("continue=%+v next=%+v err=%v", continued, next, err)
	}
	bad, err := s.Job(jobs[0].JobID)
	if err != nil || bad.State != deploy.Failed {
		t.Fatalf("舊失敗單被改了：%+v err=%v", bad, err)
	}
	all, _ := s.ListJobs("", 20)
	if len(all) != 2 || next[0].JobID == jobs[0].JobID {
		t.Fatalf("Continue 應開全新下一批：%+v", all)
	}
}

func TestContinueWithNoNextBatchFinishesButKeepsStuck(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "bad", "bad", true)
	now := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 5, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(jobs[0].JobID, "bad", now.Add(-time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceJobByHub(jobs[0].JobID, deploy.Timeout, now); err != nil {
		t.Fatal(err)
	}
	_, _ = s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, now)
	trustedFinished := now.Add(2 * time.Minute)
	s.nowFn = func() time.Time { return trustedFinished }
	continued, opened, err := s.ContinueDeployment(d.DeploymentID, now.Add(-time.Hour))
	if err != nil || continued.State != DeploymentFinished || continued.FinishedAt == nil ||
		!continued.FinishedAt.Equal(trustedFinished) || len(opened) != 0 {
		t.Fatalf("continue=%+v opened=%+v err=%v", continued, opened, err)
	}
	v, _ := s.DeploymentView(d.DeploymentID, trustedFinished)
	if v.Stuck != 1 {
		t.Fatalf("finished 不准洗掉 stuck：%+v", v)
	}
}

func TestPauseDeploymentAtBoundaryIsAtomicIdempotentAndContinueClearsIt(t *testing.T) {
	s := rolloutStore(t)
	d, first := createBatchGuardFixture(t, s, 2)
	now := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	setRolloutJobTerminal(t, s, first[0], deploy.Succeeded)
	reason := "store: promote locked: canary observation is not ready"

	changed, err := s.PauseDeploymentAtBoundary(d.DeploymentID, 1, DeploymentPausePromoteLocked, reason, now)
	if err != nil || !changed {
		t.Fatalf("boundary pause changed=%v err=%v", changed, err)
	}
	view, err := s.DeploymentView(d.DeploymentID, now)
	if err != nil || view.State != DeploymentPaused || view.BoundaryPause == nil {
		t.Fatalf("boundary pause view=%+v err=%v", view, err)
	}
	if view.BoundaryPause.OpenedBatch != 1 || view.BoundaryPause.Kind != DeploymentPausePromoteLocked ||
		view.BoundaryPause.Reason != reason || !view.BoundaryPause.PausedAt.Equal(now) {
		t.Fatalf("boundary pause evidence=%+v", view.BoundaryPause)
	}
	if again, err := s.PauseDeploymentAtBoundary(d.DeploymentID, 1, DeploymentPausePromoteLocked, reason, now.Add(time.Minute)); err != nil || again {
		t.Fatalf("replayed boundary pause changed=%v err=%v", again, err)
	}
	events, err := s.HubEventsBetween(time.Time{}, now.Add(time.Hour))
	if err != nil || len(events) != 1 || events[0].Kind != HubDeploymentBoundaryPaused || !strings.Contains(events[0].Detail, reason) {
		t.Fatalf("boundary pause events=%+v err=%v", events, err)
	}

	continued, jobs, err := s.ContinueDeployment(d.DeploymentID, now.Add(time.Minute))
	if err != nil || continued.State != DeploymentRunning || len(jobs) != 1 {
		t.Fatalf("continue after boundary pause deployment=%+v jobs=%+v err=%v", continued, jobs, err)
	}
	view, err = s.DeploymentView(d.DeploymentID, now.Add(time.Minute))
	if err != nil || view.BoundaryPause != nil {
		t.Fatalf("successful Continue did not clear boundary evidence: view=%+v err=%v", view, err)
	}
}

func TestPauseDeploymentAtBoundaryCASDoesNotPauseAfterAnotherStoreOpenedNext(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "boundary-race.db")
	firstStore, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer firstStore.Close()
	secondStore, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	d, first := createBatchGuardFixture(t, firstStore, 2)
	setRolloutJobTerminal(t, firstStore, first[0], deploy.Succeeded)

	if jobs, err := secondStore.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC()); err != nil || len(jobs) != 1 {
		t.Fatalf("competing opener jobs=%+v err=%v", jobs, err)
	}
	changed, err := firstStore.PauseDeploymentAtBoundary(
		d.DeploymentID, 1, DeploymentPauseBatchNotReady, "stale driver view", time.Now().UTC())
	if err != nil || changed {
		t.Fatalf("stale CAS pause changed=%v err=%v", changed, err)
	}
	view, err := firstStore.DeploymentView(d.DeploymentID, time.Now().UTC())
	if err != nil || view.State != DeploymentRunning || view.OpenedBatch != 2 || view.BoundaryPause != nil {
		t.Fatalf("stale CAS paused a deployment that already expanded: view=%+v err=%v", view, err)
	}
	events, err := firstStore.HubEventsBetween(time.Time{}, time.Now().UTC().Add(time.Hour))
	boundaryEvents := 0
	for _, event := range events {
		if event.Kind == HubDeploymentBoundaryPaused {
			boundaryEvents++
		}
	}
	if err != nil || boundaryEvents != 0 {
		t.Fatalf("stale CAS wrote event: events=%+v err=%v", events, err)
	}
}

func TestPauseDeploymentAtBoundaryPersistsAPlanGapWithNoNextTarget(t *testing.T) {
	s := rolloutStore(t)
	d, first := createBatchGuardFixture(t, s, 2)
	setRolloutJobTerminal(t, s, first[0], deploy.Succeeded)
	if _, err := s.DB().Exec(`UPDATE deployment_targets SET batch_no=3
		WHERE deployment_id=? AND batch_no=2`, d.DeploymentID); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	changed, err := s.PauseDeploymentAtBoundary(
		d.DeploymentID, 1, DeploymentPauseInvalidPlan, "legacy deployment has a batch gap", now)
	if err != nil || !changed {
		t.Fatalf("gap boundary pause changed=%v err=%v", changed, err)
	}
	view, err := s.DeploymentView(d.DeploymentID, now)
	if err != nil || view.State != DeploymentPaused || view.BoundaryPause == nil ||
		view.BoundaryPause.Kind != DeploymentPauseInvalidPlan {
		t.Fatalf("gap boundary pause was not persisted: view=%+v err=%v", view, err)
	}
}

func TestRetryLedgerUsesHigherRevisionSameSpecAndFreshJobs(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "bad", "bad", true)
	old, oldJobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 5, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	if _, err := s.ClaimJob(oldJobs[0].JobID, "bad", now.Add(-time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceJobByHub(oldJobs[0].JobID, deploy.Timeout, now); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetDeploymentState(old.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
		t.Fatalf("pause retry parent changed=%v err=%v", changed, err)
	}
	retry, retryJobs, err := s.CreateDeployment(NewDeployment{
		Channel: old.Channel, ResourceKind: old.ResourceKind, ResourceID: old.ResourceID, Spec: old.Spec,
		BatchSize: old.BatchSize, CreatedBy: "test", RetryOf: old.DeploymentID,
		Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.DeploymentView(retry.DeploymentID, time.Now())
	if retry.Revision <= old.Revision || retry.Spec != old.Spec || retry.RetryOf != old.DeploymentID || v.Attempt != 2 {
		t.Fatalf("retry 帳本不符：old=%+v retry=%+v view=%+v", old, retry, v)
	}
	if retryJobs[0].JobID == oldJobs[0].JobID {
		t.Fatal("retry 重用了舊 job_id")
	}
}

func TestCreateDeploymentRejectsFinishedResourceWithNonterminalDeploymentJobAtomically(t *testing.T) {
	for _, drift := range []string{
		"none", "deployment resource drift", "desired resource drift", "linked job desired drift", "missing desired row",
	} {
		t.Run(drift, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "orphan-old", "orphan-old", true)
			old, oldJobs, err := s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
				BatchSize: 1, CreatedBy: "orphan-owner", Targets: []NewDeploymentTarget{{MachineID: "orphan-old", BatchNo: 1}},
			})
			if err != nil || len(oldJobs) != 1 {
				t.Fatalf("create old deployment jobs=%+v err=%v", oldJobs, err)
			}
			if _, err := s.DB().Exec(`UPDATE deployments SET state=?,finished_at=? WHERE deployment_id=?`,
				DeploymentFinished, fmtTime(time.Now().UTC()), old.DeploymentID); err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "deployment resource drift":
				if _, err := s.DB().Exec(`UPDATE deployments SET resource_id='drifted' WHERE deployment_id=?`, old.DeploymentID); err != nil {
					t.Fatal(err)
				}
			case "desired resource drift":
				if _, err := s.DB().Exec(`UPDATE desired_state SET resource_id='drifted' WHERE desired_id=?`, old.DesiredID); err != nil {
					t.Fatal(err)
				}
			case "linked job desired drift":
				if _, err := s.DB().Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES (?,?,?,?,?,?,?,?,?)`, "forged-admission-desired", "machine", "orphan-old", "other", "other", 1,
					`{"kind":"noop"}`, fmtTime(time.Now().UTC()), "forged"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`UPDATE jobs SET desired_id=? WHERE job_id=?`, "forged-admission-desired", oldJobs[0].JobID); err != nil {
					t.Fatal(err)
				}
			case "missing desired row":
				s.DB().SetMaxOpenConns(1)
				if _, err := s.DB().Exec(`PRAGMA foreign_keys=OFF`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`DELETE FROM desired_state WHERE desired_id=?`, old.DesiredID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`PRAGMA foreign_keys=ON`); err != nil {
					t.Fatal(err)
				}
			}
			addRolloutMachine(t, s, "orphan-new", "orphan-new", true)
			before := snapshotRetryLedger(t, s)
			var eventsBefore int
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM hub_events`).Scan(&eventsBefore); err != nil {
				t.Fatal(err)
			}
			_, _, err = s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
				BatchSize: 1, CreatedBy: "must-not-claim", Targets: []NewDeploymentTarget{
					{MachineID: "orphan-old", BatchNo: 0, ExcludedReason: "conflict"},
					{MachineID: "orphan-new", BatchNo: 1},
				},
			})
			if !errors.Is(err, ErrDeploymentActiveResource) {
				t.Fatalf("finished deployment active job did not block create: %v", err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("rejected create mutated ledger: before=%+v after=%+v", before, after)
			}
			var eventsAfter int
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM hub_events`).Scan(&eventsAfter); err != nil || eventsAfter != eventsBefore {
				t.Fatalf("rejected create events=%d/%d err=%v", eventsAfter, eventsBefore, err)
			}
		})
	}
}

func TestRetryDeploymentRejectsUnrelatedFinishedResourceActiveJobAtomically(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "retry-orphan", "retry-orphan", true)
	old, oldJobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "old", Targets: []NewDeploymentTarget{{MachineID: "retry-orphan", BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	setRolloutJobTerminal(t, s, oldJobs[0], deploy.Succeeded)
	if changed, err := s.SetDeploymentState(old.DeploymentID, DeploymentRunning, DeploymentFinished, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("finish old changed=%t err=%v", changed, err)
	}
	parent, parentJobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "parent", Targets: []NewDeploymentTarget{{MachineID: "retry-orphan", BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	setRolloutJobTerminal(t, s, parentJobs[0], deploy.Failed)
	if changed, err := s.SetDeploymentState(parent.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause parent changed=%t err=%v", changed, err)
	}
	parent, err = s.Deployment(parent.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=NULL WHERE job_id=?`, deploy.Running, oldJobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	before := snapshotDeploymentMutationLedger(t, s, parent.DeploymentID)
	if _, _, err := s.CreateDeployment(retryFixtureInput(parent, "retry-orphan")); !errors.Is(err, ErrDeploymentActiveResource) {
		t.Fatalf("unrelated finished deployment active job did not block retry: %v", err)
	}
	if after := snapshotDeploymentMutationLedger(t, s, parent.DeploymentID); after != before {
		t.Fatalf("rejected retry mutated ledger: before=%+v after=%+v", before, after)
	}
}

func TestDeploymentMutationsRejectForgedJobGraphAtomically(t *testing.T) {
	corruptions := []struct {
		name           string
		leaveJobActive bool
		forge          func(*testing.T, *Store, Deployment, Job)
	}{
		{
			name:           "cleared required job link",
			leaveJobActive: true,
			forge: func(t *testing.T, s *Store, d Deployment, _ Job) {
				if _, err := s.DB().Exec(`UPDATE deployment_targets SET job_id=NULL WHERE deployment_id=?`, d.DeploymentID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "linked job machine mismatch",
			forge: func(t *testing.T, s *Store, _ Deployment, job Job) {
				addRolloutMachine(t, s, "forged-machine", "forged-machine", true)
				if _, err := s.DB().Exec(`UPDATE jobs SET machine_id=? WHERE job_id=?`, "forged-machine", job.JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "linked job desired mismatch",
			forge: func(t *testing.T, s *Store, d Deployment, job Job) {
				if _, err := s.DB().Exec(`DROP INDEX ` + desiredStateResourceRevisionIndex); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES (?,?,?,?,?,?,?,?,?)`, "forged-desired", "machine", job.MachineID, d.ResourceKind, d.ResourceID,
					d.Revision, d.Spec, fmtTime(time.Now().UTC()), "forged-ledger"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`UPDATE jobs SET desired_id=? WHERE job_id=?`, "forged-desired", job.JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "linked job revision mismatch",
			forge: func(t *testing.T, s *Store, _ Deployment, job Job) {
				if _, err := s.DB().Exec(`UPDATE jobs SET revision=revision+1 WHERE job_id=?`, job.JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "desired state identity mismatch",
			forge: func(t *testing.T, s *Store, d Deployment, _ Job) {
				if _, err := s.DB().Exec(`UPDATE desired_state SET scope_type='machine' WHERE desired_id=?`, d.DesiredID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invalid deployment channel header",
			forge: func(t *testing.T, s *Store, d Deployment, _ Job) {
				if _, err := s.DB().Exec(`UPDATE deployments SET channel='beta' WHERE deployment_id=?`, d.DeploymentID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`UPDATE desired_state SET scope_id='beta' WHERE desired_id=?`, d.DesiredID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invalid deployment revision header",
			forge: func(t *testing.T, s *Store, d Deployment, job Job) {
				if _, err := s.DB().Exec(`UPDATE deployments SET revision=0 WHERE deployment_id=?`, d.DeploymentID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`UPDATE desired_state SET revision=0 WHERE desired_id=?`, d.DesiredID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`UPDATE jobs SET revision=0 WHERE job_id=?`, job.JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "target machine missing from registry",
			forge: func(t *testing.T, s *Store, _ Deployment, job Job) {
				s.DB().SetMaxOpenConns(1)
				if _, err := s.DB().Exec(`PRAGMA foreign_keys=OFF`); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`DELETE FROM machine_registry WHERE machine_id=?`, job.MachineID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`PRAGMA foreign_keys=ON`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unknown linked job state",
			forge: func(t *testing.T, s *Store, _ Deployment, job Job) {
				if _, err := s.DB().Exec(`UPDATE jobs SET state='unknown-state' WHERE job_id=?`, job.JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "terminal linked job missing terminal time",
			forge: func(t *testing.T, s *Store, _ Deployment, job Job) {
				if _, err := s.DB().Exec(`UPDATE jobs SET terminal_at=NULL WHERE job_id=?`, job.JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "nonterminal linked job has terminal time",
			forge: func(t *testing.T, s *Store, _ Deployment, job Job) {
				if _, err := s.DB().Exec(`UPDATE jobs SET state=? WHERE job_id=?`, deploy.Running, job.JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "linked job terminal time is not canonical",
			forge: func(t *testing.T, s *Store, _ Deployment, job Job) {
				if _, err := s.DB().Exec(`UPDATE jobs SET terminal_at='2026-09-08T12:00:00+00:00' WHERE job_id=?`, job.JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "job shared by another deployment target",
			forge: func(t *testing.T, s *Store, _ Deployment, job Job) {
				insertLegacyDeployment(t, s, "forged-foreign-deployment", "canary", "other", "other", DeploymentFinished, 1)
				if _, err := s.DB().Exec(`INSERT INTO deployment_targets
 (deployment_id,machine_id,batch_no,job_id) VALUES (?,?,?,?)`,
					"forged-foreign-deployment", job.MachineID, 1, job.JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:           "active job orphaned behind compatible terminal replacement",
			leaveJobActive: true,
			forge: func(t *testing.T, s *Store, d Deployment, job Job) {
				replacementID := newID()
				now := time.Now().UTC()
				if _, err := s.DB().Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,irreversible,execution_timeout,terminal_at)
 VALUES (?,?,?,?,?,?,?,?,?)`, replacementID, job.MachineID, d.DesiredID, d.Revision,
					deploy.Failed, fmtTime(now), false, 900, fmtTime(now)); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`UPDATE deployment_targets SET job_id=?
 WHERE deployment_id=? AND machine_id=?`, replacementID, d.DeploymentID, job.MachineID); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	actions := []struct {
		name string
		run  func(*Store, Deployment) error
	}{
		{
			name: "continue",
			run: func(s *Store, d Deployment) error {
				_, _, err := s.ContinueDeployment(d.DeploymentID, time.Now().UTC())
				return err
			},
		},
		{
			name: "retry",
			run: func(s *Store, d Deployment) error {
				_, _, err := s.CreateDeployment(retryFixtureInput(d, "graph-target"))
				return err
			},
		},
		{
			name: "abandon",
			run: func(s *Store, d Deployment) error {
				_, err := s.AbandonDeployment(d.DeploymentID, time.Now().UTC())
				return err
			},
		},
	}

	for _, corruption := range corruptions {
		for _, action := range actions {
			t.Run(corruption.name+"/"+action.name, func(t *testing.T) {
				s := rolloutStore(t)
				addRolloutMachine(t, s, "graph-target", "graph-target", true)
				parent, jobs, err := s.CreateDeployment(NewDeployment{
					Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
					BatchSize: 1, CreatedBy: "job-graph-test", Targets: []NewDeploymentTarget{{MachineID: "graph-target", BatchNo: 1}},
				})
				if err != nil || len(jobs) != 1 {
					t.Fatalf("create parent=%+v jobs=%+v err=%v", parent, jobs, err)
				}
				if !corruption.leaveJobActive {
					setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
				}
				if changed, err := s.SetDeploymentState(parent.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
					t.Fatalf("pause parent changed=%v err=%v", changed, err)
				}
				parent, err = s.Deployment(parent.DeploymentID)
				if err != nil {
					t.Fatal(err)
				}
				corruption.forge(t, s, parent, jobs[0])
				if _, err := s.DeploymentView(parent.DeploymentID, time.Now().UTC()); !errors.Is(err, ErrDeploymentJobGraphMismatch) {
					t.Fatalf("DeploymentView exposed %s: %v", corruption.name, err)
				}
				before := snapshotDeploymentMutationLedger(t, s, parent.DeploymentID)

				if err := action.run(s, parent); !errors.Is(err, ErrDeploymentJobGraphMismatch) {
					t.Fatalf("%s accepted %s: %v", action.name, corruption.name, err)
				}
				if after := snapshotDeploymentMutationLedger(t, s, parent.DeploymentID); after != before {
					t.Fatalf("rejected %s mutated ledger:\nbefore=%+v\nafter=%+v", action.name, before, after)
				}
			})
		}
	}
}

type deploymentMutationLedgerSnapshot struct {
	retryLedgerSnapshot
	state, pausedAt, finishedAt, targetLinks, jobIdentities string
	controlRevision                                         int64
	events, soakBoundaries                                  int
}

func snapshotDeploymentMutationLedger(t *testing.T, s *Store, deploymentID string) deploymentMutationLedgerSnapshot {
	t.Helper()
	got := deploymentMutationLedgerSnapshot{retryLedgerSnapshot: snapshotRetryLedger(t, s)}
	if err := s.DB().QueryRow(`SELECT state,control_revision,COALESCE(paused_at,''),COALESCE(finished_at,'')
 FROM deployments WHERE deployment_id=?`, deploymentID).
		Scan(&got.state, &got.controlRevision, &got.pausedAt, &got.finishedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT COALESCE(group_concat(entry,'|'),'') FROM (
 SELECT deployment_id||':'||machine_id||':'||batch_no||':'||COALESCE(job_id,'')||':'||COALESCE(excluded_reason,'') AS entry
 FROM deployment_targets ORDER BY deployment_id,machine_id)`).Scan(&got.targetLinks); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT COALESCE(group_concat(entry,'|'),'') FROM (
 SELECT job_id||':'||machine_id||':'||COALESCE(desired_id,'')||':'||revision||':'||state||':'||COALESCE(terminal_at,'') AS entry
 FROM jobs ORDER BY job_id)`).Scan(&got.jobIdentities); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM hub_events`).Scan(&got.events); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM deployment_soak_boundaries`).Scan(&got.soakBoundaries); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDeploymentRevisionIncrementBoundariesFailClosedAtomically(t *testing.T) {
	actions := []struct {
		name  string
		setup func(*testing.T, *Store) Deployment
		run   func(*Store, Deployment) error
	}{
		{
			name: "open batch",
			setup: func(t *testing.T, s *Store) Deployment {
				d, jobs := createBatchGuardFixture(t, s, 2)
				setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
				return d
			},
			run: func(s *Store, d Deployment) error {
				_, err := s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
				return err
			},
		},
		{
			name: "pause state",
			setup: func(t *testing.T, s *Store) Deployment {
				d, _ := createBatchGuardFixture(t, s, 1)
				return d
			},
			run: func(s *Store, d Deployment) error {
				_, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC())
				return err
			},
		},
		{
			name: "finish state",
			setup: func(t *testing.T, s *Store) Deployment {
				d, jobs := createBatchGuardFixture(t, s, 1)
				setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
				return d
			},
			run: func(s *Store, d Deployment) error {
				_, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentFinished, time.Now().UTC())
				return err
			},
		},
		{
			name: "boundary pause",
			setup: func(t *testing.T, s *Store) Deployment {
				d, jobs := createBatchGuardFixture(t, s, 2)
				setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
				return d
			},
			run: func(s *Store, d Deployment) error {
				_, err := s.PauseDeploymentAtBoundary(d.DeploymentID, 1,
					DeploymentPauseBatchNotReady, "revision-limit-test", time.Now().UTC())
				return err
			},
		},
		{
			name:  "continue",
			setup: func(t *testing.T, s *Store) Deployment { return pausedFailedRevisionFixture(t, s) },
			run: func(s *Store, d Deployment) error {
				_, _, err := s.ContinueDeployment(d.DeploymentID, time.Now().UTC())
				return err
			},
		},
		{
			name:  "abandon",
			setup: func(t *testing.T, s *Store) Deployment { return pausedFailedRevisionFixture(t, s) },
			run: func(s *Store, d Deployment) error {
				_, err := s.AbandonDeployment(d.DeploymentID, time.Now().UTC())
				return err
			},
		},
		{
			name:  "retry parent",
			setup: func(t *testing.T, s *Store) Deployment { return pausedFailedRevisionFixture(t, s) },
			run: func(s *Store, d Deployment) error {
				n := retryFixtureInput(d, "machine-1")
				n.Job = NewJob{ArtifactDigest: "sha256:" + strings.Repeat("5", 64), ExecutionTimeout: 600}
				_, _, err := s.CreateDeployment(n)
				return err
			},
		},
	}
	for _, revision := range []int64{-1, MaxDeploymentControlRevision} {
		for _, action := range actions {
			t.Run(fmt.Sprintf("revision=%d/%s", revision, action.name), func(t *testing.T) {
				s := rolloutStore(t)
				d := action.setup(t, s)
				if _, err := s.DB().Exec(`UPDATE deployments SET control_revision=? WHERE deployment_id=?`, revision, d.DeploymentID); err != nil {
					t.Fatal(err)
				}
				before := snapshotDeploymentMutationLedger(t, s, d.DeploymentID)
				if err := action.run(s, d); !errors.Is(err, ErrDeploymentControlRevisionLimit) {
					t.Fatalf("revision boundary accepted: %v", err)
				}
				if after := snapshotDeploymentMutationLedger(t, s, d.DeploymentID); after != before {
					t.Fatalf("revision-boundary rejection mutated ledger: before=%+v after=%+v", before, after)
				}
			})
		}
	}
}

func pausedFailedRevisionFixture(t *testing.T, s *Store) Deployment {
	t.Helper()
	d, jobs := createBatchGuardFixture(t, s, 1)
	setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause revision fixture changed=%t err=%v", changed, err)
	}
	d, err := s.Deployment(d.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type retryLedgerSnapshot struct {
	desired, deployments, targets, jobs, counters, revision int
}

func snapshotRetryLedger(t *testing.T, s *Store) retryLedgerSnapshot {
	t.Helper()
	var got retryLedgerSnapshot
	for _, item := range []struct {
		query string
		dest  *int
	}{
		{`SELECT COUNT(*) FROM desired_state`, &got.desired},
		{`SELECT COUNT(*) FROM deployments`, &got.deployments},
		{`SELECT COUNT(*) FROM deployment_targets`, &got.targets},
		{`SELECT COUNT(*) FROM jobs`, &got.jobs},
		{`SELECT COUNT(*) FROM revision_counters`, &got.counters},
		{`SELECT COALESCE(SUM(current_revision),0) FROM revision_counters`, &got.revision},
	} {
		if err := s.DB().QueryRow(item.query).Scan(item.dest); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

func finishRetryFixture(t *testing.T, s *Store, d Deployment, jobs []Job, state string) {
	t.Helper()
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	for _, job := range jobs {
		if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`, deploy.Failed, fmtTime(at), job.JobID); err != nil {
			t.Fatal(err)
		}
	}
	if state != DeploymentRunning {
		if state == DeploymentFinished {
			if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, at); err != nil || !changed {
				t.Fatalf("pause parent before finished changed=%v err=%v", changed, err)
			}
			finished, opened, err := s.ContinueDeployment(d.DeploymentID, at)
			if err != nil || finished.State != DeploymentFinished || len(opened) != 0 {
				t.Fatalf("finish parent through paused path=%+v opened=%+v err=%v", finished, opened, err)
			}
		} else if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, state, at); err != nil || !changed {
			t.Fatalf("set parent state=%s changed=%v err=%v", state, changed, err)
		}
	}
}

func retryFixtureInput(parent Deployment, machineID string) NewDeployment {
	return NewDeployment{
		Channel: parent.Channel, ResourceKind: parent.ResourceKind, ResourceID: parent.ResourceID, Spec: parent.Spec,
		BatchSize: parent.BatchSize, CreatedBy: "retry invariant test", RetryOf: parent.DeploymentID,
		Targets: []NewDeploymentTarget{{MachineID: machineID, BatchNo: 1}},
	}
}

func TestCreateDeploymentRejectsRetryMetadataMismatchBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*NewDeployment)
	}{
		{"channel", func(n *NewDeployment) { n.Channel = "stable" }},
		{"resource kind", func(n *NewDeployment) { n.ResourceKind = "app" }},
		{"resource id", func(n *NewDeployment) { n.ResourceID = "another-openclaw" }},
		{"spec exact bytes", func(n *NewDeployment) { n.Spec += " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "bad", "bad", true)
			parent, jobs, err := s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
				BatchSize: 1, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			finishRetryFixture(t, s, parent, jobs, DeploymentFinished)
			n := retryFixtureInput(parent, "bad")
			tc.change(&n)
			before := snapshotRetryLedger(t, s)
			var createErr error
			if n.Channel == "stable" {
				tx, err := s.DB().Begin()
				if err != nil {
					t.Fatal(err)
				}
				_, _, createErr = createDeploymentTx(tx, n, time.Now().UTC())
				_ = tx.Rollback()
			} else {
				_, _, createErr = s.CreateDeployment(n)
			}
			if !errors.Is(createErr, ErrDeploymentRetryMismatch) {
				t.Fatalf("mismatch err=%v", createErr)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("mismatch retry 寫了 ledger：before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestCreateDeploymentValidatesEveryRetryAncestorBeforeWriting(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "bad", "bad", true)
	root, rootJobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	finishRetryFixture(t, s, root, rootJobs, DeploymentFinished)
	first, firstJobs, err := s.CreateDeployment(retryFixtureInput(root, "bad"))
	if err != nil {
		t.Fatal(err)
	}
	finishRetryFixture(t, s, first, firstJobs, DeploymentFinished)
	if _, err := s.DB().Exec(`UPDATE desired_state SET spec=? WHERE desired_id=?`, `{"tampered":true}`, root.DesiredID); err != nil {
		t.Fatal(err)
	}
	before := snapshotRetryLedger(t, s)
	if _, _, err := s.CreateDeployment(retryFixtureInput(first, "bad")); !errors.Is(err, ErrDeploymentRetryMismatch) {
		t.Fatalf("深層 ancestor mismatch err=%v", err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("深層 mismatch retry 寫了 ledger：before=%+v after=%+v", before, after)
	}
}

func TestCreateDeploymentRejectsMissingAndCyclicRetryParentsBeforeWriting(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		s := rolloutStore(t)
		addRolloutMachine(t, s, "bad", "bad", true)
		n := NewDeployment{
			Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
			BatchSize: 1, CreatedBy: "test", RetryOf: "missing-parent",
			Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}},
		}
		before := snapshotRetryLedger(t, s)
		if _, _, err := s.CreateDeployment(n); !errors.Is(err, ErrDeploymentRetryParentNotFound) {
			t.Fatalf("missing parent err=%v", err)
		}
		if after := snapshotRetryLedger(t, s); after != before {
			t.Fatalf("missing parent retry 寫了 ledger：before=%+v after=%+v", before, after)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		s := rolloutStore(t)
		addRolloutMachine(t, s, "bad", "bad", true)
		parent, jobs, err := s.CreateDeployment(NewDeployment{
			Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
			BatchSize: 1, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		finishRetryFixture(t, s, parent, jobs, DeploymentFinished)
		if _, err := s.DB().Exec(`UPDATE deployments SET retry_of=? WHERE deployment_id=?`, parent.DeploymentID, parent.DeploymentID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeploymentView(parent.DeploymentID, time.Now().UTC()); !errors.Is(err, ErrDeploymentRetryCycle) {
			t.Fatalf("DeploymentView 沒有 bounded cycle guard：%v", err)
		}
		before := snapshotRetryLedger(t, s)
		if _, _, err := s.CreateDeployment(retryFixtureInput(parent, "bad")); !errors.Is(err, ErrDeploymentRetryCycle) {
			t.Fatalf("cyclic parent err=%v", err)
		}
		if after := snapshotRetryLedger(t, s); after != before {
			t.Fatalf("cyclic parent retry 寫了 ledger：before=%+v after=%+v", before, after)
		}
	})
}

func TestDeploymentRetryLineageHasAHardDepthBound(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "bad", "bad", true)
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	previous := ""
	for i := 0; i <= maxDeploymentRetryLineage; i++ {
		desiredID, deploymentID, jobID := newID(), newID(), newID()
		createdAt := fmtTime(time.Now())
		if _, err := tx.Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
	 VALUES (?,?,?,?,?,?,?,?,?)`, desiredID, "channel", "canary", "openclaw", "openclaw", i+1, rolloutTestOpenClawSpec, createdAt, "depth test"); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO deployments
 (deployment_id,channel,desired_id,resource_kind,resource_id,revision,batch_size,state,created_at,created_by,finished_at,retry_of)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, deploymentID, "canary", desiredID, "openclaw", "openclaw", i+1, 1,
			DeploymentFinished, createdAt, "depth test", createdAt, nullIfEmpty(previous)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,irreversible,execution_timeout,terminal_at)
 VALUES (?,?,?,?,?,?,?,?,?)`, jobID, "bad", desiredID, i+1, deploy.Succeeded, createdAt, false, 900, createdAt); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO deployment_targets
 (deployment_id,machine_id,batch_no,job_id) VALUES (?,?,?,?)`, deploymentID, "bad", 1, jobID); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		previous = deploymentID
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeploymentView(previous, time.Now()); !errors.Is(err, ErrDeploymentRetryTooDeep) {
		t.Fatalf("DeploymentView unbounded lineage err=%v", err)
	}
	before := snapshotRetryLedger(t, s)
	n := NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "depth test", RetryOf: previous,
		Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}},
	}
	if _, _, err := s.CreateDeployment(n); !errors.Is(err, ErrDeploymentRetryTooDeep) {
		t.Fatalf("create unbounded lineage err=%v", err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("too-deep retry 寫了 ledger：before=%+v after=%+v", before, after)
	}
}

func TestCreateDeploymentRejectsMutableRetryParentWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*testing.T, *Store, Deployment, []Job)
		want error
	}{
		{"running", func(t *testing.T, s *Store, parent Deployment, jobs []Job) {
			finishRetryFixture(t, s, parent, jobs, DeploymentRunning)
		}, ErrDeploymentRetryParentRunning},
		{"paused with active job", func(t *testing.T, s *Store, parent Deployment, _ []Job) {
			if changed, err := s.SetDeploymentState(parent.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
				t.Fatalf("pause parent changed=%v err=%v", changed, err)
			}
		}, ErrDeploymentRetryParentActive},
		{"finished with active job", func(t *testing.T, s *Store, parent Deployment, _ []Job) {
			// Legacy/corrupt ledger fixture: the public transition now rejects
			// releasing ownership while this job is still active.
			if _, err := s.DB().Exec(`UPDATE deployments SET state=?,finished_at=? WHERE deployment_id=?`,
				DeploymentFinished, fmtTime(time.Now().UTC()), parent.DeploymentID); err != nil {
				t.Fatal(err)
			}
		}, ErrDeploymentRetryParentActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "bad", "bad", true)
			parent, jobs, err := s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
				BatchSize: 1, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "bad", BatchNo: 1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			tc.set(t, s, parent, jobs)
			before := snapshotRetryLedger(t, s)
			if _, _, err := s.CreateDeployment(retryFixtureInput(parent, "bad")); !errors.Is(err, tc.want) {
				t.Fatalf("parent case=%s err=%v", tc.name, err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("unfinished parent retry 寫了 ledger：before=%+v after=%+v", before, after)
			}
		})
	}
}
