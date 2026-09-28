package operator

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func newOperatorAuditReadStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAuditReadProjectsSafeExplicitEvidenceAndSampling(t *testing.T) {
	st := newOperatorAuditReadStore(t)
	for i := range 60 {
		if err := st.RecordAudit(store.AuditEntry{
			Action: store.AuditOperatorDenied, Subject: fmt.Sprintf("denial-%02d", i),
			SourceAddr: "100.64.0.7", OK: false,
		}); err != nil {
			t.Fatal(err)
		}
	}
	want := store.AuditEntry{
		Action: store.AuditMachineChannel, MachineID: "machine-1", Subject: "samplehub1",
		Reason: "planned rollout", IdempotencyKey: "request-1", RequestDigest: "sha256:digest",
		SourceAddr: "100.64.0.8", WhoNode: "operator-laptop", WhoUser: "owner@example.com",
		UserAgent: "clawctl-test/1", AuthSubject: "tailscale-user:42", AuthNodeID: "node-1",
		AuthCapability: "example.com/cap/clawctl-admin", AuthMethod: "tailscale-localapi-app-cap",
		AuthDecision: "AUTHORIZED", BoundaryDecision: "PASSED", SourceKind: "operator-api",
		OK: true, Detail: store.OperatorIdempotencyReplayPrefix + "same decision",
	}
	if err := st.RecordAudit(want); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	result, err := New(st).ListAudit(AuditListRequest{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != AuditReadSchemaVersion || result.Consistency != AuditReadConsistency ||
		!result.EvaluatedAt.Equal(now) || result.MatchedTotal != 61 || result.Total != 51 ||
		result.Succeeded != 1 || result.Failed != 50 || result.UnknownOutcome != 0 ||
		result.Denials.Mode != AuditDenialsSampled || result.Denials.Matched != 60 ||
		result.Denials.Included != 50 || result.Denials.Omitted != 10 || len(result.Items) != 51 ||
		result.NextCursor != nil || result.NextAfterAuditID != nil || result.CreationCeiling < 61 {
		t.Fatalf("result=%+v items=%d", result, len(result.Items))
	}
	first := result.Items[0]
	if first.Action != string(want.Action) || first.MachineID == nil || *first.MachineID != want.MachineID ||
		first.Outcome == nil || *first.Outcome != store.AuditOutcomeOK || !first.Replayed ||
		first.TransportRejection || first.AuthSubject == nil || *first.AuthSubject != want.AuthSubject ||
		first.WhoLabel() != "operator-laptop（owner@example.com）" || first.Issues == nil ||
		first.AlteredFields == nil || !first.Succeeded() || first.Failed() {
		t.Fatalf("projected event=%+v who=%q", first, first.WhoLabel())
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, wantKey := range []string{`"outcome":"ok"`, `"source_addr":"100.64.0.8"`, `"issues":[]`} {
		if !strings.Contains(string(raw), wantKey) {
			t.Errorf("JSON missing %s: %s", wantKey, raw)
		}
	}
	if strings.Contains(string(raw), `"ok":`) {
		t.Fatalf("safe audit DTO leaked ambiguous Store OK field: %s", raw)
	}
}

func TestAuditReadDefaultPageCannotBeFilledOnlyByDenialNoise(t *testing.T) {
	st := newOperatorAuditReadStore(t)
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditRetire, Subject: "older-domain-action", SourceAddr: "local-test", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 60 {
		if err := st.RecordAudit(store.AuditEntry{
			Action: store.AuditOperatorDenied, Subject: fmt.Sprintf("newer-denial-%02d", i),
			SourceAddr: "100.64.0.7", OK: false,
		}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := New(st).ListAudit(AuditListRequest{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 51 || len(result.Items) != 51 || result.NextCursor != nil ||
		result.Items[len(result.Items)-1].Subject != "older-domain-action" {
		t.Fatalf("default denial sampling hid domain action from first page: %+v", result)
	}
}

func TestAuditReadCursorIsFilterBoundAndDoesNotMutateCallerActions(t *testing.T) {
	st := newOperatorAuditReadStore(t)
	for i := range 4 {
		if err := st.RecordAudit(store.AuditEntry{
			Action: store.AuditConnect, MachineID: "machine-1", Subject: fmt.Sprintf("row-%d", i),
			SourceAddr: "local-test", OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	actions := []store.AuditAction{store.AuditConnect}
	request := AuditListRequest{MachineID: "machine-1", Actions: actions, Denials: AuditDenialsAll, Limit: 2}
	first, err := New(st).ListAudit(request, time.Now().UTC())
	if err != nil || first.NextCursor == nil || len(first.Items) != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if len(actions) != 1 || actions[0] != store.AuditConnect {
		t.Fatalf("normalization mutated caller action slice: %v", actions)
	}
	request.Cursor = *first.NextCursor
	second, err := New(st).ListAudit(request, time.Now().UTC())
	if err != nil || len(second.Items) != 2 || second.NextCursor != nil ||
		second.Items[0].AuditID >= first.Items[len(first.Items)-1].AuditID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	request.Outcome = store.AuditOutcomeFailed
	if _, err := New(st).ListAudit(request, time.Now().UTC()); !errors.Is(err, ErrInvalidAuditRead) {
		t.Fatalf("cursor crossed filters: %v", err)
	}
	request.Outcome = ""
	request.Cursor = (*first.NextCursor)[:len(*first.NextCursor)-1] + "A"
	if _, err := New(st).ListAudit(request, time.Now().UTC()); !errors.Is(err, ErrInvalidAuditRead) {
		t.Fatalf("tampered cursor err=%v", err)
	}
}

func TestAuditReadRepresentsMalformedRowsAndBoundsUnsafeText(t *testing.T) {
	st := newOperatorAuditReadStore(t)
	longSubject := "\u202e" + strings.Repeat("界", 900)
	if _, err := st.DB().Exec(`INSERT INTO audit_log
	 (at,action,subject,reason,source_addr,outcome,detail) VALUES (?,?,?,?,?,?,?)`,
		"bad-time", "future-action", longSubject, "line1\nline2", "", "maybe",
		string([]byte{'x', 0xff, '\n', 'y'})); err != nil {
		t.Fatal(err)
	}
	result, err := New(st).ListAudit(AuditListRequest{Denials: AuditDenialsAll}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.UnknownOutcome != 1 {
		t.Fatalf("result=%+v", result)
	}
	item := result.Items[0]
	if item.At != nil || item.Outcome != nil || item.OutcomeKnown() || len(item.Issues) != 4 ||
		len(item.AlteredFields) != 3 || item.Subject == longSubject || item.Detail == nil || item.Reason == nil ||
		!strings.Contains(*item.Detail, "�") || item.Succeeded() || item.Failed() {
		t.Fatalf("malformed projection=%+v", item)
	}
	for _, value := range []string{item.Subject, *item.Reason, *item.Detail} {
		if strings.ContainsAny(value, "\n\r\x1b") || strings.ContainsRune(value, '\u202e') {
			t.Fatalf("safe projection retained control/bidi text %q", value)
		}
	}
}

func TestAuditReadRejectsInvalidRequest(t *testing.T) {
	st := newOperatorAuditReadStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	later := now.Add(time.Hour)
	for _, request := range []AuditListRequest{
		{Limit: -1},
		{Limit: MaxAuditReadLimit + 1},
		{Actions: []store.AuditAction{"unknown"}},
		{Actions: []store.AuditAction{store.AuditConnect, store.AuditConnect}},
		{Outcome: "maybe"},
		{Principal: "\n"},
		{From: &later, To: &now},
		{Denials: "maybe"},
		{Cursor: "%%%"},
	} {
		if _, err := New(st).ListAudit(request, now); !errors.Is(err, ErrInvalidAuditRead) {
			t.Errorf("request=%+v err=%v", request, err)
		}
	}
	if _, err := New(st).ListAudit(AuditListRequest{}, time.Time{}); !errors.Is(err, ErrInvalidAuditRead) {
		t.Fatalf("zero evaluation time err=%v", err)
	}
}

func auditWhoPtr(s string) *string {
	return &s
}

// 這一格是操作員唯一看得到「是誰按的」的地方，少一個字就是少一個人。
func TestTheAuditWhoColumnSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name  string
		event AuditEvent
		want  string
	}{
		{
			name: "節點與使用者都問得到",
			event: AuditEvent{
				WhoNode:    auditWhoPtr("operator-laptop"),
				WhoUser:    auditWhoPtr("owner@example.com"),
				SourceAddr: "100.64.0.9",
			},
			want: "operator-laptop（owner@example.com）",
		},
		{
			name: "只問得到節點",
			event: AuditEvent{
				WhoNode:    auditWhoPtr("console-node"),
				SourceAddr: "100.64.0.9",
			},
			want: "console-node",
		},
		{
			name: "只問得到使用者",
			event: AuditEvent{
				WhoUser:    auditWhoPtr("owner@example.com"),
				SourceAddr: "100.64.0.9",
			},
			want: "100.64.0.9（owner@example.com）",
		},
		{
			name: "使用者問得到但連位址都沒有",
			event: AuditEvent{
				WhoUser: auditWhoPtr("owner@example.com"),
			},
			want: "未知裝置（owner@example.com）",
		},
		{
			name: "只有位址，附上問不到節點的理由",
			event: AuditEvent{
				SourceAddr:     "100.64.0.9",
				WhoUnavailable: auditWhoPtr("operator-principal-unavailable"),
			},
			want: "100.64.0.9 —— operator-principal-unavailable",
		},
		{
			name: "只有位址",
			event: AuditEvent{
				SourceAddr: "100.64.0.9",
			},
			want: "100.64.0.9",
		},
		{
			name:  "什麼都沒有",
			event: AuditEvent{},
			want:  "連來源位址都沒有",
		},
		{
			name: "授權主體不算傳輸來源",
			event: AuditEvent{
				AuthSubject: auditWhoPtr("tailscale-user:42"),
				SourceAddr:  "100.64.0.9",
			},
			want: "100.64.0.9",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.event.WhoLabel(); got != test.want {
				t.Errorf("傳輸來源實際是 %q，期望是 %q", got, test.want)
			}
		})
	}
}
