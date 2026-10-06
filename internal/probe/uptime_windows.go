//go:build windows

package probe

import (
	"time"

	"golang.org/x/sys/windows"
)

// uptimeSeconds uses GetTickCount64: milliseconds since the system was started,
// including sleep. Resolution is the system timer, typically 10–16 ms.
// Whole seconds only; a successful read of less than one second is 0, not nil.
func uptimeSeconds() *int64 {
	return tickCountUptimeSeconds(uint64(windows.DurationSinceBoot() / time.Millisecond))
}
