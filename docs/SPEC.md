# SPEC — AI-Intune / clawctl

> 這份文件回答「技術上怎麼做」。
> 為什麼要做在 [PRODUCT.md](PRODUCT.md)。時程與尚未有答案的問題記在私人工作筆記，不在這個公開倉庫。
>
> **文件優先序：** 實機量測、辯論結論與三份模型提案是私人工作筆記，不在這個公開倉庫。本文件是這裡公開的規格。有衝突時，那些未公開的量測筆記高於本文件。量測推翻過辯論的數個核心假設，那些地方本文件已改寫。

---

## 0. 讀這份 spec 之前要知道的七件事

這七條是整份設計的約束來源，不是風格偏好。每一條後面都有一串設計是為了它而存在的。

1. **痛點是「我不知道」，不是「我按不下去」。** 觀測先做，控制晚一個月無所謂 —— 而且觀測那一半弄壞不了東西。
2. **自證不算數。** 能自己說自己很好的東西，它的話不能當證據。
3. **OpenClaw 的 `status='ok'` 不等於任務成功。** 實測 64% 的 `ok` 在描述失敗（PHASE0 §1.2，私人工作筆記，不在這個公開倉庫）。所以健康分成 L1 存活與 L2 成果兩層，而且 L2 第一版不自動化。
4. **Agent 跑在使用者身分下，不是 root。** 所有 CLI 憑證都是 `0600` 屬於那個使用者，而且**每一台的 user 都不一樣**（`example-user` / `ubuntu` / `opc` / `example-user-b`）。
5. **分母是 5 台 Linux + 4 台 Windows，不是 30 台。** 「~30」對應的是 agent 數量。這個數字小到讓很多分散式設計變成過度工程 —— 見 §0.1。
6. **不是從零開始。** 已經有 inventory SSOT（`bat-servers.ps1`）與可用的健康監看（`bat-connect.ps1`）。本專案補的是**持久化、歷史、跨機器關聯，以及一支住在 Linux 上的 agent**。
7. **複雜度上限 = 半夜一個人修得動。** 這是硬性的架構約束，不是口號。任何讓維護者不敢動它的設計都是錯的，不管它多正確。

### 0.1 分母只有 5 台，這件事改變了什麼

| 原本的設計 | 5 台的現實 |
|---|---|
| `batch_size=5` 分批推送 | **新 deployment 第一批就是 1 台。**省略 `batch_size` 時後續也是 1。明示 2–5 只決定後面每一批。canary 被 Hub 判 succeeded 之後暫停，要明示 Continue 才開下一批 |
| 十張票的調度器 | 5 台機器、每台同時一個 agent → 尖峰同時占用最多 5。**10 張票配 5 台，搶票問題大概率不存在**。閘門幾乎確定不會通過，這是好消息 |
| PostgreSQL、分散式鎖、佇列 | 全部是過度工程。SQLite 單檔 + 一個 process |
| 「30 台裡有 3 台失敗」 | 「5 台裡有 1 台失敗」—— 但那 1 台的名字你必須立刻看得到 |

**分母小不代表問題小。** 量測當下，4 台遠端機器的 Claude 登入**全部過期**（137 天 / 68 天 / 13 天 / 剛過期），sampleagent1 磁碟 97% 且它自己的 agent 已經連續喊了三次以上 URGENT 而沒有人發現。**問題不在規模，在能見度。**

---

## 1. 產品定義

見 [PRODUCT.md](PRODUCT.md)。技術面只需要記住這條共用管線 —— **左邊選單每一項都是它，沒有第二條**：

```
指派期望狀態 → 產生工作單 → 分批執行 → 獨立驗證 → 回報結果 → 處理例外
```

`Update` 只是把群組的目標版本改掉。這一句讓後端少寫一半。

控制面的共同契約見 [CONTROL-PLANE-CONTRACT.md](CONTROL-PLANE-CONTRACT.md)：**一個 operation 只有一個 executor，但必須有執行端與 endpoint 端兩個不同 failure domain 的證據**。目標正常路徑是 Web 與 CLI 共用同一個 operator application service / API，不能各自直寫資料庫或另算成功；尚未遷完的能力逐項列在 [API-SURFACE.md](API-SURFACE.md)，不把過渡狀態寫成完成。

---

## 2. 架構

### 2.1 三個角色，不互相兼差

| 角色 | 誰 | 職責 | 明確不做 |
|---|---|---|---|
| **讀與解釋** | AI agent（人在迴圈裡） | 看證據、寫人話、給建議 | 不決定紅綠燈、不排工作單 |
| **動手** | `clawctl-agent`（Go static binary） | 觀測、執行工作單、跑驗證 | **不判斷自己健不健康** |
| **判斷生死** | Hub | 收證據、算狀態、發工作單 | 不碰資料面 |

> Agent 回報**事實**（時間、exit code、檔案內容、process 是否存在）。Hub 產生**判決**（Online / Degraded / Unreachable）。
> Agent 永遠不准寫 `status='Online'` —— 那就是自證。

### 2.2 資料流

```
                      ┌──────────────────────────────────┐
   Browser ─────────▶ │  Hub                             │
   clawctl CLI        │   名冊（分母）                   │──▶ Secret store
                      │   desired state · revision       │    （只存 secret_ref）
   Telegram ◀──────── │   工作單 · 證據 · 判決           │
        ▲             │   每日推播                       │
        │             └────────────────▲─────────────────┘
        │                              │
   healthchecks.io                     │  ⚠ 只有 Agent 主動向外連線
   （外部死人之鐘，                    │     HTTP over Tailscale/WireGuard
     不是 Hub 自己）                   │     機器上不開任何 inbound 控制 port
                      ┌────────────────┴─────────────────┐
                      │  Linux machine                   │
                      │   clawctl-agent (system unit,   │
                      │     explicit User=)             │
                      │    ├── adapters（唯讀優先）      │
                      │    └── watchdog                  │
                      │   OpenClaw + gateway             │
                      │   claude / codex / grok / agy    │
                      │   bat-server ── tailnet ── BAT   │
                      └──────────────────────────────────┘
```

**三條鐵律：**

- **Hub 掛掉，機器上正在跑的工作不受影響。** Agent 是 pull-based；Hub 回來之後續拉同一個 revision。
- **機器上不開任何 inbound 控制 port。** 沒有「Hub 連進去」這種東西。
- **Hub 不在資料面上。** 這條在 BAT 那裡有一個已知的張力，見 §7.3，且第一版選擇不做。

### 2.3 Hub 放哪裡

**Hub 不可以放在被它管的機器上。** 這是預設會犯的錯 —— 大家都會裝在最熟的那台 GPU 機，然後那台死掉時同時失去服務與知道服務死掉的能力。

