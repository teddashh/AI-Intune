#!/usr/bin/env bash
# 把 clawctl.json 裡每一個 panel 的查詢，真的丟到 Prometheus 上跑一次。
#
#   ./ops/prometheus/check-dashboard.sh [http://localhost:9090]
#
# 為什麼要有這支：
#   Grafana 對「查詢語法沒錯，但回傳 0 條序列」是完全沉默的 —— 它照樣把
#   面板畫出來，只是裡面空的。一頁空儀表板看起來像「機隊很安靜」，
#   實際上可能是指標名打錯、label 撞名、或 job 根本沒在抓。
#   這正是這個專案的地基一：一個乾淨的、沒有壞消息的答案，
#   第一個要懷疑的是你有沒有問對地方。
#
# 這支不會改任何東西，只讀。回傳碼：有任何 panel 是空的就 1。
set -uo pipefail

PROM="${1:-http://localhost:9090}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DASH="$HERE/grafana/clawctl.json"

[ -r "$DASH" ] || {
	echo "讀不到 $DASH" >&2
	exit 1
}

echo "儀表板 $DASH"
echo "資料來源 $PROM"
echo

python3 - "$DASH" "$PROM" <<'PY'
import json, sys, urllib.parse, urllib.request, urllib.error

dash_path, prom = sys.argv[1], sys.argv[2].rstrip("/")
dash = json.load(open(dash_path))

def query(expr):
    url = prom + "/api/v1/query?" + urllib.parse.urlencode({"query": expr})
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            return json.load(r), None
    except urllib.error.HTTPError as e:
        # Prometheus 把 PromQL 的錯誤放在 400 的 body 裡，不是只給狀態碼。
        try:
            return json.load(e), None
        except Exception:
            return None, "HTTP %s" % e.code
    except Exception as e:
        return None, str(e)

empty = bad = ok = 0

for p in dash["panels"]:
    targets = p.get("targets") or []
    if not targets:
        continue
    print("\033[1m%s\033[0m" % p["title"])
    for t in targets:
        expr = t.get("expr", "")
        legend = t.get("legendFormat", t.get("refId", ""))
        data, err = query(expr)
        if err is not None:
            bad += 1
            print("   \033[31m✗\033[0m %-34s 打不到 Prometheus：%s" % (legend, err))
            continue
        if data.get("status") != "success":
            bad += 1
            print("   \033[31m✗\033[0m %-34s %s" % (legend, data.get("error", "?")))
            print("       %s" % expr)
            continue
        n = len(data["data"]["result"])
        if n == 0:
            empty += 1
            print("   \033[33m○\033[0m %-34s \033[33m0 條序列 —— 這一格會是空的\033[0m" % legend)
            print("       %s" % expr)
        else:
            ok += 1
            sample = data["data"]["result"][0]
            labels = sample["metric"]
            who = labels.get("machine") or labels.get("instance") or "?"
            val = sample.get("value", ["", "?"])[1]
            print("   \033[32m✓\033[0m %-34s %d 條   例：%s = %s" % (legend, n, who, val))
    print()

print("─" * 60)
print("有資料 %d   \033[33m空的 %d\033[0m   \033[31m查詢壞掉 %d\033[0m" % (ok, empty, bad))

if bad:
    print("\n查詢壞掉＝指標名寫錯或語法錯，一定要修。")
if empty:
    print("""
空的不一定是錯的，但一定要先問清楚是哪一種：
  (a) 那件事真的沒發生（例：沒有任何憑證即將到期）—— 沒問題
  (b) 指標名打錯 / label 撞名（exported_machine）/ job 沒在抓 —— 是 bug
分辨方法：拿掉 label 篩選再查一次。整個指標族都空的話是 (b)。""")

sys.exit(1 if (bad or empty) else 0)
PY
