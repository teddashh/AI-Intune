<p align="center">
  <img src="site/logo.svg" width="88" alt="AI-Intune logo">
</p>

<h1 align="center">AI-Intune</h1>

<p align="center">
  <strong>Evidence-first control for AI machines.</strong><br>
  <strong>以證據為核心的 AI 機隊管理平臺。</strong>
</p>

<p align="center">
  <a href="https://teddashh.github.io/AI-Intune/">Website</a> ·
  <a href="#english">English</a> ·
  <a href="#繁體中文">繁體中文</a> ·
  <a href="README.zh-TW.md">短版繁體中文</a> ·
  <a href="docs/PRODUCT.md">Product</a> ·
  <a href="docs/CONTROL-PLANE-CONTRACT.md">Control-plane contract</a>
</p>

<p align="center">
  <img alt="Linux, macOS, Windows" src="https://img.shields.io/badge/platform-Linux%20%7C%20macOS%20%7C%20Windows-111d19?style=flat-square">
  <img alt="Go" src="https://img.shields.io/badge/implementation-Go-00ADD8?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/license-Apache--2.0-e9f66f?style=flat-square&labelColor=111d19">
</p>

---

# English

AI-Intune is a focused control plane for Linux, macOS, and Windows machines running AI agents and coding tools. It gives one operator a path from enrollment to an exact software profile, execution, verification, and retained evidence. Linux is the deployed path; macOS and Windows Agent paths have repository tests and still need hardware acceptance.

The Hub and Agent binaries are named `clawctl-hub` and `clawctl-agent`.

## Project status

AI-Intune is an operating control plane, but it is not a complete Microsoft Intune replacement. The current scope includes fleet inventory, enrollment and lifecycle, app artifacts and profiles, dependency-aware jobs, deployments, reports, audit, Tailnet reconciliation, retention maintenance, and asynchronous backup restore drills. The Web console now follows workload-oriented navigation with **Devices**, **Apps**, **Endpoint security**, **Agents**, **Reports**, **Tenant administration**, and **Troubleshooting + support**.

The Hub registers independent verifiers in their own failure domain, accepts evidence from them on a separate credential plane, and reports that evidence as its own typed verdict on the job page. A sampleagent3 fleet-peer runner was deployed for OpenClaw verification; independent evidence is scoped to jobs it has actually checked, not every job. The pinned catalog path exists for Claude Code, Codex, Grok, Antigravity, and Linux BAT Server. A complete default profile is still missing. Ticket usage has one bounded contract across Web, CSV, the strict CLI, and `GET /v1/operator/tickets`. See the [feature inventory](docs/FEATURE-INVENTORY.md) for the exact delivery gaps. The first-principles review is a private working note and is not published here.

Production installations are private tailnet services, not a public demo. The console is available at the Tailscale listener configured during Hub installation and requires the matching Tailscale capability grant.

## Why it exists

AI workloads fail quietly. A process can remain alive while its login has expired, its runtime is wrong, or it has stopped completing useful work. Conventional device management can report packages, processes, and disk usage; it does not define what success means for an AI agent.

AI-Intune starts from two rules:

1. **Visibility comes before intervention.** The first job of the control plane is to tell you which named machine changed, stopped checking in, drifted from its expected state, or failed verification.
2. **Self-reporting is not proof.** A deployment succeeds only after concrete checks prove that the endpoint reached the intended state.

That turns the operating question from “did the command run?” into “what proves this machine is correct?”

## The managed-machine workflow

```text
build Hub + Agent bundles
          │
          ▼
install Hub on the tailnet
          │
          ▼
create one-time enrollment ──► install Tailscale + Agent + systemd service
          │                                      │
          │                                      ▼
          └──────── assign machine profile ◄── fresh check-in
                                                 │
                                                 ▼
                              resolve runtimes → install app → verify
                                                 │
                                                 ▼
                                      managed + auditable
```

One bootstrap operation establishes everything an endpoint needs to pull managed work:

- Tailscale installation and tailnet join when needed
- the architecture-specific static `clawctl-agent` binary
- one-time enrollment and a machine credential
- the systemd system service running with an explicit unprivileged `User=`, the
  lingered user manager for managed runtime services, and the enabled job loop
- rootless Podman plus the fixed OpenClaw and Hermes service contracts
- Hub artifact trust and local release/state directories

The selected profile then supplies the application. An OpenClaw profile resolves the exact Node runtime first, installs OpenClaw, persists its local gateway configuration, and activates the managed service; a Hermes profile resolves the exact official multi-architecture OCI image. The endpoint verifies the selected runtime before the Hub records the assignment as complete. Assigning the other profile performs the full OpenClaw ↔ Hermes switch and restores the prior runtime if verification fails.

## Core capabilities

### Fleet truth

- Named inventory and lifecycle state for every enrolled machine
- Check-in history, operating-system facts, versions, storage, processes, and credential observations
- Overview, machine detail, evidence drill-down, change report, audit trail, and Prometheus metrics
- Fleet-wide software inventory saying which tools are installed where, at which versions, and which machines have never reported one
- Daily summaries, deadman monitoring, and external notification hooks

### Enrollment and identity

- One-time enrollment tokens with preview/apply confirmation
- Authenticated Linux `amd64` and `arm64` bootstrap downloads with size and SHA-256
- Exact Hub/Agent release binding and post-install Hub receipt
- Machine retirement/reactivation and pending-token revocation
- Fleet-wide enrollment limit that refuses new tickets once the register reaches it
- Tailscale capability-based operator authentication

### Device configuration

- Immutable settings policies with revisions; republishing the same values keeps the current revision
- Machine and channel assignments resolved against Hub defaults, most specific first
- Resolved settings and their digest handed to the Agent on every check-in
- Applied state decided only by the digest the Agent reports running; unmeasured machines are never counted as applied
- Preview/apply digests, expected-revision compare-and-set, idempotency receipts, and audit for every publication and assignment

### Device compliance

- Compliance rules built only on facts the Hub itself measures: when it received the last check-in, the version and free disk the Agent reported, whether the Agent takes jobs, and the settings plane's own applied verdict
- No rule reads a machine's own claim about being compliant
- Verdicts are computed when read, never stored, because the same evidence legitimately yields a different verdict an hour later
- Every verdict is published beside the per-rule results it follows from; a machine with a rule that cannot be measured is never counted as compliant, and a machine with no assigned policy reads as unevaluated rather than compliant
- A policy may carry one consequence: once a machine has been noncompliant without interruption for longer than the grace period, the Hub stops handing it new jobs and leaves it out of new deployments, naming that as the exclusion reason
- The length of the run is proven from the check-ins inside the grace window; one check-in that passes restarts the clock
- Consequences are computed when read like the verdicts they follow from, so a machine that checks back in is handed work again without anyone clearing anything; a machine that has never reported and a machine with unmeasurable rules never trigger one
- Preview/apply digests, expected-revision compare-and-set, idempotency receipts, and audit for every publication and assignment

### Device action catalogue

