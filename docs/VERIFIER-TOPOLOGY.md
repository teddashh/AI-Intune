# Independent verifier：producer topology 與 failure domains

> 這份文件從私人工作筆記（不在這個公開倉庫）的 P0 起點一路記到 2026-09-13：先量出 producer
> topology，再把 sampleagent3 fleet-peer runner 上線，最後讓完整的跨故障域 report 進 stable
> promotion gate。工作單終態仍只採 executor 證據；這兩個 gate 不可混為一談。

量測時間 2026-09-11，量測當下的 live Hub 是 `26dc534`（同日稍晚換版到 `d1997d3`，拓樸事實不變），ledger `~/.local/share/clawctl/clawctl.sqlite`。

## 1. 「獨立」要分開的是什麼

一個 verifier 之所以有價值，不是因為它多跑了一次指令，是因為**它壞掉的方式跟
executor 不一樣**。所以「獨立」不是形容詞，是一張要逐項回答的表：

| 要分開的東西 | 不分開會漏掉什麼 |
|---|---|
| Process | executor 的 bug／panic／OOM kill 會同時帶走驗證這一步 |
| Host | 主機被改、磁碟滿、時鐘歪、cgroup 限制，兩邊一起受影響 |
| Credential | 一把 token 被偷，攻擊者同時能「做」跟「說做完了」 |
| Clock | 同一個歪掉的時鐘蓋在 executor 與 verifier 兩份證據上 |
| Network path | 同一條斷掉的路徑讓兩邊一起沉默，而沉默看起來像沒事 |
| Codebase | 同一個「什麼叫裝好了」的錯誤認知，兩邊會一致地答錯 |
| Code path | 同一支程式裡跑兩次，共用同一份記憶體狀態與同一個錯誤前提 |

一個只分開 process、其餘全共用的 verifier，只能抓到「executor 當掉」，抓不到
「executor 對於成功的定義本身就是錯的」—— 而後者正是這個產品存在的理由
（`docs/PRODUCT.md` 地基二）。

## 2. 起點的 topology：只有一個 domain

### 2.1 證據怎麼產生

驗證指令由 executor 自己在同一個 process 裡跑完再回報：

* 規則與指令寫在 `cmd/clawctl-agent/openclaw_executor.go:911` 起（`unit_execstart`、
  `health`、`process_cmdline`、`version`、`rollback`、`stage`）。
* `execDeps.verification`（`cmd/clawctl-agent/openclaw_executor.go:1136`）把結果包成
  `model.JobVerificationRequest`，`VerifiedAt` 取的是 `d.now()` —— **agent 自己的時鐘**。
* 回報走 `POST /v1/jobs/{id}/verifications`（`cmd/clawctl-hub/api.go:59`），
  boundary 分類是 `nonOperatorAgent`（`cmd/clawctl-hub/operator_boundary.go:67`）。

### 2.2 Ledger 怎麼記

`Store.RecordVerification`（`internal/store/deploy.go:982`）的 INSERT 把三個 provenance
欄位**寫死**，而且整列的寫入條件綁在 job lease 上：

```sql
SELECT ?, job_id, machine_id, ?, ?, ?, ?, ?, ?, ?, ?, machine_id, ?, ?, 1, ?
  FROM jobs
 WHERE job_id = ? AND machine_id = ? AND lease_token = ?
   AND state IN (claimed, running, verifying)
```

* `producer_kind = 'executor_agent'`、`evidence_role = 'executor'`、
  `authority = 'machine_bearer_lease'`（`internal/store/deploy.go:58-60`）。
* `producer_id` 直接取 `jobs.machine_id`，不是呼叫端給的。
* 寫入前提是**持有那張單的 lease**。

最後一點是關鍵：今天的資料模型不是「剛好只有 executor 在寫」，是
**結構上只有持有 lease 的 executor 寫得進去**。一個獨立 verifier 沒有 lease，
也不應該有 lease，所以它現在一列都寫不了。這不是疏漏，是還沒開的門。

### 2.3 Completion gate 怎麼用它

`internal/store/deploy.go:1060` 起的 gate 要求：至少一列 `trustedExecutor`
（`producer_kind`／`evidence_role`／`authority` 三者相符且 `producer_id = jobs.machine_id`）
且 `failed = 0`。也就是說，**今天的 gate 完全建立在 executor 的自述上**。

### 2.4 Live ledger 的實況

2026-09-12 07:38Z 量的：

