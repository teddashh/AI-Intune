package operator

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestDiskCleanPublishReplaysAndRefusesAProtectedGlob(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st)
	profile := diskCleanUserProfile()
	preview, err := svc.PreviewDiskCleanProfile("channel", "stable", profile)
	if err != nil {
		t.Fatal(err)
	}
	if preview.CurrentRevision != 0 || preview.PreviewDigest == "" || preview.ConfigDigest == "" || preview.Conf == "" {
		t.Fatalf("preview = %+v", preview)
	}
	expected := preview.CurrentRevision
	actor := Actor{SourceKind: SourceKindOperatorAPI, AuthSubject: "operator"}
	first, err := svc.PublishDiskCleanProfile(DiskCleanProfilePublishRequest{
		ScopeType: "channel", ScopeID: "stable", Profile: profile,
		ExpectedRevision: &expected, PreviewDigest: preview.PreviewDigest,
		ConfirmScopeID: "stable", Reason: "publish the user profile",
		IdempotencyKey: "pub-1", Actor: actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Unchanged || first.Revision <= 0 || first.ConfigDigest != preview.ConfigDigest {
		t.Fatalf("publish = %+v", first)
	}
	again, err := svc.PublishDiskCleanProfile(DiskCleanProfilePublishRequest{
		ScopeType: "channel", ScopeID: "stable", Profile: profile,
		ExpectedRevision: &expected, PreviewDigest: preview.PreviewDigest,
		ConfirmScopeID: "stable", Reason: "publish the user profile",
		IdempotencyKey: "pub-1", Actor: actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Revision != first.Revision || again.DesiredID != first.DesiredID {
		t.Fatalf("replay = %+v", again)
	}
	var conflict *store.OperatorRequestError
	_, err = svc.PublishDiskCleanProfile(DiskCleanProfilePublishRequest{
		ScopeType: "channel", ScopeID: "stable", Profile: profile,
		ExpectedRevision: &expected, PreviewDigest: preview.PreviewDigest,
		ConfirmScopeID: "stable", Reason: "a different reason",
		IdempotencyKey: "pub-1", Actor: actor,
	})
	if !errors.As(err, &conflict) || conflict.Code != store.OperatorCodeIdempotencyConflict {
		t.Fatalf("conflict = %v", err)
	}

	bad := profile
	bad.TmpGlobRules = []maintenance.TmpGlobRule{{Glob: "/tmp/*backup*", MinAgeDays: 7, KeepNewest: 0}}
	_, err = svc.PreviewDiskCleanProfile("channel", "stable", bad)
	if !errors.As(err, &conflict) || conflict.Code != store.OperatorCodeMaintenanceProfileInvalid {
		t.Fatalf("protected glob = %v", err)
	}
}

func diskCleanUserProfile() maintenance.Profile {
	dry := true
	npm, pip, goc := 1024, 200, 5120
	thumb, trash := 30, 30
	duT, duD := 180, 2
	return maintenance.Profile{
		SchemaVersion: maintenance.SchemaVersion,
		Scope:         maintenance.ScopeUser,
		DryRun:        &dry,
		Categories:    []string{"user_tmp"},
		TmpDirs:       []string{"/tmp"},
		TmpAgeDays:    7,
		NpmCleanMinMB: &npm, PipCacheMinMB: &pip, GoCacheMinMB: &goc,
		ThumbAgeDays: &thumb, TrashAgeDays: &trash,
		AttentionPct: 90, Mount: "/",
		DuTimeoutS: &duT, DuDepth: &duD,
	}
}
