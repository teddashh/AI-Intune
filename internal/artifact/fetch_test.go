package artifact

import (
	"bytes"
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
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fetchTestVersion = "2026.9.2"

var fetchTestNow = time.Date(2026, 9, 8, 12, 34, 56, 0, time.UTC)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestNewFetcherFailsClosed(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher, err := NewFetcher(dir)
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	if fetcher.registryOrigin != ProductionRegistryOrigin || fetcher.allowHTTP {
		t.Fatalf("production policy = origin %q, allowHTTP %v", fetcher.registryOrigin, fetcher.allowHTTP)
	}
	transport, ok := fetcher.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", fetcher.client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("production transport retained an ambient proxy hook")
	}
	if !transport.DisableCompression {
		t.Fatal("production transport permits transparent representation changes")
	}
	if transport.MaxResponseHeaderBytes <= 0 {
		t.Fatal("production transport has no response-header bound")
	}
	if got := fetcher.client.CheckRedirect(&http.Request{}, nil); !errors.Is(got, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy returned %v", got)
	}

	if _, err := NewFetcher("relative/artifacts"); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("relative directory error = %v", err)
	}
	if _, err := NewFetcher(dir + string(os.PathSeparator)); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("non-canonical directory error = %v", err)
	}
	if _, err := NewFetcher(string(os.PathSeparator)); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("filesystem root directory error = %v", err)
	}

	registryCases := []string{
		"http://registry.npmjs.org",
		"https://example.com",
		"https://user@example.com",
		"https://registry.npmjs.org/path",
		"https://registry.npmjs.org?token=secret",
		"https://registry.npmjs.org#fragment",
		" https://registry.npmjs.org",
	}
	for _, registry := range registryCases {
		registry := registry
		t.Run(registry, func(t *testing.T) {
			t.Parallel()
			_, err := newFetcher(fetcherConfig{artifactsDir: filepath.Join(t.TempDir(), "artifacts"), registryURL: registry})
			if !errors.Is(err, ErrRegistryPolicy) {
				t.Fatalf("newFetcher(%q) error = %v", registry, err)
			}
		})
	}

	inputTransport := http.DefaultTransport.(*http.Transport).Clone()
	inputTransport.Proxy = func(*http.Request) (*url.URL, error) {
		return url.Parse("http://ambient-proxy.invalid")
	}
	inputClient := &http.Client{Transport: inputTransport, Timeout: time.Hour}
	testFetcher, err := newFetcher(fetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"), registryURL: "http://registry.test",
		client: inputClient, allowHTTP: true, downloadTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("test newFetcher: %v", err)
	}
	if testFetcher.client == inputClient || testFetcher.client.Transport == inputTransport {
		t.Fatal("newFetcher mutated rather than cloning the injected client/transport")
	}
	if testFetcher.client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("cloned test transport retained proxy hook")
	}
	if inputTransport.Proxy == nil || inputClient.Timeout != time.Hour {
		t.Fatal("injected client was mutated")
	}
	if testFetcher.client.Timeout != time.Minute {
		t.Fatalf("bounded client timeout = %v", testFetcher.client.Timeout)
	}

	_, err = newFetcher(fetcherConfig{
		artifactsDir: filepath.Join(t.TempDir(), "artifacts"), registryURL: "http://registry.test",
		client:    &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })},
		allowHTTP: true,
	})
	if !errors.Is(err, ErrRegistryPolicy) {
		t.Fatalf("custom RoundTripper error = %v", err)
	}
}

