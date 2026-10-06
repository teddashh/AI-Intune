package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGrokFetcherPinsOfficialMetadataAndBuildsBundle(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceKind != ArtifactSourceGrok || plan.PolicyVersion != GrokFetchPolicyVersion ||
		plan.Name != "grok" || plan.Version != version || plan.RegistryOrigin != origin.server.URL ||
		plan.EnginesNode != "" || plan.MaxBytes != DefaultGrokBundleMaxBytes ||
		plan.SourcePlan == "" || len(plan.SourcePlan) > MaxArtifactSourcePlanBytes ||
		!ValidGrokSourcePlan(plan.SourcePlan) {
		t.Fatalf("plan=%+v bytes=%d", plan, len(plan.SourcePlan))
	}
	typed, err := decodeGrokSourcePlan(plan.SourcePlan)
	if err != nil || len(typed.Sources) != 6 || typed.RootIntegrity == "" ||
		typed.Sources[0].TargetOS != "darwin" || typed.Sources[0].TargetArch != "amd64" ||
		typed.Sources[5].TargetOS != "windows" || typed.Sources[5].TargetArch != "arm64" {
		t.Fatalf("sources=%+v root=%s err=%v", typed.Sources, typed.RootIntegrity, err)
	}
	if typed.RootIntegrity != origin.packages[grokRootPackage].integrity {
		t.Fatalf("root integrity=%s", typed.RootIntegrity)
	}
	for _, source := range typed.Sources {
		state := origin.packages[source.Package]
		if state == nil || source.TarballURL != state.tarball || source.Integrity != state.integrity {
			t.Fatalf("source=%+v state=%+v", source, state)
		}
	}
	record, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err != nil || existed || record.Name != "grok" || record.Version != version || record.EnginesNode != "" {
		t.Fatalf("record=%+v existed=%t err=%v", record, existed, err)
	}
	assertGrokTestBundle(t, filepath.Join(dir, record.SHA256+".tgz"), origin)
	docs, tarballs, rootTarballs := origin.counts()
	if docs != 7 || tarballs != 6 || rootTarballs != 0 {
		t.Fatalf("docs=%d tarballs=%d rootTarballs=%d", docs, tarballs, rootTarballs)
	}
	cached, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:retry", nil)
	docsAfter, tarballsAfter, rootAfter := origin.counts()
	if err != nil || !existed || cached.SHA256 != record.SHA256 ||
		docsAfter != docs || tarballsAfter != tarballs || rootAfter != rootTarballs {
		t.Fatalf("cached=%+v existed=%t hits=%d/%d/%d->%d/%d/%d err=%v",
			cached, existed, docs, tarballs, rootTarballs, docsAfter, tarballsAfter, rootAfter, err)
	}
}

func TestGrokFetcherRejectsMissingPlatform(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	delete(origin.packages[grokRootPackage].optional, "@xai-official/grok-win32-arm64")
	fetcher := newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, tarballs, _ := origin.counts(); tarballs != 0 {
		t.Fatalf("tarballs=%d", tarballs)
	}
}

func TestGrokFetcherRejectsUnknownPlatform(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	origin.packages[grokRootPackage].optional["@xai-official/grok-freebsd-x64"] = version
	fetcher := newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, tarballs, _ := origin.counts(); tarballs != 0 {
		t.Fatalf("tarballs=%d", tarballs)
	}
}

func TestGrokFetcherRejectsDependencyVersionMismatch(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	origin.packages[grokRootPackage].optional["@xai-official/grok-linux-x64"] = "9.9.9"
	fetcher := newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestGrokFetcherRejectsPlatformVersionMismatch(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	origin.packages["@xai-official/grok-linux-x64"].version = "9.9.9"
	fetcher := newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, tarballs, _ := origin.counts(); tarballs != 0 {
		t.Fatalf("tarballs=%d", tarballs)
	}
}

func TestGrokFetcherRejectsPlatformTupleMismatch(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	origin.packages["@xai-official/grok-linux-x64"].cpu = []string{"arm64"}
	fetcher := newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	origin = newGrokTestOrigin(t, version)
	origin.packages["@xai-official/grok-win32-arm64"].os = []string{"linux"}
	fetcher = newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("os err=%v", err)
	}
}

func TestGrokFetcherRejectsPlatformFileCount(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	origin.packages["@xai-official/grok-linux-x64"].fileCount = 5
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	if _, tarballs, rootTarballs := origin.counts(); tarballs != 0 || rootTarballs != 0 {
		t.Fatalf("tarballs=%d root=%d", tarballs, rootTarballs)
	}
	assertNoPublishedGrok(t, dir)
}

