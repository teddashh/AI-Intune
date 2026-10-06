package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

var deployTestNow = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

func TestJobEventProvenanceMigrationKeepsLegacyRowsExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-job-events.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.nowFn = func() time.Time { return deployTestNow }
	registerDeployMachine(t, s, "machine-legacy-event")
	jobID := newJobForDeployTest(t, s, "machine-legacy-event")
	if _, err := s.DB().Exec(`INSERT INTO job_events
 (event_id,job_id,seq,phase,occurred_at,received_at,payload)
 VALUES ('legacy-event',?,1,'start',?,?, '{}')`, jobID,
		fmtTime(deployTestNow), fmtTime(deployTestNow)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP TRIGGER ` + jobEventLegacyProducerTrigger); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{
		"provenance_recorded", "authority", "evidence_role", "producer_id", "producer_kind",
	} {
		if _, err := s.DB().Exec(`ALTER TABLE job_events DROP COLUMN ` + column); err != nil {
			t.Fatalf("drop legacy column %s: %v", column, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExistingLedger(path); err != nil {
		t.Fatalf("legacy job event shape rejected: %v", err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var producerKind, producerID, evidenceRole, authority string
	var recorded bool
	if err := s.DB().QueryRow(`SELECT producer_kind,producer_id,evidence_role,authority,
 provenance_recorded FROM job_events WHERE event_id='legacy-event'`).Scan(
		&producerKind, &producerID, &evidenceRole, &authority, &recorded); err != nil {
		t.Fatal(err)
	}
	if producerKind != JobEventProducerExecutorAgent || producerID != "machine-legacy-event" ||
		evidenceRole != JobEventRoleExecutor || authority != JobEventAuthorityMachineLease || recorded {
		t.Fatalf("legacy event provenance kind=%q id=%q role=%q authority=%q recorded=%t",
			producerKind, producerID, evidenceRole, authority, recorded)
	}
	if err := ValidateExistingLedger(path); err != nil {
		t.Fatalf("migrated job event shape rejected: %v", err)
	}
}

func TestVerificationProvenanceMigrationKeepsLegacyRowsExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-verification.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.nowFn = func() time.Time { return deployTestNow }
	registerDeployMachine(t, s, "machine-legacy-proof")
	jobID := newJobForDeployTest(t, s, "machine-legacy-proof")
	if _, err := s.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,passed,verified_at)
 VALUES ('legacy-proof',?,?,?,?,1,?)`, jobID, "machine-legacy-proof", "health", "true",
		fmtTime(deployTestNow)); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"verifier_id", "observed_version", "observed_digest", "received_at", "provenance_recorded", "authority", "evidence_role", "producer_id", "producer_kind"} {
		if _, err := s.DB().Exec(`ALTER TABLE verification_results DROP COLUMN ` + column); err != nil {
			t.Fatalf("drop legacy column %s: %v", column, err)
		}
	}
	if _, err := s.DB().Exec(`DROP TABLE verifiers`); err != nil {
		t.Fatalf("drop legacy-absent verifier table: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExistingLedger(path); err != nil {
		t.Fatalf("legacy verification shape rejected: %v", err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var producerKind, producerID, evidenceRole, authority, receivedAt, observedDigest, observedVersion, verifierID string
	var recorded bool
	if err := s.DB().QueryRow(`SELECT producer_kind,producer_id,evidence_role,authority,
	 provenance_recorded,received_at,observed_digest,observed_version,verifier_id
	 FROM verification_results WHERE verification_id='legacy-proof'`).
		Scan(&producerKind, &producerID, &evidenceRole, &authority, &recorded, &receivedAt,
			&observedDigest, &observedVersion, &verifierID); err != nil {
		t.Fatal(err)
	}
	if producerKind != JobVerificationProducerExecutorAgent || producerID != "machine-legacy-proof" ||
		evidenceRole != JobVerificationRoleExecutor || authority != JobVerificationAuthorityMachineLease ||
		recorded || receivedAt != "" || observedDigest != "" || observedVersion != "" || verifierID != "" {
		t.Fatalf("legacy provenance kind=%q id=%q role=%q authority=%q recorded=%t received=%q observed=%q verifier=%q",
			producerKind, producerID, evidenceRole, authority, recorded, receivedAt, observedDigest, verifierID)
	}
	if err := ValidateExistingLedger(path); err != nil {
		t.Fatalf("migrated verification shape rejected: %v", err)
	}
}

func newDeployTestStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	s.nowFn = func() time.Time { return deployTestNow }
	return s
}

func registerDeployMachine(t *testing.T, s *Store, machineID string) {
	t.Helper()
	if _, err := s.DB().Exec(`
INSERT INTO machine_registry (machine_id, display_name, created_at)
VALUES (?, ?, ?)`, machineID, machineID, fmtTime(deployTestNow)); err != nil {
		t.Fatalf("建立測試機器 %q 失敗：%v", machineID, err)
	}
}

func desiredForDeployTest(t *testing.T, s *Store) (string, deploy.Revision) {
	t.Helper()
	id, rev, err := s.CreateDesiredState("machine", "machine-a", "app", "openclaw", `{}`, "測試者")
	if err != nil {
		t.Fatalf("建立測試期望狀態失敗：%v", err)
	}
	return id, rev
}

func newJobForDeployTest(t *testing.T, s *Store, machineID string) string {
	t.Helper()
	desiredID, rev := desiredForDeployTest(t, s)
	jobID, err := s.CreateJob(machineID, desiredID, rev, NewJob{})
	if err != nil {
		t.Fatalf("建立測試工作單失敗：%v", err)
	}
	return jobID
}

// ⚠ 守住配號若拆成先 SELECT 再 UPDATE，併發呼叫會拿到重複 revision 的錯。
func TestAllocateRevisionConcurrentWithoutDuplicatesOrGaps(t *testing.T) {
	s := newDeployTestStore(t)
	const n = 24
	start := make(chan struct{})
	revisions := make(chan deploy.Revision, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rev, err := s.AllocateRevision("app:openclaw")
			if err != nil {
				errs <- err
				return
			}
			revisions <- rev
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(revisions)
	for err := range errs {
		t.Errorf("併發配號失敗：%v", err)
	}

	got := make([]int, 0, n)
	for rev := range revisions {
		got = append(got, int(rev))
	}
	sort.Ints(got)
	if len(got) != n {
		t.Fatalf("成功配出的號碼有 %d 個，預期 %d 個", len(got), n)
	}
	for i, rev := range got {
		if want := i + 1; rev != want {
			t.Fatalf("排序後第 %d 個 revision 是 %d，預期 %d；有跳號或重號", i, rev, want)
		}
	}
}

// ⚠ 守住一種資源配號時誤推進另一種資源計數器的錯。
func TestAllocateRevisionScopesAreIndependent(t *testing.T) {
	s := newDeployTestStore(t)
	for i, want := range []deploy.Revision{1, 2} {
		got, err := s.AllocateRevision("app:openclaw")
		if err != nil {
			t.Fatalf("替第一個範圍第 %d 次配號失敗：%v", i+1, err)
		}
		if got != want {
			t.Fatalf("第一個範圍第 %d 次拿到 revision %d，預期 %d", i+1, got, want)
		}
	}
	got, err := s.AllocateRevision("policy:agent-config")
	if err != nil {
		t.Fatalf("替第二個範圍配號失敗：%v", err)
	}
	if got != 1 {
		t.Fatalf("第二個範圍第一次拿到 revision %d，預期 1", got)
	}
}

// ⚠ 守住接受重疊 tag 或空 scope，並在拒絕前已消耗 revision 的錯。
func TestCreateDesiredStateRejectsBadScopeWithoutWritingOrAllocating(t *testing.T) {
	s := newDeployTestStore(t)
	for _, scopeType := range []string{"tag", ""} {
		if _, _, err := s.CreateDesiredState(scopeType, "gpu", "app", "openclaw", `{}`, "測試者"); !errors.Is(err, ErrBadScope) {
			t.Errorf("scope_type=%q 回傳 %v，預期 ErrBadScope", scopeType, err)
		}
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM desired_state`); got != 0 {
		t.Fatalf("不合法 scope 寫入了 %d 筆期望狀態，預期 0", got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM revision_counters`); got != 0 {
		t.Fatalf("不合法 scope 寫入了 %d 筆配號計數器，預期 0", got)
	}
	_, rev, err := s.CreateDesiredState("channel", "canary", "app", "openclaw", `{}`, "測試者")
	if err != nil {
		t.Fatalf("不合法 scope 被拒絕後建立合法期望狀態失敗：%v", err)
	}
	if rev != 1 {
		t.Fatalf("不合法 scope 消耗了 revision，合法的第一筆拿到 %d，預期 1", rev)
	}
}

func TestCreateDesiredStateRejectsDirectManagedCatalogKindsBeforeAllocatingRevision(t *testing.T) {
	s := newDeployTestStore(t)
	beforeDesired := countRows(t, s, `SELECT COUNT(*) FROM desired_state`)
	beforeCounters := countRows(t, s, `SELECT COUNT(*) FROM revision_counters`)

	for _, kind := range []string{"openclaw", "node-runtime"} {
		if _, _, err := s.CreateDesiredState("machine", "machine-a", kind, kind,
			`{"kind":"`+kind+`","version":"24.15.0"}`, "test"); !errors.Is(err, ErrManagedCatalogDeploymentRequired) {
			t.Fatalf("direct %s desired state err=%v", kind, err)
		}
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM desired_state`); got != beforeDesired {
		t.Fatalf("被拒絕後 desired_state=%d，原本 %d", got, beforeDesired)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM revision_counters`); got != beforeCounters {
		t.Fatalf("被拒絕後 revision_counters=%d，原本 %d", got, beforeCounters)
	}
	_, rev, err := s.CreateDesiredState("machine", "machine-a", "openclaw", "openclaw", `{"kind":"noop"}`, "test")
	if err != nil || rev != 1 {
		t.Fatalf("noop 應保留且拿 revision 1：rev=%d err=%v", rev, err)
	}
}

// ⚠ 守住計數器列缺失或被改成 0 時，共用原語重用既有 revision 的錯。
func TestCreateDesiredStateRejectsAllocatedRevisionThatAlreadyExists(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate string
		args   []any
	}{
		{
			name:   "missing counter",
			mutate: `DELETE FROM revision_counters WHERE resource_scope=?`,
			args:   []any{"app:openclaw"},
		},
		{
			name:   "counter reset to zero",
			mutate: `UPDATE revision_counters SET current_revision=0 WHERE resource_scope=?`,
			args:   []any{"app:openclaw"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newDeployTestStore(t)
			registerDeployMachine(t, s, "machine-a")
			firstID, firstRev, err := s.CreateDesiredState("machine", "machine-a", "app", "openclaw", `{}`, "測試者")
			if err != nil || firstRev != 1 || firstID == "" {
				t.Fatalf("first desired state id=%q rev=%d err=%v", firstID, firstRev, err)
			}
			if _, err := s.DB().Exec(test.mutate, test.args...); err != nil {
				t.Fatal(err)
			}
			beforeDesired := countRows(t, s, `SELECT COUNT(*) FROM desired_state`)
			beforeJobs := countRows(t, s, `SELECT COUNT(*) FROM jobs`)
			beforeAudit := countRows(t, s, `SELECT COUNT(*) FROM audit_log`)
			beforeReceipts := countRows(t, s, `SELECT COUNT(*) FROM operator_idempotency`)
			beforeCounters := countRows(t, s, `SELECT COUNT(*) FROM revision_counters`)
			beforeRevision := countRows(t, s, `SELECT COALESCE(MAX(current_revision),-1)
				FROM revision_counters WHERE resource_scope=?`, "app:openclaw")

			_, _, err = s.CreateDesiredState("machine", "machine-a", "app", "openclaw", `{"n":2}`, "測試者")
			if !errors.Is(err, ErrDesiredRevisionExists) {
				t.Fatalf("second create err=%v, want ErrDesiredRevisionExists", err)
			}
			if countRows(t, s, `SELECT COUNT(*) FROM desired_state`) != beforeDesired ||
				countRows(t, s, `SELECT COUNT(*) FROM jobs`) != beforeJobs ||
				countRows(t, s, `SELECT COUNT(*) FROM audit_log`) != beforeAudit ||
				countRows(t, s, `SELECT COUNT(*) FROM operator_idempotency`) != beforeReceipts ||
				countRows(t, s, `SELECT COUNT(*) FROM revision_counters`) != beforeCounters ||
				countRows(t, s, `SELECT COALESCE(MAX(current_revision),-1)
					FROM revision_counters WHERE resource_scope=?`, "app:openclaw") != beforeRevision {
				t.Fatal("rejected create wrote desired state, job, revision, receipt, or audit")
			}
			if countRows(t, s, `SELECT COUNT(*) FROM desired_state
				WHERE resource_kind='app' AND resource_id='openclaw' AND revision=1`) != 1 {
				t.Fatal("rejected create reused revision 1")
			}
		})
	}
}

func TestCreateDesiredStateKeepsMonotonicRevisionsAndIndependentResources(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")

	var got []deploy.Revision
	for i := 0; i < 3; i++ {
		_, rev, err := s.CreateDesiredState("machine", "machine-a", "app", "openclaw", `{}`, "測試者")
		if err != nil {
			t.Fatalf("sequential create %d: %v", i+1, err)
		}
		got = append(got, rev)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("sequential revisions=%v, want 1 2 3", got)
	}

	_, other, err := s.CreateDesiredState("machine", "machine-a", "policy", "agent-config", `{}`, "測試者")
	if err != nil || other != 1 {
		t.Fatalf("independent resource revision=%d err=%v, want 1", other, err)
	}

	_, channelRev, err := s.CreateDesiredState("channel", "canary", "app", "openclaw", `{}`, "測試者")
	if err != nil || channelRev != 4 {
		t.Fatalf("same resource channel scope revision=%d err=%v, want 4", channelRev, err)
	}
	_, otherChannel, err := s.CreateDesiredState("channel", "stable", "policy", "fleet-banner", `{}`, "測試者")
	if err != nil || otherChannel != 1 {
		t.Fatalf("different resource channel scope revision=%d err=%v, want 1", otherChannel, err)
	}
}

func TestCreateJobAllowsMultipleJobsOnSameChannelDesiredState(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "machine-b")
	desiredID, rev, err := s.CreateDesiredState("channel", "canary", "app", "openclaw", `{}`, "測試者")
	if err != nil || rev != 1 {
		t.Fatalf("channel desired state rev=%d err=%v", rev, err)
	}
	firstJob, err := s.CreateJob("machine-a", desiredID, rev, NewJob{})
	if err != nil {
		t.Fatalf("first channel job: %v", err)
	}
	secondJob, err := s.CreateJob("machine-b", desiredID, rev, NewJob{})
	if err != nil {
		t.Fatalf("second channel job: %v", err)
	}
	if firstJob == "" || secondJob == "" || firstJob == secondJob {
		t.Fatalf("channel jobs=%q %q", firstJob, secondJob)
	}
	if countRows(t, s, `SELECT COUNT(*) FROM jobs WHERE desired_id=?`, desiredID) != 2 {
		t.Fatal("channel-scoped desired state did not keep both jobs")
	}
}

