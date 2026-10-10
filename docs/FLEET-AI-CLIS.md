# Fleet AI CLIs

Run these tools as the main Linux user, for example `ops`. The standard is
Claude Code, Codex, Grok, and Google Antigravity CLI (`agy`). Gemini is legacy:
use agy. The installer reports no login success and never removes Gemini.

| CLI | Install | Login evidence |
| --- | --- | --- |
| claude | Global npm `@anthropic-ai/claude-code` | Automation wrapper and private OAuth token |
| codex | Global npm `@openai/codex` | `~/.codex/auth.json`, mode 600 |
| grok | `https://x.ai/cli/install.sh` | `~/.grok/auth.json`, mode 600 |
| agy | `https://antigravity.google/cli/install.sh` | OAuth layout under `~/.gemini/antigravity-cli`, when detectable |

Grok installs to `~/.grok/bin/grok`. Link it from `~/.local/bin/grok`.
The installer links agy from known user install locations when present.
Existing Node.js is kept. If Node.js is missing, install Node.js 22 through
NodeSource. Package steps set `NEEDRESTART_MODE=l`. Never run `apt upgrade`.
Scripts never log in or restart services.

## Check and install

From a checkout on the host, as the target user:

```bash
bash ops/fleet/ai-cli/check-ai-clis.sh
bash ops/fleet/ai-cli/check-ai-clis.sh --json
bash ops/fleet/ai-cli/install-ai-clis.sh --plan
bash ops/fleet/ai-cli/install-ai-clis.sh --apply
# Optional subset:
bash ops/fleet/ai-cli/install-ai-clis.sh --apply --only claude,codex
```

Plan is the default and changes nothing. Apply runs user steps. Non-root apply
prints root steps as single-line `sudo bash -c '...'` commands. Review and run
those commands, then re-check. Do not run the entire installer with sudo unless
root is the intended CLI user. It uses the current HOME for all user steps.
Keep the installer beside `claude-automation`; copy the directory together.
Backups of existing config files use `.bak-<UTC stamp>`.

The PATH fix prepends `~/.local/bin` and `~/.grok/bin` to `/etc/environment`
for pam_env and to a marked block at the top of `~/.bashrc`, before its
interactive early return. It does not source either file. Open a new SSH session:

```bash
ssh host-a grok --version
```

The checker approximates noninteractive resolution: the binary directory must
be in `/etc/environment` PATH or be `/usr/bin` or `/usr/local/bin`. It cannot
prove that the SSH server enables pam_env. Verify with the SSH command above.
`--user-home DIR` selects credential and rc paths for inspection; it does not
switch users or change PATH.

## Human logins

For Claude, run `claude setup-token` interactively. Save the long-lived,
non-refreshing OAuth token privately to
`~/.config/claude-automation/oauth-token`. Use mode 600 or 400. The file must
belong to the user and must not be a symlink. Never put its value in a command,
rc file, report, or log. The installer creates only the directory, mode 700.

Automation always uses:

```bash
claude-automation -p 'Reply with exactly the word OK'
```

Never automate bare `claude -p` or `claude --print`. The wrapper sets
`CLAUDE_CODE_OAUTH_TOKEN` from the private file and clears alternate Anthropic
credentials and cloud-provider switches. This avoids rotating the interactive
login in `~/.claude/.credentials.json`. Interactive Claude may stay logged out.
`CLAUDE_AUTOMATION_TOKEN_FILE` can select another private file.
`CLAUDE_AUTOMATION_CLAUDE_BIN` can select the real Claude executable.
Otherwise the wrapper searches PATH and skips itself. Invalid token files or
missing Claude exit 78. Do not enable shell tracing around credentials.

For Codex, a human runs:

```bash
codex login --device-auth
codex login status
```

A privately copied `~/.codex/auth.json` with mode 600 is also supported.
Complete Grok and agy login interactively. Keep Grok auth mode 600.
No script performs login. Auth-file presence is evidence, not proof of a valid
session. The agy layout is reported as unknown when it cannot be detected.

```bash
bash ops/fleet/ai-cli/check-ai-clis.sh --live --json
agy -p 'Reply with exactly the word OK'
```

Live checks send one small prompt per CLI and may incur usage charges. Each has
a 30-second timeout. Claude runs only through the wrapper. Codex uses
`codex exec --skip-git-repo-check`; Grok and agy use `-p`. Results are `ok`,
`auth_fail`, or `error`. Raw prompt output is never printed. Version checks
have a 10-second timeout and retain only a version number.

The checker reports `ok` (exit 0), `drift` (exit 1), or usage errors (exit 2).
JSON includes `clis`, `rc_token_findings`, `automation_misuse`, `gemini_legacy`,
`verdict`, `reasons`, and `next_steps`. Unknown agy login alone is not drift.
Findings include only file and line, never values. It inspects `.bashrc`,
`.profile`, `.bash_profile`, `.zshrc`, `.config/environment.d/*`,
`/etc/environment`, and `/etc/profile.d/*` for assignments of
`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
`OPENAI_API_KEY`, `XAI_API_KEY`, and `GEMINI_API_KEY`. Remove such assignments
privately. It also checks crontab and user systemd service files for bare
Claude print commands. These text checks are heuristics, not shell parsers.

## Fleet reports

Put one SSH alias per line in `hosts.txt`; `#` comments are allowed:

```text
host-a
host-b
```

```bash
ops/fleet/fleet-run.sh --hosts hosts.txt --out reports/ai-cli -- ops/fleet/ai-cli/check-ai-clis.sh --json
ops/fleet/fleet-report.sh reports/ai-cli > reports/ai-cli.md
```

The helper sends the local script over SSH stdin. Remote hosts need bash and
python3 for the checker. SSH uses BatchMode and disables agent forwarding.
Each host has a 180-second timeout and separate `.out` and `.rc` files.
A failed host does not stop remaining hosts. The helper exits 1 if any host
returns nonzero, including checker drift. `--ssh CMD` selects an alternate
SSH executable. Arguments are quoted for the remote shell.

Use this helper for scripts whose output is safe to store and summarize.
The checker produces sanitized output. Generic scripts must uphold the same
rule: never print secrets. Report directories are mode 700 and new output
files are mode 600. The report renders malformed or failed output as unknown.