- One named catalogue per machine: connect, diagnostic drill, deployment channel, revoke enrolment ticket, retire, restore
- Each entry's availability is taken from the preview that guards its own write path, so the catalogue cannot offer something the write path will refuse
- A blocked action carries a typed blocker, what is true right now, and the one thing that makes it available again
- Retirement and restoration are two directions of one action; only the direction that changes something is listed
- Only actions the operator's capabilities cover are listed at all — a view-only operator has no action section, not a row of buttons they cannot press
- The same catalogue backs the machine page, `machine actions`, and `GET /v1/operator/machines/{id}/actions`

### Standard Store and profiles

- Official artifact intake and durable fetch operations
- Content-addressed artifacts verified by exact SHA-256 and size
- Immutable package manifests with typed adapters and platform contracts
- Immutable machine profiles with deterministic dependency resolution
- Conflict and exclusive-group validation, including one primary agent runtime per profile
- Preview/apply digests, idempotency receipts, and recovery after ambiguous transport outcomes

### Execution and verification

- Per-machine desired-state revisions and leased jobs
- Immutable prerequisite edges and deterministic dependency-failure propagation
- Exact artifact download, archive validation, staging, atomic activation, verification, and rollback
- Ordered deployments with canary/stable channels, continue, retry, and abandon operations
- Append-only assignment history, execution evidence, verification records, and audit events
- A separate credential plane for verifiers registered in another failure domain, whose evidence is structurally kept apart from the executor's and reported as its own verdict

## Architecture

```text
┌──────────────────────── Operator plane ────────────────────────┐
│ Web console                 Strict CLI / API                    │
└───────────────────────────────┬─────────────────────────────────┘
                                │ Tailscale identity + capability
                                ▼
┌────────────────────────── clawctl-hub ─────────────────────────┐
│ inventory │ observations │ catalog │ profiles │ desired state │
│ jobs      │ deployments  │ receipts │ audit    │ metrics       │
│                       SQLite ledger                             │
└───────────────────────────────┬─────────────────────────────────┘
                                │ endpoint-initiated HTTP polling
              ┌─────────────────┴──────────────────┐
              ▼                                    ▼
       clawctl-agent                         clawctl-agent
       Linux / amd64                         Linux / arm64
       probes + typed adapters               probes + typed adapters
```

The Hub is the control ledger, not the AI data path. Endpoints open no inbound control port. OAuth tokens and model API keys remain on their endpoint. The Hub does not proxy model traffic. Managed package jobs select versioned, typed adapters rather than carrying arbitrary shell text.

Every mutation follows the same contract:

```text
read current state → preview exact impact → confirm typed target
→ apply with idempotency key → commit domain state + receipt + audit together
```

## Quick start

### 1. Build and test

Use Linux with Go `1.27.1`, `systemd`, `sudo`, and a Tailscale tailnet.

```bash
git clone https://github.com/teddashh/AI-Intune.git
cd AI-Intune
make test vet
make hub agent-bundles
```

This produces the Hub plus self-contained Linux `amd64` and `arm64` Agent bootstrap archives in `build/`.

### 2. Deploy

The primary path is any Linux host with Docker. Copy `ops/docker/hub.env.example` to `ops/docker/hub.env`, set `CLAWCTL_LISTEN` to this host's Tailscale IP and the port you chose, then:

```bash
docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env up -d --build
```

`8787` in examples is the conventional example port, not a Hub default. The grant `dst` port, `CLAWCTL_PUBLIC_URL`, the agent `--hub` URL, and any tunnel origin must use that same port. Full steps: [docs/DEPLOY-OSS.md](docs/DEPLOY-OSS.md).

systemd is the alternative when the host has no Docker (the install command below). Fly.io is the hosted example: [docs/DEPLOY-FLY.md](docs/DEPLOY-FLY.md). Cloudflare Containers is not implemented. It would need tsnet (userspace Tailscale inside the Hub process), Litestream, and a Durable Object keep-alive.

### 3. Configure operator identity and install the Hub (systemd alternative)

Add the Tailscale capability grant described in [Operator authentication](docs/OPERATOR-AUTH.md), then install as the non-root account that will run the Hub:

```bash
./ops/install-hub.sh \
  --listen 100.x.y.z:8787 \
  --operator-capability-prefix example.com/cap/clawctl
```

The installer publishes both Agent architectures under the exact running Hub version and starts `clawctl-hub.service` as a systemd user unit.

### 4. Enroll an endpoint

Open `http://100.x.y.z:8787/machines/enrollment`, create an enrollment for the machine, and download the archive matching its architecture. Extract it on the endpoint and run:

```bash
tar -xzf clawctl-agent-bootstrap-linux-amd64.tar.gz
cd clawctl-agent-bootstrap-linux-amd64
./install-agent.sh --hub http://100.x.y.z:8787
```

The installer prompts for the one-time enrollment token. On an endpoint that has not joined the tailnet, it also prompts for a Tailscale auth key. For unattended provisioning, pass both through mode-`0600` files:

```bash
./install-agent.sh \
  --hub http://100.x.y.z:8787 \
  --token-file ./enrollment-token \
  --tailscale-auth-key-file ./tailscale-auth-key
```

Installation finishes after the new service has checked in and the Hub has confirmed the exact Agent version and active job channel.

An enrollment limit caps how many machines this Hub will take. It counts the machines on the register that have not been retired — the same denominator as **Reports → Enrollment** — so revoking a ticket or letting one expire does not make room; retirement does. At the limit, ticket creation is refused on every plane, and both the enrollment page and `enroll-token --preview` say so before you commit. Not setting a limit is not a limit of zero: a limit of zero is a real setting that stops every new machine. Set, change, or remove it under **Enrollment limit** on the same page, or from the CLI:

```bash
clawctl-hub enrollment-limit
clawctl-hub enrollment-limit --set 50 --reason 'licence ceiling' --preview
clawctl-hub enrollment-limit --set 50 --reason 'licence ceiling'
clawctl-hub enrollment-limit --clear --reason 'licence ceiling lifted'
```

Changing the limit is a preview/apply confirmation: the preview states what the limit is now, what it would become, and how many machines are on the register, and a limit below the current count retires nothing.

### 5. Publish and assign an app profile

In **Apps**, fetch the official artifacts, publish their standard packages, publish a profile selecting either OpenClaw or Hermes, then assign that profile to the enrolled machine. The assignment preview shows the complete resolved graph and the current assignment it supersedes before apply.

For OpenClaw, the canonical sequence is:

```text
fetch official artifacts
→ publish Node runtime
→ publish OpenClaw bound to that runtime
→ publish machine profile
→ preview assignment
→ apply assignment
→ inspect job and verification evidence
```

For Hermes, fetch and publish `hermes-agent`, publish a profile containing it, and assign it. OpenClaw and Hermes are mutually exclusive primary runtimes; assigning one stops and disables the other, persists the selection across reboot, and verifies the live service and exact runtime identity.

The Web console is the normal operator surface. The strict CLI and JSON API use the same application services for automation and recovery; see [API surface](docs/API-SURFACE.md).

### 6. Publish and assign agent settings

