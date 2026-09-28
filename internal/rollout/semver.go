// Package rollout 放部署預覽與批次判決的純邏輯。
//
// semver 判定也放在這裡，因為 Hub 預覽與 agent activation 必須回答同一個
// engines 問題；拆兩份 parser 會讓預覽說能裝、agent 卻在動手前拒絕。
package rollout

import (
	"strconv"
	"strings"
)

type semVersion [3]int64

func parseSemVersion(raw string) (semVersion, bool) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "v")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return semVersion{}, false
	}
	var v semVersion
	for i, part := range parts {
		if part == "" {
			return semVersion{}, false
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n < 0 {
			return semVersion{}, false
		}
		v[i] = n
	}
	return v, true
}

// NodeSatisfiesRange 只實作 artifact 契約實際需要的 AND comparisons 與 ||。
// 看不懂一定回 understood=false；^、~、x 或 hyphen range 絕不猜成相容。
func NodeSatisfiesRange(nodeVersion, expression string) (matched, understood bool) {
	node, ok := parseSemVersion(nodeVersion)
	if !ok || strings.TrimSpace(expression) == "" {
		return false, false
	}
	for _, rawClause := range strings.Split(expression, "||") {
		tokens := strings.Fields(rawClause)
		if len(tokens) == 0 {
			return false, false
		}
		clauseMatch := true
		for _, token := range tokens {
			op := ""
			for _, candidate := range []string{">=", "<=", ">", "<", "="} {
				if strings.HasPrefix(token, candidate) {
					op = candidate
					token = strings.TrimPrefix(token, candidate)
					break
				}
			}
			want, ok := parseRangeVersion(token)
			if op == "" || !ok {
				return false, false
			}
			cmp := compareSemVersion(node, want)
			switch op {
			case ">=":
				clauseMatch = clauseMatch && cmp >= 0
			case "<=":
				clauseMatch = clauseMatch && cmp <= 0
			case ">":
				clauseMatch = clauseMatch && cmp > 0
			case "<":
				clauseMatch = clauseMatch && cmp < 0
			case "=":
				clauseMatch = clauseMatch && cmp == 0
			}
		}
		if clauseMatch {
			return true, true
		}
	}
	return false, true
}

func parseRangeVersion(raw string) (semVersion, bool) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "v")
	parts := strings.Split(raw, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return semVersion{}, false
	}
	var v semVersion
	for i, part := range parts {
		if part == "" {
			return semVersion{}, false
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n < 0 {
			return semVersion{}, false
		}
		v[i] = n
	}
	return v, true
}

func compareSemVersion(a, b semVersion) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}
