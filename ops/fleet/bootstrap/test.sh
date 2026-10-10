#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/home" "$TMP/root/etc"
export HOME=$TMP/home
export PATH=$TMP/bin:/usr/bin:/bin
export TEST_ROOT=$TMP TEST_UID=0
export FLEET_TIMERS_INSTALLER=$TMP/missing/install-timers.sh
export FLEET_AI_CLI_INSTALLER=$TMP/missing/install-ai-clis.sh
unset SUDO_USER HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy
passed=0 failed=0 output='' rc=0
BOOT=$ROOT/ops/fleet/bootstrap/bootstrap-host.sh
NET=$ROOT/ops/fleet/net
cat > "$TMP/bin/id" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    '-u') echo "${TEST_UID:-0}";;
    '-un') /usr/bin/id -un;;
    '-u '*) echo 1000;;
    *) /usr/bin/id "$@";;
esac
STUB
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
cat >/dev/null
printf '%s\n' "$*" >> "$TEST_ROOT/argv"
case "$*" in *'--noproxy'* ) exit 7;; *) exit 0;; esac
STUB
cat > "$TMP/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TEST_ROOT/calls"
if [[ $1 == is-active && ${*: -1} != chrony && ${*: -1} != clawctl-agent ]]; then exit 3; fi
STUB
cat > "$TMP/bin/timedatectl" <<'STUB'
#!/usr/bin/env bash
if [[ $* == *--value* ]]; then echo no; else echo NTPSynchronized=no; fi
STUB
cat > "$TMP/bin/chronyc" <<'STUB'
#!/usr/bin/env bash
if [[ $* == tracking ]]; then echo 'System time     : 180.000 seconds slow of NTP time'; else
printf 'MS Name/IP address         Stratum Poll Reach LastRx Last sample\n^? 192.0.2.10 2 6 0 - +180s[+180s] +/- 0ms\n'
fi
STUB
for cmd in loginctl tailscale sudo ssh getent; do
    printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$*" >> "$TEST_ROOT/calls"\nexit 0\n' > "$TMP/bin/$cmd"
done
cat > "$TMP/bin/runuser" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TEST_ROOT/runuser-calls"
shift 3
TEST_INSTALLER_USER=$SUDO_USER "$@"
STUB
cat > "$TMP/bin/stat" <<'STUB'
#!/usr/bin/env bash
if [[ $* == "-c %u $TEST_ROOT/key" ]]; then echo 0; else /usr/bin/stat "$@"; fi
STUB
cat > "$TMP/bin/tailscale" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TEST_ROOT/calls"
if [[ $* == 'ip -4' ]]; then echo 100.64.0.10; fi
if [[ ${FAIL_TAILSCALE:-0} == 1 ]]; then echo FAKE-PRIVATE-TOKEN-VALUE >&2; exit 1; fi
STUB
chmod +x "$TMP/bin/"*
run() { if output=$("$@" 2>&1); then rc=0; else rc=$?; fi; }
has() { [[ $output == *"$1"* ]]; }
lacks() { ! has "$1"; }
expect() {
    local label=$1 want=$2; shift 2
    if [[ $rc == "$want" ]] && "$@"; then printf 'ok - %s\n' "$label"; passed=$((passed+1));
    else printf 'not ok - %s (exit %s)\n' "$label" "$rc"; failed=$((failed+1)); fi
}
valid_hints() {
    printf '%s\n' "$output" | python3 -c 'import sys; lines=sys.stdin.read().splitlines(); hints=[l for l in lines if l.startswith("sudo")]; assert hints; assert all(l.startswith("sudo bash -c " + chr(39)) and l.endswith(chr(39)) for l in hints); assert not any(l.startswith("printf ") for l in lines)'
}
no_files() { [[ ! -e $TMP/state && ! -e $TMP/root/etc/systemd ]]; }
run bash "$BOOT" --phases journald,linger,disk-clean,ai-cli --state-dir "$TMP/state" --root-prefix "$TMP/root"
expect 'plan changes no config or state' 0 no_files
expect 'plan root hints occupy one line' 0 valid_hints
TEST_UID=1000 run bash "$BOOT" --apply --phases journald,linger --state-dir "$TMP/state" --root-prefix "$TMP/root"
expect 'non-root apply prints explicit root hints' 0 valid_hints
expect 'non-root apply does not write root config' 0 test ! -e "$TMP/root/etc/systemd"
expect 'needs-root recorded' 0 grep -q needs-root "$TMP/state/state.json"
run bash "$BOOT" --apply --phases journald,disk-clean,ai-cli --state-dir "$TMP/state" --root-prefix "$TMP/root"
expect 'root-prefix journald header' 0 grep -qx '\[Journal\]' "$TMP/root/etc/systemd/journald.conf.d/50-disk-clean.conf"
expect 'root-prefix journal disk limit' 0 grep -qx 'SystemMaxUse=500M' "$TMP/root/etc/systemd/journald.conf.d/50-disk-clean.conf"
expect 'optional timer installer skipped' 0 has 'skipped: install-timers.sh not in this checkout'
expect 'optional AI installer skipped' 0 has 'skipped: install-ai-clis.sh not in this checkout'
run bash "$BOOT" --apply --resume --phases journald --state-dir "$TMP/state" --root-prefix "$TMP/root"
expect 'resume skips done phase' 0 has 'resume: journald done'
# Present installers are always temporary stubs, regardless of checkout contents.
mkdir -p "$TMP/installers"
for installer in install-timers.sh install-ai-clis.sh; do
    cat > "$TMP/installers/$installer" <<'STUB'
