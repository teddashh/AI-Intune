package probe

import "golang.org/x/sys/unix"

// Linux 的 statfs(2) 同時提供 f_frsize 與 f_bsize；區塊計數以 f_frsize 為單位，
// 因此保留 f_frsize 優先，只有它不合法時才退回 f_bsize。
func statfsBlockUnit(st *unix.Statfs_t) int64 {
	unit := int64(st.Frsize)
	if unit <= 0 {
		unit = int64(st.Bsize)
	}
	return unit
}
