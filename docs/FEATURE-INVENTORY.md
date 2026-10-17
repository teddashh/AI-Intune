# Feature inventory：Intune-style workload / submenu parity

> 這是可維護的功能與入口帳本。它回答四件事：native capability 在哪裡、
> operator API／UI／CLI 是否有 parity、是否適合由人手動觸發，以及補齊前的依賴。
> API 的安全與成功語意仍以 [API-SURFACE.md](API-SURFACE.md) 與
> [CONTROL-PLANE-CONTRACT.md](CONTROL-PLANE-CONTRACT.md) 為準。

## 狀態與邊界

- `✅`：已有可用入口；對 operator JSON API 而言，代表 stable canonical contract。
- `△`：只有部分 read/detail/preview，或仍由 Web／CLI 直接呼叫 Store。
- `—`：尚未提供。
- 「人工 action」的 `否` 是刻意的 trust boundary，不是待補按鈕。Agent callback 只能由
  per-machine bearer 呼叫；browser/operator credential 不得代替 agent claim、續租或回報結果。
- 正常 runtime 的 mutation 應收斂到 domain service + operator JSON API。Direct-Store CLI
  只應是明示、停服且受 writer fence 保護的 break-glass 路徑。

來源總錨點：machine API 註冊於 `cmd/clawctl-hub/api.go`，operator route 與權限
manifest 位於 `cmd/clawctl-hub/operator_boundary.go`，Web routes 位於
`internal/web/web.go`、`internal/web/actions.go`，CLI dispatcher 位於
`cmd/clawctl-hub/main.go`。

## 平台涵蓋

方向（2026-09-19 私人鎖文，不在這個公開倉庫）：Linux、macOS、Windows 都是一等受管平台。只有 RDP 後排。下表是本樹程式與本地測試，不是硬體驗收。

### 兩套軟體庫

| 庫 | 鎖文規則 | 本樹現況 |
|---|---|---|
| Default（always pinned） | Claude、Codex、Grok/xAI、Antigravity 四 CLI + BAT | Claude Code、Codex、Grok、Antigravity 已有官方 fetch、standard catalog 與 adapter；Linux BAT Server 也已有釘版套件路徑。完整預設 profile 仍缺。Connect BAT 的單機連線入口與釘版 BAT Server 是不同能力 |
| Pinokio（optional） | 指派／管理時釘版；升級 check + approve；可浮動收錄 | Go catalog／adapter 沒有 `pinokio`。README 的 discovery metadata 不是已交付流程 |

收錄進 catalog ≠ 每台安裝。觀測到的 `cli_tool`（claude／codex／grok 等）是清查，不是 catalog pin。Node 的 darwin／windows 接線不代表 default 庫已交付。

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

Hub schema 已平台中立：`internal/store/schema.sql:30-31` `os/arch` TEXT 無 CHECK；`internal/catalog/catalog.go:67-71` `Platform{OS,Arch}` 無 enum；`:547` `validPlatform` 只做識別字規則，`"darwin"` 與 `"windows"` 字串都過得了。過得了字串 ≠ 有 adapter 或 installer。

## 319 個 production operations 基線

此數字按 HTTP method + route 計數：24 個 non-operator operations、295 個 operator
operations；其中 149 個為 operator JSON、146 個為 HTML/BFF/CSV/download。Operator 另按 exact
capability 分成 `view=114`、`operate=33`、`admin=148`。這組數字由 route/API/service/Web/CLI
合約測試固定；319-operation surface 包含受 operator view 保護的 Agent bootstrap download、Tailnet Settings、Maintenance、Ticket usage、資料揭露面、註冊報告、每日早報預覽、註冊上限、軟體清查、每機安裝狀態、發佈與指派對照、Hub 名冊重新命名及名冊備註、管理中心導覽語言偏好，
以及只開兩條 route 的 verifier transport；
這是 repo 的 route manifest 基線，live binary 的實機紀錄是私人工作筆記，不在這個公開倉庫，兩者在部署前本來就可能不同。
新增、移除或改 method 時，須同步更新本節與 route manifest 測試。

### Machine 與 verifier transport、telemetry（24）

| # | Method / route | Native source | 人工 action | Operator 呈現方向 |
|---:|---|---|---|---|
| 1 | `POST /v1/enrollments` | `cmd/clawctl-hub/api.go` | 否；agent 兌換一次性票券 | Enrollment 顯示票券與 machine 結果，不替 agent 兌換 |
| 2 | `POST /v1/checkins` | `cmd/clawctl-hub/api.go` | 否；agent callback | Device > Monitor 顯示 freshness、agent instance 與版本 |
| 3 | `POST /v1/observations:batch` | `cmd/clawctl-hub/api.go` | 否；agent callback | Device inventory / discovered apps / health evidence |
| 4 | `GET /v1/jobs/next` | `cmd/clawctl-hub/jobs.go` | 否；agent polling | Jobs queue 與每台 machine 的 read-only 狀態；帶 resource kind/ID 供 Agent 選定獨立 revision 水位；合規性動作生效中的機器回 `204` |
| 5 | `GET /v1/agent/readiness` | `cmd/clawctl-hub/api.go` | 否；bootstrap verification | 回傳 authenticated machine 的最新 Hub-owned check-in receipt；candidate installer 綁本次 process start、exact agent version、jobs-enabled 與 fixed `device_sync_v1` executor capability；舊 agent 該欄為 null |
| 6 | `POST /v1/jobs/{id}/claims` | `cmd/clawctl-hub/jobs.go` | 否；agent callback | Job detail 顯示 target machine 與 lease 狀態；帳本沒有另存 claimant identity |
| 7 | `POST /v1/jobs/{id}/lease:renew` | `cmd/clawctl-hub/jobs.go` | 否；agent callback | Job detail 顯示 expiry / remaining time |
| 8 | `POST /v1/jobs/{id}/events` | `cmd/clawctl-hub/jobs.go` | 否；agent callback | Job detail 顯示 phase、sequence 與時間；raw payload 不在表格中揭露 |
| 9 | `POST /v1/jobs/{id}/verifications` | `cmd/clawctl-hub/jobs.go` | 否；agent callback | Job detail 顯示 verifier evidence |
| 10 | `POST /v1/jobs/{id}/complete` | `cmd/clawctl-hub/jobs.go` | 否；agent callback | 顯示 Hub 依 evidence 收斂後的終態 |
| 11 | `POST /v1/jobs/{id}/reject` | `cmd/clawctl-hub/jobs.go` | 否；agent callback | 顯示 rejection reason；不做 operator「代拒單」 |
| 12 | `GET /v1/capabilities` | `cmd/clawctl-hub/jobs.go` | 否；protocol negotiation | Device > Monitor 顯示 protocol compatibility |
| 13 | `GET /v1/artifacts/{sha256}` | `cmd/clawctl-hub/artifacts.go` | 否；machine download | Apps > Artifacts 顯示 provenance/digest；operator fetch 另立 operation |
| 14 | `HEAD /v1/artifacts/{sha256}` | `cmd/clawctl-hub/artifacts.go` | 否；machine probe | 同上 |
| 15 | `GET /healthz` | `cmd/clawctl-hub/api.go` | 否；probe | Tenant administration > Service health |
| 16 | `POST /v1/verifications` | `cmd/clawctl-hub/verifier_api.go` | 否；獨立 verifier callback | Verifier write schema v2；通過的 `openclaw.current_release` 必須帶結構化 `observed_version`。Job evidence 的 producer 是 registry 上的 verifier，不是這台機器的 agent |
| 17 | `GET /v1/verification-assignments` | `cmd/clawctl-hub/verifier_api.go` | 否；獨立 verifier polling | 只回 operator 指派給這個 credential、工作單已終態、且這次派工尚未完成的工作；fleet peer 要同一 producer 送齊三個 exact rule ID，其他 kind 一列即可。沒有 filter、資格列舉或 Hub 下發的 rule／command／digest |
| 18 | `GET /v1/agent/terminal-link` | `cmd/clawctl-hub/agent_terminal_link.go` | 否；agent 維持終端連線 | 這台機器的 agent 以 machine bearer 接上 Hub，已開啟終端的輸入與輸出經這條連線轉送；operator 不呼叫這條 route |

Machine bearer 與 operator auth 不互相繼承；實作邊界見
`cmd/clawctl-hub/api.go`、`cmd/clawctl-hub/operator_boundary.go`。Verifier bearer 與這兩者也不互相繼承，
resolver 在 `cmd/clawctl-hub/verifier_api.go`。

| 19 | `POST /v1/disk-reports/{job_id}/chunks` | machine typed report channel | 否 | Active assigned read job, sequence/digest, 64 KiB chunks, 2 MiB total |
| 20 | `POST /v1/disk-reports/{job_id}/complete` | machine typed report channel | 否 | Canonical report digest/item-count binding; private paths never audited |

| 21 | `GET /v1/ops-manifests/{id}` | signed private disk action grant | 否；assigned machine only | Active exact job/lease/digest, consumed human approval, current policy; renewed at every mutation boundary |

| 22 | `POST /v1/disk-actions/{job_id}/results` | Machine credential | 否 | Private exact action receipts; immutable replay ledger |

| 23 | `POST /v1/channels/{id}/inbound` | Signed provider webhook | Chat commands | Disabled by default; Telegram secret / Discord signature, allowlist, canonical permissions and one-time approvals; see CHANNELS.md |
| 24 | `GET /version` | JSON | Public | Build version, commit and build time only |

### Operator HTML/BFF 與 JSON（295）