func TestFetcherTarballURLMatchesCatalogByteBound(t *testing.T) {
	t.Parallel()

	fetcher, err := NewFetcher(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := ProductionRegistryOrigin + "/"
	atLimit := prefix + strings.Repeat("a", MaxTarballURLBytes-len(prefix))
	if len(atLimit) != MaxTarballURLBytes {
		t.Fatalf("test URL length=%d", len(atLimit))
	}
	if got, err := fetcher.validateTarballURL(atLimit); err != nil || got != atLimit {
		t.Fatalf("%d-byte tarball URL = %q, %v", MaxTarballURLBytes, got, err)
	}
	if _, err := fetcher.validateTarballURL(atLimit + "a"); !errors.Is(err, ErrRegistryPolicy) {
		t.Fatalf("%d-byte tarball URL error = %v", MaxTarballURLBytes+1, err)
	}
}

func TestFetcherPreviewPlanPinsExactPrivateSource(t *testing.T) {
	t.Parallel()

	tarball := []byte("exact tarball bytes")
	var tarballHits atomic.Int32
	server := newFetchTestServer(t, func(origin string, w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/openclaw/" + fetchTestVersion:
			writeMetadata(t, w, origin+"/openclaw/-/openclaw-"+fetchTestVersion+".tgz", tarball, "^22.0.0")
		case "/openclaw/-/openclaw-" + fetchTestVersion + ".tgz":
			tarballHits.Add(1)
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, req)
		}
	})
	defer server.Close()
	fetcher := mustTestFetcher(t, server, filepath.Join(t.TempDir(), "artifacts"), 1<<20, 1<<20)

	plan, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
	if err != nil {
		t.Fatalf("PreviewPlan: %v", err)
	}
	if plan.PolicyVersion != FetchPolicyVersion || plan.Name != "openclaw" || plan.Version != fetchTestVersion ||
		plan.RegistryOrigin != server.URL || plan.MaxBytes != 1<<20 || plan.PreviewedAt != fetchTestNow ||
		plan.EnginesNode != "^22.0.0" || plan.PreviewDigest != previewPlanDigest(plan) {
		t.Fatalf("unexpected preview plan: %+v", plan)
	}
	if !strings.HasPrefix(plan.PreviewDigest, "sha256:") || len(plan.PreviewDigest) != len("sha256:")+sha256.Size*2 {
		t.Fatalf("preview digest = %q", plan.PreviewDigest)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal preview plan: %v", err)
	}
	if bytes.Contains(encoded, []byte("tarball")) || bytes.Contains(encoded, []byte("openclaw-"+fetchTestVersion+".tgz")) {
		t.Fatalf("default plan serialization disclosed private source: %s", encoded)
	}

	tampered := plan
	tampered.EnginesNode = ">=0"
	if _, _, err := fetcher.FetchExact(context.Background(), tampered, "operator:alice", nil); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("tampered plan error = %v", err)
	}
	tampered = plan
	tampered.TarballURL += "?token=secret"
	tampered.PreviewDigest = previewPlanDigest(tampered)
	if _, _, err := fetcher.FetchExact(context.Background(), tampered, "operator:alice", nil); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("policy-invalid re-digested plan error = %v", err)
	}
	if got := tarballHits.Load(); got != 0 {
		t.Fatalf("invalid plans caused %d tarball requests", got)
	}
}

