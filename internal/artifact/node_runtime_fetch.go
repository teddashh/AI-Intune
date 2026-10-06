package artifact

import (
	"archive/tar"
	"archive/zip"
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
	"hash"
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
	ProductionNodeDistributionOrigin = "https://nodejs.org"
	NodeRuntimeFetchPolicyVersion    = "node-runtime-official-bundle:v3"
	DefaultNodeRuntimeSourceMaxBytes = int64(512 << 20)
	DefaultNodeRuntimeBundleMaxBytes = int64(1 << 30)

	nodeRuntimeChecksumMaxBytes     = int64(4 << 20)
	nodeRuntimeMaxEntries           = 250_000
	nodeRuntimeMaxUncompressedBytes = int64(2 << 30)
	nodeRuntimeMaxFileBytes         = int64(512 << 20)
	nodeRuntimeSourceTempPrefix     = ".node-runtime-source-"
)

type NodeRuntimeSource struct {
	TargetOS   string `json:"target_os"`
	TargetArch string `json:"target_arch"`
	Filename   string `json:"filename"`
	SHA256     string `json:"sha256"`
}

// NodeRuntimeFetchPlan pins all supported official platform archives before any bytes
// are built into the single content-addressed artifact consumed by agents.
type NodeRuntimeFetchPlan struct {
	PolicyVersion  string              `json:"policy_version"`
	Name           string              `json:"name"`
	Version        string              `json:"version"`
	SourceOrigin   string              `json:"source_origin"`
	ChecksumURL    string              `json:"checksum_url"`
	Sources        []NodeRuntimeSource `json:"sources"`
	SourceIdentity string              `json:"source_identity"`
	SourceMaxBytes int64               `json:"source_max_bytes"`
	BundleMaxBytes int64               `json:"bundle_max_bytes"`
	PreviewedAt    time.Time           `json:"previewed_at"`
	PreviewDigest  string              `json:"preview_digest"`
}

