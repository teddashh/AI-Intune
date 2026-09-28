package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/store"
)

func timePtr(t time.Time) *time.Time { return &t }

func TestJudgeJobs(t *testing.T) {
	now := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
	lease := 2 * time.Minute
	tests := []struct {
		name string
		job  store.ReapableJob
		want deploy.Event
	}{
		{
			name: "租約過期",
			job: store.ReapableJob{JobID: "expired", MachineID: "m1", State: deploy.Claimed,
				LeaseExpiresAt: timePtr(now.Add(-time.Second))},
			want: deploy.LeaseLost,
		},
		{
			name: "可逆工作超過兩份預算加租約",
			job: store.ReapableJob{JobID: "timeout", MachineID: "m1", State: deploy.Running,
				ExecutionTimeout: 30, StartedAt: timePtr(now.Add(-lease - 61*time.Second)),
				LeaseExpiresAt: timePtr(now.Add(time.Minute))},
			want: deploy.Timeout,
		},
		{
			name: "不可逆工作由狀態機決定終態",
			job: store.ReapableJob{JobID: "irreversible", MachineID: "m1", State: deploy.Verifying,
				Irreversible: true, ExecutionTimeout: 30,
				StartedAt: timePtr(now.Add(-lease - 61*time.Second)), LeaseExpiresAt: timePtr(now.Add(time.Minute))},
			want: deploy.Timeout,
		},
		{
			name: "租約活著而且仍在預算內",
			job: store.ReapableJob{JobID: "working", MachineID: "m1", State: deploy.Running,
				ExecutionTimeout: 30, StartedAt: timePtr(now.Add(-time.Minute)),
				LeaseExpiresAt: timePtr(now.Add(time.Minute))},
		},
		{
			name: "claimed 還沒有 start 只看租約",
			job: store.ReapableJob{JobID: "not-started", MachineID: "m1", State: deploy.Claimed,
				ExecutionTimeout: 1, LeaseExpiresAt: timePtr(now.Add(time.Minute))},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := judgeJobs([]store.ReapableJob{test.job}, now, lease)
			if test.want == "" {
				if len(got) != 0 {
					t.Fatalf("預期零判決，拿到 %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].event != test.want {
				t.Fatalf("判決 = %+v，預期事件 %s", got, test.want)
			}
		})
	}
}

func createReaperJob(t *testing.T, f jobsFixture, irreversible bool, timeout int) (string, modelLease) {
	t.Helper()
	desiredID, rev, err := f.store.CreateDesiredState("machine", f.machine.id, "openclaw", "openclaw", `{}`, "reaper 測試")
	if err != nil {
		t.Fatalf("建立期望狀態失敗：%v", err)
	}
	jobID, err := f.store.CreateJob(f.machine.id, desiredID, rev, store.NewJob{
		ArtifactDigest: "sha256:a", Irreversible: irreversible, ExecutionTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("建立工作單失敗：%v", err)
	}
	token, err := f.store.ClaimJob(jobID, f.machine.id, jobsTestNow, jobLeaseDuration)
	if err != nil {
		t.Fatalf("領單失敗：%v", err)
	}
	return jobID, modelLease{token: token, expires: jobsTestNow.Add(jobLeaseDuration)}
}

type modelLease struct {
	token   string
	expires time.Time
}

func startReaperJob(t *testing.T, f jobsFixture, jobID, token string, at time.Time) {
	t.Helper()
	if err := f.store.AppendJobEvent(jobID, f.machine.id, token, 1, "start", `{}`, at.Add(-80*time.Second), at); err != nil {
		t.Fatalf("寫 start 事件失敗：%v", err)
	}
	if _, err := f.store.AdvanceJobByAgent(jobID, f.machine.id, token, deploy.Start, at); err != nil {
		t.Fatalf("推進 running 失敗：%v", err)
	}
}

func TestReaperPersistsLeaseExpiryAndHubEvent(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID, lease := createReaperJob(t, f, false, 30)
	now := lease.expires.Add(time.Second)
	(&hub{store: f.store}).reapJobs(now)
	job, err := f.store.Job(jobID)
	if err != nil {
		t.Fatalf("讀工作單失敗：%v", err)
	}
	if job.State != deploy.LeaseExpired || job.LeaseToken != "" || job.LeaseExpiresAt != nil {
		t.Fatalf("reaper 收尾結果不符：%+v", job)
	}
	events, err := f.store.HubEventsBetween(now.Add(-time.Second), now.Add(time.Second))
	if err != nil || len(events) != 1 || events[0].Kind != store.JobLeaseExpired {
		t.Fatalf("Hub 日誌不符：events=%+v err=%v", events, err)
	}
}

func TestReaperTimeoutUsesStateMachineForReversibleAndIrreversible(t *testing.T) {
	for _, test := range []struct {
		name         string
		irreversible bool
		want         deploy.JobState
	}{
		{"可逆", false, deploy.Failed},
		{"不可逆", true, deploy.ManualIntervention},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newJobsFixture(t, "machine-a")
			jobID, lease := createReaperJob(t, f, test.irreversible, 10)
			startReaperJob(t, f, jobID, lease.token, jobsTestNow)
			renewAt := lease.expires
			if err := f.store.RenewLease(jobID, f.machine.id, lease.token, renewAt, jobLeaseDuration); err != nil {
				t.Fatalf("續租失敗：%v", err)
			}
			now := jobsTestNow.Add(2*10*time.Second + jobLeaseDuration + time.Second)
			(&hub{store: f.store}).reapJobs(now)
			job, err := f.store.Job(jobID)
			if err != nil || job.State != test.want {
				t.Fatalf("逾時終態 = %s err=%v，預期 %s", job.State, err, test.want)
			}
		})
	}
}

