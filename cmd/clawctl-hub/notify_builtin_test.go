package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	testBotToken      = "123456:TEST-TOKEN"
	testChatID        = "-1000000000001"
	testWebhookSecret = "webhook-hmac-secret"
	testWebhookPath   = "/hooks/SUPER-WEBHOOK-SECRET"
)

func writeNotifyEnvFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	if mode == 0 {
		mode = 0o600
	}
	p := filepath.Join(t.TempDir(), "notify.env")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseNotifyEnv(t *testing.T) {
	raw := []byte("" +
		"# comment\n" +
		"\n" +
		"export TELEGRAM_BOT_TOKEN=\"123456:TEST-TOKEN\"\n" +
		"TELEGRAM_CHAT_ID='-1000000000001'\n" +
		"CLAWCTL_NOTIFY_WEBHOOK_URL=https://example.com/hook\n" +
		"UNKNOWN_KEY=ignored\n" +
		"export CLAWCTL_NOTIFY_WEBHOOK_SECRET=\"s3cret\"\n")
	kv := parseNotifyEnv(raw)
	if kv["TELEGRAM_BOT_TOKEN"] != testBotToken {
		t.Fatalf("token = %q", kv["TELEGRAM_BOT_TOKEN"])
	}
	if kv["TELEGRAM_CHAT_ID"] != testChatID {
		t.Fatalf("chat = %q", kv["TELEGRAM_CHAT_ID"])
	}
	if kv["CLAWCTL_NOTIFY_WEBHOOK_URL"] != "https://example.com/hook" {
		t.Fatalf("webhook = %q", kv["CLAWCTL_NOTIFY_WEBHOOK_URL"])
	}
	if kv["CLAWCTL_NOTIFY_WEBHOOK_SECRET"] != "s3cret" {
		t.Fatalf("secret = %q", kv["CLAWCTL_NOTIFY_WEBHOOK_SECRET"])
	}
	if _, ok := kv["UNKNOWN_KEY"]; !ok {
		t.Fatal("unknown key was dropped instead of stored (loadNotifyEnvFile ignores it)")
	}
}

func TestParseNotifyEnvCRLF(t *testing.T) {
	raw := []byte("TELEGRAM_BOT_TOKEN=abc\r\nTELEGRAM_CHAT_ID=1\r\n")
	kv := parseNotifyEnv(raw)
	if kv["TELEGRAM_BOT_TOKEN"] != "abc" || kv["TELEGRAM_CHAT_ID"] != "1" {
		t.Fatalf("crlf parse = %#v", kv)
	}
}

func TestLoadNotifyEnvHalfTelegram(t *testing.T) {
	for _, tc := range []struct {
		name, content, key string
	}{
		{"token only", "TELEGRAM_BOT_TOKEN=" + testBotToken + "\n", "TELEGRAM_CHAT_ID"},
		{"chat only", "TELEGRAM_CHAT_ID=" + testChatID + "\n", "TELEGRAM_BOT_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeNotifyEnvFile(t, tc.content, 0o600)
			_, err := loadNotifyEnvFile(p)
			if err == nil {
				t.Fatal("expected half-Telegram config error")
			}
			msg := err.Error()
			if !strings.Contains(msg, p) || !strings.Contains(msg, tc.key) {
				t.Fatalf("error %q should name path and %s", msg, tc.key)
			}
			if strings.Contains(msg, testBotToken) || strings.Contains(msg, testChatID) {
				t.Fatalf("error leaked a secret: %s", msg)
			}
		})
	}
}

