package web

import (
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestMachineOOMKills(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "Example")
	now := time.Now().UTC()
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "No recent OOM kills reported") || !strings.Contains(body, `href="/updates"`) {
		t.Fatal("missing actionable empty state")
	}
	b := model.ObservationBatch{MeasuredAt: now, OOMKills: []model.OOMKill{{Time: now.Add(-time.Minute), Source: "kernel", Unit: "example.service", Process: "worker"}}}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatal(err)
	}
	body = get(t, s, "/machines/"+id)
	for _, want := range []string{"Recent OOM kills", "example.service", "worker", "kernel", "Unavailable"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if !strings.Contains(get(t, s, "/machines?machines=attention"), "Recent OOM kills") {
		t.Fatal("missing fleet attention")
	}
}
