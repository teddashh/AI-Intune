package operatorclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func retentionClientPreview(now time.Time) operator.RetentionPrunePreview {
	policy, _ := store.NewOperatorRetentionPolicy(store.DefaultRetention())
	result := operator.RetentionPrunePreview{
		SchemaVersion: 1, EvaluatedAt: now, Policy: policy, ExpectedRevision: 4,
		Counts: []store.PruneCount{
			{Table: "observed_state", Deleted: 3, Kept: 1, Older: now.Add(-30 * 24 * time.Hour)},
			{Table: "machine_checkins", Deleted: 5, Older: now.Add(-14 * 24 * time.Hour)},
			{Table: "ticket_occupancy_observation", Older: now.Add(-400 * 24 * time.Hour)},
			{Table: "canary_silent_failures", Kept: 2, Older: now.Add(-30 * 24 * time.Hour)},
			{Table: "notifications", Older: now.Add(-30 * 24 * time.Hour)},
		},
		TotalDeleted: 8, KeptNewest: 3, Confirmation: "DELETE 8 ROWS",
	}
	copy := result
	copy.PreviewDigest = ""
	raw, _ := json.Marshal(copy)
	result.PreviewDigest = retentionDigest(raw)
	return result
}

func TestRetentionClientReadPreviewApplyAndReplay(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	preview := retentionClientPreview(now)
	last := now.Add(-20 * time.Hour)
	status := operator.RetentionStatus{
		SchemaVersion: 1, EvaluatedAt: now, Policy: preview.Policy, Revision: 4,
		HasRun: true, LastPruneAt: &last, LastPruneRows: 7,
	}
	result := store.OperatorPruneResult{
		EvaluatedAt: now, AppliedAt: now.Add(time.Second), Policy: preview.Policy,
		Counts: preview.Counts, TotalDeleted: preview.TotalDeleted,
		KeptNewest: preview.KeptNewest, Revision: 8,
	}
	applyCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/v1/operator/maintenance/retention":
			_ = json.NewEncoder(w).Encode(status)
		case "/v1/operator/maintenance/retention/prune-preview":
			_ = json.NewEncoder(w).Encode(preview)
		case "/v1/operator/maintenance/retention/prunes":
			applyCalls++
			var body RetentionPruneApplyRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Header.Get("Idempotency-Key") != "retention-key" {
				t.Errorf("apply body=%+v err=%v key=%q", body, err, r.Header.Get("Idempotency-Key"))
			}
			out := result
			if applyCalls == 2 {
				out.Replayed = true
				w.Header().Set("Idempotency-Replayed", "true")
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	if got, err := client.RetentionStatus(t.Context()); err != nil || got.Revision != 4 || !got.HasRun {
		t.Fatalf("status=%+v err=%v", got, err)
	}
	previewRequest := RetentionPrunePreviewRequest{EvaluatedAt: &now, Policy: &preview.Policy}
	if got, err := client.PreviewRetentionPrune(t.Context(), previewRequest); err != nil || got.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("preview=%+v err=%v", got, err)
	}
	apply := RetentionPruneApplyRequest{
		EvaluatedAt: now, Policy: &preview.Policy, ExpectedRevision: preview.ExpectedRevision,
		Confirm: preview.Confirmation, PreviewDigest: preview.PreviewDigest, Reason: "scheduled retention",
	}
	if got, err := client.ApplyRetentionPrune(t.Context(), "retention-key", apply); err != nil || got.Replayed {
		t.Fatalf("fresh=%+v err=%v", got, err)
	}
	if got, err := client.ApplyRetentionPrune(t.Context(), "retention-key", apply); err != nil || !got.Replayed {
		t.Fatalf("replay=%+v err=%v", got, err)
	}
}

