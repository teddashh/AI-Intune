package store

import (
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

// assignmentFleet 是一個造得出「現在誰身上有哪一份 profile」的帳本。
//
// ⚠ 這裡不走 preview/apply 那條路，而是直接寫指派那兩張表 —— 要測的是讀的人
// 看到什麼，所以寫進去的形狀必須跟正式庫一樣，包含 supersedes 那條鏈；走 apply
// 的話每一個 case 都得先湊出一份 digest 與一次確認，而那測的是別的東西。
type assignmentFleet struct {
	t     *testing.T
	store *Store
	next  int64
	// digests 是每一個套件在目錄上那一份 manifest 的 digest。指派那張表的
	// authority trigger 逐欄比對它，所以測試不能自己編一個。
	digests map[string]string
}

func newAssignmentFleet(t *testing.T) *assignmentFleet {
	t.Helper()
	s := newTestStore(t)
	node, openclaw := storedCatalogFixture()
	digests := map[string]string{}
	for _, manifest := range []appcatalog.Manifest{node, openclaw} {
		record, err := s.PublishCatalogManifest(manifest, "operator:test")
		if err != nil {
			t.Fatalf("publish manifest %s: %v", manifest.ID, err)
		}
		digests[manifest.ID+"@"+manifest.Version] = record.Digest
	}
	return &assignmentFleet{t: t, store: s, digests: digests}
}

func (f *assignmentFleet) machine(name string) string {
	f.t.Helper()
	id, _, err := f.store.CreateEnrollTokenFor(name, 0)
	if err != nil {
		f.t.Fatalf("enrol %s: %v", name, err)
	}
	return id
}

func (f *assignmentFleet) profile(id string, revision int64, packages ...appcatalog.PackageRef) string {
	f.t.Helper()
	record, err := f.store.PublishMachineProfile(appcatalog.MachineProfile{
		SchemaVersion: 1, ID: id, Revision: revision, Packages: packages,
	}, "operator:test")
	if err != nil {
		f.t.Fatalf("publish profile %s@%d: %v", id, revision, err)
	}
	return record.Digest
}

// assign 寫一筆指派，並替它點名的每一個套件寫一列安裝意圖與一張工作單 ——
// 正式庫上指派就是這樣落地的，少了那兩筆，讀出來的形狀跟真的不一樣。
func (f *assignmentFleet) assign(machineID, profileID string, profileRevision int64,
	digest string, at time.Time, packages ...appcatalog.PackageRef,
) string {
	f.t.Helper()
	f.next++
	assignmentID := "assignment-" + fmtInt(f.next)
	// ⚠ assignment_revision 是**每台機器自己**的號碼帶，從 1 開始逐一往上；
	// 帳本的 insert guard 逐欄比對它。用一條全機隊共用的號碼帶寫進去，第二台
	// 機器的第一份指派就會被擋下來。
	previous, revision := f.currentAssignment(machineID)
	var supersedes any
	if previous != "" {
		supersedes = previous
	}
	if _, err := f.store.DB().Exec(`INSERT INTO machine_profile_assignments
 (assignment_id,machine_id,assignment_revision,profile_id,profile_revision,profile_digest,
  target_os,target_arch,assigned_at,assigned_by,supersedes_assignment_id)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		assignmentID, machineID, revision+1, profileID, profileRevision, digest,
		"linux", "amd64", fmtTime(at), "operator:test", supersedes); err != nil {
		f.t.Fatalf("insert assignment: %v", err)
	}
	for position, ref := range packages {
		desiredID, jobID := mustAssignmentLedgerRows(f.t, f.store, machineID, ref, at)
		if _, err := f.store.DB().Exec(`INSERT INTO machine_profile_assignment_packages
 (assignment_id,position,package_id,package_version,manifest_digest,desired_id,job_id,direct)
 VALUES (?,?,?,?,?,?,?,?)`,
			assignmentID, position, ref.PackageID, ref.Version,
			f.digests[ref.PackageID+"@"+ref.Version], desiredID, jobID, 1); err != nil {
			f.t.Fatalf("insert assignment package: %v", err)
		}
	}
	return assignmentID
}

func (f *assignmentFleet) currentAssignment(machineID string) (string, int64) {
	f.t.Helper()
	var id string
	var revision int64
	err := f.store.DB().QueryRow(`SELECT assignment_id,assignment_revision
 FROM machine_profile_assignments
 WHERE machine_id=? ORDER BY assignment_revision DESC LIMIT 1`, machineID).Scan(&id, &revision)
	if err != nil {
		return "", 0
	}
	return id, revision
}

// mustAssignmentLedgerRows 寫一列安裝意圖與一張工作單，走的是 profile 指派那條路
// 用的同一對 tx 函式。
//
// ⚠ 不走 CreateDesiredState／CreateJob 那兩個公開包裝，因為它們擋掉所有 managed
// catalog kind（openclaw 是其中之一）——擋的正是「不經過指派或部署就直接寫」，
// 而這裡模擬的就是指派本身，所以走 createManagedJobTx，跟
// mutateOperatorProfileAssignmentTx 同一個。
func mustAssignmentLedgerRows(t *testing.T, s *Store, machineID string,
	ref appcatalog.PackageRef, at time.Time,
) (string, string) {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	desiredID, revision, err := createDesiredStateTx(tx, "machine", machineID, ref.PackageID, ref.PackageID,
		`{"kind":"openclaw","version":"`+ref.Version+`"}`, "operator:test", at)
	if err != nil {
		t.Fatalf("create desired state: %v", err)
	}
	jobID, err := createManagedJobTx(tx, machineID, desiredID, revision, NewJob{}, at)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return desiredID, jobID
}

func fmtInt(value int64) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func assignmentsByMachine(t *testing.T, s *Store) map[string]ProfileAssignment {
	t.Helper()
	rows, err := s.FleetProfileAssignments()
	if err != nil {
		t.Fatalf("fleet profile assignments: %v", err)
	}
	out := make(map[string]ProfileAssignment, len(rows))
	for _, row := range rows {
		if _, duplicate := out[row.MachineID]; duplicate {
			t.Fatalf("同一台機器回了兩份現行指派：%s", row.MachineID)
		}
		out[row.MachineID] = row
	}
	return out
}

func openclawRef(version string) appcatalog.PackageRef {
	return appcatalog.PackageRef{PackageID: "openclaw", Version: version}
}

// 一台機器可以被指派很多次，但「現在身上是哪一份」只有一個答案。回歷史等於讓
// 畫面自己挑一筆，而畫面挑的那一筆不保證是 agent 收到的那一筆。
func TestOnlyTheProfileAMachineIsWearingNowComesBack(t *testing.T) {
	f := newAssignmentFleet(t)
	at := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	cnode := f.machine("samplehub1")
	first := f.profile("openclaw-standard", 1, openclawRef("2026.9.2"))
	second := f.profile("openclaw-standard", 2, openclawRef("2026.9.2"))
	f.assign(cnode, "openclaw-standard", 1, first, at, openclawRef("2026.9.2"))
	f.assign(cnode, "openclaw-standard", 2, second, at.Add(time.Hour), openclawRef("2026.9.2"))

	got := assignmentsByMachine(t, f.store)
	if len(got) != 1 {
		t.Fatalf("回了 %d 份指派", len(got))
	}
	if got[cnode].ProfileRevision != 2 {
		t.Fatalf("現在身上那一份是 revision %d", got[cnode].ProfileRevision)
	}
}

// ⚠⚠ 這是這個 reader 存在的理由：一份發佈了卻一台都沒指派的 profile。
// 帳本上 machine_profiles 有列、machine_profile_assignments 沒有列 —— 而讀的人
// 必須看得出差別，不能被回一份空清單就以為那份 profile 不存在。
func TestAProfilePublishedToNobodyLeavesNoAssignmentAtAll(t *testing.T) {
	f := newAssignmentFleet(t)
	f.machine("samplehub1")
	f.profile("openclaw-standard", 1, openclawRef("2026.9.2"))

	rows, err := f.store.FleetProfileAssignments()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("沒有指派過卻回了 %d 列", len(rows))
	}
	profiles, err := f.store.MachineProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 {
		t.Fatalf("發佈的 profile 有 %d 份", len(profiles))
	}
}

// 兩台機器各自有自己的現行指派，不會因為在同一個查詢裡就互相蓋掉。
func TestTwoMachinesKeepTheirOwnCurrentAssignment(t *testing.T) {
	f := newAssignmentFleet(t)
	at := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	cnode, onode := f.machine("samplehub1"), f.machine("sampleagent2")
	first := f.profile("openclaw-standard", 1, openclawRef("2026.9.2"))
	second := f.profile("openclaw-edge", 1, openclawRef("2026.9.2"))
	f.assign(cnode, "openclaw-standard", 1, first, at, openclawRef("2026.9.2"))
	f.assign(onode, "openclaw-edge", 1, second, at, openclawRef("2026.9.2"))
	// samplehub1 再被指派一次，sampleagent2 那一份不准跟著動。
	f.assign(cnode, "openclaw-edge", 1, second, at.Add(time.Hour), openclawRef("2026.9.2"))

	got := assignmentsByMachine(t, f.store)
	if len(got) != 2 {
		t.Fatalf("回了 %d 台", len(got))
	}
	if got[cnode].ProfileID != "openclaw-edge" || got[onode].ProfileID != "openclaw-edge" {
		t.Fatalf("cnode=%s onode=%s", got[cnode].ProfileID, got[onode].ProfileID)
	}
	if got[cnode].AssignmentRevision <= got[onode].AssignmentRevision {
		t.Fatalf("cnode rev=%d onode rev=%d", got[cnode].AssignmentRevision, got[onode].AssignmentRevision)
	}
}

// 一份指派點名的每一個套件都要回來，照 position 排，而且每一個都要帶著它在帳本上
// 留下的那兩筆 —— 少了 desired_id 與 job_id，畫面就只說得出「指派了」，
// 說不出「指派變成了什麼」。
func TestEveryPackageInAnAssignmentComesBackInOrderWithItsLedgerRows(t *testing.T) {
	f := newAssignmentFleet(t)
	at := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	cnode := f.machine("samplehub1")
	node := appcatalog.PackageRef{PackageID: "node-runtime", Version: "24.15.0"}
	digest := f.profile("openclaw-standard", 1, node, openclawRef("2026.9.2"))
	f.assign(cnode, "openclaw-standard", 1, digest, at, node, openclawRef("2026.9.2"))

	got := assignmentsByMachine(t, f.store)[cnode]
	if len(got.Packages) != 2 {
		t.Fatalf("回了 %d 個套件：%+v", len(got.Packages), got.Packages)
	}
	if got.Packages[0].PackageID != "node-runtime" || got.Packages[1].PackageID != "openclaw" {
		t.Fatalf("套件順序=%+v", got.Packages)
	}
	for _, pkg := range got.Packages {
		if pkg.DesiredID == "" || pkg.JobID == "" || !pkg.Direct {
			t.Fatalf("套件少了帳本那兩筆：%+v", pkg)
		}
	}
}

// 一份什麼都沒帶的指派仍然是一份指派。LEFT JOIN 的空側被當成「沒有這一列」丟掉的
// 話，那台機器會變成「沒有被指派過」——而那是一句跟事實相反的話。
func TestAnAssignmentThatCarriesNoPackageIsStillAnAssignment(t *testing.T) {
	f := newAssignmentFleet(t)
	at := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	cnode := f.machine("samplehub1")
	digest := f.profile("openclaw-standard", 1, openclawRef("2026.9.2"))
	f.assign(cnode, "openclaw-standard", 1, digest, at)

	got := assignmentsByMachine(t, f.store)
	if len(got) != 1 || got[cnode].AssignmentID == "" {
		t.Fatalf("got=%+v", got)
	}
	if len(got[cnode].Packages) != 0 {
		t.Fatalf("空指派卻帶了 %d 個套件", len(got[cnode].Packages))
	}
}

// 兩台機器相鄰的時候，一台的套件不准被算到另一台頭上。
func TestOneMachinesPackagesNeverLandOnAnother(t *testing.T) {
	f := newAssignmentFleet(t)
	at := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	node := appcatalog.PackageRef{PackageID: "node-runtime", Version: "24.15.0"}
	cnode, onode := f.machine("samplehub1"), f.machine("sampleagent2")
	digest := f.profile("openclaw-standard", 1, node, openclawRef("2026.9.2"))
	f.assign(cnode, "openclaw-standard", 1, digest, at, node, openclawRef("2026.9.2"))
	f.assign(onode, "openclaw-standard", 1, digest, at, node)

	got := assignmentsByMachine(t, f.store)
	if len(got[cnode].Packages) != 2 || len(got[onode].Packages) != 1 {
		t.Fatalf("cnode=%d onode=%d", len(got[cnode].Packages), len(got[onode].Packages))
	}
}

// 每一份指派都要帶得出它是誰、什麼時候、指的是哪一份 profile 的哪一版與哪一個
// digest —— 沒有這些，「現在誰有」就沒有辦法被質疑。
func TestEveryAssignmentCarriesWhoAssignedItAndWhichRevision(t *testing.T) {
	f := newAssignmentFleet(t)
	at := time.Date(2026, 9, 10, 15, 0, 0, 0, time.UTC)
	cnode := f.machine("samplehub1")
	digest := f.profile("openclaw-standard", 1, openclawRef("2026.9.2"))
	f.assign(cnode, "openclaw-standard", 1, digest, at, openclawRef("2026.9.2"))

	got := assignmentsByMachine(t, f.store)[cnode]
	if got.ProfileID != "openclaw-standard" || got.ProfileRevision != 1 ||
		got.ProfileDigest != digest || got.AssignedBy != "operator:test" ||
		!got.AssignedAt.Equal(at) || got.AssignmentRevision <= 0 {
		t.Fatalf("got=%+v want digest=%s at=%s", got, digest, at)
	}
}

func TestAnEmptyLedgerHasNoAssignmentsAndNoError(t *testing.T) {
	rows, err := newAssignmentFleet(t).store.FleetProfileAssignments()
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}
