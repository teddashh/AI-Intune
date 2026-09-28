package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestATransportRejectionKeepsTheSameNextStepItReturned(t *testing.T) {
	const (
		contentTypeJSON            = "application/json"
		contentTypeText            = "text/plain"
		badRequestCode             = "BAD_REQUEST"
		unsupportedMediaTypeCode   = "UNSUPPORTED_MEDIA_TYPE"
		contentTypeDetail          = "Content-Type 必須是 application/json"
		responseStatusConsequence  = "HTTP 狀態錯誤時，呼叫端會走錯重試或錯誤分支，並用錯 415 或 400 對應的修法。"
		responseCodeConsequence    = "HTTP 錯誤碼錯誤時，呼叫端按 code 判斷會把這次拒絕歸到錯誤類別。"
		responseMessageConsequence = "HTTP 訊息若給錯下一步，operator 會修錯問題，重送仍會被拒絕。"
		auditCountConsequence      = "稽核列不是恰好一列時，operator 無法把這次拒絕唯一地接回當下收到的修正指示。"
		auditDetailConsequence     = "永久稽核若沒留下 HTTP 當下的同一句下一步，operator 事後無法判斷該修哪一種傳輸問題。"
		distinctDetailConsequence  = "六種不同的 JSON 問題在稽核上變成同一句時，operator 看不出要改哪一種。"
	)
	type endpointCase struct {
		name       string
		method     string
		path       func(jobsFixture) string
		knownField string
	}
	endpoints := []endpointCase{
		{
			name: "retention prune", method: http.MethodPost,
			path:       func(jobsFixture) string { return "/v1/operator/maintenance/retention/prunes" },
			knownField: "reason",
		},
		{
			name: "machine rename", method: http.MethodPut,
			path: func(f jobsFixture) string {
				return "/v1/operator/machines/" + f.machine.id + "/display-name"
			},
			knownField: "reason",
		},
		{
			name: "verifier registration", method: http.MethodPost,
			path:       func(jobsFixture) string { return "/v1/operator/verifiers" },
			knownField: "kind",
		},
	}
	type rejectionCase struct {
		name        string
		contentType string
		body        func(string) string
		wantStatus  int
		wantCode    string
		wantDetail  string
		isJSONIssue bool
	}
	rejections := []rejectionCase{
		{
			name: "outer array", contentType: contentTypeJSON,
			body:       func(string) string { return `[]` },
			wantStatus: http.StatusBadRequest, wantCode: badRequestCode,
			wantDetail: "JSON 最外層必須是 object", isJSONIssue: true,
		},
		{
			name: "unknown field", contentType: contentTypeJSON,
			body:       func(string) string { return `{"unexpected":1}` },
			wantStatus: http.StatusBadRequest, wantCode: badRequestCode,
			wantDetail: "JSON 含有未允許或大小寫不正確的 field", isJSONIssue: true,
		},
		{
			name: "duplicate field", contentType: contentTypeJSON,
			body: func(field string) string {
				return fmt.Sprintf(`{%q:"first",%q:"second"}`, field, field)
			},
			wantStatus: http.StatusBadRequest, wantCode: badRequestCode,
			wantDetail: "JSON field 不可重複", isJSONIssue: true,
		},
		{
			name: "null field", contentType: contentTypeJSON,
			body:       func(field string) string { return fmt.Sprintf(`{%q:null}`, field) },
			wantStatus: http.StatusBadRequest, wantCode: badRequestCode,
			wantDetail: "JSON field 不可為 null", isJSONIssue: true,
		},
		{
			name: "unsupported content type", contentType: contentTypeText,
			body:       func(string) string { return `{}` },
			wantStatus: http.StatusUnsupportedMediaType, wantCode: unsupportedMediaTypeCode,
			wantDetail: contentTypeDetail,
		},
	}

	for _, endpoint := range endpoints {
		var jsonAuditDetails []string
		for _, rejection := range rejections {
			t.Run(endpoint.name+"/"+rejection.name, func(t *testing.T) {
				f := observedOperatorFixture(t)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(endpoint.method, endpoint.path(f),
					strings.NewReader(rejection.body(endpoint.knownField)))
				req.Header.Set("Content-Type", rejection.contentType)
				req = verifiedOperatorRequest(req, operatorauth.Admin)
				f.mux.ServeHTTP(rec, req)

				if rec.Code != rejection.wantStatus {
					t.Errorf("HTTP 狀態 got=%d，expected=%d；%s",
						rec.Code, rejection.wantStatus, responseStatusConsequence)
				}
				var response model.APIError
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.Code != rejection.wantCode {
					t.Errorf("HTTP 錯誤碼 got=%q，expected=%q；%s",
						response.Code, rejection.wantCode, responseCodeConsequence)
				}
				if response.Message != rejection.wantDetail {
					t.Errorf("HTTP 下一步 got=%q，expected=%q；%s",
						response.Message, rejection.wantDetail, responseMessageConsequence)
				}

				entries, err := f.store.Audit("", 10)
				if err != nil {
					t.Fatal(err)
				}
				if got := len(entries); got != 1 {
					t.Errorf("稽核列數 got=%d，expected 恰好一列；%s", got, auditCountConsequence)
					return
				}
				if !strings.Contains(entries[0].Detail, rejection.wantDetail) {
					t.Errorf("稽核下一步 got=%q，expected 含有 %q；%s",
						entries[0].Detail, rejection.wantDetail, auditDetailConsequence)
				}
				if rejection.isJSONIssue {
					jsonAuditDetails = append(jsonAuditDetails, entries[0].Detail)
				}
			})
		}
		if got := len(jsonAuditDetails); got != 4 {
			t.Errorf("%s 的 JSON 稽核句數 got=%d，expected 恰好四句；%s",
				endpoint.name, got, distinctDetailConsequence)
			continue
		}
		for left := 0; left < len(jsonAuditDetails); left++ {
			for right := left + 1; right < len(jsonAuditDetails); right++ {
				if jsonAuditDetails[left] == jsonAuditDetails[right] {
					t.Errorf("%s 的第 %d 與第 %d 句 got=%q，expected 兩兩相異；%s",
						endpoint.name, left+1, right+1, jsonAuditDetails[left],
						distinctDetailConsequence)
				}
			}
		}
	}
}

func TestTheTransportRejectionSentenceIsComposedInOneProductionSite(t *testing.T) {
	const consequence = "多於一處代表某一站又開始自己組句子，同一類拒絕會在不同端點上對 operator 講不同份量的話。"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for index, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "store.OperatorTransportRejectionPrefix") {
				sites = append(sites, fmt.Sprintf("%s:%d", name, index+1))
			}
		}
	}
	if got := len(sites); got != 1 {
		t.Errorf("組句位置 got=%d（%s），expected 恰好一處；%s", got, strings.Join(sites, ", "), consequence)
	}
}

func decodeEnrollTokenOperatorResponse(t *testing.T, rec *httptest.ResponseRecorder) enrollTokenOperatorResponse {
	t.Helper()
	var got enrollTokenOperatorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode operator enrollment response: %v; body=%s", err, rec.Body.String())
	}
	return got
}

type operatorMachineChannelResponse struct {
	MachineID       string `json:"machine_id"`
	DisplayName     string `json:"display_name"`
	PreviousChannel string `json:"previous_channel"`
	Channel         string `json:"channel"`
	Revision        int64  `json:"revision"`
	Replayed        bool   `json:"replayed"`
}

func observedOperatorFixture(t *testing.T) jobsFixture {
	t.Helper()
	f := newJobsFixture(t, "cnode-operator")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    jobsTestNow,
		OpenClaw:      model.OpenClaw{Present: true},
	}, jobsTestNow); err != nil {
		t.Fatalf("record observation: %v", err)
	}
	return f
}

func verifiedOperatorRequest(req *http.Request, permission operatorauth.Permission) *http.Request {
	return operatorauth.WithPrincipal(req, operatorauth.Principal{
		SourceAddr: "100.64.0.7", NodeStableID: "node-stable-1", DeviceName: "operator",
		TailnetUserID: "42", TailnetUserLogin: "ted@example.com",
		AuthMethod: operatorauth.AuthMethodLocalAPI, AuthorizedCapability: testOperatorCapability(permission),
	})
}

func testOperatorCapability(permission operatorauth.Permission) string {
	switch permission {
	case operatorauth.View:
		return "example.com/cap/clawctl-view"
	case operatorauth.Operate:
		return "example.com/cap/clawctl-operate"
	default:
		return "example.com/cap/clawctl-admin"
	}
}

