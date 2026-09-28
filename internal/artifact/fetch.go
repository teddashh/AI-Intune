package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	// ProductionRegistryOrigin is deliberately code-owned rather than supplied by
	// an operator request. Turning registry into request data would turn artifact
	// intake into a Hub-side SSRF primitive.
	ProductionRegistryOrigin   = "https://registry.npmjs.org"
	FetchPolicyVersion         = "openclaw-npm-fetch:v2"
	ArtifactSourceNPM          = "npm-package:v1"
	ArtifactSourceNode         = "node-runtime-bundle:v1"
	MaxArtifactSourcePlanBytes = 8192
	DefaultArtifactMaxBytes    = int64(1 << 30)
	DefaultMetadataMaxBytes    = int64(4 << 20)
	// MaxTarballURLBytes is shared by registry intake, durable worker state,
	// and sidecar catalog validation so a fetched URL cannot become unreadable
	// after it crosses a subsystem boundary.
	MaxTarballURLBytes = 2048

	productionMetadataTimeout = 30 * time.Second
	productionDownloadTimeout = 30 * time.Minute
	// Download callbacks ultimately become SQLite writer transactions. Keep
	// byte-level visibility without turning each transport Read into a ledger
	// write; phase boundaries below still persist the exact final byte count.
	fetchProgressCheckpointBytes = int64(4 << 20)
	maxSidecarBytes              = int64(64 << 10)
	artifactFetchTempPrefix      = ".artifact-fetch-"
	artifactSidecarTempPrefix    = ".artifact-sidecar-"
)

var (
	ErrInvalidFetchRequest = errors.New("artifact: invalid fetch request")
	ErrRegistryPolicy      = errors.New("artifact: registry policy rejected input")
	ErrMetadataInvalid     = errors.New("artifact: registry metadata is invalid")
	ErrMetadataTooLarge    = errors.New("artifact: registry metadata exceeds limit")
	ErrArtifactTooLarge    = errors.New("artifact: tarball exceeds limit")
	ErrIntegrityMismatch   = errors.New("artifact: tarball integrity mismatch")
	ErrArtifactStorage     = errors.New("artifact: storage operation failed")
)

var exactNPMVersionPattern = regexp.MustCompile(
	`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`,
)

// PreviewPlan is an exact, policy-bound registry answer. TarballURL remains
// internal control material and is excluded from default JSON serialization:
// an adapter must never disclose a future signed URL by embedding this value
// directly in an operator response.
type PreviewPlan struct {
	PolicyVersion   string    `json:"policy_version"`
	SourceKind      string    `json:"source_kind"`
	Name            string    `json:"name"`
	Version         string    `json:"version"`
	RegistryOrigin  string    `json:"registry_origin"`
	TarballURL      string    `json:"-"`
	SHA512Integrity string    `json:"sha512_integrity"`
	EnginesNode     string    `json:"engines_node"`
	MaxBytes        int64     `json:"max_bytes"`
	PreviewedAt     time.Time `json:"previewed_at"`
	PreviewDigest   string    `json:"preview_digest"`
	SourcePlan      string    `json:"-"`
}

// FetchPhase is an ordered durable-worker milestone. Downloading may be
// reported more than once. Verifying is reported once; Publishing may be
// repeated immediately before each atomic publish step so a fenced worker can
// stop before making the sidecar authoritative.
type FetchPhase string

const (
	FetchPhaseDownloading FetchPhase = "downloading"
	FetchPhaseVerifying   FetchPhase = "verifying"
	FetchPhasePublishing  FetchPhase = "publishing"
)

// FetchProgress is safe to persist as asynchronous operation progress.
// ExpectedBytes is nil when the origin did not provide a trustworthy length.
// A callback is invoked serially, and publication never starts unless the
// Publishing update is accepted.
type FetchProgress struct {
	Phase           FetchPhase `json:"phase"`
	DownloadedBytes int64      `json:"downloaded_bytes"`
	ExpectedBytes   *int64     `json:"expected_bytes,omitempty"`
}

// ProgressFunc may stop a download by returning an error. This lets a durable
// worker fail closed if it cannot persist progress or loses operation authority.
type ProgressFunc func(FetchProgress) error

type Fetcher struct {
	artifactsDir    string
	registry        *url.URL
	registryOrigin  string
	client          *http.Client
	metadataMax     int64
	artifactMax     int64
	metadataTimeout time.Duration
	downloadTimeout time.Duration
	allowHTTP       bool
	now             func() time.Time
	serial          *sync.Mutex
	nodeRuntime     *NodeRuntimeFetcher
	hermesImage     *HermesImageFetcher
}

