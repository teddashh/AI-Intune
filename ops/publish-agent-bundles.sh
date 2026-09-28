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

declare -A SOURCE_SHA
declare -A SOURCE_FILE
VERIFY_DIR="$(mktemp -d)"
STAGE_DIR=""
cleanup() {
  if [[ -n "$VERIFY_DIR" && "$VERIFY_DIR" == /tmp/* && -d "$VERIFY_DIR" && ! -L "$VERIFY_DIR" ]]; then
    rm -rf -- "$VERIFY_DIR"
  fi
  if [[ -n "$STAGE_DIR" && "$STAGE_DIR" == "$STATE_DIR/agent-bootstrap/.publish.$VERSION."* &&
        -d "$STAGE_DIR" && ! -L "$STAGE_DIR" ]]; then
    rm -rf -- "$STAGE_DIR"
  fi
}
trap cleanup EXIT

validate_source_bundle() {
  local arch="$1" filename="$2" bundle extract
  local -a listing
  bundle="$SOURCE_DIR/$filename"
  extract="$VERIFY_DIR/$arch"
  [[ -f "$bundle" && ! -L "$bundle" && "$(stat -c '%h' -- "$bundle")" == 1 ]] || {
    echo "Missing or linked $filename." >&2
    return 1
  }
  size="$(stat -c '%s' -- "$bundle")"
  (( size > 0 && size <= 64 * 1024 * 1024 )) || { echo "Invalid size for $filename." >&2; return 1; }
  gzip -t -- "$bundle"
  mapfile -t listing < <(tar -tvzf "$bundle")
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
  mkdir -m 0700 -- "$extract"
  tar --no-same-owner --no-same-permissions -xzf "$bundle" -C "$extract"
  [[ -f "$extract/VERSION" && ! -L "$extract/VERSION" && "$(<"$extract/VERSION")" == "$VERSION" &&
     -f "$extract/clawctl-agent" && ! -L "$extract/clawctl-agent" &&
     -f "$extract/clawctl-agent.service" && ! -L "$extract/clawctl-agent.service" &&
     -f "$extract/clawctl-hermes.service" && ! -L "$extract/clawctl-hermes.service" &&
     -f "$extract/install-agent.sh" && ! -L "$extract/install-agent.sh" &&
     -f "$extract/openclaw-gateway.service" && ! -L "$extract/openclaw-gateway.service" ]] || {
    echo "Invalid extracted files in $filename." >&2
    return 1
  }
  elf_prefix="$(od -An -t x1 -N20 -- "$extract/clawctl-agent" | tr -d ' \n')"
  [[ "${elf_prefix:0:12}" == "7f454c460201" ]] || { echo "Invalid Agent ELF in $filename." >&2; return 1; }
  machine="${elf_prefix:36:4}"
  if [[ ( "$arch" == amd64 && "$machine" != 3e00 ) ||
        ( "$arch" == arm64 && "$machine" != b700 ) ]]; then
    echo "Agent architecture mismatch in $filename." >&2
    return 1
  fi
  SOURCE_FILE["$arch"]="$bundle"
  SOURCE_SHA["$arch"]="$(sha256sum "$bundle" | awk '{print $1}')"
}

validate_source_bundle amd64 clawctl-agent-bootstrap-linux-amd64.tar.gz
validate_source_bundle arm64 clawctl-agent-bootstrap-linux-arm64.tar.gz

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
  for arch in amd64 arm64; do
    filename="clawctl-agent-bootstrap-linux-$arch.tar.gz"
    target="$TARGET_DIR/$filename"
    [[ -f "$target" && ! -L "$target" && "$(stat -c '%h' -- "$target")" == 1 ]] || {
      echo "Existing Agent bootstrap release is incomplete." >&2
      exit 1
    }
    [[ "$(sha256sum "$target" | awk '{print $1}')" == "${SOURCE_SHA[$arch]}" ]] || {
      echo "Agent bootstrap version already contains different bytes." >&2
      exit 1
    }
  done
  expected_sums="${SOURCE_SHA[amd64]}  clawctl-agent-bootstrap-linux-amd64.tar.gz
${SOURCE_SHA[arm64]}  clawctl-agent-bootstrap-linux-arm64.tar.gz"
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
for arch in amd64 arm64; do
  filename="clawctl-agent-bootstrap-linux-$arch.tar.gz"
  install -m 0644 -- "${SOURCE_FILE[$arch]}" "$STAGE_DIR/$filename"
  copied_sha="$(sha256sum "$STAGE_DIR/$filename" | awk '{print $1}')"
  [[ "$copied_sha" == "${SOURCE_SHA[$arch]}" ]] || { echo "Agent bootstrap copy verification failed." >&2; exit 1; }
  /usr/bin/sync -f -- "$STAGE_DIR/$filename"
done
{
  printf '%s  %s\n' "${SOURCE_SHA[amd64]}" clawctl-agent-bootstrap-linux-amd64.tar.gz
  printf '%s  %s\n' "${SOURCE_SHA[arm64]}" clawctl-agent-bootstrap-linux-arm64.tar.gz
} >"$STAGE_DIR/SHA256SUMS"
chmod 0644 "$STAGE_DIR/SHA256SUMS"
/usr/bin/sync -f -- "$STAGE_DIR/SHA256SUMS"
/usr/bin/sync -f -- "$STAGE_DIR"
mv -T -- "$STAGE_DIR" "$TARGET_DIR"
STAGE_DIR=""
/usr/bin/sync -f -- "$RELEASE_ROOT"
echo "Agent bootstrap $VERSION published."
