# 控制面契約：API、CLI 與雙觀測

> 這份文件定義 AI-Intune 對「一個操作真的完成」的共同語意。UI、CLI、Hub、AWX/AAP 與 endpoint agent 都必須遵守；個別實作不能另創一套成功標準。

---

## 1. 三條產品硬規則

1. **所有 operator 能力都必須進 UI。** CLI 可以保留給自動化、除錯與 break-glass，但不能存在永久的 CLI-only 正常流程。內部 transport API 不需要變成原始按鈕；它產生的狀態、證據與錯誤必須在 UI 看得到。
2. **操作介面採 Intune 的資訊架構。** 先看 fleet headline，再按 Machines、Apps、Deployments、Updates、Reports、Settings 往下鑽；每個數字都能點到受影響的機器與證據。
3. **上游擁有通用能力，clawctl 只擁有產品語意。** Inventory、credentials、job execution、workflow、schedule、RBAC 與 job events 優先交給 AWX/AAP；clawctl 自己保留 desired state、promotion policy、AI-agent-specific observations、證據關聯與最終 verdict。

## 2. 一個執行者，兩雙眼睛

同一個 operation **只能有一個 writer / executor**。不能同時讓 Controller 與 endpoint agent 都嘗試安裝同一個 app，否則 retry、rollback、lock 與 attribution 都會產生競態。

執行結果則至少由兩個不同 failure domain 觀測：

| 角色 | 回答的問題 | 典型證據 | 能不能改狀態 |
|---|---|---|---|
| 執行端 | 我收到並執行了什麼？ | upstream job ID、exit code、event stream、artifact digest | 只有它可以 |
| 獨立 actual-state verifier | target 現在實際變成什麼？ | installed version、process argv、service unit、probe、applied digest | 不可以，只讀；必須在 executor 之外的 failure domain |
| Hub | 兩份證據是否對得上 policy？ | correlation、freshness、quorum、promotion window | 只下 verdict |

第二雙眼睛不是補救機制。原本的 executor 必須獨立把正常工作做成功；第二觀測者用來抓出「命令成功但狀態沒變」、「舊事件誤認為本次成功」、「服務啟動後又立即死亡」等部分失敗。

### 2.1 兩種合法拓撲

- **Controller 執行，client agent 驗證：** AWX/AAP 經既有的 tailnet/SSH 執行已版本化的 automation；`clawctl-agent` 只讀本機 actual state，獨立回報。
- **Client agent 執行，Controller 驗證：** Hub 維持 outbound pull 工作單；完成後 AWX/AAP 用獨立、唯讀 credential 檢查 actual state。

`clawctl-agent` 在兩種拓撲都維持 outbound-only，不新增自己的 inbound port；Controller 只可使用機器原本就存在、受 tailnet 與上游 credential/RBAC 管理的 SSH 控制面。沒有這條既有控制面時，該拓撲就是不可用，不能偷偷開 port。

每個 deployment 在建立時選定其中一種拓撲，整個 attempt 不得中途換 writer。失去 executor 時必須 fenced、進入 unknown/manual intervention，不能偷偷由另一邊接手。

### 2.2 獨立 verifier 的身分與隔離

第二雙眼睛需要一個「不是機器 agent」的身分，Hub 才可能在帳本層面證明兩份證據不同源。
`verifiers` 表就是這個身分：

| kind | failure domain 的意義 | 註冊時的驗證 |
|---|---|---|
| `external_job_runner` | 外部自動化平臺（例如 AWX/AAP）的自由字串標籤 | 只驗長度與字元 |
| `fleet_peer_agent` | 另一台已入冊機器的 machine ID | 必須是同一份 registry 上未退役的列 |
| `hub_prober` | Hub 主機在同一份 registry 的 machine ID；Hub 主機未入冊時才是字面值 `hub` | Hub 主機已入冊時必須等於那一列，不得宣稱字面值 `hub`——否則它會通過寫入比較並驗證自己所在的那台機器 |

三種 kind 的故障域都落在 `jobs.machine_id` 同一個命名空間，所以隔離規則只有一條，
而且寫在 INSERT…SELECT 的 WHERE 裡：`verifiers.failure_domain <> jobs.machine_id`。
「peer 驗自己」與「hub prober 驗 Hub 自己」因此是同一個比較，不是兩條特例；
不符合時零列寫入，回 `403 VERIFIER_NOT_ELIGIBLE`。

Verifier credential 與 machine credential 是兩張表、兩組 hash，兩個方向都不互通，
而且 verifier 平面只有兩條 route：`POST /v1/verifications` 送證據、
`GET /v1/verification-assignments` 讀被指名的工作單。它不能領單、不能續租、
不能報 event、不能收終態。撤銷保留 registry 列與它寫過的每一列證據，
因為刪掉會讓既有證據無法歸屬。

獨立證據不參與工作單終態：`Store.CompleteJob` 的條件仍是「一列可信 executor 證據、
零失敗」。這是刻意的時序邊界——verifier 派工要等工作單終態才會發出；反過來要求
verifier 才能收工作單會形成無法打破的循環。

它參與的是 **stable promotion**。每一個 effective successful canary job 都必須被 operator
指派給不同 failure domain 的 active `fleet_peer_agent`，而且同一個 producer 在最新一次派工後
送齊 `openclaw.current_release`、`openclaw.gateway_http`、`openclaw.unit_state` 三條規則；三條
都通過、`current_release` 的結構化 `observed_version` 等於 canary 要部署的 exact version，且沒有
digest／release clash，才是該 target 的 `passed`。Hub 不解析 stdout 猜版號；`external_job_runner` 與 `hub_prober`
仍可產生獨立證據，但 `grants_deployment_gate=false`，不會替 stable 放行。

這個判準先經過兩種真機量測。2026-09-12 停掉 sampleagent4 gateway 時，verifier 正確回 `failed`，
同一張單的 executor 仍是 `succeeded`；重啟則量到 unit 先成為 `active/running`、HTTP 約 20 秒後
才恢復。2026-09-13 再量一次是 HTTP 於 t+15 秒恢復。因此 runner 只在 unit 已 active/running、
gateway 可觀測但尚未通過、且三條規則都可觀測時，每 5 秒重試 gateway，最多 30 秒；unit
未 active 立即回失敗，重試途中變成量不到則整張 report 不送。這個等待吸收已量到的正常啟動
窗口，不會把 inactive 或 transport failure 洗成通過。

### 2.3 獨立證據的八種 verdict

Hub 依固定優先序取第一個成立者，六種都不是通過也不是規則失敗：

| verdict | 意義 |
|---|---|
| `absent` | 沒有第二個 producer 為這張工作單寫過證據 |
| `producer_revoked` | 寫過證據的 producer 都已撤銷 |
| `digest_mismatch` | 第二個 producer 看到的 artifact digest 與工作單不同 |
| `release_mismatch` | `current_release` 通過，但結構化版號與工作單的 exact OpenClaw 版號不同 |
| `stale` | 證據都在工作單結束前送達 |
| `failed` | 至少一條規則失敗 |
| `release_unreported` | `current_release` 通過，但沒有回報結構化版號 |
| `passed` | 規則全部通過，沒有 digest clash，且結構化版號與工作單相同 |

`digest_matches_job` 由 Hub 比較，不採信 verifier 自己的結論；
`version_matches_job` 同樣由 Hub 以工作單 desired spec 的 exact OpenClaw 版號比較。
`observed_version` 只准出現在通過的 `openclaw.current_release`；舊 verifier 沒有這個欄位時明列
`release_unreported`，不能沿用 stdout 裡看似相同的字串放行。
`reported_verified_at`（verifier 自報）與 `received_at`（Hub 觀測）分開保存，
存活判定只採後者。

`total`／`rows` 計的是每一列證據；`live_producers` 計的是同一張工作單上仍有效的 distinct
`verifier_id`。同一個 verifier 一次送三條規則，或日後重新派工再送三條，只會算一個 producer；
撤銷後它的歷史列仍保留在 `total`，但不再進 `live_producers`。Deployment 每個 target 也沿用
這個 per-job 判準，不能把規則數或回報次數寫成第二雙眼睛的數量。

### 2.4 verifier 怎麼知道要驗哪一張單

Verifier 不瀏覽工作單，也不自己挑。由 operator 指名配對：一列 `verification_assignments`
（`assignment_id`／`job_id`／`verifier_id`／`assigned_at`／`assigned_by`），
`GET /v1/verification-assignments` 只回指名給這個 credential 的那幾張。
理由是 blast radius：job ID 是 16 bytes 的 crypto/rand，所以一支外流的 verifier bearer
今天幾乎是惰性的——它得先知道 job ID 才寫得進任何東西；一條「列出我有資格驗的單」
會把它變成整個機隊少一台的清單。

這張表刻意沒有 state 欄位。「做完」是由證據推導。`fleet_peer_agent` 必須由同一個 producer
在該次 `assigned_at` 後送齊三個 exact rule ID；重複同一個 rule 不足三種，兩個 producer
各送一半也不能合併。其他不授予 gate 的 verifier kind 維持「一列證據存在」即可完成派工。
因此 runner 說什麼都不能把自己的工作標成完成，也不需要 claim／lease／expiry 這一整套
會跟證據漂移的生命週期。

派工不帶指令。要驗什麼規則編譯在 verifier 自己的程式裡；一個會執行 Hub 下發指令的 verifier
不是第二個判斷，是一條用單一 bearer 控管的遠端執行管道。

發單要等工作單終態：executor 結束前產生的證據依 freshness 判準本來就是 stale，
Hub 不量產保證過期的列。派工本身可以早下，工作單頁看得到它在等什麼。

派工是 `operate`，寫入走 preview→typed 確認→`Idempotency-Key`→單一 transaction 的同一組條件。
只有 fleet-peer 派工的完整 report 會成為 stable promotion 的 input；派工本身不等於通過，
也不會改變工作單終態。

Verifier 那一側的程式是 `clawctl-agent verifier`（`5ebedf7` 起）：讀 0600 的 verifier bearer、
問 `GET /v1/verification-assignments`、用**編譯在自己程式裡**的規則量遠端、寫證據，跑一次就結束。
它不從 agent 設定繼承任何東西，也不自己排程——`6272eb5` 起在 sampleagent3 上由一個
`Type=oneshot` unit 加 `OnCalendar=*:0/15` 的 timer 週期執行，binary、unit 與憑證
都跟 managed agent 分開（agent 仍停在 `fb0e269`，沒有被這件事帶著升級）。三種「量不到」——ssh 不通、遠端缺工具、
機器不在它的 targets 檔裡——一律不寫證據，那張派工留在「等它回報」，
因為 runner 的傳輸問題不是被測機器的判決。細節與第一次真機演練見
[VERIFIER-TOPOLOGY.md](VERIFIER-TOPOLOGY.md) §4.3。

### 2.5 Stable promotion 的十一種獨立證據狀態

Job evidence 的八種 verdict 回答「這張單有哪些獨立列」；promotion gate 另外回答「目前這一次
指定的 fleet peer，是否已交出一份足以放行的完整報告」。兩者不可共用一個 enum。Gate 依固定
優先序產生十一種狀態與下一步：

