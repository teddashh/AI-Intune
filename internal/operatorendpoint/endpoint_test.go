package operatorendpoint

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseBaseURLAcceptsOnlyPinnedTailscaleOrigin(t *testing.T) {
	for _, test := range []struct {
		raw, base, authority string
		destination          netip.Addr
		port                 uint16
	}{
		{
			raw: "http://100.64.0.1:8787", base: "http://100.64.0.1:8787",
			authority: "100.64.0.1:8787", destination: netip.MustParseAddr("100.64.0.1"), port: 8787,
		},
		{
			raw: "http://100.127.255.254:1/", base: "http://100.127.255.254:1",
			authority: "100.127.255.254:1", destination: netip.MustParseAddr("100.127.255.254"), port: 1,
		},
		{
			raw: "http://[fd7a:115c:a1e0::1234]:65535/", base: "http://[fd7a:115c:a1e0::1234]:65535",
			authority: "[fd7a:115c:a1e0::1234]:65535", destination: netip.MustParseAddr("fd7a:115c:a1e0::1234"), port: 65535,
		},
	} {
		t.Run(test.raw, func(t *testing.T) {
			got, err := ParseBaseURL(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.BaseURL() != test.base || got.Authority() != test.authority ||
				got.Destination() != test.destination || got.Port() != test.port {
				t.Fatalf("endpoint=%+v, want base=%q authority=%q destination=%s port=%d",
					got, test.base, test.authority, test.destination, test.port)
			}
		})
	}
}

func TestParseBaseURLRejectsAnySecondAuthorityOrURLDecoration(t *testing.T) {
	invalid := map[string]string{
		"empty":                  "",
		"leading whitespace":     " http://100.64.0.1:8787",
		"trailing whitespace":    "http://100.64.0.1:8787 ",
		"hostname":               "http://hub.example:8787",
		"https":                  "https://100.64.0.1:8787",
		"uppercase scheme":       "HTTP://100.64.0.1:8787",
		"loopback v4":            "http://127.0.0.1:8787",
		"loopback v6":            "http://[::1]:8787",
		"lan":                    "http://192.168.1.20:8787",
		"public":                 "http://8.8.8.8:8787",
		"non tailscale ula":      "http://[fd00::1]:8787",
		"tailscale service v4":   "http://100.100.100.100:8787",
		"tailscale service v6":   "http://[fd7a:115c:a1e0::53]:8787",
		"tailscale via address":  "http://[fd7a:115c:a1e0:b1a::1]:8787",
		"unspecified":            "http://0.0.0.0:8787",
		"missing port":           "http://100.64.0.1",
		"zero port":              "http://100.64.0.1:0",
		"nonnumeric port":        "http://100.64.0.1:http",
		"out of range port":      "http://100.64.0.1:65536",
		"noncanonical port":      "http://100.64.0.1:08787",
		"base path":              "http://100.64.0.1:8787/operator",
		"dot path":               "http://100.64.0.1:8787/.",
		"encoded path":           "http://100.64.0.1:8787/%2f",
		"userinfo":               "http://user@100.64.0.1:8787",
		"query":                  "http://100.64.0.1:8787/?x=1",
		"empty force query":      "http://100.64.0.1:8787/?",
		"fragment":               "http://100.64.0.1:8787/#x",
		"empty fragment":         "http://100.64.0.1:8787/#",
		"ipv6 zone":              "http://[fd7a:115c:a1e0::1%25eth0]:8787",
		"mapped noncanonical ip": "http://[::ffff:100.64.0.1]:8787",
	}
	for name, raw := range invalid {
		t.Run(name, func(t *testing.T) {
			if endpoint, err := ParseBaseURL(raw); err == nil {
				t.Fatalf("ParseBaseURL(%q)=%+v, want rejection", raw, endpoint)
			}
		})
	}
}

func TestParseBaseURLErrorNeverEchoesRejectedUserinfo(t *testing.T) {
	const secret = "must-not-reach-logs"
	_, err := ParseBaseURL("http://user:" + secret + "%zz@100.64.0.1:8787")
	if err == nil {
		t.Fatal("malformed userinfo URL was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("rejected URL leaked userinfo: %v", err)
	}
}

func TestParseListenSharesBaseURLContract(t *testing.T) {
	got, err := ParseListen("100.64.200.2:8787")
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL() != "http://100.64.200.2:8787" {
		t.Fatalf("base=%q", got.BaseURL())
	}
	for _, invalid := range []string{"", " 100.64.200.2:8787", "hub.example:8787", "127.0.0.1:8787", "100.64.200.2:0", "100.64.200.2:8787/"} {
		if _, err := ParseListen(invalid); err == nil {
			t.Errorf("ParseListen(%q) accepted invalid listener", invalid)
		}
	}
}

func TestCanonicalLiteralAuthorityDoesNotResolveNames(t *testing.T) {
	if got, ok := CanonicalLiteralAuthority("[fd7a:115c:a1e0:0:0:0:0:1]:8787"); !ok || got != "[fd7a:115c:a1e0::1]:8787" {
		t.Fatalf("canonical=(%q,%t)", got, ok)
	}
	for _, raw := range []string{"hub.example:8787", "100.64.0.1", "100.64.0.1:0", "100.64.0.1:+80", " 100.64.0.1:80"} {
		if got, ok := CanonicalLiteralAuthority(raw); ok {
			t.Errorf("CanonicalLiteralAuthority(%q)=(%q,true), want false", raw, got)
		}
	}
}
