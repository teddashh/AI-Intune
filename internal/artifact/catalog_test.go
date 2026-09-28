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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingCatalogHashReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingCatalogHashReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	p[0] = 'x'
	return 1, nil
}

func catalogTestRecord(t *testing.T, dir, version string, body []byte, writeTarball bool) Sidecar {
	t.Helper()
	digestSum := sha256.Sum256(body)
	digest := hex.EncodeToString(digestSum[:])
	integritySum := sha512.Sum512(body)
	record := Sidecar{
		Name: "openclaw", Version: version,
		TarballURL:      "https://registry.example/openclaw/-/openclaw-" + version + ".tgz?private=RAW_URL_SECRET",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(integritySum[:]),
		SHA256:          digest, Size: int64(len(body)), EnginesNode: ">=24.15.0 <25",
		FetchedAt: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC), FetchedBy: "RAW_FETCHED_BY_SECRET",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if writeTarball {
		if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return record
}

func catalogEntriesByID(entries []CatalogEntry) map[string]CatalogEntry {
	result := make(map[string]CatalogEntry, len(entries))
	for _, entry := range entries {
		result[entry.ID] = entry
	}
	return result
}

func TestScanCatalogIsolatesMalformedMissingAndOrphanEntriesWithoutHashing(t *testing.T) {
	dir := t.TempDir()
	available := catalogTestRecord(t, dir, "2026.9.1", []byte("expected bytes"), true)
	// Same size, different content: metadata scan cannot truthfully call this
	// ready and must not hash a potentially 1 GiB artifact on every list.
	if err := os.WriteFile(filepath.Join(dir, available.SHA256+".tgz"), []byte("tampered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := catalogTestRecord(t, dir, "2026.9.2", []byte("missing tarball"), false)
	malformedDigest := strings.Repeat("c", 64)
	if err := os.WriteFile(filepath.Join(dir, malformedDigest+".json"), []byte(`{"name":`), 0o600); err != nil {
		t.Fatal(err)
	}
	orphanDigest := strings.Repeat("d", 64)
	if err := os.WriteFile(filepath.Join(dir, orphanDigest+".tgz"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "RAW_PRIVATE_FILENAME.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := ScanCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("entries=%+v", entries)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].ID >= entries[i].ID {
			t.Fatalf("catalog order is not canonical: %+v", entries)
		}
	}
	byID := catalogEntriesByID(entries)
	if got := byID[available.SHA256]; got.Status != CatalogAvailableUnverified || got.Issue != "" || got.Record == nil {
		t.Fatalf("same-size artifact list status=%+v", got)
	}
	if got := byID[missing.SHA256]; got.Status != CatalogUnavailable || got.Issue != CatalogIssueTarballMissing || got.Record == nil {
		t.Fatalf("missing tarball=%+v", got)
	}
	if got := byID[malformedDigest]; got.Status != CatalogInvalid || got.Issue != CatalogIssueSidecarInvalidJSON || got.Record != nil {
		t.Fatalf("malformed sidecar=%+v", got)
	}
	if got := byID[orphanDigest]; got.Status != CatalogUnavailable || got.Issue != CatalogIssueSidecarMissing || got.Record != nil {
		t.Fatalf("orphan tarball=%+v", got)
	}
	invalidFilename := 0
	for _, entry := range entries {
		if entry.Issue != CatalogIssueSidecarFilenameInvalid {
			continue
		}
		invalidFilename++
		if strings.Contains(entry.ID, "RAW_PRIVATE_FILENAME") || !strings.HasPrefix(entry.ID, "invalid-") {
			t.Fatalf("invalid filename leaked through catalog ID: %+v", entry)
		}
	}
	if invalidFilename != 1 {
		t.Fatalf("invalid filename entries=%d", invalidFilename)
	}
}

func TestInspectCatalogEntryPerformsFullSHA256AndReportsTruthfulStatus(t *testing.T) {
	dir := t.TempDir()
	body := []byte("full verification bytes")
	record := catalogTestRecord(t, dir, "2026.9.8", body, true)

	entry, err := InspectCatalogEntry(context.Background(), dir, record.SHA256)
	if err != nil || entry.Status != CatalogReady || entry.Issue != "" || entry.Record == nil {
		t.Fatalf("ready detail=%+v err=%v", entry, err)
	}

	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), bytes.Repeat([]byte{'x'}, len(body)), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err = InspectCatalogEntry(context.Background(), dir, record.SHA256)
	if err != nil || entry.Status != CatalogInvalid || entry.Issue != CatalogIssueTarballDigestMismatch {
		t.Fatalf("digest drift detail=%+v err=%v", entry, err)
	}

	if err := os.Remove(filepath.Join(dir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	entry, err = InspectCatalogEntry(context.Background(), dir, record.SHA256)
	if err != nil || entry.Status != CatalogUnavailable || entry.Issue != CatalogIssueTarballMissing {
		t.Fatalf("missing detail=%+v err=%v", entry, err)
	}
	if _, err := InspectCatalogEntry(context.Background(), dir, strings.Repeat("f", 64)); !errors.Is(err, ErrCatalogEntryNotFound) {
		t.Fatalf("missing ID err=%v", err)
	}
	if _, err := InspectCatalogEntry(context.Background(), dir, "../private"); !errors.Is(err, ErrCatalogEntryNotFound) {
		t.Fatalf("unsafe ID err=%v", err)
	}
}

func TestHashCatalogReaderStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &blockingCatalogHashReader{started: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := hashCatalogReader(ctx, reader)
		result <- err
	}()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("hash did not begin reading")
	}
	cancel()
	close(reader.release)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("hash cancellation err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("hash did not stop after context cancellation")
	}
}

func TestCatalogHashPermitIsContextAwareAndPerCanonicalDirectory(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "catalog-alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	release, err := acquireCatalogHashPermit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}

	otherRelease, err := acquireCatalogHashPermit(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("independent catalog permit: %v", err)
	}
	otherRelease()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if waiterRelease, acquireErr := acquireCatalogHashPermit(ctx, alias); waiterRelease != nil ||
		!errors.Is(acquireErr, context.DeadlineExceeded) {
		t.Fatalf("same-directory waiter release=%v err=%v", waiterRelease != nil, acquireErr)
	}

	// A canceled waiter must not consume or leak the only permit.
	release()
	reusedRelease, err := acquireCatalogHashPermit(context.Background(), alias)
	if err != nil {
		t.Fatalf("reacquire canceled waiter permit: %v", err)
	}
	reusedRelease()
}

func TestInspectCatalogEntryRejectsCanceledRequest(t *testing.T) {
	dir := t.TempDir()
	record := catalogTestRecord(t, dir, "2026.9.10", []byte("canceled detail"), true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectCatalogEntry(ctx, dir, record.SHA256); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled detail err=%v", err)
	}
}

func TestScanCatalogStrictSidecarSchemaAndNoFollow(t *testing.T) {
	baseDir := t.TempDir()
	body := []byte("schema fixture")
	record := catalogTestRecord(t, baseDir, "2026.9.8", body, true)
	validRaw, err := os.ReadFile(filepath.Join(baseDir, record.SHA256+".json"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		raw   []byte
		issue CatalogIssue
	}{
		{name: "unknown field", raw: []byte(strings.TrimSuffix(string(validRaw), "}") + `,"private_path":"/secret"}`), issue: CatalogIssueSidecarInvalidSchema},
		{name: "missing field", raw: []byte(strings.Replace(string(validRaw), `,"fetched_by":"RAW_FETCHED_BY_SECRET"`, "", 1)), issue: CatalogIssueSidecarInvalidSchema},
		{name: "duplicate field", raw: []byte(strings.TrimSuffix(string(validRaw), "}") + `,"version":"other"}`), issue: CatalogIssueSidecarInvalidJSON},
		{name: "case alias", raw: []byte(strings.Replace(string(validRaw), `"name":`, `"Name":`, 1)), issue: CatalogIssueSidecarInvalidSchema},
		{name: "trailing document", raw: append(append([]byte{}, validRaw...), []byte(` {}`)...), issue: CatalogIssueSidecarInvalidJSON},
		{name: "invalid integrity", raw: []byte(strings.Replace(string(validRaw), record.SHA512Integrity, "sha512-not-base64", 1)), issue: CatalogIssueSidecarInvalidMetadata},
		{name: "userinfo URL", raw: []byte(strings.Replace(string(validRaw), "https://registry.example/", "https://user:password@registry.example/", 1)), issue: CatalogIssueSidecarInvalidMetadata},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, record.SHA256+".json"), test.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			entries, err := ScanCatalog(dir)
			if err != nil || len(entries) != 1 || entries[0].Status != CatalogInvalid || entries[0].Issue != test.issue {
				t.Fatalf("entries=%+v err=%v", entries, err)
			}
		})
	}

	t.Run("sidecar symlink", func(t *testing.T) {
		dir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, validRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, record.SHA256+".json")); err != nil {
			t.Fatal(err)
		}
		entries, err := ScanCatalog(dir)
		if err != nil || len(entries) != 1 || entries[0].Issue != CatalogIssueSidecarNotRegular {
			t.Fatalf("entries=%+v err=%v", entries, err)
		}
	})

	t.Run("tarball symlink", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, record.SHA256+".json"), validRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.tgz")
		if err := os.WriteFile(outside, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, record.SHA256+".tgz")); err != nil {
			t.Fatal(err)
		}
		entries, err := ScanCatalog(dir)
		if err != nil || len(entries) != 1 || entries[0].Issue != CatalogIssueTarballNotRegular {
			t.Fatalf("entries=%+v err=%v", entries, err)
		}
	})
}

