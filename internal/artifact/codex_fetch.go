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
	"fmt"
	"io"
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
	ProductionCodexOrigin        = "https://releases.openai.com"
	CodexFetchPolicyVersion      = "codex-official-bundle:v1"
	ArtifactSourceCodex          = "codex-bundle:v1"
	DefaultCodexSourceMaxBytes   = int64(512 << 20)
	DefaultCodexChecksumMaxBytes = int64(1 << 20)
	DefaultCodexBundleMaxBytes   = int64(1 << 30)
	codexReleaseMaxBytes         = int64(4 << 20)
	codexSourceTempPrefix        = ".codex-source-"
	codexChecksumAsset           = "codex-package_SHA256SUMS"
	codexReleaseTagPrefix        = "rust-v"
)

type CodexSource struct {
	TargetOS   string `json:"target_os"`
	TargetArch string `json:"target_arch"`
	Filename   string `json:"filename"`
	SHA256     string `json:"sha256"`
}

type CodexFetchPlan struct {
	PolicyVersion    string        `json:"policy_version"`
	Name             string        `json:"name"`
	Version          string        `json:"version"`
	SourceOrigin     string        `json:"source_origin"`
	ChecksumURL      string        `json:"checksum_url"`
	ChecksumSHA256   string        `json:"checksum_sha256"`
	Sources          []CodexSource `json:"sources"`
	SourceIdentity   string        `json:"source_identity"`
	SourceMaxBytes   int64         `json:"source_max_bytes"`
	ChecksumMaxBytes int64         `json:"checksum_max_bytes"`
	BundleMaxBytes   int64         `json:"bundle_max_bytes"`
	PreviewedAt      time.Time     `json:"previewed_at"`
	PreviewDigest    string        `json:"preview_digest"`
}

type CodexFetcher struct {
	artifactsDir    string
	origin          *url.URL
	originString    string
	client          *http.Client
	metadataMax     int64
	sourceMax       int64
	checksumMax     int64
	bundleMax       int64
	metadataTimeout time.Duration
	downloadTimeout time.Duration
	allowHTTP       bool
	now             func() time.Time
	serial          *sync.Mutex
}

type codexFetcherConfig struct {
	artifactsDir    string
	originURL       string
	client          *http.Client
	metadataMax     int64
	sourceMax       int64
	checksumMax     int64
	bundleMax       int64
	metadataTimeout time.Duration
	downloadTimeout time.Duration
	allowHTTP       bool
	now             func() time.Time
}

