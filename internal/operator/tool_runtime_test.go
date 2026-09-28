package operator

import (
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/model"
)

// ⚠⚠ 正式機隊 2026-09-12 samplehub1 的形狀：Hub 量版號的是 ~/.local/bin/openclaw，真正
// 活著的 process 跑的是 clawctl 自己放的那一份。兩份的版號今天剛好一樣，所以畫面是對
// 的——而這一格必須講出它是巧合。
func TestAVersionMeasuredOnOneFileDoesNotSpeakForAnother(t *testing.T) {
	finding := ToolRuntimeOf(model.CLITool{
		Name: "openclaw", Present: true, OnPath: true, PathSource: model.PathSourceLogin,
		Path: "/home/example-user/.local/bin/openclaw", RealPath: "/home/example-user/.local/bin/openclaw",
		VersionReported: "2026.6.6", RunningPID: 1474693, RunningExe: "/usr/bin/node",
		RunningScript: "/home/example-user/.local/share/clawctl/openclaw/releases/2026.6.6/lib/node_modules/openclaw/dist/index.js",
	})
	if finding.State != ToolRuntimeOtherFile {
		t.Fatalf("state=%q", finding.State)
	}
	// ⚠ 兩個檔案都要講出來。少了其中一個，讀的人不知道該去收掉哪一份。
	if finding.MeasuredFile != "/home/example-user/.local/bin/openclaw" {
		t.Errorf("量的那一份=%q", finding.MeasuredFile)
	}
	if finding.RunningFile == "" || finding.RunningFile == finding.MeasuredFile {
		t.Errorf("跑的那一份=%q", finding.RunningFile)
	}
	if finding.Title != ToolRuntimeTitle(ToolRuntimeOtherFile) ||
		finding.Meaning != ToolRuntimeMeaning(ToolRuntimeOtherFile) ||
		finding.NextStep != ToolRuntimeNextStep(ToolRuntimeOtherFile) {
		t.Errorf("finding=%+v", finding)
	}
}

// ⚠⚠ node CLI 的 running_exe 一律是 /usr/bin/node，它對「跑的是哪一份」一點資訊都沒
// 有。正式機隊 sampleagent2 的 openclaw 就是這一格：有 PID、只有 exe。把它講成「正在跑的是
// 另一個檔案」，等於憑一個解譯器的路徑宣布這台上有兩份安裝，而下一步會是去找一個不存
// 在的檔案。
func TestAnInterpreterIsNotASecondInstall(t *testing.T) {
	finding := ToolRuntimeOf(model.CLITool{
		Name: "openclaw", Present: true, OnPath: true,
		Path: "/usr/bin/openclaw", RealPath: "/usr/lib/node_modules/openclaw/openclaw.mjs",
		VersionReported: "2026.5.20", RunningPID: 3565308, RunningExe: "/usr/bin/node",
	})
	if finding.State != ToolRuntimeUnattributed {
		t.Fatalf("state=%q", finding.State)
	}
	// 說不出來就不要給一個路徑。一個填著 /usr/bin/node 的「正在跑的檔案」會被引用。
	if finding.RunningFile != "" {
		t.Errorf("跑的那一份=%q", finding.RunningFile)
	}
}

// 一個自己就是執行檔的工具（不是 node CLI）對得上安裝路徑的時候，這一格才是「量的就是
// 跑的」。⚠ 這一種是四種「有 process」裡唯一沒有下一步的。
func TestABinaryRunningItsOwnInstalledFileIsTheSameFile(t *testing.T) {
	finding := ToolRuntimeOf(model.CLITool{
		Name: "agy", Present: true, OnPath: true,
		Path: "/home/ubuntu/.local/bin/agy", RealPath: "/home/ubuntu/.local/bin/agy",
		RunningPID: 1055101, RunningExe: "/home/ubuntu/.local/bin/agy",
	})
	if finding.State != ToolRuntimeSameFile {
		t.Fatalf("state=%q", finding.State)
	}
	if finding.RunningFile != "/home/ubuntu/.local/bin/agy" {
		t.Errorf("跑的那一份=%q", finding.RunningFile)
	}
	if finding.NextStep != "" {
		t.Errorf("量的就是跑的還給了下一步：%q", finding.NextStep)
	}
}

