package web

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func installWebArtifact(t *testing.T, s *Server) (digest, sourceSentinel, actorSentinel, dir string) {
	t.Helper()
	body := []byte("web artifact canonical bytes")
	sha256Sum, sha512Sum := sha256.Sum256(body), sha512.Sum512(body)
	digest = hex.EncodeToString(sha256Sum[:])
	dir = filepath.Join(t.TempDir(), "artifact-catalog-private-location")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceSentinel = "https://private-source.invalid/openclaw/secret-download-location.tgz"
	actorSentinel = "private-fetch-actor-sentinel"
	record := artifact.Sidecar{
		Name: "openclaw", Version: "2026.9.8", TarballURL: sourceSentinel,
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sha512Sum[:]),
		SHA256:          digest, Size: int64(len(body)), EnginesNode: ">=24",
		FetchedAt: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC), FetchedBy: actorSentinel,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	s.SetArtifactsDir(dir)
	return digest, sourceSentinel, actorSentinel, dir
}

func TestAppsViewsAreStrictAndArtifactDetailRevalidatesWithoutPrivateProvenance(t *testing.T) {
	s, _ := newServer(t)
	digest, source, fetchedBy, dir := installWebArtifact(t, s)

	for _, path := range []string{
		"/apps?", "/apps?view=", "/apps?view=overview&view=artifacts",
		"/apps?view=unknown", "/apps?unknown=value", "/apps?section=openclaw",
	} {
		if rec := doGet(t, s, path); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status=%d, want 400", path, rec.Code)
		}
	}
	overview := get(t, s, "/apps")
	if !strings.Contains(overview, `href="/apps?view=openclaw"><span class="metric-label">機器觀測到的版本數`) ||
		!strings.Contains(overview, "去重後的機器版本") {
		t.Fatalf("observed-version metric did not preserve the machine-observation boundary: %s", overview)
	}

	list := get(t, s, "/apps?view=artifacts")
	for _, want := range []string{"Artifacts", "2026.9.8", "available_unverified", digest[:12], "Detail verification：SHA-256"} {
		if !strings.Contains(list, want) {
			t.Errorf("artifact list missing %q", want)
		}
	}
	for _, forbidden := range []string{source, fetchedBy, dir, "secret-download-location"} {
		if strings.Contains(list, forbidden) {
			t.Errorf("artifact list disclosed %q", forbidden)
		}
	}

	detail := get(t, s, "/apps/artifacts/"+digest)
	for _, want := range []string{"Artifact evidence", "ready", digest, "Verified in this request"} {
		if !strings.Contains(detail, want) {
			t.Errorf("artifact detail missing %q", want)
		}
	}
	for _, forbidden := range []string{source, fetchedBy, dir, "secret-download-location"} {
		if strings.Contains(detail, forbidden) {
			t.Errorf("artifact detail disclosed %q", forbidden)
		}
	}
	if rec := doGet(t, s, "/apps/artifacts/"+digest+"?debug=1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("artifact detail with query status=%d, want 400", rec.Code)
	}
}

