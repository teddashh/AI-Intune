//go:build unix

package probe

import (
	"os"
	"syscall"
)

func deviceInode(fi os.FileInfo) (uint64, uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}
