package operatorclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	revocationMachineID   = "machine-1"
	revocationDisplayName = "host-one"
	revocationCreatedAt   = "2026-09-07T12:00:00Z"
	revocationExpiresAt   = "2026-09-07T13:00:00Z"
	revocationPreviewedAt = "2026-09-07T12:30:00Z"
	revocationRevokedAt   = "2026-09-07T12:45:00Z"
)

func revocationTestDigest(hexDigit byte) string {
	return "sha256:" + strings.Repeat(string(hexDigit), 64)
}

func pendingRevocationTestJSON(machineID string) string {
	return fmt.Sprintf(
		`{"machine_id":%q,"display_name":%q,"token_created_at":%q,"token_expires_at":%q,"token_expired":false}`,
		machineID, revocationDisplayName, revocationCreatedAt, revocationExpiresAt,
	)
}

func previewRevocationTestJSON(machineID, digest string) string {
	return fmt.Sprintf(
		`{"machine_id":%q,"display_name":%q,"token_created_at":%q,"token_expires_at":%q,"token_expired":false,"previewed_at":%q,"registry_retained":true,"denominator_delta":0,"active_agent_credential_affected":false,"preview_digest":%q}`,
		machineID, revocationDisplayName, revocationCreatedAt, revocationExpiresAt,
		revocationPreviewedAt, digest,
	)
}

func revokedEnrollmentTokenTestJSON(machineID, digest string, replayed bool) string {
	return fmt.Sprintf(
		`{"machine_id":%q,"display_name":%q,"token_created_at":%q,"token_expires_at":%q,"token_was_expired":false,"revoked_at":%q,"registry_retained":true,"denominator_delta":0,"active_agent_credential_affected":false,"preview_digest":%q,"replayed":%t}`,
		machineID, revocationDisplayName, revocationCreatedAt, revocationExpiresAt,
		revocationRevokedAt, digest, replayed,
	)
}

func revocationResponseClient(t *testing.T, status int, contentType, cacheControl string,
	replayHeaders []string, body string,
) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		for _, value := range replayHeaders {
			w.Header().Add("Idempotency-Replayed", value)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return operatorClientForServer(t, server)
}

