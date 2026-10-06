# Open-source deploy guide — clawctl-hub (OSS / any cloud)

Status: **Milestone 4 — Hub-enforced canary hold**. This document describes how to run Hub on a
generic Linux host with Docker + Cloudflare Tunnel + Tailscale. It does **not**
change the operator auth model.

Related: [`ops/docker/README.md`](../ops/docker/README.md), [`ops/install-hub.sh`](../ops/install-hub.sh),
operator auth contract (Tailscale app capabilities — see `ops/clawctl-hub.service` comments and install-hub.sh).
Full OPERATOR-AUTH.md / PRODUCT.md live in the complete documentation set.

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
                                    │  listen 100.x.y.z:8787
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
| **Any Linux + Docker** | Runs Hub; Oracle / AWS / Hetzner / home lab all fine |
| **Tailscale** | Operator identity + reachability; Hub binds its Tailscale IP |
| **Cloudflare Tunnel** | Optional public ingress for **agent** experiments |
| **SQLite volume** | Fleet state (machines, jobs, audit, …) |
| **R2 or S3 (optional)** | Large artifacts / evidence blobs when configured; otherwise local files |
| **GitHub** | Source code only — never enroll tokens or `hub.env` secrets |

**Do not** run Hub on a machine it also manages as an enrolled agent if you can avoid it (see unit comments / PRODUCT). **Do not** run Hub on the Grok Bot build box.

---

## 3. Operator auth (unchanged)

Hub still requires:

1. `CLAWCTL_LISTEN=<this-host-tailscale-ipv4>:8787`
2. `CLAWCTL_OPERATOR_CAPABILITY_PREFIX=<your-domain>/cap/clawctl`
3. Tailscale ACL grants for `-view`, `-operate`, `-admin` to that destination
4. Live `tailscaled` ≥ 1.100.0 with LocalAPI reachable

Rejected listen targets (before DB open): `0.0.0.0`, loopback, non-Tailscale
IPs, hostnames. Hub does **not** read `X-Forwarded-For`, `X-Real-IP`,
`Forwarded`, or `Authorization` for operator identity.

Docker compose therefore uses **`network_mode: host`** and mounts
`/var/run/tailscale/tailscaled.sock`. If LocalAPI is unreachable from the
container UID, fix socket permissions or use `ops/install-hub.sh` on the host
instead of Docker for the Hub process.

Join a free Tailscale tailnet: install Tailscale on the VPS and on your
operator devices, then edit ACL grants as in OPERATOR-AUTH.md (replace the
example `dst` IP with your VPS Tailscale IP).

---

## 4. Cloudflare Tunnel — what it is for (and not)

| Use | Supported today? |
|---|---|
| Operator browses `https://hub.example.com` | **No** — Host must equal pinned Tailscale authority; WhoIs needs tailnet `RemoteAddr` |
| Agent check-in via public hostname → tunnel → `http://<CLAWCTL_LISTEN>` | **Experimental** — machine bearer routes do not pin Host; treat as optional |
| Cloudflare Access as operator IdP | **Not implemented** — do not invent OIDC in front without Hub code changes |

Tunnel origin must target the **same** address Hub listens on
(`http://100.x.y.z:8787`), because Hub refuses to bind loopback.

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
docker run --rm -v clawctl-data:/data -v "$PWD:/backup" distroless 2>/dev/null || true
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

Without Docker: `make hub` then `./ops/install-hub.sh --listen … --operator-capability-prefix …`.

---

## 8. Operator MCP and CLI

An agent on a tailnet node uses `clawctl-operator` (stdio MCP or `call`). It
calls the same `/v1/operator/*` API as the Web UI. Contract, grants, canary
phases, and rollback limits: [OPERATOR-AGENT.md](OPERATOR-AGENT.md). The
agent procedure is `skills/clawctl-operator/SKILL.md`.

```bash
make operator
export CLAWCTL_HUB_URL=http://100.x.y.z:8787
./build/clawctl-operator tools
./build/clawctl-operator mcp
```

The process must run on a node that holds the Tailscale grants for this Hub.
It sends no `Authorization` header and does not open the SQLite file.

---

## 9. Still later

Done:

1. **Enrollment** — ticket page shows one install command. `--hub` is the Tailscale `http://100.x:8787` address, or an experimental `https://<hostname>` tunnel URL for agent check-in only.
2. **R2 / S3** — when `R2_*` or `S3_*` is complete, Hub stores Hub-hashed blobs and keeps digests in SQLite. Partial config refuses to start. Both groups at once is an error.
3. **Operator MCP / CLI** — `clawctl-operator` reads fleet, jobs, deployments, software, and compliance, and writes enroll tickets, deployments, and profile assignments through the existing preview/apply API. A new deployment's first batch is one machine. After Hub marks that job `succeeded`, the driver pauses. The next batch opens only on an explicit Continue from the UI, CLI, or `rollout_expand`. Deployments already in flight before this hold keep the old auto-open behavior. Plain Continue refuses a failed batch. The deployment page's separate `skip failed batch` action records a reason; MCP does not expose it.

Not implemented:

4. **Cloudflare Access / OIDC** — Hub still does not trust `X-Forwarded-*` or `Authorization` for operator identity. Do not invent an operator login in front of the Tailscale listener.
5. **Deleting the local tarball after upload** — the artifacts directory remains the working copy. Remote serve is only the fallback when that file is missing and the SQLite row matches.

---

## 10. Security checklist

- [ ] `hub.env` mode `0600`, not committed
- [ ] No enroll tokens in git
- [ ] Tailscale grants limited to operator users / devices
- [ ] Tunnel token set only when `docker-compose.tunnel.yml` is used
- [ ] Hub host is not also an enrolled production agent (or is accepted deliberately)
- [ ] External deadman (`ops/deadman.sh` or healthchecks.io) runs **off** the Hub host
