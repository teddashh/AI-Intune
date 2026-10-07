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
	echo "Cannot read $DASH" >&2
	exit 1
}

echo "Dashboard $DASH"
echo "Data source $PROM"
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
            print("   \033[31m✗\033[0m %-34s Cannot reach Prometheus: %s" % (legend, err))
            continue
        if data.get("status") != "success":
            bad += 1
            print("   \033[31m✗\033[0m %-34s %s" % (legend, data.get("error", "?")))
            print("       %s" % expr)
            continue
        n = len(data["data"]["result"])
        if n == 0:
            empty += 1
            print("   \033[33m○\033[0m %-34s \033[33m0 series — this panel will be empty\033[0m" % legend)
            print("       %s" % expr)
        else:
            ok += 1
            sample = data["data"]["result"][0]
            labels = sample["metric"]
            who = labels.get("machine") or labels.get("instance") or "?"
            val = sample.get("value", ["", "?"])[1]
            print("   \033[32m✓\033[0m %-34s %d series   e.g.: %s = %s" % (legend, n, who, val))
    print()

print("─" * 60)
print("With data %d   \033[33mEmpty %d\033[0m   \033[31mBroken query %d\033[0m" % (ok, empty, bad))

if bad:
    print("\nBroken query = typo in metric name or syntax error, must fix")
if empty:
    print("""
Empty is not necessarily wrong, but you must first clarify which kind it is:
  (a) That event really did not happen (e.g. no credentials expiring soon) — ok
  (b) Typo in metric name / conflicting label (exported_machine) / job not scraping — bug
How to tell: remove label filter and query again. If the entire metric family is empty, it is (b)""")

sys.exit(1 if (bad or empty) else 0)
PY
