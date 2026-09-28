package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func nodeProfileOperatorHTTPClient(t *testing.T, machineURL string) *operatorclient.Client {
	t.Helper()
	endpoint, err := url.Parse(machineURL)
	if err != nil || endpoint.Scheme != "http" || !net.ParseIP(endpoint.Hostname()).IsLoopback() {
		t.Fatal("operator test must dial the loopback Hub process")
	}
	// Match the Hub boundary fixture's literal authority, while routing every
	// socket to the test process. No Tailscale peer or LocalAPI is contacted.
	const authority = "100.64.200.2:8787"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// A fresh connection prevents net/http from transparently retrying an
	// idempotent POST, so the lost response reaches the caller under test.
	transport.DisableKeepAlives = true
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != authority {
			return nil, fmt.Errorf("unexpected operator test destination: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, endpoint.Host)
	}
	t.Cleanup(transport.CloseIdleConnections)
	client, err := operatorclient.NewWithHTTPClient("http://"+authority, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func nodeProfileGraphCounts(t *testing.T, f darwinNodeProfileFixture) [3]int {
	t.Helper()
	var counts [3]int
	err := f.store.DB().QueryRow(`SELECT
 (SELECT COUNT(*) FROM machine_profile_assignments WHERE machine_id=?),
 (SELECT COUNT(*) FROM desired_state WHERE scope_type='machine' AND scope_id=?),
 (SELECT COUNT(*) FROM jobs WHERE machine_id=?)`, f.job.MachineID, f.job.MachineID, f.job.MachineID).
		Scan(&counts[0], &counts[1], &counts[2])
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func assertNodeProfileStaleHTTPReplay(t *testing.T, f darwinNodeProfileFixture, client *operatorclient.Client,
	prior operator.MachineProfileAssignmentPreviewResult) {
	t.Helper()
	request := operator.MachineProfileAssignmentRequest{
		MachineID: f.job.MachineID, ProfileID: f.assignment.ProfileID, ProfileRevision: f.assignment.ProfileRevision,
		ConfirmDisplayName: prior.DisplayName, PreviewDigest: prior.PreviewDigest, Reason: "retry pinned Node",
	}
	before := nodeProfileGraphCounts(t, f)
	for _, replayed := range []bool{false, true} {
		_, err := client.AssignMachineProfile(t.Context(), "node-reassignment-stale", request)
		var rejected *operatorclient.APIError
		if !errors.As(err, &rejected) || rejected.StatusCode != http.StatusPreconditionFailed ||
			rejected.Code != store.OperatorCodeProfileAssignmentPreviewStale || rejected.Replayed != replayed {
			t.Fatalf("stale preview rejection/replay: %v", err)
		}
		if nodeProfileGraphCounts(t, f) != before {
			t.Fatal("stale preview rejection created an assignment, desired state or job")
		}
	}
}

func assignNodeProfileHTTPWithReplay(t *testing.T, f darwinNodeProfileFixture, client *operatorclient.Client,
	key string, request operator.MachineProfileAssignmentRequest, dropReply bool) operator.MachineProfileAssignmentResult {
	t.Helper()
	first, err := client.AssignMachineProfile(t.Context(), key, request)
	if dropReply {
		var rejected *operatorclient.APIError
		if err == nil || errors.As(err, &rejected) {
			t.Fatalf("expected lost assignment response, got %v", err)
		}
	} else if err != nil || first.Replayed {
		t.Fatalf("initial assignment response: replayed=%t err=%v", first.Replayed, err)
	}
	committed := nodeProfileGraphCounts(t, f)
	if committed != [3]int{2, 2, 2} {
		t.Fatalf("assignment graph before response replay=%v want [2 2 2]", committed)
	}
	replayed, err := client.AssignMachineProfile(t.Context(), key, request)
	if err != nil || !replayed.Replayed {
		t.Fatalf("assignment response replay: replayed=%t err=%v", replayed.Replayed, err)
	}
	if !dropReply {
		first.Replayed = true
		if !reflect.DeepEqual(first, replayed) {
			t.Fatal("replay changed the assignment receipt")
		}
	}
	if nodeProfileGraphCounts(t, f) != committed {
		t.Fatal("response replay duplicated an assignment, desired state or job")
	}
	return replayed
}

func assertNodeProfileOperatorAudit(t *testing.T, f darwinNodeProfileFixture) {
	t.Helper()
	entries, err := f.store.Audit(f.job.MachineID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for key, success := range map[string]bool{
		"node-reassignment-stale": false, "node-reassignment": true, "node-recovered-noop": true,
	} {
		var original, replayed int
		var digest string
		for _, entry := range entries {
			if entry.IdempotencyKey != key {
				continue
			}
			if digest == "" {
				digest = entry.RequestDigest
			}
			if entry.Action != store.AuditMachineProfileAssign || entry.OK != success ||
				entry.RequestDigest != digest || !strings.HasPrefix(digest, "sha256:") ||
				entry.SourceKind != operator.SourceKindOperatorAPI || entry.UserAgent != operatorclient.UserAgent ||
				entry.AuthSubject != "tailscale-user:42" || entry.AuthNodeID != "node-stable-1" ||
				entry.AuthCapability != "example.com/cap/clawctl-admin" || entry.AuthMethod != operatorauth.AuthMethodLocalAPI ||
				entry.AuthDecision != string(operatorauth.Authorized) {
				t.Errorf("operator audit identity/outcome mismatch for key %s", key)
			}
			if !success && !strings.Contains(entry.Detail, store.OperatorCodeProfileAssignmentPreviewStale) {
				t.Errorf("stale preview audit lost its rejection code for key %s", key)
			}
			if entry.IsOperatorReplay() {
				replayed++
			} else {
				original++
			}
		}
		if original != 1 || replayed != 1 {
			t.Errorf("audit key=%s original=%d replayed=%d want one each", key, original, replayed)
		}
	}
}