func TestValidateWebhookURL(t *testing.T) {
	ok := []string{
		"https://example.com/hook",
		"https://example.com/hook?token=abc",
		"http://127.0.0.1/hook",
		"http://127.0.0.1:8080/hook",
		"http://[::1]/hook",
		"http://100.64.0.1/hook",
		"http://100.127.255.254/hooks/x",
	}
	for _, u := range ok {
		if err := validateWebhookURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	bad := []string{
		"http://example.com/hook",
		"http://192.168.0.1/hook",
		"http://100.128.0.1/hook",
		"http://10.0.0.1/hook",
		"example.com/hook",
		"ftp://example.com/hook",
		"https://",
		"/relative",
	}
	for _, u := range bad {
		if err := validateWebhookURL(u); err == nil {
			t.Errorf("%s: accepted", u)
		}
	}
}

func TestLoadNotifyEnvUnknownKeysAndQuotes(t *testing.T) {
	p := writeNotifyEnvFile(t, ""+
		"# ops leftover\n"+
		"export TELEGRAM_BOT_TOKEN=\"123456:TEST-TOKEN\"\n"+
		"TELEGRAM_CHAT_ID=-1000000000001\n"+
		"DEADMAN_STAMP=/tmp/stamp\n", 0o600)
	cfg, err := loadNotifyEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.telegram() || cfg.webhook() {
		t.Fatalf("channels telegram=%v webhook=%v", cfg.telegram(), cfg.webhook())
	}
}

func TestPrepareNotifyPrecedence(t *testing.T) {
	env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)

	cmdAndEnv, err := prepareNotify("cat >/dev/null", env)
	if err != nil {
		t.Fatal(err)
	}
	if cmdAndEnv.builtin != nil || cmdAndEnv.kind != "command" || !cmdAndEnv.cmdHidesBuiltin {
		t.Fatalf("cmd+env: %+v", cmdAndEnv)
	}

	onlyEnv, err := prepareNotify("", env)
	if err != nil {
		t.Fatal(err)
	}
	if onlyEnv.builtin == nil || onlyEnv.kind != "telegram" || onlyEnv.cmdHidesBuiltin {
		t.Fatalf("only env: %+v", onlyEnv)
	}

	neither, err := prepareNotify("", "")
	if err != nil {
		t.Fatal(err)
	}
	if neither.builtin != nil || neither.kind != "" || neither.cmd != "" {
		t.Fatalf("neither: %+v", neither)
	}

	emptyFile := writeNotifyEnvFile(t, "# nothing\nOTHER=1\n", 0o600)
	noChannel, err := prepareNotify("", emptyFile)
	if err != nil {
		t.Fatal(err)
	}
	if noChannel.builtin != nil {
		t.Fatal("empty channels should not select builtin")
	}
}

func TestPrepareNotifyUnreadableFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.env")
	_, err := prepareNotify("", p)
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("missing file error = %v", err)
	}
}

// A Hub that uses a notify command must start even when CLAWCTL_NOTIFY_ENV
// names a file the Hub cannot read (for example a host path forwarded into a
// container): the file is the command's, not the Hub's.
func TestPrepareNotifyCommandToleratesUnusableEnvFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "host-only.env")
	setup, err := prepareNotify("cat >/dev/null", missing)
	if err != nil {
		t.Fatalf("command + unusable env file must not fail startup: %v", err)
	}
	if setup.kind != "command" || setup.builtin != nil {
		t.Fatalf("setup = %+v, want command notifier", setup)
	}
	if !strings.Contains(setup.modeWarn, missing) || !strings.Contains(setup.modeWarn, "not fatal") {
		t.Fatalf("warning = %q, want path and not-fatal note", setup.modeWarn)
	}
	half := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\n", 0o600)
	setup, err = prepareNotify("cat >/dev/null", half)
	if err != nil || setup.kind != "command" {
		t.Fatalf("command + invalid env file: setup=%+v err=%v", setup, err)
	}
	if strings.Contains(setup.modeWarn, testBotToken) {
		t.Fatal("warning leaked the token")
	}
}

func TestPrepareNotifyModeWarn(t *testing.T) {
	p := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o640)
	setup, err := prepareNotify("", p)
	if err != nil {
		t.Fatal(err)
	}
	if setup.modeWarn == "" || !strings.Contains(setup.modeWarn, p) || !strings.Contains(setup.modeWarn, "0640") {
		t.Fatalf("mode warn = %q", setup.modeWarn)
	}
	if strings.Contains(setup.modeWarn, testBotToken) {
		t.Fatalf("mode warn leaked token: %s", setup.modeWarn)
	}
	strict := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
	ok, err := prepareNotify("", strict)
	if err != nil {
		t.Fatal(err)
	}
	if ok.modeWarn != "" {
		t.Fatalf("0600 warned: %s", ok.modeWarn)
	}
}

