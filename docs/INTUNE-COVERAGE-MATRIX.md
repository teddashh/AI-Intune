# Intune 對齊覆蓋矩陣

`docs/INTUNE-UI-REFERENCE.md` 把那份參考說明書變成**版面與用語**的契約。
這一份是**功能**那一半：把說明書的每一頁歸到一個領域，再把每個領域對到
AI-Intune 現在真的有的東西，並且說出差距。

這不是宣稱 AI-Intune 實作 Microsoft Intune 或 Microsoft Graph。
它是決定「下一個要做什麼」的證據，取代用印象排順序。

## 1. 來源與計數

- 來源：`/mnt/nas/Live_Data/intune.pdf`，243,902,408 bytes
- SHA-256：`df22679d0ad6f0d6c7c5f1e09d3ece1de3a83c1b20f6f6c863efc5a382a10839`
  （與 `docs/INTUNE-UI-REFERENCE.md` 記的同一個雜湊，2026-09-12 重新驗過）
- 6,220 頁、836 篇文章，每一頁都屬於某一篇，沒有落單的頁

**一個會改變工作量的結構事實**：836 篇裡只有 786 個不同標題。48 個標題出現
超過一次，重複的部分佔 **517 頁**。端點安全整章（1,639–1,885）在 3,185–3,431
幾乎原樣再出現一次；憑證章（1,427–1,481）在 4,112–4,296 再出現一次；
「新功能」週報（597–670）與（692–765）是同一篇的兩份。

所以真正要對齊的表面是 **5,703 頁**，不是 6,220 頁。

## 2. 領域劃分

分類是關鍵字比對，依優先序取第一個命中的領域當主領域，其餘記成次領域，
所以「Android 應用程式保護原則」會落在應用程式而不是平台專屬。
836 篇全部有主領域，未分類 0 篇。

| 篇 | 頁 | 領域 | 主要頁塊 |
| ---: | ---: | --- | --- |
| 123 | 1152 | 端點安全 Security | 3860–3964、3983–4072、4198–4269、3436–3502 |
| 124 | 984 | 應用程式 Apps | 2851–3005、2619–2747、2503–2591、2420–2491 |
| 83 | 519 | 註冊 Enrollment | 1016–1092、826–886、892–937、962–1004 |
| 71 | 510 | 組態設定 Configuration | 1886–1950、2154–2215、2088–2148、1567–1600 |
| 70 | 468 | 第三方整合 Connectors | 3517–3614、3623–3695、3144–3184、2341–2376 |
| 43 | 366 | 報告監視 Reports | 4819–4973、5752–5814、5839–5862、5281–5301 |
| 55 | 339 | 角色與租戶 RBAC/Tenant | 412–496、5124–5203、384–405、4446–4459 |
| 46 | 299 | 平台專屬 Platform | 2748–2776、1489–1516、322–345、4489–4512 |
| 30 | 267 | 教學與規劃 Planning | 6057–6104、16–48、248–268、671–691 |
| 36 | 262 | Intune Suite 代理程式 | 5343–5405、5055–5112、4991–5041、4403–4445 |
| 14 | 208 | 版本資訊 Release notes | 692–766、597–670、5986–5997、1655–1663 |
| 24 | 158 | 合規性 Compliance | 3006–3067、3075–3124、3843–3859、280–290 |
| 10 | 131 | 自動化 API | 5449–5510、5669–5693、5728–5738、5594–5603 |
| 23 | 121 | 更新管理 Updates | 4595–4633、4547–4562、4566–4579、1308–1315 |
| 10 | 111 | 網路與基礎結構 Network | 3696–3742、101–126、352–375、3803–3809 |
| 18 | 96 | 教育版 Education | 6129–6149、2837–2850、5969–5979、1093–1101 |
| 9 | 67 | 疑難排解 Support | 5425–5448、5406–5417、4395–4402、177–183 |
| 21 | 65 | 裝置管理 Devices | 4742–4753、5922–5931、4780–4786、346–351 |
| 15 | 49 | 資料與隱私 Privacy | 5236–5254、5113–5123、5220–5230、6215–6220 |
| 11 | 48 | 條件式存取 CA | 3125–3137、237–247、3615–3622、6165–6170 |

