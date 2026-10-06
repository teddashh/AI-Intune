//go:build unix

package probe

func bootID() string { return readTrimmed("/proc/sys/kernel/random/boot_id") }
