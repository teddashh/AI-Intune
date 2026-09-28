package probe

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUptimeThatCannotBeReadIsNotAFreshBoot(t *testing.T) {
	tests := []struct {
		name string
		text string
		want *int64
	}{
		{name: "measured", text: "123456.78 98765.43", want: func() *int64 { v := int64(123456); return &v }()},
		{name: "fresh boot", text: "0.42 0.10", want: func() *int64 { v := int64(0); return &v }()},
		{name: "empty", text: "", want: nil},
		{name: "invalid", text: "abc 1", want: nil},
		{name: "negative", text: "-1 2", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseUptimeSeconds(tc.text)
			if tc.want == nil && got != nil {
				t.Fatalf("parseUptimeSeconds(%q) = %d，預期 nil；否則會把沒量到印成 0s，operator 會以為那台 Mac 每顆心跳都剛開機／一直在重開", tc.text, *got)
			}
			if tc.want != nil && (got == nil || *got != *tc.want) {
				t.Fatalf("parseUptimeSeconds(%q) = %v，預期 %d；否則會混淆沒量到與真的剛開機，operator 會誤判那台 Mac 一直在重開", tc.text, got, *tc.want)
			}
		})
	}
}

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
