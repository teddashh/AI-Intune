package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func decodeMachineLifecycleReadResult(t *testing.T, rec *httptest.ResponseRecorder) store.OperatorMachineLifecycleReadResult {
	t.Helper()
	var result store.OperatorMachineLifecycleReadResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode lifecycle read JSON: %v; body=%s", err, rec.Body.String())
	}
	return result
}

func decodeMachineLifecyclePreviewResult(t *testing.T, rec *httptest.ResponseRecorder) store.OperatorMachineLifecyclePreviewResult {
	t.Helper()
	var result store.OperatorMachineLifecyclePreviewResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode lifecycle preview JSON: %v; body=%s", err, rec.Body.String())
	}
	return result
}

func decodeMachineLifecycleApplyResult(t *testing.T, rec *httptest.ResponseRecorder) store.OperatorMachineLifecycleResult {
	t.Helper()
	var result store.OperatorMachineLifecycleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode lifecycle apply JSON: %v; body=%s", err, rec.Body.String())
	}
	return result
}

func assertLifecycleResponseOmitsLegacyExpected(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rec.Body.String(), `"expected"`) {
		t.Fatalf("lifecycle response exposed legacy expected: %s", rec.Body.String())
	}
}

func previewMachineLifecycleAPI(t *testing.T, f jobsFixture, desired string, revision int64) store.OperatorMachineLifecyclePreviewResult {
	t.Helper()
	path := "/v1/operator/machines/" + f.machine.id + "/lifecycle-preview"
	rec := operatorRequest(t, f.mux, http.MethodPost, path, "",
		fmt.Sprintf(`{"desired_state":%q,"expected_revision":%d}`, desired, revision))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("lifecycle preview=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	assertLifecycleResponseOmitsLegacyExpected(t, rec)
	if !strings.Contains(rec.Body.String(), `"open_agent_session_count":`) {
		t.Fatalf("lifecycle preview omitted open agent session count: %s", rec.Body.String())
	}
	result := decodeMachineLifecyclePreviewResult(t, rec)
	if result.PreviewDigest == "" || !strings.HasPrefix(result.PreviewDigest, "sha256:") {
		t.Fatalf("lifecycle preview lacks digest: %+v", result)
	}
	return result
}

func lifecycleApplyBody(desired string, revision int64, displayName, previewDigest, reason string) string {
	return fmt.Sprintf(
		`{"desired_state":%q,"expected_revision":%d,"confirm_display_name":%q,"preview_digest":%q,"reason":%q}`,
		desired, revision, displayName, previewDigest, reason)
}

func TestOperatorMachineLifecycleGETPreviewApplyReplayAndRestore(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id + "/lifecycle"

	get := operatorRequest(t, f.mux, http.MethodGet, base, "", "")
	if get.Code != http.StatusOK || get.Header().Get("Cache-Control") != "no-store" ||
		get.Header().Get("ETag") != `"lifecycle-revision-0"` {
		t.Fatalf("initial lifecycle GET=%d headers=%v body=%s", get.Code, get.Header(), get.Body.String())
	}
	assertLifecycleResponseOmitsLegacyExpected(t, get)
	initial := decodeMachineLifecycleReadResult(t, get)
	if initial.MachineID != f.machine.id || initial.DisplayName != "cnode-operator" ||
		initial.State != store.MachineLifecycleActive || initial.LifecycleRevision != 0 || initial.RetiredAt != nil ||
		!initial.RegistryRetained || !initial.HistoryPreserved || !initial.AgentAuthenticationAllowed {
		t.Fatalf("initial lifecycle=%+v", initial)
	}

	preview := previewMachineLifecycleAPI(t, f, "retired", 0)
	if preview.MachineID != f.machine.id || preview.CurrentState != store.MachineLifecycleActive ||
		preview.DesiredState != store.MachineLifecycleRetired || preview.LifecycleRevision != 0 ||
		preview.DenominatorDelta != -1 || !preview.RegistryRetained || !preview.HistoryPreserved ||
		!preview.AgentAuthenticationBefore || preview.AgentAuthenticationAfter {
		t.Fatalf("retire preview=%+v", preview)
	}
	body := lifecycleApplyBody("retired", 0, "cnode-operator", preview.PreviewDigest, "retire API test")
	fresh := operatorRequest(t, f.mux, http.MethodPut, base, "lifecycle-retire-key", body)
	if fresh.Code != http.StatusOK || fresh.Header().Get("Idempotency-Replayed") != "" ||
		fresh.Header().Get("ETag") != `"lifecycle-revision-1"` ||
		fresh.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("fresh lifecycle apply=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	assertLifecycleResponseOmitsLegacyExpected(t, fresh)
	if strings.Contains(fresh.Body.String(), `"open_agent_session_count"`) {
		t.Fatalf("lifecycle apply exposed preview-time open agent session count: %s", fresh.Body.String())
	}
	retired := decodeMachineLifecycleApplyResult(t, fresh)
	if retired.MachineID != f.machine.id || retired.PreviousState != "active" ||
		retired.State != "retired" || retired.LifecycleRevision != 1 ||
		!retired.Changed || retired.NoOp || retired.Replayed || retired.RetiredAt == nil ||
		retired.TransitionEventID == nil || retired.AppliedAt.IsZero() ||
		retired.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("fresh retire result=%+v", retired)
	}

	replay := operatorRequest(t, f.mux, http.MethodPut, base, "lifecycle-retire-key",
		fmt.Sprintf("{\n  \"reason\":\"retire API test\",\"preview_digest\":%q,\"confirm_display_name\":\"cnode-operator\",\"expected_revision\":0,\"desired_state\":\"retired\"\n}", preview.PreviewDigest))
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" ||
		replay.Header().Get("ETag") != `"lifecycle-revision-1"` {
		t.Fatalf("lifecycle replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	assertLifecycleResponseOmitsLegacyExpected(t, replay)
	replayed := decodeMachineLifecycleApplyResult(t, replay)
	if !replayed.Replayed || replayed.LifecycleRevision != retired.LifecycleRevision || replayed.State != "retired" ||
		replayed.TransitionEventID == nil || retired.TransitionEventID == nil ||
		*replayed.TransitionEventID != *retired.TransitionEventID || !replayed.AppliedAt.Equal(retired.AppliedAt) {
		t.Fatalf("lifecycle replay changed immutable receipt: first=%+v replay=%+v", retired, replayed)
	}

	conflict := operatorRequest(t, f.mux, http.MethodPut, base, "lifecycle-retire-key",
		lifecycleApplyBody("active", 1, "cnode-operator", preview.PreviewDigest, "different meaning"))
	assertAPIError(t, conflict, http.StatusConflict, store.OperatorCodeIdempotencyConflict)

	afterRetire := operatorRequest(t, f.mux, http.MethodGet, base, "", "")
	if afterRetire.Code != http.StatusOK || afterRetire.Header().Get("ETag") != `"lifecycle-revision-1"` {
		t.Fatalf("retired lifecycle GET=%d headers=%v body=%s", afterRetire.Code, afterRetire.Header(), afterRetire.Body.String())
	}
	current := decodeMachineLifecycleReadResult(t, afterRetire)
	if current.State != store.MachineLifecycleRetired || current.LifecycleRevision != 1 || current.RetiredAt == nil ||
		current.AgentAuthenticationAllowed || current.InDenominator {
		t.Fatalf("retired lifecycle GET=%+v", current)
	}

	restorePreview := previewMachineLifecycleAPI(t, f, "active", 1)
	if restorePreview.CurrentState != store.MachineLifecycleRetired || restorePreview.DesiredState != store.MachineLifecycleActive ||
		restorePreview.LifecycleRevision != 1 || restorePreview.DenominatorDelta != 1 ||
		restorePreview.AgentAuthenticationBefore || !restorePreview.AgentAuthenticationAfter {
		t.Fatalf("restore preview=%+v", restorePreview)
	}
	restoredRec := operatorRequest(t, f.mux, http.MethodPut, base, "lifecycle-restore-key",
		lifecycleApplyBody("active", 1, "cnode-operator", restorePreview.PreviewDigest, "restore API test"))
	if restoredRec.Code != http.StatusOK || restoredRec.Header().Get("ETag") != `"lifecycle-revision-2"` {
		t.Fatalf("restore apply=%d headers=%v body=%s", restoredRec.Code, restoredRec.Header(), restoredRec.Body.String())
	}
	restored := decodeMachineLifecycleApplyResult(t, restoredRec)
	if restored.PreviousState != "retired" || restored.State != "active" ||
		restored.LifecycleRevision != 2 || !restored.Changed || restored.NoOp ||
		restored.RetiredAt != nil || restored.TransitionEventID == nil || restored.AppliedAt.IsZero() || restored.Replayed {
		t.Fatalf("restore result=%+v", restored)
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 2 {
		t.Fatalf("lifecycle projection after restore=%+v err=%v", machine, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 4 {
		t.Fatalf("lifecycle apply/replay/conflict audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditMachineLifecycle || entry.RequestDigest == "" ||
			entry.WhoNode != "operator" || entry.WhoUser != "ted@example.com" ||
			entry.AuthSubject != "tailscale-user:42" || entry.AuthNodeID != "node-stable-1" ||
			entry.AuthCapability != "example.com/cap/clawctl-admin" ||
			entry.AuthMethod != operatorauth.AuthMethodLocalAPI ||
			entry.AuthDecision != string(operatorauth.Authorized) || entry.SourceKind != "operator-api" {
			t.Fatalf("lifecycle audit lost canonical action/request/auth evidence: %+v", entry)
		}
	}
}

func TestOperatorMachineLifecycleNoOpKeepsRevisionAndTransitionLedger(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id + "/lifecycle"
	preview := previewMachineLifecycleAPI(t, f, "active", 0)
	if preview.CurrentState != store.MachineLifecycleActive ||
		preview.DesiredState != store.MachineLifecycleActive || preview.LifecycleRevision != 0 ||
		preview.DenominatorDelta != 0 || preview.InDenominatorBefore != preview.InDenominatorAfter {
		t.Fatalf("active no-op preview=%+v", preview)
	}

	body := lifecycleApplyBody("active", 0, "cnode-operator", preview.PreviewDigest, "confirm active state")
	fresh := operatorRequest(t, f.mux, http.MethodPut, base, "lifecycle-no-op-key", body)
	if fresh.Code != http.StatusOK || fresh.Header().Get("ETag") != `"lifecycle-revision-0"` ||
		fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("lifecycle no-op=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	result := decodeMachineLifecycleApplyResult(t, fresh)
	if result.State != store.MachineLifecycleActive || result.PreviousState != store.MachineLifecycleActive ||
		result.LifecycleRevision != 0 || result.Changed || !result.NoOp || result.Replayed ||
		result.TransitionEventID != nil || result.RetiredAt != nil || result.AppliedAt.IsZero() {
		t.Fatalf("lifecycle no-op result=%+v", result)
	}
	if strings.Contains(fresh.Body.String(), `"transition_event_id"`) || strings.Contains(fresh.Body.String(), `"retired_at"`) {
		t.Fatalf("no-op response emitted absent transition fields: %s", fresh.Body.String())
	}

	replay := operatorRequest(t, f.mux, http.MethodPut, base, "lifecycle-no-op-key", body)
	if replay.Code != http.StatusOK || replay.Header().Get("ETag") != `"lifecycle-revision-0"` ||
		replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("lifecycle no-op replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	replayed := decodeMachineLifecycleApplyResult(t, replay)
	if !replayed.Replayed || replayed.LifecycleRevision != 0 || replayed.TransitionEventID != nil ||
		!replayed.AppliedAt.Equal(result.AppliedAt) {
		t.Fatalf("lifecycle no-op replay changed receipt: first=%+v replay=%+v", result, replayed)
	}

	var transitions int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, f.machine.id,
	).Scan(&transitions); err != nil {
		t.Fatal(err)
	}
	// Enrollment establishes the initial projection without synthesizing a
	// transition event; a no-op and its replay must keep that ledger empty.
	if transitions != 0 {
		t.Fatalf("no-op lifecycle transition rows=%d, want none", transitions)
	}
}

func TestOperatorMachineLifecycleErrorsAndReplayHeader(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id

	missing := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/machines/no-such/lifecycle", "", "")
	assertAPIError(t, missing, http.StatusNotFound, store.OperatorCodeMachineNotFound)
	missingRevision := operatorRequest(t, f.mux, http.MethodPost,
		base+"/lifecycle-preview", "", `{"desired_state":"retired"}`)
	assertAPIError(t, missingRevision, http.StatusPreconditionRequired, store.OperatorCodePreconditionRequired)
	badState := operatorRequest(t, f.mux, http.MethodPost, base+"/lifecycle-preview", "",
		`{"desired_state":"deleted","expected_revision":0}`)
	assertAPIError(t, badState, http.StatusBadRequest, store.OperatorCodeBadLifecycle)

	preview := previewMachineLifecycleAPI(t, f, "retired", 0)
	stalePreviewRevision := operatorRequest(t, f.mux, http.MethodPost,
		base+"/lifecycle-preview", "", `{"desired_state":"retired","expected_revision":1}`)
	assertAPIError(t, stalePreviewRevision, http.StatusPreconditionFailed, store.OperatorCodePreconditionFailed)
	validBody := lifecycleApplyBody("retired", 0, "cnode-operator", preview.PreviewDigest, "retire API test")
	missingKey := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "", validBody)
	assertAPIError(t, missingKey, http.StatusBadRequest, store.OperatorCodeIdempotencyKeyRequired)
	missingApplyRevision := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "lifecycle-missing-revision",
		fmt.Sprintf(`{"desired_state":"retired","confirm_display_name":"cnode-operator","preview_digest":%q,"reason":"retire API test"}`, preview.PreviewDigest))
	assertAPIError(t, missingApplyRevision, http.StatusPreconditionRequired, store.OperatorCodePreconditionRequired)
	missingReason := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "lifecycle-missing-reason",
		lifecycleApplyBody("retired", 0, "cnode-operator", preview.PreviewDigest, ""))
	assertAPIError(t, missingReason, http.StatusBadRequest, store.OperatorCodeReasonRequired)
	missingPreview := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "lifecycle-missing-preview",
		lifecycleApplyBody("retired", 0, "cnode-operator", "", "retire API test"))
	assertAPIError(t, missingPreview, http.StatusPreconditionRequired, store.OperatorCodePreviewRequired)
	staleRevision := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "lifecycle-stale-revision",
		lifecycleApplyBody("retired", 1, "cnode-operator", preview.PreviewDigest, "retire API test"))
	assertAPIError(t, staleRevision, http.StatusPreconditionFailed, store.OperatorCodePreconditionFailed)
	stalePreview := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "lifecycle-stale-preview",
		lifecycleApplyBody("retired", 0, "cnode-operator",
			"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "retire API test"))
	assertAPIError(t, stalePreview, http.StatusPreconditionFailed, store.OperatorCodePreviewStale)

	body := lifecycleApplyBody("retired", 0, "wrong-name", preview.PreviewDigest, "wrong confirmation")
	first := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "lifecycle-rejected-key", body)
	assertAPIError(t, first, http.StatusBadRequest, store.OperatorCodeConfirmationMismatch)
	if first.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("fresh rejection mislabeled replay: headers=%v", first.Header())
	}
	replay := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "lifecycle-rejected-key", body)
	assertAPIError(t, replay, http.StatusBadRequest, store.OperatorCodeConfirmationMismatch)
	if replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("rejected replay missing header: headers=%v body=%s", replay.Header(), replay.Body.String())
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 0 {
		t.Fatalf("rejections changed lifecycle=%+v err=%v", machine, err)
	}
}

func TestOperatorMachineLifecycleOversizedIdempotencyKeyIsHashedWithoutOccupyingLedger(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id
	preview := previewMachineLifecycleAPI(t, f, "retired", 0)
	oversized := strings.Repeat("long-key-", 40)
	rec := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", oversized,
		lifecycleApplyBody("retired", 0, "cnode-operator", preview.PreviewDigest, "oversized key"))
	assertAPIError(t, rec, http.StatusBadRequest, store.OperatorCodeIdempotencyKeyRequired)

	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("oversized lifecycle key audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	if entry.Action != store.AuditMachineLifecycle || entry.OK || entry.IdempotencyKey == oversized ||
		len(entry.IdempotencyKey) > 200 || !strings.HasPrefix(entry.IdempotencyKey, "invalid-key-sha256:") ||
		entry.RequestDigest == "" || entry.SourceKind != "operator-api" {
		t.Fatalf("oversized lifecycle key audit fields=%+v", entry)
	}
	var ledger int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("oversized lifecycle key ledger=%d err=%v", ledger, err)
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 0 {
		t.Fatalf("oversized lifecycle key changed projection=%+v err=%v", machine, err)
	}
}

func TestOperatorMachineLifecycleActiveJobBlockerReplaysWithoutRetiring(t *testing.T) {
	f := observedOperatorFixture(t)
	f.newJob(t, "sha256:a")
	base := "/v1/operator/machines/" + f.machine.id
	preview := previewMachineLifecycleAPI(t, f, "retired", 0)
	if preview.ActiveJobCount != 1 || len(preview.Blockers) != 1 ||
		preview.Blockers[0] != store.MachineLifecycleBlockerNonterminalJobs {
		t.Fatalf("retire preview did not expose active-job blocker: %+v", preview)
	}
	body := lifecycleApplyBody("retired", 0, "cnode-operator", preview.PreviewDigest, "wait for active job")
	first := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "lifecycle-active-job", body)
	assertAPIError(t, first, http.StatusConflict, store.OperatorCodeMachineActiveJob)
	if first.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("fresh active-job rejection marked replay: %v", first.Header())
	}
	replay := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", "lifecycle-active-job", body)
	assertAPIError(t, replay, http.StatusConflict, store.OperatorCodeMachineActiveJob)
	if replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("active-job rejection replay missing header: %v", replay.Header())
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 0 {
		t.Fatalf("active-job rejection changed lifecycle=%+v err=%v", machine, err)
	}
}

