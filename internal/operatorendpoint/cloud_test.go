package operatorendpoint

import "testing"

func TestParseCloudListen(t *testing.T) {
	for _, raw := range []string{"0.0.0.0:8787", "127.0.0.1:80", "[::]:443", "[::1]:8787", "192.168.1.2:123", "203.0.113.1:65535", "100.64.0.1:8787"} {
		t.Run(raw, func(t *testing.T) {
			got, err := ParseCloudListen(raw)
			if err != nil || got.Authority() != raw || got.Port() == 0 {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
	for _, raw := range []string{"", ":8787", "localhost:8787", "hub.example.com:443", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:080", "127.0.0.1:+80", "127.0.0.1:80/", " 127.0.0.1:80", "[::ffff:127.0.0.1]:80", "[0:0:0:0:0:0:0:1]:80", "[fe80::1%eth0]:80", "::1:80"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseCloudListen(raw); err == nil {
				t.Fatal("accepted invalid listen")
			}
		})
	}
}

func TestParsePublicURL(t *testing.T) {
	for _, tt := range []struct{ raw, base, host, scheme, authority string }{
		{"https://Hub.Example.COM:443/", "https://hub.example.com", "hub.example.com", "https", "hub.example.com"},
		{"https://hub.example.com:8443", "https://hub.example.com:8443", "hub.example.com", "https", "hub.example.com:8443"},
		{"https://203.0.113.1", "https://203.0.113.1", "203.0.113.1", "https", "203.0.113.1"},
		{"http://LOCALHOST:80", "http://localhost", "localhost", "http", "localhost"},
		{"http://127.0.0.2:8080/", "http://127.0.0.2:8080", "127.0.0.2", "http", "127.0.0.2:8080"},
		{"http://[::1]:80", "http://[::1]", "::1", "http", "[::1]"},
		{"https://[2001:db8::1]:443", "https://[2001:db8::1]", "2001:db8::1", "https", "[2001:db8::1]"},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := ParsePublicURL(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.BaseURL() != tt.base || got.Host() != tt.host || got.Scheme() != tt.scheme || got.Authority() != tt.authority {
				t.Fatalf("got %+v", got)
			}
			if !got.MatchesAuthority(tt.authority) {
				t.Fatal("canonical authority refused")
			}
		})
	}
	for _, raw := range []string{"", " https://hub.example.com", "https://hub.example.com ", "http://hub.example.com", "http://0.0.0.0", "http://192.168.1.2", "ftp://localhost", "https://user:secret@hub.example.com", "https://hub.example.com/path", "https://hub.example.com//", "https://hub.example.com/%2f", "https://hub.example.com?", "https://hub.example.com#", "https://hub.example.com?q=x", "https://hub.example.com#fragment", "https://hub.example.com:", "https://hub.example.com:0", "https://hub.example.com:65536", "https://hub.example.com:+443", "https://[fe80::1%25eth0]", "https://[localhost]", "https://-bad.example", "https://bad_.example", "https://bad..example", "https:///", "https://::1"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParsePublicURL(raw); err == nil {
				t.Fatal("accepted invalid public URL")
			}
		})
	}
}

func TestPublicAuthorityDefaultPort(t *testing.T) {
	for _, raw := range []string{"https://hub.example.com", "https://[::1]", "http://localhost", "http://[::1]"} {
		u, err := ParsePublicURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		port := ":443"
		if u.Scheme() == "http" {
			port = ":80"
		}
		if !u.MatchesAuthority(u.Authority() + port) {
			t.Fatal("explicit default port refused")
		}
		for _, bad := range []string{u.Authority() + ":8443", u.Authority() + ".", " " + u.Authority(), u.Authority() + ":0443", "evil.example"} {
			if u.MatchesAuthority(bad) {
				t.Fatalf("accepted %q", bad)
			}
		}
	}
}
