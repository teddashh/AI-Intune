#!/bin/bash
# Idempotent Autopilot host setup for an Ubuntu VM (OCI cloud-init or SSH).
# Installs Docker and the compose plugin, opens 80/443 in front of Oracle's
# default INPUT REJECT, and starts the Docker+Caddy Hub pack.
set -euo pipefail

fail() { echo "host-setup: $*" >&2; exit 1; }

sslip_from_ip() {
  local ip=$1
  [[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
  printf '%s.sslip.io\n' "${ip//./-}"
}

discover_public_ipv4() {
  local body ip
  body=$(curl -fsS --max-time 5 -H "Authorization: Bearer Oracle" \
    http://169.254.169.254/opc/v2/vnics/ 2>/dev/null) || return 1
  ip=$(printf '%s' "$body" | python3 -c '
import json, sys
data = json.load(sys.stdin)
if isinstance(data, dict):
    data = data.get("data", [])
for nic in data:
    ip = nic.get("publicIp") or nic.get("public-ip") or ""
    if ip:
        print(ip)
        break
') || return 1
  [[ -n "$ip" ]] || return 1
  printf '%s\n' "$ip"
}

# Insert an ACCEPT for tcp/$port immediately before the first REJECT or DROP
# so Oracle Ubuntu images, which REJECT everything except SSH, cannot shadow it.
open_port() {
  local bin=$1 port=$2
  if "$bin" -C INPUT -p tcp -m tcp --dport "$port" -j ACCEPT 2>/dev/null; then
    return 0
  fi
  local n=1 line inserted=0
  while IFS= read -r line; do
    case "$line" in
      *REJECT*|*DROP*)
        "$bin" -I INPUT "$n" -p tcp -m tcp --dport "$port" -j ACCEPT
        inserted=1
        break
        ;;
    esac
    n=$((n + 1))
  done < <("$bin" -S INPUT | tail -n +2)
  if [[ "$inserted" == 0 ]]; then
    "$bin" -A INPUT -p tcp -m tcp --dport "$port" -j ACCEPT
  fi
}

persist_firewall() {
  install -d -m 755 /etc/iptables
  iptables-save > /etc/iptables/rules.v4
  if command -v ip6tables-save >/dev/null 2>&1; then
    ip6tables-save > /etc/iptables/rules.v6 || true
  fi
  if command -v netfilter-persistent >/dev/null 2>&1; then
    # Also load on boot via the persistent service. rules.v4 is already written.
    netfilter-persistent save || echo "host-setup: warning: netfilter-persistent save failed; /etc/iptables/rules.v4 was still written" >&2
  fi
}

resolve_public_host() {
  local ip host
  if [[ "$public_host" != "auto" ]]; then
    printf '%s\n' "$public_host"
    return 0
  fi
  for _ in $(seq 1 60); do
    if ip=$(discover_public_ipv4); then
      host=$(sslip_from_ip "$ip") || fail "metadata public IP is not IPv4"
      printf '%s\n' "$host"
      return 0
    fi
    sleep 5
  done
  fail "could not read the instance public IPv4 from OCI metadata"
}

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  return 0
fi

public_host=""
git_ref="main"
repo="https://github.com/teddashh/AI-Intune.git"
dry_run=0
src="/opt/clawctl/src"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --public-host|--git-ref|--repo|--src)
      [[ $# -ge 2 ]] || fail "missing flag value"
      case "$1" in
        --public-host) public_host=$2 ;;
        --git-ref) git_ref=$2 ;;
        --repo) repo=$2 ;;
        --src) src=$2 ;;
      esac
      shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    *) fail "unknown flag" ;;
  esac
done

