# API / UI / CLI 能力盤點

> 這是控制面 parity 的帳本，不是行銷功能表。只要正常操作仍只有 CLI，
> 就留在「缺口」欄；高風險不是把入口藏起來，而是補 RBAC、preview、確認、
> 冪等與 audit。共同成功語意見 [CONTROL-PLANE-CONTRACT.md](CONTROL-PLANE-CONTRACT.md)。
> 逐一對照 221 個 HTTP operations（18 non-operator＋203 operator；107 條 operator JSON）、
> native Store、UI／CLI 與 Intune-style submenu 的完整清單，
> 見 [FEATURE-INVENTORY.md](FEATURE-INVENTORY.md)。

## 1. 四個 plane 不混權限

| Plane | 呼叫者 | 目前邊界 | 用途 |
|---|---|---|---|
| Agent transport | endpoint `clawctl-agent` | enroll 之後使用 per-machine bearer | 心跳、觀測、領單、lease、event、verification、artifact |
| Operator | Web、CLI、automation | direct tailnet + LocalAPI `WhoIsForIP` + exact grants app capability；browser write 再經 stdlib CSRF | console read、desired state 與編排寫入 |
| Verifier transport | 獨立 verifier（external job runner／fleet peer／hub prober）| 註冊時一次性發放的 per-verifier bearer；與 machine bearer 是不同表、不同 hash namespace | 只寫跨故障域驗證證據 |
| Read / telemetry | Prometheus、deadman | `GET /metrics` requires the operator `view` grant; a managed machine receives 403. `GET /healthz` stays a content-free public probe | health、metrics |

Machine bearer、verifier bearer 與 operator credential 三者互不承認：machine bearer 送到 `POST /v1/verifications` 回 401，verifier bearer 送到 `POST /v1/jobs/{id}/verifications` 也回 401，兩個 bearer 都不能當 operator credential。Operator route 不解析 `Authorization`，
而是逐 request 問 tailscaled 並驗 route manifest 對應的 `view`／`operate`／`admin`
fixed capability。四者沒有程式內繼承。typed display name 仍是防誤觸，不是登入；
Tailscale 證明 node 與登入該 node 的 owner，不是 fresh keyboard/MFA assertion。
完整契約見 [OPERATOR-AUTH.md](OPERATOR-AUTH.md)。

Operator auth boundary、Tailscale grant、deterministic CLI discovery 與 writer fence 已由
`bf39c04` 上線；enroll-token slice 亦由 `88fb5ff` 在 live Hub 驗收。部署證據的最新實機紀錄是私人工作筆記，不在這個公開倉庫。

## 2. 現有 JSON HTTP surface

### Agent transport

| Method | Route | Authority | 語意 |
|---|---|---|---|
| `POST` | `/v1/enrollments` | one-time enroll token | 建立 machine credential |
| `POST` | `/v1/checkins` | machine bearer | 心跳與 agent instance |
| `POST` | `/v1/observations:batch` | machine bearer | append actual-state observation |
| `GET` | `/v1/jobs/next` | machine bearer | 只取自己的下一張單；回應帶 resource kind/ID、revision、spec 與 artifact digest；合規性動作生效中的機器回 `204`，恢復報到後不需要任何人解鎖就再領得到 |
| `GET` | `/v1/agent/readiness` | machine bearer | 回傳這個 credential 對應 machine ID 的最新 Hub-owned check-in receipt，以及最新 agent-measured identity evidence 的 `identity_received_at`（Hub 時鐘）、`identity_measured_at`（agent 時鐘）、OS、arch，四欄來自同筆已存證據；macOS bootstrap 用 agent 量測時間與本機啟動時間比較，確認新證據已落帳且解析為執行中 binary 的 darwin/arch、同時 exact process 與 fixed sync executor capability 已回讀後成功；缺少量測時間時仍待證據；舊 agent 的 capability 保持未回報 |
| `POST` | `/v1/jobs/{id}/claims` | machine bearer + lease rules | 認領工作單 |
| `POST` | `/v1/jobs/{id}/lease:renew` | machine bearer + fencing token | 續租 |
| `POST` | `/v1/jobs/{id}/events` | machine bearer + fencing token | append executor event；同 seq 不同 canonical body 回 `409 JOB_EVENT_CONFLICT` |
| `POST` | `/v1/jobs/{id}/verifications` | machine bearer + fencing token | append verification evidence |
| `POST` | `/v1/jobs/{id}/complete` | machine bearer + fencing token | 要求 Hub 依 evidence 收終態 |
| `POST` | `/v1/jobs/{id}/reject` | machine bearer + fencing token | activation 前拒單 |
| `GET` | `/v1/capabilities` | machine bearer | 協定能力協商 |
| `GET`, `HEAD` | `/v1/artifacts/{sha256}` | machine bearer + active exact job/digest binding | 只下載該 machine 當前 active job 綁定的 Hub artifact；不提供 catalog enumeration 或 path substitution |

### Verifier transport

| Method | Route | Authority | 語意 |
|---|---|---|---|
| `POST` | `/v1/verifications` | verifier bearer | append 跨故障域驗證證據。Verifier write `schema_version=2`；通過的 `openclaw.current_release` 必須帶結構化 `observed_version`，失敗列與其他 rule 禁帶。INSERT…SELECT 內以 `verifiers.failure_domain <> jobs.machine_id` 為條件，同故障域或憑證已撤銷都回 `403 VERIFIER_NOT_ELIGIBLE` 且零列寫入；不收 lease token，也不改 lease、不參與 `Store.CompleteJob` 的完成判定 |
| `GET` | `/v1/verification-assignments` | verifier bearer | 只回 operator 明確指派給這個 credential、且工作單已終態、尚未有自己證據的派工。沒有 filter 參數，也沒有「我有資格驗哪些」的列舉：separation rule 允許的範圍是整個機隊少一台，那正是這個平面不該提供的枚舉。回應只帶 assignment/job/machine 識別與指派時間，沒有 rule、command 或 artifact digest |

Verifier bearer 只開這兩條 route。它不能領單、不能續租、不能報 event、不能收終態，
也不能讀任何 operator 讀模型，更不會從 Hub 收到要執行的指令 —— 一個會跑 Hub 下發指令的
verifier 不是第二個判斷，是一條用單一 bearer 控管的遠端執行管道。Hub 主機已入冊時，`hub_prober` 的 failure domain 必須等於
它在同一份 registry 的 machine ID，因此「peer 驗自己」與「hub prober 驗 Hub 自己」是同一個比較，
不是兩條特例；只有 Hub 主機完全不在名冊上時才接受字面值 `hub`。

### Operator