type fetcherConfig struct {
	artifactsDir         string
	registryURL          string
	client               *http.Client
	metadataMax          int64
	artifactMax          int64
	metadataTimeout      time.Duration
	downloadTimeout      time.Duration
	allowHTTP            bool // tests only; the exported constructor never enables it.
	now                  func() time.Time
	nodeOriginURL        string
	hermesRegistryURL    string
	hermesTokenOriginURL string
}

var fetchDirectoryLocks sync.Map // canonical directory -> *sync.Mutex

// NewFetcher constructs the production OpenClaw fetcher. Its registry and
// outbound policy are intentionally not caller-configurable.
func NewFetcher(artifactsDir string) (*Fetcher, error) {
	return newFetcher(fetcherConfig{
		artifactsDir: artifactsDir, registryURL: ProductionRegistryOrigin,
		metadataMax: DefaultMetadataMaxBytes, artifactMax: DefaultArtifactMaxBytes,
		metadataTimeout: productionMetadataTimeout, downloadTimeout: productionDownloadTimeout,
		now: time.Now,
	})
}

func newFetcher(config fetcherConfig) (*Fetcher, error) {
	dir := filepath.Clean(config.artifactsDir)
	if config.artifactsDir == "" || !filepath.IsAbs(config.artifactsDir) || dir != config.artifactsDir {
		return nil, fmt.Errorf("%w: artifacts directory must be a canonical absolute path", ErrInvalidFetchRequest)
	}
	if filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("%w: filesystem root cannot be an artifacts directory", ErrInvalidFetchRequest)
	}
	if config.registryURL == "" {
		return nil, fmt.Errorf("%w: registry is required", ErrRegistryPolicy)
	}
	registry, origin, err := parseRegistryURL(config.registryURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	if !config.allowHTTP && origin != ProductionRegistryOrigin {
		return nil, fmt.Errorf("%w: production registry must be %s", ErrRegistryPolicy, ProductionRegistryOrigin)
	}
	if config.metadataMax <= 0 {
		config.metadataMax = DefaultMetadataMaxBytes
	}
	if config.artifactMax <= 0 {
		config.artifactMax = DefaultArtifactMaxBytes
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
	fetcher := &Fetcher{
		artifactsDir: dir, registry: registry, registryOrigin: origin,
		client: client, metadataMax: config.metadataMax, artifactMax: config.artifactMax,
		metadataTimeout: config.metadataTimeout, downloadTimeout: config.downloadTimeout,
		allowHTTP: config.allowHTTP, now: config.now, serial: lock.(*sync.Mutex),
	}
	nodeOriginURL := config.nodeOriginURL
	if nodeOriginURL == "" {
		nodeOriginURL = ProductionNodeDistributionOrigin
		if config.allowHTTP {
			nodeOriginURL = config.registryURL
		}
	}
	nodeSourceMax := config.artifactMax
	if nodeSourceMax > DefaultNodeRuntimeSourceMaxBytes {
		nodeSourceMax = DefaultNodeRuntimeSourceMaxBytes
	}
	nodeRuntime, err := newNodeRuntimeFetcher(nodeRuntimeFetcherConfig{
		artifactsDir: dir, originURL: nodeOriginURL, client: config.client,
		metadataMax: config.metadataMax, sourceMax: nodeSourceMax, bundleMax: config.artifactMax,
		metadataTimeout: config.metadataTimeout, downloadTimeout: config.downloadTimeout,
		allowHTTP: config.allowHTTP, now: config.now,
	})
	if err != nil {
		return nil, err
	}
	fetcher.nodeRuntime = nodeRuntime
	hermesRegistryURL := config.hermesRegistryURL
	if hermesRegistryURL == "" {
		hermesRegistryURL = ProductionHermesRegistryOrigin
	}
	hermesTokenOriginURL := config.hermesTokenOriginURL
	if hermesTokenOriginURL == "" {
		hermesTokenOriginURL = ProductionHermesTokenOrigin
	}
	hermesImage, err := newHermesImageFetcher(hermesImageFetcherConfig{
		artifactsDir: dir, registryURL: hermesRegistryURL, tokenOriginURL: hermesTokenOriginURL,
		repository: HermesImageRepository, client: config.client,
		manifestMax: config.metadataMax, blobMax: DefaultHermesImageBlobMaxBytes,
		bundleMax:       DefaultHermesImageBundleMaxBytes,
		metadataTimeout: config.metadataTimeout, downloadTimeout: 2 * time.Hour,
		allowHTTP: config.allowHTTP, now: config.now,
	})
	if err != nil {
		return nil, err
	}
	fetcher.hermesImage = hermesImage
	return fetcher, nil
}

func hardenedFetchHTTPClient(input *http.Client, headerTimeout, requestTimeout time.Duration) (*http.Client, error) {
	client := http.Client{}
	if input != nil {
		client = *input
	}
	transport := client.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		}
	}
	base, ok := transport.(*http.Transport)
	if !ok || base == nil {
		return nil, fmt.Errorf("%w: fetch transport must be *http.Transport", ErrRegistryPolicy)
	}
	clone := base.Clone()
	clone.Proxy = nil
	clone.DisableCompression = true
	clone.MaxResponseHeaderBytes = 64 << 10
	if clone.ResponseHeaderTimeout <= 0 || clone.ResponseHeaderTimeout > headerTimeout {
		clone.ResponseHeaderTimeout = headerTimeout
	}
	client.Transport = clone
	client.Jar = nil
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if client.Timeout <= 0 || client.Timeout > requestTimeout {
		client.Timeout = requestTimeout
	}
	return &client, nil
}

