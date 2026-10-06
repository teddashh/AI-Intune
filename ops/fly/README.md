# Fly.io pack

Hosted example for one always-on Hub. The primary path is still any Linux host with Docker. See [docs/DEPLOY-FLY.md](../../docs/DEPLOY-FLY.md) and [docs/DEPLOY-OSS.md](../../docs/DEPLOY-OSS.md).

This directory does not publish a public HTTP service. Hub binds the machine's Tailscale IPv4. `8787` in `fly.toml` is `CLAWCTL_PORT`, the conventional example, not a Hub default.

From the repository root:

```sh
fly deploy . \
  --config ops/fly/fly.toml \
  --dockerfile ops/docker/Dockerfile \
  --build-target hub-fly \
  --build-arg CLAWCTL_VERSION="$(git describe --tags --always --dirty)" \
  --ha=false
```

Edit `app` and `primary_region` in `fly.toml` first. Create the app, the `clawctl_data` volume, a Tailscale tagged auth key, and the R2 token before that command. Secrets are `TS_AUTHKEY` and the `R2_*` variables. Do not commit them.

`entrypoint.sh` is root only for tailscaled and the data-directory setup. Hub and Litestream run as uid 65532. Tailscale state stays on the volume under `tailscale/` and stays root-owned, so the node keeps its Tailscale IP across deploys.