func TestEnrollmentTokenRevocationClientHappyPathAndWireContract(t *testing.T) {
	digest := revocationTestDigest('a')
	requestBody := fmt.Sprintf(`{"preview_digest":%q,"reason":"operator cleanup"}`, digest)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("call %d Accept=%q, want application/json", call, got)
		}
		if got := r.UserAgent(); got != UserAgent {
			t.Errorf("call %d User-Agent=%q, want %q", call, got, UserAgent)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("call %d unexpected query %q", call, r.URL.RawQuery)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("call %d read body: %v", call, err)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "private, max-age=0, no-store")
		switch call {
		case 1:
			if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v1/operator/machines/machine-1/enrollment-token" ||
				r.Header.Get("Content-Type") != "" || r.Header.Get("Idempotency-Key") != "" || len(raw) != 0 {
				t.Errorf("pending request method=%s path=%q content-type=%q key=%q body=%q",
					r.Method, r.URL.EscapedPath(), r.Header.Get("Content-Type"), r.Header.Get("Idempotency-Key"), raw)
			}
			_, _ = io.WriteString(w, pendingRevocationTestJSON(revocationMachineID))
		case 2:
			if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/operator/machines/machine-1/enrollment-token/revocation-preview" ||
				r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "" || string(raw) != `{}` {
				t.Errorf("preview request method=%s path=%q content-type=%q key=%q body=%q",
					r.Method, r.URL.EscapedPath(), r.Header.Get("Content-Type"), r.Header.Get("Idempotency-Key"), raw)
			}
			_, _ = io.WriteString(w, previewRevocationTestJSON(revocationMachineID, digest))
		case 3:
			if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/operator/machines/machine-1/enrollment-token/revocations" ||
				r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "revoke-key" || string(raw) != requestBody {
				t.Errorf("fresh request method=%s path=%q content-type=%q key=%q body=%q want-body=%q",
					r.Method, r.URL.EscapedPath(), r.Header.Get("Content-Type"), r.Header.Get("Idempotency-Key"), raw, requestBody)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, revokedEnrollmentTokenTestJSON(revocationMachineID, digest, false))
		case 4:
			if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/operator/machines/machine-1/enrollment-token/revocations" ||
				r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "revoke-key" || string(raw) != requestBody {
				t.Errorf("replay request method=%s path=%q content-type=%q key=%q body=%q want-body=%q",
					r.Method, r.URL.EscapedPath(), r.Header.Get("Content-Type"), r.Header.Get("Idempotency-Key"), raw, requestBody)
			}
			w.Header().Set("Idempotency-Replayed", "true")
			_, _ = io.WriteString(w, revokedEnrollmentTokenTestJSON(revocationMachineID, digest, true))
		default:
			t.Errorf("unexpected call %d", call)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)

	pending, err := client.PendingEnrollmentToken(t.Context(), revocationMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.MachineID != revocationMachineID || pending.DisplayName != revocationDisplayName ||
		pending.TokenExpired || pending.TokenExpiresAt.Sub(pending.TokenCreatedAt) != time.Hour {
		t.Fatalf("pending=%+v", pending)
	}
	preview, err := client.PreviewEnrollmentTokenRevocation(t.Context(), revocationMachineID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.MachineID != revocationMachineID || preview.PreviewDigest != digest ||
		preview.TokenExpired || !preview.RegistryRetained || preview.DenominatorDelta != 0 ||
		preview.ActiveAgentCredentialAffected || preview.PreviewedAt.Sub(preview.TokenCreatedAt) != 30*time.Minute {
		t.Fatalf("preview=%+v", preview)
	}
	body := EnrollmentTokenRevocationRequest{PreviewDigest: digest, Reason: "operator cleanup"}
	fresh, err := client.RevokeEnrollmentToken(t.Context(), revocationMachineID, "revoke-key", body)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.MachineID != revocationMachineID || fresh.PreviewDigest != digest || fresh.Replayed ||
		fresh.TokenWasExpired || !fresh.RegistryRetained || fresh.DenominatorDelta != 0 ||
		fresh.ActiveAgentCredentialAffected {
		t.Fatalf("fresh=%+v", fresh)
	}
	replay, err := client.RevokeEnrollmentToken(t.Context(), revocationMachineID, "revoke-key", body)
	if err != nil {
		t.Fatal(err)
	}
	if replay.MachineID != fresh.MachineID || replay.PreviewDigest != fresh.PreviewDigest ||
		replay.RevokedAt != fresh.RevokedAt || !replay.Replayed {
		t.Fatalf("replay=%+v fresh=%+v", replay, fresh)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("HTTP calls=%d, want 4", got)
	}
}

func TestEnrollmentTokenRevocationEscapesOpaqueMachineIDExactlyOnce(t *testing.T) {
	for _, machineID := range []string{"%61bc", "%2e%2e", "%2Ftarget", "name?query#fragment"} {
		t.Run(machineID, func(t *testing.T) {
			var gotPathValue, gotEscapedPath string
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/operator/machines/{id}/enrollment-token/revocation-preview", func(w http.ResponseWriter, r *http.Request) {
				gotPathValue = r.PathValue("id")
				gotEscapedPath = r.URL.EscapedPath()
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_, _ = io.WriteString(w, previewRevocationTestJSON(gotPathValue, revocationTestDigest('a')))
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			client := operatorClientForServer(t, server)
			if _, err := client.PreviewEnrollmentTokenRevocation(t.Context(), machineID); err != nil {
				t.Fatalf("opaque machine ID request: %v", err)
			}
			if gotPathValue != machineID {
				t.Fatalf("server machine ID=%q, want exact %q", gotPathValue, machineID)
			}
			wantPath := "/v1/operator/machines/" + url.PathEscape(machineID) + "/enrollment-token/revocation-preview"
			if gotEscapedPath != wantPath {
				t.Fatalf("escaped path=%q, want %q", gotEscapedPath, wantPath)
			}
		})
	}
}

func TestEnrollmentTokenRevocationRejectsUnsafeMachineIDsBeforeHTTP(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	body := EnrollmentTokenRevocationRequest{PreviewDigest: revocationTestDigest('a'), Reason: "cleanup"}
	for _, machineID := range []string{"", " \t", ".", "..", "a/b"} {
		t.Run(fmt.Sprintf("%q", machineID), func(t *testing.T) {
			if _, err := client.PendingEnrollmentToken(t.Context(), machineID); err == nil {
				t.Error("pending read accepted unsafe machine ID")
			}
			if _, err := client.PreviewEnrollmentTokenRevocation(t.Context(), machineID); err == nil {
				t.Error("preview accepted unsafe machine ID")
			}
			if _, err := client.RevokeEnrollmentToken(t.Context(), machineID, "key", body); err == nil {
				t.Error("revocation accepted unsafe machine ID")
			}
		})
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("unsafe machine IDs reached HTTP server %d times", got)
	}
}

func TestRevokeEnrollmentTokenRequiresIdempotencyKeyBeforeHTTP(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	body := EnrollmentTokenRevocationRequest{PreviewDigest: revocationTestDigest('a'), Reason: "cleanup"}
	for _, key := range []string{"", " \t"} {
		if _, err := client.RevokeEnrollmentToken(t.Context(), revocationMachineID, key, body); err == nil {
			t.Fatalf("accepted missing key %q", key)
		}
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("missing idempotency keys reached HTTP server %d times", got)
	}
}

func TestPendingEnrollmentTokenRejectsNoncanonicalSuccess(t *testing.T) {
	valid := pendingRevocationTestJSON(revocationMachineID)
	tests := []struct {
		name, body, contentType, cacheControl string
		status                                int
		replayHeaders                         []string
	}{
		{name: "unexpected success status", body: valid, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "wrong content type", body: valid, status: http.StatusOK, contentType: "text/plain", cacheControl: "no-store"},
		{name: "missing no-store", body: valid, status: http.StatusOK, contentType: "application/json"},
		{name: "replay header", body: valid, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"true"}},
		{name: "invalid replay header", body: valid, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"TRUE"}},
		{name: "duplicate replay header", body: valid, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"true", "true"}},
		{name: "wrong machine echo", body: strings.Replace(valid, `"machine_id":"machine-1"`, `"machine_id":"other"`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "blank display name", body: strings.Replace(valid, `"display_name":"host-one"`, `"display_name":" "`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "created equals expiry", body: strings.Replace(valid, revocationExpiresAt, revocationCreatedAt, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "missing field", body: strings.Replace(valid, `,"token_expired":false`, "", 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "duplicate field", body: strings.Replace(valid, `"token_expired":false`, `"token_expired":false,"token_expired":false`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "unknown field", body: strings.TrimSuffix(valid, "}") + `,"extra":true}`, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "null field", body: strings.Replace(valid, `"display_name":"host-one"`, `"display_name":null`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "trailing JSON", body: valid + `{}`, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := revocationResponseClient(t, tc.status, tc.contentType, tc.cacheControl, tc.replayHeaders, tc.body)
			if result, err := client.PendingEnrollmentToken(t.Context(), revocationMachineID); err == nil {
				t.Fatalf("accepted noncanonical pending response: %+v", result)
			}
		})
	}
}

func TestPreviewEnrollmentTokenRevocationRejectsNoncanonicalSuccess(t *testing.T) {
	digest := revocationTestDigest('a')
	valid := previewRevocationTestJSON(revocationMachineID, digest)
	tests := []struct {
		name, body, contentType, cacheControl string
		status                                int
		replayHeaders                         []string
	}{
		{name: "unexpected success status", body: valid, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "wrong content type", body: valid, status: http.StatusOK, contentType: "text/plain", cacheControl: "no-store"},
		{name: "missing no-store", body: valid, status: http.StatusOK, contentType: "application/json"},
		{name: "replay header", body: valid, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"true"}},
		{name: "invalid replay header", body: valid, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"1"}},
		{name: "duplicate replay header", body: valid, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"true", "true"}},
		{name: "wrong machine echo", body: strings.Replace(valid, `"machine_id":"machine-1"`, `"machine_id":"other"`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "blank display name", body: strings.Replace(valid, `"display_name":"host-one"`, `"display_name":" "`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "created equals expiry", body: strings.Replace(valid, revocationExpiresAt, revocationCreatedAt, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "preview before creation", body: strings.Replace(valid, revocationPreviewedAt, "2026-09-07T11:59:59Z", 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "expiry flag mismatch", body: strings.Replace(valid, `"token_expired":false`, `"token_expired":true`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "registry not retained", body: strings.Replace(valid, `"registry_retained":true`, `"registry_retained":false`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "denominator changes", body: strings.Replace(valid, `"denominator_delta":0`, `"denominator_delta":-1`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "active credential affected", body: strings.Replace(valid, `"active_agent_credential_affected":false`, `"active_agent_credential_affected":true`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "noncanonical digest", body: strings.Replace(valid, digest, "sha256:"+strings.Repeat("A", 64), 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "missing field", body: strings.Replace(valid, fmt.Sprintf(`,"preview_digest":%q`, digest), "", 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "duplicate field", body: strings.Replace(valid, `"denominator_delta":0`, `"denominator_delta":0,"denominator_delta":0`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "unknown field", body: strings.TrimSuffix(valid, "}") + `,"extra":true}`, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "null field", body: strings.Replace(valid, `"registry_retained":true`, `"registry_retained":null`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "trailing JSON", body: valid + `{}`, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := revocationResponseClient(t, tc.status, tc.contentType, tc.cacheControl, tc.replayHeaders, tc.body)
			if result, err := client.PreviewEnrollmentTokenRevocation(t.Context(), revocationMachineID); err == nil {
				t.Fatalf("accepted noncanonical preview response: %+v", result)
			}
		})
	}
}

func TestRevokeEnrollmentTokenRejectsNoncanonicalSuccess(t *testing.T) {
	digest := revocationTestDigest('a')
	otherDigest := revocationTestDigest('b')
	fresh := revokedEnrollmentTokenTestJSON(revocationMachineID, digest, false)
	replay := revokedEnrollmentTokenTestJSON(revocationMachineID, digest, true)
	tests := []struct {
		name, body, contentType, cacheControl string
		status                                int
		replayHeaders                         []string
	}{
		{name: "unexpected success status", body: fresh, status: http.StatusAccepted, contentType: "application/json", cacheControl: "no-store"},
		{name: "fresh as 200", body: fresh, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "replay as 201", body: replay, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"true"}},
		{name: "replay missing header", body: replay, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "fresh replay header", body: fresh, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"true"}},
		{name: "invalid replay header", body: replay, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"yes"}},
		{name: "duplicate replay header", body: replay, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: []string{"true", "true"}},
		{name: "wrong content type", body: fresh, status: http.StatusCreated, contentType: "text/plain", cacheControl: "no-store"},
		{name: "missing no-store", body: fresh, status: http.StatusCreated, contentType: "application/json"},
		{name: "wrong machine echo", body: strings.Replace(fresh, `"machine_id":"machine-1"`, `"machine_id":"other"`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "blank display name", body: strings.Replace(fresh, `"display_name":"host-one"`, `"display_name":" "`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "created equals expiry", body: strings.Replace(fresh, revocationExpiresAt, revocationCreatedAt, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "revoked before creation", body: strings.Replace(fresh, revocationRevokedAt, "2026-09-07T11:59:59Z", 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "expiry flag mismatch", body: strings.Replace(fresh, `"token_was_expired":false`, `"token_was_expired":true`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "registry not retained", body: strings.Replace(fresh, `"registry_retained":true`, `"registry_retained":false`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "denominator changes", body: strings.Replace(fresh, `"denominator_delta":0`, `"denominator_delta":-1`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "active credential affected", body: strings.Replace(fresh, `"active_agent_credential_affected":false`, `"active_agent_credential_affected":true`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "wrong digest", body: strings.Replace(fresh, digest, otherDigest, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "noncanonical digest", body: strings.Replace(fresh, digest, "sha256:"+strings.Repeat("A", 64), 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "missing field", body: strings.Replace(fresh, `,"replayed":false`, "", 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "duplicate field", body: strings.Replace(fresh, `"replayed":false`, `"replayed":false,"replayed":false`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "unknown field", body: strings.TrimSuffix(fresh, "}") + `,"extra":true}`, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "null field", body: strings.Replace(fresh, `"revoked_at":"2026-09-07T12:45:00Z"`, `"revoked_at":null`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "trailing JSON", body: fresh + `{}`, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
	}
	request := EnrollmentTokenRevocationRequest{PreviewDigest: digest, Reason: "cleanup"}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := revocationResponseClient(t, tc.status, tc.contentType, tc.cacheControl, tc.replayHeaders, tc.body)
			if result, err := client.RevokeEnrollmentToken(t.Context(), revocationMachineID, "revoke-key", request); err == nil {
				t.Fatalf("accepted noncanonical revocation response: %+v", result)
			}
		})
	}
}

func TestEnrollmentTokenRevocationAPIErrorPreservesReplayEvidence(t *testing.T) {
	client := revocationResponseClient(t, http.StatusPreconditionFailed, "application/json", "no-store",
		[]string{"true"}, `{"code":"PREVIEW_STALE","message":"preview is stale"}`)
	_, err := client.RevokeEnrollmentToken(context.Background(), revocationMachineID, "revoke-key", EnrollmentTokenRevocationRequest{
		PreviewDigest: revocationTestDigest('a'), Reason: "cleanup",
	})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.StatusCode != http.StatusPreconditionFailed || apiErr.Code != "PREVIEW_STALE" || !apiErr.Replayed {
		t.Fatalf("API error=%T %+v", err, apiErr)
	}
}

func TestEnrollmentTokenRevocationBoundaryExpiryEvidence(t *testing.T) {
	digest := revocationTestDigest('a')
	preview := strings.Replace(previewRevocationTestJSON(revocationMachineID, digest), revocationPreviewedAt, revocationExpiresAt, 1)
	preview = strings.Replace(preview, `"token_expired":false`, `"token_expired":true`, 1)
	previewClient := revocationResponseClient(t, http.StatusOK, "application/json", "no-store", nil, preview)
	if result, err := previewClient.PreviewEnrollmentTokenRevocation(t.Context(), revocationMachineID); err != nil || !result.TokenExpired {
		t.Fatalf("exclusive preview expiry boundary rejected: result=%+v err=%v", result, err)
	}

	revoked := strings.Replace(revokedEnrollmentTokenTestJSON(revocationMachineID, digest, false), revocationRevokedAt, revocationExpiresAt, 1)
	revoked = strings.Replace(revoked, `"token_was_expired":false`, `"token_was_expired":true`, 1)
	revokeClient := revocationResponseClient(t, http.StatusCreated, "application/json", "no-store", nil, revoked)
	if result, err := revokeClient.RevokeEnrollmentToken(t.Context(), revocationMachineID, "revoke-key", EnrollmentTokenRevocationRequest{
		PreviewDigest: digest, Reason: "cleanup",
	}); err != nil || !result.TokenWasExpired {
		t.Fatalf("exclusive revocation expiry boundary rejected: result=%+v err=%v", result, err)
	}
}

func TestEnrollmentTokenRevocationRequestJSONHasExactFields(t *testing.T) {
	request := EnrollmentTokenRevocationRequest{PreviewDigest: revocationTestDigest('a'), Reason: "cleanup"}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), fmt.Sprintf(`{"preview_digest":%q,"reason":"cleanup"}`, request.PreviewDigest); got != want {
		t.Fatalf("request JSON=%s, want=%s", got, want)
	}
}
