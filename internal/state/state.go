// Package state 是 Hub 的判決層 —— 把 Agent 回報的「事實」變成「狀態」。
//
// 這個 package 是整個產品真正的內容。其他都是管線。
//
// 三條規則：
//
//  1. 判決只在這裡發生。Agent 不判、資料庫不判、UI 不判。
//  2. 每一個判決都必須附一句人看得懂的理由。UI 上不准出現沒有句子的燈。
//  3. 拿不到資料的答案是 Unknown，不是 OK。
//
// 見 docs/SPEC.md §3。
package state

import (
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------- 時間門檻
//
// 這些數字都是有理由的，改之前先讀理由。

const (
	// CheckinInterval：心跳每 2 分鐘。
	CheckinInterval = 2 * time.Minute

	// UnreachableGrace：超過「下次應到 + 90s」才算失聯。
	//
	// ⚠ 心跳間隔 = 失聯門檻會製造假紅。5 分鐘心跳配 5 分鐘門檻，
	// 任何一次網路抖動都會亮紅燈，然後第一週人就把通知關掉了。
	UnreachableGrace = 90 * time.Second

	// ObservationInterval：完整觀測每 10 分鐘。比心跳慢是故意的 ——
	// 心跳要小要快，觀測失敗不該讓機器看起來像死了。
	ObservationInterval = 10 * time.Minute

	// ObservationStale：本機狀態超過這麼久沒更新 = Agent 在線但沒在觀測。
	//
	// ⚠⚠ 這個數字原本是 10 分鐘，**跟 ObservationInterval 一模一樣** ——
	// 也就是上面 UnreachableGrace 那段註解正在警告的那件事，寫下來三行之後
	// 就在同一個 const 區塊裡犯了一次。規則被寫進文件，然後被隔壁的常數違反。
	//
	// 一個等於更新週期的過期門檻是一個**保證會震盪的設計**：每一份觀測都會在
	// 下一份到達之前的那一瞬間剛好過期。2026-09-04 的實測（觀測間隔的分布，
	// 一天多的真實資料）：
	//
	//   機器      中位    p90      最大    超過 10 分的比例
	//   samplehub1  8.74m  11.27m  12.05m   32.5%
	//   sampleagent2  8.37m  11.27m  12.17m   27.4%
	//   sampleagent3  8.54m  11.42m  11.98m   30.5%
	//   sampleagent4   9.32m  11.50m  16.25m   38.2%
	//
	// **每一台有三成的觀測週期會踩線**，不是只有某一台壞掉。samplehub1 那天
	// 因此在 Degraded / Online 之間跳了 76 次，每次降級只持續 30～90 秒。
	// 那正是 PHASES「黃燈太多，通知被靜音」講的東西，也是 §5.1 的同一個形狀。
	//
	// 改成兩倍週期而不是「週期 + 90 秒」：90 秒的餘裕在 p90（11.3 分）就會
	// 再次踩線，等於換一個數字繼續震盪。兩倍週期高於實測的最大值（16.25 分），
	// 而且它講得出一句人聽得懂的話 —— **「你整整漏掉了一個週期」**。
	//
	// 代價是偵測慢 10 分鐘。可以接受：存活由心跳負責（2 分 + 90 秒），
	// 這一條回答的是「還活著，但停止觀測了」，那不是一個要用秒計的問題。
	ObservationStale = 2 * ObservationInterval

	// SilentFailure：這麼久沒有任何任務跑完 = 沉默失敗。
	//
	// ⚠ 這是 L1，只代表「沒跑完東西」，不代表「跑的東西是對的」。
	SilentFailure = 8 * time.Hour

	// ClockSkewTolerance：超過這個差距就在 UI 上標「時鐘不可信」。
	// 生死判斷本來就只用 Hub 的 received_at，所以時鐘歪掉不影響存活判定，
	// 但會讓「最後成功時間」這種 Agent 報的時間變得不能信。
	ClockSkewTolerance = 120 * time.Second

	// CrashLoopWindow / CrashLoopBootIDs：一小時內機器開機 ID 換 10 次 = 機器一直在重開。
	//
	// ⚠ 這是為了擋掉「crash-loop 但偶發心跳成功」讓 Hub 以為 Online 的情況。
	CrashLoopWindow  = time.Hour
	CrashLoopBootIDs = 10

	// CrashLoopRestarts：一小時內 agent process 重啟幾次算 crash-loop。
	//
	// ⚠ 門檻壓得比 CrashLoopBootIDs 低很多，因為這兩個數字的意義天差地遠：
	// 機器重開 3 次可能只是你在裝東西；agent process 一小時重啟 3 次，
	// 表示它連一次 10 分鐘的觀測週期都活不完，送上來的東西必然是殘的。
	//
	// 2026-09-03 實測：WatchdogSec 餵不到，每 90 秒被 SIGABRT 一次 = 一小時 40 次。
	CrashLoopRestarts = 3

	// DiskLow：低於這個值，寫入開始會失敗。
	DiskLowBytes = 512 << 20 // 512 MiB
	// DiskHighPct：用量超過這個百分比就該講話了。
	// 實測 sampleagent1 在 97% 且它自己的 agent 連續喊了三次都沒人看到。
	DiskWarnPct = 85
	DiskCritPct = 93

	// ⚠ 這裡沒有 CredExpirySoon 這種門檻，而且是刻意沒有。
	//
	// 曾經有過（7 天），寫的時候覺得很合理，一接上真機就發現它讓全機隊永遠
	// 亮黃燈：claude 的 access token 名目壽命 8 小時、背景自動續，實測剩餘
	// 2.3 小時 —— 任何「剩餘 < N 天」的規則對它永遠成立。而永遠亮的黃燈
	// 三天內就會被人靜音，靜音之後這個產品就死了。
	//
	// 所以 Derive() 只認 Agent 給的 Status，自己不從 expires_at 算任何東西
	// （見 3e）。而 Agent 那邊只會給 expired / configured / unknown
	// （見 probe.classifyCred）。expires_soon 這個值目前沒有任何生產者 ——
	// switch 裡留著那一支是為了以後真的出現長命憑證（例如 90 天的 API key）
	// 的那天，不是留給誰去接一條門檻規則。
	//
	// 真正該偵測的訊號不是「快到期」，是「這張票不再自動續了」——
	// 那要看 last_refresh 有沒有卡住，是跨時間的比對，不是一個常數。
	// 那條規則屬於 Hub 的歷史分析（Phase 2），不屬於這裡。
)

// ---------------------------------------------------------------- 狀態

type State string

const (
	// NeverReported：在名冊上（expected）但從來沒有成功 check-in 過。
	//
	// ⚠ 這個狀態存在的理由就是「名冊是分母」。一台從沒報到的機器不會從畫面上
	// 消失，它佔一格紅 —— 否則你永遠不會發現你以為納管了但其實沒有。
	// 實測案例：sampleagent3 在某台 client 上隱形了七週。
	//
	// ⚠⚠ 這個狀態原本叫 `Dead`，2026-09-05 改名。改名的理由不是措辭潔癖：
	//
	//   `Dead` 斷言的是**那台機器**。但這個判定唯一的輸入是 `!EverCheckedIn` ——
	//   一件關於 **clawctl 自己**的事實（我沒收到過東西）。其他每一個狀態
	//   （Online / Degraded / Unreachable）都是從機器**真的送來的資料**推出來的；
	//   只有這一個是從**資料的缺席**推出來的，而缺席不是死亡的證據。
	//
	//   實例：sampleagent1 從來沒 check-in 過，所以它一直掛著 `Dead`。而它活得很好 ——
	//   tailscale 說 Online、direct handshake 18ms、TCP/22 交握得起來、
	//   samplehub1 上的 sampleagent1-management 每 30 分鐘寫一次 heartbeat OK，
	//   至今 1,485 次。它只是**刻意**沒有裝 clawctl-agent
	//   （MACHINE_LOG 自己寫 `openclaw=inactive(expected)`）。
	//
	//   一台從來不打算裝 agent 的機器，在舊定義下永遠是「Dead」。
	//   那不是量錯，是這個字宣稱了它不知道的事。
	//
	// ⚠ 嚴重度**維持 4**（跟舊的 Dead 一樣高）。改名不是降級 ——
	//   「我對這台一無所知」仍然是最該處理的事情之一。改掉的只是
	//   「我知道它死了」這個假宣稱。
	NeverReported State = "NeverReported"

	// Online：最近一次心跳在門檻內，而且沒有任何降級條件。
	Online State = "Online"

	// Degraded：在線，但有東西不對。⚠ 這不是「有點小問題」，
	// 這是「它還在跟你講話，但它講的內容不對」，通常比 Unreachable 更需要看。
	Degraded State = "Degraded"

	// Unreachable：曾經在線，現在超過「下次應到 + 90s」沒消息。
	Unreachable State = "Unreachable"

	// IdentityConflict：兩台機器帶著同一個身分報到（通常是複製了資料目錄）。
	// ⚠ 兩列都留著，都標衝突。不合併 —— 否則狀態會在兩台真實機器之間跳動。
	IdentityConflict State = "IdentityConflict"
)

// AllStates 是上面那五個。
//
// ⚠⚠ **加新狀態一定要同時加到這裡。** 這個 slice 存在的理由是讓
// 「要窮舉每一個狀態」的地方（目前是 /metrics）不必自己抄一份清單 ——
// 自己抄一份的話，某天多一個狀態時那份清單不會報錯，它會**安靜地少一項**。
// 而一個少了一項的窮舉，在 Prometheus 上的樣子是那條線從來沒出現過，
// 「從來沒出現」跟「值是 0」在圖上長得一模一樣。
var AllStates = []State{Online, Degraded, Unreachable, NeverReported, IdentityConflict}

// Severity 用來排序：數字越大越該先看。
func (s State) Severity() int {
	switch s {
	case IdentityConflict:
		return 5
	case NeverReported:
		return 4
	case Unreachable:
		return 3
	case Degraded:
		return 2
	case Online:
		return 1
	}
	return 0
}

func (s State) Color() string {
	switch s {
	case Online:
		return "green"
	case Degraded:
		return "amber"
	case Unreachable, NeverReported, IdentityConflict:
		return "red"
	}
	return "grey"
}

// ---------------------------------------------------------------- 輸入

// Facts 是判決的全部輸入。⚠ 這裡面每一個欄位都是 Agent 回報的事實或
// Hub 自己記的時間，沒有任何一個是別人已經下好的判斷。
// ArtifactFact 是「一條人宣告的期望」加上「這台機器對它的最新量測」。
//
// ⚠⚠ Declared 與 Checked 是兩件事，混在一起會生出這個專案最怕的那種假綠燈：
//   - Declared=true, Checked=false → 宣告了，但**還沒量到**（agent 是舊版、
//     或它還沒跑過一輪觀測）。這**不是**通過。
//   - Checked=true               → 真的量到了，可以判。
type ArtifactFact struct {
	Unit     string
	Artifact string
	Why      string
	MaxAge   time.Duration

	// ObservationObserved distinguishes no report from a retained row whose
	// payload cannot be decoded. MeasuredAt is agent-declared; ReceivedAt is
	// the Hub admission clock and the only trusted liveness coordinate.
	ObservationObserved bool
	ObservationDecoded  bool
	ObservationInvalid  bool
	MeasuredAt          time.Time
	ReceivedAt          time.Time

	Checked bool
	Exists  bool
	ModTime *time.Time
	Err     string

	// Events 非 nil = 這條規則同時把產出物當事件流讀。
	Events *EventFact
}

// EventFact 是一條事件流的宣告 + 量測。
//
// ⚠⚠ 這是這個專案裡最靠近「掃 log 關鍵字」那條紅線的東西，界線只有一條：
// **NotOK 是人宣告的，不是我們猜的**，而且它讀的是列舉欄位不是散文。
// 真正把地基二補起來的是 MaxAge —— 見上面 ArtifactFact 的新鮮度判決。
// 兩者缺一，這裡就退回成一張關鍵字表。
type EventFact struct {
	Window   time.Duration
	Measured bool
	// FailureTypes are the operator-declared enum values that affect the
	// judgement. They remain distinct from Declared, which is the agent's
	// measured count page and is empty until an observation arrives.
	FailureTypes []string

	ObservationObserved bool
	ObservationDecoded  bool
	ObservationInvalid  bool
	MeasuredAt          time.Time
	ReceivedAt          time.Time

	// Declared 是宣告過的事件名 + 在窗口裡的次數（沒出現的 Count=0 也在）。
	Declared []EventCount

	// Undeclared 是沒人宣告過的事件名。⚠ 它是 advisory 的來源：
	// 一種新長出來的失敗事件，在只看宣告名單的世界裡是完全隱形的。
	Undeclared      []EventCount
	UndeclaredTotal int

	// Partial = 這次沒讀完整個窗口（撞到 tail 上限），Count 是下界不是總數。
	Partial     bool
	CoveredFrom *time.Time
	Malformed   int
	Err         string
}

// EventCount 是一種事件在窗口裡出現幾次、最後一次是什麼時候。
// ⚠ 這裡刻意不直接用 model.EventSummary —— internal/state 不 import model，
// 判決層看到的東西只能是事實，不能是線上格式的形狀。
type EventCount struct {
	Type   string
	Count  int
	LastAt *time.Time
}

type Facts struct {
	Now time.Time

	// Artifacts 是這台機器上所有宣告過的期望 + 量測。
	// ⚠ 空的代表**沒有人宣告過**，不代表「都通過了」。
	// 畫面必須把這兩者分開講 —— 見 machine.html。
	Artifacts []ArtifactFact

	Retired  bool
	Conflict bool // 身分衝突（同一 machine_id 被兩個不同的 machine_id_hint 用）

	// EverCheckedIn / LastCheckinReceived：⚠ 用 Hub 收到的時間，不是 Agent 說的。
	EverCheckedIn       bool
	LastCheckinReceived time.Time

	// AgentVersion 是**最後一次心跳裡 agent 自報的版本字串**，不是 Hub
	// 期望它是什麼。空字串的意思是「這台沒報到過」或「舊版 agent 不送這欄」——
	// 兩個都是「不知道」，不是「沒換版」。
	//
	// ⚠ 這一條刻意不參與任何判決。它存在的唯一理由是讓「推出去的東西
	// 到底有沒有落地」可以被**第三方**回答：Ansible 說 changed 不算數，
	// 機器自己在心跳裡說它現在是哪一版才算。自證不算數那條的另一面 ——
	// 這裡自證的是 Ansible，不是機器。
	AgentVersion    string
	CheckinInterval time.Duration // Hub 指派給這台的間隔

	ClockSkew time.Duration

	// BootIDChanges1h：機器重開機的次數（/proc/…/boot_id 換了幾個值）。
	// AgentRestarts1h：agent process 重啟的次數（agent_started_at 換了幾個值）。
	//
	// ⚠ 這兩個不是同一件事，而 crash-loop 只有後者看得到。
	// 2026-09-03 全機隊每 90 秒被 SIGABRT 一次時，機器已經連續開機 80 天，
	// BootIDChanges1h 是 0。拿機器的開機 ID 去抓 process 的崩潰迴圈，
	// 那條規則一輩子不會觸發 —— 而且它會綠得非常有說服力。
	BootIDChanges1h int
	AgentRestarts1h int
	// BootIDSignalObserved / AgentRestartSignalObserved keep an observed zero
	// distinct from legacy check-ins that did not send the corresponding
	// identity. The counts alone cannot make that distinction.
	BootIDSignalObserved       bool
	AgentRestartSignalObserved bool

	// AgentNRestarts 是 systemd 對 clawctl-agent.service 的重啟計數。
	//
	// ⚠ 這一個欄位把「crash-loop」跟「有人在部署」分開，而那兩件事
	// 在 AgentRestarts1h 眼中一模一樣（都是「process 重啟了很多次」）。
	//
	// 實測（samplehub1，2026-09-03）：
	//   讓一個 unit 一直崩 → NRestarts 12 秒內爬到 3
	//   `systemctl restart`  → NRestarts 立刻歸零
	//   再讓它崩             → 又從 0 往上爬
	//
	// 所以「重啟很多次 + NRestarts 是 0」= 有人在按，不是它自己在死。
	// ⚠ 手動重啟後的那一瞬間，一個真的 crash-loop 也會是 0 ——
	// 但它會在下一個 90 秒內把數字爬回來，而心跳每 2 分鐘就來一次。
	AgentNRestarts int

	// AgentUnitSeen：有沒有真的收到 clawctl-agent.service 這個 unit 的觀測。
	// ⚠ 沒收到的時候 AgentNRestarts 會是 0，而那個 0 的意思是「不知道」，
	// 不是「systemd 沒重啟過它」。舊版 agent 不觀測自己的 unit。
	AgentUnitSeen bool

	LingerEnabled *bool

	DiskFreeBytes  int64
	DiskTotalBytes int64

	// LastObservation：完整觀測最後一次收到的時間。
	LastObservation time.Time
	HasObservation  bool

	// OpenClawObserved：有沒有真的收到這台的 OpenClaw 觀測列。
	// ⚠ 沒收到的時候 OpenClawPresent 會是 false，而那個 false 的意思是
	// 「還沒量到」，不是「量到沒裝」。已 check-in 但觀測還沒送到的機器
	// 就是這個形狀。
	OpenClawObserved bool

	// OpenClawPresent / LastTaskEnded：L1 存活訊號。
	//
	// ⚠ LastTaskEnded 只回答「最後一次跑完是什麼時候」。它不回答
	// 「跑的東西對不對」—— 實測 64% 的 status='ok' 其 summary 在描述失敗。
	// 所以下面的判決永遠不會因為這個欄位就說一台機器「健康」。
	OpenClawPresent bool
	OpenClawRunning bool

	// OpenClawRunReason：沒偵測到 process 時，agent 說的原因原文。
	//
	// ⚠ 它決定「沒偵測到」能不能拿來當判決。agent 讀不到 /proc 的時候，
	// RunningRunning 一樣是 false，但那時候我們知道的是「我不知道」，
	// 不是「它不在」。拿第一種去下第二種的判決，就是這個專案的地基一
	// （自證不算數）反過來犯 —— 用一個關於自己的事實去斷言世界。
	OpenClawRunReason string

	// OpenClawProcessScan：那一輪 /proc 掃描本身的結果。
	//
	// ⚠ 它才是「RunningPID == 0 能不能拿來當『它不在』」的判準。
	// 值域是 model.ProcessScan*（"complete"／"restricted"／"unavailable"），
	// 空字串＝還沒送這一欄的舊 agent。
	// ⚠ 這裡刻意用純字串跟字面值比對，跟同檔 CredFact.Status（:389、:449）同一個作法：
	// internal/state 不 import internal/model。那是**封閉值域**的比對，
	// 跟在自由文字裡找關鍵字是兩回事。
	OpenClawProcessScan string

	// OpenClawDBReason：狀態資料庫找得到、卻讀不動時，agent 說的原因原文。
	//
	// ⚠ 它決定「沒有 task signal」能不能拿來當「從來沒跑完過」的判決。
	// 這跟上面 OpenClawRunReason 是同一條規則，只是換到 L1 的另一半：
	// 讀不動的時候 HasTaskSignal 一樣是 false，但我們知道的是「我不知道」。
	// ⚠ 只有在任務表本身沒被讀到時才會有值 —— 見 store.Facts 的填欄條件。
	OpenClawDBReason string
	HasTaskSignal    bool
	LastTaskEnded    time.Time

	// Credentials：每個 provider 的狀態與過期時間。
	Credentials []CredFact
}

type CredFact struct {
	Provider  string
	Status    string
	ExpiresAt *time.Time
	Note      string

	// 下面四個欄位是 SPEC §4.3 那句話的材料：
	// 「真正該偵測的訊號不是『快到期』，是『這張票不再自動續了』」。
	//
	// Agent 只看得到當下一張快照，分不出「閒置了 9 小時」跟「續期壞了」——
	// 兩者的 auth.json 長得一模一樣。分得出來的只有 Hub，因為它有歷史。
	//
	// FileMTime：憑證檔最後一次被寫的時間（三元組的一隻腳）。
	// Lifetime：expires_at − file_mtime，這種票的**名目壽命**。
	//   實測：claude 8 小時、grok 6 小時、codex 10 天、gemini 1 小時。
	//   0 代表算不出來（沒有 mtime 或沒有到期時間）。
	// RefreshesSeen：我們看著它的這段期間，檔案的 mtime 換了幾次 ——
	//   也就是它**自己續了幾次**。0 不代表壞了，可能只是我們才剛開始看。
	// WatchedFor：我們看了它多久（第一筆觀測到現在）。RefreshesSeen 一定要
	//   跟它一起讀：「3 天續了 8 次」跟「1 小時續了 0 次」講的是完全不同的事。
	FileMTime     *time.Time
	Lifetime      time.Duration
	RefreshesSeen int
	WatchedFor    time.Duration

	// Peers：其他未退役機器上，同一家供應商的登入在 Hub 看著的期間
	// 自己續期的紀錄。票的名冊還沒建，所以這只能排除「整家供應商都沒在續」，
	// 不能證明兩台用的是同一張票。
	Peers []CredPeer
}

type CredPeer struct {
	DisplayName   string
	RefreshesSeen int
	FileMTime     *time.Time
}

// CredGraceCap：一張過期的票，最多給它多久「等它自己續回來」。
//
// 短命的票（claude 8h、grok 6h）在機器閒置時**一定會**過期，然後在下一次
// 使用時自己續 —— 實測 samplehub1 的 claude 三天內續了 8 次，每次都在到期前
// 幾分鐘。對這種票，「過期」是它正常生命週期的一部分，不是故障。
// 2026-09-05 的告警紀錄：CredentialExpired 在 samplehub1/claude、samplehub1/grok、
// sampleagent2/claude 上一天各燒了兩輪、每輪一小時，然後自己熄掉。那正是
// §4.3 刪掉 expires_soon 的理由的鏡像 —— 一個會自己好的紅燈，三天內就會
// 被人靜音，靜音之後這個產品就沒有存在意義了。
//
// 所以寬限 = 一個壽命：過期不到一個壽命，是「還沒被用到」；過了一個壽命
// 還沒續，是「續不回來」。但壽命長的票（codex 10 天）不能給 10 天寬限 ——
// 一張過期了兩天沒人碰的票，已經值得看一眼。上限一天。
//
// ⚠ 算不出壽命（Lifetime == 0）的票沒有寬限。不知道它怎麼續，就不假設它會續。
const CredGraceCap = 24 * time.Hour

// CredGrace 回答「這張過期的票，還在等它自己續回來的窗口裡嗎」。
//
// 回傳寬限的長度、已經過期多久、以及是否還在窗口內。
// 非 expired、或沒有到期時間的票一律回 false —— 這個函式只對「已過期」有意義。
//
// ⚠ 判決（Derive）跟早報（cmd/clawctl-hub buildReport）都要用這一支。
// 兩邊各寫一次同一條規則，就是兩個各自會忘記的地方（PHASE1 §5.13 那一課）。
func CredGrace(c CredFact, now time.Time) (grace, overdue time.Duration, inGrace bool) {
	if c.Status != "expired" || c.ExpiresAt == nil {
		return 0, 0, false
	}
	overdue = now.Sub(*c.ExpiresAt)
	grace = c.Lifetime
	if grace > CredGraceCap {
		grace = CredGraceCap
	}
	if grace <= 0 {
		return 0, overdue, false
	}
	return grace, overdue, overdue < grace
}

// MostActiveCredPeer 挑續期次數最多的一台；平手時挑最近還在續的，再以名字
// 固定順序。這只決定文案點名誰，不增加或降低任何狀態。
// 匯出是因為早報那一行也要點名同一台 —— 兩邊各挑一次就會有一天挑到不同的人。
func MostActiveCredPeer(peers []CredPeer) (best CredPeer, active int, found bool) {
	for _, peer := range peers {
		if peer.RefreshesSeen <= 0 {
			continue
		}
		active++
		if !found || peer.RefreshesSeen > best.RefreshesSeen ||
			(peer.RefreshesSeen == best.RefreshesSeen && credPeerMoreRecent(peer, best)) {
			best = peer
			found = true
		}
	}
	return best, active, found
}

func credPeerMoreRecent(a, b CredPeer) bool {
	switch {
	case a.FileMTime != nil && b.FileMTime == nil:
		return true
	case a.FileMTime == nil && b.FileMTime != nil:
		return false
	case a.FileMTime != nil && b.FileMTime != nil && !a.FileMTime.Equal(*b.FileMTime):
		return a.FileMTime.After(*b.FileMTime)
	default:
		return a.DisplayName < b.DisplayName
	}
}

// ---------------------------------------------------------------- 判決

type Judgement struct {
	State State
	// Reason 是主要理由，一句話，人看得懂。UI 直接顯示這個。
	Reason string
	// Findings 是所有發現，包含沒有升級成主狀態的那些。
	Findings []Finding
}

type Finding struct {
	Kind     string // reachability | disk | workload | credential | agent | clock | config
	Severity int    // 1=info 2=warn 3=crit 4=agent crash-loop
	Message  string
	// Advisory 為 true 表示這條不影響主狀態，只是要讓人知道。
	Advisory bool
}

type workloadEvaluation struct {
	findings     []Finding
	degradations []Finding
}

// WorkloadFindings 只用一批完整觀測能帶出的 workload 事實，下跟 Derive
// 完全相同的 finding。Store 在 observation 與 raw rows 同一筆交易裡把這些
// finding 投影成 promote 證據；規則仍只存在這裡，不在持久層抄第二份。
func WorkloadFindings(f Facts) []Finding {
	return evaluateWorkload(f).findings
}

func evaluateWorkload(f Facts) workloadEvaluation {
	var out workloadEvaluation
	add := func(kind string, sev int, advisory bool, format string, args ...any) {
		out.findings = append(out.findings, Finding{
			Kind: kind, Severity: sev, Advisory: advisory,
			Message: fmt.Sprintf(format, args...),
		})
	}
	promote := func(sev int, msg string) {
		out.degradations = append(out.degradations, Finding{
			Kind: "workload", Severity: sev, Message: msg,
		})
	}

	// L1 沉默失敗。
	switch {
	case !f.OpenClawObserved:
		add("workload", 1, true, "還沒收到這台的 OpenClaw 觀測")
	case !f.OpenClawPresent:
		add("workload", 1, true, "這台沒有裝 OpenClaw")
	case !f.OpenClawRunning && (f.OpenClawProcessScan == "unavailable" || f.OpenClawProcessScan == "restricted"):
		// agent 自己說這一輪的 process 掃描沒有跑到底。那是「我不知道」，不是「它不在」。
		add("workload", 2, false, "OpenClaw 的 process 偵測是關的：%s", f.OpenClawRunReason)
	case !f.OpenClawRunning && f.OpenClawProcessScan != "" && f.OpenClawProcessScan != "complete":
		// 送來一個這裡讀不出來的掃描狀態。那是「我不知道」，不是「它不在」。
		add("workload", 2, false, "OpenClaw 的 process 偵測沒有回報這一輪的結果：把這台的 agent 升到跟 Hub 同版")
	case !f.OpenClawRunning && f.OpenClawProcessScan == "" && strings.Contains(f.OpenClawRunReason, "/proc"):
		// ⚠ 只給還沒送 process_scan 的舊 agent payload。
		// 掃關鍵字是這一刀要拆掉的東西；機隊升級完就刪掉這一格。
		// 留著是因為刪掉會讓全機隊在升級完成之前失去唯一的 workload 警報。
		add("workload", 2, false, "OpenClaw 的 process 偵測是關的：%s", f.OpenClawRunReason)
	case !f.OpenClawRunning:
		// 節點剛剛才回報過，機器活著；死掉的是它上面的 workload。
		m := "節點剛回報，但 OpenClaw process 不存在"
		add("workload", 3, false, "%s", m)
		promote(3, m)
	case !f.HasTaskSignal && f.OpenClawDBReason != "":
		// agent 找得到狀態資料庫卻讀不到內容。那是「我不知道」，不是「它從來沒跑完過」。
		// ⚠ 仍然 promote：不知道不可以變成沒問題，這台機器維持 Degraded。
		m := fmt.Sprintf("OpenClaw 的狀態資料庫讀不到內容：%s", f.OpenClawDBReason)
		add("workload", 3, false, "%s", m)
		promote(3, m)
	case !f.HasTaskSignal:
		m := "裝了 OpenClaw，但找不到任何任務執行紀錄 —— 它從來沒跑完過任何東西"
		add("workload", 3, false, "%s", m)
		promote(3, m)
	default:
		idle := f.Now.Sub(f.LastTaskEnded)
		if idle > SilentFailure {
			m := fmt.Sprintf("已經 %s沒有任何任務跑完（process 還在）", humanDur(idle))
			add("workload", 3, false, "%s", m)
			promote(3, m)
		} else if idle < 0 {
			// 這裡比較的是 agent 與 Hub 兩把鐘；humanDur 只負責時間長度，
			// 方向必須由呼叫端決定。
			add("workload", 1, true, "最近一次跑完的時間比現在晚 %s", humanDur(idle))
		} else {
			add("workload", 1, true, "最近一次跑完是 %s前", humanDur(idle))
		}
	}

	// 人宣告的產出物：獨立驗證器的新鮮度與結構化事件。
	for _, a := range f.Artifacts {
		label := a.Unit
		if label == "" {
			label = a.Artifact
		}
		switch {
		case !a.Checked:
			add("workload", 1, true,
				"%s 的產出物 %s 尚無量測",
				label, a.Artifact)
		case a.Err != "":
			m := fmt.Sprintf("%s 的產出物 %s 看不到：%s", label, a.Artifact, a.Err)
			add("workload", 2, false, "%s", m)
			promote(2, m)
		case !a.Exists:
			m := fmt.Sprintf("%s 應該產出 %s，而那個檔案不存在 —— %s",
				label, a.Artifact, a.Why)
			add("workload", 3, false, "%s", m)
			promote(3, m)
		case a.ModTime == nil:
			add("workload", 2, false,
				"%s 的產出物 %s 存在，但拿不到它的時間", label, a.Artifact)
		default:
			// 用 Hub 的時鐘算年齡，不用機器自己的。
			age := f.Now.Sub(*a.ModTime)
			if age > a.MaxAge {
				m := fmt.Sprintf("%s 的產出物 %s 已經 %s沒更新（期望 %s 內）—— %s",
					label, a.Artifact, humanDur(age), humanDur(a.MaxAge), a.Why)
				add("workload", 3, false, "%s", m)
				promote(3, m)
			} else if age < 0 {
				// a.ModTime 是那台機器上的檔案 mtime，f.Now 是 Hub；
				// humanDur 只講長度，方向必須在這裡決定。
				add("workload", 1, true,
					"%s 的產出物 %s 的更新時間比現在晚 %s",
					label, a.Artifact, humanDur(age))
			} else {
				add("workload", 1, true,
					"%s 的產出物 %s 在 %s前更新",
					label, a.Artifact, humanDur(age))
			}
		}
		if a.Events != nil {
			judgeEvents(label, a, add, promote)
		}
	}
	return out
}

// Derive 把事實變成判決。
//
// 順序是有意義的：先決定機器還在不在（可達性），再決定它在做什麼（工作負載），
// 最後才是那些「知道了比較好」的東西。因為一台失聯的機器，它的磁碟用量
// 是幾天前的舊資料，講它沒有意義。
func Derive(f Facts) Judgement {
	var found []Finding
	add := func(kind string, sev int, advisory bool, format string, args ...any) {
		found = append(found, Finding{
			Kind: kind, Severity: sev, Advisory: advisory,
			Message: fmt.Sprintf(format, args...),
		})
	}

	// --- 0. 退場與衝突：這兩個蓋過一切
	if f.Conflict {
		add("agent", 3, false, "多台機器使用同一個身分")
		return Judgement{State: IdentityConflict,
			Reason:   "身分衝突：同一個 machine_id 被一台以上的機器使用，兩列都保留",
			Findings: found}
	}

	// --- 1. 從來沒報到過 = NeverReported。名冊是分母。
	if !f.EverCheckedIn {
		add("reachability", 3, false, "在名冊上但從來沒有成功 check-in 過")
		return Judgement{State: NeverReported,
			Reason:   "從未報到；已納入管理分母",
			Findings: found}
	}

	// --- 2. 失聯：下次應到 + 90s
	silence := f.Now.Sub(f.LastCheckinReceived)

	if f.Now.After(CheckinDeadline(f)) {
		reason := fmt.Sprintf("失聯 %s（最後一次回報 %s）",
			humanDur(silence), f.LastCheckinReceived.Format("15:04"))
		// ⚠ 沒開 linger 的機器會在使用者登出時假離線。必須分開講，
		// 否則人會跑去查一台其實好好的機器。
		if f.LingerEnabled != nil && !*f.LingerEnabled {
			reason += "；linger 未啟用，登出會停止 agent"
			add("config", 2, true, "未啟用 linger：使用者登出時 systemd --user unit 會被停掉")
		}
		add("reachability", 3, false, "%s", reason)
		return Judgement{State: Unreachable, Reason: reason, Findings: found}
	}

	// --- 3. 以下都是「在線但有問題」。收集所有降級理由，最嚴重的當主因。
	degraded := false
	var primary string
	worst := 0
	promote := func(sev int, msg string) {
		degraded = true
		if sev >= worst {
			worst, primary = sev, msg
		}
	}

	// 3a. Agent process 一直在崩。
	//
	// ⚠ 這一條的嚴重度要壓過「觀測過期」（3b）。一台每 90 秒重啟一次的機器
	// 當然觀測過期 —— 但「觀測過期」會叫人去看觀測，而真正該看的是 systemd。
	if f.AgentRestarts1h >= CrashLoopRestarts {
		switch {
		case f.AgentUnitSeen && f.AgentNRestarts == 0:
			// ⚠ 重啟很多次，但 systemd 一次都沒有「替它重新拉起來」——
			// 那是有人在按 restart，不是它自己在死。
			// 把部署講成 crash-loop，第二次部署之後就沒有人會再看這條告警了。
			add("agent", 1, true,
				"Agent 一小時內重啟 %d 次；systemd restart count=0", f.AgentRestarts1h)
		default:
			m := fmt.Sprintf("Agent 一小時內重啟 %d 次；crash-loop", f.AgentRestarts1h)
			if f.AgentUnitSeen {
				m += fmt.Sprintf("（systemd 已經替它重新拉起來 %d 次）", f.AgentNRestarts)
			}
			add("agent", 4, false, "%s", m)
			promote(4, m)
		}
	}

	// 3a'. 機器本身一直在重開（跟上面那條是兩件事）。
	if f.BootIDChanges1h >= CrashLoopBootIDs {
		m := fmt.Sprintf("這台機器一小時內重開機 %d 次", f.BootIDChanges1h)
		add("machine", 3, false, "%s", m)
		promote(3, m)
	}

	// 3b. 觀測過期：心跳在但完整觀測沒進來
	if isObservationStale(f) {
		m := fmt.Sprintf("Agent 在線，但狀態資料已經 %s沒更新",
			humanDur(f.Now.Sub(f.LastObservation)))
		add("agent", 2, false, "%s", m)
		promote(2, m)
	} else if !f.HasObservation {
		m := "已報到，但還沒有送過完整觀測"
		add("agent", 2, false, "%s", m)
		promote(2, m)
	}

	// 3c. 磁碟
	if f.DiskTotalBytes > 0 {
		usedPct := 100 - int(f.DiskFreeBytes*100/f.DiskTotalBytes)
		switch {
		case f.DiskFreeBytes < DiskLowBytes:
			m := fmt.Sprintf("磁碟只剩 %s（%d%% 已用）—— 寫入隨時會開始失敗",
				humanBytes(f.DiskFreeBytes), usedPct)
			add("disk", 3, false, "%s", m)
			promote(3, m)
		case usedPct >= DiskCritPct:
			m := fmt.Sprintf("磁碟 %d%% 已用，只剩 %s", usedPct, humanBytes(f.DiskFreeBytes))
			add("disk", 3, false, "%s", m)
			promote(3, m)
		case usedPct >= DiskWarnPct:
			m := fmt.Sprintf("磁碟 %d%% 已用", usedPct)
			add("disk", 2, false, "%s", m)
			promote(2, m)
		}
	}

	// 3d. workload 規則同時供 observation ingest 的 promote 證據投影使用；
	// 這裡只套用同一個 pure evaluator 的結果，不另抄一份判決。
	workload := evaluateWorkload(f)
	found = append(found, workload.findings...)
	for _, finding := range workload.degradations {
		promote(finding.Severity, finding.Message)
	}

	// 3e. 憑證
	for _, c := range f.Credentials {
		switch c.Status {
		case "expired":
			// ⚠ 「過期」有兩種，而檔案本身分不出來：
			//   (a) 閒置 —— 短命的票沒被用到就會過期，下次用的時候自己續回來
			//   (b) 卡住 —— 續期壞了、或 session 被伺服器端踢了，要人重新登入
			// 分得出來的只有時間：過了一個壽命還沒續，就是 (b)。
			// 這是 SPEC §4.3 講的「看 last_refresh 有沒有卡住」，見 CredGrace。
			grace, overdue, inGrace := CredGrace(c, f.Now)
			if overdue < 0 {
				// c.Status 是 agent 用自己的鐘判的，*c.ExpiresAt 來自憑證檔案，f.Now 是 Hub。
				// humanDur 只講長度，方向必須在這裡決定。
				add("credential", 2, true, "%s 的登入的過期時間比現在晚 %s", c.Provider, humanDur(overdue))
				continue
			}
			if inGrace {
				// 還在寬限內：要看得見，但不升級成 Degraded。
				// 文案必須講清楚它**為什麼**還不算壞、以及**什麼時候**會算壞 ——
				// 否則一個黃燈跟一個「沒事」在畫面上長得一樣。
				m := fmt.Sprintf("%s 的登入 %s前過期。這種票壽命 %s、用到才續，再 %s沒續回來才算卡住",
					c.Provider, humanDur(overdue), humanDur(c.Lifetime), humanDur(grace-overdue))
				if c.WatchedFor > 0 {
					m += fmt.Sprintf("（看了 %s，它自己續了 %d 次）", humanDur(c.WatchedFor), c.RefreshesSeen)
				}
				add("credential", 2, true, "%s", m)
				continue
			}
			m := fmt.Sprintf("%s 的登入已過期", c.Provider)
			if c.ExpiresAt != nil {
				m = fmt.Sprintf("%s 的登入在 %s前就過期了",
					c.Provider, humanDur(overdue))
			}
			hasOwnHistory := false
			switch {
			case c.Lifetime > 0 && c.RefreshesSeen > 0:
				m += fmt.Sprintf("，之前 %s內它自己續過 %d 次，現在停了",
					humanDur(c.WatchedFor), c.RefreshesSeen)
				hasOwnHistory = true
			case c.Lifetime > 0 && c.WatchedFor > 0:
				m += fmt.Sprintf("，看了 %s沒續過一次", humanDur(c.WatchedFor))
				hasOwnHistory = true
			}
			if peer, activePeers, ok := MostActiveCredPeer(c.Peers); ok {
				m += fmt.Sprintf(" —— 同一家的登入這段時間在 %s 上自己續了 %d 次",
					peer.DisplayName, peer.RefreshesSeen)
				if peer.FileMTime != nil {
					if since := f.Now.Sub(*peer.FileMTime); since < 0 {
						m += fmt.Sprintf("（更新時間比現在晚 %s）", humanDur(since))
					} else {
						m += fmt.Sprintf("（最近 %s前）", humanDur(since))
					}
				}
				if activePeers > 1 {
					m += fmt.Sprintf("，另外 %d 台也在續", activePeers-1)
				}
				m += fmt.Sprintf("；%s provider 仍更新，這台的 session 已停止續期", c.Provider)
			} else if hasOwnHistory {
				m += " —— 要重新登入"
			}
			add("credential", 3, false, "%s", m)
			promote(3, m)
		case "expires_soon":
			m := fmt.Sprintf("%s 的登入即將過期", c.Provider)
			if c.ExpiresAt != nil {
				m = fmt.Sprintf("%s 的登入 %s後過期", c.Provider, humanDur(c.ExpiresAt.Sub(f.Now)))
			}
			add("credential", 2, false, "%s", m)
			promote(2, m)
		case "failed":
			m := fmt.Sprintf("%s 最近一次真實請求失敗", c.Provider)
			add("credential", 3, false, "%s", m)
			promote(3, m)
		case "unknown":
			// ⚠ unknown 不升級成 Degraded，但它一定要看得見。
			// 把 unknown 當成綠燈是這個專案最想避免的事。
			note := c.Note
			if note == "" {
				note = "讀不到過期時間"
			}
			add("credential", 2, true, "%s 的登入狀態未知：%s", c.Provider, note)
		case "absent", "configured":
			// ⚠ configured 是有憑證且過期時間還沒到，absent 是沒裝這家；兩者都是事實，不長 finding。
		case "":
			// ⚠ Advisory 分的是有沒有影響主狀態，不是是不是已知限制。
			// 這兩格不 promote，所以必須是 advisory；它們跟 unknown 靠句子與下一步
			// 分開（「把這台的 agent 升到跟 Hub 同版」vs「讀不到過期時間」），不靠這個 bit。
			add("credential", 2, true, "%s 的登入沒有回報狀態：把這台的 agent 升到跟 Hub 同版", c.Provider)
		default:
			add("credential", 2, true, "%s 的登入回報不認得的狀態 %q：把這台的 agent 升到跟 Hub 同版", c.Provider, c.Status)
		}
	}

	// 3f. 時鐘
	if f.ClockSkew > ClockSkewTolerance || f.ClockSkew < -ClockSkewTolerance {
		add("clock", 2, true,
			"這台的時鐘跟 Hub 差 %s —— 它自己回報的時間不可信（存活判斷不受影響）",
			humanDur(abs(f.ClockSkew)))
	}

	if degraded {
		return Judgement{State: Degraded, Reason: primary, Findings: found}
	}

	reason := fmt.Sprintf("%s前回報", humanDur(silence))
	if f.OpenClawPresent && f.HasTaskSignal {
		if idle := f.Now.Sub(f.LastTaskEnded); idle < 0 {
			reason += fmt.Sprintf("；最近一次跑完的時間比現在晚 %s", humanDur(idle))
		} else {
			reason += fmt.Sprintf("；最近一次跑完是 %s前", humanDur(idle))
		}
	}
	return Judgement{State: Online, Reason: reason, Findings: found}
}

// CheckinDeadline is the authoritative liveness boundary shared by state
// derivation and operator-facing rollups. A check-in is still current exactly
// at this instant; it becomes unreachable only strictly after the deadline.
func CheckinDeadline(f Facts) time.Time {
	interval := f.CheckinInterval
	if interval <= 0 {
		interval = CheckinInterval
	}
	return f.LastCheckinReceived.Add(interval).Add(UnreachableGrace)
}

// IsReportingAt says whether the Hub has a current heartbeat from this
// machine. State alone cannot answer that question: IdentityConflict is a
// durable identity verdict and can outlive the last heartbeat that caused it.
func IsReportingAt(f Facts, at time.Time) bool {
	return f.EverCheckedIn && !at.After(CheckinDeadline(f))
}

// ---------------------------------------------------------------- 格式

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// HumanDur 是 humanDur 的對外版本，給 web 層用。
//
// ⚠ 判決層跟畫面層必須用同一支格式化函式。兩邊各寫一份的話，
// 早報說「21.1 小時」而網頁說「21 小時」，人會以為那是兩件事。
func HumanDur(d time.Duration) string { return humanDur(d) }

// judgeEvents 判一條事件流。
//
// ⚠⚠ 這裡最重要的一句話：**這個 finding 只要條件還在就會一直在**。
// 它不是「今天新出現了 N 次」，是「現在這台機器上有一個沒有被解決的狀況」。
// operator 的原話是「control panel 是一個狀態機，我們永遠就能看到一些 error 在那裏」——
// 而 clawctl 整台引擎本來建立在**變化偵測**上，一個穩定地失敗的東西
// 在變化偵測裡是靜止的（實測 sampleagent2 八天每小時 48/48/49/49/48/48/49，完全平的）。
// 所以這一段刻意不看「比上次多了幾次」，只看「現在有沒有」。
func judgeEvents(label string, a ArtifactFact, add func(string, int, bool, string, ...any), promote func(int, string)) {
	e := a.Events
	switch {
	case !a.Checked || !e.Measured:
		// ⚠ 不重複講。ArtifactFact 那邊已經為「還沒量到」發過一則 advisory 了。
		return
	case e.Err != "":
		// ⚠ 跟 artifact 的 Err 同一個分寸：「看不到」是 unknown 不是失敗，
		// 但它一定要改變狀態 —— 一條讀不到的規則等於沒有在監控，
		// 而那絕不能長得跟「監控過了，沒事」一樣。
		m := fmt.Sprintf("%s 的事件流 %s 讀不到：%s", label, a.Artifact, e.Err)
		add("workload", 2, false, "%s", m)
		promote(2, m)
		return
	}

	var hits []EventCount
	for _, d := range e.Declared {
		if d.Count > 0 {
			hits = append(hits, d)
		}
	}
	if len(hits) > 0 {
		var parts []string
		for _, h := range hits {
			p := fmt.Sprintf("%s ×%d", h.Type, h.Count)
			if e.Partial {
				// ⚠ 撞到讀取上限時，次數是**下界**。把一個下界寫成總數，
				// 人會拿它去對帳，然後對不起來。
				p = fmt.Sprintf("%s ≥%d", h.Type, h.Count)
			}
			parts = append(parts, p)
		}
		m := fmt.Sprintf("%s 在過去 %s內回報了 %s —— %s",
			label, humanDur(e.Window), strings.Join(parts, "、"), a.Why)
		add("workload", 3, false, "%s", m)
		promote(3, m)
	} else {
		// ⚠ 措辭：「沒有回報」不是「沒有問題」。我們知道的只是那支驗證程式
		// 在這個窗口裡沒有寫出這幾個名字 —— 它有沒有真的檢查過，
		// 是新鮮度那一條在回答的，不是這一條。
		var names []string
		for _, d := range e.Declared {
			names = append(names, d.Type)
		}
		add("workload", 1, true, "%s 在過去 %s內沒有回報 %s",
			label, humanDur(e.Window), strings.Join(names, "、"))
	}

	// ⚠⚠ 沒宣告過的事件名一定要看得見。一種新長出來的失敗事件，
	// 在只看宣告名單的世界裡是完全隱形的 —— 而它恰好最可能是
	// 還沒有人想到的那一種。advisory：讓人看到，但不替人判它是壞事。
	if len(e.Undeclared) > 0 {
		var parts []string
		for _, u := range e.Undeclared {
			parts = append(parts, fmt.Sprintf("%s ×%d", u.Type, u.Count))
		}
		more := ""
		if e.UndeclaredTotal > len(e.Undeclared) {
			more = fmt.Sprintf("，另外還有 %d 種沒列出來", e.UndeclaredTotal-len(e.Undeclared))
		}
		add("workload", 1, true, "%s 的未宣告事件：%s%s",
			label, strings.Join(parts, "、"), more)
	}
	if e.Malformed > 0 {
		// ⚠ 安靜跳過壞掉的行，會讓一個格式壞掉的事件流長得像一個很安靜的事件流。
		add("workload", 2, true, "%s 的事件流有 %d 行無法解析",
			label, e.Malformed)
	}
}

// humanDur 講人話。「11 小時」比 "11h0m0s" 好讀，而這個產品的整個賣點
// 就是讓人一眼看懂。
func humanDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分鐘", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f 小時", d.Hours())
	default:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
}

func humanBytes(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// isObservationStale：這台機器的完整觀測是不是已經久到該講話了。
//
// ⚠ 抽成一個有名字的函式，不是留在判決裡當一行 inline 條件 ——
// 因為常數對了、比較寫錯（`>=`、拿錯欄位相減）一樣會震盪，
// 而那種錯誤只有直接測這個述詞才抓得到（thresholds_test.go）。
//
// ⚠ 用嚴格大於。等於門檻不算過期 —— 一個「剛好卡在門檻上」的觀測，
// 兩種解讀都說得通，而在說得通的兩種之間，這個專案一律選不吵人的那種。
func isObservationStale(f Facts) bool {
	return f.HasObservation && f.Now.Sub(f.LastObservation) > ObservationStale
}
