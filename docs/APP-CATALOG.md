# App Catalog and machine profiles

AI-Intune separates the endpoint management base from software selected for a
machine. The operator performs one enrollment action; the Hub expands the bound
machine profile into exact runtime and app packages.

```text
bootstrap -> enroll -> resolve profile -> runtimes -> app -> verify -> managed
```

## Bootstrap boundary

The bootstrap owns the components required before an endpoint can pull work:

- Tailscale installation and tailnet join
- the static `clawctl-agent` binary
- enrollment and the machine credential
- the systemd system unit with an explicit unprivileged `User=` and the
  lingered user manager used by managed runtime services
- the enabled job loop
- rootless Podman and subordinate UID/GID ranges
- fixed, initially disabled OpenClaw and Hermes user units
- Hub artifact trust and local release/state directories

Node, Python, uv, npm, Git and application frameworks are catalog runtimes.
They are installed in the same onboarding operation only when the selected
profile requires them.

Enrollment writes `jobs_enabled=true`; the first agent process can claim work
without a second configuration step. Every heartbeat reports the configured
value, and the Hub keeps it with the check-in ledger. Diagnostic job preflight
binds the latest value into its preview digest and creates no desired state or
job when execution is disabled or has not been reported.

`make agent-bundles` produces self-contained Linux amd64 and arm64 bootstrap
archives with an exact Hub/Agent release marker. Hub install and upgrade publish
both architectures atomically under the running Hub version. Enrollment offers
the two authenticated downloads with their sizes and SHA-256 digests. On the
target, one `install-agent.sh --hub URL` run installs and joins
Tailscale when needed, installs the matching static Agent, enables linger for
the managed runtime services, redeems enrollment, starts the Agent system
service, and waits for a fresh Hub receipt
that proves the exact Agent version and jobs channel are active. Non-interactive
provisioning supplies the enrollment and Tailscale keys through 0600 files.

## Operator workflow

The complete managed-software path is one ordered workflow:

1. Fetch an exact official artifact into the Hub catalog.
2. Publish the standard package. OpenClaw publication binds an exact published
   Node runtime; Hermes publication binds the official multi-platform OCI bundle.
3. Publish a machine profile that directly selects OpenClaw or Hermes. The
   resolver returns the complete dependency order.
4. Preview and assign that profile to an exact enrolled machine.
5. Assign the other primary-runtime profile whenever the machine should switch;
   the new assignment supersedes the prior append-only assignment.

Machine assignment converts the probe OS display and architecture into the
manifest target. Architecture `x86_64`/`amd64` maps to `amd64`; `aarch64`/`arm64`
maps to `arm64`. OS display `macOS…` maps to `darwin`; canonical Windows
displays (`windows`, `Windows …`, `Microsoft Windows…`) map to `windows`;
other nonempty OS display values retain the existing `linux` fallback. Store repeats this
conversion inside apply before accepting the prepared graph. A Linux prepared
graph cannot apply to a Windows identity.

Assignment preview and apply distinguish four rejection causes. Recognized
macOS identities remain `darwin`; recognized Windows identities remain
`windows`. A profile without packages for that target is unresolvable after
lookup. Unsupported architectures are rejected before profile lookup because no
executor exists for that target. Incomplete or malformed OS/architecture
evidence is a separate state.

| Code | HTTP | Meaning |
| --- | --- | --- |
| `MACHINE_PLATFORM_UNKNOWN` | 409 | The reported OS or architecture cannot identify the machine platform; check the agent report. |
| `MACHINE_PLATFORM_UNSUPPORTED` | 409 | The catalog has no packages for this machine platform; select another machine. |
| `MACHINE_PROFILE_NOT_FOUND` | 404 | The requested exact profile revision does not exist; select a published revision. |
| `MACHINE_PROFILE_UNRESOLVABLE` | 400 | The published profile cannot resolve for the machine target. |

Rejected applies persist their specific code with the audit and idempotency
receipt, without creating assignments or jobs. Replays report the original
rejection code as a historical decision. Existing generic rejection receipts
keep their original meaning. These distinctions do not add Windows packages or
change which adapters can execute a profile.

Each preview reopens and verifies the complete artifact bytes before returning
the canonical manifest, resolved graph, blockers and digest. Apply requires the
typed package, profile or machine confirmation plus an idempotency key. The
manifest/profile publication and assignment writers commit their domain rows,
receipt and audit in one transaction. Assignment additionally commits every
desired state, job and dependency edge together.

The Web Store, Profiles and Assignments views, the eight operator JSON routes,
the strict Go client and `clawctl-hub catalog` all call these same services.
The CLI writes a private canonical recovery receipt before every apply; `catalog
recover` replays that exact request after a transport-ambiguous response.

## Deployable manifest