func TestGrokArchiveMemberNames(t *testing.T) {
	want := map[string]string{
		"darwin-amd64":  "package/bin/grok.br",
		"darwin-arm64":  "package/bin/grok.br",
		"linux-amd64":   "package/bin/grok.br",
		"linux-arm64":   "package/bin/grok.br",
		"windows-amd64": "package/bin/grok.exe.br",
		"windows-arm64": "package/bin/grok.exe.br",
	}
	if len(grokPlatforms()) != len(want) {
		t.Fatalf("platforms=%d", len(grokPlatforms()))
	}
	for _, platform := range grokPlatforms() {
		key := platform.TargetOS + "-" + platform.TargetArch
		member, ok := want[key]
		if !ok {
			t.Fatalf("missing literal member for %s", key)
		}
		if got := grokArchiveMember(platform.TargetOS); got != member {
			t.Fatalf("%s member=%q want %q", key, got, member)
		}
	}
}

func TestGrokFetcherRejectsForeignTarballURL(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	origin.packages["@xai-official/grok-linux-x64"].tarball = "https://evil.example/grok.tgz"
	fetcher := newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("host err=%v", err)
	}
	if _, tarballs, _ := origin.counts(); tarballs != 0 {
		t.Fatalf("host tarballs=%d", tarballs)
	}

	origin = newGrokTestOrigin(t, version)
	origin.packages["@xai-official/grok-darwin-arm64"].tarball = origin.server.URL + "/other/-/file.tgz"
	fetcher = newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("path err=%v", err)
	}
	if _, tarballs, _ := origin.counts(); tarballs != 0 {
		t.Fatalf("path tarballs=%d", tarballs)
	}

	origin = newGrokTestOrigin(t, version)
	origin.packages[grokRootPackage].tarball = "https://evil.example/root.tgz"
	fetcher = newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("root err=%v", err)
	}
}

func TestGrokFetcherRejectsNonCanonicalIntegrity(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	origin.packages["@xai-official/grok-linux-x64"].integrity = "sha512-abcd"
	fetcher := newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("invalid err=%v", err)
	}

	origin = newGrokTestOrigin(t, version)
	origin.packages["@xai-official/grok-linux-arm64"].integrity = strings.TrimRight(origin.packages["@xai-official/grok-linux-arm64"].integrity, "=")
	fetcher = newGrokTestFetcher(t, origin)
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("unpadded err=%v", err)
	}
}

func TestGrokFetcherRejectsIntegrityMismatch(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	state := origin.packages["@xai-official/grok-linux-x64"]
	state.body = grokPlatformTar(t, grokFixtureArchiveMember(t, "linux"), []byte("tampered-grok-bytes"))
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("err=%v", err)
	}
	assertNoPublishedGrok(t, dir)
}

func TestGrokFetcherRejectsMissingBinary(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	state := origin.packages["@xai-official/grok-darwin-arm64"]
	state.body = grokGzipTar(t, []grokTarEntry{{name: "package/package.json", body: []byte("{}")}})
	state.integrity = grokSRI(state.body)
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	assertNoPublishedGrok(t, dir)
}

func TestGrokFetcherRejectsSymlinkBinary(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	state := origin.packages["@xai-official/grok-linux-arm64"]
	state.body = grokGzipTar(t, []grokTarEntry{
		{name: "package/README.md", body: []byte("readme")},
		{name: grokFixtureArchiveMember(t, "linux"), typeflag: tar.TypeSymlink, link: "package/README.md"},
	})
	state.integrity = grokSRI(state.body)
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err == nil || !errors.Is(err, ErrMetadataInvalid) || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err=%v", err)
	}
	assertNoPublishedGrok(t, dir)
}

func TestGrokFetcherRejectsHardLinkBinary(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	state := origin.packages["@xai-official/grok-linux-x64"]
	state.body = grokGzipTar(t, []grokTarEntry{
		{name: "package/README.md", body: []byte("readme")},
		{name: grokFixtureArchiveMember(t, "linux"), typeflag: tar.TypeLink, link: "package/README.md"},
	})
	state.integrity = grokSRI(state.body)
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err == nil || !errors.Is(err, ErrMetadataInvalid) || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err=%v", err)
	}
	assertNoPublishedGrok(t, dir)
}