| Method | Route | UI | CLI transport | 現況 |
|---|---|---|---|---|
| `GET` | `/v1/operator/machines` | All devices 與 Dashboard SSR 直接共用 in-process operator service | `machines` 預設由 explicit URL → env → `operator.json` discovery 走 HTTP；`--db` 僅 stopped-service break-glass，且 workload policy identity 必須與 Hub 最後發布值一致 | `schema_version=2`；exact machine/display、repeatable state、lifecycle/reporting/channel、1..100 limit 與 filter-bound cursor；registry creation ceiling + live health，不是 snapshot；safe presentation DTO；分母只看 active/retired，頂層 legacy `expected` 數必須等於 `active`，逐列 legacy `expected` 不參與重數；不接受沒有可管理 producer 的 `expected` filter |
| `GET` | `/v1/operator/machines/{id}` | machine detail SSR 的 judgement、expectations/artifact/event evidence、monitor、identity、resources、check-in/state history 直接共用 in-process typed service；pending card 另讀 dedicated typed operator service，BAT connect 另讀 coordinate-bearing `MachineConnect` v1 BFF projection；不再有 raw Store bridge | `machines show <machine-id>` 預設走 discovery HTTP，human／JSON 都讀同一 DTO | `schema_version=6` explicit bounded allowlist（v6：resources 的記憶體與負載改成 typed absence —— `mem_measured` 由 Hub 從「總量為 0 不可能是量測結果」推導，`load_1m` 可為 null 因為 0.00 是合法的已量到值）：Hub judgement 含 4 KiB reason 與最多 100 筆 findings；expectations 分開保留 unconfigured／config read failure／configured-but-no-applicable-rules，最多 50 條 display definitions。Artifact observation 明列 observed/decoded/invalid/read_failed/exists/mtime 與 agent/Hub clocks；event evidence 最多各 20 個 operator-declared failure enums、declared／undeclared counts，另保留 partial coverage、malformed、unknown 與 truthful total/invalid/truncated。Artifact freshness probe 不讀內容且不等於 work outcome；event observer 只解析人宣告的結構化 enum 欄位，不掃 free text 關鍵字，raw content 不進 DTO。Config path 專用欄位與 parser 欄位結構性 excluded，但 artifact path 與 bounded why/error 是 display-only、可能含嵌入路徑，不做 path/secret 掃描／遮罩；known dedicated secret fields 仍由結構排除。Active judgement 須與 fleet state 相等；retired 保留 diagnosis 但 `affects_fleet_state=false`。心跳固定 24h／1000 筆、狀態歷史 20 段、identity hints 100 筆；producer 是 machine-bearer observer agent、非獨立 verifier，存活只採 Hub received_at。BAT URL/argv/listener 與 pending enrollment 仍 excluded；HTML 的 pending metadata 改由 `/enrollment-token` 同一 service contract 提供、不回 token/hash；BAT HTML 另用不設 HTTP route 的 `MachineConnect` v1，保留 observed/decoded/invalid、agent/Hub clocks、20 個 listener 上限、bounded argv/why、producer 與 coordinate source；URL 只從 explicit non-loopback listener 或最新 decoded identity Tailscale IP 形成，永不 fallback 到 enrollment-time registry IP |
| `GET` | `/v1/operator/machines/{id}/evidence` | machine detail 的 OpenClaw、CLI tools、credentials、occupancy、systemd observations、run summaries／journals 由同一 in-process service 的 typed evidence 渲染，不再經 `Store.Detail` raw 欄位 | `machines evidence <machine-id>` 預設走 discovery HTTP；`--db` 僅 stopped-service break-glass | `schema_version=6` typed bounded disclosure；OpenClaw/CLI 分開保留 CLI/gateway/upstream/running/package sources 與 source conflict，不折成單一版本；unsupported raw version 只顯示、不解析。專用 host path、DB location 與 process ID 欄位結構性排除，只公開 observed/count/boolean 及由 agent 路徑座標推導的 `same|different` relationship；CLI tool 另帶 `process_scan`（`complete`／`restricted`／`unavailable`／空字串＝舊 agent）說出這一輪的 process 掃描本身有沒有跑完，掃描沒跑完時 `running_reason` 不得被讀成「沒有在跑」；bounded reason/raw text 不做 path/PID/secret 掃描或遮罩。Credentials 明列六態、驗證方法、續期史／bounded peers 與 agent/Hub clocks，但 status 不等於 session validity，active account ID／secret fields 結構性排除；occupancy 只由完成回合的 append-only Hub ledger 聚合，保留上游 provider/source、不用 process、不分類 error，profile/session/job/model/raw error 等 event details 結構性排除，另列 latest scan 的 rows_seen／rows_without_provider 與 clocks。Systemd unit/name/state/restart/two clocks 與 summary/journal 各段同樣有 1..100 筆、truthful total/truncated/invalid；volatile MainPID 排除且 `active/running` 不是 work outcome。所有文字逐欄 scrub/截斷並帶 bytes/truncated/issues；另分 observed/decoded/db_observed、read_failed/quiet/not-collected/undecodable，固定揭露 producer/relay、machine-bearer authority、非獨立驗證及各段 redaction 差異 |
| `GET` | `/v1/operator/jobs` | `/jobs` SSR 直接共用 in-process operator service | `job list` 預設由 explicit URL → env → `operator.json` discovery 走 HTTP；`--db` 僅 stopped-service break-glass | machine/state/deployment/resource filters、1..100 limit、opaque cursor；safe DTO 與 creation-ceiling live keyset |
| `GET` | `/v1/operator/jobs/{id}` | `/jobs/{id}` SSR 共用同一 in-process service 的 safe detail | `job show <job-id>` 預設走 discovery HTTP | safe detail 只回 desired/event/verification metadata，不回 raw evidence |
| `GET` | `/v1/operator/jobs/{id}/evidence` | `/jobs/{id}` SSR 以同一 in-process service 的 typed evidence 渲染，不再有 raw Store-model bridge | `job evidence <job-id>` 預設走 discovery HTTP；`--db` 僅 stopped-service break-glass | `schema_version=6` typed、bounded disclosure：desired spec、event metadata＋payload byte 數、window 內最後一筆 rejected 的 code/detail、verification rule/command/stdout/stderr；每欄經 invalid UTF-8/control/Cf scrub，並以該欄自報的 `max_bytes` 截斷（spec/command/stdout/stderr 16 KiB、rule_id 256、reported_phase 與 rejection code 64），帶 bytes/truncated/issues；`disclosure.max_field_bytes` 是各欄上限的最大值，不是逐欄承諾；每段 1..100 筆並標 total/truncated。Event producer 可為 machine-bearer `executor_agent`，或依賴失敗時的 `hub_scheduler`／`dependency_graph`；rejection 連同同一 producer projection。新 event/verification row 保存 producer ID/kind、evidence role、authority 與 `provenance_recorded=true`，verification 另存 Hub `received_at`；legacy row 保留 `provenance_recorded=false`，legacy verification 的 `received_at=null`。Executor 段的 verification role 仍只有 executor。`independent` 是獨立的第二段：verdict 是 absent／producer_revoked／digest_mismatch／release_mismatch／stale／failed／release_unreported／passed 八選一，依此優先序取第一個成立者；每列帶 producer 的 kind／verifier ID／display name／failure domain／state、`verifier_bearer` authority 與 `independent_verifier` role，並同時保留 verifier 自報的 `reported_verified_at` 與 Hub 的 `received_at`。`observed_digest` 與工作單 digest 的比較只由 Hub 計算成 `digest_matches_job`；`observed_version` 只綁通過的 `openclaw.current_release`，並由 Hub 對 desired spec 的 exact OpenClaw 版號算出 `version_matches_job`，兩者都不採信 verifier 的結論，也不解析 stdout。`total` 是證據列數，`live_producers` 是仍有效的 distinct verifier 身分數；同一 verifier 的多條規則或多次派工不會膨脹 producer 數。`disclosure.independent_verifier` 由這張單的 live producer 數計算，不是常數；它為 true 也不改變工作單完成 gate——`Store.CompleteJob` 仍只採 executor 證據；stable promotion 另用 fleet-peer 完整 report 與 exact version 判斷。`independent.assignments` 每一列說明誰被指派、何時指派、以及四種互斥狀態之一（`reported`／`producer_revoked`／`waiting_for_job`／`awaiting_report`），讓 `absent` 這個 verdict 分得出「沒有人被指派」與「有人被指派但還沒回報」。Fleet peer 要該次派工後三個必要 rule ID 全到齊才是 reported；狀態由既有事實推導，沒有 state 欄位可以被宣告成完成。不回 raw payload、lease token 或 `created_by` |
| `POST` | `/v1/operator/machines/{id}/diagnostic-noop-preview` | Devices > Diagnostics 的確認頁 | `job create --kind noop --machine <id> --preview` | `operate`；綁 machine lifecycle、最新 heartbeat 的 `jobs_enabled`、active-job occupancy、OpenClaw revision、timeout 與固定 noop spec；execution 未回報或未啟用時回 typed blocker |
| `POST` | `/v1/operator/machines/{id}/diagnostic-noop-jobs` | typed machine-name confirm 後建立並導向 job detail | `job create --kind noop --machine <id> --confirm-name <name> --reason <reason>` | `operate`；desired state、job、immutable receipt 與 audit 同一 transaction；fresh `201`、replay `200`，回 Location 與 revision ETag |
| `GET` | `/v1/operator/artifacts` | Apps > Artifacts；Updates 亦消費同一 read model | `artifact list` 預設走 discovery HTTP；`--db` 僅 stopped-service break-glass | status/version/limit/filter-bound cursor；bounded no-follow metadata-only scan，list 的 `available_unverified` 不代表本 request 已 hash bytes |
| `GET` | `/v1/operator/artifacts/{artifact_id}` | Artifact detail | `artifact show <artifact-id>` 預設走 discovery HTTP | canonical identity 可為 64 位小寫 SHA-256，或 isolated malformed evidence 的 `invalid-<64hex>`；以已開啟的 no-follow descriptor 檢查，可信材料完整重算 SHA-256 且相符才回 `ready`／`verified_at`，並與 deployment preview/apply 共用單目錄限流及 request cancellation；safe DTO 不回 URL、path 或 fetched-by identity |
| `GET` | `/v1/operator/catalog-manifests` | Apps > Store package list | `catalog package list` | `view`；package/kind/limit/filter-bound cursor；public DTO 只回 canonical manifest、digest 與 publication time |
| `POST` | `/v1/operator/catalog-manifests/standard-preview` | Store package settings → review | `catalog package add --preview` | `admin`；從已驗證 artifact 產生固定 Node/OpenClaw/Hermes manifest；OpenClaw 綁 exact 已發布 Node；OpenClaw/Hermes 固定互斥且同屬 `primary-agent-runtime` exclusive group |
| `POST` | `/v1/operator/catalog-manifests` | Store typed package/version confirm | `catalog package add` preview→private recovery→apply | `admin`；重新完整驗證 artifact；manifest、immutable receipt 與 audit 同 transaction；fresh `201`，replay/already-published `200` |
| `GET` | `/v1/operator/machine-profiles` | Apps > Profiles list | `catalog profile list` | `view`；profile/limit/filter-bound cursor；只回 public profile、digest 與 publication time |
| `POST` | `/v1/operator/machine-profiles/preview` | Profile settings → review | `catalog profile publish --preview` | `admin`；完整驗證所選 packages 並回 canonical profile/digest |
| `POST` | `/v1/operator/machine-profiles` | typed profile/revision confirm | `catalog profile publish` preview→private recovery→apply | `admin`；profile、immutable receipt 與 audit 同 transaction；fresh `201`，replay/already-published `200` |
| `POST` | `/v1/operator/machines/{id}/profile-assignment-preview` | Apps > Assignments settings → review | `catalog assign --preview` | `admin`；綁 exact active machine/platform/profile，解析完整 dependency order、prerequisites、job 數與 blockers |
| `POST` | `/v1/operator/machines/{id}/profile-assignments` | typed machine-name confirm | `catalog assign` preview→private recovery→apply；`catalog recover --recovery-file` 以原 key/request 回放，不重新 preview；完整輸出後才清除 recovery，輸出中斷保留原檔 | `admin`；一個 transaction 建立 dependency-ordered desired states、jobs、edges、receipt 與 audit；fresh `201`、already assigned／replay `200`；過期預覽 `412`，拒絕回放保留原 code；空 `prerequisite_packages` 回 `[]` |
| `POST` | `/v1/operator/artifact-fetches/preview` | Apps > Fetch settings → review | `artifact fetch PACKAGE@VERSION --preview` | `admin`；OpenClaw 綁 npm tarball SHA-512／engine；Node runtime 綁 nodejs.org checksums 與 deterministic multiarch bundle；Hermes 綁 Docker Hub official image index、完整 Linux amd64/arm64 OCI descriptor closure 與 deterministic bundle policy |
| `POST` | `/v1/operator/artifact-fetches` | typed package/version confirm | `artifact fetch` preview→private recovery→enqueue；可 `--wait` | `admin`；durable async enqueue，fresh/replay success 均為 `202`，replay 另有 header/body evidence；operation、immutable idempotency receipt 與原始 audit 同 transaction |
| `GET` | `/v1/operator/artifact-fetches` | Apps > Operations | `artifact fetch list` 預設走 discovery HTTP | state/name/version filters；safe durable operation ledger，不回 tarball URL、run token、idempotency key、request digest 或 private reason |
| `GET` | `/v1/operator/artifact-fetches/{id}` | Fetch operation detail | `artifact fetch show <operation-id>` 預設走 discovery HTTP | queued/running/succeeded/failed、phase、monotonic progress、attempt、result digest/size 或 bounded failure |
| `GET` | `/v1/operator/updates` | Updates 的概觀／Artifacts／Canary／Stable 共用 in-process service | —；strict Go operator client 已交付 | `schema_version=5`；channel/status/version/limit/cursor；同一 evaluation instant 組合 metadata-only artifacts、safe members、`last_observed_version`＋Hub-trusted `last_observation_received_at`、latest deployment、rollout plan 與 stable promotion gate。Stable 的 promotion 帶 `independent_required`、通過 target 數，以及每台 machine 與 job 的十一種 typed state 與下一步；不把沒有 freshness threshold 的歷史 evidence 稱為 current |
| `GET` | `/v1/operator/deployments` | All／Active／Paused-Stuck／Finished ledger submenu 共用 in-process operator service；New 另做 isolated metadata-only catalog scan，只列 `available_unverified` 且具有可信 record 的候選材料 | `deployment list` 預設走 discovery HTTP；`--db` 僅 stopped-service break-glass | channel/state/stuck filters、1..100 limit、filter-bound live keyset cursor；safe summary；真正 deployment preview/apply 仍完整驗 artifact bytes |
| `GET` | `/v1/operator/deployments/{id}` | Deployment detail 共用 safe DTO | `deployment show` 預設走 discovery HTTP | `schema_version=3`；targets、material summary、batch/revision 與三個 action eligibility；revision 耗盡會明列 blocker。每個已開單 target 帶 `independent`（八種 verdict 之一、rows、live_producers），其中 `live_producers` 對該 target 的工作單按 distinct verifier 身分計數，不按規則列或重派次數；沒有開單的 target 為 `null`——沒有工作單就沒有東西可驗，與「有工作單但沒人驗」是兩件事，後者是 `absent`。另有 deployment 層的 `independent` rollup：`opened_targets`、`passed_targets`、逐 target 的 `live_producers` 加總與固定列出全部八種 verdict 的 target 計數，全部由 targets 推導，strict client 對不上就拒收整個回應。這個 rollup 只描述該 deployment；stable promotion 另依 latest canary lineage、派工、完整規則組與 exact release version 計算 gate。不回 raw spec/created_by |
| `POST` | `/v1/operator/deployments/preview` | 新增部署 settings → review | `deployment preview` 與 create 前自動 preview | `schema_version=4`；重新驗 artifact bytes，固定 target snapshot、promotion decision 與 `preview_digest`。Stable promotion 逐台回 `independent_targets`（machine/job、十一種 state、typed next step）及通過數；任一 effective successful canary target 未通過便 fail closed |
| `POST` | `/v1/operator/deployments` | typed channel/version confirm | `deployment create` preview→apply | `admin`；fresh `201`、成功 replay `200`；拒絕 replay 保留原錯誤 status；artifact/policy stale 與 idempotency conflict 明確分開 |
| `POST` | `/v1/operator/deployments/{id}/continuation-preview` | detail → Continue review | continue 前自動 preview | `operate`；`schema_version=4`；精確下一批或 finish outcome，回 control/opened-batch precondition；若含 stable promotion，使用與 create preview 相同的 typed 獨立證據 gate |
| `POST` | `/v1/operator/deployments/{id}/continuations` | typed channel confirm | `deployment continue` preview→apply | `operate`；只開下一批或收尾。失敗批次回 `DEPLOYMENT_CONTINUE_REFUSED`，不把失敗 target 當 retry |
| `POST` | `/v1/operator/deployments/{id}/skip-failed-batch-preview` | detail → skip failed batch review | 失敗批次的單獨 preview | `operate`；digest 與 Continue 不同；opened batch 必須有失敗終態，且還有下一批 |
| `POST` | `/v1/operator/deployments/{id}/skip-failed-batches` | typed channel confirm plus a reason | deployment page `skip failed batch` | `operate`；理由寫進稽核與 Hub event。MCP 沒有這條工具。不收成 finished |
| `POST` | `/v1/operator/deployments/{id}/retry-preview` | detail → Retry review | retry 前自動 preview | `operate`；只列 exact terminal-failure identities，排除 silent/nonterminal targets |
| `POST` | `/v1/operator/deployments/{id}/retries` | typed channel/version confirm | `deployment retry` preview→apply | `operate`；建立有 `retry_of` lineage 的新 attempt，fresh `201` |
| `POST` | `/v1/operator/deployments/{id}/abandonment-preview` | detail → Abandon review | abandon 前自動 preview | `admin`；明列不再開單 targets，要求已開 jobs 全終態 |
| `POST` | `/v1/operator/deployments/{id}/abandonments` | full deployment ID confirm | `deployment abandon` preview→apply | `admin`；paused → finished，保留原 jobs/evidence |
| `GET` | `/v1/operator/machines/{id}/channel` | machine detail | `machine channel` 正常模式在 PUT 前先讀 | 回 display name、channel、revision 與 `ETag`；CLI 不得拿 display name 自動代填 typed confirmation |
| `PUT` | `/v1/operator/machines/{id}/channel` | machine detail form | 預設由 explicit URL → env → `operator.json` discovery 走真實 HTTP；`--db` 僅 stopped-service break-glass | 要求 JSON、`Idempotency-Key`、typed name、`expected_revision`；持久化 success / rejection replay |
| `GET` | `/v1/operator/machines/{id}/assigned-user` | 機器明細與 Devices 指派使用者欄 | `machine assigned-user` 寫入前讀取；`machines show` 顯示指派使用者 | `view`；回使用者識別碼、登入名稱與版本；`ETag` 為 `"assigned-user-revision-N"`；未指派顯示「未指派」 |
| `PUT` | `/v1/operator/machines/{id}/assigned-user` | `admin` 預覽指派後輸入機器名稱確認 | `machine assigned-user --machine ID --user USER_ID或none --confirm-name NAME`；可指定 `--hub-url` 或 `--db` | `admin`；JSON 欄位 `user_id`、`expected_revision`、`confirm_display_name`，要求 `Idempotency-Key`；`none` 取消指派；重放帶 `Idempotency-Replayed: true`；來源不可用為 `503 TAILNET_SOURCE_UNAVAILABLE`，使用者不在名冊為 `400 ASSIGNED_USER_NOT_IN_ROSTER`，輸入無效為 `400 BAD_ASSIGNED_USER` |
| `GET` | `/v1/operator/machines/{id}/actions` | machine detail 的「動作」那一節；每個可用動作連到同一頁上那張表單 | `machine actions --machine <id>`（`--json` 出同一份 DTO） | `view`；`schema_version=5` 的裝置動作目錄，九個具名動作中 retire／restore 只出現會改變狀態的方向，所以這台的指派使用者在 Linux 機器上最多八列，其他人最多七列。每個 action 的 available 直接取自守住它寫入路徑的 preview／read contract；重新命名與編輯名冊備註對 active／retired 都可用，輸入與 stale 由各自的專用 preview 判斷。被擋的動作回 typed blocker、現況與下一步；沒有下一步的 blocker 不硬編一句。只列出 principal 握有 capability 的動作，view-only 拿到空清單而不是按不動的按鈕。每個動作帶封閉的 `surface`：`operator-api` 的動作必須指到一條真的註冊過、是 JSON、是 operator API、而且要求同一個 capability 的路由；`surface=web`（`connect` 與 `open_terminal`）沒有 operator API 路徑，`method`／`path` 缺席。`open_terminal` 只出現給這台機器的指派使用者，而且只出現在 Linux 機器上。這條由 `TestEveryOperatorAPIMachineActionMatchesItsRegisteredRoute` 對著路由表守住 |
| `POST` | `/v1/operator/machines/{id}/display-name-preview` | machine detail 的「重新命名」review 頁 | `machine rename --machine <id> --set <name> --preview`；apply 未帶 replay coordinates 時也會先呼叫 | `admin`；只讀、`Cache-Control: no-store`。驗 1..256 bytes、UTF-8、邊界空白／控制字元與跨名冊重複；digest 綁 machine ID、目前名稱、新名稱與固定影響。回應明列 machine ID、agent、機器 hostname 不變，名稱型 expectations 改用新名稱，以及 pending enrollment ticket 標籤是否會更新 |
| `PUT` | `/v1/operator/machines/{id}/display-name` | review typed-confirm 後套用，完成後仍以 immutable machine ID 導回 detail | `machine rename --machine <id> --set <name> --confirm-name <current> --reason <reason>`，只走 HTTP operator API | `admin`；要求 JSON、`Idempotency-Key`、目前名稱確認、reason 與 preview digest。套用時重驗目前名稱與唯一性；名冊名稱、所有未兌換 ticket 的標籤、immutable receipt 與 audit 同 transaction。machine ID、agent credential、token/hash、hostname 與 lifecycle 不變；名稱型 expectations 自此以新名稱匹配。成功／拒絕 replay 都回原判決，不再次改名 |
| `POST` | `/v1/operator/machines/{id}/notes-preview` | machine detail 的「名冊備註」review 頁 | `machine notes --machine <id> --set <notes> --preview`；apply 未帶 replay coordinates 時也會先呼叫 | `admin`；只讀、`Cache-Control: no-store`。備註可為空字串（清除），其餘要求 UTF-8、最多 1000 bytes，不接受邊界空白、控制字元或 format control；digest 綁 machine ID、目前名稱、舊備註、新備註與固定影響 |
| `PUT` | `/v1/operator/machines/{id}/notes` | review typed-confirm 後套用，完成後導回單機動作 | `machine notes --machine <id> --set <notes> --confirm-name <current> --reason <reason>`；`--set ''` 清除，只走 HTTP operator API | `admin`；要求 JSON、`Idempotency-Key`、目前名稱確認、reason 與 preview digest。以目前名稱／舊備註 CAS 寫回名冊，receipt 只記錄變更前後是否有備註，audit detail 只記錄更新或清除，兩者都不複製備註內容。machine ID、機器設定與 agent 不變；active／retired 都可更新，成功／拒絕 replay 不再寫入 |
| `GET` | `/v1/operator/machines/{id}/lifecycle` | Devices > Lifecycle 與 machine detail 的原生狀態來源 | `machine lifecycle --machine <id>` 預設走 discovery HTTP | 回 `active|retired`、獨立 `lifecycle_revision` / `ETag`、由 lifecycle 唯一決定的分母、保留 credential/pending-ticket 存在性、當前驗證／兌換是否允許及非終態 job 數；不回 legacy `expected` 或任何 secret/hash |
| `POST` | `/v1/operator/machines/{id}/lifecycle-preview` | Lifecycle 清單／detail → 確認頁 | `machine lifecycle --set active|retired --preview` 或 apply 前自動預覽 | `admin`；綁 exact machine/revision/state、分母 delta、channel/history 保留、bearer 驗證與 pending-ticket 兌換前後影響；另回 preview-time `open_agent_session_count`，但不綁 preview digest；退役遇非終態 job 回 `nonterminal_jobs` blocker |
| `PUT` | `/v1/operator/machines/{id}/lifecycle` | 原 `retire` / `unretire` Web routes 皆改為 canonical apply adapter | `machine lifecycle --set ... --confirm-name ... --reason ...`；`--db` 僅 stopped-service break-glass | `admin`；兩方向都要 exact display-name、reason、preview digest、expected lifecycle revision 與 `Idempotency-Key`；退役會結束套用時仍開啟的終端工作階段；projection＋transition event＋immutable receipt＋audit 同 transaction，fresh/replay 均回 `200`，apply response／receipt 不帶 preview-time session count |
| `POST` | `/v1/operator/machines/{id}/diagnostic-noop-preview` | Devices > Diagnostics → confirmation | `job create --kind noop --machine <id> --preview` 或 apply 前自動預覽 | `operate`；固定 noop spec，綁 machine lifecycle、最新 heartbeat 的 `jobs_enabled`、active-job occupancy、OpenClaw revision、timeout 與 preview digest；unknown/false 不建立工作單 |
| `POST` | `/v1/operator/machines/{id}/diagnostic-noop-jobs` | typed machine-name confirm；成功後導向 job detail | `job create --kind noop --machine <id> --confirm-name ... --reason ...`；`--db` 僅 stopped-service break-glass | `operate`；fresh `201`／replay `200`，desired state、job、idempotency receipt 與 audit 同 transaction；回 job Location 與 revision ETag |
| `POST` | `/v1/operator/enrollment-tokens/preview` | Machines > Enrollment 的 review 頁 | `enroll-token` create 前或 `--preview` | 純 preview；驗 display name 與 60..86400 秒 TTL，回到期時間、分母影響與 policy-bound `preview_digest`，不建 machine/token/idempotency state |
| `POST` | `/v1/operator/enrollment-tokens` | review confirm | `enroll-token` 正常模式由 discovery 走 HTTP；`--db` 僅 stopped-service break-glass | 要求 preview digest 與 `Idempotency-Key`；fresh `201` 只在該 response 回一次明文，replay `200` 只回 redacted receipt；復原必須撤原票、退役原本未報到的名冊列，再建立新列與票，沒有原 machine reissue |
| `GET` | `/v1/operator/machines/{id}/enrollment-token` | machine detail 的 pending-ticket 狀態 | —；revoke CLI 直接取較完整的 revocation preview | 只回這台 machine 唯一一張尚未兌換的 enrollment ticket 與到期狀態；不存在、已用、已撤銷或多張歧義狀態都不冒充 active credential／成功結果 |
| `POST` | `/v1/operator/machines/{id}/enrollment-token/revocation-preview` | revoke review | `enroll-token revoke --preview` 或 mutation 前自動呼叫 | exact `{}` 純讀 preview；digest 綁 ticket identity 與固定影響：registry 保留、分母變化 0、active agent credential 不受影響 |
| `POST` | `/v1/operator/machines/{id}/enrollment-token/revocations` | revoke confirm | `enroll-token revoke` 正常模式走 HTTP；`--db` 僅 stopped-service break-glass | 要求 preview digest 與 `Idempotency-Key`；fresh `201`、redacted replay `200`，刪除 exact pending ticket 並將 receipt、idempotency、audit 同 transaction commit |
| `GET` | `/v1/operator/audit-events` | `/audit` SSR 共用同一 operator service | `audit [list]` 預設由 deterministic discovery 走 HTTP；`--db` 僅 stopped-service break-glass | machine/action/outcome/principal/capability/source-kind/correlation/time/denials filters、1..100 limit 與 filter-bound cursor；explicit safe allowlist、creation-ceiling live keyset 與 bounded denial sampling |
| `GET` | `/v1/operator/reports` | `/reports` 落地頁共用同一 operator service | `report list`（`--json` 出同一份 DTO） | `view`；`schema_version=1` 的報告清單。每一份報告說出自己回答什麼、範圍多大、看得到多遠、能不能整份匯出。「看得到多遠」讀的是這台 Hub 現行的保留期，並取它與該報告單次讀取上限的較小者；不接受任何 query parameter |
| `GET` | `/v1/operator/daily-report` | — | `report` 預設由 deterministic discovery 走 HTTP；`--db` 僅 stopped-service break-glass | `view`；`schema_version=1` 的每日早報本文預覽。`since_seconds` 只接受 canonical 整數 1..2592000，預設 86400；回 Hub 的整秒評估時刻、窗起點、秒數與 exact notification body。GET 不送出通知也不寫推播紀錄；官方 client 重驗時間窗、64 KiB 本文上限、UTF-8、結尾換行與終端控制字元 |
| `GET` | `/v1/operator/machines/{id}/timeline` | machine detail 的「事件時間軸」 | `machine timeline --machine <id>`（`--json`／`--csv`） | `view`；`schema_version=1`、1..30 天（預設 7）的單機事件時間軸。名冊、健康判定、工作單與操作員動作由新到舊排成一條，每一列都來自該頁自己在用的讀取器，不另外重查；每一個來源各自交代讀到幾列與有沒有讀完這段期間；窗的兩端取整秒以對齊稽核的時間篩選 |
| `GET` | `/v1/operator/data-disclosure` | `/tenant/data` 揭露面共用同一 operator catalogue | `data`（`--json` 出同一份 DTO） | `view`；`schema_version=1` 的資料揭露面。十四類資料涵蓋二十四張存著 `machine_id` 的表，各自說出留的是什麼、誰產生的、含不含自由文字、留多久、退役之後還剩什麼、去哪一頁看。「哪些表存著機器的資料」由 SQLite 自己回答，「哪些表會被清、看哪一個保留期」由真的在刪東西的 prune 表與已驗證的 FK cascade 回答；不接受任何 query parameter |
| `GET` | `/v1/operator/machines/{id}/data` | machine detail 的「資料」 | `machine data --machine <id>`（`--json`／`--csv`） | `view`；同一份目錄加上這台實際的列數、最舊與最新、現在的清除界線。時間一律用 Hub 自己的鐘；沒有 Hub 時刻的列照樣算進列數但不進最舊 / 最新；不接受任何 query parameter |
| `GET` | `/v1/operator/enrollment-report` | `/reports/enrollment` 與 `/reports/enrollment.csv` | `report enrollment`（`--json`／`--csv`） | `view`；`schema_version=1` 的註冊報告。名冊就是分母，每一列剛好落在一個階段，各自附「這是什麼」與「下一步」。「報到過沒有」讀機隊清單那一份投影，「票過期了沒有」讀單機頁在用的同一條 SQL 判準；離開分母只有一條路：退役。名冊超過 2,000 列時拒絕回一份少算的分母；不接受任何 query parameter |
| `GET` | `/v1/operator/software-report` | `/reports/software` 與 `/reports/software.csv`，總覽的「工具版本」也讀同一份 | `report software`（`--json`／`--csv`） | `view`；`schema_version=2` 的軟體清查。每一台每一個工具一格，六種互斥且窮盡的狀態各自附「這是什麼」與「下一步」；「沒回報過」與「這台上沒有」是兩件事，不併成一個「沒有」。分母與註冊報告共用同一份名冊投影，退役的機器不出現也不決定「最新是哪一版」，只有退役那台有過的工具整列都不在。「最新」指的是這個機隊裡看到的最新版，不是上游發布的最新版——這個 Hub 沒有上游版本來源，所以那句限制隨報告一起送出。版號用點分數字比大小，比不出來的既不當基準也不被講成落後。⚠ 裝著東西的那幾格還帶第二個軸 `runtime`：那個版號講的是不是正在跑的那一份，七種互斥且窮盡（量的就是跑的／正在跑的是另一個檔案／正在跑的那個檔案已經不在磁碟上／有 process 在跑它但說不出跑的是哪一個檔案／process 偵測沒有跑到底／沒有找到在跑它的 process／這一筆觀測沒有講 process 的事），各自附「這是什麼」「下一步」與它講的那兩個檔案。它跟 `shadowed` 是兩個獨立的軸，不合成一個布林。只有中間那兩種算進 `misattributed`——「我不知道」不是發現，算進去機隊上每一格都會變成待辦事項；`misattributed` 在摘要與下一步裡都排在版號不一致前面，因為那幾格的版號量的是沒在跑的那一份，拿它們比「誰比較新」比的是沒有人在用的檔案。`running_exe` 對 node CLI 一律是解譯器，所以對不上安裝路徑的 exe 算「說不出來」，不算「另一個檔案」。這一軸不回答「哪一份是 Hub 放的」——agent 還沒把那件事講進觀測裡，由 Hub 比對路徑長相等於猜。分母超過 2,000 台或回報超過 200 個工具時拒絕回一份少算的清查；不接受任何 query parameter |
| `GET` | `/v1/operator/install-report` | `/reports/install` 與 `/reports/install.csv` | `report install`（`--json`／`--csv`） | `view`；`schema_version=2` 的每機安裝狀態。每一台每一個資源一格，十種互斥且窮盡的狀態各自附「這是什麼」與「下一步」。「指派的」是這台最後真的收到的那一筆安裝意圖：machine scope 與它所在 channel 的 scope 合起來取 revision 最大的一筆——revision 的計數器 key 是**資源**不是 scope，所以兩種 scope 永遠比得出先後，agent 用來擋跨 scope 降版的 `MaxSeen` 走的是同一條規則，這一頁不會跟機器實際收到的那一筆不一致。`{"kind":"noop"}` 的診斷單不是安裝意圖，它不會蓋掉它前面那一筆真的指派。對不起來的情況拆成五種不同的答案而不是一個「未知」：指派那一筆沒講版號、東西在但問不到版號、機器回報說它上面沒有、機器有回報但回報裡沒有這個東西、機器從來沒回報過——五種各自有各自的下一步。「指派的比看到的舊」不講成落後也不講成安裝失敗：機器上的東西可以從指派以外的路徑裝上去，那句限制隨報告一起送出，每一個平面都印。這台回報說它上面有的那幾格再答第二個問題：**看到的那個版號量的是不是正在跑的那一份**——`runtime` 帶著軟體清查那一份 `ToolRuntimeOf` 的同一個判準與同一組句子（`other_file`／`gone_file`／`same_file`／`unattributed`／`unscanned`／`idle`／`unstated`），以及量版號的那個檔案與正在跑的那個檔案。只有 `other_file` 與 `gone_file` 算在 `misattributed`／`misattributed_on` 裡；那個數字在整份報告的那一行字與下一步上都**排在「指派的跟看到的不一樣」前面**，因為那幾格的版號量在一個沒有人在跑的檔案上，比方向比的是一個沒有人在用的檔案——正式庫 2026-09-12 samplehub1 就是這一格，照「指派的比看到的舊」動手會把一台其實沒事的機器部署回舊版。`misattributed_on` 跟 `matching_on`／`differing_on` 是兩個獨立的軸：一台「指派的跟看到的一樣」也可以是量錯了檔案。「沒有被指派過」是一種正面狀態，不是空白。分母與註冊報告共用同一份名冊投影，退役的機器不出現。分母超過 2,000 台或超過 200 個資源時拒絕回一份少算的報告；不接受任何 query parameter |
| `GET` | `/v1/operator/profile-report` | `/reports/profile` 與 `/reports/profile.csv` | `report profile`（`--json`／`--csv`） | `view`；`schema_version=2` 的發佈與指派對照。它回答的是每機安裝狀態問不到的那一半：一份發佈了卻一台都沒指派的 profile，在那一頁上不存在，因為那一頁的分母是機器。每一個已發佈的 profile revision 一列，四種互斥且窮盡的狀態各自附「這是什麼」與「下一步」：機隊上有機器穿著這一版／只有已退役的機器身上還是這一版／這個 profile 已經發佈到更新的版本／發佈了一台都沒指派。後兩種刻意分開——前者是舊 revision 的預期樣子，後者才是要被看見的那一列。每一列再把它點名的每一個（套件, 版本）拆成四種狀態：指派過也看得到／指派過但沒有一台回報它／沒有指派過但看得到／沒有指派過也沒有看到過。「指派過沒有」與「看到過沒有」是兩個獨立的軸，不併成一個「有沒有」：前者是工作單那一側的事，後者是有人從指派以外的路徑裝上去的。每一格的 `seen_misattributed_on` 另數 `seen_on` 裡有幾台量到的版號不是正在跑的那一份，整份的 `seen_misattributed` 數這種格子；量錯檔案的機器仍算看得到，這一軸不改四種狀態。有量錯檔案的那一格，下一步改成到每機安裝狀態核對執行檔與其版號（`operator.ProfilePackageNextStep`，Hub 與用戶端共用同一條規則）；摘要那一列的下一步仍是狀態那一句。⚠「指派過沒有」讀的是整條 `desired_state` 的歷史（`FleetIntentVersions`），不是現行意圖——拿「現在沒有人被叫去裝它」回答「從來沒有人被叫去裝它」，會讓一版指派過後來被蓋掉的套件顯示成從來沒有指派過。沒講版號的意圖不算一版。分母與註冊報告、每機安裝狀態共用同一份名冊投影；退役機器的觀測不算「機隊上看得到」，但它身上的指派照樣列出來。這個 Hub 沒有上游版本來源，所以它不講「那一版是不是最新」，那句限制隨報告一起送出。分母超過 2,000 台或發佈超過 500 版 profile 時拒絕回一份少算的報告；不接受任何 query parameter |
| `GET` | `/v1/operator/enrollment-limit` | `/machines/enrollment` 的「註冊上限」那一節 | `enrollment-limit`（`--json` 出同一份 DTO） | `view`；`schema_version=1` 的註冊上限。有沒有設上限、上限幾台、名冊上幾台、還可以再納管幾台，以及到了上限要再納管該做什麼。算的台數與註冊報告的分母共用同一條 SQL 判準與同一個 Go 投影，不各自算一次；沒有設上限回的是「沒有上限」，不是上限 0；不接受任何 query parameter |
| `POST` | `/v1/operator/enrollment-limit/preview` | 改上限的 review 頁 | `enrollment-limit --set N --preview` 或 `--clear --preview`，apply 前自動呼叫 | `admin`；純 preview，驗 0..10000 的台數，回現在的上限、要改成什麼、名冊上幾台、會不會立刻擋住開票，以及一個比目前台數還小的上限退役不了任何一台。digest 綁「要改成什麼」，不綁「現在有幾台」——後者是機隊隨時在動的事實，放進去會讓一份沒有問題的確認在有機器報到時失效 |
| `POST` | `/v1/operator/enrollment-limit` | review confirm | `enrollment-limit --set N --reason R` 或 `--clear --reason R` | `admin`；要求 preview digest、`expected_revision` 與 `Idempotency-Key`，fresh/replay 均回 `200`。取消上限是把 `limit_set` 寫成 0 的一次 upsert，不是刪列：`revision` 只往前走，才不會在設了又取消之後把一份過期的預覽重新變成有效的；replay 回的是今天的上限，不是當時改成什麼。preview 過期回 `412`，開票撞上限回 `409`——那是機隊現在的事實，不是送錯的 request |
| `GET` | `/v1/operator/changes` | `/reports/changes` SSR 共用同一 operator service | `report changes` 預設由 deterministic discovery 走 HTTP；`--db` 僅 stopped-service break-glass | exact machine/repeatable kind/subject、`(from,to]` 秒精度 window、1..100 limit 與 filter-bound cursor；transition + endpoint-delta typed safe DTO，明列 retention/malformed/registry/state coverage |
| `GET` | `/v1/operator/tickets` | `/reports/tickets` 與 CSV 匯出共用同一 operator service | `tickets` 預設由 deterministic discovery 走 HTTP；支援 text/JSON/CSV，`--db` 僅 stopped-service break-glass | 1..30 天固定 `[from,to]` Hub received-time snapshot、opaque provider filter、100 providers 與 per-field evidence bounds、250,000 rows／32 MiB candidate budget；completed 與 verified 分開 |
| `GET` | `/v1/operator/tailnet` | `/settings/tailnet` SSR 共用同一 in-process operator service | `tailnet` 預設由 deterministic discovery 走 HTTP；`--db` 僅 stopped-service break-glass | `view`；stable node ID、source observation time、peer count、未納管／在線無心跳／退役仍在線與 active ignore rules；來源不可用不冒充空清單，且仍回傳可管理的本地 ignore rules |
| `POST` | `/v1/operator/tailnet/peer-ignore-preview` | Tailnet Settings ignore/unignore review | `tailnet ignore|unignore --preview` 或 apply 前自動預覽 | `admin`；固定 stable peer ID、hostname snapshot、目前/套用後狀態、1..366 天 expiry、reason、revision 與 digest |
| `PUT` | `/v1/operator/tailnet/peer-ignores/{id}` | typed hostname confirm | `tailnet ignore|unignore --confirm-hostname ... --reason ...`；`--db` 僅 stopped-service break-glass | `admin`；expected revision、preview digest 與 `Idempotency-Key`；rule、immutable receipt、structured audit 同 transaction，fresh/replay 均回 `200` |
| `GET` | `/v1/operator/settings` | Devices > Configuration 的盤面 | `settings list` 預設走 discovery HTTP | `view`；一次回已發佈原則、目前指派、每台的 effective settings、verdict 與最後回報時間，以及 Hub 預設值；verdict 依 never_reported／unknown／mismatch／pending／applied 的優先序取第一個成立者，量不到的機器永遠不會是 applied |
| `POST` | `/v1/operator/setting-policies/preview` | 發佈設定原則 → review | `settings publish --preview` | `admin`；純讀；回 current/next revision、canonical settings digest、preview digest、是否 unchanged，以及今天解析到這個原則的機器數 |
| `POST` | `/v1/operator/setting-policies` | typed policy-id confirm | `settings publish --reason ...` | `admin`；`expected_revision` 不符回 `409`，preview digest 不符回 `412`；同值重發不產生新 revision；policy row、idempotency receipt 與 audit 同 transaction，fresh `201`、replay/unchanged `200` |
| `POST` | `/v1/operator/setting-assignments/preview` | 指派設定原則 → review | `settings assign --preview` | `admin`；純讀；回目前生效的原則與 revision、指派後的值、preview digest 與這個範圍涵蓋的機器數；原則或 revision 不存在回 `404` |
| `POST` | `/v1/operator/setting-assignments` | typed scope-id confirm | `settings assign --reason ...` | `admin`；scope 只接受 machine（須存在且未退役）或 channel（canary／stable）；指派同一 digest 不產生新 assignment revision；assignment、receipt 與 audit 同 transaction，fresh `201`、replay/unchanged `200` |
| `GET` | `/v1/operator/compliance` | Devices > 合規性 的盤面 | `compliance list` 預設走 discovery HTTP | `view`；一次回已發佈的合規性原則、目前指派、每台的判決與推出那個判決的每一條規則結果、原則的不合規動作對每一台做到哪一步（未觸發／寬限中／生效中），以及被停發工作單的台數與判決時間；判決與動作狀態都現算不落表，因為它們是「現在幾點」的函數 |
| `POST` | `/v1/operator/compliance-policies/preview` | 發佈合規性原則 → review | `compliance publish --preview` | `admin`；純讀；回 current/next revision、canonical rules digest、preview digest、是否 unchanged、不合規動作各自會做什麼與寬限期怎麼算的說明，以及今天由這個原則判決的機器數 |
| `POST` | `/v1/operator/compliance-policies` | typed policy-id confirm | `compliance publish --reason ...` | `admin`；`expected_revision` 不符回 `409`，preview digest 不符回 `412`；`actions[]` 寫在原則文件裡（目前唯一一種是 `block_jobs`，`grace_seconds` 0–86400），所以換寬限期或拿掉動作都是新的 revision；同一組規則與動作換順序寫仍是同一份，不產生新 revision；policy row、idempotency receipt 與 audit 同 transaction，fresh `201`、replay/unchanged `200` |
| `POST` | `/v1/operator/compliance-assignments/preview` | 指派合規性原則 → review | `compliance assign --preview` | `admin`；純讀；回目前生效的原則與 revision、指派後的規則與不合規動作、preview digest 與這個範圍涵蓋的機器數；原則或 revision 不存在回 `404` |
| `POST` | `/v1/operator/compliance-assignments` | typed scope-id confirm | `compliance assign --reason ...` | `admin`；scope 只接受 machine（須存在且未退役）或 channel（canary／stable）；指派同一 digest 不產生新 assignment revision；assignment、receipt 與 audit 同 transaction，fresh `201`、replay/unchanged `200` |
| `GET` | `/v1/operator/disk-clean/summaries` | 維護頁的磁碟清理一節 | `clawctl-operator call disk_clean_summaries` | `view`；不接受 query。回已指派或已回報的機器。每列含 Hub 判決、是否過期、config_digest 是否等於指派的 revision、Hub 自己的 disk_free_min_percent 評估，以及腳本的 attention。摘要裡的 root 物件只讀 |
| `GET` | `/v1/operator/disk-clean/summaries/{id}` | 同上，單機 | `clawctl-operator call disk_clean_summary` | `view`；名冊沒有這台回 `404`。已註冊但尚未指派的機器仍回一列，判決是 stale |
| `POST` | `/v1/operator/disk-clean/profile-preview` | — | `disk_clean_profile_preview` | `admin`；純讀。封閉的 profile 文件，未知鍵拒絕。回渲染後的 conf、config_digest 與 preview_digest。不承諾下一個 revision 號碼 |
| `POST` | `/v1/operator/disk-clean/profiles` | — | `disk_clean_profile_publish` | `admin`；`expected_revision` 是這個 scope 目前的 revision（沒有則 0）。不符回 `409`，preview digest 不符回 `412`。同一份 conf 不產生新 revision。fresh `201`，replay 或 unchanged `200`。Idempotency-Key 與 audit 同 transaction |
| `POST` | `/v1/operator/disk-clean/dry-run-preview` | — | `disk_clean_dry_run_preview` | `admin`；純讀。點名要跑可逆 dry-run 的機器。capability、jobs 與進行中的工作單是 blocker，不進 digest |
| `POST` | `/v1/operator/disk-clean/dry-runs` | — | `disk_clean_dry_run_apply` | `admin`；每台一張可逆工作單，並寫下指派。blocker 在 apply 回 `409`，不是 `412`。fresh `201`，replay `200` |
| `POST` | `/v1/operator/disk-clean/canary-preview` | — | `disk_clean_canary_preview` | `admin`；純讀。canary 恰好一台，而且必須在 machine_ids 裡。其餘機器要等 continue |
| `POST` | `/v1/operator/disk-clean/canaries` | — | `disk_clean_canary_apply` | `admin`；只開第一批，恰好那一台。每一台目標都必須已有這份 revision 的成功 dry-run。fresh `201`，replay `200`。不會在 continue 之前對其餘機器建立工作單 |
| `POST` | `/v1/operator/disk-clean/continuation-preview` | — | `disk_clean_continue_preview` | `admin`；純讀。digest 含 rollout 目前的 state，所以 preview 和 apply 之間若 reconcile 改變了狀態，apply 回 `412` |
| `POST` | `/v1/operator/disk-clean/continuations` | — | `disk_clean_continue_apply` | `admin`；要求 preview_digest、expected_control_revision 與 expected_opened_batch。只在 canary 成功且狀態是 paused 時打開其餘機器。沒有其餘機器則直接 finished，不建工作單 |
| `POST` | `/v1/operator/disk-clean/abandonment-preview` | — | `disk_clean_abandon_preview` | `admin`；純讀。放棄不會再開工作單 |
| `POST` | `/v1/operator/disk-clean/abandonments` | — | `disk_clean_abandon_apply` | `admin`；進行中的工作單還沒結束時拒絕。指派留著。fresh `201`，replay `200` |
| `GET` | `/v1/operator/maintenance/retention` | Tenant administration > Maintenance | `prune` 的 policy/status 契約 | `view`；回 canonical 秒級 policy、revision、never-run/zero-row-run 可區分的最新清理時間與刪除數 |
| `POST` | `/v1/operator/maintenance/retention/prune-preview` | Maintenance 清理 review | `prune` 或 apply 前自動預覽 | `admin`；固定 evaluation time、policy、revision、五張表 exact boundary／deleted／protected-newest counts、`DELETE N ROWS` 與 digest；zero-row 不建立 apply request |
| `POST` | `/v1/operator/maintenance/retention/prunes` | typed `DELETE N ROWS` confirm | `prune --apply --reason ... --confirm ...`；ambiguous retry 重用原 key、評估時間、revision、digest 與三個 policy 值；`--db` 僅 stopped-service break-glass | `admin`；revision/scope stale fail closed，`Idempotency-Key` 綁 canonical request；delete、五筆 retention log、immutable receipt 與 structured audit 同 transaction，fresh/replay 均回 `200` |
| `POST` | `/v1/operator/maintenance/restore-drill-preview` | Maintenance 還原演練 review | `restore-drill preview` 或 run 前自動預覽 | `admin`；固定最新 standalone backup 的 name/size/mtime/SHA-256、live expected、`VERIFY filename` 與 digest；不開 operation |
| `POST` | `/v1/operator/maintenance/restore-drills` | typed `VERIFY filename` confirm | `restore-drill run --reason ... --confirm ...`；ambiguous retry 同時重用原 key 與 preview digest；`--db` 僅 stopped-service break-glass | `admin`；durable async enqueue，operation、immutable receipt 與 structured audit 同 transaction；fresh `202`、replay `200` |
| `GET` | `/v1/operator/maintenance/restore-drills` | Maintenance recent operations | `restore-drill list` | `view`；1..100 bounded newest-first ledger，回 queued/running/succeeded/failed 與 safe evidence |
| `GET` | `/v1/operator/maintenance/restore-drills/{id}` | Restore-drill operation detail | `restore-drill show <operation-id>` | `view`；固定備份身分、attempt/timestamps、成功 counts 或 typed failure；不回 run token、private reason、request digest 或 idempotency key |
| `POST` | `/v1/operator/verifiers/preview` | —（本切片不加導覽入口） | `verifier register --preview` 或 apply 前自動呼叫 | `admin`；純 preview，驗 kind、display name 與 failure domain（`fleet_peer_agent`／`hub_prober` 的 domain 必須是名冊上未退役的 machine ID），回固定 policy（separation rule、`first-response-only`、撤銷保留列，以及只有 `fleet_peer_agent` 的 `grants_deployment_gate=true`）與綁住三者的 `preview_digest`；不寫 `verifiers`、`operator_idempotency`、`audit_log` |
| `POST` | `/v1/operator/verifiers` | —（本切片不加導覽入口） | `verifier register` preview→apply | `admin`；要求 preview digest 與 `Idempotency-Key`；registry row、redacted receipt 與 audit 同一 transaction；fresh `201` 只在該 response 回一次 credential 明文，replay `200` 完全不含 credential 欄位並回 `revoke_and_register`——沒有 reissue |
| `GET` | `/v1/operator/verifiers` | —（本切片不加導覽入口） | `verifier list` | `view`；active 與 revoked 兩段計數相加等於列數，id 與 display name 都不重複；只回 registry projection，沒有 credential 或 hash 欄位 |
| `GET` | `/v1/operator/verifiers/{id}` | —（本切片不加導覽入口） | `verifier show <verifier-id>` | `view`；state 與 `revoked_at` 必須一致，時鐘一律 UTC；`last_seen_at` 是 Hub 收到該 verifier 最後一次寫入的時間，不是它自己宣告的 |
| `POST` | `/v1/operator/verifiers/{id}/revocation-preview` | —（本切片不加導覽入口） | `verifier revoke --preview` 或 apply 前自動呼叫 | `admin`；exact `{}` 純讀 preview；digest 綁 verifier identity 與固定影響：registry 列保留、既有證據列保留、失去唯一 producer 的工作單數不超過受影響證據列數 |
| `POST` | `/v1/operator/verifiers/{id}/revocations` | —（本切片不加導覽入口） | `verifier revoke` preview→apply，retry 需帶 `--expected-revision` | `admin`；要求 preview digest、`Idempotency-Key` 與 revision CAS；憑證即刻失效，列與證據都不刪，該 verifier 為唯一 producer 的工作單 verdict 轉為 `producer_revoked`，display name 仍占用 |
| `POST` | `/v1/operator/verifiers/{id}/assignment-preview` | `/jobs/{id}` 的派工面板 | `verifier assign --preview` 或 apply 前自動呼叫 | `operate`；exact `{"job_id":…}` 純讀 preview；回兩邊識別、工作單目前狀態與固定 policy（separation rule、發單需終態、Hub 不下發指令，以及只有 `fleet_peer_agent` 授予部署閘）。Fleet peer 的 `satisfied_by` 是同一次派工後三個 exact rule ID 全到齊；其他 kind 維持一列證據即可。Digest 綁 verifier／failure domain／job／machine＋policy；刻意不綁工作單狀態，因為預覽到套用之間工作單結束是預期，不是 stale |
| `POST` | `/v1/operator/verifiers/{id}/assignments` | `/jobs/{id}` 的派工面板 | `verifier assign` preview→apply | `operate`；要求 preview digest、逐字相同的 `confirm_verifier_name` 與 `Idempotency-Key`；派工列、receipt 與 audit 同一 transaction。同一組 verifier＋job 可再派一次；fleet peer 要等該次派工後三條必要規則全到齊才不再出現在 polling 清單。派工永遠不決定工作單成敗；只有完成的 fleet-peer report 參與 stable promotion |

