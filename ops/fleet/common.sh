#!/usr/bin/env bash
# Shared quoting and idempotent configuration writes.
set -euo pipefail
fleet_hint() {
    local command=$1
    command=${command//\'/\'\\\'\'}
    printf "sudo bash -c '%s'\n" "$command"
}
fleet_quote() { printf '%q' "$1"; }
fleet_backup() {
    if [[ -e "$1" ]]; then cp -p -- "$1" "$1.bak-$(date -u +%Y%m%dT%H%M%S%NZ)"; fi
}
fleet_write() {
    local file=$1 content=$2
    if [[ -f "$file" ]] && [[ $(cat -- "$file") == "$content" ]]; then return; fi
    mkdir -p -- "$(dirname -- "$file")"
    fleet_backup "$file"
    printf '%s\n' "$content" > "$file"
    chmod 644 "$file"
}
# Oracle Linux may not load /etc/chrony/sources.d by default.
fleet_chrony_include() {
    local prefix=$1 config
    for config in "$prefix/etc/chrony/chrony.conf" "$prefix/etc/chrony.conf"; do
        [[ -f "$config" ]] || continue
        if ! awk '$1 == "sourcedir" && $2 == "/etc/chrony/sources.d" {found=1} END {exit !found}' "$config"; then
            fleet_backup "$config" || return 2
            printf '\nsourcedir /etc/chrony/sources.d\n' >> "$config" || return 2
            return 0
        fi
        return 1
    done
    return 1
}
# fleet_redact FILE...: copy stdin to stdout, replacing the (trimmed) contents
# of each secret file with [redacted]. Defense in depth for delegated tools.
fleet_redact() {
    python3 -c '
import sys
secrets = []
for path in sys.argv[1:]:
    try:
        value = open(path).read().strip()
    except OSError:
        continue
    if len(value) >= 8:
        secrets.append(value)
for line in sys.stdin:
    for value in secrets:
        line = line.replace(value, "[redacted]")
    sys.stdout.write(line)
    sys.stdout.flush()
' "$@"
}
