package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/teddashh/AI-Intune/internal/ledgerlock"
	"github.com/teddashh/AI-Intune/internal/store"
	"golang.org/x/sys/unix"
)

func runRollbackSnapshot(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("rollback-snapshot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	capability := fs.Bool("rollback-snapshot", false, "create a consistent, verified quiescent rollback snapshot")
	dbPath := fs.String("db", defaultDB(), "path to SQLite database")
	destination := fs.String("out", "", "snapshot destination file; must not exist")
	maintenanceHeld := fs.Bool("maintenance-already-held", false, "upgrade script has created and verified maintenance marker")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*capability {
		return errors.New("missing --rollback-snapshot")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*destination) == "" {
		return errors.New("rollback snapshot missing --out")
	}
	// Marker must precede every read of the live database. On any later error it
	// remains intentionally: only the controlling upgrade script, after putting
	// the old binary and untouched DB back into service, may remove it.
	if *maintenanceHeld {
		if err := validateUpgradeMaintenanceMarker(*dbPath); err != nil {
			return err
		}
	} else {
		if err := createUpgradeMaintenanceMarker(*dbPath); err != nil {
			return err
		}
	}
	if err := ledgerlock.ValidateUpgradeTarget(*dbPath); err != nil {
		return fmt.Errorf("rollback snapshot ledger path rejected: %w", err)
	}
	present, err := store.CreateRollbackSnapshot(*dbPath, *destination)
	if err != nil {
		return err
	}
	if !present {
		fmt.Fprintln(out, "rollback snapshot: no DB prior to upgrade; verified no orphan WAL/SHM/journal, ledger considered empty and quiescent")
		return nil
	}
	fmt.Fprintln(out, "rollback snapshot: consistent single-file snapshot quick_check complete, ledger is quiescent")
	return nil
}

func runUpgradeMaintenanceBegin(argv []string, out io.Writer, disableCurrentExecutable func() error) error {
	fs := flag.NewFlagSet("upgrade-maintenance-begin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	capability := fs.Bool("upgrade-maintenance-begin", false, "atomically create upgrade maintenance marker before any final process drain")
	dbPath := fs.String("db", defaultDB(), "path to SQLite database")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*capability {
		return errors.New("missing --upgrade-maintenance-begin")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if disableCurrentExecutable == nil {
		return errors.New("upgrade maintenance executable disabler not initialized")
	}
	// Make the invoking inode non-executable before publishing the marker. If
	// power is lost at either boundary, no .new/installed path can later be used
	// as an unfenced serve process against an uncertain rollback coordinate.
	if err := disableCurrentExecutable(); err != nil {
		return fmt.Errorf("disable maintenance helper executable: %w", err)
	}
	if err := createUpgradeMaintenanceMarker(*dbPath); err != nil {
		return fmt.Errorf("executable disabled; create marker: %w", err)
	}
	fmt.Fprintln(out, "upgrade maintenance: marker created; old binary must be disabled and process=0 re-verified before creating snapshot")
	return nil
}

func disableCurrentExecutableForMaintenance() error {
	// /proc/self/exe is a kernel-owned reference to the inode that is actually
	// running.  Opening that reference works both for the installed/.new path
	// (link count 1) and for the upgrade script's already-unlinked private probe
	// (link count 0), without trusting a pathname returned by os.Executable.
	fd, err := unix.Open("/proc/self/exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open running executable: %w", err)
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return fmt.Errorf("fstat running executable: %w", err)
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Uid != uint32(os.Geteuid()) || opened.Nlink > 1 {
		return fmt.Errorf("running executable must be a regular file owned by current user with link count<=1")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return fmt.Errorf("chmod running executable 0600: %w", err)
	}
	return nil
}