| # | Method / route | 類型 | 人工 action | 備註 |
|---:|---|---|---|---|
| 25 | `GET /metrics` | plain text | 讀取 | Requires the operator view grant. A caller without that grant, including every managed machine, receives 403. `/healthz` stays public. |
| 26 | `GET /` | HTML | 讀取 | Overview dashboard |
| 27 | `GET /machines` | HTML | 讀取 | All devices |
| 28 | `GET /machines/enrollment` | HTML | 讀取／建立入口 | Enrollment 專頁與 Tailnet 候選對照 |
| 29 | `GET /machines/{id}` | HTML | 讀取 | Device detail；judgement、expectation/artifact/event、monitor/identity/resources/history 與 machine evidence 都由 typed bounded DTO 渲染；pending metadata 讀 dedicated typed operator service；BAT coordinates 讀 `MachineConnect` v1 typed BFF，不再有 raw Store bridge |
| 30 | `GET /jobs` | HTML | 讀取 | Global Jobs list、filters 與 keyset paging；SSR 共用 in-process operator service |
| 31 | `GET /jobs/{id}` | HTML | 讀取 | Job detail；desired/verifier evidence 與 rejection detail 改由 typed bounded evidence DTO 渲染（scrub、依欄位 `max_bytes` 截斷、agent/Hub provenance），不再有 raw Store-model bridge；獨立證據分開顯示列數與 distinct live verifier 身分數，同一 verifier 的多條規則／多次重派不冒充多雙眼睛 |
| 32 | `GET /apps` | HTML | 讀取 | Apps 的概觀／OpenClaw／Store／Profiles／Assignments／Artifacts／Fetch／Operations 八個 submenu；Profiles 每一版說出它現在穿在幾台身上與那一格是什麼，Assignments 名冊上每一台說出它現在穿著哪一版，兩頁與 `/reports/profile` 讀同一份 `ProfileReport` |
| 33 | `GET /apps/artifacts/{id}` | HTML | 讀取／完整驗證 | Artifact safe detail；完整重算 SHA-256 後才可顯示 `ready` |
| 34 | `GET /apps/artifact-fetches/{id}` | HTML | 讀取 | Durable fetch operation safe detail |
| 35 | `GET /deployments` | HTML | 讀取 | Deployment list |
| 36 | `GET /deployments/{id}` | HTML | 讀取 | Deployment detail；targets 表加一欄跨故障域證據（每個已開單 target 的 verdict、rows、distinct 可用 producer），表頭一行數出「已開單的 N 台中有幾台被第二個 producer 回報通過」。沒開單的 target 留空，不給 verdict |
| 37 | `GET /updates` | HTML | 讀取／preview | 共用 native Updates model 的 artifacts、channel 成員、最近部署與 rollout gate |
| 38 | `GET /reports/tickets` | HTML | 讀取 | Ticket ledger |
| 39 | `GET /reports/tickets.csv` | CSV | 讀取／下載 | Browser export，不是 stable JSON API |
| 40 | `GET /audit` | HTML | 讀取 | Audit safe ledger；SSR 共用 operator service、完整 filters 與 creation-ceiling paging |
| 41 | `POST /machines/{id}/connect` | BFF | 是 | Redirect 到 BAT；Web 不代理遠端工作階段 |
| 42 | `POST /apps/artifact-fetches/preview` | HTML form | 是，preview | 固定 production registry 的 exact-version intake review |
| 43 | `POST /apps/artifact-fetches` | HTML form | 是 | typed package/version、idempotent durable enqueue 與 atomic audit |
| 44 | `POST /machines/{id}/retire` | HTML form | 是 | 穩定 Web apply route；現由 canonical lifecycle service 要求 preview/revision/typed confirmation/idempotency |
| 45 | `POST /machines/{id}/unretire` | HTML form | 是 | 穩定 Web apply route；與 retire 共用同一 atomic lifecycle writer 與完整確認 |
| 46 | `POST /enrollments/preview` | HTML form | 是，preview | Web adapter 呼叫 canonical operator service |
| 47 | `POST /enrollments` | HTML form | 是 | 一次性 secret；Web adapter 呼叫 canonical service |
| 48 | `POST /machines/{id}/revoke-token/preview` | HTML form | 是，preview | 顯示 exact pending-ticket identity 與固定影響 |
| 49 | `POST /machines/{id}/revoke-token` | HTML form | 是 | **只撤銷 pending enrollment ticket**，不是 active credential |
| 50 | `POST /machines/{id}/channel` | HTML form | 是 | Web adapter 呼叫 canonical channel service |
| 51 | `POST /deployments/preview` | HTML form | 是，preview | canonical create preview；固定 artifact、target snapshot、policy digest |
| 52 | `POST /deployments` | HTML form | 是 | canonical create apply；typed channel/version、idempotency、atomic audit |
| 53 | `POST /deployments/{id}/continue-preview` | HTML form | 是，preview | canonical；精確下一批或 finish outcome |
| 54 | `POST /deployments/{id}/continue` | HTML form | 是 | canonical；control revision/opened batch precondition |
| 55 | `POST /deployments/{id}/retry-preview` | HTML form | 是，preview | canonical；只列 exact terminal-failure targets |
| 56 | `POST /deployments/{id}/retry` | HTML form | 是 | canonical；建立有 lineage 的新 attempt |
| 57 | `POST /deployments/{id}/abandon-preview` | HTML form | 是，preview | canonical；列出不再開單的 targets |
| 58 | `POST /deployments/{id}/abandon` | HTML form | 是 | canonical；full-ID confirmation、terminal-job gate |
| 59 | `GET /v1/operator/machines/{id}/channel` | JSON | 讀取 | canonical；回 revision / `ETag` |
| 60 | `PUT /v1/operator/machines/{id}/channel` | JSON | 是 | canonical；typed confirmation + revision + idempotency |
| 61 | `POST /v1/operator/enrollment-tokens/preview` | JSON | 是，preview | canonical；回 policy-bound preview digest |
| 62 | `POST /v1/operator/enrollment-tokens` | JSON | 是 | canonical；明文 token 只在 fresh response 回傳一次 |
| 63 | `GET /v1/operator/machines/{id}/enrollment-token` | JSON | 讀取 | canonical；只回唯一 pending ticket metadata，不回 hash/secret；多張歧義時 fail closed |
| 64 | `POST /v1/operator/machines/{id}/enrollment-token/revocation-preview` | JSON | 是，preview | canonical；digest 綁 exact ticket 與影響 policy |
| 65 | `POST /v1/operator/machines/{id}/enrollment-token/revocations` | JSON | 是 | canonical；fresh `201`、redacted replay `200`、atomic audit |
| 66 | `GET /v1/operator/machines` | JSON | 讀取 | schema v2；safe DTO、machine/display/state/lifecycle/reporting/channel filters、1..100 paging、filter-bound registry-creation ceiling live keyset；分母只看 active/retired，legacy `expected` 不參與重數；不接受 `expected` filter |
| 67 | `GET /v1/operator/machines/{id}` | JSON | 讀取 | schema v5 typed detail；safe summary＋Hub judgement／100 findings＋最多 50 條 expectation display definitions 與 bounded artifact/event evidence＋monitor/identity/resources/check-ins/state history。保留 clocks、unknown/read-failed/invalid/truncated；artifact freshness 不讀內容、不冒充 outcome，event 只用 operator-declared enums 與 structured counts、不掃關鍵字；connect/pending excluded |
| 68 | `GET /v1/operator/jobs` | JSON | 讀取 | Jobs list read slice；filters、creation-ceiling live keyset 與 safe DTO |
| 69 | `GET /v1/operator/jobs/{id}` | JSON | 讀取 | Job safe detail；省略 desired spec、raw event payload 與 verifier command/output（typed evidence 見 85） |
| 70 | `GET /v1/operator/artifacts` | JSON | 讀取 | Metadata-only catalog list；`available_unverified` 不冒充 byte verification |
| 71 | `GET /v1/operator/artifacts/{sha256}` | JSON | 讀取／完整驗證 | Safe detail；no-follow descriptor 上完整重算 SHA-256 |
| 72 | `POST /v1/operator/artifact-fetches/preview` | JSON | 是，preview | exact OpenClaw 固定 npm origin/SHA-512/engine；exact Node runtime 固定 nodejs.org checksums 與 deterministic multiarch bundle；exact Hermes 固定 Docker Hub official image index 與完整 Linux amd64/arm64 OCI closure；exact Claude Code 固定 downloads.claude.ai manifest 與 linux／darwin／windows amd64／arm64 binary SHA-256；exact Codex 固定 releases.openai.com `release.json`、`rust-v<version>`、六份 codex-package archive 與 `codex-package_SHA256SUMS`；exact Antigravity 固定六份 latest-pointer manifest，只接受上游目前發佈的版號 |
| 73 | `POST /v1/operator/artifact-fetches` | JSON | 是 | durable enqueue；fresh/replay 均為 `202`，replay 另有 header/body evidence；idempotency/audit 同 transaction |
| 74 | `GET /v1/operator/artifact-fetches` | JSON | 讀取 | Durable operation list；state/name/version filters |
| 75 | `GET /v1/operator/artifact-fetches/{id}` | JSON | 讀取 | Operation phase/progress/result 或 bounded failure code/detail |
| 76 | `GET /v1/operator/updates` | JSON | 讀取／preview | `schema_version=5`；Artifacts、channel members、latest deployment、rollout plan，以及逐 canary target 顯示十一種獨立證據狀態與下一步的 stable promotion gate |
| 77 | `GET /v1/operator/deployments` | JSON | 讀取 | canonical safe list；channel/state/stuck filters、live keyset cursor |
| 78 | `GET /v1/operator/deployments/{id}` | JSON | 讀取 | canonical safe detail（`schema_version=3`）；targets、material summary、action eligibility；每個已開單 target 帶 `independent`（八種 verdict 之一／rows／distinct live_producers），未開單 target 為 `null`；另有 deployment 層 rollup（opened/passed targets、逐 target live producers 加總、八種 verdict 的 target 計數），由 targets 推導而非另一份真相；不回 raw spec/created_by |
| 79 | `POST /v1/operator/deployments/preview` | JSON | 是，preview | `schema_version=4`；create preview；重新驗 artifact bytes，固定 targets 與 promote verdict；stable 逐台要求 active fleet peer 在最新派工後送齊三條必要規則，且 `current_release` 的結構化版號等於 canary exact version |
| 80 | `POST /v1/operator/deployments` | JSON | 是 | create apply；fresh `201`、成功 replay `200`；拒絕 replay 保留原錯誤 status/code |
| 81 | `POST /v1/operator/deployments/{id}/continuation-preview` | JSON | 是，preview | continue/finish preview |
| 82 | `POST /v1/operator/deployments/{id}/continuations` | JSON | 是 | optimistic precondition + idempotent apply |
| 83 | `POST /v1/operator/deployments/{id}/retry-preview` | JSON | 是，preview | exact terminal-failure retry scope |
| 84 | `POST /v1/operator/deployments/{id}/retries` | JSON | 是 | 新 attempt、fresh `201`、lineage receipt |
| 85 | `POST /v1/operator/deployments/{id}/abandonment-preview` | JSON | 是，preview | terminal-job gate 與 unopened impact |
| 86 | `POST /v1/operator/deployments/{id}/abandonments` | JSON | 是 | paused → finished；full-ID confirmation |
| 87 | `GET /v1/operator/audit-events` | JSON | 讀取 | Safe allowlist audit read；完整 filters、creation-ceiling cursor 與 bounded denial sampling |
| 88 | `GET /reports/changes` | HTML | 讀取 | Changes typed safe read；SSR 共用 operator service、固定 window/ceilings、coverage 與 paging |
| 89 | `GET /v1/operator/changes` | JSON | 讀取 | Transition + endpoint-delta safe DTO；Hub clock、bounded filters/window/paging、retention/malformed disclosure |
| 90 | `GET /machines/lifecycle` | HTML | 讀取／預覽入口 | Devices > Lifecycle 可發現 submenu；顯示 active/retired 與獨立 revision，不再顯示 legacy expected 軸 |
| 91 | `POST /machines/{id}/lifecycle-preview` | HTML form | 是，preview | Web adapter 呼叫 canonical lifecycle service；retire 確認另列當下開啟的終端工作階段數，並說明套用時仍開啟者會結束；restore 不顯示這項退役影響 |
| 92 | `GET /v1/operator/machines/{id}/lifecycle` | JSON | 讀取 | Safe current projection；回 `lifecycle_revision` / `ETag`，分母只由 active/retired 決定；不回 legacy `expected` 或 credential/token hash |
| 93 | `POST /v1/operator/machines/{id}/lifecycle-preview` | JSON | 是，preview | `admin`；另回不綁 digest/receipt 的 `open_agent_session_count` preview-time fact；其餘保留／存取影響綁 exact revision；非終態 job 會阻擋 active→retired |
| 94 | `PUT /v1/operator/machines/{id}/lifecycle` | JSON | 是 | `admin`；typed name/reason、preview digest、revision、idempotency；retire 結束套用時仍開啟的終端工作階段；projection/event/receipt/audit 原子提交 |
| 95 | `GET /v1/operator/jobs/{id}/evidence` | JSON | 讀取 | `view`；`schema_version=6` typed bounded evidence：desired spec、event metadata＋payload bytes、最後一筆 rejected code/detail、verification rule/command/stdout/stderr；每欄依自報 `max_bytes` scrub/截斷（16 KiB／256／64）、每段 1..100 筆；event 與 rejection 明列 agent 或 Hub scheduler producer；新 event/verification row 明列 producer/role/authority 與 row provenance，verification 另有 Hub received_at；legacy row 明列未記錄；另有 `independent` 段：八種 verdict、verifier 的 kind／ID／display name／failure domain／撤銷狀態、`verifier_bearer` authority、兩個時鐘與 expected／observed version；digest/version match 都由 Hub 比較，stdout 不參與版號判斷；`independent_verifier` 由該單 live producer 數算出來，不是常數；不回 raw payload／lease token |
| 96 | `GET /v1/operator/machines/{id}/evidence` | JSON | 讀取 | `view`；`schema_version=6` typed bounded OpenClaw/CLI/credential/occupancy/systemd/run-summary/journal evidence；systemd unit 帶 `measured`／`reason`、CLI tool 帶 `process_scan`，把「問不到」與「確實不存在」分開；版本來源與 conflict 不合併、raw version 只顯示不解析；專用 host path／DB location／PID、credential secret/account 與 occupancy event identity 均結構性排除；各段有 clocks、truthful totals/invalid、producer/relay、redaction 差異及 strict-client coherence |
| 97 | `GET /machines/diagnostics` | HTML | 讀取／建立入口 | Devices > Diagnostics；列出名冊機器與 noop protocol drill 表單 |
| 98 | `POST /machines/{id}/diagnostic-noop-preview` | HTML form | 是，preview | `operate`；顯示 timeout、revision、heartbeat execution 狀態、active-job occupancy、固定 impact 與 blockers |
| 99 | `POST /machines/{id}/diagnostic-noop-jobs` | HTML form | 是 | `operate`；typed machine-name confirm，成功後導向 canonical job detail |
| 100 | `POST /v1/operator/machines/{id}/diagnostic-noop-preview` | JSON | 是，preview | `operate`；固定 noop spec、machine lifecycle、最新 `jobs_enabled`、active jobs、OpenClaw revision 與 preview digest；execution unknown/false 回 typed blocker |
| 101 | `POST /v1/operator/machines/{id}/diagnostic-noop-jobs` | JSON | 是 | `operate`；fresh `201`／replay `200`；atomic desired state、job、idempotency receipt 與 audit，回 Location/ETag |
| 102 | `POST /apps/store/packages/preview` | HTML form | 是，preview | `admin`；從已驗證 artifact 產生 fixed Node/OpenClaw/Hermes manifest review |
| 103 | `POST /apps/store/packages` | HTML form | 是 | `admin`；canonical token、typed package/version confirm 與 atomic publication/audit |
| 104 | `POST /apps/profiles/preview` | HTML form | 是，preview | `admin`；選取 exact packages，顯示完整解析結果 |
| 105 | `POST /apps/profiles` | HTML form | 是 | `admin`；canonical token、typed profile/revision confirm 與 atomic publication/audit |
| 106 | `POST /apps/profile-assignments/preview` | HTML form | 是，preview | `admin`；顯示 exact machine/profile、dependency order、jobs、prerequisites 與 blockers |
| 107 | `POST /apps/profile-assignments` | HTML form | 是 | `admin`；typed machine-name confirm，atomic 建立 desired states/jobs/edges/receipt/audit |
| 108 | `GET /v1/operator/catalog-manifests` | JSON | 讀取 | `view`；safe package/kind filters 與 filter-bound keyset cursor |
| 109 | `POST /v1/operator/catalog-manifests/standard-preview` | JSON | 是，preview | `admin`；full artifact verification，固定 Node/OpenClaw/Hermes manifest 與 exact dependency/conflict contract |
| 110 | `POST /v1/operator/catalog-manifests` | JSON | 是 | `admin`；typed package/version/digest confirmation，fresh `201`、replay/already-published `200` |
| 111 | `GET /v1/operator/machine-profiles` | JSON | 讀取 | `view`；safe profile filter 與 filter-bound keyset cursor |
| 112 | `POST /v1/operator/machine-profiles/preview` | JSON | 是，preview | `admin`；驗證 selected packages 並固定 canonical profile digest |
| 113 | `POST /v1/operator/machine-profiles` | JSON | 是 | `admin`；typed profile/revision/digest confirmation，fresh `201`、replay/already-published `200` |
| 114 | `POST /v1/operator/machines/{id}/profile-assignment-preview` | JSON | 是，preview | `admin`；解析 compatible dependency graph 與 exact job impact |
| 115 | `POST /v1/operator/machines/{id}/profile-assignments` | JSON | 是 | `admin`；fresh `201`／replay `200`；atomic desired states、dependency-ordered jobs、edges、receipt 與 audit |
| 116 | `GET /downloads/agent/{arch}` | download | 讀取／下載 | `view`；既有 `amd64`／`arm64` 與 Linux aliases、已發布的 `darwin-amd64`／`darwin-arm64`。每次下載重開 no-follow descriptor 並重驗版本、ELF／Mach-O 架構、archive layout、大小與 SHA-256 |
| 117 | `GET /settings/tailnet` | HTML | 讀取 | `view`；顯示 source observation time、stable peer identity、未納管／已忽略／在線無心跳／退役仍在線分類 |
| 118 | `POST /settings/tailnet/peer-ignore-preview` | HTML form | 是，preview | `admin`；固定 peer stable ID、目前 hostname、expiry、reason 與 revision |
| 119 | `POST /settings/tailnet/peer-ignores` | HTML form | 是 | `admin`；typed hostname confirmation、preview digest、revision、idempotency 與 atomic audit |
| 120 | `GET /v1/operator/tailnet` | JSON | 讀取 | `view`；canonical reconcile DTO；來源不可用與空清單分開，本地 active ignore rules 仍可讀取，所有 collections 非 null |
| 121 | `POST /v1/operator/tailnet/peer-ignore-preview` | JSON | 是，preview | `admin`；stable node ID identity、1..366 天 expiry 與 current rule revision |
| 122 | `PUT /v1/operator/tailnet/peer-ignores/{id}` | JSON | 是 | `admin`；fresh/replay 均為 `200`，replay header/body 一致；domain state、receipt、audit 原子提交 |
| 123 | `GET /tenant/maintenance` | HTML | 讀取 | `view`；顯示目前 retention policy、revision、上次清理與還原演練狀態 |
| 124 | `POST /tenant/maintenance/retention/prune-preview` | HTML form | 是，preview | `admin`；以目前 Hub policy 固定 evaluation time、revision、五張表的刪除／保留數與 protected newest evidence |
| 125 | `POST /tenant/maintenance/retention/prunes` | HTML form | 是 | `admin`；逐字 `DELETE N ROWS`、reason、revision、preview digest 與 idempotency；刪除、retention ledger、receipt、audit 原子提交 |
| 126 | `GET /v1/operator/maintenance/retention` | JSON | 讀取 | `view`；canonical policy、revision 與 never-run／zero-row-run 可區分的最新狀態 |
| 127 | `POST /v1/operator/maintenance/retention/prune-preview` | JSON | 是，preview | `admin`；可明示秒級 policy 與 evaluation time，回 exact boundaries/counts/confirmation/digest |
| 128 | `POST /v1/operator/maintenance/retention/prunes` | JSON | 是 | `admin`；fresh/replay 均 `200`；stale revision/scope fail closed，domain deletion 與證據原子提交 |
| 129 | `GET /tenant/maintenance/restore-drills/{id}` | HTML | 讀取 | `view`；顯示 queued/running/succeeded/failed、固定備份身分、結果或 typed failure |
| 130 | `POST /tenant/maintenance/restore-drill-preview` | HTML form | 是，preview | `admin`；固定最新 standalone backup 的 name/size/mtime/SHA-256、live expected 與 `VERIFY filename` |
| 131 | `POST /tenant/maintenance/restore-drills` | HTML form | 是 | `admin`；確認後 durable enqueue，導向 operation detail；不替換正式資料庫 |
| 132 | `POST /v1/operator/maintenance/restore-drill-preview` | JSON | 是，preview | `admin`；strict empty object，回 pinned backup identity、live expected、confirmation 與 digest |
| 133 | `POST /v1/operator/maintenance/restore-drills` | JSON | 是 | `admin`；durable async enqueue，fresh `202`／replay `200`，operation／receipt／audit 原子提交 |
| 134 | `GET /v1/operator/maintenance/restore-drills` | JSON | 讀取 | `view`；bounded newest-first durable operation ledger |
| 135 | `GET /v1/operator/maintenance/restore-drills/{id}` | JSON | 讀取 | `view`；safe operation detail，不回 run token、private reason 或 idempotency material |
| 136 | `GET /v1/operator/tickets` | JSON | 讀取 | `view`；固定 `[from,to]` Hub receive-time snapshot、1..30 天與 opaque provider filter；bounded/scrubbed provider、machine、agent、error evidence |
| 137 | `POST /v1/operator/verifiers/preview` | JSON | 是 | `admin`；純 preview，回 policy 與綁 kind／name／failure domain 的 `preview_digest`；只有 `fleet_peer_agent` 的 `grants_deployment_gate=true`；零列寫入 |
| 138 | `POST /v1/operator/verifiers` | JSON | 是 | `admin`；canonical；fresh `201` 只回一次 credential 明文，replay `200` 不含 credential 並回 `revoke_and_register` |
| 139 | `GET /v1/operator/verifiers` | JSON | 讀取 | `view`；active／revoked 計數與列數一致，沒有 credential 或 hash 欄位 |
| 140 | `GET /v1/operator/verifiers/{id}` | JSON | 讀取 | `view`；state 與 `revoked_at` 一致，`last_seen_at` 是 Hub 觀測到的寫入時間 |
| 141 | `POST /v1/operator/verifiers/{id}/revocation-preview` | JSON | 是 | `admin`；exact `{}`；固定影響：registry 列與既有證據都保留 |
| 142 | `POST /v1/operator/verifiers/{id}/revocations` | JSON | 是 | `admin`；revision CAS＋`Idempotency-Key`；憑證失效但列與證據保留，唯一 producer 的工作單轉 `producer_revoked` |
| 143 | `POST /v1/operator/verifiers/{id}/assignment-preview` | JSON | 是，preview | `operate`；exact `{"job_id":…}` 純讀；digest 綁兩邊識別與固定 policy（separation rule、kind-specific satisfied_by／gate、發單需終態、Hub 不下發指令），刻意不綁工作單狀態 |
| 144 | `POST /v1/operator/verifiers/{id}/assignments` | JSON | 是 | `operate`；preview digest＋逐字 `confirm_verifier_name`＋`Idempotency-Key`；派工列、receipt 與 audit 同 transaction；同域以 `409` 擋下。Fleet peer 要同一次派工後三條必要規則全到齊才算完成，其他 kind 一列即可 |
| 145 | `POST /jobs/{id}/verifier-assignment-preview` | HTML form | 是，preview | `operate`；工作單頁只列出 failure domain 不同的 active verifier；確認頁帶理由與 preview digest |
| 146 | `POST /jobs/{id}/verifier-assignments` | HTML form | 是 | `operate`；逐字 verifier 名稱確認交由 writer 在同一 transaction 比對；成功 303 回工作單頁，該頁即顯示待回報 |
| 147 | `GET /machines/configuration` | HTML | 讀取 | `view`；Devices > Configuration 盤面：已發佈原則、目前指派、每台的 effective settings、verdict 與最後回報時間
| 148 | `POST /machines/configuration/policy-preview` | HTML form | 是，preview | `admin`；純讀；回 current/next revision、canonical settings digest、preview digest 與影響機器數
| 149 | `POST /machines/configuration/policies` | HTML form | 是 | `admin`；typed policy-id 確認；同值重發不產生新 revision，`expected_revision` 不符回衝突
| 150 | `POST /machines/configuration/assignment-preview` | HTML form | 是，preview | `admin`；純讀；回目前生效的原則與指派後的值，原則或 revision 不存在回 `404`
| 151 | `POST /machines/configuration/assignments` | HTML form | 是 | `admin`；typed scope-id 確認；assignment、receipt 與 audit 同 transaction
| 152 | `GET /v1/operator/settings` | JSON | 讀取 | `view`；與 Web 盤面同一個 operator service；verdict 依 never_reported／unknown／mismatch／pending／applied 優先序取第一個成立者
| 153 | `POST /v1/operator/setting-policies/preview` | JSON | 是，preview | `admin`；零列寫入；`preview_digest` 綁 canonical settings
| 154 | `POST /v1/operator/setting-policies` | JSON | 是 | `admin`；fresh `201`、replay/unchanged `200`；policy row、receipt 與 audit 同 transaction
| 155 | `POST /v1/operator/setting-assignments/preview` | JSON | 是，preview | `admin`；零列寫入；回這個範圍涵蓋的機器數
| 156 | `POST /v1/operator/setting-assignments` | JSON | 是 | `admin`；scope 只接受 machine（須存在且未退役）或 channel；同 digest 不產生新 assignment revision
| 157 | `GET /machines/compliance` | HTML | 讀取 | `view`；Devices > 合規性 盤面：四個判決計數、已發佈原則與它們不符合時會做什麼、目前指派，以及每台的判決、推出它的每一條規則結果，與動作現在做到哪一步（未觸發／寬限中／生效中）；有原則帶動作時多一行被停發工作單的台數
| 158 | `POST /machines/compliance/policy-preview` | HTML form | 是，preview | `admin`；純讀；規則來自一個規則種類一列的 checkbox，動作也是一種一列（checkbox ＋寬限秒數），重複規則或重複動作在這個平面表達不出來
| 159 | `POST /machines/compliance/policies` | HTML form | 是 | `admin`；typed policy-id 確認；同一組規則與動作換順序寫仍是同一份，不產生新 revision；確認頁無條件帶回每個動作的寬限秒數，因為 0 是「判定不符合就立即生效」而不是沒填
| 160 | `POST /machines/compliance/assignment-preview` | HTML form | 是，preview | `admin`；純讀；回目前生效的原則與指派後的規則與動作，原則或 revision 不存在回 `404`
| 161 | `POST /machines/compliance/assignments` | HTML form | 是 | `admin`；typed scope-id 確認；assignment、receipt 與 audit 同 transaction
| 162 | `GET /v1/operator/compliance` | JSON | 讀取 | `view`；判決與動作狀態現算不落表，回應帶判決時間；每台連同每一條規則的 pass／fail／unmeasured 與每個動作的狀態一起回，另回被停發工作單的台數
| 163 | `POST /v1/operator/compliance-policies/preview` | JSON | 是，preview | `admin`；零列寫入；`preview_digest` 綁 canonical rules
| 164 | `POST /v1/operator/compliance-policies` | JSON | 是 | `admin`；fresh `201`、replay/unchanged `200`；policy row、receipt 與 audit 同 transaction
| 165 | `POST /v1/operator/compliance-assignments/preview` | JSON | 是，preview | `admin`；零列寫入；回這個範圍涵蓋的機器數
| 166 | `POST /v1/operator/compliance-assignments` | JSON | 是 | `admin`；scope 只接受 machine（須存在且未退役）或 channel；同 digest 不產生新 assignment revision
| 167 | `GET /v1/operator/machines/{id}/actions` | JSON | 讀取 | `view`；`schema_version=5` 的裝置動作目錄。每個動作帶一個封閉的 `surface`（`operator-api`／`web`）；`web` 的動作沒有 operator API 路徑，`method`／`path` 缺席而不是填一條做不到那件事的路徑。九個具名動作中 retire／restore 只出現會改變狀態的一個方向，因此這台的指派使用者在 Linux 機器上最多八列，其他人最多七列。`connect` 與 `open_terminal` 是兩個 `surface=web` 的動作，`open_terminal` 只出現給這台機器的指派使用者，而且只出現在 Linux 機器上；每個動作的可用與否直接取自守住它寫入路徑的 preview/read contract，目錄不自己再判一次；重新命名與編輯名冊備註對 active／retired 都可用，輸入由各自專用 preview 判；只列出 principal 握有 capability 的動作
| 168 | `GET /reports` | HTML | 讀取 | `view`；報告落地頁。每一份報告在點進去之前就說出自己回答什麼、範圍多大、看得到多遠、能不能整份匯出；「看得到多遠」取現行保留期與該報告單次讀取上限的較小者；只有在範圍裡完整的報告才給匯出連結
| 169 | `GET /v1/operator/reports` | JSON | 讀取 | `view`；同一份報告清單的 canonical DTO；不接受任何 query parameter
| 170 | `GET /machines/{id}/timeline` | HTML | 讀取 | `view`；單機事件時間軸。名冊、健康判定、工作單與操作員動作由新到舊排成一條，每一列都來自那一頁自己在用的讀取器；每個來源各自交代讀到幾列與有沒有讀完；1..30 天，預設 7
| 171 | `GET /machines/{id}/timeline.csv` | CSV | 讀取／下載 | `view`；匯出的就是畫面上那些列，一列不多一列不少；檔名帶 machine_id 而不是顯示名稱；走與票證匯出同一份 CSV 契約（BOM、UTC RFC 3339 奈秒時間、逐格試算表公式防護）
| 172 | `GET /v1/operator/machines/{id}/timeline` | JSON | 讀取 | `view`；同一條時間軸的 canonical DTO；窗的兩端取整秒以對齊稽核的時間篩選，只接受 `days`
| 173 | `GET /tenant/data` | HTML | 讀取 | `view`；資料揭露面。十四類資料各自交代留的是什麼、誰產生的、含不含自由文字、留多久、退役之後還剩什麼、去哪一頁看；「留多久」讀的是這台 Hub 現行的保留期，而「會不會被清」讀的是真的在刪東西的那張 prune 表
| 174 | `GET /machines/{id}/data` | HTML | 讀取 | `view`；單機的留存量。同一份目錄加上這台實際的列數、最舊與最新、現在的清除界線；沒有 Hub 時刻的列照樣算進列數，但不進最舊 / 最新
| 175 | `GET /machines/{id}/data.csv` | CSV | 讀取／下載 | `view`；匯出的就是畫面上那些類，一類不多一類不少；走與票證、時間軸同一份 CSV 契約（BOM、UTC RFC 3339 奈秒時間、逐格試算表公式防護）
| 176 | `GET /v1/operator/data-disclosure` | JSON | 讀取 | `view`；`schema_version=1` 的揭露面 canonical DTO；不接受任何 query parameter
| 177 | `GET /v1/operator/machines/{id}/data` | JSON | 讀取 | `view`；同一份單機留存量的 canonical DTO；不接受任何 query parameter
| 178 | `GET /reports/enrollment` | HTML | 讀取 | `view`；註冊報告。說好要納管的機器來了沒有；名冊每一列剛好落在一個階段（已納管／拿了憑證沒回來／等它來／票過期沒用／沒有票可以用／已退役），各自附「這是什麼」與「下一步」；還沒到的排在最上面，等最久的排最前面；不接受任何 query parameter
| 179 | `GET /reports/enrollment.csv` | CSV | 讀取／下載 | `view`；匯出的就是畫面上那些列，一列不多一列不少；走與票證、時間軸、資料揭露同一份 CSV 契約（BOM、UTC RFC 3339 奈秒時間、逐格試算表公式防護）
| 180 | `GET /v1/operator/enrollment-report` | JSON | 讀取 | `view`；`schema_version=1` 的註冊報告 canonical DTO；不接受任何 query parameter；名冊超過 2,000 列時拒絕回一份少算的分母
| 181 | `POST /machines/enrollment/limit-preview` | HTML | 是 | `admin`；改註冊上限的 review 頁。現在的上限、要改成什麼、名冊上幾台三張卡一起講；空白的台數欄位拒收，不讀成 0；比目前台數小的上限明說它退役不了任何一台
| 182 | `POST /machines/enrollment/limits` | HTML | 是 | `admin`；confirm。要求 preview digest、`expected_revision` 與 idempotency key；取消上限走同一條路，並記下是誰取消的
| 183 | `GET /v1/operator/enrollment-limit` | JSON | 讀取 | `view`；`schema_version=1` 的註冊上限 canonical DTO；算的台數與註冊報告的分母共用同一條 SQL 判準；沒有設上限回「沒有上限」而不是上限 0；不接受任何 query parameter
| 184 | `POST /v1/operator/enrollment-limit/preview` | JSON | 是 | `admin`；純 preview，驗 0..10000 台；digest 綁「要改成什麼」，不綁「現在有幾台」
| 185 | `POST /v1/operator/enrollment-limit` | JSON | 是 | `admin`；fresh/replay 均回 `200`；preview 過期回 `412`，開票撞上限回 `409`；replay 回的是今天的上限，不是當時改成什麼
| 186 | `GET /reports/software` | HTML | 讀取 | `view`；軟體清查。機隊上裝了什麼、各是哪一版、哪幾台沒有，以及那個版號講的是不是正在跑的那一份；每一台每一個工具一格，六種互斥且窮盡的狀態（跟機隊裡最新的一樣／比機隊裡最新的舊／比不出來／裝了問不到版號／這台上沒有／沒回報過）各自附「這是什麼」與「下一步」；裝著東西的那幾格再帶第二個軸（量的就是跑的／正在跑的是另一個檔案／正在跑的那個檔案已經不在磁碟上／有 process 在跑它但說不出跑的是哪一個／process 偵測沒有跑到底／沒有找到在跑它的 process／這一筆觀測沒有講 process 的事）與它講的那兩個檔案，「正在跑的是另一個檔案」與「已經不在磁碟上」那幾格單獨列一節排在版號不一致前面；「最新」指的是機隊裡看到的最新版，那句限制留在畫面上；不接受任何 query parameter
| 187 | `GET /reports/software.csv` | CSV | 讀取／下載 | `view`；一列是「一台機器 × 一個工具」，一格不多一格不少，15 欄含「版號講的是哪一份」「量版號的那個檔案」「正在跑的那個檔案」；走與票證、時間軸、資料揭露、註冊報告同一份 CSV 契約（BOM、UTC RFC 3339 奈秒時間、逐格試算表公式防護）
| 188 | `GET /v1/operator/software-report` | JSON | 讀取 | `view`；`schema_version=2` 的軟體清查 canonical DTO（`runtime`／`misattributed`／`runtime_states`；加欄位就要把版號 +1，strict decoder 對不認得的欄位整份拒收）；不接受任何 query parameter；分母超過 2,000 台或回報超過 200 個工具時拒絕回一份少算的清查
| 189 | `GET /reports/install` | HTML | 讀取 | `view`；每機安裝狀態。我叫哪幾台裝什麼、我在它們上面看到什麼；每一台每一個資源一格，十種互斥且窮盡的狀態（指派的跟看到的一樣／指派的比看到的新／指派的比看到的舊／比不出來／指派的那一筆沒講版號／裝了問不到版號／指派了這台上沒有／指派了看不到這個東西／指派了這台沒回報過／沒有被指派過）各自附「這是什麼」與「下一步」；「指派的」取 machine scope 與該台所在 channel scope 裡 revision 最大的那一筆，跟 agent 實際收到的是同一條規則；「指派的比看到的舊」不講成落後或失敗，那句「東西可以從指派以外的路徑裝上去」留在畫面上；這台回報說它上面有的那幾格再答「看到的那個版號量的是不是正在跑的那一份」（跟軟體清查同一份判準與同一組句子），量錯檔案的那幾格單獨一節排在「指派的跟看到的不一樣」前面、也標在資源 × 機器那張表的每一格上，摘要卡與報告那一行字都帶著那個數字；不接受任何 query parameter
| 190 | `GET /reports/install.csv` | CSV | 讀取／下載 | `view`；一列是「一台機器 × 一個資源」，一格不多一格不少；走與票證、時間軸、資料揭露、註冊報告、軟體清查同一份 CSV 契約（BOM、UTC RFC 3339 奈秒時間、逐格試算表公式防護）；`版號講的是哪一份`／`量版號的那個檔案`／`正在跑的那個檔案` 三欄各自佔一欄，因為照「看到的版號」排序的人只能從那兩欄看出這一列量錯了檔案
| 191 | `GET /v1/operator/install-report` | JSON | 讀取 | `view`；`schema_version=2` 的每機安裝狀態 canonical DTO（`misattributed`、`runtime_states`、每一列的 `runtime`、每一個資源的 `misattributed_on`）；不接受任何 query parameter；分母超過 2,000 台或超過 200 個資源時拒絕回一份少算的報告
| 192 | `GET /reports/profile` | HTML | 讀取 | `view`；發佈與指派。這份 profile 發佈了然後呢；每一個已發佈的 revision 四種狀態（有機器穿著／只有已退役的還穿著／已發佈到更新的版本／發佈了一台都沒指派），它點名的每一個（套件, 版本）四種狀態（指派過也看得到／指派過但沒一台回報／沒指派過但看得到／沒指派過也沒看到過）；「發佈了一台都沒指派」與「點名了一個這個 Hub 沒見過的版本」各自有自己的一節，不埋在大表裡；看得到的版號量在沒人跑的那份檔案上的格子另有一節，排在「一台都沒指派」前面，逐版表也列出其中幾台量錯；不接受任何 query parameter
| 193 | `GET /reports/profile.csv` | CSV | 讀取／下載 | `view`；一列是「一份已發佈的 revision × 它點名的一個套件版本」，一列不多一列不少——改成一台機器一列的話，一份一台都沒指派的 profile 在檔案裡一列都沒有；穿著它的機器擠在一格裡並標出已退役；`其中版號不是跑的那一份的台數` 獨立成欄，量錯檔案的機器仍算進看到幾台；走與票證、時間軸、註冊、軟體清查、安裝狀態同一條匯出管線（BOM、公式中和、UTC、那句限制當最後一欄）
| 194 | `GET /v1/operator/profile-report` | JSON | 讀取 | `view`；`schema_version=2` 的發佈與指派對照 canonical DTO；每一個已發佈的 profile revision 四種狀態，它點名的每一個（套件, 版本）四種狀態；每格的 `seen_misattributed_on` 是 `seen_on` 的子集，整份的 `seen_misattributed` 按格數計，兩者都不另造第五種狀態；有量錯檔案的那一格，下一步改成到每機安裝狀態核對執行檔與其版號；「指派過沒有」讀整條 `desired_state` 歷史，不是現行意圖；不接受任何 query parameter；分母超過 2,000 台或發佈超過 500 版時拒絕回一份少算的報告
| 195 | `POST /machines/{id}/display-name-preview` | HTML | 是，preview | `admin`；單機動作的重新命名 review。明列目前名稱、新名稱、immutable machine ID、名稱型 expectations 的 key 變更，以及 pending enrollment ticket 標籤是否一併更新；要求理由但不寫入
| 196 | `POST /machines/{id}/display-name` | HTML | 是 | `admin`；逐字確認目前名稱後經 canonical service 套用，成功仍以 machine ID 導回 detail；replay 不再次改名
| 197 | `POST /v1/operator/machines/{id}/display-name-preview` | JSON | 是，preview | `admin`；只讀、no-store；驗新名稱格式與跨名冊重複，digest 綁目前名稱與固定影響
| 198 | `PUT /v1/operator/machines/{id}/display-name` | JSON | 是 | `admin`；名冊名稱、未兌換 ticket 標籤、immutable receipt 與 audit 同 transaction；machine ID、agent credential、token/hash、hostname、channel 與 lifecycle 不變；stale preview `412`，確認缺失／不符 `428`，重複名稱 `409`
| 199 | `POST /machines/{id}/notes-preview` | HTML | 是，preview | `admin`；單機動作的名冊備註 review；顯示舊值、新值、理由與固定影響，要求理由但不寫入
| 200 | `POST /machines/{id}/notes` | HTML | 是 | `admin`；逐字確認目前 display name 後經 canonical service 套用；成功與 replay 都以 machine ID 導回動作區
| 201 | `POST /v1/operator/machines/{id}/notes-preview` | JSON | 是，preview | `admin`；只讀、no-store；空字串表示清除，其餘備註限 1000 bytes UTF-8 且不收邊界空白／digest 綁舊值與新值
| 202 | `PUT /v1/operator/machines/{id}/notes` | JSON | 是 | `admin`；備註 CAS、immutable receipt 與 audit 同 transaction；receipt/audit 不複製 free text；machine ID、機器設定與 agent 不變；stale preview `412`，preview 缺失／名稱不符 `428`
| 203 | `GET /v1/operator/daily-report` | JSON | 讀取 | `view`；`schema_version=1`；running Hub 以自己的 expectations、public URL 與還原演練章產生 exact notification body；1 秒至 30 天，預設 24 小時；不送出、不寫 notification receipt；`clawctl-hub report` 正常模式只走這條 API，明示 `--db` 才進 stopped-service fence
| 204 | `POST /preferences/navigation-language` | HTML form | 是，browser preference | `view`；右上角在 `en`／`zh-Hant` 間切換管理中心外框與導覽；同源 form 恰好一個 `locale` 與一個 `return_to`，cross-site POST 以 `CROSS_ORIGIN_REQUEST` 拒絕；HttpOnly SameSite=Lax cookie、同站 bounded `return_to`、不寫 Store／audit；尚未翻譯的證據內容保留 `lang="zh-Hant"`
| 205 | `GET /v1/operator/machines/{id}/assigned-user` | JSON | 讀取 | `view`；使用者識別碼、登入名稱、版本與 `ETag` |
| 206 | `PUT /v1/operator/machines/{id}/assigned-user` | JSON | 是 | `admin`；核對 Tailnet 使用者名冊、名稱確認、版本檢查、請求重放與稽核；`none` 取消指派 |
| 207 | `POST /machines/{id}/assigned-user-preview` | HTML form | 讀取／預覽 | `admin`；顯示目前與目標使用者，保留預期版本；要求輸入機器名稱 |
| 208 | `POST /machines/{id}/assigned-user` | HTML form | 是 | `admin`；套用指派或取消指派後回機器明細；來源不可用與空名冊分別顯示 |
| 209 | `GET /machines/{id}/terminals/{session}` | HTML | 讀取／輸入 | `operate`；這位操作員在這台機器上已開啟的終端，到達時顯示輸出並接受輸入。機器頁的「開啟終端」貼到列 204 |
| 210 | `POST /machines/{id}/terminals` | HTML | 是 | `operate`；機器頁的「開啟終端」貼到這裡，開啟這台機器的終端，成功後前往該終端頁；重送同一組開啟請求回到同一個終端 |
| 211 | `GET /machines/{id}/terminals/{session}/socket` | HTML | 讀取／輸入 | `operate`；終端連線。這位操作員的終端頁用這條連線收發這個工作階段的輸入與輸出。機器頁的「開啟終端」貼到列 204 |
| 212 | `POST /deployments/{id}/skip-failed-batch-preview` | HTML form | 是，preview | `operate`；單獨標示的 skip failed batch。空理由在進 review 前拒絕。digest 與 Continue 不同 |
| 213 | `POST /deployments/{id}/skip-failed-batch` | HTML form | 是 | `operate`；確認 channel。理由寫進稽核與 Hub event。失敗的工作單不重開。不能把最後一批收成 finished |
| 214 | `POST /v1/operator/deployments/{id}/skip-failed-batch-preview` | JSON | 是，preview | `operate`；與 HTML 同一份 eligibility。opened batch 沒有失敗終態，或沒有下一批，則不可執行 |
| 215 | `POST /v1/operator/deployments/{id}/skip-failed-batches` | JSON | 是 | `operate`；plain Continue 在失敗批次回 `DEPLOYMENT_CONTINUE_REFUSED`。MCP 不呼叫這條 |
| 216 | `GET /v1/operator/disk-clean/summaries` | JSON | 讀取 | `view`；已指派或已回報的 disk-clean 摘要。Hub 判決、過期、digest、disk_free_min_percent 與 attention。不接受 query |
| 217 | `GET /v1/operator/disk-clean/summaries/{id}` | JSON | 讀取 | `view`；單機。名冊沒有這台回 404。已註冊未指派仍回 stale |
| 218 | `POST /v1/operator/disk-clean/profile-preview` | JSON | 是，preview | `admin`；渲染 conf 與 digest，不寫 revision |
| 219 | `POST /v1/operator/disk-clean/profiles` | JSON | 是 | `admin`；expected_revision、preview_digest、Idempotency-Key。同一份 conf 不產生新 revision。fresh 201，replay 或 unchanged 200 |
| 220 | `POST /v1/operator/disk-clean/dry-run-preview` | JSON | 是，preview | `admin`；點名 dry-run 目標。blocker 不進 digest |
| 221 | `POST /v1/operator/disk-clean/dry-runs` | JSON | 是 | `admin`；每台一張可逆工作單。fresh 201，replay 200 |
| 222 | `POST /v1/operator/disk-clean/canary-preview` | JSON | 是，preview | `admin`；canary 恰好一台 |
| 223 | `POST /v1/operator/disk-clean/canaries` | JSON | 是 | `admin`；只開第一批。continue 之前不對其餘機器建立工作單 |
| 224 | `POST /v1/operator/disk-clean/continuation-preview` | JSON | 是，preview | `admin`；digest 含目前 state |
| 225 | `POST /v1/operator/disk-clean/continuations` | JSON | 是 | `admin`；paused 且 canary 成功後才打開其餘機器 |
| 226 | `POST /v1/operator/disk-clean/abandonment-preview` | JSON | 是，preview | `admin`；放棄不開新工作單 |
| 227 | `POST /v1/operator/disk-clean/abandonments` | JSON | 是 | `admin`；進行中的工作單還沒結束時拒絕。指派留著 |
| 228 | `POST /machines/{id}/keyed-installer` | download | Read / download | `admin`; verified Linux bootstrap plus Hub URL and one-time enrollment token; no redemption; no-store |

