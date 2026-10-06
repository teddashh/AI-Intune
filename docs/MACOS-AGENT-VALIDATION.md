# Mac agent 安裝與報到驗收

在 Linux 建置端產出可攜帶到 Mac 的完整安裝包：

```sh
make agent-bundles-darwin
```

`build/` 會產生兩個檔案，建置輸出會列出各自的 SHA-256：

- `clawctl-agent-bootstrap-darwin-arm64.tar.gz`：Apple Silicon。
- `clawctl-agent-bootstrap-darwin-amd64.tar.gz`：Intel Mac。

每包包含 `VERSION`、`clawctl-agent`、`clawctl-agent.plist` 與
`install-agent-macos.sh`。版本值由 Makefile 的 `VERSION` 決定；需要指定時使用
`make agent-bundles-darwin VERSION='<已選定版本>'`。

## Hub 發布與下載

Hub 的 bootstrap release 保留 Linux amd64／arm64 必要組；同目錄若出現 Darwin
檔案，amd64／arm64 必須同時存在且通過版本、archive layout 與 Mach-O 架構檢查。
Publisher 保留整組檔案與 SHA256SUMS，同版重送必須位元組及平台組合完全一致。
`upgrade-hub.sh` 的建置步驟已包括 `agent-bundles-darwin`；這是程式行為，
本輪沒有執行 live upgrade 或 publish。

註冊頁列出已發布的 OS／架構。既有 Linux `/downloads/agent/amd64`、`arm64`
連結保留；Darwin 使用同一路由的 `darwin-amd64`、`darwin-arm64`。
不存在的 Darwin 組不顯示下載。取回的檔名、digest 與 Hub 版本須與所選組一致。

## 在目標 Mac 上驗收

1. 將符合 Mac 架構的安裝包複製到該機。以 `shasum -a 256 '<安裝包>'`
   與建置端列出的 SHA-256 核對，再解壓到新的目錄。
2. 在解壓目錄執行 `cat VERSION`、`./clawctl-agent version`，核對版本一致。
3. 以將執行 agent 的一般使用者帳號安裝。enrollment token 檔案留在 Mac，
   權限設為 `0600`，不要填進命令列或貼回工作紀錄：

   ```sh
   ./install-agent-macos.sh --hub '<Hub-URL>' --token-file '<Mac-本機-token-檔案>'
   ```

   請先替換引號內的位址與路徑。已有有效 enrollment 的機器，可只傳
   `--hub '<Hub-URL>'`。
4. installer 最後必須回報 `Hub ready`，其中 machine ID、agent 版本與
   `evidence=darwin/arm64` 或 `evidence=darwin/amd64` 要符合該機。
   此步驟會等到 Hub 保存本次啟動後的 check-in、identity 量測與執行能力。
5. 從 Hub 該機詳情核對 macOS 版本、架構與報到時間。Hub 上的 OS 必須是
   `macOS` 加上版本，不能是空白。空白是未知，不是 Linux。
   在該 Mac 上另外記錄兩項獨立量測（不記錄 token）：

   ```sh
   xxd -p -l 8 /System/Library/CoreServices/SystemVersion.plist
   /usr/bin/sw_vers
   ```

   輸出的前綴 `62706c697374` 是 binary plist（`bplist`）；`3c3f786d6c`
   是 XML（`<?xml`）。`sw_vers` 的 ProductName 必須是 `macOS`，並帶
   ProductVersion。Hub 的 OS 字串在 plist 為 XML 時採用
   ProductUserVisibleVersion（若該鍵有值），否則採用 ProductVersion；
   `sw_vers` 只列出 ProductVersion。兩者可能不同，都要寫進驗收紀錄。
   來源：`sw_vers(1)` 與
   `/System/Library/CoreServices/SystemVersion.plist`。
   Linux 上的編譯與 `go test` 不是這一步。
6. 記錄 bundle 版本、machine ID、`Hub ready` 結果、Hub OS／架構、
   plist 前八位元組、`sw_vers` 輸出與驗收時間；不記錄任何 token。
7. 核對 uptime 有量測值，後續 check-in 會增加；agent process 重啟後不應
   歸零。若有進行 Mac 睡眠／喚醒，再核對 uptime 包含這段經過時間。
   讀取失敗的狀態仍應為未知，不能當成剛開機的 0 秒。

遇到失敗時，保留 installer 的失敗步驟與
`~/Library/Logs/clawctl/agent.log` 中相關且已去除敏感值的錯誤訊息。

## 本地 profile 串接驗證

在 repo 根目錄執行：

```sh
go test ./cmd/clawctl-agent -run '^TestDarwinNodeProfileExecutorEvidenceCompletesAssignedJob$' -count=1
go test ./cmd/clawctl-agent -run '^TestDarwinNodeJobsRunnerCompletesThroughHubHTTP$' -count=1
```

此測試從已存的 Mac identity 建立 exact Node profile 指派，將產生的工作單
交給 executor，再將其原始 artifact／Node／npm 證據存入 Hub store。
證據尚未齊全時不得完成，齊全後須寫入成功狀態；涵蓋 arm64／amd64，
以及首次啟用、重用同一份釘版套件兩種情境。

