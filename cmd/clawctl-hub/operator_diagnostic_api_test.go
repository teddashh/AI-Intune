package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func diagnosticOperatorRequest(t *testing.T, mux *http.ServeMux, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "100.64.0.7:41234"
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req = verifiedOperatorRequest(req, operatorauth.Operate)
	mux.ServeHTTP(rec, req)
	return rec
}

func recordDiagnosticCheckin(t *testing.T, f jobsFixture) {
	t.Helper()
	jobsEnabled := true
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: jobsTestNow,
		AgentVersion: "diagnostic-api-test", BootID: "boot-diagnostic", AgentSeq: 1,
		JobsEnabled: &jobsEnabled,
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorDiagnosticNoopPreviewCreateReplayAndAudit(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	base := "/v1/operator/machines/" + f.machine.id
	previewRec := diagnosticOperatorRequest(t, f.mux, base+"/diagnostic-noop-preview", "",
		`{"execution_timeout_seconds":90}`)
	if previewRec.Code != http.StatusOK || previewRec.Header().Get("Cache-Control") != "no-store" ||
		previewRec.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("preview=%d headers=%v body=%s", previewRec.Code, previewRec.Header(), previewRec.Body.String())
	}
	var preview store.OperatorDiagnosticNoopPreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.MachineID != f.machine.id || preview.DisplayName != "cnode-operator" ||
		preview.ExecutionTimeoutSeconds != 90 || preview.ChangesMachineConfiguration || !preview.EverReported ||
		preview.JobsEnabled == nil || !*preview.JobsEnabled ||
		preview.ActiveJobCount != 0 || preview.Blockers == nil || len(preview.Blockers) != 0 || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v", preview)
	}
	body := fmt.Sprintf(`{"execution_timeout_seconds":90,"confirm_display_name":"cnode-operator","preview_digest":%q,"reason":"API protocol drill"}`, preview.PreviewDigest)
	fresh := diagnosticOperatorRequest(t, f.mux, base+"/diagnostic-noop-jobs", "diagnostic-api-key", body)
	if fresh.Code != http.StatusCreated || fresh.Header().Get("Idempotency-Replayed") != "" ||
		fresh.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("fresh=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var result store.OperatorDiagnosticNoopResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.JobID == "" || result.DesiredID == "" || result.MachineID != f.machine.id ||
		result.DisplayName != "cnode-operator" || result.Replayed || result.Revision != result.PlannedRevision ||
		fresh.Header().Get("Location") != "/v1/operator/jobs/"+result.JobID ||
		fresh.Header().Get("ETag") != fmt.Sprintf(`"diagnostic-noop-revision-%d"`, result.Revision) {
		t.Fatalf("result=%+v headers=%v", result, fresh.Header())
	}
	replay := diagnosticOperatorRequest(t, f.mux, base+"/diagnostic-noop-jobs", "diagnostic-api-key", body)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	var replayed store.OperatorDiagnosticNoopResult
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil || !replayed.Replayed ||
		replayed.JobID != result.JobID || replayed.DesiredID != result.DesiredID {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditDiagnosticNoop || entry.AuthSubject != "tailscale-user:42" ||
			entry.AuthCapability != "example.com/cap/clawctl-operate" || entry.SourceKind != "operator-api" ||
			entry.RequestDigest == "" || entry.IdempotencyKey != "diagnostic-api-key" || !entry.OK {
			t.Fatalf("audit=%+v", entry)
		}
	}
}

func TestOperatorDiagnosticNoopTransportAndDomainRejections(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	base := "/v1/operator/machines/" + f.machine.id
	bad := diagnosticOperatorRequest(t, f.mux, base+"/diagnostic-noop-jobs", "transport-key", `{"unknown":true}`)
	assertAPIError(t, bad, http.StatusBadRequest, "BAD_REQUEST")
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != store.AuditDiagnosticNoop ||
		!entries[0].IsOperatorTransportRejection() || entries[0].RequestDigest != "" {
		t.Fatalf("transport audit=%+v err=%v", entries, err)
	}
	preview := diagnosticOperatorRequest(t, f.mux, base+"/diagnostic-noop-preview", "", `{"execution_timeout_seconds":60}`)
	var p store.OperatorDiagnosticNoopPreviewResult
	if err := json.Unmarshal(preview.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"execution_timeout_seconds":60,"confirm_display_name":"wrong","preview_digest":%q,"reason":"test mismatch"}`, p.PreviewDigest)
	rejected := diagnosticOperatorRequest(t, f.mux, base+"/diagnostic-noop-jobs", "domain-key", body)
	assertAPIError(t, rejected, http.StatusBadRequest, store.OperatorCodeConfirmationMismatch)
	replayed := diagnosticOperatorRequest(t, f.mux, base+"/diagnostic-noop-jobs", "domain-key", body)
	if replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("rejection replay headers=%v body=%s", replayed.Header(), replayed.Body.String())
	}
}
