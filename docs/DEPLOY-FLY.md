# Deploy clawctl-hub on Fly.io

Run one always-on Hub with public HTTPS, a local admin, required authenticator MFA, and keyed Linux installers. Fly terminates TLS and supports the Hub's WebSocket connections. Tailscale is unnecessary for the recommended path. The same image also supports the [advanced Tailscale-only path](#advanced-tailscale-only-hub-on-fly).

## Autopilot on Fly (recommended)

Start with this repository checked out and flyctl installed (`fly` or `flyctl` on PATH). You do not need Go installed locally. The simple path is `deploy.sh`: authenticate with `fly auth login` or a Fly org token in `FLY_API_TOKEN`, then run from the repository root:

```sh
ops/fly/deploy.sh --org <your-org> --app <your-app-name> --region iad --dry-run
ops/fly/deploy.sh --org <your-org> --app <your-app-name> --region iad
# Optional: add --secrets-file ~/clawctl-fly/secrets.env (regular file, mode 0600).
```

The flags also accept `FLY_ORG`, `FLY_APP`, `FLY_REGION` (default `iad`), `FLY_SECRETS_FILE`, and `FLY_PUBLIC_URL` (`--public-url https://hub.example.com` for custom-domain checks). Dry-run prints the plan without calling Fly or making network requests. The script copies the config outside the repository, creates only missing resources, deploys with `--yes --ha=false`, scales to one 1024mb Machine, and waits up to about three minutes for HTTPS `/healthz`. It keeps the 3GB `clawctl_data` volume and never allocates a dedicated IPv4 or creates Postgres. R2 and Telegram are optional; unset `R2_*` / `LITESTREAM_*` is valid. The caller's secrets file is retained.

flyctl 0.4.115 defaults to Depot. If deployment fails with Depot, handshake (such as `authentication handshake failed: EOF`), or `list workers` output, the script retries once with `--depot=false --yes`. Classic builds can create a running `fly-builder-*` app. The script compares app inventories before and after that retry and destroys only new builder apps, preserving pre-existing builders. Failed teardown warns with the builder name and exits non-zero; inspect and remove that specific builder after confirming its identity.

When `/setup` returns 200, the script prints a generated first-run setup code once on your terminal. Protect terminal and log access. Once an admin exists, `/setup` returns 404 and the script never prints an old code. Finish setup in the browser or use the non-browser helper:

```sh
# Save the first-run code privately in this file using a terminal editor.
install -m 600 /dev/null ~/clawctl-fly/setup-code
nano ~/clawctl-fly/setup-code
ops/fly/setup-admin.sh --app <your-app-name> --username <your-admin-name> \
  --setup-code-file ~/clawctl-fly/setup-code --out ~/clawctl-fly/admin-credentials.json
```

Use `--url https://hub.example.com` for a custom origin. `--setup-code` is also accepted, but a file avoids shell history and process arguments. The helper generates a random password, enrolls RFC 6238 SHA1 TOTP (six digits, 30 seconds), and saves username, password, TOTP secret, and ten recovery codes to a new mode-0600 file. It refuses overwrites and prints only the file path and enrollment status. It uses one keep-alive HTTPS connection: MFA is bound to the full client IP, so opening a new TCP connection for each request can fail when egress IP rotates. First-run setup creates `__Host-clawctl_session` directly; it does not use `hub_pending_mfa`.

To remove a disposable app, preview first, then type the exact app name when prompted (piped input also works). Destroy never deploys:

```sh
ops/fly/deploy.sh --app <your-app-name> --destroy --dry-run
ops/fly/deploy.sh --app <your-app-name> --destroy
```

Destroy removes the app's machines, its `clawctl_data` volumes, and the app. It is irreversible. The manual steps below are the expanded form of the deploy path.

**Only these account setup steps require Ted's / your own accounts:**