- Hub 的 hostname 顯示在 Settings
- 若 Hub 的 hostname 出現在未退役名冊 → **Dashboard 常駐警告**
- 死人之鐘（healthchecks.io 或等價物）必須在 Hub 之外

### 2.4 Agent 跑在哪個身分（實測決定，不是偏好）

**`systemd --user` unit，跟 OpenClaw 同一個 user。**

原因（PHASE0 §2.6，私人工作筆記，不在這個公開倉庫）：所有 CLI 憑證都是 `0600` 且屬於該使用者。root daemon 讀得到（root 讀什麼都行），但那是不必要的權限擴張，而且判斷「現在是哪個帳號在用」本來就需要使用者的 session 脈絡。

⚠️ **必須 `loginctl enable-linger <user>`**，否則使用者登出時 unit 會被停掉，然後 Hub 會顯示一整排 Unreachable 而機器其實好好的。這條要寫在 bootstrap 的第一步，不是註腳。

### 2.5 systemd unit

```ini
[Unit]
Description=clawctl agent
After=network-online.target

[Service]
Type=notify
ExecStart=/home/%u/.local/share/clawctl/current/clawctl-agent
WatchdogSec=90
Restart=always
RestartSec=5

# ⚠ 沒有這行，installing 途中被 kill 會留下孤兒 wget/tar 佔著檔案
KillMode=control-group

NoNewPrivileges=true
PrivateTmp=false          # 要讀 /tmp/openclaw/ 的輔助線索
ProtectSystem=strict
ReadWritePaths=%h/.local/share/clawctl %h/.local/state/clawctl

[Install]
WantedBy=default.target
```

`WatchdogSec` **只認心跳 goroutine**。心跳與工作執行分成兩條 goroutine：工作那條 deadlock 時，心跳仍然要能報「我卡在 job X 的 phase Y 已經 N 秒」，而不是整個 unit 被 watchdog 打死然後看起來像正常重啟。

### 2.6 為什麼 Agent 是 Go static binary

**它去升級一個 Node 寫的 CLI 時，不能把自己一起搞死。**

- 不依賴 Node、Python、OpenClaw，或任何它管理的 CLI
- **內建 SQLite 讀取，不 shell out 到 `sqlite3`** —— 實測 samplehub1 上根本沒裝 `sqlite3`（PHASE0 §1.5，私人工作筆記，不在這個公開倉庫）
- 自升級一年兩次，用 `install.sh` 或 Ansible，**不走工作單** —— 站在梯子上搬梯子

---

## 3. 健康模型（本專案唯一真正原創的部分）

### 3.1 為什麼不能只有一盞燈

實測結果：OpenClaw 的成功訊號存在，但**它的語意是「agent 的回合正常結束並產出文字」，不是「任務目標達成」**。

- 最近 400 筆 `status='ok'` 裡 **257 筆（64%）的 summary 在描述失敗**
- `sampleagent4-heartbeat` **連續 12 次以上成功地回報自己失敗**，而 `consecutive_errors` 一直是 0

**這就是地基二預言的假綠燈，而且它已經在生產環境裡了。** 所以：

### 3.2 兩層

| 層 | 問題 | 來源 | 第一版 |
|---|---|---|---|
| **L1 存活** | 排程還在轉嗎？ | `cron_run_logs.ts` 最大值、`task_runs.ended_at` | ✅ 自動判定，可靠 |
| **L2 成果** | 它做的事情對嗎？ | **沒有結構化欄位** | ❌ 攤原文給人看 |

**UI 上的硬規定：**

- L1 綠燈的文案是 **「最近有跑完」**，不准寫「健康」、不准寫「正常」
- L2 未接通時，機器詳細頁**常駐一行**：`成果判定未接通 — 此處綠燈只代表跑完了，不代表做對了`
- **絕對不准解析 summary 文字來推斷成敗。** 那是「掃 log 關鍵字」的變形，地基二否決過

> 未來自動化 L2 的唯一合法路徑是**由管理端定義的獨立驗證指令**（§6），不是文字探勘。

### 3.3 五層成功，永遠不壓成一個布林

| 層 | 意思 | 綠的條件 |
|---|---|---|
| `auth` | 登入還在 | 真實請求成功，或獨立 verify 通過。**憑證檔沒過期只能是黃燈** |
| `provider` | 上游服務通 | 最近一次真實呼叫沒有 5xx / 429 |
| `tool` | CLI 跑得動 | `discover` 回得出 binary path + 版本 |
| `workflow` | 任務跑完 | L1 有合格完成事件 |
| `quality` | 做對了嗎 | **永遠 `unknown`**。這一版不假裝判得出來 |

### 3.4 機器狀態（衍生值，Hub 算，Agent 不准寫）

```
NeverReported       未退役且從未成功 check-in
Online              最近一次 received_at 在門檻內
Degraded            在線，但工作負載 / 憑證 / crash-loop / 碟不足其中之一
Unreachable         曾經在線，超過「下次應到 + 90s」沒收到
Identity conflict    憑證或指紋衝突（兩列都保留，不合併）
Hub unhealthy       外部死人之鐘沒響（這一列不是 Hub 自己寫的）
```

### 3.5 時間門檻

| 項目 | 值 | 為什麼是這個數 |
|---|---|---|
| 心跳間隔 | **2 分鐘** ±20% jitter | jitter 防止 30 台在整分鐘同時撞 Hub |
| Unreachable 門檻 | **下次應到 + 90s** | ⚠️ 心跳間隔 = 失聯門檻會製造假紅，第一週就會讓人關掉通知 |
| observed state 上報 | 10 分鐘 | 比心跳慢；心跳失敗不等於狀態過期 |
| 本機狀態檔過期 | mtime > 10 分鐘 | 顯示「Agent 在線，狀態檔過期 N 分鐘」 |
| silent failure | 8 小時無合格完成事件 | |
| job Stuck | 非終態且 `now - last_event.received_at > 15 分鐘` | |
| job 執行逾時 | 預設 15 分鐘 | |
| 時鐘不可信 | `abs(sent_at - received_at) > 120s` | |

**生死一律用 Hub 的 `received_at` 算。** 機器時鐘快兩天，`last_success` 會看起來像一分鐘前。

### 3.6 OpenClaw 觀測的實作限制（照做，這些都踩過）

