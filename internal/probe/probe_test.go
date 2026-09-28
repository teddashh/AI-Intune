package probe

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// 這個檔案裡的 fixture 全部是自己造的。⚠ 單元測試不可以相依真機狀態 ——
// 真機比對走檔尾的 TestLiveCollect（預設 skip）。

// ---------------------------------------------------------------- JWT

func TestScanProcessesClassifiesProcVisibility(t *testing.T) {
	procs, status := scanProcesses()
	if status == model.ProcessScanUnavailable {
		t.Fatal("測試機應可讀取 /proc，scanProcesses 卻回報 unavailable")
	}
	if status == model.ProcessScanComplete && len(procs) < minPlausibleProcs {
		t.Errorf("只有 %d 個可見 process，卻回報 complete", len(procs))
	}
	if len(procs) < minPlausibleProcs && status != model.ProcessScanRestricted {
		t.Errorf("只有 %d 個可見 process，status = %q, want restricted", len(procs), status)
	}
}

func TestScanProcessesInClassifiesProcVisibility(t *testing.T) {
	writeProc := func(t *testing.T, root string, pid int, cmdline []byte, mode os.FileMode) {
		t.Helper()
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), cmdline, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "comm"), []byte("fake\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/usr/bin/fake", filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	writeReadable := func(t *testing.T, root string, count int) {
		t.Helper()
		for i := 0; i < count; i++ {
			writeProc(t, root, 100000+i, []byte("fake\x00--pid\x00"), 0o644)
		}
	}

	t.Run("permission denied is restricted despite enough readable processes", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 可讀 mode 0000 的檔案，無法製造 EACCES")
		}
		root := t.TempDir()
		writeReadable(t, root, minPlausibleProcs)
		writeProc(t, root, 200000, []byte("hidden\x00"), 0o000)

		procs, status := scanProcessesIn(root)
		if len(procs) < minPlausibleProcs {
			t.Fatalf("測試 fixture 只產生 %d 個可見 process，沒有通過數量門檻", len(procs))
		}
		if status != model.ProcessScanRestricted {
			t.Errorf("status = %q, want %q", status, model.ProcessScanRestricted)
		}
	})

	t.Run("enough readable processes is complete", func(t *testing.T) {
		root := t.TempDir()
		writeReadable(t, root, minPlausibleProcs)
		_, status := scanProcessesIn(root)
		if status != model.ProcessScanComplete {
			t.Errorf("status = %q, want %q", status, model.ProcessScanComplete)
		}
	})

	t.Run("too few readable processes is restricted", func(t *testing.T) {
		root := t.TempDir()
		writeReadable(t, root, minPlausibleProcs-1)
		_, status := scanProcessesIn(root)
		if status != model.ProcessScanRestricted {
			t.Errorf("status = %q, want %q", status, model.ProcessScanRestricted)
		}
	})

	t.Run("vanished process does not restrict visibility", func(t *testing.T) {
		root := t.TempDir()
		writeReadable(t, root, minPlausibleProcs)
		if err := os.Mkdir(filepath.Join(root, "200000"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, status := scanProcessesIn(root)
		if status != model.ProcessScanComplete {
			t.Errorf("status = %q, want %q", status, model.ProcessScanComplete)
		}
	})

	t.Run("kernel thread does not restrict visibility", func(t *testing.T) {
		root := t.TempDir()
		writeReadable(t, root, minPlausibleProcs)
		writeProc(t, root, 200000, nil, 0o644)
		_, status := scanProcessesIn(root)
		if status != model.ProcessScanComplete {
			t.Errorf("status = %q, want %q", status, model.ProcessScanComplete)
		}
	})

	t.Run("missing root is unavailable", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "missing")
		_, status := scanProcessesIn(root)
		if status != model.ProcessScanUnavailable {
			t.Errorf("status = %q, want %q", status, model.ProcessScanUnavailable)
		}
	})
}

func TestProcessObservationsSharesProcessScan(t *testing.T) {
	for _, tt := range []struct {
		name        string
		processScan string
		wantReason  string
	}{
		{
			name:        "restricted",
			processScan: model.ProcessScanRestricted,
			wantReason:  "只掃得到 0 個 process，這台的 /proc 視野被限制了，查不出 bat-server 是否在跑",
		},
		{
			name:        "unavailable",
			processScan: model.ProcessScanUnavailable,
			wantReason:  "讀不到 /proc，這台的 process 偵測整個是關的",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tools, bat := processObservations(context.Background(), nil, tt.processScan)
			for _, tool := range tools {
				if tool.ProcessScan != tt.processScan {
					t.Errorf("%s ProcessScan = %q，想要 %q", tool.Name, tool.ProcessScan, tt.processScan)
				}
			}
			if bat.Reason != tt.wantReason {
				t.Errorf("BAT Reason = %q，想要 %q", bat.Reason, tt.wantReason)
			}
			if strings.Contains(bat.Reason, "沒有一個是") {
				t.Errorf("不完整掃描不能宣告 BAT 缺席：%q", bat.Reason)
			}
		})
	}
}

// makeJWT 造一張只有 exp 有意義的假 JWT。簽章是垃圾，因為我們本來就不驗簽。
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc(payload) + ".not-a-real-signature"
}

func TestJWTExp(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  *time.Time
	}{
		{"seconds", makeJWT(t, map[string]any{"exp": 1788415073}), timePtr(time.Unix(1788415073, 0).UTC())},
		{"float exp", makeJWT(t, map[string]any{"exp": 1788415073.0}), timePtr(time.Unix(1788415073, 0).UTC())},
		{"no exp claim", makeJWT(t, map[string]any{"sub": "x"}), nil},
		{"exp null", makeJWT(t, map[string]any{"exp": nil}), nil},
		{"not a jwt", "hello", nil},
		{"two segments only", "aGVhZGVy." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1788415073}`)), timePtr(time.Unix(1788415073, 0).UTC())},
		{"bad base64", "a.!!!!.c", nil},
		{"payload not json", "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c", nil},
		{"empty", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := jwtExp(tc.token)
			assertTimeEq(t, got, tc.want)
		})
	}
}

// TestCodexUsesAccessTokenNotIDToken 釘住整個 probe 最容易被「順手清理」壞掉的地方。
//
// ⚠ id_token 壽命 1 小時、實測平常就是過期的。如果有人把 codexCred 改成讀
// id_token，整個機隊會每一台都紅。這個測試就是為了讓那次改動在 CI 上爆炸。
func TestCodexUsesAccessTokenNotIDToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	expiredIDToken := makeJWT(t, map[string]any{"exp": now.Add(-10 * 24 * time.Hour).Unix()})
	liveAccessToken := makeJWT(t, map[string]any{"exp": now.Add(30 * 24 * time.Hour).Unix()})

	home := t.TempDir()
	writeJSON(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":     expiredIDToken,
			"access_token": liveAccessToken,
			"account_id":   "acct-1",
		},
		"last_refresh": "2026-08-24T05:57:53.867579385Z",
	})
	writeJSON(t, filepath.Join(home, ".codex", "codex-accounts.json"), map[string]any{
		"activeAccountId": "acct-b",
		"accounts": map[string]any{
			"acct-a": map[string]any{"email": "a@example.com"},
			"acct-b": map[string]any{"email": "b@example.com"},
		},
	})

	c := codexCred(home, now)
	if c.Status != model.CredConfigured {
		t.Fatalf("status = %q, want %q (probably read id_token instead of access_token)",
			c.Status, model.CredConfigured)
	}
	wantExp := now.Add(30 * 24 * time.Hour)
	if c.ExpiresAt == nil || !c.ExpiresAt.Equal(wantExp) {
		t.Errorf("expires_at = %v, want %v", c.ExpiresAt, wantExp)
	}
	if c.LastRefresh == nil || c.LastRefresh.Format(time.RFC3339) != "2026-08-24T05:57:53Z" {
		t.Errorf("last_refresh = %v, want 2026-08-24T05:57:53Z", c.LastRefresh)
	}
	// ⚠ BAT 熱抽換 auth.json，活躍帳號只有 codex-accounts.json 知道。
	if c.ActiveAccountID != "acct-b" {
		t.Errorf("active_account_id = %q, want acct-b", c.ActiveAccountID)
	}
	if c.AccountCount != 2 {
		t.Errorf("account_count = %d, want 2", c.AccountCount)
	}
	assertNoSecrets(t, c)
}

// ---------------------------------------------------------------- grok

func TestGrokExpiry(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want *time.Time
	}{{
		// 實測 samplehub1 的形狀：頂層 key 帶動態 uuid，expires_at 是 RFC3339 字串。
		name: "dynamic uuid key, rfc3339 string",
		doc:  `{"https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828":{"key":"secret","expires_at":"2026-09-03T04:18:35.306566984Z"}}`,
		want: timePtr(time.Date(2026, 9, 3, 4, 18, 35, 306566984, time.UTC)),
	}, {
		name: "different uuid still found",
		doc:  `{"https://auth.x.ai::00000000-0000-0000-0000-000000000000":{"expires_at":"2026-01-02T03:04:05Z"}}`,
		want: timePtr(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)),
	}, {
		name: "epoch seconds",
		doc:  `{"https://auth.x.ai::x":{"expires_at":1788415073}}`,
		want: timePtr(time.Unix(1788415073, 0).UTC()),
	}, {
		name: "epoch millis",
		doc:  `{"https://auth.x.ai::x":{"expires_at":1788415073000}}`,
		want: timePtr(time.Unix(1788415073, 0).UTC()),
	}, {
		name: "array of sessions takes the latest",
		doc:  `{"https://auth.x.ai::x":[{"expires_at":"2026-01-01T00:00:00Z"},{"expires_at":"2027-01-01T00:00:00Z"}]}`,
		want: timePtr(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)),
	}, {
		name: "several dynamic keys takes the latest",
		doc: `{"https://auth.x.ai::a":{"expires_at":"2026-01-01T00:00:00Z"},` +
			`"https://auth.x.ai::b":{"expires_at":"2026-06-01T00:00:00Z"}}`,
		want: timePtr(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)),
	}, {
		name: "foreign top-level keys ignored",
		doc:  `{"version":1,"https://example.com::x":{"expires_at":"2099-01-01T00:00:00Z"}}`,
		want: nil,
	}, {
		name: "no expires_at anywhere",
		doc:  `{"https://auth.x.ai::x":{"key":"secret","refresh_token":"r"}}`,
		want: nil,
	}, {
		name: "empty object",
		doc:  `{}`,
		want: nil,
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := grokExpiry([]byte(tc.doc))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertTimeEq(t, got, tc.want)
		})
	}
}

func TestGrokExpiryBadJSON(t *testing.T) {
	if _, err := grokExpiry([]byte("{not json")); err == nil {
		t.Fatal("want error for malformed json, got nil")
	}
}

// ---------------------------------------------------------------- 分類

func TestClassifyCred(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		expires *time.Time
		want    model.CredStatus
	}{
		{"no expiry field at all", nil, model.CredUnknown},
		{"long past", timePtr(now.Add(-90 * 24 * time.Hour)), model.CredExpired},
		{"one second past", timePtr(now.Add(-time.Second)), model.CredExpired},
		{"exactly now counts as expired", timePtr(now), model.CredExpired},

		// ⚠ 以下這些「快到期了」的案例全部必須是 configured，不是 expires_soon。
		//
		// 這些 access token 都會自動續期，8 小時的名目壽命是常態。
		// 只要還沒過期，就代表續期迴圈還在運作，人不需要知道。
		// 詳細理由見 classifyCred 的註解 —— 這條規則是在真機上跑出來的，
		// 改回「剩餘 < 7 天就報警」會讓每台機器永遠是黃燈。
		{"1 second left is still fine", timePtr(now.Add(time.Second)), model.CredConfigured},
		{"claude-style 8h token", timePtr(now.Add(8 * time.Hour)), model.CredConfigured},
		{"grok-style 6h token", timePtr(now.Add(6 * time.Hour)), model.CredConfigured},
		{"codex-style 10d token", timePtr(now.Add(10 * 24 * time.Hour)), model.CredConfigured},
		{"30 days left", timePtr(now.Add(30 * 24 * time.Hour)), model.CredConfigured},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCred(now, tc.expires); got != tc.want {
				t.Errorf("classifyCred = %q, want %q", got, tc.want)
			}
		})
	}
}

// agent 永遠不該從單一次讀取就喊 expires_soon。
//
// 那需要「這個檔案已經好幾輪沒被更新」這種跨時間的觀察，而 agent 只看得到
// 當下這張快照。判斷放 Hub —— 跟「被踢掉」的偵測是同一個道理。
func TestAgentNeverEmitsExpiresSoon(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for h := -100; h < 24*400; h += 7 {
		exp := now.Add(time.Duration(h) * time.Hour)
		if got := classifyCred(now, &exp); got == model.CredExpiresSoon {
			t.Fatalf("classifyCred 在剩餘 %dh 時回傳了 expires_soon —— "+
				"agent 不該從單一快照做這個判斷", h)
		}
	}
}

// ---------------------------------------------------------------- 版本矛盾

func TestSourcesDisagree(t *testing.T) {
	tests := []struct {
		name     string
		versions []string
		want     bool
	}{
		{"agree", []string{"2.1.205", "2.1.205"}, false},
		// 實測 samplehub1 的 grok：自己說 1.0.3，package.json 說 1.0.13。
		{"grok lies about itself", []string{"1.0.3", "1.0.13"}, true},
		{"only one source", []string{"0.144.1", ""}, false},
		{"only package.json", []string{"", "1.0.13"}, false},
		{"no source at all", []string{"", ""}, false},
		{"three sources one odd", []string{"1.0.3", "1.0.3", "0.2.118"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourcesDisagree(tc.versions...); got != tc.want {
				t.Errorf("sourcesDisagree(%q) = %v, want %v", tc.versions, got, tc.want)
			}
		})
	}
}

func TestVersionRegex(t *testing.T) {
	tests := []struct{ raw, want string }{
		{"OpenClaw 2026.6.1 (2e08f0f)", "2026.6.1"},
		{"2.1.205 (Claude Code)", "2.1.205"},
		{"codex-cli 0.144.0", "0.144.0"},
		{"grok 1.0.3 (1a29d5bc12)", "1.0.3"},
		{"no version here", ""},
	}
	for _, tc := range tests {
		if got := versionRe.FindString(tc.raw); got != tc.want {
			t.Errorf("versionRe on %q = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestNodeModulesRegex(t *testing.T) {
	tests := []struct{ realpath, want string }{
		{"/usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe",
			"/usr/local/lib/node_modules/@anthropic-ai/claude-code"},
		{"/usr/lib/node_modules/openclaw/openclaw.mjs", "/usr/lib/node_modules/openclaw"},
		{"/usr/lib/node_modules/@xai-official/grok/bin/grok", "/usr/lib/node_modules/@xai-official/grok"},
		// standalone 安裝沒有 node_modules —— 不可以硬湊出一個第二來源。
		{"/home/example-user-c/.codex/packages/standalone/releases/0.144.1-aarch64-unknown-linux-musl/bin/codex", ""},
		{"/home/example-user-b/.local/bin/agy", ""},
	}
	for _, tc := range tests {
		// 直接呼叫 production 用的那支 —— 測試自己重寫一遍正是上一版藏 bug 的地方。
		if got := packageDirFor(tc.realpath); got != tc.want {
			t.Errorf("packageDirFor(%q) = %q, want %q", tc.realpath, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------- 時間解析

func TestFlexTime(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want *time.Time
	}{
		{"epoch millis", `1788415575541`, timePtr(time.UnixMilli(1788415575541).UTC())},
		{"epoch seconds", `1788415073`, timePtr(time.Unix(1788415073, 0).UTC())},
		{"rfc3339 nano string", `"2026-09-03T04:18:35.306566984Z"`,
			timePtr(time.Date(2026, 9, 3, 4, 18, 35, 306566984, time.UTC))},
		{"rfc3339 with offset", `"2026-09-03T00:18:35-04:00"`,
			timePtr(time.Date(2026, 9, 3, 4, 18, 35, 0, time.UTC))},
		{"numeric string", `"1788415073"`, timePtr(time.Unix(1788415073, 0).UTC())},
		{"null", `null`, nil},
		{"missing field", ``, nil},
		{"zero", `0`, nil},
		{"garbage string", `"soon"`, nil},
		{"object", `{"a":1}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertTimeEq(t, flexTime(json.RawMessage(tc.raw)), tc.want)
		})
	}
}