func TestCreateJobRejectsExistingDirectManagedCatalogDesiredWithoutWriting(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	beforeJobs := countRows(t, s, `SELECT COUNT(*) FROM jobs`)
	beforeCounters := countRows(t, s, `SELECT COUNT(*) FROM revision_counters`)
	for index, kind := range []string{"openclaw", "node-runtime"} {
		desiredID := "raw-" + kind
		if _, err := s.DB().Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES (?,?,?,?,?,?,?,?,?)`, desiredID, "machine", "machine-a", kind, kind, 77+index,
			`{"kind":"`+kind+`","version":"24.15.0"}`, fmtTime(deployTestNow), "legacy"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateJob("machine-a", desiredID, deploy.Revision(77+index), NewJob{}); !errors.Is(err, ErrManagedCatalogDeploymentRequired) {
			t.Fatalf("direct %s job err=%v", kind, err)
		}
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM jobs`); got != beforeJobs {
		t.Fatalf("被拒絕後 jobs=%d，原本 %d", got, beforeJobs)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM revision_counters`); got != beforeCounters {
		t.Fatalf("被拒絕後 revision_counters=%d，原本 %d", got, beforeCounters)
	}
	noopID, noopRevision, err := s.CreateDesiredState("machine", "machine-a", "openclaw", "openclaw", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatalf("建立 noop desired state：%v", err)
	}
	if _, err := s.CreateJob("machine-a", noopID, noopRevision, NewJob{}); err != nil {
		t.Fatalf("noop CreateJob 不應被 OpenClaw guard 擋住：%v", err)
	}
}

// ⚠⚠ 守住只靠呼叫端事後比對歸屬，讓 A 能用自己的身分讀到 B 工作單的錯；也守住 job_id 列舉。
func TestJobForMachineHidesOtherMachinesJobLikeMissingJob(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "machine-b")
	desiredID, rev := desiredForDeployTest(t, s)
	jobID, err := s.CreateJob("machine-b", desiredID, rev, NewJob{})
	if err != nil {
		t.Fatalf("替 B 建立工作單失敗：%v", err)
	}

	_, wrongOwnerErr := s.JobForMachine(jobID, "machine-a")
	_, missingErr := s.JobForMachine("不存在的工作單", "machine-a")
	if wrongOwnerErr != ErrJobNotFound {
		t.Fatalf("A 查 B 的工作單回傳 %v，預期 ErrJobNotFound", wrongOwnerErr)
	}
	if missingErr != ErrJobNotFound {
		t.Fatalf("查不存在的工作單回傳 %v，預期 ErrJobNotFound", missingErr)
	}
	if wrongOwnerErr != missingErr {
		t.Fatalf("歸屬不符與不存在回傳不同錯誤：%v、%v", wrongOwnerErr, missingErr)
	}
}

// ⚠ 守住替已退役機器開單，留下永遠不會被領走之 pending 工作單的錯。
func TestCreateJobRejectsRetiredMachine(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, rev := desiredForDeployTest(t, s)
	if err := s.RetireMachine("machine-a", deployTestNow); err != nil {
		t.Fatalf("退役測試機器失敗：%v", err)
	}
	if _, err := s.CreateJob("machine-a", desiredID, rev, NewJob{}); !errors.Is(err, ErrMachineRetired) {
		t.Fatalf("替已退役機器開單回傳 %v，預期 ErrMachineRetired", err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM jobs`); got != 0 {
		t.Fatalf("已退役機器仍被寫入 %d 張工作單，預期 0", got)
	}
}

// ⚠ 守住用插入順序代替 revision 排序，導致先領到較新工作單的錯。
func TestNextJobForMachineChoosesLowestRevision(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, _ := desiredForDeployTest(t, s)
	for _, rev := range []deploy.Revision{43, 42, 41} {
		if _, err := s.CreateJob("machine-a", desiredID, rev, NewJob{}); err != nil {
			t.Fatalf("建立 revision %d 工作單失敗：%v", rev, err)
		}
	}
	job, ok, err := s.NextJobForMachine("machine-a")
	if err != nil {
		t.Fatalf("讀取下一張工作單失敗：%v", err)
	}
	if !ok {
		t.Fatal("有三張尚未開始的工作單，卻回傳沒有工作單")
	}
	if job.Revision != 41 {
		t.Fatalf("下一張工作單 revision=%d，預期 41", job.Revision)
	}
}

// ⚠ 守住 created_at 時鐘順序與 revision 相反時，錯把時鐘當成配號順序的錯。
func TestNextJobForMachineIgnoresCreatedAtOrder(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, _ := desiredForDeployTest(t, s)
	createdTimes := map[deploy.Revision]time.Time{
		41: deployTestNow.Add(3 * time.Hour),
		42: deployTestNow.Add(2 * time.Hour),
		43: deployTestNow.Add(1 * time.Hour),
	}
	for _, rev := range []deploy.Revision{41, 42, 43} {
		at := createdTimes[rev]
		s.nowFn = func() time.Time { return at }
		if _, err := s.CreateJob("machine-a", desiredID, rev, NewJob{}); err != nil {
			t.Fatalf("建立 revision %d 工作單失敗：%v", rev, err)
		}
	}
	job, ok, err := s.NextJobForMachine("machine-a")
	if err != nil {
		t.Fatalf("讀取下一張工作單失敗：%v", err)
	}
	if !ok {
		t.Fatal("刻意反轉時間後有三張工作單，卻回傳沒有工作單")
	}
	if job.Revision != 41 {
		t.Fatalf("created_at 與 revision 反向時拿到 revision %d，預期 41", job.Revision)
	}
}

// ⚠ 守住把「沒有工作單」誤報成資料庫錯誤，讓正常輪詢一直失敗的錯。
func TestNextJobForMachineReturnsFalseWhenEmpty(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	job, ok, err := s.NextJobForMachine("machine-a")
	if err != nil {
		t.Fatalf("沒有工作單時回傳錯誤：%v", err)
	}
	if ok {
		t.Fatalf("沒有工作單時卻回傳 true，工作單為 %+v", job)
	}
}

// The common poll must answer from a read-only snapshot. A write transaction
// already holding the single writer connection must not make that poll wait.
func TestNextJobForMachineDoesNotWaitOnTheWriter(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, _ := desiredForDeployTest(t, s)
	if _, err := s.CreateJob("machine-a", desiredID, 41, NewJob{}); err != nil {
		t.Fatalf("建立 revision 41 工作單失敗：%v", err)
	}
	held, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("佔住 writer：%v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = held.Rollback()
		}
	}
	defer release()

	type result struct {
		job Job
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		job, ok, err := s.NextJobForMachine("machine-a")
		done <- result{job, ok, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("writer 被佔住時讀下一張工作單失敗：%v", got.err)
		}
		if !got.ok || got.job.Revision != 41 {
			t.Fatalf("writer 被佔住時下一張工作單 = ok:%v revision:%d，預期 revision 41", got.ok, got.job.Revision)
		}
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("NextJobForMachine 在 writer 被佔住時超過 1 秒還沒回來")
	}
}

func TestCreateJobStoresOrderedMachineLocalPrerequisitesAtomically(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "machine-b")
	desiredID, rev := desiredForDeployTest(t, s)
	first, err := s.CreateJob("machine-a", desiredID, rev, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateJob("machine-a", desiredID, rev+1, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CreateJob("machine-b", desiredID, rev, NewJob{})
	if err != nil {
		t.Fatal(err)
	}

	before := countRows(t, s, `SELECT COUNT(*) FROM jobs`)
	invalid := []struct {
		name string
		ids  []string
		want error
	}{
		{name: "blank", ids: []string{""}, want: ErrJobPrerequisiteInvalid},
		{name: "duplicate", ids: []string{first, first}, want: ErrJobPrerequisiteInvalid},
		{name: "over limit", ids: make([]string, maxJobPrerequisites+1), want: ErrJobPrerequisiteInvalid},
		{name: "missing", ids: []string{"missing-job"}, want: ErrJobPrerequisiteNotFound},
		{name: "other machine", ids: []string{foreign}, want: ErrJobPrerequisiteWrongMachine},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := s.CreateJob("machine-a", desiredID, rev+2,
				NewJob{PrerequisiteJobIDs: test.ids}); !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
			if got := countRows(t, s, `SELECT COUNT(*) FROM jobs`); got != before {
				t.Fatalf("invalid graph wrote a job: count=%d want=%d", got, before)
			}
		})
	}

	child, err := s.CreateJob("machine-a", desiredID, rev+2,
		NewJob{PrerequisiteJobIDs: []string{second, first}})
	if err != nil {
		t.Fatal(err)
	}
	edges, err := s.JobPrerequisites(child)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 || edges[0].JobID != child || edges[0].PrerequisiteJobID != second ||
		edges[0].Position != 0 || edges[1].PrerequisiteJobID != first || edges[1].Position != 1 {
		t.Fatalf("edges=%+v want ordered [%s %s]", edges, second, first)
	}
}

