package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	in90s := now.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	in10m := now.Add(10 * time.Minute).UTC().Format(http.TimeFormat)
	past := now.Add(-time.Minute).UTC().Format(http.TimeFormat)
	same := now.UTC().Format(http.TimeFormat)
	rfc850 := now.Add(30 * time.Second).UTC().Format(time.RFC850)

	tests := []struct {
		name   string
		header string
		want   time.Duration
		ok     bool
	}{
		{name: "delta seconds", header: "12", want: 12 * time.Second, ok: true},
		{name: "leading spaces", header: "  15 ", want: 15 * time.Second, ok: true},
		{name: "zero", header: "0", want: 0, ok: true},
		{name: "zero with padding", header: "00", want: 0, ok: true},
		{name: "capped integer", header: "999999", want: retryAfterCap, ok: true},
		{name: "max int64 capped", header: "9223372036854775807", want: retryAfterCap, ok: true},
		{name: "http-date inside the cap", header: in90s, want: 90 * time.Second, ok: true},
		{name: "http-date capped", header: in10m, want: retryAfterCap, ok: true},
		{name: "http-date in the past", header: past, want: 0, ok: true},
		{name: "http-date equal to now", header: same, want: 0, ok: true},
		{name: "rfc850", header: rfc850, want: 30 * time.Second, ok: true},
		{name: "empty", header: "", ok: false},
		{name: "spaces", header: "   ", ok: false},
		{name: "garbage", header: "soon", ok: false},
		{name: "not a date", header: "not-a-date", ok: false},
		{name: "fraction", header: "12.5", ok: false},
		{name: "unit suffix", header: "12s", ok: false},
		{name: "negative", header: "-1", ok: false},
		{name: "leading plus", header: "+12", ok: false},
		{name: "overflow int64", header: "9223372036854775808", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tt.header, now)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("parseRetryAfter(%q) = %s, %v; want %s, %v", tt.header, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDoJSONStatusModeStoresRetryAfter(t *testing.T) {
	future := time.Now().Add(10 * time.Minute).UTC().Format(http.TimeFormat)
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	busy := `{"code":"HUB_BUSY","message":"writer is busy"}`

	tests := []struct {
		name    string
		status  int
		header  string
		set     bool
		body    string
		want    time.Duration
		has     bool
		wantErr string
	}{
		{name: "503 delta", status: http.StatusServiceUnavailable, header: "12", set: true, body: busy, want: 12 * time.Second, has: true, wantErr: "hub 503 HUB_BUSY: writer is busy"},
		{name: "503 trimmed", status: http.StatusServiceUnavailable, header: " 15 ", set: true, body: busy, want: 15 * time.Second, has: true},
		{name: "503 http-date capped", status: http.StatusServiceUnavailable, header: future, set: true, body: busy, want: retryAfterCap, has: true},
		{name: "503 past date", status: http.StatusServiceUnavailable, header: past, set: true, body: busy, want: 0, has: true},
		{name: "503 garbage", status: http.StatusServiceUnavailable, header: "soon", set: true, body: busy, has: false},
		{name: "503 missing", status: http.StatusServiceUnavailable, body: busy, has: false},
		{name: "429 delta", status: http.StatusTooManyRequests, header: "7", set: true, body: `{"code":"RATE_LIMIT","message":"slow down"}`, want: 7 * time.Second, has: true},
		{name: "500 ignores header", status: http.StatusInternalServerError, header: "12", set: true, body: "plain", has: false, wantErr: "hub 500: plain"},
		{name: "401 ignores header", status: http.StatusUnauthorized, header: "12", set: true, body: `{"code":"UNAUTHORIZED","message":"no"}`, has: false, wantErr: "hub 401 UNAUTHORIZED: no"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.set {
					w.Header().Set("Retry-After", tt.header)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			_, err := doJSONStatus(context.Background(), http.MethodPost, srv.URL+"/v1/checkins", "tok", map[string]int{"n": 1}, nil)
			var httpErr *hubHTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("error = %v, want *hubHTTPError", err)
			}
			if httpErr.StatusCode != tt.status || httpErr.HasRetryAfter != tt.has || httpErr.RetryAfter != tt.want {
				t.Fatalf("status=%d has=%v retry=%s; want status=%d has=%v retry=%s",
					httpErr.StatusCode, httpErr.HasRetryAfter, httpErr.RetryAfter, tt.status, tt.has, tt.want)
			}
			if tt.wantErr != "" && err.Error() != tt.wantErr {
				t.Fatalf("error text = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestBackoffCeilingsAndFullJitter(t *testing.T) {
	if got := jobPostBackoffCeiling(0); got != jobPostBackoffBase {
		t.Fatalf("attempt 0 ceiling = %s", got)
	}
	if got := jobPostBackoffCeiling(1); got != 500*time.Millisecond {
		t.Fatalf("attempt 1 = %s", got)
	}
	if got := jobPostBackoffCeiling(2); got != time.Second {
		t.Fatalf("attempt 2 = %s", got)
	}
	if got := jobPostBackoffCeiling(6); got != 16*time.Second {
		t.Fatalf("attempt 6 = %s", got)
	}
	if got := jobPostBackoffCeiling(7); got != jobPostBackoffCap {
		t.Fatalf("attempt 7 = %s", got)
	}
	if got := jobPostBackoffCeiling(100); got != jobPostBackoffCap {
		t.Fatalf("attempt 100 = %s", got)
	}

	wantPoll := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, pollBackoffCap}
	for i, want := range wantPoll {
		if got := pollBackoffCeiling(30*time.Second, i+1); got != want {
			t.Fatalf("30s failures=%d ceiling = %s, want %s", i+1, got, want)
		}
	}
	if got := pollBackoffCeiling(2*time.Minute, 1); got != 2*time.Minute {
		t.Fatalf("2m failures=1 = %s", got)
	}
	if got := pollBackoffCeiling(2*time.Minute, 2); got != 4*time.Minute {
		t.Fatalf("2m failures=2 = %s", got)
	}
	if got := pollBackoffCeiling(2*time.Minute, 3); got != pollBackoffCap {
		t.Fatalf("2m failures=3 = %s", got)
	}
	if got := pollBackoffCeiling(10*time.Minute, 1); got != pollBackoffCap {
		t.Fatalf("interval above the cap = %s", got)
	}
	if got := pollBackoffCeiling(0, 1); got != 30*time.Second {
		t.Fatalf("non-positive interval = %s", got)
	}
	if got := pollBackoffCeiling(-time.Second, 0); got != 30*time.Second {
		t.Fatalf("failures clamped = %s", got)
	}

	if got := fullJitter(0); got != 0 {
		t.Fatalf("zero ceiling = %s", got)
	}
	if got := fullJitter(-time.Millisecond); got != 0 {
		t.Fatalf("negative ceiling = %s", got)
	}
	const samples = 200
	ceiling := 50 * time.Millisecond
	seen := map[time.Duration]struct{}{}
	for i := 0; i < samples; i++ {
		got := fullJitter(ceiling)
		if got < 0 || got > ceiling {
			t.Fatalf("fullJitter(%s) = %s", ceiling, got)
		}
		seen[got] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatal("full jitter did not vary")
	}

	low := 8 * time.Second
	high := 12 * time.Second
	jitterSeen := map[time.Duration]struct{}{}
	for i := 0; i < 80; i++ {
		got := jitter(10 * time.Second)
		if got < low || got >= high {
			t.Fatalf("jitter(10s) = %s, want [%s, %s)", got, low, high)
		}
		jitterSeen[got] = struct{}{}
	}
	if len(jitterSeen) < 2 {
		t.Fatal("poll jitter did not vary")
	}
}

func TestHubBusyWaitAndRetryLine(t *testing.T) {
	jittered := 2 * time.Minute
	plain := errors.New("dial tcp: connection refused")
	if wait, floor, busy := hubBusyWait(jittered, nil); busy || floor != 0 || wait != jittered {
		t.Fatalf("nil error = %s, %s, %v", wait, floor, busy)
	}
	if wait, floor, busy := hubBusyWait(jittered, plain); busy || floor != 0 || wait != jittered {
		t.Fatalf("transport error = %s, %s, %v", wait, floor, busy)
	}
	cases := []struct {
		name  string
		err   error
		wait  time.Duration
		floor time.Duration
		busy  bool
	}{
		{name: "500", err: &hubHTTPError{StatusCode: 500, HasRetryAfter: true, RetryAfter: 30 * time.Second}, wait: jittered},
		{name: "429", err: &hubHTTPError{StatusCode: 429, HasRetryAfter: true, RetryAfter: 30 * time.Second}, wait: jittered},
		{name: "503 without header", err: &hubHTTPError{StatusCode: 503}, wait: jittered},
		{name: "503 garbage", err: &hubHTTPError{StatusCode: 503, HasRetryAfter: false}, wait: jittered},
		{name: "503 below the interval", err: &hubHTTPError{StatusCode: 503, HasRetryAfter: true, RetryAfter: 30 * time.Second}, wait: jittered, floor: 30 * time.Second, busy: true},
		{name: "503 above the interval", err: &hubHTTPError{StatusCode: 503, HasRetryAfter: true, RetryAfter: 3 * time.Minute}, wait: 3 * time.Minute, floor: 3 * time.Minute, busy: true},
		{name: "503 zero", err: &hubHTTPError{StatusCode: 503, HasRetryAfter: true, RetryAfter: 0}, wait: jittered, floor: 0, busy: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			wait, floor, busy := hubBusyWait(jittered, tt.err)
			if wait != tt.wait || floor != tt.floor || busy != tt.busy {
				t.Fatalf("got wait=%s floor=%s busy=%v; want wait=%s floor=%s busy=%v", wait, floor, busy, tt.wait, tt.floor, tt.busy)
			}
		})
	}
	if got := hubBusyRetryLine(0); got != "hub busy, retrying in 0s" {
		t.Fatalf("zero = %q", got)
	}
	if got := hubBusyRetryLine(2 * time.Second); got != "hub busy, retrying in 2s" {
		t.Fatalf("2s = %q", got)
	}
	if got := hubBusyRetryLine(1500 * time.Millisecond); got != "hub busy, retrying in 2s" {
		t.Fatalf("1.5s = %q", got)
	}
	if got := hubBusyRetryLine(time.Nanosecond); got != "hub busy, retrying in 1s" {
		t.Fatalf("1ns = %q", got)
	}
	if got := hubBusyRetryLine(5 * time.Minute); got != "hub busy, retrying in 300s" {
		t.Fatalf("5m = %q", got)
	}
}

func TestWaitNextObservationHonorsRetryAfterFloor(t *testing.T) {
	t.Run("canceled context returns immediately", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		reason := waitNextObservation(ctx, time.Hour, time.Minute, nil)
		if reason != observationCanceled {
			t.Fatalf("reason = %d", reason)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("canceled wait took %s", elapsed)
		}
	})

	t.Run("nudge waits out the floor", func(t *testing.T) {
		nudge := make(chan struct{}, 1)
		nudge <- struct{}{}
		start := time.Now()
		reason := waitNextObservation(context.Background(), 200*time.Millisecond, 80*time.Millisecond, nudge)
		elapsed := time.Since(start)
		if reason != observationNudged {
			t.Fatalf("reason = %d, elapsed %s", reason, elapsed)
		}
		if elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("nudge woke at %s; the floor is 80ms and the full wait is 200ms", elapsed)
		}
	})

	t.Run("floor covering the whole wait ignores a queued nudge", func(t *testing.T) {
		nudge := make(chan struct{}, 1)
		nudge <- struct{}{}
		start := time.Now()
		reason := waitNextObservation(context.Background(), 80*time.Millisecond, 80*time.Millisecond, nudge)
		elapsed := time.Since(start)
		if reason != observationTimer {
			t.Fatalf("reason = %d, elapsed %s", reason, elapsed)
		}
		if elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("timer woke at %s", elapsed)
		}
		select {
		case <-nudge:
		default:
			t.Fatal("the queued nudge was consumed while the server floor covered the wait")
		}
	})
}

type statusStep struct {
	status     int
	retryAfter string
	setHeader  bool
	body       string
	hijack     bool
}

func statusServer(t *testing.T, steps []statusStep) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := newHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(hits.Add(1)) - 1
		if i >= len(steps) {
			http.Error(w, "extra", http.StatusInternalServerError)
			return
		}
		step := steps[i]
		if step.hijack {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("response cannot be hijacked")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		if step.setHeader {
			w.Header().Set("Retry-After", step.retryAfter)
		}
		if step.body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(step.status)
		if step.body != "" {
			_, _ = io.WriteString(w, step.body)
		}
	}))
	return srv, &hits
}

