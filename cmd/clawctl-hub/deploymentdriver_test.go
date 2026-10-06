package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func newDriverDeployment(t *testing.T, first, second []string) (jobsFixture, store.Deployment, []store.Job) {
	t.Helper()
	f := newJobsFixture(t, first[0])
	ids := map[string]string{first[0]: f.machine.id}
	for _, name := range append(append([]string{}, first[1:]...), second...) {
		ids[name] = enrollViaHTTP(t, f.mux, f.store, name).id
	}
	for _, id := range ids {
		if _, err := f.store.DB().Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	var targets []store.NewDeploymentTarget
	for _, name := range first {
		targets = append(targets, store.NewDeploymentTarget{MachineID: ids[name], BatchNo: 1})
	}
	for _, name := range second {
		targets = append(targets, store.NewDeploymentTarget{MachineID: ids[name], BatchNo: 2})
	}
	digest := strings.Repeat("a", 64)
	d, jobs, err := f.store.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		BatchSize: len(first), CreatedBy: "driver test", Targets: targets,
		Job: store.NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, d, jobs
}

func driverTargetInBatch(t *testing.T, st *store.Store, deploymentID string, batchNo int) store.DeploymentTarget {
	t.Helper()
	view, err := st.DeploymentView(deploymentID, jobsTestNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range view.Targets {
		if target.BatchNo == batchNo {
			return target
		}
	}
	t.Fatalf("deployment %s has no target in batch %d", deploymentID, batchNo)
	return store.DeploymentTarget{}
}

func TestCompleteHandlerOpensNextBatchImmediately(t *testing.T) {
	f, _, jobs := newDriverDeployment(t, []string{"first"}, []string{"second"})
	lease := claimJobViaHTTP(t, f, jobs[0].JobID)
	if _, err := f.store.AdvanceJobByAgent(jobs[0].JobID, jobs[0].MachineID, lease.LeaseToken, deploy.Start, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordVerification(jobs[0].JobID, jobs[0].MachineID, lease.LeaseToken,
		"rule", "true", 0, "", "", true, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobs[0].JobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete=%d %s", rec.Code, rec.Body.String())
	}
	all, _ := f.store.ListJobs("", 20)
	if len(all) != 2 {
		t.Fatalf("complete 回來前下一批尚未開：%+v", all)
	}
}

func TestRejectHandlerPausesImmediately(t *testing.T) {
	f, d, jobs := newDriverDeployment(t, []string{"first"}, []string{"second"})
	lease := claimJobViaHTTP(t, f, jobs[0].JobID)
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobs[0].JobID+"/reject", f.machine.token,
		model.JobRejectRequest{LeaseToken: lease.LeaseToken, RejectionCode: string(deploy.PreconditionFailed), Seq: 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject=%d %s", rec.Code, rec.Body.String())
	}
	v, _ := f.store.DeploymentView(d.DeploymentID, jobsTestNow)
	if v.State != store.DeploymentPaused || v.OpenedBatch != 1 {
		t.Fatalf("reject 回來前 deployment 尚未停：%+v", v)
	}
}

func TestVerificationFailurePausesImmediately(t *testing.T) {
	f, d, jobs := newDriverDeployment(t, []string{"first"}, []string{"second"})
	lease := claimJobViaHTTP(t, f, jobs[0].JobID)
	if _, err := f.store.AdvanceJobByAgent(jobs[0].JobID, jobs[0].MachineID, lease.LeaseToken, deploy.Start, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordVerification(jobs[0].JobID, jobs[0].MachineID, lease.LeaseToken,
		"rule", "false", 1, "", "", false, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobs[0].JobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete=%d %s", rec.Code, rec.Body.String())
	}
	v, _ := f.store.DeploymentView(d.DeploymentID, jobsTestNow)
	if v.State != store.DeploymentPaused || v.TerminalStuck != 1 {
		t.Fatalf("verification failure 回來前 deployment 尚未停：%+v", v)
	}
}

func succeedDriverJob(t *testing.T, st *store.Store, job store.Job, now time.Time) {
	t.Helper()
	token, err := st.ClaimJob(job.JobID, job.MachineID, now.Add(-time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(job.JobID, job.MachineID, token, deploy.Start, now.Add(-50*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordVerification(job.JobID, job.MachineID, token, "rule", "true", 0, "", "", true, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(job.JobID, job.MachineID, token, deploy.FinishWork, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkSucceededIfVerified(job.JobID, now); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentDriverFailurePausesAndDoesNotOpenNextBatch(t *testing.T) {
	f, d, jobs := newDriverDeployment(t, []string{"bad", "waiting"}, []string{"later"})
	now := jobsTestNow
	token, err := f.store.ClaimJob(jobs[0].JobID, jobs[0].MachineID, now.Add(-time.Minute), time.Hour)
	if err != nil || token == "" {
		t.Fatal(err)
	}
	if _, err := f.store.AdvanceJobByHub(jobs[0].JobID, deploy.Timeout, now); err != nil {
		t.Fatal(err)
	}
	(&hub{store: f.store}).advanceDeployments(now)
	v, err := f.store.DeploymentView(d.DeploymentID, now)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != store.DeploymentPaused || v.OpenedBatch != 1 || v.TerminalStuck != 1 {
		t.Fatalf("失敗後沒有停：%+v", v)
	}
	if got, _ := f.store.ListJobs("", 20); len(got) != 2 {
		t.Fatalf("後續批次被開了：%+v", got)
	}
	events, _ := f.store.HubEventsBetween(time.Time{}, now.Add(24*time.Hour))
	if len(events) != 1 || events[0].Kind != store.HubDeploymentPaused {
		t.Fatalf("pause event=%+v", events)
	}
}

func TestDeploymentDriverIsIdempotentAndFinishes(t *testing.T) {
	f, d, jobs := newDriverDeployment(t, []string{"first"}, []string{"second"})
	now := jobsTestNow
	succeedDriverJob(t, f.store, jobs[0], now)
	h := &hub{store: f.store}
	h.advanceDeployments(now)
	h.advanceDeployments(now)
	all, err := f.store.ListJobs("", 20)
	if err != nil || len(all) != 2 {
		t.Fatalf("同輪兩次應恰有兩張單：len=%d err=%v jobs=%+v", len(all), err, all)
	}
	var second store.Job
	for _, job := range all {
		if job.JobID != jobs[0].JobID {
			second = job
		}
	}
	succeedDriverJob(t, f.store, second, now.Add(time.Minute))
	h.advanceDeployments(now.Add(time.Minute))
	v, _ := f.store.DeploymentView(d.DeploymentID, now.Add(time.Minute))
	if v.State != store.DeploymentFinished || v.Counts[deploy.Succeeded] != 2 {
		t.Fatalf("最後一批成功沒 finished：%+v", v)
	}
}

func TestDeploymentDriverHoldsSucceededCanaryUntilExplicitContinue(t *testing.T) {
	f, d, jobs := newDriverDeployment(t, []string{"first"}, []string{"second"})
	if _, err := f.store.DB().Exec(`UPDATE deployments SET pause_after_canary=1 WHERE deployment_id=?`, d.DeploymentID); err != nil {
		t.Fatal(err)
	}
	now := jobsTestNow
	succeedDriverJob(t, f.store, jobs[0], now)
	h := &hub{store: f.store}
	h.advanceDeployments(now)
	h.advanceDeployments(now)
	v, err := f.store.DeploymentView(d.DeploymentID, now)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != store.DeploymentPaused || v.OpenedBatch != 1 || !v.PauseAfterCanary {
		t.Fatalf("canary hold did not pause after batch 1: %+v", v)
	}
	all, err := f.store.ListJobs("", 20)
	if err != nil || len(all) != 1 {
		t.Fatalf("canary hold opened the next batch: len=%d err=%v", len(all), err)
	}
	events, _ := f.store.HubEventsBetween(time.Time{}, now.Add(24*time.Hour))
	held := 0
	for _, event := range events {
		if event.Kind == store.HubDeploymentCanaryHeld {
			held++
		}
		if event.Kind == store.HubDeploymentContinued || event.Kind == store.HubDeploymentPaused {
			t.Fatalf("canary hold recorded %s: %+v", event.Kind, events)
		}
	}
	if held != 1 {
		t.Fatalf("canary hold events=%+v", events)
	}
}

func TestDeploymentDriverPausesDeterministicBoundaryBlocksWithOneVisibleEvent(t *testing.T) {
	tests := []struct {
		name       string
		wantKind   string
		wantReason string
		arrange    func(*testing.T, jobsFixture, store.Deployment)
	}{
		{
			name: "promote lock", wantKind: store.DeploymentPausePromoteLocked, wantReason: "promote 鎖著",
			arrange: func(t *testing.T, f jobsFixture, d store.Deployment) {
				if _, err := f.store.DB().Exec(`UPDATE deployments SET channel='stable' WHERE deployment_id=?`, d.DeploymentID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.DB().Exec(`UPDATE desired_state SET scope_id='stable' WHERE desired_id=?`, d.DesiredID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.DB().Exec(`UPDATE machine_registry SET channel='stable' WHERE channel='canary'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "active resource conflict", wantKind: store.DeploymentPauseConflict, wantReason: "active",
			arrange: func(t *testing.T, f jobsFixture, d store.Deployment) {
				target := driverTargetInBatch(t, f.store, d.DeploymentID, 2)
				desired, rev, err := f.store.CreateDesiredState("machine", target.MachineID, "openclaw", "openclaw", `{"kind":"noop"}`, "driver conflict test")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.CreateJob(target.MachineID, desired, rev, store.NewJob{}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "newer terminal revision", wantKind: store.DeploymentPauseStaleRevision, wantReason: "revision",
			arrange: func(t *testing.T, f jobsFixture, d store.Deployment) {
				target := driverTargetInBatch(t, f.store, d.DeploymentID, 2)
				desired, rev, err := f.store.CreateDesiredState("machine", target.MachineID, "openclaw", "openclaw", `{"kind":"noop"}`, "driver revision test")
				if err != nil {
					t.Fatal(err)
				}
				jobID, err := f.store.CreateJob(target.MachineID, desired, rev, store.NewJob{})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`, deploy.Succeeded, jobsTestNow.Format(time.RFC3339), jobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "deferred target changed channel", wantKind: store.DeploymentPauseTargetChanged, wantReason: "不再是未退場的 canary 成員",
			arrange: func(t *testing.T, f jobsFixture, d store.Deployment) {
				target := driverTargetInBatch(t, f.store, d.DeploymentID, 2)
				if _, err := f.store.DB().Exec(`UPDATE machine_registry SET channel='stable' WHERE machine_id=?`, target.MachineID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "legacy batch gap", wantKind: store.DeploymentPauseInvalidPlan, wantReason: "non-contiguous",
			arrange: func(t *testing.T, f jobsFixture, d store.Deployment) {
				if _, err := f.store.DB().Exec(`UPDATE deployment_targets SET batch_no=3
					WHERE deployment_id=? AND batch_no=2`, d.DeploymentID); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, d, jobs := newDriverDeployment(t, []string{"first"}, []string{"second"})
			succeedDriverJob(t, f.store, jobs[0], jobsTestNow)
			tc.arrange(t, f, d)
			h := &hub{store: f.store}
			h.advanceDeployments(jobsTestNow)

			view, err := f.store.DeploymentView(d.DeploymentID, jobsTestNow)
			if err != nil || view.State != store.DeploymentPaused || view.OpenedBatch != 1 || view.BoundaryPause == nil {
				t.Fatalf("deterministic block did not pause: view=%+v err=%v", view, err)
			}
			if view.BoundaryPause.Kind != tc.wantKind || !strings.Contains(view.BoundaryPause.Reason, tc.wantReason) {
				t.Fatalf("boundary evidence=%+v want kind=%q reason~%q", view.BoundaryPause, tc.wantKind, tc.wantReason)
			}
			events, err := f.store.HubEventsBetween(time.Time{}, jobsTestNow.Add(time.Hour))
			if err != nil || len(events) != 1 || events[0].Kind != store.HubDeploymentBoundaryPaused {
				t.Fatalf("boundary events=%+v err=%v", events, err)
			}
			h.advanceDeployments(jobsTestNow.Add(time.Minute))
			events, err = f.store.HubEventsBetween(time.Time{}, jobsTestNow.Add(time.Hour))
			if err != nil || len(events) != 1 {
				t.Fatalf("replayed driver tick duplicated boundary event: events=%+v err=%v", events, err)
			}
		})
	}
}

func TestDeploymentDriverDoesNotPauseAnUntypedDatabaseFailure(t *testing.T) {
	f, d, jobs := newDriverDeployment(t, []string{"first"}, []string{"second"})
	succeedDriverJob(t, f.store, jobs[0], jobsTestNow)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_driver_job_insert
		BEFORE INSERT ON jobs BEGIN SELECT RAISE(ABORT, 'injected database failure'); END`); err != nil {
		t.Fatal(err)
	}
	(&hub{store: f.store}).advanceDeployments(jobsTestNow)
	view, err := f.store.DeploymentView(d.DeploymentID, jobsTestNow)
	if err != nil || view.State != store.DeploymentRunning || view.OpenedBatch != 1 || view.BoundaryPause != nil {
		t.Fatalf("untyped DB failure paused deployment: view=%+v err=%v", view, err)
	}
	events, err := f.store.HubEventsBetween(time.Time{}, jobsTestNow.Add(time.Hour))
	if err != nil || len(events) != 0 {
		t.Fatalf("untyped DB failure wrote pause event: events=%+v err=%v", events, err)
	}
	if _, err := f.store.OpenDeploymentBatch(d.DeploymentID, 2, jobsTestNow); err == nil || errors.Is(err, store.ErrDeploymentBatchNotReady) {
		t.Fatalf("trigger did not produce an untyped storage failure: %v", err)
	}
}
