package operatorclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func operatorClientForServer(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	base, authority := tailnetBaseForServer(t, server)
	client, err := NewWithHTTPClient(base, &http.Client{
		Transport: transportDialingServer(t, authority, server),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func enrollmentClientTestDigest() string {
	return "sha256:" + strings.Repeat("a", 64)
}

func enrollmentClientTestToken() string {
	return base64.RawURLEncoding.EncodeToString(make([]byte, 32))
}

func tailnetBaseForServer(t *testing.T, server *httptest.Server) (string, string) {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	logicalAuthority := net.JoinHostPort("100.64.0.1", u.Port())
	return "http://" + logicalAuthority, logicalAuthority
}

func transportDialingServer(t *testing.T, logicalAuthority string, server *httptest.Server) *http.Transport {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == logicalAuthority {
			address = server.Listener.Addr().String()
		}
		return dialer.DialContext(ctx, network, address)
	}
	return transport
}

func TestNonJSONErrorStillHasStableAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != UserAgent {
			t.Errorf("operator client User-Agent=%q, want %q", got, UserAgent)
		}
		http.Error(w, "proxy generated HTML", http.StatusTeapot)
	}))
	defer server.Close()
	base, logicalAuthority := tailnetBaseForServer(t, server)
	client, err := NewWithHTTPClient(base, &http.Client{
		Transport: transportDialingServer(t, logicalAuthority, server),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetMachineChannel(context.Background(), "machine-id")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTeapot ||
		apiErr.Code != "HTTP_ERROR" || apiErr.Message != http.StatusText(http.StatusTeapot) {
		t.Fatalf("fallback error=%T %v API=%+v", err, err, apiErr)
	}
}

func TestCustomHTTPClientCannotRestoreRedirectedMutation(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits.Add(1)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL+"/stolen")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	base, logicalAuthority := tailnetBaseForServer(t, redirect)
	suppliedTransport := transportDialingServer(t, logicalAuthority, redirect)
	supplied := &http.Client{Transport: suppliedTransport}
	client, err := NewWithHTTPClient(base, supplied)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PutMachineChannel(context.Background(), "machine-id", "redirect-key", MachineChannelRequest{
		Channel: "canary", ExpectedRevision: 0, ConfirmDisplayName: "machine",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect response=%T %v", err, err)
	}
	if targetHits.Load() != 0 {
		t.Fatal("custom HTTP client resent operator mutation to redirect authority")
	}
	if supplied.Timeout != 0 || supplied.CheckRedirect != nil || supplied.Transport != suppliedTransport {
		t.Fatal("NewWithHTTPClient mutated caller's client")
	}
	if client.http.Timeout != 30*time.Second || client.http.CheckRedirect == nil {
		t.Fatalf("operator safety policy missing: timeout=%s redirect_set=%t",
			client.http.Timeout, client.http.CheckRedirect != nil)
	}
}

func TestOperatorClientBypassesConfiguredProxy(t *testing.T) {
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"channel-revision-1"`)
		_, _ = fmt.Fprint(w, `{"machine_id":"machine-id","display_name":"machine","previous_channel":"stable","channel":"stable","revision":1,"replayed":false}`)
	}))
	defer origin.Close()

	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		http.Error(w, "operator request reached proxy", http.StatusBadGateway)
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}

	base, logicalAuthority := tailnetBaseForServer(t, origin)
	transport := transportDialingServer(t, logicalAuthority, origin)
	transport.Proxy = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	supplied := &http.Client{Transport: transport}
	client, err := NewWithHTTPClient(base, supplied)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetMachineChannel(context.Background(), "machine-id"); err != nil {
		t.Fatal(err)
	}
	if got := originHits.Load(); got != 1 {
		t.Fatalf("origin hits=%d, want 1", got)
	}
	if got := proxyHits.Load(); got != 0 {
		t.Fatalf("proxy intercepted %d operator requests", got)
	}
	if supplied.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("NewWithHTTPClient mutated caller's proxy policy")
	}
	cloned, ok := client.http.Transport.(*http.Transport)
	if !ok || cloned == transport || cloned.Proxy != nil {
		t.Fatal("operator client did not clone transport with Proxy=nil")
	}
}

func TestProductionClientHasNoAmbientProxyHook(t *testing.T) {
	client, err := New("http://100.64.0.1:8787")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("production transport=%T proxy_disabled=%t", client.http.Transport, ok && transport.Proxy == nil)
	}
}

type wrappedRoundTripper struct {
	next http.RoundTripper
}

func (w wrappedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return w.next.RoundTrip(req)
}

func TestOperatorClientRejectsOpaqueRoundTripper(t *testing.T) {
	client, err := NewWithHTTPClient("http://100.64.0.1:8787", &http.Client{
		Transport: wrappedRoundTripper{next: http.DefaultTransport},
	})
	if err == nil || client != nil {
		t.Fatalf("opaque RoundTripper accepted: client=%+v err=%v", client, err)
	}
}

func TestMachineChannelRejectsPathCleaningSegmentsBeforeHTTP(t *testing.T) {
	client, err := New("http://100.64.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, machineID := range []string{"", ".", "..", "a/b"} {
		if _, err := client.GetMachineChannel(context.Background(), machineID); err == nil {
			t.Errorf("machine_id %q was accepted", machineID)
		}
	}
}

func TestMachineChannelEscapesOpaqueMachineIDExactlyOnce(t *testing.T) {
	for _, machineID := range []string{"%61bc", "%2e%2e", "%2Ftarget", "name?query#fragment"} {
		t.Run(machineID, func(t *testing.T) {
			var gotPathValue string
			mux := http.NewServeMux()
			mux.HandleFunc("PUT /v1/operator/machines/{id}/channel", func(w http.ResponseWriter, r *http.Request) {
				gotPathValue = r.PathValue("id")
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", `"channel-revision-1"`)
				_ = json.NewEncoder(w).Encode(MachineChannelResponse{
					MachineID: gotPathValue, DisplayName: "machine", PreviousChannel: "",
					Channel: "canary", Revision: 1,
				})
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			base, authority := tailnetBaseForServer(t, server)
			client, err := NewWithHTTPClient(base, &http.Client{
				Transport: transportDialingServer(t, authority, server),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.PutMachineChannel(t.Context(), machineID, "opaque-id", MachineChannelRequest{
				Channel: "canary", ExpectedRevision: 0, ConfirmDisplayName: "machine",
			}); err != nil {
				t.Fatalf("opaque machine ID request: %v", err)
			}
			if gotPathValue != machineID {
				t.Fatalf("server received machine ID %q, want exact %q", gotPathValue, machineID)
			}
		})
	}
}

func TestClientRejectsNonPinnedBaseURLs(t *testing.T) {
	for _, raw := range []string{
		"http://hub.example:8787",
		"https://100.64.0.1:8787",
		"http://127.0.0.1:8787",
		"http://192.168.1.2:8787",
		"http://100.64.0.1",
		"http://100.64.0.1:0",
		"http://100.64.0.1:8787/base",
		"http://user@100.64.0.1:8787",
		"http://100.64.0.1:8787?query",
		"http://100.64.0.1:8787#fragment",
		" http://100.64.0.1:8787",
	} {
		t.Run(raw, func(t *testing.T) {
			if client, err := New(raw); err == nil {
				t.Fatalf("New(%q)=%+v, want rejection", raw, client)
			}
		})
	}
}

func TestClientRejectsMaliciousSuccessEvidence(t *testing.T) {
	for _, test := range []struct {
		name, body, etag string
	}{
		{name: "wrong machine", body: `{"machine_id":"other","display_name":"machine","previous_channel":"stable","channel":"stable","revision":1,"replayed":false}`, etag: `"channel-revision-1"`},
		{name: "invalid channel", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"privileged","channel":"privileged","revision":1,"replayed":false}`, etag: `"channel-revision-1"`},
		{name: "empty display name", body: `{"machine_id":"machine-id","display_name":"","previous_channel":"stable","channel":"stable","revision":1,"replayed":false}`, etag: `"channel-revision-1"`},
		{name: "etag mismatch", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"stable","channel":"stable","revision":1,"replayed":false}`, etag: `"channel-revision-2"`},
		{name: "GET transition", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"canary","channel":"stable","revision":1,"replayed":false}`, etag: `"channel-revision-1"`},
		{name: "GET replay", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"stable","channel":"stable","revision":1,"replayed":true}`, etag: `"channel-revision-1"`},
		{name: "missing field", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"stable","channel":"stable","revision":1}`, etag: `"channel-revision-1"`},
		{name: "duplicate field", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"stable","channel":"stable","revision":1,"revision":1,"replayed":false}`, etag: `"channel-revision-1"`},
		{name: "unknown field", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"stable","channel":"stable","revision":1,"replayed":false,"extra":true}`, etag: `"channel-revision-1"`},
		{name: "null field", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":null,"channel":"stable","revision":1,"replayed":false}`, etag: `"channel-revision-1"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", test.etag)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			base, authority := tailnetBaseForServer(t, server)
			client, err := NewWithHTTPClient(base, &http.Client{
				Transport: transportDialingServer(t, authority, server),
			})
			if err != nil {
				t.Fatal(err)
			}
			if result, err := client.GetMachineChannel(t.Context(), "machine-id"); err == nil {
				t.Fatalf("accepted malicious response: %+v", result)
			}
		})
	}
}

