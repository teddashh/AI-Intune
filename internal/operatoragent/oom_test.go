package operatoragent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
)

type oomFakeHub struct{ fakeHub }

func (f *oomFakeHub) MachineOOMKills(context.Context, string) (operator.OOMKillResult, error) {
	f.call("oom")
	return operator.OOMKillResult{}, nil
}
func TestMachineOOMTool(t *testing.T) {
	f := &oomFakeHub{}
	s := &Service{Hub: f}
	for _, raw := range []string{`{}`, `{"machine_id":"example","raw":true}`} {
		if _, err := s.Call(context.Background(), "machine_oom_kills", json.RawMessage(raw)); err == nil {
			t.Fatal("accepted invalid args")
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("invalid input called Hub")
	}
	if _, err := s.Call(context.Background(), "machine_oom_kills", json.RawMessage(`{"machine_id":"example"}`)); err != nil || !f.called("oom") {
		t.Fatalf("read: %v", err)
	}
	for _, tool := range Tools() {
		if tool.Name == "machine_oom_kills" {
			if !tool.Annotations.ReadOnlyHint {
				t.Fatal("missing readonly hint")
			}
			return
		}
	}
	t.Fatal("tool missing")
}
