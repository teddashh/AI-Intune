package main

import (
	"runtime/debug"
	"testing"
)

func TestSoftMemoryLimitDefaultsWhenUnset(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	t.Setenv("GOMEMLIMIT", "")
	applySoftMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != softMemoryLimitBytes {
		t.Fatalf("soft limit = %d, want %d", got, softMemoryLimitBytes)
	}
}

func TestSoftMemoryLimitRespectsGOMEMLIMIT(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	t.Setenv("GOMEMLIMIT", "64MiB")
	applySoftMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != prev {
		t.Fatalf("GOMEMLIMIT is set but the limit changed from %d to %d", prev, got)
	}
}
