#!/usr/bin/env bash
# Atomically stage the checked-in clawctl-hub user unit and reload only the
# systemd manager. This deliberately does not stop, start or restart the Hub.
# upgrade-hub.sh requires this exact on-disk/loaded contract before rollout.

set -euo pipefail

cd "$(dirname "$0")/.."

if [[ $EUID -eq 0 ]]; then
	echo "⚠ 不要用 root 跑這支 —— Hub 是 user unit。" >&2
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
	echo "✗ caller HOME 與 EUID=$EUID 的 canonical passwd home 不一致；尚未建立 lock 或呼叫 systemctl。" >&2
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
	echo "✗ 另一支 clawctl-hub upgrade/rollback/unit staging 正在進行；沒有改 unit。" >&2
	exit 1
elif [[ $lock_rc -ne 0 ]]; then
	exit 1
fi
if [[ -e "$MAINTENANCE_MARKER" || -L "$MAINTENANCE_MARKER" ]]; then
	echo "✗ 有 upgrade maintenance marker；先完成人工復原，沒有改 unit。" >&2
	exit 1
fi
if [[ ! -f "$DESIRED" || -L "$DESIRED" || ! -f "$UNIT" || -L "$UNIT" || ! -O "$UNIT" ]]; then
	echo "✗ desired/installed unit 必須是非 symlink regular file，且 installed unit 由目前使用者持有。" >&2
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
	echo "✗ 讀不到唯一的 loaded unit contract；沒有改 unit。" >&2
	exit 1
fi
if [[ "${LOADED[LoadState]}" != loaded || "${LOADED[FragmentPath]}" != "$UNIT" || -n "${LOADED[DropInPaths]}" ||
	( "${LOADED[NeedDaemonReload]}" != no && "${LOADED[NeedDaemonReload]}" != yes ) ||
	"${LOADED[ActiveState]}" != active ||
	! "${LOADED[MainPID]}" =~ ^[1-9][0-9]*$ ||
	! "${LOADED[NRestarts]}" =~ ^[0-9]+$ ||
	! "${LOADED[InvocationID]}" =~ ^[0-9a-f]{32}$ ]]; then
	echo "✗ Hub unit 不是單一、已載入且 active 的 installer-managed unit；沒有改 unit。" >&2
	exit 1
fi

if ! DESIRED_BLOB="$(git rev-parse --verify HEAD:ops/clawctl-hub.service 2>/dev/null)" ||
	[[ "$(git cat-file -t "$DESIRED_BLOB" 2>/dev/null)" != blob ]]; then
	echo "✗ HEAD 沒有可識別的 checked-in unit blob；沒有改 unit。" >&2
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
	echo "✗ checked-in unit 尚未 commit；拒絕把無法以 revision 識別的設定放到 live disk。" >&2
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
	echo "✗ installed unit 不是 repo 內任何受控版本；拒絕覆蓋可能的本機 customization。" >&2
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
		echo "✗ unit atomic staging 失敗；尚未要求 systemd reload。" >&2
		return 1
	fi
	trap - HUP INT TERM
	if ! /usr/bin/timeout --kill-after=2s 10s /usr/bin/systemctl --user daemon-reload; then
		echo "✗ desired unit 已安全留在磁碟，但 daemon-reload 沒完成；Hub 沒有 restart。重跑本腳本即可收斂。" >&2
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
		echo "✗ desired unit 已留在磁碟，但 loaded contract 尚未收斂；Hub 沒有由本腳本 restart。請重跑並檢查 systemctl show。" >&2
		return 1
	fi
	echo "✓ 已原子更新並 daemon-reload $UNIT；PID=$before_pid、NRestarts=$before_restarts，沒有 restart。"
}

stage_checked_in_unit
