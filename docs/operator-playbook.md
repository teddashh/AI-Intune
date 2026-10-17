# Operator playbook

Use this playbook with the [API surface](API-SURFACE.md),
[operator authentication guide](OPERATOR-AUTH.md), and the linked runbooks.
All identifiers and access destinations must come from private, reviewed inventory.
No example below is permission to deploy, delete data, change access, or sign in
as another person. Record the human decision and a scrubbed outcome in the incident.
“Planned Hub feature” describes a gap or proposal, not a shipped capability.

## Pre-deploy snapshot and builder retries

**Symptom:** A release is ready, or a remote builder fails during deployment.

**Hub today:** Deployment pages manage agent workloads. Hub self-deployment remains an ops script; no UI/API snapshot receipt is added. `ops/fly/deploy.sh` gates deployment on a new, completed, recent Fly snapshot of every `clawctl_data` volume and prints its ID. The timeout defaults to 180 seconds per volume (`FLY_SNAPSHOT_TIMEOUT`); failure blocks deployment. Scheduled Litestream snapshots are a separate recovery mechanism.

**Operator steps:** Confirm app and region from private inventory and obtain deploy approval. Preview with `--dry-run`, then run the wrapper once. Record verified volume/snapshot IDs in private change evidence. `--skip-snapshot` or `FLY_SKIP_SNAPSHOT=1` bypasses the gate with a loud warning; review an alternative recovery plan before using it. Snapshots do not prove database consistency or cover external objects. Review retention and rehearse restore separately.

Each builder retries transient transport failures three times with bounded backoff before auto falls back Depot → remote → local. Permanent errors stop immediately; new classic builders are cleaned up before fallback. If cleanup fails, inspect the named builder before proceeding. Verify version and health after rollout. See [Fly deployment](DEPLOY-FLY.md) and [restore drill](runbooks/RESTORE-DRILL.md).

## Verify the deployed version

**Symptom:** The UI appears unchanged, or the operator cannot identify the running build.

**Hub today:** `GET /v1/operator/hub-status` returns build version to authorized
operators. The separate version-endpoint change adds unauthenticated `GET /version`
with only `version`, `commit`, `build_time`, plus a version/commit web footer.
Until that change is deployed, `/version` may return 404.

**Manual steps:** Request `/version` through the approved Hub origin after the
rollout. Compare the full commit to the reviewed release commit and the build time
to the release receipt. A `dev` version or `unknown` provenance is not deployed
commit proof. Check `/healthz` separately, then verify an authenticated read and
fresh agent observations. Do not infer version from a browser cache or a tag alone.

**Planned Hub feature:** A release comparison panel with desired/running build
provenance and rollout verification timestamps.

## Investigate Degraded machines

**Symptom:** A machine is `Degraded` (displayed as “Needs attention”).

**Hub today:** Fleet lists show concise reasons; machine detail shows findings,
judge reason, observation/check-in evidence, credentials and resource data.
Short summaries are bounded; inspect detail for all returned findings and any
truncation or missing-data indicators. A heartbeat is not workload-success proof.

**Manual steps:** Open detail from the affected row. Check measurement times and
missing/stale evidence before diagnosing. Separate agent, workload, disk, clock,
configuration and login findings. Review only the affected service logs and
bounded resource samples through an approved access path. For credentials, hand
sign-in to the assigned service user; for disk, use the approval workflow below.
Repair the cause after impact review, wait for fresh observations, and confirm
both the finding and workload symptoms clear. Preserve timestamps, not raw secrets.

**Planned Hub feature:** Reason-specific next actions, repair receipts, and
fresh-evidence confirmation; the separate degraded-UI change improves initial guidance.

## Scope service-token routes

**Symptom:** A bot is denied a needed read, or a token has excess authority.

**Hub today:** Admins can inspect exact allowlists and choose routes at creation.
Tokens have fixed scopes, expiry, optional source CIDRs, self-inspection and
rotation. The separate route-edit change adds per-token checkbox editing with
password plus a fresh TOTP and old-to-new audit diffs.