Open **Devices → Configuration** to see the settings every Agent is meant to run beside the settings each one reported running. Publish a policy, assign it to a machine or to a channel, and the Hub hands the resolved values to the Agent on its next check-in. Machine assignments win over channel assignments, which win over the Hub defaults.

A machine counts as applied only when it echoes the exact digest the Hub resolved for it. Every other state — never reported, unknown, waiting for the next check-in, running something else — is shown as its own state and never as applied. The equivalent HTTP-first CLI flow is:

```bash
clawctl-hub settings list
clawctl-hub settings publish --policy tighter-checkin \
  --checkin 90s --observation 5m --preview
clawctl-hub settings publish --policy tighter-checkin \
  --checkin 90s --observation 5m --reason 'shorten fleet check-in'
clawctl-hub settings assign --scope channel --scope-id canary \
  --policy tighter-checkin --revision 1 --reason 'canary first'
```

Publishing the same values again keeps the current revision instead of creating a new one. Check-in intervals accept 30s to 1h, observation intervals 1m to 24h, and an observation interval may not be shorter than the check-in interval it belongs to.

### 7. Judge the fleet against a compliance policy

Open **Devices → Compliance** to see each machine's verdict beside the rule results it follows from. Publish a rule set, assign it to a machine or to a channel, and the Hub judges every machine against it on every read. Machine assignments win over channel assignments; a machine with no assignment reads as unevaluated.

The verdict carries its own evidence: each rule reports pass, fail, or no evidence, and one unmeasurable rule is enough to keep a machine out of the compliant count. Nothing is stored — the same machine can legitimately move from compliant to noncompliant with no new check-in, because freshness is measured against the Hub's clock at the moment you read the board.

Add `--block-jobs-after` and the verdict changes what the fleet does: a machine that has been noncompliant without interruption for longer than that grace period stops being handed jobs and is left out of new deployments. The board says which machines are in grace, when each one becomes due, and how many are being withheld work right now. The equivalent HTTP-first CLI flow is:

```bash
clawctl-hub compliance list
clawctl-hub compliance publish --policy fleet-floor \
  --checkin-max-age 15m --settings-applied --preview
clawctl-hub compliance publish --policy fleet-floor \
  --checkin-max-age 15m --settings-applied \
  --block-jobs-after 1h --reason 'fleet floor'
clawctl-hub compliance assign --scope channel --scope-id canary \
  --policy fleet-floor --revision 1 --reason 'canary first'
```

The same rules written in any order are the same rule set and keep the current revision. Check-in age accepts 60s to 168h in whole seconds, free-disk floors accept 1 to 99 percent, and a version rule compares the Agent's reported version against the exact string you published. `--block-jobs-after` accepts 0 to 24h in whole seconds, where 0 means the moment the verdict says noncompliant; changing it publishes a new revision, because it is a different decision.

### 8. Review Tailnet inventory

Open **Endpoint security** to compare Tailscale's observed peers with the managed roster. Ignore rules target the stable node ID, require a reason and expiry, and are reviewed before confirmation. The equivalent HTTP-first CLI flow is:

```bash
clawctl-hub tailnet
clawctl-hub tailnet ignore --peer-id node-id --reason 'personal device' --preview
clawctl-hub tailnet ignore --peer-id node-id --reason 'personal device' \
  --confirm-hostname laptop --days 30
```

Normal CLI use discovers the authenticated Hub. An explicit `--db` selects the stopped-service break-glass path.
If the Tailnet source is unavailable, reads exit non-zero but still print locally persisted active ignore rules so existing exceptions remain removable.

### 9. Review retention maintenance

Open **Tenant administration → Maintenance** to inspect the active retention policy and its latest completed run. An admin can preview exact deleted/retained counts and protected newest evidence before entering the displayed `DELETE N ROWS` confirmation. The equivalent HTTP-first CLI flow is:

```bash
clawctl-hub prune
clawctl-hub prune --apply --reason 'scheduled retention maintenance' \
  --confirm 'DELETE 42 ROWS'
```

Use the exact row count printed by the immediately preceding preview; `42` above is only an example. The preview is read-only. Apply commits deletions, the retention ledger, the idempotency receipt, and audit evidence atomically. An explicit `--db` remains a stopped-service break-glass path.

### 10. Run a backup restore drill

Open **Tenant administration → Maintenance** to preview the newest standalone backup, review its pinned identity and SHA-256, enter the displayed `VERIFY filename` confirmation, and follow the durable background operation to completion. The equivalent HTTP-first CLI flow is:

```bash
clawctl-hub restore-drill preview
clawctl-hub restore-drill run --reason 'quarterly recovery verification' \
  --confirm 'VERIFY clawctl-YYYYMMDDTHHMMSSZ-before-revision.sqlite'
clawctl-hub restore-drill list
clawctl-hub restore-drill show OPERATION_ID
```

The worker copies the exact pinned backup into a private temporary file, validates it with the production schema, and compares restored registry evidence with the live control plane. A completion stamp is written only after success. The drill never replaces the live database or modifies the backup. An explicit `--db` remains a stopped-service, writer-fenced break-glass path.

### 11. Review Agent activity, reports, enrollment, one machine's timeline, and what this Hub keeps

Open **Agents** to review desired-state jobs, leases, event chronology, and verification evidence. Open **Reports** to see every report this Hub produces before opening any of them: what each one answers, how far back it can still see under the retention policy in force, and whether it can be exported whole. Open **Reports → Enrollment** to see which of the machines this Hub was told to manage have actually turned up: the register is the denominator, every row states the moment someone declared it, whether a ticket is still waiting or expired unused, whether the machine has ever reported, how long it has been waited for, and what to do next. Retirement is the only way a row leaves the denominator; revoking a ticket or letting it expire does not remove it. Open **Reports → Software inventory** to see what is installed across the fleet, at which versions, and which machines do not have it. Every machine in the register gets a cell for every tool, and a cell that says "never reported" is kept apart from one that says "not on this machine": the first sends you to that machine's agent, the second sends you to install something. Newest means the newest version seen in this fleet, not the newest version published upstream — this Hub has no upstream version source, and the page says so. Open **Reports → Install state** to see what this Hub told each machine to install and what it sees on that machine. Every machine in the register gets a cell for every resource. What was assigned is the highest-revision intent across that machine's own scope and the scope of the channel it is in — the same rule the agent applies to what it receives, so the page cannot disagree with what the machine was actually sent. A cell where the assignment is older than what is running is not a failure and is never called one: a thing can be installed by a path that does not go through assignment, and this Hub cannot see that path, so the page says so. The five ways a cell cannot be lined up stay five different answers with five different next steps, and "never assigned" is a state of its own rather than a blank. Open **Reports → Ticket usage** to select a 7, 14, or 30 day fixed Hub-received window, filter by an opaque provider reference, and export the same bounded projection shown by the console. Open a machine's **Event timeline** to read registry, health verdicts, jobs, and operator actions for that machine as one newest-first list, with each source stating how many rows it read and whether it reached the start of the window. The equivalent HTTP-first CLI reads are:

```bash
clawctl-hub job list
clawctl-hub job evidence JOB_ID
clawctl-hub report list
clawctl-hub report enrollment
clawctl-hub report enrollment --csv
clawctl-hub report software
clawctl-hub report software --csv
clawctl-hub report install
clawctl-hub report install --csv
clawctl-hub tickets --days 14
clawctl-hub tickets --days 14 --json
clawctl-hub tickets --days 14 --csv
clawctl-hub machine timeline --machine MACHINE_ID --days 7
clawctl-hub machine timeline --machine MACHINE_ID --days 7 --csv
clawctl-hub data
clawctl-hub machine data --machine MACHINE_ID
clawctl-hub machine data --machine MACHINE_ID --csv
```

Open **Tenant administration → Data disclosure** to read what this Hub keeps about a machine at all: thirteen kinds of data covering every table that stores a machine identifier, each stating what it holds, whether the machine reported it or the Hub decided it, whether it contains free text, how long it stays under the retention policy in force, and what survives retirement. Open a machine's **Data** page for that same catalogue carrying this machine's own row counts, oldest and newest row, and the cutoff the current retention policy applies. Which tables hold machine data is answered by the schema itself, and which of them are pruned on a clock is answered by the code that does the deleting.

Ticket completion counts are kept separate from verification success. A report is only offered as an export when it is whole inside its range; the cursor-paged reports are not, because such a file would carry one page while looking like the entire report. Every export shares one contract: byte order mark, UTC RFC 3339 timestamps, an empty cell for an absent value, and spreadsheet-formula protection on every cell.

## Current application scope

| Component | Platforms | State |
|---|---|---|
| Management Agent | Linux, macOS, Windows `amd64`, `arm64` | Linux deployed; macOS/Windows bootstrap, enrollment, polling, probe, and jobs covered by repository tests; hardware acceptance pending |
| Node runtime | Linux, macOS, Windows `amd64`, `arm64` | Official intake, deterministic bundle, install, verify, rollback |
| Claude Code, Codex, Grok, Antigravity | Linux, macOS, Windows `amd64`, `arm64` | Official intake, pinned catalog, typed install and measurement paths |
| BAT Server | Linux `amd64`, `arm64` | Official intake, pinned catalog, managed service path |
| OpenClaw | Linux `amd64`, `arm64` | Official intake, managed first install, dependency binding, health verification, rollback |
| Hermes | Linux `amd64`, `arm64` | Official OCI intake, rootless Podman activation, identity verification, rollback |
| Profile replacement | Linux `amd64`, `arm64` | Append-only supersession and verified OpenClaw ↔ Hermes switching |

OpenClaw and Hermes belong to the exclusive `primary-agent-runtime` group: a machine profile selects one, never both. The Standard Store can grow from AI-Intune-supported packages, operator-owned private packages, and Pinokio discovery metadata. A Pinokio recipe becomes deployable only after it is converted into AI-Intune's immutable artifact, typed adapter, dependency, verification, and rollback contract.

## Evidence model

AI-Intune keeps different claims separate instead of compressing them into one green status:

| Evidence | Answers |
|---|---|
| Machine check-in | Is the management Agent present and current? |
| Probe facts | What is installed, running, expiring, full, or changed? |
| Desired state | What exact package revision should this machine have? |
| Reported settings digest | Which settings is this machine actually running right now? |
| Job events | What was claimed, staged, activated, retried, or rejected? |
| Verification result | Which concrete rule passed or failed, with what output? |
| Assignment receipt | Which profile and dependency graph were committed? |
| Audit event | Who requested the change, from where, and under which capability? |

This separation is what makes dashboards, reports, retries, and recovery trustworthy.

## Repository map

| Path | Purpose |
|---|---|
| `cmd/clawctl-hub/` | Hub server, Web console, CLI, APIs, schedulers, and workers |
| `cmd/clawctl-agent/` | Endpoint probe, enrollment, job loop, adapters, and verification |
| `internal/` | Domain services, strict clients, catalog, deployment, auth, and SQLite store |
| `ops/` | Tested installers, upgrades, bundle publication, monitoring, and notifications |
| `site/` | This project's bilingual GitHub Pages site |
| `docs/` | Product, contract, architecture, feature inventory, and operating guides |

Start with these documents:

- [Technical spec](docs/SPEC.md)
- [Product definition](docs/PRODUCT.md)
- [Control-plane contract](docs/CONTROL-PLANE-CONTRACT.md)
- [Independent verifier topology and failure domains](docs/VERIFIER-TOPOLOGY.md)
- [App Catalog and machine profiles](docs/APP-CATALOG.md)
- [Operator authentication](docs/OPERATOR-AUTH.md)
- [API surface](docs/API-SURFACE.md)
- [Feature inventory](docs/FEATURE-INVENTORY.md)
- [Open-source deploy guide](docs/DEPLOY-OSS.md)
- [Short Traditional Chinese page](README.zh-TW.md)

Engineering and operations handoff notes are private working notes and are not published here.

## Development

```bash
make test        # all Go tests plus the complete ops test suite
make vet         # Go static analysis
make build       # local Hub and Agent
make cross       # Linux amd64 and arm64 Agent binaries
make agent-bundles
```

`make test` puts the Go test temporary directory on `/dev/shm` when it exists. Running `go test ./...` directly with `TMPDIR` on disk makes the Store, Hub, Web, operator, and Agent packages exceed the default 10-minute package timeout.

The Agent is built with `CGO_ENABLED=0` so the management plane does not share the runtime failure domain of the Node applications it manages.

## License

AI-Intune is licensed under the [Apache License 2.0](LICENSE).

---

# 繁體中文

AI-Intune 是管理 Linux、macOS 與 Windows AI 機器的控制平臺。它讓單一管理者從註冊端點、指派精確軟體 profile、執行工作單、驗證狀態到保留證據。Linux 已有部署路徑；macOS 與 Windows Agent 目前有程式庫測試，仍需實機驗收。

Hub 與 Agent 的執行檔名稱分別是 `clawctl-hub` 與 `clawctl-agent`。

## 專案目前狀態

AI-Intune 已是可運作的控制平臺，但不是完整的 Microsoft Intune 替代品。目前涵蓋機隊名冊、註冊與生命週期、應用程式 artifacts 與 profiles、具依賴關係的工作單、部署、報告、稽核、Tailnet 對照、資料保留維護，以及非同步備份還原演練。Web console 已改為工作負載式導覽，包括 **裝置**、**應用程式**、**端點安全性**、**代理程式**、**報告**、**租用戶管理**與**疑難排解 + 支援**。

Hub 可以註冊位於另一個 failure domain 的獨立 verifier、以獨立的憑證平面接收證據，並在工作單頁面呈現獨立 verdict。sampleagent3 已部署 OpenClaw fleet-peer runner；只有它實際量測過的工作單才有第二個 producer 的證據。Claude Code、Codex、Grok、Antigravity 與 Linux BAT Server 已有釘版套件路徑；完整預設 profile 仍缺。票證使用量由 Web、CSV、嚴格 CLI 與 `GET /v1/operator/tickets` 共用同一個有界契約。精確現況見 [feature inventory](docs/FEATURE-INVENTORY.md)。第一性原理檢驗是私人工作筆記，不在這個公開倉庫。

