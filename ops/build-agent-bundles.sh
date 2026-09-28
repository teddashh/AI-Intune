#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
BUILD="$ROOT/build"
VERSION="${1:-}"
TARGET_OS="${2:-linux}"
[[ "$VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$ ]] || { echo "Invalid bundle version" >&2; exit 2; }
[[ "$TARGET_OS" == linux || "$TARGET_OS" == darwin ]] || { echo "Unsupported bundle OS" >&2; exit 2; }

WORK="$(mktemp -d "$BUILD/.agent-bundles.XXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT

for arch in amd64 arm64; do
  binary="$BUILD/clawctl-agent-$TARGET_OS-$arch"
  [[ -f "$binary" && ! -L "$binary" ]] || { echo "Missing $binary" >&2; exit 1; }
  stage="$WORK/$TARGET_OS-$arch"
  mkdir "$stage"
  install -m 0755 "$binary" "$stage/clawctl-agent"
  if [[ "$TARGET_OS" == linux ]]; then
    install -m 0755 "$ROOT/ops/install-agent.sh" "$stage/install-agent.sh"
    install -m 0644 "$ROOT/ops/clawctl-agent.service" "$stage/clawctl-agent.service"
    install -m 0644 "$ROOT/ops/clawctl-hermes.service" "$stage/clawctl-hermes.service"
    install -m 0644 "$ROOT/ops/openclaw-gateway.service" "$stage/openclaw-gateway.service"
    files=(VERSION clawctl-agent clawctl-agent.service clawctl-hermes.service install-agent.sh openclaw-gateway.service)
  else
    install -m 0755 "$ROOT/ops/install-agent-macos.sh" "$stage/install-agent-macos.sh"
    install -m 0644 "$ROOT/ops/clawctl-agent.plist" "$stage/clawctl-agent.plist"
    files=(VERSION clawctl-agent clawctl-agent.plist install-agent-macos.sh)
  fi
  printf '%s\n' "$VERSION" >"$stage/VERSION"
  chmod 0644 "$stage/VERSION"
  output="$BUILD/clawctl-agent-bootstrap-$TARGET_OS-$arch.tar.gz"
  candidate="$WORK/clawctl-agent-bootstrap-$TARGET_OS-$arch.tar.gz"
  tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner \
    -cf - -C "$stage" "${files[@]}" | gzip -n >"$candidate"
  chmod 0644 "$candidate"
  mv -f "$candidate" "$output"
  printf '%s  %s\n' "$(sha256sum "$output" | awk '{print $1}')" "$(basename "$output")"
done
