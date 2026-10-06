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

func TestCodexFetcherPinsOfficialReleaseAndBuildsBundle(t *testing.T) {
	const version = "0.155.1"
	origin := newCodexTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: dir, registryURL: origin.server.URL, codexOriginURL: origin.server.URL,
		client: origin.server.Client(), metadataMax: 1 << 20, artifactMax: 1 << 20,
		metadataTimeout: time.Minute, downloadTimeout: time.Minute, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fetcher.PreviewPlan(t.Context(), "codex", version)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceKind != ArtifactSourceCodex || plan.PolicyVersion != CodexFetchPolicyVersion ||
		plan.Name != "codex" || plan.Version != version || plan.RegistryOrigin != origin.server.URL ||
		plan.EnginesNode != "" || plan.MaxBytes != DefaultCodexBundleMaxBytes ||
		plan.SourcePlan == "" || len(plan.SourcePlan) > MaxArtifactSourcePlanBytes ||
		!ValidCodexSourcePlan(plan.SourcePlan) {
		t.Fatalf("plan=%+v bytes=%d", plan, len(plan.SourcePlan))
	}
	typed, err := decodeCodexSourcePlan(plan.SourcePlan)
	if err != nil || len(typed.Sources) != 6 || typed.ChecksumSHA256 == "" ||
		typed.Sources[0].Filename != "codex-package-x86_64-unknown-linux-musl.tar.gz" ||
		typed.Sources[5].TargetOS != "windows" || typed.Sources[5].TargetArch != "arm64" {
		t.Fatalf("sources=%+v checksum=%s err=%v", typed.Sources, typed.ChecksumSHA256, err)
	}
	record, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err != nil || existed || record.Name != "codex" || record.Version != version {
		t.Fatalf("record=%+v existed=%t err=%v", record, existed, err)
	}
	assertCodexTestBundle(t, filepath.Join(dir, record.SHA256+".tgz"), origin)
	releases, files := origin.counts()
	if releases != 1 || files != 7 {
		t.Fatalf("releases=%d files=%d", releases, files)
	}
	cached, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:retry", nil)
	releasesAfter, filesAfter := origin.counts()
	if err != nil || !existed || cached.SHA256 != record.SHA256 || releasesAfter != releases || filesAfter != files {
		t.Fatalf("cached=%+v existed=%t hits=%d/%d->%d/%d err=%v", cached, existed, releases, files, releasesAfter, filesAfter, err)
	}
}