正式部署是 tailnet 內的私有服務，不是公開 demo。管理中心位於安裝 Hub 時設定的 Tailscale listener，且必須具有相符的 Tailscale capability grant 才能開啟。

## 為什麼需要 AI-Intune

AI 工作經常安靜地失敗。Process 可能還活著，但登入已經過期、runtime 版本不對，或 agent 早已沒有完成任何有效工作。傳統裝置管理可以回報套件、process 與磁碟使用量，卻沒有定義 AI agent 到底怎樣才算真正成功。

AI-Intune 從兩條規則出發：

1. **先看清楚，再動手。** 控制平臺首先要準確指出：哪一台具名機器發生變化、停止 check-in、偏離預期狀態，或沒有通過驗證。
2. **自我回報不算證據。** 只有具體檢查證明端點已經到達預期狀態，部署才算成功。

因此維運時問的不再是「指令有沒有跑」，而是「什麼證明這台機器現在是對的」。

## 完整納管流程

```text
建立 Hub + Agent bundles
          │
          ▼
在 tailnet 安裝 Hub
          │
          ▼
建立一次性 enrollment ──► 安裝 Tailscale + Agent + systemd service
          │                                      │
          │                                      ▼
          └────────── 指派 machine profile ◄── 首次 check-in
                                                 │
                                                 ▼
                              解析 runtime → 安裝 app → 驗證
                                                 │
                                                 ▼
                                           完整納管 + 可稽核
```

一次 bootstrap 會建立端點取得管理工作的全部基礎：

- 在需要時安裝 Tailscale 並加入 tailnet
- 符合 CPU 架構的靜態 `clawctl-agent` 執行檔
- 一次性註冊與 machine credential
- 以明確非特權 `User=` 執行的 systemd system service、供受管 runtime
  services 使用的 lingered user manager，以及已啟用的 job loop
- rootless Podman，以及固定的 OpenClaw、Hermes service contract
- Hub artifact trust 與本機 release/state 目錄

接著由選定的 profile 補上應用程式。OpenClaw profile 會先解析精確 Node runtime、安裝 OpenClaw、寫入本機 gateway 設定，再啟用受管 service；Hermes profile 則解析官方 multi-architecture OCI image。端點驗證選定的 runtime 後，Hub 才會將整次指派記錄為完成。改派另一個 profile 時會完成 OpenClaw ↔ Hermes 全程切換；驗證失敗則恢復原 runtime。

## 核心能力

### 機隊真實狀態

- 每台已註冊機器都有具名 inventory 與 lifecycle state
- Check-in 歷史、作業系統 facts、版本、儲存空間、process 與 credential observations
- Overview、機器詳細頁、證據逐層檢視、變更報告、audit trail 與 Prometheus metrics
- 全機隊軟體清查：哪個工具裝在哪幾台、各是哪一版、哪幾台從來沒回報過
- 每日摘要、deadman monitoring 與外部通知 hook

### 註冊與身分

- 具有 preview/apply 確認的一次性 enrollment token
- 經驗證的 Linux `amd64`、`arm64` bootstrap 下載，畫面顯示大小與 SHA-256
- 精確綁定 Hub/Agent release，安裝後由 Hub 回傳完成收據
- 機器退役／重新啟用與未使用 token 撤銷
- 全機隊註冊上限，名冊滿了就不再開新的票
- 以 Tailscale capability 驗證 operator 身分與權限

### 裝置組態

- 不可變的設定原則與 revision；以相同的值重新發布會保留目前 revision
- Machine 與 channel 指派對照 Hub 預設值解析，愈明確者優先
- 每次 check-in 都把解析後的設定與其 digest 交給 Agent
- 是否已套用只由 Agent 回報自己正在跑的 digest 決定；量不到的機器永遠不算已套用
- 每次發布與指派都有 preview/apply digest、expected-revision compare-and-set、冪等收據與 audit

### 裝置合規性

- 規則只建立在 Hub 自己量得到的事實上：它收到上一次 check-in 的時刻、Agent 回報的版本與剩餘磁碟、Agent 是否收工作單，以及設定平面自己的套用判決
- 沒有任何一條規則讀機器自己對合規的主張
- 判決在讀的時候現算，不落表，因為同一批證據在一小時後可以合法地變成另一個判決
- 每個判決旁邊就是推出它的每一條規則結果；只要有一條規則量不到，那台就不算符合；沒有指派原則的機器顯示為未指派，不是符合
- 一份原則可以帶一個後果：一台機器連續不符合超過寬限期之後，Hub 就不再發新的工作單給它，新的部署也會把它排除，並且直接說排除理由是這個
- 連續多久是從寬限期那段窗裡的每一次 check-in 證明出來的；中間只要有一次通過，時鐘就重新起算
- 後果跟它依據的判決一樣是讀的時候現算，所以機器一恢復報到就自己領得回工作單，不需要任何人解鎖；從未報到的機器與有規則量不到的機器都不會觸發任何後果
- 每次發布與指派都有 preview/apply digest、expected-revision compare-and-set、冪等收據與 audit

### 裝置動作目錄

- 每台機器一份具名目錄：連線、開啟終端、診斷工作單、重新命名、編輯名冊備註、部署通道、撤銷註冊票、退役、恢復管理
- `connect` 與 `open_terminal` 從網頁執行；`open_terminal` 只給這台的指派使用者，而且只出現在 Linux 機器上。指派使用者最多八列，其他人最多七列。終端工作階段預設閒置 30 分鐘（不計輸出與心跳）或開啟滿 12 小時會自動關閉，可透過 `CLAWCTL_TERMINAL_IDLE_TIMEOUT` 與 `CLAWCTL_TERMINAL_MAX_LIFETIME` 調整。
- 每一項的可用與否直接取自守住它自己那條寫入路徑的 preview，所以目錄不會列出一件按下去會被拒絕的事
- 被擋的動作會說出 typed blocker、現在的狀況，以及讓它重新可用的那一件事
- 退役與恢復管理是同一個動作的兩個方向；只列出會改變狀態的那一個
- 只列出操作員的 capability 涵蓋得到的動作 —— view-only 的人看到的是沒有動作那一節，不是一排按不動的按鈕
- 單機頁、`machine actions` 與 `GET /v1/operator/machines/{id}/actions` 用的是同一份目錄

### Standard Store 與 Profile

- 官方 artifact intake 與具持久狀態的 fetch operation
- 依精確 SHA-256 與大小驗證的 content-addressed artifact
- 綁定 typed adapter 與 platform contract 的不可變 package manifest
- 具確定性依賴解析的不可變 machine profile
- 衝突與 exclusive group 檢查；每個 profile 只允許一個 primary agent runtime
- Preview/apply digest、冪等收據與 transport 結果不明時的 recovery

### 執行與驗證

