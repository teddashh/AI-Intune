#!/usr/bin/env bash

set -Eeuo pipefail

HUB=""
TOKEN=""
TOKEN_FILE=""
TAILSCALE_KEY_FILE=""
BIN_SRC=""
STEP="preflight"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
SECRET_FILES=()
REENROLL=0
BACKUP_CONFIG=""

usage() {
  cat <<'EOF'
Usage: ./install-agent.sh --hub URL [options]

  --hub URL
      http://<tailscale-ipv4>:<port>  (same address as the operator UI)
      https://<hostname>[:port]       (experimental Cloudflare Tunnel; agent check-in only)

Options:
  --token TOKEN                    one-time enrollment token
  --token-file FILE                0600 file containing the enrollment token
  --tailscale-auth-key-file FILE   0600 file containing a Tailscale auth key
  --binary FILE                    clawctl-agent binary for this machine
  --reenroll                       reenroll this machine to a new Hub (Linux only)
EOF
}

fail() {
  echo "$1" >&2
  if [[ -n "$BACKUP_CONFIG" && -f "$BACKUP_CONFIG" ]]; then
    echo "Previous configuration backed up at: $BACKUP_CONFIG (see docs/MOVE-AGENTS.md, Rollback)" >&2
  fi
  exit 1
}

on_error() {
  local rc=$?
  echo "Install failed: $STEP" >&2
  if [[ -n "$BACKUP_CONFIG" && -f "$BACKUP_CONFIG" ]]; then
    echo "Previous configuration backed up at: $BACKUP_CONFIG (see docs/MOVE-AGENTS.md, Rollback)" >&2
  fi
  exit "$rc"
}

cleanup() {
  local path
  for path in "${SECRET_FILES[@]}"; do
    [[ -n "$path" ]] && rm -f -- "$path"
  done
}

trap on_error ERR
trap cleanup EXIT

