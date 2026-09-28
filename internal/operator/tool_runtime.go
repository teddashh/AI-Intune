package operator

// 「這一格的版號，講的是不是正在跑的那一份」。
//
// ⚠⚠ 正式機隊 2026-09-12 的形狀：四台在籍機器上，openclaw 各有**兩份**安裝。Hub 量
// 版號的是人 PATH 上那一份（`path_source=login`），而真正活著的 process 跑的是
// `~/.local/share/clawctl/openclaw/releases/<ver>/…` 那一份。今天兩份的版號剛好一樣，
// 所以畫面是對的——那是巧合，不是保證。任何人 `npm i -g openclaw@新版`，畫面立刻變成
// 新版，實際在跑的還是舊 release，而每機安裝狀態會說「指派的比看到的舊」，然後有人會
// 去回滾一台其實沒事的機器。
//
// ⚠⚠ 同一份量測裡，24 格 (機器, 工具) 沒有一格能說出「我量版號的那個檔案就是正在跑的
// 那個檔案」：4 格是另一個檔案或已經被刪掉的檔案，4 格說不出正在跑的是哪一個檔案，
// 16 格沒有找到在跑它的 process。這一段存在的理由就是把那件事講出來，而不是讓一個
// 版號替兩份安裝發言。
//
// ⚠ 這一段**不**回答「哪一份是 Hub 放的」。那句話需要知道 Hub 自己的安裝版面，而
// agent 現在沒有把「這個檔案在我的 release 目錄底下」講進觀測裡——由 Hub 去比對路徑
// 長相等於猜。它只回答一個比得出來的問題：量版號的那個檔案，跟正在跑的那個檔案，
// 是不是同一個。

import (
	"path"
	"strings"

	"github.com/teddashh/AI-Intune/internal/model"
)

// ToolRuntime 是「正在跑的那一份，跟我量版號的那一份，是什麼關係」。
//
// ⚠ 七種互斥且窮盡，全部是正面陳述。說不出正在跑的檔案有四種，其中 Unscanned 與
// Idle 的差別是 process 掃描本身有沒有跑完，不是回報句子的長相。「正在跑的是另一個
// 檔案」是一個發現；其餘說不出來的狀態是各自不同的觀測邊界。併成一個「對不上」的話，
// 唯一要人動手的那一種會被埋在一堆「我不知道」裡。
type ToolRuntime string

const (
	// ToolRuntimeSameFile：量版號的那個檔案就是正在跑的那個檔案。
	ToolRuntimeSameFile ToolRuntime = "same_file"
	// ToolRuntimeOtherFile：正在跑的是另一個檔案，所以這一格的版號講的是沒在跑的那一份。
	ToolRuntimeOtherFile ToolRuntime = "other_file"
	// ToolRuntimeGoneFile：正在跑的那個檔案已經從磁碟上不見了。
	ToolRuntimeGoneFile ToolRuntime = "gone_file"
	// ToolRuntimeUnattributed：有 process 在跑它，但說不出它跑的是哪一個檔案。
	ToolRuntimeUnattributed ToolRuntime = "unattributed"
	// ToolRuntimeUnscanned：這一輪的 process 偵測沒有跑到底，查不出有沒有 process 在跑它。
	ToolRuntimeUnscanned ToolRuntime = "unscanned"
	// ToolRuntimeIdle：沒有找到任何一個 process 在跑它。
	ToolRuntimeIdle ToolRuntime = "idle"
	// ToolRuntimeUnstated：這一筆觀測沒有講 process 的事。
	ToolRuntimeUnstated ToolRuntime = "unstated"
)

var toolRuntimeOrder = []ToolRuntime{
	ToolRuntimeOtherFile, ToolRuntimeGoneFile, ToolRuntimeSameFile,
	ToolRuntimeUnattributed, ToolRuntimeUnscanned, ToolRuntimeIdle, ToolRuntimeUnstated,
}

// ToolRuntimes returns every runtime state in canonical order.
//
// ⚠ 要人動手的那兩種排在最前面。一份照字母排的清單會把「正在跑的是另一個檔案」排在
// 「沒有找到在跑它的 process」後面，而後者在機隊上永遠是最多的那一種。
func ToolRuntimes() []ToolRuntime {
	return append([]ToolRuntime{}, toolRuntimeOrder...)
}

