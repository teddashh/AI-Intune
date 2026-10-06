package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
)

func validatePut(digest, mediaType string, size int64) error {
	if !ValidDigest(digest) || size <= 0 || size > MaxBytes || !validMediaType(mediaType) {
		return ErrInvalid
	}
	return nil
}

func validMediaType(mediaType string) bool {
	if mediaType == "" || len(mediaType) > 128 || mediaType != strings.TrimSpace(mediaType) {
		return false
	}
	for i := 0; i < len(mediaType); i++ {
		if mediaType[i] < 0x20 || mediaType[i] == 0x7f {
			return false
		}
	}
	return true
}

// readVerified reads exactly size bytes and requires their SHA-256 to be digest.
// The returned reader is positioned at the start of a temp file the caller must close.
func readVerified(r io.Reader, size int64, digest string) (*os.File, error) {
	if err := validatePut(digest, "application/octet-stream", size); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "clawctl-blob-*")
	if err != nil {
		return nil, ErrInvalid
	}
	keep := false
	defer func() {
		if !keep {
			name := tmp.Name()
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return nil, ErrInvalid
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(r, size+1))
	if err != nil {
		return nil, ErrInvalid
	}
	if n != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return nil, ErrDigestMismatch
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, ErrInvalid
	}
	keep = true
	return tmp, nil
}

func objectFor(backend, digest, mediaType string, size int64) Object {
	return Object{
		Digest: digest, Size: size, Key: ObjectKey(digest),
		MediaType: mediaType, Backend: backend,
	}
}