func TestParseSystemdTime(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want *time.Time
	}{
		{"unix format", "@1788324026", timePtr(time.Unix(1788324026, 0).UTC())},
		{"unix with micros", "@1788324026.500000", timePtr(time.Unix(1788324026, 0).UTC().Add(500000 * time.Microsecond))},
		{"human format utc", "Wed 2026-09-02 00:40:26 UTC", timePtr(time.Date(2026, 9, 2, 0, 40, 26, 0, time.UTC))},
		{"never active", "", nil},
		{"n/a", "n/a", nil},
		{"garbage", "some day", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertTimeEq(t, parseSystemdTime(tc.in), tc.want)
		})
	}
}

func TestTruncRunes(t *testing.T) {
	// summary 實測含中文，用 byte 截會切出半個字。
	if got := truncRunes("機器隊列健康狀態", 4); got != "機器隊列" {
		t.Errorf("truncRunes = %q, want 機器隊列", got)
	}
	if got := truncRunes("abc", 10); got != "abc" {
		t.Errorf("truncRunes = %q, want abc", got)
	}
	if got := truncRunes("abc", 0); got != "" {
		t.Errorf("truncRunes = %q, want empty", got)
	}
}

// ---------------------------------------------------------------- 憑證整體

func TestCredentialsSyntheticHome(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	home := t.TempDir()

	writeJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": "SECRET-ACCESS",
			"expiresAt":   now.Add(8 * time.Hour).UnixMilli(),
		},
	})
	writeJSON(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{
		"tokens": map[string]any{
			"access_token": makeJWT(t, map[string]any{"exp": now.Add(-time.Hour).Unix()}),
		},
	})
	writeJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		"https://auth.x.ai::9f0d1b8e-0000-4000-8000-000000000000": map[string]any{
			"key":        "SECRET-KEY",
			"expires_at": now.Add(90 * 24 * time.Hour).Format(time.RFC3339Nano),
		},
	})
	// .gemini 不建 → absent；.openclaw 建目錄 → unknown。
	if err := os.MkdirAll(filepath.Join(home, ".openclaw"), 0o755); err != nil {
		t.Fatal(err)
	}

	creds := credentials(home, now)
	byProvider := map[string]model.Credential{}
	for _, c := range creds {
		byProvider[c.Provider] = c
		assertNoSecrets(t, c)
	}
	if len(creds) != 5 {
		t.Fatalf("got %d credentials, want 5 (名冊是分母，沒裝的也要出現)", len(creds))
	}

	want := map[string]model.CredStatus{
		// ⚠ 8 小時後到期仍然是 configured：那是 access token 的正常壽命，
		// 續期迴圈還活著。只有「已經過去」才是人要處理的事。
		"claude":   model.CredConfigured,
		"codex":    model.CredExpired, // JWT exp 已經過去一小時
		"grok":     model.CredConfigured,
		"gemini":   model.CredAbsent, // 沒有 ~/.gemini
		"openclaw": model.CredUnknown,
	}
	for provider, wantStatus := range want {
		if got := byProvider[provider].Status; got != wantStatus {
			t.Errorf("%s status = %q, want %q", provider, got, wantStatus)
		}
	}
	// ⚠ openclaw 沒有任何過期欄位，必須誠實說出來，不可以裝成 configured。
	if byProvider["openclaw"].Note == "" {
		t.Error("openclaw unknown 必須附原因")
	}
	if byProvider["gemini"].ExpiresAt != nil {
		t.Error("absent 的 provider 不該有 expires_at")
	}
	if byProvider["claude"].FileMTime == nil {
		t.Error("claude 應該要有 file_mtime（三元組的一隻腳）")
	}
}

func TestCredentialsUnreadableIsNotAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 讀得到任何東西，測不出 permission_denied")
	}
	now := time.Now().UTC()
	home := t.TempDir()
	path := filepath.Join(home, ".claude", ".credentials.json")
	writeJSON(t, path, map[string]any{"claudeAiOauth": map[string]any{"expiresAt": 1}})
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	c := claudeCred(home, now)
	// ⚠ 「讀不動」不等於「沒裝」。混在一起會讓權限壞掉的機器看起來很乾淨。
	if c.Status != model.CredUnknown {
		t.Errorf("status = %q, want unknown", c.Status)
	}
	if c.Note != "permission_denied" {
		t.Errorf("note = %q, want permission_denied", c.Note)
	}
}

func TestCredentialsBadJSON(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := claudeCred(home, time.Now())
	if c.Status != model.CredUnknown || c.Note == "" {
		t.Errorf("bad json → %q / %q, want unknown + reason", c.Status, c.Note)
	}
}

// TestACredentialErrorKeepsItsOriginalTextApartFromItsCategory 釘的是 model.go:866
// 「原文，不分類。地基二」與 probe.go:1297「Note 帶歸類詞、LastError 帶原文」這一對。
//
// ⚠ 實測四臂（全樹 go test ./... -count=1，下刀前）：
//   - credUnparseable 的 LastError: err.Error() 改成 reasonFor(err)：全樹全綠。
//   - credUnreadable 的 LastError: err.Error() 改成 reasonFor(err)：全樹全綠。
//   - Note 改成帶原文（Note: err.Error()）：全樹全綠。
//   - 檔案不在的那一支也寫 LastError：全樹全綠。
//
// 四臂全空。
// 下刀後同四臂重量，每一臂都只有這一支紅（others=[]），而且各自對到不同的子測試：
//   - credUnparseable 改歸類詞 → 「看不懂的憑證不把歸類詞黏進原文」紅。
//   - credUnreadable 改歸類詞 → 「讀不動的憑證講得出是哪一個路徑」紅。
//   - Note 改帶原文 → 「看不懂的憑證不把歸類詞黏進原文」紅。
//   - 檔案不在也寫 LastError → **這一支維持綠**。那是另一條規矩
//     （probe.go:1287「LastError 刻意留空」），這一刀沒有涵蓋它。
//
// 既有的 TestCredentialsUnreadableIsNotAbsent 守的是 Note == "permission_denied"，
// 也就是歸類那一半；原文那一半在下這一刀之前一個看守者都沒有。
//
// 看得見：internal/web/templates/machine.html:402 把 LastError 畫成一格
// white-space:pre-wrap 的 amber 區塊，CLI 走 cmd/clawctl-hub/machinescmd.go:661 的
// LAST_ERROR:。兩個表面上「permission_denied」跟
// 「open /home/…/.credentials.json: permission denied」是完全不同的兩句話：
// 前者不能拿去做下一步，後者可以。
func TestACredentialErrorKeepsItsOriginalTextApartFromItsCategory(t *testing.T) {
	t.Run("讀不動的憑證講得出是哪一個路徑", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 讀得到任何東西，測不出 permission_denied")
		}
		home := t.TempDir()
		path := filepath.Join(home, ".claude", ".credentials.json")
		writeJSON(t, path, map[string]any{"claudeAiOauth": map[string]any{"expiresAt": 1}})
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}

		c := claudeCred(home, time.Now().UTC())
		if !strings.Contains(c.LastError, path) {
			t.Errorf("last_error = %q；歸類詞只說得出 permission_denied，人接下來要做的第一件事是 denied 在哪一個路徑，那個答案只在原文裡", c.LastError)
		}
		if c.LastError == c.Note {
			t.Errorf("last_error 和 note 都是 %q；兩欄變成同一個字串的時候，原文那一欄就不存在了，但畫面上還有兩格，看起來像有兩份證據", c.LastError)
		}
	})

	t.Run("看不懂的憑證不把歸類詞黏進原文", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, ".claude", ".credentials.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}

		c := claudeCred(home, time.Now().UTC())
		if !strings.Contains(c.Note, "bad_json") {
			t.Errorf("note = %q, want category bad_json", c.Note)
		}
		if strings.Contains(c.LastError, "bad_json") {
			t.Errorf("last_error = %q；原文一旦被黏上歸類前綴，就變成一個看起來可以拿去 strings.HasPrefix 判斷的字串，而地基二不准任何人那樣做", c.LastError)
		}
		if c.LastError == "" {
			t.Error("last_error = empty, want original error text")
		}
	})
}

// TestAProviderThatIsNotInstalledCarriesNoErrorLine 釘的是 probe.go:1287
// 「LastError 刻意留空」。
//
// ⚠ 實測（全樹 go test ./... -count=1）：把 credUnreadable 的 ErrNotExist 那一支
// 改成連 LastError: err.Error() 一起寫出去，下刀前全樹全綠，一個看守者都沒有。
// 同一輪量的另外三臂（兩支 LastError 改歸類詞、Note 改帶原文）都被上一刀
// TestACredentialErrorKeepsItsOriginalTextApartFromItsCategory 接住了，
// 只有這一臂漏在外面——因為它不是「原文被換掉」，是「本來就不該有的那一行冒出來」。
//
// 下刀後四臂重量：
//   - 檔案不在也寫 LastError → 只有這一支紅（others=[]）。
//   - 兩支 LastError 改歸類詞 → 這一支**維持綠**，紅的是上一刀。兩刀互不相欠。
//   - 檔案不在改講成 unknown → 這一支紅（空轉守衛開火）加上
//     TestCredentialsSyntheticHome 紅。那一臂不是這一支的隔離證據。
//
// 既有的 TestCredentialsSyntheticHome 對 absent 只守了
// 「不該有 expires_at」（:533）。同一列旁邊的 LastError 沒有人守。
//
// 看得見：machine.html:402 是 {{with .LastError}} 包住的 amber pre-wrap 區塊，
// 空字串不渲染、非空就多一格。名冊是分母，沒裝的那幾家也會出現在那張表上
// （TestCredentialsSyntheticHome 自己寫著「名冊是分母，沒裝的也要出現」），
// 所以多出來的紅字會乘上機隊台數。
func TestAProviderThatIsNotInstalledCarriesNoErrorLine(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	home := t.TempDir()

	writeJSON(t, filepath.Join(home, ".claude", ".credentials.json"), map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": "SECRET-ACCESS",
			"expiresAt":   now.Add(8 * time.Hour).UnixMilli(),
		},
	})
	writeJSON(t, filepath.Join(home, ".codex", "auth.json"), map[string]any{
		"tokens": map[string]any{
			"access_token": makeJWT(t, map[string]any{"exp": now.Add(-time.Hour).Unix()}),
		},
	})
	writeJSON(t, filepath.Join(home, ".grok", "auth.json"), map[string]any{
		"https://auth.x.ai::9f0d1b8e-0000-4000-8000-000000000000": map[string]any{
			"key":        "SECRET-KEY",
			"expires_at": now.Add(90 * 24 * time.Hour).Format(time.RFC3339Nano),
		},
	})
	if err := os.MkdirAll(filepath.Join(home, ".openclaw"), 0o755); err != nil {
		t.Fatal(err)
	}

	creds := credentials(home, now)
	absent := 0
	for _, c := range creds {
		if c.Status != model.CredAbsent {
			continue
		}
		absent++
		if c.LastError != "" {
			t.Errorf("%s last_error = %q；沒裝這一家不是錯誤，它就是答案本身；寫進 LastError 的話，internal/web/templates/machine.html:402 會替每一台沒裝這一家的機器多畫一格 amber 區塊，而那一格底下沒有任何人要做的事", c.Provider, c.LastError)
		}
	}
	if absent == 0 {
		t.Fatal("fixture 裡沒有 absent 的 provider，這個斷言什麼都沒驗到")
	}
}

