#!/usr/bin/env python3
"""Parse network configuration without evaluating shell or disclosing credentials."""
import glob
import json
import os
import re
import shlex
import subprocess
import sys
import urllib.parse

KEYS = ('HTTPS_PROXY', 'https_proxy', 'HTTP_PROXY', 'http_proxy', 'NO_PROXY', 'no_proxy')


def environment(prefix):
    values = {}
    try:
        for line in open(prefix + '/etc/environment'):
            match = re.match(r'^\s*(?:export\s+)?(' + '|'.join(KEYS) + r')=(.*)$', line.strip())
            if match:
                value = match[2].strip()
                if len(value) >= 2 and value[0] == value[-1] and value[0] in '\"\'':
                    value = value[1:-1]
                if not any(ord(c) < 32 for c in value):
                    values[match[1]] = value
    except FileNotFoundError:
        pass
    return values


def endpoint(value):
    try:
        u = urllib.parse.urlsplit(value if '://' in value else 'http://' + value)
        return (u.hostname or '', u.port or (443 if u.scheme == 'https' else 80))
    except ValueError:
        return ('invalid', 0)


def safe(values):
    # Show only endpoints, even when malformed input contains secret material.
    return {k: ('%s:%s' % endpoint(v) if 'proxy' in k.lower() and k.lower() != 'no_proxy'
                else v if '@' not in v else '[redacted]') for k, v in values.items()}


def render(prefix):
    values = environment(prefix)
    lines = ['[Service]']
    for upper in ('HTTPS_PROXY', 'HTTP_PROXY', 'NO_PROXY'):
        value = values.get(upper, values.get(upper.lower(), ''))
        if upper == 'NO_PROXY':
            value = ','.join(dict.fromkeys(['localhost', '127.0.0.1', '::1', '.ts.net', '100.64.0.0/10'] + value.split(',')))
        if any(c in value for c in '\n\r\x00'):
            sys.exit('Unsafe environment value')
        escaped = value.replace('\\', '\\\\').replace('"', '\\"').replace('%', '%%')
        for key in (upper, upper.lower()):
            lines.append('Environment="%s=%s"' % (key, escaped))
    return '\n'.join(lines)


def probe(hub, proxy=None):
    # Proxy credentials travel on curl's stdin, never in argv or diagnostics.
    config = ''
    if proxy:
        value = proxy.replace('\\', '\\\\').replace('"', '\\"')
        config = 'proxy = "' + value + '"\nnoproxy = ""\n'
    argv = ['curl', '-q', '--config', '-', '-fsS', '--connect-timeout', '5', '--max-time', '10', '--output', '/dev/null']
    if not proxy:
        argv += ['--noproxy', '*']
    r = subprocess.run(argv + [hub.rstrip('/') + '/healthz'], input=config, text=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return r.returncode == 0


def proxy_check(prefix, hub):
    disk = environment(prefix)
    shell = {k: os.environ[k] for k in KEYS if os.environ.get(k)}
    dropins = glob.glob(prefix + '/etc/systemd/system/clawctl-agent.service.d/*.conf')
    unit = {}
    for path in sorted(dropins):
        try:
            for line in open(path):
                if line.strip().startswith('Environment='):
                    for assignment in shlex.split(line.strip().split('=', 1)[1]):
                        if '=' in assignment:
                            k, v = assignment.split('=', 1)
                            unit[k] = v.replace('%%', '%')
        except (PermissionError, ValueError):
            continue
    wanted = {k: v for k, v in disk.items() if k.lower() != 'no_proxy' and v}
    match = bool(dropins) and all(endpoint(unit.get(k, unit.get(k.swapcase(), ''))) == endpoint(v) for k, v in wanted.items())
    no_proxy = unit.get('NO_PROXY', unit.get('no_proxy', ''))
    missing = [s for s in ('localhost', '127.0.0.1', '::1', '.ts.net', '100.64.0.0/10') if s not in no_proxy.split(',')]
    proxy = disk.get('HTTPS_PROXY', disk.get('https_proxy', disk.get('HTTP_PROXY', disk.get('http_proxy', ''))))
    direct = via = None
    if hub:
        u = urllib.parse.urlsplit(hub)
        if u.scheme not in ('http', 'https') or not u.hostname or u.username or u.password or u.query or u.fragment:
            sys.exit('Invalid Hub URL')
        direct = probe(hub)
        via = probe(hub, proxy) if proxy else None
    return dict(environment=safe(shell), etc_environment=safe(disk), dropin_present=bool(dropins),
                dropin_match=match, no_proxy_missing=missing, direct=direct, proxy=via,
                verdict='ok' if (not proxy or (match and not missing)) and (not hub or direct or via) else 'fix-needed')


if __name__ == '__main__':
    action, prefix = sys.argv[1:3]
    if action == 'render':
        print(render(prefix))
    elif action == 'proxy':
        print(json.dumps(proxy_check(prefix, sys.argv[3])))