type NodeRuntimeFetcher struct {
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

type nodeRuntimeFetcherConfig struct {
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

func NewNodeRuntimeFetcher(artifactsDir string) (*NodeRuntimeFetcher, error) {
	return newNodeRuntimeFetcher(nodeRuntimeFetcherConfig{
		artifactsDir: artifactsDir, originURL: ProductionNodeDistributionOrigin,
		metadataMax: nodeRuntimeChecksumMaxBytes, sourceMax: DefaultNodeRuntimeSourceMaxBytes,
		bundleMax: DefaultNodeRuntimeBundleMaxBytes, metadataTimeout: productionMetadataTimeout,
		downloadTimeout: productionDownloadTimeout, now: time.Now,
	})
}

func newNodeRuntimeFetcher(config nodeRuntimeFetcherConfig) (*NodeRuntimeFetcher, error) {
	dir := filepath.Clean(config.artifactsDir)
	if config.artifactsDir == "" || !filepath.IsAbs(config.artifactsDir) || dir != config.artifactsDir ||
		filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("%w: artifacts directory must be a canonical absolute non-root path", ErrInvalidFetchRequest)
	}
	origin, originString, err := parseRegistryURL(config.originURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	if !config.allowHTTP && originString != ProductionNodeDistributionOrigin {
		return nil, fmt.Errorf("%w: Node distribution origin must be %s", ErrRegistryPolicy, ProductionNodeDistributionOrigin)
	}
	if config.metadataMax <= 0 {
		config.metadataMax = nodeRuntimeChecksumMaxBytes
	}
	if config.sourceMax <= 0 {
		config.sourceMax = DefaultNodeRuntimeSourceMaxBytes
	}
	if config.bundleMax <= 0 {
		config.bundleMax = DefaultNodeRuntimeBundleMaxBytes
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
	return &NodeRuntimeFetcher{
		artifactsDir: dir, origin: origin, originString: originString, client: client,
		metadataMax: config.metadataMax, sourceMax: config.sourceMax, bundleMax: config.bundleMax,
		metadataTimeout: config.metadataTimeout, downloadTimeout: config.downloadTimeout,
		allowHTTP: config.allowHTTP, now: config.now, serial: lock.(*sync.Mutex),
	}, nil
}

func (f *NodeRuntimeFetcher) PreviewPlan(ctx context.Context, version string) (NodeRuntimeFetchPlan, error) {
	if f == nil || f.client == nil || f.origin == nil || ctx == nil || !validNodeRuntimeVersion(version) {
		return NodeRuntimeFetchPlan{}, fmt.Errorf("%w: exact node-runtime version and fetcher are required", ErrInvalidFetchRequest)
	}
	checksumURL := f.nodeDistributionURL(version, "SHASUMS256.txt")
	requestCtx, cancel := context.WithTimeout(ctx, f.metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return NodeRuntimeFetchPlan{}, fmt.Errorf("%w: build Node checksum request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "text/plain")
	resp, err := f.client.Do(req)
	if err != nil {
		return NodeRuntimeFetchPlan{}, fmt.Errorf("%w: read Node checksums: %v", ErrMetadataInvalid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return NodeRuntimeFetchPlan{}, fmt.Errorf("%w: Node checksums returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return NodeRuntimeFetchPlan{}, fmt.Errorf("%w: Node checksums Content-Encoding is not identity", ErrMetadataInvalid)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaErr != nil || (mediaType != "text/plain" && mediaType != "application/octet-stream") {
		return NodeRuntimeFetchPlan{}, fmt.Errorf("%w: Node checksums Content-Type is not plain text", ErrMetadataInvalid)
	}
	body, err := readBoundedBody(resp.Body, resp.ContentLength, f.metadataMax, ErrMetadataTooLarge)
	if err != nil {
		return NodeRuntimeFetchPlan{}, err
	}
	sources, err := parseNodeRuntimeChecksums(version, body)
	if err != nil {
		return NodeRuntimeFetchPlan{}, err
	}
	now := f.now().UTC()
	if now.IsZero() {
		return NodeRuntimeFetchPlan{}, fmt.Errorf("%w: clock returned zero preview time", ErrInvalidFetchRequest)
	}
	plan := NodeRuntimeFetchPlan{
		PolicyVersion: NodeRuntimeFetchPolicyVersion, Name: "node-runtime", Version: version,
		SourceOrigin: f.originString, ChecksumURL: checksumURL, Sources: sources,
		SourceMaxBytes: f.sourceMax, BundleMaxBytes: f.bundleMax, PreviewedAt: now,
	}
	plan.SourceIdentity = nodeRuntimeSourceIdentity(plan)
	plan.PreviewDigest = nodeRuntimePreviewDigest(plan)
	return plan, nil
}

func parseNodeRuntimeChecksums(version string, body []byte) ([]NodeRuntimeSource, error) {
	wanted := map[string]NodeRuntimeSource{
		"node-v" + version + "-linux-x64.tar.gz": {
			TargetOS: "linux", TargetArch: "amd64", Filename: "node-v" + version + "-linux-x64.tar.gz",
		},
		"node-v" + version + "-linux-arm64.tar.gz": {
			TargetOS: "linux", TargetArch: "arm64", Filename: "node-v" + version + "-linux-arm64.tar.gz",
		},
		"node-v" + version + "-darwin-x64.tar.gz": {
			TargetOS: "darwin", TargetArch: "amd64", Filename: "node-v" + version + "-darwin-x64.tar.gz",
		},
		"node-v" + version + "-darwin-arm64.tar.gz": {
			TargetOS: "darwin", TargetArch: "arm64", Filename: "node-v" + version + "-darwin-arm64.tar.gz",
		},
		"node-v" + version + "-win-x64.zip": {
			TargetOS: "windows", TargetArch: "amd64", Filename: "node-v" + version + "-win-x64.zip",
		},
		"node-v" + version + "-win-arm64.zip": {
			TargetOS: "windows", TargetArch: "arm64", Filename: "node-v" + version + "-win-arm64.zip",
		},
	}
	found := make(map[string]NodeRuntimeSource, len(wanted))
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !ValidSHA256Hex(fields[0]) || fields[1] == "" || path.IsAbs(fields[1]) ||
			path.Clean(fields[1]) != fields[1] || strings.ContainsAny(fields[1], "\\\x00") {
			return nil, fmt.Errorf("%w: Node checksum line is invalid", ErrMetadataInvalid)
		}
		source, selected := wanted[fields[1]]
		if !selected {
			continue
		}
		if _, duplicate := found[fields[1]]; duplicate {
			return nil, fmt.Errorf("%w: Node checksum target is duplicated", ErrMetadataInvalid)
		}
		source.SHA256 = fields[0]
		found[fields[1]] = source
	}
	if len(found) != len(wanted) {
		return nil, fmt.Errorf("%w: Node checksums do not contain every supported target", ErrMetadataInvalid)
	}
	result := make([]NodeRuntimeSource, 0, len(found))
	for _, source := range found {
		result = append(result, source)
	}
	sort.Slice(result, func(i, j int) bool {
		return nodeRuntimeTargetOrder(result[i]) < nodeRuntimeTargetOrder(result[j])
	})
	return result, nil
}

func nodeRuntimeTargetOrder(source NodeRuntimeSource) string {
	osOrder := "9"
	switch source.TargetOS {
	case "linux":
		osOrder = "0"
	case "darwin":
		osOrder = "1"
	case "windows":
		osOrder = "2"
	}
	archOrder := "1"
	if source.TargetArch == "amd64" {
		archOrder = "0"
	}
	return osOrder + archOrder
}

func nodeRuntimeSourceKey(targetOS, targetArch string) string {
	return targetOS + "-" + targetArch
}

func validNodeRuntimeVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(version) > 128 || len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

func ValidNodeRuntimeVersion(version string) bool { return validNodeRuntimeVersion(version) }

func (f *NodeRuntimeFetcher) nodeDistributionURL(version, filename string) string {
	return f.originString + "/dist/v" + url.PathEscape(version) + "/" + url.PathEscape(filename)
}

func nodeRuntimeSourceIdentity(plan NodeRuntimeFetchPlan) string {
	body := struct {
		PolicyVersion  string              `json:"policy_version"`
		Name           string              `json:"name"`
		Version        string              `json:"version"`
		SourceOrigin   string              `json:"source_origin"`
		ChecksumURL    string              `json:"checksum_url"`
		Sources        []NodeRuntimeSource `json:"sources"`
		SourceMaxBytes int64               `json:"source_max_bytes"`
		BundleMaxBytes int64               `json:"bundle_max_bytes"`
	}{
		plan.PolicyVersion, plan.Name, plan.Version, plan.SourceOrigin, plan.ChecksumURL,
		plan.Sources, plan.SourceMaxBytes, plan.BundleMaxBytes,
	}
	raw, _ := json.Marshal(body)
	sum := sha512.Sum512(raw)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func nodeRuntimePreviewDigest(plan NodeRuntimeFetchPlan) string {
	body := struct {
		PolicyVersion  string              `json:"policy_version"`
		Name           string              `json:"name"`
		Version        string              `json:"version"`
		SourceOrigin   string              `json:"source_origin"`
		ChecksumURL    string              `json:"checksum_url"`
		Sources        []NodeRuntimeSource `json:"sources"`
		SourceIdentity string              `json:"source_identity"`
		SourceMaxBytes int64               `json:"source_max_bytes"`
		BundleMaxBytes int64               `json:"bundle_max_bytes"`
	}{
		plan.PolicyVersion, plan.Name, plan.Version, plan.SourceOrigin, plan.ChecksumURL,
		plan.Sources, plan.SourceIdentity, plan.SourceMaxBytes, plan.BundleMaxBytes,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeNodeRuntimeSourcePlan(raw string) (NodeRuntimeFetchPlan, error) {
	var plan NodeRuntimeFetchPlan
	if raw == "" || len(raw) > MaxArtifactSourcePlanBytes {
		return plan, fmt.Errorf("%w: Node runtime source plan is missing or oversized", ErrInvalidFetchRequest)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("%w: decode Node runtime source plan", ErrInvalidFetchRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return plan, fmt.Errorf("%w: Node runtime source plan has trailing data", ErrInvalidFetchRequest)
	}
	canonical, err := marshalCompactNoEscape(plan)
	if err != nil || string(canonical) != raw {
		return plan, fmt.Errorf("%w: Node runtime source plan is not canonical", ErrInvalidFetchRequest)
	}
	return plan, nil
}

// ValidNodeRuntimeSourcePlan reports whether raw is the canonical private plan
// representation accepted across the durable Store boundary.
func ValidNodeRuntimeSourcePlan(raw string) bool {
	_, err := decodeNodeRuntimeSourcePlan(raw)
	return err == nil
}

func (f *NodeRuntimeFetcher) validatePlan(plan NodeRuntimeFetchPlan) error {
	if plan.PolicyVersion != NodeRuntimeFetchPolicyVersion || plan.Name != "node-runtime" ||
		!validNodeRuntimeVersion(plan.Version) || plan.SourceOrigin != f.originString ||
		plan.ChecksumURL != f.nodeDistributionURL(plan.Version, "SHASUMS256.txt") ||
		plan.SourceMaxBytes != f.sourceMax || plan.BundleMaxBytes != f.bundleMax ||
		plan.PreviewedAt.IsZero() || plan.PreviewedAt.Location() != time.UTC || len(plan.Sources) != 6 {
		return fmt.Errorf("%w: Node runtime plan does not match active policy", ErrInvalidFetchRequest)
	}
	want := []NodeRuntimeSource{
		{TargetOS: "linux", TargetArch: "amd64", Filename: "node-v" + plan.Version + "-linux-x64.tar.gz"},
		{TargetOS: "linux", TargetArch: "arm64", Filename: "node-v" + plan.Version + "-linux-arm64.tar.gz"},
		{TargetOS: "darwin", TargetArch: "amd64", Filename: "node-v" + plan.Version + "-darwin-x64.tar.gz"},
		{TargetOS: "darwin", TargetArch: "arm64", Filename: "node-v" + plan.Version + "-darwin-arm64.tar.gz"},
		{TargetOS: "windows", TargetArch: "amd64", Filename: "node-v" + plan.Version + "-win-x64.zip"},
		{TargetOS: "windows", TargetArch: "arm64", Filename: "node-v" + plan.Version + "-win-arm64.zip"},
	}
	for i := range want {
		if plan.Sources[i].TargetOS != want[i].TargetOS || plan.Sources[i].TargetArch != want[i].TargetArch ||
			plan.Sources[i].Filename != want[i].Filename || !ValidSHA256Hex(plan.Sources[i].SHA256) {
			return fmt.Errorf("%w: Node runtime source identity is invalid", ErrInvalidFetchRequest)
		}
	}
	if plan.SourceIdentity != nodeRuntimeSourceIdentity(plan) || plan.PreviewDigest != nodeRuntimePreviewDigest(plan) {
		return fmt.Errorf("%w: Node runtime plan digest does not match", ErrInvalidFetchRequest)
	}
	return nil
}

func (f *NodeRuntimeFetcher) FetchExact(ctx context.Context, plan NodeRuntimeFetchPlan, fetchedBy string,
	progress ProgressFunc,
) (Sidecar, bool, error) {
	if f == nil || f.client == nil || f.serial == nil || ctx == nil {
		return Sidecar{}, false, fmt.Errorf("%w: Node runtime fetcher or context is incomplete", ErrInvalidFetchRequest)
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
		sourcePaths[nodeRuntimeSourceKey(source.TargetOS, source.TargetArch)] = temp
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
		Name: "node-runtime", Version: plan.Version, TarballURL: plan.ChecksumURL,
		SHA512Integrity: plan.SourceIdentity, SHA256: sha256Hex, Size: size,
		EnginesNode: "", FetchedAt: f.now().UTC(), FetchedBy: fetchedBy,
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

func (f *NodeRuntimeFetcher) findCachedExact(ctx context.Context, plan NodeRuntimeFetchPlan) (Sidecar, bool, error) {
	entries, err := ScanCatalog(f.artifactsDir)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: read artifact catalog: %v", ErrArtifactStorage, err)
	}
	for _, entry := range entries {
		if entry.Record == nil || (entry.Status != CatalogAvailableUnverified && entry.Status != CatalogReady) {
			continue
		}
		record := *entry.Record
		if record.Name != "node-runtime" || record.Version != plan.Version || record.TarballURL != plan.ChecksumURL ||
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

func (f *NodeRuntimeFetcher) downloadSource(ctx context.Context, version string, source NodeRuntimeSource) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.nodeDistributionURL(version, source.Filename), nil)
	if err != nil {
		return "", fmt.Errorf("%w: build Node archive request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/gzip, application/zip, application/octet-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("artifact: download Node archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("artifact: Node archive returned HTTP %d", resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "", fmt.Errorf("%w: Node archive Content-Encoding is not identity", ErrMetadataInvalid)
	}
	if resp.ContentLength > f.sourceMax {
		return "", fmt.Errorf("%w: Node archive declared %d bytes, limit %d", ErrArtifactTooLarge, resp.ContentLength, f.sourceMax)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaErr != nil || (mediaType != "application/gzip" && mediaType != "application/octet-stream" &&
		mediaType != "application/x-gzip" && mediaType != "application/zip" && mediaType != "application/x-zip-compressed") {
		return "", fmt.Errorf("%w: Node archive Content-Type is not an allowed archive type", ErrMetadataInvalid)
	}
	temp, err := os.CreateTemp(f.artifactsDir, nodeRuntimeSourceTempPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("%w: create Node archive temp: %v", ErrArtifactStorage, err)
	}
	path := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return "", fmt.Errorf("%w: chmod Node archive temp: %v", ErrArtifactStorage, err)
	}
	h := sha256.New()
	written, err := io.CopyBuffer(io.MultiWriter(temp, h), io.LimitReader(resp.Body, f.sourceMax+1), make([]byte, 128<<10))
	if err != nil {
		return "", fmt.Errorf("artifact: download Node archive: %w", err)
	}
	if written == 0 || written > f.sourceMax {
		return "", fmt.Errorf("%w: Node archive size is outside policy", ErrArtifactTooLarge)
	}
	expected, _ := hex.DecodeString(source.SHA256)
	if subtle.ConstantTimeCompare(h.Sum(nil), expected) != 1 {
		return "", ErrIntegrityMismatch
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("%w: fsync Node archive temp: %v", ErrArtifactStorage, err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("%w: close Node archive temp: %v", ErrArtifactStorage, err)
	}
	keep = true
	return path, nil
}

type boundedHashWriter struct {
	w       io.Writer
	hash    hash.Hash
	written int64
	maximum int64
}

func (w *boundedHashWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.maximum-w.written {
		return 0, ErrArtifactTooLarge
	}
	n, err := w.w.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.written += int64(n)
	}
	return n, err
}

func (f *NodeRuntimeFetcher) buildBundle(ctx context.Context, plan NodeRuntimeFetchPlan,
	sourcePaths map[string]string,
) (string, string, int64, error) {
	temp, err := os.CreateTemp(f.artifactsDir, artifactFetchTempPrefix+"*.tmp")
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Node bundle temp: %v", ErrArtifactStorage, err)
	}
	path := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return "", "", 0, fmt.Errorf("%w: chmod Node bundle temp: %v", ErrArtifactStorage, err)
	}
	digest := sha256.New()
	output := &boundedHashWriter{w: temp, hash: digest, maximum: plan.BundleMaxBytes}
	gz, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Node bundle gzip: %v", ErrArtifactStorage, err)
	}
	gz.Header.ModTime = time.Unix(0, 0).UTC()
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	closeBundle := func() error {
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}
	for _, name := range []string{
		"node-runtime/", "node-runtime/linux-amd64/", "node-runtime/linux-arm64/",
		"node-runtime/darwin-amd64/", "node-runtime/darwin-arm64/",
		"node-runtime/windows-amd64/", "node-runtime/windows-arm64/",
	} {
		if err := tw.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeDir, 0, "", false)); err != nil {
			return "", "", 0, fmt.Errorf("%w: write Node bundle root: %v", ErrArtifactStorage, err)
		}
	}
	budget := &nodeRuntimeBuildBudget{entries: 7}
	for _, source := range plan.Sources {
		if err := copyNodeRuntimeSource(ctx, tw,
			sourcePaths[nodeRuntimeSourceKey(source.TargetOS, source.TargetArch)], plan.Version, source, budget); err != nil {
			return "", "", 0, err
		}
	}
	if err := closeBundle(); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return "", "", 0, ErrArtifactTooLarge
		}
		return "", "", 0, fmt.Errorf("%w: close Node bundle: %v", ErrArtifactStorage, err)
	}
	if output.written == 0 {
		return "", "", 0, fmt.Errorf("%w: Node bundle is empty", ErrMetadataInvalid)
	}
	if err := temp.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("%w: fsync Node bundle: %v", ErrArtifactStorage, err)
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Node bundle: %v", ErrArtifactStorage, err)
	}
	keep = true
	return path, hex.EncodeToString(digest.Sum(nil)), output.written, nil
}

type nodeRuntimeBuildBudget struct {
	entries      int
	uncompressed int64
}

func copyNodeRuntimeSource(ctx context.Context, destination *tar.Writer, sourcePath, version string,
	source NodeRuntimeSource, budget *nodeRuntimeBuildBudget,
) error {
	if source.TargetOS == "windows" {
		return copyNodeRuntimeZipSource(ctx, destination, sourcePath, version, source, budget)
	}
	input, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("%w: open Node archive: %v", ErrArtifactStorage, err)
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("%w: open Node archive gzip: %v", ErrMetadataInvalid, err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	rootArch := "x64"
	if source.TargetArch == "arm64" {
		rootArch = "arm64"
	}
	root := "node-v" + version + "-" + source.TargetOS + "-" + rootArch
	prefix := "node-runtime/" + source.TargetOS + "-" + source.TargetArch
	seen := make(map[string]struct{})
	entryTypes := make(map[string]byte)
	parentsWithChildren := make(map[string]struct{})
	nodeFound, npmFound := false, false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read Node archive: %v", ErrMetadataInvalid, err)
		}
		budget.entries++
		if budget.entries > nodeRuntimeMaxEntries || header.Size < 0 || header.Size > nodeRuntimeMaxFileBytes ||
			budget.uncompressed > nodeRuntimeMaxUncompressedBytes-header.Size {
			return fmt.Errorf("%w: Node archive exceeds extraction policy", ErrArtifactTooLarge)
		}
		budget.uncompressed += header.Size
		relative, err := nodeRuntimeSourcePath(header.Name, root)
		if err != nil {
			return err
		}
		if relative == "" {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("%w: Node archive root is not a directory", ErrMetadataInvalid)
			}
			continue
		}
		if _, duplicate := seen[relative]; duplicate {
			return fmt.Errorf("%w: Node archive contains a duplicate path", ErrMetadataInvalid)
		}
		seen[relative] = struct{}{}
		for parent := path.Dir(relative); parent != "." && parent != "/"; parent = path.Dir(parent) {
			parentsWithChildren[parent] = struct{}{}
			if kind, exists := entryTypes[parent]; exists && kind != tar.TypeDir {
				return fmt.Errorf("%w: Node archive traverses a non-directory", ErrMetadataInvalid)
			}
		}
		executable := header.Mode&0o111 != 0
		switch header.Typeflag {
		case tar.TypeDir:
		case tar.TypeReg, tar.TypeRegA:
		case tar.TypeSymlink:
			if header.Size != 0 || !validNodeRuntimeSourceLink(relative, header.Linkname) {
				return fmt.Errorf("%w: Node archive symlink is invalid", ErrMetadataInvalid)
			}
		default:
			return fmt.Errorf("%w: Node archive entry type is unsupported", ErrMetadataInvalid)
		}
		if header.Typeflag != tar.TypeDir {
			if _, hasChildren := parentsWithChildren[relative]; hasChildren {
				return fmt.Errorf("%w: Node archive non-directory replaces an ancestor", ErrMetadataInvalid)
			}
		}
		entryTypes[relative] = header.Typeflag
		if relative == "bin/node" && (header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA) && executable {
			nodeFound = true
		}
		if relative == "lib/node_modules/npm/bin/npm-cli.js" && (header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA) {
			npmFound = true
		}
		name := prefix + "/" + relative
		if header.Typeflag == tar.TypeDir {
			name += "/"
		}
		out := nodeRuntimeBundleHeader(name, header.Typeflag, header.Size, header.Linkname, executable)
		if err := destination.WriteHeader(out); err != nil {
			return fmt.Errorf("%w: write Node bundle header: %v", ErrArtifactStorage, err)
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			written, err := io.CopyN(destination, reader, header.Size)
			if err != nil || written != header.Size {
				return fmt.Errorf("%w: Node archive file is incomplete", ErrMetadataInvalid)
			}
		}
	}
	if !nodeFound || !npmFound {
		return fmt.Errorf("%w: Node archive lacks node or npm", ErrMetadataInvalid)
	}
	return nil
}

func copyNodeRuntimeZipSource(ctx context.Context, destination *tar.Writer, sourcePath, version string,
	source NodeRuntimeSource, budget *nodeRuntimeBuildBudget,
) error {
	reader, err := zip.OpenReader(sourcePath)
	if err != nil {
		return fmt.Errorf("%w: open Node zip archive: %v", ErrMetadataInvalid, err)
	}
	defer reader.Close()
	rootArch := "x64"
	if source.TargetArch == "arm64" {
		rootArch = "arm64"
	}
	root := "node-v" + version + "-win-" + rootArch
	prefix := "node-runtime/" + source.TargetOS + "-" + source.TargetArch
	seen := make(map[string]struct{})
	emitted := map[string]struct{}{prefix + "/": {}}
	nodeFound, npmFound := false, false
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		budget.entries++
		if budget.entries > nodeRuntimeMaxEntries || file.UncompressedSize64 > uint64(nodeRuntimeMaxFileBytes) {
			return fmt.Errorf("%w: Node archive exceeds extraction policy", ErrArtifactTooLarge)
		}
		name := strings.ReplaceAll(file.Name, "\\", "/")
		relative, err := nodeRuntimeSourcePath(name, root)
		if err != nil {
			return err
		}
		if relative == "" {
			continue
		}
		if file.FileInfo().IsDir() {
			relative = remapWindowsNodeRelative(strings.TrimSuffix(relative, "/"))
			if relative == "" {
				continue
			}
			if err := emitNodeRuntimeBundleDirs(destination, prefix, path.Dir(relative+"/x"), emitted, budget); err != nil {
				return err
			}
			continue
		}
		relative = remapWindowsNodeRelative(relative)
		if _, duplicate := seen[relative]; duplicate {
			return fmt.Errorf("%w: Node archive contains a duplicate path", ErrMetadataInvalid)
		}
		seen[relative] = struct{}{}
		if budget.uncompressed > nodeRuntimeMaxUncompressedBytes-int64(file.UncompressedSize64) {
			return fmt.Errorf("%w: Node archive exceeds extraction policy", ErrArtifactTooLarge)
		}
		budget.uncompressed += int64(file.UncompressedSize64)
		if err := emitNodeRuntimeBundleDirs(destination, prefix, path.Dir(relative), emitted, budget); err != nil {
			return err
		}
		opened, err := file.Open()
		if err != nil {
			return fmt.Errorf("%w: read Node zip file: %v", ErrMetadataInvalid, err)
		}
		executable := relative == "bin/node.exe" || strings.HasSuffix(relative, ".exe")
		header := nodeRuntimeBundleHeader(prefix+"/"+relative, tar.TypeReg, int64(file.UncompressedSize64), "", executable)
		if err := destination.WriteHeader(header); err != nil {
			_ = opened.Close()
			return fmt.Errorf("%w: write Node bundle header: %v", ErrArtifactStorage, err)
		}
		written, err := io.CopyN(destination, opened, int64(file.UncompressedSize64))
		_ = opened.Close()
		if err != nil || written != int64(file.UncompressedSize64) {
			return fmt.Errorf("%w: Node archive file is incomplete", ErrMetadataInvalid)
		}
		if relative == "bin/node.exe" {
			nodeFound = true
		}
		if relative == "lib/node_modules/npm/bin/npm-cli.js" {
			npmFound = true
		}
	}
	if !nodeFound || !npmFound {
		return fmt.Errorf("%w: Node archive lacks node or npm", ErrMetadataInvalid)
	}
	return nil
}

func remapWindowsNodeRelative(relative string) string {
	if relative == "node.exe" {
		return "bin/node.exe"
	}
	if relative == "node_modules" || strings.HasPrefix(relative, "node_modules/") {
		return path.Join("lib", relative)
	}
	return relative
}

func emitNodeRuntimeBundleDirs(destination *tar.Writer, prefix, dir string, emitted map[string]struct{},
	budget *nodeRuntimeBuildBudget,
) error {
	if dir == "." || dir == "/" || dir == "" {
		return nil
	}
	var parts []string
	for parent := dir; parent != "." && parent != "/"; parent = path.Dir(parent) {
		parts = append([]string{parent}, parts...)
	}
	for _, relative := range parts {
		name := prefix + "/" + relative + "/"
		if _, exists := emitted[name]; exists {
			continue
		}
		budget.entries++
		if budget.entries > nodeRuntimeMaxEntries {
			return fmt.Errorf("%w: Node archive exceeds extraction policy", ErrArtifactTooLarge)
		}
		if err := destination.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeDir, 0, "", false)); err != nil {
			return fmt.Errorf("%w: write Node bundle root: %v", ErrArtifactStorage, err)
		}
		emitted[name] = struct{}{}
	}
	return nil
}

