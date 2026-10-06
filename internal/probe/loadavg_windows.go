//go:build windows

package probe

// Windows has no /proc/loadavg and no equivalent 1-minute load average.
func load1m() *float64 { return nil }
