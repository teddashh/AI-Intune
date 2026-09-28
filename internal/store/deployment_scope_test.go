package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func deploymentScopeInput(machineIDs []string, batchSize int, excluded map[string]bool) NewDeployment {
	targets := make([]NewDeploymentTarget, 0, len(machineIDs))
	included := 0
	for _, machineID := range machineIDs {
		if excluded[machineID] {
			targets = append(targets, NewDeploymentTarget{MachineID: machineID, ExcludedReason: "conflict"})
			continue
		}
		included++
		targets = append(targets, NewDeploymentTarget{
			MachineID: machineID,
			BatchNo:   (included-1)/batchSize + 1,
		})
	}
	return NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec: rolloutTestOpenClawSpec, BatchSize: batchSize, CreatedBy: "scope-owner-test",
		Targets: targets,
	}
}

func insertLegacyDeployment(t *testing.T, s *Store, id, channel, resourceKind, resourceID, state string, revision int) {
	t.Helper()
	desiredID := "desired-" + id
	now := fmtTime(time.Date(2026, 9, 6, 12, revision, 0, 0, time.UTC))
	if _, err := s.DB().Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES (?,?,?,?,?,?,?,?,?)`, desiredID, "channel", channel, resourceKind, resourceID, revision,
		rolloutTestOpenClawSpec, now, "legacy-fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO deployments
 (deployment_id,channel,desired_id,resource_kind,resource_id,revision,batch_size,state,created_at,created_by)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, id, channel, desiredID, resourceKind, resourceID, revision, 5, state, now,
		"legacy-fixture"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateDeploymentRejectsSecondActiveResourceAcrossDisjointTargets(t *testing.T) {
	s := rolloutStore(t)
	machines := make([]string, 10)
	for i := range machines {
		machines[i] = fmt.Sprintf("machine-%02d", i+1)
		addRolloutMachine(t, s, machines[i], machines[i], true)
	}

	first, firstJobs, err := s.CreateDeployment(deploymentScopeInput(machines, 5, nil))
	if err != nil || len(firstJobs) != 5 {
		t.Fatalf("first deployment=%+v jobs=%d err=%v", first, len(firstJobs), err)
	}
	excluded := make(map[string]bool, 5)
	for _, machineID := range machines[:5] {
		excluded[machineID] = true
	}
	before := snapshotRetryLedger(t, s)
	_, secondJobs, err := s.CreateDeployment(deploymentScopeInput(machines, 5, excluded))
	if !errors.Is(err, ErrDeploymentActiveResource) || !strings.Contains(err.Error(), "active deployment") {
		t.Fatalf("second disjoint deployment jobs=%+v err=%v", secondJobs, err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("rejected second deployment wrote ledger: before=%+v after=%+v", before, after)
	}
	var active int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE state NOT IN (?,?,?,?,?)`,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != first.BatchSize {
		t.Fatalf("active jobs=%d, batch_size=%d", active, first.BatchSize)
	}
}

