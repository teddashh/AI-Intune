#!/usr/bin/env bash

set -Eeuo pipefail

HUB=""
HUB_EXPLICIT=0
TOKEN_EXPLICIT=0
TOKEN=""
TOKEN_FILE=""
TAILSCALE_KEY_FILE=""
BIN_SRC=""
STEP="preflight"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
SECRET_FILES=()
NO_TAILSCALE=0
EMBEDDED_TOKEN=0
REENROLL=0
BACKUP_CONFIG=""
REENROLL_PHASE=0
NO_CONTAINER_RUNTIME=0
OLD_SYSTEM_ACTIVE=0
OLD_SYSTEM_ENABLED=0
OLD_LEGACY_ACTIVE=0
OLD_LEGACY_ENABLED=0
OLD_SYSTEM_UNIT=0
BACKUP_UNIT=""
BACKUP_BINARY=""

usage() {
  cat <<'EOF'
Usage: ./install-agent.sh [--hub URL] [options]

  --hub URL
      http://<tailscale-ipv4>:<port>  (same address as the operator UI)
      https://<hostname>[:port]       (public HTTPS Hub; Tailscale skipped)

Keyed packages read hub-url and enroll-token beside this script when flags are absent.
The embedded enroll-token is removed after successful enrollment.

Options:
  --token TOKEN                    one-time enrollment token
  --token-file FILE                0600 file containing the enrollment token
  --tailscale-auth-key-file FILE   0600 file containing a Tailscale auth key
  --binary FILE                    clawctl-agent binary for this machine
  --no-tailscale                   skip Tailscale install and connection
  --no-container-runtime           skip podman setup (Hermes container jobs need it)
  --reenroll                       reenroll this machine to a new Hub (Linux only)
EOF
}

fail() {
  echo "$1" >&2
  if [[ "$REENROLL_PHASE" == 1 ]]; then rollback_reenroll; fi
  if [[ -n "$BACKUP_CONFIG" && -f "$BACKUP_CONFIG" ]]; then
    echo "Previous configuration backed up at: $BACKUP_CONFIG (see docs/MOVE-AGENTS.md, Rollback)" >&2
  fi
  exit 1
}

on_error() {
  local rc=$?
  # Let the parent assignment report substitution failures and roll back once.
  if (( BASH_SUBSHELL > 0 )); then return "$rc"; fi
  echo "Install failed: $STEP" >&2
  if [[ "$REENROLL_PHASE" == 1 ]]; then rollback_reenroll; fi
  if [[ -n "$BACKUP_CONFIG" && -f "$BACKUP_CONFIG" ]]; then
    echo "Previous configuration backed up at: $BACKUP_CONFIG (see docs/MOVE-AGENTS.md, Rollback)" >&2
  fi
  exit "$rc"
}

