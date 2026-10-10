#!/usr/bin/env bash
set -euo pipefail
name=''; pattern=''; json=false
while (($#)); do case $1 in --name) name=$2; shift 2;; --pattern) pattern=$2; shift 2;; --json) json=true; shift;; *) exit 2;; esac; done
[[ $name =~ ^[a-zA-Z0-9_-]+$ ]] || exit 2
pattern=${pattern:-$name}
tmp=$(mktemp); trap 'rm -f "$tmp"' EXIT
record() {
 local label=$1 safe=$2; shift 2
 local data='' status=ok rc=0
 if command -v "$1" >/dev/null; then data=$("$@" 2>/dev/null) || rc=$?; else rc=127; fi
 # rg/grep return 1 for no match (2 when some files were unreadable), crontab 1 for no crontab, find 1 for unreadable subdirs.
 if ((rc != 0)) && ! [[ $rc == 1 && ( $1 == rg || $1 == grep || $1 == crontab || $1 == find ) ]] && ! [[ $rc == 2 && ( $1 == grep || $1 == rg ) ]]; then status=skipped; data=''; fi
 printf '%s' "$data" | python3 -c '
import sys,json,re
label,safe,status,pattern=sys.argv[1:]
# rg -l already matched file contents; its output is file names, not lines to filter.
raw=sys.stdin.read().splitlines()
if safe=="procs":
 # "pid ppid args": drop this inventory run itself (its own argv contains the pattern).
 rows=[r.split(None,2) for r in raw if len(r.split(None,2))==3]
 parent={a:b for a,b,_ in rows}
 me=__import__("os").environ.get("INVENTORY_PID","")
 def mine(pid):
  seen=set()
  while pid and pid not in seen:
   if pid==me: return True
   seen.add(pid); pid=parent.get(pid)
  return False
 ancestors=set(); pid=me
 while pid and pid not in ancestors: ancestors.add(pid); pid=parent.get(pid)
 raw=[c for p,_,c in rows if not mine(p) and p not in ancestors]
 safe="private"
lines=[s for s in raw if s and (safe=="files" or re.search(pattern,s))]
if safe=="units": lines=[s.split()[0] for s in lines if s.split()]
if safe=="ports":
 out=[]
 for s in lines:
  fields=s.split(); port=fields[4].rsplit(":",1)[-1] if len(fields)>4 else ""
  names=re.findall(r"\(\(\"([^\"]+)\"",s)
  out.append(port+" "+",".join(names))
 lines=out
print(json.dumps(dict(source=label,status=status,count=len(lines),names=lines[:50] if safe!="private" else [],more=max(0,len(lines)-50) if safe!="private" else 0)))
' "$label" "$safe" "$status" "$pattern" >> "$tmp"
}
record system-units units systemctl list-unit-files --no-pager --no-legend
record user-units units systemctl --user list-unit-files --no-pager --no-legend
record system-timers units systemctl list-unit-files --type=timer --no-pager --no-legend
record user-timers units systemctl --user list-unit-files --type=timer --no-pager --no-legend
INVENTORY_PID=$$ record processes procs ps -eo pid=,ppid=,args=
record ports ports ss -lntup
record cron private crontab -l
record dpkg names dpkg-query -W -f='${Package}\n'
record rpm names rpm -qa
record snap names snap list
record flatpak names flatpak list --columns=application
record npm names npm list -g --depth=0
record pip names python3 -m pip list
record docker names docker ps -a --format '{{.Names}} {{.Image}}'
record docker-images names docker images --format '{{.Repository}}:{{.Tag}}'
for file in "$HOME/.claude/settings.json" "$HOME/.codex/hooks.json" "$HOME/.gemini/settings.json" "$HOME"/.grok/hooks/* "$HOME/.bashrc" "$HOME/.profile"; do
 [[ ! -f $file ]] || record "$file" private cat "$file"
done
for dir in "$HOME/.config/systemd/user" /etc/systemd/system /etc/cron.d /etc/nginx /opt /usr/local/bin "$HOME/.local/bin"; do
 [[ ! -d $dir ]] || record "paths $dir" names find "$dir" -maxdepth 3 -print
done
for dir in "$HOME" /etc/nginx /etc/cron.d; do
 [[ -d $dir ]] || continue
 if ! command -v rg >/dev/null; then
  record "reference-files $dir" files grep -rIlE --exclude='*.bak-*' --exclude='backup-*' --exclude-dir=node_modules --exclude-dir=.cache --exclude-dir=.git --exclude-dir='*-removal-backup-*' -- "$pattern" "$dir"
  continue
 fi
 record "reference-files $dir" files rg -l --hidden --max-depth 6 --glob '!*.bak-*' --glob '!backup-*' --glob '!*-removal-backup-*' --glob '!**/node_modules/**' --glob '!**/.cache/**' --glob '!**/.git/**' -- "$pattern" "$dir"
done
record 'project data (not removed)' names find "$HOME" -maxdepth 6 -name .git -print
python3 - "$tmp" "$json" <<'PY'
import json,sys
sources=[json.loads(s) for s in open(sys.argv[1])]
if sys.argv[2]=='true': print(json.dumps({'sources':sources}))
else:
 for s in sources:
  print(f"{s['source']}: {s['count'] if s['status']=='ok' else 'skipped'}")
  for n in s['names']: print('  '+n)
  if s['more']: print(f"  ... {s['more']} more")
PY
