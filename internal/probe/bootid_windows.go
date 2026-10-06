//go:build windows

package probe

// Windows has no kernel boot UUID analogous to /proc/sys/kernel/random/boot_id.
func bootID() string { return "" }
