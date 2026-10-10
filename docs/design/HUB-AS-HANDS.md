# Hub as hands: moving bot procedures onto the Hub

Status: **DRAFT for review.** No code in this PR.

## Opening principle: three layers, two tools

**Three layers. Escalate in this order: SaaS → Hermes → Grok Bot by hand.**

1. **Intune (the Fly Hub) is the SaaS.** Anything that can be configured inside it is configured there first,
   including scheduling. Fixed jobs (daily check, disk-clean audit, BAT sweep, backups, restore drill) are Hub
   schedules running catalog scripts, with a scope, an audit record, and approval for dangerous operations.
   The Hub is the only executor.
2. **Hermes is the AI assistant agent** (on the always-on cloud VM C4). It runs cron and judgment jobs **through the
   Hub MCP** and takes direction from the Grok Bots. It does not SSH into hosts or run scripts itself.
3. **Grok Bots are treated as people** (operators: the box bot and the per-host bots). They direct Hermes through the
   Hub MCP and go into a machine directly only when the first two layers fail, the way a human operator would.
   Anything a Grok Bot had to do by hand is a gap to fold back into layer 1 or 2. Nothing operational stays
   on the box, because box state does not persist.

**Two tools follow the same logic:**
- **Intune is for operations:** fleet health, maintenance, backups, upgrades, host lifecycle.
- **The BAT connector is for development:** coding sessions, permission prompts, session failover/cleanup,
  task milestones. It has its own SaaS layer (BAT server/task service), Hermes on top, and Grok Bots by hand last.
  Where a BAT procedure needs scheduling or audit, it is scheduled from the Hub and calls BAT. The development
  logic stays in BAT.

C4 is the fallback executor when Fly is down (§5).

Host labels used here: **W1** GPU workstation, **W2** corporate-network workstation, **C1–C4** cloud VMs (C3 = production
web app, C4 = agent-runtime host). The real-name mapping lives in the private overlay, never in this repo.

## 1. Inventory: what runs where today

The box and all six hosts were checked read-only on 2026-10-10.

### 1a. Operator box (does not persist)
| Item | What it does | Notes |
|---|---|---|
| `~/.ssh/config`: 19 host aliases; 7 via userspace `tailscale nc`, 8 via `cloudflared access ssh` | The only path into most hosts | Needs a userspace `tailscaled` (started by hand with sudo) and cached Cloudflare Access tokens in `~/.cloudflared`. Lost on every box rebuild. |
| `~/.local/bin`: bat-connector-mcp, bat-agent-connector-mcp, batc, bat-connect(-supervise), bat-launch, bat-provision, cloudflared(-access-ssh), ensure-tailscale, ts-tcp-forward.py, AI CLIs, hermes/hgw | Connector and tunnel installs | Installed by hand; no manifest. |
| `~/.hermes` (a full Hermes home: config, cron, scripts, kanban/state DBs, profiles) | A copy of the agent runtime | The cron config enables `fleet-health-daily` and `task-events-sweep`, but no gateway process was running at check time. The live runtime is on C4. |
| `/workspace/fleet-inventory/` (RUNBOOK.md, collect*.sh, `health/fleet-health.py` + daily JSON/MD) | Fleet health check that posts to Discord | The C4 cron points at this path (`fleet-health.sh`), but it only exists on the box. |
| `~/BAT-Fleet-Kit/` (SOP, NETWORK, CREDENTIALS, client) | BAT runbook and client kit | Docs plus a credentials index. |
| Per-bot daily-check state: `*-daily-state.json` for C3 and W1 (plus a W1 check-state file) | Routines written by the box bot (reachability, disk, backups, error counts, alert de-dup) | The routine itself is a bot instruction; its state is a box file. |
| `logs/cloud-hub/*`, `sot-tools/check-drift.sh`, the restore-drill harness, account helper scripts | Hub ops: deploy log, drift check, drill, account checks | Uses box-only admin credential files. |
| Box env secrets (Fly token, R2 token pair, Cloudflare token, Discord bot token, ...) | Credentials for all of the above | Injected per session; not usable from the Hub. |

