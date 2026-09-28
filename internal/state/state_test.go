package state

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// healthy 是一台各方面都正常的機器。每個測試只改它一個欄位，
// 這樣失敗時就知道是哪一條規則出問題。
func healthy() Facts {
	return Facts{
		Now:                 now,
		EverCheckedIn:       true,
		LastCheckinReceived: now.Add(-30 * time.Second),
		CheckinInterval:     CheckinInterval,
		HasObservation:      true,
		LastObservation:     now.Add(-2 * time.Minute),
		DiskFreeBytes:       500 << 30,
		DiskTotalBytes:      1000 << 30,
		OpenClawObserved:    true,
		OpenClawPresent:     true,
		OpenClawRunning:     true,
		OpenClawProcessScan: "complete",
		HasTaskSignal:       true,
		LastTaskEnded:       now.Add(-10 * time.Minute),
	}
}

func TestHealthyIsOnline(t *testing.T) {
	j := Derive(healthy())
	if j.State != Online {
		t.Fatalf("預期 Online，得到 %s（%s）", j.State, j.Reason)
	}
	if j.Reason == "" {
		t.Error("每個判決都必須附一句人看得懂的理由")
	}
}

func TestTheLastTaskSentenceNeverCallsAFutureTimestampThePast(t *testing.T) {
	// 這句話是「最近一次跑完」自己講的，不是掛在時鐘 finding 上：healthy() 沒有 clock skew，
	// 所以這四列都沒有時鐘 finding，那句話仍然要出現。
	tests := []struct {
		name          string
		lastTaskEnded time.Time
		want          string
		banned        []string
	}{
		{
			name:          "跑完在過去",
			lastTaskEnded: now.Add(-10 * time.Minute),
			want:          "最近一次跑完是 10 分鐘前",
			banned:        []string{"晚", "後"},
		},
		// 這列鎖定分支條件是 idle < 0，不是 idle <= 0。
		{
			name:          "剛好就是現在",
			lastTaskEnded: now,
			want:          "最近一次跑完是 0 秒前",
			banned:        []string{"晚", "後"},
		},
		{
			name:          "agent 的鐘快一點",
			lastTaskEnded: now.Add(80 * time.Second),
			want:          "最近一次跑完的時間比現在晚 1 分鐘",
			banned:        []string{"是 1 分鐘前", "後"},
		},
		{
			name:          "agent 的鐘快很多",
			lastTaskEnded: now.Add(10 * time.Hour),
			want:          "最近一次跑完的時間比現在晚 10.0 小時",
			banned:        []string{"是 10.0 小時前", "後"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := healthy()
			f.LastTaskEnded = tt.lastTaskEnded
			j := Derive(f)
			for i := range j.Findings {
				if j.Findings[i].Kind == "clock" {
					t.Errorf("這句話不可以靠時鐘 finding 才成立，但出現了：%s", j.Findings[i].Message)
				}
			}

			if j.State != Online {
				t.Errorf("未來的任務時間不該降級，預期 Online，得到 %s（%s）", j.State, j.Reason)
			}
			if !strings.Contains(j.Reason, tt.want) {
				t.Errorf("主理由應包含 %q，實際：%s", tt.want, j.Reason)
			}
			for _, banned := range tt.banned {
				if strings.Contains(j.Reason, banned) {
					t.Errorf("主理由不應包含 %q，實際：%s", banned, j.Reason)
				}
			}

			var workload *Finding
			for i := range j.Findings {
				if j.Findings[i].Kind == "workload" && strings.Contains(j.Findings[i].Message, "最近一次跑完") {
					workload = &j.Findings[i]
					break
				}
			}
			if workload == nil {
				t.Fatal("找不到最近一次跑完的 workload finding")
			}
			if !strings.Contains(workload.Message, tt.want) {
				t.Errorf("workload finding 應包含 %q，實際：%s", tt.want, workload.Message)
			}
			for _, banned := range tt.banned {
				if strings.Contains(workload.Message, banned) {
					t.Errorf("workload finding 不應包含 %q，實際：%s", banned, workload.Message)
				}
			}
			if !workload.Advisory {
				t.Errorf("workload finding 應維持 advisory，實際：%+v", *workload)
			}
			if workload.Severity != 1 {
				t.Errorf("workload finding severity 應維持 1，實際：%d", workload.Severity)
			}
		})
	}
}

// ⚠ 這是整個 package 最重要的測試。
//
// L1 訊號只代表「跑完了」，不代表「做對了」——實測上游 status='ok' 有
// 64% 的 summary 在描述失敗。所以綠燈的文案永遠不准說「健康」或「成功」。
// 有人來「改善文案」時，這個測試會擋住他。
func TestOnlineNeverClaimsHealthyOrSuccess(t *testing.T) {
	j := Derive(healthy())
	banned := []string{"健康", "成功", "正常運作", "healthy", "success"}
	all := j.Reason
	for _, f := range j.Findings {
		all += " " + f.Message
	}
	for _, w := range banned {
		if strings.Contains(all, w) {
			t.Errorf("判決文字不准出現 %q —— L1 只知道「跑完了」，不知道「做對了」。實際：%s", w, all)
		}
	}
	if !strings.Contains(all, "跑完") {
		t.Errorf("應該明確說「跑完」而不是含糊的好壞，實際：%s", all)
	}
}

// 名冊是分母：從沒報到的機器是 NeverReported，不是消失。
func TestNeverCheckedInIsDeadNotMissing(t *testing.T) {
	f := healthy()
	f.EverCheckedIn = false
	j := Derive(f)
	if j.State != NeverReported {
		t.Fatalf("預期 NeverReported，得到 %s", j.State)
	}
	if !strings.Contains(j.Reason, "從未報到") || !strings.Contains(j.Reason, "管理分母") {
		t.Errorf("理由要說明它算在分母裡，實際：%s", j.Reason)
	}
}

// 心跳間隔 = 失聯門檻會製造假紅。必須有寬限。
func TestUnreachableHasGraceBeyondInterval(t *testing.T) {
	f := healthy()
	// 剛好超過一個間隔，但還在寬限內 → 不該是 Unreachable
	f.LastCheckinReceived = now.Add(-CheckinInterval - 30*time.Second)
	if j := Derive(f); j.State == Unreachable {
		t.Errorf("超過間隔但仍在 90s 寬限內不該判失聯（這會製造假紅），得到 %s", j.State)
	}
	// 超過間隔 + 寬限 → Unreachable
	f.LastCheckinReceived = now.Add(-CheckinInterval - UnreachableGrace - time.Second)
	if j := Derive(f); j.State != Unreachable {
		t.Errorf("超過間隔+寬限應該判失聯，得到 %s", j.State)
	}
}

// 沒開 linger 的機器會假離線。必須在理由裡分開講，
// 否則人會跑去查一台其實好好的機器。
func TestUnreachableDistinguishesMissingLinger(t *testing.T) {
	f := healthy()
	f.LastCheckinReceived = now.Add(-time.Hour)
	no := false
	f.LingerEnabled = &no
	j := Derive(f)
	if j.State != Unreachable {
		t.Fatalf("預期 Unreachable，得到 %s", j.State)
	}
	if !strings.Contains(j.Reason, "linger") {
		t.Errorf("沒開 linger 的失聯必須在理由裡講出來，實際：%s", j.Reason)
	}
}

