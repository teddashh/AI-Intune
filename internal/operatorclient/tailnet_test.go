package operatorclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

func tailnetClientHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
}

func TestTailnetClientReadPreviewApplyAndReplay(t *testing.T) {
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("a", 64)
	previewRequest := operator.TailnetPeerIgnorePreviewRequest{
		PeerID: "node-laptop", Action: "ignore", ExpiresAt: now.Add(24 * time.Hour), Reason: "personal device",
	}
	preview := operator.TailnetPeerIgnorePreview{
		PeerID: "node-laptop", Hostname: "laptop", Action: "ignore", ObservedAt: now,
		IgnoredAfter: true, ExpiresAt: previewRequest.ExpiresAt, Reason: previewRequest.Reason,
		PreviewDigest: digest,
	}
	overview := operator.TailnetOverview{
		Available: true, ObservedAt: now, PeerCount: 2,
		Unenrolled:      []tailnet.Peer{{StableID: "node-laptop", Hostname: "laptop", IP: "100.64.0.2"}},
		OnlineButSilent: []operator.TailnetMachinePeer{}, RetiredButOnline: []tailnet.RetiredPeer{},
		Ignored: []store.TailnetPeerIgnore{},
	}
	applyRequest := TailnetPeerIgnoreRequest{
		Action: "ignore", ExpiresAt: preview.ExpiresAt, ConfirmHostname: "laptop",
		PreviewDigest: digest, Reason: preview.Reason,
	}
	result := store.OperatorTailnetPeerIgnoreResult{
		PeerID: "node-laptop", Hostname: "laptop", Action: "ignore", Ignored: true,
		ExpiresAt: preview.ExpiresAt, Revision: 1,
	}
	var puts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("headers=%v", r.Header)
		}
		tailnetClientHeaders(w)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/operator/tailnet":
			_ = json.NewEncoder(w).Encode(overview)
		case "POST /v1/operator/tailnet/peer-ignore-preview":
			var got operator.TailnetPeerIgnorePreviewRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got != previewRequest {
				t.Errorf("preview request=%+v err=%v", got, err)
			}
			_ = json.NewEncoder(w).Encode(preview)
		case "PUT /v1/operator/tailnet/peer-ignores/node-laptop":
			if r.Header.Get("Idempotency-Key") != "tailnet-key" {
				t.Errorf("idempotency header=%q", r.Header.Get("Idempotency-Key"))
			}
			var got TailnetPeerIgnoreRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got != applyRequest {
				t.Errorf("apply request=%+v err=%v", got, err)
			}
			response := result
			if puts.Add(1) > 1 {
				response.Replayed = true
				w.Header().Set("Idempotency-Replayed", "true")
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	if got, err := client.Tailnet(t.Context()); err != nil || got.PeerCount != 2 || len(got.Unenrolled) != 1 {
		t.Fatalf("overview=%+v err=%v", got, err)
	}
	if got, err := client.PreviewTailnetPeerIgnore(t.Context(), previewRequest); err != nil || got.PreviewDigest != digest {
		t.Fatalf("preview=%+v err=%v", got, err)
	}
	if got, err := client.PutTailnetPeerIgnore(t.Context(), "node-laptop", "tailnet-key", applyRequest); err != nil || got.Replayed {
		t.Fatalf("apply=%+v err=%v", got, err)
	}
	if got, err := client.PutTailnetPeerIgnore(t.Context(), "node-laptop", "tailnet-key", applyRequest); err != nil || !got.Replayed {
		t.Fatalf("replay=%+v err=%v", got, err)
	}
}

func TestTailnetClientRejectsIgnoreSuccessThatLeavesPeerUnignored(t *testing.T) {
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("a", 64)
	request := TailnetPeerIgnoreRequest{
		Action: "ignore", ExpiresAt: now.Add(24 * time.Hour),
		ConfirmHostname: "laptop", PreviewDigest: digest,
		Reason: "personal device",
	}
	honest := store.OperatorTailnetPeerIgnoreResult{
		PeerID: "node-laptop", Hostname: "laptop", Action: "ignore",
		PreviousIgnored: false, Ignored: true, ExpiresAt: request.ExpiresAt,
		Revision: 1, Replayed: false,
	}
	apply := func(result store.OperatorTailnetPeerIgnoreResult) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tailnetClientHeaders(w)
			_ = json.NewEncoder(w).Encode(result)
		}))
		defer server.Close()
		client := operatorClientForServer(t, server)
		_, err := client.PutTailnetPeerIgnore(
			t.Context(), "node-laptop", "tailnet-key", request,
		)
		return err
	}
	if err := apply(honest); err != nil {
		t.Fatalf("honest ignore response was rejected, so the next assertion "+
			"would not measure the intended clause: %v", err)
	}

	contradictory := honest
	contradictory.Ignored = false
	contradictory.ExpiresAt = time.Time{}
	const expected = "operator client: tailnet apply response does not match request target or action"
	if err := apply(contradictory); err == nil || err.Error() != expected {
		var got string
		if err != nil {
			got = err.Error()
		}
		t.Errorf("got error %q, expected %q; an ignore request whose receipt says "+
			"the peer was not ignored must not exit 0 and print a nonexistent "+
			"successful suppression, because the operator would treat the peer "+
			"as handled and continue to the next item", got, expected)
	}
}

