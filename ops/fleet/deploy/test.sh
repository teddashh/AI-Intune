#!/usr/bin/env bash
set -euo pipefail
trap 'printf "not ok - area assertion failed\n" >&2' ERR
ROOT=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d); trap 'chmod -R u+w "$TMP"; rm -rf "$TMP"' EXIT
mkdir "$TMP/bin"
export DEPLOY_ROOT="$TMP/app" DEPLOY_WINDOW=0-59 PATH="$TMP/bin:$PATH"
cat > "$TMP/bin/id" <<'STUB'
#!/usr/bin/env bash
echo 0
STUB
cat > "$TMP/bin/chown" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
cat > "$TMP/bin/date" <<'STUB'
#!/usr/bin/env bash
if [[ $* == '-u +%M' ]]; then echo 30; else /usr/bin/date "$@"; fi
STUB
chmod +x "$TMP/bin/"*
cat > "$TMP/hooks" <<'HOOKS'
printf 'fake-secret-value\n'
hook_build() { printf 'code\n' > "$2/app"; touch "$2/CODE_ONLY"; }
hook_health() { [[ ${FAIL_CANARY:-0} != 1 || $2 != candidate ]] || return 1; printf '%s\n' "$1"; }
hook_smoke() { [[ ${FAIL_POST:-0} != 1 || $1 != bbbbbbb || $2 != live ]]; }
hook_restart() { :; }
hook_edge() { :; }
HOOKS
passed=0
ok() { passed=$((passed+1)); printf 'ok - %s\n' "$1"; }
run() { bash "$ROOT/deploy-template.sh" "$@" --hooks "$TMP/hooks"; }
run build --sha aaaaaaa
[[ ! -e $DEPLOY_ROOT ]]; ok 'plan does not stage'
run preflight; ok 'preflight'
run build --sha aaaaaaa --apply > "$TMP/output"
run build --sha bbbbbbb --apply >> "$TMP/output"
if grep -q fake-secret-value "$TMP/output"; then echo "not ok - fake secret leaked"; exit 1; fi; ok 'trusted hook output never exposes fake token' 
ln -s releases/aaaaaaa "$DEPLOY_ROOT/current"
if DEPLOY_WINDOW=40-50 run deploy --sha bbbbbbb --apply; then exit 1; fi
ok 'window refusal'
if DEPLOY_WINDOW=40-50 DEPLOY_TEST_REFUSE=0 run deploy --sha bbbbbbb --apply; then exit 1; fi
ok 'test hook cannot allow'
if DEPLOY_TEST_REFUSE=1 run deploy --sha bbbbbbb --apply; then exit 1; fi
ok 'test hook may refuse'
if FAIL_CANARY=1 run deploy --sha bbbbbbb --apply; then exit 1; fi
[[ $(readlink "$DEPLOY_ROOT/current") == releases/aaaaaaa ]]; ok 'canary abort preserves current'
if FAIL_POST=1 run deploy --sha bbbbbbb --apply; then exit 1; fi
[[ $(readlink "$DEPLOY_ROOT/current") == releases/aaaaaaa ]]; ok 'automatic rollback'
run deploy --sha bbbbbbb --apply
[[ $(readlink "$DEPLOY_ROOT/current") == releases/bbbbbbb ]]; ok 'deploy switches'
run rollback --apply
[[ $(readlink "$DEPLOY_ROOT/current") == releases/aaaaaaa ]]; ok 'rollback restores previous'
[[ -d $DEPLOY_ROOT/releases/aaaaaaa && -d $DEPLOY_ROOT/releases/bbbbbbb ]]; ok 'releases retained'
(
 flock -n 8
 if run deploy --sha bbbbbbb --apply; then exit 1; fi
) 8>"$DEPLOY_ROOT/state/lock"
ok 'lock excludes concurrent operation'
if DEPLOY_WINDOW=40-50 run rollback --apply; then exit 1; fi
ok 'rollback window refusal'
DEPLOY_WINDOW=40-50 run rollback --apply --force-window
ok 'explicit rollback window override'
chmod u+w "$DEPLOY_ROOT/releases/bbbbbbb/app"
printf 'tampered\n' >> "$DEPLOY_ROOT/releases/bbbbbbb/app"
if run deploy --sha bbbbbbb --apply; then exit 1; fi
[[ $(readlink "$DEPLOY_ROOT/current") == releases/aaaaaaa ]]; ok 'tampered release refused before switching' 
cat > "$TMP/bin/id" <<'STUB'
#!/usr/bin/env bash
echo 1000
STUB
if run rollback --apply > "$TMP/hint"; then exit 1; fi
python3 - "$TMP/hint" <<'PY'
import sys
lines=open(sys.argv[1]).read().splitlines()
assert len(lines)==1 and lines[0].startswith("sudo bash -c '") and lines[0].endswith("'")
assert 'DEPLOY_ROOT=' in lines[0]
PY
ok 'root hint is one line and preserves deployment paths'
printf 'passed: %s, failed: 0\n'  "$passed"