func TestUnmeasuredLingerProducesNoLingerFinding(t *testing.T) {
	f := healthy()
	f.LastCheckinReceived = now.Add(-time.Hour)
	f.LingerEnabled = nil
	j := Derive(f)
	if j.State != Unreachable {
		t.Fatalf("未量到 linger 的失聯機器預期 Unreachable，得到 %s", j.State)
	}
	if strings.Contains(j.Reason, "linger 未啟用") {
		t.Errorf("未量到 linger 的失聯理由卻聲稱 linger 未啟用：%s", j.Reason)
	}
	for _, finding := range j.Findings {
		if strings.Contains(finding.Message, "未啟用 linger") {
			t.Errorf("未量到 linger 卻產生『未啟用 linger』finding：%+v", finding)
		}
	}
}

// process 活著但很久沒跑完東西 —— 這是這個產品存在的理由。
func TestSilentFailureIsNotGreen(t *testing.T) {
	f := healthy()
	f.LastTaskEnded = now.Add(-SilentFailure - time.Hour)
	j := Derive(f)
	if j.State == Online {
		t.Fatalf("9 小時沒跑完任何東西不該是綠燈")
	}
	if !strings.Contains(j.Reason, "沒有任何任務跑完") {
		t.Errorf("理由要講清楚是什麼沒發生，實際：%s", j.Reason)
	}
}

// 裝了但從來沒跑完 —— 實測 sampleagent3 就是這樣，開機 79 天、task_runs 是空的。
func TestNeverRanAnythingIsDegraded(t *testing.T) {
	f := healthy()
	f.HasTaskSignal = false
	j := Derive(f)
	if j.State != Degraded {
		t.Fatalf("預期 Degraded，得到 %s", j.State)
	}
	if !strings.Contains(j.Reason, "從來沒跑完") {
		t.Errorf("實際：%s", j.Reason)
	}
}

// 資料庫讀不到只代表任務紀錄未知，不能把「不知道」說成從來沒跑完。
func TestUnreadableOpenClawDBDoesNotClaimItNeverRan(t *testing.T) {
	f := healthy()
	f.HasTaskSignal = false
	f.OpenClawDBReason = "sqlite: unable to open database file"
	j := Derive(f)
	if j.State != Degraded {
		t.Fatalf("預期 Degraded，得到 %s", j.State)
	}
	all := j.Reason
	for _, finding := range j.Findings {
		all += " " + finding.Message
	}
	if !strings.Contains(all, f.OpenClawDBReason) {
		t.Errorf("判決理由或 findings 找不到 agent 原因，實際：%s", all)
	}
	if strings.Contains(all, "從來沒跑完過任何東西") {
		t.Errorf("資料庫讀不到時不可以斷言從來沒跑完，實際：%s", all)
	}

	f.OpenClawDBReason = ""
	j = Derive(f)
	all = j.Reason
	for _, finding := range j.Findings {
		all += " " + finding.Message
	}
	if !strings.Contains(all, "從來沒跑完過任何東西") {
		t.Errorf("沒有 DB 讀取錯誤時必須維持既有判決，實際：%s", all)
	}
}

// 沒裝 OpenClaw 不是故障（sampleagent1 就是刻意沒裝）。
func TestOpenClawAbsentIsNotAFailure(t *testing.T) {
	f := healthy()
	f.OpenClawPresent = false
	f.HasTaskSignal = false
	if j := Derive(f); j.State != Online {
		t.Errorf("沒裝 OpenClaw 不該讓機器變紅，得到 %s（%s）", j.State, j.Reason)
	}
}

// 還沒量到 OpenClaw 時，不可以被講成已經量到而且沒裝。
func TestOpenClawObservationUnknownIsNotReportedAsAbsent(t *testing.T) {
	baseline := Derive(healthy())
	f := healthy()
	f.OpenClawObserved = false
	j := Derive(f)
	all := j.Reason
	for _, finding := range j.Findings {
		all += " " + finding.Message
	}
	if !strings.Contains(all, "還沒收到這台的 OpenClaw 觀測") {
		t.Errorf("未觀測時要明說還沒收到觀測，實際：%s", all)
	}
	if strings.Contains(all, "這台沒有裝 OpenClaw") {
		t.Errorf("未觀測時不可以斷言沒裝 OpenClaw，實際：%s", all)
	}
	if j.State != baseline.State {
		t.Errorf("未觀測 advisory 不該改變 state：得到 %s，原本 %s", j.State, baseline.State)
	}

	f = healthy()
	f.OpenClawPresent = false
	j = Derive(f)
	all = j.Reason
	for _, finding := range j.Findings {
		all += " " + finding.Message
	}
	if !strings.Contains(all, "這台沒有裝 OpenClaw") {
		t.Errorf("明確觀測到沒裝時必須維持既有判決，實際：%s", all)
	}
}

// 實測 sampleagent1：磁碟 97%，而它自己的 agent 連續喊了三次都沒人看到。
func TestDiskPressure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		free, tot  int64
		wantState  State
		wantSubstr string
	}{
		{"97% used", 17 << 30, 512 << 30, Degraded, "磁碟"},
		{"88% used", 60 << 30, 512 << 30, Degraded, "磁碟"},
		{"almost empty", 100 << 20, 512 << 30, Degraded, "寫入隨時會開始失敗"},
		{"plenty", 400 << 30, 512 << 30, Online, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := healthy()
			f.DiskFreeBytes, f.DiskTotalBytes = tc.free, tc.tot
			j := Derive(f)
			if j.State != tc.wantState {
				t.Fatalf("預期 %s，得到 %s（%s）", tc.wantState, j.State, j.Reason)
			}
			if tc.wantSubstr != "" && !strings.Contains(j.Reason, tc.wantSubstr) {
				t.Errorf("理由裡應該有 %q，實際：%s", tc.wantSubstr, j.Reason)
			}
		})
	}
}

// 一直在重開機的機器會偶爾送出成功的心跳。不能因此判它 Online。
//
// ⚠ 這個測試以前叫 TestCrashLoopIsNotOnline，斷言 boot_id 變 10 次 =「crash-loop」。
// 那個信念是錯的：boot_id 換值代表**機器**重開機，跟 agent process 崩潰迴圈
// 是兩件事。2026-09-03 事故裡機器連續開機 80 天、boot_id 一動也沒動，
// 而 agent 每 90 秒死一次。crash-loop 的測試改在
// TestAgentRestartLoopIsCaughtEvenThoughTheMachineNeverRebooted。
func TestRepeatedRebootsAreNotOnline(t *testing.T) {
	f := healthy()
	f.BootIDChanges1h = CrashLoopBootIDs
	j := Derive(f)
	if j.State != Degraded {
		t.Fatalf("一小時重開機 10 次不該是 Online，得到 %s", j.State)
	}
	if !strings.Contains(j.Reason, "重開機") {
		t.Errorf("實際：%s", j.Reason)
	}
}

