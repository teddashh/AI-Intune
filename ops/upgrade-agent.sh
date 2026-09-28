#!/usr/bin/env bash

set -Eeuo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$ROOT"

usage() {
  cat <<'EOF'
Usage: ./ops/upgrade-agent.sh --local [user@host ...] [options]

Options:
  --local              upgrade this machine
  --binary-dir DIR     use prebuilt architecture binaries from DIR
  --allow-dirty        build and identify modified executable inputs
EOF
}

TARGETS=()
DO_LOCAL=0
ALLOW_DIRTY=0
BIN_DIR=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --local) DO_LOCAL=1; shift ;;
    --allow-dirty) ALLOW_DIRTY=1; shift ;;
    --binary-dir) [[ $# -ge 2 ]] || { usage >&2; exit 2; }; BIN_DIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
    *) TARGETS+=("$1"); shift ;;
  esac
done

if [[ $DO_LOCAL -eq 0 && ${#TARGETS[@]} -eq 0 ]]; then
  usage >&2
  exit 2
fi
if [[ $EUID -eq 0 ]]; then
  echo "Run as the account that runs clawctl-agent" >&2
  exit 2
fi

VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
BUILD_DIRTY="$(git status --porcelain -- '*.go' 'go.mod' 'go.sum' '*.html' '*.sql' 2>/dev/null)"
if [[ -n "$BUILD_DIRTY" ]]; then
  VERSION="$VERSION-dirty"
fi
if [[ -n "$BUILD_DIRTY" && $ALLOW_DIRTY -eq 0 ]]; then
  printf 'Executable inputs are not committed:\n%s\n' "$BUILD_DIRTY" >&2
  echo "Commit them or use --allow-dirty" >&2
  exit 1
fi

if [[ -z "$BIN_DIR" ]]; then
  echo "Building agent: $VERSION"
  make cross VERSION="$VERSION" >/dev/null
  BIN_DIR="build"
fi
for arch in amd64 arm64; do
  [[ -f "$BIN_DIR/clawctl-agent-linux-$arch" && ! -L "$BIN_DIR/clawctl-agent-linux-$arch" ]] || {
    echo "Missing $BIN_DIR/clawctl-agent-linux-$arch" >&2
    exit 1
  }
done

arch_binary() {
  case "$1" in
    x86_64|amd64) echo "$BIN_DIR/clawctl-agent-linux-amd64" ;;
    aarch64|arm64) echo "$BIN_DIR/clawctl-agent-linux-arm64" ;;
    *) return 1 ;;
  esac
}

current_agent_contract() {
  cat <<'EOS'
set -Eeuo pipefail
config="$HOME/.config/clawctl/agent.json"
bin="$HOME/.local/bin/clawctl-agent"
unit="$HOME/.config/systemd/user/clawctl-agent.service"
[[ -f "$config" && ! -L "$config" && "$(stat -c '%a' "$config")" == "600" ]] || { echo "Enrolled agent config is missing" >&2; exit 1; }
[[ -f "$bin" && ! -L "$bin" ]] || { echo "Managed agent binary is missing" >&2; exit 1; }
[[ -f "$unit" && ! -L "$unit" ]] || { echo "Managed agent unit is missing" >&2; exit 1; }
systemctl --user is-enabled --quiet clawctl-agent.service || { echo "Agent service is not enabled" >&2; exit 1; }
systemctl --user is-active --quiet clawctl-agent.service || { echo "Agent service is not active" >&2; exit 1; }
fragment="$(systemctl --user show clawctl-agent.service -p FragmentPath --value)"
[[ "$fragment" == "$unit" ]] || { echo "Unit path mismatch: $fragment" >&2; exit 1; }
main_pid="$(systemctl --user show clawctl-agent.service -p MainPID --value)"
[[ "$main_pid" =~ ^[1-9][0-9]*$ ]] || { echo "Agent MainPID is missing" >&2; exit 1; }
[[ "$(ps -o uid= -p "$main_pid" | xargs)" == "$EUID" ]] || { echo "Agent process owner mismatch" >&2; exit 1; }
EOS
}

remote_script() {
  cat <<'EOS'
set -Eeuo pipefail
want=$1
bin="$HOME/.local/bin/clawctl-agent"
unit="$HOME/.config/systemd/user/clawctl-agent.service"
runtime="$HOME/.local/share/clawctl"
config="$HOME/.config/clawctl/agent.json"
hermes_unit="$HOME/.config/systemd/user/clawctl-hermes.service"
openclaw_unit="$HOME/.config/systemd/user/openclaw-gateway.service"
openclaw_dropin="$HOME/.config/systemd/user/openclaw-gateway.service.d"

[[ -f "$config" && ! -L "$config" && "$(stat -c '%a' "$config")" == "600" ]] || { echo "Enrolled agent config is missing" >&2; exit 1; }
[[ -f "$bin.new" && ! -L "$bin.new" ]] || { echo "Staged agent binary is missing" >&2; exit 1; }
[[ -f "$unit.new" && ! -L "$unit.new" ]] || { echo "Staged agent unit is missing" >&2; exit 1; }
[[ -f "$hermes_unit.new" && ! -L "$hermes_unit.new" ]] || { echo "Staged Hermes unit is missing" >&2; exit 1; }
[[ -f "$openclaw_unit.new" && ! -L "$openclaw_unit.new" ]] || { echo "Staged OpenClaw unit is missing" >&2; exit 1; }
[[ ! -L "$hermes_unit" && ! -L "$openclaw_unit" ]] || { echo "Managed runtime unit path is a symlink" >&2; exit 1; }
[[ ! -e "$hermes_unit" || -f "$hermes_unit" ]] || { echo "Hermes unit path is not a regular file" >&2; exit 1; }
[[ ! -e "$openclaw_unit" || -f "$openclaw_unit" ]] || { echo "OpenClaw unit path is not a regular file" >&2; exit 1; }
install -d -m 0700 "$runtime" "$runtime/openclaw" "$runtime/hermes" "$runtime/hermes/data" "$openclaw_dropin" "$HOME/.openclaw"
chmod 0755 "$bin.new"

got="$(timeout 5 "$bin.new" version 2>&1)" || {
  echo "Agent binary did not start: $got" >&2
  rm -f -- "$bin.new" "$unit.new" "$hermes_unit.new" "$openclaw_unit.new"
  exit 1
}
if [[ "$got" != "$want" ]]; then
  echo "Agent version mismatch: got=$got want=$want" >&2
  rm -f -- "$bin.new" "$unit.new" "$hermes_unit.new" "$openclaw_unit.new"
  exit 1
fi

chmod 0644 "$unit.new"
chmod 0644 "$hermes_unit.new" "$openclaw_unit.new"
mv -f "$unit.new" "$unit"
mv -f "$hermes_unit.new" "$hermes_unit"
if [[ ! -e "$openclaw_unit" && ! -L "$openclaw_unit" ]] ||
   grep -Fqx '# AI-Intune managed OpenClaw unit v1' "$openclaw_unit" 2>/dev/null; then
  mv -f "$openclaw_unit.new" "$openclaw_unit"
else
  rm -f -- "$openclaw_unit.new"
fi
systemctl --user daemon-reload
mv -f "$bin.new" "$bin"
systemctl --user reset-failed clawctl-agent.service 2>/dev/null || true
started_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
systemctl --user restart clawctl-agent.service
systemctl --user is-active --quiet clawctl-agent.service
"$bin" verify --since "$started_at" --timeout 2m

process_count="$(ps -eo comm= | awk '$1=="clawctl-agent" { n++ } END { print n+0 }')"
restarts="$(systemctl --user show clawctl-agent.service -p NRestarts --value)"
running_version="$(timeout 5 "$bin" version)"
writable_roots="$(systemctl --user show clawctl-agent.service -p ReadWritePaths --value)"
fragment="$(systemctl --user show clawctl-agent.service -p FragmentPath --value)"
need_reload="$(systemctl --user show clawctl-agent.service -p NeedDaemonReload --value)"
main_pid="$(systemctl --user show clawctl-agent.service -p MainPID --value)"
expected_roots="$HOME/.config/clawctl $openclaw_dropin $HOME/.cache/clawctl $HOME/.local/share/clawctl $HOME/.openclaw"

[[ "$running_version" == "$want" ]] || { echo "Running version mismatch: $running_version" >&2; exit 1; }
[[ "$fragment" == "$unit" ]] || { echo "Unit path mismatch: $fragment" >&2; exit 1; }
[[ "$writable_roots" == "$expected_roots" ]] || { echo "Writable roots mismatch: $writable_roots" >&2; exit 1; }
[[ "$need_reload" == "no" ]] || { echo "Unit reload is incomplete" >&2; exit 1; }
[[ "$restarts" == "0" ]] || { echo "Agent restart count: $restarts" >&2; exit 1; }
[[ "$process_count" == "1" ]] || { echo "Agent process count: $process_count" >&2; exit 1; }
[[ "$main_pid" =~ ^[1-9][0-9]*$ ]] || { echo "Agent MainPID is missing" >&2; exit 1; }
[[ "$(ps -o comm= -p "$main_pid" | xargs)" == "clawctl-agent" ]] || { echo "Agent MainPID identity mismatch" >&2; exit 1; }

echo "Managed: agent=$running_version service=active jobs=enabled processes=1 restarts=0"
EOS
}

upgrade_local() {
  local arch source
  arch="$(uname -m)"
  source="$(arch_binary "$arch")" || { echo "Unsupported architecture: $arch" >&2; return 1; }
  echo "Upgrading local agent: $arch"
  bash -s <<<"$(current_agent_contract)" || return
  install -d -m 0755 "$HOME/.local/bin" "$HOME/.config/systemd/user"
  install -m 0755 "$source" "$HOME/.local/bin/clawctl-agent.new"
  install -m 0644 ops/clawctl-agent.service "$HOME/.config/systemd/user/clawctl-agent.service.new"
  install -m 0644 ops/clawctl-hermes.service "$HOME/.config/systemd/user/clawctl-hermes.service.new"
  install -m 0644 ops/openclaw-gateway.service "$HOME/.config/systemd/user/openclaw-gateway.service.new"
  bash -s "$VERSION" <<<"$(remote_script)"
}

upgrade_remote() {
  local target=$1 arch source
  echo "Upgrading agent: $target"
  arch="$(timeout 20 ssh -o BatchMode=yes -o ConnectTimeout=8 "$target" 'uname -m')" || {
    echo "SSH unavailable: $target" >&2
    return 1
  }
  source="$(arch_binary "$arch")" || { echo "Unsupported architecture on $target: $arch" >&2; return 1; }
  timeout 20 ssh -o BatchMode=yes "$target" 'bash -s' <<<"$(current_agent_contract)" || {
    echo "Managed agent preflight failed: $target" >&2
    return 1
  }
  timeout 20 ssh -o BatchMode=yes "$target" \
    'install -d -m 0755 "$HOME/.local/bin" "$HOME/.config/systemd/user" && install -d -m 0700 "$HOME/.local/share/clawctl"' || {
    echo "Agent directories failed: $target" >&2
    return 1
  }
  scp -q -o BatchMode=yes "$source" "$target:.local/bin/clawctl-agent.new" || {
    echo "Agent binary transfer failed: $target" >&2
    return 1
  }
  scp -q -o BatchMode=yes ops/clawctl-agent.service "$target:.config/systemd/user/clawctl-agent.service.new" || {
    echo "Agent unit transfer failed: $target" >&2
    return 1
  }
  scp -q -o BatchMode=yes ops/clawctl-hermes.service "$target:.config/systemd/user/clawctl-hermes.service.new" || {
    echo "Hermes unit transfer failed: $target" >&2
    return 1
  }
  scp -q -o BatchMode=yes ops/openclaw-gateway.service "$target:.config/systemd/user/openclaw-gateway.service.new" || {
    echo "OpenClaw unit transfer failed: $target" >&2
    return 1
  }
  timeout 180 ssh -o BatchMode=yes "$target" "bash -s '$VERSION'" <<<"$(remote_script)"
}

failures=0
if [[ $DO_LOCAL -eq 1 ]]; then
  upgrade_local || failures=$((failures + 1))
fi
for target in "${TARGETS[@]}"; do
  upgrade_remote "$target" || failures=$((failures + 1))
done

if [[ $failures -ne 0 ]]; then
  echo "Agent upgrades failed: $failures" >&2
  exit 1
fi
echo "Fleet ready: agent=$VERSION machines=$((DO_LOCAL + ${#TARGETS[@]}))"
