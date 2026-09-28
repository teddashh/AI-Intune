package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type hermesRegistryFixture struct {
	server        *httptest.Server
	version       string
	indexDigest   string
	manifests     map[string][]byte
	blobs         map[string][]byte
	mu            sync.Mutex
	mutatedBlob   string
	omitArch      string
	duplicateArch string
}

func newHermesRegistryFixture(t *testing.T) *hermesRegistryFixture {
	t.Helper()
	f := &hermesRegistryFixture{
		version: "2026.9.7", manifests: make(map[string][]byte), blobs: make(map[string][]byte),
	}
	platformDescriptors := make([]ociDescriptor, 0, 2)
	for _, arch := range []string{"amd64", "arm64"} {
		config := []byte("{\"architecture\":\"" + arch + "\",\"os\":\"linux\"}\n")
		layer := []byte("hermes-image-layer-" + arch + "\n")
		configDescriptor := testOCIDescriptor("application/vnd.oci.image.config.v1+json", config)
		layerDescriptor := testOCIDescriptor("application/vnd.oci.image.layer.v1.tar+gzip", layer)
		f.blobs[configDescriptor.Digest] = config
		f.blobs[layerDescriptor.Digest] = layer
		manifest := ociManifest{
			SchemaVersion: 2, MediaType: hermesImageMediaManifest,
			Config: configDescriptor, Layers: []ociDescriptor{layerDescriptor},
		}
		body, err := marshalCompactNoEscape(manifest)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, '\n')
		descriptor := testOCIDescriptor(hermesImageMediaManifest, body)
		descriptor.Platform = &ociPlatform{OS: "linux", Architecture: arch}
		f.manifests[descriptor.Digest] = body
		platformDescriptors = append(platformDescriptors, descriptor)
	}
	index := ociIndex{SchemaVersion: 2, MediaType: hermesImageMediaIndex, Manifests: platformDescriptors}
	indexBody, err := marshalCompactNoEscape(index)
	if err != nil {
		t.Fatal(err)
	}
	indexBody = append(indexBody, '\n')
	indexDescriptor := testOCIDescriptor(hermesImageMediaIndex, indexBody)
	f.indexDigest = indexDescriptor.Digest
	f.manifests["v"+f.version] = indexBody
	f.manifests[f.indexDigest] = indexBody
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func testOCIDescriptor(mediaType string, body []byte) ociDescriptor {
	sum := sha256.Sum256(body)
	return ociDescriptor{MediaType: mediaType, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(body))}
}

