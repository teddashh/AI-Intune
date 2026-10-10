---
name: fleet-tool-removal
description: Inventory, back up and remove an explicitly retired tool using exact manifest entries and secret-safe verification.
---

# Fleet tool removal

Read [TOOL-REMOVAL](../../docs/runbooks/TOOL-REMOVAL.md). Any agent can run the
read-only inventory and manifest preview. Check dependencies and package
identity before removal. Preserve git worktrees and project data. Never use
package globs or infer packages from a tool name.

Review the parse-only manifest. Confirm private backup coverage before apply.
Use the JSON-aware stripper for supported hook formats; review other formats
manually. Root steps must be explicit single-line hints when not running as
root. Run verify, inspect remaining ports and references, and record retained
data. Never print secret values or real fleet identifiers. Never merge a PR
without a human request.
