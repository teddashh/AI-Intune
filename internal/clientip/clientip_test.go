package clientip

import (
	"net/http/httptest"
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
