package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultTelegramAPI     = "https://api.telegram.org"
	telegramMaxRunes       = 3900
	telegramTruncateSuffix = "\n… (truncated; full text is on the Hub)"
	notifyRequestTimeout   = 20 * time.Second
	notifyResponseLimit    = 1 << 20
)

var webhookHTTPAllowedNet = netip.MustParsePrefix("100.64.0.0/10")

// notifyFile is a parsed CLAWCTL_NOTIFY_ENV file. Secrets stay in this
// value and are never copied onto hub or into logs.
type notifyFile struct {
	path          string
	perm          os.FileMode
	token         string
	chatID        string
	webhookURL    string
	webhookSecret string
}

func (f notifyFile) telegram() bool {
	return f.token != "" && f.chatID != ""
}

func (f notifyFile) webhook() bool {
	return f.webhookURL != ""
}

func (f notifyFile) hasChannel() bool {
	return f.telegram() || f.webhook()
}

func (f notifyFile) channelName() string {
	switch {
	case f.telegram() && f.webhook():
		return "telegram+webhook"
	case f.telegram():
		return "telegram"
	case f.webhook():
		return "webhook"
	default:
		return ""
	}
}

func (f notifyFile) secrets() []string {
	return []string{f.token, f.chatID, f.webhookURL, f.webhookSecret}
}

type notifySetup struct {
	cmd             string
	envPath         string
	builtin         *builtinNotifier
	kind            string
	cmdHidesBuiltin bool
	modeWarn        string
}

// builtinNotifier re-reads the env file on every Notify so a rotated token
// is picked up without a restart. apiBase is empty in production (Telegram's
// public API) and set only by tests.
type builtinNotifier struct {
	path     string
	apiBase  string
	channels string
}

func (n builtinNotifier) telegramAPI() string {
	if strings.TrimSpace(n.apiBase) != "" {
		return strings.TrimRight(n.apiBase, "/")
	}
	return defaultTelegramAPI
}

func newNotifyHTTPClient() *http.Client {
	return &http.Client{
		Timeout: notifyRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// prepareNotify decides which notifier serve() will use. Secrets are read
// only from envPath; cmd is the legacy --notify-cmd / CLAWCTL_NOTIFY_CMD.
func prepareNotify(cmd, envPath string) (notifySetup, error) {
	cmd = strings.TrimSpace(cmd)
	envPath = strings.TrimSpace(envPath)
	out := notifySetup{cmd: cmd, envPath: envPath}

	var cfg notifyFile
	var hasFile bool
	if envPath != "" {
		loaded, err := loadNotifyEnvFile(envPath)
		switch {
		case err != nil && cmd != "":
			// With a notify command the file belongs to that command (it
			// inherits CLAWCTL_NOTIFY_ENV). A path the Hub itself cannot read,
			// such as a host path inside a container, must not stop a Hub that
			// never uses the built-in channels.
			out.modeWarn = fmt.Sprintf("WARN notify env file %s is not usable by the Hub; the notify command is used, so this is not fatal", envPath)
		case err != nil:
			return notifySetup{}, err
		default:
			cfg = loaded
			hasFile = true
			if cfg.perm&^0o600 != 0 {
				out.modeWarn = fmt.Sprintf("WARN notify env file %s mode %04o is broader than 0600", envPath, cfg.perm)
			}
		}
	}

	if cmd != "" {
		out.kind = "command"
		if hasFile && cfg.hasChannel() {
			out.cmdHidesBuiltin = true
		}
		return out, nil
	}
	if hasFile && cfg.hasChannel() {
		out.kind = cfg.channelName()
		out.builtin = &builtinNotifier{path: envPath, channels: out.kind}
		return out, nil
	}
	return out, nil
}

func loadNotifyEnvFile(path string) (notifyFile, error) {
	if strings.TrimSpace(path) == "" {
		return notifyFile{}, errors.New("notify env file path is empty")
	}
	f, err := os.Open(path)
	if err != nil {
		return notifyFile{}, fmt.Errorf("notify env file %s: cannot read", path)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return notifyFile{}, fmt.Errorf("notify env file %s: cannot read", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return notifyFile{}, fmt.Errorf("notify env file %s: cannot read", path)
	}
	kv := parseNotifyEnv(raw)
	out := notifyFile{
		path:          path,
		perm:          info.Mode().Perm(),
		token:         strings.TrimSpace(kv["TELEGRAM_BOT_TOKEN"]),
		chatID:        strings.TrimSpace(kv["TELEGRAM_CHAT_ID"]),
		webhookURL:    strings.TrimSpace(kv["CLAWCTL_NOTIFY_WEBHOOK_URL"]),
		webhookSecret: strings.TrimSpace(kv["CLAWCTL_NOTIFY_WEBHOOK_SECRET"]),
	}
	if (out.token == "") != (out.chatID == "") {
		missing := "TELEGRAM_CHAT_ID"
		if out.token == "" {
			missing = "TELEGRAM_BOT_TOKEN"
		}
		return notifyFile{}, fmt.Errorf("notify env file %s: %s is missing (Telegram requires TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID together)", path, missing)
	}
	if out.webhookURL != "" {
		if err := validateWebhookURL(out.webhookURL); err != nil {
			return notifyFile{}, fmt.Errorf("notify env file %s: CLAWCTL_NOTIFY_WEBHOOK_URL %s", path, err.Error())
		}
	}
	return out, nil
}

// parseNotifyEnv reads KEY=VALUE lines without a shell: blank lines and
// # comments are ignored, a leading "export " is stripped, matching quotes
// around the value are stripped, and there is no variable expansion.
func parseNotifyEnv(raw []byte) map[string]string {
	text := string(raw)
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	out := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		out[key] = val
	}
	return out
}

func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return errors.New("must be an absolute https URL (http is allowed only for loopback or 100.64.0.0/10)")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		addr, err := netip.ParseAddr(u.Hostname())
		if err != nil {
			return errors.New("must be an absolute https URL (http is allowed only for loopback or 100.64.0.0/10)")
		}
		addr = addr.Unmap()
		if addr.IsLoopback() || webhookHTTPAllowedNet.Contains(addr) {
			return nil
		}
		return errors.New("must be an absolute https URL (http is allowed only for loopback or 100.64.0.0/10)")
	default:
		return errors.New("must be an absolute https URL (http is allowed only for loopback or 100.64.0.0/10)")
	}
}

func webhookSchemeHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (n builtinNotifier) Notify(ctx context.Context, kind, body string) (string, error) {
	cfg, err := loadNotifyEnvFile(n.path)
	if err != nil || !cfg.hasChannel() {
		channel := n.channels
		if channel == "" {
			channel = "builtin"
		}
		return channel, fmt.Errorf("notify env file %s: unreadable or invalid", n.path)
	}
	if strings.TrimSpace(body) == "" {
		return cfg.channelName(), errors.New("empty notification body")
	}
	var errs []string
	if cfg.telegram() {
		if err := n.sendTelegram(ctx, cfg, body); err != nil {
			errs = append(errs, "telegram: "+sanitizeNotifyError(err, cfg.secrets()).Error())
		}
	}
	if cfg.webhook() {
		if err := n.sendWebhook(ctx, cfg, kind, body); err != nil {
			errs = append(errs, "webhook: "+sanitizeNotifyError(err, cfg.secrets()).Error())
		}
	}
	channel := cfg.channelName()
	if len(errs) > 0 {
		return channel, errors.New(strings.Join(errs, "; "))
	}
	return channel, nil
}

func (n builtinNotifier) sendTelegram(ctx context.Context, cfg notifyFile, body string) error {
	text := truncateTelegram(body)
	form := url.Values{}
	form.Set("chat_id", cfg.chatID)
	form.Set("text", text)
	form.Set("disable_web_page_preview", "true")
	_, desc, err := telegramAPI(ctx, n.telegramAPI(), cfg.token, "sendMessage", form, cfg.secrets())
	if err != nil {
		return err
	}
	if desc != "" {
		return errors.New(desc)
	}
	return nil
}

func truncateTelegram(body string) string {
	if utf8.RuneCountInString(body) <= telegramMaxRunes {
		return body
	}
	runes := []rune(body)
	return string(runes[:telegramMaxRunes]) + telegramTruncateSuffix
}

type telegramAPIResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

func telegramAPI(ctx context.Context, apiBase, token, method string, form url.Values, secrets []string) (telegramAPIResponse, string, error) {
	var parsed telegramAPIResponse
	endpoint := strings.TrimRight(apiBase, "/") + "/bot" + token + "/" + method
	reqCtx, cancel := context.WithTimeout(ctx, notifyRequestTimeout)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, body)
	if err != nil {
		return parsed, "", sanitizeNotifyError(err, secrets)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("User-Agent", "clawctl-hub")
	resp, err := newNotifyHTTPClient().Do(req)
	if err != nil {
		return parsed, "", sanitizeNotifyError(err, secrets)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, notifyResponseLimit))
	_ = json.Unmarshal(raw, &parsed)
	if resp.StatusCode == http.StatusOK && parsed.OK {
		return parsed, "", nil
	}
	if parsed.Description != "" {
		return parsed, parsed.Description, nil
	}
	if resp.StatusCode != http.StatusOK {
		return parsed, "", fmt.Errorf("Telegram HTTP %d", resp.StatusCode)
	}
	return parsed, "", errors.New("Telegram request failed")
}

