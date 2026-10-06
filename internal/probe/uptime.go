package probe

import (
	"strconv"
	"strings"
)

// parseUptimeSeconds 解析 /proc/uptime 的第一個欄位。
func parseUptimeSeconds(text string) *int64 {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return nil
	}
	seconds := int64(v)
	return &seconds
}