# Rollback deliberately checks each command: ERR traps are disabled here to avoid recursion.
rollback_reenroll() {
  REENROLL_PHASE=0
  trap - ERR
  set +e
  local failed=0
  if [[ -e "$SYSTEM_UNIT_FILE" ]]; then
    sudo systemctl stop clawctl-agent.service || failed=1
  fi
  if [[ "$OLD_SYSTEM_UNIT" == 1 ]]; then
    sudo install -m 0644 "$BACKUP_UNIT" "$SYSTEM_UNIT_FILE" || failed=1
  else
    if [[ -e "$SYSTEM_UNIT_FILE" ]]; then
      sudo systemctl disable clawctl-agent.service || failed=1
    fi
    sudo rm -f -- "$SYSTEM_UNIT_FILE" || failed=1
  fi
  if [[ -n "$BACKUP_BINARY" ]]; then
    sudo install -m 0755 -o root -g root "$BACKUP_BINARY" "$BIN_FILE.new" &&
      sudo mv -f "$BIN_FILE.new" "$BIN_FILE" || failed=1
    restore_binary_label || failed=1
  fi
  sudo systemctl daemon-reload || failed=1
  if [[ -f "$STAGING_CONFIG" ]]; then
    mv -f "$STAGING_CONFIG" "$CONFIG_DIR/agent.json.failed-reenroll-$REENROLL_STAMP" || failed=1
    chmod 0600 "$CONFIG_DIR/agent.json.failed-reenroll-$REENROLL_STAMP" || failed=1
  elif [[ -f "$CONFIG_FILE" ]]; then
    mv -f "$CONFIG_FILE" "$CONFIG_DIR/agent.json.failed-reenroll-$REENROLL_STAMP" || failed=1
    chmod 0600 "$CONFIG_DIR/agent.json.failed-reenroll-$REENROLL_STAMP" || failed=1
  fi
  install -m 0600 "$BACKUP_CONFIG" "$CONFIG_FILE.rollback" &&
    mv -f "$CONFIG_FILE.rollback" "$CONFIG_FILE" || failed=1
  if [[ "$OLD_SYSTEM_UNIT" == 1 ]]; then
    if [[ "$OLD_SYSTEM_ENABLED" == 1 ]]; then
      sudo systemctl enable clawctl-agent.service || failed=1
    else
      sudo systemctl disable clawctl-agent.service || failed=1
    fi
  fi
  if [[ "$OLD_LEGACY_ENABLED" == 1 ]]; then
    systemctl --user enable clawctl-agent.service || failed=1
  fi
  if [[ "$OLD_SYSTEM_ACTIVE" == 1 ]]; then
    sudo systemctl start clawctl-agent.service &&
      systemctl is-active --quiet clawctl-agent.service || failed=1
  fi
  if [[ "$OLD_LEGACY_ACTIVE" == 1 ]]; then
    systemctl --user start clawctl-agent.service &&
      systemctl --user is-active --quiet clawctl-agent.service || failed=1
  fi
  if [[ "$failed" == 0 ]]; then
    if [[ "$OLD_SYSTEM_ACTIVE" == 1 || "$OLD_LEGACY_ACTIVE" == 1 ]]; then
      echo "Rollback completed: previous configuration restored; old agent is running again. Retire the abandoned enrollment on the NEW Hub: $HUB" >&2
    else
      echo "Rollback completed: previous configuration restored; old units were previously inactive. Retire the abandoned enrollment on the NEW Hub: $HUB" >&2
    fi
  else
    {
      echo "Rollback failed. Manual recovery steps:"
      echo "  sudo systemctl stop clawctl-agent.service"
      printf '  install -m 0600 %q %q\n' "$BACKUP_CONFIG" "$CONFIG_FILE"
      if [[ "$OLD_SYSTEM_UNIT" == 1 ]]; then
        printf '  sudo install -m 0644 %q %q\n' "$BACKUP_UNIT" "$SYSTEM_UNIT_FILE"
      else
        echo "  sudo systemctl disable clawctl-agent.service"
        printf '  sudo rm -f -- %q\n' "$SYSTEM_UNIT_FILE"
      fi
      if [[ -n "$BACKUP_BINARY" ]]; then
        printf '  sudo install -m 0755 -o root -g root %q %q\n' "$BACKUP_BINARY" "$BIN_FILE"
        printf '  # On SELinux hosts: sudo restorecon -F %q\n' "$BIN_FILE"
      fi
      echo "  sudo systemctl daemon-reload"
      if [[ "$OLD_SYSTEM_UNIT" == 1 ]]; then
        if [[ "$OLD_SYSTEM_ENABLED" == 1 ]]; then
          echo "  sudo systemctl enable clawctl-agent.service"
        else
          echo "  sudo systemctl disable clawctl-agent.service"
        fi
      fi
      if [[ "$OLD_SYSTEM_ACTIVE" == 1 ]]; then
        echo "  sudo systemctl start clawctl-agent.service"
        echo "  systemctl is-active clawctl-agent.service"
      fi
      if [[ "$OLD_LEGACY_ACTIVE" == 1 ]]; then
        if [[ "$OLD_LEGACY_ENABLED" == 1 ]]; then echo "  systemctl --user enable clawctl-agent.service"; fi
        echo "  systemctl --user start clawctl-agent.service"
        echo "  systemctl --user is-active clawctl-agent.service"
      fi
      echo "Retire the abandoned enrollment on the NEW Hub: $HUB"
    } >&2
  fi
  exit 1
}

