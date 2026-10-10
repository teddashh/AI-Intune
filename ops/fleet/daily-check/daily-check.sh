#!/usr/bin/env bash
set -euo pipefail
JSON=0 WRITE=0 SKIP_ROOT=0 STATE=${XDG_STATE_HOME:-$HOME/.local/state}/fleet-daily-check
while [ "$#" -gt 0 ]; do
    case "$1" in
        --json) JSON=1 ;; --write) WRITE=1 ;; --skip-root) SKIP_ROOT=1 ;;
        --state-dir) [ "$#" -ge 2 ] || exit 2; STATE=$2; shift ;;
        -h|--help) printf '%s\n' 'usage: daily-check.sh [--json] [--write] [--skip-root] [--state-dir DIR]'; exit 0 ;;
        *) printf 'unknown option\n' >&2; exit 2 ;;
    esac
    shift
done
# DAILY_CHECK_ROOT redirects only the fixed system evidence paths for offline tests.
exec python3 - "$JSON" "$WRITE" "$STATE" "$SKIP_ROOT" <<'PY'
import datetime as dt
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import tempfile
import time

json_output, write, state, skip_root = sys.argv[1:]
skip_root = skip_root == '1'  # host without root-scope timers (no sudo)
root = os.environ.get('DAILY_CHECK_ROOT', '')
now = time.time()
reasons = []
severity = 0

def flag(reason, level=1):
    global severity
    reasons.append(reason)
    severity = max(severity, level)

def system_path(path):
    return Path(root + path)

def command(args):
    try:
        p = subprocess.run(args, capture_output=True, text=True, timeout=15,
                           env={**os.environ, 'LC_ALL': 'C'})
        return p.stdout if p.returncode == 0 else None
    except (OSError, subprocess.SubprocessError, UnicodeError):
        return None

summaries = {}
scopes = [('root', system_path('/var/lib/disk-clean/last.json')),
          ('user', Path.home() / '.local/state/disk-clean/last.json')]
for scope, path in scopes[1:] if skip_root else scopes:
    try:
        data = json.loads(path.read_text())
        if data.get('schema') != 'fleet-disk-clean/v1' or data.get('scope') != scope:
            raise ValueError()
        age = now - dt.datetime.fromisoformat(data['ts'].replace('Z', '+00:00')).timestamp()
        mode = data['mode']
        freed = data['freed_bytes']
        if mode not in ('dry-run', 'mixed', 'apply') or type(freed) is not int or freed < 0:
            raise ValueError()
        # Never echo untrusted notes, attention text, host names, or unknown fields.
        attention = bool(data.get('attention'))
        summaries[scope] = dict(age_hours=round(age / 3600, 2), mode=mode,
                                freed_bytes=freed, attention=attention)
        if age > 48 * 3600:
            flag(scope + ' summary stale')
        if age < -300:
            flag(scope + ' summary timestamp in future')
        if attention:
            flag(scope + ' summary needs attention')
    except (OSError, UnicodeError, ValueError, KeyError, TypeError, AttributeError, OverflowError):
        summaries[scope] = None
        flag(scope + ' summary missing or invalid')

timers = {}
for scope, names in [('system', [] if skip_root else ['disk-clean-root']), ('user', ['disk-clean', 'disk-clean-weekly'])]:
    ctl = ['systemctl'] + (['--user'] if scope == 'user' else [])
    failed = command(ctl + ['--failed', '--no-legend', '--plain', '--no-pager'])
    if failed is None:
        flag(scope + ' failed units unavailable')
    elif failed.strip():
        flag(scope + ' failed units present')
    for name in names:
        timer = command(ctl + ['show', name + '.timer', '-p', 'LastTriggerUSec', '-p', 'ActiveState'])
        service = command(ctl + ['show', name + '.service', '-p', 'Result'])
        def properties(text):
            return dict(line.split('=', 1) for line in (text or '').splitlines() if '=' in line)
        t, s = properties(timer), properties(service)
        last = t.get('LastTriggerUSec', '')
        # Reduce command output to known values; do not publish arbitrary diagnostics.
        active = t.get('ActiveState') == 'active'
        success = s.get('Result') == 'success'
        triggered = last not in ('', 'n/a', '0')
        trigger_ts = None
        if triggered:
            parsed = command(['date', '-u', '-d', last, '+%s'])
            if parsed and parsed.strip().isdigit():
                trigger_ts = int(parsed.strip())
            else:
                triggered = False
        timers[name] = dict(active=active, last_trigger_epoch=trigger_ts, success=success,
                            first_run_pending=active and not triggered)
        # An active timer that has not fired yet (e.g. a weekly timer installed
        # mid-week) is waiting, not broken; summary age still catches a dead schedule.
        if not active or not success:
            flag(name + ' timer inactive or unsuccessful')
        elif not triggered:
            pass
        elif now - trigger_ts > (8 * 86400 if name.endswith('weekly') else 48 * 3600):
            flag(name + ' timer trigger stale')

