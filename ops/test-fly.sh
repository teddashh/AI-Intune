#!/bin/bash
# Tests for Fly.io helper scripts
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE/.." # repo root

run_test() {
  local name="$1"
  shift
  echo "--- RUN   $name"
  # Run outside an if condition so errexit remains active inside test bodies.
  "$@"
  echo "--- PASS  $name"
}

test_notify_env() {
  local tmpdir
  tmpdir=$(mktemp -d)
  trap "rm -rf '$tmpdir'" RETURN

  # 0. asserts default dir is /run/clawctl-notify and entrypoint does not chmod that dir
  if ! grep -q 'CLAWCTL_NOTIFY_OUT_DIR:-/run/clawctl-notify' ops/fly/notify-env.sh; then
    echo "fail: notify-env.sh does not default to /run/clawctl-notify"
    return 1
  fi
  if grep -E 'chmod [^#]*/run/clawctl-notify' ops/fly/entrypoint.sh; then
    echo "fail: entrypoint.sh chmods /run/clawctl-notify"
    return 1
  fi

  # 1. writes only set keys, mode 0600
  env -i CLAWCTL_NOTIFY_OUT_DIR="$tmpdir/1" TELEGRAM_BOT_TOKEN="token1" TELEGRAM_CHAT_ID="chat1" /bin/sh ops/fly/notify-env.sh
  if [[ ! -f "$tmpdir/1/notify.env" ]]; then echo "fail: no file"; return 1; fi
  if ! grep -q "TELEGRAM_BOT_TOKEN=token1" "$tmpdir/1/notify.env"; then echo "fail: missing token"; return 1; fi
  if grep -q "CLAWCTL_NOTIFY_WEBHOOK_URL=" "$tmpdir/1/notify.env"; then echo "fail: extra key"; return 1; fi
  mode=$(stat -c "%a" "$tmpdir/1/notify.env")
  if [[ "$mode" != "600" ]]; then echo "fail: mode is $mode"; return 1; fi

  # 2. refuses newline
  if env -i CLAWCTL_NOTIFY_OUT_DIR="$tmpdir/2" TELEGRAM_BOT_TOKEN="tok
en" TELEGRAM_CHAT_ID="chat" /bin/sh ops/fly/notify-env.sh 2>/dev/null; then
    echo "fail: accepted newline"
    return 1
  fi

  # 3. refuses token without chat id
  if env -i CLAWCTL_NOTIFY_OUT_DIR="$tmpdir/3" TELEGRAM_BOT_TOKEN="token" /bin/sh ops/fly/notify-env.sh 2>/dev/null; then
    echo "fail: accepted token without chat id"
    return 1
  fi

  # 4. does nothing when none set
  env -i CLAWCTL_NOTIFY_OUT_DIR="$tmpdir/4" /bin/sh ops/fly/notify-env.sh
  if [[ -d "$tmpdir/4" ]]; then echo "fail: created dir when none set"; return 1; fi

  # 5. refuses when CLAWCTL_NOTIFY_ENV preset + vars
  if env -i CLAWCTL_NOTIFY_ENV="/some/path" CLAWCTL_NOTIFY_OUT_DIR="$tmpdir/5" TELEGRAM_BOT_TOKEN="token" TELEGRAM_CHAT_ID="chat" /bin/sh ops/fly/notify-env.sh 2>/dev/null; then
    echo "fail: accepted when CLAWCTL_NOTIFY_ENV is preset"
    return 1
  fi
}