type codexReleaseDocument struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		Digest             string `json:"digest"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func ValidCodexVersion(version string) bool { return validNodeRuntimeVersion(version) }

func CodexPackageFilename(targetOS, targetArch string) (string, bool) {
	for _, source := range codexWantedPlatforms() {
		if source.TargetOS == targetOS && source.TargetArch == targetArch {
			return source.Filename, true
		}
	}
	return "", false
}

func CodexChecksumAssetName() string { return codexChecksumAsset }

func CodexCommandRelative(targetOS string) string {
	if targetOS == "windows" {
		return "bin/codex.exe"
	}
	return "bin/codex"
}

func CodexPackageRequiredPaths(targetOS string) []string {
	switch targetOS {
	case "windows":
		return []string{
			"codex-package.json",
			"bin/codex.exe",
			"bin/codex-code-mode-host.exe",
			"codex-path/rg.exe",
			"codex-resources/codex-command-runner.exe",
			"codex-resources/codex-windows-sandbox-setup.exe",
		}
	case "linux":
		return []string{
			"codex-package.json",
			"bin/codex",
			"bin/codex-code-mode-host",
			"codex-path/rg",
			"codex-resources/bwrap",
		}
	case "darwin":
		return []string{
			"codex-package.json",
			"bin/codex",
			"bin/codex-code-mode-host",
			"codex-path/rg",
		}
	default:
		return nil
	}
}

func CodexPackagePathExecutable(relative string) bool {
	return relative != "" && relative != "codex-package.json"
}

func CleanCodexPackagePath(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: Codex package path is invalid", ErrMetadataInvalid)
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: Codex package path is invalid", ErrMetadataInvalid)
	}
	return cleaned, nil
}

func CodexPackageDigest(body []byte, filename string) (string, error) {
	manifest, err := parseCodexChecksumManifest(body)
	if err != nil {
		return "", err
	}
	digest, ok := manifest[filename]
	if !ok {
		return "", fmt.Errorf("%w: Codex checksum manifest lacks %s", ErrMetadataInvalid, filename)
	}
	return digest, nil
}

func newCodexFetcher(config codexFetcherConfig) (*CodexFetcher, error) {
	dir := filepath.Clean(config.artifactsDir)
	if config.artifactsDir == "" || !filepath.IsAbs(config.artifactsDir) || dir != config.artifactsDir ||
		filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("%w: artifacts directory must be a canonical absolute non-root path", ErrInvalidFetchRequest)
	}
	origin, originString, err := parseRegistryURL(config.originURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	if !config.allowHTTP && originString != ProductionCodexOrigin {
		return nil, fmt.Errorf("%w: Codex origin must be %s", ErrRegistryPolicy, ProductionCodexOrigin)
	}
	if config.metadataMax <= 0 {
		config.metadataMax = codexReleaseMaxBytes
	}
	if config.sourceMax <= 0 {
		config.sourceMax = DefaultCodexSourceMaxBytes
	}
	if config.checksumMax <= 0 {
		config.checksumMax = DefaultCodexChecksumMaxBytes
	}
	if config.bundleMax <= 0 {
		config.bundleMax = DefaultCodexBundleMaxBytes
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
	return &CodexFetcher{
		artifactsDir: dir, origin: origin, originString: originString, client: client,
		metadataMax: config.metadataMax, sourceMax: config.sourceMax, checksumMax: config.checksumMax,
		bundleMax: config.bundleMax, metadataTimeout: config.metadataTimeout,
		downloadTimeout: config.downloadTimeout, allowHTTP: config.allowHTTP, now: config.now,
		serial: lock.(*sync.Mutex),
	}, nil
}

func codexWantedPlatforms() []CodexSource {
	return []CodexSource{
		{TargetOS: "linux", TargetArch: "amd64", Filename: "codex-package-x86_64-unknown-linux-musl.tar.gz"},
		{TargetOS: "linux", TargetArch: "arm64", Filename: "codex-package-aarch64-unknown-linux-musl.tar.gz"},
		{TargetOS: "darwin", TargetArch: "amd64", Filename: "codex-package-x86_64-apple-darwin.tar.gz"},
		{TargetOS: "darwin", TargetArch: "arm64", Filename: "codex-package-aarch64-apple-darwin.tar.gz"},
		{TargetOS: "windows", TargetArch: "amd64", Filename: "codex-package-x86_64-pc-windows-msvc.tar.gz"},
		{TargetOS: "windows", TargetArch: "arm64", Filename: "codex-package-aarch64-pc-windows-msvc.tar.gz"},
	}
}

func (f *CodexFetcher) releaseURL(version, name string) string {
	return strings.TrimSuffix(f.originString, "/") + "/codex/releases/" + url.PathEscape(version) + "/" + url.PathEscape(name)
}

func (f *CodexFetcher) PreviewPlan(ctx context.Context, version string) (CodexFetchPlan, error) {
	if f == nil || f.client == nil || f.origin == nil || ctx == nil || !ValidCodexVersion(version) {
		return CodexFetchPlan{}, fmt.Errorf("%w: exact codex version and fetcher are required", ErrInvalidFetchRequest)
	}
	checksumURL := f.releaseURL(version, "release.json")
	requestCtx, cancel := context.WithTimeout(ctx, f.metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return CodexFetchPlan{}, fmt.Errorf("%w: build Codex release request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return CodexFetchPlan{}, fmt.Errorf("%w: read Codex release: %v", ErrMetadataInvalid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return CodexFetchPlan{}, fmt.Errorf("%w: Codex release returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return CodexFetchPlan{}, fmt.Errorf("%w: Codex release Content-Encoding is not identity", ErrMetadataInvalid)
	}
	body, err := readBoundedBody(resp.Body, resp.ContentLength, f.metadataMax, ErrMetadataTooLarge)
	if err != nil {
		return CodexFetchPlan{}, fmt.Errorf("%w: Codex release: %v", ErrMetadataInvalid, err)
	}
	checksumSHA, sources, err := f.parseCodexRelease(version, body)
	if err != nil {
		return CodexFetchPlan{}, err
	}
	now := f.now().UTC()
	if now.IsZero() {
		return CodexFetchPlan{}, fmt.Errorf("%w: clock returned zero preview time", ErrInvalidFetchRequest)
	}
	plan := CodexFetchPlan{
		PolicyVersion: CodexFetchPolicyVersion, Name: "codex", Version: version,
		SourceOrigin: f.originString, ChecksumURL: checksumURL, ChecksumSHA256: checksumSHA,
		Sources: sources, SourceMaxBytes: f.sourceMax, ChecksumMaxBytes: f.checksumMax,
		BundleMaxBytes: f.bundleMax, PreviewedAt: now,
	}
	plan.SourceIdentity = codexSourceIdentity(plan)
	plan.PreviewDigest = codexPreviewDigest(plan)
	return plan, nil
}

func (f *CodexFetcher) parseCodexRelease(version string, body []byte) (string, []CodexSource, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var document codexReleaseDocument
	if err := decoder.Decode(&document); err != nil {
		return "", nil, fmt.Errorf("%w: Codex release is not JSON", ErrMetadataInvalid)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", nil, fmt.Errorf("%w: Codex release contains trailing JSON", ErrMetadataInvalid)
	}
	if document.TagName != codexReleaseTagPrefix+version {
		return "", nil, fmt.Errorf("%w: Codex release tag does not match the request", ErrMetadataInvalid)
	}
	wanted := map[string]CodexSource{}
	for _, source := range codexWantedPlatforms() {
		wanted[source.Filename] = source
	}
	seen := map[string]struct{}{}
	found := map[string]CodexSource{}
	checksumSHA := ""
	for _, asset := range document.Assets {
		if asset.Name == "" {
			return "", nil, fmt.Errorf("%w: Codex release asset name is empty", ErrMetadataInvalid)
		}
		if _, ok := seen[asset.Name]; ok {
			return "", nil, fmt.Errorf("%w: Codex release asset %s is duplicated", ErrMetadataInvalid, asset.Name)
		}
		seen[asset.Name] = struct{}{}
		source, wantPackage := wanted[asset.Name]
		wantChecksum := asset.Name == codexChecksumAsset
		if !wantPackage && !wantChecksum {
			continue
		}
		digest, ok := codexSHA256Digest(asset.Digest)
		if !ok || asset.BrowserDownloadURL != f.releaseURL(version, asset.Name) {
			return "", nil, fmt.Errorf("%w: Codex release asset %s is not on the pinned origin", ErrMetadataInvalid, asset.Name)
		}
		if wantChecksum {
			checksumSHA = digest
			continue
		}
		source.SHA256 = digest
		found[asset.Name] = source
	}
	if checksumSHA == "" {
		return "", nil, fmt.Errorf("%w: Codex release lacks %s", ErrMetadataInvalid, codexChecksumAsset)
	}
	result := codexWantedPlatforms()
	for i := range result {
		got, ok := found[result[i].Filename]
		if !ok {
			return "", nil, fmt.Errorf("%w: Codex release lacks target %s", ErrMetadataInvalid, result[i].Filename)
		}
		result[i].SHA256 = got.SHA256
	}
	return checksumSHA, result, nil
}

func codexSHA256Digest(value string) (string, bool) {
	encoded, ok := strings.CutPrefix(value, "sha256:")
	if !ok || !ValidSHA256Hex(encoded) {
		return "", false
	}
	return encoded, true
}

func validCodexAssetName(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func parseCodexChecksumManifest(body []byte) (map[string]string, error) {
	if len(body) == 0 || bytes.Contains(body, []byte{'\r'}) || bytes.Contains(body, []byte{0}) {
		return nil, fmt.Errorf("%w: Codex checksum manifest is empty or not plain text", ErrMetadataInvalid)
	}
	text := string(body)
	text = strings.TrimSuffix(text, "\n")
	if text == "" || strings.Contains(text, "\n\n") {
		return nil, fmt.Errorf("%w: Codex checksum manifest is empty or not plain text", ErrMetadataInvalid)
	}
	lines := strings.Split(text, "\n")
	result := make(map[string]string, len(lines))
	for _, line := range lines {
		digest, name, ok := strings.Cut(line, "  ")
		if !ok || strings.Contains(name, " ") || !ValidSHA256Hex(digest) || !validCodexAssetName(name) {
			return nil, fmt.Errorf("%w: Codex checksum manifest line is invalid", ErrMetadataInvalid)
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("%w: Codex checksum manifest repeats %s", ErrMetadataInvalid, name)
		}
		result[name] = digest
	}
	return result, nil
}

func codexSourceIdentity(plan CodexFetchPlan) string {
	body := struct {
		Policy   string        `json:"policy_version"`
		Version  string        `json:"version"`
		Origin   string        `json:"source_origin"`
		Checksum string        `json:"checksum_sha256"`
		Sources  []CodexSource `json:"sources"`
	}{plan.PolicyVersion, plan.Version, plan.SourceOrigin, plan.ChecksumSHA256, plan.Sources}
	raw, _ := marshalCompactNoEscape(body)
	sum := sha512.Sum512(raw)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func codexPreviewDigest(plan CodexFetchPlan) string {
	body := struct {
		PolicyVersion    string        `json:"policy_version"`
		Name             string        `json:"name"`
		Version          string        `json:"version"`
		SourceOrigin     string        `json:"source_origin"`
		ChecksumURL      string        `json:"checksum_url"`
		ChecksumSHA256   string        `json:"checksum_sha256"`
		Sources          []CodexSource `json:"sources"`
		SourceIdentity   string        `json:"source_identity"`
		SourceMaxBytes   int64         `json:"source_max_bytes"`
		ChecksumMaxBytes int64         `json:"checksum_max_bytes"`
		BundleMaxBytes   int64         `json:"bundle_max_bytes"`
	}{plan.PolicyVersion, plan.Name, plan.Version, plan.SourceOrigin, plan.ChecksumURL,
		plan.ChecksumSHA256, plan.Sources, plan.SourceIdentity, plan.SourceMaxBytes,
		plan.ChecksumMaxBytes, plan.BundleMaxBytes}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeCodexSourcePlan(raw string) (CodexFetchPlan, error) {
	var plan CodexFetchPlan
	if raw == "" || len(raw) > MaxArtifactSourcePlanBytes {
		return plan, fmt.Errorf("%w: Codex source plan is missing or oversized", ErrInvalidFetchRequest)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("%w: decode Codex source plan", ErrInvalidFetchRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return plan, fmt.Errorf("%w: Codex source plan has trailing data", ErrInvalidFetchRequest)
	}
	canonical, err := marshalCompactNoEscape(plan)
	if err != nil || string(canonical) != raw {
		return plan, fmt.Errorf("%w: Codex source plan is not canonical", ErrInvalidFetchRequest)
	}
	return plan, nil
}

func ValidCodexSourcePlan(raw string) bool {
	_, err := decodeCodexSourcePlan(raw)
	return err == nil
}

func (f *CodexFetcher) validatePlan(plan CodexFetchPlan) error {
	if plan.PolicyVersion != CodexFetchPolicyVersion || plan.Name != "codex" ||
		!ValidCodexVersion(plan.Version) || plan.SourceOrigin != f.originString ||
		plan.ChecksumURL != f.releaseURL(plan.Version, "release.json") ||
		!ValidSHA256Hex(plan.ChecksumSHA256) ||
		plan.SourceMaxBytes != f.sourceMax || plan.ChecksumMaxBytes != f.checksumMax ||
		plan.BundleMaxBytes != f.bundleMax || plan.PreviewedAt.IsZero() ||
		plan.PreviewedAt.Location() != time.UTC || len(plan.Sources) != 6 {
		return fmt.Errorf("%w: Codex plan does not match active policy", ErrInvalidFetchRequest)
	}
	want := codexWantedPlatforms()
	for i := range want {
		got := plan.Sources[i]
		if got.TargetOS != want[i].TargetOS || got.TargetArch != want[i].TargetArch ||
			got.Filename != want[i].Filename || !ValidSHA256Hex(got.SHA256) {
			return fmt.Errorf("%w: Codex source identity is invalid", ErrInvalidFetchRequest)
		}
	}
	if plan.SourceIdentity != codexSourceIdentity(plan) || plan.PreviewDigest != codexPreviewDigest(plan) {
		return fmt.Errorf("%w: Codex plan digest does not match", ErrInvalidFetchRequest)
	}
	return nil
}

func (f *CodexFetcher) FetchExact(ctx context.Context, plan CodexFetchPlan, fetchedBy string,
	progress ProgressFunc,
) (Sidecar, bool, error) {
	if f == nil || f.client == nil || f.serial == nil || ctx == nil {
		return Sidecar{}, false, fmt.Errorf("%w: Codex fetcher or context is incomplete", ErrInvalidFetchRequest)
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
	checksumPath, err := f.downloadExact(downloadCtx, f.releaseURL(plan.Version, codexChecksumAsset), plan.ChecksumSHA256, f.checksumMax)
	if err != nil {
		return Sidecar{}, false, err
	}
	defer os.Remove(checksumPath)
	checksumBody, err := os.ReadFile(checksumPath)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: read Codex checksum manifest: %v", ErrArtifactStorage, err)
	}
	manifest, err := parseCodexChecksumManifest(checksumBody)
	if err != nil {
		return Sidecar{}, false, err
	}
	for _, source := range plan.Sources {
		if manifest[source.Filename] != source.SHA256 {
			return Sidecar{}, false, fmt.Errorf("%w: Codex checksum manifest does not match release.json for %s", ErrIntegrityMismatch, source.Filename)
		}
	}
	sourcePaths := make(map[string]string, len(plan.Sources))
	for _, source := range plan.Sources {
		temp, err := f.downloadExact(downloadCtx, f.releaseURL(plan.Version, source.Filename), source.SHA256, f.sourceMax)
		if err != nil {
			return Sidecar{}, false, err
		}
		defer os.Remove(temp)
		sourcePaths[source.Filename] = temp
	}
	tempPath, sha256Hex, size, err := f.buildBundle(downloadCtx, plan, checksumPath, sourcePaths)
	if err != nil {
		return Sidecar{}, false, err
	}
	defer os.Remove(tempPath)
	if err := reportFetchProgress(progress, FetchPhaseVerifying, size, &size); err != nil {
		return Sidecar{}, false, err
	}
	record := Sidecar{
		Name: "codex", Version: plan.Version, TarballURL: plan.ChecksumURL,
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

func (f *CodexFetcher) findCachedExact(ctx context.Context, plan CodexFetchPlan) (Sidecar, bool, error) {
	entries, err := ScanCatalog(f.artifactsDir)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: read artifact catalog: %v", ErrArtifactStorage, err)
	}
	for _, entry := range entries {
		if entry.Record == nil || (entry.Status != CatalogAvailableUnverified && entry.Status != CatalogReady) {
			continue
		}
		record := *entry.Record
		if record.Name != "codex" || record.Version != plan.Version || record.TarballURL != plan.ChecksumURL ||
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

func (f *CodexFetcher) downloadExact(ctx context.Context, rawURL, wantSHA string, maximum int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build Codex archive request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("artifact: download Codex archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("artifact: Codex archive returned HTTP %d", resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "", fmt.Errorf("%w: Codex archive Content-Encoding is not identity", ErrMetadataInvalid)
	}
	if resp.ContentLength <= 0 {
		return "", fmt.Errorf("%w: Codex archive did not declare a size", ErrMetadataInvalid)
	}
	if resp.ContentLength > maximum {
		return "", fmt.Errorf("%w: Codex archive declared %d bytes, limit %d", ErrArtifactTooLarge, resp.ContentLength, maximum)
	}
	temp, err := os.CreateTemp(f.artifactsDir, codexSourceTempPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("%w: create Codex archive temp: %v", ErrArtifactStorage, err)
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
		return "", fmt.Errorf("%w: chmod Codex archive temp: %v", ErrArtifactStorage, err)
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(resp.Body, resp.ContentLength+1))
	if err != nil {
		return "", fmt.Errorf("%w: copy Codex archive: %v", ErrMetadataInvalid, err)
	}
	if written != resp.ContentLength {
		return "", fmt.Errorf("%w: Codex archive size mismatch", ErrIntegrityMismatch)
	}
	if hex.EncodeToString(digest.Sum(nil)) != wantSHA {
		return "", fmt.Errorf("%w: Codex archive SHA-256 mismatch", ErrIntegrityMismatch)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("%w: fsync Codex archive: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, nil
}

func (f *CodexFetcher) buildBundle(ctx context.Context, plan CodexFetchPlan, checksumPath string, sourcePaths map[string]string) (string, string, int64, error) {
	temp, err := os.CreateTemp(f.artifactsDir, artifactFetchTempPrefix+"*.tmp")
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Codex bundle temp: %v", ErrArtifactStorage, err)
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
		return "", "", 0, fmt.Errorf("%w: chmod Codex bundle temp: %v", ErrArtifactStorage, err)
	}
	digest := sha256.New()
	output := &boundedHashWriter{w: temp, hash: digest, maximum: plan.BundleMaxBytes}
	gz, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Codex bundle gzip: %v", ErrArtifactStorage, err)
	}
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	dirs := []string{"codex/"}
	for _, source := range plan.Sources {
		dirs = append(dirs, "codex/"+source.TargetOS+"-"+source.TargetArch+"/")
	}
	sort.Strings(dirs)
	seenDir := map[string]struct{}{}
	for _, name := range dirs {
		if _, ok := seenDir[name]; ok {
			continue
		}
		seenDir[name] = struct{}{}
		if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeDir, 0, "", false)); err != nil {
			return "", "", 0, fmt.Errorf("%w: write Codex bundle directory: %v", ErrArtifactStorage, err)
		}
	}
	type bundleFile struct {
		name string
		path string
	}
	files := []bundleFile{{name: "codex/" + codexChecksumAsset, path: checksumPath}}
	for _, source := range plan.Sources {
		files = append(files, bundleFile{
			name: "codex/" + source.TargetOS + "-" + source.TargetArch + "/" + source.Filename,
			path: sourcePaths[source.Filename],
		})
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return "", "", 0, err
		}
		if err := writeCodexBundleFile(tw, file.name, file.path); err != nil {
			return "", "", 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Codex bundle tar: %v", ErrArtifactStorage, err)
	}
	if err := gz.Close(); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return "", "", 0, ErrArtifactTooLarge
		}
		return "", "", 0, fmt.Errorf("%w: close Codex bundle gzip: %v", ErrArtifactStorage, err)
	}
	if output.written == 0 {
		return "", "", 0, fmt.Errorf("%w: Codex bundle is empty", ErrMetadataInvalid)
	}
	if err := temp.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("%w: fsync Codex bundle: %v", ErrArtifactStorage, err)
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Codex bundle: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, hex.EncodeToString(digest.Sum(nil)), output.written, nil
}

func writeCodexBundleFile(tw *tar.Writer, name, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open Codex staged archive: %v", ErrArtifactStorage, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat Codex staged archive: %v", ErrArtifactStorage, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return fmt.Errorf("%w: Codex staged archive is not a regular file", ErrIntegrityMismatch)
	}
	if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeReg, info.Size(), "", false)); err != nil {
		return fmt.Errorf("%w: write Codex bundle header: %v", ErrArtifactStorage, err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("%w: write Codex bundle file: %v", ErrArtifactStorage, err)
	}
	return nil
}
