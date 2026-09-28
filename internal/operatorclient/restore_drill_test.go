package operatorclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func restoreDrillClientPreview() operator.RestoreDrillPreview {
	result := operator.RestoreDrillPreview{
		SchemaVersion: 1, EvaluatedAt: time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC),
		Backup: store.RestoreDrillBackup{
			Name: "clawctl-20260911T120000Z-before-test.sqlite", SizeBytes: 4096,
			ModifiedAt: time.Date(2026, 9, 11, 11, 55, 0, 123, time.UTC),
			SHA256:     "sha256:" + strings.Repeat("a", 64),
		},
		LiveExpected: 3,
	}
	result.Confirmation = store.RestoreDrillConfirmation(result.Backup.Name)
	raw, _ := json.Marshal(struct {
		SchemaVersion int                      `json:"schema_version"`
		Backup        store.RestoreDrillBackup `json:"backup"`
		LiveExpected  int                      `json:"live_expected"`
		Confirmation  string                   `json:"confirmation"`
	}{result.SchemaVersion, result.Backup, result.LiveExpected, result.Confirmation})
	hash := sha256.Sum256(raw)
	result.PreviewDigest = "sha256:" + hex.EncodeToString(hash[:])
	return result
}

func restoreDrillQueuedOperation(preview operator.RestoreDrillPreview) store.RestoreDrillOperation {
	return store.RestoreDrillOperation{
		OperationID: "0123456789abcdef0123456789abcdef", Backup: preview.Backup,
		PreviewDigest: preview.PreviewDigest, LiveExpectedAtPreview: preview.LiveExpected,
		State: store.RestoreDrillQueued, Phase: store.RestoreDrillPhaseQueued,
		CreatedAt: preview.EvaluatedAt, UpdatedAt: preview.EvaluatedAt,
	}
}

func TestRestoreDrillClientPreviewCreateListAndShow(t *testing.T) {
	preview := restoreDrillClientPreview()
	operation := restoreDrillQueuedOperation(preview)
	createCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/maintenance/restore-drill-preview":
			_ = json.NewEncoder(w).Encode(preview)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/maintenance/restore-drills":
			createCalls++
			if r.Header.Get("Idempotency-Key") != "restore-drill-key" {
				t.Errorf("key=%q", r.Header.Get("Idempotency-Key"))
			}
			var body RestoreDrillApplyRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PreviewDigest != preview.PreviewDigest {
				t.Errorf("body=%+v err=%v", body, err)
			}
			result := store.OperatorRestoreDrillResult{Operation: operation, Replayed: createCalls == 2}
			if result.Replayed {
				w.Header().Set("Idempotency-Replayed", "true")
			} else {
				w.WriteHeader(http.StatusAccepted)
			}
			_ = json.NewEncoder(w).Encode(result)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operator/maintenance/restore-drills":
			_ = json.NewEncoder(w).Encode(store.RestoreDrillListResult{
				SchemaVersion: 1, Consistency: store.RestoreDrillReadConsistency,
				EvaluatedAt: preview.EvaluatedAt, Total: 1, Items: []store.RestoreDrillOperation{operation},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operator/maintenance/restore-drills/"+operation.OperationID:
			_ = json.NewEncoder(w).Encode(operation)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	gotPreview, err := client.PreviewRestoreDrill(t.Context())
	if err != nil || gotPreview.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("preview=%+v err=%v", gotPreview, err)
	}
	body := RestoreDrillApplyRequest{PreviewDigest: preview.PreviewDigest, Confirm: preview.Confirmation, Reason: "quarterly test"}
	if result, err := client.CreateRestoreDrill(t.Context(), "restore-drill-key", body); err != nil || result.Replayed {
		t.Fatalf("create=%+v err=%v", result, err)
	}
	if result, err := client.CreateRestoreDrill(t.Context(), "restore-drill-key", body); err != nil || !result.Replayed {
		t.Fatalf("replay=%+v err=%v", result, err)
	}
	if result, err := client.RestoreDrillOperations(t.Context(), 10); err != nil || result.Total != 1 || len(result.Items) != 1 {
		t.Fatalf("list=%+v err=%v", result, err)
	}
	if result, err := client.RestoreDrillOperation(t.Context(), operation.OperationID); err != nil || result.OperationID != operation.OperationID {
		t.Fatalf("show=%+v err=%v", result, err)
	}
}

func TestRestoreDrillClientRejectsUntrustedRepresentations(t *testing.T) {
	preview := restoreDrillClientPreview()
	queued := restoreDrillQueuedOperation(preview)
	tests := []struct {
		name string
		body any
		call func(*Client) error
	}{
		{name: "nested unknown preview field", body: map[string]any{
			"schema_version": preview.SchemaVersion, "evaluated_at": preview.EvaluatedAt,
			"backup": map[string]any{"name": preview.Backup.Name, "size_bytes": preview.Backup.SizeBytes,
				"modified_at": preview.Backup.ModifiedAt, "sha256": preview.Backup.SHA256, "unknown": true},
			"live_expected": preview.LiveExpected, "confirmation": preview.Confirmation, "preview_digest": preview.PreviewDigest,
		}, call: func(client *Client) error { _, err := client.PreviewRestoreDrill(t.Context()); return err }},
		{name: "contradictory queued result", body: func() any {
			value := queued
			machines := 1
			value.Machines = &machines
			return value
		}(), call: func(client *Client) error {
			_, err := client.RestoreDrillOperation(t.Context(), queued.OperationID)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_ = json.NewEncoder(w).Encode(test.body)
			}))
			defer server.Close()
			if err := test.call(operatorClientForServer(t, server)); err == nil {
				t.Fatal("untrusted representation was accepted")
			}
		})
	}
}

func TestRestoreDrillPreviewRejectsADigestThatDoesNotBindTheBackupShown(t *testing.T) {
	honest := restoreDrillClientPreview()
	preview := func(result operator.RestoreDrillPreview) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(result)
		}))
		defer server.Close()
		_, err := operatorClientForServer(t, server).PreviewRestoreDrill(t.Context())
		return err
	}

	if err := preview(honest); err != nil {
		t.Fatalf("honest preview was rejected, so the case below would not "+
			"measure the digest binding under test: %v", err)
	}

	tampered := honest
	tampered.PreviewDigest = "sha256:" + strings.Repeat("0", 64)
	const expected = "operator client: restore drill preview digest mismatch"
	if err := preview(tampered); err == nil || err.Error() != expected {
		t.Errorf("got error %v, expected %q; without this rejection, CLI run can "+
			"re-preview and forward a digest bound to different backup facts while "+
			"printing no backup fields; because --confirm binds only the filename, "+
			"the Hub may execute and attest facts different from the numbers the "+
			"operator approved", err, expected)
	}
}
