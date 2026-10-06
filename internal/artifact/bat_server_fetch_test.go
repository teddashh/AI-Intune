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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	batTestVersion     = "3.2.10"
	batTestAMD64Asset  = "bat-server-linux-x86_64.tar.gz"
	batTestARM64Asset  = "bat-server-linux-aarch64.tar.gz"
	batTestAMD64Member = "bat-server/linux-amd64/bat-server.tar.gz"
	batTestARM64Member = "bat-server/linux-arm64/bat-server.tar.gz"
	batTestAMD64Binary = "bat-server-binary-amd64-v1"
	batTestARM64Binary = "bat-server-binary-arm64-v1"
	batTestSignedQuery = "se=1&sig=test"
)

func TestBATServerFetcherPinsReleaseAndBuildsBundle(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := newBATServerTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceKind != ArtifactSourceBATServer || plan.PolicyVersion != BATServerFetchPolicyVersion ||
		plan.Name != "bat-server" || plan.Version != batTestVersion || plan.RegistryOrigin != origin.api.URL ||
		plan.EnginesNode != "" || plan.MaxBytes != DefaultBATServerBundleMaxBytes ||
		plan.SourcePlan == "" || len(plan.SourcePlan) > MaxArtifactSourcePlanBytes ||
		!ValidBATServerSourcePlan(plan.SourcePlan) {
		t.Fatalf("plan=%+v bytes=%d", plan, len(plan.SourcePlan))
	}
	if !strings.Contains(plan.TarballURL, "/repos/tony1223/better-agent-terminal/releases/tags/v3.2.10") ||
		strings.Contains(plan.TarballURL, "/dl/") || strings.Contains(plan.SourcePlan, "browser_download_url") ||
		strings.Contains(plan.SourcePlan, "/dl/") || strings.Contains(plan.SourcePlan, "sig=") ||
		strings.Contains(plan.SourcePlan, "AppImage") || strings.Contains(plan.SourcePlan, "BetterAgentTerminal") {
		t.Fatalf("plan stored an unpinned download identity: url=%s plan=%s", plan.TarballURL, plan.SourcePlan)
	}
	typed, err := decodeBATServerSourcePlan(plan.SourcePlan)
	if err != nil || len(typed.Sources) != 2 {
		t.Fatalf("sources=%+v err=%v", typed.Sources, err)
	}
	amd64Body := origin.body(batTestAMD64Asset)
	arm64Body := origin.body(batTestARM64Asset)
	if typed.Sources[0].TargetOS != "linux" || typed.Sources[0].TargetArch != "amd64" ||
		typed.Sources[0].Asset != "bat-server-linux-x86_64.tar.gz" ||
		typed.Sources[0].Size != int64(len(amd64Body)) || typed.Sources[0].Digest != batTestDigest(amd64Body) ||
		typed.Sources[1].TargetOS != "linux" || typed.Sources[1].TargetArch != "arm64" ||
		typed.Sources[1].Asset != "bat-server-linux-aarch64.tar.gz" ||
		typed.Sources[1].Size != int64(len(arm64Body)) || typed.Sources[1].Digest != batTestDigest(arm64Body) ||
		strings.Contains(plan.SourcePlan, "binary_sha256") {
		t.Fatalf("sources=%+v plan=%s", typed.Sources, plan.SourcePlan)
	}
	if releases, downloads, files, _ := origin.counts(); releases != 1 || downloads != 0 || files != 0 {
		t.Fatalf("preview hits releases=%d downloads=%d files=%d", releases, downloads, files)
	}
	record, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err != nil || existed || record.Name != "bat-server" || record.Version != batTestVersion ||
		record.EnginesNode != "" || record.TarballURL != plan.TarballURL {
		t.Fatalf("record=%+v existed=%t err=%v", record, existed, err)
	}
	sidecar, err := os.ReadFile(filepath.Join(dir, record.SHA256+".json"))
	if err != nil || strings.Contains(string(sidecar), "/dl/") || strings.Contains(string(sidecar), "sig=") {
		t.Fatalf("sidecar=%s err=%v", sidecar, err)
	}
	assertBATServerBundle(t, filepath.Join(dir, record.SHA256+".tgz"), amd64Body, arm64Body)
	releases, downloads, files, evil := origin.counts()
	if releases != 2 || downloads != 2 || files != 2 || evil != 0 || origin.credentialN != 0 || origin.badAgentN != 0 {
		t.Fatalf("hits releases=%d downloads=%d files=%d evil=%d creds=%d agent=%d",
			releases, downloads, files, evil, origin.credentialN, origin.badAgentN)
	}
	cached, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:retry", nil)
	releasesAfter, downloadsAfter, filesAfter, _ := origin.counts()
	if err != nil || !existed || cached.SHA256 != record.SHA256 ||
		releasesAfter != releases || downloadsAfter != downloads || filesAfter != files {
		t.Fatalf("cached=%+v existed=%t hits=%d/%d/%d->%d/%d/%d err=%v",
			cached, existed, releases, downloads, files, releasesAfter, downloadsAfter, filesAfter, err)
	}
}

