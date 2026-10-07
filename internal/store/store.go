// Package store 是 Hub 的持久層。
//
// 這一層只做兩件事：把 Agent 說的「事實」原封不動存下來，以及把存下來的事實
// 組成 internal/state 需要的輸入。
//
// ⚠ 這裡不判斷任何一台機器好不好。判決只在 internal/state 發生（docs/SPEC.md §3）。
// 如果你在這個檔案裡想寫一個 if 來決定某台機器是不是健康的，那個 if 走錯地方了。
//
// 四條寫入規則，違反了整個產品就沒有東西可看：
//
//  1. observed_state 是 append-only。永不覆蓋、永不合併、永不刪除。
//     上游的資料庫 7 天 / 每 job 2000 筆就會把歷史吃掉 —— Hub 是唯一的歷史，
//     而 Dashboard 預設顯示「相對昨天的變化」，只有歷史答得出來。
//  2. check-in 是 UPSERT-only（PK = machine_id, sent_at）。網路抖動時 5 台機器
//     同時重試，不可以長出重複的列。
//  3. 身分衝突不「解決」—— 兩列都留著並標成衝突。挑一個當真的會讓一台機器的
//     狀態在兩台真實機器之間跳動，而且永遠查不出為什麼。
//  4. 生死一律用 Hub 的 received_at 算，不用 Agent 說的 sent_at。機器時鐘快兩天
//     會讓 sent_at 看起來永遠很新鮮。
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"

	_ "modernc.org/sqlite"
)

// schema.sql 直接編進 binary。⚠ 部署時少一個檔案就少一種「昨天還好好的」。
//
//go:embed schema.sql
var schemaSQL string

// ---------------------------------------------------------------- 常數

// observed_state.kind / source 的合法值。字串直接對應 schema.sql 的註解。
const (
	KindIdentity   = "identity"
	KindResources  = "resources"
	KindSystemd    = "systemd"
	KindJournal    = "journal"
	KindArtifact   = "artifact"
	KindEvents     = "events"
	KindOpenClaw   = "openclaw"
	KindCredential = "credential"
	KindCLITool    = "cli_tool"
	KindBAT        = "bat"

	SourceAgentMeasurement = "agent_measurement"
	SourceTaskEvent        = "task_event"
)

const (
	// DetailCheckinWindow / DetailCheckinLimit：詳細頁 sparkline 的範圍。
	// 2 分鐘一次心跳，24 小時約 720 點，畫得出「哪一段沒聲音」。
	DetailCheckinWindow = 24 * time.Hour
	DetailCheckinLimit  = 1000

	// DetailHistoryLimit：詳細頁顯示幾段狀態歷史。
	// 「這台什麼時候開始壞的」是最常問的問題（schema.sql 的註解）。
	DetailHistoryLimit = 20
)

// ---------------------------------------------------------------- 錯誤

var (
	ErrNotFound                        = errors.New("store: machine not found")
	ErrUnauthorized                    = errors.New("store: unknown or invalid agent token")
	ErrRollbackNotQuiescent            = errors.New("store: rollback ledger is not quiescent")
	ErrDesiredResourceRevisionConflict = errors.New("store: desired state resource revision is duplicated")
	// ErrWorkloadPolicyIdentityMismatch 表示這個 process 記憶體裡載入的
	// expectations 不是 DB 已發布的 active generation。此時不可下發 token，
	// 也不可用這個 process 的 policy 為 observation 作健康見證。
	ErrWorkloadPolicyIdentityMismatch = errors.New("store: in-memory workload policy does not match the active generation")

	// ErrEnrollToken 是所有 enroll token 失敗的共同父錯誤。
	//
	// ⚠ HTTP 層一律用 errors.Is(err, ErrEnrollToken) 判斷，然後回同一個
	// ENROLL_TOKEN_INVALID —— 不要把「不存在 / 已用過 / 過期」的差別回給對方。
	// 那三種差別只寫在 Hub 的 log 裡給人看：告訴一個還沒通過認證的呼叫端
	// 「這張 token 存在但已經用過了」，等於免費送他一個列舉的答案。
	ErrEnrollToken = errors.New("store: enroll token rejected")

	ErrTokenInvalid = fmt.Errorf("%w: unknown", ErrEnrollToken)
	ErrTokenUsed    = fmt.Errorf("%w: already used", ErrEnrollToken)
	ErrTokenExpired = fmt.Errorf("%w: expired", ErrEnrollToken)
	// ErrTokenRetired：票還沒用過也沒過期，但它綁的那台機器已經退役了。
	//
	// ⚠ 少了這一條會產生一個很難查的狀態：報到**成功**（拿到 agent token），
	// 然後之後每一個請求都 401 —— 因為 AuthenticateAgent 濾掉 retired 的機器。
	// 「裝好了但一直在 401」比「一開始就被拒絕」難查得多。
	ErrTokenRetired = fmt.Errorf("%w: machine retired", ErrEnrollToken)
)

// ---------------------------------------------------------------- 資料型別

// Machine 是名冊上的一列 —— 也就是分母。
//
// ⚠ 名冊有 5 台、只有 4 台報到，那 1 台亮紅燈，不是從畫面上消失。
// 一台從來沒 check-in 過不代表它不存在，只代表你還不知道它怎麼了。
type Machine struct {
	// RegistrySequence is SQLite's immutable insertion sequence for the current
	// ledger generation. It is never serialized or accepted from an operator;
	// safe list cursors use it only as a creation ceiling. VACUUM/restore starts
	// a new traversal contract, so callers must discard old cursors there.
	RegistrySequence  int64  `json:"-"`
	MachineID         string `json:"machine_id"`
	DisplayName       string `json:"display_name"`       // 人講的名字：samplehub1 / sampleagent1
	Channel           string `json:"channel,omitempty"`  // 空字串 = DB 的 NULL，尚未指派
	ChannelRevision   int64  `json:"channel_revision"`   // channel mutation 的 optimistic concurrency token
	LifecycleRevision int64  `json:"lifecycle_revision"` // active/retired mutation 的 optimistic concurrency token
	// 空字串 = DB 的 NULL。ID 是配對鍵；login 只是顯示，可以被改成跟別人一樣。
	AssignedUserID       string `json:"assigned_user_id,omitempty"`
	AssignedUserLogin    string `json:"assigned_user_login,omitempty"`
	AssignedUserRevision int64  `json:"assigned_user_revision"` // 指派使用者 mutation 的 optimistic concurrency token
	Hostname             string `json:"hostname,omitempty"`
	// Expected 是尚待移除的舊名冊欄位；production writers 一律寫 true。
	// 分母只看 active/retired lifecycle，不再讀這個值。
	Expected    bool   `json:"expected"`
	UnixUser    string `json:"unix_user,omitempty"`
	OS          string `json:"os,omitempty"`
	Arch        string `json:"arch,omitempty"`
	TailscaleIP string `json:"tailscale_ip,omitempty"` // 詳細頁最底下的 Connect BAT 要用
	// MachineIDHint 是 /etc/machine-id。重灌會變，所以它只是候選身分，不是身分本身。
	MachineIDHint string     `json:"machine_id_hint,omitempty"`
	LingerEnabled *bool      `json:"linger_enabled,omitempty"` // nil = 還不知道
	Notes         string     `json:"notes,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	EnrolledAt    *time.Time `json:"enrolled_at,omitempty"`
	RetiredAt     *time.Time `json:"retired_at,omitempty"` // retire 後離開分母，歷史留著
}

// InDenominator：未退役就在分母裡；只有明確 retire 能讓名冊列離開。
func (m Machine) InDenominator() bool { return m.RetiredAt == nil }

// StateCount 是一個狀態的機器數，給 Dashboard 第一排用。
type StateCount struct {
	State state.State `json:"state"`
	Count int         `json:"count"`
}

// Overview 是 Dashboard 的讀取模型。
//
// ⚠ Machines 一定包含名冊上每一台未退場的機器，包含從來沒報到過的那些。
// 「沒資料」在這裡不是「跳過這一列」，而是一格紅燈。
type Overview struct {
	Now time.Time `json:"now"`
	// Total 是未退役 Machines 的列數；Expected 是尚待改名的同一個分母數。
	Total    int                 `json:"total"`
	Expected int                 `json:"expected"`
	Counts   map[state.State]int `json:"counts"`
	Machines []MachineRow        `json:"machines"` // 最該看的排最前面
	// Findings 是全機隊的發現攤平後排序的結果，最嚴重的在最前面。
	// ⚠ 連 advisory 的也留著。把 unknown 藏起來是這個專案最想避免的事。
	Findings []FleetFinding `json:"findings"`

	// Retired 是退役的機器。**不算 Counts / Total / Expected，但一定列出來。**
	//
	// ⚠⚠ 這一格是一個 bug 的墓碑。原本 Overview 直接 `continue` 掉退役的
	// 機器，理由是「它離開分母了」—— 前半句對，後半句（所以不用顯示）錯得
	// 很嚴重，而且錯了兩層：
	//
	//  1. 取消退役的按鈕在單機頁上，而單機頁只有從機隊表格點得進去。
	//     機器一從表格上消失，那個按鈕就變成一扇沒有門的房間 ——
	//     路由在、handler 在、測試綠的，但人到不了。
	//     retire 於是在 UI 上變成不可逆的，而「可以反悔」是
	//     internal/web/actions.go 開頭自己寫下的三條規則之一。
	//
	//  2. 「我是不是退役錯了一台」變成答不出來的問題。
	//
	// ⚠ 退役的機器刻意**不算狀態燈**：對一台你故意關掉的機器算出紅燈，
	// 那盞燈不代表任何事，只會讓真的紅燈變便宜。
	Retired []Machine `json:"retired,omitempty"`
}

// StateCounts 把 Counts 依嚴重度由高到低攤成有序的列，給 UI 用。
func (o Overview) StateCounts() []StateCount {
	out := make([]StateCount, 0, len(o.Counts))
	for st, n := range o.Counts {
		out = append(out, StateCount{State: st, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State.Severity() != out[j].State.Severity() {
			return out[i].State.Severity() > out[j].State.Severity()
		}
		return out[i].State < out[j].State
	})
	return out
}

// DenominatorCounts is the authoritative state rollup for operator headlines,
// KPIs, and reports. Every visible non-retired row is in the denominator.
func (o Overview) DenominatorCounts() map[state.State]int {
	counts := make(map[state.State]int)
	for _, machine := range o.Machines {
		if machine.InDenominator() {
			counts[machine.State]++
		}
	}
	return counts
}

// ReportingCount is the authoritative liveness numerator for every operator
// surface. It intentionally cannot be reconstructed from State because a
// durable IdentityConflict may remain after its last heartbeat has expired.
func (o Overview) ReportingCount() int {
	n := 0
	for _, machine := range o.Machines {
		if machine.InDenominator() && state.IsReportingAt(machine.Facts, o.Now) {
			n++
		}
	}
	return n
}

// MachineRow 是 Dashboard 上的一列：機器 + 判決 + 判決用到的事實。
//
// Facts 整包帶著走是刻意的 —— UI 上每一盞燈都要點得進去看到證據，
// 而證據就是判決的輸入。點不進去的燈等於沒有燈。
type MachineRow struct {
	Machine
	State  state.State `json:"state"`
	Reason string      `json:"reason"` // 一句人看得懂的話，UI 直接顯示
	// StateSince：目前這個狀態是什麼時候進來的（來自 machine_state_history）。
	// 早報要靠它寫「still broken since Sep 1」而不是每天再點一盞新燈。
	StateSince *time.Time      `json:"state_since,omitempty"`
	Facts      state.Facts     `json:"facts"`
	Findings   []state.Finding `json:"findings"`
}

// FleetFinding 是一條掛著機器名字的 Finding，給 Dashboard 的全機隊清單用。
type FleetFinding struct {
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name"`
	state.Finding
}

// CheckinPoint 是 sparkline 的一個點。
//
// ⚠ ReceivedAt 才是「這台有沒有在講話」的座標；SentAt 只用來算時鐘偏移。
type CheckinPoint struct {
	SentAt                time.Time     `json:"sent_at"`
	ReceivedAt            time.Time     `json:"received_at"`
	AgentVersion          string        `json:"agent_version,omitempty"`
	BootID                string        `json:"boot_id,omitempty"`
	AgentSeq              int64         `json:"agent_seq"`
	AgentSeqObserved      bool          `json:"agent_seq_observed"`
	UptimeSeconds         int64         `json:"uptime_seconds"`
	UptimeObserved        bool          `json:"uptime_observed"`
	DiskFreeBytes         int64         `json:"disk_free_bytes"`
	DiskTotalBytes        int64         `json:"disk_total_bytes"`
	DiskFreeObserved      bool          `json:"disk_free_observed"`
	DiskTotalObserved     bool          `json:"disk_total_observed"`
	ObservationAgeSeconds *int64        `json:"observation_age_seconds,omitempty"`
	ClockSkew             time.Duration `json:"clock_skew"`
	ClockSkewObserved     bool          `json:"clock_skew_observed"`
}

// JournalRow 是某個 unit 最近一次的 journal 摘要。
//
// ⚠⚠ 這裡面的 Example 是**原文**，而且必須維持是原文。不要解析它、
// 不要關鍵字比對、不要從它推導任何狀態欄位 —— 跟 MachineReadEvidence 的
// RunSummaries 同一條規矩。理由寫在 PRODUCT.md 地基二：最痛的那種錯，
// log 裡沒有 error。它存在的唯一理由是讓人自己讀。
type JournalRow struct {
	model.UnitJournal
	MeasuredAt time.Time `json:"measured_at"`
	ReceivedAt time.Time `json:"received_at"`
}

// IdentityHint 是這台機器看過的一個 /etc/machine-id。
//
// ⚠ 超過一列就是身分衝突，而且兩列都要留著、兩列都要顯示。
// 不要在這裡加「哪個才是真的」的欄位 —— Hub 不知道，猜錯的代價是狀態
// 在兩台真實機器之間跳動，而且沒有人查得出為什麼。
type IdentityHint struct {
	Hint         string    `json:"hint"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	Count        int       `json:"count"`
	FromRegistry bool      `json:"from_registry"` // 名冊上登記的那一個
}

// StateSpan 是一段狀態區間。LeftAt 為 nil 表示現在還在這個狀態。
// 存成區間而不是一列，因為「失聯的起訖時間」要留在歷史裡。
type StateSpan struct {
	State     state.State `json:"state"`
	Reason    string      `json:"reason"`
	EnteredAt time.Time   `json:"entered_at"`
	LeftAt    *time.Time  `json:"left_at,omitempty"`
}

// Detail 是單機詳細頁的讀取模型。
type Detail struct {
	Machine             Machine           `json:"machine"`
	Now                 time.Time         `json:"now"`
	State               state.State       `json:"state"`
	Reason              string            `json:"reason"`
	Findings            []state.Finding   `json:"findings"` // 全部，含 advisory
	Facts               state.Facts       `json:"facts"`
	Checkins            []CheckinPoint    `json:"checkins"` // 由舊到新，畫 sparkline 用
	CheckinsTruncated   bool              `json:"checkins_truncated"`
	History             []StateSpan       `json:"history"` // 最近幾段狀態，含起訖
	HistoryTruncated    bool              `json:"history_truncated"`
	Identity            *model.Identity   `json:"identity,omitempty"`
	IdentityObserved    bool              `json:"identity_observed"`
	IdentityDecoded     bool              `json:"identity_decoded"`
	IdentityObservedAt  *ObservationClock `json:"identity_observed_at,omitempty"`
	Resources           *model.Resources  `json:"resources,omitempty"`
	ResourcesObserved   bool              `json:"resources_observed"`
	ResourcesDecoded    bool              `json:"resources_decoded"`
	ResourcesObservedAt *ObservationClock `json:"resources_observed_at,omitempty"`
	// IdentityHints：看過的每一個 machine-id。> 1 列就是衝突，兩列都在這裡。
	IdentityHints []IdentityHint `json:"identity_hints"`

	// Connect 是「怎麼連上這台的 BAT」，含**連不上時的理由**。
	// ⚠ 理由跟位址一樣重要：機隊四台有兩台 --bind=localhost，
	// 那兩台任何位址都連不上，而那件事必須寫在畫面上，不是留白。
	Connect ConnectInfo `json:"connect"`

	// ExpectConfigured / ExpectErr 是「期望設定檔」本身的狀態。
	//
	// ⚠⚠ 畫面必須分得出三件事，它們在「沒有 finding」上長得一模一樣：
	//   1. 沒有人宣告過期望              → ExpectConfigured=false
	//   2. 宣告了，而且全部通過          → Configured=true, Facts.Artifacts 有東西
	//   3. 宣告了，但設定檔讀壞了        → ExpectErr 非空
	// 把 1 或 3 顯示成 2，就是這個產品存在的理由的反面。
	ExpectConfigured bool   `json:"expect_configured"`
	ExpectErr        string `json:"expect_err,omitempty"`
}

// Change 是「相對昨天變了什麼」的一列，早報用。
type Change struct {
	MachineID   string    `json:"machine_id"`
	DisplayName string    `json:"display_name"`
	Kind        string    `json:"kind"`    // state|registry|identity|credential|cli_tool|systemd|openclaw
	Subject     string    `json:"subject"` // provider 名 / unit 名 / 工具名 ...
	From        string    `json:"from"`    // 空字串 = 這次才第一次出現
	To          string    `json:"to"`
	At          time.Time `json:"at"`
	// Severity 只有狀態轉移才有值（直接取 state.State.Severity()），因為那是
	// internal/state 已經判過的。⚠ 其他變化一律 1 —— store 不判斷一個事實
	// 有多嚴重，那是判決，不是持久化。早報的排序規則見 docs/OPEN-QUESTIONS Q8。
	Severity int `json:"severity"`
}

// ---------------------------------------------------------------- Store

type Store struct {
	// db is the single writer connection. rdb is the query-only reader pool,
	// opened on the same file after migrations. Reads that do not need to see
	// the caller's uncommitted writes go to rdb: with MaxOpenConns(1), a read
	// on db while this goroutine holds a write transaction deadlocks.
	db  *sql.DB
	rdb *sql.DB
	// gate bounds and times writer acquisition. The wait budget is not the
	// transaction lifetime.
	gate writerGate
	// nowFn 讓測試可以固定時間。正式路徑一律 time.Now().UTC()。
	nowFn func() time.Time
	// changeReadSlots prevents a few broad operator reports from occupying all
	// SQLite readers. Acquisition is deliberately non-blocking: callers can
	// return a typed busy response instead of queuing behind an expensive read.
	changeReadSlots chan struct{}

	// expects 是人宣告的期望。nil = 沒有人宣告過。
	// ⚠ nil 跟「宣告了但全部通過」是兩件事，畫面必須分得出來。
	expects       *expect.Set
	expectsLoaded bool

	// afterReconcileOverview 只給 deterministic transaction-boundary 測試用。
	// production 永遠是 nil；hook 執行時 BEGIN IMMEDIATE writer lock 已在手上。
	afterReconcileOverview func()
	// afterBatchPromoteGate 只給 stable Continue/OpenDeploymentBatch 的
	// gate→insert transaction-boundary 測試用；production 永遠是 nil。
	afterBatchPromoteGate func()
	// Artifact fetch hooks只給 deterministic transaction-boundary 測試用；
	// production 永遠是 nil。
	afterArtifactFetchListCount     func()
	afterArtifactFetchEnqueueCommit func(string)
}

// SetExpectations 由 Hub 在啟動時交進來。
//
// ⚠ 沒呼叫過 = 沒有任何期望，那**不是**「都通過了」。
func (s *Store) SetExpectations(e *expect.Set) {
	s.expects = e
	s.expectsLoaded = true
}

// Expectations 回目前的宣告集合（給畫面用，可能是 nil）。
func (s *Store) Expectations() *expect.Set { return s.expects }

// PublishExpectationsPolicy 把 Hub service 這個 process 真正載入的期望政策發布到 DB。
// 另一個 CLI process 不會繼承 systemd 的 EnvironmentFile；它必須從這裡讀，
// 不能把自己的 expects=nil 當成 service 的 active policy。
func (s *Store) PublishExpectationsPolicy(now time.Time) error {
	fingerprint, valid, err := expectationsPolicyFingerprint(s.expects)
	if err != nil {
		return err
	}
	tx, err := s.beginWrite(context.Background(), "publish_expectations_policy")
	if err != nil {
		return fmt.Errorf("store: begin publish active workload policy: %w", err)
	}
	defer tx.Rollback()
	// Open() creating the new tables is not evidence that the final recorder ever
	// ran. The service's first successful policy publish is the earliest point at
	// which incoming observations can be projected under a coherent policy. Keep
	// that first boundary forever across restarts and later policy generations.
	if _, err := tx.Exec(`INSERT INTO canary_evidence_epoch(singleton,started_at) VALUES(1,?)
		 ON CONFLICT(singleton) DO NOTHING`, fmtTime(now)); err != nil {
		return fmt.Errorf("store: publish canary evidence epoch: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO active_workload_policy
	 (singleton,expectations_fingerprint,generation,valid,updated_at) VALUES(1,?,1,?,?)
	 ON CONFLICT(singleton) DO UPDATE SET
	 generation=CASE
	   WHEN active_workload_policy.expectations_fingerprint<>excluded.expectations_fingerprint
	     OR active_workload_policy.valid<>excluded.valid
	   THEN active_workload_policy.generation+1
	   ELSE active_workload_policy.generation
	 END,
	 expectations_fingerprint=excluded.expectations_fingerprint,
	 valid=excluded.valid,updated_at=excluded.updated_at`,
		fingerprint, valid, fmtTime(now)); err != nil {
		return fmt.Errorf("store: publish active workload policy: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit active workload policy: %w", err)
	}
	return nil
}

