package probe

import (
	"testing"

	"github.com/teddashh/AI-Intune/internal/catalog"
)

const darwinSystemVersionFixture = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>ProductBuildVersion</key>
    <string>24B83</string>
    <key>ProductName</key>
    <string>macOS</string>
    <key>ProductUserVisibleVersion</key>
    <string>15.1</string>
    <key>ProductVersion</key>
    <string>15.1</string>
</dict>
</plist>`

func TestOSReleasePrettyNameParsesBothQuotedAndBareValues(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "quoted Ubuntu", in: "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n", want: "Ubuntu 24.04.1 LTS"},
		{name: "bare Debian", in: "NAME=Debian GNU/Linux\nPRETTY_NAME=Debian\n", want: "Debian"},
		{name: "missing", in: "NAME=Alpine Linux\nVERSION_ID=3.21\n", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseOSReleasePrettyName([]byte(test.in)); got != test.want {
				t.Fatalf("PRETTY_NAME 解析結果對不上：got=%q want=%q", got, test.want)
			}
		})
	}
}

func TestDarwinSystemVersionBecomesADisplayName(t *testing.T) {
	if got := parseDarwinSystemVersion([]byte(darwinSystemVersionFixture)); got != "macOS 15.1" {
		t.Fatalf("SystemVersion.plist 顯示名稱對不上：got=%q want=%q", got, "macOS 15.1")
	}
}

func TestDarwinSystemVersionWithoutBothKeysReportsNothing(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "missing ProductName", in: `<plist><dict><key>ProductUserVisibleVersion</key><string>15.1</string></dict></plist>`},
		{name: "missing ProductUserVisibleVersion", in: `<plist><dict><key>ProductName</key><string>macOS</string></dict></plist>`},
		{name: "malformed plist", in: `這不是 XML <key>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseDarwinSystemVersion([]byte(test.in)); got != "" {
				t.Fatalf("plist 鍵值不完整時應回空字串，卻得到 %q；回半截版本號比回空字串更糟，因為下游沒辦法分辨", got)
			}
		})
	}
}

// TestAMacIsNeverResolvedAsALinuxTarget 同時守住 probe 產生值與 catalog 解讀值。
func TestAMacIsNeverResolvedAsALinuxTarget(t *testing.T) {
	display := parseDarwinSystemVersion([]byte(darwinSystemVersionFixture))
	got, err := catalog.PlatformFromProbeIdentity(display, "arm64")
	if err != nil || got != (catalog.Platform{OS: "darwin", Arch: "arm64"}) {
		t.Fatalf("macOS probe 身分沒有保留為 darwin/arm64：platform=%+v err=%v", got, err)
	}
}
