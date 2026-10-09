---
name: clawctl-fly-deploy
description: Use when deploying clawctl Hub on Fly.io as public HTTPS Autopilot, including first-run MFA setup, verification, and disposable app cleanup.
---

# Fly Autopilot deployment

Use plain shell from the repository root. Read `docs/DEPLOY-FLY.md`. Prerequisites: flyctl installed (`fly` or `flyctl` on PATH), Python 3, curl, and a Fly org token in `FLY_API_TOKEN`. Load the token privately into the environment; never print it or pass it as a command argument.

Ask the human for the org, app name, and region (default `iad`). Decide the custom domain before any real enrollment. R2 backups and Telegram notifications are optional. Without R2, leave every `R2_*` and `LITESTREAM_*` setting unset. If optional secrets are supplied, use a regular mode-0600 file outside the repository; never read its contents into chat.

## Commands

Replace placeholders with the confirmed choices. First preview:

```sh
ops/fly/deploy.sh --org <org> --app <app> --region iad --dry-run
```

Before deploying a production app, have the human confirm the exact app name. Then:

```sh
ops/fly/deploy.sh --org <org> --app <app> --region iad
# With optional secrets:
ops/fly/deploy.sh --org <org> --app <app> --region iad --secrets-file /private/fly-secrets.env
```

The script copies the public config outside the tree, creates only missing resources, deploys, and scales to one Machine. It waits for `/healthz` and checks whether `/setup` is still open before reading first-run logs. A setup code may be printed once by deploy.sh on the operator's own terminal on first run only. Never paste it into chat.

Humans can complete `/setup` in the browser. For non-browser setup, have the operator save the code privately, then run:

```sh
ops/fly/setup-admin.sh --app <app> --username <username> \
  --setup-code-file /private/setup-code --out /private/admin-credentials.json
# After a custom domain switch:
ops/fly/setup-admin.sh --url https://hub.example.com --username <username> \
  --setup-code-file /private/setup-code --out /private/admin-credentials.json
```

The helper generates a password and writes username, password, TOTP secret, and ten recovery codes to a new mode-0600 JSON file. It never prints those values and refuses overwrites. Use secure storage for the file. It uses one keep-alive HTTPS connection for setup, enrollment, and confirmation.

Account security can regenerate ten recovery codes with the current password and an unused authenticator code; recovery codes are not accepted. All previous codes are invalidated. Failures count toward the shared account/client lockout. New codes are shown once, and the regeneration audit contains no codes. Alternatively, stop the Hub and run `clawctl-hub regenerate-recovery-codes --db PATH --username U`, then restart it; the host command requires MFA enabled and prints one code per line. An authenticator step used to sign in cannot be reused: wait for the next 30-second step before regenerating, and use one keep-alive HTTPS connection for login and regeneration.

To destroy a confirmed disposable app:

```sh
ops/fly/deploy.sh --app <app> --destroy --dry-run
ops/fly/deploy.sh --app <app> --destroy
```

Type the exact app name at the prompt. Non-TTY stdin is supported, but must supply the same exact name on one line. There is no flag that skips the typed name. Destroy removes the app's machines, its clawctl_data volumes, and the app; it never deploys.

## Verification

- HTTPS `/healthz` returns 200 with valid TLS; never disable certificate verification.
- A wrong Host on `/login` returns 421. `/healthz` intentionally exempts Host for Fly probes and is not the Host test.
- MFA is required: after enrollment, password-only login does not create a session. Complete authenticator verification privately.
- Audit client IP is the real client IP, not a proxy address in `172.16.0.0/12` or `fdaa:`. Check trusted proxies and `Fly-Client-IP` if wrong.
- Perform an enroll/retire drill with a disposable agent in an isolated HOME. Use a keyed Linux installer (amd64 and arm64 are available) or `clawctl-agent enroll` with private token input. Verify identity and a fresh HTTPS check-in, then retire through the UI/API. Keep the token out of chat and avoid the operator's existing agent state.
- A live terminal WebSocket at `/v1/agent/terminal-link` requires a local bat-server endpoint. HTTPS check-in alone does not establish that WebSocket; do not claim it does.
- Run a restore drill only when R2 replication is configured.

## Stop rules

- No resources beyond the documented one shared-cpu-1x 1024mb Machine and 3GB clawctl_data volume without asking. The classic build fallback may temporarily create a builder; the script tears down only newly created fly-builder-* apps.
- Never allocate a dedicated IPv4. Never create Postgres.
- Never merge. Do not deploy or destroy a production app without the human confirming the app name.
- Never paste secrets, setup codes, passwords, TOTP secrets or codes, recovery codes, or enrollment tokens into chat. Keep raw logs and credential files private.

## Troubleshooting

- Depot handshake, `Waiting for depot builder...`, or `failed to list workers`: deploy.sh retries once with `--depot=false --yes` and tears down new classic builders. Other failures are not retried.
- Builder left behind: teardown failure names the app and exits non-zero. Confirm that exact builder was created by the retry before removing it. Preserve pre-existing builders and all unrelated apps.
- Setup already closed (`/setup` 404): use `/login`. Do not recover an old setup code from logs. setup-admin.sh exits without writing a file.
- MFA `Pending login expired`: login is bound to the full client IP. New connections can rotate egress IP; use one keep-alive HTTPS connection. First-run setup uses `__Host-clawctl_session` directly, not `hub_pending_mfa`.
- Restore drill missing R2 variables: configure replication first. The drill requires `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`, `LITESTREAM_BUCKET`, and `LITESTREAM_ENDPOINT`; it does not work without R2.
- Wrong-host 421 after switching domains: open the origin set in `CLAWCTL_PUBLIC_URL`, verify DNS and TLS, and enroll real machines only after that origin is stable. Old agent URLs require re-enrollment.
