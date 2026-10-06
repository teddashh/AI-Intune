# Disk-clean maintenance

Disk-clean is a maintenance profile. The Hub stores it as desired state
`resource_kind=maintenance`, `resource_id=disk-clean`. The agent runs the
script embedded in the agent binary. The Hub, not the script, decides the
verdict.

This is not an artifact deployment. `CreateDeployment` always mints a new
desired revision, snapshots every non-retired member of a channel, and is
shaped around an artifact digest. Disk-clean keeps the same preview, digest,
idempotency, audit, and pause-after-canary rules on its own ledger:
`maintenance_rollouts`. The operator names the target list. That list does not
have to be every machine in the channel. A machine-scoped profile's only legal
target is that machine. A channel-scoped profile's targets must exist, not be
retired, and currently sit in that channel.

## Flow

1. Publish a profile revision. Preview, then apply with `preview_digest`,
   `expected_revision` (the scope's current revision, or 0), `confirm_scope_id`,
   a reason, and an `Idempotency-Key`. The same rendered conf does not mint a
   new revision. The preview does not promise the next number: the revision
   counter is shared by every scope.
2. Dry-run the named machines. The job is reversible. The agent always passes
   `--dry-run`, which forces dry-run even when the conf says `DRY_RUN=0`.
   Read the summary before anything that can delete.
3. Apply a canary of exactly one machine from that list. Every target,
   including the machines that will wait, must already have a succeeded dry-run
   of this same revision whose summary digest matches. Only the canary gets a
   job. The rollout pauses when that job succeeds.
4. Continue, with a new preview, opens the rest. The digest includes the
   rollout state, the control revision, and the opened batch. Abandon opens
   nothing more and refuses while a job is still running. The assignment stays
   after abandon.

Publishing alone does not assign the profile. Assignment is written when a
dry-run or a canary is applied.

A job is dry-run or apply by `irreversible`, not by a second desired revision.
The conf bytes and `config_digest` are the same for both.

## What the agent runs

The agent does not search `PATH`. It writes the embedded copy of
`ops/maintenance/disk-clean` to a private directory under its cache
(`clawctl/maintenance/disk-clean`, mode `0700`), checks the bytes, writes the
conf mode `0600`, checks that digest, then execs that absolute path with
`--scope` and `--conf`. `--dry-run` is added only when the job is not
irreversible. Before writing, the agent refuses the directory if it is a
symlink, not owned by the agent's euid, or group/other-writable.

The job passes only when the script exits 0, the summary parses, the echoed
`config_digest` matches, and the summary `mode` is the one the job implies:
`dry-run` for a dry-run job; for an apply job, `apply` when the profile has
`dry_run: false`, `mixed` when `dry_run: true` with `apply_categories`, and
otherwise `dry-run` (an apply of a pure dry-run profile is a no-op).

`--scope root` requires the agent process itself to be root. This agent runs
as the configured user (`User=@@CLAWCTL_AGENT_USER@@` in
`ops/clawctl-agent.service`). If it is not root, the job is rejected with
`PRECONDITION_FAILED`. There is no sudo. The user summary's `root` object, when
the host's root timer has written `/var/lib/disk-clean/last.json`, is evidence
only.

The spec carries `kind`, `schema_version`, `resource_id`, `scope`,
`config_digest`, the rendered conf, and the profile. It has no shell, no
operator command, and no path outside the closed keys. The agent validates the
spec again with the same Go rules and refuses otherwise.

Old agents are not sent this job. Check-in advertises
`maintenance_disk_clean_v1`. The Hub stores that on the exact check-in and
does not infer it from `agent_version`. Startup readiness still requires only
`device_sync_v1`, so a new agent talking to an older Hub is not stuck.

The flock stays at the script default (`~/.local/state/disk-clean`) so a user
timer already on the host shares the lock and `last.json`.

## Closed profile

Unknown JSON keys are refused. Bounds are enforced and the error names the
key. User categories are `user_tmp`, `tmp_globs`, `npm_cache`, `pip_cache`,
`uv_cache`, `go_cache`, `thumbnails`, `trash`. Root categories are `tmpfiles`,
`journal`, `pkg_cache`, `docker`, `snap`. `docker` prunes dangling images and
old build cache only. There is no volume, container, or `docker system prune`
option, and none can be added.

`tmp_dirs` is user-only and only `/tmp` and `/var/tmp`. `mount` is only `/`.
`extra_protect_names` can add tokens. It cannot remove the built-in list.
`tmp_glob_rules` are user-only, one segment under a directory in `tmp_dirs`,
and are refused when the glob would match protected data.

The Hub renders a deterministic conf. Header comments are part of the digest.
`config_digest` is `sha256:` of those exact bytes, the same value the script
prints for the file it was given. The job ships the conf and the digest. The
agent checks the file it wrote. The summary echoes the digest. The Hub
compares that echo to the assignment's expected digest.

## Protected names

The list lives in `maintenance.BuiltinProtect` and in the script's
`BUILTIN_PROTECT`. They are tested to be the same list. The Hub always renders
it. The script always merges it back in, so a conf that omits `PROTECT_NAMES`
or replaces it still protects every entry. The same names are excluded from
tmpfiles cleanup. A glob rule that targets one of them is refused by profile
validation and again by the agent. Because a pattern check cannot prove that
a glob never matches a protected name (`foo*` vs `foo-clawctl`), the script
also skips every individual glob match whose name is protected.

```text
systemd-private-* .X11-unix .ICE-unix .XIM-unix .font-unix .Test-unix
tmux-* ssh-* snap-private-tmp
*openclaw* *clawctl* *claude* *codex* *grok* *agy* *cursor* *vscode*
*backup* *snapshot* *.sock *.pid *.lock .X*-lock .s.PGSQL*
*odoo* *postgres* pulse-* dbus-* gpg-* krb5cc* hsperfdata_*
```

The script does not reboot, restart a service, install a package, or remove a
Docker volume or container.

## Evidence and verdict

A successful run prints one JSON line, schema `fleet-disk-clean/v1`. The agent
returns it as executor evidence: command, exit code, and that line as
`stdout_excerpt`, rule `disk_clean_summary`. The Hub stores a projection of
the line. The job can succeed on that evidence alone. The Hub verdict is a
separate reading and can fail after the job succeeded.

Verdict order:

- digest mismatch, including `none(defaults)` when a profile was expected: fail
- disk still under the assigned `disk_free_min_percent` after an apply or mixed run: fail
- attention non-empty: attention
- summary missing or older than 48 hours: stale
- otherwise: pass

The 48 hours is `maintenance.StaleAfter`. It is not a profile key. Age uses the
Hub clock (`received_at`). A summary that is exactly 48 hours old is not stale.
A missing summary is stale, not a digest failure.

The disk check uses the latest `resources` observation and the machine's
assigned `disk_free_min_percent` rule. No rule, or no usable byte counts, is
`unmeasured` and does not fail the verdict by itself. After an apply or
mixed run, an observation the Hub received before the summary (or that the
agent measured before the summary `ts`) is a pre-clean reading: the disk is
`unmeasured` until a newer observation arrives, so the "still over threshold
after cleaning" fail and alert are based only on a post-clean reading. The compliance board
still evaluates disk from the latest check-in. Those two readings are not
forced to be the same number.

`attention` on the summary is a hint. It does not decide the verdict on its own
beyond the rule above, and it does not delete anything.

## Alerts

The Hub's notifier (`deliver`, the same path as the daily report) is the only
transport. Every attempt is recorded in `notifications`. The minute loop
reconciles rollouts, then sweeps disk-clean, then runs the daily report. The
daily stamp and the daily watchdog are not touched.

A failed send is not retried every minute. Each alert kind backs off on the
daily report's schedule: the next attempt is at the last failure plus
1m, 2m, 4m, 8m, 16m, 32m, then 60m, counting failures since that kind's last
delivery. One kind's backoff does not hold back another machine's alert. With
no notify command configured, each kind records one undelivered row per day and
logs one warning.

Verdicts are computed on the read pool. The sweep holds the single writer only
to update `maintenance_alert_state`, and only for rows that changed.

A machine that has this profile assigned alerts when attention is non-empty,
when its latest summary is stale, or when an apply/mixed run left disk under
the assigned threshold. A dry-run does not raise the disk alert. The same
fingerprint is not sent again until it clears; an undelivered one is retried on
the backoff above. A change of fingerprint alerts again. Kinds look like
`disk-clean:<machine id>:<condition>`.

## Operator surface

JSON, under `/v1/operator/disk-clean/`:

| Method | Path | Grant |
|---|---|---|
| GET | `/summaries` and `/summaries/{id}` | view |
| POST | `/profile-preview`, `/profiles` | admin |
| POST | `/dry-run-preview`, `/dry-runs` | admin |
| POST | `/canary-preview`, `/canaries` | admin |
| POST | `/continuation-preview`, `/continuations` | admin |
| POST | `/abandonment-preview`, `/abandonments` | admin |

The same twelve calls are `disk_clean_*` tools on `clawctl-operator` (MCP and
`call`). Schemas are closed. Writes require `preview_digest` and an
idempotency key before any HTTP call. Continue and abandon also require the
expected control revision and opened batch. Local argument errors are
`invalid_arguments`.

The maintenance page (`GET /tenant/maintenance`) shows the board read-only:
verdict, staleness, digest match, the Hub disk evaluation, attention, and the
embedded root summary when the agent reported one. Writes stay on the JSON API.

## What this does not do

- It does not reboot, restart services, install packages, or prune Docker
  volumes, containers, or the whole Docker system.
- It does not send the job to an agent that has not advertised
  `maintenance_disk_clean_v1`.
- It does not apply past the canary without Continue.
- It does not add a notify transport.
- It does not turn per-host glob rules into per-machine assignments. One
  profile document is one scope (one machine, or the canary channel, or the
  stable channel).
- It does not migrate an existing hand-written conf. The rendered conf is the
  one the agent runs.
- It does not run `--scope root` unless the agent process is root.
- It does not choose the canary machine for you. The operator names it.
