//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTerminalRecordLockRefusesSecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "terminal-ptys.lock")
	first, err := lockTerminalRecord(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer first.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file = %v, %v; want mode 0600", info, err)
	}
	// flock(2) treats separate open(2) calls as separate holders, even in one
	// process, which is how a second agent process would see it.
	if second, err := lockTerminalRecord(path); !errors.Is(err, errTerminalRecordHeld) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second lock = %v; want errTerminalRecordHeld", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := lockTerminalRecord(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = third.Close()
}

func TestTerminalRecordStoreHoldsItsLockAcrossAttempts(t *testing.T) {
	dir := t.TempDir()
	var first terminalRecordStore
	record, err := first.open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if again, err := first.open(dir); err != nil || again != record {
		t.Fatalf("second open of the same store = %p, %v; want the same record %p", again, err, record)
	}
	var second terminalRecordStore
	if other, err := second.open(dir); !errors.Is(err, errTerminalRecordHeld) || other != nil {
		t.Fatalf("another store's open = %p, %v; want errTerminalRecordHeld", other, err)
	}
	if _, err := second.open(filepath.Join(dir, "missing")); err == nil || errors.Is(err, errTerminalRecordHeld) {
		t.Fatalf("open in a missing directory = %v; want an error that is not errTerminalRecordHeld", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("open created the missing directory: %v", err)
	}
}
