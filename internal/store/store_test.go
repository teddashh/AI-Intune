package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"
)

func TestCanonicalOperatorTimeAcceptsOnlyTheShapeTheHubWrites(t *testing.T) {
	hubWritten := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		value       time.Time
		want        bool
		consequence string
	}{
		{
			name: "hub written", value: hubWritten, want: true,
			consequence: "Hub 自己寫下的 UTC 整秒收據被拒絕，會讓真正的原始決定無法重放",
		},
		{
			name: "zero", value: time.Time{}, want: false,
			consequence: "Hub 接受了沒有寫入時間的收據，會把缺少身分時間的資料當成自己當初寫下的那一份",
		},
		{
			name: "sub second", value: hubWritten.Add(time.Millisecond), want: false,
			consequence: "Hub 接受了帶小數秒的收據，會把一份不符合自己秒級寫入形狀的收據當成原件",
		},
		{
			name: "same instant elsewhere", value: hubWritten.In(time.FixedZone("plus-eight", 8*3600)), want: false,
			consequence: "Hub 接受了同一時刻但是 +08:00 序列化形狀的收據；fmtTime 帳本比對看不出差別，會把 Hub 從來不會寫的收據當成原件",
		},
		{
			name: "same instant zero offset", value: hubWritten.In(time.FixedZone("zero-offset", 0)), want: false,
			consequence: "Hub 接受了同一時刻但是 +00:00 而非 Z 的序列化形狀；fmtTime 帳本比對會抹平差別，會把不是 Hub 寫下的收據認成原件",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := canonicalOperatorTime(test.value)
			if got != test.want {
				t.Errorf("canonicalOperatorTime(%v)=%t, expected %t; %s", test.value, got, test.want, test.consequence)
			}
		})
	}
}

func TestOpenEscapesSQLiteURIMetacharactersInLedgerPath(t *testing.T) {
	for _, name := range []string{
		"ledger?mode=memory.sqlite",
		"ledger%3fmode=memory.sqlite",
		"ledger#fragment.sqlite",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			st, err := Open(path)
			if err != nil {
				t.Fatalf("Open(%q): %v", path, err)
			}
			var sequence int
			var schema, openedPath string
			if err := st.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &schema, &openedPath); err != nil {
				_ = st.Close()
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if schema != "main" || filepath.Clean(openedPath) != path {
				t.Fatalf("SQLite opened %q (%s), want exact ledger %q", openedPath, schema, path)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("exact ledger path was not created: %v", err)
			}
		})
	}
}

// 測試一律打真的 SQLite 檔，不用 mock。
//
// ⚠ 這是刻意的。這一層的規則幾乎全部是 SQL 的行為 —— UPSERT 會不會產生重複列、
// append-only 有沒有真的 append、FK 擋不擋得住陌生機器。mock 掉資料庫等於把
// 唯一要測的東西測掉了。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustEnroll(t *testing.T, s *Store, name string, now time.Time) string {
	t.Helper()
	// Fixed-time fixtures must put token issuance/registry creation on the same
	// clock as the enrollment and later lifecycle transitions.
	previousNow := s.nowFn
	s.nowFn = func() time.Time { return now.UTC() }
	defer func() { s.nowFn = previousNow }()
	tok, err := s.CreateEnrollToken(name, time.Hour)
	if err != nil {
		t.Fatalf("create enroll token: %v", err)
	}
	id, _, err := s.RedeemEnrollToken(tok, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: tok,
		Hostname: name, OS: "linux", Arch: "amd64", UnixUser: "example-user",
	}, now)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return id
}

// healthyBatch 是一批「什麼問題都沒有」的觀測，讓機器判成 Online。
// 各個測試再從這裡改一兩個欄位去製造它要測的那一種壞掉。
func healthyBatch(measuredAt time.Time) model.ObservationBatch {
	lastTask := measuredAt.Add(-time.Hour)
	matched := true
	fingerprint, _, _ := expectationsPolicyFingerprint(nil)
	return model.ObservationBatch{
		SchemaVersion:       model.SchemaVersion,
		MeasuredAt:          measuredAt,
		WorkloadPolicyToken: workloadPolicyToken(fingerprint, 1, "samplehub1"),
		Identity: model.Identity{
			Hostname: "samplehub1", OS: "linux", Arch: "amd64", UnixUser: "example-user",
			MachineIDHint: "aaaaaaaaaaaa", BootID: "boot-1", LingerEnabled: true, LingerMeasured: true,
		},
		Resources: model.Resources{
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
			MemTotalBytes: 16 << 30, MemAvailableBytes: 8 << 30, CPUCount: 8, Load1m: func() *float64 { v := 0.4; return &v }(),
		},
		Systemd: []model.Unit{
			{Name: "openclaw.service", Present: true, ActiveState: "active", SubState: "running", NRestarts: 0},
		},
		OpenClaw: model.OpenClaw{
			Present: true, CLIVersion: "2026.6.1", GatewayVersion: "2026.6.1", UpstreamVersion: "2026.5.9",
			Install: &model.OpenClawInstall{
				UnitFound: true, MainPID: 4242, ProcessMatchesUnit: &matched,
				RunningDirVersion: "2026.6.1",
			},
			DB: &model.OpenClawDB{
				Present: true, Layout: "consolidated", Path: "~/.openclaw/state/openclaw.sqlite",
				LastTaskEndedAt: &lastTask, TaskRunRows: 120,
			},
		},
		Credentials: []model.Credential{
			{Provider: "claude", Status: model.CredConfigured},
		},
		CLITools: []model.CLITool{
			{Name: "openclaw", Present: true, Path: "/usr/bin/openclaw", VersionReported: "2026.6.1", RunningPID: 4242},
		},
	}
}

func healthyCheckin(sentAt time.Time) model.Checkin {
	jobsEnabled := true
	return model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: sentAt, AgentVersion: "0.1.0",
		BootID: "boot-1", AgentSeq: 1, UptimeSeconds: func() *int64 { v := int64(86400); return &v }(),
		DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
		JobsEnabled: &jobsEnabled,
	}
}

func TestUnmeasuredLingerStaysUnknownInFacts(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "unmeasured-linger", now.Add(-time.Hour))

	unmeasured := healthyBatch(now.Add(-2 * time.Minute))
	unmeasured.Identity.LingerEnabled = false
	unmeasured.Identity.LingerMeasured = false
	if err := s.RecordObservation(id, unmeasured, now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("寫入未量到 linger 的觀測失敗：%v", err)
	}
	facts, err := s.Facts(id, now)
	if err != nil {
		t.Fatalf("讀取未量到 linger 的 facts 失敗：%v", err)
	}
	if facts.LingerEnabled != nil {
		t.Errorf("未量到 linger 時 Facts.LingerEnabled = %v，預期 nil；否則畫面會把未知說成未開啟", *facts.LingerEnabled)
	}

	measured := healthyBatch(now.Add(-time.Minute))
	measured.Identity.LingerEnabled = false
	measured.Identity.LingerMeasured = true
	if err := s.RecordObservation(id, measured, now.Add(-time.Minute)); err != nil {
		t.Fatalf("寫入已量到但未開啟 linger 的觀測失敗：%v", err)
	}
	facts, err = s.Facts(id, now)
	if err != nil {
		t.Fatalf("讀取已量到 linger 的 facts 失敗：%v", err)
	}
	if facts.LingerEnabled == nil || *facts.LingerEnabled {
		t.Errorf("已量到且未開啟 linger 時 Facts.LingerEnabled = %v，預期非 nil 且為 false；否則真的未開啟會被畫成未知", facts.LingerEnabled)
	}
}

// TestProcessScanCrossPackageBinding 釘住跨套件契約：model.ProcessScan* 的字面值
// 必須正好等於 internal/state 判決層期待的封閉值域，且要完整穿過 Store。
func TestProcessScanCrossPackageBinding(t *testing.T) {
	for name, binding := range map[string]struct {
		got  string
		want string
	}{
		"Complete":    {model.ProcessScanComplete, "complete"},
		"Restricted":  {model.ProcessScanRestricted, "restricted"},
		"Unavailable": {model.ProcessScanUnavailable, "unavailable"},
	} {
		if binding.got != binding.want {
			t.Errorf("model.ProcessScan%s = %q，want %q；internal/state/state.go 的 evaluateWorkload 用字面值比對，改常數就要同步改那裡", name, binding.got, binding.want)
		}
	}

	s := newTestStore(t)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now)
	b := healthyBatch(now)
	b.CLITools = []model.CLITool{{
		Name:          "openclaw",
		Present:       true,
		Path:          "/usr/bin/openclaw",
		RunningPID:    0,
		ProcessScan:   model.ProcessScanUnavailable,
		RunningReason: "process 偵測沒有跑到",
	}}
	if err := s.RecordObservation(id, b, now); err != nil {
		t.Fatalf("record observation: %v", err)
	}

	facts, err := s.Facts(id, now)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if facts.OpenClawProcessScan != model.ProcessScanUnavailable {
		t.Fatalf("process scan = %q，want %q", facts.OpenClawProcessScan, model.ProcessScanUnavailable)
	}
	for _, finding := range state.WorkloadFindings(facts) {
		if strings.Contains(finding.Message, "process 不存在") {
			t.Errorf("typed unavailable 穿過 Store 後仍被判成 process 不在：%+v", finding)
		}
	}

	completeAt := now.Add(time.Minute)
	complete := healthyBatch(completeAt)
	complete.CLITools = []model.CLITool{{
		Name:          "openclaw",
		Present:       true,
		Path:          "/usr/bin/openclaw",
		RunningPID:    0,
		ProcessScan:   model.ProcessScanComplete,
		RunningReason: "掃了 312 個 process，沒有一個對得上",
	}}
	if err := s.RecordObservation(id, complete, completeAt); err != nil {
		t.Fatalf("record complete observation: %v", err)
	}
	completeFacts, err := s.Facts(id, completeAt)
	if err != nil {
		t.Fatalf("complete facts: %v", err)
	}
	var absent bool
	for _, finding := range state.WorkloadFindings(completeFacts) {
		if strings.Contains(finding.Message, "process 不存在") {
			absent = true
		}
	}
	if !absent {
		t.Errorf("typed complete 穿過 Store 後沒有接上缺席判決：%+v", state.WorkloadFindings(completeFacts))
	}
}