// ⚠ 每一種狀態各自帶一句「這是什麼」與一句「下一步做什麼」，每一個平面講的是同一組
// 句子。量的就是跑的那一種沒有下一步——硬編一句會讓人以為還有事要做。
var toolRuntimeSentences = map[ToolRuntime]profileSentences{
	ToolRuntimeSameFile: {
		title:   "量版號的那一份就是正在跑的那一份",
		meaning: "這台上這個工具的版號，量的就是現在活著的那個 process 在跑的檔案。",
	},
	ToolRuntimeOtherFile: {
		title:    "正在跑的是另一個檔案",
		meaning:  "這台上這個工具有兩份，而畫面上那個版號量的是沒有在跑的那一份。",
		nextStep: "決定要留哪一份，再把另一份收掉。",
	},
	ToolRuntimeGoneFile: {
		title:    "正在跑的那個檔案已經不在磁碟上",
		meaning:  "那個 process 還活著，而它在跑的檔案已經被刪掉或換掉了。",
		nextStep: "重啟它，讓它跑現在磁碟上的那一份。",
	},
	ToolRuntimeUnattributed: {
		title:    "有 process 在跑它，說不出跑的是哪一個檔案",
		meaning:  "這台上有 process 對得上這個工具，但它執行的是解譯器，不是這個工具自己的檔案。",
		nextStep: "到這台的單機頁看它回報的 process 與安裝路徑。",
	},
	ToolRuntimeUnscanned: {
		title:    "這台的 process 偵測沒有跑到底",
		meaning:  "這台這一輪沒有把 process 掃完，所以查不出有沒有 process 在跑這個工具。",
		nextStep: "先修好這台的 process 偵測，再回來看這一格。",
	},
	ToolRuntimeIdle: {
		title:    "沒有找到在跑它的 process",
		meaning:  "這台上沒有找到任何一個 process 在跑這個工具。",
		nextStep: "到這台的單機頁看它的 process 偵測回報了什麼。",
	},
	ToolRuntimeUnstated: {
		title:    "這一筆觀測沒有講 process 的事",
		meaning:  "這台最新一筆觀測既沒有說它在跑，也沒有說它為什麼找不到。",
		nextStep: "等這台下一次回報，或到單機頁看它上一次量到什麼。",
	},
}

// ToolRuntimeTitle / ToolRuntimeMeaning / ToolRuntimeNextStep are the one set of
// sentences every surface prints.
func ToolRuntimeTitle(stateValue ToolRuntime) string {
	return toolRuntimeSentences[stateValue].title
}

func ToolRuntimeMeaning(stateValue ToolRuntime) string {
	return toolRuntimeSentences[stateValue].meaning
}

func ToolRuntimeNextStep(stateValue ToolRuntime) string {
	return toolRuntimeSentences[stateValue].nextStep
}

// ⚠ 哪幾種說得出正在跑的是哪一個檔案，寫在一張表上，不是散在各處的 switch。前三種
// 說得出來，後四種說不出來——而「說不出來」的那四格留白，跟一格留白被讀成「沒有檔案
// 在跑」是兩件事。
var toolRuntimeNamesFile = map[ToolRuntime]bool{
	ToolRuntimeSameFile:  true,
	ToolRuntimeOtherFile: true,
	ToolRuntimeGoneFile:  true,
}

// ToolRuntimeNamesRunningFile says whether this state can name the running file.
func ToolRuntimeNamesRunningFile(stateValue ToolRuntime) bool {
	return toolRuntimeNamesFile[stateValue]
}

// ToolRuntimeFinding 是一格的完整答案：狀態，加上它講的是哪兩個檔案。
type ToolRuntimeFinding struct {
	State    ToolRuntime `json:"state"`
	Title    string      `json:"title"`
	Meaning  string      `json:"meaning"`
	NextStep string      `json:"next_step,omitempty"`

	// MeasuredFile 是這一格的版號從哪一個檔案量的。
	//
	// ⚠ 它必須跟著狀態一起出去。一句「正在跑的是另一個檔案」沒有講出是哪兩個檔案的
	// 時候，讀的人沒有辦法知道該去收掉哪一份。
	MeasuredFile string `json:"measured_file,omitempty"`
	// RunningFile 是那個 process 正在跑的檔案。說得出的時候才有。
	RunningFile string `json:"running_file,omitempty"`
}

// deletedFileSuffix 是 /proc 對一個已經被 unlink 的檔案的講法。
const deletedFileSuffix = " (deleted)"

// ToolRuntimeOf 回答「這一格的版號，講的是不是正在跑的那一份」。
//
// ⚠⚠ 判斷的順序有意義。`running_exe` 對 node CLI 一律是 `/usr/bin/node`，它對「跑的
// 是哪一份」一點資訊都沒有——拿它去跟安裝路徑比，會把一個解譯器講成「這台上的第二份
// 安裝」，然後有人會去找一個不存在的檔案。所以只有 `running_script`，或者 `running_exe`
// 真的對得上這個工具自己的安裝路徑，才算說出了正在跑的是哪一個檔案。
func ToolRuntimeOf(tool model.CLITool) ToolRuntimeFinding {
	finding := ToolRuntimeFinding{MeasuredFile: firstToolPath(tool.RealPath, tool.Path)}
	switch {
	// ⚠ 舊 agent 沒有 ProcessScan，仍沿用 RunningReason 判 Idle；這裡刻意不仿 state
	// 的相容格去解析理由文字。任何不認得的非空新值也保守算沒掃完。
	case tool.RunningPID == 0 && tool.ProcessScan != "" && tool.ProcessScan != model.ProcessScanComplete:
		finding.State = ToolRuntimeUnscanned
	case tool.RunningPID == 0 && tool.RunningReason != "":
		finding.State = ToolRuntimeIdle
	case tool.RunningPID == 0:
		finding.State = ToolRuntimeUnstated
	default:
		finding.State, finding.RunningFile = runningFileOf(tool)
	}
	sentences := toolRuntimeSentences[finding.State]
	finding.Title, finding.Meaning, finding.NextStep =
		sentences.title, sentences.meaning, sentences.nextStep
	return finding
}

