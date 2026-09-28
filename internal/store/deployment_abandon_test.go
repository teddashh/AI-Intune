package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func TestSetDeploymentFinishedRejectsLiveJobsAndKeepsResourceOwner(t *testing.T) {
	s := rolloutStore(t)
	for _, id := range []string{"old-target", "new-target"} {
		addRolloutMachine(t, s, id, id, true)
	}
	first, firstJobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "finish-guard", Targets: []NewDeploymentTarget{
			{MachineID: "old-target", BatchNo: 1},
			{MachineID: "new-target", ExcludedReason: "conflict"},
		},
	})
	if err != nil || len(firstJobs) != 1 || firstJobs[0].MachineID != "old-target" {
		t.Fatalf("first deployment=%+v jobs=%+v err=%v", first, firstJobs, err)
	}

	changed, finishErr := s.SetDeploymentState(first.DeploymentID, DeploymentRunning, DeploymentFinished, time.Now().UTC())
	if finishErr == nil && changed {
		// This is the pre-fix exploit: releasing the resource slot lets a second
		// deployment open a disjoint job while the first NotStarted job is live.
		second, secondJobs, secondErr := s.CreateDeployment(NewDeployment{
			Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
			BatchSize: 1, CreatedBy: "finish-bypass", Targets: []NewDeploymentTarget{
				{MachineID: "old-target", ExcludedReason: "conflict"},
				{MachineID: "new-target", BatchNo: 1},
			},
		})
		t.Fatalf("running→finished released owner with a live job: second=%+v jobs=%+v err=%v",
			second, secondJobs, secondErr)
	}
	if changed || !errors.Is(finishErr, ErrDeploymentFinishNotReady) {
		t.Fatalf("unsafe finish changed=%v err=%v", changed, finishErr)
	}
	if got, err := s.Deployment(first.DeploymentID); err != nil || got.State != DeploymentRunning || got.FinishedAt != nil {
		t.Fatalf("rejected finish changed deployment: %+v err=%v", got, err)
	}
	_, _, err = s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "finish-bypass", Targets: []NewDeploymentTarget{
			{MachineID: "old-target", ExcludedReason: "conflict"},
			{MachineID: "new-target", BatchNo: 1},
		},
	})
	if !errors.Is(err, ErrDeploymentActiveResource) {
		t.Fatalf("rejected finish did not retain active resource owner: %v", err)
	}
}