func TestOperatorMachineLifecycleStrictTransportIsAuditedWithoutConsumingKey(t *testing.T) {
	tests := []struct {
		name, pathSuffix, contentType, body string
		wantStatus                          int
		wantCode                            string
		forbidden                           string
	}{
		{name: "query", pathSuffix: "?unknown=1", contentType: "application/json", body: `{}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "unsupported media", contentType: "text/plain", body: `{}`,
			wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "malformed", contentType: "application/json", body: `{`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "case alias", contentType: "application/json",
			body:       `{"DESIRED_STATE":"retired","expected_revision":0,"confirm_display_name":"cnode-operator","preview_digest":"x","reason":"x"}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST", forbidden: "DESIRED_STATE"},
		{name: "duplicate", contentType: "application/json",
			body:       `{"desired_state":"retired","desired_state":"active","expected_revision":0,"confirm_display_name":"cnode-operator","preview_digest":"x","reason":"x"}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "null", contentType: "application/json",
			body:       `{"desired_state":null,"expected_revision":0,"confirm_display_name":"cnode-operator","preview_digest":"x","reason":"x"}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "trailing", contentType: "application/json",
			body:       `{"desired_state":"retired","expected_revision":0,"confirm_display_name":"cnode-operator","preview_digest":"x","reason":"x"}{}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "oversized", contentType: "application/json", body: strings.Repeat(" ", (64<<10)+1),
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "PAYLOAD_TOO_LARGE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			key := "lifecycle-transport-" + strings.ReplaceAll(test.name, " ", "-")
			path := "/v1/operator/machines/" + f.machine.id + "/lifecycle" + test.pathSuffix
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(test.body))
			req.RemoteAddr = "100.64.0.7:41234"
			req.Header.Set("Content-Type", test.contentType)
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("User-Agent", "lifecycle-transport-test/1")
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			f.mux.ServeHTTP(rec, req)
			assertAPIError(t, rec, test.wantStatus, test.wantCode)
			if rec.Header().Get("Cache-Control") != "no-store" ||
				(test.forbidden != "" && strings.Contains(rec.Body.String(), test.forbidden)) {
				t.Fatalf("unsafe transport response headers=%v body=%s", rec.Header(), rec.Body.String())
			}
			machine, err := f.store.GetMachine(f.machine.id)
			if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 0 {
				t.Fatalf("transport rejection changed lifecycle=%+v err=%v", machine, err)
			}
			var ledger int
			if err := f.store.DB().QueryRow(
				`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&ledger); err != nil || ledger != 0 {
				t.Fatalf("transport rejection occupied key rows=%d err=%v", ledger, err)
			}
			entries, err := f.store.Audit(f.machine.id, 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("transport audit=%+v err=%v", entries, err)
			}
			entry := entries[0]
			if entry.Action != store.AuditMachineLifecycle || entry.OK || entry.MachineID != f.machine.id ||
				entry.Subject != "cnode-operator" || entry.IdempotencyKey != key || entry.RequestDigest != "" ||
				!entry.IsOperatorTransportRejection() || entry.SourceAddr != "100.64.0.7" ||
				entry.WhoNode != "operator" || entry.WhoUser != "ted@example.com" ||
				entry.UserAgent != "lifecycle-transport-test/1" || entry.AuthSubject != "tailscale-user:42" ||
				entry.AuthNodeID != "node-stable-1" || entry.AuthCapability != "example.com/cap/clawctl-admin" ||
				entry.AuthMethod != operatorauth.AuthMethodLocalAPI ||
				entry.AuthDecision != string(operatorauth.Authorized) || entry.SourceKind != "operator-api" ||
				(test.forbidden != "" && strings.Contains(entry.Detail, test.forbidden)) {
				t.Fatalf("transport audit fields=%+v", entry)
			}
		})
	}
}

