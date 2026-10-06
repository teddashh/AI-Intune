package blobstore

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// Memory is an in-process backend for tests. It does not open a network connection.
type Memory struct {
	mu    sync.Mutex
	items map[string]memItem
}

type memItem struct {
	body  []byte
	media string
}

// NewMemory returns an empty backend named "memory".
func NewMemory() *Memory {
	return &Memory{items: map[string]memItem{}}
}

func (m *Memory) Name() string { return backendMemory }

func (m *Memory) Put(ctx context.Context, digest, mediaType string, size int64, r io.Reader) (Object, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	if err := validatePut(digest, mediaType, size); err != nil {
		return Object{}, err
	}
	body, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return Object{}, ErrInvalid
	}
	if int64(len(body)) != size {
		return Object{}, ErrDigestMismatch
	}
	verified, err := readVerified(bytes.NewReader(body), size, digest)
	if err != nil {
		return Object{}, err
	}
	defer verified.Close()
	_ = osRemove(verified.Name())
	stored := append([]byte(nil), body...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[digest] = memItem{body: stored, media: mediaType}
	return objectFor(m.Name(), digest, mediaType, size), nil
}

func (m *Memory) Open(ctx context.Context, digest string) (io.ReadCloser, Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, Object{}, err
	}
	if !ValidDigest(digest) {
		return nil, Object{}, ErrInvalid
	}
	m.mu.Lock()
	item, ok := m.items[digest]
	m.mu.Unlock()
	if !ok {
		return nil, Object{}, ErrNotFound
	}
	obj := objectFor(m.Name(), digest, item.media, int64(len(item.body)))
	return io.NopCloser(bytes.NewReader(item.body)), obj, nil
}

// osRemove is split so memory.go does not need to document temp-file cleanup
// next to the map. readVerified already hashed the bytes; the temp is discarded.
func osRemove(name string) error {
	return removeFile(name)
}
