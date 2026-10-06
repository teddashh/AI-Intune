package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func TestAntigravityPreviewAPIAndCLINamePublishedVersion(t *testing.T) {
	const sentence = "上游目前只提供 Antigravity 1.2.15；改用這個版號重新 Preview。"
	recorder := httptest.NewRecorder()
	writeOperatorArtifactFetchPreviewError(recorder, &artifact.UpstreamVersionError{
		Name: "antigravity", Requested: "1.2.13", Published: "1.2.15",
	})
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), sentence) ||
		!strings.Contains(recorder.Body.String(), "UPSTREAM_VERSION_UNAVAILABLE") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	hidden := httptest.NewRecorder()
	writeOperatorArtifactFetchPreviewError(hidden, &artifact.UpstreamVersionError{Name: "antigravity", Published: "9.8.7-not-a-version"})
	if hidden.Code != http.StatusBadGateway || strings.Contains(hidden.Body.String(), "9.8.7-not-a-version") {
		t.Fatalf("invalid published status=%d body=%s", hidden.Code, hidden.Body.String())
	}
	metadata := httptest.NewRecorder()
	writeOperatorArtifactFetchPreviewError(metadata, artifact.ErrMetadataInvalid)
	var apiErr model.APIError
	if err := json.Unmarshal(metadata.Body.Bytes(), &apiErr); err != nil || apiErr.Code != "REGISTRY_RESPONSE_REJECTED" ||
		strings.Contains(apiErr.Message, "1.2.15") {
		t.Fatalf("metadata=%+v err=%v body=%s", apiErr, err, metadata.Body.String())
	}

	name, version, err := parseArtifactFetchCLITarget("antigravity@1.2.14")
	if err != nil || name != "antigravity" || version != "1.2.14" {
		t.Fatalf("parse name=%s version=%s err=%v", name, version, err)
	}
	if _, _, err := parseArtifactFetchCLITarget("antigravity@1.2.14-4571742832820224"); err == nil {
		t.Fatal("accepted a build id as a version")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactFetchCLIResponseHeaders(w)
		if r.URL.Path != "/v1/operator/artifact-fetches/preview" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(model.APIError{Code: "UPSTREAM_VERSION_UNAVAILABLE", Message: sentence})
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	var out, errOut bytes.Buffer
	err = runArtifactFetchCommandWithDeps(t.Context(), []string{
		"antigravity@1.2.13", "--confirm-version", "1.2.13", "--reason", "fetch published CLI",
		"--idempotency-key", "antigravity-preview-moved",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), sentence) || !strings.Contains(err.Error(), "1.2.15") {
		t.Fatalf("err=%v stdout=%s stderr=%s", err, out.String(), errOut.String())
	}
}

func TestAntigravityDirectPreviewNamesPublishedVersion(t *testing.T) {
	const sentence = "上游目前只提供 Antigravity 1.2.15；改用這個版號重新 Preview。"
	backend := artifactFetchCLIBackend{
		source: "direct DB operator service",
		preview: func(operator.ArtifactFetchPreviewRequest) (operator.ArtifactFetchPreviewResult, error) {
			return operator.ArtifactFetchPreviewResult{}, &artifact.UpstreamVersionError{
				Name: "antigravity", Requested: "1.2.13", Published: "1.2.15",
			}
		},
	}
	var out, errOut bytes.Buffer
	err := executeArtifactFetchMutation(t.Context(), artifactFetchCLIInput{
		name: "antigravity", version: "1.2.13", previewOnly: true,
	}, backend, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), sentence) ||
		strings.Contains(err.Error(), artifact.ErrUpstreamVersionUnavailable.Error()) || out.Len() != 0 {
		t.Fatalf("err=%v stdout=%s stderr=%s", err, out.String(), errOut.String())
	}
}
