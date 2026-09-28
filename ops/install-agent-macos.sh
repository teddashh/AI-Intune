#!/usr/bin/env bash
set -Eeuo pipefail

STEP="initialization"
SECRET_FILES=()

fail() {
  echo "Error: $*" >&2
  exit 1
}

on_error() {
  echo "Install failed: $STEP" >&2
}

cleanup() {
  local file
  # macOS 內建的 bash 3.2 在 set -u 下展開空陣列會報 unbound variable。
  [[ ${#SECRET_FILES[@]} -gt 0 ]] || return 0
  for file in "${SECRET_FILES[@]}"; do
    [[ ! -e "$file" ]] || rm -f -- "$file"
  done
}

trap on_error ERR
trap cleanup EXIT

usage() {
  echo "Usage: $0 --hub URL [--token TOKEN | --token-file FILE] [--binary FILE]"
}

HUB=""
TOKEN=""
TOKEN_FILE=""
BIN_SRC=""

while (($#)); do
  case "$1" in
    --hub)
      (($# >= 2)) || fail "--hub requires a URL"
      HUB="$2"
      shift 2
      ;;
    --token)
      (($# >= 2)) || fail "--token requires a token"
      TOKEN="$2"
      shift 2
      ;;
    --token-file)
      (($# >= 2)) || fail "--token-file requires a file"
      TOKEN_FILE="$2"
      shift 2
      ;;
    --binary)
      (($# >= 2)) || fail "--binary requires a file"
      BIN_SRC="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      fail "Unknown option: $1"
      ;;
  esac
done

STEP="preflight"
[[ -n "$HUB" ]] || fail "--hub URL is required"
HUB="${HUB%/}"
[[ "$HUB" == http://* || "$HUB" == https://* ]] || fail "--hub must start with http:// or https://"
((EUID != 0)) || fail "Run this installer as the user account that will run the agent, not as root"
[[ "$(uname -s)" == Darwin ]] || fail "This installer requires macOS. On Linux run ./install-agent.sh"
command -v launchctl >/dev/null 2>&1 || fail "launchctl is required"
[[ -z "$TOKEN" || -z "$TOKEN_FILE" ]] || fail "Use only one of --token and --token-file"

case "$(uname -m)" in
  arm64) AGENT_ARCH="arm64" ;;
  x86_64|amd64) AGENT_ARCH="amd64" ;;
  *) fail "Unsupported architecture: $(uname -m)" ;;
esac

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
PLIST_TEMPLATE="$SCRIPT_DIR/clawctl-agent.plist"
[[ -f "$PLIST_TEMPLATE" && ! -L "$PLIST_TEMPLATE" ]] || fail "LaunchAgent template must be a regular file and not a symlink: $PLIST_TEMPLATE"

if [[ -n "$BIN_SRC" ]]; then
  [[ -f "$BIN_SRC" ]] || fail "Agent binary is not a regular file: $BIN_SRC"
else
  for candidate in \
    "$SCRIPT_DIR/clawctl-agent" \
    "$SCRIPT_DIR/../build/clawctl-agent-darwin-$AGENT_ARCH" \
    "$SCRIPT_DIR/../build/clawctl-agent"; do
    if [[ -f "$candidate" ]]; then
      BIN_SRC="$candidate"
      break
    fi
  done
  [[ -n "$BIN_SRC" ]] || fail "Agent binary not found; use --binary FILE"
fi

BIN_DIR="$HOME/.local/bin"
BIN_FILE="$BIN_DIR/clawctl-agent"
CONFIG_DIR="$HOME/Library/Application Support/clawctl"
CONFIG_FILE="$CONFIG_DIR/agent.json"
CACHE_DIR="$HOME/Library/Caches/clawctl"
LOG_DIR="$HOME/Library/Logs/clawctl"
LOG_FILE="$LOG_DIR/agent.log"
AGENT_DIR="$HOME/Library/LaunchAgents"
PLIST_FILE="$AGENT_DIR/com.clawctl.agent.plist"
LABEL="com.clawctl.agent"

ensure_home_ancestor() {
  local path="$1"
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -d "$path" && ! -L "$path" ]] || fail "Home path is not safe: $path"
    [[ "$(stat -f '%u' "$path")" == "$EUID" ]] || fail "Home path owner is not $USER: $path"
  else
    mkdir -- "$path"
  fi
}

ensure_directory() {
  local directory="$1"
  local mode="$2"
  if [[ -e "$directory" ]]; then
    [[ -d "$directory" && ! -L "$directory" ]] || fail "Directory must not be a symlink: $directory"
    [[ "$(stat -f '%u' "$directory")" == "$EUID" ]] || fail "Directory must be owned by the current user: $directory"
  else
    mkdir -m "$mode" -- "$directory"
  fi
  chmod "$mode" -- "$directory"
  [[ "$(stat -f '%Lp' "$directory")" == "${mode#0}" ]] || fail "Could not set permissions on directory: $directory"
}

ensure_home_ancestor "$HOME/.local"
ensure_directory "$BIN_DIR" 0755
ensure_home_ancestor "$HOME/Library"
ensure_directory "$AGENT_DIR" 0755
ensure_home_ancestor "$HOME/Library/Application Support"
ensure_directory "$CONFIG_DIR" 0700
ensure_home_ancestor "$HOME/Library/Caches"
ensure_directory "$CACHE_DIR" 0700
ensure_home_ancestor "$HOME/Library/Logs"
ensure_directory "$LOG_DIR" 0700

STEP="agent install"
AGENT_VERSION="$("$BIN_SRC" version)"
[[ -n "$AGENT_VERSION" ]] || fail "Agent version is empty"
install -m 0755 -- "$BIN_SRC" "$BIN_FILE.new"
mv -f -- "$BIN_FILE.new" "$BIN_FILE"
echo "Agent installed: $AGENT_VERSION"

STEP="agent enrollment"
if [[ ! -e "$CONFIG_FILE" ]]; then
  if [[ -n "$TOKEN_FILE" ]]; then
    [[ -f "$TOKEN_FILE" && ! -L "$TOKEN_FILE" ]] || fail "Token file must be a regular file and not a symlink: $TOKEN_FILE"
    TOKEN_MODE="$(stat -f '%Lp' "$TOKEN_FILE")"
    [[ "$TOKEN_MODE" == 600 || "$TOKEN_MODE" == 400 ]] || fail "Token file permissions must be 0600 or 0400: $TOKEN_FILE"
  else
    if [[ -z "$TOKEN" ]]; then
      read -r -s -p "Enrollment token: " TOKEN
      echo
    fi
    [[ -n "$TOKEN" ]] || fail "Enrollment token is required"
    TOKEN_FILE="$(mktemp "${TMPDIR:-/tmp}/clawctl-token.XXXXXX")"
    SECRET_FILES+=("$TOKEN_FILE")
    chmod 0600 -- "$TOKEN_FILE"
    printf '%s\n' "$TOKEN" >"$TOKEN_FILE"
    TOKEN=""
  fi
  "$BIN_FILE" enroll --hub "$HUB" --token-file "$TOKEN_FILE"
  [[ -f "$CONFIG_FILE" && ! -L "$CONFIG_FILE" ]] || fail "Enrollment did not create a regular config file: $CONFIG_FILE"
  [[ "$(stat -f '%Lp' "$CONFIG_FILE")" == 600 ]] || fail "Agent config permissions must be 0600: $CONFIG_FILE"
elif [[ ! -f "$CONFIG_FILE" || -L "$CONFIG_FILE" ]]; then
  fail "Agent config must be a regular file and not a symlink: $CONFIG_FILE"
fi

STEP="launchd service"
xml_escape() {
  local value="$1"
  value="${value//&/\&amp;}"
  value="${value//</\&lt;}"
  value="${value//>/\&gt;}"
  printf '%s' "$value"
}
BIN_XML="$(xml_escape "$BIN_FILE")"
LOG_XML="$(xml_escape "$LOG_FILE")"
PLIST_CONTENT="$(<"$PLIST_TEMPLATE")"
PLIST_CONTENT="${PLIST_CONTENT//@@CLAWCTL_AGENT_BIN@@/$BIN_XML}"
PLIST_CONTENT="${PLIST_CONTENT//@@CLAWCTL_AGENT_LOG@@/$LOG_XML}"
[[ "$PLIST_CONTENT" != *@@* ]] || fail "LaunchAgent plist contains an unresolved placeholder"
printf '%s\n' "$PLIST_CONTENT" >"$PLIST_FILE.new"
chmod 0644 -- "$PLIST_FILE.new"
mv -f -- "$PLIST_FILE.new" "$PLIST_FILE"

STEP="launchd load"
launchctl bootout "gui/$UID/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$UID" "$PLIST_FILE"
SERVICE_STARTED_AT="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
launchctl kickstart -k "gui/$UID/$LABEL"

STEP="Hub readiness"
"$BIN_FILE" verify --hub "$HUB" --since "$SERVICE_STARTED_AT" --timeout 2m --require-platform-evidence

echo "Managed: agent=$AGENT_VERSION service=$LABEL config=\"$CONFIG_FILE\""
