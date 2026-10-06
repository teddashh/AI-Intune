package main

import (
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func diskCleanSend(machine, condition string) store.DiskCleanAlertSend {
	return store.DiskCleanAlertSend{
		Kind: "disk-clean:" + machine + ":" + condition, Body: "disk-clean " + condition + " on " + machine,
		MachineID: machine, Condition: condition, Fingerprint: "fp-1",
	}
}

// A failing notify command must not be retried every minute by the sweep. The
// schedule is the daily report's: 1m, 2m, 4m, ... capped at 60m.
func TestDiskCleanAlertRetriesFollowTheDailyBackoff(t *testing.T) {
	h := newHub(t, "")
	h.notifyCmd = "exit 1"
	send := diskCleanSend("machine-a", "stale")
	start := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 32 * time.Minute, 60 * time.Minute, 60 * time.Minute,
	}
	now := start
	for i, wait := range want {
		if h.sendDiskCleanAlert(send, now) {
			t.Fatalf("attempt %d reported delivery from a failing command", i+1)
		}
		if got := notificationRows(t, h, send.Kind); got != i+1 {
			t.Fatalf("attempt %d at %s: rows=%d, want %d", i+1, now.Format(time.RFC3339), got, i+1)
		}
		// The minute ticker fires every minute until the backoff elapses;
		// none of those ticks may run the command again.
		for tick := now.Add(time.Minute); tick.Before(now.Add(wait)); tick = tick.Add(time.Minute) {
			h.sendDiskCleanAlert(send, tick)
		}
		if got := notificationRows(t, h, send.Kind); got != i+1 {
			t.Fatalf("attempt %d retried before its %s backoff: rows=%d", i+1, wait, got)
		}
		now = now.Add(wait)
	}
}

// Backoff is per alert kind, and a delivery resets it.
func TestDiskCleanAlertBackoffIsPerKindAndResetsOnDelivery(t *testing.T) {
	h := newHub(t, "")
	h.notifyCmd = "exit 1"
	a := diskCleanSend("machine-a", "stale")
	b := diskCleanSend("machine-b", "stale")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	h.sendDiskCleanAlert(a, now)
	h.sendDiskCleanAlert(a, now.Add(time.Minute)) // second failure: next wait is 2m
	if h.sendDiskCleanAlert(b, now.Add(time.Minute)); notificationRows(t, h, b.Kind) != 1 {
		t.Fatal("another machine's alert was held back by machine-a's backoff")
	}

	h.notifyCmd = "cat >/dev/null"
	if h.sendDiskCleanAlert(a, now.Add(2*time.Minute)) {
		t.Fatal("machine-a retried inside its 2m backoff")
	}
	if !h.sendDiskCleanAlert(a, now.Add(3*time.Minute)) {
		t.Fatal("machine-a was not retried when its backoff elapsed")
	}
	h.notifyCmd = "exit 1"
	h.sendDiskCleanAlert(a, now.Add(4*time.Minute))
	if got := notificationRows(t, h, a.Kind); got != 4 {
		t.Fatalf("after a delivery the next failure should be attempted at once: rows=%d", got)
	}
	if h.sendDiskCleanAlert(a, now.Add(4*time.Minute+30*time.Second)); notificationRows(t, h, a.Kind) != 4 {
		t.Fatal("first failure after a delivery did not restart the 1m backoff")
	}
}

// With no notify command, a disk-clean alert records one row per kind per day
// and does not log on every minute tick.
func TestDiskCleanAlertUnconfiguredIsQuiet(t *testing.T) {
	buf := captureLog(t)
	h := newHub(t, "")
	send := diskCleanSend("machine-a", "attention")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 90; i++ {
		h.sendDiskCleanAlert(send, now.Add(time.Duration(i)*time.Minute))
	}
	if got := notificationRows(t, h, send.Kind); got != 1 {
		t.Fatalf("unconfigured rows = %d, want 1", got)
	}
	if n := strings.Count(buf.String(), "WARN notify not configured kind="+send.Kind); n != 1 {
		t.Fatalf("unconfigured warning logged %d times", n)
	}
	if n := strings.Count(buf.String(), "not delivered; next attempt"); n != 0 {
		t.Fatalf("unconfigured alert logged a retry line %d times", n)
	}
}
