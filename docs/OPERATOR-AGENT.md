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
abandon, and profile assignment use `admin`. Missing a grant is an error from
Hub. The tool does not retry with a different identity.

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

## Tools

Reads: `fleet_overview`, `machines_list`, `machine_get`, `machine_evidence`,
`jobs_list`, `job_get`, `job_evidence`, `deployments_list`, `deployment_get`,
`software_report`, `compliance`, `rollout_status`.

Previews (they return `preview_digest`): `enroll_ticket_preview`,
`deployment_create_preview`, `deployment_continue_preview`,
`deployment_abandon_preview`, `profile_assignment_preview`, `rollout_preview`.

Writes: `enroll_ticket_create`, `deployment_create`, `deployment_continue`,
`deployment_abandon`, `profile_assignment_apply`, `rollout_apply`,
`rollout_expand`.

A write requires `preview_digest` copied from a preview response
(`sha256:` and 64 lowercase hex). The write call does not create a preview
for you. Deployment continue, abandon, and `rollout_expand` also require
`expected_control_revision` and `expected_opened_batch` from the deployment
you just read, plus an `idempotency_key` you choose and reuse for a retry.

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
   - `ready_to_expand` while the deployment is `running`: poll. The existing
     deployment driver opens the next batch after that Hub verdict. The tool
     writes nothing.
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
rollback lines. Continue and Abandon stay on that page. Continue can still
skip a failed batch when you submit it there. `rollout_expand` will not.

The new-deployment form defaults batch size to 1. The server default for an
API caller that omits `batch_size` is still 5. The canary tools send 1.

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
