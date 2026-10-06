# PRODUCT — AI-Intune / clawctl

> 這份文件回答「這是什麼、給誰、成功長什麼樣」。
> 技術怎麼做在 [SPEC.md](SPEC.md)。時程與尚未有答案的問題記在私人工作筆記，不在這個公開倉庫。

---

## 1. 一句話

**AI-Intune 是一個管 Linux、macOS 與 Windows、單人維護的集中維運平臺，讓你打開一頁就知道自己那幾十台跑著 AI agent 的機器誰沒報到、誰版本不對、誰的登入掉了、誰很久沒真的完成過工作 —— 而且每一盞燈背後都有證據。**

CLI 與後端服務代號 `clawctl`。

### 三條產品硬規則

1. **所有 operator 功能與 API 都要有 UI。** CLI 保留給 automation、診斷與 break-glass，不得形成永久的 CLI-only 正常流程。
2. **介面要像 Intune 一樣一目了然。** 先看 fleet headline，再從 Machines、Apps、Deployments、Updates、Reports 與 Settings 往下鑽；數字必須能點到機器與證據。
3. **盡量貼著上游走。** AWX/AAP 等上游負責通用 inventory、execution、workflow、credential 與 RBAC；只有 desired state、AI agent 健康語意、promotion policy、雙觀測關聯與 verdict 由 clawctl 自己寫。

API、CLI 與雙端驗證的正式邊界見 [CONTROL-PLANE-CONTRACT.md](CONTROL-PLANE-CONTRACT.md)。

---

## 2. 這是誰的問題

使用者是一位 operator，手上有（實測盤點是私人工作筆記，不在這個公開倉庫，2026-09-02）：

- **5 台 Linux 機器**：samplehub1（家用 GPU 主機）、sampleagent1 / sampleagent2 / sampleagent3（Oracle Cloud）、sampleagent4（公司機）。四台跑 OpenClaw，**四個不同版本**
- **4 台 Windows 客戶端**：當時是人坐的地方，也是憑證與現有監看工具住的地方。2026-09-19 鎖文把 Windows 列為與 Linux／macOS 同等的受管端點；這次盤點不是「Windows 不管」
- **約 40 個 agent / subagent** 分散在這些機器上 —— 這才是「~30」那個數字的來源
- **約 10 個個人 AI 帳號登入**：2~3 個 Claude、2 個 Grok、2 個 GPT、3 個 Gemini
- **一批 API key**
- 這些機器上的 agent **實際在幫他處理 operation**，不是玩具

**當時 Linux 分母只有 5 台，但盤點當下的狀態是：4 台遠端機器的 Claude 登入全部過期**（137 天 / 68 天 / 13 天 / 剛過期），**sampleagent1 磁碟 97% 且它自己的管理 agent 已經連續三次以上寫下 URGENT 而沒有人看到**。

問題從來不在規模，在能見度。

他每天實際在痛的是這串，逐字照抄：

> 我根本不知道哪些是什麼版本、哪些被 logout 了、哪些 openclaw 有 error、哪些的 api 用了多少、過期了需要我重新登入、cli 版本不對、需要裝什麼軟體，光找到對的那台機器 ssh 進去就快要搞死我了。
>
> **尤其 agent 時代很多工作都是 error 會沉默的。**

---

## 3. 產品的兩條地基

整個專案建立在兩個經過長時間辯論、四方獨立驗證的判斷上。所有設計選擇都可以回推到這兩條。

### 地基一：這是「我不知道」的問題，不是「我按不下去」的問題

上面那串痛點，**沒有任何一條是「按鈕不夠多」**。SSH 讓人想死的不是 SSH 本身，是要先猜是哪一台；真的找到了，那幾行指令根本不花時間。

**所以先做觀測，控制晚一個月做都沒差。** 觀測那一半弄壞不了任何東西，而且直接消掉九成的痛。

這條決定了 Phase 1 是唯讀的，也決定了為什麼 Dashboard 比批次部署重要。

### 地基二：自證不算數

**能自己說自己很好的東西，它的話不能當證據。**

這條殺掉三種很自然、但全都會生出假綠燈的做法：

| 做法 | 為什麼是假的 |
|---|---|
| 讓 OpenClaw / AI agent 自己寫一個 `health.json` 說「我很好」 | 卡住的 agent 一樣寫得出「我很好」。那不是狀態，那是屍體的體溫 |
| 掃 log 關鍵字，看到 `429` / `Unauthorized` 才算壞 | **最痛的那種錯，定義上就是 log 裡沒有 error** |
| 部署工具跑完全綠 = 裝好了 | 那只代表 SSH 有通、指令有跑完 |