func TestNotifyMetricsConfiguredFlag(t *testing.T) {
	env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)

	cmdHub := newHub(t, "")
	cmdHub.notifyCmd = "exit 1"
	if got := single(t, parseExposition(t, metricsBody(t, cmdHub)), "clawctl_notify_configured"); got != 1 {
		t.Fatalf("command configured = %v", got)
	}

	builtinHub := newHub(t, "")
	setup, err := prepareNotify("", env)
	if err != nil {
		t.Fatal(err)
	}
	builtinHub.notifyEnv = setup.envPath
	builtinHub.notifyBuiltin = setup.builtin
	builtinHub.notifyKind = setup.kind
	body := metricsBody(t, builtinHub)
	if !strings.Contains(body, "1 when a notifier (command or built-in channel) is configured, otherwise 0.") {
		t.Fatalf("HELP text missing:\n%s", body)
	}
	if got := single(t, parseExposition(t, body), "clawctl_notify_configured"); got != 1 {
		t.Fatalf("builtin configured = %v", got)
	}

	none := newHub(t, "")
	if got := single(t, parseExposition(t, metricsBody(t, none)), "clawctl_notify_configured"); got != 0 {
		t.Fatalf("unconfigured = %v", got)
	}
}

func TestLogNotifyConfiguredKinds(t *testing.T) {
	env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
	buf := captureLog(t)

	h := newHub(t, "")
	h.notifyCmd = "true"
	h.notifyEnv = env
	h.notifyEnvChannelsIgnored = true
	h.logNotifyConfigured()
	out := buf.String()
	if !strings.Contains(out, "notify: using notify command; built-in channels in the notify env file are not used") {
		t.Fatalf("cmd+channels log = %q", out)
	}
	if strings.Contains(out, testBotToken) {
		t.Fatalf("log leaked token: %s", out)
	}

	buf.Reset()
	h = newHub(t, "")
	h.notifyEnv = env
	h.notifyBuiltin = &builtinNotifier{path: env, channels: "telegram"}
	h.notifyKind = "telegram"
	h.logNotifyConfigured()
	out = buf.String()
	if !strings.Contains(out, "notify: using telegram") || !strings.Contains(out, env) {
		t.Fatalf("telegram log = %q", out)
	}
	if strings.Contains(out, testBotToken) || strings.Contains(out, testChatID) {
		t.Fatalf("log leaked secret: %s", out)
	}
}

func telegramHandler(t *testing.T, token string, fn func(method string, form url.Values, w http.ResponseWriter, r *http.Request)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/bot" + token + "/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			t.Errorf("path %q does not start with /bot<token>/", r.URL.Path)
			http.Error(w, "not found", 404)
			return
		}
		method := strings.TrimPrefix(r.URL.Path, prefix)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "form", 400)
			return
		}
		fn(method, r.Form, w, r)
	})
}

func writeTelegramJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestTelegramNotifySuccessAndForm(t *testing.T) {
	var gotMethod string
	var gotForm url.Values
	srv := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
		gotMethod = method
		gotForm = form
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		if strings.Contains(r.Form.Encode(), testBotToken) {
			t.Error("bot token appeared in the form body")
		}
		writeTelegramJSON(w, 200, `{"ok":true,"result":{}}`)
	}))
	defer srv.Close()
	env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
	n := builtinNotifier{path: env, apiBase: srv.URL, channels: "telegram"}
	channel, err := n.Notify(context.Background(), "daily", "hello fleet")
	if err != nil {
		t.Fatal(err)
	}
	if channel != "telegram" {
		t.Fatalf("channel = %q", channel)
	}
	if gotMethod != "sendMessage" {
		t.Fatalf("method = %q", gotMethod)
	}
	if gotForm.Get("chat_id") != testChatID || gotForm.Get("text") != "hello fleet" || gotForm.Get("disable_web_page_preview") != "true" {
		t.Fatalf("form = %v", gotForm)
	}
}

