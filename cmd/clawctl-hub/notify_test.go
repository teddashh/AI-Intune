package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotifyBackoffDurations(t *testing.T) {
	want := []time.Duration{
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		32 * time.Minute,
		60 * time.Minute,
		60 * time.Minute,
	}
	for i, d := range want {
		if got := notifyBackoff(i + 1); got != d {
			t.Fatalf("notifyBackoff(%d) = %s, want %s", i+1, got, d)
		}
	}
}

func TestDailyReportBackoffFollowsTheSchedule(t *testing.T) {
	h := newHub(t, "")
	h.reportAt = "08:00"
	h.notifyCmd = "exit 1"
	due := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 32 * time.Minute, 60 * time.Minute, 60 * time.Minute,
	}
	now := due
	for i, wait := range want {
		before := notificationRows(t, h, "daily")
		h.maybeSendReport(now)
		after := notificationRows(t, h, "daily")
		if after != before+1 {
			t.Fatalf("attempt %d at %s recorded %d new rows, want 1", i+1, now.Format(time.RFC3339), after-before)
		}
		_, failures, lastFailed, err := h.store.NotificationAttemptsSince("daily", due)
		if err != nil {
			t.Fatal(err)
		}
		if failures != i+1 {
			t.Fatalf("failures = %d, want %d", failures, i+1)
		}
		if got := notifyBackoff(failures); got != wait {
			t.Fatalf("backoff after %d failures = %s, want %s", failures, got, wait)
		}
		if _, found, err := h.store.LastNotification("daily"); err != nil || found {
			t.Fatalf("failed attempt consumed the day: found=%v err=%v", found, err)
		}
		early := lastFailed.Add(wait).Add(-time.Second)
		h.maybeSendReport(early)
		if got := notificationRows(t, h, "daily"); got != after {
			t.Fatalf("attempt %d retried at %s before the backoff elapsed", i+1, early.Format(time.RFC3339))
		}
		now = lastFailed.Add(wait)
	}
}

func TestDailyReportBackoffSurvivesRestart(t *testing.T) {
	h := newHub(t, "")
	h.reportAt = "08:00"
	h.notifyCmd = "exit 1"
	due := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{due, due.Add(time.Minute), due.Add(3 * time.Minute)} {
		if err := h.store.RecordNotification("daily", "cmd", "earlier", false, "fail", at); err != nil {
			t.Fatal(err)
		}
	}
	// Three failures already on disk: the next try is lastFailed + 4m.
	early := due.Add(3*time.Minute + 4*time.Minute - time.Second)
	h.maybeSendReport(early)
	if got := notificationRows(t, h, "daily"); got != 3 {
		t.Fatalf("restarted hub retried early: rows=%d", got)
	}
	h.maybeSendReport(due.Add(3*time.Minute + 4*time.Minute))
	if got := notificationRows(t, h, "daily"); got != 4 {
		t.Fatalf("restarted hub missed the scheduled attempt: rows=%d", got)
	}
}

func TestDeliveredDailyReportStopsFurtherAttempts(t *testing.T) {
	h := newHub(t, "")
	h.reportAt = "08:00"
	h.notifyCmd = "cat >/dev/null"
	due := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	h.maybeSendReport(due)
	h.maybeSendReport(due.Add(time.Hour))
	if got := notificationRows(t, h, "daily"); got != 1 {
		t.Fatalf("successful report rows = %d, want 1", got)
	}
	last, found, err := h.store.LastNotification("daily")
	if err != nil || !found || !last.Equal(due) {
		t.Fatalf("last delivered = %v found=%v err=%v", last, found, err)
	}
}

func TestUnconfiguredNotifyRecordsOncePerDay(t *testing.T) {
	buf := captureLog(t)
	h := newHub(t, "")
	h.reportAt = "08:00"
	p := &pingLog{}
	p.install(t)
	h.reportPingURL = "https://hc.example/abc"
	due := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	const sentinel = "REPORT-BODY-UNCONFIGURED"

	if h.deliver("daily", sentinel, due) {
		t.Fatal("unconfigured deliver reported success")
	}
	if h.deliver("daily", sentinel, due.Add(time.Hour)) {
		t.Fatal("second unconfigured deliver reported success")
	}
	h.maybeSendReport(due.Add(2 * time.Hour))
	if got := notificationRows(t, h, "daily"); got != 1 {
		t.Fatalf("unconfigured rows = %d, want 1", got)
	}
	var errText string
	if err := h.store.DB().QueryRow(`SELECT COALESCE(error, '') FROM notifications WHERE kind = 'daily'`).Scan(&errText); err != nil {
		t.Fatal(err)
	}
	if errText != notifyNotConfigured {
		t.Fatalf("error text = %q, want %q", errText, notifyNotConfigured)
	}
	out := buf.String()
	if strings.Count(out, sentinel) != 1 {
		t.Fatalf("unconfigured body was logged %d times:\n%s", strings.Count(out, sentinel), out)
	}
	if strings.Count(out, "WARN notify not configured kind=daily") != 1 {
		t.Fatalf("unconfigured warning count wrong:\n%s", out)
	}
	if got := p.got(); len(got) != 1 || !strings.HasSuffix(got[0], "/fail") {
		t.Fatalf("unconfigured pings = %v, want one /fail", got)
	}

	h.maybeSendReport(due.Add(24 * time.Hour))
	if got := notificationRows(t, h, "daily"); got != 2 {
		t.Fatalf("next day rows = %d, want 2", got)
	}
}

