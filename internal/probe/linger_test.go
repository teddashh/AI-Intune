package probe

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLingerEnabledInSeparatesNotEnabledFromNotMeasured(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "enabled"), nil, 0o600); err != nil {
		t.Fatalf("建立 linger 測試檔失敗：%v", err)
	}
	tests := []struct {
		name         string
		root         string
		unixUser     string
		wantEnabled  bool
		wantMeasured bool
	}{
		{name: "使用者檔存在", root: root, unixUser: "enabled", wantEnabled: true, wantMeasured: true},
		{name: "使用者檔不存在", root: root, unixUser: "disabled", wantEnabled: false, wantMeasured: true},
		{name: "使用者為空", root: root, unixUser: "", wantEnabled: false, wantMeasured: false},
		{name: "根目錄為空", root: "", unixUser: "enabled", wantEnabled: false, wantMeasured: false},
		{name: "根目錄不存在", root: filepath.Join(root, "missing"), unixUser: "enabled", wantEnabled: false, wantMeasured: true},
	}
	// 權限錯誤在以 root 執行的測試環境無法可靠製造，因此不以假的 fixture 測它。
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			enabled, measured := lingerEnabledIn(test.root, test.unixUser)
			if enabled != test.wantEnabled || measured != test.wantMeasured {
				t.Errorf("lingerEnabledIn(%q, %q) = (%t, %t)，預期 (%t, %t)；把「沒開」跟「沒量到」混成同一個 false，會讓畫面對一台根本沒有 linger 的機器說「未開啟」", test.root, test.unixUser, enabled, measured, test.wantEnabled, test.wantMeasured)
			}
		})
	}
}