func TestTelegramNotifyOKFalseAndNon200(t *testing.T) {
	t.Run("ok false", func(t *testing.T) {
		srv := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
			writeTelegramJSON(w, 200, `{"ok":false,"description":"Bad Request: chat not found"}`)
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
		n := builtinNotifier{path: env, apiBase: srv.URL, channels: "telegram"}
		_, err := n.Notify(context.Background(), "daily", "hello")
		if err == nil || !strings.Contains(err.Error(), "Bad Request: chat not found") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), testBotToken) {
			t.Fatalf("error leaked token: %v", err)
		}
	})
	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
			writeTelegramJSON(w, 401, `{"ok":false,"description":"Unauthorized"}`)
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
		n := builtinNotifier{path: env, apiBase: srv.URL, channels: "telegram"}
		_, err := n.Notify(context.Background(), "daily", "hello")
		if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestTelegramEmptyBodyRefused(t *testing.T) {
	called := false
	srv := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
		called = true
		writeTelegramJSON(w, 200, `{"ok":true}`)
	}))
	defer srv.Close()
	env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
	n := builtinNotifier{path: env, apiBase: srv.URL, channels: "telegram"}
	_, err := n.Notify(context.Background(), "daily", "  \n\t")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("empty body was posted")
	}
}

func TestTelegramTruncation(t *testing.T) {
	var got string
	srv := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
		got = form.Get("text")
		writeTelegramJSON(w, 200, `{"ok":true}`)
	}))
	defer srv.Close()
	env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
	n := builtinNotifier{path: env, apiBase: srv.URL, channels: "telegram"}
	body := strings.Repeat("世", telegramMaxRunes+1)
	if _, err := n.Notify(context.Background(), "daily", body); err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(got) <= telegramMaxRunes {
		t.Fatalf("truncated length %d, want original 3900 runes plus suffix", utf8.RuneCountInString(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("世", telegramMaxRunes)) {
		t.Fatal("truncated text lost the leading runes")
	}
	if !strings.HasSuffix(got, telegramTruncateSuffix) {
		t.Fatalf("suffix missing: %q", got[len(got)-80:])
	}
}

func TestWebhookSuccessWithAndWithoutSignature(t *testing.T) {
	t.Run("with signature", func(t *testing.T) {
		var raw []byte
		var sig string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ = io.ReadAll(r.Body)
			sig = r.Header.Get("X-Clawctl-Signature")
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("content-type %s", r.Header.Get("Content-Type"))
			}
			w.WriteHeader(http.StatusAccepted)
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, ""+
			"CLAWCTL_NOTIFY_WEBHOOK_URL="+srv.URL+testWebhookPath+"\n"+
			"CLAWCTL_NOTIFY_WEBHOOK_SECRET="+testWebhookSecret+"\n", 0o600)
		n := builtinNotifier{path: env, channels: "webhook"}
		channel, err := n.Notify(context.Background(), "daily", "hello")
		if err != nil {
			t.Fatal(err)
		}
		if channel != "webhook" {
			t.Fatalf("channel = %q", channel)
		}
		var payload webhookPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Kind != "daily" || payload.Text != "hello" {
			t.Fatalf("payload = %+v", payload)
		}
		if _, err := time.Parse(time.RFC3339, payload.SentAt); err != nil {
			t.Fatalf("sent_at %q: %v", payload.SentAt, err)
		}
		mac := hmac.New(sha256.New, []byte(testWebhookSecret))
		_, _ = mac.Write(raw)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if sig != want {
			t.Fatalf("sig = %q want %q", sig, want)
		}
	})
	t.Run("without signature", func(t *testing.T) {
		var sig string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sig = r.Header.Get("X-Clawctl-Signature")
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, "CLAWCTL_NOTIFY_WEBHOOK_URL="+srv.URL+"\n", 0o600)
		n := builtinNotifier{path: env, channels: "webhook"}
		if _, err := n.Notify(context.Background(), "daily", "hello"); err != nil {
			t.Fatal(err)
		}
		if sig != "" {
			t.Fatalf("unexpected signature %q", sig)
		}
	})
}

func TestWebhookNon2xxAndRedirect(t *testing.T) {
	t.Run("non-2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", http.StatusInternalServerError)
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, "CLAWCTL_NOTIFY_WEBHOOK_URL="+srv.URL+testWebhookPath+"\n", 0o600)
		n := builtinNotifier{path: env, channels: "webhook"}
		_, err := n.Notify(context.Background(), "daily", "hello")
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), testWebhookPath) || strings.Contains(err.Error(), srv.URL) {
			t.Fatalf("error leaked URL: %v", err)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			http.Redirect(w, r, "https://example.com/steal?token="+testBotToken, http.StatusFound)
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, "CLAWCTL_NOTIFY_WEBHOOK_URL="+srv.URL+testWebhookPath+"\n", 0o600)
		n := builtinNotifier{path: env, channels: "webhook"}
		_, err := n.Notify(context.Background(), "daily", "hello")
		if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
			t.Fatalf("err = %v", err)
		}
		if hits != 1 {
			t.Fatalf("followed redirect: hits=%d", hits)
		}
		if strings.Contains(err.Error(), testBotToken) || strings.Contains(err.Error(), testWebhookPath) {
			t.Fatalf("error leaked secret: %v", err)
		}
	})
}

