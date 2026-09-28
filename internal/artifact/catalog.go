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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// CatalogStatus separates a cheap catalog scan from proof over the artifact
// bytes. A list must never call matching path/type/size "ready": a 1 GiB
// tarball is hashed only when an operator opens its detail or starts a
// deployment preview/apply.
type CatalogStatus string

const (
	CatalogAvailableUnverified CatalogStatus = "available_unverified"
	CatalogReady               CatalogStatus = "ready"
	CatalogUnavailable         CatalogStatus = "unavailable"
	CatalogInvalid             CatalogStatus = "invalid"
)

// CatalogIssue is deliberately bounded. Filesystem paths, JSON parser input,
// and upstream URLs must not escape through an operator-facing projection.
type CatalogIssue string

const (
	CatalogIssueSidecarMissing         CatalogIssue = "sidecar_missing"
	CatalogIssueSidecarFilenameInvalid CatalogIssue = "sidecar_filename_invalid"
	CatalogIssueSidecarNotRegular      CatalogIssue = "sidecar_not_regular"
	CatalogIssueSidecarUnreadable      CatalogIssue = "sidecar_unreadable"
	CatalogIssueSidecarTooLarge        CatalogIssue = "sidecar_too_large"
	CatalogIssueSidecarInvalidJSON     CatalogIssue = "sidecar_invalid_json"
	CatalogIssueSidecarInvalidSchema   CatalogIssue = "sidecar_invalid_schema"
	CatalogIssueSidecarInvalidMetadata CatalogIssue = "sidecar_invalid_metadata"
	CatalogIssueTarballMissing         CatalogIssue = "tarball_missing"
	CatalogIssueTarballNotRegular      CatalogIssue = "tarball_not_regular"
	CatalogIssueTarballSizeMismatch    CatalogIssue = "tarball_size_mismatch"
	CatalogIssueTarballDigestMismatch  CatalogIssue = "tarball_digest_mismatch"
	CatalogIssueTarballChanged         CatalogIssue = "tarball_changed"
	CatalogIssueTarballUnreadable      CatalogIssue = "tarball_unreadable"
)

const (
	maxCatalogSidecarBytes  int64 = 64 << 10
	maxCatalogArtifactBytes int64 = 1 << 30
	// Full hashing is deliberately serialized per catalog directory. A Hub may
	// serve several independent catalogs in tests or future multi-tenant
	// adapters, but one catalog must not let concurrent detail requests multiply
	// disk I/O without bound.
	catalogHashConcurrencyPerDirectory = 1
)

var ErrCatalogEntryNotFound = errors.New("artifact: catalog entry not found")

type catalogHashLimiter struct {
	permits chan struct{}
}

var catalogHashLimiters sync.Map

// CatalogEntry is an internal catalog fact. Record contains private source
// provenance and therefore must never be serialized directly by an operator
// adapter; the operator package projects an explicit allowlist instead.
type CatalogEntry struct {
	ID     string
	SHA256 string
	Record *Sidecar
	Status CatalogStatus
	Issue  CatalogIssue
}

type catalogFiles struct {
	id          string
	digest      string
	sidecarName string
	tarballName string
	forcedIssue CatalogIssue
}

