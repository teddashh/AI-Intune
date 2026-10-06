package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func TestArtifactFetchWebAntigravityPreviewNamesPublishedVersion(t *testing.T) {
	s, _ := newServer(t)
	const published = "1.2.15"
	sentence := "上游目前只提供 Antigravity " + published + "；改用這個版號重新 Preview。"
	fake := &fakeArtifactWebOperator{previewErr: &artifact.UpstreamVersionError{
		Name: "antigravity", Requested: "1.2.13", Published: published,
	}}
	s.artifactOperator = fake
	preview := postForm(t, s, "/apps/artifact-fetches/preview", url.Values{
		"name": {"antigravity"}, "version": {"1.2.13"}, "reason": {"fetch the published CLI"},
	})
	if preview.Code != http.StatusConflict || fake.previewCalls != 1 || len(fake.applyCalls) != 0 ||
		!strings.Contains(preview.Body.String(), sentence) || !strings.Contains(preview.Body.String(), "沒有建立 fetch preview") {
		t.Fatalf("status=%d calls=%d applies=%d body=%s", preview.Code, fake.previewCalls, len(fake.applyCalls), preview.Body.String())
	}
	status, detail := artifactFetchWebError(fmt.Errorf("%w: hidden-upstream-9.9.9-not-a-version", artifact.ErrMetadataInvalid), true)
	if status != http.StatusBadGateway || strings.Contains(detail, "hidden-upstream") || strings.Contains(detail, "9.9.9") {
		t.Fatalf("status=%d detail=%q", status, detail)
	}
}

func TestArtifactFetchWebAcceptsAntigravityTarget(t *testing.T) {
	s, _ := newServer(t)
	fake := &fakeArtifactWebOperator{
		operationID: "antigravity-fetch-operation",
		preview: operator.ArtifactFetchPreviewResult{
			SchemaVersion: operator.ArtifactFetchPreviewSchemaVersion, PolicyVersion: artifact.AntigravityFetchPolicyVersion,
			SourceKind: artifact.ArtifactSourceAntigravity, PreviewedAt: time.Now().UTC(),
			Name: "antigravity", Version: "1.2.14", RegistryOrigin: artifact.ProductionAntigravityManifestOrigin,
			SHA512Integrity: "sha512-" + strings.Repeat("A", 4), MaxBytes: artifact.DefaultAntigravityBundleMaxBytes,
			PreviewDigest: "sha256:" + strings.Repeat("a", 64), EnqueueAllowed: true, Blockers: []string{},
		},
	}
	s.artifactOperator = fake
	preview := postForm(t, s, "/apps/artifact-fetches/preview", url.Values{
		"name": {"antigravity"}, "version": {"1.2.14"}, "reason": {"official antigravity bundle"},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "antigravity@1.2.14") {
		t.Fatalf("status=%d body=%s", preview.Code, preview.Body.String())
	}
	fake.preview.RegistryOrigin = artifact.ProductionRegistryOrigin
	foreign := postForm(t, s, "/apps/artifact-fetches/preview", url.Values{
		"name": {"antigravity"}, "version": {"1.2.14"}, "reason": {"official antigravity bundle"},
	})
	if foreign.Code != http.StatusInternalServerError || len(fake.applyCalls) != 0 ||
		strings.Contains(foreign.Body.String(), artifact.ProductionRegistryOrigin) {
		t.Fatalf("foreign origin status=%d applies=%d body=%s", foreign.Code, len(fake.applyCalls), foreign.Body.String())
	}
}
