#!/usr/bin/env bash
set -euo pipefail
BASE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCOPE='' CONF='' PREFIX='' APPLY=0 APPROVE=0 DAILY=0
ARGS=("$@")
usage() { printf '%s\n' 'usage: install-timers.sh --scope user|root [--conf FILE] [--plan|--apply] [--approve] [--prefix DIR] [--with-daily-check]'; }
while [ "$#" -gt 0 ]; do
    case "$1" in
        --scope|--conf|--prefix)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            case "$1" in --scope) SCOPE=$2 ;; --conf) CONF=$2 ;; --prefix) PREFIX=$2 ;; esac
            shift ;;
        --apply) APPLY=1 ;; --plan) APPLY=0 ;; --approve) APPROVE=1 ;; --with-daily-check) DAILY=1 ;;
        -h|--help) usage; exit 0 ;; *) usage >&2; exit 2 ;;
    esac
    shift
done
[[ "$SCOPE" == user || "$SCOPE" == root ]] || { usage >&2; exit 2; }
[[ -z "$PREFIX" || "$PREFIX" == /* ]] || { printf 'prefix must be absolute\n' >&2; exit 2; }
[ "$DAILY" = 0 ] || [ "$SCOPE" = user ] || { printf 'daily check needs user scope\n' >&2; exit 2; }
CONF=${CONF:-$BASE/conf/$SCOPE.conf.example}
[ -f "$CONF" ] || { printf 'config file missing\n' >&2; exit 2; }
# Quote a complete shell command inside one shell single-quoted string.
sudo_hint() {
    local command quoted
    printf -v command '%q ' bash "$BASE/install-timers.sh" "${ARGS[@]}" --apply
    quoted=${command//\'/\'\\\'\'}
    printf "sudo bash -c '%s'\n" "$quoted"
}
if [ "$SCOPE" = root ]; then
    BIN=$PREFIX/usr/local/sbin/disk-clean
    CONFIG=$PREFIX/etc/disk-clean/root.conf
    UNITS=$PREFIX/etc/systemd/system
    NAMES=(disk-clean-root)
    CTL=(systemctl)
else
    BIN=$PREFIX$HOME/.local/bin/disk-clean
    CONFIG=$PREFIX$HOME/.config/disk-clean/config
    UNITS=$PREFIX$HOME/.config/systemd/user
    NAMES=(disk-clean disk-clean-weekly)
    CTL=(systemctl --user)
    [ "$DAILY" = 0 ] || NAMES+=(fleet-daily-check)
fi
printf 'Install %s timers; config: %s; approve: %s\n' "$SCOPE" "$CONFIG" "$APPROVE"
if [ -f "$CONFIG" ]; then
    if cmp -s "$CONF" "$CONFIG"; then printf 'Existing config kept; template identical\n'; else printf 'Existing config kept; template differs (contents withheld)\n'; fi
fi
if [ "$APPLY" = 0 ]; then printf 'Plan only; pass --apply to install\n'; exit 0; fi
if [ "$SCOPE" = root ] && [ "$(id -u)" -ne 0 ]; then sudo_hint; exit 1; fi
backup() { cp -p -- "$1" "$1.bak-$(date -u +%Y%m%dT%H%M%S%NZ)"; }
copy() {
    local src=$1 dest=$2 mode=$3
    mkdir -p -- "$(dirname "$dest")"
    if [ -f "$dest" ] && cmp -s "$src" "$dest" && [ "$(stat -c %a "$dest")" = "${mode#0}" ]; then return; fi
    [ ! -e "$dest" ] || backup "$dest"
    install -m "$mode" -- "$src" "$dest"
}
copy "$BASE/disk-clean" "$BIN" 0755
[ "$(sha256sum "$BIN" | cut -d' ' -f1)" = "$(sha256sum "$BASE/disk-clean" | cut -d' ' -f1)" ] || { printf 'script checksum mismatch\n' >&2; exit 1; }
[ -e "$CONFIG" ] || copy "$CONF" "$CONFIG" 0600
if [ "$APPROVE" = 1 ]; then
    # Normalize every DRY_RUN assignment without executing config content.
    tmp=$(mktemp)
    trap 'rm -f "$tmp"' EXIT
    awk 'BEGIN {seen=0} /^[[:space:]]*DRY_RUN[[:space:]]*=/ {print "DRY_RUN=0"; seen=1; next} {print} END {if (!seen) print "DRY_RUN=0"}' "$CONFIG" > "$tmp"
    if ! cmp -s "$tmp" "$CONFIG"; then backup "$CONFIG"; install -m 0600 "$tmp" "$CONFIG"; fi
fi
for name in "${NAMES[@]}"; do
    src=$BASE/systemd/$([ "$SCOPE" = root ] && printf system || printf user)
    [ "$name" != fleet-daily-check ] || src=$BASE/../fleet/daily-check
    for suffix in service timer; do copy "$src/$name.$suffix" "$UNITS/$name.$suffix" 0644; done
done
if [ "$SCOPE" = root ]; then
    copy "$BASE/tmpfiles/00-disk-clean.conf" "$PREFIX/etc/tmpfiles.d/00-disk-clean.conf" 0644
    copy "$BASE/journald/50-disk-clean.conf" "$PREFIX/etc/systemd/journald.conf.d/50-disk-clean.conf" 0644
elif [ "$DAILY" = 1 ]; then
    copy "$BASE/../fleet/daily-check/daily-check.sh" "$PREFIX$HOME/.local/bin/fleet-daily-check" 0755
fi
# A prefix is an offline staging tree, never the running host's systemd instance.
if [ -z "$PREFIX" ]; then
    "${CTL[@]}" daemon-reload
    for name in "${NAMES[@]}"; do "${CTL[@]}" enable --now "$name.timer"; done
    if [ "$SCOPE" = user ] && [ "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null || true)" != yes ]; then
        if [ "$(id -u)" -eq 0 ]; then loginctl enable-linger "$(id -un)"; else
            printf -v command '%q ' loginctl enable-linger "$(id -un)"
            quoted=${command//\'/\'\\\'\'}
            printf "sudo bash -c '%s'\n" "$quoted"
        fi
    fi
fi
printf 'Installed; cleaner config controls dry-run approval\n'
