//go:build windows

package probe

import "golang.org/x/sys/windows"

// diskUsage reports caller-available and total bytes from GetDiskFreeSpaceExW.
// "/" is the OS volume (GetWindowsDirectory), matching Linux root-fs check-in.
func diskUsage(path string) (free, total int64) {
	target := path
	if path == "/" || path == "" {
		dir, err := windows.GetWindowsDirectory()
		if err != nil {
			return 0, 0
		}
		target = dir
	}
	freeBytes, totalBytes, err := diskSpace(target)
	if err != nil {
		return 0, 0
	}
	return windowsDiskBytes(freeBytes, totalBytes)
}

func diskSpace(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeAvail, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeAvail, &totalBytes, &totalFree); err != nil {
		return 0, 0, err
	}
	return freeAvail, totalBytes, nil
}
