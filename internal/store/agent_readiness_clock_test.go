package store

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestAgentReadinessKeepsIdentityMeasurementAndReceiptClocksSeparate(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	machineID := mustEnroll(t, s, "mac-clock-skew", at)
	first := model.ObservationBatch{
		MeasuredAt: at.Add(time.Hour),
		Identity:   model.Identity{OS: "macOS 15.7", Arch: "arm64"},
	}
	if err := s.RecordObservation(machineID, first, at); err != nil {
		t.Fatal(err)
	}

	// A delayed observation can arrive later with an earlier agent timestamp.
	// Both clocks and the platform must come from the latest received row.
	latest := model.ObservationBatch{
		MeasuredAt: at.Add(-time.Hour),
		Identity:   model.Identity{OS: "macOS 15.6", Arch: "x86_64"},
	}
	receivedAt := at.Add(time.Second)
	if err := s.RecordObservation(machineID, latest, receivedAt); err != nil {
		t.Fatal(err)
	}

	got, err := s.AgentReadiness(machineID)
	if err != nil || got.IdentityMeasuredAt == nil || !got.IdentityMeasuredAt.Equal(latest.MeasuredAt) ||
		got.IdentityReceivedAt == nil || !got.IdentityReceivedAt.Equal(receivedAt) ||
		got.IdentityOS != latest.Identity.OS || got.IdentityArch != latest.Identity.Arch {
		t.Fatalf("identity clocks and platform=%+v err=%v", got, err)
	}
}