func TestJobDependencyTriggersRejectCyclesMutationAndUnsafeClaims(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "machine-b")
	desiredID, rev := desiredForDeployTest(t, s)
	a, _ := s.CreateJob("machine-a", desiredID, rev, NewJob{})
	b, _ := s.CreateJob("machine-a", desiredID, rev+1, NewJob{})
	c, _ := s.CreateJob("machine-a", desiredID, rev+2, NewJob{})
	foreign, _ := s.CreateJob("machine-b", desiredID, rev, NewJob{})

	if _, err := s.DB().Exec(`INSERT INTO job_dependencies
 (job_id,prerequisite_job_id,position) VALUES (?,?,0)`, a, b); err != nil {
		t.Fatalf("valid direct edge: %v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO job_dependencies
 (job_id,prerequisite_job_id,position) VALUES (?,?,0)`, b, a); err == nil ||
		!strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error=%v", err)
	}
	if _, err := s.DB().Exec(`UPDATE job_dependencies SET position=1 WHERE job_id=?`, a); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("update error=%v", err)
	}
	if _, err := s.DB().Exec(`DELETE FROM job_dependencies WHERE job_id=?`, a); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("delete error=%v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO job_dependencies
 (job_id,prerequisite_job_id,position) VALUES (?,?,0)`, foreign, a); err == nil ||
		!strings.Contains(err.Error(), "same machine") {
		t.Fatalf("cross-machine error=%v", err)
	}
	if _, err := s.DB().Exec(`INSERT INTO job_dependencies
 (job_id,prerequisite_job_id,position) VALUES (?,?,128)`, c, a); err == nil ||
		!strings.Contains(err.Error(), "CHECK constraint") {
		t.Fatalf("position bound error=%v", err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=? WHERE job_id=?`, deploy.Claimed, a); err == nil ||
		!strings.Contains(err.Error(), "not satisfied") {
		t.Fatalf("unsafe direct claim error=%v", err)
	}
	if _, err := s.ClaimJob(a, "machine-a", deployTestNow, time.Minute); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("guarded ClaimJob error=%v want ErrJobNotFound", err)
	}
}

func TestNextJobRequiresSuccessfulPrerequisitesAndKeepsResourceRevisionsIndependent(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	parentDesired, parentRevision, err := s.CreateDesiredState(
		"machine", "machine-a", "runtime", "node", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.CreateJob("machine-a", parentDesired, parentRevision, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	independentDesired, _, err := s.CreateDesiredState(
		"machine", "machine-a", "service", "tailscale", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	independent, err := s.CreateJob("machine-a", independentDesired, 1, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	childDesired, childRevision, err := s.CreateDesiredState(
		"machine", "machine-a", "app", "openclaw", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateJob("machine-a", childDesired, childRevision,
		NewJob{PrerequisiteJobIDs: []string{parent, independent}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(parent, "machine-a", deployTestNow, time.Minute); err != nil {
		t.Fatal(err)
	}

	job, ok, err := s.NextJobForMachine("machine-a")
	if err != nil || !ok || job.JobID != independent {
		t.Fatalf("while parent runs next=%+v ok=%t err=%v want independent %s", job, ok, err, independent)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=?,lease_token=NULL,lease_expires_at=NULL
 WHERE job_id=?`, deploy.Succeeded, fmtTime(deployTestNow), parent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, fmtTime(deployTestNow), independent); err != nil {
		t.Fatal(err)
	}
	job, ok, err = s.NextJobForMachine("machine-a")
	if err != nil || !ok || job.JobID != child {
		t.Fatalf("after prerequisite success next=%+v ok=%t err=%v want child %s", job, ok, err, child)
	}

	// Raw revision values from different resource counters do not reorder their
	// creation sequence.
	otherDesired, _, err := s.CreateDesiredState(
		"machine", "machine-a", "policy", "agent", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.CreateJob("machine-a", otherDesired, 99, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	lowDesired, _, err := s.CreateDesiredState(
		"machine", "machine-a", "config", "shell", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob("machine-a", lowDesired, 1, NewJob{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, fmtTime(deployTestNow), child); err != nil {
		t.Fatal(err)
	}
	job, ok, err = s.NextJobForMachine("machine-a")
	if err != nil || !ok || job.JobID != first {
		t.Fatalf("independent revisions next=%+v ok=%t err=%v want first-created %s", job, ok, err, first)
	}
}

func TestNextJobKeepsLaterResourceRevisionBlockedWhileEarlierRevisionRuns(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, rev := desiredForDeployTest(t, s)
	earlier, err := s.CreateJob("machine-a", desiredID, rev, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	later, err := s.CreateJob("machine-a", desiredID, rev+1, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(earlier, "machine-a", deployTestNow, time.Minute); err != nil {
		t.Fatal(err)
	}
	if job, ok, err := s.NextJobForMachine("machine-a"); err != nil || ok {
		t.Fatalf("next=%+v ok=%t err=%v want later revision blocked", job, ok, err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=?,lease_token=NULL,lease_expires_at=NULL
 WHERE job_id=?`, deploy.Succeeded, fmtTime(deployTestNow), earlier); err != nil {
		t.Fatal(err)
	}
	job, ok, err := s.NextJobForMachine("machine-a")
	if err != nil || !ok || job.JobID != later {
		t.Fatalf("next=%+v ok=%t err=%v want later %s", job, ok, err, later)
	}
}

func TestNextJobCascadesFailedPrerequisiteWithHubEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, rev := desiredForDeployTest(t, s)
	root, err := s.CreateJob("machine-a", desiredID, rev, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateJob("machine-a", desiredID, rev+1,
		NewJob{PrerequisiteJobIDs: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := s.CreateJob("machine-a", desiredID, rev+2,
		NewJob{PrerequisiteJobIDs: []string{child}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Failed, fmtTime(deployTestNow), root); err != nil {
		t.Fatal(err)
	}
	if job, ok, err := s.NextJobForMachine("machine-a"); err != nil || ok {
		t.Fatalf("next=%+v ok=%t err=%v want empty after cascade", job, ok, err)
	}
	for _, jobID := range []string{child, grandchild} {
		job, err := s.Job(jobID)
		if err != nil || job.State != deploy.Rejected || job.TerminalAt == nil ||
			!job.TerminalAt.Equal(deployTestNow) {
			t.Fatalf("cascaded job=%+v err=%v", job, err)
		}
		events, err := s.JobEvents(jobID)
		if err != nil || len(events) != 1 {
			t.Fatalf("events=%+v err=%v", events, err)
		}
		var payload model.JobRejectEventPayload
		if err := json.Unmarshal([]byte(events[0].Payload), &payload); err != nil {
			t.Fatal(err)
		}
		if events[0].Seq != 1 || events[0].Phase != "rejected" ||
			events[0].ProducerKind != JobEventProducerHubScheduler ||
			events[0].ProducerID != "hub" || events[0].EvidenceRole != JobEventRoleScheduler ||
			events[0].Authority != JobEventAuthorityDependencyGraph || !events[0].ProvenanceRecorded ||
			payload.RejectionCode != string(deploy.DependencyFailed) {
			t.Fatalf("event=%+v payload=%+v", events[0], payload)
		}
	}
	if _, _, err := s.NextJobForMachine("machine-a"); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM job_events WHERE job_id IN (?,?)`, child, grandchild); got != 2 {
		t.Fatalf("repeat poll duplicated dependency evidence: %d", got)
	}
}

// ⚠ 守的是「開單時收了 digest 與 irreversible，卻沒有真的寫進資料庫」——
// 那會讓 internal/deploy 那條不可逆路徑變成永遠走不到的死碼，
// 而 agent 拿到的工作單裡沒有可以拿來驗產物的東西（不變量 2 落空）。
func TestCreateJobRoundTripsDigestAndIrreversible(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, rev := desiredForDeployTest(t, s)

	jobID, err := s.CreateJob("machine-a", desiredID, rev, NewJob{
		ArtifactDigest:   "sha256:abc",
		Irreversible:     true,
		ExecutionTimeout: 42,
	})
	if err != nil {
		t.Fatalf("建立工作單失敗：%v", err)
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.ArtifactDigest != "sha256:abc" || !job.Irreversible || job.ExecutionTimeout != 42 {
		t.Errorf("開單時給的東西沒有原樣讀回來：%+v", job)
	}
}

// ⚠ 守的是把「沒有指定 digest」寫成空字串。
// 一個 "" 的 digest 如果被下一層當成有效值，任何產物都會對不上；
// 被當成「不用驗」則不變量 2 整條沒了。NULL 逼下一層明確處理這件事。
func TestCreateJobWithoutDigestStoresNullNotEmptyString(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, rev := desiredForDeployTest(t, s)

	jobID, err := s.CreateJob("machine-a", desiredID, rev, NewJob{})
	if err != nil {
		t.Fatalf("建立工作單失敗：%v", err)
	}
	var isNull bool
	if err := s.DB().QueryRow(
		`SELECT artifact_digest IS NULL FROM jobs WHERE job_id = ?`, jobID).Scan(&isNull); err != nil {
		t.Fatalf("查 artifact_digest 失敗：%v", err)
	}
	if !isNull {
		t.Error("沒有指定 digest 時，資料庫裡不該是空字串，要是 NULL")
	}
	// 沒給逾時就用 schema 的預設值，不是 0 秒。
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.ExecutionTimeout != 900 {
		t.Errorf("沒給執行逾時應該落在預設的 900 秒，拿到 %d", job.ExecutionTimeout)
	}
}

// ⚠⚠ 守住領單若拆成先 SELECT 再 UPDATE，兩個 agent 會同時取得同一張單的錯。
func TestClaimJobConcurrentHasExactlyOneWinner(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")

	start := make(chan struct{})
	tokens := make(chan string, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
			if err != nil {
				errs <- err
				return
			}
			tokens <- token
		}()
	}
	close(start)
	wg.Wait()
	close(tokens)
	close(errs)

	var gotTokens []string
	for token := range tokens {
		gotTokens = append(gotTokens, token)
	}
	var gotErrs []error
	for err := range errs {
		gotErrs = append(gotErrs, err)
	}
	if len(gotTokens) != 1 || len(gotErrs) != 1 {
		t.Fatalf("併發領單得到 %d 個成功、%d 個錯誤，預期各一個", len(gotTokens), len(gotErrs))
	}
	if gotTokens[0] == "" {
		t.Fatal("成功領單卻拿到空 token")
	}
	if !errors.Is(gotErrs[0], ErrJobNotFound) {
		t.Fatalf("落敗的領單回傳 %v，預期 ErrJobNotFound", gotErrs[0])
	}
}

// ⚠⚠ 同一個 job_id 過期後重跑會讓 agent 的 seq 從 1 開始，跟原執行的
// 事件發生 replay conflict。過期單只能由 reaper 收尾。
func TestExpiredJobCannotBeReclaimed(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	oldToken, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("第一次領單失敗：%v", err)
	}
	reclaimedAt := deployTestNow.Add(2 * time.Minute)
	if _, err := s.ClaimJob(jobID, "machine-a", reclaimedAt, time.Minute); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("租約過期後重新領單回傳 %v，預期 ErrJobNotFound", err)
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回過期工作單失敗：%v", err)
	}
	if job.State != deploy.Claimed || job.LeaseToken != oldToken {
		t.Fatalf("拒絕重新領單後改動了舊租約：%+v", job)
	}
}

// ⚠ 守住網路重試被當成錯誤，或同序號完全相同的事件長出第二列。
func TestAppendJobEventIdenticalReplayIsIdempotent(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("領取測試工作單失敗：%v", err)
	}
	firstAt := deployTestNow.Add(time.Second)
	if err := s.AppendJobEvent(jobID, "machine-a", token, 7, "第一次", `{"內容":"甲","step":1}`, firstAt, firstAt); err != nil {
		t.Fatalf("寫入第一次事件失敗：%v", err)
	}
	if err := s.AppendJobEvent(jobID, "machine-a", token, 7, "第一次", `{
		"step": 1,
		"內容": "甲"
	}`,
		firstAt, firstAt.Add(time.Second)); err != nil {
		t.Fatalf("重送同序號、同 canonical body 事件失敗：%v", err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM job_events WHERE job_id = ? AND seq = 7`, jobID); got != 1 {
		t.Fatalf("同序號重送後有 %d 列事件，預期 1 列", got)
	}
	var phase, payload, occurredAt, receivedAt string
	if err := s.DB().QueryRow(`
SELECT phase, payload, occurred_at, received_at FROM job_events WHERE job_id = ? AND seq = 7`, jobID).
		Scan(&phase, &payload, &occurredAt, &receivedAt); err != nil {
		t.Fatalf("讀取回放後事件失敗：%v", err)
	}
	if phase != "第一次" || payload != `{"內容":"甲","step":1}` || occurredAt != fmtTime(firstAt) ||
		receivedAt != fmtTime(firstAt) {
		t.Fatalf("重送改動了第一次事件：phase=%q payload=%q occurred_at=%q received_at=%q",
			phase, payload, occurredAt, receivedAt)
	}
}

// ⚠⚠ 同一個 (job_id, seq) 只能代表同一個 canonical event body。
// 安靜保留第一筆會讓 agent 以為不同的事件已被 Hub 接受。
func TestAppendJobEventReplayWithDifferentBodyConflicts(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("領取測試工作單失敗：%v", err)
	}
	occurredAt := deployTestNow.Add(time.Second)
	if err := s.AppendJobEvent(jobID, "machine-a", token, 7, "installing", `{"step":"download"}`,
		occurredAt, deployTestNow); err != nil {
		t.Fatalf("寫入第一次事件失敗：%v", err)
	}

	tests := []struct {
		name       string
		phase      string
		payload    string
		occurredAt time.Time
	}{
		{name: "phase", phase: "activating", payload: `{"step":"download"}`, occurredAt: occurredAt},
		{name: "payload", phase: "installing", payload: `{"step":"activate"}`, occurredAt: occurredAt},
		{name: "occurred_at", phase: "installing", payload: `{"step":"download"}`, occurredAt: occurredAt.Add(time.Second)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.AppendJobEvent(jobID, "machine-a", token, 7, tc.phase, tc.payload,
				tc.occurredAt, deployTestNow.Add(time.Second))
			if !errors.Is(err, ErrJobEventConflict) {
				t.Fatalf("同 seq 不同 body 回傳 %v，預期 ErrJobEventConflict", err)
			}
		})
	}

	var phase, payload, storedOccurredAt string
	if err := s.DB().QueryRow(`
SELECT phase, payload, occurred_at FROM job_events WHERE job_id = ? AND seq = 7`, jobID).
		Scan(&phase, &payload, &storedOccurredAt); err != nil {
		t.Fatalf("讀取衝突後事件失敗：%v", err)
	}
	if phase != "installing" || payload != `{"step":"download"}` || storedOccurredAt != fmtTime(occurredAt) {
		t.Fatalf("衝突回放改動了第一次事件：phase=%q payload=%q occurred_at=%q",
			phase, payload, storedOccurredAt)
	}
}

// ⚠ 守住用目前最大 seq 當門檻，誤丟掉較小但從未收到之補洞事件的錯。
func TestAppendJobEventAcceptsUnseenLowerSequence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("領取測試工作單失敗：%v", err)
	}
	for _, seq := range []int{5, 3} {
		if err := s.AppendJobEvent(jobID, "machine-a", token, seq, "執行", `{}`,
			deployTestNow, deployTestNow); err != nil {
			t.Fatalf("寫入 seq=%d 事件失敗：%v", seq, err)
		}
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM job_events WHERE job_id = ?`, jobID); got != 2 {
		t.Fatalf("先收 5 再補 3 後共有 %d 列，預期 2 列", got)
	}
}

// ⚠⚠ 守住沒有任何驗證證據，Hub 卻接受 agent 結論並把工作單設為 succeeded 的錯。
func TestMarkSucceededIfVerifiedRejectsJobWithoutVerification(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("沒有驗證證據時回傳 %v，預期 ErrNoVerification", err)
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State == deploy.Succeeded {
		t.Fatal("沒有驗證證據的工作單被設成 succeeded")
	}
}

func TestMarkSucceededIfVerifiedRejectsVerifierRoleWithoutExecutorEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []deploy.Event{deploy.Start, deploy.FinishWork} {
		if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, event, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,passed,verified_at,
  producer_kind,producer_id,evidence_role,authority,provenance_recorded,received_at)
 VALUES ('verifier-only',?,?,?,?,1,?,'controller_verifier','controller-a','verifier',
  'controller_read_only',1,?)`, jobID, "machine-a", "health", "GET /health",
		fmtTime(deployTestNow), fmtTime(deployTestNow)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("verifier-only evidence error=%v want ErrNoVerification", err)
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if job.State == deploy.Succeeded {
		t.Fatal("verifier-only evidence advanced job without executor evidence")
	}
}

func TestDarwinNodeRuntimeCannotSucceedFromSelfAttestedEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "darwin-node")
	spec := `{"kind":"node-runtime","version":"24.15.0","target_os":"darwin","target_arch":"arm64","bundle_layout":"node-runtime-bundle:v1","artifact":{"sha256":"` + strings.Repeat("a", 64) + `","size":1,"url":"/v1/artifacts/` + strings.Repeat("a", 64) + `"}}`
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := createDesiredStateTx(tx, "machine", "darwin-node", "node-runtime", "node-runtime",
		spec, "operator", deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	jobID, err := createManagedJobTx(tx, "darwin-node", desiredID, revision, NewJob{
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
	}, deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	token, err := s.ClaimJob(jobID, "darwin-node", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []deploy.Event{deploy.Start, deploy.FinishWork} {
		if _, err := s.AdvanceJobByAgent(jobID, "darwin-node", token, event, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordVerification(jobID, "darwin-node", token, "launchctl", "launchctl print gui/501/com.ai-intune.agent",
		0, "state = running", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("self-attested Darwin Node evidence error=%v want ErrNoVerification", err)
	}
	job, err := s.JobForMachine(jobID, "darwin-node")
	if err != nil {
		t.Fatal(err)
	}
	if job.State == deploy.Succeeded {
		t.Fatal("Darwin Node runtime succeeded without Node/npm runtime measurements")
	}
	nodePath := "/Users/operator/.local/share/clawctl/node-runtime/releases/24.15.0/bin/node"
	if err := s.RecordVerification(jobID, "darwin-node", token, "node-runtime-activate-node",
		nodePath+" --version", 0, "v24.15.0\n", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("missing npm measurement error=%v want ErrNoVerification", err)
	}
	if err := s.RecordVerification(jobID, "darwin-node", token, "node-runtime-activate-npm",
		nodePath+" /Users/operator/.local/share/clawctl/node-runtime/releases/24.15.0/lib/node_modules/npm/bin/npm-cli.js --version",
		0, "11.7.0\n", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("missing exact artifact measurement error=%v want ErrNoVerification", err)
	}
	if err := s.RecordVerification(jobID, "darwin-node", token, "node-runtime-activate-artifact",
		"cat /Users/operator/.local/share/clawctl/node-runtime/releases/24.15.0/.clawctl-artifact-sha256",
		0, "sha256:"+strings.Repeat("b", 64)+"\n", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("wrong artifact measurement error=%v want ErrNoVerification", err)
	}
	for _, evidence := range []struct {
		rule, command, stdout string
	}{
		{"node-runtime-current-artifact", "cat /Users/operator/.local/share/clawctl/node-runtime/releases/24.15.0/.clawctl-artifact-sha256", "sha256:" + strings.Repeat("a", 64) + "\n"},
		{"node-runtime-current-node", nodePath + " --version", "v24.15.0\n"},
		{"node-runtime-current-npm", nodePath + " /Users/operator/.local/share/clawctl/node-runtime/releases/24.15.0/lib/node_modules/npm/bin/npm-cli.js --version", "11.7.0\n"},
	} {
		if err := s.RecordVerification(jobID, "darwin-node", token, evidence.rule,
			evidence.command, 0, evidence.stdout, "", true, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
		t.Fatalf("measured Darwin Node runtime state=%q error=%v", state, err)
	}
}

func TestWindowsNodeRuntimeCannotSucceedFromSelfAttestedEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "windows-node")
	spec := `{"kind":"node-runtime","version":"24.15.0","target_os":"windows","target_arch":"amd64","bundle_layout":"node-runtime-bundle:v1","artifact":{"sha256":"` + strings.Repeat("a", 64) + `","size":1,"url":"/v1/artifacts/` + strings.Repeat("a", 64) + `"}}`
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := createDesiredStateTx(tx, "machine", "windows-node", "node-runtime", "node-runtime",
		spec, "operator", deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	jobID, err := createManagedJobTx(tx, "windows-node", desiredID, revision, NewJob{
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
	}, deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	token, err := s.ClaimJob(jobID, "windows-node", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []deploy.Event{deploy.Start, deploy.FinishWork} {
		if _, err := s.AdvanceJobByAgent(jobID, "windows-node", token, event, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordVerification(jobID, "windows-node", token, "scheduled-task",
		`Get-ScheduledTask -TaskName clawctl-agent`,
		0, "State = Ready", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("self-attested Windows Node evidence error=%v want ErrNoVerification", err)
	}
	job, err := s.JobForMachine(jobID, "windows-node")
	if err != nil {
		t.Fatal(err)
	}
	if job.State == deploy.Succeeded {
		t.Fatal("Windows Node runtime succeeded without Node/npm runtime measurements")
	}
	nodePath := `C:\Users\operator\.local\share\clawctl\node-runtime\releases\24.15.0\bin\node.exe`
	if err := s.RecordVerification(jobID, "windows-node", token, "node-runtime-activate-node",
		nodePath+" --version", 0, "v24.15.0\n", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("missing npm measurement error=%v want ErrNoVerification", err)
	}
	if err := s.RecordVerification(jobID, "windows-node", token, "node-runtime-activate-npm",
		nodePath+` C:\Users\operator\.local\share\clawctl\node-runtime\releases\24.15.0\lib\node_modules\npm\bin\npm-cli.js --version`,
		0, "11.7.0\n", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("missing exact artifact measurement error=%v want ErrNoVerification", err)
	}
	if err := s.RecordVerification(jobID, "windows-node", token, "node-runtime-activate-artifact",
		`cat C:\Users\operator\.local\share\clawctl\node-runtime\releases\24.15.0\.clawctl-artifact-sha256`,
		0, "sha256:"+strings.Repeat("b", 64)+"\n", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("wrong artifact measurement error=%v want ErrNoVerification", err)
	}
	for _, evidence := range []struct {
		rule, command, stdout string
	}{
		{"node-runtime-current-artifact", `cat C:\Users\operator\.local\share\clawctl\node-runtime\releases\24.15.0\.clawctl-artifact-sha256`, "sha256:" + strings.Repeat("a", 64) + "\n"},
		{"node-runtime-current-node", nodePath + " --version", "v24.15.0\n"},
		{"node-runtime-current-npm", nodePath + ` C:\Users\operator\.local\share\clawctl\node-runtime\releases\24.15.0\lib\node_modules\npm\bin\npm-cli.js --version`, "11.7.0\n"},
	} {
		if err := s.RecordVerification(jobID, "windows-node", token, evidence.rule,
			evidence.command, 0, evidence.stdout, "", true, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
		t.Fatalf("measured Windows Node runtime state=%q error=%v", state, err)
	}
}

// ⚠⚠ 守住只看見一筆通過就忽略同張單的失敗證據，錯誤宣告 succeeded 的錯。
func TestClaudeCodeCannotSucceedFromSelfAttestedEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "claude-linux")
	spec := `{"kind":"claude-code","version":"2.1.278","target_os":"linux","target_arch":"amd64","bundle_layout":"claude-code-bundle:v1","artifact":{"sha256":"` + strings.Repeat("c", 64) + `","size":1,"url":"/v1/artifacts/` + strings.Repeat("c", 64) + `"}}`
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := createDesiredStateTx(tx, "machine", "claude-linux", "claude-code", "claude-code",
		spec, "operator", deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	jobID, err := createManagedJobTx(tx, "claude-linux", desiredID, revision, NewJob{
		ArtifactDigest: "sha256:" + strings.Repeat("c", 64),
	}, deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	token, err := s.ClaimJob(jobID, "claude-linux", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []deploy.Event{deploy.Start, deploy.FinishWork} {
		if _, err := s.AdvanceJobByAgent(jobID, "claude-linux", token, event, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordVerification(jobID, "claude-linux", token, "self-report",
		"echo installed", 0, "ok", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("self-attested Claude Code error=%v", err)
	}
	binary := "/home/operator/.local/share/clawctl/claude-code/releases/2.1.278/bin/claude"
	for _, evidence := range []struct {
		rule, command, stdout string
	}{
		{"claude-code-current-artifact", "cat /home/operator/.local/share/clawctl/claude-code/releases/2.1.278/.clawctl-artifact-sha256", "sha256:" + strings.Repeat("c", 64) + "\n"},
		{"claude-code-current-version", binary + " --version", "2.1.278 (Claude Code)\n"},
	} {
		if err := s.RecordVerification(jobID, "claude-linux", token, evidence.rule,
			evidence.command, 0, evidence.stdout, "", true, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
		t.Fatalf("measured Claude Code state=%q error=%v", state, err)
	}
}

func TestCodexCannotSucceedFromSelfAttestedEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "codex-linux")
	spec := `{"kind":"codex","version":"0.155.1","target_os":"linux","target_arch":"amd64","bundle_layout":"codex-bundle:v1","artifact":{"sha256":"` + strings.Repeat("d", 64) + `","size":1,"url":"/v1/artifacts/` + strings.Repeat("d", 64) + `"}}`
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := createDesiredStateTx(tx, "machine", "codex-linux", "codex", "codex",
		spec, "operator", deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	jobID, err := createManagedJobTx(tx, "codex-linux", desiredID, revision, NewJob{
		ArtifactDigest: "sha256:" + strings.Repeat("d", 64),
	}, deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	token, err := s.ClaimJob(jobID, "codex-linux", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []deploy.Event{deploy.Start, deploy.FinishWork} {
		if _, err := s.AdvanceJobByAgent(jobID, "codex-linux", token, event, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordVerification(jobID, "codex-linux", token, "self-report",
		"echo installed", 0, "ok", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("self-attested Codex error=%v", err)
	}
	binary := "/home/operator/.local/share/clawctl/codex/releases/0.155.1/bin/codex"
	for _, evidence := range []struct {
		rule, command, stdout string
	}{
		{"codex-current-artifact", "cat /home/operator/.local/share/clawctl/codex/releases/0.155.1/.clawctl-artifact-sha256", "sha256:" + strings.Repeat("d", 64) + "\n"},
		{"codex-current-version", binary + " --version", "codex-cli 0.155.1\n"},
	} {
		if err := s.RecordVerification(jobID, "codex-linux", token, evidence.rule,
			evidence.command, 0, evidence.stdout, "", true, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
		t.Fatalf("measured Codex state=%q error=%v", state, err)
	}
}

func TestGrokCannotSucceedFromSelfAttestedEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "grok-linux")
	spec := `{"kind":"grok","version":"1.0.40","target_os":"linux","target_arch":"amd64","bundle_layout":"grok-bundle:v1","artifact":{"sha256":"` + strings.Repeat("e", 64) + `","size":1,"url":"/v1/artifacts/` + strings.Repeat("e", 64) + `"}}`
	jobID := insertMeasuredEvidenceJob(t, s, "grok-linux", "grok", spec)
	token := advanceMeasuredEvidenceJob(t, s, jobID, "grok-linux")
	if err := s.RecordVerification(jobID, "grok-linux", token, "self-report",
		"echo installed", 0, "ok", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("self-attested Grok error=%v", err)
	}
	binary := "/home/operator/.local/share/clawctl/grok/releases/1.0.40/bin/grok"
	for _, evidence := range []struct {
		rule, command, stdout string
	}{
		{"grok-current-artifact", "cat /home/operator/.local/share/clawctl/grok/releases/1.0.40/.clawctl-artifact-sha256", "sha256:" + strings.Repeat("e", 64) + "\n"},
		{"grok-current-version", binary + " --version", "grok 1.0.40 (eb1a2256660d) [stable]\n"},
	} {
		if err := s.RecordVerification(jobID, "grok-linux", token, evidence.rule,
			evidence.command, 0, evidence.stdout, "", true, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
		t.Fatalf("measured Grok state=%q error=%v", state, err)
	}
}

func TestBATServerCannotSucceedFromSelfAttestedEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "bat-linux")
	artifactHash := strings.Repeat("e", 64)
	binaryHash := strings.Repeat("c", 64)
	spec := batServerEvidenceSpec("amd64", artifactHash, binaryHash)
	jobID := insertMeasuredEvidenceJob(t, s, "bat-linux", "bat-server", spec)
	token := advanceMeasuredEvidenceJob(t, s, jobID, "bat-linux")
	if err := s.RecordVerification(jobID, "bat-linux", token, "self-report",
		"echo installed", 0, "ok", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("self-attested BAT Server error=%v", err)
	}
	recordBATServerEvidence(t, s, jobID, "bat-linux", token, "3.2.10", artifactHash, binaryHash,
		"bat-server-linux-x86_64/bat-server", "active\n")
	if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
		t.Fatalf("measured BAT Server state=%q error=%v", state, err)
	}
}

func TestBATServerMeasuredEvidenceRequiresInstalledBinaryHash(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "bat-hash")
	artifactHash := strings.Repeat("e", 64)
	binaryHash := strings.Repeat("c", 64)
	spec := batServerEvidenceSpec("amd64", artifactHash, binaryHash)
	jobID := insertMeasuredEvidenceJob(t, s, "bat-hash", "bat-server", spec)
	token := advanceMeasuredEvidenceJob(t, s, jobID, "bat-hash")
	recordBATServerEvidence(t, s, jobID, "bat-hash", token, "3.2.10", artifactHash, strings.Repeat("d", 64),
		"bat-server-linux-x86_64/bat-server", "active\n")
	_, ready, err := s.measuredEvidenceStatus(jobID)
	if err != nil || ready {
		t.Fatalf("ready=%t err=%v", ready, err)
	}
}

func TestBATServerMeasuredEvidenceRequiresActiveUnit(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "bat-unit")
	artifactHash := strings.Repeat("e", 64)
	binaryHash := strings.Repeat("c", 64)
	spec := batServerEvidenceSpec("amd64", artifactHash, binaryHash)
	jobID := insertMeasuredEvidenceJob(t, s, "bat-unit", "bat-server", spec)
	token := advanceMeasuredEvidenceJob(t, s, jobID, "bat-unit")
	recordBATServerEvidence(t, s, jobID, "bat-unit", token, "3.2.10", artifactHash, binaryHash,
		"bat-server-linux-x86_64/bat-server", "inactive\n")
	_, ready, err := s.measuredEvidenceStatus(jobID)
	if err != nil || ready {
		t.Fatalf("ready=%t err=%v", ready, err)
	}
}

func TestBATServerMeasuredEvidenceAcceptsARM64BinaryPath(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "bat-arm")
	artifactHash := strings.Repeat("a", 64)
	binaryHash := strings.Repeat("b", 64)
	spec := batServerEvidenceSpec("arm64", artifactHash, binaryHash)
	jobID := insertMeasuredEvidenceJob(t, s, "bat-arm", "bat-server", spec)
	token := advanceMeasuredEvidenceJob(t, s, jobID, "bat-arm")
	recordBATServerEvidence(t, s, jobID, "bat-arm", token, "3.2.10", artifactHash, binaryHash,
		"bat-server-linux-x86_64/bat-server", "active\n")
	if _, ready, err := s.measuredEvidenceStatus(jobID); err != nil || ready {
		t.Fatalf("amd64 path satisfied arm64 ready=%t err=%v", ready, err)
	}
	jobID = insertMeasuredEvidenceJob(t, s, "bat-arm", "bat-server", spec)
	token = advanceMeasuredEvidenceJob(t, s, jobID, "bat-arm")
	recordBATServerEvidence(t, s, jobID, "bat-arm", token, "3.2.10", artifactHash, binaryHash,
		"bat-server-linux-aarch64/bat-server", "active\n")
	if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
		t.Fatalf("arm64 state=%q err=%v", state, err)
	}
}

func batServerEvidenceSpec(arch, artifactHash, binaryHash string) string {
	return fmt.Sprintf(`{"kind":"bat-server","version":"3.2.10","target_os":"linux","target_arch":%q,"bundle_layout":"bat-server-bundle:v1","binary_sha256":%q,"artifact":{"sha256":%q,"size":1,"url":"/v1/artifacts/%s"}}`,
		arch, binaryHash, artifactHash, artifactHash)
}

func recordBATServerEvidence(t *testing.T, s *Store, jobID, machine, token, version, artifactHash, binaryHash, binaryRel, unitStdout string) {
	t.Helper()
	release := "/home/operator/.local/share/clawctl/bat-server/releases/" + version
	for _, evidence := range []struct {
		rule, command, stdout string
	}{
		{"bat-server-release", "test -d " + release, release + "\n"},
		{"bat-server-artifact", "cat " + release + "/.clawctl-artifact-sha256", "sha256:" + artifactHash + "\n"},
		{"bat-server-binary", "sha256sum " + release + "/" + binaryRel, binaryHash + "\n"},
		{"bat-server-unit", "systemctl --user is-active " + model.BATServerUnit, unitStdout},
		{"bat-server-endpoint", model.BATServerEndpointCommand, "authenticated\n"},
	} {
		if err := s.RecordVerification(jobID, machine, token, evidence.rule,
			evidence.command, 0, evidence.stdout, "", true, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGrokMeasuredVersionUsesCommandThenExactVersion(t *testing.T) {
	const version = "1.0.40"
	if !grokMeasuredVersionMatches("\n\ngrok 1.0.40 (eb1a2256660d) [stable]\n", version) {
		t.Fatal("official version line was not accepted")
	}
	if grokMeasuredVersionMatches("1.0.40 grok\n", version) {
		t.Fatal("accepted a line that does not start with grok")
	}
	if grokMeasuredVersionMatches("grokx 1.0.40\n", version) {
		t.Fatal("accepted a command that is not grok")
	}
	if grokMeasuredVersionMatches("grok 1.0.41 (eb1a2256660d) [stable]\n", version) {
		t.Fatal("accepted a different version")
	}
}

func TestMeasuredEvidenceCheckersClaimEveryDeclaredKind(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "measured-registry")
	declared := 0
	for _, contract := range agentadapter.Contracts() {
		if !contract.RequiresMeasuredEvidence {
			continue
		}
		declared++
		claimed := false
		var statusErr error
		for _, platform := range contract.Platforms {
			spec := fmt.Sprintf(`{"kind":%q,"version":"1.2.3","target_os":%q,"target_arch":%q,"bundle_layout":"bundle","artifact":{"sha256":"%s","size":1,"url":"/v1/artifacts/%s"}}`,
				contract.ExecutorKind, platform.OS, platform.Arch, strings.Repeat("a", 64), strings.Repeat("a", 64))
			jobID := insertMeasuredEvidenceJob(t, s, "measured-registry", contract.ExecutorKind, spec)
			required, _, err := s.measuredEvidenceStatus(jobID)
			if err != nil {
				statusErr = err
				break
			}
			if required {
				claimed = true
				break
			}
		}
		if statusErr != nil || !claimed {
			t.Fatalf("kind %s claimed=%t err=%v", contract.ExecutorKind, claimed, statusErr)
		}
	}
	if declared == 0 {
		t.Fatal("no adapter declares measured evidence")
	}
}

func TestDeclaredMeasuredEvidenceKindsHaveAChecker(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "measured-unwired")
	declared := 0
	for _, contract := range agentadapter.Contracts() {
		if !contract.RequiresMeasuredEvidence {
			continue
		}
		declared++
		spec := fmt.Sprintf(`{"kind":%q,"version":"1.2.3","target_os":"linux","target_arch":"amd64"}`, contract.ExecutorKind)
		jobID := insertMeasuredEvidenceJob(t, s, "measured-unwired", contract.ExecutorKind, spec)
		wired, _, _, err := s.measuredEvidenceDecision(jobID)
		if err != nil || !wired {
			t.Fatalf("kind %s wired=%t err=%v", contract.ExecutorKind, wired, err)
		}
	}
	if declared == 0 {
		t.Fatal("no adapter declares measured evidence")
	}
	jobID := insertMeasuredEvidenceJob(t, s, "measured-unwired", "openclaw", `{"kind":"openclaw","version":"1.2.3"}`)
	wired, required, _, err := s.measuredEvidenceDecision(jobID)
	if err != nil || wired || required {
		t.Fatalf("openclaw wired=%t required=%t err=%v", wired, required, err)
	}
}

func TestUndeclaredMeasuredEvidenceKindsStillSucceedFromExecutorEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "measured-undeclared")
	undeclared := 0
	for _, contract := range agentadapter.Contracts() {
		if contract.RequiresMeasuredEvidence {
			continue
		}
		undeclared++
		spec := fmt.Sprintf(`{"kind":%q,"version":"1.2.3"}`, contract.ExecutorKind)
		jobID := insertMeasuredEvidenceJob(t, s, "measured-undeclared", contract.ExecutorKind, spec)
		token := advanceMeasuredEvidenceJob(t, s, jobID, "measured-undeclared")
		if err := s.RecordVerification(jobID, "measured-undeclared", token, "self-report",
			"echo installed", 0, "ok", "", true, deployTestNow); err != nil {
			t.Fatal(err)
		}
		if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
			t.Fatalf("kind %s state=%q err=%v", contract.ExecutorKind, state, err)
		}
	}
	if undeclared == 0 {
		t.Fatal("no adapter keeps executor evidence")
	}
}

func insertMeasuredEvidenceJob(t *testing.T, s *Store, machineID, kind, spec string) string {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := createDesiredStateTx(tx, "machine", machineID, kind, kind, spec, "operator", deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	jobID, err := createManagedJobTx(tx, machineID, desiredID, revision, NewJob{
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
	}, deployTestNow)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func advanceMeasuredEvidenceJob(t *testing.T, s *Store, jobID, machineID string) string {
	t.Helper()
	token, err := s.ClaimJob(jobID, machineID, deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []deploy.Event{deploy.Start, deploy.FinishWork} {
		if _, err := s.AdvanceJobByAgent(jobID, machineID, token, event, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
	return token
}

func TestMarkSucceededIfVerifiedRejectsAnyFailedResult(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("領取測試工作單失敗：%v", err)
	}
	for _, result := range []struct {
		ruleID string
		passed bool
	}{
		{ruleID: "版本", passed: true},
		{ruleID: "服務", passed: false},
	} {
		if err := s.RecordVerification(jobID, "machine-a", token, result.ruleID, "實際驗證命令",
			1, "輸出", "錯誤輸出", result.passed, deployTestNow); err != nil {
			t.Fatalf("寫入 %s 驗證失敗：%v", result.ruleID, err)
		}
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow); !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("同時有通過與失敗證據時回傳 %v，預期 ErrVerificationFailed", err)
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State == deploy.Succeeded {
		t.Fatal("存在失敗證據的工作單被設成 succeeded")
	}
}

// ⚠ 守住通過證據齊備後沒有由 Hub 寫入 succeeded，或漏記終態時間的錯。
func TestMarkSucceededIfVerifiedWithOnlyPassedResults(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("領取測試工作單失敗：%v", err)
	}
	// ⚠ 走狀態機到 verifying。succeeded 只有這一個入口 ——
	// 直接從 claimed 標成功等於宣稱一台還沒開始裝的機器裝好了。
	for _, ev := range []deploy.Event{deploy.Start, deploy.FinishWork} {
		if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, ev, deployTestNow); err != nil {
			t.Fatalf("推進 %s 失敗：%v", ev, err)
		}
	}
	command := "openclaw verify --rule version --format exact"
	if err := s.RecordVerification(jobID, "machine-a", token, "版本", command,
		0, "通過輸出", "", true, deployTestNow); err != nil {
		t.Fatalf("寫入通過證據失敗：%v", err)
	}
	var storedCommand, producerKind, producerID, evidenceRole, authority, receivedAt, observedDigest, observedVersion, verifierID string
	var provenanceRecorded bool
	if err := s.DB().QueryRow(`SELECT command,producer_kind,producer_id,evidence_role,authority,
	 provenance_recorded,received_at,observed_digest,observed_version,verifier_id
	 FROM verification_results WHERE job_id = ?`, jobID).
		Scan(&storedCommand, &producerKind, &producerID, &evidenceRole, &authority,
			&provenanceRecorded, &receivedAt, &observedDigest, &observedVersion, &verifierID); err != nil {
		t.Fatalf("讀回驗證命令失敗：%v", err)
	}
	if storedCommand != command {
		t.Fatalf("驗證命令沒有原樣保存：拿到 %q，預期 %q", storedCommand, command)
	}
	if producerKind != JobVerificationProducerExecutorAgent || producerID != "machine-a" ||
		evidenceRole != JobVerificationRoleExecutor || authority != JobVerificationAuthorityMachineLease ||
		!provenanceRecorded || receivedAt != fmtTime(deployTestNow) || observedDigest != "" ||
		observedVersion != "" || verifierID != "" {
		t.Fatalf("驗證 provenance 不符：kind=%q id=%q role=%q authority=%q recorded=%t received=%q observed=%q verifier=%q",
			producerKind, producerID, evidenceRole, authority, provenanceRecorded, receivedAt, observedDigest, verifierID)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil {
		t.Fatalf("只有通過證據時標記成功失敗：%v", err)
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.Succeeded || job.TerminalAt == nil {
		t.Fatalf("通過驗證後工作單終態不符：%+v", job)
	}
	var leaseTokenNull, leaseExpiresNull bool
	if err := s.DB().QueryRow(`
SELECT lease_token IS NULL, lease_expires_at IS NULL FROM jobs WHERE job_id = ?`, jobID).
		Scan(&leaseTokenNull, &leaseExpiresNull); err != nil {
		t.Fatalf("查終態租約欄位失敗：%v", err)
	}
	if !leaseTokenNull || !leaseExpiresNull {
		t.Fatalf("succeeded 終態仍留著租約：lease_token_null=%v lease_expires_at_null=%v",
			leaseTokenNull, leaseExpiresNull)
	}
}

func TestRecordVerificationExactRetryDoesNotDuplicateEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, deploy.Start, deployTestNow); err != nil {
		t.Fatal(err)
	}
	write := func() error {
		return s.RecordVerification(jobID, "machine-a", token, "health", "true", 0,
			"ok", "", true, deployTestNow)
	}
	if err := write(); err != nil {
		t.Fatal(err)
	}
	var firstID, firstReceivedAt string
	if err := s.DB().QueryRow(`SELECT verification_id,received_at FROM verification_results WHERE job_id=?`, jobID).
		Scan(&firstID, &firstReceivedAt); err != nil {
		t.Fatal(err)
	}
	s.nowFn = func() time.Time { return deployTestNow.Add(time.Minute) }
	if err := write(); err != nil {
		t.Fatalf("完全相同的驗證重送失敗：%v", err)
	}
	var replayID, replayReceivedAt string
	if err := s.DB().QueryRow(`SELECT verification_id,received_at FROM verification_results WHERE job_id=?`, jobID).
		Scan(&replayID, &replayReceivedAt); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID); got != 1 ||
		replayID != firstID || replayReceivedAt != firstReceivedAt {
		t.Fatalf("回放改寫驗證帳本：count=%d id=%q/%q received_at=%q/%q",
			got, firstID, replayID, firstReceivedAt, replayReceivedAt)
	}
}

func TestRecordVerificationRejectsSameRuleWithDifferentEvidence(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, deploy.Start, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordVerification(jobID, "machine-a", token, "health", "true", 0,
		"ok", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordVerification(jobID, "machine-a", token, "health", "true", 1,
		"failed", "", false, deployTestNow); !errors.Is(err, ErrJobVerificationConflict) {
		t.Fatalf("不同驗證證據回傳 %v，預期 ErrJobVerificationConflict", err)
	}
	var exitCode int
	var stdout string
	var passed bool
	if err := s.DB().QueryRow(`SELECT exit_code,stdout_excerpt,passed FROM verification_results WHERE job_id=?`, jobID).
		Scan(&exitCode, &stdout, &passed); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID); got != 1 ||
		exitCode != 0 || stdout != "ok" || !passed {
		t.Fatalf("衝突重送改寫既有列：count=%d exit=%d stdout=%q passed=%t", got, exitCode, stdout, passed)
	}
}

func TestRecordVerificationKeepsIndependentEvidenceSeparate(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, deploy.Start, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordVerification(jobID, "machine-a", token, "health", "true", 0,
		"ok", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	verifier, _, err := s.RegisterVerifier(VerifierKindExternalJobRunner, "awx-verifier", "awx", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIndependentVerification(independentRequest(verifier.VerifierID, jobID)); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID); got != 2 {
		t.Fatalf("同規則的 executor 與 independent 證據共有 %d 列，預期 2", got)
	}
	if got := countRows(t, s, `SELECT COUNT(DISTINCT evidence_role) FROM verification_results WHERE job_id=?`, jobID); got != 2 {
		t.Fatalf("驗證證據角色共有 %d 種，預期 executor 與 independent", got)
	}
}

// ⚠⚠ complete 先讀到 verifying 後，reaper 或 reject handler 可能先把同一列收成終態。
// 隨後到達的成功標記不得把已落地的終態與終態時間覆寫掉。
func TestMarkSucceededIfVerifiedPreservesConcurrentTerminalState(t *testing.T) {
	tests := []struct {
		name      string
		event     deploy.Event
		byHub     bool
		wantState deploy.JobState
	}{
		{name: "timeout", event: deploy.Timeout, byHub: true, wantState: deploy.Failed},
		{name: "reject", event: deploy.Reject, wantState: deploy.Rejected},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newDeployTestStore(t)
			registerDeployMachine(t, s, "machine-a")
			jobID := newJobForDeployTest(t, s, "machine-a")
			token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
			if err != nil {
				t.Fatalf("領取測試工作單失敗：%v", err)
			}
			for _, ev := range []deploy.Event{deploy.Start, deploy.FinishWork} {
				if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, ev, deployTestNow); err != nil {
					t.Fatalf("推進 %s 失敗：%v", ev, err)
				}
			}
			if err := s.RecordVerification(jobID, "machine-a", token, "版本", "openclaw verify --rule version",
				0, "通過輸出", "", true, deployTestNow); err != nil {
				t.Fatalf("寫入通過證據失敗：%v", err)
			}

			terminalAt := deployTestNow.Add(10 * time.Second)
			var gotState deploy.JobState
			if test.byHub {
				gotState, err = s.AdvanceJobByHub(jobID, test.event, terminalAt)
			} else {
				gotState, err = s.AdvanceJobByAgent(jobID, "machine-a", token, test.event, terminalAt)
			}
			if err != nil {
				t.Fatalf("收成終態失敗：%v", err)
			}
			if gotState != test.wantState {
				t.Fatalf("收成終態得到 %s，預期 %s", gotState, test.wantState)
			}

			markedState, err := s.MarkSucceededIfVerified(jobID, terminalAt.Add(10*time.Second))
			if err != nil {
				t.Fatalf("對已收成終態的工作單標記成功回傳錯誤：%v", err)
			}
			if markedState != test.wantState {
				t.Fatalf("對已收成終態的工作單標記成功回傳 %s，預期 %s", markedState, test.wantState)
			}

			var state deploy.JobState
			var storedTerminalAt string
			var leaseTokenNull, leaseExpiresNull bool
			if err := s.DB().QueryRow(`
SELECT state, terminal_at, lease_token IS NULL, lease_expires_at IS NULL
  FROM jobs WHERE job_id = ?`, jobID).
				Scan(&state, &storedTerminalAt, &leaseTokenNull, &leaseExpiresNull); err != nil {
				t.Fatalf("讀回帳本列失敗：%v", err)
			}
			if state != test.wantState {
				t.Errorf("已收成的終態被改成 %s，預期仍為 %s", state, test.wantState)
			}
			if storedTerminalAt != fmtTime(terminalAt) {
				t.Errorf("terminal_at=%q，預期保留 %q", storedTerminalAt, fmtTime(terminalAt))
			}
			if !leaseTokenNull || !leaseExpiresNull {
				t.Errorf("終態列仍留著租約：lease_token_null=%v lease_expires_at_null=%v",
					leaseTokenNull, leaseExpiresNull)
			}
		})
	}
}

func TestReapableJobsUsesStartReceivedAtAndExcludesTerminalJobs(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Hour)
	if err != nil {
		t.Fatalf("領取工作單失敗：%v", err)
	}
	occurredAt := deployTestNow.Add(80 * time.Second)
	receivedAt := deployTestNow.Add(2 * time.Second)
	if err := s.AppendJobEvent(jobID, "machine-a", token, 1, "start", `{}`, occurredAt, receivedAt); err != nil {
		t.Fatalf("寫 start 事件失敗：%v", err)
	}
	if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, deploy.Start, receivedAt); err != nil {
		t.Fatalf("推進 running 失敗：%v", err)
	}
	jobs, err := s.ReapableJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ReapableJobs = %+v err=%v", jobs, err)
	}
	if jobs[0].StartedAt == nil || !jobs[0].StartedAt.Equal(receivedAt) || jobs[0].StartedAt.Equal(occurredAt) {
		t.Fatalf("started_at 沒用 Hub 的 received_at：%+v", jobs[0])
	}
	if _, err := s.AdvanceJobByHub(jobID, deploy.Timeout, receivedAt.Add(time.Hour)); err != nil {
		t.Fatalf("判工作單逾時失敗：%v", err)
	}
	jobs, err = s.ReapableJobs()
	if err != nil || len(jobs) != 0 {
		t.Fatalf("終態仍出現在 ReapableJobs：%+v err=%v", jobs, err)
	}
}

// ⚠⚠ 守住繞過 deploy.OnFailure，讓可逆與不可逆失敗的兩個終態互換的錯。
func TestFailJobUsesOnFailureForTerminalState(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, rev := desiredForDeployTest(t, s)

	tests := []struct {
		name         string
		irreversible bool
		want         deploy.JobState
	}{
		{name: "可逆", irreversible: false, want: deploy.Failed},
		{name: "不可逆", irreversible: true, want: deploy.ManualIntervention},
	}
	for i, test := range tests {
		jobID, err := s.CreateJob("machine-a", desiredID, rev+deploy.Revision(i), NewJob{
			Irreversible: test.irreversible,
		})
		if err != nil {
			t.Fatalf("建立%s工作單失敗：%v", test.name, err)
		}
		if err := s.FailJob(jobID, test.irreversible, deployTestNow); err != nil {
			t.Fatalf("標記%s工作單失敗：%v", test.name, err)
		}
		job, err := s.JobForMachine(jobID, "machine-a")
		if err != nil {
			t.Fatalf("讀回%s工作單失敗：%v", test.name, err)
		}
		if job.State != test.want || job.TerminalAt == nil {
			t.Errorf("%s工作單得到 state=%q terminal_at=%v，預期 state=%q 且有終態時間",
				test.name, job.State, job.TerminalAt, test.want)
		}
	}
}

func TestFailJobClearsLeaseOnClaimedJob(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, rev := desiredForDeployTest(t, s)

	tests := []struct {
		name               string
		irreversible       bool
		wantState          deploy.JobState
		stateConsequence   string
		tokenConsequence   string
		expiresConsequence string
	}{
		{
			name:               "可逆",
			irreversible:       false,
			wantState:          deploy.Failed,
			stateConsequence:   "已回退的失敗被寫成別的終態，操作員會以為這台機器還需要人工介入",
			tokenConsequence:   "單已經是 failed，帳本卻仍顯示有持有者，操作員會空等 agent 交還才肯重派，而不是立刻重試",
			expiresConsequence: "已關閉的單上還留著一個未來的租約到期時間，操作員會等那個時間過了才重派",
		},
		{
			name:               "不可逆",
			irreversible:       true,
			wantState:          deploy.ManualIntervention,
			stateConsequence:   "不可逆的失敗被寫成已回退，操作員會以為機器已經回到舊版而不去現場處理",
			tokenConsequence:   "單已經是 manual_intervention，帳本卻仍顯示有持有者，操作員會繼續等 agent 結束，而不是立刻上手干預",
			expiresConsequence: "已關閉的單上還留著一個未來的租約到期時間，操作員會等那個時間過了才上手處理",
		},
	}
	for i, test := range tests {
		jobID, err := s.CreateJob("machine-a", desiredID, rev+deploy.Revision(i), NewJob{
			Irreversible: test.irreversible,
		})
		if err != nil {
			t.Fatalf("建立%s工作單失敗：%v", test.name, err)
		}
		token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
		if err != nil {
			t.Fatalf("領取%s工作單失敗：%v", test.name, err)
		}
		if token == "" {
			t.Fatalf("領取%s工作單得到空 token", test.name)
		}
		if err := s.FailJob(jobID, test.irreversible, deployTestNow); err != nil {
			t.Fatalf("標記%s工作單失敗：%v", test.name, err)
		}
		var gotState deploy.JobState
		var leaseToken, leaseExpires sql.NullString
		if err := s.DB().QueryRow(`
SELECT state, lease_token, lease_expires_at FROM jobs WHERE job_id = ?`, jobID).
			Scan(&gotState, &leaseToken, &leaseExpires); err != nil {
			t.Fatal(err)
		}
		if gotState != test.wantState {
			t.Errorf("%s 的終態是 %q，預期 %q —— %s", test.name, gotState, test.wantState, test.stateConsequence)
		}
		if leaseToken.Valid {
			t.Errorf("%s 的 lease_token 仍然是 %q，預期已清成 NULL —— %s", test.name, leaseToken.String, test.tokenConsequence)
		}
		if leaseExpires.Valid {
			t.Errorf("%s 的 lease_expires_at 仍然是 %q，預期已清成 NULL —— %s", test.name, leaseExpires.String, test.expiresConsequence)
		}
	}
}

func TestFailJobRejectsIrreversibilityMismatch(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	if err := s.FailJob(jobID, true, deployTestNow); !errors.Is(err, ErrJobIrreversibility) {
		t.Fatalf("mismatched failure verdict error=%v want ErrJobIrreversibility", err)
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != deploy.NotStarted || job.TerminalAt != nil {
		t.Fatalf("mismatched failure verdict mutated job: %+v", job)
	}
}

func TestJobEvidenceRejectsInvalidSequenceAndTimesBeforeWrite(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		seq                    int
		phase, payload         string
		occurredAt, receivedAt time.Time
	}{
		"negative sequence": {-1, "start", `{}`, deployTestNow, deployTestNow},
		"empty phase":       {1, "", `{}`, deployTestNow, deployTestNow},
		"oversized payload": {1, "start", strings.Repeat("x", maxJobEventPayloadBytes+1), deployTestNow, deployTestNow},
		"zero agent time":   {1, "start", `{}`, time.Time{}, deployTestNow},
		"zero hub time":     {1, "start", `{}`, deployTestNow, time.Time{}},
		"unencodable year":  {1, "start", `{}`, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), deployTestNow},
	} {
		t.Run(name, func(t *testing.T) {
			if err := s.AppendJobEvent(jobID, "machine-a", token, test.seq, test.phase, test.payload,
				test.occurredAt, test.receivedAt); !errors.Is(err, ErrInvalidJobEvidence) {
				t.Fatalf("error=%v want ErrInvalidJobEvidence", err)
			}
		})
	}
	if err := s.RecordVerification(jobID, "machine-a", token, "health", "true", 0,
		"", "", true, time.Time{}); !errors.Is(err, ErrInvalidJobEvidence) {
		t.Fatalf("zero verification time error=%v want ErrInvalidJobEvidence", err)
	}
	if err := s.RecordVerification(jobID, "machine-a", token, "health",
		strings.Repeat("x", maxJobVerificationCommandBytes+1), 0, "", "", true,
		deployTestNow); !errors.Is(err, ErrInvalidJobEvidence) {
		t.Fatalf("oversized verification error=%v want ErrInvalidJobEvidence", err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM job_events WHERE job_id=?`, jobID); got != 0 {
		t.Fatalf("invalid events wrote %d rows", got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID); got != 0 {
		t.Fatalf("invalid verification wrote %d rows", got)
	}
}

func TestAppendJobEventWithReplayRejectsAnotherJobsLeaseTokenOnSameMachine(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-1")
	jobA := newJobForDeployTest(t, s, "machine-1")
	jobB := newJobForDeployTest(t, s, "machine-1")
	if _, err := s.ClaimJob(jobA, "machine-1", deployTestNow, time.Minute); err != nil {
		t.Fatalf("claim job A: %v", err)
	}
	jobBToken, err := s.ClaimJob(jobB, "machine-1", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("claim job B: %v", err)
	}

	before := countRows(t, s, `SELECT COUNT(*) FROM job_events WHERE job_id=?`, jobA)
	if _, err := s.AppendJobEventWithReplay(jobA, "machine-1", jobBToken, 41, "start", `{}`,
		deployTestNow, deployTestNow); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("job B token writing job A event error=%v want ErrLeaseInvalid", err)
	}
	if after := countRows(t, s, `SELECT COUNT(*) FROM job_events WHERE job_id=?`, jobA); after != before {
		t.Fatalf("rejected job A event changed row count from %d to %d", before, after)
	}
}

func TestRecordVerificationRejectsAnotherJobsLeaseTokenOnSameMachine(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-1")
	jobA := newJobForDeployTest(t, s, "machine-1")
	jobB := newJobForDeployTest(t, s, "machine-1")
	if _, err := s.ClaimJob(jobA, "machine-1", deployTestNow, time.Minute); err != nil {
		t.Fatalf("claim job A: %v", err)
	}
	jobBToken, err := s.ClaimJob(jobB, "machine-1", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("claim job B: %v", err)
	}

	before := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobA)
	if err := s.RecordVerification(jobA, "machine-1", jobBToken, "health", "true", 0,
		"ok", "", true, deployTestNow); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("job B token writing job A verification error=%v want ErrLeaseInvalid", err)
	}
	if after := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobA); after != before {
		t.Fatalf("rejected job A verification changed row count from %d to %d", before, after)
	}
}

// Older Hub binaries accepted larger event payloads. An exact retry must still
// heal the event/state crash window after an upgrade, while a new oversized
// event must not extend that legacy exception.
func TestOversizedLegacyJobEventCanReplayButCannotBeInserted(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	occurredAt := deployTestNow.Add(-time.Minute)
	legacyPayload := `{"detail":"` + strings.Repeat("x", maxJobEventPayloadBytes) + `"}`
	if len(legacyPayload) <= maxJobEventPayloadBytes {
		t.Fatal("legacy test payload is not oversized")
	}
	if _, err := s.DB().Exec(`
INSERT INTO job_events (event_id, job_id, seq, phase, occurred_at, received_at, payload)
VALUES (?, ?, ?, ?, ?, ?, ?)`, "legacy-oversized-event", jobID, 7, "start",
		fmtTime(occurredAt), fmtTime(deployTestNow), legacyPayload); err != nil {
		t.Fatalf("insert legacy oversized event: %v", err)
	}

	replayed, err := s.AppendJobEventWithReplay(jobID, "machine-a", token, 7, "start",
		legacyPayload, occurredAt, deployTestNow.Add(time.Second))
	if err != nil || !replayed {
		t.Fatalf("exact legacy replay: replayed=%v err=%v", replayed, err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM job_events WHERE job_id=?`, jobID); got != 1 {
		t.Fatalf("legacy replay changed row count to %d", got)
	}

	if _, err := s.AppendJobEventWithReplay(jobID, "machine-a", token, 8, "start",
		legacyPayload, occurredAt, deployTestNow); !errors.Is(err, ErrInvalidJobEvidence) {
		t.Fatalf("new oversized event error=%v want ErrInvalidJobEvidence", err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM job_events WHERE job_id=?`, jobID); got != 1 {
		t.Fatalf("rejected oversized event changed row count to %d", got)
	}
}

func TestInvalidMetadataLegacyJobEventCanReplayButCannotBeInserted(t *testing.T) {
	tests := []struct {
		name       string
		seq        int
		occurredAt time.Time
		newSeq     int
	}{
		{name: "negative sequence", seq: -1, occurredAt: deployTestNow, newSeq: -2},
		{name: "zero occurred at", seq: 7, occurredAt: time.Time{}, newSeq: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newDeployTestStore(t)
			registerDeployMachine(t, s, "machine-a")
			jobID := newJobForDeployTest(t, s, "machine-a")
			token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`
INSERT INTO job_events (event_id, job_id, seq, phase, occurred_at, received_at, payload)
VALUES (?, ?, ?, ?, ?, ?, ?)`, "legacy-invalid-metadata", jobID, test.seq, "start",
				fmtTime(test.occurredAt), fmtTime(deployTestNow), `{}`); err != nil {
				t.Fatalf("insert legacy event: %v", err)
			}

			replayed, err := s.AppendJobEventWithReplay(jobID, "machine-a", token, test.seq,
				"start", `{}`, test.occurredAt, deployTestNow)
			if err != nil || !replayed {
				t.Fatalf("exact legacy replay: replayed=%v err=%v", replayed, err)
			}
			if _, err := s.AppendJobEventWithReplay(jobID, "machine-a", token, test.newSeq,
				"start", `{}`, test.occurredAt, deployTestNow); !errors.Is(err, ErrInvalidJobEvidence) {
				t.Fatalf("new invalid event error=%v want ErrInvalidJobEvidence", err)
			}
			if got := countRows(t, s, `SELECT COUNT(*) FROM job_events WHERE job_id=?`, jobID); got != 1 {
				t.Fatalf("invalid event changed row count to %d", got)
			}
		})
	}
}

