package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/teddashh/AI-Intune/internal/ledgerlock"
)

const upgradeProtocolReady = "upgrade-protocol-ready:v1 stopped-check=true ledger-path=true maintenance-self-disable=true held-snapshot=true dormant-artifacts=true writer-lock-gate=true"

func runUpgradeProtocolCheck(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("upgrade-protocol-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	capability := fs.Bool("upgrade-protocol-check", false, "verify binary supports full upgrade handoff protocol")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*capability {
		return errors.New("missing --upgrade-protocol-check")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	fmt.Fprintln(out, upgradeProtocolReady)
	return nil
}

func runUpgradeLedgerPathCheck(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("upgrade-ledger-path-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	capability := fs.Bool("upgrade-ledger-path-check", false, "verify upgrade ledger and SQLite sidecars are canonical owned unaliased files")
	dbPath := fs.String("db", defaultDB(), "path to SQLite database")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*capability {
		return errors.New("missing --upgrade-ledger-path-check")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := ledgerlock.ValidateUpgradeTarget(*dbPath); err != nil {
		return err
	}
	fmt.Fprintln(out, "upgrade ledger path: canonical parent, main DB, and existing SQLite sidecars identity verified")
	return nil
}

// runUpgradeStoppedCheck lets a staged candidate prove the exact loaded unit
// and recursive cgroup are stopped without requiring the candidate inode to be
// the currently installed binary. Direct DB mode adds that same-inode check
// separately; an upgrade candidate necessarily lives at .new until handoff.
func runUpgradeStoppedCheck(ctx context.Context, argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("upgrade-stopped-check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	capability := fs.Bool("upgrade-stopped-check", false, "verify managed Hub unit contract and recursive cgroup are fully stopped")
	dbPath := fs.String("db", defaultDB(), "path to SQLite database")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*capability {
		return errors.New("missing --upgrade-stopped-check")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := verifyManagedHubStoppedWithChecks(ctx, *dbPath, queryManagedHubStatus, verifyManagedHubCgroupEmpty); err != nil {
		return err
	}
	fmt.Fprintln(out, "upgrade stopped proof: exact loaded unit, MainPID=0, inactive/dead, recursive cgroup empty")
	return nil
}
