#!/usr/bin/env bash
set -euo pipefail
DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=ops/fleet/common.sh
source "$DIR/../common.sh"
json=0 apply=0 server=time.cloudflare.com site='' prefix=''
while (($#)); do
    case $1 in
        --json) json=1; shift;; --apply) apply=1; shift;;
        --server) server=${2:?}; shift 2;; --site-ntp) site=${2:?}; shift 2;;
        --root-prefix) prefix=${2-}; shift 2;;
        *) echo 'Unknown option' >&2; exit 2;;
    esac
done
[[ $server =~ ^[a-zA-Z0-9][a-zA-Z0-9.:-]*$ && ( -z $site || $site =~ ^[a-zA-Z0-9][a-zA-Z0-9.:-]*$ ) ]] || { echo 'Invalid NTP host' >&2; exit 2; }
daemon=unknown
for service in chrony chronyd; do
    if systemctl is-active --quiet "$service"; then daemon=$service; break; fi
done
if [[ $daemon == unknown ]] && systemctl is-active --quiet systemd-timesyncd; then daemon=systemd-timesyncd; fi
sync=$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)
sources='' tracking='' reachable=0
if [[ $daemon == chrony || $daemon == chronyd ]]; then
    sources=$(chronyc -n sources 2>/dev/null || true)
    tracking=$(chronyc tracking 2>/dev/null || true)
    reachable=$(awk '$1 ~ /^[\^=#][*+?x~-]$/ && $5 ~ /^[0-7]+$/ && $5 != "0" {n++} END {print n+0}' <<< "$sources")
    file=$prefix/etc/chrony/sources.d/10-local.sources
    content="server $server iburst nts prefer"
    [[ -z $site ]] || content+=$'\n'"server $site iburst"
else
    file=$prefix/etc/systemd/timesyncd.conf.d/10-local.conf
    content=$'[Time]\n'"NTP=$server${site:+ $site}"
fi
# printf writes the file without restarting journald or unrelated services.
printf -v command 'set -euo pipefail; source %q; file=%q; content=%q; changed=0; if [[ ! -f "$file" ]] || [[ $(cat "$file") != "$content" ]]; then mkdir -p "${file%%/*}"; fleet_backup "$file"; printf "%%s\\n" "$content" > "$file"; chmod 644 "$file"; changed=1; fi' "$DIR/../common.sh" "$file" "$content"
if [[ $daemon == chrony || $daemon == chronyd ]]; then
    printf -v extra '; if fleet_chrony_include %q; then changed=1; else [[ $? == 1 ]] || exit 1; fi; if [[ $changed == 1 ]]; then systemctl restart %q; fi; chronyc -n sources; timedatectl show -p NTPSynchronized' "$prefix" "$daemon"
    command+=$extra
elif [[ $daemon == systemd-timesyncd ]]; then
    command+='; if [[ $changed == 1 ]]; then systemctl restart systemd-timesyncd; fi; timedatectl show -p NTPSynchronized'
fi
hint=$(fleet_hint "$command")
if [[ $daemon == unknown ]]; then hint='Install and enable chrony or systemd-timesyncd first.'; fi
if [[ $apply == 1 ]]; then
    [[ $daemon != unknown ]] || { echo "$hint" >&2; exit 1; }
    if [[ $(id -u) == 0 ]]; then bash -c "$command" >/dev/null; fi
fi
python3 - "$json" "$daemon" "$sync" "$reachable" "$tracking" "$hint" <<'PY'
import json,re,sys
j,daemon,sync,reachable,tracking,hint=sys.argv[1:]
m=re.search(r'System time\s*:\s*(.*)',tracking)
r=dict(daemon=daemon,synchronized=sync=='yes',reachable_sources=int(reachable),offset=m[1] if m else None,
       verdict='drift' if sync!='yes' or (daemon in ('chrony','chronyd') and int(reachable)==0) else 'ok',fix_hint=hint)
if j=='1': print(json.dumps(r))
else:
    for k,v in r.items():
        if k!='fix_hint': print('%s: %s' % (k,v))
    if r['verdict']!='ok': print(hint)
PY
