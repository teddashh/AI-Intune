# AI-Intune

**English** · [繁體中文](README.zh-TW.md)

AI-Intune is an evidence based control plane for machines running AI agents and coding tools. The Hub records inventory, observations, desired state, jobs, verification evidence, and audit events. Agents connect outbound from managed machines.

[Project website](https://teddashh.github.io/AI-Intune/) · [Apache 2.0 license](LICENSE)

## Build and test

Use Go 1.27.1 on Linux:

```sh
git clone https://github.com/teddashh/AI-Intune.git
cd AI-Intune
make test vet
make hub agent-bundles
```

The Hub binary is `build/clawctl-hub`. Linux Agent binaries and bootstrap bundles are produced under `build/`. The repository also contains platform specific Agent installers and system service templates in `ops/`.

## Deploy

The primary path is any Linux host with Docker. See [docs/DEPLOY-OSS.md](docs/DEPLOY-OSS.md). Set `CLAWCTL_LISTEN` to that host's Tailscale IP and the port you chose (`8787` is the conventional example, not a Hub default). The Tailscale grant `dst` port, `CLAWCTL_PUBLIC_URL`, the agent `--hub` URL, and any tunnel origin must use that same port.

```sh
cp ops/docker/hub.env.example ops/docker/hub.env
docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env up -d --build
```

systemd (`ops/install-hub.sh`) is the alternative when the host has no Docker. Fly.io is the hosted example: [docs/DEPLOY-FLY.md](docs/DEPLOY-FLY.md). Cloudflare Containers is not implemented. It would need tsnet (userspace Tailscale inside the Hub process), Litestream, and a Durable Object keep-alive.

## Operating model

The Hub is intended to run on a private network. Operator access uses Tailscale identity and capabilities; each Agent has its own machine credential. Enrollment uses a one time token. Credentials and local configuration stay on the operator or managed machine and are never included in this repository.

See the source code and tests for the current API and behavior. Configure a separate private environment before deploying; the example addresses in the code and tests are illustrative.
