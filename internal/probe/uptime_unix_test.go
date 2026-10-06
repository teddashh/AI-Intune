//go:build unix

package probe

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestClockUptimeKeepsElapsedSecondsDistinctFromUnavailable(t *testing.T) {
	readError := errors.New("clock unavailable")
	for _, test := range []struct {
		name     string
		measured unix.Timespec
		err      error
		want     int64
		wantNil  bool
	}{
		{"elapsed", unix.NsecToTimespec(int64(123456*time.Second + 789*time.Millisecond)), nil, 123456, false},
		{"fresh boot", unix.NsecToTimespec(int64(420 * time.Millisecond)), nil, 0, false},
		{"unavailable", unix.Timespec{}, readError, 0, true},
		{"failed read with partial output", unix.NsecToTimespec(int64(10 * time.Second)), readError, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := readClockUptimeSeconds(func(out *unix.Timespec) error {
				*out = test.measured
				return test.err
			})
			if test.wantNil {
				if got != nil {
					t.Fatalf("failed clock read became measured uptime=%d", *got)
				}
			} else if got == nil || *got != test.want {
				t.Fatalf("uptime=%v, want measured %d seconds", got, test.want)
			}
		})
	}
}
