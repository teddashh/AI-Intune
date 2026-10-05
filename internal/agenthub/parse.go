// Package agenthub accepts the hub_url an agent may enroll against.
//
// The operator UI stays on a literal Tailscale HTTP origin. An https hostname
// is an experimental Cloudflare Tunnel for the agent plane only. This package
// does not change operator authentication.
package agenthub

import (
	"errors"
	"net"
	"net/url"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
)

// Parse returns the canonical hub URL for a new enrollment.
//
// http:// is delegated to the operator endpoint contract (literal Tailscale
// IP and an explicit port). https:// is a hostname, optional port, and no
// path. One trailing slash is removed. An explicit port is kept as written.
func Parse(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", errors.New("agent hub: URL is empty or has surrounding whitespace")
	}
	if strings.HasPrefix(raw, "http://") {
		endpoint, err := operatorendpoint.ParseBaseURL(raw)
		if err != nil {
			return "", errors.New("agent hub: http URL must be a literal Tailscale IP and port")
		}
		return endpoint.BaseURL(), nil
	}
	if !strings.HasPrefix(raw, "https://") {
		return "", errors.New("agent hub: URL must be http://<tailscale-ip>:<port> or https://<hostname>")
	}
	return parseTunnel(raw)
}

func parseTunnel(raw string) (string, error) {
	if strings.ContainsAny(raw, "@?#") {
		return "", errors.New("agent hub: URL must not include userinfo, a query, or a fragment")
	}
	if strings.HasSuffix(raw, "/") {
		raw = strings.TrimSuffix(raw, "/")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", errors.New("agent hub: https URL is not a hostname")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return "", errors.New("agent hub: URL must not include a query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("agent hub: URL must not include a path")
	}
	if strings.Contains(raw, "/") {
		// A leftover slash means the caller sent more than one trailing slash
		// or a path that url.Parse folded. Either form is rejected.
		rest := strings.TrimPrefix(raw, "https://")
		if strings.Contains(rest, "/") {
			return "", errors.New("agent hub: URL must not include a path")
		}
	}
	host := u.Hostname()
	if host == "" || strings.Contains(host, ":") {
		return "", errors.New("agent hub: https URL is not a hostname")
	}
	canonicalHost, err := canonicalTunnelHost(host)
	if err != nil {
		return "", err
	}
	port := u.Port()
	if port == "" {
		return "https://" + canonicalHost, nil
	}
	if !canonicalPort(port) {
		return "", errors.New("agent hub: port is not an explicit non-zero port")
	}
	// Keep a port the caller wrote, including 443. Do not add one they omitted.
	if !strings.Contains(u.Host, ":") {
		return "", errors.New("agent hub: port is not explicit")
	}
	return "https://" + canonicalHost + ":" + port, nil
}

func canonicalTunnelHost(host string) (string, error) {
	lower := strings.ToLower(host)
	if !tunnelHostname(lower) {
		return "", errors.New("agent hub: https URL must be a DNS hostname")
	}
	return lower, nil
}

func tunnelHostname(host string) bool {
	if host == "" || len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}
	letter := false
	for i := 0; i < len(host); i++ {
		c := host[i]
		if c >= 'a' && c <= 'z' {
			letter = true
		}
	}
	if !letter {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "localhost" || !dnsLabel(label) {
			return false
		}
	}
	return true
}

func dnsLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func canonicalPort(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	if len(port) > 1 && port[0] == '0' {
		return false
	}
	n := 0
	for i := 0; i < len(port); i++ {
		c := port[i]
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 1 && n <= 65535
}