分類器與文章表在 `/tmp/ai-intune-pdf-audit/`（`classify.py`、`articles.json`、
`by-area.txt`）。那是可重建的中間產物，不入庫。

## 3. 狀態的定義

| 狀態 | 意思 |
| --- | --- |
| 已交付 | Web／CLI／API／持久化／稽核／測試都有，操作員今天就能用 |
| 部分 | 核心路徑在，但有明確缺的動作或視圖，缺的部分列在該列 |
| 缺 | 對本產品有意義，但一條路徑都沒有 |
| 不適用 | 本產品沒有這個受管對象（手機、Apple／Google 帳號、第三方 MDM），照抄只會做出空殼 |

「不適用」是判斷，不是省事。`docs/INTUNE-UI-REFERENCE.md` 的規則是：
**不為了長得像 Intune 而掛上一個工作負載**。

## 4. 矩陣

### 4.1 有對應、已交付或部分交付

| 領域 | AI-Intune 目的地 | 狀態 | 缺什麼 |
| --- | --- | --- | --- |
| 組態設定 Configuration | `/machines/configuration`、`clawctl-hub settings`、`/v1/operator/settings`＋`setting-policies`／`setting-assignments` | 已交付 | 設定只有報到與量測兩個間隔；沒有設定範本庫、沒有衝突解析報告、沒有每機套用失敗的原因欄 |
| 合規性 Compliance | `/machines/compliance`、`clawctl-hub compliance`、`/v1/operator/compliance`＋`compliance-policies`／`compliance-assignments` | 已交付 | 規則只有 Hub 量得到的五種事實，沒有自訂指令碼規則；不合規動作只有停發工作單一種；沒有合規性趨勢報告。Intune 的「給端點使用者的通知」不適用：沒有另一個要被推播的終端使用者；開票與合規的操作員就是納管的人。Windows 一等受管不改變這條 |
| 註冊 Enrollment | `/machines/enrollment`（含註冊上限）、`enroll-token`、`POST /v1/operator/enrollment-tokens`、`enrollment-limit`、`/v1/operator/enrollment-limit`、`/reports/enrollment`、`report enrollment`、`GET /v1/operator/enrollment-report` | 已交付 | 註冊報告與註冊上限都已交付；上限算的台數就是報告的分母，撤票與票過期都不空出名額，退役才會。Intune 的「註冊通知」不適用：開票的操作員就是納管的人，沒有另一個要被通知的終端使用者。Hub 報到不檢查 OS 家族；Linux／macOS／Windows 都是一等受管平台 |
| 裝置管理 Devices | `/machines`、`/machines/{id}`（動作目錄）、`/machines/lifecycle`、`machine actions`／`machine rename`／`machine notes`、`GET /v1/operator/machines/{id}/actions` 與 display-name／notes preview/apply | 部分 | 動作目錄已有九個具名動作，各有可用狀態、後果、被擋原因與下一步。`connect` 與 `open_terminal` 從網頁執行；`open_terminal`（開啟終端）只出現給這台機器的指派使用者，而且只出現在 Linux 機器上，所以指派使用者最多八列，其他人最多七列。「重新命名」改 Hub 名冊顯示名稱並在同交易更新未兌換 ticket 標籤；「編輯名冊備註」只改 Hub 名冊，receipt 與 audit 不複製 free text。兩者都保留 machine ID、機器設定與 agent。Intune 那邊有 30 個具名動作（同步、重新啟動、收集診斷、遠端鎖定…），其餘仍須逐個做完整垂直切片 |
| 應用程式 Apps | `/apps`（store／packages／profiles／assignments／artifacts／fetch／operations／openclaw）、`/reports/software`、`/reports/install`、`/reports/profile` | 已交付 | 已安裝清查與每機安裝狀態都已交付（`/reports/software` 回答哪個工具裝在哪幾台、各是哪一版、哪幾台沒有，以及那個版號講的是不是正在跑的那一份；`/reports/install` 回答我叫這一台裝的那一個跟我看到的一不一樣，指派意圖取 machine 與 channel 兩個 scope 裡 revision 最大的那一筆，而這台回報說它上面有的那幾格再答「看到的那個版號量的是不是正在跑的那一份」——量錯檔案那一節與那個數字在四個平面上都排在「指派的跟看到的不一樣」前面，因為那幾格比方向比的是一個沒有人在用的檔案）；`/reports/profile` 回答「這份 profile 發佈了，然後呢」，並再答每一格「看得到」的版號是不是量在沒有人跑的那一份；量錯的台數是 `seen_on` 的子集，另按 revision × 套件版本數有問題的格子，不把它折成第五種指派狀態。Profiles 與 Assignments 兩頁已改讀同一份投影：Profiles 每一版說出它現在穿在幾台身上、另有幾台已退役仍是這一版與那一格是什麼，Assignments 名冊上每一台說出它現在穿著哪一版、什麼時候被誰指派的，沒有的那一格講出「還沒有被指派過任何 profile」而不是留白（正式庫 2026-09-12 的形狀就是 `machine_profiles` 1 列、`machine_profile_assignments` 0 列，所以五台全部落在那一格）。「解除安裝意圖」量過了，這個系統沒有那個動作：spec 只有 `openclaw` 與 `noop` 兩種 kind、executor 三種都是「裝這一版」、store 一個移除路徑都沒有——要做它得先在 agent 那側長出一個會刪東西的新原語。Store 目前只有 Node／OpenClaw／Hermes；default 四 CLI＋BAT 與 Pinokio 尚未成為 catalog 套件，收錄也不表示每台都安裝 |
| 更新管理 Updates | `/updates`、`/deployments`、channel（Canary／Stable） | 已交付 | 更新環不是一級物件、沒有加速與暫停、沒有更新報告頁 |
| 報告監視 Reports | `/reports`（落地頁）、`/reports/changes`、`/reports/tickets`、`/reports/enrollment`、`/reports/software`、`/reports/install`、`/reports/profile`、`/machines/{id}/timeline`、`/machines/{id}/data`、七個 `.csv` 匯出、`/audit`、`report list`、`report enrollment`、`report software`、`report install`、`report profile`、`machine timeline`、`machine data` | 已交付 | 報告是九份固定的清單，不能自訂欄位或存成自己的檢視；沒有排程寄送；分頁讀的兩份（變更、稽核）匯不出整份 |
| 疑難排解 Support | `/machines/diagnostics` | 部分 | 只送一張不改設定的 noop 工作單證明 agent 會執行；沒有把機器狀態收回來的收集動作 |
| 自動化 API | `/v1/operator/*`（90 條 JSON 路由）、`clawctl-hub` 20 個子命令 | 已交付 | 產品裡沒有 API 參考面；CLI 沒有 `usage` 總表 |
| 網路與基礎結構 Network | `/settings/tailnet` | 部分 | 沒有「agent 要能連到哪些端點才算可用」這一頁 |
| 角色與租戶 RBAC/Tenant | Tailnet WhoIs ＋ app capability（view／operate／admin）、`/tenant/maintenance`、`/tenant/data` | 部分 | 權限是四個憑證平面硬編的，沒有角色物件、沒有範圍標籤、沒有多重操作員核准、沒有租用戶狀態頁 |
| Intune Suite 代理程式 | `/jobs`、`/jobs/{id}`、verifier 派工 | 部分 | 有活動與證據，沒有「建議」、沒有隨選裝置查詢、沒有異常報告 |