| state | 現況 | 下一步 |
|---|---|---|
| `unassigned` | 沒有可授予 gate 的 fleet-peer 派工 | 指派 verifier |
| `awaiting_report` | 已指派，但尚未收到這次派工的列 | 等 verifier 回報 |
| `incomplete_report` | 同一 producer 尚未送齊三種必要 rule | 重跑 verifier |
| `producer_revoked` | 有報告，但 producer 已撤銷 | 指派 active verifier |
| `digest_mismatch` | verifier 回報的非空 digest 與工作單衝突 | 重跑 canary |
| `release_mismatch` | verifier 看到的結構化 OpenClaw 版號不是 canary 版號 | 修復後重跑 canary |
| `stale` | 完整列沒有落在工作單終態後的可用時間窗 | 重新派工 |
| `failed` | 至少一條可用規則沒有通過 | 修復後重跑 canary |
| `canary_not_succeeded` | 這台的 canary 工作單沒有成功（被排除、沒開單、失敗或未完成） | 修復後重跑 canary |
| `release_unreported` | `current_release` 沒有回報結構化版號 | 升級後重新指派 verifier |
| `passed` | 同一 active fleet peer 的三條必要規則完整通過 | 無 |

⚠⚠ 最新一次派工會要求一份新的完整報告，舊的 pass 不得自動沿用；但在有效派工後已收到的
failed、digest clash 或 release mismatch 也不能靠再派一次洗掉，必須重跑 canary。Stale 與舊版
verifier 沒回結構化版號可以在升級後，用重新派工取得的新鮮完整 report 補正。所有比較以 Hub
`received_at` 為準，並排除晚於這次 evaluation instant 的列；verifier
自報時間只展示，不授予 gate。參與上述比較的 Hub 時間必須是 UTC 秒級、以 `Z` 結尾的 canonical
RFC3339；任何一筆派工 `assigned_at`、independent `received_at` 或 job `terminal_at` 不是這個形式，
閘門一律拒絕判定，不猜測先後。Partial POST 會讓派工繼續出現在 polling 清單，而不是把「收到一列」
誤當成「完整」。

## 3. API 還是 CLI

| 情境 | 首選 | 原因 |
|---|---|---|
| Web 正常操作 | clawctl operator API | 穩定、可驗證、可做 idempotency/RBAC/audit |
| clawctl CLI 正常操作 | 同一個 operator API | CLI 是另一個 client，不是第二套 business logic |
| AWX/AAP runtime | 官方 REST API | 有結構化 job ID、events、status 與 OAuth |
| AWX/AAP 平臺設定 | 官方 Ansible collection / configuration as code | 跟上游 schema 與 migration |
| 上游只有穩定 CLI | Hub 內的版本鎖定 adapter | 對外仍暴露一致的 operator API；未知版本停寫 |
| 官方 CLI | read-only smoke test、診斷、break-glass | 如果 CLI 只是包同一個 API，它只能驗 contract，不能算獨立部署證據 |
| 無 Controller 的本機 Ansible | `ansible-runner` | 使用其 artifact/event model，不自行解析終端輸出 |

Web server 不 shell out 到 `awx` CLI。若 API 與官方 CLI 都要測，測試結果分開標成 `api_contract` 與 `cli_contract`，不能把兩個成功誤算成雙觀測。

⚠⚠ Web 表單在解析階段被拒、還組不出 canonical request digest 時，audit 仍是 operator domain
evidence，不由 Web 自行決定 action／subject／detail。Web 只交 typed rejection code、原 request key
與同一個 boundary 已驗證的 `Actor`；operator service 固定 audit shape、拒絕未列舉的 code，再以
best-effort 寫入。這種 transport rejection 不建立 idempotency receipt，也不改 business state。
第一個收斂點是 enrollment-limit malformed apply；其兩個 code 固定為
`ENROLLMENT_LIMIT_FORM_INVALID` 與 `ENROLLMENT_LIMIT_FORM_REVISION_INVALID`。第二個 typed
ownership 收斂點是 retention malformed apply；它只接受
`RETENTION_FORM_EVALUATED_AT_INVALID` 與 `RETENTION_FORM_COORDINATES_INVALID`，且不把未解析的
座標寫進 audit。第三個收斂點是 deployment malformed apply；Web 只交
`create`／`continue`／`retry`／`abandon` 四種 typed action、path target 與 verified `Actor`，operator
固定 canonical audit action、subject 與 `BAD_REQUEST` detail。無效 path target 只記為
`deployment action`，不把原始 target 寫進 audit；不列舉的 action 拒絕寫入。這個邊界不代表
Connect click audit 的既有 Web ownership 已一起遷移。第四個收斂點才是 Connect：Web 只交
`detail_unavailable`／`projection_unavailable`／`address_unavailable`／`redirected` 四種 typed outcome、
同一份 `MachineConnect` measured projection、reason 與 verified `Actor`；operator service 固定
`connect` action、machine/display/URL subject 與 safe detail，拒絕 identity 不一致或 available/URL/why
互相矛盾的 shape。這是 best-effort 的按鈕使用證據：audit 寫失敗不能擋住已量出的 redirect，直接
複製頁面上的 BAT URL 也不會留下這筆紀錄。Production Web 已沒有直接 `s.store.RecordAudit`。
restore-drill 原本另有一條
「operator 成功卻沒回 operation ID」的 Web fallback audit；Store fresh／replay 成功都只會在合法
operation 已寫入並讀回後回傳，因此該分支不可達，已直接移除而不是搬成新的相容路徑。

## 4. 一個 app update 的閉環

```text
UI / CLI
  │  create operation（Idempotency-Key + preview digest）
  ▼
clawctl operator API
  │  freeze targets + desired revision + correlation ID
  ▼
唯一 executor（Controller 或 client agent）
  │  receipt → stage → activate → executor result
  ▼
另一側的獨立 actual-state verifier
（Controller 執行 → endpoint 驗；agent 執行 → Controller 驗）
  │  evidence bound to operation/attempt/target/artifact
  ▼
Hub verdict
  ├─ applied + stable → 才能建立 stable deployment
  ├─ failed          → rollback 或停批
  └─ unknown         → 保留現場，manual intervention
```

成功必須逐層呈現，不能壓成一個綠色布林值：

1. `accepted`：控制面接受請求並固定 targets。
2. `dispatched`：唯一 executor 接到同一個 operation/attempt。
3. `executed`：執行端完成，artifact digest 與 exit status 符合。
4. `applied`：executor 之外的 verifier 證明 target actual state 符合 desired state。
5. `stable`：觀察窗內沒有 silent failure，獨立觀測仍一致。

API `2xx`、CLI exit code `0`、Ansible job `successful` 都只可能證明前幾層，不能單獨宣告 `stable`。

## 5. Canonical operator API

目標結構是 `UI adapter → operator application service ← HTTP API ← CLI`；同一個 service 再由 adapter 對接 AWX/AAP、agent protocol 與資料庫。Web BFF 不需要對自己繞一圈 HTTP，但不能另寫 validation 或 business logic；CLI 正常模式必須真的走 HTTP API，才能驗到 transport contract。除明確的 stopped-service recovery 外，不准正常流程直接改 SQLite。

本輪 Machines read slice 依這個結構提供 `GET /v1/operator/machines` 與
`GET /v1/operator/machines/{id}`：Web SSR 直接共用同一個 in-process operator service，
不 hairpin 自己的 HTTP endpoint；CLI `machines` 正常模式預設以 deterministic discovery
走 HTTP，只有明示 `--db` 才進 stopped-service break-glass。JSON 由 explicit allowlist DTO
組成，不直接序列化 Store model。Fleet list 維持不輸出 free-form judgement reason 或 agent 自報版本；
detail v5 另以明示 disclosure 公開 Hub judgement、bounded finding messages、expectation display definitions、artifact/event evidence、agent version、host identity、resources、check-ins 與 state history。
Direct read 必須先證明 shell 載入的 workload policy 與 Hub 最後發布 identity 一致。List v2 支援
exact machine/display、state/lifecycle/reporting/channel filters、1..100 limit 與 opaque cursor；
cursor 綁 filters 與首頁 registry creation ceiling。後頁排除新建 machine，但既有 machine 的 health、
channel、retire/unretire 與 membership 保持 live。每頁 health evidence 以該 response
`evaluated_at` 的 Hub receive time 為上限；它不是跨 query/table snapshot。Restore、`VACUUM` 或
stopped maintenance 後必須捨棄 cursor。List 已完成 parity；detail v5 的 judgement、expectations/artifact/events、monitor、identity/resources、
24 小時 bounded check-ins、bounded state history 與 non-expiring identity hints，以及 MachineEvidence v4 的
OpenClaw/CLI、credentials、occupancy、systemd metadata、run-summary/journal 都已 typed。Detail v3 把
received_at 固定為 liveness clock，保留 sent_at／measured_at 但不採信為存活座標；host identity 與 Tailscale IP
明示 included；expectations 保留 unconfigured/read-failed/no-applicable 三態、最多 50 條 display definitions，
artifact 與 event observations 保留 observed/decoded/invalid/read-failed、兩個時鐘與 bounded counts。Artifact
freshness probe 不讀內容也不代表 work outcome；event failure types 由 operator 宣告，observer 只解析結構化
enum 欄位、不掃 free text 關鍵字，raw content 不進 DTO。Config-path 專用欄位與 parser 欄位 excluded；artifact
path、why/error 是 bounded display text，可能含路徑且不做 path/secret 掃描。BAT connect coordinates 與 pending enrollment 明示 excluded。Judgement
由 Hub 推導，但 input 可能含未獨立驗證的 machine evidence；active judgement 必須與 fleet state 一致，retired
judgement 則保留診斷且 `affects_fleet_state=false`。Judgement reason、最多 100 筆 finding message 與 state-history
reason 都只做 bounded display，不解析、不掃 path/secret；因此文字可能保留嵌入路徑，known dedicated secret fields
則由 DTO 結構排除。Pending ticket HTML 另讀既有 typed operator contract；BAT HTML 另讀 coordinate-bearing `MachineConnect` v1 BFF projection，Machine detail HTML 已無 raw Store bridge；
Machine detail 尚未把 verifier 的 target-side actual-state 證據接成自己的 section，所以 detail
整體仍標 `△`；工作單與 stable promotion 已使用獨立 verifier。

