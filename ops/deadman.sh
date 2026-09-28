#!/usr/bin/env bash
# 外部死人之鐘。
#
# ⚠⚠ 這支東西必須跑在「不是 Hub」的機器上。這不是建議，這是它存在的全部理由。
#
# Hub 監看整個機隊，那誰監看 Hub？不能是 Hub 自己 —— 一個死掉的 process 不會
# 發出「我死了」的通知，而一個活著但 worker 卡住的 Hub 會很有自信地說自己健康。
# 「自證不算數」這條規則對 Hub 自己也適用，而且對它最適用。
#
# 它監看的是**送達**，不是 Hub 的死活。判斷方式只有一條：
#
#     早報有沒有準時出現在它該出現的地方？
#
# 沒有出現，就代表這條鏈上有東西斷了 —— Hub 死了、worker 卡住了、推播管道掛了、
# 或者機器根本關機了。這支腳本刻意不去區分是哪一種，因為那需要它去問 Hub，
# 而會去問 Hub 的東西就不是死人之鐘了。它只知道一件事：你今天早上沒收到訊息。
#
# ---------------------------------------------------------------------------
# 怎麼用
#
# 1. Hub 那邊開 --report-stamp（或 CLAWCTL_REPORT_STAMP）：
#
#      clawctl-hub --report-stamp ~/.local/share/clawctl/last-report.stamp
#
#    Hub 只在早報**確實送達之後**才蓋這個章（notify-cmd exit 0），
#    而且只有 daily 那一種通知會蓋。
#
#    ⚠ 不要用 cron 跑 `date -u +%s > stamp`。那看起來一樣，意思完全不同：
#    那個時間戳說的是「cron 還活著」。於是 Telegram token 被撤銷、
#    reportLoop 卡死、buildReport panic —— 這三種情況死人之鐘全部維持綠燈。
#
# 2. 把時間戳送到另一台機器。samplehub1 的 crontab：
#
#      */15 * * * * scp -q ~/.local/share/clawctl/last-report.stamp \
#                     sampleagent2:.local/share/clawctl-deadman/hub.stamp
#
#    ⚠ cron 在這裡只是搬運工，不是消息來源 —— 它搬的是 Hub 蓋的章。
#    所以 scp 壞掉時，那一端的時間戳會停止更新 → 告警。方向是對的。
#
# 3. 那台機器（不是 Hub）的 crontab：
#
#      0 14 * * * DEADMAN_ALERT_CMD=... deadman.sh ~/.local/share/clawctl-deadman/hub.stamp
#
#    用固定的 UTC 時間、配 26 小時門檻，就不必去推算兩台機器的時區差。
#    ⚠ 一天只跑一次。改成每小時跑會在真的壞掉時連叫 24 次，
#    而一個會連叫的死人之鐘三天內就會被靜音。
#
# 4. 裝好之後**驗一次它叫得出聲音**：
#
#      DEADMAN_ALERT_CMD=... deadman.sh --test
#
#    ⚠ 上面每一段偵測邏輯都可以用一個假的時間戳驗（餵它舊的，它就叫）。
#    唯一驗不了的是最後一哩：那則訊息有沒有真的送到人手上。
#    token 過期、chat_id 打錯、bot 被踢出群組 —— 這三件事都不會有任何東西
#    告訴你，因為會告訴你的正是壞掉的那條管道。
#
#    ⚠⚠ 而 --test 說「送出去了」也只代表**指令回了 0**。
#    真正的驗收是你手機上有沒有響。
#
# 傳輸方式（scp / tailscale / syncthing / 一個 HTTP PUT）刻意留給你決定。
# 唯一的硬性要求是：**這條路徑不能需要 Hub 活著才能走**。
# 如果你的死人之鐘是去打 Hub 的 API 問「你還好嗎」，那你沒有死人之鐘。
# ---------------------------------------------------------------------------

set -uo pipefail