// ScanCatalog performs bounded, no-follow metadata inspection only. It keeps
// one malformed sidecar or orphan tarball as an individual entry instead of
// failing the whole catalog. The only global errors are inability to enumerate
// the configured catalog directory itself.
func ScanCatalog(dir string) ([]CatalogEntry, error) {
	if strings.TrimSpace(dir) == "" || dir != strings.TrimSpace(dir) {
		return nil, errors.New("artifact: catalog directory is required")
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []CatalogEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("artifact: enumerate catalog: %w", err)
	}

	grouped := make(map[string]*catalogFiles)
	invalidSidecars := make([]catalogFiles, 0)
	for _, entry := range entries {
		name := entry.Name()
		switch filepath.Ext(name) {
		case ".json":
			digest := strings.TrimSuffix(name, ".json")
			if ValidSHA256Hex(digest) {
				files := grouped[digest]
				if files == nil {
					files = &catalogFiles{id: digest, digest: digest}
					grouped[digest] = files
				}
				files.sidecarName = name
				continue
			}
			// Hash the directory entry name into a stable, non-sensitive ID. The
			// filename itself might contain a credential or local path fragment.
			sum := sha256.Sum256([]byte(name))
			invalidSidecars = append(invalidSidecars, catalogFiles{
				id: "invalid-" + hex.EncodeToString(sum[:]), sidecarName: name,
				forcedIssue: CatalogIssueSidecarFilenameInvalid,
			})
		case ".tgz":
			digest := strings.TrimSuffix(name, ".tgz")
			if !ValidSHA256Hex(digest) {
				continue
			}
			files := grouped[digest]
			if files == nil {
				files = &catalogFiles{id: digest, digest: digest}
				grouped[digest] = files
			}
			files.tarballName = name
		}
	}

	files := make([]catalogFiles, 0, len(grouped)+len(invalidSidecars))
	for _, group := range grouped {
		files = append(files, *group)
	}
	files = append(files, invalidSidecars...)
	sort.Slice(files, func(i, j int) bool { return files[i].id < files[j].id })

	result := make([]CatalogEntry, 0, len(files))
	for _, group := range files {
		result = append(result, scanCatalogEntry(dir, group))
	}
	return result, nil
}

// InspectCatalogEntry repeats the cheap scan to bind the requested ID to the
// current live catalog, then hashes matching bytes once through an opened,
// no-follow descriptor. It bounds full hashes per canonical catalog directory
// and stops reading as soon as ctx is canceled. Callers receive ready only
// after that full verification succeeds.
func InspectCatalogEntry(ctx context.Context, dir, id string) (CatalogEntry, error) {
	if ctx == nil {
		return CatalogEntry{}, errors.New("artifact: catalog inspection context is required")
	}
	if !validCatalogID(id) {
		return CatalogEntry{}, ErrCatalogEntryNotFound
	}
	if err := ctx.Err(); err != nil {
		return CatalogEntry{}, err
	}
	entries, err := ScanCatalog(dir)
	if err != nil {
		return CatalogEntry{}, err
	}
	for _, entry := range entries {
		if entry.ID != id {
			continue
		}
		if entry.Status != CatalogAvailableUnverified || entry.Record == nil {
			return entry, nil
		}
		issue, verifyErr := verifyCatalogArtifactWithPermit(ctx, dir, *entry.Record)
		if verifyErr != nil {
			return CatalogEntry{}, verifyErr
		}
		if issue == "" {
			entry.Status = CatalogReady
			entry.Issue = ""
			return entry, nil
		}
		entry.Issue = issue
		if issue == CatalogIssueTarballMissing {
			entry.Status = CatalogUnavailable
		} else {
			entry.Status = CatalogInvalid
		}
		return entry, nil
	}
	return CatalogEntry{}, ErrCatalogEntryNotFound
}

func verifyCatalogArtifactWithPermit(ctx context.Context, dir string, record Sidecar) (CatalogIssue, error) {
	release, err := acquireCatalogHashPermit(ctx, dir)
	if err != nil {
		return "", err
	}
	defer release()
	return verifyCatalogArtifactContext(ctx, dir, record)
}