func TestFetcherPreviewRejectsUnsafeOrUnboundedMetadata(t *testing.T) {
	t.Parallel()

	validTarball := []byte("tarball")
	validSRI := sha512SRI(validTarball)
	tests := []struct {
		name        string
		metadataMax int64
		wantErr     error
		serve       func(origin string, w http.ResponseWriter, req *http.Request)
	}{
		{
			name: "declared too large", metadataMax: 64, wantErr: ErrMetadataTooLarge,
			serve: func(_ string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "65")
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			name: "chunked body too large", metadataMax: 64, wantErr: ErrMetadataTooLarge,
			serve: func(_ string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				_, _ = w.Write(bytes.Repeat([]byte("x"), 65))
			},
		},
		{
			name: "wrong content type", wantErr: ErrMetadataInvalid,
			serve: func(_ string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(`{}`))
			},
		},
		{
			name: "encoded representation", wantErr: ErrMetadataInvalid,
			serve: func(origin string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				writeMetadata(t, w, origin+"/package.tgz", validTarball, "")
			},
		},
		{
			name: "trailing JSON", wantErr: ErrMetadataInvalid,
			serve: func(origin string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `%s {}`, metadataJSON(origin+"/package.tgz", validSRI, ""))
			},
		},
		{
			name: "identity mismatch", wantErr: ErrMetadataInvalid,
			serve: func(origin string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(metadataJSONFor("other", fetchTestVersion, origin+"/package.tgz", validSRI, ""))
			},
		},
		{
			name: "non SHA512 SRI", wantErr: ErrMetadataInvalid,
			serve: func(origin string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(metadataJSON(origin+"/package.tgz", "sha256-Zm9v", ""))
			},
		},
		{
			name: "cross origin tarball", wantErr: ErrRegistryPolicy,
			serve: func(_ string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(metadataJSON("http://other.invalid/package.tgz", validSRI, ""))
			},
		},
		{
			name: "tarball credentials", wantErr: ErrRegistryPolicy,
			serve: func(origin string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(metadataJSON(strings.Replace(origin, "://", "://user:secret@", 1)+"/package.tgz", validSRI, ""))
			},
		},
		{
			name: "tarball query", wantErr: ErrRegistryPolicy,
			serve: func(origin string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(metadataJSON(origin+"/package.tgz?token=secret", validSRI, ""))
			},
		},
		{
			name: "tarball fragment", wantErr: ErrRegistryPolicy,
			serve: func(origin string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(metadataJSON(origin+"/package.tgz#fragment", validSRI, ""))
			},
		},
		{
			name: "control in engines", wantErr: ErrMetadataInvalid,
			serve: func(origin string, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(metadataJSON(origin+"/package.tgz", validSRI, "node\nsecret"))
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := newFetchTestServer(t, test.serve)
			defer server.Close()
			metadataMax := test.metadataMax
			if metadataMax == 0 {
				metadataMax = 1 << 20
			}
			fetcher := mustTestFetcher(t, server, filepath.Join(t.TempDir(), "artifacts"), 1<<20, metadataMax)
			_, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("PreviewPlan error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestFetcherRejectsMetadataRedirectAndTags(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	server := newFetchTestServer(t, func(_ string, w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		if req.URL.Path == "/redirected" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.Redirect(w, req, "/redirected", http.StatusFound)
	})
	defer server.Close()
	fetcher := mustTestFetcher(t, server, filepath.Join(t.TempDir(), "artifacts"), 1<<20, 1<<20)

	_, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
	if !errors.Is(err, ErrMetadataInvalid) {
		t.Fatalf("redirect error = %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("redirect followed; request count = %d", got)
	}
	for _, target := range [][2]string{{"openclaw", "latest"}, {"openclaw", "1.2"}, {"other", fetchTestVersion}} {
		if _, err := fetcher.PreviewPlan(context.Background(), target[0], target[1]); !errors.Is(err, ErrInvalidFetchRequest) {
			t.Fatalf("PreviewPlan(%q, %q) error = %v", target[0], target[1], err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("invalid selectors reached registry; request count = %d", got)
	}
}

func TestFetcherFetchExactPublishesAndReusesVerifiedArtifact(t *testing.T) {
	t.Parallel()

	tarball := bytes.Repeat([]byte("openclaw-tarball"), 32)
	var tarballHits atomic.Int32
	server := newFetchTestServer(t, func(origin string, w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/openclaw/" + fetchTestVersion:
			writeMetadata(t, w, origin+"/package.tgz", tarball, ">=22")
		case "/package.tgz":
			tarballHits.Add(1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, req)
		}
	})
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := mustTestFetcher(t, server, dir, 1<<20, 1<<20)
	plan, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
	if err != nil {
		t.Fatalf("PreviewPlan: %v", err)
	}

	var progress []FetchProgress
	record, existed, err := fetcher.FetchExact(context.Background(), plan, "operator:alice", func(update FetchProgress) error {
		progress = append(progress, update)
		return nil
	})
	if err != nil {
		t.Fatalf("FetchExact: %v", err)
	}
	if existed {
		t.Fatal("first fetch reported an existing artifact")
	}
	wantSHA256 := sha256.Sum256(tarball)
	wantDigest := hex.EncodeToString(wantSHA256[:])
	if record.SHA256 != wantDigest || record.Size != int64(len(tarball)) || record.SHA512Integrity != sha512SRI(tarball) ||
		record.FetchedAt != fetchTestNow || record.FetchedBy != "operator:alice" {
		t.Fatalf("unexpected sidecar: %+v", record)
	}
	assertProgress(t, progress, int64(len(tarball)), true)
	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, wantDigest+".tgz"), 0o600)
	assertMode(t, filepath.Join(dir, wantDigest+".json"), 0o600)
	stored, err := os.ReadFile(filepath.Join(dir, wantDigest+".tgz"))
	if err != nil || !bytes.Equal(stored, tarball) {
		t.Fatalf("stored tarball = %q, error %v", stored, err)
	}
	if err := ValidateStoredArtifact(dir, record); err != nil {
		t.Fatalf("ValidateStoredArtifact: %v", err)
	}
	assertNoFetchTemps(t, dir)

	progress = nil
	cached, existed, err := fetcher.FetchExact(context.Background(), plan, "operator:bob", func(update FetchProgress) error {
		progress = append(progress, update)
		return nil
	})
	if err != nil {
		t.Fatalf("cached FetchExact: %v", err)
	}
	if !existed || cached != record {
		t.Fatalf("cached result = existed %v, record %+v", existed, cached)
	}
	assertProgress(t, progress, int64(len(tarball)), true)
	if got := tarballHits.Load(); got != 1 {
		t.Fatalf("tarball downloads = %d, want 1", got)
	}
}