test_r2_env() {
  # 1. defaults
  out=$(env -i /bin/sh -c '. ops/fly/r2-env.sh >/dev/null && echo "$LITESTREAM_SYNC_INTERVAL $LITESTREAM_SNAPSHOT_INTERVAL $LITESTREAM_RETENTION"')
  if [[ "$out" != "10s 24h 168h" ]]; then echo "fail: bad defaults: $out"; return 1; fi

  # 2. rejects bad durations
  if env -i LITESTREAM_SYNC_INTERVAL="5d" /bin/sh -c '. ops/fly/r2-env.sh' 2>/dev/null; then echo "fail: accepted 5d"; return 1; fi
  if env -i LITESTREAM_SYNC_INTERVAL="0s" /bin/sh -c '. ops/fly/r2-env.sh' 2>/dev/null; then echo "fail: accepted 0s"; return 1; fi
  if env -i LITESTREAM_SYNC_INTERVAL="1.5h" /bin/sh -c '. ops/fly/r2-env.sh' 2>/dev/null; then echo "fail: accepted 1.5h"; return 1; fi

  # 3. retention < snapshot
  if env -i LITESTREAM_SNAPSHOT_INTERVAL="48h" LITESTREAM_RETENTION="24h" /bin/sh -c '. ops/fly/r2-env.sh' 2>/dev/null; then
    echo "fail: accepted retention < snapshot"
    return 1
  fi

  # 4. derivation
  out=$(env -i R2_ACCOUNT_ID="12345678901234567890123456789012" /bin/sh -c '. ops/fly/r2-env.sh >/dev/null && echo "$R2_ENDPOINT"')
  if [[ "$out" != "https://12345678901234567890123456789012.r2.cloudflarestorage.com" ]]; then echo "fail: bad R2_ENDPOINT: $out"; return 1; fi
}

test_restore_drill() {
  local tmpdir
  tmpdir=$(mktemp -d)
  trap "rm -rf '$tmpdir'" RETURN

  mkdir -p "$tmpdir/bin"
  local log_args="$tmpdir/litestream_args"
  local mktemp_log="$tmpdir/mktemp_dirs"

  # stub mktemp to log created tempdirs
  cat << EOF > "$tmpdir/bin/mktemp"
#!/bin/sh
res=\$(/bin/mktemp "\$@")
echo "\$res" >> "$mktemp_log"
echo "\$res"
EOF
  chmod +x "$tmpdir/bin/mktemp"

  # stub litestream
  cat << EOF > "$tmpdir/bin/litestream"
#!/bin/sh
echo "\$*" > "$log_args"
if [ "\$1" != "restore" ]; then exit 1; fi
shift
while [ \$# -gt 0 ]; do
  if [ "\$1" = "-o" ]; then out="\$2"; shift 2; continue; fi
  shift
done
touch "\$out"
EOF
  chmod +x "$tmpdir/bin/litestream"

  # stub sqlite3
  cat << 'EOF' > "$tmpdir/bin/sqlite3"
#!/bin/sh
case "$2" in
  "PRAGMA integrity_check;")
    echo "${INTEGRITY_RESULT:-ok}"
    ;;
  "SELECT count(*) FROM sqlite_master WHERE type='table';")
    echo "4"
    ;;
  "SELECT 1 FROM sqlite_master WHERE type='table' AND name='machine_registry';")
    echo "1"
    ;;
  "SELECT count(*) FROM machine_registry;")
    echo "2"
    ;;
  *)
    echo "0"
    ;;