| 229 | `GET /account/security` | HTML | Read | `admin`; local sessions only; Tailscale principals receive 404 |
| 230 | `POST /account/security/totp/begin` | HTML | Mutation | `admin`; local sessions only; Tailscale principals receive 404 |
| 231 | `POST /account/security/totp/confirm` | HTML | Mutation | `admin`; local sessions only; Tailscale principals receive 404 |
| 232 | `POST /account/security/totp/disable` | HTML | Mutation | `admin`; local sessions only; Tailscale principals receive 404 |
| 233 | `POST /account/security/password` | HTML | Mutation | `admin`; local sessions only; Tailscale principals receive 404 |
| 234 | `POST /account/security/recovery-codes/regenerate` | HTML | Mutation | `admin`; local sessions only; password plus authenticator required; replaces ten codes, shown once; shared lockout and audit without codes; host alternative `regenerate-recovery-codes` |
| 235 | `GET /account/users` | HTML | Read | `admin`; active local session only; password and current TOTP for mutations |
| 236 | `POST /account/users/create` | HTML | Mutation | `admin`; active local session only; password and current TOTP for mutations |
| 237 | `POST /account/users/disable` | HTML | Mutation | `admin`; active local session only; password and current TOTP for mutations |
| 238 | `POST /account/users/enable` | HTML | Mutation | `admin`; active local session only; password and current TOTP for mutations |
| 239 | `POST /account/users/email` | HTML | Mutation | `admin`; active local session only; password and current TOTP for mutations |
| 240 | `POST /account/users/rename` | HTML | Mutation | `admin`; active local session only; password and current TOTP for mutations |
| 241 | `GET /v1/operator/script-catalog` | JSON | Read | `view`; embedded catalog metadata and closed args schema |
| 242 | `POST /v1/operator/script-runs/preview` | JSON | Preview | `operate` for read scripts; validated explicit targets and audit |
| 243 | `POST /v1/operator/script-runs` | JSON | Mutation | Matching preview digest, idempotency key; atomic catalog job fanout and audit |
| 244 | `GET /v1/operator/schedules` | JSON | Read | `view`; bounded schedule list with revision, next run, pause reason and last jobs |
| 245 | `GET /v1/operator/schedules/{id}` | JSON | Read | `view`; stored schedule configuration and run metadata |
| 246 | `POST /v1/operator/schedules/{operation}/{phase}` | JSON | Preview / Mutation | `operate` for read scripts, `admin` for write scripts; `upsert` / `set-enabled`, `preview` / `apply`; apply binds preview digest, expected revision and idempotency key |
| 247 | `POST /v1/operator/schedules/{id}/run-now` | JSON | Mutation | `operate` for read scripts, `admin` for write scripts; required Idempotency-Key, saved job-ID replay, approved hash and overlap guard |
| 248 | `GET /v1/operator/jobs-summary` | JSON | 讀取 | `view`; bounded supervision counts, latency and Hub threshold flags |
| 249 | `GET /v1/operator/approvals-summary` | JSON | 讀取 | `view`; observed approvals; disk_ops ledger counts/oldest pending age; legacy unpersisted preview metrics remain null |
| 250 | `GET /v1/operator/hub-status` | JSON | 讀取 | `view`; uptime, storage sizes, restore evidence and machine coverage; no paths or secrets |

