package main

// 同一組 route tally 寫在四份現行文件裡。18685d8 加了
// GET /v1/agent/terminal-link，FEATURE-INVENTORY 沒有列，四份文件的
// 總數也沒有人同時去對 manifest。數字各自看都像對的。
//
// ⚠ 解析限定在該份文件的那一節、或那一段固定句子裡，不整份 md 亂 grep。
// 這個 repo 已經被敘述文字裡的例子咬過四次，見 install_doc_test.go。
// PHASES.md 與 HANDOFF-2026-09-13.md 是各階段當時的數字，這支測試不開它們。
//
// ⚠ 對不上就改文件。把斷言放寬到「差不多有寫到」不是測試。

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
)

type routeTally struct {
	nonOperator int
	operator    int
	total       int
	view        int
	operate     int
	admin       int
	json        int
	html        int
	machine     int
	verifier    int
}

func TestRouteTallyDocumentsMatchManifest(t *testing.T) {
	tally := routeTallyFromPolicies(t)
	assertAPISurfaceTally(t, tally)
	// These documents describe the unchanged Tailscale surface. Public account
	// routes are separately tallied in API-SURFACE; documentation changes here
	// are deliberately limited to that file for Autopilot Pass 2.
	tailscale := tally
	tailscale.total -= len(accountRoutePatterns)
	tailscale.nonOperator -= len(accountRoutePatterns)
	assertFeatureInventoryTally(t, tailscale)
	assertControlPlaneTally(t, tailscale)
	assertOperatorAuthTally(t, tailscale)
}

func routeTallyFromPolicies(t *testing.T) routeTally {
	t.Helper()
	var tally routeTally
	tally.nonOperator = len(nonOperatorRoutePolicies)
	tally.operator = len(operatorRoutePolicies)
	tally.total = tally.nonOperator + tally.operator

	var agent []string
	health := 0
	accounts := 0
	for pattern, policy := range nonOperatorRoutePolicies {
		switch policy.Class {
		case nonOperatorAgent:
			agent = append(agent, pattern)
		case nonOperatorAccount:
			accounts++
			if !isAccountRoute(pattern) {
				t.Fatalf("invalid account route %q", pattern)
			}
		case nonOperatorHealth:
			health++
			if pattern != "GET /healthz" {
				t.Fatalf("health policy %q 不是 GET /healthz", pattern)
			}
		case nonOperatorMetrics:
			t.Fatalf("non-operator route %q 仍公開 /metrics；它要 operator view", pattern)
		default:
			t.Fatalf("non-operator route %q class %d 沒有歸類", pattern, policy.Class)
		}
	}
	if health != 1 {
		t.Fatalf("health=%d，公開探針只剩 /healthz", health)
	}

	verifier := verifierRoutesNamedByOperatorAuth(t)
	verifierSet := map[string]struct{}{}
	for _, pattern := range verifier {
		policy, ok := nonOperatorRoutePolicies[pattern]
		if !ok || policy.Class != nonOperatorAgent {
			t.Fatalf("文件點名的 verifier route %q 不在 non-operator agent manifest", pattern)
		}
		verifierSet[pattern] = struct{}{}
	}
	for _, pattern := range agent {
		if _, named := verifierSet[pattern]; !named {
			tally.machine++
		}
	}
	tally.verifier = len(verifierSet)
	if accounts != 5 {
		t.Fatalf("account routes=%d, want 5", accounts)
	}
	if tally.machine+tally.verifier+health+accounts != tally.nonOperator {
		t.Fatalf("machine %d + verifier %d + health %d != non-operator %d",
			tally.machine, tally.verifier, health, tally.nonOperator)
	}

	for pattern, policy := range operatorRoutePolicies {
		switch policy.Permission {
		case operatorauth.View:
			tally.view++
		case operatorauth.Operate:
			tally.operate++
		case operatorauth.Admin:
			tally.admin++
		default:
			t.Fatalf("operator route %q permission %d 沒有出現在文件的 capability tally", pattern, policy.Permission)
		}
		switch policy.Representation {
		case operatorJSON:
			tally.json++
			_, path, ok := strings.Cut(pattern, " ")
			if !ok || !strings.HasPrefix(path, "/v1/operator/") {
				t.Fatalf("operator JSON route %q 不在 /v1/operator/*，OPERATOR-AUTH.md 的那句限定不再成立", pattern)
			}
		case operatorHTML, operatorPlain:
			// Plain-text /metrics is counted with the HTML/BFF/CSV/download
			// bucket. It is not an operator JSON route.
			tally.html++
		default:
			t.Fatalf("operator route %q representation %d 不是 JSON、HTML 或 plain", pattern, policy.Representation)
		}
	}
	if tally.view+tally.operate+tally.admin != tally.operator {
		t.Fatalf("view %d + operate %d + admin %d != operator %d",
			tally.view, tally.operate, tally.admin, tally.operator)
	}
	if tally.json+tally.html != tally.operator {
		t.Fatalf("JSON %d + HTML %d != operator %d", tally.json, tally.html, tally.operator)
	}
	return tally
}

