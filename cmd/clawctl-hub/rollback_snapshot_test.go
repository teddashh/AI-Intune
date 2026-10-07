package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/store"
)

func privateRollbackTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("make rollback fixture parent private: %v", err)
	}
	return dir
}

func TestDisableCurrentExecutableSupportsUnlinkedPrivateProbe(t *testing.T) {
	if os.Getenv("CLAWCTL_TEST_DISABLE_UNLINKED_EXE") == "1" {
		if err := disableCurrentExecutableForMaintenance(); err != nil {
			t.Fatalf("disable private executable: %v", err)
		}
		return
	}

	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(running)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	probePath := filepath.Join(t.TempDir(), "clawctl-hub.probe")
	writableProbe, err := os.OpenFile(probePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(writableProbe, source); err != nil {
		_ = writableProbe.Close()
		t.Fatal(err)
	}
	if err := writableProbe.Sync(); err != nil {
		_ = writableProbe.Close()
		t.Fatal(err)
	}
	if err := writableProbe.Close(); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(probePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(probePath); err != nil {
		_ = probe.Close()
		t.Fatal(err)
	}

	cmd := exec.Command("/proc/self/fd/3", "-test.run=^TestDisableCurrentExecutableSupportsUnlinkedPrivateProbe$")
	cmd.ExtraFiles = []*os.File{probe}
	cmd.Env = append(os.Environ(), "CLAWCTL_TEST_DISABLE_UNLINKED_EXE=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = probe.Close()
		t.Fatalf("run unlinked private probe: %v\n%s", err, output)
	}
	info, err := probe.Stat()
	if err != nil {
		_ = probe.Close()
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		_ = probe.Close()
		t.Fatalf("private executable mode=%#o, want 0600", got)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunRollbackSnapshotCreatesPrivateMarkerAndStandaloneBackup(t *testing.T) {
	dir := privateRollbackTempDir(t)
	source := filepath.Join(dir, "hub.sqlite")
	destination := filepath.Join(dir, "backup.sqlite")
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runRollbackSnapshot([]string{
		"--rollback-snapshot", "--db", source, "--out", destination,
	}, &out); err != nil {
		t.Fatalf("rollback snapshot command: %v", err)
	}
	if !strings.Contains(out.String(), "snapshot") || !strings.Contains(out.String(), "ledger is quiescent") {
		t.Fatalf("snapshot output=%q", out.String())
	}
	destinationInfo, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if got := destinationInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("snapshot mode=%#o, want 0600", got)
	}
	marker := upgradeMaintenanceMarker(source)
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("maintenance marker missing: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("marker mode=%#o, want 0600", got)
	}
}

func TestRunRollbackSnapshotDoesNotTakeOverExistingMarker(t *testing.T) {
	dir := privateRollbackTempDir(t)
	source := filepath.Join(dir, "hub.sqlite")
	destination := filepath.Join(dir, "backup.sqlite")
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	marker := upgradeMaintenanceMarker(source)
	const owner = "another upgrade owns this\n"
	if err := os.WriteFile(marker, []byte(owner), 0o600); err != nil {
		t.Fatal(err)
	}

	err = runRollbackSnapshot([]string{
		"--rollback-snapshot", "--db", source, "--out", destination,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "Hub upgrade maintenance in progress") {
		t.Fatalf("existing marker was accepted: %v", err)
	}
	got, readErr := os.ReadFile(marker)
	if readErr != nil || string(got) != owner {
		t.Fatalf("existing marker was replaced: got=%q err=%v", got, readErr)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("snapshot started despite existing marker: %v", statErr)
	}
}

func TestRunRollbackSnapshotAcceptsOnlyVerifiedPreexistingMarker(t *testing.T) {
	dir := privateRollbackTempDir(t)
	source := filepath.Join(dir, "hub.sqlite")
	destination := filepath.Join(dir, "backup.sqlite")
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runUpgradeMaintenanceBegin([]string{
		"--upgrade-maintenance-begin", "--db", source,
	}, &bytes.Buffer{}, func() error { return nil }); err != nil {
		t.Fatalf("begin maintenance: %v", err)
	}
	if err := runRollbackSnapshot([]string{
		"--rollback-snapshot", "--maintenance-already-held", "--db", source, "--out", destination,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("snapshot with established marker: %v", err)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
}

func TestRunRollbackSnapshotRejectsUnsafePreexistingMarker(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "wrong contents", prepare: func(t *testing.T, marker string) {
			if err := os.WriteFile(marker, []byte("someone else\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "group readable", prepare: func(t *testing.T, marker string) {
			if err := os.WriteFile(marker, []byte(upgradeMaintenanceContents), 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", prepare: func(t *testing.T, marker string) {
			victim := marker + ".victim"
			if err := os.WriteFile(victim, []byte(upgradeMaintenanceContents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, marker); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", prepare: func(t *testing.T, marker string) {
			victim := marker + ".victim"
			if err := os.WriteFile(victim, []byte(upgradeMaintenanceContents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(victim, marker); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := privateRollbackTempDir(t)
			source := filepath.Join(dir, "hub.sqlite")
			destination := filepath.Join(dir, "backup.sqlite")
			fixture.prepare(t, upgradeMaintenanceMarker(source))
			err := runRollbackSnapshot([]string{
				"--rollback-snapshot", "--maintenance-already-held", "--db", source, "--out", destination,
			}, &bytes.Buffer{})
			if err == nil {
				t.Fatal("unsafe established marker was accepted")
			}
			if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("unsafe marker reached snapshot: %v", statErr)
			}
		})
	}
}

func TestRunUpgradeMaintenanceBeginIsExclusiveAndRejectsExtraArguments(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.sqlite")
	var out bytes.Buffer
	if err := runUpgradeMaintenanceBegin([]string{
		"--upgrade-maintenance-begin", "--db", dbPath,
	}, &out, func() error { return nil }); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if !strings.Contains(out.String(), "marker created") {
		t.Fatalf("begin output=%q", out.String())
	}
	if err := runUpgradeMaintenanceBegin([]string{
		"--upgrade-maintenance-begin", "--db", dbPath,
	}, &bytes.Buffer{}, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "Hub upgrade maintenance in progress") {
		t.Fatalf("duplicate begin err=%v", err)
	}
	if err := runUpgradeMaintenanceBegin([]string{
		"--upgrade-maintenance-begin", "--db", filepath.Join(t.TempDir(), "other.sqlite"), "extra",
	}, &bytes.Buffer{}, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("extra argument err=%v", err)
	}
}

func TestRunRollbackSnapshotLeavesMarkerOnFailClosedRefusal(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			dir := privateRollbackTempDir(t)
			source := filepath.Join(dir, "missing.sqlite")
			destination := filepath.Join(dir, "backup.sqlite")
			if err := os.WriteFile(source+suffix, []byte("orphan"), 0o600); err != nil {
				t.Fatal(err)
			}

			err := runRollbackSnapshot([]string{
				"--rollback-snapshot", "--db", source, "--out", destination,
			}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "SQLite sidecar "+suffix) {
				t.Fatalf("orphan %s snapshot err=%v", suffix, err)
			}
			if _, err := os.Stat(upgradeMaintenanceMarker(source)); err != nil {
				t.Fatalf("refusal removed fail-closed marker: %v", err)
			}
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refusal left partial snapshot: %v", err)
			}
		})
	}
}

func TestRunRollbackSnapshotRejectsUnexpectedArguments(t *testing.T) {
	err := runRollbackSnapshot([]string{
		"--rollback-snapshot", "extra",
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("unexpected argument err=%v", err)
	}
}

func TestCLIMaintenanceBarrierBlocksCommandsAndTracksDBFlag(t *testing.T) {
	dir := privateRollbackTempDir(t)
	production := filepath.Join(dir, "production.sqlite")
	t.Setenv("CLAWCTL_DB", filepath.Join(dir, "other.sqlite"))
	marker := upgradeMaintenanceMarker(production)
	if err := os.WriteFile(marker, []byte("upgrade\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, argv := range [][]string{
		{"deployment", "create", "--db", production},
		{"machine", "channel", "--db=" + production},
		{"machines", "--db", production},
		{"unknown-command", "--db", production},
	} {
		if err := rejectCLIWhileUpgradeMaintenance(argv); err == nil || !strings.Contains(err.Error(), "maintenance") {
			t.Fatalf("argv=%q bypassed maintenance: %v", argv, err)
		}
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := rejectCLIWhileUpgradeMaintenance([]string{"deployment", "create", "--db", production}); err != nil {
		t.Fatalf("command stayed blocked after marker removal: %v", err)
	}
}

func TestCLIMaintenanceBarrierBlocksAllNormalCLIEvenForAnotherDB(t *testing.T) {
	dir := privateRollbackTempDir(t)
	production := filepath.Join(dir, "production.sqlite")
	t.Setenv("CLAWCTL_DB", production)
	if err := os.WriteFile(upgradeMaintenanceMarker(production), []byte("upgrade\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(dir, "alternate.sqlite")
	if err := rejectCLIWhileUpgradeMaintenance([]string{"machines", "--db", alternate}); err == nil {
		t.Fatal("normal CLI targeting another DB bypassed global upgrade maintenance")
	}
}

func TestCLIMaintenanceBarrierAllowsOnlyServeAndUpgradeCapabilities(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.sqlite")
	t.Setenv("CLAWCTL_DB", dbPath)
	if err := os.WriteFile(upgradeMaintenanceMarker(dbPath), []byte("upgrade\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, argv := range [][]string{
		nil,
		{"--listen", "127.0.0.1:0"},
		{"--rollback-compatible", "--db", dbPath},
		{"--rollback-snapshot", "--db", dbPath, "--out", filepath.Join(t.TempDir(), "backup.sqlite")},
		{"--upgrade-maintenance-begin", "--db", dbPath},
		{"--upgrade-stopped-check", "--db", dbPath},
		{"version"},
	} {
		if err := rejectCLIWhileUpgradeMaintenance(argv); err != nil {
			t.Fatalf("safe argv=%q was blocked: %v", argv, err)
		}
	}
}

func TestCLIMaintenanceBarrierFailsClosedOnMarkerStatError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.sqlite")
	t.Setenv("CLAWCTL_DB", dbPath)
	marker := upgradeMaintenanceMarker(dbPath)
	if err := os.Symlink(filepath.Base(marker), marker); err != nil {
		t.Fatal(err)
	}
	if err := rejectCLIWhileUpgradeMaintenance([]string{"machines"}); err == nil {
		t.Fatal("marker stat error allowed CLI")
	}
}

func TestHTTPFirstCommandsIgnoreUnrelatedLocalMaintenanceMarker(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "local-default.sqlite")
	t.Setenv("CLAWCTL_DB", dbPath)
	if err := os.WriteFile(upgradeMaintenanceMarker(dbPath), []byte("upgrade\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := []string{"machine", "channel", "--hub-url", "http://100.64.0.9:8787", "--machine", "id"}
	if err := rejectTopLevelCLIWhileUpgradeMaintenance("machine", argv); err != nil {
		t.Fatalf("remote HTTP mode was coupled to local marker: %v", err)
	}
	machinesArgv := []string{"machines", "--hub-url", "http://100.64.0.9:8787"}
	if err := rejectTopLevelCLIWhileUpgradeMaintenance("machines", machinesArgv); err != nil {
		t.Fatalf("remote machine list was coupled to local marker: %v", err)
	}
	for _, jobArgv := range [][]string{
		{"job", "list", "--hub-url", "http://100.64.0.9:8787"},
		{"job", "show", "--hub-url", "http://100.64.0.9:8787", "job-id"},
	} {
		if err := rejectTopLevelCLIWhileUpgradeMaintenance("job", jobArgv); err != nil {
			t.Fatalf("remote job read was coupled to local marker: %v", err)
		}
	}
	for _, deploymentArgv := range [][]string{
		{"deployment", "list", "--hub-url", "http://100.64.0.9:8787"},
		{"deployment", "show", "--hub-url", "http://100.64.0.9:8787", "deployment-id"},
		{"deployment", "preview", "--hub-url", "http://100.64.0.9:8787", "--channel", "canary", "--version", "2026.9.8"},
		// Top-level dispatch must not substitute the unrelated default marker
		// for the direct path's exact-target check. The direct command reaches
		// rejectDBWhileUpgradeMaintenance only after taking both locks.
		{"deployment", "list", "--db", dbPath},
	} {
		if err := rejectTopLevelCLIWhileUpgradeMaintenance("deployment", deploymentArgv); err != nil {
			t.Fatalf("deployment read/preview transport was coupled to local marker argv=%q: %v", deploymentArgv, err)
		}
	}
	if err := rejectTopLevelCLIWhileUpgradeMaintenance("job", []string{"job", "create", "--kind", "noop"}); err == nil {
		t.Fatal("direct job create bypassed the maintenance marker")
	}
	for _, action := range []string{"create", "continue", "retry", "abandon"} {
		if err := rejectTopLevelCLIWhileUpgradeMaintenance("deployment", []string{"deployment", action}); err == nil {
			t.Fatalf("deployment %s bypassed the maintenance marker", action)
		}
	}
	if err := rejectDBWhileUpgradeMaintenance(dbPath); err == nil || !strings.Contains(err.Error(), "direct DB") {
		t.Fatalf("exact direct target bypassed marker: %v", err)
	}
}
