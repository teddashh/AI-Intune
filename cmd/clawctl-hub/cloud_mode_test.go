package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/web"
)

func TestPublicBaseCloud(t *testing.T) {
	for _, mode := range []string{"local", "both"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CLAWCTL_AUTH_MODE", mode)
			for _, tt := range []struct{ listen, public, want string }{
				{"0.0.0.0:8787", "https://Hub.Example.com:443/", "https://hub.example.com"},
				{"[::]:8787", "https://hub.example.com:8443", "https://hub.example.com:8443"},
				{"127.0.0.1:8787", "http://localhost:80/", "http://localhost"},
				{"127.0.0.1:8787", "", ""},
				{"127.0.0.1:8787", "http://hub.example.com", ""},
				{"localhost:8787", "https://hub.example.com", ""},
			} {
				t.Setenv("CLAWCTL_PUBLIC_URL", tt.public)
				got, why := publicBase(tt.listen)
				if got != tt.want || (why == "") != (tt.want != "") {
					t.Fatalf("%+v: %q, %q", tt, got, why)
				}
			}
		})
	}
	t.Setenv("CLAWCTL_AUTH_MODE", "")
	if _, why := publicBase("127.0.0.1:8787"); why == "" {
		t.Fatal("default accepted loopback")
	}
	if _, err := operatorDestinationFromListen("127.0.0.1:8787"); err == nil {
		t.Fatal("default destination accepted loopback")
	}
	t.Setenv("CLAWCTL_AUTH_MODE", "invalid")
	if _, why := publicBase("100.64.0.1:8787"); why == "" {
		t.Fatal("unknown mode accepted")
	}
	t.Setenv("CLAWCTL_PUBLIC_URL", "https://hub.example.com")
	if got, why := publicBase("127.0.0.1:8787", "local"); got != "https://hub.example.com" || why != "" {
		t.Fatal("explicit mode did not override env")
	}
}

func TestAuthMode(t *testing.T) {
	for _, raw := range []string{"", "tailscale", "local", "both"} {
		if _, err := parseAuthMode(raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"other", "LOCAL", " local", "both "} {
		if _, err := parseAuthMode(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestCloudRoutingWithoutTailscale(t *testing.T) {
	for _, mode := range []authMode{authModeLocal, authModeBoth} {
		t.Run(string(mode), func(t *testing.T) {
			t.Setenv("CLAWCTL_PUBLIC_URL", "https://Hub.Example.com:443")
			config, err := cloudConfiguration(mode, "127.0.0.1:8787")
			if err != nil {
				t.Fatal(err)
			}
			// An empty capability prefix would make construction of LocalAPI auth fail.
			auth, err := configuredOperatorAuthorizer(mode, "127.0.0.1:8787", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := auth.(denyOperatorAuthorizer); !ok {
				t.Fatal("constructed Tailscale authorizer")
			}
			st := boundaryStore(t)
			ui, err := web.New(st, "hub")
			if err != nil {
				t.Fatal(err)
			}
			handler, err := newHubHTTPHandler(&hub{store: st}, ui, auth, config.public.Authority(), config)
			if err != nil {
				t.Fatal(err)
			}
			for _, tt := range []struct {
				host, forwarded, path, code string
				status                      int
			}{
				{"hub.example.com", "evil.example", "/v1/operator/machines", string(operatorauth.Unauthenticated), 401},
				{"hub.example.com:443", "", "/v1/operator/machines", string(operatorauth.Unauthenticated), 401},
				{"HUB.EXAMPLE.COM", "", "/v1/operator/machines", string(operatorauth.Unauthenticated), 401},
				{"evil.example", "hub.example.com", "/v1/operator/machines", operatorAuthorityDecisionCode, 421},
				{"hub.example.com:80", "hub.example.com", "/v1/operator/machines", operatorAuthorityDecisionCode, 421},
				{"127.0.0.1:8787", "hub.example.com", "/v1/operator/machines", operatorAuthorityDecisionCode, 421},
				{"hub.example.com", "", "/healthz", "", 200},
			} {
				req := httptest.NewRequest("GET", tt.path, nil)
				req.Host = tt.host
				req.Header.Set("X-Forwarded-Host", tt.forwarded)
				req.Header.Set("X-Forwarded-For", "100.64.0.10")
				req.Header.Set("X-Forwarded-Proto", "https")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.code) {
					t.Fatalf("%+v: %d %s", tt, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestCloudBothTailnetAndCSP(t *testing.T) {
	t.Setenv("CLAWCTL_PUBLIC_URL", "https://hub.example.com")
	for _, mode := range []authMode{authModeLocal, authModeBoth} {
		config, err := cloudConfiguration(mode, testOperatorAuthority)
		if err != nil {
			t.Fatal(err)
		}
		if config.matches(testOperatorAuthority) != (mode == authModeBoth) {
			t.Fatal("wrong tailnet allowlist")
		}
		auth, err := configuredOperatorAuthorizer(mode, testOperatorAuthority, "example.com/cap/clawctl")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := auth.(*operatorauth.Authorizer); ok != (mode == authModeBoth) {
			t.Fatal("wrong authorizer")
		}
		boundary := newOperatorBoundary(http.NewServeMux(), auth, nil, nil, config.public.Authority(), config)
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Forwarded-Proto", "http")
		rec := httptest.NewRecorder()
		boundary.writeSecurityHeaders(rec, req, operatorSecurityTerminal)
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "connect-src wss://hub.example.com;") {
			t.Fatal(rec.Header())
		}
		for _, origin := range []string{"https://hub.example.com", "https://hub.example.com:443"} {
			req.Header.Set("Origin", origin)
			if !operatorTerminalOriginAllowed(req, config.public.Authority(), config) {
				t.Fatal("cloud origin refused")
			}
		}
		for _, origin := range []string{"http://hub.example.com", "https://evil.example", "https://hub.example.com?", "https://hub.example.com/"} {
			req.Header.Set("Origin", origin)
			if operatorTerminalOriginAllowed(req, config.public.Authority(), config) {
				t.Fatal("bad origin accepted")
			}
		}
	}
}
