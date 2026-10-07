package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyAuditWithoutOperatorCorrelationStillRoundTrips(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecordAudit(AuditEntry{
		Action: AuditConnect, Subject: "legacy action", SourceAddr: "local-test", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("legacy audit list=%+v err=%v", entries, err)
	}
	if entries[0].IdempotencyKey != "" || entries[0].RequestDigest != "" {
		t.Fatalf("legacy audit invented operator correlation: %+v", entries[0])
	}
	if entries[0].AuthSubject != "" || entries[0].AuthNodeID != "" ||
		entries[0].AuthCapability != "" || entries[0].AuthMethod != "" ||
		entries[0].AuthDecision != "" || entries[0].BoundaryDecision != "" || entries[0].SourceKind != "" {
		t.Fatalf("legacy audit invented auth provenance: %+v", entries[0])
	}
}

func TestOpenAddsOperatorAndAuthColumnsToLegacyAuditLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAudit(AuditEntry{
		Action: AuditConnect, Subject: "pre-migration legacy", SourceAddr: "local-test", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP INDEX idx_audit_auth_action_at`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`ALTER TABLE audit_log DROP COLUMN idempotency_key`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`ALTER TABLE audit_log DROP COLUMN request_digest`); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"auth_subject", "auth_node_id", "auth_capability", "auth_method", "auth_decision", "boundary_decision", "source_kind"} {
		if _, err := s.DB().Exec(`ALTER TABLE audit_log DROP COLUMN ` + column); err != nil {
			t.Fatalf("drop %s: %v", column, err)
		}
	}
	_ = s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("migrate legacy audit_log: %v", err)
	}
	defer s.Close()
	var indexCount int
	if err := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='idx_audit_auth_action_at'`).Scan(&indexCount); err != nil || indexCount != 1 {
		t.Fatalf("login audit index: %d %v", indexCount, err)
	}
	have, err := columnSet(s.DB(), "audit_log")
	if err != nil || !have["idempotency_key"] || !have["request_digest"] ||
		!have["auth_subject"] || !have["auth_node_id"] || !have["auth_capability"] ||
		!have["auth_method"] || !have["auth_decision"] || !have["boundary_decision"] || !have["source_kind"] {
		t.Fatalf("operator audit columns not restored: have=%v err=%v", have, err)
	}
	entries, err := s.Audit("", 10)
	if err != nil || len(entries) != 1 || entries[0].Subject != "pre-migration legacy" ||
		entries[0].IdempotencyKey != "" || entries[0].RequestDigest != "" ||
		entries[0].AuthSubject != "" || entries[0].AuthNodeID != "" ||
		entries[0].AuthCapability != "" || entries[0].AuthMethod != "" ||
		entries[0].AuthDecision != "" || entries[0].BoundaryDecision != "" || entries[0].SourceKind != "" {
		t.Fatalf("legacy audit did not survive migration: entries=%+v err=%v", entries, err)
	}
}

func TestAuditAuthProvenanceRoundTripsBesideTransportProvenance(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := AuditEntry{
		Action: AuditMachineChannel, Subject: "samplehub1", SourceAddr: "100.64.0.7",
		WhoNode: "operator-laptop", WhoUser: "operator@example.com",
		WhoUnavailable: "", UserAgent: "clawctl-test/1",
		AuthSubject: "tailscale-user:42", AuthNodeID: "node-stable-1",
		AuthCapability: "example.com/cap/clawctl-admin",
		AuthMethod:     "tailscale-localapi-app-cap", AuthDecision: "AUTHORIZED",
		BoundaryDecision: "PASSED", SourceKind: "operator-api", OK: true,
	}
	if err := s.RecordAudit(want); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	got := entries[0]
	if got.SourceAddr != want.SourceAddr || got.WhoNode != want.WhoNode ||
		got.WhoUser != want.WhoUser || got.WhoUnavailable != want.WhoUnavailable ||
		got.UserAgent != want.UserAgent || got.AuthSubject != want.AuthSubject ||
		got.AuthNodeID != want.AuthNodeID || got.AuthCapability != want.AuthCapability ||
		got.AuthMethod != want.AuthMethod || got.AuthDecision != want.AuthDecision ||
		got.BoundaryDecision != want.BoundaryDecision ||
		got.SourceKind != want.SourceKind {
		t.Fatalf("audit provenance round trip\n got: %+v\nwant: %+v", got, want)
	}
}

func TestOperatorStoreOverwritesMismatchedAuditCorrelation(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "correlation-machine", "correlation-machine", true)
	revision := int64(1) // fixture's legacy-style channel write is revisioned by the trigger
	_, err := s.ApplyOperatorMachineChannel(OperatorMachineChannelRequest{
		MachineID: "correlation-machine", Channel: "stable", ExpectedRevision: &revision,
		ConfirmDisplayName: "correlation-machine",
		IdempotencyKey:     "authoritative-key",
		RequestDigest:      "sha256:authoritative-digest",
		Audit: AuditEntry{
			IdempotencyKey: "caller-lie", RequestDigest: "sha256:caller-lie",
			SourceAddr: "local-test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.Audit("correlation-machine", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	if entries[0].IdempotencyKey != "authoritative-key" ||
		entries[0].RequestDigest != "sha256:authoritative-digest" {
		t.Fatalf("caller broke audit correlation: %+v", entries[0])
	}
	var joined int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_log a
	 JOIN operator_idempotency i
	   ON i.idempotency_key=a.idempotency_key AND i.request_digest=a.request_digest
	 WHERE a.audit_id=?`, entries[0].ID).Scan(&joined); err != nil || joined != 1 {
		t.Fatalf("authoritative audit does not join: joined=%d err=%v", joined, err)
	}
}

func TestAuditDoesNotPersistOversizedIdempotencyKey(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key := strings.Repeat("oversized-key-", 100)
	if err := s.RecordAudit(AuditEntry{
		Action: AuditMachineChannel, Subject: "samplehub1", SourceAddr: "local-test",
		IdempotencyKey: key, OK: false,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	got := entries[0].IdempotencyKey
	if got == key || len(got) > 200 || !strings.HasPrefix(got, "invalid-key-sha256:") ||
		!strings.Contains(got, "未保存") {
		t.Fatalf("oversized key was not replaced by bounded labelled hash: %q", got)
	}
}

func TestAuditBoundsCallerControlledSubject(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	oversized := strings.Repeat("caller-controlled-path/", 200)
	if err := s.RecordAudit(AuditEntry{
		Action: AuditOperatorDenied, Subject: oversized, SourceAddr: "100.64.0.7:4321",
		OK: false,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	if got := entries[0].Subject; got == oversized ||
		len([]rune(got)) != auditMaxReason+len([]rune("…（已截斷）")) ||
		!strings.HasPrefix(got, oversized[:auditMaxReason]) || !strings.HasSuffix(got, "…（已截斷）") {
		t.Fatalf("caller-controlled subject was not bounded to %d runes plus marker: len=%d", auditMaxReason, len([]rune(got)))
	}
}

func TestOperatorDenialAuditIsBoundedWithoutDeletingDomainAudit(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecordAudit(AuditEntry{
		Action: AuditRetire, Subject: "permanent-domain-row", SourceAddr: "local-test", OK: false,
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := s.recordOperatorDenialBounded(AuditEntry{
			Action: AuditOperatorDenied, Subject: fmt.Sprintf("denial-%d", i),
			SourceAddr: "100.64.0.7", OK: false,
		}, 3); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.Audit("", 20)
	if err != nil {
		t.Fatal(err)
	}
	denials := make(map[string]bool)
	domainRows := 0
	for _, entry := range entries {
		switch entry.Action {
		case AuditOperatorDenied:
			denials[entry.Subject] = true
		case AuditRetire:
			if entry.Subject == "permanent-domain-row" {
				domainRows++
			}
		}
	}
	if len(denials) != 3 || !denials["denial-2"] || !denials["denial-3"] || !denials["denial-4"] {
		t.Fatalf("bounded denial ring=%v, want newest three", denials)
	}
	if domainRows != 1 {
		t.Fatalf("bounding denials deleted ordinary control audit: entries=%+v", entries)
	}
	if err := s.recordOperatorDenialBounded(AuditEntry{Action: AuditRetire}, 3); err == nil {
		t.Fatal("bounded denial writer accepted a non-denial action")
	}
	if err := s.recordOperatorDenialBounded(AuditEntry{Action: AuditOperatorDenied}, 0); err == nil {
		t.Fatal("bounded denial writer accepted a non-positive limit")
	}
}

func TestConsoleAuditCapsDenialsWithoutHidingOlderDomainRows(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 175 {
		if err := s.RecordAudit(AuditEntry{
			Action: AuditRetire, Subject: fmt.Sprintf("domain-%03d", i),
			SourceAddr: "local-test", OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 80 {
		if err := s.RecordAudit(AuditEntry{
			Action: AuditOperatorDenied, Subject: fmt.Sprintf("denial-%03d", i),
			SourceAddr: "100.64.0.7", OK: false,
		}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := s.ConsoleAudit("")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != AuditLimit {
		t.Fatalf("console audit has %d rows, want %d", len(entries), AuditLimit)
	}
	denials := 0
	subjects := make(map[string]bool, len(entries))
	for _, entry := range entries {
		subjects[entry.Subject] = true
		if entry.Action == AuditOperatorDenied {
			denials++
		}
	}
	if denials != consoleOperatorDenialLimit {
		t.Fatalf("console audit has %d denials, want fixed cap %d", denials, consoleOperatorDenialLimit)
	}
	for _, want := range []string{"denial-079", "denial-030", "domain-174", "domain-025"} {
		if !subjects[want] {
			t.Errorf("console audit missing expected row %q", want)
		}
	}
	for _, excluded := range []string{"denial-029", "domain-024"} {
		if subjects[excluded] {
			t.Errorf("console audit included row beyond its per-kind/total limit %q", excluded)
		}
	}

	// Audit remains the unshaped primitive: its newest 200 rows include all 80
	// newer denials. ConsoleAudit is strictly a presentation query.
	raw, err := s.Audit("", AuditLimit)
	if err != nil {
		t.Fatal(err)
	}
	rawDenials := 0
	for _, entry := range raw {
		if entry.Action == AuditOperatorDenied {
			rawDenials++
		}
	}
	if rawDenials != 80 {
		t.Fatalf("Audit semantics changed: newest raw rows contain %d denials, want 80", rawDenials)
	}
}

func TestConsoleAuditAppliesMachineFilterBeforeDenialSampling(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 10 {
		if err := s.RecordAudit(AuditEntry{
			Action: AuditConnect, MachineID: "machine-a", Subject: fmt.Sprintf("a-domain-%02d", i),
			SourceAddr: "local-test", OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 60 {
		for _, machineID := range []string{"machine-a", "machine-b"} {
			if err := s.RecordAudit(AuditEntry{
				Action: AuditOperatorDenied, MachineID: machineID,
				Subject:    fmt.Sprintf("%s-denial-%02d", machineID, i),
				SourceAddr: "100.64.0.7", OK: false,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	entries, err := s.ConsoleAudit("machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 10+consoleOperatorDenialLimit {
		t.Fatalf("machine console audit has %d rows, want 60", len(entries))
	}
	denials := 0
	for _, entry := range entries {
		if entry.MachineID != "machine-a" || strings.Contains(entry.Subject, "machine-b") {
			t.Fatalf("machine filter leaked another machine: %+v", entry)
		}
		if entry.Action == AuditOperatorDenied {
			denials++
		}
	}
	if denials != consoleOperatorDenialLimit {
		t.Fatalf("machine-filtered denial count=%d, want %d", denials, consoleOperatorDenialLimit)
	}
}