Machine evidence v6 由 HTML detail、`GET /v1/operator/machines/{id}/evidence` 與
`machines evidence` 共用 `operator.Service.MachineEvidence`。⚠ v5 起 systemd unit 多了
`measured`／`reason`，把「systemctl 問不到」跟「systemctl 說這個 unit 不存在」分開；v6 起
CLI tool 多了 `process_scan`（`complete`／`restricted`／`unavailable`，空字串＝還沒送這一欄的
舊 agent），讓「這一輪的 process 掃描本身有沒有跑完」變成 typed 值而不是一句話。
`process_scan` 是封閉 enum 字串，跟 `path_source`／`daemon_reach` 同一類，不進 evidence text
政策表；strict client 對不認得的值整份拒收。⚠ 不准寫成 `present ⇒ process_scan == complete`：
PATH 命中就 `present=true`，跟 `/proc` 視野無關。Systemd observation 與 journal 的 producer
都是 machine bearer 下的 observer agent，但仍是不同 evidence types：systemd 只公開 bounded
unit/name、present、active/sub state、active-enter、restart count 與 agent/Hub clocks，volatile MainPID
從 Store DTO 結構即排除；`systemd_state_is_work_outcome=false`，不能把 active/running 當作工作成功。
Run summary 的 producer 是 OpenClaw：
它先寫進自己的 sqlite，再由 observer agent 在 machine bearer 下 relay；journal 的 producer 則是
observer agent 從 `journalctl` 讀取。Hub 沒有獨立驗證任一段，因此 DTO 固定
`independent_verifier=false`，也把 journal 已在 agent 端做 secret-shape redaction、summary 完全沒有
redaction step 分成兩個 boolean。所有 free text 只准做 invalid UTF-8/control/Cf scrub 與逐欄 byte
truncation；不准解析、掃關鍵字或從內容推導 state/outcome。

OpenClaw observation 與 CLI tool rows 也由 machine bearer 下的 observer agent 產生。DTO 分開保留
CLI/gateway/upstream/running/package JSON 等版本來源與 `version_sources_disagree`，Hub 不挑一個來源冒充
canonical version；unsupported 的 `version_raw` 採 bounded block-text，只顯示、不解析。Unit/binary/runtime/
release/DB 的專用 host path 與 process PID 不存在於 Store/API DTO，只公開存在性、數量、可寫性、restart、
各段 clocks，以及 agent 先由 path 座標推導的 `same|different` relationship。這些 relationship 不是 Hub
重建路徑；bounded reason/raw text 也不做 path、PID 或 secret 掃描／遮罩，契約必須明說這個限制。

Credential rows 同樣由 observer agent 在 machine bearer 下產生，status 保留六個 enum 詞與 verification
method；`configured` 只代表本機檔案的到期時間未到，不是 session validity。DTO 保留 bounded note/error、
file/refresh/expiry clocks、Hub 觀測 clocks 與 append-only refresh history，但 active account 只回
`active_account_selected`／count，不回識別碼；token、hash 與其他 secret-bearing fields 不存在於 Store/API DTO。
同 provider 的 peer 只證明另一台曾自動續期，不證明兩台是同一張票，且每列最多六個 peers。

Occupancy rows 只從 Hub append-only `ticket_occupancy_observation` 聚合真的完成回合，不讀 process 或憑證檔。
OpenClaw task log 是 producer、observer agent 是 relay、Hub 是 aggregator；provider/source 保留上游原文不正規化，
error 只計數不分類。Profile ID、session/occupant evidence、job/model、token 與 raw error 都不進 DTO；每個
provider/source 最多列六個 agent 並回 exact total/truncated。最新 OpenClaw DB scan 另列
`rows_seen`／`rows_without_provider` 與 agent/Hub clocks，格式不可信時用 `observation_invalid`，不把缺口補成猜測。

這份 DTO 固定四條不能合併的語義：(1) `lines_are_lower_bound` 是 agent 撞到 source line cap，
`EvidenceText.truncated` 是 Hub 撞到該欄 byte cap；(2) `read_failed=true`＋`err` 是沒有聽到，與
成功讀取但 `lines=0` 的 quiet 不同；(3) `units_without_journal` 是這一輪未收集，不是 quiet，已有 bytes
但 payload 解不開則另計 `undecodable`；(4) OpenClaw `status=ok` 只代表回合結束並產出文字，DTO 固定
`status_is_outcome=false`、`terminal_outcome_recorded=false`。OpenClaw observation 與 DB section 的
存在性另以 `observed`／`db_observed` 分開，所有列表回 pre-limit truthful total/truncated。

Devices > Lifecycle 也已落在同一結構：`GET /v1/operator/machines/{id}/lifecycle`、
`POST /v1/operator/machines/{id}/lifecycle-preview` 與
`PUT /v1/operator/machines/{id}/lifecycle` 共用 `operator.Service` 與唯一 canonical Store
writer；Web 新增可發現的 `/machines/lifecycle` submenu 與 preview/confirm 頁，舊
retire/unretire POST 只保留為同一 apply 的穩定 adapter。CLI `machine lifecycle` 正常模式經
deterministic discovery 走 HTTP，只有明示 `--db` 才能在 Hub 完全停止時直接呼叫同一 service。
Read 回 `active|retired`、獨立 `lifecycle_revision` / `ETag`、分母、channel、保留
credential/pending-ticket 的存在性與當下是否允許驗證／兌換，不回 legacy `expected` 或 secret/hash。
Preview 綁定這些影響、exact revision 與非終態 job 數；它另回
`open_agent_session_count`，只表示這台機器在 preview 時開啟的終端工作階段數。這個數字不進
impact、preview digest、apply response 或 immutable receipt，工作階段開關不會讓被退役者把
preview 刷成 stale。active→retired 有任何非終態 job 時以 `nonterminal_jobs` blocker 拒絕，
因為退役後 agent 會立即失去身分驗證。

Retire 閣住 machine bearer 驗證與未過期 pending enrollment-ticket 兌換，並結束套用時仍開啟的
終端工作階段；preview 顯示的數字不承諾套用時恰好結束同樣數量。它保留
registry row、history、channel、credential 與 ticket。因此 restore active 也是高風險變更：保留的
bearer 可能立即重新通過驗證，未過期票券可能重新兌換；兩方向都必須有
reason、exact display-name confirmation、preview digest、expected lifecycle revision 與
`Idempotency-Key`。真實 transition 將 projection/revision、append-only lifecycle event、immutable
receipt 與原始 structured audit 同 transaction commit；no-op 只留 receipt/audit，不增 revision
且不建 event。成功與拒絕 replay 都使用原判決，不重新解釋後來狀態。這份
契約有 Store/service/API/strict-client/Web/CLI 合約測試，並已由 `c91695b` live 驗收（實機紀錄是私人工作筆記，不在這個公開倉庫）。

Devices 的「重新命名」是 **Hub 名冊顯示名稱** mutation，不是遠端 hostname mutation。
`POST /v1/operator/machines/{id}/display-name-preview` 與
`PUT /v1/operator/machines/{id}/display-name` 以 immutable machine ID 鎖定目標；preview digest
再綁目前名稱、新名稱與固定影響，apply 要操作員逐字確認目前名稱。套用前若目前名稱已變，
就以 stale preview／confirmation mismatch 拒絕，不把新的現在式重新解釋成舊決定。

⚠⚠ 顯示名稱同時是 expectation 規則的比對 key，也是未兌換 enrollment ticket 的 metadata。
所以成功改名後，名稱型 expectations 會自然改以新名稱匹配；這個影響必須在 Web／CLI／JSON
preview 明說。所有綁定同一 machine ID、尚未兌換的 ticket 標籤則在同一 transaction 更新，
token plaintext/hash 都不改，否則既有兌換路徑會因 ticket 名稱與 registry 名稱不一致而拒絕。
名冊名稱、pending-ticket 標籤、immutable idempotency receipt 與 structured audit 缺任何一項都整筆
rollback。machine ID、machine bearer、agent、機器 hostname、channel 與 active/retired lifecycle
全部不變；active 與 retired registry row 都可重新命名。

Devices 的「編輯名冊備註」是另一條 Hub-only mutation。備註只存在
`machine_registry.notes`；不會轉成 machine configuration，不會派 job，也不要求 agent
報回。preview digest 綁 immutable machine ID、目前 display name、舊備註、新備註與
固定影響；apply 再要 exact current-name confirmation、reason、`Idempotency-Key`，並以
舊備註 CAS 擋住並行更新。空字串是明確的清除意圖；沒有帶 `--set` 不能被 CLI
解讀為清除。active 與 retired row 都可更新。

⚠⚠ 備註是人工 free text，audit 與 immutable idempotency receipt 必須證明「誰在何時因何
更新／清除」，但不複製備註內容。receipt 只存更新前／後是否有備註；
audit detail 只存具名動作與不變的影響。canonical request digest 仍綁定 exact 內容，
因此同 key 換內容會衝突，而成功 replay 不需把 free text 放進永久 receipt。

Jobs read slice 同樣落在 `UI adapter → operator service ← HTTP API ← CLI`：新增
`GET /v1/operator/jobs`、`GET /v1/operator/jobs/{id}` 與 global HTML `GET /jobs`；Web SSR
共用 in-process service，`job list`／`job show` 預設以 deterministic discovery 走 HTTP，只有
明示 `--db` 才能進 stopped-service fence。List cursor 綁定 filters 與第一頁的 per-traversal
creation ceiling，因此不會在後頁納入之後插入的 job；這仍是 **creation-ceiling live keyset**，
不是 snapshot。既有列的 state、lease、event/verification counts 可在翻頁間改變，state-filtered
membership 也可能跨頁變動。Cursor 只適用於同一 ledger generation 的連續 traversal；stopped
maintenance、restore 或手動 `VACUUM` 後必須捨棄舊 cursor、從第一頁重來，因為 hidden rowid
不是 durable generation token。

Operator JSON 使用獨立 safe projection：desired 只回 identity/scope/resource metadata，event
只回 sequence/phase/time，verification 只回受限結果 metadata；desired spec、raw event payload、
verifier command、stdout/stderr、free-form detail 與 lease token 一律省略，而且 safe Store query
不選取 raw content 欄位。Agent event payload／verification excerpts 寫入上限為 64 KiB，verification
command 為 16 KiB。HTML job detail 不再有 raw Store-model bridge：desired spec、verifier
rule/command/output 與從 raw event payload 解出的 rejection code/detail 由 `operator.Service.JobEvidence`
的 `schema_version=6` typed evidence DTO 揭露（scrub、每欄依自報 `max_bytes` 截斷、bytes/truncated/issues、每段 1..100 筆），
`GET /v1/operator/jobs/{id}/evidence`（exact `view`）與 `job evidence` 回同一份；raw payload 只回 byte 數。
Event 與 rejection producer 可為該 machine 的 executor agent（machine bearer 租約），或 Hub scheduler
依 dependency graph 產生的 `DEPENDENCY_FAILED`；agent callback 不接受這個 Hub-only code。
新 event/verification row 原子保存 producer ID/kind、role、authority 與 row provenance；verification 另保存
`provenance_recorded=true` 與 Hub received_at；migration 不替 legacy row 造收件時間，後者維持
`provenance_recorded=false`／`received_at=null`。`MarkSucceededIfVerified` 也不接受 verifier-role-only evidence
取代 executor evidence。DTO 另有 `independent` 段（八種 verdict、verifier 身分、兩個時鐘、
Hub 算出的 `digest_matches_job`／`version_matches_job`，以及 expected／observed version），
`independent_verifier` 由該單 live producer 數算出來而不是常數。
live ledger 已有 sampleagent3 的 `fleet_peer_agent` 以另一組 bearer、另一台主機與另一條 SSH 路徑
產生的獨立 verification；executor provenance 仍是原 machine bearer，兩段不互相取代。
Noop diagnostic 已是 canonical operator operation：Diagnostics Web、兩條 operator JSON mutation
與 `job create --kind noop` HTTP CLI 共用 `operator.Service.CreateDiagnosticNoop`。Preview 綁 machine
lifecycle、active-job occupancy、OpenClaw revision 與固定 `{"kind":"noop"}` spec；apply 以 typed
display name、reason、preview digest 與 idempotency key 原子建立 desired state、job、receipt 和 audit。
Agent claim/lease/event/verification/complete/reject callbacks 的 per-machine bearer 邊界不變。

