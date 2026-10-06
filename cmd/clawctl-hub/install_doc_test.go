package main

// 同一個事實被寫在好幾個檔案裡，而沒有任何東西會同時讀它們。
//
// ⚠ 這一整支測試是 2026-09-04 那次乾淨機器實測長出來的。當天的狀況是：
// 文件說把 binary 裝到 /usr/local/bin，unit 的 ExecStart 寫 %h/.local/bin，
// Makefile 把產物編到 build/ 而文件寫 bin/ —— 三個檔案，三個答案，
// 全部都是綠的，因為 repo 裡沒有一個測試會同時打開兩個以上的檔案。
//
// 那四行手打指令**每一行都曾經是對的**。它們不是被寫錯，是過期了。
// 而過期不會讓任何東西變紅。
//
// ⚠⚠ 這些測試的解析刻意**限定在 §2.2 那一節之內**，不整份 md 亂 grep。
// 這個 repo 已經被「在敘述文字裡撈到假的命中」咬過四次（§5.13、§5.21…），
// 而 §5 裡到處都在引用這些路徑當例子 —— 整份 grep 一定會撈到它們。
//
// 骨架（區段切割、ExecStart 的 `\` 續行處理）出自協力的 codex。
// 它交的版本比對的是「文件 vs unit」，而那個版本**紅得起來、綠不了**：
// ExecStart 是 `%h/.local/bin/clawctl-hub`，那是 systemd 的 specifier，
// 沒有任何一行人類會寫的 shell 指令長成那樣。一支永遠不可能通過的測試
// 不是測試，是一句斷言說世界壞了 —— 而「它是紅的，很好」跟
// 「它是綠的，很好」是同一種偷懶。所以現在比對的是**真的會被執行的那份**
// （ops/install-hub.sh），而 %h 與 $HOME 有正規化。

import (
	"bufio"
	"regexp"
	"strings"
	"testing"
)

// --- ops/install-hub.sh 裡真正在搬檔案的那兩行
var (
	installerBinDirRE = regexp.MustCompile(`(?m)^BIN_DIR="([^"]+)"`)
	installerSrcRE    = regexp.MustCompile(`(?m)^\s*BIN_SRC="\$HERE/\.\./([^"]+)"`)
	makeHubOutputRE   = regexp.MustCompile(`(?m)^\s*\$\(BUILD\)\s+-o\s+(\S+)\s+\./cmd/clawctl-hub\s*$`)
	cliListenRE       = regexp.MustCompile(`fs\.String\("listen",\s*"([^"]+)"`)
)

// TestInstallerPutsBinaryWhereTheUnitLooksForIt
//
// ⚠ 防的是：安裝腳本跟 unit 各自看都對，合在一起卻找不到 binary。
// 這個失效的症狀特別難認 —— systemctl 會說 unit 存在、enable 會成功，
// 然後 start 的時候才吐一個關於 203/EXEC 的錯，而那個訊息裡沒有一個字
// 提到「你把檔案裝到別的地方去了」。
func TestInstallerPutsBinaryWhereTheUnitLooksForIt(t *testing.T) {
	sh := readRepoFile(t, "../../ops/install-hub.sh")
	m := installerBinDirRE.FindStringSubmatch(sh)
	if m == nil {
		t.Fatal(`ops/install-hub.sh 裡找不到 BIN_DIR="..."`)
	}
	installed := normalizeHome(strings.TrimSuffix(m[1], "/") + "/clawctl-hub")

	execPath, _ := unitSettings(t)
	wanted := normalizeHome(execPath)

	if installed != wanted {
		t.Errorf("unit 會去 %s 找 binary，但 install-hub.sh 把它裝到 %s\n"+
			"（原文：ExecStart=%s，BIN_DIR=%s）", wanted, installed, execPath, m[1])
	}
}

// TestInstallerDefaultSourceMatchesMakefileOutput
//
// ⚠ 防的是：Makefile 改了產物目錄，安裝腳本繼續從舊路徑取檔。
// 2026-09-04 之前文件寫的是 bin/，Makefile 寫的是 build/ ——
// 那個錯活了整個 Phase 1，因為沒有人執行過那份文件。
func TestInstallerDefaultSourceMatchesMakefileOutput(t *testing.T) {
	sh := readRepoFile(t, "../../ops/install-hub.sh")
	m := installerSrcRE.FindStringSubmatch(sh)
	if m == nil {
		t.Fatal(`ops/install-hub.sh 裡找不到預設的 BIN_SRC="$HERE/../..."`)
	}
	makefile := readRepoFile(t, "../../Makefile")
	out := makeHubOutputRE.FindStringSubmatch(makefile)
	if out == nil {
		t.Fatal("Makefile 裡找不到 ./cmd/clawctl-hub 的 $(BUILD) -o 產物")
	}
	if m[1] != out[1] {
		t.Errorf("Makefile 把 Hub 編到 %s，但 install-hub.sh 預設從 %s 取檔", out[1], m[1])
	}
}

