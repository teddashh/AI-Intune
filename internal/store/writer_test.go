package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestWritersSerializeWithoutBusy(t *testing.T) {
	s := newTestStore(t)
	const goroutines = 20
	const per = 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*per)
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for n := 0; n < per; n++ {
				tx, err := s.beginWrite(context.Background(), "hub_event")
				if err != nil {
					errs <- err
					return
				}
				_, err = tx.Exec(
					`INSERT INTO hub_events (at, kind, detail) VALUES (?, ?, ?)`,
					time.Now().UTC().Format(time.RFC3339), "started", fmt.Sprintf("g%d-n%d", g, n))
				if err != nil {
					_ = tx.Rollback()
					errs <- err
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("serialized write failed: %v", err)
	}
	if t.Failed() {
		return
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM hub_events`); got != goroutines*per {
		t.Fatalf("hub_events = %d, want %d", got, goroutines*per)
	}
}

func TestWriterQueueTimeoutIsBusy(t *testing.T) {
	s := newTestStore(t)
	s.SetWriterWait(50 * time.Millisecond)
	held, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("hold writer: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = held.Rollback()
		}
	}
	defer release()

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		started := time.Now()
		_, err := s.beginWrite(context.Background(), "timeout_probe")
		done <- result{err, time.Since(started)}
	}()
	select {
	case got := <-done:
		if !IsBusy(got.err) || !errors.Is(got.err, ErrWriterBusy) {
			t.Fatalf("writer queue timeout = %v, want IsBusy and ErrWriterBusy", got.err)
		}
		if errors.Is(got.err, context.Canceled) {
			t.Fatalf("timeout was reported as cancellation: %v", got.err)
		}
		if got.elapsed > 2*time.Second {
			t.Fatalf("writer queue timeout took %s", got.elapsed)
		}
	case <-time.After(2 * time.Second):
		release()
		<-done
		t.Fatal("beginWrite did not time out while the writer was held")
	}
}

func TestReaderProceedsWhileWriterIsHeld(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.DB().Exec(
		`INSERT INTO machine_registry (machine_id, display_name, created_at) VALUES (?, ?, ?)`,
		"m-committed", "m-committed", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	held, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("hold writer: %v", err)
	}
	defer held.Rollback()
	if _, err := held.Exec(
		`INSERT INTO machine_registry (machine_id, display_name, created_at) VALUES (?, ?, ?)`,
		"m-open", "m-open", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("uncommitted insert: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		var n int
		err := s.rdb.QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&n)
		if err != nil {
			done <- err
			return
		}
		if n != 1 {
			done <- fmt.Errorf("reader saw %d registry rows, want the committed row only", n)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader did not return within 1s while a writer transaction was held")
	}
}

func TestIsBusyRecognizesWriterTimeoutAndIgnoresCancellation(t *testing.T) {
	if !IsBusy(fmt.Errorf("wrapped: %w", ErrWriterBusy)) {
		t.Fatal("wrapped ErrWriterBusy was not busy")
	}
	if IsBusy(context.Canceled) || IsBusy(errors.New("disk I/O")) || IsBusy(nil) {
		t.Fatal("IsBusy was true for an error that is not lock contention")
	}
}
