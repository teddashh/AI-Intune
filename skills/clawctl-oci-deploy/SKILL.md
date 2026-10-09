---
name: clawctl-oci-deploy
description: Use when deploying clawctl Hub on an Oracle Cloud Ubuntu VM with Docker and Caddy as public HTTPS Autopilot, including firewall, first-run MFA, and verification.
---

# OCI Autopilot deployment

Use plain shell from the repository root. Read `docs/DEPLOY-OCI.md`. Prerequisites: OCI CLI on `PATH`, a config in `~/.oci/config` (or `OCI_CLI_CONFIG_FILE`), Python 3, curl, and an SSH key. `oci setup config` generates the API key. The human pastes only the public half under Console → My profile → API keys. Never print the private key, fingerprint file contents, or `~/.oci` material.

Ask the human for the display name (default `clawctl-hub`), region, shape (default 2 OCPU / 12 GB, or `--free-max` for 4/24), Ubuntu 22.04 or 24.04, SSH CIDR, and domain. Decide the real domain before any enrollment. Without a domain the URL is `<public-ip-dashed>.sslip.io` and is only for a first look.

## Commands

Preview, including a read-only image and quota check:

```sh
ops/oci/provision.sh --dry-run
```

Before a real create, have the human confirm the name and that Always Free headroom exists. Then:

```sh
ops/oci/provision.sh --name clawctl-hub --ssh-cidr <operator-ip>/32
# Optional final name:
ops/oci/provision.sh --name clawctl-hub --domain hub.example.com --ssh-cidr <operator-ip>/32
```

For a VM that already exists:

```sh
ops/oci/install-hub.sh --host <ip-or-name> --dry-run
ops/oci/install-hub.sh --host <ip-or-name> --public-host <hostname> \
  --admin-user <username> --admin-out ~/clawctl-oci/admin.json
```

`install-hub.sh` waits for cloud-init, then runs the same host setup. It is idempotent. Admin enrollment calls `ops/fly/setup-admin.sh` and writes username, password, TOTP secret, and recovery codes to a new mode-0600 file. It never prints those values. A setup code may appear once on the operator's own terminal when admin flags are omitted. Never paste it into chat.

To destroy a confirmed disposable stack, preview, then type the exact name. There is no flag that skips the typed name:

```sh
ops/oci/provision.sh --name clawctl-hub --destroy --dry-run
ops/oci/provision.sh --name clawctl-hub --destroy
```

## Verification

- HTTPS `/healthz` returns 200 with valid TLS. Never disable certificate verification.
- Through Caddy, a foreign Host on any path returns 421 at the edge: `curl -H 'Host: wrong.example' https://<host>/login`. The in-network `docker run` check of Hub in `docs/DEPLOY-OCI.md` is an optional deeper check. `/healthz` on Hub directly is still exempt.
- MFA is required: after enrollment, password-only login does not create a session. Complete authenticator verification privately.
- Audit client IP is the real client, not an address in the Docker bridge (`CLAWCTL_DOCKER_SUBNET`, default `172.31.87.0/24`). Caddy's `trusted_proxies` stays unset so client-supplied forwarded headers are ignored. Hub trusts only that bridge.
- Perform an enroll/retire drill with a disposable agent in an isolated HOME. Use a keyed Linux installer (amd64 and arm64) or `clawctl-agent enroll` with private token input. Verify identity and a fresh HTTPS check-in, then retire. Keep the token out of chat.
- A live terminal WebSocket needs the agent's outbound link. HTTPS check-in alone does not establish it.

## Stop rules

- Do not exceed Always Free (one A1 shape totaling at most 4 OCPU and 24 GB, and 200 GB of block storage, including what already exists) unless the human agrees to pay. Never pass `--allow-paid` on your own.
- Do not launch a second VM when the tenancy's free A1 quota is already used.
- Names must start with `clawctl-`. Never terminate or delete a resource whose display name does not match the stack you were asked to manage.
- Never merge. Do not provision or destroy without the human confirming the name.
- Never paste secrets, API keys, SSH private keys, setup codes, passwords, TOTP secrets or codes, recovery codes, or enrollment tokens into chat.

## Troubleshooting

- **Out of host capacity:** the script retries AD-1, AD-3, then AD-2. A Pay As You Go upgrade often helps and is still free inside the limits above. Ask before upgrading.
- **80/443 blocked though the security list is open:** Oracle Ubuntu images REJECT every host port except 22. `install-hub.sh` inserts accepts before that REJECT and writes `/etc/iptables/rules.v4`. Re-run it; do not only change the security list.
- **ACME / TLS errors:** the public name must point at the VM. sslip.io does. A custom domain needs DNS first. Do not enroll real machines until that URL is the one in `CLAWCTL_PUBLIC_URL`.
- **Setup already closed (`/setup` 404):** use `/login`. Do not mine old codes out of logs.

To reach the VM over an SSH alias (for example Tailscale with public port 22 closed):

```sh
ops/oci/install-hub.sh --host myvm --public-host 203-0-113-10.sslip.io --dry-run
```

The alias uses SSH config's User, HostName, and ProxyCommand; `--user` overrides User and `--ssh-config FILE` selects a config file. Supply `--public-host` for private addresses or aliases resolving to DNS names. Otherwise an effective public IPv4 becomes a sslip.io name. Dry-run probes SSH and prints the remote setup plan without changing the VM.
