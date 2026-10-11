package main

import (
	"encoding/json"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/scriptcatalog"
	"github.com/teddashh/AI-Intune/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperatorScriptAPI(t *testing.T) {
	f := observedOperatorFixture(t)
	enabled := true
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{SchemaVersion: model.SchemaVersion, SentAt: jobsTestNow, JobsEnabled: &enabled, ScriptV1: true}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	e, _ := scriptcatalog.Lookup("fleet-probe-v1")
	body := store.ScriptRunRequest{ScriptID: e.ID, ScriptSHA256: e.SHA256, Args: json.RawMessage(`{}`), Targets: []string{f.machine.id}, Reason: "probe facts"}
	raw, _ := json.Marshal(body)
	preview := diagnosticOperatorRequest(t, f.mux, "/v1/operator/script-runs/preview", "", string(raw))
	if preview.Code != 200 {
		t.Fatalf("preview %d %s", preview.Code, preview.Body.String())
	}
	var out store.ScriptRunResult
	if err := json.Unmarshal(preview.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	body.PreviewDigest = out.PreviewDigest
	raw, _ = json.Marshal(body)
	applied := diagnosticOperatorRequest(t, f.mux, "/v1/operator/script-runs", "probe-api-key", string(raw))
	if applied.Code != 201 {
		t.Fatalf("apply %d %s", applied.Code, applied.Body.String())
	}
	for _, change := range []func(*store.ScriptRunRequest){func(r *store.ScriptRunRequest) { r.ScriptID = "unknown" }, func(r *store.ScriptRunRequest) { r.ScriptSHA256 = strings.Repeat("b", 64) }, func(r *store.ScriptRunRequest) { r.Targets = make([]string, 51) }} {
		r := body
		change(&r)
		raw, _ := json.Marshal(r)
		rec := diagnosticOperatorRequest(t, f.mux, "/v1/operator/script-runs/preview", "", string(raw))
		if rec.Code != 400 {
			t.Fatalf("rejected request %d", rec.Code)
		}
	}
}
func TestScriptScopeDefenseInDepth(t *testing.T) {
	f := observedOperatorFixture(t)
	e, _ := scriptcatalog.Lookup("fleet-probe-v1")
	raw, _ := json.Marshal(store.ScriptRunRequest{ScriptID: e.ID, ScriptSHA256: e.SHA256, Args: json.RawMessage(`{}`), Targets: []string{f.machine.id}, Reason: "probe facts"})
	req := httptest.NewRequest(http.MethodPost, "/v1/operator/script-runs/preview", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req = verifiedOperatorRequest(req, operatorauth.View)
	rec := httptest.NewRecorder()
	(&hub{store: f.store}).handleScriptPreview(rec, req)
	if rec.Code < 400 {
		t.Fatal("view principal created script preview")
	}
}

func TestScriptPermissionInspectionReplaysBody(t *testing.T) {
	for _, body := range []string{`{"script_id":"fleet-probe-v1","args":{}}`, `{"script_id":"unknown"}`, strings.Repeat(" ", 17000)} {
		r := httptest.NewRequest(http.MethodPost, "/v1/operator/script-runs", strings.NewReader(body))
		permission := scriptRequestPermission(r)
		if len(body) < 16000 && permission != operatorauth.Operate {
			t.Fatal("read catalog requires operate")
		}
		replay, err := io.ReadAll(r.Body)
		if err != nil || string(replay) != body {
			t.Fatal("permission inspection changed body")
		}
	}
}

func TestScriptCatalogAPIObjectEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	(&hub{}).handleScriptCatalog(rec, httptest.NewRequest(http.MethodGet, "/v1/operator/script-catalog", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("catalog status=%d headers=%v", rec.Code, rec.Header())
	}
	var response scriptcatalog.CatalogResponse
	decoder := json.NewDecoder(rec.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].ID != "fleet-probe-v1" {
		t.Fatalf("catalog %+v", response)
	}
}
