# clawctl-hub — portable static image (CGO_ENABLED=0).
#
# Hub is a long-lived Go + SQLite process. Do NOT rewrite it as Vercel/Workers.
# At runtime the process MUST:
#   1. Bind a canonical literal Tailscale node IP:port (see CLAWCTL_LISTEN)
#   2. Reach the host tailscaled LocalAPI socket for operator WhoIs
#
# Therefore compose uses network_mode: host + a socket bind-mount.
# Build from the repository root:
#   docker build -f ops/docker/Dockerfile \
#     --build-arg CLAWCTL_VERSION=dev -t clawctl-hub:local .
#
# CLAWCTL_VERSION is baked into the Hub binary (main.version) and into the
# Linux agent bootstrap release. Hub loads that release from
# <dir of --db>/agent-bootstrap/<version> at startup (both amd64 and arm64
# are required). The default target below stores the release in the image at
# /usr/local/share/clawctl/agent-bootstrap/<version>/. The hub-init target
# copies the same tree to /opt/clawctl-seed/... and the compose one-shot
# seeds it into the data volume on first use. Hub has no flag for another path.
#
# A named volume's root is not the image directory. Compose runs hub-init
# before hub to chown /var/lib/clawctl to uid 65532 and mode 0700.
# The hub-fly target is the Fly.io image (kernel tailscaled + Litestream).
# It is not the default build target. See ops/fly/README.md.

# ---- build ----
FROM golang:1.27.1-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG CLAWCTL_VERSION=dev
ENV CGO_ENABLED=0
# Bundles are packed by ops/build-agent-bundles.sh (same archive layout the
# Hub checks). SHA256SUMS matches ops/publish-agent-bundles.sh for the
# required Linux pair: "<sha256><two spaces><filename>" amd64 then arm64.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    <<'EOF'
set -eu
version="${CLAWCTL_VERSION}"
case "$version" in
  "" )
    echo "CLAWCTL_VERSION is empty" >&2
    exit 1
    ;;
esac
case "$version" in
  *[!A-Za-z0-9._+-]* )
    echo "CLAWCTL_VERSION contains unsupported characters: $version" >&2
    exit 1
    ;;
esac
case "$version" in
  [A-Za-z0-9]* ) ;;
  * )
    echo "CLAWCTL_VERSION must start with a letter or digit: $version" >&2
    exit 1
    ;;
esac
if [ "${#version}" -gt 128 ]; then
  echo "CLAWCTL_VERSION is too long" >&2
  exit 1
fi

mkdir -p /out /src/build
ldflags="-s -w -X main.version=${version}"

go build -buildvcs=false -trimpath -ldflags "$ldflags" \
  -o /out/clawctl-hub ./cmd/clawctl-hub
go build -buildvcs=false -trimpath -ldflags "$ldflags" \
  -o /out/clawctl-agent ./cmd/clawctl-agent

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
  -ldflags "$ldflags" \
  -o /src/build/clawctl-agent-linux-amd64 ./cmd/clawctl-agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=false -trimpath \
  -ldflags "$ldflags" \
  -o /src/build/clawctl-agent-linux-arm64 ./cmd/clawctl-agent

/src/ops/build-agent-bundles.sh "$version" linux

amd64=clawctl-agent-bootstrap-linux-amd64.tar.gz
arm64=clawctl-agent-bootstrap-linux-arm64.tar.gz
dest="/out/agent-bootstrap/${version}"
mkdir -p "$dest"
install -m 0644 "/src/build/$amd64" "$dest/$amd64"
install -m 0644 "/src/build/$arm64" "$dest/$arm64"
: > "$dest/SHA256SUMS"
for name in "$amd64" "$arm64"; do
  digest=$(sha256sum "$dest/$name") || exit 1
  printf '%s  %s\n' "${digest%% *}" "$name" >> "$dest/SHA256SUMS"
done
chmod 0644 "$dest/SHA256SUMS"
chmod 0700 /out/agent-bootstrap "$dest"
EOF

# ---- volume init (one-shot; not the default build target) ----
# busybox supplies sh, chown, and chmod. The Hub image is distroless and
# cannot fix a root-owned volume itself.
FROM busybox:1.36.1 AS hub-init
ARG CLAWCTL_VERSION=dev
ENV CLAWCTL_VERSION=${CLAWCTL_VERSION}
COPY --from=build /out/agent-bootstrap /opt/clawctl-seed/agent-bootstrap
COPY ops/docker/hub-data-init.sh /usr/local/bin/hub-data-init.sh
USER 0:0
ENTRYPOINT ["/bin/sh", "/usr/local/bin/hub-data-init.sh"]

