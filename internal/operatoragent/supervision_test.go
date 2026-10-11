package operatoragent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func (f *fakeHub) JobsSummary(context.Context, operator.SupervisionFilter) (operator.JobsSummary, error) {
	f.call("JobsSummary")
	return operator.JobsSummary{}, f.err
}
func (f *fakeHub) ApprovalsSummary(context.Context, string) (operator.ApprovalsSummary, error) {
	f.call("ApprovalsSummary")
	return operator.ApprovalsSummary{}, f.err
}
func (f *fakeHub) HubStatus(context.Context) (operator.HubStatus, error) {
	f.call("HubStatus")
	return operator.HubStatus{}, f.err
}

func TestSupervisionToolsReadOnlyAndStrict(t *testing.T) {
	for _, name := range []string{"jobs_summary", "approvals_summary", "hub_status"} {
		found := false
		for _, tool := range Tools() {
			if tool.Name == name {
				found = true
				if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.InputSchema["additionalProperties"] != false {
					t.Fatalf("unsafe schema %+v", tool)
				}
			}
		}
		if !found {
			t.Fatalf("missing %s", name)
		}
		f := &fakeHub{}
		svc := &Service{Hub: f}
		if _, err := svc.Call(context.Background(), name, json.RawMessage(`{}`)); err != nil || len(f.calls) != 1 {
			t.Fatalf("%s: %v %+v", name, err, f.calls)
		}
		f.calls = nil
		if _, err := svc.Call(context.Background(), name, json.RawMessage(`{"command":"anything"}`)); err == nil || len(f.calls) != 0 {
			t.Fatalf("unknown arguments reached Hub: %s", name)
		}
	}
	for _, args := range []string{`{"window":"30d"}`, `null`, `{"window":null}`, `{"kind":""}`, `{"machine_id":" invalid "}`, `{"per_machine":"true"}`} {
		f := &fakeHub{}
		if _, err := (&Service{Hub: f}).Call(context.Background(), "jobs_summary", json.RawMessage(args)); err == nil || len(f.calls) > 0 {
			t.Fatalf("invalid args %s reached Hub", args)
		}
	}
	f := &fakeHub{}
	f.err = &CallError{Code: "test", Message: "failed"}
	if _, err := (&Service{Hub: f}).Call(context.Background(), "hub_status", json.RawMessage(`{}`)); err == nil {
		t.Fatal("lost Hub error")
	}
}