agent_hub_octet() {
  local o="$1"
  [[ "$o" =~ ^[0-9]+$ ]] || return 1
  if [[ ${#o} -gt 1 && "${o:0:1}" == 0 ]]; then
    return 1
  fi
  (( 10#$o <= 255 )) || return 1
  return 0
}

agent_hub_port() {
  local port="$1"
  [[ "$port" =~ ^[0-9]+$ ]] || return 1
  if [[ ${#port} -gt 1 && "${port:0:1}" == 0 ]]; then
    return 1
  fi
  (( 10#$port >= 1 && 10#$port <= 65535 )) || return 1
  return 0
}

# http:// is the operator UI address: a literal Tailscale IPv4 and an explicit port.
# https:// is an experimental Cloudflare Tunnel for agent check-in only.
accept_agent_hub_url() {
  local rest ip port host o1 o2 o3 o4 extra
  extra=""
  case "$HUB" in
    *[@?#]*|*' '*|*$'\t'*|*$'\n'*|*$'\r'*) fail "--hub must not include userinfo, a query, or a fragment" ;;
  esac
  case "$HUB" in
    */) HUB="${HUB%/}" ;;
  esac
  case "$HUB" in
    http://*)
      rest="${HUB#http://}"
      case "$rest" in
        */*) fail "--hub must not include a path" ;;
        *:*) ip="${rest%:*}"; port="${rest##*:}" ;;
        *) fail "--hub http URL must be a Tailscale IPv4 and an explicit port" ;;
      esac
      case "$ip" in
        *:*|*']'*) fail "--hub http URL must be a Tailscale IPv4 and an explicit port" ;;
      esac
      IFS=. read -r o1 o2 o3 o4 extra <<< "$ip"
      [[ -z "$extra" && -n "$o1" && -n "$o2" && -n "$o3" && -n "$o4" ]] || fail "--hub http URL must be a Tailscale IPv4 and an explicit port"
      agent_hub_octet "$o1" || fail "--hub http URL must be a Tailscale IPv4 and an explicit port"
      agent_hub_octet "$o2" || fail "--hub http URL must be a Tailscale IPv4 and an explicit port"
      agent_hub_octet "$o3" || fail "--hub http URL must be a Tailscale IPv4 and an explicit port"
      agent_hub_octet "$o4" || fail "--hub http URL must be a Tailscale IPv4 and an explicit port"
      agent_hub_port "$port" || fail "--hub http URL must be a Tailscale IPv4 and an explicit port"
      if [[ "$o1" != 100 ]] || (( 10#$o2 < 64 || 10#$o2 > 127 )); then
        fail "--hub http URL must be a Tailscale node IPv4"
      fi
      if [[ "$ip" == "100.100.100.100" || ( "$o2" == 115 && ( "$o3" == 92 || "$o3" == 93 ) ) ]]; then
        fail "--hub http URL must be a Tailscale node IPv4"
      fi
      HUB="http://${o1}.${o2}.${o3}.${o4}:${port}"
      ;;
    https://*)
      rest="${HUB#https://}"
      case "$rest" in
        */*) fail "--hub must not include a path" ;;
        *:*) host="${rest%:*}"; port="${rest##*:}" ;;
        *) host="$rest"; port="" ;;
      esac
      [[ -n "$host" && "$host" != *:* && "$host" != *'['* ]] || fail "--hub https URL must be a DNS hostname"
      host="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')"
      [[ "$host" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$ ]] || fail "--hub https URL must be a DNS hostname"
      [[ "$host" =~ [a-z] ]] || fail "--hub https URL must be a DNS hostname"
      case ".$host." in
        *.localhost.*) fail "--hub https URL must be a DNS hostname" ;;
      esac
      if [[ -n "$port" ]]; then
        agent_hub_port "$port" || fail "--hub https URL must be a DNS hostname and an explicit port"
        HUB="https://${host}:${port}"
      else
        HUB="https://${host}"
      fi
      ;;
    *)
      fail "--hub must be http://<tailscale-ipv4>:<port> or https://<hostname>"
      ;;
  esac
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --hub) [[ $# -ge 2 ]] || fail "--hub requires a value"; HUB="$2"; shift 2 ;;
    --token) [[ $# -ge 2 ]] || fail "--token requires a value"; TOKEN="$2"; shift 2 ;;
    --token-file) [[ $# -ge 2 ]] || fail "--token-file requires a value"; TOKEN_FILE="$2"; shift 2 ;;
    --tailscale-auth-key-file) [[ $# -ge 2 ]] || fail "--tailscale-auth-key-file requires a value"; TAILSCALE_KEY_FILE="$2"; shift 2 ;;
    --binary) [[ $# -ge 2 ]] || fail "--binary requires a value"; BIN_SRC="$2"; shift 2 ;;
    --reenroll) REENROLL=1; shift 1 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "$HUB" ]] || { usage >&2; exit 2; }
accept_agent_hub_url
# Ops test hook: validate --hub and stop. Not an operator feature.
if [[ "${CLAWCTL_INSTALL_AGENT_CHECK_HUB:-}" == "1" ]]; then
  printf '%s\n' "$HUB"
  exit 0
fi
[[ $EUID -ne 0 ]] || fail "Run this installer as the account that will run clawctl-agent"
AGENT_USER="$(id -un)"
UNAME_S="$(uname -s)"
if [[ "$UNAME_S" != Linux ]]; then
  [[ "$UNAME_S" != Darwin ]] || fail "This installer is for Linux. On macOS run ops/install-agent-macos.sh --hub $HUB"
  case "$UNAME_S" in
    MINGW*|MSYS*|CYGWIN*|Windows_NT)
      fail "This installer is for Linux. On Windows run ops/install-agent-windows.ps1 --hub $HUB"
      ;;
  esac
  fail "This installer requires Linux"
fi
command -v systemctl >/dev/null || fail "systemd is required"
command -v loginctl >/dev/null || fail "systemd-logind is required"
command -v sudo >/dev/null || fail "sudo is required"
[[ -z "$TOKEN" || -z "$TOKEN_FILE" ]] || fail "Use only one of --token or --token-file"

case "$(uname -m)" in
  x86_64|amd64) AGENT_ARCH="amd64" ;;
  aarch64|arm64) AGENT_ARCH="arm64" ;;
  *) fail "Unsupported architecture: $(uname -m)" ;;
esac

if [[ -z "$BIN_SRC" ]]; then
  for candidate in \
    "$SCRIPT_DIR/clawctl-agent" \
    "$SCRIPT_DIR/../build/clawctl-agent-linux-$AGENT_ARCH" \
    "$SCRIPT_DIR/../build/clawctl-agent"; do
    if [[ -f "$candidate" ]]; then
      BIN_SRC="$candidate"
      break
    fi
  done
fi
[[ -n "$BIN_SRC" && -f "$BIN_SRC" && ! -L "$BIN_SRC" ]] || fail "clawctl-agent binary not found; use --binary FILE"
[[ -f "$SCRIPT_DIR/clawctl-agent.service" && ! -L "$SCRIPT_DIR/clawctl-agent.service" ]] || fail "clawctl-agent.service not found"
[[ -f "$SCRIPT_DIR/clawctl-hermes.service" && ! -L "$SCRIPT_DIR/clawctl-hermes.service" ]] || fail "clawctl-hermes.service not found"
[[ -f "$SCRIPT_DIR/openclaw-gateway.service" && ! -L "$SCRIPT_DIR/openclaw-gateway.service" ]] || fail "openclaw-gateway.service not found"

private_secret_file() {
  local path=$1 mode
  [[ -f "$path" && ! -L "$path" ]] || fail "Secret file must be a regular file: $path"
  mode="$(stat -c '%a' "$path")"
  [[ "$mode" == "600" || "$mode" == "400" ]] || fail "Secret file must have mode 0600 or 0400: $path"
}

new_secret_file() {
  local value=$1 output_name=$2 path
  path="$(mktemp)"
  chmod 0600 "$path"
  printf '%s\n' "$value" >"$path"
  SECRET_FILES+=("$path")
  printf -v "$output_name" '%s' "$path"
}

render_agent_unit() {
  local source=$1 destination=$2 agent_user agent_uid escaped_home
  agent_user="$AGENT_USER"
  agent_uid="$(id -u)"
  [[ "$agent_user" =~ ^[a-zA-Z_][a-zA-Z0-9_.-]*\$?$ ]] || fail "Unsupported agent user name: $agent_user"
  [[ "$agent_uid" =~ ^[1-9][0-9]*$ ]] || fail "Agent must use a non-root numeric UID"
  [[ "$HOME" =~ ^/[a-zA-Z0-9._/-]+$ && "$HOME" != *"/../"* && "$HOME" != */.. ]] ||
    fail "Agent home contains characters that cannot be safely rendered into systemd: $HOME"
  escaped_home=${HOME//\\/\\\\}
  escaped_home=${escaped_home//&/\\&}
  sed -e "s|@@CLAWCTL_AGENT_USER@@|$agent_user|g" \
      -e "s|@@CLAWCTL_AGENT_UID@@|$agent_uid|g" \
      -e "s|__CLAWCTL_AGENT_UID__|$agent_uid|g" \
      -e "s|@@CLAWCTL_AGENT_HOME@@|$escaped_home|g" \
      "$source" >"$destination"
  ! grep -Fq 'CLAWCTL_AGENT_' "$destination" || fail "Agent systemd unit rendering is incomplete"
}

ensure_owned_directory() {
  local path=$1 mode=$2
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -d "$path" && ! -L "$path" ]] || fail "Directory path is not safe: $path"
    [[ "$(stat -c '%u' "$path")" == "$EUID" ]] || fail "Directory owner is not $USER: $path"
  else
    mkdir "$path"
  fi
  chmod "$mode" "$path"
}

ensure_home_ancestor() {
  local path=$1
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -d "$path" && ! -L "$path" ]] || fail "Home path is not safe: $path"
    [[ "$(stat -c '%u' "$path")" == "$EUID" ]] || fail "Home path owner is not $USER: $path"
  else
    mkdir "$path"
  fi
}

install_download_client() {
  if command -v curl >/dev/null; then
    return
  elif command -v apt-get >/dev/null; then
    sudo env DEBIAN_FRONTEND=noninteractive apt-get update
    sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y curl ca-certificates
  elif command -v dnf >/dev/null; then
    sudo dnf install -y curl ca-certificates
  elif command -v yum >/dev/null; then
    sudo yum install -y curl ca-certificates
  elif command -v zypper >/dev/null; then
    sudo zypper --non-interactive install curl ca-certificates
  elif command -v pacman >/dev/null; then
    sudo pacman -Sy --noconfirm curl ca-certificates
  else
    fail "Install curl and run this installer again"
  fi
}

install_container_runtime() {
  if [[ ! -x /usr/bin/podman ]] || ! command -v newuidmap >/dev/null || ! command -v newgidmap >/dev/null; then
    if command -v apt-get >/dev/null; then
      sudo env DEBIAN_FRONTEND=noninteractive apt-get update
      sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y podman uidmap slirp4netns fuse-overlayfs
    elif command -v dnf >/dev/null; then
      sudo dnf install -y podman shadow-utils slirp4netns fuse-overlayfs
    elif command -v yum >/dev/null; then
      sudo yum install -y podman shadow-utils slirp4netns fuse-overlayfs
    elif command -v zypper >/dev/null; then
      sudo zypper --non-interactive install podman shadow
    elif command -v pacman >/dev/null; then
      sudo pacman -Sy --noconfirm podman shadow slirp4netns fuse-overlayfs
    else
      fail "A supported package manager is required"
    fi
  fi
  [[ -x /usr/bin/podman ]] || fail "podman is not installed at /usr/bin/podman"
  command -v newuidmap >/dev/null || fail "newuidmap is not installed"
  command -v newgidmap >/dev/null || fail "newgidmap is not installed"
}

ensure_subordinate_ids() {
  sudo env TARGET_USER="$USER" bash -c '
    set -Eeuo pipefail
    exec 9>/run/lock/clawctl-subids.lock
    flock 9
    touch /etc/subuid /etc/subgid
    has_range() {
      awk -F: -v user="$TARGET_USER" '\''$1 == user && $3 >= 65536 { found=1 } END { exit !found }'\'' "$1"
    }
    need_uid=1
    need_gid=1
    has_range /etc/subuid && need_uid=0
    has_range /etc/subgid && need_gid=0
    (( need_uid || need_gid )) || exit 0
    first=100000
    count=65536
    while :; do
      last=$((first + count - 1))
      (( last < 2147483647 )) || { echo "No subordinate ID range is available" >&2; exit 1; }
      if ! awk -F: -v first="$first" -v last="$last" '\''
        NF >= 3 && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ {
          end=$2+$3-1
          if ($2 <= last && end >= first) found=1
        }
        END { exit !found }
      '\'' /etc/subuid /etc/subgid; then
        break
      fi
      first=$((first + count))
    done
    last=$((first + count - 1))
    if (( need_uid )); then
      usermod --add-subuids "$first-$last" "$TARGET_USER"
    fi
    if (( need_gid )); then
      usermod --add-subgids "$first-$last" "$TARGET_USER"
    fi
  '
}

tailscale_connected() {
  sudo tailscale ip -4 2>/dev/null | awk 'NF { found=1 } END { exit !found }'
}

STEP="privilege check"
sudo -v

STEP="container runtime"
install_container_runtime
ensure_subordinate_ids

STEP="Tailscale install"
if ! command -v tailscale >/dev/null; then
  install_download_client
  TAILSCALE_INSTALLER="$(mktemp)"
  SECRET_FILES+=("$TAILSCALE_INSTALLER")
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    https://tailscale.com/install.sh --output "$TAILSCALE_INSTALLER"
  sudo sh "$TAILSCALE_INSTALLER"
fi
sudo systemctl enable --now tailscaled.service

STEP="Tailscale connect"
if ! tailscale_connected; then
  if [[ -n "$TAILSCALE_KEY_FILE" ]]; then
    private_secret_file "$TAILSCALE_KEY_FILE"
  elif [[ -t 0 ]]; then
    read -r -s -p "Tailscale auth key: " TAILSCALE_KEY
    echo
    [[ -n "$TAILSCALE_KEY" ]] || fail "Tailscale auth key is required"
    new_secret_file "$TAILSCALE_KEY" TAILSCALE_KEY_FILE
    unset TAILSCALE_KEY
  else
    fail "Tailscale is not connected; use --tailscale-auth-key-file FILE"
  fi
  sudo tailscale up --auth-key="file:$TAILSCALE_KEY_FILE"
fi
TAILSCALE_IP="$(sudo tailscale ip -4 | awk 'NF { print; exit }')"
[[ -n "$TAILSCALE_IP" ]] || fail "Tailscale did not assign an IPv4 address"
echo "Tailscale connected: $TAILSCALE_IP"

BIN_DIR="$HOME/.local/bin"
CONFIG_HOME="$HOME/.config"
CONFIG_DIR="$CONFIG_HOME/clawctl"
UNIT_PARENT="$CONFIG_HOME/systemd"
UNIT_DIR="$UNIT_PARENT/user"
SYSTEM_UNIT_DIR="${CLAWCTL_SYSTEM_UNIT_DIR:-/etc/systemd/system}"
LOCAL_HOME="$HOME/.local"
SHARE_DIR="$LOCAL_HOME/share"
RUNTIME_DIR="$SHARE_DIR/clawctl"
CACHE_HOME="$HOME/.cache"
CACHE_DIR="$CACHE_HOME/clawctl"
CONTAINER_CONFIG_DIR="$CONFIG_HOME/containers"
CONTAINER_STORAGE_DIR="$SHARE_DIR/containers"
HERMES_DIR="$RUNTIME_DIR/hermes"
HERMES_DATA_DIR="$HERMES_DIR/data"
OPENCLAW_DIR="$RUNTIME_DIR/openclaw"
OPENCLAW_DROPIN_DIR="$UNIT_DIR/openclaw-gateway.service.d"
OPENCLAW_STATE_DIR="$HOME/.openclaw"
CONFIG_FILE="$CONFIG_DIR/agent.json"
LEGACY_UNIT_FILE="$UNIT_DIR/clawctl-agent.service"
SYSTEM_UNIT_FILE="$SYSTEM_UNIT_DIR/clawctl-agent.service"
HERMES_UNIT_FILE="$UNIT_DIR/clawctl-hermes.service"
OPENCLAW_UNIT_FILE="$UNIT_DIR/openclaw-gateway.service"
BIN_FILE="$BIN_DIR/clawctl-agent"

[[ ! -L "$OPENCLAW_UNIT_FILE" ]] || fail "OpenClaw unit path is a symlink: $OPENCLAW_UNIT_FILE"
[[ ! -e "$OPENCLAW_UNIT_FILE" || -f "$OPENCLAW_UNIT_FILE" ]] || fail "OpenClaw unit path is not a regular file: $OPENCLAW_UNIT_FILE"

STEP="private directories"
ensure_home_ancestor "$LOCAL_HOME"
ensure_owned_directory "$BIN_DIR" 0755
ensure_owned_directory "$SHARE_DIR" 0755
ensure_owned_directory "$RUNTIME_DIR" 0700
ensure_home_ancestor "$CONFIG_HOME"
ensure_owned_directory "$CONFIG_DIR" 0700
ensure_owned_directory "$CONTAINER_CONFIG_DIR" 0700
ensure_owned_directory "$UNIT_PARENT" 0755
ensure_owned_directory "$UNIT_DIR" 0755
ensure_home_ancestor "$CACHE_HOME"
ensure_owned_directory "$CACHE_DIR" 0700
ensure_owned_directory "$CONTAINER_STORAGE_DIR" 0700
ensure_owned_directory "$HERMES_DIR" 0700
ensure_owned_directory "$HERMES_DATA_DIR" 0700
ensure_owned_directory "$OPENCLAW_DIR" 0700
ensure_owned_directory "$OPENCLAW_DROPIN_DIR" 0700
ensure_owned_directory "$OPENCLAW_STATE_DIR" 0700

STEP="agent install"
AGENT_VERSION="$("$BIN_SRC" version)"
[[ -n "$AGENT_VERSION" ]] || fail "clawctl-agent version is empty"
install -m 0755 "$BIN_SRC" "$BIN_FILE.new"
mv -f "$BIN_FILE.new" "$BIN_FILE"
RENDERED_AGENT_UNIT="$(mktemp)"
SECRET_FILES+=("$RENDERED_AGENT_UNIT")
render_agent_unit "$SCRIPT_DIR/clawctl-agent.service" "$RENDERED_AGENT_UNIT"
sudo install -d -m 0755 "$SYSTEM_UNIT_DIR"
sudo install -m 0644 "$RENDERED_AGENT_UNIT" "$SYSTEM_UNIT_FILE.new"
sudo mv -f "$SYSTEM_UNIT_FILE.new" "$SYSTEM_UNIT_FILE"
install -m 0644 "$SCRIPT_DIR/clawctl-hermes.service" "$HERMES_UNIT_FILE.new"
mv -f "$HERMES_UNIT_FILE.new" "$HERMES_UNIT_FILE"
if [[ ! -e "$OPENCLAW_UNIT_FILE" && ! -L "$OPENCLAW_UNIT_FILE" ]] ||
   grep -Fqx '# AI-Intune managed OpenClaw unit v1' "$OPENCLAW_UNIT_FILE" 2>/dev/null; then
  install -m 0644 "$SCRIPT_DIR/openclaw-gateway.service" "$OPENCLAW_UNIT_FILE.new"
  mv -f "$OPENCLAW_UNIT_FILE.new" "$OPENCLAW_UNIT_FILE"
fi
echo "Agent installed: $AGENT_VERSION"

# Agent 是 system service，但它管理的 Hermes、OpenClaw 與 BAT 仍是 user
# services。先開 linger 並拉起 user manager，agent 才能透過上面的 bus
# 環境變數在登出後繼續管理它們。
STEP="user manager persistence"
sudo loginctl enable-linger "$AGENT_USER"
[[ "$(loginctl show-user "$AGENT_USER" --property=Linger --value)" == "yes" ]] || fail "linger is not enabled for $AGENT_USER"
sudo systemctl start "user@$EUID.service"
export XDG_RUNTIME_DIR="/run/user/$EUID"

STEP="container runtime readiness"
PODMAN_ROOTLESS="$(systemd-run --user --wait --pipe --quiet --collect --service-type=exec \
  --unit=clawctl-podman-readiness -- /usr/bin/podman info --format '{{.Host.Security.Rootless}}')"
[[ "$PODMAN_ROOTLESS" == "true" ]] || fail "rootless podman is not ready"

STEP="agent enrollment"
CURRENT_HUB=""
if [[ -f "$CONFIG_FILE" ]]; then
  CURRENT_HUB="$(grep '"hub_url"[[:space:]]*:[[:space:]]*"[^"]*"' "$CONFIG_FILE" | head -n 1 | sed -e 's/.*"hub_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')"
fi

BACKUP_CONFIG=""

if [[ -f "$CONFIG_FILE" && "$REENROLL" == "0" ]]; then
  if [[ -n "$CURRENT_HUB" && "$CURRENT_HUB" != "$HUB" ]]; then
    fail "Machine is enrolled to a different Hub ($CURRENT_HUB). Run with --reenroll to move it to the new Hub."
  fi
elif [[ ! -f "$CONFIG_FILE" || "$REENROLL" == "1" ]]; then
  if [[ -n "$TOKEN_FILE" ]]; then
    private_secret_file "$TOKEN_FILE"
  elif [[ -n "$TOKEN" ]]; then
    new_secret_file "$TOKEN" TOKEN_FILE
    unset TOKEN
  elif [[ -t 0 ]]; then
    read -r -s -p "Enrollment token: " TOKEN
    echo
    [[ -n "$TOKEN" ]] || fail "Enrollment token is required"
    new_secret_file "$TOKEN" TOKEN_FILE
    unset TOKEN
  else
    fail "Enrollment token is required; use --token-file FILE"
  fi

  WAS_ACTIVE=0

  if [[ -f "$CONFIG_FILE" && "$REENROLL" == "1" ]]; then
    if systemctl is-active --quiet clawctl-agent.service 2>/dev/null; then
      WAS_ACTIVE=1
      sudo systemctl stop clawctl-agent.service || true
    fi
    BACKUP_CONFIG="$CONFIG_DIR/agent.json.pre-reenroll-$(date -u +%Y%m%dT%H%M%SZ)"
    mv "$CONFIG_FILE" "$BACKUP_CONFIG"
    chmod 0600 "$BACKUP_CONFIG"
  fi

  if ! "$BIN_FILE" enroll --hub "$HUB" --token-file "$TOKEN_FILE"; then
    if [[ -n "$BACKUP_CONFIG" && -f "$BACKUP_CONFIG" ]]; then
      mv "$BACKUP_CONFIG" "$CONFIG_FILE"
      if [[ "$WAS_ACTIVE" == "1" ]]; then
        sudo systemctl start clawctl-agent.service || true
      fi
      fail "Enrollment failed: previous configuration restored; the agent binary is now the new Hub's version, so rerun the old Hub's install-agent.sh (without --reenroll) to put back its matching binary."
    fi
    fail "Enrollment failed"
  fi
fi
[[ -f "$CONFIG_FILE" && ! -L "$CONFIG_FILE" ]] || fail "Agent enrollment did not create its config"
[[ "$(stat -c '%a' "$CONFIG_FILE")" == "600" ]] || fail "Agent config mode is not 0600"

STEP="agent service"
sudo systemctl daemon-reload
[[ "$(systemctl show clawctl-agent.service --property=LoadState --value)" == "loaded" ]] || fail "clawctl-agent system unit did not load"
[[ "$(systemctl show clawctl-agent.service --property=FragmentPath --value)" == "$SYSTEM_UNIT_FILE" ]] || fail "clawctl-agent system unit path mismatch before migration"
[[ "$(systemctl show clawctl-agent.service --property=User --value)" == "$AGENT_USER" ]] || fail "clawctl-agent systemd User contract mismatch before migration"
[[ "$(systemctl show clawctl-agent.service --property=ProtectSystem --value)" == "strict" ]] || fail "clawctl-agent ProtectSystem sandbox is not loaded before migration"
[[ "$(systemctl show clawctl-agent.service --property=ProtectHome --value)" == "read-only" ]] || fail "clawctl-agent ProtectHome sandbox is not loaded before migration"
sudo systemctl enable clawctl-agent.service
if [[ -e "$LEGACY_UNIT_FILE" || -L "$LEGACY_UNIT_FILE" ]]; then
  [[ -f "$LEGACY_UNIT_FILE" && ! -L "$LEGACY_UNIT_FILE" ]] || fail "Legacy agent unit path is not safe: $LEGACY_UNIT_FILE"
  systemctl --user disable --now clawctl-agent.service
  rm -f -- "$LEGACY_UNIT_FILE"
  systemctl --user daemon-reload
fi
sudo systemctl reset-failed clawctl-agent.service 2>/dev/null || true
SERVICE_STARTED_AT="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
sudo systemctl restart clawctl-agent.service
systemctl is-active --quiet clawctl-agent.service

STEP="Hub readiness"
"$BIN_FILE" verify --hub "$HUB" --since "$SERVICE_STARTED_AT" --timeout 2m
RESTARTS_AFTER="$(systemctl show clawctl-agent.service --property=NRestarts --value)"
[[ "$RESTARTS_AFTER" == "0" ]] || fail "clawctl-agent restarted during installation"
MAIN_PID="$(systemctl show clawctl-agent.service --property=MainPID --value)"
[[ "$MAIN_PID" =~ ^[1-9][0-9]*$ ]] || fail "clawctl-agent has no MainPID"
[[ "$(ps -o comm= -p "$MAIN_PID" | xargs)" == "clawctl-agent" ]] || fail "clawctl-agent MainPID identity mismatch"
[[ "$(ps -o uid= -p "$MAIN_PID" | xargs)" == "$EUID" ]] || fail "clawctl-agent process owner mismatch"
[[ "$(systemctl show clawctl-agent.service --property=FragmentPath --value)" == "$SYSTEM_UNIT_FILE" ]] || fail "clawctl-agent is not loaded as the managed system unit"
[[ "$(systemctl show clawctl-agent.service --property=User --value)" == "$(id -un)" ]] || fail "clawctl-agent systemd User contract mismatch"
[[ "$(systemctl show clawctl-agent.service --property=ProtectSystem --value)" == "strict" ]] || fail "clawctl-agent ProtectSystem sandbox is not loaded"
[[ "$(systemctl show clawctl-agent.service --property=ProtectHome --value)" == "read-only" ]] || fail "clawctl-agent ProtectHome sandbox is not loaded"
AGENT_COUNT="$(ps -eo comm= | awk '$1=="clawctl-agent" { n++ } END { print n+0 }')"
[[ "$AGENT_COUNT" == "1" ]] || fail "Expected one clawctl-agent process; found $AGENT_COUNT"

echo "Managed: agent=$AGENT_VERSION tailscale=$TAILSCALE_IP service=active jobs=enabled"
if [[ -n "${BACKUP_CONFIG:-}" ]]; then
  echo ""
  echo "Re-enrollment successful. The old configuration was backed up to:"
  echo "  $BACKUP_CONFIG"
  echo "To roll back, restore that file to $CONFIG_FILE and rerun the old Hub's install-agent.sh without --reenroll (see docs/MOVE-AGENTS.md)."
  echo "Please remember to retire this machine on the old Hub."
fi