func TestBATServerFetcherRejectsPrerelease(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	origin.prerelease = true
	fetcher := newBATServerTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, downloads, files, _ := origin.counts(); downloads != 0 || files != 0 {
		t.Fatalf("downloads=%d files=%d", downloads, files)
	}
}

func TestBATServerFetcherRejectsDraft(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	origin.draft = true
	fetcher := newBATServerTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, downloads, files, _ := origin.counts(); downloads != 0 || files != 0 {
		t.Fatalf("downloads=%d files=%d", downloads, files)
	}
}

func TestBATServerFetcherRejectsTagMismatch(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	origin.tagName = "v9.9.9"
	fetcher := newBATServerTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, downloads, files, _ := origin.counts(); downloads != 0 || files != 0 {
		t.Fatalf("downloads=%d files=%d", downloads, files)
	}
}

func TestBATServerFetcherRejectsMissingAsset(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	origin.dropAsset(batTestARM64Asset)
	fetcher := newBATServerTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, downloads, files, _ := origin.counts(); downloads != 0 || files != 0 {
		t.Fatalf("downloads=%d files=%d", downloads, files)
	}
}

func TestBATServerFetcherRejectsDuplicateAsset(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	origin.duplicateAsset(batTestAMD64Asset)
	fetcher := newBATServerTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, downloads, files, _ := origin.counts(); downloads != 0 || files != 0 {
		t.Fatalf("downloads=%d files=%d", downloads, files)
	}
}

func TestBATServerFetcherRejectsMalformedDigest(t *testing.T) {
	cases := []struct {
		name   string
		digest string
	}{
		{name: "empty", digest: ""},
		{name: "no prefix", digest: strings.Repeat("ab", 32)},
		{name: "short", digest: "sha256:" + strings.Repeat("ab", 31)},
		{name: "long", digest: "sha256:" + strings.Repeat("ab", 33)},
		{name: "uppercase", digest: "sha256:" + strings.ToUpper(strings.Repeat("ab", 32))},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			origin := newBATServerTestOrigin(t)
			origin.setDigest(batTestAMD64Asset, item.digest)
			fetcher := newBATServerTestFetcher(t, origin)
			if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrMetadataInvalid) {
				t.Fatalf("err=%v", err)
			}
			if _, downloads, files, _ := origin.counts(); downloads != 0 || files != 0 {
				t.Fatalf("downloads=%d files=%d", downloads, files)
			}
		})
	}
}

func TestBATServerFetcherRejectsAssetSize(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		origin := newBATServerTestOrigin(t)
		origin.setSize(batTestAMD64Asset, 0)
		fetcher := newBATServerTestFetcher(t, origin)
		if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrMetadataInvalid) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("negative", func(t *testing.T) {
		origin := newBATServerTestOrigin(t)
		origin.setSize(batTestARM64Asset, -1)
		fetcher := newBATServerTestFetcher(t, origin)
		if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrMetadataInvalid) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("above_max", func(t *testing.T) {
		origin := newBATServerTestOrigin(t)
		origin.setSize(batTestAMD64Asset, DefaultBATServerSourceMaxBytes+1)
		fetcher := newBATServerTestFetcher(t, origin)
		if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrArtifactTooLarge) {
			t.Fatalf("err=%v", err)
		}
		if _, downloads, files, _ := origin.counts(); downloads != 0 || files != 0 {
			t.Fatalf("downloads=%d files=%d", downloads, files)
		}
	})
}

