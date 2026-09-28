package store

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
	"time"
)

func TestAuditReadSamplingAndCreationCeilingTraversal(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	if err := st.RecordAudit(AuditEntry{
		Action: AuditRetire, Subject: "domain-row", SourceAddr: "local-test", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 60 {
		if err := st.RecordAudit(AuditEntry{
			Action: AuditOperatorDenied, Subject: fmt.Sprintf("denial-%02d", i),
			SourceAddr: "100.64.0.7", OK: false,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.ListAuditReads(AuditReadFilter{Limit: 30, SampleOperatorDenials: 50})
	if err != nil {
		t.Fatal(err)
	}
	if first.MatchedTotal != 61 || first.Total != 51 || first.OperatorDenialsTotal != 60 ||
		first.OperatorDenialsIncluded != 50 || first.Succeeded != 1 || first.Failed != 50 ||
		len(first.Items) != 30 || first.Next == nil {
		t.Fatalf("first page=%+v items=%d", first, len(first.Items))
	}
	if first.Items[0].Subject != "denial-59" || first.Items[29].Subject != "denial-30" {
		t.Fatalf("first writer-order page=%q..%q", first.Items[0].Subject, first.Items[29].Subject)
	}

	// A new row cannot move the continuation or change its frozen totals.
	st.nowFn = func() time.Time { return now.Add(time.Minute) }
	if err := st.RecordAudit(AuditEntry{
		Action: AuditConnect, Subject: "new-after-first-page", SourceAddr: "local-test", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := st.ListAuditReads(AuditReadFilter{
		Limit: 30, SampleOperatorDenials: 50,
		CreationCeiling: &first.CreationCeiling, After: first.Next,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.MatchedTotal != 61 || second.Total != 51 || len(second.Items) != 21 || second.Next != nil {
		t.Fatalf("second page=%+v items=%d", second, len(second.Items))
	}
	if second.Items[0].Subject != "denial-29" || second.Items[len(second.Items)-1].Subject != "domain-row" {
		t.Fatalf("second writer-order page=%q..%q", second.Items[0].Subject, second.Items[len(second.Items)-1].Subject)
	}
	for _, item := range second.Items {
		if item.Subject == "new-after-first-page" {
			t.Fatal("new audit row entered a frozen cursor traversal")
		}
	}
}

func TestAuditReadExactFiltersAndAllDenials(t *testing.T) {
	st := newTestStore(t)
	from := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	st.nowFn = func() time.Time { return from.Add(30 * time.Minute) }
	want := AuditEntry{
		Action: AuditMachineChannel, MachineID: "machine-1", Subject: "samplehub1", Reason: "rollout",
		IdempotencyKey: "correlation-1", RequestDigest: "sha256:digest",
		SourceAddr: "100.64.0.7", WhoUser: "owner@example.com",
		AuthSubject: "tailscale-user:42", AuthCapability: "example.com/cap/clawctl-admin",
		SourceKind: "operator-api", OK: false,
	}
	if err := st.RecordAudit(want); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordAudit(AuditEntry{
		Action: AuditConnect, MachineID: "machine-2", Subject: "other", SourceAddr: "local", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	page, err := st.ListAuditReads(AuditReadFilter{
		MachineID: "machine-1", Actions: []AuditAction{AuditMachineChannel},
		Outcome: AuditOutcomeFailed, Principal: "tailscale-user:42",
		Capability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
		Correlation: "correlation-1", From: &from, To: &to, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.MatchedTotal != 1 || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("filtered page=%+v", page)
	}
	got := page.Items[0]
	if got.MachineID != want.MachineID || got.Action != string(want.Action) || got.Outcome == nil ||
		*got.Outcome != AuditOutcomeFailed || got.AuthSubject != want.AuthSubject || got.Issues == nil {
		t.Fatalf("filtered item=%+v", got)
	}

	// An explicit unsampled read can traverse all persisted denials.
	if err := st.RecordAudit(AuditEntry{
		Action: AuditOperatorDenied, Subject: "denial", SourceAddr: "100.64.0.8", OK: false,
	}); err != nil {
		t.Fatal(err)
	}
	all, err := st.ListAuditReads(AuditReadFilter{
		Actions: []AuditAction{AuditOperatorDenied}, Limit: 10, SampleOperatorDenials: 0,
	})
	if err != nil || all.Total != 1 || all.OperatorDenialsIncluded != 1 || all.OperatorDenialsTotal != 1 {
		t.Fatalf("all denials=%+v err=%v", all, err)
	}
}

func TestAuditReadPreservesMalformedLegacyClassification(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.DB().Exec(`INSERT INTO audit_log
 (at,action,subject,source_addr,outcome) VALUES (?,?,?,?,?)`,
		"not-a-time", "future-action", "legacy-corrupt", "", "maybe"); err != nil {
		t.Fatal(err)
	}
	page, err := st.ListAuditReads(AuditReadFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.UnknownOutcome != 1 {
		t.Fatalf("malformed page=%+v", page)
	}
	item := page.Items[0]
	if item.At != nil || item.Outcome != nil || item.Issues == nil || len(item.Issues) != 4 {
		t.Fatalf("malformed row was silently normalized: %+v", item)
	}
	wantIssues := []string{"invalid_at", "unknown_action", "unknown_outcome", "empty_source_addr"}
	for _, want := range wantIssues {
		found := false
		for _, issue := range item.Issues {
			found = found || issue == want
		}
		if !found {
			t.Errorf("missing issue %q in %v", want, item.Issues)
		}
	}
}

// 空的 subject 是現網狀態而非壞資料：rejectOperatorEnrollTokenTransport、
// rejectOperatorPolicyTransport 與 rejectOperatorVerifierTransport 都刻意不採信遭拒
// request body 裡的 display_name、policy_id 或 scope_id。empty_subject 是帳本對此唯一的
// 結構化標記；console 的「對象無法判讀」另有測試守護，即使拿掉標記仍會顯示，但
// ledger issues 會漏報已發生的事，且 operatorclient 以
// issues["empty_subject"] != (item.Subject == "") 配對時會拒收整頁。實測短路
// audit_read.go:440-442 後全樹仍全綠，隔壁 empty_source_addr 同樣短路則有四支測試變紅，
// 因此這一格原本沒有守護。第二列是有 subject 的對照，證明標記有條件；無 subject
// 那列刻意要求剛好一項 issue，也同時證明它沒有觸發其他資料問題。
// 後續以每次只短路一格重跑全樹實測：短路 audit_read.go:440-442 後只有這支紅、
// others=[]，它是 empty_subject 唯一的看守者；短路 audit_read.go:443-445 則這支綠、
// 仍是既有那四支紅，沒有互搶，而短路 audit.go:213-216 寫入端補 source_addr 後
// 這支與全樹都綠，那一格仍刻意不補，因為現網寫入端送不出空的 source_addr。
// 把 AuditEnrollToken 從 allAuditActions 拿掉時，這支也會和
// TestEveryDeclaredAuditActionIsReadable、
// TestEnrollmentRefusedByTheLimitIsAuditedWithItsCode 一起紅；這不是它的守備範圍，
// 而是 issues 剛好一項／剛好零項的斷言形狀必然會在這兩列多踩到任何其他 issue 時
// 失敗，例如 action 被移出值域就會多一個 unknown_action。
// 這個副作用正是此斷言形狀想要的：它證明 empty_subject 是被那一格蓋上，不是順便
// 被別的東西蓋到；但它不是這支測試的隔離證據，allAuditActions 另有專屬看守者，
// 下一個人若因改動值域看到這支紅，該修的是值域那一邊。
func TestAnAuditRowWrittenWithoutASubjectIsMarkedUnreadable(t *testing.T) {
	st := newTestStore(t)
	if err := st.RecordAudit(AuditEntry{
		Action: AuditEnrollToken, Subject: "", SourceAddr: "100.64.0.7", OK: false,
	}); err != nil {
		t.Fatalf("寫入刻意沒有 subject 的稽核列失敗，無法驗證現網寫入路徑：%v", err)
	}
	if err := st.RecordAudit(AuditEntry{
		Action: AuditEnrollToken, Subject: "samplehub1", SourceAddr: "100.64.0.7", OK: false,
	}); err != nil {
		t.Fatalf("寫入有 subject 的對照稽核列失敗，無法驗證標記是否有條件：%v", err)
	}

	page, err := st.ListAuditReads(AuditReadFilter{Limit: 10})
	if err != nil {
		t.Fatalf("讀取稽核列失敗，無法檢查 subject 的結構化標記：%v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("目前讀到 %d 列，預期剛好 2 列；資料不足會讓 subject 標記驗證空轉", len(page.Items))
	}

	emptySubjectFound := false
	nonEmptySubjectFound := false
	for _, item := range page.Items {
		if item.Subject == "" {
			emptySubjectFound = true
			if len(item.Issues) != 1 || item.Issues[0] != "empty_subject" {
				t.Fatalf("沒有 subject 的現網稽核列目前 issues=%v，預期只能有 empty_subject，否則帳本未正確描述唯一的資料狀態", item.Issues)
			}
			continue
		}

		nonEmptySubjectFound = true
		if len(item.Issues) != 0 {
			t.Fatalf("有 subject 的對照稽核列目前 issues=%v，預期沒有 issue；empty_subject 不應無條件套用", item.Issues)
		}
	}
	if !emptySubjectFound {
		t.Fatal("目前兩列中找不到沒有 subject 的稽核列，測試沒有驗到現網刻意留空的狀態")
	}
	if !nonEmptySubjectFound {
		t.Fatal("目前兩列中找不到有 subject 的對照列，無法證明 empty_subject 標記是有條件的")
	}
}

func TestAuditReadRejectsInvalidFilters(t *testing.T) {
	st := newTestStore(t)
	ceiling := int64(1)
	for _, filter := range []AuditReadFilter{
		{Limit: 0},
		{Limit: MaxAuditReadPageSize + 1},
		{Limit: 10, Actions: []AuditAction{"unknown"}},
		{Limit: 10, Actions: []AuditAction{AuditConnect, AuditConnect}},
		{Limit: 10, Outcome: "maybe"},
		{Limit: 10, Principal: " bad"},
		{Limit: 10, Principal: string([]byte{'x', 0xff})},
		{Limit: 10, After: &AuditReadPosition{AuditID: 1}},
		{Limit: 10, CreationCeiling: &ceiling, After: &AuditReadPosition{AuditID: 2}},
	} {
		if _, err := st.ListAuditReads(filter); !errors.Is(err, ErrInvalidAuditRead) {
			t.Errorf("filter=%+v err=%v", filter, err)
		}
	}
}

func TestAuditReadTimeWindowComparesParsedInstants(t *testing.T) {
	st := newTestStore(t)
	for _, row := range []struct{ at, subject string }{
		{"2026-09-08T12:00:00Z", "canonical-endpoint"},
		{"2026-09-08T12:00:00.500000001Z", "fractional-after-endpoint"},
		{"2026-09-08T08:30:00-04:00", "offset-at-1230-utc"},
		{"2026-09-08 12:30:00", "legacy-at-1230-utc"},
		{"not-a-time", "invalid-time"},
	} {
		if _, err := st.DB().Exec(`INSERT INTO audit_log
 (at,action,subject,source_addr,outcome) VALUES (?,?,?,?,?)`,
			row.at, string(AuditConnect), row.subject, "local-test", "ok"); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	page, err := st.ListAuditReads(AuditReadFilter{To: &endpoint, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.MatchedTotal != 1 || len(page.Items) != 1 || page.Items[0].Subject != "canonical-endpoint" {
		t.Fatalf("exact to endpoint admitted a later fraction or invalid time: %+v", page)
	}
	at1230 := endpoint.Add(30 * time.Minute)
	page, err = st.ListAuditReads(AuditReadFilter{From: &at1230, To: &at1230, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.MatchedTotal != 2 || len(page.Items) != 2 ||
		page.Items[0].At == nil || !page.Items[0].At.Equal(at1230) ||
		page.Items[1].At == nil || !page.Items[1].At.Equal(at1230) {
		t.Fatalf("offset/legacy instants were compared as raw strings: %+v", page)
	}
}

func TestAuditReadContextCancellationAndActionCatalogIsolation(t *testing.T) {
	st := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.ListAuditReadsContext(ctx, AuditReadFilter{Limit: 10}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read err=%v", err)
	}
	actions := AuditActions()
	if len(actions) == 0 {
		t.Fatal("empty audit action catalog")
	}
	actions[0] = "mutated-by-caller"
	if !IsKnownAuditAction(AuditConnect) || IsKnownAuditAction(actions[0]) || AuditActions()[0] != AuditConnect {
		t.Fatalf("caller mutated audit action validation: %v", AuditActions())
	}
}

func TestAuditReadReplayClassificationIsRestrictedToCanonicalOperatorActions(t *testing.T) {
	for _, action := range []AuditAction{AuditConnect, AuditRetire, AuditUnretire, AuditOperatorDenied} {
		record := AuditReadRecord{Action: string(action), Detail: OperatorIdempotencyReplayPrefix + "lookalike"}
		if record.IsOperatorReplay() {
			t.Errorf("legacy/non-mutation action %q was classified as an operator replay", action)
		}
		record.Detail = OperatorTransportRejectionPrefix + "lookalike"
		if record.IsOperatorTransportRejection() {
			t.Errorf("legacy/non-mutation action %q was classified as an operator transport rejection", action)
		}
	}
	record := AuditReadRecord{Action: string(AuditMachineChannel), Detail: OperatorIdempotencyReplayPrefix + "cached"}
	if !record.IsOperatorReplay() {
		t.Fatal("canonical operator mutation lost replay classification")
	}
	record.Detail = OperatorTransportRejectionPrefix + "bad JSON"
	if !record.IsOperatorTransportRejection() {
		t.Fatal("canonical operator mutation lost transport-rejection classification")
	}
}

// The console offers one filter per entry in allAuditActions and stamps
// `unknown_action` on every row whose action is missing from it. So an action
// the Hub writes but this catalog forgets is not a cosmetic gap: the operator
// is told their own 派工 record is evidence the product cannot identify, and
// no filter reaches it. That is exactly what shipped — `verification-assign`
// was declared, written and displayed while the read catalog omitted it.
//
// Reading the declarations instead of restating them is the point. A hand
// written list here would need the same edit the catalog needs, so it would
// have been forgotten in the same commit.
func TestEveryDeclaredAuditActionIsReadable(t *testing.T) {
	declared := declaredAuditActions(t)
	if len(declared) < len(allAuditActions) {
		t.Fatalf("只解析出 %d 個 AuditAction 宣告，比 catalog 的 %d 少", len(declared), len(allAuditActions))
	}
	for _, action := range declared {
		if !IsKnownAuditAction(action) {
			t.Errorf("audit.go 宣告了 %q，但 allAuditActions 沒有它 —— "+
				"寫得出來的動作，稽核頁讀回來會標成 unknown_action 且篩不到", action)
		}
	}
	known := make(map[AuditAction]bool, len(declared))
	for _, action := range declared {
		known[action] = true
	}
	for _, action := range allAuditActions {
		if !known[action] {
			t.Errorf("allAuditActions 有 %q，但 audit.go 沒有宣告它", action)
		}
	}
}

// declaredAuditActions returns every `X AuditAction = "..."` constant in the
// package source. It parses rather than greps so a value inside a comment or a
// string cannot be mistaken for a declaration.
func declaredAuditActions(t *testing.T) []AuditAction {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, "audit.go", nil, 0)
	if err != nil {
		t.Fatalf("parse audit.go: %v", err)
	}
	var actions []AuditAction
	for _, decl := range parsed.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			name, ok := value.Type.(*ast.Ident)
			if !ok || name.Name != "AuditAction" {
				continue
			}
			for _, expression := range value.Values {
				literal, ok := expression.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("AuditAction 常數 %v 不是字串字面值", value.Names)
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", literal.Value, err)
				}
				actions = append(actions, AuditAction(unquoted))
			}
		}
	}
	if len(actions) == 0 {
		t.Fatal("audit.go 裡一個 AuditAction 宣告都沒解析到")
	}
	return actions
}
