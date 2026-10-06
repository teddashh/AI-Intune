package store

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
)

func artifactFetchTestDigest(fill string) string {
	return "sha256:" + strings.Repeat(fill, 64)
}

func artifactFetchTestRequest(key, requestFill string) OperatorArtifactFetchRequest {
	return OperatorArtifactFetchRequest{
		Name: "openclaw", Version: "1.2.3",
		PreviewDigest: artifactFetchTestDigest("a"), IdempotencyKey: key,
		RequestDigest: artifactFetchTestDigest(requestFill), Reason: "operator requested exact package",
		Audit: AuditEntry{SourceAddr: "100.64.0.1:1234", AuthSubject: "operator-test"},
	}
}

func artifactFetchTestPrepared(req OperatorArtifactFetchRequest) ArtifactFetchPrepared {
	return ArtifactFetchPrepared{
		Name: req.Name, Version: req.Version, SourceKind: artifact.ArtifactSourceNPM,
		RegistryOrigin:  "https://registry.npmjs.org",
		TarballURL:      "https://registry.npmjs.org/openclaw/-/openclaw-1.2.3.tgz",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 64)),
		EnginesNode:     ">=22", MaxBytes: 1 << 20,
		CurrentPreviewDigest: req.PreviewDigest,
	}
}

func TestValidSingleLineText(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		maxBytes   int
		allowEmpty bool
		want       bool
	}{
		{name: "ordinary multi-byte text", value: "操作理由", maxBytes: 500, want: true},
		{name: "control rune", value: "reason\x1b[2J", maxBytes: 500},
		{name: "format rune", value: "reason\u202Etext", maxBytes: 500},
		{name: "line separator", value: "reason\u2028text", maxBytes: 500},
		{name: "paragraph separator", value: "reason\u2029text", maxBytes: 500},
		{name: "invalid UTF-8", value: string([]byte{'r', 0xff}), maxBytes: 500},
		{name: "untrimmed", value: " reason ", maxBytes: 500},
		{name: "empty", value: "", maxBytes: 500},
		{name: "empty allowed", value: "", maxBytes: 500, allowEmpty: true, want: true},
		{name: "over length", value: strings.Repeat("a", 501), maxBytes: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validSingleLineText(tt.value, tt.maxBytes, tt.allowEmpty); got != tt.want {
				t.Fatalf("validSingleLineText(%q, %d, %t) = %t, want %t",
					tt.value, tt.maxBytes, tt.allowEmpty, got, tt.want)
			}
		})
	}
}

func TestOperatorArtifactFetchPersistsPrivateNodeRuntimePlan(t *testing.T) {
	s := newTestStore(t)
	req := artifactFetchTestRequest("artifact-fetch-node-runtime", "d")
	req.Name, req.Version = "node-runtime", "24.21.0"
	sourcePlanBytes, err := json.Marshal(artifact.NodeRuntimeFetchPlan{
		PolicyVersion: artifact.NodeRuntimeFetchPolicyVersion, Name: req.Name, Version: req.Version,
		SourceOrigin: artifact.ProductionNodeDistributionOrigin,
		ChecksumURL:  artifact.ProductionNodeDistributionOrigin + "/dist/v24.21.0/SHASUMS256.txt",
		Sources: []artifact.NodeRuntimeSource{
			{TargetOS: "linux", TargetArch: "amd64", Filename: "node-v24.21.0-linux-x64.tar.gz", SHA256: strings.Repeat("a", 64)},
			{TargetOS: "linux", TargetArch: "arm64", Filename: "node-v24.21.0-linux-arm64.tar.gz", SHA256: strings.Repeat("b", 64)},
		},
		SourceIdentity: "source-identity", SourceMaxBytes: 1 << 20, BundleMaxBytes: 1 << 30,
		PreviewedAt: time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC), PreviewDigest: req.PreviewDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	sourcePlan := string(sourcePlanBytes)
	prepared := ArtifactFetchPrepared{
		Name: req.Name, Version: req.Version, SourceKind: artifact.ArtifactSourceNode,
		SourcePlan: sourcePlan, RegistryOrigin: artifact.ProductionNodeDistributionOrigin,
		TarballURL:      artifact.ProductionNodeDistributionOrigin + "/dist/v24.21.0/SHASUMS256.txt",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 64)),
		MaxBytes:        1 << 30, CurrentPreviewDigest: req.PreviewDigest,
	}
	created, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		return prepared, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Operation.SourceKind != artifact.ArtifactSourceNode || created.Operation.Name != req.Name ||
		created.Operation.EnginesNode != "" {
		t.Fatalf("node operation=%+v", created.Operation)
	}
	claim, err := s.ClaimArtifactFetchOperation(created.Operation.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if claim.SourcePlan != sourcePlan || claim.TarballURL != prepared.TarballURL {
		t.Fatalf("node claim did not preserve source plan: %+v", claim)
	}
	for label, value := range map[string]any{"operation": created.Operation, "claim": claim} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), sourcePlan) || strings.Contains(string(raw), "checksum_url") ||
			strings.Contains(string(raw), "source_plan") {
			t.Fatalf("%s exposed private source plan: %s", label, raw)
		}
	}
}

