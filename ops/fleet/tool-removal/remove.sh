#!/usr/bin/env bash
set -euo pipefail
name=''; manifest=''; apply=false; root_only=false
while (($#)); do
 case $1 in --name) name=$2; shift 2;; --manifest) manifest=$2; shift 2;; --apply) apply=true; shift;; --root-only) root_only=true; shift;; *) exit 2;; esac
done
[[ $name =~ ^[a-zA-Z0-9_-]+$ && -f $manifest && ! -L $manifest ]] || exit 2
dir=$(cd "$(dirname "$0")" && pwd)
USER_UNITS=''; SYSTEM_UNITS=''; PACKAGES=''; PATHS=''; HOOK_PATTERN=''; PORT=''; ALLOW_PACKAGES=''
declare -A seen=()
while IFS= read -r line || [[ -n $line ]]; do
 [[ -n $line && $line != \#* ]] || continue
 [[ $line == *=* ]] || exit 2
 key=${line%%=*}; value=${line#*=}
 [[ $key =~ ^(USER_UNITS|SYSTEM_UNITS|PACKAGES|PATHS|HOOK_PATTERN|PORT|ALLOW_PACKAGES)$ && ! ${seen[$key]+yes} ]] || exit 2
 seen[$key]=1; printf -v "$key" '%s' "$value"
done < "$manifest"
[[ -z $PORT || $PORT =~ ^[0-9]+$ ]] || exit 2
[[ -z $PORT ]] || ((PORT>0 && PORT<65536))
for unit in $USER_UNITS $SYSTEM_UNITS; do [[ $unit =~ ^[a-zA-Z0-9_][a-zA-Z0-9_@.-]*\.(service|timer)$ ]] || exit 2; done
for package in $PACKAGES; do [[ $package =~ ^[a-zA-Z0-9][a-zA-Z0-9+.:_-]*$ ]] || exit 2; done
for package in $PACKAGES; do
 case $package in orca|screen|code)
  allowed=false
  for approved in $ALLOW_PACKAGES; do [[ $approved != "$package" ]] || allowed=true; done
  if ! $allowed; then printf 'refuse ambiguous package: %s (requires ALLOW_PACKAGES)\n' "$package"; exit 2; fi;;
 esac
done
# Canonicalize before authorizing, reject symlinks in any path component.
for path in $PATHS; do
 [[ $path != *..* && $path != "$HOME" && $path != /opt && $path != /usr/local && $path != /etc/systemd ]] || exit 2
 case $path in "$HOME"/*|/opt/*|/usr/local/*|/etc/systemd/*) ;; *) exit 2;; esac
 [[ $(realpath -m -- "$path") == "$path" ]] || exit 2
 parent=$path
 while [[ $parent != / ]]; do [[ ! -L $parent ]] || exit 2; parent=$(dirname "$parent"); done
 if [[ -d $path ]] && find "$path" -name .git -print -quit | grep -q .; then echo 'refuse project data'; exit 2; fi
done
configs=()
for file in "$HOME/.claude/settings.json" "$HOME/.codex/hooks.json" "$HOME/.gemini/settings.json"; do [[ ! -f $file ]] || configs+=("$file"); done
if [[ -n $HOOK_PATTERN && ${#configs[@]} -gt 0 ]]; then
 python3 "$dir/strip-hooks.py" --pattern "$HOOK_PATTERN" "${configs[@]}"
fi
printf 'plan: exact units=%s packages=%s paths=%s port=%s\n' "$USER_UNITS $SYSTEM_UNITS" "$PACKAGES" "$PATHS" "$PORT"
if ! $apply; then exit 0; fi
is_root=false
[[ $(id -u) != 0 ]] || is_root=true
if $root_only && ! $is_root; then echo 'root-only requires root'; exit 2; fi
# Delegate the complete user phase to the owner of HOME under sudo.
uid=$(stat -c %u "$HOME")
if $is_root && [[ $uid != 0 ]]; then
 user=$(getent passwd "$uid" | cut -d: -f1)
 [[ -n $user ]] || exit 1
 runuser -u "$user" -- env FLEET_REMOVE_DELEGATED=1 HOME="$HOME" XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" bash "$dir/remove.sh" --name "$name" --manifest "$(realpath "$manifest")" --apply
fi
user_part=false
if ! $is_root; then user_part=true; fi
umask 077
backup_dir=$(mktemp -d "$HOME/$name-removal-backup-$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")
items=()
for path in $PATHS; do
 if $user_part; then [[ $path == "$HOME"/* ]] || continue; else [[ $path != "$HOME"/* ]] || continue; fi
 [[ ! -e $path ]] || items+=("$path")
done
if $user_part; then
 for unit in $USER_UNITS; do [[ ! -f $HOME/.config/systemd/user/$unit ]] || items+=("$HOME/.config/systemd/user/$unit"); done
 items+=("${configs[@]}")
else
 for unit in $SYSTEM_UNITS; do [[ ! -f /etc/systemd/system/$unit ]] || items+=("/etc/systemd/system/$unit"); done
 for package in $PACKAGES; do
  if command -v dpkg-query >/dev/null; then dpkg-query -L "$package" > "$backup_dir/package-$package" 2>/dev/null || true
  elif command -v rpm >/dev/null; then rpm -ql "$package" > "$backup_dir/package-$package" 2>/dev/null || true; fi
 done
 for family in iptables ip6tables; do
  if [[ -n $PORT ]] && command -v "$family" >/dev/null; then command -v "$family-save" >/dev/null || exit 1; fi
  if command -v "$family-save" >/dev/null; then "$family-save" > "$backup_dir/$family.rules"; fi
 done
fi
printf 'private removal backup\n' > "$backup_dir/receipt"
backup="$backup_dir/items.tar"
items+=("$backup_dir/"*)
tar -cf "$backup" -- "${items[@]}" 2>/dev/null
chmod 600 "$backup"
[[ -s $backup ]] || exit 1
printf 'backup: %s\n' "$backup"
# Use the owner of HOME for user units even when the root step runs via sudo.
userctl() {
 local uid user
 uid=$(stat -c %u "$HOME")
 if [[ $EUID == 0 && $uid != 0 ]]; then
  user=$(getent passwd "$uid" | cut -d: -f1)
  [[ -n $user ]] || return 1
  runuser -u "$user" -- env XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" systemctl --user "$@"
 else systemctl --user "$@"; fi
}
# Stop scoped units only. Never kill processes based on a broad name match.
if $user_part; then
for unit in $USER_UNITS; do
 if [[ $(userctl show "$unit" --property=LoadState --value) != not-found ]]; then
  userctl stop "$unit"; userctl disable "$unit"
  userctl kill --kill-whom=all "$unit" || true
 fi
 rm -f -- "$HOME/.config/systemd/user/$unit"
done
if [[ -n $HOOK_PATTERN && ${#configs[@]} -gt 0 ]]; then python3 "$dir/strip-hooks.py" --apply --pattern "$HOOK_PATTERN" "${configs[@]}"; fi
for path in $PATHS; do [[ $path != "$HOME"/* ]] || rm -rf -- "$path"; done
[[ -z $USER_UNITS ]] || userctl daemon-reload
printf -v cmd '%q ' bash "$dir/remove.sh" --name "$name" --manifest "$(realpath "$manifest")" --apply --root-only
printf -v home_assignment 'HOME=%q ' "$HOME"
cmd="$home_assignment$cmd"
if [[ ${FLEET_REMOVE_DELEGATED:-0} == 1 ]]; then printf 'User part finished.\n'; exit 0; fi
printf "sudo bash -c '%s'\n" "${cmd//\'/\'\\\'\'}"
printf 'User part finished; root part pending.\n'
exit 0
fi
for unit in $SYSTEM_UNITS; do
 if [[ $(systemctl show "$unit" --property=LoadState --value) != not-found ]]; then
  systemctl stop "$unit"; systemctl disable "$unit"
  systemctl kill --kill-whom=all "$unit" || true
 fi
 rm -f -- "/etc/systemd/system/$unit"
done
for package in $PACKAGES; do
 if command -v dpkg-query >/dev/null; then
  installed=$(dpkg-query -W -f='${db:Status-Status}' "$package" 2>/dev/null || true)
  [[ $installed != installed ]] || apt-get purge -y -- "$package" >/dev/null 2>&1
 elif command -v rpm >/dev/null; then
  if rpm -q -- "$package" >/dev/null 2>&1; then rpm -e -- "$package" >/dev/null 2>&1; fi
 else echo 'no supported package manager'; exit 1; fi
done
for path in $PATHS; do [[ $path == "$HOME"/* ]] || rm -rf -- "$path"; done
[[ -z $SYSTEM_UNITS ]] || systemctl daemon-reload
removed=0
if [[ -n $PORT ]]; then
 for family in iptables ip6tables; do
  command -v "$family" >/dev/null || continue
  rules=$("$family" -S INPUT)
  while IFS= read -r rule; do
   spec=()
   # iptables -S quotes comments; preserve each argument without eval.
   mapfile -d '' -t spec < <(printf '%s' "$rule" | python3 -c 'import shlex,sys; sys.stdout.buffer.write(b"".join(s.encode()+b"\0" for s in shlex.split(sys.stdin.read())))')
   [[ ${spec[0]:-} == -A && ${spec[1]:-} == INPUT ]] || continue
   for ((i=2;i<${#spec[@]}-1;i++)); do
    if [[ ${spec[i]} == --dport && ${spec[i+1]} == "$PORT" ]]; then
     spec[0]=-D
     "$family" "${spec[@]}"
     removed=$((removed+1)); break
    fi
   done
  done <<< "$rules"
 done
fi
printf 'Firewall rules removed: %s\n' "$removed"
printf 'Root part finished. Run verify; inspect dependencies and unscoped processes manually.\n'
