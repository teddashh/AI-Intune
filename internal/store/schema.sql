-- clawctl Hub schema
--
-- 原則（docs/SPEC.md §4）：Phase 1 就把最終形狀 migrate 出來。空表很便宜，
-- 重寫很貴。Phase 1 的程式只寫 machine_registry / machine_checkins /
-- observed_state / credential_* / ticket_occupancy_observation / notifications。
-- desired_* 與 jobs 建出來但留空，Phase 4 才接寫入路徑。
--
-- 型別以 SQLite 為準：TEXT 存 UUID 與 RFC3339，JSON 存 TEXT。
-- 5 台機器不跑 PostgreSQL —— 那是另一個要備份、升級、會單獨死的 process。

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- ---------------------------------------------------------------- 名冊

-- 名冊就是分母。
--
-- ⚠ 名冊有 5 台、只有 4 台報到，那 1 台亮紅燈，不是從畫面上消失。
-- 這是整個資料模型最重要的一句話。一台機器從來沒 check-in 過不代表它不存在，
-- 只代表你不知道它怎麼了 —— 而那正是這個產品要解決的事。
CREATE TABLE IF NOT EXISTS machine_registry (
  machine_id      TEXT PRIMARY KEY,
  display_name    TEXT NOT NULL,          -- 人講的名字：samplehub1 / sampleagent1
  channel         TEXT,                   -- NULL＝未指派；只准 canary | stable
  channel_revision INTEGER NOT NULL DEFAULT 0, -- operator optimistic concurrency token
  lifecycle_revision INTEGER NOT NULL DEFAULT 0, -- active/retired optimistic concurrency token
  assigned_user_id TEXT,                  -- tailnet 使用者的穩定 ID；NULL＝未指派。login 可以改成跟別人一樣，配對只認這個 ID
  assigned_user_login TEXT,               -- 顯示用的 login，例如 operator@example.com
  assigned_user_revision INTEGER NOT NULL DEFAULT 0, -- operator optimistic concurrency token
  hostname        TEXT,                   -- ⚠ 實測 5 台裡 3 台跟 tailnet 名稱對不上
  expected        INTEGER NOT NULL DEFAULT 1,   -- 分母。0 才不算
  unix_user       TEXT,                   -- 實測每台都不一樣：example-user/ubuntu/opc/example-user-b
  os              TEXT,
  arch            TEXT,
  tailscale_ip    TEXT,
  machine_id_hint TEXT,                   -- /etc/machine-id，重灌會變
  agent_token_hash TEXT,                  -- 只存 hash，不存 token 本身
  linger_enabled  INTEGER,                -- 0 的機器會假離線，UI 要分開講
  notes           TEXT,
  created_at      TEXT NOT NULL,
  enrolled_at     TEXT,
  retired_at      TEXT                    -- retire 後離開分母，但歷史留著
);

CREATE INDEX IF NOT EXISTS ix_registry_expected
  ON machine_registry (expected, retired_at);
CREATE INDEX IF NOT EXISTS ix_registry_change_read
  ON machine_registry (created_at DESC, machine_id);

-- 即時 agent session 的授權投影。BAT Server 沒有使用者 ACL，所以每一列都要
-- 留下 Hub 實際核對過的 tailnet 使用者；關閉後保留作為稽核與留存揭露。
CREATE TABLE IF NOT EXISTS agent_sessions (
  session_id                  TEXT PRIMARY KEY, -- session 的穩定識別碼
  machine_id                  TEXT NOT NULL REFERENCES machine_registry(machine_id), -- session 所連線的機器
  operator_tailnet_user_id    TEXT NOT NULL, -- 開啟 session 的 operator tailnet 穩定使用者 ID
  operator_tailnet_user_login TEXT NOT NULL, -- 開啟 session 時看到的 operator tailnet login
  opened_at                   TEXT NOT NULL, -- Hub 核准並建立 session 的 UTC 時刻
  closed_at                   TEXT, -- Hub 關閉 session 的 UTC 時刻；NULL 表示仍開著
  close_reason                TEXT, -- session 關閉原因；仍開著時為 NULL
  CHECK ((closed_at IS NULL AND close_reason IS NULL) OR
         (closed_at IS NOT NULL AND close_reason IS NOT NULL AND close_reason <> ''))
);

CREATE INDEX IF NOT EXISTS ix_agent_sessions_machine_open
  ON agent_sessions (machine_id, closed_at, opened_at DESC, session_id);