// 一張短命的票剛過期，還在「等它自己續回來」的窗口裡：看得見，但不降級。
// 過了一個壽命還沒續：降級，而且文案要說出證據（看了多久、續了幾次）。
//
// ⚠ 這是 SPEC §4.3 的「不是快到期，是不再自動續了」。2026-09-05 的告警紀錄：
// samplehub1/claude 一天內「過期」兩輪、每輪一小時、然後自己好 —— 那不是故障，
// 是一張 8 小時的票在機器閒置時的正常樣子。對它亮紅燈，紅燈就會被靜音。
func TestExpiredCredentialGetsOneLifetimeOfGrace(t *testing.T) {
	watched := 3 * 24 * time.Hour

	// 過期 1.5 小時、壽命 8 小時、三天內續了 8 次 → 還在寬限內，不降級。
	exp := now.Add(-90 * time.Minute)
	f := healthy()
	f.Credentials = []CredFact{{Provider: "claude", Status: "expired", ExpiresAt: &exp,
		Lifetime: 8 * time.Hour, RefreshesSeen: 8, WatchedFor: watched}}
	j := Derive(f)
	if j.State != Online {
		t.Fatalf("剛過期、還在寬限內的票不該讓機器降級，得到 %s（%s）", j.State, j.Reason)
	}
	var found *Finding
	for i := range j.Findings {
		if j.Findings[i].Kind == "credential" {
			found = &j.Findings[i]
		}
	}
	if found == nil || !found.Advisory {
		t.Fatalf("寬限內的過期票一定要出現在 findings 裡（advisory），得到 %+v", j.Findings)
	}
	for _, want := range []string{"claude", "8.0 小時", "續了 8 次", "才算卡住"} {
		if !strings.Contains(found.Message, want) {
			t.Errorf("文案要講清楚為什麼還不算壞、什麼時候會算壞；缺 %q：%s", want, found.Message)
		}
	}

	// 同一張票過期 9 小時（> 一個壽命）→ 降級，證據要在句子裡。
	exp = now.Add(-9 * time.Hour)
	f = healthy()
	f.Credentials = []CredFact{{Provider: "claude", Status: "expired", ExpiresAt: &exp,
		Lifetime: 8 * time.Hour, RefreshesSeen: 8, WatchedFor: watched}}
	j = Derive(f)
	if j.State != Degraded {
		t.Fatalf("過了一個壽命還沒續回來就是卡住，應該降級，得到 %s", j.State)
	}
	if !strings.Contains(j.Reason, "續過 8 次") || !strings.Contains(j.Reason, "重新登入") {
		t.Errorf("降級的理由要帶上歷史證據：%s", j.Reason)
	}

	// 壽命 10 天的票（codex）過期 2 天：寬限上限是一天，不是十天。
	exp = now.Add(-48 * time.Hour)
	f = healthy()
	f.Credentials = []CredFact{{Provider: "codex", Status: "expired", ExpiresAt: &exp,
		Lifetime: 10 * 24 * time.Hour, RefreshesSeen: 0, WatchedFor: watched}}
	if j = Derive(f); j.State != Degraded {
		t.Fatalf("寬限上限一天：codex 過期兩天該降級，得到 %s", j.State)
	}
	if !strings.Contains(j.Reason, "沒續過一次") {
		t.Errorf("從沒見它續過，句子要說出來：%s", j.Reason)
	}

	// 算不出壽命的票沒有寬限：不知道它怎麼續，就不假設它會續。
	exp = now.Add(-10 * time.Minute)
	f = healthy()
	f.Credentials = []CredFact{{Provider: "grok", Status: "expired", ExpiresAt: &exp}}
	if j = Derive(f); j.State != Degraded {
		t.Fatalf("沒有壽命資訊的過期票不該有寬限，得到 %s", j.State)
	}

	// 寬限的邊界：剛好等於一個壽命不算在窗口內（跟其他門檻一樣，等於不算過）。
	exp = now.Add(-8 * time.Hour)
	c := CredFact{Provider: "claude", Status: "expired", ExpiresAt: &exp, Lifetime: 8 * time.Hour}
	if _, _, in := CredGrace(c, now); in {
		t.Error("過期剛好一個壽命，寬限應該已經結束")
	}
	c.Status = "configured"
	if _, _, in := CredGrace(c, now); in {
		t.Error("CredGrace 只對 expired 有意義")
	}
}

func TestTheExpiredCredentialSentenceNeverCallsAFutureExpiryThePast(t *testing.T) {
	tests := []struct {
		name         string
		lifetime     time.Duration
		expiresAt    time.Time
		want         string
		banned       []string
		wantState    State
		wantAdvisory bool
		wantSeverity int
	}{
		{
			name: "有壽命、已經過期", lifetime: 8 * time.Hour, expiresAt: now.Add(-90 * time.Minute),
			want: "的登入 1.5 小時前過期", banned: []string{"晚"}, wantState: Online, wantAdvisory: true, wantSeverity: 2,
		},
		// 這列鎖定分支條件是 overdue < 0，不是 overdue <= 0。
		{
			name: "有壽命、剛好在這一刻到期", lifetime: 8 * time.Hour, expiresAt: now,
			want: "的登入 0 秒前過期", banned: []string{"晚"}, wantState: Online, wantAdvisory: true, wantSeverity: 2,
		},
		{
			name: "有壽命、agent 的鐘快一點", lifetime: 8 * time.Hour, expiresAt: now.Add(80 * time.Second),
			want: "的登入的過期時間比現在晚 1 分鐘", banned: []string{"前過期", "才算卡住"}, wantState: Online, wantAdvisory: true, wantSeverity: 2,
		},
		{
			name: "有壽命、agent 的鐘快很多", lifetime: 8 * time.Hour, expiresAt: now.Add(10 * time.Hour),
			want: "的登入的過期時間比現在晚 10.0 小時", banned: []string{"前過期", "才算卡住"}, wantState: Online, wantAdvisory: true, wantSeverity: 2,
		},
		{
			name: "沒壽命、已經過期", lifetime: 0, expiresAt: now.Add(-90 * time.Minute),
			want: "的登入在 1.5 小時前就過期了", banned: []string{"晚"}, wantState: Degraded, wantAdvisory: false, wantSeverity: 3,
		},
		// 這列鎖定分支條件是 overdue < 0，不是 overdue <= 0。
		{
			name: "沒壽命、剛好在這一刻到期", lifetime: 0, expiresAt: now,
			want: "的登入在 0 秒前就過期了", banned: []string{"晚"}, wantState: Degraded, wantAdvisory: false, wantSeverity: 3,
		},
		// 這列釘住沒有壽命的未來過期時間也不得 promote；
		// 沒有它，攔截臂可以略過 Lifetime == 0，讓這種憑證繼續降級。
		{
			name: "沒壽命、agent 的鐘快一點", lifetime: 0, expiresAt: now.Add(80 * time.Second),
			want: "的登入的過期時間比現在晚 1 分鐘", banned: []string{"前就過期了"}, wantState: Online, wantAdvisory: true, wantSeverity: 2,
		},
		{
			name: "沒壽命、agent 的鐘快很多", lifetime: 0, expiresAt: now.Add(10 * time.Hour),
			want: "的登入的過期時間比現在晚 10.0 小時", banned: []string{"前就過期了"}, wantState: Online, wantAdvisory: true, wantSeverity: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := healthy()
			f.Credentials = []CredFact{{
				Provider: "claude", Status: "expired", ExpiresAt: &tt.expiresAt,
				Lifetime: tt.lifetime,
			}}
			j := Derive(f)
			if j.State != tt.wantState {
				t.Errorf("預期狀態 %s，得到 %s（%s）", tt.wantState, j.State, j.Reason)
			}

			var credential *Finding
			for i := range j.Findings {
				if j.Findings[i].Kind == "clock" {
					t.Errorf("憑證過期句不應掛在時鐘 finding 上：%s", j.Findings[i].Message)
				}
				if j.Findings[i].Kind == "credential" {
					credential = &j.Findings[i]
				}
			}
			if credential == nil {
				t.Fatal("找不到 credential finding")
			}
			if !strings.Contains(credential.Message, tt.want) {
				t.Errorf("credential finding 應包含 %q，實際：%s", tt.want, credential.Message)
			}
			for _, banned := range tt.banned {
				if strings.Contains(credential.Message, banned) {
					t.Errorf("credential finding 不應包含 %q，實際：%s", banned, credential.Message)
				}
			}
			if credential.Advisory != tt.wantAdvisory {
				t.Errorf("credential finding advisory=%v，預期 %v（%s）", credential.Advisory, tt.wantAdvisory, credential.Message)
			}
			if credential.Severity != tt.wantSeverity {
				t.Errorf("credential finding severity=%d，預期 %d（%s）", credential.Severity, tt.wantSeverity, credential.Message)
			}
		})
	}
}

