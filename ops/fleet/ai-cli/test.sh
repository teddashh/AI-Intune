#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
AREA=$ROOT/ops/fleet/ai-cli
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export HOME=$TMP/home
mkdir -p "$HOME/.config/claude-automation" "$TMP/bin" "$TMP/first"
export PATH=$TMP/first:$TMP/bin:/usr/bin:/bin
passed=0
failed=0
output=
rc=0
run() { rc=0; output=$("$@" 2>&1) || rc=$?; }
expect() {
    local name=$1
    shift
    if "$@"; then printf 'ok - %s\n' "$name"; passed=$((passed+1)); else printf 'not ok - %s (exit %s)\n' "$name" "$rc"; failed=$((failed+1)); fi
}
status() { [ "$rc" -eq "$1" ]; }
lacks_secret() { [[ $output != *fixture-private-value* ]]; }
cat > "$TMP/bin/claude" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = --version ]; then printf '1.2.3\n'; exit 0; fi
[ "${CLAUDE_CODE_OAUTH_TOKEN:-}" = fixture-private-value ] || exit 9
[ "${ANTHROPIC_API_KEY+x}" != x ] || exit 10
[ "${ANTHROPIC_AUTH_TOKEN+x}" != x ] || exit 10
[ "${CLAUDE_CODE_USE_BEDROCK+x}" != x ] || exit 10
[ "${CLAUDE_CODE_USE_VERTEX+x}" != x ] || exit 10
printf 'OK\n'
STUB
cat > "$TMP/bin/cli" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = --version ]; then printf '1.2.3\n'; else printf '%s\n' "${FAKE_LIVE:-OK}"; fi
STUB
for name in codex grok agy gemini node; do ln -s cli "$TMP/bin/$name"; done
cat > "$TMP/bin/crontab" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' '0 * * * * claude -p hello'
STUB
cat > "$TMP/bin/id" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = -u ]; then printf '12345\n'; else /usr/bin/id "$@"; fi
STUB
for name in npm curl; do
    printf '#!/usr/bin/env bash\nexit 99\n' > "$TMP/bin/$name"
done
chmod +x "$TMP/bin/"*
ln -s "$AREA/claude-automation" "$TMP/first/claude"
ln -s "$AREA/claude-automation" "$TMP/bin/claude-automation"
token=$HOME/.config/claude-automation/oauth-token
run "$AREA/claude-automation" -p test
expect 'wrapper missing token' status 78
printf 'fixture-private-value\n' > "$TMP/token"
ln -s "$TMP/token" "$token"
run "$AREA/claude-automation" -p test
expect 'wrapper refuses symlink' status 78
rm "$token"
cp "$TMP/token" "$token"
chmod 644 "$token"
run "$AREA/claude-automation" -p test
expect 'wrapper refuses 0644' status 78
chmod 600 "$token"
: > "$token"
run "$AREA/claude-automation" -p test
expect 'wrapper refuses empty token' status 78
printf 'fixture-private-value\n' > "$token"
run env ANTHROPIC_API_KEY=remove ANTHROPIC_AUTH_TOKEN=remove CLAUDE_CODE_USE_BEDROCK=1 CLAUDE_CODE_USE_VERTEX=1 "$AREA/claude-automation" -p test
expect 'wrapper passes env token and skips itself' status 0
expect 'wrapper never prints token' lacks_secret
run env CLAUDE_AUTOMATION_CLAUDE_BIN="$AREA/claude-automation" "$AREA/claude-automation" -p test
expect 'wrapper rejects recursive override' status 78
run env CLAUDE_AUTOMATION_CLAUDE_BIN="$TMP/bin/claude" "$AREA/claude-automation" -p test
expect 'wrapper honours override' status 0
chmod 400 "$token"
run "$AREA/claude-automation" -p test
expect 'wrapper accepts 400' status 0
cat > "$TMP/bin/stat" <<'STUB'
#!/usr/bin/env bash
printf '600 another-user\n'
STUB
chmod +x "$TMP/bin/stat"
run "$AREA/claude-automation" -p test
expect 'wrapper refuses wrong owner' status 78
rm "$TMP/bin/stat"
run env PATH="$TMP/first:/usr/bin:/bin" "$AREA/claude-automation" -p test
expect 'wrapper missing real Claude exits 78' status 78
printf 'export OPENAI_API_KEY=fixture-private-value\ncase $- in *i*) ;; *) return;; esac\n' > "$HOME/.bashrc"
mkdir -p "$HOME/.config/systemd/user"
printf 'ExecStart=claude --print test\n' > "$HOME/.config/systemd/user/example.service"
run "$AREA/check-ai-clis.sh" --json --user-home "$HOME"
expect 'checker drift exit' status 1
printf '%s\n' "$output" > "$TMP/check.json"
run python3 -m json.tool "$TMP/check.json"
expect 'checker valid JSON' status 0
run python3 - "$TMP/check.json" "$HOME" <<'PY'
import json,sys
x=json.load(open(sys.argv[1]))
assert sys.argv[2]+'/.bashrc:1' in x['rc_token_findings']
assert 'crontab:1' in x['automation_misuse']
assert sys.argv[2]+'/.config/systemd/user/example.service:1' in x['automation_misuse']
assert x['gemini_legacy']
assert x['clis']['claude']['login_state']=='ready'
assert 'fixture-private-value' not in json.dumps(x)
PY
expect 'checker findings and legacy without secret' status 0
run "$AREA/check-ai-clis.sh" --bad
expect 'checker usage exit' status 2
# Real system PATH directories resolve; mock lookup without touching them.
mkdir -p "$HOME/.codex" "$HOME/.grok" "$HOME/.gemini/antigravity-cli"
printf '{}\n' > "$HOME/.codex/auth.json"
printf '{}\n' > "$HOME/.grok/auth.json"
printf '{}\n' > "$HOME/.gemini/antigravity-cli/oauth.json"
chmod 600 "$HOME/.codex/auth.json" "$HOME/.grok/auth.json"
: > "$HOME/.bashrc"
rm "$HOME/.config/systemd/user/example.service"
printf '#!/usr/bin/env bash\nexit 1\n' > "$TMP/bin/crontab"
rm "$TMP/bin/gemini"
mkdir -p "$TMP/python"
cat > "$TMP/python/sitecustomize.py" <<'PY'
import shutil
original=shutil.which
def which(name,*args,**kwargs):
    if name in ('claude','codex','grok','agy'): return '/usr/local/bin/'+name
    return original(name,*args,**kwargs)