第九個 Devices 遠端動作選「同步裝置資料」，不把 noop 改名。第一個最小切片只建立 fixed
`device-sync` v1 executor 與 capability handshake：spec 不含 command/path/URL/free text，resource 固定
為 `device/sync`、`irreversible=false`、timeout 1～300 秒。Agent 在 check-in 明示
`device_sync_v1=true`，Hub 綁定 exact check-in 保存並從同一筆 latest check-in 回 readiness receipt；舊 agent
維持 null，絕不從 version 字串猜。Executor evidence 只說同步要求已接受，Hub 確認工作單終態後才
排既有 observation loop；它不等於新 observation 已抵達。公開 preview/apply 與 Devices 入口尚未
建立，因此現在不會向舊 agent 產生這種 job。威脅模型、回復與後續啟用閘見
[DEVICE-SYNC.md](DEVICE-SYNC.md)。

Deployments 現在也落在同一結構：safe list/detail、create preview/apply，以及
continue/retry/abandon 的 preview/apply 都由 `operator.Service` 擁有。Web 有 All、Active、
Paused/Stuck、Finished、New 五個 submenu；HTML adapter 不再接 raw spec/created_by。
CLI 正常模式用 deterministic discovery 呼叫相同的 10 條 JSON operations，只有明示 `--db`
才進 stopped-service fence 並直接呼叫同一 service。每個 apply 都綁 canonical lowercase
`sha256:` preview digest；既有 deployment 再以 control revision 與至少為 1 的 opened batch 做 CAS，
control revision 已到上限或 ledger 中為負值時禁止任何遞增。Deployment、desired state、jobs、
Hub event、idempotency receipt 與 audit 在一個 writer transaction commit；相同 key/body replay
使用原判決，不重新依賴今天的 artifact 或 fleet 狀態。Create apply 的 channel、version、
artifact、batch、timeout、irreversible 六個 planning fields 都必須在 JSON 明列，missing/null
或 batch/timeout 的顯式 `0` 都不能借 domain default 取得另一種 canonical spelling。OpenClaw spec artifact 與 job template
digest 在 initial create 及每次延後開批的 writer transaction 都必須一致；split-authority ledger
只可 finish/abandon，不可再長出 job。成功 replay 須對上 canonical success receipt，並以原
request digest 重新核對 typed channel/version/deployment confirmation，再驗完整 durable
deployment/job/target graph 與恰好一筆原始 success audit；Retry 還要重驗整條 terminal、material-coherent
ancestor lineage。拒絕 replay 只對 canonical rejection code/detail 與恰好一筆原始 failed audit，
不因今天的 deployment/fleet state 改變歷史判決。Corrupt/orphan cache 一律不回放。
已開 target 的 `job_id` 也必須全局單一，且 job 的 machine/desired/revision 必須與
target/deployment 一致；反向也要求 deployment 專屬 `desired_id` 下的每張 job 都恰好連回
該 deployment 的 matching target。Graph 另要求 desired-state 的 channel/resource/revision identity
仍與 deployment 相同、每個 target machine 仍存在、job state 為已知值，且 terminal state
恰好有 `terminal_at` 證據。Preview projection 與所有會開新 job 或釋放 owner 的 writer
transaction 都重驗；owner admission 也會掃描同資源所有 deployment-owned 非終態 job，
損壞、legacy 錯鏈或被 finished row 隱藏的工作都不得取得第二份執行 authority。會再開 job 的
delayed batch／Continue、Retry、promotion 與 cached-success evidence 另逐張核對 linked job
artifact digest 與 OpenClaw spec；fresh finish-only Continue／Abandon 不依賴已下架的 material。Promotion 會對
每一代 canary attempt 重驗 stored batch plan，超過 batch-size blast radius 的舊帳不得成為放行證據。

CLI 的正常 HTTP mutation 在 apply 前必須有已 fsync 的 private recovery receipt，
綁定完整 request、key、reason、CAS pointers 與正規化後的唯一 Hub authority。
`--recovery-file` 只能以原 action 原樣 replay，不接受另一組 request/transport flags；
result 全部寫出後才能刪除，否則必須保留。

Artifacts/Updates 也使用同一結構。Catalog list 是 bounded no-follow metadata inspection，
matching bytes 只能叫 `available_unverified`；artifact detail 才在已開啟的 descriptor 上完整計算
SHA-256 並回 `ready`；detail 與 deployment preview/apply 共用單目錄 hash 限流及 caller cancellation。
壞 sidecar、orphan 或被替換的 tarball 是各自的 unavailable/invalid evidence，
不能把整個 catalog 洗成零；safe projection 不回 filesystem path、upstream tarball URL 或 fetched-by
identity。Updates service 在同一 evaluation instant 組合這份 metadata-only catalog、channel members、
最後觀測版本與 Hub 收件時間、latest deployment、rollout plan 與 stable promotion gate，不另建一套
update writer；沒有 freshness threshold 的歷史 observation 不得標成 current。

Artifact intake 只接受 exact `openclaw@semver`、`node-runtime@major.minor.patch`、
`hermes-agent@major.minor.patch`、`claude-code@major.minor.patch` 與 `codex@major.minor.patch`。OpenClaw production origin 固定為
`https://registry.npmjs.org`；Node runtime 固定從 `https://nodejs.org` 讀取 official
`SHASUMS256.txt`，釘住 Linux／Darwin `.tar.gz` 與 Windows `.zip`（x64/arm64）六個 archive 後建成
deterministic multi-platform bundle；Claude Code 固定從 `https://downloads.claude.ai/claude-code-releases`
讀取 official `manifest.json` 並釘住 linux／darwin／windows amd64／arm64 六份 binary；Codex 固定從
`https://releases.openai.com/codex/releases/<version>/release.json` 讀取 official release，要求
`tag_name` 為 `rust-v<version>`，並釘住 linux musl、darwin、windows msvc amd64／arm64 六份
`codex-package` archive 與 `codex-package_SHA256SUMS`；Hermes 固定從 Docker Hub 解析 exact official image tag、釘住 upstream index digest
與 Linux amd64/arm64 完整 OCI descriptor closure，再建成 deterministic multi-platform OCI bundle。
Request 不能換 origin、redirect/proxy 或 max-bytes policy。Preview 固定 upstream
identity，apply 重讀 metadata 後才 durable enqueue；private typed source plan 只在 Store/worker
邊界以 canonical JSON 傳遞，不進 public DTO。Operation/idempotency receipt/原始 audit 同
transaction。Hub 在 READY 前只清理 stale temp 並輪替所有 running authority，不執行
upstream I/O；READY 後的單一 runtime worker 才 oldest-first reclaim running work、再持續 claim queue。
run token 會 fence 舊 worker 的 progress/terminal write，terminal write 未完成會在下一輪安全
reclaim。CLI HTTP enqueue 同樣先建立 fsync、`0700`/`0600` private recovery receipt；
5xx/transport ambiguity 只接受原 request replay，確定性 4xx 拒絕會完成 receipt cleanup。
Queued/running artifact fetch 也列入 rollback compatibility gate，不能把仍在進行的 durable intent
留給不理解這張表或 worker protocol 的舊 binary。
Apps 的概觀、OpenClaw、Store、Profiles、Assignments、Artifacts、Fetch、Operations 八個
submenu 與 detail/actions 都消費這些 native services。Store/Profile/Assignment 共用一個
artifact-aware operator service：從已驗證 material 建立固定 Node/OpenClaw/Hermes manifest，
OpenClaw 綁 exact 已發布 Node；OpenClaw 與 Hermes 互斥且同屬 `primary-agent-runtime` exclusive group。
Profile preview 重新驗證 bytes；assignment preview 解析全部 dependency graph，apply 在一個
transaction 建立 dependency-ordered desired states、jobs、edges、idempotency receipt 與 audit。
不同 profile 的 apply 建立帶 `supersedes_assignment_id` 的下一版 append-only assignment；端點 adapter
負責 OpenClaw ↔ Hermes 的 stop/disable、enable/start、runtime identity 驗證與失敗回復。
Profile assignment 與 noop diagnostic 的 `jobs_enabled` 建立閘，以嚴格 RFC3339 解析後真實時間
（instant）最新的 check-in 為準，不取 `received_at` 字典序最大的字串；可解析的 offset 形式正常
參與比較，不採 device-sync 的全歷史 canonical-strict 契約，避免不影響結果的舊列阻斷既有 live
route 的合法操作。任何一筆 `received_at` 無法解析時則一律 fail closed，因為略過它可能讓較舊的
enabled 列勝出；preview 與 apply 都拒絕且不建立任何 authority row，而 preview 仍是 autocommit
讀取，沒有新增 transaction。
Web、operator JSON、strict Go client 與 HTTP CLI 共用同一套 preview/digest/apply 契約；CLI
在 mutation 前建立 private canonical recovery receipt，ambiguous response 以原 request 精確重放。

目前 production surface 固定為
221（18 non-operator＋203 operator；107 operator JSON＋96 HTML/BFF/CSV/download），exact auth tally 是
`view=83`／`operate=24`／`admin=96`。這是由 manifest 測試固定的 contract；實機紀錄是私人工作筆記，不在這個公開倉庫。

管理中心導覽語言是 browser presentation preference，不是 fleet state。右上角是同源 form
`POST /preferences/navigation-language`，只接受 `zh-Hant`／`en`，以 HttpOnly、SameSite=Lax、
Path=/ cookie 保存；返回位置必須是單一、長度受限、不含 control/format 字元的同站 request URI。
這條 `view` route 不開 Store、不留 audit，也不建立第三種語系 fallback；它是啟動檢查唯一放行的
unsafe `view` route，cross-site POST 仍在 handler 前被拒。英文切換目前只涵蓋 product bar、
workload/resource navigation、breadcrumb 與 document title；各頁的證據與動作文案仍以
`lang="zh-Hant"` 明確標示，直到該內容完成逐句契約翻譯。

Retention Maintenance 由 Web、operator JSON、strict client 與 HTTP-first CLI 共用同一個
operator service。Status 明列 active policy、revision 與最近一次清理；preview 固定 Hub
evaluation time、秒級 policy、revision、五張表的 exact boundary/deleted/protected-newest counts
與 `DELETE N ROWS`。Apply 重新計算同一份 preview，stale scope/revision fail closed，並在一個
transaction 提交 deletion、每表 retention ledger、immutable idempotency receipt 與 structured
audit。Zero-row preview 不建立 apply action；明示 direct DB 仍須通過 stopped-service writer fence。