func parseRegistryURL(raw string, allowHTTP bool) (*url.URL, string, error) {
	if raw != strings.TrimSpace(raw) {
		return nil, "", fmt.Errorf("%w: registry URL has surrounding whitespace", ErrRegistryPolicy)
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" {
		return nil, "", fmt.Errorf("%w: registry URL is not an absolute hierarchical URL", ErrRegistryPolicy)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		u.Path != "" || u.RawPath != "" {
		return nil, "", fmt.Errorf("%w: registry URL may contain only scheme and authority", ErrRegistryPolicy)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return nil, "", fmt.Errorf("%w: registry must use HTTPS", ErrRegistryPolicy)
	}
	origin, err := canonicalOrigin(u)
	if err != nil {
		return nil, "", err
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return nil, "", fmt.Errorf("%w: canonical registry origin is invalid", ErrRegistryPolicy)
	}
	return parsed, origin, nil
}

func canonicalOrigin(u *url.URL) (string, error) {
	if u == nil || u.Hostname() == "" {
		return "", fmt.Errorf("%w: URL has no hostname", ErrRegistryPolicy)
	}
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	return strings.ToLower(u.Scheme) + "://" + host, nil
}

type registryVersionMetadata struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
	Engines struct {
		Node string `json:"node"`
	} `json:"engines"`
}

