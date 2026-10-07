#!/usr/bin/env bash

set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DEADMAN="$ROOT/ops/deadman.sh"
CHECK_METRICS="$ROOT/ops/check-metrics.sh"
UPGRADE_HUB="$ROOT/ops/upgrade-hub.sh"
INSTALL_HUB="$ROOT/ops/install-hub.sh"
STAGE_HUB_UNIT="$ROOT/ops/stage-hub-unit.sh"
SAFE_UPGRADE_LOCK="$ROOT/ops/safe-upgrade-lock.sh"
INSTALL_AGENT="$ROOT/ops/install-agent.sh"
INSTALL_AGENT_MACOS="$ROOT/ops/install-agent-macos.sh"
INSTALL_AGENT_WINDOWS="$ROOT/ops/install-agent-windows.ps1"
AGENT_TASK_XML="$ROOT/ops/clawctl-agent.task.xml"
BUILD_AGENT_BUNDLES="$ROOT/ops/build-agent-bundles.sh"
PUBLISH_AGENT_BUNDLES="$ROOT/ops/publish-agent-bundles.sh"
UPGRADE_AGENT="$ROOT/ops/upgrade-agent.sh"
AGENT_UNIT="$ROOT/ops/clawctl-agent.service"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

passed=0
failed=0
output=''
rc=0

# ⚠ 真告警在沒有通知指令時會寫 syslog；測試不該拿開發機當沙盒。
mkdir -p "$TMP/bin"
printf '#!/usr/bin/env bash\nexit 0\n' >"$TMP/bin/logger"
chmod +x "$TMP/bin/logger"

run() {
	if output=$("$@" 2>&1); then
		rc=0
	else
		rc=$?
	fi
}

pass() {
	passed=$((passed + 1))
	printf 'ok - %s\n' "$1"
}

fail() {
	failed=$((failed + 1))
	printf 'not ok - %s\n' "$1"
	printf '  exit: %s\n' "$rc"
	if [ -n "$output" ]; then
		printf '  output: %s\n' "$output"
	else
		printf '  output: <empty>\n'
	fi
}

expect() {
	local name=$1 expected_rc=$2
	shift 2
	if [ "$rc" -eq "$expected_rc" ] && "$@"; then
		pass "$name"
	else
		fail "$name"
	fi
}

is_silent() {
	[ -z "$output" ]
}

has() {
	case "$output" in
	*"$1"*) return 0 ;;
	*) return 1 ;;
	esac
}

lacks() {
	! has "$1"
}

has_both() {
	has "$1" && has "$2"
}

appears_before() {
	local first=$1 second=$2 file=$3
	awk -v first="$first" -v second="$second" '
		index($0, first) && first_at == 0 { first_at = NR }
		index($0, second) && second_at == 0 { second_at = NR }
		END { exit !(first_at > 0 && second_at > 0 && first_at < second_at) }
	' "$file"
}

appears_before_last() {
	local first=$1 second=$2 file=$3
	awk -v first="$first" -v second="$second" '
		index($0, first) && first_at == 0 { first_at = NR }
		index($0, second) { second_at = NR }
		END { exit !(first_at > 0 && second_at > 0 && first_at < second_at) }
	' "$file"
}

appears_in_order3() {
	local first=$1 second=$2 third=$3 file=$4
	awk -v first="$first" -v second="$second" -v third="$third" '
		stage == 0 && index($0, first) { stage = 1; next }
		stage == 1 && index($0, second) { stage = 2; next }
		stage == 2 && index($0, third) { stage = 3; exit }
		END { exit !(stage == 3) }
	' "$file"
}

function_body_has() {
	local name=$1 needle=$2 file=$3 body
	body="$(sed -n "/^${name}() {$/,/^}$/p" "$file")"
	[[ -n "$body" && "$body" == *"$needle"* ]]
}

function_body_lacks() {
	local name=$1 needle=$2 file=$3 body
	body="$(sed -n "/^${name}() {$/,/^}$/p" "$file")"
	[[ -n "$body" && "$body" != *"$needle"* ]]
}

function_body_appears_in_order3() {
	local name=$1 first=$2 second=$3 third=$4 file=$5 body
	body="$(sed -n "/^${name}() {$/,/^}$/p" "$file")"
	awk -v first="$first" -v second="$second" -v third="$third" '
		stage == 0 && index($0, first) { stage = 1; next }
		stage == 1 && index($0, second) { stage = 2; next }
		stage == 2 && index($0, third) { stage = 3; exit }
		END { exit !(stage == 3) }
	' <<<"$body"
}

# Run only upgrade-hub's verify() against deterministic command doubles. This
# exercises HTTP status/body, process ownership and restart gates without
# stopping the developer's real user service.
verify_fixture() {
	local metrics_code=$1 metrics_body=$2 optional=$3
	local health_code=${4:-200} health_body=${5:-alive}
	local home_code=${6:-200} home_body=${7:-'<html></html>'}
	local unit_active=${8:-1} restarts=${9:-0}
	local process_names=${10:-clawctl-hub}
	local writer_lock_rc=${11:-75}
	local verify_source process_count_source
	verify_source="$(sed -n '/^verify() {$/,/^}$/p' "$UPGRADE_HUB")"
	process_count_source="$(sed -n '/^hub_process_count() {$/,/^}$/p' "$UPGRADE_HUB")"
	process_count_source="${process_count_source/local proc_root=\"\/proc\"/local proc_root=\"$TMP\/empty-proc\"}"
	(
		URL=http://hub.invalid
		UNIT=clawctl-hub.service
		FAKE_METRICS_CODE=$metrics_code
		FAKE_METRICS_BODY=$metrics_body
		FAKE_HEALTH_CODE=$health_code
		FAKE_HEALTH_BODY=$health_body
		FAKE_HOME_CODE=$home_code
		FAKE_HOME_BODY=$home_body
		FAKE_UNIT_ACTIVE=$unit_active
		FAKE_RESTARTS=$restarts
		FAKE_PROCESS_NAMES=$process_names
		FAKE_WRITER_LOCK_RC=$writer_lock_rc
		WRITER_LOCK=/private/clawctl.sqlite.writer.lock
		ps() {
			local fake_pid=100 process_name
			while IFS= read -r process_name; do
				[[ -n "$process_name" ]] || continue
				printf '%s %s\n' "$fake_pid" "$process_name"
				fake_pid=$((fake_pid + 1))
			done <<<"$FAKE_PROCESS_NAMES"
		}
		systemctl() {
			if [[ "$*" == *' is-active '* ]]; then
				[[ $FAKE_UNIT_ACTIVE == 1 ]]
				return
			fi
			if [[ "$*" == *' show '* ]]; then
				printf '%s\n' "$FAKE_RESTARTS"
				return
			fi
			return 0
		}
		curl() {
			local output_file='' write_code=0 url='' arg
			while [[ $# -gt 0 ]]; do
				arg=$1
				shift
				case "$arg" in
				-o) output_file=$1; shift ;;
				-w) write_code=1; shift ;;
				http://*) url=$arg ;;
				esac
			done
			local code body
			case "$url" in
			*/healthz) code=$FAKE_HEALTH_CODE; body=$FAKE_HEALTH_BODY ;;
			*/metrics) code=$FAKE_METRICS_CODE; body=$FAKE_METRICS_BODY ;;
			*/) code=$FAKE_HOME_CODE; body=$FAKE_HOME_BODY ;;
			*) return 2 ;;
			esac
			if [[ -n "$output_file" ]]; then
				printf '%s' "$body" >"$output_file"
			else
				printf '%s' "$body"
			fi
			if [[ $write_code == 1 ]]; then
				printf '%s' "$code"
			fi
			return 0
		}
		acquire_writer_lock() {
			if [[ $FAKE_WRITER_LOCK_RC -eq 0 ]]; then
				WRITER_LOCK_FD=99
				return 0
			fi
			return "$FAKE_WRITER_LOCK_RC"
		}
		release_writer_lock() { unset WRITER_LOCK_FD; }
		eval "$process_count_source"
		eval "$verify_source"
		verify new-version "$optional"
	)
}

drain_fixture() {
	local process_names=$1 process_count_source drain_source
	process_count_source="$(sed -n '/^hub_process_count() {$/,/^}$/p' "$UPGRADE_HUB")"
	process_count_source="${process_count_source/local proc_root=\"\/proc\"/local proc_root=\"$TMP\/empty-proc\"}"
	drain_source="$(sed -n '/^assert_no_hub_process() {$/,/^}$/p' "$UPGRADE_HUB")"
	(
		UNIT=clawctl-hub.service
		FAKE_PROCESS_NAMES=$process_names
		ps() {
			local fake_pid=100 process_name
			while IFS= read -r process_name; do
				[[ -n "$process_name" ]] || continue
				printf '%s %s\n' "$fake_pid" "$process_name"
				fake_pid=$((fake_pid + 1))
			done <<<"$FAKE_PROCESS_NAMES"
		}
		systemctl() { return 1; }
		eval "$process_count_source"
		eval "$drain_source"
		assert_no_hub_process
	)
}

binary_restore_fixture() {
	local mode=$1 fixture fail_source restore_source dormant_source stage_source cleanup_source sync_source
	fixture="$(mktemp -d "$TMP/binary-restore-${mode}.XXXXXX")"
	fail_source="$(sed -n '/^fail_closed_binary_restore() {$/,/^}$/p' "$UPGRADE_HUB")"
	restore_source="$(sed -n '/^restore_verified_binary() {$/,/^restore_binary_after_snapshot_failure() {$/p' "$UPGRADE_HUB" | sed '$d')"
	dormant_source="$(sed -n '/^make_dormant_binary_non_executable() {$/,/^}$/p' "$UPGRADE_HUB")"
	stage_source="$(sed -n '/^stage_executable_probe() {$/,/^}$/p' "$UPGRADE_HUB")"
	cleanup_source="$(sed -n '/^cleanup_staged_binary_probes() {$/,/^}$/p' "$UPGRADE_HUB")"
	sync_source="$(sed -n '/^durably_sync_path() {$/,/^}$/p' "$UPGRADE_HUB")"
	(
		BIN="$fixture/clawctl-hub"
		STAGED_BINARY_PROBE_FDS=()
		local saved="$fixture/clawctl-hub.prev"
		printf '#!/usr/bin/env bash\nprintf "old-version\\n"\n' >"$BIN"
		chmod 0755 "$BIN"
		if [[ "$mode" != missing ]]; then
			printf '#!/usr/bin/env bash\nprintf "wanted-version\\n"\n' >"$saved"
			chmod 0755 "$saved"
		fi
		eval "$fail_source"
		eval "$cleanup_source"
		eval "$sync_source"
		eval "$dormant_source"
		eval "$stage_source"
		eval "$restore_source"
		assert_no_hub_process() { return 0; }
		if [[ "$mode" == install-failure ]]; then
			# This fixture targets the final atomic install.  The private-probe
			# primitive has its own structural assertions below, so make that
			# prerequisite deterministic here and fail only the publish copy.
			stage_executable_probe() {
				chmod 0700 -- "$1"
				printf -v "$2" '%s' "$1"
			}
			cleanup_staged_binary_probes() { :; }
			install() { return 73; }
		fi
		if restore_verified_binary "$saved" wanted-version; then
			return 1
		fi
		[[ "$("$BIN" version)" == old-version ]]
	)
}

private_probe_fixture() {
	local fixture dormant_source stage_source cleanup_source
	fixture="$(mktemp -d "$TMP/private-probe.XXXXXX")"
	dormant_source="$(sed -n '/^make_dormant_binary_non_executable() {$/,/^}$/p' "$UPGRADE_HUB")"
	stage_source="$(sed -n '/^stage_executable_probe() {$/,/^}$/p' "$UPGRADE_HUB")"
	cleanup_source="$(sed -n '/^cleanup_staged_binary_probes() {$/,/^}$/p' "$UPGRADE_HUB")"
	(
		BIN="$fixture/clawctl-hub"
		local source="$fixture/clawctl-hub.prev" PROBE='' got mode
		printf '#!/usr/bin/env bash\nprintf "probe-ok\\n"\n' >"$source"
		chmod 0755 "$source"
		STAGED_BINARY_PROBE_FDS=()
		eval "$cleanup_source"
		eval "$dormant_source"
		eval "$stage_source"
		stage_executable_probe "$source" PROBE
		mode="$(stat -c '%a' -- "$source")"
		[[ "$mode" == 600 ]]
		shopt -s nullglob
		local leftovers=("$fixture"/clawctl-hub.probe.*)
		shopt -u nullglob
		[[ ${#leftovers[@]} -eq 0 ]]
		got="$("$PROBE")"
		[[ "$got" == probe-ok ]]
		cleanup_staged_binary_probes
		if "$PROBE" >/dev/null 2>&1; then
			return 1
		fi
	)
}

# Execute the exact grant-preflight function with only systemd-run replaced by
# a deterministic command double. /usr/bin/timeout remains real, so non-zero
# child exits still exercise the set -e-sensitive capture path used in rollout.
operator_auth_preflight_fixture() {
	local mode=$1 source fake_dir fake_run args_file result
	fake_dir="$TMP/operator-auth-preflight"
	fake_run="$fake_dir/systemd-run"
	args_file="$fake_dir/args-$mode"
	mkdir -p "$fake_dir"
	printf '%s\n' \
		'#!/usr/bin/env bash' \
		'printf "%s\n" "$@" >"$FAKE_PREFLIGHT_ARGS"' \
		'case "$FAKE_PREFLIGHT_MODE" in' \
		'  success) printf "%s\n" "operator-auth-ready:v1 source=100.64.200.2 destination=100.64.200.2 authority=100.64.200.2:8787 capabilities=view,operate,admin self-only=true" ;;' \
		'  deny) printf "%s\n" "OPERATOR_CAPABILITY_REQUIRED" >&2; exit 42 ;;' \
		'  empty) exit 0 ;;' \
		'  wrong-version) printf "%s\n" "operator-auth-ready:v2 source=100.64.200.2 destination=100.64.200.2 authority=100.64.200.2:8787 capabilities=view,operate,admin self-only=true" ;;' \
		'  different-source) printf "%s\n" "operator-auth-ready:v1 source=100.100.10.20 destination=100.64.200.2 authority=100.64.200.2:8787 capabilities=view,operate,admin self-only=true" ;;' \
		'  multiline) printf "%s\n%s\n" "operator-auth-ready:v1 source=100.64.200.2 destination=100.64.200.2 authority=100.64.200.2:8787 capabilities=view,operate,admin self-only=true" "extra" ;;' \
		'  hang) sleep 5 ;;' \
		'  *) exit 99 ;;' \
		'esac' >"$fake_run"
	chmod +x "$fake_run"
	source="$(sed -n '/^run_operator_auth_preflight() {$/,/^}$/p' "$UPGRADE_HUB")"
	source="${source//\/usr\/bin\/systemd-run/$fake_run}"
	if [[ "$mode" == hang ]]; then
		source="${source/--kill-after=2s 15s/--kill-after=0.1s 0.2s}"
	fi
	(
		HUB_ENV="$TMP/hub env must stay opaque"
		LISTEN=''
		URL=''
		export FAKE_PREFLIGHT_MODE="$mode" FAKE_PREFLIGHT_ARGS="$args_file"
		eval "$source"
		if run_operator_auth_preflight /candidate/clawctl-hub.new; then
			result=0
		else
			result=$?
		fi
		printf '\nresolved-listen=%s resolved-url=%s\n' "$LISTEN" "$URL"
		exit "$result"
	)
}

systemd_unit_contract_fixture() {
	local mode=$1 source fixture
	source="$(sed -n '/^verify_systemd_unit_contract() {$/,/^}$/p' "$UPGRADE_HUB")"
	source="${source/\/usr\/bin\/timeout --kill-after=2s 10s \/usr\/bin\/systemctl/systemctl}"
	fixture="$TMP/systemd-contract-$mode"
	rm -rf "$fixture"
	mkdir -p "$fixture/home/.config/systemd/user" "$fixture/home/.config/clawctl" "$fixture/home/.local/bin"
	cp "$ROOT/ops/clawctl-hub.service" "$fixture/home/.config/systemd/user/clawctl-hub.service"
	if [[ "$mode" == disk-drift ]]; then
		printf '\n# unexplained local edit\n' >>"$fixture/home/.config/systemd/user/clawctl-hub.service"
	fi
	(
		HOME="$fixture/home"
		HUB_ENV="$fixture/home/.config/clawctl/hub.env"
		BIN="$fixture/home/.local/bin/clawctl-hub"
		DB="$fixture/home/.local/share/clawctl/clawctl.sqlite"
		UNIT=clawctl-hub.service
		FAKE_CONTRACT_MODE=$mode
		systemctl() {
			[[ "$FAKE_CONTRACT_MODE" != read-fail ]] || return 73
			local load_state=loaded fragment="$HOME/.config/systemd/user/clawctl-hub.service"
			local dropins='' need_reload=no envfiles="$HUB_ENV (ignore_errors=yes)"
			local environment='CLAWCTL_LISTEN=127.0.0.1:8787 CLAWCTL_OPERATOR_CAPABILITY_PREFIX= CLAWCTL_PUBLIC_URL= CLAWCTL_REPORT_AT=08:00'
			local unset_environment='' pam_name=''
			local exec_start="{ path=$BIN ; argv[]=$BIN --db $DB --listen \${CLAWCTL_LISTEN} --report-at \${CLAWCTL_REPORT_AT} --report-stamp \${CLAWCTL_REPORT_STAMP} ; ignore_errors=no ; }"
			case "$FAKE_CONTRACT_MODE" in
				not-loaded) load_state=not-found ;;
				wrong-fragment) fragment=/tmp/foreign.service ;;
				dropin) dropins=/tmp/override.conf ;;
				wrong-envfile) envfiles=/tmp/foreign.env ;;
				missing-default) environment='CLAWCTL_LISTEN=127.0.0.1:8787 CLAWCTL_OPERATOR_CAPABILITY_PREFIX=' ;;
				unset) unset_environment=CLAWCTL_PUBLIC_URL ;;
				pam) pam_name=login ;;
				wrong-exec) exec_start='{ path=/tmp/foreign ; argv[]=/tmp/foreign ; }' ;;
				wrong-db) exec_start="{ path=$BIN ; argv[]=$BIN --db /tmp/foreign.sqlite --listen \${CLAWCTL_LISTEN} --report-at \${CLAWCTL_REPORT_AT} --report-stamp \${CLAWCTL_REPORT_STAMP} ; ignore_errors=no ; }" ;;
				need-reload) need_reload=yes ;;
			esac
			printf 'LoadState=%s\nFragmentPath=%s\nDropInPaths=%s\nNeedDaemonReload=%s\nExecStart=%s\nEnvironment=%s\nEnvironmentFiles=%s\nUnsetEnvironment=%s\nPAMName=%s\n' \
				"$load_state" "$fragment" "$dropins" "$need_reload" "$exec_start" "$environment" "$envfiles" "$unset_environment" "$pam_name"
		}
		eval "$source"
		verify_systemd_unit_contract
	)
}

