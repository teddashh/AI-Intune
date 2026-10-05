# Open-source deploy guide — clawctl-hub (OSS / any cloud)

Status: **Milestone 1 packaging**. This document describes how to run Hub on a
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
| **R2 (future)** | Large artifacts / evidence blobs — env scaffolded, code TODO |
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

Enable with compose profile `tunnel` and `CLOUDFLARE_TUNNEL_TOKEN` (see
`ops/docker/cloudflared.yml.example`).

---

## 5. Storage split

| Store | Contents |
|---|---|
| **SQLite** (`CLAWCTL_DB`, volume `clawctl-data`) | Registry, jobs, audit, tickets, settings — source of truth |
| **Local artifacts dir** (next to DB) | Packaged agent bundles / artifact blobs Hub already keeps on disk |
| **R2 (planned)** | Large evidence / artifact objects; env vars `R2_*` scaffolded in `hub.env.example` — **Milestone 2 wiring** |
| **GitHub** | Code, Dockerfiles, docs — no secrets, no live DBs |

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

1. Operator opens `http://<CLAWCTL_LISTEN>/` (Tailscale).
2. Create an enrollment ticket in the UI (or `clawctl-hub enroll-token <name>` with discovery).
3. On the target machine, run the installer with the one-time token and Hub URL
   (`http://<CLAWCTL_LISTEN>` today; public tunnel URL only for experiments).
4. Agent redeems the ticket, stores machine bearer, then periodic check-in /
   observation push.
5. Confirm the machine appears in Machines with fresh check-in evidence.

Details: PRODUCT.md, install scripts under `ops/install-agent*.sh`.

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

# Optional tunnel profile
docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env --profile tunnel up -d
```

Without Docker: `make hub` then `./ops/install-hub.sh --listen … --operator-capability-prefix …`.

---

## 8. Milestone 2 (next)

Not in this packaging milestone:

1. **Enrollment UX** — polish ticket create → copy-paste install command for Docker/public URL cases; document agent `hub_url` choices (Tailscale vs tunnel).
2. **R2 wiring** — Hub reads `R2_*`, uploads large artifacts/evidence, keeps SQLite references; backup story for objects + volume.
3. **Optional operator path behind Access** — only if Hub gains an explicit trusted-proxy / OIDC mode (today: do not fake it with headers).

---

## 9. Security checklist

- [ ] `hub.env` mode `0600`, not committed
- [ ] No enroll tokens in git
- [ ] Tailscale grants limited to operator users / devices
- [ ] Tunnel token only if profile `tunnel` is used
- [ ] Hub host is not also an enrolled production agent (or is accepted deliberately)
- [ ] External deadman (`ops/deadman.sh` or healthchecks.io) runs **off** the Hub host