func TestGrokFetcherRejectsArchivePathEscape(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	state := origin.packages["@xai-official/grok-darwin-x64"]
	state.body = grokGzipTar(t, []grokTarEntry{
		{name: grokFixtureArchiveMember(t, "darwin"), body: []byte("real-binary")},
		{name: "../outside", body: []byte("escape")},
	})
	state.integrity = grokSRI(state.body)
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	assertNoPublishedGrok(t, dir)
}

func TestGrokFetcherRejectsTooManyArchiveEntries(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	entries := []grokTarEntry{{name: grokFixtureArchiveMember(t, "windows"), body: []byte("windows-binary")}}
	for i := 0; i < grokArchiveEntryLimit; i++ {
		entries = append(entries, grokTarEntry{name: fmt.Sprintf("package/extra-%02d", i), body: []byte("x")})
	}
	state := origin.packages["@xai-official/grok-win32-x64"]
	state.body = grokGzipTar(t, entries)
	state.integrity = grokSRI(state.body)
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("err=%v", err)
	}
	assertNoPublishedGrok(t, dir)
}

func TestGrokFetcherRejectsBadContentLength(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	origin.packages["@xai-official/grok-darwin-x64"].omitLength = true
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("missing length err=%v", err)
	}
	assertNoPublishedGrok(t, dir)

	origin.packages["@xai-official/grok-darwin-x64"].omitLength = false
	origin.packages["@xai-official/grok-darwin-x64"].zeroLength = true
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrMetadataInvalid) || errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("zero length err=%v", err)
	}
	assertNoPublishedGrok(t, dir)

	smallDir := filepath.Join(t.TempDir(), "artifacts")
	small, err := newGrokFetcher(grokFetcherConfig{
		artifactsDir: smallDir, originURL: origin.server.URL, client: origin.server.Client(),
		metadataMax: 1 << 20, sourceMax: 8, bundleMax: 1 << 20, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	origin.packages["@xai-official/grok-darwin-x64"].zeroLength = false
	smallPlan, err := small.PreviewPlan(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := small.FetchExact(t.Context(), smallPlan, "operator:test", nil); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("oversize err=%v", err)
	}
	assertNoPublishedGrok(t, smallDir)
}

func TestGrokFetcherRejectsUnpinnedVersions(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	fetcher := newGrokTestFetcher(t, origin)
	for _, candidate := range []string{"latest", "1.0", "1.0.40-alpha.1", "1.0.40-beta.1", "v1.0.40"} {
		if _, err := fetcher.PreviewPlan(t.Context(), "grok", candidate); !errors.Is(err, ErrInvalidFetchRequest) {
			t.Fatalf("%s err=%v", candidate, err)
		}
	}
	if docs, tarballs, _ := origin.counts(); docs != 0 || tarballs != 0 {
		t.Fatalf("unpinned versions reached origin docs=%d tarballs=%d", docs, tarballs)
	}
}

func TestGrokFetcherRejectsNonProductionOrigin(t *testing.T) {
	_, err := newGrokFetcher(grokFetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"),
		originURL:    "https://example.com",
	})
	if !errors.Is(err, ErrRegistryPolicy) {
		t.Fatalf("err=%v", err)
	}
}

func TestGrokFetcherRejectsRedirect(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := newGrokTestFetcherIn(t, origin, dir)
	origin.redirectDocs = true
	if _, err := fetcher.PreviewPlan(t.Context(), "grok", version); !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("doc redirect err=%v", err)
	}
	if _, tarballs, _ := origin.counts(); tarballs != 0 {
		t.Fatalf("doc redirect tarballs=%d", tarballs)
	}
	origin.redirectDocs = false
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	state := origin.packages["@xai-official/grok-darwin-x64"]
	origin.evilBody = state.body
	state.redirect = origin.server.URL + "/evil-same-bytes"
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); err == nil || errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("tarball redirect err=%v", err)
	}
	if origin.evilN != 0 {
		t.Fatalf("followed redirect evil=%d", origin.evilN)
	}
	assertNoPublishedGrok(t, dir)
}

func TestGrokFetcherRejectsTamperedPlanBeforeDownload(t *testing.T) {
	const version = "1.0.40"
	origin := newGrokTestOrigin(t, version)
	fetcher := newGrokTestFetcher(t, origin)
	plan, err := fetcher.PreviewPlan(t.Context(), "grok", version)
	if err != nil {
		t.Fatal(err)
	}
	plan.SourcePlan = strings.Replace(plan.SourcePlan, `"target_os":"darwin"`, `"target_os":"linux"`, 1)
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("err=%v", err)
	}
	if _, tarballs, _ := origin.counts(); tarballs != 0 {
		t.Fatalf("tarballs=%d", tarballs)
	}
}

