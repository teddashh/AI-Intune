package probe

import "os"

// prettyOSName 讀取 macOS 系統版本的權威檔案；macOS 沒有 /etc/os-release。
// 與 Linux 側相同，直接讀檔而不執行 sw_vers，以少啟動一個 subprocess。
// 尚未用實機確認目標 macOS 版本上的檔案是 XML plist 還是 binary plist；
// parseDarwinSystemVersion 使用 encoding/xml，若檔案是 binary plist，解析會失敗並回傳空字串。
// 這與改動前因 /etc/os-release 不存在而回傳空字串的行為完全相同，因此不會更差，只是不會改善。
// 拿到 Mac 後需要實測的就是 plist 格式；若需支援 binary plist，可加入 sw_vers 作為退路。
func prettyOSName() string {
	b, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist")
	if err != nil {
		return ""
	}
	return parseDarwinSystemVersion(b)
}
