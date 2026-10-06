package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestApplyOperatorArtifactFetchAcceptsAntigravityBundleBudget(t *testing.T) {
	s := newTestStore(t)
	req := artifactFetchTestRequest("artifact-fetch-antigravity-budget", "e")
	req.Name, req.Version = "antigravity", "1.2.14"
	releaseDirectory := "https://storage.googleapis.com/antigravity-public/antigravity-cli/1.2.14-4571742832820224/"
	sha := strings.Repeat("ab", 64)
	plan := artifact.AntigravityFetchPlan{
		PolicyVersion: artifact.AntigravityFetchPolicyVersion, Name: req.Name, Version: req.Version,
		Build: "4571742832820224", ManifestOrigin: artifact.ProductionAntigravityManifestOrigin,
		DownloadOrigin: artifact.ProductionAntigravityDownloadOrigin, ReleaseDirectory: releaseDirectory,
		Sources: []artifact.AntigravitySource{
			{TargetOS: "linux", TargetArch: "amd64", Platform: "linux_amd64", Dir: "linux-x64", File: "cli_linux_x64.tar.gz", SHA512: sha},
			{TargetOS: "linux", TargetArch: "arm64", Platform: "linux_arm64", Dir: "linux-arm", File: "cli_linux_arm64.tar.gz", SHA512: sha},
			{TargetOS: "darwin", TargetArch: "amd64", Platform: "darwin_amd64", Dir: "darwin-x64", File: "cli_mac_x64.tar.gz", SHA512: sha},
			{TargetOS: "darwin", TargetArch: "arm64", Platform: "darwin_arm64", Dir: "darwin-arm", File: "cli_mac_arm64.tar.gz", SHA512: sha},
			{TargetOS: "windows", TargetArch: "amd64", Platform: "windows_amd64", Dir: "windows-x64", File: "cli_windows_x64.exe", SHA512: sha},
			{TargetOS: "windows", TargetArch: "arm64", Platform: "windows_arm64", Dir: "windows-arm", File: "cli_windows_arm64.exe", SHA512: sha},
		},
		SourceIdentity: "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 64)),
		SourceMaxBytes: artifact.DefaultAntigravitySourceMaxBytes, BundleMaxBytes: artifact.DefaultAntigravityBundleMaxBytes,
		PreviewedAt:   time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
		PreviewDigest: req.PreviewDigest,
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(plan); err != nil {
		t.Fatal(err)
	}
	sourcePlan := strings.TrimSuffix(encoded.String(), "\n")
	if !artifact.ValidAntigravitySourcePlan(sourcePlan) {
		t.Fatalf("source plan is not canonical: %s", sourcePlan)
	}
	prepared := ArtifactFetchPrepared{
		Name: req.Name, Version: req.Version, SourceKind: artifact.ArtifactSourceAntigravity,
		SourcePlan: sourcePlan, RegistryOrigin: artifact.ProductionAntigravityManifestOrigin,
		TarballURL: releaseDirectory, SHA512Integrity: plan.SourceIdentity,
		MaxBytes: artifact.DefaultAntigravityBundleMaxBytes, CurrentPreviewDigest: req.PreviewDigest,
	}

	for i, plan := range []string{
		strings.Replace(sourcePlan, "{", `{"mirror":"https://example.invalid/",`, 1),
		sourcePlan + "\n",
	} {
		refused := req
		refused.IdempotencyKey = "artifact-fetch-antigravity-refused-" + strconv.Itoa(i)
		refused.RequestDigest = artifactFetchTestDigest(strconv.Itoa(i))
		tampered := prepared
		tampered.SourcePlan = plan
		if _, err := s.ApplyOperatorArtifactFetch(refused, func() (ArtifactFetchPrepared, error) {
			return tampered, nil
		}); !errors.Is(err, ErrArtifactFetchInvalid) {
			t.Fatalf("plan %d: accepted a source plan the fetcher did not write: %v", i, err)
		}
	}
	var operations int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM artifact_fetch_operations`).Scan(&operations); err != nil || operations != 0 {
		t.Fatalf("operations=%d err=%v", operations, err)
	}

	created, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		return prepared, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed || !created.Audited || created.Operation.State != ArtifactFetchQueued ||
		created.Operation.SourceKind != artifact.ArtifactSourceAntigravity ||
		created.Operation.MaxBytes != artifact.DefaultAntigravityBundleMaxBytes ||
		created.Operation.Name != "antigravity" {
		t.Fatalf("created=%+v", created)
	}
}

func TestPreparedAntigravityAssignmentSpecPinsTargetAndLayout(t *testing.T) {
	contract, ok := agentadapter.Lookup(appcatalog.Adapter{Name: "antigravity", Version: 1})
	if !ok {
		t.Fatal("antigravity adapter is not registered")
	}
	sha := strings.Repeat("c", 64)
	manifest := appcatalog.Manifest{ID: "antigravity", Version: "1.2.14",
		Artifact: appcatalog.Artifact{SHA256: sha, Size: 4096}}
	target := appcatalog.Platform{OS: "darwin", Arch: "arm64"}
	spec := func(mutate func(*model.AntigravitySpec)) string {
		t.Helper()
		value := model.AntigravitySpec{
			Kind: "antigravity", Version: "1.2.14", TargetOS: "darwin", TargetArch: "arm64",
			BundleLayout: model.AntigravityBundleLayoutV1,
			Artifact:     &model.ArtifactRef{SHA256: sha, Size: 4096, URL: "/v1/artifacts/" + sha},
		}
		if mutate != nil {
			mutate(&value)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if !validPreparedAssignmentSpec(contract, manifest, target, spec(nil)) {
		t.Fatal("refused the prepared Antigravity spec")
	}
	for name, mutate := range map[string]func(*model.AntigravitySpec){
		"kind":         func(spec *model.AntigravitySpec) { spec.Kind = "grok" },
		"version":      func(spec *model.AntigravitySpec) { spec.Version = "1.2.15" },
		"target os":    func(spec *model.AntigravitySpec) { spec.TargetOS = "linux" },
		"target arch":  func(spec *model.AntigravitySpec) { spec.TargetArch = "amd64" },
		"layout":       func(spec *model.AntigravitySpec) { spec.BundleLayout = "grok-bundle:v1" },
		"artifact url": func(spec *model.AntigravitySpec) { spec.Artifact.URL = "/v1/artifacts/other" },
		"artifact size": func(spec *model.AntigravitySpec) {
			spec.Artifact.Size = 4097
		},
		"upstream sha512": func(spec *model.AntigravitySpec) { spec.Artifact.SHA512 = strings.Repeat("d", 128) },
	} {
		if validPreparedAssignmentSpec(contract, manifest, target, spec(mutate)) {
			t.Errorf("%s: accepted a prepared spec that does not match the manifest and target", name)
		}
	}
}