| 限制 | 做法 |
|---|---|
| **資料庫有兩種 layout，同時活在機隊上** | `2026.6.x` → `state/openclaw.sqlite`（`cron_jobs`+`cron_run_logs`+`task_runs`）；`2026.5.x` → `tasks/runs.sqlite`（只有 `task_runs`）。**兩個路徑都要試**。欄位相同，所以 L1 兩種都拿得到 |
| WAL 有 5MB 未 checkpoint | 用 `file:...?mode=ro`，**不可用 `immutable=1`**，會讀到過期資料 |
| 沒有 `sqlite3` CLI | Go 內建讀取 |
| `terminal_outcome` 全機隊都是 NULL | 上游沒在寫。**L2 沒有結構化來源這件事在每一台都成立** |
| `cron_jobs.last_run_at_ms` 是**開始**時間 | 完成時間只能用 `cron_run_logs.ts` / `task_runs.ended_at` |
| `task_runs` 只留 7 天、`cron_run_logs` 每 job 2000 筆環形 | **Hub 是唯一的歷史**；輪詢必須快過最兇的 job 填滿 2000 筆 |
| log 在 `/tmp/openclaw/`（tmpfs） | 重開機就沒了。只當輔助線索，不當健康判準也不當歷史 |
| `systemctl is-active` 因 `Restart=always` 恆為 active | 用 `systemctl show -p ActiveEnterTimestamp` + 數 `~/.openclaw/logs/stability/*.json` |
| webhook 只在 `summary` 非空時才發 | **失敗是靜音的。** pull 是唯一真實來源，webhook 只是延遲優化 |

### 3.7 版本：一台機器上有三個座標

實測同一台：CLI `2026.6.1` / gateway `2026.6.6` / upstream `2026.8.2`，而 `openclaw --version` **只報 CLI**，前面還有多行不一致警告。

所以：

- 三個座標**分開存**，UI 分開顯示 —— CLI 舊了跟 gateway 舊了的修法不一樣
- **`--version` 全部不可信**（grok 說 1.0.3 實際 1.0.13；`bat-server` 根本沒有 `--version`）
- 多來源（package.json / version.json / binary hash）交叉比對，**矛盾時如實顯示矛盾，不要挑一個當答案**
- `discover` 必須回 `binary_path` + `pid` + `/proc/<pid>/exe` 的 realpath。三者不一致 → `Degraded`，文案寫「**裝的是 2.2.0，跑的是 2.1.0**」

---

## 4. 資料模型

**Phase 1 就把最終形狀 migrate 出來。空表很便宜，重寫很貴。** Phase 1 的程式只寫名冊、check-in、observed、憑證中繼、票的占用、通知；`desired_*` / `jobs` 留空。

型別以 SQLite 為準（`TEXT` 存 UUID / RFC3339，JSON 存 TEXT）。**30 台第一年不跑 PostgreSQL** —— 那是另一個要備份、要升級、會單獨死的 process。Repository 介面留著，切換條件是「測到 WAL 寫入 p99 > 100ms 連續一週」，不是「比較正式」。

### 4.1 名冊 —— 分母

```sql
CREATE TABLE machine_registry (
  machine_id     TEXT PRIMARY KEY,
  hostname       TEXT NOT NULL,
  expected       INTEGER NOT NULL DEFAULT 1,  -- 舊欄位；分母不讀它
  tailscale_ip   TEXT,
  os_release     TEXT,
  unix_user      TEXT NOT NULL,               -- agent 跑在誰身上
  enrolled_at    TEXT,
  retired_at     TEXT,
  notes          TEXT
);
```

> **名冊有 30 台、只有 27 台報到，那 3 台亮紅燈，不是從畫面上消失。**
> 這是整個資料模型最重要的一句話。`retired_at IS NULL AND 從未 check-in` = `NeverReported`，佔一格紅。

### 4.2 觀測 —— 只 append，永不覆蓋

```sql
CREATE TABLE machine_checkins (
  machine_id           TEXT NOT NULL,
  sent_at              TEXT NOT NULL,
  received_at          TEXT NOT NULL,   -- 生死只用這個
  agent_version        TEXT,
  boot_id              TEXT,            -- crash-loop 偵測
  agent_seq            INTEGER,         -- 單調遞增
  disk_free_bytes      INTEGER,
  current_job_id       TEXT,
  current_job_phase    TEXT,
  stalled_seconds      INTEGER,
  max_seen_revision    INTEGER,
  max_applied_revision INTEGER,
  PRIMARY KEY (machine_id, sent_at)
);

CREATE TABLE observed_state (
  observation_id TEXT PRIMARY KEY,
  machine_id     TEXT NOT NULL,
  measured_at    TEXT NOT NULL,
  received_at    TEXT NOT NULL,
  kind           TEXT NOT NULL,   -- openclaw / cli_tool / systemd / disk / credential
  subject        TEXT NOT NULL,   -- 'openclaw' / 'claude' / ...
  payload        TEXT NOT NULL,   -- JSON
  source         TEXT NOT NULL    -- agent_measurement / task_event
);
CREATE INDEX ix_observed_recent ON observed_state (machine_id, subject, measured_at DESC);
```

⚠️ **`observed_state` 不可以用 `machine_id` 當 PK 做成一台一列的快照。** Gemini 提案是那樣寫的，實測否決：上游 7 天 / 2000 筆就把歷史吃掉，Hub 是唯一的歷史。而且「今天是綠的，星期三就死了」只有 append-only 答得出來。

Dashboard 預設顯示**相對昨天的變化**，不是一排當下的紅綠燈 —— 這是這張表 append-only 才做得到的事。

### 4.3 憑證 —— 五個詞，不是一個布林

```sql
CREATE TABLE credential_profile (       -- 票的身分
  profile_id          TEXT PRIMARY KEY,
  profile_name        TEXT UNIQUE NOT NULL,   -- 'claude-02' / 'grok-01'
  provider            TEXT NOT NULL,
  kind                TEXT NOT NULL,          -- oauth | api_key（寫入後不可改）
  account_label       TEXT,
  secret_ref          TEXT,                   -- ⚠ 僅 api_key。oauth 必須 NULL
  soft_cap_machines   INTEGER,                -- 未測留 NULL，UI 寫「未測」
  planned_rotation_at TEXT,
  revoked_at          TEXT,
  created_at          TEXT NOT NULL
);

CREATE TABLE credential_on_machine (    -- 該機看得到的狀態
  machine_id          TEXT NOT NULL,
  profile_id          TEXT NOT NULL,
  status              TEXT NOT NULL,    -- configured|expires_soon|expired|unknown|failed
  reported_expiry_at  TEXT,
  last_refresh_at     TEXT,             -- 三元組之一
  file_mtime          TEXT,             -- 三元組之一
  verified_at         TEXT,
  verification_method TEXT,
  last_real_use_at    TEXT,
  last_error_category TEXT,
  last_error          TEXT,
  PRIMARY KEY (machine_id, profile_id)
);
```

**`configured` 不是 Auth OK。** 它只代表「檔案裡的過期時間還沒到」。讀不到 expiry 是 `unknown`，**不是** `configured` 當綠。