#!/usr/bin/env bash
printf '%s: %s: %s\n' "${0##*/}" "${TEST_INSTALLER_USER:-main}" "$*" >> "$TEST_ROOT/installers.log"
STUB
done
cat > "$TMP/bin/getent" <<'STUB'
#!/usr/bin/env bash
if [[ $1 == passwd ]]; then
    printf '%s:x:1000:1000::%s:/bin/bash\n' "$2" "$HOME"
else
    exit 0
fi
STUB
cat > "$TMP/bin/chown" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$TMP/bin/getent" "$TMP/bin/chown"
export FLEET_TIMERS_INSTALLER=$TMP/installers/install-timers.sh
export FLEET_AI_CLI_INSTALLER=$TMP/installers/install-ai-clis.sh
for installer_mode in apply plan; do
    : > "$TMP/installers.log"
    TEST_UID=1000 run bash "$BOOT" "--$installer_mode" --phases disk-clean,ai-cli --state-dir "$TMP/delegated-state"
    expect "user timer $installer_mode argv" 0 grep -qx "install-timers.sh: main: --scope user --$installer_mode" "$TMP/installers.log"
    expect "user AI $installer_mode argv" 0 grep -qx "install-ai-clis.sh: main: --$installer_mode" "$TMP/installers.log"
    expect "user AI $installer_mode has no root wrapper" 0 lacks "bash $FLEET_AI_CLI_INSTALLER"
    if [[ $installer_mode == plan ]]; then
        expect 'non-root timer root plan runs directly' 0 grep -qx 'install-timers.sh: main: --scope root --plan' "$TMP/installers.log"
        expect 'non-root delegated plans have no sudo hints' 0 lacks 'sudo'
    else
        expect 'non-root timer apply retains root hint' 0 valid_hints
        expect 'non-root timer root apply is deferred' 0 bash -c '! grep -q -- "--scope root" "$1"' _ "$TMP/installers.log"
    fi
    : > "$TMP/installers.log"
    : > "$TMP/runuser-calls"
    TEST_UID=0 SUDO_USER=ops run bash "$BOOT" "--$installer_mode" --phases disk-clean,ai-cli --state-dir "$TMP/delegated-state"
    expect "sudo user timer $installer_mode argv" 0 grep -qx "install-timers.sh: ops: --scope user --$installer_mode" "$TMP/installers.log"
    expect "root timer $installer_mode argv" 0 grep -qx "install-timers.sh: main: --scope root --$installer_mode" "$TMP/installers.log"
    expect "sudo user AI $installer_mode argv" 0 grep -qx "install-ai-clis.sh: ops: --$installer_mode" "$TMP/installers.log"
    expect "AI $installer_mode delegates to main user" 0 grep -Fxq -- "-u ops -- bash $FLEET_AI_CLI_INSTALLER --$installer_mode" "$TMP/runuser-calls"
    expect "root delegated $installer_mode has no sudo hints" 0 lacks 'sudo'
