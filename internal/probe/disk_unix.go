//go:build unix

package probe

import "golang.org/x/sys/unix"

// diskUsage 回 (可用, 總量)。用 Bavail（非 root 可用）而不是 Bfree，
// 因為 agent 不是 root，保留區塊對它來說就是不存在。
func diskUsage(path string) (free, total int64) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0
	}
	unit := statfsBlockUnit(&st)
	return int64(st.Bavail) * unit, int64(st.Blocks) * unit
}
