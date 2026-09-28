package probe

import "golang.org/x/sys/unix"

// Darwin's CLOCK_MONOTONIC_RAW uses mach_continuous_time, including sleep.
// System uptime therefore does not reset with this agent or use wall-clock time.
func uptimeSeconds() *int64 {
	return readClockUptimeSeconds(func(ts *unix.Timespec) error {
		return unix.ClockGettime(unix.CLOCK_MONOTONIC_RAW, ts)
	})
}