func operatorRequest(t *testing.T, mux *http.ServeMux, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "100.64.0.7:41234"
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	permission := operatorauth.Admin
	if method == http.MethodGet {
		permission = operatorauth.View
	}
	req = verifiedOperatorRequest(req, permission)
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeOperatorChannelResponse(t *testing.T, rec *httptest.ResponseRecorder) operatorMachineChannelResponse {
	t.Helper()
	var got operatorMachineChannelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode operator channel response: %v; body=%s", err, rec.Body.String())
	}
	return got
}

func pendingOperatorRevocationFixture(t *testing.T, name string) (jobsFixture, string, string) {
	t.Helper()
	f := observedOperatorFixture(t)
	machineID, token, err := f.store.CreateEnrollTokenFor(name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return f, machineID, token
}

func TestOperatorEnrollmentTokenRevocationGETPreviewCreateAndReplay(t *testing.T) {
	f, machineID, token := pendingOperatorRevocationFixture(t, "api-revoke")
	base := "/v1/operator/machines/" + machineID + "/enrollment-token"
	getResult := operatorRequest(t, f.mux, http.MethodGet, base, "", "")
	if getResult.Code != http.StatusOK || getResult.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET=%d headers=%v body=%s", getResult.Code, getResult.Header(), getResult.Body.String())
	}
	var status store.OperatorPendingEnrollTokenResult
	if err := json.Unmarshal(getResult.Body.Bytes(), &status); err != nil ||
		status.MachineID != machineID || status.DisplayName != "api-revoke" || status.TokenExpired {
		t.Fatalf("status=%+v err=%v", status, err)
	}

	previewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/revocation-preview", "", `{}`)
	if previewRec.Code != http.StatusOK || previewRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview=%d headers=%v body=%s", previewRec.Code, previewRec.Header(), previewRec.Body.String())
	}
	var preview store.OperatorEnrollTokenRevocationPreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil ||
		preview.MachineID != machineID || preview.PreviewDigest == "" ||
		!preview.RegistryRetained || preview.DenominatorDelta != 0 ||
		preview.ActiveAgentCredentialAffected {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	body := fmt.Sprintf(`{"preview_digest":%q,"reason":"exposed"}`, preview.PreviewDigest)
	freshRec := operatorRequest(t, f.mux, http.MethodPost, base+"/revocations", "api-revoke-key", body)
	if freshRec.Code != http.StatusCreated || freshRec.Header().Get("Idempotency-Replayed") != "" ||
		freshRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("fresh=%d headers=%v body=%s", freshRec.Code, freshRec.Header(), freshRec.Body.String())
	}
	var fresh store.OperatorEnrollTokenRevocationResult
	if err := json.Unmarshal(freshRec.Body.Bytes(), &fresh); err != nil || fresh.Replayed ||
		fresh.MachineID != machineID || !fresh.RegistryRetained ||
		fresh.DenominatorDelta != 0 || fresh.ActiveAgentCredentialAffected ||
		fresh.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
	for _, forbidden := range []string{token, "token_hash", hashForOperatorAPITest(token)} {
		if strings.Contains(freshRec.Body.String(), forbidden) {
			t.Fatalf("fresh response contains %q: %s", forbidden, freshRec.Body.String())
		}
	}

	replayRec := operatorRequest(t, f.mux, http.MethodPost, base+"/revocations", "api-revoke-key", body)
	if replayRec.Code != http.StatusOK || replayRec.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replayRec.Code, replayRec.Header(), replayRec.Body.String())
	}
	var replay store.OperatorEnrollTokenRevocationResult
	if err := json.Unmarshal(replayRec.Body.Bytes(), &replay); err != nil || !replay.Replayed ||
		replay.MachineID != fresh.MachineID || !replay.RevokedAt.Equal(fresh.RevokedAt) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	conflict := operatorRequest(t, f.mux, http.MethodPost, base+"/revocations", "api-revoke-key",
		fmt.Sprintf(`{"preview_digest":%q,"reason":"different"}`, preview.PreviewDigest))
	assertAPIError(t, conflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT")
	if _, _, err := f.store.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "api-revoke", OS: "linux", Arch: "amd64",
	}, time.Now().UTC()); !errors.Is(err, store.ErrTokenInvalid) {
		t.Fatalf("revoked token redeem=%v", err)
	}
}

func hashForOperatorAPITest(token string) string {
	// The hash itself is intentionally not reproduced in production response
	// code. Tests only need a deterministic sentinel for a leak assertion.
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}

