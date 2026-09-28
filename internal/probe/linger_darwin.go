package probe

// macOS 沒有 systemd linger，所以這裡量不到，回傳 measured=false；不能假裝
// 量到一個 false。LaunchAgent 在使用者登出後會怎樣是另一個問題，這個函式
// 不回答它：沒有實機量過，不猜。
func lingerFacts(string) (enabled, measured bool) { return false, false }
