package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

var operatorDeploymentTestPreviewDigest = "sha256:" + strings.Repeat("a", 64)

func TestOperatorDeploymentReplayRejectsForgedOrOrphanedSuccessReceipt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mutate       func(t *testing.T, raw string) string
		forgeRequest func(*OperatorDeploymentRequest)
		corrupt      func(t *testing.T, s *Store, result OperatorDeploymentResult)
		deleteAudit  bool
	}{
		{
			name: "duplicate canonical field",
			mutate: func(t *testing.T, raw string) string {
				t.Helper()
				return strings.Replace(raw, `"version":"v1"`, `"version":"v1","version":"v1"`, 1)
			},
		},
		{
			name: "forged immutable channel",
			mutate: func(t *testing.T, raw string) string {
				t.Helper()
				var receipt operatorDeploymentReceipt
				if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.Deployment.Channel = "FORGED_SECRET_MARKER"
				forged, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				return string(forged)
			},
		},
		// Replay 直接把 cache 裡的收據投影給 operator，不重讀帳本。
		// batch_size 是這份收據裡唯一「operator 看得到、又沒有鄰居在對帳」
		// 的欄位：validOperatorDeploymentReceipt 只查 1..Max 與
		// len(jobs) <= BatchSize，success detail 字串不含它，
		// deployment_targets／audit_log 也沒有這個值。把 1 改成 2 仍然
		// 全部合法，只有 sameOperatorDeploymentImmutable 會發現它跟
		// deployments.batch_size 不一致。上面那個 channel case 其實是被
		// ConfirmChannel 擋下的，走不到這道比對。
		{
			name: "forged receipt batch size",
			mutate: func(t *testing.T, raw string) string {
				t.Helper()
				var receipt operatorDeploymentReceipt
				if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.Deployment.BatchSize = 2
				forged, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				return string(forged)
			},
		},
		{
			name: "forged job identity",
			mutate: func(t *testing.T, raw string) string {
				t.Helper()
				var receipt operatorDeploymentReceipt
				if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.Jobs[0].JobID = "FORGED_SECRET_MARKER"
				forged, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				return string(forged)
			},
		},
		{
			name: "zero opened batch",
			mutate: func(t *testing.T, raw string) string {
				t.Helper()
				var receipt operatorDeploymentReceipt
				if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.OpenedBatch = 0
				forged, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				return string(forged)
			},
		},
		{
			name: "noncanonical receipt preview digest",
			mutate: func(t *testing.T, raw string) string {
				t.Helper()
				var receipt operatorDeploymentReceipt
				if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.PreviewDigest = "sha256:" + strings.Repeat("A", 64)
				forged, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				return string(forged)
			},
		},
		{
			name: "noncanonical request preview digest",
			forgeRequest: func(req *OperatorDeploymentRequest) {
				req.PreviewDigest = "sha256:" + strings.Repeat("A", 64)
			},
		},
		{
			name: "active orphan for deployment desired state",
			corrupt: func(t *testing.T, s *Store, result OperatorDeploymentResult) {
				t.Helper()
				job := result.Jobs[0]
				if _, err := s.DB().Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,artifact_digest,irreversible,execution_timeout)
 VALUES (?,?,?,?,?,?,?,?,?)`, newID(), job.MachineID, result.Deployment.DesiredID,
					result.Deployment.Revision, deploy.Running, fmtTime(result.Deployment.CreatedAt),
					job.ArtifactDigest, job.Irreversible, job.ExecutionTimeout); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "stored linked job material mismatch",
			corrupt: func(t *testing.T, s *Store, result OperatorDeploymentResult) {
				t.Helper()
				if _, err := s.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`,
					"sha256:"+strings.Repeat("b", 64), result.Jobs[0].JobID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{name: "missing original success audit", deleteAudit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 8, 13, 0, 0, 0, time.UTC)
			s.nowFn = func() time.Time { return now }
			addRolloutMachine(t, s, "replay-target", "replay-target", true)
			req := OperatorDeploymentRequest{
				PreviewDigest: operatorDeploymentTestPreviewDigest, IdempotencyKey: "deployment-replay-integrity",
				ConfirmChannel: "canary", ConfirmVersion: "2026.9.2",
				RequestDigest: "sha256:replay-body", Audit: AuditEntry{SourceAddr: "test", Reason: "roll out"},
			}
			prepared := OperatorDeploymentPrepared{
				CurrentPreviewDigest: req.PreviewDigest,
				Deployment: NewDeployment{
					Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
					Spec: rolloutTestOpenClawSpec, BatchSize: 1, CreatedBy: "operator-test",
					Targets: []NewDeploymentTarget{{MachineID: "replay-target", BatchNo: 1}},
				},
			}
			first, err := s.ApplyOperatorDeploymentCreate(req, func() (OperatorDeploymentPrepared, error) {
				return prepared, nil
			})
			if err != nil || first.Deployment.DeploymentID == "" {
				t.Fatalf("fresh=%+v err=%v", first, err)
			}
			if tc.corrupt != nil {
				tc.corrupt(t, s, first)
			} else if tc.deleteAudit {
				if _, err := s.DB().Exec(`DELETE FROM audit_log WHERE idempotency_key=?`, req.IdempotencyKey); err != nil {
					t.Fatal(err)
				}
			} else if tc.mutate != nil {
				var raw string
				if err := s.DB().QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
					req.IdempotencyKey).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB().Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
					tc.mutate(t, raw), req.IdempotencyKey); err != nil {
					t.Fatal(err)
				}
			}

			replayReq := req
			if tc.forgeRequest != nil {
				tc.forgeRequest(&replayReq)
			}
			prepareCalls := 0
			result, err := s.ApplyOperatorDeploymentCreate(replayReq, func() (OperatorDeploymentPrepared, error) {
				prepareCalls++
				return prepared, nil
			})
			if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") ||
				result.Replayed || prepareCalls != 0 {
				t.Fatalf("invalid replay result=%+v calls=%d err=%v", result, prepareCalls, err)
			}
			var subject, reason, outcome, detail string
			if err := s.DB().QueryRow(`SELECT subject,COALESCE(reason,''),outcome,COALESCE(detail,'')
				 FROM audit_log WHERE idempotency_key=? ORDER BY audit_id DESC LIMIT 1`, req.IdempotencyKey).
				Scan(&subject, &reason, &outcome, &detail); err != nil {
				t.Fatal(err)
			}
			if subject != "deployment idempotency cache" || reason != "" || outcome != "failed" ||
				detail != operatorDeploymentCacheInvalid || strings.Contains(detail, "FORGED_SECRET_MARKER") {
				t.Fatalf("invalid cache audit subject=%q reason=%q outcome=%q detail=%q", subject, reason, outcome, detail)
			}
			var successfulReplays int
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=?
				 AND outcome='ok' AND detail LIKE ?`, req.IdempotencyKey,
				OperatorIdempotencyReplayPrefix+"%").Scan(&successfulReplays); err != nil || successfulReplays != 0 {
				t.Fatalf("successful replay audits=%d err=%v", successfulReplays, err)
			}
		})
	}
}

func TestOperatorDeploymentReplayRequiresOriginalRejectionAudit(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 8, 13, 30, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	req := OperatorDeploymentRequest{
		PreviewDigest: operatorDeploymentTestPreviewDigest, IdempotencyKey: "deployment-rejection-evidence",
		ConfirmChannel: "canary", ConfirmVersion: "2026.9.2",
		RequestDigest: "sha256:rejected-body", Audit: AuditEntry{SourceAddr: "test", Reason: "reject me"},
	}
	_, err := s.ApplyOperatorDeploymentCreate(req, func() (OperatorDeploymentPrepared, error) {
		return OperatorDeploymentPrepared{}, &OperatorRequestError{Code: OperatorCodeDeploymentPreviewStale}
	})
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeDeploymentPreviewStale {
		t.Fatalf("fresh rejection=%+v err=%v", rejection, err)
	}
	if _, err := s.DB().Exec(`DELETE FROM audit_log WHERE idempotency_key=?`, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	prepareCalls := 0
	_, err = s.ApplyOperatorDeploymentCreate(req, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") || prepareCalls != 0 {
		t.Fatalf("orphan rejection calls=%d err=%v", prepareCalls, err)
	}
	var outcome, detail, reason string
	if err := s.DB().QueryRow(`SELECT outcome,COALESCE(detail,''),COALESCE(reason,'')
	 FROM audit_log WHERE idempotency_key=? ORDER BY audit_id DESC LIMIT 1`, req.IdempotencyKey).
		Scan(&outcome, &detail, &reason); err != nil {
		t.Fatal(err)
	}
	if outcome != "failed" || detail != operatorDeploymentCacheInvalid || reason != "" {
		t.Fatalf("invalid rejection cache audit outcome=%q detail=%q reason=%q", outcome, detail, reason)
	}
}

func TestOperatorDeploymentRetryReplayValidatesEntireParentLineage(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 8, 13, 40, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	addRolloutMachine(t, s, "lineage-target", "lineage-target", true)
	root, rootJobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec: rolloutTestOpenClawSpec, BatchSize: 1, CreatedBy: "lineage-root",
		Targets: []NewDeploymentTarget{{MachineID: "lineage-target", BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	setRolloutJobTerminal(t, s, rootJobs[0], deploy.Failed)
	if changed, err := s.SetDeploymentState(root.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
		t.Fatalf("pause root changed=%t err=%v", changed, err)
	}
	root, err = s.Deployment(root.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	parent, parentJobs, err := s.CreateDeployment(retryFixtureInput(root, "lineage-target"))
	if err != nil {
		t.Fatal(err)
	}
	setRolloutJobTerminal(t, s, parentJobs[0], deploy.Failed)
	if changed, err := s.SetDeploymentState(parent.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
		t.Fatalf("pause parent changed=%t err=%v", changed, err)
	}
	parent, err = s.Deployment(parent.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	revision, opened := parent.ControlRevision, 1
	req := OperatorDeploymentRequest{
		DeploymentID: parent.DeploymentID, PreviewDigest: operatorDeploymentTestPreviewDigest,
		ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
		ConfirmChannel: "canary", ConfirmVersion: "2026.9.2",
		IdempotencyKey: "retry-lineage-replay", RequestDigest: "sha256:retry-lineage-body",
		Audit: AuditEntry{SourceAddr: "test", Reason: "retry lineage"},
	}
	prepared := OperatorDeploymentPrepared{
		CurrentPreviewDigest: req.PreviewDigest, TerminalFailureMachineIDs: []string{"lineage-target"},
		Deployment: retryFixtureInput(parent, "lineage-target"),
	}
	first, err := s.ApplyOperatorDeploymentRetry(req, func() (OperatorDeploymentPrepared, error) {
		return prepared, nil
	})
	if err != nil || first.Deployment.RetryOf != parent.DeploymentID {
		t.Fatalf("fresh retry=%+v err=%v", first, err)
	}

	rootJob := rootJobs[0]
	if _, err := s.DB().Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,artifact_digest,irreversible,execution_timeout)
 VALUES (?,?,?,?,?,?,?,?,?)`, newID(), rootJob.MachineID, root.DesiredID, root.Revision,
		deploy.Running, fmtTime(now), rootJob.ArtifactDigest, rootJob.Irreversible, rootJob.ExecutionTimeout); err != nil {
		t.Fatal(err)
	}
	prepareCalls := 0
	replay, err := s.ApplyOperatorDeploymentRetry(req, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{}, errors.New("invalid replay touched current state")
	})
	if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") ||
		replay.Replayed || prepareCalls != 0 {
		t.Fatalf("orphaned ancestor replay=%+v calls=%d err=%v", replay, prepareCalls, err)
	}
	var replaySuccesses int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=?
 AND outcome='ok' AND detail LIKE ?`, req.IdempotencyKey,
		OperatorIdempotencyReplayPrefix+"%").Scan(&replaySuccesses); err != nil || replaySuccesses != 0 {
		t.Fatalf("successful replay audits=%d err=%v", replaySuccesses, err)
	}
	var subject, outcome, detail string
	if err := s.DB().QueryRow(`SELECT subject,outcome,COALESCE(detail,'') FROM audit_log
 WHERE idempotency_key=? ORDER BY audit_id DESC LIMIT 1`, req.IdempotencyKey).
		Scan(&subject, &outcome, &detail); err != nil {
		t.Fatal(err)
	}
	if subject != "deployment idempotency cache" || outcome != "failed" || detail != operatorDeploymentCacheInvalid {
		t.Fatalf("invalid lineage audit subject=%q outcome=%q detail=%q", subject, outcome, detail)
	}
}

func TestApplyOperatorDeploymentRejectsZeroExpectedOpenedBatchBeforePrepare(t *testing.T) {
	s := rolloutStore(t)
	revision, opened := int64(0), 0
	req := OperatorDeploymentRequest{
		DeploymentID: "zero-opened-deployment", PreviewDigest: operatorDeploymentTestPreviewDigest,
		ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
		ConfirmChannel: "canary",
		IdempotencyKey: "zero-opened-batch", RequestDigest: "sha256:zero-opened-body",
		Audit: AuditEntry{SourceAddr: "test"},
	}
	prepareCalls := 0
	apply := func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{}, errors.New("zero opened batch reached prepare")
	}
	_, err := s.ApplyOperatorDeploymentContinue(req, apply)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeDeploymentPreconditionFailed ||
		!rejection.Audited || rejection.Replayed || prepareCalls != 0 {
		t.Fatalf("fresh zero rejection=%+v calls=%d err=%v", rejection, prepareCalls, err)
	}
	_, err = s.ApplyOperatorDeploymentContinue(req, apply)
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeDeploymentPreconditionFailed ||
		!rejection.Audited || !rejection.Replayed || prepareCalls != 0 {
		t.Fatalf("replayed zero rejection=%+v calls=%d err=%v", rejection, prepareCalls, err)
	}
}

func TestApplyOperatorDeploymentRejectsNoncanonicalPreviewDigestBeforePrepare(t *testing.T) {
	for _, previewDigest := range []string{"sha256:words", "sha256:" + strings.Repeat("A", 64), " SHA256:" + strings.Repeat("a", 64)} {
		t.Run(previewDigest[:min(len(previewDigest), 16)], func(t *testing.T) {
			s := rolloutStore(t)
			req := OperatorDeploymentRequest{
				PreviewDigest: previewDigest, IdempotencyKey: "bad-preview-" + newID(),
				ConfirmChannel: "canary", ConfirmVersion: "2026.9.2",
				RequestDigest: "sha256:bad-preview-body", Audit: AuditEntry{SourceAddr: "test"},
			}
			prepareCalls := 0
			_, err := s.ApplyOperatorDeploymentCreate(req, func() (OperatorDeploymentPrepared, error) {
				prepareCalls++
				return OperatorDeploymentPrepared{}, errors.New("bad preview reached prepare")
			})
			var rejection *OperatorRequestError
			if !errors.As(err, &rejection) || rejection.Code != OperatorCodePreviewRequired || prepareCalls != 0 {
				t.Fatalf("digest=%q rejection=%+v calls=%d err=%v", previewDigest, rejection, prepareCalls, err)
			}
		})
	}
}

func TestOperatorDeploymentReplayRejectsForgedTerminalLifecycleTimes(t *testing.T) {
	for _, action := range []operatorDeploymentAction{operatorDeploymentContinue, operatorDeploymentAbandon} {
		t.Run(string(action), func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 8, 13, 45, 0, 0, time.UTC)
			s.nowFn = func() time.Time { return now }
			d, jobs := createBatchGuardFixture(t, s, 1)
			setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
			if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
				t.Fatalf("pause changed=%t err=%v", changed, err)
			}
			d, err := s.Deployment(d.DeploymentID)
			if err != nil {
				t.Fatal(err)
			}
			revision, opened := d.ControlRevision, 1
			req := OperatorDeploymentRequest{
				DeploymentID: d.DeploymentID, PreviewDigest: operatorDeploymentTestPreviewDigest,
				ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
				ConfirmChannel:      "canary",
				ConfirmDeploymentID: d.DeploymentID, IdempotencyKey: "terminal-lifecycle-" + string(action),
				RequestDigest: "sha256:terminal-lifecycle-body", Audit: AuditEntry{SourceAddr: "test"},
			}
			prepare := func() (OperatorDeploymentPrepared, error) {
				return OperatorDeploymentPrepared{CurrentPreviewDigest: req.PreviewDigest}, nil
			}
			var first OperatorDeploymentResult
			switch action {
			case operatorDeploymentContinue:
				first, err = s.ApplyOperatorDeploymentContinue(req, prepare)
			case operatorDeploymentAbandon:
				first, err = s.ApplyOperatorDeploymentAbandon(req, prepare)
			}
			if err != nil || first.Deployment.State != DeploymentFinished || first.Deployment.FinishedAt == nil {
				t.Fatalf("fresh terminal result=%+v err=%v", first, err)
			}
			var raw string
			if err := s.DB().QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
				req.IdempotencyKey).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var receipt operatorDeploymentReceipt
			if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
				t.Fatal(err)
			}
			forgedFinished := time.Date(2099, 1, 2, 3, 4, 6, 0, time.UTC)
			receipt.Deployment.FinishedAt = &forgedFinished
			if action == operatorDeploymentAbandon {
				forgedPaused := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
				receipt.Deployment.PausedAt = &forgedPaused
			}
			forged, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
				string(forged), req.IdempotencyKey); err != nil {
				t.Fatal(err)
			}
			prepareCalls := 0
			prepareAgain := func() (OperatorDeploymentPrepared, error) {
				prepareCalls++
				return OperatorDeploymentPrepared{}, errors.New("invalid replay touched state")
			}
			var replay OperatorDeploymentResult
			switch action {
			case operatorDeploymentContinue:
				replay, err = s.ApplyOperatorDeploymentContinue(req, prepareAgain)
			case operatorDeploymentAbandon:
				replay, err = s.ApplyOperatorDeploymentAbandon(req, prepareAgain)
			}
			if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") ||
				replay.Replayed || prepareCalls != 0 {
				t.Fatalf("forged terminal replay=%+v calls=%d err=%v", replay, prepareCalls, err)
			}
			stored, err := s.Deployment(d.DeploymentID)
			if err != nil || stored.FinishedAt == nil || !stored.FinishedAt.Equal(*first.Deployment.FinishedAt) {
				t.Fatalf("durable lifecycle changed stored=%+v first=%+v err=%v", stored, first.Deployment, err)
			}
		})
	}
}

func TestApplyOperatorDeploymentCreateFreshReplayConflictAndRejectionReplay(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	addRolloutMachine(t, s, "create-target", "create-target", true)
	prepared := OperatorDeploymentPrepared{
		CurrentPreviewDigest: operatorDeploymentTestPreviewDigest,
		Deployment: NewDeployment{
			Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
			Spec: rolloutTestOpenClawSpec, BatchSize: 1, CreatedBy: "operator-test",
			Targets: []NewDeploymentTarget{{MachineID: "create-target", BatchNo: 1}},
		},
	}
	req := OperatorDeploymentRequest{
		PreviewDigest: prepared.CurrentPreviewDigest, IdempotencyKey: "deployment-create-key",
		ConfirmChannel: "canary", ConfirmVersion: "2026.9.2",
		RequestDigest: "sha256:create-body", Audit: AuditEntry{SourceAddr: "test", Reason: "roll out"},
	}
	prepareCalls := 0
	prepare := func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return prepared, nil
	}
	first, err := s.ApplyOperatorDeploymentCreate(req, prepare)
	if err != nil || first.Replayed || !first.Audited || first.Deployment.DeploymentID == "" ||
		first.Deployment.ControlRevision != 0 || first.OpenedBatch != 1 || len(first.Jobs) != 1 {
		t.Fatalf("fresh result=%+v err=%v", first, err)
	}
	forged := req
	forged.ConfirmChannel = "stable"
	forgedCalls := 0
	forgedReplay, err := s.ApplyOperatorDeploymentCreate(forged, func() (OperatorDeploymentPrepared, error) {
		forgedCalls++
		return OperatorDeploymentPrepared{}, errors.New("invalid cached confirmation reached prepare")
	})
	if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") ||
		forgedReplay.Replayed || forgedCalls != 0 {
		t.Fatalf("forged create replay=%+v calls=%d err=%v", forgedReplay, forgedCalls, err)
	}
	var subject, outcome, detail string
	if err := s.DB().QueryRow(`SELECT subject,outcome,COALESCE(detail,'') FROM audit_log
 WHERE idempotency_key=? ORDER BY audit_id DESC LIMIT 1`, req.IdempotencyKey).
		Scan(&subject, &outcome, &detail); err != nil {
		t.Fatal(err)
	}
	if subject != "deployment idempotency cache" || outcome != "failed" || detail != operatorDeploymentCacheInvalid {
		t.Fatalf("invalid confirmation audit subject=%q outcome=%q detail=%q", subject, outcome, detail)
	}
	var replaySuccesses int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=?
 AND outcome='ok' AND detail LIKE ?`, req.IdempotencyKey,
		OperatorIdempotencyReplayPrefix+"%").Scan(&replaySuccesses); err != nil || replaySuccesses != 0 {
		t.Fatalf("successful replay audits=%d err=%v", replaySuccesses, err)
	}
	replayed, err := s.ApplyOperatorDeploymentCreate(req, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{}, errors.New("replay touched current material")
	})
	if err != nil || !replayed.Replayed || !replayed.Audited ||
		replayed.Deployment.DeploymentID != first.Deployment.DeploymentID || prepareCalls != 1 {
		t.Fatalf("replay result=%+v calls=%d err=%v", replayed, prepareCalls, err)
	}
	conflictReq := req
	conflictReq.RequestDigest = "sha256:different-body"
	_, err = s.ApplyOperatorDeploymentCreate(conflictReq, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return prepared, nil
	})
	var conflict *OperatorRequestError
	if !errors.As(err, &conflict) || conflict.Code != OperatorCodeIdempotencyConflict ||
		!conflict.Audited || prepareCalls != 1 {
		t.Fatalf("conflict=%+v calls=%d err=%v", conflict, prepareCalls, err)
	}
	var deployments, receipts, createdEvents int
	for _, check := range []struct {
		query string
		args  []any
		out   *int
	}{
		{`SELECT COUNT(*) FROM deployments`, nil, &deployments},
		{`SELECT COUNT(*) FROM operator_idempotency`, nil, &receipts},
		{`SELECT COUNT(*) FROM hub_events WHERE kind=?`, []any{HubDeploymentCreated}, &createdEvents},
	} {
		if err := s.DB().QueryRow(check.query, check.args...).Scan(check.out); err != nil {
			t.Fatal(err)
		}
	}
	if deployments != 1 || receipts != 1 || createdEvents != 1 {
		t.Fatalf("deployments=%d receipts=%d created_events=%d", deployments, receipts, createdEvents)
	}
	audits, err := s.Audit("", 20)
	if err != nil {
		t.Fatal(err)
	}
	var actionAudits, replayAudits int
	for _, audit := range audits {
		if audit.Action != AuditDeploymentCreate {
			continue
		}
		actionAudits++
		if audit.IsOperatorReplay() {
			replayAudits++
		}
	}
	if actionAudits != 4 || replayAudits != 1 {
		t.Fatalf("create audits=%d replay=%d rows=%+v", actionAudits, replayAudits, audits)
	}

	rejectedReq := req
	rejectedReq.IdempotencyKey = "deployment-create-rejected"
	rejectedReq.RequestDigest = "sha256:rejected-body"
	rejectionCalls := 0
	rejectPrepare := func() (OperatorDeploymentPrepared, error) {
		rejectionCalls++
		return OperatorDeploymentPrepared{}, &OperatorRequestError{
			Code: OperatorCodeDeploymentPreviewStale, Detail: "service-observed material changed",
		}
	}
	_, err = s.ApplyOperatorDeploymentCreate(rejectedReq, rejectPrepare)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeDeploymentPreviewStale ||
		rejection.Replayed || !rejection.Audited {
		t.Fatalf("fresh rejection=%+v err=%v", rejection, err)
	}
	_, err = s.ApplyOperatorDeploymentCreate(rejectedReq, func() (OperatorDeploymentPrepared, error) {
		rejectionCalls++
		return prepared, nil
	})
	if !errors.As(err, &rejection) || !rejection.Replayed || !rejection.Audited || rejectionCalls != 1 {
		t.Fatalf("replayed rejection=%+v calls=%d err=%v", rejection, rejectionCalls, err)
	}
	var cachedOutcome string
	if err := s.DB().QueryRow(`SELECT outcome FROM operator_idempotency WHERE idempotency_key=?`,
		rejectedReq.IdempotencyKey).Scan(&cachedOutcome); err != nil || cachedOutcome != "rejected" {
		t.Fatalf("rejection outcome=%q err=%v", cachedOutcome, err)
	}
}

