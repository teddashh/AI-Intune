package operatorclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
	"github.com/teddashh/AI-Intune/internal/store"
)

func settingClientDigest(suffix string) string {
	return "sha256:" + strings.Repeat(suffix, 64/len(suffix))
}

func settingClientBoard() operator.SettingBoardResult {
	reported := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	return operator.SettingBoardResult{
		Policies: []store.SettingPolicySummary{}, Assignments: []store.SettingAssignmentRecord{},
		Defaults: settingpolicy.Defaults(),
		Machines: []operator.SettingBoardMachine{{
			MachineID: "m1", DisplayName: "samplehub1", Source: settingpolicy.SourceMachine,
			PolicyID: "fast-fleet", Revision: 2, Settings: settingpolicy.Defaults(),
			Verdict: settingpolicy.VerdictApplied, VerdictLabel: settingpolicy.Label(settingpolicy.VerdictApplied),
			ReportedAt: &reported,
		}},
		Counts: map[settingpolicy.Verdict]int{settingpolicy.VerdictApplied: 1},
	}
}

func settingClientServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return operatorClientForServer(t, server)
}

func TestSettingClientReadsPreviewsAndWrites(t *testing.T) {
	board := settingClientBoard()
	digest := settingClientDigest("a")
	previewDigest := settingClientDigest("b")
	publishCalls, assignCalls := 0, 0

	client := settingClientServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operator/settings":
			_ = json.NewEncoder(w).Encode(board)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/setting-policies/preview":
			_ = json.NewEncoder(w).Encode(operator.SettingPolicyPreviewResult{
				PolicyID: "fast-fleet", CurrentRev: 1, NextRev: 2,
				Settings: settingpolicy.Settings{SchemaVersion: 1, CheckinIntervalSeconds: 60,
					ObservationIntervalSeconds: 300},
				Digest: digest, PreviewDigest: previewDigest, AffectedMachines: 1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/setting-policies":
			publishCalls++
			if r.Header.Get("Idempotency-Key") != "publish-key" {
				t.Errorf("publish key=%q", r.Header.Get("Idempotency-Key"))
			}
			result := store.OperatorSettingPolicyResult{PolicyID: "fast-fleet", Revision: 2,
				Digest: digest, Settings: settingpolicy.Defaults(), PublishedAt: time.Now().UTC(),
				Replayed: publishCalls == 2}
			if result.Replayed {
				w.Header().Set("Idempotency-Replayed", "true")
			} else {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(result)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/setting-assignments/preview":
			_ = json.NewEncoder(w).Encode(operator.SettingAssignmentPreviewResult{
				Scope: settingpolicy.ScopeMachine, ScopeID: "m1", ScopeLabel: "裝置",
				PolicyID: "fast-fleet", Revision: 2, Settings: settingpolicy.Defaults(),
				Digest: digest, PreviewDigest: previewDigest, AffectedMachines: 1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/setting-assignments":
			assignCalls++
			result := store.OperatorSettingAssignmentResult{AssignmentID: "a1",
				Scope: settingpolicy.ScopeMachine, ScopeID: "m1", Revision: 1,
				PolicyID: "fast-fleet", PolicyRev: 2, Digest: digest,
				Settings: settingpolicy.Defaults(), AssignedAt: time.Now().UTC(),
				Replayed: assignCalls == 2}
			if result.Replayed {
				w.Header().Set("Idempotency-Replayed", "true")
			} else {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(result)
		default:
			http.NotFound(w, r)
		}
	})

	gotBoard, err := client.SettingBoard(t.Context())
	if err != nil || len(gotBoard.Machines) != 1 || gotBoard.Counts[settingpolicy.VerdictApplied] != 1 {
		t.Fatalf("board=%+v err=%v", gotBoard, err)
	}

	preview, err := client.PreviewSettingPolicy(t.Context(), SettingPolicyPreviewRequest{
		PolicyID: "fast-fleet", CheckinIntervalSeconds: 60, ObservationIntervalSeconds: 300,
	})
	if err != nil || preview.NextRev != 2 || preview.PreviewDigest != previewDigest || preview.Unchanged {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}

	one := int64(1)
	publish := SettingPolicyPublishRequest{PolicyID: "fast-fleet", CheckinIntervalSeconds: 60,
		ObservationIntervalSeconds: 300, ExpectedRevision: &one, PreviewDigest: previewDigest,
		ConfirmPolicyID: "fast-fleet", Reason: "機隊要更早發現失聯"}
	published, err := client.PublishSettingPolicy(t.Context(), "publish-key", publish)
	if err != nil || published.Revision != 2 || published.Replayed {
		t.Fatalf("publish=%+v err=%v", published, err)
	}
	replayed, err := client.PublishSettingPolicy(t.Context(), "publish-key", publish)
	if err != nil || !replayed.Replayed {
		t.Fatalf("publish replay=%+v err=%v", replayed, err)
	}

	assignPreview, err := client.PreviewSettingAssignment(t.Context(), SettingAssignmentPreviewRequest{
		Scope: "machine", ScopeID: "m1", PolicyID: "fast-fleet", Revision: 2,
	})
	if err != nil || assignPreview.AffectedMachines != 1 {
		t.Fatalf("assign preview=%+v err=%v", assignPreview, err)
	}

	assign := SettingAssignmentRequest{Scope: "machine", ScopeID: "m1", PolicyID: "fast-fleet",
		Revision: 2, PreviewDigest: previewDigest, ConfirmScopeID: "m1", Reason: "先在這一台上線"}
	assigned, err := client.AssignSettingPolicy(t.Context(), "assign-key", assign)
	if err != nil || assigned.AssignmentID != "a1" || assigned.Replayed {
		t.Fatalf("assign=%+v err=%v", assigned, err)
	}
	assignedReplay, err := client.AssignSettingPolicy(t.Context(), "assign-key", assign)
	if err != nil || !assignedReplay.Replayed {
		t.Fatalf("assign replay=%+v err=%v", assignedReplay, err)
	}
}

// TestSettingClientRejectsContradictoryResponses covers the three ways a Hub
// could hand back bytes that look fine but mean something else.
func TestSettingClientRejectsContradictoryResponses(t *testing.T) {
	digest := settingClientDigest("a")

	t.Run("board counts disagree with rows", func(t *testing.T) {
		board := settingClientBoard()
		board.Counts = map[settingpolicy.Verdict]int{settingpolicy.VerdictApplied: 4}
		client := settingClientServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(board)
		})
		if _, err := client.SettingBoard(t.Context()); err == nil {
			t.Fatal("計數與列不符卻被接受")
		}
	})

	t.Run("default source still names a policy", func(t *testing.T) {
		board := settingClientBoard()
		board.Machines[0].Source = settingpolicy.SourceDefault
		client := settingClientServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(board)
		})
		if _, err := client.SettingBoard(t.Context()); err == nil {
			t.Fatal("來源是 default 卻帶著原則，仍被接受")
		}
	})

	t.Run("fresh status carries a replayed result", func(t *testing.T) {
		client := settingClientServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(store.OperatorSettingPolicyResult{
				PolicyID: "fast-fleet", Revision: 2, Digest: digest,
				Settings: settingpolicy.Defaults(), Replayed: true,
			})
		})
		one := int64(1)
		_, err := client.PublishSettingPolicy(t.Context(), "publish-key", SettingPolicyPublishRequest{
			PolicyID: "fast-fleet", CheckinIntervalSeconds: 60, ObservationIntervalSeconds: 300,
			ExpectedRevision: &one, PreviewDigest: settingClientDigest("b"),
			ConfirmPolicyID: "fast-fleet", Reason: "機隊要更早發現失聯",
		})
		if err == nil {
			t.Fatal("201 帶著 replayed=true 仍被接受")
		}
	})

	t.Run("preview marked unchanged while minting a revision", func(t *testing.T) {
		client := settingClientServer(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(operator.SettingPolicyPreviewResult{
				PolicyID: "fast-fleet", CurrentRev: 1, NextRev: 2,
				Settings: settingpolicy.Defaults(), Digest: digest,
				PreviewDigest: settingClientDigest("b"), Unchanged: true,
			})
		})
		if _, err := client.PreviewSettingPolicy(t.Context(), SettingPolicyPreviewRequest{
			PolicyID: "fast-fleet", CheckinIntervalSeconds: 60, ObservationIntervalSeconds: 300,
		}); err == nil {
			t.Fatal("unchanged 卻要產生新 revision，仍被接受")
		}
	})
}
