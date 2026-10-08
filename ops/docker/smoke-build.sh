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

bash "$HERE/test-autopilot.sh"

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
echo "==> clawctl OSS smoke-build (version=$VERSION)"
echo "    root=$ROOT"

export GOTOOLCHAIN="${GOTOOLCHAIN:-auto}"
export CGO_ENABLED=0

# Init script is POSIX sh (busybox). The Fly entrypoint is POSIX sh too.
sh -n "$HERE/hub-data-init.sh"
sh -n "$ROOT/ops/fly/entrypoint.sh"
sh -n "$ROOT/ops/fly/notify-env.sh"
sh -n "$ROOT/ops/fly/r2-env.sh"
sh -n "$ROOT/ops/fly/restore-drill.sh"
if command -v bash >/dev/null 2>&1; then
  bash -n "$HERE/hub-data-init.sh"
  bash -n "$ROOT/ops/fly/entrypoint.sh"
  bash -n "$ROOT/ops/fly/notify-env.sh"
  bash -n "$ROOT/ops/fly/r2-env.sh"
  bash -n "$ROOT/ops/fly/restore-drill.sh"
fi

echo "==> parsing ops/fly/fly.toml and ops/fly/litestream.yml"
# tomllib is Python 3.11+. Older hosts (Ubuntu 22.04 ships 3.10) skip the
# parse instead of failing the whole smoke build.
if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import tomllib' 2>/dev/null; then
  echo "    skip: python3 with tomllib (3.11+) not available; fly.toml not parsed"
else
python3 - <<'PY'
import tomllib
from pathlib import Path
root = Path("ops/fly")
doc = tomllib.loads((root / "fly.toml").read_text())
if doc["env"].get("CLAWCTL_AUTH_MODE") != "local" or "http_service" not in doc:
    raise SystemExit("fly.toml must publish Autopilot http_service in local mode")
advanced = tomllib.loads((root / "fly.tailscale.toml").read_text())
if advanced["env"].get("CLAWCTL_AUTH_MODE") != "tailscale":
    raise SystemExit("fly.tailscale.toml must explicitly select tailscale mode")
if "http_service" in advanced or "services" in advanced:
    raise SystemExit("fly.tailscale.toml must not publish http_service or services")
for key in ("mounts", "restart", "vm", "build"):
    if advanced[key] != doc[key]:
        raise SystemExit(f"Fly packs disagree on {key}")
mounts = doc["mounts"]
if mounts[0]["source"] != "clawctl_data" or mounts[0]["destination"] != "/var/lib/clawctl":
    raise SystemExit(f"unexpected mounts: {mounts}")
build = doc["build"]
if "dockerfile" in build:
    raise SystemExit(f"fly.toml [build] must not contain dockerfile key: {build}")
if build.get("build-target") != "hub-fly":
    raise SystemExit(f"unexpected build: {build}")
dockerfile = (root / ".." / "docker" / "Dockerfile").resolve()
if not dockerfile.is_file():
    raise SystemExit(f"dockerfile missing: {dockerfile}")
if doc["env"]["CLAWCTL_PORT"] != "8787":
    raise SystemExit("CLAWCTL_PORT example missing")
restart = doc["restart"]
if not any(item.get("policy") == "always" for item in restart):
    raise SystemExit(f"restart policy: {restart}")
vm = doc.get("vm", {})
if isinstance(vm, list):
    vm = vm[0] if vm else {}
elif not isinstance(vm, dict):
    vm = {}
mem_str = str(vm.get("memory", "")).strip().lower()
if mem_str.endswith("gb"):
    mem_mb = int(mem_str[:-2]) * 1024
elif mem_str.endswith("mb"):
    mem_mb = int(mem_str[:-2])
elif mem_str.endswith("g"):
    mem_mb = int(mem_str[:-1]) * 1024
elif mem_str.endswith("m"):
    mem_mb = int(mem_str[:-1])
elif mem_str.isdigit():
    mem_mb = int(mem_str)
else:
    raise SystemExit(f"invalid or missing vm memory: {vm.get('memory')}")
