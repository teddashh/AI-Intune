//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

func currentEUID() int { return os.Geteuid() }

// checkPrivateDir refuses a maintenance directory that another account could
// tamper with between the write and the exec: it must be a real directory
// (not a symlink), owned by this process's euid, and not group/other-writable.
func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a plain directory", dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group- or other-writable (%#o)", dir, info.Mode().Perm())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s ownership cannot be checked", dir)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not the agent euid %d", dir, st.Uid, os.Geteuid())
	}
	return nil
}
