//go:build windows

package probe

import (
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// prettyOSName combines RtlGetNtVersionNumbers with CurrentVersion registry
// strings, retaining the measured NT version without guessing a client edition.
func prettyOSName() string {
	major, minor, build := windows.RtlGetNtVersionNumbers()
	product, display := windowsCurrentVersionStrings()
	return windowsDisplayName(windowsVersionFacts{
		Major:          major,
		Minor:          minor,
		Build:          build,
		ProductName:    product,
		DisplayVersion: display,
	})
}

func windowsCurrentVersionStrings() (productName, displayVersion string) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
		registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", ""
	}
	defer key.Close()
	productName, _, _ = key.GetStringValue("ProductName")
	displayVersion, _, _ = key.GetStringValue("DisplayVersion")
	if strings.TrimSpace(displayVersion) == "" {
		displayVersion, _, _ = key.GetStringValue("ReleaseId")
	}
	return strings.TrimSpace(productName), strings.TrimSpace(displayVersion)
}