### 4.2 有對應、還沒做

| 領域 | 為什麼對本產品有意義 | 狀態 |
| --- | --- | --- |
| 資料與隱私 Privacy | Hub 收 checkin、observation、job event、verification。說明書用 5,220–5,254 交代收了什麼、存多久、給誰。 | 已交付（`/tenant/data` 與 `/machines/{id}/data`） |
| 端點安全 Security（只取基準那一段） | 說明書 3,983–3,985 叫「本機 AI 代理程式基準 - OpenClaw 安全性基準設定參考」，但它的方向相反：那是一份在 Windows 端點上**封鎖** OpenClaw 的基準（WSL1／WSL 開關，兩條 Windows Firewall CSP 規則擋 `node.exe` 出站）。Windows 是一等受管平台，這些封鎖值不能當產品政策。可以借的是形狀：一組具名設定、一份基準、每台裝置的符合狀態。 | 缺 |
| 平台專屬 Platform | Linux、macOS、Windows 都是一等受管平台。Android／iOS／iPadOS／ChromeOS／HoloLens 仍然不適用。RDP 後排，不是 Windows agent。現況見下表：Linux 已出貨；macOS 交叉編譯與 Node 本地接線、尚未實機驗收；Windows agent 已有編譯、原生 probe、credential ACL、user scheduled task bootstrap、Task Scheduler 記錄摘要與 Node runtime adapter，實機 enrollment 仍待完成。 | 部分 |

