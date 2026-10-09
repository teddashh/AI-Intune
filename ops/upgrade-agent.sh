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
bin="${CLAWCTL_SYSTEM_BIN_DIR:-/usr/local/bin}/clawctl-agent"
legacy_bin="$HOME/.local/bin/clawctl-agent"
system_unit="${CLAWCTL_SYSTEM_UNIT_DIR:-/etc/systemd/system}/clawctl-agent.service"
legacy_unit="$HOME/.config/systemd/user/clawctl-agent.service"
[[ -f "$config" && ! -L "$config" && "$(stat -c '%a' "$config")" == "600" ]] || { echo "Enrolled agent config is missing" >&2; exit 1; }
[[ ( -f "$bin" && ! -L "$bin" ) || ( -f "$legacy_bin" && ! -L "$legacy_bin" ) ]] || { echo "Managed agent binary is missing" >&2; exit 1; }
command -v sudo >/dev/null || { echo "sudo is required" >&2; exit 1; }
sudo -n true || { echo "Passwordless or pre-authorized sudo is required" >&2; exit 1; }
if [[ -f "$system_unit" && ! -L "$system_unit" ]]; then
  systemctl is-enabled --quiet clawctl-agent.service || { echo "Agent system service is not enabled" >&2; exit 1; }
  systemctl is-active --quiet clawctl-agent.service || { echo "Agent system service is not active" >&2; exit 1; }
  fragment="$(systemctl show clawctl-agent.service -p FragmentPath --value)"
  [[ "$fragment" == "$system_unit" ]] || { echo "System unit path mismatch: $fragment" >&2; exit 1; }
  main_pid="$(systemctl show clawctl-agent.service -p MainPID --value)"
elif [[ -f "$legacy_unit" && ! -L "$legacy_unit" ]]; then
  systemctl --user is-enabled --quiet clawctl-agent.service || { echo "Legacy agent user service is not enabled" >&2; exit 1; }
  systemctl --user is-active --quiet clawctl-agent.service || { echo "Legacy agent user service is not active" >&2; exit 1; }
  fragment="$(systemctl --user show clawctl-agent.service -p FragmentPath --value)"
  [[ "$fragment" == "$legacy_unit" ]] || { echo "Legacy unit path mismatch: $fragment" >&2; exit 1; }
  main_pid="$(systemctl --user show clawctl-agent.service -p MainPID --value)"
else
  echo "Managed agent system or legacy user unit is missing" >&2
  exit 1
fi
[[ "$main_pid" =~ ^[1-9][0-9]*$ ]] || { echo "Agent MainPID is missing" >&2; exit 1; }
[[ "$(ps -o uid= -p "$main_pid" | xargs)" == "$EUID" ]] || { echo "Agent process owner mismatch" >&2; exit 1; }
EOS
}