func TestScanCatalogMissingDirectoryIsEmptyAndRejectsInvalidDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	entries, err := ScanCatalog(missing)
	if err != nil || entries == nil || len(entries) != 0 {
		t.Fatalf("missing catalog entries=%+v err=%v", entries, err)
	}
	for _, dir := range []string{"", " " + missing} {
		if _, err := ScanCatalog(dir); err == nil {
			t.Fatalf("invalid directory %q accepted", dir)
		}
	}
}

func TestCatalogSidecarArtifactSizeBounds(t *testing.T) {
	dir := t.TempDir()
	record := catalogTestRecord(t, dir, "2026.9.8", []byte("size fixture"), false)
	record.Size = maxCatalogArtifactBytes
	if !validCatalogSidecar(record, record.SHA256) {
		t.Fatal("exact 1 GiB sidecar size was rejected")
	}
	for _, size := range []int64{0, -1, maxCatalogArtifactBytes + 1} {
		record.Size = size
		if validCatalogSidecar(record, record.SHA256) {
			t.Fatalf("out-of-policy artifact size %d was accepted", size)
		}
	}
}

func TestCatalogTarballURLByteBoundMatchesFetcher(t *testing.T) {
	dir := t.TempDir()
	record := catalogTestRecord(t, dir, "2026.9.9", []byte("url bound fixture"), false)
	prefix := "https://registry.example/"
	record.TarballURL = prefix + strings.Repeat("a", MaxTarballURLBytes-len(prefix))
	if len(record.TarballURL) != MaxTarballURLBytes || !validCatalogSidecar(record, record.SHA256) {
		t.Fatalf("catalog rejected %d-byte tarball URL", len(record.TarballURL))
	}
	record.TarballURL += "a"
	if validCatalogSidecar(record, record.SHA256) {
		t.Fatalf("catalog accepted %d-byte tarball URL", len(record.TarballURL))
	}
}
