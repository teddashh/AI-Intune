#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export HOME=$TMP/home
export DAILY_CHECK_ROOT=$TMP/evidence
export PATH=$TMP/bin:$PATH
export PYTHONPATH=$TMP/python
export COMMAND_LOG=$TMP/commands
export DISK_PCT=20 FAILED_UNITS=0 FAKE_UID=0
INSTALL=$ROOT/ops/maintenance/install-timers.sh
UNINSTALL=$ROOT/ops/maintenance/uninstall-timers.sh
CHECK=$ROOT/ops/fleet/daily-check/daily-check.sh
passed=0 failed=0 output='' rc=0
run() { if output=$("$@" 2>&1); then rc=0; else rc=$?; fi; }
ok() { passed=$((passed+1)); printf 'ok - %s\n' "$1"; }
bad() { failed=$((failed+1)); printf 'not ok - %s (exit %s; output withheld)\n' "$1" "$rc"; }
expect() { local name=$1; shift; if "$@"; then ok "$name"; else bad "$name"; fi; }
exit_is() { [ "$rc" = "$1" ]; }
contains() { [[ "$output" == *"$1"* ]]; }
sudo_lines() {
    local line count=0
    while IFS= read -r line; do
        if [[ "$line" == *sudo* ]]; then
            [[ "$line" == "sudo bash -c '"* && "$line" == *"'" ]] || return 1
            count=$((count+1))
        fi
    done <<< "$output"
    [ "$count" -eq 1 ]
}
mkdir -p "$TMP/bin" "$HOME" "$DAILY_CHECK_ROOT/var/lib/disk-clean" "$DAILY_CHECK_ROOT/etc/systemd/journald.conf.d" "$HOME/.local/state/disk-clean"
cat > "$TMP/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$COMMAND_LOG"
case " $* " in
    *' --failed '*) [ "$FAILED_UNITS" = 0 ] || printf 'host-a.service loaded failed failed\n' ;;
    *' show '*.timer*) if [ "${UNTRIGGERED:-0}" = 1 ]; then printf 'ActiveState=active\nLastTriggerUSec=n/a\n'; else printf 'ActiveState=active\nLastTriggerUSec=%s\n' "$(date -u '+%Y-%m-%d %H:%M:%S UTC')"; fi ;;
    *' show '*.service*) printf 'Result=success\n' ;;
esac
STUB
cat > "$TMP/bin/df" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/fake 100 20 80 %s%% %s\n' "$DISK_PCT" "${*: -1}"
STUB
cat > "$TMP/bin/journalctl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'Archived and active journals take up 25.0M in the file system.\n'
STUB
cat > "$TMP/bin/timedatectl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf 'NTPSynchronized=yes\n'
STUB
cat > "$TMP/bin/id" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in -u) printf '%s\n' "$FAKE_UID" ;; -un) printf 'ops\n' ;; *) exit 2 ;; esac
STUB
for cmd in loginctl systemd-tmpfiles; do
    cat > "$TMP/bin/$cmd" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s %s\n' "${0##*/}" "$*" >> "$COMMAND_LOG"
STUB
done
mkdir -p "$TMP/python"
printf 'import socket\nsocket.gethostname = lambda: "host-a"\n' > "$TMP/python/sitecustomize.py"
chmod +x "$TMP/bin/"*
: > "$COMMAND_LOG"
fixture() {
    python3 - "$HOME/.local/state/disk-clean/last.json" "$DAILY_CHECK_ROOT/var/lib/disk-clean/last.json" "${1:-0}" <<'PY'
import datetime as dt,json,sys
for path,scope in zip(sys.argv[1:3],['user','root']):
    ts=dt.datetime.now(dt.timezone.utc)-dt.timedelta(hours=int(sys.argv[3]))
    with open(path,'w') as f:
        json.dump(dict(schema='fleet-disk-clean/v1',scope=scope,ts=ts.isoformat(),mode='dry-run',freed_bytes=0,attention=''),f)
PY
    cp "$ROOT/ops/maintenance/journald/50-disk-clean.conf" "$DAILY_CHECK_ROOT/etc/systemd/journald.conf.d/50-disk-clean.conf"
}
fixture
run bash "$INSTALL" --scope root --prefix "$TMP/stage"
expect 'install plan succeeds' exit_is 0
expect 'install plan changes nothing' test ! -e "$TMP/stage"
run bash "$INSTALL" --scope root --prefix "$TMP/stage" --apply
expect 'root stage apply' exit_is 0
expect 'root cleaner executable mode' test "$(stat -c %a "$TMP/stage/usr/local/sbin/disk-clean")" = 755
expect 'root conf private mode' test "$(stat -c %a "$TMP/stage/etc/disk-clean/root.conf")" = 600
expect 'root unit readable mode' test "$(stat -c %a "$TMP/stage/etc/systemd/system/disk-clean-root.timer")" = 644
expect 'cleaner exact copy' cmp -s "$ROOT/ops/maintenance/disk-clean" "$TMP/stage/usr/local/sbin/disk-clean"
expect 'journald header installed' grep -qx '\[Journal\]' "$TMP/stage/etc/systemd/journald.conf.d/50-disk-clean.conf"
expect 'tmpfiles installed' test -f "$TMP/stage/etc/tmpfiles.d/00-disk-clean.conf"
run bash "$INSTALL" --scope root --prefix "$TMP/stage" --apply
expect 'second apply succeeds' exit_is 0
expect 'second apply creates no backups' test -z "$(find "$TMP/stage" -name '*.bak-*' -print)"
printf '\n# keep custom config\n' >> "$TMP/stage/etc/disk-clean/root.conf"
cp "$TMP/stage/etc/disk-clean/root.conf" "$TMP/kept"
run bash "$INSTALL" --scope root --prefix "$TMP/stage" --apply
expect 'existing conf preserved' cmp -s "$TMP/kept" "$TMP/stage/etc/disk-clean/root.conf"
expect 'diff summary without content' contains 'template differs'
run bash "$INSTALL" --scope root --prefix "$TMP/stage" --apply --approve
expect 'approve flips dry-run' grep -qx 'DRY_RUN=0' "$TMP/stage/etc/disk-clean/root.conf"
backup=$(find "$TMP/stage/etc/disk-clean" -name '*.bak-*' -print -quit)
expect 'approve backs up config' cmp -s "$TMP/kept" "$backup"
run env FAKE_UID=1000 bash "$INSTALL" --scope root --prefix "$TMP/nonroot" --apply
expect 'nonroot apply rejected' exit_is 1
expect 'root apply sudo hint single line' sudo_lines
expect 'nonroot changes nothing' test ! -e "$TMP/nonroot"
run bash "$INSTALL" --scope user --prefix "$TMP/userstage" --with-daily-check --apply
expect 'user stage apply' exit_is 0
for name in disk-clean disk-clean-weekly fleet-daily-check; do
    expect "$name user timer installed" test -f "$TMP/userstage$HOME/.config/systemd/user/$name.timer"