func TestBATServerFetcherRejectsDigestMismatch(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := newBATServerTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	body := origin.body(batTestAMD64Asset)
	body[len(body)-1] ^= 0xff
	origin.setBody(batTestAMD64Asset, body)
	_, _, err = fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if !errors.Is(err, ErrIntegrityMismatch) || strings.Contains(err.Error(), "does not match preview") {
		t.Fatalf("err=%v", err)
	}
	assertNoPublishedBATServer(t, dir)
}

func TestBATServerFetcherRejectsReleaseDrift(t *testing.T) {
	t.Run("digest", func(t *testing.T) {
		origin := newBATServerTestOrigin(t)
		dir := filepath.Join(t.TempDir(), "artifacts")
		fetcher := newBATServerTestFetcherIn(t, origin, dir)
		plan, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion)
		if err != nil {
			t.Fatal(err)
		}
		lied := "sha256:" + strings.Repeat("ab", 32)
		if lied == batTestDigest(origin.body(batTestAMD64Asset)) {
			t.Fatal("drift digest collided with the fixture")
		}
		_, beforeDownloads, beforeFiles, _ := origin.counts()
		origin.mutate = "digest"
		_, _, err = fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
		if err == nil || !errors.Is(err, ErrInvalidFetchRequest) || !strings.Contains(err.Error(), "bat-server source plan does not match preview") {
			t.Fatalf("err=%v", err)
		}
		if _, downloads, files, _ := origin.counts(); downloads != beforeDownloads || files != beforeFiles {
			t.Fatalf("downloads=%d files=%d before=%d/%d", downloads, files, beforeDownloads, beforeFiles)
		}
		assertNoPublishedBATServer(t, dir)
	})
	t.Run("size", func(t *testing.T) {
		origin := newBATServerTestOrigin(t)
		dir := filepath.Join(t.TempDir(), "artifacts")
		fetcher := newBATServerTestFetcherIn(t, origin, dir)
		plan, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion)
		if err != nil {
			t.Fatal(err)
		}
		_, beforeDownloads, beforeFiles, _ := origin.counts()
		origin.mutate = "size"
		_, _, err = fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
		if err == nil || !errors.Is(err, ErrInvalidFetchRequest) || !strings.Contains(err.Error(), "bat-server source plan does not match preview") {
			t.Fatalf("err=%v", err)
		}
		if _, downloads, files, _ := origin.counts(); downloads != beforeDownloads || files != beforeFiles {
			t.Fatalf("downloads=%d files=%d before=%d/%d", downloads, files, beforeDownloads, beforeFiles)
		}
		assertNoPublishedBATServer(t, dir)
	})
}

func TestBATServerFetcherRejectsOversizedBundle(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	fetcher := newBATServerDirectFetcher(t, origin, 1)
	plan, err := fetcher.PreviewPlan(t.Context(), batTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("err=%v", err)
	}
	assertNoPublishedBATServer(t, fetcher.artifactsDir)
}

func TestBATServerFetcherRejectsRedirectLoop(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := newBATServerTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	_, beforeDownloads, beforeFiles, _ := origin.counts()
	origin.redirectMode = "loop"
	_, _, err = fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	_, downloads, files, _ := origin.counts()
	if !errors.Is(err, ErrRegistryPolicy) || downloads-beforeDownloads != 6 || files != beforeFiles {
		t.Fatalf("err=%v downloads=%d files=%d before=%d/%d", err, downloads, files, beforeDownloads, beforeFiles)
	}
	assertNoPublishedBATServer(t, dir)
}

func TestBATServerFetcherRejectsTooManyRedirects(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := newBATServerTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	_, beforeDownloads, beforeFiles, _ := origin.counts()
	origin.redirectMode = "chain"
	_, _, err = fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	_, downloads, files, _ := origin.counts()
	if !errors.Is(err, ErrRegistryPolicy) || downloads-beforeDownloads != 6 || files != beforeFiles {
		t.Fatalf("err=%v downloads=%d files=%d before=%d/%d", err, downloads, files, beforeDownloads, beforeFiles)
	}
	assertNoPublishedBATServer(t, dir)
}