func TestFetcherCanceledHashPermitWaitReleasesSerialLock(t *testing.T) {
	tarball := []byte("cached artifact lock ordering")
	server := newFetchTestServer(t, func(origin string, w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/openclaw/" + fetchTestVersion:
			writeMetadata(t, w, origin+"/package.tgz", tarball, ">=22")
		case "/package.tgz":
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, req)
		}
	})
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := mustTestFetcher(t, server, dir, 1<<20, 1<<20)
	plan, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetcher.FetchExact(context.Background(), plan, "operator:first", nil); err != nil {
		t.Fatal(err)
	}

	releasePermit, err := acquireCatalogHashPermit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	permitReleased := false
	t.Cleanup(func() {
		if !permitReleased {
			releasePermit()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fetchResult := make(chan error, 1)
	go func() {
		_, _, fetchErr := fetcher.FetchExact(ctx, plan, "operator:retry", nil)
		fetchResult <- fetchErr
	}()

	lockDeadline := time.After(time.Second)
	for {
		if !fetcher.serial.TryLock() {
			break
		}
		fetcher.serial.Unlock()
		select {
		case <-lockDeadline:
			t.Fatal("FetchExact did not acquire its serial lock")
		case <-time.After(time.Millisecond):
		}
	}
	reconcileResult := make(chan error, 1)
	go func() {
		_, reconcileErr := fetcher.ReconcileStaleTemps(time.Now().UTC())
		reconcileResult <- reconcileErr
	}()
	select {
	case reconcileErr := <-reconcileResult:
		t.Fatalf("reconcile bypassed FetchExact serial lock: %v", reconcileErr)
	case <-time.After(25 * time.Millisecond):
	}

	cancel()
	select {
	case fetchErr := <-fetchResult:
		if !errors.Is(fetchErr, context.Canceled) {
			t.Fatalf("canceled cached validation error = %v", fetchErr)
		}
	case <-time.After(time.Second):
		t.Fatal("FetchExact remained blocked on the catalog permit after cancellation")
	}
	select {
	case reconcileErr := <-reconcileResult:
		if reconcileErr != nil {
			t.Fatalf("reconcile after canceled fetch: %v", reconcileErr)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled FetchExact did not release its serial lock")
	}
	releasePermit()
	permitReleased = true
}

func TestFetcherFetchExactRejectsBadRepresentationsAndCleansTemps(t *testing.T) {
	t.Parallel()

	callbackErr := errors.New("operation lease lost")
	tests := []struct {
		name        string
		artifactMax int64
		wantErr     error
		integrity   func([]byte) string
		serve       func(w http.ResponseWriter, req *http.Request, tarball []byte)
		progress    ProgressFunc
		wantTarget  int32
	}{
		{
			name: "integrity mismatch", artifactMax: 1024, wantErr: ErrIntegrityMismatch,
			integrity: func([]byte) string { return sha512SRI([]byte("different bytes")) },
			serve:     func(w http.ResponseWriter, _ *http.Request, tarball []byte) { _, _ = w.Write(tarball) },
		},
		{
			name: "chunked body over bound", artifactMax: 4, wantErr: ErrArtifactTooLarge,
			serve: func(w http.ResponseWriter, _ *http.Request, tarball []byte) {
				w.WriteHeader(http.StatusOK)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				_, _ = w.Write(tarball)
			},
		},
		{
			name: "encoded tarball", artifactMax: 1024, wantErr: ErrMetadataInvalid,
			serve: func(w http.ResponseWriter, _ *http.Request, tarball []byte) {
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = w.Write(tarball)
			},
		},
		{
			name: "redirect", artifactMax: 1024,
			serve: func(w http.ResponseWriter, req *http.Request, _ []byte) {
				http.Redirect(w, req, "/redirect-target", http.StatusFound)
			},
		},
		{
			name: "publishing progress callback failure", artifactMax: 1024, wantErr: callbackErr,
			serve: func(w http.ResponseWriter, _ *http.Request, tarball []byte) { _, _ = w.Write(tarball) },
			progress: func(update FetchProgress) error {
				if update.Phase == FetchPhasePublishing {
					return callbackErr
				}
				return nil
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			tarball := []byte("12345")
			var targetHits atomic.Int32
			server := newFetchTestServer(t, func(origin string, w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/openclaw/" + fetchTestVersion:
					integrity := sha512SRI(tarball)
					if test.integrity != nil {
						integrity = test.integrity(tarball)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(metadataJSON(origin+"/package.tgz", integrity, ""))
				case "/package.tgz":
					test.serve(w, req, tarball)
				case "/redirect-target":
					targetHits.Add(1)
					_, _ = w.Write(tarball)
				default:
					http.NotFound(w, req)
				}
			})
			defer server.Close()
			dir := filepath.Join(t.TempDir(), "artifacts")
			fetcher := mustTestFetcher(t, server, dir, test.artifactMax, 1<<20)
			plan, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
			if err != nil {
				t.Fatalf("PreviewPlan: %v", err)
			}
			_, _, err = fetcher.FetchExact(context.Background(), plan, "operator:alice", test.progress)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("FetchExact error = %v, want %v", err, test.wantErr)
				}
			} else if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
				t.Fatalf("redirect FetchExact error = %v", err)
			}
			if got := targetHits.Load(); got != test.wantTarget {
				t.Fatalf("redirect target hits = %d, want %d", got, test.wantTarget)
			}
			assertNoPublishedArtifacts(t, dir)
			assertNoFetchTemps(t, dir)
		})
	}
}