func TestOperatorEnrollmentTokenRevocationErrorsAndStrictTransport(t *testing.T) {
	f, machineID, _ := pendingOperatorRevocationFixture(t, "api-revoke-errors")
	base := "/v1/operator/machines/" + machineID + "/enrollment-token"
	missingMachine := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/machines/no-such/enrollment-token", "", "")
	assertAPIError(t, missingMachine, http.StatusNotFound, "MACHINE_NOT_FOUND")

	missingKey := operatorRequest(t, f.mux, http.MethodPost, base+"/revocations", "",
		`{"preview_digest":"sha256:any","reason":"x"}`)
	assertAPIError(t, missingKey, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")
	missingPreview := operatorRequest(t, f.mux, http.MethodPost, base+"/revocations", "missing-preview",
		`{"preview_digest":"","reason":"x"}`)
	assertAPIError(t, missingPreview, http.StatusPreconditionRequired, "PREVIEW_REQUIRED")

	for i, body := range []string{
		`{"Preview_Digest":"x","reason":"x"}`,
		`{"preview_digest":"x","preview_digest":"y","reason":"x"}`,
		`{"preview_digest":"x","reason":"x","unknown":true}`,
		`{"preview_digest":null,"reason":"x"}`,
		`{"preview_digest":"x","reason":"x"} {}`,
	} {
		key := fmt.Sprintf("strict-revoke-%d", i)
		rec := operatorRequest(t, f.mux, http.MethodPost, base+"/revocations", key, body)
		assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
		var ledger int
		if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&ledger); err != nil || ledger != 0 {
			t.Fatalf("transport rejection key=%s ledger=%d err=%v", key, ledger, err)
		}
	}

	preview := operatorRequest(t, f.mux, http.MethodPost, base+"/revocation-preview", "", `{"reason":"not allowed"}`)
	assertAPIError(t, preview, http.StatusBadRequest, "BAD_REQUEST")
	var pending int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=? AND used_at IS NULL`, machineID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("rejections changed pending token=%d err=%v", pending, err)
	}
}

func TestOperatorEnrollmentTokenRevocationTransportRejectionsAreAuditedAndRetryable(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
	}{
		{name: "unsupported media", contentType: "text/plain", body: `{}`,
			wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "empty body", contentType: "application/json", body: "",
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "non object", contentType: "application/json", body: `[]`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "oversized JSON", contentType: "application/json", body: strings.Repeat(" ", (64<<10)+1),
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "PAYLOAD_TOO_LARGE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, machineID, _ := pendingOperatorRevocationFixture(t, "api-revoke-transport")
			base := "/v1/operator/machines/" + machineID + "/enrollment-token"
			previewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/revocation-preview", "", `{}`)
			if previewRec.Code != http.StatusOK {
				t.Fatalf("setup preview=%d body=%s", previewRec.Code, previewRec.Body.String())
			}
			var preview store.OperatorEnrollTokenRevocationPreviewResult
			if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}

			key := "transport-revoke-" + strings.ReplaceAll(tc.name, " ", "-")
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, base+"/revocations", strings.NewReader(tc.body))
			req.RemoteAddr = "100.64.0.7:41234"
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("User-Agent", "operator-revoke-transport-test/1")
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			f.mux.ServeHTTP(rec, req)
			assertAPIError(t, rec, tc.wantStatus, tc.wantCode)
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("transport rejection can be cached: headers=%v", rec.Header())
			}
			var pending, ledger int
			if err := f.store.DB().QueryRow(
				`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=? AND used_at IS NULL`, machineID,
			).Scan(&pending); err != nil || pending != 1 {
				t.Fatalf("transport rejection changed pending ticket count=%d err=%v", pending, err)
			}
			if err := f.store.DB().QueryRow(
				`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key,
			).Scan(&ledger); err != nil || ledger != 0 {
				t.Fatalf("transport rejection occupied idempotency key: count=%d err=%v", ledger, err)
			}
			entries, err := f.store.Audit(machineID, 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("transport rejection audit=%+v err=%v", entries, err)
			}
			entry := entries[0]
			if entry.Action != store.AuditRevokeToken || entry.OK || entry.MachineID != machineID ||
				entry.Subject != "api-revoke-transport" || entry.IdempotencyKey != key ||
				entry.RequestDigest != "" || entry.SourceAddr != "100.64.0.7" ||
				entry.WhoNode != "operator" || entry.WhoUser != "ted@example.com" ||
				entry.UserAgent != "operator-revoke-transport-test/1" ||
				entry.AuthSubject != "tailscale-user:42" || entry.AuthNodeID != "node-stable-1" ||
				entry.AuthCapability != "example.com/cap/clawctl-admin" ||
				entry.AuthMethod != operatorauth.AuthMethodLocalAPI ||
				entry.AuthDecision != string(operatorauth.Authorized) ||
				entry.SourceKind != "operator-api" || !entry.IsOperatorTransportRejection() ||
				!strings.Contains(entry.Detail, tc.wantCode) ||
				!strings.Contains(entry.Detail, "canonical request digest 無法取得") {
				t.Fatalf("transport rejection audit fields=%+v", entry)
			}

			goodBody := fmt.Sprintf(`{"preview_digest":%q,"reason":"corrected transport"}`,
				preview.PreviewDigest)
			good := operatorRequest(t, f.mux, http.MethodPost, base+"/revocations", key, goodBody)
			if good.Code != http.StatusCreated {
				t.Fatalf("corrected request could not reuse transport-rejected key: %d %s",
					good.Code, good.Body.String())
			}
			var result store.OperatorEnrollTokenRevocationResult
			if err := json.Unmarshal(good.Body.Bytes(), &result); err != nil || result.Replayed ||
				result.MachineID != machineID || !result.RegistryRetained {
				t.Fatalf("corrected result=%+v err=%v", result, err)
			}
			if err := f.store.DB().QueryRow(
				`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key,
			).Scan(&ledger); err != nil || ledger != 1 {
				t.Fatalf("corrected request ledger count=%d err=%v", ledger, err)
			}
		})
	}
}

func TestOperatorEnrollmentTokenRevocationOversizedIdempotencyKeyIsBoundedInAudit(t *testing.T) {
	f, machineID, _ := pendingOperatorRevocationFixture(t, "api-revoke-oversized-key")
	base := "/v1/operator/machines/" + machineID + "/enrollment-token"
	previewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/revocation-preview", "", `{}`)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("setup preview=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var preview store.OperatorEnrollTokenRevocationPreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"preview_digest":%q,"reason":"oversized idempotency key"}`,
		preview.PreviewDigest)
	oversized := strings.Repeat("long-key-", 40)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, base+"/revocations", strings.NewReader(body))
	req.RemoteAddr = "100.64.0.7:41234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", oversized)
	req.Header.Set("User-Agent", "operator-revoke-key-test/1")
	req = verifiedOperatorRequest(req, operatorauth.Admin)
	f.mux.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusBadRequest, store.OperatorCodeIdempotencyKeyRequired)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("oversized-key rejection can be cached: headers=%v", rec.Header())
	}

	entries, err := f.store.Audit(machineID, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("oversized-key audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	if entry.Action != store.AuditRevokeToken || entry.OK || entry.MachineID != machineID ||
		entry.IdempotencyKey == oversized || len(entry.IdempotencyKey) > 200 ||
		!strings.HasPrefix(entry.IdempotencyKey, "invalid-key-sha256:") ||
		entry.RequestDigest == "" || entry.IsOperatorTransportRejection() ||
		entry.SourceAddr != "100.64.0.7" || entry.WhoNode != "operator" ||
		entry.WhoUser != "ted@example.com" || entry.UserAgent != "operator-revoke-key-test/1" ||
		entry.AuthSubject != "tailscale-user:42" || entry.AuthNodeID != "node-stable-1" ||
		entry.AuthCapability != "example.com/cap/clawctl-admin" ||
		entry.AuthMethod != operatorauth.AuthMethodLocalAPI ||
		entry.AuthDecision != string(operatorauth.Authorized) || entry.SourceKind != "operator-api" {
		t.Fatalf("oversized-key audit fields=%+v", entry)
	}
	var pending, ledger int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=? AND used_at IS NULL`, machineID,
	).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("oversized key changed pending ticket count=%d err=%v", pending, err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("oversized key wrote idempotency ledger: count=%d err=%v", ledger, err)
	}
}

func TestOperatorMachineChannelRequiresIdempotencyAndPrecondition(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	body := `{"channel":"canary","expected_revision":0,"confirm_display_name":"cnode-operator"}`

	missingKey := operatorRequest(t, f.mux, http.MethodPut, path, "", body)
	assertAPIError(t, missingKey, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")

	missingRevision := operatorRequest(t, f.mux, http.MethodPut, path, "missing-revision",
		`{"channel":"canary","confirm_display_name":"cnode-operator"}`)
	assertAPIError(t, missingRevision, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED")

	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("rejected requests changed machine: machine=%+v err=%v", m, err)
	}
}

func TestOperatorMachineChannelIdempotencyDigestAndAudit(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	body := `{"channel":"canary","expected_revision":0,"confirm_display_name":"cnode-operator"}`

	first := operatorRequest(t, f.mux, http.MethodPut, path, "channel-request-1", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first channel mutation=%d: %s", first.Code, first.Body.String())
	}
	got := decodeOperatorChannelResponse(t, first)
	if got.MachineID != f.machine.id || got.PreviousChannel != "" || got.Channel != "canary" ||
		got.Revision != 1 || got.Replayed {
		t.Fatalf("first response=%+v", got)
	}

	// Canonical-body identity: key order and insignificant whitespace are not a
	// different request and must replay rather than conflict.
	replay := operatorRequest(t, f.mux, http.MethodPut, path, "channel-request-1",
		"{ \n  \"confirm_display_name\": \"cnode-operator\", \"expected_revision\": 0, \"channel\": \"canary\"\n}")
	if replay.Code != http.StatusOK {
		t.Fatalf("idempotent replay=%d: %s", replay.Code, replay.Body.String())
	}
	replayed := decodeOperatorChannelResponse(t, replay)
	if !replayed.Replayed || replayed.Revision != 1 || replayed.PreviousChannel != "" || replayed.Channel != "canary" {
		t.Fatalf("replay response=%+v", replayed)
	}

	conflict := operatorRequest(t, f.mux, http.MethodPut, path, "channel-request-1",
		`{"channel":"stable","expected_revision":1,"confirm_display_name":"cnode-operator"}`)
	assertAPIError(t, conflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT")

	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.Channel != "canary" || m.ChannelRevision != 1 {
		t.Fatalf("replay/conflict changed machine again: machine=%+v err=%v", m, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 3 {
		t.Fatalf("operator requests must all be audited: entries=%+v err=%v", entries, err)
	}
	if entries[0].OK || !strings.Contains(entries[0].Detail, "idempotency") {
		t.Fatalf("digest conflict audit=%+v", entries[0])
	}
	if !entries[1].OK || !strings.Contains(entries[1].Detail, "replay") {
		t.Fatalf("replay audit=%+v", entries[1])
	}
	if !entries[2].OK || entries[2].Reason != "未指派 → canary" ||
		entries[2].WhoNode != "operator" || entries[2].WhoUser != "ted@example.com" {
		t.Fatalf("first mutation audit=%+v", entries[2])
	}
	if entries[2].IdempotencyKey != "channel-request-1" || entries[2].RequestDigest == "" {
		t.Fatalf("first mutation audit is not correlatable: %+v", entries[2])
	}
	for _, entry := range entries {
		if entry.AuthSubject != "tailscale-user:42" || entry.AuthNodeID != "node-stable-1" ||
			entry.AuthCapability != "example.com/cap/clawctl-admin" ||
			entry.AuthMethod != operatorauth.AuthMethodLocalAPI ||
			entry.AuthDecision != string(operatorauth.Authorized) ||
			entry.SourceKind != "operator-api" {
			t.Fatalf("operator audit lost verified authority: %+v", entry)
		}
	}
	if entries[1].IdempotencyKey != entries[2].IdempotencyKey ||
		entries[1].RequestDigest != entries[2].RequestDigest {
		t.Fatalf("replay audit lost original request identity: first=%+v replay=%+v", entries[2], entries[1])
	}
	if entries[0].IdempotencyKey != entries[2].IdempotencyKey ||
		entries[0].RequestDigest == "" || entries[0].RequestDigest == entries[2].RequestDigest {
		t.Fatalf("conflict audit did not retain conflicting request identity: first=%+v conflict=%+v", entries[2], entries[0])
	}
	var joined int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*)
	 FROM audit_log a JOIN operator_idempotency i
	   ON i.idempotency_key=a.idempotency_key AND i.request_digest=a.request_digest
	 WHERE a.audit_id=?`, entries[2].ID).Scan(&joined); err != nil || joined != 1 {
		t.Fatalf("first mutation audit cannot directly join idempotency ledger: joined=%d err=%v", joined, err)
	}
}

