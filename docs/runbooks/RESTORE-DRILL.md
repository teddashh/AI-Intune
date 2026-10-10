# Restore drill (Fly Hub, Litestream → R2)

Proves the R2 replica can rebuild the Hub database. Run it monthly and after any change to replication or secrets.
Production is never touched: no `fly ssh`, and no machine, volume, or secret changes.

## 1. Pick a throwaway location
- **Preferred: an off-host sandbox** on an operator box. The restored copy holds password hashes and TOTP secrets,
  so keep it on tmpfs and let it vanish. Example: `bwrap --unshare-all --share-net --ro-bind / / --tmpfs /tmp ...`,
  or a disposable container.
- In-place alternative: `fly ssh console -a <app> -C clawctl-restore-drill` restores into a temp dir on the live machine.
  It is quick, but it runs on production, so use the off-host drill when the rule is "don't touch prod".
- Do **not** start a second Fly machine from the Hub image with the normal entrypoint. It would run `litestream replicate`
  against the same bucket path and overwrite the real replica. If you need Fly, use a separate app with no IPs or services,
  and run only `litestream restore` as the machine command.

## 2. Credentials
Put `R2_ACCOUNT_ID` (or `R2_ENDPOINT`), `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY` and `R2_BUCKET` in a `0600` file on tmpfs
(e.g. `/dev/shm/r2.env`), and `shred -u` it afterwards. Never echo the values.
`R2_ACCESS_KEY_ID` is the S3 **Access Key ID**, which is the R2 API token's *id*, not its value. The secret is sha256(token value).
If you only have the token value, `GET /client/v4/accounts/<account>/tokens/verify` with it returns the id.

## 3. Restore and verify
Use the same Litestream minor version as production (0.5.x) and verify the release checksum. Then run:

```sh
set -a; . /dev/shm/r2.env; set +a
. ops/fly/r2-env.sh                     # derives LITESTREAM_ENDPOINT/BUCKET
export LITESTREAM_PATH=${LITESTREAM_PATH:-clawctl/litestream/clawctl.sqlite}
time litestream restore -config ops/fly/litestream.yml -o /tmp/drill/clawctl.sqlite /var/lib/clawctl/clawctl.sqlite
sqlite3 -readonly /tmp/drill/clawctl.sqlite 'PRAGMA integrity_check;'          # expect: ok
sqlite3 -readonly /tmp/drill/clawctl.sqlite "SELECT (SELECT count(*) FROM machine_registry),
  (SELECT count(*) FROM hub_accounts), (SELECT max(received_at) FROM machine_checkins);"
```
`sh ops/fly/restore-drill.sh --config ops/fly/litestream.yml` wraps the restore and integrity check
(it prints table and machine counts and cleans up).

## 4. Compare with live, read-only
Just before the restore, sign in to the Hub as an admin and read `GET /v1/operator/machines` (count `items`, max
`last_checkin_received_at`) and the users page (admin count). Pass when:
- `integrity_check` is `ok`;
- the machine and admin counts are equal to live;
- the restored latest check-in is no older than the live value minus the sync interval (default 10s), which is the RPO.

Record the `time` of the restore (RTO for data; a full rebuild adds deploy and boot time).

## 5. Clean up and record
Exit the sandbox (or `rm -rf` the drill dir), `shred -u` the env file, and confirm no `clawctl.sqlite*` copies or
`litestream` processes remain. Then write the date, versions, timings, counts and result into your ops log.

| Symptom | Cause |
|---|---|
| `SignatureDoesNotMatch` / `InvalidAccessKeyId` | Token value used as the access key id (see step 2) |
| `no matching backups` / empty restore | Wrong `LITESTREAM_PATH`, or replication never started (check `fly logs` for "replicating to") |
| counts lower than live | Replication stalled; check the Hub logs for Litestream errors before trusting backups |
