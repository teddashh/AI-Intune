# clawctl-hub Docker 部署 / Docker deploy

For the recommended local-admin + public HTTPS path, use the standalone [Autopilot Docker+Caddy pack](../../docs/AUTOPILOT.md). The existing instructions below describe the advanced Tailscale pack.

建議先參閱 [Autopilot（英文）](../../docs/AUTOPILOT.md)；以下保留進階 Tailscale 路徑。

> **Any small Linux VPS / Docker host works** — not Oracle Cloud specific.  
> **Do not host the Hub on a build/CI machine** (that is a build environment, not a fleet console).

---

## English (brief)

### Locked architecture

- Keep **clawctl-hub (Go + SQLite)** — do **not** rewrite to Vercel/Workers.
- Docker Compose + volume `clawctl-data` for SQLite/state. `hub-data-init` sets that volume to uid **65532** (distroless nonroot) and mode **0700** before Hub starts, and seeds Linux agent bundles on first use.
- **Operator auth = Tailscale app capabilities** (join a free tailnet; see [docs/OPERATOR-AUTH.md](../../docs/OPERATOR-AUTH.md)).
- Optional **Cloudflare Tunnel** for experimental agent ingress only — Hub does **not** trust `X-Forwarded-*` for operator identity; operator UI stays on the Tailscale IP.
- Storage split: SQLite = state and digests; optional R2/S3 = large blobs when configured; GitHub = code only.
- **Not Oracle-only.** Any small Linux VM / Docker host works. See [Where to run it (cost and fit)](../../docs/DEPLOY-OSS.md#where-to-run-it-cost-and-fit).
- **Do not host the Hub on a build/CI machine.**

### Hard constraint

`CLAWCTL_LISTEN` must be this host’s literal Tailscale `IP:port`. Hub has no production default port. `8787` is the conventional example. The grant `dst` port, `CLAWCTL_PUBLIC_URL`, agent `--hub`, and any tunnel origin must use that same port. Compose uses `network_mode: host` and mounts `/var/run/tailscale/tailscaled.sock`.

### Sizing and fresh VM setup

- **VM size**: At least 1 GB RAM free for the Hub (2 GB RAM VM recommended; building the image on a 1 GB VM can run out of memory — use the GHCR image via `docker-compose.ghcr.yml` from [docs/RELEASE.md](../../docs/RELEASE.md) instead, if published).
- **Fresh VM**: Install Docker Engine from Docker's official apt repo and Tailscale with its official script. Join with `sudo tailscale up` (or a tagged auth key via `--auth-key=file:PATH`). Close public SSH at the provider firewall, use Tailscale SSH, enable `unattended-upgrades`, and keep the VM single-purpose. Full instructions: [Fresh VM setup (Docker path)](../../docs/DEPLOY-OSS.md#fresh-vm-setup-docker-path).

### Steps

1. Join Tailscale on the VPS; note `tailscale ip -4`.
2. Save grants (`-view` / `-operate` / `-admin`) for that destination. See [docs/OPERATOR-AUTH.md#quick-start-english](../../docs/OPERATOR-AUTH.md#quick-start-english).
3. `cp ops/docker/hub.env.example ops/docker/hub.env` and set `CLAWCTL_LISTEN` + `CLAWCTL_OPERATOR_CAPABILITY_PREFIX`.  
   **Version note**: Env files are not shell-expanded, so `CLAWCTL_VERSION=$(git rev-parse --short HEAD)` inside `hub.env` is taken literally. Put the value printed by `git rev-parse --short HEAD` into `hub.env` (e.g. `CLAWCTL_VERSION=d217deb`), or `export CLAWCTL_VERSION="$(git rev-parse --short HEAD)"` in the shell before compose. Default `dev` or reusing a version never refreshes agent bundles in the volume. Always pass the git short SHA for real deploys.
4. `docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env up -d --build`  
   Build arg `CLAWCTL_VERSION` (default `dev`) is `main.version`. Hub requires both Linux bundles at `/var/lib/clawctl/agent-bootstrap/<that version>/`. The image builds them (`CGO_ENABLED=0`, amd64 and arm64) and `hub-data-init` copies them into the volume when that directory is absent. It does not replace an existing release.
5. **First login**: Open `http://<CLAWCTL_LISTEN>/` from an authorized tailnet browser. Verify operator identity in the top bar. See [OPERATOR-AUTH quick start](../../docs/OPERATOR-AUTH.md#quick-start-english).
6. **Enroll**: create ticket in UI → install agent on the machine → outbound check-in. To move existing agents to this Hub, see [docs/MOVE-AGENTS.md](../../docs/MOVE-AGENTS.md).
7. Optional tunnel — extra compose file, not `--profile tunnel`. Compose v5 interpolates profile-disabled services, so the token must not live in `docker-compose.yml`.  
   `docker compose -f ops/docker/docker-compose.yml -f ops/docker/docker-compose.tunnel.yml --env-file ops/docker/hub.env up -d`  
   With no `CLOUDFLARE_TUNNEL_TOKEN`, that command fails at interpolation. Agent experiments only.

### Backups

The Docker pack has no continuous replication (Litestream is only in the Fly image today; see [docs/DEPLOY-FLY.md](../../docs/DEPLOY-FLY.md)). Stop the container and tar `clawctl-data`, and copy the archive off-host. Test each archive: extract it into a scratch directory on another machine and run `sqlite3 clawctl.sqlite 'PRAGMA integrity_check;'` (expect `ok`). The built-in `clawctl-hub restore-drill` checks only the standalone SQLite backups the Hub writes itself.

Smoke / fallback: `./ops/docker/smoke-build.sh` (falls back to `make hub` if Docker is missing).

See [docs/DEPLOY-OSS.md](../../docs/DEPLOY-OSS.md) for the full open-source deploy guide. Fly.io, the hosted example, is `docs/DEPLOY-FLY.md`. Cloudflare Containers is not implemented (it would need tsnet, Litestream, and a Durable Object keep-alive).

---

## 中文（精簡）

### 架構（已鎖定）

| 層 | 做法 |
|---|---|
| Hub | 維持 Go + SQLite（`clawctl-hub`），**不要**改寫成 Vercel / Workers |
| 執行 | Docker Compose + 持久 volume `clawctl-data` |
| Operator 身分 | **Tailscale grants app capability**（見 `docs/OPERATOR-AUTH.md`） |
| 公網入口 | 可選 Cloudflare Tunnel（目前只適合作 **agent 實驗**；operator UI 仍走 Tailscale IP） |
| 物件儲存 | SQLite = 狀態與 digest；有設 `R2_*` 或 `S3_*` 才把大檔放到物件儲存；否則沿用本機 artifacts。GitHub = 程式碼 |

### 硬性限制

Hub 的 `--listen` / `CLAWCTL_LISTEN` **必須是本機的 literal Tailscale IP:port**（例如 `100.64.0.12:8787`）。  
Hub **沒有**正式環境預設埠。`--listen` 旗標預設 `127.0.0.1:8770` 會被拒絕。`8787` 只是慣例範例。grant `dst`、`CLAWCTL_PUBLIC_URL`、agent `--hub`、tunnel origin 必須與 `CLAWCTL_LISTEN` 同一個埠。  
`0.0.0.0`、`127.0.0.1`、LAN／公網 IP、hostname 都會在開 DB 前被拒絕。  
因此 compose 使用 **`network_mode: host`**，並掛載 `tailscaled.sock`。

Hub **不信任** `X-Forwarded-*`／`Forwarded` 做 operator 身分。

### 規格與全新主機設置

- **主機規格**：Hub 至少需要 1 GB 可用 RAM（建議 2 GB RAM VM；在 1 GB VM 上本機建置映像檔可能遇到記憶體不足 OOM —— 若有發布，可改用 `docs/RELEASE.md` 的 `docker-compose.ghcr.yml` 跑 GHCR 預編映像檔）。
- **全新主機**：從 Docker 官方 apt repo 安裝 Docker Engine，並以 Tailscale 官方腳本安裝 Tailscale。以 `sudo tailscale up`（或透過 `--auth-key=file:PATH` 讀取 0600 金鑰檔）加入 tailnet。雲端防火牆關閉公網 SSH、使用 Tailscale SSH、啟用 `unattended-upgrades`、保持專機專用。詳見 [全新主機設置（Docker 路徑）](../../docs/DEPLOY-OSS.md#fresh-vm-setup-docker-path)。

### 步驟（任意 VPS）

1. **加入免費 Tailscale tailnet**（主機安裝 `tailscaled`，記下 `tailscale ip -4`）。
2. 在 Tailscale ACL 寫入三個 app capability grant（`…-view` / `…-operate` / `…-admin`），`dst` 指到該 IP，埠與 `CLAWCTL_LISTEN` 相同（範例才是 `tcp:8787`）。詳見 [docs/OPERATOR-AUTH.md#quick-start-english](../../docs/OPERATOR-AUTH.md#quick-start-english)。
3. 複製環境檔並填值：
   ```bash
   cp ops/docker/hub.env.example ops/docker/hub.env
   # CLAWCTL_LISTEN=$(tailscale ip -4):<port>   # 8787 只是慣例範例，非 Hub 預設
   # CLAWCTL_OPERATOR_CAPABILITY_PREFIX=example.com/cap/clawctl
   # CLAWCTL_VERSION=d217deb                  # hub.env 不做 shell 展開；填入 git rev-parse --short HEAD 字面值，或在 shell 執行 export CLAWCTL_VERSION="$(git rev-parse --short HEAD)"
   ```
4. **建置並啟動**（在 repo 根目錄）。`hub-data-init` 會先把 volume `clawctl-data` 收成 uid **65532**、mode **0700**，並在第一次啟動時把 Linux agent bundles 種進 `/var/lib/clawctl/agent-bootstrap/<版本>/`，然後 Hub 才起來：
   ```bash
   docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env up -d --build
   ```
   版本用 build arg `CLAWCTL_VERSION`（預設 `dev`），同時寫進 Hub `main.version` 與 bundle 目錄名。Hub 啟動時固定讀 `<db 目錄>/agent-bootstrap/<版本>` 的 linux amd64 與 arm64，沒有別的路徑可設。重用相同版本名稱不會重新覆寫 volume 內既有的 bundles。
5. **首次登入**：從**已授權的 tailnet 裝置**開啟 `http://<CLAWCTL_LISTEN>/`，確認 top bar 顯示 login／capability。
6. **註冊第一台機器**：UI 開 enroll ticket → 在目標機跑 agent installer → agent outbound check-in。若需將既有端點遷移至此 Hub，請參考 [docs/MOVE-AGENTS.md](../../docs/MOVE-AGENTS.md)。
7. （可選）Cloudflare Tunnel。不要用 `--profile tunnel`：Compose v5 會插值 profile 沒開的 service，token 寫在主檔會讓每個指令都失敗。token 只放在 overlay：
   ```bash
   # hub.env 設 CLOUDFLARE_TUNNEL_TOKEN，tunnel 的 origin 指到 http://<CLAWCTL_LISTEN>
   docker compose -f ops/docker/docker-compose.yml \
     -f ops/docker/docker-compose.tunnel.yml \
     --env-file ops/docker/hub.env up -d
   ```
   沒設 token 時，這條指令在插值階段就失敗。公網 hostname **不能**當 operator UI；僅 agent 實驗。

### 備份

Docker 套件目前沒有連續複寫功能（Litestream 連續備份目前僅建置於 Fly 映像檔；見 `docs/DEPLOY-FLY.md`）。請停止容器後以 tar 封存 `clawctl-data`，複製至主機外部保存。測試每份封存檔：解壓縮至另一台機器的暫存目錄，並執行 `sqlite3 clawctl.sqlite 'PRAGMA integrity_check;'`（預期輸出 `ok`）。內建的 `clawctl-hub restore-drill` 僅檢查 Hub 自行寫入的獨立 SQLite 備份。

### 沒有 Docker 時

```bash
./ops/docker/smoke-build.sh   # 會退回 make hub
# 或沿用既有：make hub && ./ops/install-hub.sh --listen … --operator-capability-prefix …
```

完整說明：`docs/DEPLOY-OSS.md`。託管範例：`docs/DEPLOY-FLY.md`（Fly.io）。Cloudflare Containers 尚未實作。
