package main

import (
	"errors"
	"fmt"
	"path/filepath"
)

// canonicalServeDBPath turns a relative flag or CLAWCTL_DB value into the one
// absolute lexical identity used by the writer lock, SQLite, and artifact
// directory. Filesystem identity and symlink checks remain the responsibility
// of ledgerlock at the mutation boundary.
func canonicalServeDBPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("database path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	if !filepath.IsAbs(absolute) || absolute != filepath.Clean(absolute) {
		return "", errors.New("database path did not resolve to a canonical absolute path")
	}
	return absolute, nil
}
