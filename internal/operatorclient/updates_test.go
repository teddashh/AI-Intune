package operatorclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func updateClientTestResult(channel string) operator.UpdateReadResult {
	artifacts := artifactClientTestList()
	version := "2026.9.7"
	observedAt := artifacts.EvaluatedAt.Add(-time.Minute)
	return operator.UpdateReadResult{
		SchemaVersion: operator.UpdateReadSchemaVersion,
		Consistency:   operator.UpdateReadConsistencyLive,
		EvaluatedAt:   artifacts.EvaluatedAt,
		Artifacts:     artifacts,
		Channels: []operator.UpdateChannelSummary{{
			Name: channel, MemberCount: 1, Members: []operator.UpdateMachineSummary{{
				MachineID: "machine-1", DisplayName: "canary-box", LastObservedVersion: &version,
				LastObservationReceivedAt: &observedAt,
			}},
			Previews: []operator.UpdateArtifactPreview{{
				Artifact: artifacts.Items[0],
				Plan:     &operator.UpdatePlanSummary{MissingPackages: 1},
				Blockers: []string{"no_included_targets"},
			}},
		}},
	}
}

func TestUpdateClientAcceptsCanonicalChannelRead(t *testing.T) {
	raw, err := json.Marshal(updateClientTestResult("canary"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"last_observed_version":"2026.9.7"`) ||
		strings.Contains(string(raw), `"current_version"`) {
		t.Fatalf("updates member JSON did not preserve historical evidence semantics: %s", raw)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactClientTestHeaders(w)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/operator/updates" ||
			r.URL.Query().Get("channel") != "canary" || r.URL.Query().Get("status") != "available_unverified" ||
			r.URL.Query().Get("version") != "2026.9.8" || r.URL.Query().Get("limit") != "7" ||
			r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("request method=%s path=%s query=%v headers=%v", r.Method, r.URL.Path, r.URL.Query(), r.Header)
		}
		_ = json.NewEncoder(w).Encode(updateClientTestResult("canary"))
	}))
	defer server.Close()
	result, err := operatorClientForServer(t, server).Updates(t.Context(), operator.UpdateReadRequest{
		Channel: "canary", Status: operator.ArtifactAvailableUnverified, Version: "2026.9.8", Limit: 7,
	})
	if err != nil || len(result.Channels) != 1 || result.Channels[0].Name != "canary" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestUpdateClientRejectsContradictoryAndPrivateServerEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*operator.UpdateReadResult)
		field  string
	}{
		{"wrong channel", func(v *operator.UpdateReadResult) { v.Channels[0].Name = "stable" }, ""},
		{"verified overview", func(v *operator.UpdateReadResult) {
			v.Artifacts.Items[0].Status = operator.ArtifactReady
			v.Channels[0].Previews[0].Artifact.Status = operator.ArtifactReady
		}, ""},
		{"preview version differs from artifact page", func(v *operator.UpdateReadResult) {
			v.Channels[0].Previews[0].Artifact.Version = artifactClientTestPtr("2026.9.9")
		}, ""},
		{"preview references differ from artifact page", func(v *operator.UpdateReadResult) {
			v.Channels[0].Previews[0].Artifact.DeploymentReferences++
		}, ""},
		{"member count mismatch", func(v *operator.UpdateReadResult) { v.Channels[0].MemberCount++ }, ""},
		{"member version without evidence time", func(v *operator.UpdateReadResult) {
			v.Channels[0].Members[0].LastObservationReceivedAt = nil
		}, ""},
		{"member evidence after evaluation", func(v *operator.UpdateReadResult) {
			future := v.EvaluatedAt.Add(time.Second)
			v.Channels[0].Members[0].LastObservationReceivedAt = &future
		}, ""},
		{"private unknown field", nil, `,"tarball_url":"https://secret.invalid/token"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := updateClientTestResult("canary")
			if test.mutate != nil {
				test.mutate(&value)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if test.field != "" {
				raw = []byte(strings.TrimSuffix(string(raw), "}") + test.field + "}")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				artifactClientTestHeaders(w)
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			if got, err := operatorClientForServer(t, server).Updates(t.Context(), operator.UpdateReadRequest{Channel: "canary"}); err == nil {
				t.Fatalf("accepted contradictory result: %+v", got)
			}
		})
	}
}

func TestEqualArtifactSummaryCoversCompleteCanonicalProjection(t *testing.T) {
	base := artifactClientTestSummary(operator.ArtifactAvailableUnverified)
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	var independent operator.ArtifactSummary
	if err := json.Unmarshal(raw, &independent); err != nil {
		t.Fatal(err)
	}
	if !equalArtifactSummary(base, independent) {
		t.Fatal("equal summaries with independently decoded pointers did not match")
	}

	mutations := []struct {
		name   string
		mutate func(*operator.ArtifactSummary)
	}{
		{"artifact id", func(v *operator.ArtifactSummary) { v.ArtifactID = strings.Repeat("b", 64) }},
		{"name", func(v *operator.ArtifactSummary) { v.Name = artifactClientTestPtr("other") }},
		{"version", func(v *operator.ArtifactSummary) { v.Version = artifactClientTestPtr("2026.9.9") }},
		{"sha256", func(v *operator.ArtifactSummary) { v.SHA256 = artifactClientTestPtr(strings.Repeat("b", 64)) }},
		{"digest", func(v *operator.ArtifactSummary) {
			v.Digest = artifactClientTestPtr("sha256:" + strings.Repeat("b", 64))
		}},
		{"size", func(v *operator.ArtifactSummary) { v.SizeBytes = artifactClientTestPtr(int64(4097)) }},
		{"engines", func(v *operator.ArtifactSummary) { v.EnginesNode = artifactClientTestPtr(">=24") }},
		{"fetched at", func(v *operator.ArtifactSummary) {
			v.FetchedAt = artifactClientTestPtr(artifactClientTestTime().Add(-2 * time.Hour))
		}},
		{"status", func(v *operator.ArtifactSummary) { v.Status = operator.ArtifactUnavailable }},
		{"issue", func(v *operator.ArtifactSummary) {
			v.Issue = artifactClientTestPtr(operator.ArtifactIssueTarballMissing)
		}},
		{"deployment references", func(v *operator.ArtifactSummary) { v.DeploymentReferences++ }},
		{"active deployment references", func(v *operator.ArtifactSummary) { v.ActiveDeploymentReferences++ }},
		{"verified at", func(v *operator.ArtifactSummary) { v.VerifiedAt = artifactClientTestPtr(artifactClientTestTime()) }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			candidate := independent
			test.mutate(&candidate)
			if equalArtifactSummary(base, candidate) {
				t.Fatal("field mismatch was not detected")
			}
		})
	}
}

func TestUpdateClientRejectsInvalidRequestBeforeHTTP(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	for _, request := range []operator.UpdateReadRequest{
		{Channel: "beta"}, {Status: operator.ArtifactReady}, {Version: " bad"}, {Limit: -1}, {Cursor: " bad"},
	} {
		if _, err := client.Updates(t.Context(), request); err == nil {
			t.Errorf("accepted request %+v", request)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid requests reached HTTP server %d times", hits.Load())
	}
}
