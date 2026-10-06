package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func operatorRetentionFixture(t *testing.T) jobsFixture {
	t.Helper()
	f := newJobsFixture(t, "retention-api-machine")
	now := time.Now().UTC().Truncate(time.Second)
	for _, at := range []time.Time{now.Add(-40 * 24 * time.Hour), now.Add(-24 * time.Hour)} {
		if _, err := f.store.DB().Exec(`INSERT OR IGNORE INTO machine_checkins
 (machine_id,sent_at,received_at,agent_version,boot_id,agent_seq)
 VALUES (?,?,?,?,?,?)`, f.machine.id, at.Format(time.RFC3339), at.Format(time.RFC3339),
			"test", "retention-api", at.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	(&hub{store: f.store, retention: store.DefaultRetention()}).operatorRoutes(f.mux)
	return f
}

func TestOperatorRetentionReadPreviewApplyAndReplay(t *testing.T) {
	f := operatorRetentionFixture(t)
	read := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/maintenance/retention", "", "")
	if read.Code != http.StatusOK || read.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("read=%d headers=%v body=%s", read.Code, read.Header(), read.Body.String())
	}
	var status operator.RetentionStatus
	if err := json.Unmarshal(read.Body.Bytes(), &status); err != nil || status.SchemaVersion != 1 || status.HasRun {
		t.Fatalf("status=%+v err=%v", status, err)
	}

	previewRec := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/maintenance/retention/prune-preview", "", `{}`)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var preview operator.RetentionPrunePreview
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil || preview.TotalDeleted < 1 ||
		preview.Confirmation == "" || preview.PreviewDigest == "" || len(preview.Counts) != 5 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	body, _ := json.Marshal(retentionPruneApplyBody{
		EvaluatedAt: preview.EvaluatedAt, Policy: &preview.Policy,
		ExpectedRevision: &preview.ExpectedRevision, Confirm: preview.Confirmation,
		PreviewDigest: preview.PreviewDigest, Reason: "operator retention test",
	})
	fresh := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/maintenance/retention/prunes", "retention-api-key", string(body))
	if fresh.Code != http.StatusOK || fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("fresh=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var result store.OperatorPruneResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &result); err != nil || result.TotalDeleted != preview.TotalDeleted ||
		result.Replayed || result.Revision <= preview.ExpectedRevision {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/maintenance/retention/prunes", "retention-api-key", string(body))
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &result); err != nil || !result.Replayed {
		t.Fatalf("replay result=%+v err=%v", result, err)
	}
	page, err := f.store.ListAuditReads(store.AuditReadFilter{
		Actions: []store.AuditAction{store.AuditRetentionPrune}, Limit: 10,
	})
	if err != nil || len(page.Items) != 2 || page.Items[0].AuthSubject != "tailscale-user:42" ||
		page.Items[0].AuthCapability != "example.com/cap/clawctl-admin" {
		t.Fatalf("audit=%+v err=%v", page, err)
	}
}

func TestOperatorRetentionTransportRejectionDoesNotOccupyKey(t *testing.T) {
	f := operatorRetentionFixture(t)
	rec := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/maintenance/retention/prunes", "retention-transport", `{"unknown":true}`)
	assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	var receipts int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
		"retention-transport").Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("receipts=%d err=%v", receipts, err)
	}
	page, err := f.store.ListAuditReads(store.AuditReadFilter{
		Actions: []store.AuditAction{store.AuditRetentionPrune}, Limit: 10,
	})
	if err != nil || len(page.Items) != 1 || !page.Items[0].IsOperatorTransportRejection() {
		t.Fatalf("audit=%+v err=%v", page, err)
	}
}