func TestOperatorMachineChannelRequiresJSONContentType(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(
		`{"channel":"canary","expected_revision":0,"confirm_display_name":"cnode-operator"}`))
	req.Header.Set("Idempotency-Key", "wrong-media-type")
	req.Header.Set("Content-Type", "text/plain")
	req = verifiedOperatorRequest(req, operatorauth.Admin)
	f.mux.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE")
	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("wrong media type changed machine=%+v err=%v", m, err)
	}
}

func TestOperatorMachineChannelTransportRejectionsAreAuditedOnce(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
	}{
		{name: "unsupported content type", contentType: "text/plain", body: `{}`,
			wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "malformed JSON", contentType: "application/json", body: `{`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "JSON null", contentType: "application/json", body: `null`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "unknown field", contentType: "application/json",
			body:       `{"channel":"canary","expected_revision":0,"confirm_display_name":"cnode-operator","surprise":true}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "trailing JSON", contentType: "application/json",
			body:       `{"channel":"canary","expected_revision":0,"confirm_display_name":"cnode-operator"}{}`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST"},
		{name: "oversized JSON", contentType: "application/json", body: strings.Repeat(" ", (64<<10)+1),
			wantStatus: http.StatusRequestEntityTooLarge, wantCode: "PAYLOAD_TOO_LARGE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			key := "transport-" + strings.ReplaceAll(tc.name, " ", "-")
			path := "/v1/operator/machines/" + f.machine.id + "/channel"
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(tc.body))
			req.RemoteAddr = "100.64.0.7:41234"
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("User-Agent", "operator-audit-test/1")
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			f.mux.ServeHTTP(rec, req)
			assertAPIError(t, rec, tc.wantStatus, tc.wantCode)

			machine, err := f.store.GetMachine(f.machine.id)
			if err != nil || machine.Channel != "" || machine.ChannelRevision != 0 {
				t.Fatalf("transport rejection mutated machine=%+v err=%v", machine, err)
			}
			entries, err := f.store.Audit(f.machine.id, 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("transport rejection audit=%+v err=%v", entries, err)
			}
			entry := entries[0]
			if entry.Action != store.AuditMachineChannel || entry.OK || entry.MachineID != f.machine.id ||
				entry.IdempotencyKey != key || entry.RequestDigest != "" || entry.SourceAddr != "100.64.0.7" ||
				entry.WhoNode != "operator" || entry.WhoUser != "ted@example.com" ||
				entry.UserAgent != "operator-audit-test/1" || !strings.Contains(entry.Detail, tc.wantCode) ||
				entry.AuthSubject != "tailscale-user:42" || entry.AuthNodeID != "node-stable-1" ||
				entry.AuthCapability != "example.com/cap/clawctl-admin" ||
				entry.AuthDecision != string(operatorauth.Authorized) || entry.SourceKind != "operator-api" ||
				!strings.Contains(entry.Detail, "canonical request digest 無法取得") {
				t.Fatalf("transport rejection audit fields=%+v", entry)
			}
			var ledger int
			if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&ledger); err != nil || ledger != 0 {
				t.Fatalf("transport rejection occupied idempotency key: count=%d err=%v", ledger, err)
			}
		})
	}
}

func TestOperatorTransportRejectionDoesNotConsumeCorrectedRequestKey(t *testing.T) {
	for name, badBody := range map[string]string{"malformed": `{`, "null": `null`} {
		t.Run(name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			path := "/v1/operator/machines/" + f.machine.id + "/channel"
			key := "correctable-transport-key-" + name
			bad := operatorRequest(t, f.mux, http.MethodPut, path, key, badBody)
			assertAPIError(t, bad, http.StatusBadRequest, "BAD_REQUEST")
			good := operatorRequest(t, f.mux, http.MethodPut, path, key,
				`{"channel":"canary","expected_revision":0,"confirm_display_name":"cnode-operator"}`)
			if good.Code != http.StatusOK {
				t.Fatalf("corrected request could not reuse transport-rejected key: %d %s", good.Code, good.Body.String())
			}
			result := decodeOperatorChannelResponse(t, good)
			if result.Channel != "canary" || result.Revision != 1 || result.Replayed {
				t.Fatalf("corrected request=%+v", result)
			}
		})
	}
}

func TestOperatorMachineChannelMissingIdempotencyIsAuditedOnce(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(
		`{"channel":"canary","expected_revision":0,"confirm_display_name":"cnode-operator"}`))
	req.RemoteAddr = "100.64.0.7:41234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "operator-audit-test/1")
	req = verifiedOperatorRequest(req, operatorauth.Admin)
	f.mux.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")

	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 1 || entries[0].OK || entries[0].IdempotencyKey != "" ||
		entries[0].RequestDigest == "" || entries[0].WhoNode != "operator" ||
		entries[0].UserAgent != "operator-audit-test/1" {
		t.Fatalf("missing-key audit=%+v err=%v", entries, err)
	}
	var ledger int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("missing key wrote idempotency ledger: count=%d err=%v", ledger, err)
	}
}

func TestOperatorMachineChannelOversizedIdempotencyKeyIsHashedInAudit(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	oversized := strings.Repeat("long-key-", 40)
	rec := operatorRequest(t, f.mux, http.MethodPut, path, oversized,
		`{"channel":"canary","expected_revision":0,"confirm_display_name":"cnode-operator"}`)
	assertAPIError(t, rec, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 1 || entries[0].IdempotencyKey == oversized ||
		len(entries[0].IdempotencyKey) > 200 ||
		!strings.HasPrefix(entries[0].IdempotencyKey, "invalid-key-sha256:") ||
		entries[0].RequestDigest == "" {
		t.Fatalf("oversized-key audit=%+v err=%v", entries, err)
	}
	var ledger int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("oversized key wrote idempotency ledger: count=%d err=%v", ledger, err)
	}
}

func TestOperatorTransportRejectionWithoutBoundaryPrincipalIsMarkedUnverified(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{`))
	req.RemoteAddr = "100.64.0.8:41234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "whois-failed-transport")
	f.mux.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 1 || entries[0].SourceAddr != "100.64.0.8:41234" ||
		entries[0].WhoUnavailable == "" || !strings.Contains(entries[0].WhoUnavailable, "principal") ||
		entries[0].AuthDecision != string(operatorauth.AuthConfigurationInvalid) {
		t.Fatalf("unverified component request audit=%+v err=%v", entries, err)
	}
}

