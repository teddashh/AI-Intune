#!/usr/bin/env bash
set -euo pipefail
trap 'printf "not ok - area assertion failed\n" >&2' ERR
ROOT=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
mkdir "$TMP/bin"
export PATH="$TMP/bin:$PATH" LOG="$TMP/log"
cat > "$TMP/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
[[ ${FAIL_SYSTEMCTL:-0} == 0 ]] || exit 1
printf '%s\n' "$*" >> "$LOG"
case "$*" in
*list-unit-files*) echo 'job.timer enabled';;
*is-enabled*) echo enabled;;
*is-active*--quiet*) exit 1;;
*is-active*) echo active;;
*show*) echo job.service;;
esac
STUB
cat > "$TMP/bin/psql" <<'STUB'
#!/usr/bin/env bash
if [[ $* == *pg_tables* ]]; then echo 'public.items'; else printf 'public.items\t2\tabcd\n'; fi
STUB
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
printf 'fake-secret-value\n' >&2
printf '200\t'
STUB
chmod +x "$TMP/bin/"*
passed=0
check() { if "$@"; then passed=$((passed+1)); printf 'ok - %s\n' "$*"; else printf 'not ok - %s\n' "$*"; exit 1; fi; }
check bash "$ROOT/scheduler-freeze.sh" freeze --pattern job --state "$TMP/state" --user
check test ! -e "$TMP/state"
check bash "$ROOT/scheduler-freeze.sh" freeze --pattern job --state "$TMP/state" --user --apply
check grep -q 'mask --runtime job.timer' "$LOG"
cp "$TMP/state" "$TMP/original-state"
check bash "$ROOT/scheduler-freeze.sh" freeze --pattern job --state "$TMP/state" --user --apply
check cmp "$TMP/state" "$TMP/original-state"
: > "$LOG"
check bash "$ROOT/scheduler-freeze.sh" thaw --pattern job --state "$TMP/state" --user --apply
check grep -q 'enable job.timer' "$LOG"
check test "$(grep -c 'start job.timer' "$LOG" || true)" = 0
if bash "$ROOT/scheduler-freeze.sh" verify-off --pattern job --user; then echo 'not ok - verify-off'; exit 1; fi
printf 'ok - verify-off rejects active timer\n'; passed=$((passed+1))
check bash "$ROOT/pg-table-checksums.sh" snapshot --db sample --psql "$TMP/bin/psql" --out "$TMP/a"
check bash "$ROOT/pg-table-checksums.sh" compare "$TMP/a" "$TMP/a"
printf 'public.items\t3\tabcd\n' > "$TMP/b"
if bash "$ROOT/pg-table-checksums.sh" compare "$TMP/a" "$TMP/b"; then exit 1; fi
printf 'ok - table differences rejected\n'; passed=$((passed+1))
printf '/health\n' > "$TMP/paths"
check bash "$ROOT/url-parity.sh" snapshot --base https://hub.example.com --paths "$TMP/paths" --out "$TMP/urls"
check bash "$ROOT/url-parity.sh" compare "$TMP/urls" "$TMP/urls"
bash "$ROOT/url-parity.sh" snapshot --base https://hub.example.com --paths "$TMP/paths" > "$TMP/output" 2>&1
check test "$(grep -c fake-secret-value "$TMP/output" || true)" = 0
printf '/health\t503\t\n' > "$TMP/other"
if bash "$ROOT/url-parity.sh" compare "$TMP/urls" "$TMP/other"; then exit 1; fi
printf 'ok - route differences rejected\n'; passed=$((passed+1))
check bash "$ROOT/url-parity.sh" compare "$TMP/urls" "$TMP/other" --only-prefix /other
printf '# comment\n/health\n/down\n' > "$TMP/paths2"
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do last=$a; done
[[ $last != */down ]] || exit 7
printf '200\t'
STUB
chmod +x "$TMP/bin/curl"
check bash "$ROOT/url-parity.sh" snapshot --base http://hub.example.com --paths "$TMP/paths2" --out "$TMP/urls2"
check test "$(wc -l < "$TMP/urls2")" = 2
check grep -q $'^/down\t000' "$TMP/urls2"
cat > "$TMP/bin/psql-log" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$LOG.psql"
if [[ $* == *pg_tables* ]]; then echo 'public.items'; else printf 'public.items\t2\tabcd\n'; fi
STUB
chmod +x "$TMP/bin/psql-log"
check bash "$ROOT/pg-table-checksums.sh" snapshot --db sample --psql "env psql-log" --out "$TMP/c"
check grep -q 'REPEATABLE READ READ ONLY' "$LOG.psql"
check cmp "$TMP/a" "$TMP/c"
cat > "$TMP/bin/id" <<'STUB'
#!/usr/bin/env bash
echo 1000
STUB
chmod +x "$TMP/bin/id"
bash "$ROOT/scheduler-freeze.sh" freeze --pattern job --state "$TMP/root-state" --apply > "$TMP/hint" || true
check python3 - "$TMP/hint" <<'PY'
import sys
lines=open(sys.argv[1]).read().splitlines()
assert len(lines)==1 and lines[0].startswith("sudo bash -c '") and lines[0].endswith("'")
PY
if FAIL_SYSTEMCTL=1 bash "$ROOT/scheduler-freeze.sh" verify-off --pattern job --user; then exit 1; fi
printf 'ok - failed timer inventory refuses verification\n'; passed=$((passed+1))
printf 'passed: %s, failed: 0\n' "$passed"
