#!/usr/bin/env bash
# Atomically stage the checked-in clawctl-hub user unit and reload only the
# systemd manager. This deliberately does not stop, start or restart the Hub.
# upgrade-hub.sh requires this exact on-disk/loaded contract before rollout.

set -euo pipefail

cd "$(dirname "$0")/.."

if [[ $EUID -eq 0 ]]; then
	echo "⚠ Do not run this as root — Hub is a user unit" >&2
	exit 2
fi

canonical_home() {
	local rows account_name passwd_marker account_uid account_gid gecos account_home account_shell resolved
	mapfile -t rows < <(/usr/bin/getent passwd "$EUID")
	[[ ${#rows[@]} -eq 1 ]] || return 1
	IFS=: read -r account_name passwd_marker account_uid account_gid gecos account_home account_shell <<<"${rows[0]}"
	[[ "$account_uid" == "$EUID" && -n "$account_home" && "$account_home" == /* &&
		! -L "$account_home" && -d "$account_home" && "$HOME" == "$account_home" ]] || return 1
	resolved="$(/usr/bin/realpath -e -- "$HOME")" || return 1
	[[ "$resolved" == "$account_home" ]]
}

if ! canonical_home; then
	echo "✗ caller HOME does not match canonical passwd home for EUID=$EUID; no lock created and systemctl not called" >&2
	exit 1
fi

UNIT_NAME=clawctl-hub.service
UNIT="$HOME/.config/systemd/user/$UNIT_NAME"
DESIRED="$PWD/ops/clawctl-hub.service"
UPGRADE_LOCK="$HOME/.local/share/clawctl/clawctl.sqlite.upgrade.lock"
MAINTENANCE_MARKER="$HOME/.local/share/clawctl/clawctl.sqlite.upgrade-maintenance"

source "$PWD/ops/safe-upgrade-lock.sh"
lock_rc=0
acquire_upgrade_lock "$UPGRADE_LOCK" || lock_rc=$?
if [[ $lock_rc -eq 75 ]]; then
	echo "✗ Another clawctl-hub upgrade/rollback/unit staging is in progress; unit unchanged" >&2
	exit 1
elif [[ $lock_rc -ne 0 ]]; then
	exit 1
fi
if [[ -e "$MAINTENANCE_MARKER" || -L "$MAINTENANCE_MARKER" ]]; then
	echo "✗ Upgrade maintenance marker exists; complete manual recovery first, unit unchanged" >&2
	exit 1
fi
if [[ ! -f "$DESIRED" || -L "$DESIRED" || ! -f "$UNIT" || -L "$UNIT" || ! -O "$UNIT" ]]; then
	echo "✗ desired/installed unit must be non-symlink regular files, and installed unit owned by current user" >&2
	exit 1
fi

read_loaded_contract() {
	local raw name value property
	declare -gA LOADED=()
	raw="$(/usr/bin/timeout --kill-after=2s 10s /usr/bin/systemctl --user show "$UNIT_NAME" \
		-p LoadState -p FragmentPath -p DropInPaths -p NeedDaemonReload -p ActiveState \
		-p MainPID -p NRestarts -p InvocationID 2>/dev/null)" || return 1
	while IFS='=' read -r name value; do
		case "$name" in
			LoadState|FragmentPath|DropInPaths|NeedDaemonReload|ActiveState|MainPID|NRestarts|InvocationID) ;;
			*) return 1 ;;
		esac
		[[ ! -v "LOADED[$name]" ]] || return 1
		LOADED[$name]="$value"
	done <<<"$raw"
	for property in LoadState FragmentPath DropInPaths NeedDaemonReload ActiveState MainPID NRestarts InvocationID; do
		[[ -v "LOADED[$property]" ]] || return 1
	done
}

if ! read_loaded_contract; then
	echo "✗ Cannot read unique loaded unit contract; unit unchanged" >&2
	exit 1
fi
if [[ "${LOADED[LoadState]}" != loaded || "${LOADED[FragmentPath]}" != "$UNIT" || -n "${LOADED[DropInPaths]}" ||
	( "${LOADED[NeedDaemonReload]}" != no && "${LOADED[NeedDaemonReload]}" != yes ) ||
	"${LOADED[ActiveState]}" != active ||
	! "${LOADED[MainPID]}" =~ ^[1-9][0-9]*$ ||
	! "${LOADED[NRestarts]}" =~ ^[0-9]+$ ||
	! "${LOADED[InvocationID]}" =~ ^[0-9a-f]{32}$ ]]; then
	echo "✗ Hub unit is not a single, loaded and active installer-managed unit; unit unchanged" >&2
	exit 1
fi

if ! DESIRED_BLOB="$(git rev-parse --verify HEAD:ops/clawctl-hub.service 2>/dev/null)" ||
	[[ "$(git cat-file -t "$DESIRED_BLOB" 2>/dev/null)" != blob ]]; then
	echo "✗ HEAD has no identifiable checked-in unit blob; unit unchanged" >&2
	exit 1
fi

desired_blob_equals_file() {
	local file="$1"
	git cat-file blob "$DESIRED_BLOB" 2>/dev/null | cmp -s -- "$file" -
}

desired_unit_matches_head() {
	desired_blob_equals_file "$DESIRED"
}

if ! desired_unit_matches_head; then
	echo "✗ Checked-in unit is not yet committed; refusing to place settings that cannot be identified by revision onto live disk" >&2
	exit 1
fi

# Do not overwrite an unexplained local customization. The currently installed
# file must be either desired already or byte-for-byte equal to a tracked
# historical revision of this repository's unit.
installed_unit_is_managed() {
	local revision
	if desired_blob_equals_file "$UNIT"; then
		return 0
	fi
	while IFS= read -r revision; do
		if git show "$revision:ops/clawctl-hub.service" 2>/dev/null | cmp -s -- "$UNIT" -; then
			return 0
		fi
	done < <(git log --format=%H -- ops/clawctl-hub.service)
	return 1
}

if ! installed_unit_is_managed; then
	echo "✗ installed unit is not any tracked version in repo; refusing to overwrite possible local customization" >&2
	exit 1
fi

before_pid="${LOADED[MainPID]}"
before_restarts="${LOADED[NRestarts]}"
before_invocation="${LOADED[InvocationID]}"

stage_checked_in_unit() {
	local unit_dir stage_tmp
	unit_dir="$(dirname "$UNIT")"
	stage_tmp="$(mktemp "$unit_dir/.clawctl-hub.service.new.XXXXXX")" || return 1
	# Before mv, interruption only leaves a disposable hidden temp. After mv the
	# temp path no longer exists and the desired unit remains monotonically on
	# disk; rerunning this script completes a pending daemon-reload.
	trap 'rm -f -- "$stage_tmp"; exit 129' HUP
	trap 'rm -f -- "$stage_tmp"; exit 130' INT
	trap 'rm -f -- "$stage_tmp"; exit 143' TERM
	# Read the content-addressed HEAD blob directly into the private temp. The
	# mutable worktree file was checked for operator mistakes above, but is never
	# a rollout source after that check, closing the verify/copy TOCTOU window.
	if ! git cat-file blob "$DESIRED_BLOB" >"$stage_tmp" 2>/dev/null ||
		! chmod 0644 -- "$stage_tmp" || ! mv -- "$stage_tmp" "$UNIT"; then
		rm -f -- "$stage_tmp"
		trap - HUP INT TERM
		echo "✗ Unit atomic staging failed; systemd reload not requested" >&2
		return 1
	fi
	trap - HUP INT TERM
	if ! /usr/bin/timeout --kill-after=2s 10s /usr/bin/systemctl --user daemon-reload; then
		echo "✗ desired unit safely left on disk, but daemon-reload not completed; Hub not restarted. Rerun this script to converge" >&2
		return 1
	fi
	if ! read_loaded_contract ||
		[[ "${LOADED[LoadState]}" != loaded || "${LOADED[FragmentPath]}" != "$UNIT" || -n "${LOADED[DropInPaths]}" ||
			"${LOADED[NeedDaemonReload]}" != no || "${LOADED[ActiveState]}" != active ||
			! "${LOADED[MainPID]}" =~ ^[1-9][0-9]*$ || ! "${LOADED[NRestarts]}" =~ ^[0-9]+$ ||
			! "${LOADED[InvocationID]}" =~ ^[0-9a-f]{32}$ ||
			"${LOADED[MainPID]}" != "$before_pid" || "${LOADED[NRestarts]}" != "$before_restarts" ||
			"${LOADED[InvocationID]}" != "$before_invocation" ]] ||
		! desired_blob_equals_file "$UNIT"; then
		echo "✗ desired unit left on disk, but loaded contract not converged; Hub was not restarted by this script. Please rerun and check systemctl show" >&2
		return 1
	fi
	echo "✓ Atomically updated and daemon-reloaded $UNIT; PID=$before_pid, NRestarts=$before_restarts, no restart"
}

stage_checked_in_unit
