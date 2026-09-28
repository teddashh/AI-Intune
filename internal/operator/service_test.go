package operator

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func enrollMachine(t *testing.T, st *store.Store, name string, observed bool) string {
	t.Helper()
	tok, err := st.CreateEnrollToken(name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)
	id, _, err := st.RedeemEnrollToken(tok, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: tok,
		Hostname: name, OS: "linux", Arch: "amd64", UnixUser: "operator-test",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if observed {
		if err := st.RecordObservation(id, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now,
			OpenClaw: model.OpenClaw{Present: true},
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestMachineChannelIdempotencySurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := enrollMachine(t, st, "persistent-key", true)
	revision := int64(0)
	req := MachineChannelRequest{
		MachineID: id, Channel: "canary", ExpectedRevision: &revision,
		ConfirmDisplayName: "persistent-key", IdempotencyKey: "persists-across-restart",
		Actor: Actor{SourceAddr: "local-test"},
	}
	first, err := New(st).ChangeMachineChannel(req)
	if err != nil || first.Revision != 1 || first.Replayed {
		t.Fatalf("first result=%+v err=%v", first, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	replay, err := New(st).ChangeMachineChannel(req)
	if err != nil || !replay.Replayed || replay.Revision != 1 {
		t.Fatalf("reopened replay=%+v err=%v", replay, err)
	}
	m, err := st.GetMachine(id)
	if err != nil || m.Channel != "canary" || m.ChannelRevision != 1 {
		t.Fatalf("reopened machine=%+v err=%v", m, err)
	}
	entries, err := st.Audit(id, 10)
	if err != nil || len(entries) != 2 || entries[0].IdempotencyKey != req.IdempotencyKey ||
		entries[0].RequestDigest == "" || entries[0].RequestDigest != entries[1].RequestDigest {
		t.Fatalf("reopened replay audit=%+v err=%v", entries, err)
	}
}

func TestMachineChannelRejectedOutcomeIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := enrollMachine(t, st, "rejected-key", false)
	revision := int64(0)
	req := MachineChannelRequest{
		MachineID: id, Channel: "stable", ExpectedRevision: &revision,
		ConfirmDisplayName: "rejected-key", IdempotencyKey: "rejected-request",
		Actor: Actor{SourceAddr: "local-test"},
	}
	_, firstErr := New(st).ChangeMachineChannel(req)
	if !errors.Is(firstErr, store.ErrNeverObserved) {
		t.Fatalf("first rejection=%v", firstErr)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Date(2026, 9, 6, 18, 5, 0, 0, time.UTC)
	if err := st.RecordObservation(id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		OpenClaw: model.OpenClaw{Present: true},
	}, now); err != nil {
		t.Fatal(err)
	}
	_, replayErr := New(st).ChangeMachineChannel(req)
	if !errors.Is(replayErr, store.ErrNeverObserved) {
		t.Fatalf("same rejected request acquired new meaning: %v", replayErr)
	}
	var rejection *store.OperatorRequestError
	if !errors.As(replayErr, &rejection) || !rejection.Replayed {
		t.Fatalf("rejected replay did not say replay: %T %v", replayErr, replayErr)
	}
	m, err := st.GetMachine(id)
	if err != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("cached rejection changed machine=%+v err=%v", m, err)
	}
	entries, err := st.Audit(id, 10)
	if err != nil || len(entries) != 2 || !strings.Contains(entries[0].Detail, "idempotency replay") {
		t.Fatalf("rejection replay audit=%+v err=%v", entries, err)
	}
	if entries[0].IdempotencyKey != "rejected-request" || entries[0].RequestDigest == "" ||
		entries[1].IdempotencyKey != entries[0].IdempotencyKey ||
		entries[1].RequestDigest != entries[0].RequestDigest {
		t.Fatalf("rejection/replay audit correlation=%+v", entries)
	}
}

func TestMachineChannelMutationRollsBackWhenAtomicAuditCannotBeWritten(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := enrollMachine(t, st, "atomic-audit", true)
	if _, err := st.DB().Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	_, err = New(st).ChangeMachineChannel(MachineChannelRequest{
		MachineID: id, Channel: "canary", ExpectedRevision: &revision,
		ConfirmDisplayName: "atomic-audit", IdempotencyKey: "audit-must-commit",
		Actor: Actor{SourceAddr: "local-test"},
	})
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("missing audit table did not fail closed: %v", err)
	}
	m, getErr := st.GetMachine(id)
	if getErr != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("mutation survived missing atomic audit: machine=%+v err=%v", m, getErr)
	}
	var requests int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key='audit-must-commit'`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("idempotency result committed without audit: %d", requests)
	}
}

func TestCachedMachineChannelReplayRefusesUnauditedResponse(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := enrollMachine(t, st, "replay-audit", true)
	revision := int64(0)
	req := MachineChannelRequest{
		MachineID: id, Channel: "canary", ExpectedRevision: &revision,
		ConfirmDisplayName: "replay-audit", IdempotencyKey: "replay-needs-audit",
		Actor: Actor{SourceAddr: "local-test"},
	}
	if _, err := New(st).ChangeMachineChannel(req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st).ChangeMachineChannel(req); err == nil || !strings.Contains(err.Error(), "replay audit") {
		t.Fatalf("cached response escaped without replay audit: %v", err)
	}
	m, err := st.GetMachine(id)
	if err != nil || m.Channel != "canary" || m.ChannelRevision != 1 {
		t.Fatalf("failed replay audit changed original result: machine=%+v err=%v", m, err)
	}
}

func TestMachineChannelCarriesAuthAndTransportProvenanceToAudit(t *testing.T) {
	for _, tc := range []struct {
		name           string
		idempotencyKey string
		wantErr        error
	}{
		{name: "atomic mutation", idempotencyKey: "authenticated-change"},
		{name: "pre-ledger rejection", wantErr: store.ErrIdempotencyKeyRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			id := enrollMachine(t, st, "authenticated-machine", true)
			revision := int64(0)
			actor := Actor{
				SourceAddr: "100.64.0.7", WhoNode: "operator-laptop",
				WhoUser: "operator@example.com", UserAgent: "clawctl-test/1",
				AuthSubject: "tailscale-user:42", AuthNodeID: "node-stable-1",
				AuthCapability: "example.com/cap/clawctl-admin",
				AuthMethod:     "tailscale-localapi-app-cap", AuthDecision: "AUTHORIZED",
				SourceKind: "operator-api",
			}
			_, err = New(st).ChangeMachineChannel(MachineChannelRequest{
				MachineID: id, Channel: "canary", ExpectedRevision: &revision,
				ConfirmDisplayName: "authenticated-machine", IdempotencyKey: tc.idempotencyKey,
				Actor: actor,
			})
			if tc.wantErr == nil && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error=%v, want %v", err, tc.wantErr)
			}
			entries, err := st.Audit(id, 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("audit=%+v err=%v", entries, err)
			}
			got := entries[0]
			if got.SourceAddr != actor.SourceAddr || got.WhoNode != actor.WhoNode ||
				got.WhoUser != actor.WhoUser || got.UserAgent != actor.UserAgent ||
				got.AuthSubject != actor.AuthSubject || got.AuthNodeID != actor.AuthNodeID ||
				got.AuthCapability != actor.AuthCapability || got.AuthMethod != actor.AuthMethod ||
				got.AuthDecision != actor.AuthDecision || got.SourceKind != actor.SourceKind {
				t.Fatalf("actor provenance did not reach audit: %+v", got)
			}
		})
	}
}

func TestEnrollTokenServiceFreshAndReplayShareAtomicDomainPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service := New(st)
	preview, err := service.PreviewEnrollToken(EnrollTokenPreviewRequest{
		DisplayName: "operator-enroll", TTLSeconds: 7200,
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{
		SourceAddr: "100.64.0.20", WhoNode: "operator-node", WhoUser: "operator@example.com",
		UserAgent: "clawctl-test/1", AuthSubject: "tailscale-user:20", AuthNodeID: "node-20",
		AuthCapability: "example.com/cap/clawctl-admin",
		AuthMethod:     "tailscale-localapi-app-cap", AuthDecision: "AUTHORIZED",
		SourceKind: SourceKindOperatorAPI,
	}
	req := EnrollTokenCreateRequest{
		DisplayName: preview.DisplayName, TTLSeconds: preview.TTLSeconds,
		PreviewDigest: preview.PreviewDigest, Reason: "add expected onode",
		IdempotencyKey: "operator-enroll-service", Actor: actor,
	}
	first, err := service.CreateEnrollToken(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.EnrollmentToken == "" || !first.SecretAvailable || first.Replayed ||
		first.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("fresh=%+v", first)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	service = New(st)
	replay, err := service.CreateEnrollToken(req)
	if err != nil {
		t.Fatal(err)
	}
	if replay.MachineID != first.MachineID || replay.EnrollmentToken != "" ||
		replay.SecretAvailable || !replay.Replayed || !replay.RecoveryRequired ||
		replay.RecoveryAction != store.OperatorEnrollTokenRecoveryRevokeAndReissue {
		t.Fatalf("replay=%+v", replay)
	}
	entries, err := st.Audit(first.MachineID, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	for _, got := range entries {
		if got.Reason != req.Reason || got.IdempotencyKey != req.IdempotencyKey ||
			got.RequestDigest != EnrollTokenSemanticDigest(req) || got.SourceAddr != actor.SourceAddr ||
			got.WhoNode != actor.WhoNode || got.WhoUser != actor.WhoUser ||
			got.AuthSubject != actor.AuthSubject || got.AuthNodeID != actor.AuthNodeID ||
			got.AuthCapability != actor.AuthCapability || got.AuthMethod != actor.AuthMethod ||
			got.AuthDecision != actor.AuthDecision || got.SourceKind != actor.SourceKind ||
			strings.Contains(got.Detail, first.EnrollmentToken) {
			t.Fatalf("enroll audit provenance/secret=%+v", got)
		}
	}
}

func TestEnrollTokenSemanticDigestBindsReasonAndCanonicalBody(t *testing.T) {
	base := EnrollTokenCreateRequest{
		DisplayName: "digest-enroll", TTLSeconds: 3600,
		PreviewDigest: "sha256:preview", Reason: "because",
		IdempotencyKey: "key-one", Actor: Actor{SourceAddr: "100.64.0.1"},
	}
	want := EnrollTokenSemanticDigest(base)
	decoration := base
	decoration.IdempotencyKey = "key-two"
	decoration.Actor = Actor{SourceAddr: "100.64.0.2", AuthSubject: "another-actor"}
	if got := EnrollTokenSemanticDigest(decoration); got != want {
		t.Fatalf("transport decoration changed semantic digest: %q != %q", got, want)
	}
	tests := []struct {
		name string
		edit func(*EnrollTokenCreateRequest)
	}{
		{name: "display name", edit: func(r *EnrollTokenCreateRequest) { r.DisplayName += "-other" }},
		{name: "ttl", edit: func(r *EnrollTokenCreateRequest) { r.TTLSeconds++ }},
		{name: "preview", edit: func(r *EnrollTokenCreateRequest) { r.PreviewDigest += "x" }},
		{name: "reason", edit: func(r *EnrollTokenCreateRequest) { r.Reason += "!" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.edit(&changed)
			if got := EnrollTokenSemanticDigest(changed); got == want {
				t.Fatalf("%s did not change semantic digest", tc.name)
			}
		})
	}
}

func TestEnrollTokenServiceSameKeyChangedReasonConflicts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	service := New(st)
	preview, err := service.PreviewEnrollToken(EnrollTokenPreviewRequest{
		DisplayName: "reason-conflict", TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := EnrollTokenCreateRequest{
		DisplayName: preview.DisplayName, TTLSeconds: preview.TTLSeconds,
		PreviewDigest: preview.PreviewDigest, Reason: "first reason",
		IdempotencyKey: "reason-conflict-key", Actor: Actor{SourceAddr: "local-test"},
	}
	first, err := service.CreateEnrollToken(req)
	if err != nil {
		t.Fatal(err)
	}
	req.Reason = "different reason"
	result, err := service.CreateEnrollToken(req)
	if !errors.Is(err, store.ErrIdempotencyConflict) || result.EnrollmentToken != "" {
		t.Fatalf("changed reason result=%+v err=%v", result, err)
	}
	var machines, tokens int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machines); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM enrollment_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if machines != 1 || tokens != 1 {
		t.Fatalf("conflict changed state: machines=%d tokens=%d original=%s", machines, tokens, first.MachineID)
	}
}

func TestEnrollTokenServiceMissingKeyHasFallbackAuditAndNoState(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	service := New(st)
	preview, err := service.PreviewEnrollToken(EnrollTokenPreviewRequest{
		DisplayName: "missing-key", TTLSeconds: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{
		SourceAddr: "100.64.0.30", AuthSubject: "tailscale-user:30",
		AuthCapability: "example.com/cap/clawctl-admin", AuthDecision: "AUTHORIZED",
		SourceKind: SourceKindOperatorAPI,
	}
	result, err := service.CreateEnrollToken(EnrollTokenCreateRequest{
		DisplayName: preview.DisplayName, TTLSeconds: preview.TTLSeconds,
		PreviewDigest: preview.PreviewDigest, Reason: "must be audited", Actor: actor,
	})
	if !errors.Is(err, store.ErrIdempotencyKeyRequired) || result.EnrollmentToken != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM machine_registry`,
		`SELECT COUNT(*) FROM enrollment_tokens`,
		`SELECT COUNT(*) FROM operator_idempotency`,
	} {
		var count int
		if err := st.DB().QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("missing key changed state for %q: %d", query, count)
		}
	}
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	if entry.Action != store.AuditEnrollToken || entry.OK || entry.Subject != preview.DisplayName ||
		entry.Reason != "must be audited" || entry.IdempotencyKey != "" || entry.RequestDigest == "" ||
		entry.SourceAddr != actor.SourceAddr || entry.AuthSubject != actor.AuthSubject ||
		entry.AuthCapability != actor.AuthCapability || entry.AuthDecision != actor.AuthDecision ||
		entry.SourceKind != actor.SourceKind {
		t.Fatalf("fallback audit=%+v", entry)
	}
}

