//go:build unix

package probe

import "golang.org/x/sys/unix"

func readClockUptimeSeconds(read func(*unix.Timespec) error) *int64 {
	var ts unix.Timespec
	if err := read(&ts); err != nil {
		return nil
	}
	if ts.Sec < 0 {
		return nil
	}
	sec := int64(ts.Sec)
	return &sec
}
