#!/usr/bin/env bash
# Shared no-truncate lifecycle/writer locks for Hub rollout scripts.
# Caller must set -u and must have validated its canonical HOME first.

safe_lock_path_matches_fd() {
	local path="$1" fd="$2"
	local file_mode file_uid file_links file_inode fd_mode fd_uid fd_links fd_inode
	if ! IFS='|' read -r file_mode file_uid file_links file_inode < <(
		/usr/bin/stat -c '%f|%u|%h|%d:%i' -- "$path" 2>/dev/null
	) || ! IFS='|' read -r fd_mode fd_uid fd_links fd_inode < <(
		/usr/bin/stat -L -c '%f|%u|%h|%d:%i' -- "/proc/self/fd/$fd" 2>/dev/null
	); then
		return 1
	fi
	(( (16#$file_mode & 16#f000) == 16#8000 )) &&
		(( (16#$fd_mode & 16#f000) == 16#8000 )) &&
		[[ "$file_uid" == "$EUID" && "$fd_uid" == "$EUID" &&
			"$file_links" == 1 && "$fd_links" == 1 && "$file_inode" == "$fd_inode" ]]
}

acquire_safe_lock() {
	local path="$1" fd_variable="$2" label="$3" parent real_parent parent_mode parent_uid lock_fd
	local file_mode file_uid file_links file_inode
	parent="$(dirname "$path")"
	mkdir -p -- "$parent" || return 1
	if [[ ! -d "$parent" || -L "$parent" || ! -O "$parent" ]] ||
		! real_parent="$(/usr/bin/realpath -e -- "$parent")" || [[ "$real_parent" != "$parent" ]]; then
		echo "✗ $label parent 不是目前使用者持有的 canonical non-symlink directory：$parent" >&2
		return 1
	fi
	if ! IFS='|' read -r parent_mode parent_uid < <(
		/usr/bin/stat -c '%f|%u' -- "$parent" 2>/dev/null
	) || ! (( (16#$parent_mode & 16#f000) == 16#4000 )) || [[ "$parent_uid" != "$EUID" ]]; then
		echo "✗ 無法確認 $label parent 的 type/owner：$parent" >&2
		return 1
	fi
	if (( (16#$parent_mode & 16#3f) != 0 )); then
		# This is the Hub's private DB state directory. Once its canonical path and
		# owner are proven, converge old installer-created 0775/0755 modes to 0700
		# before creating or opening a lifecycle lock inside it.
		if ! chmod 0700 -- "$parent" ||
			! IFS='|' read -r parent_mode parent_uid < <(
				/usr/bin/stat -c '%f|%u' -- "$parent" 2>/dev/null
			) || (( (16#$parent_mode & 16#f000) != 16#4000 )) ||
			(( (16#$parent_mode & 16#3f) != 0 )) || [[ "$parent_uid" != "$EUID" ]]; then
			echo "✗ $label parent 無法安全收斂為 private directory：$parent" >&2
			return 1
		fi
	fi

	# Create only when absent. noclobber makes an existing path (including a
	# symlink inserted in the check/create race) a failure, never a truncation.
	if [[ ! -e "$path" && ! -L "$path" ]]; then
		(umask 077; set -o noclobber; : >"$path") 2>/dev/null || true
	fi
	if ! IFS='|' read -r file_mode file_uid file_links file_inode < <(
		/usr/bin/stat -c '%f|%u|%h|%d:%i' -- "$path" 2>/dev/null
	); then
		echo "✗ 無法 lstat $label；沒有開啟或修改目標：$path" >&2
		return 1
	fi
	if ! (( (16#$file_mode & 16#f000) == 16#8000 )) ||
		[[ "$file_uid" != "$EUID" || "$file_links" != 1 ]]; then
		echo "✗ $label 必須是目前使用者持有、link count=1 的 regular file；拒絕 symlink/hardlink：$path" >&2
		return 1
	fi

	# <> opens without O_TRUNC. Compare the opened object with a second lstat of
	# the path before chmod/flock, so a path swap cannot redirect either action.
	if ! exec {lock_fd}<>"$path"; then
		echo "✗ 無法以 no-truncate 模式開啟 $label：$path" >&2
		return 1
	fi
	if ! safe_lock_path_matches_fd "$path" "$lock_fd"; then
		echo "✗ $label path/fd identity 在開啟時改變；拒絕 chmod/flock：$path" >&2
		exec {lock_fd}>&-
		return 1
	fi
	if ! chmod 0600 -- "/proc/self/fd/$lock_fd"; then
		echo "✗ 無法把已驗證的 $label fd 設為 0600：$path" >&2
		exec {lock_fd}>&-
		return 1
	fi
	if ! /usr/bin/flock -n "$lock_fd"; then
		exec {lock_fd}>&-
		return 75
	fi
	# A second identity check after flock closes the pathname-swap window: a
	# second process must not be able to lock a replacement inode at this path.
	if ! safe_lock_path_matches_fd "$path" "$lock_fd"; then
		echo "✗ $label path/fd identity 在 flock 後改變；拒絕繼續：$path" >&2
		exec {lock_fd}>&-
		return 1
	fi
	printf -v "$fd_variable" '%s' "$lock_fd"
}

acquire_upgrade_lock() {
	acquire_safe_lock "$1" UPGRADE_LOCK_FD "upgrade lock"
}

acquire_writer_lock() {
	acquire_safe_lock "$1" WRITER_LOCK_FD "writer lock"
}

release_writer_lock() {
	if [[ -n "${WRITER_LOCK_FD:-}" ]]; then
		exec {WRITER_LOCK_FD}>&-
		unset WRITER_LOCK_FD
	fi
}
