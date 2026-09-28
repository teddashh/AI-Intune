package main

import (
	"context"
	"errors"
	"log"
	"time"
)

const artifactFetchWorkerInterval = time.Second

var errArtifactFetchLifecycleUnavailable = errors.New("artifact fetch lifecycle is unavailable")

type artifactFetchLifecycle interface {
	RecoverArtifactFetchOperations(context.Context) (int, error)
	RunQueuedArtifactFetchOperations(context.Context) (int, error)
}

func recoverArtifactFetchWorker(ctx context.Context, worker artifactFetchLifecycle) (int, error) {
	if worker == nil {
		return 0, errArtifactFetchLifecycleUnavailable
	}
	return worker.RecoverArtifactFetchOperations(ctx)
}

func artifactFetchWorkerLoop(ctx context.Context, worker artifactFetchLifecycle) {
	ticker := time.NewTicker(artifactFetchWorkerInterval)
	defer ticker.Stop()
	runArtifactFetchWorkerLoop(ctx, worker, ticker.C, log.Printf)
}

// runArtifactFetchWorkerLoop keeps time and logging injectable. Error values
// are never passed to logf: registry paths, local paths, and transport errors
// are worker-private even when a drain fails.
func runArtifactFetchWorkerLoop(ctx context.Context, worker artifactFetchLifecycle,
	ticks <-chan time.Time, logf func(string, ...any),
) {
	if ctx == nil || worker == nil || ticks == nil {
		return
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			processed, err := worker.RunQueuedArtifactFetchOperations(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logf("artifact fetch queue drain failed; private detail suppressed; will retry")
				continue
			}
			if processed > 0 {
				logf("artifact fetch queue completed %d operation(s)", processed)
			}
		}
	}
}
