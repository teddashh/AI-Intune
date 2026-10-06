package probe

// prettyOSName reads SystemVersion.plist. A working XML parse is the identity
// and does not start sw_vers. Missing, oversized, or non-XML (including
// binary) files use a bounded /usr/bin/sw_vers. Failure stays empty.
func prettyOSName() string {
	b, err := readDarwinSystemVersionPlist("/System/Library/CoreServices/SystemVersion.plist")
	return resolveDarwinPrettyOSName(b, err, invokeDarwinSwVers)
}