Audit read 由 `/audit`、`GET /v1/operator/audit-events` 與 `clawctl-hub audit [list]` 共用
`operator.Service.ListAudit`。輸出採 explicit safe allowlist；畸形時間／結果以 `null` 加 `issues`
保存證據，無效 UTF-8 與 Unicode control/format 字元替換並列出 `altered_fields`。所有 exact filters
與最後一筆 writer sequence 都綁進 opaque cursor，第一頁另固定 creation ceiling；`matched_total`
代表 denial 採樣前的符合數。預設只納入 traversal 最新 50 筆 `operator-denied`，明示
`denials=all` 才完整顯示。這是 live keyset，不是 snapshot；unsafe denial 又有最新 1000 筆
bounded ring，因此 restore、VACUUM 或 ring deletion 後不能把舊 cursor 當 durable generation token。

`action` 這個 filter 與列上的 `unknown_action` 標記都來自 `store` 的 canonical action 目錄。
**加一個新的 audit action 時，寫入面與這份目錄要一起改**：漏掉讀取面不會 compile error，
但那個動作寫下的每一列都會被標成產品認不得的證據，而且任何 filter 都找不到它
（`945e198` 修掉 `verification-assign` 的這個狀況，並由測試比對宣告與目錄）。

Changes read 由 `/reports/changes`、`GET /v1/operator/changes` 與
`clawctl-hub report changes` 共用 `operator.Service.ListChanges`。Registry create／retire／unretire
與 Hub state history 以逐筆 transition 呈現；agent observation 只做 `(from,to]` endpoint delta，
明確不聲稱中間事件序列。預設 window 24 小時、最長 30 天，所有 membership/order 使用 Hub-owned
receipt/transition time；agent `measured_at` 是分開顯示的不受信任座標。Cursor 綁 exact filters、
window、evaluation instant 與所有 append-source/retention ceilings；retention 若讓 traversal 移位就
以 `410` fail closed。Store 先下推 exact filters，篩選後 traversal 的 `total`／`matched_total` 必須
相等，kind counts 也只計該集合。每次最多檢查 250,000 筆 observation window rows、每個 selected
source 250,000 筆 timestamp metadata、10,000 endpoint keys、10,000 transitions、32 MiB candidate
metadata 與 32 MiB endpoint payload；超限 `422`、兩個 read slot 已滿 `429`、10 秒
逾時 `503`。Typed safe DTO 不直接序列化 Store payload，也不輸出 hostname/account/path/PID/argv 或
free-form note/error/reason；非公開 credential／CLI／systemd subject 一律固定遮罩且不能查詢。
systemd 的 `present` 是三態：`true` 是量到 unit 存在；`false` 必定同時帶 `measured=true`，
代表 systemctl 明確回答沒有這個 unit；`null` 同時帶 `measured=false`，代表這一輪沒拿到
systemctl 的回答（含還不會送 `measured` 的舊 agent），那一格也不輸出 `restarts`。
⚠ `present=true` 時 `measured` 一律是 null——`model.Unit` 上不存在 `Present ⇒ Measured`
的不變量，投影 `measured=false` 會把舊 agent 的零值講成「沒量到」。agent 給的原因原文
不進這個 DTO。公開 change DTO 的欄位或欄位語意改變時 `ChangeReadSchemaVersion` 必須升版
（目前 2）；cursor 綁同一個常數，所以升版會一併拒絕舊 cursor 並要求重新查詢。
Retention、registry/state transition migration 與 malformed/unplaceable timestamp coverage 都是 response
contract 的一部分；被 exact filters 排除、實際未讀取的來源明列 `not_applicable`。State current-span
projection 可在同秒覆寫，Changes 因此只讀 trigger 維護的
append-only transition event ledger；升級會回填 surviving spans，但對升級前已消失的同秒值保持 partial。

Ticket usage read 由 `/reports/tickets`、`/reports/tickets.csv`、`GET /v1/operator/tickets` 與
`clawctl-hub tickets` 共用 `operator.Service.ListTicketsContext`。查詢採 Hub `received_at` 的固定
`[from,to]` 交易快照，預設 7 天、最多 30 天，可用 SHA-256 opaque provider reference 精確下鑽。
Store 在 materialize 前固定 250,000 candidate rows 與 32 MiB evidence bytes 上限；operator DTO 再固定
100 providers、每 provider 50 machines、50 agents、5 error variants，以及 256-byte identity／2048-byte
error text。所有文字使用共用 scrub/truncation metadata，完成回合與驗證成功分開，報到率低於 80%
時調度資格為 false。Web、JSON、terminal 與 spreadsheet-safe CSV 都只讀這個 typed projection。

資料揭露由 `/tenant/data`、`/machines/{id}/data`、`/machines/{id}/data.csv`、
`GET /v1/operator/data-disclosure`、`GET /v1/operator/machines/{id}/data`、`clawctl-hub data` 與
`clawctl-hub machine data` 共用同一份目錄。它回答的是「這個 Hub 手上有這台機器的什麼」：
十四類資料涵蓋二十四張存著 `machine_id` 的表，各自說出留的是什麼、誰產生的（機器自報／Hub 判定／
操作員輸入）、含不含自由文字、留多久、退役之後還剩什麼。

⚠ 目錄不自己抄任何對照表。哪些表存著機器的資料由 SQLite 的 `machine_id` 欄位回答；哪些表會被
時間清、看哪一個保留期由 `pruneJobs` 與已驗證的 FK cascade 回答；「留多久」讀的是這台 Hub 現行
的保留期。`machine_job_capabilities` 沒有自己的 Hub 時刻，它綁在 exact check-in 上並隨父列清除，
不能被講成永久保留。一個自己抄一份的揭露面會漂，而漂掉的樣子是產品承諾了一個它不會遵守的
保留期。同一類裡的表必須全部同一種待遇——半數被清、半數留著的類別只給得出一句保留期，而那
一句對其中一半是假的，所以那種類別拒絕算出結果而不是挑一邊講。

單機那一份逐類量列數、最舊與最新，時間一律用 Hub 自己的鐘。沒有 Hub 時刻的列照樣算進列數，
但不進最舊 / 最新，也不被擺到某一個時刻上；生產庫裡真的有這種列。會被時間清的類別另外給出
現行保留期算出來的清除界線，並說明每一組最新的那一列永遠留著。揭露面只讀不寫；退役不刪任何
一列，它改變的只有「不再有新的一列」。

註冊報告由 `/reports/enrollment`、`/reports/enrollment.csv`、`GET /v1/operator/enrollment-report`
與 `clawctl-hub report enrollment` 共用 `operator.Service.EnrollmentReport`。它回答一句話：說好要
納管的機器，來了沒有。開一張票就是有人明確說出「我打算納管這台」，從那一刻起這一列就在分母裡。
名冊上的每一列剛好落在一個階段——已納管、拿了憑證沒回來、等它來、票過期沒用、沒有票可以用、
已退役——每一個階段各自附一句「這是什麼」與一句「下一步做什麼」，Web、JSON、terminal 與 CSV
講的是同一組句子。

⚠ 這份報告不自己判斷任何一件事實。「報到過沒有」讀的是機隊清單那一份 `machineSummariesFrom`
投影裡的最後一次報到；「票過期了沒有」讀的是單機頁在用的同一條 SQL 判準
`enrollmentTicketExpiredPredicate`。兩份各自算一次的話，畫面上會出現同一台機器在兩頁上有兩個
答案。解不出來的 `expires_at` 一律算過期，跟兌換路徑同一條規則。一台報到過卻沒有兌換時刻的
機器在協定上走不到，所以它讓整份報告失敗，而不是被塞進某一個階段。

離開分母只有一條路：退役。撤票、票過期都不會讓一列名冊消失——「你說要納管、但它沒來」是事實
的一部分，而把它藏起來正是 sampleagent3 隱形七週那個 bug 的形狀。名冊讀不完（超過 2,000 列）或未用票
讀不完（超過 5,000 張）時，兩邊都拒絕回一份少算的清單，因為一個少算的分母看起來就是全部。

⚠⚠ 分母只看 lifecycle：`retired_at IS NULL` 的每一列都算，`retired_at IS NOT NULL` 的每一列都不算。
Machines list 舊 schema／回應裡仍存在的 `expected` 欄位不參與判決、清單重數、dashboard、週報或 Hub
主機辨識；它不能成為第二條讓機器安靜離開分母的路。生命週期 read／preview／apply DTO 已移除該欄位，
新 immutable receipt 使用 v2；v1 receipt 仍可嚴格回放。Prometheus 只用 `clawctl_machines_total` 表示
未退役名冊分母；不再輸出與它同值、卻暗示另一套判準的 `clawctl_machines_expected`，Grafana 也只查前者。

註冊上限由 `/machines/enrollment`、`POST /machines/enrollment/limit-preview`、
`POST /machines/enrollment/limits`、`GET /v1/operator/enrollment-limit`、
`POST /v1/operator/enrollment-limit/preview`、`POST /v1/operator/enrollment-limit` 與
`clawctl-hub enrollment-limit` 共用 `operator.Service.EnrollmentLimit`／`PreviewEnrollmentLimit`／
`SetEnrollmentLimit`。它回答「這個 Hub 還收不收得下一台」，並且改得動那個答案：現在名冊上幾台、
上限幾台、還可以再納管幾台，以及到了上限要再納管該做什麼。

⚠ 上限算的台數必須逐台等於註冊報告的分母。兩邊共用同一條 SQL 判準
`enrollmentDenominatorPredicate` 與同一個 Go 投影，不是各自寫一次——兩份各自算一次的話，
畫面上會出現「報告說 5 台」而「上限說 4 台」，而那兩個數字沒有一個問得出誰是對的。因此撤票
與票過期都不會空出名額，退役才會，跟報告那一段同一條規則。

⚠ 沒有設上限不是上限 0。上限 0 是一個真的可以設的值，意思是誰都不准再納管；兩句話在畫面上
是相反的意思。取消上限是把 `limit_set` 寫成 0 的一次 upsert，不是刪掉那一列：`revision` 只准
往前走，而 `revision` 就是「你看到的還是不是現在這一份」那個判準——一個會倒退的版本號，會在
設了又取消之後把一份早就過期的預覽重新變成有效的。留著那一列也讓「誰把上限拿掉、為什麼」
問得到答案，不必去翻稽核。空白的台數欄位不准讀成 0，台數也不准 TrimSpace：一個會自己修掉
輸入的欄位，會讓 `" 4"` 跟 `"4"` 變成兩個看起來一樣、canonical request digest 卻不同的請求。

改上限走 preview/apply：preview digest digest 的是「要改成什麼」，不是「現在有幾台」——現在
有幾台是機隊隨時在動的事實，把它放進 digest 會讓一份沒有問題的確認在有機器報到時失效。
過期由 `expected_revision` 這個 precondition 擋，送出時逐字比對。上限本身不擋 preview，只有
真的開票那一次擋：那一次數發生在開票的同一筆 writer transaction 裡，因為開票會加一列名冊。

