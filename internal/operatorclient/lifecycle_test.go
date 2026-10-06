package operatorclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func lifecycleClientTestTime() time.Time {
	return time.Date(2026, 9, 8, 21, 15, 0, 0, time.UTC)
}

func lifecycleClientTestDigest() string {
	return "sha256:" + strings.Repeat("b", 64)
}

func lifecycleClientTestImpact(before, after store.MachineLifecycleState) MachineLifecycleImpact {
	beforeActive := before == store.MachineLifecycleActive
	afterActive := after == store.MachineLifecycleActive
	return MachineLifecycleImpact{
		InDenominatorBefore: beforeActive, InDenominatorAfter: afterActive,
		DenominatorDelta: boolDelta(beforeActive, afterActive),
		RegistryRetained: true, HistoryPreserved: true,
		Channel: "canary", ChannelRevision: 2, ChannelPreserved: true,
		AgentCredentialPresent:    true,
		AgentAuthenticationBefore: beforeActive, AgentAuthenticationAfter: afterActive,
		PendingEnrollmentTokenCount: 2, PendingEnrollmentTokenExpiredCount: 1,
		PendingEnrollmentRedemptionBefore: beforeActive,
		PendingEnrollmentRedemptionAfter:  afterActive,
		Blockers:                          []string{},
	}
}

func lifecycleClientTestRead() MachineLifecycleReadResponse {
	return MachineLifecycleReadResponse{
		MachineID: "machine-1", DisplayName: "cnode", State: store.MachineLifecycleActive,
		LifecycleRevision: 4, InDenominator: true,
		RegistryRetained: true, HistoryPreserved: true, Channel: "canary", ChannelRevision: 2,
		AgentCredentialPresent: true, AgentAuthenticationAllowed: true,
		PendingEnrollmentTokenCount: 2, PendingEnrollmentTokenExpiredCount: 1,
		PendingEnrollmentRedemptionAllowed: true,
	}
}

func lifecycleClientTestPreview() MachineLifecyclePreviewResponse {
	return MachineLifecyclePreviewResponse{
		MachineID: "machine-1", DisplayName: "cnode",
		CurrentState: store.MachineLifecycleActive, DesiredState: store.MachineLifecycleRetired,
		LifecycleRevision: 4, PreviewedAt: lifecycleClientTestTime(),
		OpenAgentSessionCount:  2,
		MachineLifecycleImpact: lifecycleClientTestImpact(store.MachineLifecycleActive, store.MachineLifecycleRetired),
		PreviewDigest:          lifecycleClientTestDigest(),
	}
}

func lifecycleClientTestApply() MachineLifecycleResponse {
	retiredAt, eventID := lifecycleClientTestTime(), int64(17)
	return MachineLifecycleResponse{
		MachineID: "machine-1", DisplayName: "cnode",
		PreviousState: store.MachineLifecycleActive, State: store.MachineLifecycleRetired,
		LifecycleRevision: 5, Changed: true, NoOp: false,
		TransitionEventID: &eventID, RetiredAt: &retiredAt, AppliedAt: lifecycleClientTestTime(),
		MachineLifecycleImpact: lifecycleClientTestImpact(store.MachineLifecycleActive, store.MachineLifecycleRetired),
		PreviewDigest:          lifecycleClientTestDigest(), Replayed: false,
	}
}

func lifecycleClientHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
}

func lifecycleClientJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func spliceJSONBeforeClosingBrace(canonical []byte, fragment string) []byte {
	body := make([]byte, 0, len(canonical)+len(fragment))
	body = append(body, canonical[:len(canonical)-1]...)
	body = append(body, fragment...)
	return append(body, canonical[len(canonical)-1])
}

