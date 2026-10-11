package operator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestSupervisionServiceValidatesAndCapsDTOs(t *testing.T) {
	st := newOperatorJobReadStore(t)
	s := New(st)
	now := time.Now().UTC()
	for _, f := range []SupervisionFilter{{Window: "bad"}, {Kind: " padded "}, {MachineID: "bad\nvalue"}, {Kind: strings.Repeat("a", 257)}} {
		if _, err := s.JobsSummary(context.Background(), f, now); err != ErrInvalidSupervision {
			t.Fatalf("validation %+v %v", f, err)
		}
	}
	if _, err := s.ApprovalsSummary(context.Background(), "bad", now); err != ErrInvalidSupervision {
		t.Fatalf("window validation %v", err)
	}
	status, err := s.HubStatus(context.Background(), strings.Repeat("v", 256), now.Add(-time.Hour), now)
	if err != nil || len(status.Version) > 128 || status.UptimeSeconds == nil || *status.UptimeSeconds != 3600 {
		t.Fatalf("status %+v %v", status, err)
	}
	if err := st.UpsertMachine(store.Machine{MachineID: "machine-a", DisplayName: "sample-agent", Expected: true, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	desired, rev, err := st.CreateDesiredState("machine", "machine-a", "diagnostic", "noop", `{"kind":"`+strings.Repeat("a", 300)+`","script_id":"script\u0001id"}`, "operator-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob("machine-a", desired, rev, store.NewJob{}); err != nil {
		t.Fatal(err)
	}
	r, err := s.JobsSummary(context.Background(), SupervisionFilter{}, now.Add(time.Minute))
	if err != nil || len(r.Items) != 1 || len(r.Items[0].Kind) > 256 || strings.Contains(*r.Items[0].ScriptID, "\x01") {
		t.Fatalf("bounded DTO %+v %v", r, err)
	}
}

func TestSupervisionLabelsRedactCredentialShapes(t *testing.T) {
	for _, value := range []string{"Bearer sample-secret-value", "token=sample-secret-value", "sk-sample-secret-value"} {
		result, _, _ := supervisionText(value, 256)
		if result != "[redacted]" {
			t.Fatalf("credential shape not redacted")
		}
	}
}
