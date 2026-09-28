package deploy

import "testing"

// 這支守 SPEC §5.3 的亂序洞：43 已套用後，晚到的 41 不得讓機器降版。
func TestLaterStaleRevisionIsRejectedAfterNewestApplied(t *testing.T) {
	w := Watermarks{MaxSeen: 40, MaxApplied: 40}
	admission, w := Admit(w, 43)
	if !admission.Accepted || admission.Reason != "" {
		t.Fatalf("revision 43 應被允收，得到 %+v", admission)
	}
	w = Applied(w, 43)

	admission, got := Admit(w, 41)
	if admission.Accepted || admission.Reason != StaleRevision {
		t.Fatalf("晚到的 revision 41 應以 %q 拒絕，得到 %+v", StaleRevision, admission)
	}
	if got != w {
		t.Fatalf("拒絕舊 revision 不得改動水位：原為 %+v，得到 %+v", w, got)
	}
}

// 這支守整個 package 存在的理由：43 即使套用失敗，MaxSeen 也已是 43，
// 41 仍須被拒絕；只記 MaxApplied 會讓 41 成功降版而且全綠。
func TestFailedNewestRevisionStillBlocksOlderRevision(t *testing.T) {
	w := Watermarks{MaxSeen: 40, MaxApplied: 40}
	admission, w := Admit(w, 43)
	if !admission.Accepted {
		t.Fatalf("revision 43 應被允收，得到 %+v", admission)
	}
	if w.MaxSeen != 43 || w.MaxApplied != 40 {
		t.Fatalf("套用前只准推進 MaxSeen，得到 %+v", w)
	}

	admission, got := Admit(w, 41)
	if admission.Accepted || admission.Reason != StaleRevision {
		t.Fatalf("revision 43 套用失敗後，41 仍應以 %q 拒絕，得到 %+v", StaleRevision, admission)
	}
	if got != w {
		t.Fatalf("拒絕舊 revision 不得改動水位：原為 %+v，得到 %+v", w, got)
	}
}

// 這支守的是誤把 revision 當冪等鍵：相同 revision 的新 job_id 必須仍可重送。
func TestSameRevisionCanBeAdmittedAgain(t *testing.T) {
	w := Watermarks{MaxSeen: 43, MaxApplied: 43}
	admission, got := Admit(w, 43)
	if !admission.Accepted || admission.Reason != "" {
		t.Fatalf("相同 revision 應被允收，得到 %+v", admission)
	}
	if got != w {
		t.Fatalf("相同 revision 不應改動水位：原為 %+v，得到 %+v", w, got)
	}
}

// 這支守的是把 rollback 寫成 revision 倒退：舊版只能包在更高 revision 中，
// 而任何 Applied 輸入都不得讓 MaxApplied 變小。
func TestRollbackRequiresHigherRevisionAndAppliedNeverDecreases(t *testing.T) {
	w := Watermarks{MaxSeen: 43, MaxApplied: 43}
	if admission, got := Admit(w, 42); admission.Accepted || admission.Reason != StaleRevision || got != w {
		t.Fatalf("較低 revision 的 rollback 應被拒絕且水位不動，允收=%+v，水位=%+v", admission, got)
	}

	admission, w := Admit(w, 44)
	if !admission.Accepted {
		t.Fatalf("包著舊版內容的較高 revision 44 應被允收，得到 %+v", admission)
	}
	w = Applied(w, 44)
	for _, r := range []Revision{43, 1, -1, 44} {
		got := Applied(w, r)
		if got.MaxApplied != 44 {
			t.Fatalf("Applied(%d) 讓 MaxApplied 從 44 倒退成 %d", r, got.MaxApplied)
		}
		w = got
	}
}

