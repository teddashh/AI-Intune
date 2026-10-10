# Generic deploy pattern

Use `ops/fleet/deploy/deploy-template.sh` for code-only releases. Run previews
first. Apply commands run as root and take a nonblocking flock. Never share
one root/state directory between apps. Keep these directories root-owned and
not writable by the app account. Hooks are trusted operator shell code and
are sourced even in preflight. Keep them root-owned and reviewed. They must
not print secrets, dump environments, or put credentials in argv.

```bash
export DEPLOY_ROOT=/opt/app
export DEPLOY_STATE=/opt/app/state
export DEPLOY_WINDOW=15-55
bash ops/fleet/deploy/deploy-template.sh preflight --hooks /etc/app/deploy-hooks.sh
bash ops/fleet/deploy/deploy-template.sh build --sha abcdef0 --hooks /etc/app/deploy-hooks.sh
sudo bash ops/fleet/deploy/deploy-template.sh build --sha abcdef0 --hooks /etc/app/deploy-hooks.sh --apply
sudo bash ops/fleet/deploy/deploy-template.sh deploy --sha abcdef0 --hooks /etc/app/deploy-hooks.sh --apply
sudo bash ops/fleet/deploy/deploy-template.sh rollback --hooks /etc/app/deploy-hooks.sh --apply
bash ops/fleet/deploy/deploy-template.sh status
```

Pass environment paths explicitly through the root shell when sudo resets the
environment. The defaults above need no preservation. The window uses UTC
minutes, inclusive. Switch and rollback refuse outside it. Only rollback
accepts `--force-window`. `DEPLOY_TEST_REFUSE=1` can refuse an operation;
no test variable can allow an operation outside the window.

Implement these functions in the hook file:

| Hook | Contract |
| --- | --- |
| `hook_build SHA STAGE` | Build into STAGE only. Validate the code diff has no DB/schema changes, then write CODE_ONLY. Do not start production. |
| `hook_health SHA candidate` | Start a candidate on a side port with no worker/scheduler side effects; probe it; output exactly its reported SHA. Clean up candidate resources before returning. |
| `hook_health SHA live` | Probe production; output exactly its reported SHA. |
| `hook_smoke SHA candidate\|live` | Run bounded smoke checks; nonzero means failure. |
| `hook_restart SHA` | Restart only the app service. Never restart the database, proxy, or host. |
| `hook_edge SHA snapshot` | Save a private pre-switch URL snapshot through the reverse proxy. |
| `hook_edge SHA verify` | Check edge health and compare the same routes with the snapshot. Return nonzero on mismatch. |

Use the migration URL parity helper inside `hook_edge`. Save snapshots outside
sealed release directories. Ensure rollback edge checks tolerate the return
to the previous SHA and still require the same route behavior. Hooks must
bound their own timeouts and return nonzero on failures. Hook diagnostics
are suppressed; put safe diagnostics in a private operator log if needed.

Build stages under the deploy root, requires CODE_ONLY and rejects filenames
containing schema or migration. The operator must review the diff: filename
checks alone cannot prove code-only behavior. Releases become root-owned and
read-only, with a sha256 manifest. Build refuses reuse of a SHA. Releases are
never deleted. Every later switch validates the manifest and file set.

Before the first deploy, seed `current` to an already built, verified initial
release under `releases/`. Confirm its live health and edge checks manually.
Deploy validates previous and candidate releases, runs canary health and smoke,
saves edge parity, rechecks the time window, records previous, and atomically
replaces `current` using a temporary symlink and `mv -T`. It restarts only the
app and runs health, smoke and edge/parity gates. Any post-check failure
restores the recorded previous release and checks it again. Failed automatic
rollback is recorded as ROLLBACK FAILED and requires an operator.

Every state transition has a UTC entry in `state/steps.log`. State backups
preserve earlier previous values. A process crash during switching needs
operator recovery with the one-command rollback. Automatic rollback after a
failed switch is immediate even if the window has just ended. Never use this
pattern for schema changes; use a separately reviewed migration plan.
