#!/usr/bin/env bash
# Disk-clean hardening tests. They use a private temp directory, never root,
# and never the real npm/pip/go/docker/apt/journalctl/snap cleaners.
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="$ROOT/ops/maintenance/disk-clean"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

passed=0
failed=0
ok() { passed=$((passed + 1)); printf 'ok  %s\n' "$1"; }
bad() { failed=$((failed + 1)); printf 'FAIL %s\n' "$1" >&2; }

install_stubs() {
  local home="$1"
  local bin="$home/.local/bin"
  local name
  mkdir -p "$bin"
  for name in npm pip pip3 uv go docker apt-get dnf journalctl snap systemd-tmpfiles; do
    cat >"$bin/$name" <<'EOF'
#!/bin/sh
printf 'stub invoked: %s\n' "$0" >>"${DISK_CLEAN_STUB_LOG:?}"
exit 99
EOF
    chmod 755 "$bin/$name"
  done
}

age() { touch -d '10 days ago' "$1"; }

# $1 name  $2 PROTECT_NAMES line or empty to omit the key
run_user_tmp() {
  local name="$1"
  local protect_line="$2"
  local home="$WORK/$name"
  local tmp="$home/scratch"
  local state="$home/state"
  local conf="$home/c.conf"
  local log="$home/stubs.log"
  mkdir -p "$tmp" "$state"
  install_stubs "$home"
  : >"$log"
  age_file() { printf x >"$1"; age "$1"; }
  age_file "$tmp/safe-old.txt"
  age_file "$tmp/openclaw-state"
  age_file "$tmp/my-backup"
  age_file "$tmp/agent.sock"
  age_file "$tmp/run.pid"
  age_file "$tmp/run.lock"
  age_file "$tmp/systemd-private-xyz"
  age_file "$tmp/odoo-cache"
  age_file "$tmp/postgres-x"
  age_file "$tmp/.s.PGSQL.5432"
  printf x >"$tmp/recent.txt"
  cat >"$conf" <<EOF
DRY_RUN=0
CATEGORIES="user_tmp"
TMP_AGE_DAYS=1
MOUNT=/
TMP_DIRS="$tmp"
$protect_line
EOF
  local out rc
  out=$(DISK_CLEAN_STUB_LOG="$log" DISK_CLEAN_STATE="$state" HOME="$home" bash "$SCRIPT" --scope user --conf "$conf" 2>&1) && rc=0 || rc=$?
  if [ "$rc" -ne 0 ]; then bad "$name exit $rc"; printf '%s\n' "$out" >&2; return; fi
  if [ -s "$log" ]; then bad "$name invoked a cleaner stub"; cat "$log" >&2; return; fi
  if [ -e "$tmp/safe-old.txt" ]; then bad "$name left an eligible file"; return; fi
  local survivor
  for survivor in openclaw-state my-backup agent.sock run.pid run.lock systemd-private-xyz odoo-cache postgres-x .s.PGSQL.5432 recent.txt; do
    if [ ! -e "$tmp/$survivor" ]; then bad "$name deleted $survivor"; return; fi
  done
  local digest
  digest=$(sha256sum "$conf" | awk '{print $1}')
  case "$out" in
    *"\"config_digest\":\"sha256:$digest\""*) ;;
    *) bad "$name digest mismatch"; printf '%s\n' "$out" >&2; return ;;
  esac
  ok "$name"
}

run_user_tmp "omit" ""
run_user_tmp "shrink" 'PROTECT_NAMES="zzz-only"'