- **Required: Fly signup**, the payment method Fly requires, and `fly auth login` in step 1.
- **Optional but recommended: Cloudflare R2 enablement, bucket creation, and the first bucket-scoped API token.** Follow [Cloudflare R2 setup](#cloudflare-r2-setup-optional). Hub runs without R2; leave every `R2_*` / `LITESTREAM_*` setting unset in that case.

The remaining deployment steps are commands and Hub setup. Telegram is optional; use existing bot/chat credentials if you already have them.

### 1. Install flyctl and log in

```sh
curl -L https://fly.io/install.sh | sh
export PATH="$HOME/.fly/bin:$PATH"
fly auth login
```

### 2. Prepare the app and volume

Copy the config **out of the Git tree** so edits do not dirty the build version:

```sh
mkdir -p ~/clawctl-fly
cp ops/fly/fly.toml ~/clawctl-fly/fly.toml
```

Edit `app` to a unique name and `primary_region` to your preferred region (for example `iad`). Replace `<your-app-name>` and `<your-region>` below with those values. Run from the repository root:

```sh
fly apps create <your-app-name> --org <your-org>
fly volumes create clawctl_data --region <your-region> --size 3 -a <your-app-name> --yes
```

This config selects `CLAWCTL_AUTH_MODE=local`, port 8787, public HTTP service with forced HTTPS, and one always-on Machine. No capability prefix or Tailscale auth key is needed. The entrypoint defaults `CLAWCTL_PUBLIC_URL` to `https://<your-app-name>.fly.dev` from `FLY_APP_NAME` and logs that URL. A configured URL must be HTTPS. `both` is refused: a public wildcard listener cannot provide the literal Tailscale listener required for WhoIs. Use either this pack or the private pack.

### 3. Stage optional secrets

If you are running without backups or notifications and want a generated setup code, skip this step. Otherwise create a mode-0600 file outside the repository:

```sh
install -m 600 /dev/null ~/clawctl-fly/secrets.env
nano ~/clawctl-fly/secrets.env
```

Add only the settings you intend to use, one `NAME=value` per line (no quotes or spaces). Replace the placeholders; omit unused groups entirely:

```dotenv
R2_ACCOUNT_ID=REPLACE-ME
R2_ACCESS_KEY_ID=REPLACE-ME
R2_SECRET_ACCESS_KEY=REPLACE-ME
R2_BUCKET=REPLACE-ME
# Optional notifications (both values required):
# TELEGRAM_BOT_TOKEN=REPLACE-ME
# TELEGRAM_CHAT_ID=REPLACE-ME
# Optional explicit first-run code, at least 16 characters:
# CLAWCTL_SETUP_CODE=REPLACE-WITH-A-LONG-PRIVATE-CODE
# Optional fail-closed backups:
# LITESTREAM_REQUIRED=1
# Optional off-host daily-report deadman switch:
# CLAWCTL_REPORT_PING_URL=https://hc-ping.com/REPLACE-ME
```

`R2_ENDPOINT` can replace `R2_ACCOUNT_ID`. The complete R2 group enables both Litestream SQLite replication and Hub artifact storage. Partial replication settings refuse startup. Keep setup codes and credentials private; pass the file through stdin:

```sh
fly secrets import --stage -a <your-app-name> < ~/clawctl-fly/secrets.env
shred -u ~/clawctl-fly/secrets.env
```

### 4. Deploy and create your admin

From the repository root:

```sh
fly deploy . \
  --config ~/clawctl-fly/fly.toml \
  --dockerfile ops/docker/Dockerfile \
  --build-target hub-fly \
  --build-arg CLAWCTL_VERSION="$(git rev-parse --short HEAD)" \
  --ha=false --yes
fly scale count 1 -a <your-app-name> --yes
fly logs -a <your-app-name>
```

Read the generated first-run setup code from the Hub log; protect log access. Press Ctrl-C to stop following logs. If you supplied `CLAWCTL_SETUP_CODE`, use that value instead: it is not printed. Open **`https://<your-app-name>.fly.dev/setup`**, enter the code, and create your admin (username: 3–64 letters, digits, dots, underscores or hyphens; password: 12–256 bytes). Add the displayed TOTP secret to your authenticator, confirm a six-digit code, and save the one-time recovery codes. MFA is required by default. Setup closes after the first admin exists; subsequent sign-ins use `/login`.

Account security can regenerate ten recovery codes with the current password and an unused authenticator code; recovery codes are not accepted. All previous codes are invalidated. Failures count toward the shared account/client lockout. New codes are shown once, and the regeneration audit contains no codes. Alternatively, stop the Hub and run `clawctl-hub regenerate-recovery-codes --db PATH --username U`, then restart it; the host command requires MFA enabled and prints one code per line.

### 5. Optional custom domain — choose before enrolling machines

```sh
fly certs add hub.example.com -a <your-app-name>
fly certs show hub.example.com -a <your-app-name>
```

Configure your domain's DNS using the CNAME or A+AAAA records reported by `fly certs show`. Once the certificate and DNS are ready:

```sh
fly secrets set CLAWCTL_PUBLIC_URL=https://hub.example.com -a <your-app-name>
```

For a custom domain, pass `--public-url https://hub.example.com` (or set `FLY_PUBLIC_URL`) to check that origin and show its setup URL. The URL must be HTTPS with a DNS host, an optional port, and no path, query, fragment, or userinfo; a trailing slash is allowed. This selects the checks URL; configure `CLAWCTL_PUBLIC_URL` separately. Without an explicit URL, a fly.dev setup response of HTTP 421 reports the custom domain and leaves the successful deployment intact.

Hub pins the operator Host to `CLAWCTL_PUBLIC_URL`. `/` and `/login` return 421 for a wrong Host. `/healthz` intentionally does not enforce Host so Fly's probe works; it is not a Host validation test. Never allocate a dedicated IPv4. After switching domains, the `fly.dev` URL stops serving the operator UI. Open `https://hub.example.com/login` (or `/setup` if no admin exists). Agents enrolled with the old `hub-url` must re-enroll against the new URL; choose the domain before enrolling machines. See [Moving agents](MOVE-AGENTS.md).

### 6. Enroll your first machine

In the Hub Web console, register a machine and download its **keyed Linux installer** for amd64 or arm64 from the token result page; the download page offers both architectures. The package contains a one-time enrollment token and the public Hub URL; transfer it privately. If lost, revoke the pending ticket and issue a new one.

On the endpoint, as the non-root account that will run the agent, with sudo available:

```sh
mkdir -m 700 clawctl-enrollment
tar xzf /path/to/clawctl-agent-MACHINE-linux-amd64.tar.gz -C clawctl-enrollment
cd clawctl-enrollment
./install-agent.sh
```

Use the arm64 archive for ARM. HTTPS enrollment skips Tailscale automatically. The endpoint needs systemd, systemd-logind, sudo, and outbound HTTPS. Delete the archive and transfer copies after success. Verify a fresh Hub-received check-in and identity on the machine page. HTTPS check-in alone does not hold a live WebSocket. The agent dials `/v1/agent/terminal-link` only after a local bat-server endpoint exists. See [Autopilot](AUTOPILOT.md) for the full enrollment and security model.

### 7. Verify backups and keep the Hub current

Check `https://<your-app-name>.fly.dev/healthz` (or your custom domain), then follow [verification, Telegram, and restore drill](#verification-telegram-and-restore-drill). The restore drill requires R2 replication variables; it does not work without R2. Run it after R2 is configured and monthly thereafter. It restores to a temporary directory and never touches the live database. Follow [upgrades](#upgrades) to deploy each new commit with a unique version while keeping the same volume and one Machine.

Public mode defaults to `CLAWCTL_TRUSTED_PROXIES=172.16.0.0/12` (fly-proxy egress) and `CLAWCTL_CLIENT_IP_HEADER=Fly-Client-IP`. On Fly the rightmost X-Forwarded-For entry is the app's edge IP. 6PN uses IPv6 `fdaa::/16` and is not trusted. Explicit proxy/header settings, including empty trust, are respected; see [client IP configuration](AUTOPILOT.md#client-ip-and-reverse-proxies).

## Advanced: Tailscale-only Hub on Fly

This private pack preserves the existing Tailscale behavior: no public HTTP service; operators and agents reach the literal Tailscale IPv4. Use `ops/fly/fly.tailscale.toml`. Its `[env]` explicitly sets `CLAWCTL_AUTH_MODE = "tailscale"`. Do not add a public service to its config.

### What you need

*   **Fly.io**: An account with a payment card on file. Install the CLI: `curl -L https://fly.io/install.sh | sh` (ensure `~/.fly/bin` is in your `PATH`). Authenticate with `fly auth login`. Alternatively, create an access token in the Fly dashboard (Account → Access Tokens) and export it without leaving it in shell history: `read -rs FLY_API_TOKEN && export FLY_API_TOKEN` (paste token, press Enter) — never put it directly on a command line.
*   **Tailscale**: A tailnet you control.
*   **Cloudflare R2** (Optional but recommended): For database replication and blob storage.
*   **Telegram Bot** (Optional): For built-in notifications.

### Tailscale Setup

You need to configure your tailnet ACLs to allow operators to reach the Hub and to allow the Hub to identify them.

1.  See [OPERATOR-AUTH.md#quick-start-english](OPERATOR-AUTH.md#quick-start-english) for the complete policy example and the first-login check.
2.  In your tailnet ACLs, add `tagOwners` for `tag:clawctl-hub`.
3.  Add the operator grant with app capabilities (e.g. `example.com/cap/clawctl-view`, `-operate`, and `-admin`).
4.  Add an agent-to-Hub network grant on the port you will use (the default example is `8787`).
5.  Generate a new Auth Key in the Tailscale admin panel. Make it **tagged** (`tag:clawctl-hub`), **pre-approved**, **single-use** (the Fly volume will persist the state after the first boot), and **not ephemeral** (an ephemeral node is removed when offline, which would change the Hub's IP). A short expiry (e.g. 1 day) is fine because it is used once.

### Prepare Configuration

Do NOT edit `ops/fly/fly.tailscale.toml` directly in the Git tree. Doing so makes `git describe` produce a `-dirty` version string, which bakes a dirty version into your deployments. Note that `git rev-parse --short HEAD` names the commit you have checked out — check out the commit you mean to deploy.

1.  Copy the file out of the tree:
    ```sh
    mkdir -p ~/clawctl-fly && cp ops/fly/fly.tailscale.toml ~/clawctl-fly/fly.tailscale.toml
    ```
2.  Edit `~/clawctl-fly/fly.tailscale.toml`:
    *   Change `app` to a unique name.
    *   Change `primary_region` to your preferred region (e.g., `iad`).
    *   Set `CLAWCTL_OPERATOR_CAPABILITY_PREFIX` to match your Tailscale ACL capability domain.

### Create App and Volume

Run these commands from the repository root:

```sh
fly apps create <your-app-name>
fly volumes create clawctl_data --region <your-region> --size 3 -a <your-app-name>
```
*Note: `--size 3` creates a 3 GB volume.*

### Set Secrets Safely

Secrets should be passed via stdin to avoid leaking them in your shell history. Create `secrets.env` in `~/clawctl-fly`, not in the repository (keeps it out of git), without exposing it to other users:

1.  Initialize a restricted file:
    ```sh
    install -m 600 /dev/null ~/clawctl-fly/secrets.env
    ```
    Edit it with a terminal editor, e.g. `nano ~/clawctl-fly/secrets.env`. Populate it with one `NAME=value` per line, no quotes, no spaces:
    ```env
    TS_AUTHKEY=tskey-auth-REPLACE-ME
    R2_ACCOUNT_ID=REPLACE-ME
    R2_ACCESS_KEY_ID=REPLACE-ME
    R2_SECRET_ACCESS_KEY=REPLACE-ME
    R2_BUCKET=REPLACE-ME
    # Optional:
    TELEGRAM_BOT_TOKEN=REPLACE-ME
    TELEGRAM_CHAT_ID=REPLACE-ME
    CLAWCTL_REPORT_PING_URL=https://hc-ping.com/REPLACE-ME
    # LITESTREAM_REQUIRED=1
    ```
    *Note: `CLAWCTL_REPORT_PING_URL` is an optional healthchecks.io-style ping sent after each daily report, serving as an off-host deadman switch for a Hub you cannot watch from the same host.*
2.  Import them:
    ```sh
    fly secrets import --stage -a <your-app-name> < ~/clawctl-fly/secrets.env
    ```
3.  Securely delete the file:
    ```sh
    shred -u ~/clawctl-fly/secrets.env
    ```

### Deploy

Deploy using the explicit version of your current Git commit. From the repository root:

```sh
fly deploy . \
  --config ~/clawctl-fly/fly.tailscale.toml \
  --dockerfile ops/docker/Dockerfile \
  --build-target hub-fly \
  --build-arg CLAWCTL_VERSION="$(git rev-parse --short HEAD)" \
  --ha=false
```

Then scale the machine count to precisely 1:

```sh
fly scale count 1 -a <your-app-name>
```

### Find Tailscale IP and Enroll

1.  Find your new machine's Tailscale IPv4 address:
    ```sh
    fly ssh console -a <your-app-name> -C "tailscale ip -4"
    ```
2.  From a tailnet device logged in as a user with operator grants, open `http://<that-ip>:<CLAWCTL_PORT>/` in your browser (e.g., `http://100.x.y.z:8787/`). See [OPERATOR-AUTH.md#quick-start-english](OPERATOR-AUTH.md#quick-start-english) for the first-login check.
3.  Open `http://<that-ip>:<CLAWCTL_PORT>/machines/enrollment` from your own device, create the enrollment, download the bootstrap archive for the endpoint's architecture, and run the install command shown there (see README "Enroll an endpoint").

*Note: Do not run Hub API commands such as `clawctl-hub machines` or `clawctl-hub enroll-token` through `fly ssh console`; they are rejected because the caller is the tagged Hub node (`HUMAN_PRINCIPAL_REQUIRED`). Use the web console or the CLI / `clawctl-operator` from your own tailnet device. See [OPERATOR-AUTH.md](OPERATOR-AUTH.md). Local-only commands (`tailscale ip -4`, `notify-check`, `clawctl-restore-drill`) are fine over ssh.*

Check `http://<that-ip>:<CLAWCTL_PORT>/healthz` and follow the shared verification and backup steps below.

## Cost note

This shape uses one `shared-cpu-1x` machine with 1 GB (`1024mb`) of RAM and a 3 GB volume. This is roughly $7/month at the time of writing; check the [Fly pricing page](https://fly.io/docs/about/pricing/) for exact current numbers. Cloudflare R2's free tier usually covers the storage and operations needed at the default 10 s sync interval.

## Cloudflare R2 setup (optional)

1.  In Cloudflare dashboard: navigate to **R2 Object Storage** → enable it (Cloudflare asks for a payment method even for the free tier).
2.  Click **Create bucket** and choose a name (e.g. `clawctl-backup`).
3.  Go to **R2** → **Manage API tokens** → **Create API token**.
4.  Set Permissions to **Object Read & Write**, choose **Apply to specific buckets only**, and pick your bucket.
5.  Click **Create API Token**, then copy the **Access Key ID** and **Secret Access Key** (shown once).
6.  Find your **Account ID** on the R2 overview page (also inside the S3 endpoint URL).

*Note: Configuring this single `R2_*` group enables BOTH Litestream DB replication (saving to the `clawctl/litestream/...` prefix, configurable via `LITESTREAM_PATH`) and the Hub's artifact/evidence blob store (saving to the `blobs/` prefix) in the same bucket.*

## Verification, Telegram, and restore drill

1.  Check `/healthz` on your public HTTPS origin, or the literal Tailscale listener for the advanced pack.
2.  Check the logs to ensure there are no startup errors:
    ```sh
    fly logs -a <your-app-name>
    ```
3.  If Telegram is configured, test your notifier (`notify-check` verifies the token and chat via Telegram `getMe`/`getChat` and sends no message). The entrypoint writes `/run/clawctl-notify/notify.env` and exports `CLAWCTL_NOTIFY_ENV` only for the Hub process; an ssh session does not have it. The command must be:
    ```sh
    fly ssh console -a <your-app-name> -C "clawctl-hub notify-check --notify-env /run/clawctl-notify/notify.env"
    ```
4.  If R2 is configured, run the **Restore Drill** to verify Litestream replication (recommend doing this monthly). Fly exposes app secrets as environment variables to `fly ssh console` commands, so the drill can read the R2 settings:
    ```sh
    fly ssh console -a <your-app-name> -C clawctl-restore-drill
    ```
    This restores the latest replica into a fresh temp directory, checks integrity, prints table counts, and cleans up. It never touches the live database.

    **Off-host variant**: On any Linux machine, install Litestream 0.5.x and SQLite 3, put your `R2_*` variables into a `0600` file (`r2.env`), and run:
    ```sh
    set -a; . ./r2.env; set +a; sh ops/fly/restore-drill.sh --config ops/fly/litestream.yml
    ```

## Litestream tuning

You can tune replication parameters by setting these Fly secrets (using the `secrets.env` method).

| Variable | Default | Description | Tradeoff |
|---|---|---|---|
| `LITESTREAM_SYNC_INTERVAL` | `10s` | How often WAL changes sync to R2. | Lower = smaller RPO (data loss window) but higher R2 Class A API costs. |
| `LITESTREAM_SNAPSHOT_INTERVAL` | `24h` | How often a full database snapshot is written. | Lower = faster restore time, but uses more R2 storage and PUTs. |
| `LITESTREAM_RETENTION` | `168h` | How long snapshots and WAL files are kept. | Higher = longer recovery window, but uses more R2 storage. Must be >= snapshot interval. |

## Volume layout and Litestream

| Path | Owner | What it is |
|---|---|---|
| `/var/lib/clawctl` | 65532, mode 0700 | SQLite parent. Hub refuses another owner or any group/other bit. |
| `clawctl.sqlite`, `-wal`, `-shm` | 65532 | The ledger. Hub opens it in WAL mode. |
| `clawctl.sqlite.writer.lock`, `.upgrade.lock` | 65532 | Local locks. Not part of the SQLite replica. |
| `agent-bootstrap/<version>/` | 65532 | Linux amd64 and arm64 bundles plus `SHA256SUMS`. Seeded once. An existing release is not replaced. |
| `tailscale/` | root, mode 0700 | tailscaled state. Excluded from the chown. |

Litestream and Hub both run as uid/gid 65532, so the database, the WAL, the shared-memory file, and Litestream's sidecar directory stay owned by 65532. Hub refuses other owners.

SQLite settings Hub already uses (`internal/store` open): `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`, immediate transactions. The WAL autocheckpoint is SQLite's default (1000 pages). Litestream 0.5 needs WAL. `busy_timeout` of 5 seconds is how Hub waits if Litestream briefly holds a lock. `writer.lock` is a separate file. A restored database does not need the old lock file; Hub creates it. Do not copy `tailscale/` into the replica.

On boot, if `clawctl.sqlite` is absent, the entrypoint runs `litestream restore -if-db-not-exists -if-replica-exists` as uid 65532. An existing database is not overwritten. Litestream then runs `replicate -exec` and supervises Hub. If Hub exits, Litestream exits and Fly restarts the machine (`restart` policy `always`). `SIGTERM` goes to Litestream (PID 1), which forwards it so Hub's shutdown can finish and Litestream can flush. `kill_timeout` is 30 seconds.

## Upgrading an existing Tailscale Fly Hub

Before deploying the new image, copy `CLAWCTL_AUTH_MODE = "tailscale"` into the `[env]` section of your existing out-of-tree `fly.toml`. Keep its private service configuration and volume. The entrypoint also auto-detects non-empty persisted Tailscale state or a set `TS_AUTHKEY` when the mode is unset, preserving older deployments. An explicit mode always wins; explicit local mode warns and discards `TS_AUTHKEY`.

## Upgrades

`CLAWCTL_VERSION` is baked into `main.version` and into `agent-bootstrap/<version>/`. Keep an explicit version (e.g. current commit hash) on deploy:

```sh
fly deploy . \
  --config ~/clawctl-fly/fly.toml \
  --dockerfile ops/docker/Dockerfile \
  --build-target hub-fly \
  --build-arg CLAWCTL_VERSION="$(git rev-parse --short HEAD)" \
  --ha=false
```

The init step seeds that version only when its directory is absent. Reusing a version string leaves the already seeded bundles in place without re-seeding. Keep the volume and public URL stable. For the advanced pack, use `--config ~/clawctl-fly/fly.tailscale.toml`; the Tailscale IP stays if the volume stays.

## Troubleshooting

*Note: Never delete the volume. It holds the Hub database and enrollment state; in the advanced pack the Tailscale IP is also the Hub's identity to your agents. If you must move agents to a new Hub, see [MOVE-AGENTS.md](MOVE-AGENTS.md).*

| Symptom | What to check |
|---|---|
| Depot handshake / `failed to list workers` | `deploy.sh` retries once with `--depot=false --yes` and removes only builders created during that retry. If teardown fails, inspect the named builder; do not remove pre-existing builders. |
| MFA says `Pending login expired` | Pending login is bound to the full client IP. Use one keep-alive HTTPS connection when egress changes per TCP connection; `setup-admin.sh` does this for setup and TOTP confirmation. |
| Public UI returns `421` (wrong host) | `CLAWCTL_PUBLIC_URL` must match the browser's HTTPS origin. After a domain switch, use the new domain; the old Host is refused. |
| Public health check failing | Check `fly logs` for startup/config/volume errors; `CLAWCTL_PORT` and `http_service.internal_port` must both be 8787. Probe is GET `/healthz`; it needs no admin session. |
| Setup code absent from logs | Setup is closed once an admin exists; use `/login`. An explicit `CLAWCTL_SETUP_CODE` is never logged. See Autopilot recovery if credentials are lost. |
| All clients share rate limits or one IP | Check trusted proxy CIDRs and `Fly-Client-IP`; defaults trust only fly-proxy IPv4 egress, not 6PN. Empty or incorrect trust uses the proxy peer IP. |
| `both` refused on Fly | Choose `local` or `tailscale`; public wildcard listening cannot supply literal Tailscale WhoIs identity. |
| `TS_AUTHKEY is required` | No state file yet. Set the Fly secret. The key is not echoed. |
| `/dev/net/tun is missing` | The machine is not a Firecracker VM with a tun device. |
| `tailscale up failed` | Auth key tags, `tagOwners`, and `TS_TAGS` (default `tag:clawctl-hub`). |
| Advanced Tailscale listen refused / Hub exits before the DB opens | `CLAWCTL_LISTEN` must be the Tailscale IP from `tailscale ip -4`, not `0.0.0.0`, loopback, or a public IP. There is no auth-bypass flag. |
| Advanced Tailscale `AUTH_SOURCE_UNAVAILABLE` | LocalAPI socket mode `0660`, group 65532, and a running tailscaled. Hub does not trust `X-Forwarded-*`. |
| `Hub cannot read notify file` | Should not happen with the built-in path; verify `/run/clawctl-notify` is owned by `65532:65532` mode `0700` and `notify.env` is mode `0600`. |
| `notify-check: no env file` | Pass `--notify-env /run/clawctl-notify/notify.env`; if the file is missing, the `TELEGRAM_*` secrets were not set when the machine started (`fly secrets list`, then `fly secrets deploy`). |
| Ownership errors on the data dir | Hub and Litestream must be uid 65532. `tailscale/` must stay root. Do not chown the whole volume to root after init. |
| Replication refused at boot | A partial `R2_*` set is a hard error. Unset them all, or set access key, secret, bucket, and endpoint (or account id). |
| `restore-drill: missing required environment variables` | Required replication variables (`R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `LITESTREAM_BUCKET`, `LITESTREAM_ENDPOINT`) are not set in the environment. |
| `CLAWCTL_NOTIFY_ENV is already set` | Operator mounted a custom notify file but also passed Fly secrets. Remove one. |
| Restore drill fails integrity check | `restore-drill.sh` reports `integrity check failed`. Corrupted replica or partial upload. Check Litestream logs. |

Build the image locally when Docker is available:

```sh
./ops/docker/smoke-build.sh
```

That builds the `hub-fly` target, checks `tailscaled`, `tailscale`, `litestream`, `clawctl-hub`, and the bundle files, and checks that Tailscale mode fails clearly with no `TS_AUTHKEY`. It also parses both Fly config files. For local-only static validation, run `CLAWCTL_SMOKE_STATIC_ONLY=1 bash ops/docker/smoke-build.sh`; `bash ops/test-fly.sh` uses only local stubs.