| 251 | `GET /account/service-tokens` | HTML | Admin | Local human session and MFA; service token management |
| 252 | `POST /account/service-tokens/create` | HTML | Admin | Local human session and MFA; service token management |
| 253 | `POST /account/service-tokens/revoke` | HTML | Admin | Local human session and MFA; service token management |
| 254 | `GET /v1/operator/service-tokens/self` | JSON | Read | Service token only; own metadata, no allowlist requirement |
| 255 | `POST /v1/operator/service-tokens/rotate` | JSON | Yes | Service token only; newest chain token; 10-minute overlap |
| 256 | `GET /v1/operator/disk-ops/policies` | JSON | Read | Human view; immutable disk policy history; paths included, token-ineligible |
| 257 | `GET /v1/operator/disk-ops/policies/{id}` | JSON | Read | Human view; latest revision; token-ineligible |
| 258 | `POST /v1/operator/disk-ops/policies/preview` | JSON | Preview / Mutation | Human admin; normalized policy/revision/digest/30-minute expiry |
| 259 | `POST /v1/operator/disk-ops/policies` | JSON | Preview / Mutation | Human admin; default-off flag, preview binding, idempotency and atomic audit |
| 260 | `GET /v1/operator/disk-ops/approvals` | JSON | Read | View; path-free ledger, exact view-token allowlist eligible |
| 261 | `GET /tenant/disk-policies` | HTML | Read | Human admin; canonical service, no integration required; preview binding and reauthentication on UI apply |
| 262 | `GET /tenant/disk-policies/{id}` | HTML | Read | Human admin; canonical service, no integration required; preview binding and reauthentication on UI apply |
| 263 | `POST /tenant/disk-policies/preview` | HTML | Preview / Mutation | Human admin; canonical service, no integration required; preview binding and reauthentication on UI apply |
| 264 | `POST /tenant/disk-policies/apply` | HTML | Preview / Mutation | Human admin; canonical service, no integration required; preview binding and reauthentication on UI apply |
| 265 | `GET /tenant/approvals` | HTML | Read | Human view; state/machine/operation filters; decisions visible to admins only |
| 266 | `POST /tenant/approvals/preview` | HTML | Preview / Mutation | Human admin; canonical service, no integration required; preview binding and reauthentication on UI apply |
| 267 | `POST /tenant/approvals/apply` | HTML | Preview / Mutation | Human admin; canonical service, no integration required; preview binding and reauthentication on UI apply |
| 268 | `POST /v1/operator/disk-ops/approvals/{id}/decision/preview` | JSON | Preview / Mutation | Human admin; canonical service, no integration required; preview binding and reauthentication on UI apply |
| 269 | `POST /v1/operator/disk-ops/approvals/{id}/decision` | JSON | Preview / Mutation | Human admin; canonical service, no integration required; preview binding and reauthentication on UI apply |

| 270 | `GET /v1/operator/disk-ops/status/{machine_id}` | JSON | Read | Human view; path-bearing status; token-ineligible |
| 271 | `GET /machines/{id}/disk` | HTML | Read | Human view; status, previews, safe cleanup and schedules |
| 272 | `POST /machines/{id}/disk/actions` | HTML | Preview / Mutation | Human admin; CSRF, preview binding, apply reauthentication and audit |

The inventory excludes six public account routes; API-SURFACE includes them.

HTML/BFF handlers 見 `internal/web/web.go`、`internal/web/actions.go`、
`internal/web/terminal_open.go`、
`internal/web/verifier_assignment_actions.go`、`internal/web/assigned_user.go`；
終端連線見 `cmd/clawctl-hub/operator_terminal_socket.go`；
149 條 operator JSON 路由（Secrets 狀態由 `internal/web/secrets.go` 提供）見 `cmd/clawctl-hub/operator_api.go`、`cmd/clawctl-hub/operator_disk_clean_api.go`、
`cmd/clawctl-hub/operator_assigned_user_api.go`、
`cmd/clawctl-hub/operator_deployment_api.go`、`cmd/clawctl-hub/operator_artifact_api.go`、
`cmd/clawctl-hub/operator_artifact_fetch_api.go`、`cmd/clawctl-hub/operator_update_api.go` 與
`cmd/clawctl-hub/operator_audit_api.go`、`cmd/clawctl-hub/operator_change_api.go`、
`cmd/clawctl-hub/operator_machine_lifecycle_api.go`、`cmd/clawctl-hub/operator_diagnostic_api.go`、
`cmd/clawctl-hub/operator_ticket_api.go`、`cmd/clawctl-hub/operator_report_api.go`、
`cmd/clawctl-hub/operator_enrollment_limit_api.go`、
`cmd/clawctl-hub/operator_machine_rename_api.go`、
`cmd/clawctl-hub/operator_catalog_api.go`、`cmd/clawctl-hub/operator_retention_api.go`、
`cmd/clawctl-hub/operator_restore_drill_api.go`、
`cmd/clawctl-hub/operator_verification_assignment_api.go`，共用
domain service 見 `internal/operator/`。