Machines list/detail/evidence 由 Web SSR 直接呼叫同一個 in-process operator service，不 hairpin 回自己的
JSON route。List 的 cursor 綁全部 filters 與首頁 registry insertion ceiling，後頁排除之後才建立的
machine；`matched_total` 與 ceiling 內 global totals 分開。這仍是 **live keyset**：既有列的 health、
channel、retire/unretire 與 filter membership 可在頁間改變。每次 response 的 evidence query
以該次 `evaluated_at` 的 Hub `received_at` 為上限，但不保證跨 query/table 的 atomic snapshot；restore、
`VACUUM` 或 stopped maintenance 後須丟棄 cursor。DTO 對 display name 做 invalid UTF-8/control/Cf
safe projection 並保留 issues/altered-fields，也不包含會夾帶 path／agent error 的 free-form reason 或
agent 自報版本；這些 evidence 必須等獨立 disclosure contract。
Jobs list/detail 也由 Web、JSON 與 CLI 共用 operator service。List 的 opaque cursor 綁住 filter 與
第一頁取得的 per-traversal creation ceiling，所以後頁不會混入之後才建立的 job；它仍是 **live
keyset**，不是 snapshot：既有 job 的 state、lease、event/verification counts 可以在翻頁間改變，
使用 state filter 時，成員甚至可能跨頁進出，total/state counts 也只是該次 evaluation 的讀值。
Cursor 只對同一 ledger generation 的連續 traversal 有效；stopped maintenance、restore 或手動
`VACUUM` 後必須丟棄舊 cursor 並重讀第一頁，server 不把 hidden rowid 當 durable generation token。
工作單可保存最多 128 條同機、immutable、ordered prerequisite edges。`/v1/jobs/next` 只回全部
prerequisites 已成功且同資源沒有較低 nonterminal revision 的 runnable head；不同資源的 revision
互不比較，以建立順序挑選。任一 prerequisite 進入 failed/rejected/lease_expired/manual_intervention，
Hub scheduler 會在同一交易把所有仍未開始的 descendants 收成 rejected，並各留一筆
`DEPENDENCY_FAILED` event；claim SQL 與 DB trigger 都會擋住依賴未滿足的 child。
JSON safe projection 省略 desired spec、raw event payload、verifier command、stdout/stderr 與
free-form detail；safe Store query 也不選取這些 raw content 欄位，事件／驗證只回受限 metadata
和 counts。Agent event payload／verification excerpts 的寫入上限是 64 KiB，verification command
是 16 KiB。HTML job detail 不再有 raw Store-model bridge：`/jobs/{id}`、`GET /v1/operator/jobs/{id}/evidence`
與 `job evidence` 共用 `operator.Service.JobEvidence` 的 typed disclosure contract（`schema_version=5`）。
desired spec、verifier rule/command/stdout/stderr 與 window 內最後一筆 rejected 事件解出的 code/detail
都經 invalid UTF-8/control/Cf scrub，並以各欄自報的 `max_bytes` 截斷（spec 與 verifier command/stdout/stderr 16 KiB、
rule_id 256 bytes、reported_phase 與 rejection code 64 bytes），帶 bytes/truncated/issues；`disclosure.max_field_bytes`
只是各欄上限的最大值，不是逐欄承諾；raw event payload 只回
byte 數；每段 1..100 筆並標 total/truncated；rejection code 另標是否屬於協定已知集合。每筆 event
明列 producer：agent callback 是該 job machine 的 `executor_agent`／machine bearer，依賴圖判決是
`hub_scheduler`／`dependency_graph`；rejection projection 必須與原 event producer 相同。
Verification 固定 `independent_verifier=false`。新 event/verification row 原子保存
producer/role/authority，verification 另存 Hub `received_at`；升級前 row 明列
`provenance_recorded=false`，legacy verification 的 `received_at=null`。
Verification 仍由執行工作的同一支 client agent 產生，尚未形成 executor 之外的 failure domain；
保存 provenance 只是為之後接獨立 verifier 建立邊界，不是雙觀測。