```
verification rows by provenance:
  kind=executor_agent   role=executor            authority=machine_bearer_lease prov=0 rows=32 failed=3
  kind=executor_agent   role=executor            authority=machine_bearer_lease prov=1 rows=5  failed=0
  kind=fleet_peer_agent role=independent_verifier authority=verifier_bearer      prov=1 rows=18 failed=0
executor producer_id == target machine_id: 37 / 37
independent rows: 18，涵蓋 6 張工作單，來自 1 個 verifier
independent rules: openclaw.current_release, openclaw.gateway_http, openclaw.unit_state
independent observed_digest 有值的列: 0
independent verified_at -> received_at delta: min 0s, max 6s
verification_assignments: 6
```

executor 那 37 列仍全部是自述，`producer_id` 100% 等於被測機器自己——這不會改變，
那正是它要回答的問題。改變的是多了一層：18 列由 sampleagent3 上的 runner 寫入，
`producer_id` 是 verifier 而不是被測機器。

`prov=0` 的 32 列是 provenance 欄位加入之前的舊 binary 寫的；
`provenance_recorded=0` 把「我們知道是誰」跟「我們只是預設是誰」分開，不要把它們併成一欄。

（09:31Z 的壞機演練之後是 21 列、7 張單、1 張 `failed`；見 §4.3。）

獨立那 18 列的 `observed_digest` **全部是空的**，而且應該是空的：裝好的 OpenClaw 是一棵
展開的目錄，不是工作單那顆 artifact，對它取任何 hash 都不會等於工作單的 digest。
`EvaluateIndependentVerdict` 只把**有值而且不同**的 digest 算成 `digest_mismatch`，
所以空值不會誤判——但也表示這 6 張單的 `passed` 沒有比對過任何 digest。
畫面上那一句因此在 `5ebedf7` 改掉了（見 §4.3）。

這批舊列也沒有結構化 `observed_version`；當時版號只存在 stdout，Hub 不會解析。2026-09-13
唯讀比對正式帳本中 8 筆 `current_release` 列時，相關既有樣本剛好與各自工作單版號相同，但那是
缺口量測，不是產品判準。Verifier write schema v2 起 `current_release` 通過時必須另外回版號；migration
後的舊列保持空值，讀模型明列 `release_unreported`，stable gate 不會拿 stdout 補值放行。

獨立列的時間差是 0 到 6 秒，跟 §2.5 那個 -81 秒無關：時鐘快的是 sampleagent4，
而 sampleagent4 在這裡是**被測的那一台**，不是 producer。producer 是 sampleagent3，它的時鐘跟 Hub 對得上。
這正是「每個 producer 帶自己的 clock、排序一律用 Hub 的 received_at」要處理的形狀。

### 2.5 那個負的時間差要單獨講

`verified_at → received_at` 的最小值是 **-81 秒**：agent 說的時間比 Hub 收到的時間還晚。
這不是資料壞掉，是 sampleagent4 的時鐘快約 80 秒且沒有同步（已知問題）。它的意思是：

**agent 自報的時間不能拿來排序，也不能拿來判新鮮度。**

今天只有一個 producer，所以這件事只影響顯示。一旦有第二個 producer，
「executor 說 12:00 裝好、verifier 說 11:59 看到舊版」就會變成一個**無法解讀**的矛盾 ——
除非兩份證據各自帶自己的 producer clock，而排序與 freshness 一律用 Hub 的 `received_at`。
契約必須在第一天就這樣寫，不能等撞到再補。

## 3. 今天共用了什麼

| 要分開的東西 | 今天 | 說明 |
|---|---|---|
| Process | ❌ 共用 | 同一個 `clawctl-agent` process 執行完直接驗證 |
| Host | ❌ 共用 | 驗證指令對 localhost 的 systemd／`/proc`／`127.0.0.1` |
| Credential | ❌ 共用 | 同一把 per-machine bearer，外加同一張 job lease |
| Clock | ❌ 共用 | `VerifiedAt = d.now()`，agent 自己的時鐘 |
| Network path | ❌ 共用 | 同一條 agent → Hub 的 Tailnet 連線 |
| Codebase | ❌ 共用 | 規則與判定都在同一支 binary 裡 |
| Code path | ❌ 共用 | 同一次執行流程內的下一個步驟 |

