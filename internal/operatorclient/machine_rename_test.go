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
)

func renameClientDigest() string { return "sha256:" + strings.Repeat("a", 64) }

func renameClientPreview() MachineRenamePreviewResponse {
	return MachineRenamePreviewResponse{
		MachineID: "machine-1", CurrentDisplayName: "cnode-before", DisplayName: "cnode-after",
		PreviewedAt:        time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC),
		MachineIDPreserved: true, AgentUnaffected: true, ExpectationKeyChanges: true,
		PendingTokenLabelChanges: true, PreviewDigest: renameClientDigest(),
	}
}

func renameClientApply() MachineRenameResponse {
	return MachineRenameResponse{
		MachineID: "machine-1", PreviousDisplayName: "cnode-before", DisplayName: "cnode-after",
		AppliedAt:          time.Date(2026, 9, 13, 20, 1, 0, 0, time.UTC),
		MachineIDPreserved: true, AgentUnaffected: true, ExpectationKeyChanged: true,
		PendingTokenLabelChanged: true, PreviewDigest: renameClientDigest(),
	}
}

func writeRenameClientJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func TestMachineRenameClientPreviewApplyAndReplay(t *testing.T) {
	preview := renameClientPreview()
	apply := renameClientApply()
	var applies atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent || r.URL.RawQuery != "" {
			t.Errorf("unsafe request=%s headers=%v", r.URL.String(), r.Header)
		}
		raw, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/machines/machine-1/display-name-preview":
			if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "" ||
				string(raw) != `{"display_name":"cnode-after"}` {
				t.Errorf("preview headers=%v body=%s", r.Header, raw)
			}
			writeRenameClientJSON(t, w, preview)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/operator/machines/machine-1/display-name":
			if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "rename-key" ||
				string(raw) != `{"display_name":"cnode-after","confirm_display_name":"cnode-before","preview_digest":"`+renameClientDigest()+`","reason":"align registry"}` {
				t.Errorf("apply headers=%v body=%s", r.Header, raw)
			}
			response := apply
			if applies.Add(1) > 1 {
				response.Replayed = true
				w.Header().Set("Idempotency-Replayed", "true")
			}
			writeRenameClientJSON(t, w, response)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	gotPreview, err := client.PreviewMachineRename(t.Context(), "machine-1",
		MachineRenamePreviewRequest{DisplayName: "cnode-after"})
	if err != nil || gotPreview.PreviewDigest != renameClientDigest() || !gotPreview.PendingTokenLabelChanges {
		t.Fatalf("preview=%+v err=%v", gotPreview, err)
	}
	body := MachineRenameRequest{
		DisplayName: "cnode-after", ConfirmDisplayName: "cnode-before",
		PreviewDigest: renameClientDigest(), Reason: "align registry",
	}
	fresh, err := client.PutMachineRename(t.Context(), "machine-1", "rename-key", body)
	if err != nil || fresh.Replayed || !fresh.PendingTokenLabelChanged {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
	replay, err := client.PutMachineRename(t.Context(), "machine-1", "rename-key", body)
	if err != nil || !replay.Replayed || !replay.AppliedAt.Equal(fresh.AppliedAt) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestMachineRenameClientRejectsContradictoryResponses(t *testing.T) {
	for name, response := range map[string]any{
		"preview wrong target": func() any {
			value := renameClientPreview()
			value.MachineID = "machine-2"
			return value
		}(),
		"preview says agent changes": func() any {
			value := renameClientPreview()
			value.AgentUnaffected = false
			return value
		}(),
		"preview omits expectation impact": func() any {
			value := renameClientPreview()
			value.ExpectationKeyChanges = false
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeRenameClientJSON(t, w, response)
			}))
			defer server.Close()
			if _, err := operatorClientForServer(t, server).PreviewMachineRename(t.Context(), "machine-1",
				MachineRenamePreviewRequest{DisplayName: "cnode-after"}); err == nil {
				t.Fatal("contradictory preview was accepted")
			}
		})
	}

	apply := renameClientApply()
	apply.PreviousDisplayName = "someone-else"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeRenameClientJSON(t, w, apply)
	}))
	defer server.Close()
	if _, err := operatorClientForServer(t, server).PutMachineRename(t.Context(), "machine-1", "rename-key",
		MachineRenameRequest{
			DisplayName: "cnode-after", ConfirmDisplayName: "cnode-before",
			PreviewDigest: renameClientDigest(), Reason: "align registry",
		}); err == nil {
		t.Fatal("contradictory apply was accepted")
	}
}

func TestMachineRenameClientRejectsUnknownResponseFieldAndInvalidInputBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"machine_id":"machine-1","unknown":true}`))
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	if _, err := client.PreviewMachineRename(t.Context(), "machine-1",
		MachineRenamePreviewRequest{DisplayName: "cnode-after"}); err == nil {
		t.Fatal("unknown response field was accepted")
	}
	before := hits.Load()
	if _, err := client.PreviewMachineRename(t.Context(), "machine-1",
		MachineRenamePreviewRequest{DisplayName: " cnode-after"}); err == nil {
		t.Fatal("edge whitespace input was accepted")
	}
	if _, err := client.PutMachineRename(t.Context(), "machine-1", "", MachineRenameRequest{}); err == nil {
		t.Fatal("empty apply was accepted")
	}
	if hits.Load() != before {
		t.Fatalf("invalid input reached network: before=%d after=%d", before, hits.Load())
	}
}