到上限時開票回 `409 ENROLLMENT_LIMIT_REACHED`，不是 400——那是機隊現在的事實，不是送錯的
request：同一份 body 在退役一台或把上限調高之後就會成立，回 400 會叫呼叫端去改一個沒有錯的
body。預覽過期回 `412 ENROLLMENT_LIMIT_PREVIEW_STALE`。每一次改上限與每一次被上限擋下的
註冊都留稽核，包含在寫進 store 之前就被擋下的那些。

軟體清查由 `/reports/software`、`/reports/software.csv`、`GET /v1/operator/software-report`、
`clawctl-hub report software` 與總覽上那張「工具版本」共用
`operator.Service.SoftwareReport`。它回答「機隊上裝了什麼、各是哪一版、哪幾台沒有」，
只讀 Hub 已經在收的 `cli_tool` 觀測，不新增任何要 agent 自己說「我做到了」的欄位。

⚠⚠ 這份報告最危險的地方不是算錯，是講太多。這個 Hub 沒有任何上游的版本來源：不知道
claude 最新是幾版、不知道 openclaw 今天發了什麼。所以它講得出口的只有「這個機隊裡最新的是
X，這一台是 Y」，而不是「這一台該升級了」——後面那句話需要一個我們沒有的事實。整個機隊都
落後兩個月的時候這份報告全綠，那不是 bug，那是它能誠實說出的極限，而那句限制隨報告一起送
出（`SoftwareReportCaveat`），在網頁、terminal 與 JSON 上都印出來：一張全綠的表少了那句話會
被讀成「都是最新的」。

⚠ 一格有六種互斥且窮盡的狀態，全部是正面陳述：跟機隊裡最新的一樣／比機隊裡最新的舊／比不
出來／裝了問不到版號／這台上沒有／沒回報過。最後兩種一定要分開：前者是「這台回報過，而且
說它上面沒有」，後者是「這台從來沒回報過這個工具」。兩者的下一步完全不同——一個要去裝東
西，一個要去看那台的 agent——而把兩者併成一個「沒有」，等於讓沉默看起來像一個已知的答案。
同一條規則管到那一行摘要：一個工具沒有任何一台裝著時，只有在每一台都回報過的情況下才可以
說「都沒有」，否則要把「幾台上沒有」與「幾台沒回報過」兩個數字分開講。

⚠ 分母是名冊上沒有退役的台數，跟註冊報告共用同一份 `machineSummariesFrom` 投影；機器來自
名冊而不是來自觀測，因為讓觀測決定有哪些機器等於讓「有回報的」變成分母，而一台從來沒回報
過的機器正是最該被看到的那一台。退役的機器不出現，也不決定「最新是哪一版」，而且只有退役
那台有過的工具整列都不在：留著它，機隊上每一台都會是「沒回報過」，而那一列永遠不會變綠——
沒有人少裝了任何東西。工具清單取自觀測裡出現過的 subject 聯集，不是一份寫死的表（那份表住
在 agent 那一側）。

⚠⚠ 版號一律用點分數字比大小，不可以用字串比。實測 samplehub1 的 grok 自報 1.0.3、sampleagent2 是
1.0.13——字串比會說 1.0.3 比較新，於是畫面會叫人把 sampleagent2 那台降級。有任何一段不是純數字
（`1.0.3-beta`）就算比不出來：比不出來的既不參加「誰是最新」的選舉，也不會被講成落後，因為
一個猜出來的「落後」會讓人動手改一台其實沒問題的機器。候選人要先排序再選——第一版直接
range 一個 map，於是當 map 剛好先吐出比不出來的那一個時，整列的基準會變成空的（實測 30 跑
1 敗），而偶爾錯的燈會被當成雜訊。

⚠⚠ 一格還有第二個軸：那個版號，講的是不是正在跑的那一份（`operator.ToolRuntimeOf`，
`schema_version=2`）。七種互斥且窮盡：量版號的那一份就是正在跑的那一份／正在跑的是另一個檔
案／正在跑的那個檔案已經不在磁碟上／有 process 在跑它但說不出跑的是哪一個檔案／process 偵測
沒有跑到底／沒有找到在跑它的 process／這一筆觀測沒有講 process 的事。後面四種一定要分開：「正在
跑的是另一個檔案」是一個發現；「說不出跑的是哪一個檔案」是關於 Hub 自己的事實；「process 偵測
沒有跑到底」是這一輪的量測邊界（`model.CLITool.ProcessScan` 不是 `complete`）；「沒有找到在跑
它的 process」只有在掃描確實跑完時才說得出口。⚠ 舊 agent 不送 `process_scan`，那一格仍落在
「沒有找到」——這裡被禁止改去掃 `running_reason` 的句子（`docs/PRODUCT.md` 地基二），所以機隊
升級完成以前，空值只能保守沿用今天的答案。併成一個「對不上」的話，唯一要人動手的那一種會被
埋在一堆「我不知道」裡。它跟 `shadowed` 是兩個獨立的軸，不准合成一個布林：前者問「人那條
PATH 跟 daemon 那條 PATH 解到的是不是同一個檔案」，後者問「我量版號的那個檔案跟現在活著的
process 在跑的是不是同一個」。

⚠⚠ 2026-09-12 實測（5 台在籍、6 個工具、24 格有觀測）：17 格裝著東西，**沒有一格**能說出「我
量版號的那個檔案就是正在跑的那個檔案」——4 格正在跑的是另一個檔案（samplehub1／sampleagent2／sampleagent3／
sampleagent4 的 openclaw，各自量的是 `~/.local/bin/openclaw`（sampleagent2 是 `/usr/bin/openclaw`），跑的
是 `~/.local/share/clawctl/openclaw/releases/<ver>/…/dist/index.js`）、1 格正在跑的檔案已經不在
磁碟上（sampleagent2 的 agy 量到 1.2.2，活著的 process 跑的是 `agy.1787036247195252617.old`）、3 格
說不出跑的是哪一個檔案、9 格沒有找到在跑它的 process。四台 openclaw 今天兩份的版號剛好一樣，
所以畫面是對的——那是巧合，不是保證；任何人 `npm i -g openclaw@新版`，畫面立刻變成新版，實際
在跑的還是舊 release。

⚠ 只有「正在跑的是另一個檔案」與「正在跑的那個檔案已經不在磁碟上」算進 `misattributed`
（`ToolRuntimeMisattributed`）。「說不出來」與「沒有找到在跑它的 process」不算：把「我不知
道」算成一個發現，機隊上每一格都會變成待辦事項，然後沒有人再看這個數字——實測 17 格裡有 12 格
落在那兩種上。`misattributed` 在那一行摘要與下一步裡都排在版號不一致前面：那幾格的版號量的是
沒在跑的那一份，所以拿它們去比「誰比較新」，比的是一個沒有人在用的檔案，版號不一致的判定本身
就還不能信。同一句話也印在總覽上那張「工具版本」表上——那是最多人看的一頁，一張「工具版本一
致」的表講的是一批沒有人在跑的檔案時，那一頁就變成一個讓人放心的謊。

⚠ 這一軸**不**回答「哪一份是 Hub 放的」。那句話需要知道 Hub 自己的安裝版面，而 agent 現在沒有
把「這個檔案在我的 release 目錄底下」講進觀測裡——由 Hub 去比對路徑長相等於猜。`running_exe`
對 node CLI 一律是 `/usr/bin/node`，它對「跑的是哪一份」一點資訊都沒有，所以只有
`running_script`，或者 `running_exe` 真的對得上這個工具自己的安裝路徑，才算說出了正在跑的是哪
一個檔案；對不上的 exe 算「說不出來」，不算「另一個檔案」——把一個解譯器講成第二份安裝，會叫人
去收掉一個不存在的檔案。「沒有找到在跑它的 process」也不講成「它沒在跑」：agent 把「讀不到
/proc」「視野被限制」「掃過了真的沒有」只編成一句散文，而掃字串判狀態是禁止的。

版號可以是它自己講的，也可以是從 `package.json` 讀的；後者要說清楚不是它本人講的，因為工
具壞掉的時候檔案上那個版號會跟真正跑起來的版本不一樣。分母超過 2,000 台或回報超過 200 個
工具時拒絕回一份少算的清查，跟註冊報告同一條理由。這份報告讀的是每一台最新的那一筆，而保
留期的 prune 永遠留著每一組 `(machine_id, kind, subject)` 的最新一列，所以它的保留期交代是
`newest_kept`，不是 `unbounded`，也不是一般的 retention 窗。

每機安裝狀態由 `/reports/install`、`/reports/install.csv`、`GET /v1/operator/install-report` 與
`clawctl-hub report install` 共用 `operator.Service.InstallReport`。它回答「我叫這一台裝的那一
個，跟我在它上面看到的一不一樣」，比的是兩件 Hub 自己知道的事：帳本上最後一筆安裝意圖，跟
這台最新一筆工具觀測。

⚠⚠ 「這台最後被指派裝什麼」不是「這台自己那一筆」。部署一律是 channel scope，所以一台機器
收到的東西多半根本沒有 machine scope 的那一列。正確答案是 `machine:<id>` 與 `channel:<它的
channel>` 兩個 scope 合起來、revision 最大的那一筆——而這件事成立是因為 `createDesiredStateTx`
的計數器 key 是 `resourceKind+":"+resourceID`，**是資源不是 scope**，同一個資源在整個機隊上共用
一條號碼帶。agent 用來擋跨 scope 降版的 `MaxSeen` 走的就是這條規則，所以這一頁跟機器實際收到
的那一筆不可能不一致。只查 channel 或只查 machine 都會得到一份看起來很乾淨但是錯的報告：第
一次量的時候只查了 channel，結論是「5 台裡有 4 台沒有安裝意圖」，兩個 scope 一起查才看到 4 台
其實都有、而且 3 台完全對得上。

⚠ 這條資源號碼帶上的每個 revision 必須唯一。`createDesiredStateTx` 配號後、寫入前會檢查相同
`(resource_kind, resource_id, revision)` 是否已存在；schema 的
`ux_desired_state_resource_revision` unique index 另作 backstop，連繞過共用 writer 的寫入也不能製造
同號歧義。帳本若在索引建立前已經有碰撞，這個 Hub 會拒絕開啟，不刪列、不選 winner，也不改
counter。同一個號碼若對應兩筆不同的 `desired_state`，agent 的 `MaxSeen` 與 receipt replay 都會失去
唯一可判讀的 authority。

⚠ `{"kind":"noop"}` 的診斷單不是安裝意圖。`validateDeploymentMaterial` 本來就不把它當可部署的
材料，而實測機隊上 22 筆 machine scope 有 12 筆是 noop、四台的**最後**一筆 machine scope 全是
noop。所以 `FleetInstallIntents` 在標記「這個 scope 已經看過了」**之前**就先丟掉 noop，否則一張
診斷單會把它前面那一筆真的指派整個擦掉，畫面會說這台從來沒有被指派過。

