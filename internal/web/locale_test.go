package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func postNavigationLocale(t *testing.T, server *Server, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	server.Routes(mux)
	recorder := httptest.NewRecorder()
	request := verifiedWebRequest(httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)), "example.com/cap/clawctl-view")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(recorder, request)
	return recorder
}

// formEncodeEveryByte is the longest encoding a browser can send for s.
func formEncodeEveryByte(s string) string {
	var encoded strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&encoded, "%%%02X", s[i])
	}
	return encoded.String()
}

func assertOneNavigationLanguageForm(t *testing.T, body, locale, returnTo string) {
	t.Helper()
	if strings.Count(body, `action="/preferences/navigation-language"`) != 1 ||
		strings.Count(body, `<form class="language-form" method="post" action="/preferences/navigation-language">`) != 1 ||
		strings.Count(body, `name="locale"`) != 1 || strings.Count(body, `name="return_to"`) != 1 ||
		!strings.Contains(body, `<input type="hidden" name="locale" value="`+locale+`">`) ||
		!strings.Contains(body, `<input type="hidden" name="return_to" value="`+returnTo+`">`) ||
		strings.Contains(body, `href="/preferences/navigation-language`) {
		t.Fatalf("navigation language form for locale=%q return_to=%q: %s", locale, returnTo, body)
	}
}

