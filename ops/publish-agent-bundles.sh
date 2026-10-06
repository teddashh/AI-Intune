#!/usr/bin/env bash

set -euo pipefail

VERSION=""
SOURCE_DIR=""
STATE_DIR=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --source-dir) SOURCE_DIR="${2:-}"; shift 2 ;;
    --state-dir) STATE_DIR="${2:-}"; shift 2 ;;
    -h|--help)
      echo "usage: publish-agent-bundles.sh --version VERSION --source-dir DIR --state-dir DIR"
      exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ ! "$VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$ ]]; then
  echo "Invalid Agent bootstrap version." >&2
  exit 2
fi
if [[ -z "$SOURCE_DIR" || -L "$SOURCE_DIR" || ! -d "$SOURCE_DIR" ]] ||
   ! SOURCE_DIR="$(realpath -e -- "$SOURCE_DIR")"; then
  echo "Invalid Agent bootstrap source directory." >&2
  exit 2
fi
if [[ -z "$STATE_DIR" || "$STATE_DIR" != /* || -L "$STATE_DIR" || ! -d "$STATE_DIR" || ! -O "$STATE_DIR" ]] ||
   [[ "$(realpath -e -- "$STATE_DIR")" != "$STATE_DIR" ]]; then
  echo "Invalid Hub state directory." >&2
  exit 2
fi
state_mode="$(stat -c '%a' -- "$STATE_DIR")"
if (( (8#$state_mode & 8#022) != 0 )); then
  echo "Hub state directory is writable by group or other." >&2
  exit 2
fi

LINUX_AMD64="clawctl-agent-bootstrap-linux-amd64.tar.gz"
LINUX_ARM64="clawctl-agent-bootstrap-linux-arm64.tar.gz"
DARWIN_AMD64="clawctl-agent-bootstrap-darwin-amd64.tar.gz"
DARWIN_ARM64="clawctl-agent-bootstrap-darwin-arm64.tar.gz"
WINDOWS_AMD64="clawctl-agent-bootstrap-windows-amd64.tar.gz"
WINDOWS_ARM64="clawctl-agent-bootstrap-windows-arm64.tar.gz"

declare -A SOURCE_SHA
declare -A SOURCE_FILE
source_has_darwin=0
source_has_windows=0
verify_root="${TMPDIR:-/tmp}"
if [[ "$verify_root" != /* ]]; then
  echo "Invalid publisher verify directory." >&2
  exit 2
fi
if ! verify_root="$(realpath -e -- "$verify_root")" ||
   [[ -z "$verify_root" || "$verify_root" == / || ! -d "$verify_root" ]]; then
  echo "Invalid publisher verify directory." >&2
  exit 2
fi
VERIFY_DIR="$(mktemp -d -- "$verify_root/clawctl-publish.XXXXXX")"
STAGE_DIR=""
cleanup() {
  if [[ -n "${VERIFY_DIR:-}" && -n "${verify_root:-}" &&
        "$VERIFY_DIR" == "$verify_root"/clawctl-publish.* &&
        "$VERIFY_DIR" != "$verify_root" &&
        -d "$VERIFY_DIR" && ! -L "$VERIFY_DIR" ]]; then
    rm -rf -- "$VERIFY_DIR"
  fi
  if [[ -n "$STAGE_DIR" && "$STAGE_DIR" == "$STATE_DIR/agent-bootstrap/.publish.$VERSION."* &&
        -d "$STAGE_DIR" && ! -L "$STAGE_DIR" ]]; then
    rm -rf -- "$STAGE_DIR"
  fi
}
trap cleanup EXIT

path_present() {
  [[ -e "$1" || -L "$1" ]]
}

selected_checksum_manifest() {
  printf '%s  %s\n' "${SOURCE_SHA[$LINUX_AMD64]}" "$LINUX_AMD64"
  printf '%s  %s\n' "${SOURCE_SHA[$LINUX_ARM64]}" "$LINUX_ARM64"
  if [[ "$source_has_darwin" -eq 1 ]]; then
    printf '%s  %s\n' "${SOURCE_SHA[$DARWIN_AMD64]}" "$DARWIN_AMD64"
    printf '%s  %s\n' "${SOURCE_SHA[$DARWIN_ARM64]}" "$DARWIN_ARM64"
  fi
  if [[ "$source_has_windows" -eq 1 ]]; then
    printf '%s  %s\n' "${SOURCE_SHA[$WINDOWS_AMD64]}" "$WINDOWS_AMD64"
    printf '%s  %s\n' "${SOURCE_SHA[$WINDOWS_ARM64]}" "$WINDOWS_ARM64"
  fi
}

validate_source_bundle() {
  local os="$1" arch="$2" filename="$3" bundle extract size prefix machine entry entry_size expanded=0
  local -a listing
  bundle="$SOURCE_DIR/$filename"
  extract="$VERIFY_DIR/$os-$arch"
  [[ -f "$bundle" && ! -L "$bundle" && "$(stat -c '%h' -- "$bundle")" == 1 ]] || {
    echo "Missing or linked $filename." >&2
    return 1
  }
  size="$(stat -c '%s' -- "$bundle")"
  (( size > 0 && size <= 64 * 1024 * 1024 )) || { echo "Invalid size for $filename." >&2; return 1; }
  gzip -t -- "$bundle"
  mapfile -t listing < <(tar -tvzf "$bundle")
  if [[ "$os" == linux ]]; then
    [[ ${#listing[@]} -eq 6 ]] || { echo "Invalid archive entries in $filename." >&2; return 1; }
    [[ "${listing[0]%% *}" == "-rw-r--r--" && "${listing[0]##* }" == "VERSION" &&
       "${listing[1]%% *}" == "-rwxr-xr-x" && "${listing[1]##* }" == "clawctl-agent" &&
       "${listing[2]%% *}" == "-rw-r--r--" && "${listing[2]##* }" == "clawctl-agent.service" &&
       "${listing[3]%% *}" == "-rw-r--r--" && "${listing[3]##* }" == "clawctl-hermes.service" &&
       "${listing[4]%% *}" == "-rwxr-xr-x" && "${listing[4]##* }" == "install-agent.sh" &&
       "${listing[5]%% *}" == "-rw-r--r--" && "${listing[5]##* }" == "openclaw-gateway.service" ]] || {
      echo "Invalid archive layout in $filename." >&2
      return 1
    }
  elif [[ "$os" == darwin ]]; then
    [[ ${#listing[@]} -eq 4 ]] || { echo "Invalid archive entries in $filename." >&2; return 1; }
    [[ "${listing[0]%% *}" == "-rw-r--r--" && "${listing[0]##* }" == "VERSION" &&
       "${listing[1]%% *}" == "-rwxr-xr-x" && "${listing[1]##* }" == "clawctl-agent" &&
       "${listing[2]%% *}" == "-rw-r--r--" && "${listing[2]##* }" == "clawctl-agent.plist" &&
       "${listing[3]%% *}" == "-rwxr-xr-x" && "${listing[3]##* }" == "install-agent-macos.sh" ]] || {
      echo "Invalid archive layout in $filename." >&2
      return 1
    }
  else
    [[ ${#listing[@]} -eq 4 ]] || { echo "Invalid archive entries in $filename." >&2; return 1; }
    [[ "${listing[0]%% *}" == "-rw-r--r--" && "${listing[0]##* }" == "VERSION" &&
       "${listing[1]%% *}" == "-rwxr-xr-x" && "${listing[1]##* }" == "clawctl-agent.exe" &&
       "${listing[2]%% *}" == "-rw-r--r--" && "${listing[2]##* }" == "clawctl-agent.task.xml" &&
       "${listing[3]%% *}" == "-rwxr-xr-x" && "${listing[3]##* }" == "install-agent-windows.ps1" ]] || {
      echo "Invalid archive layout in $filename." >&2
      return 1
    }
  fi
  for entry in "${listing[@]}"; do
    read -r _ _ entry_size _ <<<"$entry"
    [[ "$entry_size" =~ ^[0-9]+$ ]] && (( entry_size > 0 && entry_size <= 64 * 1024 * 1024 )) || {
      echo "Invalid expanded size in $filename." >&2
      return 1
    }
    expanded=$((expanded + entry_size))
    (( expanded <= 128 * 1024 * 1024 )) || { echo "Archive too large in $filename." >&2; return 1; }
  done
  mkdir -m 0700 -- "$extract"
  tar --no-same-owner --no-same-permissions -xzf "$bundle" -C "$extract"
  if [[ "$os" == linux ]]; then
    [[ -f "$extract/VERSION" && ! -L "$extract/VERSION" && "$(<"$extract/VERSION")" == "$VERSION" &&
       -f "$extract/clawctl-agent" && ! -L "$extract/clawctl-agent" &&
       -f "$extract/clawctl-agent.service" && ! -L "$extract/clawctl-agent.service" &&
       -f "$extract/clawctl-hermes.service" && ! -L "$extract/clawctl-hermes.service" &&
       -f "$extract/install-agent.sh" && ! -L "$extract/install-agent.sh" &&
       -f "$extract/openclaw-gateway.service" && ! -L "$extract/openclaw-gateway.service" ]] || {
      echo "Invalid extracted files in $filename." >&2
      return 1
    }
    prefix="$(od -An -t x1 -N20 -- "$extract/clawctl-agent" | tr -d ' \n')"
    [[ "${prefix:0:12}" == "7f454c460201" ]] || { echo "Invalid Agent ELF in $filename." >&2; return 1; }
    machine="${prefix:36:4}"
    if [[ ( "$arch" == amd64 && "$machine" != 3e00 ) ||
          ( "$arch" == arm64 && "$machine" != b700 ) ]]; then
      echo "Agent architecture mismatch in $filename." >&2
      return 1
    fi
  elif [[ "$os" == darwin ]]; then
    [[ -f "$extract/VERSION" && ! -L "$extract/VERSION" && "$(<"$extract/VERSION")" == "$VERSION" &&
       -f "$extract/clawctl-agent" && ! -L "$extract/clawctl-agent" &&
       -f "$extract/clawctl-agent.plist" && ! -L "$extract/clawctl-agent.plist" &&
       -f "$extract/install-agent-macos.sh" && ! -L "$extract/install-agent-macos.sh" ]] || {
      echo "Invalid extracted files in $filename." >&2
      return 1
    }
    prefix="$(od -An -t x1 -N16 -- "$extract/clawctl-agent" | tr -d ' \n')"
    [[ "${prefix:0:8}" != "7f454c46" ]] || { echo "Invalid Agent Mach-O in $filename." >&2; return 1; }
    [[ "${prefix:0:8}" == "cffaedfe" ]] || { echo "Invalid Agent Mach-O in $filename." >&2; return 1; }
    [[ "$(stat -c '%s' -- "$extract/clawctl-agent")" -ge 32 && "${prefix:24:8}" == "02000000" ]] || {
      echo "Invalid Agent Mach-O executable in $filename." >&2
      return 1
    }
    machine="${prefix:8:8}"
    if [[ ( "$arch" == amd64 && "$machine" != "07000001" ) ||
          ( "$arch" == arm64 && "$machine" != "0c000001" ) ]]; then
      echo "Agent architecture mismatch in $filename." >&2
      return 1
    fi
  else
    [[ -f "$extract/VERSION" && ! -L "$extract/VERSION" && "$(<"$extract/VERSION")" == "$VERSION" &&
       -f "$extract/clawctl-agent.exe" && ! -L "$extract/clawctl-agent.exe" &&
       -f "$extract/clawctl-agent.task.xml" && ! -L "$extract/clawctl-agent.task.xml" &&
       -f "$extract/install-agent-windows.ps1" && ! -L "$extract/install-agent-windows.ps1" ]] || {
      echo "Invalid extracted files in $filename." >&2
      return 1
    }
    [[ "$(od -An -t x1 -N2 -- "$extract/clawctl-agent.exe" | tr -d ' \n')" == 4d5a ]] || {
      echo "Invalid Agent PE in $filename." >&2
      return 1
    }
    local pe_offset
    pe_offset="$(od -An -t u4 -j 60 -N4 -- "$extract/clawctl-agent.exe" | tr -d ' ')"
    [[ "$pe_offset" =~ ^[0-9]+$ ]] && (( pe_offset >= 64 && pe_offset <= 1024 )) || {
      echo "Invalid Agent PE in $filename." >&2
      return 1
    }
    [[ "$(od -An -t x1 -j "$pe_offset" -N4 -- "$extract/clawctl-agent.exe" | tr -d ' \n')" == 50450000 ]] || {
      echo "Invalid Agent PE in $filename." >&2
      return 1
    }
    machine="$(od -An -t x1 -j $((pe_offset + 4)) -N2 -- "$extract/clawctl-agent.exe" | tr -d ' \n')"
    if [[ ( "$arch" == amd64 && "$machine" != 6486 ) ||
          ( "$arch" == arm64 && "$machine" != 64aa ) ]]; then
      echo "Agent architecture mismatch in $filename." >&2
      return 1
    fi
    local characteristics
    characteristics="$(od -An -t u2 -j $((pe_offset + 22)) -N2 -- "$extract/clawctl-agent.exe" | tr -d ' ')"
    [[ "$characteristics" =~ ^[0-9]+$ ]] || { echo "Invalid Agent PE executable in $filename." >&2; return 1; }
    if (( (characteristics & 2) == 0 || (characteristics & 8192) != 0 )); then
      echo "Invalid Agent PE executable in $filename." >&2
      return 1
    fi
  fi
  if [[ "$os" == darwin ]]; then
    head -c 2 "$extract/install-agent-macos.sh" | grep -Fq '#!' || { echo "Invalid macOS installer." >&2; return 1; }
    for entry in Darwin launchctl clawctl-agent.plist --hub com.clawctl.agent; do
      grep -Fq -- "$entry" "$extract/install-agent-macos.sh" || { echo "Invalid macOS installer." >&2; return 1; }
    done
    for entry in '<?xml' '<plist' '<key>Label</key>' com.clawctl.agent '<key>KeepAlive</key>' '@@CLAWCTL_AGENT_BIN@@' '@@CLAWCTL_AGENT_LOG@@'; do
      grep -Fq -- "$entry" "$extract/clawctl-agent.plist" || { echo "Invalid macOS plist." >&2; return 1; }
    done
    if grep -Fq '[Unit]' "$extract/clawctl-agent.plist"; then
      echo "Invalid macOS plist." >&2
      return 1
    fi
  fi
  if [[ "$os" == windows ]]; then
    for entry in Windows Register-ScheduledTask clawctl-agent.task.xml --hub clawctl-agent --require-platform-evidence; do
      grep -Fq -- "$entry" "$extract/install-agent-windows.ps1" || { echo "Invalid Windows installer." >&2; return 1; }
    done
    if grep -Fq launchctl "$extract/install-agent-windows.ps1"; then
      echo "Invalid Windows installer." >&2
      return 1
    fi
    for entry in '<?xml' '<Task' '<LogonTrigger>' InteractiveToken LeastPrivilege '@@CLAWCTL_AGENT_BIN@@' '\clawctl\clawctl-agent'; do
      grep -Fq -- "$entry" "$extract/clawctl-agent.task.xml" || { echo "Invalid Windows task XML." >&2; return 1; }
    done
    if grep -Fq '[Unit]' "$extract/clawctl-agent.task.xml" || grep -Fq '<plist' "$extract/clawctl-agent.task.xml"; then
      echo "Invalid Windows task XML." >&2
      return 1
    fi
  fi
  SOURCE_FILE["$filename"]="$bundle"
  SOURCE_SHA["$filename"]="$(sha256sum "$bundle" | awk '{print $1}')"
}

validate_existing_release_file() {
  local filename="$1" target
  target="$TARGET_DIR/$filename"
  [[ -f "$target" && ! -L "$target" && "$(stat -c '%h' -- "$target")" == 1 ]] || {
    echo "Existing Agent bootstrap release is incomplete." >&2
    return 1
  }
  [[ "$(sha256sum "$target" | awk '{print $1}')" == "${SOURCE_SHA[$filename]}" ]] || {
    echo "Agent bootstrap version already contains different bytes." >&2
    return 1
  }
}

validate_source_bundle linux amd64 "$LINUX_AMD64"
validate_source_bundle linux arm64 "$LINUX_ARM64"
if path_present "$SOURCE_DIR/$DARWIN_AMD64" || path_present "$SOURCE_DIR/$DARWIN_ARM64"; then
  source_has_darwin=1
  validate_source_bundle darwin amd64 "$DARWIN_AMD64"
  validate_source_bundle darwin arm64 "$DARWIN_ARM64"
fi
if path_present "$SOURCE_DIR/$WINDOWS_AMD64" || path_present "$SOURCE_DIR/$WINDOWS_ARM64"; then
  source_has_windows=1
  validate_source_bundle windows amd64 "$WINDOWS_AMD64"
  validate_source_bundle windows arm64 "$WINDOWS_ARM64"
fi

RELEASE_ROOT="$STATE_DIR/agent-bootstrap"
if [[ -e "$RELEASE_ROOT" || -L "$RELEASE_ROOT" ]]; then
  if [[ -L "$RELEASE_ROOT" || ! -d "$RELEASE_ROOT" || ! -O "$RELEASE_ROOT" ||
        "$(realpath -e -- "$RELEASE_ROOT")" != "$RELEASE_ROOT" ]]; then
    echo "Invalid Agent bootstrap release directory." >&2
    exit 1
  fi
  chmod 0700 -- "$RELEASE_ROOT"
else
  mkdir -m 0700 -- "$RELEASE_ROOT"
fi

TARGET_DIR="$RELEASE_ROOT/$VERSION"
if [[ -e "$TARGET_DIR" || -L "$TARGET_DIR" ]]; then
  if [[ -L "$TARGET_DIR" || ! -d "$TARGET_DIR" || ! -O "$TARGET_DIR" ]]; then
    echo "Invalid existing Agent bootstrap release." >&2
    exit 1
  fi
  target_mode="$(stat -c '%a' -- "$TARGET_DIR")"
  if (( (8#$target_mode & 8#022) != 0 )); then
    echo "Existing Agent bootstrap release is writable by group or other." >&2
    exit 1
  fi
  existing_has_darwin=0
  existing_has_windows=0
  if path_present "$TARGET_DIR/$DARWIN_AMD64" || path_present "$TARGET_DIR/$DARWIN_ARM64"; then
    existing_has_darwin=1
  fi
  if path_present "$TARGET_DIR/$WINDOWS_AMD64" || path_present "$TARGET_DIR/$WINDOWS_ARM64"; then
    existing_has_windows=1
  fi
  if [[ "$source_has_darwin" -ne "$existing_has_darwin" ||
        "$source_has_windows" -ne "$existing_has_windows" ]]; then
    echo "Agent bootstrap version already contains a different platform set." >&2
    exit 1
  fi
  validate_existing_release_file "$LINUX_AMD64"
  validate_existing_release_file "$LINUX_ARM64"
  if [[ "$existing_has_darwin" -eq 1 ]]; then
    validate_existing_release_file "$DARWIN_AMD64"
    validate_existing_release_file "$DARWIN_ARM64"
  fi
  if [[ "$existing_has_windows" -eq 1 ]]; then
    validate_existing_release_file "$WINDOWS_AMD64"
    validate_existing_release_file "$WINDOWS_ARM64"
  fi
  expected_sums="$(selected_checksum_manifest)"
  [[ -f "$TARGET_DIR/SHA256SUMS" && ! -L "$TARGET_DIR/SHA256SUMS" &&
     "$(stat -c '%h' -- "$TARGET_DIR/SHA256SUMS")" == 1 &&
     "$(<"$TARGET_DIR/SHA256SUMS")" == "$expected_sums" ]] || {
    echo "Existing Agent bootstrap checksum manifest is invalid." >&2
    exit 1
  }
  echo "Agent bootstrap $VERSION already published."
  exit 0
fi

STAGE_DIR="$(mktemp -d "$RELEASE_ROOT/.publish.$VERSION.XXXXXX")"
chmod 0700 -- "$STAGE_DIR"
selected=("$LINUX_AMD64" "$LINUX_ARM64")
if [[ "$source_has_darwin" -eq 1 ]]; then
  selected+=("$DARWIN_AMD64" "$DARWIN_ARM64")
fi
if [[ "$source_has_windows" -eq 1 ]]; then
  selected+=("$WINDOWS_AMD64" "$WINDOWS_ARM64")
fi
for filename in "${selected[@]}"; do
  install -m 0644 -- "${SOURCE_FILE[$filename]}" "$STAGE_DIR/$filename"
  copied_sha="$(sha256sum "$STAGE_DIR/$filename" | awk '{print $1}')"
  [[ "$copied_sha" == "${SOURCE_SHA[$filename]}" ]] || { echo "Agent bootstrap copy verification failed." >&2; exit 1; }
  /usr/bin/sync -f -- "$STAGE_DIR/$filename"
done
selected_checksum_manifest >"$STAGE_DIR/SHA256SUMS"
chmod 0644 "$STAGE_DIR/SHA256SUMS"
/usr/bin/sync -f -- "$STAGE_DIR/SHA256SUMS"
/usr/bin/sync -f -- "$STAGE_DIR"
mv -T -- "$STAGE_DIR" "$TARGET_DIR"
STAGE_DIR=""
/usr/bin/sync -f -- "$RELEASE_ROOT"
echo "Agent bootstrap $VERSION published."
