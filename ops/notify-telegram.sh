#!/usr/bin/env bash
# 把 stdin 的內容送到 Telegram。給 clawctl-hub --notify-cmd 用。
#
# 設定（擇一，先找到的先用）：
#   $CLAWCTL_NOTIFY_ENV                 明確指定
#   ~/.config/clawctl/notify.env        建議放這裡
#   ~/.config/heartbeat-patrol.env      沿用既有的 bot
#
# 檔案內容：
#   TELEGRAM_BOT_TOKEN=...
#   TELEGRAM_CHAT_ID=...
#
# ⚠⚠ 這支腳本最重要的性質是：**送不出去就 exit 非零。**
#
# clawctl-hub 用 exit code 決定要不要把這則早報記成 delivered，而
# 「今天沒收到早報」必須能區分成「Hub 死了」跟「Hub 活著但推播壞了」。
# 一支永遠 exit 0 的推播腳本會讓 Hub 每天記下一則從來沒有送達的早報，
# 而那正好把死人之鐘變成裝飾品 —— 它會永遠顯示一切正常。
#
# curl 的 --fail 不夠：Telegram 在參數錯誤時會回 HTTP 200 加上
# {"ok":false,...}。所以下面真的去讀了 body。

set -uo pipefail

# --check：**不送任何訊息**，只確認這條管道現在還通不通。
#
# ⚠⚠ 這個模式存在的理由，是 §5.23 量到的那個洞：
# 死人之鐘半夜要用的那個 token，在它真的需要用之前**從來沒有被用過**。
# 它可能三個月前就被撤銷了，而你會在最需要它的那一天才發現 ——
# 那一天它的工作正好是「告訴你出事了」。
#
# Telegram 有兩個唯讀端點剛好能問到這件事，而且不會送出任何東西：
#   getMe    → 這個 token 還有效嗎
#   getChat  → 這個聊天室還在嗎、而且這隻 bot 還在裡面嗎
#
# 第二個檢查驗證目標聊天室的送達權限。
# bot 被踢出群組、群組被刪掉、chat_id 打錯 —— 這三種 getMe 全部會過。
CHECK=0
if [ "${1:-}" = "--check" ]; then
	CHECK=1
	shift
fi

# ⚠ 環境變數優先於設定檔，而不是相反。
#
# 原本這裡無條件 source，結果是：呼叫端明明設好了 TELEGRAM_BOT_TOKEN，
# 檔案裡的值還是會安靜地蓋過去。除了違反最小驚訝，它還讓這支腳本
# 沒辦法被測 —— 你沒有辦法餵一個假的 token 進來看它會不會正確地失敗。
# 而「送失敗時會不會回報失敗」正是這支腳本唯一重要的性質。
_caller_token="${TELEGRAM_BOT_TOKEN:-}"
_caller_chat="${TELEGRAM_CHAT_ID:-}"

_src="caller environment variables"
if [ -z "$_caller_token" ] || [ -z "$_caller_chat" ]; then
	for f in "${CLAWCTL_NOTIFY_ENV:-}" \
		"$HOME/.config/clawctl/notify.env" \
		"$HOME/.config/heartbeat-patrol.env"; do
		# ⚠ 用 -r 不是 -f：明確指定 CLAWCTL_NOTIFY_ENV=/dev/null 之類的
		# 非普通檔時，應該當成「就用這個，裡面沒東西」，而不是靜靜地
		# 掉到下一個候選去 —— 那會讓人以為自己覆寫成功了，其實沒有。
		if [ -n "$f" ] && [ -r "$f" ]; then
			# shellcheck disable=SC1090
			. "$f"
			_src="$f"
			[ -n "${CLAWCTL_NOTIFY_ENV:-}" ] || _fellback=1
			break
		fi
	done
fi