健康狀態只有兩種合法來源：

1. 那台**剛剛真的跑完一次任務**（留下時間、用了哪張票、花多少）
2. **工作做完之後，由一支跟被測對象無關的程式**去跑驗證指令，過或不過

這條決定了資料模型裡為什麼 `verification_results` 是獨立一張表、為什麼 `succeeded` 要由 Hub 檢查證據才給、為什麼憑證狀態不能壓成一個布林值。

---

## 4. 成功長什麼樣

### 4.1 唯一的北極星指標

> **任何一個功能，如果做完之後「人需要在場的次數」沒有變少，那它就是裝飾品。**

### 4.2 可以量的

| 指標 | 現在 | 目標 |
|---|---|---|
| 發現一台機器有問題所需時間 | 不確定，通常是**事情已經爛掉之後**才發現 | 早上一則推播，或打開 Dashboard 10 秒內 |
| 從「知道有問題」到「連上正確那台機器」 | 翻 IP、猜 hostname、開 SSH，數分鐘到數十分鐘 | Dashboard 點兩下 |
| OAuth 續期的處理方式 | 隨機在下午三點、半夜、禮拜天早上掉線 | **提前一週知道**，禮拜天早上一次處理完 |
| 「這台到底有沒有在做事」 | 只能看 process 活著沒（永遠是活的） | 看得到「最後一次真的完成任務是幾點」 |
| 升級 30 台之後的結果 | 「指令送出去了」 | 「30 台驗證裝好、3 台回退、每一台都點得進去看證據」 |

### 4.3 三個月後還在用

單人養的維運工具，**最常見的死法不是壞掉，是荒廢。而荒廢的綠燈比沒有燈更危險，因為你還在信它。**

所以有兩條硬性產品要求：

- **每天早上主動推一則訊息。** 沒變化也要送一行 `alive; 0 changes`，因為「沒收到訊息」必須能明確代表「Hub 死了」，而不是「今天沒事」。
- **由 Hub 以外的東西監看那則訊息有沒有準時送到。** Hub 自己說自己健康，一樣是自證。

### 4.4 複雜度的天花板

> **複雜度的上限，就是你半夜一個人修得動的程度。**
>
> 一個你不敢動的監控系統，跟沒有監控是一樣的。

任何設計如果讓單人維護者不敢在半夜動它，這個設計就是錯的 —— 不管它在架構上多正確。

---

## 5. 它不是什麼

明確劃掉，避免範圍膨脹：

| 不是 | 為什麼 |
|---|---|
| **不是 OAuth token 的集中錢包** | Hub 代收再派發，等於把所有帳號的鑰匙集中放在一個看板裡。中控台被打穿，你損失的應該是一個看板，不是十個帳號。**token 永遠留在它自己那台機器上** |
| **不是 AI 流量的 Proxy / Gateway** | 把資料面拐回 Hub，Hub 一斷全部停；而且訂閱制帳號的流量根本不能 proxy，做下去是永遠追不上上游的相容地獄 |
| **不是通用遠端 shell** | 一旦能下發任意 shell 字串，這東西就是一個批量後門。Script 必須是版本化、有 digest、有 timeout 的套件，不是自由文字 |
| **不是完整的 log 平臺** | 平台事件（誰在何時對哪台做了什麼、結果如何）存 Hub；應用程式 log 交給既有系統，不要全塞進 Hub 的資料庫 |
| **不是會自己修東西的 AI** | AI 負責解釋與建議，不決定紅綠燈、不自己排下一張工作單、不自動換 key、不自動換模型 |
| **不是全平臺 MDM** | Linux、macOS、Windows 都是一等受管平台。不做完整 MDM（沒有 DEP／ABM／手機／條件式存取），RDP 後排、也不等於 Windows agent。服務管理層各平台一份（systemd／launchd／Windows 使用者排程工作），不做一套抽象吃下所有 OS |
| **不是通用 RMM / 資產盤點平臺** | 抄 Intune 的是**管理邏輯**（偵測規則、check-in 模式），不是它的功能廣度 |

## 5.5 平台涵蓋範圍

方向以 2026-09-19 的私人鎖文為準（不在這個公開倉庫）：Linux、macOS、Windows 都是一等受管平台。只有 RDP 後排。下面是本樹目前量得到的程式狀態，不是實機或硬體驗收。

### 兩套軟體庫