done
expect 'daily checker executable' test "$(stat -c %a "$TMP/userstage$HOME/.local/bin/fleet-daily-check")" = 755
expect 'prefix never contacts systemd' test ! -s "$COMMAND_LOG"
run env FAKE_UID=1000 bash "$INSTALL" --scope user --with-daily-check --apply
expect 'live user apply uses fakes' exit_is 0
expect 'linger sudo hint single line' sudo_lines
expect 'only timers enabled' grep -q 'enable --now disk-clean.timer' "$COMMAND_LOG"
expect 'no service started or journald restarted' bash -c '! grep -Eq "(^| )(start|restart)( |$)|enable --now .*\.service" "$1"' _ "$COMMAND_LOG"
run bash "$CHECK" --json
expect 'fresh summary verdict ok' exit_is 0
expect 'JSON parses and includes both scopes' bash -c 'python3 -c '\''import json,sys; s=json.load(sys.stdin); assert s["verdict"]=="ok"; assert set(s["disk_clean"])=={"root","user"}'\'' <<< "$1"' _ "$output"
fixture 49
run bash "$CHECK"
expect 'stale summary warns' exit_is 1
expect 'stale reason' contains 'summary stale'
fixture
run env DISK_PCT=96 bash "$CHECK" --json
expect '96 percent critical' exit_is 2
run env DISK_PCT=85 bash "$CHECK"
expect '85 percent warns' exit_is 1
run env FAILED_UNITS=1 bash "$CHECK"
expect 'failed unit warns' exit_is 1
printf 'SystemMaxUse=500M\n' > "$DAILY_CHECK_ROOT/etc/systemd/journald.conf.d/50-disk-clean.conf"
run bash "$CHECK"
expect 'missing Journal header warns' exit_is 1
expect 'missing header reason' contains 'Journal header'
fixture
run env UNTRIGGERED=1 bash "$CHECK" --json
expect 'active timer waiting for first run is not a warning' exit_is 0
expect 'first run pending recorded' contains '"first_run_pending":true'
printf 'SystemMaxUse=700M\n' > "$DAILY_CHECK_ROOT/etc/systemd/journald.conf.d/99-size.conf"
run bash "$CHECK"
expect 'headerless extra drop-in warns' exit_is 1
expect 'headerless drop-in named' contains '99-size.conf'
rm "$DAILY_CHECK_ROOT/etc/systemd/journald.conf.d/99-size.conf"
rm "$DAILY_CHECK_ROOT/var/lib/disk-clean/last.json"
run bash "$CHECK" --skip-root --json
expect 'skip-root ignores missing root summary' exit_is 0
fixture
rm "$DAILY_CHECK_ROOT/var/lib/disk-clean/last.json"
run bash "$CHECK"
expect 'missing root summary warns' exit_is 1
fixture
printf 'not json\n' > "$HOME/.local/state/disk-clean/last.json"
run bash "$CHECK"
expect 'invalid user summary warns' exit_is 1
fixture
mkdir -p "$DAILY_CHECK_ROOT/var/run"
: > "$DAILY_CHECK_ROOT/var/run/reboot-required"
run bash "$CHECK"
expect 'reboot required warns' exit_is 1
rm "$DAILY_CHECK_ROOT/var/run/reboot-required"
run bash "$CHECK" --state-dir "$TMP/readonly"
expect 'checker without write leaves state absent' test ! -e "$TMP/readonly"
fixture
# Fake secret is deliberately placed only on disk, never in argv or failure output.
printf 'fake-private-token-value\n' > "$HOME/token"
python3 - "$HOME/.local/state/disk-clean/last.json" "$HOME/token" <<'PY'
import json,sys
p=sys.argv[1]; s=json.load(open(p)); s['attention']=open(sys.argv[2]).read(); s['unknown']=s['attention']; json.dump(s,open(p,'w'))
PY
run bash "$CHECK" --json --write --state-dir "$TMP/state"
expect 'attention warns' exit_is 1
expect 'fake token never in output or files' python3 - "$HOME/token" "$TMP/state" "$output" <<'PY'
import pathlib,sys
secret=pathlib.Path(sys.argv[1]).read_text().strip()
assert secret not in sys.argv[3]
for p in pathlib.Path(sys.argv[2]).iterdir(): assert secret not in p.read_text()
PY
fixture
run bash "$CHECK" --json --write --state-dir "$TMP/state"
expect 'write succeeds' exit_is 0
expect 'write both files with matching timestamp and no temporary files' python3 - "$TMP/state" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]); s=json.loads((p/'summary.json').read_text()); line=(p/'summary.txt').read_text()
assert line.startswith(s['ts']+' ') and ' ok ' in line and len(line.splitlines())==1
assert sorted(x.name for x in p.iterdir())==['summary.json','summary.txt']
assert all(x.stat().st_mode & 0o777 == 0o600 for x in p.iterdir())
PY
# Observe os.replace directly to verify both destinations use atomic replacement.
python3 - "$CHECK" "$TMP/atomic" <<'PY'
import pathlib,sys
source=pathlib.Path(sys.argv[1]).read_text().split("<<'PY'\n",1)[1].rsplit('\nPY',1)[0]
import os,contextlib,io,socket
socket.gethostname=lambda: "host-a"
real=os.replace
seen=[]
def replace(a,b):
    assert pathlib.Path(a).parent==pathlib.Path(b).parent
    seen.append(pathlib.Path(b).name)
    return real(a,b)
