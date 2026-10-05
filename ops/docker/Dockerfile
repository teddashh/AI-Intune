# clawctl-hub — portable static image (CGO_ENABLED=0).
#
# Hub is a long-lived Go + SQLite process. Do NOT rewrite it as Vercel/Workers.
# At runtime the process MUST:
#   1. Bind a canonical literal Tailscale node IP:port (see CLAWCTL_LISTEN)
#   2. Reach the host tailscaled LocalAPI socket for operator WhoIs
#
# Therefore compose uses network_mode: host + a socket bind-mount.
# Build from the repository root:
#   docker build -f ops/docker/Dockerfile -t clawctl-hub:local .

# ---- build ----
FROM golang:1.27.1-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=dev
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out && \
    go build -buildvcs=false -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/clawctl-hub ./cmd/clawctl-hub && \
    go build -buildvcs=false -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/clawctl-agent ./cmd/clawctl-agent

# ---- runtime ----
# Distroless: static binary, non-root, no shell. --listen defaults from
# CLAWCTL_LISTEN inside the binary (serveListenDefault); no shell expansion needed.
FROM gcr.io/distroless/static-debian12:nonroot

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

# Documented port only — with network_mode: host the process binds CLAWCTL_LISTEN.
EXPOSE 8787

ENTRYPOINT ["/usr/local/bin/clawctl-hub"]
CMD ["--db", "/var/lib/clawctl/clawctl.sqlite", "--report-stamp", "/var/lib/clawctl/last-report.stamp"]
