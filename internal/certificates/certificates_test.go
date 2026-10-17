package certificates

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeGeneratedCertificates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expired  bool
		trusted  bool
		gap      bool
		wrongSAN bool
	}{{"valid", false, true, false, false}, {"untrusted", false, false, false, false}, {"expired", true, true, false, false}, {"wildcard gap", false, true, true, false}, {"wrong SAN", false, true, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			now := time.Now()
			end := now.Add(10 * 24 * time.Hour)
			if tc.expired {
				end = now.Add(-time.Hour)
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.test"}, DNSNames: []string{"example.test", "*.example.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-24 * time.Hour), NotAfter: end, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
			if tc.wrongSAN {
				template.IPAddresses = []net.IP{net.ParseIP("192.0.2.1")}
			}
			der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			cert, _ := x509.ParseCertificate(der)
			roots := x509.NewCertPool()
			if tc.trusted {
				roots.AddCert(cert)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("probe sent HTTP") }))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
			server.StartTLS()
			defer server.Close()
			expected := []string{"example.test", "www.example.test"}
			if tc.gap {
				expected = append(expected, "deep.www.example.test", "other.test")
			}
			c := Config{Endpoint: strings.TrimPrefix(server.URL, "https://"), Expected: expected}
			if err = c.Validate(); err != nil {
				t.Fatal(err)
			}
			result := Probe(context.Background(), c, roots)
			if result.ObservedAt.IsZero() || !result.NotAfter.Equal(cert.NotAfter) || result.Issuer != cert.Issuer.String() || len(result.SANs) != 3 || result.ProbeError != "" {
				t.Fatalf("missing observation: %+v", result)
			}
			if (result.VerificationError != "") != (tc.expired || !tc.trusted || tc.wrongSAN) {
				t.Fatalf("verification: %s", result.VerificationError)
			}
			if tc.gap && len(result.Gaps) != 2 || !tc.gap && len(result.Gaps) != 0 {
				t.Fatalf("gaps: %v", result.Gaps)
			}
			s := Evaluate(Monitor{Config: c, Result: result}, now)
			if tc.expired && !strings.Contains(strings.Join(s.Flags, ","), "expired") {
				t.Fatal(s)
			}
		})
	}
}
func TestThresholdsAndStale(t *testing.T) {
	now := time.Now()
	c := Config{Endpoint: "example.test:443", Expected: []string{"example.test"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		days int
		flag string
	}{{40, ""}, {29, "30"}, {13, "14"}, {6, "7"}, {-1, "expired"}} {
		s := Evaluate(Monitor{Config: c, Result: Result{ObservedAt: now, NotAfter: now.Add(time.Duration(tc.days) * 24 * time.Hour)}}, now)
		if tc.flag == "" && len(s.Flags) != 0 || tc.flag != "" && !strings.Contains(strings.Join(s.Flags, ","), tc.flag) {
			t.Fatalf("%+v", s)
		}
	}
	s := Evaluate(Monitor{Config: c, Result: Result{ObservedAt: now.Add(-4 * time.Hour)}}, now)
	if len(s.Flags) != 1 || s.Flags[0] != "stale probe" {
		t.Fatal(s)
	}
}
func TestConfigRejectsURLsCredentialsAndWildcards(t *testing.T) {
	for _, endpoint := range []string{"https://example.test:443/path", "user:pass@example.test:443", "example.test:0", "example.test:65536", "bad name:443"} {
		c := Config{Endpoint: endpoint, Expected: []string{"example.test"}}
		if c.Validate() == nil {
			t.Fatal(endpoint)
		}
	}
	c := Config{Endpoint: "example.test:443", Expected: []string{"*.example.test"}}
	if c.Validate() == nil {
		t.Fatal("accepted wildcard expectation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := Probe(ctx, Config{Endpoint: "example.test:443"}, nil)
	if r.ProbeError == "" {
		t.Fatal("cancelled probe succeeded")
	}
}

func TestRecentFailedProbeHasRepairAction(t *testing.T) {
	now := time.Now()
	s := Evaluate(Monitor{Result: Result{ObservedAt: now, AttemptedAt: now, NotAfter: now.Add(90 * 24 * time.Hour), ProbeError: "TLS connection or handshake failed"}}, now)
	if len(s.Flags) != 1 || s.Flags[0] != "probe failed" || !strings.Contains(s.NextAction, "connectivity") {
		t.Fatalf("failed probe must offer repair: %+v", s)
	}
}
