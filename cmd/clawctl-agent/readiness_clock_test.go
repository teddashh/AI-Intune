package main

import (
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestPlatformReadinessUsesAgentMeasurementClockNotHubReceiptClock(t *testing.T) {
	since := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	osDisplay := "Linux"
	if runtime.GOOS == "darwin" {
		osDisplay = "macOS 15.7"
	}
	for _, test := range []struct {
		name        string
		measured    any
		received    any
		wantPending bool
	}{
		{"Hub clock behind, new evidence", since.Add(time.Second), since.Add(-time.Hour), false},
		{"Hub clock ahead, new evidence", since.Add(time.Second), since.Add(time.Hour), false},
		{"Hub clock ahead, old evidence", since.Add(-time.Second), since.Add(time.Hour), true},
		{"measurement at startup", since, since.Add(-time.Hour), false},
		{"no measurement clock", nil, since.Add(time.Hour), true},
		{"no stored receipt", since.Add(time.Second), nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Decode the actual wire shape: an older Hub has no measurement
			// field and cannot prove freshness using the agent's clock.
			payload, err := json.Marshal(map[string]any{
				"machine_id": "machine-1", "agent_version": "v1", "agent_started_at": since,
				"last_checkin_received_at": since.Add(-time.Hour), "jobs_enabled": true, "device_sync_v1": true,
				"identity_measured_at": test.measured, "identity_received_at": test.received,
				"identity_os": osDisplay, "identity_arch": runtime.GOARCH,
			})
			if err != nil {
				t.Fatal(err)
			}
			var receipt model.AgentReadinessResponse
			if err := json.Unmarshal(payload, &receipt); err != nil {
				t.Fatal(err)
			}
			pending := readinessPending(receipt, "machine-1", "v1", since, true)
			if (pending != "") != test.wantPending {
				t.Fatalf("pending=%q, want pending=%t", pending, test.wantPending)
			}
		})
	}
}
