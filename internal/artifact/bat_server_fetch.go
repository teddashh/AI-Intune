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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ArtifactSourceBATServer        = "bat-server-bundle:v1"
	BATServerFetchPolicyVersion    = "bat-server-github-release:v1"
	ProductionBATServerOrigin      = "https://api.github.com"
	DefaultBATServerSourceMaxBytes = int64(512 << 20) // per asset; largest real asset is 269 MiB
	DefaultBATServerBundleMaxBytes = int64(1 << 30)   // both assets together are ~527 MiB
	batServerRepository            = "tony1223/better-agent-terminal"
	batServerArtifactName          = "bat-server"
	batServerSourceTempPrefix      = ".bat-server-source-"
	batServerUserAgent             = "ai-intune"
	batServerRedirectLimit         = 5
	batServerAssetURLMaxBytes      = 16 << 10
)

type BATServerSource struct {
	TargetOS   string `json:"target_os"`
	TargetArch string `json:"target_arch"`
	Asset      string `json:"asset"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
}

type BATServerFetchPlan struct {
	PolicyVersion      string            `json:"policy_version"`
	Name               string            `json:"name"`
	Version            string            `json:"version"`
	SourceOrigin       string            `json:"source_origin"`
	VersionDocumentURL string            `json:"version_document_url"`
	Sources            []BATServerSource `json:"sources"`
	SourceIdentity     string            `json:"source_identity"`
	SourceMaxBytes     int64             `json:"source_max_bytes"`
	BundleMaxBytes     int64             `json:"bundle_max_bytes"`
	PreviewedAt        time.Time         `json:"previewed_at"`
	PreviewDigest      string            `json:"preview_digest"`
}

type BATServerFetcher struct {
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

type batServerFetcherConfig struct {
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

type batServerPlatform struct {
	TargetOS   string
	TargetArch string
	Asset      string
}

type batServerReleaseDocument struct {
	TagName    string                  `json:"tag_name"`
	Draft      bool                    `json:"draft"`
	Prerelease bool                    `json:"prerelease"`
	Assets     []batServerReleaseAsset `json:"assets"`
}

type batServerReleaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type batServerFetchedAsset struct {
	source      BATServerSource
	downloadURL string
}

func ValidBATServerVersion(version string) bool { return validNodeRuntimeVersion(version) }

func newBATServerFetcher(config batServerFetcherConfig) (*BATServerFetcher, error) {
	dir := filepath.Clean(config.artifactsDir)
	if config.artifactsDir == "" || !filepath.IsAbs(config.artifactsDir) || dir != config.artifactsDir ||
		filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("%w: artifacts directory must be a canonical absolute non-root path", ErrInvalidFetchRequest)
	}
	origin, originString, err := parseRegistryURL(config.originURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	if !config.allowHTTP && originString != ProductionBATServerOrigin {
		return nil, fmt.Errorf("%w: bat-server origin must be %s", ErrRegistryPolicy, ProductionBATServerOrigin)
	}
	if config.metadataMax <= 0 {
		config.metadataMax = DefaultMetadataMaxBytes
	}
	if config.sourceMax <= 0 {
		config.sourceMax = DefaultBATServerSourceMaxBytes
	}
	if config.bundleMax <= 0 {
		config.bundleMax = DefaultBATServerBundleMaxBytes
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
	return &BATServerFetcher{
		artifactsDir: dir, origin: origin, originString: originString, client: client,
		metadataMax: config.metadataMax, sourceMax: config.sourceMax, bundleMax: config.bundleMax,
		metadataTimeout: config.metadataTimeout, downloadTimeout: config.downloadTimeout,
		allowHTTP: config.allowHTTP, now: config.now, serial: lock.(*sync.Mutex),
	}, nil
}

func batServerPlatforms() []batServerPlatform {
	return []batServerPlatform{
		{TargetOS: "linux", TargetArch: "amd64", Asset: "bat-server-linux-x86_64.tar.gz"},
		{TargetOS: "linux", TargetArch: "arm64", Asset: "bat-server-linux-aarch64.tar.gz"},
	}
}

func batServerTag(version string) string { return "v" + version }

func (f *BATServerFetcher) versionDocumentURL(version string) string {
	return f.originString + "/repos/" + batServerRepository + "/releases/tags/" + url.PathEscape(batServerTag(version))
}

func (f *BATServerFetcher) PreviewPlan(ctx context.Context, version string) (BATServerFetchPlan, error) {
	if f == nil || f.client == nil || f.origin == nil || ctx == nil || !ValidBATServerVersion(version) {
		return BATServerFetchPlan{}, fmt.Errorf("%w: exact bat-server version and fetcher are required", ErrInvalidFetchRequest)
	}
	requestCtx, cancel := context.WithTimeout(ctx, f.metadataTimeout)
	defer cancel()
	fresh, err := f.loadRelease(requestCtx, version)
	if err != nil {
		return BATServerFetchPlan{}, err
	}
	now := f.now().UTC()
	if now.IsZero() {
		return BATServerFetchPlan{}, fmt.Errorf("%w: clock returned zero preview time", ErrInvalidFetchRequest)
	}
	sources := make([]BATServerSource, 0, len(fresh))
	for _, asset := range fresh {
		sources = append(sources, asset.source)
	}
	plan := BATServerFetchPlan{
		PolicyVersion: BATServerFetchPolicyVersion, Name: batServerArtifactName, Version: version,
		SourceOrigin: f.originString, VersionDocumentURL: f.versionDocumentURL(version),
		Sources: sources, SourceMaxBytes: f.sourceMax, BundleMaxBytes: f.bundleMax, PreviewedAt: now,
	}
	plan.SourceIdentity = batServerSourceIdentity(plan)
	plan.PreviewDigest = batServerPreviewDigest(plan)
	if err := f.validatePlan(plan); err != nil {
		return BATServerFetchPlan{}, err
	}
	return plan, nil
}

func (f *BATServerFetcher) getJSON(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build bat-server metadata request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", batServerUserAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: read bat-server metadata: %v", ErrMetadataInvalid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("%w: bat-server metadata returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, fmt.Errorf("%w: bat-server metadata Content-Encoding is not identity", ErrMetadataInvalid)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaErr != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return nil, fmt.Errorf("%w: bat-server metadata Content-Type is not JSON", ErrMetadataInvalid)
	}
	body, err := readBoundedBody(resp.Body, resp.ContentLength, f.metadataMax, ErrMetadataTooLarge)
	if err != nil {
		return nil, fmt.Errorf("%w: bat-server metadata: %v", ErrMetadataInvalid, err)
	}
	return body, nil
}

func decodeBATServerRelease(body []byte) (batServerReleaseDocument, error) {
	var document batServerReleaseDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("%w: bat-server release document is not JSON", ErrMetadataInvalid)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, fmt.Errorf("%w: bat-server release document contains trailing JSON", ErrMetadataInvalid)
	}
	return document, nil
}

func (f *BATServerFetcher) loadRelease(ctx context.Context, version string) ([]batServerFetchedAsset, error) {
	body, err := f.getJSON(ctx, f.versionDocumentURL(version))
	if err != nil {
		return nil, err
	}
	document, err := decodeBATServerRelease(body)
	if err != nil {
		return nil, err
	}
	if document.TagName != batServerTag(version) {
		return nil, fmt.Errorf("%w: bat-server release tag does not match the request", ErrMetadataInvalid)
	}
	if document.Draft {
		return nil, fmt.Errorf("%w: bat-server release %s is a draft", ErrMetadataInvalid, version)
	}
	if document.Prerelease {
		return nil, fmt.Errorf("%w: bat-server release %s is a prerelease", ErrMetadataInvalid, version)
	}
	seen := make(map[string]struct{}, len(document.Assets))
	indexed := make(map[string]batServerReleaseAsset, len(document.Assets))
	for _, asset := range document.Assets {
		if asset.Name == "" {
			return nil, fmt.Errorf("%w: bat-server release asset name is empty", ErrMetadataInvalid)
		}
		if _, ok := seen[asset.Name]; ok {
			return nil, fmt.Errorf("%w: bat-server release repeats asset %s", ErrMetadataInvalid, asset.Name)
		}
		seen[asset.Name] = struct{}{}
		indexed[asset.Name] = asset
	}
	fresh := make([]batServerFetchedAsset, 0, len(batServerPlatforms()))
	for _, platform := range batServerPlatforms() {
		asset, ok := indexed[platform.Asset]
		if !ok {
			return nil, fmt.Errorf("%w: bat-server release lacks %s", ErrMetadataInvalid, platform.Asset)
		}
		digest, err := batServerCanonicalDigest(asset.Digest)
		if err != nil {
			return nil, err
		}
		if asset.Size <= 0 {
			return nil, fmt.Errorf("%w: bat-server asset %s size is invalid", ErrMetadataInvalid, asset.Name)
		}
		if asset.Size > f.sourceMax {
			return nil, fmt.Errorf("%w: bat-server asset %s declared %d bytes, limit %d", ErrArtifactTooLarge, asset.Name, asset.Size, f.sourceMax)
		}
		if err := f.validateAssetURL(asset.BrowserDownloadURL); err != nil {
			return nil, err
		}
		fresh = append(fresh, batServerFetchedAsset{
			source: BATServerSource{
				TargetOS: platform.TargetOS, TargetArch: platform.TargetArch, Asset: platform.Asset,
				Size: asset.Size, Digest: digest,
			},
			downloadURL: asset.BrowserDownloadURL,
		})
	}
	return fresh, nil
}

func batServerCanonicalDigest(value string) (string, error) {
	encoded, ok := strings.CutPrefix(value, "sha256:")
	if !ok || len(encoded) != hex.EncodedLen(sha256.Size) || encoded != strings.ToLower(encoded) {
		return "", fmt.Errorf("%w: bat-server digest is not canonical sha256", ErrMetadataInvalid)
	}
	raw, err := hex.DecodeString(encoded)
	if err != nil || len(raw) != sha256.Size {
		return "", fmt.Errorf("%w: bat-server digest is not canonical sha256", ErrMetadataInvalid)
	}
	canonical := "sha256:" + hex.EncodeToString(raw)
	if subtle.ConstantTimeCompare([]byte(canonical), []byte(value)) != 1 {
		return "", fmt.Errorf("%w: bat-server digest is not canonical sha256", ErrMetadataInvalid)
	}
	return canonical, nil
}

func batServerSourceIdentity(plan BATServerFetchPlan) string {
	body := struct {
		Policy   string            `json:"policy_version"`
		Version  string            `json:"version"`
		Origin   string            `json:"source_origin"`
		Document string            `json:"version_document_url"`
		Sources  []BATServerSource `json:"sources"`
	}{plan.PolicyVersion, plan.Version, plan.SourceOrigin, plan.VersionDocumentURL, plan.Sources}
	raw, _ := marshalCompactNoEscape(body)
	sum := sha512.Sum512(raw)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func batServerPreviewDigest(plan BATServerFetchPlan) string {
	body := struct {
		PolicyVersion      string            `json:"policy_version"`
		Name               string            `json:"name"`
		Version            string            `json:"version"`
		SourceOrigin       string            `json:"source_origin"`
		VersionDocumentURL string            `json:"version_document_url"`
		Sources            []BATServerSource `json:"sources"`
		SourceIdentity     string            `json:"source_identity"`
		SourceMaxBytes     int64             `json:"source_max_bytes"`
		BundleMaxBytes     int64             `json:"bundle_max_bytes"`
	}{plan.PolicyVersion, plan.Name, plan.Version, plan.SourceOrigin, plan.VersionDocumentURL,
		plan.Sources, plan.SourceIdentity, plan.SourceMaxBytes, plan.BundleMaxBytes}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeBATServerSourcePlan(raw string) (BATServerFetchPlan, error) {
	var plan BATServerFetchPlan
	if raw == "" || len(raw) > MaxArtifactSourcePlanBytes {
		return plan, fmt.Errorf("%w: bat-server source plan is missing or oversized", ErrInvalidFetchRequest)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("%w: decode bat-server source plan", ErrInvalidFetchRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return plan, fmt.Errorf("%w: bat-server source plan has trailing data", ErrInvalidFetchRequest)
	}
	canonical, err := marshalCompactNoEscape(plan)
	if err != nil || string(canonical) != raw {
		return plan, fmt.Errorf("%w: bat-server source plan is not canonical", ErrInvalidFetchRequest)
	}
	return plan, nil
}

func ValidBATServerSourcePlan(raw string) bool {
	_, err := decodeBATServerSourcePlan(raw)
	return err == nil
}

func (f *BATServerFetcher) validatePlan(plan BATServerFetchPlan) error {
	documentURL := f.versionDocumentURL(plan.Version)
	if plan.PolicyVersion != BATServerFetchPolicyVersion || plan.Name != batServerArtifactName ||
		!ValidBATServerVersion(plan.Version) || plan.SourceOrigin != f.originString ||
		plan.VersionDocumentURL != documentURL || plan.SourceMaxBytes != f.sourceMax ||
		plan.BundleMaxBytes != f.bundleMax || plan.PreviewedAt.IsZero() ||
		plan.PreviewedAt.Location() != time.UTC || len(plan.Sources) != len(batServerPlatforms()) {
		return fmt.Errorf("%w: bat-server plan does not match active policy", ErrInvalidFetchRequest)
	}
	want := batServerPlatforms()
	for i := range want {
		got := plan.Sources[i]
		if got.TargetOS != want[i].TargetOS || got.TargetArch != want[i].TargetArch || got.Asset != want[i].Asset {
			return fmt.Errorf("%w: bat-server source identity is invalid", ErrInvalidFetchRequest)
		}
		if _, err := batServerCanonicalDigest(got.Digest); err != nil {
			return fmt.Errorf("%w: bat-server source digest is invalid", ErrInvalidFetchRequest)
		}
		if got.Size <= 0 || got.Size > f.sourceMax {
			return fmt.Errorf("%w: bat-server source size is outside policy", ErrInvalidFetchRequest)
		}
	}
	if plan.SourceIdentity != batServerSourceIdentity(plan) || plan.PreviewDigest != batServerPreviewDigest(plan) {
		return fmt.Errorf("%w: bat-server plan digest does not match", ErrInvalidFetchRequest)
	}
	return nil
}

func (f *BATServerFetcher) FetchExact(ctx context.Context, plan BATServerFetchPlan, fetchedBy string,
	progress ProgressFunc,
) (Sidecar, bool, error) {
	if f == nil || f.client == nil || f.serial == nil || ctx == nil {
		return Sidecar{}, false, fmt.Errorf("%w: bat-server fetcher or context is incomplete", ErrInvalidFetchRequest)
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
	metaCtx, cancelMeta := context.WithTimeout(ctx, f.metadataTimeout)
	defer cancelMeta()
	fresh, err := f.loadRelease(metaCtx, plan.Version)
	if err != nil {
		return Sidecar{}, false, err
	}
	if !batServerSourcesMatch(plan.Sources, fresh) {
		return Sidecar{}, false, fmt.Errorf("%w: bat-server source plan does not match preview", ErrInvalidFetchRequest)
	}
	if err := reportFetchProgress(progress, FetchPhaseDownloading, 0, nil); err != nil {
		return Sidecar{}, false, err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, f.downloadTimeout)
	defer cancel()
	memberPaths := make(map[string]string, len(fresh))
	for _, asset := range fresh {
		pathName, err := f.downloadExact(downloadCtx, asset.source.Asset, asset.downloadURL, asset.source.Digest, asset.source.Size)
		if err != nil {
			return Sidecar{}, false, err
		}
		defer os.Remove(pathName)
		memberPaths[asset.source.TargetOS+"-"+asset.source.TargetArch] = pathName
	}
	if len(memberPaths) != len(plan.Sources) {
		return Sidecar{}, false, fmt.Errorf("%w: bat-server bundle is missing a platform", ErrMetadataInvalid)
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
		Name: batServerArtifactName, Version: plan.Version, TarballURL: plan.VersionDocumentURL,
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

func batServerSourcesMatch(plan []BATServerSource, fresh []batServerFetchedAsset) bool {
	if len(plan) != len(fresh) {
		return false
	}
	for i := range plan {
		got := fresh[i].source
		if plan[i].TargetOS != got.TargetOS || plan[i].TargetArch != got.TargetArch ||
			plan[i].Asset != got.Asset || plan[i].Size != got.Size || plan[i].Digest != got.Digest {
			return false
		}
	}
	return true
}

func (f *BATServerFetcher) findCachedExact(ctx context.Context, plan BATServerFetchPlan) (Sidecar, bool, error) {
	entries, err := ScanCatalog(f.artifactsDir)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: read artifact catalog: %v", ErrArtifactStorage, err)
	}
	for _, entry := range entries {
		if entry.Record == nil || (entry.Status != CatalogAvailableUnverified && entry.Status != CatalogReady) {
			continue
		}
		record := *entry.Record
		if record.Name != batServerArtifactName || record.Version != plan.Version || record.TarballURL != plan.VersionDocumentURL ||
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

func (f *BATServerFetcher) downloadExact(ctx context.Context, asset, rawURL, digest string, size int64) (string, error) {
	canonical, err := batServerCanonicalDigest(digest)
	if err != nil || canonical != digest {
		return "", fmt.Errorf("%w: bat-server digest is not canonical", ErrInvalidFetchRequest)
	}
	if size <= 0 || size > f.sourceMax {
		return "", fmt.Errorf("%w: bat-server asset size is outside policy", ErrArtifactTooLarge)
	}
	want, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil || len(want) != sha256.Size {
		return "", fmt.Errorf("%w: bat-server digest is not canonical", ErrInvalidFetchRequest)
	}
	current := rawURL
	followed := 0
	var resp *http.Response
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := f.validateAssetURL(current); err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return "", fmt.Errorf("%w: build bat-server asset request: %v", ErrRegistryPolicy, err)
		}
		req.Header.Set("Accept", "application/octet-stream")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("User-Agent", batServerUserAgent)
		resp, err = f.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("artifact: download bat-server asset: %w", err)
		}
		if batServerRedirectStatus(resp.StatusCode) {
			next, locErr := batServerRedirectTarget(current, resp)
			discardBATServerResponse(resp)
			if locErr != nil {
				return "", locErr
			}
			if followed >= batServerRedirectLimit {
				return "", fmt.Errorf("%w: bat-server asset exceeded %d redirects", ErrRegistryPolicy, batServerRedirectLimit)
			}
			if err := f.validateAssetURL(next); err != nil {
				return "", err
			}
			followed++
			current = next
			continue
		}
		break
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("artifact: bat-server asset %s returned HTTP %d", asset, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "", fmt.Errorf("%w: bat-server asset Content-Encoding is not identity", ErrMetadataInvalid)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/octet-stream" {
		return "", fmt.Errorf("%w: bat-server asset Content-Type is not octet-stream", ErrMetadataInvalid)
	}
	if resp.ContentLength > f.sourceMax {
		return "", fmt.Errorf("%w: bat-server asset declared %d bytes, limit %d", ErrArtifactTooLarge, resp.ContentLength, f.sourceMax)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != size {
		return "", fmt.Errorf("%w: bat-server asset size mismatch", ErrIntegrityMismatch)
	}
	temp, err := os.CreateTemp(f.artifactsDir, batServerSourceTempPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("%w: create bat-server asset temp: %v", ErrArtifactStorage, err)
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
		return "", fmt.Errorf("%w: chmod bat-server asset temp: %v", ErrArtifactStorage, err)
	}
	hash := sha256.New()
	output := &boundedHashWriter{w: temp, hash: hash, maximum: f.sourceMax}
	written, err := io.CopyBuffer(output, resp.Body, make([]byte, 128<<10))
	if err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return "", err
		}
		return "", fmt.Errorf("%w: copy bat-server asset: %v", ErrMetadataInvalid, err)
	}
	if written != size {
		return "", fmt.Errorf("%w: bat-server asset size mismatch", ErrIntegrityMismatch)
	}
	if subtle.ConstantTimeCompare(hash.Sum(nil), want) != 1 {
		return "", fmt.Errorf("%w: bat-server asset SHA-256 mismatch", ErrIntegrityMismatch)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("%w: fsync bat-server asset: %v", ErrArtifactStorage, err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("%w: close bat-server asset: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, nil
}

func batServerRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func discardBATServerResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}

// batServerRedirectTarget keeps the raw Location text. Reprinting a parsed URL
// can change query escaping and invalidate the signed download URL.
func batServerRedirectTarget(current string, resp *http.Response) (string, error) {
	if resp == nil {
		return "", fmt.Errorf("%w: bat-server asset redirect is missing a location", ErrRegistryPolicy)
	}
	location := resp.Header.Get("Location")
	if location == "" || strings.ContainsAny(location, "\r\n") {
		return "", fmt.Errorf("%w: bat-server asset redirect is missing a location", ErrRegistryPolicy)
	}
	loc, err := url.Parse(location)
	if err != nil || loc == nil {
		return "", fmt.Errorf("%w: bat-server asset redirect URL is invalid", ErrRegistryPolicy)
	}
	if loc.IsAbs() {
		return location, nil
	}
	base, err := url.Parse(current)
	if err != nil || base == nil {
		return "", fmt.Errorf("%w: bat-server asset URL is invalid", ErrRegistryPolicy)
	}
	return base.ResolveReference(loc).String(), nil
}

func (f *BATServerFetcher) validateAssetURL(raw string) error {
	if raw == "" || len(raw) > batServerAssetURLMaxBytes || raw != strings.TrimSpace(raw) ||
		strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("%w: bat-server asset URL is invalid", ErrRegistryPolicy)
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.Hostname() == "" || u.Path == "" {
		return fmt.Errorf("%w: bat-server asset URL is invalid", ErrRegistryPolicy)
	}
	if u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return fmt.Errorf("%w: bat-server asset URL is invalid", ErrRegistryPolicy)
	}
	if u.Scheme != "https" && !(f.allowHTTP && u.Scheme == "http") {
		return fmt.Errorf("%w: bat-server asset URL is invalid", ErrRegistryPolicy)
	}
	if f.allowHTTP {
		return nil
	}
	if !batServerProductionAssetURL(u) {
		return fmt.Errorf("%w: bat-server asset URL is outside GitHub release hosting", ErrRegistryPolicy)
	}
	return nil
}

// batServerProductionAssetHosts are the only hosts a production fetch may
// contact for release bytes: the release download URL on github.com and the
// GitHub release-asset storage it redirects to. The release digest is the
// integrity check; this pin keeps whoever publishes a release from pointing
// the Hub at an arbitrary (including private or link-local) host.
var batServerProductionAssetHosts = map[string]bool{
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

func batServerProductionAssetURL(u *url.URL) bool {
	if u.Scheme != "https" || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "github.com" {
		return strings.HasPrefix(u.Path, "/"+batServerRepository+"/releases/download/")
	}
	return batServerProductionAssetHosts[host]
}

func batServerBundleMember(targetOS, targetArch string) string {
	return "bat-server/" + targetOS + "-" + targetArch + "/bat-server.tar.gz"
}

func BATServerBundleMember(targetOS, targetArch string) (string, bool) {
	if !batServerTargetSupported(targetOS, targetArch) {
		return "", false
	}
	return batServerBundleMember(targetOS, targetArch), true
}

func batServerTargetSupported(targetOS, targetArch string) bool {
	switch targetOS + "/" + targetArch {
	case "linux/amd64", "linux/arm64":
		return true
	default:
		return false
	}
}

func (f *BATServerFetcher) buildBundle(ctx context.Context, plan BATServerFetchPlan, memberPaths map[string]string) (string, string, int64, error) {
	temp, err := os.CreateTemp(f.artifactsDir, artifactFetchTempPrefix+"*.tmp")
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create bat-server bundle temp: %v", ErrArtifactStorage, err)
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
		return "", "", 0, fmt.Errorf("%w: chmod bat-server bundle temp: %v", ErrArtifactStorage, err)
	}
	digest := sha256.New()
	output := &boundedHashWriter{w: temp, hash: digest, maximum: plan.BundleMaxBytes}
	gz, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create bat-server bundle gzip: %v", ErrArtifactStorage, err)
	}
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	dirs := []string{"bat-server/"}
	for _, source := range plan.Sources {
		dirs = append(dirs, "bat-server/"+source.TargetOS+"-"+source.TargetArch+"/")
	}
	sort.Strings(dirs)
	seenDir := map[string]struct{}{}
	for _, name := range dirs {
		if _, ok := seenDir[name]; ok {
			continue
		}
		seenDir[name] = struct{}{}
		if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeDir, 0, "", false)); err != nil {
			if errors.Is(err, ErrArtifactTooLarge) {
				return "", "", 0, ErrArtifactTooLarge
			}
			return "", "", 0, fmt.Errorf("%w: write bat-server bundle directory: %v", ErrArtifactStorage, err)
		}
	}
	for _, source := range plan.Sources {
		if err := ctx.Err(); err != nil {
			return "", "", 0, err
		}
		memberPath := memberPaths[source.TargetOS+"-"+source.TargetArch]
		if memberPath == "" {
			return "", "", 0, fmt.Errorf("%w: bat-server bundle is missing a platform", ErrMetadataInvalid)
		}
		if err := writeBATServerBundleFile(tw, batServerBundleMember(source.TargetOS, source.TargetArch), memberPath); err != nil {
			if errors.Is(err, ErrArtifactTooLarge) {
				return "", "", 0, ErrArtifactTooLarge
			}
			return "", "", 0, err
		}
	}
	if err := tw.Close(); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return "", "", 0, ErrArtifactTooLarge
		}
		return "", "", 0, fmt.Errorf("%w: close bat-server bundle tar: %v", ErrArtifactStorage, err)
	}
	if err := gz.Close(); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return "", "", 0, ErrArtifactTooLarge
		}
		return "", "", 0, fmt.Errorf("%w: close bat-server bundle gzip: %v", ErrArtifactStorage, err)
	}
	if output.written == 0 {
		return "", "", 0, fmt.Errorf("%w: bat-server bundle is empty", ErrMetadataInvalid)
	}
	if err := temp.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("%w: fsync bat-server bundle: %v", ErrArtifactStorage, err)
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close bat-server bundle: %v", ErrArtifactStorage, err)
	}
	keep = true
	return pathName, hex.EncodeToString(digest.Sum(nil)), output.written, nil
}

func writeBATServerBundleFile(tw *tar.Writer, name, pathName string) error {
	file, err := os.Open(pathName)
	if err != nil {
		return fmt.Errorf("%w: open bat-server staged asset: %v", ErrArtifactStorage, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat bat-server staged asset: %v", ErrArtifactStorage, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return fmt.Errorf("%w: bat-server staged asset is not a regular file", ErrIntegrityMismatch)
	}
	if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeReg, info.Size(), "", false)); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return ErrArtifactTooLarge
		}
		return fmt.Errorf("%w: write bat-server bundle header: %v", ErrArtifactStorage, err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return ErrArtifactTooLarge
		}
		return fmt.Errorf("%w: write bat-server bundle file: %v", ErrArtifactStorage, err)
	}
	return nil
}
