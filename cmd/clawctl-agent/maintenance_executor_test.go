package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestMaintenanceExecutorDryRunDigestAndRootRefusal(t *testing.T) {
	spec, raw := mustMaintenanceSpec(t, false)
	dir := t.TempDir()
	var argv []string
	exec := maintenanceExecutor{
		now:  func() time.Time { return time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC) },
		euid: func() int { return 1000 },
		dir:  dir,
		run: func(_ context.Context, name string, args ...string) (string, string, error) {
			argv = append([]string{name}, args...)
			body, err := os.ReadFile(confArg(t, args))
			if err != nil {
				t.Fatal(err)
			}
			if maintenanceDigest(body) != spec.ConfigDigest {
				t.Fatalf("written digest %s", maintenanceDigest(body))
			}
			return maintenanceSummaryLine("dry-run", spec.ConfigDigest, ""), "", nil
		},
	}
	job := model.JobResponse{
		ResourceKind: maintenance.ResourceKind, ResourceID: maintenance.ResourceID,
		ArtifactDigest: spec.ConfigDigest, Irreversible: false, ExecutionTimeout: 30,
		Spec: raw,
	}
	got, err := exec.Run(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Passed || got[0].RuleID != maintenance.VerificationRuleID || got[0].ExitCode != 0 {
		t.Fatalf("verification %+v", got)
	}
	if !strings.Contains(got[0].Command, "--dry-run") || !strings.Contains(strings.Join(argv, " "), "--dry-run") {
		t.Fatalf("argv %v command %s", argv, got[0].Command)
	}
	if strings.Contains(got[0].Command, "sudo") {
		t.Fatal("executor used sudo")
	}
	info, err := os.Stat(argv[0])
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("script mode %v err %v", info, err)
	}

	// The user spec renders DRY_RUN=1 with no staged categories, so an
	// irreversible run must still report dry-run. A summary claiming "apply"
	// does not match the approved revision and fails.
	job.Irreversible = true
	for _, tc := range []struct {
		mode string
		pass bool
	}{{"dry-run", true}, {"apply", false}, {"mixed", false}} {
		argv = nil
		mode := tc.mode
		exec.run = func(_ context.Context, name string, args ...string) (string, string, error) {
			argv = append([]string{name}, args...)
			return maintenanceSummaryLine(mode, spec.ConfigDigest, ""), "", nil
		}
		got, err = exec.Run(context.Background(), job)
		if err != nil || got[0].Passed != tc.pass {
			t.Fatalf("irreversible mode=%s err=%v got=%+v", tc.mode, err, got)
		}
		if strings.Contains(strings.Join(argv, " "), "--dry-run") {
			t.Fatalf("apply argv included --dry-run: %v", argv)
		}
	}

	prev := maintenanceConfMutator
	maintenanceConfMutator = func(path string) {
		_ = os.WriteFile(path, []byte("DRY_RUN=1\n"), 0o600)
	}
	t.Cleanup(func() { maintenanceConfMutator = prev })
	called := false
	exec.run = func(context.Context, string, ...string) (string, string, error) {
		called = true
		return "", "", nil
	}
	_, err = exec.Run(context.Background(), job)
	var rejected *rejectError
	if !errors.As(err, &rejected) || rejected.Code != deploy.PreconditionFailed || called {
		t.Fatalf("digest mismatch err=%v called=%v", err, called)
	}

	maintenanceConfMutator = nil
	rootSpec, rootRaw := mustMaintenanceSpec(t, true)
	rootJob := model.JobResponse{
		ResourceKind: maintenance.ResourceKind, ResourceID: maintenance.ResourceID,
		ArtifactDigest: rootSpec.ConfigDigest, Irreversible: true, ExecutionTimeout: 30,
		Spec: rootRaw,
	}
	_, err = exec.Run(context.Background(), rootJob)
	if !errors.As(err, &rejected) || rejected.Code != deploy.PreconditionFailed || !strings.Contains(rejected.Detail, "not root") {
		t.Fatalf("root refusal err=%v", err)
	}
}

func mustMaintenanceSpec(t *testing.T, root bool) (maintenance.Spec, []byte) {
	t.Helper()
	var profile maintenance.Profile
	if root {
		dry := false
		hours := 168
		vartmp := 14
		profile = maintenance.Profile{
			SchemaVersion: maintenance.SchemaVersion, Scope: maintenance.ScopeRoot, DryRun: &dry,
			Categories: []string{"journal"}, TmpAgeDays: 10, VarTmpAgeDays: &vartmp,
			JournalMaxSize: "500M", JournalMaxAge: "30d", DockerUntilHours: &hours,
			AttentionPct: 90, Mount: "/",
		}
	} else {
		dry := true
		npm, pip, goc, thumb, trash, duT, duD := 0, 0, 0, 30, 30, 30, 2
		profile = maintenance.Profile{
			SchemaVersion: maintenance.SchemaVersion, Scope: maintenance.ScopeUser, DryRun: &dry,
			Categories: []string{"user_tmp"}, TmpDirs: []string{"/tmp"}, TmpAgeDays: 7,
			NpmCleanMinMB: &npm, PipCacheMinMB: &pip, GoCacheMinMB: &goc,
			ThumbAgeDays: &thumb, TrashAgeDays: &trash, DuTimeoutS: &duT, DuDepth: &duD,
			AttentionPct: 90, Mount: "/",
		}
	}
	spec, _, err := maintenance.BuildSpec(profile)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := maintenance.MarshalSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	return spec, raw
}

func maintenanceDigest(body []byte) string {
	return maintenance.Digest(body)
}

func confArg(t *testing.T, args []string) string {
	t.Helper()
	for i, arg := range args {
		if arg == "--conf" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("argv has no --conf: %v", args)
	return ""
}

func maintenanceSummaryLine(mode, digest, attention string) string {
	catMode := "dry-run"
	if mode == "apply" || mode == "mixed" {
		catMode = "apply"
	}
	return fmt.Sprintf(
		`{"schema":"fleet-disk-clean/v1","version":"1.1.0","scope":"user","host":"host-a","user":"agent","mode":%q,"ts":"2026-10-06T12:00:00Z","duration_s":1,"config_digest":%q,"disk":{"mount":"/","pct_before":10,"pct_after":10,"avail_bytes_before":1,"avail_bytes_after":1},"candidate_bytes":0,"freed_bytes":0,"categories":{"user_tmp":{"mode":%q,"status":"ok","candidate_bytes":0,"freed_bytes":0,"items":0,"note":"ok"}},"attention":%q,"weekly":null,"root":null}`,
		mode, digest, catMode, attention)
}