func assertAPISurfaceTally(t *testing.T, tally routeTally) {
	t.Helper()
	lead := leadUntilHeading(t, "../../docs/API-SURFACE.md")
	matches := apiSurfaceTallyRE.FindAllStringSubmatch(lead, -1)
	if len(matches) != 1 {
		t.Fatalf("docs/API-SURFACE.md 開頭的 HTTP operations 句子出現 %d 次", len(matches))
	}
	got := matches[0]
	requireEqual(t, "API-SURFACE total", atoi(t, got[1]), tally.total)
	requireEqual(t, "API-SURFACE non-operator", atoi(t, got[2]), tally.nonOperator)
	requireEqual(t, "API-SURFACE operator", atoi(t, got[3]), tally.operator)
	requireEqual(t, "API-SURFACE JSON", atoi(t, got[4]), tally.json)
}

func assertFeatureInventoryTally(t *testing.T, tally routeTally) {
	t.Helper()
	section := sectionFromHeading(t, "../../docs/FEATURE-INVENTORY.md",
		`(?m)^## \d+ 個 production operations 基線\s*$`)
	heading := featureHeadingRE.FindAllStringSubmatch(section, -1)
	if len(heading) != 1 {
		t.Fatalf("FEATURE-INVENTORY 基線標題出現 %d 次", len(heading))
	}
	requireEqual(t, "FEATURE-INVENTORY 標題", atoi(t, heading[0][1]), tally.total)

	paragraphs := featureTallyRE.FindAllStringSubmatch(section, -1)
	if len(paragraphs) != 1 {
		t.Fatalf("FEATURE-INVENTORY 基線段落出現 %d 次", len(paragraphs))
	}
	got := paragraphs[0]
	requireEqual(t, "FEATURE-INVENTORY non-operator", atoi(t, got[1]), tally.nonOperator)
	requireEqual(t, "FEATURE-INVENTORY operator", atoi(t, got[2]), tally.operator)
	requireEqual(t, "FEATURE-INVENTORY JSON", atoi(t, got[3]), tally.json)
	requireEqual(t, "FEATURE-INVENTORY HTML", atoi(t, got[4]), tally.html)
	requireEqual(t, "FEATURE-INVENTORY view", atoi(t, got[5]), tally.view)
	requireEqual(t, "FEATURE-INVENTORY operate", atoi(t, got[6]), tally.operate)
	requireEqual(t, "FEATURE-INVENTORY admin", atoi(t, got[7]), tally.admin)

	surfaces := featureSurfaceRE.FindAllStringSubmatch(section, -1)
	if len(surfaces) != 1 {
		t.Fatalf("FEATURE-INVENTORY「N-operation surface」出現 %d 次", len(surfaces))
	}
	requireEqual(t, "FEATURE-INVENTORY surface", atoi(t, surfaces[0][1]), tally.total)

	machineHead := featureMachineHeadRE.FindAllStringSubmatch(section, -1)
	operatorHead := featureOperatorHeadRE.FindAllStringSubmatch(section, -1)
	if len(machineHead) != 1 || len(operatorHead) != 1 {
		t.Fatalf("FEATURE-INVENTORY 分表標題 machine=%d operator=%d", len(machineHead), len(operatorHead))
	}
	requireEqual(t, "FEATURE-INVENTORY machine 分表", atoi(t, machineHead[0][1]), tally.nonOperator)
	requireEqual(t, "FEATURE-INVENTORY operator 分表", atoi(t, operatorHead[0][1]), tally.operator)

	machineAt := strings.Index(section, "### Machine 與 verifier transport、telemetry（")
	operatorAt := strings.Index(section, "### Operator HTML/BFF 與 JSON（")
	if machineAt < 0 || operatorAt < machineAt {
		t.Fatal("FEATURE-INVENTORY 兩張表的順序不對")
	}
	machineNums, machineRoutes := inventoryRows(t, section[machineAt:operatorAt])
	operatorNums, operatorRoutes := inventoryRows(t, section[operatorAt:])
	requireSameRoutes(t, "FEATURE-INVENTORY non-operator 列", machineRoutes, policyRouteSet(t, nonOperatorKeys()))
	requireSameRoutes(t, "FEATURE-INVENTORY operator 列", operatorRoutes, policyRouteSet(t, operatorKeys()))

	nums := append(append([]int{}, machineNums...), operatorNums...)
	requireContiguousRows(t, nums, tally.total)
}