func expectationsPolicyFingerprint(exps *expect.Set) (string, bool, error) {
	type policy struct {
		Version    int                 `json:"version"`
		Configured bool                `json:"configured"`
		Error      string              `json:"error,omitempty"`
		Rules      []model.Expectation `json:"rules"`
	}
	p := policy{Version: 1}
	if exps != nil {
		p.Configured, p.Error = exps.Configured, exps.Err
		p.Rules = append([]model.Expectation(nil), exps.Rules...)
	}
	// Rules 與 NotOK 的順序不影響政策語意。先正規化，讓 service
	// restart 或 JSON 重排不會平白要求全機隊重送 observation。
	for i := range p.Rules {
		if p.Rules[i].Events == nil {
			continue
		}
		events := *p.Rules[i].Events
		events.NotOK = append([]string(nil), events.NotOK...)
		sort.Strings(events.NotOK)
		p.Rules[i].Events = &events
	}
	sort.Slice(p.Rules, func(i, j int) bool {
		a, _ := json.Marshal(p.Rules[i])
		b, _ := json.Marshal(p.Rules[j])
		return string(a) < string(b)
	})
	raw, err := json.Marshal(p)
	if err != nil {
		return "", false, fmt.Errorf("store: fingerprint workload policy: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), p.Error == "", nil
}

// workloadPolicyToken 把全域 active policy 與規則套用所依賴的顯示名稱綁在一起。
// Token 是 opaque wire identity；rename 後即使全域設定檔一個 byte 都沒變，
// 舊名稱下量到的 effective rules 也不能替新名稱作證。
func workloadPolicyToken(fingerprint string, generation int64, displayName string) string {
	if fingerprint == "" || generation <= 0 || displayName == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", fingerprint, generation, displayName)))
	return hex.EncodeToString(sum[:])
}

// CurrentWorkloadPolicyToken 回傳這個 Store 記憶體中真正載入的 policy token。
// Hub service 用它下發；Observation ingest 也用同一算法驗回傳值。
func (s *Store) CurrentWorkloadPolicyToken(displayName string) (string, error) {
	policy, matches, err := s.matchingActiveWorkloadPolicy(s.rdb)
	if err != nil {
		return "", err
	}
	if !matches {
		return "", ErrWorkloadPolicyIdentityMismatch
	}
	return workloadPolicyToken(policy.fingerprint, policy.generation, displayName), nil
}

// matchingActiveWorkloadPolicy 把 process 真正載入的 expectations 與 DB 的
// persisted identity 對齊。generation 只由 PublishExpectationsPolicy 遞增；
// check-in 與 ingest 都必須讀同一列，不能各自只重算 fingerprint。
func (s *Store) matchingActiveWorkloadPolicy(q workloadPolicyQueryRower) (activeWorkloadPolicy, bool, error) {
	policy, err := activeWorkloadPolicyFrom(q)
	if err != nil {
		return policy, false, err
	}
	if !s.expectsLoaded {
		return policy, false, nil
	}
	fingerprint, valid, err := expectationsPolicyFingerprint(s.expects)
	if err != nil {
		return policy, false, err
	}
	matches := policy.published && policy.generation > 0 &&
		policy.fingerprint == fingerprint && policy.valid == valid
	return policy, matches, nil
}

// Open 開啟（必要時建立）Hub 的 SQLite 檔並把 schema 跑一遍。
//
// schema.sql 全部是 CREATE TABLE IF NOT EXISTS，重複跑是安全的。
// 5 台機器不跑 PostgreSQL —— 那是另一個要備份、升級、會單獨死的 process。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create dir: %w", err)
		}
	}
	// busy_timeout 是必要的：心跳、觀測、Dashboard 查詢會同時打進來，
	// 沒有它就會在第一次併發時吐 SQLITE_BUSY。
	// _txlock=immediate 讓每一個顯式寫入交易在 BEGIN 就先拿 SQLite 的 writer
	// reservation。這不只是少一種 SQLITE_BUSY：stable promote 會先拿這把跨
	// process 的鎖、再重讀 canary 證據，於是 Hub 的 failure recorder 或另一支
	// clawctl-hub 同時建立的新 canary 只能排在它前面或後面，不能插在判決與開單之間。
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	// busy_timeout is the backstop for other processes (Litestream, a direct
	// CLI while the Hub is stopped, upgrade scripts). In-process writers queue
	// on the single connection instead of racing the busy handler.
	// synchronous=NORMAL with WAL cannot corrupt the file. A power loss may
	// drop the last commits that had not been checkpointed.
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	dsn := u.String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	// The active schema adds a partial UNIQUE index for active deployment
	// ownership. Detect a legacy violation before running any migration
	// statements so an upgrade fails closed with the exact resource and owners,
	// rather than partially migrating and surfacing SQLite's opaque constraint
	// text. Repeat the diagnosis after Exec as well: an old process could have
	// committed a second owner between this read and index creation.
	if err := rejectLegacyDuplicateActiveDeploymentResources(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if err := rejectDuplicateDesiredStateResourceRevisions(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		if conflictErr := rejectDuplicateDesiredStateResourceRevisions(db); conflictErr != nil &&
			errors.Is(conflictErr, ErrDesiredResourceRevisionConflict) {
			db.Close()
			return nil, fmt.Errorf("store: migrate: %w", conflictErr)
		}
		if conflictErr := rejectLegacyDuplicateActiveDeploymentResources(db); conflictErr != nil &&
			errors.Is(conflictErr, ErrDeploymentActiveResource) {
			db.Close()
			return nil, fmt.Errorf("store: migrate: %w (create active-owner index: %v)", conflictErr, err)
		}
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	if err := addMissingColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	// auth_subject may be absent until addMissingColumns on legacy databases.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_auth_action_at ON audit_log(auth_subject, action, at)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate login audit index: %w", err)
	}
	if err := ensureJobEventProvenanceTrigger(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate job event provenance trigger: %w", err)
	}
	if err := ensureMachineChannelRevisionTrigger(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate channel revision trigger: %w", err)
	}
	if err := ensureMachineLifecycleRevisionTriggers(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate lifecycle revision triggers: %w", err)
	}
	if err := ensureMachineAssignedUserRevisionTrigger(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate assigned user revision trigger: %w", err)
	}
	if err := backfillIdentityHints(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate identity hints: %w", err)
	}
	if err := clearTerminalLeases(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	// The writer pool is one connection for the life of the process. Set this
	// only after migrations, which run on this same *sql.DB before any reader
	// exists. Lifetime 0 keeps that connection (and its pragmas) until Close.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	rdb, err := openReader(path)
	if err != nil {
		db.Close()
		return nil, err
	}
	st := &Store{
		db: db, rdb: rdb, nowFn: func() time.Time { return time.Now().UTC() },
		changeReadSlots: make(chan struct{}, maxConcurrentChangeReads),
	}
	st.gate.stats.init()
	return st, nil
}

// openReader opens the query-only pool on a file the writer has already migrated.
// journal_mode is a property of the file; setting it here would be a write.
func openReader(path string) (*sql.DB, error) {
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	u.RawQuery = q.Encode()
	rdb, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	rdb.SetMaxOpenConns(8)
	if err := rdb.Ping(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("store: ping reader: %w", err)
	}
	return rdb, nil
}

const desiredStateResourceRevisionIndex = "ux_desired_state_resource_revision"

func rejectDuplicateDesiredStateResourceRevisions(db *sql.DB) error {
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(
 SELECT 1 FROM sqlite_master WHERE type='table' AND name='desired_state')`).Scan(&exists); err != nil {
		return fmt.Errorf("store: inspect desired_state table: %w", err)
	}
	if !exists {
		return nil
	}
	var kind, id string
	var revision int64
	var count int
	var desiredIDs string
	err := db.QueryRow(`SELECT resource_kind,resource_id,revision,n,desired_ids FROM (
 SELECT resource_kind,resource_id,revision,
  COUNT(*) OVER (PARTITION BY resource_kind,resource_id,revision) AS n,
  GROUP_CONCAT(desired_id, ',') OVER (
   PARTITION BY resource_kind,resource_id,revision ORDER BY desired_id
   ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING
  ) AS desired_ids
 FROM desired_state
) WHERE n > 1
ORDER BY resource_kind,resource_id,revision
LIMIT 1`).Scan(&kind, &id, &revision, &count, &desiredIDs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: scan desired state resource revision: %w", err)
	}
	return fmt.Errorf("%w: resource %s:%s revision %d has %d desired state entries [%s]; this Hub cannot open this ledger, use the previous Hub to restore service",
		ErrDesiredResourceRevisionConflict, kind, id, revision, count, desiredIDs)
}

// CheckRollbackCompatible 只讀地確認這份 ledger 能不能交給另一版 Hub。
//
// 它刻意不呼叫 Open：Open 會跑 migration 與 terminal lease backfill；upgrade
// script 必須在「備份升級前 DB」之前先做這個檢查，不能因為檢查本身先改了 DB。
// mode=ro 仍會讀 WAL（不能用 immutable，否則會漏看尚未 checkpoint 的工作單），
// query_only 則讓未來不小心塞進這條路徑的寫入也直接失敗。
func CheckRollbackCompatible(path string) error {
	present, err := rollbackDatabasePresent(path)
	if err != nil {
		return err
	}
	if !present {
		// 第一次安裝還沒有 ledger；不要為了回答「是空的」反而建立一個。
		return nil
	}
	if err := ValidateExistingLedger(path); err != nil {
		return fmt.Errorf("store: rollback compatibility source is not a clawctl ledger: %w", err)
	}

	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fmt.Errorf("store: open rollback compatibility DB read-only: %w", err)
	}
	defer db.Close()
	// 兩個 count 必須來自同一個 SQLite snapshot；否則一個 writer 可能剛好
	// 在兩次查詢之間開單，拼出一份從未同時成立過的「靜止」答案。
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store: begin rollback compatibility read: %w", err)
	}
	defer tx.Rollback()

	var activeDeployments, nonTerminalJobs, activeArtifactFetches int
	var deploymentTableCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='deployments'`).Scan(&deploymentTableCount); err != nil {
		return fmt.Errorf("store: identify deployments table for rollback: %w", err)
	}
	if deploymentTableCount > 1 {
		return fmt.Errorf("store: deployments table identity is ambiguous")
	}
	// Phase 1 ledgers predate deployment orchestration.  The strict identity
	// manifest above proves this is a complete historical clawctl ledger, so an
	// absent deployments table has the exact semantic value of zero active
	// deployments; every other query error remains fail closed.
	if deploymentTableCount == 1 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM deployments WHERE state IN (?,?)`,
			DeploymentRunning, DeploymentPaused).Scan(&activeDeployments); err != nil {
			return fmt.Errorf("store: count active deployments for rollback: %w", err)
		}
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM jobs
 WHERE state NOT IN (?,?,?,?,?)`, deploy.Succeeded, deploy.Failed, deploy.Rejected,
		deploy.LeaseExpired, deploy.ManualIntervention).Scan(&nonTerminalJobs); err != nil {
		return fmt.Errorf("store: count nonterminal jobs for rollback: %w", err)
	}
	var artifactFetchTableCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='artifact_fetch_operations'`).Scan(&artifactFetchTableCount); err != nil {
		return fmt.Errorf("store: identify artifact fetch table for rollback: %w", err)
	}
	if artifactFetchTableCount > 1 {
		return errors.New("store: artifact fetch table identity is ambiguous")
	}
	// Ledgers from before the artifact-operation slice have no such table and
	// therefore exactly zero active fetches. New ledgers must not be handed to an
	// older binary while a durable intent still needs its worker.
	if artifactFetchTableCount == 1 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM artifact_fetch_operations WHERE state IN ('queued','running')`).Scan(&activeArtifactFetches); err != nil {
			return fmt.Errorf("store: count active artifact fetches for rollback: %w", err)
		}
	}
	if activeDeployments != 0 || nonTerminalJobs != 0 || activeArtifactFetches != 0 {
		return fmt.Errorf("%w: %d active deployments (running/paused), %d non-terminal jobs, %d active artifact fetches (queued/running) remaining; quiesce the ledger before changing versions",
			ErrRollbackNotQuiescent, activeDeployments, nonTerminalJobs, activeArtifactFetches)
	}
	return nil
}

// clearTerminalLeases 補正舊版 Hub 留下的「終態卻仍有租約」的工作單列。
//
// ⚠ 2026-09-06 之前的終態 UPDATE 只改 state 與 terminal_at，lease_token 與
// lease_expires_at 原封不動。帳本上因此有幾張已經 succeeded／failed 的單
// 看起來還有一個有效持有者。新版每一條終態路徑都在同一個 UPDATE 清租約，
// 但既有的列不會自己好；這裡宣告不變量「終態 ⇒ 沒有租約」，違反的就修。
// 冪等：修過一次之後 WHERE 就再也對不到任何列。
func clearTerminalLeases(db *sql.DB) error {
	_, err := db.Exec(`
UPDATE jobs
   SET lease_token = NULL, lease_expires_at = NULL
 WHERE state IN (?, ?, ?, ?, ?)
   AND (lease_token IS NOT NULL OR lease_expires_at IS NOT NULL)`,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention)
	if err != nil {
		return fmt.Errorf("clear terminal leases: %w", err)
	}
	return nil
}

// addMissingColumns 補上舊資料庫缺的欄位。
//
// ⚠ `CREATE TABLE IF NOT EXISTS` 對已經存在的表**完全沒有作用** ——
// 改了 schema.sql，新機器會拿到新欄位，跑了三個月的那台不會，
// 而且它不會報錯，只會在第一次 INSERT 時說「no such column」。
//
// 這裡刻意不做版本號式的遷移框架：那需要一張 migration 表、一個版本序列，
// 以及每次改欄位都要記得寫遷移。目前這個專案只有一個人在改，
// 「宣告想要的欄位，缺的就補上」比較不會忘。等到需要改欄位型別或搬資料
// 的那天，再換成真正的遷移框架 —— ADD COLUMN 撐不到那裡。
func addMissingColumns(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin add missing columns: %w", err)
	}
	defer tx.Rollback()
	want := map[string]map[string]string{
		// ⚠ agent_started_at 是 2026-09-03 事故後補的。舊 DB 沒有這欄，
		// 而那些機器正是最需要被抓出來的那批。
		"machine_checkins": {
			"agent_started_at": "TEXT",
			"jobs_enabled":     "INTEGER CHECK (jobs_enabled IN (0,1))",
			// 舊 DB 沒有這欄，而那批機器正是還沒回報設定的那批。
			"settings_digest": "TEXT",
		},
		"ticket_occupancy_observation": {
			"model":           "TEXT",
			"job_id":          "TEXT",
			"agent_id":        "TEXT",
			"run_status":      "TEXT",
			"total_tokens":    "INTEGER",
			"last_error_text": "TEXT",
		},
		"machine_registry": {
			"channel":                "TEXT",
			"channel_revision":       "INTEGER NOT NULL DEFAULT 0",
			"lifecycle_revision":     "INTEGER NOT NULL DEFAULT 0",
			"assigned_user_id":       "TEXT",
			"assigned_user_login":    "TEXT",
			"assigned_user_revision": "INTEGER NOT NULL DEFAULT 0",
		},
		// desired_state revision is immutable deployment material identity.  This
		// separate token fences stale lifecycle/open-batch operator requests.
		"deployments": {
			"control_revision":   "INTEGER NOT NULL DEFAULT 0",
			"pause_after_canary": "INTEGER NOT NULL DEFAULT 0",
		},
		"audit_log": {
			"idempotency_key":   "TEXT",
			"request_digest":    "TEXT",
			"auth_subject":      "TEXT",
			"auth_node_id":      "TEXT",
			"auth_capability":   "TEXT",
			"auth_method":       "TEXT",
			"auth_decision":     "TEXT",
			"boundary_decision": "TEXT",
			"source_kind":       "TEXT",
		},
		// 舊 schema 沒有 recovery marker；ALTER 後會把每台最新一段 fail-closed
		// 標成 open，更舊的段才保留 DEFAULT 0。
		"canary_silent_failures": {
			"open": "INTEGER NOT NULL DEFAULT 0 CHECK (open IN (0,1))",
		},
		// generation=1 讓舊 DB 的 active row 有一個可持久化的起點；新 token
		// 格式包含 generation，因此升級前的 witness 仍會自然失效。
		"active_workload_policy": {
			"generation": "INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0)",
		},
		// 這張 projection 是 promote-lock slice 新增的；若有人曾以較早的
		// 候選版開過 DB，補欄後舊 witness 一律以空 evidence/token fail closed。
		"workload_observation_witness": {
			"evidence_at":           "TEXT NOT NULL DEFAULT ''",
			"openclaw_present":      "INTEGER NOT NULL DEFAULT 0 CHECK (openclaw_present IN (0,1))",
			"workload_policy_token": "TEXT NOT NULL DEFAULT ''",
		},
		// 舊 verification rows 只能從當時唯一的 machine-agent write path
		// 推知 executor role，row 本身沒有保存 provenance 或 Hub 收件時間。
		// provenance_recorded=0 與 received_at='' 必須保留這個差別。
		"verification_results": {
			"producer_kind":       "TEXT NOT NULL DEFAULT 'executor_agent'",
			"producer_id":         "TEXT NOT NULL DEFAULT ''",
			"evidence_role":       "TEXT NOT NULL DEFAULT 'executor'",
			"authority":           "TEXT NOT NULL DEFAULT 'machine_bearer_lease'",
			"provenance_recorded": "INTEGER NOT NULL DEFAULT 0 CHECK (provenance_recorded IN (0,1))",
			"received_at":         "TEXT NOT NULL DEFAULT ''",
			"observed_digest":     "TEXT NOT NULL DEFAULT ''",
			"observed_version":    "TEXT NOT NULL DEFAULT ''",
			"verifier_id":         "TEXT NOT NULL DEFAULT ''",
		},
		"job_events": {
			"producer_kind":       "TEXT NOT NULL DEFAULT 'executor_agent'",
			"producer_id":         "TEXT NOT NULL DEFAULT ''",
			"evidence_role":       "TEXT NOT NULL DEFAULT 'executor'",
			"authority":           "TEXT NOT NULL DEFAULT 'machine_bearer_lease'",
			"provenance_recorded": "INTEGER NOT NULL DEFAULT 0 CHECK (provenance_recorded IN (0,1))",
		},
		"artifact_fetch_operations": {
			"source_kind": "TEXT NOT NULL DEFAULT '' CHECK (length(CAST(source_kind AS BLOB)) <= 64)",
			"source_plan": "TEXT NOT NULL DEFAULT '' CHECK (length(CAST(source_plan AS BLOB)) <= 8192)",
		},
	}
	legacyFailureOpenAdded := false
	verificationTablePresent := false
	jobEventTablePresent := false
	for table, cols := range want {
		have, err := columnSet(tx, table)
		if err != nil {
			return err
		}
		if len(have) == 0 {
			continue // 表還不存在（不該發生，但不要在這裡爆炸）
		}
		if table == "verification_results" {
			verificationTablePresent = true
		}
		if table == "job_events" {
			jobEventTablePresent = true
		}
		for col, typ := range cols {
			if have[col] {
				continue
			}
			if _, err := tx.Exec(fmt.Sprintf(
				"ALTER TABLE %s ADD COLUMN %s %s", table, col, typ)); err != nil {
				return fmt.Errorf("add %s.%s: %w", table, col, err)
			}
			if table == "canary_silent_failures" && col == "open" {
				legacyFailureOpenAdded = true
			}
		}
	}
	if verificationTablePresent {
		// producer_id 的 legacy default 不能引用同一列 machine_id。這是由
		// 舊版唯一 writer endpoint 推得的 target identity，不是 row-recorded
		// provenance，所以 provenance_recorded 仍必須是 0。
		if _, err := tx.Exec(`UPDATE verification_results
		 SET producer_id=machine_id
		 WHERE provenance_recorded=0 AND producer_id=''`); err != nil {
			return fmt.Errorf("backfill legacy verification producer target: %w", err)
		}
	}
	if jobEventTablePresent {
		if _, err := tx.Exec(`UPDATE job_events
		 SET producer_id=(SELECT jobs.machine_id FROM jobs WHERE jobs.job_id=job_events.job_id)
		 WHERE provenance_recorded=0 AND producer_id=''`); err != nil {
			return fmt.Errorf("backfill legacy job event producer target: %w", err)
		}
	}
	if legacyFailureOpenAdded {
		// 舊 schema 沒有 recovery marker，不能把所有歷史段預設成 closed：
		// 每台最新一段可能仍持續中。先 fail-closed 標 open，等第一個明確
		// coherent healthy observation 再關；更舊的段維持 DEFAULT 0。
		if _, err := tx.Exec(`UPDATE canary_silent_failures
		 SET open=1
		 WHERE rowid IN (
		   SELECT current.rowid FROM canary_silent_failures current
		   WHERE current.rowid=(
		     SELECT newest.rowid FROM canary_silent_failures newest
		     WHERE newest.machine_id=current.machine_id
		     ORDER BY newest.last_seen_at DESC,newest.rowid DESC LIMIT 1
		   )
		 )`); err != nil {
			return fmt.Errorf("mark latest legacy canary failures open: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit add missing columns: %w", err)
	}
	return nil
}

type columnQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func columnSet(q columnQueryer, table string) (map[string]bool, error) {
	rows, err := q.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func (s *Store) Close() error {
	var err error
	if s.rdb != nil {
		err = s.rdb.Close()
	}
	if s.db != nil {
		err = errors.Join(err, s.db.Close())
	}
	return err
}

// DB 讓其他 package（例如備份、還原演練）拿到底層連線。讀多於寫時很有用。
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) now() time.Time { return s.nowFn() }

// Now is the Hub's own clock, which is the only clock any verdict is allowed to
// be measured against. Callers that need to stamp "as of when" use this rather
// than time.Now so a fixed-clock test measures what production measures.
func (s *Store) Now() time.Time { return s.nowFn() }

// ---------------------------------------------------------------- 時間與識別碼

// fmtTime 一律存 UTC 的 RFC3339。
//
// 三個好處：字串比大小就等於時間比大小（索引與 BETWEEN 才會對）、跨機器沒有
// 時區歧義、秒級精度讓 Agent 重試時 sent_at 完全一樣 —— 那正是 UPSERT 撞得到
// 同一列的前提。
func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// canonicalOperatorTime 承認一個時間值就是 Hub 寫收據時用的那個形狀：
// 非零、location 就是 time.UTC（Go 對任何零 offset 的 zone 都印 Z，
// 所以只比字串擋不住 +00:00）、而且落在整秒上。
func canonicalOperatorTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC &&
		value.Equal(value.UTC().Truncate(time.Second))
}

// nilIfZero：零值時間要寫成 NULL，不能寫成 "0001-01-01T00:00:00Z"。
// ⚠ 寫成字串的話，舊版 agent（根本不送這欄）會全部共用同一個「值」，
// COUNT(DISTINCT) 得到 1，看起來就像「重啟過一次」——
// 一個憑空捏造的事實。NULL 才是「不知道」。
func nilIfZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func fmtTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return fmtTime(*t)
}

// parseTime 防禦性解析。
//
// ⚠ 一列壞掉的資料不可以讓整個 Hub 掛掉 —— Hub 掛掉的時候沒有任何人在看，
// 而這個產品的整個賣點就是「它會在你沒看的時候替你看著」。解不出來就當零值，
// 上層看到零值會自己表現成 unknown。
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func parseTimeNull(ns sql.NullString) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	return parseTime(ns.String)
}

func parseTimePtr(ns sql.NullString) *time.Time {
	t := parseTimeNull(ns)
	if t.IsZero() {
		return nil
	}
	return &t
}

// newID 產生一個觀測 / 事件的主鍵。不用 uuid package —— 16 bytes 的 crypto/rand
// 已經夠，少一個直接相依。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在 Linux 上失敗等於系統壞了。這裡仍然要產出唯一值，
		// 因為丟掉一筆觀測比用一個難看的 ID 更糟。
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// newToken 產生 32 bytes 的隨機 token，base64url 無 padding（43 字，好貼）。
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken：⚠ 資料庫裡永遠只有 hash。拿到 DB 檔的人不能拿它去冒充任何一台機器。
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------- enrollment

// CreateEnrollToken 開一張一次性的 enroll token。回傳的明文只會出現這一次。
func (s *Store) CreateEnrollToken(displayName string, ttl time.Duration) (string, error) {
	_, tok, err := s.CreateEnrollTokenFor(displayName, ttl)
	return tok, err
}

