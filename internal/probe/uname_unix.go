//go:build unix

package probe

import "golang.org/x/sys/unix"

func kernelAndArch() (kernel, arch string) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		return utsString(uts.Release[:]), utsString(uts.Machine[:]) // x86_64 / aarch64，不是 GOARCH 的 amd64
	}
	return "", ""
}