**`expires_soon` 沒有生產者 —— 這是 Phase 1 實測改掉的，不是漏做。** 這份 spec 原本寫「剩餘 ≤ 3 天就 `expires_soon`」，程式一接上真機就發現這條規則會讓**全機隊永遠亮黃燈**：claude 的 access token 名目壽命 8 小時、背景自動續期，量到的剩餘是 2.3 小時 —— 任何「剩餘 < N 天」的門檻對它永遠成立。而永遠亮的黃燈幾天內就會被靜音，靜音之後這個產品就沒有存在意義了（這正是 Grok 在辯論中點名的失效模式 D1）。

所以 Agent 只回報 `expired` / `configured` / `unknown`，判決層也不從 `expires_at` 自己算任何東西。**真正該偵測的訊號不是「快到期」，是「這張票不再自動續了」**—— 那要看 `last_refresh` 有沒有卡住，是跨時間、跨機器的比對，不是一個常數門檻。那條規則屬於 Phase 2 的歷史分析，而三元組 `{expires_at, last_refresh, file_mtime}` 存在的理由就是這個。

> ✅ **2026-09-06 做了，而且它修的是「過期」那一側的鏡像問題。**
> 實測 `observed_state` 三天的歷史：samplehub1 的 claude 續了 8 次、每次都在到期前幾分鐘，
> 也就是**用到才續**。於是機器閒置 9 小時，這張 8 小時的票就「過期」，然後下次用到自己好。
> 2026-09-05 的告警紀錄：`CredentialExpired` 在 samplehub1/claude、samplehub1/grok、sampleagent2/claude
> 上一天各燒兩輪、每輪一小時、自己熄掉 —— 跟被刪掉的 `expires_soon` 是同一種永遠會被靜音的燈。
>
> 現在的規則（`state.CredGrace`，判決、早報、Prometheus 規則三處同一條）：
> **過期不到一個壽命 = 還沒被用到，看得見但不降級；過了一個壽命還沒續 = 續不回來，降級、要人。**
> 壽命 = `expires_at − file_mtime`（claude 8h、grok 6h、codex 10d），寬限上限一天；
> 算不出壽命的票沒有寬限。Hub 從 append-only 的歷史數出「看了多久、它自己續了幾次」，
> 這兩個數字一起出現在判決句、詳細頁與 `/metrics`
> （`clawctl_credential_lifetime_seconds` / `_refreshes_seen` / `_watched_seconds`）。
> 上線當天的效果：四台 `Degraded` 變成三台，剩下的三張過期票（4 天／16 天／70 天）全部是真的要重新登入。
> 跨機器那一半 **2026-09-06 做了 provider 層級的**：一張過了寬限還沒續的票，判決句會補上
> 「同一家的登入這段時間在 samplehub1 上自己續了 8 次（最近 2 小時前）」，用來排除「那家整個掛了」，
> 把矛頭指回這一台的 session。⚠ 票的名冊（`credential_profile` / `ticket_static_assignment`）還是空的，
> 所以 Hub **分不出兩台用的是不是同一張票**，文案必須說「同一家」、不准說「同一張」，
> 而且要明講「票的名冊還沒建」。同儕證據**只補解釋、不點燈**：寬限內的票不因它升級，configured 完全不碰。
> 上線當天：sampleagent2/grok、sampleagent3/claude、sampleagent4/claude 三句都指向 samplehub1 的 8 次續期。

**「被踢掉」偵測不到 —— 用三元組繞過去。** 本機檔案裡沒有任何欄位能表達「session 被伺服器端撤銷了」。所以 Agent 回報 `{reported_expiry_at, last_refresh_at, file_mtime}`，**由 Hub 做跨機器比對**：同一張票在 A 上一直在更新、在 B 上 mtime 卡住三天 → B 大概被踢了。判斷在 Hub，不在 Agent。

### 4.4 十張票的帳本 —— 第一版只收帳，不算調度

這是本平台唯一沒有現成東西可抄的部分。**租約引擎可以沒有，帳本不能沒有** —— 沒有帳本，Phase 5 的輪替只會變成口頭故事。

```sql
CREATE TABLE ticket_static_assignment (   -- operator 的意圖
  profile_id  TEXT NOT NULL,
  machine_id  TEXT NOT NULL,
  assigned_at TEXT NOT NULL,
  assigned_by TEXT NOT NULL,
  desired     INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (profile_id, machine_id)
);

CREATE TABLE ticket_occupancy_observation (  -- 占用帳本，只 append
  observation_id      TEXT PRIMARY KEY,
  machine_id          TEXT NOT NULL,
  profile_id          TEXT NOT NULL,
  provider            TEXT NOT NULL,
  kind                TEXT NOT NULL,
  measured_at         TEXT NOT NULL,
  received_at         TEXT NOT NULL,
  occupant_evidence   TEXT NOT NULL,   -- ⚠ 沒證據不准寫 profile_id
  process_alive       INTEGER NOT NULL,
  last_success_at     TEXT,
  last_success_task_id TEXT,
  last_error_category TEXT,            -- rate_limit|auth|timeout|provider|network|none
  last_error_at       TEXT,
  source              TEXT NOT NULL
);

CREATE TABLE credential_ledger_event (    -- 生命週期，只 append
  event_id    TEXT PRIMARY KEY,
  profile_id  TEXT NOT NULL,
  machine_id  TEXT,
  event_type  TEXT NOT NULL,  -- assigned|unassigned|deployed|revoked|observed_in_use
                              -- |success|rate_limited|auth_failed|machine_offline
                              -- |released_by_human
  occurred_at TEXT NOT NULL,
  received_at TEXT NOT NULL,
  evidence_ref TEXT,
  actor       TEXT NOT NULL   -- agent|operator|hub
);
```

> ⚠️ **上面的欄位定義被真機推翻過一次。實作以 `internal/store/schema.sql` 為準。**
>
> 這一節原本說「以 `task_event` 為優先」。實測**上游根本沒有 `task_event` 這張表**，
> 五台機器上都沒有。真正拿得到的占用證據是 cron 的執行紀錄，而且兩種 layout
> 放在不同地方：
>
> | layout | 位置 | 實測 provider 填充率 |
> |---|---|---|
> | consolidated (2026.6.x) | `cron_run_logs` 表 | samplehub1 3315/4035 |
> | split (2026.5.x) | `~/.openclaw/cron/runs/*.jsonl` | sampleagent2 2793/2942 |
>
> 另外兩個欄位也改了，完整理由寫在 `schema.sql` 的註解裡：
>
> - **`last_error_category` 改成 `last_error_text`。** 把自由文字對到
>   `rate_limit|auth|timeout|…` 就是「掃 log 關鍵字」的變形，地基二否決過。
>   而且實測最常見的一句是 `cron: job interrupted by gateway restart`，
>   它不屬於上面列的任何一類 —— 硬分類只會分錯。
> - **`profile_id` 目前一律 NULL，而那是對的。** 我們有證據證明「用了 openai
>   這一家」，沒有證據證明「用了哪一張票」。硬填就是違反下面那條硬規則。
>
> ⚠️ **provider 一律存上游原文，不正規化。** 實測 samplehub1 寫 `openai`、
> sampleagent2 寫 `openai-codex`，那是同一條 codex 訂閱在不同 OpenClaw 版本下的
> 兩個名字。自動併起來會蓋掉真實差異；自動拆開會看起來像一次從來沒發生過的換票。

