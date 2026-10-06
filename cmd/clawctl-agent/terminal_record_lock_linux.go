//go:build linux

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockTerminalRecord takes an exclusive, non-blocking lock on path. The lock
// lasts while the returned file stays open.
func lockTerminalRecord(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errTerminalRecordHeld
		}
		return nil, err
	}
	return file, nil
}