1. **Default（always pinned）：** Claude、Codex、Grok/xAI、Antigravity 四個 CLI，加上 BAT（Connect BAT 是預設連線）。Claude Code、Codex、Grok、Antigravity 與 Linux BAT Server 已有官方來源 intake、標準 catalog manifest 與 adapter；完整預設 profile 尚未交付。
2. **Pinokio（optional）：** 上游 Store 可浮動；**指派／管理時釘版**；升級是 check + approve，沒有靜默自動升級。本樹尚未有 Pinokio 收錄或指派流程。

收錄進 catalog 不表示每台都安裝。Node runtime、Claude Code、Codex、Grok 與 Antigravity 有 Linux／macOS／Windows 套件路徑；OpenClaw、Hermes 與 BAT Server 目前只支援 Linux。這些路徑不代表完整 default 四 CLI＋BAT profile 已交付，也不代表 macOS／Windows 已通過實機驗收。token 留在各機。

### 逐 OS 矩陣

| 層 | Linux | macOS | Windows |
| --- | --- | --- | --- |
| Agent | 已出貨；linux amd64／arm64 | darwin amd64／arm64 交叉編譯通過；尚未實機驗收 | windows amd64／arm64 交叉編譯通過；原生 probe 已接線，尚未實機驗收 |
| Enrollment | systemd installer 兌換票 | installer／LaunchAgent／Hub readiness 已有 Linux fixture 驗證 | credential ACL、user scheduled task installer、Hub bootstrap 雙架構組與 fixture check-in／readiness 已接線；尚未實機 enrollment |
| Service | systemd system unit + 明確非特權 `User=`；user manager 保留 linger 供 runtime units 使用 | LaunchAgent；沒有對應的 watchdog／CPU／記憶體上限 | 目前使用者 scheduled task（logon）；沒有 linger／watchdog／CPU／記憶體上限 |
| Check-in | 報到、心跳與觀測在線 | XML plist 優先；binary／無法讀取時以有逾時與輸出上限的 `sw_vers` 取得身分；uptime 已接線 | NT 版本、原生架構、CPU、uptime、磁碟與記憶體接入共用 probe；credential restart 後的 check-in／readiness 已有本機 fixture；尚未實機 enrollment |
| Evidence | systemd／journal／`/proc` | 記憶體、load、boot_id、服務與日誌仍缺原生量測 | 原生資源量測、MachineGuid 候選、clawctl-agent scheduled task 狀態與 Task Scheduler 一小時記錄摘要；load／linger／boot_id 保留未知 |
| Profile | Node／OpenClaw／Hermes | Node 本地 HTTP／executor／recovery 已驗證；OpenClaw／Hermes 仍只 Linux | Node 本地 preview／apply／executor／Hub 量測證據已驗證；OpenClaw／Hermes 仍只 Linux |
| Packaging | Hub 發布／下載 Linux 必要雙架構組 | Hub 可發布／下載完整 Darwin 雙架構組，沿用既有下載 route；舊 Linux-only release 仍可讀 | Hub 可發布／下載完整 Windows 雙架構組；舊 Linux-only／Linux+Darwin release 仍可讀 |

Hub 名冊與 catalog schema 已平台中立：`machine_registry.os/arch` 是 TEXT 無 CHECK；`catalog.Platform` 無 enum；`validPlatform` 只做識別字規則。指派身分轉換保留 linux／darwin／windows；沒有適用 executor 時不建立工作單。

### Linux

已出貨。報到、check-in、觀測、job、verifier 都在線上。2026-09-02 盤點的五台 Linux 是當時的實測分母，不是產品只做 Linux 的理由。

### macOS

一等平台。交叉編譯、installer／LaunchAgent、darwin 身分與 Node runtime 已在本機接線；**沒有在真的 Mac 上跑過**。

已經做到的：

- `internal/probe/statfs_linux.go`／`statfs_darwin.go` 依平台分檔；`make cross-darwin` 產 darwin binary。
- `ops/install-agent-macos.sh` 裝 binary、enroll、渲染 `ops/clawctl-agent.plist`、`launchctl bootstrap`、`verify`；Linux installer 在 Darwin 上指出該跑哪一支。
- `internal/probe/osname_darwin.go` 讀 `SystemVersion.plist`；XML 內優先採用非空 `ProductUserVisibleVersion`，缺少時採用 `ProductVersion`。binary／無法讀取時以有逾時、輸出上限且移除 child `SYSTEM_VERSION_COMPAT` 的 `sw_vers` 取得身分；失敗或 malformed XML 保留未知。
- Mac 顯示身分保留為 darwin target；profile 指派讀取已落帳的 agent identity。
- Node runtime artifact／executor 已接入 darwin amd64／arm64。本地測試涵蓋 preview／apply、釘版工作單、HTTP 下載授權、成功／失敗完成、遺失回應重試、同版恢復；`catalog assign` recovery 檔在完整輸出後才清除。這是 fixture／operator HTTP 證據，不是 Mac 上的 Node／npm 執行證明。
- `make agent-bundles-darwin` 產 amd64／arm64 bootstrap（binary、installer、plist、VERSION）。[Mac 驗收路徑](MACOS-AGENT-VALIDATION.md) 列出實機步驟。
- uptime 接入系統連續時鐘，含睡眠、不因 agent 重啟歸零；讀取失敗仍為未知。