// ⚠ 守住 ClaimJob 漏把 machine_id 放進 WHERE，讓別台機器領走工作單的錯。
func TestClaimJobRejectsOtherMachineAsNotFound(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "machine-b")
	jobID := newJobForDeployTest(t, s, "machine-a")
	if _, err := s.ClaimJob(jobID, "machine-b", deployTestNow, time.Minute); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("B 領 A 的工作單回傳 %v，預期 ErrJobNotFound", err)
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回 A 的工作單失敗：%v", err)
	}
	if job.State != deploy.NotStarted || job.LeaseToken != "" {
		t.Fatalf("B 領單失敗後仍改動 A 的工作單：%+v", job)
	}
}

// ⚠⚠ 守的是「agent 自己送 VerificationPassed 就把單標成 succeeded」。
// 那個事件在狀態機上是 succeeded 的唯一入口 —— 讓機器送得出來，
// 等於讓被測對象自己宣布通過，verification_results 那張表就沒有意義了。
func TestAgentMayNotSendHubOnlyEvents(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("領取測試工作單失敗：%v", err)
	}
	for _, ev := range []deploy.Event{deploy.VerificationPassed, deploy.VerificationFailed, deploy.Timeout, deploy.LeaseLost} {
		if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, ev, deployTestNow); !errors.Is(err, ErrEventNotForAgent) {
			t.Errorf("agent 送 %s 應該被拒絕，拿到 %v", ev, err)
		}
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.Claimed {
		t.Errorf("被拒絕的事件不該動到狀態，現在是 %s", job.State)
	}
}

