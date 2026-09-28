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

func notesClientDigest() string { return "sha256:" + strings.Repeat("b", 64) }

func notesClientPreview() MachineNotesPreviewResponse {
	return MachineNotesPreviewResponse{
		MachineID: "machine-1", DisplayName: "samplehub1", CurrentNotes: "", Notes: "GPU runner",
		PreviewedAt:          time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC),
		RegistryNotesChanged: true, MachineConfigurationUnchanged: true, AgentUnaffected: true,
		PreviewDigest: notesClientDigest(),
	}
}

func notesClientApply() MachineNotesResponse {
	return MachineNotesResponse{
		MachineID: "machine-1", DisplayName: "samplehub1", NotesPresent: true,
		AppliedAt:            time.Date(2026, 9, 13, 20, 1, 0, 0, time.UTC),
		RegistryNotesChanged: true, MachineConfigurationUnchanged: true, AgentUnaffected: true,
		PreviewDigest: notesClientDigest(),
	}
}

func writeNotesClientJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func TestMachineNotesClientPreviewApplyAndReplay(t *testing.T) {
	preview := notesClientPreview()
	apply := notesClientApply()
	var applies atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/operator/machines/machine-1/notes-preview":
			if string(raw) != `{"notes":"GPU runner"}` || r.Header.Get("Idempotency-Key") != "" {
				t.Errorf("preview headers=%v body=%s", r.Header, raw)
			}
			writeNotesClientJSON(t, w, preview)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/operator/machines/machine-1/notes":
			if r.Header.Get("Idempotency-Key") != "notes-key" ||
				string(raw) != `{"notes":"GPU runner","confirm_display_name":"samplehub1","preview_digest":"`+notesClientDigest()+`","reason":"record purpose"}` {
				t.Errorf("apply headers=%v body=%s", r.Header, raw)
			}
			response := apply
			if applies.Add(1) > 1 {
				response.Replayed = true
				w.Header().Set("Idempotency-Replayed", "true")
			}
			writeNotesClientJSON(t, w, response)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	gotPreview, err := client.PreviewMachineNotes(t.Context(), "machine-1",
		MachineNotesPreviewRequest{Notes: "GPU runner"})
	if err != nil || gotPreview.PreviewDigest != notesClientDigest() {
		t.Fatalf("preview=%+v err=%v", gotPreview, err)
	}
	body := MachineNotesRequest{
		Notes: "GPU runner", ConfirmDisplayName: "samplehub1",
		PreviewDigest: notesClientDigest(), Reason: "record purpose",
	}
	fresh, err := client.PutMachineNotes(t.Context(), "machine-1", "notes-key", body)
	if err != nil || fresh.Replayed || !fresh.NotesPresent {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
	replay, err := client.PutMachineNotes(t.Context(), "machine-1", "notes-key", body)
	if err != nil || !replay.Replayed || !replay.AppliedAt.Equal(fresh.AppliedAt) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestMachineNotesClientRejectsContradictoryResponseAndInvalidInput(t *testing.T) {
	response := notesClientPreview()
	response.MachineConfigurationUnchanged = false
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeNotesClientJSON(t, w, response)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	if _, err := client.PreviewMachineNotes(t.Context(), "machine-1",
		MachineNotesPreviewRequest{Notes: "GPU runner"}); err == nil {
		t.Fatal("contradictory preview was accepted")
	}
	before := hits.Load()
	for _, notes := range []string{
		" leading", "line\nbreak", "zero\u200bwidth", string([]byte{0xff}), strings.Repeat("x", 1001),
	} {
		if _, err := client.PreviewMachineNotes(t.Context(), "machine-1",
			MachineNotesPreviewRequest{Notes: notes}); err == nil {
			t.Fatalf("invalid notes %q accepted", notes)
		}
	}
	if _, err := client.PutMachineNotes(t.Context(), "machine-1", "", MachineNotesRequest{}); err == nil {
		t.Fatal("empty apply was accepted")
	}
	if hits.Load() != before {
		t.Fatalf("invalid input reached network: before=%d after=%d", before, hits.Load())
	}
}

func TestMachineNotesClientRejectsContradictoryApplyResponse(t *testing.T) {
	response := notesClientApply()
	response.NotesPresent = false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeNotesClientJSON(t, w, response)
	}))
	defer server.Close()
	_, err := operatorClientForServer(t, server).PutMachineNotes(t.Context(), "machine-1", "notes-key",
		MachineNotesRequest{
			Notes: "GPU runner", ConfirmDisplayName: "samplehub1",
			PreviewDigest: notesClientDigest(), Reason: "record purpose",
		})
	if err == nil {
		t.Fatal("contradictory apply response was accepted")
	}
}

func TestMachineNotesClientStrictDecoderRejectsUnknownField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"machine_id":"machine-1","unknown":true}`))
	}))
	defer server.Close()
	if _, err := operatorClientForServer(t, server).PreviewMachineNotes(t.Context(), "machine-1",
		MachineNotesPreviewRequest{Notes: "GPU runner"}); err == nil {
		t.Fatal("unknown response field was accepted")
	}
}
