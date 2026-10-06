package main

import (
	"fmt"
	"time"
)

const (
	// clockSkewThreshold matches the historical check-in warning: a gap of
	// exactly two minutes is still treated as acceptable.
	clockSkewThreshold = 2 * time.Minute
	// clockSkewRemindEvery is the slowest a sustained skew is logged again.
	clockSkewRemindEvery = time.Hour
)

// clockSkewTracker is the rate-limit state for the check-in loop.
// The zero value has not yet seen skew and has not logged.
type clockSkewTracker struct {
	skewed   bool
	lastNote time.Time
}

type clockSkewKind uint8

const (
	clockSkewQuiet clockSkewKind = iota
	clockSkewBecameSkewed
	clockSkewRecovered
	clockSkewReminder
)

// clockSkewEvent is the logging decision for one successful check-in.
// Skew is the estimated offset (positive means the hub timestamp is ahead of
// the local send midpoint, so the local clock is behind). RTT is the measured
// postJSON round trip that produced the estimate.
type clockSkewEvent struct {
	Kind clockSkewKind
	Skew time.Duration
	RTT  time.Duration
}

// estimateClockSkew returns receivedAt - (sentAt + rtt/2).
// ok is false when either timestamp is missing; that is not a measurement and
// must not move the tracker. A negative rtt is treated as zero.
func estimateClockSkew(sentAt, receivedAt time.Time, rtt time.Duration) (time.Duration, bool) {
	if sentAt.IsZero() || receivedAt.IsZero() {
		return 0, false
	}
	if rtt < 0 {
		rtt = 0
	}
	return receivedAt.Sub(sentAt.Add(rtt / 2)), true
}

// observe decides whether this sample should be logged.
// Transitions (ok -> skewed, skewed -> ok) always log. While the clock stays
// skewed, a reminder is logged at most once per clockSkewRemindEvery, measured
// from the previous note. now is the agent's clock at the moment of the decision.
func (t clockSkewTracker) observe(now time.Time, skew, rtt time.Duration) (clockSkewTracker, clockSkewEvent) {
	event := clockSkewEvent{Skew: skew, RTT: rtt}
	skewed := clockSkewAbs(skew) > clockSkewThreshold
	switch {
	case skewed && !t.skewed:
		event.Kind = clockSkewBecameSkewed
		t.skewed = true
		t.lastNote = now
	case !skewed && t.skewed:
		event.Kind = clockSkewRecovered
		t.skewed = false
		t.lastNote = time.Time{}
	case skewed && !t.lastNote.IsZero() && now.Sub(t.lastNote) >= clockSkewRemindEvery:
		event.Kind = clockSkewReminder
		t.lastNote = now
	}
	return t, event
}

func clockSkewAbs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// formatClockSkewEvent renders the English log line for a non-quiet event.
// An empty string means the caller should not log.
func formatClockSkewEvent(event clockSkewEvent) string {
	if event.Kind == clockSkewQuiet {
		return ""
	}
	skew := event.Skew.Round(time.Second)
	rtt := event.RTT.Round(time.Millisecond)
	switch event.Kind {
	case clockSkewBecameSkewed:
		return fmt.Sprintf("clock skew: local clock differs from hub by %s (rtt %s)", skew, rtt)
	case clockSkewRecovered:
		return fmt.Sprintf("clock skew recovered: estimated skew %s (rtt %s)", skew, rtt)
	case clockSkewReminder:
		return fmt.Sprintf("clock skew persists: local clock differs from hub by %s (rtt %s)", skew, rtt)
	default:
		return ""
	}
}