func TestTailnetClientAcceptsLocalRulesWhileSourceIsUnavailable(t *testing.T) {
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	overview := operator.TailnetOverview{
		Unavailable: "tailscaled unavailable", Unenrolled: []tailnet.Peer{},
		OnlineButSilent: []operator.TailnetMachinePeer{}, RetiredButOnline: []tailnet.RetiredPeer{},
		Ignored: []store.TailnetPeerIgnore{{
			PeerID: "node-phone", Hostname: "phone", Reason: "personal device",
			ExpiresAt: now.Add(24 * time.Hour), Revision: 1, Active: true,
		}},
		LegacyIgnoredCount: 1,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tailnetClientHeaders(w)
		_ = json.NewEncoder(w).Encode(overview)
	}))
	defer server.Close()
	got, err := operatorClientForServer(t, server).Tailnet(t.Context())
	if err != nil || got.Available || len(got.Ignored) != 1 || got.LegacyIgnoredCount != 1 {
		t.Fatalf("overview=%+v err=%v", got, err)
	}
}

func TestTailnetClientRejectsInvalidRequestsBeforeNetwork(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, request := range []operator.TailnetPeerIgnorePreviewRequest{
		{PeerID: "", Action: "ignore", ExpiresAt: time.Now(), Reason: "reason"},
		{PeerID: "node", Action: "bad", ExpiresAt: time.Now(), Reason: "reason"},
		{PeerID: "node", Action: "ignore", Reason: "reason"},
		{PeerID: "node", Action: "unignore", ExpiresAt: time.Now(), Reason: "reason"},
		{PeerID: "node", Action: "unignore", Reason: "bad\nreason"},
	} {
		if _, err := client.PreviewTailnetPeerIgnore(t.Context(), request); err == nil {
			t.Fatalf("invalid preview accepted: %+v", request)
		}
	}
	valid := TailnetPeerIgnoreRequest{Action: "unignore", ConfirmHostname: "laptop", PreviewDigest: digest, Reason: "reason"}
	for _, test := range []struct {
		peer, key string
		body      TailnetPeerIgnoreRequest
	}{
		{"bad/id", "key", valid},
		{"node", "", valid},
		{"node", "key", func() TailnetPeerIgnoreRequest { v := valid; v.ExpectedRevision = -1; return v }()},
		{"node", "key", func() TailnetPeerIgnoreRequest { v := valid; v.PreviewDigest = "bad"; return v }()},
	} {
		if _, err := client.PutTailnetPeerIgnore(t.Context(), test.peer, test.key, test.body); err == nil {
			t.Fatalf("invalid apply accepted: %+v", test)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid requests reached network: hits=%d", hits.Load())
	}
}

func TestTailnetClientRejectsContradictoryOrExpandedSuccess(t *testing.T) {
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	responses := []string{
		`{"available":false,"unavailable":"offline","peer_count":0,"unenrolled":[],"online_but_silent":[],"retired_but_online":[],"ignored":[],"legacy_ignored_count":0,"unknown":true}`,
		fmt.Sprintf(`{"available":true,"observed_at":%q,"peer_count":0,"unenrolled":[],"online_but_silent":[],"retired_but_online":[],"ignored":[],"legacy_ignored_count":0}`, now.Format(time.RFC3339)),
		`{"available":false,"unavailable":"offline","peer_count":0,"unenrolled":null,"online_but_silent":[],"retired_but_online":[],"ignored":[],"legacy_ignored_count":0}`,
	}
	for _, body := range responses {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			tailnetClientHeaders(w)
			_, _ = w.Write([]byte(body))
		}))
		client := operatorClientForServer(t, server)
		if _, err := client.Tailnet(t.Context()); err == nil {
			server.Close()
			t.Fatalf("invalid success accepted: %s", body)
		}
		server.Close()
	}
}