func TestArtifactFetchOperationPagesUseNarrowSafeProjection(t *testing.T) {
	s, st := newServer(t)
	requestKey := "private-idempotency-key-sentinel"
	source := "https://private-source.invalid/openclaw/signed-secret.tgz"
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512.Size))
	previewDigest := "sha256:" + strings.Repeat("a", 64)
	requestDigest := "sha256:" + strings.Repeat("b", 64)
	created, err := st.ApplyOperatorArtifactFetch(store.OperatorArtifactFetchRequest{
		Name: "openclaw", Version: "2026.9.8", PreviewDigest: previewDigest,
		Reason: "test intake", IdempotencyKey: requestKey, RequestDigest: requestDigest,
		Audit: store.AuditEntry{AuthSubject: "private-operator-subject"},
	}, func() (store.ArtifactFetchPrepared, error) {
		return store.ArtifactFetchPrepared{
			Name: "openclaw", Version: "2026.9.8", RegistryOrigin: "https://private-source.invalid",
			TarballURL: source, SHA512Integrity: integrity, EnginesNode: ">=24",
			MaxBytes: 1 << 20, CurrentPreviewDigest: previewDigest,
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := st.ClaimArtifactFetchOperation(created.Operation.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	errorDetail := "private-worker-error-detail-with-secret-location"
	failed, err := st.FailArtifactFetchOperation(created.Operation.OperationID, claim.RunToken,
		"ARTIFACT_FETCH_TEST_FAILURE", errorDetail)
	if err != nil {
		t.Fatal(err)
	}
	created.Operation = failed

	for _, path := range []string{
		"/apps?view=operations", "/apps/artifact-fetches/" + created.Operation.OperationID,
	} {
		body := get(t, s, path)
		for _, want := range []string{created.Operation.OperationID[:12], "openclaw@2026.9.8", "failed", "ARTIFACT_FETCH_TEST_FAILURE"} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s missing %q", path, want)
			}
		}
		for _, forbidden := range []string{source, requestKey, integrity, previewDigest, requestDigest,
			"private-operator-subject", "private-source.invalid", claim.RunToken, errorDetail} {
			if strings.Contains(body, forbidden) {
				t.Errorf("GET %s disclosed %q", path, forbidden)
			}
		}
	}
}

type fakeArtifactWebOperator struct {
	preview       operator.ArtifactFetchPreviewResult
	previewErr    error
	previewCalls  int
	applyCalls    []operator.ArtifactFetchApplyRequest
	operationID   string
	successfulKey string
}

func (*fakeArtifactWebOperator) ListArtifacts(operator.ArtifactListRequest, time.Time) (operator.ArtifactListResult, error) {
	return operator.ArtifactListResult{}, errors.New("unexpected ListArtifacts")
}

func (*fakeArtifactWebOperator) ArtifactDetail(context.Context, string, time.Time) (operator.ArtifactDetailResult, error) {
	return operator.ArtifactDetailResult{}, errors.New("unexpected ArtifactDetail")
}

func (f *fakeArtifactWebOperator) PreviewArtifactFetch(_ context.Context, request operator.ArtifactFetchPreviewRequest) (operator.ArtifactFetchPreviewResult, error) {
	f.previewCalls++
	return f.preview, f.previewErr
}

func (f *fakeArtifactWebOperator) ApplyArtifactFetch(_ context.Context, request operator.ArtifactFetchApplyRequest) (operator.ArtifactFetchApplyResult, error) {
	f.applyCalls = append(f.applyCalls, request)
	if request.ConfirmName != request.Name || request.ConfirmVersion != request.Version {
		return operator.ArtifactFetchApplyResult{}, &store.OperatorRequestError{
			Code: store.OperatorCodeArtifactFetchInvalid, Detail: store.OperatorCodeArtifactFetchInvalid,
		}
	}
	replayed := f.successfulKey == request.IdempotencyKey
	if f.successfulKey == "" {
		f.successfulKey = request.IdempotencyKey
	}
	return operator.ArtifactFetchApplyResult{
		Operation: store.ArtifactFetchOperation{
			OperationID: f.operationID, Name: request.Name, Version: request.Version,
			State: store.ArtifactFetchQueued, Phase: store.ArtifactFetchPhaseQueued,
		},
		Replayed: replayed,
	}, nil
}

func (*fakeArtifactWebOperator) ArtifactFetchOperation(string) (store.ArtifactFetchOperation, error) {
	return store.ArtifactFetchOperation{}, errors.New("unexpected ArtifactFetchOperation")
}

func (*fakeArtifactWebOperator) ArtifactFetchOperations(store.ArtifactFetchListRequest) (store.ArtifactFetchListResult, error) {
	return store.ArtifactFetchListResult{}, errors.New("unexpected ArtifactFetchOperations")
}

func TestArtifactFetchFormsAreAdminOnlyTypedStrictAndReuseOneRequestIdentity(t *testing.T) {
	s, _ := newServer(t)
	fake := &fakeArtifactWebOperator{
		operationID: "artifact-fetch-operation-123",
		preview: operator.ArtifactFetchPreviewResult{
			SchemaVersion: operator.ArtifactFetchPreviewSchemaVersion,
			PolicyVersion: artifact.FetchPolicyVersion, SourceKind: artifact.ArtifactSourceNPM,
			PreviewedAt: time.Now().UTC(),
			Name:        "openclaw", Version: "2026.9.8",
			RegistryOrigin:  artifact.ProductionRegistryOrigin,
			SHA512Integrity: "private-upstream-integrity-sentinel",
			MaxBytes:        artifact.DefaultArtifactMaxBytes,
			PreviewDigest:   "sha256:" + strings.Repeat("c", 64), EnqueueAllowed: true,
			Blockers: []string{},
		},
	}
	s.artifactOperator = fake
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	viewOnly := renderWithCapabilities(t, s, "/apps?view=fetch", operatorauth.CapabilityNames{View: names.View})
	if strings.Contains(viewOnly, `action="/apps/artifact-fetches/preview"`) {
		t.Fatal("view-only operator saw fetch form")
	}
	admin := renderWithCapabilities(t, s, "/apps?view=fetch", names)
	if !strings.Contains(admin, `method="post" action="/apps/artifact-fetches/preview"`) {
		t.Fatal("admin fetch view missing POST preview form")
	}

	preview := postForm(t, s, "/apps/artifact-fetches/preview", url.Values{
		"name": {"openclaw"}, "version": {"2026.9.8"}, "reason": {" scheduled intake "},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "輸入 package 與 exact version 以確認") ||
		!strings.Contains(preview.Body.String(), `name="confirm_name"`) ||
		!strings.Contains(preview.Body.String(), `name="confirm_version"`) {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	for _, forbidden := range []string{fake.preview.RegistryOrigin, fake.preview.SHA512Integrity} {
		if strings.Contains(preview.Body.String(), forbidden) {
			t.Errorf("preview disclosed %q", forbidden)
		}
	}
	form := url.Values{
		"name":            {"openclaw"},
		"version":         {"2026.9.8"},
		"preview_digest":  {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"reason":          {hiddenFormValue(t, preview.Body.String(), "reason")},
		"idempotency_key": {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
		"confirm_name":    {"openclaw"},
		"confirm_version": {"wrong"},
	}
	bad := postForm(t, s, "/apps/artifact-fetches", form)
	if bad.Code != http.StatusBadRequest || len(fake.applyCalls) != 1 {
		t.Fatalf("wrong typed confirmation status=%d apply_calls=%d", bad.Code, len(fake.applyCalls))
	}
	preview = postForm(t, s, "/apps/artifact-fetches/preview", url.Values{
		"name": {"openclaw"}, "version": {"2026.9.8"}, "reason": {" scheduled intake "},
	})
	form.Set("preview_digest", hiddenFormValue(t, preview.Body.String(), "preview_digest"))
	form.Set("reason", hiddenFormValue(t, preview.Body.String(), "reason"))
	form.Set("idempotency_key", hiddenFormValue(t, preview.Body.String(), "idempotency_key"))
	form.Set("confirm_version", "2026.9.8")
	first := postForm(t, s, "/apps/artifact-fetches", form)
	second := postForm(t, s, "/apps/artifact-fetches", form)
	for index, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/apps/artifact-fetches/"+fake.operationID {
			t.Errorf("apply %d status=%d location=%q", index+1, rec.Code, rec.Header().Get("Location"))
		}
	}
	if second.Header().Get("Idempotency-Replayed") != "true" || len(fake.applyCalls) != 3 ||
		fake.applyCalls[1].IdempotencyKey == "" || fake.applyCalls[1].IdempotencyKey != fake.applyCalls[2].IdempotencyKey ||
		fake.applyCalls[0].IdempotencyKey == fake.applyCalls[1].IdempotencyKey ||
		fake.applyCalls[1].Reason != "scheduled intake" || fake.applyCalls[1].Actor.SourceKind != operator.SourceKindWeb {
		t.Fatalf("apply calls did not preserve one request identity/provenance: %+v", fake.applyCalls)
	}
	if fake.previewCalls != 2 {
		t.Fatalf("preview calls=%d, want 2", fake.previewCalls)
	}
}

func TestArtifactFetchWebNodeRuntimePreviewAndApply(t *testing.T) {
	s, _ := newServer(t)
	fake := &fakeArtifactWebOperator{
		operationID: "node-runtime-fetch-operation-123",
		preview: operator.ArtifactFetchPreviewResult{
			SchemaVersion:   operator.ArtifactFetchPreviewSchemaVersion,
			PolicyVersion:   artifact.NodeRuntimeFetchPolicyVersion,
			SourceKind:      artifact.ArtifactSourceNode,
			PreviewedAt:     time.Now().UTC(),
			Name:            "node-runtime",
			Version:         "24.21.0",
			RegistryOrigin:  artifact.ProductionNodeDistributionOrigin,
			SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512.Size)),
			MaxBytes:        artifact.DefaultArtifactMaxBytes,
			PreviewDigest:   "sha256:" + strings.Repeat("d", 64),
			EnqueueAllowed:  true,
			Blockers:        []string{},
		},
	}
	s.artifactOperator = fake
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	page := renderWithCapabilities(t, s, "/apps?view=fetch", names)
	for _, want := range []string{`<select name="name" required>`, `value="openclaw"`, `value="hermes-agent"`, `value="node-runtime"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("fetch page missing %q: %s", want, page)
		}
	}

	preview := postForm(t, s, "/apps/artifact-fetches/preview", url.Values{
		"name": {"node-runtime"}, "version": {"24.21.0"}, "reason": {"managed runtime"},
	})
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	for _, want := range []string{"node-runtime@24.21.0", artifact.ArtifactSourceNode, artifact.NodeRuntimeFetchPolicyVersion} {
		if !strings.Contains(body, want) {
			t.Fatalf("node preview missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "Node engines") || strings.Contains(body, artifact.ProductionNodeDistributionOrigin) {
		t.Fatalf("node preview exposed an inapplicable or private field: %s", body)
	}

	form := url.Values{
		"name":            {"node-runtime"},
		"version":         {"24.21.0"},
		"preview_digest":  {hiddenFormValue(t, body, "preview_digest")},
		"reason":          {hiddenFormValue(t, body, "reason")},
		"idempotency_key": {hiddenFormValue(t, body, "idempotency_key")},
		"confirm_name":    {"node-runtime"},
		"confirm_version": {"24.21.0"},
	}
	created := postForm(t, s, "/apps/artifact-fetches", form)
	if created.Code != http.StatusSeeOther ||
		created.Header().Get("Location") != "/apps/artifact-fetches/"+fake.operationID || len(fake.applyCalls) != 1 {
		t.Fatalf("apply status=%d location=%q calls=%d body=%s", created.Code,
			created.Header().Get("Location"), len(fake.applyCalls), created.Body.String())
	}
	if got := fake.applyCalls[0]; got.Name != "node-runtime" || got.Version != "24.21.0" ||
		got.ConfirmName != got.Name || got.ConfirmVersion != got.Version {
		t.Fatalf("node apply=%+v", got)
	}
}

func TestArtifactFetchPOSTRejectsViewOnlyAndAmbiguousFormBeforeCanonicalCall(t *testing.T) {
	s, _ := newServer(t)
	fake := &fakeArtifactWebOperator{}
	s.artifactOperator = fake
	names, _ := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	mux := http.NewServeMux()
	s.Routes(mux)

	req := requestWithCapabilities(http.MethodPost, "/apps/artifact-fetches/preview", operatorauth.CapabilityNames{View: names.View})
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || fake.previewCalls != 0 {
		t.Fatalf("view-only preview status=%d calls=%d", rec.Code, fake.previewCalls)
	}

	duplicate := url.Values{"name": {"openclaw", "openclaw"}, "version": {"2026.9.8"}}
	rec = postForm(t, s, "/apps/artifact-fetches/preview", duplicate)
	if rec.Code != http.StatusBadRequest || fake.previewCalls != 0 {
		t.Fatalf("ambiguous preview status=%d calls=%d", rec.Code, fake.previewCalls)
	}
	for _, form := range []url.Values{
		{"name": {"openclaw"}, "version": {"2026.9.8"}},
		{"name": {"openclaw"}, "version": {"2026.9.8"}, "reason": {""}},
		{"name": {"openclaw"}, "version": {"2026.9.8"}, "reason": {"bad\nreason"}},
	} {
		rec = postForm(t, s, "/apps/artifact-fetches/preview", form)
		if rec.Code != http.StatusBadRequest || fake.previewCalls != 0 {
			t.Fatalf("invalid reason preview status=%d calls=%d", rec.Code, fake.previewCalls)
		}
	}
}