func TestOperatorEnrollmentTokenFreshAndReplayAreDifferentWireShapes(t *testing.T) {
	f := observedOperatorFixture(t)
	preview := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/enrollment-tokens/preview", "",
		`{"display_name":"new-linux-host","ttl_seconds":3600}`)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	if preview.Header().Get("Cache-Control") != "no-store" ||
		!strings.HasPrefix(preview.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("preview security headers=%v", preview.Header())
	}
	var impact store.OperatorEnrollTokenPreviewResult
	if err := json.Unmarshal(preview.Body.Bytes(), &impact); err != nil {
		t.Fatal(err)
	}
	if impact.DisplayName != "new-linux-host" || impact.TTLSeconds != 3600 ||
		impact.ExpiresAtIfCreatedNow.Sub(impact.PreviewedAt) != time.Hour ||
		!impact.CreatesExpectedMachine || !impact.RevocationKeepsRegistryRow ||
		impact.InitialState != store.OperatorEnrollTokenInitialStateNeverReported ||
		impact.SecretDelivery != store.OperatorEnrollTokenSecretDeliveryFirstOnly ||
		!strings.HasPrefix(impact.PreviewDigest, "sha256:") {
		t.Fatalf("preview impact=%+v", impact)
	}

	var machinesBefore, tokensBefore int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machinesBefore); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM enrollment_tokens`).Scan(&tokensBefore); err != nil {
		t.Fatal(err)
	}
	createBody := fmt.Sprintf(
		`{"display_name":"new-linux-host","ttl_seconds":3600,"preview_digest":%q,"reason":"new lab node"}`,
		impact.PreviewDigest)
	const key = "operator-enroll-fresh-replay"
	firstRec := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/enrollment-tokens", key, createBody)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("fresh create status=%d body=%s", firstRec.Code, firstRec.Body.String())
	}
	if firstRec.Header().Get("Cache-Control") != "no-store" ||
		firstRec.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("fresh create headers=%v", firstRec.Header())
	}
	first := decodeEnrollTokenOperatorResponse(t, firstRec)
	secret, err := base64.RawURLEncoding.DecodeString(first.EnrollmentToken)
	if err != nil || len(secret) != 32 || first.MachineID == "" ||
		first.DisplayName != impact.DisplayName || first.TTLSeconds != impact.TTLSeconds ||
		first.PreviewDigest != impact.PreviewDigest || first.ExpiresAt.Sub(first.CreatedAt) != time.Hour ||
		!first.SecretAvailable || first.Replayed || first.RecoveryRequired || first.RecoveryAction != "" {
		t.Fatalf("fresh create response=%+v tokenErr=%v decodedBytes=%d", first, err, len(secret))
	}
	if !strings.Contains(firstRec.Body.String(), `"recovery_action":""`) {
		t.Fatalf("fresh response omitted explicit recovery_action: %s", firstRec.Body.String())
	}

	replayRec := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/enrollment-tokens", key, createBody)
	if replayRec.Code != http.StatusOK || replayRec.Header().Get("Idempotency-Replayed") != "true" ||
		replayRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("replay status=%d headers=%v body=%s", replayRec.Code, replayRec.Header(), replayRec.Body.String())
	}
	replay := decodeEnrollTokenOperatorResponse(t, replayRec)
	if replay.MachineID != first.MachineID || replay.DisplayName != first.DisplayName ||
		!replay.CreatedAt.Equal(first.CreatedAt) || !replay.ExpiresAt.Equal(first.ExpiresAt) ||
		replay.TTLSeconds != first.TTLSeconds || replay.PreviewDigest != first.PreviewDigest ||
		replay.EnrollmentToken != "" || replay.SecretAvailable || !replay.Replayed ||
		!replay.RecoveryRequired || replay.RecoveryAction != store.OperatorEnrollTokenRecoveryRevokeAndReissue {
		t.Fatalf("redacted replay=%+v first=%+v", replay, first)
	}
	if strings.Contains(replayRec.Body.String(), "enrollment_token") ||
		strings.Contains(replayRec.Body.String(), first.EnrollmentToken) {
		t.Fatalf("replay exposed enrollment secret or field: %s", replayRec.Body.String())
	}

	var machinesAfter, tokensAfter, ledgerRows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machinesAfter); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM enrollment_tokens`).Scan(&tokensAfter); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if machinesAfter != machinesBefore+1 || tokensAfter != tokensBefore+1 || ledgerRows != 1 {
		t.Fatalf("replay mutated state again machines=%d→%d tokens=%d→%d ledger=%d",
			machinesBefore, machinesAfter, tokensBefore, tokensAfter, ledgerRows)
	}
	var receiptJSON string
	if err := f.store.DB().QueryRow(
		`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&receiptJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receiptJSON, "enrollment_token") || strings.Contains(receiptJSON, first.EnrollmentToken) ||
		strings.Contains(receiptJSON, "secret_available") || strings.Contains(receiptJSON, "recovery_required") {
		t.Fatalf("durable receipt contains delivery state or secret: %s", receiptJSON)
	}
	entries, err := f.store.Audit(first.MachineID, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("fresh/replay audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditEnrollToken || !entry.OK || entry.Subject != first.DisplayName ||
			entry.Reason != "new lab node" || entry.IdempotencyKey != key || entry.RequestDigest == "" ||
			entry.AuthCapability != "example.com/cap/clawctl-admin" || entry.SourceKind != "operator-api" {
			t.Fatalf("enrollment audit lost canonical authority/correlation: %+v", entry)
		}
	}
	if !entries[0].IsOperatorReplay() || entries[1].IsOperatorReplay() {
		t.Fatalf("fresh/replay audit classification=%+v", entries)
	}
}

func TestOperatorEnrollmentTokenConcurrentSameKeyDeliversExactlyOneSecret(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	previewRec := operatorRequest(t, mux, http.MethodPost,
		"/v1/operator/enrollment-tokens/preview", "",
		`{"display_name":"concurrent-host","ttl_seconds":3600}`)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var preview store.OperatorEnrollTokenPreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(
		`{"display_name":"concurrent-host","ttl_seconds":3600,"preview_digest":%q,"reason":"double submit"}`,
		preview.PreviewDigest)
	const key = "concurrent-enroll-same-key"

	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost,
				"/v1/operator/enrollment-tokens", strings.NewReader(body))
			req.RemoteAddr = "100.64.0.7:41234"
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", key)
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			mux.ServeHTTP(rec, req)
			responses <- rec
		}()
	}
	close(start)
	wg.Wait()
	close(responses)

	freshCount, replayCount := 0, 0
	var fresh, replay enrollTokenOperatorResponse
	var freshSecret string
	for rec := range responses {
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("concurrent response can be cached: status=%d headers=%v", rec.Code, rec.Header())
		}
		got := decodeEnrollTokenOperatorResponse(t, rec)
		switch rec.Code {
		case http.StatusCreated:
			freshCount++
			fresh = got
			freshSecret = got.EnrollmentToken
			secret, err := base64.RawURLEncoding.DecodeString(got.EnrollmentToken)
			if err != nil || len(secret) != 32 || !got.SecretAvailable || got.Replayed ||
				got.RecoveryRequired || got.RecoveryAction != "" ||
				rec.Header().Get("Idempotency-Replayed") != "" {
				t.Fatalf("concurrent fresh response=%+v headers=%v tokenErr=%v", got, rec.Header(), err)
			}
		case http.StatusOK:
			replayCount++
			replay = got
			if got.EnrollmentToken != "" || got.SecretAvailable || !got.Replayed ||
				!got.RecoveryRequired || got.RecoveryAction != store.OperatorEnrollTokenRecoveryRevokeAndReissue ||
				rec.Header().Get("Idempotency-Replayed") != "true" ||
				strings.Contains(rec.Body.String(), "enrollment_token") {
				t.Fatalf("concurrent replay response=%+v headers=%v body=%s", got, rec.Header(), rec.Body.String())
			}
		default:
			t.Fatalf("concurrent create status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if freshCount != 1 || replayCount != 1 {
		t.Fatalf("concurrent delivery fresh=%d replay=%d", freshCount, replayCount)
	}
	if fresh.MachineID == "" || replay.MachineID != fresh.MachineID ||
		replay.DisplayName != fresh.DisplayName || replay.TTLSeconds != fresh.TTLSeconds ||
		replay.PreviewDigest != fresh.PreviewDigest || !replay.CreatedAt.Equal(fresh.CreatedAt) ||
		!replay.ExpiresAt.Equal(fresh.ExpiresAt) || freshSecret == "" {
		t.Fatalf("concurrent receipts disagree fresh=%+v replay=%+v", fresh, replay)
	}

	for table, want := range map[string]int{
		"machine_registry":     1,
		"enrollment_tokens":    1,
		"operator_idempotency": 1,
	} {
		var rows int
		if err := st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&rows); err != nil || rows != want {
			t.Fatalf("concurrent state %s rows=%d want=%d err=%v", table, rows, want, err)
		}
	}
	var receiptJSON string
	if err := st.DB().QueryRow(
		`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&receiptJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receiptJSON, "enrollment_token") || strings.Contains(receiptJSON, freshSecret) {
		t.Fatalf("concurrent durable receipt persisted secret: %s", receiptJSON)
	}
	entries, err := st.Audit(fresh.MachineID, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("concurrent audit=%+v err=%v", entries, err)
	}
	replayAudits, freshAudits := 0, 0
	for _, entry := range entries {
		if entry.Action != store.AuditEnrollToken || !entry.OK || entry.IdempotencyKey != key ||
			entry.RequestDigest == "" || entry.Reason != "double submit" ||
			entry.SourceKind != "operator-api" {
			t.Fatalf("concurrent audit contract=%+v", entry)
		}
		if entry.IsOperatorReplay() {
			replayAudits++
		} else {
			freshAudits++
		}
	}
	if replayAudits != 1 || freshAudits != 1 {
		t.Fatalf("concurrent audit classifications fresh=%d replay=%d entries=%+v",
			freshAudits, replayAudits, entries)
	}
}