# --test：現在就送一則（明確標示為測試的）告警，證明這口鐘叫得出聲音。
#
# ⚠⚠ 為什麼需要它：上面每一段偵測邏輯都驗得了 —— 餵它一個舊的時間戳，
# 它就會叫。但「叫」的定義是**呼叫 DEADMAN_ALERT_CMD**，而那則訊息有沒有
# 真的送到人手上，是這條鏈上唯一沒辦法用假資料驗證的一段。
#
# 一個偵測得到、但叫不出聲音的死人之鐘，跟沒有是一樣的 —— 而且更糟：
# 它讓你以為有人在看。token 過期、chat_id 打錯、bot 被踢出群組，
# 這三件事都不會有任何東西告訴你，因為會告訴你的正是壞掉的那條管道。
#
# ⚠ 它走的是**跟真告警一模一樣的那段程式碼**（下面的 deliver）。
# 另外寫一段「測試用的送出」會測到另一條路徑，那就什麼都沒證明。
TEST_MODE=0
if [ "${1:-}" = "--test" ]; then
	TEST_MODE=1
	shift
fi

STAMP="${1:-/var/lib/deadman/hub.stamp}"
MAX_AGE_HOURS="${DEADMAN_MAX_AGE_HOURS:-26}"

# ⚠ 26 小時，不是 24。早報是每天一則，允許它晚兩小時 ——
# 一次網路抖動不該叫醒你，連續兩天沒有才該。
max_age=$((MAX_AGE_HOURS * 3600))

# deliver 把一則訊息送出去。
#   0 = 送出去了   1 = 送不出去（指令回非零）   2 = 根本沒設告警指令
#
# ⚠ 真告警跟 --test 共用這一段，這是刻意的：一個走另一條路徑的測試，
# 證明的是那條路徑能動，而不是真告警那天能動。
#
# ⚠⚠ 1 跟 2 一定要分開。它們的處理一樣（都是壞的），但**要對人講的話不一樣**：
# 「送不出去」要去查 token 跟網路，「沒設定」要去寫 crontab。
# 把兩件事講成同一句，就是叫人去查一個不存在的問題。
deliver() {
	# ⚠ 這個通知管道必須跟 Hub 用的那個**不一樣**。
	# 兩邊共用一個 Telegram bot，那個 bot 掛掉的時候你會同時失去早報
	# 跟「早報沒來」的告警 —— 一個共同失效點，兩個都靜音。
	#
	# 2026-09-04 實際去量了（token 只比 sha256 前綴，不看值）：
	#
	#   Hub（samplehub1）      token sample… = @Example_Hub_Bot
	#   死人之鐘（sampleagent2） token sample… = @Example_Alert_Bot
	#   兩邊的 chat_id      一樣，而 getChat 回 type=private、id 是正數
	#
	# ⚠ 那個「一樣的 chat_id」不是共同失效點，我一開始把它讀成共同失效點了。
	# Telegram 的私訊 chat_id 就是**收件人本人的 user id**，而每一對
	# bot↔人 是各自獨立的對話。所以靜音 @Example_Hub_Bot 不會靜音
	# @Example_Alert_Bot，封鎖一隻也不會封鎖另一隻。
	# （如果 id 是負數就是群組，那時兩隻送進同一個群才真的是共同失效點。）
	#
	# 所以上面那段話成立：token 分開了，對話也分開了。剩下的共同點是
	# 「同一個人、同一支手機」—— 那個換幾隻 bot 都解決不了，不是這裡的問題。
	local msg="$1"
	if [ -z "${DEADMAN_ALERT_CMD:-}" ]; then
		return 2
	fi
	printf '%s\n' "clawctl deadman: $msg" | sh -c "$DEADMAN_ALERT_CMD"
}

