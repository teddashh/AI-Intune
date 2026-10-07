#!/usr/bin/env bash
# samplehub1 上一次把整條觀測鏈裝好：Prometheus → Alertmanager → Grafana。
#
#   sudo CLAWCTL_FLEET_JSON=/path/to/fleet.json ./ops/prometheus/install.sh
#
# 第 1 段的站點值（fleet.json、prometheus.yml）由環境變數給，見 install-prometheus.sh。
#
# 這支不重寫那兩支既有的腳本，它就是照順序把它們跑一遍，再補上 Grafana。
# 想單獨重跑某一段，那兩支還是可以自己執行。
#
# ⚠⚠ 第 2 段（Alertmanager）跑完之後，大約 5 分鐘（group_wait）會**真的**
#     開始送 Telegram，而且不會再問你一次。想先看會送什麼：
#     跑之前開 http://localhost:9090/alerts。
#
# ⚠  Grafana 不在 Ubuntu 26.04 的倉庫裡，第 3 段會加 Grafana 官方的 APT repo。
#     加別人的 repo 是有代價的（他們的簽章金鑰從此對你的 apt 有效），
#     但這正是「跟著上游、不自己維護 binary」要付的那筆錢 ——
#     跟 tailscale.list / docker.list / github-cli.list 同一類，這台上已經有四個了。
#
# ⚠⚠ 三段都必須真的成功，這支才算跑完。**沒有「不致命所以跳過」這種事** ——
#     前一版就是這樣寫的，於是它在 Grafana 沒裝成的時候印了
#     「前兩段是好的」並 exit 0，operator 據此回報「跑完了」。
#     一支友善地結束的腳本，比一支說「我沒做成」的腳本危險得多。
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
	echo "要 root：sudo $0" >&2
	exit 1
fi

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

hr() { printf '\n\033[1m━━━━━━ %s ━━━━━━\033[0m\n' "$*"; }

hr "第 1 段 / 共 3 段：Prometheus + node_exporter"
"$HERE/install-prometheus.sh"

hr "第 2 段 / 共 3 段：Alertmanager（跑完約 5 分鐘後開始送 Telegram）"
"$HERE/install-alertmanager.sh"

hr "第 3 段 / 共 3 段：Grafana"
# ⚠⚠ 2026-09-05：這一段原本是內嵌的，而且在拿不到套件時印「前兩段是好的」
#     然後 exit 0 —— operator 讀到「做完了」、回報「跑完了」，Grafana 根本沒裝。
#     /var/log/apt/history.log 裡連一筆 install grafana 都沒有。
#     現在拆成獨立一支，而且它不會安靜地跳過：沒做成就 exit 非零，
#     set -e 讓這裡跟著失敗。三段都要真的成，才算跑完。
"$HERE/install-grafana.sh"

hr "三段都做完了"
cat <<EOF
Prometheus    http://100.64.200.2:9090
Alertmanager  http://100.64.200.2:9093
Grafana       http://100.64.200.2:3000/d/clawctl-fleet

對答案：./ops/prometheus/check-dashboard.sh
靜音一台：amtool silence add machine=sampleagent1 -d 720h -c '我自己處理'
EOF
