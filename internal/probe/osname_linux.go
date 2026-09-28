package probe

import "os"

// prettyOSName 讀 /etc/os-release 的 PRETTY_NAME。
// 原型用 `. /etc/os-release && echo $PRETTY_NAME` 起了一個 shell；這裡直接解析檔案，
// 少一個 subprocess 也少一次 shell 注入面。
func prettyOSName() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	return parseOSReleasePrettyName(b)
}
