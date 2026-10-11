# Operator MCP and CLI

`clawctl-operator` is a client of the existing Hub JSON API (`/v1/operator/*`).
The Web UI uses the same routes. This process does not open SQLite and does
not add an operator login.

Deploy Hub first: [DEPLOY-OSS.md](DEPLOY-OSS.md). The agent procedure is
[`skills/clawctl-operator/SKILL.md`](../skills/clawctl-operator/SKILL.md).

## Identity

Run `clawctl-operator` on a tailnet node that already has the Hub grants
(`view`, `operate`, `admin` as separate keys). Hub calls Tailscale LocalAPI
`WhoIs` on the TCP source of each request. The source address is the node
this process runs on.

Set the origin with `--hub-url` or `CLAWCTL_HUB_URL`:

```text
http://<hub-tailscale-ipv4>:<port>
```

`<port>` is the port in Hub's `CLAWCTL_LISTEN`. Hub has no production default
port. `8787` in other examples is only the conventional example. The `--listen`
flag default `127.0.0.1:8770` is refused.

That is the same literal Tailscale listener as the operator UI. There is no
path, userinfo, query, or `https` hostname for this client. The command sends
no `Authorization` header and does not read `X-Forwarded-*`. A proxy in the
environment is not used.

`clawctl-hub` subcommands can still discover `operator.json`. This binary does
not. Pass the origin explicitly so an agent does not pick up another file.

The HTTP user agent is the shared operator client. It is a label in the audit
trail, not a credential.

Capability is still the route's existing requirement. A read uses `view`. A
deployment continue preview uses `operate`. Enrollment, deployment create,
abandon, profile assignment, and every disk-clean write use `admin`.
Disk-clean reads use `view`. Missing a grant is an error from Hub. The tool
does not retry with a different identity.

## Commands

```bash
make operator
./build/clawctl-operator --hub-url "$CLAWCTL_HUB_URL" tools
./build/clawctl-operator --hub-url "$CLAWCTL_HUB_URL" call fleet_overview
./build/clawctl-operator --hub-url "$CLAWCTL_HUB_URL" mcp
```

`mcp` speaks newline-delimited JSON-RPC on stdin/stdout (one JSON object per
line). It also accepts a `Content-Length` request frame and still answers with
one JSON line. Logs stay off stdout.

`call <tool> [json]` prints the tool result as one JSON line. Errors go to
stderr as `{"code","message"}`.

Local argument errors use code `invalid_arguments`: bad JSON, a missing or
invalid field, an unknown tool, and an unknown field. An unknown field is
named, and the message lists the allowed fields for that tool (`(none)` when
the tool takes no fields). Errors that come back from Hub keep the Hub code.
A transport failure that is not a Hub error object stays `hub_error`.
`preview_digest_required`, `expected_revision_required`, and `canary_blocked`
stay their own codes. The MCP `tools/call` path and the CLI `call` path use
the same `Call` result.

`initialize` reports `serverInfo.version` as this binary's build version
(`main.version`, injected with `-ldflags`, `dev` when unset). That is not a
protocol constant. Protocol version negotiation is unchanged.

`tools/list` includes MCP `annotations` on every tool: `readOnlyHint` (true
for reads and previews), `destructiveHint` (true for apply, continue, expand,
abandon, and profile assignment; false for `enroll_ticket_create`),
`idempotentHint` (true for reads, previews, and writes that take an
idempotency key), and `openWorldHint: false`. Input schemas set `enum`,
`minLength` / `maxLength` / `pattern`, and `minimum` / `maximum` from the Hub
validators, plus `additionalProperties: false` and `required`. They do not
add a tighter limit than Hub enforces. `expected_control_revision` has a
minimum and no JSON Schema maximum, because the Hub maximum is the maximum
int64.

## Tools

Reads: `fleet_overview`, `machines_list`, `machine_get`, `machine_evidence`,
`jobs_list`, `job_get`, `job_evidence`, `deployments_list`, `deployment_get`,
`software_report`, `compliance`, `rollout_status`, `disk_clean_summaries`,
`disk_clean_summary`.

Previews (they return `preview_digest`): `enroll_ticket_preview`,
`deployment_create_preview`, `deployment_continue_preview`,
`deployment_abandon_preview`, `profile_assignment_preview`, `rollout_preview`,
`disk_clean_profile_preview`, `disk_clean_dry_run_preview`,
`disk_clean_canary_preview`, `disk_clean_continue_preview`,
`disk_clean_abandon_preview`.

Writes: `enroll_ticket_create`, `deployment_create`, `deployment_continue`,
`deployment_abandon`, `profile_assignment_apply`, `rollout_apply`,
`rollout_expand`, `disk_clean_profile_publish`, `disk_clean_dry_run_apply`,
`disk_clean_canary_apply`, `disk_clean_continue_apply`,
`disk_clean_abandon_apply`.

A write requires `preview_digest` copied from a preview response
(`sha256:` and 64 lowercase hex). The write call does not create a preview
for you. Deployment continue, abandon, and `rollout_expand` also require
`expected_control_revision` and `expected_opened_batch` from the deployment
you just read, plus an `idempotency_key` you choose and reuse for a retry.
Disk-clean continue and abandon use the same two fields from
`disk_clean_continue_preview` or `disk_clean_abandon_preview`. Profile publish
uses `expected_revision` from `disk_clean_profile_preview` (`current_revision`,
which is 0 when the scope has no revision yet).

