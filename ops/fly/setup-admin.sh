#!/bin/bash
# Finish first-run setup and save credentials privately.
set -euo pipefail
url=''
app=''
username=''
setup_code=''
setup_code_file=''
out=''
fail() { echo "setup-admin: $*" >&2; exit 1; }
while [[ $# -gt 0 ]]; do
  [[ $# -ge 2 ]] || fail "missing flag value"
  case "$1" in
    --url) url=$2 ;; --app) app=$2 ;; --username) username=$2 ;;
    --setup-code-file) setup_code_file=$2 ;; --setup-code) setup_code=$2 ;; --out) out=$2 ;;
    *) fail "unknown flag" ;;
  esac
  shift 2
done
[[ -n "$url" ]] || { [[ "$app" =~ ^[a-z0-9-]{1,63}$ ]] || fail "--url or valid --app required"; url="https://$app.fly.dev"; }
[[ -n "$username" && -n "$out" ]] || fail "--username and --out required"
[[ -n "$setup_code" || -n "$setup_code_file" ]] || fail "setup code required"
[[ -z "$setup_code" || -z "$setup_code_file" ]] || fail "choose only one setup code source"
[[ ! -e "$out" && ! -L "$out" ]] || fail "output file already exists"
command -v python3 >/dev/null || fail "python3 not found on PATH"
# The code is passed in the environment, never as a Python command argument.
SETUP_ADMIN_CODE="$setup_code" python3 - "$url" "$username" "$setup_code_file" "$out" <<'PY'
import base64
import hashlib
import hmac
import http.client
from http.cookies import SimpleCookie
from html.parser import HTMLParser
import json
import os
from pathlib import Path
import re
import secrets
import ssl
import struct
import sys
import time
from urllib.parse import urlencode, urlsplit

class Blocks(HTMLParser):
    def __init__(self):
        super().__init__()
        self.tag = None
        self.text = ''
        self.codes = []
        self.pres = []
    def handle_starttag(self, tag, attrs):
        if tag in ('code', 'pre'):
            self.tag, self.text = tag, ''
    def handle_data(self, data):
        if self.tag:
            self.text += data
    def handle_endtag(self, tag):
        if tag == self.tag:
            (self.codes if tag == 'code' else self.pres).append(self.text)
            self.tag = None

def main():
    raw_url, username, code_file, output = sys.argv[1:]
    origin = urlsplit(raw_url)
    if (origin.scheme != 'https' or not origin.hostname or origin.username or
            origin.password or origin.path not in ('', '/') or origin.query or origin.fragment):
        raise RuntimeError('--url must be an HTTPS origin')
    if not re.fullmatch(r'[A-Za-z0-9._-]{3,64}', username):
        raise RuntimeError('invalid username')
    if os.path.lexists(output):
        raise RuntimeError('output file already exists')
    code = Path(code_file).read_text().strip() if code_file else os.environ['SETUP_ADMIN_CODE']
    if not code:
        raise RuntimeError('empty setup code')
    password = secrets.token_urlsafe(32)
    cookies = SimpleCookie()
    # One TLS connection keeps IP-bound authentication stable across requests.
    conn = http.client.HTTPSConnection(origin.hostname, origin.port or 443,
                                       timeout=30, context=ssl.create_default_context())
    conn.connect()
    socket = conn.sock
    def request(method, path, fields=None):
        if conn.sock is not socket:
            raise RuntimeError('keep-alive connection lost; refusing to reconnect')
        headers = {'Connection': 'keep-alive'}
        if cookies:
            headers['Cookie'] = '; '.join(f'{key}={value.value}' for key, value in cookies.items())
        body = None
        if fields is not None:
            body = urlencode(fields)
            headers['Content-Type'] = 'application/x-www-form-urlencoded'
        conn.request(method, path, body, headers)
        response = conn.getresponse()
        data = response.read().decode('utf-8')
        for key, value in response.getheaders():
            if key.lower() == 'set-cookie':
                cookies.load(value)
        if response.will_close or conn.sock is not socket:
            raise RuntimeError('server closed the keep-alive connection; refusing to reconnect')
        return response.status, response.getheader('Location'), data
    try:
        status, location, _ = request('POST', '/setup', {
            'username': username, 'setup_code': code, 'password': password,
            'next': '/account/security?enroll=1'})
        if status == 404:
            raise RuntimeError('setup is already closed; use /login')
        target = urlsplit(location or '')
        if status not in (302, 303) or target.path != '/account/security' or target.query != 'enroll=1':
            raise RuntimeError('setup did not redirect to MFA enrollment')
        if '__Host-clawctl_session' not in cookies:
            raise RuntimeError('setup session cookie missing')
        status, _, html = request('GET', '/account/security?enroll=1')
        if status != 200:
            raise RuntimeError('cannot open MFA enrollment')
        blocks = Blocks()
        blocks.feed(html)
        candidates = [''.join(value.split()) for value in blocks.codes
                      if not value.startswith('otpauth://')]
        candidates = [value for value in candidates if re.fullmatch(r'[A-Z2-7]{16,}', value)]
        if len(candidates) != 1:
            raise RuntimeError('cannot parse TOTP secret')
        secret = candidates[0]
        key = base64.b32decode(secret + '=' * ((-len(secret)) % 8))
        digest = hmac.new(key, struct.pack('>Q', int(time.time()) // 30), hashlib.sha1).digest()
        offset = digest[-1] & 15
        number = (struct.unpack('>I', digest[offset:offset + 4])[0] & 0x7fffffff) % 1000000
        status, _, html = request('POST', '/account/security/totp/confirm', {'code': f'{number:06d}'})
        if status != 200:
            raise RuntimeError('MFA confirmation failed')
        blocks = Blocks()
        blocks.feed(html)
        recovery = [value.strip().splitlines() for value in blocks.pres]
        recovery = [values for values in recovery if len(values) == 10 and all(values)]
        if len(recovery) != 1:
            raise RuntimeError('cannot parse recovery codes')
        fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, 'w') as file:
                fd = None
                json.dump({'username': username, 'password': password, 'totp_secret': secret,
                           'recovery_codes': recovery[0]}, file, indent=2)
                file.write('\n')
        finally:
            if fd is not None:
                os.close(fd)
        print(f'MFA enrolled. Credentials saved to {output}')
    finally:
        conn.close()

try:
    main()
except Exception as error:
    # Never include server bodies or exception values that may contain secrets.
    safe = str(error) if isinstance(error, RuntimeError) else 'request or credential file operation failed'
    print(f'setup-admin: {safe}', file=sys.stderr)
    sys.exit(1)
PY
