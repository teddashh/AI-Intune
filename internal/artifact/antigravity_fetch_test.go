package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
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

const (
	agyTestVersion = "1.2.14"
	agyTestBuild   = "4571742832820224"
)

type agyTestPlatform struct {
	platform string
	dir      string
	file     string
	goos     string
	arch     string
}

func agyTestPlatforms() []agyTestPlatform {
	return []agyTestPlatform{
		{"linux_amd64", "linux-x64", "cli_linux_x64.tar.gz", "linux", "amd64"},
		{"linux_arm64", "linux-arm", "cli_linux_arm64.tar.gz", "linux", "arm64"},
		{"darwin_amd64", "darwin-x64", "cli_mac_x64.tar.gz", "darwin", "amd64"},
		{"darwin_arm64", "darwin-arm", "cli_mac_arm64.tar.gz", "darwin", "arm64"},
		{"windows_amd64", "windows-x64", "cli_windows_x64.exe", "windows", "amd64"},
		{"windows_arm64", "windows-arm", "cli_windows_arm64.exe", "windows", "arm64"},
	}
}

func agyTestReleaseDirectory(version, build string) string {
	return "https://storage.googleapis.com/antigravity-public/antigravity-cli/" + version + "-" + build + "/"
}

func agyTestFileURL(version, build, dir, file string) string {
	return "https://storage.googleapis.com/antigravity-public/antigravity-cli/" + version + "-" + build + "/" + dir + "/" + file
}

type agyTestManifest struct {
	version  string
	url      string
	sha      string
	raw      string
	status   int
	encoding string
}

type agyTestOrigin struct {
	server       *httptest.Server
	mu           sync.Mutex
	docs         map[string]agyTestManifest
	bodies       map[string][]byte
	manifestHits int
	fileHits     int
	failOnFile   bool
}

