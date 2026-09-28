package probe

func uptimeSeconds() *int64 { return parseUptimeSeconds(readTrimmed("/proc/uptime")) }