func acquireCatalogHashPermit(ctx context.Context, dir string) (func(), error) {
	if ctx == nil {
		return nil, errors.New("artifact: catalog hash context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := catalogHashDirectoryKey(dir)
	value, _ := catalogHashLimiters.LoadOrStore(key, &catalogHashLimiter{
		permits: make(chan struct{}, catalogHashConcurrencyPerDirectory),
	})
	limiter := value.(*catalogHashLimiter)
	select {
	case limiter.permits <- struct{}{}:
		// A cancel and a released permit can become ready together. Recheck after
		// acquisition so canceled waiters never begin an expensive hash.
		if err := ctx.Err(); err != nil {
			<-limiter.permits
			return nil, err
		}
		var once sync.Once
		return func() {
			once.Do(func() { <-limiter.permits })
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func catalogHashDirectoryKey(dir string) string {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		absolute = filepath.Clean(dir)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(absolute)
}

func scanCatalogEntry(dir string, files catalogFiles) CatalogEntry {
	result := CatalogEntry{ID: files.id, SHA256: files.digest}
	if files.forcedIssue != "" {
		result.Status, result.Issue = CatalogInvalid, files.forcedIssue
		return result
	}
	if files.sidecarName == "" {
		// An orphan regular tarball is unavailable because it has no trusted
		// metadata binding. A symlink/device with a digest-looking name is an
		// invalid filesystem fact and must not be softened into "missing".
		info, err := os.Lstat(filepath.Join(dir, files.tarballName))
		switch {
		case errors.Is(err, os.ErrNotExist):
			result.Status, result.Issue = CatalogUnavailable, CatalogIssueSidecarMissing
		case err != nil:
			result.Status, result.Issue = CatalogInvalid, CatalogIssueTarballUnreadable
		case !info.Mode().IsRegular():
			result.Status, result.Issue = CatalogInvalid, CatalogIssueTarballNotRegular
		default:
			result.Status, result.Issue = CatalogUnavailable, CatalogIssueSidecarMissing
		}
		return result
	}
	raw, issue := readCatalogSidecar(filepath.Join(dir, files.sidecarName))
	if issue != "" {
		result.Status, result.Issue = CatalogInvalid, issue
		return result
	}
	record, issue := decodeCatalogSidecar(raw, files.digest)
	if issue != "" {
		result.Status, result.Issue = CatalogInvalid, issue
		return result
	}
	result.Record = &record
	if files.tarballName == "" {
		result.Status, result.Issue = CatalogUnavailable, CatalogIssueTarballMissing
		return result
	}
	info, err := os.Lstat(filepath.Join(dir, files.tarballName))
	if errors.Is(err, os.ErrNotExist) {
		result.Status, result.Issue = CatalogUnavailable, CatalogIssueTarballMissing
		return result
	}
	if err != nil {
		result.Status, result.Issue = CatalogInvalid, CatalogIssueTarballUnreadable
		return result
	}
	if !info.Mode().IsRegular() {
		result.Status, result.Issue = CatalogInvalid, CatalogIssueTarballNotRegular
		return result
	}
	if info.Size() != record.Size {
		result.Status, result.Issue = CatalogInvalid, CatalogIssueTarballSizeMismatch
		return result
	}
	result.Status = CatalogAvailableUnverified
	return result
}

func readCatalogSidecar(path string) ([]byte, CatalogIssue) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, CatalogIssueSidecarUnreadable
	}
	if !before.Mode().IsRegular() {
		return nil, CatalogIssueSidecarNotRegular
	}
	if before.Size() > maxCatalogSidecarBytes {
		return nil, CatalogIssueSidecarTooLarge
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, CatalogIssueSidecarUnreadable
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, CatalogIssueSidecarUnreadable
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() {
		return nil, CatalogIssueSidecarUnreadable
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCatalogSidecarBytes+1))
	if err != nil {
		return nil, CatalogIssueSidecarUnreadable
	}
	if int64(len(raw)) > maxCatalogSidecarBytes {
		return nil, CatalogIssueSidecarTooLarge
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != opened.Size() {
		return nil, CatalogIssueSidecarUnreadable
	}
	return raw, ""
}

func decodeCatalogSidecar(raw []byte, filenameDigest string) (Sidecar, CatalogIssue) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || rejectCatalogDuplicateFields(raw) != nil {
		return Sidecar{}, CatalogIssueSidecarInvalidJSON
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		return Sidecar{}, CatalogIssueSidecarInvalidJSON
	}
	wanted := map[string]bool{
		"name": true, "version": true, "tarball_url": true, "sha512_integrity": true,
		"sha256": true, "size": true, "engines_node": true, "fetched_at": true, "fetched_by": true,
	}
	if len(shape) != len(wanted) {
		return Sidecar{}, CatalogIssueSidecarInvalidSchema
	}
	for name := range shape {
		if !wanted[name] {
			return Sidecar{}, CatalogIssueSidecarInvalidSchema
		}
	}
	var record Sidecar
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Sidecar{}, CatalogIssueSidecarInvalidJSON
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Sidecar{}, CatalogIssueSidecarInvalidJSON
	}
	if !validCatalogSidecar(record, filenameDigest) {
		return Sidecar{}, CatalogIssueSidecarInvalidMetadata
	}
	return record, ""
}

func validCatalogSidecar(record Sidecar, filenameDigest string) bool {
	parsed, err := url.Parse(record.TarballURL)
	return validCatalogText(record.Name, 128, false) && validCatalogText(record.Version, 128, false) &&
		record.SHA256 == filenameDigest && ValidSHA256Hex(record.SHA256) &&
		record.Size > 0 && record.Size <= maxCatalogArtifactBytes &&
		validCatalogText(record.EnginesNode, 512, true) && validCatalogText(record.FetchedBy, 256, true) &&
		validCatalogText(record.TarballURL, MaxTarballURLBytes, false) && validCatalogText(record.SHA512Integrity, 1024, false) &&
		!record.FetchedAt.IsZero() && record.FetchedAt.Location() == time.UTC &&
		err == nil && parsed.Host != "" && parsed.User == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") &&
		validSHA512Integrity(record.SHA512Integrity)
}

func validCatalogText(value string, maxBytes int, allowEmpty bool) bool {
	if len(value) > maxBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) || (!allowEmpty && value == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func validSHA512Integrity(value string) bool {
	encoded, ok := strings.CutPrefix(value, "sha512-")
	if !ok || encoded == "" {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	return err == nil && len(decoded) == sha512.Size
}

func verifyCatalogArtifactContext(ctx context.Context, dir string, record Sidecar) (CatalogIssue, error) {
	if ctx == nil {
		return "", errors.New("artifact: catalog hash context is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := filepath.Join(dir, record.SHA256+".tgz")
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return CatalogIssueTarballMissing, nil
	}
	if err != nil {
		return CatalogIssueTarballUnreadable, nil
	}
	if !before.Mode().IsRegular() {
		return CatalogIssueTarballNotRegular, nil
	}
	if before.Size() != record.Size {
		return CatalogIssueTarballSizeMismatch, nil
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return CatalogIssueTarballUnreadable, nil
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return CatalogIssueTarballUnreadable, nil
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != record.Size {
		return CatalogIssueTarballChanged, nil
	}
	digest, err := hashCatalogReader(ctx, f)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return CatalogIssueTarballUnreadable, nil
	}
	if digest != record.SHA256 {
		return CatalogIssueTarballDigestMismatch, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != record.Size {
		return CatalogIssueTarballChanged, nil
	}
	return "", nil
}

func hashCatalogReader(ctx context.Context, source io.Reader) (string, error) {
	if ctx == nil || source == nil {
		return "", errors.New("artifact: catalog hash input is required")
	}
	h := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			_, _ = h.Write(buffer[:n])
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				return hex.EncodeToString(h.Sum(nil)), nil
			}
			return "", readErr
		}
		if n == 0 {
			return "", io.ErrNoProgress
		}
	}
}

func validCatalogID(id string) bool {
	if ValidSHA256Hex(id) {
		return true
	}
	digest, ok := strings.CutPrefix(id, "invalid-")
	return ok && ValidSHA256Hex(digest)
}

func rejectCatalogDuplicateFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("sidecar is not an object")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return errors.New("duplicate or invalid sidecar field")
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return errors.New("sidecar object is not closed")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("sidecar has trailing JSON")
	}
	return nil
}