方向鎖文：default 庫（Claude／Codex／Grok／Antigravity 四 CLI＋BAT）always pinned；Pinokio optional、指派時釘版、升級 check + approve。本樹 Store 只有 Node／OpenClaw／Hermes。收錄 ≠ 安裝。下表是程式現況，不是硬體驗收。

| 層 | Linux | macOS | Windows |
| --- | --- | --- | --- |
| Agent | 已出貨；linux amd64／arm64 | darwin amd64／arm64 交叉編譯通過；尚未實機驗收 | windows amd64／arm64 交叉編譯通過；原生 probe 已接線，尚未實機驗收 |
| Enrollment | systemd installer 兌換票 | installer／LaunchAgent／Hub readiness 已有 Linux fixture 驗證 | credential ACL、user scheduled task installer、Hub bootstrap 雙架構組與 fixture check-in／readiness 已接線；尚未實機 enrollment |
| Service | systemd system unit + 明確非特權 `User=`；user manager 保留 linger 供 runtime units 使用 | LaunchAgent；沒有對應的 watchdog／CPU／記憶體上限 | 目前使用者 scheduled task（logon）；沒有 linger／watchdog／CPU／記憶體上限 |
| Check-in | 報到、心跳與觀測在線 | XML plist 優先；binary／無法讀取時以有逾時與輸出上限的 `sw_vers` 取得身分；uptime 已接線 | NT 版本、原生架構、CPU、uptime、磁碟與記憶體接入共用 probe；credential restart 後的 check-in／readiness 已有本機 fixture；尚未實機 enrollment |
| Evidence | systemd／journal／`/proc` | 記憶體、load、boot_id、服務與日誌仍缺原生量測 | 原生資源量測、MachineGuid 候選、clawctl-agent scheduled task 狀態與 Task Scheduler 一小時記錄摘要；load／linger／boot_id 保留未知 |
| Profile | Node／OpenClaw／Hermes | Node 本地 HTTP／executor／recovery 已驗證；OpenClaw／Hermes 仍只 Linux | Node 本地 preview／apply／executor／Hub 量測證據已驗證；OpenClaw／Hermes 仍只 Linux |
| Packaging | Hub 發布／下載 Linux 必要雙架構組 | Hub 可發布／下載完整 Darwin 雙架構組，沿用既有下載 route；舊 Linux-only release 仍可讀 | Hub 可發布／下載完整 Windows 雙架構組；舊 Linux-only／Linux+Darwin release 仍可讀 |

### 4.3 不適用

| 領域 | 頁 | 為什麼 |
| --- | ---: | --- |
| 第三方整合 Connectors | 468 | 內容是 MTD 廠商、Jamf、憑證連接器、NAC。AI-Intune 沒有第三方 MDM 或威脅防禦要接。 |
| 教育版 Education | 96 | 學校租戶、Apple 校務管理、學生裝置。沒有這個對象。 |
| 條件式存取 CA | 48 | 依賴 Microsoft Entra 的存取控制平面。AI-Intune 的存取邊界是 Tailnet 身分＋app capability，已在 `/settings/tailnet`。 |
| 版本資訊 Release notes | 208 | 私人工作筆記的介面規則（不在這個公開倉庫）：產品介面不得出現開發歷程、遷移說明、預告。這些屬於 repo 文件，不是操作員畫面。 |
| 端點安全 Security（其餘） | ~1,150 | BitLocker／FileVault／Defender／SCEP／PKCS／VPN／Wi-Fi／受攻擊面縮小——都是完整 MDM 對終端使用者裝置的控制。Windows 一等受管指的是 clawctl-agent（service／check-in／evidence／profile），不是這一整章。 |
| 應用程式 Apps（SDK 那一段） | ~330 | Intune App SDK／App Wrapping Tool 是給第三方 App 開發者的，不是操作員畫面。 |
| 教學與規劃 Planning（授權與試用） | ~80 | 訂閱、授權、試用。本產品沒有計費平面。 |

## 5. 建置順序

