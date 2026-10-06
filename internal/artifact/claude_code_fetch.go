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
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ProductionClaudeCodeOrigin      = "https://downloads.claude.ai"
	ClaudeCodeFetchPolicyVersion    = "claude-code-official-bundle:v1"
	ArtifactSourceClaudeCode        = "claude-code-bundle:v1"
	DefaultClaudeCodeSourceMaxBytes = int64(512 << 20)
	DefaultClaudeCodeBundleMaxBytes = int64(1 << 30)
	claudeCodeManifestMaxBytes      = int64(1 << 20)
	claudeCodeSourceTempPrefix      = ".claude-code-source-"
)

type ClaudeCodeSource struct {
	TargetOS   string `json:"target_os"`
	TargetArch string `json:"target_arch"`
	Platform   string `json:"platform"`
	Filename   string `json:"filename"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
}

type ClaudeCodeFetchPlan struct {
	PolicyVersion  string             `json:"policy_version"`
	Name           string             `json:"name"`
	Version        string             `json:"version"`
	SourceOrigin   string             `json:"source_origin"`
	ChecksumURL    string             `json:"checksum_url"`
	Sources        []ClaudeCodeSource `json:"sources"`
	SourceIdentity string             `json:"source_identity"`
	SourceMaxBytes int64              `json:"source_max_bytes"`
	BundleMaxBytes int64              `json:"bundle_max_bytes"`
	PreviewedAt    time.Time          `json:"previewed_at"`
	PreviewDigest  string             `json:"preview_digest"`
}

type ClaudeCodeFetcher struct {
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

type claudeCodeFetcherConfig struct {
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

type claudeCodeOfficialManifest struct {
	Version   string `json:"version"`
	Platforms map[string]struct {
		Binary   string `json:"binary"`
		Checksum string `json:"checksum"`
		Size     int64  `json:"size"`
	} `json:"platforms"`
}

func ValidClaudeCodeVersion(version string) bool { return validNodeRuntimeVersion(version) }

func newClaudeCodeFetcher(config claudeCodeFetcherConfig) (*ClaudeCodeFetcher, error) {
	dir := filepath.Clean(config.artifactsDir)
	if config.artifactsDir == "" || !filepath.IsAbs(config.artifactsDir) || dir != config.artifactsDir ||
		filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("%w: artifacts directory must be a canonical absolute non-root path", ErrInvalidFetchRequest)
	}
	origin, originString, err := parseRegistryURL(config.originURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	if !config.allowHTTP && originString != ProductionClaudeCodeOrigin {
		return nil, fmt.Errorf("%w: Claude Code origin must be %s", ErrRegistryPolicy, ProductionClaudeCodeOrigin)
	}
	if config.metadataMax <= 0 {
		config.metadataMax = claudeCodeManifestMaxBytes
	}
	if config.sourceMax <= 0 {
		config.sourceMax = DefaultClaudeCodeSourceMaxBytes
	}
	if config.bundleMax <= 0 {
		config.bundleMax = DefaultClaudeCodeBundleMaxBytes
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
	return &ClaudeCodeFetcher{
		artifactsDir: dir, origin: origin, originString: originString, client: client,
		metadataMax: config.metadataMax, sourceMax: config.sourceMax, bundleMax: config.bundleMax,
		metadataTimeout: config.metadataTimeout, downloadTimeout: config.downloadTimeout,
		allowHTTP: config.allowHTTP, now: config.now, serial: lock.(*sync.Mutex),
	}, nil
}

func claudeCodeWantedPlatforms() []ClaudeCodeSource {
	return []ClaudeCodeSource{
		{TargetOS: "linux", TargetArch: "amd64", Platform: "linux-x64", Filename: "claude"},
		{TargetOS: "linux", TargetArch: "arm64", Platform: "linux-arm64", Filename: "claude"},
		{TargetOS: "darwin", TargetArch: "amd64", Platform: "darwin-x64", Filename: "claude"},
		{TargetOS: "darwin", TargetArch: "arm64", Platform: "darwin-arm64", Filename: "claude"},
		{TargetOS: "windows", TargetArch: "amd64", Platform: "win32-x64", Filename: "claude.exe"},
		{TargetOS: "windows", TargetArch: "arm64", Platform: "win32-arm64", Filename: "claude.exe"},
	}
}

func (f *ClaudeCodeFetcher) releaseURL(version, name string) string {
	return strings.TrimSuffix(f.originString, "/") + "/claude-code-releases/" + url.PathEscape(version) + "/" + name
}

func (f *ClaudeCodeFetcher) PreviewPlan(ctx context.Context, version string) (ClaudeCodeFetchPlan, error) {
	if f == nil || f.client == nil || f.origin == nil || ctx == nil || !ValidClaudeCodeVersion(version) {
		return ClaudeCodeFetchPlan{}, fmt.Errorf("%w: exact claude-code version and fetcher are required", ErrInvalidFetchRequest)
	}
	checksumURL := f.releaseURL(version, "manifest.json")
	requestCtx, cancel := context.WithTimeout(ctx, f.metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return ClaudeCodeFetchPlan{}, fmt.Errorf("%w: build Claude Code manifest request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return ClaudeCodeFetchPlan{}, fmt.Errorf("%w: read Claude Code manifest: %v", ErrMetadataInvalid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return ClaudeCodeFetchPlan{}, fmt.Errorf("%w: Claude Code manifest returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return ClaudeCodeFetchPlan{}, fmt.Errorf("%w: Claude Code manifest Content-Encoding is not identity", ErrMetadataInvalid)
	}
	body, err := readBoundedBody(resp.Body, resp.ContentLength, f.metadataMax, ErrMetadataTooLarge)
	if err != nil {
		return ClaudeCodeFetchPlan{}, fmt.Errorf("%w: Claude Code manifest: %v", ErrMetadataInvalid, err)
	}
	sources, err := parseClaudeCodeManifest(version, body)
	if err != nil {
		return ClaudeCodeFetchPlan{}, err
	}
	now := f.now().UTC()
	if now.IsZero() {
		return ClaudeCodeFetchPlan{}, fmt.Errorf("%w: clock returned zero preview time", ErrInvalidFetchRequest)
	}
	plan := ClaudeCodeFetchPlan{
		PolicyVersion: ClaudeCodeFetchPolicyVersion, Name: "claude-code", Version: version,
		SourceOrigin: f.originString, ChecksumURL: checksumURL, Sources: sources,
		SourceMaxBytes: f.sourceMax, BundleMaxBytes: f.bundleMax, PreviewedAt: now,
	}
	plan.SourceIdentity = claudeCodeSourceIdentity(plan)
	plan.PreviewDigest = claudeCodePreviewDigest(plan)
	return plan, nil
}

func parseClaudeCodeManifest(version string, body []byte) ([]ClaudeCodeSource, error) {
	var manifest claudeCodeOfficialManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("%w: Claude Code manifest is not JSON", ErrMetadataInvalid)
	}
	if manifest.Version != version {
		return nil, fmt.Errorf("%w: Claude Code manifest version does not match the request", ErrMetadataInvalid)
	}
	wanted := claudeCodeWantedPlatforms()
	result := make([]ClaudeCodeSource, 0, len(wanted))
	for _, source := range wanted {
		entry, ok := manifest.Platforms[source.Platform]
		if !ok || entry.Binary != source.Filename || !ValidSHA256Hex(entry.Checksum) || entry.Size <= 0 {
			return nil, fmt.Errorf("%w: Claude Code manifest lacks target %s", ErrMetadataInvalid, source.Platform)
		}
		source.SHA256 = entry.Checksum
		source.Size = entry.Size
		result = append(result, source)
	}
	return result, nil
}

func claudeCodeSourceIdentity(plan ClaudeCodeFetchPlan) string {
	body := struct {
		Policy  string             `json:"policy_version"`
		Version string             `json:"version"`
		Origin  string             `json:"source_origin"`
		Sources []ClaudeCodeSource `json:"sources"`
	}{plan.PolicyVersion, plan.Version, plan.SourceOrigin, plan.Sources}
	raw, _ := marshalCompactNoEscape(body)
	sum := sha512.Sum512(raw)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func claudeCodePreviewDigest(plan ClaudeCodeFetchPlan) string {
	body := struct {
		PolicyVersion  string             `json:"policy_version"`
		Name           string             `json:"name"`
		Version        string             `json:"version"`
		SourceOrigin   string             `json:"source_origin"`
		ChecksumURL    string             `json:"checksum_url"`
		Sources        []ClaudeCodeSource `json:"sources"`
		SourceIdentity string             `json:"source_identity"`
		SourceMaxBytes int64              `json:"source_max_bytes"`
		BundleMaxBytes int64              `json:"bundle_max_bytes"`
	}{plan.PolicyVersion, plan.Name, plan.Version, plan.SourceOrigin, plan.ChecksumURL, plan.Sources,
		plan.SourceIdentity, plan.SourceMaxBytes, plan.BundleMaxBytes}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeClaudeCodeSourcePlan(raw string) (ClaudeCodeFetchPlan, error) {
	var plan ClaudeCodeFetchPlan
	if raw == "" || len(raw) > MaxArtifactSourcePlanBytes {
		return plan, fmt.Errorf("%w: Claude Code source plan is missing or oversized", ErrInvalidFetchRequest)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("%w: decode Claude Code source plan", ErrInvalidFetchRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return plan, fmt.Errorf("%w: Claude Code source plan has trailing data", ErrInvalidFetchRequest)
	}
	canonical, err := marshalCompactNoEscape(plan)
	if err != nil || string(canonical) != raw {
		return plan, fmt.Errorf("%w: Claude Code source plan is not canonical", ErrInvalidFetchRequest)
	}
	return plan, nil
}

func ValidClaudeCodeSourcePlan(raw string) bool {
	_, err := decodeClaudeCodeSourcePlan(raw)
	return err == nil
}

func (f *ClaudeCodeFetcher) validatePlan(plan ClaudeCodeFetchPlan) error {
	if plan.PolicyVersion != ClaudeCodeFetchPolicyVersion || plan.Name != "claude-code" ||
		!ValidClaudeCodeVersion(plan.Version) || plan.SourceOrigin != f.originString ||
		plan.ChecksumURL != f.releaseURL(plan.Version, "manifest.json") ||
		plan.SourceMaxBytes != f.sourceMax || plan.BundleMaxBytes != f.bundleMax ||
		plan.PreviewedAt.IsZero() || plan.PreviewedAt.Location() != time.UTC || len(plan.Sources) != 6 {
		return fmt.Errorf("%w: Claude Code plan does not match active policy", ErrInvalidFetchRequest)
	}
	want := claudeCodeWantedPlatforms()
	for i := range want {
		got := plan.Sources[i]
		if got.TargetOS != want[i].TargetOS || got.TargetArch != want[i].TargetArch ||
			got.Platform != want[i].Platform || got.Filename != want[i].Filename ||
			!ValidSHA256Hex(got.SHA256) || got.Size <= 0 || got.Size > f.sourceMax {
			return fmt.Errorf("%w: Claude Code source identity is invalid", ErrInvalidFetchRequest)
		}
	}
	if plan.SourceIdentity != claudeCodeSourceIdentity(plan) || plan.PreviewDigest != claudeCodePreviewDigest(plan) {
		return fmt.Errorf("%w: Claude Code plan digest does not match", ErrInvalidFetchRequest)
	}
	return nil
}

func (f *ClaudeCodeFetcher) FetchExact(ctx context.Context, plan ClaudeCodeFetchPlan, fetchedBy string,
	progress ProgressFunc,
) (Sidecar, bool, error) {
	if f == nil || f.client == nil || f.serial == nil || ctx == nil {
		return Sidecar{}, false, fmt.Errorf("%w: Claude Code fetcher or context is incomplete", ErrInvalidFetchRequest)
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
		temp, err := f.downloadSource(downloadCtx, plan.Version, source)
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
		Name: "claude-code", Version: plan.Version, TarballURL: plan.ChecksumURL,
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

func (f *ClaudeCodeFetcher) findCachedExact(ctx context.Context, plan ClaudeCodeFetchPlan) (Sidecar, bool, error) {
	entries, err := ScanCatalog(f.artifactsDir)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: read artifact catalog: %v", ErrArtifactStorage, err)
	}
	for _, entry := range entries {
		if entry.Record == nil || (entry.Status != CatalogAvailableUnverified && entry.Status != CatalogReady) {
			continue
		}
		record := *entry.Record
		if record.Name != "claude-code" || record.Version != plan.Version || record.TarballURL != plan.ChecksumURL ||
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

func (f *ClaudeCodeFetcher) downloadSource(ctx context.Context, version string, source ClaudeCodeSource) (string, error) {
	rawURL := f.releaseURL(version, path.Join(source.Platform, source.Filename))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build Claude Code binary request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("artifact: download Claude Code binary: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("artifact: Claude Code binary returned HTTP %d", resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "", fmt.Errorf("%w: Claude Code binary Content-Encoding is not identity", ErrMetadataInvalid)
	}
	if resp.ContentLength > 0 && resp.ContentLength != source.Size {
		return "", fmt.Errorf("%w: Claude Code binary declared %d bytes, manifest %d", ErrIntegrityMismatch, resp.ContentLength, source.Size)
	}
	if source.Size > f.sourceMax {
		return "", fmt.Errorf("%w: Claude Code binary declared %d bytes, limit %d", ErrArtifactTooLarge, source.Size, f.sourceMax)
	}
	temp, err := os.CreateTemp(f.artifactsDir, claudeCodeSourceTempPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("%w: create Claude Code binary temp: %v", ErrArtifactStorage, err)
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
		return "", fmt.Errorf("%w: chmod Claude Code binary temp: %v", ErrArtifactStorage, err)
	}
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(resp.Body, source.Size+1))
	if err != nil {
		return "", fmt.Errorf("%w: copy Claude Code binary: %v", ErrMetadataInvalid, err)
	}
	if written != source.Size {
		return "", fmt.Errorf("%w: Claude Code binary size mismatch", ErrIntegrityMismatch)
	}
	if hex.EncodeToString(digest.Sum(nil)) != source.SHA256 {
		return "", fmt.Errorf("%w: Claude Code binary SHA-256 mismatch", ErrIntegrityMismatch)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("%w: fsync Claude Code binary: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, nil
}

func (f *ClaudeCodeFetcher) buildBundle(ctx context.Context, plan ClaudeCodeFetchPlan, sourcePaths map[string]string) (string, string, int64, error) {
	temp, err := os.CreateTemp(f.artifactsDir, artifactFetchTempPrefix+"*.tmp")
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Claude Code bundle temp: %v", ErrArtifactStorage, err)
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
		return "", "", 0, fmt.Errorf("%w: chmod Claude Code bundle temp: %v", ErrArtifactStorage, err)
	}
	digest := sha256.New()
	output := &boundedHashWriter{w: temp, hash: digest, maximum: plan.BundleMaxBytes}
	gz, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Claude Code bundle gzip: %v", ErrArtifactStorage, err)
	}
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	dirs := []string{"claude-code/"}
	for _, source := range plan.Sources {
		dirs = append(dirs, "claude-code/"+source.TargetOS+"-"+source.TargetArch+"/",
			"claude-code/"+source.TargetOS+"-"+source.TargetArch+"/bin/")
	}
	sort.Strings(dirs)
	seenDir := map[string]struct{}{}
	for _, name := range dirs {
		if _, ok := seenDir[name]; ok {
			continue
		}
		seenDir[name] = struct{}{}
		if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeDir, 0, "", false)); err != nil {
			return "", "", 0, fmt.Errorf("%w: write Claude Code bundle root: %v", ErrArtifactStorage, err)
		}
	}
	for _, source := range plan.Sources {
		if err := ctx.Err(); err != nil {
			return "", "", 0, err
		}
		body, err := os.ReadFile(sourcePaths[source.TargetOS+"-"+source.TargetArch])
		if err != nil {
			return "", "", 0, fmt.Errorf("%w: read Claude Code binary: %v", ErrArtifactStorage, err)
		}
		if int64(len(body)) != source.Size {
			return "", "", 0, fmt.Errorf("%w: Claude Code staged binary size mismatch", ErrIntegrityMismatch)
		}
		name := "claude-code/" + source.TargetOS + "-" + source.TargetArch + "/bin/" + source.Filename
		if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeReg, int64(len(body)), "", true)); err != nil {
			return "", "", 0, fmt.Errorf("%w: write Claude Code bundle header: %v", ErrArtifactStorage, err)
		}
		if _, err := tw.Write(body); err != nil {
			return "", "", 0, fmt.Errorf("%w: write Claude Code bundle file: %v", ErrArtifactStorage, err)
		}
	}
	if err := tw.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Claude Code bundle tar: %v", ErrArtifactStorage, err)
	}
	if err := gz.Close(); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return "", "", 0, ErrArtifactTooLarge
		}
		return "", "", 0, fmt.Errorf("%w: close Claude Code bundle gzip: %v", ErrArtifactStorage, err)
	}
	if output.written == 0 {
		return "", "", 0, fmt.Errorf("%w: Claude Code bundle is empty", ErrMetadataInvalid)
	}
	if err := temp.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("%w: fsync Claude Code bundle: %v", ErrArtifactStorage, err)
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Claude Code bundle: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, hex.EncodeToString(digest.Sum(nil)), output.written, nil
}