func TestMachineLifecycleClientReadPreviewApplyAndReplay(t *testing.T) {
	preview := lifecycleClientTestPreview()
	apply := lifecycleClientTestApply()
	read := lifecycleClientTestRead()
	var applyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent || r.URL.RawQuery != "" {
			t.Errorf("unsafe request headers/path: %s headers=%v", r.URL.String(), r.Header)
		}
		lifecycleClientHeaders(w)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/operator/machines/machine-1/lifecycle":
			if r.Header.Get("Content-Type") != "" || r.Header.Get("Idempotency-Key") != "" {
				t.Errorf("lifecycle GET carried mutation headers: %v", r.Header)
			}
			w.Header().Set("ETag", `"lifecycle-revision-4"`)
			_, _ = w.Write(lifecycleClientJSON(t, read))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/machines/machine-1/lifecycle-preview":
			raw, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "" ||
				string(raw) != `{"desired_state":"retired","expected_revision":4}` {
				t.Errorf("preview request headers=%v body=%s", r.Header, raw)
			}
			_, _ = w.Write(lifecycleClientJSON(t, preview))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/operator/machines/machine-1/lifecycle":
			raw, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "lifecycle-key" ||
				string(raw) != `{"desired_state":"retired","expected_revision":4,"confirm_display_name":"cnode","preview_digest":"`+lifecycleClientTestDigest()+`","reason":"hardware refresh"}` {
				t.Errorf("apply request headers=%v body=%s", r.Header, raw)
			}
			w.Header().Set("ETag", `"lifecycle-revision-5"`)
			response := apply
			if applyCalls.Add(1) > 1 {
				response.Replayed = true
				w.Header().Set("Idempotency-Replayed", "true")
			}
			_, _ = w.Write(lifecycleClientJSON(t, response))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)

	gotRead, err := client.MachineLifecycle(t.Context(), "machine-1")
	if err != nil || gotRead.State != store.MachineLifecycleActive || gotRead.Meta.ETagRevision != 4 {
		t.Fatalf("lifecycle read=%+v err=%v", gotRead, err)
	}
	requestPreview := MachineLifecyclePreviewRequest{DesiredState: store.MachineLifecycleRetired, ExpectedRevision: 4}
	gotPreview, err := client.PreviewMachineLifecycle(t.Context(), "machine-1", requestPreview)
	if err != nil || gotPreview.PreviewDigest != lifecycleClientTestDigest() || gotPreview.DenominatorDelta != -1 ||
		gotPreview.OpenAgentSessionCount != 2 {
		t.Fatalf("lifecycle preview=%+v err=%v", gotPreview, err)
	}
	requestApply := MachineLifecycleRequest{
		DesiredState: store.MachineLifecycleRetired, ExpectedRevision: 4,
		ConfirmDisplayName: "cnode", PreviewDigest: lifecycleClientTestDigest(), Reason: "hardware refresh",
	}
	fresh, err := client.PutMachineLifecycle(t.Context(), "machine-1", "lifecycle-key", requestApply)
	if err != nil || fresh.Replayed || fresh.Meta.IdempotencyReplayed || fresh.Meta.ETagRevision != 5 ||
		fresh.TransitionEventID == nil || fresh.RetiredAt == nil {
		t.Fatalf("fresh lifecycle apply=%+v err=%v", fresh, err)
	}
	replayed, err := client.PutMachineLifecycle(t.Context(), "machine-1", "lifecycle-key", requestApply)
	if err != nil || !replayed.Replayed || !replayed.Meta.IdempotencyReplayed ||
		replayed.TransitionEventID == nil || *replayed.TransitionEventID != *fresh.TransitionEventID {
		t.Fatalf("replayed lifecycle apply=%+v err=%v", replayed, err)
	}
}