// ⚠⚠ Hub 的 reaper 入口也不是「相信這個 caller」的後門。
// VerificationPassed 若能從這裡進去，就能在沒有任何
// verification_results 的情況下製造 succeeded，再被 promote 當真。
func TestHubMayOnlySendReaperEvents(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("領取測試工作單失敗：%v", err)
	}
	for _, ev := range []deploy.Event{deploy.Start, deploy.FinishWork} {
		if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, ev, deployTestNow); err != nil {
			t.Fatalf("推進 %s 失敗：%v", ev, err)
		}
	}

	for _, ev := range []deploy.Event{
		deploy.VerificationPassed,
		deploy.VerificationFailed,
		deploy.Claim,
		deploy.Start,
		deploy.FinishWork,
		deploy.Reject,
		deploy.Event("未知事件"),
	} {
		if _, err := s.AdvanceJobByHub(jobID, ev, deployTestNow); !errors.Is(err, ErrEventNotForHub) {
			t.Errorf("Hub 送 %s 應該被拒絕，拿到 %v", ev, err)
		}
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.Verifying {
		t.Fatalf("被拒絕的 Hub 事件不該改動狀態，現在是 %s", job.State)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID); got != 0 {
		t.Fatalf("未寫驗證卻有 %d 列 verification_results", got)
	}
}