七項全部共用。所以現在的 `verification_results` 應該被讀成
**「executor 對自己工作的結構化自述」**，它有價值（比沒有好很多，而且 3 列 failed
確實擋下過東西），但它不是第二雙眼睛。DTO 裡的 `independent_verifier=false` 是
如實描述，不是保守設定。

## 4. 可選的 independent domain

以下四個都不需要先買或先裝任何東西就能評估。每一欄回答的是「這個選擇分開了嗎」。

| | A. 同機不同 unit | B. 另一台 fleet 機器 | C. Hub 內建 prober | D. AWX/AAP job |
|---|---|---|---|---|
| Process | ✅ | ✅ | ✅ | ✅ |
| Host | ❌ | ✅ | ✅ | ✅ |
| Credential | ✅ | ✅ | ✅ | ✅ |
| Clock | ❌ | ✅ | ✅ | ✅ |
| Network path | ❌ | ✅ | ✅（改走 Tailnet 進去） | ✅ |
| Codebase | ❌ | ❌ | ❌ | ✅ |
| Code path | ✅ | ✅ | ✅ | ✅ |
| 新增基礎設施 | 無 | 無 | 無 | 需要 AWX/AAP |
| 主要缺點 | 幾乎沒分開什麼 | 同一份「什麼叫裝好」的認知 | Hub 自己變成 producer，且 samplehub1 同時是 managed endpoint（§14.19） | 最大的建置成本 |

幾個不能忽略的細節：

* **B 的網路方向（2026-09-11 實測後修正）**：原本這裡寫「用 gateway `/health` 這種從外面
  看得見的座標」。量過之後那句話是錯的——**OpenClaw gateway 只聽 `127.0.0.1:18789`**
  （samplehub1 與 sampleagent4 都量過，IPv4 與 IPv6 loopback 各一條），Tailnet 上看不到它。
  機器在 Tailnet 上對外只有 port 22，而那條是 Tailscale SSH 在接。所以 B 的唯一可行路徑是
  **走 Tailscale SSH 進去看**，不是純網路探測。這也代表 B 的規則和 D（AWX/AAP 跑 Ansible
  over SSH）會長得幾乎一樣，只差在 codebase 沒分開——B 是 D 的便宜版，不是另一種東西。
* **B 的可達性（2026-09-11 實測）**：peer→peer 走 Tailscale SSH **是通的**。
  sampleagent3 → `example-user-b@100.64.200.5`（sampleagent4）與 sampleagent3 → `example-user@100.64.200.2`（samplehub1）
  都回得來，不需要改 ACL。⚠ 用錯 user 會得到 `Connection closed by <ip> port 22`，
  那看起來很像 ACL 拒絕，其實只是 user 錯了（見 `fleet-ssh-and-arch` 那條同樣的坑）。
  先確認 user 再下結論，不要因為一次 connection closed 就判定這個 topology 不可行。
* **B 看得到的座標（2026-09-11 從 sampleagent3 量 sampleagent4）**：`openclaw/current` symlink 解出來的
  release 路徑（＝實際裝上去的版本，由檔案系統回答，不是 agent 自述）、gateway 在
  `127.0.0.1:18789` 有沒有應答、以及 unit 的 `ActiveState`／`SubState`／`MainPID`。
  前兩個有意義；第三個最弱，`active/running` 依 §1 不是工作結果，只能當自己一條規則報，
  不能當成 pass。artifact digest 這台看不到——peer 看得到的是解開後的 release 目錄，
  不是 artifact tarball，所以 `observed_digest` 應該留空（契約允許，空值永遠不算 clash），
  不要湊一個算得出來但意義不同的 hash 上去。
* **C 的自證問題**：Hub 驗 samplehub1 的工作，等於自己驗自己所在的主機（§14.19）。
  samplehub1 的工作用 C 來驗不算獨立；驗其他三台可以。
* **D 分開了 codebase**，這是唯一能抓到「兩邊一致地答錯」的選項，也是唯一需要新基礎設施的。
* **sampleagent1 不在任何選項的 producer 清單裡**：它從沒報到過，而且刻意留在分母。

私人工作筆記（不在這個公開倉庫）已經定序：**先把 contract、fixture 與 scenario tests
做完，再決定要不要接 AWX/AAP**。那些測試已隨 `d1997d3` 完成。

### 4.1 已經選的（2026-09-11）

**B（另一台 fleet 機器，`fleet_peer_agent`），第一階段先只觀察、不進部署閘。**