// ---------------------------------------------------------------- sqlite layout

// TestOpenClawDBBothLayouts 釘住「兩種 layout 同時活在機隊上」這件事。
// ⚠ 寫死任一條路徑，今天就會在 5 台裡錯 2 台。
func TestOpenClawDBBothLayouts(t *testing.T) {
	tests := []struct {
		name       string
		relPath    string
		wantLayout string
		withCron   bool
	}{
		{"consolidated 2026.6.x", "state/openclaw.sqlite", "consolidated", true},
		{"split 2026.5.x", "tasks/runs.sqlite", "split", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), ".openclaw")
			dbPath := filepath.Join(root, tc.relPath)
			makeOpenClawDB(t, dbPath, tc.withCron)

			d := openClawDB(context.Background(), root)
			if d.Reason != "" {
				t.Fatalf("reason = %q, want empty", d.Reason)
			}
			if !d.Present || d.Layout != tc.wantLayout || d.Path != dbPath {
				t.Fatalf("got present=%v layout=%q path=%q", d.Present, d.Layout, d.Path)
			}
			if d.TaskRunRows != 3 {
				t.Errorf("task_run_rows = %d, want 3", d.TaskRunRows)
			}
			if d.LastTaskEndedAt == nil || d.LastTaskEndedAt.UnixMilli() != 1788391852815 {
				t.Errorf("last_task_ended_at = %v, want 1788391852815ms", d.LastTaskEndedAt)
			}
			if d.TaskStatusCount["succeeded"] != 2 || d.TaskStatusCount["lost"] != 1 {
				t.Errorf("task_status_counts = %v", d.TaskStatusCount)
			}
			// 實測全機隊都是 0；有一天上游開始寫，這個數字要能動。
			if d.TerminalOutcomePopulated != 0 {
				t.Errorf("terminal_outcome_populated = %d, want 0", d.TerminalOutcomePopulated)
			}
			if tc.withCron {
				if d.CronRunLogRows != 2 || len(d.RecentSummaries) != 2 {
					t.Errorf("cron rows = %d, summaries = %d", d.CronRunLogRows, len(d.RecentSummaries))
				}
				// ⚠ status='ok' 只代表回合正常結束，不代表任務成功。原文要原封帶回。
				if len(d.RecentSummaries) > 0 && d.RecentSummaries[0].Status != "ok" {
					t.Errorf("recent summary status = %q, want ok", d.RecentSummaries[0].Status)
				}
			} else if d.CronRunLogRows != 0 || len(d.RecentSummaries) != 0 {
				// split layout 沒有 cron 表，不可以生出資料來。
				t.Errorf("split layout 不該有 cron 資料: rows=%d summaries=%d",
					d.CronRunLogRows, len(d.RecentSummaries))
			}
		})
	}
}

// TestOpenClawDBPrefersConsolidated：兩個都在時要挑新的那個。
// sampleagent3 上留著 *.migrated 的殘骸，順序錯了就會讀到屍體檔。
func TestOpenClawDBPrefersConsolidated(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "state", "openclaw.sqlite"), true)
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)
	if d := openClawDB(context.Background(), root); d.Layout != "consolidated" {
		t.Errorf("layout = %q, want consolidated", d.Layout)
	}
}

func TestOpenClawDBMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	d := openClawDB(context.Background(), root)
	if d.Present {
		t.Fatal("present = true, want false")
	}
	// 拿不到就要說為什麼。
	if d.Reason == "" {
		t.Error("missing db 必須附 reason")
	}
}

// TestOpenClawDBIgnoresMigratedCorpse：*.migrated 是遷移殘骸，不是資料庫。
func TestOpenClawDBIgnoresMigratedCorpse(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite.migrated"), false)
	if d := openClawDB(context.Background(), root); d.Present {
		t.Errorf("讀到了 %q —— *.migrated 是屍體檔，不可以當成資料庫", d.Path)
	}
}

// makeOpenClawDB 造一個跟上游同欄位的假資料庫。
func makeOpenClawDB(t *testing.T, path string, withCron bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE task_runs (task_id TEXT, status TEXT, created_at INTEGER,
		started_at INTEGER, ended_at INTEGER, terminal_summary TEXT, terminal_outcome TEXT)`)
	exec(`INSERT INTO task_runs VALUES ('t1','succeeded',1,2,1788391852815,'done',NULL)`)
	exec(`INSERT INTO task_runs VALUES ('t2','succeeded',1,2,1788391000000,'done',NULL)`)
	exec(`INSERT INTO task_runs VALUES ('t3','lost',1,2,NULL,NULL,NULL)`)
	if !withCron {
		return
	}
	exec(`CREATE TABLE cron_jobs (job_id TEXT, enabled INTEGER, last_run_at_ms INTEGER,
		last_run_status TEXT, consecutive_errors INTEGER)`)
	exec(`INSERT INTO cron_jobs VALUES ('j1',1,1788391800010,'ok',0)`)
	exec(`CREATE TABLE cron_run_logs (job_id TEXT, ts INTEGER, status TEXT, summary TEXT)`)
	exec(`INSERT INTO cron_run_logs VALUES ('j1',?,'ok','Heartbeat completed. Result: failure due to disk 97%.')`,
		time.Now().Add(-time.Hour).UnixMilli())
	exec(`INSERT INTO cron_run_logs VALUES ('j1',?,'error','boom')`,
		time.Now().Add(-2*time.Hour).UnixMilli())
}

// TestCronJobsReadStructuredCounts 守的是「只有最後執行時間、沒有工作分母」這個錯。
// enabled 的 NULL 不能算啟用，逾期也只能數已啟用的工作。
func TestCronJobsReadStructuredCounts(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	path := filepath.Join(root, "state", "openclaw.sqlite")
	makeOpenClawDB(t, path, false)
	base := time.Now().UTC().Truncate(time.Millisecond)
	execOpenClawSQL(t, path,
		`CREATE TABLE cron_jobs (enabled INTEGER, next_run_at_ms INTEGER)`,
		`INSERT INTO cron_jobs VALUES (1, `+fmt.Sprint(base.Add(-time.Hour).UnixMilli())+`)`,
		`INSERT INTO cron_jobs VALUES (1, `+fmt.Sprint(base.Add(2*time.Hour).UnixMilli())+`)`,
		`INSERT INTO cron_jobs VALUES (0, `+fmt.Sprint(base.Add(-3*time.Hour).UnixMilli())+`)`,
		`INSERT INTO cron_jobs VALUES (NULL, `+fmt.Sprint(base.Add(-4*time.Hour).UnixMilli())+`)`)

	d := openClawDB(context.Background(), root)
	if d.Reason != "" {
		t.Fatalf("讀取排程工作不該失敗：%q", d.Reason)
	}
	if !d.CronJobsTotalMeasured || d.CronJobsTotal != 4 {
		t.Errorf("排程工作總數 = %d，量到 = %v；預期是 4 且已量到",
			d.CronJobsTotal, d.CronJobsTotalMeasured)
	}
	if !d.CronJobsEnabledMeasured || d.CronJobsEnabled != 2 {
		t.Errorf("啟用數 = %d，量到 = %v；NULL 不能算啟用",
			d.CronJobsEnabled, d.CronJobsEnabledMeasured)
	}
	if !d.CronJobsScheduleMeasured || d.CronJobsOverdue != 1 {
		t.Errorf("逾期數 = %d，量到 = %v；預期只有 1 個已啟用工作逾期",
			d.CronJobsOverdue, d.CronJobsScheduleMeasured)
	}
	if d.NextCronRunAt == nil || d.NextCronRunAt.UnixMilli() != base.Add(-time.Hour).UnixMilli() {
		t.Errorf("最早下次執行時間 = %v，預期是 %v", d.NextCronRunAt, base.Add(-time.Hour))
	}
}

// TestCronJobsEmptyTableIsMeasuredZero 守的是「空表的 SUM 回 NULL，又被誤當成讀取失敗」這個錯。
// 表在且是空的，意思是真的 0 個工作，不是不知道。
func TestCronJobsEmptyTableIsMeasuredZero(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	path := filepath.Join(root, "state", "openclaw.sqlite")
	makeOpenClawDB(t, path, false)
	execOpenClawSQL(t, path, `CREATE TABLE cron_jobs (enabled INTEGER, next_run_at_ms INTEGER)`)

	d := openClawDB(context.Background(), root)
	if d.Reason != "" {
		t.Fatalf("空的 cron_jobs 不該讀取失敗：%q", d.Reason)
	}
	if !d.CronJobsTotalMeasured || !d.CronJobsEnabledMeasured || !d.CronJobsScheduleMeasured {
		t.Errorf("空表的三格數字都應明確量到：%+v", d)
	}
	if d.CronJobsTotal != 0 || d.CronJobsEnabled != 0 || d.CronJobsOverdue != 0 || d.NextCronRunAt != nil {
		t.Errorf("空表應是明確的 0 個工作：%+v", d)
	}
}

// TestCronJobsMissingTableStaysUnknown 守的是「沒有 cron_jobs 表被說成 0 個工作」這個錯。
// 舊版 split layout 沒有這張表是已知形狀，不能讓整份觀測失敗。
func TestCronJobsMissingTableStaysUnknown(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)

	d := openClawDB(context.Background(), root)
	if d.Reason != "" {
		t.Fatalf("沒有 cron_jobs 表不該報錯：%q", d.Reason)
	}
	if d.CronJobsTotalMeasured || d.CronJobsEnabledMeasured || d.CronJobsScheduleMeasured ||
		d.CronJobsTotal != 0 || d.CronJobsEnabled != 0 || d.CronJobsOverdue != 0 || d.NextCronRunAt != nil {
		t.Errorf("沒有 cron_jobs 表時應維持未量到的零值：%+v", d)
	}
}

// TestCronJobsMissingNextRunColumnKeepsCounts 守的是「舊 schema 少 next_run_at_ms，
// 連總數與啟用數都一起消失」這個錯。少欄位只能少收排程時間那一格。
func TestCronJobsMissingNextRunColumnKeepsCounts(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	path := filepath.Join(root, "state", "openclaw.sqlite")
	makeOpenClawDB(t, path, false)
	execOpenClawSQL(t, path,
		`CREATE TABLE cron_jobs (enabled INTEGER)`,
		`INSERT INTO cron_jobs VALUES (1)`,
		`INSERT INTO cron_jobs VALUES (0)`,
		`INSERT INTO cron_jobs VALUES (NULL)`)

	d := openClawDB(context.Background(), root)
	if d.Reason != "" {
		t.Fatalf("少 next_run_at_ms 不該讓觀測失敗：%q", d.Reason)
	}
	if !d.CronJobsTotalMeasured || d.CronJobsTotal != 3 ||
		!d.CronJobsEnabledMeasured || d.CronJobsEnabled != 1 {
		t.Errorf("舊 schema 的總數與啟用數沒有照常收到：%+v", d)
	}
	if d.CronJobsScheduleMeasured || d.CronJobsOverdue != 0 || d.NextCronRunAt != nil {
		t.Errorf("沒有 next_run_at_ms 卻生出排程時間材料：%+v", d)
	}
}

// TestSplitCronJobsReadStructuredCounts 守的是「split layout 只有執行紀錄，
// 卻讀不到宣告工作分母與下一次排程」這個錯；停用工作的過期時間不能混進來。
func TestSplitCronJobsReadStructuredCounts(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)
	base := time.Now().UTC().Truncate(time.Millisecond)
	writeJSON(t, filepath.Join(root, "cron", "jobs.json"), map[string]any{
		"version": 1,
		"jobs": []map[string]any{
			{"id": "past", "enabled": true},
			{"id": "future", "enabled": true},
			{"id": "disabled", "enabled": false},
		},
	})
	writeJSON(t, filepath.Join(root, "cron", "jobs-state.json"), map[string]any{
		"version": 1,
		"jobs": map[string]any{
			"past":     map[string]any{"state": map[string]any{"nextRunAtMs": base.Add(-time.Hour).UnixMilli()}},
			"future":   map[string]any{"state": map[string]any{"nextRunAtMs": base.Add(2 * time.Hour).UnixMilli()}},
			"disabled": map[string]any{"state": map[string]any{"nextRunAtMs": base.Add(-3 * time.Hour).UnixMilli()}},
		},
	})

	d := openClawDB(context.Background(), root)
	if !d.CronJobsTotalMeasured || !d.CronJobsEnabledMeasured || !d.CronJobsScheduleMeasured {
		t.Fatalf("三組 split 排程材料都應量到：%+v", d)
	}
	if d.CronJobsTotal != 3 || d.CronJobsEnabled != 2 || d.CronJobsOverdue != 1 {
		t.Errorf("排程數字 = 總數 %d、啟用 %d、逾期 %d；預期 3、2、1",
			d.CronJobsTotal, d.CronJobsEnabled, d.CronJobsOverdue)
	}
	if d.NextCronRunAt == nil || d.NextCronRunAt.UnixMilli() != base.Add(-time.Hour).UnixMilli() {
		t.Errorf("最早下次執行時間 = %v，預期是 %v", d.NextCronRunAt, base.Add(-time.Hour))
	}
}

// TestSplitCronJobsLastRunUsesAllJobs 守的是「只看啟用工作，
// 因此把一台其實有在跑的機器說成很久沒跑」這個錯。
func TestSplitCronJobsLastRunUsesAllJobs(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)
	base := time.Now().UTC().Truncate(time.Millisecond)
	writeJSON(t, filepath.Join(root, "cron", "jobs.json"), map[string]any{
		"jobs": []map[string]any{
			{"id": "older", "enabled": true},
			{"id": "newer", "enabled": true},
			{"id": "newest-disabled", "enabled": false},
		},
	})
	writeJSON(t, filepath.Join(root, "cron", "jobs-state.json"), map[string]any{
		"jobs": map[string]any{
			"older":           map[string]any{"state": map[string]any{"lastRunAtMs": base.Add(-3 * time.Hour).UnixMilli()}},
			"newer":           map[string]any{"state": map[string]any{"lastRunAtMs": base.Add(-2 * time.Hour).UnixMilli()}},
			"newest-disabled": map[string]any{"state": map[string]any{"lastRunAtMs": base.Add(-time.Hour).UnixMilli()}},
		},
	})

	d := openClawDB(context.Background(), root)
	if d.LastCronRunAt == nil || d.LastCronRunAt.UnixMilli() != base.Add(-time.Hour).UnixMilli() {
		t.Errorf("上次排程執行時間 = %v，預期是停用工作的最新時間 %v",
			d.LastCronRunAt, base.Add(-time.Hour))
	}
}

// TestSplitCronJobsWithoutStateLeavesLastRunUnknown 守的是「缺少狀態檔時，
// 用零值或現在時間假裝量到上次執行」這個錯。
func TestSplitCronJobsWithoutStateLeavesLastRunUnknown(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)
	writeJSON(t, filepath.Join(root, "cron", "jobs.json"), map[string]any{
		"jobs": []map[string]any{{"id": "job-1", "enabled": true}},
	})

	d := openClawDB(context.Background(), root)
	if d.LastCronRunAt != nil {
		t.Errorf("沒有 jobs-state.json 時上次排程執行應維持 nil，實際是 %v", d.LastCronRunAt)
	}
}

// TestSplitCronJobsMissingLastRunStillMeasuresNext 守的是「沒有 lastRunAtMs
// 就捏造上次執行時間，或連已有的 nextRunAtMs 也不量」這個錯。
func TestSplitCronJobsMissingLastRunStillMeasuresNext(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	writeJSON(t, filepath.Join(root, "cron", "jobs.json"), map[string]any{
		"jobs": []map[string]any{{"id": "job-1", "enabled": true}},
	})
	writeJSON(t, filepath.Join(root, "cron", "jobs-state.json"), map[string]any{
		"jobs": map[string]any{
			"job-1": map[string]any{"state": map[string]any{"nextRunAtMs": next.UnixMilli()}},
		},
	})

	d := openClawDB(context.Background(), root)
	if d.LastCronRunAt != nil {
		t.Errorf("沒有 lastRunAtMs 時上次排程執行應維持 nil，實際是 %v", d.LastCronRunAt)
	}
	if !d.CronJobsScheduleMeasured || d.NextCronRunAt == nil || d.NextCronRunAt.UnixMilli() != next.UnixMilli() {
		t.Errorf("缺 lastRunAtMs 不該影響下次排程觀測：%+v", d)
	}
}

// TestSplitCronJobsWithoutStateKeepsCounts 守的是「少 jobs-state.json 時，
// 已經從 jobs.json 量到的分母也一起消失」這個錯。
func TestSplitCronJobsWithoutStateKeepsCounts(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)
	writeJSON(t, filepath.Join(root, "cron", "jobs.json"), map[string]any{
		"jobs": []map[string]any{
			{"id": "enabled", "enabled": true},
			{"id": "disabled", "enabled": false},
		},
	})

	d := openClawDB(context.Background(), root)
	if !d.CronJobsTotalMeasured || d.CronJobsTotal != 2 ||
		!d.CronJobsEnabledMeasured || d.CronJobsEnabled != 1 {
		t.Errorf("只有 jobs.json 時仍應收到總數與啟用數：%+v", d)
	}
	if d.CronJobsScheduleMeasured || d.CronJobsOverdue != 0 || d.NextCronRunAt != nil {
		t.Errorf("沒有 jobs-state.json 卻生出排程時間材料：%+v", d)
	}
}

// TestSplitCronJobsMissingFilesKeepOtherObservation 守的是「兩個 cron JSON 都不在，
// 就把未知寫成 0，或連 runs.sqlite 裡其他可量事實也丟掉」這兩個錯。
func TestSplitCronJobsMissingFilesKeepOtherObservation(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)

	d := openClawDB(context.Background(), root)
	if d.CronJobsTotalMeasured || d.CronJobsEnabledMeasured || d.CronJobsScheduleMeasured {
		t.Errorf("兩個 cron JSON 都不在時應維持未量到：%+v", d)
	}
	if d.TaskRunRows != 3 || d.LastTaskEndedAt == nil {
		t.Errorf("cron JSON 缺檔不該吃掉其他觀測：%+v", d)
	}
}

// TestSplitCronJobsBadJSONStaysUnknown 守的是「壞掉的 jobs.json 讓 probe 失敗，
// 或在沒有可用工作名冊時硬算排程時間」這個錯。
func TestSplitCronJobsBadJSONStaysUnknown(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)
	dir := filepath.Join(root, "cron")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jobs.json"), []byte(`{"jobs":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "jobs-state.json"), map[string]any{"jobs": map[string]any{}})

	d := openClawDB(context.Background(), root)
	if d.Reason != "" {
		t.Fatalf("壞掉的 jobs.json 不該讓 probe 失敗：%q", d.Reason)
	}
	if d.CronJobsTotalMeasured || d.CronJobsEnabledMeasured || d.CronJobsScheduleMeasured {
		t.Errorf("壞掉的 jobs.json 應維持未量到：%+v", d)
	}
}

