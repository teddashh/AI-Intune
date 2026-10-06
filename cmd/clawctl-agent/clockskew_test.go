package main

import (
	"strings"
	"testing"
	"time"
)

func TestEstimateClockSkewSubtractsHalfTheRoundTrip(t *testing.T) {
	sent := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	got, ok := estimateClockSkew(sent, sent.Add(3*time.Minute), 6*time.Minute)
	if !ok || got != 0 {
		t.Fatalf("skew = %s, ok=%v; a 3min timestamp gap with a 6min RTT is zero skew", got, ok)
	}

	got, ok = estimateClockSkew(sent, sent.Add(3*time.Minute), 10*time.Second)
	want := 3*time.Minute - 5*time.Second
	if !ok || got != want {
		t.Fatalf("skew = %s, ok=%v; want %s", got, ok, want)
	}

	got, ok = estimateClockSkew(sent, sent.Add(3*time.Minute), -time.Second)
	if !ok || got != 3*time.Minute {
		t.Fatalf("negative RTT skew = %s, ok=%v; want 3m0s", got, ok)
	}

	if _, ok := estimateClockSkew(sent, time.Time{}, time.Second); ok {
		t.Fatal("missing hub timestamp was treated as a measurement")
	}
	if _, ok := estimateClockSkew(time.Time{}, sent, time.Second); ok {
		t.Fatal("missing send timestamp was treated as a measurement")
	}
}

func TestClockSkewTrackerLogsTransitionsAndHourlyReminders(t *testing.T) {
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	skewed := 3 * time.Minute
	border := clockSkewThreshold
	over := clockSkewThreshold + time.Nanosecond
	rtt := 20 * time.Millisecond

	var track clockSkewTracker
	track, event := track.observe(base, border, rtt)
	if event.Kind != clockSkewQuiet || track.skewed {
		t.Fatalf("exactly 2min is skewed: %+v track=%+v", event, track)
	}
	track, event = track.observe(base, -border, rtt)
	if event.Kind != clockSkewQuiet || track.skewed {
		t.Fatalf("exactly -2min is skewed: %+v", event)
	}

	track, event = track.observe(base, over, rtt)
	if event.Kind != clockSkewBecameSkewed || !track.skewed {
		t.Fatalf("2min+1ns did not transition: %+v", event)
	}
	if line := formatClockSkewEvent(event); !strings.Contains(line, "clock skew:") || !strings.Contains(line, "rtt") {
		t.Fatalf("transition line = %q", line)
	}

	track, event = track.observe(base.Add(30*time.Minute), skewed, rtt)
	if event.Kind != clockSkewQuiet {
		t.Fatalf("reminder inside the hour: %+v", event)
	}
	track, event = track.observe(base.Add(clockSkewRemindEvery), skewed, rtt)
	if event.Kind != clockSkewReminder {
		t.Fatalf("hourly reminder missing: %+v", event)
	}
	if line := formatClockSkewEvent(event); !strings.Contains(line, "persists") || !strings.Contains(line, "rtt") {
		t.Fatalf("reminder line = %q", line)
	}
	track, event = track.observe(base.Add(clockSkewRemindEvery+time.Minute), skewed, rtt)
	if event.Kind != clockSkewQuiet {
		t.Fatalf("second reminder inside the next hour: %+v", event)
	}

	track, event = track.observe(base.Add(clockSkewRemindEvery+2*time.Minute), 0, rtt)
	if event.Kind != clockSkewRecovered || track.skewed {
		t.Fatalf("recovery = %+v track=%+v", event, track)
	}
	if line := formatClockSkewEvent(event); !strings.Contains(line, "recovered") || !strings.Contains(line, "rtt") {
		t.Fatalf("recovery line = %q", line)
	}

	// A new skew after recovery logs immediately, even inside the old hour.
	track, event = track.observe(base.Add(clockSkewRemindEvery+3*time.Minute), -skewed, rtt)
	if event.Kind != clockSkewBecameSkewed || !track.skewed {
		t.Fatalf("second transition = %+v", event)
	}
	if line := formatClockSkewEvent(event); !strings.Contains(line, "-3m0s") {
		t.Fatalf("signed skew missing from %q", line)
	}
}

func TestClockSkewRTTCorrectionChangesTheThresholdDecision(t *testing.T) {
	sent := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	// Raw gap is 2min30s, which would warn. Half of a 1min RTT brings it to exactly 2min.
	skew, ok := estimateClockSkew(sent, sent.Add(2*time.Minute+30*time.Second), time.Minute)
	if !ok || skew != clockSkewThreshold {
		t.Fatalf("corrected skew = %s, ok=%v", skew, ok)
	}
	var track clockSkewTracker
	_, event := track.observe(sent, skew, time.Minute)
	if event.Kind != clockSkewQuiet {
		t.Fatalf("RTT-corrected boundary logged: %+v", event)
	}

	skew, ok = estimateClockSkew(sent, sent.Add(2*time.Minute+30*time.Second+time.Nanosecond), time.Minute)
	if !ok || skew != clockSkewThreshold+time.Nanosecond {
		t.Fatalf("corrected skew = %s, ok=%v", skew, ok)
	}
	_, event = track.observe(sent, skew, time.Minute)
	if event.Kind != clockSkewBecameSkewed {
		t.Fatalf("one nanosecond over the corrected boundary was quiet: %+v", event)
	}
}