第二支測試在 Linux 建置端啟動臨時 Hub test process，使用正式 machine API
與 bearer 認證。jobs runner 經 HTTP 領單、下載 exact artifact、回報三筆證據、
送出 finish／complete，再核對 Hub 成功狀態及 agent 已套用版本。
arm64／amd64 都涵蓋正常回應與第一個 complete 回應遺失後重試；收到成功
回應前，agent 不得先更新已套用版本。Node runtime 的下載授權要求工作單
屬於該機、尚未完成，且 job、spec 與要求下載的 digest 一致。

HTTP 測試另涵蓋 npm 在暫存檢查與切換後檢查失敗：Hub 應回 `failed`，
不因成功證據未齊而停在 `verifying`；agent 已套用版本不前進，終態工作單
清除租約。切換後失敗須保存回復證據，首次安裝的 `current` 應被移除。
兩個架構與完成回應遺失後重試都包含在內，共 12 個初次執行情境。

8 個失敗情境接著以正式 operator client 經 HTTP 重新 preview／apply 同一份
profile：profile revision、Node 版本與 digest 不變，assignment 與工作單
revision 增加，產生新 job ID。重建 jobs runner 時沿用原本的日誌與安裝目錄。
暫存檢查失敗後重新下載；切換後檢查失敗則重驗並重用保留的 release。
新工作單經 HTTP 回報成功，收到完成回應才將已套用 revision 從 0 更新為 2，
舊工作單及失敗證據完整保留。恢復成功後再次指派同版，應回 already assigned，
不產生新 revision 或工作單。共驗證 20 次 HTTP 工作單執行，含 8 次恢復。

operator HTTP 另核對以下回應與稽核：

| 操作 | 預期結果 |
| --- | --- |
| 沿用工作單尚未失敗時的舊預覽 | HTTP 412、`PROFILE_ASSIGNMENT_PREVIEW_STALE`，不建立新指派 |
| 同一 key 重送已被拒絕的要求 | 仍為 HTTP 412，`Idempotency-Replayed: true`，保留原拒絕判決 |
| 重新預覽後以新 key 指派 | HTTP 201；若回應在落帳後遺失，同一 key 重試取得 HTTP 200 與原 assignment／job |
| 恢復成功後再指派同版 | HTTP 200、already assigned；空 `prerequisite_packages` 為 `[]`，同一 key 回放仍可由正式 client 讀取 |

每個恢復情境核對三組原始／回放稽核，包含 request digest、成功或拒絕、
verified operator subject／node／admin capability、來源與 client User-Agent。
operator origin 保留正式 literal-IP contract，測試 dial 固定導向 loopback Hub；
LocalAPI 身分由 fixture 提供，沒有連線至任何實際 Tailscale peer。

恢復完成後，上述 HTTP 測試也啟動 CLI 子行程，對舊失敗 job 與新成功 job
分別執行 `job show`／`job evidence` 的 JSON 與文字模式。8 組恢復流程共
16 張工作單、64 次 CLI 查證、44 筆 executor 證據；核對終態、清除的 lease、
原 artifact digest／spec／revision、通過與失敗數、rule／command／stdout／stderr、
量測及收件時間與 executor 來源。查詢後 assignment／desired state／job 數維持不變。

Operator 可把 recovery 結果中的 job ID 代入下列 `JOB_ID`；舊失敗 job 保留
原失敗證據，新 job 須回 `succeeded` 並有完整 Node／npm 證據：

```sh
clawctl-hub job show --json JOB_ID
clawctl-hub job evidence JOB_ID --json --limit 100
```

`catalog assign` 的本機 recovery 鏈另由以下測試驗證：

```sh
go test ./cmd/clawctl-hub -run '^TestCatalogCLIAssignmentRecoveryReplaysCommittedDarwinJob$' -count=1
```

此測試使用正式 CLI command dispatcher、operator client、HTTP boundary 與
Hub ledger。arm64／amd64 各測一次及連續兩次回應遺失，共四個情境；handler
先完成指派或回放，再關閉 socket。每次失敗保留 `0700` 目錄內的 `0600`
recovery 檔及重試指令；再次執行只提供檔案路徑，不重新 discovery 或 preview。
沿用原 Hub authority、key、machine、profile revision、確認名稱、reason 與
preview digest，成功時 JSON 回傳原 assignment／job，`replayed=true`，並清除檔案。
全程每台仍只有一筆 assignment／desired state／job，Node 版本與 artifact digest
不變；共核對 10 筆原始／回放稽核與 recovery request digest。

CLI 輸出中斷另由 `TestCatalogCLIOutputInterruptionRetainsRecovery` 驗證：

```sh
go test ./cmd/clawctl-hub -run '^TestCatalogCLIOutputInterruptionRetainsRecovery$' -count=1
```

