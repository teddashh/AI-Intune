package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAntigravityMaterialAcceptsEveryTargetAndRefusesTampering(t *testing.T) {
	dir := t.TempDir()
	body := agyMaterialBundle(t, agyTestVersion, agyTestBuild, nil)
	record := writeAgyMaterialBundle(t, dir, body)
	targets := make([]NodeRuntimeTarget, 0, 6)
	for _, platform := range agyTestPlatforms() {
		targets = append(targets, NodeRuntimeTarget{OS: platform.goos, Arch: platform.arch})
	}
	if err := ValidateAntigravityBundleTargetsContext(t.Context(), dir, record, targets...); err != nil {
		t.Fatal(err)
	}
	for _, platform := range agyTestPlatforms() {
		if err := ValidateAntigravityBundleTargetsContext(t.Context(), dir, record,
			NodeRuntimeTarget{OS: platform.goos, Arch: platform.arch}); err != nil {
			t.Fatalf("%s/%s: %v", platform.goos, platform.arch, err)
		}
	}

	cases := []struct {
		name   string
		mutate func(*testing.T, *agyMaterialDraft)
	}{
		{name: "tampered file", mutate: func(t *testing.T, draft *agyMaterialDraft) {
			draft.digests = map[string]string{"linux-amd64": strings.Repeat("ab", 64)}
		}},
		{name: "manifest version", mutate: func(t *testing.T, draft *agyMaterialDraft) {
			draft.versions["linux-amd64"] = "1.2.15"
		}},
		{name: "missing platform", mutate: func(t *testing.T, draft *agyMaterialDraft) {
			delete(draft.files, "windows-arm64")
			delete(draft.versions, "windows-arm64")
		}},
		{name: "extra member", mutate: func(t *testing.T, draft *agyMaterialDraft) {
			draft.extra = append(draft.extra, tarEntry{name: "antigravity/extra", body: []byte("no")})
		}},
		{name: "symlink", mutate: func(t *testing.T, draft *agyMaterialDraft) {
			draft.extra = append(draft.extra, tarEntry{name: "antigravity/link", link: "antigravity"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			badDir := t.TempDir()
			draft := newAgyMaterialDraft(agyTestVersion)
			tc.mutate(t, draft)
			bad := writeAgyMaterialBundle(t, badDir, draft.bytes(t, agyTestVersion, agyTestBuild))
			err := ValidateAntigravityBundleTargetsContext(t.Context(), badDir, bad, targets...)
			if !errors.Is(err, ErrMetadataInvalid) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

type tarEntry struct {
	name string
	body []byte
	link string
}

type agyMaterialDraft struct {
	files    map[string][]byte
	versions map[string]string
	digests  map[string]string
	extra    []tarEntry
}

func newAgyMaterialDraft(version string) *agyMaterialDraft {
	draft := &agyMaterialDraft{files: map[string][]byte{}, versions: map[string]string{}}
	for _, platform := range agyTestPlatforms() {
		key := platform.goos + "-" + platform.arch
		draft.files[key] = []byte("antigravity-bytes-" + platform.file)
		draft.versions[key] = version
	}
	return draft
}

func (d *agyMaterialDraft) bytes(t *testing.T, version, build string) []byte {
	t.Helper()
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
	for _, platform := range agyTestPlatforms() {
		key := platform.goos + "-" + platform.arch
		if _, ok := d.files[key]; !ok {
			continue
		}
		writeDir("antigravity/" + key)
	}
	for _, platform := range agyTestPlatforms() {
		key := platform.goos + "-" + platform.arch
		body, ok := d.files[key]
		if !ok {
			continue
		}
		base := "antigravity/" + key
		fileName := platform.file
		sum := sha512.Sum512(body)
		encoded := hex.EncodeToString(sum[:])
		if override, ok := d.digests[key]; ok {
			encoded = override
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: base + "/" + fileName, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
		manifest, err := json.Marshal(antigravityManifestDocument{
			Version: d.versions[key],
			URL:     agyTestFileURL(version, build, platform.dir, fileName),
			SHA512:  encoded,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: base + "/manifest.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(manifest)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(manifest); err != nil {
			t.Fatal(err)
		}
	}
	for _, extra := range d.extra {
		header := &tar.Header{Name: extra.name, Mode: 0o644}
		if extra.link != "" {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = extra.link
		} else {
			header.Typeflag = tar.TypeReg
			header.Size = int64(len(extra.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if extra.link == "" {
			if _, err := tw.Write(extra.body); err != nil {
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

func agyMaterialBundle(t *testing.T, version, build string, mutate func(*agyMaterialDraft)) []byte {
	t.Helper()
	draft := newAgyMaterialDraft(version)
	if mutate != nil {
		mutate(draft)
	}
	return draft.bytes(t, version, build)
}

func writeAgyMaterialBundle(t *testing.T, dir string, body []byte) Sidecar {
	t.Helper()
	sum := sha256.Sum256(body)
	record := Sidecar{
		Name: "antigravity", Version: agyTestVersion, SHA256: hex.EncodeToString(sum[:]),
		Size: int64(len(body)),
	}
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}
