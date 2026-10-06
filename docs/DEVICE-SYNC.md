# Devices「同步裝置資料」原語

## 決定

第九個具名 Devices 遠端動作選「同步裝置資料」。它不是把現有 diagnostic noop 改名：
agent 接受一張固定 `device-sync` 工作單，Hub 確認工作單進入終態後，agent 才把既有的完整
observation loop 排入一次立即執行。這個動作不執行 shell、不讀 operator 提供的 path／URL、
不重啟服務，也不改機器設定。

第一個最小切片交付 executor 與 capability handshake。第二個本地切片建立 Store-owned canonical
preview/apply，但尚不建立 HTTP／CLI／Web transport 或顯示動作入口：

- agent 只接受 resource `device/sync`、`irreversible=false`、1～300 秒 timeout，以及只有
  `kind`／`schema_version` 的 fixed v1 spec；
- agent 每次 check-in 明示 `device_sync_v1=true`；Hub 綁定 exact check-in 保存該次回報，readiness receipt
  從同一筆最新 check-in 回讀；
- 舊 agent 沒有這個欄位，Store 的 latest capability projection 保持 `NULL`。Hub 不從
  `agent_version` 字串猜 capability；
- `maintenance_disk_clean_v1` 是同一種 handshake 的另一個欄位，不是 device-sync 的一部分，
  也不從版號字串推斷。舊 agent 省略它，Hub 就不建立 disk-clean 工作單。agent 是否就緒仍只看
  `device_sync_v1`，避免舊 Hub 把新 agent 卡在啟動。見 [MAINTENANCE-DISK-CLEAN.md](MAINTENANCE-DISK-CLEAN.md)；
- Store preview 釘住 exact latest check-in／capability、machine lifecycle、`jobs_enabled`、nonterminal-job
  occupancy 與 `device:sync` revision；apply 對 capability 未確認、退役、jobs disabled 與 stale preview
  回 typed rejection；
- latest check-in 選取要求該 machine 的每筆 `received_at` 都是 UTC 秒級 canonical RFC3339，
  且所選 row 的 `sent_at` 亦同；違反時 fail closed 為資料完整性拒絕，不是操作員可修正的
  typed rejection。preview shape 與 `preview_digest` 不變。同一條 canonical-strict 規則也適用於
  readiness receipt 回讀，因為 `jobs_enabled` 與 `device_sync_v1` 綁在同一筆被選中的 check-in；不得
  改用 operator 建立閘的 profile assignment／diagnostic parsed-instant max，否則兩欄可能來自不同列；
- Store apply 在單一 transaction 建立 fixed desired state、job、immutable idempotency receipt 與 audit，
  failure injection 證明不會留下 orphan revision／desired state／job／receipt／audit；
- typed rejection 的 audit 與 immutable receipt 也同進退；狀態改善後同 key/body 仍回放原拒絕，
  而 receipt、desired state 或原始 audit 任一不一致都 fail closed、不補寫新證據；
- success／rejection replay 也逐欄核回原始 decision audit 的 transport、Tailnet、authenticated principal、
  boundary decision 與 source kind provenance；caller evidence 缺失或換人時 cache-invalid，不會用只對得上的
  action／subject／digest／outcome 洗出一筆新的 replay audit；
- cached success receipt 的 `display_name` 必須逐字等於原 request 的 `confirm_display_name`；即使 receipt、
  audit subject 與 receipt hash 一起改寫，也不能把另一個名稱冒充成 operator 當時確認的目標。這不會把
  receipt 綁到目前名冊名稱，所以後續合法 rename 不影響原 request replay；
- replay 會從 cached success receipt 重建成功時的非退役 machine snapshot 與完整 impact，並重算
  `preview_digest`；receipt 內的 preview facts 不能在維持原 digest 時被替換，即使 exact JSON 與 audit hash
  也一起改寫。Agent clock 仍不和 Hub clock 排序；
- success receipt 明示 `created_by`，並同時核回原 request 的 server-derived creator 與 durable desired-state
  row；三者任一分歧都 cache-invalid。兩個 writer 共用同一份 preview 時，只有第一個能取得 `device:sync`
  revision，第二個以 stale preview 留下拒絕判決；
- success replay 也核對原始 audit subject，並要求 fixed sync job 維持零 dependency edge：既不能依賴
  另一張 job，也不能被下游 job 當成 prerequisite；任一被改寫都 cache-invalid，不把不同目標或參與額外
  派工 graph 的工作單冒充原 receipt；
- cached success receipt 必須明示空 `blockers`，且 `created_at` 與兩個 latest-check-in 時刻都必須是
  UTC 秒級 canonical RFC3339；Hub-owned `latest_checkin_received_at` 不得晚於 decision `created_at`，
  agent-owned `latest_checkin_sent_at` 不參與跨 clock 排序。即使 receipt 與 audit hash 一起改寫，其他表示法
  或未來才收到的 readiness 仍不會被接受；
- 同一 `(machine_id,sent_at)` check-in 重送時，payload 與 capability 是一份原子 receipt：成功撤回會讓
  sync preview 同時反映 capability-unconfirmed／jobs-disabled；delete 或 reinsert 失敗則完整保留舊 receipt；
