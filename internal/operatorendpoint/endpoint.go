// Package operatorendpoint owns the single network-origin contract shared by
// the Hub's human/operator server and its official HTTP clients.
//
// An operator endpoint is deliberately narrower than a general URL: it is
// plain HTTP to one canonical literal Tailscale IP and one explicit, non-zero
// numeric port. Tailscale supplies the encrypted/authenticated network path;
// accepting DNS names, another URL scheme, or a path prefix would create a
// second authority that the Hub's Host-pinning boundary does not recognize.
package operatorendpoint

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"tailscale.com/net/tsaddr"
)

// Endpoint is an already validated operator origin. Its fields are private so
// callers cannot accidentally construct a value that bypasses ParseBaseURL.
type Endpoint struct {
	baseURL     string
	authority   string
	destination netip.Addr
	port        uint16
}

// ParseBaseURL accepts only http://<canonical-literal-tailscale-ip>:<port>,
// optionally followed by one slash. It returns a normalized URL without a
// trailing slash.
func ParseBaseURL(raw string) (Endpoint, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return Endpoint{}, errors.New("operator endpoint: URL 不可為空或帶前後空白")
	}
	if !strings.HasPrefix(raw, "http://") {
		return Endpoint{}, errors.New("operator endpoint: 只接受 canonical http:// scheme")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// net/url's parse error includes the original URL. A rejected authority
		// can contain mistaken userinfo, so returning that error verbatim would
		// copy credentials into CLI logs even though userinfo is never accepted.
		return Endpoint{}, errors.New("operator endpoint: URL 格式錯誤")
	}
	if u.Scheme != "http" || u.Opaque != "" || u.Host == "" {
		return Endpoint{}, errors.New("operator endpoint: 只接受完整的 http:// literal Tailscale IP 位址")
	}
	if u.User != nil {
		return Endpoint{}, errors.New("operator endpoint: URL 不可帶 userinfo")
	}
	// Check both the parsed fields and delimiters. net/url intentionally drops
	// an empty fragment marker, while an empty query is represented by
	// ForceQuery; neither spelling belongs in an origin.
	if u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "?") {
		return Endpoint{}, errors.New("operator endpoint: URL 不可帶 query")
	}
	if u.Fragment != "" || u.RawFragment != "" || strings.Contains(raw, "#") {
		return Endpoint{}, errors.New("operator endpoint: URL 不可帶 fragment")
	}
	if u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return Endpoint{}, errors.New("operator endpoint: URL 不可帶 base path")
	}

	authority, ok := CanonicalLiteralAuthority(u.Host)
	if !ok || authority != u.Host {
		return Endpoint{}, errors.New("operator endpoint: authority 必須是 canonical literal-ip:nonzero-port")
	}
	host, portText, _ := net.SplitHostPort(authority)
	destination, err := netip.ParseAddr(host)
	if err != nil {
		return Endpoint{}, fmt.Errorf("operator endpoint: literal IP 格式錯誤: %w", err)
	}
	destination = destination.Unmap()
	if !tsaddr.IsTailscaleIP(destination) {
		return Endpoint{}, fmt.Errorf("operator endpoint: %s 不是 Tailscale IP", destination)
	}
	if destination == tsaddr.TailscaleServiceIP() || destination == tsaddr.TailscaleServiceIPv6() ||
		tsaddr.TailscaleViaRange().Contains(destination) {
		return Endpoint{}, fmt.Errorf("operator endpoint: %s 不是 Tailscale node IP", destination)
	}
	parsedPort, _ := strconv.ParseUint(portText, 10, 16)
	return Endpoint{
		baseURL:     "http://" + authority,
		authority:   authority,
		destination: destination,
		port:        uint16(parsedPort),
	}, nil
}

// ParseListen applies the same endpoint contract to a server listen
// authority, such as "100.64.0.1:8787".
func ParseListen(listen string) (Endpoint, error) {
	if listen == "" || strings.TrimSpace(listen) != listen {
		return Endpoint{}, errors.New("operator endpoint: listen authority 不可為空或帶前後空白")
	}
	endpoint, err := ParseBaseURL("http://" + listen)
	if err != nil {
		return Endpoint{}, err
	}
	if endpoint.Authority() != listen {
		return Endpoint{}, errors.New("operator endpoint: listen 必須是 canonical literal-ip:nonzero-port authority")
	}
	return endpoint, nil
}

// CanonicalLiteralAuthority parses a host:port authority without consulting
// DNS. It intentionally does not require a Tailscale address: request Host
// values use this helper before comparison with an already validated endpoint.
func CanonicalLiteralAuthority(authority string) (string, bool) {
	if authority == "" || strings.TrimSpace(authority) != authority {
		return "", false
	}
	host, portText, err := net.SplitHostPort(authority)
	if err != nil || host == "" || portText == "" {
		return "", false
	}
	for _, r := range portText {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsValid() || addr.Zone() != "" {
		return "", false
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", false
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(port)).String(), true
}

func (e Endpoint) BaseURL() string         { return e.baseURL }
func (e Endpoint) Authority() string       { return e.authority }
func (e Endpoint) Destination() netip.Addr { return e.destination }
func (e Endpoint) Port() uint16            { return e.port }