Audit Web、JSON 與 CLI 同樣只走 `operator.Service.ListAudit`。Action 可重複，其他 exact filters
包含 machine、outcome、principal、capability、source kind、correlation 與秒精度 RFC3339 時間窗；
cursor 綁住全部 filters、最後一筆 writer sequence 與第一頁 creation ceiling。`matched_total` 是
denial sampling 前的符合數，`total` 是採樣後整段 traversal 的可見總數；預設只納入全 traversal 最新 50 筆
`operator-denied`，`denials=all` 才不採樣。這仍是 live keyset，不是 snapshot；unsafe denial 本身
另受最新 1000 筆 bounded ring 保護，restore、VACUUM 或 ring deletion 後舊 cursor 不具 durable
generation 保證。Safe projection 不直接序列化 ledger row：畸形時間／結果改為 `null` 並附 `issues`，
無效 UTF-8、Unicode control/format 字元會替換且列入 `altered_fields`。

Changes Web、JSON 與 CLI 只走 `operator.Service.ListChanges`。名冊建立／retire／unretire 與 Hub
state history 是逐筆 durable transition；identity、credential、CLI、systemd 與 OpenClaw 則只比較
window 起點與終點的最新 Hub-received observation，不能被稱為完整事件流。Window 固定為 `(from,to]`，
預設 24 小時、上限 30 天；排序與成員資格只用 Hub-owned time，agent `measured_at` 獨立呈現。
Cursor 綁 exact filters、window/evaluation instant 與 registry/lifecycle/state/observation/retention ceilings；
續頁期間若 retention 刪除 observation row，server 回 `410 CHANGE_TRAVERSAL_GONE`，而不是位移資料。
Exact filters 下推至 Store；`total`、相容欄位 `matched_total` 與 kind counts 都描述篩選後完整 traversal，
兩個 total 必須相等。單次 read 最多檢查 250,000 筆 window observation、每個 selected source
250,000 筆 timestamp metadata、10,000 endpoint keys、10,000 transitions、32 MiB candidate metadata
與 32 MiB endpoint payload，超過即回 `422 CHANGE_READ_TOO_BROAD`；最多兩個
並行 reader，忙碌回 `429`，10 秒逾時回 `503`。Safe DTO 只含固定 typed fields；credential／CLI／
systemd subject 僅保留公開 allowlist，未知值固定成 `(redacted subject)` 且不可作 query filter。
Raw payload、hostname/account/path/PID/argv/free-form note/error/reason 不會離開 operator projection；
retention、舊 ledger registry/state transition tracking 起點及 malformed/unplaceable timestamp 都在
coverage 明列；被 kind/subject filter 排除、未實際讀取的 observation/registry/state source 則回
`not_applicable`，client 會核對該值與 request。State source 使用 append-only event ledger；升級時回填仍存在的 span，但不假裝重建
升級前已被同秒覆寫掉的值。

