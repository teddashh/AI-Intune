package operatorclient

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/teddashh/AI-Intune/internal/store"
)

// ReadServiceTokenFile verifies the opened file as well as the directory entry.
// SameFile prevents a replacement between Lstat and Open from being accepted.
func ReadServiceTokenFile(path string) (string, error) {
	invalid := errors.New("token file must be a regular, non-symlink file with mode 0600 containing a service token")
	before, err := os.Lstat(path)
	if err != nil {
		return "", invalid
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != 0600 {
		return "", invalid
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", invalid
	}
	f := os.NewFile(uintptr(fd), "service-token")
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm() != 0600 {
		return "", invalid
	}
	data, err := io.ReadAll(io.LimitReader(f, 128))
	if err != nil {
		return "", invalid
	}
	secret := strings.TrimSpace(string(data))
	if !store.ValidServiceSecret(secret) {
		return "", invalid
	}
	return secret, nil
}

func ServiceOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || strings.TrimSpace(raw) != raw {
		return "", errors.New("service token requires an HTTPS origin without credentials, path, query or fragment")
	}
	u.Path = ""
	return u.String(), nil
}

func NewWithTokenFile(origin, path string) (*Client, error) {
	base, err := ServiceOrigin(origin)
	if err != nil {
		return nil, err
	}
	secret, err := ReadServiceTokenFile(path)
	if err != nil {
		return nil, err
	}
	transport, err := transportWithoutAmbientProxy(nil)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(base)
	return &Client{base: u, token: secret, http: &http.Client{Timeout: requestTimeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
