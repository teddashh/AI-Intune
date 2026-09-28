package web

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

var subNavigationLinkRE = regexp.MustCompile(`<a href="([^"]+)" class="subnav-item[^"]*"[^>]*>([^<]+)</a>`)

func renderedSubNavigation(t *testing.T, body, label string) string {
	t.Helper()
	start := strings.Index(body, `<nav class="subnav" aria-label="`+label+`">`)
	if start < 0 {
		t.Fatalf("page missing %q subnavigation", label)
	}
	end := strings.Index(body[start:], `</nav>`)
	if end < 0 {
		t.Fatalf("%q subnavigation is truncated", label)
	}
	return body[start : start+end+len(`</nav>`)]
}

func assertSubNavigation(t *testing.T, s *Server, path, label, current string, expected map[string]string) {
	t.Helper()
	requested, err := url.Parse(path)
	if err != nil {
		t.Fatalf("parse navigation request %q: %v", path, err)
	}
	// Browsers never send the fragment to the server.  RequestURI preserves the
	// query-backed section selection while exercising the same transport shape.
	body := get(t, s, requested.RequestURI())
	assertSubNavigationBody(t, s, "GET "+path, body, label, current, expected)
}

func assertSubNavigationBody(t *testing.T, s *Server, describe, body, label, current string, expected map[string]string) {
	t.Helper()
	nav := renderedSubNavigation(t, body, label)
	if got := strings.Count(nav, `aria-current="`); got != 1 {
		t.Fatalf("%s subnavigation has %d current leaves, want exactly one: %s", describe, got, nav)
	}
	if !strings.Contains(nav, `aria-current="page">`+current+`</a>`) &&
		!strings.Contains(nav, `aria-current="location">`+current+`</a>`) {
		t.Fatalf("%s current subnavigation leaf is not %q: %s", describe, current, nav)
	}
	matches := subNavigationLinkRE.FindAllStringSubmatch(nav, -1)
	if len(matches) != len(expected) {
		t.Errorf("%s subnavigation has %d links, want %d: %s", describe, len(matches), len(expected), nav)
	}
	links := make(map[string]string, len(matches))
	for _, match := range matches {
		links[html.UnescapeString(match[1])] = html.UnescapeString(match[2])
	}
	for href, wantText := range expected {
		if gotText, ok := links[href]; !ok || gotText != wantText {
			t.Errorf("%s subnavigation link %s text=%q, want %q: %s", describe, href, gotText, wantText, nav)
		}
	}

	// Every advertised leaf must resolve.  For fragment leaves, also prove that
	// the target document contains the named anchor; a pretty submenu pointing
	// at a non-existent section is not a delivered navigation path.
	for _, match := range matches {
		href := html.UnescapeString(match[1])
		u, err := url.Parse(href)
		if err != nil || !strings.HasPrefix(u.Path, "/") {
			t.Fatalf("%s has invalid internal subnavigation href %q: %v", describe, href, err)
		}
		target := get(t, s, u.RequestURI())
		if u.Fragment != "" {
			anchor := regexp.MustCompile(`<[^>]*id="` + regexp.QuoteMeta(u.Fragment) + `"[^>]*tabindex="-1"[^>]*>`)
			if !anchor.MatchString(target) {
				t.Errorf("%s leaf %q resolves but focusable anchor #%s does not exist", describe, href, u.Fragment)
			}
		}
	}
}

func sectionHref(base, section string) string {
	return base + "?section=" + url.QueryEscape(section) + "#" + section
}