- 每台機器獨立的 desired-state revision 與 leased job
- 不可變 prerequisite edge 與確定性的 dependency failure 傳遞
- 精確 artifact 下載、archive 驗證、staging、原子啟用、驗證與 rollback
- 具 canary/stable channel 的有序 deployment，以及 continue、retry、abandon 操作
- Append-only assignment 歷史、執行證據、verification record 與 audit event
- 為另一個 failure domain 的 verifier 保留的獨立憑證平面；它寫的證據與 executor 的在結構上分開，並以自己的 verdict 呈現

## 架構

```text
┌──────────────────────── Operator plane ────────────────────────┐
│ Web console                 Strict CLI / API                    │
└───────────────────────────────┬─────────────────────────────────┘
                                │ Tailscale identity + capability
                                ▼
┌────────────────────────── clawctl-hub ─────────────────────────┐
│ inventory │ observations │ catalog │ profiles │ desired state │
│ jobs      │ deployments  │ receipts │ audit    │ metrics       │
│                       SQLite ledger                             │
└───────────────────────────────┬─────────────────────────────────┘
                                │ 端點主動發起 HTTP polling
              ┌─────────────────┴──────────────────┐
              ▼                                    ▼
       clawctl-agent                         clawctl-agent
       Linux / amd64                         Linux / arm64
       probe + typed adapter                 probe + typed adapter
```

Hub 是控制 ledger，不是 AI 資料路徑。端點不開放 inbound 控制 port；OAuth token 與 model API key 留在各自端點；Hub 不代理 model traffic；受管理的 package job 只選擇有版本的 typed adapter，不攜帶任意 shell 文字。

所有變更都遵守相同 contract：

```text
讀取現況 → preview 精確影響 → 確認具型別的目標
→ 使用 idempotency key 套用 → domain state + receipt + audit 同時 commit
```

## 快速開始

### 1. 建立並測試

使用具備 Go `1.27.1`、`systemd`、`sudo` 與 Tailscale tailnet 的 Linux 環境。

```bash
git clone https://github.com/teddashh/AI-Intune.git
cd AI-Intune
make test vet
make hub agent-bundles
```

完成後，`build/` 會包含 Hub，以及自含的 Linux `amd64`、`arm64` Agent bootstrap archives。

### 2. 部署

主要路徑是任何有 Docker 的 Linux 主機。把 `ops/docker/hub.env.example` 複製成 `ops/docker/hub.env`，將 `CLAWCTL_LISTEN` 設成這台機器的 Tailscale IP 與你選的埠，然後：

```bash
docker compose -f ops/docker/docker-compose.yml --env-file ops/docker/hub.env up -d --build
```

文件裡的 `8787` 只是慣例範例埠，不是 Hub 的預設埠。Tailscale grant 的 `dst` 埠、`CLAWCTL_PUBLIC_URL`、agent 的 `--hub`、以及 tunnel origin 都必須跟 `CLAWCTL_LISTEN` 使用同一個埠。完整步驟見 [docs/DEPLOY-OSS.md](docs/DEPLOY-OSS.md)。

沒有 Docker 時改走 systemd（下面的安裝指令）。Fly.io 是文件裡的託管範例：[docs/DEPLOY-FLY.md](docs/DEPLOY-FLY.md)。Cloudflare Containers 尚未實作；那條路需要 tsnet（Hub 行程內的 userspace Tailscale）、Litestream，以及 Durable Object keep-alive。

### 3. 設定 operator 身分並安裝 Hub（systemd 替代）

先依照 [Operator authentication](docs/OPERATOR-AUTH.md) 加入 Tailscale capability grant，再由實際執行 Hub 的 non-root account 安裝：

```bash
./ops/install-hub.sh \
  --listen 100.x.y.z:8787 \
  --operator-capability-prefix example.com/cap/clawctl
```

安裝器會將兩種 Agent 架構發布到精確的 Hub 版本之下，並以 systemd user unit 啟動 `clawctl-hub.service`。

### 4. 註冊端點

開啟 `http://100.x.y.z:8787/machines/enrollment`，建立該機器的 enrollment，並下載符合 CPU 架構的 archive。將 archive 放到端點、解開後執行：

```bash
tar -xzf clawctl-agent-bootstrap-linux-amd64.tar.gz
cd clawctl-agent-bootstrap-linux-amd64
./install-agent.sh --hub http://100.x.y.z:8787
```

安裝器會要求輸入一次性 enrollment token；尚未加入 tailnet 的端點也會要求 Tailscale auth key。無人值守佈署則將兩者放在 mode `0600` 的檔案傳入：

```bash
./install-agent.sh \
  --hub http://100.x.y.z:8787 \
  --token-file ./enrollment-token \
  --tailscale-auth-key-file ./tailscale-auth-key
```

新 service 完成 check-in，並由 Hub 確認精確 Agent 版本與已啟用 job channel 後，安裝才會結束。

註冊上限限制這個 Hub 收得下幾台。它算的是名冊上沒有退役的台數，也就是 **報告 → 註冊** 的那個分母；撤票或讓票過期都不會空出名額，退役才會。到了上限，每一個介面都開不出新的票，註冊頁與 `enroll-token --preview` 在送出之前就先講。沒有設上限不是上限 0：上限 0 是一個真的可以設的值，意思是誰都不准再納管。要設定、調整或取消，到同一頁的 **註冊上限**，或走 CLI：

```bash
clawctl-hub enrollment-limit
clawctl-hub enrollment-limit --set 50 --reason 'licence ceiling' --preview
clawctl-hub enrollment-limit --set 50 --reason 'licence ceiling'
clawctl-hub enrollment-limit --clear --reason 'licence ceiling lifted'
```

改上限是 preview/apply 確認：預覽會講現在的上限、要改成幾台、名冊上現在幾台，而一個比目前台數還小的上限不會退役任何一台。

### 5. 發布並指派 App Profile

在 **Apps** 取得官方 artifacts、發布標準套件、發布選擇 OpenClaw 或 Hermes 的 profile，然後將 profile 指派給已註冊機器。Assignment preview 會在 apply 前顯示完整解析後的 dependency graph，以及這次會取代的目前 assignment。

OpenClaw 的標準順序是：

```text
取得官方 artifacts
→ 發布 Node runtime
→ 發布綁定該 runtime 的 OpenClaw
→ 發布 machine profile
→ preview assignment
→ apply assignment
→ 檢查 job 與 verification evidence
```

Hermes 則依序取得並發布 `hermes-agent`、建立包含它的 profile，再指派給機器。OpenClaw 與 Hermes 是互斥的 primary runtime；指派其中一個會停止並停用另一個、保存開機後的選擇，並驗證 live service 與精確 runtime identity。

Web console 是正常操作介面。Strict CLI 與 JSON API 使用相同 application service，提供 automation 與 recovery；完整入口見 [API surface](docs/API-SURFACE.md)。

### 6. 發布並指派代理程式設定

開啟 **裝置 → 組態**，可以並排看到每台 Agent 該跑的設定，以及它回報自己正在跑的設定。發布一份設定原則、指派給某台機器或某個 channel，Hub 就會在它下次報到時把解析後的值交給它。機器指派優先於 channel 指派，channel 指派優先於 Hub 預設值。