safe_upgrade_lock_fixture() {
	local mode=$1 fixture parent lock victim expected lock_rc=0
	fixture="$TMP/safe-lock-$mode"
	rm -rf "$fixture"
	mkdir -p "$fixture/parent"
	chmod 0700 "$fixture/parent"
	parent="$fixture/parent"
	lock="$parent/upgrade.lock"
	victim="$fixture/victim"
	expected="$fixture/expected"
	printf 'do-not-truncate-or-chmod\n' >"$victim"
	cp "$victim" "$expected"
	chmod 0640 "$victim"

	case "$mode" in
		symlink)
			ln -s "$victim" "$lock"
			;;
		hardlink)
			ln "$victim" "$lock"
			;;
		existing)
			cp "$victim" "$lock"
			chmod 0644 "$lock"
			;;
		parent-symlink)
			mkdir "$fixture/real-parent"
			chmod 0700 "$fixture/real-parent"
			ln -s "$fixture/real-parent" "$fixture/alias-parent"
			parent="$fixture/alias-parent"
			lock="$parent/upgrade.lock"
			;;
		group-writable-parent)
			chmod 0775 "$parent"
			;;
		group-readable-parent)
			chmod 0755 "$parent"
			;;
		*) return 99 ;;
	esac

	source "$SAFE_UPGRADE_LOCK"
	acquire_upgrade_lock "$lock" || lock_rc=$?
	if [[ $lock_rc -eq 0 ]]; then
		exec {UPGRADE_LOCK_FD}>&-
	fi
	case "$mode" in
		symlink|hardlink)
		[[ $lock_rc -eq 1 && "$(stat -c %a "$victim")" == 640 ]] && cmp -s -- "$victim" "$expected" || return 99
		return 1
		;;
		parent-symlink)
		[[ $lock_rc -eq 1 && ! -e "$fixture/real-parent/upgrade.lock" ]] || return 99
		return 1
		;;
		existing)
		[[ $lock_rc -eq 0 && "$(stat -c %a "$lock")" == 600 ]] && cmp -s -- "$lock" "$expected"
		;;
		group-writable-parent|group-readable-parent)
			[[ $lock_rc -eq 0 && "$(stat -c %a "$parent")" == 700 && -f "$lock" ]]
			;;
	esac
}

writer_lock_contention_fixture() {
	local fixture lock ready release holder lock_rc=0
	fixture="$TMP/writer-lock-contention"
	rm -rf "$fixture"
	mkdir -p "$fixture"
	chmod 0700 "$fixture"
	lock="$fixture/clawctl.sqlite.writer.lock"
	ready="$fixture/ready"
	release="$fixture/release"
	(
		exec 8<>"$lock"
		flock 8
		: >"$ready"
		while [[ ! -e "$release" ]]; do sleep 0.01; done
	) &
	holder=$!
	for _ in $(seq 1 100); do
		[[ -e "$ready" ]] && break
		sleep 0.01
	done
	source "$SAFE_UPGRADE_LOCK"
	acquire_writer_lock "$lock" || lock_rc=$?
	: >"$release"
	wait "$holder"
	[[ $lock_rc -eq 75 && -z "${WRITER_LOCK_FD:-}" ]]
}

stage_checked_in_unit_fixture() {
	local mode=$1 fixture source fake_systemctl
	fixture="$TMP/stage-function-$mode"
	rm -rf "$fixture"
	mkdir -p "$fixture"
	printf 'desired unit\n' >"$fixture/committed"
	cp "$fixture/committed" "$fixture/desired"
	printf 'old unit\n' >"$fixture/unit"
	fake_systemctl="$fixture/systemctl"
	printf '%s\n' \
		'#!/usr/bin/env bash' \
		'printf "%s\n" "$*" >>"$FAKE_STAGE_SYSTEMCTL_LOG"' \
		'[[ "$*" == "--user daemon-reload" ]] || exit 97' \
		'[[ "$FAKE_STAGE_SYSTEMCTL_MODE" != fail ]] || exit 73' >"$fake_systemctl"
	chmod +x "$fake_systemctl"
	source="$(sed -n '/^stage_checked_in_unit() {$/,/^}$/p' "$STAGE_HUB_UNIT")"
	source="${source//\/usr\/bin\/systemctl/$fake_systemctl}"
	(
		UNIT="$fixture/unit"
		DESIRED="$fixture/desired"
		before_pid=4321
		before_restarts=0
		before_invocation=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
		DESIRED_BLOB=fake-content-addressed-blob
		export FAKE_STAGE_SYSTEMCTL_LOG="$fixture/systemctl.log"
		FAKE_STAGE_SYSTEMCTL_MODE=success
		FAKE_STAGE_CONTRACT_MODE=stable
		export FAKE_STAGE_SYSTEMCTL_MODE FAKE_STAGE_CONTRACT_MODE
		read_loaded_contract() {
			local invocation=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa restarts=0 pid=4321
			case "$FAKE_STAGE_CONTRACT_MODE" in
				invocation-drift) invocation=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
				restart-invalid) restarts=not-a-number ;;
				pid-drift) pid=9876 ;;
			esac
			declare -gA LOADED=(
				[LoadState]=loaded
				[FragmentPath]="$UNIT"
				[DropInPaths]=''
				[NeedDaemonReload]=no
				[ActiveState]=active
				[MainPID]="$pid"
				[NRestarts]="$restarts"
				[InvocationID]="$invocation"
			)
		}
		git() {
			if [[ "$1" == cat-file && "$2" == blob && "$3" == "$DESIRED_BLOB" ]]; then
				command cat "$fixture/committed"
				return 0
			fi
			return 97
		}
		desired_blob_equals_file() {
			git cat-file blob "$DESIRED_BLOB" 2>/dev/null | cmp -s -- "$1" -
		}
		eval "$source"
		case "$mode" in
			success)
			stage_checked_in_unit
			;;
			worktree-drift)
			printf 'uncommitted bytes introduced after verification\n' >"$DESIRED"
			stage_checked_in_unit
			;;
			reload-fail)
			FAKE_STAGE_SYSTEMCTL_MODE=fail
			export FAKE_STAGE_SYSTEMCTL_MODE
			stage_checked_in_unit
			;;
			fail-rerun)
			FAKE_STAGE_SYSTEMCTL_MODE=fail
			export FAKE_STAGE_SYSTEMCTL_MODE
			local first_rc=0
			stage_checked_in_unit || first_rc=$?
			[[ $first_rc -eq 1 ]] || return 98
			cmp -s -- "$DESIRED" "$UNIT" || return 98
			FAKE_STAGE_SYSTEMCTL_MODE=success
			export FAKE_STAGE_SYSTEMCTL_MODE
			stage_checked_in_unit
			;;
			interrupt)
			mv() {
				command mv "$@"
				kill -TERM "$BASHPID"
			}
			stage_checked_in_unit
			;;
			invocation-drift|restart-invalid|pid-drift)
			FAKE_STAGE_CONTRACT_MODE="$mode"
			export FAKE_STAGE_CONTRACT_MODE
			stage_checked_in_unit
			;;
			*) return 99 ;;
		esac
	)
}

stage_repo_guard_fixture() {
	local mode=$1 fixture blob_source desired_source managed_source
	fixture="$TMP/stage-repo-$mode"
	rm -rf "$fixture"
	mkdir -p "$fixture/repo/ops"
	(
		cd "$fixture/repo" || exit 99
		git init -q
		printf 'old managed unit\n' >ops/clawctl-hub.service
		git add ops/clawctl-hub.service
		git -c user.name=Test -c user.email=test@example.invalid commit -qm old
		DESIRED="$fixture/repo/ops/clawctl-hub.service"
		UNIT="$fixture/installed.service"
		DESIRED_BLOB="$(git rev-parse --verify HEAD:ops/clawctl-hub.service)"
		blob_source="$(sed -n '/^desired_blob_equals_file() {$/,/^}$/p' "$STAGE_HUB_UNIT")"
		desired_source="$(sed -n '/^desired_unit_matches_head() {$/,/^}$/p' "$STAGE_HUB_UNIT")"
		managed_source="$(sed -n '/^installed_unit_is_managed() {$/,/^}$/p' "$STAGE_HUB_UNIT")"
		eval "$blob_source"
		eval "$desired_source"
		eval "$managed_source"
		case "$mode" in
			dirty-assume-unchanged)
			printf 'hidden dirty unit\n' >"$DESIRED"
			git update-index --assume-unchanged ops/clawctl-hub.service
			desired_unit_matches_head
			;;
			dirty-skip-worktree)
			printf 'hidden dirty unit\n' >"$DESIRED"
			git update-index --skip-worktree ops/clawctl-hub.service
			desired_unit_matches_head
			;;
			managed-history)
			cp "$DESIRED" "$UNIT"
			printf 'new managed unit\n' >"$DESIRED"
			git add ops/clawctl-hub.service
			git -c user.name=Test -c user.email=test@example.invalid commit -qm new
			DESIRED_BLOB="$(git rev-parse --verify HEAD:ops/clawctl-hub.service)"
			installed_unit_is_managed
			;;
			unmanaged)
			printf 'foreign customization\n' >"$UNIT"
			installed_unit_is_managed
			;;
			*) return 99 ;;
		esac
	)
}

file_absent() {
	[[ ! -e "$1" ]]
}

operator_auth_failure_handoff_fixture() {
	local fixture source
	fixture="$TMP/operator-auth-handoff"
	rm -rf "$fixture"
	mkdir -p "$fixture"
	printf 'candidate\n' >"$fixture/clawctl-hub.new"
	source="$(sed -n '/^echo "→ Preflight Tailscale operator grant/,/^stop_hub_for_database_move || exit 1$/p' "$UPGRADE_HUB")"
	(
		set -euo pipefail
		BIN="$fixture/clawctl-hub"
		run_operator_auth_preflight() { return 42; }
		stop_hub_for_database_move() { : >"$fixture/stop-called"; }
		eval "$source"
	)
}

operator_config_resolution_fixture() {
	local script=$1 mode=$2 source fixture
	fixture="$TMP/operator-config-resolution-$(basename "$script")-$mode"
	rm -rf "$fixture"
	mkdir -p "$fixture/home" "$fixture/xdg" "$fixture/real-xdg"
	chmod 0700 "$fixture/home" "$fixture/xdg" "$fixture/real-xdg"
	source="$(sed -n '/^resolve_operator_config_paths() {$/,/^}$/p' "$script")"
	(
		HOME="$fixture/home"
		OPERATOR_CONFIG_HOME=''
		OPERATOR_CONFIG_DIR=''
		OPERATOR_CONFIG=''
		case "$mode" in
			default) unset XDG_CONFIG_HOME ;;
			custom) XDG_CONFIG_HOME="$fixture/xdg" ;;
			relative) XDG_CONFIG_HOME='relative/config' ;;
			noncanonical) XDG_CONFIG_HOME="$fixture/xdg/../xdg" ;;
			symlink-root)
				rm -rf "$fixture/xdg"
				ln -s "$fixture/real-xdg" "$fixture/xdg"
				XDG_CONFIG_HOME="$fixture/xdg"
				;;
			*) return 99 ;;
		esac
		eval "$source"
		resolve_operator_config_paths || exit $?
		printf 'root=%s\ndir=%s\nfile=%s\n' \
			"$OPERATOR_CONFIG_HOME" "$OPERATOR_CONFIG_DIR" "$OPERATOR_CONFIG"
	)
}

operator_config_directory_fixture() {
	local script=$1 mode=$2 source fixture guard_rc=0
	fixture="$TMP/operator-config-directory-$(basename "$script")-$mode"
	rm -rf "$fixture"
	mkdir -p "$fixture"
	chmod 0700 "$fixture"
	if [[ "$script" == "$INSTALL_HUB" ]]; then
		source="$(sed -n '/^reject_unsafe_existing_directory() {$/,/^}$/p' "$script")
$(sed -n '/^ensure_owned_canonical_directory() {$/,/^}$/p' "$script")
$(sed -n '/^ensure_private_owned_directory() {$/,/^}$/p' "$script")
$(sed -n '/^ensure_operator_config_directory() {$/,/^}$/p' "$script")"
	else
		source="$(sed -n '/^ensure_operator_config_directory() {$/,/^}$/p' "$script")"
	fi
	OPERATOR_CONFIG_HOME="$fixture/xdg"
	OPERATOR_CONFIG_DIR="$OPERATOR_CONFIG_HOME/clawctl"
	case "$mode" in
		create) ;;
		symlink-final)
			mkdir "$OPERATOR_CONFIG_HOME" "$fixture/victim"
			chmod 0700 "$OPERATOR_CONFIG_HOME"
			chmod 0750 "$fixture/victim"
			ln -s "$fixture/victim" "$OPERATOR_CONFIG_DIR"
			;;
		writable-root)
			mkdir "$OPERATOR_CONFIG_HOME"
			chmod 0770 "$OPERATOR_CONFIG_HOME"
			;;
		*) return 99 ;;
	esac
	eval "$source"
	ensure_operator_config_directory || guard_rc=$?
	case "$mode" in
		create)
			[[ $guard_rc -eq 0 && ! -L "$OPERATOR_CONFIG_HOME" && ! -L "$OPERATOR_CONFIG_DIR" &&
				"$(stat -c '%a' "$OPERATOR_CONFIG_HOME")" == 700 &&
				"$(stat -c '%a' "$OPERATOR_CONFIG_DIR")" == 700 ]] || return 99
			;;
		symlink-final)
			[[ $guard_rc -eq 1 && -L "$OPERATOR_CONFIG_DIR" &&
				"$(stat -c '%a' "$fixture/victim")" == 750 ]] || return 99
			;;
		writable-root)
			[[ $guard_rc -eq 1 && "$(stat -c '%a' "$OPERATOR_CONFIG_HOME")" == 770 &&
				! -e "$OPERATOR_CONFIG_DIR" ]] || return 99
			;;
	esac
	return "$guard_rc"
}

