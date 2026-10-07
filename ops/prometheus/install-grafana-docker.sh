#!/usr/bin/env bash
# Grafana，跑在容器裡。**不需要 root。**
#
#   ./ops/prometheus/install-grafana-docker.sh
#
# ⚠⚠ 為什麼不是 install-grafana.sh（apt）：
#
#   那一支要 root，而且會把 Grafana 官方的 APT repo 跟簽章金鑰加進這台機器 ——
#   從此他們的金鑰對你**整個 apt** 有效。那筆錢對「一頁儀表板」來說太貴了。
#   samplehub1 上 sudo 要密碼（只有 openclaw-pkg-install 跟 chattr 是 NOPASSWD），
#   而 example-user 本來就在 docker group 裡 —— 跑容器是那個 group 的正當用途，
#   不是繞過什麼。兩支都留著：要 apt 版的人跑那一支，這一支是預設。
#
# ⚠ 只綁 tailnet 位址，不綁 0.0.0.0。--network host 配上預設的 http_addr
#   會把 :3000 開在**每一張網卡**上，包含公網那張。一頁把整個機隊的狀態
#   攤開來的儀表板，不應該用「大概沒有人會掃到」當作它的存取控制。
set -euo pipefail

IMAGE="grafana/grafana-oss:13.0.2"
NAME="clawctl-grafana"
HOME_DIR="${CLAWCTL_GRAFANA_HOME:-$HOME/.local/share/clawctl/grafana}"
# ⚠ 位址從 CLAWCTL_LISTEN 推，不要再寫死一份。兩份會有一天不一樣。
ADDR="${CLAWCTL_GRAFANA_ADDR:-$(sed -n 's/^CLAWCTL_LISTEN=//p' "$HOME/.config/clawctl/hub.env" 2>/dev/null | tr -d '"' | cut -d: -f1)}"
ADDR="${ADDR:-127.0.0.1}"
PORT="${CLAWCTL_GRAFANA_PORT:-3000}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

command -v docker >/dev/null || { echo "Docker not found" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "Cannot connect to docker daemon (is example-user in the docker group?)" >&2; exit 1; }

echo "══ 1/4 Configuration files → $HOME_DIR"
# ⚠ 設定檔抄一份到 $HOME_DIR，不要直接 mount git worktree ——
#   容器的生命週期不該綁在某個人的 checkout 上（分支一切走，儀表板就空了）。
install -d -m 0755 "$HOME_DIR"/provisioning/datasources "$HOME_DIR"/provisioning/dashboards "$HOME_DIR"/dashboards
install -m 0644 "$HERE/grafana/datasource.yml"         "$HOME_DIR/provisioning/datasources/clawctl.yml"
install -m 0644 "$HERE/grafana/dashboard-provider.yml" "$HOME_DIR/provisioning/dashboards/clawctl.yml"
install -m 0644 "$HERE/grafana/clawctl.json"           "$HOME_DIR/dashboards/clawctl.json"

echo "══ 2/4 Starting container ($IMAGE, bound to $ADDR:$PORT)"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" \
	--network host \
	--restart unless-stopped \
	-e GF_SERVER_HTTP_ADDR="$ADDR" \
	-e GF_SERVER_HTTP_PORT="$PORT" \
	-e GF_ANALYTICS_REPORTING_ENABLED=false \
	-e GF_ANALYTICS_CHECK_FOR_UPDATES=false \
	-v clawctl-grafana-data:/var/lib/grafana \
	-v "$HOME_DIR/provisioning":/etc/grafana/provisioning:ro \
	-v "$HOME_DIR/dashboards":/var/lib/grafana/dashboards:ro \
	"$IMAGE" >/dev/null

echo "══ 3/4 Waiting for it to start"
for i in $(seq 1 60); do
	code="$(curl -s -o /dev/null -w '%{http_code}' -m 3 "http://$ADDR:$PORT/api/health" || true)"
	[ "$code" = "200" ] && break
	sleep 2
done
[ "${code:-}" = "200" ] || { echo "Grafana did not start (last HTTP $code). docker logs $NAME" >&2; exit 1; }
curl -s "http://$ADDR:$PORT/api/health"; echo

echo "══ 4/4 Verifying provisioning took effect"
# ⚠⚠ 這一段是重點。uid 對不上的話 Grafana **不會報錯**，它會畫出一整頁
#    空面板 —— 正好是這個專案最痛恨的那種失敗：看起來像「機隊很安靜」。
#    所以不看「容器起來了沒」，看「資料源在不在、那一頁在不在」。
ds="$(curl -s -u "${GF_ADMIN:-admin}:${GF_PASS:-admin}" "http://$ADDR:$PORT/api/datasources/uid/clawctl-prom" || true)"
echo "$ds" | grep -q '"uid":"clawctl-prom"' || { echo "Data source clawctl-prom was not provisioned: $ds" >&2; exit 1; }
echo "   Data source clawctl-prom ✓"
dash="$(curl -s -u "${GF_ADMIN:-admin}:${GF_PASS:-admin}" "http://$ADDR:$PORT/api/search?query=" || true)"
echo "$dash" | grep -q 'clawctl' || { echo "Dashboard was not provisioned: $dash" >&2; exit 1; }
echo "   Dashboard ✓"

cat <<MSG

Grafana  http://$ADDR:$PORT/d/clawctl-fleet
Credentials  admin / admin (password change enforced on first login)

Config source: ops/prometheus/grafana/clawctl.json (allowUiUpdates: false)
Update command: ./ops/prometheus/install-grafana-docker.sh

Verification: ./ops/prometheus/check-dashboard.sh
MSG
