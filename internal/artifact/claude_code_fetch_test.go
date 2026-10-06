package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaudeCodeFetcherPinsOfficialManifestAndBuildsBundle(t *testing.T) {
	const version = "2.1.278"
	origin := newClaudeCodeTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: dir, registryURL: origin.server.URL, claudeOriginURL: origin.server.URL,
		client: origin.server.Client(), metadataMax: 1 << 20, artifactMax: 1 << 20,
		metadataTimeout: time.Minute, downloadTimeout: time.Minute, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fetcher.PreviewPlan(t.Context(), "claude-code", version)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceKind != ArtifactSourceClaudeCode || plan.PolicyVersion != ClaudeCodeFetchPolicyVersion ||
		plan.Name != "claude-code" || plan.Version != version || plan.RegistryOrigin != origin.server.URL ||
		plan.EnginesNode != "" || plan.MaxBytes != DefaultClaudeCodeBundleMaxBytes ||
		plan.SourcePlan == "" || !ValidClaudeCodeSourcePlan(plan.SourcePlan) {
		t.Fatalf("plan=%+v", plan)
	}
	typed, err := decodeClaudeCodeSourcePlan(plan.SourcePlan)
	if err != nil || len(typed.Sources) != 6 || typed.Sources[0].Platform != "linux-x64" ||
		typed.Sources[4].Platform != "win32-x64" || typed.Sources[5].Filename != "claude.exe" {
		t.Fatalf("sources=%+v err=%v", typed.Sources, err)
	}
	record, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err != nil || existed || record.Name != "claude-code" || record.Version != version {
		t.Fatalf("record=%+v existed=%t err=%v", record, existed, err)
	}
	assertClaudeCodeTestBundle(t, filepath.Join(dir, record.SHA256+".tgz"), version)
	_, before := origin.counts()
	cached, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:retry", nil)
	_, after := origin.counts()
	if err != nil || !existed || cached.SHA256 != record.SHA256 || after != before {
		t.Fatalf("cached=%+v existed=%t hits=%d->%d err=%v", cached, existed, before, after, err)
	}
}

func TestClaudeCodeFetcherRejectsIncompleteManifest(t *testing.T) {
	const version = "2.1.278"
	origin := newClaudeCodeTestOrigin(t, version)
	origin.mu.Lock()
	delete(origin.manifest.Platforms, "win32-arm64")
	origin.mu.Unlock()
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"), registryURL: origin.server.URL,
		claudeOriginURL: origin.server.URL, client: origin.server.Client(),
		metadataMax: 1 << 20, artifactMax: 1 << 20, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetcher.PreviewPlan(t.Context(), "claude-code", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestClaudeCodeFetcherRejectsBinaryChecksumMismatch(t *testing.T) {
	const version = "2.1.278"
	origin := newClaudeCodeTestOrigin(t, version)
	origin.mu.Lock()
	origin.binaries["linux-x64/claude"] = []byte("tampered-claude")
	origin.mu.Unlock()
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"), registryURL: origin.server.URL,
		claudeOriginURL: origin.server.URL, client: origin.server.Client(),
		metadataMax: 1 << 20, artifactMax: 1 << 20, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fetcher.PreviewPlan(t.Context(), "claude-code", version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("err=%v", err)
	}
}

type claudeCodeTestOrigin struct {
	server    *httptest.Server
	mu        sync.Mutex
	manifest  claudeCodeOfficialManifest
	binaries  map[string][]byte
	manifestN int
	binaryN   int
}

func newClaudeCodeTestOrigin(t *testing.T, version string) *claudeCodeTestOrigin {
	t.Helper()
	origin := &claudeCodeTestOrigin{
		manifest: claudeCodeOfficialManifest{Version: version, Platforms: map[string]struct {
			Binary   string `json:"binary"`
			Checksum string `json:"checksum"`
			Size     int64  `json:"size"`
		}{}},
		binaries: map[string][]byte{},
	}
	for _, source := range claudeCodeWantedPlatforms() {
		body := []byte("claude-" + source.Platform + "-" + version)
		sum := sha256.Sum256(body)
		origin.binaries[source.Platform+"/"+source.Filename] = body
		origin.manifest.Platforms[source.Platform] = struct {
			Binary   string `json:"binary"`
			Checksum string `json:"checksum"`
			Size     int64  `json:"size"`
		}{Binary: source.Filename, Checksum: hex.EncodeToString(sum[:]), Size: int64(len(body))}
	}
	origin.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin.mu.Lock()
		defer origin.mu.Unlock()
		prefix := "/claude-code-releases/" + version + "/"
		if r.URL.Path == prefix+"manifest.json" {
			origin.manifestN++
			raw, err := json.Marshal(origin.manifest)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, prefix)
		body, ok := origin.binaries[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		origin.binaryN++
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	t.Cleanup(origin.server.Close)
	return origin
}

func (o *claudeCodeTestOrigin) counts() (manifests, binaries int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.manifestN, o.binaryN
}

func assertClaudeCodeTestBundle(t *testing.T, path, version string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	want := map[string]string{}
	for _, source := range claudeCodeWantedPlatforms() {
		want["claude-code/"+source.TargetOS+"-"+source.TargetArch+"/bin/"+source.Filename] = "claude-" + source.Platform + "-" + version
	}
	seen := map[string]bool{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		expected, ok := want[header.Name]
		if !ok {
			continue
		}
		body, err := io.ReadAll(reader)
		if err != nil || string(body) != expected || header.Mode != 0o755 {
			t.Fatalf("entry=%s body=%q mode=%o err=%v", header.Name, body, header.Mode, err)
		}
		seen[header.Name] = true
	}
	if len(seen) != len(want) {
		t.Fatalf("bundle files=%v want=%v", seen, want)
	}
}