依「對本產品的真實價值 × 它擋住多少其他東西」排，不依說明書頁數排。

1. ~~**組態設定**~~：已交付。設定原則、指派、每機套用狀態、稽核、Web／CLI／API 全套。
   套用狀態的唯一證據是 agent 回送的 digest；報到間隔與 log 都不算數。
2. ~~**合規性判定**~~：已交付。規則原則、指派、每台的判決與推出它的每一條規則結果、
   稽核、Web／CLI／API 全套。規則只建立在 Hub 量得到的事實上，沒有任何一條讀機器
   自己對合規的主張；判決在讀的時候現算，不落表，因為它是「現在幾點」的函數。
3. ~~**不合規動作**~~：已交付。動作住在原則文件裡，所以它沿用同一套預覽／確認／digest／
   稽核，路由數不變。一種後果：連續不符合超過寬限期就停發工作單，那台同時被排除在
   新部署之外（理由印「合規性停發工作單」，不是「衝突」或「租約過期」）。動作跟判決
   一樣現算、不落表，機器一恢復報到就自己領得回工作單。連續多久由證據決定：Hub 重判
   寬限期那段窗裡的每一次 check-in，中間有一次過就重新起算。從未報到與量不到都不觸發
   任何動作。
   「到期後改 channel」評估後不做：channel 是操作員寫下的事實，有自己的預覽與稽核；
   讓判決去改它會跟操作員互相覆寫，而且那是一個要落表的寫入，會毀掉「恢復就自動解除」
   這個性質。
4. ~~**裝置動作目錄**~~：已交付。`/machines/{id}` 的九個具名動作（連線、開啟終端、診斷工作單、重新命名、
　 編輯名冊備註、部署通道、撤銷註冊票、退役、恢復管理）收成一份目錄，Web／CLI／API 共用同一份判斷；
　 retire／restore 只出現會改變 lifecycle 的方向。`connect` 與 `open_terminal` 從網頁執行，
　 `open_terminal` 只出現給這台機器的指派使用者而且只出現在 Linux 機器上，所以指派使用者最多八列、其他人最多七列。
   ⚠ 目錄不自己判斷任何一個動作能不能做：每個 available 都直接讀守住那條寫入路徑的
   preview，所以不會出現「目錄說可以、按下去被拒絕」。被擋的動作給 typed blocker、
   現在的狀況與下一步；沒有東西要處理的 blocker（沒有待撤銷的票）就不硬編一句下一步。
   退役與恢復管理只列會改變狀態的那一個方向。目錄只列出這個操作員握有 capability 的
   動作，view-only 連「動作」那一節都不會出現。新增 1 條 JSON route（`view`），
　 原目錄本身沒有新增 Web route；後續的重新命名與名冊備註垂直切片各加 Web 與 JSON preview/apply，
　 只改 Hub 名冊，不冒充遠端 hostname 或機器設定 mutation。
5. ~~**報告匯總頁**~~：已交付。`/reports` 落地頁在點進去之前就答完三件事——這份報告算
   的是什麼、看得回去多遠、能不能把列整份帶走。「看得回去多遠」讀的是這台 Hub 現行的
   保留期，取它與該報告單次讀取上限的較小者，所以把保留期調短這一頁會跟著改，而票證
   的 400 天帳本也不會被講成「這份報告看得到 400 天」。
   ⚠ 落地頁不重算任何一份報告的數字：四個讀取器裡三個是 cursor 分頁的，`ListChanges`
   還有併發閘與時間預算，一個帶數字的落地頁會比它連過去的每一份報告都慢，而且會無故
   失敗。匯出只給在範圍裡完整的報告：分頁讀的報告匯出的是當下那一頁，而那個檔案打開
   之後看起來跟整份報告一模一樣。
   單機事件時間軸把散在四頁的故事收成一條：名冊（進名冊、退役）、健康判定、工作單
   （開單、收尾）與操作員動作，由新到舊。每一列都來自那一頁自己在用的讀取器，時間軸
   不自己重查，所以它講的工作單收尾時刻就是工作單頁講的那一個。每一個來源各自交代
   讀到幾列、有沒有讀完這段期間，一個總數不會被當成「這段期間就只發生了這些」。
   刻意不收的兩種列：原始報到（每分鐘一列的雜訊，會變的是狀態判決）與 agent 回報的
   觀測內容（那是 `/reports/changes` 在答的）。
   匯出契約收斂成一份：BOM、UTC RFC 3339 奈秒時間、空格代表沒有值、每一格都擋試算表
   公式，欄數或控制字元對不上就拒絕產生檔案而不是輸出一份大致上對的。
   新增 3 條 HTML／CSV route 與 2 條 JSON route（都是 `view`）。
