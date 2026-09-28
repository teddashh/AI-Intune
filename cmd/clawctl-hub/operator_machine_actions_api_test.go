package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
)

// grantedOperatorRequest 給的是「這個人握有哪些 capability」，跟「這次請求用的是
// 哪一個」是兩件事。動作目錄讀的是前者。
func grantedOperatorRequest(req *http.Request, operate, admin bool) *http.Request {
	principal := operatorauth.Principal{
		SourceAddr: "100.64.0.7", NodeStableID: "node-stable-1", DeviceName: "operator",
		TailnetUserID: "42", TailnetUserLogin: "ted@example.com",
		AuthMethod:           operatorauth.AuthMethodLocalAPI,
		AuthorizedCapability: testOperatorCapability(operatorauth.View),
		GrantedCapabilities: operatorauth.CapabilityNames{
			View: testOperatorCapability(operatorauth.View),
		},
	}
	if operate {
		principal.GrantedCapabilities.Operate = testOperatorCapability(operatorauth.Operate)
	}
	if admin {
		principal.GrantedCapabilities.Admin = testOperatorCapability(operatorauth.Admin)
	}
	return operatorauth.WithPrincipal(req, principal)
}

func machineActionsRequest(t *testing.T, f jobsFixture, path string, operate, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "100.64.0.7:41234"
	f.mux.ServeHTTP(rec, grantedOperatorRequest(req, operate, admin))
	return rec
}

func decodeMachineActions(t *testing.T, rec *httptest.ResponseRecorder) operator.MachineActionCatalogue {
	t.Helper()
	var catalogue operator.MachineActionCatalogue
	if err := json.Unmarshal(rec.Body.Bytes(), &catalogue); err != nil {
		t.Fatalf("decode catalogue: %v body=%s", err, rec.Body.String())
	}
	return catalogue
}

