package probe

import "golang.org/x/sys/windows"

var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
)

func kernelAndArch() (kernel, arch string) {
	major, minor, build := windows.RtlGetNtVersionNumbers()
	kernel = windowsKernelRelease(major, minor, build)
	arch = windowsNativeArch(func() (uint16, error) {
		var processMachine, nativeMachine uint16
		err := windows.IsWow64Process2(windows.CurrentProcess(), &processMachine, &nativeMachine)
		return nativeMachine, err
	})
	// If IsWow64Process2 is absent or fails, architecture remains unknown.
	// GetNativeSystemInfo and GOARCH can report the emulated process instead.
	return kernel, arch
}