if mem_mb < 1024:
    raise SystemExit(f"vm memory must be >= 1024mb, got {vm.get('memory')}")
text = (root / "litestream.yml").read_text()
for needle in (
    "${LITESTREAM_BUCKET}",
    "${LITESTREAM_PATH}",
    "${LITESTREAM_ENDPOINT}",
    "${R2_ACCESS_KEY_ID}",
    "${R2_SECRET_ACCESS_KEY}",
    "type: s3",
    "/var/lib/clawctl/clawctl.sqlite",
    "replica:",
    "sync-interval",
    "snapshot:",
    "retention",
):
    if needle not in text:
        raise SystemExit(f"litestream.yml missing {needle}")
if "replicas:" in text:
    raise SystemExit("litestream.yml must use replica:, not replicas:")
if "latest" in text:
    raise SystemExit("litestream.yml must pin replicas without the word latest")
print("    ok fly.toml and litestream.yml")
PY
fi

if [[ "${CLAWCTL_SMOKE_STATIC_ONLY:-0}" == 1 ]]; then
  echo "SUCCESS (static checks; Docker and build skipped)"
  exit 0
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

  echo "==> building fly image clawctl-hub-fly:local"
  docker build \
    -f ops/docker/Dockerfile \
    --target hub-fly \
    --build-arg "CLAWCTL_VERSION=$VERSION" \
    -t "clawctl-hub-fly:local" \
    "$ROOT"
  echo "==> checking fly image binaries and bundles"
  docker run --rm --entrypoint /usr/local/bin/tailscaled clawctl-hub-fly:local --version
  docker run --rm --entrypoint /usr/local/bin/tailscale clawctl-hub-fly:local version
  docker run --rm --entrypoint /usr/local/bin/litestream clawctl-hub-fly:local version
  docker run --rm --entrypoint /usr/local/bin/clawctl-hub clawctl-hub-fly:local version
  copy_bundle_tree "clawctl-hub-fly:local" "/usr/local/share/clawctl/agent-bootstrap/${VERSION}"
  copy_bundle_tree "clawctl-hub-fly:local" "/opt/clawctl-seed/agent-bootstrap/${VERSION}"

  echo "==> fly entrypoint rejects a missing TS_AUTHKEY"
  fly_err="$(mktemp)"
  if docker run --rm -e CLAWCTL_AUTH_MODE=tailscale --entrypoint /bin/sh clawctl-hub-fly:local /usr/local/bin/clawctl-fly-entrypoint >"$fly_err" 2>&1; then
    echo "FAIL: entrypoint succeeded without TS_AUTHKEY" >&2
    cat "$fly_err" >&2
    rm -f "$fly_err"
    exit 1
  fi
  if ! grep -q 'TS_AUTHKEY' "$fly_err"; then
    echo "FAIL: entrypoint did not name TS_AUTHKEY" >&2
    cat "$fly_err" >&2
    rm -f "$fly_err"
    exit 1
  fi
  rm -f "$fly_err"
  echo "    ok entrypoint without TS_AUTHKEY"

  echo
  echo "SUCCESS (docker)"
  echo "  image: clawctl-hub:local"
  echo "  init:  clawctl-hub-init:local"
  echo "  fly:   clawctl-hub-fly:local"
  echo "  next:  copy ops/docker/hub.env.example → hub.env,"
  echo "         set CLAWCTL_LISTEN=\$(tailscale ip -4):<port> and capability prefix."
  echo "         8787 is the conventional example port, not a Hub default."
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
echo "    - Deploy with Docker (primary) or ops/install-hub.sh --listen <tailscale-ip>:<port> \\"
echo "        --operator-capability-prefix <domain>/cap/clawctl"
echo "      8787 is the conventional example port, not a Hub default."
echo "    - Or install Docker and re-run this script for the image path"
echo "      (image checks: compose config with no tunnel token; bundle files; hub-fly binaries)"
echo "    - fly.toml and litestream.yml were parsed. Docker image checks were skipped."
echo "  reminder: do not host the Hub on a build/CI machine; use a separate Linux host."