// TestCredentialPeerEvidenceOnlyExplainsStuckExpiredLogin 守的是同儕證據的權限：
// 它只能替「已過寬限、原本就會亮紅燈」的登入補解釋，不能自己點燈，也不能
// 動到 configured。輸出只保留可觀測到的 provider 與 session 更新狀態。
func TestCredentialPeerEvidenceOnlyExplainsStuckExpiredLogin(t *testing.T) {
	recent := now.Add(-2 * time.Hour)
	peers := []CredPeer{
		{DisplayName: "sampleagent4", RefreshesSeen: 2, FileMTime: &recent},
		{DisplayName: "samplehub1", RefreshesSeen: 8, FileMTime: &recent},
	}

	exp := now.Add(-9 * time.Hour)
	f := healthy()
	f.Credentials = []CredFact{{
		Provider: "claude", Status: "expired", ExpiresAt: &exp,
		Lifetime: 8 * time.Hour, WatchedFor: 3 * 24 * time.Hour, Peers: peers,
	}}
	j := Derive(f)
	if j.State != Degraded {
		t.Fatalf("超過寬限的過期登入仍然應該降級，得到 %s", j.State)
	}
	for _, want := range []string{"samplehub1", "續了 8 次", "另外 1 台也在續", "provider 仍更新", "session 已停止續期"} {
		if !strings.Contains(j.Reason, want) {
			t.Errorf("跨機器證據少了 %q：%s", want, j.Reason)
		}
	}
	if strings.Contains(j.Reason, "sampleagent4 上自己續了") {
		t.Errorf("多台同儕只該點名最活躍的一台：%s", j.Reason)
	}

	// 還在寬限內時，同儕只是一份尚未用到的材料，不能提早把機器降級。
	exp = now.Add(-time.Hour)
	f = healthy()
	f.Credentials = []CredFact{{
		Provider: "claude", Status: "expired", ExpiresAt: &exp,
		Lifetime: 8 * time.Hour, WatchedFor: 3 * 24 * time.Hour, Peers: peers,
	}}
	j = Derive(f)
	if j.State != Online {
		t.Fatalf("寬限內的票不因同儕證據升級，得到 %s（%s）", j.State, j.Reason)
	}

	// configured 不是這條規則的輸入；即使塞入同儕，也不該長出 credential finding。
	f = healthy()
	f.Credentials = []CredFact{{Provider: "claude", Status: "configured", Peers: peers}}
	j = Derive(f)
	if j.State != Online {
		t.Fatalf("configured 不受同儕證據影響，得到 %s（%s）", j.State, j.Reason)
	}
	for _, finding := range j.Findings {
		if finding.Kind == "credential" {
			t.Errorf("configured 不該因同儕長出憑證 finding：%+v", finding)
		}
	}
}

func TestThePeerCredentialSentenceNeverCallsAFutureMtimeThePast(t *testing.T) {
	tests := []struct {
		name      string
		fileMTime time.Time
		want      string
		banned    []string
	}{
		{
			name:      "peer 的 mtime 在過去",
			fileMTime: now.Add(-2 * time.Hour),
			want:      "（最近 2.0 小時前）",
			banned:    []string{"晚"},
		},
		// 這列鎖定分支條件是 since < 0，不是 since <= 0。
		{
			name:      "peer 的 mtime 剛好是現在",
			fileMTime: now,
			want:      "（最近 0 秒前）",
			banned:    []string{"晚"},
		},
		{
			name:      "agent 的鐘快一點",
			fileMTime: now.Add(80 * time.Second),
			want:      "（更新時間比現在晚 1 分鐘）",
			banned:    []string{"最近 1 分鐘前"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp := now.Add(-9 * time.Hour)
			f := healthy()
			f.Credentials = []CredFact{{
				Provider: "claude", Status: "expired", ExpiresAt: &exp,
				Lifetime: 8 * time.Hour, WatchedFor: 3 * 24 * time.Hour,
				Peers: []CredPeer{{DisplayName: "samplehub1", RefreshesSeen: 8, FileMTime: &tt.fileMTime}},
			}}
			j := Derive(f)
			if !strings.Contains(j.Reason, tt.want) {
				t.Errorf("同儕憑證理由應包含 %q，實際：%s", tt.want, j.Reason)
			}
			for _, banned := range tt.banned {
				if strings.Contains(j.Reason, banned) {
					t.Errorf("同儕憑證理由不應包含 %q，實際：%s", banned, j.Reason)
				}
			}
		})
	}
}

func TestASingleActivePeerDoesNotSayAnotherZeroMachinesAreRenewing(t *testing.T) {
	recent := now.Add(-2 * time.Hour)
	tests := []struct {
		name   string
		peers  []CredPeer
		want   string
		banned []string
	}{
		// MostActiveCredPeer 回傳 ok 時 activePeers 至少是 1；> 1 是唯一擋住「另外 0 台也在續」的比較。
		// 把它放寬成 >= 1 時，這列必須紅。
		{
			name: "只有一台同儕在續",
			peers: []CredPeer{
				{DisplayName: "samplehub1", RefreshesSeen: 8, FileMTime: &recent},
			},
			want:   "在 samplehub1 上自己續了 8 次",
			banned: []string{"另外 0 台"},
		},
		{
			name: "兩台同儕在續",
			peers: []CredPeer{
				{DisplayName: "samplehub1", RefreshesSeen: 8, FileMTime: &recent},
				{DisplayName: "sampleagent4", RefreshesSeen: 2, FileMTime: &recent},
			},
			want:   "另外 1 台也在續",
			banned: []string{"另外 0 台", "另外 2 台"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp := now.Add(-9 * time.Hour)
			f := healthy()
			f.Credentials = []CredFact{{
				Provider: "claude", Status: "expired", ExpiresAt: &exp,
				Lifetime: 8 * time.Hour, WatchedFor: 3 * 24 * time.Hour, Peers: tt.peers,
			}}
			j := Derive(f)
			if !strings.Contains(j.Reason, tt.want) {
				t.Errorf("同儕憑證理由應包含 %q，實際：%s", tt.want, j.Reason)
			}
			for _, banned := range tt.banned {
				if strings.Contains(j.Reason, banned) {
					t.Errorf("同儕憑證理由不應包含 %q，實際：%s", banned, j.Reason)
				}
			}
		})
	}
}