func TestConfiguredNotifyDoesNotLogTheBodyOnRetries(t *testing.T) {
	buf := captureLog(t)
	h := newHub(t, "")
	h.reportAt = "08:00"
	h.notifyCmd = "exit 1"
	due := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	const sentinel = "REPORT-BODY-CONFIGURED-SENTINEL"
	if h.deliver("daily", sentinel, due) || h.deliver("daily", sentinel, due.Add(time.Minute)) {
		t.Fatal("failing command reported delivery")
	}
	out := buf.String()
	if strings.Contains(out, sentinel) {
		t.Fatalf("configured attempt logged the report body:\n%s", out)
	}
	if !strings.Contains(out, "notify attempt kind=daily bytes=") || !strings.Contains(out, "attempt=1") || !strings.Contains(out, "attempt=2") {
		t.Fatalf("attempt lines missing:\n%s", out)
	}
	if strings.Count(out, "outcome=failed next=") != 2 {
		t.Fatalf("retry log did not name the next attempt:\n%s", out)
	}
}

func TestNotifyFailurePingOnlyOnFirstAndCap(t *testing.T) {
	p := &pingLog{}
	p.install(t)
	h := newHub(t, "")
	h.reportAt = "08:00"
	h.reportPingURL = "https://hc.example/abc"
	h.notifyCmd = "exit 1"
	due := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	h.deliver("daily", "one", due)
	h.deliver("daily", "two", due.Add(time.Minute))
	for i := 2; i < 6; i++ {
		if err := h.store.RecordNotification("daily", "cmd", "seed", false, "fail", due.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	h.deliver("daily", "seven", due.Add(6*time.Minute))
	got := p.got()
	if len(got) != 2 {
		t.Fatalf("failure pings = %d (%v), want the first attempt and the capped attempt", len(got), got)
	}
	for _, u := range got {
		if !strings.HasSuffix(u, "/fail") {
			t.Fatalf("failure ping %q is not /fail", u)
		}
	}
}

func TestLogNotifyConfiguredDoesNotRunTheCommand(t *testing.T) {
	buf := captureLog(t)
	h := newHub(t, "")
	marker := filepath.Join(t.TempDir(), "ran")
	h.notifyCmd = "touch " + marker
	h.logNotifyConfigured()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("startup executed the notify command: %v", err)
	}
	if !strings.Contains(buf.String(), "notify command is configured") {
		t.Fatalf("startup log = %q", buf.String())
	}
	if strings.Contains(buf.String(), marker) || strings.Contains(buf.String(), "touch") {
		t.Fatalf("startup log included the command: %s", buf.String())
	}
	buf.Reset()
	h.notifyCmd = ""
	h.logNotifyConfigured()
	if buf.Len() != 0 {
		t.Fatalf("unconfigured startup logged %q", buf.String())
	}
}

func TestNotifyMetricsReadFromTheStore(t *testing.T) {
	h := newHub(t, "")
	h.notifyCmd = "exit 1"
	success := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	fail1 := success.Add(time.Minute)
	fail2 := success.Add(2 * time.Minute)
	if err := h.store.RecordNotification("daily", "cmd", "old-fail", false, "x", success.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.RecordNotification("daily", "cmd", "ok", true, "", success); err != nil {
		t.Fatal(err)
	}
	if err := h.store.RecordNotification("daily", "cmd", "no", false, "x", fail1); err != nil {
		t.Fatal(err)
	}
	if err := h.store.RecordNotification("daily", "cmd", "no", false, "x", fail2); err != nil {
		t.Fatal(err)
	}
	fam := parseExposition(t, metricsBody(t, h))
	if got := single(t, fam, "clawctl_notify_configured"); got != 1 {
		t.Fatalf("configured = %v, want 1", got)
	}
	if got := sampleValue(t, fam, "clawctl_notify_consecutive_failures", "kind", "daily"); got != 2 {
		t.Fatalf("consecutive failures = %v, want 2", got)
	}
	if got := sampleValue(t, fam, "clawctl_notify_last_success_timestamp_seconds", "kind", "daily"); got != float64(success.Unix()) {
		t.Fatalf("last success = %v, want %d", got, success.Unix())
	}
	if got := sampleValue(t, fam, "clawctl_notify_last_attempt_timestamp_seconds", "kind", "daily"); got != float64(fail2.Unix()) {
		t.Fatalf("last attempt = %v, want %d", got, fail2.Unix())
	}

	fresh := newHub(t, "")
	body := metricsBody(t, fresh)
	if strings.Contains(body, "clawctl_notify_last_success_timestamp_seconds") || strings.Contains(body, "clawctl_notify_last_attempt_timestamp_seconds") {
		t.Fatalf("fresh hub emitted a timestamp before any attempt:\n%s", body)
	}
	fam = parseExposition(t, body)
	if got := single(t, fam, "clawctl_notify_configured"); got != 0 {
		t.Fatalf("unconfigured = %v, want 0", got)
	}
	if got := sampleValue(t, fam, "clawctl_notify_consecutive_failures", "kind", "daily"); got != 0 {
		t.Fatalf("fresh consecutive failures = %v, want 0", got)
	}
}

func notificationRows(t *testing.T, h *hub, kind string) int {
	t.Helper()
	var n int
	if err := h.store.DB().QueryRow(`SELECT COUNT(*) FROM notifications WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func metricsBody(t *testing.T, h *hub) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func sampleValue(t *testing.T, fam map[string]*family, name, label, value string) float64 {
	t.Helper()
	f := fam[name]
	if f == nil {
		t.Fatalf("metric %s is absent", name)
	}
	for _, s := range f.samples {
		if s.labels[label] == value {
			return s.value
		}
	}
	t.Fatalf("metric %s has no %s=%s sample", name, label, value)
	return 0
}