func newAgyTestOrigin(t *testing.T, version, build string) *agyTestOrigin {
	t.Helper()
	origin := &agyTestOrigin{
		docs:   map[string]agyTestManifest{},
		bodies: map[string][]byte{},
	}
	for _, platform := range agyTestPlatforms() {
		body := []byte("antigravity-bytes-" + platform.file)
		sum := sha512.Sum512(body)
		origin.bodies[platform.platform] = body
		origin.docs[platform.platform] = agyTestManifest{
			version: version,
			url:     agyTestFileURL(version, build, platform.dir, platform.file),
			sha:     hex.EncodeToString(sum[:]),
			status:  http.StatusOK,
		}
	}
	origin.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin.mu.Lock()
		defer origin.mu.Unlock()
		const prefix = "/manifests/"
		if strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, ".json") {
			platform := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), ".json")
			doc, ok := origin.docs[platform]
			if !ok {
				t.Errorf("preview requested unknown manifest %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			origin.manifestHits++
			if doc.encoding != "" {
				w.Header().Set("Content-Encoding", doc.encoding)
			}
			status := doc.status
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			if doc.raw != "" {
				_, _ = w.Write([]byte(doc.raw))
				return
			}
			raw, err := json.Marshal(antigravityManifestDocument{Version: doc.version, URL: doc.url, SHA512: doc.sha})
			if err != nil {
				t.Errorf("encode manifest: %v", err)
				return
			}
			_, _ = w.Write(raw)
			return
		}
		origin.fileHits++
		if origin.failOnFile {
			t.Errorf("preview requested a platform file %s", r.URL.Path)
		}
		name := strings.TrimPrefix(r.URL.Path, "/antigravity-public/antigravity-cli/"+version+"-"+build+"/")
		for _, platform := range agyTestPlatforms() {
			if name == platform.dir+"/"+platform.file {
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write(origin.bodies[platform.platform])
				return
			}
		}
		if !origin.failOnFile {
			t.Errorf("unexpected download %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(origin.server.Close)
	return origin
}

func (o *agyTestOrigin) counts() (manifests, files int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.manifestHits, o.fileHits
}

func (o *agyTestOrigin) setDoc(platform string, mutate func(*agyTestManifest)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	doc := o.docs[platform]
	mutate(&doc)
	o.docs[platform] = doc
}

func (o *agyTestOrigin) setAll(mutate func(*agyTestManifest)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for platform, doc := range o.docs {
		mutate(&doc)
		o.docs[platform] = doc
	}
}

func newAgyTestFetcher(t *testing.T, origin *agyTestOrigin) *Fetcher {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: dir, registryURL: origin.server.URL, client: origin.server.Client(),
		metadataMax: 1 << 20, artifactMax: 1 << 20,
		metadataTimeout: time.Minute, downloadTimeout: time.Minute, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fetcher
}

func TestAntigravityPreviewReadsManifestsOnly(t *testing.T) {
	origin := newAgyTestOrigin(t, agyTestVersion, agyTestBuild)
	origin.failOnFile = true
	fetcher := newAgyTestFetcher(t, origin)
	plan, err := fetcher.PreviewPlan(t.Context(), "antigravity", agyTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	manifests, files := origin.counts()
	if manifests != 6 || files != 0 {
		t.Fatalf("manifests=%d files=%d", manifests, files)
	}
	if plan.SourceKind != ArtifactSourceAntigravity || plan.PolicyVersion != AntigravityFetchPolicyVersion ||
		plan.Name != "antigravity" || plan.Version != agyTestVersion || plan.EnginesNode != "" ||
		plan.RegistryOrigin != origin.server.URL || plan.MaxBytes != DefaultAntigravityBundleMaxBytes ||
		plan.TarballURL != agyTestReleaseDirectory(agyTestVersion, agyTestBuild) ||
		!strings.HasPrefix(plan.SHA512Integrity, "sha512-") || !ValidAntigravitySourcePlan(plan.SourcePlan) {
		t.Fatalf("plan=%+v", plan)
	}
	typed, err := decodeAntigravitySourcePlan(plan.SourcePlan)
	if err != nil || typed.Build != agyTestBuild || typed.DownloadOrigin != origin.server.URL ||
		typed.ReleaseDirectory != plan.TarballURL || len(typed.Sources) != 6 {
		t.Fatalf("typed=%+v err=%v", typed, err)
	}
	for i, platform := range agyTestPlatforms() {
		got := typed.Sources[i]
		if got.Platform != platform.platform || got.Dir != platform.dir || got.File != platform.file ||
			got.TargetOS != platform.goos || got.TargetArch != platform.arch {
			t.Fatalf("source %d=%+v", i, got)
		}
	}
	second := newAgyTestOrigin(t, agyTestVersion, agyTestBuild)
	second.mu.Lock()
	body := append([]byte(nil), second.bodies["linux_amd64"]...)
	body[0] ^= 0xff
	sum := sha512.Sum512(body)
	second.bodies["linux_amd64"] = body
	doc := second.docs["linux_amd64"]
	doc.sha = hex.EncodeToString(sum[:])
	second.docs["linux_amd64"] = doc
	second.mu.Unlock()
	other, err := newAgyTestFetcher(t, second).PreviewPlan(t.Context(), "antigravity", agyTestVersion)
	if err != nil || other.PreviewDigest == plan.PreviewDigest {
		t.Fatalf("digest %s other %s err=%v", plan.PreviewDigest, other.PreviewDigest, err)
	}
}

func TestAntigravityPreviewRefusesUpstreamMetadata(t *testing.T) {
	const foreign = "9.8.7-not-a-version"
	cases := []struct {
		name     string
		mutate   func(*agyTestOrigin)
		want     error
		sentence string
		hidden   string
	}{
		{
			name: "published version",
			mutate: func(o *agyTestOrigin) {
				o.setAll(func(doc *agyTestManifest) { doc.version = "1.2.15" })
			},
			want:     ErrUpstreamVersionUnavailable,
			sentence: "upstream currently only provides Antigravity 1.2.15; retry Preview with this version",
		},
		{
			name: "versions disagree",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("windows_arm64", func(doc *agyTestManifest) { doc.version = "9.8.7" })
			},
			want: ErrMetadataInvalid, hidden: "9.8.7",
		},
		{
			name: "version is not major.minor.patch",
			mutate: func(o *agyTestOrigin) {
				o.setAll(func(doc *agyTestManifest) { doc.version = foreign })
			},
			want: ErrMetadataInvalid, hidden: foreign,
		},
		{
			name: "mixed builds",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("darwin_arm64", func(doc *agyTestManifest) {
					doc.url = agyTestFileURL(agyTestVersion, "4571742832820225", "darwin-arm", "cli_mac_arm64.tar.gz")
				})
			},
			want: ErrMetadataInvalid, hidden: "4571742832820225",
		},
		{
			name: "non-digit build",
			mutate: func(o *agyTestOrigin) {
				o.setAll(func(doc *agyTestManifest) {
					doc.url = strings.Replace(doc.url, agyTestBuild, "12ab", 1)
				})
			},
			want: ErrMetadataInvalid, hidden: "12ab",
		},
		{
			name: "oversized build",
			mutate: func(o *agyTestOrigin) {
				build := strings.Repeat("9", 33)
				o.setAll(func(doc *agyTestManifest) {
					doc.url = strings.Replace(doc.url, agyTestBuild, build, 1)
				})
			},
			want: ErrMetadataInvalid, hidden: strings.Repeat("9", 33),
		},
		{
			name: "wrong host",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) {
					doc.url = strings.Replace(doc.url, "https://storage.googleapis.com", "https://example.invalid", 1)
				})
			},
			want: ErrMetadataInvalid, hidden: "example.invalid",
		},
		{
			name: "wrong dir",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) {
					doc.url = strings.Replace(doc.url, "/linux-x64/", "/linux-amd64/", 1)
				})
			},
			want: ErrMetadataInvalid, hidden: "linux-amd64",
		},
		{
			name: "wrong file",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) {
					doc.url = strings.Replace(doc.url, "cli_linux_x64.tar.gz", "cli_linux_amd64.tar.gz", 1)
				})
			},
			want: ErrMetadataInvalid, hidden: "cli_linux_amd64.tar.gz",
		},
		{
			name: "extra path segment",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) {
					doc.url = strings.Replace(doc.url, "/linux-x64/", "/extra/linux-x64/", 1)
				})
			},
			want: ErrMetadataInvalid,
		},
		{
			name: "query string",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) { doc.url += "?download=1" })
			},
			want: ErrMetadataInvalid, hidden: "download=1",
		},
		{
			name: "uppercase sha512",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) { doc.sha = strings.ToUpper(doc.sha) })
			},
			want: ErrMetadataInvalid,
		},
		{
			name: "short sha512",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) { doc.sha = doc.sha[:64] })
			},
			want: ErrMetadataInvalid,
		},
		{
			name: "unknown json key",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) {
					doc.raw = `{"version":"1.2.14","url":"` + doc.url + `","sha512":"` + doc.sha + `","size":1}`
				})
			},
			want: ErrMetadataInvalid, hidden: `"size"`,
		},
		{
			name: "missing key",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) {
					doc.raw = `{"version":"1.2.14","url":"` + doc.url + `"}`
				})
			},
			want: ErrMetadataInvalid,
		},
		{
			name: "trailing data",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_amd64", func(doc *agyTestManifest) {
					doc.raw = `{"version":"1.2.14","url":"` + doc.url + `","sha512":"` + doc.sha + `"}{}`
				})
			},
			want: ErrMetadataInvalid,
		},
		{
			name: "non-200",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("darwin_amd64", func(doc *agyTestManifest) { doc.status = http.StatusNotFound })
			},
			want: ErrMetadataInvalid,
		},
		{
			name: "non-identity encoding",
			mutate: func(o *agyTestOrigin) {
				o.setDoc("linux_arm64", func(doc *agyTestManifest) { doc.encoding = "gzip" })
			},
			want: ErrMetadataInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origin := newAgyTestOrigin(t, agyTestVersion, agyTestBuild)
			origin.failOnFile = true
			tc.mutate(origin)
			fetcher := newAgyTestFetcher(t, origin)
			_, err := fetcher.PreviewPlan(t.Context(), "antigravity", agyTestVersion)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v", err)
			}
			_, files := origin.counts()
			if files != 0 {
				t.Fatalf("preview downloaded %d files", files)
			}
			var upstream *UpstreamVersionError
			if errors.As(err, &upstream) {
				if upstream.OperatorSentence() != tc.sentence || upstream.Published != "1.2.15" || upstream.Requested != agyTestVersion {
					t.Fatalf("upstream=%+v sentence=%q", upstream, upstream.OperatorSentence())
				}
			} else if tc.sentence != "" {
				t.Fatal("missing upstream version error")
			}
			if tc.hidden != "" && strings.Contains(err.Error(), tc.hidden) {
				t.Fatalf("error echoed upstream text %q: %v", tc.hidden, err)
			}
		})
	}
}

