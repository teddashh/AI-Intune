#!/usr/bin/env bash
# Build clawctl-hub for OSS Docker packaging.
# Prefers Docker image build; falls back to make hub / go build when Docker is unavailable.
#
# With Docker, also checks the two deploy failures this pack has hit:
#   - `docker compose config` on docker-compose.yml succeeds with no tunnel token
#   - the Linux amd64 and arm64 agent bootstrap files exist in the image
#     (and in the hub-init seed image) under agent-bootstrap/$CLAWCTL_VERSION
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
cd "$ROOT"

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
echo "==> clawctl OSS smoke-build (version=$VERSION)"
echo "    root=$ROOT"

export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
export CGO_ENABLED=0

# Init script is POSIX sh (busybox). Parse it with both shells when present.
sh -n "$HERE/hub-data-init.sh"
if command -v bash >/dev/null 2>&1; then
  bash -n "$HERE/hub-data-init.sh"
fi

have_docker=0
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  have_docker=1
fi

copy_bundle_tree() {
  local image="$1" root="$2" cid tmp name rc=0
  cid="$(docker create "$image")"
  tmp="$(mktemp -d)"
  for name in \
    clawctl-agent-bootstrap-linux-amd64.tar.gz \
    clawctl-agent-bootstrap-linux-arm64.tar.gz \
    SHA256SUMS
  do
    if ! docker cp "$cid:$root/$name" "$tmp/$name"; then
      echo "FAIL: $image is missing $root/$name" >&2
      rc=1
      break
    fi
    if [[ ! -s "$tmp/$name" ]]; then
      echo "FAIL: $image has an empty $root/$name" >&2
      rc=1
      break
    fi
  done
  if [[ "$rc" -eq 0 ]]; then
    if [[ "$(grep -c . "$tmp/SHA256SUMS" || true)" -ne 2 ]] ||
       ! grep -q '  clawctl-agent-bootstrap-linux-amd64.tar.gz$' "$tmp/SHA256SUMS" ||
       ! grep -q '  clawctl-agent-bootstrap-linux-arm64.tar.gz$' "$tmp/SHA256SUMS"; then
      echo "FAIL: $image SHA256SUMS is not the publish-agent-bundles.sh Linux layout" >&2
      rc=1
    fi
  fi
  docker rm "$cid" >/dev/null 2>&1 || true
  rm -rf "$tmp"
  if [[ "$rc" -ne 0 ]]; then
    exit 1
  fi
  echo "    ok $image:$root"
}

if [[ "$have_docker" -eq 1 ]]; then
  if ! docker compose version >/dev/null 2>&1; then
    echo "FAIL: docker is available but the compose plugin is not" >&2
    exit 1
  fi

  echo "==> Docker available — building image clawctl-hub:local"
  docker build \
    -f ops/docker/Dockerfile \
    --target hub \
    --build-arg "CLAWCTL_VERSION=$VERSION" \
    -t "clawctl-hub:local" \
    "$ROOT"
  echo "==> building one-shot image clawctl-hub-init:local"
  docker build \
    -f ops/docker/Dockerfile \
    --target hub-init \
    --build-arg "CLAWCTL_VERSION=$VERSION" \
    -t "clawctl-hub-init:local" \
    "$ROOT"

  echo "==> checking agent bootstrap files in the images"
  copy_bundle_tree "clawctl-hub:local" "/usr/local/share/clawctl/agent-bootstrap/${VERSION}"
  copy_bundle_tree "clawctl-hub-init:local" "/opt/clawctl-seed/agent-bootstrap/${VERSION}"

  echo "==> compose config with no tunnel token"
  cfg="$(mktemp)"
  env -u CLOUDFLARE_TUNNEL_TOKEN \
    CLAWCTL_LISTEN=100.64.0.1:8787 \
    CLAWCTL_OPERATOR_CAPABILITY_PREFIX=example.com/cap/clawctl \
    CLAWCTL_VERSION="$VERSION" \
    docker compose -f ops/docker/docker-compose.yml config >"$cfg"
  grep -q 'hub-data-init:' "$cfg"
  grep -q 'service_completed_successfully' "$cfg"
  if grep -q 'CLOUDFLARE_TUNNEL_TOKEN' "$cfg"; then
    echo "FAIL: default compose config referenced CLOUDFLARE_TUNNEL_TOKEN" >&2
    rm -f "$cfg"
    exit 1
  fi
  if grep -q 'clawctl-cloudflared' "$cfg"; then
    echo "FAIL: default compose config included cloudflared" >&2
    rm -f "$cfg"
    exit 1
  fi
  rm -f "$cfg"
  echo "    ok default compose config"

  echo "==> tunnel override fails clearly without a token"
  tunnel_err="$(mktemp)"
  if env -u CLOUDFLARE_TUNNEL_TOKEN \
      CLAWCTL_LISTEN=100.64.0.1:8787 \
      CLAWCTL_OPERATOR_CAPABILITY_PREFIX=example.com/cap/clawctl \
      docker compose \
        -f ops/docker/docker-compose.yml \
        -f ops/docker/docker-compose.tunnel.yml \
        config >/dev/null 2>"$tunnel_err"; then
    echo "FAIL: tunnel compose config succeeded without CLOUDFLARE_TUNNEL_TOKEN" >&2
    cat "$tunnel_err" >&2
    rm -f "$tunnel_err"
    exit 1
  fi
  if ! grep -q 'CLOUDFLARE_TUNNEL_TOKEN' "$tunnel_err"; then
    echo "FAIL: tunnel compose failed without naming CLOUDFLARE_TUNNEL_TOKEN" >&2
    cat "$tunnel_err" >&2
    rm -f "$tunnel_err"
    exit 1
  fi
  rm -f "$tunnel_err"
  echo "    ok tunnel override rejects a missing token"

  echo "==> tunnel override config accepts a token"
  env CLOUDFLARE_TUNNEL_TOKEN=dummy-token-for-config \
    CLAWCTL_LISTEN=100.64.0.1:8787 \
    CLAWCTL_OPERATOR_CAPABILITY_PREFIX=example.com/cap/clawctl \
    docker compose \
      -f ops/docker/docker-compose.yml \
      -f ops/docker/docker-compose.tunnel.yml \
      config | grep -q 'dummy-token-for-config'
  echo "    ok tunnel override"

  echo
  echo "SUCCESS (docker)"
  echo "  image: clawctl-hub:local"
  echo "  init:  clawctl-hub-init:local"
  echo "  next:  copy ops/docker/hub.env.example → hub.env,"
  echo "         set CLAWCTL_LISTEN=\$(tailscale ip -4):8787 and capability prefix,"
  echo "         then: docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env up -d"
  echo "  note:  Hub refuses non-Tailscale listen; compose uses network_mode: host."
  echo "         hub-data-init chowns the volume to uid 65532 mode 0700 and seeds bundles."
  echo "         Tunnel: add -f ops/docker/docker-compose.tunnel.yml and set CLOUDFLARE_TUNNEL_TOKEN."
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
echo "      (image checks: compose config with no tunnel token; bundle files in the image)"
echo "  reminder: Grok Bot box must NOT host the Hub; use any other Linux VPS."