**兩條規則：**

- `ticket_occupancy_observation` **只寫真的跑完的回合**，帶 provider、時間、
  session_key、錯誤原文。沒有 provider 的回合不入帳，但**要數進去並顯示出來** ——
  實測缺 provider 的多半正好是失敗的回合，也就是最想知道「當時用的是哪張票」的那些。
  一本安靜地丟掉三成資料的帳，看起來會跟一本完整的帳一模一樣。
  **不准用 `process_alive` 假裝占用。**
- 機器 Unreachable **不自動釋放** oauth 占用（session 還在那顆碟上），也不把 api_key 標成已歸還。帳本寫 `machine_offline`，要釋放由人按 `released_by_human`。**離線就當還鑰匙，是在製造帳上有票、世上沒票的洞。**

⚠️ **`~/.codex/auth.json` 會被 BAT 熱抽換**，所以判斷「現在哪張票在用」必須同時讀 `~/.codex/codex-accounts.json` 的 `activeAccountId`，否則會答錯。

### 4.5 期望狀態與工作單

```sql
CREATE TABLE desired_state (
  desired_id    TEXT PRIMARY KEY,
  scope_type    TEXT NOT NULL,   -- machine | channel（canary/stable）⚠ 不准任意 tag
  scope_id      TEXT NOT NULL,
  resource_kind TEXT NOT NULL,
  resource_id   TEXT NOT NULL,
  revision      INTEGER NOT NULL,
  spec          TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  created_by    TEXT NOT NULL
);

CREATE TABLE revision_counters (
  resource_scope TEXT PRIMARY KEY,
  current_revision INTEGER NOT NULL
);

CREATE TABLE jobs (
  job_id            TEXT PRIMARY KEY,   -- 由 Hub 產生，冪等鍵
  machine_id        TEXT NOT NULL,
  desired_id        TEXT NOT NULL,
  revision          INTEGER NOT NULL,
  state             TEXT NOT NULL,
  lease_token       TEXT,               -- fencing token
  lease_expires_at  TEXT,
  execution_timeout INTEGER NOT NULL DEFAULT 900,
  artifact_digest   TEXT,
  irreversible      INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL,
  terminal_at       TEXT
);

CREATE TABLE job_dependencies (
  job_id              TEXT NOT NULL,
  prerequisite_job_id TEXT NOT NULL,
  position            INTEGER NOT NULL CHECK (position >= 0 AND position < 128),
  PRIMARY KEY (job_id, prerequisite_job_id),
  UNIQUE (job_id, position)
);

CREATE TABLE job_events (
  event_id            TEXT PRIMARY KEY,
  job_id              TEXT NOT NULL,
  seq                 INTEGER NOT NULL,
  phase               TEXT NOT NULL,
  occurred_at         TEXT NOT NULL,
  received_at         TEXT NOT NULL,
  payload             TEXT NOT NULL,
  producer_kind       TEXT NOT NULL,
  producer_id         TEXT NOT NULL,
  evidence_role       TEXT NOT NULL,
  authority           TEXT NOT NULL,
  provenance_recorded INTEGER NOT NULL,
  UNIQUE (job_id, seq)
);

CREATE TABLE verification_results (   -- ⚠ 獨立一張表，不是 job 的欄位
  verification_id TEXT PRIMARY KEY,
  job_id          TEXT NOT NULL,
  machine_id      TEXT NOT NULL,
  rule_id         TEXT NOT NULL,
  command         TEXT NOT NULL,   -- 實際跑的指令
  exit_code       INTEGER,
  stdout_excerpt  TEXT,
  stderr_excerpt  TEXT,
  passed          INTEGER NOT NULL,
  verified_at     TEXT NOT NULL
);
```

`job_dependencies` 是 machine-local、ordered、immutable 的 DAG。Hub 只把所有 prerequisite
都進入 `succeeded` 的工作單交給 agent；資料庫 trigger 同時禁止舊版 Hub 或直接 SQL 將 blocked
child 從 `not_started` 改成 `claimed`。任何 prerequisite 進入失敗終態時，scheduler 在下一次該機器
輪詢中把所有下游工作單收成 `rejected`，並寫入 producer=`hub_scheduler`、
authority=`dependency_graph` 的 `DEPENDENCY_FAILED` 事件。相同 resource 的較低 revision 若仍在
非終態，較高 revision 不可越序；不同 resource 的 revision counter 彼此獨立，從各自可執行的 head
按建立順序選單。

`verification_results` 是獨立的表，因為 executor 與第二個 producer 的證據都必須保留身分與角色，
不能壓成 Agent 自己宣告的一個結果。`jobs.state='succeeded'` 只採 Hub 驗過的 executor-role evidence；
工作單終態後才產生的 fleet-peer actual-state report 另進 stable promotion gate，兩者不互相取代。
未認領（`not_started`）的工作單不得帶有執行證據（executor event 或 executor 驗證列）；獨立驗證列不在此限。

### 4.6 群組畫在爆炸半徑上

`linux` / `gpu` / `client-A` 這種重疊標籤當**分類**沒問題；當 `desired_state` 的 scope 就有問題。所以 `scope_type` 只准 `machine` 或 `channel`。

**`canary` 的定義是「壞掉我賠一個下午」，不是「硬體比較新」。**

---

## 5. API

原則：**Agent 只做 outbound。** 沒有任何一支 API 是 Hub 打進機器的。

### 5.1 Agent → Hub

