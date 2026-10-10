# New Linux host

Use Ubuntu 22.04, 24.04, or 26.04, or Oracle Linux 9. Use amd64 or arm64.
Create the VM with a non-root account named `ops`, an SSH key, and sudo access.
Keep enough free disk for the agent, its jobs, and a 500 MB journal.
Clone this repository on the host. The scripts require Bash, Python 3, curl,
systemd, and the usual Linux account and file tools.

## Preview and prepare

Run as `ops` from the checkout:

```bash
bash ops/fleet/bootstrap/bootstrap-host.sh --hub https://hub.example.com --hostname host-a
bash ops/fleet/net/proxy-check.sh --hub https://hub.example.com
bash ops/fleet/net/ntp-check.sh
```

Preflight reads the OS, architecture, user, sudo readiness, disk, clock, DNS,
and Hub health directly and through `/etc/environment`'s proxy. It writes no
state or config. Fix failed checks before enrollment. See
[Corporate network](CORPORATE-NETWORK.md).

For optional Tailscale enrollment, put a short-lived auth key in a root-owned
file outside the checkout, mode 0600. Never put its value in a command.
Add `--tailscale-auth-key-file /root/tailscale-key --hostname host-a`.
Bootstrap installs Tailscale with its official installer when it is missing,
sets `--ssh=false`, verifies its IPv4 address, and shreds the key after success.
Without a key, this phase is skipped. The keyed agent installer can also set up
Tailscale. Do not pass a shredded key to a later enrollment.

## Apply host settings

```bash
sudo bash ops/fleet/bootstrap/bootstrap-host.sh --apply --resume \
  --hub https://hub.example.com --hostname host-a \
  --phases preflight,journald,disk-clean,ai-cli,linger
```

This caps journald at 500 MB without restarting it, enables linger for `ops`,
and calls optional timer and AI CLI installers when present. The timer installer
is called with `--scope user` and `--scope root`, plus `--plan` or `--apply`.
The AI CLI installer receives `--plan` or `--apply`. Missing scripts are skipped.
`FLEET_TIMERS_INSTALLER` and `FLEET_AI_CLI_INSTALLER` override the delegated installer paths for tests and custom checkouts.
Non-root apply prints root commands and records `needs-root`; it does not invoke sudo.
Review each printed single-line command before running it.

State lives in `~/.local/state/fleet-bootstrap/state.json`. When invoked through
sudo, this is the invoking user's home. Each phase has a status and UTC time.
`--resume` skips only `done`; failed, skipped, and needs-root phases are retried.
Omit `--resume` to reassess completed phases after changing the Hub or options.
Existing config files get `.bak-<UTC stamp>` backups before changes.
`--state-dir` and `--root-prefix` support isolated tests; the prefix redirects
config files only, not service commands. Do not use it as a service sandbox.

## Enroll one host

On the Hub, create an enrollment ticket and download the keyed Linux package
for the host architecture. Transfer it privately. See
[Fly keyed installer](../DEPLOY-FLY.md#6-enroll-your-first-machine).
Stage it as `ops`, outside the checkout:

```bash
mkdir -m 700 "$HOME/clawctl-enrollment"
tar xzf /path/to/keyed-linux-package.tar.gz -C "$HOME/clawctl-enrollment"
chmod 600 "$HOME/clawctl-enrollment/enroll-token"
bash ops/fleet/bootstrap/bootstrap-host.sh --phases agent \
  --package-dir "$HOME/clawctl-enrollment" --hub https://hub.example.com
bash ops/fleet/bootstrap/bootstrap-host.sh --apply --phases agent \
  --package-dir "$HOME/clawctl-enrollment" --hub https://hub.example.com
systemctl is-active clawctl-agent
```

The staged directory must be owned by `ops` and mode 0700. Its token must be
owned by `ops` and mode 0600. Run this phase as `ops`, not root. The staged
`install-agent.sh` runs as `ops` and runs its own sudo preflight, so a host that
needs a sudo password asks for it there. If bootstrap itself runs under sudo, it
hands the installer to `ops` with `runuser`. Installer output is shown, but
passed through a filter that replaces the token file's value with `[redacted]`
(the installer does not print it; the filter is a second guard).
A failed enrollment is recorded as failed; inspect service status and retry.
Bootstrap accepts `--token-file`, `--no-tailscale`, `--no-container-runtime`,
`--no-proxy-dropin`, and `--reenroll` for the staged installer.
For migration, follow [Moving agents](../MOVE-AGENTS.md).

Verify the expected identity and a fresh Hub-received check-in on the machine
page. Local service status alone is not enrollment evidence. Then remove the
staged directory, archive, and transfer copies. Keep config backups private.

After SSH through Tailscale works, consider closing public TCP 22 in the cloud
firewall manually. Bootstrap disables Tailscale SSH; use OpenSSH over the tailnet
unless you separately enable Tailscale SSH and configure its policy. Test access
before changing firewall rules.

## Daily check

```bash
systemctl is-active clawctl-agent
bash ops/fleet/net/proxy-check.sh --hub https://hub.example.com
bash ops/fleet/net/ntp-check.sh
```

Check the latest check-in and evidence on the Hub. Review timer status and disk
space. Disk-clean behavior is described in [maintenance](../../ops/maintenance/README.md).