func TestOperatorMachineLifecycleCorrectedTransportCanReuseIdempotencyKey(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id
	preview := previewMachineLifecycleAPI(t, f, "retired", 0)
	key := "lifecycle-corrected-transport"
	bad := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", key, `{`)
	assertAPIError(t, bad, http.StatusBadRequest, "BAD_REQUEST")

	good := operatorRequest(t, f.mux, http.MethodPut, base+"/lifecycle", key,
		lifecycleApplyBody("retired", 0, "cnode-operator", preview.PreviewDigest, "correct transport"))
	if good.Code != http.StatusOK || good.Header().Get("Idempotency-Replayed") != "" ||
		good.Header().Get("ETag") != `"lifecycle-revision-1"` {
		t.Fatalf("corrected lifecycle request=%d headers=%v body=%s", good.Code, good.Header(), good.Body.String())
	}
	result := decodeMachineLifecycleApplyResult(t, good)
	if result.State != store.MachineLifecycleRetired || result.LifecycleRevision != 1 ||
		!result.Changed || result.Replayed {
		t.Fatalf("corrected lifecycle result=%+v", result)
	}
	var ledger int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key,
	).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatalf("corrected lifecycle key rows=%d err=%v", ledger, err)
	}
}

func TestOperatorMachineLifecyclePreviewStrictJSONAndGETQuery(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id
	for _, test := range []struct {
		name, method, path, contentType, body string
		wantStatus                            int
		wantCode                              string
	}{
		{name: "GET query", method: http.MethodGet, path: base + "/lifecycle?x=1",
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "preview query", method: http.MethodPost, path: base + "/lifecycle-preview?x=1",
			contentType: "application/json", body: `{}`, wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "preview media", method: http.MethodPost, path: base + "/lifecycle-preview",
			contentType: "text/plain", body: `{}`, wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "preview unknown", method: http.MethodPost, path: base + "/lifecycle-preview",
			contentType: "application/json", body: `{"desired_state":"retired","expected_revision":0,"attacker":true}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "preview duplicate", method: http.MethodPost, path: base + "/lifecycle-preview",
			contentType: "application/json", body: `{"desired_state":"retired","expected_revision":0,"expected_revision":1}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "preview null", method: http.MethodPost, path: base + "/lifecycle-preview",
			contentType: "application/json", body: `{"desired_state":"retired","expected_revision":null}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.contentType != "" {
				req.Header.Set("Content-Type", test.contentType)
			}
			permission := operatorauth.Admin
			if test.method == http.MethodGet {
				permission = operatorauth.View
			}
			req = verifiedOperatorRequest(req, permission)
			f.mux.ServeHTTP(rec, req)
			assertAPIError(t, rec, test.wantStatus, test.wantCode)
		})
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 0 {
		t.Fatalf("read/preview transport changed lifecycle=%+v err=%v", machine, err)
	}
	var auditRows, ledgerRows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 0 || ledgerRows != 0 {
		t.Fatalf("GET/preview rejection wrote audit=%d idempotency=%d", auditRows, ledgerRows)
	}
}