// PreviewPlan reads exact-version metadata from the fixed registry and binds
// every value that may affect the later byte fetch into PreviewDigest.
func (f *Fetcher) PreviewPlan(ctx context.Context, name, version string) (PreviewPlan, error) {
	if f == nil || f.client == nil || f.registry == nil || ctx == nil {
		return PreviewPlan{}, fmt.Errorf("%w: fetcher or context is incomplete", ErrInvalidFetchRequest)
	}
	if name == "node-runtime" {
		if f.nodeRuntime == nil {
			return PreviewPlan{}, fmt.Errorf("%w: Node runtime fetcher is unavailable", ErrInvalidFetchRequest)
		}
		plan, err := f.nodeRuntime.PreviewPlan(ctx, version)
		if err != nil {
			return PreviewPlan{}, err
		}
		raw, err := marshalCompactNoEscape(plan)
		if err != nil || len(raw) > MaxArtifactSourcePlanBytes {
			return PreviewPlan{}, fmt.Errorf("%w: encode Node runtime source plan", ErrInvalidFetchRequest)
		}
		return PreviewPlan{
			PolicyVersion: plan.PolicyVersion, SourceKind: ArtifactSourceNode,
			Name: plan.Name, Version: plan.Version, RegistryOrigin: plan.SourceOrigin,
			TarballURL: plan.ChecksumURL, SHA512Integrity: plan.SourceIdentity,
			MaxBytes: plan.BundleMaxBytes, PreviewedAt: plan.PreviewedAt,
			PreviewDigest: plan.PreviewDigest, SourcePlan: string(raw),
		}, nil
	}
	if name == "hermes-agent" {
		if f.hermesImage == nil {
			return PreviewPlan{}, fmt.Errorf("%w: Hermes image fetcher is unavailable", ErrInvalidFetchRequest)
		}
		plan, err := f.hermesImage.PreviewPlan(ctx, version)
		if err != nil {
			return PreviewPlan{}, err
		}
		raw, err := marshalCompactNoEscape(plan)
		if err != nil || len(raw) > MaxArtifactSourcePlanBytes {
			return PreviewPlan{}, fmt.Errorf("%w: encode Hermes image source plan", ErrInvalidFetchRequest)
		}
		return PreviewPlan{
			PolicyVersion: plan.PolicyVersion, SourceKind: ArtifactSourceHermesImage,
			Name: plan.Name, Version: plan.Version, RegistryOrigin: plan.RegistryOrigin,
			TarballURL:      f.hermesImage.registryURL("manifests", plan.IndexDigest),
			SHA512Integrity: plan.SourceIdentity, MaxBytes: plan.BundleMaxBytes,
			PreviewedAt: plan.PreviewedAt, PreviewDigest: plan.PreviewDigest, SourcePlan: string(raw),
		}, nil
	}
	if name != "openclaw" || !validExactNPMVersion(version) {
		return PreviewPlan{}, fmt.Errorf("%w: target must be exact openclaw, node-runtime, or hermes-agent", ErrInvalidFetchRequest)
	}
	requestCtx, cancel := context.WithTimeout(ctx, f.metadataTimeout)
	defer cancel()
	endpoint := f.registryOrigin + "/" + url.PathEscape(name) + "/" + url.PathEscape(version)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return PreviewPlan{}, fmt.Errorf("%w: build metadata request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return PreviewPlan{}, fmt.Errorf("%w: read registry metadata: %v", ErrMetadataInvalid, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return PreviewPlan{}, fmt.Errorf("%w: registry metadata returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return PreviewPlan{}, fmt.Errorf("%w: registry metadata Content-Encoding is not identity", ErrMetadataInvalid)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaErr != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return PreviewPlan{}, fmt.Errorf("%w: registry metadata Content-Type is not JSON", ErrMetadataInvalid)
	}
	body, err := readBoundedBody(resp.Body, resp.ContentLength, f.metadataMax, ErrMetadataTooLarge)
	if err != nil {
		return PreviewPlan{}, err
	}
	var metadata registryVersionMetadata
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&metadata); err != nil {
		return PreviewPlan{}, fmt.Errorf("%w: decode registry metadata: %v", ErrMetadataInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return PreviewPlan{}, fmt.Errorf("%w: registry metadata contains trailing JSON", ErrMetadataInvalid)
	}
	if metadata.Name != name || metadata.Version != version {
		return PreviewPlan{}, fmt.Errorf("%w: registry metadata identity does not match request", ErrMetadataInvalid)
	}
	if !validBoundedText(metadata.Engines.Node, 512, true) {
		return PreviewPlan{}, fmt.Errorf("%w: registry engines.node is invalid", ErrMetadataInvalid)
	}
	tarballURL, err := f.validateTarballURL(metadata.Dist.Tarball)
	if err != nil {
		return PreviewPlan{}, err
	}
	_, canonicalSRI, err := decodeCanonicalSHA512SRI(metadata.Dist.Integrity)
	if err != nil {
		return PreviewPlan{}, fmt.Errorf("%w: %v", ErrMetadataInvalid, err)
	}
	now := f.now().UTC()
	if now.IsZero() {
		return PreviewPlan{}, fmt.Errorf("%w: clock returned zero preview time", ErrInvalidFetchRequest)
	}
	plan := PreviewPlan{
		PolicyVersion: FetchPolicyVersion, SourceKind: ArtifactSourceNPM, Name: name, Version: version,
		RegistryOrigin: f.registryOrigin, TarballURL: tarballURL,
		SHA512Integrity: canonicalSRI, EnginesNode: metadata.Engines.Node,
		MaxBytes: f.artifactMax, PreviewedAt: now,
	}
	plan.PreviewDigest = previewPlanDigest(plan)
	return plan, nil
}