func TestGrokProductionPlanFitsSourceBudget(t *testing.T) {
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, sha512.Size))
	assertGrokPlanBudget(t, "1.0.40", integrity)
	long := strings.Repeat("1", 40) + "." + strings.Repeat("2", 40) + "." + strings.Repeat("3", 40)
	if !ValidGrokVersion(long) {
		t.Fatal("long version fixture is not exact")
	}
	assertGrokPlanBudget(t, long, integrity)
}

func assertGrokPlanBudget(t *testing.T, version, integrity string) {
	t.Helper()
	sources := make([]GrokSource, 0, len(grokPlatforms()))
	for _, platform := range grokPlatforms() {
		unscoped := strings.TrimPrefix(platform.Package, grokScopePrefix)
		sources = append(sources, GrokSource{
			TargetOS: platform.TargetOS, TargetArch: platform.TargetArch, Package: platform.Package,
			TarballURL: ProductionRegistryOrigin + "/" + platform.Package + "/-/" + unscoped + "-" + version + ".tgz",
			Integrity:  integrity,
		})
	}
	plan := GrokFetchPlan{
		PolicyVersion: GrokFetchPolicyVersion, Name: "grok", Version: version,
		SourceOrigin:       ProductionRegistryOrigin,
		VersionDocumentURL: ProductionRegistryOrigin + "/" + url.PathEscape(grokRootPackage) + "/" + url.PathEscape(version),
		RootIntegrity:      integrity, Sources: sources, SourceIdentity: integrity,
		SourceMaxBytes: DefaultGrokSourceMaxBytes, BundleMaxBytes: DefaultGrokBundleMaxBytes,
		PreviewedAt:   time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC),
		PreviewDigest: "sha256:" + strings.Repeat("ab", 32),
	}
	raw, err := marshalCompactNoEscape(plan)
	if err != nil || len(raw) > MaxArtifactSourcePlanBytes {
		t.Fatalf("version=%s len=%d err=%v", version, len(raw), err)
	}
}

type grokTarEntry struct {
	name     string
	body     []byte
	typeflag byte
	link     string
}

func grokGzipTar(t *testing.T, entries []grokTarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		flag := entry.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name: entry.name, Mode: 0o644, Size: int64(len(entry.body)),
			Typeflag: flag, Linkname: entry.link, ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
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
	return buf.Bytes()
}

func grokFixtureArchiveMember(t *testing.T, targetOS string) string {
	t.Helper()
	switch targetOS {
	case "darwin", "linux":
		return "package/bin/grok.br"
	case "windows":
		return "package/bin/grok.exe.br"
	default:
		t.Fatalf("fixture archive member for %s", targetOS)
		return ""
	}
}

func grokPlatformTar(t *testing.T, member string, payload []byte) []byte {
	t.Helper()
	return grokGzipTar(t, []grokTarEntry{
		{name: "package/package.json", body: []byte(`{"name":"fixture"}`)},
		{name: "package/README.md", body: []byte("readme")},
		{name: "package/THIRD_PARTY_NOTICES.md", body: []byte("notices")},
		{name: member, body: payload},
	})
}

