#!/usr/bin/env bash
set -euo pipefail
cmd=${1:-}; shift || true
if [[ $cmd == compare ]]; then
 a=$1; b=$2; shift 2; prefix=''
 if (($#)); then [[ $1 == --only-prefix ]] || exit 2; prefix=$2; fi
 exec python3 - "$a" "$b" "$prefix" <<'PY'
import sys

def load(p):
 d={}
 for l in open(p):
  k,s,url=l.rstrip('\n').split('\t'); assert k not in d; d[k]=(s,url)
 return d
a,b=map(load,sys.argv[1:3]); bad=False
for k in sorted(a.keys()|b.keys()):
 if a.get(k)!=b.get(k):
  required=k.startswith(sys.argv[3]); print(('different: ' if required else 'warning: ')+k); bad |= required
sys.exit(int(bad))
PY
fi
if [[ $cmd != snapshot ]]; then
 echo 'snapshot --base https://site --paths FILE [--resolve host:443:ip] [--out FILE]; compare A B [--only-prefix /x]'
 echo 'SNI caveat: transparent egress proxies may ignore --resolve. This cannot be detected reliably; use an independent host that honours it.'
 [[ $cmd != --help ]] || exit 0
 exit 2
fi
base=''; paths=''; out=/dev/stdout; resolve=()
while (($#)); do
 case $1 in --base) base=$2;; --paths) paths=$2;; --out) out=$2;; --resolve) resolve=(--resolve "$2");; *) exit 2;; esac
 shift 2
done
[[ ( $base == https://* || $base == http://* ) && -f $paths ]] || exit 2
umask 077
while IFS= read -r path || [[ -n $path ]]; do
 [[ -n $path && $path != \#* ]] || continue
 [[ $path == /* && $path != *$'\t'* ]] || exit 2
 # A failed request is recorded as status 000 so one dead route does not hide the rest.
 result=$(curl -sS --max-time 20 -o /dev/null -w '%{http_code}\t%{redirect_url}' "${resolve[@]}" "${base%/}$path" 2>/dev/null) || result=$'000\t'
 printf '%s\t%s\n' "$path" "$result"
done < "$paths" > "$out"
