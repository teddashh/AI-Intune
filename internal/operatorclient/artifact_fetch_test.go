package operatorclient

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func artifactFetchClientTime() time.Time {
	return time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
}

func artifactFetchClientDigest(fill string) string {
	return "sha256:" + strings.Repeat(fill, 64)
}

func artifactFetchClientIntegrity() string {
	return "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x4a}, 64))
}

func artifactFetchClientOperation(state store.ArtifactFetchState) store.ArtifactFetchOperation {
	created := artifactFetchClientTime()
	operation := store.ArtifactFetchOperation{
		OperationID: "0123456789abcdef0123456789abcdef",
		Name:        "openclaw", Version: "2026.9.8", SourceKind: artifact.ArtifactSourceNPM,
		RegistryOrigin:  artifact.ProductionRegistryOrigin,
		SHA512Integrity: artifactFetchClientIntegrity(), EnginesNode: ">=22",
		IdentityDigest: artifactFetchClientDigest("b"), PreviewDigest: artifactFetchClientDigest("a"),
		MaxBytes: artifact.DefaultArtifactMaxBytes, State: store.ArtifactFetchQueued,
		Phase: store.ArtifactFetchPhaseQueued, CreatedAt: created, UpdatedAt: created,
	}
	switch state {
	case store.ArtifactFetchRunning:
		started := created.Add(time.Second)
		operation.State, operation.Phase = state, store.ArtifactFetchPhaseVerifying
		operation.ProgressBytes, operation.Attempt = 4096, 2
		operation.StartedAt, operation.UpdatedAt = &started, created.Add(2*time.Second)
	case store.ArtifactFetchSucceeded:
		started, finished := created.Add(time.Second), created.Add(3*time.Second)
		sha, size := strings.Repeat("c", 64), int64(4096)
		operation.State, operation.Phase = state, store.ArtifactFetchPhaseComplete
		operation.ProgressBytes, operation.Attempt = size, 1
		operation.ResultSHA256, operation.ResultSizeBytes = &sha, &size
		operation.StartedAt, operation.FinishedAt, operation.UpdatedAt = &started, &finished, finished
	case store.ArtifactFetchFailed:
		started, finished := created.Add(time.Second), created.Add(3*time.Second)
		code, detail := "DOWNLOAD_FAILED", "registry closed the response"
		operation.State, operation.Phase = state, store.ArtifactFetchPhaseDownloading
		operation.ProgressBytes, operation.Attempt = 128, 1
		operation.ErrorCode, operation.ErrorDetail = &code, &detail
		operation.StartedAt, operation.FinishedAt, operation.UpdatedAt = &started, &finished, finished
	}
	return operation
}

func artifactFetchClientPreview() operator.ArtifactFetchPreviewResult {
	engines := ">=22"
	return operator.ArtifactFetchPreviewResult{
		SchemaVersion: operator.ArtifactFetchPreviewSchemaVersion,
		PolicyVersion: artifact.FetchPolicyVersion, SourceKind: artifact.ArtifactSourceNPM,
		PreviewedAt: artifactFetchClientTime(),
		Name:        "openclaw", Version: "2026.9.8", RegistryOrigin: artifact.ProductionRegistryOrigin,
		SHA512Integrity: artifactFetchClientIntegrity(), EnginesNode: &engines,
		MaxBytes: artifact.DefaultArtifactMaxBytes, PreviewDigest: artifactFetchClientDigest("a"),
		EnqueueAllowed: true, Blockers: []string{},
	}
}

func artifactFetchClientHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
}

func artifactFetchClientApplyRequest() operator.ArtifactFetchApplyRequest {
	return operator.ArtifactFetchApplyRequest{
		Name: "openclaw", Version: "2026.9.8", PreviewDigest: artifactFetchClientDigest("a"),
		ConfirmName: "openclaw", ConfirmVersion: "2026.9.8", Reason: "fetch exact release",
	}
}

