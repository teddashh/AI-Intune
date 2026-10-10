#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
# shellcheck source=ops/fleet/common.sh
source "$ROOT/ops/fleet/common.sh"
mode=plan phases=preflight,tailscale,journald,disk-clean,ai-cli,agent,linger
hub='' hostname='' key='' package='' prefix='' resume=0 no_tailscale=0
main_user=${SUDO_USER:-$(id -un)}
user_home=$HOME
if [[ -n ${SUDO_USER:-} ]]; then user_home=$(getent passwd "$main_user" | cut -d: -f6); fi
state_dir=$user_home/.local/state/fleet-bootstrap
args=()
while (($#)); do
    case $1 in
        --plan) mode=plan; shift;;
        --apply) mode=apply; shift;;
        --resume) resume=1; shift;;
        --phases|--hub|--hostname|--tailscale-auth-key-file|--package-dir|--state-dir|--root-prefix|--token-file)
            [[ $# -ge 2 && $2 != *$'\n'* ]] || { echo 'Missing or multiline option value' >&2; exit 2; }
            case $1 in
                --phases) phases=$2;; --hub) hub=$2;; --hostname) hostname=$2;;
                --tailscale-auth-key-file) key=$2;; --package-dir) package=$2;;
                --state-dir) state_dir=$2;; --root-prefix) prefix=$2;;
                --token-file) args+=("$1" "$2");;
            esac
            shift 2;;
        --no-tailscale) no_tailscale=1; args+=("$1"); shift;;
        --no-container-runtime|--no-proxy-dropin|--reenroll) args+=("$1"); shift;;
        *) echo 'Unknown option' >&2; exit 2;;
    esac