// TestDaemonReachAndPathSourceCrossPackageBinding 是 TestProcessScanCrossPackageBinding
// 的姊妹，理由同一個常數區註解。
//
// ⚠ 實測：逐一把這五個常數的字面值改成 xxx-x，跑全樹 go test ./... -count=1：
//   - DaemonReachShadowed → 紅的是 TestAShadowedBinaryShowsBothFilesOnScreen
//   - DaemonReachMissing → 紅的是 TestAVersionReadOffDiskIsNotPresentedAsTheToolAnswering
//   - PathSourceDaemon → 紅的是 TestAToolMeasuredInTheWrongEnvironmentSaysSo
//   - PathSourceLogin → 紅的是上面那兩支
//   - DaemonReachSame → 全樹全綠
//
// ⚠ 那四個是被網頁測試順帶守到的：它們斷言的是渲染出來的字，不是字面值契約。
// machine.html 沒有任何分支用到 "same"，所以 "same" 是唯一沒有被順帶守到的——
// 而它跟另外四個一樣會被寫進 observed_state。
//
// ⚠ 整個測試樹都用常數寫 payload，所以改名時測試會跟著一起移動；只有真正躺在
// 資料庫裡的舊 payload 帶著舊字面值。第二個子測試故意用硬寫字串就是為了站在舊
// payload 那一邊。
//
// ⚠ 實測隔離盤（這一刀落地後，全樹 go test ./... -count=1，逐一改字面值）：
//   - DaemonReachSame → 兩個子測試都紅、others=[]。這一支是唯一的看守者，
//     而且 a_payload_written_with_literals_still_reads_back 真的會咬，
//     不是字面值比對的套套邏輯。
//   - PathSourceLogin → 兩個子測試都紅，另有既有的兩支網頁測試紅。
//   - DaemonReachShadowed／DaemonReachMissing／PathSourceDaemon →
//     只有 literals 那一格紅（這支的 payload 沒用到那三個值），
//     另有各自既有的網頁測試紅。那三格不是這一刀的隔離證據，
//     它本來就有人守；這一刀在那三格的貢獻是把契約寫明，不是唯一覆蓋。
//   - 對照組 ProcessScanComplete → 本測試兩格全綠，
//     只有姊妹那支 TestProcessScanCrossPackageBinding 等紅。
//     沒有順手焊到隔壁的值域。
func TestDaemonReachAndPathSourceCrossPackageBinding(t *testing.T) {
	t.Run("literals", func(t *testing.T) {
		for name, binding := range map[string]struct {
			got  string
			want string
		}{
			"DaemonReachSame":     {model.DaemonReachSame, "same"},
			"DaemonReachShadowed": {model.DaemonReachShadowed, "shadowed"},
			"DaemonReachMissing":  {model.DaemonReachMissing, "missing"},
			"PathSourceLogin":     {model.PathSourceLogin, "login"},
			"PathSourceDaemon":    {model.PathSourceDaemon, "daemon"},
		} {
			if binding.got != binding.want {
				t.Errorf("model.%s = %q，want %q；這些字面值會被序列化進 observed_state 的 payload 躺很久，且 machine.html 用硬寫字面值比對 shadowed、missing、daemon，改常數不會讓 Go 編譯失敗", name, binding.got, binding.want)
			}
		}
	})

	t.Run("a payload written with literals still reads back", func(t *testing.T) {
		st := newTestStore(t)
		now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
		machineID := mustEnroll(t, st, "samplehub1", now)
		batch := healthyBatch(now)
		batch.CLITools = []model.CLITool{{
			Name: "openclaw", Present: true, OnPath: true, Path: "/usr/bin/openclaw",
			PresentEvidence: "path", DaemonReach: "same", PathSource: "login",
			DaemonPath: "/usr/bin/openclaw", RunningPID: 4242,
		}}
		if err := st.RecordObservation(machineID, batch, now); err != nil {
			t.Fatalf("record observation: %v", err)
		}

		evidence, err := st.MachineReadEvidence(machineID, now, 10)
		if err != nil {
			t.Fatalf("machine evidence: %v", err)
		}
		if evidence.CLIToolsInvalid != 0 || len(evidence.CLITools) != 1 {
			t.Fatalf("CLIToolsInvalid=%d len(CLITools)=%d，want 0 and 1；常數字面值一改，validMachineReadCLITool 的封閉值域就不再包含舊 payload 的字，整列會被算進 CLIToolsInvalid 並從 Items 消失，歷史觀測會在沒有任何測試變紅的情況下被吞掉", evidence.CLIToolsInvalid, len(evidence.CLITools))
		}
		if evidence.CLITools[0].DaemonReach != "same" {
			t.Errorf("DaemonReach = %q，want %q；舊 payload 的字面值必須穿過 validMachineReadCLITool，否則歷史觀測會被吞掉", evidence.CLITools[0].DaemonReach, "same")
		}
		if evidence.CLITools[0].PathSource != "login" {
			t.Errorf("PathSource = %q，want %q；舊 payload 的字面值必須穿過 validMachineReadCLITool，否則歷史觀測會被吞掉", evidence.CLITools[0].PathSource, "login")
		}
	})
}

// TestCredentialStatusCrossPackageBinding 釘住跨套件契約：model.Cred* 的字面值
// 必須正好等於 internal/state 判決層期待的封閉值域，且 expires_soon 要完整穿過 Store。
func TestCredentialStatusCrossPackageBinding(t *testing.T) {
	for name, binding := range map[string]struct {
		got  model.CredStatus
		want string
	}{
		"Absent":     {model.CredAbsent, "absent"},
		"Configured": {model.CredConfigured, "configured"},
		"Expired":    {model.CredExpired, "expired"},
		"Unknown":    {model.CredUnknown, "unknown"},
		"Failed":     {model.CredFailed, "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			if string(binding.got) != binding.want {
				t.Errorf("model.Cred%s = %q，want %q；internal/state/state.go 用字面值比對，改常數就要同步改那裡", name, binding.got, binding.want)
			}
		})
	}

	t.Run("ExpiresSoon", func(t *testing.T) {
		if model.CredExpiresSoon != "expires_soon" {
			t.Errorf("model.CredExpiresSoon = %q，want %q；internal/state/state.go 用字面值比對，改常數就要同步改那裡", model.CredExpiresSoon, "expires_soon")
		}

		s := newTestStore(t)
		now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
		id := mustEnroll(t, s, "samplehub1", now)
		if err := s.RecordCheckin(id, healthyCheckin(now), now); err != nil {
			t.Fatalf("record checkin: %v", err)
		}
		b := healthyBatch(now)
		b.Credentials = []model.Credential{{Provider: "claude", Status: model.CredExpiresSoon}}
		if err := s.RecordObservation(id, b, now); err != nil {
			t.Fatalf("record observation: %v", err)
		}

		facts, err := s.Facts(id, now)
		if err != nil {
			t.Fatalf("facts: %v", err)
		}
		j := state.Derive(facts)
		var found bool
		for _, finding := range j.Findings {
			if finding.Kind == "credential" && strings.Contains(finding.Message, "claude 的登入即將過期") {
				found = true
			}
		}
		if !found {
			t.Errorf("model.CredExpiresSoon 穿過 Store 後沒有接上即將過期判決：%+v", j.Findings)
		}
	})
}

func stampCurrentWorkloadPolicy(t *testing.T, s *Store, displayName string, b *model.ObservationBatch) {
	t.Helper()
	token, err := s.CurrentWorkloadPolicyToken(displayName)
	if err != nil {
		t.Fatalf("workload policy token: %v", err)
	}
	b.WorkloadPolicyToken = token
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// ---------------------------------------------------------------- enrollment

func TestEnrollAuthenticateAndBurn(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)

	tok, err := s.CreateEnrollToken("samplehub1", time.Hour)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if tok == "" {
		t.Fatal("enroll token is empty")
	}
	// ⚠ 資料庫裡不可以有 token 明文。
	if n := countRows(t, s, `SELECT COUNT(*) FROM enrollment_tokens WHERE token_hash = ?`, tok); n != 0 {
		t.Fatal("enroll token stored in plaintext")
	}

	req := model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: tok,
		Hostname: "samplehub1", MachineIDHint: "aaaaaaaaaaaa", UnixUser: "example-user",
		OS: "linux", Arch: "amd64", AgentVersion: "0.1.0",
	}
	machineID, agentTok, err := s.RedeemEnrollToken(tok, req, now)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if machineID == "" || agentTok == "" {
		t.Fatalf("empty identity: machine=%q token=%q", machineID, agentTok)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM machine_registry WHERE agent_token_hash = ?`, agentTok); n != 0 {
		t.Fatal("agent token stored in plaintext")
	}

	got, err := s.AuthenticateAgent(agentTok)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got != machineID {
		t.Fatalf("authenticate returned %q, want %q", got, machineID)
	}

	// 錯的 token 一律拒絕。
	for _, bad := range []string{"", "not-a-token", agentTok + "x", agentTok[:len(agentTok)-1]} {
		if _, err := s.AuthenticateAgent(bad); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("bad token %q accepted (err=%v)", bad, err)
		}
	}

	// ⚠ 用過即焚：同一張 token 不能換到第二個身分。
	_, _, err = s.RedeemEnrollToken(tok, req, now.Add(time.Minute))
	if !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("second redeem err = %v, want ErrTokenUsed", err)
	}
	if !errors.Is(err, ErrEnrollToken) {
		t.Fatalf("ErrTokenUsed does not wrap ErrEnrollToken")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM machine_registry`); n != 1 {
		t.Fatalf("registry has %d machines after double redeem, want 1", n)
	}

	// 不存在的 token 與過期的 token。
	if _, _, err := s.RedeemEnrollToken("nope", req, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("unknown token err = %v, want ErrTokenInvalid", err)
	}
	tok2, err := s.CreateEnrollToken("sampleagent1", time.Minute)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if _, _, err := s.RedeemEnrollToken(tok2, req, s.now().Add(2*time.Minute)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired token err = %v, want ErrTokenExpired", err)
	}
}