# ---- runtime (default target) ----
# Distroless: static binary, non-root, no shell. --listen defaults from
# CLAWCTL_LISTEN inside the binary (serveListenDefault); no shell expansion needed.
FROM gcr.io/distroless/static-debian12:nonroot AS hub

LABEL org.opencontainers.image.title="clawctl-hub" \
      org.opencontainers.image.description="clawctl fleet Hub (Go + SQLite); Tailscale operator auth" \
      org.opencontainers.image.source="https://github.com/teddashh/AI-Intune"

USER nonroot:nonroot

VOLUME ["/var/lib/clawctl"]

ENV CLAWCTL_DB=/var/lib/clawctl/clawctl.sqlite \
    CLAWCTL_REPORT_STAMP=/var/lib/clawctl/last-report.stamp

WORKDIR /var/lib/clawctl

COPY --from=build --chown=nonroot:nonroot /out/clawctl-hub /usr/local/bin/clawctl-hub
COPY --from=build --chown=nonroot:nonroot /out/clawctl-agent /usr/local/bin/clawctl-agent
# Same bytes the init image seeds onto the volume. Hub does not read this
# path; smoke-build.sh checks that the files are present in the image.
COPY --from=build --chown=nonroot:nonroot /out/agent-bootstrap /usr/local/share/clawctl/agent-bootstrap

# Conventional example port only. Hub has no production default port.
# The process binds CLAWCTL_LISTEN. The flag default 127.0.0.1:8770 is refused.
EXPOSE 8787

ENTRYPOINT ["/usr/local/bin/clawctl-hub"]
CMD ["--db", "/var/lib/clawctl/clawctl.sqlite", "--report-stamp", "/var/lib/clawctl/last-report.stamp"]

# ---- Fly.io (kernel tailscaled + Litestream). Not the default target. ----
# Pins are explicit image tags. Do not float to :latest.
# tailscale/tailscale:v1.102.5 publishes tailscaled and tailscale at
# /usr/local/bin. litestream/litestream:0.5.17 publishes litestream at
# /usr/local/bin. Both stay in their own stages so this image does not run
# their entrypoints.
FROM tailscale/tailscale:v1.102.5 AS tailscale-bins
FROM litestream/litestream:0.5.17 AS litestream-bins

FROM debian:bookworm-slim AS hub-fly
ARG CLAWCTL_VERSION=dev
ENV CLAWCTL_VERSION=${CLAWCTL_VERSION} \
    CLAWCTL_DB=/var/lib/clawctl/clawctl.sqlite \
    CLAWCTL_REPORT_STAMP=/var/lib/clawctl/last-report.stamp \
    DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      iptables \
      iproute2 \
      util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system --gid 65532 nonroot \
 && useradd --system --uid 65532 --gid 65532 --no-create-home \
      --home-dir /var/lib/clawctl --shell /usr/sbin/nologin nonroot \
 && mkdir -p /var/lib/clawctl /var/run/tailscale /run/clawctl \
 && chmod 0700 /var/lib/clawctl /run/clawctl

COPY --from=tailscale-bins /usr/local/bin/tailscaled /usr/local/bin/tailscaled
COPY --from=tailscale-bins /usr/local/bin/tailscale /usr/local/bin/tailscale
COPY --from=litestream-bins /usr/local/bin/litestream /usr/local/bin/litestream
COPY --from=build /out/clawctl-hub /usr/local/bin/clawctl-hub
COPY --from=build /out/agent-bootstrap /opt/clawctl-seed/agent-bootstrap
COPY --from=build /out/agent-bootstrap /usr/local/share/clawctl/agent-bootstrap
COPY ops/docker/hub-data-init.sh /usr/local/bin/hub-data-init.sh
COPY ops/fly/entrypoint.sh /usr/local/bin/clawctl-fly-entrypoint
COPY ops/fly/litestream.yml /etc/litestream.yml

RUN chmod 0755 /usr/local/bin/clawctl-hub /usr/local/bin/tailscaled \
      /usr/local/bin/tailscale /usr/local/bin/litestream \
      /usr/local/bin/hub-data-init.sh /usr/local/bin/clawctl-fly-entrypoint \
 && chmod 0644 /etc/litestream.yml

VOLUME ["/var/lib/clawctl"]
# No EXPOSE and no public port. Operators and agents reach Hub over Tailscale.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/clawctl-fly-entrypoint"]
