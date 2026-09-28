package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeArtifactFetchLifecycle struct {
	recover func(context.Context) (int, error)
	drain   func(context.Context) (int, error)
}

func (f *fakeArtifactFetchLifecycle) RecoverArtifactFetchOperations(ctx context.Context) (int, error) {
	if f.recover == nil {
		return 0, nil
	}
	return f.recover(ctx)
}

func (f *fakeArtifactFetchLifecycle) RunQueuedArtifactFetchOperations(ctx context.Context) (int, error) {
	if f.drain == nil {
		return 0, nil
	}
	return f.drain(ctx)
}

func TestRecoverArtifactFetchWorkerBlocksStartupUntilComplete(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	worker := &fakeArtifactFetchLifecycle{recover: func(ctx context.Context) (int, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-release:
			return 3, nil
		}
	}}
	result := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		count, err := recoverArtifactFetchWorker(t.Context(), worker)
		result <- struct {
			count int
			err   error
		}{count, err}
	}()
	<-entered
	select {
	case got := <-result:
		t.Fatalf("startup recovery returned before release: %+v", got)
	default:
	}
	close(release)
	select {
	case got := <-result:
		if got.err != nil || got.count != 3 {
			t.Fatalf("startup recovery = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("startup recovery did not finish")
	}
}

func TestRecoverArtifactFetchWorkerRejectsMissingLifecycle(t *testing.T) {
	if _, err := recoverArtifactFetchWorker(t.Context(), nil); !errors.Is(err, errArtifactFetchLifecycleUnavailable) {
		t.Fatalf("nil lifecycle error = %v", err)
	}
}

func TestRunArtifactFetchWorkerLoopTicksAndLogsOnlySafeSummaries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	drainStarted := make(chan int, 3)
	call := 0
	worker := &fakeArtifactFetchLifecycle{drain: func(ctx context.Context) (int, error) {
		call++
		drainStarted <- call
		switch call {
		case 1:
			return 2, nil
		case 2:
			return 0, errors.New("GET https://registry.npmjs.org/private-token failed at /srv/private/artifacts")
		default:
			<-ctx.Done()
			return 0, ctx.Err()
		}
	}}
	var logMu sync.Mutex
	logs := []string{}
	logged := make(chan string, 2)
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		logMu.Lock()
		logs = append(logs, line)
		logMu.Unlock()
		logged <- line
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runArtifactFetchWorkerLoop(ctx, worker, ticks, logf)
	}()

	ticks <- time.Now()
	if got := <-drainStarted; got != 1 {
		t.Fatalf("first drain call = %d", got)
	}
	if line := <-logged; !strings.Contains(line, "2 operation") {
		t.Fatalf("success log = %q", line)
	}
	ticks <- time.Now()
	if got := <-drainStarted; got != 2 {
		t.Fatalf("second drain call = %d", got)
	}
	if line := <-logged; !strings.Contains(line, "private detail suppressed") {
		t.Fatalf("failure log = %q", line)
	}
	ticks <- time.Now()
	if got := <-drainStarted; got != 3 {
		t.Fatalf("third drain call = %d", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker loop did not stop after context cancellation")
	}

	logMu.Lock()
	joined := strings.Join(logs, "\n")
	logMu.Unlock()
	for _, private := range []string{"private-token", "/srv/private", "registry.npmjs.org", "GET https"} {
		if strings.Contains(joined, private) {
			t.Fatalf("worker log leaked %q: %s", private, joined)
		}
	}
	if len(logged) != 0 {
		t.Fatal("context cancellation produced a failure log")
	}
}

func TestRunArtifactFetchWorkerLoopStopsOnClosedTicker(t *testing.T) {
	ticks := make(chan time.Time)
	close(ticks)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runArtifactFetchWorkerLoop(t.Context(), &fakeArtifactFetchLifecycle{}, ticks, nil)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed ticker did not stop worker loop")
	}
}