func TestBATServerFetcherRejectsMetadataRedirect(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	origin.redirectRelease = true
	fetcher := newBATServerTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, _, _, evil := origin.counts(); evil != 0 {
		t.Fatalf("followed metadata redirect evil=%d", evil)
	}
}

func TestBATServerFetcherRejectsUnpinnedVersions(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	fetcher := newBATServerTestFetcher(t, origin)
	for _, candidate := range []string{"latest", "3.2", "v3.2.10", "3.2.11-pre.4", "3.2.10-pre.4", "03.2.10"} {
		if _, err := fetcher.PreviewPlan(t.Context(), "bat-server", candidate); !errors.Is(err, ErrInvalidFetchRequest) {
			t.Fatalf("%s err=%v", candidate, err)
		}
	}
	if releases, downloads, _, _ := origin.counts(); releases != 0 || downloads != 0 {
		t.Fatalf("unpinned versions reached origin releases=%d downloads=%d", releases, downloads)
	}
}

func TestBATServerFetcherRejectsNonProductionOrigin(t *testing.T) {
	_, err := newBATServerFetcher(batServerFetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"),
		originURL:    "https://example.com",
	})
	if !errors.Is(err, ErrRegistryPolicy) {
		t.Fatalf("err=%v", err)
	}
}

func TestBATServerFetcherRejectsTamperedPlanBeforeDownload(t *testing.T) {
	origin := newBATServerTestOrigin(t)
	fetcher := newBATServerTestFetcher(t, origin)
	plan, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	_, beforeDownloads, beforeFiles, _ := origin.counts()
	plan.SourcePlan = strings.Replace(plan.SourcePlan, `"target_arch":"amd64"`, `"target_arch":"arm64"`, 1)
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("err=%v", err)
	}
	if _, downloads, files, _ := origin.counts(); downloads != beforeDownloads || files != beforeFiles {
		t.Fatalf("downloads=%d files=%d before=%d/%d", downloads, files, beforeDownloads, beforeFiles)
	}
}