shutil.which=which
import subprocess
original_run=subprocess.run
def run(args,*a,**kw):
    if args[0].startswith('/usr/local/bin/'):
        return subprocess.CompletedProcess(args,0,'1.2.3\n','')
    return original_run(args,*a,**kw)
subprocess.run=run
PY
run env PYTHONPATH="$TMP/python" "$AREA/check-ai-clis.sh" --json
expect 'checker clean verdict exits zero' status 0
run env FAKE_LIVE=fixture-private-value "$AREA/check-ai-clis.sh" --json --live
expect 'live drift exit' status 1
expect 'live output redacted' lacks_secret
run env FAKE_LIVE='unauthorized fixture-private-value' "$AREA/check-ai-clis.sh" --json --live
expect 'live authentication output redacted' lacks_secret
[[ $output == *auth_fail* ]] && auth_detected=true || auth_detected=false
expect 'live authentication classified' "$auth_detected"
# Live answer is the last stdout line; banners on stderr must not break it.
cat > "$TMP/banner-cli" <<'STUB'
#!/usr/bin/env bash
if [ "${1:-}" = --version ]; then printf '1.2.3\n'; exit 0; fi
printf 'banner: model fixture\n' >&2
printf 'thinking...\nOK\n'
STUB
chmod +x "$TMP/banner-cli"
mkdir -p "$TMP/banner"
for name in codex grok agy; do ln -s "$TMP/banner-cli" "$TMP/banner/$name"; done
run env PATH="$TMP/banner:$PATH" "$AREA/check-ai-clis.sh" --json --live
live_ok=$(printf '%s' "$output" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(all(d["clis"][n].get("live")=="ok" for n in ("codex","grok","agy")))' 2>/dev/null || true)
expect 'live ok despite stderr banner' [ "$live_ok" = True ]
# CLIs in ~/.local/bin but not on the non-interactive PATH: present, PATH drift.
mkdir -p "$TMP/udir/.local/bin" "$TMP/minbin"
ln -s "$TMP/bin/cli" "$TMP/udir/.local/bin/agy"
for tool in python3 bash env; do ln -sf "$(command -v "$tool")" "$TMP/minbin/$tool"; done
run env PATH="$TMP/minbin" "$AREA/check-ai-clis.sh" --json --user-home "$TMP/udir"
user_dir=$(printf '%s' "$output" | python3 -c 'import json,sys; d=json.load(sys.stdin); a=d["clis"]["agy"]; print(a["present"] and not a["resolves_noninteractive"] and "noninteractive PATH: agy" in d["reasons"])' 2>/dev/null || true)
expect 'user bin dir found but flagged noninteractive' [ "$user_dir" = True ]
before=$(find "$HOME" -type f -exec sha256sum {} + | sort)
run "$AREA/install-ai-clis.sh" --plan
expect 'install plan exit' status 0
after=$(find "$HOME" -type f -exec sha256sum {} + | sort)
expect 'plan changes nothing' test "$before" = "$after"
rm "$TMP/first/claude" "$TMP/bin/claude" "$TMP/bin/codex" "$TMP/bin/node"
mkdir -p "$HOME/.grok/bin"
cp "$TMP/bin/cli" "$HOME/.grok/bin/grok"
command() {
    if [ "${1:-}" = -v ] && [ "${2:-}" = node ]; then return 1; fi
    builtin command "$@"
}
export -f command
run "$AREA/install-ai-clis.sh" --apply
unset -f command
expect 'nonroot install apply' status 0
printf '%s\n' "$output" > "$TMP/install.out"
run python3 - "$TMP/install.out" <<'PY'
import sys,shlex
lines=open(sys.argv[1]).read().splitlines()
hints=[x for x in lines if x.startswith('sudo')]
assert len(hints)==4
for x in hints:
 assert x.startswith("sudo bash -c '") and x.endswith("'")
 assert len(shlex.split(x))==4
assert all('apt upgrade' not in x for x in hints)
PY
expect 'all root hints single-line quoted commands' status 0
expect 'wrapper installed executable' test -x "$HOME/.local/bin/claude-automation"
expect 'grok symlink installed' test -L "$HOME/.local/bin/grok"
expect 'bashrc backed up' bash -c 'compgen -G "$HOME/.bashrc.bak-*" >/dev/null'
run "$AREA/install-ai-clis.sh" --apply --only claude
expect 'second apply' status 0
expect 'PATH block inserted once at top' test "$(grep -c '^# fleet-ai-cli PATH begin$' "$HOME/.bashrc")" = 1
expect 'private token directory' test "$(stat -c %a "$HOME/.config/claude-automation")" = 700
cat > "$TMP/bin/ssh-stub" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = -o ] && [ "$2" = BatchMode=yes ] && [ "$4" = ForwardAgent=no ] || exit 9
bash -c "$6"
STUB
chmod +x "$TMP/bin/ssh-stub"
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$1"\n' > "$TMP/remote.sh"
run "$ROOT/ops/fleet/fleet-run.sh" --host host-a --out "$TMP/reports" --ssh "$TMP/bin/ssh-stub" -- "$TMP/remote.sh" 'space and quote '\'' literal'
expect 'fleet run stub and quoted arguments' status 0
expect 'fleet argument preserved' test "$(cat "$TMP/reports/host-a.out")" = 'space and quote '\'' literal'
printf 'host-a # comment\nhost-b\n' > "$TMP/hosts"
run "$ROOT/ops/fleet/fleet-run.sh" --hosts "$TMP/hosts" --out "$TMP/reports" --ssh "$TMP/bin/ssh-stub" -- "$TMP/remote.sh" OK
expect 'fleet hosts file comments' status 0
cp "$TMP/check.json" "$TMP/reports/host-a.out"
run "$ROOT/ops/fleet/fleet-report.sh" "$TMP/reports"
expect 'fleet report exit' status 0
expect 'fleet report table' bash -c '[[ $1 == *"| host-a |"* && $1 == *"| host-b | unknown"* ]]' _ "$output"
printf '#!/usr/bin/env bash\nexit 7\n' > "$TMP/failure.sh"
run "$ROOT/ops/fleet/fleet-run.sh" --hosts "$TMP/hosts" --out "$TMP/failures" --ssh "$TMP/bin/ssh-stub" -- "$TMP/failure.sh"
expect 'fleet failure returns nonzero' status 1
expect 'fleet continues after failure and records rc' test "$(cat "$TMP/failures/host-b.rc")" = 7
printf '\npassed: %s, failed: %s\n'  "$passed" "$failed"
[ "$failed" -eq 0 ]
