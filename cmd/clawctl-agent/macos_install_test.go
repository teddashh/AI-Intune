package main

// 同一個事實寫在 plist、安裝腳本、Makefile 三個檔案裡，而沒有任何東西
// 會同時打開兩個以上。launchd 對它不認得的 key 是安靜忽略的，所以寫錯
// 不會紅。這些測試把跨檔案的安裝契約放在同一處核對。

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readOpsFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "ops", name))
	if err != nil {
		t.Fatalf("讀取 ops/%s 失敗：%v", name, err)
	}
	return string(b)
}

func regexpSet(re *regexp.Regexp, text string, group int) map[string]bool {
	set := make(map[string]bool)
	for _, match := range re.FindAllStringSubmatch(text, -1) {
		set[match[group]] = true
	}
	return set
}

func TestMacOSInstallerRendersEveryPlistPlaceholder(t *testing.T) {
	plistTokens := regexpSet(regexp.MustCompile(`@@[A-Z0-9_]+@@`), readOpsFile(t, "clawctl-agent.plist"), 0)
	scriptTokens := regexpSet(regexp.MustCompile(`\$\{PLIST_CONTENT//(@@[A-Z0-9_]+@@)/`), readOpsFile(t, "install-agent-macos.sh"), 1)
	for token := range plistTokens {
		if !scriptTokens[token] {
			t.Errorf("佔位符 %s 只在 plist 裡，安裝腳本不會代換它", token)
		}
	}
	for token := range scriptTokens {
		if !plistTokens[token] {
			t.Errorf("佔位符 %s 只在安裝腳本裡，plist 沒有使用它", token)
		}
	}
}

func TestMacOSLaunchAgentLabelIsTheOneTheInstallerControls(t *testing.T) {
	plist := readOpsFile(t, "clawctl-agent.plist")
	script := readOpsFile(t, "install-agent-macos.sh")
	plistMatch := regexp.MustCompile(`(?s)<key>Label</key>\s*<string>([^<]+)</string>`).FindStringSubmatch(plist)
	labelMatch := regexp.MustCompile(`(?m)^LABEL="([^"]+)"`).FindStringSubmatch(script)
	fileMatch := regexp.MustCompile(`(?m)^PLIST_FILE="[^"]*/([^/"]+)\.plist"`).FindStringSubmatch(script)
	if plistMatch == nil || labelMatch == nil || fileMatch == nil {
		t.Fatal("無法同時讀出 plist Label、腳本 LABEL 與 PLIST_FILE；對不上時 bootstrap 會成功，kickstart 才會說找不到那個服務")
	}
	if plistMatch[1] != labelMatch[1] || plistMatch[1] != fileMatch[1] {
		t.Errorf("plist Label=%q、腳本 LABEL=%q、PLIST_FILE 名稱=%q 對不上；bootstrap 會成功，kickstart 才會說找不到那個服務", plistMatch[1], labelMatch[1], fileMatch[1])
	}
}

func TestMacOSInstallerDefaultBinarySourceMatchesMakefileOutput(t *testing.T) {
	makefileBytes, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("讀取 Makefile 失敗：%v", err)
	}
	makefile := string(makefileBytes)
	start := strings.Index(makefile, "cross-darwin:")
	if start < 0 {
		t.Fatal("Makefile 找不到 cross-darwin；使用者跑完 make cross-darwin，安裝腳本仍會說 Agent binary not found")
	}
	section := makefile[start:]
	if end := strings.Index(section, "\n\n"); end >= 0 {
		section = section[:end]
	}
	outputs := regexpSet(regexp.MustCompile(`-o\s+build/clawctl-agent-darwin-([A-Za-z0-9_]+)`), section, 1)
	script := readOpsFile(t, "install-agent-macos.sh")
	architectures := regexpSet(regexp.MustCompile(`(?m)^\s*[^)]*\)\s+AGENT_ARCH="([^"]+)"`), script, 1)
	for arch := range outputs {
		if !architectures[arch] {
			t.Errorf("Makefile 會產生 darwin-%s，但安裝腳本不認得；使用者剛跑完 make cross-darwin，安裝腳本仍會說 Agent binary not found", arch)
		}
	}
	for arch := range architectures {
		if !outputs[arch] {
			t.Errorf("安裝腳本會尋找 darwin-%s，但 make cross-darwin 不會產生；安裝腳本會說 Agent binary not found", arch)
		}
	}
	if !strings.Contains(script, `"$SCRIPT_DIR/../build/clawctl-agent-darwin-$AGENT_ARCH"`) {
		t.Error("安裝腳本預設候選沒有 $SCRIPT_DIR/../build/clawctl-agent-darwin-$AGENT_ARCH；使用者剛跑完 make cross-darwin，仍會看到 Agent binary not found")
	}
}

