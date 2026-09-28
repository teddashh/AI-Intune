package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

type navigationLocale string

const (
	navigationLocaleTraditionalChinese navigationLocale = "zh-Hant"
	navigationLocaleEnglish            navigationLocale = "en"
	navigationLocaleCookie                              = "clawctl_navigation_locale"
	navigationLocaleCookieMaxAge                        = 365 * 24 * 60 * 60
	navigationReturnTargetMaxBytes                      = 4096
)

type navigationLocaleView struct {
	Tag             string
	ContentTag      string
	PageTitle       string
	ToggleHref      string
	ToggleLabel     string
	ToggleTitle     string
	SkipLabel       string
	BrandAria       string
	BrandLabel      string
	VerifiedLabel   string
	UnverifiedLabel string
	MainNavAria     string
	NavGroupLabel   string
	DashboardLabel  string
	DevicesLabel    string
	AppsLabel       string
	EndpointLabel   string
	AgentsLabel     string
	ReportsLabel    string
	TenantLabel     string
	SupportLabel    string
	BreadcrumbAria  string
	BreadcrumbHome  string
}

func navigationLocaleFromRequest(r *http.Request) navigationLocale {
	if r == nil {
		return navigationLocaleTraditionalChinese
	}
	cookie, err := r.Cookie(navigationLocaleCookie)
	if err != nil {
		return navigationLocaleTraditionalChinese
	}
	locale, ok := parseNavigationLocale(cookie.Value)
	if !ok {
		return navigationLocaleTraditionalChinese
	}
	return locale
}

func parseNavigationLocale(raw string) (navigationLocale, bool) {
	switch navigationLocale(raw) {
	case navigationLocaleTraditionalChinese:
		return navigationLocaleTraditionalChinese, true
	case navigationLocaleEnglish:
		return navigationLocaleEnglish, true
	default:
		return navigationLocaleTraditionalChinese, false
	}
}

func navigationLocaleFor(r *http.Request, title string) navigationLocaleView {
	locale := navigationLocaleFromRequest(r)
	requestURI := "/"
	if r != nil && r.URL != nil && strings.HasPrefix(r.URL.RequestURI(), "/") {
		requestURI = r.URL.RequestURI()
	}
	target := navigationLocaleEnglish
	view := navigationLocaleView{
		Tag:             string(navigationLocaleTraditionalChinese),
		ContentTag:      string(navigationLocaleTraditionalChinese),
		PageTitle:       title,
		ToggleLabel:     "English",
		ToggleTitle:     "將管理中心導覽切換為英文",
		SkipLabel:       "跳到主要內容",
		BrandAria:       "AI-Intune 管理中心首頁",
		BrandLabel:      "AI-Intune 系統管理中心",
		VerifiedLabel:   "Tailscale 已驗證",
		UnverifiedLabel: "Operator 未驗證",
		MainNavAria:     "主要導覽",
		NavGroupLabel:   "管理中心",
		DashboardLabel:  "儀表板",
		DevicesLabel:    "裝置",
		AppsLabel:       "應用程式",
		EndpointLabel:   "端點安全性",
		AgentsLabel:     "代理程式",
		ReportsLabel:    "報告",
		TenantLabel:     "租用戶管理",
		SupportLabel:    "疑難排解 + 支援",
		BreadcrumbAria:  "麵包屑",
		BreadcrumbHome:  "AI-Intune 管理中心",
	}
	if locale == navigationLocaleEnglish {
		target = navigationLocaleTraditionalChinese
		view = navigationLocaleView{
			Tag:             string(navigationLocaleEnglish),
			ContentTag:      string(navigationLocaleTraditionalChinese),
			PageTitle:       englishNavigationText(title),
			ToggleLabel:     "繁體中文",
			ToggleTitle:     "Switch management-center navigation to Traditional Chinese",
			SkipLabel:       "Skip to main content",
			BrandAria:       "AI-Intune admin center home",
			BrandLabel:      "AI-Intune admin center",
			VerifiedLabel:   "Tailscale verified",
			UnverifiedLabel: "Operator not verified",
			MainNavAria:     "Primary navigation",
			NavGroupLabel:   "Admin center",
			DashboardLabel:  "Dashboard",
			DevicesLabel:    "Devices",
			AppsLabel:       "Apps",
			EndpointLabel:   "Endpoint security",
			AgentsLabel:     "Agents",
			ReportsLabel:    "Reports",
			TenantLabel:     "Tenant administration",
			SupportLabel:    "Troubleshooting + support",
			BreadcrumbAria:  "Breadcrumb",
			BreadcrumbHome:  "AI-Intune admin center",
		}
	}
	view.ToggleHref = "/preferences/navigation-language/" + string(target) +
		"?return_to=" + url.QueryEscape(requestURI)
	return view
}