func TestRetiredMachineCannotAuthenticateButKeepsHistory(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	tok, _ := s.CreateEnrollToken("sampleagent3", time.Hour)
	id, agentTok, err := s.RedeemEnrollToken(tok, model.EnrollRequest{EnrollToken: tok, Hostname: "sampleagent3"}, now)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if err := s.RecordCheckin(id, healthyCheckin(now), now); err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if err := s.RetireMachine(id, now.Add(time.Hour)); err != nil {
		t.Fatalf("retire: %v", err)
	}

	if _, err := s.AuthenticateAgent(agentTok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("retired machine still authenticates (err=%v)", err)
	}
	// ⚠ 離開分母，但歷史留著。retire 不是刪除。
	if n := countRows(t, s, `SELECT COUNT(*) FROM machine_checkins WHERE machine_id = ?`, id); n != 1 {
		t.Fatalf("retire deleted history: %d checkin rows", n)
	}
	if _, err := s.Detail(id, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("retired machine detail unreadable: %v", err)
	}
	ov, err := s.Overview(now.Add(2 * time.Hour))
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.Total != 0 {
		t.Fatalf("retired machine still counted in overview: total=%d", ov.Total)
	}
	if err := s.RetireMachine("no-such-machine", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retire unknown err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------- 名冊是分母

// TestFiveRegisteredFourReportingTheFifthMustNotVanish 是這個檔案裡最重要的測試。
//
// 名冊有 5 台、只有 4 台報到，那 1 台亮紅燈 —— 不是從畫面上消失。
// 整個產品就是這一句話。實測案例：sampleagent3 在某台 client 上隱形了七週，
// 沒有人發現，因為它沒有出現在任何一個清單裡。
func TestFiveRegisteredFourReportingTheFifthMustNotVanish(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	names := []string{"samplehub1", "sampleagent1", "sampleagent2", "sampleagent4", "sampleagent3"}

	ids := map[string]string{}
	for _, n := range names {
		ids[n] = mustEnroll(t, s, n, now.Add(-24*time.Hour))
	}
	// sampleagent3 從來沒有報到過 —— 它在名冊上，但你不知道它怎麼了。
	silent := "sampleagent3"
	for _, n := range names {
		if n == silent {
			continue
		}
		if err := s.RecordCheckin(ids[n], healthyCheckin(now.Add(-30*time.Second)), now.Add(-30*time.Second)); err != nil {
			t.Fatalf("%s checkin: %v", n, err)
		}
		if err := s.RecordObservation(ids[n], healthyBatch(now.Add(-time.Minute)), now.Add(-time.Minute)); err != nil {
			t.Fatalf("%s observation: %v", n, err)
		}
	}

	ov, err := s.Overview(now)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.Total != 5 || ov.Expected != 5 {
		t.Fatalf("overview total=%d expected=%d, want 5/5 — 分母不是「有回報的機器數」", ov.Total, ov.Expected)
	}
	if len(ov.Machines) != 5 {
		t.Fatalf("overview lists %d machines, want 5 — 沒報到的那台被吃掉了", len(ov.Machines))
	}
	if ov.Counts[state.NeverReported] != 1 {
		t.Fatalf("NeverReported count = %d, want 1 (counts=%v)", ov.Counts[state.NeverReported], ov.Counts)
	}
	if ov.Counts[state.Online] != 4 {
		t.Fatalf("Online count = %d, want 4 (counts=%v)", ov.Counts[state.Online], ov.Counts)
	}

	var deadRow *MachineRow
	for i := range ov.Machines {
		if ov.Machines[i].DisplayName == silent {
			deadRow = &ov.Machines[i]
		}
	}
	if deadRow == nil {
		t.Fatal("sampleagent3 是名冊上的一台，但它不在 Overview.Machines 裡 —— 這就是這個產品存在的理由")
	}
	if deadRow.State != state.NeverReported {
		t.Fatalf("silent machine state = %q, want NeverReported", deadRow.State)
	}
	if deadRow.Reason == "" {
		t.Fatal("NeverReported 這盞燈沒有理由 —— UI 上不准出現沒有句子的燈")
	}
	if deadRow.Facts.EverCheckedIn {
		t.Fatal("silent machine reported as having checked in")
	}
	if !deadRow.Expected {
		t.Fatal("silent machine dropped out of the denominator")
	}
	// 最該看的排最上面。
	if ov.Machines[0].DisplayName != silent {
		t.Fatalf("most severe row is %q, want %q", ov.Machines[0].DisplayName, silent)
	}
	// 而且點得進去。
	d, err := s.Detail(deadRow.MachineID, now)
	if err != nil {
		t.Fatalf("silent machine detail: %v", err)
	}
	if d.State != state.NeverReported || d.Reason == "" {
		t.Fatalf("detail state=%q reason=%q", d.State, d.Reason)
	}

	// StateCounts 要照嚴重度排。
	sc := ov.StateCounts()
	if len(sc) == 0 || sc[0].State != state.NeverReported {
		t.Fatalf("StateCounts not sorted by severity: %+v", sc)
	}
}

func TestDenominatorCountsIncludesEveryActiveMachineAndExcludesRetired(t *testing.T) {
	retiredAt := time.Now().UTC()
	ov := Overview{Machines: []MachineRow{
		{Machine: Machine{Expected: true}, State: state.Online},
		{Machine: Machine{Expected: true}, State: state.Unreachable},
		{Machine: Machine{Expected: false}, State: state.Online},
		{Machine: Machine{Expected: true, RetiredAt: &retiredAt}, State: state.Unreachable},
	}}
	counts := ov.DenominatorCounts()
	if counts[state.Online] != 2 || counts[state.Unreachable] != 1 {
		t.Fatalf("denominator counts=%v，want online=2 unreachable=1", counts)
	}
	if got := counts[state.Online] + counts[state.Unreachable]; got != 3 {
		t.Fatalf("denominator total=%d，want every active row and no retired row", got)
	}
}

func TestReportingCountUsesHeartbeatFreshnessAndDenominator(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	fresh := state.Facts{EverCheckedIn: true, LastCheckinReceived: now.Add(-time.Minute), CheckinInterval: state.CheckinInterval}
	stale := state.Facts{EverCheckedIn: true, LastCheckinReceived: now.Add(-time.Hour), CheckinInterval: state.CheckinInterval}
	ov := Overview{Now: now, Machines: []MachineRow{
		{Machine: Machine{Expected: true}, State: state.IdentityConflict, Facts: fresh},
		{Machine: Machine{Expected: true}, State: state.IdentityConflict, Facts: stale},
		{Machine: Machine{Expected: false}, State: state.Online, Facts: fresh},
	}}
	if got := ov.ReportingCount(); got != 2 {
		t.Fatalf("ReportingCount=%d, want both fresh active rows", got)
	}
}

// ---------------------------------------------------------------- 心跳冪等

// 5 台機器在網路抖動時重試同一個心跳，不可以長出重複的列。
// 重複列會讓 COUNT(DISTINCT boot_id) 這種查詢開始說謊。
func TestCheckinIsUpsertOnly(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now.Add(-time.Hour))

	sentAt := now.Add(-time.Minute)
	c := healthyCheckin(sentAt)
	if err := s.RecordCheckin(id, c, now.Add(-time.Minute)); err != nil {
		t.Fatalf("first checkin: %v", err)
	}
	// 同一個 sent_at 重送三次，received_at 一次比一次晚（就是重試的樣子）。
	c.AgentSeq = 7
	jobsEnabled := false
	c.JobsEnabled = &jobsEnabled
	c.DeviceSyncV1 = true
	for i := 1; i <= 3; i++ {
		if err := s.RecordCheckin(id, c, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM machine_checkins WHERE machine_id = ?`, id); n != 1 {
		t.Fatalf("%d checkin rows after 4 sends of the same sent_at, want 1", n)
	}
	// payload 會更新…
	var seq int64
	var storedJobsEnabled bool
	var received string
	if err := s.DB().QueryRow(
		`SELECT agent_seq, jobs_enabled, received_at FROM machine_checkins WHERE machine_id = ?`, id).
		Scan(&seq, &storedJobsEnabled, &received); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if seq != 7 || storedJobsEnabled {
		t.Fatalf("upsert payload agent_seq=%d jobs_enabled=%t", seq, storedJobsEnabled)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM machine_job_capabilities
	 WHERE machine_id=? AND sent_at=? AND capability=? AND supported=1`,
		id, fmtTime(sentAt), model.DeviceSyncJobKind); got != 1 {
		t.Fatalf("device-sync capability rows=%d", got)
	}
	// …但 received_at 保留第一次收到的時間，否則時鐘偏移的量測會被重試污染。
	if got, want := parseTime(received), now.Add(-time.Minute); !got.Equal(want) {
		t.Fatalf("received_at = %s, want %s (第一次收到的時間才是量測值)", got, want)
	}

	// 陌生機器寫不進來（FK），而且錯誤要看得懂。
	if err := s.RecordCheckin("ghost", healthyCheckin(now), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("checkin from unknown machine err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------- 憑證續期史

func TestLatestBySubjectCutoffUsesSinglePassAndStableTieBreak(t *testing.T) {
	s := newTestStore(t)
	cutoff := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "windowed-credentials", cutoff.Add(-24*time.Hour))
	type evidence struct {
		observationID string
		subject       string
		payload       string
		measuredAt    time.Time
		receivedAt    time.Time
	}
	for _, row := range []evidence{
		{"old", "claude", "old", cutoff.Add(-2 * time.Hour), cutoff.Add(-2 * time.Hour)},
		{"tie-old", "claude", "tie-old", cutoff.Add(-time.Hour), cutoff.Add(-30 * time.Minute)},
		// Same measured/received time: the later rowid is the deterministic winner.
		{"tie-winner", "claude", "tie-winner", cutoff.Add(-time.Hour), cutoff.Add(-30 * time.Minute)},
		// A higher agent time received after the evaluation instant must not win.
		{"future", "claude", "future", cutoff.Add(3 * time.Hour), cutoff.Add(time.Second)},
		{"grok", "grok", "grok-winner", cutoff.Add(-time.Minute), cutoff.Add(-time.Minute)},
	} {
		if _, err := s.DB().Exec(`INSERT INTO observed_state
 (observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
 VALUES(?,?,?,?,?,?,?,?)`, row.observationID, id, fmtTime(row.measuredAt), fmtTime(row.receivedAt),
			KindCredential, row.subject, row.payload, SourceAgentMeasurement); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.latestBySubject(id, KindCredential, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Subject != "claude" || got[0].Payload != "tie-winner" ||
		got[1].Subject != "grok" || got[1].Payload != "grok-winner" {
		t.Fatalf("latest evidence at cutoff = %+v", got)
	}

	// Guard the production performance property as a query-shape invariant,
	// not a flaky wall-clock assertion. The previous correlated MAX rescanned
	// each subject history for every outer row and made Overview take 11s on the
	// real ledger; the ranked query must touch observed_state through one plan node.
	plan, err := s.DB().Query(`EXPLAIN QUERY PLAN `+latestBySubjectAtSQL,
		id, KindCredential, fmtTime(cutoff))
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	observedNodes := 0
	for plan.Next() {
		var node, parent, unused int
		var detail string
		if err := plan.Scan(&node, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		canonicalDetail := strings.ToUpper(detail)
		if strings.Contains(canonicalDetail, "CORRELATED") {
			t.Fatalf("latest-by-subject plan regained correlated rescan: %s", detail)
		}
		if strings.Contains(canonicalDetail, "OBSERVED_STATE") {
			observedNodes++
		}
	}
	if err := plan.Err(); err != nil {
		t.Fatal(err)
	}
	if observedNodes != 1 {
		t.Fatalf("latest-by-subject plan has %d observed_state nodes, want one", observedNodes)
	}
}

// ⚠⚠ 目前事實是 Hub 收到的最後一筆，不是 agent 宣稱量得最晚的那一筆。
func TestLatestObservationPrefersTheHubClockOverAFutureAgentClock(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "future-agent-clock", now.Add(-2*time.Hour))
	poisonedMeasuredAt := now.AddDate(1, 0, 0)

	poisoned := healthyBatch(poisonedMeasuredAt)
	poisoned.OpenClaw.Install.RunningDirVersion = "2026.9.1"
	if err := s.RecordObservation(id, poisoned, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	honest := healthyBatch(now)
	honest.OpenClaw.Install.RunningDirVersion = "2026.9.2"
	if err := s.RecordObservation(id, honest, now); err != nil {
		t.Fatal(err)
	}

	got, err := s.LatestOpenClawInstallObservationsAt([]string{id}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got[id].Install == nil || got[id].Install.RunningDirVersion != "2026.9.2" {
		t.Fatalf("latest OpenClaw install = %+v, want honest 2026.9.2 observation", got[id])
	}
	var storedMeasuredAt string
	if err := s.DB().QueryRow(`SELECT measured_at FROM observed_state
 WHERE machine_id=? AND kind=? AND subject=? AND received_at=?`,
		id, KindOpenClaw, KindOpenClaw, fmtTime(now.Add(-time.Hour))).Scan(&storedMeasuredAt); err != nil {
		t.Fatal(err)
	}
	if storedMeasuredAt != fmtTime(poisonedMeasuredAt) {
		t.Fatalf("poisoned measured_at = %q, want ledger-preserved %q", storedMeasuredAt, fmtTime(poisonedMeasuredAt))
	}
}

// ⚠⚠ 目前事實是 Hub 收到的最後一筆，不是 agent 宣稱量得最晚的那一筆。
func TestFleetToolsPrefersTheHubClockOverAFutureAgentClock(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "future-tool-clock", now.Add(-2*time.Hour))

	poisoned := healthyBatch(now.AddDate(1, 0, 0))
	poisoned.CLITools[0].VersionReported = "2026.9.1"
	if err := s.RecordObservation(id, poisoned, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	honest := healthyBatch(now)
	honest.CLITools[0].VersionReported = "2026.9.2"
	if err := s.RecordObservation(id, honest, now); err != nil {
		t.Fatal(err)
	}

	got, err := s.FleetTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MachineID != id || got[0].Name != "openclaw" || got[0].VersionReported != "2026.9.2" {
		t.Fatalf("FleetTools = %+v, want honest 2026.9.2 observation", got)
	}
}

// Facts 要帶「這張票自己續了幾次」—— 那是 observed_state append-only 才答得出來的
// 問題，而且是 SPEC §4.3 講的「看 last_refresh 有沒有卡住」的唯一材料。
//
// 三次觀測、兩個不同的 file_mtime → 續了 1 次。同一個 mtime 看到兩次不算兩次。
// 讀不到檔案（沒有 mtime）的觀測不算一次「續」。
func TestFactsCarryCredentialRefreshHistory(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))

	mt1 := base.Add(-9 * time.Hour)
	mt2 := base.Add(-time.Hour)
	obs := func(at time.Time, mt *time.Time) {
		b := healthyBatch(at)
		b.Credentials = []model.Credential{{
			Provider: "claude", Status: model.CredConfigured,
			ExpiresAt: ptrTime(at.Add(8 * time.Hour)), FileMTime: mt,
		}}
		if err := s.RecordObservation(id, b, at); err != nil {
			t.Fatalf("observation @%s: %v", at, err)
		}
	}
	obs(base.Add(-8*time.Hour), &mt1)
	obs(base.Add(-7*time.Hour), &mt1) // 同一個 mtime 再看到一次，不是一次續
	obs(base.Add(-30*time.Minute), &mt2)

	f, err := s.Facts(id, base)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if len(f.Credentials) != 1 {
		t.Fatalf("credentials = %+v, want 1", f.Credentials)
	}
	c := f.Credentials[0]
	if c.RefreshesSeen != 1 {
		t.Errorf("RefreshesSeen = %d, want 1（兩個不同的 mtime = 續了一次）", c.RefreshesSeen)
	}
	if want := 8 * time.Hour; c.WatchedFor != want {
		t.Errorf("WatchedFor = %s, want %s（第一筆觀測到現在）", c.WatchedFor, want)
	}
	// 壽命 = 到期 − 檔案寫入。最新那筆：expires = at+8h，mtime = base−1h，at = base−30m → 8.5h。
	if want := 8*time.Hour + 30*time.Minute; c.Lifetime != want {
		t.Errorf("Lifetime = %s, want %s", c.Lifetime, want)
	}
	if c.FileMTime == nil || !c.FileMTime.Equal(mt2) {
		t.Errorf("FileMTime = %v, want %s", c.FileMTime, mt2)
	}

	// Typed evidence 那一列也要有同一組數字 —— 畫面跟判決不准各說各話。
	evidence, err := s.MachineReadEvidence(id, base, MaxMachineEvidencePageSize)
	if err != nil {
		t.Fatalf("machine evidence: %v", err)
	}
	if len(evidence.Credentials) != 1 || evidence.Credentials[0].RefreshesSeen != 1 ||
		evidence.Credentials[0].WatchedFor != 8*time.Hour {
		t.Errorf("typed credential row = %+v, want refreshes 1 / watched 8h", evidence.Credentials)
	}

	// 一張從頭到尾讀不到檔案的票：0 次，而且沒有壽命。
	obs2 := healthyBatch(base.Add(-time.Minute))
	obs2.Credentials = []model.Credential{{Provider: "gemini", Status: model.CredUnknown}}
	if err := s.RecordObservation(id, obs2, base.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	f, _ = s.Facts(id, base)
	for _, c := range f.Credentials {
		if c.Provider == "gemini" && (c.RefreshesSeen != 0 || c.Lifetime != 0) {
			t.Errorf("讀不到檔案的票不該有續期次數或壽命：%+v", c)
		}
	}
}

// TestCredentialPeersOnlyUseOtherActiveMachinesWithRefreshes 守的是三種很像、
// 但證據價值完全不同的資料：同一家在別台真的換過 file_mtime、只是不斷看到
// 同一個 mtime，以及已經退役機器留下的歷史。後兩種都不准拿來指向這台的
// session；不然一句跨機器推論會偷渡沒有量到的事實。
func TestCredentialPeersOnlyUseOtherActiveMachinesWithRefreshes(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	stuckID := mustEnroll(t, s, "sampleagent2", base.Add(-4*24*time.Hour))
	activeID := mustEnroll(t, s, "samplehub1", base.Add(-4*24*time.Hour))
	retiredID := mustEnroll(t, s, "oldbox", base.Add(-4*24*time.Hour))

	record := func(id string, at, mt time.Time) {
		t.Helper()
		exp := mt.Add(8 * time.Hour)
		b := healthyBatch(at)
		b.Credentials = []model.Credential{{
			Provider: "claude", Status: model.CredConfigured, ExpiresAt: &exp, FileMTime: &mt,
		}}
		if err := s.RecordObservation(id, b, at); err != nil {
			t.Fatalf("寫入 %s 在 %s 的觀測：%v", id, at, err)
		}
	}

	stuckMTime := base.Add(-3 * 24 * time.Hour)
	record(stuckID, base.Add(-3*24*time.Hour), stuckMTime)
	record(stuckID, base.Add(-time.Hour), stuckMTime)

	for _, age := range []time.Duration{25 * time.Hour, 17 * time.Hour, 9 * time.Hour, time.Hour} {
		mt := base.Add(-age)
		record(activeID, base.Add(-age+time.Minute), mt)
		mtRetired := mt.Add(-time.Minute)
		record(retiredID, base.Add(-age+2*time.Minute), mtRetired)
	}
	if err := s.RetireMachine(retiredID, base.Add(-30*time.Minute)); err != nil {
		t.Fatalf("退役 oldbox：%v", err)
	}

	stuckFacts, err := s.Facts(stuckID, base)
	if err != nil {
		t.Fatalf("讀取卡住機器的事實：%v", err)
	}
	if len(stuckFacts.Credentials) != 1 || len(stuckFacts.Credentials[0].Peers) != 1 {
		t.Fatalf("卡住那台的同儕 = %+v，應該只留下未退役且真的續過的 samplehub1", stuckFacts.Credentials)
	}
	peer := stuckFacts.Credentials[0].Peers[0]
	if peer.DisplayName != "samplehub1" || peer.RefreshesSeen != 3 || peer.FileMTime == nil || !peer.FileMTime.Equal(base.Add(-time.Hour)) {
		t.Errorf("同儕續期史 = %+v，應該是 samplehub1 續了 3 次、最近一次在一小時前", peer)
	}

	activeFacts, err := s.Facts(activeID, base)
	if err != nil {
		t.Fatalf("讀取有續期機器的事實：%v", err)
	}
	if len(activeFacts.Credentials) != 1 || len(activeFacts.Credentials[0].Peers) != 0 {
		t.Fatalf("有在續的 samplehub1 不該把卡住或退役機器當同儕：%+v", activeFacts.Credentials)
	}

	evidence, err := s.MachineReadEvidence(stuckID, base, MaxMachineEvidencePageSize)
	if err != nil {
		t.Fatalf("讀取卡住機器的 typed evidence：%v", err)
	}
	if len(evidence.Credentials) != 1 || len(evidence.Credentials[0].Peers) != 1 ||
		evidence.Credentials[0].Peers[0].DisplayName != "samplehub1" {
		t.Errorf("typed evidence 與 Facts 應該拿到同一份同儕證據：%+v", evidence.Credentials)
	}
}

// ---------------------------------------------------------------- 觀測 append-only

// 兩次觀測 = 兩列，兩列都查得到，Detail 顯示新的那一列。
//
// ⚠ 如果有人把 RecordObservation 改成「一台一列的快照」，這個測試會紅。
// 它紅了不是測試壞了 —— 是「相對昨天的變化」那個畫面壞了。
func TestObservationIsAppendOnly(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))

	first := healthyBatch(base.Add(-20 * time.Minute))
	first.OpenClaw.CLIVersion = "2026.5.9"
	first.CLITools[0].VersionReported = "2026.5.9"
	first.Credentials[0].Status = model.CredConfigured
	if err := s.RecordObservation(id, first, base.Add(-20*time.Minute)); err != nil {
		t.Fatalf("first observation: %v", err)
	}

	second := healthyBatch(base.Add(-2 * time.Minute))
	second.OpenClaw.CLIVersion = "2026.6.1"
	second.CLITools[0].VersionReported = "2026.6.1"
	second.Credentials[0].Status = model.CredExpiresSoon
	if err := s.RecordObservation(id, second, base.Add(-2*time.Minute)); err != nil {
		t.Fatalf("second observation: %v", err)
	}

	for _, kind := range []string{KindIdentity, KindResources, KindOpenClaw} {
		if n := countRows(t, s,
			`SELECT COUNT(*) FROM observed_state WHERE machine_id = ? AND kind = ?`, id, kind); n != 2 {
			t.Fatalf("kind %q has %d rows after two batches, want 2 — 觀測被覆蓋了", kind, n)
		}
	}
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM observed_state WHERE machine_id = ? AND kind = ? AND subject = 'claude'`,
		id, KindCredential); n != 2 {
		t.Fatalf("credential history collapsed to %d rows, want 2", n)
	}
	// 舊的那一列還原封不動 —— Hub 是唯一的歷史。
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM observed_state WHERE machine_id = ? AND kind = ?
		   AND payload LIKE '%"cli_version":"2026.5.9"%'`,
		id, KindOpenClaw); n != 1 {
		t.Fatal("舊的 OpenClaw 觀測不見了")
	}

	d, err := s.Detail(id, base)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	evidence, err := s.MachineReadEvidence(id, base, MaxMachineEvidencePageSize)
	if err != nil {
		t.Fatalf("machine evidence: %v", err)
	}
	if len(evidence.SystemdUnits) != 1 || evidence.SystemdUnits[0].Name != "openclaw.service" {
		t.Fatalf("typed systemd units = %+v", evidence.SystemdUnits)
	}
	if len(evidence.Credentials) != 1 || evidence.Credentials[0].Status != model.CredExpiresSoon {
		t.Fatalf("typed credentials = %+v, want newest (expires_soon)", evidence.Credentials)
	}
	if !evidence.OpenClaw.Decoded || evidence.OpenClaw.CLIVersion != "2026.6.1" {
		t.Fatalf("typed OpenClaw = %+v, want newest (2026.6.1)", evidence.OpenClaw)
	}
	if len(evidence.CLITools) != 1 || evidence.CLITools[0].VersionReported != "2026.6.1" {
		t.Fatalf("typed CLI tools = %+v, want newest", evidence.CLITools)
	}
	if len(d.Checkins) != 0 {
		t.Fatalf("no checkins were recorded but sparkline has %d points", len(d.Checkins))
	}
}

// ---------------------------------------------------------------- 身分衝突

// 同一個 machine_id 出現兩個不同的 /etc/machine-id ⇒ 衝突，而且兩列都留著。
//
// ⚠ 不合併、不覆蓋、不刪掉舊的。挑一個當「真的」會讓一台機器的狀態
// 在兩台真實機器之間跳動，而且永遠查不出為什麼。
func TestIdentityConflictKeepsBothRows(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))

	if err := s.RecordCheckin(id, healthyCheckin(base.Add(-time.Minute)), base.Add(-time.Minute)); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	a := healthyBatch(base.Add(-10 * time.Minute))
	a.Identity.MachineIDHint = "aaaaaaaaaaaa"
	if err := s.RecordObservation(id, a, base.Add(-10*time.Minute)); err != nil {
		t.Fatalf("observation a: %v", err)
	}
	f, err := s.Facts(id, base)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.Conflict {
		t.Fatal("single identity flagged as conflict")
	}

	// 第二台機器帶著同一個身分報到，但 /etc/machine-id 不一樣。
	b := healthyBatch(base.Add(-1 * time.Minute))
	b.Identity.MachineIDHint = "bbbbbbbbbbbb"
	b.Identity.Hostname = "samplehub1-clone"
	if err := s.RecordObservation(id, b, base.Add(-time.Minute)); err != nil {
		t.Fatalf("observation b: %v", err)
	}

	f, err = s.Facts(id, base)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if !f.Conflict {
		t.Fatal("Facts.Conflict = false, want true — 兩個不同的 machine-id 沒被當成衝突")
	}
	if j := state.Derive(f); j.State != state.IdentityConflict {
		t.Fatalf("derived state = %q, want IdentityConflict", j.State)
	}

	// ⚠ 兩列都還在。
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM observed_state WHERE machine_id = ? AND kind = ?`, id, KindIdentity); n != 2 {
		t.Fatalf("%d identity rows, want 2 — 有人為了「解決」衝突刪了一列", n)
	}
	// 名冊上的 hint 沒有被第二台覆蓋掉。機器不准自己宣告自己是誰。
	m, err := s.GetMachine(id)
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if m.MachineIDHint != "aaaaaaaaaaaa" {
		t.Fatalf("registry machine_id_hint = %q, want the original — 觀測不准覆蓋名冊身分", m.MachineIDHint)
	}

	d, err := s.Detail(id, base)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(d.IdentityHints) != 2 {
		t.Fatalf("detail shows %d identity hints, want 2 — 兩列都要看得到", len(d.IdentityHints))
	}
	seen := map[string]bool{}
	for _, h := range d.IdentityHints {
		seen[h.Hint] = true
	}
	if !seen["aaaaaaaaaaaa"] || !seen["bbbbbbbbbbbb"] {
		t.Fatalf("identity hints = %+v, want both", d.IdentityHints)
	}
}

// ---------------------------------------------------------------- 時鐘與 crash-loop

func TestClockSkewAndBootIDCounting(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now.Add(-24*time.Hour))

	// 一小時內換了 4 個 boot_id：process 一直在崩，但偶爾的心跳有送到。
	for i := 0; i < 4; i++ {
		at := now.Add(-time.Duration(50-i*10) * time.Minute)
		c := healthyCheckin(at)
		c.BootID = "boot-" + string(rune('a'+i))
		if err := s.RecordCheckin(id, c, at); err != nil {
			t.Fatalf("checkin %d: %v", i, err)
		}
	}
	// 一小時之外的那一個不算。
	old := healthyCheckin(now.Add(-3 * time.Hour))
	old.BootID = "boot-old"
	if err := s.RecordCheckin(id, old, now.Add(-3*time.Hour)); err != nil {
		t.Fatalf("old checkin: %v", err)
	}

	f, err := s.Facts(id, now)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.BootIDChanges1h != 4 {
		t.Fatalf("BootIDChanges1h = %d, want 4 (一小時外的那筆不該算進來)", f.BootIDChanges1h)
	}

	// ⚠ ClockSkew = sent_at - received_at。機器時鐘快 5 分鐘 → 正的 5 分鐘。
	receivedAt := now.Add(-30 * time.Second)
	fast := healthyCheckin(receivedAt.Add(5 * time.Minute))
	fast.BootID = "boot-a"
	if err := s.RecordCheckin(id, fast, receivedAt); err != nil {
		t.Fatalf("skewed checkin: %v", err)
	}
	f, err = s.Facts(id, now)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.ClockSkew != 5*time.Minute {
		t.Fatalf("ClockSkew = %s, want +5m", f.ClockSkew)
	}
	// ⚠ 生死判斷用 received_at，不受時鐘影響。
	if !f.LastCheckinReceived.Equal(receivedAt) {
		t.Fatalf("LastCheckinReceived = %s, want %s (存活一律用 Hub 的時間)", f.LastCheckinReceived, receivedAt)
	}

	// 機器時鐘慢的方向也要是對的號誌。
	receivedAt2 := now.Add(-10 * time.Second)
	slow := healthyCheckin(receivedAt2.Add(-3 * time.Minute))
	slow.BootID = "boot-a"
	if err := s.RecordCheckin(id, slow, receivedAt2); err != nil {
		t.Fatalf("slow checkin: %v", err)
	}
	f, err = s.Facts(id, now)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.ClockSkew != -3*time.Minute {
		t.Fatalf("ClockSkew = %s, want -3m", f.ClockSkew)
	}
}

// ---------------------------------------------------------------- 憑證五個詞

// ⚠ 憑證狀態是五個詞，不是布林。任何地方把它壓成 true/false 都會讓
// "configured"（檔案裡的過期時間還沒到）跟「登入真的還有效」混為一談。
func TestCredentialStatusRoundTripsAsWords(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))

	expiry := base.Add(48 * time.Hour)
	statuses := []model.CredStatus{
		model.CredAbsent, model.CredConfigured, model.CredExpiresSoon,
		model.CredExpired, model.CredUnknown, model.CredFailed,
	}
	b := healthyBatch(base.Add(-time.Minute))
	b.Credentials = nil
	for i, st := range statuses {
		b.Credentials = append(b.Credentials, model.Credential{
			Provider: string(rune('a'+i)) + "-provider", Status: st,
			ExpiresAt: &expiry, ActiveAccountID: "acct-" + string(st),
			Note: "為什麼是這個狀態的人話說明",
		})
	}
	if err := s.RecordObservation(id, b, base.Add(-time.Minute)); err != nil {
		t.Fatalf("observation: %v", err)
	}

	evidence, err := s.MachineReadEvidence(id, base, MaxMachineEvidencePageSize)
	if err != nil {
		t.Fatalf("machine evidence: %v", err)
	}
	if len(evidence.Credentials) != len(statuses) {
		t.Fatalf("typed evidence has %d credentials, want %d", len(evidence.Credentials), len(statuses))
	}
	got := map[model.CredStatus]MachineReadCredential{}
	for _, c := range evidence.Credentials {
		got[c.Status] = c
	}
	for _, st := range statuses {
		row, ok := got[st]
		if !ok {
			t.Fatalf("status %q did not round-trip; got %+v", st, evidence.Credentials)
		}
		if !row.ActiveAccountSelected {
			t.Fatalf("status %q lost the non-secret active-account presence fact", st)
		}
		if row.Note == "" {
			t.Fatalf("status %q lost its Note — unknown 沒有理由等於沒有資訊", st)
		}
		if row.ExpiresAt == nil || !row.ExpiresAt.Equal(expiry) {
			t.Fatalf("status %q lost ExpiresAt: %v", st, row.ExpiresAt)
		}
	}
	if strings.Contains(fmt.Sprintf("%+v", evidence), "acct-") {
		t.Fatal("typed credential evidence retained an active account identifier")
	}

	// Facts 帶給判決層的也必須是同樣那幾個字。
	f, err := s.Facts(id, base)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	factStatuses := map[string]bool{}
	for _, c := range f.Credentials {
		factStatuses[c.Status] = true
	}
	for _, st := range statuses {
		if !factStatuses[string(st)] {
			t.Fatalf("Facts lost credential status %q: %+v", st, f.Credentials)
		}
	}

	// 投影表裡存的也是那幾個字。
	var stored string
	if err := s.DB().QueryRow(
		`SELECT status FROM credential_on_machine WHERE machine_id = ? AND provider = 'd-provider'`, id).
		Scan(&stored); err != nil {
		t.Fatalf("credential projection: %v", err)
	}
	if stored != string(model.CredExpired) {
		t.Fatalf("credential_on_machine.status = %q, want %q", stored, model.CredExpired)
	}
}

// ---------------------------------------------------------------- summary 原文

// ⚠ summary 原文照搬，一個字都不解析。
//
// 實測 cron_run_logs.status='ok' 只代表「agent 的回合正常結束並產出文字」，
// 64% 的 ok 其 summary 在描述失敗。這個測試放了一段滿是 "failed" / "error" 的
// summary，然後要求機器的狀態完全不受影響 —— 如果有人在 store 裡加了關鍵字
// 比對，這裡會紅。
func TestRunSummariesAreCarriedVerbatimAndNeverParsed(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 15, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))
	if err := s.RecordCheckin(id, healthyCheckin(base.Add(-30*time.Second)), base.Add(-30*time.Second)); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	const raw = "task failed: could not connect, error 500, giving up after 3 retries ❌"
	b := healthyBatch(base.Add(-time.Minute))
	b.OpenClaw.DB.RecentSummaries = []model.RunSummary{
		{JobID: "job-1", At: base.Add(-2 * time.Hour), Status: "ok", Summary: raw},
		{JobID: "job-2", At: base.Add(-3 * time.Hour), Status: "ok", Summary: "一切正常"},
	}
	if err := s.RecordObservation(id, b, base.Add(-time.Minute)); err != nil {
		t.Fatalf("observation: %v", err)
	}

	d, err := s.Detail(id, base)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	evidence, err := s.MachineReadEvidence(id, base, MaxMachineEvidencePageSize)
	if err != nil {
		t.Fatalf("machine evidence: %v", err)
	}
	if len(evidence.RunSummaries) != 2 {
		t.Fatalf("machine evidence carries %d summaries, want 2", len(evidence.RunSummaries))
	}
	if evidence.RunSummaries[0].Summary != raw {
		t.Fatalf("summary was rewritten:\n got %q\nwant %q", evidence.RunSummaries[0].Summary, raw)
	}
	if evidence.RunSummaries[0].Status != "ok" {
		t.Fatalf("summary status = %q, want the upstream value verbatim", evidence.RunSummaries[0].Status)
	}
	if evidence.OpenClaw.DB == nil {
		t.Fatalf("typed OpenClaw DB facts missing: %+v", evidence.OpenClaw)
	}
	// ⚠ 這一行是重點：summary 裡滿是 failed / error，機器狀態一律不受影響。
	if d.State != state.Online {
		t.Fatalf("state = %q — 有人在讀 summary 的文字推斷成敗了", d.State)
	}
	if !d.Facts.HasTaskSignal {
		t.Fatal("HasTaskSignal = false, want true (DB 有 last_task_ended_at)")
	}
}

// DB 讀不到的時候 HasTaskSignal 必須是 false。⚠ 「不知道」不可以變成「沒問題」。
func TestNoTaskSignalWhenOpenClawDBIsAbsent(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 16, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))
	if err := s.RecordCheckin(id, healthyCheckin(base.Add(-30*time.Second)), base.Add(-30*time.Second)); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	b := healthyBatch(base.Add(-time.Minute))
	b.OpenClaw.DB = &model.OpenClawDB{Present: false, Reason: "找不到 openclaw.sqlite（兩種 layout 都試過）"}
	if err := s.RecordObservation(id, b, base.Add(-time.Minute)); err != nil {
		t.Fatalf("observation: %v", err)
	}
	f, err := s.Facts(id, base)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if !f.OpenClawPresent {
		t.Fatal("OpenClawPresent = false, want true")
	}
	if f.HasTaskSignal {
		t.Fatal("HasTaskSignal = true 但 DB 根本讀不到 —— 不知道被當成了沒問題")
	}
	if j := state.Derive(f); j.State != state.Degraded {
		t.Fatalf("state = %q, want Degraded (裝了 OpenClaw 但沒有任何執行紀錄)", j.State)
	}

	// DB 整個 nil 也一樣。
	b2 := healthyBatch(base)
	b2.OpenClaw.DB = nil
	if err := s.RecordObservation(id, b2, base); err != nil {
		t.Fatalf("observation: %v", err)
	}
	f, err = s.Facts(id, base)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.HasTaskSignal {
		t.Fatal("HasTaskSignal = true 但 DB 是 nil")
	}
}

// 任務表讀不到時不能把未知說成從未跑完；TaskRunRows > 0 代表表已讀成，應維持舊判決而不是漏修。
func TestUnreadableOpenClawDBReasonDistinguishesUnknownFromNeverRan(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 16, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))
	if err := s.RecordCheckin(id, healthyCheckin(base.Add(-30*time.Second)), base.Add(-30*time.Second)); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	const reason = "sqlite: unable to open database file"
	b := healthyBatch(base.Add(-time.Minute))
	b.OpenClaw.DB = &model.OpenClawDB{Present: true, Reason: reason}
	if err := s.RecordObservation(id, b, base.Add(-time.Minute)); err != nil {
		t.Fatalf("observation: %v", err)
	}
	f, err := s.Facts(id, base)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.HasTaskSignal {
		t.Fatal("HasTaskSignal = true, want false")
	}
	if f.OpenClawDBReason != reason {
		t.Fatalf("OpenClawDBReason = %q, want %q", f.OpenClawDBReason, reason)
	}
	if j := state.Derive(f); strings.Contains(j.Reason, "從來沒跑完過任何東西") {
		t.Fatalf("資料庫讀不到時不可以斷言從來沒跑完：%s", j.Reason)
	}

	b = healthyBatch(base)
	b.OpenClaw.DB = &model.OpenClawDB{Present: true, Reason: reason, TaskRunRows: 3}
	if err := s.RecordObservation(id, b, base); err != nil {
		t.Fatalf("observation: %v", err)
	}
	f, err = s.Facts(id, base)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.OpenClawDBReason != "" {
		t.Fatalf("OpenClawDBReason = %q, want empty when task table was read", f.OpenClawDBReason)
	}
	if j := state.Derive(f); !strings.Contains(j.Reason, "從來沒跑完過任何東西") {
		t.Fatalf("任務表已讀到且沒有跑完紀錄時必須維持既有判決：%s", j.Reason)
	}
}

// 還沒量到 OpenClaw 時，不可以被講成已經量到而且沒裝。
func TestFactsDistinguishesUnobservedOpenClawFromObservedAbsence(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now.Add(-time.Hour))
	if err := s.RecordCheckin(id, healthyCheckin(now.Add(-time.Minute)), now.Add(-time.Minute)); err != nil {
		t.Fatalf("checkin: %v", err)
	}

	f, err := s.Facts(id, now)
	if err != nil {
		t.Fatalf("facts before observation: %v", err)
	}
	if f.OpenClawObserved {
		t.Fatal("沒有 observation 時 OpenClawObserved = true")
	}
	j := state.Derive(f)
	all := j.Reason
	for _, finding := range j.Findings {
		all += " " + finding.Message
	}
	if strings.Contains(all, "這台沒有裝 OpenClaw") {
		t.Errorf("沒有 observation 時不可以斷言沒裝 OpenClaw，實際：%s", all)
	}
	if !strings.Contains(all, "還沒收到這台的 OpenClaw 觀測") {
		t.Errorf("沒有 observation 時要明說還沒收到觀測，實際：%s", all)
	}

	if err := s.RecordObservation(id, healthyBatch(now), now); err != nil {
		t.Fatalf("observation: %v", err)
	}
	f, err = s.Facts(id, now)
	if err != nil {
		t.Fatalf("facts after observation: %v", err)
	}
	if !f.OpenClawObserved {
		t.Fatal("收到 observation 後 OpenClawObserved = false")
	}
}

// ---------------------------------------------------------------- 狀態歷史

func TestDetailReportsExactCheckinAndHistoryTruncation(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 9, 19, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "bounded-detail", now.Add(-time.Hour))
	oldest := now.Add(-time.Duration(DetailCheckinLimit+10) * time.Second)
	for i := 0; i <= DetailCheckinLimit; i++ {
		at := oldest.Add(time.Duration(i) * time.Second)
		if err := s.RecordCheckin(id, model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "v3",
			BootID: "boot-one", AgentSeq: int64(i + 1), AgentStartedAt: now.Add(-time.Hour),
			UptimeSeconds: func() *int64 { v := int64(100 + i); return &v }(), DiskFreeBytes: 50, DiskTotalBytes: 100,
		}, at); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i <= DetailHistoryLimit; i++ {
		machineState := state.Online
		if i%2 == 1 {
			machineState = state.Degraded
		}
		if err := s.RecordStateTransition(id, machineState, fmt.Sprintf("reason-%d", i),
			now.Add(time.Duration(i-DetailHistoryLimit-1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	detail, err := s.Detail(id, now)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.CheckinsTruncated || len(detail.Checkins) != DetailCheckinLimit ||
		detail.Checkins[0].AgentSeq != 2 || detail.Checkins[len(detail.Checkins)-1].AgentSeq != int64(DetailCheckinLimit+1) {
		t.Fatalf("checkin bound/truncation=%t count=%d first=%+v last=%+v",
			detail.CheckinsTruncated, len(detail.Checkins), detail.Checkins[0], detail.Checkins[len(detail.Checkins)-1])
	}
	if !detail.HistoryTruncated || len(detail.History) != DetailHistoryLimit ||
		detail.History[0].Reason != fmt.Sprintf("reason-%d", DetailHistoryLimit) {
		t.Fatalf("history bound/truncation=%t count=%d newest=%+v",
			detail.HistoryTruncated, len(detail.History), detail.History[0])
	}
}

func TestStateTransitionsAreRecordedAsIntervals(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 17, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))

	if err := s.RecordStateTransition(id, state.Online, "32 秒前回報", base); err != nil {
		t.Fatalf("transition 1: %v", err)
	}
	// ⚠ 狀態沒變就不該再寫一列，否則「什麼時候開始壞的」會被沖掉。
	for i := 1; i <= 3; i++ {
		if err := s.RecordStateTransition(id, state.Online, "還是好的", base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("repeat transition: %v", err)
		}
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM machine_state_history WHERE machine_id = ?`, id); n != 1 {
		t.Fatalf("%d history rows for an unchanged state, want 1", n)
	}

	brokeAt := base.Add(10 * time.Minute)
	if err := s.RecordStateTransition(id, state.Unreachable, "失聯 4 分鐘", brokeAt); err != nil {
		t.Fatalf("transition 2: %v", err)
	}
	spans, err := s.stateHistory(id, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2", len(spans))
	}
	if spans[0].State != state.Unreachable || spans[0].LeftAt != nil {
		t.Fatalf("newest span = %+v, want open Unreachable", spans[0])
	}
	if !spans[0].EnteredAt.Equal(brokeAt) {
		t.Fatalf("EnteredAt = %s, want %s", spans[0].EnteredAt, brokeAt)
	}
	if spans[1].LeftAt == nil || !spans[1].LeftAt.Equal(brokeAt) {
		t.Fatalf("old span was not closed: %+v — 失聯的起訖時間要留在歷史裡", spans[1])
	}

	// Overview 要能講出「從什麼時候開始的」。
	ov, err := s.Overview(brokeAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if len(ov.Machines) != 1 {
		t.Fatalf("%d rows", len(ov.Machines))
	}
	if ov.Machines[0].State != state.NeverReported {
		t.Fatalf("state = %q, want NeverReported (從沒 check-in 過)", ov.Machines[0].State)
	}
	// 判決是 NeverReported、歷史開著的是 Unreachable ⇒ StateSince 誠實留空。
	if ov.Machines[0].StateSince != nil {
		t.Fatalf("StateSince = %v, want nil when history disagrees with the live verdict", ov.Machines[0].StateSince)
	}
}

// ---------------------------------------------------------------- 相對昨天

func TestChangesSince(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 18, 0, 0, 0, time.UTC)
	yesterday := base.Add(-24 * time.Hour)
	id := mustEnroll(t, s, "samplehub1", base.Add(-48*time.Hour))

	old := healthyBatch(base.Add(-30 * time.Hour))
	old.Credentials[0].Status = model.CredConfigured
	old.CLITools[0].VersionReported = "2026.5.9"
	if err := s.RecordObservation(id, old, base.Add(-30*time.Hour)); err != nil {
		t.Fatalf("old observation: %v", err)
	}
	fresh := healthyBatch(base.Add(-time.Hour))
	fresh.Credentials[0].Status = model.CredExpired
	fresh.CLITools[0].VersionReported = "2026.6.1"
	fresh.CLITools[0].VersionPackageJSON = "2026.6.3"
	fresh.CLITools[0].SourcesDisagree = true
	if err := s.RecordObservation(id, fresh, base.Add(-time.Hour)); err != nil {
		t.Fatalf("fresh observation: %v", err)
	}
	if err := s.RecordStateTransition(id, state.Degraded, "claude 的登入已過期", base.Add(-time.Hour)); err != nil {
		t.Fatalf("transition: %v", err)
	}

	changes, err := s.ChangesSince(yesterday, base)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	byKey := map[string]Change{}
	for _, c := range changes {
		byKey[c.Kind+"/"+c.Subject] = c
	}

	cred, ok := byKey["credential/claude"]
	if !ok {
		t.Fatalf("credential change missing; got %+v", changes)
	}
	if cred.From != "configured" || cred.To != "expired" {
		t.Fatalf("credential change %q → %q, want configured → expired", cred.From, cred.To)
	}
	tool, ok := byKey["cli_tool/openclaw"]
	if !ok {
		t.Fatalf("cli tool change missing; got %+v", changes)
	}
	if tool.From == tool.To || tool.To == "" {
		t.Fatalf("cli tool change %q → %q", tool.From, tool.To)
	}
	// ⚠ 版本來源矛盾要如實出現在變化裡，不要挑一個當答案。
	if !strings.Contains(tool.To, "矛盾") {
		t.Fatalf("cli tool change hides the version disagreement: %q", tool.To)
	}
	st, ok := byKey["state/state"]
	if !ok {
		t.Fatalf("state change missing; got %+v", changes)
	}
	if st.To != string(state.Degraded) || st.Severity != state.Degraded.Severity() {
		t.Fatalf("state change = %+v", st)
	}
	// 狀態轉移排在事實變化前面（它是唯一被判過的那一種）。
	if changes[0].Kind != "state" {
		t.Fatalf("first change is %q, want state", changes[0].Kind)
	}
	// resources 每一批都在動，⚠ 刻意不算變化，否則早報變噪音。
	if _, ok := byKey["resources/resources"]; ok {
		t.Fatal("resources leaked into the change list")
	}

	// 沒有任何事發生的區間 ⇒ 沒有變化（早報那時候要送 "alive; 0 changes"）。
	quiet, err := s.ChangesSince(base.Add(-30*time.Minute), base)
	if err != nil {
		t.Fatalf("quiet changes: %v", err)
	}
	if len(quiet) != 0 {
		t.Fatalf("%d changes in a quiet window: %+v", len(quiet), quiet)
	}
}

// ---------------------------------------------------------------- 通知

func TestNotifications(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 19, 0, 0, 0, time.UTC)

	if _, ok, err := s.LastNotification("daily_report"); err != nil || ok {
		t.Fatalf("LastNotification on empty db = ok:%v err:%v", ok, err)
	}
	// ⚠ 送失敗也要記，但不算「送到」。
	if err := s.RecordNotification("daily_report", "telegram", "alive; 0 changes",
		false, "telegram 429", base); err != nil {
		t.Fatalf("record failed notification: %v", err)
	}
	if _, ok, err := s.LastNotification("daily_report"); err != nil || ok {
		t.Fatalf("failed delivery counted as sent (ok=%v err=%v)", ok, err)
	}

	delivered := base.Add(time.Minute)
	if err := s.RecordNotification("daily_report", "telegram", "alive; 0 changes",
		true, "", delivered); err != nil {
		t.Fatalf("record delivered: %v", err)
	}
	at, ok, err := s.LastNotification("daily_report")
	if err != nil || !ok {
		t.Fatalf("LastNotification = ok:%v err:%v", ok, err)
	}
	if !at.Equal(delivered) {
		t.Fatalf("LastNotification = %s, want %s", at, delivered)
	}
	if _, ok, _ := s.LastNotification("alert"); ok {
		t.Fatal("kinds are not isolated")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM notifications`); n != 2 {
		t.Fatalf("%d notification rows, want 2 (失敗的那次也要留著)", n)
	}
}

// ---------------------------------------------------------------- 防禦性解析

// ⚠ 一列壞掉的資料不可以讓 Hub 掛掉。Hub 掛掉的時候沒有人在看，
// 而這個產品的整個賣點就是「它會在你沒看的時候替你看著」。
func TestMalformedRowsDoNotPanic(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 20, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))

	if _, err := s.DB().Exec(`
INSERT INTO machine_checkins (machine_id, sent_at, received_at, max_seen_revision, max_applied_revision)
VALUES (?, 'not-a-timestamp', 'also-not-a-timestamp', 0, 0)`, id); err != nil {
		t.Fatalf("insert junk checkin: %v", err)
	}
	if _, err := s.DB().Exec(`
INSERT INTO observed_state (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES ('junk', ?, 'nope', 'nope', ?, 'identity', '{not json', 'agent_measurement')`,
		id, KindIdentity); err != nil {
		t.Fatalf("insert junk observation: %v", err)
	}

	f, err := s.Facts(id, base)
	if err != nil {
		t.Fatalf("Facts on malformed rows: %v", err)
	}
	if f.EverCheckedIn {
		t.Fatal("解不出時間的心跳被當成有效的報到")
	}
	if _, err := s.Detail(id, base); err != nil {
		t.Fatalf("Detail on malformed rows: %v", err)
	}
	if _, err := s.Overview(base); err != nil {
		t.Fatalf("Overview on malformed rows: %v", err)
	}
	if _, err := s.ChangesSince(base.Add(-24*time.Hour), base); err != nil {
		t.Fatalf("ChangesSince on malformed rows: %v", err)
	}
}

// ---------------------------------------------------------------- 名冊編輯

func TestUpsertMachineKeepsIdentityAndToken(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 2, 21, 0, 0, 0, time.UTC)

	tok, _ := s.CreateEnrollToken("samplehub1", time.Hour)
	id, agentTok, err := s.RedeemEnrollToken(tok, model.EnrollRequest{EnrollToken: tok, Hostname: "samplehub1"}, base)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	m, err := s.GetMachine(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	m.DisplayName = "samplehub1 (書房)"
	m.Notes = "5090 那台"
	if err := s.UpsertMachine(m); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// ⚠ 改顯示名稱不該把機器的身分洗掉。
	if got, err := s.AuthenticateAgent(agentTok); err != nil || got != id {
		t.Fatalf("agent token broken by a registry edit: %q %v", got, err)
	}
	got, err := s.GetMachine(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DisplayName != "samplehub1 (書房)" || got.Notes != "5090 那台" {
		t.Fatalf("upsert lost the edit: %+v", got)
	}
	if !got.CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("created_at changed: %s → %s", m.CreatedAt, got.CreatedAt)
	}

	// 先寫進名冊、之後才報到，是刻意支援的順序（那台在報到前是 NeverReported）。
	if err := s.UpsertMachine(Machine{MachineID: "planned-1", DisplayName: "還沒裝的那台", Expected: true}); err != nil {
		t.Fatalf("upsert new: %v", err)
	}
	list, err := s.ListMachines()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("%d machines, want 2", len(list))
	}
	if _, err := s.GetMachine("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetMachine(nope) = %v, want ErrNotFound", err)
	}
	if err := s.UpsertMachine(Machine{}); err == nil {
		t.Fatal("upsert with empty machine_id accepted")
	}
}

// TestIssuingATokenPutsTheMachineInTheDenominator 是這個檔案裡最重要的測試之一，
// 而它是補上去的 —— 因為原本的程式沒做到，而且是在自己的 e2e 畫面上被抓到的。
//
// 開一張給 sampleagent3 的票，就是一個人說出「我打算納管 sampleagent3」。從那一刻起
// 它就在分母裡。它從此不報到，畫面上要有一格紅燈寫「從未報到」——
// 不是它壓根不存在。
//
// 原本的行為是：名冊列等到 Redeem 才建。於是「開了票、機器沒來、名冊上什麼
// 都沒有」，完美重現了 sampleagent3 隱形七週的那個 bug —— 在一個以「不要再發生
// 那件事」為存在理由的產品裡。
func TestIssuingATokenPutsTheMachineInTheDenominator(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	if _, err := s.CreateEnrollToken("sampleagent3", time.Hour); err != nil {
		t.Fatalf("create token: %v", err)
	}
	// ⚠ 刻意不 redeem。這台機器永遠不會報到。

	ov, err := s.Overview(now)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.Expected != 1 {
		t.Fatalf("開了票的機器不在分母裡：Expected=%d，應該是 1", ov.Expected)
	}
	var found bool
	for _, m := range ov.Machines {
		if m.DisplayName == "sampleagent3" {
			found = true
			if m.State != state.NeverReported {
				t.Errorf("從未報到的機器狀態是 %s，應該是 NeverReported", m.State)
			}
			if m.EnrolledAt != nil {
				t.Error("沒報到過的機器不該有 enrolled_at")
			}
		}
	}
	if !found {
		t.Fatal("sampleagent3 從畫面上消失了 —— 這正是這個產品要修的那個 bug")
	}
}

// TestRedeemFillsTheExistingRowInsteadOfCreatingASecond：報到是把名冊列填滿，
// 不是再建一列。
//
// ⚠ 少了這條，同一台機器會有兩列：開票時建的那列永遠 NeverReported，報到後建的那列
// 正常運作 —— 畫面上同時出現兩台同名機器，其中一台永遠紅著，而且沒有人
// 查得出為什麼。
func TestRedeemFillsTheExistingRowInsteadOfCreatingASecond(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	preID, tok, err := s.CreateEnrollTokenFor("samplehub1", time.Hour)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	gotID, _, err := s.RedeemEnrollToken(tok, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: tok,
		Hostname: "cnoderidge-ai1", OS: "linux", Arch: "amd64", UnixUser: "example-user",
	}, now)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	if gotID != preID {
		t.Errorf("報到換了一個身分：開票時 %s，報到後 %s", preID, gotID)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM machine_registry`); n != 1 {
		t.Errorf("名冊上有 %d 列，應該只有 1 列", n)
	}

	m, err := s.GetMachine(gotID)
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	// ⚠ 人取的名字不可以被機器自報的 hostname 蓋掉。
	// 實測 5 台裡有 3 台的 hostname 跟人記得的名字不一樣。
	if m.DisplayName != "samplehub1" {
		t.Errorf("顯示名稱被 hostname 蓋掉了：%s", m.DisplayName)
	}
	if m.Hostname != "cnoderidge-ai1" {
		t.Errorf("hostname 沒有填進去：%q", m.Hostname)
	}
	if m.EnrolledAt == nil {
		t.Error("報到之後 enrolled_at 還是空的")
	}
}

// TestExpiredUnusedTokenLeavesTheMachineOnScreen：票過期沒被用掉，
// 名冊列照樣留著。那不是垃圾，那是「你說要納管、但它沒來」這個事實。
func TestExpiredUnusedTokenLeavesTheMachineOnScreen(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	_, tok, err := s.CreateEnrollTokenFor("sampleagent7", time.Nanosecond)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if _, _, err := s.RedeemEnrollToken(tok, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: tok,
	}, now.Add(time.Hour)); !errors.Is(err, ErrEnrollToken) {
		t.Fatalf("過期的票應該被拒絕，得到 %v", err)
	}

	ov, err := s.Overview(now.Add(time.Hour))
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if ov.Expected != 1 {
		t.Fatalf("票過期之後機器離開了分母：Expected=%d", ov.Expected)
	}
	if ov.Machines[0].State != state.NeverReported {
		t.Errorf("狀態是 %s，應該是 NeverReported", ov.Machines[0].State)
	}
}

