#!/usr/bin/env bash
set -euo pipefail
trap 'printf "not ok - area assertion failed\n" >&2' ERR
ROOT=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
export HOME="$TMP/home" PATH="$TMP/bin:$PATH"
mkdir -p "$HOME/.claude" "$TMP/bin"
# All inventory commands are local fakes. Never inspect the real host.
for cmd in systemctl ps ss crontab dpkg-query rpm snap flatpak npm docker rg iptables-save ip6tables-save; do
 printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/bin/$cmd"
done
cat > "$TMP/bin/find" <<'STUB'
#!/usr/bin/env bash
case $1 in "$HOME"|"$HOME"/*) exec /usr/bin/find "$@";; *) exit 0;; esac
STUB
cat > "$TMP/bin/dpkg-query" <<'STUB'
#!/usr/bin/env bash
if [[ $1 == -W ]]; then echo installed; fi
STUB
cat > "$TMP/bin/python3" <<'STUB'
#!/usr/bin/env bash
if [[ ${1:-} == -m ]]; then exit 0; fi
exec /usr/bin/python3 "$@"
STUB
cat > "$TMP/bin/id" <<'STUB'
#!/usr/bin/env bash
echo "${FAKE_UID:-1000}"
STUB
cat > "$TMP/bin/apt-get" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PACKAGE_LOG"
STUB
cat > "$TMP/bin/stat" <<'STUB'
#!/usr/bin/env bash
echo 0
STUB
cat > "$TMP/bin/rm" <<'STUB'
#!/usr/bin/env bash
for path in "$@"; do
 if [[ $path == /opt/retired-ide-test ]]; then
  [[ ${FAKE_UID:-1000} == 0 ]] || exit 1
  exit 0
 fi
 if [[ $path == "$HOME/tool" ]]; then
  /usr/bin/find "$HOME" -name items.tar -size +0c | /usr/bin/grep -q . || exit 1
 fi
done
exec /usr/bin/rm "$@"
STUB
chmod +x "$TMP/bin/"*
passed=0
ok() { passed=$((passed+1)); printf 'ok - %s\n' "$1"; }
printf '{"hooks":{"Start":[{"hooks":[{"command":"retired-ide fake-secret-value"},{"command":"keep"}]}]},"statusLine":{"command":"retired-ide"},"token":"fake-secret-value"}\n' > "$HOME/.claude/settings.json"
python3 "$ROOT/strip-hooks.py" --pattern retired-ide "$HOME/.claude/settings.json" > "$TMP/output"
if grep -q fake-secret-value "$TMP/output"; then echo "not ok - fake secret leaked"; exit 1; fi; ok 'preview output never contains fake token'
python3 "$ROOT/strip-hooks.py" --apply --pattern retired-ide "$HOME/.claude/settings.json" >> "$TMP/output"
python3 - "$HOME/.claude/settings.json" <<'PY'
import json,sys,glob,os
v=json.load(open(sys.argv[1]))
assert v['token']=='fake-secret-value'
assert v['hooks']['Start'][0]['hooks']==[{'command':'keep'}]
assert 'statusLine' not in v
assert os.stat(glob.glob(sys.argv[1]+'.bak-*')[0]).st_mode & 0o777 == 0o600
PY
ok 'JSON hook removal preserves other values and backs up'
printf 'PACKAGES=retired-ide\nPATHS=%s/tool /opt/retired-ide-test\nHOOK_PATTERN=retired-ide\n' "$HOME" > "$TMP/manifest"
mkdir "$HOME/tool"
bash "$ROOT/remove.sh" --name retired-ide --manifest "$TMP/manifest" > "$TMP/output"
[[ -d $HOME/tool ]]; ok 'plan retains files'
bash "$ROOT/remove.sh" --name retired-ide --manifest "$TMP/manifest" --apply > "$TMP/hint" || true
python3 - "$TMP/hint" <<'PY'
import sys
h=[l for l in open(sys.argv[1]).read().splitlines() if l.startswith('sudo')]
assert len(h)==1 and h[0].startswith("sudo bash -c '") and h[0].endswith("'")
PY
[[ ! -e $HOME/tool ]]
find "$HOME" -name items.tar -size +0c | grep -q .
ok 'user removal and private backup with one root hint'
for path in / /usr /etc "$HOME" "$HOME/../bad"; do
 printf 'PATHS=%s\n' "$path" > "$TMP/bad"
 if bash "$ROOT/remove.sh" --name retired-ide --manifest "$TMP/bad"; then exit 1; fi
done
ok 'dangerous paths rejected'
printf 'PACKAGES=retired*\n' > "$TMP/bad"
if bash "$ROOT/remove.sh" --name retired-ide --manifest "$TMP/bad"; then exit 1; fi
ok 'package glob rejected'
printf 'PACKAGES=$(touch %s/injected)\n' "$TMP" > "$TMP/bad"
if bash "$ROOT/remove.sh" --name retired-ide --manifest "$TMP/bad"; then exit 1; fi
[[ ! -e $TMP/injected ]]; ok 'manifest never sourced'
export PACKAGE_LOG="$TMP/packages"
FAKE_UID=0 bash "$ROOT/remove.sh" --name retired-ide --manifest "$TMP/manifest" --apply > "$TMP/output"
[[ ! -e $HOME/tool ]]; grep -qx 'purge -y -- retired-ide' "$PACKAGE_LOG"
if grep -q fake-secret-value "$TMP/output"; then echo "not ok - fake secret leaked"; exit 1; fi; ok 'exact purge with no token output'
bash "$ROOT/inventory.sh" --name retired-ide > "$TMP/output"
if grep -q fake-secret-value "$TMP/output"; then echo "not ok - fake secret leaked"; exit 1; fi; ok 'inventory hides secret content'
bash "$ROOT/verify.sh" --name retired-ide --pattern retired-ide > "$TMP/output"; ok 'verify empty inventory'
printf '#!/usr/bin/env bash\nexit 1\n' > "$TMP/bin/systemctl"
bash "$ROOT/verify.sh" --name retired-ide --pattern retired-ide > "$TMP/output"
grep -q skipped "$TMP/output"
ok 'unavailable inventory is skipped'
cat > "$TMP/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
printf 'retired-ide.service enabled\n'
STUB
for cmd in ps crontab; do
 printf '#!/usr/bin/env bash\necho "retired-ide fake-secret-value"\n' > "$TMP/bin/$cmd"
done
printf 'retired-ide fake-secret-value\n' > "$HOME/.bashrc"
mkdir -p "$HOME/.local/bin/retired-ide"
cat > "$TMP/bin/dpkg-query" <<'STUB'
#!/usr/bin/env bash
printf 'retired-ide\n'
STUB
bash "$ROOT/inventory.sh" --name retired-ide > "$TMP/output"
grep -q '  retired-ide.service' "$TMP/output"
grep -q '  retired-ide$' "$TMP/output"
grep -q "$HOME/.local/bin/retired-ide" "$TMP/output"
if grep -q fake-secret-value "$TMP/output"; then echo "not ok - fake secret leaked"; exit 1; fi
bash "$ROOT/inventory.sh" --name retired-ide --json | python3 -c 'import json,sys; assert json.load(sys.stdin)["sources"]'
ok 'bounded safe names and JSON without private lines'
printf 'PACKAGES=orca\n' > "$TMP/guard"
if bash "$ROOT/remove.sh" --name retired-ide --manifest "$TMP/guard" > "$TMP/output"; then exit 1; fi
printf 'ALLOW_PACKAGES=orca\n' >> "$TMP/guard"
bash "$ROOT/remove.sh" --name retired-ide --manifest "$TMP/guard" > "$TMP/output"
ok 'ambiguous package requires explicit allow'
export FIREWALL_LOG="$TMP/firewall"
for cmd in iptables ip6tables; do
 cat > "$TMP/bin/$cmd" <<'STUB'
#!/usr/bin/env bash
if [[ $1 == -S ]]; then
 printf '%s\n' '-A INPUT -p tcp --dport 8080 -j ACCEPT' '-A INPUT -p tcp --dport 8081 -j ACCEPT' '-A INPUT -p tcp --sport 8080 -j ACCEPT'
else
 /usr/bin/find "$HOME" -name items.tar -size +0c | /usr/bin/grep -q . || exit 1
 printf '%s\n' "$*" >> "$FIREWALL_LOG"
fi
STUB
 chmod +x "$TMP/bin/$cmd"
 printf '#!/usr/bin/env bash\necho saved-rules\n' > "$TMP/bin/$cmd-save"
 chmod +x "$TMP/bin/$cmd-save"
done
printf 'PORT=8080\n' > "$TMP/firewall-manifest"
FAKE_UID=0 bash "$ROOT/remove.sh" --name retired-ide --manifest "$TMP/firewall-manifest" --apply --root-only > "$TMP/output"
[[ $(wc -l < "$FIREWALL_LOG") == 2 ]]
[[ $(grep -c -- '-D INPUT -p tcp --dport 8080 -j ACCEPT' "$FIREWALL_LOG") == 2 ]]
ok 'root firewall deletes only exact destination port rules after backup'
printf '{broken' > "$HOME/.codex-invalid"
mkdir -p "$HOME/.codex"
cp "$HOME/.codex-invalid" "$HOME/.codex/hooks.json"
if bash "$ROOT/verify.sh" --name absent-tool > "$TMP/output"; then exit 1; fi
grep -q 'invalid JSON:' "$TMP/output"
ok 'invalid config JSON fails verification'
rm -f "$HOME/.codex/hooks.json"
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$HOME/notes/history.log"\n' > "$TMP/bin/rg"
chmod +x "$TMP/bin/rg"
bash "$ROOT/inventory.sh" --name absent-tool > "$TMP/output"
grep -q 'history.log' "$TMP/output"
ok 'reference scan lists matching file names'
bash "$ROOT/verify.sh" --name absent-tool > "$TMP/output" 2> "$TMP/notes"
grep -q 'note (not a failure): reference-files' "$TMP/notes"
ok 'reference-file hits are notes, not verify failures'
printf 'passed: %s, failed: 0\n' "$passed"
