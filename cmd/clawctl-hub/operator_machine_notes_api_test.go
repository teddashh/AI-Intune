package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestOperatorMachineNotesPreviewApplyReplayAndClear(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id
	previewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/notes-preview", "",
		`{"notes":"GPU runner"}`)
	if previewRec.Code != http.StatusOK || previewRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview=%d headers=%v body=%s", previewRec.Code, previewRec.Header(), previewRec.Body.String())
	}
	var preview store.OperatorMachineNotesPreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil ||
		preview.MachineID != f.machine.id || preview.DisplayName != "cnode-operator" ||
		preview.CurrentNotes != "" || preview.Notes != "GPU runner" ||
		!preview.RegistryNotesChanged || !preview.MachineConfigurationUnchanged ||
		!preview.AgentUnaffected || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	body := fmt.Sprintf(`{"notes":"GPU runner","confirm_display_name":"cnode-operator","preview_digest":%q,"reason":"record purpose"}`,
		preview.PreviewDigest)
	fresh := operatorRequest(t, f.mux, http.MethodPut, base+"/notes", "api-notes-key", body)
	if fresh.Code != http.StatusOK || fresh.Header().Get("Idempotency-Replayed") != "" ||
		strings.Contains(fresh.Body.String(), "GPU runner") {
		t.Fatalf("fresh=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var result store.OperatorMachineNotesResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &result); err != nil || result.MachineID != f.machine.id ||
		result.DisplayName != "cnode-operator" || result.PreviousNotesPresent || !result.NotesPresent ||
		!result.RegistryNotesChanged || !result.MachineConfigurationUnchanged || !result.AgentUnaffected ||
		result.Replayed || result.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.Notes != "GPU runner" {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPut, base+"/notes", "api-notes-key", body)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditMachineNotes || !entry.OK ||
			entry.AuthCapability != testOperatorCapability(operatorauth.Admin) ||
			strings.Contains(entry.Detail, "GPU runner") {
			t.Fatalf("audit entry=%+v", entry)
		}
	}
}

func TestOperatorMachineNotesRejectsStaleAndMalformedApplyWithoutMutation(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id
	previewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/notes-preview", "",
		`{"notes":"desired"}`)
	var preview store.OperatorMachineNotesPreviewResult
	if previewRec.Code != http.StatusOK || json.Unmarshal(previewRec.Body.Bytes(), &preview) != nil {
		t.Fatalf("preview=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	if _, err := f.store.DB().Exec(`UPDATE machine_registry SET notes='changed elsewhere' WHERE machine_id=?`, f.machine.id); err != nil {
		t.Fatal(err)
	}
	staleBody := fmt.Sprintf(`{"notes":"desired","confirm_display_name":"cnode-operator","preview_digest":%q,"reason":"record purpose"}`,
		preview.PreviewDigest)
	stale := operatorRequest(t, f.mux, http.MethodPut, base+"/notes", "api-notes-stale", staleBody)
	if stale.Code != http.StatusPreconditionFailed || !strings.Contains(stale.Body.String(), store.OperatorCodeMachineNotesPreviewStale) {
		t.Fatalf("stale=%d body=%s", stale.Code, stale.Body.String())
	}
	malformed := operatorRequest(t, f.mux, http.MethodPut, base+"/notes", "api-notes-malformed",
		`{"notes":"first","notes":"second"}`)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed=%d body=%s", malformed.Code, malformed.Body.String())
	}
	machine, _ := f.store.GetMachine(f.machine.id)
	if machine.Notes != "changed elsewhere" {
		t.Fatalf("notes changed to %q", machine.Notes)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 2 || entries[0].OK || entries[1].OK ||
		entries[0].Action != store.AuditMachineNotes || entries[1].Action != store.AuditMachineNotes ||
		!strings.HasPrefix(entries[0].Detail, store.OperatorTransportRejectionPrefix) {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}