func TestBATServerPreviewPlanReadsReleaseMetadataOnly(t *testing.T) {
	var mu sync.Mutex
	assetHits := 0
	assets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		assetHits++
		mu.Unlock()
		t.Errorf("preview requested asset %s %s", r.Method, r.URL.RequestURI())
		http.Error(w, "asset", http.StatusInternalServerError)
	}))
	t.Cleanup(assets.Close)

	releaseHits := 0
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		releaseHits++
		mu.Unlock()
		if r.URL.Path != "/repos/"+batServerRepository+"/releases/tags/v"+batTestVersion {
			http.NotFound(w, r)
			return
		}
		raw, err := json.Marshal(map[string]any{
			"tag_name": "v" + batTestVersion, "draft": false, "prerelease": false,
			"assets": []map[string]any{
				{
					"name": batTestAMD64Asset, "size": int64(1),
					"digest":               "sha256:" + strings.Repeat("ab", 32),
					"browser_download_url": assets.URL + "/" + batTestAMD64Asset,
				},
				{
					"name": batTestARM64Asset, "size": int64(1),
					"digest":               "sha256:" + strings.Repeat("cd", 32),
					"browser_download_url": assets.URL + "/" + batTestARM64Asset,
				},
			},
		})
		if err != nil {
			http.Error(w, "encode", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(releases.Close)

	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"), registryURL: releases.URL,
		batServerOriginURL: releases.URL, metadataMax: 1 << 20, artifactMax: 1 << 20,
		metadataTimeout: 5 * time.Second, downloadTimeout: 15 * time.Second, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fetcher.PreviewPlan(t.Context(), "bat-server", batTestVersion)
	mu.Lock()
	gotAssets, gotReleases := assetHits, releaseHits
	mu.Unlock()
	if err != nil || gotAssets != 0 || gotReleases != 1 {
		t.Fatalf("err=%v asset requests=%d release requests=%d", err, gotAssets, gotReleases)
	}
	if strings.Contains(plan.SourcePlan, "binary_sha256") || !ValidBATServerSourcePlan(plan.SourcePlan) {
		t.Fatalf("plan=%s", plan.SourcePlan)
	}
	stale := strings.Replace(plan.SourcePlan, `"digest":"`, `"binary_sha256":"`+strings.Repeat("ef", 32)+`","digest":"`, 1)
	if stale == plan.SourcePlan {
		t.Fatal("stored plan fixture did not gain binary_sha256")
	}
	if _, err := decodeBATServerSourcePlan(stale); err == nil || !errors.Is(err, ErrInvalidFetchRequest) || ValidBATServerSourcePlan(stale) {
		t.Fatalf("stale plan err=%v", err)
	}
}

func TestBATServerBundleMemberNames(t *testing.T) {
	amd64, ok := BATServerBundleMember("linux", "amd64")
	if !ok || amd64 != "bat-server/linux-amd64/bat-server.tar.gz" {
		t.Fatalf("amd64=%q ok=%t", amd64, ok)
	}
	arm64, ok := BATServerBundleMember("linux", "arm64")
	if !ok || arm64 != "bat-server/linux-arm64/bat-server.tar.gz" {
		t.Fatalf("arm64=%q ok=%t", arm64, ok)
	}
	for _, platform := range [][2]string{
		{"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"}, {"linux", "386"},
	} {
		if _, ok := BATServerBundleMember(platform[0], platform[1]); ok {
			t.Fatalf("accepted %s/%s", platform[0], platform[1])
		}
	}
}

func TestBATServerSourceTempPrefixIsRecognized(t *testing.T) {
	if batServerSourceTempPrefix != ".bat-server-source-" {
		t.Fatalf("prefix=%q", batServerSourceTempPrefix)
	}
	if !isArtifactTempName(batServerSourceTempPrefix + "interrupted.tmp") {
		t.Fatal("bat-server source temp prefix is not recognized")
	}
}

func batServerFixtureAssetArchive(t *testing.T, root string, binary []byte) []byte {
	t.Helper()
	if root != "bat-server-linux-x86_64" && root != "bat-server-linux-aarch64" {
		t.Fatalf("fixture root %q is not an upstream arch directory", root)
	}
	if len(binary) == 0 {
		t.Fatal("fixture binary is empty")
	}
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: root + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: root + "/bat-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(binary)),
	}); err != nil {
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

func batTestDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type batServerFixtureAsset struct {
	name   string
	size   int64
	digest string
}

type batServerTestOrigin struct {
	api             *httptest.Server
	files           *httptest.Server
	mu              sync.Mutex
	version         string
	assets          []batServerFixtureAsset
	bodies          map[string][]byte
	releaseN        int
	downloadN       int
	fileN           int
	evilN           int
	credentialN     int
	badAgentN       int
	draft           bool
	prerelease      bool
	tagName         string
	redirectRelease bool
	redirectMode    string
	mutate          string
}

func newBATServerTestOrigin(t *testing.T) *batServerTestOrigin {
	t.Helper()
	amd64 := batServerFixtureAssetArchive(t, "bat-server-linux-x86_64", []byte(batTestAMD64Binary))
	arm64 := batServerFixtureAssetArchive(t, "bat-server-linux-aarch64", []byte(batTestARM64Binary))
	origin := &batServerTestOrigin{
		version: batTestVersion,
		bodies: map[string][]byte{
			batTestAMD64Asset:                         append([]byte(nil), amd64...),
			batTestARM64Asset:                         append([]byte(nil), arm64...),
			"bat-server-x86_64.AppImage":              []byte("appimage-amd64"),
			"bat-server-aarch64.AppImage":             []byte("appimage-arm64"),
			"BetterAgentTerminal-linux-x86_64.tar.gz": []byte("desktop"),
		},
	}
	origin.assets = []batServerFixtureAsset{
		{name: "bat-server-x86_64.AppImage", size: 0, digest: "not-a-digest"},
		{name: batTestAMD64Asset, size: int64(len(amd64)), digest: batTestDigest(amd64)},
		{name: "BetterAgentTerminal-linux-x86_64.tar.gz", size: 7, digest: ""},
		{name: "bat-server-aarch64.AppImage", size: -1, digest: "sha256:ABCD"},
		{name: batTestARM64Asset, size: int64(len(arm64)), digest: batTestDigest(arm64)},
	}
	origin.files = httptest.NewServer(http.HandlerFunc(origin.serveFile))
	origin.api = httptest.NewServer(http.HandlerFunc(origin.serveAPI))
	t.Cleanup(origin.api.Close)
	t.Cleanup(origin.files.Close)
	return origin
}

func (o *batServerTestOrigin) noteRequest(r *http.Request) {
	if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		o.credentialN++
	}
	if r.Header.Get("User-Agent") != "ai-intune" {
		o.badAgentN++
	}
}