⚠ 一格有十種互斥且窮盡的狀態，全部是正面陳述：指派的跟看到的一樣／指派的比看到的新／指派的
比看到的舊／比不出來／指派的那一筆沒講版號／裝了問不到版號／指派了這台上沒有／指派了看不到
這個東西／指派了這台沒回報過／沒有被指派過。中間那五種都是「對不起來」，但它們要人做的事完
全不同——去看那一筆指派的內容、去看這台為什麼答不出版號、去看這個資源的工作單、去看這台回
報了什麼、去看 agent 有沒有在回報——所以它們維持五個不同的答案，不併成一個「未知」。「沒有
被指派過」是自己的一種狀態，不是空白：畫面上的空白會被讀成「查不到」，而這裡的事實是「還沒
有人叫它裝」。

⚠⚠ 「指派的比看到的舊」不是失敗，也不准被講成落後或裝失敗。samplehub1 上真的跑著比它的指派新
的那一版，因為四台機器都從 `releases/<ver>/` 起動，而那條路不經過部署。這個 Hub 看不到指派以
外的安裝路徑，所以那句限制隨報告一起送出（`InstallReportCaveat`），網頁、terminal、CSV 與 JSON
都印：少了那句話，一列「指派的比看到的舊」會被讀成有人亂動這台，而下一步會變成回滾一台好機
器。這一格的下一步是「決定哪一邊是對的」，不是「修好它」。

⚠⚠ 那句限制擋得住「有人從別的路徑裝了東西」，擋不住「我量的那個版號根本不是正在跑的那一
份」。所以這台回報說它上面有的那幾格再答第二個問題：`InstallRow.Runtime` 帶著軟體清查那一份
`ToolRuntimeOf` 的同一個判準與同一組句子（`other_file`／`gone_file`／`same_file`／
`unattributed`／`idle`／`unstated`），以及量版號的那個檔案與正在跑的那個檔案。只有 `other_file`
與 `gone_file` 算進 `Misattributed`／`MisattributedOn`（`ToolRuntimeMisattributed`）：把「說不出
來」也算進去，機隊上每一格都會變成待辦事項，然後沒有人再看這個數字。

⚠⚠ 這個數字在那一行字與下一步上都**排在「指派的跟看到的不一樣」前面**。正式庫 2026-09-12 的
samplehub1 就是那一格——版號量在 login copy 上，在跑的是 `releases/<ver>/` 那一份——照「指派的比看
到的舊」動手，第一件事會是把一台其實沒事的機器部署回舊版。而它跟 `MatchingOn`／`DifferingOn`
是**兩個獨立的軸，不是它們的一部分**：一台「指派的跟看到的一樣」也可以是量在一個已經被 unlink
的檔案上（正式庫上 sampleagent2 的 `agy` 就是），而那一格看起來最安全。用戶端那一邊逐格重數，而且
那幾個檔案名要對得起來：`other_file` 兩邊必須真的是兩個檔案（`SameToolFile`），說得出正在跑的
是哪一個檔案的那三種一定要講出來、說不出來的那三種一定要留白。

⚠ 版號比大小與軟體清查共用同一個點分數字比較器，理由一樣：字串比會說 1.0.3 比 1.0.13 新。觀
測到的版號可以是它自己講的，也可以是從 `package.json` 讀的，後者在每一個平面都標示出處。分母
是名冊上沒有退役的台數，跟註冊報告與軟體清查共用同一份投影；超過 2,000 台或超過 200 個資源時
拒絕回一份少算的報告。`desired_state` 不在 prune 的五張表裡，所以指派那一側沒有保留期問題；觀
測那一側跟軟體清查一樣是 `newest_kept`。

發佈與指派由 `/reports/profile`、`/reports/profile.csv`、`GET /v1/operator/profile-report` 與
`clawctl-hub report profile` 共用 `operator.Service.ProfileReport`。它回答「這份 profile 發佈了，
然後呢」。

⚠⚠ 每機安裝狀態的分母是機器，所以一份一台都沒指派的 profile 在那一頁上完全不存在。
這一頁的分母是**已發佈的 revision**，它存在的唯一理由就是把那幾列畫出來。正式庫
2026-09-12 的形狀：`machine_profiles` 1 列（`openclaw-standard` rev 1，點名 openclaw
2026.9.2），`machine_profile_assignments` 0 列，`desired_state` 22 列裡一列都沒提過
2026.9.2，五台在籍機器裡回報 openclaw 的那四台分別是 2026.6.10／2026.6.6／2026.5.26／
2026.5.20。所以正式庫上唯一那一列會是「發佈了一台都沒指派」，它點名的那一個套件版本會是
「沒有指派過也沒有看到過」——而在每機安裝狀態上，這件事一個字都看不到。

⚠ 一份已發佈的 revision 有四種互斥且窮盡的狀態：機隊上有機器穿著這一版／只有已退役的
機器身上還是這一版／這個 profile 已經發佈到更新的版本／發佈了一台都沒指派。後兩種刻意
分開——沒有人穿而有更新的 revision 是舊版本的預期樣子，沒有人穿而它就是最新的那一版才是
發現。它點名的每一個（套件, 版本）另有四種：指派過也看得到／指派過但沒有一台回報它／
沒有指派過但看得到／沒有指派過也沒有看到過。「指派過沒有」與「看到過沒有」是兩個獨立的
軸，合成一個「有沒有」會把兩個相反的發現寫進同一格：前者是工作單那一側的事，後者是有人
從指派以外的路徑裝上去的。

⚠⚠ `SeenOn` 只回答機器上有沒有那個版號的檔案，不能回答那是不是正在跑的那一份。
`SeenMisattributedOn` 因此另數 `SeenOn` 裡由共用 `ToolRuntimeOf` 判成 `other_file` 或
`gone_file` 的台數；那些機器**照樣算進 `SeenOn`**，因為檔案確實裝上去了。把它們扣掉會把一份
已經裝上去的 profile 寫成「沒有一台回報它」，接著叫人再指派一次。它也不是第五種套件狀態：
「這個 Hub 指派過沒有」與「看到的版號量的是哪一份」是兩個獨立的問題；塞回狀態會把四種狀態
拆成八種。整份報告的 `SeenMisattributed` 數的是「一份 revision × 一個套件版本」的格數，不是
台數；同一台機器可以出現在多份 profile 的同版套件裡。用戶端逐格重數這個總數，並拒收
`SeenMisattributedOn > SeenOn` 的回應。

⚠⚠ 「這個 Hub 指派過這一版沒有」讀的是整條 `desired_state` 的歷史
（`FleetIntentVersions`），不是現行意圖。現行意圖只回每個 scope 每個資源的最後一筆，拿它
回答過去式的問題，會讓一版指派過後來被蓋掉的套件顯示成從來沒有指派過。沒講版號的意圖不算
一版——一筆「指派過但沒說哪一版」回答不了「有沒有指派過這一版」。

⚠ 分母與註冊報告、每機安裝狀態共用同一份名冊投影。退役機器的觀測不算「機隊上看得到」，
但它身上的指派照樣列出來。這個 Hub 沒有上游版本來源，所以它不講「那一版是不是最新」，
那句限制隨報告一起送出（`ProfileReportCaveat`），網頁、CSV 與 JSON 都印。匯出的一列是
「一份已發佈的 revision × 它點名的一個套件版本」，不是一台機器一列——後者會讓一份一台都
沒指派的 profile 在檔案裡一列都沒有，而 0 列跟「這份 profile 不存在」在試算表裡長得一模
一樣。

一格的版號講的是哪一份安裝，由 `operator.ToolRuntimeOf` 回答。它讀的是同一筆工具觀測，
不多讀任何東西。

⚠⚠ 正式機隊 2026-09-12 的形狀：四台在籍機器上 openclaw 各有**兩份**安裝。Hub 量版號的是
人 PATH 上那一份（`path_source=login`），而真正活著的 process 跑的是
`~/.local/share/clawctl/openclaw/releases/<ver>/…` 那一份——部署放進 `releases/<ver>/`、把
unit 指過去，**刻意不碰**人 PATH 上那一份，所以兩份會一直並存。今天兩份的版號剛好一樣，
所以畫面是對的，那是巧合不是保證：任何人 `npm i -g openclaw@新版`，畫面立刻變成新版而實際
在跑的還是舊 release，每機安裝狀態會說「指派的比看到的舊」，然後有人會去回滾一台其實沒事的
機器。同一份量測裡 24 格 (機器, 工具) **沒有一格**說得出「我量版號的那個檔案就是正在跑的那
個檔案」：4 格是另一個檔案或已經被刪掉的檔案、4 格說不出正在跑的是哪一個、16 格沒有找到在
跑它的 process。

⚠ 六種互斥且窮盡，全部是正面陳述：量版號的那一份就是正在跑的那一份／正在跑的是另一個檔案／
正在跑的那個檔案已經不在磁碟上／有 process 但說不出跑的是哪一個檔案／沒有找到在跑它的
process／這一筆觀測沒有講 process 的事。後三種刻意分開——第一種是關於這台機器的事實，第二種
是「我掃過了」，第三種是「它什麼都沒說」，而把它們併成一個「對不上」會讓唯一要人動手的那一
種被埋在一堆「我不知道」裡。兩個檔案的路徑隨狀態一起出去：一句「正在跑的是另一個檔案」沒有
講出是哪兩個檔案的時候，讀的人不知道該去收掉哪一份。「量的那一份」是解開 symlink 之後那個真
的檔案，不是 PATH 上那個名字——兩個指向同一個檔案的 symlink 不是兩份安裝，而要收掉的是檔案。

⚠⚠ `running_exe` 對 node CLI 一律是 `/usr/bin/node`，它對「跑的是哪一份」一點資訊都沒有。
所以只有 `running_script`，或者 `running_exe` 真的對得上這個工具自己的安裝路徑，才算說出了
正在跑的是哪一個檔案；對不上的 exe 算「說不出來」，不算「另一個檔案」。把一個解譯器講成
「這台上的第二份安裝」，下一步會是去找一個不存在的檔案。同理，說不出來的時候那個路徑欄位要
留白——一個填著 `/usr/bin/node` 的「正在跑的檔案」會被引用。⚠ 已經被 unlink 的檔案是例外：
那是 `/proc` 講的事實，跟認不認得出是哪一份無關，所以它自己一種狀態，不因為對不上安裝路徑就
被降級成「說不出來」。

⚠⚠ agent 那一側的「找不到 process」其實是三種事實（讀不到 `/proc`／`/proc` 視野被限制／掃過
了真的沒有），而它們今天**只以句子的形式**存在（`probe.go` 的 `RunningReason`）。Hub 不准掃那
句話來判狀態（地基二），所以這一格對三種都講同一句「沒有找到在跑它的 process」，而那一句不准
升級成「它沒在跑」——後者是一個 Hub 現在證明不了的斷言，而且那三種裡有兩種的意思是「我看不
見」。要分開它們得由 agent 多講一個結構化欄位，那是自己的一刀。

⚠ 這一段**不**回答「哪一份是 Hub 放的」。那句話需要 Hub 知道自己的安裝版面，而 agent 現在沒有
把「這個檔案在我的 release 目錄底下」講進觀測裡；由 Hub 去比對路徑長相等於猜。接管（把
unmanaged 的那一份收成 managed）要等那個欄位，而且「都變 managed」是一個會重算的狀態不是一次
性遷移——人隨時可以 `npm i -g` 裝回來，一張「已遷移」的清單三天後就開始說謊。