install_private_directory_fixture() {
	local mode=$1 source fixture path guard_rc=0
	fixture="$TMP/install-private-directory-$mode"
	rm -rf "$fixture"
	mkdir -p "$fixture/parent"
	chmod 0700 "$fixture/parent"
	path="$fixture/parent/clawctl"
	source="$(sed -n '/^reject_unsafe_existing_directory() {$/,/^}$/p' "$INSTALL_HUB")
$(sed -n '/^ensure_private_owned_directory() {$/,/^}$/p' "$INSTALL_HUB")"
	case "$mode" in
		symlink)
			mkdir "$fixture/victim"
			chmod 0750 "$fixture/victim"
			ln -s "$fixture/victim" "$path"
			;;
		file)
			printf 'do-not-change\n' >"$path"
			chmod 0640 "$path"
			;;
		owned-directory)
			mkdir "$path"
			chmod 0755 "$path"
			;;
		*) return 99 ;;
	esac
	eval "$source"
	ensure_private_owned_directory "$path" 'test directory' || guard_rc=$?
	case "$mode" in
		symlink)
			[[ $guard_rc -eq 1 && -L "$path" && "$(stat -c '%a' "$fixture/victim")" == 750 ]] || return 99
			;;
		file)
			[[ $guard_rc -eq 1 && "$(stat -c '%a' "$path")" == 640 && "$(<"$path")" == do-not-change ]] || return 99
			;;
		owned-directory)
			[[ $guard_rc -eq 0 && -d "$path" && ! -L "$path" && "$(stat -c '%a' "$path")" == 700 ]] || return 99
			;;
	esac
	return "$guard_rc"
}

install_ancestor_symlink_fixture() {
	local ancestor=$1 source fixture path victim guard_rc=0
	fixture="$TMP/install-ancestor-symlink-$ancestor"
	rm -rf "$fixture"
	mkdir -p "$fixture/home" "$fixture/victim"
	chmod 0700 "$fixture/home"
	chmod 0750 "$fixture/victim"
	path="$fixture/home/.$ancestor"
	ln -s "$fixture/victim" "$path"
	source="$(sed -n '/^reject_unsafe_existing_directory() {$/,/^}$/p' "$INSTALL_HUB")"
	eval "$source"
	reject_unsafe_existing_directory "$path" 'install ancestor' || guard_rc=$?
	[[ $guard_rc -eq 1 && -L "$path" && "$(stat -c '%a' "$fixture/victim")" == 750 ]] || return 99
	case "$ancestor" in
		config) [[ ! -e "$fixture/victim/systemd" && ! -e "$fixture/victim/clawctl" ]] || return 99 ;;
		local) [[ ! -e "$fixture/victim/bin" && ! -e "$fixture/victim/share" ]] || return 99 ;;
		*) return 99 ;;
	esac
	return "$guard_rc"
}

