package operatoragent

import (
	"context"
	"encoding/json"
	"github.com/teddashh/AI-Intune/internal/scriptcatalog"
	"github.com/teddashh/AI-Intune/internal/store"
	"testing"
)

type fakeScriptHub struct {
	fakeHub
	scriptCalls int
	apply       bool
	key         string
	body        store.ScriptRunRequest
}

func (f *fakeScriptHub) ScriptCatalog(context.Context) ([]scriptcatalog.Entry, error) {
	f.scriptCalls++
	return scriptcatalog.List(), nil
}
func (f *fakeScriptHub) ScriptRun(_ context.Context, body store.ScriptRunRequest, apply bool, key string) (store.ScriptRunResult, error) {
	f.scriptCalls++
	f.body = body
	f.apply = apply
	f.key = key
	return store.ScriptRunResult{}, nil
}
func TestScriptToolsPreviewApply(t *testing.T) {
	f := &fakeScriptHub{}
	s := &Service{Hub: f}
	for _, tool := range scriptTools() {
		if tool.InputSchema["additionalProperties"] != false {
			t.Fatal("open tool input")
		}
	}
	if _, err := s.Call(context.Background(), "script_run_apply", json.RawMessage(`{"idempotency_key":"k"}`)); callErr(t, err).Code != "preview_digest_required" || f.scriptCalls != 0 {
		t.Fatal("apply did not fail closed")
	}
	raw := `{"script_id":"fleet-probe-v1","script_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","args":{},"targets":["machine-a"],"reason":"probe facts","preview_digest":"` + testDigest + `","idempotency_key":"script-key"}`
	if _, err := s.Call(context.Background(), "script_run_apply", json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	if f.scriptCalls != 1 || !f.apply || f.key != "script-key" || f.body.PreviewDigest != testDigest {
		t.Fatal("apply must use original digest and key without preview")
	}
	if _, err := s.Call(context.Background(), "script_run_apply", json.RawMessage(raw[:len(raw)-1]+`,"command":"true"}`)); err == nil {
		t.Fatal("unknown argument accepted")
	}
}