[[ -n "$public_host" ]] || fail "--public-host is required (a hostname or auto)"
[[ "$public_host" =~ ^[A-Za-z0-9.-]+$ ]] || fail "invalid --public-host"
[[ "$git_ref" =~ ^[A-Za-z0-9._/-]+$ ]] || fail "invalid --git-ref"
[[ "$git_ref" != *..* ]] || fail "invalid --git-ref"
[[ "$repo" =~ ^https://[A-Za-z0-9._/-]+$ ]] || fail "--repo must be an https URL without credentials"
[[ "$src" =~ ^/[A-Za-z0-9._/-]+$ ]] || fail "invalid --src"
[[ "$src" != *..* ]] || fail "invalid --src"

if [[ "$dry_run" == 1 && "$public_host" == auto ]]; then
  echo "Plan: install Docker Engine and the compose plugin if missing."
  echo "Plan: accept TCP 80 and 443 on INPUT before any REJECT/DROP, then persist with iptables-save and netfilter-persistent."
  echo "Plan: clone $repo ref $git_ref into $src."
  echo "Plan: read the public IPv4 from instance metadata and use <ipv4-dashed>.sslip.io."
  echo "Plan: set CLAWCTL_AUTH_MODE=local and CLAWCTL_PUBLIC_URL=https://<ipv4-dashed>.sslip.io via the Autopilot compose pack."
  echo "WARNING: public URL will use sslip.io. Choose a real domain and update CLAWCTL_PUBLIC_HOST before enrolling machines." >&2
  echo "clawctl-oci-url=https://<ipv4-dashed>.sslip.io"
  exit 0
fi

host=$(resolve_public_host)
case "$host" in
  *.sslip.io)
    echo "WARNING: public URL https://$host uses sslip.io. Choose a real domain and update CLAWCTL_PUBLIC_HOST before enrolling machines." >&2
    ;;
esac

if [[ "$dry_run" == 1 ]]; then
  echo "Plan: install Docker Engine and the compose plugin if missing."
  echo "Plan: accept TCP 80 and 443 on INPUT before any REJECT/DROP, then persist with iptables-save and netfilter-persistent."
  echo "Plan: clone $repo ref $git_ref into $src."
  echo "Plan: set CLAWCTL_AUTH_MODE=local and CLAWCTL_PUBLIC_URL=https://$host via the Autopilot compose pack."
  echo "clawctl-oci-url=https://$host"
  exit 0
fi

[[ "$(id -u)" == 0 ]] || fail "run as root (cloud-init or sudo)"
export DEBIAN_FRONTEND=noninteractive
if command -v flock >/dev/null 2>&1; then
  exec 9>/var/lock/clawctl-oci-setup.lock
  flock 9
fi

if ! command -v netfilter-persistent >/dev/null 2>&1; then
  apt-get update
  apt-get install -y debconf-utils
  debconf-set-selections <<'EOF'
iptables-persistent iptables-persistent/autosave_v4 boolean true
iptables-persistent iptables-persistent/autosave_v6 boolean true
EOF
  apt-get install -y iptables-persistent
fi

open_port iptables 80
open_port iptables 443
if ip6tables -S INPUT >/dev/null 2>&1; then
  open_port ip6tables 80 || true
  open_port ip6tables 443 || true
fi
persist_firewall

if ! docker compose version >/dev/null 2>&1; then
  apt-get update
  apt-get install -y ca-certificates curl git python3 gnupg
  install -m 0755 -d /etc/apt/keyrings
  if [[ ! -f /etc/apt/keyrings/docker.asc ]]; then
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc
  fi
  # shellcheck disable=SC1091
  . /etc/os-release
  arch=$(dpkg --print-architecture)
  printf 'deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu %s stable\n' \
    "$arch" "$VERSION_CODENAME" > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
  systemctl enable --now docker
fi
command -v git >/dev/null || { apt-get update; apt-get install -y git ca-certificates; }
command -v curl >/dev/null || { apt-get update; apt-get install -y curl ca-certificates; }
command -v python3 >/dev/null || { apt-get update; apt-get install -y python3; }
docker compose version >/dev/null || fail "docker compose plugin is not available"

install -d -m 755 "$(dirname "$src")"
if [[ ! -d "$src/.git" ]]; then
  git clone --depth 1 --branch "$git_ref" "$repo" "$src"
else
  git -C "$src" remote set-url origin "$repo"
  git -C "$src" fetch --depth 1 origin "$git_ref"
  git -C "$src" checkout --detach FETCH_HEAD
fi
version=$(git -C "$src" rev-parse --short HEAD)
[[ "$version" =~ ^[A-Za-z0-9._-]+$ ]] || fail "refused to record an unusual git version"

umask 077
cat > "$src/ops/docker/autopilot.env" <<EOF
# Written by ops/oci/host-setup.sh. Do not commit.
# The compose pack sets CLAWCTL_AUTH_MODE=local and
# CLAWCTL_PUBLIC_URL=https://\$CLAWCTL_PUBLIC_HOST
CLAWCTL_PUBLIC_HOST=$host
CLAWCTL_VERSION=$version
EOF
chmod 600 "$src/ops/docker/autopilot.env"

compose=(docker compose -p clawctl-autopilot --env-file "$src/ops/docker/autopilot.env" -f "$src/ops/docker/docker-compose.autopilot.yml")
config=$("${compose[@]}" config)
printf '%s\n' "$config" | grep -q 'CLAWCTL_AUTH_MODE: local' || fail "compose did not set CLAWCTL_AUTH_MODE=local"
printf '%s\n' "$config" | grep -q "CLAWCTL_PUBLIC_URL: https://$host" || fail "compose did not set CLAWCTL_PUBLIC_URL"

"${compose[@]}" up -d --build

install -d -m 700 /var/lib/clawctl
umask 077
printf 'CLAWCTL_PUBLIC_URL=https://%s\n' "$host" > /var/lib/clawctl/result.env
chmod 600 /var/lib/clawctl/result.env

deadline=$((SECONDS + 300))
until curl -fsS --max-time 10 "https://$host/healthz" >/dev/null; do
  if (( SECONDS >= deadline )); then
    echo "host-setup: HTTPS health check timed out for https://$host/healthz" >&2
    echo "host-setup: check the security list, the host iptables REJECT rule, DNS, and Caddy ACME logs." >&2
    exit 1
  fi
  sleep 3
done

echo "clawctl-oci-url=https://$host"
status=$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 10 "https://$host/setup") || status=000
case "$status" in
  200)
    logs=$("${compose[@]}" logs hub 2>/dev/null || true)
    code=$(printf '%s\n' "$logs" | sed -n 's/.*enter setup code \([^[:space:]]*\).*/\1/p' | tail -n 1)
    if [[ -n "$code" ]]; then
      printf '%s\n' "$code" > /var/lib/clawctl/setup-code
      chmod 600 /var/lib/clawctl/setup-code
      printf 'clawctl-setup-code=%s\n' "$code"
    else
      echo "No generated setup code found; use your configured code."
    fi
    ;;
  404) echo "Setup is already closed; use /login." ;;
  *) fail "unexpected setup HTTP status" ;;
esac
echo "Next: open https://$host/setup or run ops/oci/install-hub.sh with --admin-user so it can call ops/fly/setup-admin.sh."
echo "Choose a real domain before enrolling machines if this URL is a sslip.io name."