func TestBothChannelsOneFails(t *testing.T) {
	tg := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
		writeTelegramJSON(w, 200, `{"ok":true}`)
	}))
	defer tg.Close()
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer hook.Close()
	env := writeNotifyEnvFile(t, ""+
		"TELEGRAM_BOT_TOKEN="+testBotToken+"\n"+
		"TELEGRAM_CHAT_ID="+testChatID+"\n"+
		"CLAWCTL_NOTIFY_WEBHOOK_URL="+hook.URL+testWebhookPath+"\n", 0o600)
	n := builtinNotifier{path: env, apiBase: tg.URL, channels: "telegram+webhook"}
	channel, err := n.Notify(context.Background(), "daily", "hello")
	if channel != "telegram+webhook" {
		t.Fatalf("channel = %q", channel)
	}
	if err == nil || !strings.Contains(err.Error(), "webhook") || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testBotToken) || strings.Contains(err.Error(), testWebhookPath) {
		t.Fatalf("error leaked a secret: %v", err)
	}
}

func TestDeliverBuiltinNotifierRecordsAndBackoff(t *testing.T) {
	var ok atomic.Bool
	srv := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
		if !ok.Load() {
			writeTelegramJSON(w, 200, `{"ok":false,"description":"flood"}`)
			return
		}
		writeTelegramJSON(w, 200, `{"ok":true}`)
	}))
	defer srv.Close()
	env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
	h := newHub(t, "")
	h.reportAt = "08:00"
	h.notifyEnv = env
	h.notifyBuiltin = &builtinNotifier{path: env, apiBase: srv.URL, channels: "telegram"}
	h.notifyKind = "telegram"
	due := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)

	h.maybeSendReport(due)
	if got := notificationRows(t, h, "daily"); got != 1 {
		t.Fatalf("rows after fail = %d", got)
	}
	var channel string
	var delivered int
	var errText string
	if err := h.store.DB().QueryRow(`SELECT channel, delivered, COALESCE(error,'') FROM notifications WHERE kind='daily'`).Scan(&channel, &delivered, &errText); err != nil {
		t.Fatal(err)
	}
	if channel != "telegram" || delivered != 0 || !strings.Contains(errText, "flood") {
		t.Fatalf("row channel=%s delivered=%d err=%q", channel, delivered, errText)
	}
	if strings.Contains(errText, testBotToken) {
		t.Fatalf("DB error leaked token: %s", errText)
	}

	h.maybeSendReport(due.Add(30 * time.Second))
	if got := notificationRows(t, h, "daily"); got != 1 {
		t.Fatalf("retried inside 1m backoff: rows=%d", got)
	}

	h.maybeSendReport(due.Add(time.Minute))
	if got := notificationRows(t, h, "daily"); got != 2 {
		t.Fatalf("missed the 1m retry: rows=%d", got)
	}

	ok.Store(true)
	h.maybeSendReport(due.Add(3 * time.Minute))
	if got := notificationRows(t, h, "daily"); got != 3 {
		t.Fatalf("success attempt rows=%d", got)
	}
	var lastDelivered int
	if err := h.store.DB().QueryRow(`SELECT delivered FROM notifications WHERE kind='daily' ORDER BY sent_at DESC LIMIT 1`).Scan(&lastDelivered); err != nil {
		t.Fatal(err)
	}
	if lastDelivered != 1 {
		t.Fatal("last row was not delivered")
	}
	h.maybeSendReport(due.Add(4 * time.Minute))
	if got := notificationRows(t, h, "daily"); got != 3 {
		t.Fatalf("delivered report was sent again: rows=%d", got)
	}
}

