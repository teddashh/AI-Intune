#!/usr/bin/env bash
# Grafana：裝、接上本機 Prometheus、把 git 裡那一頁 provisioning 進去。
#
#   sudo ./ops/prometheus/install-grafana.sh
#
# Grafana 不在 Ubuntu 26.04 的倉庫裡，所以這支會加 Grafana 官方 APT repo。
# 加別人的 repo 有代價（他們的簽章金鑰從此對你的 apt 有效），
# 但那正是「跟著上游、不自己維護 binary」要付的錢 ——
# 這台上已經有 tailscale / docker / github-cli / nodesource 四個同類的了。
#
# ⚠⚠ 這支**不會**安靜地跳過。任何一步沒做成就 exit 非零並說出下一步該做什麼。
#     這條規矩是 2026-09-05 用血換的：前一版在拿不到套件時印
#     「前兩段是好的」然後 exit 0，於是 operator 讀到「做完了」、
#     回報「跑完了」，而 Grafana 根本沒裝。
#     一支會說「我沒做成」的腳本，比一支友善地結束的腳本有用得多。
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
	echo "要 root：sudo $0" >&2
	exit 1
fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
KEY=/etc/apt/keyrings/grafana.gpg
LIST=/etc/apt/sources.list.d/grafana.list

# ⚠ unattended-upgrades / apt-daily.timer 可能正握著鎖。等，不要當場死。
APT=(apt-get -o DPkg::Lock::Timeout=300)

have_grafana() { apt-cache policy grafana 2>/dev/null | grep -q 'Candidate: [0-9]'; }

echo "══ 1/6 倉庫"
if have_grafana; then
	echo "   apt 已經找得到 grafana（$(apt-cache policy grafana | awk '/Candidate:/{print $2}')），不動倉庫"
else
	install -d -m 0755 /etc/apt/keyrings
	if [ ! -s "$KEY" ]; then
		# ⚠ curl 失敗時 gpg 仍會產生 0 byte 的檔，然後 apt-get update
		#   只印一行 NO_PUBKEY 警告就繼續 —— 又是一個安靜地沒裝到。
		TMPKEY="$(mktemp)"
		trap 'rm -f "$TMPKEY"' EXIT
		curl -fsSL --max-time 30 https://apt.grafana.com/gpg.key | gpg --dearmor >"$TMPKEY"
		[ -s "$TMPKEY" ] || {
			echo "拿不到 Grafana 的簽章金鑰（網路？）" >&2
			exit 1
		}
		install -m 0644 "$TMPKEY" "$KEY"
		echo "   金鑰 $KEY  sha256=$(sha256sum "$KEY" | cut -c1-12)…"
	else
		echo "   金鑰已經有了  sha256=$(sha256sum "$KEY" | cut -c1-12)…"
	fi

	echo "deb [signed-by=$KEY] https://apt.grafana.com stable main" >"$LIST"

	# 先試只更新這一份 list（快）。不成就整包更新 —— 不要因為「快」而放棄「成」。
	"${APT[@]}" update -o Dir::Etc::sourcelist="sources.list.d/grafana.list" \
		-o Dir::Etc::sourceparts="-" -o APT::Get::List-Cleanup="0" || true
	if ! have_grafana; then
		echo "   只更新單一 list 沒生效，改整包 apt-get update"
		"${APT[@]}" update
	fi
fi

have_grafana || {
	cat >&2 <<EOF
加完倉庫 apt 還是找不到 grafana 這個套件。**沒有裝成，不要當作裝好了。**
自己看一眼：
  cat $LIST
  apt-cache policy grafana
  apt-get update            # 看有沒有 NO_PUBKEY / 404
EOF
	exit 1
}

echo "══ 2/6 安裝"
DEBIAN_FRONTEND=noninteractive "${APT[@]}" install -y grafana
# apt-get install 成功 ≠ 套件真的在。明著問 dpkg。
dpkg -s grafana >/dev/null 2>&1 || {
	echo "apt 說成功，但 dpkg -s grafana 查不到 —— 沒有裝成" >&2
	exit 1
}
echo "   $(dpkg-query -W -f='${Package} ${Version}' grafana)"