func TestOperatorEnrollmentTokenTransportRejectionsDoNotConsumeKey(t *testing.T) {
	cases := []struct {
		name, contentType, body string
		status                  int
	}{
		{name: "unsupported media", contentType: "text/plain", body: `{}`, status: http.StatusUnsupportedMediaType},
		{name: "malformed", contentType: "application/json", body: `{`, status: http.StatusBadRequest},
		{name: "unknown field", contentType: "application/json", body: `{"display_name":"host","ttl_seconds":3600,"preview_digest":"x","reason":"r","extra":true}`, status: http.StatusBadRequest},
		{name: "duplicate known field", contentType: "application/json", body: `{"display_name":"host","display_name":"other","ttl_seconds":3600,"preview_digest":"x","reason":"r"}`, status: http.StatusBadRequest},
		{name: "trailing JSON", contentType: "application/json", body: `{"display_name":"host","ttl_seconds":3600,"preview_digest":"x","reason":"r"}{}`, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			preview := operatorRequest(t, f.mux, http.MethodPost,
				"/v1/operator/enrollment-tokens/preview", "",
				`{"display_name":"correcexample-userost","ttl_seconds":3600}`)
			if preview.Code != http.StatusOK {
				t.Fatalf("setup preview=%d %s", preview.Code, preview.Body.String())
			}
			var impact store.OperatorEnrollTokenPreviewResult
			if err := json.Unmarshal(preview.Body.Bytes(), &impact); err != nil {
				t.Fatal(err)
			}

			key := "transport-enroll-" + strings.ReplaceAll(tc.name, " ", "-")
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/operator/enrollment-tokens", strings.NewReader(tc.body))
			req.RemoteAddr = "100.64.0.7:41234"
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Idempotency-Key", key)
			req.Header.Set("User-Agent", "operator-enroll-test/1")
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			f.mux.ServeHTTP(rec, req)
			wantCode := "BAD_REQUEST"
			if tc.status == http.StatusUnsupportedMediaType {
				wantCode = "UNSUPPORTED_MEDIA_TYPE"
			}
			assertAPIError(t, rec, tc.status, wantCode)
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("transport rejection can be cached: headers=%v", rec.Header())
			}
			var ledger int
			if err := f.store.DB().QueryRow(
				`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&ledger); err != nil || ledger != 0 {
				t.Fatalf("transport rejection occupied key rows=%d err=%v", ledger, err)
			}
			entries, err := f.store.Audit("", 10)
			if err != nil || len(entries) != 1 || entries[0].Action != store.AuditEnrollToken ||
				entries[0].OK || !entries[0].IsOperatorTransportRejection() ||
				entries[0].IdempotencyKey != key || entries[0].RequestDigest != "" ||
				entries[0].AuthCapability != "example.com/cap/clawctl-admin" ||
				entries[0].SourceKind != "operator-api" {
				t.Fatalf("transport audit=%+v err=%v", entries, err)
			}

			corrected := fmt.Sprintf(
				`{"display_name":"correcexample-userost","ttl_seconds":3600,"preview_digest":%q,"reason":"corrected transport"}`,
				impact.PreviewDigest)
			good := operatorRequest(t, f.mux, http.MethodPost,
				"/v1/operator/enrollment-tokens", key, corrected)
			if good.Code != http.StatusCreated {
				t.Fatalf("corrected request could not reuse key: %d %s", good.Code, good.Body.String())
			}
		})
	}
}

func TestOperatorEnrollmentTokenHTTPErrorMappingAndNoStore(t *testing.T) {
	f := observedOperatorFixture(t)
	badPreview := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/enrollment-tokens/preview", "",
		`{"display_name":" padded ","ttl_seconds":59}`)
	assertAPIError(t, badPreview, http.StatusBadRequest, store.OperatorCodeBadDisplayName)
	if badPreview.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("preview rejection missing no-store")
	}

	missingPreview := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/enrollment-tokens", "missing-preview",
		`{"display_name":"host","ttl_seconds":3600,"preview_digest":"","reason":"test"}`)
	assertAPIError(t, missingPreview, http.StatusPreconditionRequired, store.OperatorCodePreviewRequired)
	if missingPreview.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("create rejection missing no-store")
	}
	replayed := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/enrollment-tokens", "missing-preview",
		`{"reason":"test","preview_digest":"","ttl_seconds":3600,"display_name":"host"}`)
	assertAPIError(t, replayed, http.StatusPreconditionRequired, store.OperatorCodePreviewRequired)
	if replayed.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replayed rejection missing evidence: headers=%v", replayed.Header())
	}

	stale := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/enrollment-tokens", "stale-preview",
		`{"display_name":"host","ttl_seconds":3600,"preview_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"test"}`)
	assertAPIError(t, stale, http.StatusPreconditionFailed, store.OperatorCodePreviewStale)

	missingKey := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/enrollment-tokens", "",
		`{"display_name":"host","ttl_seconds":3600,"preview_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"test"}`)
	assertAPIError(t, missingKey, http.StatusBadRequest, store.OperatorCodeIdempotencyKeyRequired)
}

func TestOperatorEnrollmentPreviewRejectsDuplicateKnownJSONField(t *testing.T) {
	f := observedOperatorFixture(t)
	rec := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/enrollment-tokens/preview", "",
		`{"display_name":"host","display_name":"other","ttl_seconds":3600}`)
	assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("duplicate preview response can be cached: headers=%v", rec.Header())
	}
	var machines, ledger int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machines); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if machines != 1 || ledger != 0 {
		t.Fatalf("duplicate preview changed state machines=%d ledger=%d", machines, ledger)
	}
}

func TestOperatorJSONRequestsRejectCaseInsensitiveFieldAliasesBeforeDomain(t *testing.T) {
	tests := []struct {
		name, method, path, key, body string
		wantAudit                     bool
		forbiddenEvidence             []string
	}{
		{
			name: "machine single uppercase alias", method: http.MethodPut,
			path: "/v1/operator/machines/{machine}/channel", key: "alias-case-1",
			body:      `{"CHANNEL":"canary","expected_revision":0,"confirm_display_name":"cnode-operator"}`,
			wantAudit: true, forbiddenEvidence: []string{"CHANNEL"},
		},
		{
			name: "machine canonical identity duplicate", method: http.MethodPut,
			path: "/v1/operator/machines/{machine}/channel", key: "alias-case-2",
			body:      `{"channel":"canary","CHANNEL":"stable","expected_revision":0,"confirm_display_name":"cnode-operator"}`,
			wantAudit: true, forbiddenEvidence: []string{"CHANNEL"},
		},
		{
			name: "preview single DISPLAY_NAME", method: http.MethodPost,
			path:              "/v1/operator/enrollment-tokens/preview",
			body:              `{"DISPLAY_NAME":"alias-host","ttl_seconds":3600}`,
			forbiddenEvidence: []string{"DISPLAY_NAME"},
		},
		{
			name: "preview canonical identity duplicate", method: http.MethodPost,
			path:              "/v1/operator/enrollment-tokens/preview",
			body:              `{"display_name":"alias-host","DISPLAY_NAME":"other-host","ttl_seconds":3600}`,
			forbiddenEvidence: []string{"DISPLAY_NAME"},
		},
		{
			name: "create single DISPLAY_NAME", method: http.MethodPost,
			path: "/v1/operator/enrollment-tokens", key: "alias-case-3",
			body:      `{"DISPLAY_NAME":"alias-host","ttl_seconds":3600,"preview_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"test"}`,
			wantAudit: true, forbiddenEvidence: []string{"DISPLAY_NAME"},
		},
		{
			name: "create display canonical identity duplicate", method: http.MethodPost,
			path: "/v1/operator/enrollment-tokens", key: "alias-case-4",
			body:      `{"display_name":"alias-host","DISPLAY_NAME":"other-host","ttl_seconds":3600,"preview_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"test"}`,
			wantAudit: true, forbiddenEvidence: []string{"DISPLAY_NAME"},
		},
		{
			name: "create reason canonical identity duplicate", method: http.MethodPost,
			path: "/v1/operator/enrollment-tokens", key: "alias-case-5",
			body:      `{"display_name":"alias-host","ttl_seconds":3600,"preview_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"safe","REASON":"attacker-controlled"}`,
			wantAudit: true, forbiddenEvidence: []string{"REASON", "attacker-controlled"},
		},
		{
			name: "create arbitrary attacker field", method: http.MethodPost,
			path: "/v1/operator/enrollment-tokens", key: "alias-case-6",
			body:      `{"display_name":"alias-host","ttl_seconds":3600,"preview_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"test","ATTACKER_FIELD_DO_NOT_PERSIST":true}`,
			wantAudit: true, forbiddenEvidence: []string{"ATTACKER_FIELD_DO_NOT_PERSIST"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			path := strings.Replace(tc.path, "{machine}", f.machine.id, 1)
			var machinesBefore, tokensBefore, ledgerBefore int
			if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machinesBefore); err != nil {
				t.Fatal(err)
			}
			if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM enrollment_tokens`).Scan(&tokensBefore); err != nil {
				t.Fatal(err)
			}
			if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledgerBefore); err != nil {
				t.Fatal(err)
			}

			rec := operatorRequest(t, f.mux, tc.method, path, tc.key, tc.body)
			assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
			for _, evidence := range tc.forbiddenEvidence {
				if strings.Contains(rec.Body.String(), evidence) {
					t.Fatalf("response reflected attacker field evidence %q: %s", evidence, rec.Body.String())
				}
			}

			var machinesAfter, tokensAfter, ledgerAfter int
			if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machinesAfter); err != nil {
				t.Fatal(err)
			}
			if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM enrollment_tokens`).Scan(&tokensAfter); err != nil {
				t.Fatal(err)
			}
			if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledgerAfter); err != nil {
				t.Fatal(err)
			}
			if machinesAfter != machinesBefore || tokensAfter != tokensBefore || ledgerAfter != ledgerBefore {
				t.Fatalf("case alias changed state machines=%d→%d tokens=%d→%d ledger=%d→%d",
					machinesBefore, machinesAfter, tokensBefore, tokensAfter, ledgerBefore, ledgerAfter)
			}
			machine, err := f.store.GetMachine(f.machine.id)
			if err != nil || machine.Channel != "" || machine.ChannelRevision != 0 {
				t.Fatalf("case alias changed machine=%+v err=%v", machine, err)
			}
			entries, err := f.store.Audit("", 10)
			wantEntries := 0
			if tc.wantAudit {
				wantEntries = 1
			}
			if err != nil || len(entries) != wantEntries {
				t.Fatalf("case alias audit=%+v want=%d err=%v", entries, wantEntries, err)
			}
			for _, entry := range entries {
				if !entry.IsOperatorTransportRejection() || entry.RequestDigest != "" {
					t.Fatalf("case alias was not classified as pre-domain transport rejection: %+v", entry)
				}
				for _, evidence := range tc.forbiddenEvidence {
					if strings.Contains(entry.Detail, evidence) {
						t.Fatalf("audit persisted attacker field evidence %q: %+v", evidence, entry)
					}
				}
			}
		})
	}
}