量到的缺口：

- plist 與安裝腳本只經過 Linux 上的 mock（`ops/test-ops.sh` 的 `launchctl`／`uname`／BSD `stat`）。目標 macOS 的 `SystemVersion.plist` 是 XML 還是 binary 沒有實機確認。
- probe 其餘 Linux-only 來源：`/etc/machine-id`、`/proc/sys/kernel/random/boot_id`、`/proc/meminfo`／`loadavg`、`systemctl --user show`、`journalctl --user`。Darwin `load1m()` 回 nil。
- launchd 沒有 `Type=notify`／`WatchdogSec`／`CPUQuota`／`MemoryMax` 的對應；plist 檔頭逐條寫明。
- Hub bootstrap 已支援完整 Darwin 雙架構組；新程式尚未部署到 live Hub。
- OpenClaw／Hermes adapter 仍只提供 Linux。Node 接線不代表所有 app 都能指派到 Mac。

### Windows

一等平台。Windows amd64／arm64 agent 已可交叉編譯；`internal/probe/*_windows.go` 使用原生 API 量測 NT 版本、原生架構、uptime、磁碟與記憶體，MachineGuid 只作身分候選。無法量測時保留未知；不從 NT build 猜 Windows 10／11，不把 emulated process 架構當成硬體架構。

credential 檔以目前使用者、受保護、不含繼承項的 DACL 建立與驗證。Hub 可發布完整 Windows amd64／arm64 bootstrap（PE executable、user scheduled task、PowerShell installer）。觀測把 `\clawctl\clawctl-agent` 的 Task Scheduler 狀態寫進既有 Unit 契約；其餘 watched unit 為已量到的未註冊。已註冊的 agent unit 讀 Task Scheduler Operational 一小時記錄，走既有 journal 摘要（行數與句型，不判健康）；讀不到與安靜分開。Node runtime artifact／executor 已接入 windows amd64／arm64；官方 intake 把 win-x64／win-arm64 zip 重排成 bundle 的 `bin/node.exe` 與 `lib/node_modules/`。本地測試涵蓋 preview／apply、釘版工作單、成功／失敗完成與 Hub 量測證據。這是 fixture 證據，不是 Windows 上的 Node／npm 或 Event Log 執行證明。enrollment receipt、bundle 准入與 task 狀態對照已有本機 fixture。[Windows 驗收路徑](WINDOWS-AGENT-VALIDATION.md) 列出實機步驟。尚未交付 LocalSystem service 或 OpenClaw／Hermes adapter。RDP 後排。

---

## 6. 為什麼是自己做，而不是裝一套現成的

現成的裝置管理平臺（Intune、Lansweeper、osquery/Fleet）都很成熟，但它們有一個共同的盲區：**它們看得懂作業系統，看不懂 AI agent。**

它們回答得了「這台裝了哪些套件、開了哪些 port、磁碟剩多少」。
它們回答不了：

- 「Claude 這個登入禮拜四會過期」
- 「這台最後一次真正跑完任務是八小時前」
- 「這張票現在掛在哪一台」

而後面那三個，正好就是這個使用者全部的痛。

所以策略是：**能力借用、邊界自己畫。** systemd 顧 process、Tailscale 顧網路、Ansible 顧初裝、Postgres 顧持久化 —— 這些一律不自己寫。但「什麼叫健康」、「什麼叫成功」、「證據是什麼」這層語意，沒有人替你定義過，這是這個專案唯一真正要寫的東西。