// ⚠⚠ 正式機隊 sampleagent2 的 agy：process 還活著，而它在跑的那個檔案已經被 unlink 了。
// 這是 /proc 講的事實，跟認不認得出那是哪一份無關——它不能因為對不上安裝路徑就被降級成
// 「說不出來」，那句話會讓一個要重啟的 process 看起來只是量不到。
func TestADeletedFileStillRunningIsItsOwnFinding(t *testing.T) {
	finding := ToolRuntimeOf(model.CLITool{
		Name: "agy", Present: true, OnPath: true,
		Path: "/home/ubuntu/.local/bin/agy", RealPath: "/home/ubuntu/.local/bin/agy",
		RunningPID: 1055101,
		RunningExe: "/home/ubuntu/.local/bin/agy.1787036247195252617.old (deleted)",
	})
	if finding.State != ToolRuntimeGoneFile {
		t.Fatalf("state=%q", finding.State)
	}
	// 那個 " (deleted)" 是 /proc 的講法，不是檔名的一部分。
	if finding.RunningFile != "/home/ubuntu/.local/bin/agy.1787036247195252617.old" {
		t.Errorf("跑的那一份=%q", finding.RunningFile)
	}
}

func TestADeletedScriptIsGoneEvenWhenItIsTheInstalledFile(t *testing.T) {
	finding := ToolRuntimeOf(model.CLITool{
		Name: "openclaw", Present: true, OnPath: true,
		Path: "/home/example-user/.local/bin/openclaw", RealPath: "/opt/openclaw/index.js",
		RunningPID: 42, RunningScript: "/opt/openclaw/index.js (deleted)",
	})
	if finding.State != ToolRuntimeGoneFile {
		t.Fatalf("state=%q", finding.State)
	}
}

// ⚠⚠ 「沒有找到在跑它的 process」跟「這一筆觀測沒有講 process 的事」是兩件事。前者是
// agent 掃過之後的答案，後者是它什麼都沒說——把後者講成前者，等於拿「我不知道」當「它
// 沒在跑」。
func TestNotFoundIsNotTheSameAsNotStated(t *testing.T) {
	idle := ToolRuntimeOf(model.CLITool{
		Name: "claude", Present: true, OnPath: true,
		Path: "/home/example-user-c/.local/bin/claude", RealPath: "/home/example-user-c/.local/bin/claude",
		RunningReason: "掃了 118 個 process，沒有一個對得上",
	})
	if idle.State != ToolRuntimeIdle {
		t.Fatalf("掃過了沒有 state=%q", idle.State)
	}
	unstated := ToolRuntimeOf(model.CLITool{
		Name: "claude", Present: true, OnPath: true,
		Path: "/home/example-user-c/.local/bin/claude", RealPath: "/home/example-user-c/.local/bin/claude",
	})
	if unstated.State != ToolRuntimeUnstated {
		t.Fatalf("什麼都沒說 state=%q", unstated.State)
	}
	if idle.NextStep == unstated.NextStep {
		t.Errorf("兩種的下一步一樣：%q", idle.NextStep)
	}
}

