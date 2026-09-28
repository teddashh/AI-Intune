package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func successfulOperatorProfileAssignment(t *testing.T, key string) (*Store, time.Time, OperatorMachineProfileAssignmentRequest) {
	t.Helper()
	st, machineID, now, prepared := operatorProfileAssignmentFixture(t)
	if _, err := st.DB().Exec(`INSERT INTO machine_checkins
		(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,1)`, machineID, fmtTime(now), fmtTime(now)); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorMachineProfileAssignment(machineID, "node-profile", 1, prepared)
	if err != nil {
		t.Fatal(err)
	}
	request := operatorProfileAssignmentTestRequest(machineID, preview)
	request.IdempotencyKey = key
	request.RequestDigest = sha256Digest([]byte(key))
	if _, err := st.ApplyOperatorMachineProfileAssignment(request, func() (OperatorMachineProfileAssignmentPrepared, error) { return prepared, nil }); err != nil {
		t.Fatal(err)
	}
	return st, now, request
}

func operatorProfileAssignmentCachedReceipt(t *testing.T, st *Store, key string) operatorProfileAssignmentReceipt {
	t.Helper()
	var raw string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeOperatorProfileAssignmentReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func storeOperatorProfileAssignmentCachedReceipt(t *testing.T, st *Store, key string, receipt operatorProfileAssignmentReceipt) {
	t.Helper()
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`, string(raw), key); err != nil {
		t.Fatal(err)
	}
}

func assertOperatorProfileAssignmentReplayRejected(t *testing.T, st *Store, request OperatorMachineProfileAssignmentRequest, consequence string) {
	t.Helper()
	before := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	assignmentsBefore := countRows(t, st, `SELECT COUNT(*) FROM machine_profile_assignments`)
	packagesBefore := countRows(t, st, `SELECT COUNT(*) FROM machine_profile_assignment_packages`)
	desiredBefore := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
	jobsBefore := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
	idempotencyBefore := countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`)
	result, err := st.ApplyOperatorMachineProfileAssignment(request, func() (OperatorMachineProfileAssignmentPrepared, error) {
		t.Fatal("replay called the prepare callback")
		return OperatorMachineProfileAssignmentPrepared{}, nil
	})
	if err == nil {
		t.Errorf("err=%v, expected cached receipt rejection; %s", err, consequence)
	}
	if result.AssignmentID != "" || result.AssignmentRevision != 0 || result.MachineID != "" || result.DisplayName != "" || result.LifecycleRevision != 0 ||
		result.ProfileID != "" || result.ProfileRevision != 0 || result.ProfileDigest != "" || result.Target.OS != "" || result.Target.Arch != "" ||
		!result.AssignedAt.IsZero() || len(result.Packages) != 0 || result.AlreadyAssigned || result.PreviewDigest != "" || result.Replayed || !result.Audited {
		t.Errorf("result={AssignmentID:%q AssignmentRevision:%d MachineID:%q DisplayName:%q LifecycleRevision:%d ProfileID:%q ProfileRevision:%d ProfileDigest:%q "+
			"Target:%+v AssignedAt:%s Packages:%v AlreadyAssigned:%t PreviewDigest:%q Replayed:%t Audited:%t}; expected empty operator fields and Audited=true; %s",
			result.AssignmentID, result.AssignmentRevision, result.MachineID, result.DisplayName, result.LifecycleRevision, result.ProfileID, result.ProfileRevision,
			result.ProfileDigest, result.Target, result.AssignedAt, result.Packages, result.AlreadyAssigned, result.PreviewDigest, result.Replayed, result.Audited, consequence)
	}
	after := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	entries, auditErr := st.Audit("", 1)
	if auditErr != nil {
		t.Fatal(auditErr)
	}
	actualDetail := "<missing>"
	if len(entries) > 0 {
		actualDetail = entries[0].Detail
	}
	if after-before != 1 || actualDetail != operatorProfileAssignmentCacheInvalidDetail {
		t.Errorf("audit row delta=%d, Detail=%q; expected delta=1, Detail=%q; 被拒絕的重放若沒有精確稽核，operator 事後無法確認這份指派收據為何沒有生效",
			after-before, actualDetail, operatorProfileAssignmentCacheInvalidDetail)
	}
	assignmentsAfter := countRows(t, st, `SELECT COUNT(*) FROM machine_profile_assignments`)
	packagesAfter := countRows(t, st, `SELECT COUNT(*) FROM machine_profile_assignment_packages`)
	desiredAfter := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
	jobsAfter := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
	idempotencyAfter := countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`)
	if assignmentsAfter != assignmentsBefore || packagesAfter != packagesBefore || desiredAfter != desiredBefore ||
		jobsAfter != jobsBefore || idempotencyAfter != idempotencyBefore {
		t.Errorf("authority row counts before=(%d,%d,%d,%d,%d), after=(%d,%d,%d,%d,%d); 被拒絕的重放若動到了這五張表任何一張，operator 的指派權威會被一次它自己都不承認的重放改寫",
			assignmentsBefore, packagesBefore, desiredBefore, jobsBefore, idempotencyBefore,
			assignmentsAfter, packagesAfter, desiredAfter, jobsAfter, idempotencyAfter)
	}
}

func TestOperatorProfileAssignmentReplayRejectsReceiptThatLeftItsAssignment(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*operatorProfileAssignmentReceipt)
		consequence string
	}{
		{
			name: "schema version",
			mutate: func(receipt *operatorProfileAssignmentReceipt) {
				receipt.SchemaVersion = operatorProfileAssignmentVersion + "-next"
			},
			consequence: "收據宣稱它是另一個版本的收據格式寫的，Hub 會去背書一份它自己沒寫過的形狀",
		},
		{
			name: "lifecycle revision",
			mutate: func(receipt *operatorProfileAssignmentReceipt) {
				receipt.LifecycleRevision = -1
			},
			consequence: "負數 lifecycle revision 顯然不能當版本條件使用，operator 不能拿這份收據做後續條件操作",
		},
		{
			name: "display name",
			mutate: func(receipt *operatorProfileAssignmentReceipt) {
				receipt.DisplayName = ""
			},
			consequence: "重放交回一個沒有名字的指派結果，用名字對帳、選台或開工單的人會拿到一份指不出對象的紀錄",
		},
		{
			name: "preview digest",
			mutate: func(receipt *operatorProfileAssignmentReceipt) {
				receipt.PreviewDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
			consequence: "重放交回的收據宣稱它答的是另一份確認畫面，operator 對帳時會以為自己確認過的是別的內容",
		},
		{
			name: "assigned_at left utc",
			mutate: func(receipt *operatorProfileAssignmentReceipt) {
				receipt.AssignedAt = receipt.AssignedAt.In(time.FixedZone("UTC+8", 8*60*60))
			},
			consequence: "重放時間雖與原指派同秒，卻不是三張表裡的 canonical 時間；operator 對帳會失配，原樣送進要求 canonical 時間的入口也會被拒",
		},
		{
			name: "assigned_at carries a fraction",
			mutate: func(receipt *operatorProfileAssignmentReceipt) {
				receipt.AssignedAt = receipt.AssignedAt.Add(500 * time.Millisecond)
			},
			consequence: "重放時間雖與原指派同秒，卻不是三張表裡的 canonical 時間；operator 對帳會失配，原樣送進要求 canonical 時間的入口也會被拒",
		},
		{
			name: "planned revision",
			mutate: func(receipt *operatorProfileAssignmentReceipt) {
				receipt.Packages[0].PlannedRevision = receipt.Packages[0].Revision + 5
				receipt.Packages[0].CurrentRevision = receipt.Packages[0].PlannedRevision - 1
			},
			consequence: "收據聲稱推到的版本不是 Hub 寫入 desired_state.revision 的版本；operator 拿它做 If-Match 會被拒，或誤以為升級完成而不再追 job",
		},
		{
			name: "current revision",
			mutate: func(receipt *operatorProfileAssignmentReceipt) {
				receipt.Packages[0].CurrentRevision++
			},
			consequence: "收據聲稱從 current 推進一格到 planned，兩數卻不再相差一格；operator 會誤判這台現在停在哪一版",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st, _, request := successfulOperatorProfileAssignment(t, "profile-replay-"+test.name)
			receipt := operatorProfileAssignmentCachedReceipt(t, st, request.IdempotencyKey)
			test.mutate(&receipt)
			storeOperatorProfileAssignmentCachedReceipt(t, st, request.IdempotencyKey, receipt)
			assertOperatorProfileAssignmentReplayRejected(t, st, request, test.consequence)
		})
	}
}

func TestOperatorProfileAssignmentNoOpReplayRejectsARevisionThatMoved(t *testing.T) {
	const consequence = "already_assigned 的收據宣稱這次沒有推進，兩個版本卻不相等；operator 會以為這台早就在目標版本而不再核對套件是否落地"
	st, machineID, now, prepared := operatorProfileAssignmentFixture(t)
	if _, err := st.DB().Exec(`INSERT INTO machine_checkins
		(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,1)`, machineID, fmtTime(now), fmtTime(now)); err != nil {
		t.Fatal(err)
	}
	firstPreview, err := st.PreviewOperatorMachineProfileAssignment(machineID, "node-profile", 1, prepared)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := operatorProfileAssignmentTestRequest(machineID, firstPreview)
	firstRequest.IdempotencyKey = "profile-no-op-first"
	firstRequest.RequestDigest = sha256Digest([]byte(firstRequest.IdempotencyKey))
	first, err := st.ApplyOperatorMachineProfileAssignment(firstRequest, func() (OperatorMachineProfileAssignmentPrepared, error) { return prepared, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`, deploy.Succeeded, fmtTime(now), first.Packages[0].JobID); err != nil {
		t.Fatal(err)
	}
	secondPreview, err := st.PreviewOperatorMachineProfileAssignment(machineID, "node-profile", 1, prepared)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := operatorProfileAssignmentTestRequest(machineID, secondPreview)
	secondRequest.IdempotencyKey = "profile-no-op-second"
	secondRequest.RequestDigest = sha256Digest([]byte(secondRequest.IdempotencyKey))
	if _, err := st.ApplyOperatorMachineProfileAssignment(secondRequest, func() (OperatorMachineProfileAssignmentPrepared, error) { return prepared, nil }); err != nil {
		t.Fatal(err)
	}
	receipt := operatorProfileAssignmentCachedReceipt(t, st, secondRequest.IdempotencyKey)
	if !receipt.AlreadyAssigned {
		t.Errorf("AlreadyAssigned=%t, expected true; no-op 收據若不是 already_assigned，就沒有覆蓋不推進版本的重放路徑", receipt.AlreadyAssigned)
	}
	receipt.Packages[0].CurrentRevision++
	storeOperatorProfileAssignmentCachedReceipt(t, st, secondRequest.IdempotencyKey, receipt)
	assertOperatorProfileAssignmentReplayRejected(t, st, secondRequest, consequence)
}

func TestOperatorProfileAssignmentReplayRejectsCacheRowTimeThatLeftItsReceipt(t *testing.T) {
	const consequence = "operator_idempotency 那一列的 created_at 離開了它存的那份收據，這一列不再能證明它存的是那一次決定"
	st, now, request := successfulOperatorProfileAssignment(t, "profile-cache-row-time")
	var cachedCreatedAt string
	if err := st.DB().QueryRow(`SELECT created_at FROM operator_idempotency WHERE idempotency_key=?`, request.IdempotencyKey).Scan(&cachedCreatedAt); err != nil {
		t.Fatal(err)
	}
	if cachedCreatedAt != fmtTime(now) {
		t.Errorf("cache created_at=%q, expected %q; operator 的原決定已被歸到錯的時間，這一列無法證明它存的是那一次決定", cachedCreatedAt, fmtTime(now))
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET created_at=? WHERE idempotency_key=?`, fmtTime(now.Add(time.Second)), request.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	assertOperatorProfileAssignmentReplayRejected(t, st, request, consequence)
}
