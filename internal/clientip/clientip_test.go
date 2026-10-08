package clientip

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	for _, tt := range []struct {
		name, proxies, header, peer string
		values                      []string
		want                        string
	}{
		{"untrusted", "10.0.0.0/8", "", "192.0.2.1:1", []string{"203.0.113.1"}, "192.0.2.1"},
		{"rightmost", "10.0.0.0/8", "", "10.0.0.1:1", []string{"203.0.113.9, 192.0.2.1, 10.0.0.2"}, "192.0.2.1"},
		{"all trusted", "10.0.0.0/8", "", "10.0.0.1:1", []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.1"},
		{"malformed", "10.0.0.0/8", "", "10.0.0.1:1", []string{"192.0.2.1, garbage, 10.0.0.2"}, "10.0.0.2"},
		{"multiple lines", "10.0.0.0/8", "", "10.0.0.1:1", []string{"203.0.113.9", "192.0.2.1, 10.0.0.2"}, "192.0.2.1"},
		{"fly pack defaults", "172.16.0.0/12", "Fly-Client-IP", "172.19.0.2:8787", []string{"192.0.2.1"}, "192.0.2.1"},
		{"fly pack 6pn untrusted", "172.16.0.0/12", "Fly-Client-IP", "[fdaa:0:1::2]:8787", []string{"192.0.2.1"}, "fdaa:0:1::2"},
		{"fly pack empty trust", "", "Fly-Client-IP", "172.19.0.2:8787", []string{"192.0.2.1"}, "172.19.0.2"},
		{"fly", "10.0.0.1", "Fly-Client-IP", "10.0.0.1:1", []string{"192.0.2.1"}, "192.0.2.1"},
		{"fly multiple", "10.0.0.1", "Fly-Client-IP", "10.0.0.1:1", []string{"192.0.2.1", "192.0.2.2"}, "10.0.0.1"},
		{"fly garbage", "10.0.0.1", "Fly-Client-IP", "10.0.0.1:1", []string{"192.0.2.1, garbage"}, "10.0.0.1"},
		{"ipv6", "fd00::/16", "", "[fd00::1]:1", []string{"2001:db8::1, fd00::2"}, "2001:db8::1"},
		{"unmapped", "", "", "[::ffff:192.0.2.1]:1", nil, "192.0.2.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := Parse(tt.proxies, tt.header)
			if e != nil {
				t.Fatal(e)
			}
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.peer
			for _, v := range tt.values {
				req.Header.Add(r.Header, v)
			}
			if got := r.Resolve(req); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
}
func TestConfig(t *testing.T) {
	for _, s := range []string{"garbage", "fe80::1%eth0", "0.0.0.0/0", "::/0", "10.0.0.0/7", "fd00::/15", "::ffff:0.0.0.0/96"} {
		if _, e := Parse(s, ""); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	if _, e := Parse("", "X-Real-IP"); e == nil {
		t.Fatal("accepted header")
	}
	if r, e := Parse("192.0.2.1, 2001:db8::1\t10.0.0.0/8", ""); e != nil || len(r.Proxies) != 3 {
		t.Fatal(r, e)
	}
}
func TestEntryCap(t *testing.T) {
	r, _ := Parse("10.0.0.0/8", "")
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:1"
	v := "192.0.2.1"
	for range 32 {
		v += ", 10.0.0.2"
	}
	req.Header.Set(r.Header, v)
	if r.Resolve(req) != "10.0.0.1" {
		t.Fatal("exceeded cap")
	}
}

func TestKey(t *testing.T) {
	for _, tt := range []struct{ addr, want string }{
		{"192.0.2.1", "192.0.2.1"}, {"::ffff:192.0.2.1", "192.0.2.1"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"}, {"2001:db8:1:2:ffff::abcd", "2001:db8:1:2::/64"},
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},
	} {
		if got := Key(tt.addr); got != tt.want {
			t.Errorf("Key(%q) = %q, want %q", tt.addr, got, tt.want)
		}
	}
}

func TestHeaderEdges(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "Fly-Client-IP"} {
		for _, value := range []string{"", " ", "192.0.2.1:5678", "[2001:db8::1]:443", ",192.0.2.1", "192.0.2.1,", "192.0.2.1, ,10.0.0.2", "fe80::1%eth0"} {
			// Leading entries before the first untrusted hop cannot influence XFF resolution.
			want := "10.0.0.1"
			if header == "X-Forwarded-For" && value == ",192.0.2.1" {
				want = "192.0.2.1"
			}
			if header == "X-Forwarded-For" && value == "192.0.2.1, ,10.0.0.2" {
				want = "10.0.0.2"
			}
			r, _ := Parse("10.0.0.0/8", header)
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = "10.0.0.1:1"
			req.Header.Set(header, value)
			if got := r.Resolve(req); got != want {
				t.Errorf("%s %q: %s want %s", header, value, got, want)
			}
		}
		r, _ := Parse("::ffff:10.0.0.0/104", header)
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "[::ffff:10.0.0.1]:1"
		req.Header.Set(strings.ToLower(header), "  ::ffff:192.0.2.1  ")
		if got := r.Resolve(req); got != "192.0.2.1" {
			t.Fatal(got)
		}
		req.RemoteAddr = "192.0.2.2:1"
		if got := r.Resolve(req); got != "192.0.2.2" {
			t.Fatal("untrusted header", got)
		}
	}
}