# by_hand 判斷這一次執行是不是有人自己叫的（而不是 cron）。
#
# ⚠⚠ 這是 2026-09-03 真的發生過的事付的學費：那天 02:24（EDT）operator 的手機
# 連響四次，四則訊息長得跟真的機隊全滅一模一樣 ——
#
#	找不到時間戳 … Hub 從來沒有成功送出過早報，或是傳輸那一段斷了。
#
# 而那是我在 sampleagent2 上手動把六種情境跑過一遍，其中四種會叫。
# **他收到的前四則告警，全部是假的，而且他分不出來。**
#
# `--test` 那條路早就有標記，而且旁邊還寫著「半夜三點收到一則『Hub 死了』
# 而其實是有人在測，代價是下一則真的告警會被當成又在測」。
# 註解說對了危險，然後只守了兩扇門的其中一扇 ——
# 手動跑真情境的那扇沒人守，而它送出的是**真告警的原文**。
#
# 判準用 SSH_CONNECTION 或 TTY：cron 兩個都沒有，`ssh host cmd` 有前者，
# 坐在終端機前有後者。它只會**加上**一句話，永遠不會讓真告警變安靜。
by_hand() {
	[ -n "${SSH_CONNECTION:-}" ] || [ -n "${SSH_CLIENT:-}" ] || [ -t 0 ] || [ -t 2 ]
}

alert() {
	local msg="$1" rc=0
	# ⚠ 標記加在**訊息裡**，不是只加在 stderr。手機上看得到的只有訊息。
	if by_hand; then
		msg="$msg

來源：manual；host=$(hostname 2>/dev/null || echo unknown)"
	fi
	echo "clawctl deadman: $msg" >&2

	deliver "$msg" || rc=$?
	if [ "$rc" -eq 0 ]; then
		exit 1
	fi
	if [ "$rc" -eq 2 ]; then
		logger -t clawctl-deadman -p daemon.crit -- "$msg" 2>/dev/null || true
		exit 1
	fi

	# ⚠ 告警送不出去。這是這條鏈的最後一環，而它沒有守望者 ——
	# 沒有任何東西能在「告警管道壞掉」時通知你，因為那正是壞掉的那條管道。
	#
	# 能做的只有一件事：不要把它吞掉。原本這裡不看 exit code，於是一個
	# token 過期的死人之鐘會每天安靜地跑完、什麼都不做、exit 1，
	# 而 cron 預設不會說話。那就變成一個沒有人知道已經失效的守望者 ——
	# 跟這整個專案要修的假綠燈是同一種東西。
	#
	# 所以：落到 syslog（daemon.crit，會被 journal 收），並且在 stderr 上
	# 明確講出來 —— cron 會把 stderr 寄出去或寫進 log，那是另一條路徑。
	echo "clawctl deadman: 告警通道失敗；事件已寫入 syslog 與 cron stderr。" >&2
	logger -t clawctl-deadman -p daemon.crit -- "告警管道失效，原訊息：$msg" 2>/dev/null || true
	exit 1
}

if [ "$TEST_MODE" -eq 1 ]; then
	# ⚠ 訊息要一眼看得出是測試。半夜三點收到一則「Hub 死了」而其實是有人
	# 在測，代價是下一則真的告警會被當成又在測 —— 那比沒收到更糟。
	msg="clawctl deadman 測試（--test）"
	rc=0
	deliver "$msg" || rc=$?
	case "$rc" in
	0)
		echo "告警測試已送出。"
		# ⚠ 順便講出安靜的日子有沒有人在看這條管道。沒設不是錯，
		# 但「以為有設」跟「真的有設」差很多，而平常看不出來。
		if [ -n "${DEADMAN_ALERT_CHECK_CMD:-}" ]; then
			echo "平日健檢：已啟用。"
		else
			echo "平日健檢：未啟用。"
		fi
		exit 0
		;;
	2)
		echo "✗ 告警通道未設定；事件只寫入 syslog。" >&2
		echo "  下一步：在 crontab 設定 DEADMAN_ALERT_CMD，例如 ~/.local/libexec/clawctl/notify-telegram.sh" >&2
		exit 2
		;;
	*)
		echo "✗ 告警通道失敗。" >&2
		echo "  下一步：檢查 DEADMAN_ALERT_CMD 的連線與憑證。" >&2
		exit 1
		;;
	esac