Artifacts 與 Standard Store 是完整的 native vertical slice。Apps 提供「概觀、OpenClaw、Store、
Profiles、Assignments、Artifacts、Fetch、Operations」八個可發現 submenu，並有 package/profile
safe lists、artifact/fetch-operation safe detail 與 preview/apply。Catalog list
只做 bounded/no-follow 的 sidecar、檔案類型與大小 inspection，所以 matching entry 明列為
`available_unverified`；detail 才在已開啟的 descriptor 上完整重算 SHA-256，相符後回 `ready`。
單筆 malformed sidecar、orphan tarball 或 size mismatch 各自保留 `invalid`／`unavailable` evidence，
不把整個 catalog 變成「零」，也不把 local path、upstream tarball URL 或 fetched-by identity 投影給
operator。

Fetch policy 支援 exact `openclaw@semver`、`node-runtime@major.minor.patch`、
`hermes-agent@major.minor.patch`、`claude-code@major.minor.patch`、`codex@major.minor.patch` 與
`antigravity@major.minor.patch`。OpenClaw 固定從
`https://registry.npmjs.org` 取得 exact tarball，流式驗 upstream SHA-512 並計算 Hub SHA-256。
Node runtime 固定從 `https://nodejs.org` 取得 `SHASUMS256.txt`，同時 pin Linux／Darwin `.tar.gz`
與 Windows `.zip`（x64／arm64）共六份 archive，逐一驗 SHA-256，再以 canonical path、mode、owner
與 timestamp 建成 deterministic gzip/tar bundle；Windows zip 的 `node.exe`／`node_modules/` 重排成
bundle 的 `bin/node.exe`／`lib/node_modules/`。
Hermes 固定從 Docker Hub 取得 exact `nousresearch/hermes-agent:v<version>`，pin upstream index
digest 與 Linux amd64／arm64 完整 OCI descriptor closure，再建成 deterministic OCI gzip/tar bundle。
Claude Code 固定從 `https://downloads.claude.ai/claude-code-releases` 讀取 exact
`manifest.json`，釘住 linux／darwin／windows amd64／arm64 六份 official binary SHA-256，
再建成 deterministic gzip/tar bundle。
Codex 固定從 `https://releases.openai.com/codex/releases/<version>/release.json` 讀取 exact release，
要求 `tag_name` 為 `rust-v<version>`，只下載 linux musl、darwin 與 windows msvc 的 amd64／arm64
六份 `codex-package-*.tar.gz`。每一份 digest 必須同時符合 release.json 與
`codex-package_SHA256SUMS`，再建成 deterministic gzip/tar bundle。GitHub fallback 與 `latest` channel
不在這條 intake。Antigravity 從
`https://antigravity-cli-auto-updater-974169037036.us-central1.run.app` 讀六份 latest-pointer manifest，
並從 `https://storage.googleapis.com` 下載官方檔。只有上游目前發佈的 `major.minor.patch` 可以取得；
版號不含 build id。bundle 是 `antigravity/<os>-<arch>/<official file>` 加上 `manifest.json`。
這六種 source 都停用 ambient proxy 與 redirect，並使用 private `0700` directory、`0600` temp、fsync 與
identity-collision-checked atomic publish。Preview/apply 綁同一份 exact source identity，source plan 只供 worker 使用，
不進 public operation DTO。