func TestDeliveredPagesExposeReachableSubNavigation(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "nav-machine")
	jobID, _, _ := createWebJob(t, st, id, false)
	deployments, err := st.ListDeployments(time.Now().UTC())
	if err != nil || len(deployments) != 1 {
		t.Fatalf("deployment fixture=%d err=%v", len(deployments), err)
	}
	deploymentID := deployments[0].DeploymentID
	s.SetTailnetStatus(tailnet.Status{
		Available: true,
		Self:      tailnet.Peer{Hostname: "nav-machine", IP: "100.64.0.1", Online: true},
	})

	machineNav := map[string]string{
		sectionHref("/machines", "fleet-kpis"): "概觀", sectionHref("/machines", "machines"): "所有裝置",
		sectionHref("/machines", "findings"): "監視", "/jobs": "代理程式活動", "/machines/enrollment": "註冊裝置",
		"/machines/configuration": "組態", "/machines/compliance": "合規性",
		"/machines/lifecycle": "生命週期", "/updates": "更新",
		"/machines?lifecycle=retired&section=machines#machines": "已退役",
	}
	assertSubNavigation(t, s, "/machines", "裝置子選單", "概觀", machineNav)
	assertSubNavigation(t, s, sectionHref("/machines", "machines"), "裝置子選單", "所有裝置", machineNav)
	assertSubNavigation(t, s, sectionHref("/machines", "findings"), "裝置子選單", "監視", machineNav)
	assertSubNavigation(t, s, "/machines?lifecycle=retired&section=machines#machines", "裝置子選單", "已退役", machineNav)
	assertSubNavigation(t, s, "/machines/enrollment", "裝置子選單", "註冊裝置", machineNav)
	assertSubNavigation(t, s, "/machines/lifecycle", "裝置子選單", "生命週期", machineNav)
	assertSubNavigation(t, s, "/machines/configuration", "裝置子選單", "組態", machineNav)
	assertSubNavigation(t, s, "/machines/compliance", "裝置子選單", "合規性", machineNav)
	assertSubNavigation(t, s, "/machines/diagnostics", "疑難排解子選單", "裝置診斷", map[string]string{
		"/machines/diagnostics": "裝置診斷",
	})
	detailBase := "/machines/" + id
	detailNav := map[string]string{
		sectionHref(detailBase, "machine-overview"): "概觀", sectionHref(detailBase, "monitor"): "監視",
		sectionHref(detailBase, "properties"): "屬性", sectionHref(detailBase, "apps-and-credentials"): "應用與憑證",
		sectionHref(detailBase, "jobs"): "工作單", sectionHref(detailBase, "actions"): "動作",
		detailBase + "/timeline": "事件時間軸", detailBase + "/data": "資料",
	}
	assertSubNavigation(t, s, detailBase, "裝置詳細資料子選單", "概觀", detailNav)
	assertSubNavigation(t, s, sectionHref(detailBase, "monitor"), "裝置詳細資料子選單", "監視", detailNav)
	agentNav := map[string]string{
		sectionHref("/jobs", "agent-overview"): "概觀", sectionHref("/jobs", "job-ledger"): "活動",
		"/machines/enrollment": "裝置註冊", "/apps?view=profiles": "設定檔", "/deployments": "部署", "/audit": "稽核記錄",
	}
	assertSubNavigation(t, s, "/jobs", "代理程式子選單", "概觀", agentNav)
	assertSubNavigation(t, s, sectionHref("/jobs", "job-ledger"), "代理程式子選單", "活動", agentNav)
	jobBase := "/jobs/" + jobID
	assertSubNavigation(t, s, jobBase, "代理程式活動詳細資料子選單", "概觀", map[string]string{
		"/jobs":                                   "所有活動",
		sectionHref(jobBase, "job-overview"):      "概觀",
		sectionHref(jobBase, "desired-state"):     "期望狀態",
		sectionHref(jobBase, "job-events"):        "事件",
		sectionHref(jobBase, "job-verifications"): "驗證證據",
	})
	assertSubNavigation(t, s, sectionHref(jobBase, "job-events"), "代理程式活動詳細資料子選單", "事件", map[string]string{
		"/jobs":                                   "所有活動",
		sectionHref(jobBase, "job-overview"):      "概觀",
		sectionHref(jobBase, "desired-state"):     "期望狀態",
		sectionHref(jobBase, "job-events"):        "事件",
		sectionHref(jobBase, "job-verifications"): "驗證證據",
	})
	appsNav := map[string]string{
		"/apps?view=overview":    "概觀",
		"/apps?view=openclaw":    "OpenClaw",
		"/apps?view=store":       "應用程式目錄",
		"/apps?view=profiles":    "設定檔",
		"/apps?view=assignments": "指派",
		"/apps?view=artifacts":   "安裝套件",
		"/apps?view=fetch":       "套件擷取",
		"/apps?view=operations":  "作業",
		"/deployments":           "部署",
	}
	assertSubNavigation(t, s, "/apps", "應用子選單", "概觀", appsNav)
	assertSubNavigation(t, s, "/apps?view=artifacts", "應用子選單", "安裝套件", appsNav)
	assertSubNavigation(t, s, "/apps?view=operations", "應用子選單", "作業", appsNav)
	deploymentNav := map[string]string{
		"/deployments":               "全部",
		"/deployments?view=active":   "進行中",
		"/deployments?view=stuck":    "已暫停 / 卡住",
		"/deployments?view=finished": "已完成",
		"/deployments?view=new":      "新增部署",
	}
	assertSubNavigation(t, s, "/deployments", "部署子選單", "全部", deploymentNav)
	assertSubNavigation(t, s, "/deployments?view=stuck", "部署子選單", "已暫停 / 卡住", deploymentNav)
	assertSubNavigation(t, s, "/deployments?view=new", "部署子選單", "新增部署", deploymentNav)
	assertSubNavigation(t, s, "/deployments/"+deploymentID, "部署詳細資料子選單", "概觀", map[string]string{
		sectionHref("/deployments/"+deploymentID, "deployment-overview"): "概觀",
		sectionHref("/deployments/"+deploymentID, "stuck"):               "監視與卡住",
		sectionHref("/deployments/"+deploymentID, "deployment-targets"):  "目標與工作單",
		sectionHref("/deployments/"+deploymentID, "deployment-actions"):  "動作",
		sectionHref("/deployments/"+deploymentID, "deployment-settings"): "設定",
	})
	assertSubNavigation(t, s, "/updates", "更新子選單", "更新概觀", map[string]string{
		"/updates": "更新概觀", sectionHref("/updates", "artifacts"): "安裝套件",
		sectionHref("/updates", "channel-canary"): "Canary", sectionHref("/updates", "channel-stable"): "Stable",
	})
	assertSubNavigation(t, s, sectionHref("/updates", "channel-stable"), "更新子選單", "Stable", map[string]string{
		"/updates": "更新概觀", sectionHref("/updates", "artifacts"): "安裝套件",
		sectionHref("/updates", "channel-canary"): "Canary", sectionHref("/updates", "channel-stable"): "Stable",
	})
	reportsNav := map[string]string{
		"/reports": "報告總覽", "/reports/changes": "變更",
		"/reports/tickets": "票證使用量", "/reports/enrollment": "註冊",
		"/reports/software": "軟體清查", "/reports/install": "安裝狀態",
		"/reports/profile": "發佈與指派", "/audit": "稽核記錄",
	}
	assertSubNavigation(t, s, "/reports", "報告子選單", "報告總覽", reportsNav)
	// 同一台機器的兩頁要給同一份子選單。少一項的選單看起來像那一節在這裡不存在。
	assertSubNavigation(t, s, detailBase+"/timeline", "裝置詳細資料子選單", "事件時間軸", detailNav)
	assertSubNavigation(t, s, detailBase+"/data", "裝置詳細資料子選單", "資料", detailNav)
	tenantNav := map[string]string{"/tenant/maintenance": "維護", "/tenant/data": "資料揭露"}
	assertSubNavigation(t, s, "/tenant/maintenance", "租用戶子選單", "維護", tenantNav)
	assertSubNavigation(t, s, "/tenant/data", "租用戶子選單", "資料揭露", tenantNav)
	assertSubNavigation(t, s, "/reports/changes", "報告子選單", "變更", reportsNav)
	assertSubNavigation(t, s, "/reports/tickets", "報告子選單", "票證使用量", reportsNav)
	assertSubNavigation(t, s, "/reports/enrollment", "報告子選單", "註冊", reportsNav)
	assertSubNavigation(t, s, "/reports/software", "報告子選單", "軟體清查", reportsNav)
	assertSubNavigation(t, s, "/reports/install", "報告子選單", "安裝狀態", reportsNav)
	assertSubNavigation(t, s, "/reports/profile", "報告子選單", "發佈與指派", reportsNav)
	assertSubNavigation(t, s, "/audit", "報告子選單", "稽核記錄", reportsNav)
}