func TestRetentionClientRejectsApplyResponsesThatMisrepresentDestructiveOutcome(t *testing.T) {
	const expectedError = "operator client: retention apply response does not match request"

	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	preview := retentionClientPreview(now)
	request := RetentionPruneApplyRequest{
		EvaluatedAt: now, Policy: &preview.Policy, ExpectedRevision: preview.ExpectedRevision,
		Confirm: preview.Confirmation, PreviewDigest: preview.PreviewDigest,
		Reason: "scheduled retention",
	}
	honest := store.OperatorPruneResult{
		EvaluatedAt: now, AppliedAt: now.Add(time.Second), Policy: preview.Policy,
		Counts: preview.Counts, TotalDeleted: preview.TotalDeleted,
		KeptNewest: preview.KeptNewest, Revision: preview.ExpectedRevision + 1,
	}
	apply := func(result store.OperatorPruneResult) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(result)
		}))
		defer server.Close()
		client := operatorClientForServer(t, server)
		_, err := client.ApplyRetentionPrune(t.Context(), "retention-key", request)
		return err
	}
	if err := apply(honest); err != nil {
		t.Fatalf("honest response was rejected, so the cases below would not measure "+
			"the response-mismatch clauses under test: %v", err)
	}
	tests := []struct {
		name        string
		mutate      func(*store.OperatorPruneResult)
		consequence string
	}{
		{
			name: "evaluated_at does not identify the request",
			mutate: func(result *store.OperatorPruneResult) {
				delta := -time.Second
				result.EvaluatedAt = result.EvaluatedAt.Add(delta)
				for i := range result.Counts {
					result.Counts[i].Older = result.Counts[i].Older.Add(delta)
				}
			},
			consequence: "stdout's only echo of the request identity would describe a " +
				"destructive cleanup as of another second, so a subsequent --as-of or " +
				"retry --evaluated-at would not match the deletion window",
		},
		{
			name: "total deleted is zero",
			mutate: func(result *store.OperatorPruneResult) {
				result.TotalDeleted = 0
				for i := range result.Counts {
					result.Counts[i].Deleted = 0
				}
			},
			consequence: "the CLI would print deleted=0 and exit 0 after the operator " +
				"confirmed DELETE N ROWS, translating the no-data protocol outcome into " +
				"a completed cleanup with zero deletions",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := honest
			result.Counts = append([]store.PruneCount(nil), honest.Counts...)
			test.mutate(&result)
			err := apply(result)
			if err == nil || err.Error() != expectedError {
				t.Errorf("got error %v, expected %q; consequence: %s", err, expectedError,
					test.consequence)
			}
		})
	}
}

// apply 路徑上 validateRetentionCounts 是唯一會質疑 counts 的守衛：
// preview 還有 preview_digest 重算當後備，apply 沒有。而這份 counts
// 是不可逆刪除之後 operator 手上唯一一份「刪掉了什麼」的紀錄。
// 既有那支 RejectsContradictoryCounts 走 preview 且只動 TotalDeleted，
// 會先被 Confirmation 那一條攔下，從來沒行使過這支 validator。
func TestRetentionApplyRejectsACountsBlockThatContradictsThePolicy(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	preview := retentionClientPreview(now)
	request := RetentionPruneApplyRequest{
		EvaluatedAt: now, Policy: &preview.Policy, ExpectedRevision: preview.ExpectedRevision,
		Confirm: preview.Confirmation, PreviewDigest: preview.PreviewDigest,
		Reason: "scheduled retention",
	}
	honest := store.OperatorPruneResult{
		EvaluatedAt: now, AppliedAt: now.Add(time.Second), Policy: preview.Policy,
		Counts: preview.Counts, TotalDeleted: preview.TotalDeleted,
		KeptNewest: preview.KeptNewest, Revision: preview.ExpectedRevision + 1,
	}
	apply := func(result store.OperatorPruneResult) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(result)
		}))
		defer server.Close()
		client := operatorClientForServer(t, server)
		_, err := client.ApplyRetentionPrune(t.Context(), "retention-counts-key", request)
		return err
	}
	if err := apply(honest); err != nil {
		t.Fatalf("honest baseline was rejected, so the cases below would not measure "+
			"the validateRetentionCounts clause under test: %v", err)
	}
	tests := []struct {
		name        string
		mutate      func(*store.OperatorPruneResult)
		expected    string
		consequence string
	}{
		{
			name: "boundary leaves the policy window",
			mutate: func(result *store.OperatorPruneResult) {
				result.Counts[0].Older = result.Counts[0].Older.Add(24 * time.Hour)
			},
			expected: "operator client: retention count boundary is invalid",
			consequence: "the only durable receipt of an irreversible deletion would name a cutoff " +
				"the approved policy never implied, so the operator could not tell which rows were destroyed",
		},
		{
			name: "a table is renamed in the receipt",
			mutate: func(result *store.OperatorPruneResult) {
				result.Counts[0].Table = "observed_state_archive"
			},
			expected: "operator client: retention count boundary is invalid",
			consequence: "the receipt would attribute the deletions to a table the policy does not cover, " +
				"so the operator would look for the destroyed rows in the wrong place",
		},
		{
			name: "the per-table deletions do not add up",
			mutate: func(result *store.OperatorPruneResult) {
				result.Counts[0].Deleted++
			},
			expected: "operator client: retention count totals are inconsistent",
			consequence: "the per-table breakdown would claim one more destroyed row than the total " +
				"the operator confirmed, so the two halves of the same receipt disagree about what was deleted",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := honest
			result.Counts = append([]store.PruneCount(nil), honest.Counts...)
			test.mutate(&result)
			err := apply(result)
			if err == nil || err.Error() != test.expected {
				t.Errorf("got error %v, expected %q; consequence: %s", err, test.expected,
					test.consequence)
			}
		})
	}
}

