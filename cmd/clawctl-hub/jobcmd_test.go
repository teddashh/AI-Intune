package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/model"
)

func writeJobTestArtifact(t *testing.T, dir, version, seed string) artifactSidecar {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("test OpenClaw tarball " + seed)
	sum := sha256.Sum256(body)
	sri := sha512.Sum512(body)
	digest := hex.EncodeToString(sum[:])
	record := artifactSidecar{
		Name: "openclaw", Version: version,
		TarballURL:      "https://registry.npmjs.org/openclaw/-/openclaw-" + version + ".tgz",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sri[:]),
		SHA256:          digest, Size: int64(len(body)),
		EnginesNode: ">=22.19.0", FetchedAt: jobsTestNow, FetchedBy: "fixture-fetch-principal",
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeArtifactSidecar(dir, record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestPrepareOpenClawJobGeneratesSpecFromSidecar(t *testing.T) {
	dir := t.TempDir()
	record := writeJobTestArtifact(t, dir, "2026.6.10", "one")
	res, err := prepareOpenClawJob(dir, record.Version, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != "sha256:"+record.SHA256 {
		t.Fatalf("artifact_digest=%q", res.Digest)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(res.Spec)); err != nil || compact.String() != res.Spec {
		t.Fatalf("spec is not compact JSON: %q err=%v", res.Spec, err)
	}
	if !strings.Contains(res.Spec, `"engines_node":">=22.19.0"`) {
		t.Fatalf("spec escaped engines constraint: %s", res.Spec)
	}
	var spec model.OpenClawSpec
	if err := json.Unmarshal([]byte(res.Spec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "openclaw" || spec.Version != record.Version || spec.Artifact == nil {
		t.Fatalf("spec=%+v", spec)
	}
	if got := spec.Artifact; got.SHA256 != record.SHA256 || got.Size != record.Size ||
		got.URL != "/v1/artifacts/"+record.SHA256 || got.EnginesNode != record.EnginesNode ||
		got.UpstreamTarball != "" || got.SHA512 != "" {
		t.Fatalf("artifact ref=%+v", got)
	}
}

func TestPrepareOpenClawJobRequiresFetchedArtifact(t *testing.T) {
	_, err := prepareOpenClawJob(t.TempDir(), "2026.6.10", "")
	if err == nil || !strings.Contains(err.Error(), "artifact fetch openclaw@2026.6.10") {
		t.Fatalf("err=%v", err)
	}
}

func TestPrepareOpenClawJobRequiresDigestForAmbiguousVersion(t *testing.T) {
	dir := t.TempDir()
	one := writeJobTestArtifact(t, dir, "2026.6.10", "one")
	two := writeJobTestArtifact(t, dir, "2026.6.10", "two")
	_, err := prepareOpenClawJob(dir, "2026.6.10", "")
	if err == nil || !strings.Contains(err.Error(), one.SHA256) || !strings.Contains(err.Error(), two.SHA256) {
		t.Fatalf("err=%v", err)
	}
	res, err := prepareOpenClawJob(dir, "2026.6.10", two.SHA256)
	if err != nil || res.Digest != "sha256:"+two.SHA256 {
		t.Fatalf("result=%+v err=%v", res, err)
	}
}
