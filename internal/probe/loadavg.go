package probe

import (
	"strconv"
	"strings"
)

// parseLoad1m 解析 /proc/loadavg 的第一個欄位。
func parseLoad1m(text string) *float64 {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil
	}
	return &v
}
