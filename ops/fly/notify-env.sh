#!/bin/sh
# Helper to write notify.env for clawctl-hub from Fly secrets.

out_dir="${CLAWCTL_NOTIFY_OUT_DIR:-/run/clawctl-notify}"
chown_target="${CLAWCTL_NOTIFY_CHOWN_TARGET:-65532:65532}"

if [ -z "${TELEGRAM_BOT_TOKEN:-}" ] && [ -z "${TELEGRAM_CHAT_ID:-}" ] && \
   [ -z "${CLAWCTL_NOTIFY_WEBHOOK_URL:-}" ] && [ -z "${CLAWCTL_NOTIFY_WEBHOOK_SECRET:-}" ]; then
  return 0 2>/dev/null || exit 0
fi

nl='
'
cr=$(printf '\r')
for var in TELEGRAM_BOT_TOKEN TELEGRAM_CHAT_ID CLAWCTL_NOTIFY_WEBHOOK_URL CLAWCTL_NOTIFY_WEBHOOK_SECRET; do
  eval "val=\${$var:-}"
  case "$val" in
    *"$nl"*|*"$cr"*)
      echo "clawctl-fly: $var contains a newline or carriage return. The value is not printed." >&2
      return 1 2>/dev/null || exit 1
      ;;
  esac
done

if [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -z "${TELEGRAM_CHAT_ID:-}" ]; then
  echo "clawctl-fly: TELEGRAM_BOT_TOKEN is set but TELEGRAM_CHAT_ID is missing." >&2
  return 1 2>/dev/null || exit 1
fi
if [ -z "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${TELEGRAM_CHAT_ID:-}" ]; then
  echo "clawctl-fly: TELEGRAM_CHAT_ID is set but TELEGRAM_BOT_TOKEN is missing." >&2
  return 1 2>/dev/null || exit 1
fi

if [ -n "${CLAWCTL_NOTIFY_ENV:-}" ]; then
  echo "clawctl-fly: CLAWCTL_NOTIFY_ENV is already set, cannot automatically write notify secrets." >&2
  return 1 2>/dev/null || exit 1
fi

mkdir -p "$out_dir"
if [ "$(id -u)" -eq 0 ] && [ -n "$chown_target" ]; then
  chown "$chown_target" "$out_dir"
fi
chmod 0700 "$out_dir"

env_file="$out_dir/notify.env"

(
  umask 077
  > "$env_file"
  if [ -n "${TELEGRAM_BOT_TOKEN:-}" ]; then
    printf 'TELEGRAM_BOT_TOKEN=%s\n' "$TELEGRAM_BOT_TOKEN" >> "$env_file"
  fi
  if [ -n "${TELEGRAM_CHAT_ID:-}" ]; then
    printf 'TELEGRAM_CHAT_ID=%s\n' "$TELEGRAM_CHAT_ID" >> "$env_file"
  fi
  if [ -n "${CLAWCTL_NOTIFY_WEBHOOK_URL:-}" ]; then
    printf 'CLAWCTL_NOTIFY_WEBHOOK_URL=%s\n' "$CLAWCTL_NOTIFY_WEBHOOK_URL" >> "$env_file"
  fi
  if [ -n "${CLAWCTL_NOTIFY_WEBHOOK_SECRET:-}" ]; then
    printf 'CLAWCTL_NOTIFY_WEBHOOK_SECRET=%s\n' "$CLAWCTL_NOTIFY_WEBHOOK_SECRET" >> "$env_file"
  fi
)

if [ "$(id -u)" -eq 0 ] && [ -n "$chown_target" ]; then
  chown "$chown_target" "$env_file"
fi
chmod 0600 "$env_file"

export CLAWCTL_NOTIFY_ENV="$env_file"

unset TELEGRAM_BOT_TOKEN TELEGRAM_CHAT_ID CLAWCTL_NOTIFY_WEBHOOK_URL CLAWCTL_NOTIFY_WEBHOOK_SECRET || true
