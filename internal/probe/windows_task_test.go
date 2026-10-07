package probe

import (
	"errors"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestWindowsNativeTaskObservation(t *testing.T) {
	lastRun := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	t.Run("running agent task", func(t *testing.T) {
		got := windowsObservedUnit(windowsAgentTaskUnit, windowsTaskView{
			Found: true, State: windowsTaskStateRunning, LastRun: lastRun,
		}, nil)
		if !got.Present || !got.Measured || got.ActiveState != "active" || got.SubState != "running" {
			t.Fatalf("unit=%+v", got)
		}
		if got.ActiveEnterTimestamp == nil || !got.ActiveEnterTimestamp.Equal(lastRun) {
			t.Fatalf("last run=%v", got.ActiveEnterTimestamp)
		}
		if got.NRestarts != 0 || got.MainPID != 0 {
			t.Fatalf("invented restart or pid: %+v", got)
		}
	})
	t.Run("ready is registered and not running", func(t *testing.T) {
		got := windowsObservedUnit(windowsAgentTaskUnit, windowsTaskView{
			Found: true, State: windowsTaskStateReady,
		}, nil)
		if !got.Present || got.ActiveState != "inactive" || got.SubState != "ready" {
			t.Fatalf("ready mapped as running: %+v", got)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		got := windowsObservedUnit(windowsAgentTaskUnit, windowsTaskView{
			Found: true, State: windowsTaskStateDisabled,
		}, nil)
		if !got.Present || got.ActiveState != "inactive" || got.SubState != "disabled" {
			t.Fatalf("disabled=%+v", got)
		}
	})
	t.Run("missing agent task", func(t *testing.T) {
		got := windowsObservedUnit(windowsAgentTaskUnit, windowsTaskView{}, nil)
		if !got.Measured || got.Present || got.Reason != "" {
			t.Fatalf("missing task=%+v", got)
		}
	})
	t.Run("query failed stays unknown", func(t *testing.T) {
		got := windowsObservedUnit(windowsAgentTaskUnit, windowsTaskView{Found: true, State: windowsTaskStateRunning},
			errors.New("Get-ScheduledTask clawctl-agent failed: exit 1"))
		if got.Measured || got.Present || got.Reason == "" {
			t.Fatalf("failed query=%+v", got)
		}
	})
	t.Run("other watched units are measured absent", func(t *testing.T) {
		got := windowsObservedUnit("openclaw-gateway.service", windowsTaskView{Found: true, State: windowsTaskStateRunning}, nil)
		if !got.Measured || got.Present || got.ActiveState != "" {
			t.Fatalf("unregistered unit=%+v", got)
		}
	})
	t.Run("unknown state", func(t *testing.T) {
		got := windowsObservedUnit(windowsAgentTaskUnit, windowsTaskView{Found: true, State: 99}, nil)
		if !got.Present || !got.Measured || got.ActiveState != "" || got.Reason == "" {
			t.Fatalf("unknown state=%+v", got)
		}
	})
	t.Run("parse report", func(t *testing.T) {
		got, err := parseWindowsTaskReport("found=1\nstate=4\nlastrun=2026-09-21T12:00:00Z\n")
		if err != nil || !got.Found || got.State != windowsTaskStateRunning || !got.LastRun.Equal(lastRun) {
			t.Fatalf("parse=%+v err=%v", got, err)
		}
		missing, err := parseWindowsTaskReport("found=0\n")
		if err != nil || missing.Found {
			t.Fatalf("missing=%+v err=%v", missing, err)
		}
		never, err := parseWindowsTaskReport("found=1\nstate=3\nlastrun=0001-01-01T00:00:00Z\n")
		if err != nil || !never.LastRun.IsZero() || never.State != windowsTaskStateReady {
			t.Fatalf("never run=%+v err=%v", never, err)
		}
		if _, err := parseWindowsTaskReport("Status: Running\n"); err == nil {
			t.Fatal("localized schtasks text was accepted")
		}
	})
}

func TestWindowsTaskObservationKeepsWatchedDenominator(t *testing.T) {
	units := make([]model.Unit, 0, len(watchedUnits))
	for _, name := range watchedUnits {
		units = append(units, windowsObservedUnit(name, windowsTaskView{Found: true, State: windowsTaskStateRunning}, nil))
	}
	if len(units) != len(watchedUnits) {
		t.Fatalf("units=%d want %d", len(units), len(watchedUnits))
	}
	var agent model.Unit
	for _, u := range units {
		if u.Name == windowsAgentTaskUnit {
			agent = u
			continue
		}
		if !u.Measured || u.Present {
			t.Fatalf("unregistered %s=%+v", u.Name, u)
		}
	}
	if !agent.Present || agent.ActiveState != "active" {
		t.Fatalf("agent=%+v", agent)
	}
}
