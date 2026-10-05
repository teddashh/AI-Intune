package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func TestMemoryAndDirRoundTrip(t *testing.T) {
	body := []byte("hub measured these bytes")
	digest := digestOf(body)
	dir, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []Backend{NewMemory(), dir} {
		obj, err := backend.Put(context.Background(), digest, "application/gzip", int64(len(body)), bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%s put: %v", backend.Name(), err)
		}
		if obj.Key != "blobs/"+digest || obj.Size != int64(len(body)) || obj.Backend != backend.Name() {
			t.Fatalf("%s object: %+v", backend.Name(), obj)
		}
		rc, got, err := backend.Open(context.Background(), digest)
		if err != nil {
			t.Fatalf("%s open: %v", backend.Name(), err)
		}
		read, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read, body) || got.Size != int64(len(body)) {
			t.Fatalf("%s bytes = %q size %d", backend.Name(), read, got.Size)
		}
		if _, err := backend.Put(context.Background(), digest, "application/gzip", int64(len(body)), bytes.NewReader([]byte("nope"))); !errors.Is(err, ErrDigestMismatch) {
			t.Fatalf("%s mismatch: %v", backend.Name(), err)
		}
	}
}

func TestPutRejectsInvalidIdentity(t *testing.T) {
	mem := NewMemory()
	_, err := mem.Put(context.Background(), "abc", "text/plain", 1, strings.NewReader("x"))
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("short digest: %v", err)
	}
	_, _, err = mem.Open(context.Background(), strings.Repeat("a", 64))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestKnownSigV4Vector(t *testing.T) {
	client, err := newS3(s3Config{
		name: backendS3, endpoint: "https://examplebucket.s3.amazonaws.com",
		bucket: "examplebucket", region: "us-east-1",
		accessKey: "AKIAIOSFODNN7EXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	client.sign(req, emptyPayloadHash, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	auth := req.Header.Get("Authorization")
	const want = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if !strings.Contains(auth, "Signature="+want) {
		canonical := canonicalRequest(req, []string{"host", "range", "x-amz-content-sha256", "x-amz-date"}, emptyPayloadHash)
		t.Fatalf("authorization = %s\ncanonical:\n%s", auth, canonical)
	}
}

func TestS3RoundTripUsesPathStyleAndRealPayloadHash(t *testing.T) {
	body := []byte("evidence-bytes")
	digest := digestOf(body)
	var gotPath, gotHash, gotAuth string
	var stored []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHash = r.Header.Get("x-amz-content-sha256")
		gotAuth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodPut:
			stored, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Length", "14")
			_, _ = w.Write(stored)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client, err := newS3(s3Config{
		name: backendR2, endpoint: srv.URL, bucket: "clawctl-artifacts", region: "auto",
		accessKey: "access-key", secretKey: "secret-key", allowHTTP: true, client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Put(context.Background(), digest, "application/gzip", int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/clawctl-artifacts/blobs/"+digest || gotHash != digest || !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("put path=%s hash=%s auth=%s", gotPath, gotHash, gotAuth)
	}
	if strings.Contains(gotAuth, "secret-key") {
		t.Fatal("authorization contains the secret")
	}
	rc, obj, err := client.Open(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(read, body) || obj.Size != int64(len(body)) || obj.Backend != backendR2 {
		t.Fatalf("open = %q %+v", read, obj)
	}
}

func TestFromEnv(t *testing.T) {
	account := strings.Repeat("ab", 16)
	secret := "super-secret-value"
	endpoint := "https://" + account + ".r2.cloudflarestorage.com"
	full := map[string]string{
		"R2_ACCESS_KEY_ID":     "access-key",
		"R2_SECRET_ACCESS_KEY": secret,
		"R2_BUCKET":            "clawctl-artifacts",
		"R2_ENDPOINT":          endpoint,
		"R2_ACCOUNT_ID":        account,
	}
	getenv := func(env map[string]string) func(string) string {
		return func(key string) string { return env[key] }
	}
	backend, summary, err := FromEnv(getenv(map[string]string{}))
	if err != nil || backend != nil || summary != "" {
		t.Fatalf("empty = %v %q %v", backend, summary, err)
	}
	_, _, err = FromEnv(getenv(map[string]string{"R2_BUCKET": "clawctl-artifacts", "R2_SECRET_ACCESS_KEY": secret}))
	if err == nil || !errors.Is(err, ErrConfig) || strings.Contains(err.Error(), secret) {
		t.Fatalf("partial: %v", err)
	}
	both := map[string]string{"R2_BUCKET": "a", "S3_BUCKET": "b", "S3_SECRET_ACCESS_KEY": secret}
	_, _, err = FromEnv(getenv(both))
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "S3_SECRET") {
		t.Fatalf("both: %v", err)
	}
	backend, summary, err = FromEnv(getenv(full))
	if err != nil {
		t.Fatal(err)
	}
	if backend.Name() != backendR2 || !strings.Contains(summary, "bucket=clawctl-artifacts") || !strings.Contains(summary, "endpoint="+account+".r2.cloudflarestorage.com") || strings.Contains(summary, secret) {
		t.Fatalf("summary = %q name %s", summary, backend.Name())
	}
	mismatch := map[string]string{}
	for k, v := range full {
		mismatch[k] = v
	}
	mismatch["R2_ENDPOINT"] = "https://other.r2.cloudflarestorage.com"
	_, _, err = FromEnv(getenv(mismatch))
	if err == nil || strings.Contains(err.Error(), account) || strings.Contains(err.Error(), secret) {
		t.Fatalf("account mismatch: %v", err)
	}
	s3env := map[string]string{
		"S3_ACCESS_KEY_ID": "access-key", "S3_SECRET_ACCESS_KEY": secret,
		"S3_BUCKET": "artifacts", "S3_ENDPOINT": "https://s3.example.com",
	}
	backend, summary, err = FromEnv(getenv(s3env))
	if err != nil || backend.Name() != backendS3 || !strings.Contains(summary, "endpoint=s3.example.com") || strings.Contains(summary, secret) {
		t.Fatalf("s3 = %s %q %v", backendName(backend), summary, err)
	}
}

func backendName(b Backend) string {
	if b == nil {
		return ""
	}
	return b.Name()
}

func TestDirRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	dir, err := NewDir(root)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("regular")
	digest := digestOf(body)
	if _, err := dir.Put(context.Background(), digest, "application/octet-stream", int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	path := root + "/blobs/" + digest
	if err := os.Rename(path, path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".real", path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dir.Open(context.Background(), digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("symlink open: %v", err)
	}
}
