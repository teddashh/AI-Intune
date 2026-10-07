package totp

import (
	"encoding/base32"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRFC6238(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for _, v := range []struct {
		at   int64
		code string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		got, err := Code(secret, v.at/30)
		if err != nil || got != v.code {
			t.Fatalf("%d: %s %v", v.at, got, err)
		}
		for _, offset := range []int64{-30, 0, 30} {
			step, ok := Verify(secret, v.code, time.Unix(v.at+offset, 0))
			if !ok || step != v.at/30 {
				t.Fatalf("skew %d", offset)
			}
		}
		if _, ok := Verify(secret, v.code, time.Unix(v.at+60, 0)); ok {
			t.Fatal("accepted outside skew")
		}
	}
}
func TestSecretURI(t *testing.T) {
	secret, err := NewSecret()
	if err != nil || len(secret) != 32 {
		t.Fatal(secret, err)
	}
	u, err := url.Parse(URI(secret, "a+b: c", "host.example:443"))
	if err != nil || u.Query().Get("issuer") != "clawctl Hub (host.example:443)" || !strings.Contains(u.Path, "a+b: c") || u.Query().Get("secret") != secret {
		t.Fatal(u, err)
	}
}

func TestURIEscapesLabelComponents(t *testing.T) {
	u, err := url.Parse(URI("SECRET", "user:name/?", "hub.example:443"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.EscapedPath(), "hub.example%3A443") || !strings.Contains(u.EscapedPath(), "user%3Aname%2F%3F") {
		t.Fatal(u.EscapedPath())
	}
}