**Manual steps:** Have a human admin review the bot's actual task. Select the
smallest eligible manifest routes; do not grant broad machine evidence merely to
fix a missing summary read. Use view for read-only work and an appropriate scope
for approved operations. Once route editing is deployed, expand the live token's
editor and save with a fresh factor; check the audit diff and verify an intended
read plus a denied read. Changes apply to that credential immediately; other
credentials in its rotation chain retain their grants. If all old authority must
end, revoke the chain and issue a narrowly scoped replacement with an explicit
handoff window. Never paste token plaintext into tickets or command output.
See [operator authentication](OPERATOR-AUTH.md).

**Planned Hub feature:** Reviewed grant-change previews, chain-wide exposure
visibility and explicit replacement/revocation timing.

## Secret-scrubbed evidence for bots

**Symptom:** A bot needs incident evidence, but upstream text may contain credentials.

**Hub today:** Machine detail/evidence DTOs exclude dedicated secret fields, but
bounded free-text findings, credential notes/errors and run summaries are not
universally secret-redacted. Structural exclusion is not a general bot-safe contract.

**Manual steps:** Prefer enum states, counts and summary endpoints. A human must
review each free-text field before releasing evidence. Remove tokens, cookies,
authorization headers, env values, URLs with credentials/query secrets, private
access inventory, and sensitive log fragments. Send the minimum typed facts,
time window, source timestamp and explicit omissions. Do not treat a successful
secret-pattern scan as proof; unknown credential formats can evade it. Keep raw
evidence in approved restricted storage, with access references outside bot output.

**Planned Hub feature:** A dedicated bot-safe projection with enum-only reasons,
redaction/provenance flags, omission markers and secret-shaped regression fixtures.

## Disk cleanup approval

**Symptom:** Disk findings or low-space episodes suggest removing data.

**Hub today:** Disk reports, policies, previews and human approval/decision surfaces
exist; writes are feature-gated. Service tokens cannot approve destructive cleanup.

**Manual steps:** Inspect the affected mount and report freshness. Generate a
bounded preview and review exact candidates, exclusions, estimated recovery,
policy revision and backup/restore implications. Obtain the required human
approval for that preview; do not substitute a bot recommendation. Apply only
through the approved workflow when enabled. If candidates or policy change,
preview again. Review results and a fresh disk observation; do not claim recovered
space from an approval alone. See [disk maintenance](MAINTENANCE-DISK-CLEAN.md).

**Planned Hub feature:** Clearer end-to-end recovery receipts and links between
space episodes, approved previews and verified recovered bytes.

## Heartbeat or tool-call loops

**Symptom:** Frequent successful check-ins or repeated tool calls occur without task progress.

**Hub today:** Open **Attention / Loops** from the main navigation, or review the
Loop episodes panel on a machine. The default detector opens an episode above 50
identical calls in 10 minutes without reported progress. Human admins can change
the call threshold and window on that page. The bundled agent reports heartbeat
calls with successful observation uploads as progress; other tools must provide
optional `call_activity` evidence. Legacy agents and uninstrumented tools remain
unknown. An empty page does not prove that tasks are healthy.

**Manual steps:** Review machine, process/call signature, start, last report,
last progress, window count and rate. Compare with job transitions and task
checkpoints. Rate is averaged over the full configured window; a batch's actual
call timing is unknown. Inspect whether repeated calls are waiting on the same
dependency. Preserve counts and opaque signatures, never full arguments. Ask the
responsible operator to bound or stop the affected task after impact review;
frequent ordinary monitoring alone is not a reason to stop it.

Use **Acknowledge** to mark review while retaining the open episode. Use
**Resolve** after reviewing recovery; this resets the counting window, and
continued no-progress calls can open another episode. Explicit reported progress
also resolves an episode. Missing reports never resolve it. Confirm fresh progress
after repair, rather than treating successful heartbeats as task completion.
Read-only automation can use `GET /v1/operator/loops` or MCP `loops_list` with
optional machine filtering. This feature does not send notifications or control
processes.

## Memory pressure and OOM kills

**Symptom:** Services vanish or restart while memory is scarce.

**Hub today:** Resource snapshots show memory totals/availability, and restart
findings may exist. The Hub does not provide typed kernel-OOM/systemd-oomd cause
attribution; a restart alone is not an OOM diagnosis.

