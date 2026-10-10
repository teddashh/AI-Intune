---
name: fleet-host-migration
description: Migrate a host with copy, verification and cutover gates, or adopt a code-only deployment with canary checks and rollback.
---

# Fleet host migration and deploy

Read [HOST-MIGRATION](../../docs/runbooks/HOST-MIGRATION.md) for moves and
[DEPLOY-PATTERN](../../docs/runbooks/DEPLOY-PATTERN.md) for code releases.
Any agent can follow this sequence. Inventory and preview first. Record secret
paths only. Require operator evidence at every gate. Test the new host egress
guard before restoring workers. Never start a second poller for one bot token.
Require archive hashes, counts, checksums and route parity before cutover.
Use an independent parity host that honours curl resolution.

Use scheduler-freeze for system and user timers with separate state files.
Apply only within the authorized maintenance window. Keep old host rollback
available and retain its volume. For deployment, review trusted hooks and the
code-only diff, build sealed releases, then canary and switch. Verify the
recorded previous release and automatic rollback evidence. Never merge a PR
without a human request. Never print secrets or real fleet identifiers.