func backoffRunner(t *testing.T, hubURL string, interval time.Duration, backoff func(int) time.Duration, jitterFn func(time.Duration) time.Duration, sleep func(context.Context, time.Duration) error) *jobsRunner {
	t.Helper()
	r, err := newJobsRunner(jobsOptions{
		HubURL:       hubURL,
		Token:        "agent-token",
		JournalPath:  filepath.Join(t.TempDir(), "journal.json"),
		PollInterval: interval,
		Executor:     noopExecutor{now: func() time.Time { return jobsTestNow }},
		Now:          func() time.Time { return jobsTestNow },
		Sleep:        sleep,
		RetryBackoff: backoff,
		PollJitter:   jitterFn,
	})
	if err != nil {
		t.Fatalf("newJobsRunner: %v", err)
	}
	return r
}

func fixedBackoff(d time.Duration) func(int) time.Duration {
	return func(int) time.Duration { return d }
}

func recordSleeps(dst *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*dst = append(*dst, d)
		return nil
	}
}

func TestJobPostHonorsRetryAfter(t *testing.T) {
	busy := `{"code":"HUB_BUSY","message":"writer is busy"}`
	okBody := `{}`

	t.Run("delta seconds beats a short injected backoff", func(t *testing.T) {
		srv, hits := statusServer(t, []statusStep{
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: "3", body: busy},
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: "3", body: busy},
			{status: http.StatusOK, body: okBody},
		})
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(time.Millisecond), identityDuration, recordSleeps(&slept))
		if err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", map[string]int{"n": 1}, nil, http.StatusOK); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != 3 || len(slept) != 2 || slept[0] != 3*time.Second || slept[1] != 3*time.Second {
			t.Fatalf("hits=%d sleeps=%v", hits.Load(), slept)
		}
	})

	t.Run("injected backoff wins when it is longer", func(t *testing.T) {
		srv, hits := statusServer(t, []statusStep{
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: "1", body: busy},
			{status: http.StatusOK, body: okBody},
		})
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(4*time.Second), identityDuration, recordSleeps(&slept))
		if err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != 2 || len(slept) != 1 || slept[0] != 4*time.Second {
			t.Fatalf("hits=%d sleeps=%v", hits.Load(), slept)
		}
	})

	t.Run("429 is retried and honors the header", func(t *testing.T) {
		srv, hits := statusServer(t, []statusStep{
			{status: http.StatusTooManyRequests, setHeader: true, retryAfter: "4", body: `{"code":"RATE_LIMIT","message":"slow"}`},
			{status: http.StatusOK, body: okBody},
		})
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(time.Millisecond), identityDuration, recordSleeps(&slept))
		if err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != 2 || len(slept) != 1 || slept[0] != 4*time.Second {
			t.Fatalf("hits=%d sleeps=%v", hits.Load(), slept)
		}
	})

	t.Run("500 does not parse Retry-After", func(t *testing.T) {
		srv, hits := statusServer(t, []statusStep{
			{status: http.StatusInternalServerError, setHeader: true, retryAfter: "9", body: "boom"},
			{status: http.StatusOK, body: okBody},
		})
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(100*time.Millisecond), identityDuration, recordSleeps(&slept))
		if err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != 2 || len(slept) != 1 || slept[0] != 100*time.Millisecond {
			t.Fatalf("hits=%d sleeps=%v", hits.Load(), slept)
		}
	})

	t.Run("408 retries without honoring the header", func(t *testing.T) {
		srv, hits := statusServer(t, []statusStep{
			{status: http.StatusRequestTimeout, setHeader: true, retryAfter: "9", body: "slow"},
			{status: http.StatusOK, body: okBody},
		})
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(100*time.Millisecond), identityDuration, recordSleeps(&slept))
		if err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != 2 || len(slept) != 1 || slept[0] != 100*time.Millisecond {
			t.Fatalf("hits=%d sleeps=%v", hits.Load(), slept)
		}
	})

	t.Run("garbage header falls back to the injected backoff", func(t *testing.T) {
		srv, _ := statusServer(t, []statusStep{
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: "not-a-date", body: busy},
			{status: http.StatusOK, body: okBody},
		})
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(250*time.Millisecond), identityDuration, recordSleeps(&slept))
		if err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK); err != nil {
			t.Fatal(err)
		}
		if len(slept) != 1 || slept[0] != 250*time.Millisecond {
			t.Fatalf("sleeps=%v", slept)
		}
	})

	t.Run("http-date is capped at five minutes", func(t *testing.T) {
		header := time.Now().Add(10 * time.Minute).UTC().Format(http.TimeFormat)
		srv, hits := statusServer(t, []statusStep{
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: header, body: busy},
			{status: http.StatusOK, body: okBody},
		})
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(time.Millisecond), identityDuration, recordSleeps(&slept))
		if err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != 2 || len(slept) != 1 || slept[0] != 5*time.Minute {
			t.Fatalf("hits=%d sleeps=%v", hits.Load(), slept)
		}
	})

	t.Run("three failures return the last error", func(t *testing.T) {
		srv, hits := statusServer(t, []statusStep{
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: "2", body: busy},
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: "2", body: busy},
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: "2", body: busy},
		})
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(time.Millisecond), identityDuration, recordSleeps(&slept))
		err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK)
		var httpErr *hubHTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("err = %v", err)
		}
		if hits.Load() != 3 || len(slept) != 2 {
			t.Fatalf("hits=%d sleeps=%v", hits.Load(), slept)
		}
	})

	t.Run("transport errors retry without a Retry-After", func(t *testing.T) {
		srv, hits := statusServer(t, []statusStep{{hijack: true}, {hijack: true}, {hijack: true}})
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(7*time.Millisecond), identityDuration, recordSleeps(&slept))
		err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK)
		var httpErr *hubHTTPError
		if err == nil || errors.As(err, &httpErr) {
			t.Fatalf("err = %v", err)
		}
		if hits.Load() != 3 || len(slept) != 2 || slept[0] != 7*time.Millisecond || slept[1] != 7*time.Millisecond {
			t.Fatalf("hits=%d sleeps=%v err=%v", hits.Load(), slept, err)
		}
	})

	t.Run("cancel during the sleep returns that error", func(t *testing.T) {
		srv, hits := statusServer(t, []statusStep{
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: "3", body: busy},
			{status: http.StatusOK, body: okBody},
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(time.Millisecond), identityDuration, func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		})
		err := r.post(ctx, srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if hits.Load() != 1 {
			t.Fatalf("hits=%d", hits.Load())
		}
	})
}

func TestJobPostDoesNotRetryClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, hits := statusServer(t, []statusStep{
				{status: status, setHeader: true, retryAfter: "30", body: `{"code":"NO","message":"no"}`},
				{status: http.StatusOK, body: `{}`},
			})
			var slept []time.Duration
			r := backoffRunner(t, srv.URL, time.Second, fixedBackoff(time.Millisecond), identityDuration, recordSleeps(&slept))
			err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK)
			var httpErr *hubHTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != status || httpErr.HasRetryAfter {
				t.Fatalf("err = %v", err)
			}
			if hits.Load() != 1 || len(slept) != 0 {
				t.Fatalf("hits=%d sleeps=%v", hits.Load(), slept)
			}
		})
	}
}

func TestJobPostDefaultBackoffIsFullJitter(t *testing.T) {
	steps := make([]statusStep, 0, 40)
	for i := 0; i < 20; i++ {
		steps = append(steps,
			statusStep{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
			statusStep{status: http.StatusOK, body: `{}`},
		)
	}
	srv, _ := statusServer(t, steps)
	var slept []time.Duration
	r := backoffRunner(t, srv.URL, time.Second, nil, identityDuration, recordSleeps(&slept))
	for i := 0; i < 20; i++ {
		if err := r.post(context.Background(), srv.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK); err != nil {
			t.Fatal(err)
		}
	}
	if len(slept) != 20 {
		t.Fatalf("sleeps = %d", len(slept))
	}
	seen := map[time.Duration]struct{}{}
	for _, d := range slept {
		if d < 0 || d > jobPostBackoffBase {
			t.Fatalf("default first-retry sleep %s is outside [0, %s]", d, jobPostBackoffBase)
		}
		seen[d] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatal("default post backoff did not vary")
	}

	double := statusServerSteps(t, []statusStep{
		{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
		{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
		{status: http.StatusOK, body: `{}`},
	})
	var again []time.Duration
	r2 := backoffRunner(t, double.URL, time.Second, nil, identityDuration, recordSleeps(&again))
	if err := r2.post(context.Background(), double.URL+"/v1/jobs/job-1/events", nil, nil, http.StatusOK); err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || again[0] > 500*time.Millisecond || again[1] > time.Second {
		t.Fatalf("two-failure sleeps = %v", again)
	}
}

func TestJobPollBackoffResetsAfterSuccess(t *testing.T) {
	t.Run("204 resets the failure count", func(t *testing.T) {
		steps := []statusStep{
			{status: http.StatusServiceUnavailable, setHeader: true, retryAfter: "45", body: `{"code":"HUB_BUSY","message":"busy"}`},
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
			{status: http.StatusNoContent},
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
			{status: http.StatusNoContent},
		}
		assertPollSleeps(t, 10*time.Second, steps, []time.Duration{
			45 * time.Second,
			20 * time.Second,
			10 * time.Second,
			10 * time.Second,
			10 * time.Second,
		})
	})

	t.Run("a delivered job resets the failure count", func(t *testing.T) {
		steps := []statusStep{
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
			{status: http.StatusOK, body: `{"job_id":"job-1"}`},
			{status: http.StatusNotFound, body: `{"code":"JOB_NOT_FOUND","message":"gone"}`},
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
		}
		assertPollSleeps(t, 10*time.Second, steps, []time.Duration{
			10 * time.Second,
			20 * time.Second,
			10 * time.Second,
		})
	})
}

func TestJobPollBackoffCapsAndIgnoresClientErrors(t *testing.T) {
	t.Run("exponential cap", func(t *testing.T) {
		steps := []statusStep{
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
		}
		assertPollSleeps(t, 2*time.Minute, steps, []time.Duration{
			2 * time.Minute,
			4 * time.Minute,
			5 * time.Minute,
			5 * time.Minute,
		})
	})

	t.Run("jitter above the cap is clamped before Retry-After", func(t *testing.T) {
		srv, hits := statusServer(t, []statusStep{
			{status: http.StatusServiceUnavailable, body: `{"code":"HUB_BUSY","message":"busy"}`},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var slept []time.Duration
		r := backoffRunner(t, srv.URL, 10*time.Second, fixedBackoff(0), func(d time.Duration) time.Duration {
			return d + time.Hour
		}, func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			cancel()
			return context.Canceled
		})
		r.run(ctx)
		if hits.Load() != 1 || len(slept) != 1 || slept[0] != pollBackoffCap {
			t.Fatalf("hits=%d sleeps=%v", hits.Load(), slept)
		}
	})

	for _, status := range []int{http.StatusUnauthorized, http.StatusRequestTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assertPollSleeps(t, 10*time.Second, []statusStep{
				{status: status, setHeader: true, retryAfter: "45", body: `{"code":"NO","message":"no"}`},
			}, []time.Duration{10 * time.Second})
		})
	}

	t.Run("transport errors use the exponential poll delay", func(t *testing.T) {
		assertPollSleeps(t, 10*time.Second, []statusStep{{hijack: true}, {hijack: true}}, []time.Duration{
			10 * time.Second,
			20 * time.Second,
		})
	})
}

func TestJobPollJitterStaysInsideTheCeiling(t *testing.T) {
	var slept []time.Duration
	r := backoffRunner(t, "http://127.0.0.1:9", 10*time.Second, nil, nil, recordSleeps(&slept))
	r.pollFailures = 1
	for i := 0; i < 60; i++ {
		if err := r.pollAfterError(context.Background(), errors.New("down")); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[time.Duration]struct{}{}
	for _, d := range slept {
		if d < 8*time.Second || d >= 12*time.Second {
			t.Fatalf("jittered poll delay %s is outside [8s, 12s)", d)
		}
		if d > pollBackoffCap {
			t.Fatalf("jittered poll delay %s exceeds the cap", d)
		}
		seen[d] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatal("poll jitter did not vary")
	}
}

func assertPollSleeps(t *testing.T, interval time.Duration, steps []statusStep, want []time.Duration) {
	t.Helper()
	srv, hits := statusServer(t, steps)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var slept []time.Duration
	r := backoffRunner(t, srv.URL, interval, fixedBackoff(time.Millisecond), identityDuration, func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		if len(slept) >= len(want) {
			cancel()
			return context.Canceled
		}
		return nil
	})
	r.run(ctx)
	if len(slept) != len(want) {
		t.Fatalf("sleeps=%v, want %v (hits=%d)", slept, want, hits.Load())
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Fatalf("sleeps=%v, want %v", slept, want)
		}
	}
}

func identityDuration(d time.Duration) time.Duration { return d }

func statusServerSteps(t *testing.T, steps []statusStep) *httptest.Server {
	t.Helper()
	srv, _ := statusServer(t, steps)
	return srv
}