6. ~~**資料與隱私揭露面**~~：已交付。`/tenant/data` 講這個 Hub 對每一台機器留了什麼，
   單機那一頁講它現在實際留著多少列。十四類資料涵蓋二十四張存著 `machine_id` 的表，
   各自說出留的是什麼、誰產生的（機器自報／Hub 判定／操作員輸入）、含不含自由文字、
   留多久、退役之後還剩什麼、去哪一頁看。
   ⚠ 目錄不自己抄任何對照表：哪些表存著機器的資料由 SQLite 的 `machine_id` 欄位回答，
   哪些表會被時間清、看哪一個保留期由真的在刪東西的那張 prune 表回答，天數讀的是這台 Hub
   現行的保留期。同一類裡的表必須全部同一種待遇——半數被清、半數留著的類別只給得出一句
   保留期，而那一句對其中一半是假的。
   單機那一份的時間一律用 Hub 自己的鐘；沒有 Hub 時刻的列照樣算進列數，但不進最舊 / 最新，
   也不被擺到某一個時刻上。退役不刪任何一列，它改變的只有「不再有新的一列」。
   新增 3 條 HTML／CSV route 與 2 條 JSON route（都是 `view`）。
7. ~~**註冊報告**~~：已交付。`/reports/enrollment` 回答一句話：說好要納管的機器，來了沒有。
   開一張票就是有人明確說出「我打算納管這台」，從那一刻起這一列就在分母裡。名冊上的每一列
   剛好落在一個階段——已納管、拿了憑證沒回來、等它來、票過期沒用、沒有票可以用、已退役——
   各自附一句「這是什麼」與一句「下一步做什麼」；還沒到的排在最上面，等最久的排最前面。
   量過之後沒有做的是 `expected` 開關：那一欄在正式環境從來沒有被改過，寫入只發生在開票
   那一刻，而讓它可寫等於在退役之外再開一條安靜的離開分母的路——那正是
   `internal/store/store.go:1034` 明文排除的東西，也是 sampleagent3 隱形七週那個 bug 的形狀。
   這份報告不自己判斷任何事實：「報到過沒有」讀機隊清單那一份投影，「票過期了沒有」讀單機頁
   在用的同一條 SQL 判準，所以兩頁不會對同一台機器講出兩個答案。
   新增 2 條 HTML／CSV route 與 1 條 JSON route（都是 `view`）。
8. ~~**註冊限制**~~：已交付。`/machines/enrollment` 的「註冊上限」回答「這個 Hub 還收不收得下
   一台」，並且改得動那個答案。上限是一個會擋住開票的物件，不是一份報告：擋人那一次數發生在
   開票的同一筆 writer transaction 裡，而不是事後在報告上把超額的列標紅。
   它算的台數與註冊報告的分母共用同一條 SQL 判準與同一個 Go 投影，不各自算一次——兩份各自算，
   畫面上就會出現「報告說 5 台、上限說 4 台」，而那兩個數字沒有一個問得出誰是對的。
   所以撤票與票過期都不空出名額，退役才會；**sampleagent1 仍在分母裡**。
   沒有設上限不是上限 0：上限 0 是一個真的可以設的值，意思是誰都不准再納管。取消上限是一次
   upsert 不是刪列，`revision` 只往前走，才不會在設了又取消之後把一份過期的預覽重新變成有效的。
   新增 2 條 HTML route（`admin`）與 3 條 JSON route（`view` 1、`admin` 2）。
