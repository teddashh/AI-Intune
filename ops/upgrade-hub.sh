#!/usr/bin/env bash
# 把 clawctl-hub 換版到目前 HEAD。Hub 只有一台（samplehub1，本機）。
#
# 用法：
#   ./upgrade-hub.sh
#   ./upgrade-hub.sh --allow-dirty
#   ./upgrade-hub.sh --rollback          # 換回上一版（.prev）
#
# ⚠⚠ 這支腳本跟 ops/upgrade-agent.sh 是同一個事故的兩半。
#
# agent 那支寫出來之後，Hub 換版還是手抄的 —— 而「手抄那份步驟」正是
# 2026-09-03 六小時雙 agent 事故的入口（docs/PHASE1.md §5.17）。
# 同一個理由套在 Hub 上更嚴重：agent 掛了少一台的資料，
# **Hub 掛了你就什麼都看不到了**，包括「Hub 掛了」這件事本身。
#
# Hub 跟 agent 不一樣的三件事，這支腳本大部分的長度都是為了它們：
#
#   1. **它有一個資料庫。** 換版可能跑 migration。所以換之前先由 SQLite
#      online backup 做一致 snapshot；raw cp 一個開著 WAL 的 DB 會拿到
#      一份撕裂的檔案，而那份檔案看起來完全正常。
#   2. **它起不來的時候沒有東西會告訴你。** agent 起不來，Hub 上那台會轉紅；
#      Hub 起不來，畫面整個不見，而「連不上」跟「你的網路有問題」長得一樣。
#      所以這支腳本自己驗，驗不過就**自動換回上一版**。
#   3. **它自己說「我還活著」不算數。** /healthz 回 alive 只證明有一個
#      process 在聽那個 port，不證明它是新的那個、也不證明畫面 render 得出來。
#      所以下面問的是六個不同的東西，見「六道驗收」。

set -euo pipefail

cd "$(dirname "$0")/.."

ALLOW_DIRTY=0 ; ROLLBACK=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --allow-dirty) ALLOW_DIRTY=1; shift ;;
    --rollback)    ROLLBACK=1; shift ;;
    -h|--help)     sed -n '2,8p' "$0"; exit 0 ;;
    *)             echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ $EUID -eq 0 ]]; then
  echo "⚠ Do not run this script as root — Hub is a user unit" >&2
  exit 2
fi

