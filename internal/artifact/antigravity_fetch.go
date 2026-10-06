package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ProductionAntigravityManifestOrigin = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app"
	ProductionAntigravityDownloadOrigin = "https://storage.googleapis.com"
	AntigravityFetchPolicyVersion       = "antigravity-official-bundle:v1"
	ArtifactSourceAntigravity           = "antigravity-bundle:v1"
	DefaultAntigravitySourceMaxBytes    = int64(512 << 20)
	DefaultAntigravityBundleMaxBytes    = int64(1 << 30)
	AntigravityManifestMaxBytes         = int64(64 << 10)
	MaxAntigravityBundleEntries         = 64
	antigravitySourceTempPrefix         = ".antigravity-source-"
)

// ErrUpstreamVersionUnavailable is returned when every Antigravity manifest
// publishes one valid version and it is not the version the operator requested.
var ErrUpstreamVersionUnavailable = errors.New("artifact: upstream does not offer the requested version")

// UpstreamVersionError names the version upstream currently publishes.
// Published is accepted only after ValidAntigravityVersion succeeds.
type UpstreamVersionError struct {
	Name      string
	Requested string
	Published string
}

func (e *UpstreamVersionError) Error() string {
	if e == nil || e.Name != "antigravity" || !ValidAntigravityVersion(e.Published) || !ValidAntigravityVersion(e.Requested) {
		return ErrUpstreamVersionUnavailable.Error()
	}
	return fmt.Sprintf("%s: %s publishes %s", ErrUpstreamVersionUnavailable.Error(), e.Name, e.Published)
}

func (e *UpstreamVersionError) Unwrap() error { return ErrUpstreamVersionUnavailable }

// OperatorSentence is the preview refusal shown to an operator.
func (e *UpstreamVersionError) OperatorSentence() string {
	if e == nil || e.Name != "antigravity" || !ValidAntigravityVersion(e.Published) {
		return ""
	}
	return "上游目前只提供 Antigravity " + e.Published + "；改用這個版號重新 Preview。"
}

type AntigravitySource struct {
	TargetOS   string `json:"target_os"`
	TargetArch string `json:"target_arch"`
	Platform   string `json:"platform"`
	Dir        string `json:"dir"`
	File       string `json:"file"`
	SHA512     string `json:"sha512"`
}

type AntigravityFetchPlan struct {
	PolicyVersion    string              `json:"policy_version"`
	Name             string              `json:"name"`
	Version          string              `json:"version"`
	Build            string              `json:"build"`
	ManifestOrigin   string              `json:"manifest_origin"`
	DownloadOrigin   string              `json:"download_origin"`
	ReleaseDirectory string              `json:"release_directory"`
	Sources          []AntigravitySource `json:"sources"`
	SourceIdentity   string              `json:"source_identity"`
	SourceMaxBytes   int64               `json:"source_max_bytes"`
	BundleMaxBytes   int64               `json:"bundle_max_bytes"`
	PreviewedAt      time.Time           `json:"previewed_at"`
	PreviewDigest    string              `json:"preview_digest"`
}

type AntigravityFetcher struct {
	artifactsDir         string
	manifestOrigin       *url.URL
	manifestOriginString string
	downloadOrigin       *url.URL
	downloadOriginString string
	client               *http.Client
	metadataMax          int64
	sourceMax            int64
	bundleMax            int64
	metadataTimeout      time.Duration
	downloadTimeout      time.Duration
	allowHTTP            bool
	now                  func() time.Time
	serial               *sync.Mutex
}

type antigravityFetcherConfig struct {
	artifactsDir    string
	manifestURL     string
	downloadURL     string
	client          *http.Client
	metadataMax     int64
	sourceMax       int64
	bundleMax       int64
	metadataTimeout time.Duration
	downloadTimeout time.Duration
	allowHTTP       bool
	now             func() time.Time
}

type antigravityPlatform struct {
	TargetOS   string
	TargetArch string
	Platform   string
	Dir        string
	File       string
}

type antigravityManifestDocument struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512"`
}

func ValidAntigravityVersion(version string) bool { return validNodeRuntimeVersion(version) }