- 已完成的 success／rejection decision 只依 immutable receipt 回放，不因後來 capability 撤回、jobs disabled
  或新增 active job 而重新判決；若 replay audit 暫時寫入失敗，原 decision 與 intent 仍完整保留，同 key
  重試可在 audit 恢復後安全完成；
- success replay 會核對 `device:sync` revision counter 仍存在且至少涵蓋 receipt revision；counter 遺失或
  倒退時 fail closed，但後續合法配置的更高 revision 不會讓舊 receipt 失效；
- success replay 要求 receipt 的 `(resource kind, resource ID, revision)` 在 durable desired-state ledger
  中只對到一列；共用 writer 的寫入前檢查與 schema unique index 共同保證這條全機隊號碼帶唯一，
  繞過 writer 的寫入也不能複製 revision。若歷史帳本在建索引前已有歧義，Hub 拒絕開啟，不回放
  authority，也不刪列或選 winner；
- 同 key 被不同 operation／request digest 使用時，conflict audit 必須成功才回 typed conflict；audit 暫時
  寫入失敗不改原 decision，恢復後可重試 conflict，原 key 也仍能回放原結果；
- rejected replay 會核對 cached outcome／response shape、canonical error code／detail／created_at 與原始
  rejection audit；任一缺失或改寫都 cache-invalid，且不補寫 audit、revision 或 intent；
- success replay 會把 job 的 machine、desired link、revision、created_at、artifact digest、irreversible 與
  execution timeout 核回 durable row；這些建立時欄位被改寫就 cache-invalid，state／lease／terminal lifecycle
  則仍由既有工作單狀態機推進，但必須維持 canonical shape：`not_started` 無 lease／terminal、三種 active
  state 有 32-byte canonical raw-base64url fencing token 與完整 lease expiry、且無 terminal，四種可達終態
  清除 lease 且有 terminal；固定 `irreversible=false` 的 sync job 不接受 `manual_intervention`。lease expiry／
  terminal time 只接受 UTC 秒級 canonical RFC3339；active lease expiry 必須嚴格晚於 job 建立時間，terminal
  不得早於建立時間（允許同秒）。`not_started` 的 job 不得帶有 executor 執行證據；獨立驗證列不在此限。
  `succeeded` 另須恰有一列 fixed v1 executor verification：machine lease
  provenance、`device_sync_request_accepted`／`agent observation queue`、exit 0、固定 stdout、空 stderr 與
  passed 全部一致；stdout／stderr 必須是 writer 明確保存的 non-NULL exact value，missing 不等同 empty，且不能
  混入其他 executor evidence。Verification row ID 也必須符合既有 stored-identifier 契約。Independent verifier
  evidence 不計入這個 cardinality。
  Verification 的 agent／Hub 時刻都須為 UTC 整秒
  canonical RFC3339；Hub `received_at` 必須介於 job 建立與成功終態之間。
  成功單也必須恰有 lease-bound executor 的 seq 1 `start`／seq 2 `finish` event，兩者 payload 都是 exact `{}`；
  event row ID 必須符合既有 stored-identifier 契約，時刻同樣須 canonical，且只有 Hub `received_at` 受建立至
  終態區間約束。三筆 Hub-owned 收件時間必須滿足 `start received_at <= verification received_at <= finish
  received_at`；UTC 整秒同值合法。只有 Hub `received_at` 參與這個排序；agent-owned `occurred_at` 與
  `verified_at` 仍只檢查 canonical 格式，不參與先後判決。
  未知 state、矛盾組合、非 canonical token、歧義／倒流時刻或錯誤／缺失驗證都不能回放；
- success replay 也把 desired state 的 scope type／ID、resource kind／ID、revision、spec、created_at 與
  created_by 核回 durable row；任一建立時欄位被改寫都不回放 receipt，也不補寫 authority；
- success replay 要求 receipt 指向的 desired state 只連到原始那張 job；若同一份 intent 被另掛第二張
  job，整份 cache fail closed，不把額外 execution authority 冒充成原判決的一部分；
- success replay 要求目前 machine lifecycle revision 至少涵蓋 receipt revision；receipt 偽造到尚未發生的
  未來 revision 時 fail closed，但 job 終態後的合法 retire／unretire 不會使舊 decision 失效；
- cached success receipt 只接受 writer 以 `json.Marshal` 產生的 exact byte-canonical 單一 JSON object；
  whitespace、欄位重排、等價 escape、duplicate／unknown top-level field 或 trailing document 一律
  cache-invalid，不能依 decoder 的語意正規化或 last-key-wins 行為回放被改寫的文件；
- Devices 目錄仍是九個動作，沒有公開 Web／JSON／CLI create path，也沒有建立 live sync job。

後續只有在一台 agent 已經由 Hub-owned readiness receipt 證明這個 capability 後，才能公開
operator transport。Store authority 已把 fixed spec、machine lifecycle、最新 capability、
`jobs_enabled`、nonterminal-job occupancy、resource revision、typed confirmation、reason、
idempotency receipt 與 audit 綁在同一個 transaction；三個操作平面與 Devices 目錄要等完整啟用刀
一起完成才出現。