func TestOperatorMachineChannelRejectsDuplicateKnownJSONField(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	rec := operatorRequest(t, f.mux, http.MethodPut, path, "duplicate-channel-field",
		`{"channel":"canary","channel":"stable","expected_revision":0,"confirm_display_name":"cnode-operator"}`)
	assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.Channel != "" || machine.ChannelRevision != 0 {
		t.Fatalf("duplicate field changed channel: machine=%+v err=%v", machine, err)
	}
	var ledger int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key='duplicate-channel-field'`).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("duplicate field occupied idempotency key: rows=%d err=%v", ledger, err)
	}
}

func TestOperatorMachineChannelRequiresExplicitChannelLiteral(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	missing := operatorRequest(t, f.mux, http.MethodPut, path, "explicit-channel",
		`{"expected_revision":0,"confirm_display_name":"cnode-operator"}`)
	assertAPIError(t, missing, http.StatusBadRequest, "BAD_CHANNEL")

	// Explicit none is a different canonical request from a missing channel; it
	// must conflict with the occupied key, never replay the BAD_CHANNEL result.
	noneWithSameKey := operatorRequest(t, f.mux, http.MethodPut, path, "explicit-channel",
		`{"channel":"none","expected_revision":0,"confirm_display_name":"cnode-operator"}`)
	assertAPIError(t, noneWithSameKey, http.StatusConflict, "IDEMPOTENCY_CONFLICT")
	empty := operatorRequest(t, f.mux, http.MethodPut, path, "empty-channel",
		`{"channel":"","expected_revision":0,"confirm_display_name":"cnode-operator"}`)
	assertAPIError(t, empty, http.StatusBadRequest, "BAD_CHANNEL")

	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("missing/empty channel mutated machine=%+v err=%v", m, err)
	}
}

func TestOperatorMachineChannelRejectsStaleRevision(t *testing.T) {
	f := observedOperatorFixture(t)
	if err := f.store.SetMachineChannel(f.machine.id, "canary"); err != nil {
		t.Fatal(err)
	}
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	rec := operatorRequest(t, f.mux, http.MethodPut, path, "stale-channel-request",
		`{"channel":"stable","expected_revision":0,"confirm_display_name":"cnode-operator"}`)
	assertAPIError(t, rec, http.StatusPreconditionFailed, "PRECONDITION_FAILED")

	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.Channel != "canary" || m.ChannelRevision != 1 {
		t.Fatalf("stale request changed channel: machine=%+v err=%v", m, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 1 || entries[0].OK || !strings.Contains(entries[0].Detail, "revision") {
		t.Fatalf("stale request audit=%+v err=%v", entries, err)
	}
}

func TestOperatorMachineChannelRetiredMachineIsReadOnlyAndRejectionReplays(t *testing.T) {
	f := observedOperatorFixture(t)
	if err := f.store.SetMachineChannel(f.machine.id, "canary"); err != nil {
		t.Fatal(err)
	}
	// Enrollment uses the Store clock rather than the fixture's Hub clock, so
	// derive this lifecycle sequence from a current, valid Hub coordinate.
	retiredAt := time.Now().UTC()
	if err := f.store.RetireMachine(f.machine.id, retiredAt); err != nil {
		t.Fatal(err)
	}
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	body := `{"channel":"stable","expected_revision":1,"confirm_display_name":"cnode-operator"}`
	first := operatorRequest(t, f.mux, http.MethodPut, path, "retired-machine-channel", body)
	assertAPIError(t, first, http.StatusConflict, "MACHINE_RETIRED")
	if err := f.store.UnretireMachine(f.machine.id, retiredAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPut, path, "retired-machine-channel", body)
	assertAPIError(t, replay, http.StatusConflict, "MACHINE_RETIRED")
	if replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("retired rejection replay missing header: %v", replay.Header())
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.Channel != "canary" || machine.ChannelRevision != 1 {
		t.Fatalf("retired request changed channel: machine=%+v err=%v", machine, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 2 || entries[0].OK || entries[1].OK ||
		!strings.Contains(entries[0].Detail, "idempotency replay") ||
		!strings.Contains(entries[1].Detail, "退役") {
		t.Fatalf("retired request audit=%+v err=%v", entries, err)
	}
}

func TestLegacyRawChannelWriterBumpsRevisionAcrossReopenAndGets412(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	testHub := &hub{store: st, artifactsDir: artifactsDirFor(path)}
	testHub.machineAndPublicRoutes(mux)
	testHub.operatorRoutes(mux)
	machine := enrollViaHTTP(t, mux, st, "legacy-writer")
	if err := st.RecordObservation(machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: jobsTestNow,
		OpenClaw: model.OpenClaw{Present: true},
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	// Simulate an old/rollback binary that knows channel but not channel_revision.
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id=?`, machine.id); err != nil {
		t.Fatal(err)
	}
	changed, err := st.GetMachine(machine.id)
	if err != nil || changed.ChannelRevision != 1 {
		t.Fatalf("legacy writer was not revisioned: machine=%+v err=%v", changed, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mux = http.NewServeMux()
	testHub = &hub{store: st, artifactsDir: artifactsDirFor(path)}
	testHub.machineAndPublicRoutes(mux)
	testHub.operatorRoutes(mux)
	rec := operatorRequest(t, mux, http.MethodPut,
		"/v1/operator/machines/"+machine.id+"/channel", "stale-after-legacy-writer",
		`{"channel":"stable","expected_revision":0,"confirm_display_name":"legacy-writer"}`)
	assertAPIError(t, rec, http.StatusPreconditionFailed, "PRECONDITION_FAILED")
	after, err := st.GetMachine(machine.id)
	if err != nil || after.Channel != "canary" || after.ChannelRevision != 1 {
		t.Fatalf("stale request overwrote legacy writer: machine=%+v err=%v", after, err)
	}
}

func TestOperatorMachineChannelGETAndMachineBearerAreSeparate(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	// An agent credential has no role in operator authorization. The operator
	// plane consumes a LocalAPI principal; it must not inherit machine authed().
	req.Header.Set("Authorization", "Bearer definitely-not-an-agent-token")
	req = verifiedOperatorRequest(req, operatorauth.View)
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator GET inherited machine bearer middleware: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeOperatorChannelResponse(t, rec)
	if got.MachineID != f.machine.id || got.DisplayName != "cnode-operator" || got.Revision != 0 {
		t.Fatalf("operator GET=%+v", got)
	}

	agent := requestJobAPI(t, f.mux, http.MethodGet, "/v1/capabilities", "definitely-not-an-agent-token", nil)
	assertAPIError(t, agent, http.StatusUnauthorized, model.ErrUnauthorized)
}

func TestOperatorMachineChannelWrongConfirmationIsAudited(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/channel"
	rec := operatorRequest(t, f.mux, http.MethodPut, path, "wrong-confirmation",
		`{"channel":"canary","expected_revision":0,"confirm_display_name":"cnode"}`)
	assertAPIError(t, rec, http.StatusBadRequest, "CONFIRMATION_MISMATCH")
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != store.AuditMachineChannel || entries[0].OK {
		t.Fatalf("confirmation rejection audit=%+v err=%v", entries, err)
	}
	if entries[0].IdempotencyKey != "wrong-confirmation" || entries[0].RequestDigest == "" {
		t.Fatalf("confirmation rejection audit is not correlatable: %+v", entries[0])
	}
}

func TestAnEnrollmentTokenRejectedBeforeItsBodyIsReadNamesNoSubject(t *testing.T) {
	const machineIDConsequence = "transport 被擋時還沒有機器被建出來；這欄若有值，operator 會照著它去開一台不存在的機器的詳情頁"
	cases := []struct {
		name, contentType, body, key string
		wantStatus                   int
		wantCode, consequence        string
	}{
		{
			name:        "Content-Type 被擋時 body 其實讀得懂",
			contentType: "text/plain",
			body:        `{"display_name":"legible-enroll-host","ttl_seconds":3600,"preview_digest":"x","reason":"r"}`,
			key:         "content-type",
			wantStatus:  http.StatusUnsupportedMediaType,
			wantCode:    "UNSUPPORTED_MEDIA_TYPE",
			consequence: "這列的對象若寫成一個名字，operator 會拿它去機器清單跟後續成功的註冊列裡對那一台，卻對不到；而且會去改 display_name，其實該改的是 Content-Type——這條路上 Hub 根本沒讀過 body，名字可能本來就是對的",
		},
		{
			name:        "JSON 讀不完",
			contentType: "application/json",
			body:        `{`,
			key:         "incomplete-json",
			wantStatus:  http.StatusBadRequest,
			wantCode:    "BAD_REQUEST",
			consequence: "這列的對象若寫成一個名字，所有被擋下來的註冊都會疊在同一個假名下，operator 會以為那是一台反覆註冊失敗的機器",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost,
				"/v1/operator/enrollment-tokens", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Idempotency-Key", "enroll-subject-"+tc.key)
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			f.mux.ServeHTTP(rec, req)

			assertAPIError(t, rec, tc.wantStatus, tc.wantCode)
			entries, err := f.store.Audit("", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("audit entry count got=%d，預期恰好 1 列；0 列代表這次拒絕在 ledger 上完全沒留痕跡，稽核者查不到有人試過發 token；多於 1 列代表同一次請求被算成多次嘗試，會把註冊失敗率算高", len(entries))
				return
			}
			if entries[0].Subject != "" {
				t.Errorf("audit subject got=%q，預期空字串，讓 CLI／console 顯示 typed state「對象無法判讀」；%s", entries[0].Subject, tc.consequence)
			}
			if entries[0].MachineID != "" {
				t.Errorf("audit machine ID got=%q，預期空字串；%s", entries[0].MachineID, machineIDConsequence)
			}
		})
	}
}