`internal/catalog` accepts one strict schema. Every field is required; unknown,
missing, case-aliased, duplicate, `null` and trailing JSON are rejected.

```json
{
  "schema_version": 1,
  "id": "openclaw",
  "version": "2026.9.2",
  "kind": "app",
  "title": "OpenClaw",
  "source": {
    "catalog": "ai-intune",
    "upstream_url": "https://github.com/example/openclaw",
    "revision": "2026.9.2",
    "license": "MIT"
  },
  "artifact": {
    "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "size": 1024
  },
  "adapter": {
    "name": "openclaw",
    "version": 1
  },
  "platforms": [
    {"os": "linux", "arch": "amd64"},
    {"os": "linux", "arch": "arm64"}
  ],
  "dependencies": [
    {"package_id": "node-runtime", "version": "24.15.0"}
  ],
  "provides": ["agent-runtime"],
  "conflicts": ["hermes-agent"],
  "exclusive_groups": ["primary-agent-runtime"]
}
```

The manifest contains no shell commands. `adapter.name` and `adapter.version`
select code implemented and advertised by `clawctl-agent`. Artifact bytes are
content-addressed before a manifest becomes deployable.

## Manifest admission

The Hub admits a manifest through one idempotent, audited operation. Before the
ledger write it hashes the complete content-addressed artifact and requires the
sidecar name, exact version, SHA-256 and size to match the manifest. The typed
adapter registry then requires an executable package, adapter version and
platform contract. `openclaw/v1`, `hermes-agent/v1`, and `node-runtime/v1`
admit their exact packages for Linux amd64 and arm64. Metadata-only runtime
entries do not pass this execution gate.

The manifest, global idempotency receipt and audit event commit in one SQLite
transaction. A retry returns the original receipt without hashing the artifact
again. A new key with identical canonical content records an
`already_published` result without adding another manifest. Unsupported
adapters, unavailable bytes, metadata mismatches, immutable identity conflicts
and catalog capacity each have a stable result code.

## Machine profile

A profile selects exact package versions:

```json
{
  "schema_version": 1,
  "id": "openclaw-standard",
  "revision": 7,
  "packages": [
    {"package_id": "openclaw", "version": "2026.9.2"}
  ]
}
```

The resolver produces one stable topological plan regardless of catalog or
profile input order. Shared dependencies appear once. Missing material,
version splits, cycles, unsupported platforms, package conflicts and exclusive
group conflicts have distinct typed result codes.

The Hub stores canonical manifests and profiles in its SQLite ledger. Package
ID plus exact version and profile ID plus revision are immutable identities.
Publishing identical canonical content is an idempotent replay; different
content under an existing identity is rejected. Every read verifies the stored
JSON, identity, digest, publisher and publication time before resolution.

A profile enters the ledger only after its exact package graph resolves for at
least one declared target platform. Assignment resolves it again for the
machine's exact OS and architecture, with dependencies ordered before apps.
Profile publication also re-hashes every artifact in the resolved plan and
re-checks every adapter contract. Its immutable profile row, global
idempotency receipt and audit event commit together. Retrying the same request
returns the original receipt without resolving or hashing the graph again.

Assignment preview re-hashes the complete selected graph and binds the machine
identity, display name, lifecycle revision, exact OS/architecture, latest
`jobs_enabled`, nonterminal job occupancy, current package revisions, manifest
digests, spec digests and artifact digests into one review digest. Apply repeats
the material checks, then commits the assignment attempt, every package desired
state, every job, dependency edges, the idempotency receipt and audit evidence
in one transaction. Assignment and package-link rows are append-only and have
database triggers that reject mutation, deletion, skipped revisions, wrong
supersession, mismatched profile digests and cross-machine job links.

A completed assignment of the same exact profile is a no-op. A terminal
attempt that did not complete successfully creates a new assignment revision whose
`supersedes_assignment_id` points to the failed attempt. A nonterminal job blocks
another assignment. A different profile creates the next assignment revision,
whose `supersedes_assignment_id` points to the current assignment.

OpenClaw and Hermes both occupy `primary-agent-runtime`; one profile can select
at most one. Their typed Agent adapters prepare the new material, snapshot the
current runtime state, stop and disable it, enable and verify the replacement,
then restore the prior service, boot state, data, configuration, and exact
runtime identity when verification fails.

## Execution graph

The job ledger stores the resolver plan as same-machine, immutable, ordered
prerequisite edges. One job accepts at most 128 direct prerequisites. A child
becomes runnable only after every prerequisite succeeds. Claim validation is
enforced in both the Store query and a SQLite trigger.

If a prerequisite ends in `failed`, `rejected`, `lease_expired` or
`manual_intervention`, the Hub scheduler rejects every unstarted descendant in
the same polling transaction. Each rejection has code `DEPENDENCY_FAILED` and
recorded `hub_scheduler` / `dependency_graph` provenance. This code is not in
the agent rejection allowlist. Within one resource, lower nonterminal revisions
run first; independent resources keep creation order instead of comparing their
unrelated revision counters.