func TestFetcherPublishingFenceLeavesRecoverableOrphan(t *testing.T) {
	t.Parallel()

	tarball := []byte("durable publish")
	server := newFetchTestServer(t, func(origin string, w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/openclaw/" + fetchTestVersion:
			writeMetadata(t, w, origin+"/package.tgz", tarball, "")
		case "/package.tgz":
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, req)
		}
	})
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := mustTestFetcher(t, server, dir, 1024, 4096)
	plan, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
	if err != nil {
		t.Fatalf("PreviewPlan: %v", err)
	}
	claimLost := errors.New("worker claim lost")
	publishingCalls := 0
	_, _, err = fetcher.FetchExact(context.Background(), plan, "operator", func(update FetchProgress) error {
		if update.Phase == FetchPhasePublishing {
			publishingCalls++
			if publishingCalls == 2 {
				return claimLost
			}
		}
		return nil
	})
	if !errors.Is(err, claimLost) || publishingCalls != 2 {
		t.Fatalf("second publishing fence = calls %d, error %v", publishingCalls, err)
	}
	digest := sha256.Sum256(tarball)
	digestHex := hex.EncodeToString(digest[:])
	if _, err := os.Stat(filepath.Join(dir, digestHex+".tgz")); err != nil {
		t.Fatalf("first atomic step did not leave recoverable bytes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, digestHex+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fenced worker made sidecar authoritative: %v", err)
	}
	assertNoFetchTemps(t, dir)

	record, existed, err := fetcher.FetchExact(context.Background(), plan, "operator", nil)
	if err != nil || existed {
		t.Fatalf("retry after orphan = existed %v, error %v", existed, err)
	}
	if err := ValidateStoredArtifact(dir, record); err != nil {
		t.Fatalf("retry did not recover artifact: %v", err)
	}
}

func TestFetcherSerializesConcurrentFetchesAcrossInstances(t *testing.T) {
	t.Parallel()

	tarball := bytes.Repeat([]byte("serialized"), 128)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var tarballHits atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	server := newFetchTestServer(t, func(origin string, w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/openclaw/" + fetchTestVersion:
			writeMetadata(t, w, origin+"/package.tgz", tarball, "")
		case "/package.tgz":
			tarballHits.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := maxActive.Load()
				if current <= old || maxActive.CompareAndSwap(old, current) {
					break
				}
			}
			entered <- struct{}{}
			<-release
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, req)
		}
	})
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "artifacts")
	first := mustTestFetcher(t, server, dir, 1<<20, 1<<20)
	second := mustTestFetcher(t, server, dir, 1<<20, 1<<20)
	plan, err := first.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
	if err != nil {
		t.Fatalf("PreviewPlan: %v", err)
	}

	type result struct {
		existed bool
		err     error
	}
	results := make(chan result, 2)
	go func() {
		_, existed, err := first.FetchExact(context.Background(), plan, "operator:first", nil)
		results <- result{existed, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first fetch did not reach tarball server")
	}
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		_, existed, err := second.FetchExact(context.Background(), plan, "operator:second", nil)
		results <- result{existed, err}
	}()
	<-secondStarted
	close(release)

	got := []result{<-results, <-results}
	for _, result := range got {
		if result.err != nil {
			t.Fatalf("concurrent FetchExact: %v", result.err)
		}
	}
	if got[0].existed == got[1].existed {
		t.Fatalf("existing results = %v, %v; want one publisher and one reuse", got[0].existed, got[1].existed)
	}
	if hits := tarballHits.Load(); hits != 1 {
		t.Fatalf("tarball hits = %d, want 1", hits)
	}
	if maximum := maxActive.Load(); maximum != 1 {
		t.Fatalf("maximum concurrent downloads = %d, want 1", maximum)
	}
}

