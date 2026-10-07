// Package clientip resolves client addresses only through explicitly trusted peers.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type Resolver struct {
	Proxies []netip.Prefix
	Header  string
}

func Parse(proxies, header string) (Resolver, error) {
	r := Resolver{Header: header}
	if header == "" {
		r.Header = "X-Forwarded-For"
	}
	if r.Header != "X-Forwarded-For" && r.Header != "Fly-Client-IP" {
		return r, fmt.Errorf("CLAWCTL_CLIENT_IP_HEADER must be X-Forwarded-For or Fly-Client-IP")
	}
	for _, entry := range strings.FieldsFunc(proxies, func(c rune) bool { return c == ',' || c == ' ' || c == '\t' || c == '\n' || c == '\r' }) {
		p, err := netip.ParsePrefix(entry)
		if err != nil {
			a, e := parseIP(entry)
			if e != nil {
				return r, fmt.Errorf("CLAWCTL_TRUSTED_PROXIES: invalid entry %q", entry)
			}
			a = a.Unmap()
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return r, fmt.Errorf("CLAWCTL_TRUSTED_PROXIES: overly broad prefix %q", entry)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		minimum := 16
		if p.Addr().Is4() {
			minimum = 8
		}
		if p.Bits() < minimum {
			return r, fmt.Errorf("CLAWCTL_TRUSTED_PROXIES: prefix %q is too broad (minimum /%d)", entry, minimum)
		}
		r.Proxies = append(r.Proxies, p.Masked())
	}
	return r, nil
}
func (r Resolver) trusted(a netip.Addr) bool {
	for _, p := range r.Proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
func parseIP(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err == nil && a.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("scoped IP")
	}
	return a.Unmap(), err
}
func (r Resolver) Resolve(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	peer, err := parseIP(host)
	if err != nil {
		return req.RemoteAddr
	}
	if !r.trusted(peer) {
		return peer.String()
	}
	if r.Header == "Fly-Client-IP" {
		values := req.Header.Values("Fly-Client-IP")
		if len(values) == 1 {
			if a, err := parseIP(values[0]); err == nil {
				return a.String()
			}
		}
		return peer.String()
	}
	entries := strings.Split(strings.Join(req.Header.Values("X-Forwarded-For"), ","), ",")
	last := peer
	for i, n := len(entries)-1, 0; i >= 0 && n < 32; i, n = i-1, n+1 {
		a, err := parseIP(entries[i])
		if err != nil {
			return last.String()
		}
		if !r.trusted(a) {
			return a.String()
		}
		last = a
	}
	return peer.String()
}
