package state

import (
	"strings"
	"testing"
	"time"
)

// baseFacts 是一台各方面都正常的機器，好讓測試只講產出物那一件事。
func baseFacts(now time.Time) Facts {
	return Facts{
		Now:           now,
		EverCheckedIn: true, LastCheckinReceived: now.Add(-time.Minute),
		CheckinInterval: 2 * time.Minute,
		HasObservation:  true, LastObservation: now.Add(-2 * time.Minute),
		OpenClawPresent: true, OpenClawRunning: true,
		HasTaskSignal: true, LastTaskEnded: now.Add(-10 * time.Minute),
	}
}

func findingsOf(j Judgement, kind string) []Finding {
	var out []Finding
	for _, f := range j.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// 這是 sampleagent2 的真實數字：unit 從 2026-06-15 起 active、重啟 0 次，
// 而它宣告要產出的 machine-mission.md 也停在 2026-06-15 —— 81 天沒動。
func TestAStaleArtifactMakesTheMachineDegraded(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	mt := now.Add(-81 * 24 * time.Hour)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{{
		Unit: "openclaw-watcher.service", Artifact: "/home/ubuntu/.openclaw/workspace/baseline/machine-mission.md",
		Why: "watcher 每一輪應該重寫這個檔案", MaxAge: 15 * time.Minute,
		Checked: true, Exists: true, ModTime: &mt,
	}}
	j := Derive(f)
	if j.State != Degraded {
		t.Fatalf("產出物 81 天沒更新，狀態卻是 %s", j.State)
	}
	fs := findingsOf(j, "workload")
	var got string
	for _, x := range fs {
		if strings.Contains(x.Message, "machine-mission.md") {
			got = x.Message
		}
	}
	if got == "" {
		t.Fatalf("沒有產出對應的 finding：%+v", fs)
	}
	// 理由必須原封不動出現 —— 半年後看到這條紅燈的人需要它。
	if !strings.Contains(got, "watcher 每一輪應該重寫這個檔案") {
		t.Fatalf("finding 沒有帶上宣告時寫的理由：%s", got)
	}
	// ⚠ 而且必須能流進早報：report.go 只收 workload 且非 advisory 的。
	for _, x := range fs {
		if strings.Contains(x.Message, "machine-mission.md") && x.Advisory {
			t.Fatal("這條被標成 advisory，早報就看不到它了")
		}
	}
}

// ⚠⚠ 這一支守的是最危險的那個假綠燈：
// 宣告了、但從來沒量到，**不可以**長得跟「量過而且通過」一樣。
func TestDeclaredButNeverMeasuredIsNotAPass(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{{
		Unit: "openclaw-watcher.service", Artifact: "/tmp/x.md",
		Why: "應該每輪重寫", MaxAge: 15 * time.Minute,
		Checked: false,
	}}
	j := Derive(f)

	var seen bool
	for _, x := range findingsOf(j, "workload") {
		if strings.Contains(x.Message, "尚無量測") {
			seen = true
		}
	}
	if !seen {
		t.Fatal("宣告了卻沒量到，畫面上完全看不到 —— 那看起來就像通過了")
	}
	// 但它不該把一台剛裝好的機器變紅。
	if j.State == Degraded {
		t.Fatal("還沒量到就降級了 —— 剛裝好的機器會無故變紅")
	}
}

// 「看不到這個檔案」不是「它沒產出」。權限問題被畫成產出失敗，人會去修錯的東西。
func TestCannotSeeIsNotTheSameAsDidNotProduce(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{{
		Unit: "u.service", Artifact: "/root/secret.md", Why: "w", MaxAge: time.Minute,
		Checked: true, Err: "permission denied",
	}}
	j := Derive(f)
	var msg string
	for _, x := range findingsOf(j, "workload") {
		if strings.Contains(x.Message, "secret.md") {
			msg = x.Message
		}
	}
	if !strings.Contains(msg, "看不到") {
		t.Fatalf("權限錯誤沒有被講成「看不到」：%s", msg)
	}
	if strings.Contains(msg, "不存在") {
		t.Fatalf("把「看不到」講成了「不存在」：%s", msg)
	}
}

// 檔案不存在是一個答案，而且是最嚴重的那個。
func TestAMissingArtifactIsReportedAsMissing(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{{
		Unit: "u.service", Artifact: "/tmp/never.md", Why: "應該要有", MaxAge: time.Minute,
		Checked: true, Exists: false,
	}}
	j := Derive(f)
	if j.State != Degraded {
		t.Fatalf("宣告的產出物根本不存在，狀態卻是 %s", j.State)
	}
}

// ⚠ 新鮮的產出物只能說「它有在寫」，不能說「它做對了」。
// 一個把錯誤內容寫進檔案的服務，mtime 一樣是新的。
func TestAFreshArtifactDoesNotClaimCorrectness(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	mt := now.Add(-time.Minute)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{{
		Unit: "u.service", Artifact: "/tmp/fresh.md", Why: "w", MaxAge: 15 * time.Minute,
		Checked: true, Exists: true, ModTime: &mt,
	}}
	j := Derive(f)
	if j.State == Degraded {
		t.Fatal("產出物是新的，卻降級了")
	}
	var msg string
	for _, x := range findingsOf(j, "workload") {
		if strings.Contains(x.Message, "fresh.md") {
			msg = x.Message
		}
	}
	if !strings.Contains(msg, "在 1 分鐘前更新") {
		t.Fatalf("新鮮產出物應顯示量測結果：%s", msg)
	}
	for _, claim := range []string{"成功", "正確", "通過"} {
		if strings.Contains(msg, claim) {
			t.Fatalf("mtime finding 包含成果判決 %q：%s", claim, msg)
		}
	}
}

func TestTheArtifactFreshnessSentenceNeverCallsAFutureMtimeThePast(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		modTime time.Time
		want    string
		banned  []string
	}{
		{
			name:    "mtime 在過去",
			modTime: now.Add(-time.Minute),
			want:    "在 1 分鐘前更新",
			banned:  []string{"晚", "後"},
		},
		// 這列鎖定分支條件是 age < 0，不是 age <= 0。
		{
			name:    "mtime 剛好是現在",
			modTime: now,
			want:    "在 0 秒前更新",
			banned:  []string{"晚", "後"},
		},
		{
			name:    "agent 的鐘快一點",
			modTime: now.Add(80 * time.Second),
			want:    "的更新時間比現在晚 1 分鐘",
			banned:  []string{"在 1 分鐘前更新", "後"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := baseFacts(now)
			f.Artifacts = []ArtifactFact{{
				Unit: "u.service", Artifact: "/tmp/fresh.md", Why: "w", MaxAge: 15 * time.Minute,
				Checked: true, Exists: true, ModTime: &tt.modTime,
			}}
			j := Derive(f)
			if j.State != Online {
				t.Errorf("產出物 mtime 不該降級，預期 Online，得到 %s（%s）", j.State, j.Reason)
			}

			var workload *Finding
			for i := range j.Findings {
				if j.Findings[i].Kind == "workload" && strings.Contains(j.Findings[i].Message, "fresh.md") {
					workload = &j.Findings[i]
					break
				}
			}
			if workload == nil {
				t.Fatal("找不到產出物新鮮度的 workload finding")
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

// 沒有人宣告任何期望時，不准無中生有任何 workload finding。
func TestNoDeclarationsProducesNoArtifactFindings(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	j := Derive(baseFacts(now))
	for _, x := range findingsOf(j, "workload") {
		if strings.Contains(x.Message, "產出物") {
			t.Fatalf("沒有宣告卻冒出產出物的 finding：%s", x.Message)
		}
	}
}
