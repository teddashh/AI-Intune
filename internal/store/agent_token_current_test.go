package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestAgentTokenCurrentTracksRotationAndRetirement(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, enrollToken, err := st.CreateEnrollTokenFor("token-current", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineID, agentToken, err := st.RedeemEnrollToken(enrollToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, Hostname: "token-current", UnixUser: "tester", OS: "linux", Arch: "amd64",
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	check := func(name, machine, bearer string, want bool) {
		t.Helper()
		got, err := st.AgentTokenCurrent(machine, bearer)
		if err != nil || got != want {
			t.Fatalf("%s: AgentTokenCurrent=%v err=%v, want %v", name, got, err, want)
		}
	}
	check("current", machineID, agentToken, true)
	check("wrong bearer", machineID, agentToken+"x", false)
	check("empty bearer", machineID, "", false)
	check("unknown machine", "no-such-machine", agentToken, false)
	if _, err := st.DB().Exec(`UPDATE machine_registry SET agent_token_hash=? WHERE machine_id=?`,
		hashToken("rotated"), machineID); err != nil {
		t.Fatal(err)
	}
	check("old bearer after rotation", machineID, agentToken, false)
	check("new bearer after rotation", machineID, "rotated", true)
	if err := st.RetireMachine(machineID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	check("retired", machineID, "rotated", false)
}