func (o *batServerTestOrigin) serveAPI(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.noteRequest(r)
	if r.URL.Path == "/evil-release" {
		o.evilN++
		o.writeRelease(w)
		return
	}
	if r.URL.Path == "/repos/tony1223/better-agent-terminal/releases/tags/v"+o.version {
		o.releaseN++
		if o.redirectRelease {
			http.Redirect(w, r, o.api.URL+"/evil-release", http.StatusFound)
			return
		}
		o.writeRelease(w)
		return
	}
	name, ok := strings.CutPrefix(r.URL.Path, "/dl/")
	if !ok || name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	o.downloadN++
	if o.downloadN > 24 {
		http.Error(w, "bounded", http.StatusInternalServerError)
		return
	}
	switch o.redirectMode {
	case "loop":
		http.Redirect(w, r, o.api.URL+r.URL.Path, http.StatusFound)
	case "chain":
		hop := 0
		if raw := r.URL.Query().Get("hop"); raw != "" {
			hop, _ = strconv.Atoi(raw)
		}
		if hop < 6 {
			http.Redirect(w, r, o.api.URL+r.URL.Path+"?hop="+strconv.Itoa(hop+1), http.StatusFound)
			return
		}
		o.writeFileBody(w, name)
	default:
		http.Redirect(w, r, o.files.URL+"/objects/"+name+"?"+batTestSignedQuery, http.StatusFound)
	}
}

func (o *batServerTestOrigin) writeRelease(w http.ResponseWriter) {
	tag := o.tagName
	if tag == "" {
		tag = "v" + o.version
	}
	assets := make([]map[string]any, 0, len(o.assets))
	for _, asset := range o.assets {
		size, digest := asset.size, asset.digest
		if o.releaseN >= 2 && asset.name == batTestAMD64Asset {
			switch o.mutate {
			case "digest":
				digest = "sha256:" + strings.Repeat("ab", 32)
			case "size":
				size++
			}
		}
		assets = append(assets, map[string]any{
			"name": asset.name, "size": size, "digest": digest,
			"browser_download_url": o.api.URL + "/dl/" + asset.name,
			"state":                "uploaded",
		})
	}
	raw, err := json.Marshal(map[string]any{
		"tag_name": tag, "draft": o.draft, "prerelease": o.prerelease,
		"assets": assets, "html_url": "https://example.invalid/bat-server",
	})
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (o *batServerTestOrigin) serveFile(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.noteRequest(r)
	name, ok := strings.CutPrefix(r.URL.Path, "/objects/")
	if !ok || name == "" {
		http.NotFound(w, r)
		return
	}
	if r.URL.RawQuery != batTestSignedQuery {
		http.Error(w, "query", http.StatusBadRequest)
		return
	}
	o.fileN++
	o.writeFileBody(w, name)
}

func (o *batServerTestOrigin) writeFileBody(w http.ResponseWriter, name string) {
	body, ok := o.bodies[name]
	if !ok {
		http.Error(w, "missing", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (o *batServerTestOrigin) body(name string) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]byte(nil), o.bodies[name]...)
}

func (o *batServerTestOrigin) setBody(name string, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bodies[name] = append([]byte(nil), body...)
}

func (o *batServerTestOrigin) setDigest(name, digest string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range o.assets {
		if o.assets[i].name == name {
			o.assets[i].digest = digest
		}
	}
}

func (o *batServerTestOrigin) setSize(name string, size int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range o.assets {
		if o.assets[i].name == name {
			o.assets[i].size = size
		}
	}
}

func (o *batServerTestOrigin) dropAsset(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	kept := make([]batServerFixtureAsset, 0, len(o.assets))
	for _, asset := range o.assets {
		if asset.name != name {
			kept = append(kept, asset)
		}
	}
	o.assets = kept
}

func (o *batServerTestOrigin) duplicateAsset(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, asset := range o.assets {
		if asset.name == name {
			o.assets = append(o.assets, asset)
			return
		}
	}
}

func (o *batServerTestOrigin) counts() (releases, downloads, files, evil int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.releaseN, o.downloadN, o.fileN, o.evilN
}

func newBATServerTestFetcher(t *testing.T, origin *batServerTestOrigin) *Fetcher {
	t.Helper()
	return newBATServerTestFetcherIn(t, origin, filepath.Join(t.TempDir(), "artifacts"))
}

func newBATServerTestFetcherIn(t *testing.T, origin *batServerTestOrigin, dir string) *Fetcher {
	t.Helper()
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: dir, registryURL: origin.api.URL, batServerOriginURL: origin.api.URL,
		client: origin.api.Client(), metadataMax: 1 << 20, artifactMax: 1 << 20,
		metadataTimeout: 5 * time.Second, downloadTimeout: 15 * time.Second, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fetcher
}

func newBATServerDirectFetcher(t *testing.T, origin *batServerTestOrigin, bundleMax int64) *BATServerFetcher {
	t.Helper()
	fetcher, err := newBATServerFetcher(batServerFetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"), originURL: origin.api.URL,
		client: origin.api.Client(), metadataMax: 1 << 20, sourceMax: DefaultBATServerSourceMaxBytes,
		bundleMax: bundleMax, metadataTimeout: 5 * time.Second, downloadTimeout: 15 * time.Second,
		allowHTTP: true, now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fetcher
}

func assertNoPublishedBATServer(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, ".json") {
			t.Fatalf("published %s", name)
		}
	}
}

