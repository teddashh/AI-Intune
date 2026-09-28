package ledgerlock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func privateLedgerPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "clawctl.sqlite")
}

func TestWriterLockIsNonblockingAndHeldUntilClose(t *testing.T) {
	dbPath := privateLedgerPath(t)
	first, err := AcquireWriter(dbPath)
	if err != nil {
		t.Fatalf("first AcquireWriter: %v", err)
	}
	if first.Path() != WriterPath(dbPath) {
		t.Fatalf("locked path = %q, want %q", first.Path(), WriterPath(dbPath))
	}
	if _, err := AcquireWriter(dbPath); !errors.Is(err, ErrContended) {
		t.Fatalf("second AcquireWriter error = %v, want ErrContended", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close must be idempotent: %v", err)
	}
	after, err := AcquireWriter(dbPath)
	if err != nil {
		t.Fatalf("AcquireWriter after Close: %v", err)
	}
	defer after.Close()
}

func TestExistingLockIsNeverTruncatedAndConvergesTo0600(t *testing.T) {
	dbPath := privateLedgerPath(t)
	lockPath := WriterPath(dbPath)
	want := []byte("do-not-truncate\n")
	if err := os.WriteFile(lockPath, want, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := AcquireWriter(dbPath)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	defer h.Close()
	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("lock content changed: got %q want %q", got, want)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock mode = %o, want 600", got)
	}
}

func TestUnsafeExistingLockIsRefusedWithoutTouchingVictim(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dbPath := privateLedgerPath(t)
			victim := filepath.Join(filepath.Dir(dbPath), "victim")
			want := []byte("do-not-truncate-or-chmod\n")
			if err := os.WriteFile(victim, want, 0o640); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "symlink" {
				err = os.Symlink(victim, WriterPath(dbPath))
			} else {
				err = os.Link(victim, WriterPath(dbPath))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := AcquireWriter(dbPath); err == nil {
				t.Fatal("AcquireWriter accepted unsafe lock path")
			}
			got, err := os.ReadFile(victim)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("victim content changed: %q", got)
			}
			info, err := os.Stat(victim)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o640 {
				t.Fatalf("victim mode changed to %o", got)
			}
		})
	}
}

func TestLedgerParentMustAlreadyBeCanonicalOwnedAndPrivate(t *testing.T) {
	t.Run("relative", func(t *testing.T) {
		if _, err := AcquireWriter("relative/clawctl.sqlite"); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("relative path error = %v", err)
		}
	})

	t.Run("unclean", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		dbPath := dir + "/child/../clawctl.sqlite"
		if _, err := AcquireWriter(dbPath); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("unclean path error = %v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "missing", "clawctl.sqlite")
		if _, err := AcquireWriter(dbPath); err == nil {
			t.Fatal("missing parent accepted")
		}
		if _, err := os.Lstat(filepath.Dir(dbPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lock acquisition created parent: %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		realParent := filepath.Join(root, "real")
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "alias")
		if err := os.Symlink(realParent, alias); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(alias, "clawctl.sqlite")
		if _, err := AcquireWriter(dbPath); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink parent error = %v", err)
		}
		if _, err := os.Lstat(filepath.Join(realParent, "clawctl.sqlite.writer.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lock created through parent symlink: %v", err)
		}
	})

	t.Run("not private", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		dbPath := filepath.Join(dir, "clawctl.sqlite")
		if _, err := AcquireWriter(dbPath); err == nil || !strings.Contains(err.Error(), "private") {
			t.Fatalf("shared parent error = %v", err)
		}
		if _, err := os.Lstat(WriterPath(dbPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lock created in shared parent: %v", err)
		}
	})
}

func TestPathFDIdentityIsCheckedBeforeAndAfterFlock(t *testing.T) {
	for _, stage := range []string{"after-open", "after-flock"} {
		t.Run(stage, func(t *testing.T) {
			dbPath := privateLedgerPath(t)
			replacement := []byte("replacement-must-not-be-locked-or-chmodded\n")
			swap := func(path string) {
				if err := os.Rename(path, path+".opened"); err != nil {
					t.Fatalf("rename lock in hook: %v", err)
				}
				if err := os.WriteFile(path, replacement, 0o644); err != nil {
					t.Fatalf("create replacement in hook: %v", err)
				}
			}
			hooks := &acquireHooks{}
			if stage == "after-open" {
				hooks.afterOpen = swap
			} else {
				hooks.afterFlock = swap
			}
			if _, err := acquire(dbPath, writerLock, hooks); err == nil || !strings.Contains(err.Error(), "identity") {
				t.Fatalf("path swap error = %v, want identity failure", err)
			}
			got, err := os.ReadFile(WriterPath(dbPath))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(replacement) {
				t.Fatalf("replacement content changed: %q", got)
			}
			info, err := os.Stat(WriterPath(dbPath))
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o644 {
				t.Fatalf("replacement mode changed to %o", got)
			}
		})
	}
}

