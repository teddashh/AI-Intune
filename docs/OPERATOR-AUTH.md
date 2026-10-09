# Operator identity, permissions, and browser boundary (Operator 身分、權限與瀏覽器邊界)

## Auth modes

Local accounts are the default for the recommended [Autopilot quick start](AUTOPILOT.md). The binary itself defaults to `tailscale`; choose with `CLAWCTL_AUTH_MODE` or `--auth-mode`.

| Mode | Operator authentication and listener |
|---|---|
| `tailscale` | Advanced private mesh: literal Tailscale listener, LocalAPI WhoIs, exact capability grants; no local account login. |
| `local` | Autopilot: local admin session cookie, public Host pinned to `CLAWCTL_PUBLIC_URL`; wildcard/loopback listeners allowed. |
| `both` | Local session first; absent cookie may fall back to WhoIs only with a literal Tailscale listener and valid grants/LocalAPI. An invalid cookie never falls back. Wildcard/loopback listeners provide local auth only. |

Forwarded headers never establish operator identity. Local mode includes setup-code bootstrap, multiple admin accounts, login rate limits and lockout; see [Autopilot security and recovery](AUTOPILOT.md#security-model-and-current-limits). Operator CLI/MCP still uses the Tailscale path; local maintenance CLI commands are documented separately there.

Account security can regenerate ten recovery codes with the current password and an unused authenticator code; recovery codes are not accepted. All previous codes are invalidated. Failures count toward the shared account/client lockout. New codes are shown once, and the regeneration audit contains no codes. Alternatively, stop the Hub and run `clawctl-hub regenerate-recovery-codes --db PATH --username U`, then restart it; the host command requires MFA enabled and prints one code per line.

## Quick start (English)

This existing quick start describes the advanced **tailscale** mode. For local accounts and HTTPS, start with [Autopilot](AUTOPILOT.md).

### Operator request checks
On every operator request (web console or `/v1/operator/*` API), the Hub verifies:
- **Authority / Host pinning**: The HTTP `Host` header must strictly match the literal Tailscale listener `IP:port` configured at startup (e.g. `100.64.200.2:8787` from `CLAWCTL_LISTEN`). Request headers like `Forwarded`, `X-Forwarded-*`, `X-Real-IP`, `Tailscale-*`, or `Authorization` are untrusted caller-controlled bytes and ignored for operator identity. This prevents DNS-rebinding attacks against authenticated browsers.
- **LocalAPI WhoIs**: The Hub inspects the connection peer via Tailscale LocalAPI `WhoIsForIP` (`Request.RemoteAddr` against the listener IP). The caller must be a non-tagged node owned by an explicit Tailscale user profile with stable user and node IDs.
- **Three capabilities**:
  - `<prefix>-view`: Read-only access to the web console, Prometheus `/metrics`, and viewing machines, jobs, deployments, artifacts, reports, and audit logs.
  - `<prefix>-operate`: Operational actions, including opening terminal sessions, terminal connect BATs, verifier dispatch/assignment, and deployment continue/retry.
  - `<prefix>-admin`: Administrative mutations, including enrollment token creation/revocation, registration limits, channel assignments, machine lifecycle, policy publication, and retention pruning.
- **No implied inheritance**: Capabilities are strictly non-hierarchical (`admin` does NOT imply `operate` or `view`). To grant full administrative access, a Tailscale grant must explicitly include all three capability keys.
- **Daemon requirement**: The Hub host requires `tailscaled >= 1.100.0`. Earlier daemons may ignore destination scoping on capability lookups; the Hub fails closed if `tailscaled` is older.

### Choosing the capability prefix
The capability prefix format is strictly validated by `NamesForPrefix` as `<domain>/cap/<app>`:
- Exactly three slash-separated components.
- All lowercase, without whitespace.
- The domain must have at least two labels (e.g. `example.com`), each matching `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`.
- `tailscale.com` and `tailscale.io` (and their subdomains) are reserved and rejected.
- The `<app>` slug must match `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`.

Use a domain you control, e.g. `example.com/cap/clawctl`. If you do not have a custom domain, use your GitHub Pages domain: `<your-github-username>.github.io/cap/clawctl`.

Set the exact same value in `CLAWCTL_OPERATOR_CAPABILITY_PREFIX` (or `--operator-capability-prefix`).

### Tailnet policy configuration (HuJSON)
Merge the following snippet into your Tailnet policy file in the Tailscale Admin console (**Access controls**). Merge into existing `tagOwners` and `grants` sections; do not overwrite or replace the entire file. Use the editor's built-in validation before saving.

```hujson
  "tagOwners": {
    "tag:clawctl-hub":   ["autogroup:admin"],
    "tag:clawctl-agent": ["autogroup:admin"]   // optional, if you tag managed machines
  },
  "grants": [
    // 1. Operators: reach the Hub port AND get the three app capabilities.
    { "src": ["you@example.com"], "dst": ["tag:clawctl-hub"], "ip": ["tcp:8787"],
      "app": { "example.com/cap/clawctl-view": [{}], "example.com/cap/clawctl-operate": [{}],
               "example.com/cap/clawctl-admin": [{}] } },
    // 2. Managed machines (agents): reach the Hub port only, no capabilities.
    { "src": ["tag:clawctl-agent"], "dst": ["tag:clawctl-hub"], "ip": ["tcp:8787"] }
  ]
```

- **`dst`**: May be the tag (`tag:clawctl-hub`) if the Hub joined with a tagged auth key (such as Fly or Docker packs), or the Hub's literal Tailscale IP (e.g. `["100.64.200.2"]`) if running on a host joined interactively as a user (`tailscale up`).
- **`src` for operators**: The operator grant's `src` user gets the capabilities on every device logged in as that user; to narrow it, list specific devices instead.
- **`src` for agents**: May be `tag:clawctl-agent`, specific host IPs/names, or `autogroup:member`. If your tailnet policy still contains the default allow-all grant, grant 2 is already covered, but grant 1's `app` block is always required.
- **Port**: The port in `tcp:8787` must equal the port configured in `CLAWCTL_LISTEN` (or `--listen`).
- **Capabilities**: A view-only operator receives only `example.com/cap/clawctl-view`. Grant all three keys to full operators.

### Tagged Hub hosts
- `install-hub.sh`'s final homepage probe runs from the Hub host itself; on a tagged host it always fails with `HUMAN_PRINCIPAL_REQUIRED`. Use `install-hub.sh` on a host joined as a user, or use the Docker/Fly packs (which do not run that probe) and verify from your own device.
- On any tagged Hub host (the Fly pack, or a VM joined with a tagged auth key), operator CLI commands that call the Hub API from that host (e.g. `clawctl-hub machines`, `clawctl-hub enroll-token`) are rejected the same way. Use the web console, or the CLI / `clawctl-operator` from your own tailnet device that is logged in as the grant `src` user. Local-only commands (`clawctl-hub notify-check`, the stopped-service `--db` break-glass path) are unaffected.

### First-login verification
Open `http://<tailscale-ip>:<port>/` in a web browser from a device on your tailnet, logged in to Tailscale as the user specified in the grant's `src` (`you@example.com`).

- **Success**: The browser loads the clawctl operator console / dashboard with HTTP 200.
- **Common failures**:
  - **HTTP 403 Forbidden (`OPERATOR_CAPABILITY_REQUIRED`)**: The Tailscale user identity is missing the required capability (error detail: `Tailscale identity is missing <prefix>-view`). Verify that the `app` map in your Tailscale grant contains `<prefix>-view`.
  - **HTTP 403 Forbidden (`HUMAN_PRINCIPAL_REQUIRED`)**: The request arrived from a tagged machine rather than a personal user node (error detail: `operator plane accepts only non-tagged devices with explicit Tailscale users`). Ensure your browser is running on an operator device logged into Tailscale with a human account.
  - **HTTP 421 Misdirected Request (`OPERATOR_AUTHORITY_REQUIRED`)**: The request used a MagicDNS name (e.g. `http://hub-node.ts.net:8787/`) or another hostname instead of the literal listener address (error detail: `請使用 Hub 明示的 Tailscale IP 與 port`). You must browse to the exact literal `http://<tailscale-ip>:<port>/`.
  - **HTTP 503 Service Unavailable (`AUTH_SOURCE_UNAVAILABLE`)**: Tailscale LocalAPI is unreachable, `tailscaled` is not in the `Running` state, or the installed `tailscaled` version is older than `1.100.0`. Check `journalctl -u tailscaled` and ensure LocalAPI socket access is available.
  - **HTTP 503 Service Unavailable (`AUTH_CONFIGURATION_INVALID`)**: The Hub's configured destination IP does not match any Tailscale IP currently assigned to the host.

The sections below (Traditional Chinese) are the full contract and the test-pinned route tallies.

---

> 下列「不另做帳號、密碼、cookie session」及 Tailscale 身分契約適用於 `tailscale` 模式；本機帳號＋公開 HTTPS 請見 [Autopilot（英文）](AUTOPILOT.md)。

> **Status / 狀態（2026-09-07 21:58Z）：Live / 已推 live。** Tailscale grant, Hub env,
> deterministic CLI discovery, writer fence, and `88fb5ff` enroll-token slice are live; the boundary
> in this document is the formal operator plane contract.
> （Tailscale grant、Hub env、deterministic CLI discovery、writer fence 與 `88fb5ff` enroll-token slice 都已上線；本文件的 boundary 現在就是正式 operator plane 契約。）

Jobs、Deployments、Artifacts、Updates 與 Machine Lifecycle routes 沿用這個已上線 boundary，但各 slice 的 live 版本與
驗收證據的最新部署紀錄是私人工作筆記，不在這個公開倉庫；candidate contract 不冒充
live 驗收。

`7c3c0bb` 先完成 auth boundary；`bf39c04` 再上線 deterministic CLI discovery、
process-lifetime writer fence 與 upgrade lock handoff。§6 保留兩次 rollout 的獨立證據，
不把後一版的驗收倒填到前一版。

clawctl 不另做一套帳號、密碼、cookie session 或角色資料庫。Hub 的 operator plane
直接使用 Tailscale 已經維護的 node identity、tailnet user 與 grants app capabilities；
瀏覽器的 CSRF 防護直接使用 Go 標準庫。這符合本專案的原則：通用能力交給上游，
clawctl 只保留「哪一條產品操作需要哪一種權限」的語意。

上游契約：

- Tailscale grants app capabilities：<https://tailscale.com/docs/features/access-control/grants/grants-app-capabilities>
- Tailscale grants 語法：<https://tailscale.com/docs/reference/syntax/grants>
- Go LocalAPI `Client.WhoIsForIP`：<https://pkg.go.dev/tailscale.com/client/local#Client.WhoIsForIP>
- Go `http.CrossOriginProtection`：<https://pkg.go.dev/net/http#CrossOriginProtection>

## 1. 證明了什麼，也沒有證明什麼

每一個匹配已註冊 UI／operator API route 的 request 都先要求 HTTP `Host`
等於啟動時釘住的 literal Tailscale listener `IP:port`，再用
`Request.RemoteAddr` 與該 listener IP 呼叫 `WhoIsForIP`。Hub 不讀 `Forwarded`、`X-Forwarded-*`、
`X-Real-IP`、`Tailscale-*` 或 `Authorization` 來判斷 operator；在目前 direct-tailnet
拓撲裡，那些都只是 caller 可以自己填的 HTTP bytes。

`Host` 檢查不是把 header 當身分，而是釘住 server authority：否則已授權
工作站瀏覽惡意網站時，對方可用 DNS rebinding 使 `evil.example`
指到 Hub，並送出對惡意網站本身來說合法的 same-origin request。目前
只接受 literal listener authority；MagicDNS/custom hostname 要等明確 allowlist 實作，
不從 DNS 或 request header 自動猜。

成功判決證明：

1. 來源是 tailscaled 認得的非 tagged node；
2. 該 node 有明確的 tailnet user profile 與 stable user/node ID；
3. Tailscale 對這個 source → Hub destination 回傳該 route 要求的 exact app capability。

它不證明「此刻坐在鍵盤前的人」剛完成登入或 MFA。若未來需要 Entra Conditional
Access、fresh MFA、明確 logout 或 browser session，再在 operator plane 前加入
oauth2-proxy／Entra OIDC；不要把目前的 node-owner assertion 改名成那種保證。

Producer plane 用的是 bearer，而且有兩種：machine bearer 與 verifier bearer。
兩者是不同表、不同 hash namespace，互相不認：machine bearer 送到 `POST /v1/verifications`
回 401，verifier bearer 送到 `POST /v1/jobs/{id}/verifications` 也回 401，兩者都不能當
operator credential。認證失敗一律回同一句話，不透露憑證是未知、已撤銷，還是屬於另一個 plane。
Verifier credential 在註冊的 fresh response 明文出現一次，之後 list、detail、preview、
receipt、audit 與 replay 都沒有這個欄位；遺失的唯一復原路徑是撤銷後重建。

## 2. 三個 capability 不做隱含繼承

必填 prefix 由環境或 flag 明示，例如：

```text
CLAWCTL_OPERATOR_CAPABILITY_PREFIX=example.com/cap/clawctl
```

Hub 只認三個 parameter-free fixed keys：

```text
example.com/cap/clawctl-view
example.com/cap/clawctl-operate
example.com/cap/clawctl-admin
```

程式內沒有 `admin ⇒ operate ⇒ view`。需要完整控制台的管理者必須由 Tailscale policy
明確取得三把 key；這讓 policy 保持 authority，不讓 Hub 重新解釋一個自製 role hierarchy。

目前 route 分類：

| Capability | Route |
|---|---|
| `view` | Prometheus `GET /metrics`（plain text；沒有 grant 回 403）；所有已註冊的 HTML/CSV `GET`；管理中心導覽語言的同源 form `POST`（唯一歸為 `view` 的 unsafe route）；operator Machines／Jobs／Deployments／Artifacts list/detail、Store packages、Profiles、job evidence、machine evidence、artifact-fetch operation list/detail、Updates／Audit／Changes／Reports 落地頁、每日早報預覽與單機事件時間軸（含 CSV 匯出）、資料揭露面與單機留存量（含 CSV 匯出）／Tailnet／Retention read、註冊上限 read、軟體清查（含 CSV 匯出）、machine-channel、machine-lifecycle、pending enrollment-ticket metadata 與 verifier registry list/detail、disk-clean summaries `GET` |
| `operate` | Connect BAT；開啟終端；終端頁；終端連線；deployment Continue/Retry 與 skip failed batch、noop diagnostic 的 Web／JSON preview/apply；派工（指名 verifier 驗某一張工作單）的 Web／JSON preview/apply |
| `admin` | enroll-ticket create／pending-ticket revoke、註冊上限設定／取消、channel 指派、machine lifecycle、Hub 名冊重新命名／備註、Tailnet ignore、裝置設定原則發佈／指派、裝置合規性原則發佈／指派與 retention prune 的 Web/JSON preview/apply、operator machine-channel `PUT`；artifact fetch、Standard Store package publication、Profile publication／Assignment 與 deployment Create/Abandon 的 Web 與 JSON preview/apply；verifier 註冊與撤銷的 JSON preview/apply（目前沒有 Web 入口）；disk-clean profile、dry-run、canary、continue 與 abandon 的 JSON preview/apply（維護頁只讀） |

此 Tailscale surface 不含另列於 [API-SURFACE.md](API-SURFACE.md) 的 6 條公開 account operations；包含新的 Admin keyed-installer download。

目前 code route manifest 固定為 234 operations：18 條 non-operator，加上 216 條 operator
routes；operator manifest 的 exact capability tally 是 `view=83`、`operate=24`、`admin=109`，
其中 `/v1/operator/*` JSON routes 共 107 條、HTML/BFF/CSV/download 共 109 條。這些數字由 route manifest 測試固定，不能靠
較高 capability 的隱含繼承湊數。
234-operation boundary 包含 machine-bearer bootstrap readiness receipt、operator-only bundle download、Tailnet Settings、Retention Maintenance、資料揭露面、註冊報告、每日早報預覽、註冊上限、軟體清查、每機安裝狀態、發佈與指派對照、Hub 名冊重新命名／備註、verifier registry 與派工、disk-clean 摘要與發布；完整 route 與 ledger 證據的實機紀錄是私人工作筆記，不在這個公開倉庫。

operator route manifest 與實際註冊清單在 Hub 啟動時做雙向比對。繞過 operator
boundary 的 18 條 route 也有另一份完整 manifest；兩份不能
重疊。那 18 條是 15 條 machine transport、2 條 verifier transport、`/healthz`；其中
`POST /v1/verifications`（write schema v2）與 `GET /v1/verification-assignments` 走的是 verifier bearer 而不是 machine
bearer，manifest 仍把它們歸在 non-operator，因為它們同樣不經 operator boundary。
Machine plane 的 `/v1/` 第二段必須是 literal 且不能是 `operator`，所以
`/v1/{plane}/...`、`/v1/{rest...}` 這類可能在 root mux 蓋過 operator API 的 pattern
會被拒絕。新增 route 卻忘記分類、policy 多出不存在的 route、重複 route 或不合法
分類都會讓 Hub 拒絕啟動；runtime 仍保留 fail-closed 檢查。

## 3. samplehub1 的 Tailscale grant

在 Tailscale admin console 的 access controls 中，把下列 entry 合併進既有 `grants`
陣列；不要覆蓋其他 policy：

```json
{
  "src": ["operator@example.com"],
  "dst": ["100.64.200.2"],
  "ip": ["tcp:8787"],
  "app": {
    "example.com/cap/clawctl-view": [{}],
    "example.com/cap/clawctl-operate": [{}],
    "example.com/cap/clawctl-admin": [{}]
  }
}
```

先用 Tailscale policy editor 自己的 validation／test 功能驗證再儲存。這份 grant 同時
限制 source identity、Hub destination 與 TCP reachability；Hub 再從 `WhoIsForIP`
取得 destination-scoped app capability 做應用層判決。

然後在 samplehub1 的 `~/.config/clawctl/hub.env` 加：

```text
CLAWCTL_OPERATOR_CAPABILITY_PREFIX=example.com/cap/clawctl
```

設定檔本身不是 credential，但仍沿用既有 `0600`。`--listen` 必須是明確的 Tailscale
IP（目前 `100.64.200.2:8787`）；hostname、`0.0.0.0`、LAN/public IP 與 loopback 都會
在開 DB 之前被拒絕。authority destination 永遠不從 HTTP `Host` 推導。
`CLAWCTL_PUBLIC_URL` 若仍存在，只能與推導出的
`http://100.64.200.2:8787` 完全相同（尾斜線除外）；目前沒有 TLS termination 或
可信 hostname allowlist，另一個 URL 會在開 DB 前被拒絕，不能產生一組 UI 會拒絕的
早報連結或 enroll 指令。

Hub 每次授權前也會透過 LocalAPI 確認 tailscaled 至少是 `1.100.0`；
更舊的 daemon 可能忽略 `dst_ip` 而回傳未限定 destination 的 capability，
所以一律 fail closed。目前實機是 `1.102.2`。

### 3.1 CLI discovery 不是另一份 grant

上面的 Tailscale grant 是持久 policy，不是每次操作都要 user 重跑一次。每一個 HTTP
request 由 Hub 向 tailscaled LocalAPI 重驗目前 source／destination／capability；CLI 本身不
保存 token，也不偽造登入 header。

CLI 要保存的只有同一個 Hub origin。正常順位固定為：

1. explicit `--hub-url`
2. `CLAWCTL_HUB_URL`
3. `${XDG_CONFIG_HOME:-$HOME/.config}/clawctl/operator.json`

`operator.json` 只有一個欄位：

```json
{"hub_url":"http://100.64.200.2:8787"}
```

Fresh `install-hub.sh` 啟動服務後以 operator 首頁 HTTP 200 證明本機 principal 有 `view`，
再原子寫入這個檔；它不宣稱已驗過 `operate` 或 `admin`。Forward `upgrade-hub.sh` 則在停 live
Hub 前用 candidate 與 systemd 真正解析出的環境，從同一份 coherent LocalAPI `CapMap` 證明
`view,operate,admin` 三把獨立 key，才原子寫入。兩者都把 `clawctl` config directory 收斂為
`0700`，檔案為 `0600`。

scripts 與 Go 的 `os.UserConfigDir` 使用同一條規則：`XDG_CONFIG_HOME` 有設定時必須是
canonical absolute path，operator config 寫到該 root；未設定時才用 `$HOME/.config`。
這只適用於 operator client discovery；systemd unit 與 `hub.env` 仍固定在
`$HOME/.config/systemd/user`、`$HOME/.config/clawctl`，不會隨 custom XDG root 搬家。另一台
operator workstation 只需一次建立同一份檔案，或設定 env；瀏覽器仍直接開 URL，不讀本機
CLI config。選中的來源如果空白、malformed 或不安全會直接失敗，不能往下猜。`agent.json`
含 machine bearer，`hub.env` 由 systemd 解析，`CLAWCTL_PUBLIC_URL` 是 server assertion，三者
都不是 operator discovery authority。

Client 與 server 共用 strict endpoint parser：目前只接受
`http://<canonical-literal-Tailscale-IP>:<nonzero-port>`，且沒有 base path、userinfo、query
或 fragment。CLI 停用 ambient HTTP proxy、拒絕 redirect。Machine mutation client 會把
machine ID、channel、display name、revision 與 ETag 對回原 request；Machines 與 Jobs read
client 也 strict decode schema、nested unknown/duplicate/null/trailing body、大小上限與跨欄位
invariants，不因 HTTP `2xx` 就接受不完整或矛盾的資料。Machines list client 另在 I/O 前驗全部
filters/cursor，核對 creation ceiling、page order/continuation、global/matched totals、state lower bounds、
safe projection evidence，並拒絕晚於 `evaluated_at` 的 health evidence。Deployment client 另核對 preview
schema/policy/canonical lowercase `sha256:` digest、target canonical order、artifact/promotion coherence、control revision、
retry lineage、fresh/replay status 與 header/body replay evidence。Deployment detail 的獨立證據也走同一條
規則：每個已開單 target 必須帶八種之一的 verdict，`absent` 必須零列、零 live producer 必須是
`producer_revoked`，未開單 target 不得帶 verdict，而 deployment 層的 rollup 由 client 自己從 targets
重算；`release_mismatch`／`release_unreported` 也必須落在固定位置。Job evidence v6 另由 strict client
重算 `version_reported`／`version_matches_job`，不採信 verifier 的判斷或 stdout。rollup 只要與 targets
不一致，整個 response 被拒，不會挑其中一邊當答案。

Machine lifecycle strict client 也會在 I/O 前拒絕非 canonical machine ID、state、revision、digest、
typed confirmation/reason 與不完整 retry coordinates；response 必須同時符合 machine/state/revision、
`ETag`、fresh/replay header/body、denominator、credential authentication、pending-ticket redemption、
retired timestamp、transition event 與 blocker invariants。它不會因為 JSON 可 decode 或 HTTP `2xx` 就
接受矛盾 receipt。

Artifact client 同樣 strict decode nested unknown/duplicate/null/trailing body、大小上限、status/count、
digest/time、operation state/phase/progress 與 result/failure invariants。`artifact list/show` 與
`artifact fetch list/show` 沒有明示 transport 時必須走 deterministic discovery HTTP；`artifact fetch`
先取得 fixed-policy preview，再帶 typed exact version、reason、`Idempotency-Key` 與 preview digest
durable enqueue。Production CLI 拒絕 `--registry`／`--max-bytes`，不能把 caller URL 變成 SSRF
authority。只有 explicit `--db` 才進 stopped-service lifecycle/writer/systemd fence。

HTTP artifact enqueue 在送出 apply 前建立 private canonical recovery receipt，綁 exact request、
idempotency key、private reason、preview digest 與 normalized Hub authority；預設 parent `0700`、
檔案 `0600`，先 fsync/revalidate 再送出。Ambiguous response 只可用
`artifact fetch --recovery-file <absolute-path>` 原樣 replay；receipt 在完整結果輸出後才做
inode-checked cleanup，明確 4xx 拒絕也在收到 canonical 判決後清除；5xx 與 transport
ambiguity 則保留給 exact replay。Private reason 不回 terminal。

`clawctl-hub job list`／`job show`／`job evidence` 與 Machines read 一樣，沒有明示 transport 時必須走上述
deterministic discovery HTTP，discovery 或 response 驗證失敗都不得靜默 fallback SQLite。
`deployment preview/create/list/show/continue/retry/abandon` 亦遵守相同規則；mutation 先 preview，
再以 digest、typed confirmation、idempotency key 與（既有 deployment）control/opened-batch
precondition apply；opened batch 必須至少為 1，control revision 已耗盡時三個 action eligibility
明列 blocker 且禁止 mutation。
Ambiguous response 只能重用原整組 canonical inputs，不得只換 key 或 revision。
四種 deployment HTTP mutation 會在 apply 前把 exact request、key、private reason、CAS
pointers 與 normalized Hub authority 寫入 private canonical recovery receipt；預設目錄 `0700`、
檔案 `0600`，並在發送前 fsync/revalidate。同 action 可用 `--recovery-file` 原樣重放，
檔案只在完整結果寫出後做 inode-checked cleanup；private reason 不會出現在 terminal。
只有 explicit `--db <absolute-canonical-existing-ledger>` 才進 stopped-service lifecycle/writer/
systemd fence。`job create --kind noop` 已使用 canonical diagnostic preview/apply operator service；
正常模式走 deterministic discovery HTTP，明示 `--db` 才進同一 stopped-service fence。

`clawctl-hub machine lifecycle` 正常模式同樣只走 deterministic discovery HTTP；read、
`--set active|retired --preview` 與 apply 都使用同一個 strict client。Apply 不會自動把 GET
的 display name 代填成 confirmation，而是強制操作者明示 `--confirm-name` 與 `--reason`。
Ambiguous response 只可同時重用原 `--idempotency-key`、`--expected-revision` 與
`--preview-digest`，不得混搭新 preview。只有 explicit `--db` 才進已停服的同一 canonical
service；舊 top-level `retire` 只是安全 alias，不再保留無 revision/preview 的 direct Store 旁路。

## 4. HTTP 與錯誤契約

順序固定是：

```text
route classification → pinned Host authority → LocalAPI authorization → CrossOriginProtection → handler
```

狀態碼：

| 狀況 | HTTP | code |
|---|---:|---|
| `Host` 不是釘住的 listener authority | 421 | `OPERATOR_AUTHORITY_REQUIRED` |
| Tailscale 找不到 peer／來源不是 tailnet IP | 401 | `OPERATOR_IDENTITY_REQUIRED` |
| tagged node 或沒有明確 user | 403 | `HUMAN_PRINCIPAL_REQUIRED` |
| 缺 exact capability | 403 | `OPERATOR_CAPABILITY_REQUIRED` |
| 跨來源 browser write | 403 | `CROSS_ORIGIN_REQUEST` |
| LocalAPI/socket/timeout／daemon 過舊 | 503 | `AUTH_SOURCE_UNAVAILABLE` |
| malformed app capability／route manifest | 503 | `AUTH_CONFIGURATION_INVALID` |

已註冊的 `/v1/operator/*` route 的 auth/Host/CSRF denial 回穩定 JSON
`{"code","message"}`；Web route 回 HTML 說明頁。未知 path 與 method mismatch 沒有
domain handler，保留 Go `ServeMux` 的 404/405，不把它偽裝成已分類 API contract。
Operator response 一律 `Cache-Control: no-store`，並帶 CSP、`frame-ancestors 'none'`、
`nosniff` 與 `Referrer-Policy: no-referrer`。

目前 35 條 JSON operator routes 包含 Machines/Jobs/Deployments/Artifacts read、job evidence、machine evidence、artifact-fetch
preview/create/operation read、Updates/Audit/Changes read、machine channel、machine lifecycle read/preview/apply、enroll token、pending-ticket revoke，
以及 Deployments create/continue/retry/abandon preview/apply。
所有 read 要求 exact `view`；deployment Continue/Retry 要求 exact `operate`；Create/Abandon 與
artifact fetch、其餘管理 mutation 要求 exact `admin`。Preview 不寫 state，回 policy-bound digest 與明列影響；
apply 缺 preview 回 428 `PREVIEW_REQUIRED`；enrollment policy digest 過期回 412
`PREVIEW_STALE`，deployment snapshot/material 變更回 412 `DEPLOYMENT_PREVIEW_STALE`；計畫沒有可排進的機器
回 409 `DEPLOYMENT_NO_INCLUDED_TARGETS`。同一 idempotency key 換 body 或 operation 回 409
`IDEMPOTENCY_CONFLICT`。Deployment Create/Retry fresh
回 201，Continue/Abandon fresh 回 200；成功 replay 回 200、`Idempotency-Replayed: true`，
並在 body 明列 `replayed=true`。拒絕 replay 保留原 domain error status/code，以同一 header 標示 replay，
error body 不冒充 success receipt。Create apply 的六個 planning fields 都必須 explicit；missing/null
或 batch/timeout 的顯式 `0`
在進 service/idempotency ledger 前以 audited transport rejection 擋下。Deployment cached success
只有在 canonical success receipt、request-digest-bound 原 typed confirmations、action lifecycle、完整 durable
deployment/job/target graph 與唯一原始 success audit 全一致時才可 replay；Retry 另重驗整條
ancestor lineage。Graph 會核對 desired-state identity、target machine registry、known job state 與
terminal-state/`terminal_at` 證據；owner admission 也拒絕被 finished/corrupt ledger 隱藏的同資源
非終態 job。會再開 job 的 delayed batch／Continue、Retry、promotion 與 cached-success evidence
逐張核對 linked job artifact digest 與 OpenClaw spec；fresh finish-only Continue／Abandon 不依賴已下架的 material，
promotion lineage 則逐代重驗 stored batch plan。Cached rejection 則驗
canonical rejection code/detail 與唯一原始 failed audit，不重讀今天的 fleet state。Corrupt/orphan cache
fail closed 且不把 forged value 複製到 audit。
Enrollment create fresh 也回 201，但只在該次 response
交付明文 token；replay 只回 redacted receipt。如需復原，必須撤銷原票、退役原本
未報到的名冊列，再建立新列與票；目前沒有原 machine reissue，也不會重顯或另發 secret。

Artifact fetch preview 回 200 且不入列；create fresh 與成功 replay 都回 202，replay 由
`Idempotency-Replayed: true` 和 body 的 `replayed=true` 共同證明。Metadata 在 apply 時改變回
412 `ARTIFACT_FETCH_PREVIEW_STALE`，同 package/version 或相同 material 已有 active operation 回
409 `ARTIFACT_FETCH_ACTIVE`；暫時 registry/prepare 失敗回 502，尚未入列也不消耗 key。
Operation read 只要求 exact `view`，enqueue/preview 則要求 exact `admin`；worker-only tarball URL、
run token、idempotency key、request digest 與 private reason 不進 safe operation response。

Jobs read 的 list cursor 固定原 filters 與第一頁的 creation ceiling；後頁不會納入 ceiling
之後建立的 job，但這是 **live keyset** 而非資料庫 snapshot。既有列的 state、lease、event／
verification counts 仍可改變，state filter 的成員也可能在頁與頁之間進出。Cursor 只適用同一
ledger generation 的連續 traversal；stopped maintenance、restore 或手動 `VACUUM` 後必須丟棄
舊 cursor、重新讀第一頁，server 不把 hidden rowid 當 durable generation token。JSON detail 只回
desired/event/verification 的受限 metadata，刻意省略 raw desired spec、event payload、verifier
command、stdout/stderr 與 free-form detail；safe Store query 也不選取 raw content 欄位。Agent
event payload／verification excerpts 寫入上限為 64 KiB，verification command 為 16 KiB。
這個安全投影之外，`GET /v1/operator/jobs/{id}/evidence`（exact `view`）、`job evidence` 與 HTML job detail
共用 `operator.Service.JobEvidence` 的 `schema_version=6` typed evidence DTO：desired spec、verifier
rule/command/output 與 rejected code/detail 經 scrub、每欄依自報 `max_bytes` 截斷並帶 bytes/truncated/issues，raw payload
只回 byte 數；HTML 不再有 raw Store-model bridge。Event/rejection 明列 executor agent 或 Hub dependency
scheduler producer；新 event/verification row 明列 producer ID/kind、role、authority 與 row provenance，
verification 另存 Hub received_at；legacy row 維持 provenance 未記錄／received_at null。
`independent` 是獨立的第二段，verdict 為 `absent`／`producer_revoked`／`digest_mismatch`／
`release_mismatch`／`stale`／`failed`／`release_unreported`／`passed` 八選一，每列帶 verifier 的
kind／ID／display name／failure domain／撤銷狀態、`verifier_bearer` authority，以及 verifier 自報
時鐘與 Hub `received_at` 兩個座標。`current_release` 另帶結構化 `observed_version`；Hub 對 desired
spec 的 exact OpenClaw version 重算 `version_reported`／`version_matches_job`，不解析 stdout。
`disclosure.independent_verifier` 由這張單的 live producer 數算出來，不是常數；
被獨立 producer 回報過的工作單算出 `true`，未回報者仍是 `false`。

v5 另新增 `independent.assignments`：每一列說明哪一個 verifier 被指派看這張單、
何時指派、以及四種互斥狀態之一 —— `reported`（已送出獨立證據）、`producer_revoked`
（指派的 verifier 已撤銷，不會再回報）、`waiting_for_job`（等這張工作單結束才會發出）、
`awaiting_report`（已發出，等它回報）。這一段存在的理由是讓 `absent` 這個 verdict
分得出「沒有人被指派」與「有人被指派但還沒回報」；狀態全部由既有事實推導，
沒有任何一個欄位可以被宣告成完成。
executor 段的 role 仍只有 executor，operator 畫面或 API 同時讀到兩者不能算雙觀測。

Machine evidence v4 由 `GET /v1/operator/machines/{id}/evidence`（exact `view`）、
`machines evidence` 與 machine HTML detail 共用 `operator.Service.MachineEvidence`。Systemd 與 journals 都由
machine bearer 下的 observer agent 取得，前者來自 systemd metadata、後者來自 `journalctl`；run summaries 則由 OpenClaw 寫入自己的
sqlite，再由同一 observer agent 轉送。Systemd metadata 另列 producer、agent/Hub clocks、invalid count
與 bounded paging；MainPID 不存在於 DTO，`systemd_main_pid_excluded=true`，而
`systemd_state_is_work_outcome=false`。DTO 分開標示 producer 與 relay，固定
`independent_verifier=false`、`status_is_outcome=false`、`terminal_outcome_recorded=false`；看到
OpenClaw `status=ok` 不得當成任務成功。Journal err/example 在 agent 端做 secret-shape redaction，
run summary 沒有 redaction step，兩個 disclosure boolean 不得合併。所有文字逐欄 bounded scrub，
block fields 只保留 LF/TAB，response 每段 1..100 筆並回 truthful total/truncated；strict client 另有
32 MiB response cap。`read_failed` 與 quiet 分開，agent 自己的 line cap 與 Hub byte truncation 分開，
沒有 journal observation 的 unit 明列為 not collected（不是 quiet），存在但解不開的 journal 另計
undecodable。Machine detail 的兩次 in-process read 共用 evaluation cutoff，但不是跨 query atomic snapshot。
同一 DTO 的 credential section 只回本機檔案狀態、verification method、bounded note/error、續期史與
bounded peer evidence；固定 `credential_status_is_session_validity=false`，active account ID、token/hash 等
secret fields 結構性排除。Occupancy section 的 OpenClaw producer、observer-agent relay 與 Hub aggregation
分開揭露；只使用 completed-run ledger，provider 不正規化、error 不分類、process 不作證據，並排除
profile/session/job/model/raw-error event details。兩段皆保留 agent/Hub clocks、truthful total/truncated/invalid。

Pending-ticket revoke 也採 preview + idempotency：preview digest 綁住 hidden ticket identity、
registry retained、denominator delta 0 與 active agent credential unaffected。Fresh revoke 回 201，
同 request replay 回 200 與原本的 redacted receipt；它從不回 token hash，也不把 pending ticket
撤銷寫成「active agent credential 已撤銷」。

Machine lifecycle GET 要求 exact `view`；Web/JSON preview 與 apply 要求 exact `admin`。Preview 必須有
`expected_revision`，apply 另要 `Idempotency-Key`、preview digest、reason 與 exact display name；
缺 precondition/preview 回 428，stale revision/digest 回 412，active→retired 有非終態 job 回
409 `MACHINE_HAS_ACTIVE_JOB`，revision 耗盡／損毀回 409 `LIFECYCLE_REVISION_EXHAUSTED`。
Fresh 與成功 replay 均回 200；replay 以 `Idempotency-Replayed: true` 與 body
`replayed=true` 共同證明。相同 key/body 的拒絕 replay 保留原 status/code，不重跑今日的
credential/ticket/job 狀態。Malformed/oversized apply 無法得到 canonical digest，因此只記
`machine-lifecycle` transport rejection audit、不占 idempotency key；修正 bytes 後可沿用同 key。
退役不刪 bearer 或 pending ticket，所以 restore active 可能立即恢復保留 bearer 的驗證與
未過期 ticket 的兌換；這是 preview/typed confirmation 的安全契約，不是 credential rotation。

`/metrics` 不依賴 LocalAPI（Prometheus 故障不能拖垮 machine plane），但它會曝露機器
ID、名稱與版本，所以同樣只接受啟動時釘住的 literal Tailscale `Host`；錯誤 Host 回
421，且不寫 log 或 DB，避免把探測流量變成另一條放大路徑。`/healthz` 只回
`alive`，刻意保持無身分、無機隊資料的 liveness probe。

`CrossOriginProtection` 依上游契約允許沒有 `Origin`／`Sec-Fetch-Site` 的非瀏覽器 CLI，
但 CLI 仍經同一個 LocalAPI capability 驗證，所以這不是 auth bypass。程式沒有加入
trusted origin 或 insecure bypass pattern，也沒有自己維護 CSRF cookie/token lifecycle。

## 5. Audit

每一筆已接入的 control write 同時保存 transport、adapter 與可用的 domain correlation；
HTTP adapter 另外保存 authority：

- transport：source address、可讀 node/login、User-Agent；
- authority：stable Tailscale user ID、node stable ID、exact authorized capability、auth
  method、decision code；Host/CSRF 等 request guard 另存 `boundary_decision`；
- adapter：`web`、`operator-api`，或刻意繞過 HTTP 的 `direct-db-cli`；後者的
  `auth_*` 保持 NULL，UI 明示「不適用（本機 direct-DB process）」而不虛構 principal；
- domain correlation：action、target、outcome、idempotency key／request digest（適用時）。

Host、auth 或 CSRF 在 unsafe method 前擋下時，只可能留下 `operator-denied` boundary
row；domain handler、state revision 與 idempotency ledger 都不能被碰到。拒絕流量是
不可信輸入，不能靠「每一包都永久寫 SQLite/journald」來製造磁碟 DoS，因此採固定
cardinality 的全域 window：每分鐘前 12 筆 denial 進 log，而 unsafe denial 另有自己的
前 12 筆持久化配額；safe GET flood 不會吃掉第一筆 unsafe audit。超額數量在下一個
window request 聚合成一筆 summary，`operator-denied` 在 DB 只保留最新 1000 筆，普通
domain/control audit 不受這個 ring 影響。Audit UI 再把 denial noise 最多顯示 50 筆，
保留正常動作的可見性；這是顯示採樣，不是刪除正常 audit。

`/audit`、`GET /v1/operator/audit-events` 與 `clawctl-hub audit [list]` 共用同一個
`operator.Service.ListAudit` safe read。它支援 machine、可重複 action、outcome、principal、
capability、source kind、correlation、秒精度 RFC3339 時間窗、denial mode、limit 與 opaque cursor；
cursor 綁 exact filters、最後一筆 writer sequence 與第一頁 creation ceiling。`matched_total` 是
denial 採樣前符合數，`total` 是採樣後整段 traversal 的可見總數；預設只納入全 traversal 最新 50 筆 denial，
`denials=all` 才不採樣。Safe projection 把畸形時間／結果改成 `null` 並附 `issues`，無效 UTF-8
與 Unicode control/format 字元替換且列入 `altered_fields`，不把 private Store row 直接序列化。
這仍是 live keyset；denial 的最新 1000 筆 ring、restore 或 `VACUUM` 都可能使舊 traversal 失效，
所以 cursor 不宣稱 durable generation 或 historical snapshot。

`/reports/changes`、`GET /v1/operator/changes` 與 `clawctl-hub report changes` 也共用同一個
`operator.Service.ListChanges` safe read，兩條 HTTP GET 都要求 exact `view`。Exact machine、repeatable
kind、subject、`(from,to]` 秒精度 window、limit 與 cursor 在進 Store 前先驗證；預設 24 小時、最多
30 天與每頁 100 筆。Cursor 固定 filters/window 與各 append source ceiling，但 retention 可以讓舊
traversal 以 `410` fail closed，所以不稱為 snapshot。Exact filters 在 Store 下推，兩個 total 與 kind
counts 都描述篩選後 traversal。單次成本限 250,000 筆 observation window rows、每個 selected source
250,000 筆 timestamp metadata、10,000 endpoint keys、10,000 transitions、32 MiB candidate metadata、
32 MiB endpoint payload、兩個並行 reader 與 10 秒；超限／忙碌／逾時分別回
`422`／`429`／`503`。Registry/state 是逐筆 transition；agent observation 是 endpoint delta，排序只信
Hub receipt/transition time。DTO 不含 raw payload、hostname/account、path、PID、argv 或 free-form reason；
非 allowlist subject 固定遮罩且不能 query，並明列 retention、registry/state transition migration 與
malformed timestamp coverage；未被 exact filters 選取的 source 回 `not_applicable`。State Changes 讀
append-only event ledger；升級前被 current-span 同秒覆寫
而已遺失的值不會被虛構回填。

`/reports/tickets`、`/reports/tickets.csv`、`GET /v1/operator/tickets` 與 `clawctl-hub tickets`
共用同一個 typed ticket-usage read。HTTP routes 要求 exact `view`，不接受 ETag 或 idempotency replay。
固定 `[from,to]` Hub receive-time snapshot 最長 30 天，查詢先受 candidate row/byte 上限保護，再投影為
有界且可追蹤 truncation/issues 的 provider、machine、agent 與 error evidence。CLI 正常模式使用
deterministic Hub discovery；只有明示 `--db` 才能進 stopped-service writer-fenced break-glass。

`/tenant/data`、`/machines/{id}/data`、`/machines/{id}/data.csv`、
`GET /v1/operator/data-disclosure`、`GET /v1/operator/machines/{id}/data`、`clawctl-hub data` 與
`clawctl-hub machine data` 共用同一份資料揭露目錄。五條 HTTP GET 都要求 exact `view`，一個
query parameter 都不接受。目錄自己不抄任何對照表：哪些表存著機器的資料由 SQLite 的
`machine_id` 欄位回答，哪些表會被時間清、看哪一個保留期由 `pruneJobs` 回答，「留多久」讀的是
這台 Hub 現行的保留期而不是預設值。每一類宣告的「看得到它」那一頁與所需 capability，會跟 route
manifest 上那條 route 真正要求的 permission 對照。單機那一份逐類量列數、最舊與最新，時間一律
用 Hub 自己的鐘；沒有 Hub 時刻的列照樣算進列數但不進最舊 / 最新，也不被擺到某一個時刻上。
揭露面只讀不寫，退役也不刪任何一列——它改變的只有「不再有新的一列」。

Host/CSRF 在授權前擋下的 row 會明示「授權未評估」，聚合 row 明示逐筆授權結果沒有
保留；只有真正的舊 row 才標 legacy。舊 row 的 auth 欄位是 NULL，不倒填虛構身分。

Verifier registry 的兩個 action 是 `verifier-register` 與 `verifier-revoke`，subject 都是
verifier 的 display name。Register 的成功 detail 只有 `verifier_id`、`kind`、`failure_domain`
與 separation rule；被拒的 intent 也留一筆 `failed` row 並帶 rejection code。
Revoke 的成功 detail 帶 verifier identity、`revision=n→n+1`、受影響證據列數、失去唯一
producer 的工作單數，以及 registry 列與證據都保留這兩個固定事實，不帶被保留證據的內容。
兩個 action 都沒有 credential 明文——credential 從來沒有離開 fresh response 進到任何一張表。

## 6. 上線順序與實測

1. 先存 Tailscale grant。
2. 從另一台已授權 tailnet 裝置確認 TCP 8787 可達。
3. 加入 `CLAWCTL_OPERATOR_CAPABILITY_PREFIX`。
4. 先跑 `./ops/stage-hub-unit.sh`。它只原子更新 checked-in user unit 並
   `daemon-reload`，驗證 PID、numeric restart count 與 systemd `InvocationID` 都未變；不
   stop/start/restart。生命週期鎖在開啟前也會把已確認 owner/canonical 的 Hub 私有 state
   directory 收斂為 `0700`，且不會 truncate 既有 lock。正式 unit 會清掉
   manager ambient 的 auth variables，再讓同一份 `hub.env` override，並以 explicit `--db`
   把 live ledger 釘在 upgrade script 要 snapshot／restore 的路徑。
5. 用正式 `./ops/upgrade-hub.sh` 換版。它不自己解析 `hub.env`，而是讓 systemd transient
   service 以同一份 `EnvironmentFile` 與正式 sandbox 執行 candidate。Candidate 用同一套
   HTTP success contract 與 LocalAPI coherent `CapMap`，驗證 Hub 自己對本機 trusted
   destination 同時取得三把 exact capability。失敗會在停止 live Hub、開 DB 或建立
   snapshot 之前退出；通過後再重驗 on-disk/loaded unit snapshot，才進
   snapshot／rollback／health／agent recheck。Self-probe 成功後、停止 Hub 前，它會另把已證實
   的 origin 寫入 `operator.json`；這不會把 app capability 或任何 bearer 複製進 CLI。
   Script 從起點到結束持有 lifecycle（upgrade）lock；停乾淨後另持有 writer lock 才可碰
   ledger，啟動 Hub 前釋放 writer lock，讓 Hub 自己取得 process-lifetime lock。Maintenance
   marker 跨過這段 handoff，直到 candidate 的六道驗收完成前都封住 HTTP mutation；明示 direct-DB
   break-glass 遇到 marker 則直接拒絕，不把它當作進場許可。
6. 從已授權裝置確認首頁 top bar 顯示 login、device 與 `view,operate,admin`。
7. 用「有 TCP 8787 reachability、但缺 required app capability」的 test identity 做 403
   negative test；若正式 grant 同時擋 TCP，暫時建立只給 port、不給 app key 的測試 grant，
   驗完即移除。再測 cross-site write 不改 state。

`/healthz` 屬 machine/public plane，單看 readiness 仍可能是綠的，但 operator UI 會
全部 403；所以順序固定是先 policy、後 binary，不能把管理者鎖在控制台外。

**2026-09-07 15:45Z 實測：** visual editor 已保存 §3 的 exact grant；candidate 的
LocalAPI self-probe 一次取得 `view,operate,admin`，source／destination 都是
`100.64.200.2`。正式 `upgrade-hub.sh` 將 Hub `864c372`→`7c3c0bb`，先建立
`clawctl-20260907T154537Z-before-7c3c0bb.sqlite` 一致 snapshot，再通過五道驗收。
上線後 unit `active/running`、`NRestarts=0`，只監聽 `100.64.200.2:8787`；正確
authority 首頁回 200，偽造 `Host: 127.0.0.1:8787` 回 421。Dashboard、Machines、
Apps、Deployments、Updates、Tickets、Audit 七個頁面都回 200，Prometheus 官方 parser
讀過 39 個指標／175 行 sample。四台受管 agent 在 20～44 秒內重新報到；sampleagent1 已被
名冊宣告與監看，但刻意未裝 agent、未 enroll 且禁止自動推送，不是本次換版回歸。

**2026-09-07 19:56～20:03Z 實測：** 正式 `upgrade-hub.sh` 將 Hub
`7c3c0bb`→`bf39c04`，建立 `clawctl-20260907T195630Z-before-bf39c04.sqlite`
獨立 snapshot（`0600`、73,732,096 bytes、SHA-256
`2a217f2de7bd12a54fb265bc4e33a328b01a16c3acd3eb744b08a5413e248586`）。Candidate 在
exact stopped-unit／recursive-cgroup、ledger path/schema 與 quiescence 通過後才建 snapshot；
上線後六道 gate 全過，maintenance marker 才 durable unlink。最後一次受控
maintenance drill 後，unit 為 `active/running`、PID `1441191`、`NRestarts=0`，只有
`100.64.200.2:8787` listener，writer lock 實測 contended；首頁回 200 且顯示
`operator@example.com` 與 `view,operate,admin`。`operator.json` 是 owner 的 `0600`、parent `0700`，
沒有 bearer。

Installed CLI 未帶 `--hub-url` 或 `--db` 即經 discovery 對 samplehub1 做 `canary`→`canary`，
revision 維持 0；同 key replay 明示「未重做」，audit 記錄 `operator-api`、
`operator@example.com`、exact admin capability 與 official client UA。明示空的 `CLAWCTL_HUB_URL`
在 network 前 fail closed，audit 0 筆且 state 不變；清空 `NO_PROXY` 並同時設定無法連線的
`HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY` 時仍成功，證明 ambient proxy 沒有成為第二 authority。
Hub active 時明示 `--db` 被 writer-lock contention 在 SQLite open 前拒絕；只有在
`inactive/dead/MainPID=0` 才成功，audit 明標 `direct-db-cli` 且不虛構 HTTP principal。
官方 Prometheus parser 讀過 39 個指標／175 samples；四台已安裝 agent 都在最後
Hub start 後 100 秒內重新 check-in，rollout 後 journal 無 error pattern 且無 warning-or-higher。
實作前的 shell 222/222、全套 Go tests、`go vet`、race detector 與 static build 全過；
兩份獨立對抗審查均無可重現 P0/P1。本次是首次從不理解新 handoff protocol 的舊版
升級，因此 `.prev` 不支援新的手動 `--rollback`；本次 forward 當下的 automatic
rollback 已有獨立復原路徑，之後由 `bf39c04` 開始的 rollout 才同時支援新手動協定。

**2026-09-07 21:54～21:58Z 實測：** `upgrade-hub.sh` 將 `bf39c04`→`88fb5ff`，建立
`clawctl-20260907T215445Z-before-88fb5ff.sqlite`（`0600`、75,075,584 bytes、SHA-256
`632c65fe85d750a072e075eabb27c602be3f9ee17305fa37e9e0b673fb784f5b`）。六道 gate 全過；Hub
`active/running`、PID 1672651、`NRestarts=0`、單一 tailnet listener 且持有 writer lock。官方 parser
讀過 39 個指標／175 samples，四台 agent 心跳均新鮮，rollout 後 journal 無 warning-or-higher。

預設 discovery 的 live enroll 驗收證明 preview 不寫 state；fresh stdout 恰一個 32-byte base64url
secret，create stderr／replay／audit 均無明文；同 key/body replay 非零結束且 stdout 空白，同 key 改 reason 回 409。
撤票回 303，之後以原 token 兌換回 403 `ENROLL_TOKEN_INVALID`；測試 machine 再退役，active 分母回到
5，永久 audit 含 fresh/replay/revoke/retire 且無明文。確認撤銷後已刪除 tmpfs secret。

## Multiple admin accounts

Sign in using a username or optional email; both are trimmed and case-insensitive.
Emails must be plain ASCII addresses (no display name).
All local accounts currently have the admin role. Open `/account/users` from
Account security to create, enable, disable, rename users, or change/clear email.
Email and username must be unique. Every mutation requires your current password
and an unused current authenticator code (recovery codes are not accepted). When
MFA enforcement is explicitly disabled and you have no enrolled factor, password
alone is accepted. You cannot disable yourself or the last active admin.
Disabling a user revokes all their sessions. Renaming preserves sessions.
New users have no MFA and, with enforcement enabled (default), their first login
opens `/account/security` for forced enrollment; other routes stay blocked until
confirmation. Recovery codes appear once after confirmation.

With the Hub stopped, host maintenance commands require an existing database:

```sh
clawctl-hub add-admin --db PATH --username alice --email alice@example.com < password-file
clawctl-hub set-email --db PATH --username alice --email alice@example.com
clawctl-hub set-email --db PATH --username alice --email ""
clawctl-hub rename-user --db PATH --username alice --new-username carol
clawctl-hub disable-user --db PATH --username bob
clawctl-hub enable-user --db PATH --username bob
clawctl-hub reset-admin-password --db PATH --username alice < password-file
```

`add-admin` reads a 12–256 byte password from stdin, like `bootstrap-admin`, and
works with existing accounts. `reset-admin-password` selects the normalized
username; optional `--disable-mfa` forces enrollment again. Host disable also
protects the last active admin. User mutations are audit logged without passwords.
