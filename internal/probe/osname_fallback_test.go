package probe

import (
	"testing"

	"github.com/teddashh/AI-Intune/internal/catalog"
)

// ProductVersion is the key CoreFoundation reads from SystemVersion.plist.
// The user-visible override is optional and must win regardless of key order.
func TestDarwinProductVersionWithoutAVisibleOverrideStillIdentifiesAMac(t *testing.T) {
	const name = `<key>ProductName</key><string>macOS</string>`
	const version = `<key>ProductVersion</key><string>15.1</string>`
	const visible = `<key>ProductUserVisibleVersion</key><string>15.1.1</string>`
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"product version", name + version, "macOS 15.1"},
		{"version before name", version + name, "macOS 15.1"},
		{"empty override", name + `<key>ProductUserVisibleVersion</key><string></string>` + version, "macOS 15.1"},
		{"later override", name + version + visible, "macOS 15.1.1"},
		{"earlier override", visible + version + name, "macOS 15.1.1"},
		{"no product name", version, ""},
		{"empty version", name + `<key>ProductVersion</key><string></string>`, ""},
		{"nonstring version", name + `<key>ProductVersion</key><integer>15</integer>`, ""},
		{"truncated suffix", name + version + `<key>ProductBuildVersion</key><string>`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := parseDarwinSystemVersion([]byte(`<plist><dict>` + test.body + `</dict></plist>`))
			if got != test.want {
				t.Fatalf("OS display=%q, want %q", got, test.want)
			}
			if test.want == "" {
				return
			}
			for _, arch := range []string{"arm64", "x86_64"} {
				target, err := catalog.PlatformFromProbeIdentity(got, arch)
				wantArch := map[string]string{"arm64": "arm64", "x86_64": "amd64"}[arch]
				if target != (catalog.Platform{OS: "darwin", Arch: wantArch}) || err != nil {
					t.Fatalf("Mac identity must remain a darwin target: target=%+v err=%v", target, err)
				}
			}
		})
	}
}