func TestDirectLockOrderAndPartialFailureCleanup(t *testing.T) {
	t.Run("lifecycle contention creates no writer sidecar", func(t *testing.T) {
		dbPath := privateLedgerPath(t)
		lifecycle, err := AcquireUpgrade(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer lifecycle.Close()
		if _, err := AcquireDirect(dbPath); !errors.Is(err, ErrContended) {
			t.Fatalf("AcquireDirect error = %v, want lifecycle contention", err)
		}
		if _, err := os.Lstat(WriterPath(dbPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("writer path was touched before lifecycle acquisition: %v", err)
		}
	})

	t.Run("writer contention releases lifecycle", func(t *testing.T) {
		dbPath := privateLedgerPath(t)
		writer, err := AcquireWriter(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		if _, err := AcquireDirect(dbPath); !errors.Is(err, ErrContended) {
			t.Fatalf("AcquireDirect error = %v, want writer contention", err)
		}
		lifecycle, err := AcquireUpgrade(dbPath)
		if err != nil {
			t.Fatalf("partial failure leaked lifecycle lock: %v", err)
		}
		defer lifecycle.Close()
	})
}

func TestUpgradeAndWriterAreIndependent(t *testing.T) {
	dbPath := privateLedgerPath(t)
	upgrade, err := AcquireUpgrade(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer upgrade.Close()
	writer, err := AcquireWriter(dbPath)
	if err != nil {
		t.Fatalf("writer must not contend with lifecycle lock: %v", err)
	}
	defer writer.Close()
}

func TestGoLifecycleLockInteroperatesWithUpgradeScriptFlock(t *testing.T) {
	if _, err := os.Stat("/usr/bin/flock"); err != nil {
		t.Skip("/usr/bin/flock unavailable")
	}
	dbPath := privateLedgerPath(t)
	guard, err := AcquireUpgrade(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/flock", "-n", UpgradePath(dbPath), "-c", "true")
	if err := cmd.Run(); err == nil {
		_ = guard.Close()
		t.Fatal("shell flock bypassed Go lifecycle owner")
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/usr/bin/flock", "-n", UpgradePath(dbPath), "-c", "true").CombinedOutput(); err != nil {
		t.Fatalf("shell flock stayed blocked after Close: %v (%s)", err, output)
	}
}

func TestGoWriterLockInteroperatesWithUpgradeScriptFlock(t *testing.T) {
	if _, err := os.Stat("/usr/bin/flock"); err != nil {
		t.Skip("/usr/bin/flock unavailable")
	}
	dbPath := privateLedgerPath(t)
	guard, err := AcquireWriter(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/usr/bin/flock", "-n", WriterPath(dbPath), "-c", "true").Run(); err == nil {
		_ = guard.Close()
		t.Fatal("shell flock bypassed Go writer owner")
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/usr/bin/flock", "-n", WriterPath(dbPath), "-c", "true").CombinedOutput(); err != nil {
		t.Fatalf("shell writer flock stayed blocked after Close: %v (%s)", err, output)
	}
}

func TestValidateExistingDBIsSideEffectFreeAndRejectsAliases(t *testing.T) {
	t.Run("regular", func(t *testing.T) {
		dbPath := privateLedgerPath(t)
		if err := os.WriteFile(dbPath, []byte("ledger"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ValidateExistingDB(dbPath); err != nil {
			t.Fatalf("ValidateExistingDB: %v", err)
		}
	})

	for _, kind := range []string{"missing", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dbPath := privateLedgerPath(t)
			victim := filepath.Join(filepath.Dir(dbPath), "victim.sqlite")
			if kind != "missing" {
				if err := os.WriteFile(victim, []byte("ledger"), 0o644); err != nil {
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
			if err := ValidateExistingDB(dbPath); err == nil {
				t.Fatalf("ValidateExistingDB accepted %s target", kind)
			}
			for _, sidecar := range []string{UpgradePath(dbPath), WriterPath(dbPath), dbPath + "-wal", dbPath + "-shm"} {
				if _, err := os.Lstat(sidecar); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("validation created sidecar %s: %v", sidecar, err)
				}
			}
		})
	}
}

func TestWriterRefusesExistingLedgerAliasesBeforeCreatingLock(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dbPath := privateLedgerPath(t)
			victim := filepath.Join(filepath.Dir(dbPath), "victim.sqlite")
			if err := os.WriteFile(victim, []byte("ledger"), 0o600); err != nil {
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
			if guard, err := AcquireWriter(dbPath); err == nil {
				_ = guard.Close()
				t.Fatal("writer accepted aliased ledger")
			}
			if _, err := os.Lstat(WriterPath(dbPath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("writer created lock beside rejected alias: %v", err)
			}
		})
	}
}

func TestWriterRefusesOrphanedSQLiteSidecarsBeforeCreatingLockOrLedger(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			dbPath := privateLedgerPath(t)
			if err := os.WriteFile(dbPath+suffix, []byte("orphan evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			if guard, err := AcquireWriter(dbPath); err == nil {
				_ = guard.Close()
				t.Fatal("writer accepted a missing ledger with an orphaned SQLite sidecar")
			}
			for _, unexpected := range []string{dbPath, WriterPath(dbPath)} {
				if _, err := os.Lstat(unexpected); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("writer refusal created %s: %v", unexpected, err)
				}
			}
		})
	}
}

func TestExistingSQLiteSidecarsMustBeOwnedUnaliasedRegularFiles(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		for _, kind := range []string{"symlink", "hardlink", "fifo"} {
			t.Run(suffix+"/"+kind, func(t *testing.T) {
				dbPath := privateLedgerPath(t)
				if err := os.WriteFile(dbPath, []byte("ledger"), 0o600); err != nil {
					t.Fatal(err)
				}
				sidecarPath := dbPath + suffix
				victimPath := filepath.Join(filepath.Dir(dbPath), "victim")
				wantVictim := []byte("must-not-be-touched\n")
				if kind != "fifo" {
					if err := os.WriteFile(victimPath, wantVictim, 0o640); err != nil {
						t.Fatal(err)
					}
				}
				switch kind {
				case "symlink":
					if err := os.Symlink(victimPath, sidecarPath); err != nil {
						t.Fatal(err)
					}
				case "hardlink":
					if err := os.Link(victimPath, sidecarPath); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := unix.Mkfifo(sidecarPath, 0o600); err != nil {
						t.Fatal(err)
					}
				}

				if err := ValidateExistingDB(dbPath); err == nil {
					t.Fatalf("ValidateExistingDB accepted %s SQLite sidecar", kind)
				}
				if guard, err := AcquireWriter(dbPath); err == nil {
					_ = guard.Close()
					t.Fatalf("AcquireWriter accepted %s SQLite sidecar", kind)
				}
				if _, err := os.Lstat(WriterPath(dbPath)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("writer created a lock beside rejected sidecar: %v", err)
				}

				if kind == "fifo" {
					info, err := os.Lstat(sidecarPath)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode()&os.ModeNamedPipe == 0 {
						t.Fatalf("rejected FIFO changed type: %v", info.Mode())
					}
					return
				}
				gotVictim, err := os.ReadFile(victimPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(gotVictim) != string(wantVictim) {
					t.Fatalf("victim content changed: got %q want %q", gotVictim, wantVictim)
				}
				victimInfo, err := os.Stat(victimPath)
				if err != nil {
					t.Fatal(err)
				}
				if got := victimInfo.Mode().Perm(); got != 0o640 {
					t.Fatalf("victim mode changed to %o", got)
				}
			})
		}
	}
}

func TestOwnedUnaliasedRegularSQLiteSidecarsAreAccepted(t *testing.T) {
	dbPath := privateLedgerPath(t)
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal"} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateExistingDB(dbPath); err != nil {
		t.Fatalf("ValidateExistingDB rejected safe sidecars: %v", err)
	}
	guard, err := AcquireWriter(dbPath)
	if err != nil {
		t.Fatalf("AcquireWriter rejected safe sidecars: %v", err)
	}
	defer guard.Close()
}

func TestExistingSQLiteSidecarRelativeAndAbsoluteIdentityMustMatch(t *testing.T) {
	relativeParent := t.TempDir()
	absoluteParent := t.TempDir()
	for _, dir := range []string{relativeParent, absoluteParent} {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "clawctl.sqlite-wal"), []byte(dir), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	parentFD, err := unix.Open(relativeParent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(parentFD)

	err = validateExistingSQLiteSidecars(
		parentFD,
		"clawctl.sqlite",
		filepath.Join(absoluteParent, "clawctl.sqlite"),
	)
	if err == nil || !strings.Contains(err.Error(), "changed while it was being verified") {
		t.Fatalf("sidecar identity mismatch error = %v", err)
	}
}

func TestSQLiteSidecarOwnerMismatchIsRejected(t *testing.T) {
	stat := unix.Stat_t{
		Mode:  unix.S_IFREG | 0o600,
		Uid:   uint32(os.Geteuid()) + 1,
		Nlink: 1,
	}
	if err := validateOwnedRegular("SQLite sidecar", "/ledger-wal", &stat); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("owner mismatch error = %v", err)
	}
}