Artifact fetch intent、immutable idempotency receipt 與原始 structured audit 在同一 writer
transaction。Queue state 是 queued/running/succeeded/failed，phase 是
queued/downloading/verifying/publishing/complete；progress 只能單調前進。Hub 在 READY 前只做
local recovery：清理 stale temp，並替所有 running operation 換掉舊 authority；真正的 registry I/O
在 READY 後由單一 runtime worker 先 oldest-first reclaim running work，再持續 claim queue。每次
claim/reclaim 產生新的 run token，舊 worker 的 progress 或 terminal write 會被 fence；暫時性
terminal write 失敗留下的 running row 也會由下一輪安全 reclaim。正常 CLI 的 `artifact list/show/fetch`
與 `artifact fetch list/show` 都走
deterministic discovery HTTP；只有明示 `--db` 才進 stopped-service fence。HTTP enqueue 在送出前
建立 fsync、`0700`/`0600`、no-clobber private recovery receipt，綁 exact request/key/reason/digest/
authority；ambiguous response 只可用 `--recovery-file` 原樣 replay，完整結果寫出後才 inode-checked
清除。Web/JSON safe operation 不回 worker URL/token、idempotency key、request digest 或 private reason。

Updates 不再由 HTML 自己拼 Store rows；`operator.Service.Updates` 與
`GET /v1/operator/updates` 在同一 caller-supplied evaluation instant 組合 artifact page、canary/stable
成員、最後觀測版本與 Hub 收件時間、latest safe deployment、rollout plan 和 stable promotion gate。這是 read/preview
composition，不是第二個 deployment writer；artifact 在 overview 仍是 metadata-only，真正 create/
promotion 繼續走 Deployments preview/apply，且會重新驗 material bytes。

Deployments list/detail 同樣回 explicit safe DTO；Web template model 不接 raw desired spec、artifact
URL/provenance 或 `created_by`。List cursor 綁 exact channel/state/stuck filters 與最後一筆 keyset
位置，但仍是 live traversal：既有 deployment 的 state、counts、stuck 與 action eligibility 可以在
翻頁間改變。Create/Continue/Retry/Abandon 都分成 preview 與 apply；apply 只接受 canonical
lowercase `sha256:` preview digest，既有 deployment 另綁 `control_revision` 與至少為 1 的
`opened_batch`，且已耗盡的 `control_revision` 一律 fail closed、不做整數 rollover。Create/Retry fresh 回 `201`，Continue/Abandon
fresh 回 `200`；所有成功 replay 都回 `200`、`Idempotency-Replayed: true` 且 body 亦為
`replayed=true`。Mutation、desired state/jobs、Hub event、receipt 與 audit 同 transaction；相同
key/body 先讀持久判決，不因今天 artifact 已移除或 target 狀態改變而重新執行。Create apply
要求六個 planning fields 全部明列；batch/timeout 的顯式 `0` 也在 service/ledger 前拒絕。
Initial/delayed batch 都將 OpenClaw spec artifact 綁到 job
template digest。成功 replay 還要能由 canonical success receipt、request-digest-bound 原 typed
confirmations、完整 durable
deployment/job/target graph 與唯一原始 success audit 反證；Retry 連整條 ancestor lineage 都重驗。
Graph 同時約束 deployment 與 desired-state identity、target machine 仍在 registry、job state 為已知值，
以及 terminal state 與 `terminal_at` 證據相符；同資源任何殘留的非終態 deployment job 也不得被
finished/corrupt ledger 藏過新的 owner admission。會再開 job 的 delayed batch／Continue、Retry、
promotion 與 cached-success evidence 另逐張核對 linked job artifact digest 與 OpenClaw spec；
fresh finish-only Continue／Abandon 不依賴已下架的 material。Promotion 也重新驗每一代 attempt 的 stored
batch plan，不能把超過 blast-radius 上限的 legacy canary 當作合格證據。
拒絕 replay 則只信 canonical rejection code/detail 與唯一原始 failed audit，不讀今天的 fleet state；
兩者都不能只因 cached JSON 能 decode 就相信。

Stable promotion 的 public contract 由 deployment preview `schema_version=4`、deployment action
`schema_version=4` 與 updates `schema_version=5` 共用。`promotion.independent_required` 固定明列；
`independent_passed_targets` 必須等於 `independent_targets` 裡的 passed 數。每個 target 都帶 exact
machine、`job_id` 與十一種 state 之一與對應的 typed `next_step`。`job_id` 只有 `canary_not_succeeded` 可以是空的，其餘 state 必須是 exact job；重複的 job ID 只在非空的 ID 之間檢查。strict client 會重新計數並拒收 unknown
state、錯誤 next step、重複 machine、重複的非空 job、nil slice，或宣稱 allowed 卻有未通過 target 的 body。
`release_mismatch` 要求修復後重跑 canary；`release_unreported` 要求先升級舊 verifier，再重新派工。
Gate 只在 stable promotion 生效：工作單完成仍只採 executor evidence。