// ⚠⚠ agent 那一側的「找不到」其實是三種事實：讀不到 /proc、/proc 視野被限制、掃過了
// 真的沒有。Hub 只讀 typed ProcessScan，不掃理由句子：前兩種只能說沒掃完，只有 complete
// 才能說沒有找到。兩組句子都不准升級成「它沒在跑」。
func TestTheFleetNeverClaimsItIsNotRunningFromAReasonItCannotRead(t *testing.T) {
	for _, test := range []struct {
		reason string
		scan   string
		want   ToolRuntime
	}{
		{"讀不到 /proc，這台的 process 偵測整個是關的", model.ProcessScanUnavailable, ToolRuntimeUnscanned},
		{"只掃得到 3 個 process，這台的 /proc 視野被限制了", model.ProcessScanRestricted, ToolRuntimeUnscanned},
		{"掃了 118 個 process，沒有一個對得上", model.ProcessScanComplete, ToolRuntimeIdle},
	} {
		finding := ToolRuntimeOf(model.CLITool{
			Name: "claude", Present: true, RunningReason: test.reason, ProcessScan: test.scan,
		})
		if finding.State != test.want {
			t.Errorf("%q → state=%q，該是 %q", test.reason, finding.State, test.want)
		}
	}
	for _, banned := range []string{"沒在跑", "沒有在跑", "停了", "掛了", "失敗"} {
		for _, stateValue := range []ToolRuntime{ToolRuntimeIdle, ToolRuntimeUnscanned} {
			for _, sentence := range []string{
				ToolRuntimeTitle(stateValue), ToolRuntimeMeaning(stateValue),
				ToolRuntimeNextStep(stateValue),
			} {
				if sentence != "" && strings.Contains(sentence, banned) {
					t.Errorf("%q 那一句講成 %q：%q", stateValue, banned, sentence)
				}
			}
		}
	}
}

func TestProcessScanBoundsWhatTheRuntimeCanClaim(t *testing.T) {
	installed := "/home/example-user-c/.local/bin/claude"
	for name, test := range map[string]struct {
		tool model.CLITool
		want ToolRuntime
	}{
		"unavailable 沒有 PID": {
			model.CLITool{RunningReason: "讀不到 /proc", ProcessScan: model.ProcessScanUnavailable},
			ToolRuntimeUnscanned,
		},
		"restricted 沒有 PID": {
			model.CLITool{RunningReason: "視野受限", ProcessScan: model.ProcessScanRestricted},
			ToolRuntimeUnscanned,
		},
		"未來的非空值沒有 PID": {
			model.CLITool{RunningReason: "只掃了一部分", ProcessScan: "partial"},
			ToolRuntimeUnscanned,
		},
		"restricted 但已經找到 PID": {
			model.CLITool{RealPath: installed, RunningPID: 4242, RunningScript: installed,
				ProcessScan: model.ProcessScanRestricted},
			ToolRuntimeSameFile,
		},
		"舊 agent 有理由": {
			model.CLITool{RunningReason: "任何舊格式理由"},
			ToolRuntimeIdle,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ToolRuntimeOf(test.tool).State; got != test.want {
				t.Fatalf("state=%q，該是 %q", got, test.want)
			}
		})
	}
}

// ⚠ daemon 那一份不算「量的那一份」。把 daemon_path 也算進去的話，一台正在跑 daemon
// 那一份、而版號量的是人那一份的機器會顯示「量的就是跑的」——那正是要被看見的那一格。
func TestTheDaemonsCopyIsNotTheFileTheVersionCameFrom(t *testing.T) {
	finding := ToolRuntimeOf(model.CLITool{
		Name: "claude", Present: true, OnPath: true,
		Path:          "/home/example-user-b/.local/bin/claude",
		RealPath:      "/home/example-user-b/.local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe",
		DaemonReach:   model.DaemonReachShadowed,
		DaemonPath:    "/usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe",
		RunningPID:    378850,
		RunningScript: "/usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe",
	})
	if finding.State != ToolRuntimeOtherFile {
		t.Fatalf("state=%q", finding.State)
	}
}

// 路徑上多一個 "./" 或結尾多一個 "/" 不是兩個檔案。⚠ 不這樣收的話，同一個檔案會被
// 講成「這台上有兩份安裝」，而那一列會叫人去刪一個不存在的重複。
func TestTheSameFileWrittenTwoWaysIsStillOneFile(t *testing.T) {
	finding := ToolRuntimeOf(model.CLITool{
		Name: "openclaw", Present: true,
		RealPath: "/opt/openclaw/./bin/openclaw", RunningPID: 7,
		RunningScript: "/opt/openclaw/bin/openclaw",
	})
	if finding.State != ToolRuntimeSameFile {
		t.Fatalf("state=%q", finding.State)
	}
}

