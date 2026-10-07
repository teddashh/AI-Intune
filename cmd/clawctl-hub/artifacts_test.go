package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

type fakeNPMRegistry struct {
	server      *httptest.Server
	tarball     []byte
	integrity   string
	version     string
	enginesNode string
	tarballHits atomic.Int64
}

func newFakeNPMRegistry(t *testing.T, tarball []byte, integrity string) *fakeNPMRegistry {
	t.Helper()
	f := &fakeNPMRegistry{
		tarball: tarball, integrity: integrity, version: "2026.9.2",
		enginesNode: ">=22.22.3 <23 || >=24.15.0 <25 || >=25.9.0",
	}
	f.server = httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openclaw/" + f.version:
			writeJSON(w, http.StatusOK, map[string]any{
				"name": "openclaw", "version": f.version,
				"dist": map[string]any{
					"tarball":   f.server.URL + "/openclaw/-/openclaw-" + f.version + ".tgz",
					"integrity": f.integrity,
				},
				"engines": map[string]any{"node": f.enginesNode},
			})
		case "/openclaw/-/openclaw-" + f.version + ".tgz":
			f.tarballHits.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(f.tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	oldClient := artifactHTTPClient
	artifactHTTPClient = f.server.Client()
	t.Cleanup(func() { artifactHTTPClient = oldClient })
	return f
}

func sha512Integrity(b []byte) string {
	sum := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

func TestFetchArtifactStoresHubMeasuredTarballAndSidecarAndReusesIt(t *testing.T) {
	tarball := []byte("fake tgz bytes\x00that Hub measures itself")
	registry := newFakeNPMRegistry(t, tarball, sha512Integrity(tarball))
	dir := filepath.Join(t.TempDir(), "artifacts")
	fetchedAt := time.Date(2026, 9, 6, 18, 30, 0, 0, time.UTC)
	oldNow := artifactNow
	artifactNow = func() time.Time { return fetchedAt }
	t.Cleanup(func() { artifactNow = oldNow })

	record, existed, err := fetchArtifact(context.Background(), dir, "openclaw@2026.9.2",
		registry.server.URL, defaultArtifactMaxBytes, "tester@hub")
	if err != nil {
		t.Fatalf("fetch 失敗：%v", err)
	}
	if existed {
		t.Fatal("第一次 fetch 不該說已經有了")
	}
	sha256Sum := sha256.Sum256(tarball)
	wantSHA256 := hex.EncodeToString(sha256Sum[:])
	if record.SHA256 != wantSHA256 || record.Size != int64(len(tarball)) || record.Name != "openclaw" ||
		record.Version != registry.version || record.SHA512Integrity != registry.integrity ||
		record.EnginesNode != registry.enginesNode || record.FetchedAt != fetchedAt || record.FetchedBy != "tester@hub" {
		t.Fatalf("sidecar 紀錄不對：%+v", record)
	}
	wantTarballURL := registry.server.URL + "/openclaw/-/openclaw-2026.9.2.tgz"
	if record.TarballURL != wantTarballURL {
		t.Fatalf("tarball_url = %q，預期 %q", record.TarballURL, wantTarballURL)
	}
	tgzPath := filepath.Join(dir, wantSHA256+".tgz")
	gotTarball, err := os.ReadFile(tgzPath)
	if err != nil || !bytes.Equal(gotTarball, tarball) {
		t.Fatalf("落地 tarball 不對：%q err=%v", gotTarball, err)
	}
	var sidecar artifactSidecar
	sidecarBytes, err := os.ReadFile(filepath.Join(dir, wantSHA256+".json"))
	if err != nil || json.Unmarshal(sidecarBytes, &sidecar) != nil || sidecar != record {
		t.Fatalf("落地 sidecar 不對：%s err=%v decoded=%+v", sidecarBytes, err, sidecar)
	}
	// sidecar 是人會 cat 來看的檔；engines 的 ">=" 不該被 encoding/json 轉成 \u003e=。
	if !strings.Contains(string(sidecarBytes), `"engines_node":">=22.22.3`) {
		t.Fatalf("sidecar 把 > 轉義了：%s", sidecarBytes)
	}
	before, err := os.Stat(tgzPath)
	if err != nil {
		t.Fatal(err)
	}

	again, existed, err := fetchArtifact(context.Background(), dir, "openclaw@2026.9.2",
		registry.server.URL, defaultArtifactMaxBytes, "someone-else")
	if err != nil || !existed || again != record {
		t.Fatalf("第二次應直接回已經有了：record=%+v existed=%v err=%v", again, existed, err)
	}
	after, err := os.Stat(tgzPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("重抓改了 tarball mtime：before=%s after=%s", before.ModTime(), after.ModTime())
	}
	if registry.tarballHits.Load() != 1 {
		t.Fatalf("已經有了仍重抓 tarball；hits=%d", registry.tarballHits.Load())
	}
}

func TestFetchArtifactRejectsSHA512MismatchWithoutLeavingFiles(t *testing.T) {
	registry := newFakeNPMRegistry(t, []byte("downloaded"), sha512Integrity([]byte("different")))
	dir := filepath.Join(t.TempDir(), "artifacts")
	_, _, err := fetchArtifact(context.Background(), dir, "openclaw@2026.9.2",
		registry.server.URL, defaultArtifactMaxBytes, "tester")
	if err == nil || !strings.Contains(err.Error(), "sha512") || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("sha512 mismatch 錯誤不清楚：%v", err)
	}
	assertNoArtifactFiles(t, dir)
}

func TestFetchArtifactStopsAtMaxBytesWithoutLeavingFiles(t *testing.T) {
	tarball := bytes.Repeat([]byte("x"), 33)
	registry := newFakeNPMRegistry(t, tarball, sha512Integrity(tarball))
	dir := filepath.Join(t.TempDir(), "artifacts")
	_, _, err := fetchArtifact(context.Background(), dir, "openclaw@2026.9.2", registry.server.URL, 16, "tester")
	if err == nil || !strings.Contains(err.Error(), "max-bytes") {
		t.Fatalf("超過上限錯誤不清楚：%v", err)
	}
	assertNoArtifactFiles(t, dir)
}

func assertNoArtifactFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("拒收後留下檔案：%v", names)
	}
}