// TestInstallDocsAndScriptRequireOperatorAuthInputs
//
// 新版 Hub 不存在「可用的 loopback 預設」：unit 裡的值只是
// fail-closed placeholder。真正的安裝契約是「明示 tailnet listener +
// 明示 capability prefix + 事先存好 grant」，而且 healthz 綠不算
// operator console 可用。這支測試把文件、腳本與 unit 的三份真相綁在一起。
func TestInstallDocsAndScriptRequireOperatorAuthInputs(t *testing.T) {
	body := hubSection(t)
	script := readRepoFile(t, "../../ops/install-hub.sh")
	unit := readRepoFile(t, "../../ops/clawctl-hub.service")

	for _, want := range []string{
		"--listen",
		"--operator-capability-prefix",
		"OPERATOR-AUTH.md",
		"Tailscale literal IP",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("docs/DEPLOY-OSS.md §2.2 沒有保留 operator auth 安裝契約 %q", want)
		}
	}
	for _, want := range []string{
		"CLAWCTL_OPERATOR_CAPABILITY_PREFIX",
		"GET / HTTP/1.0",
		"operator 首頁",
		"docs/OPERATOR-AUTH.md",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("ops/install-hub.sh 沒有實作 operator auth 安裝/驗收契約 %q", want)
		}
	}

	_, unitListen := unitSettings(t)
	if unitListen != "127.0.0.1:8787" {
		t.Errorf("unit 的 fail-closed placeholder 變成 %s；若要改契約，請同步改文件與測試", unitListen)
	}
	if !strings.Contains(unit, "fail-closed placeholder") {
		t.Error("ops/clawctl-hub.service 沒有把 loopback 明示標成 fail-closed placeholder")
	}
}

// TestLoopbackDefaultsFailBeforeOpeningDB
//
// CLI 與 unit 仍然是兩個不同的 loopback port，但這個差異現在不應該能
// 打開第二個 writer。它們必須在 store.Open 前先經過 literal destination
// 與 operatorauth.New，並被非 Tailscale destination 驟回。
func TestLoopbackDefaultsFailBeforeOpeningDB(t *testing.T) {
	main := readRepoFile(t, "main.go")
	serveAt := strings.Index(main, "func serve(argv []string)")
	if serveAt < 0 {
		t.Fatal("main.go 裡找不到 serve function")
	}
	serveBody := main[serveAt:]
	m := cliListenRE.FindStringSubmatch(serveBody)
	if m == nil {
		t.Fatal(`main.go 裡找不到 fs.String("listen", ...)`)
	}
	_, unitListen := unitSettings(t)
	for label, listen := range map[string]string{"CLI": m[1], "unit": unitListen} {
		if !strings.HasPrefix(listen, "127.0.0.1:") {
			t.Errorf("%s 預設 %s 不是預期的 fail-closed loopback", label, listen)
		}
	}

	doc := hubSection(t)
	for _, want := range []string{"fail-closed placeholder", "不再能安靜啟動第二個 writer"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/DEPLOY-OSS.md §2.2 沒有說明 loopback 失敗契約 %q", want)
		}
	}

	destinationAt := strings.Index(serveBody, "operatorDestinationFromListen(*addr)")
	authorizerAt := strings.Index(serveBody, "operatorauth.New(")
	listenAt := strings.Index(serveBody, `net.Listen("tcp", *addr)`)
	storeAt := strings.Index(serveBody, "openServeStore(*dbPath)")
	if destinationAt < 0 || authorizerAt < 0 || listenAt < 0 || storeAt < 0 {
		t.Fatalf("main.go 找不到 bind/auth-before-store 錨點：destination=%d authorizer=%d listen=%d store=%d",
			destinationAt, authorizerAt, listenAt, storeAt)
	}
	if !(destinationAt < authorizerAt && authorizerAt < listenAt && listenAt < storeAt) {
		t.Errorf("main.go 必須在 validated Store.Open 前驗證並 bind listener：destination=%d authorizer=%d listen=%d store=%d",
			destinationAt, authorizerAt, listenAt, storeAt)
	}
}