done
export FLEET_TIMERS_INSTALLER=$TMP/missing/install-timers.sh
export FLEET_AI_CLI_INSTALLER=$TMP/missing/install-ai-clis.sh
mkdir -m 755 "$TMP/package"
printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/package/install-agent.sh"
printf '%s\n' 'FAKE-PRIVATE-TOKEN-VALUE' > "$TMP/package/enroll-token"
chmod 600 "$TMP/package/enroll-token"
run bash "$BOOT" --apply --phases agent --package-dir "$TMP/package" --state-dir "$TMP/state"
expect 'wrong package mode refused' 1 has '0700'
expect 'token never printed on failure' 1 lacks 'FAKE-PRIVATE-TOKEN-VALUE'
expect 'failed phase recorded' 1 grep -q failed "$TMP/state/state.json"
printf 'HTTPS_PROXY="http://user:pass@proxy.corp.example:8080"\nNO_PROXY="localhost,.ts.net"\n' > "$TMP/root/etc/environment"
run bash "$NET/proxy-check.sh" --hub https://hub.example.com --root-prefix "$TMP/root"
expect 'proxy credential redacted' 0 lacks 'user:pass'
expect 'proxy secret absent from curl argv' 0 bash -c '! grep -q "user:pass" "$1"' _ "$TMP/argv"
expect 'missing proxy drop-in detected' 0 has 'dropin_present: false'
expect 'missing tailnet range detected' 0 has '100.64.0.0/10'
expect 'direct failure reported' 0 has 'direct: false'
expect 'proxy reachability reported' 0 has 'proxy: true'
expect 'proxy fix is a single-line sudo hint' 0 valid_hints
# Execute the printed fix only under the test prefix and with stub services.
hint=$(printf '%s\n' "$output" | sed -n '/^sudo bash -c /p')
run bash -c "${hint#sudo }"
expect 'proxy fix installs drop-in' 0 test -f "$TMP/root/etc/systemd/system/clawctl-agent.service.d/10-proxy.conf"
run bash "$NET/proxy-check.sh" --hub https://hub.example.com --root-prefix "$TMP/root" --json
expect 'proxy drop-in matches endpoint and required bypasses' 0 has '"verdict": "ok"'
expect 'JSON output redacts proxy credentials' 0 lacks 'user:pass'
run bash "$NET/ntp-check.sh" --root-prefix "$TMP/root"
expect 'zero reach chrony sources parsed' 0 has 'reachable_sources: 0'
expect 'unsynchronized clock reports drift' 0 has 'verdict: drift'
expect 'offset reported' 0 has '180.000 seconds'
expect 'NTP fix is a single-line sudo hint' 0 valid_hints
run bash "$NET/ntp-check.sh" --apply --site-ntp ntp.corp.example --root-prefix "$TMP/root"
expect 'NTP apply writes NTS source' 0 grep -qx 'server time.cloudflare.com iburst nts prefer' "$TMP/root/etc/chrony/sources.d/10-local.sources"
expect 'NTP apply adds site source' 0 grep -qx 'server ntp.corp.example iburst' "$TMP/root/etc/chrony/sources.d/10-local.sources"
printf 'old\n' > "$TMP/root/etc/chrony/sources.d/10-local.sources"
run bash "$NET/ntp-check.sh" --apply --root-prefix "$TMP/root"
expect 'NTP backs up replaced config' 0 bash -c 'compgen -G "$1.bak-*" >/dev/null' _ "$TMP/root/etc/chrony/sources.d/10-local.sources"
SSH=$NET/ssh-config-tailscale-nc.sh
config=$TMP/home/.ssh/config
run bash "$SSH" --name host-a --ip 100.64.0.10 --user ops --config "$config" --write
expect 'SSH writes userspace proxy command' 0 grep -q 'ProxyCommand /usr/bin/tailscale' "$config"
expect 'SSH config mode 600' 0 test "$(stat -c %a "$config")" = 600
cp "$config" "$TMP/first"
run bash "$SSH" --name host-a --ip 100.64.0.10 --user ops --config "$config" --write
expect 'SSH write is idempotent' 0 cmp -s "$config" "$TMP/first"
run bash "$SSH" --name host-a --ip 100.64.0.10 --user ops --identity /tmp/key --config "$config" --write
expect 'SSH marker replacement updates identity' 0 grep -q 'IdentityFile /tmp/key' "$config"
expect 'SSH replacement keeps backup' 0 bash -c 'compgen -G "$1.bak-*" >/dev/null' _ "$config"
printf 'Host host-b\n    User ops\n' >> "$config"
run bash "$SSH" --name host-b --ip 100.64.0.10 --user ops --config "$config" --write
expect 'unmarked SSH alias refused' 1 has 'outside fleet markers'
run bash "$SSH" --name host-b --ip 100.64.0.10 --user ops --config "$config" --write --rename-existing host-old
expect 'explicit rename preserves old alias' 0 grep -qx 'Host host-old' "$config"
run bash "$SSH" --check host-a --config "$config"
expect 'SSH check uses stub' 0 has 'SSH: ok'
expect 'SSH check uses batch mode and timeout' 0 grep -q 'BatchMode=yes -o ConnectTimeout=10 host-a true' "$TMP/calls"
run bash "$BOOT" --phases preflight --hub https://hub.example.com --root-prefix "$TMP/root" --state-dir "$TMP/plan-state"
expect 'preflight reports OS user and DNS' 0 has 'hub DNS: ok'
expect 'preflight leaves state absent' 0 test ! -e "$TMP/plan-state"
expect 'fake token absent from command logs' 0 bash -c '! grep -q FAKE-PRIVATE-TOKEN-VALUE "$1" "$2"' _ "$TMP/calls" "$TMP/argv"
# Root service failures must remain retryable, rather than recording done.
cat > "$TMP/bin/loginctl" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
run bash "$BOOT" --apply --phases linger --state-dir "$TMP/state"
expect 'root command failure propagates' 1 has 'failed: linger'
expect 'root command failure state is failed' 1 python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["linger"]["status"]=="failed"' "$TMP/state/state.json"
# A deliberately noisy staged installer must never leak its fake token.
chmod 700 "$TMP/package"
cat > "$TMP/package/install-agent.sh" <<'STUB'
#!/usr/bin/env bash
cat enroll-token
echo FAKE-PRIVATE-TOKEN-VALUE >&2
exit "${FAIL_INSTALL:-0}"
STUB
chmod +x "$TMP/package/install-agent.sh"
run bash "$BOOT" --apply --phases agent --package-dir "$TMP/package" --state-dir "$TMP/state"
expect 'staged enrollment delegates successfully' 0 has 'Remove the staged package'
expect 'successful installer token output suppressed' 0 lacks 'FAKE-PRIVATE-TOKEN-VALUE'
FAIL_INSTALL=1 run bash "$BOOT" --apply --phases agent --package-dir "$TMP/package" --state-dir "$TMP/state"
expect 'failed staged enrollment propagates' 1 has 'failed: agent'
expect 'failed installer token output suppressed' 1 lacks 'FAKE-PRIVATE-TOKEN-VALUE'
printf 'FAKE-PRIVATE-TOKEN-VALUE\n' > "$TMP/key"
chmod 600 "$TMP/key"
run bash "$BOOT" --phases tailscale --hostname host-a --tailscale-auth-key-file "$TMP/key" --state-dir "$TMP/state"
expect 'Tailscale plan retains key' 0 test -f "$TMP/key"
expect 'Tailscale root hint is one line' 0 valid_hints
run bash "$BOOT" --apply --phases tailscale --hostname host-a --tailscale-auth-key-file "$TMP/key" --state-dir "$TMP/state"
expect 'Tailscale verifies IPv4' 0 has '100.64.0.10'
expect 'Tailscale success removes key' 0 test ! -e "$TMP/key"
expect 'Tailscale never prints key contents' 0 lacks 'FAKE-PRIVATE-TOKEN-VALUE'
printf 'FAKE-PRIVATE-TOKEN-VALUE\n' > "$TMP/key"
chmod 600 "$TMP/key"
FAIL_TAILSCALE=1 run bash "$BOOT" --apply --phases tailscale --hostname host-a --tailscale-auth-key-file "$TMP/key" --state-dir "$TMP/state"
expect 'failed Tailscale keeps key for retry' 1 test -f "$TMP/key"
expect 'failed Tailscale output cannot leak key' 1 lacks 'FAKE-PRIVATE-TOKEN-VALUE'
run bash "$BOOT" --apply --phases tailscale --no-tailscale --state-dir "$TMP/state"
expect 'explicit no-tailscale skips phase' 0 has 'skipped: --no-tailscale'
# An Oracle-style config needs a sourcedir, and only one copy is added.
printf 'pool ntp.corp.example iburst\n' > "$TMP/root/etc/chrony.conf"
run bash "$NET/ntp-check.sh" --apply --root-prefix "$TMP/root"
expect 'chrony main config loads sources directory' 0 grep -qx 'sourcedir /etc/chrony/sources.d' "$TMP/root/etc/chrony.conf"
expect 'chrony main config backup exists' 0 bash -c 'compgen -G "$1.bak-*" >/dev/null' _ "$TMP/root/etc/chrony.conf"
cp "$TMP/root/etc/chrony.conf" "$TMP/chrony-before"
cp "$TMP/calls" "$TMP/calls-before"
run bash "$NET/ntp-check.sh" --apply --root-prefix "$TMP/root"
expect 'chrony apply preserves identical main config' 0 cmp -s "$TMP/chrony-before" "$TMP/root/etc/chrony.conf"
expect 'chrony apply avoids redundant restart' 0 test "$(grep -c 'restart chrony' "$TMP/calls")" = "$(grep -c 'restart chrony' "$TMP/calls-before")"
# timesyncd hosts use their own drop-in and restart only that daemon.
cat > "$TMP/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$TEST_ROOT/calls"
if [[ $1 == is-active && ${*: -1} != systemd-timesyncd ]]; then exit 3; fi
STUB
run bash "$NET/ntp-check.sh" --apply --root-prefix "$TMP/root" --site-ntp ntp.corp.example
expect 'timesyncd drop-in has Time header' 0 grep -qx '\[Time\]' "$TMP/root/etc/systemd/timesyncd.conf.d/10-local.conf"
expect 'timesyncd sets source list' 0 grep -qx 'NTP=time.cloudflare.com ntp.corp.example' "$TMP/root/etc/systemd/timesyncd.conf.d/10-local.conf"
TEST_UID=1000 run bash "$NET/ntp-check.sh" --apply --root-prefix "$TMP/root"
expect 'non-root NTP apply prints a single-line root step' 0 valid_hints
TEST_UID=1000 run bash "$NET/ntp-check.sh" --apply --json --root-prefix "$TMP/root"
expect 'NTP JSON remains valid on non-root apply' 0 python3 -c 'import json,sys; json.loads(sys.argv[1])' "$output"
printf '\npassed: %s, failed: %s\n' "$passed" "$failed"
[[ $failed == 0 ]]
