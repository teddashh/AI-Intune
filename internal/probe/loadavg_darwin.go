package probe

// macOS 沒有 /proc/loadavg；雖然有 vm.loadavg sysctl，但尚未在實機量過它的
// 結構解法，所以這裡回「沒量到」而不是猜一個數字。拿到 Mac 後要補的就是這一檔。
func load1m() *float64 { return nil }
