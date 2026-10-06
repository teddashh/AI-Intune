#!/bin/sh
# Fly.io entrypoint for clawctl-hub.
#
# Runs as root only long enough to start kernel-mode tailscaled and to
# prepare the data directory. Hub and Litestream then run as uid/gid 65532.
# This script execs that process, so SIGTERM reaches Litestream (or Hub)
# as PID 1. Litestream forwards the signal to the Hub child and flushes.
#
# Tailscale state lives at $CLAWCTL_DATA/tailscale, root-owned mode 0700.
# hub-data-init excludes that directory from its chown. Hub's writer lock
# checks only the SQLite parent directory, so the extra subdirectory is
# allowed. The node keeps the same Tailscale IP while this state and the
# Fly volume survive. Agents enroll against that literal IP.
#
# The auth key is never printed. It is written to a root-only file and
# passed as tailscale's --auth-key=file: form (tailscale v1.102), then
# removed. TS_AUTHKEY is unset before the long-running process starts.
set -eu

data=${CLAWCTL_DATA:-/var/lib/clawctl}
state_dir=$data/tailscale
state_file=$state_dir/tailscaled.state
socket=/var/run/tailscale/tailscaled.sock
keyfile=

cleanup_key() {
  if [ -n "${keyfile:-}" ]; then
    rm -f "$keyfile"
  fi
  unset TS_AUTHKEY || true
}
trap cleanup_key EXIT

reject_whitespace() {
  name=$1
  eval "val=\${$name:-}"
  case $val in
    *[[:space:]]*)
      echo "clawctl-fly: $name contains whitespace. The value is not printed." >&2
      exit 1
      ;;
  esac
}

have_state=0
if [ -L "$state_file" ]; then
  echo "clawctl-fly: refusing symlink $state_file" >&2
  exit 1
fi
if [ -f "$state_file" ] && [ -s "$state_file" ]; then
  have_state=1
fi
if [ "$have_state" -eq 0 ] && [ -z "${TS_AUTHKEY:-}" ]; then
  echo "clawctl-fly: TS_AUTHKEY is required until Tailscale state exists at $state_file." >&2
  echo "clawctl-fly: set the Fly secret TS_AUTHKEY. The key is not printed." >&2
  exit 1
fi
if [ -n "${TS_AUTHKEY:-}" ]; then
  reject_whitespace TS_AUTHKEY
fi

if [ ! -c /dev/net/tun ]; then
  echo "clawctl-fly: /dev/net/tun is missing." >&2
  echo "clawctl-fly: kernel-mode tailscaled needs a Fly Firecracker VM, or another host that provides /dev/net/tun." >&2
  exit 1
fi

for name in R2_ACCOUNT_ID R2_ACCESS_KEY_ID R2_SECRET_ACCESS_KEY R2_BUCKET R2_ENDPOINT LITESTREAM_BUCKET LITESTREAM_ENDPOINT LITESTREAM_PATH TS_HOSTNAME TS_TAGS; do
  reject_whitespace "$name"
done

host=${TS_HOSTNAME:-clawctl-hub}
tags=${TS_TAGS:-tag:clawctl-hub}
case $host in
  *[!A-Za-z0-9.-]* | "")
    echo "clawctl-fly: TS_HOSTNAME must be letters, digits, dots, and hyphens" >&2
    exit 1
    ;;
esac
case $tags in
  *[!A-Za-z0-9:,_-]* | "")
    echo "clawctl-fly: TS_TAGS must be comma-separated tag: names" >&2
    exit 1
    ;;
esac
case $tags in
  tag:*) ;;
  *)
    echo "clawctl-fly: TS_TAGS must start with tag:" >&2
    exit 1
    ;;
esac

port=${CLAWCTL_PORT:-8787}
case $port in
  *[!0-9]*)
    echo "clawctl-fly: CLAWCTL_PORT must be a number from 1 to 65535. 8787 is only the conventional example." >&2
    exit 1
    ;;
