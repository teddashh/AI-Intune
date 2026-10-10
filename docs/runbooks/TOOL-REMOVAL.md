# Tool removal

Inventory first. Confirm dependencies, back up, remove exact manifest entries,
then verify. Keep project data and git worktrees. Use names, file paths and
counts as evidence. Never copy config values or tokens into logs.

```bash
bash ops/fleet/tool-removal/inventory.sh --name retired-ide --pattern 'retired-ide'
bash ops/fleet/tool-removal/remove.sh --name retired-ide --manifest removal.manifest
bash ops/fleet/tool-removal/remove.sh --name retired-ide --manifest removal.manifest --apply
bash ops/fleet/tool-removal/verify.sh --name retired-ide --pattern 'retired-ide|:8080'
```

Inventory reports bounded matching names (first 50 per source, then a remaining
count) for system/user unit files and timers, packages, containers/images, paths
and listening port numbers plus process names. `--json` emits one object.
Process argv, cron, rc files and AI config content are counts only. It inventories
dpkg/rpm/snap/flatpak/npm/pip packages, containers, binaries and directories.
It reports counts for Claude settings hooks and statusLine,
Codex hooks, Gemini settings, Grok hooks, rc files, cron and web references.
Run in each affected user's login session and inventory system cron separately.
Counts are evidence to review, not authorization to delete.

Before removal, inspect what else references the tool. Include reverse proxy
routes, docker mounts, service dependencies, firewall rules and shell startup
files. Read them privately; do not print credential-bearing values. Resolve
each dependency before applying. Stop unscoped processes manually by verified
PID; the helper kills only processes scoped to exact listed units.

The manifest uses unquoted KEY=VALUE lines and whitespace-separated exact
names and paths. It is parsed, never sourced. No shell expansion is performed.
Only these keys are accepted (ALLOW_PACKAGES is an optional explicit override):

```text
USER_UNITS=retired-ide.service
SYSTEM_UNITS=retired-ide.timer
PACKAGES=retired-ide
ALLOW_PACKAGES=
PATHS=/opt/retired-ide
HOOK_PATTERN=retired-ide
PORT=8080
```

PATHS must be absolute under the invoking HOME, /opt, /usr/local or /etc/systemd.
The directory roots themselves, `..`, symlinks and detected git projects are
refused. Paths with whitespace are unsupported. Inspect directories for project
data before listing them. Exact dpkg/rpm packages are supported by apply;
handle snap/flatpak/npm/pip with separately reviewed exact commands after backup.
Never use package globs. Check package file lists and dependencies before purge.

Apply as the invoking user first. It creates a private 0600 backup tarball of
user paths, user unit files and pre-edit JSON in
`~/<name>-removal-backup-<UTC>-<suffix>/` before deletion. It stops and disables
exact user units, removes their files, strips hooks and removes listed HOME
paths. It exits successfully with “root part pending” and exactly one
single-line `sudo bash -c '...'` hint using absolute script and manifest paths.
Run that printed line to complete system cleanup; sudo is unnecessary for the
user phase. The line preserves HOME and passes `--apply --root-only`.

Root apply handles system units, exact packages, paths outside HOME and the
specified firewall port. When HOME belongs to a non-root user, it also runs the
user phase through `runuser`. The user bus must be running for user units.
Root backups include system files, package file lists, and `iptables-save` /
`ip6tables-save` output. A non-empty archive is required before deletion.
Firewall cleanup deletes every INPUT rule with the exact `--dport PORT` token
pair in both families and reports the count; it never flushes chains. Review
proxy routes and remaining listeners separately. Backups contain secrets;
keep them private. JSON edits also create private `.bak-<UTC>` backups.

The JSON-aware stripper removes matching command entries only under hooks or
statusLine, preserving other JSON values. It handles nested hook arrays in
Claude/Codex/Gemini. Review Grok's hook format manually; it is inventory only.
Preview and apply both report filenames and change counts, never commands.

```bash
python3 ops/fleet/tool-removal/strip-hooks.py --pattern retired-ide ~/.claude/settings.json
python3 ops/fleet/tool-removal/strip-hooks.py --apply --pattern retired-ide ~/.claude/settings.json
```

Verify fails on real matches or invalid supported JSON. Unavailable sources
are reported as `skipped` and do not fail verification. Collect missing evidence
manually when needed. Reference scans emit filenames only, use a HOME depth
limit of six, skip node_modules, .cache and .git, and exclude `.bak-*`,
`backup-*` and removal backup directories/tarballs. Matching repositories and
worktrees are reported as “project data (not removed)”; retain and review them.
Retained references can still make verification fail; never delete projects
just to pass verification. Restore the private
archive and config backups to roll back, reinstall exact packages if needed,
then restore recorded unit enablement and firewall rules.

## Worked removal example: Orca IDE

Orca IDE is retired. This example only describes removal. Inventory its exact
installed package and units. GNOME `orca` is a different screen reader package.
If the IDE package is `orca-ide`, put only `orca-ide` in PACKAGES. The built-in ambiguous-name guard refuses `orca` (also `screen` and `code`)
unless `ALLOW_PACKAGES=orca` explicitly repeats it in the manifest. For IDE
removal leave that override absent; never infer packages or use `orca*`. Preserve IDE project worktrees.
Use an IDE-specific hook pattern; a broad `orca` match may remove unrelated
accessibility integration. Verify those accessibility tools still work after
removing the IDE.
