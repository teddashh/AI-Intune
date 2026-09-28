package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func seedOperatorArtifactFetch(t *testing.T, st *store.Store) store.ArtifactFetchOperation {
	t.Helper()
	previewDigest := "sha256:" + strings.Repeat("a", 64)
	result, err := st.ApplyOperatorArtifactFetch(store.OperatorArtifactFetchRequest{
		Name: "openclaw", Version: "2026.9.8", PreviewDigest: previewDigest,
		Reason: "api read fixture", IdempotencyKey: "artifact-fetch-api-fixture",
		RequestDigest: "sha256:" + strings.Repeat("b", 64),
		Audit:         store.AuditEntry{SourceAddr: "100.64.0.1", SourceKind: "test"},
	}, func() (store.ArtifactFetchPrepared, error) {
		return store.ArtifactFetchPrepared{
			Name: "openclaw", Version: "2026.9.8", SourceKind: artifact.ArtifactSourceNPM,
			RegistryOrigin:  artifact.ProductionRegistryOrigin,
			TarballURL:      artifact.ProductionRegistryOrigin + "/openclaw/-/private-upstream-path.tgz",
			SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64)),
			EnginesNode:     ">=24.15.0 <25", MaxBytes: artifact.DefaultArtifactMaxBytes,
			CurrentPreviewDigest: previewDigest,
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Operation
}

func TestOperatorArtifactFetchListAndDetailAreSafeNoStoreReads(t *testing.T) {
	f := newJobsFixture(t, "artifact-fetch-read")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	operation := seedOperatorArtifactFetch(t, f.store)

	list := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/artifact-fetches?state=queued&name=openclaw&version=2026.9.8&limit=7", "", "")
	if list.Code != http.StatusOK || list.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list status=%d headers=%v body=%s", list.Code, list.Header(), list.Body.String())
	}
	for _, forbidden := range []string{"private-upstream-path", "tarball_url", "run_token", "idempotency_key", "api read fixture"} {
		if strings.Contains(list.Body.String(), forbidden) {
			t.Fatalf("safe operation list disclosed %q: %s", forbidden, list.Body.String())
		}
	}
	var listed store.ArtifactFetchListResult
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || listed.Total != 1 ||
		len(listed.Items) != 1 || listed.Items[0].OperationID != operation.OperationID {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}

	detail := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/artifact-fetches/"+operation.OperationID, "", "")
	if detail.Code != http.StatusOK || detail.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("detail status=%d headers=%v body=%s", detail.Code, detail.Header(), detail.Body.String())
	}
	var got store.ArtifactFetchOperation
	if err := json.Unmarshal(detail.Body.Bytes(), &got); err != nil || got.OperationID != operation.OperationID ||
		got.State != store.ArtifactFetchQueued || got.Phase != store.ArtifactFetchPhaseQueued {
		t.Fatalf("detail=%+v err=%v", got, err)
	}

	badQuery := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/artifact-fetches/"+operation.OperationID+"?raw=true", "", "")
	assertAPIError(t, badQuery, http.StatusBadRequest, "BAD_REQUEST")
	missing := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/artifact-fetches/missing", "", "")
	assertAPIError(t, missing, http.StatusNotFound, "ARTIFACT_FETCH_NOT_FOUND")
}

func TestOperatorArtifactFetchListRejectsAmbiguousQueries(t *testing.T) {
	for _, query := range []string{
		"?", "?unknown=x", "?state=queued&state=failed", "?state=pending",
		"?name=%20openclaw", "?version=", "?limit=0", "?limit=101", "?limit=01x",
	} {
		t.Run(query, func(t *testing.T) {
			request := httptestRequest(http.MethodGet, "/v1/operator/artifact-fetches"+query)
			if _, err := parseOperatorArtifactFetchListRequest(request); err == nil {
				t.Fatalf("query %q was accepted", query)
			}
		})
	}
}

func TestOperatorArtifactFetchMutationTransportIsStrictAndDoesNotConsumeKey(t *testing.T) {
	f := newJobsFixture(t, "artifact-fetch-transport")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	for _, test := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"wrong media", "text/plain", `{}`, http.StatusUnsupportedMediaType},
		{"missing", "application/json", `{"name":"openclaw"}`, http.StatusBadRequest},
		{"unknown", "application/json", `{"name":"openclaw","version":"2026.9.8","preview_digest":"sha256:x","confirm_name":"openclaw","confirm_version":"2026.9.8","reason":"x","tarball_url":"private"}`, http.StatusBadRequest},
		{"duplicate", "application/json", `{"name":"openclaw","name":"other","version":"2026.9.8","preview_digest":"sha256:x","confirm_name":"openclaw","confirm_version":"2026.9.8","reason":"x"}`, http.StatusBadRequest},
		{"null", "application/json", `{"name":"openclaw","version":null,"preview_digest":"sha256:x","confirm_name":"openclaw","confirm_version":"2026.9.8","reason":"x"}`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := operatorRequestWithContentType(t, f.mux, http.MethodPost,
				"/v1/operator/artifact-fetches", "artifact-fetch-transport-"+test.name, test.contentType, test.body)
			if rec.Code != test.status || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
		})
	}
	var receipts int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key LIKE 'artifact-fetch-transport-%'`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("transport receipts=%d err=%v", receipts, err)
	}
}

func TestOperatorArtifactFetchRejectsEmptyReasonDurablyBeforeRegistryIO(t *testing.T) {
	f := newJobsFixture(t, "artifact-fetch-empty-reason")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	body := `{"name":"openclaw","version":"2026.9.8","preview_digest":"sha256:` +
		strings.Repeat("a", 64) + `","confirm_name":"openclaw","confirm_version":"2026.9.8","reason":""}`
	key := "artifact-fetch-empty-reason"
	for attempt := 0; attempt < 2; attempt++ {
		rec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/artifact-fetches", key, body)
		assertAPIError(t, rec, http.StatusBadRequest, store.OperatorCodeArtifactFetchInvalid)
		if (rec.Header().Get("Idempotency-Replayed") == "true") != (attempt == 1) {
			t.Fatalf("attempt=%d replay header=%q", attempt, rec.Header().Get("Idempotency-Replayed"))
		}
	}
	var operations, receipts, audits int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM artifact_fetch_operations`:                                                &operations,
		`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key='artifact-fetch-empty-reason'`: &receipts,
		`SELECT COUNT(*) FROM audit_log WHERE idempotency_key='artifact-fetch-empty-reason'`:            &audits,
	} {
		if err := f.store.DB().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if operations != 0 || receipts != 1 || audits != 2 {
		t.Fatalf("operations=%d receipts=%d audits=%d", operations, receipts, audits)
	}
}

func operatorRequestWithContentType(t *testing.T, mux *http.ServeMux, method, path, key, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Idempotency-Key", key)
	req = verifiedOperatorRequest(req, operatorauth.Admin)
	mux.ServeHTTP(rec, req)
	return rec
}
