#!/usr/bin/env bash
set -euo pipefail
cmd=${1:-status}; shift || true
apply=false; force=false; hooks=''; sha=''
while (($#)); do
 case $1 in --apply) apply=true; shift;; --force-window) force=true; shift;; --hooks) hooks=$2; shift 2;; --sha) sha=$2; shift 2;; *) exit 2;; esac
done
root=${DEPLOY_ROOT:-/opt/app}; state=${DEPLOY_STATE:-$root/state}; window=${DEPLOY_WINDOW:-15-55}
[[ $cmd =~ ^(preflight|build|deploy|rollback|status)$ ]] || exit 2
if $force && [[ $cmd != rollback ]]; then echo 'force-window is rollback only'; exit 2; fi
if [[ $cmd == status ]]; then
 [[ ! -L $root/current ]] || readlink "$root/current"
 exit 0
fi
fails=0
# Hook diagnostics may contain credentials. Never forward their output.
if [[ -f $hooks ]]; then
 # shellcheck disable=SC1090
 source "$hooks" >/dev/null 2>&1
fi
for f in hook_build hook_health hook_smoke hook_restart hook_edge; do
 declare -F "$f" >/dev/null || { printf 'FAIL missing %s\n' "$f"; fails=$((fails+1)); }
done
for binary in flock sha256sum find sort xargs diff readlink mv ln; do
 command -v "$binary" >/dev/null || { printf 'FAIL missing %s\n' "$binary"; fails=$((fails+1)); }
done
[[ $window =~ ^([0-9]{1,2})-([0-9]{1,2})$ ]] || exit 2
lo=$((10#${BASH_REMATCH[1]})); hi=$((10#${BASH_REMATCH[2]}))
((lo<=hi && hi<=59)) || exit 2
[[ $cmd != preflight ]] || { printf 'FAILs: %s\n' "$fails"; exit "$((fails>0))"; }
((fails==0)) || exit 2
if ! $apply; then printf 'plan: %s release %s\n' "$cmd" "$sha"; exit 0; fi
if [[ $(id -u) != 0 ]]; then
 printf -v environment 'DEPLOY_ROOT=%q DEPLOY_STATE=%q DEPLOY_WINDOW=%q ' "$root" "$state" "$window"
 printf -v line '%q ' "$0" "$cmd" --hooks "$hooks" --sha "$sha" --apply
 line="$environment$line"
 $force && line+='--force-window'
 printf "sudo bash -c '%s'\n" "${line//\'/\'\\\'\'}"; exit 2
fi
umask 077
mkdir -p "$root/releases" "$state"
exec 9>"$state/lock"
flock -n 9 || { echo 'deployment locked'; exit 1; }
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$1" >> "$state/steps.log"; }
run_hook() {
 log "$1 start"
 if "$@" >/dev/null 2>&1; then log "$1 passed"; else log "$1 failed"; return 1; fi
}
guard() {
 local minute
 minute=$((10#$(date -u +%M)))
 if [[ ${DEPLOY_TEST_REFUSE:-0} == 1 ]] || { ! $force && ((minute<lo || minute>hi)); }; then
  log 'window refused'; echo 'window refused'; return 1
 fi
}
valid_release() {
 [[ $1 =~ ^[a-f0-9]{7,64}$ && -d $root/releases/$1 && ! -L $root/releases/$1 ]] || return 1
 if find "$root/releases/$1" -type l | grep -q .; then return 1; fi
 (cd "$root/releases/$1" && diff -q manifest.sha256 <(find . -type f ! -name manifest.sha256 -print0 | sort -z | xargs -0 sha256sum) >/dev/null 2>&1)
}
switch_to() {
 ln -sfn "releases/$1" "$root/.current.$$"
 mv -Tf "$root/.current.$$" "$root/current"
 log "switch $1"
 run_hook hook_restart "$1"
}
health() {
 local actual
 log "health $1 $2 start"
 if actual=$(hook_health "$1" "$2" 2>/dev/null) && [[ $actual == "$1" ]]; then
  log "health $1 $2 passed"
 else log "health $1 $2 failed"; return 1; fi
}
log "$cmd begin"
if [[ $cmd == build ]]; then
 [[ $sha =~ ^[a-f0-9]{7,64}$ && ! -e $root/releases/$sha ]] || exit 2
 stage=$(mktemp -d "$root/.stage.XXXXXX")
 trap '[[ ! -d $stage ]] || { chmod -R u+w "$stage"; rm -rf "$stage"; }' EXIT
 run_hook hook_build "$sha" "$stage" || { log 'build failed'; exit 1; }
 # Hook must explicitly attest code-only; migration/schema files are refused too.
 [[ -f $stage/CODE_ONLY ]] || { echo 'code-only attestation missing'; exit 1; }
 if find "$stage" -iname '*migration*' -o -iname '*schema*' | grep -q .; then echo 'schema changes refused'; exit 1; fi
 if find "$stage" -type l | grep -q .; then echo 'release symlinks refused'; exit 1; fi
 (cd "$stage" && find . -type f ! -name manifest.sha256 -print0 | sort -z | xargs -0 sha256sum > manifest.sha256)
 chown -R 0:0 "$stage"
 mv "$stage" "$root/releases/$sha"; chmod -R a-w "$root/releases/$sha"; log "built $sha"; exit 0
fi
guard
if [[ $cmd == rollback ]]; then
 previous=$(cat "$state/previous")
 valid_release "$previous" || exit 1
 switch_to "$previous" && health "$previous" live && run_hook hook_smoke "$previous" live && run_hook hook_edge "$previous" verify
 log 'rollback complete'; exit 0
fi
valid_release "$sha" || { echo 'release seal invalid'; exit 1; }
[[ -L $root/current ]] || { echo 'seed a verified initial current release first'; exit 2; }
previous=$(basename "$(readlink "$root/current")")
valid_release "$previous" || exit 1
[[ $(readlink "$root/current") == "releases/$previous" ]] || exit 1
[[ $sha != "$previous" ]] || exit 0
# health candidate hook starts a side-port candidate and returns its exact SHA.
health "$sha" candidate && run_hook hook_smoke "$sha" candidate || { log 'canary failed'; exit 1; }
run_hook hook_edge "$previous" snapshot || exit 1
guard
[[ ! -e $state/previous ]] || cp -p "$state/previous" "$state/previous.bak-$(date -u +%Y%m%dT%H%M%S%NZ)"
printf '%s\n' "$previous" > "$state/previous"
if switch_to "$sha" && health "$sha" live && run_hook hook_smoke "$sha" live && run_hook hook_edge "$sha" verify; then
 log 'deploy complete'
else
 log 'post checks failed; automatic rollback'
 switch_to "$previous" && health "$previous" live && run_hook hook_smoke "$previous" live && run_hook hook_edge "$previous" verify || { log 'ROLLBACK FAILED'; exit 1; }
 exit 1
fi
