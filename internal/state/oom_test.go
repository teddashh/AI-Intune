package state

import "testing"

func TestOOMFindingPromotesAttention(t *testing.T) {
	f := healthy()
	f.RecentOOMKills = 1
	j := Derive(f)
	if j.State != Degraded {
		t.Fatalf("state %s", j.State)
	}
	found := false
	for _, finding := range j.Findings {
		if finding.Kind == "memory" && !finding.Advisory {
			found = true
		}
	}
	if !found {
		t.Fatal("missing actionable memory finding")
	}
	f.AgentRestarts1h = CrashLoopRestarts
	if severe := Derive(f); severe.Reason == j.Reason {
		t.Fatal("OOM replaced higher-severity crash-loop reason")
	}
	f.AgentRestarts1h = 0
	f.RecentOOMKills = 0
	if Derive(f).State != Online {
		t.Fatal("expired evidence retained attention")
	}
}
