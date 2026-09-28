package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/artifact"
)

func TestCanonicalServeDBPathMakesRelativeFlagSafeForArtifactConstructor(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := canonicalServeDBPath(filepath.Join("state", "..", "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workingDirectory, "hub.sqlite")
	if got != want || !filepath.IsAbs(got) || filepath.Clean(got) != got {
		t.Fatalf("canonical path=%q want=%q", got, want)
	}
	artifactsDir := artifactsDirFor(got)
	if _, err := artifact.NewFetcher(artifactsDir); err != nil {
		t.Fatalf("canonical serve artifact directory rejected by production constructor: %v", err)
	}
}

func TestCanonicalServeDBPathResolvesRelativeEnvironmentDefault(t *testing.T) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWCTL_DB", filepath.Join("relative-state", "ledger.sqlite"))
	got, err := canonicalServeDBPath(defaultDB())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workingDirectory, "relative-state", "ledger.sqlite")
	if got != want {
		t.Fatalf("canonical environment path=%q want=%q", got, want)
	}
}

func TestServeCanonicalizesDBBeforeLockAndStore(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	serveAt := strings.Index(string(raw), "func serve(argv []string)")
	if serveAt < 0 {
		t.Fatal("main.go has no serve function")
	}
	body := string(raw[serveAt:])
	canonicalAt := strings.Index(body, "canonicalServeDBPath(*dbPath)")
	lockAt := strings.Index(body, "ledgerlock.AcquireWriter(*dbPath)")
	storeAt := strings.Index(body, "openServeStore(*dbPath)")
	artifactAt := strings.Index(body, "artifactsDirFor(*dbPath)")
	if canonicalAt < 0 || lockAt < 0 || storeAt < 0 || artifactAt < 0 ||
		canonicalAt > lockAt || canonicalAt > storeAt || canonicalAt > artifactAt {
		t.Fatalf("serve must canonicalize DB before lock/store/artifact construction: canonical=%d lock=%d store=%d artifact=%d",
			canonicalAt, lockAt, storeAt, artifactAt)
	}
}