var englishNavigationTexts = map[string]string{
	"總覽":                    "Overview",
	"裝置":                    "Devices",
	"裝置註冊":                  "Device enrollment",
	"裝置生命週期":                "Device lifecycle",
	"裝置組態":                  "Device configuration",
	"裝置合規性":                 "Device compliance",
	"診斷":                    "Diagnostics",
	"代理程式活動":                "Agent activity",
	"應用程式":                  "Apps",
	"部署":                    "Deployments",
	"新增部署":                  "New deployment",
	"更新":                    "Updates",
	"報告":                    "Reports",
	"變更":                    "Changes",
	"票證使用量":                 "Ticket usage",
	"註冊報告":                  "Enrollment report",
	"軟體清查":                  "Software inventory",
	"每機安裝狀態":                "Per-device installation status",
	"發佈與指派":                 "Published and assigned",
	"稽核記錄":                  "Audit log",
	"維護":                    "Maintenance",
	"還原演練":                  "Restore drill",
	"資料揭露":                  "Data disclosure",
	"確認建立 deployment":       "Confirm deployment creation",
	"確認繼續":                  "Confirm continuation",
	"確認部署動作":                "Confirm deployment action",
	"確認永久清理":                "Confirm permanent cleanup",
	"確認還原演練":                "Confirm restore drill",
	"確認註冊上限":                "Confirm enrollment limit",
	"確認派工":                  "Confirm assignment",
	"確認設定原則":                "Confirm configuration policy",
	"確認設定指派":                "Confirm configuration assignment",
	"確認合規性原則":               "Confirm compliance policy",
	"確認合規性指派":               "Confirm compliance assignment",
	"確認 Store package":      "Confirm Store package",
	"確認 Profile":            "Confirm profile",
	"確認 Profile Assignment": "Confirm profile assignment",
	"註冊上限已更新":               "Enrollment limit updated",
	"資料清理完成":                "Data cleanup completed",

	"裝置子選單":         "Devices navigation",
	"裝置詳細資料子選單":     "Device details navigation",
	"更新子選單":         "Updates navigation",
	"代理程式子選單":       "Agents navigation",
	"代理程式活動詳細資料子選單": "Agent activity details navigation",
	"疑難排解子選單":       "Troubleshooting navigation",
	"應用子選單":         "Apps navigation",
	"部署子選單":         "Deployments navigation",
	"部署詳細資料子選單":     "Deployment details navigation",
	"報告子選單":         "Reports navigation",
	"租用戶子選單":        "Tenant administration navigation",
	"代理程式":          "Agents",
	"疑難排解 + 支援":     "Troubleshooting + support",
	"租用戶管理":         "Tenant administration",
	"概觀":            "Overview",
	"所有裝置":          "All devices",
	"監視":            "Monitor",
	"註冊裝置":          "Enroll devices",
	"組態":            "Configuration",
	"合規性":           "Compliance",
	"生命週期":          "Lifecycle",
	"已退役":           "Retired",
	"裝置診斷":          "Device diagnostics",
	"活動":            "Activity",
	"設定檔":           "Profiles",
	"所有活動":          "All activity",
	"期望狀態":          "Desired state",
	"事件":            "Events",
	"驗證證據":          "Verification evidence",
	"屬性":            "Properties",
	"應用與憑證":         "Apps and credentials",
	"工作單":           "Jobs",
	"動作":            "Actions",
	"事件時間軸":         "Event timeline",
	"資料":            "Data",
	"應用程式目錄":        "App catalog",
	"指派":            "Assignments",
	"安裝套件":          "Installation packages",
	"套件擷取":          "Package fetches",
	"作業":            "Operations",
	"全部":            "All",
	"進行中":           "Active",
	"已暫停 / 卡住":      "Paused / stuck",
	"已完成":           "Finished",
	"監視與卡住":         "Monitoring and stuck",
	"目標與工作單":        "Targets and jobs",
	"設定":            "Settings",
	"更新概觀":          "Update overview",
	"報告總覽":          "Reports overview",
	"註冊":            "Enrollment",
	"安裝狀態":          "Installation status",
}

func englishNavigationText(value string) string {
	if translated, ok := englishNavigationTexts[value]; ok {
		return translated
	}
	for _, prefix := range []struct{ zh, en string }{
		{"代理程式活動 ", "Agent activity "},
		{"確認撤銷 ", "Confirm revocation "},
		{"確認重新命名 ", "Confirm rename "},
		{"確認名冊備註 ", "Confirm registry notes "},
		{"確認 fetch ", "Confirm fetch "},
		{"確認 ", "Confirm "},
	} {
		if strings.HasPrefix(value, prefix.zh) {
			return prefix.en + strings.TrimPrefix(value, prefix.zh)
		}
	}
	return value
}

func localizeSubNavigation(nav *subNavigation, locale navigationLocale) {
	if nav == nil || locale != navigationLocaleEnglish {
		return
	}
	nav.Label = englishNavigationText(nav.Label)
	nav.Title = englishNavigationText(nav.Title)
	for index := range nav.Items {
		nav.Items[index].Label = englishNavigationText(nav.Items[index].Label)
	}
}

func localizeAccessView(access *accessView, locale navigationLocale) {
	if access == nil || locale != navigationLocaleEnglish || !access.Known {
		return
	}
	access.Attribution = "Source device " + access.Device + "; Tailscale user " + access.Login
}

func (s *Server) setNavigationLanguage(w http.ResponseWriter, r *http.Request) {
	locale, ok := parseNavigationLocale(r.PathValue("locale"))
	if !ok {
		http.Error(w, "不支援的導覽語言。", http.StatusBadRequest)
		return
	}
	returnTarget, err := navigationReturnTarget(r.URL.Query())
	if err != nil {
		http.Error(w, "導覽語言請求無效。", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: navigationLocaleCookie, Value: string(locale), Path: "/",
		MaxAge: navigationLocaleCookieMaxAge, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, returnTarget, http.StatusSeeOther)
}

func navigationReturnTarget(query url.Values) (string, error) {
	values, ok := query["return_to"]
	if !ok || len(query) != 1 || len(values) != 1 {
		return "", errors.New("return target is required exactly once")
	}
	target := values[0]
	if len(target) == 0 || len(target) > navigationReturnTargetMaxBytes ||
		strings.HasPrefix(target, "//") ||
		strings.ContainsAny(target, "\\#\r\n") {
		return "", errors.New("return target is not a bounded local path")
	}
	for _, character := range target {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			return "", errors.New("return target contains control characters")
		}
	}
	parsed, err := url.ParseRequestURI(target)
	if err != nil || parsed.Host != "" || parsed.Path == "" {
		return "", errors.New("return target is not a local request URI")
	}
	return parsed.RequestURI(), nil
}