// TestSplitCronJobsArrayAndStateMapAreDifferentShapes 守的是「拿同一個型別解析
// jobs.json 的陣列與 jobs-state.json 的映射，結果其中一份永遠讀不到」這個錯。
func TestSplitCronJobsArrayAndStateMapAreDifferentShapes(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	writeJSON(t, filepath.Join(root, "cron", "jobs.json"), map[string]any{
		"jobs": []map[string]any{{"id": "job-1", "enabled": true}},
	})
	writeJSON(t, filepath.Join(root, "cron", "jobs-state.json"), map[string]any{
		"jobs": map[string]any{
			"job-1": map[string]any{"state": map[string]any{"nextRunAtMs": next.UnixMilli()}},
		},
	})

	d := openClawDB(context.Background(), root)
	if !d.CronJobsTotalMeasured || !d.CronJobsEnabledMeasured || !d.CronJobsScheduleMeasured ||
		d.CronJobsTotal != 1 || d.CronJobsEnabled != 1 || d.NextCronRunAt == nil ||
		d.NextCronRunAt.UnixMilli() != next.UnixMilli() {
		t.Errorf("陣列名冊與映射狀態沒有各自解析成功：%+v", d)
	}
}

// TestSplitCronJobsMissingOrNullEnabledIsDisabled 守的是「enabled 缺欄或為 null，
// 被 Go 零值或寬鬆轉型誤算成啟用」這個錯。
func TestSplitCronJobsMissingOrNullEnabledIsDisabled(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)
	writeJSON(t, filepath.Join(root, "cron", "jobs.json"), map[string]any{
		"jobs": []map[string]any{
			{"id": "missing"},
			{"id": "null", "enabled": nil},
			{"id": "enabled", "enabled": true},
		},
	})

	d := openClawDB(context.Background(), root)
	if !d.CronJobsTotalMeasured || d.CronJobsTotal != 3 ||
		!d.CronJobsEnabledMeasured || d.CronJobsEnabled != 1 {
		t.Errorf("enabled 缺欄或為 null 時不應算啟用：%+v", d)
	}
}

// execOpenClawSQL 在測試資料庫執行已知的 SQL，一句失敗就停。
func execOpenClawSQL(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("開啟測試資料庫失敗：%v", err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("建立測試資料失敗：%v", err)
		}
	}
}

// ---------------------------------------------------------------- 占用帳本

// TestOccupancyNeedsProviderEvidence
//
// ⚠ 帳本的硬規則：沒有 provider 的回合**不寫**，但要**數進去**。
//
// SPEC §4.4 說「沒證據不准寫 profile_id」。實測缺 provider 的多半正好是
// 失敗的回合（samplehub1 3315/4035、sampleagent2 2793/2942）—— 也就是最想知道
// 「當時用的是哪張票」的那些。一本安靜地丟掉三成資料的帳，
// 看起來會跟一本完整的帳一模一樣。
func TestOccupancyNeedsProviderEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	path := filepath.Join(root, "state", "openclaw.sqlite")
	makeOpenClawDB(t, path, true)
	addProviderColumns(t, path, []cronRow{
		{"j1", -1 * time.Hour, "ok", "openai", "gpt-5.5", "agent:cnode-network:cron:j1:run:r1", "", 20901},
		{"j2", -2 * time.Hour, "error", "", "", "agent:doom:cron:j2:run:r2", "cron: job interrupted by gateway restart", 0},
		{"j3", -3 * time.Hour, "ok", "anthropic", "claude", "agent:main:cron:j3:run:r3", "", 500},
	})

	d := openClawDB(context.Background(), root)
	if d.Reason != "" {
		t.Fatalf("reason = %q, want empty", d.Reason)
	}
	if len(d.Occupancy) != 2 {
		t.Fatalf("寫進帳本的筆數 = %d，want 2（沒有 provider 的那筆不該寫）", len(d.Occupancy))
	}
	if d.OccupancyRowsSeen < 3 {
		t.Errorf("rows_seen = %d，want ≥3 —— 跳過的也要數，否則缺口是隱形的", d.OccupancyRowsSeen)
	}
	if d.OccupancyRowsNoProvider != 1 {
		t.Errorf("rows_no_provider = %d，want 1", d.OccupancyRowsNoProvider)
	}
	got := d.Occupancy[0]
	if got.Provider != "openai" {
		t.Errorf("provider = %q，want openai（原文，不正規化）", got.Provider)
	}
	if got.AgentID != "cnode-network" {
		t.Errorf("agent_id = %q，want cnode-network（從 session_key 切出來）", got.AgentID)
	}
	if got.Source != "cron_run_logs" {
		t.Errorf("source = %q", got.Source)
	}
}

// ⚠ 舊版 OpenClaw 的 cron_run_logs 沒有 provider 欄位。
// 少一個欄位就讓整份觀測掛掉，等於「舊版本的機器在畫面上消失」——
// 而那正是這個產品要修的那個 bug。
func TestOldCronSchemaDoesNotBreakTheWholeObservation(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "state", "openclaw.sqlite"), true) // 沒有 provider 欄

	d := openClawDB(context.Background(), root)
	if d.Reason != "" {
		t.Fatalf("舊 schema 讓整份觀測失敗了：%q", d.Reason)
	}
	if d.TaskRunRows != 3 {
		t.Errorf("其他欄位也該照常收：task_run_rows = %d", d.TaskRunRows)
	}
	if len(d.Occupancy) != 0 {
		t.Errorf("沒有 provider 欄位就是沒有占用證據，不該生出 %d 筆", len(d.Occupancy))
	}
}