// 這支守的是拒絕碼漂移：agent 與 Hub scheduler 的持久化代碼都必須穩定。
func TestRejectionCodesMatchSpec(t *testing.T) {
	cases := []struct {
		name string
		got  RejectionCode
		want string
	}{
		{"舊 revision", StaleRevision, "STALE_REVISION"},
		{"重複 job ID", DuplicateJobID, "DUPLICATE_JOB_ID"},
		{"產物雜湊不符", ArtifactHashMismatch, "ARTIFACT_HASH_MISMATCH"},
		{"租約無效", LeaseInvalid, "LEASE_INVALID"},
		{"不可逆遷移", IrreversibleMigration, "IRREVERSIBLE_MIGRATION"},
		{"前置條件不符", PreconditionFailed, "PRECONDITION_FAILED"},
		{"未指定模型切換", UnspecifiedModelSwitch, "UNSPECIFIED_MODEL_SWITCH"},
		{"上游工作失敗", DependencyFailed, "DEPENDENCY_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if string(tc.got) != tc.want {
				t.Errorf("拒絕碼應為 %q，得到 %q", tc.want, tc.got)
			}
		})
	}
}

// 這支守的是跳過必要階段：主要工作流程必須逐步經過 claimed、running 與 verifying。
func TestMainJobPathAdvancesOneStageAtATime(t *testing.T) {
	steps := []struct {
		from JobState
		ev   Event
		want JobState
	}{
		{NotStarted, Claim, Claimed},
		{Claimed, Start, Running},
		{Running, FinishWork, Verifying},
		{Verifying, VerificationPassed, Succeeded},
	}
	for _, step := range steps {
		got, err := Advance(Job{State: step.from}, step.ev)
		if err != nil || got != step.want {
			t.Errorf("%q 收到 %q 應進入 %q，得到 %q，錯誤=%v", step.from, step.ev, step.want, got, err)
		}
	}
}

// 這支守的是 Agent 自己宣告完成就被算 succeeded：窮舉所有狀態與事件後，
// 唯一能進 succeeded 的組合必須是 verifying + VerificationPassed。
func TestSucceededHasExactlyOneEntrance(t *testing.T) {
	states := []JobState{NotStarted, Claimed, Running, Verifying, Succeeded, Failed, Rejected, LeaseExpired, ManualIntervention}
	events := []Event{Claim, Start, FinishWork, VerificationPassed, VerificationFailed, Reject, LeaseLost, Timeout, Event("未知事件")}
	entrances := 0
	for _, state := range states {
		for _, event := range events {
			got, err := Advance(Job{State: state}, event)
			if got != Succeeded || err != nil {
				continue
			}
			entrances++
			if state != Verifying || event != VerificationPassed {
				t.Errorf("不該由 %q + %q 進入 succeeded", state, event)
			}
		}
	}
	if entrances != 1 {
		t.Fatalf("succeeded 應恰有一個入口，得到 %d 個", entrances)
	}
}

// 這支守的是終態回放又改寫結果：每個終態收到任何事件都必須回錯且保持原狀。
func TestTerminalStatesRejectEveryEventWithoutChanging(t *testing.T) {
	terminals := []JobState{Succeeded, Failed, Rejected, LeaseExpired, ManualIntervention}
	events := []Event{Claim, Start, FinishWork, VerificationPassed, VerificationFailed, Reject, LeaseLost, Timeout, Event("未知事件")}
	for _, terminal := range terminals {
		if !IsTerminal(terminal) {
			t.Errorf("%q 應為終態", terminal)
		}
		for _, event := range events {
			got, err := Advance(Job{State: terminal}, event)
			if err == nil {
				t.Errorf("終態 %q 收到 %q 應回錯", terminal, event)
			}
			if got != terminal {
				t.Errorf("終態 %q 收到 %q 後不得變成 %q", terminal, event, got)
			}
		}
	}
	for _, active := range []JobState{NotStarted, Claimed, Running, Verifying} {
		if IsTerminal(active) {
			t.Errorf("進行中狀態 %q 不應被判成終態", active)
		}
	}
}