// 憑證過期是 Degraded；unknown 不升級，但必須看得見。
//
// ⚠ 把 unknown 當綠燈是這個專案最想避免的事。
func TestCredentialStates(t *testing.T) {
	exp := now.Add(-13 * 24 * time.Hour)
	f := healthy()
	f.Credentials = []CredFact{{Provider: "claude", Status: "expired", ExpiresAt: &exp}}
	j := Derive(f)
	if j.State != Degraded || !strings.Contains(j.Reason, "claude") {
		t.Fatalf("過期憑證應該讓機器降級並點名，得到 %s（%s）", j.State, j.Reason)
	}

	f = healthy()
	f.Credentials = []CredFact{{Provider: "openclaw", Status: "unknown", Note: "沒有過期欄位"}}
	j = Derive(f)
	if j.State != Online {
		t.Errorf("unknown 不該升級成 Degraded，得到 %s", j.State)
	}
	var seen bool
	for _, fd := range j.Findings {
		if fd.Kind == "credential" && strings.Contains(fd.Message, "未知") {
			seen = true
		}
	}
	if !seen {
		t.Error("unknown 不升級，但一定要出現在 findings 裡 —— 不能被當成沒事")
	}
}

func TestUnrecognizedCredentialStatusIsVisible(t *testing.T) {
	credentialFindings := func(j Judgement) []Finding {
		t.Helper()
		var findings []Finding
		for _, finding := range j.Findings {
			if finding.Kind == "credential" {
				findings = append(findings, finding)
			}
		}
		return findings
	}

	t.Run("不認得的非空值", func(t *testing.T) {
		f := healthy()
		f.Credentials = []CredFact{{Provider: "claude", Status: "revoked"}}
		j := Derive(f)
		findings := credentialFindings(j)
		if len(findings) != 1 {
			t.Fatalf("revoked 應該長出一個 credential finding，得到 %+v", findings)
		}
		if !strings.Contains(findings[0].Message, "revoked") {
			t.Errorf("finding 沒有帶出不認得的值：%q", findings[0].Message)
		}
		if !findings[0].Advisory {
			t.Errorf("協定漂移必須標成 advisory：%+v", findings[0])
		}
		if j.State != Online {
			t.Errorf("不認得的狀態不該讓機器降級，得到 %s（%s）", j.State, j.Reason)
		}
	})

	t.Run("空字串是缺少狀態", func(t *testing.T) {
		f := healthy()
		f.Credentials = []CredFact{{Provider: "claude", Status: ""}}
		j := Derive(f)
		findings := credentialFindings(j)
		if len(findings) != 1 {
			t.Fatalf("空狀態應該長出一個 credential finding，得到 %+v", findings)
		}
		if findings[0].Message != "claude 的登入沒有回報狀態：把這台的 agent 升到跟 Hub 同版" {
			t.Errorf("空狀態的句子不對：%q", findings[0].Message)
		}
		if !findings[0].Advisory {
			t.Errorf("缺少狀態必須標成 advisory：%+v", findings[0])
		}
		if j.State != Online {
			t.Errorf("缺少狀態不該讓機器降級，得到 %s（%s）", j.State, j.Reason)
		}
	})

	t.Run("事實狀態不長 finding", func(t *testing.T) {
		for _, status := range []string{"absent", "configured"} {
			f := healthy()
			f.Credentials = []CredFact{{Provider: "claude", Status: status}}
			if findings := credentialFindings(Derive(f)); len(findings) != 0 {
				t.Errorf("%s 不該長出 credential finding：%+v", status, findings)
			}
		}
	})

	t.Run("unknown 維持 advisory 與原句", func(t *testing.T) {
		f := healthy()
		f.Credentials = []CredFact{{Provider: "openclaw", Status: "unknown", Note: "沒有過期欄位"}}
		j := Derive(f)
		findings := credentialFindings(j)
		if len(findings) != 1 {
			t.Fatalf("unknown 應該長出一個 credential finding，得到 %+v", findings)
		}
		if findings[0].Message != "openclaw 的登入狀態未知：沒有過期欄位" || !findings[0].Advisory {
			t.Errorf("unknown 行為改變：%+v", findings[0])
		}
		if j.State != Online {
			t.Errorf("unknown 不該讓機器降級，得到 %s（%s）", j.State, j.Reason)
		}
	})
}

// Derive／evaluateWorkload／judgeEvents 裡每一個能跟 Online 共存的
// add，都要在這張表裡有一列。
func TestOnlineNonAdvisoryFindingsAreWorkloadEvidence(t *testing.T) {
	expired := now.Add(-90 * time.Minute)
	modTime := now.Add(-time.Minute)
	cases := []struct {
		name   string
		change func(*Facts)
	}{
		{"process scan unavailable", func(f *Facts) {
			f.OpenClawRunning = false
			f.OpenClawProcessScan = "unavailable"
		}},
		{"process scan restricted", func(f *Facts) {
			f.OpenClawRunning = false
			f.OpenClawProcessScan = "restricted"
		}},
		{"process scan unrecognized", func(f *Facts) {
			f.OpenClawRunning = false
			f.OpenClawProcessScan = "future-value"
		}},
		{"artifact exists without modtime", func(f *Facts) {
			f.Artifacts = []ArtifactFact{{Unit: "backup", Artifact: "snapshot", Checked: true, Exists: true}}
		}},
		{"credential status missing", func(f *Facts) {
			f.Credentials = []CredFact{{Provider: "claude", Status: ""}}
		}},
		{"credential status unrecognized", func(f *Facts) {
			f.Credentials = []CredFact{{Provider: "claude", Status: "revoked"}}
		}},
		{"credential status unknown", func(f *Facts) {
			f.Credentials = []CredFact{{Provider: "claude", Status: "unknown"}}
		}},
		{"expired credential in grace", func(f *Facts) {
			f.Credentials = []CredFact{{Provider: "claude", Status: "expired", ExpiresAt: &expired, Lifetime: 8 * time.Hour}}
		}},
		{"clock skew", func(f *Facts) {
			f.ClockSkew = ClockSkewTolerance + time.Second
		}},
		{"operator restarted agent", func(f *Facts) {
			f.AgentRestarts1h = CrashLoopRestarts
			f.AgentUnitSeen = true
		}},
		{"undeclared event", func(f *Facts) {
			f.Artifacts = []ArtifactFact{{
				Unit: "backup", Artifact: "events.jsonl", Checked: true, Exists: true, ModTime: &modTime, MaxAge: time.Hour,
				Events: &EventFact{Measured: true, Window: time.Hour, Undeclared: []EventCount{{Type: "new_error", Count: 1}}, UndeclaredTotal: 1},
			}}
		}},
		{"malformed event line", func(f *Facts) {
			f.Artifacts = []ArtifactFact{{
				Unit: "backup", Artifact: "events.jsonl", Checked: true, Exists: true, ModTime: &modTime, MaxAge: time.Hour,
				Events: &EventFact{Measured: true, Window: time.Hour, Malformed: 1},
			}}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := healthy()
			tc.change(&f)
			j := Derive(f)
			if j.State != Online {
				t.Fatalf("這列必須跟 Online 共存，得到 %s（%s）", j.State, j.Reason)
			}
			for _, finding := range j.Findings {
				if !finding.Advisory && finding.Kind != "workload" {
					t.Errorf("Online 的 non-advisory finding 必須是 workload：%+v", finding)
				}
			}
		})
	}
}

// 身分衝突蓋過一切，而且兩列都要保留（保留由 store 負責，這裡只驗判決）。
func TestIdentityConflictWins(t *testing.T) {
	f := healthy()
	f.Conflict = true
	f.DiskFreeBytes = 1 << 20 // 同時磁碟也快滿
	j := Derive(f)
	if j.State != IdentityConflict {
		t.Fatalf("身分衝突應該蓋過其他狀態，得到 %s", j.State)
	}
}

