package operatorclient

import (
	"context"
	"encoding/json"
	"github.com/teddashh/AI-Intune/internal/scriptcatalog"
	"github.com/teddashh/AI-Intune/internal/store"
	"net/http"
	"testing"
)

func TestScriptClientCatalogPreviewApply(t *testing.T) {
	e, _ := scriptcatalog.Lookup("fleet-probe-v1")
	body := store.ScriptRunRequest{ScriptID: e.ID, ScriptSHA256: e.SHA256, Args: json.RawMessage(`{}`), Targets: []string{"machine-a"}, Reason: "probe facts"}
	digest := settingClientDigest("a")
	applies := 0
	client := settingClientServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("operator client must not invent auth")
		}
		switch r.URL.Path {
		case "/v1/operator/script-catalog":
			_ = json.NewEncoder(w).Encode(scriptcatalog.CatalogResponse{Items: scriptcatalog.List()})
		case "/v1/operator/script-runs/preview":
			if r.Header.Get("Idempotency-Key") != "" {
				t.Error("preview sent idempotency key")
			}
			_ = json.NewEncoder(w).Encode(store.ScriptRunResult{PreviewDigest: digest, Targets: body.Targets, JobIDs: []string{}})
		case "/v1/operator/script-runs":
			applies++
			var wire store.ScriptRunRequest
			if json.NewDecoder(r.Body).Decode(&wire) != nil || wire.PreviewDigest != digest || r.Header.Get("Idempotency-Key") != "script-key" {
				t.Error("apply coordinates changed")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(store.ScriptRunResult{PreviewDigest: digest, Targets: body.Targets, JobIDs: []string{"job-a"}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	if entries, err := client.ScriptCatalog(context.Background()); err != nil || len(entries) != 1 {
		t.Fatalf("catalog %v %v", entries, err)
	}
	preview, err := client.ScriptRun(context.Background(), body, false, "")
	if err != nil {
		t.Fatal(err)
	}
	body.PreviewDigest = preview.PreviewDigest
	if result, err := client.ScriptRun(context.Background(), body, true, "script-key"); err != nil || len(result.JobIDs) != 1 || applies != 1 {
		t.Fatalf("apply %+v %v", result, err)
	}
}

func TestScriptCatalogResponseIsStrict(t *testing.T) {
	raw, err := json.Marshal(scriptcatalog.CatalogResponse{Items: scriptcatalog.List()})
	if err != nil {
		t.Fatal(err)
	}
	items, err := decodeScriptCatalog(raw)
	if err != nil || len(items) != 1 || items[0].ID != "fleet-probe-v1" {
		t.Fatalf("catalog %v %v", items, err)
	}
	if items, err := decodeScriptCatalog([]byte(`{"items":[]}`)); err != nil || items == nil || len(items) != 0 {
		t.Fatalf("empty catalog %v %v", items, err)
	}
	for _, raw := range []string{`[]`, `null`, `{}`, `{"items":null}`, `{"Items":[]}`, `{"items":[],"unknown":true}`, `{"items":[],"items":[]}`, `{"items":[]} {}`, `{"items":[{"id":"incomplete"}]}`} {
		if _, err := decodeScriptCatalog([]byte(raw)); err == nil {
			t.Errorf("accepted invalid catalog: %s", raw)
		}
	}
}