// ---------------------------------------------------------------- 占用帳本

// TestOccupancyLedgerIsAppendOnlyAndIdempotent
//
// ⚠ agent 每 10 分鐘會重送最近 200 筆 cron 紀錄，所以同一個回合會被送很多次。
// 如果每次都寫成新的一列，帳本會在一天之內把同一件事記 144 遍 ——
// 一本會自我複製的帳，比沒有帳更難查。
//
// observation_id 是內容的雜湊，重送會命中 INSERT OR IGNORE。
// ⚠ 是 IGNORE 不是 UPDATE：帳本被改寫過就不是帳本了。
func TestOccupancyLedgerIsAppendOnlyAndIdempotent(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	id := mustEnroll(t, s, "samplehub1", now.Add(-time.Hour))

	at := now.Add(-30 * time.Minute)
	b := healthyBatch(now)
	b.OpenClaw.DB = &model.OpenClawDB{
		Present: true, Layout: "consolidated",
		OccupancyRowsSeen: 3, OccupancyRowsNoProvider: 1,
		Occupancy: []model.OccupancyEvidence{{
			Source: "cron_run_logs", JobID: "j1", At: at, Status: "ok",
			Provider: "openai", Model: "gpt-5.5",
			SessionKey: "agent:cnode-network:cron:j1:run:r1",
			AgentID:    "cnode-network", TotalTokens: 20901,
		}},
	}

	for i := 0; i < 3; i++ { // 送三次，模擬 agent 的重複上報
		if err := s.RecordObservation(id, b, now); err != nil {
			t.Fatalf("第 %d 次觀測失敗：%v", i+1, err)
		}
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM ticket_occupancy_observation`); n != 1 {
		t.Fatalf("同一個回合被記了 %d 次，want 1 —— 帳本在自我複製", n)
	}

	// ⚠ profile_id 必須是 NULL。我們有證據證明「用了 openai 這一家」，
	// 但沒有證據證明「用了哪一張票」—— 硬填就是 SPEC 禁止的無證據書寫。
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM ticket_occupancy_observation WHERE profile_id IS NOT NULL`); n != 0 {
		t.Error("帳本填了 profile_id，但沒有任何證據指向某一張票")
	}
	// ⚠ process_alive 永遠是 0。「有個 process 活著」不是占用的證據 ——
	// 用它假裝占用，帳本會在每一台開著 OpenClaw 的機器上都顯示滿載。
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM ticket_occupancy_observation WHERE process_alive != 0`); n != 0 {
		t.Error("有列用 process_alive 假裝占用")
	}
}

// 沒有 provider 的回合不准入帳，但缺口要看得見。
func TestOccupancyWithoutProviderIsNotWrittenButIsCounted(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	id := mustEnroll(t, s, "samplehub1", now.Add(-time.Hour))

	b := healthyBatch(now)
	b.OpenClaw.DB = &model.OpenClawDB{
		Present: true, OccupancyRowsSeen: 2, OccupancyRowsNoProvider: 1,
		Occupancy: []model.OccupancyEvidence{
			{Source: "cron_run_logs", JobID: "j1", At: now.Add(-time.Minute),
				Status: "ok", Provider: "openai", AgentID: "main"},
			// ⚠ 這一筆沒有 provider。probe 理論上已經濾掉了，這是第二道。
			{Source: "cron_run_logs", JobID: "j2", At: now.Add(-2 * time.Minute),
				Status: "error", ErrorText: "cron: job interrupted by gateway restart"},
		},
	}
	if err := s.RecordObservation(id, b, now); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM ticket_occupancy_observation`); n != 1 {
		t.Fatalf("帳本筆數 = %d，want 1（沒有 provider 的那筆不准寫）", n)
	}

	evidence, err := s.MachineReadEvidence(id, now, MaxMachineEvidencePageSize)
	if err != nil {
		t.Fatal(err)
	}
	// 缺口必須送到畫面上。一本安靜地丟掉資料的帳，看起來跟完整的一模一樣。
	if evidence.OccupancyRowsSkipped != 1 || evidence.OccupancyRowsSeen != 2 {
		t.Errorf("typed occupancy completeness = %d/%d，want skipped 1 / seen 2",
			evidence.OccupancyRowsSkipped, evidence.OccupancyRowsSeen)
	}
	if len(evidence.Occupancy) != 1 || evidence.Occupancy[0].Provider != "openai" {
		t.Fatalf("彙總結果不對：%+v", evidence.Occupancy)
	}
	if len(evidence.Occupancy[0].Agents) != 1 || evidence.Occupancy[0].Agents[0] != "main" {
		t.Errorf("agents = %v，want [main]", evidence.Occupancy[0].Agents)
	}
}

