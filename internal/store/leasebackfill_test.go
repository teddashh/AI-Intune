package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

// 舊版 Hub 的終態 UPDATE 沒清租約欄位，帳本上留著「終態卻仍有持有者」的列。
// 新版每一條終態路徑都在同一個 UPDATE 清掉，但既有的列不會自己好；
// 這裡證明 Open 會把它們補正，而且不碰仍在進行中的單。
func TestOpenClearsLeasesLeftOnTerminalJobsByOlderBinaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := deployTestNow
	machineID := mustEnroll(t, s, "old-binary", now)
	desiredID, rev, err := s.CreateDesiredState("machine", machineID, "openclaw", "openclaw", `{}`, "回填測試")
	if err != nil {
		t.Fatalf("建立期望狀態失敗：%v", err)
	}
	newJob := func() string {
		jobID, err := s.CreateJob(machineID, desiredID, rev, NewJob{ArtifactDigest: "sha256:a", ExecutionTimeout: 30})
		if err != nil {
			t.Fatalf("建立工作單失敗：%v", err)
		}
		if _, err := s.ClaimJob(jobID, machineID, now, 2*time.Minute); err != nil {
			t.Fatalf("領單失敗：%v", err)
		}
		return jobID
	}
	stale, live := newJob(), newJob()
	// 模擬舊版 Hub：直接寫終態，租約欄位原封不動。
	if _, err := s.db.Exec(`UPDATE jobs SET state = ?, terminal_at = ? WHERE job_id = ?`,
		deploy.Succeeded, fmtTime(now), stale); err != nil {
		t.Fatalf("模擬舊版終態失敗：%v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.Job(stale)
	if err != nil {
		t.Fatalf("讀終態單失敗：%v", err)
	}
	if got.State != deploy.Succeeded || got.LeaseToken != "" || got.LeaseExpiresAt != nil {
		t.Fatalf("重開後終態單仍留著租約：%+v", got)
	}
	kept, err := s.Job(live)
	if err != nil {
		t.Fatalf("讀進行中的單失敗：%v", err)
	}
	if kept.State != deploy.Claimed || kept.LeaseToken == "" || kept.LeaseExpiresAt == nil {
		t.Fatalf("回填動到了進行中的單：%+v", kept)
	}
}