| 273 | `GET /v1/operator/disk-ops/reports` | operator.DiskReports | 否 | View; finalized summaries, no private paths |
| 274 | `GET /v1/operator/disk-ops/reports/{id}` | operator.DiskReport | 否 | View; cursor/limit <=100; sanitized display paths |
| 275 | `GET /disk-reports/{id}` | operator.DiskReportHuman | 否 | Human viewer; admin may see full paths; exact human decisions, quarantine and restore available |
| 276 | `POST /v1/operator/disk-ops/actions/preview` | JSON | Human admin + fresh reauth | P2-04 human-only quarantine / restore |
| 277 | `POST /v1/operator/disk-ops/actions/decide` | JSON | Human admin + fresh reauth | P2-04 human-only quarantine / restore |
| 278 | `GET /v1/operator/disk-ops/quarantine/{machine_id}` | JSON | 讀取 | P2-04 human-only quarantine / restore |
| 279 | `POST /v1/operator/disk-ops/restores/preview` | JSON | Human admin + fresh reauth | P2-04 human-only quarantine / restore |
| 280 | `POST /v1/operator/disk-ops/restores` | JSON | Human admin + fresh reauth | P2-04 human-only quarantine / restore |
| 281 | `POST /disk-reports/{id}/actions` | HTML form | Human admin + fresh reauth | P2-04 human-only quarantine / restore |
| 282 | `POST /disk-actions/apply` | HTML form | Human admin + fresh reauth | P2-04 human-only quarantine / restore |
| 283 | `GET /machines/{id}/quarantine` | HTML | 讀取 | P2-04 human-only quarantine / restore |
| 284 | `POST /machines/{id}/quarantine/restore` | HTML form | Human admin + fresh reauth | P2-04 human-only quarantine / restore |

| 285 | `GET /v1/operator/disk-ops/alerts` | JSON | No | P2-05; exact view permission; shared operator service and audited human mutations |
| 286 | `GET /v1/operator/disk-ops/ownership/{machine_id}/{scope}` | JSON | No | P2-05; exact view permission; shared operator service and audited human mutations |
| 287 | `POST /v1/operator/disk-ops/ownership/preview` | JSON | Yes | P2-05; exact admin permission; shared operator service and audited human mutations |
| 288 | `POST /v1/operator/disk-ops/ownership` | JSON | Yes | P2-05; exact admin permission; shared operator service and audited human mutations |
| 289 | `POST /v1/operator/disk-ops/alerts/ack/preview` | JSON | Yes | P2-05; exact admin permission; shared operator service and audited human mutations |
| 290 | `POST /v1/operator/disk-ops/alerts/ack` | JSON | Yes | P2-05; exact admin permission; shared operator service and audited human mutations |
| 291 | `GET /tenant/disk-alerts` | HTML | No | P2-05; exact view permission; shared operator service and audited human mutations |
| 292 | `POST /tenant/disk-alerts/ack/preview` | HTML | Yes | P2-05; exact admin permission; shared operator service and audited human mutations |
| 293 | `POST /tenant/disk-alerts/ack` | HTML | Yes | P2-05; exact admin permission; shared operator service and audited human mutations |
| 294 | `POST /machines/{id}/disk/ownership` | HTML | Yes | P2-05; exact admin permission; shared operator service and audited human mutations |
| 295 | `GET /tenant/schedules` | HTML | 否 | `view`；schedule list |
| 296 | `GET /tenant/schedules/new` | HTML | 否 | `operate`；new schedule form |
| 297 | `GET /tenant/schedules/{id}` | HTML | 否 | `view`；schedule detail |
| 298 | `GET /tenant/schedules/{id}/edit` | HTML | 否 | `operate`；edit schedule form |
| 299 | `POST /tenant/schedules/preview` | HTML | 是，preview | `operate`；upsert/set-enabled preview; write catalog scripts require admin |
| 300 | `POST /tenant/schedules/apply` | HTML | 是 | `operate`；digest-bound, idempotent upsert/set-enabled apply; write catalog scripts require admin |
| 301 | `POST /tenant/schedules/{id}/run-now` | HTML | 是 | `operate`；idempotent manual run; write catalog scripts require admin |
| 302 | `GET /secrets` | HTML | 讀取 | `admin`；read-only secret reference status; never values |
| 303 | `GET /v1/operator/secrets` | JSON | 讀取 | `admin`；name/ref/status only; never values |
| 304 | `GET /channels` | HTML | 讀取 | `view`；channel health and metadata bindings |
| 305 | `GET /v1/operator/channels` | JSON | 讀取 | `view`；safe channel inventory |
| 306 | `POST /channels/{operation}` | HTML | 寫入 | `admin`；save/remove/test/bind/unbind; audited and same-origin |
| 307 | `POST /secrets/test` | HTML form | 寫入 | `admin`；configured ref test fetch; audited and same-origin; status only |
| 308 | `POST /account/service-tokens/routes` | HTML form | Human admin + fresh MFA | Exact manifest route edits; per-route old-to-new audit diff |
| 309 | `GET /attention/loops` | HTML | 讀取 | `view`；loop episode list, policy, coverage and actionable empty state |
| 310 | `POST /attention/loops` | HTML form | 寫入 | `admin`；policy update and acknowledge/resolve actions |
| 311 | `GET /v1/operator/loops` | JSON | 讀取 | `view`；bounded agent-reported loop evidence; optional machine filter |
| 312 | `GET /tenant/certificates` | HTML | 是 | View; TLS evidence, empty state and next actions |
| 313 | `POST /tenant/certificates` | HTML form | 是 | Human Admin; audited add/edit/remove/queued probe |
| 314 | `GET /v1/operator/certificates` | JSON | 是 | View; sorted certificate inventory |
| 315 | `GET /v1/operator/certificates/{id}` | JSON | 是 | View; certificate configuration and latest evidence |
| 316 | `POST /v1/operator/certificates` | JSON | 是 | Human Admin; create certificate monitor |
| 317 | `PUT /v1/operator/certificates/{id}` | JSON | 是 | Human Admin; revision-checked configuration replacement |
| 318 | `DELETE /v1/operator/certificates/{id}` | JSON | 是 | Human Admin; revision-checked removal |
| 319 | `POST /v1/operator/certificates/{id}/probe` | JSON | 是 | Human Admin; revision-checked queued probe |

## Workload / submenu parity matrix

### Home > Overview / Service health

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Fleet overview | `operator.Service.ListMachines`；`internal/operator` | ✅ Dashboard，SSR 共用 in-process service | ✅ Machines safe summary slice | ✅ `machines` HTTP | 讀取 | Dead signals、double agents、Hub events 等 ancillary diagnostics 仍待 typed API |
| Dead signals / double agents | `internal/store/deadsignal.go`; `internal/store/doubleagent.go` | ✅ | — | — | 讀取 | 納入 overview/alerts read API；保留 unknown 與 zero 的差別 |
| Hub events / fleet tools | `internal/store/hubevents.go`; `internal/store/fleettools.go` | ✅ | — | — | 讀取 | 建立 diagnostics read API；再做 drilldown |
| Hub health / metrics | `GET /healthz` is a public probe; `GET /metrics` requires the operator view grant and a managed machine receives 403 | △ 摘要 | view | — | 否；probe/scrape | Health submenu 僅顯示結果，不包裝成 mutation |

### Devices > All devices

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Machine list / status | `operator.Service.ListMachinesPage`；`internal/operator` | ✅ canonical filters/paging | ✅ `GET /v1/operator/machines` schema v2 | ✅ `machines` HTTP | 讀取 | machine/display/state/lifecycle/reporting/channel filters；creation-ceiling live keyset；received-at evidence cutoff；restore/VACUUM 後重開 traversal，不宣稱 atomic snapshot |
| Machine detail | `operator.Service.MachineDetail`＋`MachineEvidence`；`internal/operator` | ✅ typed judgement/expectations/monitor/identity/resources/history＋typed evidence | △ `GET /v1/operator/machines/{id}` v5＋evidence v4 | △ `machines show`＋`machines evidence` | 讀取 | detail v5 有 Hub judgement／100 findings、50 條 expectation definitions、20-entry event pages、artifact/event clocks 與 unknown/read-failed/invalid/truncated，另有 24h/1000 check-ins、20 state spans、100 durable identity hints。Artifact freshness 不是 outcome，event enum 由 operator 宣告且不掃 free text；display text 不做 path/secret 掃描。Evidence v4 負責 OpenClaw/CLI/credential/occupancy/systemd/text。pending HTML 與 BAT connect 已分別讀 dedicated typed operator service／typed BFF v1；獨立 verifier 證據在工作單與部署 gate 呈現，不併進 machine detail |
| Registry display-name rename | `ApplyOperatorMachineRename`；display-name preview + `PUT` | ✅ 單機動作 preview/confirm | ✅ | ✅ `machine rename` HTTP only | 是 | 唯一名稱、typed current-name confirmation、stale preview、idempotent replay 與 atomic registry/pending-ticket-label/receipt/audit；名稱型 expectations 改用新名稱，machine ID、agent、hostname、channel 與 lifecycle 不變 |
| 機器指派使用者 | `MachineAssignedUser/ChangeMachineAssignedUser` | ✅ 明細、Devices 欄位與預覽／確認 | ✅ `GET/PUT assigned-user` | ✅ `machine assigned-user` HTTP／資料庫；`machines show` | 是 | Tailnet 使用者識別碼或 `none`；名稱確認、版本檢查、請求重放與稽核 |
| Registry notes | `ApplyOperatorMachineNotes`；notes preview + `PUT` | ✅ 單機動作 preview/confirm | ✅ | ✅ `machine notes` HTTP only | 是 | 空字串清除、typed current-name confirmation、舊備註 CAS、stale preview、idempotent replay 與 atomic registry/receipt/audit；receipt/audit 只記錄有無與動作，不複製 free text；機器設定與 agent 不變 |
| Actual-state history | checkins/observations；`internal/store/store.go` | ✅ bounded recent history | ✅ detail v5 | ✅ `machines show` | 讀取 | 已有 source provenance、兩個 clocks 與 truthful limit/truncated；後續若需要任意 range 再另加 keyset endpoint，不把 24h/20 段冒稱完整歷史 |

### Devices > Monitor / Agent

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Agent version / instance | `CheckinPoint`；`internal/store/store.go` | ✅ 最新值＋heartbeat strip | ✅ detail v3 | ✅ `machines show` | 讀取 | agent version、boot ID、sequence、uptime 與 distinct start/boot identities 都 typed；舊 agent 未送欄位維持 null/unknown |
| Job execution readiness | heartbeat `jobs_enabled`；`machine_checkins` | ✅ Diagnostics preview | ✅ diagnostic preview/apply | ✅ `job create --preview` | 讀取／preflight | enrollment 預設啟用；latest true 才建立 diagnostic desired state 與 job，unknown/false 回 typed blocker |
| Observation freshness | `Detail` / observations | ✅ | ✅ detail v3 | ✅ `machines show` | 讀取 | latest Hub receive time、agent measured/sent clocks、clock skew 與 absent/undecodable 分開；liveness 只採 received_at |
| Protocol capabilities | check-in `device_sync_v1`＋`GET /v1/agent/readiness`；`GET /v1/capabilities` | — | — | agent only | 否；agent negotiation | 第九個 Devices 候選已選「同步裝置資料」；fixed executor/capability receipt 與 Store-only canonical preview/apply 已落地，但尚無公開 operator create path，Devices 目錄仍不顯示。舊 agent 不從 version 猜支援 |
| Expected tools / units | `FleetTools`、machine detail resources | ✅ 部分 | — | — | 讀取 | 統一 compliance/evidence view；依賴 detail API |

### Devices > Enrollment

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Enrollment preview | canonical service；`internal/operator/service.go` | ✅ Enrollment submenu | ✅ | ✅ | 是，安全 preview | 已移入可發現的專頁；Dashboard 保留 CTA |
| Enrollment ticket create | canonical service；`internal/operator/service.go` | ✅ | ✅ | ✅ | 是 | 增加 enrollment list/history read model；不保存明文 token |
| Agent redeem ticket | `POST /v1/enrollments` | 僅顯示結果 | machine API only | agent `enroll` | **否；agent callback** | 回應是版本化 authority receipt；agent 嚴格拒絕未知／重複／尾隨 JSON，machine/token/interval/settings identity 不完整時不寫設定也不啟用 deploy jobs。缺少、錯誤、已過期票券皆明確失敗；不新增 operator/browser redeem action |
| Pending ticket get | `OperatorPendingEnrollToken` | ✅ 每台唯一一張；多張 fail closed | ✅ per-machine GET | ✅ revoke preview | 讀取 | 後續再加跨 machine list、expiry/state filters |
| Pending ticket revoke | `ApplyOperatorEnrollTokenRevocation` | ✅ preview/confirm | ✅ | ✅ HTTP／break-glass | 是 | 已有 digest、idempotency、stale-state、atomic redacted receipt/audit |

#### Credential terminology is a security contract

`POST /machines/{id}/revoke-token` 只撤銷**尚未兌換的 enrollment ticket**，handler
透過 canonical operator service 進入同一個 transaction。它不是
「revoke agent token」。Active machine credential 的 rotate/revoke Store method、operator API、
UI 與 CLI 目前全部不存在。Retire 只會因 authentication query 排除 retired machine 而停止授權，
見 `internal/store/store.go`；也不應冒充 credential rotation。

後續功能與畫面必須固定拆成三項：

1. Pending enrollment ticket revoke。
2. Active agent credential rotate/revoke（新 capability；須定義 reconnect 與 recovery）。
3. Machine retire/unretire（名冊 lifecycle，可逆狀態）。

### Devices > Lifecycle

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Lifecycle read | `OperatorMachineLifecycle`；`GET /v1/operator/machines/{id}/lifecycle` | ✅ Lifecycle submenu＋detail entry | ✅ | ✅ `machine lifecycle` HTTP／break-glass | 讀取 | Safe projection 明列 revision、分母、channel、credential/ticket 可用性與非終態 job；不曝 secret/hash；`c91695b` 已完成 live rollout |
| Retire | `ApplyOperatorMachineLifecycle`；preview + `PUT .../lifecycle` | ✅ preview/confirm | ✅ | ✅ HTTP／break-glass | 是 | Preview 明列當下開啟的終端工作階段數；Web caveat 與 CLI apply notice 說明套用時仍開啟者會結束。Count 不綁 digest/receipt；其餘獨立 `lifecycle_revision`、typed name/reason、digest/idempotency、atomic event/receipt/audit 與 `nonterminal_jobs` blocker 已完成合約測試 |
| Restore active | 同一 canonical lifecycle service/API | ✅ preview/confirm | ✅ | ✅ HTTP／break-glass | 是 | 保留 registry/history/channel；明列恢復後 retained bearer 可立即再驗證、未過期 pending ticket 可再兌換，不冒充 credential rotation |
| Active credential rotate/revoke | 尚無 native capability | — | — | — | 是，高風險 | 先定 threat/recovery model，再建 Store/service/API；不可沿用 pending-ticket 名稱 |

