# Fly.io pack

The primary Fly path is **Autopilot over public HTTPS**: local admin, required TOTP MFA, and keyed agent installers. Follow [docs/DEPLOY-FLY.md](../../docs/DEPLOY-FLY.md#autopilot-on-fly-recommended) from app creation through enrollment and backups.

Copy `ops/fly/fly.toml` out of the repository to `~/clawctl-fly/fly.toml`, edit `app` and `primary_region`, create the app and a 3 GB `clawctl_data` volume, and stage optional R2/Telegram/setup-code secrets via a mode-0600 file. Do not commit secrets. From the repository root:

```sh
fly deploy . \
  --config ~/clawctl-fly/fly.toml \
  --dockerfile ops/docker/Dockerfile \
  --build-target hub-fly \
  --build-arg CLAWCTL_VERSION="$(git rev-parse --short HEAD)" \
  --ha=false
```

Local mode is the default for fresh deployments without Tailscale state or a set `TS_AUTHKEY`. It listens on `0.0.0.0:8787`, needs neither `TS_AUTHKEY` nor `/dev/net/tun`, and starts no Tailscale process. Fly terminates HTTPS; `CLAWCTL_PUBLIC_URL` defaults to `https://$FLY_APP_NAME.fly.dev` and must be HTTPS. The trusted proxy defaults are `172.16.0.0/12` and `Fly-Client-IP`; IPv6 6PN is not trusted. Read the generated setup code from logs and open `/setup`. Choose any custom domain before enrolling machines because Host is pinned to the public URL.

For the [advanced Tailscale-only path](../../docs/DEPLOY-FLY.md#advanced-tailscale-only-hub-on-fly), copy `fly.tailscale.toml` out of tree and pass that copy as `--config`. The config sets `CLAWCTL_AUTH_MODE = "tailscale"` in `[env]`; stage only the initial tagged `TS_AUTHKEY` and any optional secrets. The private config has no public HTTP service. Hub binds the machine's Tailscale IPv4, and state persists under root-owned `tailscale/` on the volume. `both` is refused on Fly because wildcard public listening cannot supply literal Tailscale WhoIs identity.

Both paths share data initialization, versioned bundles, R2/Litestream, Telegram secret files, uid/gid 65532, and PID-1 signal handling. Keep one Machine and the same volume. `8787` is the pack's conventional port, not the binary default. See the deployment guide for restore drills, tuning, and upgrades.

Local validation: `bash ops/test-fly.sh` and `CLAWCTL_SMOKE_STATIC_ONLY=1 bash ops/docker/smoke-build.sh`. No account or deployment is needed.

## Upgrading an existing Tailscale Fly Hub

Copy `CLAWCTL_AUTH_MODE = "tailscale"` into `[env]` in your out-of-tree `fly.toml` before upgrading. When the mode is unset, the entrypoint also auto-detects non-empty persisted Tailscale state or a set `TS_AUTHKEY`. Explicit mode settings always win; explicit local mode warns and unsets the ignored key. Keep the existing private config and volume.