// 時鐘歪掉不影響存活判斷（生死只用 Hub 的 received_at），但要講出來。
func TestClockSkewIsAdvisoryOnly(t *testing.T) {
	f := healthy()
	f.ClockSkew = 48 * time.Hour
	j := Derive(f)
	if j.State != Online {
		t.Errorf("時鐘歪掉不該改變存活判斷，得到 %s", j.State)
	}
	var found bool
	for _, fd := range j.Findings {
		if fd.Kind == "clock" && fd.Advisory {
			found = true
		}
	}
	if !found {
		t.Error("時鐘漂移要以 advisory 的形式出現")
	}
}

func TestSeverityOrdering(t *testing.T) {
	if IdentityConflict.Severity() <= NeverReported.Severity() ||
		NeverReported.Severity() <= Unreachable.Severity() ||
		Unreachable.Severity() <= Degraded.Severity() ||
		Degraded.Severity() <= Online.Severity() {
		t.Error("嚴重度排序不對，Dashboard 的排序會錯")
	}
}

// TestNoStraySpaceBeforeChinese 抓的是一類很小但很煩的排版 bug：
//
//	「已經 21.1 小時 沒有任何任務跑完」
//	「5 秒 前回報」
//
// humanDur 回傳的是「數字 + 空格 + 中文單位」，所以格式字串裡再加一個空格，
// 就會在兩個中文字之間留下一個空格。數字跟中文之間留空格是對的，
// 中文跟中文之間不是。
//
// ⚠ 這種 bug 不會讓任何測試變紅，也不會有人回報 —— 它只是讓每一句話
// 看起來都差一點。而這個產品賣的就是「一句人看得懂的話」。
func TestNoStraySpaceBeforeChinese(t *testing.T) {
	now := time.Now().UTC()
	expired := now.Add(-3 * time.Hour)
	soon := now.Add(2 * time.Hour)

	cases := []struct {
		name string
		f    Facts
	}{
		{"在線且最近有跑完", Facts{
			Now: now, EverCheckedIn: true,
			LastCheckinReceived: now.Add(-5 * time.Second),
			HasObservation:      true, LastObservation: now.Add(-time.Minute),
			OpenClawPresent: true, HasTaskSignal: true, LastTaskEnded: now.Add(-time.Hour),
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
		}},
		{"沉默失敗", Facts{
			Now: now, EverCheckedIn: true,
			LastCheckinReceived: now.Add(-5 * time.Second),
			HasObservation:      true, LastObservation: now.Add(-time.Minute),
			OpenClawPresent: true, OpenClawRunning: true, HasTaskSignal: true,
			LastTaskEnded: now.Add(-21 * time.Hour),
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
		}},
		{"觀測過期", Facts{
			Now: now, EverCheckedIn: true,
			LastCheckinReceived: now.Add(-5 * time.Second),
			HasObservation:      true, LastObservation: now.Add(-30 * time.Minute),
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
		}},
		{"憑證已過期", Facts{
			Now: now, EverCheckedIn: true,
			LastCheckinReceived: now.Add(-5 * time.Second),
			HasObservation:      true, LastObservation: now.Add(-time.Minute),
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
			Credentials: []CredFact{{Provider: "grok", Status: "expired", ExpiresAt: &expired}},
		}},
		{"憑證剛過期、寬限內", Facts{
			Now: now, EverCheckedIn: true,
			LastCheckinReceived: now.Add(-5 * time.Second),
			HasObservation:      true, LastObservation: now.Add(-time.Minute),
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
			Credentials: []CredFact{{Provider: "claude", Status: "expired", ExpiresAt: &expired,
				Lifetime: 8 * time.Hour, RefreshesSeen: 8, WatchedFor: 72 * time.Hour}},
		}},
		{"憑證過期、寬限已過", Facts{
			Now: now, EverCheckedIn: true,
			LastCheckinReceived: now.Add(-5 * time.Second),
			HasObservation:      true, LastObservation: now.Add(-time.Minute),
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
			Credentials: []CredFact{
				{Provider: "claude", Status: "expired", ExpiresAt: &expired,
					Lifetime: 2 * time.Hour, RefreshesSeen: 8, WatchedFor: 72 * time.Hour},
				{Provider: "grok", Status: "expired", ExpiresAt: &expired,
					Lifetime: 2 * time.Hour, RefreshesSeen: 0, WatchedFor: 72 * time.Hour},
			},
		}},
		{"憑證即將過期", Facts{
			Now: now, EverCheckedIn: true,
			LastCheckinReceived: now.Add(-5 * time.Second),
			HasObservation:      true, LastObservation: now.Add(-time.Minute),
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
			Credentials: []CredFact{{Provider: "claude", Status: "expires_soon", ExpiresAt: &soon}},
		}},
		{"失聯", Facts{
			Now: now, EverCheckedIn: true,
			LastCheckinReceived: now.Add(-30 * time.Minute),
			DiskFreeBytes:       50 << 30, DiskTotalBytes: 100 << 30,
		}},
	}

	// 中文單位後面直接跟著空格再接中文，就是這個 bug。
	bad := []string{"秒 ", "分鐘 ", "小時 ", "天 "}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			texts := []string{Derive(c.f).Reason}
			for _, fd := range Derive(c.f).Findings {
				texts = append(texts, fd.Message)
			}
			for _, txt := range texts {
				for _, b := range bad {
					i := strings.Index(txt, b)
					if i < 0 {
						continue
					}
					// 後面接的是中文字才算錯；接標點或 ASCII 是正常的。
					rest := []rune(txt[i+len(b):])
					if len(rest) > 0 && rest[0] >= 0x4e00 && rest[0] <= 0x9fff {
						t.Errorf("中文之間多了一個空格：%q", txt)
					}
				}
			}
		})
	}
}

// TestAgentRestartLoopIsCaughtEvenThoughTheMachineNeverRebooted 是 2026-09-03
// 那次事故的回歸測試。
//
// 當天全機隊每 90 秒被 systemd SIGABRT 一次（WatchdogSec 餵不到），
// samplehub1 的 restart counter 一小時內到 6，Hub 從頭到尾顯示綠燈。
//
// crash-loop 偵測「有做」：store 會數、state 有規則、machine.html 有欄位、
// state_test 也有測試 —— 每一個零件單獨看都是對的。
// 錯的是它數的東西：boot_id 是**機器**的開機 ID，process 重啟時它不會變。
// 機器已經連續開機 80 天，boot_id 一次都沒動過，所以那條規則一輩子不會觸發。
//
// ⚠ 這就是為什麼這個測試把 BootIDChanges1h 釘死在 0：事故當下它就是 0。
func TestAgentRestartLoopIsCaughtEvenThoughTheMachineNeverRebooted(t *testing.T) {
	f := healthy()
	f.BootIDChanges1h = 0 // 機器沒重開過，一次都沒有
	f.AgentRestarts1h = 24
	f.HasObservation = false // 每個 process 都活不到送出第一次觀測

	j := Derive(f)
	if j.State == Online {
		t.Fatalf("agent 一小時重啟 24 次還判 Online：%s", j.Reason)
	}
	if !strings.Contains(j.Reason, "重啟") {
		t.Errorf("主因沒說重啟，看的人不會知道要去看 systemd：%q", j.Reason)
	}
}

