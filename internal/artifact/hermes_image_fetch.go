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
	ProductionHermesRegistryOrigin = "https://registry-1.docker.io"
	ProductionHermesTokenOrigin    = "https://auth.docker.io"
	HermesImageRepository          = "nousresearch/hermes-agent"
	HermesImageFetchPolicyVersion  = "hermes-official-oci-bundle:v1"
	ArtifactSourceHermesImage      = "oci-image-bundle:v1"

	DefaultHermesImageBlobMaxBytes   = int64(2 << 30)
	DefaultHermesImageBundleMaxBytes = int64(3 << 30)
	hermesImageManifestMaxBytes      = int64(4 << 20)
	hermesImageTokenMaxBytes         = int64(64 << 10)
	hermesImageMaxLayers             = 512
	hermesImageRefName               = "docker.io/nousresearch/hermes-agent"
	hermesImageMediaIndex            = "application/vnd.oci.image.index.v1+json"
	hermesImageMediaManifest         = "application/vnd.oci.image.manifest.v1+json"
	hermesImageMediaDockerIndex      = "application/vnd.docker.distribution.manifest.list.v2+json"
	hermesImageMediaDockerManifest   = "application/vnd.docker.distribution.manifest.v2+json"
)

type HermesImagePlatformManifest struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

// HermesImageFetchPlan records only immutable OCI identities. Registry bearer
// tokens and redirect URLs remain process-local and never cross the Store.
type HermesImageFetchPlan struct {
	PolicyVersion  string                        `json:"policy_version"`
	Name           string                        `json:"name"`
	Version        string                        `json:"version"`
	RegistryOrigin string                        `json:"registry_origin"`
	Repository     string                        `json:"repository"`
	Tag            string                        `json:"tag"`
	IndexDigest    string                        `json:"index_digest"`
	IndexMediaType string                        `json:"index_media_type"`
	Manifests      []HermesImagePlatformManifest `json:"manifests"`
	SourceIdentity string                        `json:"source_identity"`
	BlobMaxBytes   int64                         `json:"blob_max_bytes"`
	BundleMaxBytes int64                         `json:"bundle_max_bytes"`
	PreviewedAt    time.Time                     `json:"previewed_at"`
	PreviewDigest  string                        `json:"preview_digest"`
}

type HermesImageFetcher struct {
	artifactsDir    string
	registry        *url.URL
	registryOrigin  string
	tokenOrigin     string
	repository      string
	client          *http.Client
	manifestMax     int64
	blobMax         int64
	bundleMax       int64
	metadataTimeout time.Duration
	downloadTimeout time.Duration
	allowHTTP       bool
	now             func() time.Time
	serial          *sync.Mutex
	tokenMu         sync.Mutex
	token           string
}