agent_installer_complete_fixture() {
	local fixture="$TMP/agent-installer-complete" mock home token tailscale_key log install_agent system_unit_dir
	mock="$fixture/mock"
	home="$fixture/home"
	token="$fixture/explicit-token"
	tailscale_key="$fixture/tailscale-key"
	log="$fixture/calls"
	system_unit_dir="$fixture/systemd/system"
	rm -rf "$fixture"
	mkdir -p "$mock" "$home"
	chmod 0700 "$home"
	printf '%s\n' 'one-time-token' >"$token"
	printf '%s\n' 'tskey-auth-test' >"$tailscale_key"
	chmod 0600 "$token" "$tailscale_key"

	printf '%s\n' '#!/usr/bin/env bash' \
		'if [[ "$1" == "-v" ]]; then exit 0; fi' \
		'if [[ "$1" == "env" && "$2" == TARGET_USER=* ]]; then exit 0; fi' \
		'exec "$@"' >"$mock/sudo"
	printf '%s\n' '#!/usr/bin/env bash' \
		'printf "systemctl %s\n" "$*" >>"$AGENT_TEST_LOG"' \
		'case "$*" in' \
		'  *property=NRestarts*) echo 0 ;;' \
		'  *property=MainPID*) echo 4242 ;;' \
		'  *property=LoadState*) echo loaded ;;' \
		'  *property=FragmentPath*) echo "$AGENT_TEST_SYSTEM_UNIT/clawctl-agent.service" ;;' \
		'  *property=User*) echo "$AGENT_TEST_USER" ;;' \
		'  *property=ProtectSystem*) echo strict ;;' \
		'  *property=ProtectHome*) echo read-only ;;' \
		'esac' \
		'exit 0' >"$mock/systemctl"
	printf '%s\n' '#!/usr/bin/env bash' \
		'if [[ "$1" == "show-user" ]]; then echo yes; fi' \
		'exit 0' >"$mock/loginctl"
	printf '%s\n' '#!/usr/bin/env bash' \
		'if [[ "$1" == "ip" && "$2" == "-4" ]]; then' \
		'  [[ -f "$TAILSCALE_TEST_STATE" ]] || exit 1' \
		'  echo 100.64.0.77' \
		'elif [[ "$1" == "up" && "$2" == --auth-key=file:* ]]; then' \
		'  key_file=${2#--auth-key=file:}' \
		'  [[ "$(<"$key_file")" == tskey-auth-test ]] || exit 3' \
		'  echo tailscale-up >>"$TAILSCALE_TEST_LOG"' \
		'  touch "$TAILSCALE_TEST_STATE"' \
		'else' \
		'  exit 2' \
		'fi' >"$mock/tailscale"
	printf '%s\n' '#!/usr/bin/env bash' \
		'if [[ "$1" == "-o" && "$2" == "uid=" ]]; then echo "$AGENT_TEST_UID"; else echo clawctl-agent; fi' >"$mock/ps"
	printf '%s\n' '#!/usr/bin/env bash' 'echo true' >"$mock/systemd-run"
	printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$mock/podman"
	printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$mock/newuidmap"
	printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$mock/newgidmap"
	printf '%s\n' '#!/usr/bin/env bash' \
		'printf "%s\\n" "$*" >>"$AGENT_TEST_LOG"' \
		'case "$1" in' \
		'  version) echo test-version ;;' \
		'  enroll) ' \
		'    for ((i=1;i<=$#;i++)); do if [[ "${!i}" == "--token-file" ]]; then j=$((i+1)); [[ "$(cat -- "${!j}")" == "${EXPECTED_ENROLL_TOKEN:-one-time-token}" ]] || exit 4; fi; done ' \
		'    if [[ "$FAIL_ENROLL" == "1" ]]; then exit 1; fi ' \
		'    hub=""; for ((i=1;i<=$#;i++)); do if [[ "${!i}" == "--hub" ]]; then j=$((i+1)); hub="${!j}"; break; fi; done ' \
		'    mkdir -p "$HOME/.config/clawctl"; printf "{\"hub_url\":\"%s\"}\\n" "$hub" >"$HOME/.config/clawctl/agent.json"; chmod 0600 "$HOME/.config/clawctl/agent.json" ;;' \
		'  verify) ' \
		'    if [[ "${FAIL_VERIFY:-}" == "1" ]]; then exit 1; fi ' \
		'    echo "Hub ready: machine_id=test checkin=2026-09-10T18:00:00Z jobs=enabled agent=test-version" ;;' \
		'  *) exit 1 ;;' \
		'esac' >"$fixture/clawctl-agent"
	chmod 0755 "$mock/sudo" "$mock/systemctl" "$mock/loginctl" "$mock/tailscale" "$mock/ps" \
		"$mock/systemd-run" "$mock/podman" "$mock/newuidmap" "$mock/newgidmap" \
		"$fixture/clawctl-agent"
	cp "$AGENT_UNIT" "$fixture/clawctl-agent.service"
	cp "$ROOT/ops/clawctl-hermes.service" "$fixture/clawctl-hermes.service"
	cp "$ROOT/ops/openclaw-gateway.service" "$fixture/openclaw-gateway.service"
	install_agent="$fixture/install-agent.sh"
	sed "s#/usr/bin/podman#$mock/podman#g" "$INSTALL_AGENT" >"$install_agent"
	chmod 0755 "$install_agent"

	HOME="$home" USER="fixture-user" AGENT_TEST_LOG="$log" TAILSCALE_TEST_LOG="$log" \
		TAILSCALE_TEST_STATE="$fixture/tailscale-state" PATH="$mock:$PATH" \
		CLAWCTL_SYSTEM_UNIT_DIR="$system_unit_dir" AGENT_TEST_SYSTEM_UNIT="$system_unit_dir" \
		AGENT_TEST_USER="$(id -un)" AGENT_TEST_UID="$EUID" \
		"$install_agent" --hub http://100.64.0.1:8787 --token-file "$token" \
		--tailscale-auth-key-file "$tailscale_key" --binary "$fixture/clawctl-agent" || return
	[[ -f "$system_unit_dir/clawctl-agent.service" ]] || return 91
	[[ ! -e "$home/.config/systemd/user/clawctl-agent.service" ]] || return 98
	grep -Fq "User=$(id -un)" "$system_unit_dir/clawctl-agent.service" || return 99
	! grep -Fq 'CLAWCTL_AGENT_' "$system_unit_dir/clawctl-agent.service" || return 100
	[[ -f "$home/.config/systemd/user/clawctl-hermes.service" ]] || return 96
	[[ -f "$home/.config/systemd/user/openclaw-gateway.service" ]] || return 97
	[[ -x "$home/.local/bin/clawctl-agent" ]] || return 92
	[[ -d "$home/.local/share/clawctl/hermes/data" && -d "$home/.local/share/containers" ]] || return 97
	grep -Fq 'enroll --hub http://100.64.0.1:8787 --token-file' "$log" || return 93
	grep -Fq 'verify --hub http://100.64.0.1:8787 --since ' "$log" || return 94
	grep -Fxq 'tailscale-up' "$log" || return 95

	# Re-enrollment Case 1: existing config same hub -> skip enroll
	rm -f "$log"
	HOME="$home" USER="fixture-user" AGENT_TEST_LOG="$log" TAILSCALE_TEST_LOG="$log" \
		TAILSCALE_TEST_STATE="$fixture/tailscale-state" PATH="$mock:$PATH" \
		CLAWCTL_SYSTEM_UNIT_DIR="$system_unit_dir" AGENT_TEST_SYSTEM_UNIT="$system_unit_dir" \
		AGENT_TEST_USER="$(id -un)" AGENT_TEST_UID="$EUID" \
		"$install_agent" --hub http://100.64.0.1:8787 --binary "$fixture/clawctl-agent" || return 101
	grep -q 'enroll --hub' "$log" && return 102

	# Re-enrollment Case 2: existing config different hub without flag -> fails with hint
	rm -f "$log"
	if HOME="$home" USER="fixture-user" AGENT_TEST_LOG="$log" TAILSCALE_TEST_LOG="$log" \
		TAILSCALE_TEST_STATE="$fixture/tailscale-state" PATH="$mock:$PATH" \
		CLAWCTL_SYSTEM_UNIT_DIR="$system_unit_dir" AGENT_TEST_SYSTEM_UNIT="$system_unit_dir" \
		AGENT_TEST_USER="$(id -un)" AGENT_TEST_UID="$EUID" \
		"$install_agent" --hub http://100.64.0.2:8787 --binary "$fixture/clawctl-agent" >"$fixture/out2" 2>&1; then
		return 103
	fi
	grep -q 'enrolled to a different Hub' "$fixture/out2" || return 104

	# Re-enrollment Case 3: --reenroll success
	rm -f "$log"
	HOME="$home" USER="fixture-user" AGENT_TEST_LOG="$log" TAILSCALE_TEST_LOG="$log" \
		TAILSCALE_TEST_STATE="$fixture/tailscale-state" PATH="$mock:$PATH" \
		CLAWCTL_SYSTEM_UNIT_DIR="$system_unit_dir" AGENT_TEST_SYSTEM_UNIT="$system_unit_dir" \
		AGENT_TEST_USER="$(id -un)" AGENT_TEST_UID="$EUID" \
		"$install_agent" --hub http://100.64.0.2:8787 --reenroll --token-file "$token" --binary "$fixture/clawctl-agent" || return 105
	grep -q 'systemctl stop clawctl-agent.service' "$log" || return 106
	local backups=("$home"/.config/clawctl/agent.json.pre-reenroll-*)
	[[ ${#backups[@]} -eq 1 ]] || return 107
	[[ "$(stat -c '%a' "${backups[0]}")" == "600" ]] || return 108
	grep -q '100.64.0.1' "${backups[0]}" || return 109
	grep -q '100.64.0.2' "$home/.config/clawctl/agent.json" || return 110

	# Re-enrollment Case 4: --reenroll with failing enroll stub -> original restored
	rm -f "$log"
	rm -f "$home"/.config/clawctl/agent.json.pre-reenroll-*
	cp "$home/.config/clawctl/agent.json" "$fixture/agent.before"
	if FAIL_ENROLL=1 HOME="$home" USER="fixture-user" AGENT_TEST_LOG="$log" TAILSCALE_TEST_LOG="$log" \
		TAILSCALE_TEST_STATE="$fixture/tailscale-state" PATH="$mock:$PATH" \
		CLAWCTL_SYSTEM_UNIT_DIR="$system_unit_dir" AGENT_TEST_SYSTEM_UNIT="$system_unit_dir" \
		AGENT_TEST_USER="$(id -un)" AGENT_TEST_UID="$EUID" \
		"$install_agent" --hub http://100.64.0.3:8787 --reenroll --token-file "$token" --binary "$fixture/clawctl-agent" >"$fixture/out4" 2>&1; then
		return 111
	fi
	grep -qi 'previous configuration restored' "$fixture/out4" || return 112
	grep -q "old Hub's install-agent.sh" "$fixture/out4" || return 123
	grep -q '100.64.0.2' "$home/.config/clawctl/agent.json" || return 113
	cmp -s "$home/.config/clawctl/agent.json" "$fixture/agent.before" || return 114
	local restored_backups=("$home"/.config/clawctl/agent.json.pre-reenroll-*)
	[[ ! -e "${restored_backups[0]}" ]] || return 115

	# Re-enrollment Case 5: --reenroll when existing config points at the SAME hub still re-enrolls
	rm -f "$log"
	rm -f "$home"/.config/clawctl/agent.json.pre-reenroll-*
	HOME="$home" USER="fixture-user" AGENT_TEST_LOG="$log" TAILSCALE_TEST_LOG="$log" \
		TAILSCALE_TEST_STATE="$fixture/tailscale-state" PATH="$mock:$PATH" \
		CLAWCTL_SYSTEM_UNIT_DIR="$system_unit_dir" AGENT_TEST_SYSTEM_UNIT="$system_unit_dir" \
		AGENT_TEST_USER="$(id -un)" AGENT_TEST_UID="$EUID" \
		"$install_agent" --hub http://100.64.0.2:8787 --reenroll --token-file "$token" --binary "$fixture/clawctl-agent" || return 116
	grep -q 'systemctl stop clawctl-agent.service' "$log" || return 117
	grep -Fq 'enroll --hub http://100.64.0.2:8787 --token-file' "$log" || return 118
	local same_hub_backups=("$home"/.config/clawctl/agent.json.pre-reenroll-*)
	[[ ${#same_hub_backups[@]} -eq 1 ]] || return 119
	[[ "$(stat -c '%a' "${same_hub_backups[0]}")" == "600" ]] || return 120
	grep -q '100.64.0.2' "${same_hub_backups[0]}" || return 121
	grep -q '100.64.0.2' "$home/.config/clawctl/agent.json" || return 122

	# Re-enrollment Case 6: --reenroll failure after successful re-enrollment prints backup path
	rm -f "$log"
	rm -f "$home"/.config/clawctl/agent.json.pre-reenroll-*
	if FAIL_VERIFY=1 HOME="$home" USER="fixture-user" AGENT_TEST_LOG="$log" TAILSCALE_TEST_LOG="$log" \
		TAILSCALE_TEST_STATE="$fixture/tailscale-state" PATH="$mock:$PATH" \
		CLAWCTL_SYSTEM_UNIT_DIR="$system_unit_dir" AGENT_TEST_SYSTEM_UNIT="$system_unit_dir" \
		AGENT_TEST_USER="$(id -un)" AGENT_TEST_UID="$EUID" \
		"$install_agent" --hub http://100.64.0.3:8787 --reenroll --token-file "$token" --binary "$fixture/clawctl-agent" >"$fixture/out6" 2>&1; then
		return 124
	fi
	local verify_fail_backups=("$home"/.config/clawctl/agent.json.pre-reenroll-*)
	[[ ${#verify_fail_backups[@]} -eq 1 ]] || return 125
	[[ -f "${verify_fail_backups[0]}" ]] || return 126
	grep -Fq "Previous configuration backed up at: ${verify_fail_backups[0]} (see docs/MOVE-AGENTS.md, Rollback)" "$fixture/out6" || return 127

	# Keyed package cases use the same full installer stubs and re-enroll path.
	keyed_fixture_run() {
		HOME="$home" USER="fixture-user" AGENT_TEST_LOG="$log" TAILSCALE_TEST_LOG="$log" \
			TAILSCALE_TEST_STATE="$fixture/tailscale-state" PATH="$mock:$PATH" \
			CLAWCTL_SYSTEM_UNIT_DIR="$system_unit_dir" AGENT_TEST_SYSTEM_UNIT="$system_unit_dir" \
			AGENT_TEST_USER="$(id -un)" AGENT_TEST_UID="$EUID" \
			"$install_agent" "$@" >"$fixture/keyed-output" 2>&1
	}
	printf '%s\r\n' 'https://hub.example.com' >"$fixture/hub-url"
	printf '%s\n' 'one-time-token' >"$fixture/enroll-token"
	chmod 0644 "$fixture/enroll-token"
	if keyed_fixture_run --reenroll; then return 130; fi
	grep -Fq 'chmod 600 enroll-token' "$fixture/keyed-output" || return 131
	[[ -f "$fixture/enroll-token" ]] || return 132

	chmod 0600 "$fixture/enroll-token"
	rm -f "$log"
	keyed_fixture_run --reenroll || return 133
	grep -Fq "enroll --hub https://hub.example.com --token-file $fixture/enroll-token" "$log" || return 134
	grep -Fq 'Tailscale: skipped (Hub reached over HTTPS)' "$fixture/keyed-output" || return 135
	grep -Fq 'tailscale=skipped' "$fixture/keyed-output" || return 136
	grep -Fq 'Embedded enroll-token removed' "$fixture/keyed-output" || return 137
	[[ ! -e "$fixture/enroll-token" ]] || return 138
	! grep -q 'tailscale' "$log" || return 139
	! grep -q 'one-time-token' "$fixture/keyed-output" || return 140

	# Explicit Hub and token-file override even invalid/insecure embedded files.
	printf '%s\n' 'invalid-hub' >"$fixture/hub-url"
	printf '%s\n' 'unused-embedded-secret' >"$fixture/enroll-token"
	chmod 0644 "$fixture/enroll-token"
	rm -f "$log"
	keyed_fixture_run --reenroll --hub http://100.64.0.4:8787 --token-file "$token" --no-tailscale || return 141
	grep -Fq "enroll --hub http://100.64.0.4:8787 --token-file $token" "$log" || return 142
	grep -Fq 'Tailscale: skipped (--no-tailscale)' "$fixture/keyed-output" || return 143
	! grep -q 'tailscale' "$log" || return 144
	[[ -f "$fixture/enroll-token" ]] || return 145

	# Explicit --token also overrides the embedded file and never appears in output.
	rm -f "$log"
	EXPECTED_ENROLL_TOKEN=explicit-secret keyed_fixture_run --reenroll --hub https://override.example.com --token explicit-secret || return 146
	grep -Fq 'enroll --hub https://override.example.com --token-file' "$log" || return 147
	! grep -q 'explicit-secret' "$fixture/keyed-output" "$log" || return 148
	[[ -f "$fixture/enroll-token" ]] || return 149

	# Failed enrollment retains the embedded secret and restores the old config.
	printf '%s\n' 'https://hub.example.com' >"$fixture/hub-url"
	printf '%s\n' 'one-time-token' >"$fixture/enroll-token"
	chmod 0600 "$fixture/enroll-token"
	cp "$home/.config/clawctl/agent.json" "$fixture/keyed-before"
	if FAIL_ENROLL=1 keyed_fixture_run --reenroll; then return 150; fi
	[[ -f "$fixture/enroll-token" ]] || return 151
	cmp -s "$home/.config/clawctl/agent.json" "$fixture/keyed-before" || return 152

	# Fresh install consumes the embedded files without flags.
	rm -f "$home/.config/clawctl/agent.json" "$log"
	keyed_fixture_run || return 153
	[[ ! -e "$fixture/enroll-token" ]] || return 154
	grep -Fq 'enroll --hub https://hub.example.com --token-file' "$log" || return 155
	unset -f keyed_fixture_run

}

macos_agent_installer_complete_fixture() {
	local fixture="$TMP/macos-agent-installer-complete" mock home token log install_macos real_stat
	mock="$fixture/mock"
	home="$fixture/home"
	token="$fixture/enroll-token"
	log="$fixture/calls"
	real_stat=$(command -v stat)
	mkdir -p "$mock" "$home"
	chmod 0700 "$home"
	printf '%s\n' 'one-time-token' >"$token"
	chmod 0600 "$token"

	printf '%s\n' '#!/usr/bin/env bash' \
		'case "$1" in' \
		'  -s) echo Darwin ;;' \
		'  -m) echo arm64 ;;' \
		'  *) echo Darwin ;;' \
		'esac' >"$mock/uname"
	printf '%s\n' '#!/usr/bin/env bash' \
		'if [[ "$1" == "-f" && "$2" == "%u" ]]; then shift 2; exec '"$real_stat"' -c %u "$@"; fi' \
		'if [[ "$1" == "-f" && "$2" == "%Lp" ]]; then shift 2; exec '"$real_stat"' -c %a "$@"; fi' \
		'exec '"$real_stat"' "$@"' >"$mock/stat"
	printf '%s\n' '#!/usr/bin/env bash' \
		'printf "%s\n" "$*" >>"$LAUNCHCTL_TEST_LOG"' \
		'case "$1" in' \
		'  bootout) [[ -f "$LAUNCHCTL_TEST_STATE" ]] || exit 1 ;;' \
		'  bootstrap) touch "$LAUNCHCTL_TEST_STATE" ;;' \
		'esac' \
		'exit 0' >"$mock/launchctl"
	printf '%s\n' '#!/usr/bin/env bash' \
		'printf "%s\n" "$*" >>"$AGENT_TEST_LOG"' \
		'case "$1" in' \
		'  version) echo test-version ;;' \
		'  enroll) mkdir -p "$HOME/Library/Application Support/clawctl"; printf "{}\n" >"$HOME/Library/Application Support/clawctl/agent.json"; chmod 0600 "$HOME/Library/Application Support/clawctl/agent.json" ;;' \
		'  verify) echo "Hub ready: fixture" ;;' \
		'  *) exit 1 ;;' \
		'esac' >"$fixture/clawctl-agent"
	chmod 0755 "$mock/uname" "$mock/stat" "$mock/launchctl" "$fixture/clawctl-agent"
	cp "$ROOT/ops/clawctl-agent.plist" "$fixture/clawctl-agent.plist"
	install_macos="$fixture/install-agent-macos.sh"
	cp "$INSTALL_AGENT_MACOS" "$install_macos"
	chmod 0755 "$install_macos"

	HOME="$home" USER="fixture-user" AGENT_TEST_LOG="$log" \
		LAUNCHCTL_TEST_LOG="$log" LAUNCHCTL_TEST_STATE="$fixture/launchd-state" \
		PATH="$mock:$PATH" "$install_macos" \
		--hub http://100.64.0.1:8787 --token-file "$token" \
		--binary "$fixture/clawctl-agent" || return
	[[ -f "$home/Library/LaunchAgents/com.clawctl.agent.plist" ]] || return 91
	! grep -Fq '@@' "$home/Library/LaunchAgents/com.clawctl.agent.plist" || return 92
	grep -Fq "<string>$home/.local/bin/clawctl-agent</string>" "$home/Library/LaunchAgents/com.clawctl.agent.plist" || return 93
	grep -Fq "<string>$home/Library/Logs/clawctl/agent.log</string>" "$home/Library/LaunchAgents/com.clawctl.agent.plist" || return 94
	[[ -x "$home/.local/bin/clawctl-agent" ]] || return 95
	[[ -f "$home/Library/Application Support/clawctl/agent.json" ]] || return 96
	grep -Fq 'enroll --hub http://100.64.0.1:8787 --token-file' "$log" || return 97
	grep -Fq 'verify --hub http://100.64.0.1:8787 --since ' "$log" || return 98
	grep -Fq 'bootstrap gui/' "$log" || return 99
	grep -Fq 'kickstart -k gui/' "$log" || return 100
}

macos_installer_rejects_linux_fixture() {
	local fixture="$TMP/macos-installer-rejects-linux" mock
	mock="$fixture/mock"
	mkdir -p "$mock"
	printf '%s\n' '#!/usr/bin/env bash' '[[ "$1" == "-s" ]] && echo Linux || echo arm64' >"$mock/uname"
	chmod 0755 "$mock/uname"
	HOME="$fixture/home" USER="fixture-user" PATH="$mock:$PATH" \
		"$INSTALL_AGENT_MACOS" --hub http://100.64.0.1:8787
}

linux_installer_names_macos_fixture() {
	local fixture="$TMP/linux-installer-names-macos" mock
	mock="$fixture/mock"
	mkdir -p "$mock"
	printf '%s\n' '#!/usr/bin/env bash' '[[ "$1" == "-s" ]] && echo Darwin || echo arm64' >"$mock/uname"
	chmod 0755 "$mock/uname"
	HOME="$fixture/home" USER="fixture-user" PATH="$mock:$PATH" \
		"$INSTALL_AGENT" --hub http://100.64.0.1:8787
}

make_publisher_bundle_fixture() {
	local source_dir=$1 arch=$2 version=$3 payload=${4:-one} stage="$1/stage-$2"
	local machine
	rm -rf "$stage"
	mkdir -p "$stage"
	printf '%s\n' "$version" >"$stage/VERSION"
	case "$arch" in
		amd64) machine='\076\000' ;;
		arm64) machine='\267\000' ;;
		*) return 99 ;;
	esac
	{
		printf '\177ELF\002\001'
		printf '\000%.0s' {1..12}
		printf '%b%s' "$machine" "$payload"
	} >"$stage/clawctl-agent"
	printf '[Unit]\nDescription=fixture\n' >"$stage/clawctl-agent.service"
	printf '[Unit]\nDescription=Hermes fixture\n' >"$stage/clawctl-hermes.service"
	printf '[Unit]\nDescription=OpenClaw fixture\n' >"$stage/openclaw-gateway.service"
	printf '#!/usr/bin/env bash\nprintf %s\n' "$payload" >"$stage/install-agent.sh"
	chmod 0644 "$stage/VERSION" "$stage/clawctl-agent.service" "$stage/clawctl-hermes.service" "$stage/openclaw-gateway.service"
	chmod 0755 "$stage/clawctl-agent" "$stage/install-agent.sh"
	tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner \
		-cf - -C "$stage" VERSION clawctl-agent clawctl-agent.service clawctl-hermes.service install-agent.sh openclaw-gateway.service |
		gzip -n >"$source_dir/clawctl-agent-bootstrap-linux-$arch.tar.gz"
}

non_elf_agent_bundle_publisher_fixture() {
	local fixture="$TMP/agent-bundle-publisher-non-elf"
	local source_dir="$fixture/source" state_dir="$fixture/state" version=fixture-non-elf
	local stage="$source_dir/stage-amd64"
	mkdir -p "$source_dir" "$state_dir"
	chmod 0700 "$state_dir"
	make_publisher_bundle_fixture "$source_dir" amd64 "$version" one || return
	make_publisher_bundle_fixture "$source_dir" arm64 "$version" one || return
	{
		printf '\317\372\355\376'
		printf '\000%.0s' {1..16}
	} >"$stage/clawctl-agent"
	chmod 0755 "$stage/clawctl-agent"
	tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner -cf - -C "$stage" \
		VERSION clawctl-agent clawctl-agent.service clawctl-hermes.service install-agent.sh openclaw-gateway.service \
		| gzip -n >"$source_dir/clawctl-agent-bootstrap-linux-amd64.tar.gz"
	"$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir"
}

wrong_arch_agent_bundle_publisher_fixture() {
	local fixture="$TMP/agent-bundle-publisher-wrong-arch"
	local source_dir="$fixture/source" state_dir="$fixture/state" version=fixture-wrong-arch
	mkdir -p "$source_dir" "$state_dir"
	chmod 0700 "$state_dir"
	make_publisher_bundle_fixture "$source_dir" amd64 "$version" one || return
	make_publisher_bundle_fixture "$source_dir" arm64 "$version" one || return
	# 檔名仍是 amd64，但 payload 的 ELF e_machine 明確標成 arm64。
	make_publisher_bundle_fixture "$source_dir" amd64 "$version" one || return
	printf '\267\000' | dd of="$source_dir/stage-amd64/clawctl-agent" bs=1 seek=18 conv=notrunc status=none || return
	tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner -cf - -C "$source_dir/stage-amd64" \
		VERSION clawctl-agent clawctl-agent.service clawctl-hermes.service install-agent.sh openclaw-gateway.service \
		| gzip -n >"$source_dir/clawctl-agent-bootstrap-linux-amd64.tar.gz"
	"$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir"
}

agent_bundle_publisher_fixture() {
	local fixture="$TMP/agent-bundle-publisher" source_dir state_dir version=publisher-test
	rm -rf "$fixture"
	mkdir -p "$fixture/source" "$fixture/state"
	chmod 0700 "$fixture/state"
	source_dir="$fixture/source"
	state_dir="$fixture/state"
	make_publisher_bundle_fixture "$source_dir" amd64 "$version" one || return
	make_publisher_bundle_fixture "$source_dir" arm64 "$version" one || return
	"$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir" || return
	"$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir" || return
	local release="$state_dir/agent-bootstrap/$version"
	[[ -f "$release/clawctl-agent-bootstrap-linux-amd64.tar.gz" &&
	   -f "$release/clawctl-agent-bootstrap-linux-arm64.tar.gz" &&
	   -f "$release/SHA256SUMS" && "$(stat -c '%a' "$release")" == 700 ]] || return 91
	make_publisher_bundle_fixture "$source_dir" amd64 "$version" two || return
	if "$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir"; then
		return 92
	fi
	return 0
}

make_darwin_publisher_bundle_fixture() {
	local source_dir=$1 arch=$2 version=$3 defect=${4:-valid} stage="$1/darwin-$2" machine
	mkdir -p "$stage"
	printf '%s\n' "$version" >"$stage/VERSION"
	case "$arch" in
		amd64) machine='\007\000\000\001' ;;
		arm64) machine='\014\000\000\001' ;;
	esac
	{
		printf '\317\372\355\376%b\000\000\000\000\002\000\000\000' "$machine"
		printf '\000%.0s' {1..16}
	} >"$stage/clawctl-agent"
	cp "$ROOT/ops/clawctl-agent.plist" "$stage/clawctl-agent.plist"
	cp "$INSTALL_AGENT_MACOS" "$stage/install-agent-macos.sh"
	case "$defect" in
		wrong-arch) printf '\000\000\000\000' | dd of="$stage/clawctl-agent" bs=1 seek=4 conv=notrunc status=none ;;
		non-executable) printf '\006\000\000\000' | dd of="$stage/clawctl-agent" bs=1 seek=12 conv=notrunc status=none ;;
		elf) printf '\177ELF' | dd of="$stage/clawctl-agent" bs=1 conv=notrunc status=none ;;
		plist) printf '[Unit]\n' >"$stage/clawctl-agent.plist" ;;
		installer) printf '#!/bin/sh\nexit 0\n' >"$stage/install-agent-macos.sh" ;;
		version) printf 'another-version\n' >"$stage/VERSION" ;;
	esac
	chmod 0644 "$stage/VERSION" "$stage/clawctl-agent.plist"
	chmod 0755 "$stage/clawctl-agent" "$stage/install-agent-macos.sh"
	tar --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner -cf - -C "$stage" \
		VERSION clawctl-agent clawctl-agent.plist install-agent-macos.sh |
		gzip -n >"$source_dir/clawctl-agent-bootstrap-darwin-$arch.tar.gz"
}