func TestFetcherRefusesExistingDigestMetadataCollision(t *testing.T) {
	t.Parallel()

	tarball := []byte("collision bytes")
	server := newFetchTestServer(t, func(origin string, w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/openclaw/" + fetchTestVersion:
			writeMetadata(t, w, origin+"/package.tgz", tarball, "")
		case "/package.tgz":
			_, _ = w.Write(tarball)
		default:
			http.NotFound(w, req)
		}
	})
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := mustTestFetcher(t, server, dir, 1<<20, 1<<20)
	plan, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
	if err != nil {
		t.Fatalf("PreviewPlan: %v", err)
	}
	if err := ensurePrivateArtifactDir(dir); err != nil {
		t.Fatalf("ensure dir: %v", err)
	}
	digest := sha256.Sum256(tarball)
	digestHex := hex.EncodeToString(digest[:])
	collision := Sidecar{
		Name: "openclaw", Version: "0.0.1", TarballURL: plan.TarballURL,
		SHA512Integrity: plan.SHA512Integrity, SHA256: digestHex, Size: int64(len(tarball)),
		FetchedAt: fetchTestNow, FetchedBy: "operator:other",
	}
	body, _ := json.Marshal(collision)
	if err := os.WriteFile(filepath.Join(dir, digestHex+".json"), body, 0o600); err != nil {
		t.Fatalf("write collision sidecar: %v", err)
	}

	_, _, err = fetcher.FetchExact(context.Background(), plan, "operator:alice", nil)
	if !errors.Is(err, ErrArtifactStorage) || !strings.Contains(err.Error(), "different metadata") {
		t.Fatalf("collision error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, digestHex+".tgz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collision published tarball, stat error = %v", err)
	}
	assertNoFetchTemps(t, dir)
}

func TestFetcherReconcileStaleTemps(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher, err := NewFetcher(dir)
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	old := fetchTestNow.Add(-2 * time.Hour)
	cutoff := fetchTestNow.Add(-time.Hour)
	oldTemps := []string{
		artifactFetchTempPrefix + "old.tmp",
		artifactSidecarTempPrefix + "old.tmp",
		".artifact-old.tmp",
		".sidecar-old.tmp",
	}
	for _, name := range append(append([]string{}, oldTemps...), "unrelated.tmp") {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("temp"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
	fresh := filepath.Join(dir, artifactFetchTempPrefix+"fresh.tmp")
	if err := os.WriteFile(fresh, []byte("fresh"), 0o600); err != nil {
		t.Fatalf("write fresh: %v", err)
	}
	if err := os.Chtimes(fresh, fetchTestNow, fetchTestNow); err != nil {
		t.Fatalf("chtimes fresh: %v", err)
	}

	removed, err := fetcher.ReconcileStaleTemps(cutoff)
	if err != nil {
		t.Fatalf("ReconcileStaleTemps: %v", err)
	}
	if removed != len(oldTemps) {
		t.Fatalf("removed = %d, want %d", removed, len(oldTemps))
	}
	for _, name := range oldTemps {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale temp %s remains: %v", name, err)
		}
	}
	for _, name := range []string{filepath.Base(fresh), "unrelated.tmp"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("preserved file %s: %v", name, err)
		}
	}
	assertMode(t, dir, 0o700)

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	link := filepath.Join(dir, artifactFetchTempPrefix+"link.tmp")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create temp symlink: %v", err)
	}
	removed, err = fetcher.ReconcileStaleTemps(time.Now().Add(time.Hour))
	if err != nil || removed < 1 {
		t.Fatalf("reconcile symlink = removed %d, error %v", removed, err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "keep" {
		t.Fatalf("symlink target changed: %q, %v", content, err)
	}
}

func TestFetcherReconcileRefusesTempNamedDirectory(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher, err := NewFetcher(dir)
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	tempDir := filepath.Join(dir, artifactFetchTempPrefix+"directory.tmp")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	removed, err := fetcher.ReconcileStaleTemps(time.Now().Add(time.Hour))
	if removed != 0 || !errors.Is(err, ErrArtifactStorage) {
		t.Fatalf("ReconcileStaleTemps = removed %d, error %v", removed, err)
	}
	if info, statErr := os.Stat(tempDir); statErr != nil || !info.IsDir() {
		t.Fatalf("temp-named directory was removed: %v", statErr)
	}
}

func newFetchTestServer(t *testing.T, handler func(string, http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		handler(server.URL, w, req)
	}))
	return server
}