// ⚠ 同一條訂閱在不同 OpenClaw 版本下叫不同名字（openai / openai-codex）。
// 帳本不准自動併起來 —— 那會蓋掉真實差異；也不准自動拆開 ——
// 那會看起來像一次從來沒發生過的換票。兩個名字就是兩列，如實呈現。
func TestOccupancyKeepsProviderNamesVerbatim(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	a := mustEnroll(t, s, "samplehub1", now.Add(-time.Hour))
	b := mustEnroll(t, s, "sampleagent2", now.Add(-time.Hour))

	put := func(id, provider, source string) {
		t.Helper()
		batch := healthyBatch(now)
		batch.OpenClaw.DB = &model.OpenClawDB{Present: true,
			Occupancy: []model.OccupancyEvidence{{
				Source: source, JobID: "j1", At: now.Add(-time.Minute),
				Status: "ok", Provider: provider, AgentID: "main",
			}}}
		if err := s.RecordObservation(id, batch, now); err != nil {
			t.Fatal(err)
		}
	}
	put(a, "openai", "cron_run_logs")
	put(b, "openai-codex", "cron_runs_jsonl")

	ea, _ := s.MachineReadEvidence(a, now, MaxMachineEvidencePageSize)
	eb, _ := s.MachineReadEvidence(b, now, MaxMachineEvidencePageSize)
	if len(ea.Occupancy) != 1 || ea.Occupancy[0].Provider != "openai" {
		t.Errorf("samplehub1 的 provider = %+v，want openai 原文", ea.Occupancy)
	}
	if len(eb.Occupancy) != 1 || eb.Occupancy[0].Provider != "openai-codex" {
		t.Errorf("sampleagent2 的 provider = %+v，want openai-codex 原文", eb.Occupancy)
	}
}