type webhookPayload struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	SentAt string `json:"sent_at"`
}

func (n builtinNotifier) sendWebhook(ctx context.Context, cfg notifyFile, kind, body string) error {
	payload, err := json.Marshal(webhookPayload{
		Kind:   kind,
		Text:   body,
		SentAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, notifyRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.webhookURL, bytes.NewReader(payload))
	if err != nil {
		return sanitizeNotifyError(err, cfg.secrets())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "clawctl-hub")
	if cfg.webhookSecret != "" {
		mac := hmac.New(sha256.New, []byte(cfg.webhookSecret))
		_, _ = mac.Write(payload)
		req.Header.Set("X-Clawctl-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := newNotifyHTTPClient().Do(req)
	if err != nil {
		return sanitizeNotifyError(err, cfg.secrets())
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, notifyResponseLimit))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// sanitizeNotifyError unwraps *url.Error (which embeds the full URL, often
// including a bot token or webhook secret) down to the operation and inner
// error, then strips any remaining secret substrings.
func sanitizeNotifyError(err error, secrets []string) error {
	if err == nil {
		return nil
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if uerr.Err != nil {
			err = fmt.Errorf("%s: %v", uerr.Op, uerr.Err)
		} else {
			err = errors.New(uerr.Op)
		}
	}
	msg := err.Error()
	redacted := msg
	for _, s := range secrets {
		if s == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, s, "[redacted]")
	}
	if redacted == msg {
		return err
	}
	return errors.New(redacted)
}

func (n builtinNotifier) check(ctx context.Context, out io.Writer) error {
	cfg, err := loadNotifyEnvFile(n.path)
	if err != nil {
		return err
	}
	if cfg.perm&^0o600 != 0 {
		log.Printf("WARN notify env file %s mode %04o is broader than 0600", n.path, cfg.perm)
	}
	if !cfg.hasChannel() {
		return fmt.Errorf("notify-check: no channels in %s", n.path)
	}
	failed := false
	if cfg.telegram() {
		if err := n.checkTelegram(ctx, cfg, out); err != nil {
			failed = true
		}
	}
	if cfg.webhook() {
		host := webhookSchemeHost(cfg.webhookURL)
		fmt.Fprintf(out, "notify-check: webhook %s (config only; not posted)\n", host)
	}
	if failed {
		return errNotifyCheckFailed
	}
	return nil
}

func (n builtinNotifier) checkTelegram(ctx context.Context, cfg notifyFile, out io.Writer) error {
	failed := false
	me, desc, err := telegramAPI(ctx, n.telegramAPI(), cfg.token, "getMe", nil, cfg.secrets())
	if err != nil {
		fmt.Fprintln(out, "notify-check: cannot reach Telegram (network or DNS)")
		failed = true
	} else if desc != "" {
		fmt.Fprintf(out, "notify-check: token rejected: %s\n", desc)
		failed = true
	} else {
		username := telegramUsername(me.Result)
		if username != "" {
			fmt.Fprintf(out, "notify-check: token is valid → @%s\n", username)
		} else {
			fmt.Fprintln(out, "notify-check: token is valid")
		}
	}

	form := url.Values{}
	form.Set("chat_id", cfg.chatID)
	chat, desc, err := telegramAPI(ctx, n.telegramAPI(), cfg.token, "getChat", form, cfg.secrets())
	if err != nil {
		fmt.Fprintln(out, "notify-check: cannot reach Telegram (network or DNS)")
		failed = true
	} else if desc != "" {
		fmt.Fprintf(out, "notify-check: cannot send to this chat: %s\n", desc)
		failed = true
	} else {
		who := telegramChatName(chat.Result)
		if who == "" {
			who = "(no title)"
		}
		fmt.Fprintf(out, "notify-check: can send to %q\n", who)
	}
	if failed {
		return errors.New("notify-check: Telegram check failed")
	}
	return nil
}

func telegramUsername(result json.RawMessage) string {
	var me struct {
		Username string `json:"username"`
	}
	_ = json.Unmarshal(result, &me)
	return me.Username
}

func telegramChatName(result json.RawMessage) string {
	var chat struct {
		Title     string `json:"title"`
		FirstName string `json:"first_name"`
	}
	_ = json.Unmarshal(result, &chat)
	if chat.Title != "" {
		return chat.Title
	}
	return chat.FirstName
}