func grokSRI(body []byte) string {
	sum := sha512.Sum512(body)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

type grokPackageState struct {
	name       string
	version    string
	os         []string
	cpu        []string
	bin        map[string]string
	optional   map[string]string
	tarball    string
	integrity  string
	fileCount  int
	body       []byte
	payload    []byte
	targetOS   string
	targetArch string
	omitLength bool
	zeroLength bool
	redirect   string
}

type grokTestOrigin struct {
	server       *httptest.Server
	mu           sync.Mutex
	version      string
	packages     map[string]*grokPackageState
	docN         int
	tarballN     int
	rootTarballN int
	evilN        int
	evilBody     []byte
	redirectDocs bool
}

func newGrokTestOrigin(t *testing.T, version string) *grokTestOrigin {
	t.Helper()
	origin := &grokTestOrigin{version: version, packages: map[string]*grokPackageState{}}
	origin.server = httptest.NewServer(http.HandlerFunc(origin.serve))
	t.Cleanup(origin.server.Close)
	optional := map[string]string{}
	for _, platform := range grokPlatforms() {
		optional[platform.Package] = version
	}
	origin.packages[grokRootPackage] = &grokPackageState{
		name: grokRootPackage, version: version,
		os: []string{"darwin", "linux", "win32"}, cpu: []string{"arm64", "x64"},
		bin: map[string]string{grokBinName: grokBinPath}, optional: optional,
		tarball:   origin.server.URL + "/" + grokRootPackage + "/-/grok-" + version + ".tgz",
		integrity: grokSRI([]byte("root-not-downloaded")), fileCount: 2,
	}
	for _, platform := range grokPlatforms() {
		payload := []byte("grok-fixture-" + platform.TargetOS + "-" + platform.TargetArch)
		body := grokPlatformTar(t, grokFixtureArchiveMember(t, platform.TargetOS), payload)
		unscoped := strings.TrimPrefix(platform.Package, grokScopePrefix)
		origin.packages[platform.Package] = &grokPackageState{
			name: platform.Package, version: version,
			os: []string{platform.NPMOS}, cpu: []string{platform.NPMCPU},
			tarball:   origin.server.URL + "/" + platform.Package + "/-/" + unscoped + "-" + version + ".tgz",
			integrity: grokSRI(body), fileCount: 4, body: body, payload: payload,
			targetOS: platform.TargetOS, targetArch: platform.TargetArch,
		}
	}
	return origin
}

func (o *grokTestOrigin) serve(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if r.URL.Path == "/evil-same-bytes" {
		o.evilN++
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(o.evilBody)
		return
	}
	if strings.Contains(r.URL.Path, "/-/") {
		for _, state := range o.packages {
			if state.tarball == "" {
				continue
			}
			parsed, err := url.Parse(state.tarball)
			if err != nil || parsed.Path != r.URL.Path {
				continue
			}
			if state.body == nil {
				o.rootTarballN++
				w.WriteHeader(http.StatusNotFound)
				return
			}
			o.tarballN++
			if state.redirect != "" {
				http.Redirect(w, r, state.redirect, http.StatusFound)
				return
			}
			if state.omitLength {
				writeGrokChunked(w, state.body)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			if state.zeroLength {
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = w.Write(state.body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	name, ok := grokVersionDocPackage(r.URL.Path, o.version)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	o.docN++
	if o.redirectDocs {
		http.Redirect(w, r, "https://evil.example/metadata", http.StatusFound)
		return
	}
	state := o.packages[name]
	if state == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	payload := map[string]any{
		"name": state.name, "version": state.version, "license": grokLicense,
		"os": state.os, "cpu": state.cpu, "description": "fixture",
		"dist": map[string]any{
			"tarball": state.tarball, "integrity": state.integrity,
			"unpackedSize": 1024, "fileCount": state.fileCount,
		},
	}
	if state.bin != nil {
		payload["bin"] = state.bin
	}
	if state.optional != nil {
		payload["optionalDependencies"] = state.optional
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func grokVersionDocPackage(path, version string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	suffix := "/" + version
	if !strings.HasSuffix(trimmed, suffix) {
		return "", false
	}
	name := strings.TrimSuffix(trimmed, suffix)
	if name == "" || strings.Contains(name, "/-/") {
		return "", false
	}
	return name, true
}

func writeGrokChunked(w http.ResponseWriter, body []byte) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unavailable", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprintf(bufrw, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(body), body)
	_ = bufrw.Flush()
}

func (o *grokTestOrigin) counts() (docs, tarballs, rootTarballs int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.docN, o.tarballN, o.rootTarballN
}

func newGrokTestFetcher(t *testing.T, origin *grokTestOrigin) *Fetcher {
	t.Helper()
	return newGrokTestFetcherIn(t, origin, filepath.Join(t.TempDir(), "artifacts"))
}

func newGrokTestFetcherIn(t *testing.T, origin *grokTestOrigin, dir string) *Fetcher {
	t.Helper()
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: dir, registryURL: origin.server.URL, client: origin.server.Client(),
		metadataMax: 1 << 20, artifactMax: 1 << 20, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fetcher
}

func assertNoPublishedGrok(t *testing.T, dir string) {
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

func assertGrokTestBundle(t *testing.T, path string, origin *grokTestOrigin) {
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
	want := map[string][]byte{}
	for _, platform := range grokPlatforms() {
		state := origin.packages[platform.Package]
		want[grokBundleMember(platform.TargetOS, platform.TargetArch)] = state.payload
	}
	if _, ok := want["grok/windows-amd64/bin/grok.exe.br"]; !ok {
		t.Fatal("windows bundle path was not pinned")
	}
	if _, ok := want["grok/linux-amd64/bin/grok.br"]; !ok {
		t.Fatal("linux bundle path was not pinned")
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
			if header.Mode != 0o755 || (name != "grok" && !strings.HasPrefix(name, "grok/")) {
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
		t.Fatalf("bundle files=%v", seen)
	}
}