esac
if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
  echo "clawctl-fly: CLAWCTL_PORT must be a number from 1 to 65535" >&2
  exit 1
fi

mkdir -p "$state_dir" /var/run/tailscale /run/clawctl
chmod 0700 "$state_dir" /run/clawctl
chown 0:0 "$state_dir"

echo "clawctl-fly: starting tailscaled (state $state_dir)"
# tailscaled does not read TS_AUTHKEY; keep it out of its environment.
env -u TS_AUTHKEY tailscaled \
  --statedir="$state_dir" \
  --socket="$socket" \
  --port=41641 \
  >>"$state_dir/tailscaled.log" 2>&1 &
ts_pid=$!

i=0
while [ "$i" -lt 30 ]; do
  if [ -S "$socket" ]; then
    break
  fi
  if ! kill -0 "$ts_pid" 2>/dev/null; then
    echo "clawctl-fly: tailscaled exited during startup. See $state_dir/tailscaled.log" >&2
    exit 1
  fi
  i=$((i + 1))
  sleep 1
done
if [ ! -S "$socket" ]; then
  echo "clawctl-fly: timed out waiting for $socket" >&2
  exit 1
fi

up_base="--socket=$socket up --hostname=$host --advertise-tags=$tags"
if [ -n "${TS_AUTHKEY:-}" ]; then
  keyfile=/run/clawctl/ts-authkey
  umask 077
  printf '%s' "$TS_AUTHKEY" >"$keyfile"
  chmod 0600 "$keyfile"
  unset TS_AUTHKEY || true
  # file: keeps the key off the process argument list. env -u keeps it out
  # of tailscale's environment as well.
  if ! env -u TS_AUTHKEY timeout 60 tailscale $up_base --auth-key="file:$keyfile"; then
    echo "clawctl-fly: tailscale up failed. The auth key was not printed." >&2
    exit 1
  fi
  rm -f "$keyfile"
  keyfile=
else
  if ! timeout 60 tailscale $up_base; then
    echo "clawctl-fly: tailscale up failed using persisted state. Set TS_AUTHKEY if the node must log in again." >&2
    exit 1
  fi
fi

ip=
i=0
while [ "$i" -lt 30 ]; do
  ip=$(tailscale --socket="$socket" ip -4 2>/dev/null | head -n 1 || true)
  case $ip in
    100.*)
      break
      ;;
  esac
  ip=
  i=$((i + 1))
  sleep 1
done
if [ -z "$ip" ]; then
  echo "clawctl-fly: timed out waiting for tailscale ip -4" >&2
  exit 1
fi
export CLAWCTL_LISTEN="${ip}:${port}"
echo "clawctl-fly: CLAWCTL_LISTEN=${CLAWCTL_LISTEN}"

chown root:65532 "$socket"
chmod 0660 "$socket"

export CLAWCTL_DATA="$data"
export CLAWCTL_CHOWN_EXCLUDE=tailscale
/bin/sh /usr/local/bin/hub-data-init.sh

export CLAWCTL_DB="${CLAWCTL_DB:-$data/clawctl.sqlite}"
export CLAWCTL_REPORT_STAMP="${CLAWCTL_REPORT_STAMP:-$data/last-report.stamp}"
if [ -L "$CLAWCTL_DB" ]; then
  echo "clawctl-fly: refusing symlink $CLAWCTL_DB" >&2
  exit 1
fi

litestream_required=0
case ${LITESTREAM_REQUIRED:-} in
  1 | true | TRUE | yes | YES) litestream_required=1 ;;
  "" | 0 | false | FALSE | no | NO) litestream_required=0 ;;
  *)
    echo "clawctl-fly: LITESTREAM_REQUIRED must be 1, true, yes, or unset" >&2
    exit 1
    ;;
esac

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