// split layout 沒有 cron_run_logs，證據在 ~/.openclaw/cron/runs/*.jsonl。
// 這是照著 sampleagent2 的真檔案寫的 —— SPEC 完全沒提到這個位置。
func TestOccupancyFromSplitLayoutJSONL(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite"), false)

	dir := filepath.Join(root, "cron", "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ms := func(d time.Duration) int64 { return time.Now().Add(d).UnixMilli() }
	lines := fmt.Sprintf(`{"ts":%d,"jobId":"j1","action":"finished","status":"ok","provider":"openai-codex","model":"gpt-5.5","sessionKey":"agent:sampleagent2-management:cron:j1:run:r1","usage":{"total_tokens":20901}}
{"ts":%d,"jobId":"j2","action":"finished","status":"error","sessionKey":"agent:doom:cron:j2:run:r2","error":"boom"}
這一行不是 JSON，應該被跳過而不是讓整份觀測失敗
{"ts":%d,"jobId":"j3","action":"finished","status":"ok","provider":"openai-codex","sessionKey":"agent:main:cron:j3:run:r3"}
`, ms(-time.Hour), ms(-2*time.Hour), ms(-3*time.Hour))
	if err := os.WriteFile(filepath.Join(dir, "j.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	d := openClawDB(context.Background(), root)
	if len(d.Occupancy) != 2 {
		t.Fatalf("帳本筆數 = %d，want 2", len(d.Occupancy))
	}
	if d.Occupancy[0].Source != "cron_runs_jsonl" {
		t.Errorf("source = %q，want cron_runs_jsonl", d.Occupancy[0].Source)
	}
	// ⚠ sampleagent2 寫 "openai-codex"、samplehub1 寫 "openai"，那是同一條 codex 訂閱
	// 在不同版本下的兩個名字。帳本一律存原文 —— 自動併起來會蓋掉真實差異，
	// 自動拆開會看起來像一次從來沒發生過的換票。
	if d.Occupancy[0].Provider != "openai-codex" {
		t.Errorf("provider = %q，want openai-codex 原文", d.Occupancy[0].Provider)
	}
	if d.OccupancyRowsNoProvider != 1 {
		t.Errorf("rows_no_provider = %d，want 1（j2 沒有 provider）", d.OccupancyRowsNoProvider)
	}
}

type cronRow struct {
	jobID      string
	age        time.Duration
	status     string
	provider   string
	model      string
	sessionKey string
	errText    string
	tokens     int
}

// addProviderColumns 把測試 DB 的 cron_run_logs 換成有 provider 的新版 schema。
func addProviderColumns(t *testing.T, path string, rows []cronRow) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`DROP TABLE cron_run_logs`,
		`CREATE TABLE cron_run_logs (job_id TEXT, ts INTEGER, status TEXT, summary TEXT,
			provider TEXT, model TEXT, session_key TEXT, error TEXT, total_tokens INTEGER)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO cron_run_logs VALUES (?,?,?,'摘要',?,?,?,?,?)`,
			r.jobID, time.Now().Add(r.age).UnixMilli(), r.status,
			nullIfEmpty(r.provider), nullIfEmpty(r.model), r.sessionKey,
			nullIfEmpty(r.errText), r.tokens); err != nil {
			t.Fatal(err)
		}
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------- 心跳

func TestHeartbeat(t *testing.T) {
	lastObservationUnixNano.Store(0)

	c, err := Heartbeat(context.Background(), 42)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if c.SchemaVersion != model.SchemaVersion || c.AgentSeq != 42 || c.SentAt.IsZero() {
		t.Errorf("bad envelope: %+v", c)
	}
	// ⚠ 沒觀測過就是 nil，不是 0 —— 0 的意思是「剛剛才觀測完」，正好相反。
	if c.ObservationAgeSeconds != nil {
		t.Errorf("observation_age = %v, want nil before any Collect", *c.ObservationAgeSeconds)
	}

	lastObservationUnixNano.Store(time.Now().Add(-90 * time.Second).UnixNano())
	c, err = Heartbeat(context.Background(), 43)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if c.ObservationAgeSeconds == nil || *c.ObservationAgeSeconds < 89 || *c.ObservationAgeSeconds > 92 {
		t.Errorf("observation_age = %v, want ~90", c.ObservationAgeSeconds)
	}
	// Phase 4 的欄位由 agent journal 填，probe 不准自己編。
	if c.MaxSeenRevision != 0 || c.MaxAppliedRevision != 0 {
		t.Errorf("probe 不該填 revision 欄位: %+v", c)
	}
}

func TestHeartbeatDeadContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Heartbeat(ctx, 1); err == nil {
		t.Fatal("want error on cancelled context")
	}
	if _, err := Collect(ctx); err == nil {
		t.Fatal("want error on cancelled context")
	}
}

func TestObservationAge(t *testing.T) {
	now := time.Now()
	lastObservationUnixNano.Store(0)
	if got := observationAge(now); got != nil {
		t.Errorf("age = %v, want nil", *got)
	}
	// 時鐘往回跳時不可以回報負數年齡。
	lastObservationUnixNano.Store(now.Add(time.Hour).UnixNano())
	if got := observationAge(now); got == nil || *got != 0 {
		t.Errorf("age = %v, want 0 for a future observation", got)
	}
	lastObservationUnixNano.Store(0)
}

// ---------------------------------------------------------------- 真機比對
//
// 預設 skip。要跟 tools/fleet-probe.py 對答案時才開：
//
//	CLAWCTL_PROBE_LIVE=1 CLAWCTL_PROBE_OUT=/tmp/go.json go test ./internal/probe/ -run TestLiveCollect -v

func TestLiveCollect(t *testing.T) {
	if os.Getenv("CLAWCTL_PROBE_LIVE") == "" {
		t.Skip("set CLAWCTL_PROBE_LIVE=1 to probe this machine for real")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	batch, err := Collect(ctx)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	t.Logf("Collect took %s", time.Since(start))

	b, err := json.MarshalIndent(batch, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("CLAWCTL_PROBE_OUT"); out != "" {
		if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", out)
	} else {
		fmt.Println(string(b))
	}

	if batch.Identity.Hostname == "" {
		t.Error("hostname 是空的")
	}
	if batch.Resources.DiskTotalBytes == 0 {
		t.Error("disk_total 是 0")
	}
	if len(batch.Systemd) != len(watchedUnits) {
		t.Errorf("systemd units = %d, want %d（沒裝的也要出現）", len(batch.Systemd), len(watchedUnits))
	}
	for _, c := range batch.Credentials {
		assertNoSecrets(t, c)
	}
}

// ---------------------------------------------------------------- 測試小工具

func timePtr(t time.Time) *time.Time { return &t }

func assertTimeEq(t *testing.T, got, want *time.Time) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil:
		t.Errorf("got nil, want %s", want.Format(time.RFC3339Nano))
	case want == nil:
		t.Errorf("got %s, want nil", got.Format(time.RFC3339Nano))
	case !got.Equal(*want):
		t.Errorf("got %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

// assertNoSecrets 是規則 2 的執法者：憑證輸出裡不可以出現 token 的任何片段。
func assertNoSecrets(t *testing.T, c model.Credential) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠ 這裡找的是**祕密的值**，不是欄位名稱。
	// last_refresh 是三元組的一員，它的名字裡有 "refresh" 是合法的；
	// 會出事的是 refresh_token 的內容跑出來。所以比對的是我們在測試 fixture
	// 裡種下的哨兵字串與 JWT/API key 的特徵前綴。
	for _, bad := range []string{"SECRET", "eyJ", "sk-", "Bearer ", "refresh_token"} {
		if containsFold(string(b), bad) && !allowedMention(c, bad) {
			t.Errorf("%s 的輸出裡出現了 %q：%s", c.Provider, bad, b)
		}
	}
}

// allowedMention：note 裡那句 "exp from access_token (NOT id_token)" 是刻意留下的
// 警語，不是洩漏。除了它以外任何 token 字樣都是 bug。
func allowedMention(c model.Credential, needle string) bool {
	return (needle == "access_token" || needle == "refresh") && containsFold(c.Note, needle)
}

func containsFold(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		stringsIndexFold(haystack, needle) >= 0
}

func stringsIndexFold(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if equalFold(s[i:i+len(substr)], substr) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 判準：adapter 碰到未知 CLI 版本 → 仍回得出 binary path 與 raw version 字串，
// 標 unsupported。
//
// ⚠ 重點是「仍回得出」。一個承認自己不支援、卻順手把手上的證據丟掉的 adapter，
// 比一個猜錯版本號的還糟：猜錯的至少留下一條可以推翻的線索。
func TestUnknownCLIVersionKeepsThePathAndTheRawString(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "weirdcli")
	// 一個沒有任何 semver 的 --version 輸出。實機上這種事會發生在
	// 換了 release channel、或工具改用 codename 當版本的時候。
	writeExec(t, bin, "#!/bin/sh\necho 'weirdcli (nightly channel, build cosmic-otter)'\n")
	t.Setenv("PATH", dir)

	tool := cliTool(context.Background(), "weirdcli", nil, model.ProcessScanUnavailable, probeSearchPath())

	if tool.Support != model.SupportUnsupported {
		t.Errorf("support = %q, want unsupported —— 取不出版本號卻沒承認", tool.Support)
	}
	if !tool.Present || tool.Path != bin {
		t.Errorf("path 掉了：present=%v path=%q，人接下來要去看的就是這個檔案",
			tool.Present, tool.Path)
	}
	if !strings.Contains(tool.VersionRaw, "cosmic-otter") {
		t.Errorf("version_raw = %q，原文沒留下來 —— 那是唯一還能人工判讀的東西",
			tool.VersionRaw)
	}
	if tool.VersionReported != "" {
		t.Errorf("version_reported = %q —— 讀不懂就不要生一個出來", tool.VersionReported)
	}
}

// 反面：讀得懂的時候不准標 unsupported。
// ⚠ 少了這一條，把 Support 寫死成 unsupported 也會通過上面那個測試，
// 然後每一台好機器都掛著一個「不支援」——§5.1 那盞永遠亮的黃燈。
func TestKnownCLIVersionIsNotMarkedUnsupported(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "normalcli")
	writeExec(t, bin, "#!/bin/sh\necho '2.1.7'\n")
	t.Setenv("PATH", dir)

	tool := cliTool(context.Background(), "normalcli", nil, model.ProcessScanUnavailable, probeSearchPath())
	if tool.Support != model.SupportOK {
		t.Errorf("support = %q, want supported（版本號取得出來）", tool.Support)
	}
	if tool.VersionReported != "2.1.7" {
		t.Errorf("version_reported = %q, want 2.1.7", tool.VersionReported)
	}
}

// 「沒有資料庫」跟「資料庫搬家了」要分得開。
//
// ⚠ 沒有這一條，OpenClaw 下一次改 layout 的時候，全機隊會安靜地顯示
// 「找不到 sqlite」——跟一台從沒跑過任務的新機器一模一樣的畫面。
// 那個錯誤可以在沒有人發現的情況下持續好幾個月，因為它看起來完全正常。
func TestUnknownOpenClawLayoutIsNotTheSameAsNoDatabase(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	// 一個我們不認得的位置。2027 年的 OpenClaw 可能就長這樣。
	makeOpenClawDB(t, filepath.Join(root, "data", "openclaw.sqlite"), true)

	d := openClawDB(context.Background(), root)
	if d.Support != model.SupportUnsupported {
		t.Errorf("support = %q, want unsupported —— 底下明明有一個 sqlite", d.Support)
	}
	if len(d.FoundAt) == 0 {
		t.Fatal("說了不支援卻沒說在哪裡找到的 —— 那句話就變成猜測")
	}
	if !strings.Contains(d.FoundAt[0], "data/openclaw.sqlite") {
		t.Errorf("found_at = %v，沒指到真的那個檔案", d.FoundAt)
	}
}

// 真的什麼都沒有的時候，不准說「不支援」。
func TestEmptyOpenClawDirIsMissingNotUnsupported(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if d := openClawDB(context.Background(), root); d.Support != "" {
		t.Errorf("support = %q —— 目錄是空的，adapter 沒有理由怪版本", d.Support)
	}
}

// 遷移殘骸不是新 layout 的證據。
// ⚠ sampleagent3 上真的有一個 .migrated 檔。拿它當證據，那台會永遠標著 unsupported。
func TestMigratedCorpseDoesNotLookLikeANewLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".openclaw")
	makeOpenClawDB(t, filepath.Join(root, "state", "openclaw.sqlite"), true)
	makeOpenClawDB(t, filepath.Join(root, "tasks", "runs.sqlite.migrated"), false)

	d := openClawDB(context.Background(), root)
	if d.Support == model.SupportUnsupported {
		t.Errorf("被 .migrated 殘骸騙了：found_at = %v", d.FoundAt)
	}
	if d.Layout != "consolidated" {
		t.Errorf("layout = %q, want consolidated", d.Layout)
	}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestShowUnitMeasurementStates(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		present    bool
		measured   bool
		reasonPart string
		active     string
		sub        string
		ok         bool
	}{
		{
			name:       "systemctl unavailable",
			script:     "#!/bin/sh\necho 'Failed to connect to bus' >&2\nexit 1\n",
			reasonPart: "Failed to connect to bus",
		},
		{
			name:     "unit not found",
			script:   "#!/bin/sh\necho 'LoadState=not-found'\n",
			measured: true,
			ok:       true,
		},
		{
			name:     "normal unit",
			script:   "#!/bin/sh\nprintf 'LoadState=loaded\\nActiveState=active\\nSubState=running\\n'\n",
			present:  true,
			measured: true,
			active:   "active",
			sub:      "running",
			ok:       true,
		},
		{
			name:       "missing LoadState",
			script:     "#!/bin/sh\necho 'ActiveState=active'\n",
			reasonPart: "systemctl 沒有回報 LoadState",
			ok:         true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeExec(t, filepath.Join(dir, "systemctl"), tc.script)
			t.Setenv("PATH", dir)

			got, ok := showUnit(context.Background(), "clawctl-agent.service", true)
			if ok != tc.ok {
				t.Errorf("ok=%v, want %v", ok, tc.ok)
			}
			if got.Present != tc.present || got.Measured != tc.measured {
				t.Errorf("Present=%v Measured=%v, want Present=%v Measured=%v",
					got.Present, got.Measured, tc.present, tc.measured)
			}
			if tc.reasonPart == "" && got.Reason != "" {
				t.Errorf("Reason=%q, want empty", got.Reason)
			}
			if tc.reasonPart != "" && !strings.Contains(got.Reason, tc.reasonPart) {
				t.Errorf("Reason=%q, want it to contain %q", got.Reason, tc.reasonPart)
			}
			if tc.name == "systemctl unavailable" {
				if !strings.Contains(got.Reason, "systemctl --user show clawctl-agent.service") ||
					strings.Contains(got.Reason, "-p LoadState") {
					t.Errorf("Reason=%q, want concise command identity without property arguments", got.Reason)
				}
			}
			if got.ActiveState != tc.active || got.SubState != tc.sub {
				t.Errorf("ActiveState=%q SubState=%q, want %q/%q",
					got.ActiveState, got.SubState, tc.active, tc.sub)
			}
		})
	}
}