fi

if [ ! -f "$STAMP" ]; then
	# ⚠ 檔案不存在跟檔案很舊是同一件事，不是「還沒開始所以先放過」。
	# 「還沒設定好」在這裡跟「壞掉了」的後果一模一樣：你收不到早報。
	alert "找不到時間戳 $STAMP —— Hub 從來沒有成功送出過早報，或是傳輸那一段斷了。"
fi

stamped=$(cat "$STAMP" 2>/dev/null || echo 0)
case "$stamped" in
*[!0-9]* | '') alert "時間戳 $STAMP 的內容不是一個 unix 時間：'$stamped'" ;;
esac

now=$(date -u +%s)
age=$((now - stamped))

if [ "$age" -lt 0 ]; then
	# 未來的時間戳代表兩邊時鐘對不上。這不是「沒事」，這是量測工具本身壞了。
	alert "時間戳在未來（差 $((-age)) 秒）—— 兩台機器的時鐘對不上，這個檢查現在沒有意義。"
fi

if [ "$age" -gt "$max_age" ]; then
	alert "已經 $((age / 3600)) 小時沒有收到早報（上限 ${MAX_AGE_HOURS} 小時）。Hub、worker、或推播管道其中一個斷了。"
fi

# ---------------------------------------------------------------------------
# 走到這裡代表早報有按時來。今天沒有事情要跟人講。
#
# ⚠⚠ 但正因為今天沒事，這條告警管道今天也**不會被用到**。
# 而它下一次被用到，就是真的出事那天。§5.23 量到的洞就在這裡：
# 死人之鐘的 token 可能三個月前就被撤銷了，沒有任何人會知道，
# 直到它唯一該說話的那一天 —— 而那天它會安靜。
#
# 一條只在出事那天才會被用到的路徑，等於一條沒有被測試的路徑。
# 修法不是更用力地相信它，是找一個不吵到人的方式每天走一次。
#
# `notify-telegram.sh --check` 只問 getMe / getChat（唯讀，不送出任何東西），
# 所以這個檢查不會變成每天被忽略的雜訊 —— 安靜的日子它一個字都不會印。
#
# ⚠ 沒設就跳過，不囉嗦：這是選配，而不是每天警告一次的理由。
#   `--test` 會告訴你到底有沒有設。
# ⚠ CHECK_CMD 的輸出會被放進告警訊息、寫進 syslog。指過去的東西不准印秘密。
#   （`notify-telegram.sh --check` 有一條測試在守這件事。）
if [ -n "${DEADMAN_ALERT_CHECK_CMD:-}" ]; then
	if ! check_out=$(sh -c "$DEADMAN_ALERT_CHECK_CMD" 2>&1); then
		# ⚠ 這裡走 alert()，不是自己印一行就算了 —— 它會先試著真的送出去
		# （管道可能只壞了一半，那就讓人的手機收到），送不出去才落 syslog。
		# 而不管哪一種，exit 都是非零，cron 那條路也會叫。
		# ⚠ 健檢可能什麼都沒印就回非零。那時原本會送出
		# 「管道自己壞了：」後面空一片 —— 看起來像訊息被截斷，
		# 而收到的人會先去查訊息怎麼壞了，而不是去查管道。
		# 沒有理由就要講「沒有理由」。（跟 --check 裡網路不通那段同一個坑。）
		alert "告警通道健檢失敗：$(
			printf '%s' "${check_out:-（健檢沒有印任何訊息，只回了非零）}" | tr '\n' ' | '
		)"
	fi
fi

# ⚠ 正常情況安靜地結束。死人之鐘每天報平安，就會變成每天被忽略的雜訊，
# 然後它真的響的那天也會被忽略。
exit 0