**Manual steps:** Correlate UTC incident times with the affected unit's status and
bounded journal entries, kernel OOM records, systemd-oomd events, cgroup
`memory.events` and pressure-stall samples. Use reviewed, narrowly scoped sudo
only where required. Record sample times, limits, killed unit and evidence source;
keep application logs private. Review workload concurrency/limits before an
approved change. Check fresh resource and workload observations afterward.

**Planned Hub feature:** Bounded PSI/cgroup evidence and typed exit causes linked
to affected services, incident windows and notifications.

## Runaway disk growth

**Symptom:** Free space falls quickly even though utilization has not crossed a threshold.

**Hub today:** Disk episodes use utilization thresholds. External monitoring can
forecast exhaustion from history, but no first-class short-window GB/hour growth
incident was found. Forecasting and sudden growth are different signals.

**Manual steps:** Take two or more timestamped measurements of the same mount;
calculate growth per elapsed hour and time-to-full, accounting for sample gaps.
Use bounded directory or file-size inspection to locate writers; check logs,
cache churn and deleted-but-open files. Avoid expensive recursive scans on a
stressed host. Preserve redacted size/time evidence, review the writer's retention
policy, and use the cleanup approval flow before deletion. Confirm the rate falls.

**Planned Hub feature:** Mount growth-rate episodes with sample coverage,
sustained-rate thresholds and time-to-full alongside utilization alerts.

## TLS expiry and SAN coverage

**Symptom:** A certificate is near expiry, or one access name fails TLS while another works.

**Hub today:** Deployment checks can validate TLS at deployment time. There is no
central endpoint-certificate expiry inventory; provider login expiry is unrelated.

**Manual steps:** Inventory each approved access name privately, its owner,
termination layer and renewal method. Probe each name with SNI and normal chain
and hostname verification; inspect issuer, not-after date and Subject Alternative
Names. Check every actual access name against SAN coverage, including alternate
access paths: a valid certificate for one name does not cover another. Do not
suppress verification to declare success. Track renewal lead time and stale probe
results, renew through the approved owner, then verify each name and chain again.
Keep private keys and private endpoint inventory out of shared evidence.

**Planned Hub feature:** Endpoint/owner inventory, verified-chain probes, expiry
alerts, SAN coverage-gap findings and renewal instructions with freshness markers.

## Host sudoers, service-user sign-in and chat setup through a jump host

**Symptom:** A job needs privilege, an AI CLI is unauthenticated, or a chat channel cannot send.

**Hub today:** Jobs/installers, some CLI/credential evidence, channel inventory,
bindings and tests exist. They do not constitute complete host IAM, service-user
sign-in or provider token provisioning. Human provider sign-in remains a handoff.

**Manual steps:** Use the approved jump-host path from private inventory when
required. Confirm the target and assigned service user before changing anything.
Review the host's exact helper commands and install the smallest per-host sudoers
scope; validate policy syntax and permitted/denied commands. Full sudo needs an
explicit reviewed exception. Sign in interactively as the assigned service user
through the provider flow, with the correct home and environment; never capture
or relay a person's credentials to a bot. For chat, resolve approved secret
references and install provider tokens using the existing secret provisioning
procedure with restricted file ownership/mode. Review bindings and inbound
provider setup before an authorized test, which may send an actual message.
Record only readiness and test results. See [new host](runbooks/NEW-HOST.md),
[AI CLIs](FLEET-AI-CLIS.md) and [channels](CHANNELS.md).

**Planned Hub feature:** A per-host readiness checklist for narrow helpers,
assigned user, sign-in handoff, secret-reference resolution and webhook setup.

## Jump-host-only machines

**Symptom:** Direct operator access fails, while an approved jump path is available.

**Hub today:** Agent observations, jobs and terminal/BAT actions depend on their
own enrollment, assignment and reachability. They cannot replace every bootstrap
connection. A jump host is a fallback or local requirement, not universal policy.

**Manual steps:** Confirm the documented path and target identity from private
inventory. Check jump-host credentials and target reachability separately. Use
the approved SSH proxy/jump configuration; keep credentials at their intended
boundary and avoid agent forwarding unless explicitly authorized. Diagnose outbound
agent-to-Hub connectivity separately from browser-to-Hub or SSH connectivity.
Do not open public inbound access as a shortcut. Re-establish the approved path,
then verify agent freshness and only the authorized terminal capabilities.
See [operator SSH](runbooks/OPERATOR-BOX-SSH.md).

