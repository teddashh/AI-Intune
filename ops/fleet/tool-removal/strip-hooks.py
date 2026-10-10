#!/usr/bin/env python3
"""Remove matching command hook entries without exposing configuration values."""
import argparse
import datetime
import json
import os
import re
import shutil
import tempfile

p = argparse.ArgumentParser()
p.add_argument('files', nargs='+')
p.add_argument('--pattern', required=True)
p.add_argument('--apply', action='store_true')
a = p.parse_args()
pattern = re.compile(a.pattern)


def matches(value):
    return isinstance(value, dict) and isinstance(value.get('command'), str) and pattern.search(value['command'])


def clean(value, hook=False):
    if isinstance(value, list):
        return [clean(v, hook) for v in value if not (hook and matches(v))]
    if isinstance(value, dict):
        result = {}
        for k, v in value.items():
            inside = hook or k in ('hooks', 'statusLine')
            if inside and matches(v):
                continue
            result[k] = clean(v, inside)
        return result
    return value


for path in a.files:
    if not os.path.exists(path):
        continue
    if os.path.islink(path):
        raise SystemExit('refuse symlink config')
    with open(path, encoding='utf-8') as f:
        original = json.load(f)
    updated = clean(original)
    changed = original != updated
    print(f'{path}: changed={int(changed)}')
    if changed and a.apply:
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
        backup = path + '.bak-' + stamp
        shutil.copy2(path, backup)
        os.chmod(backup, 0o600)
        fd, tmp = tempfile.mkstemp(dir=os.path.dirname(os.path.abspath(path)))
        try:
            with os.fdopen(fd, 'w', encoding='utf-8') as f:
                json.dump(updated, f, indent=2)
                f.write('\n')
            shutil.copystat(path, tmp)
            os.chown(tmp, os.stat(path).st_uid, os.stat(path).st_gid)
            os.replace(tmp, path)
        finally:
            if os.path.exists(tmp):
                os.unlink(tmp)
