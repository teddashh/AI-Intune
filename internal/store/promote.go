package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/state"
)

// RecordCanarySilentFailure 把同一台一小時內連續看到的失敗延伸成一段區間。
// ⚠ 不能只做「每小時一個點」：deployment 若在那一小時中間完成，持續到完成後的失敗會被前一個點吞掉。
// continuity 刻意不用 reason 當 key；finding 文字可能含「已經 8h01m」這種每輪都變的時長。
func (s *Store) RecordCanarySilentFailure(machineID, reason string, now time.Time) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("store: begin canary silent failure: %w", err)
	}
	defer tx.Rollback()
	recorded, err := recordCanarySilentFailureTx(tx, machineID, reason, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit canary silent failure: %w", err)
	}
	return recorded, nil
}

func recordCanarySilentFailureTx(tx *sql.Tx, machineID, reason string, now time.Time) (bool, error) {
	if machineID == "" || reason == "" {
		return false, errors.New("store: canary silent failure needs machine and reason")
	}
	now = now.UTC().Truncate(time.Second)
	var first, last sql.NullString
	err := tx.QueryRow(`SELECT first_seen_at,last_seen_at FROM canary_silent_failures
	 WHERE machine_id=? AND open=1 ORDER BY last_seen_at DESC LIMIT 1`, machineID).Scan(&first, &last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: latest canary silent failure: %w", err)
	}
	if err == nil {
		lastSeen := parseTime(last.String)
		if !lastSeen.IsZero() && !now.After(lastSeen.Add(time.Hour)) {
			if now.After(lastSeen) {
				if _, err := tx.Exec(`UPDATE canary_silent_failures SET last_seen_at=?
	 WHERE machine_id=? AND first_seen_at=? AND open=1`, fmtTime(now), machineID, first.String); err != nil {
					return false, fmt.Errorf("store: extend canary silent failure: %w", err)
				}
			}
			return false, nil
		}
		// 一小時以上沒抽樣時另開一段，避免把沒有量到的空窗說成持續失敗；
		// 舊段與新 open 段在同一個 tx 切換，promote 永遠看不到全綠縫隙。
		if _, err := tx.Exec(`UPDATE canary_silent_failures SET open=0
	 WHERE machine_id=? AND first_seen_at=? AND open=1`, machineID, first.String); err != nil {
			return false, fmt.Errorf("store: close stale canary silent failure: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO canary_silent_failures(machine_id,reason,first_seen_at,last_seen_at,open)
	 VALUES(?,?,?,?,1)
	 ON CONFLICT(machine_id,first_seen_at) DO UPDATE SET
	   reason=excluded.reason,last_seen_at=excluded.last_seen_at,open=1`,
		machineID, reason, fmtTime(now), fmtTime(now)); err != nil {
		return false, fmt.Errorf("store: record canary silent failure: %w", err)
	}
	return true, nil
}

// closeCanarySilentFailureTx 只接受明確的健康 judgement；時間沒再抽到不是 recovery。
func closeCanarySilentFailureTx(tx *sql.Tx, machineID string, now time.Time) error {
	return closeCanarySilentFailureWithEvidenceTx(tx, machineID, now, now)
}

func closeCanarySilentFailureWithEvidenceTx(tx *sql.Tx, machineID string, evidenceAt, receivedAt time.Time) error {
	if machineID == "" {
		return errors.New("store: close canary silent failure needs machine")
	}
	evidenceStamp := fmtTime(evidenceAt.UTC().Truncate(time.Second))
	receivedStamp := fmtTime(receivedAt.UTC().Truncate(time.Second))
	if _, err := tx.Exec(`UPDATE canary_silent_failures
	 SET last_seen_at=?,open=0
	 WHERE machine_id=? AND open=1 AND last_seen_at < ?`, receivedStamp, machineID, evidenceStamp); err != nil {
		return fmt.Errorf("store: close canary silent failure: %w", err)
	}
	return nil
}

// ReconcileFleet 在拿到 SQLite writer reservation 之後才讀 Overview，並在同一
// 筆交易裡寫入狀態歷史與 promote failure span。它只會開啟或延長
// failure；只有同一批明確健康的 observation 才能關閉。另一個 Store 的 stable create
// 只能完整地排在這次 judgement 前面或後面，不能插進 derive/materialize 之間。
func (s *Store) ReconcileFleet(now time.Time) error {
	now = now.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin fleet reconcile: %w", err)
	}
	defer tx.Rollback()

	// 這些讀走第二條 pooled connection。WAL 允許 reader 與已拿 writer
	// reservation 的 transaction 共存，而 reservation 已擋住其他 writer。
	ov, err := s.Overview(now)
	if err != nil {
		return fmt.Errorf("store: reconcile overview: %w", err)
	}
	if s.afterReconcileOverview != nil {
		s.afterReconcileOverview()
	}
	for _, machine := range ov.Machines {
		if err := recordStateTransitionTx(tx, machine.MachineID, machine.State, machine.Reason, ov.Now); err != nil {
			return fmt.Errorf("store: reconcile state %s: %w", machine.DisplayName, err)
		}
		failureReason := ""
		if machine.State == state.Unreachable {
			failureReason = "失聯"
		} else {
			for _, finding := range machine.Findings {
				if finding.Kind == "workload" && !finding.Advisory {
					failureReason = finding.Message
					break
				}
			}
		}
		if failureReason != "" {
			if _, err := recordCanarySilentFailureTx(tx, machine.MachineID, failureReason, ov.Now); err != nil {
				return fmt.Errorf("store: reconcile promote evidence %s: %w", machine.DisplayName, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit fleet reconcile: %w", err)
	}
	return nil
}

// CanarySilentFailuresSince 回傳跟 [since, now] 有重疊的失敗區間，兩端都含。
// 時間只存到秒；排除剛好等於 finished_at 的列會把同一秒的失敗洗掉。
func (s *Store) CanarySilentFailuresSince(machineIDs []string, since, now time.Time) ([]rollout.SilentFailure, error) {
	if len(machineIDs) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(machineIDs)), ",")
	args := make([]any, 0, len(machineIDs)+2)
	for _, id := range machineIDs {
		args = append(args, id)
	}
	args = append(args, fmtTime(now), fmtTime(since))
	rows, err := s.db.Query(`SELECT f.first_seen_at,f.last_seen_at,m.display_name,f.reason
 FROM canary_silent_failures f JOIN machine_registry m ON m.machine_id=f.machine_id
	 WHERE f.machine_id IN (`+marks+`) AND (f.open=1 OR (f.first_seen_at <= ? AND f.last_seen_at >= ?))
	 ORDER BY f.first_seen_at,f.machine_id,f.reason`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: canary silent failures since: %w", err)
	}
	defer rows.Close()
	var out []rollout.SilentFailure
	for rows.Next() {
		var failure rollout.SilentFailure
		var first, last string
		if err := rows.Scan(&first, &last, &failure.DisplayName, &failure.Reason); err != nil {
			return nil, fmt.Errorf("store: scan canary silent failure: %w", err)
		}
		failure.FirstSeenAt, failure.LastSeenAt = parseTime(first), parseTime(last)
		out = append(out, failure)
	}
	return out, rows.Err()
}

// PromoteFacts 只認 OpenClaw 最新一張 canary attempt；舊的同 digest 證據不能越過
// 一張較新的 paused／不同版本 deployment。retry 則沿 retry_of 聚合，保留前一輪已成功的證人。
func (s *Store) PromoteFacts(version, digest string, now time.Time) (rollout.PromoteFacts, error) {
	facts := rollout.PromoteFacts{Version: version, Digest: strings.TrimPrefix(digest, "sha256:")}
	rows, err := s.db.Query(`SELECT p.deployment_id,p.channel,p.desired_id,p.resource_kind,p.resource_id,
		 p.revision,p.control_revision,p.batch_size,p.pause_after_canary,p.state,p.created_at,p.created_by,p.paused_at,p.finished_at,p.retry_of,d.spec
	 FROM deployments p JOIN desired_state d ON d.desired_id=p.desired_id
	 WHERE p.channel='canary' ORDER BY p.rowid DESC`)
	if err != nil {
		return facts, fmt.Errorf("store: scan latest canary deployment: %w", err)
	}
	var latest Deployment
	var found bool
	for rows.Next() {
		candidate, err := scanDeployment(rows)
		if err != nil {
			rows.Close()
			return facts, fmt.Errorf("store: scan latest canary deployment: %w", err)
		}
		kind, _, parseErr := model.ParseJobSpec([]byte(candidate.Spec))
		canonicalResource := candidate.ResourceKind == "openclaw" && candidate.ResourceID == "openclaw"
		// 舊版 CreateDeployment 沒有驗 resource/spec material，ledger 可能留下
		// canonical OpenClaw resource 搭配 malformed／noop spec。這仍是一張較新的
		// OpenClaw attempt，必須遮住舊綠燈；不能因為 spec 正好解不開就把它當雜訊。
		// 反向錯標（spec 明確是 OpenClaw、resource key 卻不是）也同樣是 boundary。
		// 只有 resource 與可解析 spec 都明確不是 OpenClaw 的 deployment 才能略過。
		if !canonicalResource && (parseErr != nil || kind != "openclaw") {
			continue
		}
		latest, found = candidate, true
		break
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return facts, fmt.Errorf("store: scan latest canary deployment: %w", err)
	}
	if err := rows.Close(); err != nil {
		return facts, fmt.Errorf("store: close latest canary deployment rows: %w", err)
	}
	if !found {
		return facts, nil
	}
	if latest.ResourceKind != "openclaw" || latest.ResourceID != "openclaw" {
		return facts, fmt.Errorf("%w: latest canary %s has OpenClaw spec under %s:%s",
			ErrDeploymentMaterialMismatch, latest.DeploymentID, latest.ResourceKind, latest.ResourceID)
	}
	want := strings.TrimPrefix(digest, "sha256:")
	var latestSpec model.OpenClawSpec
	if json.Unmarshal([]byte(latest.Spec), &latestSpec) != nil || latestSpec.Kind != "openclaw" || latestSpec.Artifact == nil ||
		!artifact.ValidSHA256Hex(latestSpec.Artifact.SHA256) {
		return facts, fmt.Errorf("%w: latest canary %s has invalid OpenClaw spec",
			ErrDeploymentMaterialMismatch, latest.DeploymentID)
	}
	if latest.State != DeploymentFinished || latest.FinishedAt == nil {
		facts.CanaryBlock = fmt.Sprintf("最新 canary 部署 %s 還是 %s，不能拿更舊的結果",
			shortStoreID(latest.DeploymentID, 8), latest.State)
		return facts, nil
	}
	var soakBoundaryEvidenceID int64
	if err := s.db.QueryRow(`SELECT evidence_id FROM deployment_soak_boundaries WHERE deployment_id=?`,
		latest.DeploymentID).Scan(&soakBoundaryEvidenceID); errors.Is(err, sql.ErrNoRows) {
		// schema migration can create the boundary table, but it cannot reconstruct
		// which observations were appended before an already-finished deployment.
		facts.CanaryBlock = fmt.Sprintf("最新 canary 部署 %s 沒有 observation sequence boundary；必須重跑 canary",
			shortStoreID(latest.DeploymentID, 8))
		return facts, nil
	} else if err != nil {
		return facts, fmt.Errorf("store: read canary soak evidence boundary: %w", err)
	}
	if soakBoundaryEvidenceID < 0 {
		facts.CanaryBlock = fmt.Sprintf("最新 canary 部署 %s 的 observation sequence boundary 無效；必須重跑 canary",
			shortStoreID(latest.DeploymentID, 8))
		return facts, nil
	}
	evidenceEpoch, epochPublished, err := s.canaryEvidenceEpoch()
	if err != nil {
		return facts, err
	}
	if !epochPublished {
		facts.CanaryBlock = "Hub 還沒有 canary evidence history 的啟用邊界；必須在 recorder 啟用後重跑 canary"
		return facts, nil
	}
	// 新表在 upgrade 時是空的；不能用 migration 前完成的 canary，搭配升級後
	// 一筆 fresh healthy witness，把整段未曾記錄的歷史冒充成「沒有 failure」。
	// 同秒沒有足夠順序證據，也必須重跑到嚴格晚於 epoch。
	if !latest.FinishedAt.After(evidenceEpoch) {
		facts.CanaryBlock = fmt.Sprintf("最新 canary 部署 %s 完成於 evidence history 啟用邊界 %s 之前；必須重跑 canary",
			shortStoreID(latest.DeploymentID, 8), fmtTime(evidenceEpoch))
		return facts, nil
	}
	if latestSpec.Version != version || latestSpec.Artifact.SHA256 != want {
		facts.CanaryBlock = fmt.Sprintf("最新 canary 部署 %s 是 %s（digest %s），不能拿更舊的 %s（digest %s）結果",
			shortStoreID(latest.DeploymentID, 8), latestSpec.Version, shortStoreID(latestSpec.Artifact.SHA256, 12),
			version, shortStoreID(want, 12))
		return facts, nil
	}

	// 同一條 cycle/depth guard 同時給 DB view 與 create transaction 使用；這裡再逐張
	// 載入 target，一條長 lineage 仍是 O(n)，不會每個 ancestor 又重走一次 ancestry。
	deployments, err := deploymentRetryLineage(s.db, latest)
	if err != nil {
		return facts, err
	}
	lineage := make([]DeploymentView, 0, len(deployments))
	lineageJobIDs := make(map[string]struct{})
	for i, deployment := range deployments {
		if err := validateDeploymentJobGraph(s.db, deployment.DeploymentID); err != nil {
			return facts, err
		}
		if err := validateStoredDeploymentBatchPlan(s.db, deployment); err != nil {
			return facts, err
		}
		if i > 0 {
			if deployment.Channel != latest.Channel || deployment.ResourceKind != latest.ResourceKind ||
				deployment.ResourceID != latest.ResourceID || deployment.Spec != latest.Spec {
				return facts, fmt.Errorf("%w: ancestor %s", ErrDeploymentRetryMismatch, deployment.DeploymentID)
			}
			// paused／finished legacy 帳本只有在所有已開 job 都終態時才固定；否則
			// 後代 retry 的 success 會遮掉仍在變動的 ancestor target。
			if deployment.State == DeploymentRunning {
				return facts, fmt.Errorf("%w: ancestor %s", ErrDeploymentRetryParentRunning, deployment.DeploymentID)
			}
			if deployment.State != DeploymentPaused && deployment.State != DeploymentFinished {
				return facts, fmt.Errorf("%w: ancestor %s has state %q", ErrDeploymentRetryParentRunning, deployment.DeploymentID, deployment.State)
			}
			active, err := deploymentHasNonTerminalJobs(s.db, deployment.DeploymentID)
			if err != nil {
				return facts, err
			}
			if active {
				return facts, fmt.Errorf("%w: ancestor %s", ErrDeploymentRetryParentActive, deployment.DeploymentID)
			}
		}
		if err := validateDeploymentJobMaterialGraph(s.db, deployment.DeploymentID); err != nil {
			return facts, err
		}
		targets, err := s.DeploymentTargets(deployment.DeploymentID)
		if err != nil {
			return facts, err
		}
		for _, target := range targets {
			if target.JobID != "" {
				lineageJobIDs[target.JobID] = struct{}{}
			}
		}
		lineage = append(lineage, DeploymentView{Deployment: deployment, Targets: targets})
	}
	type aggregate struct {
		target      rollout.CanaryTarget
		unresolved  bool
		succeededAt *time.Time
	}
	byMachine := make(map[string]*aggregate)
	var order []string
	for i := len(lineage) - 1; i >= 0; i-- {
		for _, target := range lineage[i].Targets {
			a := byMachine[target.MachineID]
			if a == nil {
				a = &aggregate{target: rollout.CanaryTarget{MachineID: target.MachineID, DisplayName: target.DisplayName}}
				byMachine[target.MachineID] = a
				order = append(order, target.MachineID)
			}
			a.target.DisplayName = target.DisplayName
			switch {
			case target.ExcludedReason != "":
				a.target.JobID = ""
				a.target.ExcludedReason = target.ExcludedReason
				a.target.Ran = false
				a.target.Succeeded = false
				a.succeededAt = nil
			case target.JobID == "":
				a.target.JobID = ""
				a.target.ExcludedReason = ""
				a.target.Ran = false
				a.target.Succeeded = false
				a.succeededAt = nil
			case target.JobState == deploy.Succeeded:
				a.target.ExcludedReason = ""
				a.target.JobID = target.JobID
				a.target.Ran = true
				if target.TerminalAt == nil {
					a.target.Succeeded = false
					a.unresolved = true
					a.succeededAt = nil
				} else {
					a.target.Succeeded = true
					a.unresolved = false
					at := *target.TerminalAt
					a.succeededAt = &at
				}
			default:
				a.target.ExcludedReason = ""
				a.target.JobID = target.JobID
				a.target.Ran = true
				a.target.Succeeded = false
				a.unresolved = true
				a.succeededAt = nil
			}
		}
	}
	run := &rollout.CanaryRun{DeploymentID: latest.DeploymentID, FinishedAt: *latest.FinishedAt}
	ids := make([]string, 0, len(order))
	cohortFinished := run.FinishedAt
	allSucceeded := len(order) > 0
	for _, id := range order {
		a := byMachine[id]
		run.Targets = append(run.Targets, a.target)
		ids = append(ids, id)
		if a.unresolved {
			run.Stuck++
		}
		if !a.target.Ran || !a.target.Succeeded || a.succeededAt == nil {
			allSucceeded = false
		} else if a.succeededAt.After(cohortFinished) {
			cohortFinished = *a.succeededAt
		}
	}
	if run.Stuck == 0 && allSucceeded {
		run.FinishedAt = cohortFinished
	}
	for i := range run.Targets {
		if !run.Targets[i].Ran || !run.Targets[i].Succeeded || run.Targets[i].JobID == "" {
			continue
		}
		gate, err := s.promotionIndependentGateState(run.Targets[i].JobID, version, now)
		if err != nil {
			return facts, err
		}
		run.Targets[i].IndependentGate = gate
	}
	if !run.FinishedAt.After(evidenceEpoch) {
		facts.CanaryBlock = fmt.Sprintf("canary %s 的 cohort 完成時間沒有嚴格晚於 evidence history 啟用邊界 %s；必須重跑 canary",
			shortStoreID(latest.DeploymentID, 8), fmtTime(evidenceEpoch))
		return facts, nil
	}
	run.WaitFrom = run.FinishedAt

	ov, err := s.Overview(now)
	if err != nil {
		return facts, err
	}
	judged := make(map[string]MachineRow, len(ov.Machines))
	for _, row := range ov.Machines {
		judged[row.MachineID] = row
	}
	retired := make(map[string]bool, len(ov.Retired))
	for _, machine := range ov.Retired {
		retired[machine.MachineID] = machine.RetiredAt != nil
	}
	workloadWitnesses, err := s.latestWorkloadObservationWitnesses(ids)
	if err != nil {
		return facts, err
	}
	policy, err := s.activeWorkloadPolicy()
	if err != nil {
		return facts, err
	}
	applied, err := s.latestSucceededOpenClawWitnesses(ids)
	if err != nil {
		return facts, err
	}
	for i := range run.Targets {
		ct := &run.Targets[i]
		row, ok := judged[ct.MachineID]
		ct.Judged, ct.Retired = ok, retired[ct.MachineID]
		if ok {
			ct.IdentityConflict = row.State == state.IdentityConflict
			ct.ClockUntrusted = row.Facts.ClockSkew > state.ClockSkewTolerance || row.Facts.ClockSkew < -state.ClockSkewTolerance
			ct.Unreachable = row.State == state.Unreachable
			ct.SilentNow = hasCanarySilentFinding(row.Findings)
		}
		// latestWorkloadObservationWitnesses 沒有列時不寫鍵，因此 witnessFound
		// 就是帳本上有沒有這台的 workload witness。
		workloadWitness, witnessFound := workloadWitnesses[ct.MachineID]
		ct.WorkloadObserved = witnessFound
		ct.LastObservation = workloadWitness.evidenceAt
		ct.OpenClawPresent = workloadWitness.openClawPresent
		ct.RunningVersion = workloadWitness.runningVersion
		ct.WorkloadReady, ct.WorkloadReason = workloadWitnessReadiness(
			workloadWitness, policy, ct.DisplayName, version, now)
		coverageReady, coverageReason, err := s.workloadObservationCoverage(
			ct.MachineID, ct.DisplayName, version, policy, soakBoundaryEvidenceID, run.FinishedAt, now)
		if err != nil {
			return facts, err
		}
		if !coverageReady && ct.WorkloadReady {
			ct.WorkloadReady = false
			ct.WorkloadReason = coverageReason
		}
		appliedWitness := applied[ct.MachineID]
		ct.AppliedDigest = appliedWitness.digest
		ct.AppliedReason = appliedWitness.reason
		if appliedWitness.jobID != "" {
			if _, inLineage := lineageJobIDs[appliedWitness.jobID]; !inLineage {
				lineageReason := fmt.Sprintf(
					"最近一張 applied digest 的 OpenClaw 工作單 %s 是 canary retry lineage 外重套；必須重跑 canary",
					shortStoreID(appliedWitness.jobID, 8))
				if ct.AppliedReason == "" {
					ct.AppliedReason = lineageReason
				} else {
					ct.AppliedReason += "；" + lineageReason
				}
			}
		}
	}
	facts.Canary = run
	facts.Silent, err = s.CanarySilentFailuresSince(ids, run.FinishedAt, now)
	return facts, err
}

// promotionIndependentGateState answers whether the effective successful
// canary job has one complete report from an assigned, active fleet peer. The
// runner owns the commands; the Hub recognizes only the three rule identities
// that make that report complete. Rows from an unassigned producer, an older
// assignment, or another verifier kind cannot unlock stable promotion. A
// failed, digest-clashing, or release-clashing row from an older assignment
// still locks this canary attempt; reassignment is not a way to erase an
// observed failure.
func (s *Store) promotionIndependentGateState(jobID, expectedVersion string, now time.Time) (rollout.IndependentGateState, error) {
	if !validObservedVersion(expectedVersion) || expectedVersion == "" {
		return "", fmt.Errorf("%w: promotion gate expected version is invalid", ErrDeploymentMaterialMismatch)
	}
	if err := s.validatePromotionIndependentGateTimes(jobID); err != nil {
		return "", err
	}
	var assigned, active int
	if err := s.db.QueryRow(`SELECT COUNT(*),
	       COALESCE(SUM(CASE WHEN v.revoked_at IS NULL THEN 1 ELSE 0 END),0)
	  FROM verification_assignments AS a
	  JOIN verifiers AS v ON v.verifier_id=a.verifier_id
	 WHERE a.job_id=? AND v.kind=? AND a.assigned_at<=?`, jobID, VerifierKindFleetPeerAgent,
		fmtTime(now)).Scan(&assigned, &active); err != nil {
		return "", fmt.Errorf("store: count promotion verifier assignments: %w", err)
	}
	if assigned == 0 {
		return rollout.IndependentGateUnassigned, nil
	}
	if active == 0 {
		return rollout.IndependentGateProducerRevoked, nil
	}

	var rows, live, current, clashes, releaseClashes, fresh, failed, releaseUnreported, complete int
	err := s.db.QueryRow(`
WITH current_assignments AS (
  SELECT a.verifier_id,MAX(a.assigned_at) AS assigned_at
    FROM verification_assignments AS a
    JOIN verifiers AS v ON v.verifier_id=a.verifier_id
	 WHERE a.job_id=? AND v.kind=? AND a.assigned_at<=?
   GROUP BY a.verifier_id
), eligible AS (
	SELECT r.verifier_id,r.rule_id,r.passed,r.observed_digest,r.observed_version,r.received_at,
         CASE WHEN r.received_at>=a.assigned_at THEN 1 ELSE 0 END AS current_report,
         j.artifact_digest,j.terminal_at,v.revoked_at
    FROM verification_results AS r
    JOIN jobs AS j ON j.job_id=r.job_id
    JOIN verifiers AS v ON v.verifier_id=r.verifier_id
    JOIN current_assignments AS a ON a.verifier_id=r.verifier_id
	 WHERE r.job_id=? AND r.evidence_role=? AND r.received_at<=?
     AND EXISTS (SELECT 1 FROM verification_assignments AS prior
                  WHERE prior.job_id=r.job_id AND prior.verifier_id=r.verifier_id
                    AND prior.assigned_at<=r.received_at)
), per_producer AS (
  SELECT verifier_id,COUNT(*) AS rows,
         SUM(CASE WHEN revoked_at IS NULL THEN 1 ELSE 0 END) AS live,
		 SUM(CASE WHEN revoked_at IS NULL AND current_report=1 THEN 1 ELSE 0 END) AS current_rows,
         SUM(CASE WHEN revoked_at IS NULL AND observed_digest<>''
                   AND observed_digest<>COALESCE(artifact_digest,'') THEN 1 ELSE 0 END) AS clashes,
		 SUM(CASE WHEN revoked_at IS NULL AND rule_id=? AND passed=1
		           AND observed_version<>'' AND observed_version<>? THEN 1 ELSE 0 END) AS release_clashes,
         SUM(CASE WHEN revoked_at IS NULL AND terminal_at IS NOT NULL AND terminal_at<>''
                   AND received_at>terminal_at THEN 1 ELSE 0 END) AS fresh,
         -- 終態前送到的列還不能描述終態，不是可用規則；算進 failed 會讓 Stale 指示的重新指派反而把 gate 鎖成 Failed。
         SUM(CASE WHEN revoked_at IS NULL AND terminal_at IS NOT NULL AND terminal_at<>''
                   AND received_at>terminal_at AND passed=0 THEN 1 ELSE 0 END) AS failed,
		 SUM(CASE WHEN revoked_at IS NULL AND current_report=1 AND rule_id=? AND passed=1
		           AND observed_version='' AND terminal_at IS NOT NULL AND terminal_at<>''
		           AND received_at>terminal_at THEN 1 ELSE 0 END) AS release_unreported,
         COUNT(DISTINCT CASE WHEN revoked_at IS NULL AND terminal_at IS NOT NULL AND terminal_at<>''
					 AND received_at>terminal_at AND current_report=1
					 AND rule_id IN (?,?,?) THEN rule_id END) AS fresh_rules
    FROM eligible
   GROUP BY verifier_id
)
SELECT COALESCE(SUM(rows),0),COALESCE(SUM(live),0),COALESCE(SUM(current_rows),0),
	   COALESCE(SUM(clashes),0),COALESCE(SUM(release_clashes),0),
	   COALESCE(SUM(fresh),0),COALESCE(SUM(failed),0),COALESCE(SUM(release_unreported),0),
       COALESCE(MAX(CASE WHEN live>0 AND fresh_rules=? THEN 1 ELSE 0 END),0)
	FROM per_producer`, jobID, VerifierKindFleetPeerAgent, fmtTime(now), jobID,
		JobVerificationRoleIndependent, fmtTime(now),
		model.IndependentRuleOpenClawCurrentRelease, expectedVersion,
		model.IndependentRuleOpenClawCurrentRelease,
		model.IndependentRuleOpenClawCurrentRelease, model.IndependentRuleOpenClawGatewayHTTP,
		model.IndependentRuleOpenClawUnitState, model.IndependentFleetPeerRequiredRules).
		Scan(&rows, &live, &current, &clashes, &releaseClashes, &fresh, &failed,
			&releaseUnreported, &complete)
	if err != nil {
		return "", fmt.Errorf("store: aggregate promotion independent gate: %w", err)
	}
	switch {
	case rows == 0 || live == 0:
		return rollout.IndependentGateAwaitingReport, nil
	case clashes > 0:
		return rollout.IndependentGateDigestMismatch, nil
	case releaseClashes > 0:
		return rollout.IndependentGateReleaseMismatch, nil
	// failed 只數自己落在終態後可用時間窗裡的失敗列，所以走到這裡一定有新鮮證據。
	case failed > 0:
		return rollout.IndependentGateFailed, nil
	case current == 0:
		return rollout.IndependentGateAwaitingReport, nil
	case fresh == 0:
		return rollout.IndependentGateStale, nil
	case releaseUnreported > 0:
		return rollout.IndependentGateReleaseUnreported, nil
	case complete == 0:
		return rollout.IndependentGateIncompleteReport, nil
	default:
		return rollout.IndependentGatePassed, nil
	}
}

func (s *Store) validatePromotionIndependentGateTimes(jobID string) error {
	// 空的 INDEPENDENT received_at 仍代表沒有合格證據；它不參與下方的時間比較。
	rows, err := s.db.Query(`
SELECT 'assignment assigned_at',a.assigned_at
  FROM verification_assignments AS a
  JOIN verifiers AS v ON v.verifier_id=a.verifier_id
 WHERE a.job_id=? AND v.kind=?
UNION ALL
SELECT 'independent result received_at',r.received_at
  FROM verification_results AS r
 WHERE r.job_id=? AND r.evidence_role=? AND r.received_at<>''
UNION ALL
SELECT 'job terminal_at',j.terminal_at
  FROM jobs AS j
 WHERE j.job_id=? AND j.terminal_at IS NOT NULL AND j.terminal_at<>''`,
		jobID, VerifierKindFleetPeerAgent, jobID,
		JobVerificationRoleIndependent, jobID)
	if err != nil {
		return fmt.Errorf("store: inspect promotion independent gate times: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var field, raw string
		if err := rows.Scan(&field, &raw); err != nil {
			return fmt.Errorf("store: inspect promotion independent gate time: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil || raw != fmtTime(parsed) {
			return fmt.Errorf("store: promotion independent gate %s is not canonical", field)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: inspect promotion independent gate times: %w", err)
	}
	return nil
}

func (s *Store) canaryEvidenceEpoch() (time.Time, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT started_at FROM canary_evidence_epoch WHERE singleton=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: read canary evidence epoch: %w", err)
	}
	epoch := parseTime(raw)
	if epoch.IsZero() {
		return time.Time{}, false, nil
	}
	return epoch, true, nil
}

func shortStoreID(value string, n int) string {
	runes := []rune(value)
	if len(runes) <= n {
		return value
	}
	return string(runes[:n])
}

type promoteOpenClawWitness struct {
	receivedAt time.Time
	install    *model.OpenClawInstall
	present    bool
}

// latestPromoteOpenClawWitnesses 把 running version 跟產生它的同一列 Hub received_at 綁在一起。
// 不能拿另一種 kind 的 fresh 列，替一筆舊的 OpenClaw version 觀測冒充 freshness。
func (s *Store) latestPromoteOpenClawWitnesses(machineIDs []string) (map[string]promoteOpenClawWitness, error) {
	out := make(map[string]promoteOpenClawWitness, len(machineIDs))
	for _, machineID := range machineIDs {
		obs, ok, err := s.latestObservation(machineID, KindOpenClaw, KindOpenClaw)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		witness := promoteOpenClawWitness{receivedAt: obs.ReceivedAt}
		if openclaw, ok := unmarshalInto[model.OpenClaw](obs.Payload); ok {
			witness.install = openclaw.Install
			witness.present = openclaw.Present
		}
		out[machineID] = witness
	}
	return out, nil
}

type workloadObservationWitness struct {
	verdict         string
	receivedAt      time.Time
	evidenceAt      time.Time
	openClawPresent bool
	runningVersion  string
	policyToken     string
	policyValid     bool
}

func (s *Store) latestWorkloadObservationWitnesses(machineIDs []string) (map[string]workloadObservationWitness, error) {
	out := make(map[string]workloadObservationWitness, len(machineIDs))
	for _, machineID := range machineIDs {
		var witness workloadObservationWitness
		var received, evidence string
		err := s.db.QueryRow(`SELECT verdict,received_at,evidence_at,openclaw_present,running_version,workload_policy_token,policy_valid
		 FROM workload_observation_witness WHERE machine_id=?`, machineID).
			Scan(&witness.verdict, &received, &evidence, &witness.openClawPresent, &witness.runningVersion,
				&witness.policyToken, &witness.policyValid)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: workload observation witness for %s: %w", machineID, err)
		}
		witness.receivedAt = parseTime(received)
		witness.evidenceAt = parseTime(evidence)
		out[machineID] = witness
	}
	return out, nil
}

// workloadObservationCoverage 證明 canary cohort 完成後的每一段時間都有 Hub
// 真正收到的同批健康證據。只驗 latest 會讓 Hub outage、睡眠喚醒後的 clock
// jump、短暫版本漂移或 policy 切換，被最後一批綠燈完全洗掉。
//
// evidence_id 是 ingest 順序；時間戳仍必須單調，避免 Hub clock 往回走時用
// 字串排序重排歷史。這張 ledger 永久 append-only；deployment 完成時保存的 cursor
// 把 canary 前後分開，舊歷史不能被拿來補 canary 完成後的覆蓋。
func (s *Store) workloadObservationCoverage(machineID, displayName, version string,
	policy activeWorkloadPolicy, afterEvidenceID int64, since, now time.Time,
) (bool, string, error) {
	since = since.UTC().Truncate(time.Second)
	now = now.UTC().Truncate(time.Second)
	if since.IsZero() || now.Before(since) {
		return false, "canary workload 觀測窗口的時間無效", nil
	}
	if !policy.published || policy.generation <= 0 || !policy.valid {
		return false, "active workload policy 無效，無法驗證連續觀測", nil
	}
	expectedToken := workloadPolicyToken(policy.fingerprint, policy.generation, displayName)
	rows, err := s.db.Query(`SELECT received_at,evidence_at,verdict,openclaw_present,
		 running_version,workload_policy_token,policy_valid
		 FROM workload_observation_evidence
		 WHERE machine_id=? AND evidence_id>? ORDER BY evidence_id`, machineID, afterEvidenceID)
	if err != nil {
		return false, "", fmt.Errorf("store: workload observation coverage for %s: %w", machineID, err)
	}
	defer rows.Close()

	var started bool
	var previousReceived, previousEvidence time.Time
	for rows.Next() {
		var receivedRaw, evidenceRaw, verdict, runningVersion, policyToken string
		var openClawPresent, policyValid bool
		if err := rows.Scan(&receivedRaw, &evidenceRaw, &verdict, &openClawPresent,
			&runningVersion, &policyToken, &policyValid); err != nil {
			return false, "", fmt.Errorf("store: scan workload observation coverage for %s: %w", machineID, err)
		}
		receivedAt := parseTime(receivedRaw)
		if receivedAt.IsZero() {
			return false, "workload 觀測 ledger 有無效 received_at，時間覆蓋不可信", nil
		}
		if receivedAt.After(now) {
			return false, "workload 觀測 ledger 的 received_at 晚於目前判斷時間", nil
		}
		if receivedAt.Before(since) {
			return false, "canary 完成後 append 的 workload 觀測時間早於完成時間，Hub 時鐘順序不可信", nil
		}
		if !started {
			started = true
			if receivedAt.Sub(since) > state.ObservationStale {
				return false, fmt.Sprintf("canary 完成後有超過 %s 沒有 workload 觀測", state.ObservationStale), nil
			}
		} else {
			switch {
			case receivedAt.Before(previousReceived):
				return false, "workload 觀測 ledger 的 Hub 時間倒退，連續覆蓋不可信", nil
			case receivedAt.Sub(previousReceived) > state.ObservationStale:
				return false, fmt.Sprintf("workload 觀測中間有超過 %s 的空窗", state.ObservationStale), nil
			}
		}

		evidenceAt := parseTime(evidenceRaw)
		switch {
		case verdict == workloadWitnessFailure:
			return false, "canary 期間有一批 workload 觀測明確失敗", nil
		case verdict != workloadWitnessHealthy:
			return false, "canary 期間有一批 workload 觀測證據不完整", nil
		case !policyValid || policyToken == "" || policyToken != expectedToken:
			return false, "canary 期間 workload policy generation/token 曾經不同", nil
		case !openClawPresent:
			return false, "canary 期間有一批觀測回報 OpenClaw 不存在", nil
		case runningVersion == "" || runningVersion != version:
			return false, fmt.Sprintf("canary 期間 workload 版本曾是 %q，不是 %s", runningVersion, version), nil
		case evidenceAt.IsZero():
			return false, "canary 期間有一批 workload 觀測沒有可信 evidence_at", nil
		case evidenceAt.After(now):
			return false, "workload 觀測 ledger 的 evidence_at 晚於目前判斷時間", nil
		case evidenceAt.After(receivedAt):
			return false, "workload 觀測的 evidence_at 晚於 Hub received_at", nil
		// 這一臂同時涵蓋 canary 完成後第一列：同列的 received 不早於 since，
		// 因此 evidence 一旦落後 since 超過一個 stale 窗，received 落後 evidence
		// 也必然超過一個 stale 窗，這一臂會先開火。
		case receivedAt.Sub(evidenceAt) > state.ObservationStale:
			return false, fmt.Sprintf("workload 觀測的量測證據落後 Hub 收件超過 %s", state.ObservationStale), nil
		case !previousEvidence.IsZero() && !evidenceAt.After(previousEvidence):
			return false, "workload 觀測的 evidence_at 沒有嚴格向前，連續覆蓋不可信", nil
		case !previousEvidence.IsZero() && evidenceAt.Sub(previousEvidence) > state.ObservationStale:
			return false, fmt.Sprintf("workload 量測證據中間有超過 %s 的空窗", state.ObservationStale), nil
		}
		previousReceived, previousEvidence = receivedAt, evidenceAt
	}
	if err := rows.Err(); err != nil {
		return false, "", fmt.Errorf("store: workload observation coverage rows for %s: %w", machineID, err)
	}
	if !started {
		return false, "canary 完成後沒有 workload 觀測覆蓋", nil
	}
	if now.Sub(previousReceived) > state.ObservationStale || now.Sub(previousEvidence) > state.ObservationStale {
		return false, fmt.Sprintf("最近 %s 內沒有 workload 觀測覆蓋", state.ObservationStale), nil
	}
	return true, "", nil
}

type activeWorkloadPolicy struct {
	fingerprint string
	generation  int64
	valid       bool
	published   bool
}

type workloadPolicyQueryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

func activeWorkloadPolicyFrom(q workloadPolicyQueryRower) (activeWorkloadPolicy, error) {
	var policy activeWorkloadPolicy
	err := q.QueryRow(`SELECT expectations_fingerprint,generation,valid FROM active_workload_policy WHERE singleton=1`).
		Scan(&policy.fingerprint, &policy.generation, &policy.valid)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, nil
	}
	if err != nil {
		return policy, fmt.Errorf("store: active workload policy: %w", err)
	}
	policy.published = true
	return policy, nil
}

func (s *Store) activeWorkloadPolicy() (activeWorkloadPolicy, error) {
	policy, err := activeWorkloadPolicyFrom(s.db)
	if err != nil || !s.expectsLoaded {
		return policy, err
	}
	fingerprint, valid, err := expectationsPolicyFingerprint(s.expects)
	if err != nil {
		return policy, err
	}
	if !policy.published || policy.generation <= 0 ||
		policy.fingerprint != fingerprint || policy.valid != valid {
		return policy, ErrWorkloadPolicyIdentityMismatch
	}
	return policy, nil
}

func workloadWitnessReadiness(witness workloadObservationWitness, policy activeWorkloadPolicy,
	displayName, version string, now time.Time,
) (bool, string) {
	expectedToken := workloadPolicyToken(policy.fingerprint, policy.generation, displayName)
	switch {
	case !policy.published:
		return false, "Hub 還沒有發布 active workload expectations policy"
	case policy.generation <= 0:
		return false, "Hub 的 active workload expectations policy generation 無效"
	case !policy.valid:
		return false, "Hub 的 active workload expectations policy 無效"
	case witness.receivedAt.IsZero():
		return false, "還沒有同一批證據完整的 workload observation"
	case witness.evidenceAt.IsZero():
		return false, "最新 workload observation 沒有可信的量測時間"
	case witness.policyToken == "" || witness.policyToken != expectedToken:
		return false, "workload expectations 已變更，還沒有按新規則完成一批觀測"
	case !witness.policyValid:
		return false, "最新 workload observation 是在無效 expectations policy 下量的"
	case witness.verdict == workloadWitnessFailure:
		return false, "最新同一批 workload observation 明確失敗"
	case witness.verdict != workloadWitnessHealthy:
		return false, "最新同一批 workload observation 證據不完整"
	case !witness.openClawPresent:
		return false, "workload witness 對應的 OpenClaw observation 明確不存在"
	case witness.runningVersion == "" || witness.runningVersion != version:
		return false, fmt.Sprintf("workload witness 的 OpenClaw 版本是 %q，不是 %s", witness.runningVersion, version)
	case witness.evidenceAt.After(now.UTC()):
		return false, "workload witness 的 evidence_at 晚於目前判斷時間"
	case now.UTC().Sub(witness.evidenceAt) > state.ObservationStale:
		return false, fmt.Sprintf("workload witness 不新鮮（門檻 %s）", state.ObservationStale)
	default:
		return true, ""
	}
}

// latestSucceededOpenClawDigests 從每台全部 explicit OpenClaw attempt 判斷最近真正
// succeeded 的 digest。失敗／進行中 attempt 會遮住舊成功；同版換 tarball 仍要擋，
// 只比 RunningDirVersion 看不出這件事。
type appliedOpenClawWitness struct {
	digest     string
	terminalAt time.Time
	jobID      string
	reason     string
}

func (s *Store) latestSucceededOpenClawWitnesses(machineIDs []string) (map[string]appliedOpenClawWitness, error) {
	out := make(map[string]appliedOpenClawWitness, len(machineIDs))
	for _, machineID := range machineIDs {
		rows, err := s.db.Query(`SELECT j.job_id,j.state,j.artifact_digest,j.terminal_at,d.spec,d.resource_kind,d.resource_id
		 FROM jobs j JOIN desired_state d ON d.desired_id=j.desired_id
		 WHERE j.machine_id=?
		 ORDER BY j.rowid DESC`, machineID)
		if err != nil {
			return nil, fmt.Errorf("store: latest succeeded OpenClaw job for %s: %w", machineID, err)
		}
		var candidate appliedOpenClawWitness
		var haveBoundary, blockedReason string
		for rows.Next() {
			var digest, terminalAt sql.NullString
			var jobID, raw, resourceKind, resourceID, rawState string
			if err := rows.Scan(&jobID, &rawState, &digest, &terminalAt, &raw, &resourceKind, &resourceID); err != nil {
				rows.Close()
				return nil, err
			}
			var spec model.OpenClawSpec
			// 無法辨認的舊雜訊（包含 legacy noop）可以略過；一旦明確是
			// OpenClaw，缺少／不合法的 material 就必須遮住更舊 witness。
			if json.Unmarshal([]byte(raw), &spec) != nil || spec.Kind != "openclaw" {
				continue
			}
			jobState := deploy.JobState(rawState)
			// rejected 是 mutation 前的 admission 結果，不代表機器曾換版，可以越過；
			// 其他非終態 attempt 還會變，任何一筆存在都讓 witness fail closed。
			if jobState == deploy.Rejected {
				continue
			}
			if !deploy.IsTerminal(jobState) {
				if blockedReason == "" {
					blockedReason = fmt.Sprintf("OpenClaw 工作單 %s 仍是非終態 %s", shortStoreID(jobID, 8), jobState)
				}
				continue
			}
			// terminal 卻沒有可排序的時間是壞帳本；尤其 succeeded 不能拿
			// created_at 猜先後，再跟另一筆成功拼成 witness。
			if !terminalAt.Valid || parseTime(terminalAt.String).IsZero() {
				if blockedReason == "" {
					blockedReason = fmt.Sprintf("OpenClaw 工作單 %s 缺少有效 terminal_at，時間順序不可信", shortStoreID(jobID, 8))
				}
				continue
			}
			if haveBoundary != "" {
				continue
			}
			haveBoundary = jobID
			candidate.jobID = jobID
			candidate.terminalAt = parseTime(terminalAt.String)
			if jobState != deploy.Succeeded {
				candidate.reason = fmt.Sprintf("最近一張 explicit OpenClaw 工作單 %s 是 %s，不是 succeeded",
					shortStoreID(jobID, 8), jobState)
				continue
			}
			if resourceKind == "openclaw" && resourceID == "openclaw" && spec.Artifact != nil &&
				artifact.ValidSHA256Hex(spec.Artifact.SHA256) &&
				digest.Valid && digest.String == "sha256:"+spec.Artifact.SHA256 {
				candidate.digest = spec.Artifact.SHA256
			} else {
				candidate.reason = fmt.Sprintf("最近一張 explicit OpenClaw 工作單 %s 的 material 不完整或不一致",
					shortStoreID(jobID, 8))
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if blockedReason != "" {
			candidate.digest = ""
			candidate.reason = blockedReason
		}
		if haveBoundary != "" || blockedReason != "" {
			out[machineID] = candidate
		}
	}
	return out, nil
}

// latestSucceededOpenClawDigests 保留給只需要檢查 digest boundary 的 caller；
// promote 本身還會用 jobID 驗證最新 witness 必須屬於選中的 canary lineage。
func (s *Store) latestSucceededOpenClawDigests(machineIDs []string) (map[string]string, error) {
	witnesses, err := s.latestSucceededOpenClawWitnesses(machineIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(witnesses))
	for machineID, witness := range witnesses {
		out[machineID] = witness.digest
	}
	return out, nil
}

func hasCanarySilentFinding(findings []state.Finding) bool {
	for _, finding := range findings {
		if finding.Kind == "workload" && !finding.Advisory {
			return true
		}
	}
	return false
}
