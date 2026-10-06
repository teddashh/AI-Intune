//go:build windows

package main

import (
	"fmt"
	"os"
)

func currentEUID() int { return -1 }

// checkPrivateDir on Windows only refuses symlinks and non-directories; the
// directory lives under the user's profile and inherits its ACL.
func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a plain directory", dir)
	}
	return nil
}
