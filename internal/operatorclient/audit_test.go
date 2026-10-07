package operatorclient

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func auditClientTestResult() operator.AuditListResult {
	evaluatedAt := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	at := evaluatedAt.Add(-time.Minute)
	machineID := "machine-1"
	reason := "planned"
	key := "request-1"
	digest := "sha256:digest"
	owner := "owner@example.com"
	principal := "tailscale-user:42"
	capability := "example.com/cap/clawctl-admin"
	sourceKind := "operator-api"
	detail := store.OperatorIdempotencyReplayPrefix + "same decision"
	outcome := store.AuditOutcomeOK
	return operator.AuditListResult{
		SchemaVersion: operator.AuditReadSchemaVersion, Consistency: operator.AuditReadConsistency,
		EvaluatedAt: evaluatedAt, CreationCeiling: 7, MatchedTotal: 1, Total: 1, Succeeded: 1,
		Denials: operator.AuditDenials{Mode: operator.AuditDenialsSampled},
		Items: []operator.AuditEvent{{
			AuditID: 7, At: &at, Action: string(store.AuditMachineChannel), Outcome: &outcome,
			MachineID: &machineID, Subject: "samplehub1", Reason: &reason,
			IdempotencyKey: &key, RequestDigest: &digest, SourceAddr: "100.64.0.7",
			WhoUser: &owner, AuthSubject: &principal, AuthCapability: &capability,
			SourceKind: &sourceKind, Detail: &detail, Replayed: true,
			Issues: []string{}, AlteredFields: []string{},
		}},
	}
}