func mustTestFetcher(t *testing.T, server *httptest.Server, dir string, artifactMax, metadataMax int64) *Fetcher {
	t.Helper()
	fetcher, err := newFetcher(fetcherConfig{
		artifactsDir: dir, registryURL: server.URL, client: server.Client(),
		artifactMax: artifactMax, metadataMax: metadataMax,
		metadataTimeout: 5 * time.Second, downloadTimeout: 5 * time.Second,
		allowHTTP: true, now: func() time.Time { return fetchTestNow },
	})
	if err != nil {
		t.Fatalf("newFetcher: %v", err)
	}
	return fetcher
}

func writeMetadata(t *testing.T, w http.ResponseWriter, tarballURL string, tarball []byte, engines string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if _, err := w.Write(metadataJSON(tarballURL, sha512SRI(tarball), engines)); err != nil {
		t.Errorf("write metadata: %v", err)
	}
}

func metadataJSON(tarballURL, integrity, engines string) []byte {
	return metadataJSONFor("openclaw", fetchTestVersion, tarballURL, integrity, engines)
}

func metadataJSONFor(name, version, tarballURL, integrity, engines string) []byte {
	body, _ := json.Marshal(map[string]any{
		"name": name, "version": version,
		"dist":    map[string]string{"tarball": tarballURL, "integrity": integrity},
		"engines": map[string]string{"node": engines},
	})
	return body
}

func sha512SRI(body []byte) string {
	digest := sha512.Sum512(body)
	return "sha512-" + base64.StdEncoding.EncodeToString(digest[:])
}

func assertProgress(t *testing.T, updates []FetchProgress, want int64, wantExpected bool) {
	t.Helper()
	if len(updates) == 0 {
		t.Fatal("no progress updates")
	}
	ranks := map[FetchPhase]int{
		FetchPhaseDownloading: 1,
		FetchPhaseVerifying:   2,
		FetchPhasePublishing:  3,
	}
	previous := int64(-1)
	previousRank := 0
	for _, update := range updates {
		rank := ranks[update.Phase]
		if rank == 0 || rank < previousRank {
			t.Fatalf("progress phase regressed: %+v", updates)
		}
		if update.DownloadedBytes < previous {
			t.Fatalf("progress bytes regressed: %+v", updates)
		}
		previous = update.DownloadedBytes
		previousRank = rank
		if wantExpected && (update.ExpectedBytes == nil || *update.ExpectedBytes != want) {
			t.Fatalf("progress expected bytes = %v, want %d", update.ExpectedBytes, want)
		}
	}
	if previous != want {
		t.Fatalf("final downloaded bytes = %d, want %d", previous, want)
	}
	if updates[0].Phase != FetchPhaseDownloading || updates[len(updates)-1].Phase != FetchPhasePublishing {
		t.Fatalf("progress did not span downloading through publishing: %+v", updates)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}

func assertNoPublishedArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read artifacts directory: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tgz" || filepath.Ext(entry.Name()) == ".json" {
			t.Fatalf("failed fetch published %s", entry.Name())
		}
	}
}

func assertNoFetchTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read artifacts directory: %v", err)
	}
	for _, entry := range entries {
		if isArtifactTempName(entry.Name()) {
			t.Fatalf("temporary artifact remains: %s", entry.Name())
		}
	}
}

func TestReadBoundedBodyPropagatesReadFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("read failed")
	_, err := readBoundedBody(io.MultiReader(strings.NewReader("ok"), errorReader{want}), -1, 100, ErrMetadataTooLarge)
	if !errors.Is(err, want) {
		t.Fatalf("readBoundedBody error = %v", err)
	}
}

func TestFetchProgressReaderCheckpointsInsteadOfWritingPerRead(t *testing.T) {
	t.Parallel()
	total := fetchProgressCheckpointBytes*3 + 123
	expected := total
	updates := make([]FetchProgress, 0, 3)
	reader := &fetchProgressReader{
		reader: bytes.NewReader(make([]byte, int(total))), expected: &expected,
		callback: func(update FetchProgress) error {
			updates = append(updates, update)
			return nil
		},
	}
	written, err := io.CopyBuffer(io.Discard, reader, make([]byte, 128<<10))
	if err != nil || written != total {
		t.Fatalf("checkpoint copy bytes=%d err=%v", written, err)
	}
	if len(updates) != 3 {
		t.Fatalf("progress callback count=%d updates=%+v", len(updates), updates)
	}
	for index, update := range updates {
		want := int64(index+1) * fetchProgressCheckpointBytes
		if update.Phase != FetchPhaseDownloading || update.DownloadedBytes != want ||
			update.ExpectedBytes == nil || *update.ExpectedBytes != total {
			t.Fatalf("checkpoint %d=%+v want_bytes=%d", index, update, want)
		}
	}
}