// runningFileOf 是「這個 process 在跑哪一個檔案」那一段。
func runningFileOf(tool model.CLITool) (ToolRuntime, string) {
	// ⚠ script 優先。node CLI 的答案只在這個欄位上；exe 永遠是解譯器。
	if file, deleted, ok := toolRunningFile(tool.RunningScript); ok {
		return toolRuntimeForFile(tool, file, deleted), file
	}
	file, deleted, ok := toolRunningFile(tool.RunningExe)
	if !ok {
		return ToolRuntimeUnattributed, ""
	}
	// ⚠ 一個已經被 unlink 的檔案是 /proc 講的事實，跟它是解譯器還是這個工具本身無關：
	// 那個 process 在跑的東西，磁碟上已經沒有了。這句話不能因為認不出是哪一份就丟掉。
	if deleted {
		return ToolRuntimeGoneFile, file
	}
	// ⚠⚠ 對不上安裝路徑的 exe 不算答案，算「說不出來」。把 /usr/bin/node 講成「另一個
	// 檔案」，等於憑一個解譯器的路徑宣布這台上有兩份安裝。
	if !SameToolFile(file, tool.RealPath) && !SameToolFile(file, tool.Path) {
		return ToolRuntimeUnattributed, ""
	}
	return ToolRuntimeSameFile, file
}

func toolRuntimeForFile(tool model.CLITool, file string, deleted bool) ToolRuntime {
	if deleted {
		return ToolRuntimeGoneFile
	}
	// ⚠ 比的是「版號從哪一個檔案量的」，所以只比 realpath 與 path。daemon_path 是**另
	// 一個**檔案，把它算進來會在正在跑的是 daemon 那一份的時候說「量的就是跑的」。
	if SameToolFile(file, tool.RealPath) || SameToolFile(file, tool.Path) {
		return ToolRuntimeSameFile
	}
	return ToolRuntimeOtherFile
}

// toolRunningFile 把 /proc 給的那個路徑收成「檔案」與「它還在不在」。
func toolRunningFile(value string) (file string, deleted, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false, false
	}
	if trimmed, cut := strings.CutSuffix(value, deletedFileSuffix); cut {
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == "" {
			return "", false, false
		}
		return trimmed, true, true
	}
	return value, false, true
}

// SameToolFile says whether two reported paths name one file.
//
// ⚠ 它是匯出的，因為「同一個檔案」這件事有第二個讀者：用戶端重數的時候要判一格
// 「正在跑的是另一個檔案」是不是真的講了兩個檔案。兩邊各寫一次比法，其中一邊哪天
// 少清一次 `..`，就會有一格說「把另一份收掉」而那兩個路徑其實是同一份。
func SameToolFile(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	return path.Clean(left) == path.Clean(right)
}

func firstToolPath(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ToolRuntimeCount 是七種「版號講的是哪一份」各有幾格。
//
// ⚠ 軟體清查與每機安裝狀態共用這一個型別，不各自寫一份。兩頁的分母不一樣，但這一軸
// 的狀態集合是同一個——各寫一份的話，有一天其中一頁會多出一種狀態，而讀的人會以為那
// 兩個數字算的是同一件事。
type ToolRuntimeCount struct {
	State    ToolRuntime `json:"state"`
	Title    string      `json:"title"`
	Count    int         `json:"count"`
	Meaning  string      `json:"meaning"`
	NextStep string      `json:"next_step,omitempty"`
}

// ToolRuntimeMisattributed says whether this cell's version was measured on a
// file that is not the one running.
//
// ⚠ 只有那兩種算。「說不出來」與「沒有找到在跑它的 process」不算——把「我不知道」
// 算成一個發現，機隊上每一格都會變成待辦事項，然後沒有人再看這個數字。
func ToolRuntimeMisattributed(stateValue ToolRuntime) bool {
	return stateValue == ToolRuntimeOtherFile || stateValue == ToolRuntimeGoneFile
}

// toolRuntimeCSV 讓沒有這一軸的那幾格留白。⚠ 留白跟「沒有找到在跑它的 process」是
// 兩件事：前者是「這台上沒有這個工具，所以沒有版號可問」。
func toolRuntimeCSV(finding *ToolRuntimeFinding, pick func(ToolRuntimeFinding) string) string {
	if finding == nil {
		return ""
	}
	return pick(*finding)
}
