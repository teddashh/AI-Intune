package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	GrokFetchPolicyVersion    = "grok-official-bundle:v1"
	ArtifactSourceGrok        = "grok-bundle:v1"
	DefaultGrokSourceMaxBytes = int64(512 << 20)
	DefaultGrokBundleMaxBytes = int64(1 << 30)
	MaxGrokBinaryBytes        = int64(512 << 20)
	MaxGrokBundleEntries      = 64
	grokRootPackage           = "@xai-official/grok"
	grokScopePrefix           = "@xai-official/"
	grokArchiveEntryLimit     = 32
	grokSourceTempPrefix      = ".grok-source-"
	grokLicense               = "Apache-2.0"
	grokBinName               = "grok"
	grokBinPath               = "bin/grok"
)

type GrokSource struct {
	TargetOS   string `json:"target_os"`
	TargetArch string `json:"target_arch"`
	Package    string `json:"package"`
	TarballURL string `json:"tarball_url"`
	Integrity  string `json:"integrity"`
}

type GrokFetchPlan struct {
	PolicyVersion      string       `json:"policy_version"`
	Name               string       `json:"name"`
	Version            string       `json:"version"`
	SourceOrigin       string       `json:"source_origin"`
	VersionDocumentURL string       `json:"version_document_url"`
	RootIntegrity      string       `json:"root_integrity"`
	Sources            []GrokSource `json:"sources"`
	SourceIdentity     string       `json:"source_identity"`
	SourceMaxBytes     int64        `json:"source_max_bytes"`
	BundleMaxBytes     int64        `json:"bundle_max_bytes"`
	PreviewedAt        time.Time    `json:"previewed_at"`
	PreviewDigest      string       `json:"preview_digest"`
}

type GrokFetcher struct {
	artifactsDir    string
	origin          *url.URL
	originString    string
	client          *http.Client
	metadataMax     int64
	sourceMax       int64
	bundleMax       int64
	metadataTimeout time.Duration
	downloadTimeout time.Duration
	allowHTTP       bool
	now             func() time.Time
	serial          *sync.Mutex
}

type grokFetcherConfig struct {
	artifactsDir    string
	originURL       string
	client          *http.Client
	metadataMax     int64
	sourceMax       int64
	bundleMax       int64
	metadataTimeout time.Duration
	downloadTimeout time.Duration
	allowHTTP       bool
	now             func() time.Time
}

type grokPlatform struct {
	TargetOS   string
	TargetArch string
	NPMOS      string
	NPMCPU     string
	Package    string
}

