package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestHistoricalProfileReplacementRejectionIsReplayOnly(t *testing.T) {
	if _, current := canonicalOperatorProfileAssignmentRejectionDetail(historicalProfileReplacementCode); current {
		t.Fatal("historical profile replacement rejection is still active")
	}
	if detail, stored := storedOperatorProfileAssignmentRejectionDetail(historicalProfileReplacementCode); !stored || detail != "目前已有不同 profile；請使用 replacement workflow" {
		t.Fatalf("stored detail=%q present=%v", detail, stored)
	}
}

func operatorProfileAssignmentFixture(t *testing.T) (*Store, string, time.Time, OperatorMachineProfileAssignmentPrepared) {
	t.Helper()
	st := newTestStore(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := mustEnroll(t, st, "profile-checkin-order", now.Add(-time.Hour))
	node, _ := storedCatalogFixture()
	manifest, err := st.PublishCatalogManifest(node, "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := st.PublishMachineProfile(appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion,
		ID:            "node-profile",
		Revision:      1,
		Packages:      []appcatalog.PackageRef{{PackageID: node.ID, Version: node.Version}},
	}, "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(model.NodeRuntimeSpec{
		Kind: node.Adapter.Name, Version: node.Version, TargetOS: "linux", TargetArch: "amd64",
		BundleLayout: model.NodeRuntimeBundleLayoutV1,
		Artifact: &model.ArtifactRef{
			SHA256: node.Artifact.SHA256, Size: node.Artifact.Size,
			URL: "/v1/artifacts/" + node.Artifact.SHA256,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared := OperatorMachineProfileAssignmentPrepared{
		ProfileDigest: profile.Digest,
		Target:        appcatalog.Platform{OS: "linux", Arch: "amd64"},
		Packages: []OperatorMachineProfileAssignmentPreparedPackage{{
			PackageID: node.ID, PackageVersion: node.Version, ManifestDigest: manifest.Digest,
			ResourceKind: node.Adapter.Name, ResourceID: node.ID, Spec: string(spec),
			ArtifactDigest:   "sha256:" + node.Artifact.SHA256,
			ExecutionTimeout: OperatorMachineProfileAssignmentDefaultTimeout, Direct: true,
		}},
	}
	return st, machineID, now, prepared
}

func operatorProfileAssignmentTestRequest(machineID string, preview OperatorMachineProfileAssignmentPreviewResult) OperatorMachineProfileAssignmentRequest {
	return OperatorMachineProfileAssignmentRequest{
		MachineID: machineID, ProfileID: preview.ProfileID, ProfileRevision: preview.ProfileRevision,
		ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "test check-in ordering", IdempotencyKey: "profile-checkin-order",
		RequestDigest: sha256Digest([]byte("profile-checkin-order")), AssignedBy: "operator:test",
		Audit: AuditEntry{SourceAddr: "100.64.0.7", AuthSubject: "operator:test", SourceKind: "operator-api"},
	}
}

func TestOperatorProfileAssignmentUsesLatestCheckinInstantForJobsEnabled(t *testing.T) {
	st, machineID, _, prepared := operatorProfileAssignmentFixture(t)
	if _, err := st.DB().Exec(`INSERT INTO machine_checkins
		(machine_id,sent_at,received_at,jobs_enabled) VALUES
		(?,?,?,1),(?,?,?,0)`,
		machineID, "2026-09-14T11:59:00Z", "2026-09-14T11:59:00Z",
		machineID, "2026-09-14T11:59:30Z", "2026-09-14T10:59:30-01:00"); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorMachineProfileAssignment(machineID, "node-profile", 1, prepared)
	if err != nil {
		t.Fatal(err)
	}
	_, applyErr := st.ApplyOperatorMachineProfileAssignment(operatorProfileAssignmentTestRequest(machineID, preview),
		func() (OperatorMachineProfileAssignmentPrepared, error) { return prepared, nil })
	hasDisabled := len(preview.Blockers) == 1 &&
		preview.Blockers[0] == OperatorMachineProfileAssignmentBlockerExecutionDisabled
	assignments := countRows(t, st, `SELECT COUNT(*) FROM machine_profile_assignments`)
	desired := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
	jobs := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
	if !hasDisabled || !errors.Is(applyErr, ErrAgentExecutionDisabled) || assignments != 0 || desired != 0 || jobs != 0 {
		t.Fatalf("blockers=%v apply err=%v assignment rows=%d desired rows=%d job rows=%d",
			preview.Blockers, applyErr, assignments, desired, jobs)
	}
}

func TestOperatorProfileAssignmentRejectsUnparseableCheckinWithoutAuthority(t *testing.T) {
	st, machineID, _, prepared := operatorProfileAssignmentFixture(t)
	validPreview, err := st.PreviewOperatorMachineProfileAssignment(machineID, "node-profile", 1, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if validPreview.EverReported || validPreview.JobsEnabled != nil || len(validPreview.Blockers) != 1 ||
		validPreview.Blockers[0] != OperatorMachineProfileAssignmentBlockerNeverReported {
		t.Fatalf("never-reported preview=%+v", validPreview)
	}
	if _, err := st.DB().Exec(`INSERT INTO machine_checkins
		(machine_id,sent_at,received_at,jobs_enabled) VALUES
		(?,?,?,1),(?,?,?,0)`,
		machineID, "2026-09-14T11:58:00Z", "2026-09-14T11:58:00Z",
		machineID, "2026-09-14T11:59:00Z", "not-rfc3339"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PreviewOperatorMachineProfileAssignment(machineID, "node-profile", 1, prepared); err == nil ||
		err.Error() != "store: profile assignment target projection is invalid" {
		t.Fatalf("preview err=%v, want invalid projection", err)
	}
	if _, err := st.ApplyOperatorMachineProfileAssignment(operatorProfileAssignmentTestRequest(machineID, validPreview),
		func() (OperatorMachineProfileAssignmentPrepared, error) { return prepared, nil }); err == nil ||
		err.Error() != "store: profile assignment target projection is invalid" {
		t.Fatalf("apply err=%v, want invalid projection", err)
	}
	for table, query := range map[string]string{
		"assignment":  `SELECT COUNT(*) FROM machine_profile_assignments`,
		"package":     `SELECT COUNT(*) FROM machine_profile_assignment_packages`,
		"desired":     `SELECT COUNT(*) FROM desired_state`,
		"job":         `SELECT COUNT(*) FROM jobs`,
		"revision":    `SELECT COUNT(*) FROM revision_counters`,
		"idempotency": `SELECT COUNT(*) FROM operator_idempotency`,
		"audit":       `SELECT COUNT(*) FROM audit_log`,
	} {
		if got := countRows(t, st, query); got != 0 {
			t.Fatalf("invalid projection wrote %s authority rows=%d", table, got)
		}
	}
}

func TestOperatorProfileAssignmentCanonicalCheckinOrderingRemainsStable(t *testing.T) {
	t.Run("unselected history preserves preview digest", func(t *testing.T) {
		st, machineID, _, prepared := operatorProfileAssignmentFixture(t)
		if _, err := st.DB().Exec(`INSERT INTO machine_checkins
			(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,1)`,
			machineID, "2026-09-14T11:59:00Z", "2026-09-14T11:59:00Z"); err != nil {
			t.Fatal(err)
		}
		before, err := st.PreviewOperatorMachineProfileAssignment(machineID, "node-profile", 1, prepared)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO machine_checkins
			(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,0)`,
			machineID, "2026-09-14T11:58:00Z", "2026-09-14T11:58:00Z"); err != nil {
			t.Fatal(err)
		}
		after, err := st.PreviewOperatorMachineProfileAssignment(machineID, "node-profile", 1, prepared)
		if err != nil || after.PreviewDigest != before.PreviewDigest || after.JobsEnabled == nil ||
			!*after.JobsEnabled || len(after.Blockers) != 0 {
			t.Fatalf("before=%+v after=%+v err=%v", before, after, err)
		}
	})

	t.Run("same instant tie uses later rowid and preserves null", func(t *testing.T) {
		st, machineID, _, prepared := operatorProfileAssignmentFixture(t)
		if _, err := st.DB().Exec(`INSERT INTO machine_checkins
			(machine_id,sent_at,received_at,jobs_enabled) VALUES
			(?,?,?,1),(?,?,?,NULL)`,
			machineID, "2026-09-14T11:59:00Z", "2026-09-14T11:59:00Z",
			machineID, "2026-09-14T11:59:01Z", "2026-09-14T11:59:00Z"); err != nil {
			t.Fatal(err)
		}
		preview, err := st.PreviewOperatorMachineProfileAssignment(machineID, "node-profile", 1, prepared)
		if err != nil || preview.JobsEnabled != nil || len(preview.Blockers) != 1 ||
			preview.Blockers[0] != OperatorMachineProfileAssignmentBlockerExecutionUnknown {
			t.Fatalf("preview=%+v err=%v", preview, err)
		}
	})
}