9. ~~**軟體清查**~~：已交付。`/reports/software` 回答「機隊上裝了什麼、各是哪一版、哪幾台沒
   有」。它只讀 Hub 已經在收的 `cli_tool` 觀測，不新增任何要 agent 自己說「我做到了」的欄位，
   也不把觀測改寫成判決：「沒有觀測」與「沒有安裝」是兩件事，一格「沒回報過」要去看那台的
   agent，一格「這台上沒有」要去裝東西，而兩個鐘（agent 量的、Hub 收的）都留著。
   它講得出口的只有「這個機隊裡最新的是 X，這一台是 Y」，不是「這一台該升級了」——這個 Hub
   沒有上游版本來源，所以那句限制隨報告一起送出，在網頁、terminal 與 JSON 上都印出來。
   分母與註冊報告共用同一份名冊投影；退役的機器不出現也不決定「最新是哪一版」，
   只有退役那台有過的工具整列都不在。**sampleagent1 仍在分母裡**，它那六格都是「沒回報過」。
   總覽上那張「工具版本」已改讀同一份 operator projection，`internal/web/toolmatrix.go` 已移除。
   ⚠⚠ `schema_version=2` 起每一格再答第二個問題：**那個版號，講的是不是正在跑的那一份**。
   2026-09-12 實測 5 台在籍、6 個工具、24 格有觀測，17 格裝著東西，而**沒有一格**能說出「我量版
   號的那個檔案就是正在跑的那個檔案」：4 格正在跑的是另一個檔案（四台的 openclaw 量的是
   `~/.local/bin/openclaw`，跑的是 `releases/<ver>/…/dist/index.js`）、1 格正在跑的檔案已經不在磁
   碟上（sampleagent2 的 agy 量到 1.2.2，process 跑的是 `agy.…​.old`）、3 格說不出跑的是哪一個檔案、
   9 格沒有找到在跑它的 process。那四台今天兩份的版號剛好一樣，所以畫面是對的——那是巧合。
   只有前兩種算進 `misattributed`：「我不知道」不是發現，算進去 17 格裡有 12 格會變成待辦事項。
   它跟 `shadowed` 是兩個獨立的軸（那四格 `shadowed` 都是 false），而且在摘要與下一步裡都排在版號
   不一致前面，總覽那張表上也標出來。它不回答「哪一份是 Hub 放的」——那要 agent 先把「這個檔案在我
   的 release 目錄底下」講進觀測裡。沒有新增 route。
   新增 2 條 HTML／CSV route 與 1 條 JSON route（都是 `view`）。
10. ~~**每機安裝狀態**~~：已交付。`/reports/install` 回答一句話：我叫這一台裝的那一個，跟我在
   它上面看到的一不一樣。它比的是兩件 Hub 自己知道的事——帳本上最後一筆安裝意圖，跟這台最新一
   筆工具觀測——不新增任何要 agent 自己說「我做到了」的欄位。
   **「這台最後被叫去裝什麼」取 machine 與 channel 兩個 scope 裡 revision 最大的那一筆**，因為
   revision 的計數器 key 是資源不是 scope，而 agent 的 MaxSeen 走的就是這條規則；只查一個 scope
   會得到一份看起來很乾淨但是錯的報告（第一次量就是這樣量出「4 台沒有安裝意圖」的）。
   診斷用的 `{"kind":"noop"}` 不是安裝意圖，而且要在標記「這個 scope 看過了」之前就丟掉。
   對不起來拆成五個各有下一步的答案，不併成一個「未知」；**「沒有被指派過」是一級的正面狀態**，
   不是空格。⚠⚠「指派的比看到的舊」不是失敗也不准講成落後——機隊從 `releases/<ver>/` 起動，那條
   路不經過部署，所以那句「東西可以從指派以外的路徑裝上去」隨報告送到每一個平面。
   分母與註冊報告、軟體清查共用同一份名冊投影，**sampleagent1 仍在分母裡**，它那一格是「沒有被指派過」。
   新增 2 條 HTML／CSV route 與 1 條 JSON route（都是 `view`）。

每一項照私人工作筆記裡的垂直切片規則做完才讓它出現在導覽（那些筆記不在這個公開倉庫）：
Web、CLI、API、持久化、稽核、測試、文件、部署。做完一個才開下一個。

## 6. UI／UX 對齊

版面契約在 `docs/INTUNE-UI-REFERENCE.md`。功能補齊之後才輪到它，
因為私人工作筆記的規則是（不在這個公開倉庫）：**不替不存在的工作流程畫 Intune 形狀的導覽**。
現有 85 條 HTML／CSV／下載 route 的版面一致性檢查，跟 4.1 每一列的補件一起做，不另外開一輪。
