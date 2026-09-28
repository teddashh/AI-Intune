package operator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

type fixedTailnetSource struct{ status tailnet.Status }

func (s *fixedTailnetSource) Get(context.Context) tailnet.Status { return s.status }

func newTailnetOperatorTestService(t *testing.T, status tailnet.Status) (*Service, *store.Store, *fixedTailnetSource) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	source := &fixedTailnetSource{status: status}
	return NewWithTailnet(st, source), st, source
}

func tailnetOperatorTestStatus(now time.Time) tailnet.Status {
	return tailnet.Status{
		Available: true, ObservedAt: now,
		Self: tailnet.Peer{StableID: "node-hub", Hostname: "hub", IP: "100.64.0.1", Online: true},
		Peers: []tailnet.Peer{
			{StableID: "node-laptop", Hostname: "laptop", IP: "100.64.0.2", OS: "windows", Online: true},
		},
	}
}

func TestTailnetOperatorPreviewApplyReplayAndOverview(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	svc, st, source := newTailnetOperatorTestService(t, tailnetOperatorTestStatus(now))
	request := TailnetPeerIgnorePreviewRequest{
		PeerID: " node-laptop ", Action: "ignore", ExpiresAt: now.Add(30 * 24 * time.Hour),
		Reason: " personal workstation ",
	}
	preview, err := svc.PreviewTailnetPeerIgnore(t.Context(), request, now)
	if err != nil || preview.PeerID != "node-laptop" || preview.Hostname != "laptop" ||
		preview.CurrentlyIgnored || !preview.IgnoredAfter || preview.ExpectedRevision != 0 ||
		preview.Reason != "personal workstation" || preview.PreviewDigest == "" || !preview.ObservedAt.Equal(now) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	apply := TailnetPeerIgnoreApplyRequest{
		TailnetPeerIgnorePreviewRequest: TailnetPeerIgnorePreviewRequest{
			PeerID: preview.PeerID, Action: preview.Action, ExpiresAt: preview.ExpiresAt, Reason: preview.Reason,
		},
		ExpectedRevision: preview.ExpectedRevision, ConfirmHostname: preview.Hostname,
		PreviewDigest: preview.PreviewDigest, IdempotencyKey: "operator-tailnet-1",
		Actor: Actor{AuthSubject: "tailscale-user:42", AuthCapability: "example.com/cap/clawctl-admin", SourceKind: SourceKindOperatorAPI},
	}
	first, err := svc.ApplyTailnetPeerIgnore(t.Context(), apply, now)
	if err != nil || !first.Ignored || first.Revision != 1 || first.Replayed {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	// Replay meaning is fixed by the idempotency receipt, not by a later
	// Tailnet observation. A rename must not turn the same request into a new
	// confirmation failure or corrupt-cache verdict.
	source.status.Peers[0].Hostname = "renamed-laptop"
	replay, err := svc.ApplyTailnetPeerIgnore(t.Context(), apply, now)
	if err != nil || !replay.Replayed || replay.Revision != 1 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	overview, err := svc.Tailnet(t.Context(), now)
	if err != nil || !overview.Available || overview.PeerCount != 2 || len(overview.Ignored) != 1 ||
		len(overview.Unenrolled) != 1 || overview.Unenrolled[0].StableID != "node-hub" {
		t.Fatalf("overview=%+v err=%v", overview, err)
	}
	entries, err := st.ListAuditReads(store.AuditReadFilter{Actions: []store.AuditAction{store.AuditTailnetPeerIgnore}, Limit: 10})
	if err != nil || len(entries.Items) != 2 || entries.Items[0].AuthSubject != "tailscale-user:42" ||
		!entries.Items[0].IsOperatorReplay() {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}

func TestTailnetOperatorFailsClosedOnSourceAndPreviewChanges(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	svc, _, source := newTailnetOperatorTestService(t, tailnet.Status{Unavailable: "tailscaled unavailable"})
	_, err := svc.PreviewTailnetPeerIgnore(t.Context(), TailnetPeerIgnorePreviewRequest{
		PeerID: "node-laptop", Action: "ignore", ExpiresAt: now.Add(24 * time.Hour), Reason: "test",
	}, now)
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeTailnetSourceUnavailable {
		t.Fatalf("source rejection=%#v err=%v", rejection, err)
	}

	source.status = tailnetOperatorTestStatus(now)
	request := TailnetPeerIgnorePreviewRequest{
		PeerID: "node-laptop", Action: "ignore", ExpiresAt: now.Add(24 * time.Hour), Reason: "test",
	}
	preview, err := svc.PreviewTailnetPeerIgnore(t.Context(), request, now)
	if err != nil {
		t.Fatal(err)
	}
	source.status.Peers[0].Hostname = "renamed-laptop"
	_, err = svc.ApplyTailnetPeerIgnore(t.Context(), TailnetPeerIgnoreApplyRequest{
		TailnetPeerIgnorePreviewRequest: request,
		ExpectedRevision:                preview.ExpectedRevision, ConfirmHostname: preview.Hostname,
		PreviewDigest: preview.PreviewDigest, IdempotencyKey: "stale-tailnet-preview",
	}, now)
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeConfirmationMismatch {
		t.Fatalf("renamed peer rejection=%#v err=%v", rejection, err)
	}
}

func TestTailnetOperatorKeepsPersistedRulesManageableWhenSourceIsUnavailable(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	svc, _, source := newTailnetOperatorTestService(t, tailnetOperatorTestStatus(now))
	request := TailnetPeerIgnorePreviewRequest{
		PeerID: "node-laptop", Action: "ignore", ExpiresAt: now.Add(24 * time.Hour), Reason: "first",
	}
	preview, err := svc.PreviewTailnetPeerIgnore(t.Context(), request, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ApplyTailnetPeerIgnore(t.Context(), TailnetPeerIgnoreApplyRequest{
		TailnetPeerIgnorePreviewRequest: request, ExpectedRevision: 0,
		ConfirmHostname: preview.Hostname, PreviewDigest: preview.PreviewDigest,
		IdempotencyKey: "ignore-before-outage",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	source.status = tailnet.Status{Unavailable: "tailscaled unavailable"}
	overview, err := svc.Tailnet(t.Context(), now)
	if err != nil || overview.Available || overview.Unavailable != "tailscaled unavailable" ||
		len(overview.Ignored) != 1 || overview.Ignored[0].PeerID != "node-laptop" || len(overview.Unenrolled) != 0 {
		t.Fatalf("unavailable overview=%+v err=%v", overview, err)
	}
	unignore, err := svc.PreviewTailnetPeerIgnore(t.Context(), TailnetPeerIgnorePreviewRequest{
		PeerID: "node-laptop", Action: "unignore", Reason: "remove exception",
	}, now)
	if err != nil || unignore.Hostname != "laptop" || !unignore.CurrentlyIgnored || unignore.IgnoredAfter || unignore.ExpectedRevision != 1 {
		t.Fatalf("unignore preview=%+v err=%v", unignore, err)
	}
}
