// Package objectref publishes Hub-measured bytes to a blob backend and records
// the digest in SQLite. It does not accept an agent-supplied digest as proof.
package objectref

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/teddashh/AI-Intune/internal/blobstore"
	"github.com/teddashh/AI-Intune/internal/store"
	"golang.org/x/sys/unix"
)

// ObjectLedger is the SQLite side of a published blob. The store keeps the
// digest, size, and object key. It does not store the bytes.
type ObjectLedger interface {
	UpsertObjectBlob(store.ObjectBlob) error
}

// Publisher mirrors a local artifact, or stores evidence bytes the Hub hashed
// itself. A nil Backend is not used by the worker; callers skip a nil publisher.
type Publisher struct {
	Backend blobstore.Backend
	Store   ObjectLedger
}

// PublishArtifact re-opens the local tarball, re-hashes it, and uploads that
// digest. The local file stays in place for catalog and deployment reads.
func (p Publisher) PublishArtifact(ctx context.Context, artifactsDir, digest string, size int64) error {
	if p.Backend == nil || p.Store == nil {
		return blobstore.ErrInvalid
	}
	if !blobstore.ValidDigest(digest) || size <= 0 || size > blobstore.MaxBytes || !filepath.IsAbs(artifactsDir) {
		return blobstore.ErrInvalid
	}
	path := filepath.Join(artifactsDir, digest+".tgz")
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return blobstore.ErrInvalid
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return blobstore.ErrInvalid
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return blobstore.ErrDigestMismatch
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, size+1))
	if err != nil || n != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return blobstore.ErrDigestMismatch
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return blobstore.ErrInvalid
	}
	obj, err := p.Backend.Put(ctx, digest, "application/gzip", size, f)
	if err != nil {
		return err
	}
	return p.Store.UpsertObjectBlob(store.ObjectBlob{
		Digest: obj.Digest, SizeBytes: obj.Size, ObjectKey: obj.Key,
		Backend: obj.Backend, Kind: blobstore.KindArtifact, MediaType: "application/gzip",
	})
}

// PublishEvidence hashes r itself. There is no caller-supplied digest.
func (p Publisher) PublishEvidence(ctx context.Context, r io.Reader, size int64, mediaType string) (blobstore.Object, error) {
	if p.Backend == nil || p.Store == nil || r == nil {
		return blobstore.Object{}, blobstore.ErrInvalid
	}
	if size <= 0 || size > blobstore.MaxBytes || !validEvidenceMedia(mediaType) {
		return blobstore.Object{}, blobstore.ErrInvalid
	}
	body, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil || int64(len(body)) != size {
		return blobstore.Object{}, blobstore.ErrDigestMismatch
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	obj, err := p.Backend.Put(ctx, digest, mediaType, size, bytes.NewReader(body))
	if err != nil {
		return blobstore.Object{}, err
	}
	if err := p.Store.UpsertObjectBlob(store.ObjectBlob{
		Digest: obj.Digest, SizeBytes: obj.Size, ObjectKey: obj.Key,
		Backend: obj.Backend, Kind: blobstore.KindEvidence, MediaType: mediaType,
	}); err != nil {
		return blobstore.Object{}, err
	}
	return obj, nil
}

func validEvidenceMedia(mediaType string) bool {
	if mediaType == "" || len(mediaType) > 128 || mediaType != trim(mediaType) {
		return false
	}
	for i := 0; i < len(mediaType); i++ {
		if mediaType[i] < 0x20 || mediaType[i] == 0x7f {
			return false
		}
	}
	return true
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