func TestArtifactFetchClientAcceptsNodeRuntimeSourceDTOs(t *testing.T) {
	preview := artifactFetchClientPreview()
	preview.Name = "node-runtime"
	preview.Version = "24.21.0"
	preview.SourceKind = artifact.ArtifactSourceNode
	preview.RegistryOrigin = artifact.ProductionNodeDistributionOrigin
	preview.PolicyVersion = artifact.NodeRuntimeFetchPolicyVersion
	preview.EnginesNode = nil
	if err := validateArtifactFetchPreviewResult(preview, operator.ArtifactFetchPreviewRequest{
		Name: "node-runtime", Version: "24.21.0",
	}); err != nil {
		t.Fatal(err)
	}

	operation := artifactFetchClientOperation(store.ArtifactFetchQueued)
	operation.Name = "node-runtime"
	operation.Version = "24.21.0"
	operation.SourceKind = artifact.ArtifactSourceNode
	operation.RegistryOrigin = artifact.ProductionNodeDistributionOrigin
	operation.EnginesNode = ""
	if err := validateArtifactFetchOperation(operation); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v24.21.0", "24.21.0-rc.1", "24.21"} {
		preview.Version = version
		if err := validateArtifactFetchPreviewResult(preview, operator.ArtifactFetchPreviewRequest{
			Name: "node-runtime", Version: version,
		}); err == nil {
			t.Fatalf("accepted node runtime version %q", version)
		}
	}
}

func TestArtifactFetchClientAcceptsHermesImageSourceDTOs(t *testing.T) {
	preview := artifactFetchClientPreview()
	preview.Name = "hermes-agent"
	preview.Version = "2026.9.7"
	preview.SourceKind = artifact.ArtifactSourceHermesImage
	preview.RegistryOrigin = artifact.ProductionHermesRegistryOrigin
	preview.PolicyVersion = artifact.HermesImageFetchPolicyVersion
	preview.EnginesNode = nil
	preview.MaxBytes = artifact.DefaultHermesImageBundleMaxBytes
	if err := validateArtifactFetchPreviewResult(preview, operator.ArtifactFetchPreviewRequest{
		Name: "hermes-agent", Version: "2026.9.7",
	}); err != nil {
		t.Fatal(err)
	}
	operation := artifactFetchClientOperation(store.ArtifactFetchQueued)
	operation.Name = "hermes-agent"
	operation.Version = "2026.9.7"
	operation.SourceKind = artifact.ArtifactSourceHermesImage
	operation.RegistryOrigin = artifact.ProductionHermesRegistryOrigin
	operation.EnginesNode = ""
	operation.MaxBytes = artifact.DefaultHermesImageBundleMaxBytes
	if err := validateArtifactFetchOperation(operation); err != nil {
		t.Fatal(err)
	}
	if err := validateArtifactFetchTarget("hermes-agent", "2026.9.7"); err != nil {
		t.Fatal(err)
	}
	if err := validateArtifactFetchTarget("claude-code", "2.1.278"); err != nil {
		t.Fatal(err)
	}
	if err := validateArtifactFetchTarget("claude-code", "latest"); err == nil {
		t.Fatal("accepted claude-code latest")
	}
	if err := validateArtifactFetchTarget("codex", "0.155.1"); err != nil {
		t.Fatal(err)
	}
	if err := validateArtifactFetchTarget("codex", "latest"); err == nil {
		t.Fatal("accepted codex latest")
	}
	preview.Name = "grok"
	preview.Version = "1.0.40"
	preview.SourceKind = artifact.ArtifactSourceGrok
	preview.RegistryOrigin = artifact.ProductionRegistryOrigin
	preview.PolicyVersion = artifact.GrokFetchPolicyVersion
	preview.EnginesNode = nil
	preview.MaxBytes = artifact.DefaultGrokBundleMaxBytes
	if err := validateArtifactFetchPreviewResult(preview, operator.ArtifactFetchPreviewRequest{
		Name: "grok", Version: "1.0.40",
	}); err != nil {
		t.Fatal(err)
	}
	operation.Name = "grok"
	operation.Version = "1.0.40"
	operation.SourceKind = artifact.ArtifactSourceGrok
	operation.RegistryOrigin = artifact.ProductionRegistryOrigin
	operation.EnginesNode = ""
	operation.MaxBytes = artifact.DefaultGrokBundleMaxBytes
	if err := validateArtifactFetchOperation(operation); err != nil {
		t.Fatal(err)
	}
	if err := validateArtifactFetchTarget("grok", "1.0.40"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"latest", "1.0", "1.0.40-alpha.1", "1.0.40-beta.1"} {
		if err := validateArtifactFetchTarget("grok", version); err == nil {
			t.Fatalf("accepted grok version %q", version)
		}
	}
	preview.Name = "bat-server"
	preview.Version = "3.2.10"
	preview.SourceKind = artifact.ArtifactSourceBATServer
	preview.RegistryOrigin = artifact.ProductionBATServerOrigin
	preview.PolicyVersion = artifact.BATServerFetchPolicyVersion
	preview.EnginesNode = nil
	preview.MaxBytes = artifact.DefaultBATServerBundleMaxBytes
	if err := validateArtifactFetchPreviewResult(preview, operator.ArtifactFetchPreviewRequest{
		Name: "bat-server", Version: "3.2.10",
	}); err != nil {
		t.Fatal(err)
	}
	operation.Name = "bat-server"
	operation.Version = "3.2.10"
	operation.SourceKind = artifact.ArtifactSourceBATServer
	operation.RegistryOrigin = artifact.ProductionBATServerOrigin
	operation.EnginesNode = ""
	operation.MaxBytes = artifact.DefaultBATServerBundleMaxBytes
	if err := validateArtifactFetchOperation(operation); err != nil {
		t.Fatal(err)
	}
	if err := validateArtifactFetchTarget("bat-server", "3.2.10"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"latest", "3.2", "v3.2.10", "3.2.11-pre.4"} {
		if err := validateArtifactFetchTarget("bat-server", version); err == nil {
			t.Fatalf("accepted bat-server version %q", version)
		}
	}
	for _, version := range []string{"v2026.9.7", "latest", "2026.9"} {
		if err := validateArtifactFetchTarget("hermes-agent", version); err == nil {
			t.Fatalf("accepted Hermes version %q", version)
		}
	}
}

