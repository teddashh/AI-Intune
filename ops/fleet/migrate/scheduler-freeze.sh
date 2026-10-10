#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == --help ]]; then echo 'list|freeze|thaw|verify-off --pattern REGEX [--user] [--state FILE] [--apply] [--now]'; exit 0; fi
command=${1:-list}; shift || true
pattern=''; state=''; apply=false; now=false; scope=()
while (($#)); do
 case $1 in
 --pattern) pattern=$2; shift 2;; --state) state=$2; shift 2;;
 --user) scope=(--user); shift;; --apply) apply=true; shift;; --now) now=true; shift;;
 --help) echo 'list|freeze|thaw|verify-off --pattern REGEX [--user] [--state FILE] [--apply] [--now]'; exit 0;;
 *) exit 2;; esac
done
[[ -n $pattern ]] || exit 2
[[ $command =~ ^(list|freeze|thaw|verify-off)$ ]] || exit 2
ctl() { systemctl "${scope[@]}" "$@"; }
if [[ $command == thaw || ( $command == freeze && -n $state && -f $state ) ]]; then
 [[ -f $state ]] || exit 2
 rows=$(python3 - "$state" "${scope[*]}" <<'PY'
import json,re,sys
s=json.load(open(sys.argv[1]))
assert s['scope']==sys.argv[2]
for r in s['timers']:
 assert re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_@.-]*\.timer',r['unit'])
 print(r['unit'],r['enabled'],r['active'],r['masked'],sep='\t')
PY
 )
else
 rows=''
 units=$(ctl list-unit-files --type=timer --no-legend --no-pager)
 while read -r unit _; do
  [[ $unit == *.timer && $unit =~ $pattern ]] || continue
  enabled=$(ctl is-enabled "$unit" 2>/dev/null || true)
  active=$(ctl is-active "$unit" 2>/dev/null || true)
  [[ -n $enabled && -n $active && $active != unknown ]] || { echo 'timer state unavailable' >&2; exit 1; }
  masked=false; [[ $enabled != masked* ]] || masked=true
  rows+="$unit"$'\t'"$enabled"$'\t'"$active"$'\t'"$masked"$'\n'
 done <<< "$units"
fi
if [[ $command == list ]]; then
 printf 'unit\tenabled\tactive\tmasked\n%s\n' "$rows"
 while IFS=$'\t' read -r unit _; do
  [[ -z $unit ]] || ctl show "$unit" --property=NextElapseUSecRealtime
 done <<< "$rows"
 exit 0
fi
if [[ $command == verify-off ]]; then
 bad=0
 while IFS=$'\t' read -r unit enabled active _; do
  [[ -n $unit ]] || continue
  service=$(ctl show "$unit" --property=Unit --value)
  [[ -n $service ]] || service=${unit%.timer}.service
  if [[ $enabled == enabled* || $active == active ]] || ctl is-active --quiet "$service"; then
   printf 'running or enabled: %s\n' "$unit"; bad=1
  fi
 done <<< "$rows"
 services=$(ctl list-unit-files --type=service --no-legend --no-pager)
 while read -r unit _; do
  [[ $unit == *.service && $unit =~ $pattern ]] || continue
  if ctl is-active --quiet "$unit"; then printf 'running service: %s\n' "$unit"; bad=1; fi
 done <<< "$services"
 exit "$bad"
fi
if ! $apply; then printf 'plan %s:\n%s\n' "$command" "$rows"; exit 0; fi
if ((${#scope[@]} == 0)) && [[ $(id -u) != 0 ]]; then
 printf -v cmd '%q ' "$0" "$command" --pattern "$pattern" --state "$state" --apply
 $now && cmd+='--now'
 printf "sudo bash -c '%s'\n" "${cmd//\'/\'\\\'\'}"
 exit 2
fi
if [[ $command == freeze && ! -f $state ]]; then
 [[ -n $state && ! -e $state ]] || { echo 'state must be a regular file; preserve original freeze state'; exit 2; }
 umask 077
 printf '%s' "$rows" | python3 -c 'import sys,json; json.dump({"scope":sys.argv[1],"timers":[dict(zip(["unit","enabled","active","masked"],l.split("\t"))) for l in sys.stdin.read().splitlines() if l]},open(sys.argv[2],"x"))' "${scope[*]}" "$state"
fi
while IFS=$'\t' read -r unit enabled active masked; do
 [[ -n $unit ]] || continue
 [[ $unit =~ $pattern ]] || { echo 'state pattern mismatch'; exit 2; }
 if [[ $command == freeze ]]; then
  ctl stop "$unit"; ctl disable "$unit"; ctl mask --runtime "$unit"
 else
  ctl stop "$unit"
  ctl unmask --runtime "$unit"
  ctl disable "$unit"
  if [[ $enabled == masked-runtime ]]; then ctl mask --runtime "$unit"; fi
  if [[ $enabled == enabled ]]; then ctl enable "$unit"; elif [[ $enabled == enabled-runtime ]]; then ctl enable --runtime "$unit"; fi
  if $now && [[ $active == active ]]; then ctl start "$unit"; fi
 fi
done <<< "$rows"
