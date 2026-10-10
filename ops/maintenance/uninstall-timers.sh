#!/usr/bin/env bash
set -euo pipefail
BASE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCOPE='' PREFIX='' APPLY=0 PURGE=0
ARGS=("$@")
usage() { printf '%s\n' 'usage: uninstall-timers.sh --scope user|root [--plan|--apply] [--purge] [--prefix DIR]'; }
while [ "$#" -gt 0 ]; do
    case "$1" in
        --scope|--prefix)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            case "$1" in --scope) SCOPE=$2 ;; --prefix) PREFIX=$2 ;; esac; shift ;;
        --apply) APPLY=1 ;; --plan) APPLY=0 ;; --purge) PURGE=1 ;;
        -h|--help) usage; exit 0 ;; *) usage >&2; exit 2 ;;
    esac
    shift
done
[[ "$SCOPE" == user || "$SCOPE" == root ]] || { usage >&2; exit 2; }
[[ -z "$PREFIX" || "$PREFIX" == /* ]] || { usage >&2; exit 2; }
if [ "$SCOPE" = root ]; then
    UNITS=$PREFIX/etc/systemd/system
    FILES=("$PREFIX/usr/local/sbin/disk-clean")
    CONFIGS=("$PREFIX/etc/disk-clean/root.conf" "$PREFIX/etc/tmpfiles.d/00-disk-clean.conf" "$PREFIX/etc/systemd/journald.conf.d/50-disk-clean.conf")
    NAMES=(disk-clean-root); CTL=(systemctl)
else
    UNITS=$PREFIX$HOME/.config/systemd/user
    FILES=("$PREFIX$HOME/.local/bin/disk-clean" "$PREFIX$HOME/.local/bin/fleet-daily-check")
    CONFIGS=("$PREFIX$HOME/.config/disk-clean/config")
    NAMES=(disk-clean disk-clean-weekly fleet-daily-check); CTL=(systemctl --user)
fi
printf 'Remove %s timers; purge config: %s; retain summaries and linger\n' "$SCOPE" "$PURGE"
[ "$APPLY" = 1 ] || { printf 'Plan only; pass --apply to uninstall\n'; exit 0; }
if [ "$SCOPE" = root ] && [ "$(id -u)" -ne 0 ]; then
    printf -v command '%q ' bash "$BASE/uninstall-timers.sh" "${ARGS[@]}" --apply
    quoted=${command//\'/\'\\\'\'}
    printf "sudo bash -c '%s'\n" "$quoted"
    exit 1
fi
for name in "${NAMES[@]}"; do
    if [ -f "$UNITS/$name.timer" ] && [ -z "$PREFIX" ]; then "${CTL[@]}" disable --now "$name.timer"; fi
    FILES+=("$UNITS/$name.service" "$UNITS/$name.timer")
done
[ "$PURGE" = 0 ] || FILES+=("${CONFIGS[@]}")
for file in "${FILES[@]}"; do
    [ ! -e "$file" ] || { cp -p -- "$file" "$file.bak-$(date -u +%Y%m%dT%H%M%S%NZ)"; rm -- "$file"; }
done
[ -n "$PREFIX" ] || "${CTL[@]}" daemon-reload