any_replica=0
if [ -n "${R2_ACCOUNT_ID:-}${R2_ACCESS_KEY_ID:-}${R2_SECRET_ACCESS_KEY:-}${R2_BUCKET:-}${R2_ENDPOINT:-}${LITESTREAM_BUCKET:-}${LITESTREAM_ENDPOINT:-}${LITESTREAM_PATH:-}" ]; then
  any_replica=1
fi

missing=""
if [ -z "${R2_ACCESS_KEY_ID:-}" ]; then
  missing="$missing R2_ACCESS_KEY_ID"
fi
if [ -z "${R2_SECRET_ACCESS_KEY:-}" ]; then
  missing="$missing R2_SECRET_ACCESS_KEY"
fi
if [ -z "${LITESTREAM_BUCKET:-}" ]; then
  missing="$missing R2_BUCKET/LITESTREAM_BUCKET"
fi
if [ -z "${LITESTREAM_ENDPOINT:-}" ]; then
  missing="$missing R2_ENDPOINT/LITESTREAM_ENDPOINT/R2_ACCOUNT_ID"
fi

use_litestream=0
if [ "$any_replica" -eq 1 ] || [ "$litestream_required" -eq 1 ]; then
  if [ -n "$missing" ]; then
    echo "clawctl-fly: Litestream replication is incomplete. Missing:${missing}." >&2
    echo "clawctl-fly: values are not printed. Unset every R2_* and LITESTREAM_* variable to run Hub without replication, or set LITESTREAM_REQUIRED=1 to require it." >&2
    exit 1
  fi
  use_litestream=1
fi

# Litestream splits -exec with shellwords and starts the program directly
# (no shell), so this must not begin with "exec". It forwards SIGTERM to the
# child and exits when the child exits. The paths contain no spaces.
case "${CLAWCTL_DB}${CLAWCTL_REPORT_STAMP}" in
  *[[:space:]\'\"]*)
    echo "clawctl-fly: CLAWCTL_DB and CLAWCTL_REPORT_STAMP must not contain whitespace or quotes" >&2
    exit 1
    ;;
esac
hub_cmd="/usr/local/bin/clawctl-hub --db ${CLAWCTL_DB} --report-stamp ${CLAWCTL_REPORT_STAMP}"
drop="setpriv --reuid=65532 --regid=65532 --clear-groups --inh-caps=-all"

if [ "$use_litestream" -eq 1 ]; then
  if [ -z "${LITESTREAM_PATH:-}" ]; then
    export LITESTREAM_PATH="clawctl/litestream/clawctl.sqlite"
  fi
  case $LITESTREAM_PATH in
    /* | .. | ../* | */.. | */../* | *[[:space:]]* | *..*)
      echo "clawctl-fly: LITESTREAM_PATH must be a relative object key without '..' or whitespace. The value is not printed." >&2
      exit 1
      ;;
  esac
  if [ ! -e "$CLAWCTL_DB" ]; then
    echo "clawctl-fly: database is absent; restoring from Litestream if a replica exists"
    $drop /usr/local/bin/litestream restore \
      -config /etc/litestream.yml \
      -if-db-not-exists \
      -if-replica-exists \
      "$CLAWCTL_DB"
  else
    echo "clawctl-fly: database already exists; not restoring over it"
  fi
  echo "clawctl-fly: Litestream will supervise Hub as uid 65532"
  unset TS_AUTHKEY || true
  trap - EXIT
  # shellcheck disable=SC2086
  exec $drop /usr/local/bin/litestream replicate \
    -config /etc/litestream.yml \
    -exec "$hub_cmd"
fi

echo "clawctl-fly: WARNING: R2_/LITESTREAM_ variables are unset. Hub will run without Litestream replication." >&2
echo "clawctl-fly: set R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY, R2_BUCKET, and R2_ENDPOINT or R2_ACCOUNT_ID to enable it." >&2
unset TS_AUTHKEY || true
trap - EXIT
# shellcheck disable=SC2086
exec $drop /usr/local/bin/clawctl-hub \
  --db "$CLAWCTL_DB" \
  --report-stamp "$CLAWCTL_REPORT_STAMP"