func TestSetDeploymentFinishedRequiresEveryIncludedTargetOpenedAndSucceeded(t *testing.T) {
	for _, tc := range []struct {
		name          string
		batches       int
		firstTerminal deploy.JobState
	}{
		{name: "later target unopened", batches: 2, firstTerminal: deploy.Succeeded},
		{name: "opened target failed", batches: 1, firstTerminal: deploy.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			d, first := createBatchGuardFixture(t, s, tc.batches)
			setRolloutJobTerminal(t, s, first[0], tc.firstTerminal)
			changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentFinished, time.Now().UTC())
			if changed || !errors.Is(err, ErrDeploymentFinishNotReady) {
				t.Fatalf("finish incomplete deployment changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestSetDeploymentFinishedAllowsCompleteSuccessAndIgnoresExcludedTarget(t *testing.T) {
	s := rolloutStore(t)
	for _, id := range []string{"included", "excluded"} {
		addRolloutMachine(t, s, id, id, true)
	}
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 1, CreatedBy: "finish-success", Targets: []NewDeploymentTarget{
			{MachineID: "included", BatchNo: 1}, {MachineID: "excluded", ExcludedReason: "missing_package"},
		},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("create deployment=%+v jobs=%+v err=%v", d, jobs, err)
	}
	setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentFinished, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("complete success finish changed=%v err=%v", changed, err)
	}
}

func TestSetDeploymentFinishedRejectsLegacyZeroTargetPlan(t *testing.T) {
	s := rolloutStore(t)
	insertLegacyDeployment(t, s, "zero-target", "canary", "openclaw", "openclaw", DeploymentRunning, 1)
	changed, err := s.SetDeploymentState("zero-target", DeploymentRunning, DeploymentFinished, time.Now().UTC())
	if changed || !errors.Is(err, ErrDeploymentInvalidBatchPlan) {
		t.Fatalf("zero-target finish changed=%v err=%v", changed, err)
	}
	if d, err := s.Deployment("zero-target"); err != nil || d.State != DeploymentRunning {
		t.Fatalf("zero-target rejection released owner: deployment=%+v err=%v", d, err)
	}
}

func TestPausedStablePromoteLockCanBeAbandonedToRerunCanary(t *testing.T) {
	s := rolloutStore(t)
	d, first := createBatchGuardFixture(t, s, 2)
	setRolloutJobTerminal(t, s, first[0], deploy.Succeeded)
	relabelDeploymentStable(t, s, d)
	pausedAt := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	if changed, err := s.PauseDeploymentAtBoundary(
		d.DeploymentID, 1, DeploymentPausePromoteLocked, "canary evidence became locked", pausedAt,
	); err != nil || !changed {
		t.Fatalf("pause stable boundary changed=%v err=%v", changed, err)
	}
	if _, _, err := s.ContinueDeployment(d.DeploymentID, pausedAt.Add(time.Minute)); !errors.Is(err, ErrPromoteLocked) {
		t.Fatalf("paused stable Continue should remain promote-locked: %v", err)
	}

	canary := NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: d.Spec,
		BatchSize: 2, CreatedBy: "fresh-canary", Targets: []NewDeploymentTarget{
			{MachineID: "machine-1", BatchNo: 1}, {MachineID: "machine-2", BatchNo: 1},
		},
		Job: NewJob{ArtifactDigest: "sha256:" + strings.Repeat("5", 64), ExecutionTimeout: 600},
	}
	if _, _, err := s.CreateDeployment(canary); !errors.Is(err, ErrDeploymentActiveResource) {
		t.Fatalf("paused stable owner did not reproduce canary deadlock: %v", err)
	}

	abandonedAt := pausedAt.Add(2 * time.Minute)
	s.nowFn = func() time.Time { return abandonedAt }
	abandoned, err := s.AbandonDeployment(d.DeploymentID, pausedAt.Add(-time.Hour))
	if err != nil || abandoned.State != DeploymentFinished || abandoned.FinishedAt == nil || !abandoned.FinishedAt.Equal(abandonedAt) {
		t.Fatalf("abandon=%+v err=%v", abandoned, err)
	}
	if abandoned.PausedAt == nil || !abandoned.PausedAt.Equal(pausedAt) {
		t.Fatalf("abandon erased historical pause time: %+v", abandoned)
	}
	view, err := s.DeploymentView(d.DeploymentID, abandonedAt)
	if err != nil || view.BoundaryPause == nil || view.BoundaryPause.Kind != DeploymentPausePromoteLocked {
		t.Fatalf("abandon erased boundary evidence: view=%+v err=%v", view, err)
	}
	events, err := s.HubEventsBetween(abandonedAt, abandonedAt)
	if err != nil || len(events) != 1 || events[0].Kind != HubDeploymentAbandoned ||
		!strings.Contains(events[0].Detail, d.DeploymentID) {
		t.Fatalf("abandon event=%+v err=%v", events, err)
	}
	if fresh, jobs, err := s.CreateDeployment(canary); err != nil || fresh.State != DeploymentRunning || len(jobs) != 2 {
		t.Fatalf("fresh canary after abandon=%+v jobs=%+v err=%v", fresh, jobs, err)
	}
}

func TestAbandonDeploymentRejectsRunningOrPausedWithLiveJob(t *testing.T) {
	s := rolloutStore(t)
	d, _ := createBatchGuardFixture(t, s, 1)
	if _, err := s.AbandonDeployment(d.DeploymentID, time.Now().UTC()); !errors.Is(err, ErrDeploymentNotPaused) {
		t.Fatalf("abandon running deployment err=%v", err)
	}
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause deployment changed=%v err=%v", changed, err)
	}
	if _, err := s.AbandonDeployment(d.DeploymentID, time.Now().UTC()); !errors.Is(err, ErrDeploymentAbandonActiveJobs) {
		t.Fatalf("abandon paused live job err=%v", err)
	}
	if got, err := s.Deployment(d.DeploymentID); err != nil || got.State != DeploymentPaused {
		t.Fatalf("rejected abandon changed deployment: %+v err=%v", got, err)
	}
}

func TestAbandonDeploymentEventFailureRollsBackOwnerRelease(t *testing.T) {
	s := rolloutStore(t)
	d, jobs := createBatchGuardFixture(t, s, 1)
	setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause changed=%v err=%v", changed, err)
	}
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_abandon_event
 BEFORE INSERT ON hub_events WHEN NEW.kind='deployment_abandoned'
 BEGIN SELECT RAISE(ABORT, 'injected abandon event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AbandonDeployment(d.DeploymentID, time.Now().UTC()); err == nil ||
		!strings.Contains(err.Error(), "injected abandon event failure") {
		t.Fatalf("abandon event failure err=%v", err)
	}
	if got, err := s.Deployment(d.DeploymentID); err != nil || got.State != DeploymentPaused || got.FinishedAt != nil {
		t.Fatalf("event failure did not roll back owner: deployment=%+v err=%v", got, err)
	}
}
