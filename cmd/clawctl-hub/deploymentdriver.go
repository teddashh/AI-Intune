package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/store"
)

// advanceDeployments 做一輪批次判決。SQLite 的 target.job_id 是冪等鍵，
// 所以同一輪連跑兩次，第二次只會讀回既有 job，不會生第二張。
func (h *hub) advanceDeployments(now time.Time) {
	views, err := h.store.RunningDeployments(now)
	if err != nil {
		log.Printf("deployment driver: failed to read running deployments: %v", err)
		return
	}
	for _, view := range views {
		var current []rollout.JobState
		for _, target := range view.Targets {
			if target.JobID != "" && target.BatchNo == view.OpenedBatch {
				current = append(current, rollout.JobState{MachineID: target.MachineID, State: target.JobState})
			}
		}
		decision := rollout.Decide(current, view.OpenedBatch < view.TotalBatches)
		switch decision.Action {
		case rollout.Wait:
			continue
		case rollout.Pause:
			changed, err := h.store.SetDeploymentState(view.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, now)
			if err != nil {
				log.Printf("deployment driver: failed to pause %s: %v", view.DeploymentID, err)
				continue
			}
			if changed {
				detail := fmt.Sprintf("deployment %s 批次 %d 失敗即停；machines=%s", view.DeploymentID, view.OpenedBatch, strings.Join(decision.StuckMachineIDs, ","))
				if err := h.store.RecordHubEvent(store.HubDeploymentPaused, detail, now); err != nil {
					log.Printf("deployment driver: failed to record pause event: %v", err)
				}
			}
		case rollout.OpenNext:
			if view.PauseAfterCanary && view.OpenedBatch == 1 {
				changed, err := h.store.SetDeploymentState(view.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, now)
				if err != nil {
					log.Printf("deployment driver: canary hold failed to pause %s: %v", view.DeploymentID, err)
					continue
				}
				if changed {
					detail := fmt.Sprintf("deployment %s canary batch succeeded；expansion waits for explicit Continue", view.DeploymentID)
					if err := h.store.RecordHubEvent(store.HubDeploymentCanaryHeld, detail, now); err != nil {
						log.Printf("deployment driver: failed to record canary hold event: %v", err)
					}
				}
				continue
			}
			if _, err := h.store.OpenDeploymentBatch(view.DeploymentID, view.OpenedBatch+1, now); err != nil {
				kind, deterministic := store.DeploymentBoundaryPauseKind(err)
				if !deterministic {
					log.Printf("deployment driver: %s failed to open batch %d, skipping this round: %v",
						view.DeploymentID, view.OpenedBatch+1, err)
					continue
				}
				changed, pauseErr := h.store.PauseDeploymentAtBoundary(
					view.DeploymentID, view.OpenedBatch, kind, err.Error(), now)
				if pauseErr != nil {
					log.Printf("deployment driver: batch %d blocked by %s, but failed to pause %s: %v",
						view.OpenedBatch+1, kind, view.DeploymentID, pauseErr)
					continue
				}
				if changed {
					log.Printf("deployment driver: %s paused by safety boundary after batch %d: %v",
						view.DeploymentID, view.OpenedBatch, err)
				}
			}
		case rollout.Finish:
			changed, err := h.store.SetDeploymentState(view.DeploymentID, store.DeploymentRunning, store.DeploymentFinished, now)
			if err != nil {
				log.Printf("deployment driver: failed to finish %s: %v", view.DeploymentID, err)
				continue
			}
			if changed {
				if err := h.store.RecordHubEvent(store.HubDeploymentFinished,
					fmt.Sprintf("deployment %s 所有已規劃批次完成", view.DeploymentID), now); err != nil {
					log.Printf("deployment driver: failed to record finished event: %v", err)
				}
			}
		}
	}
}