每日早報預覽由 `GET /v1/operator/daily-report` 與 `clawctl-hub report` 共用 running Hub 的
`buildReport`。正常 CLI 不再自行開 SQLite：它從 explicit URL、環境或 `operator.json` 發現 Hub，
discovery 失敗就失敗，不回退本機 DB。因此預覽讀到的是 service process 已載入的 expectations、
釘住的 public URL 與還原演練章，而不是另一個 shell process 重建的一個相似世界。

⚠⚠ 這個 GET 只產生「現在會生成的本文」，不呼叫 `deliver`，也不寫 notification receipt；讀一次
不能吃掉今天尚未送出的早報。窗以 `since_seconds` 表達，預設 24 小時、最短 1 秒、最長 30 天，
回應同時帶 Hub 的整秒 `evaluated_at`、`since` 與 `window_seconds`。官方 client 要自己重算三者，
並拒收超過 64 KiB、沒有結尾換行或帶終端控制／格式字元的本文。明示 `--db` 的相容路徑只在 Hub
完全停止時可用，沿用 upgrade→writer locks、ledger identity、maintenance 與 exact unit stopped proof，
並要求 shell expectations 與 Hub 最後發布的 workload policy identity 相同；它不是正常 transport。

### 5.1 UI 覆蓋規則

每一個 operator API operation 都要同時具備：

- 可發現的 UI 入口；
- preview/impact/不可逆警告；
- operation、target、actor、time、request digest 與結果的 audit；
- 同一頁可追 executor 與另一 failure domain 的 verifier evidence；
- 錯誤時的 retry、abandon 或 manual-intervention 路徑；
- 對應 CLI client，供 automation 與 break-glass 使用。

高風險操作不是藏回 CLI，而是在 UI 加權限、typed confirmation、preview 與 maintenance mode。

唯一的例外是「故障域尚未挑定」的能力：verifier 註冊／撤銷六條 route 目前只有 JSON API
與 CLI，沒有導覽入口。加一個還沒有人可以用來完成任何事的選單，是暴露半成品，不是 parity。
第一個 verifier 真的派上線時，入口與這段一起補。

### 5.2 寫入共同條件

所有 mutation 必須有：

- `Idempotency-Key` 與 canonical body digest；同 key 不同 body 回 conflict；
- preview 產生的 `preview_digest`；targets/policy 改變時執行回 precondition failed；
- 全鏈路 `correlation_id`、per-target `attempt_id` 與 executor job ID；
- request actor、來源（UI/CLI/automation）、Hub receipt time 與 audit event；
- optimistic concurrency / revision guard；
- 一次性 secret 只在 fresh response 出現一次，replay 完全不含該欄位（不是遮罩，是欄位不存在），
  且只提供「撤銷後重建」這一條復原路徑，沒有 reissue；
- 明確 timeout、cancel、retry 與 fencing 語意。

### 5.3 證據共同欄位

每份 verification evidence 至少包含：

- `evidence_id`（可去重）、`operation_id`、`attempt_id`、`target_id`；
- `producer_id`、producer type、evidence role（executor / verifier）、adapter/version 與 credential scope；
  獨立證據的 producer 另存 verifier ID、kind、display name、failure domain 與撤銷狀態；
- expected value、observed value、artifact/config digest；
- target observed time、Hub received time、freshness limit；
- rule ID/version、command/probe identity、exit code 與受限輸出摘要；
- verdict (`pass` / `fail` / `unknown`) 與 failure class。

同一 sequence number 若 body digest 不同必須拒絕為 replay conflict；不能靜默保留第一份。

## 6. 上游與自寫邊界

| 能力 | System of record | clawctl 保存什麼 |
|---|---|---|
| Inventory / hosts / groups | AWX/AAP | upstream ID、machine mapping、產品額外 metadata |
| Credentials / execution environments | AWX/AAP | reference 與 capability，不複製 secret |
| Job templates / workflows / schedules | AWX/AAP | template mapping、policy binding、correlation |
| Runtime jobs / events | AWX/AAP | normalized summary、upstream link、必要證據 hash |
| Endpoint actual state | clawctl observations | 原始受限證據、freshness、producer identity |
| Desired state / channel / revision | clawctl | 完整產品狀態 |
| Canary promotion / silent-failure gate | clawctl | policy、判決與證據 |
| UI navigation / fleet verdict | clawctl | Intune-like view model |

Webhook 只用來加速更新；定期讀官方 API 才是 convergence truth。不得讀寫 AWX/AAP 私有資料庫，也不 fork 它的 UI。

### 6.1 UI 的上游邊界

- [AI Solutions Intelligence Dashboard](https://github.com/microsoft/AI-Solutions-Intelligence-Dashboard) 與 [AI-in-One Dashboard](https://github.com/microsoft/AI-in-One-Dashboard) 是 Power BI 分析產品；只借它們的 KPI 與 drill-down，不把 Power BI 當 control plane。它們在資料缺漏時可能只留下空白 visual；`unavailable ≠ zero` 並明標 unknown 是 clawctl 另外堅持的規則，不歸功於這兩個 repo。
- [Intune Dashboard](https://github.com/haavarstein/intune-dashboard/blob/main/docs/FEATURES.md) 是直接讀 Microsoft Graph 的 client-side portal；借它的 overview → filtered list → detail 流程，不複製它的 Intune-only data model 或 browser token 邊界。
- clawctl 維持可獨立部署的薄 SSR/BFF，不 fork AWX UI。AAP 具備官方 plugin route 時只用官方路由掛入；免費 AWX 則用同一套 standalone UI 與 API adapter。
- 不為了外觀引入第二套 SPA business logic。通用 widgets 可以跟成熟上游元件走，但 verdict、權限與 mutation 一律留在 server-side operator API。

## 7. 現況與實作順序

目前 Hub 的 JSON API 同時包含 agent transport 與已明確分類的 operator plane。Canonical
operator vertical slices 包含 machine channel、machine lifecycle、machine registry rename、enroll-token create、pending enrollment-ticket
revoke、Artifacts read/async fetch、Updates read，以及 Deployments 完整 read/control；Read plane 另有
Machines、Jobs 與 Reports Changes safe reads。Machines 的 run-summary/journal evidence 與 Jobs evidence
都由 Web/JSON/CLI 共用 service/API；noop diagnostic 也由 Diagnostics Web、JSON API 與 HTTP CLI
共用同一個 service，明示 `--db` 才走 stopped-service break-glass。
Artifacts 與 Deployments 的 Web、JSON、CLI 已共用各自的 native service，正常 CLI 不再直接開 ledger。這些已遷移的
CLI 只有明示 `--db` 才進 stopped-service break-glass；該路徑要求既有 canonical ledger、依序
取得 lifecycle（upgrade）→ writer locks、**拒絕** active/stale maintenance marker，由 systemd
證明同一 managed binary／DB 已 exact stopped，再驗完整 Phase-1 schema identity。其他尚未遷移的
standalone CLI process 仍列為 debt。現有 deployment 已具備 revision、lease/fencing、
stage/activate/rollback、artifact digest、promotion window、silent-failure gate、canonical control
transaction 與 safe presentation；verification ledger 已保存 producer/role/authority 與 Hub received_at。
第二個 producer 已在 sampleagent3 上線：`verifiers` registry、一次性 verifier bearer、以
`verifiers.failure_domain <> jobs.machine_id` 在 INSERT 內隔離的證據寫入，以及工作單讀模型的
八種 verdict（見 §2.2、§2.3）。同一組 verdict 也已經接到 deployment detail：每個已開單 target
一個 verdict，加一個由 targets 推導的 rollup，所以「這批有幾台被第二個 producer 講過」
可以在 deployment 這一層讀到。Stable promotion 另用派工、producer kind、三條必要規則、
Hub 收件時間、結構化 exact release version 與 evaluation upper bound 計算十一種 gate state；detail rollup 本身不是 gate。
工作單完成仍只採 executor 證據，stable 建立則必須讓每個 effective successful canary target
通過這個獨立 gate。

Upgrade/rollback 則在整支 lifecycle 持有 upgrade lock；Hub 停乾淨後，script 再取得 writer
lock 才能讀、snapshot、quarantine 或 restore ledger。啟動受控 Hub 前必須放掉 shell 的 writer
lock，避免和 Hub 自己的 process-lifetime lock 死結；upgrade lifecycle lock 仍由 script 持有，
而已建立的 maintenance marker 會跨過這段 handoff，讓 candidate 即使已開 DB 也只能讀，直到
六道驗收（單一 process、health、唯一 build info、完整首頁、`NRestarts=0`、writer lock 確實被 Hub 持有）
成功才 durable unlink。任何 automatic/manual recovery 也都會把 marker 保留到復原後 Hub 的驗收通過；
已啟動的 recovery 若驗收失敗，會先將 installed binary 變成不可執行、停掉並排空 process，
保留 marker 與 DB 當下原貌，不猜測哪一個 rollback coordinate 應該覆蓋可能合法的寫入。
Direct break-glass 不建立或借用這個 marker，
看到 marker 反而必須拒絕，因為它代表前一個 lifecycle 尚未安全收尾。

Flock 是合法 writer 之間的協作協定：state directory 必須保持 private `0700`，參與者不得在
持 lock 期間 rename/unlink ledger、SQLite sidecar 或 lock pathname。它不聲稱能對抗已完全控制同一 UID
的程式；這種攻擊者無須繞過 flock，本來就能直接篡改 ledger。

現況的逐項 parity 與缺口以 [API-SURFACE.md](API-SURFACE.md) 為準；那張表沒有打勾的能力，不能因為 CLI 可用就宣稱已經進了控制面。

因此依序完成：

1. Machines list、Reports > Changes、Devices > Lifecycle 與 Devices > Diagnostics parity 都已 live（`aa22597`／`3351cab`／`c91695b`／`c16d17c`）；Jobs HTML raw evidence 已改為 typed evidence disclosure（`f6cdd6d`／`bb1698a` 已 live）；Machines detail 的 machine evidence、monitor/identity/resources/history、judgement/findings 與 expectations/artifact/events 已依序收斂到 typed contract（detail v5 `7082e5d` 已 live），pending HTML bridge 由 `faa0659` 收斂到 dedicated operator contract，BAT connect bridge 亦由 `c82de07`／`713f276` 收斂到 typed BFF；下一步處理其他 CLI-only／direct-Store capability。
2. 將 Deployments 已交付的 idempotency、preview precondition、atomic audit/replay 模式套到其餘 Web／CLI mutation，並補統一 correlation。
3. Deployment evidence 的 producer/role-aware schema、sampleagent3 fleet-peer runner 與 stable promotion
   gate 已完成；工作單仍由 executor evidence 收終態，stable 才要求另一 failure domain 的完整三規則 report。
4. 接 AWX/AAP adapter；先 read-only inventory/job visibility，再接單一 executor topology。
5. 將所有 operator capability 放進 Intune-like UI，逐項做 API/UI/CLI parity 測試。

在這五步完成前，不以「又多一個按鈕」宣稱控制面完成。
