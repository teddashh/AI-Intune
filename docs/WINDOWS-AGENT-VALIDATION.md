# Windows agent 安裝與報到驗收

在 Linux 建置端產出可攜帶到 Windows 的完整安裝包：

```sh
make agent-bundles-windows
```

`build/` 會產生兩個檔案，建置輸出會列出各自的 SHA-256：

- `clawctl-agent-bootstrap-windows-amd64.tar.gz`：64-bit x86 Windows。
- `clawctl-agent-bootstrap-windows-arm64.tar.gz`：ARM64 Windows。

每包包含 `VERSION`、`clawctl-agent.exe`、`clawctl-agent.task.xml` 與
`install-agent-windows.ps1`。版本值由 Makefile 的 `VERSION` 決定；需要指定時使用
`make agent-bundles-windows VERSION='<已選定版本>'`。

這是目前使用者的 scheduled task，對應 Linux 以明確 `User=` 執行的 systemd system unit 與 macOS 的
LaunchAgent。沒有 LocalSystem service，也沒有 linger：工作階段在使用者登入時啟動。

## Hub 發布與下載

Hub 的 bootstrap release 保留 Linux amd64／arm64 必要組；同目錄若出現 Windows
檔案，amd64／arm64 必須同時存在且通過版本、archive layout 與 PE 架構／executable
檢查。Publisher 保留整組檔案與 SHA256SUMS，同版重送必須位元組及平台組合完全一致。
`upgrade-hub.sh` 的建置步驟已包括 `agent-bundles-windows`。這是程式行為，
本輪沒有執行 live upgrade 或 publish。

註冊頁列出已發布的 OS／架構。既有 Linux 與 Darwin 下載連結保留；Windows 使用
同一路由的 `windows-amd64`、`windows-arm64`。不存在的 Windows 組不顯示下載。

## 在目標 Windows 上驗收

1. 將符合該機架構的安裝包複製到該機。以檔案 SHA-256 與建置端列出的值核對，
   再解壓到新的目錄。
2. 在解壓目錄執行 `Get-Content VERSION` 與 `.\clawctl-agent.exe version`，核對版本一致。
3. 以將執行 agent 的使用者帳號安裝。enrollment token 檔案留在該機，設為僅目前
   使用者可讀寫，不要填進命令列或貼回工作紀錄：

   ```powershell
   .\install-agent-windows.ps1 --hub '<Hub-URL>' --token-file '<Windows-本機-token-檔案>'
   ```

   請先替換引號內的位址與路徑。已有有效 enrollment 的機器，可只傳
   `--hub '<Hub-URL>'`。
4. installer 最後必須回報 Hub ready，其中 machine ID、agent 版本與
   `evidence=windows/amd64` 或 `evidence=windows/arm64` 要符合該機。
   此步驟會等到 Hub 保存本次啟動後的 check-in、identity 量測與執行能力。
5. 從 Hub 該機詳情核對 Windows NT 版本、架構與報到時間。Hub 上的 OS 必須是
   `Windows NT` 加上量到的版本；空白是未知。
6. 核對 `clawctl-agent.service` 的 journal 摘要：有行數或安靜（0 行且無讀取失敗）。
   讀取失敗與安靜分開。摘要只有行數與句型，不是健康判決。
7. 記錄 bundle 版本、machine ID、Hub ready 結果、Hub OS／架構與驗收時間；
   不記錄任何 token。

遇到失敗時，保留 installer 的失敗步驟。Linux 上的編譯與 `go test` 不是這一步。

## 實機 profile 驗收

以這台已報到的 Windows 預覽一個只含 exact Node runtime 版本的 profile，
核對 target 為 windows、套件版本與 artifact digest 固定。指派後核對工作單
回報的 Node／npm 執行驗證（`bin/node.exe`）。這仍需 Windows 實機結果，Linux
上的打包與交叉編譯不構成實機驗收。