restore_binary_label() {
  if command -v restorecon >/dev/null &&
     { { command -v selinuxenabled >/dev/null && selinuxenabled; } || [[ -e /sys/fs/selinux/enforce ]]; }; then
    sudo restorecon -F "$BIN_FILE" || return $?
  fi
  return 0
}

install_agent_unit() {
  sudo install -d -m 0755 "$SYSTEM_UNIT_DIR"
  sudo install -m 0644 "$RENDERED_AGENT_UNIT" "$SYSTEM_UNIT_FILE.new"
  sudo mv -f "$SYSTEM_UNIT_FILE.new" "$SYSTEM_UNIT_FILE"
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
# https:// reaches the Hub directly without requiring Tailscale.
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
    --hub) [[ $# -ge 2 ]] || fail "--hub requires a value"; HUB="$2"; HUB_EXPLICIT=1; shift 2 ;;
    --token) [[ $# -ge 2 ]] || fail "--token requires a value"; TOKEN="$2"; TOKEN_EXPLICIT=1; shift 2 ;;
    --token-file) [[ $# -ge 2 ]] || fail "--token-file requires a value"; TOKEN_FILE="$2"; TOKEN_EXPLICIT=1; shift 2 ;;
    --tailscale-auth-key-file) [[ $# -ge 2 ]] || fail "--tailscale-auth-key-file requires a value"; TAILSCALE_KEY_FILE="$2"; shift 2 ;;
    --binary) [[ $# -ge 2 ]] || fail "--binary requires a value"; BIN_SRC="$2"; shift 2 ;;
    --no-tailscale) NO_TAILSCALE=1; shift 1 ;;
    --no-container-runtime) NO_CONTAINER_RUNTIME=1; shift ;;
    --reenroll) REENROLL=1; shift 1 ;;
    --help|-h) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ "$HUB_EXPLICIT" == "0" && -f "$SCRIPT_DIR/hub-url" ]]; then
  HUB="$(cat -- "$SCRIPT_DIR/hub-url")"
  HUB="${HUB%$'\r'}"
fi
if [[ "$TOKEN_EXPLICIT" == "0" && -f "$SCRIPT_DIR/enroll-token" ]]; then
  TOKEN_FILE="$SCRIPT_DIR/enroll-token"
  EMBEDDED_TOKEN=1
fi
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
  [[ "$mode" == "600" || "$mode" == "400" ]] || fail "Secret file must have mode 0600 or 0400: $path; for an embedded token, run chmod 600 enroll-token"
}

new_secret_file() {
  local value=$1 output_name=$2 path
  path="$(mktemp)"
  chmod 0600 "$path"
  printf '%s\n' "$value" >"$path"
  SECRET_FILES+=("$path")
  printf -v "$output_name" '%s' "$path"
}

