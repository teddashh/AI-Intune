//go:build unix

package probe

// machineIDHint：比 hostname 穩定，但重灌會變，所以只是候選之一，不是身分本身。
func machineIDHint() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if s := readTrimmed(p); s != "" {
			return s
		}
	}
	return ""
}
