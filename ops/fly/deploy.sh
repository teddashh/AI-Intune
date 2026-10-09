#!/bin/bash
# Deploy the public HTTPS pack without changing the checked-in config.
set -euo pipefail
org=${FLY_ORG:-}
app=${FLY_APP:-}
region=${FLY_REGION:-iad}
url=${FLY_PUBLIC_URL:-}
public_url_given=0
[[ -z "$url" ]] || public_url_given=1
secrets_file=${FLY_SECRETS_FILE:-}
dry_run=0
destroy=0
fail() { echo "deploy: $*" >&2; exit 1; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --org|--app|--region|--secrets-file|--public-url)
      [[ $# -ge 2 ]] || fail "missing flag value"
      case "$1" in
        --org) org=$2 ;; --app) app=$2 ;; --region) region=$2 ;; --secrets-file) secrets_file=$2 ;;
        --public-url) url=$2; public_url_given=1 ;;
      esac
      shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    --destroy) destroy=1; shift ;;
    *) fail "unknown flag" ;;
  esac
done
[[ "$app" =~ ^[a-z0-9-]{1,63}$ ]] || fail "invalid or empty app name"
[[ "$region" =~ ^[a-z0-9]+$ ]] || fail "invalid or empty region"
if [[ "$destroy" == 0 ]]; then
  [[ "$org" =~ ^[a-z0-9-]+$ ]] || fail "invalid or empty org"
