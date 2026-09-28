package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func TestOperatorArtifactListAndDetailAreSafeNoStoreReads(t *testing.T) {
	f := newJobsFixture(t, "artifact-read")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	record := writeJobTestArtifact(t, f.artifactsDir, "2026.9.8", "operator-artifact-read")

	listResponse := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/artifacts?status=available_unverified&version=2026.9.8&limit=7", "", "")
	if listResponse.Code != http.StatusOK || listResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list status=%d headers=%v body=%s", listResponse.Code, listResponse.Header(), listResponse.Body.String())
	}
	for _, forbidden := range []string{record.TarballURL, record.FetchedBy, "tarball_url", "fetched_by", f.artifactsDir} {
		if forbidden != "" && strings.Contains(listResponse.Body.String(), forbidden) {
			t.Fatalf("safe artifact list disclosed %q: %s", forbidden, listResponse.Body.String())
		}
	}
	var list operator.ArtifactListResult
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.SchemaVersion != operator.ArtifactReadSchemaVersion ||
		list.Consistency != operator.ArtifactReadConsistencyLive || list.Total != 1 || len(list.Items) != 1 ||
		list.Items[0].ArtifactID != record.SHA256 || list.Items[0].Status != operator.ArtifactAvailableUnverified ||
		list.Items[0].VerifiedAt != nil || list.NextCursor != nil {
		t.Fatalf("list=%+v", list)
	}

	detailResponse := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/artifacts/"+record.SHA256, "", "")
	if detailResponse.Code != http.StatusOK || detailResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("detail status=%d headers=%v body=%s", detailResponse.Code, detailResponse.Header(), detailResponse.Body.String())
	}
	var detail operator.ArtifactDetailResult
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Item.ArtifactID != record.SHA256 || detail.Item.Status != operator.ArtifactReady ||
		detail.Item.VerifiedAt == nil || !detail.Item.VerifiedAt.Equal(detail.EvaluatedAt) {
		t.Fatalf("detail=%+v", detail)
	}
}

func TestOperatorArtifactDetailPropagatesRequestCancellation(t *testing.T) {
	f := newJobsFixture(t, "artifact-read-canceled")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	record := writeJobTestArtifact(t, f.artifactsDir, "2026.9.8", "operator-artifact-read-canceled")

	req := httptestRequest(http.MethodGet, "/v1/operator/artifacts/"+record.SHA256)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req.WithContext(ctx))
	assertAPIError(t, rec, http.StatusInternalServerError, "INTERNAL")
}

func TestOperatorArtifactCatalogIsolatesMalformedAndOrphanEntries(t *testing.T) {
	f := newJobsFixture(t, "artifact-catalog-errors")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	if err := os.MkdirAll(f.artifactsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	malformedDigest := strings.Repeat("b", 64)
	orphanDigest := strings.Repeat("c", 64)
	if err := os.WriteFile(filepath.Join(f.artifactsDir, malformedDigest+".json"), []byte(`{"secret":"must-not-escape"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.artifactsDir, orphanDigest+".tgz"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/artifacts", "", "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "must-not-escape") {
		t.Fatalf("catalog status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result operator.ArtifactListResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || len(result.Items) != 2 || result.Items[0].Status != operator.ArtifactInvalid ||
		result.Items[0].Issue == nil || *result.Items[0].Issue != operator.ArtifactReadIssue(artifact.CatalogIssueSidecarInvalidSchema) ||
		result.Items[1].Status != operator.ArtifactUnavailable || result.Items[1].Issue == nil ||
		*result.Items[1].Issue != operator.ArtifactReadIssue(artifact.CatalogIssueSidecarMissing) {
		t.Fatalf("catalog=%+v", result)
	}
}

func TestOperatorArtifactRoutesRejectAmbiguousQueriesAndInvalidIdentity(t *testing.T) {
	for _, query := range []string{
		"?", "?unknown=value", "?status=invalid&status=unavailable", "?status=ready",
		"?version=%20bad", "?limit=0", "?limit=101", "?limit=01x", "?cursor=%20bad",
	} {
		t.Run(query, func(t *testing.T) {
			request := httptestRequest(http.MethodGet, "/v1/operator/artifacts"+query)
			if _, err := parseOperatorArtifactListRequest(request); err == nil {
				t.Fatalf("query %q was accepted", query)
			}
		})
	}

	f := newJobsFixture(t, "artifact-invalid-id")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	query := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/artifacts/"+strings.Repeat("a", 64)+"?raw=true", "", "")
	assertAPIError(t, query, http.StatusBadRequest, "BAD_REQUEST")
	invalid := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/artifacts/"+strings.Repeat("A", 64), "", "")
	assertAPIError(t, invalid, http.StatusBadRequest, "BAD_REQUEST")
	missing := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/artifacts/"+strings.Repeat("a", 64), "", "")
	assertAPIError(t, missing, http.StatusNotFound, "ARTIFACT_NOT_FOUND")
}

func httptestRequest(method, target string) *http.Request {
	req, _ := http.NewRequest(method, target, nil)
	return req
}