func TestCodexFetcherRejectsIncompleteRelease(t *testing.T) {
	const version = "0.155.1"
	origin := newCodexTestOrigin(t, version)
	origin.mu.Lock()
	filtered := origin.assets[:0]
	for _, asset := range origin.assets {
		if asset.Name != "codex-package-aarch64-pc-windows-msvc.tar.gz" {
			filtered = append(filtered, asset)
		}
	}
	origin.assets = filtered
	origin.mu.Unlock()
	fetcher := newCodexTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "codex", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestCodexFetcherRejectsGitHubAssetURL(t *testing.T) {
	const version = "0.155.1"
	origin := newCodexTestOrigin(t, version)
	origin.mu.Lock()
	for i := range origin.assets {
		if origin.assets[i].Name == codexChecksumAsset {
			origin.assets[i].BrowserDownloadURL = "https://github.com/openai/codex/releases/download/rust-v" + version + "/" + codexChecksumAsset
		}
	}
	origin.mu.Unlock()
	fetcher := newCodexTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "codex", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, files := origin.counts(); files != 0 {
		t.Fatalf("downloaded %d archives from a rejected manifest", files)
	}
}

func TestCodexFetcherRejectsChecksumDisagreementBeforePackages(t *testing.T) {
	const version = "0.155.1"
	origin := newCodexTestOrigin(t, version)
	origin.mu.Lock()
	body := origin.files[codexChecksumAsset]
	replaced := bytes.Replace(body, []byte(origin.digests["codex-package-x86_64-unknown-linux-musl.tar.gz"]), []byte(strings.Repeat("ab", 32)), 1)
	origin.files[codexChecksumAsset] = replaced
	sum := sha256.Sum256(replaced)
	digest := hex.EncodeToString(sum[:])
	origin.digests[codexChecksumAsset] = digest
	for i := range origin.assets {
		if origin.assets[i].Name == codexChecksumAsset {
			origin.assets[i].Digest = "sha256:" + digest
		}
	}
	origin.mu.Unlock()
	fetcher := newCodexTestFetcher(t, origin)
	plan, err := fetcher.PreviewPlan(t.Context(), "codex", version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("err=%v", err)
	}
	if _, files := origin.counts(); files != 1 {
		t.Fatalf("files=%d, want only the checksum manifest", files)
	}
}

func TestCodexFetcherRejectsPackageChecksumMismatch(t *testing.T) {
	const version = "0.155.1"
	origin := newCodexTestOrigin(t, version)
	origin.mu.Lock()
	origin.files["codex-package-x86_64-apple-darwin.tar.gz"] = []byte("tampered-codex")
	origin.mu.Unlock()
	fetcher := newCodexTestFetcher(t, origin)
	plan, err := fetcher.PreviewPlan(t.Context(), "codex", version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestCodexFetcherRejectsRedirect(t *testing.T) {
	const version = "0.155.1"
	origin := newCodexTestOrigin(t, version)
	origin.mu.Lock()
	origin.redirectRelease = true
	origin.mu.Unlock()
	fetcher := newCodexTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "codex", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("release redirect err=%v", err)
	}
	origin.mu.Lock()
	origin.redirectRelease = false
	origin.redirect = map[string]string{
		"codex-package-x86_64-unknown-linux-musl.tar.gz": "https://github.com/openai/codex/releases/download/rust-v" + version + "/codex-package-x86_64-unknown-linux-musl.tar.gz",
	}
	origin.mu.Unlock()
	plan, err := fetcher.PreviewPlan(t.Context(), "codex", version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); err == nil || errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("package redirect err=%v", err)
	}
}

func TestCodexFetcherRejectsUnpinnedVersions(t *testing.T) {
	const version = "0.155.1"
	origin := newCodexTestOrigin(t, version)
	fetcher := newCodexTestFetcher(t, origin)
	for _, candidate := range []string{"latest", "0.155.1-alpha.1", "0.155.1-beta.1", "v0.155.1", "rust-v0.155.1"} {
		if _, err := fetcher.PreviewPlan(t.Context(), "codex", candidate); !errors.Is(err, ErrInvalidFetchRequest) {
			t.Fatalf("%s err=%v", candidate, err)
		}
	}
	if releases, files := origin.counts(); releases != 0 || files != 0 {
		t.Fatalf("unpinned versions reached origin releases=%d files=%d", releases, files)
	}
}

func TestCodexFetcherRejectsNonProductionOrigin(t *testing.T) {
	_, err := newCodexFetcher(codexFetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"),
		originURL:    "https://github.com",
	})
	if !errors.Is(err, ErrRegistryPolicy) {
		t.Fatalf("err=%v", err)
	}
}

