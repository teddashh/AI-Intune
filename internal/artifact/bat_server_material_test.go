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

func TestBATServerMaterialResolvesPinnedBinaryHash(t *testing.T) {
	const version = "3.2.10"
	dir := t.TempDir()
	amd64Inner := batServerFixtureAssetArchive(t, "bat-server-linux-x86_64", []byte("amd64-binary-bytes"))
	arm64Inner := batServerFixtureAssetArchive(t, "bat-server-linux-aarch64", []byte("arm64-binary-bytes"))
	bundle := batMaterialBundle(t, map[string][]byte{
		"bat-server/linux-amd64/bat-server.tar.gz": amd64Inner,
		"bat-server/linux-arm64/bat-server.tar.gz": arm64Inner,
	})
	record := writeBATServerFixtureSidecar(t, dir, version, bundle)
	amd64Sum := sha256.Sum256([]byte("amd64-binary-bytes"))
	arm64Sum := sha256.Sum256([]byte("arm64-binary-bytes"))
	amd64Hash := hex.EncodeToString(amd64Sum[:])
	arm64Hash := hex.EncodeToString(arm64Sum[:])

	material, err := ResolveBATServerMaterialContext(t.Context(), dir, version, record.SHA256, "linux", "amd64")
	if err != nil || material.TargetOS != "linux" || material.TargetArch != "amd64" ||
		material.BinarySHA256 != amd64Hash || material.Version != version ||
		!strings.Contains(material.Spec, `"kind":"bat-server"`) ||
		!strings.Contains(material.Spec, `"bundle_layout":"bat-server-bundle:v1"`) ||
		!strings.Contains(material.Spec, `"binary_sha256":"`+amd64Hash+`"`) {
		t.Fatalf("material=%+v err=%v", material, err)
	}
	arm, err := ResolveBATServerMaterialContext(t.Context(), dir, version, record.SHA256, "linux", "arm64")
	if err != nil || arm.TargetArch != "arm64" || arm.BinarySHA256 != arm64Hash {
		t.Fatalf("arm=%+v err=%v", arm, err)
	}
	if strings.Contains(arm.Spec, amd64Hash) {
		t.Fatalf("arm64 spec carries the amd64 hash: %s", arm.Spec)
	}
	for _, target := range [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"windows", "amd64"}, {"windows", "arm64"}, {"linux", "386"}} {
		if _, err := ResolveBATServerMaterialContext(t.Context(), dir, version, record.SHA256, target[0], target[1]); err == nil {
			t.Fatalf("accepted %s/%s", target[0], target[1])
		}
	}
}

func TestBATServerInstalledBinaryPathsAreLiteral(t *testing.T) {
	amd64, ok := model.BATServerInstalledBinary("amd64")
	if !ok || amd64 != "bat-server-linux-x86_64/bat-server" {
		t.Fatalf("amd64=%q ok=%t", amd64, ok)
	}
	arm64, ok := model.BATServerInstalledBinary("arm64")
	if !ok || arm64 != "bat-server-linux-aarch64/bat-server" {
		t.Fatalf("arm64=%q ok=%t", arm64, ok)
	}
	for _, arch := range []string{"386", "amd64v2", "x86_64", "aarch64"} {
		if _, ok := model.BATServerInstalledBinary(arch); ok {
			t.Fatalf("accepted arch %s", arch)
		}
	}
}