先只觀察的理由寫在 §6：在實機量過「它會擋下什麼、又會誤擋什麼」之前，
把獨立證據接進閘只是把一個沒量過的判準放到升級路徑上。兩邊量測已在 §4.3 完成，
2026-09-13 起它進入 stable promotion；`Store.CompleteJob` 仍然不變。

Runner 要解決的第一個問題是**它怎麼知道要驗哪一張單**——verifier 平面原本只有
`POST /v1/verifications` 一條 route，讀不到工作單。答案見 §4.2；runner 本身見 §4.3，
`5ebedf7` 起它存在，而且已經在真機上寫過證據。

### 4.2 派工：怎麼知道要驗哪一張單（2026-09-12 已選）

原本寫在這裡的兩條路都不要選：

* **讓 runner 另外拿 operator `view` 去讀 job list。** 量過才知道這條比想像中更糟：
  `internal/operatorauth/operatorauth.go` 的 human-principal gate 會拒絕 tagged node，
  也會拒絕沒有 `UserProfile.LoginName` 的 node；而 `tailscale status --json` 顯示這個
  tailnet 的所有節點目前都沒有 tag、擁有者都是 `operator@example.com`。也就是說這條路今天能走，
  是因為 daemon 借用了人的身分——而且機隊一旦正確地打上 tag，它就當場壞掉。
* **在 verifier 平面加一條「列出我有資格驗的工作單」route。** job ID 是 16 bytes 的
  crypto/rand（`internal/store/store.go` 的 `newID()`），所以今天一個被偷走的 verifier
  bearer 幾乎是惰性的：它猜不到單號。加一條列舉 route，正好是把它從惰性變成危險的那一步；
  而 separation rule 允許的範圍是**整個機隊少一台**，那正是這條 route 會交出去的東西。

選的是第三條：**派工**。operator 指名「哪一個 verifier 要看哪一張單」，verifier 平面
只拿得到指名給它的那幾張。這樣被偷走的 bearer 能看到的，恰好只有 operator 已經
打算交給它的東西。

落在 `verification_assignments`（`assignment_id`／`job_id`／`verifier_id`／`assigned_at`／
`assigned_by`），以及三條 route：`POST /v1/operator/verifiers/{id}/assignment-preview`、
`POST /v1/operator/verifiers/{id}/assignments`（都是 exact `operate`）與
`GET /v1/verification-assignments`（verifier bearer）。四個設計決定值得寫下來：

1. **沒有 state 欄位。** 一張 fleet-peer 派工「做完了」的定義是同一組 `job_id`＋
   `verifier_id` 在該次 `assigned_at` 後收齊三個 exact rule ID；重複一條不算三條，兩個
   producer 各送一半也不能合併。其他不授予 gate 的 verifier kind 維持一列證據即可。
   沒有 claimed_at、lease 或 expiry，因此沒有一整套會跟事實漂移的生命週期。這跟
   `PRODUCT.md` 地基二「自證不算數」是同一條規則：沒有欄位可以被宣告成完成。
2. **發單要等工作單終態。** 在 executor 結束前產生的證據，依 §5 第 3 條的 freshness 判準
   本來就是 stale。所以 Hub 不發它，否則只是量產保證過期的列。派工本身可以早就下：
   「這張單落地之後驗它」是合法的，而且工作單頁看得到它在等什麼。
3. **派工不帶指令。** 規則編譯在 verifier 自己的程式裡。一個會執行 Hub 下發指令的 verifier
   不是第二個判斷，是一條用單一 bearer 控管的遠端執行管道。`commands_supplied_by_hub`
   因此是 policy 的一部分、進 preview digest，改了它就讓所有舊 preview 失效。
4. **Gate 能力跟 kind 綁定。** `grants_deployment_gate` 寫在 policy 裡：只有
   `fleet_peer_agent=true`，`external_job_runner`／`hub_prober=false`。它只授予 stable
   promotion，不授予工作單終態；`Store.CompleteJob` 一個字都沒改。

工作單頁的派工段有四種互斥狀態：`reported`、`producer_revoked`、`waiting_for_job`、
`awaiting_report`。它們存在的理由是讓 `absent` 這個 verdict 分得出「沒有人被指派」
與「有人被指派但還沒回報」——這兩件事在畫面上長得一樣，意思卻完全相反。

### 4.3 Runner：`clawctl-agent verifier`（`5ebedf7`，2026-09-12 上線於 sampleagent3）

第二雙眼睛終於有一支程式。它跟 agent 共用同一個 binary，但用的是另一組憑證、
問的是另一個問題：