darwin_publisher_fixture() {
	local mode=$1 fixture="$TMP/darwin-publisher-$1" version=darwin-publisher
	local source_dir="$fixture/source" state_dir="$fixture/state" temp_dir="$fixture/tmp" release
	mkdir -p "$source_dir" "$state_dir" "$temp_dir"
	chmod 0700 "$state_dir"
	make_publisher_bundle_fixture "$source_dir" amd64 "$version" || return
	make_publisher_bundle_fixture "$source_dir" arm64 "$version" || return
	make_darwin_publisher_bundle_fixture "$source_dir" amd64 "$version" "$mode" || return
	if [[ "$mode" != partial ]]; then
		make_darwin_publisher_bundle_fixture "$source_dir" arm64 "$version" || return
	fi
	if [[ "$mode" == symlink ]]; then
		mv "$source_dir/clawctl-agent-bootstrap-darwin-arm64.tar.gz" "$source_dir/linked.tar.gz"
		ln -s linked.tar.gz "$source_dir/clawctl-agent-bootstrap-darwin-arm64.tar.gz"
	fi
	release="$state_dir/agent-bootstrap/$version"
	if [[ "$mode" == valid || "$mode" == membership || "$mode" == changed || "$mode" == existing-partial ]]; then
		TMPDIR="$temp_dir" "$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir" || return
		[[ "$(wc -l <"$release/SHA256SUMS")" == 4 ]] || return 91
		(cd "$release" && sha256sum -c SHA256SUMS) || return
		TMPDIR="$temp_dir" "$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir" || return
		local before
		before=$(sha256sum "$release"/*)
		case "$mode" in
			membership) rm "$source_dir"/clawctl-agent-bootstrap-darwin-*.tar.gz ;;
			changed)
				printf '\001' | dd of="$source_dir/darwin-amd64/clawctl-agent" bs=1 seek=20 conv=notrunc status=none
				tar --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner -cf - -C "$source_dir/darwin-amd64" \
					VERSION clawctl-agent clawctl-agent.plist install-agent-macos.sh |
					gzip -n >"$source_dir/clawctl-agent-bootstrap-darwin-amd64.tar.gz"
				;;
			existing-partial)
				rm "$release/clawctl-agent-bootstrap-darwin-arm64.tar.gz"
				before=$(sha256sum "$release"/*)
				;;
		esac
		if [[ "$mode" != valid ]]; then
			if TMPDIR="$temp_dir" "$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir"; then return 92; fi
			[[ "$(sha256sum "$release"/*)" == "$before" ]] || return 93
		fi
	else
		if TMPDIR="$temp_dir" "$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir"; then return 94; fi
		[[ ! -e "$release" ]] || return 95
	fi
	[[ -z "$(ls -A "$temp_dir")" ]] || return 96
}

make_windows_publisher_bundle_fixture() {
	local source_dir=$1 arch=$2 version=$3 defect=${4:-valid} stage="$1/windows-$2" machine
	mkdir -p "$stage"
	printf '%s\n' "$version" >"$stage/VERSION"
	case "$arch" in
		amd64) machine='\144\206' ;;
		arm64) machine='\144\252' ;;
	esac
	{
		printf 'MZ'
		printf '\000%.0s' {1..58}
		printf '\200\000\000\000'
		printf '\000%.0s' {1..64}
		printf 'PE\000\000%b' "$machine"
		printf '\000%.0s' {1..16}
		printf '\002\000'
	} >"$stage/clawctl-agent.exe"
	cp "$AGENT_TASK_XML" "$stage/clawctl-agent.task.xml"
	cp "$INSTALL_AGENT_WINDOWS" "$stage/install-agent-windows.ps1"
	case "$defect" in
		wrong-arch) printf '\000\000' | dd of="$stage/clawctl-agent.exe" bs=1 seek=132 conv=notrunc status=none ;;
		non-executable) printf '\000\000' | dd of="$stage/clawctl-agent.exe" bs=1 seek=150 conv=notrunc status=none ;;
		dll) printf '\002\040' | dd of="$stage/clawctl-agent.exe" bs=1 seek=150 conv=notrunc status=none ;;
		elf) printf '\177ELF' | dd of="$stage/clawctl-agent.exe" bs=1 conv=notrunc status=none ;;
		task) printf '[Unit]\n' >"$stage/clawctl-agent.task.xml" ;;
		installer) printf '#!/bin/sh\nexit 0\n' >"$stage/install-agent-windows.ps1" ;;
		version) printf 'another-version\n' >"$stage/VERSION" ;;
	esac
	chmod 0644 "$stage/VERSION" "$stage/clawctl-agent.task.xml"
	chmod 0755 "$stage/clawctl-agent.exe" "$stage/install-agent-windows.ps1"
	tar --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner -cf - -C "$stage" \
		VERSION clawctl-agent.exe clawctl-agent.task.xml install-agent-windows.ps1 |
		gzip -n >"$source_dir/clawctl-agent-bootstrap-windows-$arch.tar.gz"
}

windows_publisher_fixture() {
	local mode=$1 fixture="$TMP/windows-publisher-$1" version=windows-publisher
	local source_dir="$fixture/source" state_dir="$fixture/state" temp_dir="$fixture/tmp" release
	mkdir -p "$source_dir" "$state_dir" "$temp_dir"
	chmod 0700 "$state_dir"
	make_publisher_bundle_fixture "$source_dir" amd64 "$version" || return
	make_publisher_bundle_fixture "$source_dir" arm64 "$version" || return
	make_windows_publisher_bundle_fixture "$source_dir" amd64 "$version" "$mode" || return
	if [[ "$mode" != partial ]]; then
		make_windows_publisher_bundle_fixture "$source_dir" arm64 "$version" || return
	fi
	if [[ "$mode" == symlink ]]; then
		mv "$source_dir/clawctl-agent-bootstrap-windows-arm64.tar.gz" "$source_dir/linked.tar.gz"
		ln -s linked.tar.gz "$source_dir/clawctl-agent-bootstrap-windows-arm64.tar.gz"
	fi
	release="$state_dir/agent-bootstrap/$version"
	if [[ "$mode" == valid || "$mode" == membership || "$mode" == changed || "$mode" == existing-partial ]]; then
		TMPDIR="$temp_dir" "$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir" || return
		[[ "$(wc -l <"$release/SHA256SUMS")" == 4 ]] || return 91
		(cd "$release" && sha256sum -c SHA256SUMS) || return
		TMPDIR="$temp_dir" "$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir" || return
		local before
		before=$(sha256sum "$release"/*)
		case "$mode" in
			membership) rm "$source_dir"/clawctl-agent-bootstrap-windows-*.tar.gz ;;
			changed)
				printf '\001' | dd of="$source_dir/windows-amd64/clawctl-agent.exe" bs=1 seek=20 conv=notrunc status=none
				tar --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner -cf - -C "$source_dir/windows-amd64" \
					VERSION clawctl-agent.exe clawctl-agent.task.xml install-agent-windows.ps1 |
					gzip -n >"$source_dir/clawctl-agent-bootstrap-windows-amd64.tar.gz"
				;;
			existing-partial)
				rm "$release/clawctl-agent-bootstrap-windows-arm64.tar.gz"
				before=$(sha256sum "$release"/*)
				;;
		esac
		if [[ "$mode" != valid ]]; then
			if TMPDIR="$temp_dir" "$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir"; then return 92; fi
			[[ "$(sha256sum "$release"/*)" == "$before" ]] || return 93
		fi
	else
		if TMPDIR="$temp_dir" "$PUBLISH_AGENT_BUNDLES" --version "$version" --source-dir "$source_dir" --state-dir "$state_dir"; then return 94; fi
		[[ ! -e "$release" ]] || return 95
	fi
	[[ -z "$(ls -A "$temp_dir")" ]] || return 96
}

# --- Hub rollback：舊 binary 不得在看不懂新版 evidence ledger 時直接起來。
run bash -n "$UPGRADE_HUB"
expect 'upgrade-hub shell syntax valid' 0 is_silent

run bash -n "$INSTALL_HUB"
expect 'install-hub shell syntax valid' 0 is_silent

run bash -n "$INSTALL_AGENT"
expect 'install-agent shell syntax valid' 0 is_silent

run bash -n "$BUILD_AGENT_BUNDLES"
expect 'agent bundle builder shell syntax valid' 0 is_silent

run bash -n "$PUBLISH_AGENT_BUNDLES"
expect 'agent bundle publisher shell syntax valid' 0 is_silent

run agent_bundle_publisher_fixture
expect 'agent bundle publisher atomically admits one immutable complete release' 0 has 'different bytes'
run non_elf_agent_bundle_publisher_fixture
expect 'agent bundle publisher does not ship macOS binary as Linux agent' 1 has 'Invalid Agent ELF'
run wrong_arch_agent_bundle_publisher_fixture
expect 'agent bundle publisher does not ship arm64 binary as amd64' 1 has 'Agent architecture mismatch'

for darwin_case in valid partial wrong-arch non-executable elf plist installer version symlink membership changed existing-partial; do
	run darwin_publisher_fixture "$darwin_case"
	expect "Darwin publisher validates immutable release: $darwin_case" 0 true
done

for windows_case in valid partial wrong-arch non-executable dll elf task installer version symlink membership changed existing-partial; do
	run windows_publisher_fixture "$windows_case"
	expect "Windows publisher validates immutable release: $windows_case" 0 true
done

run bash -n "$UPGRADE_AGENT"
expect 'upgrade-agent shell syntax valid' 0 is_silent

run grep -Fq 'build -buildvcs=false -trimpath' "$ROOT/Makefile"
expect 'release binaries exclude ambient VCS state from reproducible bytes' 0 is_silent

run grep -Fq 'ReadWritePaths=@@CLAWCTL_AGENT_HOME@@/.config/clawctl @@CLAWCTL_AGENT_HOME@@/.config/systemd/user/openclaw-gateway.service.d @@CLAWCTL_AGENT_HOME@@/.cache/clawctl @@CLAWCTL_AGENT_HOME@@/.local/share/clawctl @@CLAWCTL_AGENT_HOME@@/.openclaw' "$AGENT_UNIT"
expect 'agent unit grants the exact managed write roots for catalog activation' 0 is_silent

run grep -Fq 'User=@@CLAWCTL_AGENT_USER@@' "$AGENT_UNIT"
expect 'agent unit renders an explicit unprivileged service user' 0 is_silent

run grep -Fq 'WantedBy=multi-user.target' "$AGENT_UNIT"
expect 'agent unit is enabled by the system manager target' 0 is_silent

run appears_before 'ensure_owned_directory "$RUNTIME_DIR" 0700' 'sudo systemctl restart clawctl-agent.service' "$INSTALL_AGENT"
expect 'agent install creates the private runtime root before service start' 0 is_silent

run appears_in_order3 'STEP="Tailscale install"' '"$BIN_FILE" enroll --hub "$HUB" --token-file "$TOKEN_FILE"' 'sudo systemctl restart clawctl-agent.service' "$INSTALL_AGENT"
expect 'agent install connects the dependency plane before enrollment and service start' 0 is_silent

run appears_in_order3 'https://tailscale.com/install.sh' 'sudo systemctl enable --now tailscaled.service' 'sudo tailscale up --auth-key="file:$TAILSCALE_KEY_FILE"' "$INSTALL_AGENT"
expect 'agent install provisions and starts Tailscale before joining the tailnet' 0 is_silent

run grep -Fq 'sudo tailscale up --auth-key="file:$TAILSCALE_KEY_FILE"' "$INSTALL_AGENT"
expect 'agent install keeps the Tailscale auth key out of argv' 0 is_silent

run appears_before 'sudo loginctl enable-linger "$AGENT_USER"' 'sudo systemctl restart clawctl-agent.service' "$INSTALL_AGENT"
expect 'agent install enables persistence before service start' 0 is_silent

run appears_in_order3 'sudo systemctl reset-failed clawctl-agent.service' 'sudo systemctl restart clawctl-agent.service' '"$BIN_FILE" verify --hub "$HUB" --since "$SERVICE_STARTED_AT" --timeout 2m' "$INSTALL_AGENT"
expect 'agent install resets the restart gate before start and requires a fresh Hub receipt' 0 is_silent

run appears_in_order3 'sudo systemctl daemon-reload' 'systemctl --user disable --now clawctl-agent.service' 'sudo systemctl restart clawctl-agent.service' "$INSTALL_AGENT"
expect 'agent install loads the system unit before retiring the legacy user service' 0 is_silent

run appears_before '[[ "$AGENT_COUNT" == "1" ]]' 'echo "Managed: agent=$AGENT_VERSION tailscale=$TAILSCALE_IP service=active jobs=enabled"' "$INSTALL_AGENT"
expect 'agent install publishes success only after exact process validation' 0 is_silent

run grep -E '⚠|temporary|暫時|disclaimer|可能缺少|可能需要' "$INSTALL_AGENT"
expect 'agent installer contains no defensive or temporary interface copy' 1 is_silent

run agent_installer_complete_fixture
expect 'agent installer completes Tailscale, enrollment, service and Hub receipt in one run' 0 has 'Managed: agent=test-version tailscale=100.64.0.77 service=active jobs=enabled'

run macos_agent_installer_complete_fixture
expect 'macOS install script substitutes all placeholders in launchd plist' 0 true

run macos_installer_rejects_linux_fixture
expect 'macOS install script indicates which script to run on Linux' 1 has 'On Linux run ./install-agent.sh'

run linux_installer_names_macos_fixture
expect 'Linux install script indicates which script to run on macOS' 1 has 'ops/install-agent-macos.sh'

run appears_in_order3 'got="$(timeout 5 "$bin.new" version' 'sudo mv -f "$system_unit.new" "$system_unit"' 'sudo systemctl restart clawctl-agent.service' "$UPGRADE_AGENT"
expect 'agent upgrade validates binary before publishing unit and restarting' 0 is_silent

run appears_before 'Managed agent preflight failed: $target' 'scp -q -o BatchMode=yes "$source"' "$UPGRADE_AGENT"
expect 'agent upgrade validates the enrolled service account before transfer' 0 is_silent

run grep -Fq 'writable_roots="$(systemctl show clawctl-agent.service -p ReadWritePaths --value)"' "$UPGRADE_AGENT"
expect 'agent upgrade verifies the live writable-root contract' 0 is_silent

run appears_in_order3 'sudo systemctl reset-failed clawctl-agent.service' 'sudo systemctl restart clawctl-agent.service' '"$bin" verify --since "$started_at" --timeout 2m' "$UPGRADE_AGENT"
expect 'agent upgrade requires a fresh Hub readiness receipt' 0 is_silent

run appears_in_order3 'sudo systemctl daemon-reload' 'systemctl --user disable --now clawctl-agent.service' 'sudo systemctl restart clawctl-agent.service' "$UPGRADE_AGENT"
expect 'agent upgrade migrates from the user manager without overlapping agents' 0 is_silent

run appears_before '[[ "$restarts" == "0" ]]' 'echo "Managed: agent=$running_version service=active jobs=enabled processes=1 restarts=0"' "$UPGRADE_AGENT"
expect 'agent upgrade publishes success only after the zero-restart gate' 0 is_silent

run grep -E '⚠|temporary|暫時|disclaimer|可能缺少|可能需要' "$UPGRADE_AGENT"
expect 'agent upgrade contains no defensive or temporary interface copy' 1 is_silent

run appears_before 'ensure_private_owned_directory "$CONFIG_DIR"' 'systemctl --user enable --now clawctl-hub.service' "$INSTALL_HUB"
expect 'clean install creates private config and writer-lock parents before Hub starts' 0 is_silent

run awk '
	/# --- 5\. / { in_directories=1; next }
	/# --- 6\. / { in_directories=0 }
	in_directories && /reject_unsafe_existing_directory "\$CONFIG_DIR"/ && !config_guard { config_guard=NR }
	in_directories && /reject_unsafe_existing_directory "\$STATE_DIR"/ && !state_guard { state_guard=NR }
	in_directories && /ensure_(owned|private|operator)_/ && !first_write { first_write=NR }
	END { exit !(config_guard > 0 && state_guard > 0 && first_write > config_guard && first_write > state_guard) }
' "$INSTALL_HUB"
expect 'installer rejects existing config/state targets before its first directory create or chmod' 0 is_silent

run grep -F 'mkdir -p "$BIN_DIR"' "$INSTALL_HUB"
expect 'installer never follows HOME-relative ancestors with recursive mkdir' 1 is_silent

run install_private_directory_fixture symlink
expect 'installer directory guard rejects a final symlink without chmodding its target' 1 has 'symlink'

run install_private_directory_fixture file
expect 'installer directory guard rejects a non-directory without changing its bytes or mode' 1 has 'directory'

run install_private_directory_fixture owned-directory
expect 'installer directory guard converges an owned canonical directory to 0700' 0 is_silent

run function_body_has reject_unsafe_existing_directory '! -O "$path"' "$INSTALL_HUB"
expect 'installer directory guard explicitly rejects a directory not owned by the caller' 0 is_silent

run install_ancestor_symlink_fixture config
expect 'installer rejects a symlinked HOME config ancestor before creating children through it' 1 has 'symlink'

run install_ancestor_symlink_fixture local
expect 'installer rejects a symlinked HOME local ancestor before creating children through it' 1 has 'symlink'

untrusted_install_home="$TMP/install-caller-controlled-home"
mkdir -p "$untrusted_install_home"
run env HOME="$untrusted_install_home" XDG_CONFIG_HOME="$untrusted_install_home/xdg" \
	bash "$INSTALL_HUB" --listen 100.64.0.1:8787 --operator-capability-prefix example.test/cap/clawctl
expect 'installer rejects caller-controlled HOME before resolving installation paths' 1 has_both 'canonical home' 'no installation paths created or modified'
run file_absent "$untrusted_install_home/.local"
expect 'installer HOME rejection creates no local lifecycle tree' 0 is_silent
run file_absent "$untrusted_install_home/xdg"
expect 'installer HOME rejection creates no XDG operator tree' 0 is_silent

for operator_script in "$INSTALL_HUB" "$UPGRADE_HUB"; do
	operator_name="$(basename "$operator_script")"
	run operator_config_resolution_fixture "$operator_script" default
	expect "$operator_name defaults operator discovery to HOME/.config" 0 has "file=$TMP/operator-config-resolution-$operator_name-default/home/.config/clawctl/operator.json"

	run operator_config_resolution_fixture "$operator_script" custom
	expect "$operator_name honors absolute canonical XDG_CONFIG_HOME" 0 has "file=$TMP/operator-config-resolution-$operator_name-custom/xdg/clawctl/operator.json"

	run operator_config_resolution_fixture "$operator_script" relative
	expect "$operator_name rejects relative XDG_CONFIG_HOME" 1 has 'canonical absolute'

	run operator_config_resolution_fixture "$operator_script" noncanonical
	expect "$operator_name rejects lexically non-canonical XDG_CONFIG_HOME" 1 has 'canonical absolute'

	run operator_config_resolution_fixture "$operator_script" symlink-root
	expect "$operator_name rejects a symlinked XDG_CONFIG_HOME" 1 has 'canonical absolute'

	run operator_config_directory_fixture "$operator_script" create
	expect "$operator_name safely creates a private custom XDG clawctl directory" 0 is_silent

	run operator_config_directory_fixture "$operator_script" symlink-final
	expect "$operator_name rejects a final XDG clawctl symlink without chmodding its target" 1 has 'symlink'

	run operator_config_directory_fixture "$operator_script" writable-root
	expect "$operator_name rejects a group-writable XDG config root before creating clawctl" 1 has 'group/other'
done

run appears_in_order3 'if [[ "$operator_ok" -ne 1 ]]' 'OPERATOR_TMP="$(mktemp' 'CLI operator discovery' "$INSTALL_HUB"
expect 'installer writes discovery only after the authenticated operator probe' 0 is_silent

run grep -F 'mv -fT -- "$OPERATOR_TMP" "$OPERATOR_CONFIG"' "$INSTALL_HUB"
expect 'installer atomically replaces the exact operator config path' 0 has 'mv -fT'

run appears_in_order3 'run_operator_auth_preflight "$BIN.new"' 'write_operator_discovery' 'stop_hub_for_database_move || exit 1' "$UPGRADE_HUB"
expect 'upgrade pins discovered authority after grant proof and before stopping Hub' 0 is_silent

run appears_in_order3 'make hub agent-bundles agent-bundles-darwin agent-bundles-windows VERSION="$VERSION"' './ops/publish-agent-bundles.sh --version "$VERSION"' 'stop_hub_for_database_move || exit 1' "$UPGRADE_HUB"
expect 'Hub upgrade publishes matching Agent bundles before stopping the live Hub' 0 is_silent

run appears_before '"$HERE/publish-agent-bundles.sh" --version "$BUNDLE_VERSION"' 'systemctl --user enable --now clawctl-hub.service' "$INSTALL_HUB"
expect 'Hub install publishes matching Agent bundles before starting the service' 0 is_silent

run grep -Fq "'ops/publish-agent-bundles.sh'" "$UPGRADE_HUB"
expect 'Hub dirty-version calculation includes the bundle publisher' 0 is_silent

run bash -n "$STAGE_HUB_UNIT"
expect 'stage-hub-unit shell syntax valid' 0 is_silent

run bash -n "$SAFE_UPGRADE_LOCK"
expect 'shared lifecycle lock shell syntax valid' 0 is_silent

run safe_upgrade_lock_fixture symlink
expect 'lifecycle lock rejects a symlink without truncating or chmodding its target' 1 has 'symlink/hardlink'

run safe_upgrade_lock_fixture hardlink
expect 'lifecycle lock rejects a hardlink without changing its shared inode' 1 has 'symlink/hardlink'

run safe_upgrade_lock_fixture parent-symlink
expect 'lifecycle lock rejects a non-canonical symlink parent before creating a lock' 1 has 'canonical non-symlink'

run safe_upgrade_lock_fixture existing
expect 'lifecycle lock preserves existing bytes while making the verified inode private' 0 is_silent

run safe_upgrade_lock_fixture group-writable-parent
expect 'owned group-writable legacy state directory converges to 0700 before lifecycle locking' 0 is_silent

run safe_upgrade_lock_fixture group-readable-parent
expect 'owned group-readable legacy state directory converges to 0700 before Go writer locking' 0 is_silent

run writer_lock_contention_fixture
expect 'shell writer lock fails closed when another ledger writer owns the inode' 0 is_silent

run grep -E 'systemctl --user (start|stop|restart|try-restart)' "$STAGE_HUB_UNIT"
expect 'unit staging has no code path that starts, stops or restarts Hub' 1 is_silent

run grep -F 'systemctl --user daemon-reload' "$STAGE_HUB_UNIT"
expect 'unit staging asks upstream systemd to reload the atomic file' 0 has 'daemon-reload'

run grep -F '"${LOADED[MainPID]}" != "$before_pid"' "$STAGE_HUB_UNIT"
expect 'unit staging rejects any live PID change' 0 has 'before_pid'

run grep -F 'git show "$revision:ops/clawctl-hub.service"' "$STAGE_HUB_UNIT"
expect 'unit staging refuses to overwrite files outside tracked unit history' 0 has 'git show'

run stage_checked_in_unit_fixture success
expect 'unit staging atomically reloads an exact contract without changing process identity' 0 has 'no restart'

run stage_checked_in_unit_fixture worktree-drift
expect 'unit staging reads the captured content-addressed HEAD blob after verification' 0 has 'no restart'
run cmp -s -- "$TMP/stage-function-worktree-drift/committed" "$TMP/stage-function-worktree-drift/unit"
expect 'a worktree TOCTOU change cannot become the staged unit bytes' 0 is_silent

run stage_checked_in_unit_fixture reload-fail
expect 'daemon-reload failure reports a monotonic rerun path and returns non-zero' 1 has_both 'left on disk' 'Rerun'
run cmp -s -- "$TMP/stage-function-reload-fail/desired" "$TMP/stage-function-reload-fail/unit"
expect 'daemon-reload failure leaves the desired unit on disk instead of rolling backward' 0 is_silent

run stage_checked_in_unit_fixture fail-rerun
expect 'rerunning after daemon-reload failure converges without a service restart' 0 has_both 'Rerun' 'no restart'

run stage_checked_in_unit_fixture interrupt
expect 'interruption immediately after atomic mv exits with the signal status' 143 is_silent
run cmp -s -- "$TMP/stage-function-interrupt/desired" "$TMP/stage-function-interrupt/unit"
expect 'post-mv interruption keeps the desired unit as the monotonic recovery point' 0 is_silent
run file_absent "$TMP/stage-function-interrupt/systemctl.log"
expect 'post-mv interruption cannot claim or attempt daemon-reload' 0 is_silent

run stage_checked_in_unit_fixture invocation-drift
expect 'unit staging rejects a changed systemd InvocationID even if PID and counter match' 1 has 'loaded contract'

run stage_checked_in_unit_fixture restart-invalid
expect 'unit staging rejects a non-numeric NRestarts property' 1 has 'loaded contract'

run stage_checked_in_unit_fixture pid-drift
expect 'unit staging rejects a changed MainPID after daemon-reload' 1 has 'loaded contract'

run stage_repo_guard_fixture dirty-assume-unchanged
expect 'HEAD blob comparison catches dirty unit hidden by assume-unchanged' 1 is_silent

run stage_repo_guard_fixture dirty-skip-worktree
expect 'HEAD blob comparison catches dirty unit hidden by skip-worktree' 1 is_silent

run stage_repo_guard_fixture managed-history
expect 'unit staging recognizes an installed byte-exact historical repo revision' 0 is_silent

run stage_repo_guard_fixture unmanaged
expect 'unit staging rejects an installed unit absent from repo history' 1 is_silent

good_metrics='clawctl_build_info{version="new-version"} 1'
run verify_fixture 200 "$good_metrics" 0
expect 'all six Hub verification gates accept an exact healthy candidate' 0 is_silent

run verify_fixture 200 "$good_metrics" 0 200 alive 200 '<html></html>' 1 0 clawctl-hub 0
expect 'candidate without a process-lifetime writer lock cannot pass verification' 1 has 'does not hold'

run verify_fixture 200 "$good_metrics" 1 200 alive 200 '<html></html>' 1 0 clawctl-hub 0
expect 'legacy rollback explicitly discloses its relaxed writer-lock gate' 0 has 'legacy recovery'

run verify_fixture 500 "$good_metrics" 0
expect 'metrics HTTP 500 cannot pass with a forged correct build_info body' 1 has 'HTTP 500'

run verify_fixture 404 '' 0
expect 'candidate metrics 404 is a hard failure' 1 has 'version evidence'

run verify_fixture 404 '' 1
expect 'explicit legacy verification may disclose and tolerate metrics 404' 0 has 'This version has no /metrics'

duplicate_metrics="$good_metrics
$good_metrics"
run verify_fixture 200 "$duplicate_metrics" 0
expect 'duplicate build_info samples fail the unique version evidence gate' 1 has 'count=2'

run verify_fixture 200 "$good_metrics" 0 500 alive
expect 'healthz HTTP 500 cannot pass with body alive' 1 has 'healthz'

run verify_fixture 200 "$good_metrics" 0 200 alive 500 '<html></html>'
expect 'homepage HTTP 500 cannot pass with a complete-looking body' 1 has 'Homepage'

run verify_fixture 200 "$good_metrics" 0 200 alive 200 '<html></html>' 0
expect 'a manually started Hub cannot pass while the systemd unit is inactive' 1 has 'systemd'

run verify_fixture 200 "$good_metrics" 0 200 alive 200 '<html></html>' 1 0 $'clawctl-hub\nclawctl-hub.pre'
expect 'verification counts renamed Hub binaries as a second process' 1 has '2 clawctl-hub'

run drain_fixture 'clawctl-hub.pre'
expect 'database drain rejects a renamed Hub binary' 1 has 'not cleanly stopped'

run grep -F 'check_rollback_compatibility "$PREV_PROBE"' "$UPGRADE_HUB"
expect 'manual rollback requires previous binary capability through a private probe' 0 has 'check_rollback_compatibility'

run appears_in_order3 'check_rollback_compatibility "$BIN" "current version $current"' 'check_rollback_compatibility "$PREV_PROBE" "previous version $prev"' 'begin_upgrade_maintenance "$BIN"' "$UPGRADE_HUB"
expect 'manual rollback checks forward-only work with current binary before consulting previous binary' 0 is_silent

run grep -F 'restore_binary_after_snapshot_failure "$BIN.failed" "$current" "$rollback_snapshot"' "$UPGRADE_HUB"
expect 'manual rollback snapshot failure enters the fail-closed restore path' 0 has 'restore_binary_after_snapshot_failure'

run grep -F 'restore_binary_after_snapshot_failure "$BIN.prev" "$was" "$PREUPGRADE_DB_BACKUP"' "$UPGRADE_HUB"
expect 'upgrade snapshot failure enters the fail-closed restore path' 0 has 'restore_binary_after_snapshot_failure'

run function_body_lacks restore_binary_after_snapshot_failure remove_upgrade_maintenance_marker "$UPGRADE_HUB"
expect 'snapshot failure path cannot remove the maintenance marker' 0 is_silent

run function_body_lacks restore_binary_after_snapshot_failure restart_current_after_refusal "$UPGRADE_HUB"
expect 'snapshot failure path cannot restart the Hub' 0 is_silent

run function_body_has restore_binary_after_snapshot_failure 'DB/WAL/SHM' "$UPGRADE_HUB"
expect 'snapshot failure gives explicit DB and sidecar recovery guidance' 0 is_silent

run function_body_has stage_executable_probe 'install -m 0600 -- "$source" "$probe"' "$UPGRADE_HUB"
expect 'binary recovery stages a private verified executable' 0 is_silent

run function_body_has stage_executable_probe 'rm -- "$probe"' "$UPGRADE_HUB"
expect 'private binary probes unlink their pathname before becoming executable' 0 is_silent

run private_probe_fixture
expect 'an unlinked fd-backed private binary probe executes and disappears on cleanup' 0 is_silent

run grep -F 'mv -fT -- "$restore_tmp" "$BIN"' "$UPGRADE_HUB"
expect 'binary recovery replaces the installed path atomically only after staging' 0 has 'restore_tmp'

run grep -F 'installed version' "$UPGRADE_HUB"
expect 'binary recovery rechecks the installed executable before marker removal' 0 has 'installed version'

run binary_restore_fixture install-failure
expect 'binary install failure leaves the installed path unchanged and fails closed' 0 has 'install'

run binary_restore_fixture missing
expect 'missing saved binary leaves the installed path unchanged and fails closed' 0 has 'cannot find'

run grep -F 'cp "$BIN.prev" "$BIN"' "$UPGRADE_HUB"
expect 'automatic rollback cannot silently continue after an unchecked binary copy' 1 is_silent

run grep -F 'fail_closed_binary_restore "automatic rollback cannot find $BIN.prev' "$UPGRADE_HUB"
expect 'automatic rollback missing previous binary is explicitly fail closed' 0 has 'fail_closed_binary_restore'

run grep -F 'restore_binary_after_handoff_refusal "$BIN.failed" "$current"' "$UPGRADE_HUB"
expect 'post-snapshot legacy capability refusal may use the recoverable handoff path' 0 has 'restore_binary_after_handoff_refusal'

run grep -F 'verify "$VERSION" 0' "$UPGRADE_HUB"
expect 'candidate verification requires metrics evidence' 0 has 'verify "$VERSION" 0'

run grep -F -- '--operator-auth-check' "$UPGRADE_HUB"
expect 'upgrade invokes the candidate-owned LocalAPI grant preflight' 0 has '--operator-auth-check'

run appears_in_order3 'if run_operator_auth_preflight "$BIN.new"; then' 'echo "→ Stopping Hub"' 'create_rollback_snapshot "$CANDIDATE_PROBE"' "$UPGRADE_HUB"
expect 'operator grant preflight runs before Hub stop and database snapshot' 0 is_silent

run operator_auth_preflight_fixture success
expect 'systemd-owned grant preflight accepts one exact v1 self-only sentinel' 0 has_both 'resolved-listen=100.64.200.2:8787' 'resolved-url=http://100.64.200.2:8787'

run grep -Fx -- '--operator-auth-check' "$TMP/operator-auth-preflight/args-success"
expect 'candidate preflight receives only its mode flag, not shell-parsed auth values' 0 has '--operator-auth-check'

run grep -F -- "EnvironmentFile=-$TMP/hub env must stay opaque" "$TMP/operator-auth-preflight/args-success"
expect 'transient preflight delegates the exact opaque EnvironmentFile path to systemd' 0 has 'EnvironmentFile='

for preflight_guard in --collect TimeoutStartSec=5s RuntimeMaxSec=10s TimeoutStopSec=2s KillMode=control-group SendSIGKILL=yes NoNewPrivileges=yes ProtectSystem=strict ProtectHome=read-only PrivateTmp=yes RestrictSUIDSGID=yes LockPersonality=yes; do
	run grep -Fx -- "$preflight_guard" "$TMP/operator-auth-preflight/args-success"
	expect "transient preflight pins $preflight_guard" 0 has "$preflight_guard"
done

run grep -Fx -- '--listen' "$TMP/operator-auth-preflight/args-success"
expect 'preflight never passes a shell-parsed listener flag' 1 is_silent

run grep -Fx -- '--operator-capability-prefix' "$TMP/operator-auth-preflight/args-success"
expect 'preflight never passes a shell-parsed capability flag' 1 is_silent

run operator_auth_preflight_fixture deny
expect 'candidate denial keeps its non-zero status through the set-e-safe capture path' 42 has_both 'exit 42' 'OPERATOR_CAPABILITY_REQUIRED'

run operator_auth_preflight_fixture empty
expect 'exit zero without the versioned sentinel fails closed' 1 has 'preflight failed'

run operator_auth_preflight_fixture wrong-version
expect 'an unknown sentinel version fails closed' 1 has 'preflight failed'

run operator_auth_preflight_fixture different-source
expect 'a successful probe for anything except the Hub self-source fails closed' 1 has 'preflight failed'

run operator_auth_preflight_fixture multiline
expect 'extra candidate output cannot be mistaken for the single success sentinel' 1 has 'preflight failed'

run operator_auth_preflight_fixture hang
expect 'a wedged transient manager/client path is bounded and fails closed' 124 has 'exit 124'

run operator_auth_failure_handoff_fixture
expect 'preflight denial exits before the exact rollout segment can stop Hub' 42 has 'Preflight'
run file_absent "$TMP/operator-auth-handoff/clawctl-hub.new"
expect 'preflight denial removes only the unactivated candidate' 0 is_silent
run file_absent "$TMP/operator-auth-handoff/stop-called"
expect 'preflight denial leaves the live systemd unit untouched' 0 is_silent

run systemd_unit_contract_fixture good
expect 'exact installed systemd execution and environment contract is accepted' 0 is_silent
for contract_mode in read-fail not-loaded wrong-fragment dropin wrong-envfile missing-default unset pam wrong-exec wrong-db need-reload disk-drift; do
	run systemd_unit_contract_fixture "$contract_mode"
	expect "systemd contract rejects $contract_mode drift before rollout" 1 has '✗'
done

run appears_before 'if [[ $ROLLBACK -eq 1 ]]; then' 'if ! verify_systemd_unit_contract; then' "$UPGRADE_HUB"
expect 'emergency rollback is not gated by forward-only systemd auth settings' 0 is_silent

run grep -E 'verify "\$(current|prev|was)" 1' "$UPGRADE_HUB"
expect 'legacy recovery verification explicitly opts into old metrics compatibility' 0 has ' 1'

run grep -F -- '--rollback-snapshot --maintenance-already-held --db "$DB" --out "$destination"' "$UPGRADE_HUB"
expect 'candidate uses SQLite online rollback snapshot under a verified existing marker' 0 has '--maintenance-already-held'

run appears_in_order3 'begin_upgrade_maintenance "$BIN.new"' 'move_installed_binary_to_dormant "$BIN.prev"' 'if ! assert_no_hub_process; then' "$UPGRADE_HUB"
expect 'maintenance precedes disabling the old binary and the final process drain' 0 is_silent

run appears_before 'if ! assert_no_hub_process; then' 'create_rollback_snapshot "$CANDIDATE_PROBE"' "$UPGRADE_HUB"
expect 'snapshot starts only after dormant-binary process drain' 0 is_silent

run function_body_has move_installed_binary_to_dormant 'make_dormant_binary_non_executable "$BIN"' "$UPGRADE_HUB"
expect 'installed binary is non-executable before it receives a dormant suffix' 0 is_silent

run function_body_has stage_executable_probe 'chmod 0700 -- "/proc/$owner_pid/fd/$probe_fd"' "$UPGRADE_HUB"
expect 'dormant compatibility checks use a private executable probe' 0 is_silent

run grep -F 'upgrade-protocol-ready:v1 stopped-check=true ledger-path=true maintenance-self-disable=true held-snapshot=true dormant-artifacts=true writer-lock-gate=true' "$UPGRADE_HUB"
expect 'upgrade requires the complete versioned fail-closed protocol sentinel' 0 has 'upgrade-protocol-ready:v1'

run grep -F 'timeout 5 "$BIN.prev"' "$UPGRADE_HUB"
expect 'upgrade script never directly executes dormant .prev' 1 is_silent

run grep -F 'create_rollback_snapshot "$BIN.failed"' "$UPGRADE_HUB"
expect 'upgrade script never directly executes dormant .failed for snapshots' 1 is_silent

run awk '
	/acquire_database_writer_lock \|\| exit 1/ { acquisitions++ }
	END { exit !(acquisitions == 3) }
' "$UPGRADE_HUB"
expect 'manual rollback, forward rollout and automatic rollback each acquire the writer lock after stop' 0 is_silent

run appears_in_order3 'stop_hub_for_database_move || exit 1' 'acquire_database_writer_lock || exit 1' 'verify_stopped_hub_contract "$BIN"' "$UPGRADE_HUB"
expect 'manual rollback proves the exact stopped unit and recursive cgroup after taking locks' 0 is_silent

run appears_in_order3 'acquire_database_writer_lock || exit 1' 'verify_stopped_hub_contract "$BIN"' 'check_rollback_compatibility "$PREV_PROBE"' "$UPGRADE_HUB"
expect 'manual rollback owns both locks and proves stopped state before its first DB read' 0 is_silent

run appears_in_order3 'echo "→ Stopping Hub"' 'acquire_database_writer_lock || exit 1' 'check_rollback_compatibility "$BIN.new"' "$UPGRADE_HUB"
expect 'forward rollout owns the writer lock before compatibility and snapshot reads' 0 is_silent

run appears_in_order3 'acquire_database_writer_lock || exit 1' 'verify_stopped_hub_contract "$BIN.new"' 'check_rollback_compatibility "$BIN.new"' "$UPGRADE_HUB"
expect 'forward rollout re-proves exact stopped state immediately before its first DB read' 0 is_silent

run appears_before 'create_rollback_snapshot "$CANDIDATE_PROBE"' 'mv -fT -- "$BIN.new" "$BIN"' "$UPGRADE_HUB"
expect 'verified snapshot exists before candidate occupies installed path' 0 is_silent

run appears_in_order3 'mv -fT -- "$BIN.new" "$BIN"' 'release_writer_lock' 'systemctl --user start "$UNIT"' "$UPGRADE_HUB"
expect 'forward rollout releases the shell writer lock immediately before candidate start' 0 is_silent

run grep -F 'cp "$DB" ' "$UPGRADE_HUB"
expect 'upgrade never raw-copies live SQLite main file' 1 is_silent

run grep -F 'PREUPGRADE_WAL_' "$UPGRADE_HUB"
expect 'rollback coordinate is standalone and has no separately timed WAL' 1 is_silent

run grep -F 'chmod 0700 "$quarantine"' "$UPGRADE_HUB"
expect 'failed migrated DB quarantine is explicitly private' 0 has '0700'

run grep -F 'mv -- "$candidate" "$quarantine/"' "$UPGRADE_HUB"
expect 'failed DB plus WAL/SHM are moved into quarantine' 0 has 'quarantine'

run function_body_has quarantine_failed_database 'durably_sync_path "$(dirname "$DB")"' "$UPGRADE_HUB"
expect 'failed DB quarantine is durable in the live ledger directory' 0 is_silent

run grep -F "for suffix in '' '-wal' '-shm' '-journal'; do" "$UPGRADE_HUB"
expect 'failed candidate rollback journal is quarantined with its database' 0 has "'-journal'"

run grep -F 'rm -f -- "$DB-wal" "$DB-shm" "$DB-journal"' "$UPGRADE_HUB"
expect 'database restore removes every incompatible SQLite sidecar' 0 has 'DB-journal'

run function_body_has restore_preupgrade_database 'durably_sync_path "$DB"' "$UPGRADE_HUB"
expect 'restored pre-upgrade database is synced before legacy binary publication' 0 is_silent

run function_body_has restore_preupgrade_database 'durably_sync_path "$(dirname "$DB")"' "$UPGRADE_HUB"
expect 'restored or absent database directory entry is synced before legacy handoff' 0 is_silent

run appears_before 'quarantine_failed_database "$quarantine"' 'restore_preupgrade_database "$PREUPGRADE_DB_BACKUP"' "$UPGRADE_HUB"
expect 'automatic rollback quarantines migrated DB before restoring backup' 0 is_silent

run awk '
	/automatic rollback failed to isolate failed candidate/ { in_auto=1 }
	in_auto && /quarantine_failed_database "\$quarantine"/ { quarantine=NR }
	in_auto && /restore_preupgrade_database "\$PREUPGRADE_DB_BACKUP"/ { restore=NR }
	END { exit !(in_auto && quarantine > 0 && restore > quarantine) }
' "$UPGRADE_HUB"
expect 'automatic rollback isolates the candidate before quarantine and database restore' 0 is_silent

run appears_before_last 'restore_preupgrade_database "$PREUPGRADE_DB_BACKUP"' 'systemctl --user start "$UNIT"' "$UPGRADE_HUB"
expect 'automatic rollback restores pre-upgrade DB before old Hub starts' 0 is_silent

run appears_in_order3 'if verify "$VERSION" 0; then' 'remove_upgrade_maintenance_marker' 'exit 0' "$UPGRADE_HUB"
expect 'successful candidate is verified before maintenance writes reopen' 0 is_silent

run appears_in_order3 'if verify "$VERSION" 0; then' 'durably_sync_path "$(dirname "$DB")" "verified candidate ledger state"' 'remove_upgrade_maintenance_marker' "$UPGRADE_HUB"
expect 'verified candidate ledger is durable before maintenance writes reopen' 0 is_silent

run appears_in_order3 'restore_preupgrade_database "$PREUPGRADE_DB_BACKUP"' 'systemctl --user start "$UNIT"' 'durably_sync_path "$(dirname "$DB")" "verified automatic rollback ledger state"' "$UPGRADE_HUB"
expect 'automatic rollback keeps maintenance across restored Hub start and verification' 0 is_silent

run appears_in_order3 'durably_sync_path "$(dirname "$DB")" "verified automatic rollback ledger state"' 'remove_upgrade_maintenance_marker' 'Already rolled back to $was' "$UPGRADE_HUB"
expect 'automatic rollback reopens writes only after restored Hub verification' 0 is_silent

run appears_in_order3 'durably_sync_path "$BIN.new" "staged candidate binary"' 'mv -fT -- "$BIN.new" "$BIN"' 'systemctl --user start "$UNIT"' "$UPGRADE_HUB"
expect 'candidate bytes are durable before activation and service start' 0 is_silent

run appears_before 'for dormant in "${stale_binary_artifacts[@]}"; do' 'if ! verify_no_stale_maintenance_marker; then' "$UPGRADE_HUB"
expect 'interrupted binary artifacts are disabled before stale marker refusal' 0 is_silent

run grep -F 'make_dormant_binary_non_executable "$BIN.new" "unactivated candidate binary"' "$UPGRADE_HUB"
expect 'every pre-activation exit makes a staged candidate non-executable' 0 has 'BIN.new'

run function_body_appears_in_order3 fail_closed_started_recovery 'make_dormant_binary_non_executable "$BIN"' 'systemctl --user stop "$UNIT"' 'if ! assert_no_hub_process; then' "$UPGRADE_HUB"
expect 'failed recovery verification disables, stops and drains the installed Hub' 0 is_silent

run function_body_appears_in_order3 restart_current_after_refusal 'if verify "$current" 1; then' 'durably_sync_path "$(dirname "$DB")"' 'remove_upgrade_maintenance_marker' "$UPGRADE_HUB"
expect 'restored current Hub keeps maintenance until post-start verification' 0 is_silent

run appears_in_order3 'if verify "$prev" 1; then' 'durably_sync_path "$(dirname "$DB")" "verified legacy rollback ledger state"' 'remove_upgrade_maintenance_marker' "$UPGRADE_HUB"
expect 'manual rollback keeps maintenance until legacy verification succeeds' 0 is_silent

run grep -E 'trap .*MAINTENANCE_MARKER|trap .*remove_upgrade_maintenance_marker' "$UPGRADE_HUB"
expect 'interruption cannot silently clear the maintenance marker' 1 is_silent

run awk '
	/if \[\[ "\$r" != "0" \]\]/ { in_restart_failure=1 }
	in_restart_failure && /ok=1/ { found=1 }
	END { exit !found }
' "$UPGRADE_HUB"
expect 'NRestarts nonzero is a failed verification gate, not a warning' 0 is_silent

run appears_in_order3 'mv -fT -- "$BIN.new" "$BIN"' 'systemctl --user reset-failed "$UNIT"' 'systemctl --user start "$UNIT"' "$UPGRADE_HUB"
expect 'restart counter is reset before candidate verification window' 0 is_silent

run grep -F '支援手動 --rollback' "$UPGRADE_HUB"
expect 'success handoff does not promise unsupported first-upgrade rollback' 1 is_silent

lock_path="$HOME/.local/share/clawctl/clawctl.sqlite.upgrade.lock"
lock_ready="$TMP/upgrade-lock-ready"
lock_release="$TMP/upgrade-lock-release"
mkdir -p "$(dirname "$lock_path")"
(
	exec 8>"$lock_path"
	flock 8
	: >"$lock_ready"
	while [[ ! -e "$lock_release" ]]; do sleep 0.02; done
) &
lock_holder=$!
for _ in $(seq 1 100); do
	[[ -e "$lock_ready" ]] && break
	sleep 0.02
done
run bash "$UPGRADE_HUB"
touch "$lock_release"
wait "$lock_holder"
expect 'a second upgrade process fails immediately on the lifecycle lock' 1 has_both 'Another' 'upgrade'

stale_marker="$TMP/clawctl.sqlite.upgrade-maintenance"
printf 'interrupted\n' >"$stale_marker"
stale_source="$(sed -n '/^verify_no_stale_maintenance_marker() {$/,/^}$/p' "$UPGRADE_HUB")"
run bash -c 'MAINTENANCE_MARKER=$1; eval "$2"; verify_no_stale_maintenance_marker' _ "$stale_marker" "$stale_source"
expect 'stale upgrade marker fails closed before build or DB handling' 1 has_both 'leftover' 'Next step'

untrusted_home="$TMP/caller-controlled-home"
systemctl_called="$TMP/systemctl-called"
home_guard_bin="$TMP/home-guard-bin"
mkdir -p "$untrusted_home" "$home_guard_bin"
printf '#!/usr/bin/env bash\n: >"$SYSTEMCTL_CALLED"\nexit 88\n' >"$home_guard_bin/systemctl"
chmod +x "$home_guard_bin/systemctl"
run env HOME="$untrusted_home" SYSTEMCTL_CALLED="$systemctl_called" \
	PATH="$home_guard_bin:/usr/bin:/bin" bash "$UPGRADE_HUB"
expect 'caller-controlled HOME is rejected before lifecycle paths are resolved' 1 has_both 'canonical home' 'no lock created'
run file_absent "$systemctl_called"
expect 'HOME rejection cannot reach systemctl' 0 is_silent
run file_absent "$untrusted_home/.local"
expect 'HOME rejection creates neither lock nor database directory under the fake home' 0 is_silent

stamp="$TMP/hub.stamp"
now=$(date -u +%s)

printf '%s\n' "$now" >"$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat "$DEADMAN" "$stamp"
expect 'fresh timestamp passes silently' 0 is_silent

printf '%s\n' "$((now - 25 * 3600))" >"$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat "$DEADMAN" "$stamp"
expect 'timestamp within threshold passes silently' 0 is_silent

printf '%s\n' "$((now - 30 * 3600))" >"$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat "$DEADMAN" "$stamp"
expect 'expired timestamp reports hours and threshold' 1 has_both '已經 30 小時' '上限 26'

printf '%s\n' "$((now + 7200))" >"$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat "$DEADMAN" "$stamp"
expect 'future timestamp is rejected' 1 has '未來'

rm -f "$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat "$DEADMAN" "$stamp"
expect 'missing timestamp is rejected' 1 has '找不到時間戳'

printf '%s\n' 'not-a-number' >"$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat "$DEADMAN" "$stamp"
expect 'non-numeric timestamp is rejected' 1 has '不是一個 unix 時間'

printf '%s\n' "$((now - 30 * 3600))" >"$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=false "$DEADMAN" "$stamp"
expect 'alert channel failure preserves original message' 1 has_both '已經 30 小時' 'alert channel failed'

run env -u DEADMAN_ALERT_CMD PATH="$TMP/bin:/usr/bin:/bin" "$DEADMAN" "$stamp"
expect 'unset alert command does not masquerade as channel failure' 1 lacks '回傳非零'

# --- 手動執行要在**訊息裡**說自己是手動的。
#
# ⚠⚠ 2026-09-03 02:24（EDT）operator 的手機連響四次，四則長得跟真的機隊全滅
# 一模一樣。那是我在 sampleagent2 上手動把六種情境跑過一遍，其中四種會叫。
# 他收到的前四則告警全是假的，而且**他分不出來**——
# 然後下一則真的來的時候，他會先問「是不是又在測」。
#
# ⚠ 標記必須在訊息本體裡：手機上看得到的只有訊息，看不到 stderr。
printf '%s\n' "$((now - 30 * 3600))" >"$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat \
	SSH_CONNECTION='10.0.0.1 22 10.0.0.2 22' "$DEADMAN" "$stamp"
expect 'manual alert execution tags manual source' 1 has_both '已經 30 小時' '來源：manual'

# ⚠⚠ 反面，而且這一條比上面那條重要：cron 的真告警**不准**被加上這句話。
# 一則真的半夜告警如果寫著「這是有人手動跑的」，人就不會去看機隊 ——
# 那比沒有標記更糟。cron 沒有 TTY、也沒有 SSH_*。
run env -i PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat "$DEADMAN" "$stamp" </dev/null
expect 'scheduled alert is not tagged as manual' 1 lacks '來源：manual'
expect 'scheduled alert itself is still delivered' 1 has '已經 30 小時'

run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat "$DEADMAN" --test
expect '--test sends test message successfully' 0 has 'clawctl deadman 測試'
expect '--test reports daily health check disabled' 0 has 'Daily health check: disabled'

run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat \
	DEADMAN_ALERT_CHECK_CMD=true "$DEADMAN" --test
expect '--test reports daily health check enabled' 0 has 'Daily health check: enabled'

# --- 平日健檢：早報正常，但告警管道自己壞了。
#
# ⚠⚠ 這是這支腳本最重要、也最容易被寫成靜音的一種狀態：**今天沒有壞消息**。
# 一個只在出事那天才會被用到的告警管道，等於一條沒有被測試的路徑 ——
# token 三個月前被撤銷也不會有人知道，直到它唯一該說話的那天（§5.23）。
printf '%s\n' "$now" >"$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat \
	DEADMAN_ALERT_CHECK_CMD=true "$DEADMAN" "$stamp"
expect 'passing health check stays silent' 0 is_silent

# ⚠ 時間戳是新鮮的 —— 舊的判斷邏輯到這裡會 exit 0 而且一個字都不印。
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat \
	DEADMAN_ALERT_CHECK_CMD=false "$DEADMAN" "$stamp"
expect 'healthy report but broken channel is not silent' 1 has '告警通道健檢失敗'
expect 'health check failure only reports current status' 1 lacks '下一次真的出事'
# ⚠ `false` 什麼都不印。沒有這一條的話訊息會是「壞了：」後面空一片，
# 收到的人會先去查訊息怎麼被截斷了，而不是去查管道。
expect 'health check with no output notes lack of message' 1 has '健檢沒有印任何訊息'

# ⚠ 健檢的輸出要帶進訊息裡，否則人只知道「壞了」不知道壞在哪。
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat \
	DEADMAN_ALERT_CHECK_CMD='echo token revoked; exit 1' "$DEADMAN" "$stamp"
expect 'includes health check reason in alert' 1 has 'token revoked'

# ⚠ 沒設健檢是選配，不是每天警告一次的理由。
run env -u DEADMAN_ALERT_CHECK_CMD PATH="$TMP/bin:/usr/bin:/bin" \
	DEADMAN_ALERT_CMD=cat "$DEADMAN" "$stamp"
expect 'unset health check does not warn daily' 0 is_silent

# ⚠⚠ 健檢**不准**擋住真告警。過期的時間戳才是這支腳本的主要工作；
# 一個「因為健檢先失敗所以沒講早報沒來」的死人之鐘是本末倒置。
printf '%s\n' "$((now - 30 * 3600))" >"$stamp"
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=cat \
	DEADMAN_ALERT_CHECK_CMD=false "$DEADMAN" "$stamp"
expect 'health check must not block real alert' 1 has '已經 30 小時'
printf '%s\n' "$now" >"$stamp"

# ⚠ 這兩個狀態的修法不同；只驗「非零」會把操作指引也一起弄丟。
run env PATH="$TMP/bin:/usr/bin:/bin" DEADMAN_ALERT_CMD=false "$DEADMAN" --test
expect '--test returns 1 on channel failure' 1 has 'Alert channel failed'

run env -u DEADMAN_ALERT_CMD PATH="$TMP/bin:/usr/bin:/bin" "$DEADMAN" --test
expect '--test returns 2 when not configured' 2 has 'Alert channel not configured'

# --- ops/prometheus/rules/*.yml：判斷搬到規則那一側之後，規則檔就是程式碼。
#
# ⚠ 一個 YAML 縮排錯掉的規則檔，Prometheus reload 會拒絕整份 —— 而「reload
# 失敗」在 systemctl 上只是一行 warning，舊規則繼續跑，畫面上什麼都不缺。
# 所以規則檔跟 Go 一樣要在 commit 之前被機器讀一次。
# promtool 在的時候規則檔必須過。本機沒裝時跳過，並把那件事講清楚。
# CI 設 CLAWCTL_REQUIRE_PROMTOOL=1 時，沒有 promtool 仍然是失敗。
if command -v promtool >/dev/null 2>&1; then
	run promtool check rules "$ROOT"/ops/prometheus/rules/*.yml
	expect 'promtool parses rules/*.yml' 0 has 'SUCCESS'
	expect 'no rules/*.yml file is broken' 0 lacks 'FAILED'
elif [ "${CLAWCTL_REQUIRE_PROMTOOL:-}" = "1" ]; then
	rc=127; output='promtool not in PATH'
	fail 'promtool parses rules/*.yml (CLAWCTL_REQUIRE_PROMTOOL=1, cannot pass without promtool)'
else
	echo "skip: promtool not in PATH, skipping ops/prometheus/rules/*.yml. Must fail in CI when CLAWCTL_REQUIRE_PROMTOOL=1."
fi

# --- ops/prometheus/install.sh only calls stage scripts that exist.
# It once called an install script that was never in this tree, so the first
# stage could not run at all.
missing=""
stages=0
for s in $(grep -o '"\$HERE/[^"]*\.sh"' "$ROOT/ops/prometheus/install.sh" | sed 's|"\$HERE/||; s|"$||'); do
	stages=$((stages + 1))
	[ -x "$ROOT/ops/prometheus/$s" ] || missing="$missing $s"
done
rc=0
output="stages=$stages missing:${missing:- none}"
if [ "$stages" -ge 3 ] && [ -z "$missing" ]; then
	pass 'ops/prometheus/install.sh calls only stage scripts that exist'
else
	fail 'ops/prometheus/install.sh calls only stage scripts that exist'
fi

run env -u CLAWCTL_FLEET_JSON "$ROOT/ops/prometheus/install-prometheus.sh"
expect 'install-prometheus.sh refuses to run without a fleet.json' 1 has 'CLAWCTL_FLEET_JSON'

run env CLAWCTL_FLEET_JSON="$ROOT/ops/prometheus/fleet.example.json" CLAWCTL_PROMETHEUS_YML="$TMP/no-such.yml" "$ROOT/ops/prometheus/install-prometheus.sh"
expect 'install-prometheus.sh refuses a missing Prometheus config' 1 has 'not found'

# --- ops/check-metrics.sh：沒有 parser 的時候要**大聲**，不是安靜跳過。
#
# ⚠ 這一條守的是這個專案最在意的那種 bug：一支「因為缺工具所以什麼都沒做」
# 的驗證腳本，如果 exit 0，在 CI 或部署輸出上會跟「驗過了，沒問題」
# 長得一模一樣。所以它必須 exit 2，而且要印出怎麼把環境補起來。
#
# ⚠⚠ 這個測試的第一版是用清空 PATH + BASH_ENV 攔截 command -v 寫的 ——
# 因為當時 check-metrics.sh 對「人明講的 CLAWCTL_PY」不做檢查，只好從外面
# 把每一個 fallback 都遮掉。那個版本自己炸了（連 mktemp 都被 PATH 弄不見），
# 而且**還是打到了線上 Hub**（絕對路徑的 /tmp/promv 沒被遮到）。
# 一個測試需要那麼用力才問得到某條路徑，通常是那條路徑本身有問題 ——
# 所以修的是腳本，不是測試。現在指一個不能用的 python 就夠了。
run env CLAWCTL_PY=/bin/false "$CHECK_METRICS"
expect 'missing prometheus parser explicitly returns 2' 2 has "not 'passed'"
# ⚠ 而且要在**碰網路之前**就退出。這一條在防的是「驗證失敗了，
# 但它已經先去打了一輪線上 Hub」—— 測試不該有能力去戳正式環境。
expect 'missing parser must not touch network' 2 lacks '== Fetching'

# --- ops/notify-telegram.sh：它必須講出自己要用哪一份設定。
#
# ⚠⚠ 這幾條守的是 2026-09-04 在 sampleagent2 上實際量到的一個活的缺陷：
# 死人之鐘的**收件人取決於它是怎麼被叫的**。cron 的那一行設了
# CLAWCTL_NOTIFY_ENV=…/deadman.env；一個人 ssh 進去手動跑則沒有，
# 於是掉到 ~/.config/heartbeat-patrol.env —— 另一個 bot（實測 chat_id 相同，
# 所以訊息還是會出現在同一個聊天室裡，更難發現），
# 而且兩個檔案都定義 TELEGRAM_BOT_TOKEN/CHAT_ID，所以手動那條**會成功**。
#
# 「我手動跑過，有收到」因此完全不成立：你驗的是另一條路。
# 修法不是改優先序（那個優先序是對的），是讓它**說出來**。
#
# ⚠ 全部用空訊息驅動：設定來源那一行印在 body 讀取之前，
#   而空訊息在 curl 之前就退出 —— 所以這些測試不碰網路、不送任何訊息。
NOTIFY="$ROOT/ops/notify-telegram.sh"
NT="$TMP/notify-home"
mkdir -p "$NT/.config/clawctl"
printf 'TELEGRAM_BOT_TOKEN=fake-a\nTELEGRAM_CHAT_ID=1\n' >"$NT/.config/clawctl/notify.env"
printf 'TELEGRAM_BOT_TOKEN=fake-b\nTELEGRAM_CHAT_ID=2\n' >"$NT/.config/heartbeat-patrol.env"

run env -u CLAWCTL_NOTIFY_ENV HOME="$NT" "$NOTIFY" </dev/null
expect 'defaults to notify.env when unspecified' 1 has 'clawctl/notify.env'
expect 'notes fallback when unspecified' 1 has 'CLAWCTL_NOTIFY_ENV not specified'

run env HOME="$NT" CLAWCTL_NOTIFY_ENV="$NT/.config/heartbeat-patrol.env" "$NOTIFY" </dev/null
expect 'uses explicitly specified env file' 1 has 'heartbeat-patrol.env'
expect 'does not warn when explicitly specified' 1 lacks 'CLAWCTL_NOTIFY_ENV not specified'

# sampleagent2 的實況：只有 heartbeat-patrol.env，沒有 notify.env
rm -f "$NT/.config/clawctl/notify.env"
run env -u CLAWCTL_NOTIFY_ENV HOME="$NT" "$NOTIFY" </dev/null
expect 'names heartbeat-patrol explicitly when only option' 1 has 'heartbeat-patrol.env'
expect 'warns about possible unintended recipient' 1 has 'different bot or chat than expected'

# ⚠ 這一條是上面每一條的前提：它們不准真的送出任何東西。
expect 'config resolution does not touch network' 1 has 'refusing to send'

# --- ops/notify-telegram.sh --check：不送訊息，只問管道通不通。
#
# ⚠ 用一支假的 curl 攔在 PATH 上，所以這些測試**完全不碰網路**，
# 而且能把 Telegram 的每一種回應都演一遍 —— 包括「token 被撤銷」，
# 那是真實環境裡最重要、也最沒辦法按需重現的一種。
cat >"$TMP/bin/curl" <<'FAKE'
#!/usr/bin/env bash
# notify-telegram.sh 把 URL（含 token）用 `curl -K -` 從 stdin 餵進來，
# 不放在 argv。假的 curl 照真的 curl 的規矩去讀 stdin，才知道要演哪個端點。
cfg=''
prev=''
for a in "$@"; do
	if [ "$prev" = "-K" ] && [ "$a" = "-" ]; then
		cfg=$(cat)
	fi
	prev=$a
done
# 把 argv 記下來，讓測試檢查 token 沒有出現在 ps 看得到的地方。
[ -z "${FAKE_CURL_ARGV:-}" ] || printf '%s\n' "$*" >>"$FAKE_CURL_ARGV"
mode=''
for a in "$@" "$cfg"; do
	case "$a" in
	*/getMe | */getMe\") mode=getMe ;;
	*/getChat | */getChat\") mode=getChat ;;
	*/sendMessage | */sendMessage\") mode=sendMessage ;;
	esac
done
case "${FAKE_CURL:-ok}" in
ok)
	case "$mode" in
	getMe) echo '{"ok":true,"result":{"username":"Fake_Bot"}}' ;;
	getChat) echo '{"ok":true,"result":{"type":"private","first_name":"Tester"}}' ;;
	sendMessage) echo '{"ok":true,"result":{"message_id":1}}' ;;
	esac
	;;
