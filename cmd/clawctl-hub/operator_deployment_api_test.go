package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestOperatorDeploymentListIsCanonicalNoStoreRead(t *testing.T) {
	f := newJobsFixture(t, "deployment-read")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)

	rec := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/deployments?channel=canary&state=running&stuck=false&limit=7", "", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var result operator.DeploymentListResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != operator.DeploymentReadSchemaVersion ||
		result.Consistency != operator.DeploymentReadConsistencyLive || result.Items == nil ||
		result.StateCounts == nil || result.Total != 0 || result.NextCursor != nil {
		t.Fatalf("list result=%+v", result)
	}
}

func TestOperatorDeploymentListRejectsAmbiguousQueries(t *testing.T) {
	tests := []string{
		"?",
		"?unknown=value",
		"?channel=canary&channel=stable",
		"?state=running&state=running",
		"?stuck=1",
		"?limit=01x",
		"?cursor=%20opaque",
	}
	for _, query := range tests {
		t.Run(query, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/operator/deployments"+query, nil)
			if _, err := parseOperatorDeploymentListRequest(request); err == nil {
				t.Fatalf("query %q was accepted", query)
			}
		})
	}
}

func TestOperatorDeploymentPreviewRequiresExactCompleteJSON(t *testing.T) {
	f := newJobsFixture(t, "deployment-preview")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	path := "/v1/operator/deployments/preview"
	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
		code        string
	}{
		{"wrong media", "text/plain", `{}`, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"},
		{"missing field", "application/json", `{"channel":"canary"}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"unknown field", "application/json", `{"channel":"canary","version":"v","artifact_sha256":"","batch_size":1,"execution_timeout_seconds":600,"irreversible":false,"spec":"raw"}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"case alias", "application/json", `{"Channel":"canary","version":"v","artifact_sha256":"","batch_size":1,"execution_timeout_seconds":600,"irreversible":false}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"duplicate", "application/json", `{"channel":"canary","channel":"stable","version":"v","artifact_sha256":"","batch_size":1,"execution_timeout_seconds":600,"irreversible":false}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"null", "application/json", `{"channel":"canary","version":null,"artifact_sha256":"","batch_size":1,"execution_timeout_seconds":600,"irreversible":false}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"explicit zero batch", "application/json", `{"channel":"canary","version":"v","artifact_sha256":"","batch_size":0,"execution_timeout_seconds":600,"irreversible":false}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"explicit zero timeout", "application/json", `{"channel":"canary","version":"v","artifact_sha256":"","batch_size":1,"execution_timeout_seconds":0,"irreversible":false}`, http.StatusBadRequest, "BAD_REQUEST"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", test.contentType)
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			f.mux.ServeHTTP(rec, req)
			assertAPIError(t, rec, test.status, test.code)
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("missing no-store: %v", rec.Header())
			}
		})
	}
}

func TestOperatorDeploymentDetailRejectsQueryAndUnknownID(t *testing.T) {
	f := newJobsFixture(t, "deployment-detail")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)

	query := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/deployments/missing?view=raw", "", "")
	assertAPIError(t, query, http.StatusBadRequest, "BAD_REQUEST")
	missing := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/deployments/missing", "", "")
	assertAPIError(t, missing, http.StatusNotFound, "DEPLOYMENT_NOT_FOUND")
}