func TestEnrollTokenCorruptCachedRejectionMapsToFixedInternalFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	service := New(st)
	req := EnrollTokenCreateRequest{
		DisplayName: "corrupt-rejection", TTLSeconds: 3600,
		IdempotencyKey: "corrupt-rejection-key", Actor: Actor{SourceAddr: "local-test"},
	}
	if _, err := service.CreateEnrollToken(req); !errors.Is(err, store.ErrPreviewRequired) {
		t.Fatalf("seed rejection=%v", err)
	}
	const corruptDetail = "do-not-copy-secret-from-corrupt-cache"
	if _, err := st.DB().Exec(`UPDATE operator_idempotency
	 SET error_code='FORGED_CODE',error_detail=? WHERE idempotency_key=?`,
		corruptDetail, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	result, err := service.CreateEnrollToken(req)
	if err == nil || result.EnrollmentToken != "" || strings.Contains(err.Error(), corruptDetail) {
		t.Fatalf("corrupt result=%+v err=%v", result, err)
	}
	status, code, detail := HTTPError(err)
	if status != 500 || code != "INTERNAL" || detail != "控制面操作失敗" {
		t.Fatalf("HTTP mapping=(%d,%q,%q)", status, code, detail)
	}
	entries, auditErr := st.Audit("", 10)
	if auditErr != nil || len(entries) != 2 || entries[0].IsOperatorReplay() || entries[0].OK ||
		strings.Contains(entries[0].Detail, corruptDetail) ||
		!strings.Contains(entries[0].Detail, "idempotency cache invalid") {
		t.Fatalf("corrupt rejection audit=%+v err=%v", entries, auditErr)
	}
}

