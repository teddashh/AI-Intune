#!/bin/sh
# Prepare the named volume clawctl-data before clawctl-hub starts.
#
# Hub runs as distroless nonroot (uid/gid 65532). Two startup checks fail
# when a new Docker volume is root:root and mode 0755:
#
#   ledgerlock.AcquireWriter refuses the SQLite parent unless that uid owns
#   it and the mode has no group or other bits (0700). See
#   internal/ledgerlock.validatePrivateParent.
#   web.SetAgentBundles loads
#   <dir of --db>/agent-bootstrap/<main.version>/ and requires that directory
#   and the Linux amd64/arm64 archives to be owned by the same uid, with the
#   directory not group/other writable. There is no flag for another path.
#   cmd/clawctl-hub serve() joins filepath.Dir(db) with "agent-bootstrap" and
#   the baked-in version.
#
# This script is the entrypoint of the hub-init image. It is idempotent.
# An existing release directory for this version is not replaced (same rule
# as ops/publish-agent-bundles.sh).
set -eu

hub_uid=65532
hub_gid=65532
data=/var/lib/clawctl
version=${CLAWCTL_VERSION:-}

case $version in
  "" )
    echo "hub-data-init: CLAWCTL_VERSION is empty" >&2
    exit 1
    ;;
esac
case $version in
  *[!A-Za-z0-9._+-]* )
    echo "hub-data-init: CLAWCTL_VERSION contains unsupported characters" >&2
    exit 1
    ;;
esac
case $version in
  [A-Za-z0-9]* ) ;;
  * )
    echo "hub-data-init: CLAWCTL_VERSION must start with a letter or digit" >&2
    exit 1
    ;;
esac
if [ "${#version}" -gt 128 ]; then
  echo "hub-data-init: CLAWCTL_VERSION is too long" >&2
  exit 1
fi

if [ -L "$data" ]; then
  echo "hub-data-init: $data is a symlink" >&2
  exit 1
fi
mkdir -p "$data"

seed=/opt/clawctl-seed/agent-bootstrap/$version
dest_root=$data/agent-bootstrap
dest=$dest_root/$version
amd64=clawctl-agent-bootstrap-linux-amd64.tar.gz
arm64=clawctl-agent-bootstrap-linux-arm64.tar.gz
sums=SHA256SUMS

if [ -L "$seed" ] || [ ! -d "$seed" ]; then
  echo "hub-data-init: image seed missing: $seed" >&2
  exit 1
fi
for name in "$amd64" "$arm64" "$sums"; do
  if [ -L "$seed/$name" ] || [ ! -f "$seed/$name" ]; then
    echo "hub-data-init: image seed missing: $seed/$name" >&2
    exit 1
  fi
done

if [ -L "$dest_root" ] || [ -L "$dest" ]; then
  echo "hub-data-init: refusing symlink in the agent bootstrap path" >&2
  exit 1
fi
mkdir -p "$dest_root"

if [ -d "$dest" ]; then
  for name in "$amd64" "$arm64" "$sums"; do
    if [ -L "$dest/$name" ] || [ ! -f "$dest/$name" ]; then
      echo "hub-data-init: $dest is incomplete; remove that directory and start again" >&2
      exit 1
    fi
  done
  echo "hub-data-init: agent bootstrap $version already present; not replacing"
else
  # Drop a crashed previous attempt, then publish by rename.
  find "$dest_root" -mindepth 1 -maxdepth 1 -type d -name ".seed.$version.*" -exec rm -rf {} +
  stage=$(mktemp -d "$dest_root/.seed.$version.XXXXXX")
  cleanup() {
    if [ -n "${stage:-}" ] && [ -d "$stage" ]; then
      rm -rf "$stage"
    fi
  }
  trap cleanup EXIT
  cp -p "$seed/$amd64" "$seed/$arm64" "$seed/$sums" "$stage/"
  chmod 0700 "$stage"
  chmod 0644 "$stage/$amd64" "$stage/$arm64" "$stage/$sums"
  chown "$hub_uid:$hub_gid" "$stage" "$stage/$amd64" "$stage/$arm64" "$stage/$sums"
  mv "$stage" "$dest"
  stage=
  trap - EXIT
  echo "hub-data-init: seeded agent bootstrap $version"
fi

# New named volumes are root-owned. Own every real file and directory in the
# volume, but do not follow symlinks (find does not descend through them).
find "$data" -xdev \( -type d -o -type f \) -exec chown "$hub_uid:$hub_gid" {} +
chmod 0700 "$data" "$dest_root" "$dest"
chmod 0644 "$dest/$amd64" "$dest/$arm64" "$dest/$sums"

db=$data/clawctl.sqlite
for path in \
  "$db" \
  "$db-wal" \
  "$db-shm" \
  "$db.writer.lock" \
  "$db.upgrade.lock" \
  "$data/last-report.stamp"
do
  if [ -L "$path" ]; then
    echo "hub-data-init: refusing symlink $path" >&2
    exit 1
  fi
done

echo "hub-data-init: $data is ${hub_uid}:${hub_gid} mode 0700"
