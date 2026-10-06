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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

func TestGrokMaterialResolvesDecompressibleMember(t *testing.T) {
	const version = "1.0.40"
	dir := t.TempDir()
	bundle := grokFixtureBundle(t, map[string][]byte{
		"linux/amd64":   brotliPayload(t, []byte("linux-amd64")),
		"linux/arm64":   brotliPayload(t, []byte("linux-arm64")),
		"darwin/amd64":  brotliPayload(t, []byte("darwin-amd64")),
		"darwin/arm64":  brotliPayload(t, []byte("darwin-arm64")),
		"windows/amd64": brotliPayload(t, []byte("windows-amd64")),
		"windows/arm64": brotliPayload(t, []byte("windows-arm64")),
	})
	record := writeGrokFixtureSidecar(t, dir, version, bundle)
	material, err := ResolveGrokMaterialContext(t.Context(), dir, version, record.SHA256, "linux", "amd64")
	if err != nil || material.Artifact.Name != "grok" || material.Version != version ||
		material.Artifact.Size != record.Size || material.Digest != "sha256:"+record.SHA256 ||
		material.TargetOS != "linux" || material.TargetArch != "amd64" ||
		!strings.Contains(material.Spec, `"kind":"grok"`) ||
		!strings.Contains(material.Spec, `"bundle_layout":"grok-bundle:v1"`) ||
		!strings.Contains(material.Spec, `"/v1/artifacts/`+record.SHA256+`"`) {
		t.Fatalf("material=%+v err=%v", material, err)
	}
	if err := ValidateGrokBundleTargetsContext(t.Context(), dir, record,
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

func TestGrokMaterialRejectsUndecompressibleMember(t *testing.T) {
	const version = "1.0.40"
	valid := brotliPayload(t, []byte("grok"))
	if len(valid) < 2 {
		t.Fatal("compressed payload is too small to truncate")
	}
	truncated := valid[:len(valid)-1]
	cases := []struct {
		name    string
		body    []byte
		wantErr error
	}{
		{name: "truncated", body: truncated, wantErr: ErrMetadataInvalid},
		{name: "not brotli", body: []byte("this is not brotli"), wantErr: ErrMetadataInvalid},
		{name: "oversize", body: brotliZeros(t, MaxGrokBinaryBytes+1), wantErr: ErrArtifactTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bundle := grokFixtureBundle(t, map[string][]byte{"linux/amd64": tc.body})
			record := writeGrokFixtureSidecar(t, dir, version, bundle)
			err := ValidateGrokBundleTargetsContext(t.Context(), dir, record,
				NodeRuntimeTarget{OS: "linux", Arch: "amd64"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestGrokBundleLayoutPaths(t *testing.T) {
	member, ok := GrokBundleMember("linux", "amd64")
	if !ok || member != "grok/linux-amd64/bin/grok.br" {
		t.Fatalf("linux member=%q ok=%t", member, ok)
	}
	member, ok = GrokBundleMember("windows", "arm64")
	if !ok || member != "grok/windows-arm64/bin/grok.exe.br" {
		t.Fatalf("windows member=%q ok=%t", member, ok)
	}
	command, ok := GrokCommandRelative("darwin")
	if !ok || command != "bin/grok" {
		t.Fatalf("darwin command=%q ok=%t", command, ok)
	}
	command, ok = GrokCommandRelative("windows")
	if !ok || command != "bin/grok.exe" {
		t.Fatalf("windows command=%q ok=%t", command, ok)
	}
	if _, ok := GrokBundleMember("linux", "386"); ok {
		t.Fatal("accepted an unsupported target")
	}
}

func writeGrokFixtureSidecar(t *testing.T, dir, version string, bundle []byte) Sidecar {
	t.Helper()
	sum := sha256.Sum256(bundle)
	source := sha512.Sum512([]byte("grok-fixture-" + version))
	digest := hex.EncodeToString(sum[:])
	record := Sidecar{
		Name: "grok", Version: version,
		TarballURL:      "https://registry.npmjs.org/@xai-official/grok/" + version,
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

func grokFixtureBundle(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "grok/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"} {
		body, ok := members[platform]
		if !ok {
			continue
		}
		osName, arch, _ := strings.Cut(platform, "/")
		name, ok := GrokBundleMember(osName, arch)
		if !ok {
			t.Fatalf("missing member for %s", platform)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
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

func brotliPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := brotli.NewWriterLevel(&buf, brotli.BestSpeed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func brotliZeros(t *testing.T, n int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := brotli.NewWriterLevel(&buf, brotli.BestSpeed)
	if _, err := io.Copy(writer, &zeroReader{n: n}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.n {
		p = p[:z.n]
	}
	for i := range p {
		p[i] = 0
	}
	z.n -= int64(len(p))
	return len(p), nil
}