func TestOperatorDeploymentCreateFreshReplayAndSafeResponse(t *testing.T) {
	f, record, request, preview := operatorDeploymentCreateFixture(t, 1)
	body := fmt.Sprintf(`{
 "channel":"canary","version":%q,"artifact_sha256":%q,
 "batch_size":1,"execution_timeout_seconds":600,"irreversible":false,
 "preview_digest":%q,"confirm_channel":"canary","confirm_version":%q,"reason":"scheduled rollout"
}`, record.Version, record.SHA256, preview.PreviewDigest, record.Version)

	fresh := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/deployments", "deployment-create-api", body)
	if fresh.Code != http.StatusCreated || fresh.Header().Get("Idempotency-Replayed") != "" ||
		fresh.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("fresh=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var created operator.DeploymentMutationResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &created); err != nil || created.Replayed ||
		created.Action != "create" || created.Channel != request.Channel || created.DeploymentID == "" ||
		created.ControlRevision != 0 || created.OpenedBatch != 1 || len(created.Jobs) != 1 ||
		created.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	for _, forbidden := range []string{"created_by", `\"spec\"`, "tarball_url", "fetched_by", "scheduled rollout"} {
		if strings.Contains(fresh.Body.String(), forbidden) {
			t.Fatalf("safe mutation response contains %q: %s", forbidden, fresh.Body.String())
		}
	}

	// A historical replay must not need today's artifact catalog. This models a
	// lost 201 response after the artifact was subsequently quarantined.
	if err := os.Remove(filepath.Join(f.artifactsDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/deployments", "deployment-create-api", body)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	var repeated operator.DeploymentMutationResult
	if err := json.Unmarshal(replay.Body.Bytes(), &repeated); err != nil || !repeated.Replayed ||
		repeated.DeploymentID != created.DeploymentID || len(repeated.Jobs) != 1 ||
		repeated.Jobs[0].JobID != created.Jobs[0].JobID {
		t.Fatalf("replayed=%+v err=%v", repeated, err)
	}
	var deployments int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&deployments); err != nil || deployments != 1 {
		t.Fatalf("deployments=%d err=%v", deployments, err)
	}
}

func TestOperatorDeploymentCreateRequiresEveryPlanningFieldBeforeLedger(t *testing.T) {
	f, record, _, preview := operatorDeploymentCreateFixture(t, 1)
	complete := map[string]any{
		"channel": "canary", "version": record.Version, "artifact_sha256": record.SHA256,
		"batch_size": 1, "execution_timeout_seconds": 600, "irreversible": false,
		"preview_digest": preview.PreviewDigest, "confirm_channel": "canary",
		"confirm_version": record.Version, "reason": "all planning fields are explicit",
	}
	planningFields := []string{
		"channel", "version", "artifact_sha256", "batch_size", "execution_timeout_seconds", "irreversible",
	}
	for _, missing := range planningFields {
		t.Run(missing, func(t *testing.T) {
			body := make(map[string]any, len(complete)-1)
			for key, value := range complete {
				if key != missing {
					body[key] = value
				}
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			key := "deployment-missing-" + strings.ReplaceAll(missing, "_", "-")
			rec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/deployments", key, string(raw))
			assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
			if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Idempotency-Replayed") != "" {
				t.Fatalf("headers=%v", rec.Header())
			}
			var receipts int
			if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if receipts != 0 {
				t.Fatalf("missing %s occupied idempotency ledger: receipts=%d", missing, receipts)
			}
		})
	}
	for _, zero := range []string{"batch_size", "execution_timeout_seconds"} {
		t.Run("explicit-zero-"+zero, func(t *testing.T) {
			body := make(map[string]any, len(complete))
			for key, value := range complete {
				body[key] = value
			}
			body[zero] = 0
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			key := "deployment-zero-" + strings.ReplaceAll(zero, "_", "-")
			rec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/deployments", key, string(raw))
			assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
			if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Idempotency-Replayed") != "" {
				t.Fatalf("headers=%v", rec.Header())
			}
			var receipts int
			if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if receipts != 0 {
				t.Fatalf("explicit zero %s occupied idempotency ledger: receipts=%d", zero, receipts)
			}
		})
	}
	var deployments int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&deployments); err != nil {
		t.Fatal(err)
	}
	if deployments != 0 {
		t.Fatalf("missing planning fields created deployments=%d", deployments)
	}
	entries, err := f.store.Audit("", 10)
	if err != nil || len(entries) != len(planningFields)+2 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditDeploymentCreate || entry.OK || !entry.IsOperatorTransportRejection() ||
			entry.RequestDigest != "" {
			t.Fatalf("transport audit=%+v", entry)
		}
	}
}

