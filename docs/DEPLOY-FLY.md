# Deploy clawctl-hub on Fly.io

Fly.io is the hosted example. The primary path is still any Linux host with Docker. See [DEPLOY-OSS.md](DEPLOY-OSS.md). A systemd install (`ops/install-hub.sh`) is the alternative when the host has no Docker.

This pack runs one always-on Fly Machine. Hub binds that machine's Tailscale IPv4. There is no public HTTP service. Operators and agents reach Hub only over Tailscale.

`8787` below is the conventional example port (`CLAWCTL_PORT`). Hub has no production default port. The `--listen` flag default `127.0.0.1:8770` is refused. The Tailscale grant `dst` port, `CLAWCTL_PUBLIC_URL`, the agent `--hub` URL, and any tunnel origin must all use the same port as `CLAWCTL_LISTEN`. On Fly, the entrypoint sets `CLAWCTL_LISTEN` to `<tailscale ipv4>:${CLAWCTL_PORT}`.

Check [Fly pricing](https://fly.io/docs/about/pricing/) before you create anything. This shape is one shared-cpu-1x machine with 512 MB of RAM, one small volume, and no public ingress. Prices change, so this guide does not quote them.

## Prerequisites

1. A Fly account and `flyctl`. This repository does not create the app for you.
2. Edit `ops/fly/fly.toml`: replace `clawctl-hub-CHANGE-ME` and `primary_region = "CHANGE-ME"`.
3. A Tailscale tailnet you control.
   - Tag `tag:clawctl-hub` with `tagOwners` that can assign it.
   - An auth key that is tagged for `tag:clawctl-hub` (reusable is convenient for the first boot; the node then keeps state on the volume).
   - App capability grants for `<your-domain>/cap/clawctl-view`, `-operate`, and `-admin`. Set `dst` to `tag:clawctl-hub` or to the machine's Tailscale IP once you know it, and set the port to the same port as `CLAWCTL_PORT` (example `tcp:8787` only when that is the port you chose).
   - `CLAWCTL_OPERATOR_CAPABILITY_PREFIX` in `fly.toml` must be that prefix (`example.com/cap/clawctl` is a placeholder).
4. An R2 bucket and a scoped API token (object read and write on that bucket). You can skip R2. Hub still runs; the entrypoint logs a warning and does not start Litestream. A partial `R2_*` set refuses to start, because Hub also refuses a partial object-storage group.

## First deploy

From the repository root:

```sh
fly apps create clawctl-hub-CHANGE-ME
fly volumes create clawctl_data --size 1 --region CHANGE-ME -a clawctl-hub-CHANGE-ME
fly secrets set \
  TS_AUTHKEY=tskey-auth-REPLACE-ME \
  R2_ACCOUNT_ID=REPLACE-ME \
  R2_ACCESS_KEY_ID=REPLACE-ME \
  R2_SECRET_ACCESS_KEY=REPLACE-ME \
  R2_BUCKET=REPLACE-ME \
  -a clawctl-hub-CHANGE-ME
fly deploy . \
  --config ops/fly/fly.toml \
  --dockerfile ops/docker/Dockerfile \
  --build-target hub-fly \
  --build-arg CLAWCTL_VERSION="$(git describe --tags --always --dirty)" \
  --ha=false
fly scale count 1 -a clawctl-hub-CHANGE-ME
```

Use your real app name and region. `--size 1` is 1 GB. The Docker context is the repo root (the `.` argument). `ops/fly/fly.toml` points the Dockerfile at `../docker/Dockerfile`, which is relative to the toml file. Do not deploy from another directory without passing the same `--dockerfile` and context.

`R2_ENDPOINT` is optional. When it is unset and `R2_ACCOUNT_ID` is set, the entrypoint derives `https://<account>.r2.cloudflarestorage.com` and exports it so Hub sees a complete `R2_*` group. The value is not printed. `LITESTREAM_BUCKET` overrides `R2_BUCKET` for the replica. `LITESTREAM_PATH` defaults to `clawctl/litestream/clawctl.sqlite`.

`LITESTREAM_REQUIRED=1` (or `true` / `yes`) fails startup when replication settings are incomplete. Leave it unset, and leave every `R2_*` and `LITESTREAM_*` variable unset, to run without replication.

Change the VM size in `[[vm]]` or with `fly scale vm shared-cpu-1x --memory 1024`. Keep one machine. A second machine would be a second Tailscale node and a second writer. Do not turn on high availability (`fly deploy --ha=false`).

## Tailscale IP and enrollment

The auth key is a Fly secret. The entrypoint writes it to a root-only file, passes `--auth-key=file:...` (tailscale v1.102), deletes the file, and unsets `TS_AUTHKEY` before Hub starts. It is not printed.

Tailscale state is `/var/lib/clawctl/tailscale` on the volume, mode `0700`, owner root. `hub-data-init` does not chown that directory (`CLAWCTL_CHOWN_EXCLUDE=tailscale`). Hub checks only the SQLite parent directory (owner 65532, mode `0700`), so the extra subdirectory is allowed. As long as the volume and that state stay, the node keeps the same Tailscale IP across restarts and deploys. Agents are enrolled against that literal IP. Deleting the volume or the state directory creates a new node and a new IP. Enrolled agents then point at the old address, and the grant `dst` must be updated to the new IP and the same port.

Find the address:

```sh
fly ssh console -a clawctl-hub-CHANGE-ME -C "tailscale ip -4"
```

That prints the IPv4. Hub is listening on `<that ip>:<CLAWCTL_PORT>`. Open `http://<that ip>:<port>/` from a tailnet device that holds the grants. Enroll an agent with `--hub http://<that ip>:<port>`. The port is the `CLAWCTL_PORT` you set, not a Hub default.

The image sets the LocalAPI socket to owner `root:65532` and mode `0660`. Hub runs as uid 65532 and uses that socket for WhoIs. If operator calls return `AUTH_SOURCE_UNAVAILABLE`, check that `/var/run/tailscale/tailscaled.sock` is still `0660` and group `65532`, and that `/dev/net/tun` exists. Fly Firecracker machines provide the tun device. This image does not use userspace Tailscale.

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

Point-in-time restore is Litestream's `-timestamp` flag. Stop the machine first so nothing writes the database. On a machine that has `/etc/litestream.yml` and the same R2 secrets, run `litestream restore` as uid 65532 against an empty `clawctl.sqlite` path. The entrypoint's normal path is only the empty-database restore (`-if-db-not-exists -if-replica-exists`). It does not overwrite a database that is already on the volume. See the Litestream 0.5 restore docs for the timestamp flag. Do not restore `tailscale/` from the replica; that state is not in R2.

## Upgrades

`CLAWCTL_VERSION` is baked into `main.version` and into `agent-bootstrap/<version>/`. Pass a new value on deploy:

```sh
fly deploy . \
  --config ops/fly/fly.toml \
  --dockerfile ops/docker/Dockerfile \
  --build-target hub-fly \
  --build-arg CLAWCTL_VERSION="$(git describe --tags --always --dirty)" \
  --ha=false
```

The init step seeds that version only when its directory is absent. Reusing a version string leaves the already seeded bundles in place. The Tailscale IP stays if the volume stays.

## Troubleshooting

| Symptom | What to check |
|---|---|
| `TS_AUTHKEY is required` | No state file yet. Set the Fly secret. The key is not echoed. |
| `/dev/net/tun is missing` | The machine is not a Firecracker VM with a tun device. |
| `tailscale up failed` | Auth key tags, `tagOwners`, and `TS_TAGS` (default `tag:clawctl-hub`). |
| Listen refused / Hub exits before the DB opens | `CLAWCTL_LISTEN` must be the Tailscale IP from `tailscale ip -4`, not `0.0.0.0`, loopback, or a public IP. There is no auth-bypass flag. |
| `AUTH_SOURCE_UNAVAILABLE` | LocalAPI socket mode `0660`, group 65532, and a running tailscaled. Hub does not trust `X-Forwarded-*`. |
| Ownership errors on the data dir | Hub and Litestream must be uid 65532. `tailscale/` must stay root. Do not chown the whole volume to root after init. |
| Replication refused at boot | A partial `R2_*` set is a hard error. Unset them all, or set access key, secret, bucket, and endpoint (or account id). |

Build the image locally when Docker is available:

```sh
./ops/docker/smoke-build.sh
```

That builds the `hub-fly` target, checks `tailscaled`, `tailscale`, `litestream`, `clawctl-hub`, and the bundle files, and checks that the entrypoint fails clearly with no `TS_AUTHKEY`. It also parses `ops/fly/fly.toml`.
