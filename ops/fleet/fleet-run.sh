#!/usr/bin/env bash
set -euo pipefail
hosts=()
out=
ssh_cmd=ssh
while [ "$#" -gt 0 ]; do
    case "$1" in
        --host|--hosts|--out|--ssh)
            [ "$#" -ge 2 ] || exit 2
            case "$1" in
                --host) hosts+=("$2") ;;
                --hosts)
                    while IFS= read -r line || [ -n "$line" ]; do
                        line=${line%%#*}
                        read -r -a entries <<< "$line"
                        [ "${#entries[@]}" -eq 0 ] || hosts+=("${entries[@]}")
                    done < "$2" ;;
                --out) out=$2 ;;
                --ssh) ssh_cmd=$2 ;;
            esac
            shift 2 ;;
        --) shift; break ;;
        *) exit 2 ;;
    esac
done
[ -n "$out" ] && [ "${#hosts[@]}" -gt 0 ] && [ "$#" -gt 0 ] && [ -f "$1" ] || { printf 'usage: fleet-run.sh --hosts FILE|--host H --out DIR [--ssh CMD] -- SCRIPT [args]\n' >&2; exit 2; }
script=$1; shift
for host in "${hosts[@]}"; do
    [[ $host =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] && [[ $host != *..* ]] || { printf 'invalid SSH alias\n' >&2; exit 2; }
done
# SSH joins argv into a remote shell command; quote each argument explicitly.
remote='bash -s --'
for arg in "$@"; do
    quoted=${arg//\'/\'\\\'\'}
    remote+=" '$quoted'"
done
mkdir -p "$out"
chmod 700 "$out"
printf 'HOST RC FIRST LINE\n'
failed=0
for host in "${hosts[@]}"; do
    rc=0
    (umask 077; timeout 180 "$ssh_cmd" -o BatchMode=yes -o ForwardAgent=no "$host" "$remote" < "$script" > "$out/$host.out" 2>&1) || rc=$?
    (umask 077; printf '%s\n' "$rc" > "$out/$host.rc")
    first=$(head -n 1 "$out/$host.out")
    printf '%s %s %s\n' "$host" "$rc" "$first"
    [ "$rc" -eq 0 ] || failed=1
done
exit "$failed"