func TestOperatorArtifactFetchFreshReplayConflictAndAtomicity(t *testing.T) {
	t.Run("fresh and replay", func(t *testing.T) {
		s := newTestStore(t)
		now := time.Date(2026, 9, 8, 16, 0, 0, 789, time.FixedZone("offset", -4*60*60))
		s.nowFn = func() time.Time { return now }
		req := artifactFetchTestRequest("artifact-fetch-fresh", "b")
		prepared := artifactFetchTestPrepared(req)
		prepareCalls := 0

		fresh, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
			prepareCalls++
			return prepared, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		wantCreated := now.UTC().Truncate(time.Second)
		if prepareCalls != 1 || fresh.Replayed || !fresh.Audited || fresh.Operation.OperationID == "" ||
			fresh.Operation.State != ArtifactFetchQueued || fresh.Operation.Phase != ArtifactFetchPhaseQueued ||
			!fresh.Operation.CreatedAt.Equal(wantCreated) || fresh.Operation.CreatedAt.Location() != time.UTC {
			t.Fatalf("fresh=%+v prepare_calls=%d", fresh, prepareCalls)
		}
		var operations, receipts, audits int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM artifact_fetch_operations`).Scan(&operations); err != nil {
			t.Fatal(err)
		}
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
			req.IdempotencyKey).Scan(&receipts); err != nil {
			t.Fatal(err)
		}
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=? AND action=?`,
			req.IdempotencyKey, AuditArtifactFetch).Scan(&audits); err != nil {
			t.Fatal(err)
		}
		if operations != 1 || receipts != 1 || audits != 1 {
			t.Fatalf("operations=%d receipts=%d audits=%d", operations, receipts, audits)
		}

		now = now.Add(time.Second)
		replay, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
			prepareCalls++
			return ArtifactFetchPrepared{}, errors.New("prepare must not run on replay")
		})
		if err != nil {
			t.Fatal(err)
		}
		if prepareCalls != 1 || !replay.Replayed || !replay.Audited ||
			replay.Operation.OperationID != fresh.Operation.OperationID {
			t.Fatalf("replay=%+v prepare_calls=%d", replay, prepareCalls)
		}

		conflicting := req
		conflicting.RequestDigest = artifactFetchTestDigest("c")
		_, err = s.ApplyOperatorArtifactFetch(conflicting, func() (ArtifactFetchPrepared, error) {
			prepareCalls++
			return prepared, nil
		})
		var rejection *OperatorRequestError
		if !errors.As(err, &rejection) || rejection.Code != OperatorCodeIdempotencyConflict ||
			!rejection.Audited || prepareCalls != 1 {
			t.Fatalf("conflict=%+v err=%v prepare_calls=%d", rejection, err, prepareCalls)
		}
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=?`,
			req.IdempotencyKey).Scan(&audits); err != nil || audits != 3 {
			t.Fatalf("audits=%d err=%v", audits, err)
		}
	})

	t.Run("prepare runs without writer and enqueue rechecks global key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hub.db")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		peer, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer peer.Close()
		now := time.Date(2026, 9, 8, 20, 1, 0, 0, time.UTC)
		s.nowFn = func() time.Time { return now }
		req := artifactFetchTestRequest("artifact-fetch-recheck", "b")
		prepared := artifactFetchTestPrepared(req)
		_, err = s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
			_, insertErr := peer.DB().Exec(`INSERT INTO operator_idempotency
 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
 VALUES (?,?,?,'rejected','OTHER','other',?)`, req.IdempotencyKey,
				"another-operator-operation:v1", req.RequestDigest, fmtTime(now))
			return prepared, insertErr
		})
		if !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("writer recheck err=%v", err)
		}
		var operations int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM artifact_fetch_operations`).Scan(&operations); err != nil || operations != 0 {
			t.Fatalf("operations=%d err=%v", operations, err)
		}
	})

	t.Run("audit failure rolls back receipt and operation", func(t *testing.T) {
		s := newTestStore(t)
		now := time.Date(2026, 9, 8, 20, 2, 0, 0, time.UTC)
		s.nowFn = func() time.Time { return now }
		if _, err := s.DB().Exec(`CREATE TRIGGER reject_artifact_fetch_audit
 BEFORE INSERT ON audit_log WHEN NEW.action='artifact-fetch'
 BEGIN SELECT RAISE(ABORT, 'test audit failure'); END`); err != nil {
			t.Fatal(err)
		}
		req := artifactFetchTestRequest("artifact-fetch-atomic", "b")
		_, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
			return artifactFetchTestPrepared(req), nil
		})
		if err == nil || !strings.Contains(err.Error(), "record artifact fetch enqueue audit") {
			t.Fatalf("err=%v", err)
		}
		for _, table := range []string{"artifact_fetch_operations", "operator_idempotency", "audit_log"} {
			var count int
			if err := s.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("table=%s count=%d err=%v", table, count, err)
			}
		}
	})
}

