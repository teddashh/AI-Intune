package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/ledgerlock"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func directDBFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "clawctl.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	machineID, _, err := st.CreateEnrollTokenFor("direct-machine", time.Hour)
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    now,
		OpenClaw:      model.OpenClaw{Present: true},
	}, now); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath, machineID
}

func directChannelArgs(dbPath, machineID string) []string {
	return []string{
		"channel", "--db", dbPath, "--machine", machineID, "--set", "canary",
		"--confirm-name", "direct-machine", "--idempotency-key", "direct-contract-key",
		"--expected-revision", "0",
	}
}

func TestMachineChannelDirectRequiresAndHoldsBothLocksUntilStoreClose(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --db invoked discovery")
		return "", nil
	}
	verified := false
	deps.verifyHubStopped = func(_ context.Context, gotPath string) error {
		if gotPath != dbPath {
			t.Fatalf("stopped proof DB=%q want=%q", gotPath, dbPath)
		}
		for name, acquire := range map[string]func(string) (*ledgerlock.Handle, error){
			"upgrade": ledgerlock.AcquireUpgrade,
			"writer":  ledgerlock.AcquireWriter,
		} {
			if guard, err := acquire(dbPath); !errors.Is(err, ledgerlock.ErrContended) {
				if guard != nil {
					_ = guard.Close()
				}
				t.Fatalf("%s lock was not held during stopped proof: %v", name, err)
			}
		}
		verified = true
		return nil
	}
	deps.openDirectDB = func(path string) (*store.Store, error) {
		if !verified {
			t.Fatal("Store.Open ran before stopped-service proof")
		}
		if guard, err := ledgerlock.AcquireWriter(path); !errors.Is(err, ledgerlock.ErrContended) {
			if guard != nil {
				_ = guard.Close()
			}
			t.Fatalf("writer lock was not held at Store.Open: %v", err)
		}
		return openExisting(path)
	}

	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), directChannelArgs(dbPath, machineID), &out, &errOut, deps); err != nil {
		t.Fatalf("direct channel: %v; stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "direct DB operator service") || !strings.Contains(out.String(), "revision=1") {
		t.Fatalf("direct output=%q", out.String())
	}
	for name, acquire := range map[string]func(string) (*ledgerlock.Handle, error){
		"upgrade": ledgerlock.AcquireUpgrade,
		"writer":  ledgerlock.AcquireWriter,
	} {
		guard, err := acquire(dbPath)
		if err != nil {
			t.Fatalf("%s lock leaked after command: %v", name, err)
		}
		_ = guard.Close()
	}
	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.Channel != "canary" || machine.ChannelRevision != 1 {
		t.Fatalf("direct mutation machine=%+v err=%v", machine, err)
	}
}

func TestMachineChannelDirectInvalidTargetCreatesNoSidecarsOrSafetyCalls(t *testing.T) {
	for _, kind := range []string{"missing", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(dir, "clawctl.sqlite")
			if kind != "missing" {
				victim := filepath.Join(dir, "victim.sqlite")
				if err := os.WriteFile(victim, []byte("not a ledger"), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink(victim, dbPath)
				} else {
					err = os.Link(victim, dbPath)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			deps := productionMachineCommandDeps()
			deps.acquireDirect = func(string) (io.Closer, error) {
				t.Fatal("invalid target reached lock acquisition")
				return nil, nil
			}
			deps.verifyHubStopped = func(context.Context, string) error {
				t.Fatal("invalid target reached systemd")
				return nil
			}
			deps.openDirectDB = func(string) (*store.Store, error) {
				t.Fatal("invalid target reached Store.Open")
				return nil, nil
			}
			var out, errOut bytes.Buffer
			err := runMachineCommandWithDeps(t.Context(), directChannelArgs(dbPath, "machine-id"), &out, &errOut, deps)
			if err == nil || !strings.Contains(err.Error(), "pre-open rejected") {
				t.Fatalf("invalid target error=%v", err)
			}
			for _, sidecar := range []string{
				ledgerlock.UpgradePath(dbPath), ledgerlock.WriterPath(dbPath), dbPath + "-wal", dbPath + "-shm",
			} {
				if _, err := os.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid target created %s: %v", sidecar, err)
				}
			}
		})
	}
}