type grokVersionDocument struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	License              string            `json:"license"`
	OS                   []string          `json:"os"`
	CPU                  []string          `json:"cpu"`
	Bin                  map[string]string `json:"bin"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	Dist                 struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
		FileCount int    `json:"fileCount"`
	} `json:"dist"`
}

func ValidGrokVersion(version string) bool { return validNodeRuntimeVersion(version) }

func newGrokFetcher(config grokFetcherConfig) (*GrokFetcher, error) {
	dir := filepath.Clean(config.artifactsDir)
	if config.artifactsDir == "" || !filepath.IsAbs(config.artifactsDir) || dir != config.artifactsDir ||
		filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("%w: artifacts directory must be a canonical absolute non-root path", ErrInvalidFetchRequest)
	}
	origin, originString, err := parseRegistryURL(config.originURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	if !config.allowHTTP && originString != ProductionRegistryOrigin {
		return nil, fmt.Errorf("%w: Grok origin must be %s", ErrRegistryPolicy, ProductionRegistryOrigin)
	}
	if config.metadataMax <= 0 {
		config.metadataMax = DefaultMetadataMaxBytes
	}
	if config.sourceMax <= 0 {
		config.sourceMax = DefaultGrokSourceMaxBytes
	}
	if config.bundleMax <= 0 {
		config.bundleMax = DefaultGrokBundleMaxBytes
	}
	if config.metadataTimeout <= 0 {
		config.metadataTimeout = productionMetadataTimeout
	}
	if config.downloadTimeout <= 0 {
		config.downloadTimeout = productionDownloadTimeout
	}
	if config.now == nil {
		config.now = time.Now
	}
	client, err := hardenedFetchHTTPClient(config.client, config.metadataTimeout, config.downloadTimeout)
	if err != nil {
		return nil, err
	}
	lock, _ := fetchDirectoryLocks.LoadOrStore(dir, &sync.Mutex{})
	return &GrokFetcher{
		artifactsDir: dir, origin: origin, originString: originString, client: client,
		metadataMax: config.metadataMax, sourceMax: config.sourceMax, bundleMax: config.bundleMax,
		metadataTimeout: config.metadataTimeout, downloadTimeout: config.downloadTimeout,
		allowHTTP: config.allowHTTP, now: config.now, serial: lock.(*sync.Mutex),
	}, nil
}

func grokPlatforms() []grokPlatform {
	return []grokPlatform{
		{TargetOS: "darwin", TargetArch: "amd64", NPMOS: "darwin", NPMCPU: "x64", Package: "@xai-official/grok-darwin-x64"},
		{TargetOS: "darwin", TargetArch: "arm64", NPMOS: "darwin", NPMCPU: "arm64", Package: "@xai-official/grok-darwin-arm64"},
		{TargetOS: "linux", TargetArch: "amd64", NPMOS: "linux", NPMCPU: "x64", Package: "@xai-official/grok-linux-x64"},
		{TargetOS: "linux", TargetArch: "arm64", NPMOS: "linux", NPMCPU: "arm64", Package: "@xai-official/grok-linux-arm64"},
		{TargetOS: "windows", TargetArch: "amd64", NPMOS: "win32", NPMCPU: "x64", Package: "@xai-official/grok-win32-x64"},
		{TargetOS: "windows", TargetArch: "arm64", NPMOS: "win32", NPMCPU: "arm64", Package: "@xai-official/grok-win32-arm64"},
	}
}

func (f *GrokFetcher) versionDocumentURL(name, version string) string {
	return f.originString + "/" + url.PathEscape(name) + "/" + url.PathEscape(version)
}

func (f *GrokFetcher) tarballURL(pkg, version string) (string, error) {
	unscoped, ok := strings.CutPrefix(pkg, grokScopePrefix)
	if !ok || unscoped == "" || strings.Contains(unscoped, "/") {
		return "", fmt.Errorf("%w: Grok package name is invalid", ErrMetadataInvalid)
	}
	return f.originString + "/" + pkg + "/-/" + unscoped + "-" + version + ".tgz", nil
}

func (f *GrokFetcher) PreviewPlan(ctx context.Context, version string) (GrokFetchPlan, error) {
	if f == nil || f.client == nil || f.origin == nil || ctx == nil || !ValidGrokVersion(version) {
		return GrokFetchPlan{}, fmt.Errorf("%w: exact grok version and fetcher are required", ErrInvalidFetchRequest)
	}
	requestCtx, cancel := context.WithTimeout(ctx, f.metadataTimeout)
	defer cancel()
	rootURL := f.versionDocumentURL(grokRootPackage, version)
	rootBody, err := f.getJSON(requestCtx, rootURL)
	if err != nil {
		return GrokFetchPlan{}, err
	}
	rootIntegrity, err := f.parseRootDocument(version, rootBody)
	if err != nil {
		return GrokFetchPlan{}, err
	}
	sources := make([]GrokSource, 0, len(grokPlatforms()))
	for _, platform := range grokPlatforms() {
		body, err := f.getJSON(requestCtx, f.versionDocumentURL(platform.Package, version))
		if err != nil {
			return GrokFetchPlan{}, err
		}
		source, err := f.parsePlatformDocument(version, platform, body)
		if err != nil {
			return GrokFetchPlan{}, err
		}
		sources = append(sources, source)
	}
	now := f.now().UTC()
	if now.IsZero() {
		return GrokFetchPlan{}, fmt.Errorf("%w: clock returned zero preview time", ErrInvalidFetchRequest)
	}
	plan := GrokFetchPlan{
		PolicyVersion: GrokFetchPolicyVersion, Name: "grok", Version: version,
		SourceOrigin: f.originString, VersionDocumentURL: rootURL, RootIntegrity: rootIntegrity,
		Sources: sources, SourceMaxBytes: f.sourceMax, BundleMaxBytes: f.bundleMax, PreviewedAt: now,
	}
	plan.SourceIdentity = grokSourceIdentity(plan)
	plan.PreviewDigest = grokPreviewDigest(plan)
	if err := f.validatePlan(plan); err != nil {
		return GrokFetchPlan{}, err
	}
	return plan, nil
}

func (f *GrokFetcher) getJSON(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build Grok metadata request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: read Grok metadata: %v", ErrMetadataInvalid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("%w: Grok metadata returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, fmt.Errorf("%w: Grok metadata Content-Encoding is not identity", ErrMetadataInvalid)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaErr != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return nil, fmt.Errorf("%w: Grok metadata Content-Type is not JSON", ErrMetadataInvalid)
	}
	body, err := readBoundedBody(resp.Body, resp.ContentLength, f.metadataMax, ErrMetadataTooLarge)
	if err != nil {
		return nil, fmt.Errorf("%w: Grok metadata: %v", ErrMetadataInvalid, err)
	}
	return body, nil
}

func decodeGrokVersionDocument(body []byte) (grokVersionDocument, error) {
	var document grokVersionDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("%w: Grok version document is not JSON", ErrMetadataInvalid)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, fmt.Errorf("%w: Grok version document contains trailing JSON", ErrMetadataInvalid)
	}
	return document, nil
}

func (f *GrokFetcher) parseRootDocument(version string, body []byte) (string, error) {
	document, err := decodeGrokVersionDocument(body)
	if err != nil {
		return "", err
	}
	if document.Name != grokRootPackage || document.Version != version || document.License != grokLicense {
		return "", fmt.Errorf("%w: Grok package identity does not match the request", ErrMetadataInvalid)
	}
	if !grokSameSet(document.OS, []string{"darwin", "linux", "win32"}) || !grokSameSet(document.CPU, []string{"arm64", "x64"}) {
		return "", fmt.Errorf("%w: Grok package platforms do not match the pinned set", ErrMetadataInvalid)
	}
	if len(document.Bin) != 1 || document.Bin[grokBinName] != grokBinPath {
		return "", fmt.Errorf("%w: Grok package bin entry is invalid", ErrMetadataInvalid)
	}
	if err := grokPinnedDependencies(version, document.OptionalDependencies); err != nil {
		return "", err
	}
	wantURL, err := f.tarballURL(grokRootPackage, version)
	if err != nil {
		return "", err
	}
	if document.Dist.Tarball != wantURL {
		return "", fmt.Errorf("%w: Grok root tarball URL is not on the pinned origin", ErrMetadataInvalid)
	}
	integrity, err := grokCanonicalIntegrity(document.Dist.Integrity)
	if err != nil {
		return "", err
	}
	return integrity, nil
}

func grokPinnedDependencies(version string, deps map[string]string) error {
	if len(deps) != len(grokPlatforms()) {
		return fmt.Errorf("%w: Grok optionalDependencies are not the pinned platform set", ErrMetadataInvalid)
	}
	for _, platform := range grokPlatforms() {
		if deps[platform.Package] != version {
			return fmt.Errorf("%w: Grok optionalDependencies are not the pinned platform set", ErrMetadataInvalid)
		}
	}
	return nil
}

func (f *GrokFetcher) parsePlatformDocument(version string, platform grokPlatform, body []byte) (GrokSource, error) {
	document, err := decodeGrokVersionDocument(body)
	if err != nil {
		return GrokSource{}, err
	}
	if document.Name != platform.Package || document.Version != version || document.License != grokLicense {
		return GrokSource{}, fmt.Errorf("%w: Grok platform %s identity does not match the request", ErrMetadataInvalid, platform.Package)
	}
	if len(document.OS) != 1 || document.OS[0] != platform.NPMOS || len(document.CPU) != 1 || document.CPU[0] != platform.NPMCPU {
		return GrokSource{}, fmt.Errorf("%w: Grok platform %s os/cpu does not match its name", ErrMetadataInvalid, platform.Package)
	}
	if document.Dist.FileCount != 4 {
		return GrokSource{}, fmt.Errorf("%w: Grok platform %s file count is invalid", ErrMetadataInvalid, platform.Package)
	}
	wantURL, err := f.tarballURL(platform.Package, version)
	if err != nil {
		return GrokSource{}, err
	}
	if document.Dist.Tarball != wantURL {
		return GrokSource{}, fmt.Errorf("%w: Grok platform %s tarball URL is not on the pinned origin", ErrMetadataInvalid, platform.Package)
	}
	integrity, err := grokCanonicalIntegrity(document.Dist.Integrity)
	if err != nil {
		return GrokSource{}, err
	}
	return GrokSource{
		TargetOS: platform.TargetOS, TargetArch: platform.TargetArch, Package: platform.Package,
		TarballURL: wantURL, Integrity: integrity,
	}, nil
}

func grokCanonicalIntegrity(value string) (string, error) {
	_, canonical, err := decodeCanonicalSHA512SRI(value)
	if err != nil || canonical != value {
		return "", fmt.Errorf("%w: Grok integrity is not canonical sha512 SRI", ErrMetadataInvalid)
	}
	return canonical, nil
}

func grokSameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(want))
	for _, item := range want {
		seen[item]++
	}
	for _, item := range got {
		seen[item]--
	}
	for _, count := range seen {
		if count != 0 {
			return false
		}
	}
	return true
}

func grokSourceIdentity(plan GrokFetchPlan) string {
	body := struct {
		Policy        string       `json:"policy_version"`
		Version       string       `json:"version"`
		Origin        string       `json:"source_origin"`
		RootIntegrity string       `json:"root_integrity"`
		Sources       []GrokSource `json:"sources"`
	}{plan.PolicyVersion, plan.Version, plan.SourceOrigin, plan.RootIntegrity, plan.Sources}
	raw, _ := marshalCompactNoEscape(body)
	sum := sha512.Sum512(raw)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func grokPreviewDigest(plan GrokFetchPlan) string {
	body := struct {
		PolicyVersion      string       `json:"policy_version"`
		Name               string       `json:"name"`
		Version            string       `json:"version"`
		SourceOrigin       string       `json:"source_origin"`
		VersionDocumentURL string       `json:"version_document_url"`
		RootIntegrity      string       `json:"root_integrity"`
		Sources            []GrokSource `json:"sources"`
		SourceIdentity     string       `json:"source_identity"`
		SourceMaxBytes     int64        `json:"source_max_bytes"`
		BundleMaxBytes     int64        `json:"bundle_max_bytes"`
	}{plan.PolicyVersion, plan.Name, plan.Version, plan.SourceOrigin, plan.VersionDocumentURL,
		plan.RootIntegrity, plan.Sources, plan.SourceIdentity, plan.SourceMaxBytes, plan.BundleMaxBytes}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeGrokSourcePlan(raw string) (GrokFetchPlan, error) {
	var plan GrokFetchPlan
	if raw == "" || len(raw) > MaxArtifactSourcePlanBytes {
		return plan, fmt.Errorf("%w: Grok source plan is missing or oversized", ErrInvalidFetchRequest)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("%w: decode Grok source plan", ErrInvalidFetchRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return plan, fmt.Errorf("%w: Grok source plan has trailing data", ErrInvalidFetchRequest)
	}
	canonical, err := marshalCompactNoEscape(plan)
	if err != nil || string(canonical) != raw {
		return plan, fmt.Errorf("%w: Grok source plan is not canonical", ErrInvalidFetchRequest)
	}
	return plan, nil
}

func ValidGrokSourcePlan(raw string) bool {
	_, err := decodeGrokSourcePlan(raw)
	return err == nil
}

func (f *GrokFetcher) validatePlan(plan GrokFetchPlan) error {
	rootURL := f.versionDocumentURL(grokRootPackage, plan.Version)
	if plan.PolicyVersion != GrokFetchPolicyVersion || plan.Name != "grok" ||
		!ValidGrokVersion(plan.Version) || plan.SourceOrigin != f.originString ||
		plan.VersionDocumentURL != rootURL || plan.SourceMaxBytes != f.sourceMax ||
		plan.BundleMaxBytes != f.bundleMax || plan.PreviewedAt.IsZero() ||
		plan.PreviewedAt.Location() != time.UTC || len(plan.Sources) != len(grokPlatforms()) {
		return fmt.Errorf("%w: Grok plan does not match active policy", ErrInvalidFetchRequest)
	}
	if _, err := grokCanonicalIntegrity(plan.RootIntegrity); err != nil {
		return fmt.Errorf("%w: Grok plan root integrity is not canonical", ErrInvalidFetchRequest)
	}
	want := grokPlatforms()
	for i := range want {
		got := plan.Sources[i]
		tarballURL, err := f.tarballURL(want[i].Package, plan.Version)
		if err != nil {
			return err
		}
		if got.TargetOS != want[i].TargetOS || got.TargetArch != want[i].TargetArch ||
			got.Package != want[i].Package || got.TarballURL != tarballURL {
			return fmt.Errorf("%w: Grok source identity is invalid", ErrInvalidFetchRequest)
		}
		if _, err := grokCanonicalIntegrity(got.Integrity); err != nil {
			return fmt.Errorf("%w: Grok source integrity is invalid", ErrInvalidFetchRequest)
		}
	}
	if plan.SourceIdentity != grokSourceIdentity(plan) || plan.PreviewDigest != grokPreviewDigest(plan) {
		return fmt.Errorf("%w: Grok plan digest does not match", ErrInvalidFetchRequest)
	}
	return nil
}

func (f *GrokFetcher) FetchExact(ctx context.Context, plan GrokFetchPlan, fetchedBy string,
	progress ProgressFunc,
) (Sidecar, bool, error) {
	if f == nil || f.client == nil || f.serial == nil || ctx == nil {
		return Sidecar{}, false, fmt.Errorf("%w: Grok fetcher or context is incomplete", ErrInvalidFetchRequest)
	}
	if err := f.validatePlan(plan); err != nil {
		return Sidecar{}, false, err
	}
	if !validBoundedText(fetchedBy, 256, false) {
		return Sidecar{}, false, fmt.Errorf("%w: fetched_by is invalid", ErrInvalidFetchRequest)
	}
	f.serial.Lock()
	defer f.serial.Unlock()
	if err := ensurePrivateArtifactDir(f.artifactsDir); err != nil {
		return Sidecar{}, false, err
	}
	if cached, ok, err := f.findCachedExact(ctx, plan); err != nil {
		return Sidecar{}, false, err
	} else if ok {
		total := cached.Size
		for _, phase := range []FetchPhase{FetchPhaseDownloading, FetchPhaseVerifying, FetchPhasePublishing} {
			if err := reportFetchProgress(progress, phase, cached.Size, &total); err != nil {
				return Sidecar{}, false, err
			}
		}
		return cached, true, nil
	}
	if err := reportFetchProgress(progress, FetchPhaseDownloading, 0, nil); err != nil {
		return Sidecar{}, false, err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, f.downloadTimeout)
	defer cancel()
	memberPaths := make(map[string]string, len(plan.Sources))
	for _, source := range plan.Sources {
		archivePath, err := f.downloadExact(downloadCtx, source.TarballURL, source.Integrity, f.sourceMax)
		if err != nil {
			return Sidecar{}, false, err
		}
		defer os.Remove(archivePath)
		memberPath, err := f.extractMember(downloadCtx, archivePath, grokArchiveMember(source.TargetOS), f.sourceMax)
		if err != nil {
			return Sidecar{}, false, err
		}
		defer os.Remove(memberPath)
		memberPaths[source.TargetOS+"-"+source.TargetArch] = memberPath
	}
	if len(memberPaths) != len(plan.Sources) {
		return Sidecar{}, false, fmt.Errorf("%w: Grok bundle is missing a platform", ErrMetadataInvalid)
	}
	tempPath, sha256Hex, size, err := f.buildBundle(downloadCtx, plan, memberPaths)
	if err != nil {
		return Sidecar{}, false, err
	}
	defer os.Remove(tempPath)
	if err := reportFetchProgress(progress, FetchPhaseVerifying, size, &size); err != nil {
		return Sidecar{}, false, err
	}
	record := Sidecar{
		Name: "grok", Version: plan.Version, TarballURL: plan.VersionDocumentURL,
		SHA512Integrity: plan.SourceIdentity, SHA256: sha256Hex, Size: size,
		FetchedAt: f.now().UTC(), FetchedBy: fetchedBy,
	}
	if record.FetchedAt.IsZero() {
		return Sidecar{}, false, fmt.Errorf("%w: clock returned zero fetch time", ErrInvalidFetchRequest)
	}
	if err := checkExistingSidecarBinding(f.artifactsDir, record); err != nil {
		return Sidecar{}, false, err
	}
	sidecarBytes, err := marshalCompactNoEscape(record)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: encode sidecar: %v", ErrArtifactStorage, err)
	}
	sidecarTemp, err := writeSyncedTemp(f.artifactsDir, artifactSidecarTempPrefix+"*.tmp", append(sidecarBytes, '\n'))
	if err != nil {
		return Sidecar{}, false, err
	}
	defer os.Remove(sidecarTemp)
	if err := reportFetchProgress(progress, FetchPhasePublishing, size, &size); err != nil {
		return Sidecar{}, false, err
	}
	if err := publishTarball(downloadCtx, tempPath, filepath.Join(f.artifactsDir, sha256Hex+".tgz"), record); err != nil {
		return Sidecar{}, false, err
	}
	if err := syncDirectory(f.artifactsDir); err != nil {
		return Sidecar{}, false, err
	}
	if err := reportFetchProgress(progress, FetchPhasePublishing, size, &size); err != nil {
		return Sidecar{}, false, err
	}
	if err := os.Rename(sidecarTemp, filepath.Join(f.artifactsDir, sha256Hex+".json")); err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: publish sidecar: %v", ErrArtifactStorage, err)
	}
	if err := syncDirectory(f.artifactsDir); err != nil {
		return Sidecar{}, false, err
	}
	return record, false, nil
}

func (f *GrokFetcher) findCachedExact(ctx context.Context, plan GrokFetchPlan) (Sidecar, bool, error) {
	entries, err := ScanCatalog(f.artifactsDir)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: read artifact catalog: %v", ErrArtifactStorage, err)
	}
	for _, entry := range entries {
		if entry.Record == nil || (entry.Status != CatalogAvailableUnverified && entry.Status != CatalogReady) {
			continue
		}
		record := *entry.Record
		if record.Name != "grok" || record.Version != plan.Version || record.TarballURL != plan.VersionDocumentURL ||
			record.SHA512Integrity != plan.SourceIdentity || record.EnginesNode != "" || record.Size <= 0 ||
			record.Size > plan.BundleMaxBytes || !validBoundedText(record.FetchedBy, 256, false) {
			continue
		}
		if err := ValidateStoredArtifactContext(ctx, f.artifactsDir, record); err == nil {
			return record, true, nil
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			return Sidecar{}, false, ctxErr
		}
	}
	return Sidecar{}, false, nil
}

func (f *GrokFetcher) downloadExact(ctx context.Context, rawURL, integrity string, maximum int64) (string, error) {
	want, canonical, err := decodeCanonicalSHA512SRI(integrity)
	if err != nil || canonical != integrity {
		return "", fmt.Errorf("%w: Grok integrity is not canonical", ErrInvalidFetchRequest)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build Grok archive request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("artifact: download Grok archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("artifact: Grok archive returned HTTP %d", resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "", fmt.Errorf("%w: Grok archive Content-Encoding is not identity", ErrMetadataInvalid)
	}
	if resp.ContentLength <= 0 {
		return "", fmt.Errorf("%w: Grok archive did not declare a positive size", ErrMetadataInvalid)
	}
	if resp.ContentLength > maximum {
		return "", fmt.Errorf("%w: Grok archive declared %d bytes, limit %d", ErrArtifactTooLarge, resp.ContentLength, maximum)
	}
	temp, err := os.CreateTemp(f.artifactsDir, grokSourceTempPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("%w: create Grok archive temp: %v", ErrArtifactStorage, err)
	}
	pathName := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(pathName)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("%w: chmod Grok archive temp: %v", ErrArtifactStorage, err)
	}
	digest := sha512.New()
	written, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(resp.Body, resp.ContentLength+1))
	if err != nil {
		return "", fmt.Errorf("%w: copy Grok archive: %v", ErrMetadataInvalid, err)
	}
	if written != resp.ContentLength {
		return "", fmt.Errorf("%w: Grok archive size mismatch", ErrIntegrityMismatch)
	}
	if subtle.ConstantTimeCompare(digest.Sum(nil), want) != 1 {
		return "", fmt.Errorf("%w: Grok archive SHA-512 mismatch", ErrIntegrityMismatch)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("%w: fsync Grok archive: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, nil
}

func grokArchiveMember(targetOS string) string {
	if targetOS == "windows" {
		return "package/bin/grok.exe.br"
	}
	return "package/bin/grok.br"
}

func grokBundleMember(targetOS, targetArch string) string {
	name := "grok.br"
	if targetOS == "windows" {
		name = "grok.exe.br"
	}
	return "grok/" + targetOS + "-" + targetArch + "/bin/" + name
}

func GrokBundleMember(targetOS, targetArch string) (string, bool) {
	if !grokTargetSupported(targetOS, targetArch) {
		return "", false
	}
	return grokBundleMember(targetOS, targetArch), true
}

func GrokCommandRelative(targetOS string) (string, bool) {
	switch targetOS {
	case "linux", "darwin":
		return "bin/grok", true
	case "windows":
		return "bin/grok.exe", true
	default:
		return "", false
	}
}

func grokTargetSupported(targetOS, targetArch string) bool {
	switch targetOS + "/" + targetArch {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64":
		return true
	default:
		return false
	}
}

func validGrokArchivePath(name string) bool {
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || strings.ContainsRune(name, 0) {
		return false
	}
	cleaned := path.Clean(name)
	if cleaned != name || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains("/"+cleaned+"/", "/../") {
		return false
	}
	return strings.HasPrefix(cleaned, "package/")
}

func (f *GrokFetcher) extractMember(ctx context.Context, archivePath, member string, maximum int64) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("%w: open Grok archive: %v", ErrArtifactStorage, err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("%w: Grok archive is not gzip", ErrMetadataInvalid)
	}
	defer gz.Close()
	reader := tar.NewReader(&grokLimitedReader{r: gz, max: maximum})
	var extracted string
	found := false
	success := false
	defer func() {
		if !success && extracted != "" {
			_ = os.Remove(extracted)
		}
	}()
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrArtifactTooLarge) {
				return "", err
			}
			return "", fmt.Errorf("%w: read Grok archive: %v", ErrMetadataInvalid, err)
		}
		entries++
		if entries > grokArchiveEntryLimit {
			return "", fmt.Errorf("%w: Grok archive has too many entries", ErrMetadataInvalid)
		}
		if !validGrokArchivePath(header.Name) {
			return "", fmt.Errorf("%w: Grok archive path is invalid", ErrMetadataInvalid)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return "", fmt.Errorf("%w: Grok archive member %s is not a regular file", ErrMetadataInvalid, header.Name)
		}
		if header.Size < 0 || header.Size > maximum {
			return "", fmt.Errorf("%w: Grok archive member %s declared %d bytes", ErrArtifactTooLarge, header.Name, header.Size)
		}
		if header.Name != member {
			if err := discardGrokEntry(reader, header.Size); err != nil {
				return "", err
			}
			continue
		}
		if found || header.Size == 0 {
			return "", fmt.Errorf("%w: Grok archive member %s is duplicated or empty", ErrMetadataInvalid, member)
		}
		pathName, err := f.copyGrokEntry(reader, header.Size)
		if err != nil {
			return "", err
		}
		extracted = pathName
		found = true
	}
	if !found {
		return "", fmt.Errorf("%w: Grok archive lacks %s", ErrMetadataInvalid, member)
	}
	success = true
	return extracted, nil
}

func discardGrokEntry(reader *tar.Reader, size int64) error {
	n, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil {
		return fmt.Errorf("%w: read Grok archive member: %v", ErrMetadataInvalid, err)
	}
	if n != size {
		return fmt.Errorf("%w: Grok archive member size mismatch", ErrIntegrityMismatch)
	}
	return nil
}

func (f *GrokFetcher) copyGrokEntry(reader *tar.Reader, size int64) (string, error) {
	temp, err := os.CreateTemp(f.artifactsDir, grokSourceTempPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("%w: create Grok member temp: %v", ErrArtifactStorage, err)
	}
	pathName := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(pathName)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("%w: chmod Grok member temp: %v", ErrArtifactStorage, err)
	}
	n, err := io.Copy(temp, io.LimitReader(reader, size+1))
	if err != nil {
		return "", fmt.Errorf("%w: copy Grok archive member: %v", ErrMetadataInvalid, err)
	}
	if n != size {
		return "", fmt.Errorf("%w: Grok archive member size mismatch", ErrIntegrityMismatch)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("%w: fsync Grok member: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, nil
}

type grokLimitedReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (l *grokLimitedReader) Read(p []byte) (int, error) {
	if l.max <= 0 || l.n > l.max {
		return 0, fmt.Errorf("%w: Grok archive decompressed data exceeds %d bytes", ErrArtifactTooLarge, l.max)
	}
	remain := l.max - l.n + 1
	if int64(len(p)) > remain {
		p = p[:remain]
	}
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.n > l.max {
		return n, fmt.Errorf("%w: Grok archive decompressed data exceeds %d bytes", ErrArtifactTooLarge, l.max)
	}
	return n, err
}

func (f *GrokFetcher) buildBundle(ctx context.Context, plan GrokFetchPlan, memberPaths map[string]string) (string, string, int64, error) {
	temp, err := os.CreateTemp(f.artifactsDir, artifactFetchTempPrefix+"*.tmp")
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Grok bundle temp: %v", ErrArtifactStorage, err)
	}
	pathName := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(pathName)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return "", "", 0, fmt.Errorf("%w: chmod Grok bundle temp: %v", ErrArtifactStorage, err)
	}
	digest := sha256.New()
	output := &boundedHashWriter{w: temp, hash: digest, maximum: plan.BundleMaxBytes}
	gz, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Grok bundle gzip: %v", ErrArtifactStorage, err)
	}
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	dirs := []string{"grok/"}
	for _, source := range plan.Sources {
		base := "grok/" + source.TargetOS + "-" + source.TargetArch + "/"
		dirs = append(dirs, base, base+"bin/")
	}
	sort.Strings(dirs)
	seenDir := map[string]struct{}{}
	for _, name := range dirs {
		if _, ok := seenDir[name]; ok {
			continue
		}
		seenDir[name] = struct{}{}
		if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeDir, 0, "", false)); err != nil {
			return "", "", 0, fmt.Errorf("%w: write Grok bundle directory: %v", ErrArtifactStorage, err)
		}
	}
	for _, source := range plan.Sources {
		if err := ctx.Err(); err != nil {
			return "", "", 0, err
		}
		memberPath := memberPaths[source.TargetOS+"-"+source.TargetArch]
		if memberPath == "" {
			return "", "", 0, fmt.Errorf("%w: Grok bundle is missing a platform", ErrMetadataInvalid)
		}
		if err := writeGrokBundleFile(tw, grokBundleMember(source.TargetOS, source.TargetArch), memberPath); err != nil {
			return "", "", 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Grok bundle tar: %v", ErrArtifactStorage, err)
	}
	if err := gz.Close(); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return "", "", 0, ErrArtifactTooLarge
		}
		return "", "", 0, fmt.Errorf("%w: close Grok bundle gzip: %v", ErrArtifactStorage, err)
	}
	if output.written == 0 {
		return "", "", 0, fmt.Errorf("%w: Grok bundle is empty", ErrMetadataInvalid)
	}
	if err := temp.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("%w: fsync Grok bundle: %v", ErrArtifactStorage, err)
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Grok bundle: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, hex.EncodeToString(digest.Sum(nil)), output.written, nil
}

func writeGrokBundleFile(tw *tar.Writer, name, pathName string) error {
	file, err := os.Open(pathName)
	if err != nil {
		return fmt.Errorf("%w: open Grok staged member: %v", ErrArtifactStorage, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat Grok staged member: %v", ErrArtifactStorage, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return fmt.Errorf("%w: Grok staged member is not a regular file", ErrIntegrityMismatch)
	}
	if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeReg, info.Size(), "", false)); err != nil {
		return fmt.Errorf("%w: write Grok bundle header: %v", ErrArtifactStorage, err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("%w: write Grok bundle file: %v", ErrArtifactStorage, err)
	}
	return nil
}
