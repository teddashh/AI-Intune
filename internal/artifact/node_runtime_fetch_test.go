package artifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

type nodeSourceTestEntry struct {
	name     string
	typeflag byte
	mode     int64
	linkname string
	body     []byte
}

type nodeFetchTestOrigin struct {
	server       *httptest.Server
	mu           sync.Mutex
	archives     map[string][]byte
	checksums    []byte
	checksumHits int
	archiveHits  int
}

func newNodeFetchTestOrigin(t *testing.T, version string) *nodeFetchTestOrigin {
	t.Helper()
	origin := &nodeFetchTestOrigin{archives: make(map[string][]byte)}
	for _, target := range []struct{ os, arch string }{
		{"linux", "x64"}, {"linux", "arm64"}, {"darwin", "x64"}, {"darwin", "arm64"},
	} {
		filename := "node-v" + version + "-" + target.os + "-" + target.arch + ".tar.gz"
		origin.archives[filename] = nodeSourceTestArchive(t, version, target.os, target.arch, nil)
	}
	for _, arch := range []string{"x64", "arm64"} {
		filename := "node-v" + version + "-win-" + arch + ".zip"
		origin.archives[filename] = nodeSourceTestZip(t, version, arch)
	}
	origin.refreshChecksums()
	origin.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin.mu.Lock()
		defer origin.mu.Unlock()
		if r.URL.Path == "/dist/v"+version+"/SHASUMS256.txt" {
			origin.checksumHits++
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(origin.checksums)
			return
		}
		filename := strings.TrimPrefix(r.URL.Path, "/dist/v"+version+"/")
		body, ok := origin.archives[filename]
		if !ok || filename == r.URL.Path {
			http.NotFound(w, r)
			return
		}
		origin.archiveHits++
		if strings.HasSuffix(filename, ".zip") {
			w.Header().Set("Content-Type", "application/zip")
		} else {
			w.Header().Set("Content-Type", "application/gzip")
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(origin.server.Close)
	return origin
}

func (o *nodeFetchTestOrigin) refreshChecksums() {
	filenames := []string{}
	for filename := range o.archives {
		filenames = append(filenames, filename)
	}
	// Deliberately reverse upstream order. Preview must produce platform order.
	if len(filenames) == 2 && strings.Contains(filenames[0], "x64") {
		filenames[0], filenames[1] = filenames[1], filenames[0]
	}
	var body strings.Builder
	for _, filename := range filenames {
		digest := sha256.Sum256(o.archives[filename])
		fmt.Fprintf(&body, "%s  %s\n", hex.EncodeToString(digest[:]), filename)
	}
	fmt.Fprintf(&body, "%s  win-x64/node.exe\n", strings.Repeat("0", 64))
	o.checksums = []byte(body.String())
}

func (o *nodeFetchTestOrigin) counts() (int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.checksumHits, o.archiveHits
}

func nodeSourceTestArchive(t *testing.T, version, targetOS, arch string, extra []nodeSourceTestEntry) []byte {
	t.Helper()
	root := "node-v" + version + "-" + targetOS + "-" + arch
	entries := []nodeSourceTestEntry{
		{name: root + "/", typeflag: tar.TypeDir, mode: 0o755},
		{name: root + "/bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: root + "/bin/node", typeflag: tar.TypeReg, mode: 0o755, body: []byte("node-" + targetOS + "-" + arch)},
		{name: root + "/bin/npm", typeflag: tar.TypeSymlink, mode: 0o777,
			linkname: "../lib/node_modules/npm/bin/npm-cli.js"},
		{name: root + "/lib/node_modules/npm/bin/npm-cli.js", typeflag: tar.TypeReg, mode: 0o644,
			body: []byte("npm-" + targetOS + "-" + arch)},
	}
	entries = append(entries, extra...)
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Typeflag: entry.typeflag, Mode: entry.mode,
			Linkname: entry.linkname, Size: int64(len(entry.body)),
		}
		if entry.typeflag != tar.TypeReg && entry.typeflag != tar.TypeRegA {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
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

func nodeSourceTestZip(t *testing.T, version, arch string) []byte {
	t.Helper()
	root := "node-v" + version + "-win-" + arch
	var output bytes.Buffer
	zw := zip.NewWriter(&output)
	for _, entry := range []struct{ name, body string }{
		{root + "/node.exe", "node-win-" + arch},
		{root + "/node_modules/npm/bin/npm-cli.js", "npm-win-" + arch},
	} {
		w, err := zw.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func nodeRuntimeTestFetcher(t *testing.T, dir string, origin *nodeFetchTestOrigin) *NodeRuntimeFetcher {
	t.Helper()
	fetcher, err := newNodeRuntimeFetcher(nodeRuntimeFetcherConfig{
		artifactsDir: dir, originURL: origin.server.URL, client: origin.server.Client(),
		metadataMax: 1 << 20, sourceMax: 1 << 20, bundleMax: 1 << 20,
		metadataTimeout: time.Minute, downloadTimeout: time.Minute, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fetcher
}

func TestNodeRuntimeFetcherPinsBuildsPublishesAndReusesBundle(t *testing.T) {
	const version = "24.21.0"
	origin := newNodeFetchTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := nodeRuntimeTestFetcher(t, dir, origin)
	plan, err := fetcher.PreviewPlan(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PolicyVersion != NodeRuntimeFetchPolicyVersion || plan.Name != "node-runtime" ||
		plan.SourceOrigin != origin.server.URL || len(plan.Sources) != 6 ||
		plan.Sources[0].TargetOS != "linux" || plan.Sources[0].TargetArch != "amd64" ||
		plan.Sources[1].TargetOS != "linux" || plan.Sources[1].TargetArch != "arm64" ||
		plan.Sources[2].TargetOS != "darwin" || plan.Sources[2].TargetArch != "amd64" ||
		plan.Sources[3].TargetOS != "darwin" || plan.Sources[3].TargetArch != "arm64" ||
		plan.Sources[4].TargetOS != "windows" || plan.Sources[4].TargetArch != "amd64" ||
		plan.Sources[5].TargetOS != "windows" || plan.Sources[5].TargetArch != "arm64" ||
		!strings.HasPrefix(plan.SourceIdentity, "sha512-") ||
		!strings.HasPrefix(plan.PreviewDigest, "sha256:") {
		t.Fatalf("plan=%+v", plan)
	}
	var progress []FetchProgress
	record, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:test", func(update FetchProgress) error {
		progress = append(progress, update)
		return nil
	})
	if err != nil || existed || record.Name != "node-runtime" || record.Version != version ||
		record.TarballURL != plan.ChecksumURL || record.SHA512Integrity != plan.SourceIdentity ||
		record.EnginesNode != "" || record.Size <= 0 || !ValidSHA256Hex(record.SHA256) {
		t.Fatalf("record=%+v existed=%t err=%v", record, existed, err)
	}
	if len(progress) != 4 || progress[0].Phase != FetchPhaseDownloading ||
		progress[1].Phase != FetchPhaseVerifying || progress[2].Phase != FetchPhasePublishing ||
		progress[3].Phase != FetchPhasePublishing {
		t.Fatalf("progress=%+v", progress)
	}
	entry, err := InspectCatalogEntry(t.Context(), dir, record.SHA256)
	if err != nil || entry.Status != CatalogReady {
		t.Fatalf("entry=%+v err=%v", entry, err)
	}
	if err := ValidateNodeRuntimeBundleTargetsContext(t.Context(), dir, record,
		NodeRuntimeTarget{OS: "linux", Arch: "amd64"},
		NodeRuntimeTarget{OS: "linux", Arch: "arm64"},
		NodeRuntimeTarget{OS: "darwin", Arch: "amd64"},
		NodeRuntimeTarget{OS: "darwin", Arch: "arm64"},
		NodeRuntimeTarget{OS: "windows", Arch: "amd64"},
		NodeRuntimeTarget{OS: "windows", Arch: "arm64"}); err != nil {
		t.Fatalf("published bundle target validation failed: %v", err)
	}
	assertNodeRuntimeTestBundle(t, filepath.Join(dir, record.SHA256+".tgz"), version)
	_, beforeArchives := origin.counts()
	cached, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:retry", nil)
	_, afterArchives := origin.counts()
	if err != nil || !existed || cached.SHA256 != record.SHA256 || afterArchives != beforeArchives {
		t.Fatalf("cached=%+v existed=%t archives=%d->%d err=%v", cached, existed, beforeArchives, afterArchives, err)
	}
}

func TestFetcherDispatchesNodeRuntimeWithoutSerializingPrivateSourcePlan(t *testing.T) {
	const version = "24.21.0"
	origin := newNodeFetchTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: dir, registryURL: origin.server.URL, nodeOriginURL: origin.server.URL,
		client: origin.server.Client(), metadataMax: 1 << 20, artifactMax: 1 << 20,
		metadataTimeout: time.Minute, downloadTimeout: time.Minute, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fetcher.PreviewPlan(t.Context(), "node-runtime", version)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceKind != ArtifactSourceNode || plan.PolicyVersion != NodeRuntimeFetchPolicyVersion ||
		plan.SourcePlan == "" || plan.RegistryOrigin != origin.server.URL || plan.EnginesNode != "" {
		t.Fatalf("generic node plan=%+v", plan)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "source_plan") || strings.Contains(string(raw), "linux-x64") ||
		strings.Contains(string(raw), "SHASUMS256.txt") {
		t.Fatalf("preview serialized private source plan: %s", raw)
	}
	// Durable operations have their own creation timestamp. The private typed
	// plan remains authoritative for the exact upstream preview timestamp.
	plan.PreviewedAt = plan.PreviewedAt.Add(time.Minute)
	record, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:generic", nil)
	if err != nil || existed || record.Name != "node-runtime" || record.Version != version ||
		record.SHA512Integrity != plan.SHA512Integrity {
		t.Fatalf("record=%+v existed=%t err=%v", record, existed, err)
	}
	assertNodeRuntimeTestBundle(t, filepath.Join(dir, record.SHA256+".tgz"), version)
}

func TestValidNodeRuntimeSourcePlanRequiresTypedCanonicalJSON(t *testing.T) {
	const version = "24.21.0"
	origin := newNodeFetchTestOrigin(t, version)
	fetcher := nodeRuntimeTestFetcher(t, filepath.Join(t.TempDir(), "artifacts"), origin)
	plan, err := fetcher.PreviewPlan(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := marshalCompactNoEscape(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidNodeRuntimeSourcePlan(string(raw)) {
		t.Fatalf("canonical source plan rejected: %s", raw)
	}
	for _, invalid := range []string{
		"", "{}", string(raw) + "\n",
		strings.Replace(string(raw), `"preview_digest":`, `"unknown":true,"preview_digest":`, 1),
		`{"version":"24.21.0","policy_version":"node-runtime-official-bundle:v1"}`,
	} {
		if ValidNodeRuntimeSourcePlan(invalid) {
			t.Fatalf("invalid source plan accepted: %q", invalid)
		}
	}
}

func TestNodeRuntimeBundleIsDeterministicAcrossCatalogs(t *testing.T) {
	const version = "24.21.0"
	origin := newNodeFetchTestOrigin(t, version)
	firstFetcher := nodeRuntimeTestFetcher(t, filepath.Join(t.TempDir(), "a"), origin)
	plan, err := firstFetcher.PreviewPlan(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := firstFetcher.FetchExact(t.Context(), plan, "operator:first", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondFetcher := nodeRuntimeTestFetcher(t, filepath.Join(t.TempDir(), "b"), origin)
	second, _, err := secondFetcher.FetchExact(t.Context(), plan, "operator:second", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 || first.Size != second.Size {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestNodeRuntimeFetcherRejectsChangedOrUnsafeSources(t *testing.T) {
	const version = "24.21.0"
	t.Run("checksum mismatch", func(t *testing.T) {
		origin := newNodeFetchTestOrigin(t, version)
		fetcher := nodeRuntimeTestFetcher(t, filepath.Join(t.TempDir(), "artifacts"), origin)
		plan, err := fetcher.PreviewPlan(t.Context(), version)
		if err != nil {
			t.Fatal(err)
		}
		origin.mu.Lock()
		origin.archives[plan.Sources[0].Filename] = append(origin.archives[plan.Sources[0].Filename], 'x')
		origin.mu.Unlock()
		if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrIntegrityMismatch) {
			t.Fatalf("error=%v", err)
		}
		assertNoNodeRuntimePublishedArtifacts(t, fetcher.artifactsDir)
	})

	for _, test := range []struct {
		name  string
		extra nodeSourceTestEntry
	}{
		{name: "traversal", extra: nodeSourceTestEntry{
			name: "node-v" + version + "-linux-x64/../../escape", typeflag: tar.TypeReg, mode: 0o644, body: []byte("x"),
		}},
		{name: "hardlink", extra: nodeSourceTestEntry{
			name: "node-v" + version + "-linux-x64/hard", typeflag: tar.TypeLink, mode: 0o644,
			linkname: "node-v" + version + "-linux-x64/bin/node",
		}},
		{name: "symlink escape", extra: nodeSourceTestEntry{
			name: "node-v" + version + "-linux-x64/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "../../escape",
		}},
		{name: "file replaces ancestor", extra: nodeSourceTestEntry{
			name: "node-v" + version + "-linux-x64/lib", typeflag: tar.TypeReg, mode: 0o644, body: []byte("x"),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			origin := newNodeFetchTestOrigin(t, version)
			filename := "node-v" + version + "-linux-x64.tar.gz"
			origin.mu.Lock()
			origin.archives[filename] = nodeSourceTestArchive(t, version, "linux", "x64", []nodeSourceTestEntry{test.extra})
			origin.refreshChecksums()
			origin.mu.Unlock()
			fetcher := nodeRuntimeTestFetcher(t, filepath.Join(t.TempDir(), "artifacts"), origin)
			plan, err := fetcher.PreviewPlan(t.Context(), version)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrMetadataInvalid) {
				t.Fatalf("error=%v", err)
			}
			assertNoNodeRuntimePublishedArtifacts(t, fetcher.artifactsDir)
		})
	}
}

func TestNodeRuntimeFetcherRejectsLinuxArchiveMasqueradingAsDarwin(t *testing.T) {
	const version = "24.21.0"
	origin := newNodeFetchTestOrigin(t, version)
	filename := "node-v" + version + "-darwin-arm64.tar.gz"
	origin.mu.Lock()
	origin.archives[filename] = nodeSourceTestArchive(t, version, "linux", "arm64", nil)
	origin.refreshChecksums()
	origin.mu.Unlock()
	fetcher := nodeRuntimeTestFetcher(t, filepath.Join(t.TempDir(), "artifacts"), origin)
	plan, err := fetcher.PreviewPlan(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("Linux archive accepted as Darwin material: %v", err)
	}
	assertNoNodeRuntimePublishedArtifacts(t, fetcher.artifactsDir)
}

func TestNodeRuntimeFetcherRejectsTarMasqueradingAsWindowsZip(t *testing.T) {
	const version = "24.21.0"
	origin := newNodeFetchTestOrigin(t, version)
	filename := "node-v" + version + "-win-x64.zip"
	origin.mu.Lock()
	origin.archives[filename] = nodeSourceTestArchive(t, version, "linux", "x64", nil)
	origin.refreshChecksums()
	origin.mu.Unlock()
	fetcher := nodeRuntimeTestFetcher(t, filepath.Join(t.TempDir(), "artifacts"), origin)
	plan, err := fetcher.PreviewPlan(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("tar.gz accepted as Windows zip material: %v", err)
	}
	assertNoNodeRuntimePublishedArtifacts(t, fetcher.artifactsDir)
}

func TestNodeRuntimeFetcherRejectsIncompleteMetadataAndTamperedPlan(t *testing.T) {
	const version = "24.21.0"
	origin := newNodeFetchTestOrigin(t, version)
	origin.mu.Lock()
	lines := strings.Split(string(origin.checksums), "\n")
	origin.checksums = []byte(lines[0] + "\n")
	origin.mu.Unlock()
	fetcher := nodeRuntimeTestFetcher(t, filepath.Join(t.TempDir(), "artifacts"), origin)
	if _, err := fetcher.PreviewPlan(t.Context(), version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("incomplete checksum error=%v", err)
	}
	origin.mu.Lock()
	origin.refreshChecksums()
	origin.mu.Unlock()
	plan, err := fetcher.PreviewPlan(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	plan.Sources[0].SHA256 = strings.Repeat("0", 64)
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("tampered plan error=%v", err)
	}
	for _, invalid := range []string{"", "latest", "24.01.0", "v24.21.0", "24.21.0-rc.1"} {
		if _, err := fetcher.PreviewPlan(t.Context(), invalid); !errors.Is(err, ErrInvalidFetchRequest) {
			t.Fatalf("version=%q error=%v", invalid, err)
		}
	}
}

func TestNodeRuntimeFetcherReconcilesKnownTempsOnly(t *testing.T) {
	const version = "24.21.0"
	origin := newNodeFetchTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := nodeRuntimeTestFetcher(t, dir, origin)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, nodeRuntimeSourceTempPrefix+"old.tmp")
	keep := filepath.Join(dir, "operator-file")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(time.Minute)
	removed, err := fetcher.ReconcileStaleTemps(cutoff)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d error=%v", removed, err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp remains: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("operator file removed: %v", err)
	}
}

func assertNodeRuntimeTestBundle(t *testing.T, filename, version string) {
	t.Helper()
	input, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	want := map[string]string{
		"node-runtime/linux-amd64/bin/node":                              "node-linux-x64",
		"node-runtime/linux-amd64/lib/node_modules/npm/bin/npm-cli.js":   "npm-linux-x64",
		"node-runtime/linux-arm64/bin/node":                              "node-linux-arm64",
		"node-runtime/linux-arm64/lib/node_modules/npm/bin/npm-cli.js":   "npm-linux-arm64",
		"node-runtime/darwin-amd64/bin/node":                             "node-darwin-x64",
		"node-runtime/darwin-amd64/lib/node_modules/npm/bin/npm-cli.js":  "npm-darwin-x64",
		"node-runtime/darwin-arm64/bin/node":                             "node-darwin-arm64",
		"node-runtime/darwin-arm64/lib/node_modules/npm/bin/npm-cli.js":  "npm-darwin-arm64",
		"node-runtime/windows-amd64/bin/node.exe":                        "node-win-x64",
		"node-runtime/windows-amd64/lib/node_modules/npm/bin/npm-cli.js": "npm-win-x64",
		"node-runtime/windows-arm64/bin/node.exe":                        "node-win-arm64",
		"node-runtime/windows-arm64/lib/node_modules/npm/bin/npm-cli.js": "npm-win-arm64",
	}
	seen := make(map[string]bool)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(header.Name, "node-v"+version) {
			t.Fatalf("upstream root leaked into bundle: %s", header.Name)
		}
		if expected, ok := want[header.Name]; ok {
			body, err := io.ReadAll(reader)
			if err != nil || string(body) != expected {
				t.Fatalf("entry=%s body=%q err=%v", header.Name, body, err)
			}
			if (strings.HasSuffix(header.Name, "/bin/node") || strings.HasSuffix(header.Name, "/bin/node.exe")) && header.Mode != 0o755 {
				t.Fatalf("node mode=%o", header.Mode)
			}
			seen[header.Name] = true
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("bundle files=%v", seen)
	}
}

func assertNoNodeRuntimePublishedArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tgz") || strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("published artifact remains: %s", entry.Name())
		}
	}
}

func TestNodeRuntimeFetcherContextCancellationStopsBuild(t *testing.T) {
	const version = "24.21.0"
	origin := newNodeFetchTestOrigin(t, version)
	fetcher := nodeRuntimeTestFetcher(t, filepath.Join(t.TempDir(), "artifacts"), origin)
	plan, err := fetcher.PreviewPlan(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := fetcher.FetchExact(ctx, plan, "operator:test", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
