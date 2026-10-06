//go:build windows

package probe

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func unixFileOwner(os.FileInfo) (string, bool) { return "", false }

func dirWritable(path string) (*bool, string) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, "量不到目錄可寫性：" + err.Error()
	}
	handle, err := windows.CreateFile(p, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err == nil {
		_ = windows.CloseHandle(handle)
		v := true
		return &v, ""
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		v := false
		return &v, ""
	}
	return nil, "量不到目錄可寫性：" + err.Error()
}

func diskFreeAvailable(path string) (int64, error) {
	free, total, err := diskSpace(path)
	if err != nil {
		return 0, err
	}
	measuredFree, measuredTotal := windowsDiskBytes(free, total)
	if measuredTotal == 0 {
		return 0, errors.New("invalid disk measurement")
	}
	return measuredFree, nil
}

func diskFreeError(target string, err error) string {
	return "GetDiskFreeSpaceEx " + target + "：" + err.Error()
}