### Devices > Configuration

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| 設定盤面 | `operator.SettingBoard`；`GET /v1/operator/settings` | ✅ `/machines/configuration` | ✅ | ✅ `settings list` | 讀取 | 每台的 effective settings 與它回報的 digest 並列；verdict 由 `settingpolicy.Judge` 以最差證據優先決定 |
| 發佈設定原則 | `ApplyOperatorSettingPolicy`；preview + `POST /v1/operator/setting-policies` | ✅ preview/confirm | ✅ | ✅ `settings publish` | 是 | revision 不可變、同值不生新 revision、`expected_revision` CAS、preview digest、idempotency 與 audit 同 transaction |
| 指派設定原則 | `ApplyOperatorSettingAssignment`；preview + `POST /v1/operator/setting-assignments` | ✅ preview/confirm | ✅ | ✅ `settings assign` | 是 | scope 為 machine 或 channel；machine 優先於 channel 優先於預設值；退役機器不可指派 |
| 合規性盤面 | `operator.ComplianceBoard`；`GET /v1/operator/compliance` | ✅ `/machines/compliance` | ✅ | ✅ `compliance list` | 讀取 | 每台的判決旁邊就是推出它的每一條規則結果，以及動作現在做到哪一步；判決由 `compliance.Judge`、動作由 `compliance.DecideActions` 現算，都不落表，因為它們是「現在幾點」的函數 |
| 發佈合規性原則 | `ApplyOperatorCompliancePolicy`；preview + `POST /v1/operator/compliance-policies` | ✅ preview/confirm | ✅ | ✅ `compliance publish` | 是 | 規則只能建立在 Hub 量得到的事實上；不合規動作寫在同一份原則文件裡，所以改寬限期就是新的 revision；同一組規則與動作換順序寫仍是同一份，不生新 revision；`expected_revision` CAS、preview digest、idempotency 與 audit 同 transaction |
| 指派合規性原則 | `ApplyOperatorComplianceAssignment`；preview + `POST /v1/operator/compliance-assignments` | ✅ preview/confirm | ✅ | ✅ `compliance assign` | 是 | scope 為 machine 或 channel；machine 優先於 channel；沒指派原則的機器是「未指派」而不是符合 |
| 不合規動作 | `compliance.DecideActions`；`store.ComplianceBlocksJobs`／`ComplianceBlockedMachines` | ✅ 盤面動作欄＋停發台數 | ✅ 盤面與兩個 preview | ✅ `compliance publish --block-jobs-after` | 讀取 | 唯一一種後果是停發工作單（寬限 0–86400 秒）：`GET /v1/jobs/next` 回 `204`，部署預覽也把那台排除並印「合規性停發工作單」。「連續不符合多久」靠重判寬限期那段窗裡的每一次 check-in 證明，中間有一次過就重新起算；動作不落表，恢復報到就自動解除；從未報到與量不到都不觸發動作 |
| 裝置動作目錄 | `operator.MachineActions`；`GET /v1/operator/machines/{id}/actions` | ✅ 單機頁「動作」那一節 | ✅ | ✅ `machine actions` | 讀取 | `schema_version=5`；九個具名動作（連線、開啟終端、診斷工作單、重新命名、編輯名冊備註、部署通道、撤銷註冊票、退役、恢復管理）。`connect` 與 `open_terminal` 是兩個 `surface=web` 的動作——連線沿用不設 coordinate JSON route 的 `MachineConnect` v1 BFF，開啟終端沿用機器頁的 `POST /machines/{id}/terminals`，所以目錄不給它們 operator API 路徑。`open_terminal` 只出現給這台機器的指派使用者，而且只出現在 Linux 機器上，所以指派使用者最多八列，其他人最多七列。每個動作的可用與否直接取自守住它寫入路徑的 preview/read contract；重新命名與名冊備註對 active／retired 都可用，各自的輸入由專用 preview 判；退役／恢復管理只列會改變狀態的方向，view-only 不顯示動作節 |
| 套用狀態 | `machine_checkins.settings_digest` | ✅ 盤面狀態欄 | ✅ board 的 `verdict` | ✅ `settings list` | 讀取 | 唯一證據是 agent 回送的 digest。不數心跳間隔、不讀 agent log —— 那是 `docs/PRODUCT.md` 地基二禁止的自證 |
| 設定集合 | `internal/settingpolicy` | ✅ | ✅ | ✅ | — | 目前只有 check-in（30s–1h）與 observation（1m–24h）兩個間隔，且 observation 不得小於 check-in。加欄位要同時提 `SchemaVersion` 並補 `Validate` |

### Devices > Channel assignments

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Channel read/change | `ApplyOperatorMachineChannel`；`internal/store/operator.go` | ✅ machine detail | ✅ GET/PUT | ✅ HTTP | 是 | 已完成 vertical slice；再加 list filter/bulk preview，不另開 direct-Store path |

### Devices > Remote assistance

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Connect BAT | `operator.Service.MachineConnect`／`RecordMachineConnectAudit`；`internal/web/actions.go` | ✅ | 不需要 proxy API | — | 是 | 保持 redirect/BFF；同一份 typed projection 決定 availability 與 audit，Web 不直接寫 Store；button audit 是 best-effort 且不涵蓋 copied URL。這是單機連線，不是 default 庫裡釘版的 BAT 套件；RDP 後排 |

### Apps > Discovered apps / OpenClaw

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Installed OpenClaw versions | `LatestOpenClawInstalls`；`internal/store/store.go` | ✅ OpenClaw submenu | — | 間接 | 讀取 | App detail、version distribution、failed/unknown drilldown；依賴 machine read API |
| Latest install result | `LatestSucceededJobForResource` | △ | — | job list | 讀取 | 關聯 deployment/job/evidence，避免只用版本字串判成功 |
| Other discovered resources | observation inventory | △ machine detail | — | — | 讀取 | 定義 resource-kind navigation；不要把未支援資源做成 action |

Apps UI 現在有「概觀、OpenClaw、Store、Profiles、Assignments、Artifacts、Fetch、
Operations」八個 submenu；OpenClaw 觀測、可部署 package/profile、machine assignment、Hub material
與非同步 intake ledger 分開呈現，見 `internal/web/templates/apps.html`。

### Apps > Store / Profiles / Assignments

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 契約 |
|---|---|---:|---:|---:|---|---|
| Standard package preview/publish | `PreviewStandardCatalogManifest/PublishStandardCatalogManifest` | ✅ Store | ✅ GET/POST preview/publish | ✅ `catalog package` HTTP | 是 | 固定 Node／OpenClaw／Hermes／Claude Code／Codex／Grok／Antigravity／Linux BAT Server manifest，重新完整驗 artifact；OpenClaw 綁 exact 已發布 Node；OpenClaw/Hermes 衝突且同屬 `primary-agent-runtime`。Pinokio 尚不在這條路徑 |
| Profile preview/publish | `PreviewMachineProfile/PublishReviewedMachineProfile` | ✅ Profiles | ✅ GET/POST preview/publish | ✅ `catalog profile` HTTP | 是 | Profile 只存 directly selected packages；publication 驗證 exact material 與 canonical digest |
| 已發佈那幾版現在怎麼樣 | `operator.Service.ProfileReport` | ✅ Profiles | ✅ `GET /v1/operator/profile-report` | ✅ `report profile` | 讀取 | 每一版帶「現在穿在幾台身上」「另有幾台已退役仍是這一版」與四種互斥狀態各自的下一步；⚠ 與 `/reports/profile` 讀同一份投影，不各自算一次 |
| Profile assignment | `PreviewMachineProfileAssignment/AssignMachineProfile` | ✅ Assignments | ✅ POST preview/apply | ✅ `catalog assign` HTTP | 是 | 選 OpenClaw 即展開 Node；選 Hermes 即綁 exact OCI；不同 profile 建立 append-only supersession；依賴順序的 desired states、jobs、edges、receipt 與 audit 同 transaction |
| 現在誰穿著哪一版 | `operator.Service.ProfileReport` 反過來讀 | ✅ Assignments | ✅ 同上 | ✅ 同上 | 讀取 | 名冊上每一台一列：現行那一份指派的 `id@revision`、指派時間與指派人；沒有的那一格講出「還沒有被指派過任何 profile」，不是留白。⚠ 一台只能穿一版（`FleetProfileAssignments` 只回 `assignment_revision` 最大的那一筆），反過來的報告當場拒絕；已退役的機器不在分母也不在表單上 |

Store 與 Profiles list 都是 bounded safe DTO 與 filter-bound keyset cursor。Web apply 攜帶 server 產生的
canonical token；CLI 每個 mutation 都依 preview 建立 private recovery receipt，同一 request 可精確重放。
⚠ Profiles 與 Assignments 兩頁的「已發佈那幾版」已改讀 `ProfileReport`，不再讀 `ListMachineProfiles`：
後者的上限是 100 版而且**靜靜截斷**，所以第 101 版發佈之後那兩頁會漏掉它而畫面上完全看不出來；
報告那一側超過 500 版是拒絕，不是截斷。

### Apps > Artifacts

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Artifact list/detail | `operator.Service.ListArtifacts/ArtifactDetail`；`internal/artifact/catalog.go` | ✅ Artifacts submenu + safe detail | ✅ `GET` list/detail | ✅ `artifact list/show` HTTP／break-glass | 讀取 | List 只做 bounded no-follow metadata inspection 並回 `available_unverified`；detail 才由已開啟的 descriptor 完整重算 SHA-256，成功後回 `ready`；detail 與 deployment preview/apply 共用單目錄限流及 caller cancellation；單筆壞 sidecar/orphan 不拖垮全 catalog |
| Official artifact fetch preview/enqueue | `PreviewArtifactFetch/ApplyArtifactFetch`；`internal/artifact/fetch.go` | ✅ Fetch review/apply | ✅ `POST` preview/create | ✅ `artifact fetch` HTTP／break-glass | 是，高風險 | 接受 exact `openclaw@semver`、`node-runtime@major.minor.patch`、`hermes-agent@major.minor.patch`、`claude-code@major.minor.patch`、`codex@major.minor.patch` 與 `antigravity@major.minor.patch`；固定 npm／nodejs.org／Docker Hub／downloads.claude.ai／releases.openai.com／Antigravity manifest origin。Antigravity 只接受上游目前發佈的版號。Claude Code 釘住官方 manifest 的 linux／darwin／windows amd64／arm64 六份 binary SHA-256。Codex 釘住 `release.json` 的 `rust-v<version>`、六份 `codex-package` archive 與 `codex-package_SHA256SUMS`。兩者都建成 deterministic bundle。不接受 arbitrary origin、proxy、redirect、GitHub fallback、`latest` channel 或 request 自訂大小上限 |
| Fetch operation list/detail | `ArtifactFetchOperations/ArtifactFetchOperation`；`internal/store/artifact_fetch.go` | ✅ Operations submenu + safe detail | ✅ `GET` list/detail | ✅ `artifact fetch list/show` HTTP／break-glass | 讀取 | Durable queued/running/succeeded/failed ledger；明列 downloading/verifying/publishing/complete phase、monotonic progress、attempt 與 bounded failure |
| Machine artifact download | `GET/HEAD /v1/artifacts/{sha256}` | 僅 evidence | machine API only | agent only | **否；agent callback** | UI 不呼叫 machine route；bearer 只能下載該 machine 目前 active exact job 所綁 digest，不能列舉或換 path；由 artifact/detail 顯示 serving readiness |

Fetch enqueue、immutable idempotency receipt 與原始 structured audit 在同一 writer transaction；
同 package/version 或 material identity 同時間只允許一個 active operation。Hub 啟動時只清理 stale temp
並輪替既有 `running` authority，再對外 READY；runtime worker 才 oldest-first reclaim running work，
接著處理 durable queue。每次 claim/reclaim 都換 run token，舊 worker 的 progress/terminal write 因
fencing 失效；暫時性 terminal write 失敗留下的 running row 會由下一輪安全 reclaim。Worker-only tarball URL、run token、
idempotency key、request digest 與 private reason 不進 safe operation DTO；artifact read 也不回 filesystem
path 或 fetched-by identity。HTTP CLI 在 enqueue 前建立 fsync、`0700`/`0600`、no-clobber private
recovery receipt；ambiguous response 只可用 `--recovery-file` 原 request 重放，完整輸出後才清除。

### Updates > Overview / Artifacts / Canary / Stable

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Update read model | `operator.Service.Updates`；`internal/operator/update_read.go` | ✅ 更新概觀／Artifacts／Canary／Stable | ✅ `GET /v1/operator/updates` | —；strict Go client 已交付 | 讀取／preview | `schema_version=5`；同一 evaluation instant 組合 metadata-only artifact page、channel members、last-observed versions＋Hub receive times、latest safe deployment、rollout plan 與 stable promotion gate；stable 逐 target 帶 machine/job、十一種 gate state 與 typed next step；channel/status/version/limit/cursor filters 共用 artifact 契約，沒有 freshness threshold 時不冒稱 current |

Updates 是 read/preview composition，不是第二套 deployment writer：真正 create/promotion 仍走
Deployments 的 canonical preview/apply。Artifact list 在 Updates 仍是 metadata-only，必須點進 Apps
artifact detail 或進 deployment preview/apply 才能取得當下 bytes 的完整 hash 證明。

### Deployments > All deployments / Details

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| List/detail | `operator.Service.ListDeployments/DeploymentDetail` | ✅ 五個 submenu + safe detail | ✅ | ✅ HTTP／break-glass | 讀取 | 已共用 revision、target、batch、material 與 action eligibility safe DTO；revision 耗盡明列 blocker；list 是 filter-bound live keyset，不是 snapshot |
| Create / promotion preview | `operator.Service.PreviewDeploymentCreate` | ✅ settings → review | ✅ | ✅ HTTP／break-glass | 是，preview | `schema_version=4`；固定 targets、policy digest 與已重新驗 bytes 的 artifact identity；stable promotion 要求每個 effective successful canary job 的 active fleet-peer 完整 report 與 exact release version |

### Deployments > Create / Rollout controls

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Create | `ApplyDeploymentCreate` | ✅ typed channel/version | ✅ | ✅ HTTP／break-glass | 是，高風險 | Preview digest、verified actor、idempotency receipt 與 mutation/audit 同 transaction；不可繞 promote lock |
| Continue | `ApplyDeploymentContinue` | ✅ typed channel | ✅ | ✅ HTTP／break-glass | 是 | CAS 綁 control revision/opened batch（batch 至少 1，revision 不可耗盡）；只開下一批或收尾，不重開既有失敗 job |
| Retry | `ApplyDeploymentRetry` | ✅ typed channel/version | ✅ | ✅ HTTP／break-glass | 是 | 只接受 preview 中 exact terminal-failure identities，建立可追 lineage 的新 attempt |
| Abandon | `ApplyDeploymentAbandon` | ✅ full-ID confirmation | ✅ | ✅ HTTP／break-glass | 是，高風險 | 所有已開 job 必須終態；明列未開 targets 後 paused → finished |

所有四個 mutation 共用同一個 Store transaction primitive：deployment／desired state／jobs、
hub event、idempotency receipt 與 structured audit 一起 commit。相同 key/body replay 回原 receipt，
不同 body/operation 回 conflict；replay 走持久化判決，不會因今天 artifact 已移除而偷偷重做。
Create apply 六個 planning fields 必須明列，batch/timeout 顯式 `0` 也拒絕；四種 apply 的 preview
digest 必須是 canonical lowercase `sha256:`，既有 deployment 的 opened batch 必須至少為 1；
control revision 為負或已達上限時所有需要遞增 revision 的 lifecycle writer 都 fail closed。OpenClaw spec 與 job template digest 在 initial/delayed
batch transaction 都綁同一份 material。成功 replay 只有在 canonical success receipt、
request-digest-bound 原 typed confirmations、完整 durable
deployment/job/target graph 與唯一原始 success audit 三者一致時成立；Retry 另重驗整條 ancestor
lineage。拒絕 replay 則驗 canonical rejection 與 exact failed audit，不依賴今天的 fleet state；
corrupt/orphan cache 一律 fail closed。
已開 target 另要求 `job_id` 全局單一且 machine/desired/revision 綁定正確；反向也要求
deployment `desired_id` 下每張 job 恰好連回同一 deployment 的 matching target。Desired-state
channel/resource/revision identity、target machine registry existence、known job state 與
terminal-state/`terminal_at` 對應也都屬 graph invariant。同資源被 finished/corrupt deployment
隱藏的非終態 job 在新 owner admission、UI projection 與 writer transaction 都 fail closed，不可
開新 job 或釋放 owner。會再開 job 的 delayed batch／Continue、Retry、promotion 與 cached-success
evidence 另逐張核對 linked job artifact digest 與 OpenClaw spec；fresh finish-only Continue／Abandon 不依賴已下架的
material。Promotion 另對 lineage 每一代重驗 stored batch plan，拒絕超出
batch-size blast radius 的 legacy 證據。
Web list/detail 只接收 operator safe DTO，不再把 raw desired spec 或 `created_by` 放進 template model。
正常 HTTP CLI mutation 在 apply 前自動建立 `0700`/`0600` private recovery receipt，
綁定 exact request/key/transport 且不在 terminal 顯示 private reason；ambiguous response 後以
同 action `--recovery-file` 重放，只有在結果完整寫出後才刪除。

