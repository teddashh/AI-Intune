package operator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/store"
)

func writeCodexStandardArtifact(t *testing.T, dir, version string) artifact.Sidecar {
	t.Helper()
	body := codexStandardBundle(t, version)
	sum := sha256.Sum256(body)
	source := sha512.Sum512([]byte("codex standard " + version))
	digest := hex.EncodeToString(sum[:])
	record := artifact.Sidecar{
		Name: "codex", Version: version,
		TarballURL:      "https://releases.openai.com/codex/releases/" + version + "/release.json",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(len(body)), FetchedAt: time.Now().UTC(), FetchedBy: "operator:test",
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

func codexStandardBundle(t *testing.T, version string) []byte {
	t.Helper()
	script := []byte("#!/bin/sh\necho codex-cli " + version + "\n")
	platforms := []struct{ osName, arch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	}
	var lines []string
	entries := []codexStandardEntry{{name: "codex/", mode: 0o755, dir: true}}
	for _, platform := range platforms {
		filename, ok := artifact.CodexPackageFilename(platform.osName, platform.arch)
		if !ok {
			t.Fatalf("missing %s/%s", platform.osName, platform.arch)
		}
		var packageEntries []codexStandardEntry
		for _, relative := range artifact.CodexPackageRequiredPaths(platform.osName) {
			body := script
			mode := int64(0o755)
			if relative == "codex-package.json" {
				body = []byte("{}\n")
				mode = 0o644
			}
			packageEntries = append(packageEntries, codexStandardEntry{name: relative, mode: mode, body: body})
		}
		pkg := codexStandardGzip(t, packageEntries)
		sum := sha256.Sum256(pkg)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+filename)
		entries = append(entries,
			codexStandardEntry{name: "codex/" + platform.osName + "-" + platform.arch + "/", mode: 0o755, dir: true},
			codexStandardEntry{name: "codex/" + platform.osName + "-" + platform.arch + "/" + filename, mode: 0o644, body: pkg},
		)
	}
	sums := strings.Join(lines, "\n") + "\n"
	entries = append(entries, codexStandardEntry{name: "codex/" + artifact.CodexChecksumAssetName(), mode: 0o644, body: []byte(sums)})
	return codexStandardGzip(t, entries)
}

type codexStandardEntry struct {
	name string
	mode int64
	body []byte
	dir  bool
}

func codexStandardGzip(t *testing.T, entries []codexStandardEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		flag := byte(tar.TypeReg)
		size := int64(len(entry.body))
		if entry.dir {
			flag = tar.TypeDir
			size = 0
		}
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: flag, Mode: entry.mode, Size: size}); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := tw.Write(entry.body); err != nil {
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
	return output.Bytes()
}