// ⚠ 守的是狀態推進繞過 internal/deploy 自己寫 SQL：
// 那個狀態機如果只被測試呼叫、沒有被真正的寫入路徑呼叫，它就不是規則，
// 只是一份文件 —— 而文件擋不住任何事情。
func TestAdvanceJobRefusesIllegalTransition(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	jobID := newJobForDeployTest(t, s, "machine-a")
	token, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute)
	if err != nil {
		t.Fatalf("領取測試工作單失敗：%v", err)
	}
	// claimed 收到 FinishWork 是跳過 running，狀態機不准。
	if _, err := s.AdvanceJobByAgent(jobID, "machine-a", token, deploy.FinishWork, deployTestNow); err == nil {
		t.Fatal("claimed 直接 FinishWork 應該被狀態機擋下")
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.Claimed {
		t.Errorf("被擋下的轉移不該動到狀態，現在是 %s", job.State)
	}
}

// ⚠⚠ 守的是不可逆的工作單逾時之後被記成 failed —— 而 failed 的意思是
// 「已經回到舊版」。那句話是假的，而它會讓人不去看那台機器。
func TestHubTimeoutOnIrreversibleJobLandsInManualIntervention(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	desiredID, rev := desiredForDeployTest(t, s)
	jobID, err := s.CreateJob("machine-a", desiredID, rev, NewJob{Irreversible: true})
	if err != nil {
		t.Fatalf("建立不可逆工作單失敗：%v", err)
	}
	if _, err := s.ClaimJob(jobID, "machine-a", deployTestNow, time.Minute); err != nil {
		t.Fatalf("領取測試工作單失敗：%v", err)
	}
	got, err := s.AdvanceJobByHub(jobID, deploy.Timeout, deployTestNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("Hub 判逾時失敗：%v", err)
	}
	if got != deploy.ManualIntervention {
		t.Errorf("不可逆的單逾時應該是 %s，拿到 %s", deploy.ManualIntervention, got)
	}
	job, err := s.JobForMachine(jobID, "machine-a")
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.ManualIntervention || job.TerminalAt == nil {
		t.Errorf("終態或終態時間不符：%+v", job)
	}
}