### Devices > Jobs / Monitor

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Global job list | `operator.Service.ListJobs`；`internal/operator/job_read.go` | ✅ `/jobs` submenu | ✅ `GET /v1/operator/jobs` | ✅ `job list` HTTP | 讀取 | 支援 machine/state/deployment/resource filters 與 live keyset；creation ceiling 只擋新插入列，不凍結 mutable state |
| Job detail / desired state | `operator.Service.JobDetail`＋`JobEvidence`；`internal/operator/job_read.go`、`job_evidence.go` | ✅ safe summary＋typed evidence | ✅ `GET /v1/operator/jobs/{id}` safe detail＋`/evidence` typed disclosure | ✅ `job show`／`job evidence` HTTP | 讀取 | safe detail 只回 desired metadata；desired spec 與 rejection detail 改由 `schema_version=6` evidence DTO 揭露（scrub、依欄位 `max_bytes` 截斷、bytes/truncated/issues、code_known、event producer、八種獨立 verdict、expected／observed version、派工段） |
| Lease state | claim/renew Store methods；`internal/store/deploy.go` | ✅ status/expiry | ✅ safe status/expiry | ✅ list/show | **否；agent callback** | 只做觀測，不提供 operator claim/renew button；lease token 不進 read DTO |
| Job prerequisite graph | `job_dependencies`＋`NextJobForMachine`；`internal/store/deploy.go` | ✅ terminal state/evidence | ✅ safe job state/evidence | ✅ `job show/evidence` | **否；Hub writer** | 最多 128 條同機 immutable ordered edges；全成功才 runnable；terminal failure 由 Hub scheduler 向下收斂成 `DEPENDENCY_FAILED`；claim SQL＋DB trigger 雙層阻擋；同資源 revision 有序、不同資源依建立順序 |
| Event stream + payload | `JobEvents`; `internal/store/deploy.go` | ✅ typed metadata＋payload bytes；rejected detail 經 scrub/截斷 | ✅ safe metadata；evidence DTO 回 payload bytes 與 rejected code/detail | ✅ `job evidence` | **否；agent callback／Hub scheduler** | raw payload 永不揭露（只回 byte 數）；window 內最後一筆 rejected 事件的 code/detail 被解出並 scrub；每筆明列 executor-agent 或 dependency-graph producer |
| Verification evidence | `JobVerifications`; `internal/store/deploy.go` | ✅ typed rule/command/stdout/stderr（scrub、依欄位 `max_bytes` 截斷）＋producer/role/two clocks | ✅ safe counts；evidence DTO 回 typed rule/command/output、`reported_verified_at` 與 optional Hub `received_at` | ✅ `job evidence` | **否；agent callback** | 新 row 原子記 producer/role/authority/Hub clock；legacy 不回填不存在的 clock。Executor 段的 producer 仍是 job machine 的 executor agent |
| 跨故障域證據 | `Store.RecordIndependentVerification`＋`EvaluateIndependentVerdict`＋stable promotion gate；`internal/store/verifier.go`、`promote.go` | ✅ `/jobs/{id}` 證據段＋部署／更新 review 的逐 target gate | ✅ evidence DTO v6、deployment preview v4、updates v5 | ✅ `job evidence` 與 deployment review | **否；verifier callback** | 工作單終態仍只採 executor；stable promotion 才要求 active fleet peer 的完整三規則 report 與 exact release version。八種 evidence verdict、十一種 gate state 各有 exact 下一步 |
| 獨立 verifier registry | `Store.RegisterVerifier`／`RevokeVerifier`；`internal/store/verifier.go`、`operator_verifier.go` | —（尚未補 registry 導覽入口） | ✅ preview/apply/list/detail/revoke-preview/revoke 六條 route | ✅ `verifier list/show/register/revoke` | 是；`admin` preview→apply | sampleagent3 的 fleet peer 已上線；只有 `fleet_peer_agent` 授予 stable gate，撤銷保留列與證據 |
| Noop protocol drill | `operator.Service.CreateDiagnosticNoop`；`internal/store/operator_diagnostic.go` | ✅ `/machines/diagnostics` preview/apply | ✅ preview/create | ✅ `job create --kind noop` HTTP／break-glass | 是，診斷 | 固定 noop spec、不改機器設定；revision、occupancy、typed confirm、idempotency 與 atomic audit 已完成 |

`/jobs` 與 detail Web adapter 共用 canonical in-process operator service；JSON／CLI 使用明列的
safe projection，不直接序列化 Store model，也不輸出 desired spec、raw event payload、verifier
command、stdout/stderr 或 free-form detail；safe Store query 從資料庫層就不選取 raw content。
Agent event payload／verification excerpts 寫入上限為 64 KiB，verification command 為 16 KiB，
避免單列內容放大。`internal/web/templates/job.html` 不再持有 raw Store-model bridge：desired spec、
verifier rule/command/output 與從 raw payload 解出的 rejection code/detail 全部經 `operator.Service.JobEvidence`
的 `schema_version=6` typed DTO（invalid UTF-8/control/Cf scrub、每欄依自報 `max_bytes` 截斷並帶 bytes/truncated/issues、
每段 1..100 筆、rejection `code_known`／`payload_decodable`），`GET /v1/operator/jobs/{id}/evidence` 與
`job evidence` 回同一份；event/rejection producer 明確區分 executor agent 與 Hub dependency scheduler；
新 event/verification row 保存 producer ID/kind、role、authority 與 row provenance，verification 另保存 Hub
received_at，legacy row 保留 provenance 未記錄／received_at null。`independent` 段的 verdict 八選一
（`absent`／`producer_revoked`／`digest_mismatch`／`release_mismatch`／`stale`／`failed`／
`release_unreported`／`passed`）依固定優先序取第一個成立者；
每列帶第二個 producer 的 kind／verifier ID／display name／failure domain／state、`verifier_bearer` authority，
以及 verifier 自報的 `reported_verified_at` 與 Hub 的 `received_at` 兩個時鐘；`digest_matches_job` 由 Hub
比較 `observed_digest` 與工作單 digest 得出，`version_matches_job` 由 Hub 比較 desired spec 的 exact version
與 `current_release` 的 `observed_version` 得出，不採信 verifier 結論，也不解析 stdout。
`independent_verifier` 由這張單的 live producer 數計算，不是常數，也不改變完成判定。
`independent.assignments`：
每一列說明誰被指派看這張單、何時指派，以及 `reported`／`producer_revoked`／`waiting_for_job`／
`awaiting_report` 四種互斥狀態之一，讓 `absent` 分得出「沒有人被指派」與「有人被指派但還沒回報」；
狀態全部由既有事實推導，沒有可被宣告成完成的欄位。List pagination 是 **creation-ceiling live keyset**：第一頁把當時已存在
的 job 列釘成上限，後頁不納入之後才插入的列，但 state、lease、event/verification counts 等 mutable
欄位不做 snapshot；state filter 的成員資格可能在翻頁間改變，total/counts 也應讀成各頁當下判決。
Cursor 只適用同一 ledger generation 的連續 traversal；stopped maintenance、restore 或手動 `VACUUM`
後要丟棄舊 cursor、重新讀第一頁，hidden rowid 不可當 durable generation token。
Agent claim、renew、event、verification、complete、reject callbacks 仍只接受 per-machine bearer，沒有
因 operator read slice 而變成人工 action。

### Endpoint security > Tailnet

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Roster / reconcile | `operator.Service.Tailnet`; `internal/operator/tailnet.go` | ✅ Tailnet Settings＋Enrollment 摘要 | ✅ | ✅ HTTP／break-glass | 是，reconcile | Source observation time、unavailable state 與四種 reconcile 分類已固定 |
| Ignored peers list | `TailnetPeerIgnores`; `internal/store/tailnet_operator.go` | ✅ active rules | ✅ | ✅ `tailnet` | 讀取 | Stable peer ID、hostname snapshot、reason、expiry、revision |
| Ignore / unignore | `PreviewTailnetPeerIgnore`／`ApplyTailnetPeerIgnore` | ✅ preview/confirm | ✅ | ✅ HTTP／break-glass | 是 | 1..366 天 expiry、typed hostname confirm、revision、idempotency、atomic audit |

Enrollment 明確只有 ignored count/提示，見 `internal/web/templates/enrollment.html`。

### Reports > Tickets / Audit / Changes

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Tickets ledger | `operator.Service.ListTicketsContext`；`internal/operator/ticket_read.go` | ✅ Intune-style filter/table/detail/CSV | ✅ `GET /v1/operator/tickets` | ✅ HTTP-first text/JSON/CSV；明示 `--db` 才進 fenced break-glass | 讀取／export | 固定 `[from,to]` received-time snapshot、7/14/30 UI window、1..30 API bound、opaque provider filter、成本上限與 safe evidence projection 已交付 |
| Audit ledger | `operator.Service.ListAudit`；`internal/store/audit_read.go` | ✅ 完整 filters／paging | ✅ safe read | ✅ HTTP／break-glass | 讀取 | 已交付 explicit allowlist、malformed evidence、creation-ceiling keyset 與 bounded denial sampling；跨 restore/VACUUM 須重開 traversal |
| Changes typed read | `operator.Service.ListChanges`；`internal/operator/change_read.go` | ✅ `/reports/changes` | ✅ `GET /v1/operator/changes` | ✅ `report changes` HTTP；bare `report` 保留 daily preview | 讀取 | Registry/state 是逐筆 transition；observations 是 `(from,to]` endpoint delta；Hub clock、bounded paging、safe DTO 與 retention/malformed coverage 已交付 |

Audit 的 action 可重複，其他 exact filters 包含 machine、outcome、principal、capability、source kind、
correlation 與秒精度 RFC3339 時間窗。`matched_total` 是 denial 採樣前符合數；預設只納入整段
traversal 最新 50 筆 `operator-denied`，明示 `denials=all` 才不採樣。Safe DTO 將畸形時間／結果
投影為 `null` 加 `issues`，無效 UTF-8 與 Unicode control/format 字元替換並列出
`altered_fields`。Cursor 綁 filters、writer sequence 與 creation ceiling；它仍是 live keyset，且
denial ledger 有最新 1000 筆 bounded ring，不能跨 restore、VACUUM 或 ring deletion 當 durable token。

Changes 的 registry/state 記錄來自 Hub append-only transition；identity、credential、CLI、systemd、
OpenClaw 則只比較 window 兩端最新 observation。排序與 `(from,to]` membership 一律用 Hub-owned
時間，agent `measured_at` 另列且不參與判定。Opaque cursor 固定 exact filters、window/evaluation
instant 與五個 source ceilings；retention 若刪除 continuation 所依賴的 observation，舊 traversal
以 `410` 直接失效。Exact filters 下推到 Store，`total`／`matched_total` 與 kind counts 都只計篩選後
集合。單次查詢硬限 253,000 筆 observation window rows、每個 selected source 253,000 筆 timestamp
metadata、10,000 endpoint keys、10,000 transitions、32 MiB candidate metadata、32 MiB endpoint
payload、兩個並行 reader 與 10 秒；過寬／忙碌／逾時分別是 `422`／`429`／`503`。
Safe projection 不輸出 raw payload、hostname/account、path/PID/argv/free-form text；未知 credential／
CLI／systemd subject 固定遮罩且不能 query，並把 retention、舊 ledger registry/state transition
tracking 與 malformed/unplaceable timestamp 都列入 coverage；未被 filter 選取的 source 明列
`not_applicable`。State current span 的同秒 overwrite
另由 append-only event ledger 捕捉；migration 只回填 surviving spans，不宣稱重建已遺失值。

### Tenant administration > Maintenance

| 功能 | Native source / API | UI | Operator API | CLI | 人工 action | 下一步 / 依賴 |
|---|---|---:|---:|---:|---|---|
| Retention status | `operator.Service.RetentionStatus`; `internal/store/operator_retention.go` | ✅ Maintenance＋Dashboard evidence | ✅ | ✅ preview | 讀取 | Policy、revision、never-run/zero-row-run 與上次刪除數共用 typed contract |
| Prune preview/apply | `PreviewRetentionPrune`／`ApplyRetentionPrune` | ✅ preview/confirm | ✅ | ✅ HTTP／break-glass | 是，破壞性 | Evaluation time、policy、四表 counts、protected newest、typed `DELETE N ROWS`、revision、idempotency、atomic retention ledger/audit |
| Restore drill preview/run/status | `operator.Service`；`internal/restoredrill`；`internal/store/restore_drill.go` | ✅ preview/confirm/operation detail | ✅ preview/enqueue/list/detail | ✅ HTTP／break-glass | 是，高風險 | 固定 standalone backup 身分與 SHA-256；durable async worker/startup fence；私有副本執行 schema migration 與 SQLite quick check；只有成功才寫 completion stamp；不替換 live DB |

## CLI transport debt

只有 `machines list/show/evidence`、`job list/show/evidence`、`audit [list]`、`deployment` 全組、`artifact list/show/fetch`（含
`artifact fetch list/show` operation status）、
`enroll-token` create／revoke、`machine channel`、`machine lifecycle`、`tailnet`、`prune` 與 `restore-drill` 的正常模式已走 operator HTTP API，見
`cmd/clawctl-hub/machinescmd.go`、`cmd/clawctl-hub/enrollcmd.go`、`cmd/clawctl-hub/enrollrevcmd.go`、
`cmd/clawctl-hub/machinecmd.go`、`cmd/clawctl-hub/jobreadcmd.go`、`cmd/clawctl-hub/deploymentcmd.go`、
`cmd/clawctl-hub/artifactreadcmd.go`、`cmd/clawctl-hub/artifactfetchcmd.go` 與
`cmd/clawctl-hub/auditcmd.go`、`cmd/clawctl-hub/reportchangescmd.go`、`cmd/clawctl-hub/tailnetcmd.go`、
`cmd/clawctl-hub/prunecmd.go`、`cmd/clawctl-hub/restoredrillcmd.go`。其餘舊命令
多由 `mustOpen` 直接開 writable Store，見 `cmd/clawctl-hub/main.go`：

| CLI | 現有能力 | 應收斂到的 submenu/API |
|---|---|---|
| `machines` | machine list / safe detail / typed free-text evidence | Devices > All devices；`machines evidence` 支援逐 section `--limit`；預設以 deterministic discovery 走 HTTP，僅明示 `--db` 進 stopped-service break-glass |
| `machine lifecycle`；`retire` alias | read/preview/apply active/retired | Devices > Lifecycle；retire preview 顯示當下開啟的終端工作階段數，apply 前若非零會提示套用時仍開啟者會結束；兩方向都要 typed confirm/reason；僅明示 `--db` 進 stopped-service canonical service |
| `report`, `report changes`, `tickets` | legacy daily preview、typed Changes、typed Ticket usage text/JSON/CSV；`tickets` 預設 discovery HTTP，明示 `--db` 才進 stopped-service fenced break-glass | Reports |
| `tailnet` | reconcile、stable-ID ignore/unignore；預設 HTTP，明示 `--db` 才進 stopped-service canonical service | Endpoint security > Tailnet |
| `prune` | retention preview/apply；預設 HTTP，明示 `--db` 才進 stopped-service canonical service | Tenant administration > Maintenance |
| `restore-drill` | restore preview/run/list/show；預設 HTTP，明示 `--db` 才進 stopped-service canonical service | Tenant administration > Maintenance；durable async operation/status 與 writer-fenced break-glass 已收斂 |
| `job list`, `job show`, `job evidence` | filtered global list、safe detail、typed bounded evidence（`--limit` 1..100） | Devices > Jobs；預設 deterministic discovery HTTP，僅明示 `--db` 進 stopped-service fenced break-glass |
| `audit [list]` | filtered safe audit ledger | Reports > Audit；預設 deterministic discovery HTTP，僅明示 `--db` 進 stopped-service fenced break-glass |
| `job create` | noop protocol drill | Devices > Diagnostics；Web、operator JSON、HTTP CLI 與 stopped-service `--db` 共用 canonical service |
| `deployment` | preview/create/list/show/continue/retry/abandon | Deployments；正常 read/preview 走 HTTP，四個 mutation 走 preview→private recovery→apply；只有明示 `--db` 才進 stopped-service canonical service |
| `artifact` | list/show/fetch；fetch list/show 為 operation status | Apps > Artifacts／Fetch／Operations；預設 deterministic discovery HTTP，只有明示 `--db` 進 stopped-service fenced break-glass；HTTP enqueue 有 private recovery receipt |
| `catalog` | package list/add、profile list/publish、machine assign/recover | Apps > Store／Profiles／Assignments；只走 deterministic discovery HTTP；每個 apply 先 preview，typed confirmation 後建立 private canonical recovery receipt；package／profile／assign／recover 完整輸出後才清除，broken pipe 保留原檔供重試 |

完整 command anchors：`cmd/clawctl-hub/main.go`、
`cmd/clawctl-hub/tickets.go`、`cmd/clawctl-hub/jobcmd.go`、`cmd/clawctl-hub/jobreadcmd.go`、
`cmd/clawctl-hub/deploymentcmd.go`、`cmd/clawctl-hub/artifactreadcmd.go`、
`cmd/clawctl-hub/artifactfetchcmd.go`、`cmd/clawctl-hub/auditcmd.go`、
`cmd/clawctl-hub/catalogcmd.go`、`cmd/clawctl-hub/catalogrecovery.go`、
`cmd/clawctl-hub/reportchangescmd.go`、`cmd/clawctl-hub/machinelifecyclecmd.go`。

## Delivery order and dependencies

