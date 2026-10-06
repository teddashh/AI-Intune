# AI-Intune

[English](README.md) · **繁體中文**

完整的雙語說明在 [README.md](README.md#繁體中文)。這一頁保留較短的公開介紹。

AI-Intune 是以證據為核心的控制平臺，管理執行 AI agent 與 coding tools 的機器。Hub 記錄機器清單、觀測資料、desired state、job、驗證證據與稽核事件。Agent 由受管機器主動連出。

[專案介紹頁](https://teddashh.github.io/AI-Intune/?lang=zh-TW) · [Apache 2.0 授權](LICENSE)

## 建置與測試

請在 Linux 上使用 Go 1.27.1：

```sh
git clone https://github.com/teddashh/AI-Intune.git
cd AI-Intune
make test vet
make hub agent-bundles
```

Hub 執行檔是 `build/clawctl-hub`。Linux 版 Agent 執行檔與 bootstrap bundle 會產生在 `build/` 底下。`ops/` 另外收錄各平臺的 Agent 安裝程式與系統服務範本。

## 部署

主要路徑是任何有 Docker 的 Linux 主機。見 [docs/DEPLOY-OSS.md](docs/DEPLOY-OSS.md)。把 `CLAWCTL_LISTEN` 設成該主機的 Tailscale IP 與你選的埠（`8787` 只是慣例範例，不是 Hub 預設）。Tailscale grant 的 `dst` 埠、`CLAWCTL_PUBLIC_URL`、agent 的 `--hub`、以及 tunnel origin 都必須使用同一個埠。

```sh
cp ops/docker/hub.env.example ops/docker/hub.env
docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env up -d --build
```

沒有 Docker 時改用 systemd（`ops/install-hub.sh`）。Fly.io 是文件裡的託管範例：[docs/DEPLOY-FLY.md](docs/DEPLOY-FLY.md)。Cloudflare Containers 尚未實作；那條路需要 tsnet（Hub 行程內的 userspace Tailscale）、Litestream，以及 Durable Object keep-alive。

## 運作模式

Hub 預期部署在私有網路中。Operator 存取使用 Tailscale 身分與 capabilities；每個 Agent 都有自己的機器憑證。註冊時使用一次性 token。憑證與本機設定只留在 operator 或受管機器上，從不放進這個 repository。

目前的 API 與行為請以原始碼與測試為準。部署前請先設定一套獨立的私有環境；程式碼與測試中的範例位址僅供說明。