## 固定協定

| 欄位 | 固定值／限制 |
| --- | --- |
| job kind | `device-sync` |
| resource | `device`／`sync`；agent watermark 依 resource 分開 |
| spec | `{"kind":"device-sync","schema_version":1}`；未知、重複或第二份 JSON 都拒絕 |
| artifact | 無；job digest 必須是 exact spec bytes 的 SHA-256 |
| irreversible | `false` |
| execution timeout | 預設 60 秒；只接受 1～300 秒 |
| executor effect | 接受同步要求；Hub 確認終態後，由既有 non-blocking nudge 排一輪完整觀測 |
| executor events | exact seq 1 `start`／seq 2 `finish`，兩者 payload 都是 `{}` |
| executor evidence | fixed rule `device_sync_request_accepted`／command `agent observation queue`；只證明要求已由 executor 接受，不宣稱新 observation 已被 Hub 收到 |

同步後的 machine state 仍只讀 Hub 已保存的 observation 與它的 agent／Hub clocks。不得掃 agent log、
不得把 executor 的成功文字當 actual-state 證據，也不得用「job succeeded」取代「新 observation 的
Hub `received_at` 已前進」。

## 威脅模型

| 風險 | 強制邊界 |
| --- | --- |
| 舊 agent 領到不認得的 job | 最新 check-in 必須明示 `device_sync_v1=true`；缺少／false 都不能建立。這一刀沒有 create path |
| 把同步變成任意遠端執行 | spec 沒有 command、path、URL 或 free text；executor strict decode fixed kind/schema，並拒絕錯 scope、不可逆旗標與超界 timeout |
| 重放或倒退 revision | 使用獨立 `device:sync` watermark scope；未來 apply 必須配置單調 revision、immutable receipt 與 idempotency key |
| 觀測風暴／資源耗盡 | 未來 preview/apply 必須要求該機器沒有 nonterminal job；一次 terminal nudge 只有一格 buffered signal，重複 signal 合併 |
| machine bearer 擴權 | agent 仍只 pull 自己 machine ID 的 job；capability 回報不是 operator authority，也不能建立工作單 |
| 自證冒充完成 | verification 只寫「要求已接受」；新狀態是否抵達只看 Hub 收到的 observation row，不讀 log 或 stdout 判斷 |
| Hub／agent 版本前後錯開 | capability 是 nullable、per-check-in 事實；不靠版本字串推測。舊 Hub 會忽略新 JSON 欄位，舊 agent 的最新 check-in 不會產生 capability row |

這個原語不碰 money、密鑰、外部帳號、公開 endpoint 或系統級權限。它沿用 agent 的使用者身分、
outbound-only transport、per-machine bearer、lease fencing 與既有 observation collector。

## 失敗與回復路徑

- Hub 沒有確認 capability：不建立工作單；先恢復／升級該 agent，再等一筆新的 Hub receipt。
- agent 收到錯 spec、scope、不可逆旗標或 timeout：activation 前以 `PRECONDITION_FAILED` 拒絕；
  機器設定不變。修正 producer 後必須用更高 revision 建立新工作單，不重領舊 lease。
- lease 續租失敗或 Agent 中止：executor context 取消；依既有 lease-expiry/manual-intervention 流程處理，
  不把同一張過期工作單交回第二個 executor。
- job 終態後觀測仍未抵達：工作單只代表同步要求已接受，頁面仍顯示原 observation 的 stale/unknown；
  修復觀測錯誤後再以新的 revision 重試。沒有 machine configuration 可 rollback。
- agent binary 回退：下一次 check-in 不再帶 capability，Store 的最新值回到 NULL，入口必須消失；
  已排隊而尚未執行的 sync job會被舊 executor 安全拒絕，不執行替代命令。
- Hub binary 回退：capability 使用獨立 optional table，不增加舊 Hub 不認得的
  `machine_checkins` 欄位；舊 Hub 照常寫 check-in 並忽略該表。恢復新 Hub 後以最新 check-in 重新判
  capability，不沿用舊 row 猜測；上一版 `85bbd2d --rollback-compatible` 已對新 schema 實際通過。
- 資料保留：capability 列以複合外鍵綁定 exact check-in；check-in 到期時在同一 transaction 級聯清除，
  最新一筆仍受既有保護。資料揭露把它列在「報到」類，不把沒有自己 Hub 時刻的從屬列冒充永久資料。

## 啟用前還必須完成

1. 逐台部署 agent，讓 Hub readiness receipt 證明 exact process 已回報 `device_sync_v1=true`；本次不做。
2. Store-owned canonical preview/apply 與 atomic writer 已完成；公開 adapter 必須只呼叫這份 authority，
   不得另寫 desired state 或 job。
3. 同一刀完成 Web、JSON、CLI、Devices 目錄、audit、recovery receipt 與 mutation tests，才可讓操作員看見。
4. 經明示授權選一台非 sampleagent1 機器做 live drill，分開驗「job 終態」與「Hub 收到較新的 observation」。

sampleagent1 不參與部署、演練或分母調整。
