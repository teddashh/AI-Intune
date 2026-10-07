# Deploy clawctl-hub on Fly.io

This is a step-by-step guide for first-time deployers to run `clawctl-hub` on Fly.io. This pack runs one always-on Fly Machine that binds a Tailscale IP. There is no public HTTP service. Operators and agents reach Hub only over Tailscale.

## 1. What you need

*   **Fly.io**: An account with a payment card on file. Install the CLI: `curl -L https://fly.io/install.sh | sh` (ensure `~/.fly/bin` is in your `PATH`). Authenticate with `fly auth login`. Alternatively, create an access token in the Fly dashboard (Account → Access Tokens) and export it without leaving it in shell history: `read -rs FLY_API_TOKEN && export FLY_API_TOKEN` (paste token, press Enter) — never put it directly on a command line.
*   **Tailscale**: A tailnet you control.
*   **Cloudflare R2** (Optional but recommended): For database replication and blob storage.
*   **Telegram Bot** (Optional): For built-in notifications.

## 2. Cost Note

This shape uses one `shared-cpu-1x` machine with 1 GB (`1024mb`) of RAM and a 3 GB volume. This is roughly $7/month at the time of writing; check the [Fly pricing page](https://fly.io/docs/about/pricing/) for exact current numbers. Cloudflare R2's free tier usually covers the storage and operations needed at the default 10 s sync interval.

## 3. Tailscale Setup

You need to configure your tailnet ACLs to allow operators to reach the Hub and to allow the Hub to identify them.

1.  See [OPERATOR-AUTH.md#quick-start-english](OPERATOR-AUTH.md#quick-start-english) for the complete policy example and the first-login check.
2.  In your tailnet ACLs, add `tagOwners` for `tag:clawctl-hub`.
3.  Add the operator grant with app capabilities (e.g. `example.com/cap/clawctl-view`, `-operate`, and `-admin`).
4.  Add an agent-to-Hub network grant on the port you will use (the default example is `8787`).
5.  Generate a new Auth Key in the Tailscale admin panel. Make it **tagged** (`tag:clawctl-hub`), **pre-approved**, **single-use** (the Fly volume will persist the state after the first boot), and **not ephemeral** (an ephemeral node is removed when offline, which would change the Hub's IP). A short expiry (e.g. 1 day) is fine because it is used once.

## 4. Cloudflare R2 Setup (Optional)

1.  In Cloudflare dashboard: navigate to **R2 Object Storage** → enable it (Cloudflare asks for a payment method even for the free tier).
2.  Click **Create bucket** and choose a name (e.g. `clawctl-backup`).
3.  Go to **R2** → **Manage API tokens** → **Create API token**.
4.  Set Permissions to **Object Read & Write**, choose **Apply to specific buckets only**, and pick your bucket.
5.  Click **Create API Token**, then copy the **Access Key ID** and **Secret Access Key** (shown once).
6.  Find your **Account ID** on the R2 overview page (also inside the S3 endpoint URL).

*Note: Configuring this single `R2_*` group enables BOTH Litestream DB replication (saving to the `clawctl/litestream/...` prefix, configurable via `LITESTREAM_PATH`) and the Hub's artifact/evidence blob store (saving to the `blobs/` prefix) in the same bucket.*

## 5. Prepare Configuration

Do NOT edit `ops/fly/fly.toml` directly in the Git tree. Doing so makes `git describe` produce a `-dirty` version string, which bakes a dirty version into your deployments. Note that `git rev-parse --short HEAD` names the commit you have checked out — check out the commit you mean to deploy.

1.  Copy the file out of the tree:
    ```sh
    mkdir -p ~/clawctl-fly && cp ops/fly/fly.toml ~/clawctl-fly/fly.toml
    ```
2.  Edit `~/clawctl-fly/fly.toml`:
    *   Change `app` to a unique name.
    *   Change `primary_region` to your preferred region (e.g., `iad`).
    *   Set `CLAWCTL_OPERATOR_CAPABILITY_PREFIX` to match your Tailscale ACL capability domain.

## 6. Create App and Volume

Run these commands from the repository root:

```sh
fly apps create <your-app-name>
fly volumes create clawctl_data --region <your-region> --size 3 -a <your-app-name>
```
*Note: `--size 3` creates a 3 GB volume.*

## 7. Set Secrets Safely

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

## 8. Deploy

Deploy using the explicit version of your current Git commit. From the repository root:

```sh
fly deploy . \
  --config ~/clawctl-fly/fly.toml \
  --dockerfile ops/docker/Dockerfile \
  --build-target hub-fly \
  --build-arg CLAWCTL_VERSION="$(git rev-parse --short HEAD)" \
  --ha=false
```

Then scale the machine count to precisely 1:

```sh
fly scale count 1 -a <your-app-name>
```

## 9. Find Tailscale IP and Enroll

1.  Find your new machine's Tailscale IPv4 address:
    ```sh
    fly ssh console -a <your-app-name> -C "tailscale ip -4"
    ```
2.  From a tailnet device logged in as a user with operator grants, open `http://<that-ip>:<CLAWCTL_PORT>/` in your browser (e.g., `http://100.x.y.z:8787/`). See [OPERATOR-AUTH.md#quick-start-english](OPERATOR-AUTH.md#quick-start-english) for the first-login check.
3.  Open `http://<that-ip>:<CLAWCTL_PORT>/machines/enrollment` from your own device, create the enrollment, download the bootstrap archive for the endpoint's architecture, and run the install command shown there (see README "Enroll an endpoint").

*Note: Do not run Hub API commands such as `clawctl-hub machines` or `clawctl-hub enroll-token` through `fly ssh console`; they are rejected because the caller is the tagged Hub node (`HUMAN_PRINCIPAL_REQUIRED`). Use the web console or the CLI / `clawctl-operator` from your own tailnet device. See [OPERATOR-AUTH.md](OPERATOR-AUTH.md). Local-only commands (`tailscale ip -4`, `notify-check`, `clawctl-restore-drill`) are fine over ssh.*

## 10. Verify Setup

1.  Check the health endpoint: `http://<that-ip>:<CLAWCTL_PORT>/healthz`.
2.  Check the logs to ensure there are no startup errors:
    ```sh
    fly logs -a <your-app-name>
    ```
3.  Test your notifier (`notify-check` verifies the token and chat via Telegram `getMe`/`getChat` and sends no message). The entrypoint writes `/run/clawctl-notify/notify.env` and exports `CLAWCTL_NOTIFY_ENV` only for the Hub process; an ssh session does not have it. The command must be:
    ```sh
    fly ssh console -a <your-app-name> -C "clawctl-hub notify-check --notify-env /run/clawctl-notify/notify.env"
    ```
4.  Run the **Restore Drill** to verify Litestream replication (recommend doing this monthly). Fly exposes app secrets as environment variables to `fly ssh console` commands, so the drill can read the R2 settings:
    ```sh
    fly ssh console -a <your-app-name> -C clawctl-restore-drill
    ```
    This restores the latest replica into a fresh temp directory, checks integrity, prints table counts, and cleans up. It never touches the live database.

    **Off-host variant**: On any Linux machine, install Litestream 0.5.x and SQLite 3, put your `R2_*` variables into a `0600` file (`r2.env`), and run:
    ```sh
    set -a; . ./r2.env; set +a; sh ops/fly/restore-drill.sh --config ops/fly/litestream.yml
    ```

## 11. Litestream Tuning

You can tune replication parameters by setting these Fly secrets (using the `secrets.env` method).

| Variable | Default | Description | Tradeoff |
|---|---|---|---|
| `LITESTREAM_SYNC_INTERVAL` | `10s` | How often WAL changes sync to R2. | Lower = smaller RPO (data loss window) but higher R2 Class A API costs. |
| `LITESTREAM_SNAPSHOT_INTERVAL` | `24h` | How often a full database snapshot is written. | Lower = faster restore time, but uses more R2 storage and PUTs. |
| `LITESTREAM_RETENTION` | `168h` | How long snapshots and WAL files are kept. | Higher = longer recovery window, but uses more R2 storage. Must be >= snapshot interval. |

## 12. Volume layout and Litestream

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

## 13. Upgrades

`CLAWCTL_VERSION` is baked into `main.version` and into `agent-bootstrap/<version>/`. Keep an explicit version (e.g. current commit hash) on deploy:

```sh
fly deploy . \
  --config ~/clawctl-fly/fly.toml \
  --dockerfile ops/docker/Dockerfile \
  --build-target hub-fly \
  --build-arg CLAWCTL_VERSION="$(git rev-parse --short HEAD)" \
  --ha=false
```

The init step seeds that version only when its directory is absent. Reusing a version string leaves the already seeded bundles in place without re-seeding. The Tailscale IP stays if the volume stays.

## 14. Troubleshooting

*Note: Never delete the volume. The Tailscale IP is the Hub's identity to your agents. If you must move agents to a new Hub, see [MOVE-AGENTS.md](MOVE-AGENTS.md).*

| Symptom | What to check |
|---|---|
| `TS_AUTHKEY is required` | No state file yet. Set the Fly secret. The key is not echoed. |
| `/dev/net/tun is missing` | The machine is not a Firecracker VM with a tun device. |
| `tailscale up failed` | Auth key tags, `tagOwners`, and `TS_TAGS` (default `tag:clawctl-hub`). |
| Listen refused / Hub exits before the DB opens | `CLAWCTL_LISTEN` must be the Tailscale IP from `tailscale ip -4`, not `0.0.0.0`, loopback, or a public IP. There is no auth-bypass flag. |
| `AUTH_SOURCE_UNAVAILABLE` | LocalAPI socket mode `0660`, group 65532, and a running tailscaled. Hub does not trust `X-Forwarded-*`. |
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

That builds the `hub-fly` target, checks `tailscaled`, `tailscale`, `litestream`, `clawctl-hub`, and the bundle files, and checks that the entrypoint fails clearly with no `TS_AUTHKEY`. It also parses your `fly.toml` file.

## Autopilot (public) mode: client IP

The full Fly public-mode pack comes in a later PR; this pack remains Tailscale-only. For public HTTP mode set `CLAWCTL_TRUSTED_PROXIES=172.16.0.0/12` and `CLAWCTL_CLIENT_IP_HEADER=Fly-Client-IP`. The rightmost X-Forwarded-For entry is the app's own edge address. Fly staff describe proxy egress as 172.16.0.0/16, but 172.19.x has been observed, hence /12. Fly 6PN private networking uses IPv6 fdaa::/16, which this configuration does not trust. See [Client IP and reverse proxies](AUTOPILOT.md#client-ip-and-reverse-proxies).