func writeHermesArtifact(t *testing.T, dir, version string) artifact.Sidecar {
	t.Helper()
	body := []byte("Hermes OCI bundle " + version)
	sum := sha256.Sum256(body)
	source := sha512.Sum512([]byte("Hermes OCI source " + version))
	digest := hex.EncodeToString(sum[:])
	record := artifact.Sidecar{
		Name: "hermes-agent", Version: version,
		TarballURL: artifact.ProductionHermesRegistryOrigin + "/v2/" + artifact.HermesImageRepository +
			"/manifests/sha256:" + strings.Repeat("b", 64),
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(len(body)), FetchedAt: time.Now().UTC(), FetchedBy: "operator:test",
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

func TestStandardCatalogPublishesCodexWithoutNodeDependency(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	record := writeCodexStandardArtifact(t, dir, "0.155.1")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256,
	})
	if err != nil || preview.Manifest.ID != "codex" || preview.Manifest.Title != "Codex" ||
		preview.Manifest.Kind != appcatalog.KindApp || preview.Manifest.Source.Catalog != "releases.openai.com" ||
		preview.Manifest.Source.Revision != "rust-v0.155.1" ||
		preview.Manifest.Source.UpstreamURL != "https://releases.openai.com/codex/releases/0.155.1/release.json" ||
		len(preview.Manifest.Platforms) != 6 || len(preview.Manifest.Dependencies) != 0 ||
		preview.EnginesNode != nil || !slices.Equal(preview.Manifest.Provides, []string{"cli.codex"}) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := service.PublishStandardCatalogManifest(t.Context(), standardCatalogPublishRequest(preview, "standard-codex"))
	if err != nil || result.Record.Manifest.ID != "codex" || result.Record.Digest != preview.ManifestDigest {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestStandardCatalogPublishesGrokWithoutNodeDependency(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	record := writeGrokStandardArtifact(t, dir, "1.0.40")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256,
	})
	if err != nil || preview.Manifest.ID != "grok" || preview.Manifest.Title != "Grok" ||
		preview.Manifest.Kind != appcatalog.KindApp || preview.Manifest.Source.Catalog != "registry.npmjs.org" ||
		preview.Manifest.Source.Revision != "1.0.40" || preview.Manifest.Source.License != "Apache-2.0" ||
		preview.Manifest.Source.UpstreamURL != "https://registry.npmjs.org/@xai-official/grok/1.0.40" ||
		len(preview.Manifest.Dependencies) != 0 || preview.EnginesNode != nil ||
		!slices.Equal(preview.Manifest.Provides, []string{"cli.grok"}) ||
		!slices.Equal(preview.Manifest.Platforms, []appcatalog.Platform{
			{OS: "darwin", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"},
			{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"}, {OS: "windows", Arch: "arm64"},
		}) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := service.PublishStandardCatalogManifest(t.Context(), standardCatalogPublishRequest(preview, "standard-grok"))
	if err != nil || result.Record.Manifest.ID != "grok" || result.Record.Digest != preview.ManifestDigest {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256, NodeRuntimeVersion: "24.21.0",
	}); !errors.Is(err, ErrInvalidStandardCatalogPreview) {
		t.Fatalf("Grok accepted Node dependency: %v", err)
	}
}

func TestStandardCatalogPublishesBATServerWithoutNodeDependency(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	record := writeBATServerStandardArtifact(t, dir, "3.2.10")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256,
	})
	if err != nil || preview.Manifest.ID != "bat-server" || preview.Manifest.Title != "BAT Server" ||
		preview.Manifest.Kind != appcatalog.KindApp ||
		preview.Manifest.Source.Catalog != "github.com/tony1223/better-agent-terminal" ||
		preview.Manifest.Source.Revision != "3.2.10" || preview.Manifest.Source.License != "MIT" ||
		preview.Manifest.Source.UpstreamURL != "https://github.com/tony1223/better-agent-terminal/releases/tag/v3.2.10" ||
		len(preview.Manifest.Dependencies) != 0 || preview.EnginesNode != nil ||
		!slices.Equal(preview.Manifest.Provides, []string{"service.bat-server"}) ||
		!slices.Equal(preview.Manifest.Platforms, []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
		}) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := service.PublishStandardCatalogManifest(t.Context(), standardCatalogPublishRequest(preview, "standard-bat-server"))
	if err != nil || result.Record.Manifest.ID != "bat-server" || result.Record.Digest != preview.ManifestDigest {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256, NodeRuntimeVersion: "24.21.0",
	}); !errors.Is(err, ErrInvalidStandardCatalogPreview) {
		t.Fatalf("BAT Server accepted Node dependency: %v", err)
	}
}