// CreateEnrollTokenFor 開票，並且**同時把這台機器寫進名冊**。
// 回傳 machine_id 與明文 token。
//
// ⚠⚠ 「開票就進名冊」是這個產品的核心承諾，不是順手做的方便功能。
//
// 開一張給 sampleagent3 的票，就是一個人明確說出「我打算納管 sampleagent3」。從那一刻起
// sampleagent3 就在分母裡。如果它從此再也沒有報到，畫面上必須有一格紅燈寫著
// 「從未報到」—— 而不是它壓根不存在。
//
// 這條規則是被自己的程式打臉打出來的：原本這裡只寫 enrollment_tokens，名冊列
// 要等 Redeem 才建。於是「開了票、機器沒來、名冊上什麼都沒有」—— 完美重現了
// sampleagent3 在某台 client 上隱形七週的那個 bug，而且是在一個以「不要再發生那件事」
// 為存在理由的產品裡。
//
// ⚠ 票過期而沒被用掉時，名冊列**照樣留著**。那不是垃圾，那是「你說要納管、
// 但它沒來」這個事實。要讓它離開分母只有一條路：明確 RetireMachine。
//
// enrollment_tokens.used_by 在開票當下就填好，而 used_at 仍是 NULL ——
// used_by 的意思是「這張票綁定的名冊列」，用掉與否一律看 used_at。
func (s *Store) CreateEnrollTokenFor(displayName string, ttl time.Duration) (string, string, error) {
	if strings.TrimSpace(displayName) == "" {
		return "", "", errors.New("store: display name required")
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	tok, err := newToken()
	if err != nil {
		return "", "", err
	}
	now := s.now()
	machineID := newID()

	tx, err := s.beginWrite(context.Background(), "create_enroll_token_for")
	if err != nil {
		return "", "", fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	// 名冊列建立時尚未退役且 enrolled_at 為 NULL：已進分母但從未報到，
	// 判決層因此給 NeverReported。expected=1 只維持舊 schema invariant。
	if _, err := tx.Exec(`
INSERT INTO machine_registry (machine_id, display_name, expected, created_at)
VALUES (?,?,1,?)`, machineID, displayName, fmtTime(now)); err != nil {
		return "", "", fmt.Errorf("store: pre-register machine: %w", err)
	}
	if _, err := tx.Exec(`
INSERT INTO enrollment_tokens (token_hash, display_name, created_at, expires_at, used_by)
VALUES (?,?,?,?,?)`,
		hashToken(tok), displayName, fmtTime(now), fmtTime(now.Add(ttl)), machineID); err != nil {
		return "", "", fmt.Errorf("store: create enroll token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("store: commit: %w", err)
	}
	return machineID, tok, nil
}

// RedeemEnrollToken 用一次性 token 換一個長期身分。用過即焚。
//
// 整個流程在一個 transaction 裡：檢查 token、建名冊列、標記 token 已用。
// ⚠ 少了 transaction，兩台同時拿同一張 token 打進來會各拿到一個身分，
// 然後你就有兩台機器共用一張票而且不知道。
func (s *Store) RedeemEnrollToken(tok string, req model.EnrollRequest, now time.Time) (string, string, error) {
	now = now.UTC()
	tx, err := s.beginWrite(context.Background(), "redeem_enroll_token")
	if err != nil {
		return "", "", fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	var displayName, expiresAt string
	var usedAt, boundTo sql.NullString
	err = tx.QueryRow(
		`SELECT display_name, expires_at, used_at, used_by FROM enrollment_tokens WHERE token_hash = ?`,
		hashToken(tok)).Scan(&displayName, &expiresAt, &usedAt, &boundTo)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrTokenInvalid
	}
	if err != nil {
		return "", "", fmt.Errorf("store: lookup enroll token: %w", err)
	}
	if usedAt.Valid && usedAt.String != "" {
		return "", "", ErrTokenUsed
	}
	if exp := parseTime(expiresAt); exp.IsZero() || !now.Before(exp) {
		// ⚠ 解不出來的 expires_at 也算過期。壞掉的時間戳不可以變成永久有效的 token。
		return "", "", ErrTokenExpired
	}

	agentTok, err := newToken()
	if err != nil {
		return "", "", err
	}

	// ⚠ 名冊列在開票時就已經建好了（見 CreateEnrollTokenFor）。這裡是**填滿**它，
	// 不是再建一個。少了這一段，同一台機器會有兩列：一列是開票時建的、
	// 永遠 NeverReported，另一列是報到後建的、正常運作 —— 而畫面上會同時出現兩台
	// 名字一樣的機器，其中一台永遠紅著。
	// ⚠ 票綁的那台機器退役了就不准兌換。
	//
	// 少了這一段，退役的機器照樣報到成功、拿到 agent token，然後之後每一個
	// 請求都 401（AuthenticateAgent 濾掉 retired）。裝好了卻一直 401
	// 比一開始就被拒絕難查得多 —— 而且 log 上看起來像憑證壞掉。
	//
	// ⚠ 這也是「發票」這個動作的反悔路徑之一：改變主意就 retire 那台，
	// 已經發出去的票會跟著失效。
	if boundTo.Valid && boundTo.String != "" {
		var retiredAt sql.NullString
		if err := tx.QueryRow(
			`SELECT retired_at FROM machine_registry WHERE machine_id = ?`,
			boundTo.String).Scan(&retiredAt); err == nil && retiredAt.Valid && retiredAt.String != "" {
			return "", "", ErrTokenRetired
		}
	}

	machineID := boundTo.String
	if machineID == "" {
		// 舊版開的票沒有綁定名冊列。補建一列，讓舊 token 還能用完。
		machineID = newID()
		if _, err := tx.Exec(`
INSERT INTO machine_registry (machine_id, display_name, expected, created_at)
VALUES (?,?,1,?)`, machineID, displayName, fmtTime(now)); err != nil {
			return "", "", fmt.Errorf("store: create machine: %w", err)
		}
	}
	// ⚠ display_name 不動：那是人取的名字，機器自報的 hostname 不可以蓋掉它
	// （實測 5 台裡有 3 台的 hostname 跟人記得的名字不一樣）。
	// ⚠ created_at 也不動：名冊列的年齡是事實，報到不會讓它重新出生。
	res, err := tx.Exec(`
UPDATE machine_registry SET
  hostname = ?, unix_user = ?, os = ?, arch = ?, tailscale_ip = ?,
  machine_id_hint = COALESCE(machine_id_hint, ?),
  agent_token_hash = ?, enrolled_at = ?, expected = 1
WHERE machine_id = ?`,
		nullStr(req.Hostname), nullStr(req.UnixUser), nullStr(req.OS), nullStr(req.Arch),
		nullStr(req.TailscaleIP), nullStr(req.MachineIDHint),
		hashToken(agentTok), fmtTime(now), machineID)
	if err != nil {
		return "", "", fmt.Errorf("store: fill machine: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 票綁的名冊列不見了（有人手動刪了資料）。這時候寧可讓報到失敗，
		// 也不要無聲地生一個沒人宣告過的身分出來。
		return "", "", fmt.Errorf("store: registry row %s bound to this ticket does not exist", machineID)
	}
	if _, err := tx.Exec(
		`UPDATE enrollment_tokens SET used_at = ?, used_by = ? WHERE token_hash = ? AND used_at IS NULL`,
		fmtTime(now), machineID, hashToken(tok)); err != nil {
		return "", "", fmt.Errorf("store: burn enroll token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("store: commit: %w", err)
	}
	return machineID, agentTok, nil
}

// AgentTokenCurrent reports whether bearer is still machineID's agent token
// on an unretired roster row. A long-lived connection authenticated once at
// upgrade (the terminal link) calls this to notice a re-enrollment or
// retirement that replaced or retired the credential it was opened with.
// The comparison is constant time, like AuthenticateAgent.
func (s *Store) AgentTokenCurrent(machineID, bearer string) (bool, error) {
	if machineID == "" || bearer == "" {
		return false, nil
	}
	var stored sql.NullString
	err := s.rdb.QueryRow(`SELECT agent_token_hash FROM machine_registry
	 WHERE machine_id=? AND retired_at IS NULL`, machineID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: agent token check: %w", err)
	}
	if !stored.Valid || stored.String == "" {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(stored.String), []byte(hashToken(bearer))) == 1, nil
}

// AuthenticateAgent 用 bearer token 換 machine_id。
//
// ⚠ 兩件事不能省：
//  1. 常數時間比對。用 WHERE agent_token_hash = ? 讓資料庫比會把比對耗時
//     洩漏出去，而且掃完整張表才回傳，讓耗時跟命中在第幾列無關。
//  2. 已 retire 的機器不給過。retire 的定義就是「它不該再寫東西進來了」；
//     讓它繼續寫會讓一台已經處理掉的機器重新出現在畫面上。
func (s *Store) AuthenticateAgent(bearer string) (string, error) {
	want := []byte(hashToken(bearer))
	rows, err := s.rdb.Query(`
SELECT machine_id, agent_token_hash FROM machine_registry
 WHERE agent_token_hash IS NOT NULL AND agent_token_hash <> '' AND retired_at IS NULL`)
	if err != nil {
		return "", fmt.Errorf("store: auth: %w", err)
	}
	defer rows.Close()

	found := ""
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			return "", fmt.Errorf("store: auth scan: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(h), want) == 1 {
			found = id
		}
		// ⚠ 不 break。提早離開會讓「命中第一列」比「命中最後一列」快。
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("store: auth rows: %w", err)
	}
	if found == "" {
		return "", ErrUnauthorized
	}
	return found, nil
}

// ---------------------------------------------------------------- 名冊

const machineCols = `rowid, machine_id, display_name, channel, channel_revision, lifecycle_revision,
	assigned_user_id, assigned_user_login, assigned_user_revision, hostname, expected, unix_user, os, arch,
	tailscale_ip, machine_id_hint, linger_enabled, notes, created_at, enrolled_at, retired_at`

type rowScanner interface{ Scan(dest ...any) error }

// scanMachine 把一列名冊讀成 Machine。所有可為 NULL 的欄位都走 sql.Null*，
// 因為 schema 裡它們真的可以是 NULL，而「拿不到」的答案是 unknown，不是零值。
func scanMachine(sc rowScanner) (Machine, error) {
	var m Machine
	var channel, hostname, unixUser, osName, arch, tsIP, hint, notes sql.NullString
	var assignedUserID, assignedUserLogin sql.NullString
	var createdAt sql.NullString
	var enrolledAt, retiredAt sql.NullString
	var expected int64
	var linger sql.NullInt64
	err := sc.Scan(&m.RegistrySequence, &m.MachineID, &m.DisplayName, &channel, &m.ChannelRevision, &m.LifecycleRevision,
		&assignedUserID, &assignedUserLogin, &m.AssignedUserRevision, &hostname, &expected, &unixUser, &osName,
		&arch, &tsIP, &hint, &linger, &notes, &createdAt, &enrolledAt, &retiredAt)
	if err != nil {
		return Machine{}, err
	}
	m.Channel = channel.String
	m.AssignedUserID = assignedUserID.String
	m.AssignedUserLogin = assignedUserLogin.String
	m.Hostname, m.UnixUser, m.OS, m.Arch = hostname.String, unixUser.String, osName.String, arch.String
	m.TailscaleIP, m.MachineIDHint, m.Notes = tsIP.String, hint.String, notes.String
	m.Expected = expected != 0
	if linger.Valid {
		b := linger.Int64 != 0
		m.LingerEnabled = &b
	}
	m.CreatedAt = parseTimeNull(createdAt)
	m.EnrolledAt = parseTimePtr(enrolledAt)
	m.RetiredAt = parseTimePtr(retiredAt)
	return m, nil
}

// UpsertMachine 建立或更新名冊上的一列。這是「人在編輯名冊」的入口 ——
// 先把機器寫進名冊、之後它才報到，是這個產品刻意支援的順序（那台會是 NeverReported）。
//
// ⚠ 不動 agent_token_hash：編輯顯示名稱不該把一台機器的身分洗掉。
// ⚠ created_at 用 COALESCE 保留原值：名冊列的年齡是事實，不是可編輯欄位。
func (s *Store) UpsertMachine(m Machine) error {
	if strings.TrimSpace(m.MachineID) == "" {
		return errors.New("store: machine_id required")
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = s.now()
	}
	var linger any
	if m.LingerEnabled != nil {
		linger = boolToInt(*m.LingerEnabled)
	}

	tx, err := s.beginWrite(context.Background(), "upsert_machine")
	if err != nil {
		return fmt.Errorf("store: begin upsert machine: %w", err)
	}
	defer tx.Rollback()

	var storedCreated string
	var storedRetired sql.NullString
	var storedLifecycleRevision int64
	exists := true
	if err := tx.QueryRow(`SELECT created_at,retired_at,lifecycle_revision FROM machine_registry WHERE machine_id=?`, m.MachineID).
		Scan(&storedCreated, &storedRetired, &storedLifecycleRevision); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: read machine lifecycle before upsert: %w", err)
		}
		exists = false
	}
	if exists && (storedLifecycleRevision < 0 || storedLifecycleRevision >= MaxMachineLifecycleRevision) {
		return fmt.Errorf("store: upsert machine: %w", ErrLifecycleRevisionLimit)
	}

	desiredRetired := m.RetiredAt != nil && !m.RetiredAt.IsZero()
	currentlyRetired := exists && storedRetired.Valid
	var retiredValue any
	var lifecycleEvent string
	var lifecycleAt time.Time
	var initialLifecycleRevision int64
	switch {
	case currentlyRetired && desiredRetired:
		// Being retired is a lifecycle state, not an editable timestamp. Preserve
		// the first transition coordinate when an unrelated roster edit writes
		// back a Machine value with a different non-nil RetiredAt.
		retiredValue = storedRetired.String
	case !currentlyRetired && desiredRetired:
		lifecycleAt = *m.RetiredAt
		retiredValue = fmtTime(lifecycleAt)
		lifecycleEvent = ChangeReadRetired
		if !exists {
			initialLifecycleRevision = 1
		}
	case currentlyRetired && !desiredRetired:
		// A nil RetiredAt carries the desired state but no transition time. The
		// Store clock is the Hub-owned coordinate for this real unretire.
		lifecycleAt = s.now()
		lifecycleEvent = ChangeReadRegistered
	}
	if lifecycleEvent != "" && exists && storedLifecycleRevision >= MaxMachineLifecycleRevision-1 {
		return fmt.Errorf("store: upsert machine: %w", ErrLifecycleRevisionLimit)
	}
	if lifecycleEvent != "" {
		lifecycleAt, err = canonicalMachineLifecycleTime(lifecycleAt)
		if err != nil {
			return fmt.Errorf("store: upsert machine lifecycle time: %w", err)
		}
		rawCreated := storedCreated
		if !exists {
			rawCreated = fmtTime(created)
		}
		if err := validateMachineLifecycleTimeTx(tx, m.MachineID, rawCreated, storedRetired, lifecycleAt); err != nil {
			return fmt.Errorf("store: upsert machine lifecycle time: %w", err)
		}
		if lifecycleEvent == ChangeReadRetired {
			retiredValue = fmtTime(lifecycleAt)
		}
	}

	// ⚠ assigned_user_* 不採用這次寫入的值。名冊編輯與報到帶進來的是零值，
	// 寫進去會把 operator 的指派與 revision 洗成未指派。
	if _, err := tx.Exec(`
INSERT INTO machine_registry
  (machine_id, display_name, lifecycle_revision, assigned_user_id, assigned_user_login, assigned_user_revision,
   hostname, expected, unix_user, os, arch, tailscale_ip,
   machine_id_hint, linger_enabled, notes, created_at, enrolled_at, retired_at)
VALUES (?,?,?,NULL,NULL,0,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(machine_id) DO UPDATE SET
  display_name    = excluded.display_name,
  hostname        = excluded.hostname,
  expected        = excluded.expected,
  unix_user       = excluded.unix_user,
  os              = excluded.os,
  arch            = excluded.arch,
  tailscale_ip    = excluded.tailscale_ip,
  machine_id_hint = excluded.machine_id_hint,
  linger_enabled  = excluded.linger_enabled,
  notes           = excluded.notes,
  created_at      = COALESCE(machine_registry.created_at, excluded.created_at),
  enrolled_at     = excluded.enrolled_at,
	retired_at      = excluded.retired_at,
	assigned_user_id = machine_registry.assigned_user_id,
	assigned_user_login = machine_registry.assigned_user_login,
	assigned_user_revision = machine_registry.assigned_user_revision,
	lifecycle_revision = CASE
	  WHEN excluded.retired_at IS NOT machine_registry.retired_at
	  THEN machine_registry.lifecycle_revision + 1
	  ELSE machine_registry.lifecycle_revision
	END`,
		m.MachineID, m.DisplayName, initialLifecycleRevision, nullStr(m.Hostname), boolToInt(m.Expected), nullStr(m.UnixUser),
		nullStr(m.OS), nullStr(m.Arch), nullStr(m.TailscaleIP), nullStr(m.MachineIDHint), linger,
		nullStr(m.Notes), fmtTime(created), fmtTimePtr(m.EnrolledAt), retiredValue); err != nil {
		return fmt.Errorf("store: upsert machine projection: %w", err)
	}
	if exists && !currentlyRetired && desiredRetired {
		if _, err := closeOpenAgentSessionsForMachine(tx, m.MachineID, lifecycleAt, AgentSessionCloseReasonMachineRetired); err != nil {
			return fmt.Errorf("store: close sessions after upsert machine retirement: %w", err)
		}
	}
	if lifecycleEvent != "" {
		if _, err := tx.Exec(`
INSERT INTO machine_registry_lifecycle_events(machine_id,event_type,occurred_at)
VALUES(?,?,?)`, m.MachineID, lifecycleEvent, fmtTime(lifecycleAt)); err != nil {
			return fmt.Errorf("store: upsert machine lifecycle evidence: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit upsert machine: %w", err)
	}
	return nil
}

// ListMachines 回傳名冊上每一台，包含已 retire 的。過濾是呼叫端的事 ——
// ⚠ 這個函式不會因為一台機器「看起來沒用了」就把它藏起來。
func (s *Store) ListMachines() ([]Machine, error) {
	rows, err := s.rdb.Query(`SELECT ` + machineCols + ` FROM machine_registry ORDER BY display_name, machine_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list machines: %w", err)
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		m, err := scanMachine(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan machine: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetMachine(id string) (Machine, error) {
	m, err := scanMachine(s.rdb.QueryRow(`SELECT `+machineCols+` FROM machine_registry WHERE machine_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Machine{}, ErrNotFound
	}
	if err != nil {
		return Machine{}, fmt.Errorf("store: get machine: %w", err)
	}
	return m, nil
}

// RetireMachine 讓一台機器離開分母。
//
// ⚠ 不刪任何東西。心跳、觀測、狀態歷史全部留著 ——
// 「這台退場之前發生了什麼」是事後唯一查得到的線索。
// expected 也不動：那是當初的意圖，是事實的一部分。
//
// 除了 retired_at，它還會關掉這台機器還開著的 agent session：退役是
// operator 的撤銷手段，留著已授權的 session 就等於沒撤銷。
func (s *Store) RetireMachine(id string, now time.Time) error {
	return s.setMachineLifecycle(id, true, now)
}

// withoutRawOccupancy 把占用證據從要存進 observed_state 的快照裡拿掉。
//
// ⚠ 這不是為了省空間而丟資料 —— 同一批證據**已經逐筆正規化**寫進
// ticket_occupancy_observation 了，那張表才是查得動的那一份。
// 留在 JSON 裡的那一份沒有任何程式碼讀（讀的是 machineReadOccupancy），
// 它只是同一件事的第二個副本。
//
// 實測（samplehub1，2026-09-03，機隊上線 11 小時後）：
//
//	kind=openclaw  303 列  7.5 MB   ← 佔整個資料庫的 92%，每列 25 KB
//	其他全部合計   4851 列  0.7 MB
//
// 每 10 分鐘一次、5 台，一年是 5 GB，而其中大部分是同一批 session_key
// 被重寫了幾萬遍。⚠ 統計用的純量（occupancy_rows_seen /
// occupancy_rows_no_provider）要留著 —— 週報在讀它們，
// 而且它們回答的是「有多少證據沒進帳」，那正好是帳本本身答不出來的事。
func withoutRawOccupancy(oc model.OpenClaw) model.OpenClaw {
	if oc.DB == nil || len(oc.DB.Occupancy) == 0 {
		return oc
	}
	// ⚠ 複製一份再改。原本那個 b.OpenClaw.DB 等一下還要交給
	// recordOccupancy 逐筆寫進帳本 —— 就地改會把帳本清空。
	db := *oc.DB
	db.Occupancy = nil
	oc.DB = &db
	return oc
}

// ---------------------------------------------------------------- 心跳

// RecordCheckin 記一次心跳。⚠ UPSERT-only，PK = (machine_id, sent_at)。
//
// 30 台帶 jitter 打進來、回應在網路抖動時掉了、Agent 重試 —— 任何非冪等的
// 實作都會在第一次抖動時長出重複列，然後 BootIDChanges1h 這種「數不同值」的
// 查詢就開始說謊。
//
// ⚠ received_at 與 clock_skew_seconds 不在 DO UPDATE 裡：第一次收到的時間才是
// 真的量測值。重試把它改成「現在」會讓時鐘偏移的量測失真，而那是我們用來判斷
// 「這台回報的時間可不可信」的唯一依據。
func (s *Store) RecordCheckin(machineID string, c model.Checkin, receivedAt time.Time) error {
	// ⚠ 規格明講：ClockSkew = sent_at - received_at。正值 = 機器時鐘比 Hub 快。
	skew := int64(c.SentAt.Sub(receivedAt) / time.Second)
	var obsAge any
	if c.ObservationAgeSeconds != nil {
		obsAge = *c.ObservationAgeSeconds
	}
	var jobsEnabled any
	if c.JobsEnabled != nil {
		jobsEnabled = *c.JobsEnabled
	}
	tx, err := s.beginWrite(context.Background(), "record_checkin")
	if err != nil {
		return fmt.Errorf("store: begin checkin: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
INSERT INTO machine_checkins
  (machine_id, sent_at, received_at, agent_version, boot_id, agent_seq, uptime_seconds,
   disk_free_bytes, disk_total_bytes, jobs_enabled, observation_age_seconds,
   max_seen_revision, max_applied_revision, clock_skew_seconds, agent_started_at,
   settings_digest)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(machine_id, sent_at) DO UPDATE SET
  agent_version           = excluded.agent_version,
  boot_id                 = excluded.boot_id,
  agent_seq               = excluded.agent_seq,
  uptime_seconds          = excluded.uptime_seconds,
  disk_free_bytes         = excluded.disk_free_bytes,
  disk_total_bytes        = excluded.disk_total_bytes,
  jobs_enabled            = excluded.jobs_enabled,
  observation_age_seconds = excluded.observation_age_seconds,
  max_seen_revision       = excluded.max_seen_revision,
  max_applied_revision    = excluded.max_applied_revision,
  agent_started_at        = excluded.agent_started_at,
  settings_digest         = excluded.settings_digest`,
		machineID, fmtTime(c.SentAt), fmtTime(receivedAt), nullStr(c.AgentVersion), nullStr(c.BootID),
		c.AgentSeq, c.UptimeSeconds, c.DiskFreeBytes, c.DiskTotalBytes, jobsEnabled, obsAge,
		c.MaxSeenRevision, c.MaxAppliedRevision, skew, fmtTimePtr(nilIfZero(c.AgentStartedAt)),
		nullStr(c.SettingsDigest))
	if err != nil {
		if isForeignKeyErr(err) {
			return fmt.Errorf("store: checkin from %q: %w", machineID, ErrNotFound)
		}
		return fmt.Errorf("store: record checkin: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM machine_job_capabilities
	 WHERE machine_id=? AND sent_at=? AND capability=?`, machineID, fmtTime(c.SentAt), model.DeviceSyncJobKind); err != nil {
		return fmt.Errorf("store: replace checkin job capability: %w", err)
	}
	if c.DeviceSyncV1 {
		if _, err := tx.Exec(`INSERT INTO machine_job_capabilities
		 (machine_id,sent_at,capability,supported) VALUES (?,?,?,1)`,
			machineID, fmtTime(c.SentAt), model.DeviceSyncJobKind); err != nil {
			return fmt.Errorf("store: record checkin job capability: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM machine_job_capabilities
	 WHERE machine_id=? AND sent_at=? AND capability=?`,
		machineID, fmtTime(c.SentAt), model.MaintenanceDiskCleanCapability); err != nil {
		return fmt.Errorf("store: replace disk-clean job capability: %w", err)
	}
	if c.MaintenanceDiskCleanV1 {
		if _, err := tx.Exec(`INSERT INTO machine_job_capabilities
		 (machine_id,sent_at,capability,supported) VALUES (?,?,?,1)`,
			machineID, fmtTime(c.SentAt), model.MaintenanceDiskCleanCapability); err != nil {
			return fmt.Errorf("store: record disk-clean job capability: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit checkin: %w", err)
	}
	return nil
}

// isForeignKeyErr：FK 違反的唯一實際原因是機器不在名冊上。翻成人看得懂的錯誤，
// 因為這個錯誤會直接變成 Agent 收到的 HTTP 回應。
func isForeignKeyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// ---------------------------------------------------------------- 完整觀測

// RecordObservation 記一批完整觀測。⚠ append-only：每個 (kind, subject) 插一列新的，
// 永遠不 UPDATE、不 DELETE、不「只留最新一筆」。
//
// 如果你正打算把這裡改成一台一列的快照 —— 請先讀 docs/SPEC.md §4.2。
// 上游的資料庫 7 天 / 每 job 2000 筆就把歷史吃掉了，Hub 是唯一還記得
// 「今天綠的東西星期三死了」的地方。而 Dashboard 的預設畫面就是那個問題的答案。
// recordOccupancy 把 cron 執行證據寫進占用帳本。
//
// ⚠⚠ 三條規則，每一條都有人踩過：
//
//  1. **只 append，永不更新。** observation_id 是內容的雜湊，重送同一筆會
//     命中 INSERT OR IGNORE 而不是覆蓋。帳本被改寫過就不是帳本了。
//  2. **沒有 provider 的回合不寫。** 那是 SPEC 的硬規則「沒證據不准寫」。
//     實測缺 provider 的多半正好是失敗的回合 —— 也就是最想知道
//     「當時用的是哪張票」的那些。缺口要留著，不要用猜的補平。
//  3. **process_alive 永遠寫 0。** 「有個 process 活著」不是占用的證據。
//     用它假裝占用，帳本會在每一台開著 OpenClaw 的機器上都顯示滿載。
func recordOccupancy(tx dbTx, machineID string, d *model.OpenClawDB, receivedAt string) error {
	if d == nil || len(d.Occupancy) == 0 {
		return nil
	}
	ins, err := tx.Prepare(`
INSERT OR IGNORE INTO ticket_occupancy_observation
  (observation_id, machine_id, profile_id, provider, model,
   measured_at, received_at, occupant_evidence, job_id, agent_id,
   process_alive, run_status, total_tokens, last_error_text, source)
VALUES (?,?,NULL,?,?,?,?,?,?,?,0,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("store: prepare occupancy: %w", err)
	}
	defer ins.Close()

	for _, e := range d.Occupancy {
		if e.Provider == "" {
			continue // 規則 2。probe 已經濾過一次，這裡是第二道。
		}
		// ⚠ id 由內容決定，不是隨機的。agent 每 10 分鐘重送最近 200 筆，
		// 用隨機 id 會讓同一個 cron 回合在帳本上出現幾十次，
		// 而一本會自我複製的帳，比沒有帳更難查。
		id := occupancyID(machineID, e.Source, e.JobID, e.At)
		if _, err := ins.Exec(id, machineID, e.Provider, nullStr(e.Model),
			fmtTime(e.At), receivedAt, e.SessionKey, nullStr(e.JobID), nullStr(e.AgentID),
			nullStr(e.Status), nullInt(e.TotalTokens), nullStr(e.ErrorText),
			e.Source); err != nil {
			if isForeignKeyErr(err) {
				return fmt.Errorf("store: occupancy from %q: %w", machineID, ErrNotFound)
			}
			return fmt.Errorf("store: insert occupancy: %w", err)
		}
	}
	return nil
}

func occupancyID(machineID, source, jobID string, at time.Time) string {
	sum := sha256.Sum256([]byte(machineID + "\x00" + source + "\x00" + jobID + "\x00" + fmtTime(at)))
	return hex.EncodeToString(sum[:16])
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func (s *Store) RecordObservation(machineID string, b model.ObservationBatch, receivedAt time.Time) error {
	tx, err := s.beginWrite(context.Background(), "record_observation")
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()
	var displayName string
	if err := tx.QueryRow(`SELECT display_name FROM machine_registry WHERE machine_id=?`, machineID).Scan(&displayName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: observation from %q: %w", machineID, ErrNotFound)
		}
		return fmt.Errorf("store: observation machine: %w", err)
	}

	ins, err := tx.Prepare(`
INSERT INTO observed_state
  (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("store: prepare observation: %w", err)
	}
	defer ins.Close()

	measured, recv := fmtTime(b.MeasuredAt), fmtTime(receivedAt)
	put := func(kind, subject string, v any) error {
		payload, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("store: marshal %s/%s: %w", kind, subject, err)
		}
		_, err = ins.Exec(newID(), machineID, measured, recv, kind, subject,
			string(payload), SourceAgentMeasurement)
		if err != nil {
			if isForeignKeyErr(err) {
				return fmt.Errorf("store: observation from %q: %w", machineID, ErrNotFound)
			}
			return fmt.Errorf("store: insert %s/%s: %w", kind, subject, err)
		}
		return nil
	}

	if err := put(KindIdentity, KindIdentity, b.Identity); err != nil {
		return err
	}
	if err := put(KindResources, KindResources, b.Resources); err != nil {
		return err
	}
	if err := put(KindOpenClaw, KindOpenClaw, withoutRawOccupancy(b.OpenClaw)); err != nil {
		return err
	}
	if err := put(KindBAT, KindBAT, b.BAT); err != nil {
		return err
	}
	for _, u := range b.Systemd {
		// subject 用原始 unit 名稱，即使是空字串。⚠ 不猜、不補、不丟掉 ——
		// 名字空掉本身就是要讓人看到的事實。
		if err := put(KindSystemd, u.Name, u); err != nil {
			return err
		}
	}
	for _, a := range b.Artifacts {
		// subject 用 unit + 路徑：同一個 unit 可以宣告多個產出物。
		if err := put(KindArtifact, a.Unit+"\x1f"+a.Artifact, a); err != nil {
			return err
		}
	}
	for _, e := range b.Events {
		// subject 跟 artifact 對齊（unit + 路徑），artifactFacts 才接得起來。
		if err := put(KindEvents, e.Unit+"\x1f"+e.Path, e); err != nil {
			return err
		}
	}
	// ⚠⚠ journal 跟 systemd 分成兩個 kind 是**故意的**，不要合併成一列。
	// systemd 那一列會被 Facts() 讀去餵 state 判定；journal 這一列不會，
	// 而且不可以。分成兩個 kind 讓「journal 不准影響判定」由結構擋著。
	// 理由見 model.UnitJournal 的註解與 PRODUCT.md 地基二。
	for _, j := range b.Journals {
		if err := put(KindJournal, j.Unit, j); err != nil {
			return err
		}
	}
	for _, c := range b.Credentials {
		if err := put(KindCredential, c.Provider, c); err != nil {
			return err
		}
	}
	for _, t := range b.CLITools {
		if err := put(KindCLITool, t.Name, t); err != nil {
			return err
		}
	}

	// credential_on_machine 是「目前狀態」的投影，可以覆蓋 —— 因為歷史在
	// observed_state 裡，不在這張表裡。⚠ 這個豁免只適用於有別處保存歷史的投影表。
	// ON CONFLICT 只更新這裡列出的欄位：verified_at / last_real_use_at / profile_id
	// 來自別的證據來源（真實請求、占用帳本），觀測不准把它們洗掉 ——
	// 只有 verified_at 能給綠燈。
	//
	// ⚠ verification_method 歸觀測管，verified_at 不歸。這兩個看起來成對，
	// 其實回答的是不同問題：method 講「上面那個 status 是怎麼算出來的」
	// （今天永遠是 file_parse），verified_at 講「最後一次真的拿它去打請求
	// 是什麼時候」（今天永遠是 NULL，因為我們不花使用者的額度去打假請求）。
	// 把它們綁在一起就會出現「method=file_parse 所以 verified_at 該清空」
	// 這種推論，而那會把別人辛苦驗來的證據洗掉。
	for _, c := range b.Credentials {
		if _, err := tx.Exec(`
INSERT INTO credential_on_machine
  (machine_id, provider, status, reported_expiry_at, last_refresh_at, file_mtime,
   active_account_id, verification_method, last_error, note, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(machine_id, provider) DO UPDATE SET
  status              = excluded.status,
  reported_expiry_at  = excluded.reported_expiry_at,
  last_refresh_at     = excluded.last_refresh_at,
  file_mtime          = excluded.file_mtime,
  active_account_id   = excluded.active_account_id,
  verification_method = excluded.verification_method,
  last_error          = excluded.last_error,
  note                = excluded.note,
  updated_at          = excluded.updated_at`,
			machineID, c.Provider, string(c.Status), fmtTimePtr(c.ExpiresAt),
			fmtTimePtr(c.LastRefresh), fmtTimePtr(c.FileMTime),
			nullStr(c.ActiveAccountID), nullStr(string(c.VerificationMethod)),
			nullStr(c.LastError), nullStr(c.Note), recv); err != nil {
			return fmt.Errorf("store: credential projection: %w", err)
		}
	}

	// 占用帳本。⚠ append-only，而且**只寫有 provider 的那些**。
	if err := recordOccupancy(tx, machineID, b.OpenClaw.DB, recv); err != nil {
		return err
	}

	// ⚠ 只在名冊還沒有 machine-id 的時候補上。已經有值就絕對不覆蓋。
	//
	// 這不是最佳化，這是規則。覆蓋掉舊的 hint 等於 Hub 偷偷把身分衝突「解決」了 ——
	// 而衝突必須讓人看見；目前唯一明確的處置是退役舊身分、重新 enroll。
	// 機器不准自己宣告自己是誰，也不能靠後續正常回報把衝突洗掉。
	if b.Identity.MachineIDHint != "" {
		if _, err := tx.Exec(`
UPDATE machine_registry SET machine_id_hint = ?
 WHERE machine_id = ? AND (machine_id_hint IS NULL OR machine_id_hint = '')`,
			b.Identity.MachineIDHint, machineID); err != nil {
			return fmt.Errorf("store: fill machine_id_hint: %w", err)
		}
	}
	// 身分證據是 non-expiring latch。它必須先於 recovery eligibility 更新，
	// 讓一批同時帶來新 machine-id 與 healthy workload 的觀測不能把 failure 關掉。
	if err := recordIdentityHintTx(tx, machineID, b.Identity.MachineIDHint, receivedAt); err != nil {
		return err
	}

	// workload verdict 的 raw rows 與 promote evidence 必須是同一個 commit。
	// 一批只有三種結果：明確失敗就開／延長，證據完整且健康就關，
	// 其他都是 unknown 且不動 span。絕不從不同 measured_at 的 latest rows 拼出 recovery。
	policy, processPolicyMatches, err := s.matchingActiveWorkloadPolicy(tx)
	if err != nil {
		return err
	}
	expectedPolicyToken := ""
	if processPolicyMatches {
		expectedPolicyToken = workloadPolicyToken(policy.fingerprint, policy.generation, displayName)
	}
	policyValid := processPolicyMatches && policy.valid
	policyMatchesBatch := policyValid && b.WorkloadPolicyToken != "" && b.WorkloadPolicyToken == expectedPolicyToken
	facts, recoveryComplete := s.workloadFactsFromObservation(displayName, b, receivedAt)
	clockSkew, trustedCheckin, err := latestTrustedCheckinTx(tx, machineID, receivedAt)
	if err != nil {
		return fmt.Errorf("store: observation evidence clock: %w", err)
	}
	evidenceAt, evidenceTimeCoherent := normalizedObservationEvidenceAt(
		b.MeasuredAt, receivedAt, clockSkew, trustedCheckin)
	previousEvidenceAt, err := workloadEvidenceWatermarkTx(tx, machineID)
	if err != nil {
		return err
	}
	evidenceIsNew := evidenceTimeCoherent &&
		(previousEvidenceAt.IsZero() || evidenceAt.After(previousEvidenceAt))
	recoveryComplete = recoveryComplete && evidenceIsNew && policyValid && policyMatchesBatch
	firstFailure := func(candidate state.Facts) string {
		for _, finding := range state.WorkloadFindings(candidate) {
			if finding.Kind == "workload" && !finding.Advisory {
				return finding.Message
			}
		}
		return ""
	}
	// Policy token 只替 Artifacts/Events 證明「用了哪份規則」。OpenClaw
	// present/process/task 是 policy-independent L1；即使舊 Agent 還沒拿到
	// 新規則，它明確回報 PID=0 或 task 沉默仍必須立即開 span。這種壞消息
	// 以 Hub received_at 作證，不受 Agent 時鐘或 recovery watermark 限制；
	// 否則 delayed/same-second bad batch 會被較新的 healthy evidence 吃掉。
	coreFacts := facts
	coreFacts.Artifacts = nil
	failureReason := firstFailure(coreFacts)
	if failureReason == "" {
		failureReason = explicitInstallFailure(b.OpenClaw)
	}
	if failureReason == "" {
		failureReason = explicitDatabaseFailure(b.OpenClaw.DB)
	}
	// Artifact/event 是 policy-dependent：壞消息不必比 healthy watermark 新，
	// 但仍必須有可信量測時間，而且 token 要精確對上 active generation。
	if failureReason == "" && evidenceTimeCoherent && policyMatchesBatch {
		failureReason = firstFailure(facts)
	}
	verdict := workloadWitnessUnknown
	if failureReason != "" {
		verdict = workloadWitnessFailure
		if _, err := recordCanarySilentFailureTx(tx, machineID, failureReason, receivedAt); err != nil {
			return fmt.Errorf("store: observation failure projection: %w", err)
		}
	} else if recoveryComplete {
		eligible, err := observationRecoveryEligibleTx(tx, machineID, receivedAt)
		if err != nil {
			return fmt.Errorf("store: observation recovery eligibility: %w", err)
		}
		afterFailures, err := evidenceAfterOpenFailuresTx(tx, machineID, evidenceAt)
		if err != nil {
			return fmt.Errorf("store: observation recovery ordering: %w", err)
		}
		if eligible && afterFailures {
			verdict = workloadWitnessHealthy
			if err := closeCanarySilentFailureWithEvidenceTx(tx, machineID, evidenceAt, receivedAt); err != nil {
				return fmt.Errorf("store: observation recovery projection: %w", err)
			}
		}
	}
	runningVersion := ""
	if b.OpenClaw.Install != nil {
		runningVersion = b.OpenClaw.Install.RunningDirVersion
	}
	if err := recordWorkloadObservationEvidenceTx(tx, machineID, verdict, receivedAt,
		evidenceAt, b.OpenClaw.Present, runningVersion, b.WorkloadPolicyToken, policyValid); err != nil {
		return err
	}
	if err := recordWorkloadObservationWitnessTx(tx, machineID, verdict, receivedAt,
		evidenceAt, b.OpenClaw.Present, runningVersion, b.WorkloadPolicyToken, policyValid); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit observation: %w", err)
	}
	return nil
}

// explicitInstallFailure 只把同批明確量到的壞狀態當 failure。缺 Install 或
// ProcessMatchesUnit=nil 是舊 Agent／讀不到證據，必須保持 unknown。
func explicitInstallFailure(openClaw model.OpenClaw) string {
	if !openClaw.Present {
		return "OpenClaw 安裝目錄明確不存在" + reasonSuffix(openClaw.Reason)
	}
	if openClaw.Install == nil {
		return ""
	}
	install := openClaw.Install
	switch {
	case !install.UnitFound:
		return "OpenClaw gateway 的 systemd unit 不存在" + reasonSuffix(install.UnitReason)
	case install.MainPID <= 0:
		return "OpenClaw gateway 沒有正在執行的 MainPID" + reasonSuffix(install.ProcessReason)
	case install.ProcessMatchesUnit != nil && !*install.ProcessMatchesUnit:
		return "OpenClaw gateway process 與 systemd unit 指向的版本不一致" + reasonSuffix(install.ProcessReason)
	default:
		return ""
	}
}

// explicitDatabaseFailure 保留 DB observation 的三態。nil 或 Present=true 且
// Reason 非空都是「沒量到／讀錯」；只有明確不存在，或成功讀到 rows=0，
// 才是壞事實。rows=0 卻同時帶 LastTaskEndedAt 是互相矛盾的 payload，不能讓
// 那個時間把「從未跑過」洗成 healthy；負數 rows 則是無效量測，保持 unknown。
func explicitDatabaseFailure(db *model.OpenClawDB) string {
	switch {
	case db == nil:
		return ""
	case !db.Present:
		return "OpenClaw 狀態資料庫明確不存在" + reasonSuffix(db.Reason)
	case strings.TrimSpace(db.Reason) != "":
		return ""
	case db.TaskRunRows == 0:
		return "OpenClaw 狀態資料庫存在，但沒有任何任務執行紀錄 —— 它從來沒跑過"
	default:
		return ""
	}
}

func reasonSuffix(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return "：" + reason
}

// workloadFactsFromObservation 只用這一個 incoming batch，絕不查 latest。
// 這是為了保住「bad batch 後面立刻接 good batch」中間那一格歷史。
// bool 只在這一批提供了所有明確的正面證據時為 true；缺列是 unknown。
func (s *Store) workloadFactsFromObservation(displayName string, b model.ObservationBatch, receivedAt time.Time) (state.Facts, bool) {
	artifacts, artifactsComplete := s.artifactFactsFromObservation(displayName, b, receivedAt)
	f := state.Facts{
		Now: receivedAt.UTC(),
		// 這條路上「這一批」一定帶著 OpenClaw payload，所以對這一批而言
		// observed 恆為真；OpenClawObserved 的 unknown case 是給讀庫路徑用的。
		OpenClawObserved: true,
		OpenClawPresent:  b.OpenClaw.Present,
		Artifacts:        artifacts,
	}
	taskMeasured := false
	if db := b.OpenClaw.DB; db != nil && db.Present &&
		strings.TrimSpace(db.Reason) == "" && db.TaskRunRows > 0 &&
		db.LastTaskEndedAt != nil && !db.LastTaskEndedAt.IsZero() {
		lastTask := db.LastTaskEndedAt.UTC()
		// 小幅領先由正常 clock skew 解釋；超過容忍值的「未來任務」不能
		// 當 recovery，也不能倒過來捏造一個健康的負 idle duration。
		if receivedAt.UTC().Sub(lastTask) >= -state.ClockSkewTolerance {
			taskMeasured = true
			f.HasTaskSignal = true
			f.LastTaskEnded = lastTask
		}
	}
	// 缺 DB/task signal 是 unknown，不是「從來沒跑完」。用 neutral 值只為了
	// 讓 pure evaluator 不把 partial batch 投影成 failure；下面的
	// recoveryComplete 仍要求這批真的帶著 signal 才能關 span。
	if !f.HasTaskSignal {
		f.HasTaskSignal = true
		f.LastTaskEnded = receivedAt.UTC()
	}
	// 同一批若有重複 name，任一明確 PID=0 都必須留下 failure；即使全部
	// 都是 running，重複列仍不能成為 coherent recovery witness。
	cliMeasured := false
	cliCount := 0
	cliExplicitFailure := false
	cliFailureReason := ""
	cliFailureProcessScan := ""
	for _, tool := range b.CLITools {
		if tool.Name != "openclaw" {
			continue
		}
		cliCount++
		cliMeasured = true
		f.OpenClawRunning = tool.RunningPID > 0
		f.OpenClawRunReason = tool.RunningReason
		f.OpenClawProcessScan = tool.ProcessScan
		if tool.RunningPID <= 0 {
			cliExplicitFailure = true
			cliFailureProcessScan = tool.ProcessScan
			if tool.RunningReason != "" {
				cliFailureReason = tool.RunningReason
			}
		}
	}
	if cliExplicitFailure {
		f.OpenClawRunning = false
		f.OpenClawRunReason = cliFailureReason
		f.OpenClawProcessScan = cliFailureProcessScan
	}
	if !cliMeasured {
		// 同上：沒送這列是 unknown；不能憑零值捏造「process 不存在」。
		f.OpenClawRunning = true
	}
	taskAge := receivedAt.UTC().Sub(f.LastTaskEnded)
	installComplete := b.OpenClaw.Install != nil && b.OpenClaw.Install.UnitFound &&
		b.OpenClaw.Install.MainPID > 0 && b.OpenClaw.Install.ProcessMatchesUnit != nil &&
		*b.OpenClaw.Install.ProcessMatchesUnit && b.OpenClaw.Install.RunningDirVersion != ""
	recoveryComplete := b.OpenClaw.Present && cliMeasured && cliCount == 1 && f.OpenClawRunning &&
		taskMeasured && taskAge >= -state.ClockSkewTolerance && taskAge <= state.SilentFailure &&
		installComplete && artifactsComplete
	return f, recoveryComplete
}

func (s *Store) artifactFactsFromObservation(displayName string, b model.ObservationBatch, now time.Time) ([]state.ArtifactFact, bool) {
	measured := make(map[string]model.ArtifactCheck, len(b.Artifacts))
	duplicateMeasurement := false
	for _, artifact := range b.Artifacts {
		key := artifact.Unit + "\x1f" + artifact.Artifact
		if _, duplicate := measured[key]; duplicate {
			duplicateMeasurement = true
		}
		measured[key] = artifact
	}
	events := make(map[string]model.EventStream, len(b.Events))
	for _, event := range b.Events {
		key := event.Unit + "\x1f" + event.Path
		if _, duplicate := events[key]; duplicate {
			duplicateMeasurement = true
		}
		events[key] = event
	}
	rules := s.expects.For(displayName)
	complete := (s.expects == nil || s.expects.Err == "") && !duplicateMeasurement
	effectiveKeys := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		key := rule.Unit + "\x1f" + rule.Artifact
		if _, duplicate := effectiveKeys[key]; duplicate {
			// Agent wire format 的 subject 就是這個 key。'*' 與 machine-specific
			// 同時宣告它時，一列 EventStream 沒有足夠身分可以綁回哪條規則。
			complete = false
		}
		effectiveKeys[key] = struct{}{}
		if _, ok := measured[key]; !ok {
			complete = false
		}
		if rule.Events != nil {
			event, ok := events[key]
			if !ok {
				complete = false
				continue
			}
			if event.Err != "" || event.Truncated {
				complete = false
			}
			required := make(map[string]bool, len(rule.Events.NotOK))
			for _, name := range rule.Events.NotOK {
				required[name] = false
			}
			seenTypes := make(map[string]struct{}, len(event.Declared)+len(event.Undeclared))
			for _, declared := range event.Declared {
				_, duplicate := seenTypes[declared.Type]
				if declared.Type == "" || duplicate || declared.Count < 0 ||
					(declared.Count == 0) != (declared.LastAt == nil) ||
					(declared.LastAt != nil && declared.LastAt.After(now.UTC().Add(state.ClockSkewTolerance))) {
					complete = false
				}
				seenTypes[declared.Type] = struct{}{}
				if _, needed := required[declared.Type]; needed {
					required[declared.Type] = true
				}
			}
			for _, seen := range required {
				if !seen {
					complete = false
				}
			}
			for _, undeclared := range event.Undeclared {
				_, duplicate := seenTypes[undeclared.Type]
				if undeclared.Type == "" || duplicate || undeclared.Count < 0 ||
					(undeclared.Count == 0) != (undeclared.LastAt == nil) ||
					(undeclared.LastAt != nil && undeclared.LastAt.After(now.UTC().Add(state.ClockSkewTolerance))) {
					complete = false
				}
				seenTypes[undeclared.Type] = struct{}{}
			}
			if event.Malformed != 0 || event.UndeclaredTotal < 0 || event.UndeclaredTotal < len(event.Undeclared) {
				complete = false
			}
			// recovery witness 不接受截斷量測。即使 CoveredFrom 剛好跨過
			// window，tail 上限仍表示這批不是完整的正面證據。
			// event.Truncated 已在上面一律標 incomplete；CoveredFrom 不能證明
			// 被 line/byte cap 丟掉的 prefix 沒有窗口內事件。
		}
	}
	facts := artifactFactsFromMeasurements(rules, measured, events, now)
	for _, fact := range facts {
		if !fact.Checked {
			complete = false
		}
		if fact.ModTime != nil && fact.ModTime.After(now.UTC().Add(state.ClockSkewTolerance)) {
			complete = false
		}
		// Partial/Malformed 在畫面上是 advisory，但不是可以關掉
		// failure span 的正面證據。Undeclared 則只是沒有人先宣告它是壞事。
		if fact.Events != nil && (!fact.Events.Measured || fact.Events.Partial || fact.Events.Malformed > 0) {
			complete = false
		}
	}
	return facts, complete
}

const (
	workloadWitnessHealthy = "healthy"
	workloadWitnessFailure = "failure"
	workloadWitnessUnknown = "unknown"
)

func normalizedObservationEvidenceAt(measuredAt, receivedAt time.Time, clockSkew time.Duration, trustedCheckin bool) (time.Time, bool) {
	if !trustedCheckin || measuredAt.IsZero() {
		return time.Time{}, false
	}
	receivedAt = receivedAt.UTC().Truncate(time.Second)
	evidenceAt := measuredAt.UTC().Add(-clockSkew).Truncate(time.Second)
	if evidenceAt.After(receivedAt) {
		if evidenceAt.After(receivedAt.Add(state.ClockSkewTolerance)) {
			return time.Time{}, false
		}
		// 網路/取樣邊界加上 skew 秒級量化可能留下小幅 future；Hub 不把
		// 未來當 freshness credit，最多 clamp 到真正 ingest 的時刻。
		evidenceAt = receivedAt
	}
	if receivedAt.Sub(evidenceAt) > state.ObservationStale {
		return time.Time{}, false
	}
	return evidenceAt, true
}

func workloadEvidenceWatermarkTx(tx dbTx, machineID string) (time.Time, error) {
	var raw string
	err := tx.QueryRow(`SELECT evidence_at FROM workload_observation_witness WHERE machine_id=?`, machineID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: prior workload evidence watermark: %w", err)
	}
	return parseTime(raw), nil
}

func evidenceAfterOpenFailuresTx(tx dbTx, machineID string, evidenceAt time.Time) (bool, error) {
	var latest sql.NullString
	if err := tx.QueryRow(`SELECT MAX(last_seen_at) FROM canary_silent_failures
	 WHERE machine_id=? AND open=1`, machineID).Scan(&latest); err != nil {
		return false, err
	}
	if !latest.Valid || latest.String == "" {
		return true, nil
	}
	return evidenceAt.After(parseTime(latest.String)), nil
}

func recordWorkloadObservationWitnessTx(tx dbTx, machineID, verdict string, receivedAt time.Time,
	evidenceAt time.Time, openClawPresent bool, runningVersion, policyToken string, policyValid bool,
) error {
	if machineID == "" || (verdict != workloadWitnessHealthy && verdict != workloadWitnessFailure && verdict != workloadWitnessUnknown) {
		return errors.New("store: invalid workload observation witness")
	}
	evidenceStamp := ""
	if !evidenceAt.IsZero() {
		evidenceStamp = fmtTime(evidenceAt)
	}
	if _, err := tx.Exec(`INSERT INTO workload_observation_witness
	 (machine_id,verdict,received_at,evidence_at,openclaw_present,running_version,workload_policy_token,policy_valid)
	 VALUES(?,?,?,?,?,?,?,?)
	 ON CONFLICT(machine_id) DO UPDATE SET
	 verdict=excluded.verdict,received_at=excluded.received_at,
	 evidence_at=CASE
	   WHEN excluded.evidence_at='' THEN workload_observation_witness.evidence_at
	   WHEN workload_observation_witness.evidence_at='' THEN excluded.evidence_at
	   WHEN excluded.evidence_at>workload_observation_witness.evidence_at THEN excluded.evidence_at
	   ELSE workload_observation_witness.evidence_at END,
	 openclaw_present=excluded.openclaw_present,
	 running_version=excluded.running_version,
		workload_policy_token=excluded.workload_policy_token,
	 policy_valid=excluded.policy_valid
	 WHERE excluded.received_at >= workload_observation_witness.received_at`,
		machineID, verdict, fmtTime(receivedAt), evidenceStamp, openClawPresent, runningVersion, policyToken, policyValid); err != nil {
		return fmt.Errorf("store: record workload observation witness: %w", err)
	}
	return nil
}

// recordWorkloadObservationEvidenceTx 留下每一批 workload verdict。latest witness
// 只能回答「現在」，這張 ledger 才能證明 canary 完成後沒有 Hub outage、unknown
// batch、policy 切換或短暫版本漂移。它與 raw observation / failure span 同一筆 commit。
func recordWorkloadObservationEvidenceTx(tx dbTx, machineID, verdict string, receivedAt time.Time,
	evidenceAt time.Time, openClawPresent bool, runningVersion, policyToken string, policyValid bool,
) error {
	if machineID == "" || (verdict != workloadWitnessHealthy && verdict != workloadWitnessFailure && verdict != workloadWitnessUnknown) {
		return errors.New("store: invalid workload observation evidence")
	}
	evidenceStamp := ""
	if !evidenceAt.IsZero() {
		evidenceStamp = fmtTime(evidenceAt)
	}
	if _, err := tx.Exec(`INSERT INTO workload_observation_evidence
	 (machine_id,received_at,evidence_at,verdict,openclaw_present,running_version,workload_policy_token,policy_valid)
	 VALUES(?,?,?,?,?,?,?,?)`, machineID, fmtTime(receivedAt), evidenceStamp, verdict,
		openClawPresent, runningVersion, policyToken, policyValid); err != nil {
		return fmt.Errorf("store: record workload observation evidence: %w", err)
	}
	return nil
}

// latestTrustedCheckinTx 取 ingest 當下已存在、仍新鮮且時鐘可信的最新心跳。
// Observation 的 measured_at 只有配上這個 skew 才能正規化成 Hub 時間。
func latestTrustedCheckinTx(tx dbTx, machineID string, now time.Time) (time.Duration, bool, error) {
	now = now.UTC().Truncate(time.Second)
	var receivedAt sql.NullString
	var clockSkew sql.NullInt64
	err := tx.QueryRow(`SELECT received_at,clock_skew_seconds FROM machine_checkins
	 WHERE machine_id=? AND received_at<=?
	 ORDER BY received_at DESC,rowid DESC LIMIT 1`, machineID, fmtTime(now)).Scan(&receivedAt, &clockSkew)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: trusted observation checkin: %w", err)
	}
	lastCheckin := parseTimeNull(receivedAt)
	if lastCheckin.IsZero() || now.After(lastCheckin.Add(state.CheckinInterval).Add(state.UnreachableGrace)) {
		return 0, false, nil
	}
	skewLimit := int64(state.ClockSkewTolerance / time.Second)
	if !clockSkew.Valid || clockSkew.Int64 > skewLimit || clockSkew.Int64 < -skewLimit {
		return 0, false, nil
	}
	return time.Duration(clockSkew.Int64) * time.Second, true, nil
}

// observationRecoveryEligibleTx 擋住 workload 以外不能被 healthy batch
// 洗掉的三種早退狀態：已退役、身分衝突、沒有新鮮心跳。必須在
// RecordObservation 的 transaction 裡查，否則看不到剛寫入的 identity。
func observationRecoveryEligibleTx(tx dbTx, machineID string, now time.Time) (bool, error) {
	now = now.UTC().Truncate(time.Second)
	var registryHint, retiredAt sql.NullString
	err := tx.QueryRow(`SELECT machine_id_hint,retired_at FROM machine_registry WHERE machine_id=?`, machineID).
		Scan(&registryHint, &retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: recovery registry: %w", err)
	}
	if retiredAt.Valid && retiredAt.String != "" {
		return false, nil
	}

	conflict, err := identityConflictTx(tx, machineID, registryHint.String)
	if err != nil {
		return false, err
	}
	if conflict {
		return false, nil
	}

	_, trusted, err := latestTrustedCheckinTx(tx, machineID, now)
	return trusted, err
}

func identityConflictTx(tx dbTx, machineID, registryHint string) (bool, error) {
	return identityConflictFrom(tx, machineID, registryHint)
}

// ---------------------------------------------------------------- 讀觀測

// observation 是 observed_state 的一列（未解析的 payload）。
type observation struct {
	Subject    string
	Payload    string
	MeasuredAt time.Time
	ReceivedAt time.Time
}

// latestObservation 取某個 (kind, subject) 最新的一列。
// 以 Hub 收到的時刻排序，不用 agent 的鐘，否則未來鐘會永遠贏過後續觀測。
// 同一個 received_at 有多列時先比 measured_at、再用 rowid 決勝 —— 後插入的贏，那是唯一可靠的順序。
func (s *Store) latestObservation(machineID, kind, subject string) (observation, bool, error) {
	var o observation
	var measured, recv string
	err := s.rdb.QueryRow(`
SELECT subject, payload, measured_at, received_at FROM observed_state
 WHERE machine_id = ? AND kind = ? AND subject = ?
 ORDER BY received_at DESC, measured_at DESC, rowid DESC LIMIT 1`,
		machineID, kind, subject).Scan(&o.Subject, &o.Payload, &measured, &recv)
	if errors.Is(err, sql.ErrNoRows) {
		return observation{}, false, nil
	}
	if err != nil {
		return observation{}, false, fmt.Errorf("store: latest %s/%s: %w", kind, subject, err)
	}
	o.MeasuredAt, o.ReceivedAt = parseTime(measured), parseTime(recv)
	return o, true, nil
}

// latestObservationReceivedBy reads the latest observation that was already
// present in the Hub ledger at one evaluation instant. The trusted received_at
// cutoff belongs in SQL: filtering a newer row after LIMIT 1 would hide an
// older row that was valid at that instant.
func (s *Store) latestObservationReceivedBy(machineID, kind, subject string, evaluatedAt time.Time) (observation, bool, error) {
	var o observation
	var measured, recv string
	err := s.rdb.QueryRow(`
SELECT subject, payload, measured_at, received_at FROM observed_state
 WHERE machine_id = ? AND kind = ? AND subject = ? AND received_at <= ?
 ORDER BY received_at DESC, measured_at DESC, rowid DESC LIMIT 1`,
		machineID, kind, subject, fmtTime(evaluatedAt)).Scan(&o.Subject, &o.Payload, &measured, &recv)
	if errors.Is(err, sql.ErrNoRows) {
		return observation{}, false, nil
	}
	if err != nil {
		return observation{}, false, fmt.Errorf("store: latest %s/%s received by evaluation instant: %w", kind, subject, err)
	}
	o.MeasuredAt, o.ReceivedAt = parseTime(measured), parseTime(recv)
	return o, true, nil
}

// LatestOpenClawDBs 拿指定這幾台各自最新的一筆 OpenClaw 觀測裡的資料庫那一段。
//
// ⚠⚠ 存在的理由是效能，而且兩次都是實測逼出來的：
//
//  1. `/metrics` 原本為了排程指標，對每一台各叫一次 `Detail()`。`Detail()` 會做
//     十幾個查詢、還會 `credRefreshHistory()` 掃一次整張 observed_state ——
//     在 46 MB 的真實資料庫上把 `/metrics` 從 57 ms 拖到 300 ms。
//  2. 第一版的修法是「一個查詢拿全機隊」，只濾 `kind`。**更慢，900 ms。**
//     因為 `ix_observed_prune` 是 `(machine_id, kind, subject, received_at DESC)`，
//     **machine_id 打頭**；不給它就整表掃描。
//
// 所以這裡吃索引：一台一個 O(1) 的查詢。⚠ 要改這支之前先跑 EXPLAIN QUERY PLAN，
// 「一個查詢一定比五個快」在有複合索引的時候是錯的。
//
// ⚠ 沒有觀測、或觀測裡沒有 db 那一段的機器**不在 map 裡**（不是給一個零值）。
// 「不知道」跟「0 個工作」在這一層就要分開，到了 /metrics 才分就太晚了。
func (s *Store) LatestOpenClawDBs(machineIDs []string) (map[string]*model.OpenClawDB, error) {
	out := make(map[string]*model.OpenClawDB, len(machineIDs))
	for _, id := range machineIDs {
		obs, ok, err := s.latestObservation(id, KindOpenClaw, KindOpenClaw)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		oc, ok := unmarshalInto[model.OpenClaw](obs.Payload)
		if !ok || oc.DB == nil || !oc.DB.Present {
			continue
		}
		out[id] = oc.DB
	}
	return out, nil
}

// OpenClawInstallObservation keeps the install payload tied to the Hub-trusted
// receive time of the observation that supplied it. Consumers must not call a
// historical payload "current" without applying their own freshness policy.
type OpenClawInstallObservation struct {
	Install    *model.OpenClawInstall
	MeasuredAt time.Time
	ReceivedAt time.Time
}

// latestOpenClawInstallObservations reads the selected OpenClaw observation
// and preserves its time coordinate even when an older agent omitted install.
// 走法與 LatestOpenClawDBs 相同：一台一個吃 ix_observed_prune 的查詢，不叫 Detail()。
func (s *Store) latestOpenClawInstallObservations(machineIDs []string,
	latest func(string, string, string) (observation, bool, error),
) (map[string]OpenClawInstallObservation, error) {
	out := make(map[string]OpenClawInstallObservation, len(machineIDs))
	for _, id := range machineIDs {
		obs, ok, err := latest(id, KindOpenClaw, KindOpenClaw)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		oc, ok := unmarshalInto[model.OpenClaw](obs.Payload)
		if !ok {
			continue
		}
		out[id] = OpenClawInstallObservation{
			Install: oc.Install, MeasuredAt: obs.MeasuredAt, ReceivedAt: obs.ReceivedAt,
		}
	}
	return out, nil
}

// LatestOpenClawInstallObservations preserves the evidence timestamp without
// imposing a historical cutoff.
func (s *Store) LatestOpenClawInstallObservations(machineIDs []string) (map[string]OpenClawInstallObservation, error) {
	return s.latestOpenClawInstallObservations(machineIDs, s.latestObservation)
}

// LatestOpenClawInstallObservationsAt is the snapshot-aware adapter used by
// update composition and rollout planning. It excludes rows received after the
// supplied evaluation instant before choosing the latest observation.
func (s *Store) LatestOpenClawInstallObservationsAt(machineIDs []string, evaluatedAt time.Time) (map[string]OpenClawInstallObservation, error) {
	if evaluatedAt.IsZero() {
		return nil, errors.New("store: OpenClaw install evaluation instant is required")
	}
	return s.latestOpenClawInstallObservations(machineIDs,
		func(machineID, kind, subject string) (observation, bool, error) {
			return s.latestObservationReceivedBy(machineID, kind, subject, evaluatedAt)
		})
}

// LatestOpenClawInstalls is the payload-only adapter retained for metrics and
// the Apps observation view. An old-agent observation without install remains
// absent from this compatibility map rather than becoming a misleading zero
// install; Updates and rollout use the evidence-preserving as-of variant.
func (s *Store) LatestOpenClawInstalls(machineIDs []string) (map[string]*model.OpenClawInstall, error) {
	evidence, err := s.LatestOpenClawInstallObservations(machineIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*model.OpenClawInstall, len(evidence))
	for id, observation := range evidence {
		if observation.Install != nil {
			out[id] = observation.Install
		}
	}
	return out, nil
}

// latestBySubject 取某個 kind 底下每個 subject 各自最新的一列。
// 先用 Hub received_at cutoff 排除尚未進帳的證據，再以單次 window ranking
// 選 measured_at 最新者；同 measured_at 依 received_at、rowid 決勝。
// credRefresh 是一家登入在一台機器上的續期史：我們從什麼時候開始看、
// 它換了幾次檔案，以及最近一次檔案時間。票的名冊還沒建，不能把 provider
// 相同解讀成同一張票。
type credRefresh struct {
	DisplayName string
	Refreshes   int
	FirstSeen   time.Time
	FileMTime   *time.Time
	Retired     bool
}

type credRefreshFleet map[string]map[string]credRefresh

// credRefreshHistory 一次數出全機隊每台每個 provider 的憑證檔，在
// observed_state 裡出現過幾個不同的 file_mtime。換了 N 個值 = 自己續了
// N-1 次。一次拿全機隊，是為了讓 Facts 組同儕證據時不會每台、每家各查一次。
//
// ⚠ 這是 observed_state append-only 才答得出來的問題（SPEC §4.2）。
// credential_on_machine 那張投影表只有最後一個值，從它永遠看不出「停了」。
//
// ⚠ 沒有 file_mtime 的觀測（absent / 讀不到）不算 —— COUNT(DISTINCT) 本來就
// 略過 NULL，所以一張從頭到尾讀不到檔案的票會是 0 次、而不是 1 次。
// 保存期 30 天（retention.go），所以 FirstSeen 最遠只到 30 天前，
// 這也是 WatchedFor 的天花板。
func (s *Store) credRefreshHistory(evaluatedAt time.Time) (credRefreshFleet, error) {
	rows, err := s.rdb.Query(`
SELECT o.machine_id,
       COALESCE(NULLIF(m.display_name, ''), o.machine_id),
       o.subject,
       COUNT(DISTINCT json_extract(o.payload, '$.file_mtime')),
       MIN(o.measured_at),
       MAX(json_extract(o.payload, '$.file_mtime')),
       CASE WHEN m.retired_at IS NULL THEN 0 ELSE 1 END
 FROM observed_state o
  JOIN machine_registry m ON m.machine_id = o.machine_id
 WHERE o.kind = ? AND o.received_at <= ?
GROUP BY o.machine_id, m.display_name, o.subject, m.retired_at`, KindCredential, fmtTime(evaluatedAt))
	if err != nil {
		return nil, fmt.Errorf("store: credential refresh history: %w", err)
	}
	defer rows.Close()
	out := credRefreshFleet{}
	for rows.Next() {
		var machineID, displayName, subject, first string
		var distinct, retired int
		var fileMTime sql.NullString
		if err := rows.Scan(&machineID, &displayName, &subject, &distinct, &first, &fileMTime, &retired); err != nil {
			return nil, fmt.Errorf("store: scan credential refresh history: %w", err)
		}
		h := credRefresh{
			DisplayName: displayName,
			FirstSeen:   parseTime(first),
			FileMTime:   parseTimePtr(fileMTime),
			Retired:     retired != 0,
		}
		if distinct > 1 {
			h.Refreshes = distinct - 1
		}
		if out[machineID] == nil {
			out[machineID] = map[string]credRefresh{}
		}
		out[machineID][subject] = h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate credential refresh history: %w", err)
	}
	return out, nil
}

// credPeers 只搬結構化的續期史：同 provider、不是自己、未退役，而且真的
// 看過 file_mtime 改變。排序讓畫面與判決每次都先看到最活躍的一台。
func credPeers(refresh credRefreshFleet, machineID, provider string) []state.CredPeer {
	var peers []state.CredPeer
	for peerID, providers := range refresh {
		h, ok := providers[provider]
		if !ok || peerID == machineID || h.Retired || h.Refreshes <= 0 {
			continue
		}
		peers = append(peers, state.CredPeer{
			DisplayName: h.DisplayName, RefreshesSeen: h.Refreshes, FileMTime: h.FileMTime,
		})
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].RefreshesSeen != peers[j].RefreshesSeen {
			return peers[i].RefreshesSeen > peers[j].RefreshesSeen
		}
		switch {
		case peers[i].FileMTime != nil && peers[j].FileMTime == nil:
			return true
		case peers[i].FileMTime == nil && peers[j].FileMTime != nil:
			return false
		case peers[i].FileMTime != nil && peers[j].FileMTime != nil && !peers[i].FileMTime.Equal(*peers[j].FileMTime):
			return peers[i].FileMTime.After(*peers[j].FileMTime)
		default:
			return peers[i].DisplayName < peers[j].DisplayName
		}
	})
	return peers
}

// credLifetime：這種票的名目壽命 = 到期時間 − 檔案寫入時間。
// 算不出來（缺任一邊、或倒過來）回 0，讓判決層知道「不知道」而不是「0 秒」。
func credLifetime(c model.Credential) time.Duration {
	if c.ExpiresAt == nil || c.FileMTime == nil {
		return 0
	}
	if d := c.ExpiresAt.Sub(*c.FileMTime); d > 0 {
		return d
	}
	return 0
}

const latestBySubjectAtSQL = `
WITH ranked AS (
 SELECT subject, payload, measured_at, received_at,
        ROW_NUMBER() OVER (
          PARTITION BY subject
          ORDER BY received_at DESC, measured_at DESC, rowid DESC
        ) AS position
   FROM observed_state
  WHERE machine_id = ? AND kind = ? AND received_at <= ?
)
SELECT subject, payload, measured_at, received_at
  FROM ranked
 WHERE position = 1
 ORDER BY subject ASC`

func (s *Store) latestBySubject(machineID, kind string, evaluatedAt time.Time) ([]observation, error) {
	// Rank only the rows admitted by the Hub-received cutoff. A correlated
	// MAX(measured_at) looks compact, but once that cutoff is added SQLite picks
	// ix_observed_prune and rescans each subject history for every outer row. On
	// the production-sized ledger that made one credential read take ~2.8s and
	// one Overview ~11s. This window shape visits the machine/kind history once
	// while preserving the received_at -> measured_at -> rowid tie-break.
	rows, err := s.rdb.Query(latestBySubjectAtSQL, machineID, kind, fmtTime(evaluatedAt))
	if err != nil {
		return nil, fmt.Errorf("store: latest by subject %s: %w", kind, err)
	}
	defer rows.Close()

	var out []observation
	seen := map[string]bool{}
	for rows.Next() {
		var o observation
		var measured, recv string
		if err := rows.Scan(&o.Subject, &o.Payload, &measured, &recv); err != nil {
			return nil, fmt.Errorf("store: scan %s: %w", kind, err)
		}
		if seen[o.Subject] {
			continue
		}
		seen[o.Subject] = true
		o.MeasuredAt, o.ReceivedAt = parseTime(measured), parseTime(recv)
		out = append(out, o)
	}
	return out, rows.Err()
}

// unmarshalInto 解 payload。⚠ 解不開就當沒有這一列，不 panic ——
// 一列壞掉的 JSON 不可以讓整個 Dashboard 打不開。
func unmarshalInto[T any](payload string) (T, bool) {
	var v T
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		return v, false
	}
	return v, true
}

// identityHints 回傳這台機器曾看過的每一個 machine-id，加上名冊登記的那一個。
//
// ⚠ 這個函式永遠不會回傳「哪個才是對的」。它只負責把所有看過的身分攤出來，
// 超過一個就是衝突。合併、挑一個、或刪掉舊的，都會讓一台機器的狀態在兩台
// 真實機器之間跳動，而且沒有人查得出為什麼。
func (s *Store) identityHints(machineID, registryHint string) ([]IdentityHint, error) {
	return identityHintsFrom(s.rdb, machineID, registryHint)
}

func (s *Store) identityHintsAt(machineID, registryHint string, evaluatedAt time.Time) ([]IdentityHint, error) {
	return identityHintsFromAt(s.rdb, machineID, registryHint, evaluatedAt)
}

// ---------------------------------------------------------------- Facts

// Facts 把存下來的東西組成 internal/state 的輸入。
//
// ⚠ 這個函式不下任何判決，只搬事實。它回傳的每一個欄位不是 Agent 說的，
// 就是 Hub 自己記的時間 —— 沒有一個是別人已經判好的結論。
func (s *Store) Facts(machineID string, now time.Time) (state.Facts, error) {
	refresh, err := s.credRefreshHistory(now.UTC())
	if err != nil {
		return state.Facts{}, err
	}
	return s.facts(machineID, now, refresh)
}

func (s *Store) facts(machineID string, now time.Time, refresh credRefreshFleet) (state.Facts, error) {
	now = now.UTC()
	m, err := s.GetMachine(machineID)
	if err != nil {
		return state.Facts{}, err
	}
	f := state.Facts{
		Now:       now,
		Artifacts: s.artifactFacts(machineID, m.DisplayName, now),
		Retired:   m.RetiredAt != nil,
		// CheckinInterval 目前是全機隊同一個值。schema 沒有 per-machine 的欄位，
		// 等到真的要分機器調節奏時再加。
		CheckinInterval: state.CheckinInterval,
		LingerEnabled:   m.LingerEnabled,
	}

	// --- 心跳。⚠ 存活一律看 received_at。
	var sentAt, recvAt, agentVer sql.NullString
	var skew sql.NullInt64
	var diskFree, diskTotal sql.NullInt64
	err = s.rdb.QueryRow(`
SELECT sent_at, received_at, clock_skew_seconds, disk_free_bytes, disk_total_bytes, agent_version
  FROM machine_checkins WHERE machine_id = ? AND received_at <= ?
 ORDER BY received_at DESC, rowid DESC LIMIT 1`, machineID, fmtTime(now)).
		Scan(&sentAt, &recvAt, &skew, &diskFree, &diskTotal, &agentVer)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 從來沒報到過。⚠ 這不是錯誤，這是最重要的一種事實。
	case err != nil:
		return state.Facts{}, fmt.Errorf("store: last checkin: %w", err)
	default:
		received := parseTimeNull(recvAt)
		if !received.IsZero() {
			f.EverCheckedIn = true
			f.LastCheckinReceived = received
		}
		// ClockSkew = sent_at - received_at。正值 = 機器時鐘比 Hub 快。
		// 存的時候就算好了，這裡優先用存的；存的是 NULL 才現算。
		if skew.Valid {
			f.ClockSkew = time.Duration(skew.Int64) * time.Second
		} else if sent := parseTimeNull(sentAt); !sent.IsZero() && !received.IsZero() {
			f.ClockSkew = sent.Sub(received)
		}
		f.DiskFreeBytes, f.DiskTotalBytes = diskFree.Int64, diskTotal.Int64
		// ⚠ 只搬，不補預設值。NULL（舊版 agent 沒送）落地就是空字串，
		// 而空字串在指標那層會讓整行不出現 —— 「不知道」要長得像不知道。
		f.AgentVersion = agentVer.String
	}

	// --- 機器重開機：一小時內看到幾個不同的 boot_id。
	// ⚠ 用 received_at 圈窗，因為時鐘歪掉的機器 sent_at 可能全部落在窗外。
	var bootIDRows int
	if err := s.rdb.QueryRow(`
SELECT COUNT(DISTINCT boot_id), COUNT(boot_id) FROM machine_checkins
 WHERE machine_id = ? AND received_at >= ? AND received_at <= ? AND boot_id IS NOT NULL AND boot_id <> ''`,
		machineID, fmtTime(now.Add(-state.CrashLoopWindow)), fmtTime(now)).Scan(&f.BootIDChanges1h, &bootIDRows); err != nil {
		return state.Facts{}, fmt.Errorf("store: boot id count: %w", err)
	}
	f.BootIDSignalObserved = bootIDRows > 0

	// --- crash-loop：一小時內看到幾個不同的 agent_started_at（= process 重啟幾次）。
	//
	// ⚠ 這一條跟上面那條長得幾乎一樣，但它們數的是完全不同的東西，
	// 而且只有這一條抓得到 agent 的崩潰迴圈。詳見 state.Facts 的註解。
	// 舊版 agent 不送這個欄位，數出來會是 0 —— 那是「不知道」，不是「沒重啟」。
	// 這裡刻意不把 0 說成健康：真正的判決在 state，這裡只負責數。
	var agentStartedRows int
	if err := s.rdb.QueryRow(`
SELECT COUNT(DISTINCT agent_started_at), COUNT(agent_started_at) FROM machine_checkins
 WHERE machine_id = ? AND received_at >= ? AND received_at <= ?
   AND agent_started_at IS NOT NULL AND agent_started_at <> ''`,
		machineID, fmtTime(now.Add(-state.CrashLoopWindow)), fmtTime(now)).
		Scan(&f.AgentRestarts1h, &agentStartedRows); err != nil {
		return state.Facts{}, fmt.Errorf("store: agent restart count: %w", err)
	}
	f.AgentRestartSignalObserved = agentStartedRows > 0

	// --- 完整觀測最後一次收到的時間。
	var lastObs sql.NullString
	if err := s.rdb.QueryRow(
		`SELECT MAX(received_at) FROM observed_state WHERE machine_id = ? AND received_at <= ?`, machineID, fmtTime(now)).
		Scan(&lastObs); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state.Facts{}, fmt.Errorf("store: last observation: %w", err)
	}
	if t := parseTimeNull(lastObs); !t.IsZero() {
		f.HasObservation, f.LastObservation = true, t
	}

	// --- identity：linger 以「觀測到的」優先，名冊只是還沒觀測到時的備胎。
	if obs, ok, err := s.latestObservationReceivedBy(machineID, KindIdentity, KindIdentity, now); err != nil {
		return state.Facts{}, err
	} else if ok {
		if id, ok := unmarshalInto[model.Identity](obs.Payload); ok {
			// 量不到的平台不准被畫成「未開啟」。
			if id.LingerMeasured {
				linger := id.LingerEnabled
				f.LingerEnabled = &linger
			}
		}
	}

	// --- 身分衝突。⚠ 看到一個以上的 machine-id 就是衝突，不做任何調解。
	hints, err := s.identityHintsAt(machineID, m.MachineIDHint, now)
	if err != nil {
		return state.Facts{}, err
	}
	f.Conflict = len(hints) > 1

	// --- OpenClaw：L1 存活訊號。
	if obs, ok, err := s.latestObservationReceivedBy(machineID, KindOpenClaw, KindOpenClaw, now); err != nil {
		return state.Facts{}, err
	} else if ok {
		f.OpenClawObserved = true
		if oc, ok := unmarshalInto[model.OpenClaw](obs.Payload); ok {
			f.OpenClawPresent = oc.Present
			// ⚠ HasTaskSignal 只來自 DB.LastTaskEndedAt。資料庫讀不到就是 false ——
			// 「不知道」不可以變成「沒問題」。而且就算有值，它回答的也只是
			// 「最後一次跑完是什麼時候」，不是「跑的東西對不對」。
			if oc.DB != nil && oc.DB.Present && oc.DB.LastTaskEndedAt != nil {
				f.HasTaskSignal = true
				f.LastTaskEnded = oc.DB.LastTaskEndedAt.UTC()
			} else if oc.DB != nil && oc.DB.Present && strings.TrimSpace(oc.DB.Reason) != "" && oc.DB.TaskRunRows == 0 {
				// readTaskRuns 先做 COUNT(*) + MAX(ended_at)，所以 TaskRunRows > 0
				// 代表任務表確實讀到了；後續 cron 探測留下的 Reason 不可把
				// 「沒有任何一筆跑完」改口成「不知道」。
				f.OpenClawDBReason = oc.DB.Reason
			}
		}
	}

	// --- systemd 對 agent 自己的重啟計數。
	//
	// ⚠ 這是「crash-loop」與「有人在部署」唯一分得開的地方。
	// AgentRestarts1h 只數 process 重啟了幾次，那兩件事在它眼中一模一樣。
	// systemd 的 NRestarts 只在**它自己**把死掉的 unit 拉起來時才加，
	// 手動 `systemctl restart` 會歸零 —— 那個差別就是「誰按的」。
	if obs, ok, err := s.latestObservationReceivedBy(machineID, KindSystemd, "clawctl-agent.service", now); err != nil {
		return state.Facts{}, err
	} else if ok {
		if u, ok := unmarshalInto[model.Unit](obs.Payload); ok && u.Present {
			f.AgentUnitSeen = true
			f.AgentNRestarts = u.NRestarts
		}
	}

	// --- OpenClaw 的 process 在不在：來自 cli_tool 觀測的 RunningPID。
	//
	// ⚠ 這個推論只有**一個方向**是有效的。
	// process 活著不代表它在做事（那是 L1 的工作），
	// 但一台裝了 OpenClaw、process 卻不在的機器，是真的需要有人去按一下。
	//
	// ⚠ 真正的判準是 ProcessScan，RunningReason 只是給人讀的下一步。
	// 兩者都要一起帶過去，否則判決或操作指引會少掉一半。
	if obs, ok, err := s.latestObservationReceivedBy(machineID, KindCLITool, "openclaw", now); err != nil {
		return state.Facts{}, err
	} else if ok {
		if t, ok := unmarshalInto[model.CLITool](obs.Payload); ok {
			f.OpenClawRunning = t.RunningPID > 0
			f.OpenClawRunReason = t.RunningReason
			f.OpenClawProcessScan = t.ProcessScan
		}
	}

	// --- 憑證。⚠ Status 原樣帶過去，五個詞，不壓成布林。
	creds, err := s.latestBySubject(machineID, KindCredential, now)
	if err != nil {
		return state.Facts{}, err
	}
	// 歷史那一半：這家登入在這台自己續了幾次，以及別台同一家有沒有在續。
	// Agent 只有快照，「續期停了」這件事只有 Hub 答得出來（SPEC §4.3）。
	for _, obs := range creds {
		c, ok := unmarshalInto[model.Credential](obs.Payload)
		if !ok {
			continue
		}
		cf := state.CredFact{
			Provider:  c.Provider,
			Status:    string(c.Status),
			ExpiresAt: c.ExpiresAt,
			Note:      c.Note,
			FileMTime: c.FileMTime,
			Lifetime:  credLifetime(c),
		}
		if h, ok := refresh[machineID][c.Provider]; ok {
			cf.RefreshesSeen = h.Refreshes
			cf.WatchedFor = now.Sub(h.FirstSeen)
		}
		cf.Peers = credPeers(refresh, machineID, c.Provider)
		f.Credentials = append(f.Credentials, cf)
	}
	sort.Slice(f.Credentials, func(i, j int) bool {
		return f.Credentials[i].Provider < f.Credentials[j].Provider
	})
	return f, nil
}

// ---------------------------------------------------------------- 狀態歷史

// openStateSpan 取目前還沒結束的那一段狀態（left_at IS NULL）。
func (s *Store) openStateSpan(machineID string) (StateSpan, bool, error) {
	var st, reason, entered string
	err := s.rdb.QueryRow(`
SELECT state, reason, entered_at FROM machine_state_history
 WHERE machine_id = ? AND left_at IS NULL
 ORDER BY entered_at DESC LIMIT 1`, machineID).Scan(&st, &reason, &entered)
	if errors.Is(err, sql.ErrNoRows) {
		return StateSpan{}, false, nil
	}
	if err != nil {
		return StateSpan{}, false, fmt.Errorf("store: open state span: %w", err)
	}
	return StateSpan{State: state.State(st), Reason: reason, EnteredAt: parseTime(entered)}, true, nil
}

// openStateSpanAt projects the state-history interval that was open at the
// supplied evaluation instant. A transition recorded just after an operator
// request began must not leak into a response labelled with the earlier time.
func (s *Store) openStateSpanAt(machineID string, evaluatedAt time.Time) (StateSpan, bool, error) {
	var st, reason, entered string
	err := s.rdb.QueryRow(`
SELECT state, reason, entered_at FROM machine_state_history
 WHERE machine_id = ? AND entered_at <= ? AND (left_at IS NULL OR left_at > ?)
 ORDER BY entered_at DESC LIMIT 1`, machineID, fmtTime(evaluatedAt), fmtTime(evaluatedAt)).
		Scan(&st, &reason, &entered)
	if errors.Is(err, sql.ErrNoRows) {
		return StateSpan{}, false, nil
	}
	if err != nil {
		return StateSpan{}, false, fmt.Errorf("store: open state span at evaluation instant: %w", err)
	}
	return StateSpan{State: state.State(st), Reason: reason, EnteredAt: parseTime(entered)}, true, nil
}

// RecordStateTransition 記一次狀態轉移。
//
// ⚠ 狀態沒變就什麼都不寫。每輪判決都插一列會讓這張表變成心跳的複本，
// 然後「這台什麼時候開始壞的」就再也答不出來了 —— 而那是最常問的問題。
//
// ⚠ 也不會回頭改已經開著那一段的 reason。entered_at 當下的理由是歷史；
// UI 上顯示的「現在為什麼」永遠是即時重算的（Overview/Detail 會呼叫 state.Derive）。
func (s *Store) RecordStateTransition(machineID string, st state.State, reason string, now time.Time) error {
	tx, err := s.beginWrite(context.Background(), "record_state_transition")
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()
	if err := recordStateTransitionTx(tx, machineID, st, reason, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit transition: %w", err)
	}
	return nil
}

func recordStateTransitionTx(tx dbTx, machineID string, st state.State, reason string, now time.Time) error {
	now = now.UTC()
	var openState, openEntered string
	err := tx.QueryRow(`
SELECT state, entered_at FROM machine_state_history
 WHERE machine_id = ? AND left_at IS NULL
 ORDER BY entered_at DESC LIMIT 1`, machineID).Scan(&openState, &openEntered)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 第一次判決，直接開一段。
	case err != nil:
		return fmt.Errorf("store: read open span: %w", err)
	case state.State(openState) == st:
		return nil // 沒變，不寫。
	default:
		if _, err := tx.Exec(
			`UPDATE machine_state_history SET left_at = ? WHERE machine_id = ? AND entered_at = ?`,
			fmtTime(now), machineID, openEntered); err != nil {
			return fmt.Errorf("store: close span: %w", err)
		}
	}

	// PK 是 (machine_id, entered_at)，秒級。同一秒內連續兩次轉移會撞 ——
	// 用 DO UPDATE 讓最後那個贏，而不是讓整批寫入失敗。
	if _, err := tx.Exec(`
INSERT INTO machine_state_history (machine_id, state, reason, entered_at)
VALUES (?,?,?,?)
ON CONFLICT(machine_id, entered_at) DO UPDATE SET
  state = excluded.state, reason = excluded.reason, left_at = NULL`,
		machineID, string(st), reason, fmtTime(now)); err != nil {
		return fmt.Errorf("store: open span: %w", err)
	}
	return nil
}

func (s *Store) stateHistory(machineID string, limit int) ([]StateSpan, error) {
	return s.stateHistoryAt(machineID, limit, time.Now().UTC())
}

func (s *Store) stateHistoryAt(machineID string, limit int, evaluatedAt time.Time) ([]StateSpan, error) {
	rows, err := s.rdb.Query(`
SELECT state, reason, entered_at,
       CASE WHEN left_at IS NULL OR left_at > ? THEN NULL ELSE left_at END
  FROM machine_state_history
 WHERE machine_id = ? AND entered_at <= ? ORDER BY entered_at DESC LIMIT ?`,
		fmtTime(evaluatedAt), machineID, fmtTime(evaluatedAt), limit)
	if err != nil {
		return nil, fmt.Errorf("store: state history: %w", err)
	}
	defer rows.Close()
	var out []StateSpan
	for rows.Next() {
		var st, reason, entered string
		var left sql.NullString
		if err := rows.Scan(&st, &reason, &entered, &left); err != nil {
			return nil, fmt.Errorf("store: scan span: %w", err)
		}
		out = append(out, StateSpan{
			State: state.State(st), Reason: reason,
			EnteredAt: parseTime(entered), LeftAt: parseTimePtr(left),
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- Overview

// Overview 是 Dashboard 的第一屏。
//
// ⚠ 名冊上每一台未退場的機器都會出現在 Machines 裡，包含從來沒 check-in 過的。
// 這是整個產品的那一句話：名冊有 5 台、只有 4 台報到，那 1 台亮紅燈，
// 不是從畫面上消失。實測案例：sampleagent3 在某台 client 上隱形了七週。
func (s *Store) Overview(now time.Time) (Overview, error) {
	now = now.UTC()
	machines, err := s.ListMachines()
	if err != nil {
		return Overview{}, err
	}
	refresh, err := s.credRefreshHistory(now)
	if err != nil {
		return Overview{}, err
	}
	ov := Overview{Now: now, Counts: map[state.State]int{}}
	for _, m := range machines {
		if m.RetiredAt != nil {
			// 離開分母，但**不離開畫面**。理由見 Overview.Retired 的註解。
			ov.Retired = append(ov.Retired, m)
			continue
		}
		f, err := s.facts(m.MachineID, now, refresh)
		if err != nil {
			return Overview{}, err
		}
		j := state.Derive(f)
		row := MachineRow{Machine: m, State: j.State, Reason: j.Reason, Facts: f, Findings: j.Findings}
		// StateSince 只有在歷史記的狀態跟現在算出來的一樣時才給值 ——
		// 不一樣表示還沒有人呼叫 RecordStateTransition，那就誠實地留空。
		if span, ok, err := s.openStateSpanAt(m.MachineID, now); err != nil {
			return Overview{}, err
		} else if ok && span.State == j.State && !span.EnteredAt.IsZero() {
			entered := span.EnteredAt
			row.StateSince = &entered
		}
		ov.Machines = append(ov.Machines, row)
		ov.Counts[j.State]++
		ov.Total++
		if m.InDenominator() {
			ov.Expected++
		}
		for _, fi := range j.Findings {
			ov.Findings = append(ov.Findings, FleetFinding{
				MachineID: m.MachineID, DisplayName: m.DisplayName, Finding: fi,
			})
		}
	}

	// 最該看的排最上面。同樣嚴重就照名字排，讓畫面在兩次重整之間是穩定的。
	sort.SliceStable(ov.Machines, func(i, j int) bool {
		a, b := ov.Machines[i], ov.Machines[j]
		if a.State.Severity() != b.State.Severity() {
			return a.State.Severity() > b.State.Severity()
		}
		return a.DisplayName < b.DisplayName
	})
	sort.SliceStable(ov.Findings, func(i, j int) bool {
		a, b := ov.Findings[i], ov.Findings[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if a.Advisory != b.Advisory {
			return !a.Advisory // 會影響狀態的排在只是提醒的前面
		}
		return a.DisplayName < b.DisplayName
	})
	return ov, nil
}

// ---------------------------------------------------------------- Detail

// Detail 是單機詳細頁。每一盞燈都要點得進去看到證據，這裡就是那些證據。
func (s *Store) Detail(machineID string, now time.Time) (Detail, error) {
	now = now.UTC()
	m, err := s.GetMachine(machineID)
	if err != nil {
		return Detail{}, err
	}
	refresh, err := s.credRefreshHistory(now)
	if err != nil {
		return Detail{}, err
	}
	f, err := s.facts(machineID, now, refresh)
	if err != nil {
		return Detail{}, err
	}
	j := state.Derive(f)
	d := Detail{
		Machine: m, Now: now,
		State: j.State, Reason: j.Reason, Findings: j.Findings, Facts: f,
	}

	if d.Checkins, err = s.checkinPoints(machineID, now); err != nil {
		return Detail{}, err
	}
	if len(d.Checkins) > DetailCheckinLimit {
		d.CheckinsTruncated = true
		d.Checkins = d.Checkins[len(d.Checkins)-DetailCheckinLimit:]
	}
	if d.History, err = s.stateHistoryAt(machineID, DetailHistoryLimit+1, now); err != nil {
		return Detail{}, err
	}
	if len(d.History) > DetailHistoryLimit {
		d.HistoryTruncated = true
		d.History = d.History[:DetailHistoryLimit]
	}
	if d.IdentityHints, err = s.identityHintsAt(machineID, m.MachineIDHint, now); err != nil {
		return Detail{}, err
	}

	if obs, ok, err := s.latestObservationReceivedBy(machineID, KindIdentity, KindIdentity, now); err != nil {
		return Detail{}, err
	} else if ok {
		d.IdentityObserved = true
		d.IdentityObservedAt = &ObservationClock{MeasuredAt: obs.MeasuredAt, ReceivedAt: obs.ReceivedAt}
		if id, ok := unmarshalInto[model.Identity](obs.Payload); ok {
			d.IdentityDecoded = true
			d.Identity = &id
		}
	}
	if obs, ok, err := s.latestObservationReceivedBy(machineID, KindResources, KindResources, now); err != nil {
		return Detail{}, err
	} else if ok {
		d.ResourcesObserved = true
		d.ResourcesObservedAt = &ObservationClock{MeasuredAt: obs.MeasuredAt, ReceivedAt: obs.ReceivedAt}
		if r, ok := unmarshalInto[model.Resources](obs.Payload); ok {
			d.ResourcesDecoded = true
			d.Resources = &r
		}
	}
	if obs, ok, err := s.latestObservationReceivedBy(machineID, KindBAT, KindBAT, now); err != nil {
		return Detail{}, err
	} else if ok {
		d.Connect.Observed = true
		d.Connect.MeasuredAt = obs.MeasuredAt
		d.Connect.ReceivedAt = obs.ReceivedAt
		if b, ok := unmarshalInto[model.BAT](obs.Payload); ok {
			d.Connect = connectInfo(d, &b)
			d.Connect.Observed = true
			d.Connect.Decoded = true
			d.Connect.MeasuredAt = obs.MeasuredAt
			d.Connect.ReceivedAt = obs.ReceivedAt
		} else {
			d.Connect.Why = "bat-server observation payload 無法解碼"
		}
	} else {
		d.Connect = connectInfo(d, nil)
	}
	if s.expects != nil {
		d.ExpectConfigured, d.ExpectErr = s.expects.Configured, s.expects.Err
	}
	return d, nil
}

// checkinPoints 取最近的心跳，由舊到新回傳（sparkline 從左往右畫）。
func (s *Store) checkinPoints(machineID string, now time.Time) ([]CheckinPoint, error) {
	rows, err := s.rdb.Query(`
SELECT sent_at, received_at, agent_version, boot_id, agent_seq, uptime_seconds,
       disk_free_bytes, disk_total_bytes, observation_age_seconds, clock_skew_seconds
  FROM machine_checkins
 WHERE machine_id = ? AND received_at >= ? AND received_at <= ?
 ORDER BY received_at DESC, rowid DESC LIMIT ?`,
		machineID, fmtTime(now.Add(-DetailCheckinWindow)), fmtTime(now), DetailCheckinLimit+1)
	if err != nil {
		return nil, fmt.Errorf("store: checkin points: %w", err)
	}
	defer rows.Close()

	var out []CheckinPoint
	for rows.Next() {
		var sent, recv string
		var agentVer, bootID sql.NullString
		var seq, uptime, free, total, obsAge, skew sql.NullInt64
		if err := rows.Scan(&sent, &recv, &agentVer, &bootID, &seq, &uptime,
			&free, &total, &obsAge, &skew); err != nil {
			return nil, fmt.Errorf("store: scan checkin: %w", err)
		}
		p := CheckinPoint{
			SentAt: parseTime(sent), ReceivedAt: parseTime(recv),
			AgentVersion: agentVer.String, BootID: bootID.String,
			AgentSeq: seq.Int64, UptimeSeconds: uptime.Int64,
			DiskFreeBytes: free.Int64, DiskTotalBytes: total.Int64,
			ClockSkew:        time.Duration(skew.Int64) * time.Second,
			AgentSeqObserved: seq.Valid, UptimeObserved: uptime.Valid,
			DiskFreeObserved: free.Valid, DiskTotalObserved: total.Valid,
			ClockSkewObserved: skew.Valid,
		}
		if obsAge.Valid {
			v := obsAge.Int64
			p.ObservationAgeSeconds = &v
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ---------------------------------------------------------------- 相對昨天的變化

// changeKinds 是會拿去做「跟昨天比」的觀測種類。
//
// ⚠ 刻意不含 resources：磁碟每一批都在動，把它算成變化會讓早報變成噪音，
// 然後第一週人就把通知關掉了。磁碟越過門檻是判決（internal/state 的事），
// 不是「變化」。
//
// ⚠⚠ 同理刻意不含 journal 與 artifact，而且**不要加進來**：
//   - journal 的行數每一批都在動，理由跟 resources 一模一樣。
//   - artifact 的 mtime 在健康的時候每一輪都在動 —— 一個**正常運轉**的
//     unit 會讓它每次都算成「變化」，於是早報天天報一件好事，
//     而真正壞掉（mtime 不動了）的那天反而**沒有變化可報**。
//     這是這個專案最愛犯的那種反向錯誤：把「它壞了」變成畫面上的空白。
//     產出物過期是判決（internal/state 的 workload finding），不是「變化」。
var changeKinds = []string{KindIdentity, KindCredential, KindCLITool, KindSystemd, KindOpenClaw}

// artifactFacts 把「人宣告的期望」跟「這台的最新量測」對起來。
//
// ⚠⚠ 以**宣告**為主鍵去迴圈，不是以量測。
// 反過來寫（迴圈跑量測）會讓一條「宣告了、但 agent 從來沒回報過」的期望
// 安靜地消失 —— 而那正是最該被看見的狀況之一：你以為你在監控它，其實沒有。
func (s *Store) artifactFacts(machineID, displayName string, now time.Time) []state.ArtifactFact {
	rules := s.expects.For(displayName)
	if len(rules) == 0 {
		return nil
	}
	// 先把量測讀進 map，鍵跟寫入時一致。
	measured := map[string]model.ArtifactCheck{}
	artifactDecoded := map[string]bool{}
	artifactInvalid := map[string]bool{}
	artifactRows := map[string]observation{}
	if obs, err := s.latestBySubject(machineID, KindArtifact, now); err != nil {
		// ⚠ 讀不到要出聲。安靜地當作「沒量到」會讓所有期望都變成
		// 「還沒量到」的 advisory，而那看起來很像「才剛裝好」。
		log.Printf("cannot read artifact measurements for %s, unable to determine expectations this round: %v", displayName, err)
	} else {
		for _, o := range obs {
			artifactRows[o.Subject] = o
			if a, ok := unmarshalInto[model.ArtifactCheck](o.Payload); ok {
				artifactDecoded[o.Subject] = true
				if a.Unit+"\x1f"+a.Artifact == o.Subject {
					measured[o.Subject] = a
				} else {
					artifactInvalid[o.Subject] = true
				}
			}
		}
	}
	events := map[string]model.EventStream{}
	eventDecoded := map[string]bool{}
	eventInvalid := map[string]bool{}
	eventRows := map[string]observation{}
	if obs, err := s.latestBySubject(machineID, KindEvents, now); err != nil {
		log.Printf("cannot read event stream measurements for %s, unable to determine event expectations this round: %v", displayName, err)
	} else {
		for _, o := range obs {
			eventRows[o.Subject] = o
			if e, ok := unmarshalInto[model.EventStream](o.Payload); ok {
				eventDecoded[o.Subject] = true
				if e.Unit+"\x1f"+e.Path == o.Subject {
					events[o.Subject] = e
				} else {
					eventInvalid[o.Subject] = true
				}
			}
		}
	}

	facts := artifactFactsFromMeasurements(rules, measured, events, now)
	for i := range facts {
		key := facts[i].Unit + "\x1f" + facts[i].Artifact
		if row, ok := artifactRows[key]; ok {
			facts[i].ObservationObserved = true
			facts[i].MeasuredAt, facts[i].ReceivedAt = row.MeasuredAt, row.ReceivedAt
			facts[i].ObservationDecoded = artifactDecoded[key]
			facts[i].ObservationInvalid = artifactInvalid[key]
		}
		if facts[i].Events != nil {
			if row, ok := eventRows[key]; ok {
				facts[i].Events.ObservationObserved = true
				facts[i].Events.MeasuredAt, facts[i].Events.ReceivedAt = row.MeasuredAt, row.ReceivedAt
				facts[i].Events.ObservationDecoded = eventDecoded[key]
				facts[i].Events.ObservationInvalid = eventInvalid[key]
			}
		}
	}
	return facts
}

func artifactFactsFromMeasurements(rules []model.Expectation, measured map[string]model.ArtifactCheck, events map[string]model.EventStream, now time.Time) []state.ArtifactFact {
	out := make([]state.ArtifactFact, 0, len(rules))
	for _, r := range rules {
		af := state.ArtifactFact{
			Unit:     r.Unit,
			Artifact: r.Artifact,
			Why:      r.Why,
			MaxAge:   time.Duration(r.MaxAgeSeconds) * time.Second,
		}
		key := r.Unit + "\x1f" + r.Artifact
		if a, ok := measured[key]; ok {
			af.Checked, af.Exists, af.ModTime, af.Err = true, a.Exists, a.ModTime, a.Err
		}
		// ⚠ 同樣以**宣告**為主：規則說要讀事件流，這裡就一定有 Events，
		// 即使量測還沒回來（Measured=false）。少掉它會讓
		// 「宣告了但沒在讀」跟「讀了但很安靜」長得一樣。
		if r.Events != nil {
			af.Events = &state.EventFact{
				Window:       time.Duration(r.Events.WindowSeconds) * time.Second,
				FailureTypes: append([]string(nil), r.Events.NotOK...),
			}
			if e, ok := events[key]; ok {
				af.Events.Measured = true
				af.Events.Declared = toEventCounts(e.Declared)
				af.Events.Undeclared = toEventCounts(e.Undeclared)
				af.Events.UndeclaredTotal = e.UndeclaredTotal
				af.Events.Malformed, af.Events.Err = e.Malformed, e.Err
				af.Events.CoveredFrom = e.CoveredFrom
				// Partial 不等於 Truncated：讀不完整份檔案沒關係，
				// ⚠ 有關係的是**沒讀滿窗口** —— 那時候次數才是下界。
				if e.Truncated && e.CoveredFrom != nil {
					af.Events.Partial = e.CoveredFrom.After(
						now.UTC().Add(-time.Duration(r.Events.WindowSeconds) * time.Second))
				}
			}
		}
		out = append(out, af)
	}
	return out
}

func toEventCounts(in []model.EventSummary) []state.EventCount {
	if len(in) == 0 {
		return nil
	}
	out := make([]state.EventCount, 0, len(in))
	for _, e := range in {
		out = append(out, state.EventCount{Type: e.Type, Count: e.Count, LastAt: e.LastAt})
	}
	return out
}

// DisplayName 回機器的顯示名稱。查不到回空字串。
func (s *Store) DisplayName(machineID string) string {
	var n string
	if err := s.rdb.QueryRow(
		`SELECT display_name FROM machine_registry WHERE machine_id = ?`, machineID).Scan(&n); err != nil {
		return ""
	}
	return n
}

// ChangesSince 回答「相對昨天變了什麼」—— Dashboard 的預設畫面與早報的內容。
//
// ⚠ 這個查詢只有在 observed_state 是 append-only 的前提下才成立。
// 任何把它改成「一台一列的快照」的最佳化都會讓這個函式永遠回傳空的，
// 而那正是這個產品唯一的賣點。
//
// 比較方式：對每個 (machine, kind, subject)，取 measured_at ≤ since 的最新一列
// 當「昨天」，取 measured_at ≤ now 的最新一列當「今天」，兩邊的摘要不一樣就是一筆變化。
func (s *Store) ChangesSince(since, now time.Time) ([]Change, error) {
	since, now = since.UTC(), now.UTC()

	names := map[string]string{}
	machines, err := s.ListMachines()
	if err != nil {
		return nil, err
	}
	for _, m := range machines {
		names[m.MachineID] = m.DisplayName
	}
	name := func(id string) string {
		if n, ok := names[id]; ok && n != "" {
			return n
		}
		return id
	}

	var out []Change

	// --- 1. 狀態轉移。⚠ Severity 直接取 state 判過的嚴重度。
	rows, err := s.rdb.Query(`
SELECT h.machine_id, h.state, h.entered_at,
       (SELECT p.state FROM machine_state_history p
         WHERE p.machine_id = h.machine_id AND p.entered_at < h.entered_at
         ORDER BY p.entered_at DESC LIMIT 1)
  FROM machine_state_history h
 WHERE h.entered_at > ? AND h.entered_at <= ?
 ORDER BY h.entered_at`, fmtTime(since), fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("store: state changes: %w", err)
	}
	for rows.Next() {
		var id, st, entered string
		var prev sql.NullString
		if err := rows.Scan(&id, &st, &entered, &prev); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan state change: %w", err)
		}
		out = append(out, Change{
			MachineID: id, DisplayName: name(id), Kind: "state", Subject: "state",
			From: prev.String, To: st, At: parseTime(entered),
			Severity: state.State(st).Severity(),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// --- 2. 名冊本身的變動：新納管 / 退場。分母變了是早報第一等的事。
	for _, m := range machines {
		if m.EnrolledAt != nil && m.EnrolledAt.After(since) && !m.EnrolledAt.After(now) {
			out = append(out, Change{
				MachineID: m.MachineID, DisplayName: m.DisplayName, Kind: "registry",
				Subject: "enrollment", To: "enrolled", At: *m.EnrolledAt, Severity: 1,
			})
		}
		if m.RetiredAt != nil && m.RetiredAt.After(since) && !m.RetiredAt.After(now) {
			out = append(out, Change{
				MachineID: m.MachineID, DisplayName: m.DisplayName, Kind: "registry",
				Subject: "enrollment", From: "enrolled", To: "retired", At: *m.RetiredAt, Severity: 1,
			})
		}
	}

	// --- 3. 觀測到的事實變了。
	before, err := s.snapshotAt(since)
	if err != nil {
		return nil, err
	}
	after, err := s.snapshotAt(now)
	if err != nil {
		return nil, err
	}
	for key, cur := range after {
		old, existed := before[key]
		if existed && old.summary == cur.summary {
			continue
		}
		if !existed && cur.summary == "" {
			continue
		}
		out = append(out, Change{
			MachineID: key.machineID, DisplayName: name(key.machineID),
			Kind: key.kind, Subject: key.subject,
			From: old.summary, To: cur.summary, At: cur.at,
			// ⚠ 一律 1。store 不判斷一個事實有多嚴重 —— 那是判決。
			// 早報的排序規則是 docs/OPEN-QUESTIONS Q8，屬於報告層。
			Severity: 1,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		if a.DisplayName != b.DisplayName {
			return a.DisplayName < b.DisplayName
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Subject < b.Subject
	})
	return out, nil
}

type subjectKey struct{ machineID, kind, subject string }

type subjectSnapshot struct {
	summary string
	at      time.Time
}

// snapshotAt 取「截至 cutoff 為止」每個 (machine, kind, subject) 最新的一列，
// 壓成一句可比較的摘要。
func (s *Store) snapshotAt(cutoff time.Time) (map[subjectKey]subjectSnapshot, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(changeKinds)), ",")
	args := []any{fmtTime(cutoff)}
	for _, k := range changeKinds {
		args = append(args, k)
	}
	args = append(args, fmtTime(cutoff))

	rows, err := s.rdb.Query(`
SELECT o.machine_id, o.kind, o.subject, o.payload, o.measured_at
  FROM observed_state o
 WHERE o.measured_at <= ? AND o.kind IN (`+placeholders+`)
   AND o.measured_at = (SELECT MAX(o2.measured_at) FROM observed_state o2
                         WHERE o2.machine_id = o.machine_id AND o2.kind = o.kind
                           AND o2.subject = o.subject AND o2.measured_at <= ?)
 ORDER BY o.machine_id, o.kind, o.subject, o.rowid DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: snapshot: %w", err)
	}
	defer rows.Close()

	out := map[subjectKey]subjectSnapshot{}
	for rows.Next() {
		var id, kind, subject, payload, measured string
		if err := rows.Scan(&id, &kind, &subject, &payload, &measured); err != nil {
			return nil, fmt.Errorf("store: scan snapshot: %w", err)
		}
		k := subjectKey{id, kind, subject}
		if _, seen := out[k]; seen {
			continue // 同 measured_at 平手，rowid 大的（先出現的）贏
		}
		out[k] = subjectSnapshot{summary: summarizeObservation(kind, payload), at: parseTime(measured)}
	}
	return out, rows.Err()
}

// summarizeObservation 把一筆觀測壓成一句人看得懂的話，只用來做前後比對。
//
// ⚠⚠ 這裡只讀事實欄位，絕對不碰 OpenClawDB.RecentSummaries。
// 那些 summary 是給人讀的原文；在程式裡讀它們來推斷成敗是「掃 log 關鍵字」
// 的變形，docs/PRODUCT.md 的地基二否決過。實測 status='ok' 的任務裡
// 64% 的 summary 在描述失敗 —— 機器讀這些字只會得到錯的答案。
//
// 也不碰 TaskStatusCount / CronStatusCount：同樣的理由，'ok' 不代表成功。
func summarizeObservation(kind, payload string) string {
	switch kind {
	case KindCredential:
		c, ok := unmarshalInto[model.Credential](payload)
		if !ok {
			return ""
		}
		// ⚠ 五個詞原樣。不要在這裡把它們分成「好」跟「不好」兩堆。
		out := string(c.Status)
		if c.ExpiresAt != nil {
			out += " expires=" + fmtTime(*c.ExpiresAt)
		}
		if c.ActiveAccountID != "" {
			out += " account=" + c.ActiveAccountID
		}
		return out
	case KindCLITool:
		t, ok := unmarshalInto[model.CLITool](payload)
		if !ok {
			return ""
		}
		if !t.Present {
			return "absent"
		}
		out := "version=" + t.VersionReported
		if t.VersionPackageJSON != "" && t.VersionPackageJSON != t.VersionReported {
			out += " package.json=" + t.VersionPackageJSON
		}
		if t.SourcesDisagree {
			// ⚠ 如實回報矛盾，不要挑一個當答案。
			out += " (版本來源互相矛盾)"
		}
		if t.RunningExe != "" && t.RealPath != "" && t.RunningExe != t.RealPath {
			out += " running=" + t.RunningExe
		}
		return out
	case KindSystemd:
		u, ok := unmarshalInto[model.Unit](payload)
		if !ok {
			return ""
		}
		if !u.Present {
			return "absent"
		}
		// NRestarts 進來是刻意的：ActiveState 幾乎沒有資訊量，
		// 一個每 10 秒崩一次的服務也是 "active"，重啟次數才看得出來。
		return fmt.Sprintf("%s/%s restarts=%d", u.ActiveState, u.SubState, u.NRestarts)
	case KindOpenClaw:
		oc, ok := unmarshalInto[model.OpenClaw](payload)
		if !ok {
			return ""
		}
		if !oc.Present {
			return "absent: " + oc.Reason
		}
		// 三個版本座標分開講 —— CLI 舊了跟 gateway 舊了的修法不一樣。
		return fmt.Sprintf("cli=%s gateway=%s upstream=%s",
			oc.CLIVersion, oc.GatewayVersion, oc.UpstreamVersion)
	case KindIdentity:
		id, ok := unmarshalInto[model.Identity](payload)
		if !ok {
			return ""
		}
		return fmt.Sprintf("hostname=%s machine-id=%s linger=%t",
			id.Hostname, id.MachineIDHint, id.LingerEnabled)
	}
	return ""
}

// ---------------------------------------------------------------- 通知

// RecordNotification 記一次推播。
//
// ⚠ 送失敗也要記。「今天沒收到早報」必須能區分成「Hub 死了」跟
// 「Hub 活著但 Telegram 掛了」—— 沒有這一列就分不出來。
func (s *Store) RecordNotification(kind, channel, body string, delivered bool, errMsg string, now time.Time) error {
	_, err := s.execWrite(context.Background(), "record_notification", `
INSERT INTO notifications (notification_id, kind, channel, sent_at, body, delivered, error)
VALUES (?,?,?,?,?,?,?)`,
		newID(), kind, channel, fmtTime(now), body, boolToInt(delivered), nullStr(errMsg))
	if err != nil {
		return fmt.Errorf("store: record notification: %w", err)
	}
	return nil
}

// LastNotification 回傳這個 kind 最後一次「真的送到」的時間。
//
// ⚠ 只算 delivered=1。送失敗的嘗試不算送過 —— 人手上沒有那則訊息。
// 呼叫端據此決定「今天的早報還要不要送」，把失敗當成功會讓一整天靜悄悄。
func (s *Store) LastNotification(kind string) (time.Time, bool, error) {
	var sentAt sql.NullString
	err := s.rdb.QueryRow(
		`SELECT MAX(sent_at) FROM notifications WHERE kind = ? AND delivered = 1`, kind).Scan(&sentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: last notification: %w", err)
	}
	t := parseTimeNull(sentAt)
	if t.IsZero() {
		return time.Time{}, false, nil
	}
	return t, true, nil
}

// NotificationAttemptsSince counts notification rows for kind at or after since.
// failures is the undelivered count and lastFailed is the newest of those.
// The daily report derives its backoff from this so a restart does not forget
// how many attempts already failed today.
func (s *Store) NotificationAttemptsSince(kind string, since time.Time) (rows, failures int, lastFailed time.Time, err error) {
	var raw sql.NullString
	err = s.rdb.QueryRow(`
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN delivered = 0 THEN 1 ELSE 0 END), 0),
       MAX(CASE WHEN delivered = 0 THEN sent_at END)
  FROM notifications
 WHERE kind = ? AND sent_at >= ?`, kind, fmtTime(since)).Scan(&rows, &failures, &raw)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("store: notification attempts: %w", err)
	}
	return rows, failures, parseTimeNull(raw), nil
}

// NotifyKindStats is the /metrics view of one notification kind.
type NotifyKindStats struct {
	LastSuccess         time.Time
	HasSuccess          bool
	LastAttempt         time.Time
	HasAttempt          bool
	ConsecutiveFailures int64
}

// NotifyKindStats reads delivery timestamps for kind. ConsecutiveFailures is
// the number of undelivered rows newer than the latest delivered row.
func (s *Store) NotifyKindStats(kind string) (NotifyKindStats, error) {
	var success, attempt sql.NullString
	var fails int64
	err := s.rdb.QueryRow(`
SELECT
  (SELECT MAX(sent_at) FROM notifications WHERE kind = ? AND delivered = 1),
  (SELECT MAX(sent_at) FROM notifications WHERE kind = ?),
  (SELECT COUNT(*) FROM notifications
    WHERE kind = ? AND delivered = 0
      AND rowid > COALESCE((SELECT MAX(rowid) FROM notifications WHERE kind = ? AND delivered = 1), 0))`,
		kind, kind, kind, kind).Scan(&success, &attempt, &fails)
	if err != nil {
		return NotifyKindStats{}, fmt.Errorf("store: notify stats: %w", err)
	}
	out := NotifyKindStats{ConsecutiveFailures: fails}
	out.LastSuccess = parseTimeNull(success)
	out.HasSuccess = !out.LastSuccess.IsZero()
	out.LastAttempt = parseTimeNull(attempt)
	out.HasAttempt = !out.LastAttempt.IsZero()
	return out, nil
}

// ConnectInfo 是詳細頁最底下那一段所需要的一切。
//
// ⚠⚠ URL 為空**不是**錯誤狀態，它是一個常見而且必須講清楚的答案。
// 實測機隊五台裡：兩台給得出位址、兩台 bat-server 綁在 localhost
// （任何位址都連不上）、一台根本沒跑 BAT。
// 一個永遠給得出位址的 Connect 面板，是在對其中三台說謊。
type ConnectInfo struct {
	URL string `json:"url,omitempty"`
	Why string `json:"why,omitempty"` // 給不出位址的原因，人看得懂的話

	BAT        model.BAT `json:"bat"`
	Observed   bool      `json:"observed"` // ledger 有沒有這台的 BAT 觀測 row
	Decoded    bool      `json:"decoded"`  // 最新 row payload 能不能解成 BAT
	MeasuredAt time.Time `json:"measured_at,omitempty"`
	ReceivedAt time.Time `json:"received_at,omitempty"`

	// RegistryIP 是名冊上那個報到當天寫下的位址。
	// ⚠ 只在它跟觀測到的不一樣時才有值 —— 那個不一樣本身要讓人看到。
	RegistryIP string `json:"registry_ip,omitempty"`
}

// connectInfo 把觀測到的 BAT 與這台自己報的 tailnet 位址兜成一個答案。
//
// ⚠ 用的是 d.Identity 的 tailscale_ip（它最近一次自己報的），
// 不是 d.Machine.TailscaleIP（名冊在報到那一刻寫下、之後永遠不更新的）。
// tailnet 位址會被回收，所以拿舊位址去連，最壞的情況不是連不上，
// 是連到**別台機器**然後在那台上動手。
func connectInfo(d Detail, bat *model.BAT) ConnectInfo {
	var observedIP string
	if d.Identity != nil {
		observedIP = d.Identity.TailscaleIP
	}
	c := ConnectInfo{}
	if observedIP != "" && d.Machine.TailscaleIP != "" && observedIP != d.Machine.TailscaleIP {
		c.RegistryIP = d.Machine.TailscaleIP
	}
	if bat == nil {
		// 還沒收過這台的 BAT 觀測。⚠ 這跟「這台沒跑 BAT」不一樣：
		// 舊版 agent 不送這個欄位，而那時候我們什麼都不知道。
		// 把「不知道」寫成「沒有」，就是這個專案存在要防的那件事。
		c.Why = "尚無 bat-server 觀測"
		return c
	}
	c.BAT, c.Decoded = *bat, true
	// A registry IP is an enrollment-time hint and may have been recycled. An
	// explicit non-loopback listener can stand on its own; a wildcard listener
	// may only be paired with the latest decoded identity address.
	c.URL, c.Why = model.ConnectURL(c.BAT, observedIP)
	return c
}