`clawctl-hub deployment create|continue|retry|abandon` 的 HTTP mode 會在 apply 前原子建立
private canonical recovery receipt；預設位置是
`${XDG_STATE_HOME:-$HOME/.local/state}/clawctl/deployment-recovery/`，目錄收斂為 `0700`、檔案
為 `0600`。Receipt 綁定完整 request、idempotency key、private reason、artifact 的明確
省略與 exact HTTP/DB transport；ambiguous response 後以同 action 加
`--recovery-file <absolute-path>` 跳過 preview 並重放原請求。CLI 只在完整結果寫出後才
以 inode-checked unlink 清除 receipt；apply/output 不確定時保留。Direct DB 不自動建檔，
但可明示 `--recovery-file`。

Machine channel、machine lifecycle、enroll-token create、pending-ticket revoke、Artifacts read/fetch、Updates read、Deployments read/control、Tailnet Settings、Retention Maintenance 與 Restore-drill Maintenance
是目前已收斂的 canonical operator vertical slices，也不代表
整個 operator plane 已完成。
Machine channel 的 optimistic revision 已能防 stale form；LocalAPI app-cap authorization 與
stdlib CSRF 已由整個 operator mux 共用。通用 `preview_digest` 在新 lifecycle 寫入已落地；尚未遷移的 mutation 與完整 `correlation_id` 仍是明列缺口。CLI 的 HTTP mode 要求操作者
明示 `--confirm-name`；新 request 預設產生 key 並使用 GET revision，ambiguous response 後則可把
原本的 `--idempotency-key` 與 `--expected-revision` 成對重用，保留完全相同的 canonical body。
Client 只接受 literal Tailscale IP 的 canonical `http://IP:port`，不讀 ambient proxy、不 follow
redirect，且 success body 的 machine ID、requested channel、typed display name、revision 與
`ETag` 必須彼此一致。Discovery 不讀 machine bearer 所在的 `agent.json`，也不把 server-side
`hub.env`／`CLAWCTL_PUBLIC_URL` 當 client authority。
`415`、畸形、未知欄位、尾隨內容與
過大的 JSON 等 transport rejection 會留下失敗 audit，但不占用 idempotency ledger、也不
冒充已取得 canonical digest；修正 request 後可以沿用同一把 key。

Machine lifecycle 的 `active` / `retired` 是名冊狀態，不是 credential rotation。原生
read/preview 會同時投影 lifecycle 分母、channel revision、保留的 agent credential、pending
enrollment tickets 與非終態 job；preview 另回這台機器當下開啟的終端工作階段數。
這個數字是可由被退役者改變的 preview-time 事實，不進 impact、preview digest、apply response 或
immutable receipt；preview digest 仍綁定其餘影響與獨立 `lifecycle_revision`。退役會讓 machine
bearer 驗證與未過期 pending ticket 兌換失效，並結束套用時仍開啟的終端工作階段，
但不刪 credential、ticket、registry row、channel 或歷史；因此恢復為 `active` 時，
保留的 bearer 可能立即重新通過驗證，未過期票券也可能重新可兌換。兩個方向因此都要
reason 與 exact display-name confirmation。Active → retired 若還有非終態 job 必須拒絕，
避免在 agent 立即失去驗證後留下無人可收的工作單。真實 transition 會將
projection、revision+1、append-only lifecycle event、immutable idempotency receipt 與原始 structured
audit 在同一個 writer transaction commit；no-op 也會留 receipt/audit，但不增 revision、
不建 transition event。相同 key/body 只回放原判決，不以今日狀態重新解釋。

Enroll create 的 preview digest 綁住 exact display name、整數秒 TTL 與目前 policy；Web 固定預覽
2 小時，CLI 預設預覽 24 小時。Machine registry row、token hash、redacted idempotency receipt 與
structured audit 在同一個 transaction commit；DB、audit 與 response cache 都沒有 token 明文。
Fresh response 若在網路上變成 ambiguous，資訊上不可能同時做到「Hub 不保存／不重顯明文」與
「client 無損取回」。因此相同 key/body 的 retry 不另開票，只回原 machine receipt，client 必須
先 revoke 原票、retire 原本未報到的名冊列，再以新 key preview/create 新的名冊列與票；目前沒有原
machine reissue。CLI 對這種 redacted
replay 保持 stdout 空白並以 non-zero 結束，不能把 receipt 誤當成可用 token。

Pending enrollment ticket 的撤銷與 active agent credential 是兩種不同 capability。撤銷只讓尚未
兌換的 ticket 失效；不刪 registry row、不改分母，也不輪替或撤銷已啟用 agent bearer。Preview
以 exclusive expiry boundary 說明 ticket 是否已過期；apply 與 enroll redeem 競爭時由同一個
SQLite writer transaction 決勝，不能同時成功。撤銷 replay 只回不含 secret/hash 的完整 receipt，
因此 CLI 將它視為成功，而不是重新執行刪除。

Live Hub 在 `Store.Open` 前取得 process-lifetime writer lock，並在既有 DB 的任何 writable open 前
驗完 17 張 Phase-1 基線表、精確欄位 metadata、PK/UNIQUE/FK 與 `schema_meta=1`；因此所有同 process 的
HTTP handler 都受同一把 fence 保護，包括尚未改走 canonical JSON API、仍直接呼叫 store 的
Web forms。這解的是第二個 Hub／direct process 同開 ledger，不等於 Web 已有 API parity。
Machines/Jobs/Artifacts/Deployments read/control、machine-channel 與 enroll-token 的明示 `--db` 都會先驗 canonical existing ledger，依序取得 lifecycle（upgrade）
與 writer lock，拒絕 active/stale maintenance marker，再證明受控 systemd Hub 對同一 binary／
DB 完全停止並重驗同一 schema identity；Machines read 另把 shell 載入的 expectations fingerprint
與 DB 中 Hub 最後發布的 active workload policy 對齊，不一致就拒絕產生判決。不符合就會 fail closed。
`job create --kind noop` 已使用 canonical diagnostic preview/apply service，正常模式走 HTTP；
明示 `--db` 時使用同一 service 與 stopped-service fence。其他 standalone direct CLI 尚未全部
接上這組 locks。

### Read / telemetry

| Method | Route | 用途 |
|---|---|---|
| `GET` | `/healthz` | Hub process readiness |
| `GET` | `/metrics` | Prometheus evidence；只接受啟動時釘住的 literal Tailscale `IP:port` Host；名冊分母是 lifecycle-only `clawctl_machines_total`，不另曝露 legacy `clawctl_machines_expected` |

HTML、CSV 與 `GET /downloads/agent/{arch}` 是 browser/BFF surface；bundle download 要求
`view` capability。`amd64`／`arm64`（及 `linux-amd64`／`linux-arm64`）是 Linux 六檔 ELF 包，
既有 URL 與 `clawctl-agent-bootstrap-linux-*.tar.gz` 檔名不變；`darwin-amd64`／`darwin-arm64`
是 Darwin 四檔 Mach-O 包，下載檔名為 `clawctl-agent-bootstrap-darwin-*.tar.gz`。每次下載重開
no-follow descriptor 並重驗版本、可執行檔架構、archive layout（Linux：installer 與 systemd
units；Darwin：`install-agent-macos.sh` 與 LaunchAgent plist）、大小與 SHA-256。這些 routes
不冒充穩定 JSON API。
`POST /preferences/navigation-language` 也是 `view` HTML route，而且是唯一歸為 `view` 的 unsafe
route：form 恰好一個 `locale`（`en`／`zh-Hant`）與一個 `return_to`，URL 不帶 query；cross-site
POST 在 handler 之前以 `403 CROSS_ORIGIN_REQUEST` 拒絕並留下 denial 稽核。成功時
以 HttpOnly、SameSite=Lax、Path=/ 的 browser cookie 保存管理中心導覽偏好，再以 `303` 返回同站且
長度受限的 `return_to`。它不寫 Store、audit 或任何 fleet state；英文目前只套用 product bar、
workload/resource navigation、breadcrumb 與 document title，證據內容仍以明確的 `lang="zh-Hant"`
邊界呈現，不能把尚未翻譯的操作判斷標成英文。
Machine/public 與 operator 的兩份 route manifest 在啟動時雙向核對且不得重疊；
machine `/v1/` plane discriminator 必須是 literal，避免 wildcard 在 root mux 蓋過
`/v1/operator/*`。

## 3. Operator parity ledger

`✅` 是現在可用；`△` 是只有 read/preview/detail，或仍繞過 HTTP operator API；
`—` 就是尚未交付，不能把它解讀成刻意不做。

