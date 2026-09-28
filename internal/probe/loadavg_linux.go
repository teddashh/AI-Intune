package probe

func load1m() *float64 { return parseLoad1m(readTrimmed("/proc/loadavg")) }
