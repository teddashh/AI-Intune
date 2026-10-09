#!/bin/bash
# Idempotent Hub install for an existing Ubuntu VM over SSH.
# Reuses ops/fly/setup-admin.sh for the first admin and TOTP enrollment.
set -euo pipefail

host=""
ssh_user="ubuntu"
identity=""
public_host=""
git_ref="main"
repo="https://github.com/teddashh/AI-Intune.git"
admin_user=""
admin_out=""
dry_run=0

fail() { echo "install-hub: $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host|--user|--identity|--public-host|--git-ref|--repo|--admin-user|--admin-out)
      [[ $# -ge 2 ]] || fail "missing flag value"
      case "$1" in
        --host) host=$2 ;;
        --user) ssh_user=$2 ;;
        --identity) identity=$2 ;;
        --public-host) public_host=$2 ;;
        --git-ref) git_ref=$2 ;;
        --repo) repo=$2 ;;
        --admin-user) admin_user=$2 ;;
        --admin-out) admin_out=$2 ;;
      esac
      shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help)
      echo "Usage: ops/oci/install-hub.sh --host ADDRESS [--public-host HOST] [--admin-user USER --admin-out FILE]"
      exit 0 ;;
    *) fail "unknown flag" ;;
  esac
done

[[ "$host" =~ ^[A-Za-z0-9.-]+$ && "$host" != *..* && "$host" != -* ]] || fail "invalid --host"
[[ "$ssh_user" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || fail "invalid --user"
[[ "$git_ref" =~ ^[A-Za-z0-9._/-]+$ && "$git_ref" != *..* ]] || fail "invalid --git-ref"
[[ "$repo" =~ ^https://[A-Za-z0-9._/-]+$ ]] || fail "--repo must be an https URL without credentials"
if [[ -n "$public_host" ]]; then
  [[ "$public_host" =~ ^[A-Za-z0-9.-]+$ && "$public_host" != *..* ]] || fail "invalid --public-host"
fi
if [[ -n "$admin_user" || -n "$admin_out" ]]; then
  [[ -n "$admin_user" && -n "$admin_out" ]] || fail "--admin-user and --admin-out are both required"
  [[ "$admin_user" =~ ^[A-Za-z0-9._-]{3,64}$ ]] || fail "invalid --admin-user"
  [[ ! -e "$admin_out" && ! -L "$admin_out" ]] || fail "admin output file already exists"
fi
if [[ -n "$identity" ]]; then
  [[ -f "$identity" ]] || fail "identity file must be a regular file"
fi

if [[ -z "$public_host" ]]; then
  if [[ "$host" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
    public_host=${host//./-}.sslip.io
    echo "WARNING: public URL https://$public_host uses sslip.io. Choose a real domain before enrolling machines." >&2
  else
    public_host=$host
  fi
fi

root=$(cd "$(dirname "$0")/../.." && pwd)
setup_admin=${CLAWCTL_SETUP_ADMIN:-$root/ops/fly/setup-admin.sh}
[[ -f "$root/ops/oci/host-setup.sh" ]] || fail "missing host-setup.sh"
[[ -f "$setup_admin" ]] || fail "missing setup-admin.sh"

ssh_base=(ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 -o ServerAliveInterval=30)
if [[ -n "$identity" ]]; then
  ssh_base+=(-i "$identity" -o IdentitiesOnly=yes)
fi

if [[ "$dry_run" == 1 ]]; then
  echo "Plan: ssh ${ssh_user}@${host} and wait for cloud-init if it is installed."
  echo "Plan: run ops/oci/host-setup.sh --public-host $public_host --git-ref $git_ref"
  echo "Plan: set CLAWCTL_AUTH_MODE=local and CLAWCTL_PUBLIC_URL=https://$public_host"
  echo "Plan: open TCP 80 and 443 before the INPUT REJECT rule and persist them."
  if [[ -n "$admin_user" ]]; then
    echo "Plan: call ops/fly/setup-admin.sh --url https://$public_host --username $admin_user --out $admin_out"
    echo "Plan: do not print the setup code, password, TOTP secret, or recovery codes."
  fi
  echo "No changes were made."
  exit 0
fi

command -v curl >/dev/null || fail "curl not found on PATH"
command -v python3 >/dev/null || fail "python3 not found on PATH"
umask 077
work=$(mktemp -d)
chmod 700 "$work"
trap 'rm -rf "$work"' EXIT

target="${ssh_user}@${host}"
"${ssh_base[@]}" "$target" "if command -v cloud-init >/dev/null 2>&1; then sudo -n cloud-init status --wait || true; fi" \
  >"$work/cloud-init" 2>&1 || fail "ssh failed while waiting for cloud-init"

remote="sudo -n bash -s -- --public-host ${public_host@Q} --git-ref ${git_ref@Q} --repo ${repo@Q}"
if ! "${ssh_base[@]}" "$target" "$remote" < "$root/ops/oci/host-setup.sh" >"$work/remote" 2>"$work/remote.err"; then
  python3 - "$work/remote.err" <<'PY' >&2
import sys
for line in open(sys.argv[1], errors="replace"):
    low = line.lower()
    if "private key" in low or "begin openssh" in low or "begin rsa" in low:
        print("[redacted]")
        continue
    if len(line) > 400:
        print("[redacted long line]")
        continue
    sys.stdout.write(line)
PY
  fail "remote setup failed"
fi
grep -v '^clawctl-setup-code=' "$work/remote" || true

url=$(sed -n 's/^clawctl-oci-url=//p' "$work/remote" | tail -n 1)
[[ -n "$url" ]] || url="https://$public_host"
code=$(sed -n 's/^clawctl-setup-code=//p' "$work/remote" | tail -n 1)
case "$url" in
  https://*) ;;
  *) fail "remote setup did not report an https URL" ;;
esac

deadline=$((SECONDS + 300))
while :; do
  status=$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 10 "$url/healthz") || status=000
  [[ "$status" == 200 ]] && break
  if (( SECONDS >= deadline )); then
    fail "HTTPS health check timed out for $url/healthz"
  fi
  sleep 3
done
echo "$url"

status=$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 10 "$url/setup") || status=000
case "$status" in
  200)
    if [[ -n "$admin_user" ]]; then
      [[ -n "$code" ]] || fail "setup is open but no setup code was found"
      printf '%s\n' "$code" > "$work/setup-code"
      chmod 600 "$work/setup-code"
      "$setup_admin" --url "$url" --username "$admin_user" --setup-code-file "$work/setup-code" --out "$admin_out"
    elif [[ -n "$code" ]]; then
      printf 'Setup code: %s\n' "$code"
      echo "Next: ops/fly/setup-admin.sh --url $url --username <admin> --setup-code-file <private-file> --out <new-0600-file>"
    else
      echo "No generated setup code found; use your configured code."
    fi
    ;;
  404)
    echo "Setup is already closed; use /login."
    ;;
  *) fail "unexpected setup HTTP status" ;;
esac
echo "Choose a real domain before enrolling machines if this URL is a sslip.io name."