| 能力 | UI | Operator JSON API | CLI | 下一個收斂動作 |
|---|---:|---:|---:|---|
| Machines list / detail | ✅ list／△ detail evidence | ✅ list／△ detail evidence | ✅ list + show + evidence | List v2、detail v5 與 machine evidence v4 已有 strict typed contracts。Detail v5 收斂 Hub judgement、bounded findings、expectation display definitions、artifact/event evidence、monitor、identity/resources、24h check-ins、state history 與 durable identity hints；retired diagnosis 與 fleet state 以 `affects_fleet_state` 分離。Expectation/artifact/event 文字 display-only，不做 path/secret 內容掃描；artifact freshness 不讀內容、不冒充 outcome，event 只使用 operator-declared enum 與 agent 結構化 counts，不掃關鍵字。Evidence v4 負責 OpenClaw／CLI、credentials／occupancy、systemd、run-summary/journal。Pending HTML 與 BAT connect 已分別改讀 dedicated typed operator service／coordinate-bearing BFF v1，Machine detail HTML 不再有 raw Store bridge；獨立 verifier 仍不存在；`7082e5d`、pending bridge `faa0659` 與 BAT bridge `c82de07`／`713f276` 已 live 驗收 |
| 機器 channel 讀／改 | ✅ | ✅ | ✅ | CLI 預設以 deterministic discovery GET+PUT；typed confirmation／retry inputs 完整，`--db` 只保留 exact stopped-service break-glass；`bf39c04` 已 live 驗收 |
| 機器指派使用者讀／改 | ✅ | ✅ | ✅ | Tailnet 名冊選人、預覽、名稱確認、版本檢查與請求重放；`none` 取消指派；來源不可用與空名冊分別顯示 |
| enroll token 建立 | ✅ | ✅ | ✅ | canonical preview/create、一次性 secret delivery、idempotency/recovery 與 stopped-service `--db` 已完成；`88fb5ff` 已 live 驗收 |
| pending enrollment ticket 查詢／撤銷 | ✅ | ✅ | ✅ | canonical get + preview + idempotent revoke；固定顯示 registry 保留、分母 0、active credential 不受影響 |
| 註冊上限 | ✅ | ✅ | ✅ | 上限算的台數與註冊報告的分母共用同一條判準；「沒有設上限」與「上限 0」是兩件事；改上限走 preview/`expected_revision`/idempotency，取消是 upsert 不是刪列；擋人那一次數在開票的同一筆 writer transaction 裡 |
| 軟體清查 | ✅ | ✅ | ✅ | 只讀 Hub 已經持有的 `cli_tool` 觀測，不新增任何要 agent 自己說「我做到了」的欄位；「沒有觀測」與「沒有安裝」分成兩種狀態；總覽那張表已改讀同一份 operator projection，`internal/web/toolmatrix.go` 已移除；`schema_version=2` 起每一格再帶「那個版號講的是不是正在跑的那一份」，2026-09-12 實測 17 格裝著的沒有一格量的是正在跑的那一份 |
| 每機安裝狀態（指派意圖 vs 觀測）| ✅ | ✅ | ✅ | 比的是 Hub 自己知道的兩件事：最後一筆安裝意圖，跟最新一筆工具觀測。意圖的解析與 agent 收到的那一筆同規則（machine ∪ channel 取最大 revision，計數器 key 是資源不是 scope），所以頁面不會跟機器實際收到的不一致；noop 診斷單不算安裝意圖。對不起來拆成五種各有下一步的答案，不併成一個「未知」；「指派的比看到的舊」不講成失敗——指派以外的安裝路徑這個 Hub 看不到，那句限制隨報告送到每一個平面 |
| 發佈的 vs 指派的（profile 發佈 vs 指派） | ✅ | ✅ | ✅ | Web、CSV 匯出、報告落地頁與子選單那一列、JSON API、`report profile` 子命令與 store 讀取器（`FleetProfileAssignments`、`FleetIntentVersions`）都已交付。一列是「一份已發佈的 revision × 它點名的一個套件版本」，不是一台機器一列——後者會讓一份一台都沒指派的 profile 在匯出檔裡一列都沒有，而那正是這份報告存在的理由。用戶端自己把每一格重數一次，連「這一列該是哪一種狀態」都自己算——那四種狀態只差一句話。缺的只剩部署 |
| active agent credential rotate／revoke | — | — | — | 尚無 native capability；先定義 reconnect / recovery threat model，不得沿用 pending-ticket 名稱 |
| retire / unretire | ✅ Lifecycle submenu＋preview/confirm | ✅ GET/preview/PUT | ✅ `machine lifecycle` HTTP／break-glass | Retire preview 明列當下開啟的終端工作階段數與套用時仍開啟者會結束；count 不綁 digest/receipt。獨立 revision、typed confirm/reason、active-job blocker、atomic event/receipt/audit 已實作並經合約測試；`c91695b` 已 live 驗收（retired→retired no-op 與 CLI/JSON replay） |
| Connect BAT 導向 | ✅ | — | — | 維持 `MachineConnect` v1 typed BFF action，不設 coordinate JSON route；Hub 不做代理。Web 只交四種 typed outcome、同一份 measured projection、reason 與 verified Actor 給 injected operator service；service 固定 connect audit 的 action／subject／safe detail，拒絕 impossible shape。Audit 是 best-effort button evidence，不阻擋已量出的 redirect，也不涵蓋人手複製 URL |
| artifact list / detail | ✅ | ✅ | ✅ HTTP | List 明列 metadata-only `available_unverified`；detail 才完整 hash 成 `ready`，malformed item 隔離且 safe DTO 不曝 URL/path/provenance identity |
| artifact fetch / operation status | ✅ | ✅ | ✅ HTTP | Fixed npm/nodejs.org/Docker Hub origins、OpenClaw、multi-platform Node runtime 與 Hermes OCI intake、durable async queue/progress、startup recovery、run-token fencing、idempotency/audit 與 private CLI recovery receipt |
| updates overview / channel preview | ✅ | ✅ | —（strict Go client） | Native read model 共用 artifact page、channel members、latest deployment、rollout 與 promotion gate；mutation 仍走 Deployments |
| deployment preview | ✅ | ✅ | ✅ HTTP | 共用 verified artifact、target snapshot、promotion policy 與 digest |
| deployment create | ✅ | ✅ | ✅ HTTP | UI/API/CLI review/typed confirm；stable promotion 逐 canary target 要求被指派的 active fleet peer 完整通過三條規則，且結構化 release version 等於 canary exact version；不可繞 promote lock |
| deployment continue | ✅ | ✅ | ✅ HTTP | control revision + opened-batch CAS、idempotency、atomic audit；stable promotion 沿用同一份獨立證據 gate |
| deployment retry | ✅ | ✅ | ✅ HTTP | 只預選 exact terminal-failure targets；新 attempt 有 lineage |
| deployment abandon | ✅ | ✅ | ✅ HTTP | full-ID confirmation + terminal-job gate + unopened impact |
| deployment list / detail | ✅ | ✅ | ✅ HTTP | safe DTO 供 CLI/UI 共用；detail 是 `schema_version=3`，每個已開單 target 帶八種獨立 verdict 之一，另有由 targets 推導的 rollup；不曝 raw spec/created_by |
| noop protocol drill | ✅ Diagnostics submenu＋preview/confirm | ✅ preview/apply | ✅ HTTP／break-glass | 固定 noop spec、不改機器設定；atomic desired/job/receipt/audit，建立後進入 job detail |
| global job list / detail / evidence | ✅ | ✅ safe projection＋typed evidence | ✅ `job list/show/evidence` HTTP | creation-ceiling pagination 仍是 live keyset；raw evidence 已改為 typed bounded disclosure（`schema_version=6`，agent/Hub event producer、role/authority、row provenance、兩個 clocks 與 independence 明標，另有八種 verdict、expected／observed version 的獨立證據段與派工段）；sampleagent3 的 fleet-peer verifier 已用獨立 bearer 與 failure domain 寫入正式帳本 |
| audit ledger | ✅ | ✅ safe projection | ✅ `audit [list]` HTTP | 完整 exact filters、creation-ceiling cursor 與 bounded denial sampling 已交付 |
| tailnet inventory reconcile | ✅ Tailnet Settings＋Enrollment 摘要 | ✅ `GET /v1/operator/tailnet` | ✅ HTTP／break-glass | Stable-ID reconcile、source observation time 與 unavailable typed state |
| ignore / unignore peer | ✅ preview/confirm | ✅ preview/PUT | ✅ `tailnet ignore|unignore` HTTP／break-glass | Reason、expiry、revision、typed hostname confirm、idempotency 與 atomic audit |
| 裝置設定原則（發佈／指派／套用狀態）| ✅ Devices > Configuration＋兩個確認頁 | ✅ board/preview/publish/preview/assign | ✅ `settings list/publish/assign` HTTP | Hub 每次 check-in 下發 effective settings 與其 digest，agent 回送它**實際在跑**的 digest，盤面只用這兩個 digest 的比較判定套用與否；不數心跳間隔、不讀 agent log。同值重發不產生 revision，`expected_revision` 不符明確 `409`。目前設定集合只有 check-in 與 observation 兩個間隔 |
| 裝置合規性（規則發佈／指派／判決／不合規動作）| ✅ Devices > 合規性＋兩個確認頁 | ✅ board/preview/publish/preview/assign | ✅ `compliance list/publish/assign` HTTP | 規則只建立在 Hub 量得到的事實上：Hub 收到 check-in 的時刻、agent 回報的版本與磁碟、它回報的工作單開關，以及設定平面自己的套用判決。沒有任何一條規則讀機器自己對合規的主張。判決在讀的時候現算不落表，因為同一批證據在一小時後可以合法地變成另一個判決；每一台都連同推出判決的每一條規則結果一起回，操作員看得到「不符合」是哪一條量出來的。量不到的機器永遠不會是符合，沒指派原則的機器是「未指派」而不是符合。原則可以帶一個不合規動作：連續不符合超過寬限期就停發工作單，那台同時被排除在新部署之外，排除理由印「合規性停發工作單」。連續多久由證據決定——Hub 重判寬限期那段窗裡的每一次 check-in，中間有一次過就重新起算。動作不落表，機器一恢復報到就自己領得回工作單；從未報到與量不到都不觸發動作 |
| prune | ✅ Maintenance preview/confirm | ✅ status/preview/apply | ✅ HTTP／break-glass | 已共用秒級 policy、evaluation time、revision 與四表 exact counts；typed destructive confirmation、protected-newest evidence、idempotency 與 atomic delete/ledger/receipt/audit 已交付 |
| restore drill | ✅ Maintenance preview/confirm/operation detail | ✅ preview/enqueue/list/detail | ✅ HTTP／break-glass | 固定 standalone backup 身分與 SHA-256；durable async worker/startup fence；私有副本執行 production schema migration 與 SQLite quick check；成功才寫完成章，永不替換 live DB |
| Changes read | ✅ | ✅ safe projection | ✅ `report changes` HTTP | transition 與 endpoint delta 語意分離；Hub clock window、bounded paging、retention/malformed coverage 與 raw-evidence redaction 已交付；bare `report` 保留 legacy daily preview |
| Ticket usage | ✅ filter/table/CSV | ✅ safe projection | ✅ `tickets` HTTP text/JSON/CSV | 固定 receive-time snapshot、provider drill-down、成本與字串界線、coverage/disclosure 已交付；CSV 中可執行的前導文字會中和 |
| 獨立 verifier 註冊／撤銷 | —（本切片不加導覽入口） | ✅ preview/apply/list/detail/revoke-preview/revoke | ✅ `verifier list/show/register/revoke` HTTP／break-glass | 六條 canonical route 已走共同寫入契約（exact capability、純 preview、`preview_digest`、typed confirmation、revision CAS、`Idempotency-Key`、state＋receipt＋audit 同 transaction、一次性 secret）|
| 派工（指名 verifier 驗某一張工作單）| ✅ `/jobs/{id}` 面板＋確認頁 | ✅ assignment-preview/assignments | ✅ `verifier assign` HTTP／break-glass | `operate`；同 failure domain 在 preview 就以 `409 VERIFICATION_ASSIGNMENT_DOMAIN_CONFLICT` 擋下，不必等證據寫入被 403。「做完」定義為證據存在（同 job＋verifier 且 `received_at >= assigned_at`），沒有 claim／lease／state 欄位。verifier 平面只拿得到指派給它的單，因此被偷走的 verifier bearer 看不到整個機隊的工作單 |
| 跨故障域證據與 stable promotion gate | ✅ 工作單證據段＋部署／更新 review | ✅ job evidence v6＋deployment preview v4＋updates v5 | ✅ `job evidence` 與 deployment review | 工作單終態仍只採 executor 證據，避免「終態後才派工」形成死鎖；stable promotion 則要求每個 effective successful canary job 都有 active `fleet_peer_agent` 的完整三規則 report，且 `current_release` 的結構化版號與 canary exact version 相同。十一種 gate state 與 exact 下一步由 Hub 計算，Web／JSON／CLI 同步呈現；Hub 不解析 stdout 猜版號 |

## 4. API 或 CLI 的決策

正常 runtime 一律優先結構化 API。官方 CLI 只有三個角色：contract smoke、診斷、
break-glass。CLI 如果只是同一個 REST API 的 wrapper，它通過只證明另一個 client
能遵守 contract，**不算第二份部署成功證據**。

對 app update，證據鏈固定是：

1. operator API 接受 request、固定 targets 與 digest；
2. 唯一 executor 回報 receipt / stage / activate / exit；
3. executor 之外的 actual-state verifier 獨立讀 installed version、service、process 與 probe
   （Controller 執行時由 endpoint；agent 執行時由 Controller）；
4. Hub 依 operation / attempt / target 關聯兩份證據後才下 verdict；
5. 觀察窗仍一致才標 stable。

第 3、4 步現在都已接上。`verifiers` 三個 kind 對應三種故障域，寫入時以
`verifiers.failure_domain <> jobs.machine_id` 做結構性隔離，所以同一台機器不可能同時是
執行者與驗證者。sampleagent3 的 `fleet_peer_agent` 已由獨立路徑產生三規則 report；stable promotion
逐 canary target 要求完整 report 與 exact release version，缺版號或錯版都 fail closed。
`Store.CompleteJob` 的條件仍是「一列可信 executor 證據、零失敗」，因為 verifier 派工在工作單
終態後才交付；把獨立 gate 放進 completion 會形成死鎖。

AWX/AAP 接入時，runtime 用官方 REST API；inventory、template、credential 與 workflow
設定用官方 Ansible collection / configuration-as-code。`awx` CLI 是由同一 API
產生的 client，所以保留做 contract smoke，不拿它冒充獨立 observer。

## 5. 每新增一個 operator operation 的 Definition of Done

- stable JSON contract 與 domain service 共用同一套 validation；
- UI 有可發現入口、impact preview、權限與必要確認；在故障域尚未挑定前不加導覽入口，
  只由已存在的頁面（工作單證據頁）呈現既有事實；
- CLI 正常模式真的呼叫 HTTP API，另以明示選項保留 break-glass；
- success、domain rejection、replay、conflict 都能由 idempotency / digest / correlation 查回；
- audit 有 structured source kind、stable Tailscale user/node ID、exact authorized
  capability、auth method/decision，以及分開保存的 transport provenance；
- API、UI、CLI parity tests；
- **同一個業務操作在每個平面要求同一個 capability。** route manifest 目前只記「這條 route 要什麼權限」，不記「這條 route 是哪一個業務操作」，所以跨平面的權限一致性沒辦法整份機械檢查；每個工作流程要自己用一條掃 manifest 的規則釘住（例：`TestSettingWritesCostAdminOnEveryPlane`、`TestComplianceWritesCostAdminOnEveryPlane`）。抄程式碼寫成的 per-route 測試擋不住這件事——`5afb64b` 的 JSON 開成 operate、`729db66` 的 Web 開成 admin，兩邊的 per-route 測試都過；
- executor evidence 與另一 failure domain 的 verifier evidence 分開標 role/來源，不把兩個 client 當兩雙眼睛；
- unknown、資料來源壞掉與「真的為零」在 API 和畫面上是不同狀態。
