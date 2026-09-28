package operator

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMachineLifecycleSemanticDigestBindsIntentButNotActor(t *testing.T) {
	revision := int64(7)
	base := MachineLifecycleRequest{
		MachineID: "machine-1", DesiredState: MachineLifecycleStateRetired,
		ExpectedRevision: &revision, ConfirmDisplayName: "samplehub1",
		PreviewDigest: "sha256:preview", Reason: "compromised credential",
		IdempotencyKey: "key", Actor: Actor{SourceAddr: "100.64.0.1", AuthSubject: "user:1"},
	}
	want := MachineLifecycleSemanticDigest(base)
	actorChanged := base
	actorChanged.Actor = Actor{SourceAddr: "100.64.0.2", AuthSubject: "user:2"}
	actorChanged.IdempotencyKey = "another-key"
	if got := MachineLifecycleSemanticDigest(actorChanged); got != want {
		t.Fatalf("transport evidence changed semantic digest: got=%q want=%q", got, want)
	}
	tests := []struct {
		name string
		edit func(*MachineLifecycleRequest)
	}{
		{"machine", func(r *MachineLifecycleRequest) { r.MachineID += "x" }},
		{"desired state", func(r *MachineLifecycleRequest) { r.DesiredState = MachineLifecycleStateActive }},
		{"expected revision", func(r *MachineLifecycleRequest) { value := int64(8); r.ExpectedRevision = &value }},
		{"missing expected revision", func(r *MachineLifecycleRequest) { r.ExpectedRevision = nil }},
		{"confirmation", func(r *MachineLifecycleRequest) { r.ConfirmDisplayName += "x" }},
		{"preview", func(r *MachineLifecycleRequest) { r.PreviewDigest += "x" }},
		{"reason", func(r *MachineLifecycleRequest) { r.Reason += "x" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.edit(&changed)
			if got := MachineLifecycleSemanticDigest(changed); got == want {
				t.Fatalf("%s was not bound into digest %q", tc.name, got)
			}
		})
	}
}

func TestMachineLifecycleServiceReadPreviewApplyReplayAndAuditProvenance(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	machineID, _, err := st.CreateEnrollTokenFor("service-lifecycle", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := New(st)
	read, err := service.MachineLifecycle(machineID)
	if err != nil || read.State != store.MachineLifecycleActive || read.LifecycleRevision != 0 {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	revision := read.LifecycleRevision
	preview, err := service.PreviewMachineLifecycle(MachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleStateRetired, ExpectedRevision: &revision,
	})
	if err != nil || preview.PreviewDigest == "" || preview.DisplayName != "service-lifecycle" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	req := MachineLifecycleRequest{
		MachineID: machineID, DesiredState: MachineLifecycleStateRetired,
		ExpectedRevision: &revision, ConfirmDisplayName: preview.DisplayName,
		PreviewDigest: preview.PreviewDigest, Reason: "retire unused enrollment",
		IdempotencyKey: "service-lifecycle-key",
		Actor: Actor{
			SourceAddr: "100.64.0.9", WhoNode: "admin-laptop", WhoUser: "owner@example.com",
			AuthSubject: "tailscale-user:9", AuthNodeID: "node-9",
			AuthCapability: "example.com/cap/clawctl-admin", AuthMethod: "tailscale-localapi",
			AuthDecision: "allow", SourceKind: SourceKindOperatorAPI,
		},
	}
	result, err := service.ChangeMachineLifecycle(req)
	if err != nil || !result.Changed || result.NoOp || result.LifecycleRevision != 1 ||
		result.State != store.MachineLifecycleRetired || !result.Audited || result.Replayed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	entries, err := st.Audit(machineID, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	if entry.Action != store.AuditMachineLifecycle || entry.IdempotencyKey != req.IdempotencyKey ||
		entry.RequestDigest != MachineLifecycleSemanticDigest(req) || entry.AuthSubject != req.Actor.AuthSubject ||
		entry.AuthNodeID != req.Actor.AuthNodeID || entry.SourceKind != req.Actor.SourceKind || !entry.OK {
		t.Fatalf("audit provenance=%+v", entry)
	}
	replay, err := service.ChangeMachineLifecycle(req)
	if err != nil || !replay.Replayed || replay.LifecycleRevision != 1 ||
		replay.TransitionEventID == nil || result.TransitionEventID == nil ||
		*replay.TransitionEventID != *result.TransitionEventID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestMachineLifecycleServiceFallbackAuditUsesFixedAction(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	service := New(st)
	revision := int64(0)
	req := MachineLifecycleRequest{
		MachineID: "missing", DesiredState: "malformed-state", ExpectedRevision: &revision,
		ConfirmDisplayName: "missing", PreviewDigest: "sha256:bad", Reason: "attempt",
		Actor: Actor{SourceAddr: "local-test", SourceKind: SourceKindDirectDBCLI},
	}
	_, err = service.ChangeMachineLifecycle(req)
	if !errors.Is(err, store.ErrIdempotencyKeyRequired) {
		t.Fatalf("missing key error=%v", err)
	}
	entries, auditErr := st.Audit("missing", 10)
	if auditErr != nil || len(entries) != 1 || entries[0].Action != store.AuditMachineLifecycle || entries[0].OK {
		t.Fatalf("fallback audit=%+v err=%v", entries, auditErr)
	}
}
