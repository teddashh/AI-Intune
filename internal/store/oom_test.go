package store

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestOOMRetentionDedupAndValidation(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	if err := s.UpsertMachine(Machine{MachineID: "oom-test", DisplayName: "Example", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	e := model.OOMKill{Time: now.Add(-time.Minute), Source: "kernel", Unit: "example.service", Process: "worker"}
	b := model.ObservationBatch{MeasuredAt: now, OOMKills: []model.OOMKill{e}}
	for i := 0; i < 2; i++ {
		if err := s.RecordObservation("oom-test", b, now); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.RecentOOMKills("oom-test", now)
	if err != nil || len(events) != 1 {
		t.Fatalf("dedup: %v %v", events, err)
	}
	events, err = s.RecentOOMKills("oom-test", now.Add(2*time.Hour))
	if err != nil || len(events) != 0 {
		t.Fatalf("expired: %v %v", events, err)
	}
	b.OOMKills = nil
	if err := s.RecordObservation("oom-test", b, now); err != nil {
		t.Fatal(err)
	}
	if events, err := s.RecentOOMKills("oom-test", now); err != nil || len(events) != 1 {
		t.Fatal("old agent erased evidence")
	}
	b.OOMKills = make([]model.OOMKill, 65)
	if err := s.RecordObservation("oom-test", b, now); err == nil {
		t.Fatal("accepted oversized batch")
	}
	b.OOMKills = []model.OOMKill{e}
	b.OOMKills[0].Process = "worker token=private"
	if err := s.RecordObservation("oom-test", b, now); err == nil {
		t.Fatal("accepted free text")
	}
	b.OOMKills = nil
	if err := s.RecordObservation("oom-test", b, now); err != nil {
		t.Fatalf("old agent: %v", err)
	}
}