func TestConcurrentDeploymentCreatesHaveOneResourceOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent-owner.db")
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

	machines := make([]string, 10)
	for i := range machines {
		machines[i] = fmt.Sprintf("concurrent-machine-%02d", i+1)
		addRolloutMachine(t, first, machines[i], machines[i], true)
	}
	leftExcluded, rightExcluded := map[string]bool{}, map[string]bool{}
	for i, machineID := range machines {
		if i < 5 {
			rightExcluded[machineID] = true
		} else {
			leftExcluded[machineID] = true
		}
	}
	inputs := []NewDeployment{
		deploymentScopeInput(machines, 5, leftExcluded),
		deploymentScopeInput(machines, 5, rightExcluded),
	}
	stores := []*Store{first, second}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := range stores {
		go func(i int) {
			<-start
			_, _, err := stores[i].CreateDeployment(inputs[i])
			results <- err
		}(i)
	}
	close(start)
	var succeeded, rejected int
	for range stores {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrDeploymentActiveResource):
			rejected++
		default:
			t.Fatalf("unexpected concurrent create error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent creates succeeded=%d rejected=%d", succeeded, rejected)
	}
	var activeDeployments, activeJobs int
	if err := first.DB().QueryRow(`SELECT COUNT(*) FROM deployments WHERE state IN (?,?)`,
		DeploymentRunning, DeploymentPaused).Scan(&activeDeployments); err != nil {
		t.Fatal(err)
	}
	if err := first.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE state NOT IN (?,?,?,?,?)`,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention).Scan(&activeJobs); err != nil {
		t.Fatal(err)
	}
	if activeDeployments != 1 || activeJobs != 5 {
		t.Fatalf("active deployments=%d jobs=%d", activeDeployments, activeJobs)
	}
}

func TestRetryAtomicallyFinishesPausedParentBeforeOpeningChild(t *testing.T) {
	s := rolloutStore(t)
	machines := make([]string, 10)
	for i := range machines {
		machines[i] = fmt.Sprintf("retry-machine-%02d", i+1)
		addRolloutMachine(t, s, machines[i], machines[i], true)
	}
	parent, firstJobs, err := s.CreateDeployment(deploymentScopeInput(machines, 5, nil))
	if err != nil {
		t.Fatal(err)
	}
	failedAt := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	for _, job := range firstJobs {
		if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
			deploy.Failed, fmtTime(failedAt), job.JobID); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := s.SetDeploymentState(parent.DeploymentID, DeploymentRunning, DeploymentPaused, failedAt); err != nil || !changed {
		t.Fatalf("pause parent changed=%v err=%v", changed, err)
	}

	retryAt := failedAt.Add(time.Hour)
	s.nowFn = func() time.Time { return retryAt }
	retryInput := deploymentScopeInput(machines[:5], 5, nil)
	retryInput.RetryOf = parent.DeploymentID
	retry, retryJobs, err := s.CreateDeployment(retryInput)
	if err != nil || len(retryJobs) != 5 {
		t.Fatalf("retry=%+v jobs=%d err=%v", retry, len(retryJobs), err)
	}
	storedParent, err := s.Deployment(parent.DeploymentID)
	if err != nil || storedParent.State != DeploymentFinished || storedParent.FinishedAt == nil || !storedParent.FinishedAt.Equal(retryAt) {
		t.Fatalf("parent was not atomically superseded: parent=%+v err=%v", storedParent, err)
	}
	if _, jobs, err := s.ContinueDeployment(parent.DeploymentID, retryAt.Add(time.Minute)); !errors.Is(err, ErrDeploymentNotPaused) || len(jobs) != 0 {
		t.Fatalf("superseded parent continued jobs=%+v err=%v", jobs, err)
	}
	var active int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE state NOT IN (?,?,?,?,?)`,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != len(retryJobs) || active > retry.BatchSize {
		t.Fatalf("retry fork exceeded batch size: active=%d retry_jobs=%d batch_size=%d", active, len(retryJobs), retry.BatchSize)
	}
}

func TestFailedRetryCreationRollsBackParentSupersession(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "retry-conflict", "retry-conflict", true)
	parent, parentJobs, err := s.CreateDeployment(deploymentScopeInput([]string{"retry-conflict"}, 1, nil))
	if err != nil {
		t.Fatal(err)
	}
	failedAt := time.Date(2026, 9, 6, 14, 0, 0, 0, time.UTC)
	setRolloutJobTerminal(t, s, parentJobs[0], deploy.Failed)
	if changed, err := s.SetDeploymentState(parent.DeploymentID, DeploymentRunning, DeploymentPaused, failedAt); err != nil || !changed {
		t.Fatalf("pause parent changed=%v err=%v", changed, err)
	}
	desiredID, revision, err := s.CreateDesiredState("machine", "retry-conflict", "openclaw", "openclaw",
		`{"kind":"noop"}`, "retry-conflict")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob("retry-conflict", desiredID, revision, NewJob{}); err != nil {
		t.Fatal(err)
	}

	before := snapshotRetryLedger(t, s)
	retry := deploymentScopeInput([]string{"retry-conflict"}, 1, nil)
	retry.RetryOf = parent.DeploymentID
	if _, _, err := s.CreateDeployment(retry); !errors.Is(err, ErrDeploymentConflict) {
		t.Fatalf("conflicting retry err=%v", err)
	}
	if after := snapshotRetryLedger(t, s); after != before {
		t.Fatalf("failed retry left partial ledger: before=%+v after=%+v", before, after)
	}
	stored, err := s.Deployment(parent.DeploymentID)
	if err != nil || stored.State != DeploymentPaused || stored.FinishedAt != nil {
		t.Fatalf("failed child did not roll back parent transition: parent=%+v err=%v", stored, err)
	}
}