// TestRestartLoopSurvivesTheTripThroughTheDatabase ——
// 2026-09-03 事故的另一半回歸測試。
//
// state 那邊有規則、machine.html 有欄位、state_test 也有測試，
// 每個零件單獨看都是對的。斷掉的是它們中間那一段：
// 送上來的事實裡根本沒有 process 的身分，於是規則永遠拿到 0。
// 所以這個測試一定要真的走一趟 DB，不能只在 state 裡塞 Facts。
func TestRestartLoopSurvivesTheTripThroughTheDatabase(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now.Add(-80*24*time.Hour))

	// 事故當天的真實形狀：每 95 秒被 SIGABRT 一次、重啟、立刻送一顆心跳。
	// ⚠ 機器連續開機 80 天，所以 boot_id 從頭到尾是同一個。
	const bootID = "同一顆-機器-沒重開過"
	for i := 0; i < 24; i++ {
		at := now.Add(-time.Duration(24-i) * 95 * time.Second)
		c := healthyCheckin(at)
		c.BootID = bootID
		c.AgentStartedAt = at // 每次都是新 process
		if err := s.RecordCheckin(id, c, at); err != nil {
			t.Fatalf("checkin %d: %v", i, err)
		}
	}

	f, err := s.Facts(id, now)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.BootIDChanges1h != 1 {
		t.Fatalf("BootIDChanges1h = %d，機器根本沒重開過", f.BootIDChanges1h)
	}
	if f.AgentRestarts1h != 24 {
		t.Fatalf("AgentRestarts1h = %d, want 24", f.AgentRestarts1h)
	}
	// 走完整條路：事實 → 判決。這一步才是當初沒接上的地方。
	if j := state.Derive(f); j.State == state.Online {
		t.Errorf("每 95 秒重啟一次還判 Online：%s", j.Reason)
	}
}