### Node runtime execution

One content-addressed gzip/tar bundle carries every supported platform tree:

```text
node-runtime/
  linux-amd64/
    bin/node
    lib/node_modules/npm/bin/npm-cli.js
  linux-arm64/
    bin/node
    lib/node_modules/npm/bin/npm-cli.js
  darwin-amd64/
    bin/node
    lib/node_modules/npm/bin/npm-cli.js
  darwin-arm64/
    bin/node
    lib/node_modules/npm/bin/npm-cli.js
  windows-amd64/
    bin/node.exe
    lib/node_modules/npm/bin/npm-cli.js
  windows-arm64/
    bin/node.exe
    lib/node_modules/npm/bin/npm-cli.js
```

The Hub resolves the package into a canonical `node-runtime-bundle:v1` spec
bound to the machine OS/architecture and an exact Hub artifact URL, SHA-256 and
size. The agent stops the response at the declared size, verifies the exact
bytes, accepts only canonical paths and safe relative symlinks, rejects links
and special files that can escape the release, and extracts only the selected
platform subtree.

The staged runtime must report the exact requested `node --version` (Windows:
`node.exe`); npm is verified through that exact Node binary. Activation
atomically replaces the `current` pointer (symlink on Unix; private regular
file on Windows). A failed post-activation check restores the prior target.
Successful retention keeps the active release and one prior exact-semver
release. OpenClaw jobs that declare this dependency remain unclaimable until
the Node runtime job succeeds.

### Node runtime intake

`artifact fetch node-runtime@<major.minor.patch>` reads the exact official
`SHASUMS256.txt` from `https://nodejs.org`, pins the Linux and Darwin
`.tar.gz` archives and the Windows `.zip` archives (x64 and arm64), and stores
that source plan with the durable fetch operation. The worker downloads every
exact archive, verifies each SHA-256, and rejects path traversal, noncanonical
or duplicate paths, special files, hard links, escaping symlinks, and
non-directory ancestors. Official Windows zips keep `node.exe` and
`node_modules/` at the archive root; the builder remaps those to
`bin/node.exe` and `lib/node_modules/` in the Hub bundle.

The builder removes the upstream top-level directory and emits the six
platform trees shown above with canonical ownership, modes, timestamps, tar
headers, ordering, and gzip header. Identical source bytes therefore produce
the same Hub SHA-256 and size. The published sidecar binds the Node version,
official checksum URL, complete source identity, output SHA-256, size, and
fetch time. The detailed source plan remains worker-only; preview and operation
DTOs expose its source kind and integrity identity.

### OpenClaw execution

The OpenClaw spec binds the exact npm artifact, its Hub SHA-256 and size, the
selected version, and the Node engine range. The Agent downloads no more than
the declared size, verifies the exact bytes, and invokes npm through the exact
managed Node binary inside a separate systemd scope. It validates the staged
package version and CLI before publishing the release.

Activation snapshots the current drop-in, configuration, database, active
state, enabled state, and alternate Hermes identity. The staged OpenClaw CLI
persists `gateway.mode=local`; the Agent then atomically updates the managed
`current` symlink, enables `openclaw-gateway.service`, and starts it. Success
requires the expected unit command, localhost health response, live process
command line, and exact OpenClaw version. Any failed step restores the prior
configuration, database, service boot state, runtime, and exact Hermes image
when Hermes was previously selected.

### Hermes execution and intake

`artifact fetch hermes-agent@<major.minor.patch>` resolves the exact official
`nousresearch/hermes-agent:v<version>` image from Docker Hub. The worker pins
the upstream index digest, downloads the Linux amd64 and arm64 descriptor
closure, verifies every blob digest and size, and publishes one deterministic
`oci-image-bundle:v1` artifact.

The machine spec binds the target OS/architecture, official image reference,
upstream index digest, bundle layout, Hub artifact URL, SHA-256, and size. The
Agent validates canonical archive paths, every blob, the complete descriptor
closure, and the selected platform before loading the image into rootless
Podman. Activation uses `clawctl-hermes.service`, persists `/opt/data` under the
managed state root, listens on localhost port 8642, and verifies the active
unit, exact running container image, and local image identity. OpenClaw and
Hermes units conflict at systemd level and their adapters also maintain exact
enabled state across forward activation and rollback.

## Catalog sources

Catalog sources provide candidates and provenance. Admission produces the
strict deployable manifest above.

| Source | Role |
|---|---|
| AI-Intune built-in | Supported runtimes and apps |
| Private catalog | Operator-owned packages and configuration |
| Pinokio Featured | Community discovery metadata |
| GitHub `topic:pinokio` | Unfiltered discovery metadata |

