package blobstore

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Dir stores blobs as regular files under root/blobs/<digest>.
type Dir struct {
	root string
}

// NewDir creates a local backend. root must be an absolute directory path.
func NewDir(root string) (*Dir, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, ErrInvalid
	}
	clean := filepath.Clean(root)
	if clean != root || filepath.Dir(clean) == clean {
		return nil, ErrInvalid
	}
	blobs := filepath.Join(clean, "blobs")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		return nil, ErrInvalid
	}
	return &Dir{root: clean}, nil
}

func (d *Dir) Name() string { return backendDir }

func (d *Dir) Put(ctx context.Context, digest, mediaType string, size int64, r io.Reader) (Object, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	verified, err := readVerified(r, size, digest)
	if err != nil {
		return Object{}, err
	}
	defer func() {
		name := verified.Name()
		_ = verified.Close()
		_ = os.Remove(name)
	}()
	if err := validatePut(digest, mediaType, size); err != nil {
		return Object{}, err
	}
	dest := filepath.Join(d.root, "blobs", digest)
	tmp, err := os.CreateTemp(filepath.Join(d.root, "blobs"), ".put-*")
	if err != nil {
		return Object{}, ErrInvalid
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return Object{}, ErrInvalid
	}
	if _, err := io.Copy(tmp, verified); err != nil {
		return Object{}, ErrInvalid
	}
	if err := tmp.Sync(); err != nil {
		return Object{}, ErrInvalid
	}
	if err := tmp.Close(); err != nil {
		return Object{}, ErrInvalid
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return Object{}, ErrInvalid
	}
	keep = true
	return objectFor(d.Name(), digest, mediaType, size), nil
}

func (d *Dir) Open(ctx context.Context, digest string) (io.ReadCloser, Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, Object{}, err
	}
	if !ValidDigest(digest) {
		return nil, Object{}, ErrInvalid
	}
	path := filepath.Join(d.root, "blobs", digest)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, Object{}, ErrNotFound
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, Object{}, ErrNotFound
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxBytes {
		_ = f.Close()
		return nil, Object{}, ErrNotFound
	}
	return f, objectFor(d.Name(), digest, "application/octet-stream", info.Size()), nil
}

func removeFile(name string) error { return os.Remove(name) }