// 舊版 agent 不送 agent_started_at。那是「不知道」，不是「沒重啟過」——
// 也不能變成「重啟過一次」。
func TestOldAgentsWithoutStartTimeAreNotCountedAsRestarting(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "old-agent", now.Add(-24*time.Hour))

	for i := 0; i < 5; i++ {
		at := now.Add(-time.Duration(5-i) * 2 * time.Minute)
		c := healthyCheckin(at)
		c.AgentStartedAt = time.Time{} // 舊版 agent：這個欄位不存在
		if err := s.RecordCheckin(id, c, at); err != nil {
			t.Fatalf("checkin %d: %v", i, err)
		}
	}

	f, err := s.Facts(id, now)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.AgentRestarts1h != 0 {
		t.Errorf("AgentRestarts1h = %d，舊 agent 沒送這個欄位就不該數出東西"+
			"（零值時間如果被寫成字串，這裡會得到 1）", f.AgentRestarts1h)
	}
}

// ---------------------------------------------------------------- 證據體檢

// 2026-09-03 事故的第三個回歸點。
//
// observation_age_seconds 全機隊 100% NULL 好幾天，這個欄位存在的唯一目的
// 就是講「agent 在線但沒在觀測」，它從第一天起就一直在講，而畫面上什麼都沒有。
// 同一天還量到 samplehub1 的 task_runs 是 940/940 全 'succeeded' —— 一個常數。
//
// 規則：**不會變的欄位不是訊號。** 一條死掉的證據管線不會產生錯誤，
// 它產生的是很有說服力的綠燈，這對一個以「我不知道」為存在理由的產品是最糟的。
func TestDeadEvidenceChannelIsReported(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now.Add(-24*time.Hour))

	// 事故當時的形狀：心跳一直進來，observation_age_seconds 每一筆都是 NULL。
	for i := 0; i < 30; i++ {
		at := now.Add(-time.Duration(30-i) * 2 * time.Minute)
		c := healthyCheckin(at)
		c.ObservationAgeSeconds = nil
		c.AgentStartedAt = at
		if err := s.RecordCheckin(id, c, at); err != nil {
			t.Fatalf("checkin %d: %v", i, err)
		}
	}

	dead, err := s.DeadSignals(now)
	if err != nil {
		t.Fatalf("dead signals: %v", err)
	}
	if !hasSignal(dead, "observation_age_seconds") {
		t.Fatalf("30 筆心跳裡 observation_age_seconds 全是 NULL，體檢沒講話。得到 %v", dead)
	}
	for _, d := range dead {
		if d.Reason == "" {
			t.Errorf("%s 沒有說明為什麼 —— 看的人不會知道要去修什麼", d.Column)
		}
	}
}

