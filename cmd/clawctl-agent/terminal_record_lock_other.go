//go:build !linux

package main

import (
	"errors"
	"os"
)

// The terminal link runs only on Linux; other builds never reach the record.
func lockTerminalRecord(string) (*os.File, error) {
	return nil, errors.New("terminal record lock requires Linux")
}