# ⚠⚠ 講出這則訊息**要送去哪一份設定**。只印路徑，永遠不印值。
#
# 2026-09-04 在 sampleagent2 上實測到的：死人之鐘的收件人**取決於它是怎麼被叫的**。
#
#   cron   → crontab 裡設了 CLAWCTL_NOTIFY_ENV=…/clawctl/deadman.env → SampleAgent2 Management bot
#   手動跑 → 沒有那個變數 → 掉到 ~/.config/heartbeat-patrol.env → **另一個 bot**
#
# 兩個檔案都定義 TELEGRAM_BOT_TOKEN 與 TELEGRAM_CHAT_ID，所以第二種**會成功送出**
# 而且 exit 0。於是「我 ssh 進去跑一次，有收到，死人之鐘是通的」這個結論
# 完全不成立 —— 你驗的是另一條路。而真正會在半夜叫你的那一條，一次都沒走過。
#
# ⚠ 這段註解原本寫「另一個 bot、**另一個聊天室**」。2026-09-04 去量了才知道
# 後半是錯的：sampleagent2 上這兩個檔案的 chat_id **一樣**，只有 token 不同。
# 所以手動跑的那則訊息其實**會出現在同一個聊天室裡**，看起來就像成功了 ——
# 這讓誤判比我原本以為的更容易，不是更難。真正沒被驗到的是那個 token：
# 半夜要用的 token 如果被撤銷，手動測試照樣全綠。
#
# 這正是這支腳本第 53 行早就寫下的那句話，只是這次是設定檔在做這件事：
# **一個把訊息送錯地方還回報成功的推播腳本，比送不出去更糟。**
#
# 所以現在它必須先說出自己要用哪一份設定。這一行不含任何秘密，
# 而它是唯一能讓人事後回答「那則告警到底送去哪了」的東西。
echo "notify-telegram: config source = $_src" >&2
if [ -n "${_fellback:-}" ]; then
	echo "notify-telegram: ⚠ CLAWCTL_NOTIFY_ENV not specified, selected via fallback" >&2
	echo "  If this machine has more than one bot, you may be using a different bot or chat than expected" >&2
	echo "  To specify recipient explicitly: CLAWCTL_NOTIFY_ENV=<path> $(basename "$0")" >&2
fi

# ⚠ 檔案只負責「補上沒設的那個」，不負責蓋掉呼叫端設好的。
#
# 原本是「兩個有一個沒設就 source」，於是只設了 chat_id 的呼叫端會發現
# 自己的值被檔案安靜地換掉 —— 送到別的聊天室去，而且 exit 0。
# 一個把訊息送錯地方還回報成功的推播腳本，比送不出去更糟。
[ -n "$_caller_token" ] && TELEGRAM_BOT_TOKEN="$_caller_token"
[ -n "$_caller_chat" ] && TELEGRAM_CHAT_ID="$_caller_chat"
unset _caller_token _caller_chat

: "${TELEGRAM_BOT_TOKEN:?missing TELEGRAM_BOT_TOKEN (see configuration instructions at start of script)}"
: "${TELEGRAM_CHAT_ID:?missing TELEGRAM_CHAT_ID}"

