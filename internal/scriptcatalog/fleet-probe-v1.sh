#!/bin/sh
set -eu
# Fixed facts only; never print host identity or systemd unit names.
LC_ALL=C
export LC_ALL
uptime=$(awk '{printf "%.0f", $1}' /proc/uptime)
load=$(awk '{printf "[%s,%s,%s]", $1,$2,$3}' /proc/loadavg)
disk=$(df -P / | awk 'NR==2 {gsub(/%/,"",$5); print $5}')
failed=null
if command -v systemctl >/dev/null 2>&1; then
  if units=$(systemctl --failed --no-legend --plain --no-pager 2>/dev/null); then
    failed=$(printf '%s\n' "$units" | awk 'NF {n++} END {print n+0}')
  fi
fi
printf '{"uptime_seconds":%s,"load":%s,"root_disk_use_percent":%s,"failed_systemd_unit_count":%s}\n' "$uptime" "$load" "$disk" "$failed"