// 心跳送達本身不能當作「它好了」的證據 —— 那 24 次心跳正是那 24 次死亡的產物。
func TestFreshCheckinsDoNotExcuseARestartLoop(t *testing.T) {
	f := healthy()
	f.LastCheckinReceived = f.Now.Add(-3 * time.Second) // 三秒前才剛收到
	f.AgentRestarts1h = 24

	if j := Derive(f); j.State == Online {
		t.Errorf("剛收到心跳就判 Online，但那顆心跳是重啟送的：%s", j.Reason)
	}
}

func TestIsReportingAtUsesExactLivenessBoundary(t *testing.T) {
	received := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "default interval fallback", interval: 0, want: CheckinInterval},
		{name: "negative interval fallback", interval: -time.Second, want: CheckinInterval},
		{name: "custom interval", interval: 7 * time.Minute, want: 7 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Facts{
				EverCheckedIn:       true,
				LastCheckinReceived: received,
				CheckinInterval:     tc.interval,
			}
			deadline := received.Add(tc.want).Add(UnreachableGrace)
			if got := CheckinDeadline(f); !got.Equal(deadline) {
				t.Fatalf("CheckinDeadline=%s, want %s", got, deadline)
			}
			if !IsReportingAt(f, deadline) {
				t.Fatal("heartbeat stopped reporting at the boundary where Derive still considers it current")
			}
			if IsReportingAt(f, deadline.Add(time.Nanosecond)) {
				t.Fatal("heartbeat remained reporting after the authoritative liveness boundary")
			}
		})
	}
	f := Facts{EverCheckedIn: false, LastCheckinReceived: received}
	if IsReportingAt(f, received) {
		t.Fatal("never-reported machine counted as current heartbeat")
	}
}

// 判準：停掉 OpenClaw、Agent 還在 → Degraded「節點剛回報，OpenClaw process
// 不存在」，不是 Unreachable。
//
// ⚠ 這兩個顏色差一格，要做的事差很遠：Unreachable 會讓人去 ping 一台
// 完全正常的機器，Degraded 會讓人去 restart 一個真的停掉的 service。
func TestOpenClawStoppedIsDegradedNotUnreachable(t *testing.T) {
	f := healthy()
	f.OpenClawRunning = false
	j := Derive(f)

	if j.State == Unreachable {
		t.Fatal("判成 Unreachable —— 節點剛剛才回報過，機器沒事，" +
			"死掉的是它上面那個東西")
	}
	if j.State != Degraded {
		t.Fatalf("預期 Degraded，得到 %s", j.State)
	}
	if !strings.Contains(j.Reason, "OpenClaw process 不存在") {
		t.Errorf("理由沒講出是哪個 process 不見了，實際：%s", j.Reason)
	}
	if !strings.Contains(j.Reason, "剛回報") {
		t.Errorf("理由沒講出「機器本身是活的」，人會誤以為要去查連線：%s", j.Reason)
	}
}

// 反面：在跑的時候不准說它沒在跑。
// ⚠ 少了這一條，把 OpenClawRunning 當成恆 false 也會通過上面那個測試 ——
// 而那正是 2026-09-03 之前的真實狀況（node script 的 exe 對不上任何工具，
// 全機隊 running_pid 100% 是 NULL）。
func TestRunningOpenClawIsNotReportedAsStopped(t *testing.T) {
	j := Derive(healthy())
	if strings.Contains(j.Reason, "process 不存在") {
		t.Errorf("openclaw 在跑卻說它不在：%s", j.Reason)
	}
	for _, f := range j.Findings {
		if strings.Contains(f.Message, "process 不存在") {
			t.Errorf("發現清單裡說它不在：%+v", f)
		}
	}
}

// ⚠ agent 讀不到 /proc 的時候，OpenClawRunning 一樣是 false ——
// 但那時候我們知道的是「我不知道」，不是「它不在」。
// 拿前者去下後者的判決，等於用一個關於自己的事實去斷言世界。
func TestUnreadableProcIsNotEvidenceThatOpenClawIsStopped(t *testing.T) {
	f := healthy()
	f.OpenClawRunning = false
	f.OpenClawProcessScan = "unavailable"
	f.OpenClawRunReason = "process 偵測沒有跑到"
	j := Derive(f)

	if strings.Contains(j.Reason, "process 不存在") {
		t.Errorf("把「我讀不到」講成「它不在」了：%s", j.Reason)
	}
	// 但也不准就這樣算了 —— 偵測是關的這件事本身要看得到。
	var said bool
	for _, fi := range j.Findings {
		if strings.Contains(fi.Message, "偵測是關的") {
			said = true
		}
	}
	if !said {
		t.Errorf("偵測壞了卻一句話都沒說，畫面上會跟一切正常長得一樣：%+v", j.Findings)
	}
}

func TestRestrictedProcessScanIsNotEvidenceOfAbsence(t *testing.T) {
	f := healthy()
	f.OpenClawRunning = false
	f.OpenClawProcessScan = "restricted"
	f.OpenClawRunReason = "process 視野不完整"
	j := Derive(f)

	if strings.Contains(j.Reason, "process 不存在") {
		t.Errorf("把受限的掃描講成 process 不在了：%s", j.Reason)
	}
	var said bool
	for _, fi := range j.Findings {
		if strings.Contains(fi.Message, "偵測是關的") {
			said = true
		}
	}
	if !said {
		t.Errorf("掃描受限卻沒顯示偵測警告：%+v", j.Findings)
	}
}

func TestUnknownProcessScanIsNotEvidenceOfAbsence(t *testing.T) {
	const unknownMessage = "OpenClaw 的 process 偵測沒有回報這一輪的結果：把這台的 agent 升到跟 Hub 同版"

	t.Run("partial 是未知而非缺席", func(t *testing.T) {
		f := healthy()
		f.OpenClawRunning = false
		f.OpenClawProcessScan = "partial"
		f.OpenClawRunReason = ""
		j := Derive(f)

		if strings.Contains(j.Reason, "process 不存在") {
			t.Errorf("把未知的掃描狀態講成 process 不在了：%s", j.Reason)
		}
		var found bool
		for _, finding := range j.Findings {
			if finding.Message == unknownMessage {
				found = true
			}
		}
		if !found {
			t.Errorf("findings 沒有完整的新訊息 %q：%+v", unknownMessage, j.Findings)
		}
	})

	t.Run("complete 仍可判決缺席", func(t *testing.T) {
		f := healthy()
		f.OpenClawRunning = false
		f.OpenClawProcessScan = "complete"
		if j := Derive(f); !strings.Contains(j.Reason, "process 不存在") {
			t.Errorf("完整掃描沒有找到 process 卻未下缺席判決：%s", j.Reason)
		}
	})
}

// 反面：掃得到 process、只是沒對上 —— 那是關於世界的事實，要下判決。
func TestScannedButNotFoundIsEvidence(t *testing.T) {
	f := healthy()
	f.OpenClawRunning = false
	f.OpenClawProcessScan = "complete"
	f.OpenClawRunReason = "掃了 312 個 process，沒有一個對得上"
	if j := Derive(f); !strings.Contains(j.Reason, "process 不存在") {
		t.Errorf("掃過了、沒有它，這是真的證據，卻沒下判決：%s", j.Reason)
	}
}