| Method | Path | 用途 |
|---|---|---|
| `POST` | `/v1/enrollments` | 用一次性 enroll token 換憑證 ⚠ 見下方註 |
| `GET`  | `/v1/capabilities` | Hub 支援哪些 resource kind / adapter 版本 |
| `POST` | `/v1/checkins` | 心跳（2 分鐘）。**UPSERT-only**；同一筆帶 nullable executor capability，舊 agent 的 `device_sync_v1` 維持未回報 |
| `POST` | `/v1/observations:batch` | observed state（10 分鐘）批次 |
| `GET`  | `/v1/jobs/next` | 拉工作單（pull-based） |
| `POST` | `/v1/jobs/{id}/claims` | 取得 lease + fencing token |
| `POST` | `/v1/jobs/{id}/lease:renew` | 續租 |
| `POST` | `/v1/jobs/{id}/events` | 階段事件（帶單調 `seq`） |
| `POST` | `/v1/jobs/{id}/verifications` | 驗證結果 |
| `POST` | `/v1/jobs/{id}/complete` | 收工 |
| `POST` | `/v1/jobs/{id}/reject` | **合法拒絕**，見 §5.2 |
| `POST` | `/v1/jobs/{id}/login-events` | 互動式登入的階段回報 |

⚠ **註（2026-09-06）：實作出貨的是 bearer token，不是這裡寫的 mTLS。**
那是一筆有意識的偏離，Phase 4 開工時重新開過並且**決定不還成 mTLS**，
改成把它要保護的三個性質寫成強制不變量。理由、這三條**擋不住什麼**、
以及重新開這個決定的觸發條件，全部寫在私人工作筆記（PHASE1 §4，不在這個公開倉庫）。
這一列留著原本的文字，因為那才是 spec 想要的東西 —— 偏離要看得見，
不是把 spec 改成現況。

**`/v1/checkins` 必須是 UPSERT-only。** 30 台帶 jitter 打進來，重試會撞；任何非冪等的實作都會在第一次網路抖動時產生重複列。

### 5.2 Agent 可以合法拒絕工作單

**「拒絕」是一等公民，不是錯誤。** 一個只會說 yes 的 agent，遇到不該做的事只能做壞。

| 拒絕碼 | 什麼時候 |
|---|---|
| `STALE_REVISION` | 收到的 revision < 本機同一 `(resource_kind, resource_id)` 的 `max_seen_revision` |
| `DUPLICATE_JOB_ID` | 這個 job_id 已經是終態 → 回放原結果 |
| `ARTIFACT_HASH_MISMATCH` | 下載的 digest 對不上 |
| `LEASE_INVALID` | fencing token 過期或被搶走 |
| `IRREVERSIBLE_MIGRATION` | 標為不可逆，需要人確認 |
| `PRECONDITION_FAILED` | 碟不足、相依缺、CLI 版本不支援 |
| `UNSPECIFIED_MODEL_SWITCH` | 要求換模型但沒有明確指定 |

`DEPENDENCY_FAILED` 只由 Hub scheduler 寫入；agent 的 `/reject` allowlist 不接受這個碼。

### 5.3 revision：兩個水位，不是一個

Agent 的 journal 以 `(resource_kind, resource_id)` 為 key，每個 resource **同時**
保存 `max_seen_revision` 與 `max_applied_revision`。不同 app、runtime 或 policy 的
revision 不會互相擋住。Hub 的領單回應必須同時帶 `resource_kind` 與
`resource_id`，Agent 在執行前以這組 identity 選定水位。

只記 `max_applied` 會有這個洞：同一 resource 在機器離線期間 Hub 發了
41、42、43，上線後亂序送達，Agent 先套 43 再收到 41。如果 43 套用失敗，
只比對 `max_applied` 會讓 41 成功，機器因此**成功地降版而且全綠**。

**要回舊版，必須由 Hub 發一個更高 revision 的 rollback desired state。** 沒有「往回走」這種操作。

### 5.4 Hub → 人

| Path | 用途 |
|---|---|
| `GET /reports/tickets` | §4.4 的週表 |
| `GET /machines/{id}` | 詳細頁（`Connect BAT` 在**最底下**） |
| `POST /deployments/{id}/continue` | **`Continue next batch`，不是 `Retry all`** |

---

## 6. Adapter

### 6.1 介面

```go
type Adapter interface {
    Name() string
    SupportedVersions() semver.Range

    Discover(ctx) (Installed, error)      // binary_path, pid, /proc/<pid>/exe, 三個版本座標
    AuthStatus(ctx) (CredStatus, error)   // 五個詞之一 + 三元組
    Install(ctx, Spec) error
    Configure(ctx, Spec) error
    StartLogin(ctx, Level) (LoginSession, error)
    Verify(ctx, Rule) (VerificationResult, error)
    Rollback(ctx, Snapshot) error         // ⚠ config + data，不只是 binary
}
```

### 6.2 未知版本 → 唯讀

CLI 版本不在 `SupportedVersions()` 內時：**`Discover` 與 `AuthStatus` 繼續跑，所有寫入停用**，該工具標 `unsupported`，但仍回報 binary path 與 raw version 字串。

理由：讀錯了頂多顯示錯；寫錯了會把機器弄壞。而 CLI 改格式的頻率遠高於我們跟得上的頻率。

### 6.3 Adapter 停損

**連續 3 台同一個 Adapter 出現 format error → Hub 把該 Adapter 標成 `broken`，停掉該 CLI 的所有寫入工作單。**

擋的是這個情境：CLI 改版把 JSON 換成 SQLite，Adapter 還在讀那個檔（現在是屍體檔），讀到過期資料而且**看起來完全正常**。三台一致的解析失敗是「上游改格式了」的訊號，不是三次巧合。

### 6.4 互動式登入的四級

| 級 | 需要什麼 | 做法 |
|---|---|---|
| 1 | device code | 全自動，Hub 顯示 code 給人輸入 |
| 2 | stdin/stdout | Agent 代跑，回報階段 |
| 3 | 完整 TTY | Agent 起 PTY |
| 4 | 更複雜 | **退回 BAT，人自己來** |

第一版只做到第 1 級 + 第 4 級。中間兩級等真的需要再說。

### 6.5 憑證離線過期的配方（實測）

| CLI | 檔案 | 欄位 | 有效期 |
|---|---|---|---|
| claude | `~/.claude/.credentials.json` | `.claudeAiOauth.expiresAt`（ms epoch） | — |
| codex | `~/.codex/auth.json` | 解 `.tokens.access_token` 的 JWT `exp` | ~10 天 |
| grok | `~/.grok/auth.json` | `.["https://auth.x.ai::<uuid>"][].expires_at` | ~6 小時 |
| agy | `~/.gemini/oauth_creds.json` | `.token.expiry` | ~1 小時 |
| openclaw | — | **沒有任何過期欄位** | ❌ |

**三個一定會踩的坑：**

1. ⚠️ **codex 千萬不要用 `id_token`。** 它壽命 1 小時、實測現在就已經過期，而系統照常運作。拿它判斷會讓**每一台機器都亮紅燈**。要用 `access_token`。
2. ⚠️ **grok 的頂層 key 是動態的**（`https://auth.x.ai::<uuid>`，每個帳號不同）。不能寫死，要遍歷。
3. ⚠️ **`~/.codex/auth.json` 會被 BAT 熱抽換**，要同時讀 `codex-accounts.json` 的 `activeAccountId`。