// 一個工具連安裝路徑都沒有（present 是從 process 來的）的時候，說得出跑哪一個檔案就
// 還是一個發現。⚠ 這一格不准變成「量的就是跑的」：沒有量過的東西不能說量的就是它。
func TestAToolWithNoInstalledPathIsNeverCalledTheSameFile(t *testing.T) {
	finding := ToolRuntimeOf(model.CLITool{
		Name: "agy", Present: true, PresentEvidence: "process",
		RunningPID: 9, RunningScript: "/opt/agy/main.js",
	})
	if finding.State != ToolRuntimeOtherFile {
		t.Fatalf("state=%q", finding.State)
	}
	if finding.MeasuredFile != "" {
		t.Errorf("量的那一份=%q", finding.MeasuredFile)
	}
}

// ⚠ 「量的那一份」講的是解開 symlink 之後那個**真的檔案**，不是 PATH 上那個名字。
// 正式機隊 sampleagent4 的 openclaw：PATH 上是 ~/.local/bin/openclaw，真的檔案在
// ~/.local/lib/node_modules 底下。印那個名字的話，一列「這台上有兩份」會講不出是哪兩
// 份——兩個指向同一個檔案的 symlink 不是兩份安裝，而要收掉的是檔案不是名字。
func TestTheFileTheVersionCameFromIsTheRealFileNotTheNameOnPath(t *testing.T) {
	finding := ToolRuntimeOf(model.CLITool{
		Name: "openclaw", Present: true, OnPath: true, PathSource: model.PathSourceLogin,
		Path:       "/home/example-user-b/.local/bin/openclaw",
		RealPath:   "/home/example-user-b/.local/lib/node_modules/openclaw/openclaw.mjs",
		RunningPID: 1849262,
		RunningScript: "/home/example-user-b/.local/share/clawctl/openclaw/releases/2026.5.26/" +
			"lib/node_modules/openclaw/dist/index.js",
	})
	if finding.MeasuredFile != "/home/example-user-b/.local/lib/node_modules/openclaw/openclaw.mjs" {
		t.Fatalf("量的那一份=%q", finding.MeasuredFile)
	}
	if finding.State != ToolRuntimeOtherFile {
		t.Fatalf("state=%q", finding.State)
	}
}

// 每一種狀態都要有它的三句話，而且只有「量的就是跑的」可以沒有下一步。
func TestEveryRuntimeStateSaysWhatItIsAndWhatToDo(t *testing.T) {
	seen := map[ToolRuntime]bool{}
	for _, stateValue := range ToolRuntimes() {
		if seen[stateValue] {
			t.Fatalf("%s 出現兩次", stateValue)
		}
		seen[stateValue] = true
		if ToolRuntimeTitle(stateValue) == "" || ToolRuntimeMeaning(stateValue) == "" {
			t.Errorf("%s 少了那兩句話", stateValue)
		}
		if ToolRuntimeNextStep(stateValue) == "" && stateValue != ToolRuntimeSameFile {
			t.Errorf("%s 沒有下一步", stateValue)
		}
	}
	if len(seen) != len(toolRuntimeSentences) {
		t.Fatalf("清單有 %d 種，句子表有 %d 種", len(seen), len(toolRuntimeSentences))
	}
	// ⚠ 要人動手的那兩種排在最前面：機隊上最多的永遠是「沒有找到在跑它的 process」，
	// 照字母排會把唯一的發現排到它後面。
	order := ToolRuntimes()
	if order[0] != ToolRuntimeOtherFile || order[1] != ToolRuntimeGoneFile {
		t.Errorf("順序=%v", order)
	}
}

// ToolRuntimes 回傳的是副本。改到它就等於改到每一個平面的狀態清單。
func TestTheRuntimeStateListCannotBeEditedByItsCaller(t *testing.T) {
	ToolRuntimes()[0] = ToolRuntime("whatever")
	if ToolRuntimes()[0] != ToolRuntimeOtherFile {
		t.Fatal("清單被呼叫端改掉了")
	}
}