disks = {}
for mount in ['/', '/home']:
    output = command(['df', '-P', mount])
    try:
        pct = int(output.splitlines()[-1].split()[4].rstrip('%'))
        if not 0 <= pct <= 100:
            raise ValueError()
        disks[mount] = pct
        if pct >= 85:
            flag(mount + ' disk at ' + str(pct) + '%', 2 if pct >= 95 else 1)
    except (AttributeError, ValueError, IndexError):
        disks[mount] = None
        flag(mount + ' disk usage unavailable')

journal = command(['journalctl', '--disk-usage'])
journal_bytes = None
match = re.search(r'take up ([0-9.]+)([BKMGT])', journal or '')
if match:
    journal_bytes = int(float(match[1]) * 1024 ** 'BKMGT'.index(match[2]))
else:
    flag('journal size unavailable')
# journald reads journald.conf then journald.conf.d/*.conf. A SystemMaxUse line
# outside a [Journal] section is silently ignored, so flag it explicitly.
cap_in_journal = False
headerless = []
journald_files = [system_path('/etc/systemd/journald.conf')]
journald_files += sorted(system_path('/etc/systemd/journald.conf.d').glob('*.conf'))
for conf in journald_files:
    try:
        lines = conf.read_text().splitlines()
    except (OSError, UnicodeError):
        continue
    section = None
    for line in lines:
        line = line.strip()
        if line.startswith('[') and line.endswith(']'):
            section = line
        elif re.match(r'SystemMaxUse\s*=\s*\S+', line):
            if section == '[Journal]':
                cap_in_journal = True
            else:
                headerless.append(conf.name)
if headerless:
    flag('journald SystemMaxUse ignored, no Journal header: ' + ', '.join(sorted(set(headerless))))
if not cap_in_journal:
    flag('journald size cap not configured')
clock = command(['timedatectl', 'show', '-p', 'NTPSynchronized'])
synced = (clock or '').strip() == 'NTPSynchronized=yes'
if not synced:
    flag('clock not synchronized or unavailable')
reboot = system_path('/var/run/reboot-required').exists()
if reboot:
    flag('reboot required')

verdict = ['ok', 'warn', 'crit'][severity]
ts = dt.datetime.fromtimestamp(now, dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
host = socket.gethostname()
summary = dict(schema='fleet-daily-check/v1', ts=ts, host=host, verdict=verdict,
               reasons=reasons, disk_clean=summaries, timers=timers, disk_use_percent=disks,
               journal_bytes=journal_bytes, clock_synchronized=synced, reboot_required=reboot)
encoded = json.dumps(summary, separators=(',', ':')) + '\n'
line = f'{ts} {host} {verdict} ' + ('; '.join(reasons) or 'all checks passed') + '\n'
if write == '1':
    directory = Path(state)
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    # Each file is replaced atomically; readers can compare ts across the pair.
    for name, content in [('summary.json', encoded), ('summary.txt', line)]:
        fd, temporary = tempfile.mkstemp(prefix='.' + name + '-', dir=directory)
        try:
            with os.fdopen(fd, 'w') as f:
                f.write(content)
                f.flush()
                os.fsync(f.fileno())
            os.replace(temporary, directory / name)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
print(encoded if json_output == '1' else line, end='')
sys.exit(severity)
PY