unauthorized) echo '{"ok":false,"error_code":401,"description":"Unauthorized"}' ;;
kicked) echo '{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked"}' ;;
# ⚠ 故意把整個 argv 和 stdin 上的設定（含 URL、含 token）吐到 stderr ——
# 這是最壞情況。下面有測試專門檢查腳本沒有把它印出去。
netfail)
	echo "curl: (6) Could not resolve host -- $* $cfg" >&2
	exit 6
	;;
esac
FAKE
chmod +x "$TMP/bin/curl"

SEKRIT='SECRET-TOKEN-MUST-NOT-APPEAR'
ck() { # $1=FAKE_CURL 模式
	run env PATH="$TMP/bin:/usr/bin:/bin" FAKE_CURL="$1" \
		TELEGRAM_BOT_TOKEN="$SEKRIT" TELEGRAM_CHAT_ID=1 \
		"$NOTIFY" --check
}

ck ok
expect '--check reports bot username on success' 0 has 'Fake_Bot'
expect '--check reports delivery target on success' 0 has 'Tester'

# ⚠ token 被撤銷是這個模式存在的唯一理由。它必須 exit 非零 ——
# 一個「檢查完說沒事」的健檢，跟沒有健檢是同一件事。
ck unauthorized
expect '--check catches revoked token' 1 has 'Unauthorized'