func TestOperatorMachineActionsListsEveryActionWithItsPathAndBlocker(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	rec := machineActionsRequest(t, f, "/v1/operator/machines/"+f.machine.id+"/actions", true, true)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("code=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	catalogue := decodeMachineActions(t, rec)
	if catalogue.SchemaVersion != operator.MachineActionsSchemaVersion ||
		catalogue.MachineID != f.machine.id || catalogue.DisplayName != "cnode-operator" ||
		catalogue.State != operator.MachineLifecycleStateActive ||
		catalogue.EvaluatedAt.IsZero() || len(catalogue.Actions) != 7 ||
		catalogue.Available+catalogue.Blocked != len(catalogue.Actions) {
		t.Fatalf("catalogue=%+v", catalogue)
	}
	seen := map[operator.MachineActionKind]operator.MachineAction{}
	for _, action := range catalogue.Actions {
		if action.Label == "" || action.Effect == "" || action.Surface == "" ||
			action.Capability == "" || action.Confirm == "" {
			t.Fatalf("動作少了對外欄位：%+v", action)
		}
		if action.Available != (action.Blocker == "") || action.Available != (action.Situation == "") {
			t.Fatalf("available 與 blocker 不一致：%+v", action)
		}
		seen[action.Kind] = action
	}
	drill, ok := seen[operator.MachineActionDiagnosticNoop]
	if !ok || !drill.Available ||
		drill.Path != "/v1/operator/machines/{id}/diagnostic-noop-jobs" ||
		drill.PreviewPath != "/v1/operator/machines/{id}/diagnostic-noop-preview" {
		t.Fatalf("drill=%+v", drill)
	}
	if _, present := seen[operator.MachineActionRestore]; present {
		t.Fatalf("服役中的機器不該列出恢復管理：%+v", catalogue.Actions)
	}
	rename := seen[operator.MachineActionRename]
	if !rename.Available || rename.Path != "/v1/operator/machines/{id}/display-name" ||
		rename.PreviewPath != "/v1/operator/machines/{id}/display-name-preview" {
		t.Fatalf("rename=%+v", rename)
	}
	notes := seen[operator.MachineActionNotes]
	if !notes.Available || notes.Path != "/v1/operator/machines/{id}/notes" ||
		notes.PreviewPath != "/v1/operator/machines/{id}/notes-preview" {
		t.Fatalf("notes=%+v", notes)
	}
}

// 防止動作目錄宣告不存在、非 JSON、來源或 capability 不相符的 operator API 路徑；
// 在役與退役狀態都要走過，因為生命週期的一對動作是二選一。
func TestEveryOperatorAPIMachineActionMatchesItsRegisteredRoute(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	seen := make(map[operator.MachineActionKind]struct{})
	checkCatalogue := func() {
		rec := machineActionsRequest(t, f, "/v1/operator/machines/"+f.machine.id+"/actions", true, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		for _, action := range decodeMachineActions(t, rec).Actions {
			seen[action.Kind] = struct{}{}
			var permission operatorauth.Permission
			switch action.Capability {
			case operator.MachineActionCapabilityOperate:
				permission = operatorauth.Operate
			case operator.MachineActionCapabilityAdmin:
				permission = operatorauth.Admin
			default:
				t.Fatalf("action=%q capability=%q", action.Kind, action.Capability)
			}
			switch action.Surface {
			case operator.MachineActionSurfaceOperatorAPI:
				assertMachineActionRoutePolicy(t, action, action.Method+" "+action.Path, permission)
				if action.PreviewPath != "" {
					assertMachineActionRoutePolicy(t, action, "POST "+action.PreviewPath, permission)
				}
			case operator.MachineActionSurfaceWeb:
				if action.Method != "" || action.Path != "" || action.PreviewPath != "" {
					t.Fatalf("action=%q surface=%q method=%q path=%q preview_path=%q",
						action.Kind, action.Surface, action.Method, action.Path, action.PreviewPath)
				}
			default:
				t.Fatalf("action=%q surface=%q", action.Kind, action.Surface)
			}
		}
	}

	checkCatalogue()
	// Enrollment uses the Store clock rather than the fixture's Hub clock, so
	// derive this lifecycle coordinate from a current, valid Hub one.
	retiredAt := time.Now().UTC()
	if err := f.store.RetireMachine(f.machine.id, retiredAt); err != nil {
		t.Fatal(err)
	}
	checkCatalogue()

	for _, kind := range operator.MachineActionKinds() {
		if _, ok := seen[kind]; !ok {
			t.Errorf("動作 kind=%q 未被看過：這支測試名字說 every，但這個動作沒有任何一趟目錄產出過它", kind)
		}
	}
}

func assertMachineActionRoutePolicy(t *testing.T, action operator.MachineAction, route string,
	wantPermission operatorauth.Permission,
) {
	t.Helper()
	policy, known := operatorRoutePolicies[route]
	if !known {
		t.Fatalf("action=%q route=%q known=%t", action.Kind, route, known)
	}
	if policy.Representation != operatorJSON || policy.SourceKind != operator.SourceKindOperatorAPI ||
		policy.Permission != wantPermission {
		t.Fatalf("action=%q route=%q representation=%d source_kind=%q permission=%q want_permission=%q",
			action.Kind, route, policy.Representation, policy.SourceKind, policy.Permission, wantPermission)
	}
}

func TestOperatorMachineActionsShowsAViewOnlyOperatorNothingToPress(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	rec := machineActionsRequest(t, f, "/v1/operator/machines/"+f.machine.id+"/actions", false, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	catalogue := decodeMachineActions(t, rec)
	if len(catalogue.Actions) != 0 || catalogue.Available != 0 || catalogue.Blocked != 0 ||
		catalogue.MachineID != f.machine.id {
		t.Fatalf("view-only catalogue=%+v", catalogue)
	}
	operateOnly := decodeMachineActions(t,
		machineActionsRequest(t, f, "/v1/operator/machines/"+f.machine.id+"/actions", true, false))
	for _, action := range operateOnly.Actions {
		if action.Capability != operator.MachineActionCapabilityOperate {
			t.Fatalf("operate-only 看到 %+v", action)
		}
	}
	if len(operateOnly.Actions) != 2 {
		t.Fatalf("operate-only actions=%+v", operateOnly.Actions)
	}
}

func TestOperatorMachineActionsRefusesUnknownMachinesAndQueries(t *testing.T) {
	f := observedOperatorFixture(t)
	missing := machineActionsRequest(t, f, "/v1/operator/machines/machine-nope/actions", true, true)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing=%d body=%s", missing.Code, missing.Body.String())
	}
	query := machineActionsRequest(t, f,
		"/v1/operator/machines/"+f.machine.id+"/actions?kind=retire", true, true)
	if query.Code != http.StatusBadRequest {
		t.Fatalf("query=%d body=%s", query.Code, query.Body.String())
	}
}
