#!/bin/sh
# Helper to set up Litestream and R2 environment variables.

validate_duration() {
  var_name=$1
  val=$2
  unit=${val#"${val%?}"}
  num=${val%?}
  case "$unit" in
    s|m|h) ;;
    *) echo "clawctl-fly: $var_name ($val) must be a duration ending in s, m, or h" >&2; return 1 ;;
  esac
  case "$num" in
    "" | *[!0-9]*) echo "clawctl-fly: $var_name ($val) must have a positive integer before the unit" >&2; return 1 ;;
    0*) echo "clawctl-fly: $var_name ($val) must not start with zero" >&2; return 1 ;;
  esac
  return 0
}

duration_to_seconds() {
  val=$1
  unit=${val#"${val%?}"}
  num=${val%?}
  case "$unit" in
    s) echo "$num" ;;
    m) echo "$((num * 60))" ;;
    h) echo "$((num * 3600))" ;;
  esac
}

export LITESTREAM_SYNC_INTERVAL="${LITESTREAM_SYNC_INTERVAL:-10s}"
export LITESTREAM_SNAPSHOT_INTERVAL="${LITESTREAM_SNAPSHOT_INTERVAL:-24h}"
export LITESTREAM_RETENTION="${LITESTREAM_RETENTION:-168h}"

validate_duration "LITESTREAM_SYNC_INTERVAL" "$LITESTREAM_SYNC_INTERVAL" || exit 1
validate_duration "LITESTREAM_SNAPSHOT_INTERVAL" "$LITESTREAM_SNAPSHOT_INTERVAL" || exit 1
validate_duration "LITESTREAM_RETENTION" "$LITESTREAM_RETENTION" || exit 1

snap_sec=$(duration_to_seconds "$LITESTREAM_SNAPSHOT_INTERVAL")
ret_sec=$(duration_to_seconds "$LITESTREAM_RETENTION")

if [ "$ret_sec" -lt "$snap_sec" ]; then
  echo "clawctl-fly: LITESTREAM_RETENTION ($LITESTREAM_RETENTION) must be >= LITESTREAM_SNAPSHOT_INTERVAL ($LITESTREAM_SNAPSHOT_INTERVAL)" >&2
  exit 1
fi

if [ -n "${R2_ACCOUNT_ID:-}" ]; then
  case $R2_ACCOUNT_ID in
    *[!0-9a-fA-F]*)
      echo "clawctl-fly: R2_ACCOUNT_ID must be 32 hex characters. The value is not printed." >&2
      exit 1
      ;;
  esac
  if [ "${#R2_ACCOUNT_ID}" -ne 32 ]; then
    echo "clawctl-fly: R2_ACCOUNT_ID must be 32 hex characters. The value is not printed." >&2
    exit 1
  fi
fi

if [ -z "${R2_ENDPOINT:-}" ] && [ -z "${LITESTREAM_ENDPOINT:-}" ] && [ -n "${R2_ACCOUNT_ID:-}" ]; then
  export R2_ENDPOINT="https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com"
  echo "clawctl-fly: derived R2_ENDPOINT from R2_ACCOUNT_ID"
fi
if [ -z "${LITESTREAM_ENDPOINT:-}" ] && [ -n "${R2_ENDPOINT:-}" ]; then
  export LITESTREAM_ENDPOINT="$R2_ENDPOINT"
fi
if [ -n "${LITESTREAM_ENDPOINT:-}" ] && [ -z "${R2_ENDPOINT:-}" ]; then
  export R2_ENDPOINT="$LITESTREAM_ENDPOINT"
  echo "clawctl-fly: set R2_ENDPOINT from LITESTREAM_ENDPOINT so Hub sees one R2 group"
fi
if [ -z "${LITESTREAM_BUCKET:-}" ] && [ -n "${R2_BUCKET:-}" ]; then
  export LITESTREAM_BUCKET="$R2_BUCKET"
fi
if [ -n "${LITESTREAM_BUCKET:-}" ] && [ -z "${R2_BUCKET:-}" ]; then
  export R2_BUCKET="$LITESTREAM_BUCKET"
fi
