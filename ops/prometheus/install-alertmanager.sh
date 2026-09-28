#!/usr/bin/env bash
# 把 Alertmanager 接上：Prometheus 送告警過去，Alertmanager 分組、抑制、送 Telegram。
#
#   sudo ./ops/prometheus/install-alertmanager.sh
#
# ⚠⚠ 這支會讓告警**真的送進 Telegram**，而且是在跑完之後大約 5 分鐘
#     （group_wait）自動開始送，**不會再問你一次**。
#     最後那段「現在會送出去的」是事後印的，不是事前的確認 ——
#     想先知道會送什麼，跑之前先看 http://localhost:9090/alerts。
#
# ⚠⚠ token 的處理規矩（跟 ops/upgrade-hub.sh 同一條）：
#     - 只用 sed 抽出單一變數，**絕不 source** 那個檔案
#     - 全程不 echo、不寫進 log、不進 alertmanager.yml
#     - 落地成 /etc/prometheus/telegram.token，0600，owner prometheus
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
	echo "要 root：sudo $0" >&2
	exit 1
fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
REAL_USER="${SUDO_USER:-$USER}"
REAL_HOME="$(getent passwd "$REAL_USER" | cut -d: -f6)"

HUB_ENV="$REAL_HOME/.config/clawctl/hub.env"
[ -r "$HUB_ENV" ] || {
	echo "讀不到 $HUB_ENV" >&2
	exit 1
}

# ⚠ 只抽這一個變數的值，不 source。hub.env 裡還有別的東西。
NOTIFY_ENV="$(sed -n 's/^CLAWCTL_NOTIFY_ENV=//p' "$HUB_ENV" | tr -d '"'"'" | head -1)"
NOTIFY_ENV="${NOTIFY_ENV/#\~/$REAL_HOME}"
[ -r "$NOTIFY_ENV" ] || {
	echo "讀不到通知設定檔（CLAWCTL_NOTIFY_ENV 指到 $NOTIFY_ENV）" >&2
	exit 1
}
echo "══ 通知設定讀自 $NOTIFY_ENV"

CHAT_ID="$(sed -n 's/^TELEGRAM_CHAT_ID=//p' "$NOTIFY_ENV" | tr -d '"'"'" | head -1)"
[ -n "$CHAT_ID" ] || {
	echo "$NOTIFY_ENV 裡沒有 TELEGRAM_CHAT_ID" >&2
	exit 1
}

echo "══ 1/5 放 token（0600，owner prometheus，全程不印出來）"
TOKEN_FILE=/etc/prometheus/telegram.token
install -m 0600 -o prometheus -g prometheus /dev/null "$TOKEN_FILE"
sed -n 's/^TELEGRAM_BOT_TOKEN=//p' "$NOTIFY_ENV" | tr -d '"'"'" | head -1 | tr -d '\n' >"$TOKEN_FILE"
[ -s "$TOKEN_FILE" ] || {
	echo "token 是空的 —— 一個帶著空 token 的 Alertmanager 會安靜地送不出去" >&2
	rm -f "$TOKEN_FILE"
	exit 1
}
printf '   %s  %s  sha256=%s…\n' "$TOKEN_FILE" \
	"$(stat -c '%a %U' "$TOKEN_FILE")" \
	"$(sha256sum "$TOKEN_FILE" | cut -c1-12)"

echo "══ 2/5 產生 alertmanager.yml（chat_id 代入，token 走 bot_token_file）"
AM=/etc/prometheus/alertmanager.yml
[ -f "$AM" ] && cp -a "$AM" "$AM.bak-$STAMP" && chmod 600 "$AM.bak-$STAMP"
sed "s|__CHAT_ID__|$CHAT_ID|" "$HERE/alertmanager.yml.tmpl" >"$AM"
# ⚠ chat_id 在裡面，不給全世界讀。640 讓 prometheus 這個 group 讀得到。
chown root:prometheus "$AM"
chmod 640 "$AM"

echo "══ 3/5 讓 Prometheus 知道 Alertmanager 在哪"
PROM=/etc/prometheus/prometheus.yml
if ! grep -q '^alerting:' "$PROM"; then
	cp -a "$PROM" "$PROM.bak-$STAMP"
	cat >>"$PROM" <<'YAML'

alerting:
  alertmanagers:
    - static_configs:
        - targets: ["localhost:9093"]
YAML
	echo "   已加入 alerting 區段"
else
	echo "   已經有了，沒動"
fi

echo "══ 4/5 檢查（過不了就不重啟）"
amtool check-config "$AM"
promtool check config "$PROM"

echo "══ 5/5 重啟"
systemctl restart prometheus-alertmanager
systemctl reload prometheus || systemctl restart prometheus
sleep 3
for s in prometheus prometheus-alertmanager; do
	printf '   %-28s %s\n' "$s" "$(systemctl is-active "$s")"
done

echo
echo "現在會送出去的（Alertmanager 分組之後的實際則數）："
sleep 5
curl -s http://localhost:9093/api/v2/alerts/groups 2>/dev/null |
	python3 -c 'import json,sys
gs=json.load(sys.stdin)
if not gs:
    print("   （沒有 —— 可能還沒收到，等一分鐘再看 http://localhost:9093）")
for g in gs:
    m=g["labels"].get("machine","?")
    print("   %-10s %d 件：%s" % (m, len(g["alerts"]),
        ", ".join(sorted({a["labels"]["alertname"] for a in g["alerts"]}))))' 2>/dev/null ||
	echo "   （讀不到，看 http://localhost:9093）"

echo
echo "靜音一台：amtool silence add machine=sampleagent1 -d 720h -c '我自己處理'"
echo "看目前靜音：amtool silence query"