func TestOperatorDeploymentConfirmationMismatchDetailIsAccurateForEveryAction(t *testing.T) {
	const wantDetail = "deployment typed confirmation 與操作目標不符"
	for _, action := range []string{"create", "continue", "retry", "abandon"} {
		t.Run(action, func(t *testing.T) {
			var f jobsFixture
			var path, body string
			if action == "create" {
				var record artifactSidecar
				var preview operator.DeploymentCreatePreviewResult
				f, record, _, preview = operatorDeploymentCreateFixture(t, 1)
				path = "/v1/operator/deployments"
				body = fmt.Sprintf(`{
				 "channel":"canary","version":%q,"artifact_sha256":%q,
				 "batch_size":1,"execution_timeout_seconds":600,"irreversible":false,
				 "preview_digest":%q,"confirm_channel":"stable","confirm_version":%q,"reason":"mismatch"
				}`, record.Version, record.SHA256, preview.PreviewDigest, record.Version)
			} else {
				machineCount := 2
				if action == "retry" {
					machineCount = 1
				}
				var record artifactSidecar
				f, record = deploymentMutationFixture(t, machineCount)
				parent, _ := seedPausedDeploymentMutation(t, f, record, machineCount)
				base := "/v1/operator/deployments/" + parent.DeploymentID
				previewLeaf := map[string]string{
					"continue": "/continuation-preview",
					"retry":    "/retry-preview",
					"abandon":  "/abandonment-preview",
				}[action]
				previewRec := operatorRequest(t, f.mux, http.MethodPost, base+previewLeaf, "", `{}`)
				if previewRec.Code != http.StatusOK {
					t.Fatalf("preview=%d body=%s", previewRec.Code, previewRec.Body.String())
				}
				var preview operator.DeploymentActionPreviewResult
				if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
					t.Fatal(err)
				}
				switch action {
				case "continue":
					path = base + "/continuations"
					body = fmt.Sprintf(`{
					 "preview_digest":%q,"expected_control_revision":%d,"expected_opened_batch":%d,
					 "confirm_channel":"stable","reason":"mismatch"
					}`, preview.PreviewDigest, preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch)
				case "retry":
					path = base + "/retries"
					body = fmt.Sprintf(`{
					 "preview_digest":%q,"expected_control_revision":%d,"expected_opened_batch":%d,
					 "confirm_channel":"canary","confirm_version":"wrong-version","reason":"mismatch"
					}`, preview.PreviewDigest, preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch)
				case "abandon":
					path = base + "/abandonments"
					body = fmt.Sprintf(`{
					 "preview_digest":%q,"expected_control_revision":%d,"expected_opened_batch":%d,
					 "confirm_deployment_id":"wrong-deployment","reason":"mismatch"
					}`, preview.PreviewDigest, preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch)
				}
			}

			key := "deployment-confirmation-mismatch-" + action
			for attempt := 0; attempt < 2; attempt++ {
				rec := operatorRequest(t, f.mux, http.MethodPost, path, key, body)
				assertAPIError(t, rec, http.StatusBadRequest, store.OperatorCodeDeploymentConfirmationMismatch)
				var apiError model.APIError
				if err := json.Unmarshal(rec.Body.Bytes(), &apiError); err != nil || apiError.Message != wantDetail {
					t.Fatalf("attempt=%d error=%+v decode=%v body=%s", attempt, apiError, err, rec.Body.String())
				}
				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				if _, leaked := envelope["replayed"]; leaked {
					t.Fatalf("attempt=%d replay state leaked into error body: %s", attempt, rec.Body.String())
				}
				wantReplay := ""
				if attempt == 1 {
					wantReplay = "true"
				}
				if got := rec.Header().Get("Idempotency-Replayed"); got != wantReplay {
					t.Fatalf("attempt=%d replay header=%q want=%q", attempt, got, wantReplay)
				}
			}
		})
	}
}

