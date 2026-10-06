package operator

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestAntigravityFetchPlanCoherenceAndUpstreamConfirm(t *testing.T) {
	plan := antigravityOperatorPreview(t)
	if err := validateArtifactFetchPlan(plan, ArtifactFetchPreviewRequest{Name: "antigravity", Version: "1.2.14"}); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name   string
		mutate func(*artifact.PreviewPlan)
	}{
		{name: "origin", mutate: func(plan *artifact.PreviewPlan) { plan.RegistryOrigin = "https://example.invalid" }},
		{name: "name", mutate: func(plan *artifact.PreviewPlan) { plan.Name = "grok" }},
		{name: "version", mutate: func(plan *artifact.PreviewPlan) { plan.Version = "1.2.15" }},
		{name: "engines", mutate: func(plan *artifact.PreviewPlan) { plan.EnginesNode = ">=22" }},
		{name: "source plan", mutate: func(plan *artifact.PreviewPlan) { plan.SourcePlan = strings.TrimSuffix(plan.SourcePlan, "}") }},
		{name: "max bytes", mutate: func(plan *artifact.PreviewPlan) { plan.MaxBytes = artifact.DefaultAntigravitySourceMaxBytes }},
		{name: "policy", mutate: func(plan *artifact.PreviewPlan) { plan.PolicyVersion = artifact.GrokFetchPolicyVersion }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			mutated := plan
			tc.mutate(&mutated)
			if err := validateArtifactFetchPlan(mutated, ArtifactFetchPreviewRequest{Name: "antigravity", Version: "1.2.14"}); !errors.Is(err, ErrInvalidArtifactFetchPreview) {
				t.Fatalf("err=%v", err)
			}
		})
	}

	backend := &fakeArtifactFetchBackend{err: &artifact.UpstreamVersionError{
		Name: "antigravity", Requested: "1.2.13", Published: "1.2.14",
	}}
	service, st, _ := artifactFetchTestService(t, backend)
	request := ArtifactFetchApplyRequest{
		Name: "antigravity", Version: "1.2.13", PreviewDigest: "sha256:" + strings.Repeat("a", 64),
		ConfirmName: "antigravity", ConfirmVersion: "1.2.13", Reason: "confirm published version",
		IdempotencyKey: "antigravity-upstream-moved",
	}
	_, err := service.ApplyArtifactFetch(t.Context(), request)
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeArtifactFetchPreviewStale ||
		!errors.Is(err, store.ErrArtifactFetchPreviewStale) {
		t.Fatalf("confirm err=%v", err)
	}
	var operations int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM artifact_fetch_operations`).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("operations=%d err=%v", operations, err)
	}
}

func antigravityOperatorPreview(t *testing.T) artifact.PreviewPlan {
	t.Helper()
	raw := antigravityOperatorSourcePlan(t)
	return artifact.PreviewPlan{
		PolicyVersion: artifact.AntigravityFetchPolicyVersion, SourceKind: artifact.ArtifactSourceAntigravity,
		Name: "antigravity", Version: "1.2.14",
		RegistryOrigin:  artifact.ProductionAntigravityManifestOrigin,
		TarballURL:      "https://storage.googleapis.com/antigravity-public/antigravity-cli/1.2.14-4571742832820224/",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, sha512Size)),
		MaxBytes:        artifact.DefaultAntigravityBundleMaxBytes, SourcePlan: raw,
		PreviewedAt:   time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		PreviewDigest: "sha256:" + strings.Repeat("d", 64),
	}
}

const sha512Size = 64

func antigravityOperatorSourcePlan(t *testing.T) string {
	t.Helper()
	sha := strings.Repeat("ab", 64)
	plan := artifact.AntigravityFetchPlan{
		PolicyVersion: artifact.AntigravityFetchPolicyVersion, Name: "antigravity", Version: "1.2.14",
		Build: "4571742832820224", ManifestOrigin: artifact.ProductionAntigravityManifestOrigin,
		DownloadOrigin:   artifact.ProductionAntigravityDownloadOrigin,
		ReleaseDirectory: "https://storage.googleapis.com/antigravity-public/antigravity-cli/1.2.14-4571742832820224/",
		Sources: []artifact.AntigravitySource{
			{TargetOS: "linux", TargetArch: "amd64", Platform: "linux_amd64", Dir: "linux-x64", File: "cli_linux_x64.tar.gz", SHA512: sha},
			{TargetOS: "linux", TargetArch: "arm64", Platform: "linux_arm64", Dir: "linux-arm", File: "cli_linux_arm64.tar.gz", SHA512: sha},
			{TargetOS: "darwin", TargetArch: "amd64", Platform: "darwin_amd64", Dir: "darwin-x64", File: "cli_mac_x64.tar.gz", SHA512: sha},
			{TargetOS: "darwin", TargetArch: "arm64", Platform: "darwin_arm64", Dir: "darwin-arm", File: "cli_mac_arm64.tar.gz", SHA512: sha},
			{TargetOS: "windows", TargetArch: "amd64", Platform: "windows_amd64", Dir: "windows-x64", File: "cli_windows_x64.exe", SHA512: sha},
			{TargetOS: "windows", TargetArch: "arm64", Platform: "windows_arm64", Dir: "windows-arm", File: "cli_windows_arm64.exe", SHA512: sha},
		},
		SourceIdentity: "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, sha512Size)),
		SourceMaxBytes: artifact.DefaultAntigravitySourceMaxBytes, BundleMaxBytes: artifact.DefaultAntigravityBundleMaxBytes,
		PreviewedAt:   time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		PreviewDigest: "sha256:" + strings.Repeat("c", 64),
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(plan); err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimSuffix(buf.String(), "\n")
	if !artifact.ValidAntigravitySourcePlan(raw) {
		t.Fatalf("source plan is not canonical (%d bytes)", len(raw))
	}
	return raw
}