echo "══ 3/6 provisioning（資料來源與儀表板都從 git 來，不從瀏覽器來）"
install -d -m 0755 /etc/grafana/provisioning/datasources
install -d -m 0755 /etc/grafana/provisioning/dashboards
install -d -m 0755 -o grafana -g grafana /var/lib/grafana/dashboards
install -m 0644 "$HERE/grafana/datasource.yml" /etc/grafana/provisioning/datasources/clawctl.yml
install -m 0644 "$HERE/grafana/dashboard-provider.yml" /etc/grafana/provisioning/dashboards/clawctl.yml

# ⚠ Grafana 載不進壞掉的儀表板時只在 log 裡寫一行就繼續開機 ——
#   使用者看到的是「儀表板列表是空的」。所以先自己驗。
python3 -m json.tool "$HERE/grafana/clawctl.json" >/dev/null || {
	echo "clawctl.json 不是合法的 JSON" >&2
	exit 1
}
install -m 0644 -o grafana -g grafana "$HERE/grafana/clawctl.json" /var/lib/grafana/dashboards/clawctl.json

echo "══ 4/6 關掉回報與更新檢查"
DEFAULTS=/etc/default/grafana-server
touch "$DEFAULTS"
# ⚠ 只 append，不 source（跟 hub.env / node-exporter 同一條規矩）。
if ! grep -q '^GF_ANALYTICS_REPORTING_ENABLED=' "$DEFAULTS"; then
	cp -a "$DEFAULTS" "$DEFAULTS.bak-$STAMP"
	cat >>"$DEFAULTS" <<'ENV'

# clawctl：不要往外回報使用狀況，不要檢查更新（apt 管版本）
GF_ANALYTICS_REPORTING_ENABLED=false
GF_ANALYTICS_CHECK_FOR_UPDATES=false
GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false
GF_USERS_ALLOW_SIGN_UP=false
GF_AUTH_ANONYMOUS_ENABLED=false
ENV
	echo "   已加入"
else
	echo "   已經有了，沒動"
fi

echo "══ 5/6 啟動"
systemctl daemon-reload
systemctl enable --now grafana-server
systemctl restart grafana-server
code=000
for _ in $(seq 1 30); do
	code="$(curl -s -o /dev/null -w '%{http_code}' -m 3 http://localhost:3000/api/health || true)"
	[ "$code" = "200" ] && break
	sleep 2
done
printf '   grafana-server %s   /api/health %s\n' "$(systemctl is-active grafana-server)" "$code"
[ "$code" = "200" ] || {
	echo "Grafana 沒回 200 —— journalctl -u grafana-server -n 50" >&2
	exit 1
}

echo "══ 6/6 驗 provisioning 真的生效（服務 active ≠ 東西載進去了）"
AUTH='admin:admin'
fail=0
if curl -s -m 5 -u "$AUTH" http://localhost:3000/api/datasources/uid/clawctl-prom | grep -q '"uid":"clawctl-prom"'; then
	echo "   資料來源 clawctl-prom  ✅"
else
	echo "   資料來源 clawctl-prom  ❌  journalctl -u grafana-server | grep -i provision" >&2
	fail=1
fi
if curl -s -m 5 -u "$AUTH" http://localhost:3000/api/dashboards/uid/clawctl-fleet | grep -q '"uid": *"clawctl-fleet"'; then
	echo "   儀表板 clawctl-fleet   ✅"
else
	echo "   儀表板 clawctl-fleet   ❌  journalctl -u grafana-server | grep -i dashboard" >&2
	fail=1
fi
[ "$fail" -eq 0 ] || {
	echo "服務起來了但 provisioning 沒生效 —— 那會是一頁空面板，不要當作好了" >&2
	exit 1
}

cat <<EOF

Grafana  http://100.64.200.2:3000/d/clawctl-fleet

⚠ 第一次登入 admin / admin，它會當場要你改掉。改。
  綁在 * 上，跟 9090/9093/9100 同一個曝光面（samplehub1 沒有公網 IP，
  只有 LAN 192.168.103.x 跟 tailnet）。要收更緊：在 $DEFAULTS 加
  GF_SERVER_HTTP_ADDR=100.64.200.2 —— 代價是 tailscale 沒起來時 Grafana 也起不來。

畫出來之後拿這支對答案（它會說哪一格是空的、以及那個空是哪一種）：
  ./ops/prometheus/check-dashboard.sh
EOF
