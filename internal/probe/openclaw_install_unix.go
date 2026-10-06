//go:build unix

package probe

import (
	"errors"
	"os"
	"os/user"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func unixFileOwner(fi os.FileInfo) (string, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	// ⚠ LookupId 失敗仍保留 uid 數字；把 owner 留白會讓 root 擁有的
	// sampleagent2 目錄看起來只是「不知道」，漏掉升級需要不同權限這個事實。
	uid := strconv.FormatUint(uint64(st.Uid), 10)
	if u, err := user.LookupId(uid); err == nil {
		return u.Username, true
	}
	return uid, true
}

func dirWritable(path string) (*bool, string) {
	// ⚠ 用 Access(W_OK) 而不是建立測試檔；建立檔案會讓這一刀從探測變成
	// 寫入，還可能在 root 擁有的 sampleagent2 套件目錄留下垃圾。
	if err := unix.Access(path, unix.W_OK); err == nil {
		v := true
		return &v, ""
	} else if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EROFS) {
		v := false
		return &v, ""
	} else {
		return nil, "量不到目錄可寫性：" + err.Error()
	}
}

func diskFreeAvailable(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func diskFreeError(target string, err error) string {
	return "statfs " + target + "：" + err.Error()
}