好消息：四家都是純 JSON、`0600`、**沒有 keyring 也沒有 SQLite**，所以算過期不需要解密、不需要呼叫 API、不會觸發任何遠端請求。

---

## 7. 開源整合決策

策略是**能力借用、邊界自己畫**。

### 7.1 用

| 工具 | 借它什麼 | 為什麼不自己寫 | 接上了嗎 |
|---|---|---|---|
| **systemd** | process 生命週期、watchdog、以 `User=` 降權的 Agent system unit 與 runtime user units | 自己寫 supervisor 是在重造一個更爛的 systemd | ✅ `ops/*.service`（⚠ watchdog 的教訓見私人工作筆記 PHASE1 §5.6，不在這個公開倉庫） |
| **Tailscale** | 網路可達性、身分、**名冊的獨立證人** | 打洞、ACL、金鑰輪替，一個都不想碰 | ✅ `internal/tailnet` |
| **SQLite** | 第一年的持久化 | 30 台。零維運、單檔備份 | ✅ |
| **Ansible** | **推的那一半**：裝 agent、換版、每台的政策收斂 | 不自己寫 SSH 迴圈與並行；⚠ 它**不是**控制面，也不做驗收（見 7.2） | ✅ 四台實跑過；playbook 在私人工作筆記，不在這個公開倉庫 |
| **healthchecks.io** | 外部死人之鐘 | **必須在 Hub 之外** —— Hub 說自己健康是自證 | ✅ `CLAWCTL_REPORT_PING_URL` |
| **Go modernc.org/sqlite** | 純 Go 讀 SQLite | 免 cgo，維持 static binary；且機器上沒有 `sqlite3` | ✅ |
| **Linux `/proc`** | process 在不在 | 不 spawn `ps`；⚠ 只能讀 `cmdline`/`comm`，**`exe` 需要 ptrace**（見私人工作筆記 PHASE1 §5.10，不在這個公開倉庫） | ✅ `internal/probe` |

#### Tailscale 那一列後來長出了第二個用途

原本只打算借「網路可達性」。實作時發現它其實回答了一個更重要的問題：

> **名冊是分母 —— 但名冊是我們自己寫進去的。**
> 它永遠答不出「有沒有一台機器存在、而我忘了把它放進名冊」。

`tailscale status --json` 是一份**不是我們產生**的機器清單。兩份的差集就是你沒在看的東西。
實機第一次跑：tailnet 上 11 台、名冊上 5 台、6 台在名冊外。

更有價值的是反過來那一半：`sampleagent1` 在 Hub 上是「從未報到」，
而 Tailscale 說它 online —— 於是那一列會多一句
「機器活著，是 agent 沒在跟 Hub 講話」。
「機器死了」跟「agent 沒在報」下一步要做的事完全不同，而 Hub 自己分不出來。

⚠ 但 Tailscale 也不是真相，它只知道裝了 Tailscale 的機器。
文案上永遠只能說「多一個獨立的來源說……」。
而且問不到 `tailscale` 的時候必須回「沒有這個來源」，不是「0 台在名冊外」——
一個空的、乾淨的、沒有壞消息的答案，第一個要懷疑的是有沒有問對地方（私人工作筆記 PHASE1 §5.9，不在這個公開倉庫）。

#### 死人之鐘變成兩條

`ops/deadman.sh`（sampleagent2）擋得住 samplehub1 掛掉，但擋不住 sampleagent2 自己掛掉、
或整個機隊一起掛掉。`CLAWCTL_REPORT_PING_URL` 完全在機隊之外。
兩條不是重複，是不同的失效面 —— 而且兩條的語意一樣硬：
**只有 daily、而且只有推播真的送達之後**才蓋章／才 ping。

### 7.2 不用

| 工具 | 為什麼不用 |
|---|---|
| **Ansible 當控制面** | push-based。機器離線就沒發生過這件事，沒有「稍後補做」也沒有狀態機。⚠ **這個反對到 2026-09-05 為止是對的，現在只對一半 —— 見下面「缺的狀態機補上了」** |
| **osquery / Fleet** | 看得懂 OS，**看不懂 OAuth 過期與最後成功時間**。維運成本 > 省下的時間。第一年只留介面 |
| **OTel / Loki** | 同上。而且它們回答不了名冊分母的問題 |
| ~~**Grafana**~~ | ⚠ **2026-09-05 這一列作廢了，改成用。** 原本的理由「回答不了名冊分母的問題」沒有錯，錯的是把它當成不用的理由 —— 分母的問題是 clawctl 自己回答的（lifecycle-only `clawctl_machines_total`），Grafana 只是把已經有答案的東西畫出來。而「把四條不同來源的線放在同一個時間軸上」這件事，手寫網頁做不到，也不值得自己寫。`ops/prometheus/grafana/`，八格，每一格的 `expr` 都被 `check-dashboard.sh` 拿去對過真實資料 |
| **PostgreSQL** | 另一個要備份、升級、會單獨死的 process。切換條件寫在 §4 |
| **Vault** | 30 台、1 個使用者。`secret_ref` + 檔案權限就夠 |
| **既有 RMM** | 抄 Intune 的是管理邏輯，不是功能廣度 |

#### 缺的狀態機補上了（2026-09-05）

當初否決 Ansible 當控制面的理由是三件事：**push-based**、**機器離線就沒發生過**、
**沒有狀態機**。前兩件今天依然成立，第三件不成立了。

Prometheus 就是那個狀態機。它知道誰在線（`up`）、誰在報到
（`clawctl_machine_last_checkin_age_seconds`）、每一台跑的是哪一版
（`clawctl_agent_info`），而且這些全部**不經過 Ansible**。於是 Intune 的模型
拼得起來：

```
宣告期望  →  Ansible 收斂  →  Prometheus 驗證它真的落地
（git / ssh.json）  （agent.yml）      （verify.sh）
```

⚠⚠ 關鍵在第三格，而且它不是補充，它是重點。**Ansible 說 `changed` 不算數。**
2026-09-03 的雙 agent 事故就是這個形狀：部署腳本回報成功，
三台上各多了一個不受 systemd 管的第二個 agent，跑了六個多小時，
兩個都誠實地送心跳，Hub 上每一盞燈都是綠的。
一個**推的人自己出具的成功回報**，跟一個空的儀表板是同一種東西。

實測（2026-09-05 推 `b94ec3b`）：`agent.yml` 四台全部回報成功之後，
`verify.sh` 先看到 **2/4**，等了約 30 秒才 4/4。那個等待就是證據 ——
它在等機器自己講話、等 Hub 收到、等 Prometheus 抓到，
它讀的不是 Ansible 的回報。

