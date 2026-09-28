package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func getWithNavigationLocale(t *testing.T, server *Server, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	server.Routes(mux)
	recorder := httptest.NewRecorder()
	request := verifiedWebRequest(httptest.NewRequest(http.MethodGet, path, nil), "example.com/cap/clawctl-view")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	mux.ServeHTTP(recorder, request)
	return recorder
}

func TestNavigationLanguagePreferencePersistsEnglishAdminCenterShell(t *testing.T) {
	server, _ := newServer(t)
	initial := getWithNavigationLocale(t, server, "/", nil)
	if initial.Code != http.StatusOK || initial.Header().Get("Content-Language") != "zh-Hant" ||
		!strings.Contains(initial.Header().Get("Vary"), "Cookie") || initial.Header().Get("Set-Cookie") != "" {
		t.Fatalf("initial language headers status=%d headers=%v", initial.Code, initial.Header())
	}
	for _, want := range []string{
		`<html lang="zh-Hant">`, `class="language-switch" href="/preferences/navigation-language/en?return_to=%2F"`,
		`aria-label="將管理中心導覽切換為英文">English</a>`, `aria-label="主要導覽"`,
	} {
		if !strings.Contains(initial.Body.String(), want) {
			t.Fatalf("Traditional Chinese shell missing %q: %s", want, initial.Body.String())
		}
	}

	preference := getWithNavigationLocale(t, server,
		"/preferences/navigation-language/en?return_to=%2Fmachines%3Fstate%3DDegraded", nil)
	if preference.Code != http.StatusSeeOther || preference.Header().Get("Location") != "/machines?state=Degraded" ||
		preference.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("language preference response status=%d headers=%v body=%s",
			preference.Code, preference.Header(), preference.Body.String())
	}
	cookies := preference.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("language preference cookies=%v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != "clawctl_navigation_locale" || cookie.Value != "en" || cookie.Path != "/" ||
		cookie.MaxAge != 31536000 || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("language preference cookie=%+v", cookie)
	}

	english := getWithNavigationLocale(t, server, "/machines?state=Degraded", cookie)
	if english.Code != http.StatusOK || english.Header().Get("Content-Language") != "en" {
		t.Fatalf("English navigation status=%d headers=%v body=%s", english.Code, english.Header(), english.Body.String())
	}
	body := english.Body.String()
	for _, want := range []string{
		`<html lang="en">`, `<title>Devices · AI-Intune</title>`, `lang="zh-Hant"`,
		`AI-Intune admin center`, `Tailscale verified`, `aria-label="Primary navigation"`,
		`<span>Dashboard</span>`, `<span>Devices</span>`, `<span>Apps</span>`,
		`<span>Endpoint security</span>`, `<span>Agents</span>`, `<span>Reports</span>`,
		`<span>Tenant administration</span>`, `<span>Troubleshooting &#43; support</span>`,
		`title="Source device `,
		`aria-label="Devices navigation"`, `>Overview</a>`, `>All devices</a>`,
		`aria-label="Switch management-center navigation to Traditional Chinese">繁體中文</a>`,
		`href="/preferences/navigation-language/zh-Hant?return_to=%2Fmachines%3Fstate%3DDegraded"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("English admin-center navigation missing %q: %s", want, body)
		}
	}
	// The evidence-rich page body has not been translated in this slice. Its
	// explicit language boundary prevents the navigation preference from
	// mislabelling Traditional Chinese evidence as English.
	if !strings.Contains(body, `<main class="main-content" id="main-content" tabindex="-1" lang="zh-Hant">`) ||
		!strings.Contains(body, `<h1>裝置</h1>`) {
		t.Fatal("English navigation did not preserve the explicit Traditional Chinese content boundary")
	}

	traditionalChinese := getWithNavigationLocale(t, server,
		"/preferences/navigation-language/zh-Hant?return_to=%2Freports%3Fsection%3Daudit", cookie)
	if traditionalChinese.Code != http.StatusSeeOther ||
		traditionalChinese.Header().Get("Location") != "/reports?section=audit" {
		t.Fatalf("Traditional Chinese preference status=%d headers=%v body=%s",
			traditionalChinese.Code, traditionalChinese.Header(), traditionalChinese.Body.String())
	}
	traditionalChineseCookies := traditionalChinese.Result().Cookies()
	if len(traditionalChineseCookies) != 1 || traditionalChineseCookies[0].Value != "zh-Hant" {
		t.Fatalf("Traditional Chinese preference cookies=%v", traditionalChineseCookies)
	}
}

func TestNavigationLanguagePreferenceRejectsUnboundedOrExternalReturnTargets(t *testing.T) {
	server, _ := newServer(t)
	tests := []string{
		"/preferences/navigation-language/fr?return_to=%2F",
		"/preferences/navigation-language/en",
		"/preferences/navigation-language/en?return_to=https%3A%2F%2Fevil.example%2F",
		"/preferences/navigation-language/en?return_to=%2F%2Fevil.example%2F",
		"/preferences/navigation-language/en?return_to=%2F&return_to=%2Fmachines",
		"/preferences/navigation-language/en?return_to=%2F&extra=1",
	}
	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			response := getWithNavigationLocale(t, server, target, nil)
			if response.Code != http.StatusBadRequest || response.Header().Get("Set-Cookie") != "" ||
				response.Header().Get("Location") != "" {
				t.Fatalf("invalid preference status=%d headers=%v body=%s",
					response.Code, response.Header(), response.Body.String())
			}
		})
	}
}

func TestUnknownNavigationLanguageCookieFallsBackToTraditionalChinese(t *testing.T) {
	server, _ := newServer(t)
	response := getWithNavigationLocale(t, server, "/", &http.Cookie{
		Name: navigationLocaleCookie, Value: "future-locale",
	})
	if response.Code != http.StatusOK || response.Header().Get("Content-Language") != "zh-Hant" ||
		!strings.Contains(response.Body.String(), `<html lang="zh-Hant">`) ||
		!strings.Contains(response.Body.String(), `>English</a>`) {
		t.Fatalf("unknown locale did not fail closed to Traditional Chinese: status=%d headers=%v body=%s",
			response.Code, response.Header(), response.Body.String())
	}
}

func TestNavigationReturnTargetAcceptsOnlyBoundedLocalRequestURIs(t *testing.T) {
	valid, err := navigationReturnTarget(url.Values{"return_to": {"/machines?state=Degraded"}})
	if err != nil || valid != "/machines?state=Degraded" {
		t.Fatalf("valid return target=%q err=%v", valid, err)
	}

	for name, query := range map[string]url.Values{
		"empty":           {"return_to": {""}},
		"oversized":       {"return_to": {"/" + strings.Repeat("a", navigationReturnTargetMaxBytes)}},
		"absolute":        {"return_to": {"https://evil.example/"}},
		"http absolute":   {"return_to": {"http://evil.example/"}},
		"opaque absolute": {"return_to": {"http:evil.example"}},
		"relative":        {"return_to": {"machines"}},
		"scheme relative": {"return_to": {"//evil.example/"}},
		"backslash":       {"return_to": {`/machines\evil`}},
		"newline":         {"return_to": {"/machines\n/evil"}},
		"format rune":     {"return_to": {"/machines\u200b"}},
		"fragment":        {"return_to": {"/machines#secret"}},
		"duplicate":       {"return_to": {"/", "/machines"}},
		"extra field":     {"return_to": {"/"}, "extra": {"1"}},
	} {
		t.Run(name, func(t *testing.T) {
			if target, err := navigationReturnTarget(query); err == nil {
				t.Fatalf("navigationReturnTarget(%v)=%q, want rejection", query, target)
			}
		})
	}
}