只有當機器回送的 digest 與 Hub 為它解析出的 digest 完全一致，才算已套用。其餘狀態——從未回報、無法判定、等下次報到、正在跑別的——各自顯示為自己的狀態，不會被算成已套用。相同的 HTTP-first CLI 流程是：

```bash
clawctl-hub settings list
clawctl-hub settings publish --policy tighter-checkin \
  --checkin 90s --observation 5m --preview
clawctl-hub settings publish --policy tighter-checkin \
  --checkin 90s --observation 5m --reason '縮短機隊報到間隔'
clawctl-hub settings assign --scope channel --scope-id canary \
  --policy tighter-checkin --revision 1 --reason '先讓 canary 跑'
```

以相同的值重新發布會保留目前 revision，不會產生新的。報到間隔接受 30s 到 1h，量測間隔接受 1m 到 24h，且量測間隔不得短於同一份原則的報到間隔。

### 7. 用合規性原則判決機隊

開啟 **裝置 → 合規性**，每台機器的判決旁邊就是推出它的那幾條規則結果。發布一組規則、指派給某台機器或某個 channel，Hub 就會在每次讀取時重新判決整個機隊。機器指派優先於 channel 指派；沒有指派的機器顯示為未指派。

判決自己帶證據：每條規則各自回報通過、不通過或沒有證據，只要有一條量不到，那台就不會被算進符合。判決不存檔——同一台機器可以在沒有新 check-in 的情況下合法地從符合變成不符合，因為新鮮度是在你讀盤面的那一刻對 Hub 的鐘量出來的。

加上 `--block-jobs-after`，判決就會真的改變機隊的行為：連續不符合超過那個寬限期的機器不再收到工作單，新的部署也會把它排除。盤面說得出哪幾台還在寬限中、各自什麼時候到期，以及現在總共有幾台被停發工作單。相同的 HTTP-first CLI 流程是：

```bash
clawctl-hub compliance list
clawctl-hub compliance publish --policy fleet-floor \
  --checkin-max-age 15m --settings-applied --preview
clawctl-hub compliance publish --policy fleet-floor \
  --checkin-max-age 15m --settings-applied \
  --block-jobs-after 1h --reason '機隊底線'
clawctl-hub compliance assign --scope channel --scope-id canary \
  --policy fleet-floor --revision 1 --reason '先讓 canary 跑'
```

同一組規則換個順序寫仍然是同一組規則，會保留目前 revision。報到年齡接受 60s 到 168h 的整秒，剩餘磁碟下限接受 1 到 99 百分比，版本規則則拿 Agent 回報的版本與你發布的字串逐字比對。`--block-jobs-after` 接受 0 到 24h 的整秒，0 表示判定不符合就立即生效；改寬限期會發布新的 revision，因為那是另一個決定。

### 8. 檢查 Tailnet 名冊

在 **端點安全性** 對照 Tailscale 觀測到的 peers 與受管名冊。忽略規則以 stable node ID 為目標，必須填理由與效期，並先預覽再確認。相同的 HTTP-first CLI 流程是：

```bash
clawctl-hub tailnet
clawctl-hub tailnet ignore --peer-id node-id --reason '個人裝置' --preview
clawctl-hub tailnet ignore --peer-id node-id --reason '個人裝置' \
  --confirm-hostname laptop --days 30
```

一般 CLI 會自動發現已驗證的 Hub；只有明示 `--db` 才使用 stopped-service break-glass 路徑。
Tailnet 來源不可用時，讀取指令會以非零狀態回報，但仍列出本地持久化的有效忽略規則，因此既有例外仍可取消。

### 9. 檢查資料保留維護

開啟 **租用戶管理 → 維護**，可以檢查目前保留政策與最近一次清理結果。Admin 會先看到精確的刪除／保留數量與受保護的各組最新證據，之後才輸入畫面顯示的 `DELETE N ROWS` 確認字串。相同的 HTTP-first CLI 流程是：

```bash
clawctl-hub prune
clawctl-hub prune --apply --reason '例行資料保留維護' \
  --confirm 'DELETE 42 ROWS'
```

請使用緊接在套用前的 preview 所顯示的精確列數；上面的 `42` 只是範例。預覽完全唯讀。套用時，刪除內容、retention ledger、idempotency receipt 與 audit evidence 會在同一個 transaction 提交；明示 `--db` 才進入 stopped-service break-glass 路徑。

### 10. 執行備份還原演練

開啟 **租用戶管理 → 維護**，先預覽最新的 standalone 備份，檢查固定的檔案身分與 SHA-256，再輸入畫面顯示的 `VERIFY 檔名`，並在 operation detail 追蹤背景作業。相同的 HTTP-first CLI 流程是：

```bash
clawctl-hub restore-drill preview
clawctl-hub restore-drill run --reason '季度備份復原驗證' \
  --confirm 'VERIFY clawctl-YYYYMMDDTHHMMSSZ-before-revision.sqlite'
clawctl-hub restore-drill list
clawctl-hub restore-drill show OPERATION_ID
```

Worker 會把固定的備份複製到私有暫存檔，以正式 schema 開啟驗證，並核對備份名冊與目前控制面的 expected 數量；只有全部成功才寫入完成章。演練不會替換正式資料庫，也不會修改備份原檔。明示 `--db` 才進入有 writer fence 的 stopped-service break-glass 路徑。

### 11. 檢查代理程式活動、報告、註冊、單機事件時間軸與這個 Hub 留了什麼

開啟 **代理程式**，檢查期望狀態工作單、租約、事件時間軸與驗證證據。開啟 **報告**，在點進任何一份之前就看得到這個 Hub 做得出哪些報告：各自回答什麼、在現行保留期下看得回去多遠、能不能整份匯出。開啟 **報告 → 註冊**，看說好要納管的機器來了沒有：名冊就是分母，每一列各自交代誰在什麼時候說要納管它、票還在等還是過期沒用、它報到過沒有、已經等了多久，以及下一步做什麼。離開分母只有一條路：退役；撤票與票過期都不會讓一列名冊消失。開啟 **報告 → 軟體清查**，看機隊上裝了什麼、各是哪一版、哪幾台沒有：名冊上每一台對每一個工具都有一格，而「沒回報過」跟「這台上沒有」是分開的兩格——前者要去看那台的 agent，後者要去裝東西。裝著東西的那幾格再答一個問題：那個版號，講的是不是正在跑的那一份。一台上有兩份安裝的時候，那一頁會把量版號的那個檔案與正在跑的那個檔案並排列出來，並把那幾格排在版號不一致前面——那些格子的版號量的是沒在跑的那一份，所以「誰比較新」比的是一個沒有人在用的檔案。「最新」指的是這個機隊裡看到的最新版，不是上游發布的最新版；這個 Hub 沒有上游版本來源，而這句話就寫在那一頁上。開啟 **報告 → 安裝狀態**，看這個 Hub 叫哪幾台裝什麼、又在它們上面看到什麼：名冊上每一台對每一個資源都有一格。「指派的」取這台自己的 scope 與它所在 channel 的 scope 裡 revision 最大的那一筆，跟 agent 判斷它收到什麼走的是同一條規則，所以這一頁不會跟機器實際收到的那一筆不一致。一格「指派的比看到的舊」不是失敗，這一頁也不會那樣講：東西可以從指派以外的路徑裝上去，而這個 Hub 看不到那條路，這句話就寫在那一頁上。對不起來的五種情況維持五種不同的答案與五種不同的下一步，「沒有被指派過」是自己的一種狀態，不是空白。開啟 **報告 → 票證使用量**，可選擇 7、14 或 30 天的固定 Hub 接收時間窗、依 opaque provider reference 篩選，並匯出與管理中心相同的有界投影。開啟某一台的 **事件時間軸**，把名冊、健康判定、工作單與操作員動作照時間由新到舊讀成一條，並由每一個來源各自交代它讀到幾列、有沒有讀完這段期間。對應的 HTTP-first CLI 為：