```
clawctl-agent verifier --hub URL --token-file T --targets F [--dry-run]
```

`--token-file` 是一個 0600、只放 verifier bearer 的檔案。**沒有從 agent 設定繼承任何東西**：
`agent.json` 是多行 JSON，連 `readSecretFile` 的格式檢查都過不了，所以「不小心共用」
在結構上就不成立。`--targets` 是這個 verifier 自己的事——machine_id 對 ssh destination，
Hub 不知道也不需要知道 runner 怎麼走到那台機器。

跑一次就結束。**要它定期跑是 systemd 的事**：一個自己排程的 verifier 會長出 claim、
lease、expiry 這一整套跟證據無關的生命週期，而那正是 §4.2 的派工表刻意不做的東西。

#### 三條規則，寫在 binary 裡

| rule_id | 遠端量什麼 | 通過的條件 |
|---|---|---|
| `openclaw.current_release` | `readlink ~/.local/share/clawctl/openclaw/current` | 指到 `releases/<單一版本段>`，並把該版本段放進結構化 `observed_version` |
| `openclaw.gateway_http` | `curl -w '%{http_code}' http://127.0.0.1:18789/health` | status code 是 `200` |
| `openclaw.unit_state` | `systemctl --user show openclaw-gateway.service -p LoadState -p ActiveState -p SubState -p NRestarts` | `ActiveState=active` 且 `SubState=running` |

三條都走 SSH，因為 gateway 只聽 `127.0.0.1:18789`，Tailnet 上沒有可探測的服務（§4.2 量過）。
三條指令都是**單行 POSIX sh**，而且原樣進證據的 `command` 欄位並經過 shell quoting——
一份沒辦法被第三個人貼回終端機重跑的證據，只是一個要人相信的說法。

`openclaw.gateway_http` **只判 status，不讀 body**。gateway 回 `{"ok":true}` 是它對自己的說法；
「在那個 port 上答得出 200」才是觀測。這是 `PRODUCT.md` 地基二在這支程式裡的樣子。

`openclaw.unit_state` 逐行拆 `key=value`，不靠位置——`systemctl show` 用**自己的順序**回屬性，
不是 `-p` 的順序。這在演練裡直接量到：samplehub1／sampleagent4 回 `LoadState` 開頭，sampleagent2 回
`NRestarts` 開頭，兩種都存在帳本裡。

`observed_digest` 一律留空，理由見 §2.4。`observed_version` 只由通過的
`openclaw.current_release` 規則回報；其他 rule 或失敗列帶版號會被 Hub 拒收。

#### 「量不到」不是判決

ssh 走不到、遠端缺 curl／systemctl、機器不在 `--targets` 裡——這三種都**不寫證據**。
那張派工留在「已發出，等它回報」，指令以非零 exit 說出要修什麼。把量不到寫成 `failed`，
等於用一台好機器的網路問題去汙染升級判斷。

而且量測是**整張單全有或全無**：三條規則裡有一條量不到就一列都不送。傳輸本身仍是逐列
POST，連線若在中途斷掉，Hub 可能只收到一部分；因此派工完成判準也要求同一 producer 的三個
exact rule ID 全到齊，partial report 會繼續出現在 polling 清單，不會假裝做完。

#### 第一次真機演練量到什麼

verifier `sampleagent3 peer verifier`（`fleet_peer_agent`，failure_domain = sampleagent3），
從 sampleagent3 驗 samplehub1／sampleagent4／sampleagent2 的 6 張已結束工作單，18 列證據全部通過：

| 被測機器 | `openclaw/current` | gateway | unit |
|---|---|---|---|
| samplehub1 | `releases/2026.6.6` | `200` | `active/running`，restarts 2 |
| sampleagent4 | `releases/2026.5.26` | `200` | `active/running`，restarts 0 |
| sampleagent2 | `releases/2026.5.20` | `200` | `active/running`，restarts 0 |

**三台跑著三個不同版本**，而這是第一次由被測機器以外的東西講出來的。

傳輸失敗那條路也在真機上走過：把 targets 指到一個走不到的位址，三條規則都印
「量不到」、一列都沒寫、exit 1，派工維持 `awaiting_report`；改回正確位址再跑一次就完成。
第二次執行**只處理那一張**——因為「做完」是由證據推導的，已回報的單不會再出現。

#### 演練順帶抓到的兩個缺陷