Pinokio repositories supply title, icon, upstream repository and installation
recipe metadata. The importer records that provenance and materializes an
AI-Intune manifest only when a typed adapter, immutable artifact, platform set,
dependencies, verification and rollback contract are present. Pinokio scripts
remain source material; they do not become endpoint job payloads.

Official source references:

- <https://github.com/pinokiocomputer/pinokio>
- <https://github.com/pinokiocomputer/sitefeed/blob/main/docs/featured.json>
- <https://github.com/pinokiocomputer/home/blob/main/js/app.js>

## Antigravity

Antigravity is a Standard Store app. Fetch reads the six latest-pointer manifests at
`https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/<platform>.json`
for `linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64`, `windows_amd64`, and
`windows_arm64`, then downloads the official files from `https://storage.googleapis.com`.
Only the version those manifests currently publish can be fetched. A different requested
version is refused, and the refusal names the version upstream currently publishes. The
catalog version is that `major.minor.patch` string. The build id stays in the release
directory and is not part of the version.

The bundle layout is `antigravity/<os>-<arch>/<official file>` plus `manifest.json` for
each of the six targets. Each Unix archive contains one root member, `antigravity`,
installed as `agy`. Windows ships the official `.exe`, installed as `agy.exe`.

After the release is activated, Unix `bin/` and `bin/agy` are mode `0555`. The release
directory stays writable. The executor runs `<release>/bin/agy --version` with
`AGY_CLI_DISABLE_AUTO_UPDATE=true`.

## Libraries not yet in the Store

The Standard Store admits `node-runtime`, `openclaw`, `hermes-agent`,
`claude-code`, `codex`, `grok`, `bat-server`, and `antigravity`. Catalog entry
does not install software; assignment of an exact profile revision is the only
install authority. Publishing a new package version does not move machines.

**Default library** (Claude, Codex, Grok/xAI, Antigravity CLIs, and Better
Agent Terminal) has official intake, a typed `clawctl-agent` adapter, and
Standard Store preview/publish for every package. Fetch does not install.

- `artifact fetch claude-code@<major.minor.patch>` pins
  `https://downloads.claude.ai/claude-code-releases/<version>/manifest.json`
  and the linux/darwin/windows amd64/arm64 binaries listed there. The adapter
  extracts the selected `bin/claude` or `bin/claude.exe`, verifies
  `claude --version`, and requires Hub artifact plus version measurements.
- `artifact fetch codex@<major.minor.patch>` pins
  `https://releases.openai.com/codex/releases/<version>/release.json`, requires
  tag `rust-v<version>`, and stores the six official `codex-package-*.tar.gz`
  archives (linux musl, darwin, windows msvc; amd64 and arm64) whose digests
  match both `release.json` and `codex-package_SHA256SUMS`. Release assets
  publish SHA-256 without a size; download requires a positive Content-Length
  inside the source limit. The bundle layout is
  `codex/<os>-<arch>/<package filename>` plus `codex/codex-package_SHA256SUMS`.
  GitHub fallback and the `latest` channel are outside this intake. The adapter
  extracts the selected `codex-package` archive, checks the official package
  members, verifies the last token of `codex --version`, and requires Hub
  artifact plus version measurements.
- `artifact fetch grok@<major.minor.patch>` pins `@xai-official/grok` and its
  six `@xai-official/grok-<os>-<cpu>` packages on `https://registry.npmjs.org`
  for linux, darwin, and windows amd64/arm64.
- `artifact fetch bat-server@<major.minor.patch>` pins the
  `tony1223/better-agent-terminal` GitHub release assets for linux amd64/arm64.
- `artifact fetch antigravity@<major.minor.patch>` follows
  [Antigravity](#antigravity).

Remaining, in order:

1. A default profile that selects those exact versions.
2. Assignment preview/apply remains the pin; upgrades are a new profile
   revision plus typed confirmation.

**Pinokio** remains optional discovery metadata. Upstream Featured JSON
(`https://raw.githubusercontent.com/pinokiocomputer/sitefeed/main/docs/featured.json`)
is an array of `{title, version, url, path, download, description, …}`. Scripts
in those repositories are source material, never job payloads. Remaining
intake, in order:

1. Bounded operator-supplied or fenced Featured document parse: HTTPS repo URL,
   title, version, provenance only. Reject install/script-shaped fields. No
   silent GitHub auto-upgrade.
2. Read-only review: source, provenance, visit-upstream link. Install action
   absent. If a Standard Store manifest already matches that exact identity,
   list that existing pin as the install authority.
3. Materialize a deployable manifest only when a typed adapter, immutable
   artifact, platform set, verification, and rollback exist.
4. Pin on assign: profile selects the exact materialized package version and source revision. Upgrade is check
   plus approve (new profile revision, preview, confirm). Catalog entry is not
   installed.