func antigravityPlatforms() []antigravityPlatform {
	return []antigravityPlatform{
		{TargetOS: "linux", TargetArch: "amd64", Platform: "linux_amd64", Dir: "linux-x64", File: "cli_linux_x64.tar.gz"},
		{TargetOS: "linux", TargetArch: "arm64", Platform: "linux_arm64", Dir: "linux-arm", File: "cli_linux_arm64.tar.gz"},
		{TargetOS: "darwin", TargetArch: "amd64", Platform: "darwin_amd64", Dir: "darwin-x64", File: "cli_mac_x64.tar.gz"},
		{TargetOS: "darwin", TargetArch: "arm64", Platform: "darwin_arm64", Dir: "darwin-arm", File: "cli_mac_arm64.tar.gz"},
		{TargetOS: "windows", TargetArch: "amd64", Platform: "windows_amd64", Dir: "windows-x64", File: "cli_windows_x64.exe"},
		{TargetOS: "windows", TargetArch: "arm64", Platform: "windows_arm64", Dir: "windows-arm", File: "cli_windows_arm64.exe"},
	}
}

// AntigravityOfficialFile is the upstream file name for one Go target.
func AntigravityOfficialFile(targetOS, targetArch string) (string, bool) {
	for _, platform := range antigravityPlatforms() {
		if platform.TargetOS == targetOS && platform.TargetArch == targetArch {
			return platform.File, true
		}
	}
	return "", false
}

func antigravityProductionFileURL(version, build, dir, file string) string {
	return ProductionAntigravityDownloadOrigin + "/antigravity-public/antigravity-cli/" + version + "-" + build + "/" + dir + "/" + file
}

func antigravityReleaseDirectory(version, build string) string {
	return ProductionAntigravityDownloadOrigin + "/antigravity-public/antigravity-cli/" + version + "-" + build + "/"
}

// ValidAntigravityReleaseDirectory reports whether raw is the production release
// directory for version, including the trailing slash.
func ValidAntigravityReleaseDirectory(version, raw string) bool {
	if !ValidAntigravityVersion(version) || raw == "" {
		return false
	}
	prefix := ProductionAntigravityDownloadOrigin + "/antigravity-public/antigravity-cli/" + version + "-"
	if !strings.HasPrefix(raw, prefix) || !strings.HasSuffix(raw, "/") {
		return false
	}
	build := strings.TrimSuffix(strings.TrimPrefix(raw, prefix), "/")
	if strings.Contains(build, "/") || !validAntigravityBuild(build) {
		return false
	}
	return raw == antigravityReleaseDirectory(version, build)
}

