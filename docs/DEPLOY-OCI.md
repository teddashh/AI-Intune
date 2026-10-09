# Deploy the Hub on an Oracle Cloud VM

One Always Free-sized Ampere VM, Docker, and Caddy. The VM is ordinary Ubuntu. Oracle's security list and the host firewall both have to allow ports 80 and 443, and the host firewall is the step people miss. Tailscale is not required.

The scripts are `ops/oci/provision.sh` (new VM) and `ops/oci/install-hub.sh` (a VM you already have). Both are idempotent. They only create or delete resources whose names start with `clawctl-`.

## The only account steps

Everything after this section is commands.

1. **Create an Oracle Cloud account.** If Ampere A1 capacity stays "out of host capacity", upgrade the tenancy to Pay As You Go. That upgrade does not by itself leave Always Free; it often unlocks capacity. Do not add paid shapes or extra boot volume until you mean to.
2. **Add an API key.** Install the [OCI CLI](https://docs.oracle.com/en-us/iaas/Content/API/SDKDocs/cliinstall.htm), then run `oci setup config`. The CLI generates the key pair. In the console open **My profile → API keys → Add API key** and paste the **public** key. Leave the private key in `~/.oci`. Never paste it into chat, a ticket, or a shell command.

## Provision

From a checkout of this repository, with `oci` and `python3` on `PATH`:

```sh
ops/oci/provision.sh --dry-run
ops/oci/provision.sh
```

Defaults:

| Choice | Default |
|---|---|
| Name | `clawctl-hub` (must match `clawctl-*`) |
| Shape | `VM.Standard.A1.Flex`, 2 OCPU / 12 GB |
| Larger free size | `--free-max` for 4 OCPU / 24 GB |
| Image | Ubuntu 22.04 aarch64 (`--os-version 24.04` also works) |
| Boot volume | 50 GB |
| SSH | port 22 from `0.0.0.0/0`, key only. Prefer `--ssh-cidr YOUR.IP/32` |
| HTTPS name | `<public-ip-dashed>.sslip.io` until you pass `--domain` |
| Region | the region in `~/.oci/config` |

`--dry-run` only reads (image, availability domains, current A1 usage) and prints the plan. A real run refuses, before it creates anything, when the request would exceed Always Free: 4 OCPU and 24 GB of A1 total, and 200 GB of block storage. `--allow-paid` overrides that refusal and can spend money. Do not pass it unless you have agreed to the charge.

Ashburn often returns "out of host capacity". The script tries availability domains AD-1, then AD-3, then AD-2, then sleeps (`--capacity-interval`, default 60 seconds) until `--capacity-max-wait` (default 30 minutes).

Cloud-init installs Docker and the Compose plugin, writes `CLAWCTL_AUTH_MODE=local` and `CLAWCTL_PUBLIC_URL`, and starts the Autopilot pack. It also opens host ports 80 and 443. Oracle's Ubuntu images ship an iptables `INPUT` chain that **rejects everything except SSH**. Opening those ports only in the security list is not enough. The script inserts the accepts **before** the REJECT rule and updates them in `/etc/iptables/rules.v4`, loaded at boot by `netfilter-persistent`, without saving live Tailscale or Docker chains.

When the instance is running, the script prints the public IP and the HTTPS URL. It then waits for `/healthz`. If you only want the VM back immediately, pass `--no-wait`; cloud-init still installs the Hub.

Finish the first admin on your machine, not by pasting secrets into chat:

```sh
ops/oci/install-hub.sh --host <public-ip> --public-host <hostname> \
  --admin-user admin --admin-out ~/clawctl-oci/admin.json
```

`install-hub.sh` is safe to run again. It waits until cloud-init has finished, then converges Docker, the firewall, and the compose stack. With `--admin-user` it calls `ops/fly/setup-admin.sh`, which enrolls TOTP and writes the password, TOTP secret, and recovery codes to a new mode-0600 file. It does not print those values.

Pick a DNS name you control and re-run with `--domain hub.example.com` (and a matching `--public-host`) **before** enrolling machines. A sslip.io name is fine for a first look and a bad Hub URL to bake into agents.

## A VM you already have

Any Ubuntu VM with SSH and passwordless sudo, Oracle or not:

```sh
ops/oci/install-hub.sh --host <ip-or-name> --dry-run
ops/oci/install-hub.sh --host <ip-or-name>
```

An effective public IPv4 from SSH config with no `--public-host` becomes `<dashed-ip>.sslip.io`. The same firewall fix runs here. The compose pack is the one in [Autopilot](AUTOPILOT.md): Caddy owns 80/443, Hub listens only on the Docker bridge, and `CLAWCTL_TRUSTED_PROXIES` is that bridge so the audit log shows the real client IP.

## Remove the stack

```sh
ops/oci/provision.sh --name clawctl-hub --destroy --dry-run
ops/oci/provision.sh --name clawctl-hub --destroy
```

Type the name when asked. Destroy terminates that instance and deletes only its VCN, subnet, security list, and internet gateway. It will not select a resource whose name does not match.

## Checks

- `https://<host>/healthz` returns 200 with a valid certificate. Do not pass `-k`.
- Wrong Host on `/login` returns 421 when the request reaches Hub. `/healthz` is not that check. Caddy only serves the configured site, and the Hub image has no shell, so from the VM run `docker run --rm --network clawctl-autopilot_default curlimages/curl:8.11.1 -sS -o /dev/null -w '%{http_code}\n' -H 'Host: wrong.example' http://hub:8787/login` and expect 421.
- After enrollment, a password alone does not open a session. Finish the authenticator prompt privately.
- The audit client IP is the caller's address, not a Docker bridge address. The pack trusts only its own bridge and Caddy does not treat the client as a trusted proxy, so a caller-supplied `X-Forwarded-For` is ignored.
- Enroll one disposable agent from an isolated `HOME`, confirm a new check-in, then retire it. Keep the token out of chat.

## When it fails

- **Out of host capacity.** Let the retry run, or upgrade to Pay As You Go and run the script again. It reuses anything it already created.
- **Ports 80/443 time out, security list is open.** The host iptables REJECT is still in front. Re-run `install-hub.sh`.
- **Certificate errors.** The name must resolve to this VM. sslip.io does. A custom name needs an A record before Caddy can finish ACME. Do not enroll agents until HTTPS for the final name works.
- **Setup already closed.** Use `/login`. Do not try to recover an old setup code from logs.

To reach the VM over an SSH alias (for example Tailscale with public port 22 closed):

```sh
ops/oci/install-hub.sh --host myvm --public-host 203-0-113-10.sslip.io --dry-run
```

The alias uses SSH config's User, HostName, and ProxyCommand; `--user` overrides User and `--ssh-config FILE` selects a config file. Supply `--public-host` for private addresses or aliases resolving to DNS names. Otherwise an effective public IPv4 becomes a sslip.io name. Dry-run probes SSH and prints the remote setup plan without changing the VM.