func writeBATServerStandardArtifact(t *testing.T, dir, version string) artifact.Sidecar {
	t.Helper()
	body := batServerStandardBundle(t)
	sum := sha256.Sum256(body)
	source := sha512.Sum512([]byte("bat-server standard " + version))
	digest := hex.EncodeToString(sum[:])
	record := artifact.Sidecar{
		Name: "bat-server", Version: version,
		TarballURL:      "https://api.github.com/repos/tony1223/better-agent-terminal/releases/tags/v" + version,
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(len(body)), FetchedAt: time.Now().UTC(), FetchedBy: "operator:test",
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

func batServerStandardBundle(t *testing.T) []byte {
	t.Helper()
	members := map[string][]byte{
		"bat-server/linux-amd64/bat-server.tar.gz": batServerStandardInner(t, "bat-server-linux-x86_64", []byte("amd64-bat")),
		"bat-server/linux-arm64/bat-server.tar.gz": batServerStandardInner(t, "bat-server-linux-aarch64", []byte("arm64-bat")),
	}
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, name := range []string{
		"bat-server/linux-amd64/bat-server.tar.gz",
		"bat-server/linux-arm64/bat-server.tar.gz",
	} {
		body := members[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func batServerStandardInner(t *testing.T, root string, binary []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: root + "/bat-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func writeGrokStandardArtifact(t *testing.T, dir, version string) artifact.Sidecar {
	t.Helper()
	body := grokStandardBundle(t)
	sum := sha256.Sum256(body)
	source := sha512.Sum512([]byte("grok standard " + version))
	digest := hex.EncodeToString(sum[:])
	record := artifact.Sidecar{
		Name: "grok", Version: version,
		TarballURL:      "https://registry.npmjs.org/@xai-official/grok/" + version,
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(len(body)), FetchedAt: time.Now().UTC(), FetchedBy: "operator:test",
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

func grokStandardBundle(t *testing.T) []byte {
	t.Helper()
	payload := []byte("#!/bin/sh\necho grok\n")
	var compressed bytes.Buffer
	writer := brotli.NewWriterLevel(&compressed, brotli.BestSpeed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body := compressed.Bytes()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, platform := range []struct{ osName, arch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	} {
		name, ok := artifact.GrokBundleMember(platform.osName, platform.arch)
		if !ok {
			t.Fatalf("missing %s/%s", platform.osName, platform.arch)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestStandardCatalogPublishesAntigravityWithoutNodeDependency(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	record := writeAntigravityStandardArtifact(t, dir, "1.2.14")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256,
	})
	if err != nil || preview.Manifest.ID != "antigravity" || preview.Manifest.Title != "Antigravity" ||
		preview.Manifest.Kind != appcatalog.KindApp || preview.Manifest.Source.Catalog != "storage.googleapis.com" ||
		preview.Manifest.Source.Revision != "1.2.14" || preview.Manifest.Source.License != "proprietary" ||
		preview.Manifest.Source.UpstreamURL != record.TarballURL ||
		len(preview.Manifest.Dependencies) != 0 || preview.EnginesNode != nil ||
		!slices.Equal(preview.Manifest.Provides, []string{"cli.agy"}) ||
		!slices.Equal(preview.Manifest.Platforms, []appcatalog.Platform{
			{OS: "darwin", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"},
			{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"}, {OS: "windows", Arch: "arm64"},
		}) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := service.PublishStandardCatalogManifest(t.Context(), standardCatalogPublishRequest(preview, "standard-antigravity"))
	if err != nil || result.Record.Manifest.ID != "antigravity" || result.Record.Digest != preview.ManifestDigest {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256, NodeRuntimeVersion: "24.21.0",
	}); !errors.Is(err, ErrInvalidStandardCatalogPreview) {
		t.Fatalf("Antigravity accepted Node dependency: %v", err)
	}
}

func TestStandardCatalogRefusesTamperedAntigravityBundle(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	record := writeAntigravityStandardRecord(t, dir, "1.2.14",
		"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.2.14-4571742832820224/",
		antigravityStandardBundle(t, "1.2.14", "cli_linux_x64.tar.gz"))
	var rejection *store.OperatorRequestError
	if _, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256,
	}); !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeCatalogArtifactMismatch {
		t.Fatalf("preview err=%v", err)
	}
	manifest, err := service.standardManifestFromRecord(record, "")
	if err != nil {
		t.Fatal(err)
	}
	request := CatalogManifestPublishRequest{
		Manifest: manifest, ConfirmPackageID: manifest.ID, ConfirmVersion: manifest.Version,
		PreviewDigest: standardCatalogPreviewDigest(manifest), Reason: "add package to standard store",
		IdempotencyKey: "standard-antigravity-tampered", Actor: verifiedDeploymentActor(),
	}
	if _, err := service.PublishStandardCatalogManifest(t.Context(), request); !errors.As(err, &rejection) ||
		rejection.Code != store.OperatorCodeCatalogArtifactMismatch {
		t.Fatalf("publish err=%v", err)
	}
}

func TestStandardCatalogRefusesAntigravitySourceOutsideReleaseDirectory(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	for _, tarballURL := range []string{
		"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.2.14-4571742832820224/linux-x64/cli_linux_x64.tar.gz",
		"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.2.13-4571742832820224/",
	} {
		record := writeAntigravityStandardRecord(t, dir, "1.2.14", tarballURL, antigravityStandardBundle(t, "1.2.14", ""))
		if _, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
			ArtifactSHA256: record.SHA256,
		}); !errors.Is(err, ErrInvalidStandardCatalogPreview) {
			t.Fatalf("%s: err=%v", tarballURL, err)
		}
	}
}

func writeAntigravityStandardArtifact(t *testing.T, dir, version string) artifact.Sidecar {
	t.Helper()
	return writeAntigravityStandardRecord(t, dir, version,
		"https://storage.googleapis.com/antigravity-public/antigravity-cli/"+version+"-4571742832820224/",
		antigravityStandardBundle(t, version, ""))
}

func writeAntigravityStandardRecord(t *testing.T, dir, version, tarballURL string, body []byte) artifact.Sidecar {
	t.Helper()
	sum := sha256.Sum256(body)
	source := sha512.Sum512([]byte("antigravity standard " + version))
	digest := hex.EncodeToString(sum[:])
	record := artifact.Sidecar{
		Name: "antigravity", Version: version,
		TarballURL:      tarballURL,
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(len(body)), FetchedAt: time.Now().UTC(), FetchedBy: "operator:test",
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

func antigravityStandardBundle(t *testing.T, version, tamperedFile string) []byte {
	t.Helper()
	type platform struct{ osName, arch, dir, file string }
	platforms := []platform{
		{"linux", "amd64", "linux-x64", "cli_linux_x64.tar.gz"},
		{"linux", "arm64", "linux-arm", "cli_linux_arm64.tar.gz"},
		{"darwin", "amd64", "darwin-x64", "cli_mac_x64.tar.gz"},
		{"darwin", "arm64", "darwin-arm", "cli_mac_arm64.tar.gz"},
		{"windows", "amd64", "windows-x64", "cli_windows_x64.exe"},
		{"windows", "arm64", "windows-arm", "cli_windows_arm64.exe"},
	}
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	writeDir := func(name string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	writeDir("antigravity")
	for _, platform := range platforms {
		writeDir("antigravity/" + platform.osName + "-" + platform.arch)
	}
	for _, platform := range platforms {
		body := []byte("antigravity-bytes-" + platform.file)
		sum := sha512.Sum512(body)
		if platform.file == tamperedFile {
			sum = sha512.Sum512([]byte("tampered-" + platform.file))
		}
		base := "antigravity/" + platform.osName + "-" + platform.arch
		if err := tw.WriteHeader(&tar.Header{Name: base + "/" + platform.file, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
		manifest, err := json.Marshal(map[string]string{
			"version": version,
			"url":     "https://storage.googleapis.com/antigravity-public/antigravity-cli/" + version + "-4571742832820224/" + platform.dir + "/" + platform.file,
			"sha512":  hex.EncodeToString(sum[:]),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: base + "/manifest.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(manifest))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(manifest); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestStandardCatalogPublishesHermesWithoutNodeDependency(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	record := writeHermesArtifact(t, dir, "2026.9.7")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256,
	})
	if err != nil || preview.Manifest.ID != "hermes-agent" || preview.Manifest.Title != "Hermes Agent" ||
		preview.Manifest.Kind != appcatalog.KindApp || preview.Manifest.Source.Catalog != "github.com" ||
		preview.Manifest.Source.Revision != "v2026.9.7" || len(preview.Manifest.Dependencies) != 0 ||
		preview.EnginesNode != nil || !slices.Equal(preview.Manifest.Provides, []string{appcatalog.CapabilityAgentRuntime}) ||
		!slices.Equal(preview.Manifest.Conflicts, []string{"openclaw"}) ||
		!slices.Equal(preview.Manifest.ExclusiveGroups, []string{appcatalog.ExclusiveGroupPrimaryAgentRuntime}) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := service.PublishStandardCatalogManifest(t.Context(), standardCatalogPublishRequest(preview, "standard-hermes"))
	if err != nil || result.Record.Manifest.ID != "hermes-agent" || result.Record.Digest != preview.ManifestDigest {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256, NodeRuntimeVersion: "24.21.0",
	}); !errors.Is(err, ErrInvalidStandardCatalogPreview) {
		t.Fatalf("Hermes accepted Node dependency: %v", err)
	}
}

func TestStandardCatalogPublishesNodeThenCompatibleOpenClaw(t *testing.T) {
	service, st, dir, openClawRecord := catalogManifestService(t)
	nodeRecord := writeNodeRuntimeArtifact(t, dir, "24.21.0")

	nodePreview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: nodeRecord.SHA256, NodeRuntimeVersion: "",
	})
	if err != nil || nodePreview.SchemaVersion != StandardCatalogPreviewSchemaVersion ||
		nodePreview.Manifest.ID != "node-runtime" || nodePreview.Manifest.Kind != appcatalog.KindRuntime ||
		nodePreview.Manifest.Source.Catalog != "nodejs.org" || nodePreview.Manifest.Source.Revision != "v24.21.0" ||
		!slices.Equal(nodePreview.Manifest.Platforms, []appcatalog.Platform{
			{OS: "darwin", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"},
			{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"}, {OS: "windows", Arch: "arm64"},
		}) ||
		nodePreview.ManifestDigest == "" || nodePreview.PreviewDigest == "" || nodePreview.EnginesNode != nil ||
		nodePreview.AlreadyPublished {
		t.Fatalf("node preview=%+v err=%v", nodePreview, err)
	}
	nodeRequest := standardCatalogPublishRequest(nodePreview, "standard-node")
	nodePublished, err := service.PublishStandardCatalogManifest(t.Context(), nodeRequest)
	if err != nil || nodePublished.Record.Digest != nodePreview.ManifestDigest || nodePublished.Replayed {
		t.Fatalf("node publish=%+v err=%v", nodePublished, err)
	}

	openClawPreview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: openClawRecord.SHA256, NodeRuntimeVersion: nodeRecord.Version,
	})
	if err != nil || openClawPreview.Manifest.ID != "openclaw" || openClawPreview.Manifest.Kind != appcatalog.KindApp ||
		openClawPreview.Manifest.Source.Catalog != "npmjs.com" || openClawPreview.EnginesNode == nil ||
		len(openClawPreview.Manifest.Dependencies) != 1 || openClawPreview.Manifest.Dependencies[0] != (appcatalog.PackageRef{
		PackageID: "node-runtime", Version: nodeRecord.Version,
	}) {
		t.Fatalf("openclaw preview=%+v err=%v", openClawPreview, err)
	}
	openClawRequest := standardCatalogPublishRequest(openClawPreview, "standard-openclaw")
	openClawPublished, err := service.PublishStandardCatalogManifest(t.Context(), openClawRequest)
	if err != nil || openClawPublished.Record.Digest != openClawPreview.ManifestDigest {
		t.Fatalf("openclaw publish=%+v err=%v", openClawPublished, err)
	}

	again, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: openClawRecord.SHA256, NodeRuntimeVersion: nodeRecord.Version,
	})
	if err != nil || !again.AlreadyPublished || again.ManifestDigest != openClawPublished.Record.Digest {
		t.Fatalf("published preview=%+v err=%v", again, err)
	}
	if _, err := st.ResolveMachineProfile("missing", 1, appcatalog.Platform{OS: "linux", Arch: "amd64"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unexpected profile side effect: %v", err)
	}
}

func TestStandardCatalogRejectsLegacyNodeBundleWithoutDarwinTargets(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	legacy := writeNodeRuntimeArtifactTargets(t, dir, "24.20.0", "linux-amd64", "linux-arm64")

	_, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: legacy.SHA256,
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeCatalogArtifactMismatch {
		t.Fatalf("legacy Linux-only bundle preview error=%v; want %s", err, store.OperatorCodeCatalogArtifactMismatch)
	}
}

func standardCatalogPublishRequest(preview StandardCatalogManifestPreviewResult,
	key string,
) CatalogManifestPublishRequest {
	return CatalogManifestPublishRequest{
		Manifest: preview.Manifest, ConfirmPackageID: preview.Manifest.ID,
		ConfirmVersion: preview.Manifest.Version, PreviewDigest: preview.PreviewDigest,
		Reason: "add package to standard store", IdempotencyKey: key, Actor: verifiedDeploymentActor(),
	}
}

func TestStandardCatalogRejectsMissingOrIncompatibleNodeDependency(t *testing.T) {
	service, _, dir, openClawRecord := catalogManifestService(t)
	for _, version := range []string{"24.21.0", "23.1.0"} {
		node := writeNodeRuntimeArtifact(t, dir, version)
		if version == "23.1.0" {
			request := catalogManifestRequest(node, "not-used")
			request.Manifest = nodeRuntimeManifest(node)
			if _, err := service.PublishCatalogManifest(t.Context(), request); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, version := range []string{"24.21.0", "23.1.0"} {
		_, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
			ArtifactSHA256: openClawRecord.SHA256, NodeRuntimeVersion: version,
		})
		var rejection *store.OperatorRequestError
		if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeCatalogDependencyInvalid {
			t.Fatalf("node=%s err=%v", version, err)
		}
	}
}

func TestStandardCatalogDurablyRejectsConfirmationAndPreviewMismatch(t *testing.T) {
	service, st, dir, _ := catalogManifestService(t)
	node := writeNodeRuntimeArtifact(t, dir, "24.21.0")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: node.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		code string
		edit func(*CatalogManifestPublishRequest)
	}{
		{"confirmation", store.OperatorCodeCatalogConfirmationMismatch, func(r *CatalogManifestPublishRequest) { r.ConfirmVersion = "24.20.0" }},
		{"preview", store.OperatorCodeCatalogPreviewStale, func(r *CatalogManifestPublishRequest) { r.PreviewDigest = "sha256:" + string(make([]byte, 64)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := standardCatalogPublishRequest(preview, "standard-reject-"+test.name)
			test.edit(&request)
			for attempt := 0; attempt < 2; attempt++ {
				_, err := service.PublishStandardCatalogManifest(t.Context(), request)
				var rejection *store.OperatorRequestError
				if !errors.As(err, &rejection) || rejection.Code != test.code || rejection.Replayed != (attempt == 1) {
					t.Fatalf("attempt=%d err=%+v", attempt, err)
				}
			}
			var count int
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, request.IdempotencyKey).Scan(&count); err != nil || count != 1 {
				t.Fatalf("receipt count=%d err=%v", count, err)
			}
		})
	}
}

func TestStandardCatalogReplayNeedsNoArtifactBytes(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	node := writeNodeRuntimeArtifact(t, dir, "24.21.0")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: node.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := standardCatalogPublishRequest(preview, "standard-replay")
	first, err := service.PublishStandardCatalogManifest(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, node.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replay, err := service.PublishStandardCatalogManifest(t.Context(), request)
	if err != nil || !replay.Replayed || replay.Record.Digest != first.Record.Digest ||
		replay.Record.PublishedAt != first.Record.PublishedAt {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestStandardCatalogPreviewRejectsInvalidIdentity(t *testing.T) {
	service, _, _, _ := catalogManifestService(t)
	_, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: "bad", NodeRuntimeVersion: "24.21.0",
	})
	if !errors.Is(err, ErrInvalidStandardCatalogPreview) {
		t.Fatalf("err=%v", err)
	}
	_, err = service.PreviewStandardCatalogManifest(nil, StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: string(make([]byte, 64)),
	})
	if !errors.Is(err, ErrInvalidStandardCatalogPreview) {
		t.Fatalf("nil context err=%v", err)
	}
}