-- Registry lifecycle is append-only evidence.  machine_registry.retired_at is
-- still the current projection used by hot paths, but it cannot answer
-- "retired, restored, then retired again" because unretire clears the value.
-- Every real transition is therefore appended here in the same transaction as
-- the projection update.  Initial registration is synthesized from the
-- immutable machine_registry.created_at row and does not need a duplicate
-- event.
CREATE TABLE IF NOT EXISTS machine_registry_lifecycle_events (
  event_id    INTEGER PRIMARY KEY AUTOINCREMENT,
  machine_id  TEXT NOT NULL REFERENCES machine_registry(machine_id),
  event_type  TEXT NOT NULL CHECK (event_type IN ('retired','registered')),
  occurred_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_registry_lifecycle_read
  ON machine_registry_lifecycle_events (occurred_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS ix_registry_lifecycle_machine
  ON machine_registry_lifecycle_events (machine_id, event_id DESC);
CREATE INDEX IF NOT EXISTS ix_registry_lifecycle_change_machine
  ON machine_registry_lifecycle_events (machine_id, occurred_at DESC, event_id DESC);

-- 看過的 /etc/machine-id 是不可自動復原的身分證據，不是 latest health sample。
-- observed_state 會受 retention 清理；若只從最近 N 筆重算，clone 停止回報後衝突
-- 會自己消失。這張 projection/latch 永不由 retention 刪除，只有 retire 舊名冊身分
-- 並重新 enroll 才會讓新身分有一個乾淨的 machine_id。
CREATE TABLE IF NOT EXISTS machine_identity_hints (
  machine_id        TEXT NOT NULL REFERENCES machine_registry(machine_id),
  hint              TEXT NOT NULL,
  first_seen_at     TEXT NOT NULL,
  last_seen_at      TEXT NOT NULL,
  observation_count INTEGER NOT NULL CHECK (observation_count > 0),
  PRIMARY KEY (machine_id, hint)
);

-- 一次性 enroll token。用過即焚。
CREATE TABLE IF NOT EXISTS enrollment_tokens (
  token_hash   TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  used_at      TEXT,
  used_by      TEXT REFERENCES machine_registry(machine_id)
);

CREATE INDEX IF NOT EXISTS ix_enrollment_tokens_machine_pending
  ON enrollment_tokens (used_by, used_at, token_hash);

-- Verifiers use a credential plane distinct from enrolled machine agents.
-- credential_hash is write-only application material: no Store read model
-- exposes it, and revocation preserves every evidence row it produced.
CREATE TABLE IF NOT EXISTS verifiers (
  verifier_id     TEXT PRIMARY KEY,
  kind            TEXT NOT NULL,
  display_name    TEXT NOT NULL,
  failure_domain  TEXT NOT NULL,
  credential_hash TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  revoked_at      TEXT,
  last_seen_at    TEXT,
  revision        INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS ix_verifiers_display_name
  ON verifiers (display_name);

-- tailnet 上看得到、但你明確說過不納管的機器。
--
-- ⚠ 這張表存在的理由跟 §5.1 那個永遠亮的黃燈一樣：沒有忽略清單，
-- 手機、Windows 桌機那些永遠不會裝 agent 的東西會一直掛在「名冊外的機器」裡，
-- 三天之後整段被當成背景雜訊 —— 然後真的多出一台機器時沒有人會注意到。
--
-- ⚠ 忽略不是刪除。被忽略的台數仍然要顯示，否則就變成偷偷藏起來。
CREATE TABLE IF NOT EXISTS tailnet_ignored (
  hostname   TEXT PRIMARY KEY,   -- 小寫正規化過
  note       TEXT,
  created_at TEXT NOT NULL
);

-- Current ignore rules target Tailscale's stable node ID. Hostname is a
-- display snapshot and confirmation value, never the authority key.
CREATE TABLE IF NOT EXISTS tailnet_peer_ignores (
  peer_id    TEXT PRIMARY KEY,
  hostname   TEXT NOT NULL,
  reason     TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  revision   INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  created_by TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_tailnet_peer_ignores_expiry
  ON tailnet_peer_ignores (expires_at);

-- ---------------------------------------------------------------- 觀測

-- 心跳。每 2 分鐘一筆，append-only。
--
-- ⚠ received_at 是 Hub 收到的時間。生死一律用它算，不用 sent_at ——
-- 機器時鐘快兩天會讓 sent_at 看起來永遠很新鮮。
CREATE TABLE IF NOT EXISTS machine_checkins (
  machine_id      TEXT NOT NULL REFERENCES machine_registry(machine_id),
  sent_at         TEXT NOT NULL,
  received_at     TEXT NOT NULL,
  agent_version   TEXT,
  boot_id         TEXT,          -- ⚠ **機器**的開機 ID，只有重開機才變
  agent_seq       INTEGER,       -- ⚠ 刻意跨重啟連續，所以它抓不到 crash-loop

  -- agent process 起來的時刻。一小時內出現 N 個不同值 = agent 重啟了 N 次。
  --
  -- ⚠ 這是唯一分得出「健康的心跳」與「重啟送出來的心跳」的欄位。
  -- 2026-09-03 之前沒有這一欄，於是全機隊每 90 秒被 SIGABRT 一次、
  -- 每次重啟送一顆心跳，Hub 看到的是規律的心跳流，判定全綠。
  agent_started_at TEXT,
  uptime_seconds  INTEGER,
  disk_free_bytes INTEGER,
  disk_total_bytes INTEGER,
  jobs_enabled INTEGER CHECK (jobs_enabled IN (0,1)),
  observation_age_seconds INTEGER,
  max_seen_revision    INTEGER NOT NULL DEFAULT 0,
  max_applied_revision INTEGER NOT NULL DEFAULT 0,
  clock_skew_seconds   INTEGER,  -- abs(sent-received) > 120 → 「時鐘不可信」

  -- agent 回報它此刻正在跑的設定文件 digest。
  --
  -- ⚠ 這是「設定套用了沒有」唯一合法的證據。不准去比心跳間隔反推，也不准
  -- 讀 agent 的 log —— 那是自證。太舊而不會回報的 agent 留 NULL，Hub 說
  -- 「還沒回報」，不說「已套用」。
  settings_digest TEXT,
  PRIMARY KEY (machine_id, sent_at)     -- UPSERT-only：重試撞到不該產生重複列
);

CREATE INDEX IF NOT EXISTS ix_checkin_recent
  ON machine_checkins (machine_id, received_at DESC);

-- Executor capability belongs to one exact check-in, not to the registry row
-- or a parsed version string. A separate optional table keeps a Hub rollback
-- compatible: an older binary ignores it and continues writing check-ins.
CREATE TABLE IF NOT EXISTS machine_job_capabilities (
  machine_id   TEXT NOT NULL,
  sent_at      TEXT NOT NULL,
  capability  TEXT NOT NULL,
  supported   INTEGER NOT NULL CHECK (supported IN (0,1)),
  PRIMARY KEY (machine_id, sent_at, capability),
  FOREIGN KEY (machine_id, sent_at) REFERENCES machine_checkins(machine_id, sent_at) ON DELETE CASCADE
);

-- 完整觀測。⚠ append-only，永不覆蓋。
--
-- 不可以做成一台一列的快照。上游的資料庫 7 天 / 每 job 2000 筆就會把歷史吃掉，
-- 所以 Hub 是唯一的歷史。而且 Dashboard 預設顯示「相對昨天的變化」，
-- 那只有 append-only 答得出來。
CREATE TABLE IF NOT EXISTS observed_state (
  observation_id TEXT PRIMARY KEY,
  machine_id     TEXT NOT NULL REFERENCES machine_registry(machine_id),
  measured_at    TEXT NOT NULL,   -- Agent 說的
  received_at    TEXT NOT NULL,   -- Hub 收到的
  kind           TEXT NOT NULL,   -- identity|resources|systemd|journal|artifact|events|openclaw|credential|cli_tool|bat
  subject        TEXT NOT NULL,   -- 'openclaw' / 'claude' / unit 名稱 ...
  payload        TEXT NOT NULL,   -- JSON
  source         TEXT NOT NULL    -- agent_measurement | task_event
);

CREATE INDEX IF NOT EXISTS ix_observed_recent
  ON observed_state (machine_id, kind, subject, measured_at DESC);
CREATE INDEX IF NOT EXISTS ix_observed_time
  ON observed_state (measured_at DESC);

-- ⚠ 這個索引供 prune（retention.go）、change read（change_read.go）與 latest 讀徑使用，而它存在的理由是量出來的。
--
-- 「最新」由 MAX(received_at) 定義；prune 同時保護 measured_at 與 received_at
-- 是保守地保留兩個鐘各自的最大值，不是「最新」的定義（見 retention.go）。
-- MAX(measured_at) 走 ix_observed_recent 是 COVERING INDEX，O(1)；
-- 但 MAX(received_at) 在沒有這個索引的時候，EXPLAIN 的結果是
--
--     SEARCH o2 USING INDEX ix_observed_recent (machine_id=? AND kind=? AND subject=?)
--
-- —— 少了 COVERING，意思是它為了算 MAX 去翻了那一組**每一列的表**。
-- 外層又是一次全表掃描，所以總成本是 N × G。現在 N=446 無所謂，
-- 但實測成長率是 4.5 MB/天（約 3500 列/天），一年後 N 超過 100 萬，
-- 那時這句 SQL 會從「7 毫秒」變成「跑不完」，而它是半夜自己跑的。
CREATE INDEX IF NOT EXISTS ix_observed_prune
  ON observed_state (machine_id, kind, subject, received_at DESC);

-- Operator Changes first narrows to the Hub-received comparison window and
-- only then loads the latest payload for each key.  Leading with kind keeps
-- the five canonical change kinds selective; received_at makes the bounded
-- window an index range instead of a retained-history table scan.
CREATE INDEX IF NOT EXISTS ix_observed_changes
  ON observed_state (kind, received_at DESC, machine_id, subject);
CREATE INDEX IF NOT EXISTS ix_observed_changes_machine
  ON observed_state (machine_id, kind, received_at DESC, subject);
CREATE INDEX IF NOT EXISTS ix_observed_changes_subject
  ON observed_state (kind, subject, received_at DESC, machine_id);

-- ---------------------------------------------------------------- 憑證與票

-- 票的身分。「票」= 一個可以登入的帳號。
CREATE TABLE IF NOT EXISTS credential_profile (
  profile_id          TEXT PRIMARY KEY,
  profile_name        TEXT UNIQUE NOT NULL,  -- claude-02 / grok-01
  provider            TEXT NOT NULL,
  kind                TEXT NOT NULL,         -- oauth | api_key（寫入後不可改）
  account_label       TEXT,
  secret_ref          TEXT,                  -- ⚠ 僅 api_key。oauth 必須 NULL
  soft_cap_machines   INTEGER,               -- 未測留 NULL，UI 寫「未測」
  planned_rotation_at TEXT,
  revoked_at          TEXT,
  created_at          TEXT NOT NULL
);

-- 該機看得到的憑證狀態。⚠ 五個詞，不是布林。
CREATE TABLE IF NOT EXISTS credential_on_machine (
  machine_id          TEXT NOT NULL REFERENCES machine_registry(machine_id),
  provider            TEXT NOT NULL,
  profile_id          TEXT REFERENCES credential_profile(profile_id),
  status              TEXT NOT NULL,  -- absent|configured|expires_soon|expired|unknown|failed
  reported_expiry_at  TEXT,
  last_refresh_at     TEXT,           -- 三元組
  file_mtime          TEXT,           -- 三元組
  active_account_id   TEXT,           -- BAT 熱抽換 auth.json，這個才是真的在用的
  verified_at         TEXT,           -- 真實請求驗過的時間。只有這個能給綠燈
  verification_method TEXT,
  last_real_use_at    TEXT,
  last_error_category TEXT,
  last_error          TEXT,
  note                TEXT,
  updated_at          TEXT NOT NULL,
  PRIMARY KEY (machine_id, provider)
);

-- 靜態指派：operator 的意圖。對 oauth 是「我打算讓這台用這張票」，
-- 不是 Hub 把 token 寫過去。
CREATE TABLE IF NOT EXISTS ticket_static_assignment (
  profile_id  TEXT NOT NULL REFERENCES credential_profile(profile_id),
  machine_id  TEXT NOT NULL REFERENCES machine_registry(machine_id),
  assigned_at TEXT NOT NULL,
  assigned_by TEXT NOT NULL,
  desired     INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (profile_id, machine_id)
);

-- 占用帳本。⚠ append-only。第一版只收帳、不算調度。
-- 沒有這本帳，任何「自動輪替」都會變成大家一起撞 429。
--
-- ⚠⚠ 這張表的欄位跟 docs/SPEC.md §4.4 原本寫的不一樣，因為 SPEC 猜錯了上游。
--
-- SPEC 說「以 task_event 為優先」。實測：**上游沒有 task_event 這張表**，
-- 五台機器上都沒有。真正拿得到的證據是 cron 的執行紀錄，而且兩種 layout
-- 放在不同地方：
--
--   consolidated (2026.6.x)  cron_run_logs 這張表
--   split        (2026.5.x)  ~/.openclaw/cron/runs/*.jsonl
--
-- 兩邊的欄位形狀一樣，而且都帶 provider —— 那正是「哪張票在被用」。
--
-- 一行 = 一次真的跑完的 cron 回合。這就是占用的證據本身，不是推論。
CREATE TABLE IF NOT EXISTS ticket_occupancy_observation (
  observation_id       TEXT PRIMARY KEY,
  machine_id           TEXT NOT NULL REFERENCES machine_registry(machine_id),

  -- ⚠ profile_id 目前一律 NULL，而且那是對的。
  -- 我們有證據證明「用了 openai 這一家」，但沒有證據證明「用了哪一張票」——
  -- credential_profile 這張表現在是空的，還沒有人建立票的名冊。
  -- 硬填一個 profile_id 就是 SPEC 明令禁止的「沒證據不准寫」。
  profile_id           TEXT REFERENCES credential_profile(profile_id),

  -- provider 是上游原文，**不正規化**。
  -- samplehub1 寫 "openai"、sampleagent2 寫 "openai-codex"，那是同一條 codex 訂閱
  -- 在不同 OpenClaw 版本下的兩個名字。自動併起來會蓋掉真實差異；
  -- 自動拆開會看起來像一次從來沒發生過的換票。
  provider             TEXT NOT NULL,
  model                TEXT,

  measured_at          TEXT NOT NULL,  -- 上游那筆紀錄的時間
  received_at          TEXT NOT NULL,  -- Hub 收到的時間

  -- occupant_evidence 是可以回頭查證的指標，不是一句形容詞。
  -- 目前放 session_key（形如 agent:<名字>:cron:<job>:run:<id>）。
  occupant_evidence    TEXT NOT NULL,
  job_id               TEXT,
  agent_id             TEXT,           -- 從 session_key 切出來；切不出來就 NULL

  -- ⚠ process_alive 永遠是 0，而且刻意留著這個欄位當提醒。
  -- SPEC 的硬規則：**不准用 process_alive 假裝占用**。
  -- 「有個 process 活著」跟「這張票正在被用」是兩件事。
  process_alive        INTEGER NOT NULL DEFAULT 0,

  -- run_status 是上游原文。⚠ "ok" 只代表這回合正常結束，不代表做對了。
  run_status           TEXT,
  total_tokens         INTEGER,

  -- ⚠ 這裡存的是錯誤原文，不是分類。
  -- SPEC 原本有 last_error_category（rate_limit|auth|timeout|provider|network），
  -- 但把自由文字對到固定分類就是「掃 log 關鍵字」的變形，地基二否決過。
  -- 而且實測最常見的一句是 "cron: job interrupted by gateway restart"，
  -- 它不屬於 SPEC 列的任何一類 —— 硬分類只會分錯。
  last_error_text      TEXT,

  source               TEXT NOT NULL   -- cron_run_logs | cron_runs_jsonl
);

CREATE INDEX IF NOT EXISTS ix_occupancy_machine
  ON ticket_occupancy_observation (machine_id, measured_at DESC);

CREATE INDEX IF NOT EXISTS ix_occupancy_window
  ON ticket_occupancy_observation (provider, measured_at DESC);

CREATE INDEX IF NOT EXISTS ix_occupancy_received
  ON ticket_occupancy_observation (received_at, observation_id);

-- ⚠ 同樣只有 prune 在用，而這張表的情況比 observed_state 更糟：
-- 它的「一組」是 (machine_id, provider)，但上面兩個索引一個只有 machine_id、
-- 一個只有 provider，沒有一個同時有兩欄。實測 EXPLAIN 的結果是
--
--     SEARCH o2 USING INDEX ix_occupancy_machine (machine_id=?)
--
-- —— 它為了算一台機器某一家 provider 的 MAX，翻了那台機器**所有** provider
-- 的整段歷史。而這張表的保留期是 400 天（帳本要答得出「去年這個月」），
-- 所以它是三張表裡會長最久的那一張。
--
-- ⚠ 欄位順序是量出來才定的，不是想出來的。
--
-- 第一版寫成 (machine_id, provider, received_at DESC, measured_at)，想法是
-- 「MAX(received_at) 取第一筆，MAX(measured_at) 掃一下 index 就好」。
-- EXPLAIN 說不是：SQLite 對 MAX(measured_at) 那一句**改用了 ix_occupancy_machine**
-- （只有 machine_id），於是 provider 只能一列一列回表比對。
--
-- 所以 measured_at 要排在 received_at 前面 —— 讓 planner 沒有理由去挑別的索引。
-- 兩句現在都是 index-only：MAX(measured_at) 直接取第一筆，
-- MAX(received_at) 掃這一組的索引項（不回表）。
CREATE INDEX IF NOT EXISTS ix_occupancy_prune
  ON ticket_occupancy_observation (machine_id, provider, measured_at DESC, received_at);

-- 生命週期帳本。⚠ append-only，第一版就要有。
-- 租約引擎可以沒有，帳本不能沒有 —— 沒有它，輪替只會變成口頭故事。
CREATE TABLE IF NOT EXISTS credential_ledger_event (
  event_id     TEXT PRIMARY KEY,
  profile_id   TEXT REFERENCES credential_profile(profile_id),
  machine_id   TEXT REFERENCES machine_registry(machine_id),
  provider     TEXT,
  event_type   TEXT NOT NULL,  -- assigned|unassigned|deployed|revoked|observed_in_use
                               -- |success|rate_limited|auth_failed|machine_offline
                               -- |expired|released_by_human
  occurred_at  TEXT NOT NULL,
  received_at  TEXT NOT NULL,
  evidence_ref TEXT,
  actor        TEXT NOT NULL   -- agent | ted | hub
);

CREATE INDEX IF NOT EXISTS ix_ledger_recent
  ON credential_ledger_event (provider, occurred_at DESC);

-- ---------------------------------------------------------------- 狀態判決

-- 機器狀態是 Hub 算出來的衍生值。⚠ Agent 永遠不准寫這張表。
--
-- 存成歷史而不是一列，因為「這台什麼時候開始壞的」是最常問的問題，
-- 而只存當前狀態的系統永遠答不出來。
CREATE TABLE IF NOT EXISTS machine_state_history (
  machine_id  TEXT NOT NULL REFERENCES machine_registry(machine_id),
  state       TEXT NOT NULL,  -- Dead|Online|Degraded|Unreachable|IdentityConflict
  reason      TEXT NOT NULL,  -- 人看得懂的句子，不是代碼
  entered_at  TEXT NOT NULL,
  left_at     TEXT,
  PRIMARY KEY (machine_id, entered_at)
);

CREATE INDEX IF NOT EXISTS ix_state_open
  ON machine_state_history (machine_id, left_at);
CREATE INDEX IF NOT EXISTS ix_state_change_read
  ON machine_state_history (entered_at DESC, machine_id);

-- machine_state_history is the current span projection. Its second-precision
-- primary key deliberately lets a same-second verdict replace that projection,
-- so rowid cannot freeze a paged historical read. This companion ledger keeps
-- every actual state value change immutable and gives Changes a stable event_id
-- ceiling. history_rowid makes the one-time migration idempotent; UPDATE events
-- leave it NULL so repeated same-second transitions remain distinct evidence.
CREATE TABLE IF NOT EXISTS machine_state_transition_events (
  event_id      INTEGER PRIMARY KEY AUTOINCREMENT,
  machine_id    TEXT NOT NULL REFERENCES machine_registry(machine_id),
  state         TEXT NOT NULL,
  entered_at    TEXT NOT NULL,
  history_rowid INTEGER UNIQUE
);

CREATE INDEX IF NOT EXISTS ix_state_transition_change_read
  ON machine_state_transition_events (entered_at DESC, event_id DESC, machine_id);
CREATE INDEX IF NOT EXISTS ix_state_transition_change_machine
  ON machine_state_transition_events (machine_id, entered_at DESC, event_id DESC);

INSERT OR IGNORE INTO machine_state_transition_events
  (machine_id,state,entered_at,history_rowid)
SELECT machine_id,state,entered_at,rowid
  FROM machine_state_history
 ORDER BY entered_at,machine_id,rowid;

CREATE TRIGGER IF NOT EXISTS append_state_transition_insert
AFTER INSERT ON machine_state_history
BEGIN
  INSERT INTO machine_state_transition_events
    (machine_id,state,entered_at,history_rowid)
  VALUES(NEW.machine_id,NEW.state,NEW.entered_at,NEW.rowid);
END;

CREATE TRIGGER IF NOT EXISTS append_state_transition_update
AFTER UPDATE OF state ON machine_state_history
WHEN OLD.state <> NEW.state
BEGIN
  INSERT INTO machine_state_transition_events
    (machine_id,state,entered_at,history_rowid)
  VALUES(NEW.machine_id,NEW.state,NEW.entered_at,NULL);
END;

-- ---------------------------------------------------------------- 通知

-- 每日推播的送出紀錄。
--
-- ⚠ 沒變化也要送一行 "alive; 0 changes" —— 因為「沒收到訊息」必須能明確
-- 代表「Hub 死了」，而不是「今天沒事」。而且要由 Hub 以外的東西監看它有沒有
-- 準時送到（Hub 說自己健康一樣是自證）。
CREATE TABLE IF NOT EXISTS notifications (
  notification_id TEXT PRIMARY KEY,
  kind            TEXT NOT NULL,  -- daily_report | alert
  channel         TEXT NOT NULL,  -- telegram | discord | stdout
  sent_at         TEXT NOT NULL,
  body            TEXT NOT NULL,
  delivered       INTEGER NOT NULL DEFAULT 0,
  error           TEXT
);

-- 清 notifications 時要留下每個 kind 最新一筆已送達的列。子查詢必須走覆蓋索引，
-- 否則一年後的相關子查詢會在半夜回表。
CREATE INDEX IF NOT EXISTS ix_notifications_prune
  ON notifications(kind, delivered, sent_at);

-- ---------------------------------------------------------------- App catalog

CREATE TABLE IF NOT EXISTS catalog_manifests (
  package_id      TEXT NOT NULL,
  package_version TEXT NOT NULL,
  manifest_json   TEXT NOT NULL,
  manifest_digest TEXT NOT NULL,
  published_at    TEXT NOT NULL,
  published_by    TEXT NOT NULL,
  PRIMARY KEY (package_id, package_version)
);

CREATE INDEX IF NOT EXISTS ix_catalog_manifests_published
  ON catalog_manifests (published_at DESC, package_id, package_version);

CREATE TABLE IF NOT EXISTS machine_profiles (
  profile_id      TEXT NOT NULL,
  profile_revision INTEGER NOT NULL CHECK (profile_revision > 0),
  profile_json    TEXT NOT NULL,
  profile_digest  TEXT NOT NULL,
  published_at    TEXT NOT NULL,
  published_by    TEXT NOT NULL,
  PRIMARY KEY (profile_id, profile_revision)
);

CREATE INDEX IF NOT EXISTS ix_machine_profiles_published
  ON machine_profiles (published_at DESC, profile_id, profile_revision DESC);

-- A profile assignment is one immutable machine-local execution attempt.  The
-- current profile is the newest assignment revision; package rows bind the
-- resolver order to the desired states and jobs created by the same commit.
CREATE TABLE IF NOT EXISTS machine_profile_assignments (
  assignment_id       TEXT PRIMARY KEY,
  machine_id          TEXT NOT NULL REFERENCES machine_registry(machine_id),
  assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
  profile_id          TEXT NOT NULL,
  profile_revision    INTEGER NOT NULL CHECK (profile_revision > 0),
  profile_digest      TEXT NOT NULL,
  target_os           TEXT NOT NULL,
  target_arch         TEXT NOT NULL,
  assigned_at         TEXT NOT NULL,
  assigned_by         TEXT NOT NULL,
  supersedes_assignment_id TEXT REFERENCES machine_profile_assignments(assignment_id),
  UNIQUE (machine_id, assignment_revision),
  FOREIGN KEY (profile_id, profile_revision)
    REFERENCES machine_profiles(profile_id, profile_revision)
);

CREATE INDEX IF NOT EXISTS ix_machine_profile_assignments_current
  ON machine_profile_assignments (machine_id, assignment_revision DESC);

CREATE TABLE IF NOT EXISTS machine_profile_assignment_packages (
  assignment_id    TEXT NOT NULL REFERENCES machine_profile_assignments(assignment_id),
  position         INTEGER NOT NULL CHECK (position >= 0),
  package_id       TEXT NOT NULL,
  package_version  TEXT NOT NULL,
  manifest_digest  TEXT NOT NULL,
  desired_id       TEXT NOT NULL UNIQUE REFERENCES desired_state(desired_id),
  job_id           TEXT NOT NULL UNIQUE REFERENCES jobs(job_id),
  direct           INTEGER NOT NULL CHECK (direct IN (0,1)),
  PRIMARY KEY (assignment_id, position),
  UNIQUE (assignment_id, package_id),
  FOREIGN KEY (package_id, package_version)
    REFERENCES catalog_manifests(package_id, package_version)
);

CREATE TRIGGER IF NOT EXISTS tr_machine_profile_assignment_insert_guard
BEFORE INSERT ON machine_profile_assignments
FOR EACH ROW
WHEN NOT EXISTS (
       SELECT 1 FROM machine_profiles p
        WHERE p.profile_id=NEW.profile_id
          AND p.profile_revision=NEW.profile_revision
          AND p.profile_digest=NEW.profile_digest
     )
  OR NEW.assignment_revision != COALESCE((
       SELECT MAX(a.assignment_revision)+1
         FROM machine_profile_assignments a
        WHERE a.machine_id=NEW.machine_id
     ),1)
  OR (NEW.assignment_revision=1 AND NEW.supersedes_assignment_id IS NOT NULL)
  OR (NEW.assignment_revision>1 AND NEW.supersedes_assignment_id IS NOT (
       SELECT a.assignment_id
         FROM machine_profile_assignments a
        WHERE a.machine_id=NEW.machine_id
        ORDER BY a.assignment_revision DESC LIMIT 1
     ))
BEGIN
  SELECT RAISE(ABORT,'invalid machine profile assignment authority');
END;

CREATE TRIGGER IF NOT EXISTS tr_machine_profile_assignment_update_guard
BEFORE UPDATE ON machine_profile_assignments
FOR EACH ROW
BEGIN
  SELECT RAISE(ABORT,'machine profile assignments are immutable');
END;

CREATE TRIGGER IF NOT EXISTS tr_machine_profile_assignment_delete_guard
BEFORE DELETE ON machine_profile_assignments
FOR EACH ROW
BEGIN
  SELECT RAISE(ABORT,'machine profile assignments are immutable');
END;

CREATE TRIGGER IF NOT EXISTS tr_machine_profile_assignment_package_insert_guard
BEFORE INSERT ON machine_profile_assignment_packages
FOR EACH ROW
WHEN NOT EXISTS (
  SELECT 1
    FROM machine_profile_assignments a
    JOIN catalog_manifests m
      ON m.package_id=NEW.package_id
     AND m.package_version=NEW.package_version
     AND m.manifest_digest=NEW.manifest_digest
    JOIN desired_state d
      ON d.desired_id=NEW.desired_id
     AND d.scope_type='machine'
     AND d.scope_id=a.machine_id
     AND d.created_at=a.assigned_at
     AND d.created_by=a.assigned_by
    JOIN jobs j
      ON j.job_id=NEW.job_id
     AND j.machine_id=a.machine_id
     AND j.desired_id=d.desired_id
     AND j.revision=d.revision
     AND j.created_at=a.assigned_at
     AND j.state='not_started'
   WHERE a.assignment_id=NEW.assignment_id
)
BEGIN
  SELECT RAISE(ABORT,'invalid machine profile assignment package authority');
END;

CREATE TRIGGER IF NOT EXISTS tr_machine_profile_assignment_package_update_guard
BEFORE UPDATE ON machine_profile_assignment_packages
FOR EACH ROW
BEGIN
  SELECT RAISE(ABORT,'machine profile assignment packages are immutable');
END;

CREATE TRIGGER IF NOT EXISTS tr_machine_profile_assignment_package_delete_guard
BEFORE DELETE ON machine_profile_assignment_packages
FOR EACH ROW
BEGIN
  SELECT RAISE(ABORT,'machine profile assignment packages are immutable');
END;

-- ---------------------------------------------------------------- Deployment ledger

CREATE TABLE IF NOT EXISTS desired_state (
  desired_id    TEXT PRIMARY KEY,
  scope_type    TEXT NOT NULL,   -- ⚠ 只准 machine | channel，不准任意 tag
  scope_id      TEXT NOT NULL,
  resource_kind TEXT NOT NULL,
  resource_id   TEXT NOT NULL,
  revision      INTEGER NOT NULL,
  spec          TEXT NOT NULL,
  created_at    TEXT NOT NULL,
  created_by    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS revision_counters (
  resource_scope   TEXT PRIMARY KEY,
  current_revision INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
  job_id            TEXT PRIMARY KEY,
  machine_id        TEXT NOT NULL REFERENCES machine_registry(machine_id),
  desired_id        TEXT REFERENCES desired_state(desired_id),
  revision          INTEGER NOT NULL,
  state             TEXT NOT NULL,
  lease_token       TEXT,
  lease_expires_at  TEXT,
  execution_timeout INTEGER NOT NULL DEFAULT 900,
  artifact_digest   TEXT,
  irreversible      INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL,
  terminal_at       TEXT
);

-- A job may run only after every prerequisite job on the same machine has
-- succeeded.  The edge order is the catalog resolver's deterministic plan
-- order; edges become immutable as soon as they are recorded.
CREATE TABLE IF NOT EXISTS job_dependencies (
  job_id             TEXT NOT NULL REFERENCES jobs(job_id),
  prerequisite_job_id TEXT NOT NULL REFERENCES jobs(job_id),
  position           INTEGER NOT NULL CHECK (position >= 0 AND position < 128),
  PRIMARY KEY (job_id, prerequisite_job_id),
  UNIQUE (job_id, position),
  CHECK (job_id <> prerequisite_job_id)
);

CREATE INDEX IF NOT EXISTS ix_job_dependencies_prerequisite
  ON job_dependencies (prerequisite_job_id, job_id);

CREATE TRIGGER IF NOT EXISTS tr_job_dependencies_same_machine
BEFORE INSERT ON job_dependencies
FOR EACH ROW
WHEN NOT EXISTS (
  SELECT 1
    FROM jobs child
    JOIN jobs prerequisite ON prerequisite.job_id = NEW.prerequisite_job_id
   WHERE child.job_id = NEW.job_id
     AND child.machine_id = prerequisite.machine_id
     AND child.state = 'not_started'
)
BEGIN
  SELECT RAISE(ABORT, 'job dependency child must be unstarted and both jobs must be on the same machine');
END;

CREATE TRIGGER IF NOT EXISTS tr_job_dependencies_acyclic
BEFORE INSERT ON job_dependencies
FOR EACH ROW
WHEN EXISTS (
  WITH RECURSIVE ancestors(job_id) AS (
    SELECT NEW.prerequisite_job_id
    UNION
    SELECT edge.prerequisite_job_id
      FROM job_dependencies edge
      JOIN ancestors parent ON parent.job_id = edge.job_id
  )
  SELECT 1 FROM ancestors WHERE job_id = NEW.job_id
)
BEGIN
  SELECT RAISE(ABORT, 'job dependency cycle');
END;

CREATE TRIGGER IF NOT EXISTS tr_job_dependencies_no_update
BEFORE UPDATE ON job_dependencies
BEGIN
  SELECT RAISE(ABORT, 'job dependencies are immutable');
END;

CREATE TRIGGER IF NOT EXISTS tr_job_dependencies_no_delete
BEFORE DELETE ON job_dependencies
BEGIN
  SELECT RAISE(ABORT, 'job dependencies are immutable');
END;

-- This trigger is also a rollback fence: a Hub binary which predates the
-- dependency scheduler can still never claim a blocked child job.
CREATE TRIGGER IF NOT EXISTS tr_jobs_claim_requires_prerequisites
BEFORE UPDATE OF state ON jobs
FOR EACH ROW
WHEN OLD.state = 'not_started'
 AND NEW.state = 'claimed'
 AND EXISTS (
   SELECT 1
     FROM job_dependencies edge
     LEFT JOIN jobs prerequisite ON prerequisite.job_id = edge.prerequisite_job_id
    WHERE edge.job_id = OLD.job_id
      AND (prerequisite.job_id IS NULL OR prerequisite.state <> 'succeeded')
 )
BEGIN
  SELECT RAISE(ABORT, 'job prerequisites are not satisfied');
END;

-- Operator job reads use stable creation keys plus a per-traversal rowid
-- ceiling. Hidden rowid is not a durable ledger generation across VACUUM or
-- restore. The machine/state variants keep common filters from causing scans.
CREATE INDEX IF NOT EXISTS ix_jobs_read_created
  ON jobs (created_at DESC, revision DESC, job_id DESC);
CREATE INDEX IF NOT EXISTS ix_jobs_read_machine_created
  ON jobs (machine_id, created_at DESC, revision DESC, job_id DESC);
CREATE INDEX IF NOT EXISTS ix_jobs_read_state_created
  ON jobs (state, created_at DESC, revision DESC, job_id DESC);
CREATE INDEX IF NOT EXISTS ix_jobs_machine_state
  ON jobs (machine_id, state);
CREATE INDEX IF NOT EXISTS ix_jobs_read_desired_created
  ON jobs (desired_id, created_at DESC, revision DESC, job_id DESC);
CREATE INDEX IF NOT EXISTS ix_desired_state_read_resource
  ON desired_state (resource_kind, resource_id, desired_id);
CREATE UNIQUE INDEX IF NOT EXISTS ux_desired_state_resource_revision
  ON desired_state (resource_kind, resource_id, revision);

CREATE TABLE IF NOT EXISTS job_events (
  event_id    TEXT PRIMARY KEY,
  job_id      TEXT NOT NULL REFERENCES jobs(job_id),
  seq         INTEGER NOT NULL,
  phase       TEXT NOT NULL,
  occurred_at TEXT NOT NULL,
  received_at TEXT NOT NULL,
  payload     TEXT NOT NULL,
  producer_kind TEXT NOT NULL DEFAULT 'executor_agent',
  producer_id TEXT NOT NULL DEFAULT '',
  evidence_role TEXT NOT NULL DEFAULT 'executor',
  authority TEXT NOT NULL DEFAULT 'machine_bearer_lease',
  provenance_recorded INTEGER NOT NULL DEFAULT 0 CHECK (provenance_recorded IN (0,1)),
  UNIQUE (job_id, seq)
);

CREATE INDEX IF NOT EXISTS ix_job_events_read_received
  ON job_events (job_id, received_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS ix_job_events_read_seq
  ON job_events (job_id, seq DESC, received_at DESC, event_id DESC);

-- ⚠ 獨立一張表，不是 jobs 的欄位。只有 executor evidence 能讓
-- jobs.state='succeeded'；independent verifier evidence 另供讀模型與 stable gate 使用。
-- 明列 producer/role 保存兩者的 provenance；獨立 verdict 由 observed
-- digest/version、Hub received_at 與 producer lifecycle 計算。
CREATE TABLE IF NOT EXISTS verification_results (
  verification_id TEXT PRIMARY KEY,
  job_id          TEXT NOT NULL REFERENCES jobs(job_id),
  machine_id      TEXT NOT NULL REFERENCES machine_registry(machine_id),
  rule_id         TEXT NOT NULL,
  command         TEXT NOT NULL,   -- 實際跑的指令，要能貼給人看
  exit_code       INTEGER,
  stdout_excerpt  TEXT,
  stderr_excerpt  TEXT,
  passed          INTEGER NOT NULL,
  verified_at     TEXT NOT NULL,
  producer_kind   TEXT NOT NULL DEFAULT 'executor_agent',
  producer_id     TEXT NOT NULL DEFAULT '',
  evidence_role   TEXT NOT NULL DEFAULT 'executor',
  authority       TEXT NOT NULL DEFAULT 'machine_bearer_lease',
  provenance_recorded INTEGER NOT NULL DEFAULT 0 CHECK (provenance_recorded IN (0,1)),
  received_at     TEXT NOT NULL DEFAULT '',
  observed_digest TEXT NOT NULL DEFAULT '',
  observed_version TEXT NOT NULL DEFAULT '',
  verifier_id     TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS ix_verifications_read_verified
  ON verification_results (job_id, verified_at DESC, verification_id DESC);

-- 派工：operator 指名「哪一個 verifier 要看哪一張單」。
--
-- ⚠ 這張表存在的理由是**範圍**，不是排程。verifier 平面沒有列舉工作單的能力，
-- 它只拿得到這張表指名給它的東西 —— 被偷走的 verifier bearer 因此只能看到
-- operator 已經打算交給它的那幾張單，而不是整個機隊的工作單。
--
-- ⚠ 這裡沒有 claimed_at、沒有 lease、沒有 state 欄位。一張派工「做完了」的
-- 定義是**證據存在**（verification_results 有同一組 job_id + verifier_id，
-- 且 received_at 不早於 assigned_at），不是有人宣稱做完。
-- 同一組 job + verifier 可以再派一次：新的一列在新的證據進來之前是 pending。
--
-- ⚠ 派工不告訴 verifier 要跑什麼指令。規則編譯在 verifier 自己的程式裡；
-- 一個會執行 Hub 下發指令的 verifier 不是第二個判斷，是一條遠端執行管道。
CREATE TABLE IF NOT EXISTS verification_assignments (
  assignment_id TEXT PRIMARY KEY,
  job_id        TEXT NOT NULL REFERENCES jobs(job_id),
  verifier_id   TEXT NOT NULL REFERENCES verifiers(verifier_id),
  assigned_at   TEXT NOT NULL,
  assigned_by   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_verification_assignments_verifier
  ON verification_assignments (verifier_id, assigned_at DESC, assignment_id DESC);

CREATE INDEX IF NOT EXISTS ix_verification_assignments_job
  ON verification_assignments (job_id, assigned_at DESC, assignment_id DESC);

CREATE TABLE IF NOT EXISTS deployments (
  deployment_id TEXT PRIMARY KEY,
  channel       TEXT NOT NULL,
  desired_id    TEXT NOT NULL REFERENCES desired_state(desired_id),
  resource_kind TEXT NOT NULL,
  resource_id   TEXT NOT NULL,
  revision      INTEGER NOT NULL,
  -- Mutable lifecycle/open-batch CAS token. desired-state revision answers
  -- what agents apply; control_revision answers which operator/driver view
  -- authorized the next control-plane transition.
  control_revision INTEGER NOT NULL DEFAULT 0,
  -- 0 keeps the pre-hold driver: a succeeded batch opens the next one.
  -- New operator creates set 1. After batch 1 is Hub-succeeded, the driver
  -- pauses until an explicit Continue. Existing rows stay 0 via DEFAULT.
  pause_after_canary INTEGER NOT NULL DEFAULT 0,
  batch_size    INTEGER NOT NULL,
  state         TEXT NOT NULL, -- running | paused | finished
  created_at    TEXT NOT NULL,
  created_by    TEXT NOT NULL,
  paused_at     TEXT,
  finished_at   TEXT,
  retry_of      TEXT REFERENCES deployments(deployment_id)
);

CREATE TABLE IF NOT EXISTS deployment_targets (
  deployment_id  TEXT NOT NULL REFERENCES deployments(deployment_id),
  machine_id     TEXT NOT NULL REFERENCES machine_registry(machine_id),
  batch_no       INTEGER NOT NULL, -- 0＝被排除，沒有單
  job_id         TEXT REFERENCES jobs(job_id),
  excluded_reason TEXT, -- conflict | missing_package | unknown_node；NULL＝在計畫內
  PRIMARY KEY (deployment_id, machine_id)
);

CREATE INDEX IF NOT EXISTS ix_deployment_targets_job
  ON deployment_targets (job_id, deployment_id);

-- Driver 在批次邊界被 promote／conflict／revision 等 deterministic guard 擋住時，
-- 把「為什麼沒有再開下一批」跟 paused state 放在同一筆 writer transaction。
-- 一般工作單失敗造成的 pause 不寫這張表；absence 本身保留舊語意。
CREATE TABLE IF NOT EXISTS deployment_boundary_pauses (
  deployment_id TEXT PRIMARY KEY REFERENCES deployments(deployment_id),
  opened_batch  INTEGER NOT NULL,
  kind          TEXT NOT NULL,
  reason        TEXT NOT NULL,
  paused_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_deployments_state_created
  ON deployments(state, created_at);

-- batch_size 是同一資源的機隊 blast-radius 上限，不只是單張 deployment 內的
-- 排版提示。同一資源跨 canary/stable 只能有一個可以再長出 job 的 owner；否則
-- 兩張各開五台，逐機 conflict 看似都合法，實際卻同時改十台。partial UNIQUE
-- index 也是 old binary/手工 caller 的最後防線。
CREATE UNIQUE INDEX IF NOT EXISTS ux_deployments_one_active_resource
  ON deployments(resource_kind, resource_id)
  WHERE state IN ('running', 'paused');

-- Canary 完成後仍可能在第 10 小時才出現沉默失敗；promote 不能只看現在這一秒。
-- ⚠ 不用 hub_events：那張表是 Hub 對自己的日誌，detail 又是自由文字；拿它判決會退回掃字串。
CREATE TABLE IF NOT EXISTS canary_silent_failures (
  machine_id    TEXT NOT NULL,
  reason        TEXT NOT NULL,
  first_seen_at TEXT NOT NULL,
  last_seen_at  TEXT NOT NULL,
  -- open=1 代表還沒有收到明確的健康觀測。不能靠「很久沒再抽樣」把它洗掉；
  -- 只有證據完整的 recovery batch 才能關閉；reconcile 只會開啟或延長。
  open           INTEGER NOT NULL DEFAULT 0 CHECK (open IN (0,1)),
  PRIMARY KEY (machine_id, first_seen_at)
);

CREATE INDEX IF NOT EXISTS ix_canary_silent_failures_last_seen
  ON canary_silent_failures(last_seen_at);
CREATE INDEX IF NOT EXISTS ix_canary_silent_failures_prune
  ON canary_silent_failures(machine_id, last_seen_at DESC);

-- 這一列是 promote failure history 真正開始可被完整記錄的邊界。
-- Open() 跑 migration 只會建立空表；Hub service 載入並發布 policy 的同一筆
-- transaction 才第一次寫入。之後 restart 或 policy 變更都不移動它，否則一個
-- 正常重啟會平白作廢正在監看的 canary。
CREATE TABLE IF NOT EXISTS canary_evidence_epoch (
  singleton  INTEGER PRIMARY KEY CHECK (singleton = 1),
  started_at TEXT NOT NULL
);

-- Hub service 在開始接流量前發布這一份 active policy。CLI 不會繼承
-- systemd EnvironmentFile，所以 promote 必須從同一個 DB 讀到 service 真正在用的版本。
CREATE TABLE IF NOT EXISTS active_workload_policy (
  singleton                INTEGER PRIMARY KEY CHECK (singleton = 1),
  expectations_fingerprint TEXT NOT NULL,
  -- 單看 fingerprint 會讓 A→B→A 復活舊 A 證據。每次語意改變都遞增，
  -- 同政策重發或 Hub restart 則保持不變。
  generation               INTEGER NOT NULL CHECK (generation > 0),
  valid                    INTEGER NOT NULL CHECK (valid IN (0,1)),
  updated_at               TEXT NOT NULL
);

-- 這不是 latest-by-subject 快照；它是「同一批 incoming observation 是否提供
-- 完整 workload 證據」的投影。每批都寫，unknown/failure 會遮住舊 healthy。
CREATE TABLE IF NOT EXISTS workload_observation_witness (
  machine_id               TEXT PRIMARY KEY REFERENCES machine_registry(machine_id),
  verdict                  TEXT NOT NULL CHECK (verdict IN ('healthy','failure','unknown')),
  received_at              TEXT NOT NULL,
  -- 正規化成 Hub 時鐘的 measurement watermark；out-of-order batch 只會把
  -- verdict 改成 unknown，不會把這個 watermark 往回推。
  evidence_at              TEXT NOT NULL,
  openclaw_present         INTEGER NOT NULL CHECK (openclaw_present IN (0,1)),
  running_version          TEXT NOT NULL,
  workload_policy_token    TEXT NOT NULL,
  policy_valid             INTEGER NOT NULL CHECK (policy_valid IN (0,1))
);

-- Promote 不能只看上面那一列 latest witness：Hub 停了半天、policy 中途換過、
-- 或機器短暫跑回舊版，最後一批恢復健康都不代表整個 canary soak 有被監看。
-- 這張 append-only ledger 把每一批 incoming workload verdict 留下來；promote
-- 從 canary cohort 完成時間一路驗到現在，任何 unknown/failure/斷線都會 fail closed。
CREATE TABLE IF NOT EXISTS workload_observation_evidence (
  evidence_id              INTEGER PRIMARY KEY AUTOINCREMENT,
  machine_id               TEXT NOT NULL REFERENCES machine_registry(machine_id),
  received_at              TEXT NOT NULL,
  evidence_at              TEXT NOT NULL,
  verdict                  TEXT NOT NULL CHECK (verdict IN ('healthy','failure','unknown')),
  openclaw_present         INTEGER NOT NULL CHECK (openclaw_present IN (0,1)),
  running_version          TEXT NOT NULL,
  workload_policy_token    TEXT NOT NULL,
  policy_valid             INTEGER NOT NULL CHECK (policy_valid IN (0,1))
);

CREATE INDEX IF NOT EXISTS ix_workload_observation_evidence_machine
  ON workload_observation_evidence(machine_id, evidence_id);

-- 完成時間是 wall clock，會回撥；只有同一個 SQLite writer 序列能證明哪些
-- observation 真的是 deployment 完成後才收到。每次正常 finished transition
-- 在同一筆 transaction 記下當時全域最大的 evidence_id。舊版已完成的 deployment
-- 沒有這列，promote 必須 fail closed 並要求重跑 canary，不能事後猜 boundary。
CREATE TABLE IF NOT EXISTS deployment_soak_boundaries (
  deployment_id TEXT PRIMARY KEY REFERENCES deployments(deployment_id),
  evidence_id   INTEGER NOT NULL CHECK (evidence_id >= 0)
);

-- ---------------------------------------------------------------- 人做的事
--
-- audit_log：這個 console 上，**人**按過的每一下。
--
-- ⚠⚠ 這張表沒有自由文字 `actor` 欄位，那是刻意的。Operator auth 已接上
-- Tailscale，但寫 actor='ted' 仍會把「程式猜的人名」混成 authority evidence。
-- 因此 stable subject、node ID、exact capability 與 decision 各自結構化保存。
--
-- 取而代之的是三個**觀測得到**的東西：
--
--   source_addr  請求從哪個位址來的。不需要問任何人，一定有值。
--   who_node     Tailscale LocalAPI 說那個位址是哪一台裝置（可讀名稱）。
--   who_user     Tailscale LocalAPI 說那台裝置登入的是哪個 tailnet 使用者。
--
-- 後兩個是**第三方的認證結果**，不是自證：Hub 編不出 node key，
-- 也沒辦法讓 Tailscale 替一個不存在的裝置背書。問不到就留空，
-- 而 who_unavailable 會說為什麼問不到 —— 空白不准當答案。
--
-- ⚠ 它認的是裝置，不是坐在鍵盤前的人。畫面上的字是「從哪一台按的」。
--
-- auth_* 是 authenticated operator adapter 已驗證過的 principal／判決；
-- boundary_decision 另存 Host/CSRF 等 request guard，不把「已授權但被 CSRF 擋下」
-- 錯寫成 auth 失敗。舊資料與
-- 尚未接上 boundary 的 adapter 留 NULL。它們不能取代上面的 transport
-- provenance：誰被授權，和 request 從哪台機器進來，是兩份不同的證據。
-- 一般 domain/control row 不受 time retention。未授權網路端可無限製造的
-- operator-denied row 則由 RecordOperatorDenial 在同一 insert transaction 維持
-- fixed-size ring，並先在 HTTP boundary 採樣／聚合；不能拿「audit 都永久保存」
-- 當成填滿 DB/WAL 的遠端 primitive。
CREATE TABLE IF NOT EXISTS audit_log (
  audit_id    INTEGER PRIMARY KEY AUTOINCREMENT,
  at          TEXT NOT NULL,
  action      TEXT NOT NULL,   -- connect / retire / unretire / enroll-token
  machine_id  TEXT,            -- 開票的時候還沒有機器，所以可以是 NULL
  subject     TEXT NOT NULL,   -- 人看得懂的對象：機器名字、或要連過去的位址
  reason      TEXT,            -- 人自己打的，原文一字不改
  idempotency_key TEXT,        -- canonical operator request；legacy action 為 NULL
  request_digest  TEXT,        -- canonical body sha256；可直接對上 idempotency ledger

  source_addr      TEXT NOT NULL,
  who_node         TEXT,
  who_user         TEXT,
  who_unavailable  TEXT,
  user_agent       TEXT,

  auth_subject     TEXT,
  auth_node_id     TEXT,
  auth_capability  TEXT,
  auth_method      TEXT,
  auth_decision    TEXT,
  boundary_decision TEXT,
  source_kind      TEXT,

  -- outcome 是「按下去之後真的發生了什麼」。
  -- ⚠ 失敗的也要留下來。一張只記成功的 audit，答不出
  -- 「有人試著 retire 一台不存在的機器」這種最需要知道的事。
  outcome     TEXT NOT NULL,   -- ok / failed
  detail      TEXT             -- 失敗原文
);

CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log(at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_machine ON audit_log(machine_id, at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_action_id ON audit_log(action, audit_id DESC);

-- Operator JSON API 的冪等帳本。key 是整個 operator plane 共用，不是每支
-- endpoint 各自一份；同一把 key 換 operation 或 request body 必須明確衝突。
-- 成功與 domain rejection 都保存，讓網路重送拿到原本的判決，而不是等環境
-- 改變後把同一個 request 重新執行。
CREATE TABLE IF NOT EXISTS operator_idempotency (
  idempotency_key TEXT PRIMARY KEY,
  operation       TEXT NOT NULL,
  request_digest  TEXT NOT NULL,
  outcome         TEXT NOT NULL CHECK (outcome IN ('ok','rejected')),
  response_json   TEXT,
  error_code      TEXT,
  error_detail    TEXT,
  created_at      TEXT NOT NULL
);

-- Artifact fetches cross a slow, failure-prone network/filesystem boundary, so
-- the operator request only enqueues durable work.  The immutable enqueue
-- receipt stays in operator_idempotency; this row is the restartable worker
-- state.  Raw tarball_url is worker material and must never be copied into a
-- safe operator read DTO.
CREATE TABLE IF NOT EXISTS artifact_fetch_operations (
  operation_id     TEXT PRIMARY KEY,
  idempotency_key  TEXT NOT NULL UNIQUE REFERENCES operator_idempotency(idempotency_key),
  request_digest   TEXT NOT NULL,
  name             TEXT NOT NULL,
  version          TEXT NOT NULL,
  source_kind      TEXT NOT NULL DEFAULT '',
  source_plan      TEXT NOT NULL DEFAULT '',
  registry_origin  TEXT NOT NULL,
  tarball_url      TEXT NOT NULL,
  sha512_integrity TEXT NOT NULL,
  engines_node     TEXT NOT NULL,
  identity_digest  TEXT NOT NULL,
  preview_digest   TEXT NOT NULL,
  max_bytes        INTEGER NOT NULL CHECK (max_bytes > 0 AND max_bytes <= 1073741824),

  state          TEXT NOT NULL CHECK (state IN ('queued','running','succeeded','failed')),
  phase          TEXT NOT NULL CHECK (phase IN ('queued','downloading','verifying','publishing','complete')),
  phase_rank     INTEGER NOT NULL CHECK (phase_rank BETWEEN 0 AND 4),
  progress_bytes INTEGER NOT NULL DEFAULT 0 CHECK (progress_bytes >= 0 AND progress_bytes <= max_bytes),
  attempt        INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
  run_token      TEXT,

  result_sha256    TEXT,
  result_size_bytes INTEGER,
  error_code        TEXT,
  error_detail      TEXT,

  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  started_at  TEXT,
  finished_at TEXT,

  CHECK (length(operation_id) BETWEEN 1 AND 128),
  CHECK (length(idempotency_key) BETWEEN 1 AND 200),
  CHECK (length(request_digest) = 71 AND request_digest GLOB 'sha256:*'),
  CHECK (length(name) BETWEEN 1 AND 128),
  CHECK (length(version) BETWEEN 1 AND 128),
  CHECK (length(CAST(source_kind AS BLOB)) <= 64),
  CHECK (length(CAST(source_plan AS BLOB)) <= 8192),
  CHECK (length(CAST(registry_origin AS BLOB)) BETWEEN 1 AND 2048),
  CHECK (length(CAST(tarball_url AS BLOB)) BETWEEN 1 AND 2048),
  CHECK (length(sha512_integrity) BETWEEN 1 AND 512),
  CHECK (length(CAST(engines_node AS BLOB)) <= 512),
  CHECK (length(identity_digest) = 71 AND identity_digest GLOB 'sha256:*'),
  CHECK (length(preview_digest) = 71 AND preview_digest GLOB 'sha256:*'),
  CHECK (run_token IS NULL OR length(run_token) BETWEEN 1 AND 200),
  CHECK (result_sha256 IS NULL OR length(result_sha256) = 64),
  CHECK (result_size_bytes IS NULL OR (result_size_bytes >= 0 AND result_size_bytes <= max_bytes)),
  CHECK (error_code IS NULL OR length(error_code) BETWEEN 1 AND 64),
  CHECK (error_detail IS NULL OR length(CAST(error_detail AS BLOB)) BETWEEN 1 AND 1000),
  CHECK (length(created_at) = 20 AND created_at GLOB '????-??-??T??:??:??Z'),
  CHECK (length(updated_at) = 20 AND updated_at GLOB '????-??-??T??:??:??Z' AND updated_at >= created_at),
  CHECK (started_at IS NULL OR
    (length(started_at) = 20 AND started_at GLOB '????-??-??T??:??:??Z'
      AND started_at >= created_at AND started_at <= updated_at)),
  CHECK (finished_at IS NULL OR
    (length(finished_at) = 20 AND finished_at GLOB '????-??-??T??:??:??Z'
      AND finished_at = updated_at)),
  CHECK (
    (state = 'queued' AND phase = 'queued' AND phase_rank = 0 AND progress_bytes = 0
      AND attempt = 0 AND run_token IS NULL AND started_at IS NULL AND finished_at IS NULL
      AND result_sha256 IS NULL AND result_size_bytes IS NULL AND error_code IS NULL AND error_detail IS NULL)
    OR
    (state = 'running' AND phase IN ('downloading','verifying','publishing') AND phase_rank BETWEEN 1 AND 3
      AND attempt > 0 AND run_token IS NOT NULL AND started_at IS NOT NULL AND finished_at IS NULL
      AND result_sha256 IS NULL AND result_size_bytes IS NULL AND error_code IS NULL AND error_detail IS NULL)
    OR
    (state = 'succeeded' AND phase = 'complete' AND phase_rank = 4
      AND attempt > 0 AND run_token IS NULL AND started_at IS NOT NULL AND finished_at IS NOT NULL
      AND result_sha256 IS NOT NULL AND result_size_bytes = progress_bytes
      AND error_code IS NULL AND error_detail IS NULL)
    OR
    (state = 'failed' AND phase IN ('downloading','verifying','publishing') AND phase_rank BETWEEN 1 AND 3
      AND attempt > 0 AND run_token IS NULL AND started_at IS NOT NULL AND finished_at IS NOT NULL
      AND result_sha256 IS NULL AND result_size_bytes IS NULL
      AND error_code IS NOT NULL AND error_detail IS NOT NULL)
  )
);

CREATE INDEX IF NOT EXISTS ix_artifact_fetch_operations_read
  ON artifact_fetch_operations(created_at DESC, operation_id DESC);
CREATE INDEX IF NOT EXISTS ix_artifact_fetch_operations_state_read
  ON artifact_fetch_operations(state, created_at DESC, operation_id DESC);
-- A republished version and one prepared identity must each have only one
-- worker authority at a time.  Terminal rows remain permanent evidence and do
-- not prevent a later explicit fetch operation.
CREATE UNIQUE INDEX IF NOT EXISTS ux_artifact_fetch_active_version
  ON artifact_fetch_operations(name, version) WHERE state IN ('queued','running');
CREATE UNIQUE INDEX IF NOT EXISTS ux_artifact_fetch_active_identity
  ON artifact_fetch_operations(identity_digest) WHERE state IN ('queued','running');

-- Restore drills are durable asynchronous operations. The worker only opens a
-- disposable copy of the pinned standalone snapshot; the live ledger stores
-- intent, status, result, audit linkage, and the restart-fencing token.
CREATE TABLE IF NOT EXISTS restore_drill_operations (
  operation_id      TEXT PRIMARY KEY,
  idempotency_key   TEXT NOT NULL UNIQUE REFERENCES operator_idempotency(idempotency_key),
  request_digest    TEXT NOT NULL,
  backup_name       TEXT NOT NULL,
  backup_size_bytes INTEGER NOT NULL CHECK (backup_size_bytes > 0),
  backup_modified_at TEXT NOT NULL,
  backup_sha256     TEXT NOT NULL,
  preview_digest    TEXT NOT NULL,
  live_expected_at_preview INTEGER NOT NULL CHECK (live_expected_at_preview >= -1),

  state      TEXT NOT NULL CHECK (state IN ('queued','running','succeeded','failed')),
  phase      TEXT NOT NULL CHECK (phase IN ('queued','verifying','complete')),
  attempt    INTEGER NOT NULL DEFAULT 0 CHECK (attempt >= 0),
  run_token  TEXT,

  machines             INTEGER,
  expected              INTEGER,
  live_expected         INTEGER,
  newest_checkin_at     TEXT,
  duration_milliseconds INTEGER,
  error_code            TEXT,
  error_detail          TEXT,

  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  started_at  TEXT,
  finished_at TEXT,

  CHECK (length(operation_id) BETWEEN 1 AND 128),
  CHECK (length(idempotency_key) BETWEEN 1 AND 200),
  CHECK (length(request_digest) = 71 AND request_digest GLOB 'sha256:*'),
  CHECK (length(CAST(backup_name AS BLOB)) BETWEEN 1 AND 255),
  CHECK (backup_name NOT LIKE '%/%' AND backup_name NOT LIKE '%\%'),
  CHECK (length(backup_sha256) = 71 AND backup_sha256 GLOB 'sha256:*'),
  CHECK (length(preview_digest) = 71 AND preview_digest GLOB 'sha256:*'),
  CHECK (run_token IS NULL OR length(run_token) BETWEEN 1 AND 200),
  CHECK (machines IS NULL OR machines > 0),
  CHECK (expected IS NULL OR expected >= 0),
  CHECK (live_expected IS NULL OR live_expected >= -1),
  CHECK (duration_milliseconds IS NULL OR duration_milliseconds >= 0),
  CHECK (error_code IS NULL OR length(error_code) BETWEEN 1 AND 64),
  CHECK (error_detail IS NULL OR length(CAST(error_detail AS BLOB)) BETWEEN 1 AND 1000),
  CHECK (length(created_at) = 20 AND created_at GLOB '????-??-??T??:??:??Z'),
  CHECK (length(updated_at) = 20 AND updated_at GLOB '????-??-??T??:??:??Z' AND updated_at >= created_at),
  CHECK (started_at IS NULL OR (length(started_at) = 20 AND started_at GLOB '????-??-??T??:??:??Z')),
  CHECK (finished_at IS NULL OR (length(finished_at) = 20 AND finished_at GLOB '????-??-??T??:??:??Z'
    AND finished_at = updated_at AND finished_at >= started_at)),
  CHECK (
    (state='queued' AND phase='queued' AND attempt=0 AND run_token IS NULL
      AND started_at IS NULL AND finished_at IS NULL AND machines IS NULL AND expected IS NULL
      AND live_expected IS NULL AND newest_checkin_at IS NULL AND duration_milliseconds IS NULL
      AND error_code IS NULL AND error_detail IS NULL)
    OR
    (state='running' AND phase='verifying' AND attempt>0 AND run_token IS NOT NULL
      AND started_at IS NOT NULL AND finished_at IS NULL AND machines IS NULL AND expected IS NULL
      AND live_expected IS NULL AND newest_checkin_at IS NULL AND duration_milliseconds IS NULL
      AND error_code IS NULL AND error_detail IS NULL)
    OR
    (state='succeeded' AND phase='complete' AND attempt>0 AND run_token IS NULL
      AND started_at IS NOT NULL AND finished_at IS NOT NULL AND machines IS NOT NULL
      AND expected IS NOT NULL AND live_expected IS NOT NULL AND duration_milliseconds IS NOT NULL
      AND error_code IS NULL AND error_detail IS NULL)
    OR
    (state='failed' AND phase='verifying' AND attempt>0 AND run_token IS NULL
      AND started_at IS NOT NULL AND finished_at IS NOT NULL AND machines IS NULL AND expected IS NULL
      AND live_expected IS NULL AND newest_checkin_at IS NULL AND duration_milliseconds IS NULL
      AND error_code IS NOT NULL AND error_detail IS NOT NULL)
  )
);

CREATE INDEX IF NOT EXISTS ix_restore_drill_operations_read
  ON restore_drill_operations(created_at DESC,operation_id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS ux_restore_drill_active
  ON restore_drill_operations((1)) WHERE state IN ('queued','running');

-- ---------------------------------------------------------------- 清理紀錄

-- 每一次 prune 刪了什麼。
--
-- ⚠ 這張表的存在理由跟 audit_log 一樣，而且更重要：audit_log 記的是
-- 人做過什麼，這張表記的是**程式自己**刪過什麼。
--
-- 沒有它的話，「這台三十天前的資料被清掉了」跟「這台三十天前沒有資料」
-- 在畫面上長得一模一樣 —— 而第二句是這個專案存在的理由（§5.7 / §5.9）。
-- 一個安靜的清理程序會把「我們刪掉了」偽裝成「從來沒發生過」。
--
-- ⚠ 它自己不准被清。它一天一列、一年三百多列，永遠不會是問題；
-- 而一張會清掉自己的清理紀錄，等於沒有紀錄。
CREATE TABLE IF NOT EXISTS retention_log (
  prune_id     INTEGER PRIMARY KEY AUTOINCREMENT,
  at           TEXT NOT NULL,
  table_name   TEXT NOT NULL,
  rows_deleted INTEGER NOT NULL,
  older_than   TEXT NOT NULL,   -- 這次的界線，之前的都清了

  -- ⚠ 過界、但因為是那一組最新的一筆而**留下來**的列數。
  -- 這是「不刪最後一筆」那條規則有在做事的證據。
  -- 它一直是 0 有兩種可能：沒有任何一組的最新筆過界（正常），
  -- 或那段邏輯壞了（不正常）—— 存下來至少讓人有機會分辨。
  kept_newest  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS ix_retention_at ON retention_log(at DESC);

-- schema 版本，migrate 用
CREATE TABLE IF NOT EXISTS schema_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT INTO schema_meta (key, value) VALUES ('version', '1')
  ON CONFLICT(key) DO NOTHING;

-- These two rows distinguish a fresh ledger from an upgraded ledger.  The
-- latter can start recording lifecycle transitions now, but clearing an old
-- retired_at already erased any earlier unretire history; migration must not
-- claim that it reconstructed evidence it never had.
INSERT INTO schema_meta (key, value)
SELECT 'registry_lifecycle_tracked_from', strftime('%Y-%m-%dT%H:%M:%SZ','now')
  ON CONFLICT(key) DO NOTHING;
INSERT INTO schema_meta (key, value)
SELECT 'registry_lifecycle_history_complete',
       CASE WHEN EXISTS (SELECT 1 FROM machine_registry) THEN '0' ELSE '1' END
  ON CONFLICT(key) DO NOTHING;

-- The transition event ledger can backfill the surviving span value, but an
-- upgraded database may already have overwritten earlier same-second values.
-- Advertise that historical limitation instead of claiming reconstructed data.
INSERT INTO schema_meta (key, value)
SELECT 'state_transition_tracked_from', strftime('%Y-%m-%dT%H:%M:%SZ','now')
  ON CONFLICT(key) DO NOTHING;
INSERT INTO schema_meta (key, value)
SELECT 'state_transition_history_complete',
       CASE WHEN EXISTS (SELECT 1 FROM machine_state_history) THEN '0' ELSE '1' END
  ON CONFLICT(key) DO NOTHING;

-- Hub 自己的日誌。
--
-- ⚠ 這張表回答的是「觀測者那幾分鐘為什麼沒在聽」。2026-09-04 的早報把
-- Hub 重啟寫成三台機器失聯，同一天 00:46 還有一次三台同秒失聯而 Hub 根本
-- 沒重啟 —— 當時只能說「Hub 沒在聽」，說不出是重啟、對帳迴圈卡住、主機睡著
-- 還是網路。這裡記的是 Hub 對自己唯一能誠實講的幾件事：什麼時候起來的、
-- 上一次活著是什麼時候、對帳有沒有慢過、主機的時鐘有沒有比程序多走。
--
-- ⚠ 它不是健康判定。「Hub 活著」由外部死人之鐘說（早報有沒有準時送到），
-- 這裡只是事後對帳用的證據 —— 一份 Hub 自己寫的「我很好」不算數，
-- 但一份「我在 03:53 到 03:55 之間不在」是可以拿去對別的紀錄的。
--
-- ⚠ 只 append。一天幾列，不清。
CREATE TABLE IF NOT EXISTS hub_events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  at       TEXT NOT NULL,
  kind     TEXT NOT NULL,   -- started | stopping | reconcile_slow | loop_stall | clock_jump
  detail   TEXT NOT NULL    -- 人看得懂的一句話
);
CREATE INDEX IF NOT EXISTS ix_hub_events_at ON hub_events(at DESC);

-- 設定原則是不可變的：發佈一次就釘死一個 (policy_id, policy_revision)。
-- 改值等於發佈下一個 revision，舊的留著，因為機器可能還在跑它。
CREATE TABLE IF NOT EXISTS setting_policies (
  policy_id       TEXT NOT NULL,
  policy_revision INTEGER NOT NULL CHECK (policy_revision > 0),
  settings_json   TEXT NOT NULL,
  settings_digest TEXT NOT NULL,
  published_at    TEXT NOT NULL,
  published_by    TEXT NOT NULL,
  PRIMARY KEY (policy_id, policy_revision)
);

CREATE INDEX IF NOT EXISTS ix_setting_policies_published
  ON setting_policies (published_at DESC, policy_id, policy_revision DESC);

-- 指派把一個已發佈的 revision 釘在一個 scope 上。
--
-- ⚠ scope 只准 machine 與 channel，跟 desired_state 同一條規則。任意 tag 會
-- 讓「這台機器吃到哪一份設定」變成無法在轉帳裡回答的問題。
CREATE TABLE IF NOT EXISTS setting_assignments (
  assignment_id       TEXT PRIMARY KEY,
  scope_type          TEXT NOT NULL CHECK (scope_type IN ('machine','channel')),
  scope_id            TEXT NOT NULL,
  assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
  policy_id           TEXT NOT NULL,
  policy_revision     INTEGER NOT NULL CHECK (policy_revision > 0),
  settings_digest     TEXT NOT NULL,
  assigned_at         TEXT NOT NULL,
  assigned_by         TEXT NOT NULL,
  supersedes_assignment_id TEXT REFERENCES setting_assignments(assignment_id),
  UNIQUE (scope_type, scope_id, assignment_revision),
  FOREIGN KEY (policy_id, policy_revision)
    REFERENCES setting_policies(policy_id, policy_revision)
);

CREATE INDEX IF NOT EXISTS ix_setting_assignments_current
  ON setting_assignments (scope_type, scope_id, assignment_revision DESC);

-- 合規性原則跟設定原則一樣是不可變的：發佈一次釘死一個
-- (policy_id, policy_revision)。改規則等於發佈下一個 revision。
--
-- ⚠ 存的是規則，不是判決。判決永遠由 Hub 在讀的時候現算，因為它依賴
-- 「現在幾點」與最新一次 check-in —— 把判決寫進表裡，就會有一列說某台機器
-- 合規、而它其實已經三天沒報到。
CREATE TABLE IF NOT EXISTS compliance_policies (
  policy_id       TEXT NOT NULL,
  policy_revision INTEGER NOT NULL CHECK (policy_revision > 0),
  rules_json      TEXT NOT NULL,
  rules_digest    TEXT NOT NULL,
  published_at    TEXT NOT NULL,
  published_by    TEXT NOT NULL,
  PRIMARY KEY (policy_id, policy_revision)
);

CREATE INDEX IF NOT EXISTS ix_compliance_policies_published
  ON compliance_policies (published_at DESC, policy_id, policy_revision DESC);

-- ⚠ scope 只准 machine 與 channel，跟 setting_assignments 同一條規則。
CREATE TABLE IF NOT EXISTS compliance_assignments (
  assignment_id       TEXT PRIMARY KEY,
  scope_type          TEXT NOT NULL CHECK (scope_type IN ('machine','channel')),
  scope_id            TEXT NOT NULL,
  assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
  policy_id           TEXT NOT NULL,
  policy_revision     INTEGER NOT NULL CHECK (policy_revision > 0),
  rules_digest        TEXT NOT NULL,
  assigned_at         TEXT NOT NULL,
  assigned_by         TEXT NOT NULL,
  supersedes_assignment_id TEXT REFERENCES compliance_assignments(assignment_id),
  UNIQUE (scope_type, scope_id, assignment_revision),
  FOREIGN KEY (policy_id, policy_revision)
    REFERENCES compliance_policies(policy_id, policy_revision)
);

CREATE INDEX IF NOT EXISTS ix_compliance_assignments_current
  ON compliance_assignments (scope_type, scope_id, assignment_revision DESC);

-- 註冊上限：這個 Hub 最多納管幾台。
--
-- ⚠ 沒有這一列就是「沒有設上限」，不是「上限 0」。一個用 0 代表無限的欄位，會在
-- 有人真的想把上限設成 0（誰都不准再納管）的那天變成一個講不出差別的設定。
-- 上限算的是分母：名冊上沒有退役的列。一張還沒用掉的票在開票那一刻就已經有名冊列，
-- 所以它本來就算在裡面，不會被重複計算。
-- ⚠ 取消上限是把 limit_set 設成 0，不是刪掉這一列。刪掉會讓 revision 退回 0，
-- 而 revision 是「你看到的還是不是現在這一份」那個判準——一個會倒退的版本號，會在
-- 設了又取消之後把一份早就過期的預覽重新變成有效的。留著這一列也讓「誰把上限拿掉、
-- 為什麼」問得到答案，不必去翻稽核。
CREATE TABLE IF NOT EXISTS enrollment_limit (
  singleton    INTEGER PRIMARY KEY CHECK (singleton = 1),
  limit_set    INTEGER NOT NULL CHECK (limit_set IN (0,1)),
  max_machines INTEGER NOT NULL CHECK (max_machines >= 0),
  revision     INTEGER NOT NULL CHECK (revision > 0),
  reason       TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  updated_by   TEXT NOT NULL
);

-- object_blobs 是 Hub 自己量過的位元組的參考，不是機器自述。
-- digest 是 Hub 在寫入 backend 之前算出的 SHA-256。這張表沒有 machine_id：
-- 它不是某台機器的事實，只是 artifacts / evidence 大檔的位址。
-- 本機 artifacts 目錄仍是工作複本。有設定 R2/S3 時，backend 才是耐久副本。
CREATE TABLE IF NOT EXISTS object_blobs (
  digest      TEXT PRIMARY KEY CHECK (
    length(digest) = 64 AND digest GLOB '????????????????????????????????????????????????????????????????'
      AND lower(digest) = digest
  ),
  size_bytes  INTEGER NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 1073741824),
  object_key  TEXT NOT NULL CHECK (object_key = 'blobs/' || digest),
  backend     TEXT NOT NULL CHECK (backend IN ('r2','s3','dir','memory')),
  kind        TEXT NOT NULL CHECK (kind IN ('artifact','evidence')),
  media_type  TEXT NOT NULL CHECK (length(media_type) BETWEEN 1 AND 128),
  created_at  TEXT NOT NULL CHECK (length(created_at) = 20 AND created_at GLOB '????-??-??T??:??:??Z')
);

-- Disk-clean maintenance. These tables are the Hub's own ledger for one
-- resource (maintenance/disk-clean). They are not artifact deployments.
-- Summaries, assignments, and alert state name a machine. Rollout targets do
-- too. The rollout row itself uses canary_machine_id so it is not a
-- machine-scoped table.
CREATE TABLE IF NOT EXISTS maintenance_summaries (
  summary_id    TEXT PRIMARY KEY,
  machine_id    TEXT NOT NULL REFERENCES machine_registry(machine_id),
  job_id        TEXT NOT NULL UNIQUE REFERENCES jobs(job_id),
  received_at   TEXT NOT NULL,
  revision      INTEGER NOT NULL,
  config_digest TEXT NOT NULL,
  mode          TEXT NOT NULL,
  summary_json  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_maintenance_summaries_machine
  ON maintenance_summaries (machine_id, received_at DESC);

CREATE TABLE IF NOT EXISTS maintenance_rollouts (
  rollout_id        TEXT PRIMARY KEY,
  desired_id        TEXT NOT NULL,
  revision          INTEGER NOT NULL,
  state             TEXT NOT NULL,
  control_revision  INTEGER NOT NULL,
  opened_batch      INTEGER NOT NULL,
  canary_machine_id TEXT NOT NULL,
  scope_type        TEXT NOT NULL,
  scope_id          TEXT NOT NULL,
  config_digest     TEXT NOT NULL,
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS maintenance_rollout_targets (
  rollout_id TEXT NOT NULL REFERENCES maintenance_rollouts(rollout_id),
  machine_id TEXT NOT NULL REFERENCES machine_registry(machine_id),
  batch_no   INTEGER NOT NULL CHECK (batch_no IN (1, 2)),
  job_id     TEXT,
  PRIMARY KEY (rollout_id, machine_id)
);

CREATE TABLE IF NOT EXISTS maintenance_assignments (
  machine_id    TEXT PRIMARY KEY REFERENCES machine_registry(machine_id),
  desired_id    TEXT NOT NULL,
  revision      INTEGER NOT NULL,
  config_digest TEXT NOT NULL,
  assigned_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS maintenance_alert_state (
  machine_id  TEXT NOT NULL REFERENCES machine_registry(machine_id),
  condition   TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  active      INTEGER NOT NULL CHECK (active IN (0, 1)),
  delivered   INTEGER NOT NULL CHECK (delivered IN (0, 1)),
  updated_at  TEXT NOT NULL,
  PRIMARY KEY (machine_id, condition)
);

-- Local operator credentials. Session bearer tokens are never persisted.
CREATE TABLE IF NOT EXISTS hub_accounts (
 account_id TEXT PRIMARY KEY,
 username TEXT UNIQUE NOT NULL CHECK(length(username) BETWEEN 3 AND 64 AND username NOT GLOB '*[^a-z0-9._-]*'),
 password_hash TEXT NOT NULL,
 created_at TEXT NOT NULL,
 failed_attempts INTEGER NOT NULL DEFAULT 0,
 locked_until TEXT,
 disabled_at TEXT
);
CREATE TABLE IF NOT EXISTS hub_sessions (
 session_hash TEXT PRIMARY KEY,
 account_id TEXT NOT NULL REFERENCES hub_accounts(account_id),
 created_at TEXT NOT NULL,
 last_seen_at TEXT NOT NULL,
 idle_expires_at TEXT NOT NULL,
 absolute_expires_at TEXT NOT NULL,
 revoked_at TEXT,
 source_addr TEXT,
 user_agent TEXT
);
CREATE INDEX IF NOT EXISTS hub_sessions_account ON hub_sessions(account_id);

-- Per-client lockout; account-wide legacy counters are no longer used.
CREATE TABLE IF NOT EXISTS hub_login_failures (
 account_id TEXT NOT NULL REFERENCES hub_accounts(account_id),
 client_ip TEXT NOT NULL,
 failed_attempts INTEGER NOT NULL,
 locked_until TEXT,
 last_failed_at TEXT NOT NULL,
 PRIMARY KEY(account_id, client_ip)
);

CREATE INDEX IF NOT EXISTS idx_hub_login_failures_age ON hub_login_failures(last_failed_at);