## Disk-clean

`disk_clean_*` does not call the artifact deployment tools. Publish a profile,
run a dry-run, read the evidence, then open a canary of exactly one machine.
`disk_clean_continue_apply` is the only way to open the rest.
`disk_clean_abandon_apply` opens nothing more. The design is
[MAINTENANCE-DISK-CLEAN.md](MAINTENANCE-DISK-CLEAN.md).

`fleet_overview` is the first page of `GET /v1/operator/machines`. Totals are
the fleet index. `page_truncated: true` means `items` are not every machine.

## Canary rollout

The phase names live in `internal/rollout`. The deployment page, `rollout_status`,
and `rollout_preview` all use `AssessCanary`.

1. `rollout_preview` calls deployment create preview with `batch_size` 1
   (omit the field, or set 1). Any other batch size is rejected before HTTP.
2. Read `canary.phase` and `preview.create_allowed`. The first included batch
   must be exactly one machine (`singular_canary`). The preview lists that
   machine.
3. `rollout_apply` sends that `preview_digest`. It does not preview again.
4. Poll `rollout_status`.
   - `canary_pending`: Hub has not marked the canary job `succeeded`. Wait.
   - `canary_stopped` or `expand_stopped`: stop. `rollout_expand` returns
     `canary_blocked` and does not send Continue.
   - `ready_to_expand` while the deployment is `running`: poll. A new
     deployment pauses after the Hub `succeeded` verdict; `rollout_expand`
     does not open the next batch while it is still running. A deployment
     created before that hold (`pause_after_canary` false) still lets the
     driver open the next batch.
   - `ready_to_expand` while `paused`: `deployment_continue_preview`, then
     `rollout_expand` with that digest and the revision from the status.
   - `expanding`: a later batch is open. Poll.
   - `finished`: included machines are `succeeded`.
   - `not_a_single_canary`: this deployment's first batch is more than one
     machine. Do not treat it as this rollout.

`succeeded` is the state Hub writes after the stored verification evidence.
An agent `finish_work` only enters `verifying`. The cross-failure-domain
verdict is reported as `canary_independent` and does not move the phase.
Stable promotion stays on the existing gate (`promotion_gate`).

The deployment page shows the same phase, the same reason, and the same
rollback lines. Continue, skip failed batch, and Abandon stay on that page.
Plain Continue refuses a failed batch. skip failed batch is a separate
button and `POST /v1/operator/deployments/{id}/skip-failed-batches`. It has
its own preview digest and records a reason. MCP tools do not send that skip.
`deployment_continue` returns `failed_batch_skip_refused` and does not write.

New operator creates and retries set `pause_after_canary`. The first included
machine is batch 1. An omitted `batch_size` means later batches are also 1.
An explicit `batch_size` of 2–5 sizes only the later batches. Existing
in-flight rows stay `pause_after_canary` 0, so their driver still opens the
next batch after a succeeded canary. Continue's refusal of a failed batch
applies to those paused deployments too.

The new-deployment form defaults the later batch size to 1. `rollout_preview`
and `rollout_apply` still reject any batch size other than 1 before HTTP.

## Rollback

`canary.rollback` is the list. In short:

- Nothing rolls the whole fleet back to the previous artifact.
- Abandon stops new jobs. A machine whose job already succeeded stays that way.
- `failed` is Hub closing a reversible job as returned to the previous version.
- `manual_intervention` has no rollback evidence.
- `rejected` means the machine did not apply the job.
- `lease_expired` means the agent stopped reporting. The machine contents are unknown.
- Stable promotion is a separate preview. A passed canary batch is not that promotion.

## Unreachable machine

Hub judges reachability from outbound check-ins. A missing check-in is not a
successful machine. Read `machine_get` and `machine_evidence`. Leave the
deployment paused.

The machine page's Connect BAT line shows the Tailscale address and a command
for the BAT client. Hub does not open that session. Where this build has the
Linux terminal action, it is on that machine page for the assigned user.
SSH you already have is the other break-glass path. None of those is a Hub
verdict. Do not call `rollout_expand` or `deployment_continue` to skip the
machine, and do not report the rollout succeeded.

## Secrets

Do not commit `hub.env`, enrollment tokens, or `CLAWCTL_HUB_URL` values that
are private to a tailnet if your policy treats them as secret. The example
files in `ops/docker/` stay examples.

## Embedded script catalog

`script_catalog_list` reads the view-scoped build-time allowlist.
`script_run_preview` validates hash, closed args, timeout, reason and 1–50 explicit
machine IDs. `script_run_apply` requires that preview's `preview_digest` and an
`idempotency_key`; it never calls preview itself. Read scripts require `operate`;
write scripts require `admin`. Only the harmless
Linux `fleet-probe-v1` is registered in phase 1. The agent runs without elevation
and refuses root execution. Output is capped/redacted job evidence; audit stores
only metadata/digests. Use `job_get` / `job_evidence` to inspect resulting jobs.