### 1b. Agent runtime on C4 (`hermes-hgw.service`, user `hermes`)
| Cron job | Schedule | State | Script |
|---|---|---|---|
| bat-hourly-sweep | `17 * * * *` | **disabled** | `bat_hourly_sweep.py`: approve safe BAT permission prompts, fail over quota-exhausted sessions, run the session_cleanup gates, nudge idle sessions; sends a zh-TW digest |
| fleet-health-daily | `23 7 * * *` | **disabled** | `fleet-health.sh` → box-only `fleet-health.py` |
| bat-watch-* | every 15m | disabled | agent job |
| task-events-sweep | every 30m | disabled | `task-events-sweep.py` (safety net for milestones the task service never got acknowledged) |
| (main profile) fw-dispatch-watch | gate script | | free-workshop work-order queue |

So today **the BAT sweep and the fleet health check are not running anywhere**.

### 1c. Per-host timers and services (outside the distros' own)
| Host | Fleet maintenance | Host-specific jobs (stay local; reported as evidence only) |
|---|---|---|
| all six | `disk-clean.timer`, `disk-clean-weekly.timer` (user); `disk-clean-root.timer` on C1–C4 | — |
| W1, W2, C1–C3 | `bat-server-auto-update.timer`, `bat-server.service` | W1: codex-reaper, retention, backups, credential renewal, app crons (9 crontab lines), tunnels `bat-tunnel@C1..C3`, `cloudflared-*`; W2: openclaw-state-backup, memory-bridge cron, acme.sh |
| C1 | — | ERP backup timer |
| C2 | openclaw-rss-watchdog.timer; **Hub dead-man feed** (crontab: `hub-poll.sh` every 15m, daily `deadman.sh`) | app backup to Drive, weekly model-drift |
| C3 | — | ~12 app timers, an app DB backup, openclaw-state-backup |
| C4 | — | podman-auto-update, adb keepalive |
| all | `clawctl-agent.service` (Hub agent), `clawctl-hermes.service`, `openclaw-gateway.service` | |

