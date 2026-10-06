// Package blobstore stores Hub-measured blob bytes.
//
// SQLite keeps the digest, size, and object key. This package never treats an
// agent-supplied digest as proof: Put reads the bytes and rejects them when
// the SHA-256 does not match. When no remote backend is configured, Hub keeps
// using the local artifacts directory and does not call this package.
package blobstore

import (
	"context"
	"errors"
	"io"
)

const (
	// MaxBytes matches the artifact fetch ceiling (1 GiB).
	MaxBytes int64 = 1 << 30

	KindArtifact = "artifact"
	KindEvidence = "evidence"

	backendDir    = "dir"
	backendMemory = "memory"
	backendR2     = "r2"
	backendS3     = "s3"
)

// Object is the identity of bytes this process has hashed.
type Object struct {
	Digest    string
	Size      int64
	Key       string
	MediaType string
	Backend   string
}

// Backend is a content-addressed blob store. Keys are always blobs/<digest>.
type Backend interface {
	Name() string
	Put(ctx context.Context, digest, mediaType string, size int64, r io.Reader) (Object, error)
	Open(ctx context.Context, digest string) (io.ReadCloser, Object, error)
}

var (
	ErrInvalid        = errors.New("blobstore: invalid object")
	ErrDigestMismatch = errors.New("blobstore: bytes do not match digest")
	ErrNotFound       = errors.New("blobstore: object not found")
	ErrConfig         = errors.New("blobstore: configuration is incomplete")
)

// ObjectKey is the only key shape stored in SQLite and in a backend.
func ObjectKey(digest string) string {
	return "blobs/" + digest
}

// ValidDigest reports whether digest is 64 lowercase hex characters.
func ValidDigest(digest string) bool {
	if len(digest) != sha256HexLen {
		return false
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

const sha256HexLen = 64
