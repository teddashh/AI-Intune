# Move agents to a new Hub

This guide explains how to migrate existing enrolled agents to a new Hub.

## Why move agents?

A new Hub is a new Tailscale node with a new literal IP address. Agents hold a single `hub_url` and cannot automatically discover a new Hub if the IP address changes. To move a machine, you must explicitly re-enroll its agent against the new Hub.

## Prerequisites

Before moving agents, ensure that:
1. The new Hub is healthy and fully deployed.
2. The necessary Tailscale ACL grants exist allowing agents to reach the new Hub's port.

> [!NOTE]
> On a fresh new Hub the machine gets a new machine ID; profiles, settings and compliance policies must be republished/assigned on the new Hub first.

## Moving a machine

The installer refuses to silently switch Hubs: without `--reenroll`, a config that points at a different Hub makes it stop with a hint.

For each machine you want to move, follow these steps:

1. Create a new enrollment ticket on the new Hub at `http://NEW:PORT/machines/enrollment` from your own device (not from a tagged Hub host, see OPERATOR-AUTH.md).
2. Download the per-architecture bootstrap archive from the new Hub's enrollment page (`clawctl-agent-bootstrap-linux-<arch>.tar.gz`), extract it, and run its `install-agent.sh`. It installs the agent binary that matches the new Hub's version.
3. Save the enrollment ticket to a file (e.g. `./token`) and ensure it has secure permissions (`chmod 0600 ./token`).
4. Run the installer with the `--reenroll` flag pointing to the new Hub:
   ```bash
   ./install-agent.sh --hub http://NEW:PORT --reenroll --token-file ./token
   ```
5. Check the machine page on the new Hub's UI. A recent check-in timestamp serves as proof of a successful move.
6. Retire the machine on the old Hub only AFTER the new Hub shows a check-in.

## Rollback steps

If the move fails or you need to revert, use the automatic backup created during re-enrollment:
1. Locate the backup file in `~/.config/clawctl/` (named `agent.json.pre-reenroll-<UTC timestamp>`).
2. Restore the backup over `agent.json`:
   ```bash
   cp ~/.config/clawctl/agent.json.pre-reenroll-<timestamp> ~/.config/clawctl/agent.json
   ```
3. Rerun the OLD Hub's bootstrap installer without `--reenroll`:
   ```bash
   ./install-agent.sh --hub http://OLD:PORT
   ```
   This ensures the binary matches the old Hub's version again (same `hub_url` → it skips enrollment), which also restarts the service.
4. Retire the machine on the new Hub, since it now holds an enrollment you are abandoning.

> [!IMPORTANT]
> The backup file still contains the old machine credential: keep it `0600` and delete it once the old Hub is retired.

## Canary order advice

Do not move all machines at once. Follow a canary strategy:
1. Move the least critical machine first.
2. Let it soak for 24 hours.
3. Move the remaining machines at a rate of one per day.
4. Move the old Hub's own host machine last.

## macOS and Windows agents

The `--reenroll` flag is currently supported on Linux only. For macOS and Windows agents, the process is manual for now:
1. Move the existing `agent.json` file aside manually.
2. Rerun the standard installer.

## Retiring the old Hub

Once all machines have been successfully moved and verified on the new Hub:
1. Take a final backup of the old Hub's database.
2. Stop the old Hub service.
3. Remove the old Hub's Tailscale grants and monitoring configurations.
