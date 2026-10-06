//go:build windows

package probe

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// machineIDHint reads MachineGuid. Reinstall changes it; it is a candidate, not identity.
func machineIDHint() string {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Cryptography`,
		registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer key.Close()
	guid, _, err := key.GetStringValue("MachineGuid")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(guid)
}