// TestInstallDocDelegatesInsteadOfDuplicating
//
// ⚠⚠ 這一支是這個檔案裡最重要的一支，因為它守的是**成因**而不是症狀。
//
// 上面那些不一致之所以會發生，是因為 §2.2 曾經是一份手抄本 —— 它把
// install-hub.sh 該做的事逐行抄成文字。抄本不會壞，只會過期，
// 而過期不會讓任何東西變紅。
//
// 所以現在的規則是：§2.2 不准再出現手打的 `install ... clawctl-hub`。
// 要教人裝 Hub 就指向那支腳本。腳本會壞，文字只會過期。
func TestInstallDocDelegatesInsteadOfDuplicating(t *testing.T) {
	body := hubSection(t)
	handCopy := regexp.MustCompile(`(?m)^\s*install\s+.*clawctl-hub`)
	if loc := handCopy.FindString(body); loc != "" {
		t.Errorf("docs/DEPLOY-OSS.md §2.2 又出現手打的安裝指令：%q\n"+
			"這正是 2026-09-04 那四行全紅的成因 —— 它們每一行都曾經是對的，"+
			"然後 unit 跟 Makefile 各自往前走，而文字不會跟著紅。"+
			"請改成呼叫 ops/install-hub.sh。", strings.TrimSpace(loc))
	}
	if !strings.Contains(body, "install-hub.sh") {
		t.Error("docs/DEPLOY-OSS.md §2.2 沒有提到 ops/install-hub.sh —— " +
			"那現在讀者要怎麼裝 Hub？")
	}
}

// ---------------------------------------------------------------- 解析

// normalizeHome 把 systemd 的 %h、shell 的 $HOME、以及 ~ 拉成同一種寫法。
// ⚠ 沒有這一步，`%h/.local/bin/clawctl-hub` 跟 `$HOME/.local/bin/clawctl-hub`
// 會被判成不同 —— 於是這支測試在正確的情況下也是紅的。
func normalizeHome(p string) string {
	for _, prefix := range []string{"%h", "$HOME", "${HOME}", "~"} {
		if strings.HasPrefix(p, prefix+"/") {
			return "<HOME>" + strings.TrimPrefix(p, prefix)
		}
	}
	return p
}

// hubSection 只回 docs/DEPLOY-OSS.md §2.2 那一節。
// PHASE1.md 是私人工作筆記，不在這個公開倉庫；安裝契約改鎖在部署指南這一節。
func hubSection(t *testing.T) string {
	t.Helper()
	doc := readRepoFile(t, "../../docs/DEPLOY-OSS.md")
	start := regexp.MustCompile(`(?m)^### 2\.2 Hub\s*$`).FindStringIndex(doc)
	if start == nil {
		t.Fatal("docs/DEPLOY-OSS.md 裡找不到 §2.2 Hub")
	}
	body := doc[start[1]:]
	if end := regexp.MustCompile(`(?m)^###\s+`).FindStringIndex(body); end != nil {
		body = body[:end[0]]
	}
	return body
}

// unitSettings 讀 ops/clawctl-hub.service 的 ExecStart 與 CLAWCTL_LISTEN。
// ⚠ ExecStart 是多行的（行尾 `\`），所以要先把續行接起來再看。
func unitSettings(t *testing.T) (execPath, listenAddr string) {
	t.Helper()
	unit := readRepoFile(t, "../../ops/clawctl-hub.service")
	for _, line := range continuedLines(unit) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ExecStart=") {
			if fields := strings.Fields(strings.TrimPrefix(line, "ExecStart=")); len(fields) > 0 {
				execPath = fields[0]
			}
		}
		if strings.HasPrefix(line, "Environment=CLAWCTL_LISTEN=") {
			listenAddr = strings.Trim(strings.TrimSpace(
				strings.TrimPrefix(line, "Environment=CLAWCTL_LISTEN=")), `"`)
		}
	}
	if execPath == "" {
		t.Fatal("ops/clawctl-hub.service 裡找不到 ExecStart 路徑")
	}
	if listenAddr == "" {
		t.Fatal("ops/clawctl-hub.service 裡找不到 CLAWCTL_LISTEN")
	}
	return execPath, listenAddr
}

func continuedLines(s string) []string {
	var lines []string
	var current strings.Builder
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t")
		continued := strings.HasSuffix(line, `\`)
		if continued {
			line = strings.TrimSpace(strings.TrimSuffix(line, `\`))
		}
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		current.WriteString(line)
		if !continued {
			lines = append(lines, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	return lines
}