func TestFetchProgressReaderCheckpointStillFencesDownload(t *testing.T) {
	t.Parallel()
	want := errors.New("durable claim lost")
	reader := &fetchProgressReader{
		reader:   bytes.NewReader(make([]byte, int(fetchProgressCheckpointBytes))),
		callback: func(FetchProgress) error { return want },
	}
	_, err := io.CopyBuffer(io.Discard, reader, make([]byte, 128<<10))
	if !errors.Is(err, want) {
		t.Fatalf("checkpoint fence error=%v", err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestPreviewDigestStableAcrossEquivalentSRIInput(t *testing.T) {
	raw := sha512.Sum512([]byte("value"))
	input := "sha512-" + base64.RawStdEncoding.EncodeToString(raw[:])
	_, canonical, err := decodeCanonicalSHA512SRI(input)
	if err != nil {
		t.Fatalf("decodeCanonicalSHA512SRI: %v", err)
	}
	if canonical != "sha512-"+base64.StdEncoding.EncodeToString(raw[:]) {
		t.Fatalf("canonical SRI = %q", canonical)
	}
}

func TestFetcherNilContextRejected(t *testing.T) {
	fetcher, err := NewFetcher(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	if _, err := fetcher.PreviewPlan(nil, "openclaw", fetchTestVersion); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("nil preview context error = %v", err)
	}
	if _, _, err := fetcher.FetchExact(nil, PreviewPlan{}, "operator", nil); !errors.Is(err, ErrInvalidFetchRequest) {
		t.Fatalf("nil fetch context error = %v", err)
	}
}

func TestFetcherCachedProgressFailureDoesNotRedownload(t *testing.T) {
	t.Parallel()
	tarball := []byte("cached")
	var hits atomic.Int32
	server := newFetchTestServer(t, func(origin string, w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/openclaw/"+fetchTestVersion {
			writeMetadata(t, w, origin+"/package.tgz", tarball, "")
			return
		}
		if req.URL.Path == "/package.tgz" {
			hits.Add(1)
			_, _ = w.Write(tarball)
			return
		}
		http.NotFound(w, req)
	})
	defer server.Close()
	fetcher := mustTestFetcher(t, server, filepath.Join(t.TempDir(), "artifacts"), 1024, 4096)
	plan, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
	if err != nil {
		t.Fatalf("PreviewPlan: %v", err)
	}
	if _, _, err := fetcher.FetchExact(context.Background(), plan, "operator", nil); err != nil {
		t.Fatalf("FetchExact: %v", err)
	}
	want := errors.New("cannot persist progress")
	if _, _, err := fetcher.FetchExact(context.Background(), plan, "operator", func(FetchProgress) error { return want }); !errors.Is(err, want) {
		t.Fatalf("cached progress error = %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("cached progress failure redownloaded: hits %d", hits.Load())
	}
}

func TestFetcherIgnoresForeignMalformedSidecarDuringCacheLookup(t *testing.T) {
	t.Parallel()
	tarball := []byte("valid")
	server := newFetchTestServer(t, func(origin string, w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/openclaw/"+fetchTestVersion {
			writeMetadata(t, w, origin+"/package.tgz", tarball, "")
			return
		}
		if req.URL.Path == "/package.tgz" {
			_, _ = w.Write(tarball)
			return
		}
		http.NotFound(w, req)
	})
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetcher := mustTestFetcher(t, server, dir, 1024, 4096)
	if err := ensurePrivateArtifactDir(dir); err != nil {
		t.Fatalf("ensure dir: %v", err)
	}
	foreignDigest := strings.Repeat("a", sha256.Size*2)
	if err := os.WriteFile(filepath.Join(dir, foreignDigest+".json"), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write malformed sidecar: %v", err)
	}
	plan, err := fetcher.PreviewPlan(context.Background(), "openclaw", fetchTestVersion)
	if err != nil {
		t.Fatalf("PreviewPlan: %v", err)
	}
	if _, _, err := fetcher.FetchExact(context.Background(), plan, "operator", nil); err != nil {
		t.Fatalf("FetchExact: %v", err)
	}
}