func TestCodexFetcherRejectsDeclaredOversizeArchive(t *testing.T) {
	const version = "0.155.1"
	origin := newCodexTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher, err := newCodexFetcher(codexFetcherConfig{
		artifactsDir: dir, originURL: origin.server.URL, client: origin.server.Client(),
		metadataMax: 1 << 20, sourceMax: 4, checksumMax: 1 << 20, bundleMax: 1 << 20,
		allowHTTP: true, now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fetcher.PreviewPlan(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

type codexTestAsset struct {
	Name               string `json:"name"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type codexTestOrigin struct {
	server          *httptest.Server
	mu              sync.Mutex
	version         string
	assets          []codexTestAsset
	files           map[string][]byte
	digests         map[string]string
	redirect        map[string]string
	redirectRelease bool
	releaseN        int
	fileN           int
}

func newCodexTestOrigin(t *testing.T, version string) *codexTestOrigin {
	t.Helper()
	origin := &codexTestOrigin{
		version: version, files: map[string][]byte{}, digests: map[string]string{},
		redirect: map[string]string{},
	}
	var lines []string
	for _, source := range codexWantedPlatforms() {
		body := []byte("codex-fixture-" + source.TargetOS + "-" + source.TargetArch + "-" + version)
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		origin.files[source.Filename] = body
		origin.digests[source.Filename] = digest
		lines = append(lines, digest+"  "+source.Filename)
		origin.assets = append(origin.assets, codexTestAsset{Name: source.Filename, Digest: "sha256:" + digest})
	}
	extraName := "codex-npm-linux-x64-" + version + ".tgz"
	extraSum := sha256.Sum256([]byte("extra-npm"))
	lines = append(lines, hex.EncodeToString(extraSum[:])+"  "+extraName)
	sums := []byte(strings.Join(lines, "\n") + "\n")
	sum := sha256.Sum256(sums)
	sumsDigest := hex.EncodeToString(sum[:])
	origin.files[codexChecksumAsset] = sums
	origin.digests[codexChecksumAsset] = sumsDigest
	origin.assets = append(origin.assets, codexTestAsset{Name: codexChecksumAsset, Digest: "sha256:" + sumsDigest})
	origin.assets = append(origin.assets, codexTestAsset{
		Name: extraName, Digest: "sha256:" + hex.EncodeToString(extraSum[:]),
		BrowserDownloadURL: "https://github.com/openai/codex/releases/download/rust-v" + version + "/" + extraName,
	})
	origin.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin.mu.Lock()
		defer origin.mu.Unlock()
		prefix := "/codex/releases/" + version + "/"
		if r.URL.Path == prefix+"release.json" {
			origin.releaseN++
			if origin.redirectRelease {
				http.Redirect(w, r, "https://api.github.com/repos/openai/codex/releases/tags/rust-v"+version, http.StatusFound)
				return
			}
			raw, err := json.Marshal(struct {
				TagName string           `json:"tag_name"`
				Assets  []codexTestAsset `json:"assets"`
			}{TagName: "rust-v" + version, Assets: origin.assets})
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
		name := strings.TrimPrefix(r.URL.Path, prefix)
		if location := origin.redirect[name]; location != "" {
			http.Redirect(w, r, location, http.StatusFound)
			return
		}
		body, ok := origin.files[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		origin.fileN++
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	}))
	t.Cleanup(origin.server.Close)
	base := origin.server.URL + "/codex/releases/" + version + "/"
	for i := range origin.assets {
		if origin.assets[i].BrowserDownloadURL == "" {
			origin.assets[i].BrowserDownloadURL = base + origin.assets[i].Name
		}
	}
	return origin
}

func newCodexTestFetcher(t *testing.T, origin *codexTestOrigin) *Fetcher {
	t.Helper()
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"), registryURL: origin.server.URL,
		codexOriginURL: origin.server.URL, client: origin.server.Client(),
		metadataMax: 1 << 20, artifactMax: 1 << 20, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fetcher
}

func (o *codexTestOrigin) counts() (releases, files int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.releaseN, o.fileN
}

func assertCodexTestBundle(t *testing.T, path string, origin *codexTestOrigin) {
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
	want := map[string][]byte{
		"codex/" + codexChecksumAsset: origin.files[codexChecksumAsset],
	}
	for _, source := range codexWantedPlatforms() {
		want["codex/"+source.TargetOS+"-"+source.TargetArch+"/"+source.Filename] = origin.files[source.Filename]
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
		if header.Typeflag == tar.TypeDir {
			if header.Mode != 0o755 {
				t.Fatalf("dir %s mode=%o", header.Name, header.Mode)
			}
			continue
		}
		expected, ok := want[header.Name]
		body, readErr := io.ReadAll(reader)
		if !ok || readErr != nil || !bytes.Equal(body, expected) || header.Mode != 0o644 {
			t.Fatalf("entry=%s mode=%o ok=%t err=%v", header.Name, header.Mode, ok, readErr)
		}
		seen[header.Name] = true
	}
	if len(seen) != len(want) {
		t.Fatalf("bundle files=%v want=%d", seen, len(want))
	}
}