而前兩件事沒有被解決，只是被**分工**了：
- Ansible 不負責「機器離線的時候稍後補做」—— 它就是當場失敗，然後那台在
  `verify.sh` 裡是一格 ⬛ 或 ❌，不會被算成綠的。
- 「稍後補做」仍然是 agent pull 那一側的事（§3 工作單）。
  **推的那一半刻意不做重試，因為一個會自己重試的 push 會讓人以為它是 pull。**

### 7.3 BAT：那個必須明講的取捨

實測（PHASE0 §3，私人工作筆記，不在這個公開倉庫）：`bat-server` v3.2.5 **沒有任何 HTTP 端點**、憑證 CN 是 `rcgen self signed cert` 且有效期到 **4096 年**、handshake **完全不認證**（給錯 token、不給 token、任意 Origin，全部回 `101`）。

| 路徑 | 判決 |
|---|---|
| iframe 嵌入 | ❌ 沒有 HTTP 端點可嵌 |
| 瀏覽器直接 `wss://` | ❌ 瀏覽器對這種自簽憑證硬拒 |
| Hub 反向代理 | ⚠️ 唯一可行，**但它讓 Hub 變成資料面的一跳** |

**第一版的決定：不做代理。** `Connect BAT` 只顯示正確的 Tailscale 位址與一行可複製的指令，用 BAT 桌面客戶端連。

理由：**痛的是「不知道是哪一台」，不是「開終端機很難」。** 顯示位址已經消掉那個痛的八成，而且完整保住了「控制面與資料面分離」。

要做代理的話，**必須當成一個寫在文件裡的明確取捨**，而不是為了 demo 好看偷偷加上去，而且要能一鍵關掉。另外：handshake 不認證代表任何能連到那個 tailnet 位址的東西都能建立連線 —— 目前靠網路層擋。Hub 若去代理，Hub 就成為那個東西，這會**放大既有風險**，不是中性的。

（好消息：BAT 已有 `claude:auth-status` 與 `codex:auth-login` 這類 JSON-RPC，未來要做代跑登入不必自己發明協定。）

---

## 8. UX

### 8.1 左邊只顯示已交付的選單

沒進到該 phase 的選單**不畫出來**。灰掉的按鈕會讓人每次都重新確認一次它還是不能按。

Phase 1 的 UI 就兩頁：**Dashboard + Machines**。

### 8.2 Dashboard 第一屏是四句話，不是一排圓點

1. **名冊分母** —— Online / Degraded / Unreachable / NeverReported / conflict，可點
2. **相對昨天變了什麼**
3. **哪張票快到期、已 failed、或本小時在搶**
4. **有沒有部署卡住**（Phase 4 才有，之前這格空白）

文案必須是句子：

> 節點 `gpu-07` 32 秒前回報；OpenClaw process 在；`claude-02` 最近一次跑完是 11 小時前。
> ⚠ 成果判定未接通 —— 這裡的綠燈只代表跑完了，不代表做對了。

### 8.3 早報是產品，網頁是下鑽

- **每天固定一則**，沒變化也要送 `alive; 0 changes` —— 「沒收到」必須明確代表「Hub 死了」
- **最多 5 行**，超過寫 `+12 more in hub`
- 同一條 Degraded 超過 7 天，第一行改成 `still broken since Sep 1`，**不是再加一盞燈**
- 早報走公網（Telegram），Hub UI 可以留在 tailnet —— 早上不想先連 VPN 才看得到
- **連續 14 天沒開網頁不算荒廢。** 成功指標是「早報準，而且人相信它」
- **幾台在同一分鐘一起失聯，先懷疑觀測者。** 早報把它們合成一行，並拿 Hub
  對自己寫的日誌（重啟／迴圈卡住／時鐘跳動；2026-09-06 起）講成因。
  ⚠ 日誌裡找不到的時候要**明說沒有解釋**，不能寫成「Hub 沒問題」——
  Hub 看不到自己的網路斷線，而「觀測者被排除了」是一句它沒量過的話

### 8.4 每一盞燈都能展開到證據

實際跑的指令、exit code、輸出摘要、驗證時間。點不進去的燈等於沒有燈。

---

## 9. 三大風險

### 風險 1：健康模型塌成「process 活著」

**已經發生了，而且已知。** 成功事件存在但語意不對（§3.1）。

處理：L1/L2 分層。L1 自動、可靠、答得出八成的痛。L2 攤原文給人看，UI 常駐聲明。**不拿 log 關鍵字、不拿 wrapper 心跳、不拿 agent 自己寫的 health JSON 填這個洞。** 三種都是自證。

### 風險 2：控制面一次做太大 → 18 台成功 12 台卡住 → 人關掉早報 → 之後的綠燈是屍體

處理：

- Phase 3 之後**強制只使用一個日曆月**才准開始寫工作單
- 工作單第一版**只覆蓋 OpenClaw 一個 app**
- 新 deployment 省略 `batch_size` 時第一批 1 台、後續也是 1。明示的後續批次可以到 5。任一批失敗就暫停，沒有 auto-continue。canary 成功後也暫停，要明示 Continue 才擴張。既有 in-flight（`pause_after_canary=0`）仍由 driver 自動開下一批
- `Stuck` 是一等公民，有獨立過濾。第一個按鈕是「列出那 12 台」，不是「再推一次」
- canary promote 鎖到「滿一個完整工作天 **且** canary 集合沒有 silent failure」

### 風險 3：十張票的焦慮在沒有資料時長出一個調度器

處理：占用與 ledger 從 Phase 2 就開始寫，但**調度器有硬閘門**（私人工作筆記裡的週表儀式，那些筆記不在這個公開倉庫），第一年預設不寫。

多數時候答案是「靜態指派指錯了」，改指派 + 等七天就解決了 —— 而那不需要寫任何程式。系統只給句子（「`claude-01` 見於 8 台、尖峰 5；`claude-03` 見於 0 台。建議把 3 台改指派到 `claude-03`」），**不給一顆會自動做的按鈕**。

---

## 10. 給第一個 clone 這個 repo 的人

**預設值就是別人的底線。** 那個人不會讀完我們吵的五輪，他只會用預設值。

- `bat-server` 綁私網位址，不開公網
- Agent 只做 outbound，機器上不開任何 inbound 控制 port
- OAuth token 不離開節點
- 上游新版只偵測、只開 PR，**不自動發版**
- 未知版本的 CLI：讀取繼續，**寫入停用**
- 新 deployment 第一批 1 台，失敗即停；canary 成功後要明示 Continue

要放寬的人，自己承擔。