func assertControlPlaneTally(t *testing.T, tally routeTally) {
	t.Helper()
	paragraph := uniqueParagraph(t, "../../docs/CONTROL-PLANE-CONTRACT.md", "目前 production surface 固定為")
	matches := contractTallyRE.FindAllStringSubmatch(paragraph, -1)
	if len(matches) != 1 {
		t.Fatalf("CONTROL-PLANE-CONTRACT production surface 句子出現 %d 次", len(matches))
	}
	got := matches[0]
	requireEqual(t, "CONTROL-PLANE total", atoi(t, got[1]), tally.total)
	requireEqual(t, "CONTROL-PLANE non-operator", atoi(t, got[2]), tally.nonOperator)
	requireEqual(t, "CONTROL-PLANE operator", atoi(t, got[3]), tally.operator)
	requireEqual(t, "CONTROL-PLANE JSON", atoi(t, got[4]), tally.json)
	requireEqual(t, "CONTROL-PLANE HTML", atoi(t, got[5]), tally.html)
	requireEqual(t, "CONTROL-PLANE view", atoi(t, got[6]), tally.view)
	requireEqual(t, "CONTROL-PLANE operate", atoi(t, got[7]), tally.operate)
	requireEqual(t, "CONTROL-PLANE admin", atoi(t, got[8]), tally.admin)
}

func assertOperatorAuthTally(t *testing.T, tally routeTally) {
	t.Helper()
	section := sectionFromHeading(t, "../../docs/OPERATOR-AUTH.md",
		`(?m)^## 2\. 三個 capability 不做隱含繼承\s*$`)
	matches := operatorAuthTallyRE.FindAllStringSubmatch(section, -1)
	if len(matches) != 1 {
		t.Fatalf("OPERATOR-AUTH route manifest 句子出現 %d 次", len(matches))
	}
	got := matches[0]
	requireEqual(t, "OPERATOR-AUTH total", atoi(t, got[1]), tally.total)
	requireEqual(t, "OPERATOR-AUTH non-operator", atoi(t, got[2]), tally.nonOperator)
	requireEqual(t, "OPERATOR-AUTH operator", atoi(t, got[3]), tally.operator)
	requireEqual(t, "OPERATOR-AUTH view", atoi(t, got[4]), tally.view)
	requireEqual(t, "OPERATOR-AUTH operate", atoi(t, got[5]), tally.operate)
	requireEqual(t, "OPERATOR-AUTH admin", atoi(t, got[6]), tally.admin)
	requireEqual(t, "OPERATOR-AUTH JSON", atoi(t, got[7]), tally.json)
	requireEqual(t, "OPERATOR-AUTH HTML", atoi(t, got[8]), tally.html)

	boundaries := operationBoundaryRE.FindAllStringSubmatch(section, -1)
	if len(boundaries) != 1 {
		t.Fatalf("OPERATOR-AUTH「N-operation boundary」出現 %d 次", len(boundaries))
	}
	requireEqual(t, "OPERATOR-AUTH boundary", atoi(t, boundaries[0][1]), tally.total)

	breakdowns := nonOperatorBreakdownRE.FindAllStringSubmatch(section, -1)
	if len(breakdowns) != 1 {
		t.Fatalf("OPERATOR-AUTH non-operator 分解出現 %d 次", len(breakdowns))
	}
	got = breakdowns[0]
	requireEqual(t, "OPERATOR-AUTH 繞過 boundary 的條數", atoi(t, got[1]), tally.nonOperator)
	requireEqual(t, "OPERATOR-AUTH 分解重述的條數", atoi(t, got[2]), tally.nonOperator)
	requireEqual(t, "OPERATOR-AUTH machine transport", atoi(t, got[3]), tally.machine)
	requireEqual(t, "OPERATOR-AUTH verifier transport", atoi(t, got[4]), tally.verifier)

	rows := operateRowRE.FindAllStringSubmatch(section, -1)
	if len(rows) != 1 {
		t.Fatalf("OPERATOR-AUTH operate 列出現 %d 次", len(rows))
	}
	if !strings.Contains(rows[0][1], "終端頁") {
		t.Errorf("OPERATOR-AUTH operate 列沒有終端頁：%s", rows[0][1])
	}
}