func TestReaperYieldsToAgentTerminalWrite(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID, lease := createReaperJob(t, f, false, 30)
	stale := judgeJobs([]store.ReapableJob{{
		JobID: jobID, MachineID: f.machine.id, State: deploy.Claimed,
		LeaseExpiresAt: timePtr(lease.expires.Add(-time.Second)),
	}}, lease.expires, jobLeaseDuration)
	startReaperJob(t, f, jobID, lease.token, jobsTestNow)
	if _, err := f.store.AdvanceJobByAgent(jobID, f.machine.id, lease.token, deploy.FinishWork, jobsTestNow); err != nil {
		t.Fatalf("推進 verifying 失敗：%v", err)
	}
	if err := f.store.RecordVerification(jobID, f.machine.id, lease.token, "health", "check", 0, "ok", "", true, jobsTestNow); err != nil {
		t.Fatalf("寫驗證失敗：%v", err)
	}
	if _, err := f.store.MarkSucceededIfVerified(jobID, jobsTestNow); err != nil {
		t.Fatalf("agent 完成工作單失敗：%v", err)
	}
	(&hub{store: f.store}).applyJobVerdicts(stale, lease.expires)
	job, err := f.store.Job(jobID)
	if err != nil || job.State != deploy.Succeeded {
		t.Fatalf("reaper 改寫了 agent 終態：job=%+v err=%v", job, err)
	}
}

func TestReaperLoopRunsInjectedTickAndClock(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID, lease := createReaperJob(t, f, false, 30)
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	calls := make(chan int, 2)
	done := make(chan struct{})
	n := 0
	go func() {
		defer close(done)
		(&hub{store: f.store}).runJobReaperLoop(ctx, ticks, func() time.Time {
			n++
			calls <- n
			if n == 1 {
				return lease.expires.Add(-time.Second)
			}
			return lease.expires.Add(time.Second)
		})
	}()
	if call := <-calls; call != 1 {
		t.Fatalf("第一次 now 呼叫編號 = %d", call)
	}
	ticks <- lease.expires
	if call := <-calls; call != 2 {
		t.Fatalf("ticker 後 now 呼叫編號 = %d", call)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reaper loop 沒有在 context 取消後結束")
	}
	job, err := f.store.Job(jobID)
	if err != nil || job.State != deploy.LeaseExpired {
		t.Fatalf("注入時鐘的一輪沒有落地：job=%+v err=%v", job, err)
	}
}

func TestExpectedStateMakesReaperCollisionNotFound(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID, lease := createReaperJob(t, f, false, 30)
	startReaperJob(t, f, jobID, lease.token, jobsTestNow)
	if _, err := f.store.AdvanceJobByHub(jobID, deploy.LeaseLost, jobsTestNow, deploy.Claimed); !errors.Is(err, store.ErrJobNotFound) {
		t.Fatalf("舊 claimed 快照撞上 running 回傳 %v，預期 ErrJobNotFound", err)
	}
}