func TestRetentionClientRejectsContradictoryCounts(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	preview := retentionClientPreview(now)
	preview.TotalDeleted++
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(preview)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	if _, err := client.PreviewRetentionPrune(t.Context(), RetentionPrunePreviewRequest{}); err == nil {
		t.Fatal("client accepted contradictory retention totals")
	}
}

func TestRetentionClientRejectsUnknownNestedCountField(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	raw, err := json.Marshal(retentionClientPreview(now))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"deleted":3`, `"deleted":3,"raw_path":"/private"`, 1))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	if _, err := client.PreviewRetentionPrune(t.Context(), RetentionPrunePreviewRequest{}); err == nil {
		t.Fatal("client accepted unknown nested retention count field")
	}
}

func TestRetentionClientRefusesANonWholeSecondEvaluationBeforeCallingTheHub(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 500000000, time.UTC)
	policy, err := store.NewOperatorRetentionPolicy(store.DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	tests := []struct {
		name string
		call func() error
	}{
		{"preview", func() error {
			_, err := client.PreviewRetentionPrune(t.Context(), RetentionPrunePreviewRequest{EvaluatedAt: &now})
			return err
		}},
		{"apply", func() error {
			_, err := client.ApplyRetentionPrune(t.Context(), "retention-key", RetentionPruneApplyRequest{
				EvaluatedAt: now, Policy: &policy, ExpectedRevision: 4, Confirm: "DELETE 1 ROWS",
				PreviewDigest: "sha256:" + strings.Repeat("a", 64), Reason: "scheduled retention",
			})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Errorf("evaluated_at 不是整秒卻未在本地拒絕；Hub 端仍會拒絕，白等一次來回才得到較不具體的錯誤")
			}
			if hits.Load() != 0 {
				t.Errorf("evaluated_at 不是整秒時呼叫 Hub %d 次，want 0", hits.Load())
			}
		})
	}
}

func TestRetentionClientTreatsAnOmittedEvaluationDifferentlyFromAZeroOne(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	preview := retentionClientPreview(now)
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(preview)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	if _, err := client.PreviewRetentionPrune(t.Context(), RetentionPrunePreviewRequest{}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("省略 evaluated_at 呼叫 Hub %d 次，want 1", hits.Load())
	}
	zero := time.Time{}
	if _, err := client.PreviewRetentionPrune(t.Context(), RetentionPrunePreviewRequest{EvaluatedAt: &zero}); err == nil {
		t.Errorf("明確零值 evaluated_at 被當成未指定；Hub 另選時間會使 digest 與重放對不上")
	}
	if hits.Load() != 1 {
		t.Errorf("明確零值 evaluated_at 呼叫了 Hub；hits=%d, want 1", hits.Load())
	}
}