func tableCounts(t *testing.T, st *store.Store, tables ...string) map[string]int {
	t.Helper()
	out := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = count
	}
	return out
}

func TestEnrollmentPageOwnsFormsAndTailnetCandidateDiscoveryWithoutGETMutation(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "enrolled-node")
	s.SetTailnetStatus(tailnet.Status{
		Available: true,
		Self:      tailnet.Peer{Hostname: "enrolled-node", IP: "100.64.0.1", Online: true},
		Peers: []tailnet.Peer{{
			Hostname: "candidate-node", IP: "100.64.0.77", OS: "linux", Online: true,
		}},
	})
	tables := []string{"machine_registry", "enrollment_tokens", "operator_idempotency", "audit_log"}
	before := tableCounts(t, st, tables...)

	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	page := renderWithCapabilities(t, s, "/machines/enrollment?name=enrolled-node", names)
	for _, want := range []string{
		"<h1>裝置註冊</h1>", `action="/enrollments/preview"`, `value="enrolled-node"`,
		"candidate-node", "100.64.0.77", "預覽 candidate-node 的開票影響",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("enrollment page missing %q", want)
		}
	}
	if got := strings.Count(page, `action="/enrollments/preview"`); got != 2 {
		t.Errorf("enrollment page has %d preview forms, want blank/prefilled pair", got)
	}
	after := tableCounts(t, st, tables...)
	for _, table := range tables {
		if after[table] != before[table] {
			t.Errorf("GET enrollment mutated %s: before=%d after=%d", table, before[table], after[table])
		}
	}

	viewOnly := renderWithCapabilities(t, s, "/machines/enrollment", operatorauth.CapabilityNames{View: names.View})
	if strings.Contains(viewOnly, `action="/enrollments/preview"`) {
		t.Error("view-only enrollment page did not preserve exact capability boundary")
	}

	dashboard := renderWithCapabilities(t, s, "/", names)
	if strings.Contains(dashboard, `action="/enrollments/preview"`) || strings.Contains(dashboard, "candidate-node") {
		t.Error("Dashboard still owns enrollment forms or candidate detail instead of linking to the dedicated page")
	}
	if !strings.Contains(dashboard, `href="/machines/enrollment"`) {
		t.Error("Dashboard is missing the enrollment CTA")
	}
	machine := renderWithCapabilities(t, s, "/machines/"+id, names)
	misleadingCTA := fmt.Sprintf(`href="/machines/enrollment?name=%s"`, url.QueryEscape("enrolled-node"))
	if strings.Contains(machine, misleadingCTA) || strings.Contains(machine, "開新的 enroll 票") {
		t.Errorf("machine detail advertises unsupported in-place ticket reissue: %s", misleadingCTA)
	}
}
