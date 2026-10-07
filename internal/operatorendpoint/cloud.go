package operatorendpoint

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// ParseCloudListen accepts a canonical literal IP with an explicit nonzero port.
// It never resolves hostnames and does not restrict the address to a tailnet.
func ParseCloudListen(raw string) (Endpoint, error) {
	authority, ok := CanonicalLiteralAuthority(raw)
	if !ok || authority != raw {
		return Endpoint{}, errors.New("operator endpoint: listen must be canonical literal-ip:nonzero-port authority")
	}
	ap, _ := netip.ParseAddrPort(authority)
	return Endpoint{baseURL: "http://" + authority, authority: authority, destination: ap.Addr(), port: ap.Port()}, nil
}

// PublicURL is a validated public origin, independent of the bind address.
type PublicURL struct{ scheme, host, authority string }

func (u PublicURL) BaseURL() string   { return u.scheme + "://" + u.authority }
func (u PublicURL) Authority() string { return u.authority }
func (u PublicURL) Scheme() string    { return u.scheme }
func (u PublicURL) Host() string      { return u.host }

// ParsePublicURL accepts HTTPS origins, or HTTP origins on localhost/loopback.
// A single trailing slash is normalized away; paths and URL metadata are refused.
func ParsePublicURL(raw string) (PublicURL, error) {
	invalid := errors.New("operator endpoint: PUBLIC_URL must be an HTTPS origin (HTTP only for localhost/loopback), without userinfo, path, query or fragment")
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "?#") {
		return PublicURL{}, invalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && u.Scheme != "http") {
		return PublicURL{}, invalid
	}
	host := strings.ToLower(u.Hostname())
	ip, ipErr := netip.ParseAddr(host)
	if ipErr == nil {
		if ip.Zone() != "" {
			return PublicURL{}, invalid
		}
		host = ip.Unmap().String()
	} else {
		if len(host) == 0 || len(host) > 253 {
			return PublicURL{}, invalid
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return PublicURL{}, invalid
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return PublicURL{}, invalid
				}
			}
		}
	}
	// Reject malformed brackets and empty port delimiters even when net/url accepts them.
	rawHost := u.Hostname()
	expected := rawHost
	if strings.Contains(rawHost, ":") {
		expected = "[" + rawHost + "]"
	}
	if u.Port() != "" {
		expected += ":" + u.Port()
	}
	if u.Host != expected {
		return PublicURL{}, invalid
	}
	if u.Scheme == "http" && host != "localhost" && (ipErr != nil || !ip.Unmap().IsLoopback()) {
		return PublicURL{}, invalid
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return PublicURL{}, invalid
		}
		port = strconv.FormatUint(n, 10)
	}
	if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		port = ""
	}
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	return PublicURL{scheme: u.Scheme, host: host, authority: authority}, nil
}

// MatchesAuthority accepts only the configured authority and its explicit
// default-port spelling. Forwarded headers never participate in this decision.
func (u PublicURL) MatchesAuthority(authority string) bool {
	if u.scheme == "" || u.authority == "" {
		return false
	}
	authority = strings.ToLower(authority)
	if authority == u.authority {
		return true
	}
	port := "443"
	if u.scheme == "http" {
		port = "80"
	}
	return u.authority == strings.TrimSuffix(net.JoinHostPort(u.host, port), ":"+port) && authority == net.JoinHostPort(u.host, port)
}