fi
if [[ "$public_url_given" == 1 ]]; then
  url=${url%/}
  dns_label='[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?'
  [[ "$url" =~ ^https://$dns_label(\.$dns_label)*(:[0-9]{1,5})?$ ]] || fail "public URL must be https://<dns-host>[:port] with no path, query, fragment, or userinfo"
else
  url="https://$app.fly.dev"
fi
fly=$(command -v fly || command -v flyctl) || fail "fly or flyctl not found on PATH"
command -v python3 >/dev/null || fail "python3 not found on PATH"
if [[ -n "$secrets_file" ]]; then
  [[ -f "$secrets_file" ]] || fail "secrets file must be a regular file"
  mode=$(python3 - "$secrets_file" <<'PY'
import os, stat, sys
print(oct(stat.S_IMODE(os.stat(sys.argv[1]).st_mode)))
PY
)
  [[ "$mode" == 0o600 ]] || fail "secrets file must have mode 0600"
  secrets_file=$(python3 - "$secrets_file" <<'PY2'
import os, sys
print(os.path.abspath(sys.argv[1]))
PY2
)
fi
plan() { printf '%q ' "$fly" "$@"; printf '\n'; }

# Print a captured Fly log without ever writing the API token.
show_log() {
  python3 - "$1" <<'PY'
import os, sys
token = os.environ.get("FLY_API_TOKEN", "")
with open(sys.argv[1], errors="replace") as handle:
    for line in handle:
        if token and token in line:
            print("[redacted]")
        else:
            sys.stdout.write(line)
PY
}
if [[ "$dry_run" == 1 ]]; then
  if [[ "$destroy" == 1 ]]; then
    echo "Plan: require typed app name, destroy its clawctl_data volumes and app (including machines)."
    plan machines list -a "$app" --json
    plan machines destroy '<machine-id>' -a "$app" --force
    plan volumes list -a "$app" --json
    plan volumes destroy '<clawctl_data-volume-id>' -a "$app" --yes
    plan apps destroy "$app" --yes
  else
    echo "Plan: create only missing resources; one 1024mb machine and a 3GB volume. Config copied outside the repo."
    plan apps create "$app" --org "$org"
    plan volumes create clawctl_data --region "$region" --size 3 -a "$app" --yes
    if [[ -n "$secrets_file" ]]; then plan secrets import --stage -a "$app"; echo "Read secrets from the supplied file via stdin."; fi
    plan deploy . --config '<temporary-config>' --dockerfile ops/docker/Dockerfile --build-target hub-fly --build-arg 'CLAWCTL_VERSION=<git-short-HEAD>' --ha=false --yes
    echo "On Depot/handshake/list workers failure: retry once with --depot=false; remove only newly created fly-builder-* apps."
    plan scale count 1 -a "$app" --yes
    echo "Wait for $url/healthz; check /setup before reading a first-run code."
  fi
  exit 0
fi
[[ -n "${FLY_API_TOKEN:-}" ]] || "$fly" auth whoami >/dev/null 2>&1 || fail "Fly authentication required"
work=$(mktemp -d)
chmod 700 "$work"
trap 'rm -rf "$work"' EXIT
# Read Fly JSON without relying on table formatting.
json_items() {
  python3 - "$1" "$2" <<'PY'
import json, sys
items = json.load(open(sys.argv[1]))
if not isinstance(items, list):
    raise ValueError('expected Fly JSON array')
for item in items:
    name = item.get('Name', item.get('name', ''))
    if sys.argv[2] == 'apps':
        print(name)
    elif sys.argv[2] == 'machines':
        print(item.get('ID', item.get('id', '')))
    elif name == 'clawctl_data':
        print(item.get('ID', item.get('id', '')))
PY
}
if [[ "$destroy" == 1 ]]; then
  printf 'Type the app name to destroy %s: ' "$app" >&2
  confirmation=''
  IFS= read -r confirmation || [[ -n "$confirmation" ]] || fail "confirmation required"
  [[ "$confirmation" == "$app" ]] || fail "confirmation did not match; nothing deleted"
  "$fly" machines list -a "$app" --json > "$work/machines"
  json_items "$work/machines" machines > "$work/machine-ids"
  while IFS= read -r id; do
    [[ -n "$id" ]] || fail "invalid machine inventory"
    "$fly" machines destroy "$id" -a "$app" --force >/dev/null 2>&1 || fail "machine destroy failed"
  done < "$work/machine-ids"
  "$fly" volumes list -a "$app" --json > "$work/volumes"
  json_items "$work/volumes" volumes > "$work/ids"
  while IFS= read -r id; do
    [[ -n "$id" ]] || fail "invalid volume inventory"
    destroyed=0
    for _ in 1 2 3 4 5 6; do
      if "$fly" volumes destroy "$id" -a "$app" --yes >/dev/null 2>&1; then
        destroyed=1
        break
      fi
      sleep 2
    done
    [[ "$destroyed" == 1 ]] || fail "volume destroy failed"
  done < "$work/ids"
  "$fly" apps destroy "$app" --yes >/dev/null 2>&1 || fail "app destroy failed"
  echo "Destroyed $app and its clawctl_data volumes."
  exit 0
fi
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"
command -v curl >/dev/null || fail "curl not found on PATH"
sed -e "s/^app = .*/app = \"$app\"/" -e "s/^primary_region = .*/primary_region = \"$region\"/" ops/fly/fly.toml > "$work/fly.toml"
"$fly" apps list --json > "$work/apps"
json_items "$work/apps" apps > "$work/names"
if ! grep -Fxq "$app" "$work/names"; then
  "$fly" apps create "$app" --org "$org" >/dev/null 2>&1 || fail "app creation failed"
fi
"$fly" volumes list -a "$app" --json > "$work/volumes"
json_items "$work/volumes" volumes > "$work/ids"
if [[ ! -s "$work/ids" ]]; then
  "$fly" volumes create clawctl_data --region "$region" --size 3 -a "$app" --yes >/dev/null 2>&1 || fail "volume creation failed"
fi
if [[ -n "$secrets_file" ]]; then
  "$fly" secrets import --stage -a "$app" < "$secrets_file" > "$work/import" 2>&1 || fail "secrets import failed"
fi
args=(deploy . --config "$work/fly.toml" --dockerfile ops/docker/Dockerfile --build-target hub-fly --build-arg "CLAWCTL_VERSION=$(git rev-parse --short HEAD)" --ha=false --yes)
if ! "$fly" "${args[@]}" > "$work/deploy" 2>&1; then
  show_log "$work/deploy" >&2
  grep -Eiq 'depot|handshake|list workers' "$work/deploy" || fail "deploy failed"
  echo "Depot failed; retrying with the classic builder."
  "$fly" apps list --json > "$work/before"
  json_items "$work/before" apps > "$work/before-names"
  retry_failed=0
  if ! "$fly" "${args[@]}" --depot=false > "$work/retry" 2>&1; then
    show_log "$work/retry" >&2
    retry_failed=1
  fi
  "$fly" apps list --json > "$work/after" || fail "cannot inventory classic builders; check for leftover fly-builder apps"
  json_items "$work/after" apps > "$work/after-names"
  cleanup_failed=0
  while IFS= read -r name; do
    if [[ "$name" == fly-builder-* ]] && ! grep -Fxq "$name" "$work/before-names"; then
      if ! "$fly" apps destroy "$name" --yes >/dev/null 2>&1; then
        echo "Warning: builder teardown failed for $name" >&2
        cleanup_failed=1
      fi
    fi
  done < "$work/after-names"
  [[ "$cleanup_failed" == 0 ]] || fail "builder teardown incomplete"
  [[ "$retry_failed" == 0 ]] || fail "classic builder deploy failed (output withheld to protect secrets)"
fi
"$fly" scale count 1 -a "$app" --yes >/dev/null 2>&1 || fail "scale failed"
deadline=$((SECONDS + 180))
while :; do
  status=$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 10 "$url/healthz") || status=000
  [[ "$status" == 200 ]] && break
  (( SECONDS < deadline )) || fail "health check timed out"
  sleep 3
done
echo "$url"
status=$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 10 "$url/setup") || fail "setup check failed"
case "$status" in
  200)
    "$fly" logs -a "$app" --no-tail > "$work/logs" 2>&1 || fail "cannot read setup logs"
    code=$(sed -n 's|.*first-run setup: open https://[^[:space:]]*/setup and enter setup code \([^[:space:]]*\).*|\1|p' "$work/logs" | tail -n 1)
    if [[ -n "$code" ]]; then printf 'Setup code: %s\n' "$code"; else echo "No generated setup code found; use your configured code."; fi ;;
  404) echo "Setup is already closed; use /login." ;;
  421)
    if [[ "$public_url_given" == 1 ]]; then
      fail "setup HTTP 421: --public-url / FLY_PUBLIC_URL must match the configured CLAWCTL_PUBLIC_URL"
    fi
    echo "fly.dev refuses operator routes (HTTP 421): CLAWCTL_PUBLIC_URL is a custom domain. Check https://<your-domain>/setup or rerun the checks with --public-url."
    exit 0 ;;
  *) fail "unexpected setup HTTP status" ;;
esac
echo "Next: open $url/setup or use ops/fly/setup-admin.sh for first-run setup."
echo "R2 is optional. Choose a custom domain before enrolling machines. Do not allocate a dedicated IPv4."
