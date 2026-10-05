package main

import "testing"

func TestServeListenDefaultFromEnv(t *testing.T) {
	t.Setenv("CLAWCTL_LISTEN", "100.64.0.9:8787")
	if got := serveListenDefault(); got != "100.64.0.9:8787" {
		t.Fatalf("serveListenDefault()=%q, want env Tailscale listener", got)
	}
}

func TestServeListenDefaultFallback(t *testing.T) {
	t.Setenv("CLAWCTL_LISTEN", "")
	if got := serveListenDefault(); got != "127.0.0.1:8770" {
		t.Fatalf("serveListenDefault()=%q, want fail-closed loopback placeholder", got)
	}
}