| 優先 | Vertical slice | 原因 / 前置依賴 |
|---|---|---|
| Delivered | Shared list reads：Machines／Jobs／Deployments | 都有 safe DTO、共用 service、filters、bounded paging 與 creation-ceiling live keyset；都明列不是跨頁 snapshot |
| Delivered | Reports Changes typed safe read | Web／JSON／CLI 共用 operator service；Hub clock、transition/endpoint-delta 語意、safe DTO、bounded paging、retention/lifecycle/malformed coverage 已交付 |
| Delivered | Machine Lifecycle | Retire/restore 的 native read/preview/apply、獨立 revision、active-job blocker、atomic transition/idempotency/audit 及 UI/API/CLI parity 已交付；retained bearer/pending-ticket 恢復語意已明列；`c91695b` 已 live 驗收（retired→retired no-op 與 CLI/JSON replay） |
| Delivered | Noop protocol Diagnostics | Devices > Diagnostics、operator JSON 與 HTTP-first CLI 共用 fixed-spec preview/apply；revision、active-job occupancy、typed confirm、idempotency、atomic desired/job/receipt/audit 已交付；`c16d17c` 已 live 驗收 |
| Delivered | Deployment preview/create/continue/retry/abandon | 已共用 artifact provenance、target snapshot、promote lock、control revision、idempotency、audit 與 UI/API/CLI adapters；stable promotion 另逐 canary target 要求 active fleet-peer 的完整三規則 report |
| Delivered | Artifact list/detail/fetch operation + Updates read | Artifact catalog、fixed-origin async intake、durable recovery/fencing、Apps 的 Artifacts／Fetch／Operations 已具 API/UI/CLI adapters；Updates composition 已具 API/UI/strict operator client，全部共用 native services |
| Delivered | Standard Store / Profiles / Assignments | 已驗證 artifact → fixed Node/OpenClaw/Hermes manifest → reviewed profile → dependency-expanded、可 supersede 的 machine assignment；Web、8 條 operator JSON routes、strict Go client 與 HTTP CLI 共用 native services；所有 apply 具 typed confirmation、idempotency、atomic audit 與 exact recovery |
| Delivered | Audit safe read | Reports > Audit 的 Web、JSON 與 CLI 共用 native operator service；具完整 filters、safe projection、creation-ceiling cursor 與 bounded denial sampling |
| Delivered | Jobs evidence disclosure | Web／JSON／CLI 共用 `operator.Service.JobEvidence` v5 typed bounded DTO（scrub、依欄位 `max_bytes` 截斷、1..100 筆）；HTML raw Store-model bridge 已拆；event/rejection 分辨 executor agent 與 Hub dependency scheduler，verification 保存 producer/role/authority 與 Hub received_at，legacy 明示未記錄；獨立段與派工段分開呈現，`independent_verifier` 由 live producer 證據推導 |
| Delivered | Machine systemd/run-summary/journal evidence disclosure | Web／JSON／CLI 共用 `operator.Service.MachineEvidence` v2 typed bounded DTO；systemd producer/two clocks/invalid、MainPID omission、state 非 outcome，以及文字的 producer/relay、redaction差異、read-failed/quiet/not-collected/undecodable 都明列；HTML 已拆掉 raw unit 與 free-text bridge 欄位；`ecb0175` 已 live 驗收 |
| Delivered | Machine credential/occupancy evidence disclosure | `MachineEvidence` 升為 v3；credential 六態／verification method／續期史／bounded peers 與 occupancy completed-run Hub-ledger aggregation 都有 producer、clocks、bounded text、total/truncated/invalid；active account ID、secret fields、profile/session/job/model/raw-error event details 結構性排除，HTML 的 `Store.Detail` credentials/occupancy bridge 已刪；`b5d86fc` 已 live 驗收 |
| Delivered | Machine OpenClaw/CLI evidence disclosure | `MachineEvidence` 升為 v4；OpenClaw install/runtime/DB 與 CLI tool rows 有 typed allowlist、clocks、paging/invalid、獨立版本來源/conflict 及 display-only raw version。專用 host path／DB location／PID 欄位排除，只留下 count/boolean/relationship；bounded reason/raw text 明列未做內容掃描。HTML 的 `Store.Detail.OpenClaw`／`CLITools` bridge 與 queries 已刪；`a45c022` 已 live 驗收 |
| Delivered | Machine monitor/identity/resources/recent-history typed detail | `MachineDetail` 升為 v3；24h/1000 check-ins、20 state spans、100 non-expiring identity hints 與 identity/resources observations 都有 bounded allowlist、agent/Hub clocks、unknown/invalid/truncated、machine-bearer producer 與 exclusion disclosure。Host identity/Tailscale IP 明示 included；connect/pending/expectation paths excluded；state reason display-only、不解析／不掃 path。HTML 與 human/JSON CLI 已改讀 typed sections；`12a5866` 已 live 驗收 |
| Delivered | Machine judgement/finding typed detail | `MachineDetail` 升為 v4；Hub-derived state/reason 與 100 筆 finding page 都有 bounded text、total/invalid/truncated 與明示 disclosure。Active judgement 鎖定 fleet state；retired retained diagnosis 明列 `affects_fleet_state=false`。文字不解析、不掃 path/secret，known dedicated secret fields 結構排除；HTML 與 human/JSON CLI 已改讀 typed judgement；`9a77c99` 已 live 驗收 |
| Delivered | Machine expectation/artifact/event typed detail | `MachineDetail` 升為 v5；unconfigured/read-failed/no-applicable 三態、50 條 display definitions、artifact stat 與 20-entry event enum/count pages 都有 clocks、unknown/invalid/truncated。Artifact freshness 不讀內容且不是 outcome；event failure enums 由 operator 宣告、不掃 free text，config-path 專用欄位/parser fields/raw content excluded；HTML 與 human/JSON CLI 已改讀 typed expectations；`7082e5d` 已 live 驗收 |
| Delivered | Machine pending enrollment typed HTML read | Device detail 的 pending card 改讀既有 `operator.Service.PendingEnrollToken`，與 `GET /v1/operator/machines/{id}/enrollment-token` 共用只含 identity/timestamps/expired 的 contract；token plaintext/hash 不進 page model，多張未兌換票或 malformed identity fail closed。`store.Detail.Pending` 與附帶 query 已刪；`faa0659` 已 live 驗收 |
| Delivered | Machine BAT connect typed BFF | Device detail 與 connect POST 共用 `MachineConnect` v1 typed projection；public detail v5 仍排除 coordinates，BFF 不另設 JSON route，HTTP decoded detail 也不能取得 private source。只接受 literal HTTPS IP、合法 port，以及 explicit listener 或最新 decoded identity IP；registry IP 僅展示、永不形成 URL。保留 observed/decoded/invalid、兩個 clocks、20-entry bound、bounded display text 與 producer/disclosure；credentials 排除、Hub 不做代理、invalid/unavailable fail closed。四種 button audit outcome 另由 operator service 固定 identity／shape，Web direct `RecordAudit` 為 0；audit 寫失敗不阻擋 redirect，copied URL 不在涵蓋內。HTML raw `Store.Detail` bridge 已刪；`c82de07`／`713f276` 已 live 驗收 |
| Delivered | Tailnet Settings | Web／operator JSON／strict client／HTTP-first CLI 共用 stable-ID reconcile 與 ignore preview/apply；source freshness、typed confirmation、expiry/revision、idempotency 與 atomic audit 具合約測試；`8abdd43`／`8df9d5a` 已 live 驗收 |
| Delivered | Retention Maintenance | 租戶管理 > 維護、operator JSON、strict client 與 HTTP-first `prune` 共用 policy/revision-bound preview/apply；typed destructive confirmation、protected-newest counts、idempotency，以及 delete/ledger/receipt/audit atomicity 已由合約測試固定；`320e0fb` 正式環境 read/preview 驗收完成，未為部署測試刪資料 |
| Delivered | Restore-drill Maintenance | 租戶管理 > 維護、operator JSON、strict client 與 HTTP-first `restore-drill` 共用 pinned-backup preview、durable async enqueue/list/detail；worker 在私有副本以正式 schema 驗證，startup/run-token fence、typed failure、idempotency 與 atomic operation/receipt/audit 已由合約測試固定；`b2dced9` 已正式部署並以 operation `d40a949ad11d569a45ba8f865f791c31` 完成非破壞演練與 replay 驗收 |
| Delivered | 派工（指名 verifier 驗某一張工作單）| 工作單頁面板、確認頁、operator JSON preview/apply、strict client 與 `verifier assign` 共用同一寫入契約；`verification_assignments` 沒有 claimed_at／lease／state 欄位。Fleet peer 要在最新派工後送齊三種必要規則，partial report 繼續 pending；其他 verifier kind 一列即可。Hub 不下發指令，verifier bearer 也看不到未指派的工作單 |
| Delivered | Independent actual-state verifier 與 stable promotion gate | sampleagent3 的 fleet-peer runner 以獨立 bearer／主機／SSH 路徑回報三條編譯在 binary 裡的規則；正常啟動的 15–20 秒 HTTP 窗口由 5 秒間隔、最多 30 秒 settling 吸收。工作單終態不等 verifier，stable 才逐 canary target 要求完整 report；`current_release` 另回結構化版號，Hub 對 canary exact version 判斷。Web／JSON／CLI 顯示十一種 gate state 與 exact 下一步 |
| Pending | Default 四 CLI＋BAT 釘版交付 | Claude Code、Codex、Grok、Antigravity 與 Linux BAT Server 已有官方 fetch、adapter、standard catalog 與量測路徑。完整預設 profile 與跨平台實機驗收仍待完成 |
| Pending | Pinokio optional 庫 | 指派時釘版、升級 check + approve。本樹沒有 Pinokio catalog／adapter |
| Pending | Windows agent 最小切片 | 與 macOS 同為一等平台。本樹 Windows amd64／arm64 已可交叉編譯；credential ACL、user scheduled task installer／觀測、Task Scheduler 記錄摘要、Hub bootstrap 雙架構組與 Node runtime adapter 已接線；實機 enrollment 與 OpenClaw／Hermes 仍待完成 |
| P2 | Reports Tickets + Apps drilldowns | Tickets 先補 exact upper bound、truncation/disclosure evidence 與 HTTP-first CLI；最後統一 export/filter 與 resource/deployment linkage |

每個新的 operator mutation 在進 UI 前，都必須具備共用 validation、preview/impact、權限、
必要確認、idempotency/revision、structured audit，以及 API/UI/CLI parity tests。詳細 Definition
of Done 維持單一來源於 `docs/API-SURFACE.md`，本文件不重複實作規格。

Catalog scheduler: durable SQLite schedules and unique slot receipts; Hub ticks
at 30 seconds, skips overlap and unavailable targets, discards missed slots on
restart, and pauses changed script hashes with `script_changed`. Configuration
uses revision checks and preview/apply receipts. Scheduled jobs remain normal
`script_v1` jobs, audited as `scheduler:<name>` and attributed by schedule name.

BAT sweep report pilot: embedded `bat-sweep-report-v1`, read-only connector
calls from an admin-approved digest-pinned connector tree, structured JSON
classifications (excerpts off by default), bounded reads, fixed no-symlink mapping
and explicit unknown cleanup/Jev gates; disabled
hourly schedule example and [comparison runbook](runbooks/BAT-SWEEP-PILOT.md).

P2-01 disk policy security: JSON applies reject local-account sessions before
replay (local users apply through the reauthenticated UI); protection removals
require per-path digest-bound acknowledgement; MCP projects only path counts and
digests; sanitized decision reasons are retained on the approval ledger and audit.
See `docs/API-SURFACE.md` for the exact request fields and transport restrictions.

### P2-03 deep disk analysis

Delivered: pinned read catalog scanner, private typed chunk/complete channel,
30-day finalized / 24-hour incomplete report retention, audited conditional
minute slots, fresh strictly-above-policy pressure trigger, and shared six-hour
manual/weekly/pressure coalescing. The machine Disk UI previews/applies manual
reports and Sunday UTC templates with an explicit offset; the ranked viewer
filters the full report before pagination and uses human-readable sizes.
JSON and optional MCP `disk_report_get` expose sanitized display paths. Human
admins can inspect full paths in HTML. P2-04 adds exact human approval, rejection, quarantine and restore controls;
agent delegation remains deferred.

### P2-04 human quarantine contract

Exact manifests and consumed human approvals bind pinned quarantine/delete/restore
jobs. Human mutations require admin and fresh reauth; they are not token eligible.
MCP uses redacted projections; delegation remains deferred. Daily expiry
(`10 6 * * *` UTC) selects only due items from consumed original quarantine approvals
under the current policy. The assigned machine alone uploads private action receipts
and fetches signed grants. See [disk maintenance](MAINTENANCE-DISK-CLEAN.md#p2-04-human-decisions-quarantine-and-restore)
for routes, human UI, recovery, readiness and metrics.
### P2-05 timer handoff and disk alerts

Delivered: machine Ownership panel with local timer facts, canary agreement table,
reauthenticated handoff/rollback reviews and exact local commands; fleet Disk alerts
history and reason-bound acknowledgement; authenticated global alert banner;
MCP `disk_alerts_list`, `disk_ownership_get`; disk status and hub supervision counts.
Six JSON and four HTML operations are listed in
[API-SURFACE](API-SURFACE.md#timer-handoff-and-disk-alerts-p2-05).
Root helper is unavailable in the current adapter, so root remains local-owned.
Screenshots use generic fixture data under `docs/screenshots/phase2/p2-05/`.

## Fly self-deployment safety (ops only)

`ops/fly/deploy.sh` verifies fresh snapshots of all `clawctl_data` volumes before deployment and retries transient builder failures before fallback. This host operation adds no Hub web, API, MCP, or store contract. Human operators use the script; see [Fly deployment](DEPLOY-FLY.md) for the explicit snapshot bypass and recovery limits.

## Sustained disk growth alerts

Humans configure growth thresholds on Disk policies and review critical growth
episodes on Disk alerts; machine detail and Disk pages show the top growing
mount, GB/hour, coverage, samples, and estimated hours to full. Empty states link
to configuration or explain the next agent observation. Acknowledgement,
notifications, retries, and recovery use existing disk alert flows. The operator
status API and path-digested MCP disk status expose the same evidence.

## Agent no-progress loop episodes

| Capability | Human UI | Operator API / MCP | Coverage |
|---|---|---|---|
| Loop evidence and machine panel | Attention / Loops; machine detail | `GET /v1/operator/loops`; `loops_list` | Optional check-in activity; bundled agent reports heartbeat calls and observation-upload progress |
| Threshold and window | Admin form on Attention / Loops | Human UI only | Generic default >50 calls / 10 minutes; bounded sliding window |
| Acknowledge / resolve | Admin episode buttons | Human UI only | Ack preserves evidence; progress or manual resolution closes an episode; continued calls may reopen |

Legacy agents and uninstrumented tools have unknown coverage. Usage uploads and
job-event prose are not treated as identical-call evidence. Empty states link to
machines and explain the reporting prerequisite. No process control or deployment
is performed by loop detection. See [API surface](API-SURFACE.md#loop-episodes-ui-first).

### TLS certificate expiry and SAN coverage

- **UI:** Tenant → Certificates (`/tenant/certificates`): add/edit/remove endpoint,
  concrete expected hostnames, owner/notes and warning-day thresholds; queue a
  probe; inspect issuer, SANs, missing names, verification errors, attempt and
  observation times, sorted days remaining and next action. Empty state leads to
  the add form. View reads; human Admin manages configuration.
- **Monitoring:** opt-in inventory (no default targets), hourly TLS-only probes,
  five-second timeout/four concurrent probes, system chain verification, correct
  DNS wildcard/IP SAN matching, default 30/14/7 expiry warnings, expired,
  chain-invalid, SAN-gap, probe-failed and stale evidence flags. Existing notifier
  ledger/backoff handles daily condition alerts. No certificate issuance or deploy.
- **Automation:** operator API CRUD/read and queued probes; MCP `certificate_list`
  and `certificate_get`. Shared operator service and atomic mutation auditing;
  revision checks protect edits and fence probes. See `docs/API-SURFACE.md`.
- **Tests:** generated certificates served by `httptest` TLS servers cover trust,
  expiry, wildcard depth and missing SANs; store tests cover revisions and retained
  evidence, and UI/API/MCP/client tests cover the operator workflows.