func TestApplyOperatorDeploymentCreateAuditFailureRollsBackEverything(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	addRolloutMachine(t, s, "atomic-create", "atomic-create", true)
	if _, err := s.DB().Exec(`CREATE TRIGGER fail_operator_deployment_create_audit
	 BEFORE INSERT ON audit_log WHEN NEW.action='deployment-create'
	 BEGIN SELECT RAISE(ABORT, 'injected deployment create audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	req := OperatorDeploymentRequest{
		PreviewDigest: operatorDeploymentTestPreviewDigest, IdempotencyKey: "atomic-create-key",
		ConfirmChannel: "canary", ConfirmVersion: "2026.9.2",
		RequestDigest: "sha256:atomic-create-body", Audit: AuditEntry{SourceAddr: "test"},
	}
	_, err := s.ApplyOperatorDeploymentCreate(req, func() (OperatorDeploymentPrepared, error) {
		return OperatorDeploymentPrepared{
			CurrentPreviewDigest: req.PreviewDigest,
			Deployment: NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
				Spec: rolloutTestOpenClawSpec, BatchSize: 1, CreatedBy: "atomic-test",
				Targets: []NewDeploymentTarget{{MachineID: "atomic-create", BatchNo: 1}},
			},
		}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "injected deployment create audit failure") {
		t.Fatalf("audit failure err=%v", err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM desired_state`, `SELECT COUNT(*) FROM deployments`,
		`SELECT COUNT(*) FROM deployment_targets`, `SELECT COUNT(*) FROM jobs`,
		`SELECT COUNT(*) FROM operator_idempotency`,
		`SELECT COUNT(*) FROM hub_events WHERE kind='deployment_created'`,
	} {
		var count int
		if err := s.DB().QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback query=%q count=%d err=%v", query, count, err)
		}
	}
}