# ⚠ getMe 過了不代表送得進去：bot 被踢出群組時 getMe 完全正常。
ck kicked
expect '--check catches bot cannot deliver to chat' 1 has 'kicked'
expect '--check explains kicked/deleted/typo symptoms' 1 has 'all look like this'

ck netfail
expect '--check reports network failure as network failure' 1 has 'cannot reach Telegram'

# ⚠⚠ 這一條守的是這個 repo 的紅線：curl 的錯誤訊息含整個 URL，URL 裡有 token。
# 這則輸出會被寫進 Hub 的 notifications 表、會進 syslog。
expect '--check never prints token' 1 lacks "$SEKRIT"

# ⚠ --check 必須用跟真的送出**同一份設定**。一支自己去找設定的健檢程式，
# 驗的是它自己那條路，不是半夜真的會走的那條 —— 那正是這一段要防的 bug。
printf 'TELEGRAM_BOT_TOKEN=fake-a\nTELEGRAM_CHAT_ID=1\n' >"$NT/.config/clawctl/notify.env"
run env -u CLAWCTL_NOTIFY_ENV PATH="$TMP/bin:/usr/bin:/bin" HOME="$NT" "$NOTIFY" --check
expect '--check uses identical config resolution' 0 has 'clawctl/notify.env'