const (
	transportRejectionContentType           = "application/json"
	transportRejectionBody                  = `{`
	transportRejectionEntryCountConsequence = "0 列代表這次拒絕在 ledger 上完全沒留痕跡，稽核者查不到有人試過；多於 1 列代表同一次請求被算成多次嘗試，會把失敗率算高"
)

func TestATransportRejectionForAnUnknownMachineNamesTheIDTheOperatorAsked(t *testing.T) {
	const unknownMachineID = "machine-not-in-the-registry"
	const unknownSubjectConsequence = "operator 會把%s這列歸成對象要等合法 JSON 才知道，下一步只去要一份能解析的 body，不會拿這列去對那台機器的時間線；重放、對帳、通知現場時會當這筆沒打到任何一台機器，但 machine id 明明就在 URL 上"
	cases := []struct {
		name, method, path, key, label string
	}{
		{name: "channel", method: http.MethodPut, path: "/v1/operator/machines/%s/channel", key: "channel", label: "channel"},
		{name: "display name", method: http.MethodPut, path: "/v1/operator/machines/%s/display-name", key: "display-name", label: "改名"},
		{name: "diagnostic noop", method: http.MethodPost, path: "/v1/operator/machines/%s/diagnostic-noop-jobs", key: "diagnostic-noop", label: "診斷"},
		{name: "notes", method: http.MethodPut, path: "/v1/operator/machines/%s/notes", key: "notes", label: "notes"},
		{name: "enrollment token revocation", method: http.MethodPost, path: "/v1/operator/machines/%s/enrollment-token/revocations", key: "enrollment-token-revocation", label: "撤銷 token"},
		{name: "lifecycle", method: http.MethodPut, path: "/v1/operator/machines/%s/lifecycle", key: "lifecycle", label: "lifecycle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, fmt.Sprintf(tc.path, unknownMachineID),
				strings.NewReader(transportRejectionBody))
			req.Header.Set("Content-Type", transportRejectionContentType)
			req.Header.Set("Idempotency-Key", "unknown-machine-"+tc.key)
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			f.mux.ServeHTTP(rec, req)

			assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
			entries, err := f.store.Audit(unknownMachineID, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("audit entry count got=%d，預期恰好 1 列；%s", len(entries), transportRejectionEntryCountConsequence)
				return
			}
			if entries[0].Subject != unknownMachineID {
				t.Errorf("audit subject got=%q，預期等於 URL 上的 machine id；"+unknownSubjectConsequence,
					entries[0].Subject, tc.label)
			}
		})
	}
}

// Existing token revocation and lifecycle tests already guard this behavior;
// repeating those cases here would duplicate coverage.
func TestATransportRejectionNamesAKnownMachineByItsDisplayName(t *testing.T) {
	const knownDisplayName = "registry-visible-host"
	const knownSubjectConsequence = "operator 日常用 display name 認機台，會拿這筆%s審計上的字串去對手上的名單；對不上就當打到另一台、或當這台還沒建檔，下一步會對錯的機器重送%s"
	cases := []struct {
		name, method, path, key, label string
	}{
		{name: "channel", method: http.MethodPut, path: "/v1/operator/machines/%s/channel", key: "channel", label: "channel"},
		{name: "display name", method: http.MethodPut, path: "/v1/operator/machines/%s/display-name", key: "display-name", label: "改名"},
		{name: "diagnostic noop", method: http.MethodPost, path: "/v1/operator/machines/%s/diagnostic-noop-jobs", key: "diagnostic-noop", label: "診斷"},
		{name: "notes", method: http.MethodPut, path: "/v1/operator/machines/%s/notes", key: "notes", label: "notes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			machineID, _, err := f.store.CreateEnrollTokenFor(knownDisplayName, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, fmt.Sprintf(tc.path, machineID),
				strings.NewReader(transportRejectionBody))
			req.Header.Set("Content-Type", transportRejectionContentType)
			req.Header.Set("Idempotency-Key", "known-machine-"+tc.key)
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			f.mux.ServeHTTP(rec, req)

			assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
			entries, err := f.store.Audit(machineID, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("audit entry count got=%d，預期恰好 1 列；%s", len(entries), transportRejectionEntryCountConsequence)
				return
			}
			if entries[0].Subject != knownDisplayName {
				t.Errorf("audit subject got=%q，預期等於名冊上的 display name；"+knownSubjectConsequence,
					entries[0].Subject, tc.label, tc.label)
			}
		})
	}
}