func TestApplyOperatorDeploymentContinueCASNoDoubleOpenAndRejectionReplay(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 8, 16, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	d, firstJobs := createBatchGuardFixture(t, s, 2)
	setRolloutJobTerminal(t, s, firstJobs[0], deploy.Failed)
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
		t.Fatalf("pause changed=%v err=%v", changed, err)
	}
	paused, err := s.Deployment(d.DeploymentID)
	if err != nil || paused.ControlRevision != 1 {
		t.Fatalf("paused=%+v err=%v", paused, err)
	}
	expectedRevision, expectedOpened := paused.ControlRevision, 1
	req := OperatorDeploymentRequest{
		DeploymentID: d.DeploymentID, PreviewDigest: operatorDeploymentTestPreviewDigest,
		ExpectedControlRevision: &expectedRevision, ExpectedOpenedBatch: &expectedOpened,
		ConfirmChannel: "canary",
		IdempotencyKey: "continue-key", RequestDigest: "sha256:continue-body",
		Audit: AuditEntry{SourceAddr: "test", Reason: "accept failed first batch"},
	}
	prepareCalls := 0
	prepare := func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{CurrentPreviewDigest: req.PreviewDigest}, nil
	}
	result, err := s.ApplyOperatorDeploymentContinue(req, prepare)
	if err != nil || result.Deployment.State != DeploymentRunning ||
		result.Deployment.ControlRevision != 2 || result.OpenedBatch != 2 || len(result.Jobs) != 1 {
		t.Fatalf("continue=%+v err=%v", result, err)
	}
	replay, err := s.ApplyOperatorDeploymentContinue(req, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{}, errors.New("replay touched deployment state")
	})
	if err != nil || !replay.Replayed || replay.Deployment.ControlRevision != 2 || prepareCalls != 1 {
		t.Fatalf("continue replay=%+v calls=%d err=%v", replay, prepareCalls, err)
	}

	staleReq := req
	staleReq.IdempotencyKey = "continue-stale-key"
	staleReq.RequestDigest = "sha256:continue-stale-body"
	_, err = s.ApplyOperatorDeploymentContinue(staleReq, prepare)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeDeploymentPreconditionFailed ||
		rejection.Replayed || !rejection.Audited {
		t.Fatalf("stale continue rejection=%+v err=%v", rejection, err)
	}
	callsAfterFreshRejection := prepareCalls
	_, err = s.ApplyOperatorDeploymentContinue(staleReq, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{}, errors.New("rejection replay touched state")
	})
	if !errors.As(err, &rejection) || !rejection.Replayed || prepareCalls != callsAfterFreshRejection {
		t.Fatalf("stale replay=%+v calls=%d/%d err=%v", rejection, prepareCalls, callsAfterFreshRejection, err)
	}
	var jobs, continuedEvents int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM deployment_targets WHERE deployment_id=? AND job_id IS NOT NULL`,
		d.DeploymentID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM hub_events WHERE kind=?`, HubDeploymentContinued).Scan(&continuedEvents); err != nil {
		t.Fatal(err)
	}
	if jobs != 2 || continuedEvents != 1 {
		t.Fatalf("jobs=%d continued_events=%d", jobs, continuedEvents)
	}
}