func TestBuiltinTokenRotation(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		const prefix = "/bot"
		rest := strings.TrimPrefix(path, prefix)
		tok, _, _ := strings.Cut(rest, "/")
		tokens = append(tokens, tok)
		writeTelegramJSON(w, 200, `{"ok":true}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	p := filepath.Join(dir, "notify.env")
	if err := os.WriteFile(p, []byte("TELEGRAM_BOT_TOKEN=token-one\nTELEGRAM_CHAT_ID="+testChatID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := builtinNotifier{path: p, apiBase: srv.URL, channels: "telegram"}
	if _, err := n.Notify(context.Background(), "daily", "one"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("TELEGRAM_BOT_TOKEN=token-two\nTELEGRAM_CHAT_ID="+testChatID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Notify(context.Background(), "daily", "two"); err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 || tokens[0] != "token-one" || tokens[1] != "token-two" {
		t.Fatalf("tokens = %v", tokens)
	}
}

func closedLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestNotifySecretsNeverLoggedOrReturned(t *testing.T) {
	addr := closedLocalAddr(t)
	closedBase := "http://" + addr
	secrets := []string{testBotToken, testChatID, testWebhookSecret, testWebhookPath, closedBase}

	assertClean := func(t *testing.T, name, text string) {
		t.Helper()
		for _, s := range secrets {
			if s != "" && strings.Contains(text, s) {
				t.Errorf("%s leaked %q in:\n%s", name, s, text)
			}
		}
	}

	t.Run("network error telegram", func(t *testing.T) {
		buf := captureLog(t)
		env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
		n := builtinNotifier{path: env, apiBase: closedBase, channels: "telegram"}
		h := newHub(t, "")
		h.notifyBuiltin = &n
		h.notifyEnv = env
		_, err := n.Notify(context.Background(), "daily", "hello")
		if err == nil {
			t.Fatal("expected network error")
		}
		assertClean(t, "notify error", err.Error())
		h.notifyBuiltin = &n
		_ = h.deliver("daily", "hello", time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC))
		assertClean(t, "log", buf.String())
		var errText string
		if qerr := h.store.DB().QueryRow(`SELECT COALESCE(error,'') FROM notifications WHERE kind='daily'`).Scan(&errText); qerr != nil {
			t.Fatal(qerr)
		}
		assertClean(t, "db", errText)
		var uerr *url.Error
		if errors.As(err, &uerr) {
			t.Fatalf("returned *url.Error which embeds the URL: %#v", uerr)
		}
	})

	t.Run("network error webhook", func(t *testing.T) {
		buf := captureLog(t)
		hookURL := closedBase + testWebhookPath
		env := writeNotifyEnvFile(t, "CLAWCTL_NOTIFY_WEBHOOK_URL="+hookURL+"\nCLAWCTL_NOTIFY_WEBHOOK_SECRET="+testWebhookSecret+"\n", 0o600)
		n := builtinNotifier{path: env, channels: "webhook"}
		_, err := n.Notify(context.Background(), "daily", "hello")
		if err == nil {
			t.Fatal("expected network error")
		}
		assertClean(t, "notify error", err.Error())
		h := newHub(t, "")
		h.notifyBuiltin = &n
		h.notifyEnv = env
		_ = h.deliver("daily", "hello", time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC))
		assertClean(t, "log", buf.String())
		var errText string
		if qerr := h.store.DB().QueryRow(`SELECT COALESCE(error,'') FROM notifications WHERE kind='daily'`).Scan(&errText); qerr != nil {
			t.Fatal(qerr)
		}
		assertClean(t, "db", errText)
	})

	t.Run("ok false", func(t *testing.T) {
		buf := captureLog(t)
		srv := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
			writeTelegramJSON(w, 200, `{"ok":false,"description":"Unauthorized"}`)
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
		n := builtinNotifier{path: env, apiBase: srv.URL, channels: "telegram"}
		_, err := n.Notify(context.Background(), "daily", "hello")
		if err == nil {
			t.Fatal("expected ok:false")
		}
		assertClean(t, "notify error", err.Error())
		h := newHub(t, "")
		h.notifyBuiltin = &n
		_ = h.deliver("daily", "hello", time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC))
		assertClean(t, "log", buf.String())
	})

	t.Run("redirect", func(t *testing.T) {
		buf := captureLog(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/steal?token="+testBotToken, http.StatusFound)
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, "CLAWCTL_NOTIFY_WEBHOOK_URL="+srv.URL+testWebhookPath+"\nCLAWCTL_NOTIFY_WEBHOOK_SECRET="+testWebhookSecret+"\n", 0o600)
		n := builtinNotifier{path: env, channels: "webhook"}
		_, err := n.Notify(context.Background(), "daily", "hello")
		if err == nil {
			t.Fatal("expected redirect failure")
		}
		assertClean(t, "notify error", err.Error())
		h := newHub(t, "")
		h.notifyBuiltin = &n
		_ = h.deliver("daily", "hello", time.Date(2026, 10, 6, 11, 0, 0, 0, time.UTC))
		assertClean(t, "log", buf.String())
	})
}

func TestNotifyCheckTelegram(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
			switch method {
			case "getMe":
				writeTelegramJSON(w, 200, `{"ok":true,"result":{"username":"Fake_Bot"}}`)
			case "getChat":
				if form.Get("chat_id") != testChatID {
					t.Errorf("chat_id = %q", form.Get("chat_id"))
				}
				writeTelegramJSON(w, 200, `{"ok":true,"result":{"title":"Ops chat"}}`)
			default:
				t.Errorf("unexpected method %s", method)
				writeTelegramJSON(w, 404, `{"ok":false,"description":"not found"}`)
			}
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
		var out bytes.Buffer
		err := runNotifyCheck([]string{"--notify-env", env}, &out, io.Discard, srv.URL)
		if err != nil {
			t.Fatalf("err = %v output=%s", err, out.String())
		}
		if !strings.Contains(out.String(), "@Fake_Bot") || !strings.Contains(out.String(), "Ops chat") {
			t.Fatalf("output = %q", out.String())
		}
		if strings.Contains(out.String(), testBotToken) || strings.Contains(out.String(), testChatID) {
			t.Fatalf("check leaked secret: %s", out.String())
		}
	})
	t.Run("getChat fails", func(t *testing.T) {
		srv := httptest.NewServer(telegramHandler(t, testBotToken, func(method string, form url.Values, w http.ResponseWriter, r *http.Request) {
			switch method {
			case "getMe":
				writeTelegramJSON(w, 200, `{"ok":true,"result":{"username":"Fake_Bot"}}`)
			case "getChat":
				writeTelegramJSON(w, 200, `{"ok":false,"description":"Bad Request: chat not found"}`)
			}
		}))
		defer srv.Close()
		env := writeNotifyEnvFile(t, "TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n", 0o600)
		var out bytes.Buffer
		err := runNotifyCheck([]string{"--notify-env", env}, &out, io.Discard, srv.URL)
		if !errors.Is(err, errNotifyCheckFailed) {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(out.String(), "cannot send to this chat") || !strings.Contains(out.String(), "chat not found") {
			t.Fatalf("output = %q", out.String())
		}
		if strings.Contains(out.String(), testBotToken) {
			t.Fatalf("leaked token: %s", out.String())
		}
	})
}

func TestNotifyCheckWebhookPrintsSchemeHost(t *testing.T) {
	env := writeNotifyEnvFile(t, "CLAWCTL_NOTIFY_WEBHOOK_URL=https://example.com"+testWebhookPath+"\n", 0o600)
	var out bytes.Buffer
	if err := runNotifyCheck([]string{"--notify-env", env}, &out, io.Discard, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "notify-check: webhook https://example.com (config only; not posted)") {
		t.Fatalf("output = %q", out.String())
	}
	if strings.Contains(out.String(), testWebhookPath) {
		t.Fatalf("printed webhook path: %s", out.String())
	}
}

func TestNotifyFileRereadInvalidFailsAttempt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notify.env")
	if err := os.WriteFile(p, []byte("TELEGRAM_BOT_TOKEN="+testBotToken+"\nTELEGRAM_CHAT_ID="+testChatID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := builtinNotifier{path: p, apiBase: "http://127.0.0.1:1", channels: "telegram"}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	channel, err := n.Notify(context.Background(), "daily", "hello")
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("err = %v", err)
	}
	if strings.Count(err.Error(), p) < 1 {
		t.Fatalf("error should name the path: %v", err)
	}
	if channel == "" {
		t.Fatal("channel empty on load failure")
	}
	if strings.Contains(err.Error(), testBotToken) {
		t.Fatalf("leaked token: %v", err)
	}
}