Mac 雙架構各涵蓋 JSON 部分輸出、文字摘要中斷、摘要成功後 job 行中斷；
另外涵蓋 package／profile 發布的 JSON／文字模式，共 10 個情境。
初次指派與 recover 的文字輸出皆包含每張工作單的
`job POSITION PACKAGE@VERSION JOB_ID`；job 行中斷在這兩條路徑各自驗證。
writer 先接收部分內容，再由已關閉讀端的本機 pipe 回傳 `EPIPE`。
初次操作與第一次 recover 都中斷，recovery 檔須逐 byte 保留；第三次完整
輸出才清除檔案。Hub 保留原成功結果，重試不新增工作單、不改釘版；共核對
30 筆原始／回放稽核。輸出失敗會回傳錯誤並指出操作已完成，可沿用原檔重試。
四個文字情境再從成功輸出的 job 行取出 ID，執行 `job show`／`job evidence`
共八次正式 CLI command／HTTP 查詢，核對原 machine、desired revision、artifact
digest；尚未執行的工作單維持 `not_started`，沒有終止時間或 executor 證據。

實際遇到指派回應遺失或 CLI 輸出中斷時，在原 operator CLI 主機沿用 stderr 顯示的路徑：

```sh
clawctl-hub catalog recover --recovery-file /absolute/path/from-stderr.json
clawctl-hub job show --json JOB_ID
clawctl-hub job evidence JOB_ID --json --limit 100
```

將恢復輸出各 `job` 行的最後一欄代入 `JOB_ID`；需要完整 assignment 結果時，
在 recover 命令加 `--json`。確認原 job ID 與 `replayed`，再依 job evidence 核對執行結果。
回放指派回應不會將尚未執行的 job 判為成功。recovery 檔不需移到 Mac，
也不含 machine token。

Node／npm 使用本地 shell fixture；這些測試不構成 Mac 實機執行驗收。

## 共用多套件 recovery 查證

```sh
go test ./cmd/clawctl-hub -run '^TestCatalogCLIMultiPackageRecoveryPreservesDependencyJobs$' -count=1
```

目前 standard OpenClaw manifest 只宣告 Linux，所以這支測試使用 Linux
arm64／amd64 identity，查證共用的 catalog／recovery／job 讀取鏈。
它不表示 OpenClaw 已支援 Mac，也不是 Mac 發行或實機驗收的新增前置條件。

每個架構各測 JSON／文字模式，以及直接成功／失敗後重新指派，共八情境。
各情境先經指派回應遺失、第一次 recovery 輸出中斷、第二次 recovery 成功。
文字中斷點在 Node job 行完整輸出後、
OpenClaw job 行途中；recovery 檔仍須保留。最後兩張工作單須依 Node →
OpenClaw 排列，保留原 job ID、版本、digest、revision 與相依邊。
單純恢復不增加 assignment、desired state 或 job。

恢復及終態均透過 `job show`／`job evidence` 查證，八情境共 120 次
operator HTTP 讀取。Machine HTTP handlers 驗證 OpenClaw 不能提前
claim，`/next` 先回原 Node job；Node 只被 claim 時，`/next` 回 204。

Node 開始後，沒有證據的 `/complete` 回 409、狀態停在 `verifying`；
補齊 artifact／Node／npm 三筆 fixture 證據但尚未 complete，仍不放行
OpenClaw。Hub 判定 Node 成功後，`/next` 才回原 OpenClaw 工作單，核對
job／desired ID、revision、完整 spec、版本與 artifact digest。
重送 Node `/complete` 回放 succeeded，不重建工作單；原 OpenClaw 可以
claim，送版本 fixture 證據後完成，重送 complete 仍回同一終態。

失敗路徑送 npm 失敗證據，Node 判為 failed，相依 OpenClaw 判為 rejected。
CLI JSON／文字均能查到 `DEPENDENCY_FAILED`、原 Node job ID，以及
`hub_scheduler`／`dependency_graph` 拒絕來源。重新指派同一釘版 profile
產生新一輪 Node → OpenClaw 工作單，兩者都成功；舊失敗證據與拒絕事件不變。

每個情境最後再指派一次，須回 `already_assigned`，保留完成的工作單，
不增加 revision 或工作；`/next` 回 204。直接成功保留一筆 assignment／
兩張 job，失敗再成功保留兩筆 assignment／四張 job。

八情境共 36 筆 executor fixture 證據、4 筆 scheduler 拒絕事件、36 筆
catalog 原始／回放稽核。CLI 核對證據內容、量測及 Hub 收件時間、來源，
並確認終態清除 lease。此測試驗證 Hub 協定，沒有執行實際 Node／OpenClaw。
完整收斂範圍及檢查記錄見私人工作筆記，不在這個公開倉庫。

## 實機 profile 驗收

以這台已報到的 Mac 預覽一個只含 exact Node runtime 版本的 profile，
核對 target 為 darwin、套件版本與 artifact digest 固定。指派後核對工作單
回報的 Node／npm 執行驗證。這仍需 Mac 實機結果，Linux 上的打包與交叉編譯
不構成實機驗收。

完整 default 庫仍依 operator 鎖定的方向包含 4 CLIs＋BAT 並固定版本；Pinokio 為
optional，只有使用者選擇指派時才釘版。此 Node runtime 驗收不代表完整
default 庫已交付，升級仍需 check + approve。