```bash
clawctl-hub job list
clawctl-hub job evidence JOB_ID
clawctl-hub report list
clawctl-hub report enrollment
clawctl-hub report enrollment --csv
clawctl-hub report software
clawctl-hub report software --csv
clawctl-hub report install
clawctl-hub report install --csv
clawctl-hub tickets --days 14
clawctl-hub tickets --days 14 --json
clawctl-hub tickets --days 14 --csv
clawctl-hub machine timeline --machine MACHINE_ID --days 7
clawctl-hub machine timeline --machine MACHINE_ID --days 7 --csv
clawctl-hub data
clawctl-hub machine data --machine MACHINE_ID
clawctl-hub machine data --machine MACHINE_ID --csv
```

開啟 **租用戶管理 → 資料揭露**，讀這個 Hub 對一台機器到底留了什麼：十四類資料涵蓋每一張存著機器識別碼的表，各自交代留的是什麼、是機器自報還是 Hub 判定、含不含自由文字、在現行保留期下留多久、退役之後還剩什麼。開啟某一台的 **資料**，同一份目錄會帶上這台自己的列數、最舊與最新的一列，以及現行保留期算出來的清除界線。哪些表存著機器的資料由 schema 本身回答，其中哪些會被時間清則由真的在刪東西的那段程式回答。

回合完成數與驗證成功分開呈現。只有在範圍裡完整的報告才給匯出：cursor 分頁的報告匯出的是當下那一頁，而那個檔案打開之後看起來跟整份報告一模一樣。每一份匯出共用同一套契約：BOM、UTC RFC 3339 時間、沒有值就留空格，以及每一格都擋試算表公式。

## 目前的應用程式範圍

| 元件 | 平臺 | 狀態 |
|---|---|---|
| Management Agent | Linux、macOS、Windows `amd64`、`arm64` | Linux 已部署；macOS／Windows bootstrap、註冊、polling、probe、job 有程式庫測試，待實機驗收 |
| Node runtime | Linux、macOS、Windows `amd64`、`arm64` | 官方 intake、確定性 bundle、安裝、驗證、rollback |
| Claude Code、Codex、Grok、Antigravity | Linux、macOS、Windows `amd64`、`arm64` | 官方 intake、釘版 catalog、具型別安裝與量測路徑 |
| BAT Server | Linux `amd64`、`arm64` | 官方 intake、釘版 catalog、受管服務路徑 |
| OpenClaw | Linux `amd64`、`arm64` | 官方 intake、受管首次安裝、依賴綁定、health verification、rollback |
| Hermes | Linux `amd64`、`arm64` | 官方 OCI intake、rootless Podman 啟用、identity verification、rollback |
| Profile replacement | Linux `amd64`、`arm64` | Append-only supersession 與已驗證的 OpenClaw ↔ Hermes 切換 |

OpenClaw 與 Hermes 同屬 exclusive `primary-agent-runtime` group：一個 machine profile 只會選擇其中之一。Standard Store 的來源可包括 AI-Intune 官方支援套件、operator 自有 private package，以及 Pinokio discovery metadata。Pinokio recipe 只有在轉換成 AI-Intune 的不可變 artifact、typed adapter、dependency、verification 與 rollback contract 後，才會成為可部署套件。

## 證據模型

AI-Intune 不把不同來源的宣告壓成一顆綠燈，而是分開保存：

| 證據 | 回答的問題 |
|---|---|
| Machine check-in | Management Agent 是否存在而且仍在運作？ |
| Probe facts | 目前安裝了什麼、什麼在跑、即將過期、空間不足或發生變化？ |
| Desired state | 這台機器應該具有哪一個精確 package revision？ |
| 回報的設定 digest | 這台機器此刻實際在跑的是哪一份設定？ |
| Job events | 哪一步已 claim、stage、activate、retry 或 reject？ |
| Verification result | 哪一條具體規則通過或失敗？實際輸出是什麼？ |
| Assignment receipt | 哪一個 profile 與 dependency graph 已經 commit？ |
| Audit event | 誰從哪裡、使用什麼 capability 要求這次變更？ |

這種分離方式讓 dashboard、report、retry 與 recovery 都有可信的基礎。

## Repository 地圖

| 路徑 | 用途 |
|---|---|
| `cmd/clawctl-hub/` | Hub server、Web console、CLI、API、scheduler 與 worker |
| `cmd/clawctl-agent/` | 端點 probe、註冊、job loop、adapter 與 verification |
| `internal/` | Domain service、strict client、catalog、deployment、auth 與 SQLite store |
| `ops/` | 經測試的安裝、升級、bundle 發布、monitoring 與通知工具 |
| `site/` | 本專案的雙語 GitHub Pages 網站 |
| `docs/` | 產品、contract、架構、功能清單與操作文件 |

建議先閱讀：

- [技術規格](docs/SPEC.md)
- [產品定義](docs/PRODUCT.md)
- [控制平臺 contract](docs/CONTROL-PLANE-CONTRACT.md)
- [獨立 verifier 的 topology 與 failure domain](docs/VERIFIER-TOPOLOGY.md)
- [App Catalog 與 machine profile](docs/APP-CATALOG.md)
- [Operator authentication](docs/OPERATOR-AUTH.md)
- [API surface](docs/API-SURFACE.md)
- [功能清單](docs/FEATURE-INVENTORY.md)
- [開放部署指南](docs/DEPLOY-OSS.md)
- [短版繁體中文](README.zh-TW.md)

開發與維運交接是私人工作筆記，不在這個公開倉庫。

## 開發

```bash
make test        # 全部 Go tests 與完整 ops test suite
make vet         # Go static analysis
make build       # 本機 Hub 與 Agent
make cross       # Linux amd64 與 arm64 Agent binaries
make agent-bundles
```

`make test` 在有 `/dev/shm` 時把 Go 測試的暫存目錄放在那裡。直接跑 `go test ./...` 而 `TMPDIR` 在磁碟上時，Store、Hub、Web、operator 與 Agent 這幾包會超過預設的 10 分鐘套件時限。

Agent 固定使用 `CGO_ENABLED=0` 建立，確保 management plane 不會與它管理的 Node application 共用同一個 runtime failure domain。

## 授權

AI-Intune 採用 [Apache License 2.0](LICENSE)。