func TestClientRejectsNoncanonicalSuccessHeadersAndStatus(t *testing.T) {
	validBody := `{"machine_id":"machine-id","display_name":"machine","previous_channel":"stable","channel":"stable","revision":1,"replayed":false}`
	for _, test := range []struct {
		name, body  string
		status      int
		contentType string
		etags       []string
		replays     []string
	}{
		{name: "wrong content type", status: http.StatusOK, contentType: "text/plain", etags: []string{`"channel-revision-1"`}},
		{name: "unexpected 201", status: http.StatusCreated, contentType: "application/json", etags: []string{`"channel-revision-1"`}},
		{name: "duplicate etag", status: http.StatusOK, contentType: "application/json", etags: []string{`"channel-revision-1"`, `"channel-revision-1"`}},
		{name: "false replay header", status: http.StatusOK, contentType: "application/json", etags: []string{`"channel-revision-1"`}, replays: []string{"false"}},
		{name: "duplicate replay header", status: http.StatusOK, contentType: "application/json", etags: []string{`"channel-revision-1"`}, replays: []string{"true", "true"}},
		{name: "plus etag revision", status: http.StatusOK, contentType: "application/json", etags: []string{`"channel-revision-+1"`}},
		{name: "leading-zero etag revision", status: http.StatusOK, contentType: "application/json", etags: []string{`"channel-revision-01"`}},
		{name: "negative-zero etag revision", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"","channel":"","revision":0,"replayed":false}`, status: http.StatusOK, contentType: "application/json", etags: []string{`"channel-revision--0"`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				for _, value := range test.etags {
					w.Header().Add("ETag", value)
				}
				for _, value := range test.replays {
					w.Header().Add("Idempotency-Replayed", value)
				}
				w.WriteHeader(test.status)
				body := test.body
				if body == "" {
					body = validBody
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			base, authority := tailnetBaseForServer(t, server)
			client, err := NewWithHTTPClient(base, &http.Client{
				Transport: transportDialingServer(t, authority, server),
			})
			if err != nil {
				t.Fatal(err)
			}
			if result, err := client.GetMachineChannel(t.Context(), "machine-id"); err == nil {
				t.Fatalf("accepted noncanonical success: %+v", result)
			}
		})
	}
}

func TestPutRejectsSuccessThatDoesNotMatchCanonicalRequest(t *testing.T) {
	for _, test := range []struct {
		name, body, etag string
	}{
		{name: "wrong channel", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"","channel":"stable","revision":1,"replayed":false}`, etag: `"channel-revision-1"`},
		{name: "wrong confirmation", body: `{"machine_id":"machine-id","display_name":"other","previous_channel":"","channel":"canary","revision":1,"replayed":false}`, etag: `"channel-revision-1"`},
		{name: "impossible revision", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"","channel":"canary","revision":7,"replayed":false}`, etag: `"channel-revision-7"`},
		{name: "transition without revision", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"stable","channel":"canary","revision":0,"replayed":false}`, etag: `"channel-revision-0"`},
		{name: "revision without transition", body: `{"machine_id":"machine-id","display_name":"machine","previous_channel":"canary","channel":"canary","revision":1,"replayed":false}`, etag: `"channel-revision-1"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("ETag", test.etag)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			base, authority := tailnetBaseForServer(t, server)
			client, err := NewWithHTTPClient(base, &http.Client{
				Transport: transportDialingServer(t, authority, server),
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.PutMachineChannel(t.Context(), "machine-id", "canonical-key", MachineChannelRequest{
				Channel: "canary", ExpectedRevision: 0, ConfirmDisplayName: "machine",
			})
			if err == nil {
				t.Fatalf("accepted noncanonical response: %+v", result)
			}
		})
	}
}

func TestEnrollmentTokenClientFreshAndReplayContracts(t *testing.T) {
	digest := enrollmentClientTestDigest()
	token := enrollmentClientTestToken()
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent ||
			r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request headers=%v", r.Header)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "private, no-store")
		switch r.URL.Path {
		case "/v1/operator/enrollment-tokens/preview":
			if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != "" ||
				string(raw) != `{"display_name":"new-host","ttl_seconds":3600}` {
				t.Errorf("preview request method=%s key=%q body=%s", r.Method, r.Header.Get("Idempotency-Key"), raw)
			}
			_, _ = fmt.Fprintf(w, `{"display_name":"new-host","ttl_seconds":3600,"previewed_at":"2026-09-07T12:00:00Z","expires_at_if_created_now":"2026-09-07T13:00:00Z","creates_expected_machine":true,"initial_state":"never_reported","revocation_keeps_registry_row":true,"secret_delivery":"first-response-only","preview_digest":%q,"limit_set":true,"limit_max_machines":10,"in_denominator":4,"headroom":6,"at_limit":false}`, digest)
		case "/v1/operator/enrollment-tokens":
			if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != "create-key" {
				t.Errorf("create request method=%s key=%q", r.Method, r.Header.Get("Idempotency-Key"))
			}
			wantBody := fmt.Sprintf(`{"display_name":"new-host","ttl_seconds":3600,"preview_digest":%q,"reason":"bootstrap"}`, digest)
			if string(raw) != wantBody {
				t.Errorf("create body=%s want=%s", raw, wantBody)
			}
			if createCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"machine_id":"machine-1","display_name":"new-host","created_at":"2026-09-07T12:00:00Z","expires_at":"2026-09-07T13:00:00Z","ttl_seconds":3600,"preview_digest":%q,"enrollment_token":%q,"secret_available":true,"replayed":false,"recovery_required":false,"recovery_action":""}`, digest, token)
				return
			}
			w.Header().Set("Idempotency-Replayed", "true")
			_, _ = fmt.Fprintf(w, `{"machine_id":"machine-1","display_name":"new-host","created_at":"2026-09-07T12:00:00Z","expires_at":"2026-09-07T13:00:00Z","ttl_seconds":3600,"preview_digest":%q,"secret_available":false,"replayed":true,"recovery_required":true,"recovery_action":"revoke_and_reissue"}`, digest)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)

	preview, err := client.PreviewEnrollToken(t.Context(), EnrollmentTokenPreviewRequest{
		DisplayName: "new-host", TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.DisplayName != "new-host" || preview.TTLSeconds != 3600 ||
		preview.ExpiresAtIfCreatedNow.Sub(preview.PreviewedAt) != time.Hour ||
		preview.PreviewDigest != digest || !preview.CreatesExpectedMachine ||
		!preview.RevocationKeepsRegistryRow {
		t.Fatalf("preview=%+v", preview)
	}
	request := EnrollmentTokenCreateRequest{
		DisplayName: "new-host", TTLSeconds: 3600, PreviewDigest: digest, Reason: "bootstrap",
	}
	fresh, err := client.CreateEnrollToken(t.Context(), "create-key", request)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.MachineID != "machine-1" || fresh.EnrollmentToken != token ||
		!fresh.SecretAvailable || fresh.Replayed || fresh.RecoveryRequired || fresh.RecoveryAction != "" {
		t.Fatalf("fresh=%+v", fresh)
	}
	replay, err := client.CreateEnrollToken(t.Context(), "create-key", request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.MachineID != fresh.MachineID || replay.EnrollmentToken != "" ||
		replay.SecretAvailable || !replay.Replayed || !replay.RecoveryRequired ||
		replay.RecoveryAction != "revoke_and_reissue" {
		t.Fatalf("replay=%+v", replay)
	}
}

func TestPreviewEnrollTokenRejectsNoncanonicalSuccess(t *testing.T) {
	digest := enrollmentClientTestDigest()
	valid := fmt.Sprintf(`{"display_name":"new-host","ttl_seconds":3600,"previewed_at":"2026-09-07T12:00:00Z","expires_at_if_created_now":"2026-09-07T13:00:00Z","creates_expected_machine":true,"initial_state":"never_reported","revocation_keeps_registry_row":true,"secret_delivery":"first-response-only","preview_digest":%q,"limit_set":true,"limit_max_machines":10,"in_denominator":4,"headroom":6,"at_limit":false}`, digest)
	tests := []struct {
		name, body, contentType, cacheControl string
		status, replayHeaders                 int
	}{
		{name: "unexpected status", body: valid, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "wrong content type", body: valid, status: http.StatusOK, contentType: "text/plain", cacheControl: "no-store"},
		{name: "missing no-store", body: valid, status: http.StatusOK, contentType: "application/json"},
		{name: "replay header", body: valid, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: 1},
		{name: "duplicate replay header", body: valid, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: 2},
		{name: "wrong display echo", body: strings.Replace(valid, `"display_name":"new-host"`, `"display_name":"other"`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "wrong ttl echo", body: strings.Replace(valid, `"ttl_seconds":3600`, `"ttl_seconds":60`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "wrong expiry", body: strings.Replace(valid, "2026-09-07T13:00:00Z", "2026-09-07T13:00:01Z", 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "false impact", body: strings.Replace(valid, `"creates_expected_machine":true`, `"creates_expected_machine":false`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "bad literal", body: strings.Replace(valid, "first-response-only", "sometimes", 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "noncanonical digest", body: strings.Replace(valid, digest, "sha256:"+strings.Repeat("A", 64), 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "missing field", body: strings.Replace(valid, `,"secret_delivery":"first-response-only"`, "", 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "duplicate field", body: strings.Replace(valid, `"ttl_seconds":3600`, `"ttl_seconds":3600,"ttl_seconds":3600`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "unknown field", body: strings.TrimSuffix(valid, "}") + `,"extra":true}`, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "null field", body: strings.Replace(valid, `"initial_state":"never_reported"`, `"initial_state":null`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "trailing JSON", body: valid + `{}`, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		// ⚠ 一份說「還收得下」但其實已經到上限的預覽，會讓人按下去才發現開不了票。
		{name: "headroom lies", body: strings.Replace(valid, `"headroom":6`, `"headroom":99`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "at limit but says room", body: strings.Replace(strings.Replace(valid, `"in_denominator":4`, `"in_denominator":10`, 1), `"headroom":6`, `"headroom":0`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "no limit but carries one", body: strings.Replace(valid, `"limit_set":true`, `"limit_set":false`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "negative denominator", body: strings.Replace(valid, `"in_denominator":4`, `"in_denominator":-1`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.cacheControl != "" {
					w.Header().Set("Cache-Control", tc.cacheControl)
				}
				for i := 0; i < tc.replayHeaders; i++ {
					w.Header().Add("Idempotency-Replayed", "true")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := operatorClientForServer(t, server)
			if result, err := client.PreviewEnrollToken(t.Context(), EnrollmentTokenPreviewRequest{
				DisplayName: "new-host", TTLSeconds: 3600,
			}); err == nil {
				t.Fatalf("accepted noncanonical preview: %+v", result)
			}
		})
	}
}

func TestCreateEnrollTokenRejectsNoncanonicalSuccess(t *testing.T) {
	digest := enrollmentClientTestDigest()
	token := enrollmentClientTestToken()
	fresh := fmt.Sprintf(`{"machine_id":"machine-1","display_name":"new-host","created_at":"2026-09-07T12:00:00Z","expires_at":"2026-09-07T13:00:00Z","ttl_seconds":3600,"preview_digest":%q,"enrollment_token":%q,"secret_available":true,"replayed":false,"recovery_required":false,"recovery_action":""}`, digest, token)
	replay := fmt.Sprintf(`{"machine_id":"machine-1","display_name":"new-host","created_at":"2026-09-07T12:00:00Z","expires_at":"2026-09-07T13:00:00Z","ttl_seconds":3600,"preview_digest":%q,"secret_available":false,"replayed":true,"recovery_required":true,"recovery_action":"revoke_and_reissue"}`, digest)
	tests := []struct {
		name, body, contentType, cacheControl string
		status, replayHeaders                 int
	}{
		{name: "fresh as 200", body: fresh, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "replay as 201", body: replay, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store", replayHeaders: 1},
		{name: "replay missing header", body: replay, status: http.StatusOK, contentType: "application/json", cacheControl: "no-store"},
		{name: "fresh replay header", body: fresh, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store", replayHeaders: 1},
		{name: "wrong content type", body: fresh, status: http.StatusCreated, contentType: "text/plain", cacheControl: "no-store"},
		{name: "missing no-store", body: fresh, status: http.StatusCreated, contentType: "application/json"},
		{name: "fresh missing token", body: strings.Replace(fresh, fmt.Sprintf(`,"enrollment_token":%q`, token), "", 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "fresh invalid token", body: strings.Replace(fresh, token, "not-a-token", 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "fresh claims no secret", body: strings.Replace(fresh, `"secret_available":true`, `"secret_available":false`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "fresh recovery", body: strings.Replace(fresh, `"recovery_required":false`, `"recovery_required":true`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "replay token field even empty", body: strings.Replace(replay, `"secret_available":false`, `"enrollment_token":"","secret_available":false`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: 1},
		{name: "replay claims secret", body: strings.Replace(replay, `"secret_available":false`, `"secret_available":true`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: 1},
		{name: "replay no recovery", body: strings.Replace(replay, `"recovery_required":true`, `"recovery_required":false`, 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: 1},
		{name: "replay wrong recovery action", body: strings.Replace(replay, "revoke_and_reissue", "retry", 1), status: http.StatusOK, contentType: "application/json", cacheControl: "no-store", replayHeaders: 1},
		{name: "wrong display echo", body: strings.Replace(fresh, `"display_name":"new-host"`, `"display_name":"other"`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "wrong ttl echo", body: strings.Replace(fresh, `"ttl_seconds":3600`, `"ttl_seconds":60`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "wrong expiry", body: strings.Replace(fresh, "2026-09-07T13:00:00Z", "2026-09-07T13:00:01Z", 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "wrong digest", body: strings.Replace(fresh, digest, "sha256:"+strings.Repeat("b", 64), 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "missing field", body: strings.Replace(fresh, `,"recovery_action":""`, "", 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "duplicate field", body: strings.Replace(fresh, `"replayed":false`, `"replayed":false,"replayed":false`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "unknown field", body: strings.TrimSuffix(fresh, "}") + `,"extra":true}`, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "null field", body: strings.Replace(fresh, `"machine_id":"machine-1"`, `"machine_id":null`, 1), status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
		{name: "trailing JSON", body: fresh + `{}`, status: http.StatusCreated, contentType: "application/json", cacheControl: "no-store"},
	}
	request := EnrollmentTokenCreateRequest{
		DisplayName: "new-host", TTLSeconds: 3600, PreviewDigest: digest, Reason: "bootstrap",
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.cacheControl != "" {
					w.Header().Set("Cache-Control", tc.cacheControl)
				}
				for i := 0; i < tc.replayHeaders; i++ {
					w.Header().Add("Idempotency-Replayed", "true")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := operatorClientForServer(t, server)
			if result, err := client.CreateEnrollToken(t.Context(), "create-key", request); err == nil {
				t.Fatalf("accepted noncanonical create response: %+v", result)
			}
		})
	}
}

func TestCreateEnrollTokenRequiresKeyBeforeHTTP(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	request := EnrollmentTokenCreateRequest{
		DisplayName: "new-host", TTLSeconds: 3600, PreviewDigest: enrollmentClientTestDigest(),
	}
	for _, key := range []string{"", " \t"} {
		if _, err := client.CreateEnrollToken(t.Context(), key, request); err == nil {
			t.Fatalf("accepted missing key %q", key)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("missing idempotency key reached HTTP server %d times", hits.Load())
	}
}

func TestEnrollmentTokenAPIErrorPreservesReplayEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `{"code":"PREVIEW_STALE","message":"preview expired"}`)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	_, err := client.CreateEnrollToken(t.Context(), "create-key", EnrollmentTokenCreateRequest{
		DisplayName: "new-host", TTLSeconds: 3600, PreviewDigest: enrollmentClientTestDigest(),
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusPreconditionFailed ||
		apiErr.Code != "PREVIEW_STALE" || !apiErr.Replayed {
		t.Fatalf("replayed API error=%T %+v", err, apiErr)
	}
}