// 這支守的是把業務拒絕、租約遺失或逾時混成一般流程事件，導致工作單留在進行中。
func TestExceptionalEventsReachTheirDistinctTerminalStates(t *testing.T) {
	cases := []struct {
		from JobState
		ev   Event
		want JobState
	}{
		{NotStarted, Reject, Rejected},
		{Claimed, Reject, Rejected},
		{Running, LeaseLost, LeaseExpired},
		{Verifying, Timeout, Failed},
		{Verifying, VerificationFailed, Failed},
	}
	for _, tc := range cases {
		got, err := Advance(Job{State: tc.from}, tc.ev)
		if err != nil || got != tc.want {
			t.Errorf("%q 收到 %q 應進入 %q，得到 %q，錯誤=%v", tc.from, tc.ev, tc.want, got, err)
		}
	}
}

// 這支守的是把不可逆失敗說成已回退：failed 與 manual_intervention 必須分開，
// 前者代表可逆變更已回退，後者代表需要人工處理，兩者絕不能互換。
func TestIrreversibleFailureRequiresManualIntervention(t *testing.T) {
	if got := OnFailure(false); got != Failed {
		t.Fatalf("可逆失敗應進入 failed，得到 %q", got)
	}
	if got := OnFailure(true); got != ManualIntervention {
		t.Fatalf("不可逆失敗應進入 manual_intervention，得到 %q", got)
	}
	if Failed == ManualIntervention {
		t.Fatal("failed 與 manual_intervention 不得是同一狀態")
	}
}

// 這支守的是 OnFailure 沒接進狀態機：Advance 自己直接寫 Failed，
// 於是一個不可逆的工作單失敗之後，畫面上說它「已經回到舊版」——
// 而那台機器其實停在中間，沒有人會去看它。
// PHASES.md Phase 4 完成判準第 6 條就是這一條。
func TestIrreversibleFailurePathsNeverClaimRollback(t *testing.T) {
	for _, ev := range []Event{VerificationFailed, Timeout} {
		for _, from := range []JobState{Claimed, Running, Verifying} {
			j := Job{State: from, Irreversible: true}
			got, err := Advance(j, ev)
			if err != nil {
				continue // 這個組合本來就不合法，不是這支要管的
			}
			if got != ManualIntervention {
				t.Errorf("不可逆的 %s 收到 %s → %s，應該是 %s（%s 的意思是已經回退了）",
					from, ev, got, ManualIntervention, Failed)
			}
		}
	}
}

// 這支是上面那支的反面：可逆的工作單失敗時**必須**說成 failed，
// 不准為了保險一律報 manual_intervention —— 那會讓每一次普通的失敗
// 都長得像需要人進去救，於是真的需要人的那次沒有人理。
func TestReversibleFailureStaysFailed(t *testing.T) {
	for _, ev := range []Event{VerificationFailed, Timeout} {
		for _, from := range []JobState{Claimed, Running, Verifying} {
			got, err := Advance(Job{State: from}, ev)
			if err != nil {
				continue
			}
			if got != Failed {
				t.Errorf("可逆的 %s 收到 %s → %s，應該是 %s", from, ev, got, Failed)
			}
		}
	}
}

func TestAllJobStatesIsCanonicalAndExhaustive(t *testing.T) {
	want := []JobState{
		NotStarted, Claimed, Running, Verifying, Succeeded,
		Failed, Rejected, LeaseExpired, ManualIntervention,
	}
	if len(AllJobStates) != len(want) {
		t.Fatalf("AllJobStates len=%d want=%d", len(AllJobStates), len(want))
	}
	seen := make(map[JobState]bool, len(AllJobStates))
	for i, got := range AllJobStates {
		if got != want[i] {
			t.Errorf("AllJobStates[%d]=%q want=%q", i, got, want[i])
		}
		if seen[got] {
			t.Errorf("AllJobStates contains duplicate %q", got)
		}
		seen[got] = true
		if !IsKnownJobState(got) {
			t.Errorf("IsKnownJobState rejected canonical state %q", got)
		}
	}
	if IsKnownJobState(JobState("future_or_corrupt")) {
		t.Fatal("IsKnownJobState accepted an unknown state")
	}
}
