---
name: clawctl-operator
description: Operate an AI-Intune / clawctl Hub from an agent. Use when deploying Hub, enrolling machines one by one, reading fleet evidence, or running a one-machine canary rollout. The contract is docs/OPERATOR-AGENT.md.
---

# clawctl operator

The tool contract, Tailscale identity, phase names, and rollback sentences are in [docs/OPERATOR-AGENT.md](../../docs/OPERATOR-AGENT.md). Hub install is in [docs/DEPLOY-OSS.md](../../docs/DEPLOY-OSS.md). Follow those files. This file is only the order of work.

Run `clawctl-operator` on a tailnet node that holds the Hub grants. Set `--hub-url` or `CLAWCTL_HUB_URL` to `http://<hub-tailscale-ipv4>:<port>`. Stdio MCP is `clawctl-operator mcp`. One JSON call is `clawctl-operator call <tool> '<json>'`.

## Deploy Hub

Follow DEPLOY-OSS. Bind Hub to its Tailscale IP. Do not put `hub.env`, tokens, or a live database in git. After Hub answers `/healthz` and the operator homepage from that node, point `CLAWCTL_HUB_URL` at the same origin.

## Enroll one machine

Repeat this per machine. Do not batch enrollments into one ticket.

1. `enroll_ticket_preview` with `display_name` and `ttl_seconds`.
2. `enroll_ticket_create` with that `preview_digest`, a reason, and an `idempotency_key` you reuse if the call retries. The fresh response carries the one-time token. A replay does not.
3. On the target, run the install command from DEPLOY-OSS (`--hub` is the Tailscale origin). Pass the token on the installer's prompt or `--token-file` mode `0600`. Do not put the token in the command that gets logged.
4. `machine_get` for the new id. A check-in timestamp there is enrollment evidence. The installer's own success line is not.

## Read the fleet

`fleet_overview` first. If `page_truncated` is true, keep calling `machines_list` with `cursor`. Then, as needed: `machine_get`, `machine_evidence`, `jobs_list`, `job_get`, `job_evidence`, `deployments_list`, `deployment_get`, `software_report`, `compliance`.

Act on Hub fields. An agent finish line, an SSH exit code, or a BAT session is not a fleet verdict.

## Canary rollout

1. `rollout_preview` for `channel`, `version`, and `artifact_sha256`. Leave `batch_size` unset or set it to 1.
2. Stop when `canary.singular_canary` is false or `preview.create_allowed` is false. The preview names the one canary machine. If that is the wrong machine, stop and say so. Do not apply a different digest.
3. `rollout_apply` with the preview's `preview_digest`, matching `confirm_channel` and `confirm_version`, a reason, and an idempotency key.
4. Poll `rollout_status`.
   - `canary_pending`: wait. Do not expand.
   - `canary_stopped` or `expand_stopped`: stop. Do not call `rollout_expand` or `deployment_continue`.
   - `ready_to_expand` and `deployment_state` `running`: poll until the Hub pauses. A new deployment does not open the next batch by itself. Do not create a second deployment. A deployment created before the hold still lets the driver open the next batch; `rollout_expand` still writes nothing while it is running.
   - `ready_to_expand` and `paused`: `deployment_continue_preview`, then `rollout_expand` with that digest and the `control_revision` and `opened_batch` from the status you just read.
   - `expanding`: poll until `finished`, `canary_stopped`, or `expand_stopped`.
   - `finished`: canary-channel batches for this deployment are done. Stable promotion is a separate `deployment_create_preview` on channel `stable`. Apply it only when that preview says `create_allowed`.
5. Show `canary.rollback` when you report the outcome. Do not add a rollback the list does not state.

The same phase is on the deployment page under Canary rollout. The new-deployment form defaults the later batch size to 1, and the first batch is always one machine. Hub enforces that for new deployments even when an API caller omits `batch_size`.

Plain Continue on a failed batch is refused. The deployment page has a separate button labelled `skip failed batch`. It needs its own preview and a reason. MCP has no skip tool. Do not use `deployment_continue` to pass a failed batch.

## Unreachable machine

Read `machine_get` and `machine_evidence`. Leave the deployment paused. Open the machine page: Connect BAT shows the address and a command; Hub does not connect for you. Use SSH you already have if you must get on the host. Where this build has the Linux terminal action, it is on that page for the assigned user.

Do not report success from SSH, BAT, the installer, or a missing check-in. Do not skip the machine with `deployment_continue`. That tool refuses a failed batch and does not write.

## Disk-clean

This is not an artifact deployment. Do not use `deployment_create` or `rollout_apply` for it.

1. `disk_clean_profile_preview`, then `disk_clean_profile_publish` with that digest and `current_revision` as `expected_revision`.
2. `disk_clean_dry_run_preview`, then `disk_clean_dry_run_apply`. Read `disk_clean_summary` before any apply that deletes.
3. `disk_clean_canary_preview` names exactly one machine from the target list. `disk_clean_canary_apply` opens only that machine.
4. After the canary pauses on success, `disk_clean_continue_preview`, then `disk_clean_continue_apply` with that digest and the preview's control revision and opened batch. That is the only step that opens the rest.
5. `disk_clean_abandon_preview` then `disk_clean_abandon_apply` opens nothing more. Do not abandon while a job is still running.

The profile is a closed document. Do not add a shell, a command, or a path outside its keys. Design: [docs/MAINTENANCE-DISK-CLEAN.md](../../docs/MAINTENANCE-DISK-CLEAN.md).

## Writes

Every write tool needs `preview_digest` from the matching preview tool in an earlier call. Continue, abandon, `rollout_expand`, and the disk-clean continue and abandon tools also need `expected_control_revision` and `expected_opened_batch`. If the tool returns `preview_digest_required`, `expected_revision_required`, `expected_revision_mismatch`, or `canary_blocked`, the write was not sent. Fix the arguments or stop. Do not invent a digest.
