package probe

import (
	"errors"
	"math"
	"testing"

	"github.com/teddashh/AI-Intune/internal/catalog"
)

func TestWindowsNativeFacts(t *testing.T) {
	t.Run("identity", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			facts windowsVersionFacts
			want  string
		}{
			{"missing", windowsVersionFacts{}, ""},
			{"client or server", windowsVersionFacts{Major: 10, Build: 26100}, "Windows NT 10.0.26100"},
			{"server", windowsVersionFacts{Major: 10, Build: 26100, ProductName: "Windows Server 2025"}, "Windows NT 10.0.26100 (Windows Server 2025)"},
			{"registry product", windowsVersionFacts{Major: 10, Build: 26100, ProductName: "Windows 11 Pro", DisplayVersion: "24H2"}, "Windows NT 10.0.26100 (Windows 11 Pro 24H2)"},
			{"compat product", windowsVersionFacts{Major: 10, Build: 22621, ProductName: "Windows 10 Pro"}, "Windows NT 10.0.22621 (Windows 10 Pro)"},
			{"invalid product", windowsVersionFacts{Major: 10, Build: 26100, ProductName: "Ubuntu"}, "Windows NT 10.0.26100"},
			{"invalid display", windowsVersionFacts{Major: 10, Build: 26100, ProductName: "Windows 11", DisplayVersion: "24H2\ninvalid"}, "Windows NT 10.0.26100"},
			{"older kernel", windowsVersionFacts{Major: 6, Minor: 1, Build: 7601}, "Windows NT 6.1.7601"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := windowsDisplayName(tc.facts)
				if got != tc.want {
					t.Fatalf("display=%q, want %q", got, tc.want)
				}
				if got != "" {
					platform, err := catalog.PlatformFromProbeIdentity(got, "x86_64")
					if err != nil || platform.OS != "windows" {
						t.Fatalf("platform=%+v err=%v", platform, err)
					}
				}
			})
		}
	})
	t.Run("native architecture", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			native uint16
			err    error
			want   string
		}{
			{"native amd64", windowsMachineAMD64, nil, "x86_64"},
			{"arm64 including emulated amd64", windowsMachineARM64, nil, "aarch64"},
			{"unsupported x86", 0x14c, nil, ""},
			{"unavailable or failed", windowsMachineAMD64, errors.New("unavailable"), ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := windowsNativeArch(func() (uint16, error) { return tc.native, tc.err })
				if got != tc.want {
					t.Fatalf("arch=%q, want %q", got, tc.want)
				}
			})
		}
	})
	t.Run("uptime", func(t *testing.T) {
		for _, ms := range []uint64{0, 999, 123456789} {
			got := tickCountUptimeSeconds(ms)
			if got == nil || *got != int64(ms/1000) {
				t.Fatalf("uptime(%d)=%v", ms, got)
			}
		}
	})
	t.Run("resource measurements", func(t *testing.T) {
		for _, tc := range []struct {
			total, available         uint64
			wantTotal, wantAvailable int64
		}{
			{16 << 30, 4 << 30, 16 << 30, 4 << 30},
			{8192, 0, 8192, 0},
			{0, 0, 0, 0},
			{8192, 16384, 0, 0},
			{math.MaxUint64, 4096, 0, 0},
		} {
			total, available := windowsMemoryBytes(tc.total, tc.available)
			free, diskTotal := windowsDiskBytes(tc.available, tc.total)
			if total != tc.wantTotal || available != tc.wantAvailable || free != tc.wantAvailable || diskTotal != tc.wantTotal {
				t.Fatalf("measurements=%d/%d disk=%d/%d, want total=%d available=%d", total, available, diskTotal, free, tc.wantTotal, tc.wantAvailable)
			}
		}
	})
}