esac
EOF
  chmod +x "$tmpdir/bin/sqlite3"

  local test_env="PATH=$tmpdir/bin:$PATH R2_ACCESS_KEY_ID=test_key R2_SECRET_ACCESS_KEY=test_secret LITESTREAM_BUCKET=test_bkt LITESTREAM_ENDPOINT=https://test.r2.cloudflarestorage.com"

  # 1. success
  out=$(env -i $test_env /bin/sh ops/fly/restore-drill.sh)
  if [[ "$out" != *"restore-drill: ok (4 tables, 2 machine_registry rows)"* ]]; then
    echo "fail: expected ok, got: $out"
    return 1
  fi

  # 2. --keep prints kept path and leaves file
  out=$(env -i $test_env /bin/sh ops/fly/restore-drill.sh --keep)
  if [[ "$out" != *"restore-drill: kept restored file at "* ]]; then
    echo "fail: expected kept message, got: $out"
    return 1
  fi
  kept_file=$(echo "$out" | sed -n 's/restore-drill: kept restored file at //p')
  if [[ ! -f "$kept_file" ]]; then
    echo "fail: kept file $kept_file does not exist"
    return 1
  fi
  rm -rf "$(dirname "$kept_file")"

  # 3. integrity failure exits non-zero and cleans up
  INTEGRITY_RESULT="corrupt database"
  set +e
  err_out=$(env -i $test_env INTEGRITY_RESULT="$INTEGRITY_RESULT" /bin/sh ops/fly/restore-drill.sh 2>&1)
  rc=$?
  set -e
  if [[ "$rc" -eq 0 ]]; then
    echo "fail: accepted integrity failure"
    return 1
  fi
  last_work=$(tail -n 1 "$mktemp_log")
  if [[ -d "$last_work" ]]; then
    echo "fail: integrity failure left work dir $last_work"
    return 1
  fi

  # 4. missing sqlite3 fails
  mkdir -p "$tmpdir/bin-nosqlite"
  cp "$tmpdir/bin/litestream" "$tmpdir/bin-nosqlite/litestream"
  cp "$tmpdir/bin/mktemp" "$tmpdir/bin-nosqlite/mktemp"
  for dir in /bin /usr/bin; do
    if [ -d "$dir" ]; then
      for f in "$dir"/*; do
        b=$(basename "$f")
        if [ "$b" != "sqlite3" ] && [ ! -e "$tmpdir/bin-nosqlite/$b" ]; then
          ln -s "$f" "$tmpdir/bin-nosqlite/$b" 2>/dev/null || true
        fi
      done
    fi
  done
  set +e
  err_out=$(env -i PATH="$tmpdir/bin-nosqlite" R2_ACCESS_KEY_ID=test_key R2_SECRET_ACCESS_KEY=test_secret LITESTREAM_BUCKET=test_bkt LITESTREAM_ENDPOINT=https://test.r2.cloudflarestorage.com /bin/sh ops/fly/restore-drill.sh 2>&1)
  rc=$?
  set -e
  if [[ "$rc" -eq 0 || "$err_out" != *"sqlite3 not found on PATH"* ]]; then
    echo "fail: expected sqlite3 missing message, got rc=$rc: $err_out"
    return 1
  fi

  # 5. unknown arg exits 2
  set +e
  err_out=$(env -i $test_env /bin/sh ops/fly/restore-drill.sh --unknown-flag 2>&1)
  rc=$?
  set -e
  if [[ "$rc" -ne 2 ]]; then
    echo "fail: unknown arg exited with $rc instead of 2: $err_out"
    return 1
  fi

  # 6. --timestamp VALUE and --timestamp=VALUE passed through to litestream
  env -i $test_env /bin/sh ops/fly/restore-drill.sh --timestamp 2026-01-02T03:04:05Z
  recorded=$(cat "$log_args")
  if [[ "$recorded" != *"-timestamp 2026-01-02T03:04:05Z"* ]]; then
    echo "fail: litestream args missing timestamp: $recorded"
    return 1
  fi

  env -i $test_env /bin/sh ops/fly/restore-drill.sh --timestamp=2026-01-02T03:04:05Z
  recorded=$(cat "$log_args")
  if [[ "$recorded" != *"-timestamp 2026-01-02T03:04:05Z"* ]]; then
    echo "fail: litestream args missing timestamp=: $recorded"
    return 1
  fi

  # 7. missing R2 vars fail and error does not contain secret
  fake_secret="super-classified-secret-xyz987"
  set +e
  err_out=$(env -i PATH="$tmpdir/bin:$PATH" R2_SECRET_ACCESS_KEY="$fake_secret" /bin/sh ops/fly/restore-drill.sh 2>&1)
  rc=$?
  set -e
  if [[ "$rc" -eq 0 || "$err_out" != *"missing required environment variables"* ]]; then
    echo "fail: expected missing variables error, got rc=$rc: $err_out"
    return 1
  fi
  if [[ "$err_out" != *"R2_ACCESS_KEY_ID"* || "$err_out" != *"LITESTREAM_BUCKET"* ]]; then
    echo "fail: expected missing variable names, got: $err_out"
    return 1
  fi
  if [[ "$err_out" == *"$fake_secret"* ]]; then
    echo "fail: error leaked secret!"
    return 1
  fi
}

run_test "test_notify_env" test_notify_env
run_test "test_r2_env" test_r2_env
run_test "test_restore_drill" test_restore_drill

test_entrypoint() (
  set -euo pipefail
  local tmpdir
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT
  mkdir -p "$tmpdir/bin" "$tmpdir/data"
  # Rewrite only host-specific absolute paths in a disposable script copy.
  # No real Hub, Tailscale, privilege changes, tun device, or network access.
  python3 - "$tmpdir" <<'PY'
import sys
from pathlib import Path
root = Path(sys.argv[1])
s = Path('ops/fly/entrypoint.sh').read_text()
s = s.replace('/usr/local/lib/clawctl-fly/', str(Path('ops/fly').resolve()) + '/')
s = s.replace('/usr/local/bin/', str(root / 'bin') + '/')
s = s.replace('/var/run/tailscale', str(root / 'socket-dir'))
s = s.replace('/run/clawctl', str(root / 'run'))
s = s.replace('[ ! -c /dev/net/tun ]', '[ "${STUB_TUN:-0}" != 1 ]')
s = s.replace('[ -S "$socket" ]', '[ -f "$socket" ]')
s = s.replace('[ ! -S "$socket" ]', '[ ! -f "$socket" ]')
(root / 'entrypoint.sh').write_text(s)
PY
  cat > "$tmpdir/bin/setpriv" <<'SH'
#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
shift 4
exec "$@"
SH
  cat > "$tmpdir/bin/clawctl-hub" <<'SH'
#!/bin/sh
printf 'hub:%s:%s:%s:%s\n' "$CLAWCTL_AUTH_MODE" "$CLAWCTL_LISTEN" "${CLAWCTL_PUBLIC_URL:-}" "${CLAWCTL_TRUSTED_PROXIES:-}|${CLAWCTL_CLIENT_IP_HEADER:-}" >> "$STUB_LOG"
[ -z "${TS_AUTHKEY:-}${TELEGRAM_BOT_TOKEN:-}${TELEGRAM_CHAT_ID:-}" ] || exit 1
if [ -n "${CLAWCTL_NOTIFY_ENV:-}" ]; then
  [ "$(stat -c %a "$CLAWCTL_NOTIFY_ENV")" = 600 ] || exit 1
fi
SH
  cat > "$tmpdir/bin/litestream" <<'SH'
#!/bin/sh
printf 'litestream:%s\n' "$1" >> "$STUB_LOG"
[ -z "${TS_AUTHKEY:-}" ] || exit 1
if [ "$1" = replicate ]; then
  exec "$(dirname "$0")/clawctl-hub"
fi
SH
  cat > "$tmpdir/bin/tailscaled" <<'SH'
#!/bin/sh
printf 'tailscaled\n' >> "$STUB_LOG"
[ -z "${TS_AUTHKEY:-}" ] || exit 1
for arg do
  case $arg in --socket=*) touch "${arg#--socket=}" ;; esac
  case $arg in --statedir=*) touch "${arg#--statedir=}/tailscaled.state" ;; esac
 done
SH
  cat > "$tmpdir/bin/tailscale" <<'SH'
#!/bin/sh
printf 'tailscale\n' >> "$STUB_LOG"
[ -z "${TS_AUTHKEY:-}" ] || exit 1
case " $* " in
  *' ip -4 '*) echo 100.64.0.7 ;;
  *' up '*)
    for arg do
      case $arg in --auth-key=file:*) [ "$(stat -c %a "${arg#--auth-key=file:}")" = 600 ] || exit 1 ;; esac
    done ;;
esac
SH
  for command in chown chmod hub-data-init.sh; do
    printf '#!/bin/sh\nexit 0\n' > "$tmpdir/bin/$command"
  done
  # Keep real chmod for notify's mode checks; chown remains a stub.
  rm "$tmpdir/bin/chmod"
  chmod +x "$tmpdir/bin/"*
  boot() {
    : > "$tmpdir/calls"
    env -i PATH="$tmpdir/bin:$PATH" STUB_LOG="$tmpdir/calls" \
      CLAWCTL_DATA="$tmpdir/data" FLY_APP_NAME=stub-hub \
      CLAWCTL_NOTIFY_OUT_DIR="$tmpdir/notify" CLAWCTL_NOTIFY_CHOWN_TARGET='' \
      "$@" /bin/sh "$tmpdir/entrypoint.sh" > "$tmpdir/output" 2>&1
  }
  fail_boot() {
    local expected=$1; shift
    if boot "$@"; then echo "fail: entrypoint accepted invalid configuration"; return 1; fi
    grep -q "$expected" "$tmpdir/output"
    ! grep -q 'stub-private-secret' "$tmpdir/output"
  }
  boot
  grep -q '^hub:local:0.0.0.0:8787:https://stub-hub.fly.dev:172.16.0.0/12|Fly-Client-IP$' "$tmpdir/calls"
  ! grep -q tailscale "$tmpdir/calls"
  grep -q 'default CLAWCTL_PUBLIC_URL=https://stub-hub.fly.dev' "$tmpdir/output"
  grep -q -- '--reuid=65532 --regid=65532 --clear-groups --inh-caps=-all' "$tmpdir/calls"
  boot CLAWCTL_AUTH_MODE=local CLAWCTL_PUBLIC_URL=https://hub.example.com CLAWCTL_TRUSTED_PROXIES=192.0.2.1 CLAWCTL_CLIENT_IP_HEADER=X-Forwarded-For
  grep -q 'https://hub.example.com:192.0.2.1|X-Forwarded-For$' "$tmpdir/calls"
  boot CLAWCTL_TRUSTED_PROXIES=''
  grep -q ':|Fly-Client-IP$' "$tmpdir/calls"
  fail_boot 'https://' CLAWCTL_PUBLIC_URL=http://hub.example.com
  fail_boot 'contains whitespace' CLAWCTL_PUBLIC_URL='https://stub-private-secret .example.com'
  fail_boot 'https://' CLAWCTL_PUBLIC_URL=''
  fail_boot 'FLY_APP_NAME must' FLY_APP_NAME='stub-private-secret@invalid'
  fail_boot 'https://' FLY_APP_NAME=''
  fail_boot 'unsupported on Fly' CLAWCTL_AUTH_MODE=both
  fail_boot 'must be tailscale, local, or both' CLAWCTL_AUTH_MODE=invalid
  boot CLAWCTL_AUTH_MODE=local TS_AUTHKEY=stub-private-secret
  grep -q 'TS_AUTHKEY is ignored' "$tmpdir/output"
  ! grep -q tailscale "$tmpdir/calls"
  ! grep -q 'stub-private-secret' "$tmpdir/output"
  fail_boot 'must be tailscale, local, or both' CLAWCTL_AUTH_MODE=''
  fail_boot 'TS_AUTHKEY is required' TS_AUTHKEY=''
  fail_boot 'TS_AUTHKEY is required' CLAWCTL_AUTH_MODE=tailscale
  fail_boot 'contains whitespace' CLAWCTL_AUTH_MODE=tailscale TS_AUTHKEY='stub-private-secret key'
  fail_boot '/dev/net/tun is missing' CLAWCTL_AUTH_MODE=tailscale TS_AUTHKEY=stub-private-secret
  boot TS_AUTHKEY=stub-private-secret STUB_TUN=1
  grep -q 'defaulting to tailscale' "$tmpdir/output"
  grep -q '^hub:tailscale:100.64.0.7:8787:' "$tmpdir/calls"
  boot CLAWCTL_AUTH_MODE=tailscale TS_AUTHKEY=stub-private-secret STUB_TUN=1
  grep -q '^tailscaled$' "$tmpdir/calls"
  grep -q '^tailscale$' "$tmpdir/calls"
  grep -q '^hub:tailscale:100.64.0.7:8787:' "$tmpdir/calls"
  [ ! -e "$tmpdir/run/ts-authkey" ]
  ! grep -q 'stub-private-secret' "$tmpdir/output"
  printf 'persisted' > "$tmpdir/data/tailscale/tailscaled.state"
  boot STUB_TUN=1
  grep -q 'defaulting to tailscale' "$tmpdir/output"
  grep -q '^hub:tailscale:100.64.0.7:8787:' "$tmpdir/calls"
  boot CLAWCTL_AUTH_MODE=local TS_AUTHKEY=stub-private-secret
  grep -q '^hub:local:' "$tmpdir/calls"
  ! grep -q tailscale "$tmpdir/calls"
  ! grep -q 'stub-private-secret' "$tmpdir/output"
  boot CLAWCTL_AUTH_MODE=tailscale STUB_TUN=1
  grep -q '^hub:tailscale:100.64.0.7:8787:' "$tmpdir/calls"
  fail_boot 'TS_HOSTNAME must' CLAWCTL_AUTH_MODE=tailscale STUB_TUN=1 TS_HOSTNAME='bad/name'
  fail_boot 'TS_TAGS must' CLAWCTL_AUTH_MODE=tailscale STUB_TUN=1 TS_TAGS=untagged
  : > "$tmpdir/data/tailscale/tailscaled.state"
  boot
  grep -q '^hub:local:' "$tmpdir/calls"
  ! grep -q tailscale "$tmpdir/calls"
  rm -rf "$tmpdir/data/tailscale"
  boot
  [ ! -d "$tmpdir/data/tailscale" ]
  fail_boot 'CLAWCTL_PORT must' CLAWCTL_PORT=65536
  fail_boot 'contains whitespace' R2_SECRET_ACCESS_KEY='stub-private-secret key'
  fail_boot 'replication is incomplete' R2_SECRET_ACCESS_KEY=stub-private-secret
  fail_boot 'replication is incomplete' LITESTREAM_REQUIRED=1
  fail_boot 'must be a duration' LITESTREAM_SYNC_INTERVAL=5d
  boot R2_ACCOUNT_ID=12345678901234567890123456789012 R2_ACCESS_KEY_ID=stub-key R2_SECRET_ACCESS_KEY=stub-private-secret R2_BUCKET=stub-bucket TELEGRAM_BOT_TOKEN=stub-private-secret TELEGRAM_CHAT_ID=stub-chat
  grep -q '^litestream:restore$' "$tmpdir/calls"
  grep -q '^litestream:replicate$' "$tmpdir/calls"
  grep -q '^hub:local:0.0.0.0:8787:' "$tmpdir/calls"
  ! grep -q tailscale "$tmpdir/calls"
  ! grep -q 'stub-private-secret' "$tmpdir/output"
  touch "$tmpdir/data/clawctl.sqlite"
  boot R2_ENDPOINT=https://stub.example.com R2_ACCESS_KEY_ID=stub-key R2_SECRET_ACCESS_KEY=stub-private-secret R2_BUCKET=stub-bucket
  ! grep -q '^litestream:restore$' "$tmpdir/calls"
  grep -q '^litestream:replicate$' "$tmpdir/calls"
)

test_fly_configs() {
  python3 - <<'PY'
import tomllib
from pathlib import Path
public = tomllib.loads(Path('ops/fly/fly.toml').read_text())
private = tomllib.loads(Path('ops/fly/fly.tailscale.toml').read_text())
assert public['env']['CLAWCTL_AUTH_MODE'] == 'local'
assert public['env']['CLAWCTL_PORT'] == '8787'
assert 'CLAWCTL_OPERATOR_CAPABILITY_PREFIX' not in public['env']
assert private['env']['CLAWCTL_AUTH_MODE'] == 'tailscale'
assert 'http_service' not in private and 'services' not in private
service = public['http_service']
assert service['internal_port'] == 8787 and service['force_https'] is True
assert service['auto_stop_machines'] == 'off' and service['auto_start_machines'] is True
assert service['min_machines_running'] == 1
assert service['checks'] == [dict(method='GET', path='/healthz', grace_period='30s', interval='30s', timeout='5s')]
assert service['concurrency']['type'] == 'connections'
assert 0 < service['concurrency']['soft_limit'] < service['concurrency']['hard_limit']
for key in ('mounts', 'restart', 'vm', 'build'):
    assert public[key] == private[key]
assert public['vm'][0]['memory'] == '1024mb'
PY
}

run_test "test_entrypoint" test_entrypoint
run_test "test_fly_configs" test_fly_configs