// TestLegacyAgentProcessScanKeepsTodayBehaviour 釘住舊 agent 的相容路徑；
// 機隊升級完成、不再有空的 process_scan 後，這格測試要連同相容分支一起刪掉。
func TestLegacyAgentProcessScanKeepsTodayBehaviour(t *testing.T) {
	t.Run("讀不到 proc", func(t *testing.T) {
		f := healthy()
		f.OpenClawRunning = false
		f.OpenClawProcessScan = ""
		f.OpenClawRunReason = "讀不到 /proc，這台的 process 偵測整個是關的"
		if j := Derive(f); strings.Contains(j.Reason, "process 不存在") {
			t.Errorf("舊 agent 的 unknown 被判成 process 不在：%s", j.Reason)
		}
	})

	t.Run("掃描完成但沒對上", func(t *testing.T) {
		f := healthy()
		f.OpenClawRunning = false
		f.OpenClawProcessScan = ""
		f.OpenClawRunReason = "掃了 312 個 process，沒有一個對得上"
		if j := Derive(f); !strings.Contains(j.Reason, "process 不存在") {
			t.Errorf("舊 agent 掃完沒對上卻沒有下判決：%s", j.Reason)
		}
	})
}

// ⚠ 部署不是 crash-loop。
//
// 2026-09-03 全機隊部署了四輪，早報說五台裡有四台在 crash-loop。
// 那句話**技術上是對的**（process 確實一小時內重啟了 5 次），
// 而它會在第二次部署之後就被當成背景雜訊 —— 然後真的 crash-loop
// 發生時，那條告警已經沒有人在看了（§5.1 的失效模式）。
//
// 分得開的證據：systemd 的 NRestarts 只在**它自己**把死掉的 unit 拉起來時才加。
// 實測 `systemctl restart` 會把它歸零。
func TestDeployIsNotACrashLoop(t *testing.T) {
	f := healthy()
	f.AgentRestarts1h = 5
	f.AgentUnitSeen = true
	f.AgentNRestarts = 0 // systemd 一次都沒有替它重新拉起來

	j := Derive(f)
	if j.State != Online {
		t.Fatalf("有人按了五次 restart 就被判成 %s：%s", j.State, j.Reason)
	}
	var said bool
	for _, fi := range j.Findings {
		if strings.Contains(fi.Message, "一小時內重啟 5 次") && strings.Contains(fi.Message, "systemd restart count=0") {
			said = true
		}
	}
	if !said {
		t.Errorf("重啟五次這件事完全沒講 —— 那又變成另一種安靜：%+v", j.Findings)
	}
}

// 反面：systemd 真的在替它收屍，那就是 crash-loop。
func TestSystemdPickingUpTheBodyIsACrashLoop(t *testing.T) {
	f := healthy()
	f.AgentRestarts1h = 5
	f.AgentUnitSeen = true
	f.AgentNRestarts = 5

	j := Derive(f)
	if !strings.Contains(j.Reason, "crash-loop") {
		t.Fatalf("systemd 收了五次屍卻不算 crash-loop：%s", j.Reason)
	}
	if !strings.Contains(j.Reason, "systemd 已經替它重新拉起來 5 次") {
		t.Errorf("沒把那個數字講出來，人沒辦法判斷嚴重程度：%s", j.Reason)
	}
}

// ⚠ 收不到 agent 自己的 unit 觀測時，NRestarts 會是 0 ——
// 而那個 0 的意思是「不知道」，不是「systemd 沒重啟過它」。
// 舊版 agent 不觀測自己的 unit，那時候不准因為看不到就說它是部署。
func TestUnknownNRestartsStillCountsAsCrashLoop(t *testing.T) {
	f := healthy()
	f.AgentRestarts1h = 5
	f.AgentUnitSeen = false
	f.AgentNRestarts = 0

	if j := Derive(f); !strings.Contains(j.Reason, "crash-loop") {
		t.Fatalf("看不到 systemd 的計數就當作沒事：%s", j.Reason)
	}
}

// ⚠⚠ 這一支守的是一個**字**，而那個字是這個產品最核心的宣稱。
//
// 2026-09-05 之前，「在名冊上但從來沒 check-in 過」的狀態叫 `Dead`。
// 那個字斷言的是**那台機器**，但這個判定唯一的輸入是 `!EverCheckedIn` ——
// 一件關於 **clawctl 自己**的事實。其他每一個狀態都是從機器真的送來的
// 資料推出來的；只有這一個是從**資料的缺席**推出來的。
//
// 實例（PHASE1、FLEET-INVENTORY 都記著）：sampleagent1 從來沒 check-in 過，
// 所以它一直掛著 Dead。而 tailscale 說它 Online、direct handshake 18ms、
// TCP/22 交握得起來、samplehub1 上的 sampleagent1-management 每 30 分鐘寫一次
// heartbeat OK 至今 1,485 次。它只是**刻意**沒有裝 clawctl-agent。
//
// 這條規則是這個專案那句鐵律的反面：
// 「一個空的、乾淨的、沒有壞消息的答案，第一個要懷疑的是你有沒有問對地方。」
// 這次是一個**壞消息**的答案，而它一樣來自問錯地方。
func TestNeverReportedDoesNotClaimTheMachineIsDead(t *testing.T) {
	j := Derive(Facts{Now: now, EverCheckedIn: false})

	if j.State != NeverReported {
		t.Fatalf("state = %q，要 NeverReported", j.State)
	}
	if j.State.Severity() != 4 {
		t.Errorf("嚴重度 = %d，要 4 —— 改名不是降級，"+
			"「我對這台一無所知」仍然是最該處理的事情之一", j.State.Severity())
	}

	// ⚠ 狀態的字面值會直接變成 clawctl_machine_state{state=...} 的 label
	// 跟畫面上那顆燈的名字。任何斷言機器死活的字都不准出現在這裡。
	for _, banned := range []string{"Dead", "dead", "Down", "Offline", "Failed"} {
		if strings.Contains(string(j.State), banned) {
			t.Errorf("狀態名裡有 %q —— 這個判定沒有對那台機器送出過任何一個封包，"+
				"它宣稱不了機器的死活", banned)
		}
	}

	if !strings.Contains(j.Reason, "從未報到") || !strings.Contains(j.Reason, "管理分母") {
		t.Errorf("理由沒有包含觀測狀態與管理範圍：%s", j.Reason)
	}
	for _, banned := range []string{"死了", "掛了", "當機"} {
		if strings.Contains(j.Reason, banned) {
			t.Errorf("理由裡出現未量測的狀態 %q", banned)
		}
	}
}

func TestTheDurationVocabularySaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "零", in: 0, want: "0 秒"},
		{name: "不到一分鐘", in: 45 * time.Second, want: "45 秒"},
		{name: "差一秒就一分鐘", in: 59 * time.Second, want: "59 秒"},
		{name: "剛好一分鐘", in: time.Minute, want: "1 分鐘"},
		{name: "一分半只講一分鐘", in: 90 * time.Second, want: "1 分鐘"},
		{name: "差一秒就一小時", in: 59*time.Minute + 59*time.Second, want: "59 分鐘"},
		{name: "剛好一小時", in: time.Hour, want: "1.0 小時"},
		{name: "小時帶半", in: 11*time.Hour + 30*time.Minute, want: "11.5 小時"},
		{name: "快滿兩天", in: 47 * time.Hour, want: "47.0 小時"},
		{name: "剛好兩天", in: 48 * time.Hour, want: "2 天"},
		{name: "九十天", in: 90 * 24 * time.Hour, want: "90 天"},
		// 這支只講長度，方向是呼叫端的責任（見 internal/web 的 ago）。
		{name: "負的時間只講長度不講方向", in: -5 * time.Minute, want: "5 分鐘"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HumanDur(tt.in); got != tt.want {
				t.Errorf("HumanDur(%s) = %q，要 %q", tt.in, got, tt.want)
			}
		})
	}
}
