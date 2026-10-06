package main

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWindowsInstallerRendersEveryTaskPlaceholder(t *testing.T) {
	taskTokens := regexpSet(regexp.MustCompile(`@@[A-Z0-9_]+@@`), readOpsFile(t, "clawctl-agent.task.xml"), 0)
	script := readOpsFile(t, "install-agent-windows.ps1")
	for token := range taskTokens {
		if !strings.Contains(script, ".Replace('"+token+"'") && !strings.Contains(script, `.Replace("`+token+`"`) {
			t.Errorf("佔位符 %s 只在 task XML 裡，安裝程式不會代換它", token)
		}
	}
}

func TestWindowsScheduledTaskNameIsTheOneTheInstallerControls(t *testing.T) {
	task := readOpsFile(t, "clawctl-agent.task.xml")
	script := readOpsFile(t, "install-agent-windows.ps1")
	uri := regexp.MustCompile(`<URI>\\clawctl\\([^<]+)</URI>`).FindStringSubmatch(task)
	name := regexp.MustCompile(`(?m)^\$TaskName = '([^']+)'`).FindStringSubmatch(script)
	path := regexp.MustCompile(`(?m)^\$TaskPath = '([^']+)'`).FindStringSubmatch(script)
	if uri == nil || name == nil || path == nil {
		t.Fatal("無法同時讀出 task URI、腳本 TaskName 與 TaskPath")
	}
	if uri[1] != name[1] || path[1] != `\clawctl\` {
		t.Errorf("task URI=%q、腳本 TaskName=%q TaskPath=%q 對不上", uri[1], name[1], path[1])
	}
}

func TestWindowsInstallerDefaultBinarySourceMatchesMakefileOutput(t *testing.T) {
	makefileBytes, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("讀取 Makefile 失敗：%v", err)
	}
	makefile := string(makefileBytes)
	start := strings.Index(makefile, "cross-windows:")
	if start < 0 {
		t.Fatal("Makefile 找不到 cross-windows")
	}
	section := makefile[start:]
	if end := strings.Index(section, "\n\n"); end >= 0 {
		section = section[:end]
	}
	outputs := regexpSet(regexp.MustCompile(`-o\s+build/clawctl-agent-windows-([A-Za-z0-9_]+)`), section, 1)
	script := readOpsFile(t, "install-agent-windows.ps1")
	for arch := range outputs {
		if !strings.Contains(script, "clawctl-agent-windows-"+arch) &&
			!strings.Contains(script, `windows-$AgentArch`) {
			t.Errorf("Makefile 會產生 windows-%s，但安裝程式沒有對應候選", arch)
		}
	}
	if !strings.Contains(script, "clawctl-agent-windows-$AgentArch") {
		t.Error("安裝程式預設候選沒有 clawctl-agent-windows-$AgentArch")
	}
}

func TestWindowsScheduledTaskIsWellFormedXML(t *testing.T) {
	decoder := xml.NewDecoder(strings.NewReader(readOpsFile(t, "clawctl-agent.task.xml")))
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("task XML 壞掉：%v", err)
		}
	}
}

func TestWindowsScheduledTaskStartsDaemonAndInstallerRequiresStoredPlatformEvidence(t *testing.T) {
	task := readOpsFile(t, "clawctl-agent.task.xml")
	if !strings.Contains(task, "<LogonTrigger>") || !strings.Contains(task, "InteractiveToken") ||
		!strings.Contains(task, "LeastPrivilege") {
		t.Fatal("task XML 不是目前使用者的 logon task")
	}
	if strings.Contains(task, "LocalSystem") || strings.Contains(task, "S-1-5-18") {
		t.Fatal("task XML 指定了 LocalSystem")
	}
	if got := dispatch(nil); got != actAgent {
		t.Fatalf("零參數啟動會 dispatch 到 %q，不是 agent check-in loop", got)
	}
	installer := readOpsFile(t, "install-agent-windows.ps1")
	if !strings.Contains(installer, "verify --hub $Hub --since $ServiceStartedAt --timeout 2m --require-platform-evidence") {
		t.Fatal("Windows installer 沒有要求本次 scheduled task 啟動後的 Hub-stored OS/arch evidence")
	}
	if !strings.Contains(installer, "Register-ScheduledTask") {
		t.Fatal("Windows installer 沒有註冊 scheduled task")
	}
}

func TestLinuxInstallerNamesAWindowsInstallerThatExists(t *testing.T) {
	linuxInstaller := readOpsFile(t, "install-agent.sh")
	const windowsPath = "ops/install-agent-windows.ps1"
	if !strings.Contains(linuxInstaller, windowsPath) {
		t.Fatalf("Linux 安裝腳本沒有指出 %s", windowsPath)
	}
	info, err := os.Stat(filepath.Join("..", "..", windowsPath))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("Linux 安裝腳本指出的 %s 不是存在的一般檔", windowsPath)
	}
}