func TestOperatorArtifactFetchFreshResponseIsTransactionQueuedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	peer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	var claimed ArtifactFetchClaim
	var claimErr error
	s.afterArtifactFetchEnqueueCommit = func(operationID string) {
		claimed, claimErr = peer.ClaimArtifactFetchOperation(operationID, false)
	}
	req := artifactFetchTestRequest("artifact-fetch-queued-snapshot", "b")
	fresh, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		return artifactFetchTestPrepared(req), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimErr != nil {
		t.Fatalf("racing worker claim: %v", claimErr)
	}
	if claimed.Operation.State != ArtifactFetchRunning {
		t.Fatalf("racing worker did not claim committed operation: %+v", claimed.Operation)
	}
	if fresh.Operation.State != ArtifactFetchQueued || fresh.Operation.Phase != ArtifactFetchPhaseQueued ||
		fresh.Operation.Attempt != 0 || fresh.Operation.StartedAt != nil {
		t.Fatalf("fresh response was not the enqueue transaction snapshot: %+v", fresh.Operation)
	}
	current, err := s.GetArtifactFetchOperation(fresh.Operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != ArtifactFetchRunning || current.Attempt != 1 {
		t.Fatalf("durable current operation did not retain worker claim: %+v", current)
	}
}

func TestOperatorArtifactFetchTransientAndDurableRejections(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 8, 21, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	req := artifactFetchTestRequest("artifact-fetch-transient", "b")
	_, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		return ArtifactFetchPrepared{}, errors.New("temporary registry outage with SECRET")
	})
	if !errors.Is(err, ErrArtifactFetchPrepareFailed) {
		t.Fatalf("transient err=%v", err)
	}
	for _, table := range []string{"artifact_fetch_operations", "operator_idempotency", "audit_log"} {
		var count int
		if err := s.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("transient table=%s count=%d err=%v", table, count, err)
		}
	}
	// The transient failure did not consume the key, so an identical retry may enqueue.
	if _, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		return artifactFetchTestPrepared(req), nil
	}); err != nil {
		t.Fatalf("retry after transient failure: %v", err)
	}

	rejectedReq := artifactFetchTestRequest("artifact-fetch-rejected", "c")
	_, err = s.ApplyOperatorArtifactFetch(rejectedReq, func() (ArtifactFetchPrepared, error) {
		return ArtifactFetchPrepared{}, &OperatorRequestError{Code: OperatorCodeArtifactFetchPreviewStale}
	})
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || !errors.Is(err, ErrArtifactFetchPreviewStale) ||
		rejection.Replayed || !rejection.Audited {
		t.Fatalf("fresh rejection=%+v err=%v", rejection, err)
	}
	now = now.Add(time.Second)
	prepareCalls := 0
	_, err = s.ApplyOperatorArtifactFetch(rejectedReq, func() (ArtifactFetchPrepared, error) {
		prepareCalls++
		return ArtifactFetchPrepared{}, nil
	})
	if !errors.As(err, &rejection) || !rejection.Replayed || !rejection.Audited ||
		!errors.Is(err, ErrArtifactFetchPreviewStale) || prepareCalls != 0 {
		t.Fatalf("replayed rejection=%+v err=%v prepare_calls=%d", rejection, err, prepareCalls)
	}
	var receipts, audits int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
		rejectedReq.IdempotencyKey).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=?`,
		rejectedReq.IdempotencyKey).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || audits != 2 {
		t.Fatalf("receipts=%d audits=%d", receipts, audits)
	}
}

func TestOperatorArtifactFetchRequiresCanonicalReasonBeforePrepare(t *testing.T) {
	for index, reason := range []string{"", "bad\nreason", " invisible\u200b"} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			s := newTestStore(t)
			req := artifactFetchTestRequest(fmt.Sprintf("artifact-fetch-reason-%d", index), "d")
			req.Reason = reason
			prepareCalls := 0
			for attempt := 0; attempt < 2; attempt++ {
				_, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
					prepareCalls++
					return artifactFetchTestPrepared(req), nil
				})
				var rejection *OperatorRequestError
				if !errors.As(err, &rejection) || rejection.Code != OperatorCodeArtifactFetchInvalid ||
					rejection.Replayed != (attempt == 1) {
					t.Fatalf("attempt=%d rejection=%+v err=%v", attempt, rejection, err)
				}
			}
			if prepareCalls != 0 {
				t.Fatalf("malformed reason reached prepare %d times", prepareCalls)
			}
			var receipts, audits, nonemptyReasons int
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
				req.IdempotencyKey).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if err := s.DB().QueryRow(`SELECT COUNT(*),COUNT(reason) FROM audit_log WHERE idempotency_key=?`,
				req.IdempotencyKey).Scan(&audits, &nonemptyReasons); err != nil {
				t.Fatal(err)
			}
			if receipts != 1 || audits != 2 || nonemptyReasons != 0 {
				t.Fatalf("receipts=%d audits=%d nonempty_reasons=%d", receipts, audits, nonemptyReasons)
			}
		})
	}
}

func TestOperatorArtifactFetchTarballURLAndSchemaUseCatalogByteBound(t *testing.T) {
	s := newTestStore(t)
	prefix := "https://registry.npmjs.org/"
	atLimit := prefix + strings.Repeat("a", artifact.MaxTarballURLBytes-len(prefix))
	if len(atLimit) != artifact.MaxTarballURLBytes || !validArtifactFetchTarballURL(atLimit) {
		t.Fatalf("store rejected %d-byte tarball URL", len(atLimit))
	}
	if validArtifactFetchTarballURL(atLimit + "a") {
		t.Fatalf("store accepted %d-byte tarball URL", len(atLimit)+1)
	}

	acceptedReq := artifactFetchTestRequest("artifact-fetch-url-limit", "b")
	accepted := artifactFetchTestPrepared(acceptedReq)
	accepted.TarballURL = atLimit
	result, err := s.ApplyOperatorArtifactFetch(acceptedReq, func() (ArtifactFetchPrepared, error) {
		return accepted, nil
	})
	if err != nil || result.Operation.State != ArtifactFetchQueued {
		t.Fatalf("exact-bound enqueue=%+v err=%v", result, err)
	}

	rejectedReq := artifactFetchTestRequest("artifact-fetch-url-over-limit", "c")
	rejected := artifactFetchTestPrepared(rejectedReq)
	rejected.TarballURL = atLimit + "a"
	_, err = s.ApplyOperatorArtifactFetch(rejectedReq, func() (ArtifactFetchPrepared, error) {
		return rejected, nil
	})
	if !errors.Is(err, ErrArtifactFetchInvalid) {
		t.Fatalf("over-bound enqueue error=%v", err)
	}

	var artifactFetchSchema string
	if err := s.DB().QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name='artifact_fetch_operations'`).
		Scan(&artifactFetchSchema); err != nil {
		t.Fatal(err)
	}
	wantConstraint := "length(CAST(tarball_url AS BLOB)) BETWEEN 1 AND 2048"
	if !strings.Contains(artifactFetchSchema, wantConstraint) || strings.Contains(artifactFetchSchema,
		"length(CAST(tarball_url AS BLOB)) BETWEEN 1 AND 4096") {
		t.Fatalf("artifact fetch schema does not enforce the shared URL bound: %s", artifactFetchSchema)
	}
}