func TestDelayedDeploymentRejectsLegacyDuplicateActiveResourceOwner(t *testing.T) {
	tests := []struct {
		name           string
		useContinue    bool
		competingFirst bool
	}{
		{name: "open/competing first", competingFirst: true},
		{name: "open/self first"},
		{name: "continue/competing first", useContinue: true, competingFirst: true},
		{name: "continue/self first", useContinue: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := rolloutStore(t)
			d, jobs := createBatchGuardFixture(t, s, 2)
			terminalState := deploy.Succeeded
			if tt.useContinue {
				terminalState = deploy.Failed
			}
			setRolloutJobTerminal(t, s, jobs[0], terminalState)
			if tt.useContinue {
				if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, time.Now().UTC()); err != nil || !changed {
					t.Fatalf("pause changed=%v err=%v", changed, err)
				}
			}
			if _, err := s.DB().Exec(`DROP INDEX IF EXISTS ` + activeDeploymentResourceIndex); err != nil {
				t.Fatal(err)
			}
			insertLegacyDeployment(t, s, "legacy-competing", "stable", d.ResourceKind, d.ResourceID, DeploymentRunning, int(d.Revision)+1)
			if !tt.competingFirst {
				if _, err := s.DB().Exec(`UPDATE deployments SET created_at=? WHERE deployment_id=?`,
					fmtTime(d.CreatedAt.Add(time.Second)), "legacy-competing"); err != nil {
					t.Fatal(err)
				}
				var firstOwner string
				if err := s.DB().QueryRow(`SELECT deployment_id FROM deployments
 WHERE resource_kind=? AND resource_id=? AND state IN (?,?)
 ORDER BY created_at,deployment_id LIMIT 1`, d.ResourceKind, d.ResourceID, DeploymentRunning, DeploymentPaused).Scan(&firstOwner); err != nil {
					t.Fatal(err)
				}
				if firstOwner != d.DeploymentID {
					t.Fatalf("first active resource owner=%q, want self %q", firstOwner, d.DeploymentID)
				}
			}

			before := snapshotRetryLedger(t, s)
			var err error
			if tt.useContinue {
				_, _, err = s.ContinueDeployment(d.DeploymentID, time.Now().UTC())
			} else {
				_, err = s.OpenDeploymentBatch(d.DeploymentID, 2, time.Now().UTC())
			}
			if !errors.Is(err, ErrDeploymentActiveResource) || !strings.Contains(err.Error(), "active deployment") {
				t.Fatalf("legacy duplicate owner err=%v", err)
			}
			if after := snapshotRetryLedger(t, s); after != before {
				t.Fatalf("legacy duplicate owner wrote ledger: before=%+v after=%+v", before, after)
			}
			if kind, ok := DeploymentBoundaryPauseKind(err); !ok || kind != DeploymentPauseConflict {
				t.Fatalf("legacy duplicate owner is not a deterministic pause: kind=%q ok=%v err=%v", kind, ok, err)
			}
		})
	}
}

func TestActiveDeploymentPartialIndexIsDatabaseBackstop(t *testing.T) {
	s := rolloutStore(t)
	insertLegacyDeployment(t, s, "first-active", "canary", "app", "shared", DeploymentRunning, 1)
	desiredID := "desired-second-active"
	if _, err := s.DB().Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES (?,?,?,?,?,?,?,?,?)`, desiredID, "channel", "stable", "app", "shared", 2, `{"kind":"app"}`,
		fmtTime(time.Now().UTC()), "unique-index-test"); err != nil {
		t.Fatal(err)
	}
	_, err := s.DB().Exec(`INSERT INTO deployments
 (deployment_id,channel,desired_id,resource_kind,resource_id,revision,batch_size,state,created_at,created_by)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, "second-active", "stable", desiredID, "app", "shared", 2, 1,
		DeploymentPaused, fmtTime(time.Now().UTC()), "unique-index-test")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("partial unique index allowed cross-channel active duplicate: %v", err)
	}
}

func TestOpenRejectsDuplicateActiveDeploymentsWithoutSuggestingUnreachableTransition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-duplicate.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP INDEX IF EXISTS ` + activeDeploymentResourceIndex); err != nil {
		t.Fatal(err)
	}
	insertLegacyDeployment(t, s, "legacy-canary", "canary", "openclaw", "openclaw", DeploymentRunning, 1)
	insertLegacyDeployment(t, s, "legacy-stable", "stable", "openclaw", "openclaw", DeploymentPaused, 2)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if reopened != nil {
		_ = reopened.Close()
	}
	if !errors.Is(err, ErrDeploymentActiveResource) || !strings.Contains(err.Error(), "openclaw:openclaw") ||
		!strings.Contains(err.Error(), "legacy-canary") || !strings.Contains(err.Error(), "legacy-stable") {
		t.Fatalf("legacy duplicate migration did not fail clearly: %v", err)
	}
	for _, misleadingAction := range []string{"legacy ledger", "再升級", "abandon", "finish", "continue", "收成"} {
		if strings.Contains(strings.ToLower(err.Error()), misleadingAction) {
			t.Errorf("duplicate active deployments suggested unreachable action %q: %v", misleadingAction, err)
		}
	}
}