func TestEnrollTokenRevocationServiceSharesAtomicPathAndActorEvidence(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	machineID, _, err := st.CreateEnrollTokenFor("service-revoke", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service := New(st)
	status, err := service.PendingEnrollToken(machineID)
	if err != nil || status.MachineID != machineID || status.DisplayName != "service-revoke" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	preview, err := service.PreviewEnrollTokenRevocation(
		EnrollTokenRevocationPreviewRequest{MachineID: machineID})
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{
		SourceAddr: "100.64.0.21", WhoNode: "operator-node", WhoUser: "operator@example.com",
		UserAgent: "clawctl-test/1", AuthSubject: "tailscale-user:21", AuthNodeID: "node-21",
		AuthCapability: "example.com/cap/clawctl-admin",
		AuthMethod:     "tailscale-localapi-app-cap", AuthDecision: "AUTHORIZED",
		SourceKind: SourceKindOperatorAPI,
	}
	req := EnrollTokenRevocationRequest{
		MachineID: machineID, PreviewDigest: preview.PreviewDigest,
		Reason: "credential exposed", IdempotencyKey: "service-revoke-key", Actor: actor,
	}
	first, err := service.RevokeEnrollToken(req)
	if err != nil || first.Replayed || !first.RegistryRetained ||
		first.DenominatorDelta != 0 || first.ActiveAgentCredentialAffected {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	replay, err := service.RevokeEnrollToken(req)
	if err != nil || !replay.Replayed || replay.MachineID != first.MachineID ||
		!replay.RevokedAt.Equal(first.RevokedAt) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	entries, err := st.Audit(machineID, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	wantDigest := EnrollTokenRevocationSemanticDigest(req)
	for _, entry := range entries {
		if entry.Action != store.AuditRevokeToken || entry.Reason != req.Reason ||
			entry.IdempotencyKey != req.IdempotencyKey || entry.RequestDigest != wantDigest ||
			entry.SourceAddr != actor.SourceAddr || entry.WhoNode != actor.WhoNode ||
			entry.WhoUser != actor.WhoUser || entry.AuthSubject != actor.AuthSubject ||
			entry.AuthNodeID != actor.AuthNodeID || entry.AuthCapability != actor.AuthCapability ||
			entry.AuthMethod != actor.AuthMethod || entry.AuthDecision != actor.AuthDecision ||
			entry.SourceKind != actor.SourceKind {
			t.Fatalf("revocation audit lost correlation/provenance: %+v", entry)
		}
	}
}

func TestEnrollTokenRevocationSemanticDigestBindsTargetPreviewAndReason(t *testing.T) {
	base := EnrollTokenRevocationRequest{
		MachineID: "machine-1", PreviewDigest: "sha256:preview", Reason: "because",
		IdempotencyKey: "key-1", Actor: Actor{SourceAddr: "100.64.0.1"},
	}
	want := EnrollTokenRevocationSemanticDigest(base)
	decoration := base
	decoration.IdempotencyKey = "key-2"
	decoration.Actor = Actor{SourceAddr: "100.64.0.2"}
	if got := EnrollTokenRevocationSemanticDigest(decoration); got != want {
		t.Fatalf("transport decoration changed digest: %q != %q", got, want)
	}
	for _, edit := range []func(*EnrollTokenRevocationRequest){
		func(r *EnrollTokenRevocationRequest) { r.MachineID += "x" },
		func(r *EnrollTokenRevocationRequest) { r.PreviewDigest += "x" },
		func(r *EnrollTokenRevocationRequest) { r.Reason += "x" },
	} {
		changed := base
		edit(&changed)
		if got := EnrollTokenRevocationSemanticDigest(changed); got == want {
			t.Fatalf("domain field did not change digest: %+v", changed)
		}
	}
}

func TestEnrollTokenNotPendingHTTPErrorIsConflict(t *testing.T) {
	status, code, detail := HTTPError(&store.OperatorRequestError{
		Code: store.OperatorCodeEnrollTokenNotPending, Detail: "not pending",
	})
	if status != http.StatusConflict || code != store.OperatorCodeEnrollTokenNotPending || detail != "not pending" {
		t.Fatalf("mapping=(%d,%q,%q)", status, code, detail)
	}
}
