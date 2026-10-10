#!/usr/bin/env bash
set -euo pipefail
exec python3 - "$@" <<'PY'
import argparse, glob, json, os, re, shutil, stat, subprocess, sys
p = argparse.ArgumentParser()
p.add_argument('--json', action='store_true')
p.add_argument('--live', action='store_true')
p.add_argument('--user-home', default=os.path.expanduser('~'))
a = p.parse_args()
h = a.user_home
# Non-interactive ssh often lacks the user bin dirs; still find the CLIs there
# (present) and report resolves_noninteractive separately.
search_path = os.pathsep.join([os.environ.get('PATH', ''), h+'/.local/bin', h+'/.grok/bin'])
def which(name):
    return shutil.which(name, path=search_path)

def run(args, seconds=10, split=False):
    # stdin is /dev/null: under `ssh host bash -s` stdin is the script itself.
    try:
        r = subprocess.run(args, capture_output=True, text=True, timeout=seconds, stdin=subprocess.DEVNULL)
        return (r.returncode, r.stdout, r.stderr) if split else (r.returncode, r.stdout + r.stderr)
    except (OSError, subprocess.TimeoutExpired):
        return (124, '', '') if split else (124, '')

def secure(path, owner=False, modes=(0o600,)):
    try:
        s = os.lstat(path)
        return stat.S_ISREG(s.st_mode) and stat.S_IMODE(s.st_mode) in modes and (not owner or s.st_uid == os.getuid())
    except OSError:
        return False

def read(path):
    try:
        with open(path) as f: return f.read()
    except OSError: return ''

envpath = ''
for line in read('/etc/environment').splitlines():
    m = re.match(r'^\s*PATH\s*=\s*(.*?)\s*$', line)
    if m: envpath = m[1].strip('"\'')
rcfiles = [h+'/'+x for x in ('.bashrc', '.profile', '.bash_profile', '.zshrc')]
rcfiles += glob.glob(h+'/.config/environment.d/*') + ['/etc/environment'] + glob.glob('/etc/profile.d/*')
keys = r'(?:CLAUDE_CODE_OAUTH_TOKEN|ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|OPENAI_API_KEY|XAI_API_KEY|GEMINI_API_KEY)'
findings = []
for path in rcfiles:
    for n, line in enumerate(read(path).splitlines(), 1):
        if not line.lstrip().startswith('#') and re.search(r'\b'+keys+r'\s*=', line):
            findings.append(f'{path}:{n}')
misuse = []
cron_rc, cron = run(['crontab', '-l'])
sources = [('crontab', cron if cron_rc == 0 else '')]
sources += [(x, read(x)) for x in glob.glob(h+'/.config/systemd/user/*.service')]
for path, data in sources:
    for n, line in enumerate(data.splitlines(), 1):
        if not line.lstrip().startswith('#') and re.search(r'(?<![\w-])(?:[\w/.-]*/)?claude\s+(?:-p\b|--print\b)', line):
            misuse.append(f'{path}:{n}')
result = {'clis': {}, 'rc_token_findings': findings, 'automation_misuse': misuse,
          'gemini_legacy': bool(which('gemini')), 'reasons': [], 'next_steps': []}
for name in ('claude', 'codex', 'grok', 'agy'):
    path = which(name)
    version = 'unknown'
    if path:
        code, raw = run([path, '--version'])
        # Only retain a version number, never arbitrary CLI output.
        m = re.search(r'(?<![\w.-])v?(\d+\.\d+(?:\.\d+)?)(?![\w.-])', raw)
        if code == 0 and m: version = m[1]
    resolves = bool(path and (os.path.dirname(path) in ('/usr/bin', '/usr/local/bin') or os.path.dirname(path) in envpath.split(':')))
    if name == 'claude':
        login = 'ready' if which('claude-automation') and secure(h+'/.config/claude-automation/oauth-token', True, (0o600, 0o400)) and bool(read(h+'/.config/claude-automation/oauth-token').strip()) else 'needed'
    elif name in ('codex', 'grok'):
        login = 'present' if secure(h+f'/.{name}/auth.json') else 'needed'
    else:
        login = 'present' if os.path.isdir(h+'/.gemini/antigravity-cli') and any(os.scandir(h+'/.gemini/antigravity-cli')) else 'unknown'
    item = dict(present=bool(path), path=path, version=version, resolves_noninteractive=resolves, login_state=login)
    for condition, reason in ((not path, 'missing'), (path and version == 'unknown', 'version unavailable'), (not resolves, 'noninteractive PATH'), (login == 'needed', 'login needed')):
        if condition: result['reasons'].append(f'{reason}: {name}')
    if login == 'needed':
        hint = {'claude': 'claude setup-token; save token privately in ~/.config/claude-automation/oauth-token (600)', 'codex': 'codex login --device-auth', 'grok': 'grok (interactive login)'}[name]
        result['next_steps'].append(f'login needed: {name} (human: {hint})')
    if a.live:
        cmds = {'claude': [which('claude-automation') or 'claude-automation', '-p'], 'codex': [which('codex') or 'codex', 'exec', '--skip-git-repo-check'], 'grok': [which('grok') or 'grok', '-p'], 'agy': [which('agy') or 'agy', '-p']}
        code, out, err = run(cmds[name]+['Reply with exactly the word OK'], 90, split=True)
        # CLIs print banners/progress on stderr; the answer is the last stdout line.
        last = (out.strip().splitlines() or [''])[-1].strip().strip('.*`"\'')
        status = 'ok' if code == 0 and last == 'OK' else 'auth_fail' if re.search(r'auth|login|unauthorized|token', out + err, re.I) else 'error'
        item['live'] = status
        item['live_reason'] = {'ok': 'OK', 'auth_fail': 'authentication required', 'error': 'prompt failed or unexpected response'}[status]
        if status != 'ok': result['reasons'].append(f'live {status}: {name}')
    result['clis'][name] = item
if findings: result['reasons'].append('tokens assigned in rc files')
if misuse: result['reasons'].append('bare claude automation')
if result['gemini_legacy']: result['reasons'].append('gemini legacy: use agy')
if any('PATH' in x or 'missing' in x for x in result['reasons']): result['next_steps'].append('preview install-ai-clis.sh --plan, then --apply')
result['verdict'] = 'drift' if result['reasons'] else 'ok'
if a.json: print(json.dumps(result))
else:
    print('CLI      PRESENT VERSION      LOGIN    NONINTERACTIVE PATH')
    for name, v in result['clis'].items(): print(f"{name:8} {str(v['present']):7} {v['version']:12} {v['login_state']:8} {v['resolves_noninteractive']} {v['path'] or '-'}")
    print('verdict: '+result['verdict'])
    for key in ('reasons', 'next_steps', 'rc_token_findings', 'automation_misuse'):
        for value in result[key]: print(key+': '+value)
sys.exit(0 if result['verdict'] == 'ok' else 1)
PY
