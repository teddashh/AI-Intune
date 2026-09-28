package probe

import (
	"context"
	"os"
	"testing"
)

// 打真的 /proc，印出來給人看。它不斷言任何結果 —— 人自己看。
//
// ⚠ 從 shell 跑不算數。這個偵測器壞掉的時候，從 shell 跑是**對的**，
// 只有從 systemd unit 裡跑才是錯的（ptrace_scope=1，見 scanProcesses）。
// 一份只在自己的殼裡驗證過的證據，就是「自證不算數」。
//
//	go test -c -o /tmp/probetest ./internal/probe/
//	systemd-run --user --quiet --wait --pipe --same-dir --setenv=CLAWCTL_LIVE=1 \
//	  --property=NoNewPrivileges=true --property=ProtectSystem=strict \
//	  --property=ProtectHome=read-only --property=RestrictSUIDSGID=true \
//	  --property=LockPersonality=true \
//	  /tmp/probetest -test.run TestLiveProcessMatch -test.v
//
// 2026-09-03 的答案：沙箱內 554 個 process、0 個讀得到 exe，
// 而 claude / codex / openclaw 三支都還是抓得到。修好之前是 0 支。
func TestLiveProcessMatch(t *testing.T) {
	if os.Getenv("CLAWCTL_LIVE") == "" {
		t.Skip("設 CLAWCTL_LIVE=1 才跑")
	}
	procs, processScan := scanProcesses()
	var withExe int
	for _, p := range procs {
		if p.exe != "" {
			withExe++
		}
	}
	t.Logf("掃到 %d 個 process，其中 %d 個讀得到 exe", len(procs), withExe)
	for _, tool := range cliTools(context.Background(), procs, processScan) {
		t.Logf("%-10s present=%v pid=%d script=%s reason=%s",
			tool.Name, tool.Present, tool.RunningPID, tool.RunningScript, tool.RunningReason)
	}
}