The `fleet-daily-check` timer (merged in #40) is not installed on any host yet.

### 1d. Per-bot routines
Each per-host chat bot (W1, W2, C1–C4) has its own scheduled routines (daily check, reminders). Those routine
definitions are not visible from the box. **Action:** export each bot's routine list before migration (step 0 in §4).

## 2. Mapping: target tool, scheduler, scope, approval

**Layer** is who owns the procedure first; escalation goes down the layers only on failure. **Tool**: Intune = operations, BAT = development.
Scopes follow the Hub's existing grant levels: `view` < `operate` < `admin`. **Approval** means the call returns a
preview digest, and a human confirms in the Hub UI (or Telegram link) before the apply call runs. The existing
`*_preview` / `*_apply` + `preview_digest` pattern is reused.

| Procedure | Hub tool / script | Scheduler | Scope | Approval |
|---|---|---|---|---|
| BAT hourly sweep, report only | BAT | 1 SaaS (Hub schedule → BAT) | `bat_sweep_preview` (script `bat-sweep`, read mode) | Hub `17 * * * *` | view | no |
| BAT: approve non-destructive permission prompts | BAT | 1 SaaS (policy) / 2 Hermes (exceptions) | `bat_permission_approve` (per toolUseId, policy-checked) | Hub (sweep) | operate | no for the allowlist; **yes** for anything else |
| BAT: quota failover, session cleanup, idle nudge | BAT | 1 SaaS / 2 Hermes | `bat_session_failover`, `bat_session_cleanup`, `bat_session_nudge` | Hub (sweep); agent runtime ad hoc | operate | no (CLEAN_ONLY/KEEP); **yes** for MERGE_AND_CLEAN, ESCALATE |
| Fleet health daily (reachability, disk, failed units, backups, cert expiry, error counts) | Intune | 1 SaaS | `fleet_daily_check` → existing `fleet-daily-check` summaries collected by the agent | Hub `23 7 * * *` | view | no |
| Per-host daily checks (C3 app, W1) and their alert de-dup state | Intune | 1 SaaS | folded into `fleet_daily_check` with per-host check profiles; state in the Hub DB | Hub | view | no |
| Disk-clean (user/root timers) | Intune | 1 SaaS (host timers audited) | existing `disk_clean_*` tools; timers stay on hosts, Hub reads `last.json` | Host timers (Hub audits) | view; admin for profile publish / canary | **yes** for apply/canary/publish (already) |
| Install timers / daily check on a host | Intune | 2 Hermes | `host_timers_install` (`ops/maintenance/install-timers.sh`) | agent runtime on request | admin | **yes** |
| AI-CLI check / report | Intune | 1 SaaS | `ai_cli_check` (`ops/fleet/ai-cli/check-ai-clis.sh --json`) | Hub weekly | view | no |
| AI-CLI install / fix | Intune | 2 Hermes | `ai_cli_install` (`install-ai-clis.sh --apply`) | agent runtime | admin | **yes** |
| Hub backup (volume snapshot) + Litestream currency check | Intune | 1 SaaS | `hub_backup_snapshot`, `hub_replica_check` | Hub daily | admin (snapshot), view (check) | no |
| Restore drill | Intune | 1 SaaS | existing `restore-drill` + off-host variant on C4 | Hub monthly; C4 monthly cross-check | admin | no (read-only restore to tmp) |
| Hub upgrade | Intune | 2 Hermes (human approves) | `hub_upgrade_preview` / `_apply` (deploy.sh, records rollback image) | agent runtime on request | admin | **yes** |
| Hub dead-man (is the Hub alive) | Intune | outside the Hub (C2) | stays **off-Hub** (C2 cron) | C2 | — | — |
| Proxy / NTP / Tailscale-SSH checks | Intune | 1 SaaS; fixes 2 Hermes | `net_check` (proxy-check, ntp-check) | Hub weekly | view | no; fixes are **yes** |
| New-host bootstrap | Intune | 2 Hermes | `host_bootstrap_plan` / `_apply` | agent runtime | admin | **yes**, every phase that needs root |
| Host migration / tool removal / deploy template | Intune | 2 Hermes; 3 Grok Bot for cutover | `migrate_*`, `tool_removal_*`, `app_deploy_*` (scripts from #42) | agent runtime | admin | **yes** (freeze, cutover, remove, deploy) |
| Task-events safety sweep | BAT | 1 SaaS (BAT push primary) | `task_events_sweep` | Hub every 30m | operate (posts to threads) | no |
| Free-workshop dispatch watch | BAT | 2 Hermes | stays in the agent runtime (judgment) | agent runtime | — | — |
| Private overlay drift check | Intune (repo ops) | 1 SaaS | `sot_drift_check` (GitHub-side script, no box) | Hub daily | view | no |
| Box ssh config / tunnels | — | 3 Grok Bot only (break-glass) | **retired** as a procedure path: the Hub reaches hosts through the agent link; direct access is break-glass only | — | — | — |

**BAT (development), not Intune:** the BAT sweep and its parts (permission approvals, quota failover, session
cleanup, idle nudges), the task-events safety sweep, and free-workshop dispatch. The BAT server auto-update timers
are host maintenance, so Intune audits them as evidence. **Intune (operations):** everything else in the table.
For BAT items the Hub only schedules and audits; the actions are BAT connector calls.

## 3. What the Hub lacks

1. **Script runner.** Today the Hub runs no scripts on hosts. Agents only claim jobs with a fixed executor (`device_sync_v1`).
   Proposed: a new executor `script_v1`.
   - It runs only scripts from a Hub-published, content-hashed **catalog** (scripts in this repo, pinned by commit).
     Arguments are validated against a per-script JSON schema.
   - It runs as the agent user, with root only for steps marked `root` in the catalog and a sudoers rule limited to those exact paths.
   - Output is capped, redacted with the `fleet_redact` patterns, stored as job evidence, and limited by a timeout and a lease.
2. **Scheduler.** An in-process cron table in the Hub DB: job, cron expression, target selector, catalog entry, enabled flag.
   - It survives restarts, does not catch up on missed runs by default, and uses a single-flight lock per job.
   - Every run is a normal Job, so it appears in `/jobs` and the audit log.
3. **Scoped MCP token for the agent runtime.** The public Hub's operator API is session plus MFA only. Add **service principals**:
   - The token is hashed at rest, scoped (`view` / `operate`, never `admin` without an approval), can be bound to a tool
     allowlist and a source IP or tailnet node, expires, and can be rotated and revoked from the UI.
   - Over HTTPS, `clawctl-operator mcp` takes `--token-file`.
   - Approval-gated tools return a pending approval that only a human session can confirm.
4. **Secrets.** Fly secrets hold the Hub's own keys (R2, Telegram, Fly API for snapshots). Per-host script secrets are not sent
   from the Hub. They stay on the host in root-owned 0600 files referenced by name in the catalog.
   - The Hub DB holds password hashes and TOTP secrets, so the R2 replica is credential material. Keep it on its own key.
5. **Audit.** Extend the audit log with principal, tool, arguments digest, approval ID, job ID, exit status, and output digest.
   Add an export and a Telegram digest for every approval-gated action.
6. **Notifications.** Route sweep and check digests through the Hub notifier (Telegram today; add Discord webhooks per channel).
7. **Fallback runner on C4.** A small `hub-fallback` mode: the same catalog and scheduler, read-only tools by default,
   enabled only when the dead-man check says the Hub is down.

## 4. Migration order

0. **Freeze the inventory.** Export each chat bot's routines (§1d). Move `fleet-health.py` and the box daily-check logic into this repo.
   Record the box-only credentials that must move to Fly secrets or host files.
1. **Pilot: BAT sweep.**
   - Port `bat_hourly_sweep.py` into the catalog. Ship `script_v1`, the scheduler, and a service token (view/operate).
   - Run it in report-only mode from the Hub on W1 and W2 for one week, next to a manual comparison.
   - Then enable the allowlisted auto-approvals. Anything outside the allowlist goes through Hub approval.
   - Success criteria: the same digest as the old script, zero unapproved destructive actions, and every action visible in the audit log.
2. **Fleet daily check.** Install `fleet-daily-check` timers on all hosts (#40), then add the Hub `fleet_daily_check` job.
   Retire the box `*-daily-state.json` routines and the `fleet-health-daily` cron.
3. **Hub self-care.** Daily snapshot plus replica check, monthly restore drill, drift check.
4. **Hermes as the only judgment client.** Point Hermes (C4) at the Hub MCP with its service token.
   Grok Bots switch to directing Hermes through the Hub MCP, and go direct only on failure (record each fallback as a gap). Delete the box `~/.hermes` copy.
5. **Admin tools behind approval.** Timers install, AI-CLI install, net fixes, bootstrap, migration, tool removal, Hub upgrade.
6. **Fallback.** Enable `hub-fallback` on C4, run a drill (stop the Fly machine in a window, confirm the C4 takeover is read-only), and document it.
7. **Retire box state.** Drop the box ssh config and tunnels from the procedures. The box keeps only what a human session needs.

## 5. Risks

| Risk | Mitigation |
|---|---|
| **Central point of control.** A compromised Hub or token can act on every host. | `admin` actions are never available to service tokens without human approval. Catalog scripts are hash-pinned, and there is no free-form command. Agents verify the catalog signature or hash. Per-host allowlists. Rate limits. Approval notices go to Telegram. Keep MFA on human logins. |
| **Fly outage.** No scheduler and no tools. | The dead-man check on C2 detects it. `hub-fallback` on C4 runs read-only checks. Host timers (disk-clean, daily check) keep running locally because they never depended on the Hub. A restore from R2 to C4 is the documented rebuild path (restore drill: ~25 s for the DB). |
| **Hub DB or backup leak.** It holds TOTP secrets, hashes, and tokens. | Separate R2 key, private bucket, restore only to tmpfs, no copies on the box, a key rotation runbook. |
| **Script runs as root on many hosts at once.** | Canary, then batches (reuse the rollout/canary model). Preview digests. Per-job single-flight. Timeouts. Root only for catalog steps marked `root`. |
| **Agent runtime makes a bad judgment call.** | It holds view/operate only. Destructive tools need a human approval. Every call is audited. |
| **Network paths differ** (W2 corporate proxy, userspace-tailnet hosts). | The executor runs on the host through the existing agent link, so there is no SSH from the Hub. The proxy drop-in already ships with the agent installer. |
| **Migration gap:** a procedure stops while it is moving. | Run old and new side by side (report-only) per step. The BAT sweep and the health check are already stopped today, so the pilot only adds coverage. |
| **Secrets drift between Fly secrets, host files, and the box.** | A single list in the private overlay of which secret lives where. Nothing on the box. |
