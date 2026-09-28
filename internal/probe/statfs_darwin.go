package probe

import "golang.org/x/sys/unix"

// Darwin 的 struct statfs 沒有 f_frsize，區塊計數單位就是 f_bsize，
// 所以這裡沒有可退回的第二個欄位。
func statfsBlockUnit(st *unix.Statfs_t) int64 {
	return int64(st.Bsize)
}