func TestArtifactFetchSourceColumnsMigrateLegacyTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-artifact-fetch.db")
	legacySchema := strings.ReplaceAll(schemaSQL, "  source_kind      TEXT NOT NULL DEFAULT '',\n", "")
	legacySchema = strings.ReplaceAll(legacySchema, "  source_plan      TEXT NOT NULL DEFAULT '',\n", "")
	legacySchema = strings.ReplaceAll(legacySchema, "  CHECK (length(CAST(source_kind AS BLOB)) <= 64),\n", "")
	legacySchema = strings.ReplaceAll(legacySchema, "  CHECK (length(CAST(source_plan AS BLOB)) <= 8192),\n", "")
	if legacySchema == schemaSQL {
		t.Fatal("legacy schema fixture did not remove source columns")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	columns, err := columnSet(s.DB(), "artifact_fetch_operations")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"source_kind", "source_plan"} {
		if _, ok := columns[name]; !ok {
			t.Fatalf("migration did not add %s", name)
		}
	}
}

func TestOperatorArtifactFetchActiveOwnership(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 8, 22, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	firstReq := artifactFetchTestRequest("artifact-fetch-owner-1", "b")
	first, err := s.ApplyOperatorArtifactFetch(firstReq, func() (ArtifactFetchPrepared, error) {
		return artifactFetchTestPrepared(firstReq), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	secondReq := artifactFetchTestRequest("artifact-fetch-owner-2", "c")
	secondReq.PreviewDigest = artifactFetchTestDigest("d")
	secondPrepared := artifactFetchTestPrepared(secondReq)
	secondPrepared.TarballURL = "https://registry.npmjs.org/openclaw/-/different.tgz"
	_, err = s.ApplyOperatorArtifactFetch(secondReq, func() (ArtifactFetchPrepared, error) {
		return secondPrepared, nil
	})
	if !errors.Is(err, ErrArtifactFetchActive) {
		t.Fatalf("active err=%v", err)
	}
	var operations int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM artifact_fetch_operations`).Scan(&operations); err != nil || operations != 1 {
		t.Fatalf("operations=%d err=%v", operations, err)
	}
	if _, err := s.ClaimArtifactFetchOperation(first.Operation.OperationID, false); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorArtifactFetchRejectsCorruptReplayEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, s *Store, operationID, key string)
	}{
		{
			name: "noncanonical receipt",
			mutate: func(t *testing.T, s *Store, _, key string) {
				t.Helper()
				if _, err := s.DB().Exec(`UPDATE operator_idempotency SET response_json=response_json || ' '
 WHERE idempotency_key=?`, key); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "orphan receipt",
			mutate: func(t *testing.T, s *Store, operationID, _ string) {
				t.Helper()
				if _, err := s.DB().Exec(`DELETE FROM artifact_fetch_operations WHERE operation_id=?`, operationID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "immutable operation mismatch",
			mutate: func(t *testing.T, s *Store, operationID, _ string) {
				t.Helper()
				if _, err := s.DB().Exec(`UPDATE artifact_fetch_operations SET request_digest=? WHERE operation_id=?`,
					artifactFetchTestDigest("d"), operationID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing original audit",
			mutate: func(t *testing.T, s *Store, _, key string) {
				t.Helper()
				if _, err := s.DB().Exec(`DELETE FROM audit_log WHERE idempotency_key=?`, key); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			now := time.Date(2026, 9, 8, 23, 0, 0, 0, time.UTC)
			s.nowFn = func() time.Time { return now }
			req := artifactFetchTestRequest("artifact-fetch-corrupt-"+strings.ReplaceAll(tc.name, " ", "-"), "b")
			fresh, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
				return artifactFetchTestPrepared(req), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, s, fresh.Operation.OperationID, req.IdempotencyKey)
			now = now.Add(time.Second)
			prepareCalls := 0
			result, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
				prepareCalls++
				return artifactFetchTestPrepared(req), nil
			})
			if !errors.Is(err, ErrArtifactFetchCacheInvalid) || prepareCalls != 0 || result.Replayed || !result.Audited {
				t.Fatalf("result=%+v prepare_calls=%d err=%v", result, prepareCalls, err)
			}
			var subject, reason, outcome, detail string
			if err := s.DB().QueryRow(`SELECT subject,COALESCE(reason,''),outcome,COALESCE(detail,'')
 FROM audit_log WHERE idempotency_key=? ORDER BY audit_id DESC LIMIT 1`, req.IdempotencyKey).
				Scan(&subject, &reason, &outcome, &detail); err != nil {
				t.Fatal(err)
			}
			if subject != "artifact fetch idempotency cache" || reason != "" || outcome != "failed" ||
				detail != operatorArtifactFetchCacheInvalid {
				t.Fatalf("subject=%q reason=%q outcome=%q detail=%q", subject, reason, outcome, detail)
			}
		})
	}
}

// Identity digest 把 name／version／source／registry／tarball_url／sha512／
// engines_node／max_bytes 綁在同一個雜湊裡。Schema 只檢查 digest 的長度與
// 前綴，不會重算；因此只改其中一個成分而不動 digest，SQLite 仍會放行。
// 讀取端只有 validateArtifactFetchRecord 能發現；replay 另有
// artifactFetchReceiptMatchesRecord 後備，但 GET／list／claim 都沒有。
func TestArtifactFetchIntegrityDriftIsRefusedOnRead(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 8, 23, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	req := artifactFetchTestRequest("artifact-fetch-integrity-drift", "b")
	fresh, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		return artifactFetchTestPrepared(req), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetArtifactFetchOperation(fresh.Operation.OperationID); err != nil {
		t.Fatalf("honest record is unreadable, so a later refusal cannot be attributed to integrity drift: %v", err)
	}

	forged := "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5b}, 64))
	if _, err := s.DB().Exec(`UPDATE artifact_fetch_operations SET sha512_integrity=? WHERE operation_id=?`,
		forged, fresh.Operation.OperationID); err != nil {
		t.Fatal(err)
	}
	if !validArtifactFetchSHA512(forged) {
		t.Errorf("forged integrity is not format-valid; this tests format validation rather than "+
			"identity binding, so an operator could still read a record whose integrity and digest disagree: %q",
			forged)
	}

	_, err = s.GetArtifactFetchOperation(fresh.Operation.OperationID)
	if !errors.Is(err, ErrArtifactFetchCorrupt) {
		t.Errorf("GetArtifactFetchOperation err=%v, want %v; Hub would return 200 and give the operator "+
			"sha512_integrity that no longer matches the identity digest as approved provenance",
			err, ErrArtifactFetchCorrupt)
	}
}

func TestArtifactFetchStateMachineRestartReclaimAndSafeReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	req := artifactFetchTestRequest("artifact-fetch-state", "b")
	prepared := artifactFetchTestPrepared(req)
	fresh, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) { return prepared, nil })
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	claim, err := s.ClaimArtifactFetchOperation(fresh.Operation.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	firstStarted := *claim.Operation.StartedAt
	if claim.Operation.State != ArtifactFetchRunning || claim.Operation.Phase != ArtifactFetchPhaseDownloading ||
		claim.Operation.Attempt != 1 || claim.RunToken == "" || claim.TarballURL != prepared.TarballURL {
		t.Fatalf("claim=%+v", claim)
	}
	encodedClaim, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedClaim), prepared.TarballURL) || strings.Contains(string(encodedClaim), claim.RunToken) ||
		strings.Contains(string(encodedClaim), req.IdempotencyKey) {
		t.Fatalf("worker secret leaked through claim JSON: %s", encodedClaim)
	}
	if _, err := s.ClaimArtifactFetchOperation(fresh.Operation.OperationID, false); !errors.Is(err, ErrArtifactFetchInvalidState) {
		t.Fatalf("duplicate claim err=%v", err)
	}

	now = now.Add(time.Second)
	if _, err := s.AdvanceArtifactFetchOperation(fresh.Operation.OperationID, claim.RunToken,
		ArtifactFetchPhaseDownloading, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceArtifactFetchOperation(fresh.Operation.OperationID, claim.RunToken,
		ArtifactFetchPhaseDownloading, 99); !errors.Is(err, ErrArtifactFetchProgress) {
		t.Fatalf("progress regression err=%v", err)
	}
	now = now.Add(time.Second)
	if _, err := s.AdvanceArtifactFetchOperation(fresh.Operation.OperationID, claim.RunToken,
		ArtifactFetchPhaseVerifying, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceArtifactFetchOperation(fresh.Operation.OperationID, claim.RunToken,
		ArtifactFetchPhaseDownloading, 101); !errors.Is(err, ErrArtifactFetchProgress) {
		t.Fatalf("phase regression err=%v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now = now.Add(time.Second)
	s.nowFn = func() time.Time { return now }
	running, err := s.ListArtifactFetchOperations(ArtifactFetchListRequest{State: ArtifactFetchRunning})
	if err != nil || running.Total != 1 || len(running.Items) != 1 ||
		running.Items[0].OperationID != fresh.Operation.OperationID {
		t.Fatalf("startup running list=%+v err=%v", running, err)
	}
	reclaimed, err := s.ClaimArtifactFetchOperation(fresh.Operation.OperationID, true)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.RunToken == claim.RunToken || reclaimed.Operation.Attempt != 2 ||
		reclaimed.Operation.Phase != ArtifactFetchPhaseVerifying || reclaimed.Operation.ProgressBytes != 100 ||
		reclaimed.Operation.StartedAt == nil || !reclaimed.Operation.StartedAt.Equal(firstStarted) {
		t.Fatalf("reclaimed=%+v", reclaimed)
	}
	if _, err := s.AdvanceArtifactFetchOperation(fresh.Operation.OperationID, claim.RunToken,
		ArtifactFetchPhasePublishing, 100); !errors.Is(err, ErrArtifactFetchClaimLost) {
		t.Fatalf("stale worker err=%v", err)
	}
	now = now.Add(time.Second)
	progressed, err := s.AdvanceArtifactFetchOperation(fresh.Operation.OperationID, reclaimed.RunToken,
		ArtifactFetchPhasePublishing, 500)
	if err != nil || progressed.ProgressBytes != 500 {
		t.Fatalf("progressed=%+v err=%v", progressed, err)
	}
	now = now.Add(time.Second)
	succeeded, err := s.SucceedArtifactFetchOperation(fresh.Operation.OperationID, reclaimed.RunToken,
		strings.Repeat("e", 64), 500)
	if err != nil {
		t.Fatal(err)
	}
	if succeeded.State != ArtifactFetchSucceeded || succeeded.Phase != ArtifactFetchPhaseComplete ||
		succeeded.ResultSHA256 == nil || succeeded.ResultSizeBytes == nil || succeeded.FinishedAt == nil ||
		!succeeded.FinishedAt.Equal(succeeded.UpdatedAt) {
		t.Fatalf("succeeded=%+v", succeeded)
	}
	if _, err := s.FailArtifactFetchOperation(fresh.Operation.OperationID, reclaimed.RunToken,
		"LATE_FAILURE", "must not change terminal state"); !errors.Is(err, ErrArtifactFetchInvalidState) {
		t.Fatalf("terminal failure err=%v", err)
	}

	// A replay returns the same durable operation identity and its current state.
	now = now.Add(time.Second)
	prepareCalls := 0
	replay, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		prepareCalls++
		return prepared, nil
	})
	if err != nil || !replay.Replayed || replay.Operation.OperationID != fresh.Operation.OperationID ||
		replay.Operation.State != ArtifactFetchSucceeded || prepareCalls != 0 {
		t.Fatalf("terminal replay=%+v calls=%d err=%v", replay, prepareCalls, err)
	}

	// Terminal rows release active ownership; a new explicit key may fetch again.
	secondReq := artifactFetchTestRequest("artifact-fetch-state-second", "c")
	secondPrepared := artifactFetchTestPrepared(secondReq)
	second, err := s.ApplyOperatorArtifactFetch(secondReq, func() (ArtifactFetchPrepared, error) {
		return secondPrepared, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	secondClaim, err := s.ClaimArtifactFetchOperation(second.Operation.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	failed, err := s.FailArtifactFetchOperation(second.Operation.OperationID, secondClaim.RunToken,
		"DOWNLOAD_FAILED", "registry closed the response")
	if err != nil || failed.State != ArtifactFetchFailed || failed.ErrorCode == nil ||
		*failed.ErrorCode != "DOWNLOAD_FAILED" || failed.ErrorDetail == nil || failed.FinishedAt == nil {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}

	detail, err := s.GetArtifactFetchOperation(second.Operation.OperationID)
	if err != nil || detail.OperationID != second.Operation.OperationID || detail.State != ArtifactFetchFailed {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	listed, err := s.ListArtifactFetchOperations(ArtifactFetchListRequest{
		State: ArtifactFetchFailed, Name: "openclaw", Version: "1.2.3", Limit: 10,
	})
	if err != nil || listed.SchemaVersion != ArtifactFetchReadSchemaVersion ||
		listed.Consistency != ArtifactFetchReadConsistency || listed.Total != 1 || len(listed.Items) != 1 {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	safeJSON, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secondPrepared.TarballURL, secondClaim.RunToken,
		secondReq.IdempotencyKey, secondReq.RequestDigest, "source_addr"} {
		if strings.Contains(string(safeJSON), forbidden) {
			t.Fatalf("safe list leaked %q: %s", forbidden, safeJSON)
		}
	}
	if _, err := s.GetArtifactFetchOperation("missing"); !errors.Is(err, ErrArtifactFetchNotFound) {
		t.Fatalf("missing detail err=%v", err)
	}
	if _, err := s.ListArtifactFetchOperations(ArtifactFetchListRequest{Limit: MaxArtifactFetchReadLimit + 1}); !errors.Is(err, ErrArtifactFetchInvalid) {
		t.Fatalf("invalid list err=%v", err)
	}
}

func TestArtifactFetchListCountAndItemsShareReadSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	peer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	req := artifactFetchTestRequest("artifact-fetch-list-snapshot", "b")
	fresh, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		return artifactFetchTestPrepared(req), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	counted := make(chan struct{})
	resume := make(chan struct{})
	s.afterArtifactFetchListCount = func() {
		close(counted)
		<-resume
	}
	type listResult struct {
		result ArtifactFetchListResult
		err    error
	}
	done := make(chan listResult, 1)
	go func() {
		result, err := s.ListArtifactFetchOperations(ArtifactFetchListRequest{
			State: ArtifactFetchQueued, Limit: 10,
		})
		done <- listResult{result: result, err: err}
	}()
	select {
	case <-counted:
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("list did not reach count boundary")
	}
	claimed, claimErr := peer.ClaimArtifactFetchOperation(fresh.Operation.OperationID, false)
	close(resume)
	listed := <-done
	if claimErr != nil {
		t.Fatalf("concurrent claim: %v", claimErr)
	}
	if claimed.Operation.State != ArtifactFetchRunning {
		t.Fatalf("concurrent claim=%+v", claimed.Operation)
	}
	if listed.err != nil || listed.result.Total != 1 || len(listed.result.Items) != 1 ||
		listed.result.Items[0].OperationID != fresh.Operation.OperationID ||
		listed.result.Items[0].State != ArtifactFetchQueued {
		t.Fatalf("split artifact fetch list snapshot=%+v err=%v", listed.result, listed.err)
	}
}

func TestArtifactFetchWorkerQueueIsOldestFirstAcrossLimit(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	operationIDs := make([]string, 0, MaxArtifactFetchReadLimit+1)
	for index := 0; index <= MaxArtifactFetchReadLimit; index++ {
		req := artifactFetchTestRequest(fmt.Sprintf("artifact-fetch-order-%03d", index), "b")
		req.Version = fmt.Sprintf("1.2.%d", index)
		prepared := artifactFetchTestPrepared(req)
		prepared.Version = req.Version
		prepared.TarballURL = fmt.Sprintf("https://registry.npmjs.org/openclaw/-/openclaw-1.2.%d.tgz", index)
		fresh, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
			return prepared, nil
		})
		if err != nil {
			t.Fatalf("enqueue operation %d: %v", index, err)
		}
		operationIDs = append(operationIDs, fresh.Operation.OperationID)
		now = now.Add(time.Second)
	}

	firstPage, err := s.ListArtifactFetchOperationIDsForWorker(
		ArtifactFetchQueued, MaxArtifactFetchReadLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstPage) != MaxArtifactFetchReadLimit {
		t.Fatalf("worker page length=%d", len(firstPage))
	}
	for index, operationID := range firstPage {
		if operationID != operationIDs[index] {
			t.Fatalf("worker page[%d]=%s, want oldest operation %s", index, operationID, operationIDs[index])
		}
	}
	for _, operationID := range firstPage {
		if operationID == operationIDs[MaxArtifactFetchReadLimit] {
			t.Fatal("newest operation displaced an older intent from the bounded worker page")
		}
	}
}

func TestFenceRunningArtifactFetchOperationsRevokesOldTokensWithoutLosingProgress(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 9, 3, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	req := artifactFetchTestRequest("artifact-fetch-startup-fence", "b")
	fresh, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		return artifactFetchTestPrepared(req), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	oldClaim, err := s.ClaimArtifactFetchOperation(fresh.Operation.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	advanced, err := s.AdvanceArtifactFetchOperation(fresh.Operation.OperationID, oldClaim.RunToken,
		ArtifactFetchPhaseVerifying, 123)
	if err != nil {
		t.Fatal(err)
	}

	fenced, err := s.FenceRunningArtifactFetchOperations()
	if err != nil || fenced != 1 {
		t.Fatalf("startup fence=%d, %v", fenced, err)
	}
	if _, err := s.AdvanceArtifactFetchOperation(fresh.Operation.OperationID, oldClaim.RunToken,
		ArtifactFetchPhasePublishing, 123); !errors.Is(err, ErrArtifactFetchClaimLost) {
		t.Fatalf("old startup token was not fenced: %v", err)
	}
	current, err := s.GetArtifactFetchOperation(fresh.Operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != ArtifactFetchRunning || current.Attempt != advanced.Attempt ||
		current.Phase != advanced.Phase || current.ProgressBytes != advanced.ProgressBytes ||
		current.StartedAt == nil || !current.StartedAt.Equal(*advanced.StartedAt) {
		t.Fatalf("startup fence discarded durable progress: before=%+v after=%+v", advanced, current)
	}
	now = now.Add(time.Second)
	reclaimed, err := s.ClaimArtifactFetchOperation(fresh.Operation.OperationID, true)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.Operation.Attempt != oldClaim.Operation.Attempt+1 || reclaimed.RunToken == oldClaim.RunToken {
		t.Fatalf("READY-time reclaim=%+v", reclaimed)
	}
}