func nodeRuntimeSourcePath(raw, root string) (string, error) {
	if raw == "" || strings.Contains(raw, "\\") || strings.ContainsRune(raw, '\x00') || path.IsAbs(raw) {
		return "", fmt.Errorf("%w: Node archive path is invalid", ErrMetadataInvalid)
	}
	name := path.Clean(strings.TrimSuffix(raw, "/"))
	if name == "." || name == ".." || strings.HasPrefix(name, "../") || (raw != name && raw != name+"/") {
		return "", fmt.Errorf("%w: Node archive path is not canonical", ErrMetadataInvalid)
	}
	if name == root {
		return "", nil
	}
	if !strings.HasPrefix(name, root+"/") {
		return "", fmt.Errorf("%w: Node archive path is outside its root", ErrMetadataInvalid)
	}
	return strings.TrimPrefix(name, root+"/"), nil
}

func validNodeRuntimeSourceLink(relative, target string) bool {
	if target == "" || strings.Contains(target, "\\") || strings.ContainsRune(target, '\x00') || path.IsAbs(target) {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(relative), target))
	return resolved != "." && resolved != ".." && !strings.HasPrefix(resolved, "../")
}

func nodeRuntimeBundleHeader(name string, kind byte, size int64, link string, executable bool) *tar.Header {
	mode := int64(0o644)
	if kind == tar.TypeDir || executable {
		mode = 0o755
	} else if kind == tar.TypeSymlink {
		mode = 0o777
	}
	return &tar.Header{
		Name: name, Typeflag: kind, Mode: mode, Size: size, Linkname: link,
		ModTime: time.Unix(0, 0).UTC(), AccessTime: time.Time{}, ChangeTime: time.Time{},
		Uid: 0, Gid: 0, Uname: "", Gname: "", Format: tar.FormatPAX,
	}
}

