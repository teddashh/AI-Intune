---
name: fleet-daily-check
description: Read local systemd disk-clean and daily host check summaries. Use for daily host health, timer evidence, and warn or crit follow-up without scanning disks. Usable by Claude, Codex, and Grok Bot.
---

# Daily host check

Read `~/.local/state/fleet-daily-check/summary.txt` first. Read `summary.json`
for timestamps, reasons, disk percentages, cleaner modes, freed bytes, and
attention flags. Check the timestamp before reporting a healthy host.

If evidence needs refreshing, run `~/.local/bin/fleet-daily-check --json`.
Exit 0 is ok, 1 is warn, and 2 is crit. This command reads fixed evidence
and never deletes anything. `--write` refreshes the saved summaries.

Never run `du` or `find` over disks. Never inspect token contents. Do not
copy arbitrary cleaner notes into tickets or public logs. Read the fixed
cleaner summaries at `~/.local/state/disk-clean/last.json` and
`/var/lib/disk-clean/last.json` if more scope evidence is needed.

For warn, report the reason and evidence age. Check the named timer or
service status. A missing root summary can need a root timer installation;
it is not proof that cleanup ran. Do not approve deletion to clear a warning.

For crit, report the mount and percentage promptly to the operator. Keep
cleanup in dry-run until the operator approves the scope or category. Do not
remove files, restart journald, or reboot as a daily-check response.

Installation, approval, rollback, and Hub schedule overlap are in
[MAINTENANCE-TIMERS.md](../../docs/MAINTENANCE-TIMERS.md). Use its concrete
commands. Root steps use the installer's single-line sudo hint. Follow
existing authorization for changes and keep secret values out of output.
