package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/store"
)

type jobVerdict struct {
	job    store.ReapableJob
	event  deploy.Event
	kind   string
	detail string
}

// judgeJobs 只依一輪查到的材料下判決，不讀資料庫也不看真實時鐘。
func judgeJobs(jobs []store.ReapableJob, now time.Time, lease time.Duration) []jobVerdict {
	var out []jobVerdict
	for _, job := range jobs {
		if job.LeaseExpiresAt != nil && job.LeaseExpiresAt.Before(now) {
			out = append(out, jobVerdict{
				job: job, event: deploy.LeaseLost, kind: store.JobLeaseExpired,
				detail: fmt.Sprintf("%s %s 在 %s 時租約過期於 %s",
					job.JobID, job.MachineID, job.State, job.LeaseExpiresAt.UTC().Format(time.RFC3339)),
			})
			continue
		}
		if job.StartedAt == nil {
			continue
		}
		// ⚠ 2× execution_timeout 不是任意緩衝：agent executor 的 health 最壞會
		// 用完整份預算，接著 rollbackWithOwnBudget 回退再拿一次同樣的預算；
		// 最後再留一個租約長度給網路。少掉第二份預算會把仍在回退的 agent
		// 判成逾時，Hub 與 agent 會同時搶著寫相反的結論。
		deadline := job.StartedAt.Add(2*time.Duration(job.ExecutionTimeout)*time.Second + lease)
		if now.After(deadline) {
			out = append(out, jobVerdict{
				job: job, event: deploy.Timeout, kind: store.JobTimeout,
				detail: fmt.Sprintf("%s %s 在 %s 時超過執行期限（Hub 於 %s 收到 start，期限 %s）",
					job.JobID, job.MachineID, job.State, job.StartedAt.UTC().Format(time.RFC3339), deadline.UTC().Format(time.RFC3339)),
			})
		}
	}
	return out
}

// reapJobs 執行一輪判決。判決與 agent 撞在同一瞬間時，舊狀態守衛會讓
// AdvanceJobByHub 回 ErrJobNotFound；那代表 agent 已經先落地，不是 reaper 壞了。
func (h *hub) reapJobs(now time.Time) {
	jobs, err := h.store.ReapableJobs()
	if err != nil {
		log.Printf("工作單 reaper：讀取待判決工作單失敗：%v", err)
		return
	}
	h.applyJobVerdicts(judgeJobs(jobs, now.UTC(), jobLeaseDuration), now)
}

func (h *hub) applyJobVerdicts(verdicts []jobVerdict, now time.Time) {
	for _, verdict := range verdicts {
		if _, err := h.store.AdvanceJobByHub(verdict.job.JobID, verdict.event, now, verdict.job.State); err != nil {
			if errors.Is(err, store.ErrJobNotFound) {
				log.Printf("工作單 reaper 讓給 agent：job=%s 原狀態=%s", verdict.job.JobID, verdict.job.State)
				continue
			}
			log.Printf("工作單 reaper 判決失敗：job=%s event=%s：%v", verdict.job.JobID, verdict.event, err)
			continue
		}
		if err := h.store.RecordHubEvent(verdict.kind, verdict.detail, now.UTC()); err != nil {
			log.Printf("工作單 reaper 寫不進 Hub 自己的日誌：job=%s kind=%s：%v",
				verdict.job.JobID, verdict.kind, err)
		}
	}
}

// jobReaperLoop 的節奏只從租約長度導出。runJobReaperLoop 把 ticker 與 now
// 注入，讓「時間到了真的會落地」不必睡真實的一分鐘才能測。
func (h *hub) jobReaperLoop(ctx context.Context) {
	ticker := time.NewTicker(jobLeaseDuration / 2)
	defer ticker.Stop()
	h.runJobReaperLoop(ctx, ticker.C, jobNow)
}

func (h *hub) runJobReaperLoop(ctx context.Context, ticks <-chan time.Time, now func() time.Time) {
	for {
		at := now()
		h.reapJobs(at)
		h.advanceDeployments(at)
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}