// 欄位有值、而且值會變 → 不該有任何告警。
func TestALiveChannelIsNotReported(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now.Add(-24*time.Hour))

	for i := 0; i < 30; i++ {
		at := now.Add(-time.Duration(30-i) * 2 * time.Minute)
		c := healthyCheckin(at)
		age := int64(100 + i) // 每筆都不一樣
		c.ObservationAgeSeconds = &age
		c.AgentStartedAt = now.Add(-24 * time.Hour) // 沒重啟過 = 只有一個值
		if err := s.RecordCheckin(id, c, at); err != nil {
			t.Fatalf("checkin %d: %v", i, err)
		}
	}

	dead, err := s.DeadSignals(now)
	if err != nil {
		t.Fatalf("dead signals: %v", err)
	}
	if hasSignal(dead, "observation_age_seconds") {
		t.Errorf("欄位好好的還被報成死掉：%v", dead)
	}
	// ⚠ agent_started_at 只有一個值是**正常**的 —— 那代表 agent 一直沒重啟過，
	// 正是我們要的結果。這一欄只能檢查 NULL，不能檢查「常數」，
	// 否則體檢會在一切正常的時候尖叫，然後在三天內被靜音。
	if hasSignal(dead, "agent_started_at") {
		t.Errorf("agent 沒重啟過反而被當成訊號死掉：%v", dead)
	}
}

// 樣本太少的時候閉嘴。剛裝好的 Hub 只有兩三筆資料，
// 那時候什麼都「看起來像常數」。
func TestFreshInstallDoesNotScream(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now.Add(-10*time.Minute))

	for i := 0; i < 3; i++ {
		at := now.Add(-time.Duration(3-i) * 2 * time.Minute)
		c := healthyCheckin(at)
		c.ObservationAgeSeconds = nil
		if err := s.RecordCheckin(id, c, at); err != nil {
			t.Fatalf("checkin %d: %v", i, err)
		}
	}

	dead, err := s.DeadSignals(now)
	if err != nil {
		t.Fatalf("dead signals: %v", err)
	}
	if len(dead) != 0 {
		t.Errorf("才 3 筆資料就開始告警：%v", dead)
	}
}

func hasSignal(ds []DeadSignal, col string) bool {
	for _, d := range ds {
		if d.Column == col {
			return true
		}
	}
	return false
}

// TestOneHealthyMachineDoesNotHideTheOtherFour ——
// 全機隊一起算的話，一台好的機器就能把四台壞的蓋掉。
//
// ⚠ 這跟 §5.5 那個「早報說 alive; 0 changes」是同一個形狀：
// 判斷寫成了全有全無，於是最壞的那幾台剛好變成沒有人講話的那幾台。
// 證據管線是**每一台各自**會死的東西，就必須每一台各自算。
func TestOneHealthyMachineDoesNotHideTheOtherFour(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)
	good := mustEnroll(t, s, "good1", now.Add(-24*time.Hour))
	bad := mustEnroll(t, s, "bad1", now.Add(-24*time.Hour))

	for i := 0; i < 30; i++ {
		at := now.Add(-time.Duration(30-i) * 2 * time.Minute)

		g := healthyCheckin(at)
		age := int64(100 + i)
		g.ObservationAgeSeconds = &age
		if err := s.RecordCheckin(good, g, at); err != nil {
			t.Fatalf("good %d: %v", i, err)
		}

		b := healthyCheckin(at)
		b.ObservationAgeSeconds = nil // 這台從來沒觀測成功過
		if err := s.RecordCheckin(bad, b, at); err != nil {
			t.Fatalf("bad %d: %v", i, err)
		}
	}

	dead, err := s.DeadSignals(now)
	if err != nil {
		t.Fatalf("dead signals: %v", err)
	}
	var namedBad, namedGood bool
	for _, d := range dead {
		if d.Column != "observation_age_seconds" {
			continue
		}
		if d.DisplayName == "bad1" {
			namedBad = true
		}
		if d.DisplayName == "good1" {
			namedGood = true
		}
	}
	if !namedBad {
		t.Fatalf("bad1 的 observation_age_seconds 整片 NULL 卻沒被點名，"+
			"因為 good1 有值把它蓋掉了。得到 %v", dead)
	}
	if namedGood {
		t.Errorf("good1 好好的卻被點名：%v", dead)
	}
}

// TestAConstantIsNotEvidenceOfADeadChannel ——
// 第一版的體檢有一條「同一欄從頭到尾只有一個值 = 管線死了」的規則。
// 它在第一份真實資料上就是 5 個誤報、0 個真陽性：
//
//	samplehub1 / sampleagent2 的 clock_skew_seconds 恆為 0  → 那兩台的時鐘就是準的
//	每台的 provider 只有一個值                      → 那台就真的只用了一個 provider
//
// NULL 是「沒有資料進來」，那是關於管線的事實。
// 常數是「資料每次都說一樣的話」，那可能就是真相 —— 而 Hub 沒有辦法分辨。
// 從分布的形狀去推論語意，跟從摘要文字去推論成敗是同一族的錯。
func TestAConstantIsNotEvidenceOfADeadChannel(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 7, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now.Add(-24*time.Hour))

	// 時鐘準的機器：skew 每一筆都是 0。
	for i := 0; i < 30; i++ {
		at := now.Add(-time.Duration(30-i) * 2 * time.Minute)
		c := healthyCheckin(at)
		c.SentAt = at // sent == received → skew 恆為 0
		age := int64(100 + i)
		c.ObservationAgeSeconds = &age
		if err := s.RecordCheckin(id, c, at); err != nil {
			t.Fatalf("checkin %d: %v", i, err)
		}
	}

	dead, err := s.DeadSignals(now)
	if err != nil {
		t.Fatalf("dead signals: %v", err)
	}
	if hasSignal(dead, "clock_skew_seconds") {
		t.Errorf("時鐘準的機器被報成「時鐘漂移偵測是關的」。"+
			"一個在正常狀態下就會亮的燈，三天內就會被靜音：%v", dead)
	}
}

// ⚠ 存快照時要把占用證據拿掉，但**帳本必須照樣寫滿**。
//
// 這兩件事的順序是這個函式唯一會出錯的地方：就地把 Occupancy 設成 nil，
// 帳本就會變成空的 —— 而帳本是 Phase 2 唯一的硬門檻。
func TestSnapshotDropsRawOccupancyButLedgerStillFills(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC()
	id := mustEnroll(t, st, "samplehub1", now)

	b := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: now}
	b.OpenClaw = model.OpenClaw{Present: true, DB: &model.OpenClawDB{
		Present: true, Layout: "consolidated", Path: "/x/openclaw.sqlite",
		OccupancyRowsSeen:       3,
		OccupancyRowsNoProvider: 1,
		Occupancy: []model.OccupancyEvidence{
			// ⚠ 兩筆的 job_id 與時間都不同。帳本的 id 是
			// (machine, source, job_id, at) 的雜湊 —— 拿兩筆一模一樣的來測，
			// 它會被正確地去重，然後這個測試會誤以為證據掉了。
			{Provider: "openai", At: now.Add(-2 * time.Minute), JobID: "j1",
				SessionKey: "agent:a:cron:j1:run:1", Source: "cron_run_logs"},
			{Provider: "openai", At: now.Add(-time.Minute), JobID: "j2",
				SessionKey: "agent:a:cron:j2:run:2", Source: "cron_run_logs"},
		},
	}}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("record: %v", err)
	}

	// 帳本要有兩筆。
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM ticket_occupancy_observation WHERE machine_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("帳本 %d 筆，want 2 —— 為了省空間把證據弄丟了", n)
	}

	// 快照裡不可以再有那個陣列。
	var payload string
	if err := st.db.QueryRow(
		`SELECT payload FROM observed_state WHERE machine_id = ? AND kind = ?`, id, KindOpenClaw).
		Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "agent:a:cron:b:run:1") {
		t.Error("占用證據在快照裡又存了一份 —— 那是整個資料庫 92% 的來源")
	}
	// ⚠ 但純量要留著：週報在讀它們，而它們回答的是帳本答不出來的事。
	for _, want := range []string{"occupancy_rows_seen", "occupancy_rows_no_provider"} {
		if !strings.Contains(payload, want) {
			t.Errorf("%s 被一起丟掉了 —— 週報就再也說不出有多少證據沒進帳", want)
		}
	}

	// ⚠ 呼叫端的 batch 不可以被就地改掉。
	if len(b.OpenClaw.DB.Occupancy) != 2 {
		t.Errorf("原本的 batch 被改了：%d 筆", len(b.OpenClaw.DB.Occupancy))
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestUnmeasuredUptimeRoundTripsAsNullNotZero(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "uptime-null-roundtrip", now.Add(-time.Hour))
	zero := int64(0)
	for _, checkin := range []model.Checkin{
		{SchemaVersion: model.SchemaVersion, SentAt: now.Add(-time.Minute), UptimeSeconds: nil},
		{SchemaVersion: model.SchemaVersion, SentAt: now, UptimeSeconds: &zero},
	} {
		if err := s.RecordCheckin(id, checkin, checkin.SentAt); err != nil {
			t.Fatalf("寫入 uptime 測試心跳失敗：%v；無法守住 nil 到 SQL NULL 的來回，畫面可能把沒量到印成 0s", err)
		}
	}

	rows, err := s.db.Query(`SELECT uptime_seconds IS NULL, COALESCE(uptime_seconds, 0)
FROM machine_checkins WHERE machine_id = ? ORDER BY sent_at`, id)
	if err != nil {
		t.Fatalf("直接查 uptime_seconds 欄位失敗：%v；必須看資料庫欄位，因為用哨兵數字取代 NULL 時畫面看起來一樣，但 SQL 聚合與數值過濾會被毒到", err)
	}
	defer rows.Close()
	var stored []struct {
		isNull bool
		value  int64
	}
	for rows.Next() {
		var row struct {
			isNull bool
			value  int64
		}
		if err := rows.Scan(&row.isNull, &row.value); err != nil {
			t.Fatalf("掃描 uptime_seconds 欄位失敗：%v；必須看資料庫欄位，因為用哨兵數字取代 NULL 時畫面看起來一樣，但 SQL 聚合與數值過濾會被毒到", err)
		}
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("走訪 uptime_seconds 欄位失敗：%v；必須看資料庫欄位，因為用哨兵數字取代 NULL 時畫面看起來一樣，但 SQL 聚合與數值過濾會被毒到", err)
	}
	if len(stored) != 2 {
		t.Fatalf("資料庫有 %d 列 uptime_seconds，預期依 sent_at 排序後有 2 列；必須看資料庫欄位，因為用哨兵數字取代 NULL 時畫面看起來一樣，但 SQL 聚合與數值過濾會被毒到", len(stored))
	}
	if !stored[0].isNull {
		t.Fatalf("沒量到 uptime 的資料庫欄位不是 NULL（值為 %d）；用哨兵數字取代 NULL 時畫面看起來一樣，但 SQL 聚合與數值過濾會被毒到", stored[0].value)
	}
	if stored[1].isNull || stored[1].value != 0 {
		t.Fatalf("量到 0 uptime 的資料庫欄位為 isNull=%t value=%d，預期非 NULL 且等於 0；只看畫面無法分辨哨兵數字，而哨兵會毒到 SQL 聚合與數值過濾", stored[1].isNull, stored[1].value)
	}

	detail, err := s.Detail(id, now)
	if err != nil {
		t.Fatalf("讀回 uptime 測試心跳失敗：%v；無法確認 operator 不會以為那台 Mac 每顆心跳都剛開機／一直在重開", err)
	}
	if len(detail.Checkins) != 2 {
		t.Fatalf("讀回 %d 顆心跳，預期 2 顆；無法分辨沒量到與真的剛開機，operator 可能誤判 Mac 一直在重開", len(detail.Checkins))
	}
	if detail.Checkins[0].UptimeObserved {
		t.Fatalf("nil uptime 讀回後 UptimeObserved=true；沒量到會被印成 0s，operator 會以為那台 Mac 每顆心跳都剛開機／一直在重開")
	}
	if !detail.Checkins[1].UptimeObserved || detail.Checkins[1].UptimeSeconds != 0 {
		t.Fatalf("量到 0 uptime 讀回為 observed=%t seconds=%d，預期 observed=true seconds=0；真的剛開機會被錯印成 unknown", detail.Checkins[1].UptimeObserved, detail.Checkins[1].UptimeSeconds)
	}
}