* `passed` 那一句原本寫「而且 digest 與這張工作單相同」。沒回報 digest 的列也會落到
  `passed`，所以它把「沒有比過」講成「比過而且一樣」。Web 與 CLI 都改成
  「沒有一列的 digest 與這張工作單相衝突」。
* 每一次成功的派工都寫了**兩列** audit：store 在自己的 transaction 裡記了一次，
  但回傳的 result 沒有帶 `Audited`，於是 service 層的 fallback 又補了一列。
  `verifier revoke` 是同一個形狀。兩者都改成由 result constructor 帶著這個欄位。
  Live ledger 的 audit seq 56–65 留著那些重複列——append-only 的帳本不回頭改，
  seq 66 起是修好之後的形狀。

#### 它現在怎麼定期跑（sampleagent3，2026-09-12）

不是用 `ops/upgrade-agent.sh`。那支腳本做的是**換 agent 版本**：它會把
`clawctl-agent` 的 binary 與 agent／hermes／openclaw 三個 unit 一起換掉再重啟 agent。
把 verifier 掛上去等於為了裝第二雙眼睛，順手把 sampleagent3 的 agent 從 `fb0e269` 推到最新版，
讓它變成機隊裡唯一一台版本不同的機器——那是另一個決定，不該被這一個夾帶。

所以 verifier 是自己的一份安裝，跟 agent 平面完全分開：

| 東西 | 位置 |
|---|---|
| binary | `~/.local/bin/clawctl-verifier`（arm64，與 agent 同源不同份） |
| unit | `~/.config/systemd/user/clawctl-verifier.service`，`Type=oneshot`（來源 `ops/clawctl-verifier.service`） |
| timer | `~/.config/systemd/user/clawctl-verifier.timer`，`OnCalendar=*:0/15`（來源 `ops/clawctl-verifier.timer`） |
| bearer | `~/.config/clawctl/verifier.token`，`0600` |
| targets | `~/.config/clawctl/verifier-targets.json` |

`clawctl-agent.service` 完全沒有被碰過，仍是 `fb0e269`、`active/running`、restarts 0。
兩個 unit 檔在 repo 裡有來源（`ops/`），與機器上那兩份 byte-identical，內容契約由
`TestVerifierUnitKeepsTheSecondPairOfEyesSeparateAndScheduled` 釘住。
**代價要講清楚**：checked-in 的是內容，不是安裝動作——沒有腳本會把它們裝上去，
所以下次要換 verifier 版本得手動 scp 加 `daemon-reload`。
要把它收進機隊升級流程是還沒做的事。

⚠ timer 用 `OnCalendar` 不是 `OnUnitActiveSec`。第一版寫的是 `OnBootSec` 加
`OnUnitActiveSec`，裝上去以後 `list-timers` 的 `NEXT` 是空的——`Persistent=` 只對
calendar timer 有效，配 monotonic timer 會被安靜地忽略。那表示機器關機期間錯過的那一次
不會補跑，而派工會停在「等它回報」而沒有人知道為什麼。換成 `OnCalendar=*:0/15` 之後
`NEXT` 才有值，並實際看著它自己在 09:30:08Z 跑了一次（沒有人按）。

⚠ 這台機器上 `journalctl --user -u clawctl-verifier.service` 回「No journal files were
found.」——**不是沒有輸出**，是使用者沒有自己的 journal 檔，訊息落在系統 journal 裡。
要讀得用 `journalctl _SYSTEMD_USER_UNIT=clawctl-verifier.service`。同一台的
`clawctl-agent.service` 也一樣，所以這是既有狀態，不是這次裝出來的。

#### 讓它說一次「不對」（2026-09-12 09:31Z，sampleagent4）

在這之前它只證明了一件事：一切正常時它說得出正常。一個永遠說 OK 的驗證器
跟沒有驗證器在證據上是同一件事，所以刻意製造一次故障。

sampleagent4 上 `systemctl --user stop openclaw-gateway.service`，派一張它沒驗過的單
（`a35906a30b9f635c61c552f4ba1a9c05`，executor 自己回報 `succeeded`），跑 runner，再啟回來。
停機到重新答得出 `200` 全長約 32 秒（09:31:46Z → 09:32:18Z）。

| rule | 結果 | 量到什麼 |
|---|---|---|
| `openclaw.current_release` | 通過 | `current=releases/2026.5.26`（symlink 沒被停機影響） |
| `openclaw.gateway_http` | **沒通過** | `http_code=000` |
| `openclaw.unit_state` | **沒通過** | `LoadState=loaded` `ActiveState=inactive` `SubState=dead` |