func TestAntigravityPreviewRefusesOversizedManifest(t *testing.T) {
	origin := newAgyTestOrigin(t, agyTestVersion, agyTestBuild)
	origin.failOnFile = true
	origin.setDoc("linux_amd64", func(doc *agyTestManifest) { doc.raw = strings.Repeat("x", 80) })
	dir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fetcher, err := newAntigravityFetcher(antigravityFetcherConfig{
		artifactsDir: dir, manifestURL: origin.server.URL, downloadURL: origin.server.URL,
		client: origin.server.Client(), metadataMax: 32, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, planErr := fetcher.PreviewPlan(t.Context(), agyTestVersion)
	if !errors.Is(planErr, ErrMetadataTooLarge) {
		t.Fatalf("err=%v", planErr)
	}
	if strings.Contains(planErr.Error(), strings.Repeat("x", 80)) {
		t.Fatalf("error echoed body: %v", planErr)
	}
}

func TestAntigravityFetchExactBuildsBundleAndReusesCache(t *testing.T) {
	origin := newAgyTestOrigin(t, agyTestVersion, agyTestBuild)
	fetcher := newAgyTestFetcher(t, origin)
	plan, err := fetcher.PreviewPlan(t.Context(), "antigravity", agyTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	record, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err != nil || existed || record.Name != "antigravity" || record.Version != agyTestVersion ||
		record.TarballURL != agyTestReleaseDirectory(agyTestVersion, agyTestBuild) ||
		record.SHA512Integrity != plan.SHA512Integrity || record.EnginesNode != "" {
		t.Fatalf("record=%+v existed=%t err=%v", record, existed, err)
	}
	assertAgyTestBundle(t, filepath.Join(fetcher.artifactsDir, record.SHA256+".tgz"), origin)
	_, files := origin.counts()
	cached, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:retry", nil)
	_, after := origin.counts()
	if err != nil || !existed || cached.SHA256 != record.SHA256 || after != files {
		t.Fatalf("cached=%+v existed=%t files=%d->%d err=%v", cached, existed, files, after, err)
	}
	tampered := plan
	tampered.TarballURL = strings.TrimSuffix(plan.TarballURL, "/")
	if _, _, err := fetcher.FetchExact(t.Context(), tampered, "operator:tamper", nil); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("tarball tamper err=%v", err)
	}
	typed, err := decodeAntigravitySourcePlan(plan.SourcePlan)
	if err != nil {
		t.Fatal(err)
	}
	typed.Sources[0].SHA512 = strings.Repeat("ab", 64)
	raw, err := marshalCompactNoEscape(typed)
	if err != nil {
		t.Fatal(err)
	}
	tampered = plan
	tampered.SourcePlan = string(raw)
	if _, _, err := fetcher.FetchExact(t.Context(), tampered, "operator:tamper", nil); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("source tamper err=%v", err)
	}
	_, filesAfter := origin.counts()
	if filesAfter != files {
		t.Fatalf("tampered plans downloaded files %d -> %d", files, filesAfter)
	}
}

func TestAntigravityFetchExactDoesNotReuseCacheForRepublishedFiles(t *testing.T) {
	origin := newAgyTestOrigin(t, agyTestVersion, agyTestBuild)
	fetcher := newAgyTestFetcher(t, origin)
	plan, err := fetcher.PreviewPlan(t.Context(), "antigravity", agyTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("antigravity-bytes-republished")
	sum := sha512.Sum512(body)
	origin.mu.Lock()
	origin.bodies["linux_amd64"] = body
	origin.mu.Unlock()
	origin.setDoc("linux_amd64", func(doc *agyTestManifest) { doc.sha = hex.EncodeToString(sum[:]) })
	republished, err := fetcher.PreviewPlan(t.Context(), "antigravity", agyTestVersion)
	if err != nil || republished.TarballURL != plan.TarballURL || republished.SHA512Integrity == plan.SHA512Integrity {
		t.Fatalf("republished=%+v err=%v", republished, err)
	}
	_, files := origin.counts()
	second, existed, err := fetcher.FetchExact(t.Context(), republished, "operator:test", nil)
	_, after := origin.counts()
	if err != nil || existed || second.SHA256 == first.SHA256 ||
		second.SHA512Integrity != republished.SHA512Integrity || after == files {
		t.Fatalf("second=%+v existed=%t files=%d->%d err=%v", second, existed, files, after, err)
	}
	assertAgyTestBundle(t, filepath.Join(fetcher.artifactsDir, second.SHA256+".tgz"), origin)
}

func TestAntigravityFetchExactRefusesSHA512MismatchAndRemovesTemp(t *testing.T) {
	origin := newAgyTestOrigin(t, agyTestVersion, agyTestBuild)
	fetcher := newAgyTestFetcher(t, origin)
	plan, err := fetcher.PreviewPlan(t.Context(), "antigravity", agyTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	origin.mu.Lock()
	origin.bodies["linux_amd64"] = []byte("tampered-antigravity")
	origin.mu.Unlock()
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("err=%v", err)
	}
	entries, err := os.ReadDir(fetcher.artifactsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), antigravitySourceTempPrefix) {
			t.Fatalf("temp remained: %s", entry.Name())
		}
	}
}

