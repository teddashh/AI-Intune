package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestReadSidecarsValidatesFilenameAndFileDigestHelperMeasuresBytes(t *testing.T) {
	dir := t.TempDir()
	body := []byte("Hub 親自量的 artifact")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	record := Sidecar{Name: "openclaw", Version: "2026.9.2", SHA256: digest}
	b, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := ReadSidecars(dir)
	if err != nil || len(records) != 1 || records[0] != record {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	if !FileHasSHA256(filepath.Join(dir, digest+".tgz"), digest) || FileHasSHA256(filepath.Join(dir, digest+".tgz"), strings.Repeat("0", 64)) {
		t.Fatal("FileHasSHA256 沒有依檔案實際位元組判斷")
	}
	if !ValidSHA256Hex(digest) || ValidSHA256Hex(strings.ToUpper(digest)) || ValidSHA256Hex("../artifact") {
		t.Fatal("ValidSHA256Hex 接受了小寫 64 hex 以外的值")
	}
}

func writeMaterialFixture(t *testing.T, dir, version, suffix string) Sidecar {
	return writeNamedMaterialFixture(t, dir, "openclaw", version, suffix)
}

func writeNamedMaterialFixture(t *testing.T, dir, name, version, suffix string) Sidecar {
	t.Helper()
	body := []byte(name + " tarball " + suffix)
	sum := sha256.Sum256(body)
	sri := sha512.Sum512(body)
	digest := hex.EncodeToString(sum[:])
	record := Sidecar{
		Name: name, Version: version,
		TarballURL:      "https://registry.example/" + name + "-" + version + ".tgz",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sri[:]),
		SHA256:          digest, Size: int64(len(body)),
		EnginesNode: ">=22.19.0", FetchedAt: time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC), FetchedBy: "test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}

func writeNodeRuntimeMaterialFixture(t *testing.T, dir, version string, targets ...NodeRuntimeTarget) Sidecar {
	t.Helper()
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tw := tar.NewWriter(gz)
	for _, target := range targets {
		key := target.OS + "-" + target.Arch
		for _, entry := range []struct {
			name string
			mode int64
			body string
		}{
			{name: nodeRuntimeMaterialNodeName(key), mode: 0o755, body: "node-" + key},
			{name: "node-runtime/" + key + "/lib/node_modules/npm/bin/npm-cli.js", mode: 0o644, body: "npm-" + key},
		} {
			if err := tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg,
				Mode: entry.mode, Size: int64(len(entry.body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body.Bytes())
	source := sha512.Sum512([]byte("Node runtime source " + version))
	digest := hex.EncodeToString(sum[:])
	record := Sidecar{
		Name: "node-runtime", Version: version,
		TarballURL:      "https://nodejs.org/dist/v" + version + "/SHASUMS256.txt",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(body.Len()),
		FetchedAt: time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC), FetchedBy: "test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}

func nodeRuntimeMaterialNodeName(key string) string {
	name := "node-runtime/" + key + "/bin/node"
	if strings.HasPrefix(key, "windows-") {
		return name + ".exe"
	}
	return name
}

func writeHermesMaterialFixture(t *testing.T, dir, version string) Sidecar {
	t.Helper()
	body := []byte("AI-Intune Hermes OCI bundle " + version)
	sum := sha256.Sum256(body)
	source := sha512.Sum512([]byte("Hermes OCI source " + version))
	digest := hex.EncodeToString(sum[:])
	indexDigest := "sha256:" + strings.Repeat("a", 64)
	record := Sidecar{
		Name: "hermes-agent", Version: version,
		TarballURL:      ProductionHermesRegistryOrigin + "/v2/" + HermesImageRepository + "/manifests/" + indexDigest,
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(len(body)), EnginesNode: "",
		FetchedAt: time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC), FetchedBy: "test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestResolveHermesMaterialBindsOfficialImageAndCanonicalBundleSpec(t *testing.T) {
	dir := t.TempDir()
	record := writeHermesMaterialFixture(t, dir, "2026.9.7")
	material, err := ResolveHermesMaterialContext(t.Context(), dir, record.Version,
		record.SHA256, "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if material.Artifact != record || material.Digest != "sha256:"+record.SHA256 ||
		material.TargetOS != "linux" || material.TargetArch != "arm64" ||
		material.ImageReference != "docker.io/nousresearch/hermes-agent:v2026.9.7" ||
		material.ImageIndexDigest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("material=%+v", material)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(material.Spec)); err != nil || compact.String() != material.Spec {
		t.Fatalf("spec=%q err=%v", material.Spec, err)
	}
	var spec model.HermesSpec
	if err := json.Unmarshal([]byte(material.Spec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "hermes-agent" || spec.Version != record.Version ||
		spec.TargetOS != "linux" || spec.TargetArch != "arm64" ||
		spec.BundleLayout != model.HermesOCIBundleLayoutV1 ||
		spec.ImageReference != material.ImageReference || spec.ImageIndexDigest != material.ImageIndexDigest ||
		spec.Artifact == nil || spec.Artifact.SHA256 != record.SHA256 || spec.Artifact.Size != record.Size ||
		spec.Artifact.URL != "/v1/artifacts/"+record.SHA256 || spec.Artifact.EnginesNode != "" ||
		spec.Artifact.UpstreamTarball != "" || spec.Artifact.SHA512 != "" {
		t.Fatalf("spec=%+v", spec)
	}
}

func TestResolveHermesMaterialRejectsInvalidTargetSourceMetadataAndBytes(t *testing.T) {
	for _, target := range [][2]string{{"darwin", "arm64"}, {"linux", "riscv64"}} {
		dir := t.TempDir()
		record := writeHermesMaterialFixture(t, dir, "2026.9.7")
		if _, err := ResolveHermesMaterialContext(t.Context(), dir, record.Version,
			record.SHA256, target[0], target[1]); err == nil {
			t.Fatalf("accepted target %v", target)
		}
	}
	for _, mutate := range []func(*Sidecar){
		func(record *Sidecar) { record.TarballURL = "https://example.com/image" },
		func(record *Sidecar) { record.TarballURL += "/extra" },
		func(record *Sidecar) { record.EnginesNode = ">=24" },
	} {
		dir := t.TempDir()
		record := writeHermesMaterialFixture(t, dir, "2026.9.7")
		mutate(&record)
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, record.SHA256+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveHermesMaterialContext(t.Context(), dir, record.Version,
			record.SHA256, "linux", "amd64"); err == nil {
			t.Fatalf("accepted metadata %+v", record)
		}
	}
	dir := t.TempDir()
	record := writeHermesMaterialFixture(t, dir, "2026.9.7")
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"),
		bytes.Repeat([]byte{'x'}, int(record.Size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveHermesMaterialContext(t.Context(), dir, record.Version,
		record.SHA256, "linux", "amd64"); err == nil {
		t.Fatal("accepted altered Hermes bundle")
	}
}

func TestResolveNodeRuntimeMaterialBindsExactPlatformAndCanonicalBundleSpec(t *testing.T) {
	dir := t.TempDir()
	record := writeNodeRuntimeMaterialFixture(t, dir, "24.15.0",
		NodeRuntimeTarget{OS: "linux", Arch: "amd64"},
		NodeRuntimeTarget{OS: "darwin", Arch: "amd64"},
		NodeRuntimeTarget{OS: "windows", Arch: "amd64"})
	material, err := ResolveNodeRuntimeMaterialContext(t.Context(), dir, record.Version,
		record.SHA256, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if material.Artifact != record || material.Digest != "sha256:"+record.SHA256 ||
		material.TargetOS != "linux" || material.TargetArch != "amd64" {
		t.Fatalf("material=%+v", material)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(material.Spec)); err != nil || compact.String() != material.Spec {
		t.Fatalf("spec=%q err=%v", material.Spec, err)
	}
	var spec model.NodeRuntimeSpec
	if err := json.Unmarshal([]byte(material.Spec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "node-runtime" || spec.Version != record.Version ||
		spec.TargetOS != "linux" || spec.TargetArch != "amd64" ||
		spec.BundleLayout != model.NodeRuntimeBundleLayoutV1 || spec.Artifact == nil ||
		spec.Artifact.SHA256 != record.SHA256 || spec.Artifact.Size != record.Size ||
		spec.Artifact.URL != "/v1/artifacts/"+record.SHA256 || spec.Artifact.EnginesNode != "" ||
		spec.Artifact.UpstreamTarball != "" || spec.Artifact.SHA512 != "" {
		t.Fatalf("spec=%+v", spec)
	}
	if _, err := ResolveNodeRuntimeMaterialContext(t.Context(), dir, record.Version,
		record.SHA256, "linux", "riscv64"); err == nil {
		t.Fatal("unsupported Node runtime target was accepted")
	}
	darwin, err := ResolveNodeRuntimeMaterialContext(t.Context(), dir, record.Version,
		record.SHA256, "darwin", "amd64")
	if err != nil {
		t.Fatalf("Darwin Node runtime material was rejected: %v", err)
	}
	var darwinSpec model.NodeRuntimeSpec
	if err := json.Unmarshal([]byte(darwin.Spec), &darwinSpec); err != nil ||
		darwin.TargetOS != "darwin" || darwin.TargetArch != "amd64" ||
		darwinSpec.TargetOS != "darwin" || darwinSpec.TargetArch != "amd64" {
		t.Fatalf("Darwin material=%+v spec=%+v err=%v", darwin, darwinSpec, err)
	}
	windows, err := ResolveNodeRuntimeMaterialContext(t.Context(), dir, record.Version,
		record.SHA256, "windows", "amd64")
	if err != nil {
		t.Fatalf("Windows Node runtime material was rejected: %v", err)
	}
	var windowsSpec model.NodeRuntimeSpec
	if err := json.Unmarshal([]byte(windows.Spec), &windowsSpec); err != nil ||
		windows.TargetOS != "windows" || windows.TargetArch != "amd64" ||
		windowsSpec.TargetOS != "windows" || windowsSpec.TargetArch != "amd64" {
		t.Fatalf("Windows material=%+v spec=%+v err=%v", windows, windowsSpec, err)
	}
}

func TestResolveNodeRuntimeMaterialRejectsLegacyBundleWithoutDarwinTarget(t *testing.T) {
	dir := t.TempDir()
	record := writeNodeRuntimeMaterialFixture(t, dir, "24.15.0",
		NodeRuntimeTarget{OS: "linux", Arch: "amd64"})

	if _, err := ResolveNodeRuntimeMaterialContext(t.Context(), dir, record.Version,
		record.SHA256, "darwin", "arm64"); err == nil {
		t.Fatal("legacy Linux-only Node bundle was advertised as Darwin deployment material")
	}
}

func TestResolveNodeRuntimeMaterialRejectsLegacyBundleWithoutWindowsTarget(t *testing.T) {
	dir := t.TempDir()
	record := writeNodeRuntimeMaterialFixture(t, dir, "24.15.0",
		NodeRuntimeTarget{OS: "linux", Arch: "amd64"},
		NodeRuntimeTarget{OS: "darwin", Arch: "amd64"})

	if _, err := ResolveNodeRuntimeMaterialContext(t.Context(), dir, record.Version,
		record.SHA256, "windows", "amd64"); err == nil {
		t.Fatal("Linux+Darwin Node bundle was advertised as Windows deployment material")
	}
}

func TestResolveOpenClawMaterialValidatesBytesAndBuildsCanonicalSpec(t *testing.T) {
	dir := t.TempDir()
	record := writeMaterialFixture(t, dir, "2026.9.8", "one")
	material, err := ResolveOpenClawMaterial(dir, record.Version, "")
	if err != nil {
		t.Fatal(err)
	}
	if material.Artifact != record || material.Digest != "sha256:"+record.SHA256 ||
		material.Version != record.Version || material.EnginesNode != record.EnginesNode {
		t.Fatalf("material=%+v", material)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(material.Spec)); err != nil || compact.String() != material.Spec {
		t.Fatalf("spec is not compact JSON: %q err=%v", material.Spec, err)
	}
	if strings.Contains(material.Spec, `\u003e`) {
		t.Fatalf("spec HTML-escaped engines range: %s", material.Spec)
	}
	var spec model.OpenClawSpec
	if err := json.Unmarshal([]byte(material.Spec), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Kind != "openclaw" || spec.Version != record.Version || spec.Artifact == nil ||
		spec.Artifact.SHA256 != record.SHA256 || spec.Artifact.Size != record.Size ||
		spec.Artifact.URL != "/v1/artifacts/"+record.SHA256 ||
		spec.Artifact.UpstreamTarball != "" || spec.Artifact.SHA512 != "" {
		t.Fatalf("spec=%+v", spec)
	}
}

func TestResolveOpenClawMaterialContextSharesCatalogPermitAndCancels(t *testing.T) {
	dir := t.TempDir()
	record := writeMaterialFixture(t, dir, "2026.9.8", "permit")
	release, err := acquireCatalogHashPermit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			release()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, resolveErr := ResolveOpenClawMaterialContext(ctx, dir, record.Version, record.SHA256)
		result <- resolveErr
	}()
	select {
	case resolveErr := <-result:
		t.Fatalf("deployment validation bypassed the catalog hash permit: %v", resolveErr)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case resolveErr := <-result:
		if !errors.Is(resolveErr, context.Canceled) {
			t.Fatalf("canceled deployment validation error = %v", resolveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled deployment validation remained blocked on the hash permit")
	}

	release()
	released = true
	material, err := ResolveOpenClawMaterialContext(context.Background(), dir, record.Version, record.SHA256)
	if err != nil || material.Artifact != record {
		t.Fatalf("resolve after permit release = %+v, %v", material, err)
	}
}

func TestValidateStoredArtifactContextRejectsNilAndCanceledContexts(t *testing.T) {
	dir := t.TempDir()
	record := writeMaterialFixture(t, dir, "2026.9.8", "context")
	if err := ValidateStoredArtifactContext(nil, dir, record); err == nil {
		t.Fatal("nil validation context was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateStoredArtifactContext(ctx, dir, record); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled validation error = %v", err)
	}
}

func TestResolveOpenClawMaterialRequiresExactArtifactChoice(t *testing.T) {
	dir := t.TempDir()
	one := writeMaterialFixture(t, dir, "2026.9.8", "one")
	two := writeMaterialFixture(t, dir, "2026.9.8", "two")
	if _, err := ResolveOpenClawMaterial(dir, one.Version, ""); err == nil ||
		!strings.Contains(err.Error(), one.SHA256) || !strings.Contains(err.Error(), two.SHA256) {
		t.Fatalf("ambiguous selection err=%v", err)
	}
	material, err := ResolveOpenClawMaterial(dir, one.Version, two.SHA256)
	if err != nil || material.Artifact.SHA256 != two.SHA256 {
		t.Fatalf("selected material=%+v err=%v", material, err)
	}
	if _, err := ResolveOpenClawMaterial(dir, one.Version, strings.ToUpper(one.SHA256)); err == nil {
		t.Fatal("accepted noncanonical requested digest")
	}
}

func TestResolveOpenClawMaterialIgnoresUnrelatedMalformedCatalogEntry(t *testing.T) {
	dir := t.TempDir()
	record := writeMaterialFixture(t, dir, "2026.9.8", "valid")
	if err := os.WriteFile(filepath.Join(dir, strings.Repeat("f", 64)+".json"),
		[]byte(`{"private":"malformed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	material, err := ResolveOpenClawMaterial(dir, record.Version, record.SHA256)
	if err != nil || material.Artifact.SHA256 != record.SHA256 {
		t.Fatalf("unrelated malformed sidecar blocked valid material: material=%+v err=%v", material, err)
	}
}

func TestResolveOpenClawMaterialRejectsMissingWrongSizedCorruptAndSymlinkTarballs(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, dir string, record Sidecar)
		want   string
	}{
		{name: "missing", mutate: func(t *testing.T, dir string, record Sidecar) {
			if err := os.Remove(filepath.Join(dir, record.SHA256+".tgz")); err != nil {
				t.Fatal(err)
			}
		}, want: "unavailable"},
		{name: "wrong size", mutate: func(t *testing.T, dir string, record Sidecar) {
			if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), []byte("short"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "size"},
		{name: "same size corrupt", mutate: func(t *testing.T, dir string, record Sidecar) {
			bad := bytes.Repeat([]byte{'x'}, int(record.Size))
			if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), bad, 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "does not match"},
		{name: "symlink", mutate: func(t *testing.T, dir string, record Sidecar) {
			path := filepath.Join(dir, record.SHA256+".tgz")
			other := filepath.Join(dir, "outside")
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(other, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, path); err != nil {
				t.Fatal(err)
			}
		}, want: "not a regular file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			record := writeMaterialFixture(t, dir, "2026.9.8", test.name)
			test.mutate(t, dir, record)
			if _, err := ResolveOpenClawMaterial(dir, record.Version, record.SHA256); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want substring %q", err, test.want)
			}
		})
	}
}
