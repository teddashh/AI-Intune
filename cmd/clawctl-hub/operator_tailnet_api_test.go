package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

func operatorTailnetFixture(t *testing.T) (jobsFixture, *tailnet.Cache) {
	t.Helper()
	f := newJobsFixture(t, "tailnet-machine")
	cache := tailnet.NewCache()
	cache.SetStatus(tailnet.Status{
		Available: true, ObservedAt: time.Now().UTC().Truncate(time.Second),
		Self:  tailnet.Peer{StableID: "node-hub", Hostname: "hub", IP: "100.64.0.1", Online: true},
		Peers: []tailnet.Peer{{StableID: "node-laptop", Hostname: "laptop", IP: "100.64.0.2", OS: "windows", Online: true}},
	})
	(&hub{store: f.store, tailnet: cache}).operatorRoutes(f.mux)
	return f, cache
}

func TestOperatorTailnetReadPreviewApplyAndReplay(t *testing.T) {
	f, _ := operatorTailnetFixture(t)
	read := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/tailnet", "", "")
	if read.Code != http.StatusOK || read.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("read=%d headers=%v body=%s", read.Code, read.Header(), read.Body.String())
	}
	var overview operator.TailnetOverview
	if err := json.Unmarshal(read.Body.Bytes(), &overview); err != nil || !overview.Available ||
		overview.PeerCount != 2 || len(overview.Unenrolled) != 2 || len(overview.Ignored) != 0 {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}

	expires := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	previewBody := fmt.Sprintf(`{"peer_id":"node-laptop","action":"ignore","expires_at":%q,"reason":"personal workstation"}`,
		expires.Format(time.RFC3339Nano))
	previewRec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/tailnet/peer-ignore-preview", "", previewBody)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var preview operator.TailnetPeerIgnorePreview
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil || preview.Hostname != "laptop" ||
		preview.PeerID != "node-laptop" || preview.ExpectedRevision != 0 || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	applyBody := fmt.Sprintf(`{"action":"ignore","expires_at":%q,"expected_revision":0,"confirm_hostname":"laptop","preview_digest":%q,"reason":"personal workstation"}`,
		expires.Format(time.RFC3339Nano), preview.PreviewDigest)
	fresh := operatorRequest(t, f.mux, http.MethodPut, "/v1/operator/tailnet/peer-ignores/node-laptop", "tailnet-api-key", applyBody)
	if fresh.Code != http.StatusOK || fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("fresh=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var result store.OperatorTailnetPeerIgnoreResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &result); err != nil || !result.Ignored || result.Replayed || result.Revision != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPut, "/v1/operator/tailnet/peer-ignores/node-laptop", "tailnet-api-key", applyBody)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &result); err != nil || !result.Replayed || result.Revision != 1 {
		t.Fatalf("replayed result=%+v err=%v", result, err)
	}
	entries, err := f.store.ListAuditReads(store.AuditReadFilter{Actions: []store.AuditAction{store.AuditTailnetPeerIgnore}, Limit: 10})
	if err != nil || len(entries.Items) != 2 || entries.Items[0].AuthSubject != "tailscale-user:42" ||
		entries.Items[0].AuthCapability != "example.com/cap/clawctl-admin" || entries.Items[0].SourceKind != "operator-api" {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}

func TestOperatorTailnetTransportRejectionIsAuditedWithoutOccupyingKey(t *testing.T) {
	f, _ := operatorTailnetFixture(t)
	rec := operatorRequest(t, f.mux, http.MethodPut, "/v1/operator/tailnet/peer-ignores/node-laptop", "transport-key", `{"unknown":true}`)
	assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	var receipts int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key='transport-key'`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("transport rejection receipts=%d err=%v", receipts, err)
	}
	entries, err := f.store.ListAuditReads(store.AuditReadFilter{Actions: []store.AuditAction{store.AuditTailnetPeerIgnore}, Limit: 10})
	if err != nil || len(entries.Items) != 1 || !entries.Items[0].IsOperatorTransportRejection() ||
		entries.Items[0].Subject != "node-laptop" || entries.Items[0].RequestDigest != "" {
		t.Fatalf("transport audit=%+v err=%v", entries, err)
	}
}

func TestOperatorTailnetRoutesRejectQueriesAndDiscloseUnavailableSource(t *testing.T) {
	f, cache := operatorTailnetFixture(t)
	for _, test := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/operator/tailnet?extra=1", ""},
		{http.MethodPost, "/v1/operator/tailnet/peer-ignore-preview?extra=1", `{}`},
		{http.MethodPut, "/v1/operator/tailnet/peer-ignores/node-laptop?extra=1", `{}`},
	} {
		rec := operatorRequest(t, f.mux, test.method, test.path, "query-key", test.body)
		assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	}
	cache.SetStatus(tailnet.Status{Unavailable: "tailscaled unavailable"})
	read := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/tailnet", "", "")
	var overview operator.TailnetOverview
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &overview) != nil || overview.Available ||
		overview.Unavailable != "tailscaled unavailable" {
		t.Fatalf("unavailable read=%d overview=%+v body=%s", read.Code, overview, read.Body.String())
	}
}
