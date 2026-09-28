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

func TestOperatorMachineRenamePreviewApplyAndReplay(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id
	previewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/display-name-preview", "",
		`{"display_name":"cnode-renamed"}`)
	if previewRec.Code != http.StatusOK || previewRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("preview=%d headers=%v body=%s", previewRec.Code, previewRec.Header(), previewRec.Body.String())
	}
	var preview store.OperatorMachineRenamePreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil ||
		preview.MachineID != f.machine.id || preview.CurrentDisplayName != "cnode-operator" ||
		preview.DisplayName != "cnode-renamed" || !preview.MachineIDPreserved ||
		!preview.AgentUnaffected || !preview.ExpectationKeyChanges || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	body := fmt.Sprintf(`{"display_name":"cnode-renamed","confirm_display_name":"cnode-operator","preview_digest":%q,"reason":"align registry"}`,
		preview.PreviewDigest)
	fresh := operatorRequest(t, f.mux, http.MethodPut, base+"/display-name", "api-rename-key", body)
	if fresh.Code != http.StatusOK || fresh.Header().Get("Cache-Control") != "no-store" ||
		fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("fresh=%d headers=%v body=%s", fresh.Code, fresh.Header(), fresh.Body.String())
	}
	var result store.OperatorMachineRenameResult
	if err := json.Unmarshal(fresh.Body.Bytes(), &result); err != nil || result.MachineID != f.machine.id ||
		result.PreviousDisplayName != "cnode-operator" || result.DisplayName != "cnode-renamed" ||
		!result.MachineIDPreserved || !result.AgentUnaffected || !result.ExpectationKeyChanged ||
		result.Replayed || result.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.DisplayName != "cnode-renamed" {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPut, base+"/display-name", "api-rename-key", body)
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &result); err != nil || !result.Replayed {
		t.Fatalf("replay result=%+v err=%v", result, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditMachineRename || !entry.OK ||
			entry.AuthCapability != testOperatorCapability(operatorauth.Admin) {
			t.Fatalf("audit entry=%+v", entry)
		}
	}
}

func TestOperatorMachineRenameRejectsStalePreviewAndMalformedApplyWithoutMutation(t *testing.T) {
	f := observedOperatorFixture(t)
	base := "/v1/operator/machines/" + f.machine.id
	previewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/display-name-preview", "",
		`{"display_name":"cnode-next"}`)
	var preview store.OperatorMachineRenamePreviewResult
	if previewRec.Code != http.StatusOK || json.Unmarshal(previewRec.Body.Bytes(), &preview) != nil {
		t.Fatalf("preview=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	if _, err := f.store.DB().Exec(`UPDATE machine_registry SET display_name='cnode-between' WHERE machine_id=?`, f.machine.id); err != nil {
		t.Fatal(err)
	}
	staleBody := fmt.Sprintf(`{"display_name":"cnode-next","confirm_display_name":"cnode-between","preview_digest":%q,"reason":"align registry"}`,
		preview.PreviewDigest)
	stale := operatorRequest(t, f.mux, http.MethodPut, base+"/display-name", "api-rename-stale", staleBody)
	if stale.Code != http.StatusPreconditionFailed || !strings.Contains(stale.Body.String(), store.OperatorCodeMachineRenamePreviewStale) {
		t.Fatalf("stale=%d body=%s", stale.Code, stale.Body.String())
	}
	malformed := operatorRequest(t, f.mux, http.MethodPut, base+"/display-name", "api-rename-malformed",
		`{"display_name":"first","display_name":"second"}`)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed=%d body=%s", malformed.Code, malformed.Body.String())
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.DisplayName != "cnode-between" {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 2 || entries[0].OK || entries[1].OK ||
		entries[0].Action != store.AuditMachineRename || entries[1].Action != store.AuditMachineRename ||
		!strings.HasPrefix(entries[0].Detail, store.OperatorTransportRejectionPrefix) {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}
