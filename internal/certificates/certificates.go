// Package certificates probes configured TLS endpoints without sending HTTP requests.
package certificates

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

const Interval = time.Hour
const StaleAfter = 3 * time.Hour

type Config struct {
	Endpoint string   `json:"endpoint"`
	Expected []string `json:"expected"`
	Owner    string   `json:"owner"`
	Notes    string   `json:"notes"`
	Days     []int    `json:"days"`
}
type Result struct {
	AttemptedAt       time.Time `json:"attempted_at"`
	ObservedAt        time.Time `json:"observed_at"`
	NotAfter          time.Time `json:"not_after"`
	Issuer            string    `json:"issuer"`
	SANs              []string  `json:"sans"`
	VerificationError string    `json:"verification_error"`
	ProbeError        string    `json:"probe_error"`
	Gaps              []string  `json:"gaps"`
}
type Monitor struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Config
	Result Result `json:"result"`
}
type Status struct {
	Monitor
	DaysLeft   int      `json:"days_left"`
	Flags      []string `json:"flags"`
	NextAction string   `json:"next_action"`
}

func hostname(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	if len(s) > 253 || strings.ContainsAny(s, "*/:@ \\?#\r\n\t") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func (c *Config) Validate() error {
	host, port, err := net.SplitHostPort(c.Endpoint)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || n < 1 || n > 65535 || !hostname(strings.ToLower(host)) {
		return errors.New("endpoint must be a hostname:port or [IP]:port without a URL path or credentials")
	}
	c.Endpoint = net.JoinHostPort(strings.ToLower(host), strconv.Itoa(n))
	if len(c.Expected) == 0 || len(c.Expected) > 64 {
		return errors.New("provide 1–64 expected hostnames")
	}
	for i, h := range c.Expected {
		h = strings.ToLower(strings.TrimSpace(h))
		if !hostname(h) {
			return errors.New("expected hostnames must be concrete DNS names or IP addresses")
		}
		c.Expected[i] = h
	}
	if len(c.Owner) > 200 || len(c.Notes) > 2000 {
		return errors.New("owner or notes too long")
	}
	if len(c.Days) == 0 {
		c.Days = []int{30, 14, 7}
	}
	if len(c.Days) > 10 {
		return errors.New("at most 10 expiry thresholds")
	}
	for _, d := range c.Days {
		if d < 1 || d > 3650 {
			return errors.New("expiry days must be between 1 and 3650")
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(c.Days)))
	return nil
}

// Probe verifies the TLS handshake with system roots (or supplied roots in tests),
// retaining the peer chain when certificate verification fails. No HTTP,
// redirects, credentials, or application payloads are sent.
func Probe(ctx context.Context, c Config, roots *x509.CertPool) Result {
	r := Result{AttemptedAt: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	host, _, _ := net.SplitHostPort(c.Endpoint)
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", c.Endpoint)
	var peers []*x509.Certificate
	if err != nil {
		var verificationErr *tls.CertificateVerificationError
		if !errors.As(err, &verificationErr) {
			r.ProbeError = "TLS connection or handshake failed"
			return r
		}
		peers = verificationErr.UnverifiedCertificates
		r.VerificationError = verificationErr.Error()
	} else {
		defer conn.Close()
		peers = conn.(*tls.Conn).ConnectionState().PeerCertificates
	}
	if len(peers) == 0 {
		r.ProbeError = "peer returned no certificate"
		return r
	}
	leaf := peers[0]
	r.ObservedAt = time.Now().UTC()
	r.NotAfter = leaf.NotAfter
	r.Issuer = leaf.Issuer.String()
	r.SANs = append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		r.SANs = append(r.SANs, ip.String())
	}
	for _, h := range c.Expected {
		if leaf.VerifyHostname(h) != nil {
			r.Gaps = append(r.Gaps, h)
		}
	}
	return r
}
func Evaluate(m Monitor, now time.Time) Status {
	if len(m.Days) == 0 {
		m.Days = []int{30, 14, 7}
	}
	s := Status{Monitor: m, Flags: []string{}, NextAction: "No action needed; Hub probes hourly."}
	r := m.Result
	if r.ObservedAt.IsZero() || now.Sub(r.ObservedAt) > StaleAfter {
		s.Flags = append(s.Flags, "stale probe")
		s.NextAction = "Check Hub connectivity, endpoint and TLS listener; request a probe."
	}
	if r.ProbeError != "" {
		s.Flags = append(s.Flags, "probe failed")
		s.NextAction = "Check Hub connectivity, endpoint and TLS listener; request a probe."
	}
	if !r.NotAfter.IsZero() {
		s.DaysLeft = int(math.Floor(r.NotAfter.Sub(now).Hours() / 24))
		if !r.NotAfter.After(now) {
			s.Flags = append(s.Flags, "expired")
		} else {
			for i := len(m.Days) - 1; i >= 0; i-- {
				if r.NotAfter.Sub(now) <= time.Duration(m.Days[i])*24*time.Hour {
					s.Flags = append(s.Flags, fmt.Sprintf("expires within %d days", m.Days[i]))
					break
				}
			}
		}
	}
	if r.VerificationError != "" {
		s.Flags = append(s.Flags, "chain invalid")
	}
	if len(r.Gaps) > 0 {
		s.Flags = append(s.Flags, "SAN gap")
	}
	if len(s.Flags) > 0 && !r.ObservedAt.IsZero() && (len(r.Gaps) > 0 || r.VerificationError != "" || !r.NotAfter.IsZero() && r.NotAfter.Sub(now) <= time.Duration(m.Days[0])*24*time.Hour) {
		s.NextAction = "Ask the owner to renew/reissue with all expected SANs and serve a valid full chain; probe again."
	}
	return s
}
func Sort(items []Status) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Result.NotAfter.IsZero() != b.Result.NotAfter.IsZero() {
			return a.Result.NotAfter.IsZero()
		}
		if a.DaysLeft != b.DaysLeft {
			return a.DaysLeft < b.DaysLeft
		}
		return a.ID < b.ID
	})
}
