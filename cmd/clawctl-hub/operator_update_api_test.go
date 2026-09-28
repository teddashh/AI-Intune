package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func TestOperatorUpdatesReturnsSafeNoStoreStructuredRead(t *testing.T) {
	f := newJobsFixture(t, "updates-read")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	record := writeJobTestArtifact(t, f.artifactsDir, "2026.9.8", "operator-updates")

	response := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/updates?channel=canary&status=available_unverified&version=2026.9.8&limit=7", "", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	for _, forbidden := range []string{record.TarballURL, record.FetchedBy, "tarball_url", "fetched_by", f.artifactsDir} {
		if forbidden != "" && strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("safe updates response disclosed %q: %s", forbidden, response.Body.String())
		}
	}
	var result operator.UpdateReadResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != operator.UpdateReadSchemaVersion || result.Consistency != operator.UpdateReadConsistencyLive ||
		result.Artifacts.Total != 1 || len(result.Artifacts.Items) != 1 || len(result.Channels) != 1 ||
		result.Channels[0].Name != "canary" || len(result.Channels[0].Previews) != 1 ||
		result.Channels[0].Previews[0].Artifact.ArtifactID != record.SHA256 ||
		result.Channels[0].Previews[0].Artifact.VerifiedAt != nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestOperatorUpdatesRejectsAmbiguousQuery(t *testing.T) {
	for _, query := range []string{
		"?", "?unknown=value", "?channel=beta", "?channel=canary&channel=stable",
		"?status=ready", "?version=%20bad", "?limit=0", "?limit=101", "?cursor=%20bad",
	} {
		t.Run(query, func(t *testing.T) {
			if _, err := parseOperatorUpdateReadRequest(httptestRequest(http.MethodGet, "/v1/operator/updates"+query)); err == nil {
				t.Fatalf("query %q was accepted", query)
			}
		})
	}
}