三列的 `exit_code` 都是 `0`：這些是**觀測到的壞消息**，不是量不到。工作單的獨立 verdict
因此是 `failed`，而同一張單的 executor 證據仍然是 `succeeded`——第一次由被測機器以外的
東西講出「它說成功，但現在它不在」。

⚠ 復原時量到一個會製造假失敗的窗口：`systemctl --user start` 之後 unit 立刻是
`active/running`，但 port 要再過**約 20 秒**才答得出 `200`（t+5／+10／+15 秒都是 `000`，
t+20 秒才 `200`）。2026-09-13 在 sampleagent4 重做一次，unit 全程 active/running，HTTP 到 t+13
仍是 `000`，t+15 才是 `200`。

因此接 gate 時選定一條窄的 settling policy：只有 unit 已 active/running、gateway 可觀測但
尚未通過、而三條規則都可觀測時，runner 才每 5 秒重試 gateway，最多 30 秒。Unit inactive
立即留下失敗；重試途中變成量不到則一列都不送。這會吸收兩次實測的 15–20 秒正常啟動窗，
但不會把停機、SSH failure 或缺工具改寫成通過。`unit_state` 與 `gateway_http` 仍是兩條分開
的規則，因為 active/running 本身不是工作結果。

#### 進 stable promotion gate（2026-09-13）

Stable promotion 對每一個 effective successful canary job 要求一份完整 fleet-peer report，且
`current_release` 的結構化版號必須與 canary 要部署的 exact OpenClaw 版號相同。
Gate 有十一種狀態：`unassigned`、`awaiting_report`、`incomplete_report`、`producer_revoked`、
`digest_mismatch`、`release_mismatch`、`stale`、`failed`、`canary_not_succeeded`、`release_unreported`、`passed`。
分數的分母是這次 canary 的每一台機器；canary 工作單沒有成功的機器（被排除、沒開單、失敗或未完成）列為 `canary_not_succeeded`，下一步是修復後重跑 canary。
Web、JSON 與 CLI 都逐台顯示工作單、狀態與下一步。Hub 不解析 stdout 猜版號；舊 verifier 沒回版號時要求
先升級再重新派工，錯版則修復後重跑 canary。工作單終態仍只看 executor，因為 verifier 派工必須等終態才會交給 runner；把這個
要求塞進 `CompleteJob` 會直接死鎖。

最新一次派工要求新的完整 report，舊 pass 不會沿用；有效派工後已收到的 failure、digest clash
或 release mismatch 也不能靠再派一次洗掉，必須重跑 canary。Stale 與舊版 verifier 的缺欄可在升級後
重新派工重驗。Gate 只採 Hub
`received_at`，排除 evaluation instant 之後的列；`reported_verified_at` 不提供放行權限。

部署次序固定是先升 Hub（建立 `observed_version` 欄並只收 write v2），再單獨換 sampleagent3 的 verifier
runner。兩者版本不一致的短窗內 POST 會明確回 400，派工保持 awaiting／unreported，沒有相容 helper
替舊 stdout 補值；新 runner 第一次完整回報後才可能放行 stable。

## 5. 不管選哪一個，契約都必須成立的事

這幾條跟 producer 是誰無關，所以現在就能定，也應該現在就定：

1. **兩種 credential 不可互換。** machine bearer 寫不進 `evidence_role='independent_verifier'`；
   verifier credential 寫不進 executor 那一列，也永遠拿不到 job lease。這要由 store 的寫入
   條件擋住，不是由 handler 的 if 擋住 —— 今天 executor 那一側就是這樣做的（§2.2）。
2. **證據要綁 exact operation／attempt／target。** 一列獨立驗證必須指名 job、那一次 attempt，
   以及它實際觀測到的 artifact digest；`current_release` 另帶結構化版號。digest 或版號對不上各有
   `digest_mismatch`／`release_mismatch`，不是 fail 也不是 pass。
3. **兩個時間座標分開帶。** 每個 producer 帶自己的 clock，排序與 freshness 一律用 Hub 的
   `received_at`。理由見 §2.5 的 -81 秒。
4. **`unknown` 不等於 pass。** 沒有獨立證據、證據過期、digest 不符、版號不符、版號未回、
   verifier 被 revoke，都要是**各自不同**的 typed 狀態，不可以併成一個「沒過」，更不可以當成過。