// samplehub1 在 2026-09-03 的真實 process 表（節錄）。
//
// ⚠ 這些不是編出來的。前四筆是這個比對器**必須拒絕**的誘餌，
// 而在修好之前，它拒絕了全部五筆 —— 包括最後那個真的。
func samplehub1Procs() []procInfo {
	return []procInfo{
		{pid: 3185, comm: "python", exe: "/home/example-user/.hermes/hermes-agent/venv/bin/python",
			argv: []string{"/home/example-user/.hermes/hermes-agent/venv/bin/python", "-m",
				"hermes_cli.main", "--profile", "openclaw-evolution", "gateway", "run", "--replace"}},
		{pid: 3197, comm: "bash", exe: "/bin/bash",
			argv: []string{"/bin/bash", "/home/example-user/.openclaw/workspace/baseline/watcher.sh"}},
		{pid: 10631, comm: "node", exe: "/usr/bin/node",
			argv: []string{"node", "/home/example-user/.openclaw/npm/projects/openclaw-codex-8902d781d4/node_modules/@openclaw/codex/node_modules/.bin/codex", "app-server"}},
		{pid: 3191, comm: "node", exe: "/usr/bin/node",
			argv: []string{"/usr/bin/node", "/home/example-user/.local/node_modules/openclaw/dist/index.js",
				"gateway", "--port", "18789"}},
	}
}

// 判準的前置條件：process 偵測要真的抓得到。
//
// ⚠ 這個測試是倒過來寫的 —— 先發現真機上 running_pid 全機隊 100% 是 NULL，
// 才回頭寫它。在那之前所有單元測試都是綠的，因為它們用的是
// exe 直接等於安裝路徑的假資料，而真機上沒有一支工具長那樣：
// 它們全是 node script，exe 一律 /usr/bin/node。
func TestNodeWrappedCLIIsFound(t *testing.T) {
	p, script, ok := matchProcess(samplehub1Procs(), "openclaw",
		"/usr/lib/node_modules/openclaw/openclaw.mjs")
	if !ok {
		t.Fatal("openclaw gateway 正在 pid 3191 跑著卻沒被抓到 —— " +
			"畫面上那會顯示成「沒有在跑」，跟真的停掉一模一樣")
	}
	if p.pid != 3191 {
		t.Fatalf("抓到 pid %d，那是誘餌不是 openclaw", p.pid)
	}
	if script != "/home/example-user/.local/node_modules/openclaw/dist/index.js" {
		t.Errorf("script = %q —— 沒有它就只知道「有個 node 在跑」，"+
			"不知道跑的是哪一份安裝", script)
	}
}

// 三個誘餌一個都不准中。
// ⚠ 抓錯一個比抓不到更糟：畫面上會出現一個很具體、而且是錯的答案。
func TestLookalikeProcessesAreNotMistakenForOpenClaw(t *testing.T) {
	for _, decoy := range samplehub1Procs()[:3] {
		if _, _, ok := matchProcess([]procInfo{decoy}, "openclaw", "/usr/lib/node_modules/openclaw/openclaw.mjs"); ok {
			t.Errorf("pid %d (%v) 被當成了 openclaw", decoy.pid, decoy.argv)
		}
	}
}

// 同一張表裡，codex 要對到 codex。
func TestNodeBinShimMatchesItsOwnTool(t *testing.T) {
	p, script, ok := matchProcess(samplehub1Procs(), "codex", "/usr/local/lib/node_modules/@openai/codex/bin/codex.js")
	if !ok || p.pid != 10631 {
		t.Fatalf("codex 沒對上：ok=%v pid=%d", ok, p.pid)
	}
	if !strings.HasSuffix(script, "/node_modules/.bin/codex") {
		t.Errorf("script = %q", script)
	}
}

// cmdline 是 NUL 分隔的。用空白切會把帶空格的路徑切成兩半。
func TestArgvIsNULSeparated(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cmdline")
	if err := os.WriteFile(f, []byte("node\x00/opt/my apps/x.js\x00run\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readArgv(f)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"node", "/opt/my apps/x.js", "run"}
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %q, want %q", got, want)
		}
	}
}

// ⚠⚠ 這是這個檔案裡最重要的一個測試。
//
// 它模擬的不是一個奇怪的邊界，而是**agent 平常跑起來時的正常狀態**：
// Ubuntu 預設 yama/ptrace_scope=1，所以 /proc/<pid>/exe 這個 symlink
// 只有該 process 的祖先讀得到。systemd service 沒有子孫，於是它讀不到
// 任何一個 —— exe 全部是空字串。
//
// 實測 2026-09-03，同一份程式碼：
//
//	從 shell 跑     → 550 個 pid，77 個讀得到 exe
//	從 agent 的 unit → 553 個 pid，**0 個**讀得到 exe
//
// 舊版把「讀不到 exe」當成「跳過這個 process」，所以在真機上
// 整張表是空的，而所有單元測試都是綠的 —— 因為測試餵的假資料
// 每一筆都已經填好 exe 了。假資料太乾淨，就測不到真的問題。
func TestProcessMatchWorksWhenExeIsUnreadable(t *testing.T) {
	procs := samplehub1Procs()
	for i := range procs {
		procs[i].exe = "" // ptrace_scope=1 的真實樣子
		// node 還會把主執行緒改名，實測 openclaw gateway 的 comm 是 MainThread。
		procs[i].comm = "MainThread"
	}
	p, script, ok := matchProcess(procs, "openclaw",
		"/usr/lib/node_modules/openclaw/openclaw.mjs")
	if !ok {
		t.Fatal("exe 讀不到就整個瞎掉 —— 而那是 agent 平常的狀態，不是例外")
	}
	if p.pid != 3191 || script == "" {
		t.Fatalf("pid=%d script=%q", p.pid, script)
	}
}

// argv[0] 就是安裝路徑的情況（非 node 的工具，或直接執行的 binary）。
func TestArgv0MatchesInstalledPath(t *testing.T) {
	procs := []procInfo{{pid: 42, exe: "", comm: "x",
		argv: []string{"/usr/local/bin/mytool", "serve"}}}
	if p, _, ok := matchProcess(procs, "mytool", "/usr/local/bin/mytool"); !ok || p.pid != 42 {
		t.Fatalf("argv[0] 等於安裝路徑卻沒對上：ok=%v", ok)
	}
}

// TestProcessScanStatePinsRunningReason 釘住掃描狀態與給人讀的原因必須講同一件事。
// ⚠ 特別是 ReadDir 成功但 0 個可用 process 代表 /proc 讀得到、只是視野受限，
// 不准再跟 ReadDir 失敗一起折成 unavailable。
func TestProcessScanStatePinsRunningReason(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ghosttool")
	writeExec(t, bin, "#!/bin/sh\necho '1.0.0'\n")
	t.Setenv("PATH", dir)

	makeProcs := func(n int) []procInfo {
		procs := make([]procInfo, 0, n)
		for i := 0; i < n; i++ {
			procs = append(procs, procInfo{pid: i + 1, argv: []string{"/usr/bin/something"}})
		}
		return procs
	}
	tests := []struct {
		name        string
		procs       []procInfo
		processScan string
		wantReason  string
	}{
		{"proc 讀不到", nil, model.ProcessScanUnavailable, "讀不到 /proc，這台的 process 偵測整個是關的"},
		{"讀得到但沒有可用 process", []procInfo{}, model.ProcessScanRestricted, "只掃得到 0 個 process，這台的 /proc 視野被限制了"},
		{"只讀得到五個 process", makeProcs(5), model.ProcessScanRestricted, "只掃得到 5 個 process，這台的 /proc 視野被限制了"},
		{"完整掃描仍沒對上", makeProcs(300), model.ProcessScanComplete, "掃了 300 個 process，沒有一個對得上"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := cliTool(context.Background(), "ghosttool", tt.procs, tt.processScan, probeSearchPath())
			if tool.ProcessScan != tt.processScan {
				t.Errorf("process_scan = %q，want %q", tool.ProcessScan, tt.processScan)
			}
			if tool.RunningReason != tt.wantReason {
				t.Errorf("running_reason = %q，want %q", tool.RunningReason, tt.wantReason)
			}
		})
	}
}

// 掃到的 process 少得不合理時，不准說「它沒在跑」。
// ⚠ 那是關於 agent 自己視野的事實，不是關於世界的事實。
func TestTooFewProcessesIsNotEvidenceOfAbsence(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ghosttool")
	writeExec(t, bin, "#!/bin/sh\necho '1.0.0'\n")
	t.Setenv("PATH", dir)

	few := []procInfo{{pid: 1, argv: []string{"/sbin/init"}}}
	tool := cliTool(context.Background(), "ghosttool", few, model.ProcessScanRestricted, probeSearchPath())
	if !strings.Contains(tool.RunningReason, "視野") {
		t.Errorf("只掃到 1 個 process 就下結論了：running_reason=%q", tool.RunningReason)
	}
}

// 反面：掃得到一整台機器的 process、確實沒有它 —— 那才是證據。
func TestPlentyOfProcessesAndStillMissingIsEvidence(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ghosttool")
	writeExec(t, bin, "#!/bin/sh\necho '1.0.0'\n")
	t.Setenv("PATH", dir)

	many := make([]procInfo, 0, 300)
	for i := 0; i < 300; i++ {
		many = append(many, procInfo{pid: i + 1, argv: []string{"/usr/bin/something"}})
	}
	tool := cliTool(context.Background(), "ghosttool", many, model.ProcessScanComplete, probeSearchPath())
	if !strings.Contains(tool.RunningReason, "沒有一個對得上") {
		t.Errorf("掃了 300 個確實沒有，這是真證據，卻沒講：%q", tool.RunningReason)
	}
}

// ⚠ kernel thread 的 cmdline 是空的，它們不可以混進 process 表。
// strings.Split("", sep) 回的是 [""] 不是 []，所以「空的就跳過」這件事
// 必須自己判斷 —— 少了它，process 總數會膨脹成四倍，
// 而那個總數正是「我的視野有沒有被擋住」的唯一判斷依據。
func TestKernelThreadsAreNotCounted(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cmdline")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readArgv(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("空的 cmdline 得到 %q，want 空 —— 一個沒有名字的東西不該佔一格", got)
	}
}

// ---------------------------------------------------------------- providers

// 每一家 provider 的兩個面都要被檢查過，不然就要寫下為什麼只檢查一半。
//
// ⚠⚠ 這支測試守的是一個**已經發生過**的 bug，不是假想的。
//
// 2026-09-03 之前，watchedTools 與 credentials() 是兩張各自手寫的清單：
//
//	watchedTools  = {claude, codex, grok, agy,    openclaw}
//	credentials() = {claude, codex, grok, gemini, openclaw}
//
// 於是 gemini 的憑證每兩分鐘被讀一次並回報（`~/.gemini` 在實機上真的存在），
// 而那支 CLI 從來沒有被檢查過 —— 裝上 @google/gemini-cli 之後，畫面會講得出
// 它的登入狀態，卻永遠說不出它裝了沒有、版號多少、在不在跑。
//
// 而 agy 剛好反過來。兩個方向都漏了，而且**兩邊各自的測試都是綠的** ——
// 因為沒有任何一支測試在比那兩張清單（§5.0：bug 住在零件中間）。
//
// ⚠ 這支測試刻意**不要求兩邊都有**。一家可以只有 CLI 沒有憑證，
// 但那必須是寫出來的決定（Reason），不是兩張清單各自漂移的結果。
func TestEveryProviderIsCheckedOnBothSidesOrSaysWhyNot(t *testing.T) {
	if len(providers) == 0 {
		t.Fatal("providers 是空的 —— 那表示這台機器上什麼都不會被檢查")
	}
	seen := map[string]bool{}
	for _, p := range providers {
		if p.Name == "" {
			t.Error("有一家 provider 沒有名字")
			continue
		}
		if seen[p.Name] {
			t.Errorf("%s 在 providers 裡出現兩次 —— 它的觀測會被回報兩份", p.Name)
		}
		seen[p.Name] = true

		if !p.HasCLI && p.Cred == nil {
			t.Errorf("%s 兩邊都不檢查，那它在這張表裡沒有任何作用", p.Name)
		}
		// 不對稱要有理由。
		if (p.HasCLI && p.Cred == nil) || (!p.HasCLI && p.Cred != nil) {
			if strings.TrimSpace(p.Reason) == "" {
				t.Errorf("%s 只檢查一半（CLI=%v、憑證=%v）卻沒有寫理由。\n"+
					"這正是 gemini 被漏掉的方式：不是有人決定不檢查它，\n"+
					"是兩張手寫的清單各自改，而沒有東西在比。",
					p.Name, p.HasCLI, p.Cred != nil)
			}
		}
	}
}

