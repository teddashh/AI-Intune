#!/usr/bin/env bash
# Build clawctl-hub for OSS Docker packaging.
# Prefers Docker image build; falls back to make hub / go build when Docker is unavailable.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
cd "$ROOT"

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
echo "==> clawctl OSS smoke-build (version=$VERSION)"
echo "    root=$ROOT"

export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
export CGO_ENABLED=0

have_docker=0
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  have_docker=1
fi

if [[ "$have_docker" -eq 1 ]]; then
  echo "==> Docker available — building image clawctl-hub:local"
  docker build \
    -f ops/docker/Dockerfile \
    --build-arg "VERSION=$VERSION" \
    -t "clawctl-hub:local" \
    "$ROOT"
  echo
  echo "SUCCESS (docker)"
  echo "  image: clawctl-hub:local"
  echo "  next:  copy ops/docker/hub.env.example → hub.env,"
  echo "         set CLAWCTL_LISTEN=\$(tailscale ip -4):8787 and capability prefix,"
  echo "         then: docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env up -d"
  echo "  note:  Hub refuses non-Tailscale listen; compose uses network_mode: host."
  exit 0
fi

echo "==> Docker not available — building local binary (CGO_ENABLED=0)"
mkdir -p build
LDFLAGS="-s -w -X main.version=${VERSION}"

if command -v make >/dev/null 2>&1; then
  echo "    using: make hub"
  make hub
else
  GO_BIN="$(command -v go 2>/dev/null || true)"
  if [[ -z "$GO_BIN" && -x "${HOME}/.local/go/bin/go" ]]; then
    GO_BIN="${HOME}/.local/go/bin/go"
  fi
  if [[ -z "$GO_BIN" ]]; then
    echo "FAIL: neither docker, make, nor go is available" >&2
    exit 1
  fi
  echo "    using: $GO_BIN build (make not installed)"
  "$GO_BIN" build -buildvcs=false -trimpath -ldflags "$LDFLAGS" \
    -o build/clawctl-hub ./cmd/clawctl-hub
fi

BIN="$ROOT/build/clawctl-hub"
if [[ ! -x "$BIN" ]]; then
  echo "FAIL: expected executable $BIN" >&2
  exit 1
fi
VER_OUT="$("$BIN" version 2>/dev/null || true)"
echo
echo "SUCCESS (local binary)"
echo "  binary: $BIN"
echo "  version: ${VER_OUT:-<unreported>}"
echo "  criteria:"
echo "    - CGO_ENABLED=0 static build produced build/clawctl-hub"
echo "    - Deploy with ops/install-hub.sh --listen <tailscale-ip>:8787 \\"
echo "        --operator-capability-prefix <domain>/cap/clawctl"
echo "    - Or install Docker and re-run this script for the image path"
echo "  reminder: Grok Bot box must NOT host the Hub; use any other Linux VPS."
