package store

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

func operatorCatalogRequest(t *testing.T, key string) OperatorCatalogManifestRequest {
	t.Helper()
	_, manifest := storedCatalogFixture()
	return OperatorCatalogManifestRequest{
		Manifest: manifest, PublishedBy: "user:test@example.com", Reason: "approve exact package",
		IdempotencyKey: key, RequestDigest: sha256Digest([]byte("request:" + key)),
		Audit: AuditEntry{
			SourceAddr: "100.64.0.10", AuthSubject: "user:test@example.com",
			AuthDecision: "authorized", SourceKind: "operator-api",
		},
	}
}

func TestOperatorCatalogManifestPublishesReplaysAndAuditsAtomically(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	req := operatorCatalogRequest(t, "catalog-publish-1")
	var verifyCalls atomic.Int64
	first, err := st.ApplyOperatorCatalogManifest(req, func() error {
		verifyCalls.Add(1)
		return nil
	})
	if err != nil || first.Replayed || first.AlreadyPublished || !first.Audited ||
		first.Record.Manifest.ID != req.Manifest.ID || first.Record.PublishedBy != req.PublishedBy {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	now = now.Add(time.Minute)
	replay, err := st.ApplyOperatorCatalogManifest(req, func() error {
		t.Fatal("idempotent replay verified external artifact again")
		return nil
	})
	if err != nil || !replay.Replayed || replay.AlreadyPublished || !replay.Audited ||
		replay.Record.Digest != first.Record.Digest || verifyCalls.Load() != 1 {
		t.Fatalf("replay=%+v calls=%d err=%v", replay, verifyCalls.Load(), err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM catalog_manifests`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='catalog-manifest'`) != 2 {
		t.Fatal("catalog publication ledgers are incomplete")
	}
	var original, replayed int
	if err := st.DB().QueryRow(`SELECT
	 SUM(CASE WHEN detail NOT LIKE 'idempotency replay；%' THEN 1 ELSE 0 END),
	 SUM(CASE WHEN detail LIKE 'idempotency replay；%' THEN 1 ELSE 0 END)
	 FROM audit_log WHERE action='catalog-manifest' AND idempotency_key=?`, req.IdempotencyKey).Scan(&original, &replayed); err != nil {
		t.Fatal(err)
	}
	if original != 1 || replayed != 1 {
		t.Fatalf("original=%d replayed=%d", original, replayed)
	}
}

func TestOperatorCatalogManifestSecondKeyRecordsAlreadyPublished(t *testing.T) {
	st := newTestStore(t)
	firstReq := operatorCatalogRequest(t, "catalog-first-key")
	first, err := st.ApplyOperatorCatalogManifest(firstReq, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	secondReq := operatorCatalogRequest(t, "catalog-second-key")
	secondReq.RequestDigest = firstReq.RequestDigest
	secondReq.PublishedBy = "user:second@example.com"
	second, err := st.ApplyOperatorCatalogManifest(secondReq, func() error { return nil })
	if err != nil || second.Replayed || !second.AlreadyPublished ||
		second.Record.PublishedBy != first.Record.PublishedBy || !second.Record.PublishedAt.Equal(first.Record.PublishedAt) {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM catalog_manifests`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 2 {
		t.Fatal("already-published decision changed immutable catalog state")
	}
}

func TestOperatorCatalogManifestCachesTypedRejectionAndConflictsOnKeyReuse(t *testing.T) {
	st := newTestStore(t)
	req := operatorCatalogRequest(t, "catalog-rejection")
	var calls atomic.Int64
	verify := func() error {
		calls.Add(1)
		return operatorError(OperatorCodeCatalogArtifactMismatch, "adapter detail is not persisted")
	}
	_, err := st.ApplyOperatorCatalogManifest(req, verify)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeCatalogArtifactMismatch ||
		rejection.Replayed || !rejection.Audited {
		t.Fatalf("first rejection=%+v err=%v", rejection, err)
	}
	_, err = st.ApplyOperatorCatalogManifest(req, func() error {
		t.Fatal("rejected replay verified external artifact again")
		return nil
	})
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeCatalogArtifactMismatch ||
		!rejection.Replayed || !rejection.Audited || calls.Load() != 1 {
		t.Fatalf("replayed rejection=%+v calls=%d err=%v", rejection, calls.Load(), err)
	}
	changed := req
	changed.RequestDigest = sha256Digest([]byte("changed body"))
	_, err = st.ApplyOperatorCatalogManifest(changed, func() error { return nil })
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("key conflict err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM catalog_manifests`) != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='catalog-manifest'`) != 3 {
		t.Fatal("rejected publication ledgers are incoherent")
	}
}

func TestOperatorCatalogManifestTransientVerificationFailureDoesNotConsumeKey(t *testing.T) {
	st := newTestStore(t)
	req := operatorCatalogRequest(t, "catalog-transient")
	transient := errors.New("artifact catalog I/O failed")
	if _, err := st.ApplyOperatorCatalogManifest(req, func() error { return transient }); !errors.Is(err, transient) {
		t.Fatalf("transient err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM catalog_manifests`) != 0 {
		t.Fatal("transient verification failure consumed publication identity")
	}
	result, err := st.ApplyOperatorCatalogManifest(req, func() error { return nil })
	if err != nil || result.Replayed {
		t.Fatalf("retry=%+v err=%v", result, err)
	}
}

func TestOperatorCatalogManifestAuditFailureRollsBackEveryLedger(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.DB().Exec(`CREATE TRIGGER reject_catalog_manifest_audit
	 BEFORE INSERT ON audit_log WHEN NEW.action='catalog-manifest'
	 BEGIN SELECT RAISE(ABORT, 'reject catalog audit'); END`); err != nil {
		t.Fatal(err)
	}
	req := operatorCatalogRequest(t, "catalog-audit-rollback")
	if _, err := st.ApplyOperatorCatalogManifest(req, func() error { return nil }); err == nil {
		t.Fatal("publication succeeded without atomic audit evidence")
	}
	if countRows(t, st, `SELECT COUNT(*) FROM catalog_manifests`) != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM audit_log`) != 0 {
		t.Fatal("audit failure left a partial catalog publication")
	}
}

func TestOperatorCatalogManifestDetectsReceiptAndLedgerTampering(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(t *testing.T, st *Store)
		tamper string
	}{
		{name: "receipt", tamper: `UPDATE operator_idempotency SET response_json='{}'`},
		{name: "manifest", tamper: `UPDATE catalog_manifests SET manifest_digest='sha256:tampered'`},
		{name: "original audit", tamper: `DELETE FROM audit_log WHERE action='catalog-manifest'`},
		// Replay 用收據上的 package id/version 去查 catalog_manifests，
		// 然後把查到的那一列交給 operator。把身分換成另一份已發布的
		// manifest，格式檢查、原始 audit 的 detail（只含 manifest_digest、
		// artifact_sha256、already_published）與 audit subject（來自 request，
		// 不是收據）全都照樣對得上；唯一發現不對的是
		// catalogManifestReceiptMatchesRecord：它比的 manifest_digest、
		// artifact_sha256 與 published_by 都跟另一份對不上。id 與 version
		// 比了恆等——那兩項就是查詢鍵；published_at 也擋不住，兩份是同一秒
		// 發布的。若這道比對失守，
		// operator 用這把 key 重送會從 JSON API 拿回另一份 manifest 的
		// 整份內容與 digest。
		{
			name: "receipt package identity",
			setup: func(t *testing.T, st *Store) {
				t.Helper()
				node, _ := storedCatalogFixture()
				if _, err := st.PublishCatalogManifest(node, "operator:test"); err != nil {
					t.Fatal(err)
				}
			},
			tamper: `UPDATE operator_idempotency SET response_json=` +
				`json_set(response_json,'$.package_id','node-runtime','$.package_version','24.15.0')`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := newTestStore(t)
			if test.setup != nil {
				test.setup(t, st)
			}
			req := operatorCatalogRequest(t, "catalog-tamper-"+test.name)
			if _, err := st.ApplyOperatorCatalogManifest(req, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(test.tamper); err != nil {
				t.Fatal(err)
			}
			result, err := st.ApplyOperatorCatalogManifest(req, func() error {
				t.Fatal("tampered replay verified external artifact")
				return nil
			})
			if !errors.Is(err, ErrCatalogManifestCacheInvalid) || !result.Audited {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestConcurrentOperatorCatalogManifestPublicationCreatesOneRecord(t *testing.T) {
	st := newTestStore(t)
	var fresh, existing, failed atomic.Int64
	var wait sync.WaitGroup
	for i := 0; i < 12; i++ {
		i := i
		wait.Add(1)
		go func() {
			defer wait.Done()
			req := operatorCatalogRequest(t, "catalog-concurrent-"+string(rune('a'+i)))
			result, err := st.ApplyOperatorCatalogManifest(req, func() error { return nil })
			if err != nil {
				failed.Add(1)
			} else if result.AlreadyPublished {
				existing.Add(1)
			} else {
				fresh.Add(1)
			}
		}()
	}
	wait.Wait()
	if fresh.Load() != 1 || existing.Load() != 11 || failed.Load() != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM catalog_manifests`) != 1 {
		t.Fatalf("fresh=%d existing=%d failed=%d", fresh.Load(), existing.Load(), failed.Load())
	}
}

func TestCatalogManifestIsKnownCanonicalAuditAction(t *testing.T) {
	if !IsKnownAuditAction(AuditCatalogManifest) || !IsCanonicalOperatorAction(AuditCatalogManifest) {
		t.Fatal("catalog manifest publication is missing from audit registries")
	}
}

func operatorMachineProfileRequest(t *testing.T, key string) OperatorMachineProfileRequest {
	t.Helper()
	_, openclaw := storedCatalogFixture()
	return OperatorMachineProfileRequest{
		Profile: appcatalog.MachineProfile{
			SchemaVersion: appcatalog.SchemaVersion, ID: "openclaw-standard", Revision: 1,
			Packages: []appcatalog.PackageRef{{PackageID: openclaw.ID, Version: openclaw.Version}},
		},
		PublishedBy: "user:test@example.com", Reason: "approve exact profile",
		IdempotencyKey: key, RequestDigest: sha256Digest([]byte("profile-request:" + key)),
		Audit: AuditEntry{
			SourceAddr: "100.64.0.10", AuthSubject: "user:test@example.com",
			AuthDecision: "authorized", SourceKind: "operator-api",
		},
	}
}

func publishMachineProfileCatalog(t *testing.T, st *Store) {
	t.Helper()
	node, openclaw := storedCatalogFixture()
	for _, manifest := range []appcatalog.Manifest{node, openclaw} {
		if _, err := st.PublishCatalogManifest(manifest, "operator:test"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOperatorMachineProfilePublishesReplaysAndAuditsAtomically(t *testing.T) {
	st := newTestStore(t)
	publishMachineProfileCatalog(t, st)
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	req := operatorMachineProfileRequest(t, "profile-publish-1")
	var verifyCalls atomic.Int64
	first, err := st.ApplyOperatorMachineProfile(req, func(profile appcatalog.MachineProfile) error {
		verifyCalls.Add(1)
		if profile.ID != req.Profile.ID {
			t.Fatalf("verified profile=%+v", profile)
		}
		return nil
	})
	if err != nil || first.Replayed || first.AlreadyPublished || !first.Audited ||
		first.Record.Profile.ID != req.Profile.ID || first.Record.PublishedBy != req.PublishedBy {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	now = now.Add(time.Minute)
	replay, err := st.ApplyOperatorMachineProfile(req, func(appcatalog.MachineProfile) error {
		t.Fatal("idempotent profile replay verified material again")
		return nil
	})
	if err != nil || !replay.Replayed || replay.AlreadyPublished || !replay.Audited ||
		replay.Record.Digest != first.Record.Digest || verifyCalls.Load() != 1 {
		t.Fatalf("replay=%+v calls=%d err=%v", replay, verifyCalls.Load(), err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM machine_profiles`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='machine-profile'`) != 2 {
		t.Fatal("profile publication ledgers are incomplete")
	}
}

func TestOperatorMachineProfileCachesGraphRejectionAndKeepsStateEmpty(t *testing.T) {
	st := newTestStore(t)
	req := operatorMachineProfileRequest(t, "profile-unresolvable")
	var calls atomic.Int64
	verify := func(appcatalog.MachineProfile) error {
		calls.Add(1)
		return operatorError(OperatorCodeMachineProfileUnresolvable, "private resolver detail")
	}
	_, err := st.ApplyOperatorMachineProfile(req, verify)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeMachineProfileUnresolvable ||
		rejection.Replayed || !rejection.Audited {
		t.Fatalf("first rejection=%+v err=%v", rejection, err)
	}
	_, err = st.ApplyOperatorMachineProfile(req, func(appcatalog.MachineProfile) error {
		t.Fatal("rejected profile replay verified material again")
		return nil
	})
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeMachineProfileUnresolvable ||
		!rejection.Replayed || calls.Load() != 1 {
		t.Fatalf("replay rejection=%+v calls=%d err=%v", rejection, calls.Load(), err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM machine_profiles`) != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 {
		t.Fatal("rejected profile changed immutable profile state")
	}
}

func TestOperatorMachineProfileConflictPrecedesMaterialVerification(t *testing.T) {
	st := newTestStore(t)
	publishMachineProfileCatalog(t, st)
	original := operatorMachineProfileRequest(t, "profile-original")
	if _, err := st.ApplyOperatorMachineProfile(original, func(appcatalog.MachineProfile) error { return nil }); err != nil {
		t.Fatal(err)
	}
	changed := operatorMachineProfileRequest(t, "profile-conflict")
	changed.Profile.Packages = []appcatalog.PackageRef{{PackageID: "node-runtime", Version: "24.15.0"}}
	_, err := st.ApplyOperatorMachineProfile(changed, func(appcatalog.MachineProfile) error {
		t.Fatal("immutable profile conflict reached material verification")
		return nil
	})
	if !errors.Is(err, ErrMachineProfileConflict) {
		t.Fatalf("conflict err=%v", err)
	}
}

func TestOperatorMachineProfileTransactionRevalidatesGraphAndRollsBackAuditFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *Store)
		want    error
	}{
		{
			name:    "graph missing at writer",
			prepare: func(t *testing.T, st *Store) {},
			want:    ErrMachineProfileUnresolvable,
		},
		{
			name: "audit rejected",
			prepare: func(t *testing.T, st *Store) {
				publishMachineProfileCatalog(t, st)
				if _, err := st.DB().Exec(`CREATE TRIGGER reject_profile_audit BEFORE INSERT ON audit_log
				 WHEN NEW.action='machine-profile' BEGIN SELECT RAISE(ABORT,'reject'); END`); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := newTestStore(t)
			test.prepare(t, st)
			req := operatorMachineProfileRequest(t, "profile-atomic-"+test.name)
			_, err := st.ApplyOperatorMachineProfile(req, func(appcatalog.MachineProfile) error { return nil })
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
			if test.want == nil && err == nil {
				t.Fatal("audit failure accepted profile")
			}
			if countRows(t, st, `SELECT COUNT(*) FROM machine_profiles`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != boolInt(test.want != nil) {
				t.Fatal("profile failure left incoherent state")
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestMachineProfileIsKnownCanonicalAuditAction(t *testing.T) {
	if !IsKnownAuditAction(AuditMachineProfile) || !IsCanonicalOperatorAction(AuditMachineProfile) {
		t.Fatal("machine profile publication is missing from audit registries")
	}
}

func TestConcurrentOperatorMachineProfilePublicationCreatesOneRecord(t *testing.T) {
	st := newTestStore(t)
	publishMachineProfileCatalog(t, st)
	base := operatorMachineProfileRequest(t, "profile-concurrent-base")
	var fresh, existing, failed atomic.Int64
	var wait sync.WaitGroup
	for i := 0; i < 12; i++ {
		i := i
		wait.Add(1)
		go func() {
			defer wait.Done()
			req := base
			req.IdempotencyKey = "profile-concurrent-" + string(rune('a'+i))
			req.RequestDigest = sha256Digest([]byte("profile-concurrent-body"))
			result, err := st.ApplyOperatorMachineProfile(req, func(appcatalog.MachineProfile) error { return nil })
			if err != nil {
				failed.Add(1)
			} else if result.AlreadyPublished {
				existing.Add(1)
			} else {
				fresh.Add(1)
			}
		}()
	}
	wait.Wait()
	if fresh.Load() != 1 || existing.Load() != 11 || failed.Load() != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM machine_profiles`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 12 {
		t.Fatalf("fresh=%d existing=%d failed=%d", fresh.Load(), existing.Load(), failed.Load())
	}
}

func TestOperatorMachineProfileDetectsReceiptAndLedgerTampering(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(t *testing.T, st *Store)
		tamper string
	}{
		{name: "receipt", tamper: `UPDATE operator_idempotency SET response_json='{}'`},
		{name: "profile", tamper: `UPDATE machine_profiles SET profile_digest='sha256:tampered'`},
		{name: "original audit", tamper: `DELETE FROM audit_log WHERE action='machine-profile'`},
		// 跟 catalog 同一條鏈：replay 用收據上的 profile id/revision 去查
		// machine_profiles，再把查到的那一列交給 operator。這裡的
		// success detail 只有 profile_digest 與 already_published，
		// 連 profile_id／revision 都不含，audit subject 又來自 request
		// 而不是收據，所以把身分換成另一份已發布的 profile 之後，
		// 只剩 machineProfileReceiptMatchesRecord 會發現 digest 與
		// published_by 對不上。id 與 revision 比了恆等——那兩項就是查詢鍵。
		{
			name: "receipt profile identity",
			setup: func(t *testing.T, st *Store) {
				t.Helper()
				profile := operatorMachineProfileRequest(t, "profile-tamper-setup").Profile
				profile.Revision = 2
				if _, err := st.PublishMachineProfile(profile, "operator:test"); err != nil {
					t.Fatal(err)
				}
			},
			tamper: `UPDATE operator_idempotency SET response_json=` +
				`json_set(response_json,'$.profile_revision',2)`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := newTestStore(t)
			publishMachineProfileCatalog(t, st)
			if test.setup != nil {
				test.setup(t, st)
			}
			req := operatorMachineProfileRequest(t, "profile-tamper-"+test.name)
			if _, err := st.ApplyOperatorMachineProfile(req, func(appcatalog.MachineProfile) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(test.tamper); err != nil {
				t.Fatal(err)
			}
			result, err := st.ApplyOperatorMachineProfile(req, func(appcatalog.MachineProfile) error {
				t.Fatal("tampered profile replay verified material")
				return nil
			})
			if !errors.Is(err, ErrMachineProfileCacheInvalid) || !result.Audited {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