// watchedTools 必須是從 providers 生出來的，不是另一份手寫清單。
//
// ⚠ 這支測試會在「有人為了方便，又在別的地方寫死一份工具名單」時變紅。
func TestWatchedToolsComesFromTheProviderTable(t *testing.T) {
	want := map[string]bool{}
	for _, p := range providers {
		if p.HasCLI {
			want[p.Name] = true
		}
	}
	if len(watchedTools) != len(want) {
		t.Fatalf("watchedTools 有 %d 項，providers 說該有 %d 項：%v vs %v",
			len(watchedTools), len(want), watchedTools, want)
	}
	for _, name := range watchedTools {
		if !want[name] {
			t.Errorf("watchedTools 裡的 %q 不在 providers 表裡 —— "+
				"它的憑證那一半永遠不會被檢查，而且沒有人會發現", name)
		}
	}
}

// 憑證那一份清單也要跟 providers 一致 —— 而且要真的跑一次去數。
//
// ⚠ 上面兩支測試都只看宣告。這一支真的呼叫 credentials()，
// 因為「表對了」跟「讀表的那個迴圈對了」是兩件事。
func TestCredentialsCoversExactlyTheProvidersThatHaveThem(t *testing.T) {
	home := t.TempDir()
	got := credentials(home, time.Now())

	want := map[string]bool{}
	for _, p := range providers {
		if p.Cred != nil {
			want[p.Name] = true
		}
	}
	if len(got) != len(want) {
		var names []string
		for _, c := range got {
			names = append(names, c.Provider)
		}
		t.Fatalf("credentials() 回了 %d 家（%v），providers 說該有 %d 家",
			len(got), names, len(want))
	}
	for _, c := range got {
		if !want[c.Provider] {
			t.Errorf("credentials() 回了 %q，但它不在 providers 表裡", c.Provider)
		}
		// ⚠ 空的 home 目錄下，每一家都必須說得出「沒有」，而不是留白。
		if c.Status == "" {
			t.Errorf("%s 在一個空的 home 下沒有給狀態 —— "+
				"一格空白會被讀成「這一欄不重要」，不是「我不知道」", c.Provider)
		}
	}
}

// gemini 這一家兩邊都要在 —— 它是這張表存在的原因。
//
// ⚠ 這支測試看起來多餘（上面三支已經在守一致性了），但它守的是別的東西：
// 上面那三支在「有人把 gemini 整家從表裡刪掉」的時候全部還是綠的。
func TestGeminiIsCheckedOnBothSides(t *testing.T) {
	var found *provider
	for i := range providers {
		if providers[i].Name == "gemini" {
			found = &providers[i]
		}
	}
	if found == nil {
		t.Fatal("gemini 不在 providers 表裡。實機上 ~/.gemini 是存在的，" +
			"而這一家正是「兩張清單漂開」那個 bug 的當事人")
	}
	if !found.HasCLI {
		t.Error("gemini 的 CLI 沒有被檢查 —— 那正是 2026-09-03 修掉的那個 bug")
	}
	if found.Cred == nil {
		t.Error("gemini 的憑證沒有被讀 —— 實機上 ~/.gemini 是存在的")
	}
}

// 一個正在跑、但不在 PATH 上的工具，不准被講成「沒有安裝」。
//
// ⚠⚠ 這支測試釘的是一個**實機上正在發生**的 bug（2026-09-03 samplehub1）：
//
//	$ ps aux | grep agy
//	example-user 1725073 1.2 0.3 3812400 224396 pts/6 Sl+ 10:59 2:23 agy
//	$ command -v agy
//	（什麼都沒有）
//
// 那個 process 從 10:59 就在跑、吃了 2 分 23 秒 CPU、它的 OAuth token
// 13:59 才更新過。而 clawctl 對全機隊四台都說 agy「沒有安裝」——
// 因為舊的 cliTool 在 LookPath 失敗時直接 return，**連手上那份 process
// 清單都不看一眼**，即使答案就在裡面。
//
// 這是 §5.10 的形狀：只讀了唯一看不到那個東西的來源，
// 然後把「我沒看到」寫成「它不存在」。
//
// ⚠ 而且方向是最糟的那一邊：一個正在跑的 process 是「它裝了」**最強**的
// 證據。PATH 講的是「這個 shell 的環境」，process 講的是「這台機器上的事實」。
// 舊的程式用弱的那個否決了強的那個。
func TestARunningToolIsNeverReportedAsNotInstalled(t *testing.T) {
	// ⚠ 用一個絕對不會在 PATH 上的名字。拿 "agy" 來測的話，
	// 在真的裝了 agy 的機器上這支測試會因為別的理由通過。
	const name = "definitely-not-on-path-zzz"
	if _, ok := lookPathIn(probeSearchPath().Dirs, name); ok {
		t.Skipf("%s 竟然在 PATH 上，這支測試測不到東西", name)
	}

	procs := []procInfo{
		{pid: 1, argv: []string{"/sbin/init"}},
		// 就是 agy 在實機上的樣子：comm 是它的名字，exe 讀不到（ptrace_scope=1）。
		{pid: 1725073, comm: name, exe: "", argv: []string{name}},
	}
	// 湊到 minPlausibleProcs 以上，不然會走「/proc 視野被限制」那一條。
	for i := 0; i < minPlausibleProcs+10; i++ {
		procs = append(procs, procInfo{pid: 5000 + i, argv: []string{"/usr/bin/other"}})
	}

	got := cliTool(context.Background(), name, procs, model.ProcessScanComplete, probeSearchPath())

	if !got.Present {
		t.Errorf("有一個叫 %s 的 process 正在跑（pid %d），而 clawctl 說它沒有安裝。\n"+
			"一個正在跑的 process 是「它裝了」最強的證據 —— 不在 PATH 上\n"+
			"只代表叫不動它，不代表它不存在。這正是 agy 在全機隊四台上\n"+
			"被講成「沒安裝」的那個 bug。", name, 1725073)
	}
	if got.OnPath {
		t.Error("它不在 PATH 上，on_path 不該是 true —— " +
			"「裝了」跟「叫得動」是兩件事，折疊起來就沒辦法講出「叫不動」")
	}
	if got.PresentEvidence != "process" {
		t.Errorf("present 的證據該是 process，是 %q —— "+
			"一個沒有來源的 present=true 沒辦法被質疑", got.PresentEvidence)
	}
	if got.RunningPID != 1725073 {
		t.Errorf("沒有把 pid 記下來（是 %d）—— 那是人接下來唯一能用的東西",
			got.RunningPID)
	}
	// ⚠ 版本問不到要說為什麼，不准留白。
	if got.VersionReason == "" {
		t.Error("問不到版本卻沒有說為什麼 —— 一格空白會被讀成「這一欄不重要」")
	}
	if !strings.Contains(got.VersionReason, "PATH") {
		t.Errorf("版本問不到的理由沒有講出「不在 PATH 上」：%q", got.VersionReason)
	}
}

// PATH 上有、但沒有在跑的工具，還是要說它裝了 —— 而且證據是 path。
//
// ⚠ 這支跟上面那支是一對。少了這一支，上面那支可以靠「無條件 present=true」
// 通過，而那會讓每一個沒裝的工具都變成裝了。
func TestAToolOnPathIsPresentEvenWhenNotRunning(t *testing.T) {
	// sh 一定在 PATH 上，而且（幾乎）一定不會有一個 comm=="sh" 的 process
	// 剛好被我們湊出來 —— 這裡的 procs 是自己給的，所以可以確定。
	procs := []procInfo{{pid: 1, argv: []string{"/sbin/init"}}}
	for i := 0; i < minPlausibleProcs+10; i++ {
		procs = append(procs, procInfo{pid: 5000 + i, argv: []string{"/usr/bin/other"}})
	}
	got := cliTool(context.Background(), "sh", procs, model.ProcessScanComplete, probeSearchPath())
	if !got.Present {
		t.Fatal("sh 在 PATH 上卻被說成沒有安裝")
	}
	if !got.OnPath {
		t.Error("sh 在 PATH 上，on_path 該是 true")
	}
	if got.PresentEvidence != "path" {
		t.Errorf("present 的證據該是 path，是 %q", got.PresentEvidence)
	}
	if got.RunningPID != 0 {
		t.Errorf("沒有對得上的 process，running_pid 該是 0，是 %d", got.RunningPID)
	}
	// ⚠ 沒找到 process 要說是哪一種沒找到。
	if got.RunningReason == "" {
		t.Error("沒有對到 process 卻沒說是哪一種沒對到 —— " +
			"「掃了 300 個都不是它」跟「我讀不到 /proc」意思相反")
	}
}

// 兩邊都沒有的工具，就是真的沒有 —— 而且要說清楚是怎麼找的。
func TestAToolNeitherOnPathNorRunningIsAbsent(t *testing.T) {
	const name = "definitely-not-on-path-zzz"
	if _, ok := lookPathIn(probeSearchPath().Dirs, name); ok {
		t.Skipf("%s 竟然在 PATH 上", name)
	}
	procs := []procInfo{{pid: 1, argv: []string{"/sbin/init"}}}
	for i := 0; i < minPlausibleProcs+10; i++ {
		procs = append(procs, procInfo{pid: 5000 + i, argv: []string{"/usr/bin/other"}})
	}
	got := cliTool(context.Background(), name, procs, model.ProcessScanComplete, probeSearchPath())
	if got.Present {
		t.Error("PATH 上沒有、也沒有在跑，卻說它裝了")
	}
	if got.RunningReason == "" {
		t.Error("說它沒裝，卻沒有說是怎麼找的 —— " +
			"「我找過了，兩個地方都沒有」跟「我沒找」不是同一句話")
	}
}

// ---------------------------------------------------------------- 誰的 PATH

// probeSearchPath：測試用的搜尋路徑 —— 就是這個 test process 自己的 PATH。
//
// ⚠ 這裡刻意**不**去問 login shell。一支會跑跑測試的人的 ~/.profile 的
// 單元測試，在別人的機器上會因為別人的 rc 檔而紅 —— 那不是在測我的程式。
func probeSearchPath() searchPath {
	return searchPath{
		Dirs:   filepath.SplitList(os.Getenv("PATH")),
		Source: model.PathSourceDaemon,
	}
}

// fakeCLI 在 dir 底下造一支會印出 version 的假 CLI，回傳它的路徑。
func fakeCLI(t *testing.T, dir, name, version string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\necho " + version + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("造假 CLI：%v", err)
	}
	return path
}

// 這是 2026-09-03 第二個 bug 的釘子，而它比第一個危險得多。
//
// 第一個 bug（agy）讓畫面說「沒有安裝」—— 錯得很明顯。
// 這一個讓畫面說「claude 2.1.205」，而 operator 打 claude 跑的是 2.1.258。
// 沒有紅燈、沒有警告、沒有空白，只有一個很像真的錯數字 ——
// 而人會照著那個數字做決定（「不用升級了，已經是新的」）。
//
// 成因：程式先用 PATH 解出一個路徑存進 t.Path，然後**用名字**去跑
// `name --version`，於是 exec 拿 daemon 自己的 PATH 又解了一次。
// 兩次解析用的是不同的 PATH，回報的路徑跟真的被問到的檔案是兩個檔案。
//
// ⚠ 這支測試把兩條 PATH 刻意指向兩個**不同版號**的同名執行檔。
// 只要程式回頭用名字去叫，版號就會是 B 的，測試就會紅。
func TestVersionIsAskedOfTheFileWeResolvedNotOfTheName(t *testing.T) {
	const name = "zzz-twocli"
	human, daemon := t.TempDir(), t.TempDir()
	humanBin := fakeCLI(t, human, name, "1.0.0")
	daemonBin := fakeCLI(t, daemon, name, "2.0.0")

	// daemon 自己的 PATH 只看得到 B 那一份。
	t.Setenv("PATH", daemon)
	sp := searchPath{Dirs: []string{human}, Source: model.PathSourceLogin}

	got := cliTool(context.Background(), name, nil, model.ProcessScanUnavailable, sp)

	if got.Path != humanBin {
		t.Fatalf("回報的路徑是 %q，該是人打得到的那一個 %q", got.Path, humanBin)
	}
	if got.VersionReported != "1.0.0" {
		t.Errorf("版號是 %q，該是 1.0.0。\n"+
			"2.0.0 代表程式回報了 A 的路徑、卻去問了 B 的版號 —— "+
			"那就是畫面上寫 claude 2.1.205 而實際跑 2.1.258 的那個 bug。",
			got.VersionReported)
	}
	// 而且那個「另一個檔案」的存在本身要被講出來，不可以被吃掉。
	if got.DaemonReach != model.DaemonReachShadowed {
		t.Errorf("daemon_reach 是 %q，該是 shadowed —— "+
			"clawctl 用名字叫到的是另一個檔案，這件事以後會咬人", got.DaemonReach)
	}
	if got.DaemonPath != daemonBin {
		t.Errorf("daemon_path 是 %q，該是 %q —— 說「有另一個」卻不說是哪一個，"+
			"等於要人自己再查一次", got.DaemonPath, daemonBin)
	}
	if got.PathSource != model.PathSourceLogin {
		t.Errorf("path_source 是 %q，該是 login —— "+
			"不說是用誰的 PATH 量的，這個路徑就沒辦法被質疑", got.PathSource)
	}
}

