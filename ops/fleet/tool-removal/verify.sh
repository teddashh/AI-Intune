#!/usr/bin/env bash
set -euo pipefail
dir=$(cd "$(dirname "$0")" && pwd)
output=$(bash "$dir/inventory.sh" "$@" --json)
printf '%s\n' "$output"
failed=0
# Reference scans and project data are evidence for a human (history files,
# kept git worktrees); they never fail verify. Everything else must be 0.
printf '%s' "$output" | python3 -c '
import json,sys
v=json.load(sys.stdin)
info=lambda s: s["source"].startswith("reference-files ") or s["source"].startswith("project data")
for s in v["sources"]:
    if info(s) and s["count"]>0: print("note (not a failure): %s: %d" % (s["source"], s["count"]), file=sys.stderr)
sys.exit(any(s["count"]>0 and not info(s) for s in v["sources"]))' || failed=1
for file in "$HOME/.claude/settings.json" "$HOME/.codex/hooks.json" "$HOME/.gemini/settings.json"; do
 [[ ! -f $file ]] || python3 - "$file" <<'PY' || failed=1
import json,sys
try: json.load(open(sys.argv[1]))
except (ValueError,OSError):
 print('invalid JSON:',sys.argv[1]); sys.exit(1)
PY
done
exit "$failed"
