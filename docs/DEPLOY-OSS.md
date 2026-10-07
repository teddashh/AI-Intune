# Open-source deploy guide — clawctl-hub (OSS / any cloud)

Status: **Milestone 4 — Hub-enforced canary hold**. The primary way to run Hub is
**any Linux host with Docker** (`ops/docker`, `docker compose`). A systemd install
(`ops/install-hub.sh`) is the alternative when the host has no Docker.
[Fly.io](DEPLOY-FLY.md) is the documented hosted example: one always-on machine,
Tailscale, and no public HTTP service. This guide does **not** change the
operator auth model.

Related: [OPERATOR-AUTH.md](OPERATOR-AUTH.md), [PRODUCT.md](PRODUCT.md),
[`ops/docker/README.md`](../ops/docker/README.md), [`ops/install-hub.sh`](../ops/install-hub.sh).

---

## 1. Why Vercel / Cloudflare Workers are the wrong place for Hub

clawctl-hub is a **long-lived control plane**:

- Process-lifetime SQLite writer lock and upgrade/maintenance fences
- In-process job scheduling, artifact fetch workers, restore drills
- Tailscale **LocalAPI** `WhoIsForIP` on every operator request (needs `tailscaled` on the same host)
- systemd-oriented notify/watchdog on bare metal; Docker uses `restart:` instead

Serverless request handlers (Vercel, Workers) cannot hold that writer lock,
cannot speak LocalAPI to a host daemon, and would force a rewrite of the
ledger model. **Do not port Hub to serverless.** Keep the Go binary; put
HTTPS in front with Cloudflare Tunnel if you need a public name for agents.

