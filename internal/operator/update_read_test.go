package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestUpdatesComposesSafeChannelAndArtifactPosture(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	record := writeArtifactReadFixture(t, dir, "2026.9.8", []byte("update artifact"), true)
	if err := prepareDeploymentObservationPolicy(st, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	observeDeploymentMachine(t, st, "machine-canary", "canary-box", "canary", "24.16.0", "2026.9.7", now.Add(-time.Minute))
	observeDeploymentMachine(t, st, "machine-stable", "stable-box", "stable", "24.16.0", "2026.9.6", now.Add(-time.Minute))
	if _, err := st.DB().Exec(`UPDATE machine_registry SET hostname=?,unix_user=?,tailscale_ip=?,notes=? WHERE machine_id=?`,
		"RAW_PRIVATE_HOST", "RAW_PRIVATE_USER", "100.64.0.1", "RAW_PRIVATE_NOTE", "machine-canary"); err != nil {
		t.Fatal(err)
	}

	result, err := NewWithArtifacts(st, dir).Updates(UpdateReadRequest{Channel: "canary", Limit: 10}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != UpdateReadSchemaVersion || result.Consistency != UpdateReadConsistencyLive ||
		!result.EvaluatedAt.Equal(now) || result.Artifacts.Total != 1 || len(result.Channels) != 1 {
		t.Fatalf("result envelope=%+v", result)
	}
	channel := result.Channels[0]
	if channel.Name != "canary" || channel.MemberCount != 1 || len(channel.Members) != 1 ||
		channel.Members[0].LastObservedVersion == nil || *channel.Members[0].LastObservedVersion != "2026.9.7" ||
		channel.Members[0].LastObservationReceivedAt == nil ||
		!channel.Members[0].LastObservationReceivedAt.Equal(now.Add(-time.Minute)) ||
		channel.LatestDeployment != nil || len(channel.Previews) != 1 {
		t.Fatalf("channel=%+v", channel)
	}
	preview := channel.Previews[0]
	if preview.Artifact.ArtifactID != record.SHA256 || preview.Artifact.Status != ArtifactAvailableUnverified ||
		preview.Artifact.VerifiedAt != nil || preview.Plan == nil || preview.Plan.Impact != 1 ||
		preview.Promotion != nil || len(preview.Blockers) != 0 {
		t.Fatalf("preview=%+v", preview)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"tarball_url", "fetched_by", "RAW_URL_SECRET", "RAW_FETCHED_BY_SECRET", dir,
		"RAW_PRIVATE_HOST", "RAW_PRIVATE_USER", "100.64.0.1", "RAW_PRIVATE_NOTE",
	} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("updates projection disclosed %q: %s", forbidden, raw)
		}
	}
}