func TestAntigravityFetchExactEnforcesSourceCapWhileStreaming(t *testing.T) {
	origin := newAgyTestOrigin(t, agyTestVersion, agyTestBuild)
	dir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	limited, err := newAntigravityFetcher(antigravityFetcherConfig{
		artifactsDir: dir, manifestURL: origin.server.URL, downloadURL: origin.server.URL,
		client: origin.server.Client(), sourceMax: 8, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := limited.PreviewPlan(t.Context(), agyTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := limited.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("err=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), antigravitySourceTempPrefix) {
			t.Fatalf("temp remained: %s", entry.Name())
		}
	}
}

func assertAgyTestBundle(t *testing.T, path string, origin *agyTestOrigin) {
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
	want := []struct {
		name string
		mode int64
		body string
	}{
		{name: "antigravity/", mode: 0o755},
		{name: "antigravity/darwin-amd64/", mode: 0o755},
		{name: "antigravity/darwin-arm64/", mode: 0o755},
		{name: "antigravity/linux-amd64/", mode: 0o755},
		{name: "antigravity/linux-arm64/", mode: 0o755},
		{name: "antigravity/windows-amd64/", mode: 0o755},
		{name: "antigravity/windows-arm64/", mode: 0o755},
	}
	for _, platform := range agyTestPlatforms() {
		origin.mu.Lock()
		body := string(origin.bodies[platform.platform])
		sha := ""
		for _, doc := range []agyTestManifest{origin.docs[platform.platform]} {
			sha = doc.sha
		}
		origin.mu.Unlock()
		want = append(want, struct {
			name string
			mode int64
			body string
		}{name: "antigravity/" + platform.goos + "-" + platform.arch + "/" + platform.file, mode: 0o644, body: body})
		manifest := `{"version":"` + agyTestVersion + `","url":"` + agyTestFileURL(agyTestVersion, agyTestBuild, platform.dir, platform.file) + `","sha512":"` + sha + `"}`
		want = append(want, struct {
			name string
			mode int64
			body string
		}{name: "antigravity/" + platform.goos + "-" + platform.arch + "/manifest.json", mode: 0o644, body: manifest})
	}
	for i, entry := range want {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("entry %d %s: %v", i, entry.name, err)
		}
		if header.Name != entry.name || header.Mode&0o777 != entry.mode {
			t.Fatalf("entry %d name=%q mode=%o want %s %o", i, header.Name, header.Mode&0o777, entry.name, entry.mode)
		}
		if entry.body == "" {
			continue
		}
		got, err := io.ReadAll(reader)
		if err != nil || string(got) != entry.body {
			t.Fatalf("entry %s body=%q err=%v", entry.name, got, err)
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing entry err=%v", err)
	}
}
