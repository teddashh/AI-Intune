//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestMaintenanceExecutorRefusesSharedDir(t *testing.T) {
	spec, raw := mustMaintenanceSpec(t, false)
	job := model.JobResponse{
		ResourceKind: maintenance.ResourceKind, ResourceID: maintenance.ResourceID,
		ArtifactDigest: spec.ConfigDigest, ExecutionTimeout: 30, Spec: raw,
	}
	ran := false
	run := func(context.Context, string, ...string) (string, string, error) { ran = true; return "", "", nil }

	shared := t.TempDir()
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"world-writable": shared, "symlink": link} {
		_, err := maintenanceExecutor{euid: func() int { return 1000 }, dir: dir, run: run}.Run(context.Background(), job)
		var rej *rejectError
		if !errors.As(err, &rej) || rej.Code != deploy.PreconditionFailed {
			t.Fatalf("%s: err = %v, want precondition rejection", name, err)
		}
		if ran {
			t.Fatalf("%s: script ran from a non-private directory", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "disk-clean")); err == nil {
			t.Fatalf("%s: script was written into a non-private directory", name)
		}
	}
}