func TestAuditClientAcceptsCanonicalFilteredRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactClientTestHeaders(w)
		query := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/v1/operator/audit-events" ||
			query.Get("machine_id") != "machine-1" || query.Get("action") != "machine-channel" ||
			query.Get("outcome") != "ok" || query.Get("principal") != "tailscale-user:42" ||
			query.Get("capability") != "example.com/cap/clawctl-admin" ||
			query.Get("source_kind") != "operator-api" || query.Get("correlation") != "request-1" ||
			query.Get("from") != "2026-09-08T19:00:00Z" || query.Get("to") != "2026-09-08T21:00:00Z" ||
			query.Get("denials") != "sampled" || query.Get("limit") != "10" ||
			r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("request method=%s path=%s query=%v headers=%v", r.Method, r.URL.Path, query, r.Header)
		}
		_ = json.NewEncoder(w).Encode(auditClientTestResult())
	}))
	defer server.Close()
	from := time.Date(2026, 9, 8, 15, 0, 0, 0, time.FixedZone("EDT", -4*60*60))
	to := from.Add(2 * time.Hour)
	result, err := operatorClientForServer(t, server).AuditEvents(t.Context(), operator.AuditListRequest{
		MachineID: "machine-1", Actions: []store.AuditAction{store.AuditMachineChannel},
		Outcome: store.AuditOutcomeOK, Principal: "tailscale-user:42",
		Capability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
		Correlation: "request-1", From: &from, To: &to,
		Denials: operator.AuditDenialsSampled, Limit: 10,
	})
	if err != nil || len(result.Items) != 1 || !result.Items[0].Succeeded() || !result.Items[0].Replayed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAuditClientRejectsContradictoryAndPrivateServerEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*operator.AuditListResult)
		field  string
	}{
		{"wrong consistency", func(v *operator.AuditListResult) { v.Consistency = "snapshot" }, ""},
		{"total mismatch", func(v *operator.AuditListResult) { v.Total++ }, ""},
		{"denial omission mismatch", func(v *operator.AuditListResult) { v.Denials.Omitted = 1 }, ""},
		{"denial item missing from aggregate", func(v *operator.AuditListResult) {
			v.Items[0].Action = string(store.AuditOperatorDenied)
			v.Items[0].Detail = nil
			v.Items[0].Replayed = false
		}, ""},
		{"duplicate writer id", func(v *operator.AuditListResult) {
			v.Items = append(v.Items, v.Items[0])
			v.Total = 2
			v.MatchedTotal = 2
			v.Succeeded = 2
		}, ""},
		{"missing invalid-at issue", func(v *operator.AuditListResult) { v.Items[0].At = nil }, ""},
		{"unknown outcome without issue", func(v *operator.AuditListResult) { v.Items[0].Outcome = nil; v.Succeeded = 0; v.UnknownOutcome = 1 }, ""},
		// 這三筆補上同一條五項雙條件中原本欠缺獨有見證的 empty_subject、
		// empty_source_addr 與 unknown_action；invalid_at 和 unknown_outcome 已由
		// 上面兩筆守住，五項至此每一項都有自己的見證。
		//
		// 這些空白不是假想形狀：empty_subject 現網天天由
		// rejectOperatorEnrollTokenTransport、rejectOperatorPolicyTransport 與
		// rejectOperatorVerifierTransport 產生。三個端點刻意不採信遭拒 request body
		// 裡的名字，寫出 subject 為空的稽核列；store 端的 audit_read.go:440-442
		// 會蓋上 empty_subject，並由
		// TestAnAuditRowWrittenWithoutASubjectIsMarkedUnreadable 守住。這裡守同一規矩
		// 的另一端：hub 若交出空白卻沒有 stamp，client 必須拒收整頁，不能把無法
		// 歸因的紀錄當成正常列畫給操作員。
		//
		// 每筆刻意只改一個欄位，而且確認只會踩到自己那一項：audit.go:287 與
		// audit.go:290 呼叫 validateAuditResponseText 驗證 subject、source_addr 時，
		// requireNonEmpty 都是 false，所以空字串不會提早被擋；AlteredFields 也未
		// 要求空欄位必須列名。因此失敗必定來自 audit.go:322 或 audit.go:323。
		// 反之，不能順手把 Issues 補成對應字串；那會讓配對成立，client 將收下它，
		// 測試便什麼也釘不住。
		//
		// 這一筆補上同一條五項雙條件裡最後一項沒有獨有見證的
		// unknown_action。五項裡 invalid_at、unknown_outcome 原本就有見證，
		// empty_subject、empty_source_addr 由上面兩筆補上，這一筆補完最後一項。
		//
		// 兩行突變缺一不可：Action 讓 knownAction 變成 false；Replayed = false
		// 則是因為 audit.go:342-347 的 replay 分類吃的是
		// IsCanonicalOperatorAction。只改 Action 會被 replay 那條先擋掉，量到的
		// 就不是這一項；同表的 noncanonical replay asserted 正是 replay 那條的
		// 見證。兩行合起來也是 store 真的會交出來的形狀：
		// audit_read.go:152-155 的 IsOperatorReplay() 用同一條公式，不是為了繞過
		// 檢查而拼湊的。
		//
		// 這一項是五項裡唯一真的出過事的：internal/store/audit_read.go:31-35 的
		// ⚠ 註解記著 verification-assign 曾經漏在 allAuditActions 外面，live
		// ledger 裡每一筆派工列都帶著 unknown_action。所以「action 未知」是合法
		// 且發生過的狀態，client 必須收得下；這一筆守的只是「hub 交出未知 action
		// 卻不 stamp」這個方向。「一致時要收下」那個方向這張表放不下（表的斷言
		// 是 err == nil 就 Fatal），另外一支測試處理。
		{"empty subject without issue", func(v *operator.AuditListResult) { v.Items[0].Subject = "" }, ""},
		{"empty source_addr without issue", func(v *operator.AuditListResult) { v.Items[0].SourceAddr = "" }, ""},
		{"unknown action without issue", func(v *operator.AuditListResult) {
			v.Items[0].Action = "future-action"
			v.Items[0].Replayed = false
		}, ""},
		{"replay without marker", func(v *operator.AuditListResult) { value := "ordinary"; v.Items[0].Detail = &value }, ""},
		{"canonical replay marker omitted classification", func(v *operator.AuditListResult) { v.Items[0].Replayed = false }, ""},
		{"noncanonical replay asserted", func(v *operator.AuditListResult) {
			v.Items[0].Action = string(store.AuditRetire)
		}, ""},
		{"transport marker omitted classification", func(v *operator.AuditListResult) {
			value := store.OperatorTransportRejectionPrefix + "bad bytes"
			v.Items[0].Detail = &value
			v.Items[0].Replayed = false
		}, ""},
		{"control character in safe response", func(v *operator.AuditListResult) { v.Items[0].Subject = "line1\nline2" }, ""},
		{"aggregate integer overflow", func(v *operator.AuditListResult) {
			maximum := int(^uint(0) >> 1)
			v.Items = []operator.AuditEvent{}
			v.MatchedTotal, v.Total = 0, 0
			v.Succeeded, v.Failed, v.UnknownOutcome = maximum, maximum, 2
		}, ""},
		{"machine filter mismatch", func(v *operator.AuditListResult) { value := "machine-2"; v.Items[0].MachineID = &value }, ""},
		{"private unknown field", nil, `,"raw_store_entry":{"secret":"no"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := auditClientTestResult()
			if test.mutate != nil {
				test.mutate(&value)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if test.field != "" {
				raw = []byte(strings.TrimSuffix(string(raw), "}") + test.field + "}")
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				artifactClientTestHeaders(w)
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			request := operator.AuditListRequest{MachineID: "machine-1"}
			if got, err := operatorClientForServer(t, server).AuditEvents(t.Context(), request); err == nil {
				t.Fatalf("accepted contradictory result: %+v", got)
			}
		})
	}
}

func TestAuditClientRejectsMalformedTimestampUnderTimeFilter(t *testing.T) {
	value := auditClientTestResult()
	value.Items[0].At = nil
	value.Items[0].Issues = []string{"invalid_at"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		artifactClientTestHeaders(w)
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer server.Close()
	from := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	if _, err := operatorClientForServer(t, server).AuditEvents(t.Context(), operator.AuditListRequest{
		MachineID: "machine-1", From: &from,
	}); err == nil {
		t.Fatal("audit client accepted malformed timestamp under a time filter")
	}
}

func TestAuditClientRejectsAggregatesContradictingExplicitFilters(t *testing.T) {
	tests := []struct {
		name    string
		request operator.AuditListRequest
		result  operator.AuditListResult
	}{
		{
			name:    "excluded denial action",
			request: operator.AuditListRequest{Actions: []store.AuditAction{store.AuditConnect}},
			result: func() operator.AuditListResult {
				value := auditClientTestResult()
				value.Items[0].Action = string(store.AuditConnect)
				value.Items[0].Detail = nil
				value.Items[0].Replayed = false
				value.MatchedTotal = 2
				value.Denials = operator.AuditDenials{
					Mode: operator.AuditDenialsSampled, Matched: 1, Omitted: 1,
				}
				return value
			}(),
		},
		{
			name:    "denial-only action",
			request: operator.AuditListRequest{Actions: []store.AuditAction{store.AuditOperatorDenied}, Limit: 1},
			result: func() operator.AuditListResult {
				value := auditClientTestResult()
				value.MatchedTotal = 2
				value.Items = []operator.AuditEvent{}
				value.Denials = operator.AuditDenials{
					Mode: operator.AuditDenialsSampled, Matched: 1, Omitted: 1,
				}
				return value
			}(),
		},
		{
			name:    "ok outcome",
			request: operator.AuditListRequest{Outcome: store.AuditOutcomeOK, Limit: 1},
			result: func() operator.AuditListResult {
				value := auditClientTestResult()
				value.Items = []operator.AuditEvent{}
				value.Succeeded = 0
				value.Failed = 1
				return value
			}(),
		},
		{
			name:    "failed outcome",
			request: operator.AuditListRequest{Outcome: store.AuditOutcomeFailed, Limit: 1},
			result: func() operator.AuditListResult {
				value := auditClientTestResult()
				value.Items = []operator.AuditEvent{}
				return value
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.result.Items) == 0 {
				cursor := auditClientCursor{
					Version: operator.AuditReadSchemaVersion,
					FilterDigest: auditClientFilterDigest(
						test.request, operator.AuditDenialsSampled),
					CreationCeiling: test.result.CreationCeiling,
					AfterAuditID:    test.result.CreationCeiling,
				}
				raw, err := json.Marshal(cursor)
				if err != nil {
					t.Fatal(err)
				}
				test.request.Cursor = base64.RawURLEncoding.EncodeToString(raw)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				artifactClientTestHeaders(w)
				_ = json.NewEncoder(w).Encode(test.result)
			}))
			defer server.Close()
			if got, err := operatorClientForServer(t, server).AuditEvents(t.Context(), test.request); err == nil {
				t.Fatalf("accepted aggregates contradicting explicit filters: %+v", got)
			}
		})
	}
}

func TestAuditClientAcceptsMaximumLegalPageLargerThanGeneralResponseLimit(t *testing.T) {
	value := auditClientTestResult()
	value.CreationCeiling = operator.MaxAuditReadLimit
	value.MatchedTotal = operator.MaxAuditReadLimit
	value.Total = operator.MaxAuditReadLimit
	value.Succeeded = operator.MaxAuditReadLimit
	value.Items = make([]operator.AuditEvent, 0, operator.MaxAuditReadLimit)
	at := value.EvaluatedAt.Add(-time.Minute)
	outcome := store.AuditOutcomeOK
	large := strings.Repeat(`\`, 2048)
	medium := strings.Repeat(`\`, 512)
	for i := range operator.MaxAuditReadLimit {
		value.Items = append(value.Items, operator.AuditEvent{
			AuditID: int64(operator.MaxAuditReadLimit - i), At: &at,
			Action: string(store.AuditConnect), Outcome: &outcome,
			Subject: large, Reason: &large, SourceAddr: medium, Detail: &large,
			Issues: []string{}, AlteredFields: []string{},
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		artifactClientTestHeaders(w)
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(value)
	}))
	defer server.Close()
	result, err := operatorClientForServer(t, server).AuditEvents(t.Context(), operator.AuditListRequest{
		Limit: operator.MaxAuditReadLimit,
	})
	if err != nil || len(result.Items) != operator.MaxAuditReadLimit {
		t.Fatalf("maximum audit page items=%d err=%v", len(result.Items), err)
	}
}

func TestAuditClientKeepsDedicatedResponseLimitBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		artifactClientTestHeaders(w)
		_, _ = w.Write([]byte(strings.Repeat(" ", int(maxAuditResponseBytes)+1)))
	}))
	defer server.Close()
	if _, err := operatorClientForServer(t, server).AuditEvents(
		t.Context(), operator.AuditListRequest{},
	); err == nil || !strings.Contains(err.Error(), "exceeds 4 MiB") {
		t.Fatalf("audit client did not enforce dedicated response bound: %v", err)
	}
}

func TestAuditClientRejectsInputsBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	now := time.Now().UTC().Truncate(time.Second)
	later := now.Add(time.Hour)
	for _, request := range []operator.AuditListRequest{
		{MachineID: " bad"}, {Principal: "\n"}, {Actions: []store.AuditAction{"unknown"}},
		{Actions: []store.AuditAction{store.AuditConnect, store.AuditConnect}}, {Outcome: "maybe"},
		{Denials: "maybe"}, {Limit: 101}, {Cursor: " bad"}, {From: &later, To: &now},
	} {
		if _, err := client.AuditEvents(t.Context(), request); err == nil {
			t.Errorf("accepted request %+v", request)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid audit requests reached network %d times", hits.Load())
	}
}

func TestAuditClientRejectsUnsafeResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		artifactClientTestHeaders(w)
		w.Header().Set("ETag", `"7"`)
		_ = json.NewEncoder(w).Encode(auditClientTestResult())
	}))
	defer server.Close()
	if _, err := operatorClientForServer(t, server).AuditEvents(t.Context(), operator.AuditListRequest{}); err == nil {
		t.Fatal("audit client accepted composite ETag")
	}
}

func TestAuditClientAcceptsRealCursorAndRejectsAnchorOrCeilingContradictions(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := range 3 {
		if err := st.RecordAudit(store.AuditEntry{
			Action: store.AuditConnect, Subject: "cursor-row-" + string(rune('0'+i)),
			SourceAddr: "local-test", OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	svc := operator.New(st)
	evaluatedAt := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactClientTestHeaders(w)
		result, err := svc.ListAudit(operator.AuditListRequest{
			Denials: operator.AuditDenialsAll, Limit: 1, Cursor: r.URL.Query().Get("cursor"),
		}, evaluatedAt)
		if err != nil {
			t.Errorf("serve audit cursor: %v", err)
			http.Error(w, "failed", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	client := operatorClientForServer(t, server)
	request := operator.AuditListRequest{Denials: operator.AuditDenialsAll, Limit: 1}
	first, err := client.AuditEvents(t.Context(), request)
	if err != nil || first.NextCursor == nil || first.NextAfterAuditID == nil {
		server.Close()
		t.Fatalf("first cursor page=%+v err=%v", first, err)
	}
	request.Cursor = *first.NextCursor
	second, err := client.AuditEvents(t.Context(), request)
	server.Close()
	if err != nil || len(second.Items) != 1 || second.Items[0].AuditID >= *first.NextAfterAuditID ||
		second.CreationCeiling != first.CreationCeiling {
		t.Fatalf("second cursor page=%+v err=%v", second, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*operator.AuditListResult)
	}{
		{"anchor", func(result *operator.AuditListResult) { *result.NextAfterAuditID++ }},
		{"ceiling", func(result *operator.AuditListResult) { result.CreationCeiling++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := svc.ListAudit(operator.AuditListRequest{Denials: operator.AuditDenialsAll, Limit: 1}, evaluatedAt)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&value)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				artifactClientTestHeaders(w)
				_ = json.NewEncoder(w).Encode(value)
			}))
			defer server.Close()
			if _, err := operatorClientForServer(t, server).AuditEvents(t.Context(), operator.AuditListRequest{
				Denials: operator.AuditDenialsAll, Limit: 1,
			}); err == nil {
				t.Fatal("client accepted contradictory cursor metadata")
			}
		})
	}
}

func TestAuditClientCanonicalMutationClassificationStaysComplete(t *testing.T) {
	for _, action := range store.AuditActions() {
		t.Run(string(action), func(t *testing.T) {
			result := auditClientTestResult()
			result.Items[0].Action = string(action)
			result.Items[0].Replayed = store.IsCanonicalOperatorAction(action)
			if err := readAuditClientTestResult(t, result); err != nil {
				t.Fatalf("client rejected store classification: %v", err)
			}

			result.Items[0].Replayed = !result.Items[0].Replayed
			if err := readAuditClientTestResult(t, result); err == nil {
				t.Fatal("client accepted replay classification contrary to store catalog")
			}
		})
	}
}

// 這支測試守 unknown_action 雙條件的另一個方向，和拒收表裡的
// unknown action without issue 成對：未知 action 有 stamp 時必須收下。拒收表的
// 斷言是 err == nil 就 Fatal，放不下這個方向。
//
// internal/store/audit_read.go:31-35 的 ⚠ 註解記著 verification-assign 曾漏在
// allAuditActions 外，當時 live ledger 的每筆派工列都帶著 unknown_action。未知
// action 因而是合法且實際發生過的狀態，client 不能讓操作員看不到那些派工列。
//
// 誠實的限制是：若把 audit.go:322 那一項焊成 false，這支仍會綠，因為配對成立時
// 該子句本來就不會開火。它守的不是「那一項存在」，而是「那一項未被收緊成單向
// 守衛」；要看到它紅，須把 issues["unknown_action"] != !knownAction 改成
// !knownAction。這和拒收表那筆守的不是同一件事。
//
// 三行突變缺一不可：Action = "future-action" 使用 repo 既有且不在 catalog 的未知
// action；Issues 的單一 unknown_action 在 allowlist 內、不重複，並避免 nil 被 null
// evidence array 檢查先擋下；Replayed = false 則符合非 canonical action 的 replay
// 分類。fixture 預設為 true，不拉回 false 會先紅在 replay 檢查；store 的
// IsOperatorReplay() 也用同一條公式，這正是 store 會交出的形狀。
func TestAuditClientAcceptsAnActionTheCatalogCannotNameWhenItSaysSo(t *testing.T) {
	result := auditClientTestResult()
	result.Items[0].Action = "future-action"
	result.Items[0].Replayed = false
	result.Items[0].Issues = []string{"unknown_action"}
	if err := readAuditClientTestResult(t, result); err != nil {
		t.Fatalf("client 目前拒收已誠實標記 unknown_action 的未知 action，導致操作員看不到合法稽核列：%v", err)
	}
}

func TestAuditClientReadsPagesContainingReplayedPolicyAndDeviceSyncRows(t *testing.T) {
	for _, action := range []store.AuditAction{
		store.AuditDeviceSync,
		store.AuditVerificationAssign,
		store.AuditSettingPolicy,
		store.AuditSettingAssign,
		store.AuditCompliancePolicy,
		store.AuditComplianceAssign,
	} {
		t.Run(string(action), func(t *testing.T) {
			result := auditClientTestResult()
			result.Items[0].Action = string(action)
			if err := readAuditClientTestResult(t, result); err != nil {
				t.Fatalf("client rejected replayed %s row: %v", action, err)
			}
		})
	}

	t.Run("connect lookalike remains rejected", func(t *testing.T) {
		result := auditClientTestResult()
		result.Items[0].Action = string(store.AuditConnect)
		if err := readAuditClientTestResult(t, result); err == nil {
			t.Fatal("client accepted replay asserted for non-canonical connect row")
		}
	})
}

func readAuditClientTestResult(t *testing.T, result operator.AuditListResult) error {
	t.Helper()
	if len(result.Items) == 1 && result.Items[0].Action == string(store.AuditOperatorDenied) {
		result.Denials.Matched = 1
		result.Denials.Included = 1
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		artifactClientTestHeaders(w)
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()
	_, err := operatorClientForServer(t, server).AuditEvents(t.Context(), operator.AuditListRequest{
		Denials: operator.AuditDenialsSampled,
		Limit:   100,
	})
	return err
}
