#!/usr/bin/env bash
set -euo pipefail
[ "$#" -eq 1 ] && [ -d "$1" ] || { printf 'usage: fleet-report.sh DIR\n' >&2; exit 2; }
exec python3 - "$1" <<'PY'
import json, pathlib, sys
print('| host | claude | codex | grok | agy | verdict |')
print('| --- | --- | --- | --- | --- | --- |')
for path in sorted(pathlib.Path(sys.argv[1]).glob('*.out')):
    try:
        d=json.loads(path.read_text())
        cells=[]
        for name in ('claude','codex','grok','agy'):
            v=d['clis'][name]
            state='missing' if not v['present'] else 'ready' if v['login_state'] in ('ready','present') and v['resolves_noninteractive'] else 'drift'
            ver=str(v.get('version') or '').replace('|','')
            cells.append(state if state=='missing' or not ver or ver=='unknown' else ver+' '+state)
        verdict=d['verdict'] if d['verdict'] in ('ok','drift') else 'error'
    except (ValueError, KeyError, TypeError, OSError): cells=['unknown']*4; verdict='error'
    host=path.stem.replace('|',r'\|').replace('\n',' ')
    print('| '+' | '.join([host]+cells+[verdict])+' |')
PY
