package blobstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// S3 is a path-style S3-compatible client. R2 uses the same signer with
// region "auto". The process hashes every body before the request so
// x-amz-content-sha256 is the real digest, never UNSIGNED-PAYLOAD.
type S3 struct {
	name      string
	endpoint  *url.URL
	bucket    string
	region    string
	accessKey string
	secretKey string
	client    *http.Client
	now       func() time.Time
}

type s3Config struct {
	name      string
	endpoint  string
	bucket    string
	region    string
	accessKey string
	secretKey string
	allowHTTP bool
	client    *http.Client
	now       func() time.Time
}

func newS3(cfg s3Config) (*S3, error) {
	if cfg.name != backendR2 && cfg.name != backendS3 {
		return nil, ErrConfig
	}
	if !validBucket(cfg.bucket) || !validCredential(cfg.accessKey) || !validCredential(cfg.secretKey) || !validRegion(cfg.region) {
		return nil, ErrConfig
	}
	endpoint, err := parseEndpoint(cfg.endpoint, cfg.allowHTTP)
	if err != nil {
		return nil, err
	}
	client := cfg.client
	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ResponseHeaderTimeout: 2 * time.Minute,
				TLSHandshakeTimeout:   15 * time.Second,
			},
		}
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	return &S3{
		name: cfg.name, endpoint: endpoint, bucket: cfg.bucket, region: cfg.region,
		accessKey: cfg.accessKey, secretKey: cfg.secretKey, client: client, now: now,
	}, nil
}

func (s *S3) Name() string { return s.name }

func (s *S3) Put(ctx context.Context, digest, mediaType string, size int64, r io.Reader) (Object, error) {
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	if err := validatePut(digest, mediaType, size); err != nil {
		return Object{}, err
	}
	verified, err := readVerified(r, size, digest)
	if err != nil {
		return Object{}, err
	}
	defer func() {
		name := verified.Name()
		_ = verified.Close()
		_ = removeFile(name)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(digest), verified)
	if err != nil {
		return Object{}, ErrStorage
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", mediaType)
	s.sign(req, digest, s.now().UTC())
	resp, err := s.client.Do(req)
	if err != nil {
		return Object{}, transportErr(err)
	}
	defer closeResponse(resp)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return Object{}, fmt.Errorf("%w: http %d", ErrStorage, resp.StatusCode)
	}
	return objectFor(s.Name(), digest, mediaType, size), nil
}

func (s *S3) Open(ctx context.Context, digest string) (io.ReadCloser, Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, Object{}, err
	}
	if !ValidDigest(digest) {
		return nil, Object{}, ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(digest), nil)
	if err != nil {
		return nil, Object{}, ErrStorage
	}
	s.sign(req, emptyPayloadHash, s.now().UTC())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, Object{}, transportErr(err)
	}
	if resp.StatusCode == http.StatusNotFound {
		closeResponse(resp)
		return nil, Object{}, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 || resp.ContentLength > MaxBytes {
		closeResponse(resp)
		return nil, Object{}, ErrStorage
	}
	obj := objectFor(s.Name(), digest, "application/octet-stream", resp.ContentLength)
	return resp.Body, obj, nil
}

func (s *S3) objectURL(digest string) string {
	u := *s.endpoint
	u.Path = "/" + s.bucket + "/" + ObjectKey(digest)
	u.RawPath = escapePath(u.Path)
	return u.String()
}

func (s *S3) sign(req *http.Request, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	req.Host = req.URL.Host
	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if req.Header.Get("Content-Type") != "" {
		signed = append(signed, "content-type")
	}
	if req.Header.Get("Range") != "" {
		signed = append(signed, "range")
	}
	sort.Strings(signed)
	scope := dateStamp + "/" + s.region + "/s3/aws4_request"
	canonical := canonicalRequest(req, signed, payloadHash)
	sum := sha256.Sum256([]byte(canonical))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	signature := hex.EncodeToString(hmacSHA256(signingKey(s.secretKey, dateStamp, s.region), []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, scope, strings.Join(signed, ";"), signature))
}

func canonicalRequest(req *http.Request, signed []string, payloadHash string) string {
	var headers strings.Builder
	for _, name := range signed {
		headers.WriteString(name)
		headers.WriteByte(':')
		headers.WriteString(headerValue(req, name))
		headers.WriteByte('\n')
	}
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	// CanonicalHeaders already ends with a newline. The SigV4 join adds another,
	// which is the blank line in the AWS GET example (signature f0e8bdb8...).
	return req.Method + "\n" + path + "\n" + req.URL.RawQuery + "\n" +
		headers.String() + "\n" + strings.Join(signed, ";") + "\n" + payloadHash
}

func headerValue(req *http.Request, name string) string {
	if name == "host" {
		return canonicalHeaderValue(req.Host)
	}
	return canonicalHeaderValue(req.Header.Get(http.CanonicalHeaderKey(name)))
}

func canonicalHeaderValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func signingKey(secret, dateStamp, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func parseEndpoint(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrConfig
	}
	if u.Path != "" && u.Path != "/" {
		return nil, ErrConfig
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return nil, ErrConfig
		}
	default:
		return nil, ErrConfig
	}
	u.Path = ""
	u.RawPath = ""
	return u, nil
}

func validBucket(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "..") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func validCredential(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsAny(value, "\r\n \t")
}

func validRegion(region string) bool {
	if region == "" || len(region) > 32 {
		return false
	}
	for i := 0; i < len(region); i++ {
		c := region[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func transportErr(err error) error {
	if err == nil {
		return nil
	}
	if errorsIsContext(err) {
		return err
	}
	return ErrStorage
}

func closeResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}