func TestMachineChannelDirectRejectsNonLedgerAfterLockingAndBeforeSystemdOrWritableOpen(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{name: "zero byte"},
		{name: "random bytes", raw: []byte("not a clawctl ledger\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(dir, "candidate.sqlite")
			if err := os.WriteFile(dbPath, test.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			deps := productionMachineCommandDeps()
			deps.verifyHubStopped = func(context.Context, string) error {
				t.Fatal("non-ledger reached systemd")
				return nil
			}
			deps.openDirectDB = func(string) (*store.Store, error) {
				t.Fatal("non-ledger reached Store.Open")
				return nil, nil
			}
			var out, errOut bytes.Buffer
			err := runMachineCommandWithDeps(t.Context(), directChannelArgs(dbPath, "machine-id"), &out, &errOut, deps)
			if err == nil || !strings.Contains(err.Error(), "is not a migratable clawctl ledger") {
				t.Fatalf("non-ledger error=%v", err)
			}
			for _, lockPath := range []string{ledgerlock.UpgradePath(dbPath), ledgerlock.WriterPath(dbPath)} {
				info, statErr := os.Lstat(lockPath)
				if statErr != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("non-ledger safety lock %s info=%v err=%v", lockPath, info, statErr)
				}
			}
			for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"} {
				if _, err := os.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("non-ledger validation left SQLite sidecar %s: %v", sidecar, err)
				}
			}
		})
	}
}

func TestMachineChannelDirectInspectsLedgerOnlyWhileBothLocksAreHeld(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	deps := productionMachineCommandDeps()
	checks := 0
	deps.validateDirectLedger = func(path string) error {
		checks++
		for name, acquire := range map[string]func(string) (*ledgerlock.Handle, error){
			"upgrade": ledgerlock.AcquireUpgrade,
			"writer":  ledgerlock.AcquireWriter,
		} {
			guard, err := acquire(path)
			if guard != nil {
				_ = guard.Close()
			}
			if !errors.Is(err, ledgerlock.ErrContended) {
				t.Fatalf("ledger validation %d did not hold %s lock: %v", checks, name, err)
			}
		}
		return store.ValidateExistingLedger(path)
	}
	deps.verifyHubStopped = func(context.Context, string) error { return nil }

	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), directChannelArgs(dbPath, machineID), &out, &errOut, deps); err != nil {
		t.Fatalf("direct channel: %v; stderr=%s", err, errOut.String())
	}
	if checks != 3 {
		t.Fatalf("ledger validation calls=%d, want pre-systemd, pre-open and post-open checks", checks)
	}
}

func TestMachineChannelDirectContentionAndMaintenanceNeverOpenStore(t *testing.T) {
	for _, mode := range []string{"upgrade contention", "writer contention", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			dbPath, machineID := directDBFixture(t)
			var held io.Closer
			var err error
			switch mode {
			case "upgrade contention":
				held, err = ledgerlock.AcquireUpgrade(dbPath)
			case "writer contention":
				held, err = ledgerlock.AcquireWriter(dbPath)
			case "maintenance":
				err = os.WriteFile(upgradeMaintenanceMarker(dbPath), []byte("upgrade\n"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if held != nil {
				defer held.Close()
			}
			deps := productionMachineCommandDeps()
			deps.verifyHubStopped = func(context.Context, string) error {
				t.Fatal("refused direct mode reached systemd")
				return nil
			}
			deps.openDirectDB = func(string) (*store.Store, error) {
				t.Fatal("refused direct mode reached Store.Open")
				return nil, nil
			}
			var out, errOut bytes.Buffer
			err = runMachineCommandWithDeps(t.Context(), directChannelArgs(dbPath, machineID), &out, &errOut, deps)
			if err == nil {
				t.Fatal("unsafe direct mode succeeded")
			}
			if mode == "maintenance" && !strings.Contains(err.Error(), "maintenance") {
				t.Fatalf("maintenance error=%v", err)
			}
		})
	}
}

func TestMachineChannelDirectWriterContentionDoesNotRecreateMissingSHM(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "live.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.CreateEnrollTokenFor("live-wal", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Fatalf("fixture did not enter WAL mode: %v", err)
	}
	if err := os.Remove(dbPath + "-shm"); err != nil {
		t.Fatalf("remove live shm fixture: %v", err)
	}
	writer, err := ledgerlock.AcquireWriter(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	deps := productionMachineCommandDeps()
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("writer contention reached systemd")
		return nil
	}
	deps.openDirectDB = func(string) (*store.Store, error) {
		t.Fatal("writer contention reached Store.Open")
		return nil, nil
	}
	var out, errOut bytes.Buffer
	err = runMachineCommandWithDeps(t.Context(), directChannelArgs(dbPath, "machine-id"), &out, &errOut, deps)
	if !errors.Is(err, ledgerlock.ErrContended) {
		t.Fatalf("writer contention error=%v", err)
	}
	if _, statErr := os.Lstat(dbPath + "-shm"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("pre-lock validation recreated live SQLite shm: %v", statErr)
	}
}
