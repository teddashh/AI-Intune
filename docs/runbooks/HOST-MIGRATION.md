# Host migration

Move host-a to host-b by copying, verifying, then cutting over. Each gate needs
recorded evidence and an operator decision. Never skip a gate. Record secret
file paths, modes, owners and sizes only. Keep dumps and state files private.

## 0. Inventory gate

List system and user services, timers, enabled units and `default.target.wants`
symlinks. Include containers, cron, listening ports, databases and sizes,
users and linger, certificates, DNS records and TTL, env and secret file paths.
List outbound mail workers and Telegram/Discord pollers. Only one poller for
one bot token may run anywhere. Two pollers cause 409 Conflict.

```bash
systemctl list-unit-files
systemctl list-timers --all
systemctl --user list-unit-files
systemctl --user list-timers --all
find ~/.config/systemd/user -type l -print
loginctl list-users
loginctl show-user ops -p Linger
ss -lntup
docker ps -a
crontab -l
sudo -u postgres psql -X -c 'SELECT datname, pg_database_size(oid) FROM pg_database;'
bash ops/fleet/migrate/scheduler-freeze.sh list --pattern '^(app|worker)-'
```

Store command output privately; process, cron and unit definitions can contain
credentials. Review all users' timers under their own login sessions.
Gate: inventory includes dependencies and every scheduler and bot location.

## 1. Prepare gate

Bootstrap host-b. Install dependencies and restore a rehearsal copy. Before
starting any restored service, block outbound traffic for every app and worker
UID with the site's firewall. Exempt only required local database access.
Test that mail, chat and other external sends fail. Record the firewall rules
and the exact command to remove them. Keep timers disabled and runtime masked.
Do not copy an active bot token into a running poller.
Gate: egress guard tested; no timers or pollers run on host-b.

## 2. Rehearsal gate

Use `pg_dump -Fc` for each database and `pg_dumpall --globals-only` for roles.
Transfer archives privately. Run `sha256sum` on every archive on both hosts;
use `sha256sum -c` on host-b. Restore into isolated databases with applications
off. Match extensions, collation and PostgreSQL version before comparing.

```bash
bash ops/fleet/migrate/pg-table-checksums.sh snapshot --db sample --out old.tsv
bash ops/fleet/migrate/pg-table-checksums.sh snapshot --db sample --out new.tsv
bash ops/fleet/migrate/pg-table-checksums.sh compare old.tsv new.tsv
printf '/\n/health\n' > routes.txt
bash ops/fleet/migrate/url-parity.sh snapshot --base https://hub.example.com --paths routes.txt --resolve hub.example.com:443:192.0.2.10 --out old-urls.tsv
bash ops/fleet/migrate/url-parity.sh compare old-urls.tsv new-urls.tsv
```

Run each snapshot on its respective host. Record N routes and require all N
statuses and redirect locations to match. Checksums sort `t::text` in C order
inside a repeatable-read transaction. Quiesce schema changes too. Exclusions
and `count-only` entries are incomplete evidence: increase `--max-rows` or
record a separately reviewed checksum before the final gate. The default is
1,000,000 rows. Large checksums can use substantial memory.
Gate: archive hashes, all table row counts and checksums, and URL parity match.

## 3. Window gate

Lower DNS TTL days before the move and wait out the old TTL. Choose a freeze
window away from scheduled work, for example :15 through :50 past the hour.
Confirm the remaining window can cover restore and verification. Record the
rollback owner and old DNS values. Gate: TTL elapsed and downtime approved.

## 4. Cutover checklist

- [ ] On host-a, freeze system and user timers. Use separate new state files.
- [ ] Stop running worker services too; verify none remain active or enabled.
- [ ] Serve a maintenance page with HTTP 503. Stop apps and block reconnections.
- [ ] Confirm zero app database sessions, including pools and background jobs. Run `sudo -u postgres psql -X -c "SELECT count(*) FROM pg_stat_activity WHERE datname = 'sample' AND pid <> pg_backend_pid();"` and require 0.
- [ ] Make final dumps, hash every archive, transfer, restore on host-b.
- [ ] Require identical hashes, table counts and checksums. STOP on any difference.
- [ ] Stop and disable bots and agents on host-a; confirm no other pollers exist.
- [ ] Remove host-b egress guard only after these gates pass.
- [ ] Enable new timers one at a time. Manually run each service once and inspect it.
- [ ] Start bots on host-b only after confirming they are stopped everywhere else.
- [ ] Make host-a nginx forward to host-b. For LE chains use `proxy_ssl_verify_depth 3` with certificate verification and the correct trusted CA file and SNI.
- [ ] Run URL parity from a third host that honours `curl --resolve`.
- [ ] Switch DNS. Run certbot on host-b, `certbot renew --dry-run`, and verify its deploy hook reloads nginx.

```bash
# Preview first; run --apply as root for system scope.
bash ops/fleet/migrate/scheduler-freeze.sh freeze --pattern '^(app|worker)-' --state /var/tmp/timers.json
sudo bash ops/fleet/migrate/scheduler-freeze.sh freeze --pattern '^(app|worker)-' --state /var/tmp/timers.json --apply
bash ops/fleet/migrate/scheduler-freeze.sh verify-off --pattern '^(app|worker)-'
# Run in the user login session; keep user state separate.
bash ops/fleet/migrate/scheduler-freeze.sh freeze --user --pattern '^(app|worker)-' --state "$HOME/timers.json" --apply
```

`enable --now` can trigger an immediate run if OnBootSec has elapsed. Avoid it.
Old timers must already be off so no catch-up storm can run twice. The thaw
helper restores enablement and runtime masking. It starts previously active
timers only with explicit `--now`. Persistent masks remain persistent.
A repeated freeze reuses the recorded set and preserves the original state.
Use a new state file for a new inventory or a later migration.

A transparent SNI egress proxy can send `curl --resolve` to the wrong host.
The script cannot reliably detect this. Use a third host with direct routing.
URL snapshots contain status and redirect destination, not content equality.
Avoid credential-bearing routes and redirects; never pass secrets in URLs.

## 5. Post gate

Require zero failed units, zero 5xx and successful first scheduled runs.
Watch host-a traffic for at least 15 minutes; only scanners may remain.
STOP host-a; do not terminate it. Retain its boot volume for at least 14 days.
Re-register the Hub machine and verifier for the new failure domain, then
retire the old Hub row. Record the machine identities privately.

## Rollback

| Phase | Action |
| --- | --- |
| Inventory or preparation | Leave host-a serving. Keep host-b guarded and off. |
| Rehearsal or window | Discard only the isolated rehearsal DB. Keep production unchanged. |
| Frozen, before DNS | Stop host-b apps and bots, restore egress guard, unmask/thaw old timers, start old apps, remove maintenance page. |
| After DNS | Freeze host-b first. Reconcile writes before restoring host-a. Switch DNS back and reverse the forwarder. Never run two writers or pollers. |
| After old host stop | Start retained host-a, validate data freshness, then follow the after-DNS rollback. |

```bash
sudo bash ops/fleet/migrate/scheduler-freeze.sh thaw --pattern '^(app|worker)-' --state /var/tmp/timers.json --apply --now
```

## Lessons

- Restored `*.wants` links re-enable units with missing binaries and cause crash loops. Diff enabled units against inventory after restore.
- Copy `~/.local/share` tool installs that links in `~/.local/bin` reference, including uv and claude installs.
- Check restored home directory ownership and user IDs.
- Preserve scheduler state, archive hashes and verification evidence until retention ends.
