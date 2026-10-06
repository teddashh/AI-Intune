package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/blobstore"
	"github.com/teddashh/AI-Intune/internal/store"
)

func newRemoteArtifactFixture(t *testing.T, backend blobstore.Backend) (jobsFixture, *hub) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	oldNow := hubNow
	hubNow = func() time.Time { return jobsTestNow }
	t.Cleanup(func() { hubNow = oldNow })
	artifactsDir := artifactsDirFor(dbPath)
	h := &hub{store: st, artifactsDir: artifactsDir, blobs: backend}
	mux := http.NewServeMux()
	h.machineAndPublicRoutes(mux)
	return jobsFixture{store: st, mux: mux, machine: enrollViaHTTP(t, mux, st, "remote-machine"), artifactsDir: artifactsDir}, h
}

func putMemoryArtifact(t *testing.T, mem *blobstore.Memory, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if _, err := mem.Put(context.Background(), digest, "application/gzip", int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestServeRemoteArtifactWhenLocalFileIsMissing(t *testing.T) {
	body := []byte("remote tarball bytes")
	mem := blobstore.NewMemory()
	digest := putMemoryArtifact(t, mem, body)
	f, h := newRemoteArtifactFixture(t, mem)
	if err := h.store.UpsertObjectBlob(store.ObjectBlob{
		Digest: digest, SizeBytes: int64(len(body)), ObjectKey: blobstore.ObjectKey(digest),
		Backend: mem.Name(), Kind: blobstore.KindArtifact, MediaType: "application/gzip",
	}); err != nil {
		t.Fatal(err)
	}
	grantArtifactDownload(t, f, digest, int64(len(body)))
	get := requestArtifact(t, f.mux, http.MethodGet, "/v1/artifacts/"+digest, f.machine.token)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), body) {
		t.Fatalf("GET remote = %d %q", get.Code, get.Body.Bytes())
	}
	if get.Header().Get("Content-Type") != "application/gzip" || get.Header().Get("ETag") != `"`+digest+`"` {
		t.Fatalf("headers %v", get.Header())
	}
	head := requestArtifact(t, f.mux, http.MethodHead, "/v1/artifacts/"+digest, f.machine.token)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD remote = %d %q", head.Code, head.Body.Bytes())
	}
}

func TestServeArtifactPrefersLocalFileOverRemote(t *testing.T) {
	remote := []byte("remote tarball bytes")
	mem := blobstore.NewMemory()
	digest := putMemoryArtifact(t, mem, remote)
	f, h := newRemoteArtifactFixture(t, mem)
	if err := h.store.UpsertObjectBlob(store.ObjectBlob{
		Digest: digest, SizeBytes: int64(len(remote)), ObjectKey: blobstore.ObjectKey(digest),
		Backend: mem.Name(), Kind: blobstore.KindArtifact, MediaType: "application/gzip",
	}); err != nil {
		t.Fatal(err)
	}
	local := []byte("local-wins")
	if err := os.MkdirAll(f.artifactsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.artifactsDir, digest+".tgz"), local, 0o600); err != nil {
		t.Fatal(err)
	}
	grantArtifactDownload(t, f, digest, int64(len(local)))
	get := requestArtifact(t, f.mux, http.MethodGet, "/v1/artifacts/"+digest, f.machine.token)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), local) {
		t.Fatalf("local file did not win: %d %q", get.Code, get.Body.Bytes())
	}
}

func TestServeArtifactWithoutBackendStaysNotFound(t *testing.T) {
	body := []byte("not on disk")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	f, h := newRemoteArtifactFixture(t, nil)
	if err := h.store.UpsertObjectBlob(store.ObjectBlob{
		Digest: digest, SizeBytes: int64(len(body)), ObjectKey: blobstore.ObjectKey(digest),
		Backend: "memory", Kind: blobstore.KindArtifact, MediaType: "application/gzip",
	}); err != nil {
		t.Fatal(err)
	}
	grantArtifactDownload(t, f, digest, int64(len(body)))
	get := requestArtifact(t, f.mux, http.MethodGet, "/v1/artifacts/"+digest, f.machine.token)
	if get.Code != http.StatusNotFound {
		t.Fatalf("nil backend status = %d %s", get.Code, get.Body.String())
	}
}

func TestServeArtifactRejectsBackendMismatch(t *testing.T) {
	body := []byte("remote tarball bytes")
	mem := blobstore.NewMemory()
	digest := putMemoryArtifact(t, mem, body)
	f, h := newRemoteArtifactFixture(t, mem)
	if err := h.store.UpsertObjectBlob(store.ObjectBlob{
		Digest: digest, SizeBytes: int64(len(body)), ObjectKey: blobstore.ObjectKey(digest),
		Backend: "s3", Kind: blobstore.KindArtifact, MediaType: "application/gzip",
	}); err != nil {
		t.Fatal(err)
	}
	grantArtifactDownload(t, f, digest, int64(len(body)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+digest, nil)
	req.Header.Set("Authorization", "Bearer "+f.machine.token)
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || bytes.Contains(rec.Body.Bytes(), body) {
		t.Fatalf("backend mismatch = %d %q", rec.Code, rec.Body.Bytes())
	}
}
