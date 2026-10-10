---
name: fleet-ai-cli
description: Install, check, and report the Linux fleet standard for Claude Code, Codex, Grok, and Google Antigravity CLI. Use for CLI drift, automation token hygiene, or noninteractive SSH PATH repair.
---

# Fleet AI CLIs

Follow [docs/FLEET-AI-CLIS.md](../../docs/FLEET-AI-CLIS.md).
This procedure works for Claude, Codex, and Grok Bot agents.

1. Run `bash ops/fleet/ai-cli/check-ai-clis.sh --json` as the target user.
2. Preview `bash ops/fleet/ai-cli/install-ai-clis.sh --plan`.
3. Apply with `--apply`, optionally `--only claude,codex,grok,agy`.
   Non-root runs print exact root commands. Keep the wrapper beside the installer.
4. Ask a human to perform the reported logins. Never log in from automation.
5. Re-check. Use `--live` when a real prompt is wanted. Confirm PATH with
   `ssh host-a grok --version` from a new session.
6. For fleet evidence, use `ops/fleet/fleet-run.sh` with the checker, then
   `ops/fleet/fleet-report.sh` on its output directory.

Never run bare `claude -p` or `claude --print` in automation. Use
`claude-automation`. Never place tokens in rc files, command arguments, logs,
or reports. Use agy, not the legacy Gemini CLI. Never restart services or
run apt upgrade. Keep examples generic. Do not copy private fleet identifiers.
