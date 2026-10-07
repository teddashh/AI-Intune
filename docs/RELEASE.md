# Releases: signed Hub images on GHCR

A version tag publishes two multi-arch images (linux/amd64, linux/arm64):

| Image | Dockerfile target | Purpose |
|---|---|---|
| `ghcr.io/teddashh/clawctl-hub` | `hub` | the Hub (distroless, nonroot) |
| `ghcr.io/teddashh/clawctl-hub-init` | `hub-init` | one-shot volume init + agent bootstrap seed |

Workflow: [`.github/workflows/release.yml`](../.github/workflows/release.yml).

## What a release carries

- Tags `vX.Y.Z` (the exact `CLAWCTL_VERSION` baked into the binaries), `X.Y.Z`, `X.Y`, and `latest` for stable versions.
- BuildKit SBOM and SLSA provenance (`mode=max`) attestations for each platform, stored in the image index.
- An SPDX SBOM per platform (syft), attached with `cosign attest --type spdxjson`.
- Cosign **keyless** signatures on the index and on each platform manifest. The workflow's GitHub OIDC token gets a short-lived Fulcio certificate, and the signature is recorded in the public Rekor transparency log. There is no long-lived signing key to store or leak.
- The SBOM files as a workflow artifact (90 days).

## Cutting a release

```bash
git tag -s v1.2.3 -m "v1.2.3"   # vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-pre
git push origin v1.2.3
```

The workflow refuses tags that are not `vMAJOR.MINOR.PATCH[-pre]`, because the tag becomes `CLAWCTL_VERSION` and the agent bootstrap path. It needs `packages: write` and `id-token: write`. Both are granted to that job only.

## Dry run

Every pull request that touches the workflow, `ops/docker/**`, `go.mod` or `go.sum` runs the same job, and so does a manual run (`workflow_dispatch`). It:

- builds both platforms and pushes them to a throwaway registry inside the job,
- checks that both platform manifests and the BuildKit attestations exist,
- generates and attests the SPDX SBOMs,
- signs with an **ephemeral key** (no Fulcio certificate, no Rekor entry),
- verifies the signatures and attestations.

Nothing reaches GHCR. The only difference from a release is where the signing identity comes from.

## Verifying an image

```bash
V=v1.2.3
for img in clawctl-hub clawctl-hub-init; do
  cosign verify "ghcr.io/teddashh/$img:$V" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp '^https://github\.com/teddashh/AI-Intune/\.github/workflows/release\.yml@refs/tags/v'
  cosign verify-attestation --type spdxjson "ghcr.io/teddashh/$img:$V" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity-regexp '^https://github\.com/teddashh/AI-Intune/\.github/workflows/release\.yml@refs/tags/v' \
    --platform linux/amd64 >/dev/null
done
docker buildx imagetools inspect "ghcr.io/teddashh/clawctl-hub:$V" --format '{{ json .SBOM }}' | head -c 400
```

## Running the release images

Use [`ops/docker/docker-compose.ghcr.yml`](../ops/docker/docker-compose.ghcr.yml) on top of the normal compose file. It replaces the local builds with the GHCR images:

```bash
# hub.env: CLAWCTL_VERSION=v1.2.3 (both images must be the same release)
docker compose -f ops/docker/docker-compose.yml -f ops/docker/docker-compose.ghcr.yml \
  --env-file ops/docker/hub.env pull
docker compose -f ops/docker/docker-compose.yml -f ops/docker/docker-compose.ghcr.yml \
  --env-file ops/docker/hub.env up -d
```

To run exactly the bytes you verified, pin by digest (`image@sha256:…`) in a local override.

The Fly image (`hub-fly` target) and the native agent bundles are not part of this workflow.