詳細的整合取捨見 [SPEC.md §8 開源整合決策](SPEC.md#8-開源整合決策)。

---

## 7. Intune 教了我們什麼

> Intune 真正值錢的東西不在左邊那排選單，那排你三天就抄完了。
> 值錢的是每一個項目後面都綁著一句「**我怎麼知道它真的成功了**」。

Intune 裡每個 app、每個 policy 都必須填一條**偵測規則（detection rule）**：你推一個安裝下去之後，機器要拿什麼證據回來證明它真的裝好了。**沒填這個，Intune 不讓你存檔。**

這就是為什麼它敢在畫面上說「200 台裡面有 3 台失敗」—— 它回報的不是「指令送出去了」，是「我去檢查過了，這 3 台不對」。

**AI-Intune 照抄這一條，而且抄得更硬**：偵測規則不只要有，還要規定「那個答案由誰產生」—— 必須由管理端定義、由一支跟被測對象無關的程式去驗，而且驗完要留下實際指令、exit code、輸出摘要與驗證時間。

同時，Intune 有一題完全沒有答案可以抄：**十張票怎麼在幾十台機器上排班。** 那是這個平台唯一獨特、也是唯一還沒設計完的東西。未解問題記在私人工作筆記，不在這個公開倉庫。

---

## 8. 功能地圖

左側選單沿用 Intune 的分類邏輯，因為那套分類本身是對的。但每一項背後都是同一條線：

```
指派期望狀態 → 產生工作單 → 分批執行 → 獨立驗證 → 回報結果 → 處理例外
```

**Update 只是把群組的目標版本改掉**，不需要另外發明機制。這一句讓後端程式碼少寫一半。

| 選單 | 它實際上是什麼 | 第一版 |
|---|---|---|
| **Dashboard** | 狀態切片 + 跟昨天比的變化。不放 log | ✅ Phase 1 |
| **Machines** | 名冊（分母）、分組、check-in、詳細頁。詳細頁**最底下**放 `Connect BAT` | ✅ Phase 1 |
| **Apps** | Default 庫（Claude／Codex／Grok／Antigravity 四 CLI＋BAT，always pinned）與 Pinokio optional（指派時釘版）。現有可發佈 Store 包含 Node、OpenClaw、Hermes、Claude Code、Codex、Grok、Antigravity、Linux BAT Server；完整 default profile 尚缺。收錄 ≠ 每台安裝。每個版本都必須有偵測規則與 artifact digest | Phase 3 |
| **Policies** | 集中的環境變數與 API key 指派。持續檢查，不符就修正 | Phase 4 |
| **Scripts** | 一次性動作。跑過就過了，只記錄那次成功還失敗 | Phase 3+ |
| **Deployments** | 這次推得怎樣：影響哪些機器、成功幾台、失敗幾台、卡在哪 | Phase 3 |
| **Updates** | 現在有哪些新版、canary / stable 通道指向哪一版 | Phase 3 |
| **Reports** | **不是另一套資料**，只是把累積的結果換個角度看 | Phase 5 |
| **Settings** | 註冊、套件來源、Tailscale、權限 | 逐步 |

**App 跟 Script 為什麼分開**：App 是**有狀態**的東西，只有「裝了 / 沒裝 / 版本多少」三種樣子，你隨時可以問它現在長怎樣；Script 是**一次性動作**，跑過就過了。這兩種東西的檢查方式完全不同，混在一頁做不下去。

**BAT 為什麼不在左邊那排**：左邊每一項都是「一次對全部機器」，terminal 是「一次對一台」。放同一層，看到紅燈就會反射性點進去，然後回到一台一台救火的日子。BAT 是讀完之後的手術刀，不是輸送帶。

---

## 9. 交付原則

1. **每個 phase 結束都要有一個真的能用的東西**，不是半成品。Phase 1 做完就該是一個你每天早上會打開的東西，即使它一個字都不能寫。
2. **每個交付的正常流程都必須在介面完成。** Phase 可以縮小功能範圍，但不能把已交付能力永久留成 shell-only；資料模型與 API 也要照最終形狀開。
3. **先做弄不壞任何東西的那一半。** 唯讀的部分沒有回退問題、沒有互踢問題、最快有畫面。
4. **不確定的事情不要寫進 spec 假裝已解決。** 未驗證就標未驗證。那些未解問題記在私人工作筆記，不在這個公開倉庫。

---

## 10. 給第一個 clone 這個 repo 的人

> 你寫完之後，會有人 clone 下來，照著 README 跑起來，然後把他自己的十個帳號放進去。
> 那個人不會讀完我們吵的這四輪。**他只會用預設值。**

所以預設值就是別人的底線。預設一律是：

- `bat-server` 綁私網位址，不開公網
- Agent 只做 outbound 連線，機器上不開任何 inbound 控制 port
- OAuth token 不離開節點
- 上游新版只偵測、只開 PR，**不自動發版**；Pinokio 與 default 庫的升級都是 check + approve，沒有靜默自動升級
- 未知版本的 CLI：讀取可以繼續，**寫入預設停用**
- catalog 收錄不表示每台都安裝；default 四 CLI＋BAT 必須釘版才算交付

要放寬的人，自己承擔。
</content>