func TestArtifactFetchClientAcceptsAntigravitySourceDTOs(t *testing.T) {
	preview := artifactFetchClientPreview()
	preview.Name = "antigravity"
	preview.Version = "1.2.14"
	preview.SourceKind = artifact.ArtifactSourceAntigravity
	preview.RegistryOrigin = artifact.ProductionAntigravityManifestOrigin
	preview.PolicyVersion = artifact.AntigravityFetchPolicyVersion
	preview.EnginesNode = nil
	preview.MaxBytes = artifact.DefaultAntigravityBundleMaxBytes
	if err := validateArtifactFetchPreviewResult(preview, operator.ArtifactFetchPreviewRequest{
		Name: "antigravity", Version: "1.2.14",
	}); err != nil {
		t.Fatal(err)
	}
	operation := artifactFetchClientOperation(store.ArtifactFetchQueued)
	operation.Name = "antigravity"
	operation.Version = "1.2.14"
	operation.SourceKind = artifact.ArtifactSourceAntigravity
	operation.RegistryOrigin = artifact.ProductionAntigravityManifestOrigin
	operation.EnginesNode = ""
	operation.MaxBytes = artifact.DefaultAntigravityBundleMaxBytes
	if err := validateArtifactFetchOperation(operation); err != nil {
		t.Fatal(err)
	}
	foreignPreview := preview
	foreignPreview.RegistryOrigin = artifact.ProductionRegistryOrigin
	if err := validateArtifactFetchPreviewResult(foreignPreview, operator.ArtifactFetchPreviewRequest{
		Name: "antigravity", Version: "1.2.14",
	}); err == nil {
		t.Fatal("accepted an Antigravity preview from the npm registry origin")
	}
	foreignOperation := operation
	foreignOperation.RegistryOrigin = artifact.ProductionRegistryOrigin
	if err := validateArtifactFetchOperation(foreignOperation); err == nil {
		t.Fatal("accepted an Antigravity operation from the npm registry origin")
	}
	if err := validateArtifactFetchTarget("antigravity", "1.2.14"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"latest", "1.2", "v1.2.14", "1.2.14-4571742832820224"} {
		if err := validateArtifactFetchTarget("antigravity", version); err == nil {
			t.Fatalf("accepted antigravity version %q", version)
		}
	}
}