func checkExistingSidecarBinding(dir string, record Sidecar) error {
	path := filepath.Join(dir, record.SHA256+".json")
	existing, present, err := readSidecarFile(path)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if existing.Name != record.Name || existing.Version != record.Version ||
		existing.TarballURL != record.TarballURL || existing.SHA512Integrity != record.SHA512Integrity ||
		existing.SHA256 != record.SHA256 || existing.Size != record.Size || existing.EnginesNode != record.EnginesNode {
		return fmt.Errorf("%w: digest %s is already bound to different metadata", ErrArtifactStorage, record.SHA256)
	}
	return nil
}

func (f *NodeRuntimeFetcher) ReconcileStaleTemps(before time.Time) (int, error) {
	if f == nil || f.serial == nil || before.IsZero() {
		return 0, fmt.Errorf("%w: fetcher and non-zero stale cutoff are required", ErrInvalidFetchRequest)
	}
	f.serial.Lock()
	defer f.serial.Unlock()
	if err := ensurePrivateArtifactDir(f.artifactsDir); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(f.artifactsDir)
	if err != nil {
		return 0, fmt.Errorf("%w: list artifact temp files: %v", ErrArtifactStorage, err)
	}
	removed := 0
	for _, entry := range entries {
		if !isArtifactTempName(entry.Name()) {
			continue
		}
		candidate := filepath.Join(f.artifactsDir, entry.Name())
		info, err := os.Lstat(candidate)
		if err != nil {
			return removed, fmt.Errorf("%w: inspect temp file: %v", ErrArtifactStorage, err)
		}
		if info.IsDir() {
			return removed, fmt.Errorf("%w: refusing to remove temp-named directory", ErrArtifactStorage)
		}
		if info.ModTime().After(before) {
			continue
		}
		if err := os.Remove(candidate); err != nil {
			return removed, fmt.Errorf("%w: remove stale temp file: %v", ErrArtifactStorage, err)
		}
		removed++
	}
	if removed > 0 {
		if err := syncDirectory(f.artifactsDir); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
