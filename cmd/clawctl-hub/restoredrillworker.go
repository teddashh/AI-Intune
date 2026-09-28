package main

import (
	"context"
	"log"
	"time"
)

const restoreDrillWorkerInterval = time.Second

type restoreDrillLifecycle interface {
	RecoverRestoreDrillOperations(context.Context) (int, error)
	RunQueuedRestoreDrillOperations(context.Context) (int, error)
}

func recoverRestoreDrillWorker(ctx context.Context, worker restoreDrillLifecycle) (int, error) {
	if worker == nil {
		return 0, errArtifactFetchLifecycleUnavailable
	}
	return worker.RecoverRestoreDrillOperations(ctx)
}

func restoreDrillWorkerLoop(ctx context.Context, worker restoreDrillLifecycle) {
	ticker := time.NewTicker(restoreDrillWorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			processed, err := worker.RunQueuedRestoreDrillOperations(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Print("restore drill queue drain failed; private detail suppressed; will retry")
				continue
			}
			if processed > 0 {
				log.Printf("restore drill queue completed %d operation(s)", processed)
			}
		}
	}
}