# A protected glob is refused. An ordinary old directory is removed.
glob_home="$WORK/globs"
glob_tmp="$glob_home/scratch"
glob_state="$glob_home/state"
glob_conf="$glob_home/c.conf"
glob_log="$glob_home/stubs.log"
mkdir -p "$glob_tmp/node-old" "$glob_tmp/node-clawctl-data" "$glob_tmp/cache-backup-1" "$glob_state"
install_stubs "$glob_home"
: >"$glob_log"
age "$glob_tmp/node-old"
age "$glob_tmp/cache-backup-1"
age "$glob_tmp/node-clawctl-data"
cat >"$glob_conf" <<EOF
DRY_RUN=0
CATEGORIES="tmp_globs"
TMP_AGE_DAYS=1
MOUNT=/
TMP_DIRS="$glob_tmp"
TMP_GLOB_RULE=$glob_tmp/node-*|1|0|
TMP_GLOB_RULE=$glob_tmp/*backup*|1|0|
EOF
glob_out=$(DISK_CLEAN_STUB_LOG="$glob_log" DISK_CLEAN_STATE="$glob_state" HOME="$glob_home" bash "$SCRIPT" --scope user --conf "$glob_conf" 2>&1) && glob_rc=0 || glob_rc=$?
if [ "$glob_rc" -ne 0 ]; then
  bad "protected glob exit $glob_rc"
  printf '%s\n' "$glob_out" >&2
elif [ -s "$glob_log" ]; then
  bad "protected glob invoked a cleaner stub"
elif [ -e "$glob_tmp/node-old" ]; then
  bad "eligible glob directory survived"
elif [ ! -e "$glob_tmp/cache-backup-1" ]; then
  bad "protected glob directory was deleted"
elif [ ! -e "$glob_tmp/node-clawctl-data" ]; then
  bad "an allowed glob deleted a protected entry (node-clawctl-data)"
elif ! printf '%s' "$glob_out" | grep -q 'refused:'; then
  bad "protected glob was not marked refused"
else
  ok "protected glob refused"
fi

# --dry-run does not delete, even when the conf says DRY_RUN=0.
dry_home="$WORK/dry"
dry_tmp="$dry_home/scratch"
dry_state="$dry_home/state"
dry_conf="$dry_home/c.conf"
mkdir -p "$dry_tmp" "$dry_state"
install_stubs "$dry_home"
printf x >"$dry_tmp/safe-old.txt"
age "$dry_tmp/safe-old.txt"
cat >"$dry_conf" <<EOF
DRY_RUN=0
CATEGORIES="user_tmp"
TMP_AGE_DAYS=1
MOUNT=/
TMP_DIRS="$dry_tmp"
EOF
dry_out=$(DISK_CLEAN_STATE="$dry_state" HOME="$dry_home" bash "$SCRIPT" --scope user --conf "$dry_conf" --dry-run 2>&1) && dry_rc=0 || dry_rc=$?
if [ "$dry_rc" -ne 0 ]; then
  bad "dry-run exit $dry_rc"
elif [ ! -e "$dry_tmp/safe-old.txt" ]; then
  bad "dry-run deleted a file"
elif ! printf '%s' "$dry_out" | grep -q '"mode":"dry-run"'; then
  bad "dry-run summary mode"
else
  ok "dry-run flag wins"
fi

# A protect token that could become a find option is refused.
bad_home="$WORK/badtok"
mkdir -p "$bad_home"
bad_conf="$bad_home/c.conf"
printf 'PROTECT_NAMES="-delete"\n' >"$bad_conf"
bad_out=$(HOME="$bad_home" bash "$SCRIPT" --scope user --conf "$bad_conf" --print-config 2>&1) && bad_rc=0 || bad_rc=$?
if [ "$bad_rc" -eq 2 ] && printf '%s' "$bad_out" | grep -q 'PROTECT_NAMES token refused'; then
  ok "dangerous protect token refused"
else
  bad "dangerous protect token rc=$bad_rc"
  printf '%s\n' "$bad_out" >&2
fi

# Root scope without uid 0 is rejected before any cleaner runs.
root_out=$(HOME="$WORK" bash "$SCRIPT" --scope root --print-config 2>&1) && root_rc=0 || root_rc=$?
if [ "$(id -u)" -ne 0 ]; then
  if [ "$root_rc" -eq 2 ] && printf '%s' "$root_out" | grep -q 'must run as root'; then
    ok "root scope refused for non-root"
  else
    bad "root scope rc=$root_rc"
  fi
fi

# uv tests use only a PATH stub and a private proc tree/cache.
run_uv_cache() {
  local name="$1" holder="$2" dry="$3" stub_mode="$4" status="$5" note="$6" prune="$7"
  local home="$WORK/uv-$name" rc expected_output
  local cache="$home/cache" proc="$home/proc" state="$home/state" log="$home/uv.log"
  mkdir -p "$cache" "$proc" "$state"
  install_stubs "$home"
  : >"$cache/.lock"
  : >"$log"
  [ "$holder" = missing ] && rm "$cache/.lock"
  case "$stub_mode" in
    ok) expected_output="prune completed" ;;
    lock) expected_output="Timeout acquiring cache lock: currently in-use" ;;
    waiting) expected_output="waiting for lock timed out" ;;
    timeout) expected_output="cache lock timeout" ;;
    error) expected_output="unrelated failure" ;;
  esac
  if [ "$holder" = 1 ]; then
    mkdir -p "$proc/12345/fd"
    ln -s "$cache/.lock" "$proc/12345/fd/3"
    ln -s "$cache/.lock" "$proc/12345/fd/4" # same PID must count once
  fi
  cat >"$home/.local/bin/uv" <<'EOF'
#!/bin/sh
printf 'argv=%s UV_LOCK_TIMEOUT=%s\n' "$*" "${UV_LOCK_TIMEOUT:-unset}" >>"${DISK_CLEAN_STUB_LOG:?}"
case "$*" in
  'cache dir') printf '%s\n' "${UV_TEST_CACHE:?}" ;;
  'cache prune')
    case "${UV_TEST_MODE:?}" in
      ok) echo 'prune completed' ;;
      lock) echo 'Timeout acquiring cache lock: currently in-use' >&2; exit 2 ;;
      waiting) echo 'waiting for lock timed out' >&2; exit 2 ;;
      timeout) echo 'cache lock timeout' >&2; exit 2 ;;
      error) echo 'unrelated failure' >&2; exit 7 ;;
    esac ;;
  *) exit 99 ;;
esac
EOF
  cat >"$home/c.conf" <<EOF
DRY_RUN=$dry
CATEGORIES="uv_cache"
MOUNT=/
ROOT_SUMMARY=$home/missing-root.json
EOF
  DISK_CLEAN_PROC_ROOT="$proc" DISK_CLEAN_STATE="$state" DISK_CLEAN_STUB_LOG="$log" \
    UV_TEST_CACHE="$cache" UV_TEST_MODE="$stub_mode" HOME="$home" \
    bash "$SCRIPT" --scope user --conf "$home/c.conf" >/dev/null 2>&1; rc=$?
  # Read only category evidence; never print the summary's host/user fields.
  if [ "$rc" -ne 0 ] || ! python3 - "$state/last.json" "$status" "$note" <<'PYTEST'
import json, sys
category = json.load(open(sys.argv[1]))['categories']['uv_cache']
assert category['status'] == sys.argv[2]
assert sys.argv[3] in category['note']
PYTEST
  then
    bad "uv $name summary rc=$rc"
    return
  fi
  if grep -q -- '--force' "$log"; then
    bad "uv $name passed force"
  elif [ "$prune" = yes ]; then
    if ! grep -qx 'argv=cache prune UV_LOCK_TIMEOUT=60' "$log"; then
      bad "uv $name prune invocation"
    elif ! grep -Fxq "$expected_output" "$state/log/"*.log; then
      bad "uv $name prune output missing from log"
    else
      ok "uv $name"
    fi
  elif grep -q 'argv=cache prune' "$log"; then
    bad "uv $name unexpectedly pruned"
  else
    ok "uv $name"
  fi
}

run_uv_cache holder 1 0 ok skipped 'deferred: cache lock held by 1' no
run_uv_cache missing-lock missing 0 ok ok 'cache_before=' yes
run_uv_cache free 0 0 ok ok 'cache_before=' yes
run_uv_cache busy 0 0 lock skipped 'deferred: uv cache lock busy' yes
run_uv_cache waiting 0 0 waiting skipped 'deferred: uv cache lock busy' yes
run_uv_cache timeout 0 0 timeout skipped 'deferred: uv cache lock busy' yes
run_uv_cache failure 0 0 error error 'uv cache prune failed rc=7' yes
run_uv_cache dry 1 1 ok ok 'lock_holders=1' no

printf '\ndisk-clean tests passed: %s failed: %s\n' "$passed" "$failed"
[ "$failed" -eq 0 ]