func TestUpdatesMarksUnavailableArtifactsAndStablePromotion(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	missing := writeArtifactReadFixture(t, dir, "2026.9.9", []byte("not published"), false)
	malformedID := strings.Repeat("f", 64)
	if malformedID == missing.SHA256 {
		t.Fatal("fixture digest collision")
	}
	if err := os.WriteFile(filepath.Join(dir, malformedID+".json"), []byte(`{"tarball_url":"RAW_MALFORMED_URL"`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := NewWithArtifacts(st, dir).Updates(UpdateReadRequest{Channel: "stable"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Channels) != 1 || len(result.Channels[0].Previews) != 2 {
		t.Fatalf("result=%+v", result)
	}
	for _, preview := range result.Channels[0].Previews {
		if preview.Plan != nil || preview.Promotion != nil || len(preview.Blockers) != 1 || preview.Blockers[0] != "artifact_not_available" {
			t.Fatalf("unavailable preview=%+v", preview)
		}
	}
	raw, _ := json.Marshal(result)
	if bytes.Contains(raw, []byte("RAW_MALFORMED_URL")) {
		t.Fatalf("malformed sidecar secret leaked: %s", raw)
	}
}

func TestUpdatesRejectsInvalidChannelAndArtifactCursor(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	svc := NewWithArtifacts(st, dir)
	now := time.Now().UTC()
	for _, request := range []UpdateReadRequest{
		{Channel: "beta"},
		{Status: ArtifactReady},
		{Limit: MaxArtifactReadLimit + 1},
		{Cursor: "not-a-cursor"},
	} {
		if _, err := svc.Updates(request, now); !errors.Is(err, ErrInvalidUpdateRead) {
			t.Fatalf("request=%+v err=%v", request, err)
		}
	}
}

func TestUpdatesTreatsUntrustedObservedVersionsAsUnknownBeforePlanning(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	writeArtifactReadFixture(t, dir, "2026.9.8", []byte("first update artifact"), true)
	writeArtifactReadFixture(t, dir, "2026.9.9", []byte("second update artifact"), true)
	if err := prepareDeploymentObservationPolicy(st, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Both fields are agent-controlled. The dotted value is deliberately large:
	// passing it into rollout's semver parser once per artifact would amplify
	// allocations, while reflecting it would violate the public DTO contract.
	untrusted := strings.Repeat("24.", 2048)
	observeDeploymentMachine(t, st, "machine-hostile", "hostile-box", "canary",
		untrusted, untrusted, now.Add(-time.Minute))

	result, err := NewWithArtifacts(st, dir).Updates(UpdateReadRequest{Channel: "canary", Limit: 10}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Channels) != 1 || len(result.Channels[0].Members) != 1 ||
		result.Channels[0].Members[0].LastObservedVersion != nil ||
		result.Channels[0].Members[0].LastObservationReceivedAt == nil ||
		len(result.Channels[0].Previews) != 2 {
		t.Fatalf("channel=%+v", result.Channels)
	}
	for _, preview := range result.Channels[0].Previews {
		if preview.Plan == nil || preview.Plan.UnknownNodes != 1 || preview.Plan.Impact != 0 {
			t.Fatalf("untrusted node version did not fail closed: %+v", preview)
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(untrusted)) {
		t.Fatal("updates DTO reflected an untrusted observed version")
	}
}

func TestUpdatesUsesLastObservationReceivedByEvaluationInstantForMemberAndPlan(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	writeArtifactReadFixture(t, dir, "2026.9.8", []byte("as-of update artifact"), true)
	if err := prepareDeploymentObservationPolicy(st, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	observeDeploymentMachine(t, st, "machine-future", "future-box", "canary",
		"22.22.3", "2026.9.6", now.Add(-time.Minute))
	observeDeploymentMachine(t, st, "machine-future", "future-box", "canary",
		"24.16.0", "2026.9.7", now.Add(time.Minute))
	result, err := NewWithArtifacts(st, dir).Updates(UpdateReadRequest{Channel: "canary"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Channels) != 1 || len(result.Channels[0].Members) != 1 {
		t.Fatalf("channels=%+v", result.Channels)
	}
	member := result.Channels[0].Members[0]
	if member.LastObservedVersion == nil || *member.LastObservedVersion != "2026.9.6" ||
		member.LastObservationReceivedAt == nil || !member.LastObservationReceivedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("future observation hid prior as-of evidence: %+v", member)
	}
	preview := result.Channels[0].Previews[0]
	if preview.Plan == nil || preview.Plan.Impact != 0 || preview.Plan.MissingPackages != 1 {
		t.Fatalf("rollout plan consumed observation received after evaluation instant: %+v", preview)
	}
}

func TestUpdatesPreservesObservationTimeWhenOldAgentOmittedInstall(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	if err := prepareDeploymentObservationPolicy(st, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	observeDeploymentMachine(t, st, "machine-old-agent", "old-agent-box", "canary",
		"24.16.0", "2026.9.7", now.Add(-time.Minute))
	if _, err := st.DB().Exec(`UPDATE observed_state SET payload=?
 WHERE rowid=(SELECT rowid FROM observed_state WHERE machine_id=? AND kind=? AND subject=?
 ORDER BY received_at DESC, measured_at DESC, rowid DESC LIMIT 1)`,
		`{"present":true}`, "machine-old-agent", store.KindOpenClaw, store.KindOpenClaw); err != nil {
		t.Fatal(err)
	}
	result, err := NewWithArtifacts(st, dir).Updates(UpdateReadRequest{Channel: "canary"}, now)
	if err != nil {
		t.Fatal(err)
	}
	member := result.Channels[0].Members[0]
	if member.LastObservedVersion != nil || member.LastObservationReceivedAt == nil ||
		!member.LastObservationReceivedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("old-agent observation time was discarded with absent install: %+v", member)
	}
}