// verifierRoutesNamedByOperatorAuth 只讀 §2 裡點名 verifier bearer 的那一句。
// manifest 的 class 沒有把 verifier 從 machine 分開，文件自己點了那兩條。
func verifierRoutesNamedByOperatorAuth(t *testing.T) []string {
	t.Helper()
	section := sectionFromHeading(t, "../../docs/OPERATOR-AUTH.md",
		`(?m)^## 2\. 三個 capability 不做隱含繼承\s*$`)
	from := strings.Index(section, "verifier transport")
	to := strings.Index(section, "Machine plane")
	if from < 0 || to < from {
		t.Fatal("OPERATOR-AUTH §2 裡找不到 verifier transport 到 Machine plane 的那一句")
	}
	matches := methodRouteRE.FindAllStringSubmatch(section[from:to], -1)
	if len(matches) == 0 {
		t.Fatal("OPERATOR-AUTH §2 沒有點名 verifier route")
	}
	routes := make([]string, 0, len(matches))
	seen := map[string]struct{}{}
	for _, match := range matches {
		if _, dup := seen[match[1]]; dup {
			t.Fatalf("OPERATOR-AUTH verifier route %s 點了兩次", match[1])
		}
		seen[match[1]] = struct{}{}
		routes = append(routes, match[1])
	}
	sort.Strings(routes)
	return routes
}

func nonOperatorKeys() []string {
	keys := make([]string, 0, len(nonOperatorRoutePolicies))
	for pattern, policy := range nonOperatorRoutePolicies {
		if policy.Class == nonOperatorAccount {
			continue
		}
		keys = append(keys, pattern)
	}
	return keys
}

func operatorKeys() []string {
	keys := make([]string, 0, len(operatorRoutePolicies))
	for pattern := range operatorRoutePolicies {
		keys = append(keys, pattern)
	}
	return keys
}

// inventoryRoutePattern 把 manifest 的 ServeMux pattern 收成清單上的寫法。
//
// ⚠ `GET /{$}` 是「只有根路徑」的 pattern，FEATURE-INVENTORY 寫 `GET /`。
// 沒有這一步，這支測試在文件正確時也是紅的。只正規化這一個 pattern。
func inventoryRoutePattern(pattern string) string {
	if pattern == "GET /{$}" {
		return "GET /"
	}
	return pattern
}

func policyRouteSet(t *testing.T, patterns []string) map[string]struct{} {
	t.Helper()
	out := make(map[string]struct{}, len(patterns))
	for _, pattern := range patterns {
		out[inventoryRoutePattern(pattern)] = struct{}{}
	}
	if len(out) != len(patterns) {
		t.Fatal("route pattern 正規化後撞名")
	}
	return out
}

func inventoryRows(t *testing.T, part string) ([]int, []string) {
	t.Helper()
	matches := inventoryRowRE.FindAllStringSubmatch(part, -1)
	nums := make([]int, 0, len(matches))
	routes := make([]string, 0, len(matches))
	for _, match := range matches {
		nums = append(nums, atoi(t, match[1]))
		routes = append(routes, match[2])
	}
	return nums, routes
}