func TestArtifactFetchClientCanonicalRoutesAndReplay(t *testing.T) {
	var creates atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactFetchClientHeaders(w)
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent || r.URL.RawQuery != "" && r.URL.Path != "/v1/operator/artifact-fetches" {
			t.Errorf("common request method=%s url=%s headers=%v", r.Method, r.URL, r.Header)
		}
		switch r.URL.Path {
		case "/v1/operator/artifact-fetches/preview":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
				r.Header.Get("Idempotency-Key") != "" {
				t.Errorf("preview request method=%s headers=%v", r.Method, r.Header)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 2 ||
				body["name"] != "openclaw" || body["version"] != "2026.9.8" {
				t.Errorf("preview body=%v err=%v", body, err)
			}
			_ = json.NewEncoder(w).Encode(artifactFetchClientPreview())
		case "/v1/operator/artifact-fetches":
			if r.Method == http.MethodPost {
				if r.Header.Get("Idempotency-Key") != "fetch-client-key" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("create headers=%v", r.Header)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(raw, &body); err != nil || len(body) != 6 ||
					body["actor"] != nil || body["idempotency_key"] != nil || body["tarball_url"] != nil {
					t.Errorf("create body=%s fields=%v err=%v", raw, body, err)
				}
				n := creates.Add(1)
				result := operator.ArtifactFetchApplyResult{Operation: artifactFetchClientOperation(store.ArtifactFetchQueued)}
				if n == 2 {
					result.Replayed = true
					result.Operation = artifactFetchClientOperation(store.ArtifactFetchRunning)
					w.Header().Set("Idempotency-Replayed", "true")
				}
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(result)
				return
			}
			if r.Method != http.MethodGet || r.Header.Get("Idempotency-Key") != "" ||
				r.URL.Query().Get("state") != "running" || r.URL.Query().Get("name") != "openclaw" ||
				r.URL.Query().Get("version") != "2026.9.8" || r.URL.Query().Get("limit") != "7" {
				t.Errorf("list method=%s url=%s headers=%v", r.Method, r.URL, r.Header)
			}
			_ = json.NewEncoder(w).Encode(store.ArtifactFetchListResult{
				SchemaVersion: store.ArtifactFetchReadSchemaVersion,
				Consistency:   store.ArtifactFetchReadConsistency, EvaluatedAt: artifactFetchClientTime().Add(time.Minute),
				Total: 1, Items: []store.ArtifactFetchOperation{artifactFetchClientOperation(store.ArtifactFetchRunning)},
			})
		case "/v1/operator/artifact-fetches/0123456789abcdef0123456789abcdef":
			if r.Method != http.MethodGet || r.Header.Get("Idempotency-Key") != "" {
				t.Errorf("detail method=%s headers=%v", r.Method, r.Header)
			}
			_ = json.NewEncoder(w).Encode(artifactFetchClientOperation(store.ArtifactFetchSucceeded))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)

	preview, err := client.PreviewArtifactFetch(t.Context(), operator.ArtifactFetchPreviewRequest{
		Name: "openclaw", Version: "2026.9.8",
	})
	if err != nil || preview.PreviewDigest != artifactFetchClientDigest("a") {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	apply := artifactFetchClientApplyRequest()
	apply.IdempotencyKey = "fetch-client-key"
	fresh, err := client.CreateArtifactFetch(t.Context(), "fetch-client-key", apply)
	if err != nil || fresh.Replayed || fresh.Operation.State != store.ArtifactFetchQueued {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
	replay, err := client.CreateArtifactFetch(t.Context(), "fetch-client-key", apply)
	if err != nil || !replay.Replayed || replay.Operation.OperationID != fresh.Operation.OperationID ||
		replay.Operation.State != store.ArtifactFetchRunning {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	list, err := client.ArtifactFetches(t.Context(), store.ArtifactFetchListRequest{
		State: store.ArtifactFetchRunning, Name: "openclaw", Version: "2026.9.8", Limit: 7,
	})
	if err != nil || list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	detail, err := client.ArtifactFetch(t.Context(), fresh.Operation.OperationID)
	if err != nil || detail.State != store.ArtifactFetchSucceeded || detail.OperationID != fresh.Operation.OperationID {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
}

func TestArtifactFetchClientRejectsInvalidRequestsBeforeHTTP(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	ctx := t.Context()

	for _, request := range []operator.ArtifactFetchPreviewRequest{
		{Name: "other", Version: "1.2.3"}, {Name: "openclaw", Version: "latest"},
		{Name: "openclaw", Version: "01.2.3"},
	} {
		if _, err := client.PreviewArtifactFetch(ctx, request); err == nil {
			t.Fatalf("accepted invalid preview request %+v", request)
		}
	}
	valid := artifactFetchClientApplyRequest()
	badRequests := []struct {
		key  string
		body operator.ArtifactFetchApplyRequest
	}{
		{"", valid},
		{" key", valid},
		{strings.Repeat("k", 201), valid},
		{"header-key", func() operator.ArtifactFetchApplyRequest { v := valid; v.IdempotencyKey = "body-key"; return v }()},
		{"key", func() operator.ArtifactFetchApplyRequest { v := valid; v.Actor.SourceAddr = "forged"; return v }()},
		{"key", func() operator.ArtifactFetchApplyRequest {
			v := valid
			v.PreviewDigest = artifactFetchClientDigest("A")
			return v
		}()},
		{"key", func() operator.ArtifactFetchApplyRequest { v := valid; v.ConfirmVersion = "1.2.4"; return v }()},
		{"key", func() operator.ArtifactFetchApplyRequest { v := valid; v.Reason = ""; return v }()},
		{"key", func() operator.ArtifactFetchApplyRequest { v := valid; v.Reason = "bad\nreason"; return v }()},
	}
	for _, test := range badRequests {
		if _, err := client.CreateArtifactFetch(ctx, test.key, test.body); err == nil {
			t.Fatalf("accepted invalid create key=%q body=%+v", test.key, test.body)
		}
	}
	for _, request := range []store.ArtifactFetchListRequest{
		{State: "unknown"}, {Name: "bad/name"}, {Version: " bad"},
		{Limit: -1}, {Limit: store.MaxArtifactFetchReadLimit + 1},
	} {
		if _, err := client.ArtifactFetches(ctx, request); err == nil {
			t.Fatalf("accepted invalid list request %+v", request)
		}
	}
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, " bad", "bad\n"} {
		if _, err := client.ArtifactFetch(ctx, id); err == nil {
			t.Fatalf("accepted unsafe operation id %q", id)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid requests reached HTTP server hits=%d", hits.Load())
	}
}

func TestArtifactFetchClientRejectsHeadersReplayContradictionsAndPrivateFields(t *testing.T) {
	canonical := artifactFetchClientOperation(store.ArtifactFetchQueued)
	raw, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		status int
		header http.Header
		body   string
	}{
		{"missing no-store", http.StatusOK, http.Header{"Content-Type": {"application/json"}}, string(raw)},
		{"wrong content type", http.StatusOK, http.Header{"Content-Type": {"text/plain"}, "Cache-Control": {"no-store"}}, string(raw)},
		{"etag", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "ETag": {`"unsafe"`}}, string(raw)},
		{"read replay", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "Idempotency-Replayed": {"true"}}, string(raw)},
		{"invalid replay header", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "Idempotency-Replayed": {"false"}}, string(raw)},
		{"unexpected success", http.StatusAccepted, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, string(raw)},
		{"tarball url", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, strings.TrimSuffix(string(raw), "}") + `,"tarball_url":"https://secret.invalid/token"}`},
		{"run token", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, strings.TrimSuffix(string(raw), "}") + `,"run_token":"secret"}`},
		{"duplicate field", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, strings.Replace(string(raw), `"state":"queued"`, `"state":"queued","state":"queued"`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for name, values := range test.header {
					for _, value := range values {
						w.Header().Add(name, value)
					}
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := operatorClientForServer(t, server)
			if result, err := client.ArtifactFetch(t.Context(), canonical.OperationID); err == nil {
				t.Fatalf("accepted result=%+v", result)
			}
		})
	}

	for _, test := range []struct {
		name         string
		status       int
		headerReplay bool
		bodyReplay   bool
		state        store.ArtifactFetchState
	}{
		{"fresh body says replay", http.StatusAccepted, false, true, store.ArtifactFetchQueued},
		{"header replay body fresh", http.StatusAccepted, true, false, store.ArtifactFetchQueued},
		{"fresh is already running", http.StatusAccepted, false, false, store.ArtifactFetchRunning},
		{"wrong accepted status", http.StatusOK, false, false, store.ArtifactFetchQueued},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				artifactFetchClientHeaders(w)
				if test.headerReplay {
					w.Header().Set("Idempotency-Replayed", "true")
				}
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(operator.ArtifactFetchApplyResult{
					Operation: artifactFetchClientOperation(test.state), Replayed: test.bodyReplay,
				})
			}))
			defer server.Close()
			client := operatorClientForServer(t, server)
			if result, err := client.CreateArtifactFetch(t.Context(), "key", artifactFetchClientApplyRequest()); err == nil {
				t.Fatalf("accepted result=%+v", result)
			}
		})
	}
}

