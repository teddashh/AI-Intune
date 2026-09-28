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

func artifactClientTestPtr[T any](value T) *T { return &value }

func artifactClientTestTime() time.Time {
	return time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC)
}

func artifactClientTestSummary(status operator.ArtifactReadStatus) operator.ArtifactSummary {
	sha := strings.Repeat("a", 64)
	result := operator.ArtifactSummary{
		ArtifactID: sha, Name: artifactClientTestPtr("openclaw"),
		Version: artifactClientTestPtr("2026.9.8"), SHA256: &sha,
		Digest: artifactClientTestPtr("sha256:" + sha), SizeBytes: artifactClientTestPtr(int64(4096)),
		EnginesNode: artifactClientTestPtr(">=22"), FetchedAt: artifactClientTestPtr(artifactClientTestTime().Add(-time.Hour)),
		Status: status, DeploymentReferences: 2, ActiveDeploymentReferences: 1,
	}
	if status == operator.ArtifactReady {
		result.VerifiedAt = artifactClientTestPtr(artifactClientTestTime())
	}
	return result
}

func artifactClientTestList() operator.ArtifactListResult {
	return operator.ArtifactListResult{
		SchemaVersion: operator.ArtifactReadSchemaVersion,
		Consistency:   operator.ArtifactReadConsistencyLive,
		EvaluatedAt:   artifactClientTestTime(),
		Total:         1,
		StatusCounts: []operator.ArtifactStatusCount{
			{Status: operator.ArtifactAvailableUnverified, Count: 1},
			{Status: operator.ArtifactUnavailable, Count: 0},
			{Status: operator.ArtifactInvalid, Count: 0},
		},
		Items: []operator.ArtifactSummary{artifactClientTestSummary(operator.ArtifactAvailableUnverified)},
	}
}

func artifactClientTestHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
}

func TestArtifactClientAcceptsCanonicalListAndVerifiedDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactClientTestHeaders(w)
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("request headers=%v", r.Header)
		}
		switch r.URL.Path {
		case "/v1/operator/artifacts":
			if r.Method != http.MethodGet || r.URL.Query().Get("status") != "available_unverified" ||
				r.URL.Query().Get("version") != "2026.9.8" || r.URL.Query().Get("limit") != "7" {
				t.Errorf("artifact list method=%s query=%v", r.Method, r.URL.Query())
			}
			_ = json.NewEncoder(w).Encode(artifactClientTestList())
		case "/v1/operator/artifacts/" + strings.Repeat("a", 64):
			result := operator.ArtifactDetailResult{
				SchemaVersion: operator.ArtifactReadSchemaVersion,
				Consistency:   operator.ArtifactReadConsistencyLive,
				EvaluatedAt:   artifactClientTestTime(),
				Item:          artifactClientTestSummary(operator.ArtifactReady),
			}
			_ = json.NewEncoder(w).Encode(result)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	list, err := client.Artifacts(t.Context(), operator.ArtifactListRequest{
		Status: operator.ArtifactAvailableUnverified, Version: "2026.9.8", Limit: 7,
	})
	if err != nil || len(list.Items) != 1 || list.Items[0].VerifiedAt != nil {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	detail, err := client.Artifact(t.Context(), strings.Repeat("a", 64))
	if err != nil || detail.Item.VerifiedAt == nil || detail.Item.Status != operator.ArtifactReady {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
}

func TestArtifactClientRejectsContradictoryServerEvidence(t *testing.T) {
	base := artifactClientTestList()
	issue := operator.ArtifactReadIssue("tarball_missing")
	for _, test := range []struct {
		name   string
		mutate func(*operator.ArtifactListResult)
	}{
		{"ready in metadata list", func(v *operator.ArtifactListResult) { v.Items[0].Status = operator.ArtifactReady }},
		{"private-like unknown field", nil},
		{"digest mismatch", func(v *operator.ArtifactListResult) {
			v.Items[0].Digest = artifactClientTestPtr("sha256:" + strings.Repeat("b", 64))
		}},
		{"status count mismatch", func(v *operator.ArtifactListResult) { v.StatusCounts[0].Count = 2 }},
		{"issue on available", func(v *operator.ArtifactListResult) { v.Items[0].Issue = &issue }},
		{"future schema", func(v *operator.ArtifactListResult) { v.SchemaVersion++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.Items = append([]operator.ArtifactSummary(nil), base.Items...)
			value.StatusCounts = append([]operator.ArtifactStatusCount(nil), base.StatusCounts...)
			if test.mutate != nil {
				test.mutate(&value)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if test.mutate == nil {
				raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"tarball_url":"https://secret.invalid/token"}`)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				artifactClientTestHeaders(w)
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			client := operatorClientForServer(t, server)
			if got, err := client.Artifacts(t.Context(), operator.ArtifactListRequest{}); err == nil {
				t.Fatalf("accepted contradictory result: %+v", got)
			}
		})
	}
}

func TestArtifactClientRejectsInvalidRequestsBeforeHTTP(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	for _, request := range []operator.ArtifactListRequest{
		{Status: operator.ArtifactReady}, {Version: " bad"}, {Limit: -1}, {Limit: 101}, {Cursor: " bad"},
	} {
		if _, err := client.Artifacts(t.Context(), request); err == nil {
			t.Errorf("accepted request %+v", request)
		}
	}
	for _, id := range []string{"", "../secret", strings.Repeat("A", 64), "invalid-short"} {
		if _, err := client.Artifact(t.Context(), id); err == nil {
			t.Errorf("accepted artifact id %q", id)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid requests reached HTTP server %d times", hits.Load())
	}
}
