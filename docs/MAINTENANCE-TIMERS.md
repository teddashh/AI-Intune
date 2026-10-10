# Local maintenance timers

Systemd runs the allowlist cleaner locally. Bots read its summary files.
No AI agent needs to scan a disk. Python 3, Bash, systemd, and sha256sum
are required. Run user commands as the account that owns the user timers.

The local and Hub jobs use the same script. Keep
`ops/maintenance/disk-clean` and its embedded agent copy identical.
Do not run both schedules in apply mode for the same scope on the same
host unless that is intended. They share a scope lock and summary file.

## Install

From the checkout, preview each scope:

```bash
bash ops/maintenance/install-timers.sh --scope root
bash ops/maintenance/install-timers.sh --scope user --with-daily-check
```

Install root timers as root. The installer prints a single-line sudo command
when a non-root account requests root apply. It never invokes sudo itself.

```bash
sudo bash ops/maintenance/install-timers.sh --scope root --apply
bash ops/maintenance/install-timers.sh --scope user --with-daily-check --apply
```

Run the printed `sudo bash -c '...'` linger step so user timers run after logout.
It enables linger for the invoking account, such as `ops`.
Existing config stays in place. A differing template is reported without
printing config contents. `--conf FILE` selects the template for a new config.
Installed cleaner bytes are checked against the checkout with sha256sum.
Changed files get `.bak-<UTC stamp>` backups. Repeated installs keep identical
files. Only timers are enabled; services are not started by the installer.

`--prefix /absolute/staging/path` stages the filesystem layout for tests or
inspection. It does not call systemctl or loginctl. User paths are placed
under the prefix followed by the invoking account's absolute home path.

## Schedules and files

| Scope | Schedule (UTC) | Config | Summary |
| --- | --- | --- | --- |
| Root | Daily 03:40, random delay up to 40 minutes | `/etc/disk-clean/root.conf` | `/var/lib/disk-clean/last.json` |
| User | Daily 04:30, random delay up to 45 minutes | `~/.config/disk-clean/config` | `~/.local/state/disk-clean/last.json` |
| User weekly | Sunday 05:30 | Same user config | `~/.local/state/disk-clean/weekly.json` |
| Daily check | Daily 05:15 | None | `~/.local/state/fleet-daily-check/summary.json` and `summary.txt` |

Timers are persistent. Catch-up runs can happen after boot or login.
The daily check follows the user cleaner when it is already running.
Schedules do not guarantee a long cleaner run has finished; a stale warning
can clear on the next check.

Root installs `/usr/local/sbin/disk-clean`; user installs `~/.local/bin/disk-clean`.
Root tmpfiles rules age `/tmp` at 10 days and `/var/tmp` at 14 days. Protected
state, backups, snapshots, sockets, and sessions are excluded. Tmpfiles rules
also affect the host's own systemd-tmpfiles schedule, independently of the
cleaner's DRY_RUN setting. Review this policy before installing root timers.
Changing cleaner age keys does not change the installed tmpfiles rules.

The journald drop-in includes `[Journal]` and `SystemMaxUse=500M`. The
installer never restarts journald. The cap applies at next boot; approved
daily journal vacuum enforces it until then. journald ignores a
`SystemMaxUse=` line that is not under a `[Journal]` header. The daily check
reads `journald.conf` and every `journald.conf.d/*.conf`, and warns by file
name when it finds such a line.

The weekly service uses the cleaner's existing bounded inventory routine.
Bots consume its output; they do not run inventory commands themselves.

## Approve

New configs have `DRY_RUN=1`. Review the dry-run summary first.
Approve each scope separately:

```bash
sudo bash ops/maintenance/install-timers.sh --scope root --apply --approve
bash ops/maintenance/install-timers.sh --scope user --apply --approve
```

`--approve` backs up the config and sets `DRY_RUN=0`. For staged approval,
keep `DRY_RUN=1` and set, for example, `APPLY_CATEGORIES="journal"` in the
root config. Back up the file before editing it. To force a single preview:

```bash
~/.local/bin/disk-clean --scope user --dry-run
```

## Read the evidence

```bash
cat ~/.local/state/fleet-daily-check/summary.txt
python3 -m json.tool ~/.local/state/fleet-daily-check/summary.json
~/.local/bin/fleet-daily-check --json
~/.local/bin/fleet-daily-check --write
```

The check never deletes anything. It reads the two fixed cleaner summaries,
system and user failed units, cleaner timer triggers and service results,
`df -P` for `/` and `/home`, journal size, the journald drop-in, clock sync,
and the reboot-required flag. It runs no disk inventory commands.
`--write` replaces each output file atomically with mode 0600. Compare `ts`
if reading both files during a write. `--state-dir DIR` changes the output
location. Without `--write`, the checker does not write any state.

Exit codes are 0 for ok, 1 for warn, and 2 for crit. Missing, invalid, or
older-than-48-hour summaries warn. Attention, failed units, inactive timers,
failed services, stale triggers, an unsynchronized clock, or a reboot flag
warn. An active timer that has not fired yet (for example a weekly timer
installed mid-week) is recorded as `first_run_pending` and does not warn.
On hosts with no root access (user timers only), pass `--skip-root` so the
missing root summary and root timer do not warn. Weekly triggers may be up to eight days old.
Disk usage at 85% warns; at 95% it is critical. Reasons are fixed diagnostics;
summary mode, age, freed bytes, and attention presence are projected without
copying arbitrary notes or unknown fields into the report.

## Remove

```bash
bash ops/maintenance/uninstall-timers.sh --scope user
bash ops/maintenance/uninstall-timers.sh --scope user --apply
sudo bash ops/maintenance/uninstall-timers.sh --scope root --apply
```

Preview is the default. Apply disables installed timers and removes managed
units and executables with backups. Config is kept unless `--purge` is added.
Summaries, backups, and linger are kept. Journald is never restarted.
