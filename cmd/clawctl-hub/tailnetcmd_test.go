package main

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

func tailnetCLIFixture(t *testing.T) (jobsFixture, string, tailnetCommandDeps, *tailnet.Cache) {
	t.Helper()
	f := newJobsFixture(t, "tailnet-cli-machine")
	cache := tailnet.NewCache()
	cache.SetStatus(tailnet.Status{
		Available: true, ObservedAt: time.Now().UTC().Truncate(time.Second),
		Self:  tailnet.Peer{StableID: "node-hub", Hostname: "hub", IP: "100.64.0.1", Online: true},
		Peers: []tailnet.Peer{{StableID: "node-laptop", Hostname: "laptop", IP: "100.64.0.2", Online: true}},
	})
	(&hub{store: f.store, tailnet: cache}).operatorRoutes(f.mux)
	server := httptest.NewServer(f.mux)
	t.Cleanup(server.Close)
	base, machineDeps := machineHTTPTestDeps(t, server)
	machineDeps.discoverHubURL = func() (string, error) { return base, nil }
	return f, base, tailnetCommandDeps{
		machineCommandDeps: machineDeps,
		newTailnetSource:   func() operator.TailnetSource { return cache },
		now:                time.Now,
	}, cache
}

func TestTailnetCLIHTTPReadPreviewApplyAndRetry(t *testing.T) {
	f, base, deps, cache := tailnetCLIFixture(t)
	var out, errOut bytes.Buffer
	if err := runTailnetCommandWithDeps(t.Context(), []string{"--hub-url", base, "--json"}, &out, &errOut, deps); err != nil {
		t.Fatalf("read: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), `"peer_count": 2`) || !strings.Contains(out.String(), `"stable_id": "node-laptop"`) {
		t.Fatalf("read output=%s", out.String())
	}
	out.Reset()
	if err := runTailnetCommandWithDeps(t.Context(), []string{
		"ignore", "--hub-url", base, "--peer-id", "node-laptop", "--reason", "personal device", "--preview",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("preview: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), `hostname="laptop"`) || !strings.Contains(out.String(), "preview-digest=sha256:") {
		t.Fatalf("preview output=%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if err := runTailnetCommandWithDeps(t.Context(), []string{
		"ignore", "--hub-url", base, "--peer-id", "node-laptop", "--reason", "personal device",
		"--confirm-hostname", "laptop",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("apply: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "ignored=false→true") || !strings.Contains(out.String(), "replayed=false") {
		t.Fatalf("apply output=%s", out.String())
	}
	coordinates := regexp.MustCompile(`idempotency-key=([^ ]+) expected-revision=([0-9]+) preview-digest=([^ ]+) expires-at=([^\n]+)`).FindStringSubmatch(errOut.String())
	if len(coordinates) != 5 {
		t.Fatalf("missing retry coordinates: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if err := runTailnetCommandWithDeps(t.Context(), []string{
		"ignore", "--hub-url", base, "--peer-id", "node-laptop", "--reason", "personal device",
		"--confirm-hostname", "laptop", "--idempotency-key", coordinates[1],
		"--expected-revision", coordinates[2], "--preview-digest", coordinates[3], "--expires-at", coordinates[4],
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("retry: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "replayed=true") || !strings.Contains(out.String(), "authoritative current") {
		t.Fatalf("retry output=%s", out.String())
	}
	entries, err := f.store.Audit("", 10)
	if err != nil || len(entries) < 2 || entries[0].UserAgent != operatorclient.UserAgent {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}

	cache.SetStatus(tailnet.Status{Unavailable: "tailscaled unavailable"})
	out.Reset()
	errOut.Reset()
	err = runTailnetCommandWithDeps(t.Context(), []string{"--hub-url", base}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "tailscaled unavailable") ||
		!strings.Contains(out.String(), "local-ignore-rules=1") || !strings.Contains(out.String(), `peer-id="node-laptop"`) {
		t.Fatalf("unavailable read output=%s stderr=%s err=%v", out.String(), errOut.String(), err)
	}
}

func TestTailnetCLIRejectsIncompleteMutationBeforeDiscovery(t *testing.T) {
	var discoveries int
	deps := tailnetCommandDeps{
		machineCommandDeps: machineCommandDeps{
			discoverHubURL: func() (string, error) { discoveries++; return "", errors.New("must not run") },
		},
		now: time.Now,
	}
	for _, args := range [][]string{
		{"ignore", "--peer-id", "node", "--reason", "reason"},
		{"ignore", "--peer-id", "node", "--reason", "reason", "--confirm-hostname", "host", "--idempotency-key", "key"},
		{"ignore", "--peer-id", "node", "--reason", "reason", "--preview", "--confirm-hostname", "host"},
		{"unignore", "--peer-id", "node", "--reason", "reason", "--confirm-hostname", "host", "--days", "3"},
	} {
		if err := runTailnetCommandWithDeps(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{}, deps); err == nil {
			t.Fatalf("invalid args accepted: %q", args)
		}
	}
	if discoveries != 0 {
		t.Fatalf("invalid mutation attempted discovery %d times", discoveries)
	}
}