// 兩邊指到同一個檔案的時候，要說「一樣」，不可以留白 ——
// 留白的意思是「早於這個區分」，跟「我比過了，是同一個」不是同一句話。
func TestWhenBothPathsAgreeItSaysSoRatherThanStayingSilent(t *testing.T) {
	const name = "zzz-onecli"
	dir := t.TempDir()
	bin := fakeCLI(t, dir, name, "3.3.3")
	t.Setenv("PATH", dir)

	got := cliTool(context.Background(), name, nil, model.ProcessScanUnavailable,
		searchPath{Dirs: []string{dir}, Source: model.PathSourceLogin})

	if got.DaemonReach != model.DaemonReachSame {
		t.Errorf("daemon_reach 是 %q，該是 same（兩邊都是 %s）", got.DaemonReach, bin)
	}
	if got.VersionReported != "3.3.3" {
		t.Errorf("版號 %q", got.VersionReported)
	}
}

// 人的 PATH 上有、daemon 的 PATH 上完全沒有 —— 那是 agy 在實機上的樣子。
// 這一格必須是 missing，不可以跟 shadowed 混在一起：
// 「會跑到另一個」跟「根本叫不動」的下一步不一樣。
func TestAToolTheDaemonCannotReachAtAllIsCalledMissingNotShadowed(t *testing.T) {
	const name = "zzz-humanonly"
	human, empty := t.TempDir(), t.TempDir()
	fakeCLI(t, human, name, "4.0.0")
	t.Setenv("PATH", empty)

	got := cliTool(context.Background(), name, nil, model.ProcessScanUnavailable,
		searchPath{Dirs: []string{human}, Source: model.PathSourceLogin})

	if got.DaemonReach != model.DaemonReachMissing {
		t.Errorf("daemon_reach 是 %q，該是 missing", got.DaemonReach)
	}
	if got.DaemonPath != "" {
		t.Errorf("daemon 拿不到，daemon_path 該是空的，是 %q", got.DaemonPath)
	}
	// ⚠ 但人這一邊是好的，版號還是要問得到。missing 不是壞消息。
	if got.VersionReported != "4.0.0" {
		t.Errorf("版號 %q —— daemon 拿不到不該影響「人打下去會跑什麼」", got.VersionReported)
	}
}

// 沒有可比的兩邊時，daemon_reach 必須留白。
//
// ⚠ 這支在防一個很容易寫出來的版本：無條件比較，於是在退回 daemon PATH 的
// 機器上每個工具都變成 same —— 那是一個**憑空生出來的保證**，
// 而它會蓋掉「我其實沒能問到人的 PATH」這件更重要的事。
func TestDaemonReachStaysBlankWhenThereIsNothingToCompare(t *testing.T) {
	const name = "zzz-nocompare"
	dir := t.TempDir()
	fakeCLI(t, dir, name, "5.0.0")
	t.Setenv("PATH", dir)

	got := cliTool(context.Background(), name, nil, model.ProcessScanUnavailable,
		searchPath{Dirs: []string{dir}, Source: model.PathSourceDaemon})

	if got.DaemonReach != "" {
		t.Errorf("用的就是 daemon 的 PATH，沒有兩邊可以比，daemon_reach 卻是 %q。\n"+
			"一個憑空生出來的「一樣」，會讓人以為這件事被檢查過了。", got.DaemonReach)
	}
}

// 問不到 login shell 的時候，要退回自己的 PATH **並且說出來**。
// 一個沒有說明的降級，跟一個假裝沒發生的降級一樣糟。
func TestAnUnreadableLoginShellIsReportedNotSilentlySwallowed(t *testing.T) {
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "no-such-shell"))
	sp := loginSearchPath(context.Background())

	if sp.Source != model.PathSourceDaemon {
		t.Errorf("shell 不存在，來源該退回 daemon，是 %q", sp.Source)
	}
	if sp.Reason == "" {
		t.Error("退回了卻沒說為什麼 —— 畫面上那個路徑會被當成「人打下去會跑的」，" +
			"而它其實是 daemon 自己的環境")
	}
	if len(sp.Dirs) == 0 {
		t.Error("退回之後連目錄都沒有 —— 那會讓全機隊所有工具一起變成「沒安裝」")
	}
}

// login shell 會印 motd、rc 檔裡也常有人自己 echo 東西。
// 拿到不像 PATH 的東西就不准用它 —— 用了會讓每個工具都變成「沒安裝」。
func TestJunkFromALoginShellIsNotMistakenForAPath(t *testing.T) {
	real := filepath.SplitList(os.Getenv("PATH"))
	cases := []struct {
		name string
		dirs []string
		want bool
	}{
		{"真的 PATH", real, true},
		{"motd 的最後一行", []string{"You have new mail."}, false},
		{"相對路徑", []string{"bin", "/usr/bin"}, false},
		{"只有一個存在的目錄", []string{"/usr/bin", "/nonexistent-zzz"}, false},
		{"空的", nil, false},
	}
	for _, c := range cases {
		if got := looksLikeSearchPath(c.dirs); got != c.want {
			t.Errorf("%s：looksLikeSearchPath(%q) = %v，該是 %v", c.name, c.dirs, got, c.want)
		}
	}
}

// lookPathIn 不准去看 os.Getenv("PATH")，也不准用 os.Setenv 換 PATH 再
// 呼叫 exec.LookPath —— 那會動到整個 process 的全域狀態，而 probe 是併發跑的。
func TestLookPathInIgnoresTheProcessPathEntirely(t *testing.T) {
	const name = "zzz-lookonly"
	dir, other := t.TempDir(), t.TempDir()
	want := fakeCLI(t, dir, name, "1")
	t.Setenv("PATH", other)

	if got, ok := lookPathIn([]string{dir}, name); !ok || got != want {
		t.Errorf("lookPathIn = (%q, %v)，該是 (%q, true)", got, ok, want)
	}
	if _, ok := lookPathIn([]string{other}, name); ok {
		t.Error("在只有別的東西的目錄裡找到了它")
	}
	// 找完之後 process 的 PATH 不准被動過。
	if os.Getenv("PATH") != other {
		t.Errorf("lookPathIn 改了 process 的 PATH（現在是 %q）—— "+
			"probe 各段是併發跑的，這等於在別人腳下換地板", os.Getenv("PATH"))
	}
	// 目錄、不可執行的檔案都不算。
	if err := os.WriteFile(filepath.Join(other, name+"-noexec"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookPathIn([]string{other}, name+"-noexec"); ok {
		t.Error("一個不可執行的檔案被當成找到了")
	}
}

// 解對了檔案還不夠 —— 那個檔案必須在**人的環境**裡被執行。
//
// ⚠ 實測 sampleagent3（2026-09-03）：openclaw 的 wrapper 第一行是
// `#!/usr/bin/env node`，所以它跑哪一個 node 完全由呼叫者的 PATH 決定。
// 用登入的 PATH 問 8 次，8 次都回 OpenClaw 2026.6.10；
// 用 daemon 的 PATH 問 6 次，6 次都回 "Node.js v22.19+ is required"。
// 同一個檔案、同一台機器，兩個環境兩個相反的答案 ——
// 一個說「裝著全機隊最新的版本」，一個說「它根本起不來」。
//
// 這支測試造一支「只有在 PATH 裡看得到某個目錄時才答得出版號」的假 CLI，
// 就是那個 wrapper 的行為的最小模型。
func TestTheToolIsRunInTheHumansEnvironmentNotTheDaemons(t *testing.T) {
	const name = "zzz-envcli"
	bin, marker, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()

	// ⚠ 這支假 CLI 模仿 `#!/usr/bin/env node`：它要的東西在 PATH 上才活得下來。
	script := "#!/bin/sh\ncase \":$PATH:\" in\n" +
		"*\":" + marker + ":\"*) echo 7.7.7 ;;\n" +
		"*) echo 'needs the marker dir on PATH' >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// daemon 自己的環境看不到 marker —— 就像 sampleagent3 的 agent 看不到 node24。
	t.Setenv("PATH", elsewhere)
	sp := searchPath{Dirs: []string{bin, marker}, Source: model.PathSourceLogin}

	got := cliTool(context.Background(), name, nil, model.ProcessScanUnavailable, sp)

	if got.VersionReported != "7.7.7" {
		t.Errorf("版號是 %q（理由：%q），該是 7.7.7。\n"+
			"這支 CLI 只有在 PATH 上看得到它要的目錄時才答得出來 —— 就像 sampleagent3 上的\n"+
			"openclaw 只有在找得到 node v24 時才起得來。解對了檔案卻在 daemon 的環境裡\n"+
			"跑它，量到的是一台「openclaw 起不來」的假機器。",
			got.VersionReported, got.VersionReason)
	}
}

// ⚠ 只換 PATH，其他環境變數照傳 —— HOME 尤其不可以掉。
// 這些工具的憑證跟設定都住在 $HOME；清掉它會得到一台「什麼都沒裝」的假機器。
func TestRunInKeepsTheRestOfTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s|%s' \"$HOME\" \"$PATH\"\n"
	path := filepath.Join(dir, "zzz-envdump")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/home/zzz-someone")

	out, _, err := runIn(context.Background(), quickTimeout, []string{dir, "/usr/bin"}, path)
	if err != nil {
		t.Fatalf("跑不起來：%v", err)
	}
	home, gotPath, _ := strings.Cut(out, "|")
	if home != "/home/zzz-someone" {
		t.Errorf("HOME 是 %q —— 換 PATH 不可以把其他環境變數一起丟掉。"+
			"憑證都住在 $HOME，丟了它每一台都會回報「沒有安裝任何憑證」", home)
	}
	if gotPath != dir+":/usr/bin" {
		t.Errorf("子行程的 PATH 是 %q，該是 %q", gotPath, dir+":/usr/bin")
	}
	// dirs 是空的時候要用自己的環境，不可以送一個空的 PATH 下去。
	out2, _, err := runIn(context.Background(), quickTimeout, nil, path)
	if err != nil {
		t.Fatalf("dirs=nil 時跑不起來：%v", err)
	}
	if _, p2, _ := strings.Cut(out2, "|"); p2 != os.Getenv("PATH") {
		t.Errorf("dirs 是空的時候子行程的 PATH 是 %q，該照用自己的 %q", p2, os.Getenv("PATH"))
	}
}

func TestRunInDoesNotPassSystemdNotificationChannelsToChildren(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/run/user/1000/notify")
	t.Setenv("WATCHDOG_PID", "42")
	t.Setenv("WATCHDOG_USEC", "30000000")
	out, _, err := runIn(context.Background(), quickTimeout, nil, "/bin/sh", "-c",
		`printf '%s|%s|%s' "${NOTIFY_SOCKET-}" "${WATCHDOG_PID-}" "${WATCHDOG_USEC-}"`)
	if err != nil {
		t.Fatalf("跑不起來：%v", err)
	}
	if out != "||" {
		t.Fatalf("systemd notification 控制變數流進子行程：%q", out)
	}
}

// 一個 process 只准問登入 shell 一次。
//
// ⚠ `bash -lc` 會把使用者整份 login profile 跑一遍，而那是有副作用的：
// 實測 samplehub1 的 agent log 裡出現 im-config，PPID 就是 agent 自己
// （Ubuntu 的 ~/.profile 會跑它）。別人的 .profile 裡還有 nvm、conda。
// 觀測平均 9 分鐘一輪 —— 一天 160 次別人的 login profile。
// **觀測不應該改變被觀測的機器。**
func TestTheLoginShellIsAskedOnlyOncePerProcess(t *testing.T) {
	// 造一支會數自己被叫了幾次的假 shell。
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	shell := filepath.Join(dir, "fake-shell")
	script := "#!/bin/sh\necho x >> " + counter + "\nprintf %s \"$PATH\"\n"
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)

	// ⚠ 快取是 package 層的，別的測試可能已經觸發過它 —— 所以這裡直接
	// 對著那個 sync.Once 的狀態驗證，而不是假設自己是第一個。
	loginPathOnce = struct {
		sync.Once
		sp searchPath
	}{}

	for i := 0; i < 5; i++ {
		cachedLoginSearchPath(context.Background())
	}
	b, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("那支假 shell 一次都沒被叫到：%v", err)
	}
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Errorf("登入 shell 被叫了 %d 次，該只有 1 次。\n"+
			"每一輪觀測都跑一次別人的 login profile，就是用觀測去改變被觀測的機器", n)
	}
}