func TestBATServerMeasuredEvidenceRequiresOwnUnitAndEndpoint(t *testing.T) {
	artifactHash := strings.Repeat("e", 64)
	binaryHash := strings.Repeat("c", 64)
	release := "/home/operator/.local/share/clawctl/bat-server/releases/3.2.10"
	type evidenceRow struct{ rule, command, stdout string }
	for _, tc := range []struct {
		name  string
		edit  func([]evidenceRow) []evidenceRow
		ready bool
	}{
		{"every row", func(rows []evidenceRow) []evidenceRow { return rows }, true},
		{"the machine's own unit", func(rows []evidenceRow) []evidenceRow {
			rows[3].command = "systemctl --user is-active bat-server.service"
			return rows
		}, false},
		{"no endpoint row", func(rows []evidenceRow) []evidenceRow { return rows[:4] }, false},
		{"endpoint row for another address", func(rows []evidenceRow) []evidenceRow {
			rows[4].command = "bat-remote auth 127.0.0.1:9876"
			return rows
		}, false},
		{"endpoint row without its newline", func(rows []evidenceRow) []evidenceRow {
			rows[4].stdout = "authenticated"
			return rows
		}, false},
		{"endpoint row with other output", func(rows []evidenceRow) []evidenceRow {
			rows[4].stdout = "authenticated\nclosed\n"
			return rows
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDeployTestStore(t)
			registerDeployMachine(t, s, "bat-endpoint")
			jobID := insertMeasuredEvidenceJob(t, s, "bat-endpoint", "bat-server", batServerEvidenceSpec("amd64", artifactHash, binaryHash))
			token := advanceMeasuredEvidenceJob(t, s, jobID, "bat-endpoint")
			for _, row := range tc.edit([]evidenceRow{
				{"bat-server-release", "test -d " + release, release + "\n"},
				{"bat-server-artifact", "cat " + release + "/.clawctl-artifact-sha256", "sha256:" + artifactHash + "\n"},
				{"bat-server-binary", "sha256sum " + release + "/bat-server-linux-x86_64/bat-server", binaryHash + "\n"},
				{"bat-server-unit", "systemctl --user is-active " + model.BATServerUnit, "active\n"},
				{"bat-server-endpoint", model.BATServerEndpointCommand, "authenticated\n"},
			}) {
				if err := s.RecordVerification(jobID, "bat-endpoint", token, row.rule, row.command, 0, row.stdout, "", true, deployTestNow); err != nil {
					t.Fatal(err)
				}
			}
			if _, ready, err := s.measuredEvidenceStatus(jobID); err != nil || ready != tc.ready {
				t.Fatalf("ready=%t err=%v, want ready=%t", ready, err, tc.ready)
			}
		})
	}
}