# --- notify-telegram.sh：token 不准出現在 curl 的 argv。
#
# ⚠⚠ URL 是 https://api.telegram.org/bot<token>/<method>。放在 argv 的話，
# 每一次送出時同一台機器上任何人 `ps -ef` 都看得到 token。腳本改成
# `curl -K -` 從 stdin 給 URL；這幾條確認 argv 裡真的沒有 token，
# 而且送出路徑的 exit code 跟原本一樣。
ARGV_LOG="$TMP/curl-argv.log"
argv_clean() { [ -s "$ARGV_LOG" ] && ! grep -qF "$SEKRIT" "$ARGV_LOG"; }

: >"$ARGV_LOG"
run env PATH="$TMP/bin:/usr/bin:/bin" FAKE_CURL=ok FAKE_CURL_ARGV="$ARGV_LOG" \
	TELEGRAM_BOT_TOKEN="$SEKRIT" TELEGRAM_CHAT_ID=1 "$NOTIFY" --check
expect '--check passes when receiving URL from stdin' 0 has 'Fake_Bot'
expect '--check curl argv does not contain token' 0 argv_clean

nsend() { # $1=FAKE_CURL 模式；訊息從 stdin 進來
	: >"$ARGV_LOG"
	run env PATH="$TMP/bin:/usr/bin:/bin" FAKE_CURL="$1" FAKE_CURL_ARGV="$ARGV_LOG" \
		TELEGRAM_BOT_TOKEN="$SEKRIT" TELEGRAM_CHAT_ID=1 "$NOTIFY" <<<'早報測試'
}

nsend ok
expect 'exits 0 on successful delivery' 0 true
expect 'delivery curl argv does not contain token' 0 argv_clean
argv_has_text() { grep -qF 'text=早報測試' "$ARGV_LOG"; }
expect 'delivery passes message content to curl' 0 argv_has_text

nsend unauthorized
expect 'exits 1 and reports reason when Telegram rejects' 1 has 'Telegram rejected message: Unauthorized'
expect 'rejection curl argv does not contain token' 1 argv_clean

nsend netfail
expect 'exits 1 on delivery network failure' 1 has 'curl failed (exit 6)'
expect 'delivery failure never prints token' 1 lacks "$SEKRIT"

if bash "$ROOT/ops/test-fly.sh"; then
	passed=$((passed + 1))
else
	failed=$((failed + 1))
fi

if bash "$ROOT/ops/test-disk-clean.sh"; then
	passed=$((passed + 1))
else
	failed=$((failed + 1))
fi

printf '\npassed: %s, failed: %s\n' "$passed" "$failed"
if [ "$failed" -ne 0 ]; then
	exit 1
fi
