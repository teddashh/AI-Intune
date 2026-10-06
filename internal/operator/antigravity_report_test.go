package operator

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestInstallReportJoinsProbeToolToPackage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	fleet := installFleet{store: st, now: now, ids: map[string]string{}}
	enroll := func(key, name string) {
		t.Helper()
		id, token, err := st.CreateEnrollTokenFor(name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token, Hostname: name,
			UnixUser: "report", OS: "linux", Arch: "amd64",
		}, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		fleet.ids[key] = id
	}
	enroll("matched", "matched-box")
	enroll("unmapped", "unmapped-box")
	observe := func(key string, tools []model.CLITool) {
		t.Helper()
		if err := st.RecordObservation(fleet.ids[key], model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-time.Minute), CLITools: tools,
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	tool := func(name, version string) model.CLITool {
		path := "/usr/local/bin/" + name
		return model.CLITool{
			Name: name, Present: true, OnPath: true, PresentEvidence: "path", VersionReported: version,
			Path: path, RealPath: path,
		}
	}
	observe("matched", []model.CLITool{tool("claude", "2.1.195"), tool("agy", "1.2.14")})
	observe("unmapped", []model.CLITool{tool("gemini", "2.1.195")})
	at := now.Add(-time.Hour)
	fleet.assign(t, "machine", fleet.ids["matched"], "claude-code", "claude-code", `{"kind":"claude-code","version":"2.1.195"}`, at)
	fleet.assign(t, "machine", fleet.ids["matched"], "antigravity", "antigravity", `{"kind":"antigravity","version":"1.2.14"}`, at)
	fleet.assign(t, "machine", fleet.ids["unmapped"], "claude-code", "claude-code", `{"kind":"claude-code","version":"2.1.195"}`, at)

	report := installReportOf(t, fleet)
	claude := installRowFor(t, installResourceNamed(t, report, "claude-code"), "matched-box")
	if claude.State != InstallMatches || claude.Observed != "2.1.195" {
		t.Fatalf("claude row=%+v", claude)
	}
	agy := installRowFor(t, installResourceNamed(t, report, "antigravity"), "matched-box")
	if agy.State != InstallMatches || agy.Observed != "1.2.14" {
		t.Fatalf("agy row=%+v", agy)
	}
	gemini := installRowFor(t, installResourceNamed(t, report, "claude-code"), "unmapped-box")
	if gemini.State != InstallUnobserved {
		t.Fatalf("unmapped gemini matched claude-code: %+v", gemini)
	}
}

func TestProfileSeenVersionsJoinsProbeToolToPackage(t *testing.T) {
	tool := func(name, version string) model.CLITool {
		path := "/usr/local/bin/" + name
		return model.CLITool{
			Name: name, Present: true, OnPath: true, PresentEvidence: "path", VersionReported: version,
			Path: path, RealPath: path,
		}
	}
	seen := profileSeenVersions([]store.FleetToolRow{
		{MachineID: "m1", CLITool: tool("claude", "2.1.195")},
		{MachineID: "m2", CLITool: tool("agy", "1.2.14")},
		{MachineID: "m3", CLITool: tool("gemini", "2.1.195")},
	}, map[string]bool{})
	if seen[profileVersionKey{packageID: "claude-code", version: "2.1.195"}].on != 1 {
		t.Fatalf("claude observation was not stored on claude-code: %+v", seen)
	}
	if seen[profileVersionKey{packageID: "antigravity", version: "1.2.14"}].on != 1 {
		t.Fatalf("agy observation was not stored on antigravity: %+v", seen)
	}
	if seen[profileVersionKey{packageID: "claude", version: "2.1.195"}].on != 0 ||
		seen[profileVersionKey{packageID: "agy", version: "1.2.14"}].on != 0 ||
		seen[profileVersionKey{packageID: "gemini", version: "2.1.195"}].on != 1 {
		t.Fatalf("probe names were rewritten incorrectly: %+v", seen)
	}
}