remote_script() {
  cat <<'EOS'
set -Eeuo pipefail
want=$1
bin="$HOME/.local/bin/clawctl-agent"
system_bin_dir="${CLAWCTL_SYSTEM_BIN_DIR:-/usr/local/bin}"
system_bin="$system_bin_dir/clawctl-agent"
[[ "$system_bin_dir" =~ ^/[a-zA-Z0-9._/-]+$ && "$system_bin_dir" != *"/../"* && "$system_bin_dir" != */.. ]] || { echo "Unsafe system binary directory" >&2; exit 1; }
raw_unit="$HOME/.local/share/clawctl/clawctl-agent.service.new"
system_unit="${CLAWCTL_SYSTEM_UNIT_DIR:-/etc/systemd/system}/clawctl-agent.service"
legacy_unit="$HOME/.config/systemd/user/clawctl-agent.service"
runtime="$HOME/.local/share/clawctl"
config="$HOME/.config/clawctl/agent.json"
hermes_unit="$HOME/.config/systemd/user/clawctl-hermes.service"
openclaw_unit="$HOME/.config/systemd/user/openclaw-gateway.service"
openclaw_dropin="$HOME/.config/systemd/user/openclaw-gateway.service.d"

[[ -f "$config" && ! -L "$config" && "$(stat -c '%a' "$config")" == "600" ]] || { echo "Enrolled agent config is missing" >&2; exit 1; }
[[ -f "$bin.new" && ! -L "$bin.new" ]] || { echo "Staged agent binary is missing" >&2; exit 1; }
[[ -f "$raw_unit" && ! -L "$raw_unit" ]] || { echo "Staged agent unit is missing" >&2; exit 1; }
[[ -f "$hermes_unit.new" && ! -L "$hermes_unit.new" ]] || { echo "Staged Hermes unit is missing" >&2; exit 1; }
[[ -f "$openclaw_unit.new" && ! -L "$openclaw_unit.new" ]] || { echo "Staged OpenClaw unit is missing" >&2; exit 1; }
[[ ! -L "$hermes_unit" && ! -L "$openclaw_unit" ]] || { echo "Managed runtime unit path is a symlink" >&2; exit 1; }
[[ ! -e "$hermes_unit" || -f "$hermes_unit" ]] || { echo "Hermes unit path is not a regular file" >&2; exit 1; }
[[ ! -e "$openclaw_unit" || -f "$openclaw_unit" ]] || { echo "OpenClaw unit path is not a regular file" >&2; exit 1; }
install -d -m 0700 "$runtime" "$runtime/openclaw" "$runtime/hermes" "$runtime/hermes/data" "$openclaw_dropin" "$HOME/.openclaw"
chmod 0755 "$bin.new"

agent_user="$(id -un)"
agent_uid="$(id -u)"
[[ "$agent_user" =~ ^[a-zA-Z_][a-zA-Z0-9_.-]*\$?$ ]] || { echo "Unsupported agent user name: $agent_user" >&2; exit 1; }
[[ "$agent_uid" =~ ^[1-9][0-9]*$ ]] || { echo "Agent must use a non-root numeric UID" >&2; exit 1; }
[[ "$HOME" =~ ^/[a-zA-Z0-9._/-]+$ && "$HOME" != *"/../"* && "$HOME" != */.. ]] || { echo "Agent home cannot be safely rendered into systemd: $HOME" >&2; exit 1; }
rendered_unit="$runtime/clawctl-agent.service.rendered"
trap 'rm -f -- "$rendered_unit"' EXIT
escaped_home=${HOME//\\/\\\\}
escaped_home=${escaped_home//&/\\&}
sed -e "s|@@CLAWCTL_AGENT_USER@@|$agent_user|g" \
    -e "s|@@CLAWCTL_AGENT_UID@@|$agent_uid|g" \
    -e "s|__CLAWCTL_AGENT_UID__|$agent_uid|g" \
    -e "s|@@CLAWCTL_AGENT_HOME@@|$escaped_home|g" \
    -e "s|@@CLAWCTL_AGENT_BIN@@|$system_bin|g" \
    "$raw_unit" >"$rendered_unit"
! grep -Fq 'CLAWCTL_AGENT_' "$rendered_unit" || { echo "Agent systemd unit rendering is incomplete" >&2; exit 1; }
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
resolve_unit_rw_paths "$rendered_unit" || { echo "Agent systemd unit ReadWritePaths could not be resolved" >&2; exit 1; }

got="$(timeout 5 "$bin.new" version 2>&1)" || {
  echo "Agent binary did not start: $got" >&2
  rm -f -- "$bin.new" "$raw_unit" "$hermes_unit.new" "$openclaw_unit.new"
  exit 1
}
if [[ "$got" != "$want" ]]; then
  echo "Agent version mismatch: got=$got want=$want" >&2
  rm -f -- "$bin.new" "$raw_unit" "$hermes_unit.new" "$openclaw_unit.new"
  exit 1
fi

sudo install -d -m 0755 "$system_bin_dir" "$(dirname "$system_unit")"
sudo install -m 0755 -o root -g root "$bin.new" "$system_bin.new"
sudo mv -f "$system_bin.new" "$system_bin"
if command -v restorecon >/dev/null &&
   { { command -v selinuxenabled >/dev/null && selinuxenabled; } || [[ -e /sys/fs/selinux/enforce ]]; }; then
  sudo restorecon -F "$system_bin"
fi
rm -f -- "$bin.new"
if [[ -f "$bin" ]]; then echo "Legacy binary $bin retained; no longer used by the system unit."; fi
chmod 0644 "$hermes_unit.new" "$openclaw_unit.new"
sudo install -m 0644 "$rendered_unit" "$system_unit.new"
sudo mv -f "$system_unit.new" "$system_unit"
rm -f -- "$raw_unit"
mv -f "$hermes_unit.new" "$hermes_unit"
if [[ ! -e "$openclaw_unit" && ! -L "$openclaw_unit" ]] ||
   grep -Fqx '# AI-Intune managed OpenClaw unit v1' "$openclaw_unit" 2>/dev/null; then
  mv -f "$openclaw_unit.new" "$openclaw_unit"
else
  rm -f -- "$openclaw_unit.new"
fi
sudo systemctl daemon-reload
[[ "$(systemctl show clawctl-agent.service -p LoadState --value)" == "loaded" ]] || { echo "Agent system unit did not load" >&2; exit 1; }
[[ "$(systemctl show clawctl-agent.service -p FragmentPath --value)" == "$system_unit" ]] || { echo "Agent system unit path mismatch before migration" >&2; exit 1; }
[[ "$(systemctl show clawctl-agent.service -p User --value)" == "$agent_user" ]] || { echo "Agent systemd User contract mismatch before migration" >&2; exit 1; }
[[ "$(systemctl show clawctl-agent.service -p ProtectSystem --value)" == "strict" ]] || { echo "ProtectSystem sandbox is not loaded before migration" >&2; exit 1; }
[[ "$(systemctl show clawctl-agent.service -p ProtectHome --value)" == "read-only" ]] || { echo "ProtectHome sandbox is not loaded before migration" >&2; exit 1; }
sudo systemctl enable clawctl-agent.service
if [[ -e "$legacy_unit" || -L "$legacy_unit" ]]; then
  [[ -f "$legacy_unit" && ! -L "$legacy_unit" ]] || { echo "Legacy agent unit path is not safe" >&2; exit 1; }
  systemctl --user disable --now clawctl-agent.service
  rm -f -- "$legacy_unit"
  systemctl --user daemon-reload
fi
sudo systemctl reset-failed clawctl-agent.service 2>/dev/null || true
started_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
sudo systemctl restart clawctl-agent.service
systemctl is-active --quiet clawctl-agent.service
"$system_bin" verify --since "$started_at" --timeout 2m

process_count="$(ps -eo comm= | awk '$1=="clawctl-agent" { n++ } END { print n+0 }')"
restarts="$(systemctl show clawctl-agent.service -p NRestarts --value)"
running_version="$(timeout 5 "$system_bin" version)"
service_user="$(systemctl show clawctl-agent.service -p User --value)"
protect_system="$(systemctl show clawctl-agent.service -p ProtectSystem --value)"
protect_home="$(systemctl show clawctl-agent.service -p ProtectHome --value)"
writable_roots="$(systemctl show clawctl-agent.service -p ReadWritePaths --value)"
fragment="$(systemctl show clawctl-agent.service -p FragmentPath --value)"
need_reload="$(systemctl show clawctl-agent.service -p NeedDaemonReload --value)"
main_pid="$(systemctl show clawctl-agent.service -p MainPID --value)"
expected_roots="$HOME/.config/clawctl $openclaw_dropin $HOME/.cache/clawctl $HOME/.local/share/clawctl $HOME/.openclaw"

[[ "$running_version" == "$want" ]] || { echo "Running version mismatch: $running_version" >&2; exit 1; }
[[ "$fragment" == "$system_unit" ]] || { echo "System unit path mismatch: $fragment" >&2; exit 1; }
[[ "$service_user" == "$agent_user" ]] || { echo "Agent service user mismatch: $service_user" >&2; exit 1; }
[[ "$protect_system" == "strict" ]] || { echo "ProtectSystem sandbox mismatch: $protect_system" >&2; exit 1; }
[[ "$protect_home" == "read-only" ]] || { echo "ProtectHome sandbox mismatch: $protect_home" >&2; exit 1; }
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
  sudo -n true 2>/dev/null || sudo -v || return
  bash -s <<<"$(current_agent_contract)" || return
  install -d -m 0755 "$HOME/.local/bin" "$HOME/.config/systemd/user"
  install -d -m 0700 "$HOME/.local/share/clawctl"
  install -m 0755 "$source" "$HOME/.local/bin/clawctl-agent.new"
  install -m 0644 ops/clawctl-agent.service "$HOME/.local/share/clawctl/clawctl-agent.service.new"
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
  # shellcheck disable=SC2016
  timeout 20 ssh -o BatchMode=yes "$target" \
    'install -d -m 0755 "$HOME/.local/bin" "$HOME/.config/systemd/user" && install -d -m 0700 "$HOME/.local/share/clawctl"' || {
    echo "Agent directories failed: $target" >&2
    return 1
  }
  scp -q -o BatchMode=yes "$source" "$target:.local/bin/clawctl-agent.new" || {
    echo "Agent binary transfer failed: $target" >&2
    return 1
  }
  scp -q -o BatchMode=yes ops/clawctl-agent.service "$target:.local/share/clawctl/clawctl-agent.service.new" || {
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