The **site/** static pages (if any) can live on Pages/Vercel; the Hub cannot.

---

## 2. Recommended topology (any Linux VPS)

```
                    Tailscale tailnet (free)
  Operator laptop ────────────────────────────┐
  (grants: view/operate/admin)                │
                                              ▼
                                    ┌─────────────────────┐
                                    │  Small Linux VPS     │
                                    │  tailscaled + Docker │
                                    │                      │
                                    │  clawctl-hub         │
                                    │  listen 100.x.y.z:8787 (example port)
                                    │  volume: clawctl-data│
                                    │  LocalAPI socket     │
                                    └──────────┬──────────┘
                                               │ optional CF Tunnel
                                               │ (agent experiments)
                                               ▼
                                         Public hostname
                                         (NOT operator UI today)

  Managed machines ──outbound HTTPS/HTTP──► Hub URL
  (agent bearer; prefer Tailscale IP today)
```

| Piece | Role |
|---|---|
| **Any Linux + Docker** | Primary path. Runs Hub; Oracle / AWS / Hetzner / home lab all fine |
| **systemd** | Alternative when you do not want Docker (`ops/install-hub.sh`) |
| **Fly.io** | Hosted example. See [DEPLOY-FLY.md](DEPLOY-FLY.md). Not the primary path |
| **Tailscale** | Operator identity + reachability; Hub binds its Tailscale IP |
| **Cloudflare Tunnel** | Optional public ingress for **agent** experiments |
| **SQLite volume** | Fleet state (machines, jobs, audit, …) |
| **R2 or S3 (optional)** | Large artifacts / evidence blobs when configured; otherwise local files |
| **GitHub** | Source code only — never enroll tokens or `hub.env` secrets |

**Do not** run Hub on a machine it also manages as an enrolled agent if you can avoid it (see unit comments / PRODUCT).

---

## 3. Operator auth (unchanged)

Hub still requires:

1. `CLAWCTL_LISTEN=<this-host-tailscale-ipv4>:<port>`
2. `CLAWCTL_OPERATOR_CAPABILITY_PREFIX=<your-domain>/cap/clawctl`
3. Tailscale ACL grants for `-view`, `-operate`, `-admin` to that destination
4. Live `tailscaled` ≥ 1.100.0 with LocalAPI reachable

Hub has **no production default port**. The `--listen` flag default is
`127.0.0.1:8770`, and Hub refuses that address before it opens the database.
The operator always passes `CLAWCTL_LISTEN` or `--listen`. `8787` in this
guide is only the conventional example.

These must all use the **same port** as `CLAWCTL_LISTEN`:

- the Tailscale grant `dst` port (`tcp:<port>`)
- `CLAWCTL_PUBLIC_URL`, which must be `http://<CLAWCTL_LISTEN>`
- the agent `--hub` URL
- a Cloudflare Tunnel origin, when you use one

Rejected listen targets (before DB open): `0.0.0.0`, loopback, non-Tailscale
IPs, hostnames. Hub does **not** read `X-Forwarded-For`, `X-Real-IP`,
`Forwarded`, or `Authorization` for operator identity.

Docker compose therefore uses **`network_mode: host`** and mounts
`/var/run/tailscale/tailscaled.sock`. If LocalAPI is unreachable from the
container UID, fix socket permissions or use `ops/install-hub.sh` on the host
instead of Docker for the Hub process.

Join a free Tailscale tailnet: install Tailscale on the VPS and on your
operator devices, then edit ACL grants as in [OPERATOR-AUTH.md](OPERATOR-AUTH.md) (replace the
example `dst` IP with your VPS Tailscale IP).

---

## 4. Cloudflare Tunnel — what it is for (and not)

| Use | Supported today? |
|---|---|
| Operator browses `https://hub.example.com` | **No** — Host must equal pinned Tailscale authority; WhoIs needs tailnet `RemoteAddr` |
| Agent check-in via public hostname → tunnel → `http://<CLAWCTL_LISTEN>` | **Experimental** — machine bearer routes do not pin Host; treat as optional |
| Cloudflare Access as operator IdP | **Not implemented** — do not invent OIDC in front without Hub code changes |

Tunnel origin must target the **same** address Hub listens on
(`http://100.x.y.z:8787` when 8787 is the port in `CLAWCTL_LISTEN`), because Hub refuses to bind loopback. The origin port is the listen port, not a fixed port.

Enable by adding `ops/docker/docker-compose.tunnel.yml` and setting
`CLOUDFLARE_TUNNEL_TOKEN` in `hub.env` (see
`ops/docker/cloudflared.yml.example`). Do not use a compose profile for this.
Compose v5 interpolates every service it loads, including profile-disabled
ones, so a required token in `docker-compose.yml` aborts `config` and `up`
even when the tunnel is not requested. Loading the override file is what
uses the tunnel; a missing token then fails at interpolation with
`CLOUDFLARE_TUNNEL_TOKEN` in the error. The default file does not read the
variable, so `docker compose config` works with no token set.

---

## 5. Storage split

| Store | Contents |
|---|---|
| **SQLite** (`CLAWCTL_DB`, volume `clawctl-data`) | Registry, jobs, audit, tickets, settings, and `object_blobs` digests — source of truth |
| **Local artifacts dir** (next to DB) | Working copy of artifact tarballs. Catalog and deployments read this directory. |
| **R2 or S3 (optional)** | Durable copy of large artifact and evidence blobs when `R2_*` or `S3_*` is complete. Hub hashes the bytes itself. Unset env keeps local files only. |
| **GitHub** | Code, Dockerfiles, docs — no secrets, no live DBs |
| **Agent bootstrap** (on the same volume) | Linux installers Hub serves at startup |

### Volume owner and agent bundles

Hub's writer lock refuses the SQLite directory unless the process uid owns it
and the mode is `0700` (no group or other permission bits). In this image that
uid is distroless `nonroot`, **65532**. A new named volume is root-owned, so
compose starts `hub-data-init` first (`busybox`, `restart: "no"`,
`depends_on` condition `service_completed_successfully`). The one-shot chowns
`clawctl-data` to `65532:65532`, sets mode `0700`, and exits before `hub` starts.

On startup Hub loads agent installers from
`<directory of CLAWCTL_DB>/agent-bootstrap/<version>/` and requires the Linux
`amd64` and `arm64` archives. `<version>` is the binary's `main.version`.
There is no flag or env var for a different directory. The image build arg
`CLAWCTL_VERSION` (default `dev`) is that version. The Dockerfile cross-compiles
`clawctl-agent` with `CGO_ENABLED=0` for both Linux architectures, packs them
with `ops/build-agent-bundles.sh`, and writes the release the same way
`ops/publish-agent-bundles.sh` does for a Linux-only publish:

```text
/var/lib/clawctl/agent-bootstrap/<CLAWCTL_VERSION>/
  clawctl-agent-bootstrap-linux-amd64.tar.gz
  clawctl-agent-bootstrap-linux-arm64.tar.gz
  SHA256SUMS
```

`hub-data-init` copies that tree from the image into the volume when the
version directory is absent, then sets the directory to owner 65532 and mode
`0700`. It does not replace a directory that is already there. Darwin and
Windows bundles are optional at startup and are not in the image.

The same files are also in the Hub image at
`/usr/local/share/clawctl/agent-bootstrap/<CLAWCTL_VERSION>/` so an image
inspection can see them. Hub does not read that path.

Pass a new `CLAWCTL_VERSION` when the agent bytes must change. Reusing a
version string leaves the already seeded release in place.

### Backup / restore (volume)

```bash
# Backup (Hub stopped or briefly quiet — prefer stop for consistency)
docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env stop hub
docker run --rm -v clawctl-data:/data -v "$PWD:/backup" busybox \
  tar czf /backup/clawctl-data-$(date -u +%Y%m%dT%H%M%SZ).tar.gz -C /data .
docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env start hub
```

Bare-metal installs still use the paths under `~/.local/share/clawctl/` and
`ops/upgrade-hub.sh` snapshots. Restore = replace volume contents / SQLite file
then start Hub; validate with `/healthz` and operator homepage HTTP 200.

---

## 6. One-time enrollment flow

Agents **outbound-push** check-ins; machines do not need inbound ports.
Self-report is not proof: the machine page timestamp is the receipt.

1. Operator opens `http://<CLAWCTL_LISTEN>/` on Tailscale. That address is the only operator UI.
2. Create an enrollment ticket in the UI. The ticket page shows one install command, for example:

   ```bash
   ./install-agent.sh --hub http://100.64.0.1:8787
   ```

   The one-time token is not embedded in that command. Paste it when the installer asks, or pass `--token-file` (mode `0600`).
3. On the target machine, run that command. The installer enrolls, starts the agent, and runs `clawctl-agent verify` until Hub has received a check-in.
4. Open the machine page. A check-in timestamp there is the evidence. The installer's own success line is not.

`--hub` accepts two forms:

| `--hub` | Who uses it |
|---|---|
| `http://<tailscale-ipv4>:<port>` | Default. Same address as the operator UI. |
| `https://<hostname>[:port]` | Experimental Cloudflare Tunnel for **agent check-in only**. Do not log into the operator UI with this URL. |

The Go agent also accepts a literal Tailscale IPv6 hub URL on `enroll`. The shell installers accept Tailscale IPv4 and https hostnames.

Evidence excerpts stay in SQLite. Bulky evidence uses the same Hub-hashed blob path as artifacts (`object_blobs`, kind `evidence`) when object storage is configured.

---

## 7. Quick commands

```bash
# Build image or fall back to make hub
./ops/docker/smoke-build.sh

# Run (host must already be on Tailscale)
cp ops/docker/hub.env.example ops/docker/hub.env
# edit CLAWCTL_LISTEN + CLAWCTL_OPERATOR_CAPABILITY_PREFIX
docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env up -d --build

# Health from any host that can reach the Tailscale IP
curl -fsS "http://${CLAWCTL_LISTEN}/healthz"   # expect: alive

# Optional tunnel. Fails at interpolation if CLOUDFLARE_TUNNEL_TOKEN is unset.
docker compose -f ops/docker/docker-compose.yml \
  -f ops/docker/docker-compose.tunnel.yml \
  --env-file ops/docker/hub.env up -d
```

Prebuilt, signed images: [RELEASE.md](RELEASE.md) (GHCR, cosign keyless, SBOM; run them with `ops/docker/docker-compose.ghcr.yml`).

Without Docker: `make hub` then `./ops/install-hub.sh --listen <tailscale-ip>:<port> --operator-capability-prefix …`. That is the systemd alternative. Hosted example: [DEPLOY-FLY.md](DEPLOY-FLY.md).

---

## Notifications

The Hub can send the daily report and disk-clean alerts through built-in Telegram and webhook channels. Secrets live only in the file named by `CLAWCTL_NOTIFY_ENV` / `--notify-env` (see `ops/notify.env.example`). They are never taken from flags or the process environment.

**Precedence**

1. If `CLAWCTL_NOTIFY_CMD` / `--notify-cmd` is set, Hub uses that command (legacy). Built-in channels in the env file are not used; the child still inherits `CLAWCTL_NOTIFY_ENV`, so `ops/notify-telegram.sh` keeps working.
2. Else if the env file defines at least one channel, Hub uses the built-in notifier.
3. Else notifications are not configured.

**Migration from notify-telegram.sh:** remove `CLAWCTL_NOTIFY_CMD` and keep `CLAWCTL_NOTIFY_ENV`. `ops/notify-telegram.sh` remains for `ops/deadman.sh` and Alertmanager install.

Check the pipes without sending a message:

```bash
clawctl-hub notify-check --notify-env /path/to/notify.env
```

Docker: add `-f ops/docker/docker-compose.notify.yml` and set `CLAWCTL_NOTIFY_ENV_HOST` to the host env file. It is mounted read-only at `/etc/clawctl/notify.env`, and the overlay sets `CLAWCTL_NOTIFY_ENV` to that path. The Hub container runs as uid 65532, so the file must be readable by that uid (`sudo chown 65532 notify.env && chmod 0600 notify.env`). An unreadable file stops the Hub at startup when the built-in notifier is in use. With a legacy notify command it only logs a warning.

---

## 8. Operator MCP and CLI

An agent on a tailnet node uses `clawctl-operator` (stdio MCP or `call`). It
calls the same `/v1/operator/*` API as the Web UI. Contract, grants, canary
phases, and rollback limits: [OPERATOR-AGENT.md](OPERATOR-AGENT.md). The
agent procedure is `skills/clawctl-operator/SKILL.md`.

```bash
make operator
export CLAWCTL_HUB_URL=http://100.x.y.z:8787   # same port as CLAWCTL_LISTEN; 8787 is the example
./build/clawctl-operator tools
./build/clawctl-operator mcp
```

The process must run on a node that holds the Tailscale grants for this Hub.
It sends no `Authorization` header and does not open the SQLite file.

---

## 9. Operator Terminal

Hub provides a browser-based web terminal to connected agents. By default, terminal sessions are bounded by two limits, configured via environment variables in `hub.env` (or Hub flags):

*   **Idle Timeout**: `CLAWCTL_TERMINAL_IDLE_TIMEOUT` (default `30m`). The session closes automatically if there is no operator input (keyboard, paste, or resize) from the page for this duration. Output from the machine or background heartbeats do not reset the timer (e.g., a session only running `tail -f` will still idle out). Bounded between `1m` and `24h`.
*   **Absolute Lifetime**: `CLAWCTL_TERMINAL_MAX_LIFETIME` (default `12h`). The session closes automatically when this duration is reached, measured from the moment the session was created in the ledger (`opened_at`), not from when the socket attaches. It cannot be extended by input or output. If a session is already past its lifetime when an operator connects, it closes immediately. Bounded between `1m` and `720h`.

There is no "disable" value; all sessions are always bounded. Empty values fall back to the defaults. Timeouts must be whole seconds and the idle timeout cannot exceed the max lifetime. A terminal closed by these limits gets its close reason written once to the session ledger (`agent_sessions.close_reason`, also returned as `close_reason` by the operator API): `終端閒置逾時，已自動關閉` (idle timeout) or `終端已達最長使用時間，已自動關閉` (lifetime reached). The Hub also logs one line, `terminal session closed: idle timeout …` / `… lifetime reached …`, with the session and machine IDs. The terminal page states both limits and the session's latest end time, warns two minutes before an idle close, and says why the terminal closed.

---

## 10. Still later

Done:

1. **Enrollment** — ticket page shows one install command. `--hub` is the Tailscale `http://100.x.y.z:<port>` address (the same port as `CLAWCTL_LISTEN`; `8787` is only the conventional example), or an experimental `https://<hostname>` tunnel URL for agent check-in only.
2. **R2 / S3** — when `R2_*` or `S3_*` is complete, Hub stores Hub-hashed blobs and keeps digests in SQLite. Partial config refuses to start. Both groups at once is an error.
3. **Operator MCP / CLI** — `clawctl-operator` reads fleet, jobs, deployments, software, and compliance, and writes enroll tickets, deployments, and profile assignments through the existing preview/apply API. A new deployment's first batch is one machine. After Hub marks that job `succeeded`, the driver pauses. The next batch opens only on an explicit Continue from the UI, CLI, or `rollout_expand`. Deployments already in flight before this hold keep the old auto-open behavior. Plain Continue refuses a failed batch. The deployment page's separate `skip failed batch` action records a reason; MCP does not expose it.

Not implemented:

4. **Cloudflare Access / OIDC** — Hub still does not trust `X-Forwarded-*` or `Authorization` for operator identity. Do not invent an operator login in front of the Tailscale listener.
5. **Deleting the local tarball after upload** — the artifacts directory remains the working copy. Remote serve is only the fallback when that file is missing and the SQLite row matches.
6. **Cloudflare Containers** — not implemented. A future advanced option would need tsnet (userspace Tailscale inside the Hub process), Litestream, and a Durable Object keep-alive. It is not a deploy path. Fly Machines, in [DEPLOY-FLY.md](DEPLOY-FLY.md), is the hosted example because a VM can run kernel-mode `tailscaled` and the SQLite process.

---

## 10. Security checklist

- [ ] `hub.env` mode `0600`, not committed
- [ ] `notify.env` mode `0600`, not committed; `CLAWCTL_NOTIFY_ENV` set (or legacy `CLAWCTL_NOTIFY_CMD`)
- [ ] No enroll tokens in git
- [ ] Tailscale grants limited to operator users / devices
- [ ] Tunnel token set only when `docker-compose.tunnel.yml` is used
- [ ] Hub host is not also an enrolled production agent (or is accepted deliberately)
- [ ] External deadman (`ops/deadman.sh` or healthchecks.io) runs **off** the Hub host

---

## 11. Hub install contract

The longer install diary this contract used to live in is a private working note and is not published here. The section below is what the repository test locks to `ops/install-hub.sh` and `ops/clawctl-hub.service`.

### 2.2 Hub

```bash
# 先照 docs/OPERATOR-AUTH.md 存好 Tailscale grant，再安裝：
./ops/install-hub.sh \
  --listen 100.x.x.x:8787 \
  --operator-capability-prefix example.com/cap/clawctl
```

⚠ **這裡刻意不列出它做了哪幾步。** 這一節原本就是那幾步的手抄本，
而手抄本會過期 —— unit 檔改了 `ExecStart`、Makefile 改了輸出目錄，
這段文字不會跟著紅。腳本會壞，文字只會過期。要知道它做什麼就去讀它，
那份註解跟程式碼在同一個檔案裡，不會各自漂走。

它會拒絕 root、先開 linger 再碰 `systemctl --user`（順序不能換，
理由在腳本裡）、擋掉「這台已經有一個 Hub 在跑」，
最後不只打 `/healthz`，也會打 operator 首頁確認 LocalAPI app capability 真能回 200。
`systemctl is-active` 不是判準，它只證明檔案放對位置；只有 healthz 綠也不能證明
管理者沒有被 policy 鎖在外面。這個首頁 probe 只驗 Hub 本機的 Tailscale principal；
外部 operator 工作站仍必須照 [OPERATOR-AUTH.md](OPERATOR-AUTH.md) §6 驗收。

裝完監聽你明示的 Tailscale literal IP；沒有 `--listen` 時腳本會嘗試用
`tailscale ip -4`，但 capability prefix 不猜。
資料庫在 `~/.local/share/clawctl/clawctl.sqlite`。CLI 的 `defaultDB()` 與 unit 的
explicit `--db` 必須指向同一處，並由跨檔測試鎖住；service 明寫參數是為了讓
`hub.env`／user-manager ambient 的 `CLAWCTL_DB` 不能把 live writer 導離 upgrade
script 實際 snapshot／restore 的 ledger。

⚠ unit 裡的 `127.0.0.1:8787` 只是 env 檔缺失時的 fail-closed placeholder；新版
binary 會在開 DB 前拒絕 loopback、hostname、`0.0.0.0`、LAN/public IP。UI/API 逐 request
使用 Tailscale `WhoIsForIP` 與 `view`／`operate`／`admin` app capability；完整契約見
[OPERATOR-AUTH.md](OPERATOR-AUTH.md)。

> ⚠ 直接跑 `clawctl-hub`（不透過 unit）時 `--listen` 的預設是 `127.0.0.1:8770`，
> 跟 unit 的 8787 不同；兩者現在都會被 operator auth 的 tailnet-IP validation 擋掉，
> 不再能安靜啟動第二個 writer。正式啟動必須明示 tailnet listener 與 capability prefix。