5. **`provenance_recorded` 的分法要沿用。** 「我們知道是誰」跟「我們只是預設是誰」不能併欄。
6. **boolean 不是人翻的。** `independent_verifier` 由這張單的 live independent producer
   數算出來。沒有 producer 寫過列，它自己就是 `false`；有人寫了，它自己就是 `true`。
   沒有「決定翻旗標」這個動作可做，也就沒有人可以為了好看而翻它。

### 5.1 這六條現在落在哪裡（`d1997d3`，2026-09-11 已上線）

| 規則 | 實作位置 | 狀態 |
|---|---|---|
| 1 憑證不可互換 | `verifiers` 與 machine credential 是兩張表兩組 hash；隔離條件寫在 `RecordIndependentVerification` 的 INSERT…SELECT WHERE 裡（`verifiers.failure_domain <> jobs.machine_id`），不是 handler 的 if | 已做 |
| 2 綁 exact operation／target、digest 與版號 | 每列帶 job ID 與 `observed_digest`；`current_release` 另帶 `observed_version`。digest/version match 都由 Hub 比較，對不上就是獨立的 mismatch verdict | 已做；attempt ID 仍不存在於工作單模型，未偽造 |
| 3 兩個時間座標分開 | `reported_verified_at`（verifier 自報）與 `received_at`（Hub）分欄保存，存活與 `stale` 判定只採後者 | 已做 |
| 4 `unknown` 不等於 pass | 八種 verdict 依固定優先序：`absent`／`producer_revoked`／`digest_mismatch`／`release_mismatch`／`stale`／`failed`／`release_unreported`／`passed`；前六種各自是獨立狀態，都不是 pass 或 rule failure | 已做 |
| 5 `provenance_recorded` 的分法 | 獨立列一律寫 `provenance_recorded=1`，`producer_kind`／`producer_id`／`verifier_id` 取自 `verifiers` 那一列本身，不是 column default | 已做 |
| 6 boolean 由證據決定 | `independent_verifier` 由 live producer 數計算；strict client 改為接受兩種自洽形狀（有 live producer、producer 全撤銷），並拒絕旗標與列數互相矛盾的 body——fail-closed 沒有放鬆，只是判斷對象從「一律為假」變成「必須自洽」 | 已做 |

2026-09-12 起 live ledger 有獨立證據，所以被驗過的工作單算出來的
`independent_verifier` 是 `true`，其餘仍是 `false`——兩邊都是算出來的，不是誰決定的。
`Store.CompleteJob` 仍只採「一列可信 executor 證據、零失敗」；stable promotion 才另外要求
latest canary 每個有效成功 target 的完整 fleet-peer report。

## 6. 現在還不能做的事

* 不要先做「已驗證」的 UI。
* 不要把 executor 的自述改名成 verified／current／healthy。
* 不要為了讓 gate 好看，把 `unknown` 當 pass。
* 不要把 `independent_verifier` 當成可以翻的旗標——它是由 live producer 數算出來的，本來就翻不動。
* 不要為 verifier 加導覽入口，直到 register／revoke 也有 Web 那一半。故障域已經挑定、
  名冊上也有一個真的在用的 verifier，所以擋住這件事的不再是「沒有東西可看」，
  而是「看得到卻只能回去打指令」——那是半成品，不是 parity。
* 不要把獨立證據塞進工作單完成 gate。派工要等工作單終態才會送出；工作單反過來等派工
  會死鎖。它只參與 stable promotion。
* 不要讓 `external_job_runner` 或 `hub_prober` 的列授予 stable gate。這一版只有已實測的
  `fleet_peer_agent` kind 可以；kind-specific `grants_deployment_gate` 是 contract，不是 UI 提示。
* 不要把 30 秒 settling 放寬成一般重試。只有 unit 已 active/running、gateway 仍可觀測時才等；
  inactive 立即失敗，任何量不到維持 pending。這個數字來自兩次 15–20 秒的實測。
* 不要讓 runner 的規則從 Hub 下發。`VerificationAssignmentsResponse` 沒有 rule／command／digest
  欄位，而且不可以長出來——一個會執行 Hub 下發指令的 verifier 不是第二個判斷，
  是一條用單一 bearer 控管的遠端執行管道。
* 不要讓 runner 把「量不到」寫成 `failed`。ssh 不通是 runner 的問題，不是被測機器的問題。
