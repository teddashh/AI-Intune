---
name: fleet-host-bootstrap
description: Bootstrap a Linux fleet host, check corporate proxy and clock settings, or configure SSH from an operator box with userspace Tailscale. Use with Claude, Codex, or Grok Bot.
---

# Fleet host bootstrap

Follow [New host](../../docs/runbooks/NEW-HOST.md),
[Corporate network](../../docs/runbooks/CORPORATE-NETWORK.md), and
[Operator box SSH](../../docs/runbooks/OPERATOR-BOX-SSH.md).

1. Run bootstrap's default plan. Check OS, architecture, user, disk, sudo, clock,
   DNS, and direct versus proxy health. Resolve failed checks before enrollment.
2. Apply only the requested phases. Non-root runs print explicit root steps;
   never invoke interactive sudo automatically. Use `--resume` for completed phases.
3. Keep auth keys and enrollment tokens in private files outside the repository.
   Report metadata only. Never print contents or put values in argv or logs.
4. Enroll one host from its user-owned 0700 keyed package directory. Check the
   service locally, then verify identity and a fresh check-in on the Hub.
5. Remove staged packages after verification. Review clock and proxy results
   after changes. Keep tailnet and local ranges in NO_PROXY.
6. For userspace Tailscale, preview the SSH block before `--write`. Use `--check`
   to verify the connection. A tunnel jump host is a documented fallback only.

Use generic examples in this public repository. Do not copy fleet identifiers
from source notes. Tests use PATH stubs and never contact real hosts.
