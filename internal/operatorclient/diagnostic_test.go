package operatorclient

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/store"
)

func diagnosticClientDigest(digit byte) string {
	return "sha256:" + strings.Repeat(string(digit), 64)
}

func diagnosticClientPreview() DiagnosticNoopPreviewResponse {
	jobsEnabled := true
	return DiagnosticNoopPreviewResponse{
		MachineID: "machine-1", DisplayName: "cnode", LifecycleRevision: 3,
		PreviewedAt:  time.Date(2026, 9, 10, 2, 30, 0, 0, time.UTC),
		Kind:         store.OperatorDiagnosticNoopKind,
		ResourceKind: store.OperatorDiagnosticNoopResourceKind, ResourceID: store.OperatorDiagnosticNoopResourceID,
		AgentWatermarkScope:     store.OperatorDiagnosticNoopWatermarkScope,
		SpecDigest:              fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(store.OperatorDiagnosticNoopSpec))),
		ExecutionTimeoutSeconds: 90, ChangesMachineConfiguration: false,
		CreatesDesiredState: true, CreatesJob: true, DeliveryRequiresJobsEnabled: true,
		JobsEnabled: &jobsEnabled, EverReported: true, ActiveJobCount: 0,
		CurrentResourceRevision: 7, PlannedRevision: 8,
		Blockers: []store.OperatorDiagnosticNoopBlocker{}, PreviewDigest: diagnosticClientDigest('a'),
	}
}

func diagnosticClientResult(replayed bool) DiagnosticNoopResponse {
	p := diagnosticClientPreview()
	return DiagnosticNoopResponse{
		MachineID: p.MachineID, DisplayName: p.DisplayName, DesiredID: "desired-1", JobID: "job-1",
		Revision: deploy.Revision(p.PlannedRevision), CreatedAt: time.Date(2026, 9, 10, 2, 31, 0, 0, time.UTC),
		LifecycleRevision: p.LifecycleRevision, Kind: p.Kind, ResourceKind: p.ResourceKind, ResourceID: p.ResourceID,
		AgentWatermarkScope: p.AgentWatermarkScope, SpecDigest: p.SpecDigest,
		ExecutionTimeoutSeconds:     p.ExecutionTimeoutSeconds,
		ChangesMachineConfiguration: p.ChangesMachineConfiguration, CreatesDesiredState: p.CreatesDesiredState,
		CreatesJob: p.CreatesJob, DeliveryRequiresJobsEnabled: p.DeliveryRequiresJobsEnabled,
		JobsEnabled: p.JobsEnabled, EverReported: p.EverReported,
		ActiveJobCount: p.ActiveJobCount, CurrentResourceRevision: p.CurrentResourceRevision,
		PlannedRevision: p.PlannedRevision, Blockers: p.Blockers, PreviewDigest: p.PreviewDigest,
		Replayed: replayed,
	}
}

func diagnosticClientWriteJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func TestDiagnosticNoopClientPreviewCreateReplayAndWireContract(t *testing.T) {
	preview := diagnosticClientPreview()
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent || r.URL.RawQuery != "" {
			t.Errorf("request headers/path=%s headers=%v", r.URL.String(), r.Header)
		}
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/operator/machines/machine-1/diagnostic-noop-preview":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
				r.Header.Get("Idempotency-Key") != "" || string(raw) != `{"execution_timeout_seconds":90}` {
				t.Errorf("preview request headers=%v body=%s", r.Header, raw)
			}
			diagnosticClientWriteJSON(t, w, preview)
		case "/v1/operator/machines/machine-1/diagnostic-noop-jobs":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
				r.Header.Get("Idempotency-Key") != "diagnostic-key" ||
				string(raw) != `{"execution_timeout_seconds":90,"confirm_display_name":"cnode","preview_digest":"`+preview.PreviewDigest+`","reason":"protocol drill"}` {
				t.Errorf("create request headers=%v body=%s", r.Header, raw)
			}
			call := createCalls.Add(1)
			result := diagnosticClientResult(call > 1)
			w.Header().Set("Location", "/v1/operator/jobs/"+result.JobID)
			w.Header().Set("ETag", `"diagnostic-noop-revision-8"`)
			if result.Replayed {
				w.Header().Set("Idempotency-Replayed", "true")
				diagnosticClientWriteJSON(t, w, result)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(result)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := operatorClientForServer(t, server)
	gotPreview, err := client.PreviewDiagnosticNoop(t.Context(), "machine-1", DiagnosticNoopPreviewRequest{ExecutionTimeoutSeconds: 90})
	if err != nil || gotPreview.PreviewDigest != preview.PreviewDigest || gotPreview.Blockers == nil {
		t.Fatalf("preview=%+v err=%v", gotPreview, err)
	}
	body := DiagnosticNoopRequest{ExecutionTimeoutSeconds: 90, ConfirmDisplayName: "cnode", PreviewDigest: preview.PreviewDigest, Reason: "protocol drill"}
	fresh, err := client.CreateDiagnosticNoop(t.Context(), "machine-1", "diagnostic-key", body)
	if err != nil || fresh.Replayed || fresh.JobID != "job-1" || fresh.Meta.ETagRevision != 8 || fresh.Meta.IdempotencyReplayed {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
	replay, err := client.CreateDiagnosticNoop(t.Context(), "machine-1", "diagnostic-key", body)
	if err != nil || !replay.Replayed || !replay.Meta.IdempotencyReplayed || replay.JobID != fresh.JobID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestDiagnosticNoopClientRejectsInconsistentSuccessEvidence(t *testing.T) {
	base := diagnosticClientResult(false)
	tests := []struct {
		name        string
		mutate      func(*DiagnosticNoopResponse, http.Header)
		consequence string // 空字串代表沿用泛用那句
	}{
		{"unknown field", func(result *DiagnosticNoopResponse, h http.Header) { result.ResourceID = "other" }, ""},
		{"missing execution evidence", func(result *DiagnosticNoopResponse, _ http.Header) { result.JobsEnabled = nil }, ""},
		{"wrong location", func(_ *DiagnosticNoopResponse, h http.Header) { h.Set("Location", "/v1/operator/jobs/other") }, ""},
		{"wrong etag", func(_ *DiagnosticNoopResponse, h http.Header) { h.Set("ETag", `"diagnostic-noop-revision-9"`) }, ""},
		{"replay header mismatch", func(_ *DiagnosticNoopResponse, h http.Header) { h.Set("Idempotency-Replayed", "true") }, ""},
		{"created at carries a fraction", func(result *DiagnosticNoopResponse, _ http.Header) {
			result.CreatedAt = result.CreatedAt.Add(500 * time.Millisecond)
		}, "Hub 交回的建立時間帶小數秒，client 卻把無法與 Hub 收據對帳或回送的值當成原件"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := base
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				h := w.Header()
				h.Set("Content-Type", "application/json")
				h.Set("Cache-Control", "no-store")
				h.Set("Location", "/v1/operator/jobs/job-1")
				h.Set("ETag", `"diagnostic-noop-revision-8"`)
				tc.mutate(&result, h)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(result)
			}))
			t.Cleanup(server.Close)
			client := operatorClientForServer(t, server)
			_, err := client.CreateDiagnosticNoop(t.Context(), "machine-1", "diagnostic-key", DiagnosticNoopRequest{
				ExecutionTimeoutSeconds: 90, ConfirmDisplayName: "cnode",
				PreviewDigest: diagnosticClientDigest('a'), Reason: "protocol drill",
			})
			if err == nil {
				if tc.consequence == "" {
					t.Errorf("inconsistent success response %q was accepted", tc.name)
					return
				}
				t.Errorf("inconsistent success response %q was accepted: %s", tc.name, tc.consequence)
			}
		})
	}
}

func TestDiagnosticNoopClientRejectsAFractionalPreviewTime(t *testing.T) {
	preview := diagnosticClientPreview()
	preview.PreviewedAt = preview.PreviewedAt.Add(500 * time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		diagnosticClientWriteJSON(t, w, preview)
	}))
	t.Cleanup(server.Close)
	client := operatorClientForServer(t, server)
	_, err := client.PreviewDiagnosticNoop(t.Context(), "machine-1", DiagnosticNoopPreviewRequest{
		ExecutionTimeoutSeconds: 90,
	})
	if err == nil {
		t.Errorf("Hub 交回的預覽時間帶小數秒，client 卻把無法與 Hub 收據對帳或回送的值當成原件")
	}
}
