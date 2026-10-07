# Autopilot: local admin, public HTTPS, keyed installer

The recommended OSS path is an Intune Autopilot-like flow: **register a device → download its keyed package → install → check in over HTTPS**. You run the Hub on Linux; endpoints need no tailnet membership or inbound control ports. This is the project's enrollment workflow, not Microsoft's Windows Autopilot service. The keyed package currently supports Linux amd64 and arm64.

```text
Operator browser -- HTTPS / session cookie --+
                                             v
Internet :80/:443 --> Caddy (TLS) --> hub:8787 --> SQLite + artifacts
                                             ^
Linux installer -- one-time enroll token -----+
Linux agent ----- HTTPS / machine bearer -----+
Browser terminal ===== WSS via Hub ===== agent outbound link
```

| Plane | Authentication |
|---|---|
| Operator Web UI | Local admin account, then a session cookie |
| Agent enrollment | One-time enrollment token exchanged for a long-lived per-machine bearer |
| Agent check-in / work | Machine bearer; it cannot authorize operator requests |
| Terminal | Operator session for the browser WSS connection; machine bearer for the agent's outbound link |
| Independent verifier | Separate verifier bearer; never a machine or operator credential |

## Quick start A: Docker + Caddy

Use a Linux server with Docker Engine and the Compose plugin, a domain you control, and this repository checked out. See [fresh VM Docker setup](DEPLOY-OSS.md#fresh-vm-setup-docker-path) for host prerequisites; Tailscale is unnecessary for this pack.

1. Point a DNS **A record** such as `hub.example.com` to the server's public IPv4 address. Open inbound TCP **80 and 443** in the cloud firewall and host firewall. Any AAAA record must also reach this server. Keep these ports available for Caddy and certificate renewal.
2. From the repository root, prepare the configuration:

   ```bash
   cp ops/docker/autopilot.env.example ops/docker/autopilot.env
   chmod 600 ops/docker/autopilot.env
   git rev-parse --short HEAD
   ```

   Edit `autopilot.env`: set `CLAWCTL_PUBLIC_HOST=hub.example.com` and set `CLAWCTL_VERSION` to the literal Git SHA printed above. Env files do not execute shell substitutions. Use a new version for each build: the initializer never overwrites existing bundles for the same version. Leave `CLAWCTL_SETUP_CODE` commented out to generate a code.
3. Build and start the **standalone** pack:

   ```bash
   docker compose -p clawctl-autopilot --env-file ops/docker/autopilot.env -f ops/docker/docker-compose.autopilot.yml up -d --build
   docker compose -p clawctl-autopilot --env-file ops/docker/autopilot.env -f ops/docker/docker-compose.autopilot.yml logs hub
   ```

   Read the first-run setup URL and code from the Hub log. Visit `https://hub.example.com/setup`, enter the code, and create the admin (username: 3–64 letters, digits, dots, underscores or hyphens; password: 12–256 bytes). Setup closes after the first account exists. Later visits use `/login`. If you supplied a setup code, use that value: it is not printed in the log. Protect access to the logs.
4. In the Web console's enrollment workflow, register a machine, confirm creation, and download the **keyed Linux installer** for its architecture from the token result page. That response is the one-time secret delivery: keep the token/package private. Downloading does not redeem the ticket or extend its TTL. If the result is lost, revoke the pending ticket and issue a fresh one.
5. Transfer the archive privately to that Linux endpoint. As the **non-root account that will run the agent**, with sudo available:

   ```bash
   mkdir -m 700 clawctl-enrollment
   tar xzf /path/to/clawctl-agent-MACHINE-linux-amd64.tar.gz -C clawctl-enrollment
   cd clawctl-enrollment
   ./install-agent.sh
   ```

   Use the `arm64` archive for ARM. The archive extracts files directly into the chosen directory. The installer reads adjacent `hub-url` and mode-0600 `enroll-token` files. HTTPS automatically skips Tailscale installation/join; `--no-tailscale` explicitly skips it too (and can be used with an already reachable HTTP tailnet URL). Explicit `--hub` and `--token-file` flags override embedded values. The endpoint needs systemd, systemd-logind, sudo, and outbound HTTPS; the installer may install additional prerequisites. After success, delete the downloaded archive and any transfer copies. Open the machine page and verify a new Hub-received check-in and reported identity; installer exit alone is not fleet evidence.

The initializer prepares the data volume as UID 65532, mode 0700, and seeds versioned agent bundles. Hub has **no published port** and no Tailscale socket. Caddy owns ports 80/443, certificate storage (`caddy_data`), and configuration storage (`caddy_config`). Its [automatic HTTPS](https://caddyserver.com/docs/automatic-https) obtains and renews certificates; its [reverse_proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy) preserves Host for this HTTP upstream and supports WebSocket upgrades by default, including terminal WSS. No extra upgrade stanza is required. Forwarded headers do not supply Hub identity.

Keep the Compose project name and volumes stable across upgrades. Back up the Hub ledger/artifacts and Caddy state; `down -v` deletes volumes. See [storage and backup](DEPLOY-OSS.md#5-storage-split); this project's data volume is normally `clawctl-autopilot_clawctl-data`, unlike the advanced pack's explicitly named `clawctl-data`. The notify override can be layered on this pack as described in `autopilot.env.example`; keep `-p clawctl-autopilot` because the legacy override declares a different project name. Its mode-0600 notify file must be owned by UID 65532. R2/S3 settings use the existing [OSS storage configuration](DEPLOY-OSS.md#5-storage-split).

## Quick start B: bare Linux / systemd with your own reverse proxy

Use a separate Hub host and a non-root service account. The existing `ops/install-hub.sh` is the advanced Tailscale installer; for local mode, prepare the binary and user service directly. From the repository root, with Go and make installed:

```bash
version=$(git rev-parse --short HEAD)
make hub agent-bundles VERSION="$version"
install -d -m 755 "$HOME/.local/bin" "$HOME/.config/systemd/user"
install -d -m 700 "$HOME/.config/clawctl" "$HOME/.local/share/clawctl"
install -m 755 build/clawctl-hub "$HOME/.local/bin/clawctl-hub"
bash ops/publish-agent-bundles.sh --version "$version" --source-dir build --state-dir "$HOME/.local/share/clawctl"
install -m 644 ops/clawctl-hub.service "$HOME/.config/systemd/user/clawctl-hub.service"
```

Create `~/.config/clawctl/hub.env`, mode 0600:

```dotenv
CLAWCTL_AUTH_MODE=local
CLAWCTL_LISTEN=127.0.0.1:8787
CLAWCTL_PUBLIC_URL=https://hub.example.com
```

The supplied user unit pins the database to `~/.local/share/clawctl/clawctl.sqlite`; a custom unit can instead use `CLAWCTL_DB` or `--db`. The existing unit's Tailscale-only comments describe its default mode; the explicit local mode above permits loopback. Enable lingering and start:

```bash
sudo loginctl enable-linger "$(id -un)"
systemctl --user daemon-reload
systemctl --user enable --now clawctl-hub
journalctl --user -u clawctl-hub -n 50 --no-pager
```

Terminate TLS at your own proxy on the same host and forward to `http://127.0.0.1:8787`. The proxy **must pass Host unchanged** (`hub.example.com`) and support **WebSocket upgrade**. For example, use this Caddy site block:

```caddyfile
hub.example.com {
    reverse_proxy 127.0.0.1:8787
}
```

Do not add forwarded identity semantics. Follow steps 3–5 above for setup and enrollment after HTTPS is reachable.

## Headless admin and lost-password recovery

Both commands require an **existing initialized Hub database**, explicit `--db` and `--username`, and read the password from **stdin**, not a password flag. Run as the database owner. Stop the Hub for this maintenance workflow, then restart it. `bootstrap-admin` creates only the first account; `reset-admin-password` changes the existing admin password, clears lockout, and revokes that account's sessions.

For the bare user service (replace the command with `reset-admin-password` for recovery):

```bash
systemctl --user stop clawctl-hub
read -r -s -p 'New admin password: ' admin_password; printf '\n'
printf '%s\n' "$admin_password" | "$HOME/.local/bin/clawctl-hub" bootstrap-admin \
  --db "$HOME/.local/share/clawctl/clawctl.sqlite" --username admin
unset admin_password
systemctl --user start clawctl-hub
```

For Docker, after Hub has initialized its database:

```bash
docker compose -p clawctl-autopilot --env-file ops/docker/autopilot.env -f ops/docker/docker-compose.autopilot.yml stop hub
read -r -s -p 'New admin password: ' admin_password; printf '\n'
printf '%s\n' "$admin_password" | docker compose -p clawctl-autopilot --env-file ops/docker/autopilot.env \
  -f ops/docker/docker-compose.autopilot.yml run --rm --no-deps -T hub bootstrap-admin \
  --db /var/lib/clawctl/clawctl.sqlite --username admin
unset admin_password
docker compose -p clawctl-autopilot --env-file ops/docker/autopilot.env -f ops/docker/docker-compose.autopilot.yml start hub
```

Use the account's existing username when resetting. Headless bootstrap needs no setup code because it requires local database access. A generated setup code is held only as a digest in process memory, rotates on restart, and works until the first admin is created. An explicitly configured code stays the same across restarts until you change it.

## Environment and flags

| Setting | Actual behavior |
|---|---|
| `CLAWCTL_AUTH_MODE` / `--auth-mode` | `tailscale` (binary default), `local`, or `both`. The Autopilot pack explicitly selects `local`. A flag overrides the env value. |
| `CLAWCTL_LISTEN` / `--listen` | `tailscale`: canonical literal Tailscale node IP and nonzero port only. `local` / `both`: canonical literal IP and nonzero port, including wildcard, loopback, LAN or public IP; no hostname. IPv6 uses brackets. Flag default is `127.0.0.1:8770`; the examples explicitly use 8787. |
| `CLAWCTL_PUBLIC_URL` | Required in `local` / `both`: HTTPS origin, or HTTP only on localhost/loopback for development with a loopback listen IP (127.0.0.0/8 or ::1); startup refuses HTTP with a wildcard, LAN, public or Tailscale listen IP. No userinfo, path (except a trailing slash), query or fragment. Pins the accepted public Host and determines secure cookies. In `tailscale`, optional but must match `http://<listen>`. |
| `CLAWCTL_SETUP_CODE` | Optional first-run code in `local` / `both`; omit to generate and log it. If supplied, requires at least 16 characters after removing whitespace/hyphens; comparisons are case-insensitive. Empty is invalid before the first admin exists. |
| `CLAWCTL_TRUSTED_PROXIES` | Comma/space separated proxy CIDRs or bare IPv4/IPv6 addresses (/32 or /128). Default empty ignores forwarded headers. Invalid entries and prefixes broader than IPv4 /8 or IPv6 /16 refuse startup. |
| `CLAWCTL_CLIENT_IP_HEADER` | `X-Forwarded-For` (default) or `Fly-Client-IP`; other values refuse startup. |
| `CLAWCTL_DOCKER_SUBNET` | Compose bridge subnet and Hub trusted proxies; default `172.31.87.0/24`. Override if it collides. |
| `CLAWCTL_DB` / `--db` | SQLite path; its parent must be private and owned by the service user. The Docker image's explicit `--db` pins `/var/lib/clawctl/clawctl.sqlite`. |
| `CLAWCTL_PUBLIC_HOST` | Compose/Caddy input only: public DNS hostname used to construct `CLAWCTL_PUBLIC_URL`. |
| `CLAWCTL_VERSION` | Docker build arg/image tag and agent-bundle version, not a Hub runtime setting. Use a unique Git SHA per build. |

## Client IP and reverse proxies

The Docker+Caddy pack configures this for you: its fixed bridge subnet is both the Compose IPAM subnet and `CLAWCTL_TRUSTED_PROXIES`, and the Hub has no published port. Only containers on that network can reach it. Override `CLAWCTL_DOCKER_SUBNET` if it collides. Caddy v2.5+ appends the real client to `X-Forwarded-For` and ignores client-sent forwarded headers unless its own `trusted_proxies` is configured ([Caddy documentation](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)).

For a bare-metal proxy on the same host, bind Hub to loopback and set:

```env
CLAWCTL_TRUSTED_PROXIES=127.0.0.1,::1
CLAWCTL_CLIENT_IP_HEADER=X-Forwarded-For
```

Trust only your proxy's actual source addresses and ensure it sanitizes forwarded headers. Empty trust uses the TCP peer; without configuration every proxied client shares one IP. Trusted XFF chains are walked right-to-left (at most 32 entries), skipping trusted hops. The first untrusted IP wins; malformed entries fall back to the last trusted hop, and all-trusted chains fall back to the peer. `Fly-Client-IP` must contain exactly one header value and one IP. `Forwarded` and `X-Real-IP` are never used. This affects account limiter, lockout, audit/session source addresses and enrollment limits; operator identity, Host checks and WhoIs continue to use their existing boundary.

For Fly public HTTP mode use:

```env
CLAWCTL_TRUSTED_PROXIES=172.16.0.0/12
CLAWCTL_CLIENT_IP_HEADER=Fly-Client-IP
```

On Fly the rightmost `X-Forwarded-For` entry is the app's own edge address. Fly staff describe fly-proxy egress as `172.16.0.0/16`, but `172.19.x` has been observed, hence `/12`. Fly 6PN private networking is IPv6 `fdaa::/16` and is not trusted by this setting. The full Fly public-mode pack comes in a later PR; the existing Fly pack remains Tailscale-only.

## Security model and current limits

- Public HTTPS exposes the login surface to the Internet. Tailscale-only deployments instead keep it privately reachable within the mesh. TLS protects transport; it does not make the public service invisible.
- Enrollment tickets have a **2-hour TTL** and are **one-time**. A keyed package is password-equivalent until its ticket is used, revoked or expires. It embeds the Hub URL and token, and is delivered with `Cache-Control: no-store`. The extracted token file is removed after successful enrollment; archived or copied tokens are not erased remotely.
- The resulting machine bearer is long-lived. If stolen, retire the compromised machine credential and re-enroll with a new ticket; `--reenroll` alone does not revoke the old Hub credential. See [Moving agents](MOVE-AGENTS.md).
- Passwords use **Argon2id** and a **12-character minimum** (enforced as 12 bytes). **5 consecutive failures** lock only that **(account, client IP)** pair for **15 minutes**. An attacker can lock out only the IP they come from; the admin from another IP is unaffected. Success resets only its pair; password reset clears all pairs. Rows older than 24 hours are pruned opportunistically. At least 50 failures across IPs in one rolling hour produce one audit signal per hour, without blocking the account. Distributed guessing is slowed by Argon2id, per-IP buckets and the password minimum; the stacked MFA PR adds a required second factor.
- Login/setup POSTs share a per-client-IP token bucket: **10/minute**, burst **5**. Enrollment uses **30/minute**, burst **30**. Behind a proxy, configure trusted proxies or every client shares the proxy IP, bucket and lockout pair.
- Sessions expire after **12 hours idle** or **7 days absolute**. Cookies are HttpOnly and SameSite=Lax; HTTPS uses a Secure `__Host-` cookie. HTTPS mode emits HSTS. Go `CrossOriginProtection` checks browser writes; Host is pinned to `PUBLIC_URL`. `/metrics` requires an operator session in local mode; `/healthz` remains a content-free public probe.
- There is **no MFA yet** (TOTP planned) and currently **one admin account** (multi-user/roles planned). Local admin authorizes all three operator capabilities. Operator **CLI/MCP transport remains Tailscale-mode only**; operator API tokens are a follow-up. The local maintenance commands above are separate from that HTTP transport.

## Advanced: private mesh + WhoIs

Keep the existing [Tailscale operator contract](OPERATOR-AUTH.md) and [Fly pack](DEPLOY-FLY.md) for a private mesh deployment. `both` mode accepts local sessions first and can fall back to WhoIs when no session cookie is present. An invalid session cookie does not fall back. WhoIs requires a **literal Tailscale listener**, a reachable LocalAPI, and the configured capability prefix/grants. With a wildcard or loopback listener, `both` has no WhoIs fallback. Public account routes require the public Host even in `both`; direct tailnet operator routes additionally accept the configured Tailscale listener authority. A proxy cannot manufacture a tailnet identity.

Hub-driven Tailscale provisioning, Cloudflare Tunnel automation, and Fly public mode are out of scope for this pass. The Fly pack remains Tailscale-only; public Fly mode is a follow-up.
