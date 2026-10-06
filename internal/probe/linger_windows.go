//go:build windows

package probe

// Windows has no systemd linger. measured=false is "not measured", not "off".
func lingerFacts(string) (enabled, measured bool) { return false, false }
