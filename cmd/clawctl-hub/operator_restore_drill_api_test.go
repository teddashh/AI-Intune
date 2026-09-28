package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/restoredrill"
	"github.com/teddashh/AI-Intune/internal/store"
)

func operatorRestoreDrillAPIFixture(t *testing.T) (jobsFixture, *operator.Service, string) {
	t.Helper()
	f := newJobsFixture(t, "restore-drill-api-machine")
	directory := filepath.Join(t.TempDir(), "backups")
	makeBackup(t, directory, "clawctl-20260911T120000Z-before-api.sqlite", time.Now().UTC(), 1)
	stamp := filepath.Join(t.TempDir(), "restore-drill.stamp")
	service := operator.New(f.store)
	service.ConfigureRestoreDrill(restoredrill.Runner{BackupsDir: directory, StampPath: stamp, Live: f.store})
	(&hub{store: f.store, operatorService: service, retention: store.DefaultRetention()}).operatorRoutes(f.mux)
	return f, service, stamp
}

func TestOperatorRestoreDrillAPIPreviewCreateWorkerReadAndReplay(t *testing.T) {
	f, service, stamp := operatorRestoreDrillAPIFixture(t)
	previewRec := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/maintenance/restore-drill-preview", "", `{}`)
	if previewRec.Code != http.StatusOK || previewRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview=%d headers=%v body=%s", previewRec.Code, previewRec.Header(), previewRec.Body.String())
	}
	var preview operator.RestoreDrillPreview
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil || preview.Backup.Name == "" ||
		preview.PreviewDigest == "" || preview.Confirmation != store.RestoreDrillConfirmation(preview.Backup.Name) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	body, _ := json.Marshal(restoreDrillApplyBody{
		PreviewDigest: &preview.PreviewDigest, Confirm: &preview.Confirmation,
		Reason: func() *string { value := "quarterly recovery verification"; return &value }(),
	})
	fresh := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/maintenance/restore-drills", "restore-drill-api-key", string(body))
	if fresh.Code != http.StatusAccepted || fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("fresh=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var created store.OperatorRestoreDrillResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &created); err != nil || created.Replayed ||
		created.Operation.State != store.RestoreDrillQueued {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	list := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/maintenance/restore-drills?limit=5", "", "")
	var listed store.RestoreDrillListResult
	if list.Code != http.StatusOK || json.Unmarshal(list.Body.Bytes(), &listed) != nil ||
		listed.Total != 1 || len(listed.Items) != 1 {
		t.Fatalf("list=%d result=%+v body=%s", list.Code, listed, list.Body.String())
	}
	if processed, err := service.RunQueuedRestoreDrillOperations(t.Context()); err != nil || processed != 1 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	detail := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/maintenance/restore-drills/"+created.Operation.OperationID, "", "")
	var completed store.RestoreDrillOperation
	if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &completed) != nil ||
		completed.State != store.RestoreDrillSucceeded || completed.Machines == nil || *completed.Machines != 1 {
		t.Fatalf("detail=%d completed=%+v body=%s", detail.Code, completed, detail.Body.String())
	}
	if _, ok := restoredrill.ReadStamp(stamp); !ok {
		t.Fatal("successful API operation did not write stamp")
	}
	replay := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/maintenance/restore-drills", "restore-drill-api-key", string(body))
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &created); err != nil || !created.Replayed ||
		created.Operation.State != store.RestoreDrillSucceeded {
		t.Fatalf("replay result=%+v err=%v", created, err)
	}
	audits, err := f.store.ListAuditReads(store.AuditReadFilter{Actions: []store.AuditAction{store.AuditRestoreDrill}, Limit: 10})
	if err != nil || len(audits.Items) != 2 || audits.Items[0].AuthSubject != "tailscale-user:42" ||
		audits.Items[0].AuthCapability != "example.com/cap/clawctl-admin" {
		t.Fatalf("audits=%+v err=%v", audits, err)
	}
}

func TestOperatorRestoreDrillAPIRejectsMalformedTransportAndQueries(t *testing.T) {
	f, _, _ := operatorRestoreDrillAPIFixture(t)
	for _, test := range []struct {
		method, path, key, body string
		want                    int
	}{
		{http.MethodPost, "/v1/operator/maintenance/restore-drill-preview?unexpected=1", "", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/v1/operator/maintenance/restore-drills", "transport-key", `{"preview_digest":"x","preview_digest":"y","confirm":"x","reason":"x"}`, http.StatusBadRequest},
		{http.MethodGet, "/v1/operator/maintenance/restore-drills?limit=101", "", "", http.StatusBadRequest},
		{http.MethodGet, "/v1/operator/maintenance/restore-drills/unknown?x=1", "", "", http.StatusBadRequest},
	} {
		rec := operatorRequest(t, f.mux, test.method, test.path, test.key, test.body)
		if rec.Code != test.want || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") ||
			rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s status=%d headers=%v body=%s", test.method, test.path, rec.Code, rec.Header(), rec.Body.String())
		}
	}
	var transportAudits int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND idempotency_key='transport-key'
 AND detail LIKE ?`, store.AuditRestoreDrill, store.OperatorTransportRejectionPrefix+"%").Scan(&transportAudits); err != nil || transportAudits != 1 {
		t.Fatalf("transportAudits=%d err=%v", transportAudits, err)
	}
}