func TestMacOSLaunchAgentUsesOnlyKeysLaunchdHonours(t *testing.T) {
	allowed := map[string]bool{"Label": true, "ProgramArguments": true, "RunAtLoad": true, "KeepAlive": true, "ThrottleInterval": true, "ProcessType": true, "StandardOutPath": true, "StandardErrorPath": true, "WorkingDirectory": true, "EnvironmentVariables": true}
	keys := regexpSet(regexp.MustCompile(`<key>([^<]+)</key>`), readOpsFile(t, "clawctl-agent.plist"), 1)
	for key := range keys {
		if !allowed[key] {
			t.Errorf("plist key %q 不在已核准白名單；launchd 對不認得的 key 會安靜忽略，WatchdogSec／CPUQuota／MemoryMax 不會有效果也不會有東西變紅；加新 key 前要先證明 launchd 真的吃它", key)
		}
	}
}

func TestMacOSLaunchAgentIsWellFormedXML(t *testing.T) {
	decoder := xml.NewDecoder(strings.NewReader(readOpsFile(t, "clawctl-agent.plist")))
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("plist XML 壞掉：%v；launchctl bootstrap 會拒收，但此時 binary 已裝好、報到也已做完", err)
		}
	}
}

func TestMacOSLaunchAgentStartsDaemonAndInstallerRequiresStoredPlatformEvidence(t *testing.T) {
	plist := readOpsFile(t, "clawctl-agent.plist")
	arguments := regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>(.*?)</array>`).FindStringSubmatch(plist)
	if arguments == nil {
		t.Fatal("plist 沒有 ProgramArguments；launchd 不知道要啟動哪個 process")
	}
	if count := len(regexp.MustCompile(`<string>[^<]+</string>`).FindAllString(arguments[1], -1)); count != 1 {
		t.Fatalf("launchd 傳了 %d 個參數項，預期只有 binary；clawctl-agent 只有零參數才會進 daemon/check-in loop", count)
	}
	if got := dispatch(nil); got != actAgent {
		t.Fatalf("plist 的零參數啟動會 dispatch 到 %q，不是 agent check-in loop", got)
	}
	installer := readOpsFile(t, "install-agent-macos.sh")
	if !strings.Contains(installer, `verify --hub "$HUB" --since "$SERVICE_STARTED_AT" --timeout 2m --require-platform-evidence`) {
		t.Fatal("macOS installer 沒有要求本次 launchd 啟動後的 Hub-stored OS/arch evidence；只看到 process/check-in 不能證明 evidence 已落帳")
	}
}

func TestLinuxInstallerNamesAMacOSInstallerThatExists(t *testing.T) {
	linuxInstaller := readOpsFile(t, "install-agent.sh")
	const macOSPath = "ops/install-agent-macos.sh"
	if !strings.Contains(linuxInstaller, macOSPath) {
		t.Fatalf("Linux 安裝腳本沒有指出 %s；這是 macOS 使用者唯一會看到的下一步，指到不存在的檔案比不指還糟", macOSPath)
	}
	info, err := os.Stat(filepath.Join("..", "..", macOSPath))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("Linux 安裝腳本指出的 %s 不是存在且可執行的一般檔；這是 macOS 使用者唯一會看到的下一步，指到不存在的檔案比不指還糟", macOSPath)
	}
}