func requireContiguousRows(t *testing.T, nums []int, total int) {
	t.Helper()
	seen := make(map[int]struct{}, len(nums))
	var dups []int
	for _, n := range nums {
		if _, ok := seen[n]; ok {
			dups = append(dups, n)
			continue
		}
		seen[n] = struct{}{}
	}
	if len(dups) != 0 {
		t.Errorf("FEATURE-INVENTORY 列號重複：%v", dups)
	}
	if len(nums) != total {
		t.Errorf("FEATURE-INVENTORY 列數 %d，manifest 是 %d", len(nums), total)
	}
	var missing []string
	for i := 1; i <= total; i++ {
		if _, ok := seen[i]; !ok {
			missing = append(missing, strconv.Itoa(i))
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Errorf("FEATURE-INVENTORY 缺列號 %s", strings.Join(missing, ", "))
	}
	for n := range seen {
		if n < 1 || n > total {
			t.Errorf("FEATURE-INVENTORY 列號 %d 超出 1..%d", n, total)
		}
	}
}

func requireSameRoutes(t *testing.T, label string, got []string, want map[string]struct{}) {
	t.Helper()
	have := make(map[string]struct{}, len(got))
	var dups []string
	for _, route := range got {
		if _, ok := have[route]; ok {
			dups = append(dups, route)
		}
		have[route] = struct{}{}
	}
	sort.Strings(dups)
	if len(dups) != 0 {
		t.Errorf("%s 路由重複：%s", label, strings.Join(dups, ", "))
	}
	var missing, extra []string
	for route := range want {
		if _, ok := have[route]; !ok {
			missing = append(missing, route)
		}
	}
	for route := range have {
		if _, ok := want[route]; !ok {
			extra = append(extra, route)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Errorf("%s 跟 manifest 不一致\n缺：%s\n多：%s", label, strings.Join(missing, ", "), strings.Join(extra, ", "))
	}
}

func leadUntilHeading(t *testing.T, path string) string {
	t.Helper()
	doc := readRepoFile(t, path)
	end := regexp.MustCompile(`(?m)^## `).FindStringIndex(doc)
	if end == nil {
		t.Fatalf("%s 裡找不到第一個二級標題", path)
	}
	return doc[:end[0]]
}

func sectionFromHeading(t *testing.T, path, startRE string) string {
	t.Helper()
	doc := readRepoFile(t, path)
	start := regexp.MustCompile(startRE).FindStringIndex(doc)
	if start == nil {
		t.Fatalf("%s 裡找不到 %s", path, startRE)
	}
	body := doc[start[0]:]
	if end := regexp.MustCompile(`(?m)^## `).FindStringIndex(body[1:]); end != nil {
		body = body[:1+end[0]]
	}
	return body
}

func uniqueParagraph(t *testing.T, path, marker string) string {
	t.Helper()
	doc := readRepoFile(t, path)
	if strings.Count(doc, marker) != 1 {
		t.Fatalf("%s 裡 %q 出現 %d 次", path, marker, strings.Count(doc, marker))
	}
	rest := doc[strings.Index(doc, marker):]
	if end := strings.Index(rest, "\n\n"); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func requireEqual(t *testing.T, label string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s 寫 %d，manifest 是 %d", label, got, want)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("%q 不是整數", s)
	}
	return n
}

var (
	apiSurfaceTallyRE = regexp.MustCompile(`逐一對照 (\d+) 個 HTTP operations（(\d+) non-operator＋(\d+) operator；(\d+) 條 operator JSON）`)
	featureHeadingRE  = regexp.MustCompile(`(?m)^## (\d+) 個 production operations 基線\s*$`)
	featureTallyRE    = regexp.MustCompile(
		`此數字按 HTTP method \+ route 計數：(\d+) 個 non-operator operations、(\d+) 個 operator\s+` +
			"operations；其中 (\\d+) 個為 operator JSON、(\\d+) 個為 HTML/BFF/CSV/download。Operator 另按 exact\\s+" +
			"capability 分成 `view=(\\d+)`、`operate=(\\d+)`、`admin=(\\d+)`")
	featureSurfaceRE      = regexp.MustCompile(`(\d+)-operation surface`)
	featureMachineHeadRE  = regexp.MustCompile(`(?m)^### Machine 與 verifier transport、telemetry（(\d+)）\s*$`)
	featureOperatorHeadRE = regexp.MustCompile(`(?m)^### Operator HTML/BFF 與 JSON（(\d+)）\s*$`)
	contractTallyRE       = regexp.MustCompile(
		"目前 production surface 固定為\\s+" +
			`(\d+)（(\d+) non-operator＋(\d+) operator；(\d+) operator JSON＋(\d+) HTML/BFF/CSV/download），exact auth tally 是\s+` +
			"`view=(\\d+)`／`operate=(\\d+)`／`admin=(\\d+)`")
	operatorAuthTallyRE = regexp.MustCompile(
		"目前 code route manifest 固定為 (\\d+) operations：(\\d+) 條 non-operator，加上 (\\d+) 條 operator\\s+" +
			"routes；operator manifest 的 exact capability tally 是 `view=(\\d+)`、`operate=(\\d+)`、`admin=(\\d+)`，\\s+" +
			"其中 `/v1/operator/\\*` JSON routes 共 (\\d+) 條、HTML/BFF/CSV/download 共 (\\d+) 條")
	operationBoundaryRE    = regexp.MustCompile(`(\d+)-operation boundary`)
	nonOperatorBreakdownRE = regexp.MustCompile(
		"繞過 operator\\s+boundary 的 (\\d+) 條 route 也有另一份完整 manifest；兩份不能\\s+" +
			"重疊。那 (\\d+) 條是 (\\d+) 條 machine transport、(\\d+) 條 verifier transport、`/healthz`")
	operateRowRE   = regexp.MustCompile("(?m)^\\| `operate` \\| (.+) \\|$")
	inventoryRowRE = regexp.MustCompile("(?m)^\\| (\\d+) \\| `([^`]+)` \\|")
	methodRouteRE  = regexp.MustCompile("`((?:GET|HEAD|POST|PUT|PATCH|DELETE) [^`]+)`")
)