func TestArtifactFetchClientRejectsOperationInvariantViolations(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*store.ArtifactFetchOperation)
	}{
		{"queued progress", func(v *store.ArtifactFetchOperation) { v.ProgressBytes = 1 }},
		{"state phase mismatch", func(v *store.ArtifactFetchOperation) { v.State = store.ArtifactFetchRunning }},
		{"uppercase identity", func(v *store.ArtifactFetchOperation) { v.IdentityDigest = artifactFetchClientDigest("A") }},
		{"bad sri", func(v *store.ArtifactFetchOperation) { v.SHA512Integrity = "sha512-not-base64" }},
		{"non-production origin", func(v *store.ArtifactFetchOperation) { v.RegistryOrigin = "http://registry.invalid" }},
		{"subsecond timestamp", func(v *store.ArtifactFetchOperation) { v.CreatedAt = v.CreatedAt.Add(time.Nanosecond) }},
		{"offset timestamp", func(v *store.ArtifactFetchOperation) { v.CreatedAt = v.CreatedAt.In(time.FixedZone("offset", 3600)) }},
		{"success size mismatch", func(v *store.ArtifactFetchOperation) {
			*v.ResultSizeBytes++
		}},
		{"failure code invalid", func(v *store.ArtifactFetchOperation) { *v.ErrorCode = "raw-error" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := store.ArtifactFetchQueued
			if strings.HasPrefix(test.name, "success") {
				state = store.ArtifactFetchSucceeded
			} else if strings.HasPrefix(test.name, "failure") {
				state = store.ArtifactFetchFailed
			}
			operation := artifactFetchClientOperation(state)
			test.mutate(&operation)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				artifactFetchClientHeaders(w)
				_ = json.NewEncoder(w).Encode(operation)
			}))
			defer server.Close()
			client := operatorClientForServer(t, server)
			if result, err := client.ArtifactFetch(t.Context(), operation.OperationID); err == nil {
				t.Fatalf("accepted result=%+v", result)
			}
		})
	}
}

func TestArtifactFetchClientPreservesReplayedAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"code":"ARTIFACT_FETCH_ACTIVE","message":"already active"}`)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	_, err := client.CreateArtifactFetch(t.Context(), "key", artifactFetchClientApplyRequest())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict ||
		apiErr.Code != "ARTIFACT_FETCH_ACTIVE" || !apiErr.Replayed {
		t.Fatalf("api error=%T %+v", err, apiErr)
	}
}
