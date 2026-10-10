#!/usr/bin/env bash
set -euo pipefail
DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=ops/fleet/common.sh
source "$DIR/../common.sh"
hub='' prefix='' json=0
while (($#)); do
    case $1 in
        --hub) hub=${2:?}; shift 2;;
        --root-prefix) prefix=${2-}; shift 2;;
        --json) json=1; shift;;
        *) echo 'Unknown option' >&2; exit 2;;
    esac
done
file=$prefix/etc/systemd/system/clawctl-agent.service.d/10-proxy.conf
printf -v command 'set -euo pipefail; source %q; mkdir -p %q; fleet_backup %q; umask 077; content=$(python3 %q render %q); printf "%%s\\n" "$content" > %q; chmod 600 %q; systemctl daemon-reload; systemctl restart clawctl-agent' "$DIR/../common.sh" "${file%/*}" "$file" "$DIR/network.py" "$prefix" "$file" "$file"
hint=$(fleet_hint "$command")
report=$(python3 "$DIR/network.py" proxy "$prefix" "$hub")
python3 - "$report" "$hint" "$json" <<'PY'
import json,sys
r=json.loads(sys.argv[1]); r['fix_hint']=sys.argv[2]
if sys.argv[3]=='1': print(json.dumps(r))
else:
    for k,v in r.items():
        if k!='fix_hint': print('%s: %s' % (k,json.dumps(v)))
    if r['verdict']!='ok': print(r['fix_hint'])
PY
