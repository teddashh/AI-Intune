package artifact

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
	"strings"
	"testing"
	"time"
)

func TestCodexMaterialResolvesExecutablePackage(t *testing.T) {
	const version = "0.155.1"
	dir := t.TempDir()
	bundle := codexFixtureBundle(t, version, true)
	record := writeCodexFixtureSidecar(t, dir, version, bundle)
	material, err := ResolveCodexMaterialContext(t.Context(), dir, version, record.SHA256, "linux", "amd64")
	if err != nil || material.TargetOS != "linux" || material.Version != version ||
		!strings.Contains(material.Spec, `"bundle_layout":"codex-bundle:v1"`) ||
		!strings.Contains(material.Spec, `"kind":"codex"`) {
		t.Fatalf("material=%+v err=%v", material, err)
	}
	if err := ValidateCodexBundleTargetsContext(t.Context(), dir, record,
		NodeRuntimeTarget{OS: "linux", Arch: "amd64"},
		NodeRuntimeTarget{OS: "linux", Arch: "arm64"},
		NodeRuntimeTarget{OS: "darwin", Arch: "amd64"},
		NodeRuntimeTarget{OS: "darwin", Arch: "arm64"},
		NodeRuntimeTarget{OS: "windows", Arch: "amd64"},
		NodeRuntimeTarget{OS: "windows", Arch: "arm64"},
	); err != nil {
		t.Fatal(err)
	}
}

func TestCodexMaterialRejectsLinuxPackageWithoutBubblewrap(t *testing.T) {
	const version = "0.155.1"
	dir := t.TempDir()
	bundle := codexFixtureBundle(t, version, false)
	record := writeCodexFixtureSidecar(t, dir, version, bundle)
	if err := ValidateCodexBundleTargetsContext(t.Context(), dir, record,
		NodeRuntimeTarget{OS: "linux", Arch: "amd64"}); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func writeCodexFixtureSidecar(t *testing.T, dir, version string, bundle []byte) Sidecar {
	t.Helper()
	sum := sha256.Sum256(bundle)
	source := sha512.Sum512([]byte("codex-fixture-" + version))
	digest := hex.EncodeToString(sum[:])
	record := Sidecar{
		Name: "codex", Version: version,
		TarballURL:      "https://releases.openai.com/codex/releases/" + version + "/release.json",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(len(bundle)),
		FetchedAt: time.Now().UTC(), FetchedBy: "operator:test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}

func codexFixtureBundle(t *testing.T, version string, linuxBubblewrap bool) []byte {
	t.Helper()
	platforms := []struct{ osName, arch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	}
	var lines []string
	entries := []codexTarEntry{{name: "codex/", mode: 0o755, dir: true}}
	for _, platform := range platforms {
		filename, ok := CodexPackageFilename(platform.osName, platform.arch)
		if !ok {
			t.Fatalf("missing filename for %s/%s", platform.osName, platform.arch)
		}
		includeBubblewrap := linuxBubblewrap || platform.osName != "linux" || platform.arch != "amd64"
		body := codexFixturePackage(t, platform.osName, version, includeBubblewrap)
		sum := sha256.Sum256(body)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+filename)
		entries = append(entries,
			codexTarEntry{name: "codex/" + platform.osName + "-" + platform.arch + "/", mode: 0o755, dir: true},
			codexTarEntry{name: "codex/" + platform.osName + "-" + platform.arch + "/" + filename, mode: 0o644, body: body},
		)
	}
	sums := strings.Join(lines, "\n") + "\n"
	entries = append(entries, codexTarEntry{name: "codex/" + CodexChecksumAssetName(), mode: 0o644, body: []byte(sums)})
	return gzipTar(t, entries)
}

func codexFixturePackage(t *testing.T, targetOS, version string, bubblewrap bool) []byte {
	t.Helper()
	script := []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo codex-cli " + version + "; exit 0; fi\nexit 1\n")
	var entries []codexTarEntry
	for _, relative := range CodexPackageRequiredPaths(targetOS) {
		if !bubblewrap && relative == "codex-resources/bwrap" {
			continue
		}
		body := script
		mode := int64(0o755)
		if relative == "codex-package.json" {
			body = []byte("{\"layout\":\"codex-package\"}\n")
			mode = 0o644
		}
		entries = append(entries, codexTarEntry{name: relative, mode: mode, body: body})
	}
	return gzipTar(t, entries)
}

type codexTarEntry struct {
	name string
	mode int64
	body []byte
	dir  bool
}

func gzipTar(t *testing.T, entries []codexTarEntry) []byte {
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