func TestServeArtifactRequiresAuthAndOnlyAcceptsLowercaseDigest(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	if err := os.MkdirAll(f.artifactsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("stored tarball")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(f.artifactsDir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	grantArtifactDownload(t, f, digest, int64(len(body)))
	outsidePath := filepath.Join(filepath.Dir(f.artifactsDir), "outside-secret")
	outsideBody := []byte("must not leak")
	if err := os.WriteFile(outsidePath, outsideBody, 0o600); err != nil {
		t.Fatal(err)
	}
	outsideSum := sha256.Sum256(outsideBody)
	outsideDigest := hex.EncodeToString(outsideSum[:])
	if err := os.Symlink(outsidePath, filepath.Join(f.artifactsDir, outsideDigest+".tgz")); err != nil {
		t.Fatal(err)
	}

	get := requestArtifact(t, f.mux, http.MethodGet, "/v1/artifacts/"+digest, f.machine.token)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), body) {
		t.Fatalf("GET artifact = %d %q", get.Code, get.Body.Bytes())
	}
	if get.Header().Get("Content-Length") != fmt.Sprint(len(body)) ||
		get.Header().Get("ETag") != `"`+digest+`"` || get.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("GET headers 不對：%v", get.Header())
	}
	head := requestArtifact(t, f.mux, http.MethodHead, "/v1/artifacts/"+digest, f.machine.token)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != fmt.Sprint(len(body)) ||
		head.Header().Get("ETag") != `"`+digest+`"` {
		t.Fatalf("HEAD 不對：status=%d headers=%v body=%q", head.Code, head.Header(), head.Body.Bytes())
	}
	unauthorized := requestArtifact(t, f.mux, http.MethodGet, "/v1/artifacts/"+digest, "")
	assertAPIError(t, unauthorized, http.StatusUnauthorized, model.ErrUnauthorized)
	other := enrollViaHTTP(t, f.mux, f.store, "machine-without-this-job")
	notGranted := requestArtifact(t, f.mux, http.MethodGet, "/v1/artifacts/"+digest, other.token)
	assertAPIError(t, notGranted, http.StatusNotFound, model.ErrArtifactNotFound)

	missing := requestArtifact(t, f.mux, http.MethodGet, "/v1/artifacts/"+strings.Repeat("0", 64), f.machine.token)
	assertAPIError(t, missing, http.StatusNotFound, model.ErrArtifactNotFound)
	symlink := requestArtifact(t, f.mux, http.MethodGet, "/v1/artifacts/"+outsideDigest, f.machine.token)
	assertAPIError(t, symlink, http.StatusNotFound, model.ErrArtifactNotFound)
	badPaths := []string{
		"/v1/artifacts/" + strings.Repeat("a", 63),
		"/v1/artifacts/" + strings.ToUpper(digest),
		"/v1/artifacts/..%2F..",
		"/v1/artifacts/..%2Foutside-secret",
		"/v1/artifacts/" + digest + "/outside-secret",
	}
	for _, path := range badPaths {
		rec := requestArtifact(t, f.mux, http.MethodGet, path, f.machine.token)
		if rec.Code != http.StatusNotFound {
			t.Errorf("不合法 path %q 回 %d，預期 404：%s", path, rec.Code, rec.Body.String())
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("must not leak")) {
			t.Errorf("不合法 path %q 讀到 artifacts_dir 外內容", path)
		}
	}
}

func grantArtifactDownload(t *testing.T, f jobsFixture, digest string, size int64) {
	t.Helper()
	if _, err := f.store.DB().Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id=?`, f.machine.id); err != nil {
		t.Fatal(err)
	}
	spec := fmt.Sprintf(`{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":%q,"size":%d,"url":%q}}`,
		digest, size, "/v1/artifacts/"+digest)
	if _, _, err := f.store.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: spec,
		BatchSize: 1, CreatedBy: "test", Targets: []store.NewDeploymentTarget{{MachineID: f.machine.id, BatchNo: 1}},
		Job: store.NewJob{ArtifactDigest: "sha256:" + digest},
	}); err != nil {
		t.Fatal(err)
	}
}

func requestArtifact(t *testing.T, mux *http.ServeMux, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	mux.ServeHTTP(rec, req)
	return rec
}
