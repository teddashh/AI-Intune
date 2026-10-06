//go:build windows

package probe

import "os"

// Windows FileInfo.Sys does not carry a device and inode without an extra
// handle syscall. Callers fall back to resolved path, size, and mtime.
func deviceInode(os.FileInfo) (uint64, uint64, bool) {
	return 0, 0, false
}