# ⚠⚠ token 不准出現在 curl 的 argv 裡。
#
# URL 是 https://api.telegram.org/bot<token>/<method>。原本直接把它當 curl 的
# 參數，結果每一次送出的那幾百毫秒裡，同一台機器上任何使用者跑 `ps -ef`
# 或讀 /proc/<pid>/cmdline 都看得到整個 token。所以 URL 改成用 curl 的
# 設定檔語法從 stdin 餵進去（`curl -K -`）：stdin 不在 argv 裡。
# 其他參數（chat_id、訊息內容）不是秘密，照舊放在 argv。
_tg_config() { # $1 = Telegram method（getMe / getChat / sendMessage）
	local t=$TELEGRAM_BOT_TOKEN
	# curl 設定檔的雙引號字串會解讀反斜線；先跳脫 \ 與 "。
	t=${t//\\/\\\\}
	t=${t//\"/\\\"}
	printf 'url = "https://api.telegram.org/bot%s/%s"\n' "$t" "$1"
}

# ⚠ --check 必須用**跟真的送出完全同一份設定**。
#
# 這是這個模式唯一重要的性質，也是它為什麼是這支腳本的一個旗標、
# 而不是另外一支腳本：一支自己去找設定的健檢程式，會重演 §5.23 ——
# 它驗的是它自己那條路，不是半夜真的會走的那條。
# 所以它接在上面的解析之後，而不是旁邊。
if [ "$CHECK" -eq 1 ]; then
	_fail=0

	# 1. token 還有效嗎
	# ⚠ 跟下面送訊息時一樣：resp 絕對不能印出來，curl 的錯誤訊息含整個 URL。
	# ⚠ --retry 是刻意的，而且只加在 --check 上。這個檢查每天在無人看管的
	# 情況下跑一次，一次 DNS 抖動就叫一聲的話，它會很快變成被忽略的雜訊 ——
	# 然後它真的抓到 token 被撤銷的那天也會被忽略（§5.11 是同一個形狀）。
	# curl 的 --retry 只重試逾時跟 5xx，**不會**重試 401，
	# 所以「token 被撤銷」照樣第一次就失敗。
	_r=$(_tg_config getMe | curl -sS --max-time 20 --retry 2 --retry-delay 3 \
		-K - 2>&1)
	if [ $? -ne 0 ]; then
		echo "notify-telegram --check: ✗ cannot reach Telegram (network or DNS)" >&2
		_fail=1
	else
		case "$_r" in
		*'"ok":true'*)
			echo "notify-telegram --check: ✓ token valid → @$(printf '%s' "$_r" |
				sed -n 's/.*"username":"\([^"]*\)".*/\1/p')"
			;;
		*)
			# token 被撤銷時 Telegram 回 401 + "Unauthorized"
			echo "notify-telegram --check: ✗ token rejected: $(printf '%s' "$_r" |
				sed -n 's/.*"description":"\([^"]*\)".*/\1/p')" >&2
			_fail=1
			;;
		esac
	fi

	# 2. 聊天室還在嗎、bot 還在裡面嗎
	#
	# ⚠ 這一步不能因為第 1 步失敗就跳過。兩件事會分別壞掉，而
	# 「token 也壞了所以我沒去看聊天室」會讓人修好 token 之後
	# 以為全部搞定了，然後在下一次真告警時發現 bot 早就被踢出群了。
	_r=$(_tg_config getChat | curl -sS --max-time 20 --retry 2 --retry-delay 3 \
		--data-urlencode "chat_id=${TELEGRAM_CHAT_ID}" \
		-K - 2>&1)
	if [ $? -ne 0 ]; then
		# ⚠ 這裡不能去 sed `_r` 裡的 description —— curl 的錯誤訊息沒有那個欄位，
		# 結果會是「✗ 送不進這個聊天室：」後面空一片，看起來像 Telegram 回了
		# 一個空的理由。網路不通跟被拒絕是兩件事，要講成兩句話。
		echo "notify-telegram --check: ✗ cannot reach Telegram (network or DNS)" >&2
		_fail=1
	else
		case "$_r" in
		*'"ok":true'*)
			# 群組有 title，私訊沒有 —— 兩種都要講得出來
			_who=$(printf '%s' "$_r" | sed -n 's/.*"title":"\([^"]*\)".*/\1/p')
			[ -n "$_who" ] || _who=$(printf '%s' "$_r" |
				sed -n 's/.*"first_name":"\([^"]*\)".*/\1/p')
			echo "notify-telegram --check: ✓ can deliver to \"${_who:-(no title)}\""
			;;
		*)
			_d=$(printf '%s' "$_r" | sed -n 's/.*"description":"\([^"]*\)".*/\1/p')
			echo "notify-telegram --check: ✗ cannot deliver to this chat: ${_d:-unrecognized response format}" >&2
			echo "  Bot kicked from group / group deleted / typo in chat_id all look like this" >&2
			_fail=1
			;;
		esac
	fi

	[ "$_fail" -eq 0 ] || exit 1
	exit 0
fi

body=$(cat)
if [ -z "${body//[[:space:]]/}" ]; then
	# ⚠ 空訊息不算送出去。上游若因為某個 bug 產出空字串，
	# 把它記成 delivered 會讓一整天安靜無聲而且沒有人知道。
	echo "notify-telegram: empty message received, refusing to send" >&2
	exit 1
fi

# Telegram 單則訊息上限 4096 字元。早報最多 5 行，正常不會碰到，
# 但截斷要留下痕跡 —— 悄悄消失的內容比截斷更糟。
if [ "${#body}" -gt 3900 ]; then
	body="${body:0:3900}
…（內容過長已截斷，完整內容在 hub 上）"
fi

resp=$(_tg_config sendMessage | curl -sS --max-time 20 \
	--data-urlencode "chat_id=${TELEGRAM_CHAT_ID}" \
	--data-urlencode "text=${body}" \
	--data "disable_web_page_preview=true" \
	-K - 2>&1)
rc=$?

if [ $rc -ne 0 ]; then
	# ⚠ 不要把 resp 印出來 —— curl 的錯誤訊息會包含整個 URL，
	# 而 URL 裡有 bot token。這則錯誤會被寫進 Hub 的 notifications 表。
	echo "notify-telegram: curl failed (exit $rc)" >&2
	exit 1
fi

case "$resp" in
*'"ok":true'*) exit 0 ;;
esac

# 只印 description，同樣是為了不把 token 寫進資料庫。
desc=$(printf '%s' "$resp" | sed -n 's/.*"description":"\([^"]*\)".*/\1/p')
echo "notify-telegram: Telegram rejected message: ${desc:-unrecognized response format}" >&2
exit 1