func assertBATServerBundle(t *testing.T, path string, amd64Body, arm64Body []byte) {
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
		"bat-server/linux-amd64/bat-server.tar.gz": amd64Body,
		"bat-server/linux-arm64/bat-server.tar.gz": arm64Body,
	}
	if _, ok := want["bat-server/linux-amd64/bat-server.tar.gz"]; !ok {
		t.Fatal("linux/amd64 bundle member literal is missing")
	}
	if _, ok := want["bat-server/linux-arm64/bat-server.tar.gz"]; !ok {
		t.Fatal("linux/arm64 bundle member literal is missing")
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
			name := strings.TrimSuffix(header.Name, "/")
			if header.Mode != 0o755 || (name != "bat-server" && !strings.HasPrefix(name, "bat-server/")) {
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
	if len(seen) != 2 || !seen[batTestAMD64Member] || !seen[batTestARM64Member] {
		t.Fatalf("bundle files=%v", seen)
	}
}

func TestBATServerProductionAssetURLPinsGitHubReleaseHosting(t *testing.T) {
	production := &BATServerFetcher{}
	for _, raw := range []string{
		"https://github.com/tony1223/better-agent-terminal/releases/download/v1.2.3/bat-server-linux-x64.tar.gz",
		"https://objects.githubusercontent.com/github-production-release-asset/1/2?X-Amz-Signature=abc",
		"https://release-assets.githubusercontent.com/github-production-release-asset/1/2?sp=r&sig=abc",
		"https://release-assets.githubusercontent.com:443/asset",
	} {
		if err := production.validateAssetURL(raw); err != nil {
			t.Errorf("validateAssetURL(%q)=%v, want accepted", raw, err)
		}
	}
	for _, raw := range []string{
		"https://github.com/someone-else/better-agent-terminal/releases/download/v1.2.3/bat-server.tar.gz",
		"https://github.com/tony1223/better-agent-terminal/archive/v1.2.3.tar.gz",
		"https://evil.example/bat-server.tar.gz",
		"https://169.254.169.254/latest/meta-data",
		"https://127.0.0.1/asset",
		"https://objects.githubusercontent.com.evil.example/asset",
		"https://release-assets.githubusercontent.com:8443/asset",
		"http://objects.githubusercontent.com/asset",
	} {
		if err := production.validateAssetURL(raw); !errors.Is(err, ErrRegistryPolicy) {
			t.Errorf("validateAssetURL(%q)=%v, want ErrRegistryPolicy", raw, err)
		}
	}
}