func validAntigravityBuild(build string) bool {
	if build == "" || len(build) > 32 {
		return false
	}
	for _, r := range build {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validAntigravitySHA512(value string) bool {
	if len(value) != sha512.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func antigravityBuildFromFileURL(version, dir, file, raw string) (string, bool) {
	prefix := ProductionAntigravityDownloadOrigin + "/antigravity-public/antigravity-cli/" + version + "-"
	suffix := "/" + dir + "/" + file
	if len(raw) <= len(prefix)+len(suffix) || !strings.HasPrefix(raw, prefix) || !strings.HasSuffix(raw, suffix) {
		return "", false
	}
	build := raw[len(prefix) : len(raw)-len(suffix)]
	if !validAntigravityBuild(build) || antigravityProductionFileURL(version, build, dir, file) != raw {
		return "", false
	}
	return build, true
}

func newAntigravityFetcher(config antigravityFetcherConfig) (*AntigravityFetcher, error) {
	dir := filepath.Clean(config.artifactsDir)
	if config.artifactsDir == "" || !filepath.IsAbs(config.artifactsDir) || dir != config.artifactsDir ||
		filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("%w: artifacts directory must be a canonical absolute non-root path", ErrInvalidFetchRequest)
	}
	manifest, manifestString, err := parseRegistryURL(config.manifestURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	download, downloadString, err := parseRegistryURL(config.downloadURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	if !config.allowHTTP && manifestString != ProductionAntigravityManifestOrigin {
		return nil, fmt.Errorf("%w: Antigravity manifest origin must be %s", ErrRegistryPolicy, ProductionAntigravityManifestOrigin)
	}
	if !config.allowHTTP && downloadString != ProductionAntigravityDownloadOrigin {
		return nil, fmt.Errorf("%w: Antigravity download origin must be %s", ErrRegistryPolicy, ProductionAntigravityDownloadOrigin)
	}
	if config.metadataMax <= 0 || config.metadataMax > AntigravityManifestMaxBytes {
		config.metadataMax = AntigravityManifestMaxBytes
	}
	if config.sourceMax <= 0 {
		config.sourceMax = DefaultAntigravitySourceMaxBytes
	}
	if config.bundleMax <= 0 {
		config.bundleMax = DefaultAntigravityBundleMaxBytes
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
	return &AntigravityFetcher{
		artifactsDir: dir, manifestOrigin: manifest, manifestOriginString: manifestString,
		downloadOrigin: download, downloadOriginString: downloadString, client: client,
		metadataMax: config.metadataMax, sourceMax: config.sourceMax, bundleMax: config.bundleMax,
		metadataTimeout: config.metadataTimeout, downloadTimeout: config.downloadTimeout,
		allowHTTP: config.allowHTTP, now: config.now, serial: lock.(*sync.Mutex),
	}, nil
}

func (f *AntigravityFetcher) manifestURL(platform string) string {
	return f.manifestOriginString + "/manifests/" + platform + ".json"
}

func (f *AntigravityFetcher) downloadURL(version, build, dir, file string) string {
	return f.downloadOriginString + "/antigravity-public/antigravity-cli/" + version + "-" + build + "/" + dir + "/" + file
}

func (f *AntigravityFetcher) PreviewPlan(ctx context.Context, version string) (AntigravityFetchPlan, error) {
	if f == nil || f.client == nil || f.manifestOrigin == nil || f.downloadOrigin == nil || ctx == nil || !ValidAntigravityVersion(version) {
		return AntigravityFetchPlan{}, fmt.Errorf("%w: exact antigravity version and fetcher are required", ErrInvalidFetchRequest)
	}
	requestCtx, cancel := context.WithTimeout(ctx, f.metadataTimeout)
	defer cancel()
	platforms := antigravityPlatforms()
	documents := make([]antigravityManifestDocument, len(platforms))
	for i, platform := range platforms {
		document, err := f.readManifest(requestCtx, platform.Platform)
		if err != nil {
			return AntigravityFetchPlan{}, err
		}
		documents[i] = document
	}
	if _, err := antigravityPublishedVersion(version, documents); err != nil {
		return AntigravityFetchPlan{}, err
	}
	build := ""
	sources := make([]AntigravitySource, len(platforms))
	for i, platform := range platforms {
		gotBuild, ok := antigravityBuildFromFileURL(version, platform.Dir, platform.File, documents[i].URL)
		if !ok || !validAntigravitySHA512(documents[i].SHA512) {
			return AntigravityFetchPlan{}, fmt.Errorf("%w: Antigravity manifest does not match the pinned file", ErrMetadataInvalid)
		}
		if build == "" {
			build = gotBuild
		} else if gotBuild != build {
			return AntigravityFetchPlan{}, fmt.Errorf("%w: Antigravity manifests do not share one build", ErrMetadataInvalid)
		}
		sources[i] = AntigravitySource{
			TargetOS: platform.TargetOS, TargetArch: platform.TargetArch, Platform: platform.Platform,
			Dir: platform.Dir, File: platform.File, SHA512: documents[i].SHA512,
		}
	}
	now := f.now().UTC()
	if now.IsZero() {
		return AntigravityFetchPlan{}, fmt.Errorf("%w: clock returned zero preview time", ErrInvalidFetchRequest)
	}
	plan := AntigravityFetchPlan{
		PolicyVersion: AntigravityFetchPolicyVersion, Name: "antigravity", Version: version, Build: build,
		ManifestOrigin: f.manifestOriginString, DownloadOrigin: f.downloadOriginString,
		ReleaseDirectory: antigravityReleaseDirectory(version, build), Sources: sources,
		SourceMaxBytes: f.sourceMax, BundleMaxBytes: f.bundleMax, PreviewedAt: now,
	}
	plan.SourceIdentity = antigravitySourceIdentity(plan)
	plan.PreviewDigest = antigravityPreviewDigest(plan)
	return plan, nil
}

func antigravityPublishedVersion(requested string, documents []antigravityManifestDocument) (string, error) {
	if len(documents) == 0 || documents[0].Version == "" {
		return "", fmt.Errorf("%w: Antigravity manifest is incomplete", ErrMetadataInvalid)
	}
	published := documents[0].Version
	for _, document := range documents[1:] {
		if document.Version != published {
			return "", fmt.Errorf("%w: Antigravity manifests do not offer one version", ErrMetadataInvalid)
		}
	}
	if !ValidAntigravityVersion(published) {
		return "", fmt.Errorf("%w: Antigravity manifest version is not major.minor.patch", ErrMetadataInvalid)
	}
	if published != requested {
		return "", &UpstreamVersionError{Name: "antigravity", Requested: requested, Published: published}
	}
	return published, nil
}

func (f *AntigravityFetcher) readManifest(ctx context.Context, platform string) (antigravityManifestDocument, error) {
	var document antigravityManifestDocument
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.manifestURL(platform), nil)
	if err != nil {
		return document, fmt.Errorf("%w: build Antigravity manifest request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return document, fmt.Errorf("%w: read Antigravity manifest: %v", ErrMetadataInvalid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return document, fmt.Errorf("%w: Antigravity manifest returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return document, fmt.Errorf("%w: Antigravity manifest Content-Encoding is not identity", ErrMetadataInvalid)
	}
	body, err := readBoundedBody(resp.Body, resp.ContentLength, f.metadataMax, ErrMetadataTooLarge)
	if err != nil {
		if errors.Is(err, ErrMetadataTooLarge) {
			return document, err
		}
		return document, fmt.Errorf("%w: Antigravity manifest: %v", ErrMetadataInvalid, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("%w: Antigravity manifest is not JSON", ErrMetadataInvalid)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, fmt.Errorf("%w: Antigravity manifest contains trailing JSON", ErrMetadataInvalid)
	}
	if document.Version == "" || document.URL == "" || document.SHA512 == "" {
		return document, fmt.Errorf("%w: Antigravity manifest is incomplete", ErrMetadataInvalid)
	}
	return document, nil
}

func antigravitySourceIdentity(plan AntigravityFetchPlan) string {
	body := struct {
		Policy         string              `json:"policy_version"`
		Version        string              `json:"version"`
		Build          string              `json:"build"`
		ManifestOrigin string              `json:"manifest_origin"`
		DownloadOrigin string              `json:"download_origin"`
		Sources        []AntigravitySource `json:"sources"`
	}{plan.PolicyVersion, plan.Version, plan.Build, plan.ManifestOrigin, plan.DownloadOrigin, plan.Sources}
	raw, _ := marshalCompactNoEscape(body)
	sum := sha512.Sum512(raw)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func antigravityPreviewDigest(plan AntigravityFetchPlan) string {
	body := struct {
		PolicyVersion    string              `json:"policy_version"`
		Name             string              `json:"name"`
		Version          string              `json:"version"`
		Build            string              `json:"build"`
		ManifestOrigin   string              `json:"manifest_origin"`
		DownloadOrigin   string              `json:"download_origin"`
		ReleaseDirectory string              `json:"release_directory"`
		Sources          []AntigravitySource `json:"sources"`
		SourceIdentity   string              `json:"source_identity"`
		SourceMaxBytes   int64               `json:"source_max_bytes"`
		BundleMaxBytes   int64               `json:"bundle_max_bytes"`
	}{plan.PolicyVersion, plan.Name, plan.Version, plan.Build, plan.ManifestOrigin, plan.DownloadOrigin,
		plan.ReleaseDirectory, plan.Sources, plan.SourceIdentity, plan.SourceMaxBytes, plan.BundleMaxBytes}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeAntigravitySourcePlan(raw string) (AntigravityFetchPlan, error) {
	var plan AntigravityFetchPlan
	if raw == "" || len(raw) > MaxArtifactSourcePlanBytes {
		return plan, fmt.Errorf("%w: Antigravity source plan is missing or oversized", ErrInvalidFetchRequest)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("%w: decode Antigravity source plan", ErrInvalidFetchRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return plan, fmt.Errorf("%w: Antigravity source plan has trailing data", ErrInvalidFetchRequest)
	}
	canonical, err := marshalCompactNoEscape(plan)
	if err != nil || string(canonical) != raw {
		return plan, fmt.Errorf("%w: Antigravity source plan is not canonical", ErrInvalidFetchRequest)
	}
	return plan, nil
}

func ValidAntigravitySourcePlan(raw string) bool {
	_, err := decodeAntigravitySourcePlan(raw)
	return err == nil
}

func (f *AntigravityFetcher) validatePlan(plan AntigravityFetchPlan) error {
	if plan.PolicyVersion != AntigravityFetchPolicyVersion || plan.Name != "antigravity" ||
		!ValidAntigravityVersion(plan.Version) || !validAntigravityBuild(plan.Build) ||
		plan.ManifestOrigin != f.manifestOriginString || plan.DownloadOrigin != f.downloadOriginString ||
		plan.ReleaseDirectory != antigravityReleaseDirectory(plan.Version, plan.Build) ||
		plan.SourceMaxBytes != f.sourceMax || plan.BundleMaxBytes != f.bundleMax ||
		plan.PreviewedAt.IsZero() || plan.PreviewedAt.Location() != time.UTC ||
		len(plan.Sources) != len(antigravityPlatforms()) {
		return fmt.Errorf("%w: Antigravity plan does not match active policy", ErrInvalidFetchRequest)
	}
	want := antigravityPlatforms()
	for i := range want {
		got := plan.Sources[i]
		pinned := antigravityProductionFileURL(plan.Version, plan.Build, want[i].Dir, want[i].File)
		if got.TargetOS != want[i].TargetOS || got.TargetArch != want[i].TargetArch ||
			got.Platform != want[i].Platform || got.Dir != want[i].Dir || got.File != want[i].File ||
			!validAntigravitySHA512(got.SHA512) || pinned == "" {
			return fmt.Errorf("%w: Antigravity source identity is invalid", ErrInvalidFetchRequest)
		}
	}
	if plan.SourceIdentity != antigravitySourceIdentity(plan) || plan.PreviewDigest != antigravityPreviewDigest(plan) {
		return fmt.Errorf("%w: Antigravity plan digest does not match", ErrInvalidFetchRequest)
	}
	return nil
}

func (f *AntigravityFetcher) FetchExact(ctx context.Context, plan AntigravityFetchPlan, fetchedBy string,
	progress ProgressFunc,
) (Sidecar, bool, error) {
	if f == nil || f.client == nil || f.serial == nil || ctx == nil {
		return Sidecar{}, false, fmt.Errorf("%w: Antigravity fetcher or context is incomplete", ErrInvalidFetchRequest)
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
	sourcePaths := make(map[string]string, len(plan.Sources))
	for _, source := range plan.Sources {
		temp, err := f.downloadSource(downloadCtx, plan, source)
		if err != nil {
			return Sidecar{}, false, err
		}
		defer os.Remove(temp)
		sourcePaths[source.TargetOS+"-"+source.TargetArch] = temp
	}
	tempPath, sha256Hex, size, err := f.buildBundle(downloadCtx, plan, sourcePaths)
	if err != nil {
		return Sidecar{}, false, err
	}
	defer os.Remove(tempPath)
	if err := reportFetchProgress(progress, FetchPhaseVerifying, size, &size); err != nil {
		return Sidecar{}, false, err
	}
	record := Sidecar{
		Name: "antigravity", Version: plan.Version, TarballURL: plan.ReleaseDirectory,
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

func (f *AntigravityFetcher) findCachedExact(ctx context.Context, plan AntigravityFetchPlan) (Sidecar, bool, error) {
	entries, err := ScanCatalog(f.artifactsDir)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: read artifact catalog: %v", ErrArtifactStorage, err)
	}
	for _, entry := range entries {
		if entry.Record == nil || (entry.Status != CatalogAvailableUnverified && entry.Status != CatalogReady) {
			continue
		}
		record := *entry.Record
		if record.Name != "antigravity" || record.Version != plan.Version || record.TarballURL != plan.ReleaseDirectory ||
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

func (f *AntigravityFetcher) downloadSource(ctx context.Context, plan AntigravityFetchPlan, source AntigravitySource) (string, error) {
	rawURL := f.downloadURL(plan.Version, plan.Build, source.Dir, source.File)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build Antigravity file request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: download Antigravity file: %v", ErrMetadataInvalid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("%w: Antigravity file returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("%w: Antigravity file Content-Encoding is not identity", ErrMetadataInvalid)
	}
	temp, err := os.CreateTemp(f.artifactsDir, antigravitySourceTempPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("%w: create Antigravity file temp: %v", ErrArtifactStorage, err)
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
		return "", fmt.Errorf("%w: chmod Antigravity file temp: %v", ErrArtifactStorage, err)
	}
	digest := sha512.New()
	written, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(resp.Body, plan.SourceMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: copy Antigravity file: %v", ErrMetadataInvalid, err)
	}
	if written > plan.SourceMaxBytes {
		return "", fmt.Errorf("%w: Antigravity file exceeds %d bytes", ErrArtifactTooLarge, plan.SourceMaxBytes)
	}
	if written <= 0 {
		return "", fmt.Errorf("%w: Antigravity file is empty", ErrMetadataInvalid)
	}
	if hex.EncodeToString(digest.Sum(nil)) != source.SHA512 {
		return "", fmt.Errorf("%w: Antigravity file SHA-512 mismatch", ErrIntegrityMismatch)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("%w: fsync Antigravity file: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, nil
}

func (f *AntigravityFetcher) buildBundle(ctx context.Context, plan AntigravityFetchPlan, sourcePaths map[string]string) (string, string, int64, error) {
	temp, err := os.CreateTemp(f.artifactsDir, artifactFetchTempPrefix+"*.tmp")
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Antigravity bundle temp: %v", ErrArtifactStorage, err)
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
		return "", "", 0, fmt.Errorf("%w: chmod Antigravity bundle temp: %v", ErrArtifactStorage, err)
	}
	digest := sha256.New()
	output := &boundedHashWriter{w: temp, hash: digest, maximum: plan.BundleMaxBytes}
	gz, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Antigravity bundle gzip: %v", ErrArtifactStorage, err)
	}
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	dirs := []string{"antigravity/"}
	for _, source := range plan.Sources {
		dirs = append(dirs, "antigravity/"+source.TargetOS+"-"+source.TargetArch+"/")
	}
	sort.Strings(dirs)
	seenDir := map[string]struct{}{}
	for _, name := range dirs {
		if _, ok := seenDir[name]; ok {
			continue
		}
		seenDir[name] = struct{}{}
		if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeDir, 0, "", false)); err != nil {
			return "", "", 0, fmt.Errorf("%w: write Antigravity bundle directory: %v", ErrArtifactStorage, err)
		}
	}
	for _, source := range plan.Sources {
		if err := ctx.Err(); err != nil {
			return "", "", 0, err
		}
		memberPath := sourcePaths[source.TargetOS+"-"+source.TargetArch]
		if memberPath == "" {
			return "", "", 0, fmt.Errorf("%w: Antigravity bundle is missing a platform", ErrMetadataInvalid)
		}
		name := "antigravity/" + source.TargetOS + "-" + source.TargetArch + "/" + source.File
		if err := writeAntigravityBundleFile(tw, name, memberPath); err != nil {
			return "", "", 0, err
		}
		manifest, err := antigravityPinnedManifest(plan.Version, plan.Build, source)
		if err != nil {
			return "", "", 0, err
		}
		manifestName := "antigravity/" + source.TargetOS + "-" + source.TargetArch + "/manifest.json"
		if err := tw.WriteHeader(nodeRuntimeBundleHeader(manifestName, tar.TypeReg, int64(len(manifest)), "", false)); err != nil {
			return "", "", 0, fmt.Errorf("%w: write Antigravity manifest header: %v", ErrArtifactStorage, err)
		}
		if _, err := tw.Write(manifest); err != nil {
			return "", "", 0, fmt.Errorf("%w: write Antigravity manifest: %v", ErrArtifactStorage, err)
		}
	}
	if err := tw.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Antigravity bundle tar: %v", ErrArtifactStorage, err)
	}
	if err := gz.Close(); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return "", "", 0, ErrArtifactTooLarge
		}
		return "", "", 0, fmt.Errorf("%w: close Antigravity bundle gzip: %v", ErrArtifactStorage, err)
	}
	if output.written == 0 {
		return "", "", 0, fmt.Errorf("%w: Antigravity bundle is empty", ErrMetadataInvalid)
	}
	if err := temp.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("%w: fsync Antigravity bundle: %v", ErrArtifactStorage, err)
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Antigravity bundle: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, hex.EncodeToString(digest.Sum(nil)), output.written, nil
}

func antigravityPinnedManifest(version, build string, source AntigravitySource) ([]byte, error) {
	raw, err := marshalCompactNoEscape(antigravityManifestDocument{
		Version: version,
		URL:     antigravityProductionFileURL(version, build, source.Dir, source.File),
		SHA512:  source.SHA512,
	})
	if err != nil || int64(len(raw)) > AntigravityManifestMaxBytes {
		return nil, fmt.Errorf("%w: encode Antigravity manifest", ErrMetadataInvalid)
	}
	return raw, nil
}

func writeAntigravityBundleFile(tw *tar.Writer, name, pathName string) error {
	file, err := os.Open(pathName)
	if err != nil {
		return fmt.Errorf("%w: open Antigravity staged member: %v", ErrArtifactStorage, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat Antigravity staged member: %v", ErrArtifactStorage, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > DefaultAntigravitySourceMaxBytes {
		return fmt.Errorf("%w: Antigravity staged member is not a regular file", ErrIntegrityMismatch)
	}
	if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeReg, info.Size(), "", false)); err != nil {
		return fmt.Errorf("%w: write Antigravity bundle header: %v", ErrArtifactStorage, err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("%w: write Antigravity bundle file: %v", ErrArtifactStorage, err)
	}
	return nil
}