**Planned Hub feature:** Reachability/access-path evidence and readiness guidance
that distinguishes jump-only bootstrap, agent transport and interactive sessions.

### Investigate sustained disk growth

A generic incident: a runaway writer consumes 12 GB/hour while disk utilization
is still below its percentage warning. Growth alerts detect this independently
of percent thresholds. Open **Disk alerts**, follow the machine link, and review
the top growing mount, window coverage, sample count, and estimated time to full.
Inspect the responsible workload before using reviewed cleanup actions; acknowledge
with a reason after assigning investigation. Acknowledgement does not clear an alert.

On **Disk policies**, create/edit `disk-default` for a fleet fallback or set a
growth override on a machine's scheduled user disk policy. Preview and apply the
revision with normal human reauthentication (disk policy writes must be enabled).
Policies without an override inherit `disk-default`, then Hub defaults of 10 GB/h
over 30 minutes. Startup overrides: `--disk-growth-gb-per-hour` /
`CLAWCTL_DISK_GROWTH_GB_PER_HOUR`, `--disk-growth-window-minutes` /
`CLAWCTL_DISK_GROWTH_WINDOW_MINUTES`; explicit flags take precedence over env.

At least three distinct observations must span 80% of the sliding window, with no
interval longer than half the window. Every interval must exceed the threshold;
cleanup interrupts sustained growth. Resizes, sparse coverage, future timestamps,
and measurements older than 15 minutes are unknown. Root samples are persisted
from agent check-ins; declared data mounts use authenticated safe-tier observations
collected by the disk reconciliation loop. Schedule sufficiently frequent previews
for data-mount coverage. Samples expire after 25 hours of Hub receipt time; restart retains history. The
latest per-mount growth evidence remains stored until replaced and is disclosed
in machine data holdings; it becomes unknown after 15 minutes.
Time-to-full is available only for positive growth and assumes a constant rate.
Two fresh non-sustained readings at least five minutes apart clear an episode;
unknown readings break that clearing sequence. Existing critical notification
channels, six-hour repeats, and acknowledgement preview/apply apply unchanged.

## Track TLS expiry and SAN gaps

A generic incident: a site's certificate renewed successfully, but an alternate
hostname was omitted from its SANs; clients failed while the primary name still
worked. Track expected names explicitly instead of relying only on expiry.

1. Open **Tenant → Certificates**. As a human administrator, add `example.test:443`,
   expected names `example.test www.example.test`, an owner and renewal notes.
   Keep credentials out of notes. Adjust warning days if needed (default 30/14/7).
2. Save and wait up to one minute for the first probe. Hub probes from its own
   network, so configure only destinations the Hub is authorized to contact.
   For private issuers, install the CA into Hub's system trust store. A monitor
   cannot bypass verification. No HTTP requests or redirects are followed.
3. Review the table, ordered by unknown expiry first and then shortest days left.
   Expired/expiry warnings require renewal; SAN gaps require reissuance covering
   every expected name; chain errors require the correct full chain, issuer trust,
   endpoint name and valid dates. `*.example.test` covers `www.example.test`, not
   `deep.www.example.test` or `example.test`.
4. A stale probe means no certificate evidence or evidence older than three hours.
   Check Hub availability, DNS, network access and TLS listener. Failed probes
   preserve the last certificate/expiry and show a separate failed attempt.
5. After the owner repairs the service, use **Probe within a minute** and confirm
   fresh evidence and cleared flags. Configure the existing Hub notifier to get
   condition alerts; successful delivery is limited to once daily per condition,
   with existing failure backoff. No recovery notification is emitted; the page
   shows current recovery evidence. Review notification health if alerts do not
   arrive. Alerts include generic flags and the authenticated Certificates link.
6. Use **Edit** to change targets, names, owner/notes or thresholds; saved edits
   reset observations. Reload and retry if a revision conflict occurs. **Remove
   monitor** stops monitoring and deletes its latest evidence, without touching
   the remote endpoint or changing existing notification/audit records.

API details and read-only MCP tools are documented in `docs/API-SURFACE.md`.
This workflow detects and tracks; the service owner still renews and installs
certificates through the normal reviewed change process.