type hermesImageFetcherConfig struct {
	artifactsDir    string
	registryURL     string
	tokenOriginURL  string
	repository      string
	client          *http.Client
	manifestMax     int64
	blobMax         int64
	bundleMax       int64
	metadataTimeout time.Duration
	downloadTimeout time.Duration
	allowHTTP       bool
	now             func() time.Time
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *ociPlatform      `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ociPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

type hermesImageBlob struct {
	descriptor ociDescriptor
	body       []byte
}

func NewHermesImageFetcher(artifactsDir string) (*HermesImageFetcher, error) {
	return newHermesImageFetcher(hermesImageFetcherConfig{
		artifactsDir: artifactsDir, registryURL: ProductionHermesRegistryOrigin,
		tokenOriginURL: ProductionHermesTokenOrigin, repository: HermesImageRepository,
		manifestMax: hermesImageManifestMaxBytes, blobMax: DefaultHermesImageBlobMaxBytes,
		bundleMax: DefaultHermesImageBundleMaxBytes, metadataTimeout: productionMetadataTimeout,
		downloadTimeout: 2 * time.Hour, now: time.Now,
	})
}

func newHermesImageFetcher(config hermesImageFetcherConfig) (*HermesImageFetcher, error) {
	dir := filepath.Clean(config.artifactsDir)
	if config.artifactsDir == "" || !filepath.IsAbs(config.artifactsDir) || dir != config.artifactsDir || filepath.Dir(dir) == dir {
		return nil, fmt.Errorf("%w: artifacts directory must be a canonical absolute non-root path", ErrInvalidFetchRequest)
	}
	registry, registryOrigin, err := parseRegistryURL(config.registryURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	_, tokenOrigin, err := parseRegistryURL(config.tokenOriginURL, config.allowHTTP)
	if err != nil {
		return nil, err
	}
	if !config.allowHTTP && (registryOrigin != ProductionHermesRegistryOrigin || tokenOrigin != ProductionHermesTokenOrigin ||
		config.repository != HermesImageRepository) {
		return nil, fmt.Errorf("%w: Hermes image source must be the official registry repository", ErrRegistryPolicy)
	}
	if !validOCIRepository(config.repository) {
		return nil, fmt.Errorf("%w: Hermes image repository is invalid", ErrRegistryPolicy)
	}
	if config.manifestMax <= 0 {
		config.manifestMax = hermesImageManifestMaxBytes
	}
	if config.blobMax <= 0 {
		config.blobMax = DefaultHermesImageBlobMaxBytes
	}
	if config.bundleMax <= 0 {
		config.bundleMax = DefaultHermesImageBundleMaxBytes
	}
	if config.metadataTimeout <= 0 {
		config.metadataTimeout = productionMetadataTimeout
	}
	if config.downloadTimeout <= 0 {
		config.downloadTimeout = 2 * time.Hour
	}
	if config.now == nil {
		config.now = time.Now
	}
	client, err := hardenedFetchHTTPClient(config.client, config.metadataTimeout, config.downloadTimeout)
	if err != nil {
		return nil, err
	}
	lock, _ := fetchDirectoryLocks.LoadOrStore(dir, &sync.Mutex{})
	return &HermesImageFetcher{
		artifactsDir: dir, registry: registry, registryOrigin: registryOrigin, tokenOrigin: tokenOrigin,
		repository: config.repository, client: client, manifestMax: config.manifestMax,
		blobMax: config.blobMax, bundleMax: config.bundleMax, metadataTimeout: config.metadataTimeout,
		downloadTimeout: config.downloadTimeout, allowHTTP: config.allowHTTP, now: config.now,
		serial: lock.(*sync.Mutex),
	}, nil
}

func validOCIRepository(value string) bool {
	if value == "" || len(value) > 256 || value != strings.ToLower(value) || value != strings.Trim(value, "/") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || len(component) > 128 {
			return false
		}
		for _, r := range component {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
				return false
			}
		}
	}
	return true
}

func ValidHermesVersion(version string) bool { return validExactNPMVersion(version) }

func (f *HermesImageFetcher) PreviewPlan(ctx context.Context, version string) (HermesImageFetchPlan, error) {
	if f == nil || f.client == nil || f.registry == nil || ctx == nil || !ValidHermesVersion(version) {
		return HermesImageFetchPlan{}, fmt.Errorf("%w: exact Hermes version and fetcher are required", ErrInvalidFetchRequest)
	}
	tag := "v" + version
	requestCtx, cancel := context.WithTimeout(ctx, f.metadataTimeout)
	defer cancel()
	body, mediaType, digest, err := f.getManifest(requestCtx, tag)
	if err != nil {
		return HermesImageFetchPlan{}, err
	}
	if mediaType != hermesImageMediaIndex && mediaType != hermesImageMediaDockerIndex {
		return HermesImageFetchPlan{}, fmt.Errorf("%w: Hermes tag is not a multi-platform OCI index", ErrMetadataInvalid)
	}
	var index ociIndex
	if err := decodeOCIJSON(body, &index); err != nil || index.SchemaVersion != 2 || index.MediaType != mediaType {
		return HermesImageFetchPlan{}, fmt.Errorf("%w: Hermes OCI index is invalid", ErrMetadataInvalid)
	}
	manifests, err := selectHermesPlatformManifests(index.Manifests)
	if err != nil {
		return HermesImageFetchPlan{}, err
	}
	now := f.now().UTC()
	if now.IsZero() {
		return HermesImageFetchPlan{}, fmt.Errorf("%w: clock returned zero preview time", ErrInvalidFetchRequest)
	}
	plan := HermesImageFetchPlan{
		PolicyVersion: HermesImageFetchPolicyVersion, Name: "hermes-agent", Version: version,
		RegistryOrigin: f.registryOrigin, Repository: f.repository, Tag: tag,
		IndexDigest: digest, IndexMediaType: mediaType, Manifests: manifests,
		BlobMaxBytes: f.blobMax, BundleMaxBytes: f.bundleMax, PreviewedAt: now,
	}
	plan.SourceIdentity = hermesImageSourceIdentity(plan)
	plan.PreviewDigest = hermesImagePreviewDigest(plan)
	return plan, nil
}

func selectHermesPlatformManifests(descriptors []ociDescriptor) ([]HermesImagePlatformManifest, error) {
	wanted := map[string]bool{"amd64": true, "arm64": true}
	result := make([]HermesImagePlatformManifest, 0, 2)
	seen := make(map[string]bool, 2)
	for _, descriptor := range descriptors {
		if descriptor.Platform == nil || descriptor.Platform.OS != "linux" || !wanted[descriptor.Platform.Architecture] {
			continue
		}
		if seen[descriptor.Platform.Architecture] || descriptor.Platform.Variant != "" ||
			(descriptor.MediaType != hermesImageMediaManifest && descriptor.MediaType != hermesImageMediaDockerManifest) ||
			!validOCIDigest(descriptor.Digest) || descriptor.Size <= 0 || descriptor.Size > hermesImageManifestMaxBytes {
			return nil, fmt.Errorf("%w: Hermes OCI platform manifest is invalid or duplicated", ErrMetadataInvalid)
		}
		seen[descriptor.Platform.Architecture] = true
		result = append(result, HermesImagePlatformManifest{
			OS: "linux", Arch: descriptor.Platform.Architecture, Digest: descriptor.Digest,
			SizeBytes: descriptor.Size, MediaType: descriptor.MediaType,
		})
	}
	if len(result) != 2 {
		return nil, fmt.Errorf("%w: Hermes OCI index does not contain both Linux targets", ErrMetadataInvalid)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Arch < result[j].Arch })
	return result, nil
}

func validOCIDigest(value string) bool {
	raw, ok := strings.CutPrefix(value, "sha256:")
	return ok && ValidSHA256Hex(raw)
}

func decodeOCIJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("OCI JSON contains trailing data")
	}
	return nil
}

func hermesImageSourceIdentity(plan HermesImageFetchPlan) string {
	body := struct {
		PolicyVersion  string                        `json:"policy_version"`
		Name           string                        `json:"name"`
		Version        string                        `json:"version"`
		RegistryOrigin string                        `json:"registry_origin"`
		Repository     string                        `json:"repository"`
		Tag            string                        `json:"tag"`
		IndexDigest    string                        `json:"index_digest"`
		IndexMediaType string                        `json:"index_media_type"`
		Manifests      []HermesImagePlatformManifest `json:"manifests"`
		BlobMaxBytes   int64                         `json:"blob_max_bytes"`
		BundleMaxBytes int64                         `json:"bundle_max_bytes"`
	}{plan.PolicyVersion, plan.Name, plan.Version, plan.RegistryOrigin, plan.Repository, plan.Tag,
		plan.IndexDigest, plan.IndexMediaType, plan.Manifests, plan.BlobMaxBytes, plan.BundleMaxBytes}
	raw, _ := json.Marshal(body)
	sum := sha512.Sum512(raw)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func hermesImagePreviewDigest(plan HermesImageFetchPlan) string {
	body := struct {
		PolicyVersion  string                        `json:"policy_version"`
		Name           string                        `json:"name"`
		Version        string                        `json:"version"`
		RegistryOrigin string                        `json:"registry_origin"`
		Repository     string                        `json:"repository"`
		Tag            string                        `json:"tag"`
		IndexDigest    string                        `json:"index_digest"`
		IndexMediaType string                        `json:"index_media_type"`
		Manifests      []HermesImagePlatformManifest `json:"manifests"`
		SourceIdentity string                        `json:"source_identity"`
		BlobMaxBytes   int64                         `json:"blob_max_bytes"`
		BundleMaxBytes int64                         `json:"bundle_max_bytes"`
	}{plan.PolicyVersion, plan.Name, plan.Version, plan.RegistryOrigin, plan.Repository, plan.Tag,
		plan.IndexDigest, plan.IndexMediaType, plan.Manifests, plan.SourceIdentity, plan.BlobMaxBytes, plan.BundleMaxBytes}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeHermesImageSourcePlan(raw string) (HermesImageFetchPlan, error) {
	var plan HermesImageFetchPlan
	if raw == "" || len(raw) > MaxArtifactSourcePlanBytes {
		return plan, fmt.Errorf("%w: Hermes image source plan is missing or oversized", ErrInvalidFetchRequest)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("%w: decode Hermes image source plan", ErrInvalidFetchRequest)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return plan, fmt.Errorf("%w: Hermes image source plan has trailing data", ErrInvalidFetchRequest)
	}
	canonical, err := marshalCompactNoEscape(plan)
	if err != nil || string(canonical) != raw {
		return plan, fmt.Errorf("%w: Hermes image source plan is not canonical", ErrInvalidFetchRequest)
	}
	return plan, nil
}

// ValidHermesImageSourcePlan reports whether raw is the canonical private OCI
// plan representation accepted across the durable Store boundary.
func ValidHermesImageSourcePlan(raw string) bool {
	_, err := decodeHermesImageSourcePlan(raw)
	return err == nil
}

func (f *HermesImageFetcher) validatePlan(plan HermesImageFetchPlan) error {
	if plan.PolicyVersion != HermesImageFetchPolicyVersion || plan.Name != "hermes-agent" ||
		!ValidHermesVersion(plan.Version) || plan.RegistryOrigin != f.registryOrigin ||
		plan.Repository != f.repository || plan.Tag != "v"+plan.Version ||
		(plan.IndexMediaType != hermesImageMediaIndex && plan.IndexMediaType != hermesImageMediaDockerIndex) ||
		!validOCIDigest(plan.IndexDigest) || plan.BlobMaxBytes != f.blobMax || plan.BundleMaxBytes != f.bundleMax ||
		plan.PreviewedAt.IsZero() || plan.PreviewedAt.Location() != time.UTC || len(plan.Manifests) != 2 {
		return fmt.Errorf("%w: Hermes image plan does not match active policy", ErrInvalidFetchRequest)
	}
	wantArch := []string{"amd64", "arm64"}
	for i, manifest := range plan.Manifests {
		if manifest.OS != "linux" || manifest.Arch != wantArch[i] || !validOCIDigest(manifest.Digest) ||
			manifest.SizeBytes <= 0 || manifest.SizeBytes > f.manifestMax ||
			(manifest.MediaType != hermesImageMediaManifest && manifest.MediaType != hermesImageMediaDockerManifest) {
			return fmt.Errorf("%w: Hermes image platform plan is invalid", ErrInvalidFetchRequest)
		}
	}
	if plan.SourceIdentity != hermesImageSourceIdentity(plan) || plan.PreviewDigest != hermesImagePreviewDigest(plan) {
		return fmt.Errorf("%w: Hermes image plan digest does not match", ErrInvalidFetchRequest)
	}
	return nil
}

func (f *HermesImageFetcher) registryURL(kind, identity string) string {
	return f.registryOrigin + "/v2/" + f.repository + "/" + kind + "/" + url.PathEscape(identity)
}

func (f *HermesImageFetcher) getManifest(ctx context.Context, identity string) ([]byte, string, string, error) {
	accept := strings.Join([]string{hermesImageMediaIndex, hermesImageMediaDockerIndex,
		hermesImageMediaManifest, hermesImageMediaDockerManifest}, ", ")
	resp, err := f.registryGet(ctx, f.registryURL("manifests", identity), accept)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, "", "", fmt.Errorf("%w: OCI manifest returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil, "", "", fmt.Errorf("%w: OCI manifest Content-Encoding is not identity", ErrMetadataInvalid)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", "", fmt.Errorf("%w: OCI manifest Content-Type is invalid", ErrMetadataInvalid)
	}
	body, err := readBoundedBody(resp.Body, resp.ContentLength, f.manifestMax, ErrMetadataTooLarge)
	if err != nil {
		return nil, "", "", err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if header := strings.TrimSpace(resp.Header.Get("Docker-Content-Digest")); header != "" && header != digest {
		return nil, "", "", ErrIntegrityMismatch
	}
	if validOCIDigest(identity) && identity != digest {
		return nil, "", "", ErrIntegrityMismatch
	}
	return body, mediaType, digest, nil
}

func (f *HermesImageFetcher) registryGet(ctx context.Context, rawURL, accept string) (*http.Response, error) {
	request := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: build OCI request", ErrRegistryPolicy)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return f.client.Do(req)
	}
	f.tokenMu.Lock()
	token := f.token
	f.tokenMu.Unlock()
	resp, err := request(token)
	if err != nil {
		return nil, fmt.Errorf("artifact: read OCI registry: %w", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	token, err = f.exchangeRegistryToken(ctx, challenge)
	if err != nil {
		return nil, err
	}
	f.tokenMu.Lock()
	f.token = token
	f.tokenMu.Unlock()
	return request(token)
}

func (f *HermesImageFetcher) exchangeRegistryToken(ctx context.Context, challenge string) (string, error) {
	parameters, err := parseBearerChallenge(challenge)
	if err != nil {
		return "", err
	}
	realm, err := url.Parse(parameters["realm"])
	if err != nil || realm == nil || !realm.IsAbs() || realm.Opaque != "" || realm.User != nil ||
		realm.RawQuery != "" || realm.ForceQuery || realm.Fragment != "" || realm.RawFragment != "" || realm.Path != "/token" {
		return "", fmt.Errorf("%w: OCI bearer realm is invalid", ErrRegistryPolicy)
	}
	origin, err := canonicalOrigin(realm)
	if err != nil || origin != f.tokenOrigin || parameters["service"] != "registry.docker.io" ||
		parameters["scope"] != "repository:"+f.repository+":pull" {
		return "", fmt.Errorf("%w: OCI bearer challenge is outside policy", ErrRegistryPolicy)
	}
	query := realm.Query()
	query.Set("service", parameters["service"])
	query.Set("scope", parameters["scope"])
	realm.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", fmt.Errorf("%w: build OCI token request", ErrRegistryPolicy)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("artifact: read OCI token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("%w: OCI token endpoint returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	body, err := readBoundedBody(resp.Body, resp.ContentLength, hermesImageTokenMaxBytes, ErrMetadataTooLarge)
	if err != nil {
		return "", err
	}
	var document struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := decodeOCIJSON(body, &document); err != nil {
		return "", fmt.Errorf("%w: OCI token response is invalid", ErrMetadataInvalid)
	}
	token := document.Token
	if token == "" {
		token = document.AccessToken
	}
	if !validBoundedText(token, 16<<10, false) || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("%w: OCI bearer token is invalid", ErrMetadataInvalid)
	}
	return token, nil
}

func parseBearerChallenge(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	if len(value) < len("Bearer ") || !strings.EqualFold(value[:len("Bearer")], "Bearer") || value[len("Bearer")] != ' ' {
		return nil, fmt.Errorf("%w: OCI registry did not provide a bearer challenge", ErrRegistryPolicy)
	}
	input := strings.TrimSpace(value[len("Bearer "):])
	result := make(map[string]string, 3)
	for input != "" {
		equals := strings.IndexByte(input, '=')
		if equals <= 0 {
			return nil, fmt.Errorf("%w: OCI bearer challenge is malformed", ErrRegistryPolicy)
		}
		key := strings.TrimSpace(input[:equals])
		input = input[equals+1:]
		if input == "" || input[0] != '"' || key == "" || result[key] != "" {
			return nil, fmt.Errorf("%w: OCI bearer challenge is malformed", ErrRegistryPolicy)
		}
		input = input[1:]
		end := strings.IndexByte(input, '"')
		if end < 0 || strings.Contains(input[:end], "\\") {
			return nil, fmt.Errorf("%w: OCI bearer challenge is malformed", ErrRegistryPolicy)
		}
		result[key] = input[:end]
		input = strings.TrimSpace(input[end+1:])
		if input != "" {
			if input[0] != ',' {
				return nil, fmt.Errorf("%w: OCI bearer challenge is malformed", ErrRegistryPolicy)
			}
			input = strings.TrimSpace(input[1:])
		}
	}
	for _, key := range []string{"realm", "service", "scope"} {
		if result[key] == "" {
			return nil, fmt.Errorf("%w: OCI bearer challenge lacks %s", ErrRegistryPolicy, key)
		}
	}
	return result, nil
}

func (f *HermesImageFetcher) FetchExact(ctx context.Context, plan HermesImageFetchPlan, fetchedBy string,
	progress ProgressFunc,
) (Sidecar, bool, error) {
	if f == nil || f.client == nil || f.serial == nil || ctx == nil {
		return Sidecar{}, false, fmt.Errorf("%w: Hermes image fetcher or context is incomplete", ErrInvalidFetchRequest)
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
	downloadCtx, cancel := context.WithTimeout(ctx, f.downloadTimeout)
	defer cancel()
	indexBody, indexMedia, indexDigest, err := f.getManifest(downloadCtx, plan.IndexDigest)
	if err != nil {
		return Sidecar{}, false, err
	}
	if indexMedia != plan.IndexMediaType || indexDigest != plan.IndexDigest {
		return Sidecar{}, false, fmt.Errorf("%w: Hermes OCI index changed after preview", ErrIntegrityMismatch)
	}
	var upstreamIndex ociIndex
	if decodeErr := decodeOCIJSON(indexBody, &upstreamIndex); decodeErr != nil {
		return Sidecar{}, false, fmt.Errorf("%w: Hermes OCI index is invalid", ErrMetadataInvalid)
	}
	selected, err := selectHermesPlatformManifests(upstreamIndex.Manifests)
	if err != nil || !sameHermesManifests(selected, plan.Manifests) {
		return Sidecar{}, false, fmt.Errorf("%w: Hermes OCI platform set changed after preview", ErrIntegrityMismatch)
	}
	manifestBlobs := make([]hermesImageBlob, 0, len(plan.Manifests))
	descriptors := make(map[string]ociDescriptor)
	for _, platform := range plan.Manifests {
		body, mediaType, digest, fetchErr := f.getManifest(downloadCtx, platform.Digest)
		if fetchErr != nil {
			return Sidecar{}, false, fetchErr
		}
		if mediaType != platform.MediaType || digest != platform.Digest || int64(len(body)) != platform.SizeBytes {
			return Sidecar{}, false, fmt.Errorf("%w: Hermes OCI platform manifest changed after preview", ErrIntegrityMismatch)
		}
		var manifest ociManifest
		if err := decodeOCIJSON(body, &manifest); err != nil || manifest.SchemaVersion != 2 || manifest.MediaType != mediaType ||
			!validOCIContentDescriptor(manifest.Config, f.blobMax) || len(manifest.Layers) == 0 || len(manifest.Layers) > hermesImageMaxLayers {
			return Sidecar{}, false, fmt.Errorf("%w: Hermes OCI image manifest is invalid", ErrMetadataInvalid)
		}
		if current, exists := descriptors[manifest.Config.Digest]; exists &&
			(current.Size != manifest.Config.Size || current.MediaType != manifest.Config.MediaType) {
			return Sidecar{}, false, fmt.Errorf("%w: Hermes OCI digest has conflicting descriptors", ErrMetadataInvalid)
		}
		descriptors[manifest.Config.Digest] = manifest.Config
		for _, layer := range manifest.Layers {
			if !validOCIContentDescriptor(layer, f.blobMax) {
				return Sidecar{}, false, fmt.Errorf("%w: Hermes OCI layer descriptor is invalid", ErrMetadataInvalid)
			}
			if current, exists := descriptors[layer.Digest]; exists && (current.Size != layer.Size || current.MediaType != layer.MediaType) {
				return Sidecar{}, false, fmt.Errorf("%w: Hermes OCI digest has conflicting descriptors", ErrMetadataInvalid)
			}
			descriptors[layer.Digest] = layer
		}
		manifestBlobs = append(manifestBlobs, hermesImageBlob{descriptor: ociDescriptor{
			MediaType: mediaType, Digest: digest, Size: int64(len(body)),
			Platform: &ociPlatform{OS: platform.OS, Architecture: platform.Arch},
		}, body: body})
	}
	expected := int64(len(indexBody))
	for _, blob := range manifestBlobs {
		expected += int64(len(blob.body))
	}
	for _, descriptor := range descriptors {
		if expected > f.bundleMax-descriptor.Size {
			return Sidecar{}, false, ErrArtifactTooLarge
		}
		expected += descriptor.Size
	}
	metadataBytes := int64(len(indexBody))
	for _, blob := range manifestBlobs {
		metadataBytes += int64(len(blob.body))
	}
	if err := reportFetchProgress(progress, FetchPhaseDownloading, metadataBytes, &expected); err != nil {
		return Sidecar{}, false, err
	}
	tempPath, sha256Hex, size, err := f.buildOCIBundle(downloadCtx, plan, manifestBlobs, descriptors,
		metadataBytes, expected, progress)
	if err != nil {
		return Sidecar{}, false, err
	}
	defer os.Remove(tempPath)
	if err := reportFetchProgress(progress, FetchPhaseVerifying, size, &size); err != nil {
		return Sidecar{}, false, err
	}
	record := Sidecar{
		Name: "hermes-agent", Version: plan.Version,
		TarballURL: f.registryURL("manifests", plan.IndexDigest), SHA512Integrity: plan.SourceIdentity,
		SHA256: sha256Hex, Size: size, EnginesNode: "", FetchedAt: f.now().UTC(), FetchedBy: fetchedBy,
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

func sameHermesManifests(left, right []HermesImagePlatformManifest) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validOCIContentDescriptor(descriptor ociDescriptor, maximum int64) bool {
	return validOCIDigest(descriptor.Digest) && descriptor.Size > 0 && descriptor.Size <= maximum &&
		validBoundedText(descriptor.MediaType, 256, false) && strings.Contains(descriptor.MediaType, "/") &&
		descriptor.Platform == nil
}

func (f *HermesImageFetcher) buildOCIBundle(ctx context.Context, plan HermesImageFetchPlan,
	manifestBlobs []hermesImageBlob, descriptors map[string]ociDescriptor, downloaded, expected int64,
	progress ProgressFunc,
) (string, string, int64, error) {
	temp, err := os.CreateTemp(f.artifactsDir, artifactFetchTempPrefix+"*.tmp")
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Hermes OCI bundle temp: %v", ErrArtifactStorage, err)
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return "", "", 0, fmt.Errorf("%w: chmod Hermes OCI bundle temp: %v", ErrArtifactStorage, err)
	}
	h := sha256.New()
	output := &boundedHashWriter{w: temp, hash: h, maximum: plan.BundleMaxBytes}
	gz, err := gzip.NewWriterLevel(output, gzip.BestSpeed)
	if err != nil {
		return "", "", 0, fmt.Errorf("%w: create Hermes OCI bundle gzip: %v", ErrArtifactStorage, err)
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
	rootIndex, nestedIndex, err := hermesOCIIndexes(plan, manifestBlobs)
	if err != nil {
		return "", "", 0, err
	}
	static := map[string][]byte{
		"oci-layout": []byte("{\"imageLayoutVersion\":\"1.0.0\"}\n"),
		"index.json": rootIndex,
	}
	for _, blob := range manifestBlobs {
		static[ociBlobPath(blob.descriptor.Digest)] = blob.body
	}
	nestedDigest := sha256.Sum256(nestedIndex)
	static["blobs/sha256/"+hex.EncodeToString(nestedDigest[:])] = nestedIndex
	names := make([]string, 0, len(static)+len(descriptors))
	for name := range static {
		names = append(names, name)
	}
	for digest := range descriptors {
		names = append(names, ociBlobPath(digest))
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return "", "", 0, err
		}
		if body, ok := static[name]; ok {
			if err := writeHermesTarBytes(tw, name, body); err != nil {
				return "", "", 0, err
			}
			continue
		}
		digest := "sha256:" + path.Base(name)
		descriptor := descriptors[digest]
		if err := f.writeRegistryBlob(ctx, tw, name, descriptor, &downloaded, expected, progress); err != nil {
			return "", "", 0, err
		}
	}
	if err := closeBundle(); err != nil {
		if errors.Is(err, ErrArtifactTooLarge) {
			return "", "", 0, ErrArtifactTooLarge
		}
		return "", "", 0, fmt.Errorf("%w: close Hermes OCI bundle: %v", ErrArtifactStorage, err)
	}
	if output.written == 0 {
		return "", "", 0, fmt.Errorf("%w: Hermes OCI bundle is empty", ErrMetadataInvalid)
	}
	if err := temp.Sync(); err != nil {
		return "", "", 0, fmt.Errorf("%w: fsync Hermes OCI bundle: %v", ErrArtifactStorage, err)
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, fmt.Errorf("%w: close Hermes OCI bundle: %v", ErrArtifactStorage, err)
	}
	keep = true
	return tempPath, hex.EncodeToString(h.Sum(nil)), output.written, nil
}

func hermesOCIIndexes(plan HermesImageFetchPlan, blobs []hermesImageBlob) ([]byte, []byte, error) {
	manifests := make([]ociDescriptor, len(blobs))
	for i, blob := range blobs {
		manifests[i] = blob.descriptor
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Platform.Architecture < manifests[j].Platform.Architecture })
	nested := ociIndex{SchemaVersion: 2, MediaType: hermesImageMediaIndex, Manifests: manifests}
	nestedRaw, err := marshalCompactNoEscape(nested)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: encode Hermes OCI platform index", ErrArtifactStorage)
	}
	nestedRaw = append(nestedRaw, '\n')
	nestedSum := sha256.Sum256(nestedRaw)
	root := ociIndex{SchemaVersion: 2, MediaType: hermesImageMediaIndex, Manifests: []ociDescriptor{{
		MediaType: hermesImageMediaIndex, Digest: "sha256:" + hex.EncodeToString(nestedSum[:]), Size: int64(len(nestedRaw)),
		Annotations: map[string]string{"org.opencontainers.image.ref.name": hermesImageRefName + ":v" + plan.Version},
	}}}
	rootRaw, err := marshalCompactNoEscape(root)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: encode Hermes OCI root index", ErrArtifactStorage)
	}
	return append(rootRaw, '\n'), nestedRaw, nil
}

func ociBlobPath(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

func writeHermesTarBytes(writer *tar.Writer, name string, body []byte) error {
	if err := writer.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeReg, int64(len(body)), "", false)); err != nil {
		return fmt.Errorf("%w: write Hermes OCI header: %v", ErrArtifactStorage, err)
	}
	if _, err := writer.Write(body); err != nil {
		return fmt.Errorf("%w: write Hermes OCI metadata: %v", ErrArtifactStorage, err)
	}
	return nil
}

func (f *HermesImageFetcher) writeRegistryBlob(ctx context.Context, writer *tar.Writer, name string,
	descriptor ociDescriptor, downloaded *int64, expected int64, progress ProgressFunc,
) error {
	resp, err := f.registryGet(ctx, f.registryURL("blobs", descriptor.Digest), "application/octet-stream")
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusFound {
		location := resp.Header.Get("Location")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		resp, err = f.followDockerBlobRedirect(ctx, location)
		if err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%w: OCI blob returned HTTP %d", ErrMetadataInvalid, resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != descriptor.Size {
		return fmt.Errorf("%w: OCI blob length changed", ErrIntegrityMismatch)
	}
	if err := writer.WriteHeader(nodeRuntimeBundleHeader(name, tar.TypeReg, descriptor.Size, "", false)); err != nil {
		return fmt.Errorf("%w: write Hermes OCI blob header: %v", ErrArtifactStorage, err)
	}
	h := sha256.New()
	reader := io.TeeReader(resp.Body, h)
	written, err := io.CopyN(writer, reader, descriptor.Size)
	if err != nil || written != descriptor.Size {
		return fmt.Errorf("%w: OCI blob body is incomplete", ErrMetadataInvalid)
	}
	var extra [1]byte
	if count, readErr := resp.Body.Read(extra[:]); count != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return fmt.Errorf("%w: OCI blob exceeds declared size", ErrIntegrityMismatch)
	}
	expectedDigest, _ := hex.DecodeString(strings.TrimPrefix(descriptor.Digest, "sha256:"))
	if subtle.ConstantTimeCompare(h.Sum(nil), expectedDigest) != 1 {
		return ErrIntegrityMismatch
	}
	*downloaded += written
	if err := reportFetchProgress(progress, FetchPhaseDownloading, *downloaded, &expected); err != nil {
		return err
	}
	return nil
}

func (f *HermesImageFetcher) followDockerBlobRedirect(ctx context.Context, location string) (*http.Response, error) {
	u, err := url.Parse(location)
	if err != nil || u == nil || !u.IsAbs() || u.Scheme != "https" || u.Opaque != "" || u.User != nil ||
		u.Hostname() != "production.cloudfront.docker.com" || u.Path == "" || u.Fragment != "" || u.RawFragment != "" {
		return nil, fmt.Errorf("%w: OCI blob redirect is outside policy", ErrRegistryPolicy)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build OCI blob redirect request", ErrRegistryPolicy)
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifact: read OCI blob redirect: %w", err)
	}
	return resp, nil
}

func (f *HermesImageFetcher) findCachedExact(ctx context.Context, plan HermesImageFetchPlan) (Sidecar, bool, error) {
	entries, err := ScanCatalog(f.artifactsDir)
	if err != nil {
		return Sidecar{}, false, fmt.Errorf("%w: read artifact catalog: %v", ErrArtifactStorage, err)
	}
	for _, entry := range entries {
		if entry.Record == nil || (entry.Status != CatalogAvailableUnverified && entry.Status != CatalogReady) {
			continue
		}
		record := *entry.Record
		if record.Name != "hermes-agent" || record.Version != plan.Version ||
			record.TarballURL != f.registryURL("manifests", plan.IndexDigest) ||
			record.SHA512Integrity != plan.SourceIdentity || record.EnginesNode != "" ||
			record.Size <= 0 || record.Size > plan.BundleMaxBytes || !validBoundedText(record.FetchedBy, 256, false) {
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