os.replace=replace
sys.argv=['checker','1','1',sys.argv[2],'0']
try:
    with contextlib.redirect_stdout(io.StringIO()):
        exec(compile(source,'daily-check','exec'))
except SystemExit as e: assert e.code==0
assert seen==['summary.json','summary.txt']
PY
ok 'both writes use same-directory atomic replace'
run bash "$UNINSTALL" --scope root --prefix "$TMP/stage"
expect 'uninstall plan' exit_is 0
expect 'uninstall plan retains timer' test -f "$TMP/stage/etc/systemd/system/disk-clean-root.timer"
run bash "$UNINSTALL" --scope root --prefix "$TMP/stage" --apply
expect 'uninstall apply removes timer' test ! -e "$TMP/stage/etc/systemd/system/disk-clean-root.timer"
expect 'uninstall retains config' test -f "$TMP/stage/etc/disk-clean/root.conf"
run bash "$UNINSTALL" --scope root --prefix "$TMP/stage" --apply --purge
expect 'purge removes config with backup' test ! -e "$TMP/stage/etc/disk-clean/root.conf"
run env FAKE_UID=1000 bash "$UNINSTALL" --scope root --apply
expect 'uninstall sudo hint single line' sudo_lines
expect 'embedded cleaner unchanged' cmp -s "$ROOT/ops/maintenance/disk-clean" "$ROOT/internal/maintenance/script/disk-clean"
if command -v systemd-analyze >/dev/null 2>&1; then
    # Verify in a private root with executable placeholders, not host services.
    mkdir -p "$TMP/verify/etc/systemd/system" "$TMP/verify/usr/local/sbin" "$TMP/verify/bin"
    cp "$ROOT/ops/maintenance/systemd/system/"* "$TMP/verify/etc/systemd/system/"
    cp /bin/true "$TMP/verify/usr/local/sbin/disk-clean"
    printf '[Unit]\nDescription=Test target\n' > "$TMP/verify/etc/systemd/system/sysinit.target"
    for unit in "$ROOT/ops/maintenance/systemd/user/"* "$ROOT/ops/fleet/daily-check/"*.service "$ROOT/ops/fleet/daily-check/"*.timer; do
        sed 's|%h/.local/bin/disk-clean|/usr/local/sbin/disk-clean|; s|%h/.local/bin/fleet-daily-check|/usr/local/sbin/disk-clean|' "$unit" > "$TMP/verify/etc/systemd/system/${unit##*/}"
    done
    run systemd-analyze --root="$TMP/verify" verify "$TMP/verify/etc/systemd/system/"*.service "$TMP/verify/etc/systemd/system/"*.timer
    expect 'systemd unit verification' exit_is 0
else
    ok 'systemd-analyze unavailable; unit verification skipped'
fi
printf '\npassed: %s, failed: %s\n' "$passed" "$failed"
[ "$failed" = 0 ]