func TestDecodeMachineLifecyclePreviewRequiresOpenAgentSessionCount(t *testing.T) {
	canonical := lifecycleClientJSON(t, lifecycleClientTestPreview())
	decoded, err := decodeMachineLifecyclePreview(canonical)
	if err != nil || decoded.OpenAgentSessionCount != 2 {
		t.Fatalf("decode lifecycle preview=%+v err=%v", decoded, err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "open_agent_session_count")
	if _, err := decodeMachineLifecyclePreview(lifecycleClientJSON(t, fields)); err == nil {
		t.Fatal("lifecycle preview decoder accepted missing open_agent_session_count")
	}
}

func TestMachineLifecycleClientRejectsInvalidInputsBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	client := operatorClientForServer(t, server)
	validPreview := MachineLifecyclePreviewRequest{DesiredState: store.MachineLifecycleRetired, ExpectedRevision: 0}
	validApply := MachineLifecycleRequest{
		DesiredState: store.MachineLifecycleRetired, ConfirmDisplayName: "cnode",
		PreviewDigest: lifecycleClientTestDigest(), Reason: "retire",
	}
	badStatePreview := validPreview
	badStatePreview.DesiredState = "deleted"
	negativePreview := validPreview
	negativePreview.ExpectedRevision = -1
	badStateApply := validApply
	badStateApply.DesiredState = "deleted"
	negativeApply := validApply
	negativeApply.ExpectedRevision = -1
	blankConfirm := validApply
	blankConfirm.ConfirmDisplayName = " "
	badDigest := validApply
	badDigest.PreviewDigest = "sha256:short"
	blankReason := validApply
	blankReason.Reason = "\t"
	tests := []struct {
		name string
		call func() error
	}{
		{"read path identity", func() error { _, err := client.MachineLifecycle(t.Context(), "../escape"); return err }},
		{"preview path identity", func() error { _, err := client.PreviewMachineLifecycle(t.Context(), " ", validPreview); return err }},
		{"preview state", func() error {
			_, err := client.PreviewMachineLifecycle(t.Context(), "machine-1", badStatePreview)
			return err
		}},
		{"preview revision", func() error {
			_, err := client.PreviewMachineLifecycle(t.Context(), "machine-1", negativePreview)
			return err
		}},
		{"empty key", func() error {
			_, err := client.PutMachineLifecycle(t.Context(), "machine-1", " ", validApply)
			return err
		}},
		{"oversized key", func() error {
			_, err := client.PutMachineLifecycle(t.Context(), "machine-1", strings.Repeat("k", 201), validApply)
			return err
		}},
		{"apply state", func() error {
			_, err := client.PutMachineLifecycle(t.Context(), "machine-1", "key", badStateApply)
			return err
		}},
		{"apply revision", func() error {
			_, err := client.PutMachineLifecycle(t.Context(), "machine-1", "key", negativeApply)
			return err
		}},
		{"apply confirmation", func() error {
			_, err := client.PutMachineLifecycle(t.Context(), "machine-1", "key", blankConfirm)
			return err
		}},
		{"apply digest", func() error {
			_, err := client.PutMachineLifecycle(t.Context(), "machine-1", "key", badDigest)
			return err
		}},
		{"apply reason", func() error {
			_, err := client.PutMachineLifecycle(t.Context(), "machine-1", "key", blankReason)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("invalid lifecycle input reached success")
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid lifecycle requests reached network %d times", hits.Load())
	}
}

func TestMachineLifecycleClientRejectsContradictoryOrNoncanonicalSuccess(t *testing.T) {
	previewRequest := MachineLifecyclePreviewRequest{DesiredState: store.MachineLifecycleRetired, ExpectedRevision: 4}
	applyRequest := MachineLifecycleRequest{
		DesiredState: store.MachineLifecycleRetired, ExpectedRevision: 4,
		ConfirmDisplayName: "cnode", PreviewDigest: lifecycleClientTestDigest(), Reason: "retire",
	}
	canonicalRead := lifecycleClientJSON(t, lifecycleClientTestRead())
	canonicalPreview := lifecycleClientJSON(t, lifecycleClientTestPreview())
	canonicalApply := lifecycleClientJSON(t, lifecycleClientTestApply())
	for _, test := range []struct {
		name   string
		method string
		body   []byte
		etag   string
		replay string
		call   func(*Client) error
	}{
		{
			name: "read without no-store", method: http.MethodGet,
			body: lifecycleClientJSON(t, MachineLifecycleReadResponse{
				MachineID: "machine-1", DisplayName: "cnode", State: store.MachineLifecycleActive,
				RegistryRetained: true, HistoryPreserved: true,
			}),
			etag: `"lifecycle-revision-0"`,
			call: func(c *Client) error { _, err := c.MachineLifecycle(t.Context(), "machine-1"); return err },
		},
		{
			name: "read legacy expected field", method: http.MethodGet,
			body: spliceJSONBeforeClosingBrace(canonicalRead, `,"expected":true`),
			etag: `"lifecycle-revision-4"`,
			call: func(c *Client) error { _, err := c.MachineLifecycle(t.Context(), "machine-1"); return err },
		},
		{
			name: "preview unknown field", method: http.MethodPost,
			body: spliceJSONBeforeClosingBrace(canonicalPreview, `,"secret":"leak"`),
			call: func(c *Client) error {
				_, err := c.PreviewMachineLifecycle(t.Context(), "machine-1", previewRequest)
				return err
			},
		},
		{
			name: "preview missing open agent session count", method: http.MethodPost,
			body: func() []byte {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(canonicalPreview, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "open_agent_session_count")
				return lifecycleClientJSON(t, fields)
			}(),
			call: func(c *Client) error {
				_, err := c.PreviewMachineLifecycle(t.Context(), "machine-1", previewRequest)
				return err
			},
		},
		{
			name: "preview legacy expected field", method: http.MethodPost,
			body: spliceJSONBeforeClosingBrace(canonicalPreview, `,"expected":true`),
			call: func(c *Client) error {
				_, err := c.PreviewMachineLifecycle(t.Context(), "machine-1", previewRequest)
				return err
			},
		},
		{
			name: "preview contradictory authentication", method: http.MethodPost,
			body: func() []byte {
				value := lifecycleClientTestPreview()
				value.AgentAuthenticationAfter = true
				return lifecycleClientJSON(t, value)
			}(),
			call: func(c *Client) error {
				_, err := c.PreviewMachineLifecycle(t.Context(), "machine-1", previewRequest)
				return err
			},
		},
		{
			name: "preview non UTC time", method: http.MethodPost,
			body: func() []byte {
				value := lifecycleClientTestPreview()
				value.PreviewedAt = value.PreviewedAt.In(time.FixedZone("EDT", -4*60*60))
				return lifecycleClientJSON(t, value)
			}(),
			call: func(c *Client) error {
				_, err := c.PreviewMachineLifecycle(t.Context(), "machine-1", previewRequest)
				return err
			},
		},
		{
			name: "apply bad revision", method: http.MethodPut,
			body: func() []byte {
				value := lifecycleClientTestApply()
				value.LifecycleRevision = 7
				return lifecycleClientJSON(t, value)
			}(),
			etag: `"lifecycle-revision-7"`,
			call: func(c *Client) error {
				_, err := c.PutMachineLifecycle(t.Context(), "machine-1", "key", applyRequest)
				return err
			},
		},
		{
			name: "apply claims a blocker", method: http.MethodPut,
			body: func() []byte {
				value := lifecycleClientTestApply()
				value.ActiveJobCount = 1
				value.Blockers = []string{"nonterminal_jobs"}
				return lifecycleClientJSON(t, value)
			}(),
			etag: `"lifecycle-revision-5"`,
			call: func(c *Client) error {
				_, err := c.PutMachineLifecycle(t.Context(), "machine-1", "key", applyRequest)
				return err
			},
		},
		{
			name: "retirement time differs from apply", method: http.MethodPut,
			body: func() []byte {
				value := lifecycleClientTestApply()
				retired := lifecycleClientTestTime().Add(-time.Minute)
				value.RetiredAt = &retired
				return lifecycleClientJSON(t, value)
			}(),
			etag: `"lifecycle-revision-5"`,
			call: func(c *Client) error {
				_, err := c.PutMachineLifecycle(t.Context(), "machine-1", "key", applyRequest)
				return err
			},
		},
		{
			name: "apply replay mismatch", method: http.MethodPut, body: canonicalApply,
			etag: `"lifecycle-revision-5"`, replay: "true",
			call: func(c *Client) error {
				_, err := c.PutMachineLifecycle(t.Context(), "machine-1", "key", applyRequest)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != test.method {
					t.Errorf("method=%s want=%s", r.Method, test.method)
				}
				w.Header().Set("Content-Type", "application/json")
				if test.name != "read without no-store" {
					w.Header().Set("Cache-Control", "no-store")
				}
				if test.etag != "" {
					w.Header().Set("ETag", test.etag)
				}
				if test.replay != "" {
					w.Header().Set("Idempotency-Replayed", test.replay)
				}
				_, _ = w.Write(test.body)
			}))
			defer server.Close()
			if err := test.call(operatorClientForServer(t, server)); err == nil {
				t.Fatal("client accepted noncanonical lifecycle success")
			}
		})
	}
}
