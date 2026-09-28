#!/usr/bin/env bash
# 拿真的 Prometheus parser 去讀真的 /metrics。
#
# 為什麼需要這一支：cmd/clawctl-hub/metrics.go 手寫 exposition format，換掉了
# 一棵相依樹。代價就是「格式對不對」沒有第三方會告訴你 —— 而
# cmd/clawctl-hub/metrics_test.go 裡的 parseExposition 是同一個人寫的，
# 它證明得了那兩份東西一致，證明不了它們對。**自證不算數。**
#
# 所以這一支用 prometheus_client（Prometheus 官方 Python client）的
# text parser 去讀**真的跑著的 Hub** 吐出來的東西。
#
# ⚠⚠ 它不在 go test 裡，是刻意的：它需要一個跑著的 Hub 跟一個 Python 環境，
# 塞進單元測試只會變成一支平常被 skip 掉的測試 —— 而一支永遠 skip 的測試
# 在 CI 上跟綠燈長得一模一樣。
#
# **換完 Hub 就跑一次這個。** 它要抓的是「改完之後這一頁還解析得動嗎」，
# 而那件事只有在真的換上去之後才問得出來。
#
# 用法：ops/check-metrics.sh [URL]
#   CLAWCTL_PY=/path/to/python   指定有裝 prometheus_client 的 python
set -euo pipefail

URL="${1:-http://100.64.200.2:8787/metrics}"

# --- 找一個裝了 prometheus_client 的 python。找不到就大聲失敗。
#
# ⚠ 這裡刻意**不** skip。「沒有 parser 所以跳過」跟「parser 說沒問題」
# 對看輸出的人來說會長得一樣，而那正好是這支腳本存在要防的那種答案。
# ⚠ CLAWCTL_PY 也要被檢查，不能因為「是人明講的」就相信它。
#
# 原本這裡是「有設就直接用」，於是 CLAWCTL_PY 打錯字或指到一個沒裝
# prometheus_client 的 python 時，腳本不會講出真正的原因 —— 它會一路跑到
# 下面才用一個難懂的方式炸掉，而上面那段「怎麼建 venv」的說明**永遠印不出來**。
# 那段說明正是為了這種情況寫的。
#
# 這個 bug 是協力的 codex 在寫測試時撞出來的：它為了逼出「找不到 parser」
# 那條路徑，只好去把 PATH 清空、再用 BASH_ENV 攔截 command -v ——
# 一個測試需要那麼用力才問得到某條路徑，通常是那條路徑本身有問題。
usable_py() {
  [ -n "$1" ] && command -v "$1" >/dev/null 2>&1 && "$1" -c 'import prometheus_client' 2>/dev/null
}

# ⚠⚠ 人明講了 CLAWCTL_PY 而它不能用的時候，**直接停**，不要退回去找別的。
#
# 這跟 ops/notify-telegram.sh 裡那條規則是同一條：設定檔只負責補上沒設的那個，
# 不負責蓋掉呼叫端設好的。你指定了要用哪一個 python，結果腳本安靜地換成另一個 ——
# 那份「驗過了」講的就不是你問的那件事。而它最惡劣的形式是這樣：
# 你以為在測一個乾淨的環境，它其實去打了線上的 Hub。
PY=""
if [ -n "${CLAWCTL_PY:-}" ]; then
  if usable_py "$CLAWCTL_PY"; then
    PY="$CLAWCTL_PY"
  else
    echo "⚠ CLAWCTL_PY=$CLAWCTL_PY 不能用（不存在，或裡面沒有 prometheus_client）。" >&2
    echo "  這支腳本沒有跑，不是「通過了」。要換一個就改 CLAWCTL_PY，要自動找就不要設它。" >&2
    exit 2
  fi
fi
if [ -z "$PY" ]; then
  for c in ./.venv/bin/python /tmp/promv/bin/python python3; do
    if usable_py "$c"; then PY="$c"; break; fi
  done
fi
if [ -z "$PY" ]; then
  cat >&2 <<'EOF'
找不到裝了 prometheus_client 的 python —— 這支腳本沒有跑，不是「通過了」。

  python3 -m venv /tmp/promv && /tmp/promv/bin/pip install prometheus_client
  CLAWCTL_PY=/tmp/promv/bin/python ops/check-metrics.sh
EOF
  exit 2
fi

echo "== 抓 $URL"
body="$(mktemp)"; trap 'rm -f "$body"' EXIT
code="$(curl -sS -o "$body" -w '%{http_code}' --max-time 10 "$URL")"
if [ "$code" != "200" ]; then
  echo "HTTP $code —— 不是 200。內文前幾行：" >&2
  head -3 "$body" >&2
  exit 1
fi

"$PY" - "$body" <<'PYEOF'
import sys
from prometheus_client.parser import text_string_to_metric_families

raw = open(sys.argv[1], encoding="utf-8").read()

# --- 反向對照：先確認這個 parser 真的會拒絕壞東西。
#
# ⚠⚠ 一個「什麼都收」的驗證器給出的綠燈，比沒有驗證更糟 —— 它會讓人
# 停止懷疑。所以在相信它說「你的格式沒問題」之前，先餵它一頁一定壞的東西，
# 確認它真的會叫。這一段失敗代表驗證器本身不能信，不是格式沒問題。
broken = 'x{bad="unclosed 1\n'
try:
    list(text_string_to_metric_families(broken))
    print("反向對照失敗：parser 連壞掉的輸入都收 —— 它的綠燈不能信", file=sys.stderr)
    sys.exit(3)
except Exception:
    pass

fams = list(text_string_to_metric_families(raw))
samples = [s for f in fams for s in f.samples]
if not fams:
    print("parser 讀完是空的 —— 一頁沒有任何指標的 metrics", file=sys.stderr)
    sys.exit(1)

# ⚠ 重複的 series 會讓 Prometheus 丟掉整頁。兩台同名機器就會中。
seen = {}
for s in samples:
    key = (s.name, tuple(sorted(s.labels.items())))
    seen[key] = seen.get(key, 0) + 1
dupes = [k for k, n in seen.items() if n > 1]
if dupes:
    for k in dupes:
        print(f"重複的 series：{k}", file=sys.stderr)
    print("⚠ Prometheus 會把整頁丟掉，不是只丟這幾行。", file=sys.stderr)
    sys.exit(1)

# ⚠ 這一頁不准宣稱健康。理由見 metrics.go 開頭：Hub 死掉的時候這個端點
# 不是回傳壞消息，它是連線失敗。自證不算數。
for f in fams:
    if f.name.endswith(("_up", "_healthy", "_ok", "_alive")):
        print(f"{f.name}：這一頁不准宣稱健康", file=sys.stderr)
        sys.exit(1)

print(f"官方 parser 讀過了：{len(fams)} 個指標、{len(samples)} 行 sample")
machines = sorted({s.labels.get("machine", "") for s in samples if "machine" in s.labels})
print(f"名冊上 {len(machines)} 台：{', '.join(machines)}")
PYEOF
