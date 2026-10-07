#!/bin/bash
# Tests for Fly.io helper scripts
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE/.." # repo root

run_test() {
  local name="$1"
  shift
  echo "--- RUN   $name"
  if "$@"; then
    echo "--- PASS  $name"
  else
    echo "--- FAIL  $name"
    exit 1
  fi
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
