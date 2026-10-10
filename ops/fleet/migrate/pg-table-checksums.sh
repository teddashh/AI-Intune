#!/usr/bin/env bash
set -euo pipefail
cmd=${1:-}; shift || true
if [[ $cmd == compare ]]; then
 exec python3 - "$@" <<'PY'
import sys

def load(p):
 d={}
 for line in open(p):
  k,n,h=line.rstrip('\n').split('\t'); assert k not in d; d[k]=(int(n),h)
 return d
a,b=map(load,sys.argv[1:3]); bad=False
for k in sorted(a.keys()|b.keys()):
 if a.get(k)!=b.get(k): print('different:',k); bad=True
print('totals:',sum(n for n,h in a.values()),sum(n for n,h in b.values()))
sys.exit(int(bad))
PY
fi
[[ $cmd == snapshot ]] || { echo 'snapshot --db NAME [--psql CMD] [--out FILE] [--exclude REGEX] [--max-rows N]; compare A B'; exit 2; }
db=''; psql_cmd=psql; out=/dev/stdout; exclude='^$'; max=1000000
while (($#)); do
 case $1 in --db) db=$2;; --psql) psql_cmd=$2;; --out) out=$2;; --exclude) exclude=$2;; --max-rows) max=$2;; *) exit 2;; esac
 shift 2
done
[[ -n $db && $max =~ ^[0-9]+$ ]] || exit 2
# --psql may carry a prefix such as "sudo -u postgres psql" (split on spaces, never eval'd).
read -r -a psql <<< "$psql_cmd"
[[ ${#psql[@]} -gt 0 ]] || exit 2
# One repeatable-read transaction keeps all table measurements consistent.
sql=$("${psql[@]}" -X -qAt -d "$db" -c "SELECT format('%I.%I',schemaname,tablename) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema') ORDER BY 1;" 2>/dev/null) || { echo 'table inventory failed' >&2; exit 1; }
script='BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;'
while IFS= read -r table; do
 [[ -n $table && ! $table =~ $exclude ]] || continue
 # Names come from PostgreSQL format(%I), not shell interpolation of user input.
 literal=${table//\'/\'\'}
 script+=" SELECT '$literal', (SELECT count(*) FROM $table), CASE WHEN (SELECT count(*) FROM $table) > $max THEN 'count-only' ELSE (SELECT md5(COALESCE(string_agg(row_text, E'\\n' ORDER BY row_text COLLATE \"C\"),'')) FROM (SELECT t::text AS row_text FROM $table t) s) END;"
done <<< "$sql"
script+=' COMMIT;'
umask 077
"${psql[@]}" -X -qAt -F $'\t' -d "$db" -c "$script" > "$out" 2>/dev/null || { echo 'snapshot failed' >&2; exit 1; }