# systemd 252 cannot bind a ReadWritePaths entry reached through a symlinked home
# ancestor (226/NAMESPACE "Permission denied" under unit-root), but the resolved
# path works. Rewrite each existing entry to its canonical path.
resolve_unit_rw_paths() {
  local unit=$1 line path resolved out=""
  line="$(grep '^ReadWritePaths=' "$unit")" || return 0
  [[ "$(grep -c '^ReadWritePaths=' "$unit")" == 1 ]] || { echo "Agent unit must have one ReadWritePaths line" >&2; return 1; }
  for path in ${line#ReadWritePaths=}; do
    resolved=$path
    if [[ -e "$path" ]]; then
      resolved="$(readlink -f -- "$path")" || return 1
      [[ "$resolved" =~ ^/[a-zA-Z0-9._/-]+$ ]] || { echo "Unsafe resolved ReadWritePaths entry: $path -> $resolved" >&2; return 1; }
    fi
    out+="${out:+ }$resolved"
  done
  sed -i "s|^ReadWritePaths=.*|ReadWritePaths=$out|" "$unit"
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
      -e "s|@@CLAWCTL_AGENT_BIN@@|$BIN_FILE|g" \
      "$source" >"$destination"
  ! grep -Fq 'CLAWCTL_AGENT_' "$destination" || fail "Agent systemd unit rendering is incomplete"
  resolve_unit_rw_paths "$destination" || fail "Agent systemd unit ReadWritePaths could not be resolved"
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

# A home ancestor such as ~/.cache may be a symlink to another disk (for example
# /data/home-user/cache). Accept it only when the resolved target is a directory
# owned by this user and not group- or world-writable; anything else could let
# another account redirect the agent's private state.
ensure_home_ancestor() {
  local path=$1 target mode
  if [[ -L "$path" ]]; then
    target="$(readlink -f -- "$path")" || fail "Home path is not safe: $path"
    [[ -n "$target" && -d "$target" && ! -L "$target" ]] || fail "Home path is not safe: $path"
    [[ "$(stat -c '%u' "$target")" == "$EUID" ]] || fail "Home path owner is not $USER: $path -> $target"
    mode="$(stat -c '%a' "$target")"
    (( (8#$mode & 8#022) == 0 )) || fail "Home path target is group- or world-writable: $path -> $target"
  elif [[ -e "$path" ]]; then
    [[ -d "$path" ]] || fail "Home path is not safe: $path"
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
sudo -n true 2>/dev/null || sudo -v

STEP="container runtime"
if [[ "$NO_CONTAINER_RUNTIME" == 0 ]]; then
  install_container_runtime
  ensure_subordinate_ids
else
  echo "Container runtime: skipped (--no-container-runtime); Hermes container jobs need podman"
fi

TAILSCALE_IP="skipped"
if [[ "$HUB" == https://* ]]; then
  echo "Tailscale: skipped (Hub reached over HTTPS)"
elif [[ "$NO_TAILSCALE" == "1" ]]; then
  echo "Tailscale: skipped (--no-tailscale)"
else
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
fi

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
# Ops test hook, like CLAWCTL_SYSTEM_UNIT_DIR; not an operator option.
SYSTEM_BIN_DIR="${CLAWCTL_SYSTEM_BIN_DIR:-/usr/local/bin}"
[[ "$SYSTEM_BIN_DIR" =~ ^/[a-zA-Z0-9._/-]+$ && "$SYSTEM_BIN_DIR" != *"/../"* && "$SYSTEM_BIN_DIR" != */.. ]] || fail "Unsafe system binary directory"
BIN_FILE="$SYSTEM_BIN_DIR/clawctl-agent"

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
if [[ "$REENROLL" == 1 && -f "$CONFIG_FILE" && -f "$BIN_FILE" ]]; then
  BACKUP_BINARY="$(mktemp)"
  SECRET_FILES+=("$BACKUP_BINARY")
  cp "$BIN_FILE" "$BACKUP_BINARY"
fi
sudo install -d -m 0755 "$SYSTEM_BIN_DIR"
sudo install -m 0755 -o root -g root "$BIN_SRC" "$BIN_FILE.new"
sudo mv -f "$BIN_FILE.new" "$BIN_FILE"
restore_binary_label
AGENT_VERSION="$("$BIN_FILE" version)"
[[ -n "$AGENT_VERSION" ]] || fail "clawctl-agent version is empty"
if [[ -f "$BIN_DIR/clawctl-agent" ]]; then
  echo "Legacy binary $BIN_DIR/clawctl-agent retained; no longer used by the system unit."
fi
RENDERED_AGENT_UNIT="$(mktemp)"
SECRET_FILES+=("$RENDERED_AGENT_UNIT")
render_agent_unit "$SCRIPT_DIR/clawctl-agent.service" "$RENDERED_AGENT_UNIT"
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
if [[ "$NO_CONTAINER_RUNTIME" == 0 ]]; then
  PODMAN_ROOTLESS="$(systemd-run --user --wait --pipe --quiet --collect --service-type=exec \
  --unit=clawctl-podman-readiness -- /usr/bin/podman info --format '{{.Host.Security.Rootless}}')"
  [[ "$PODMAN_ROOTLESS" == "true" ]] || fail "rootless podman is not ready"
fi

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

  if [[ -f "$CONFIG_FILE" && "$REENROLL" == 1 ]]; then
    STAGING_CONFIG="$CONFIG_DIR/agent.json.reenroll-new"
    [[ ! -e "$STAGING_CONFIG" && ! -L "$STAGING_CONFIG" ]] || fail "Staging config already exists: $STAGING_CONFIG"
    if ! CLAWCTL_CONFIG="$STAGING_CONFIG" "$BIN_FILE" enroll --hub "$HUB" --token-file "$TOKEN_FILE"; then
      rm -f -- "$STAGING_CONFIG"
      if [[ -n "$BACKUP_BINARY" ]]; then
        sudo install -m 0755 -o root -g root "$BACKUP_BINARY" "$BIN_FILE.new"
        sudo mv -f "$BIN_FILE.new" "$BIN_FILE"
        restore_binary_label
      fi
      fail "Enrollment failed: old agent and configuration left untouched"
    fi
    [[ -f "$STAGING_CONFIG" && ! -L "$STAGING_CONFIG" ]] || fail "Enrollment did not create staging config"
    chmod 0600 "$STAGING_CONFIG"
    REENROLL_STAMP="$(date -u +%Y%m%dT%H%M%SZ)-$$"
    BACKUP_CONFIG="$CONFIG_DIR/agent.json.pre-reenroll-$REENROLL_STAMP"
    if [[ -n "$BACKUP_BINARY" ]]; then
      DURABLE_BINARY_BACKUP="$CONFIG_DIR/clawctl-agent.binary.pre-reenroll-$REENROLL_STAMP"
      install -m 0600 "$BACKUP_BINARY" "$DURABLE_BINARY_BACKUP"
      BACKUP_BINARY="$DURABLE_BINARY_BACKUP"
    fi
    cp "$CONFIG_FILE" "$BACKUP_CONFIG"
    chmod 0600 "$BACKUP_CONFIG"
    if [[ -e "$SYSTEM_UNIT_FILE" ]]; then
      [[ -f "$SYSTEM_UNIT_FILE" && ! -L "$SYSTEM_UNIT_FILE" ]] || fail "Unsafe existing system unit"
      OLD_SYSTEM_UNIT=1
      BACKUP_UNIT="$CONFIG_DIR/clawctl-agent.service.pre-reenroll-$REENROLL_STAMP"
      cp "$SYSTEM_UNIT_FILE" "$BACKUP_UNIT"
      chmod 0600 "$BACKUP_UNIT"
      systemctl is-active --quiet clawctl-agent.service && OLD_SYSTEM_ACTIVE=1
      systemctl is-enabled --quiet clawctl-agent.service && OLD_SYSTEM_ENABLED=1
    fi
    if [[ -e "$LEGACY_UNIT_FILE" || -L "$LEGACY_UNIT_FILE" ]]; then
      [[ -f "$LEGACY_UNIT_FILE" && ! -L "$LEGACY_UNIT_FILE" ]] || fail "Unsafe legacy unit"
      systemctl --user is-active --quiet clawctl-agent.service && OLD_LEGACY_ACTIVE=1
      systemctl --user is-enabled --quiet clawctl-agent.service && OLD_LEGACY_ENABLED=1
    fi
    REENROLL_PHASE=1
    if [[ "$OLD_SYSTEM_UNIT" == 1 ]]; then sudo systemctl stop clawctl-agent.service; fi
    if [[ -f "$LEGACY_UNIT_FILE" ]]; then systemctl --user stop clawctl-agent.service; fi
    mv -f "$STAGING_CONFIG" "$CONFIG_FILE"
  elif ! "$BIN_FILE" enroll --hub "$HUB" --token-file "$TOKEN_FILE"; then
    fail "Enrollment failed"
  fi
  if [[ "$EMBEDDED_TOKEN" == "1" || "$TOKEN_FILE" == "$SCRIPT_DIR/enroll-token" ]]; then
    rm -- "$SCRIPT_DIR/enroll-token"
    echo "Embedded enroll-token removed after successful enrollment."
  fi
fi
[[ -f "$CONFIG_FILE" && ! -L "$CONFIG_FILE" ]] || fail "Agent enrollment did not create its config"
[[ "$(stat -c '%a' "$CONFIG_FILE")" == "600" ]] || fail "Agent config mode is not 0600"

STEP="agent service"
install_agent_unit
sudo systemctl daemon-reload
[[ "$(systemctl show clawctl-agent.service --property=LoadState --value)" == "loaded" ]] || fail "clawctl-agent system unit did not load"
[[ "$(systemctl show clawctl-agent.service --property=FragmentPath --value)" == "$SYSTEM_UNIT_FILE" ]] || fail "clawctl-agent system unit path mismatch before migration"
[[ "$(systemctl show clawctl-agent.service --property=User --value)" == "$AGENT_USER" ]] || fail "clawctl-agent systemd User contract mismatch before migration"
[[ "$(systemctl show clawctl-agent.service --property=ProtectSystem --value)" == "strict" ]] || fail "clawctl-agent ProtectSystem sandbox is not loaded before migration"
[[ "$(systemctl show clawctl-agent.service --property=ProtectHome --value)" == "read-only" ]] || fail "clawctl-agent ProtectHome sandbox is not loaded before migration"
sudo systemctl enable clawctl-agent.service
if [[ "$REENROLL_PHASE" == 0 ]]; then
  if [[ -e "$LEGACY_UNIT_FILE" || -L "$LEGACY_UNIT_FILE" ]]; then
    [[ -f "$LEGACY_UNIT_FILE" && ! -L "$LEGACY_UNIT_FILE" ]] || fail "Legacy agent unit path is not safe: $LEGACY_UNIT_FILE"
    systemctl --user disable --now clawctl-agent.service
    rm -f -- "$LEGACY_UNIT_FILE"
    systemctl --user daemon-reload
  fi
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

REENROLL_PHASE=0
if [[ -e "$LEGACY_UNIT_FILE" || -L "$LEGACY_UNIT_FILE" ]]; then
  [[ -f "$LEGACY_UNIT_FILE" && ! -L "$LEGACY_UNIT_FILE" ]] || fail "Legacy agent unit path is not safe: $LEGACY_UNIT_FILE"
  systemctl --user disable --now clawctl-agent.service
  rm -f -- "$LEGACY_UNIT_FILE"
  systemctl --user daemon-reload
fi
echo "Managed: agent=$AGENT_VERSION tailscale=$TAILSCALE_IP service=active jobs=enabled"
if [[ -n "${BACKUP_CONFIG:-}" ]]; then
  echo ""
  echo "Re-enrollment successful. The old configuration was backed up to:"
  echo "  $BACKUP_CONFIG"
  echo "To roll back, restore that file to $CONFIG_FILE and rerun the old Hub's install-agent.sh without --reenroll (see docs/MOVE-AGENTS.md)."
  echo "Please remember to retire this machine on the old Hub."
fi