func TestOperatorDeploymentContinuePreviewApplyAndReplay(t *testing.T) {
	f, record, _, _ := operatorDeploymentCreateFixture(t, 2)
	// Run create once through the canonical API.
	previewJSON := fmt.Sprintf(`{"channel":"canary","version":%q,"artifact_sha256":%q,"batch_size":1,"execution_timeout_seconds":600,"irreversible":false}`,
		record.Version, record.SHA256)
	previewRec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/deployments/preview", "", previewJSON)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview=%d %s", previewRec.Code, previewRec.Body.String())
	}
	var createPreview operator.DeploymentCreatePreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &createPreview); err != nil {
		t.Fatal(err)
	}
	createBody := fmt.Sprintf(`{
 "channel":"canary","version":%q,"artifact_sha256":%q,
 "batch_size":1,"execution_timeout_seconds":600,"irreversible":false,
 "preview_digest":%q,"confirm_channel":"canary","confirm_version":%q,"reason":"two batches"
}`, createPreview.Artifact.Version, createPreview.Artifact.SHA256, createPreview.PreviewDigest, createPreview.Artifact.Version)
	createdRec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/deployments", "deployment-two-batches", createBody)
	if createdRec.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", createdRec.Code, createdRec.Body.String())
	}
	var created operator.DeploymentMutationResult
	if err := json.Unmarshal(createdRec.Body.Bytes(), &created); err != nil || len(created.Jobs) != 1 {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	failedAt := time.Now().UTC().Truncate(time.Second)
	if _, err := f.store.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Failed, failedAt.Format(time.RFC3339Nano), created.Jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.store.SetDeploymentState(created.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, failedAt); err != nil || !changed {
		t.Fatalf("pause changed=%t err=%v", changed, err)
	}

	base := "/v1/operator/deployments/" + created.DeploymentID
	controlPreviewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/continuation-preview", "", `{}`)
	if controlPreviewRec.Code != http.StatusOK {
		t.Fatalf("continue preview=%d %s", controlPreviewRec.Code, controlPreviewRec.Body.String())
	}
	var controlPreview operator.DeploymentActionPreviewResult
	if err := json.Unmarshal(controlPreviewRec.Body.Bytes(), &controlPreview); err != nil ||
		!controlPreview.Eligibility.Eligible || controlPreview.Eligibility.AffectedTargets != 1 ||
		controlPreview.Deployment.ControlRevision != 1 || controlPreview.Deployment.OpenedBatch != 1 {
		t.Fatalf("continue preview=%+v err=%v", controlPreview, err)
	}
	continueBody := fmt.Sprintf(`{
 "preview_digest":%q,"expected_control_revision":1,"expected_opened_batch":1,
 "confirm_channel":"canary","reason":"reviewed next batch"
}`, controlPreview.PreviewDigest)
	fresh := operatorRequest(t, f.mux, http.MethodPost, base+"/continuations", "deployment-continue-api", continueBody)
	if fresh.Code != http.StatusOK || fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("continue=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var continued operator.DeploymentMutationResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &continued); err != nil || continued.Replayed ||
		continued.Action != "continue" || continued.ControlRevision != 2 || continued.OpenedBatch != 2 ||
		continued.State != store.DeploymentRunning || len(continued.Jobs) != 1 {
		t.Fatalf("continued=%+v err=%v", continued, err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPost, base+"/continuations", "deployment-continue-api", continueBody)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("continue replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
}

func TestOperatorDeploymentRetryRouteCreatesExactChildAndReplaysWithoutArtifact(t *testing.T) {
	f, record, _, preview := operatorDeploymentCreateFixture(t, 1)
	parent := createOperatorDeploymentViaAPI(t, f, record, preview, "deployment-retry-parent")
	failedAt := time.Now().UTC().Truncate(time.Second)
	if _, err := f.store.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Failed, failedAt.Format(time.RFC3339Nano), parent.Jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.store.SetDeploymentState(parent.DeploymentID, store.DeploymentRunning,
		store.DeploymentPaused, failedAt); err != nil || !changed {
		t.Fatalf("pause changed=%t err=%v", changed, err)
	}

	base := "/v1/operator/deployments/" + parent.DeploymentID
	previewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/retry-preview", "", `{}`)
	if previewRec.Code != http.StatusOK || previewRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("retry preview=%d headers=%v body=%s", previewRec.Code, previewRec.Header(), previewRec.Body.String())
	}
	var retryPreview operator.DeploymentActionPreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &retryPreview); err != nil ||
		!retryPreview.Eligibility.Eligible || len(retryPreview.TerminalFailureTargets) != 1 ||
		retryPreview.TerminalFailureTargets[0].MachineID != parent.Jobs[0].MachineID {
		t.Fatalf("retry preview=%+v err=%v", retryPreview, err)
	}
	body := fmt.Sprintf(`{
 "preview_digest":%q,"expected_control_revision":%d,"expected_opened_batch":%d,
 "confirm_channel":%q,"confirm_version":%q,"reason":"retry exact terminal failure"
}`, retryPreview.PreviewDigest, retryPreview.Deployment.ControlRevision,
		retryPreview.Deployment.OpenedBatch, retryPreview.Deployment.Channel, record.Version)
	fresh := operatorRequest(t, f.mux, http.MethodPost, base+"/retries", "deployment-retry-api", body)
	if fresh.Code != http.StatusCreated || fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("retry=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var child operator.DeploymentMutationResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &child); err != nil || child.Replayed ||
		child.RetryOf == nil || *child.RetryOf != parent.DeploymentID ||
		child.DeploymentID == parent.DeploymentID || len(child.Jobs) != 1 ||
		child.Jobs[0].MachineID != parent.Jobs[0].MachineID {
		t.Fatalf("retry child=%+v err=%v", child, err)
	}
	storedParent, err := f.store.Deployment(parent.DeploymentID)
	if err != nil || storedParent.State != store.DeploymentFinished || storedParent.ControlRevision != 2 {
		t.Fatalf("parent=%+v err=%v", storedParent, err)
	}
	if err := os.Remove(filepath.Join(f.artifactsDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPost, base+"/retries", "deployment-retry-api", body)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("retry replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	var repeated operator.DeploymentMutationResult
	if err := json.Unmarshal(replay.Body.Bytes(), &repeated); err != nil || !repeated.Replayed ||
		repeated.DeploymentID != child.DeploymentID || len(repeated.Jobs) != 1 ||
		repeated.Jobs[0].JobID != child.Jobs[0].JobID {
		t.Fatalf("retry replay=%+v err=%v", repeated, err)
	}
}

func TestOperatorDeploymentAbandonRouteNeedsFullIDAndPreservesUnopenedTargets(t *testing.T) {
	f, record, _, preview := operatorDeploymentCreateFixture(t, 2)
	created := createOperatorDeploymentViaAPI(t, f, record, preview, "deployment-abandon-parent")
	failedAt := time.Now().UTC().Truncate(time.Second)
	if _, err := f.store.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Failed, failedAt.Format(time.RFC3339Nano), created.Jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.store.SetDeploymentState(created.DeploymentID, store.DeploymentRunning,
		store.DeploymentPaused, failedAt); err != nil || !changed {
		t.Fatalf("pause changed=%t err=%v", changed, err)
	}
	// Abandon is lifecycle-only and must remain available if deployable bytes
	// have already been quarantined.
	if err := os.Remove(filepath.Join(f.artifactsDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	base := "/v1/operator/deployments/" + created.DeploymentID
	previewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/abandonment-preview", "", `{}`)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("abandon preview=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var abandonPreview operator.DeploymentActionPreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &abandonPreview); err != nil ||
		!abandonPreview.Eligibility.Eligible || abandonPreview.Eligibility.AffectedTargets != 1 ||
		len(abandonPreview.Targets) != 1 || abandonPreview.Artifact != nil {
		t.Fatalf("abandon preview=%+v err=%v", abandonPreview, err)
	}
	wrongBody := fmt.Sprintf(`{
 "preview_digest":%q,"expected_control_revision":%d,"expected_opened_batch":%d,
 "confirm_deployment_id":"wrong-deployment","reason":"typed confirmation test"
}`, abandonPreview.PreviewDigest, abandonPreview.Deployment.ControlRevision,
		abandonPreview.Deployment.OpenedBatch)
	wrong := operatorRequest(t, f.mux, http.MethodPost, base+"/abandonments", "deployment-abandon-wrong", wrongBody)
	assertAPIError(t, wrong, http.StatusBadRequest, store.OperatorCodeDeploymentConfirmationMismatch)

	body := strings.Replace(wrongBody, `"wrong-deployment"`, fmt.Sprintf("%q", created.DeploymentID), 1)
	fresh := operatorRequest(t, f.mux, http.MethodPost, base+"/abandonments", "deployment-abandon-api", body)
	if fresh.Code != http.StatusOK || fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("abandon=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var abandoned operator.DeploymentMutationResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &abandoned); err != nil || abandoned.Replayed ||
		abandoned.State != store.DeploymentFinished || abandoned.ControlRevision != 2 ||
		abandoned.OpenedBatch != 1 || len(abandoned.Jobs) != 0 ||
		abandoned.PausedAt == nil || abandoned.FinishedAt == nil {
		t.Fatalf("abandoned=%+v err=%v", abandoned, err)
	}
	targets, err := f.store.DeploymentTargets(created.DeploymentID)
	if err != nil || len(targets) != 2 || targets[1].JobID != "" {
		t.Fatalf("targets=%+v err=%v", targets, err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPost, base+"/abandonments", "deployment-abandon-api", body)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("abandon replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
}

func createOperatorDeploymentViaAPI(t *testing.T, f jobsFixture, record artifactSidecar,
	preview operator.DeploymentCreatePreviewResult, key string,
) operator.DeploymentMutationResult {
	t.Helper()
	body := fmt.Sprintf(`{
 "channel":"canary","version":%q,"artifact_sha256":%q,
 "batch_size":1,"execution_timeout_seconds":600,"irreversible":false,
 "preview_digest":%q,"confirm_channel":"canary","confirm_version":%q,"reason":"fixture create"
}`, record.Version, record.SHA256, preview.PreviewDigest, record.Version)
	rec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/deployments", key, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create fixture=%d body=%s", rec.Code, rec.Body.String())
	}
	var result operator.DeploymentMutationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || result.DeploymentID == "" || len(result.Jobs) != 1 {
		t.Fatalf("create fixture result=%+v err=%v", result, err)
	}
	return result
}

func TestOperatorDeploymentMalformedActionIsAuditedWithoutConsumingKey(t *testing.T) {
	f := newJobsFixture(t, "deployment-transport")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	key := "deployment-transport-key"
	rec := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/deployments/untrusted-path/continuations", key, `{"Preview_Digest":"bad"}`)
	assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	if rec.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("transport rejection looked replayed: %v", rec.Header())
	}
	var ledger int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("transport rejection occupied key rows=%d err=%v", ledger, err)
	}
	entries, err := f.store.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	if entry.Action != store.AuditDeploymentContinue || entry.Subject != "deployment continue" || entry.OK ||
		entry.IdempotencyKey != key || entry.RequestDigest != "" ||
		!strings.Contains(entry.Detail, store.OperatorTransportRejectionPrefix+"BAD_REQUEST") ||
		strings.Contains(entry.Detail, "Preview_Digest") || strings.Contains(entry.Subject, "untrusted-path") {
		t.Fatalf("transport audit=%+v", entry)
	}
}

func operatorDeploymentCreateFixture(t *testing.T, machineCount int) (jobsFixture, artifactSidecar,
	operator.DeploymentCreatePreviewRequest, operator.DeploymentCreatePreviewResult,
) {
	t.Helper()
	f := newJobsFixture(t, "deployment-target-1")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	machines := []enrolled{f.machine}
	for i := 2; i <= machineCount; i++ {
		machines = append(machines, enrollViaHTTP(t, f.mux, f.store, fmt.Sprintf("deployment-target-%d", i)))
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, machine := range machines {
		if err := f.store.RecordCheckin(machine.id, model.Checkin{SentAt: now, AgentStartedAt: now.Add(-time.Hour)}, now); err != nil {
			t.Fatal(err)
		}
		if err := f.store.RecordObservation(machine.id, model.ObservationBatch{
			MeasuredAt: now, OpenClaw: model.OpenClaw{Install: &model.OpenClawInstall{NodeVersion: "24.15.0"}},
		}, now); err != nil {
			t.Fatal(err)
		}
		if err := f.store.SetMachineChannel(machine.id, "canary"); err != nil {
			t.Fatal(err)
		}
	}
	record := writeJobTestArtifact(t, f.artifactsDir, "2026.9.8", "operator-deployment-api")
	record.EnginesNode = ">=24.15.0 <25"
	if err := writeArtifactSidecar(f.artifactsDir, record); err != nil {
		t.Fatal(err)
	}
	request := operator.DeploymentCreatePreviewRequest{
		Channel: "canary", Version: record.Version, ArtifactSHA256: record.SHA256,
		BatchSize: 1, ExecutionTimeoutSeconds: 600,
	}
	previewRec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/deployments/preview", "",
		fmt.Sprintf(`{"channel":"canary","version":%q,"artifact_sha256":%q,"batch_size":1,"execution_timeout_seconds":600,"irreversible":false}`,
			record.Version, record.SHA256))
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview=%d %s", previewRec.Code, previewRec.Body.String())
	}
	var preview operator.DeploymentCreatePreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil || !preview.CreateAllowed || preview.Impact != machineCount {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	return f, record, request, preview
}
