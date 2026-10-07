#!/bin/sh
# Hub Litestream restore drill
set -eu

usage() {
  echo "Usage: clawctl-restore-drill [--keep] [--timestamp VALUE] [--config PATH] [-h|--help]"
}

keep=0
timestamp=""
config="/etc/litestream.yml"

while [ $# -gt 0 ]; do
  case "$1" in
    --keep)
      keep=1
      shift
      ;;
    --timestamp)
      if [ $# -lt 2 ]; then
        echo "restore-drill: --timestamp requires a value" >&2
        usage >&2
        exit 2
      fi
      timestamp="$2"
      shift 2
      ;;
    --timestamp=*)
      timestamp="${1#--timestamp=}"
      shift
      ;;
    --config)
      if [ $# -lt 2 ]; then
        echo "restore-drill: --config requires a value" >&2
        usage >&2
        exit 2
      fi
      config="$2"
      shift 2
      ;;
    --config=*)
      config="${1#--config=}"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "restore-drill: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

# Source R2 env logic to derive endpoints
if [ -f /usr/local/lib/clawctl-fly/r2-env.sh ]; then
  . /usr/local/lib/clawctl-fly/r2-env.sh
else
  . "$(dirname "$0")/r2-env.sh"
fi

missing=""
if [ -z "${R2_ACCESS_KEY_ID:-}" ]; then
  missing="$missing R2_ACCESS_KEY_ID"
fi
if [ -z "${R2_SECRET_ACCESS_KEY:-}" ]; then
  missing="$missing R2_SECRET_ACCESS_KEY"
fi
if [ -z "${LITESTREAM_BUCKET:-}" ]; then
  missing="$missing LITESTREAM_BUCKET"
fi
if [ -z "${LITESTREAM_ENDPOINT:-}" ]; then
  missing="$missing LITESTREAM_ENDPOINT"
fi
if [ -n "$missing" ]; then
  echo "restore-drill: missing required environment variables:$missing" >&2
  exit 1
fi

if ! command -v litestream >/dev/null 2>&1; then
  echo "restore-drill: litestream not found on PATH" >&2
  exit 1
fi
if ! command -v sqlite3 >/dev/null 2>&1; then
  echo "restore-drill: sqlite3 not found on PATH" >&2
  exit 1
fi

if [ -z "${LITESTREAM_PATH:-}" ]; then
  export LITESTREAM_PATH="clawctl/litestream/clawctl.sqlite"
fi

db_path=${CLAWCTL_DB:-/var/lib/clawctl/clawctl.sqlite}

work=$(mktemp -d)
out="$work/clawctl.sqlite"

cleanup() {
  if [ "$keep" -eq 0 ]; then
    rm -rf "$work"
  fi
}
trap cleanup EXIT INT TERM

if [ -e "$out" ]; then
  echo "restore-drill: output path already exists: $out" >&2
  exit 1
fi

set -- restore -config "$config" -o "$out"
if [ -n "$timestamp" ]; then
  set -- "$@" -timestamp "$timestamp"
fi
set -- "$@" "$db_path"

echo "restore-drill: restoring to $out"
litestream "$@"

check=$(sqlite3 "$out" 'PRAGMA integrity_check;')
if [ "$check" != "ok" ]; then
  echo "restore-drill: integrity check failed: $check" >&2
  exit 1
fi

tables=$(sqlite3 "$out" "SELECT count(*) FROM sqlite_master WHERE type='table';")
machines="0"
if sqlite3 "$out" "SELECT 1 FROM sqlite_master WHERE type='table' AND name='machine_registry';" | grep -q 1; then
  machines=$(sqlite3 "$out" "SELECT count(*) FROM machine_registry;")
fi

echo "restore-drill: ok ($tables tables, $machines machine_registry rows)"
if [ "$keep" -eq 1 ]; then
  echo "restore-drill: kept restored file at $out"
fi