func (f *hermesRegistryFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := "/v2/" + HermesImageRepository + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	kindAndIdentity := strings.TrimPrefix(r.URL.Path, prefix)
	kind, identity, ok := strings.Cut(kindAndIdentity, "/")
	if !ok || (kind != "manifests" && kind != "blobs") {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if kind == "manifests" {
		body, found := f.manifests[identity]
		if !found {
			http.NotFound(w, r)
			return
		}
		mediaType := hermesImageMediaManifest
		if identity == "v"+f.version || identity == f.indexDigest {
			body = f.indexBodyForResponse(body)
			mediaType = hermesImageMediaIndex
		}
		digest := sha256.Sum256(body)
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(digest[:]))
		_, _ = w.Write(body)
		return
	}
	body, found := f.blobs[identity]
	if !found {
		http.NotFound(w, r)
		return
	}
	if identity == f.mutatedBlob {
		body = append([]byte{}, body...)
		body[0] ^= 0xff
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(body)
}

func (f *hermesRegistryFixture) indexBodyForResponse(original []byte) []byte {
	if f.omitArch == "" && f.duplicateArch == "" {
		return original
	}
	var index ociIndex
	_ = json.Unmarshal(original, &index)
	filtered := index.Manifests[:0]
	for _, descriptor := range index.Manifests {
		if descriptor.Platform != nil && descriptor.Platform.Architecture == f.omitArch {
			continue
		}
		filtered = append(filtered, descriptor)
		if descriptor.Platform != nil && descriptor.Platform.Architecture == f.duplicateArch {
			filtered = append(filtered, descriptor)
		}
	}
	index.Manifests = filtered
	body, _ := marshalCompactNoEscape(index)
	return append(body, '\n')
}

func newHermesFixtureFetcher(t *testing.T, fixture *hermesRegistryFixture, dir string) *HermesImageFetcher {
	t.Helper()
	fetcher, err := newHermesImageFetcher(hermesImageFetcherConfig{
		artifactsDir: dir, registryURL: fixture.server.URL, tokenOriginURL: fixture.server.URL,
		repository: HermesImageRepository, client: fixture.server.Client(), allowHTTP: true,
		manifestMax: 1 << 20, blobMax: 1 << 20, bundleMax: 8 << 20,
		metadataTimeout: time.Minute, downloadTimeout: time.Minute,
		now: func() time.Time { return time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return fetcher
}

func TestHermesImagePreviewPinsExactMultiPlatformIndex(t *testing.T) {
	fixture := newHermesRegistryFixture(t)
	fetcher := newHermesFixtureFetcher(t, fixture, t.TempDir())
	plan, err := fetcher.PreviewPlan(t.Context(), fixture.version)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PolicyVersion != HermesImageFetchPolicyVersion || plan.Name != "hermes-agent" ||
		plan.Version != fixture.version || plan.RegistryOrigin != fixture.server.URL ||
		plan.Repository != HermesImageRepository || plan.Tag != "v"+fixture.version ||
		plan.IndexDigest != fixture.indexDigest || plan.IndexMediaType != hermesImageMediaIndex ||
		len(plan.Manifests) != 2 || plan.Manifests[0].Arch != "amd64" || plan.Manifests[1].Arch != "arm64" ||
		!strings.HasPrefix(plan.SourceIdentity, "sha512-") || !strings.HasPrefix(plan.PreviewDigest, "sha256:") {
		t.Fatalf("plan=%+v", plan)
	}
	if err := fetcher.validatePlan(plan); err != nil {
		t.Fatal(err)
	}
	raw, err := marshalCompactNoEscape(plan)
	if err != nil || !ValidHermesImageSourcePlan(string(raw)) {
		t.Fatalf("canonical source plan rejected: err=%v raw=%s", err, raw)
	}
	if ValidHermesImageSourcePlan(string(raw)+"\n") ||
		ValidHermesImageSourcePlan(strings.Replace(string(raw), `"name":"hermes-agent"`, `"name":"hermes-agent","extra":true`, 1)) {
		t.Fatal("noncanonical or unknown-field source plan passed")
	}
	changed := plan
	changed.Manifests = append([]HermesImagePlatformManifest{}, plan.Manifests...)
	changed.Manifests[0].Digest = "sha256:" + strings.Repeat("a", 64)
	if err := fetcher.validatePlan(changed); err == nil {
		t.Fatal("mutated platform digest passed plan validation")
	}
}

func TestHermesImagePreviewRequiresBothArchitecturesExactlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		omit      string
		duplicate string
	}{
		{name: "missing", omit: "arm64"},
		{name: "duplicate", duplicate: "amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newHermesRegistryFixture(t)
			fixture.mu.Lock()
			fixture.omitArch = tc.omit
			fixture.duplicateArch = tc.duplicate
			fixture.mu.Unlock()
			fetcher := newHermesFixtureFetcher(t, fixture, t.TempDir())
			if _, err := fetcher.PreviewPlan(t.Context(), fixture.version); !errors.Is(err, ErrMetadataInvalid) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestHermesImageFetchBuildsDeterministicVerifiedOCIArchive(t *testing.T) {
	fixture := newHermesRegistryFixture(t)
	firstDir, secondDir := t.TempDir(), t.TempDir()
	firstFetcher := newHermesFixtureFetcher(t, fixture, firstDir)
	plan, err := firstFetcher.PreviewPlan(t.Context(), fixture.version)
	if err != nil {
		t.Fatal(err)
	}
	var phases []FetchProgress
	first, existed, err := firstFetcher.FetchExact(t.Context(), plan, "operator:test", func(update FetchProgress) error {
		phases = append(phases, update)
		return nil
	})
	if err != nil || existed || first.Name != "hermes-agent" || first.Version != fixture.version ||
		first.SHA512Integrity != plan.SourceIdentity || first.EnginesNode != "" || first.Size <= 0 {
		t.Fatalf("record=%+v existed=%t err=%v", first, existed, err)
	}
	if len(phases) < 5 || phases[0].Phase != FetchPhaseDownloading ||
		phases[len(phases)-1].Phase != FetchPhasePublishing {
		t.Fatalf("phases=%+v", phases)
	}
	entries := readHermesOCIBundle(t, filepath.Join(firstDir, first.SHA256+".tgz"))
	for _, name := range []string{"index.json", "oci-layout"} {
		if len(entries[name]) == 0 {
			t.Fatalf("bundle lacks %s", name)
		}
	}
	var root ociIndex
	if err := json.Unmarshal(entries["index.json"], &root); err != nil || len(root.Manifests) != 1 ||
		root.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != hermesImageRefName+":v"+fixture.version {
		t.Fatalf("root=%+v err=%v", root, err)
	}
	nested := entries[ociBlobPath(root.Manifests[0].Digest)]
	var platformIndex ociIndex
	if err := json.Unmarshal(nested, &platformIndex); err != nil || len(platformIndex.Manifests) != 2 ||
		platformIndex.Manifests[0].Platform.Architecture != "amd64" || platformIndex.Manifests[1].Platform.Architecture != "arm64" {
		t.Fatalf("platform index=%+v err=%v", platformIndex, err)
	}
	for digest, body := range fixture.blobs {
		if string(entries[ociBlobPath(digest)]) != string(body) {
			t.Fatalf("bundle blob %s mismatch", digest)
		}
	}
	secondFetcher := newHermesFixtureFetcher(t, fixture, secondDir)
	second, existed, err := secondFetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err != nil || existed || second.SHA256 != first.SHA256 || second.Size != first.Size {
		t.Fatalf("second=%+v existed=%t err=%v first=%+v", second, existed, err, first)
	}
	cached, existed, err := firstFetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err != nil || !existed || cached.SHA256 != first.SHA256 {
		t.Fatalf("cached=%+v existed=%t err=%v", cached, existed, err)
	}
}

func TestFetcherDispatchesHermesImagePlanAndFetch(t *testing.T) {
	fixture := newHermesRegistryFixture(t)
	dir := t.TempDir()
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: dir, registryURL: fixture.server.URL, nodeOriginURL: fixture.server.URL,
		hermesRegistryURL: fixture.server.URL, hermesTokenOriginURL: fixture.server.URL,
		client: fixture.server.Client(), metadataMax: 1 << 20, artifactMax: 8 << 20,
		metadataTimeout: time.Minute, downloadTimeout: time.Minute, allowHTTP: true,
		now: func() time.Time { return time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fetcher.PreviewPlan(t.Context(), "hermes-agent", fixture.version)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceKind != ArtifactSourceHermesImage || plan.PolicyVersion != HermesImageFetchPolicyVersion ||
		plan.Name != "hermes-agent" || plan.Version != fixture.version || plan.EnginesNode != "" ||
		plan.MaxBytes != DefaultHermesImageBundleMaxBytes || !ValidHermesImageSourcePlan(plan.SourcePlan) {
		t.Fatalf("plan=%+v", plan)
	}
	record, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil)
	if err != nil || existed || record.Name != plan.Name || record.Version != plan.Version ||
		record.TarballURL != plan.TarballURL || record.SHA512Integrity != plan.SHA512Integrity {
		t.Fatalf("record=%+v existed=%t err=%v", record, existed, err)
	}
	tampered := plan
	sourcePlan, err := decodeHermesImageSourcePlan(plan.SourcePlan)
	if err != nil {
		t.Fatal(err)
	}
	tampered.SourcePlan = strings.Replace(plan.SourcePlan, sourcePlan.IndexDigest,
		"sha256:"+strings.Repeat("a", 64), 1)
	if _, _, err := fetcher.FetchExact(t.Context(), tampered, "operator:test", nil); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("tampered source plan err=%v", err)
	}
}

func TestHermesImageFetchRejectsBlobChangedAfterPreview(t *testing.T) {
	fixture := newHermesRegistryFixture(t)
	fetcher := newHermesFixtureFetcher(t, fixture, t.TempDir())
	plan, err := fetcher.PreviewPlan(t.Context(), fixture.version)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(fixture.blobs))
	for digest := range fixture.blobs {
		keys = append(keys, digest)
	}
	sort.Strings(keys)
	fixture.mu.Lock()
	fixture.mutatedBlob = keys[0]
	fixture.mu.Unlock()
	if _, _, err := fetcher.FetchExact(t.Context(), plan, "operator:test", nil); !errors.Is(err, ErrIntegrityMismatch) {
		t.Fatalf("err=%v", err)
	}
	entries, err := os.ReadDir(fetcher.artifactsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tgz") || strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("failed fetch published %s", entry.Name())
		}
	}
}

func TestHermesImagePolicyRejectsMalformedBearerChallenges(t *testing.T) {
	valid := `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:nousresearch/hermes-agent:pull"`
	parameters, err := parseBearerChallenge(valid)
	if err != nil || parameters["scope"] != "repository:nousresearch/hermes-agent:pull" {
		t.Fatalf("parameters=%v err=%v", parameters, err)
	}
	for _, value := range []string{"Basic abc", `Bearer realm="x"`, `Bearer realm="x",realm="y",service="z",scope="s"`} {
		if _, err := parseBearerChallenge(value); err == nil {
			t.Fatalf("challenge %q passed", value)
		}
	}
}

func TestHermesImageProductionConstructorPinsOfficialSource(t *testing.T) {
	fetcher, err := NewHermesImageFetcher(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.registryOrigin != ProductionHermesRegistryOrigin || fetcher.tokenOrigin != ProductionHermesTokenOrigin ||
		fetcher.repository != HermesImageRepository || fetcher.bundleMax != DefaultHermesImageBundleMaxBytes {
		t.Fatalf("fetcher=%+v", fetcher)
	}
	if !ValidHermesVersion("2026.9.7") || ValidHermesVersion("v2026.9.7") || ValidHermesVersion("latest") {
		t.Fatal("Hermes exact-version policy changed")
	}
}

func TestHermesImageProductionPreview(t *testing.T) {
	if os.Getenv("AI_INTUNE_LIVE_HERMES_FETCH_TEST") != "1" {
		t.Skip("set AI_INTUNE_LIVE_HERMES_FETCH_TEST=1 to query the official registry")
	}
	fetcher, err := NewHermesImageFetcher(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fetcher.PreviewPlan(t.Context(), "2026.9.7")
	if err != nil {
		t.Fatal(err)
	}
	if plan.IndexDigest != "sha256:63bfb6d732f49a55d453e801057273785cc61e0f6ee43db3fa2f2a79846301b7" ||
		len(plan.Manifests) != 2 || plan.Manifests[0].Arch != "amd64" || plan.Manifests[1].Arch != "arm64" {
		t.Fatalf("official plan=%+v", plan)
	}
}

func TestHermesImageProductionFetch(t *testing.T) {
	if os.Getenv("AI_INTUNE_LIVE_HERMES_FETCH_FULL") != "1" {
		t.Skip("set AI_INTUNE_LIVE_HERMES_FETCH_FULL=1 to fetch the official multi-arch image")
	}
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher, err := NewHermesImageFetcher(dir)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := fetcher.PreviewPlan(t.Context(), "2026.9.7")
	if err != nil {
		t.Fatal(err)
	}
	record, existed, err := fetcher.FetchExact(t.Context(), plan, "operator:live-test", nil)
	if err != nil || existed || record.Size <= 1<<30 || record.Size > DefaultHermesImageBundleMaxBytes {
		t.Fatalf("record=%+v existed=%t err=%v", record, existed, err)
	}
	file, err := os.Open(filepath.Join(dir, record.SHA256+".tgz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	seenIndex, seenLayout, blobs := false, false, 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch header.Name {
		case "index.json":
			seenIndex = true
		case "oci-layout":
			seenLayout = true
		default:
			if strings.HasPrefix(header.Name, "blobs/sha256/") {
				blobs++
			}
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatal(err)
		}
	}
	if !seenIndex || !seenLayout || blobs < 4 {
		t.Fatalf("OCI archive index=%t layout=%t blobs=%d", seenIndex, seenLayout, blobs)
	}
}

func readHermesOCIBundle(t *testing.T, filename string) map[string][]byte {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	entries := make(map[string][]byte)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = body
	}
	return entries
}

func TestHermesImageFetchHonorsCancellation(t *testing.T) {
	fixture := newHermesRegistryFixture(t)
	fetcher := newHermesFixtureFetcher(t, fixture, t.TempDir())
	plan, err := fetcher.PreviewPlan(t.Context(), fixture.version)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := fetcher.FetchExact(ctx, plan, "operator:test", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