func TestNavigationLanguagePreferencePersistsEnglishAdminCenterShell(t *testing.T) {
	server, st := newServer(t)
	initial := getWithNavigationLocale(t, server, "/", nil)
	if initial.Code != http.StatusOK || initial.Header().Get("Content-Language") != "zh-Hant" ||
		!strings.Contains(initial.Header().Get("Vary"), "Cookie") || initial.Header().Get("Set-Cookie") != "" {
		t.Fatalf("initial language headers status=%d headers=%v", initial.Code, initial.Header())
	}
	for _, want := range []string{
		`<html lang="zh-Hant">`,
		`<button class="language-switch" type="submit" title="將管理中心導覽切換為英文" aria-label="將管理中心導覽切換為英文">English</button>`,
		`aria-label="主要導覽"`,
	} {
		if !strings.Contains(initial.Body.String(), want) {
			t.Fatalf("Traditional Chinese shell missing %q: %s", want, initial.Body.String())
		}
	}
	assertOneNavigationLanguageForm(t, initial.Body.String(), "en", "/")

	tables := []string{"audit_log", "operator_idempotency", "hub_events"}
	before := tableCounts(t, st, tables...)
	preference := postNavigationLocale(t, server, "/preferences/navigation-language",
		"locale=en&return_to=%2Fmachines%3Fstate%3DDegraded")
	if preference.Code != http.StatusSeeOther || preference.Header().Get("Location") != "/machines?state=Degraded" ||
		preference.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("language preference response status=%d headers=%v body=%s",
			preference.Code, preference.Header(), preference.Body.String())
	}
	if after := tableCounts(t, st, tables...); !reflect.DeepEqual(after, before) {
		t.Fatalf("language preference wrote Hub state: before=%v after=%v", before, after)
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
		`aria-label="Switch management-center navigation to Traditional Chinese">繁體中文</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("English admin-center navigation missing %q: %s", want, body)
		}
	}
	assertOneNavigationLanguageForm(t, body, "zh-Hant", "/machines?state=Degraded")
	// The evidence-rich page body has not been translated in this slice. Its
	// explicit language boundary prevents the navigation preference from
	// mislabelling Traditional Chinese evidence as English.
	if !strings.Contains(body, `<main class="main-content" id="main-content" tabindex="-1" lang="zh-Hant">`) ||
		!strings.Contains(body, `<h1>裝置</h1>`) {
		t.Fatal("English navigation did not preserve the explicit Traditional Chinese content boundary")
	}

	traditionalChinese := postNavigationLocale(t, server, "/preferences/navigation-language",
		"locale=zh-Hant&return_to=%2Freports%3Fsection%3Daudit")
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

func TestNavigationLanguagePreferenceAcceptsTheLongestValidForm(t *testing.T) {
	server, _ := newServer(t)
	target := "/" + strings.Repeat("a", navigationReturnTargetMaxBytes-1)
	body := "locale=zh-Hant&return_to=" + formEncodeEveryByte(target)
	if len(body) != navigationLanguageFormMaxBytes {
		t.Fatalf("longest valid form=%d bytes, limit=%d", len(body), navigationLanguageFormMaxBytes)
	}
	response := postNavigationLocale(t, server, "/preferences/navigation-language", body)
	cookies := response.Result().Cookies()
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != target ||
		len(cookies) != 1 || cookies[0].Value != "zh-Hant" {
		t.Fatalf("longest valid form status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestNavigationLanguagePreferenceRejectsInvalidRequests(t *testing.T) {
	const invalid = "導覽語言請求無效。\n"
	longestValidForm := "locale=zh-Hant&return_to=" +
		formEncodeEveryByte("/"+strings.Repeat("a", navigationReturnTargetMaxBytes-1))
	for name, test := range map[string]struct {
		target string
		body   string
		want   string
	}{
		"unsupported locale":      {"/preferences/navigation-language", "locale=fr&return_to=%2F", "不支援的導覽語言。\n"},
		"missing return_to":       {"/preferences/navigation-language", "locale=en", invalid},
		"missing locale":          {"/preferences/navigation-language", "return_to=%2F", invalid},
		"absolute URL":            {"/preferences/navigation-language", "locale=en&return_to=https%3A%2F%2Fevil.example%2F", invalid},
		"//evil.example/":         {"/preferences/navigation-language", "locale=en&return_to=%2F%2Fevil.example%2F", invalid},
		"repeated return_to":      {"/preferences/navigation-language", "locale=en&return_to=%2F&return_to=%2Fmachines", invalid},
		"repeated locale":         {"/preferences/navigation-language", "locale=en&locale=zh-Hant&return_to=%2F", invalid},
		"an extra field":          {"/preferences/navigation-language", "locale=en&return_to=%2F&extra=1", invalid},
		"a URL query on the POST": {"/preferences/navigation-language?locale=en", "locale=en&return_to=%2F", invalid},
		"an empty URL query":      {"/preferences/navigation-language?", "locale=en&return_to=%2F", invalid},
		"a body over the limit":   {"/preferences/navigation-language", longestValidForm + "&", invalid},
	} {
		t.Run(name, func(t *testing.T) {
			server, _ := newServer(t)
			response := postNavigationLocale(t, server, test.target, test.body)
			if response.Code != http.StatusBadRequest || response.Body.String() != test.want ||
				response.Header().Get("Set-Cookie") != "" || response.Header().Get("Location") != "" ||
				response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("invalid preference status=%d headers=%v body=%q",
					response.Code, response.Header(), response.Body.String())
			}
		})
	}
}

func TestNavigationLanguagePreferenceHasNoGETRoute(t *testing.T) {
	for _, test := range []struct {
		target string
		status int
	}{
		{"/preferences/navigation-language", http.StatusMethodNotAllowed},
		{"/preferences/navigation-language?locale=en&return_to=%2F", http.StatusMethodNotAllowed},
		{"/preferences/navigation-language/en", http.StatusNotFound},
		{"/preferences/navigation-language/en?return_to=%2F", http.StatusNotFound},
	} {
		t.Run(test.target, func(t *testing.T) {
			server, _ := newServer(t)
			response := getWithNavigationLocale(t, server, test.target, nil)
			if response.Code != test.status || response.Header().Get("Set-Cookie") != "" ||
				response.Header().Get("Location") != "" {
				t.Fatalf("GET %s status=%d headers=%v, want %d and no cookie",
					test.target, response.Code, response.Header(), test.status)
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
		!strings.Contains(response.Body.String(), `>English</button>`) {
		t.Fatalf("unknown locale did not fail closed to Traditional Chinese: status=%d headers=%v body=%s",
			response.Code, response.Header(), response.Body.String())
	}
}

func TestNavigationReturnTargetAcceptsOnlyBoundedLocalRequestURIs(t *testing.T) {
	for _, target := range []string{
		"/machines?state=Degraded",
		"/" + strings.Repeat("a", navigationReturnTargetMaxBytes-1),
	} {
		if valid, err := navigationReturnTarget(target); err != nil || valid != target {
			t.Fatalf("valid return target=%q err=%v", valid, err)
		}
	}

	for name, target := range map[string]string{
		"empty":           "",
		"oversized":       "/" + strings.Repeat("a", navigationReturnTargetMaxBytes),
		"absolute":        "https://evil.example/",
		"http absolute":   "http://evil.example/",
		"opaque absolute": "http:evil.example",
		"relative":        "machines",
		"scheme relative": "//evil.example/",
		"backslash":       `/machines\evil`,
		"newline":         "/machines\n/evil",
		"carriage return": "/machines\r/evil",
		"C1 control":      "/machines\u0085",
		"format rune":     "/machines​",
		"fragment":        "/machines#secret",
	} {
		t.Run(name, func(t *testing.T) {
			if valid, err := navigationReturnTarget(target); err == nil {
				t.Fatalf("navigationReturnTarget(%q)=%q, want rejection", target, valid)
			}
		})
	}
}
