# Fly.io pack

The primary Fly path is **Autopilot over public HTTPS**: local admin, required TOTP MFA, and keyed agent installers. Follow [docs/DEPLOY-FLY.md](../../docs/DEPLOY-FLY.md#autopilot-on-fly-recommended) from app creation through enrollment and backups.

Run `ops/fly/deploy.sh --org <your-org> --app <your-app-name> --region iad --dry-run` to preview, then omit `--dry-run` to deploy. It copies the config outside the tree, keeps one 1024mb Machine and a 3GB volume, and handles Depot fallback and cleanup of new builder apps. Optional secrets use `--secrets-file` with a mode-0600 file; R2 is not required. Never commit or print secrets.

For a custom domain, pass `--public-url https://hub.example.com` (or set `FLY_PUBLIC_URL`) to check that origin and show its setup URL. The URL must be HTTPS with a DNS host, an optional port, and no path, query, fragment, or userinfo; a trailing slash is allowed. This selects the checks URL; configure `CLAWCTL_PUBLIC_URL` separately. Without an explicit URL, a fly.dev setup response of HTTP 421 reports the custom domain and leaves the successful deployment intact.

Finish `/setup` in a browser or with `ops/fly/setup-admin.sh --app <your-app-name> --username <your-admin-name> --setup-code-file /private/setup-code --out /private/admin-credentials.json`. The helper uses one keep-alive HTTPS connection, enrolls MFA, and saves credentials privately without printing them. `deploy.sh --app <your-app-name> --destroy` requires typing the exact app name; preview it with `--dry-run`.

Local mode is the default for fresh deployments without Tailscale state or a set `TS_AUTHKEY`. It listens on `0.0.0.0:8787`, needs neither `TS_AUTHKEY` nor `/dev/net/tun`, and starts no Tailscale process. Fly terminates HTTPS; `CLAWCTL_PUBLIC_URL` defaults to `https://$FLY_APP_NAME.fly.dev` and must be HTTPS. The trusted proxy defaults are `172.16.0.0/12` and `Fly-Client-IP`; IPv6 6PN is not trusted. On first run, deploy.sh prints the generated setup code once if `/setup` is open; later deploys never print stale codes. Choose any custom domain before enrolling machines because Host is pinned to the public URL.

For the [advanced Tailscale-only path](../../docs/DEPLOY-FLY.md#advanced-tailscale-only-hub-on-fly), copy `fly.tailscale.toml` out of tree and pass that copy as `--config`. The config sets `CLAWCTL_AUTH_MODE = "tailscale"` in `[env]`; stage only the initial tagged `TS_AUTHKEY` and any optional secrets. The private config has no public HTTP service. Hub binds the machine's Tailscale IPv4, and state persists under root-owned `tailscale/` on the volume. `both` is refused on Fly because wildcard public listening cannot supply literal Tailscale WhoIs identity.

Both paths share data initialization, versioned bundles, R2/Litestream, Telegram secret files, uid/gid 65532, and PID-1 signal handling. Keep one Machine and the same volume. `8787` is the pack's conventional port, not the binary default. See the deployment guide for restore drills, tuning, and upgrades.

Local validation: `bash ops/test-fly.sh` and `CLAWCTL_SMOKE_STATIC_ONLY=1 bash ops/docker/smoke-build.sh`. No account or deployment is needed.

## Upgrading an existing Tailscale Fly Hub

Copy `CLAWCTL_AUTH_MODE = "tailscale"` into `[env]` in your out-of-tree `fly.toml` before upgrading. When the mode is unset, the entrypoint also auto-detects non-empty persisted Tailscale state or a set `TS_AUTHKEY`. Explicit mode settings always win; explicit local mode warns and unsets the ignored key. Keep the existing private config and volume.
