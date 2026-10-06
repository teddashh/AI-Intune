package blobstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrStorage is a remote failure that does not include the endpoint, the
// bucket path, or any credential. Callers log the digest and this error.
var ErrStorage = errors.New("blobstore: object storage request failed")

// FromEnv builds an R2 or S3 backend from the process environment.
//
// An empty environment returns (nil, "", nil) and Hub keeps local files.
// Setting any variable in a group requires that group's access key, secret,
// bucket, and https endpoint. Setting both groups is refused. Error text
// never includes the secret or the account id.
func FromEnv(getenv func(string) string) (Backend, string, error) {
	if getenv == nil {
		return nil, "", ErrConfig
	}
	r2, r2Set, err := readGroup(getenv, "R2")
	if err != nil {
		return nil, "", err
	}
	s3, s3Set, err := readGroup(getenv, "S3")
	if err != nil {
		return nil, "", err
	}
	switch {
	case r2Set && s3Set:
		return nil, "", fmt.Errorf("%w: set only one of R2_* or S3_*", ErrConfig)
	case r2Set:
		return finishGroup(r2, backendR2, "auto")
	case s3Set:
		return finishGroup(s3, backendS3, "us-east-1")
	default:
		return nil, "", nil
	}
}

type envGroup struct {
	account   string
	accountOK bool
	access    string
	secret    string
	bucket    string
	endpoint  string
	region    string
}

func readGroup(getenv func(string) string, prefix string) (envGroup, bool, error) {
	var group envGroup
	set := false
	read := func(suffix string) (string, error) {
		value, present, err := cleanEnv(getenv(prefix + "_" + suffix))
		if err != nil {
			return "", err
		}
		if present {
			set = true
		}
		return value, nil
	}
	var err error
	if group.account, err = read("ACCOUNT_ID"); err != nil {
		return envGroup{}, false, err
	}
	group.accountOK = group.account != ""
	if group.access, err = read("ACCESS_KEY_ID"); err != nil {
		return envGroup{}, false, err
	}
	if group.secret, err = read("SECRET_ACCESS_KEY"); err != nil {
		return envGroup{}, false, err
	}
	if group.bucket, err = read("BUCKET"); err != nil {
		return envGroup{}, false, err
	}
	if group.endpoint, err = read("ENDPOINT"); err != nil {
		return envGroup{}, false, err
	}
	if group.region, err = read("REGION"); err != nil {
		return envGroup{}, false, err
	}
	return group, set, nil
}

func cleanEnv(raw string) (string, bool, error) {
	if raw == "" {
		return "", false, nil
	}
	if strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\r\n") {
		return "", true, fmt.Errorf("%w: a storage variable is empty or contains whitespace", ErrConfig)
	}
	return raw, true, nil
}

func finishGroup(group envGroup, backend, defaultRegion string) (Backend, string, error) {
	if group.access == "" || group.secret == "" || group.bucket == "" || group.endpoint == "" {
		return nil, "", fmt.Errorf("%w: %s requires access key, secret, bucket, and endpoint", ErrConfig, backend)
	}
	if group.accountOK {
		if backend != backendR2 || !validAccountID(group.account) {
			return nil, "", fmt.Errorf("%w: account id is not a 32-character hex value", ErrConfig)
		}
		endpoint, err := url.Parse(group.endpoint)
		if err != nil || !strings.Contains(endpoint.Hostname(), group.account) {
			return nil, "", fmt.Errorf("%w: endpoint host does not contain the configured account", ErrConfig)
		}
	}
	region := group.region
	if region == "" {
		region = defaultRegion
	}
	client, err := newS3(s3Config{
		name: backend, endpoint: group.endpoint, bucket: group.bucket, region: region,
		accessKey: group.access, secretKey: group.secret,
	})
	if err != nil {
		return nil, "", err
	}
	return client, backend + " bucket=" + group.bucket + " endpoint=" + client.endpoint.Host, nil
}

func validAccountID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func errorsIsContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