func TestApplyOperatorDeploymentRetryParentChildAndAuditAreAtomic(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failAudit bool
	}{
		{name: "success"},
		{name: "audit failure rolls parent and child back", failAudit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 8, 17, 0, 0, 0, time.UTC)
			s.nowFn = func() time.Time { return now }
			addRolloutMachine(t, s, "retry-target", "retry-target", true)
			parent, parentJobs, err := s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
				Spec: rolloutTestOpenClawSpec, BatchSize: 1, CreatedBy: "parent",
				Targets: []NewDeploymentTarget{{MachineID: "retry-target", BatchNo: 1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			setRolloutJobTerminal(t, s, parentJobs[0], deploy.Failed)
			if changed, err := s.SetDeploymentState(parent.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
				t.Fatalf("pause parent changed=%v err=%v", changed, err)
			}
			parent, _ = s.Deployment(parent.DeploymentID)
			expectedRevision, expectedOpened := parent.ControlRevision, 1
			req := OperatorDeploymentRequest{
				DeploymentID: parent.DeploymentID, PreviewDigest: operatorDeploymentTestPreviewDigest,
				ExpectedControlRevision: &expectedRevision, ExpectedOpenedBatch: &expectedOpened,
				ConfirmChannel: "canary", ConfirmVersion: "2026.9.2",
				IdempotencyKey: "retry-key-" + strings.ReplaceAll(tc.name, " ", "-"),
				RequestDigest:  "sha256:retry-body", Audit: AuditEntry{SourceAddr: "test", Reason: "retry failures"},
			}
			prepared := OperatorDeploymentPrepared{
				CurrentPreviewDigest:      req.PreviewDigest,
				TerminalFailureMachineIDs: []string{"retry-target"},
				Deployment: NewDeployment{
					Channel: parent.Channel, ResourceKind: parent.ResourceKind, ResourceID: parent.ResourceID,
					Spec: parent.Spec, BatchSize: parent.BatchSize, CreatedBy: "retry-operator",
					RetryOf: parent.DeploymentID,
					Targets: []NewDeploymentTarget{{MachineID: "retry-target", BatchNo: 1}},
				},
			}
			if tc.failAudit {
				if _, err := s.DB().Exec(`CREATE TRIGGER fail_operator_deployment_retry_audit
				 BEFORE INSERT ON audit_log WHEN NEW.action='deployment-retry'
				 BEGIN SELECT RAISE(ABORT, 'injected deployment retry audit failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.ApplyOperatorDeploymentRetry(req, func() (OperatorDeploymentPrepared, error) {
				return prepared, nil
			})
			storedParent, parentErr := s.Deployment(parent.DeploymentID)
			if tc.failAudit {
				if err == nil || !strings.Contains(err.Error(), "injected deployment retry audit failure") {
					t.Fatalf("retry failure result=%+v err=%v", result, err)
				}
				if parentErr != nil || storedParent.State != DeploymentPaused ||
					storedParent.ControlRevision != expectedRevision {
					t.Fatalf("retry rollback parent=%+v err=%v", storedParent, parentErr)
				}
				var deployments, receipts, retriedEvents int
				_ = s.DB().QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&deployments)
				_ = s.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&receipts)
				_ = s.DB().QueryRow(`SELECT COUNT(*) FROM hub_events WHERE kind=?`, HubDeploymentRetried).Scan(&retriedEvents)
				if deployments != 1 || receipts != 0 || retriedEvents != 0 {
					t.Fatalf("rollback deployments=%d receipts=%d retried_events=%d", deployments, receipts, retriedEvents)
				}
				return
			}
			if err != nil || result.Deployment.RetryOf != parent.DeploymentID ||
				result.Deployment.ControlRevision != 0 || result.OpenedBatch != 1 || len(result.Jobs) != 1 {
				t.Fatalf("retry result=%+v err=%v", result, err)
			}
			if parentErr != nil || storedParent.State != DeploymentFinished ||
				storedParent.ControlRevision != expectedRevision+1 {
				t.Fatalf("retry parent=%+v err=%v", storedParent, parentErr)
			}
			var retriedEvents int
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM hub_events WHERE kind=?`, HubDeploymentRetried).Scan(&retriedEvents); err != nil || retriedEvents != 1 {
				t.Fatalf("retried events=%d err=%v", retriedEvents, err)
			}
			zero := 0
			forged := req
			forged.ExpectedOpenedBatch = &zero
			prepareCalls := 0
			replay, err := s.ApplyOperatorDeploymentRetry(forged, func() (OperatorDeploymentPrepared, error) {
				prepareCalls++
				return OperatorDeploymentPrepared{}, errors.New("invalid cached request reached prepare")
			})
			if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") ||
				replay.Replayed || prepareCalls != 0 {
				t.Fatalf("zero-opened retry replay=%+v calls=%d err=%v", replay, prepareCalls, err)
			}
			var subject, outcome, detail string
			if err := s.DB().QueryRow(`SELECT subject,outcome,COALESCE(detail,'') FROM audit_log
 WHERE idempotency_key=? ORDER BY audit_id DESC LIMIT 1`, req.IdempotencyKey).
				Scan(&subject, &outcome, &detail); err != nil {
				t.Fatal(err)
			}
			if subject != "deployment idempotency cache" || outcome != "failed" || detail != operatorDeploymentCacheInvalid {
				t.Fatalf("invalid request audit subject=%q outcome=%q detail=%q", subject, outcome, detail)
			}
			var replaySuccesses int
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=?
 AND outcome='ok' AND detail LIKE ?`, req.IdempotencyKey,
				OperatorIdempotencyReplayPrefix+"%").Scan(&replaySuccesses); err != nil || replaySuccesses != 0 {
				t.Fatalf("successful replay audits=%d err=%v", replaySuccesses, err)
			}
			forged = req
			forged.ConfirmVersion = "2026.9.2-forged"
			replay, err = s.ApplyOperatorDeploymentRetry(forged, func() (OperatorDeploymentPrepared, error) {
				prepareCalls++
				return OperatorDeploymentPrepared{}, errors.New("invalid cached confirmation reached prepare")
			})
			if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") ||
				replay.Replayed || prepareCalls != 0 {
				t.Fatalf("forged retry replay=%+v calls=%d err=%v", replay, prepareCalls, err)
			}
		})
	}
}

func TestApplyOperatorDeploymentRetryRejectsActiveOrNonExactTargets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		terminal    bool
		providedIDs []string
		wantCode    string
	}{
		{name: "active parent", providedIDs: []string{"retry-target"}, wantCode: OperatorCodeDeploymentActiveJobs},
		{name: "missing service target identity", terminal: true, wantCode: OperatorCodeDeploymentNoRetryTargets},
		{name: "non-exact service target identity", terminal: true, providedIDs: []string{"other"}, wantCode: OperatorCodeDeploymentPreviewStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
			s.nowFn = func() time.Time { return now }
			addRolloutMachine(t, s, "retry-target", "retry-target", true)
			parent, jobs, err := s.CreateDeployment(NewDeployment{
				Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
				Spec: rolloutTestOpenClawSpec, BatchSize: 1, CreatedBy: "parent",
				Targets: []NewDeploymentTarget{{MachineID: "retry-target", BatchNo: 1}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.terminal {
				setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
			}
			if changed, err := s.SetDeploymentState(parent.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
				t.Fatalf("pause changed=%v err=%v", changed, err)
			}
			parent, _ = s.Deployment(parent.DeploymentID)
			expectedRevision, expectedOpened := parent.ControlRevision, 1
			req := OperatorDeploymentRequest{
				DeploymentID: parent.DeploymentID, PreviewDigest: operatorDeploymentTestPreviewDigest,
				ExpectedControlRevision: &expectedRevision, ExpectedOpenedBatch: &expectedOpened,
				ConfirmChannel: "canary", ConfirmVersion: "2026.9.2",
				IdempotencyKey: "retry-reject-" + strings.ReplaceAll(tc.name, " ", "-"),
				RequestDigest:  "sha256:retry-reject-body", Audit: AuditEntry{SourceAddr: "test"},
			}
			_, err = s.ApplyOperatorDeploymentRetry(req, func() (OperatorDeploymentPrepared, error) {
				return OperatorDeploymentPrepared{
					CurrentPreviewDigest: req.PreviewDigest, TerminalFailureMachineIDs: tc.providedIDs,
					Deployment: NewDeployment{
						Channel: parent.Channel, ResourceKind: parent.ResourceKind, ResourceID: parent.ResourceID,
						Spec: parent.Spec, BatchSize: 1, CreatedBy: "retry", RetryOf: parent.DeploymentID,
						Targets: []NewDeploymentTarget{{MachineID: "retry-target", BatchNo: 1}},
					},
				}, nil
			})
			var rejection *OperatorRequestError
			if !errors.As(err, &rejection) || rejection.Code != tc.wantCode || !rejection.Audited {
				t.Fatalf("rejection=%+v err=%v", rejection, err)
			}
			stored, _ := s.Deployment(parent.DeploymentID)
			if stored.State != DeploymentPaused || stored.ControlRevision != expectedRevision {
				t.Fatalf("rejected retry changed parent: %+v", stored)
			}
			var deployments int
			_ = s.DB().QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&deployments)
			if deployments != 1 {
				t.Fatalf("rejected retry created child: %d", deployments)
			}
		})
	}
}

func TestApplyOperatorDeploymentAbandonConfirmationCASAndReplay(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 8, 19, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	d, jobs := createBatchGuardFixture(t, s, 1)
	setRolloutJobTerminal(t, s, jobs[0], deploy.Failed)
	if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
		t.Fatalf("pause changed=%v err=%v", changed, err)
	}
	d, _ = s.Deployment(d.DeploymentID)
	expectedRevision, expectedOpened := d.ControlRevision, 1
	base := OperatorDeploymentRequest{
		DeploymentID: d.DeploymentID, PreviewDigest: operatorDeploymentTestPreviewDigest,
		ExpectedControlRevision: &expectedRevision, ExpectedOpenedBatch: &expectedOpened,
		RequestDigest: "sha256:abandon-body", Audit: AuditEntry{SourceAddr: "test", Reason: "release owner"},
	}
	bad := base
	bad.IdempotencyKey = "abandon-bad-confirm"
	bad.ConfirmDeploymentID = "wrong"
	prepareCalls := 0
	_, err := s.ApplyOperatorDeploymentAbandon(bad, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{CurrentPreviewDigest: bad.PreviewDigest}, nil
	})
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeDeploymentConfirmationMismatch ||
		prepareCalls != 0 {
		t.Fatalf("confirmation rejection=%+v calls=%d err=%v", rejection, prepareCalls, err)
	}
	_, err = s.ApplyOperatorDeploymentAbandon(bad, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{}, errors.New("replay touched state")
	})
	if !errors.As(err, &rejection) || !rejection.Replayed || prepareCalls != 0 {
		t.Fatalf("confirmation replay=%+v calls=%d err=%v", rejection, prepareCalls, err)
	}

	good := base
	good.IdempotencyKey = "abandon-good"
	good.ConfirmDeploymentID = d.DeploymentID
	result, err := s.ApplyOperatorDeploymentAbandon(good, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{CurrentPreviewDigest: good.PreviewDigest}, nil
	})
	if err != nil || result.Deployment.State != DeploymentFinished ||
		result.Deployment.ControlRevision != expectedRevision+1 || result.OpenedBatch != 1 {
		t.Fatalf("abandon=%+v err=%v", result, err)
	}
	forged := good
	forged.ConfirmDeploymentID = "wrong"
	callsBeforeForgedReplay := prepareCalls
	forgedReplay, err := s.ApplyOperatorDeploymentAbandon(forged, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{}, errors.New("invalid cached confirmation reached prepare")
	})
	if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") ||
		forgedReplay.Replayed || prepareCalls != callsBeforeForgedReplay {
		t.Fatalf("forged abandon replay=%+v calls=%d/%d err=%v", forgedReplay, prepareCalls, callsBeforeForgedReplay, err)
	}
	var subject, outcome, detail string
	if err := s.DB().QueryRow(`SELECT subject,outcome,COALESCE(detail,'') FROM audit_log
 WHERE idempotency_key=? ORDER BY audit_id DESC LIMIT 1`, good.IdempotencyKey).
		Scan(&subject, &outcome, &detail); err != nil {
		t.Fatal(err)
	}
	if subject != "deployment idempotency cache" || outcome != "failed" || detail != operatorDeploymentCacheInvalid {
		t.Fatalf("invalid confirmation audit subject=%q outcome=%q detail=%q", subject, outcome, detail)
	}
	replayed, err := s.ApplyOperatorDeploymentAbandon(good, func() (OperatorDeploymentPrepared, error) {
		prepareCalls++
		return OperatorDeploymentPrepared{}, errors.New("replay touched finished state")
	})
	if err != nil || !replayed.Replayed || prepareCalls != 1 {
		t.Fatalf("abandon replay=%+v calls=%d err=%v", replayed, prepareCalls, err)
	}
	var events int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM hub_events WHERE kind=?`, HubDeploymentAbandoned).Scan(&events); err != nil || events != 1 {
		t.Fatalf("abandon events=%d err=%v", events, err)
	}
}

func TestDeploymentControlRevisionMigrationAndLifecycleIncrements(t *testing.T) {
	t.Run("legacy column migration", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy-control-revision.db")
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().Exec(`ALTER TABLE deployments DROP COLUMN control_revision`); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := ValidateExistingLedger(path); err != nil {
			t.Fatalf("legacy deployment shape rejected: %v", err)
		}
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		var declaredType string
		var notNull int
		var defaultValue any
		if err := s.DB().QueryRow(`SELECT type,"notnull",dflt_value FROM pragma_table_info('deployments') WHERE name='control_revision'`).
			Scan(&declaredType, &notNull, &defaultValue); err != nil {
			t.Fatal(err)
		}
		if declaredType != "INTEGER" || notNull != 1 || defaultValue != "0" {
			t.Fatalf("control revision metadata type=%q notnull=%d default=%v", declaredType, notNull, defaultValue)
		}
		if err := ValidateExistingLedger(path); err != nil {
			t.Fatalf("migrated deployment shape rejected: %v", err)
		}
	})

	t.Run("open pause continue", func(t *testing.T) {
		s := rolloutStore(t)
		now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
		s.nowFn = func() time.Time { return now }
		d, first := createBatchGuardFixture(t, s, 2)
		if d.ControlRevision != 0 {
			t.Fatalf("create control revision=%d", d.ControlRevision)
		}
		setRolloutJobTerminal(t, s, first[0], deploy.Succeeded)
		second, err := s.OpenDeploymentBatch(d.DeploymentID, 2, now)
		if err != nil || len(second) != 1 {
			t.Fatalf("open second jobs=%+v err=%v", second, err)
		}
		d, _ = s.Deployment(d.DeploymentID)
		if d.ControlRevision != 1 {
			t.Fatalf("open control revision=%d", d.ControlRevision)
		}
		// Reading an already-open batch is idempotent and changes no authority.
		if _, err := s.OpenDeploymentBatch(d.DeploymentID, 2, now); err != nil {
			t.Fatal(err)
		}
		d, _ = s.Deployment(d.DeploymentID)
		if d.ControlRevision != 1 {
			t.Fatalf("reopen control revision=%d", d.ControlRevision)
		}
		setRolloutJobTerminal(t, s, second[0], deploy.Failed)
		if changed, err := s.SetDeploymentState(d.DeploymentID, DeploymentRunning, DeploymentPaused, now); err != nil || !changed {
			t.Fatalf("pause changed=%v err=%v", changed, err)
		}
		d, _ = s.Deployment(d.DeploymentID)
		if d.ControlRevision != 2 {
			t.Fatalf("pause control revision=%d", d.ControlRevision)
		}
		d, jobs, err := s.ContinueDeployment(d.DeploymentID, now)
		if err != nil || d.State != DeploymentFinished || len(jobs) != 0 || d.ControlRevision != 3 {
			t.Fatalf("continue finish deployment=%+v jobs=%+v err=%v", d, jobs, err)
		}
	})

	t.Run("boundary pause", func(t *testing.T) {
		s := rolloutStore(t)
		now := time.Date(2026, 9, 8, 21, 0, 0, 0, time.UTC)
		s.nowFn = func() time.Time { return now }
		d, jobs := createBatchGuardFixture(t, s, 2)
		setRolloutJobTerminal(t, s, jobs[0], deploy.Succeeded)
		if changed, err := s.PauseDeploymentAtBoundary(d.DeploymentID, 1,
			DeploymentPauseBatchNotReady, "manual test boundary", now); err != nil || !changed {
			t.Fatalf("boundary pause changed=%v err=%v", changed, err)
		}
		d, _ = s.Deployment(d.DeploymentID)
		if d.ControlRevision != 1 {
			t.Fatalf("boundary pause control revision=%d", d.ControlRevision)
		}
	})
}