func TestHashBATServerBinaryRejectsMissingNonExecutableAndSymlink(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		body := batServerRawArchive(t, "bat-server-linux-x86_64/README.md", 0o644, []byte("notes"), tar.TypeReg)
		if _, err := hashBATServerBinary(t.Context(), bytes.NewReader(gzipBytes(t, body)), "amd64", 1<<20); !errors.Is(err, ErrMetadataInvalid) || !strings.Contains(err.Error(), "lacks") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("not executable", func(t *testing.T) {
		body := batServerRawArchive(t, "bat-server-linux-x86_64/bat-server", 0o644, []byte("binary"), tar.TypeReg)
		if _, err := hashBATServerBinary(t.Context(), bytes.NewReader(gzipBytes(t, body)), "amd64", 1<<20); !errors.Is(err, ErrMetadataInvalid) || !strings.Contains(err.Error(), "not executable") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		payload := []byte("symlink-body-not-empty")
		raw := batServerRawArchive(t, "bat-server-linux-x86_64/bat-server", 0o755, payload, tar.TypeReg)
		patched := patchBATServerTypeflag(t, raw, "bat-server-linux-x86_64/bat-server", tar.TypeSymlink, payload)
		header := readBATServerTarHeader(t, patched, "bat-server-linux-x86_64/bat-server")
		if header.Typeflag != tar.TypeSymlink || header.Size == 0 {
			t.Fatalf("fixture type=%q size=%d", header.Typeflag, header.Size)
		}
		_, err := hashBATServerBinary(t.Context(), bytes.NewReader(gzipBytes(t, patched)), "amd64", 1<<20)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		payload := []byte("hardlink-body-not-empty")
		raw := batServerRawArchive(t, "bat-server-linux-aarch64/bat-server", 0o755, payload, tar.TypeReg)
		patched := patchBATServerTypeflag(t, raw, "bat-server-linux-aarch64/bat-server", tar.TypeLink, payload)
		header := readBATServerTarHeader(t, patched, "bat-server-linux-aarch64/bat-server")
		if header.Typeflag != tar.TypeLink || header.Size == 0 {
			t.Fatalf("fixture type=%q size=%d", header.Typeflag, header.Size)
		}
		_, err := hashBATServerBinary(context.Background(), bytes.NewReader(gzipBytes(t, patched)), "arm64", 1<<20)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err=%v", err)
		}
	})
}

func writeBATServerFixtureSidecar(t *testing.T, dir, version string, bundle []byte) Sidecar {
	t.Helper()
	sum := sha256.Sum256(bundle)
	source := sha512.Sum512([]byte("bat-server-fixture-" + version))
	digest := hex.EncodeToString(sum[:])
	record := Sidecar{
		Name: "bat-server", Version: version,
		TarballURL:      "https://api.github.com/repos/tony1223/better-agent-terminal/releases/tags/v" + version,
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(len(bundle)),
		FetchedAt: time.Now().UTC(), FetchedBy: "operator:test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}

func batMaterialBundle(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, name := range []string{
		"bat-server/linux-amd64/bat-server.tar.gz",
		"bat-server/linux-arm64/bat-server.tar.gz",
	} {
		body, ok := members[name]
		if !ok {
			t.Fatalf("missing literal member %s", name)
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

func batServerRawArchive(t *testing.T, name string, mode int64, body []byte, typeflag byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	dir := name
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		dir = name[:slash+1]
	}
	if err := tw.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: typeflag, Mode: mode, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func readBATServerTarHeader(t *testing.T, raw []byte, name string) *tar.Header {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		header, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			return header
		}
	}
}

func patchBATServerTypeflag(t *testing.T, raw []byte, name string, typeflag byte, body []byte) []byte {
	t.Helper()
	out := append([]byte(nil), raw...)
	for off := 0; off+512 <= len(out); {
		hdr := out[off : off+512]
		if bytes.Equal(hdr, make([]byte, 512)) {
			break
		}
		size := batTarOctal(t, hdr[124:136])
		entryName := batCString(hdr[:100])
		dataAt := off + 512
		padded := int((size + 511) &^ 511)
		if dataAt+padded > len(out) {
			t.Fatalf("tar member %q overruns the archive", entryName)
		}
		if entryName == name {
			if int(size) != len(body) || size == 0 || !bytes.Equal(out[dataAt:dataAt+len(body)], body) {
				t.Fatalf("tar member %q body missing before typeflag patch", name)
			}
			hdr[156] = typeflag
			batPutTarChecksum(hdr)
			return out
		}
		off = dataAt + padded
	}
	t.Fatalf("tar member %s not found", name)
	return nil
}

func batCString(field []byte) string {
	if i := bytes.IndexByte(field, 0); i >= 0 {
		field = field[:i]
	}
	return string(field)
}

func batTarOctal(t *testing.T, field []byte) int64 {
	t.Helper()
	s := strings.Trim(string(field), " \x00")
	n, err := parseOctal(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func parseOctal(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, errors.New("bad octal")
		}
		n = n*8 + int64(c-'0')
	}
	return n, nil
}

func batPutTarChecksum(hdr []byte) {
	for i := 148; i < 156; i++ {
		hdr[i] = ' '
	}
	var sum int
	for _, b := range hdr {
		sum += int(b)
	}
	formatted := []byte("000000\x00 ")
	value := sum
	for i := 5; i >= 0; i-- {
		formatted[i] = byte('0' + value%8)
		value /= 8
	}
	copy(hdr[148:156], formatted)
}
