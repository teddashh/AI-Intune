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

printf '\ndisk-clean tests passed: %s failed: %s\n' "$passed" "$failed"
[ "$failed" -eq 0 ]