verify_canonical_home() {
	local rows account_name passwd_marker account_uid account_gid gecos account_home account_shell resolved
	mapfile -t rows < <(/usr/bin/getent passwd "$EUID")
	if [[ ${#rows[@]} -ne 1 ]]; then
		echo "✗ Cannot obtain unique passwd home for EUID=$EUID; refusing action on systemd user unit" >&2
		return 1
	fi
	IFS=: read -r account_name passwd_marker account_uid account_gid gecos account_home account_shell <<<"${rows[0]}"
	if [[ "$account_uid" != "$EUID" || -z "$account_home" || "$account_home" != /* || -L "$account_home" || ! -d "$account_home" ]]; then
		echo "✗ passwd home is unavailable or not a non-symlink absolute directory" >&2
		return 1
	fi
	if [[ "$HOME" != "$account_home" ]]; then
		echo "✗ Caller HOME=$HOME does not match canonical home for EUID=$EUID; no lock created or systemctl called" >&2
		return 1
	fi
	if ! resolved="$(/usr/bin/realpath -e -- "$HOME")" || [[ "$resolved" != "$account_home" ]]; then
		echo "✗ Physical path of HOME does not match passwd home; no lock created or systemctl called" >&2
		return 1
	fi
}

# Resolve no lifecycle path from caller-controlled HOME until it is tied to
# this EUID's passwd entry. In particular, a fake HOME must not preflight one
# unit/env file and then stop the real user's systemd service.
if ! verify_canonical_home; then
	exit 1
fi

resolve_operator_config_paths() {
	local configured resolved
	configured="${XDG_CONFIG_HOME:-$HOME/.config}"
	if [[ "$configured" != /* ]] ||
		! resolved="$(/usr/bin/realpath -m -- "$configured")" ||
		[[ "$resolved" != "$configured" ]]; then
		echo "✗ XDG operator config root must be a canonical absolute path: $configured" >&2
		return 1
	fi
	OPERATOR_CONFIG_HOME="$configured"
	OPERATOR_CONFIG_DIR="$OPERATOR_CONFIG_HOME/clawctl"
	OPERATOR_CONFIG="$OPERATOR_CONFIG_DIR/operator.json"
}

if ! resolve_operator_config_paths; then
	exit 1
fi

BIN="$HOME/.local/bin/clawctl-hub"
UNIT=clawctl-hub.service
DB="$HOME/.local/share/clawctl/clawctl.sqlite"
STATE_DIR="$(dirname -- "$DB")"
BACKUPS="$HOME/.local/share/clawctl/backups"
MAINTENANCE_MARKER="$DB.upgrade-maintenance"
UPGRADE_LOCK="$DB.upgrade.lock"
WRITER_LOCK="$DB.writer.lock"

# 整支 lifecycle 只能有一個 owner。marker 封的是 Hub/CLI writes；這把 flock
# 封的是兩支 upgrade-hub 同時改 BIN.new/BIN.prev、以及其中一支誤刪另一支
# marker 的 script-level race。fd 隨 process exit 自動釋放；lock 檔可留下。
source "$PWD/ops/safe-upgrade-lock.sh"
lock_rc=0
acquire_upgrade_lock "$UPGRADE_LOCK" || lock_rc=$?
if [[ $lock_rc -eq 75 ]]; then
  echo "✗ Another clawctl-hub upgrade/rollback is in progress; binary and DB left untouched" >&2
  exit 1
elif [[ $lock_rc -ne 0 ]]; then
	exit 1
fi

# Probe copies are the only executable form of a dormant .prev/.failed binary.
# Each copy is opened while 0600, unlinked, and only then made executable via
# its private fd. SIGKILL/power loss therefore closes the last reference rather
# than leaving a callable pathname beside an uncertain database.
STAGED_BINARY_PROBE_FDS=()
cleanup_staged_binary_probes() {
	local probe_fd
	for probe_fd in "${STAGED_BINARY_PROBE_FDS[@]}"; do
		exec {probe_fd}<&- 2>/dev/null || true
	done
	STAGED_BINARY_PROBE_FDS=()
}

durably_sync_path() {
	local path="$1" label="$2"
	if ! /usr/bin/sync -f -- "$path"; then
		echo "✗ Cannot durably sync $label: $path" >&2
		return 1
	fi
}

make_dormant_binary_non_executable() {
	local path="$1" label="${2:-dormant Hub binary}" mode uid links
	if [[ ! -e "$path" && ! -L "$path" ]]; then
		return 0
	fi
	if [[ -L "$path" || ! -f "$path" || ! -O "$path" ]] ||
		! IFS='|' read -r mode uid links < <(/usr/bin/stat -c '%f|%u|%h' -- "$path" 2>/dev/null) ||
		! (( (16#$mode & 16#f000) == 16#8000 )) || [[ "$uid" != "$EUID" || "$links" != 1 ]]; then
		echo "✗ $label must be a non-symlink regular file owned by current user with link count=1: $path" >&2
		return 1
	fi
	if ! chmod 0600 -- "$path"; then
		echo "✗ Cannot deactivate $label: $path" >&2
		return 1
	fi
}

cleanup_upgrade_runtime() {
	cleanup_staged_binary_probes
	# Before activation, .new is only a preflight vehicle.  Any exit path must
	# leave it dormant even when the failure happened before maintenance begin.
	if [[ -n "${BIN:-}" && ( -e "$BIN.new" || -L "$BIN.new" ) ]]; then
		make_dormant_binary_non_executable "$BIN.new" "unactivated candidate binary" || true
	fi
}
trap cleanup_upgrade_runtime EXIT

stage_executable_probe() {
	local source="$1" result_var="$2" probe probe_fd owner_pid="$BASHPID"
	make_dormant_binary_non_executable "$source" "Hub binary to probe" || return 1
	probe="$(mktemp "$(dirname "$BIN")/clawctl-hub.probe.XXXXXX")" || return 1
	if ! install -m 0600 -- "$source" "$probe" || ! exec {probe_fd}<"$probe"; then
		rm -f -- "$probe"
		return 1
	fi
	if ! rm -- "$probe" || ! chmod 0700 -- "/proc/$owner_pid/fd/$probe_fd"; then
		rm -f -- "$probe"
		exec {probe_fd}<&-
		return 1
	fi
	STAGED_BINARY_PROBE_FDS+=("$probe_fd")
	printf -v "$result_var" '%s' "/proc/$owner_pid/fd/$probe_fd"
}

move_installed_binary_to_dormant() {
	local destination="$1" label="$2"
	# chmod happens before rename: there is never a published executable suffix
	# between the final process drain and the rollback snapshot coordinate.
	make_dormant_binary_non_executable "$BIN" "$label" || return 1
	if ! mv -fT -- "$BIN" "$destination"; then
		echo "✗ Cannot move $label to dormant path: $destination" >&2
		return 1
	fi
	make_dormant_binary_non_executable "$destination" "$label" || return 1
	durably_sync_path "$destination" "$label" || return 1
	durably_sync_path "$(dirname "$BIN")" "Hub binary directory"
}

# --- Operator environment 的唯一 parser 是 systemd。
#
# hub.env 裡還有 bot token 與 healthchecks ping URL，不能 source、不能用 env
# 印出來，也不能自己重寫一套 EnvironmentFile grammar。Forward preflight 會
# 用 transient oneshot 交給 systemd 載入同一個檔案，candidate 只回安全的
# versioned sentinel。這同時涵蓋 quote、continuation、duplicate assignment、
# UTF-8 與 manager ambient environment 的上游語意。
HUB_ENV="$HOME/.config/clawctl/hub.env"

ensure_operator_config_directory() {
	local resolved parent permissions
	if [[ -L "$OPERATOR_CONFIG_HOME" ||
		( -e "$OPERATOR_CONFIG_HOME" && ( ! -d "$OPERATOR_CONFIG_HOME" || ! -O "$OPERATOR_CONFIG_HOME" ) ) ]]; then
		echo "✗ operator config root must be a non-symlink directory owned by current user: $OPERATOR_CONFIG_HOME" >&2
		return 1
	fi
	if [[ ! -e "$OPERATOR_CONFIG_HOME" ]]; then
		parent="$(dirname -- "$OPERATOR_CONFIG_HOME")"
		if [[ ! -d "$parent" || -L "$parent" || ! -O "$parent" ]] ||
			! resolved="$(/usr/bin/realpath -e -- "$parent")" || [[ "$resolved" != "$parent" ]]; then
			echo "✗ operator config root parent must first be a canonical directory owned by current user: $parent" >&2
			return 1
		fi
		permissions="$(stat -c '%a' -- "$parent")" || return 1
		if (( (8#$permissions & 8#022) != 0 )); then
			echo "✗ operator config root parent is writable by group/other: $parent ($permissions)" >&2
			return 1
		fi
		if ! mkdir -m 0700 -- "$OPERATOR_CONFIG_HOME"; then
			echo "✗ Cannot create operator config root: $OPERATOR_CONFIG_HOME" >&2
			return 1
		fi
	fi
	if [[ ! -d "$OPERATOR_CONFIG_HOME" || -L "$OPERATOR_CONFIG_HOME" || ! -O "$OPERATOR_CONFIG_HOME" ]] ||
		! resolved="$(/usr/bin/realpath -e -- "$OPERATOR_CONFIG_HOME")" ||
		[[ "$resolved" != "$OPERATOR_CONFIG_HOME" ]]; then
		echo "✗ operator config root is not a canonical non-symlink directory owned by current user: $OPERATOR_CONFIG_HOME" >&2
		return 1
	fi
	permissions="$(stat -c '%a' -- "$OPERATOR_CONFIG_HOME")" || return 1
	if (( (8#$permissions & 8#022) != 0 )); then
		echo "✗ operator config root is writable by group/other: $OPERATOR_CONFIG_HOME ($permissions)" >&2
		return 1
	fi
	if [[ -L "$OPERATOR_CONFIG_DIR" ||
		( -e "$OPERATOR_CONFIG_DIR" && ( ! -d "$OPERATOR_CONFIG_DIR" || ! -O "$OPERATOR_CONFIG_DIR" ) ) ]]; then
		echo "✗ operator config directory must be a non-symlink directory owned by current user: $OPERATOR_CONFIG_DIR" >&2
		return 1
	fi
	if [[ ! -e "$OPERATOR_CONFIG_DIR" ]] && ! mkdir -m 0700 -- "$OPERATOR_CONFIG_DIR"; then
		echo "✗ Cannot create operator config directory: $OPERATOR_CONFIG_DIR" >&2
		return 1
	fi
	# 不讓 final-component symlink race 把 chmod 導向別的 inode。
	if [[ ! -d "$OPERATOR_CONFIG_DIR" || -L "$OPERATOR_CONFIG_DIR" || ! -O "$OPERATOR_CONFIG_DIR" ]] ||
		! resolved="$(/usr/bin/realpath -e -- "$OPERATOR_CONFIG_DIR")" ||
		[[ "$resolved" != "$OPERATOR_CONFIG_DIR" ]]; then
		echo "✗ operator config directory is not a canonical non-symlink directory owned by current user: $OPERATOR_CONFIG_DIR" >&2
		return 1
	fi
	chmod 0700 -- "$OPERATOR_CONFIG_DIR"
}

write_operator_discovery() {
	local real_dir tmp=''
	ensure_operator_config_directory || return 1
	if [[ ! -d "$OPERATOR_CONFIG_DIR" || -L "$OPERATOR_CONFIG_DIR" || ! -O "$OPERATOR_CONFIG_DIR" ]] ||
		! real_dir="$(/usr/bin/realpath -e -- "$OPERATOR_CONFIG_DIR")" ||
		[[ "$real_dir" != "$OPERATOR_CONFIG_DIR" ]]; then
		echo "✗ operator config parent is not a canonical non-symlink directory owned by current user: $OPERATOR_CONFIG_DIR" >&2
		return 1
	fi
	if ! chmod 0700 -- "$OPERATOR_CONFIG_DIR"; then
		echo "✗ Cannot converge operator config parent permissions to 0700: $OPERATOR_CONFIG_DIR" >&2
		return 1
	fi
	tmp="$(mktemp "$OPERATOR_CONFIG_DIR/.operator.json.XXXXXX")" || return 1
	if ! chmod 0600 "$tmp" ||
		! printf '{"hub_url":"%s"}\n' "$URL" >"$tmp" ||
		! mv -fT -- "$tmp" "$OPERATOR_CONFIG"; then
		rm -f -- "$tmp"
		echo "✗ Cannot safely write CLI operator discovery: $OPERATOR_CONFIG" >&2
		return 1
	fi
	echo "  CLI operator discovery pinned $URL → $OPERATOR_CONFIG"
}

verify_systemd_unit_contract() {
	local expected_fragment="$HOME/.config/systemd/user/clawctl-hub.service"
	local desired_unit="$PWD/ops/clawctl-hub.service"
	local contract name value expected_argv required_default property
	local -A properties=()
	if ! contract="$(/usr/bin/timeout --kill-after=2s 10s /usr/bin/systemctl --user show "$UNIT" \
		-p LoadState -p FragmentPath -p DropInPaths -p NeedDaemonReload -p ExecStart \
		-p Environment -p EnvironmentFiles -p UnsetEnvironment -p PAMName 2>/dev/null)"; then
		echo "✗ Cannot read systemd execution contract for $UNIT; Hub not stopped and DB untouched" >&2
		return 1
	fi
	while IFS='=' read -r name value; do
		case "$name" in
			LoadState|FragmentPath|DropInPaths|NeedDaemonReload|ExecStart|Environment|EnvironmentFiles|UnsetEnvironment|PAMName) ;;
			*) echo "✗ $UNIT returned unknown or incomplete systemd property" >&2; return 1 ;;
		esac
		if [[ -v "properties[$name]" ]]; then
			echo "✗ Duplicate $name property in $UNIT; refusing to splice different snapshots" >&2
			return 1
		fi
		properties[$name]="$value"
	done <<<"$contract"
	for property in LoadState FragmentPath DropInPaths NeedDaemonReload ExecStart Environment EnvironmentFiles UnsetEnvironment PAMName; do
		if [[ ! -v "properties[$property]" ]]; then
			echo "✗ $UNIT missing $property systemd property" >&2
			return 1
		fi
	done
	local fragment="${properties[FragmentPath]}"
	local load_state="${properties[LoadState]}"
	local dropins="${properties[DropInPaths]}"
	local need_reload="${properties[NeedDaemonReload]}"
	local exec_start="${properties[ExecStart]}"
	local environment="${properties[Environment]}"
	local envfiles="${properties[EnvironmentFiles]}"
	local unset_environment="${properties[UnsetEnvironment]}"
	local pam_name="${properties[PAMName]}"
	if [[ "$load_state" != loaded ]]; then
		echo "✗ $UNIT LoadState=$load_state, not a startable loaded unit" >&2
		return 1
	fi
	if [[ "$fragment" != "$expected_fragment" ]]; then
    echo "✗ $UNIT FragmentPath is not installer-managed $expected_fragment" >&2
    return 1
  fi
	if [[ ! -f "$fragment" || -L "$fragment" || ! -f "$desired_unit" || -L "$desired_unit" || ! -O "$fragment" ]] ||
		! cmp -s -- "$desired_unit" "$fragment"; then
		echo "✗ Disk content of $UNIT is not the tracked unit from current checkout; run ./ops/stage-hub-unit.sh first" >&2
		return 1
	fi
  if [[ -n "$dropins" ]]; then
    echo "✗ $UNIT DropInPaths must be empty" >&2
    return 1
  fi
	if [[ "$need_reload" != no ]]; then
		echo "✗ $UNIT reports NeedDaemonReload=$need_reload; loaded unit does not match disk content" >&2
		return 1
	fi
  if [[ "$envfiles" != "$HUB_ENV" && "$envfiles" != "$HUB_ENV (ignore_errors=yes)" ]]; then
    echo "✗ $UNIT EnvironmentFiles is not exclusively $HUB_ENV" >&2
    return 1
  fi
	if [[ -n "$unset_environment" || -n "$pam_name" ]]; then
		echo "✗ $UNIT has UnsetEnvironment/PAMName post-processing; transient preflight cannot reproduce final environment" >&2
		return 1
	fi
	for required_default in \
		'CLAWCTL_LISTEN=127.0.0.1:8787' \
		'CLAWCTL_OPERATOR_CAPABILITY_PREFIX=' \
		'CLAWCTL_PUBLIC_URL='
	do
		if [[ " $environment " != *" $required_default "* ]]; then
			echo "✗ $UNIT missing managed $required_default environment default; cannot rule out user-manager ambient drift" >&2
			return 1
		fi
	done
  expected_argv="$BIN --db $DB --listen \${CLAWCTL_LISTEN} --report-at \${CLAWCTL_REPORT_AT} --report-stamp \${CLAWCTL_REPORT_STAMP}"
  if [[ "$exec_start" != *"path=$BIN ; argv[]=$expected_argv ;"* ]]; then
    echo "✗ $UNIT ExecStart does not match managed Hub start contract" >&2
    return 1
	fi
}

run_operator_auth_preflight() {
	local candidate="$1" auth_output='' rc=0
	local marker source destination authority capabilities self_only extra
	local source_value destination_value authority_value port
	# The inner RuntimeMaxSec owns the candidate even if the outer client or SSH
	# session dies. The outer timeout only bounds a wedged user manager/D-Bus.
	if auth_output="$(/usr/bin/timeout --kill-after=2s 15s \
		/usr/bin/systemd-run --user --wait --pipe --quiet --collect \
		--service-type=exec --no-ask-password --expand-environment=no \
		-p TimeoutStartSec=5s -p RuntimeMaxSec=10s -p TimeoutStopSec=2s \
		-p KillMode=control-group -p SendSIGKILL=yes \
		-p NoNewPrivileges=yes -p ProtectSystem=strict -p ProtectHome=read-only \
		-p PrivateTmp=yes -p RestrictSUIDSGID=yes -p LockPersonality=yes \
		-E CLAWCTL_LISTEN=127.0.0.1:8787 \
		-E CLAWCTL_OPERATOR_CAPABILITY_PREFIX= \
		-E CLAWCTL_PUBLIC_URL= \
		-p "EnvironmentFile=-$HUB_ENV" \
		"$candidate" --operator-auth-check </dev/null 2>&1)"; then
		rc=0
	else
		rc=$?
	fi
	IFS=' ' read -r marker source destination authority capabilities self_only extra <<<"$auth_output"
	source_value="${source#source=}"
	destination_value="${destination#destination=}"
	authority_value="${authority#authority=}"
	port="${authority_value##*:}"
	if [[ $rc -eq 0 && "$auth_output" != *$'\n'* && -z "$extra" &&
		"$marker" == 'operator-auth-ready:v1' && "$source" == source=* &&
		"$destination" == destination=* && "$source_value" == "$destination_value" &&
		"$authority" == authority=* &&
		( "$authority_value" == "$destination_value:"* || "$authority_value" == "[$destination_value]:"* ) &&
		"$port" =~ ^[0-9]+$ && "$port" -ge 1 && "$port" -le 65535 &&
		"$capabilities" == 'capabilities=view,operate,admin' && "$self_only" == 'self-only=true' ]]; then
		LISTEN="$authority_value"
		URL="http://$LISTEN"
		echo "  $auth_output"
		return 0
	fi
	[[ $rc -ne 0 ]] || rc=1
	echo "✗ Tailscale operator grant preflight failed (exit $rc); Hub was not stopped and DB was not touched" >&2
	while IFS= read -r line; do echo "  $line" >&2; done <<<"$auth_output"
	return "$rc"
}

# ============================================================ 六道驗收
#
# ⚠⚠ 這六道刻意問**六個不同的東西**。少任何一道，都有一種
# 「換版失敗但看起來成功」的情況會漏過去：
#
#   1. process 剛好 1 個   —— 舊的沒死掉的話，你會拿到兩個 Hub 搶同一個 DB
#   2. /healthz 回 alive   —— 有東西在聽那個 port
#   3. /metrics 的 build_info 是新版本
#        呼叫端必須持有 operator view grant，否則這一道是 403，
#        那代表權限而不是 candidate 壞掉。
#        ⚠ 這一道才是真正證明「換上去的是新的」。前面那個
#        `clawctl-hub version` 問的是**磁碟上那個檔案**，這一道問的是
#        **正在服務的那個 process**。它們是兩個不同的宣稱，
#        而換版失敗最常見的樣子就是這兩個不一樣。
#   4. 首頁 render 得出 </html>
#        ⚠ html/template 的欄位打錯字是**執行期**錯誤，畫面會從那裡安靜地
#        斷掉。一個少了半截的首頁 HTTP 200、healthz 也 alive，
#        而它看起來就跟「機隊沒事」一模一樣。
#   5. NRestarts = 0      —— 起來之後沒有在反覆重啟
#   6. writer flock contended —— 正在服務的新版先取得 process-lifetime fence
hub_process_count() {
  # A normal binary is visible through comm; an unlinked fd-backed probe may
  # instead have a numeric comm, but /proc/<pid>/exe still names its deleted
  # clawctl-hub.probe inode. Count unique PIDs from both witnesses.
  local proc_root="/proc" pid comm exe target count=0
  local -A seen=()
  while read -r pid comm; do
    if [[ "$comm" == clawctl-hub* ]]; then seen["$pid"]=1; fi
  done < <(ps -eo pid=,comm=)
  shopt -s nullglob
  for exe in "$proc_root"/[0-9]*/exe; do
    pid="${exe%/exe}"; pid="${pid##*/}"
    target="$(readlink -- "$exe" 2>/dev/null)" || continue
    target="${target##*/}"
    if [[ "$target" == clawctl-hub* ]]; then seen["$pid"]=1; fi
  done
  shopt -u nullglob
  for pid in "${!seen[@]}"; do count=$((count + 1)); done
  echo "$count"
}

verify() {
  local want="$1" legacy_metrics_optional="${2:-0}" ok=0

  # ⚠ 0 個跟 2 個是兩種完全不同的災難，訊息不可以共用一句。
  # 實測時這裡先寫成一句「有 N 個，兩個 Hub 會搶同一個 SQLite」，
  # 而 N=0 的時候它印出「有 0 個…兩個 Hub 會搶」—— 一句自相矛盾的話。
  # 半夜看到這句的人要先花時間確認腳本有沒有壞，才能開始查真正的問題。
  local n; n="$(hub_process_count)"
  if [[ "$n" == "0" ]]; then
    echo "  ✗ No clawctl-hub process running — failed to start"
    ok=1
  elif [[ "$n" != "1" ]]; then
    echo "  ✗ $n clawctl-hub processes running on this machine; multiple Hubs would contend for the same SQLite database"
    ok=1
  fi
  if ! systemctl --user is-active --quiet "$UNIT" 2>/dev/null; then
    echo "  ✗ clawctl-hub process exists but systemd unit is not active; manual process is not deliverable service"
    ok=1
  fi

  local code body health_body
  health_body="$(mktemp)"
  code="$(curl -sS --max-time 5 -o "$health_body" -w '%{http_code}' "$URL/healthz" 2>/dev/null)" || true
  [[ -n "$code" ]] || code=000
  body="$(cat "$health_body")"
  rm -f "$health_body"
  if [[ "$code" != "200" || "$body" != "alive" ]]; then
    echo "  ✗ $URL/healthz must return HTTP 200 and body=alive (actual HTTP $code)"
    ok=1
  fi

  # ⚠ 問正在服務的那個 process 它自己是誰。
  #
  # ⚠⚠ 這裡要分開兩件事：「問了、答錯」跟「這一版根本沒有這個端點」。
  # /metrics 是 2026-09-03 才加的（§5.19），--rollback 到更早的版本時
  # 它會 404 —— 把 404 當成「版本不對」會讓回退**永遠驗不過**，
  # 於是腳本會宣告一個其實成功的回退失敗了。
  #
  # 但也不可以把它當成通過然後安靜跳過：那就是這個專案一直在踩的那個坑
  # （一支永遠 skip 的檢查在輸出上跟綠燈長得一樣）。所以答案是**講出來**：
  # 這一道問不到，剩下四道要自己撐住，而且畫面上要看得見少了哪一道。
  local metrics_body serving serving_count
  metrics_body="$(mktemp)"
  # ⚠ 不要寫成 `|| echo 000`：curl 失敗的時候 -w 已經印了一個 000，
  # 再 echo 一個就變成 "HTTP 000000" —— 一個不存在的狀態碼。
  code="$(curl -sS --max-time 5 -o "$metrics_body" -w '%{http_code}' "$URL/metrics" 2>/dev/null)" || true
  [[ -n "$code" ]] || code=000
  serving="$(sed -n 's/^clawctl_build_info{version="\([^"]*\)"} 1$/\1/p' "$metrics_body")"
  rm -f "$metrics_body"
  serving_count="$(printf '%s\n' "$serving" | sed '/^$/d' | wc -l)"
  if [[ "$code" == "404" && "$legacy_metrics_optional" == "1" ]]; then
    echo "  ⚠ This version has no /metrics (earlier than §5.19); cannot query serving version"
    echo "    This verification gate is skipped for this run — remaining gates passed, but primary evidence is absent"
  elif [[ "$code" == "403" ]]; then
    echo "  ⚠ /metrics HTTP 403: this caller does not hold the operator view grant, so the serving version cannot be read."
    echo "    This is not a bad candidate binary. Scrape /metrics from a tailnet node that holds the <prefix>-view grant."
    ok=1
  elif [[ "$code" != "200" ]]; then
    echo "  ✗ /metrics HTTP $code, not 200; candidate lacks verifiable version evidence"
    ok=1
  elif [[ "$serving_count" != "1" ]]; then
    echo "  ✗ clawctl_build_info count=$serving_count, expected 1"
    ok=1
  elif [[ "$serving" != "$want" ]]; then
    echo "  ✗ Serving Hub reports version '${serving:-unreadable}' (HTTP $code), expected $want"
    ok=1
  fi

  # ⚠ 首頁要 render 完。截斷的畫面跟「沒事」長得一樣。
  local home_body
  home_body="$(mktemp)"
  code="$(curl -sS --max-time 10 -o "$home_body" -w '%{http_code}' "$URL/" 2>/dev/null)" || true
  [[ -n "$code" ]] || code=000
  if [[ "$code" != "200" ]] || ! grep -q '</html>' "$home_body"; then
    echo "  ✗ Homepage must return HTTP 200 and be fully rendered (actual HTTP $code; requires </html>)"
    ok=1
  fi
  rm -f "$home_body"

  local r; r="$(systemctl --user show "$UNIT" -p NRestarts --value 2>/dev/null || echo '?')"
  if [[ "$r" != "0" ]]; then
    echo "  ✗ Restarted $r times after starting: journalctl --user -u clawctl-hub -n 50"
    ok=1
  fi

  local writer_lock_rc=0
  if [[ "$legacy_metrics_optional" == "1" ]]; then
    echo "  ⚠ legacy recovery does not require process-lifetime writer lock; this version may predate that contract"
  else
    acquire_writer_lock "$WRITER_LOCK" || writer_lock_rc=$?
    if [[ $writer_lock_rc -eq 75 ]]; then
      : # expected: the running candidate owns this inode for its whole lifetime
    elif [[ $writer_lock_rc -eq 0 ]]; then
      release_writer_lock
      echo "  ✗ Running candidate does not hold $WRITER_LOCK; refusing to remove maintenance"
      ok=1
    else
      echo "  ✗ Cannot safely verify running candidate writer lock (exit $writer_lock_rc)"
      ok=1
    fi
  fi

  return $ok
}

# 這不是「binary 能不能執行」測試，而是一份 rollback 契約：candidate 必須
# 明確認得這個 flag，並且只讀確認目前沒有會被舊邏輯繼續推進的工作。
# 舊 binary 會在開 DB 前因 unknown flag 回非零，所以不能假裝支援。
check_rollback_compatibility() {
  local candidate="$1" label="$2" database="${3:-$DB}" output rc
  if output="$(timeout 10 "$candidate" --rollback-compatible --db "$database" 2>&1)"; then
    echo "  $label: $output"
    return 0
  else
    rc=$?
  fi
  echo "  ✗ $label cannot take over current ledger (exit $rc)" >&2
  if [[ -n "$output" ]]; then
    while IFS= read -r line; do echo "    $line" >&2; done <<<"$output"
  fi
  return "$rc"
}

assert_no_hub_process() {
  local n
  n="$(hub_process_count)"
  if systemctl --user is-active --quiet "$UNIT" 2>/dev/null || [[ "$n" != "0" ]]; then
    echo "✗ Hub not cleanly stopped (processes=$n); skipping checks, backups, and SQLite migration" >&2
    return 1
  fi
}

stop_hub_for_database_move() {
  systemctl --user stop "$UNIT" || true
  assert_no_hub_process
}

acquire_database_writer_lock() {
  local writer_lock_rc=0
  acquire_writer_lock "$WRITER_LOCK" || writer_lock_rc=$?
  if [[ $writer_lock_rc -eq 75 ]]; then
    echo "✗ $WRITER_LOCK is held by another writer; Hub stopped, but DB left unread, unbacked, unmoved, and unrestored" >&2
    echo "  Identify the writer; do not start Hub or rerun direct DB until the lock is released" >&2
    return 75
  elif [[ $writer_lock_rc -ne 0 ]]; then
    echo "✗ Cannot safely acquire $WRITER_LOCK; Hub stopped, DB untouched" >&2
    return "$writer_lock_rc"
  fi
  echo "  Acquired exclusive ledger writer lock; held until managed Hub starts"
}

create_rollback_snapshot() {
  local candidate="$1" destination="$2" label="$3" output rc
  if output="$(timeout 30 "$candidate" --rollback-snapshot --maintenance-already-held --db "$DB" --out "$destination" 2>&1)"; then
    echo "  $label: $output"
    return 0
  else
    rc=$?
  fi
  echo "  ✗ $label failed to create rollback snapshot (exit $rc)" >&2
  if [[ -n "$output" ]]; then
    while IFS= read -r line; do echo "    $line" >&2; done <<<"$output"
  fi
  return "$rc"
}

begin_upgrade_maintenance() {
  local candidate="$1" output rc
  if output="$(timeout 10 "$candidate" --upgrade-maintenance-begin --db "$DB" 2>&1)"; then
    echo "  $output"
    return 0
  else
    rc=$?
  fi
  echo "✗ Cannot create upgrade maintenance marker before final process drain (exit $rc)" >&2
  if [[ -n "$output" ]]; then
    while IFS= read -r line; do echo "    $line" >&2; done <<<"$output"
  fi
  return "$rc"
}

verify_stopped_hub_contract() {
  local candidate="$1" output rc
  if output="$(timeout 10 "$candidate" --upgrade-stopped-check --db "$DB" 2>&1)"; then
    echo "  $output"
    return 0
  else
    rc=$?
  fi
  echo "✗ stopped-service verification exit=$rc" >&2
  if [[ -n "$output" ]]; then
    while IFS= read -r line; do echo "  $line" >&2; done <<<"$output"
  fi
  return "$rc"
}

verify_upgrade_protocol() {
  local candidate="$1" output rc
  if output="$(timeout 5 "$candidate" --upgrade-protocol-check 2>&1)" &&
    [[ "$output" == 'upgrade-protocol-ready:v1 stopped-check=true ledger-path=true maintenance-self-disable=true held-snapshot=true dormant-artifacts=true writer-lock-gate=true' ]]; then
    return 0
  else
    rc=$?
  fi
  [[ $rc -ne 0 ]] || rc=1
  echo "✗ Binary does not support the complete fail-closed upgrade protocol of this script; Hub not stopped and DB untouched" >&2
  return "$rc"
}

verify_upgrade_ledger_target() {
  local candidate="$1" output rc
  if output="$(timeout 10 "$candidate" --upgrade-ledger-path-check --db "$DB" 2>&1)"; then
    echo "  $output"
    return 0
  else
    rc=$?
  fi
  echo "✗ Ledger main path or SQLite sidecar is not a canonical owned unaliased target (exit $rc)" >&2
  if [[ -n "$output" ]]; then
    while IFS= read -r line; do echo "    $line" >&2; done <<<"$output"
  fi
  return "$rc"
}

remove_upgrade_maintenance_marker() {
  # 只在 candidate 已驗收成功，或原 DB/binary 已完整復原之後呼叫。
  # 不設 EXIT trap：腳本被 kill 時留下 marker，下一支 CLI 會拒絕寫入。
  rm -- "$MAINTENANCE_MARKER"
  durably_sync_path "$(dirname "$MAINTENANCE_MARKER")" "maintenance marker parent" || return 1
  echo "  Upgrade maintenance cleared; HTTP and CLI writes resumed"
}

# A recovery binary is allowed to be legacy and marker-unaware.  If its
# post-start verification fails, never leave that known-bad writer running.
# Disable the installed pathname first (which also prevents restart loops from
# execing it), stop and globally drain Hub-shaped processes, then retain the
# database exactly as the failed process left it for an explicit recovery
# decision.  A known-new helper may republish the marker, but safety does not
# depend on a legacy process honoring it.
fail_closed_started_recovery() {
  local label="$1" marker_helper="${2:-}" marker_probe='' marker_ready=0
  echo "✗ $label failed verification after startup; immediately transitioning to stopped and non-executable fail-closed state" >&2

  if [[ -e "$MAINTENANCE_MARKER" || -L "$MAINTENANCE_MARKER" ]]; then
    marker_ready=1
  elif [[ -n "$marker_helper" ]]; then
    if stage_executable_probe "$marker_helper" marker_probe &&
      begin_upgrade_maintenance "$marker_probe"; then
      marker_ready=1
    else
      echo "  ⚠ Recovery helper cannot reconstruct maintenance marker; will still stop non-compliant Hub first" >&2
    fi
    cleanup_staged_binary_probes
  fi

  # chmod precedes systemctl stop so Restart=always cannot exec another copy in
  # the stop race.  The already-running inode may continue briefly, therefore
  # no DB snapshot/restore is attempted until the recursive drain is proven.
  local dormant_ok=1
  if ! make_dormant_binary_non_executable "$BIN" "failed verification installed Hub binary"; then
    dormant_ok=0
  fi
  systemctl --user stop "$UNIT" || true
  if ! assert_no_hub_process; then
    echo "  DB left unchanged; handle running process first before selecting rollback coordinate" >&2
    return 1
  fi
  if [[ $dormant_ok -eq 1 && ( -e "$BIN" || -L "$BIN" ) ]]; then
    durably_sync_path "$BIN" "disabled failed recovery binary" || dormant_ok=0
  fi
  durably_sync_path "$(dirname "$BIN")" "Hub binary directory after failed recovery drain" || dormant_ok=0

  if [[ -z "${WRITER_LOCK_FD:-}" ]]; then
    if ! acquire_database_writer_lock; then
      echo "  ✗ Process stopped, but writer lock is still occupied; DB access remains stopped" >&2
    fi
  fi
  if [[ $marker_ready -eq 1 ]]; then
    echo "  Maintenance marker retained; installed binary deactivated, DB preserved as left by failed startup" >&2
  else
    echo "  Maintenance marker could not be verified; installed binary deactivated, DB preserved as left by failed startup" >&2
  fi
  echo "  automatic restore: blocked; next step: compare DB, snapshot, and quarantine" >&2
  [[ $dormant_ok -eq 1 ]] || echo "  ✗ Binary durable-disable check failed: $BIN" >&2
  return 1
}

restart_current_after_refusal() {
  local current="$1" marker_helper="${2:-}"
  echo "→ Binary unchanged; restarting original $current"
  cleanup_staged_binary_probes
  if ! assert_no_hub_process; then
    echo "✗ Private probe process has not exited; Hub will not be started" >&2
    return 1
  fi
  if [[ -e "$BIN.new" || -L "$BIN.new" ]]; then
    make_dormant_binary_non_executable "$BIN.new" "unactivated candidate binary" || return 1
    rm -- "$BIN.new"
  fi
  # systemd Hub 必須自己取得 writer lock；絕不把 shell 持有的 lock 帶進
  # start/verify 窗口。maintenance marker（若有）仍封住這個短暫 handoff。
  release_writer_lock
  systemctl --user reset-failed "$UNIT" 2>/dev/null || true
  systemctl --user start "$UNIT" || true
  sleep 3
  if verify "$current" 1; then
    if [[ -e "$MAINTENANCE_MARKER" || -L "$MAINTENANCE_MARKER" ]]; then
      if ! durably_sync_path "$(dirname "$DB")" "restarted original ledger state" ||
        ! remove_upgrade_maintenance_marker; then
        fail_closed_started_recovery "original $current" "$marker_helper"
        return 1
      fi
    fi
    echo "  Original $current resumed service; rollback rejected"
  else
    fail_closed_started_recovery "original $current" "$marker_helper"
    return 1
  fi
}

fail_closed_binary_restore() {
  local detail="$1"
  echo "✗ Cannot safely restore original Hub binary: $detail" >&2
  echo "  Hub remains stopped; if maintenance marker was created, it is retained and writes are never reopened automatically" >&2
  return 1
}

restore_verified_binary() {
  local saved_binary="$1" want="$2" restore_tmp got VERIFY_PROBE=''
  if [[ ! -f "$saved_binary" ]]; then
    fail_closed_binary_restore "cannot find $saved_binary (expected version $want)"
    return 1
  fi
  if ! make_dormant_binary_non_executable "$saved_binary" "Hub binary to restore"; then
    fail_closed_binary_restore "$saved_binary is not a safe dormant binary"
    return 1
  fi
  if ! stage_executable_probe "$saved_binary" VERIFY_PROBE; then
    fail_closed_binary_restore "failed to create private version probe"
    return 1
  fi
  if ! got="$(timeout 5 "$VERIFY_PROBE" version 2>&1)"; then
    fail_closed_binary_restore "private probe failed to report version: $got"
    return 1
  fi
  if [[ "$got" != "$want" ]]; then
    fail_closed_binary_restore "private probe version=$got, expected $want"
    return 1
  fi
  cleanup_staged_binary_probes
  if ! assert_no_hub_process; then
    fail_closed_binary_restore "private version probe process has not exited"
    return 1
  fi
  restore_tmp="$(mktemp "$(dirname "$BIN")/clawctl-hub.restore.XXXXXX")" || {
    fail_closed_binary_restore "failed to create temporary file in $(dirname "$BIN")"
    return 1
  }
  if ! install -m 0600 -- "$saved_binary" "$restore_tmp"; then
    rm -f -- "$restore_tmp"
    fail_closed_binary_restore "install $saved_binary failed"
    return 1
  fi
  if ! durably_sync_path "$restore_tmp" "staged restored binary"; then
    return 1
  fi
  if ! mv -fT -- "$restore_tmp" "$BIN"; then
    rm -f -- "$restore_tmp"
    fail_closed_binary_restore "failed to atomically replace $BIN"
    return 1
  fi
  if ! chmod 0755 -- "$BIN"; then
    fail_closed_binary_restore "failed to enable restored binary"
    return 1
  fi
  if ! durably_sync_path "$BIN" "restored Hub binary" ||
    ! durably_sync_path "$(dirname "$BIN")" "Hub binary directory"; then
    fail_closed_binary_restore "restored binary rename not yet durable"
    return 1
  fi
  if ! got="$(timeout 5 "$BIN" version 2>&1)"; then
    fail_closed_binary_restore "installed version failed to run: $got"
    return 1
  fi
  if [[ "$got" != "$want" ]]; then
    fail_closed_binary_restore "installed version=$got, expected $want"
    return 1
  fi
  echo "  Verified restored Hub binary: $want"
  return 0
}

restore_binary_after_snapshot_failure() {
  local saved_binary="$1" current="$2" partial_snapshot="$3"
  echo "→ Snapshot failed; original $current left in dormant path, Hub remains stopped"
  make_dormant_binary_non_executable "$saved_binary" "original Hub binary after snapshot failure" || true

  # Snapshot failure can mean an orphan WAL/SHM/journal, corrupt source, I/O error or
  # a killed backup process. The script cannot prove which one occurred, so it
  # must not turn a correctly retained marker into permission to write.
  echo "✗ Rollback snapshot did not complete; maintenance marker retained, Hub will not restart automatically" >&2
  echo "  Next step: inspect DB/WAL/SHM/journal, partial snapshot, and binary, then choose recovery coordinate:" >&2
  echo "    DB: $DB" >&2
  echo "    WAL/SHM/journal: $DB-wal  $DB-shm  $DB-journal" >&2
  if [[ -e "$partial_snapshot" ]]; then
    echo "    partial snapshot: $partial_snapshot (present)" >&2
  else
    echo "    partial snapshot: $partial_snapshot (absent)" >&2
  fi
  echo "    dormant binary: $saved_binary (0600, not directly executable)" >&2
  echo "    marker: $MAINTENANCE_MARKER" >&2
}

restore_binary_after_handoff_refusal() {
  local saved_binary="$1" current="$2"
  echo "→ Snapshot succeeded, but handoff/capability rejected; restoring original $current"
  # Keep the ledger-to-binary ordering explicit even when the ledger was not
  # rewritten: the legacy/current executable may live on another filesystem.
  if ! durably_sync_path "$(dirname "$DB")" "ledger before original binary restore"; then
    return 1
  fi
  if ! restore_verified_binary "$saved_binary" "$current"; then
    return 1
  fi
  # Snapshot already proved the source was a consistent, quiescent ledger.
  # Keep the marker while the restored process starts and is verified; only a
  # successful gate may remove it.
  cleanup_staged_binary_probes
  if ! assert_no_hub_process; then
    echo "✗ Private probe process has not exited; marker retained, will not restart" >&2
    return 1
  fi
  # Keep the marker across start+verification.  A marker-aware current binary
  # stays read-only; a legacy one may ignore it, but failure still leaves the
  # barrier present for every new-aware CLI and recovery process.
  restart_current_after_refusal "$current" "$saved_binary"
}

# 新 binary 已經打開／migrate 過 DB 時，先把它留下來供事後查原因；絕不直接
# rm。目錄權限在這裡明訂，不依賴執行者當時的 umask。
quarantine_failed_database() {
  local quarantine="$1" suffix candidate moved=0
  mkdir -p "$quarantine"
  chmod 0700 "$quarantine"
  for suffix in '' '-wal' '-shm' '-journal'; do
    candidate="$DB$suffix"
    if [[ -e "$candidate" || -L "$candidate" ]]; then
      mv -- "$candidate" "$quarantine/"
      moved=$((moved + 1))
    fi
  done
  # The quarantine rename and the disappearance from the live ledger directory
  # are one crash-consistency boundary.  Flush both directory entries before
  # an old binary can be published or the maintenance marker can disappear.
  durably_sync_path "$quarantine" "failed-ledger quarantine directory" || return 1
  durably_sync_path "$(dirname "$DB")" "ledger directory after quarantine" || return 1
  echo "  DB/WAL/SHM/journal touched by new version quarantined to $quarantine (0700, $moved files)"
}

# 只還原**這一次**換版前由 SQLite online backup 做出的單檔 snapshot，
# 不猜 backups/ 裡哪一個檔名最新，也不拼接不同時間點的 WAL。
restore_preupgrade_database() {
  local backup="$1" restore_tmp=''
  mkdir -p "$(dirname "$DB")"
  if [[ $PREUPGRADE_DB_PRESENT -eq 0 ]]; then
    rm -f -- "$DB" "$DB-wal" "$DB-shm" "$DB-journal"
    durably_sync_path "$(dirname "$DB")" "absent pre-upgrade ledger state" || return 1
    echo "  No DB existed before upgrade; preserving absent state at that timestamp (old Hub will create on startup)"
    return 0
  fi
  restore_tmp="$(mktemp "$(dirname "$DB")/clawctl.sqlite.restore.XXXXXX")" || return 1
  if ! install -m 0600 -- "$backup" "$restore_tmp"; then
    rm -f -- "$restore_tmp"
    return 1
  fi
  if ! durably_sync_path "$restore_tmp" "staged pre-upgrade ledger"; then
    rm -f -- "$restore_tmp"
    return 1
  fi
  if ! mv -fT -- "$restore_tmp" "$DB"; then
    rm -f -- "$restore_tmp"
    return 1
  fi
  rm -f -- "$DB-wal" "$DB-shm" "$DB-journal"
  durably_sync_path "$DB" "restored pre-upgrade ledger" || return 1
  durably_sync_path "$(dirname "$DB")" "ledger directory after restore" || return 1
  echo "  Accurately restored pre-upgrade standalone snapshot: $backup"
}

# 遺留 marker 代表上一輪在「可能還會 restore DB」的窗口中斷。不能猜它停在
# 哪一格，也不能因為這次腳本重新啟動就自動解除。
verify_no_stale_maintenance_marker() {
	if [[ -e "$MAINTENANCE_MARKER" || -L "$MAINTENANCE_MARKER" ]]; then
		echo "✗ Found leftover upgrade maintenance marker: $MAINTENANCE_MARKER" >&2
		echo "  Next step: verify current binary, DB, quarantine, and systemd status; remove marker after stopping restore path" >&2
		return 1
	fi
}
# Sanitize known and interrupted-run artifacts before honoring a stale marker.
# A stale marker intentionally stops lifecycle progress, but it must not leave
# an executable candidate/probe beside an uncertain database indefinitely.
shopt -s nullglob
stale_binary_artifacts=(
	"$BIN.prev" "$BIN.failed" "$BIN.new"
	"$(dirname "$BIN")"/clawctl-hub.probe.*
	"$(dirname "$BIN")"/clawctl-hub.restore.*
	"$(dirname "$BIN")"/.clawctl-hub.probe.*
	"$(dirname "$BIN")"/.clawctl-hub.restore.*
)
shopt -u nullglob
for dormant in "${stale_binary_artifacts[@]}"; do
	make_dormant_binary_non_executable "$dormant" "existing dormant Hub binary" || exit 1
done
if ! verify_no_stale_maintenance_marker; then
	exit 1
fi

# ============================================================ --rollback
if [[ $ROLLBACK -eq 1 ]]; then
  # Emergency rollback must remain usable by a binary that predates the
  # operator-auth handshake. It only needs the listener for post-start health
  # checks; importantly, malformed/missing forward-only capability settings do
  # not block this path. Forward rollout below does not use this shell parser.
  LISTEN="$(sed -n 's/^CLAWCTL_LISTEN=//p' "$HUB_ENV" 2>/dev/null | tail -1 | tr -d '"'"'"'')"
  LISTEN="${LISTEN:-127.0.0.1:8787}"
  # 0.0.0.0 is a bind address, not a connectable verification target.
  URL="http://${LISTEN/#0.0.0.0:/127.0.0.1:}"
  [[ -f "$BIN.prev" ]] || { echo "No $BIN.prev to roll back to" >&2; exit 1; }
  current="$(timeout 5 "$BIN" version 2>&1 || echo '?')"
  if ! verify_upgrade_protocol "$BIN"; then
    echo "  Current installed binary=$current; complete a forward rollout first so manual rollback has marker-before-drain capability" >&2
    exit 1
  fi
  PREV_PROBE=''
  if ! stage_executable_probe "$BIN.prev" PREV_PROBE; then
    echo "✗ Cannot create private executable probe for previous version; Hub not stopped and DB untouched" >&2
    exit 1
  fi
  prev="$(timeout 5 "$PREV_PROBE" version 2>&1 || echo '?')"
  echo "→ Rolling back to $prev"
  stop_hub_for_database_move || exit 1
  acquire_database_writer_lock || exit 1
  echo "→ Rechecking exact stopped unit and recursive cgroup with current binary (DB unread)"
  if ! verify_stopped_hub_contract "$BIN"; then
    echo "✗ Emergency rollback lacks stopped-service proof; Hub remains stopped, DB and binary unmoved" >&2
    exit 1
  fi
  if ! verify_upgrade_ledger_target "$BIN"; then
    echo "✗ Emergency rollback DB identity is unsafe; Hub remains stopped, DB unread and unmoved" >&2
    exit 1
  fi
  # The previous binary can only prove the invariants it knows.  The current
  # binary must first rule out forward-only work (for example artifact fetch
  # operations) that an older rollback target would silently ignore.
  echo "→ Preflighting with current version in read-only mode: no active work unrecognized by previous version"
  if ! check_rollback_compatibility "$BIN" "current version $current"; then
    echo "✗ Refusing rollback to $prev: current ledger still has active work that cannot be handed off to previous version" >&2
    echo "  No --force; drain the ledger cleanly first, or perform a controlled restore from pre-upgrade backup" >&2
    restart_current_after_refusal "$current" "$BIN"
    exit 1
  fi
  echo "→ Checking with previous version in read-only mode: verify rollback contract and quiescent ledger"
  if ! check_rollback_compatibility "$PREV_PROBE" "previous version $prev"; then
    echo "✗ Refusing rollback to $prev: capability unsupported, or active deployments (running/paused) / non-terminal jobs / active artifact fetches remain" >&2
    echo "  No --force; drain the ledger cleanly first, or perform a controlled restore from pre-upgrade backup" >&2
    restart_current_after_refusal "$current" "$BIN"
    exit 1
  fi
  cleanup_staged_binary_probes
  if ! assert_no_hub_process; then
    echo "✗ Previous version probe process has not exited; marker and snapshot not created" >&2
    restart_current_after_refusal "$current" "$BIN"
    exit 1
  fi

  mkdir -p "$BACKUPS"
  chmod 0700 "$BACKUPS"
  rollback_stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  rollback_snapshot="$BACKUPS/clawctl-$rollback_stamp-before-rollback-to-$prev.sqlite"
  if [[ -e "$rollback_snapshot" ]]; then
    rollback_snapshot="$BACKUPS/clawctl-$rollback_stamp-$$-before-rollback-to-$prev.sqlite"
  fi

  if ! begin_upgrade_maintenance "$BIN"; then
    echo "✗ Maintenance begin state unknown; Hub remains stopped, marker will not be automatically cleared and no restart" >&2
    exit 1
  fi
  CURRENT_PROBE=''
  if ! stage_executable_probe "$BIN" CURRENT_PROBE; then
    echo "✗ Maintenance created, but cannot stage current binary; Hub remains stopped, marker retained" >&2
    exit 1
  fi
  # Marker 先封住支援 rollback 契約的 CLI；再讓 installed path 與所有
  # dormant suffix 都不可執行，最後重查已經 exec 成功的 process 全數離場。
  if ! move_installed_binary_to_dormant "$BIN.failed" "current Hub binary"; then
    exit 1
  fi
  if ! assert_no_hub_process; then
    echo "✗ Final drain failed; $BIN.failed kept at 0600, marker retained, refusing snapshot and restart" >&2
    exit 1
  fi
  if ! verify_upgrade_ledger_target "$CURRENT_PROBE"; then
    echo "✗ DB identity drifted after final drain; marker retained, snapshot not created" >&2
    exit 1
  fi
  if ! create_rollback_snapshot "$CURRENT_PROBE" "$rollback_snapshot" "handoff snapshot of current version"; then
    restore_binary_after_snapshot_failure "$BIN.failed" "$current" "$rollback_snapshot"
    release_writer_lock
    exit 1
  fi
  cleanup_staged_binary_probes
  if ! assert_no_hub_process; then
    echo "✗ Snapshot helper process has not exited; marker retained, skipping handoff" >&2
    exit 1
  fi
  rollback_check_db="$rollback_snapshot"
  if [[ ! -f "$rollback_snapshot" ]]; then
    # Clean first install: capability must see the same absent-ledger state.
    rollback_check_db="$DB"
  fi
  PREV_PROBE=''
  if ! stage_executable_probe "$BIN.prev" PREV_PROBE; then
    echo "✗ Cannot create private probe of previous version for handoff snapshot; marker retained" >&2
    exit 1
  fi
  if ! check_rollback_compatibility "$PREV_PROBE" "previous version $prev (snapshot)" "$rollback_check_db"; then
    echo "✗ Refusing rollback to $prev: previous version cannot take over completed snapshot" >&2
    if ! restore_binary_after_handoff_refusal "$BIN.failed" "$current"; then
      exit 1
    fi
    exit 1
  fi
  cleanup_staged_binary_probes
  if ! assert_no_hub_process; then
    echo "✗ Previous version handoff probe process has not exited; marker retained" >&2
    exit 1
  fi

  # ⚠ 目前這個已留在 .failed，不要直接丟掉 —— 你可能還要看它為什麼壞。
  if ! durably_sync_path "$(dirname "$DB")" "ledger before legacy binary publish"; then
    echo "✗ Ledger not yet durable; marker retained, will not publish previous binary" >&2
    exit 1
  fi
  if ! restore_verified_binary "$BIN.prev" "$prev"; then
    exit 1
  fi
  # DB 沒有再 restore 的可能、舊 binary 已就位；解除後的新 CLI 寫入會由
  # 即將啟動的舊 Hub 看見，不會掉進 rollback 黑洞。
  cleanup_staged_binary_probes
  if ! assert_no_hub_process; then
    echo "✗ Private probe process has not exited; marker retained, will not restart" >&2
    exit 1
  fi
  release_writer_lock
  systemctl --user reset-failed "$UNIT" 2>/dev/null || true
  systemctl --user start "$UNIT" || true
  sleep 3
  if verify "$prev" 1; then
    if ! durably_sync_path "$(dirname "$DB")" "verified legacy rollback ledger state" ||
      ! remove_upgrade_maintenance_marker; then
      fail_closed_started_recovery "rolled back previous version $prev" "$BIN.failed" || true
      exit 1
    fi
    echo "Rolled back to $prev. Bad binary left in $BIN.failed"
    exit 0
  fi
  fail_closed_started_recovery "rolled back previous version $prev" "$BIN.failed" || true
  echo "   journalctl --user -u clawctl-hub -n 80" >&2
  exit 1
fi

# ============================================================ 1. 這一版是誰
# Forward rollout requires the exact installed unit contract before it builds
# a candidate. The candidate later reads the real EnvironmentFile through
# systemd itself; emergency rollback above deliberately does not depend on it.
if ! verify_systemd_unit_contract; then
	exit 1
fi
#
# 跟 ops/upgrade-agent.sh 同一套，理由也一樣（見那支的註解）：
# 版本字串要描述的是**這個 binary 裡的程式碼**，不是整個工作目錄的心情，
# 所以只看會被編進 binary 的那些檔案，而不是 `git describe --dirty`
# （它對 untracked 的 .go 檔完全不說話 —— 那比 -dirty 更糟）。
VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
BUILD_DIRT="$(git status --porcelain -- '*.go' 'go.mod' 'go.sum' '*.html' '*.sql' \
  'Makefile' 'ops/install-agent.sh' 'ops/install-agent-macos.sh' 'ops/clawctl-agent.service' 'ops/clawctl-hermes.service' \
  'ops/openclaw-gateway.service' 'ops/clawctl-agent.plist' \
  'ops/build-agent-bundles.sh' 'ops/publish-agent-bundles.sh' 2>/dev/null)"
if [[ -n "$BUILD_DIRT" ]]; then
  VERSION="$VERSION-dirty"
fi
if [[ -n "$BUILD_DIRT" && $ALLOW_DIRTY -eq 0 ]]; then
  cat >&2 <<EOF
⚠ Uncommitted changes in files that will be compiled into binary:

$BUILD_DIRT

Version will be $VERSION, which cannot identify which source code is running on Hub.
Commit first, or explicitly specify --allow-dirty.
EOF
  exit 1
fi

echo "→ Building $VERSION"
# ⚠ VERSION 一定要傳給 make。Makefile 自己也會算一個，不傳的話 binary 上
# 戳的是它算的那個，而下面的比對用的是我算的這個 —— 兩個算法哪天分岔，
# 這個比對就會在一台好機器上失敗。
make hub agent-bundles agent-bundles-darwin agent-bundles-windows VERSION="$VERSION" >/dev/null

# ============================================================ 2. 先驗新 binary，再碰任何東西
#
# ⚠ 這一步在停服務**之前**。一個跑不起來的 binary 要在 Hub 還活著的時候
# 就被擋下來 —— 否則你會停掉一個好的 Hub，去換一個開不起來的。
install -m 0755 build/clawctl-hub "$BIN.new"
got="$(timeout 5 "$BIN.new" version 2>&1)" || {
  echo "✗ New binary failed to run: $got" >&2; rm -f "$BIN.new"; exit 1; }
if [[ "$got" != "$VERSION" ]]; then
  echo "✗ New binary reported version $got, expected $VERSION" >&2
  rm -f "$BIN.new"; exit 1
fi
if ! verify_upgrade_protocol "$BIN.new"; then
  make_dormant_binary_non_executable "$BIN.new" "incomplete candidate binary" || true
  exit 1
fi
was="$(timeout 5 "$BIN" version 2>&1 || echo '?')"
echo "  Currently running $was, upgrading to $VERSION"
if [[ "$was" == "$VERSION" ]]; then
  echo "  ⚠ Already this version; still running full lifecycle (restart and verification)"
fi

# ============================================================ 2.5 先證明 grant，再停 Hub
#
# /healthz 刻意不依賴 human auth，所以只看它會把「服務活著、operator 全部
# 403」誤報成成功。這個 candidate-owned handshake 直接用官方 LocalAPI，對
# 即將使用的 trusted destination 查三把 exact app capability；不開 listener、
# 不開 DB。預設 source 是 Hub 自己的 tailnet IP，符合本機 rollout owner。
echo "→ Preflight Tailscale operator grant (Hub not stopped and DB not touched yet)"
if run_operator_auth_preflight "$BIN.new"; then
	:
else
	rc=$?
	rm -f "$BIN.new"
	exit "$rc"
fi

echo "→ Publishing Agent bootstrap $VERSION"
if ! ./ops/publish-agent-bundles.sh --version "$VERSION" --source-dir "$PWD/build" --state-dir "$STATE_DIR"; then
	rm -f "$BIN.new"
	exit 1
fi
# Re-read the loaded properties and checked-in FragmentPath as one snapshot at
# the destructive boundary. A unit edit/reload between build and preflight may
# not turn an earlier proof into permission to stop the live Hub.
if ! verify_systemd_unit_contract; then
	rm -f "$BIN.new"
	exit 1
fi
if ! write_operator_discovery; then
	rm -f "$BIN.new"
	exit 1
fi

# ============================================================ 3. 停下來、封住寫入、做 SQLite snapshot
#
# raw cp 主檔/WAL 沒有一致性邊界，而且「先查靜止、再 cp」中間仍可被另一支
# CLI 開單。candidate 的 online backup 把 WAL 合成一份 standalone SQLite，
# 並且在**那份 snapshot**上 quick_check + 重查 ledger 靜止。
echo "→ Stopping Hub"
stop_hub_for_database_move || exit 1
acquire_database_writer_lock || exit 1
echo "→ Candidate rechecking exact stopped unit and recursive cgroup (DB unread)"
if ! verify_stopped_hub_contract "$BIN.new"; then
  echo "✗ Stopped-service contract drifted at shutdown boundary; Hub remains stopped, DB and binary unmoved" >&2
  exit 1
fi
if ! verify_upgrade_ledger_target "$BIN.new"; then
  echo "✗ Forward rollout DB identity is unsafe; Hub remains stopped, DB unread and unmoved" >&2
  exit 1
fi

# 先給一個不改 DB 的早期錯誤；真正有原子邊界的裁決仍是 marker 之後產生、
# 並在 snapshot 本身上做的那一次。
echo "→ New binary read-only preflight: ledger quiescent (migration not permitted at this step)"
if ! check_rollback_compatibility "$BIN.new" "candidate $VERSION"; then
  echo "✗ Refusing upgrade: complete or clear active deployments (running/paused), non-terminal jobs, and active artifact fetches (queued/running) first" >&2
  restart_current_after_refusal "$was"
  exit 1
fi

PREUPGRADE_DB_PRESENT=0
PREUPGRADE_DB_BACKUP=''
UPGRADE_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$BACKUPS"
chmod 0700 "$BACKUPS"

# 檔名帶「換版前的版本」，不是帶新版本 —— 這份備份是**那一版寫出來的**。
dest="$BACKUPS/clawctl-$UPGRADE_STAMP-before-$VERSION.sqlite"
# 同一秒重跑同一版也不能覆寫上一份 rollback 座標。
if [[ -e "$dest" || -L "$dest" ]]; then
  dest="$BACKUPS/clawctl-$UPGRADE_STAMP-$$-before-$VERSION.sqlite"
fi
PREUPGRADE_DB_BACKUP="$dest"

# Marker 必須早於 final drain：先阻止懂此契約的 CLI 進場，再把 installed
# old binary 變成 0600 dormant artifact。最後一次 process=0 之後，已經完成的
# 寫入會落進接下來的 snapshot；仍在跑的 writer 會中止本次換版。
if ! begin_upgrade_maintenance "$BIN.new"; then
  echo "✗ Maintenance begin state unknown; Hub remains stopped, marker will not be automatically cleared and no restart" >&2
  exit 1
fi
CANDIDATE_PROBE=''
if ! stage_executable_probe "$BIN.new" CANDIDATE_PROBE; then
  echo "✗ Candidate self-deactivated and marker created, but cannot stage private snapshot probe; Hub remains stopped" >&2
  exit 1
fi
if ! move_installed_binary_to_dormant "$BIN.prev" "previous Hub binary"; then
  exit 1
fi
if ! assert_no_hub_process; then
  echo "✗ Final drain failed; $BIN.prev kept at 0600, marker retained, refusing snapshot and restart" >&2
  exit 1
fi
if ! verify_upgrade_ledger_target "$CANDIDATE_PROBE"; then
  echo "✗ DB identity drifted after final drain; marker retained, snapshot not created" >&2
  exit 1
fi

echo "→ Final drain complete; creating consistent rollback snapshot under existing upgrade maintenance"
if ! create_rollback_snapshot "$CANDIDATE_PROBE" "$PREUPGRADE_DB_BACKUP" "rollback snapshot for candidate version"; then
  restore_binary_after_snapshot_failure "$BIN.prev" "$was" "$PREUPGRADE_DB_BACKUP"
  release_writer_lock
  exit 1
fi
cleanup_staged_binary_probes
if ! assert_no_hub_process; then
  echo "✗ Snapshot helper process has not exited; $BIN.new and $BIN.prev kept at 0600, marker retained" >&2
  exit 1
fi

if [[ -f "$PREUPGRADE_DB_BACKUP" ]]; then
  PREUPGRADE_DB_PRESENT=1
  echo "  Backup $(du -h "$PREUPGRADE_DB_BACKUP" | cut -f1) → $PREUPGRADE_DB_BACKUP"
  echo "  Rollback data timestamp: $UPGRADE_STAMP (Hub stopped; standalone SQLite snapshot)"

  # --- 舊備份要清掉。
  #
  # ⚠⚠ 這一段是實測長出來的：換版跑了五次之後，備份目錄 90 MB。
  # 一個只進不出的備份目錄最後會把磁碟塞滿，而磁碟滿了的 Hub 會死掉 ——
  # 於是一個**為了讓換版安全而存在**的機制，變成了停機的原因。
  #
  # 特別諷刺的是：這個專案有一整套資料保留政策
  # （internal/store/retention.go，還有一節文件在講不准刪掉最新那一筆），
  # 而它自己的備份腳本沒有同一套保留規則。
  #
  # ⚠ 規則跟 retention.go 那邊刻意一樣：**永遠留著最新的那一份。**
  # 就算 KEEP 被設成 0 也一樣 —— 一個把最後一份備份也刪掉的清理，
  # 正好在你最需要它的那一刻把它拿走。
  KEEP="${CLAWCTL_KEEP_BACKUPS:-10}"
  mapfile -t old < <(ls -t "$BACKUPS"/clawctl-*.sqlite 2>/dev/null | tail -n +$((KEEP > 1 ? KEEP + 1 : 2)))
  prune=()
  for f in "${old[@]}"; do
    # 這份是下面 automatic rollback 唯一准用的座標；即使主機時鐘跳動
    # 讓 ls -t 把它排錯，也不能在同一次換版中自己刪掉它。
    [[ "$f" == "$PREUPGRADE_DB_BACKUP" ]] || prune+=("$f")
  done
  if [[ ${#prune[@]} -gt 0 ]]; then
    # ⚠ 講出來刪了什麼。一個安靜刪東西的腳本，會讓「備份不見了」
    # 跟「從來沒備份過」在事後長得一模一樣。
    echo "  Pruned ${#prune[@]} old backups (retained newest $KEEP):"
    for f in "${prune[@]}"; do
      echo "    - $(basename "$f")"
      rm -f "$f" "$f-wal"
    done
  fi
  echo "  Backup directory now $(du -sh "$BACKUPS" | cut -f1) ($(ls "$BACKUPS"/clawctl-*.sqlite 2>/dev/null | wc -l) files)"
else
  echo "  ⚠ No $DB existed before upgrade, and candidate verified no orphaned WAL/SHM/journal; recorded as empty ledger"
fi

# ============================================================ 4. 換上去
# ⚠ mv 不是 cp。cp 會在執行中的檔案上拿到 ETXTBSY。
make_dormant_binary_non_executable "$BIN.new" "verified candidate binary" || exit 1
durably_sync_path "$BIN.new" "staged candidate binary" || exit 1
mv -fT -- "$BIN.new" "$BIN"
chmod 0755 -- "$BIN"
durably_sync_path "$BIN" "activated candidate binary" || exit 1
durably_sync_path "$(dirname "$BIN")" "Hub binary directory after activation" || exit 1
activated="$(timeout 5 "$BIN" version 2>&1)" || {
  echo "✗ Candidate cannot execute after placement at installed path; marker retained, Hub remains stopped" >&2
  exit 1
}
if [[ "$activated" != "$VERSION" ]]; then
  echo "✗ Installed candidate version=$activated, expected $VERSION; marker retained, Hub remains stopped" >&2
  exit 1
fi
echo "→ Starting Hub"
# ⚠⚠ `|| true` 不是在偷懶，它是這支腳本能不能救回自己的關鍵。
#
# 實測（2026-09-03，故意部署一個一起來就 log.Fatal 的 build）：unit 是
# Type=notify，起不來的時候 `systemctl start` 會**回非 0**，而 set -e
# 當場把整支腳本殺掉 —— 停在「壞的 binary 已經換上去、服務是 down 的」
# 那一格，**回退那一段一行都沒有跑到**。
#
# 一支只有在事情順利時才走得到回退邏輯的腳本，等於沒有回退。
# 而它失敗的樣子比沒有回退更糟：它會 exit 1 然後閉嘴，
# 你要自己看得出來 Hub 現在是死的。
# NRestarts 是 systemd unit 自上次 reset-failed 以來的累積值；先歸零，verify
# 才量到這一個 candidate，而不是把上個月的 restart 算在它頭上。
release_writer_lock
systemctl --user reset-failed "$UNIT" 2>/dev/null || true
systemctl --user start "$UNIT" || true
sleep 3

# ============================================================ 5. 驗收，不過就換回去
if verify "$VERSION" 0; then
  # 到這裡 candidate 才正式擁有 DB。marker 每個 request 都會重讀，移除後
  # 同一個 process 立刻恢復 POST/PUT/PATCH/DELETE，不必再重啟一次。
  # SQLite commit 與 marker 位於不同 pathname；先把 ledger filesystem durable，
  # 再 durable unlink marker，避免斷電後重開到「writes 已開但 DB 未落盤」。
  durably_sync_path "$(dirname "$DB")" "verified candidate ledger state" || exit 1
  remove_upgrade_maintenance_marker
  rollback_coordinate="$PREUPGRADE_DB_BACKUP"
  if [[ $PREUPGRADE_DB_PRESENT -eq 0 ]]; then
    rollback_coordinate="(No DB existed before upgrade; rollback coordinate is 'absent', no snapshot file)"
  fi
  echo
  cat <<EOF
Hub upgraded to $VERSION. Previous binary left in $BIN.prev, consistent snapshot saved to:

  $rollback_coordinate

Post-upgrade verification:

  CLAWCTL_PY=/tmp/promv/bin/python ops/check-metrics.sh

  Verify that agents reporting before upgrade have reported back within 120 seconds.
EOF
  exit 0
fi

echo
echo "⚠⚠ Candidate failed verification (see reason above). Automatically rolling back to $was"
stop_hub_for_database_move || {
  echo "⚠ Binary and DB left untouched; resolve running new Hub before rerunning controlled rollback" >&2
  exit 1
}
acquire_database_writer_lock || exit 1

# ⚠ 先把新版 executable 移出受控路徑並重驗 process=0，再隔離、還原，最後
# 才准啟動舊 binary。次序不能交換：仍可被 exec 的新版若碰到還原後的 ledger，
# 或舊 Hub 若碰到新版 schema，都可能做出 false-green。
FAILED_CANDIDATE_PROBE=''
if ! stage_executable_probe "$BIN" FAILED_CANDIDATE_PROBE; then
  fail_closed_binary_restore "automatic rollback failed to create private recovery probe for failed candidate" || true
  exit 1
fi
if ! move_installed_binary_to_dormant "$BIN.failed" "failed candidate binary"; then
  fail_closed_binary_restore "automatic rollback failed to isolate failed candidate $BIN" || true
  exit 1
fi
if ! assert_no_hub_process; then
  fail_closed_binary_restore "automatic rollback still sees Hub process after isolating candidate" || true
  exit 1
fi
if ! verify_stopped_hub_contract "$FAILED_CANDIDATE_PROBE" ||
  ! verify_upgrade_ledger_target "$FAILED_CANDIDATE_PROBE"; then
  fail_closed_binary_restore "automatic rollback stopped/ledger identity proof failed" || true
  exit 1
fi
cleanup_staged_binary_probes
if ! assert_no_hub_process; then
  fail_closed_binary_restore "automatic rollback private probe process has not exited" || true
  exit 1
fi
quarantine="$(mktemp -d "$BACKUPS/quarantine-$UPGRADE_STAMP-failed-$VERSION.XXXXXX")"
quarantine_failed_database "$quarantine"
restore_preupgrade_database "$PREUPGRADE_DB_BACKUP"

if [[ ! -f "$BIN.prev" ]]; then
  fail_closed_binary_restore "automatic rollback cannot find $BIN.prev (expected version $was)" || true
  exit 1
fi
if ! restore_verified_binary "$BIN.prev" "$was"; then
  exit 1
fi
# snapshot 已還原、舊 binary 已就位；marker 繼續跨過 start+verify。新版懂
# marker 就保持 read-only；legacy 即使忽略，失敗時 barrier 仍留給 recovery。
release_writer_lock
systemctl --user reset-failed "$UNIT" 2>/dev/null || true
# 到這一行時，$DB 才已經回到 UPGRADE_STAMP 的 pre-upgrade snapshot。
systemctl --user start "$UNIT" || true
sleep 3
if verify "$was" 1; then
  if ! durably_sync_path "$(dirname "$DB")" "verified automatic rollback ledger state" ||
    ! remove_upgrade_maintenance_marker; then
    fail_closed_started_recovery "automatic rollback restored $was" "$BIN.failed" || true
    exit 1
  fi
  echo "  Already rolled back to $was. Bad binary left in $BIN.failed"
  echo "  Database touched by new version preserved in $quarantine; service now reads pre-upgrade timestamp $UPGRADE_STAMP"
else
  fail_closed_started_recovery "automatic rollback restored $was" "$BIN.failed" || true
  echo "  journalctl --user -u clawctl-hub -n 80" >&2
  exit 1
fi
cat <<EOF

⚠ Automatic rollback restored the pre-upgrade standalone SQLite snapshot; old Hub did not open new ledger.
  Writes from failed startup were not deleted; originals preserved in 0700 quarantine:

    $quarantine

  journalctl --user -u clawctl-hub -n 80  <- check why it failed to start
EOF
exit 1