func (f *Fetcher) validateTarballURL(raw string) (string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || len(raw) > MaxTarballURLBytes {
		return "", fmt.Errorf("%w: tarball URL is missing or malformed", ErrRegistryPolicy)
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.Path == "" {
		return "", fmt.Errorf("%w: tarball URL is not absolute and hierarchical", ErrRegistryPolicy)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return "", fmt.Errorf("%w: tarball URL may not contain credentials, query, or fragment", ErrRegistryPolicy)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && !(f.allowHTTP && u.Scheme == "http") {
		return "", fmt.Errorf("%w: tarball URL must use HTTPS", ErrRegistryPolicy)
	}
	origin, err := canonicalOrigin(u)
	if err != nil {
		return "", err
	}
	if origin != f.registryOrigin {
		return "", fmt.Errorf("%w: tarball URL is not on the configured registry origin", ErrRegistryPolicy)
	}
	u.Scheme = f.registry.Scheme
	u.Host = f.registry.Host
	return u.String(), nil
}

func validExactNPMVersion(version string) bool {
	return len(version) <= 128 && exactNPMVersionPattern.MatchString(version)
}

func ValidOpenClawVersion(version string) bool { return validExactNPMVersion(version) }

func validBoundedText(value string, maxBytes int, emptyOK bool) bool {
	if (!emptyOK && value == "") || len(value) > maxBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func decodeCanonicalSHA512SRI(value string) ([]byte, string, error) {
	encoded, ok := strings.CutPrefix(value, "sha512-")
	if !ok || encoded == "" || value != strings.TrimSpace(value) {
		return nil, "", errors.New("SHA-512 integrity must be one sha512-<base64> value")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(raw) != sha512.Size {
		return nil, "", errors.New("SHA-512 integrity does not decode to 64 bytes")
	}
	return raw, "sha512-" + base64.StdEncoding.EncodeToString(raw), nil
}

func previewPlanDigest(plan PreviewPlan) string {
	body := struct {
		PolicyVersion   string `json:"policy_version"`
		SourceKind      string `json:"source_kind"`
		Name            string `json:"name"`
		Version         string `json:"version"`
		RegistryOrigin  string `json:"registry_origin"`
		TarballURL      string `json:"tarball_url"`
		SHA512Integrity string `json:"sha512_integrity"`
		EnginesNode     string `json:"engines_node"`
		MaxBytes        int64  `json:"max_bytes"`
	}{
		plan.PolicyVersion, plan.SourceKind, plan.Name, plan.Version, plan.RegistryOrigin,
		plan.TarballURL, plan.SHA512Integrity, plan.EnginesNode, plan.MaxBytes,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (f *Fetcher) validatePlan(plan PreviewPlan) ([]byte, error) {
	if plan.SourceKind != ArtifactSourceNPM || plan.SourcePlan != "" ||
		plan.PolicyVersion != FetchPolicyVersion || plan.Name != "openclaw" ||
		!validExactNPMVersion(plan.Version) || plan.RegistryOrigin != f.registryOrigin ||
		plan.MaxBytes != f.artifactMax || plan.PreviewedAt.IsZero() ||
		!validBoundedText(plan.EnginesNode, 512, true) {
		return nil, fmt.Errorf("%w: preview plan does not match active fetch policy", ErrInvalidFetchRequest)
	}
	canonicalURL, err := f.validateTarballURL(plan.TarballURL)
	if err != nil || canonicalURL != plan.TarballURL {
		return nil, fmt.Errorf("%w: preview tarball URL is not canonical", ErrInvalidFetchRequest)
	}
	expected, canonicalSRI, err := decodeCanonicalSHA512SRI(plan.SHA512Integrity)
	if err != nil || canonicalSRI != plan.SHA512Integrity {
		return nil, fmt.Errorf("%w: preview SHA-512 integrity is not canonical", ErrInvalidFetchRequest)
	}
	if plan.PreviewDigest == "" || plan.PreviewDigest != previewPlanDigest(plan) {
		return nil, fmt.Errorf("%w: preview digest does not match plan", ErrInvalidFetchRequest)
	}
	return expected, nil
}

func readBoundedBody(reader io.Reader, declared, limit int64, limitErr error) ([]byte, error) {
	if declared > limit {
		return nil, fmt.Errorf("%w: declared %d bytes, limit %d", limitErr, declared, limit)
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: body exceeded %d bytes", limitErr, limit)
	}
	return body, nil
}

// FetchExact downloads exactly the tarball URL and SRI pinned by PreviewPlan.
// Calls targeting one artifact directory are serialized even across Fetcher
// instances in this process, so a retry observes a completed content-addressed
// publish rather than racing a second rename.
func (f *Fetcher) FetchExact(ctx context.Context, plan PreviewPlan, fetchedBy string,
	progress ProgressFunc,
) (Sidecar, bool, error) {
	if f == nil || f.client == nil || f.serial == nil || ctx == nil {
		return Sidecar{}, false, fmt.Errorf("%w: fetcher or context is incomplete", ErrInvalidFetchRequest)
	}
	if plan.SourceKind == ArtifactSourceNode {
		if f.nodeRuntime == nil {
			return Sidecar{}, false, fmt.Errorf("%w: Node runtime fetcher is unavailable", ErrInvalidFetchRequest)
		}
		nodePlan, err := decodeNodeRuntimeSourcePlan(plan.SourcePlan)
		if err != nil || nodePlan.PolicyVersion != plan.PolicyVersion || nodePlan.Name != plan.Name ||
			nodePlan.Version != plan.Version || nodePlan.SourceOrigin != plan.RegistryOrigin ||
			nodePlan.ChecksumURL != plan.TarballURL || nodePlan.SourceIdentity != plan.SHA512Integrity ||
			nodePlan.BundleMaxBytes != plan.MaxBytes || nodePlan.PreviewDigest != plan.PreviewDigest ||
			plan.EnginesNode != "" {
			return Sidecar{}, false, fmt.Errorf("%w: Node runtime source plan does not match preview", ErrInvalidFetchRequest)
		}
		return f.nodeRuntime.FetchExact(ctx, nodePlan, fetchedBy, progress)
	}
	if plan.SourceKind == ArtifactSourceHermesImage {
		if f.hermesImage == nil {
			return Sidecar{}, false, fmt.Errorf("%w: Hermes image fetcher is unavailable", ErrInvalidFetchRequest)
		}
		hermesPlan, err := decodeHermesImageSourcePlan(plan.SourcePlan)
		if err != nil || hermesPlan.PolicyVersion != plan.PolicyVersion || hermesPlan.Name != plan.Name ||
			hermesPlan.Version != plan.Version || hermesPlan.RegistryOrigin != plan.RegistryOrigin ||
			f.hermesImage.registryURL("manifests", hermesPlan.IndexDigest) != plan.TarballURL ||
			hermesPlan.SourceIdentity != plan.SHA512Integrity || hermesPlan.BundleMaxBytes != plan.MaxBytes ||
			hermesPlan.PreviewDigest != plan.PreviewDigest || plan.EnginesNode != "" {
			return Sidecar{}, false, fmt.Errorf("%w: Hermes image source plan does not match preview", ErrInvalidFetchRequest)
		}
		return f.hermesImage.FetchExact(ctx, hermesPlan, fetchedBy, progress)
	}
	expectedSHA512, err := f.validatePlan(plan)
	if err != nil {
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

	downloadCtx, cancel := context.WithTimeout(ctx, f.downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, plan.TarballURL, nil)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: build tarball request: %v", ErrRegistryPolicy, err)
	}
	req.Header.Set("Accept", "application/octet-stream, application/gzip")
	resp, err := f.client.Do(req)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("artifact: download tarball: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return Sidecar{}, false, fmt.Errorf("artifact: tarball returned HTTP %d", resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return Sidecar{}, false, fmt.Errorf("%w: tarball Content-Encoding is not identity", ErrMetadataInvalid)
	}
	if resp.ContentLength > plan.MaxBytes {
		return Sidecar{}, false, fmt.Errorf("%w: declared %d bytes, limit %d", ErrArtifactTooLarge, resp.ContentLength, plan.MaxBytes)
	}

	tmp, err := os.CreateTemp(f.artifactsDir, artifactFetchTempPrefix+"*.tmp")
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: create tarball temp: %v", ErrArtifactStorage, err)
	}
	tmpPath := tmp.Name()
	tmpOpen := true
	defer func() {
		if tmpOpen {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: chmod tarball temp: %v", ErrArtifactStorage, err)
	}

	var expectedBytes *int64
	if resp.ContentLength >= 0 {
		value := resp.ContentLength
		expectedBytes = &value
	}
	if err := reportFetchProgress(progress, FetchPhaseDownloading, 0, expectedBytes); err != nil {
		return Sidecar{}, false, err
	}
	reader := &fetchProgressReader{
		reader: io.LimitReader(resp.Body, plan.MaxBytes+1), expected: expectedBytes, callback: progress,
	}
	sha512Hash, sha256Hash := sha512.New(), sha256.New()
	written, copyErr := io.CopyBuffer(io.MultiWriter(tmp, sha512Hash, sha256Hash), reader, make([]byte, 128<<10))
	if copyErr != nil {
		return Sidecar{}, false, fmt.Errorf("artifact: download tarball: %w", copyErr)
	}
	if written > plan.MaxBytes {
		return Sidecar{}, false, fmt.Errorf("%w: received more than %d bytes", ErrArtifactTooLarge, plan.MaxBytes)
	}
	if written == 0 {
		return Sidecar{}, false, fmt.Errorf("%w: tarball is empty", ErrMetadataInvalid)
	}
	if err := reportFetchProgress(progress, FetchPhaseVerifying, written, expectedBytes); err != nil {
		return Sidecar{}, false, err
	}
	if subtle.ConstantTimeCompare(sha512Hash.Sum(nil), expectedSHA512) != 1 {
		return Sidecar{}, false, ErrIntegrityMismatch
	}
	if err := tmp.Sync(); err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: fsync tarball temp: %v", ErrArtifactStorage, err)
	}
	if err := tmp.Close(); err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: close tarball temp: %v", ErrArtifactStorage, err)
	}
	tmpOpen = false

	sha256Hex := hex.EncodeToString(sha256Hash.Sum(nil))
	record := Sidecar{
		Name: plan.Name, Version: plan.Version, TarballURL: plan.TarballURL,
		SHA512Integrity: plan.SHA512Integrity, SHA256: sha256Hex, Size: written,
		EnginesNode: plan.EnginesNode, FetchedAt: f.now().UTC(), FetchedBy: fetchedBy,
	}
	if record.FetchedAt.IsZero() {
		return Sidecar{}, false, fmt.Errorf("%w: clock returned zero fetch time", ErrInvalidFetchRequest)
	}
	if err := f.checkExistingSidecarBinding(record); err != nil {
		return Sidecar{}, false, err
	}
	sidecarBytes, err := marshalCompactNoEscape(record)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: encode sidecar: %v", ErrArtifactStorage, err)
	}
	sidecarBytes = append(sidecarBytes, '\n')
	sidecarTemp, err := writeSyncedTemp(f.artifactsDir, artifactSidecarTempPrefix+"*.tmp", sidecarBytes)
	if err != nil {
		return Sidecar{}, false, err
	}
	defer os.Remove(sidecarTemp)
	if err := reportFetchProgress(progress, FetchPhasePublishing, written, expectedBytes); err != nil {
		return Sidecar{}, false, err
	}

	tarballDestination := filepath.Join(f.artifactsDir, sha256Hex+".tgz")
	if err := publishTarball(downloadCtx, tmpPath, tarballDestination, record); err != nil {
		return Sidecar{}, false, err
	}
	if err := syncDirectory(f.artifactsDir); err != nil {
		return Sidecar{}, false, err
	}
	if err := reportFetchProgress(progress, FetchPhasePublishing, written, expectedBytes); err != nil {
		return Sidecar{}, false, err
	}
	sidecarDestination := filepath.Join(f.artifactsDir, sha256Hex+".json")
	if err := os.Rename(sidecarTemp, sidecarDestination); err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: publish sidecar: %v", ErrArtifactStorage, err)
	}
	if err := syncDirectory(f.artifactsDir); err != nil {
		return Sidecar{}, false, err
	}
	return record, false, nil
}

type fetchProgressReader struct {
	reader   io.Reader
	expected *int64
	callback ProgressFunc
	read     int64
	reported int64
}

func (r *fetchProgressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.read += int64(n)
		if r.callback != nil && r.read-r.reported >= fetchProgressCheckpointBytes {
			if callbackErr := r.callback(FetchProgress{
				Phase: FetchPhaseDownloading, DownloadedBytes: r.read, ExpectedBytes: r.expected,
			}); callbackErr != nil {
				return n, callbackErr
			}
			r.reported = r.read
		}
	}
	return n, err
}

func reportFetchProgress(callback ProgressFunc, phase FetchPhase, downloaded int64, expected *int64) error {
	if callback == nil {
		return nil
	}
	if err := callback(FetchProgress{Phase: phase, DownloadedBytes: downloaded, ExpectedBytes: expected}); err != nil {
		return fmt.Errorf("artifact: report %s progress: %w", phase, err)
	}
	return nil
}

func (f *Fetcher) findCachedExact(ctx context.Context, plan PreviewPlan) (Sidecar, bool, error) {
	entries, err := ScanCatalog(f.artifactsDir)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: read artifact catalog: %v", ErrArtifactStorage, err)
	}
	for _, entry := range entries {
		if entry.Record == nil ||
			(entry.Status != CatalogAvailableUnverified && entry.Status != CatalogReady) {
			continue
		}
		record := *entry.Record
		if record.Name != plan.Name || record.Version != plan.Version || record.TarballURL != plan.TarballURL ||
			record.SHA512Integrity != plan.SHA512Integrity || record.EnginesNode != plan.EnginesNode ||
			record.Size <= 0 || record.Size > plan.MaxBytes || record.FetchedAt.IsZero() ||
			!validBoundedText(record.FetchedBy, 256, false) {
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

func (f *Fetcher) checkExistingSidecarBinding(record Sidecar) error {
	return checkExistingSidecarBinding(f.artifactsDir, record)
}

func readSidecarFile(path string) (Sidecar, bool, error) {
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Sidecar{}, false, nil
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxSidecarBytes {
		return Sidecar{}, false, fmt.Errorf("%w: existing sidecar is unavailable, non-regular, or oversized", ErrArtifactStorage)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: open existing sidecar: %v", ErrArtifactStorage, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return Sidecar{}, false, fmt.Errorf("%w: open existing sidecar", ErrArtifactStorage)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return Sidecar{}, false, fmt.Errorf("%w: existing sidecar changed while opening", ErrArtifactStorage)
	}
	body, err := io.ReadAll(io.LimitReader(f, maxSidecarBytes+1))
	if err != nil || int64(len(body)) > maxSidecarBytes {
		return Sidecar{}, false, fmt.Errorf("%w: read existing sidecar", ErrArtifactStorage)
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return Sidecar{}, false, fmt.Errorf("%w: existing sidecar changed while reading", ErrArtifactStorage)
	}
	var record Sidecar
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&record); err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: decode existing sidecar", ErrArtifactStorage)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Sidecar{}, false, fmt.Errorf("%w: existing sidecar has trailing JSON", ErrArtifactStorage)
	}
	return record, true, nil
}

func writeSyncedTemp(dir, pattern string, body []byte) (path string, err error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("%w: create sidecar temp: %v", ErrArtifactStorage, err)
	}
	path = f.Name()
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("%w: chmod sidecar temp: %v", ErrArtifactStorage, err)
	}
	if _, err := f.Write(body); err != nil {
		return "", fmt.Errorf("%w: write sidecar temp: %v", ErrArtifactStorage, err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("%w: fsync sidecar temp: %v", ErrArtifactStorage, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("%w: close sidecar temp: %v", ErrArtifactStorage, err)
	}
	keep = true
	return path, nil
}

func publishTarball(ctx context.Context, tempPath, destination string, record Sidecar) error {
	entry, err := os.Lstat(destination)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("%w: inspect tarball destination: %v", ErrArtifactStorage, err)
	case !entry.Mode().IsRegular():
		return fmt.Errorf("%w: tarball destination is not a regular file", ErrArtifactStorage)
	default:
		if err := ValidateStoredArtifactContext(ctx, filepath.Dir(destination), record); err == nil {
			return nil
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
	}
	if err := os.Rename(tempPath, destination); err != nil {
		return fmt.Errorf("%w: publish tarball: %v", ErrArtifactStorage, err)
	}
	return nil
}

func ensurePrivateArtifactDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: create artifact directory: %v", ErrArtifactStorage, err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: artifact directory is not a real directory", ErrArtifactStorage)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("%w: tighten artifact directory permissions: %v", ErrArtifactStorage, err)
	}
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("%w: open artifact directory for fsync: %v", ErrArtifactStorage, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("%w: fsync artifact directory: %v", ErrArtifactStorage, err)
	}
	return nil
}

// ReconcileStaleTemps removes only known fetch temp files whose lstat mtime is
// at or before the supplied cutoff. It never follows symlinks and refuses to
// recursively remove a directory, even if its name resembles a temp file.
func (f *Fetcher) ReconcileStaleTemps(before time.Time) (int, error) {
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
		name := entry.Name()
		if !isArtifactTempName(name) {
			continue
		}
		path := filepath.Join(f.artifactsDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return removed, fmt.Errorf("%w: inspect temp file: %v", ErrArtifactStorage, err)
		}
		if info.IsDir() {
			return removed, fmt.Errorf("%w: refusing to remove temp-named directory", ErrArtifactStorage)
		}
		if info.ModTime().After(before) {
			continue
		}
		if err := os.Remove(path); err != nil {
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

func isArtifactTempName(name string) bool {
	if !strings.HasSuffix(name, ".tmp") {
		return false
	}
	for _, prefix := range []string{artifactFetchTempPrefix, artifactSidecarTempPrefix, nodeRuntimeSourceTempPrefix, ".artifact-", ".sidecar-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
