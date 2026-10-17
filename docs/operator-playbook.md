# Operator playbook

Use this playbook with the [API surface](API-SURFACE.md),
[operator authentication guide](OPERATOR-AUTH.md), and the linked runbooks.
All identifiers and access destinations must come from private, reviewed inventory.
No example below is permission to deploy, delete data, change access, or sign in
as another person. Record the human decision and a scrubbed outcome in the incident.
“Planned Hub feature” describes a gap or proposal, not a shipped capability.

## Pre-deploy snapshot and builder retries

**Symptom:** A release is ready, or a remote builder fails during deployment.

**Hub today:** Deployment pages manage agent workloads. They do not attest that a
fresh backup of the Hub's own volume exists. Scheduled Litestream snapshots are
separate from a provider volume snapshot. The Fly wrapper already falls back
from Depot to a classic remote builder and then, when supported, a local builder.

**Manual steps:** Before an approved Hub deploy, identify the exact app, region,
and mounted data volume from private inventory. Use the provider's volume
snapshot command to create a snapshot; wait for completion and verify the
snapshot's volume ID, creation time, retention and recoverability. Record its
identifier and age in private change evidence. Stop if the snapshot cannot be
verified; the current wrapper does not enforce this gate. Review the database
backup/restore procedure as well: a volume snapshot is not proof of database
consistency or coverage of external objects. Pin the source commit, review the
image and configuration, and obtain deploy approval. Run the existing deploy
wrapper once; inspect its builder-stage outcome before retrying. Do not repeatedly
launch deployments or leave classic builder apps behind. Verify version and
health after rollout; rehearse restore separately before relying on rollback.
See [Fly deployment](DEPLOY-FLY.md) and [restore drill](runbooks/RESTORE-DRILL.md).

**Planned Hub feature:** A fresh-volume-snapshot gate and deployment receipt with
snapshot provenance, builder attempts, deployed commit and restore coverage.

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

**Hub today:** Check-in history, job events, usage bounds and crash-loop findings
exist. Request limits bound some reads; they do not detect semantic no-progress
loops, and repeated heartbeats do not establish task completion.

**Manual steps:** Compare operation counts and repeated call signatures over a
bounded window with job transitions, task completion and agent restart times.
Check whether retries are waiting on the same dependency. Preserve counts and
redacted signatures, not full arguments. Ask the responsible operator to stop or
bound the offending task after impact review; do not stop ordinary monitoring
merely because it is frequent. Verify progress resumes after repair.

**Planned Hub feature:** Per-principal/machine no-progress episodes with rates,
start time, repeated signatures, acknowledgement and notifications.

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

### Investigate an OOM-killed workload

For an incident where a worker disappears despite a healthy agent heartbeat,
open **Machines → affected machine → Recent OOM kills** (also linked from
attention next steps). Review UTC time, source, unit/container scope, process and
pressure. oomd means userspace cgroup selection; kernel means a logged kernel
victim; memcg means a local cgroup kill-counter increase. Unknown process
attribution must not be guessed from the reporting service. Counter observations
are not exact kill times and can overlap kernel events.

Check the affected service's memory limit and workload demand before deciding
whether to change limits or restart it. Recent events promote fleet attention for
one hour; expiry is not proof of workload recovery. No restart or limit change
is performed by this feature.

If empty, use the linked Updates page to check the agent version and verify
read access to the system journal and cgroup v2 counters. Do not grant broad sudo
solely for collection. The collector runs without privilege escalation:
journal query timeout 3 seconds, 256 records, 256 KiB output, one-hour lookback;
capped dmesg fallback timeout 3 seconds, 256 records and 256 KiB when the journal
contains no kernel victim;
cgroup traversal at most 2,048 entries and 4 KiB counter files. First observation
after agent restart establishes counter baselines, so earlier counter-only kills
are unknown. Inaccessible journals, log rotation, busy hosts, old kernels and
short-lived removed cgroups can leave gaps. oomd pressure is shown only if the
kill record includes it; no current PSI sample is presented as historical
pressure. Do not put secrets in unit or process names.

For automation use GET /v1/operator/machines/{id}/oom-kills with a View principal
and an exact route grant for service tokens. It is a read-only projection of the
same evidence shown to humans.