done
[[ $state_dir == /* && ( -z $prefix || $prefix == /* ) ]] || { echo 'Use absolute state and root-prefix paths' >&2; exit 2; }
IFS=, read -r -a selected <<< "$phases"
for phase in "${selected[@]}"; do
    case $phase in preflight|tailscale|journald|disk-clean|ai-cli|agent|linger) ;; *) echo 'Unknown phase' >&2; exit 2;; esac
done
state=$state_dir/state.json
record() {
    [[ $mode == apply ]] || return 0
    mkdir -p "$state_dir"
    chmod 700 "$state_dir"
    python3 - "$state" "$phase" "$status" <<'PY'
import datetime,json,os,sys,tempfile
path,phase,status=sys.argv[1:]
data=json.load(open(path)) if os.path.exists(path) else {}
data[phase]={'status':status,'time':datetime.datetime.now(datetime.timezone.utc).isoformat().replace('+00:00','Z')}
fd,tmp=tempfile.mkstemp(dir=os.path.dirname(path))
with os.fdopen(fd,'w') as f: json.dump(data,f,indent=2); f.write('\n')
os.replace(tmp,path)
PY
    if [[ $(id -u) == 0 && -n ${SUDO_USER:-} ]]; then chown -R "$main_user" "$state_dir"; fi
}
root_step() {
    local command=$1
    if [[ $mode == plan ]]; then fleet_hint "$command"; return 0; fi
    if [[ $(id -u) != 0 ]]; then fleet_hint "$command"; status=needs-root; return 0; fi
    bash -c "$command" || return 1
}
validate_package() {
    [[ -d "$package" && ! -L "$package" && $(stat -c %a "$package") == 700 && $(stat -c %U "$package") == "$main_user" ]] || { echo 'Package directory must be user-owned and mode 0700' >&2; return 1; }
    [[ -f "$package/install-agent.sh" && ! -L "$package/install-agent.sh" ]] || { echo 'Missing staged installer' >&2; return 1; }
    local i
    token=$package/enroll-token
    for ((i=0;i<${#args[@]};i++)); do
        if [[ ${args[i]} == --token-file ]]; then token=${args[i+1]}; fi
    done
    [[ -f "$token" && ! -L "$token" && $(stat -c %a "$token") == 600 && $(stat -c %U "$token") == "$main_user" ]] || { echo 'Token file must be user-owned and mode 0600' >&2; return 1; }
}
run_phase() {
    local command file script flag uid
    flag=--$mode
    case $phase in
        preflight)
            if [[ -r $prefix/etc/os-release ]]; then awk -F= '/^(ID|VERSION_ID)=/ {print}' "$prefix/etc/os-release"; fi
            printf 'arch: %s\nuser: %s\n' "$(uname -m)" "$main_user"
            if command -v sudo >/dev/null && sudo -n true >/dev/null 2>&1; then echo 'sudo: available'; else echo 'sudo: unavailable without prompt'; fi
            df -h / | tail -1
            timedatectl show -p NTPSynchronized 2>/dev/null || true
            if [[ -n $hub ]]; then
                python3 - "$hub" <<'PY'
import sys,urllib.parse,subprocess
u=urllib.parse.urlsplit(sys.argv[1])
if u.scheme not in ('http','https') or not u.hostname or u.username or u.query or u.fragment: sys.exit('Invalid Hub URL')
r=subprocess.run(['getent','ahosts',u.hostname],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
print('hub DNS: '+('ok' if r.returncode==0 else 'failed'))
PY
                [[ $? == 0 ]] || return 1
                bash "$ROOT/ops/fleet/net/proxy-check.sh" --hub "$hub" --root-prefix "$prefix" || return 1
            fi;;
        journald)
            file=$prefix/etc/systemd/journald.conf.d/50-disk-clean.conf
            printf -v command 'source %q; fleet_write %q %q' "$ROOT/ops/fleet/common.sh" "$file" $'[Journal]\nSystemMaxUse=500M'
            root_step "$command";;
        tailscale)
            if [[ $no_tailscale == 1 ]]; then echo 'skipped: --no-tailscale'; status=skipped; return; fi
            if [[ -z $key ]]; then echo 'skipped: no Tailscale auth key file'; status=skipped; return; fi
            [[ -f $key && ! -L $key && $(stat -c %a "$key") == 600 && $(stat -c %u "$key") == 0 && -n $hostname ]] || { echo 'Tailscale requires hostname and root-owned 0600 key file' >&2; return 1; }
            printf -v command 'set -euo pipefail; source %q; if ! command -v tailscale >/dev/null; then installer=$(mktemp); trap '\''rm -f "$installer"'\'' EXIT; curl -fsSL https://tailscale.com/install.sh -o "$installer"; bash "$installer"; fi; systemctl enable --now tailscaled; tailscale up --auth-key=%q --hostname=%q --ssh=false 2>&1 | fleet_redact %q; shred -u -- %q; tailscale ip -4' "$ROOT/ops/fleet/common.sh" "file:$key" "$hostname" "$key" "$key"
            root_step "$command";;
        disk-clean|ai-cli)
            if [[ $phase == disk-clean ]]; then script=${FLEET_TIMERS_INSTALLER:-$ROOT/ops/maintenance/install-timers.sh}; else script=${FLEET_AI_CLI_INSTALLER:-$ROOT/ops/fleet/ai-cli/install-ai-clis.sh}; fi
            if [[ ! -f $script ]]; then printf 'skipped: %s not in this checkout\n' "${script##*/}"; status=skipped; return; fi
            if [[ $phase == disk-clean ]]; then
                if [[ $(id -u) == 0 && -n ${SUDO_USER:-} ]]; then runuser -u "$main_user" -- bash "$script" --scope user "$flag" || return 1; else bash "$script" --scope user "$flag" || return 1; fi
                printf -v command 'bash %q --scope root %q' "$script" "$flag"
                if [[ $mode == plan ]]; then bash "$script" --scope root "$flag" || return 1
                else root_step "$command"; fi
            # The AI CLI installer runs as the main user and prints its own
            # single-line root hints; never run it wholesale as root.
            elif [[ $(id -u) == 0 && -n ${SUDO_USER:-} ]]; then runuser -u "$main_user" -- bash "$script" "$flag" || return 1
            else bash "$script" "$flag" || return 1; fi;;
        agent)
            if [[ -z $package ]]; then echo 'skipped: no staged package directory'; status=skipped; return; fi
            validate_package || return 1
            uid=$(id -u "$main_user")
            [[ $uid != 0 ]] || { echo 'Enrollment requires a non-root main user (invoke with sudo from that user)' >&2; return 1; }
            # install-agent.sh runs as the main user and does its own sudo
            # preflight; its output is shown (it never prints the token).
            local installer=(./install-agent.sh)
            if [[ -n $hub ]]; then installer+=(--hub "$hub"); fi
            installer+=("${args[@]}")
            if [[ $mode == plan ]]; then
                printf 'plan user: cd %q && %s\n' "$package" "$(printf '%q ' "${installer[@]}")"
                return 0
            fi
            if [[ $(id -u) == 0 ]]; then
                (cd "$package" && runuser -u "$main_user" -- "${installer[@]}") 2>&1 | fleet_redact "$token" || return 1
            else
                (cd "$package" && "${installer[@]}") 2>&1 | fleet_redact "$token" || return 1
            fi
            systemctl is-active --quiet clawctl-agent || { echo 'clawctl-agent is not active after install' >&2; return 1; }
            echo 'Remove the staged package and transfer copies after Hub verification.';;
        linger)
            printf -v command 'loginctl enable-linger %q' "$main_user"
            root_step "$command";;
    esac
}
failed=0
for phase in "${selected[@]}"; do
    if [[ $resume == 1 && -f $state ]] && python3 - "$state" "$phase" <<'PY'
import json,sys
sys.exit(0 if json.load(open(sys.argv[1])).get(sys.argv[2],{}).get('status')=='done' else 1)
PY
    then printf 'resume: %s done\n' "$phase"; continue; fi
    printf '%s: %s\n' "$mode" "$phase"
    status="done"
    if run_phase; then :
    else status=failed; failed=1; printf 'failed: %s\n' "$phase" >&2; fi
    record
done
exit "$failed"
