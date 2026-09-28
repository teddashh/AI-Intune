package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 2026-09-03，跟 §5.6 同一天、同一種錯，但這一次假話印在螢幕上：
//
//	$ clawctl-hub machines
//	名冊上 0 台，0 台正在回報。
//
// 那台機器上有 5 台在名冊裡、4 台正在回報。
// unit 檔跑 `--db …/clawctl.sqlite`，CLI 的預設是 `…/hub.sqlite`。
// 兩個檔案各自看都合理。
//
// 最糟的地方是它會**自己造出**支持自己答案的證據：
// store.Open 看到檔案不存在就建一個新的空 DB，然後那句「0 台」變成
// 對那個新檔案而言完全正確的陳述。自洽、完全錯誤，還留下一個 200KB 的
// 誘餌檔在旁邊，讓下一個人分不出哪個才是真的。
//
// 「名冊上 N 台」是這個產品最重要的一句話。它不准在讀錯檔案的時候還講得出來。

func TestServiceAndCLIUseTheSameDatabase(t *testing.T) {
	b, err := os.ReadFile("../../ops/clawctl-hub.service")
	if err != nil {
		t.Fatalf("讀不到 unit 檔：%v", err)
	}
	// Service now pins the ledger path so CLAWCTL_DB from EnvironmentFile or
	// the user manager cannot move the live writer away from upgrade-hub.sh's
	// snapshot/restore target. Derive the expected value from defaultDB() here;
	// do not hand-copy a third path into the test.
	m := regexp.MustCompile(`--db\s+(\S+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("unit 沒有用 --db 釘住 live ledger；CLAWCTL_DB 可讓 upgrade 備份錯檔")
	}
	t.Setenv("CLAWCTL_DB", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	unitDB := strings.Replace(string(m[1]), "%h", home, 1)
	if filepath.Clean(unitDB) != filepath.Clean(defaultDB()) {
		t.Errorf("unit live ledger=%s，CLI defaultDB=%s；兩者會看到不同名冊", unitDB, defaultDB())
	}

	// EnvironmentFile must not get a second way to override the fixed argument.
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Environment=CLAWCTL_DB=") {
			t.Errorf("unit 檔用 %s 加了第二個 DB authority", strings.TrimSpace(line))
		}
	}
}

// 唯讀指令不准把資料庫生出來。
//
// ⚠ 這一條比路徑對不對更重要：路徑遲早還是會有人打錯（--db 打錯、
// 換使用者跑、sudo 跑）。真正的防線是「檔案不在就不准回答」。
func TestReadOnlyCommandRefusesToInventAnEmptyFleet(t *testing.T) {
	missing := t.TempDir() + "/沒有這個檔.sqlite"

	if _, err := openExisting(missing); err == nil {
		t.Fatal("資料庫不存在卻開得起來 —— 那接下來那句「名冊上 0 台」會是憑空生出來的")
	} else if !strings.Contains(err.Error(), missing) {
		t.Errorf("錯誤訊息沒帶路徑，看的人不會知道自己讀錯檔案了：%v", err)
	}

	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("失敗的唯讀指令還是把檔案建出來了 —— 這會留下一個誘餌，"+
			"下一個人看到兩個 .sqlite 分不出哪個是真的", err)
	}
}
