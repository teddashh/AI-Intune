package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

func assignedUserAPICache() *tailnet.Cache {
	cache := tailnet.NewCache()
	cache.SetStatus(tailnet.Status{Available: true, Self: tailnet.Peer{UserID: "42"}, TailnetUsers: []tailnet.TailnetUser{{UserID: "42", Login: "user@example.com"}}})
	return cache
}

func TestAssignedUserAPICacheControlNoStore(t *testing.T) {
	for _, tc := range []struct {
		name, method, body, contentType string
		missing                         bool
		status                          int
	}{
		{name: "讀取成功", method: http.MethodGet, status: http.StatusOK},
		{name: "讀取不存在的機器", method: http.MethodGet, missing: true, status: http.StatusNotFound},
		{name: "指派成功", method: http.MethodPut, body: `{"user_id":"42","expected_revision":0,"confirm_display_name":"指派測試"}`, status: http.StatusOK},
		{name: "確認名稱不符", method: http.MethodPut, body: `{"user_id":"42","expected_revision":0,"confirm_display_name":"別台"}`, status: http.StatusBadRequest},
		{name: "要求格式錯誤", method: http.MethodPut, body: `{`, status: http.StatusBadRequest},
		{name: "內容類型錯誤", method: http.MethodPut, contentType: "text/plain", body: `{}`, status: http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newJobsFixture(t, "指派測試")
			(&hub{store: f.store, tailnet: assignedUserAPICache()}).operatorRoutes(f.mux)
			id := f.machine.id
			if tc.missing {
				id = "missing-machine"
			}
			contentType := tc.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			rec := operatorRequestWithContentType(t, f.mux, tc.method,
				"/v1/operator/machines/"+id+"/assigned-user", "cache-control", contentType, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("回應狀態=%d，預期=%d；內容=%s", rec.Code, tc.status, rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("%s 回應 Cache-Control=%q，預期 no-store", tc.method, got)
			}
		})
	}
}

func TestAssignedUserAPIRevisionConfirmationReplayAndClear(t *testing.T) {
	f := newJobsFixture(t, "指派測試")
	cache := assignedUserAPICache()
	h := &hub{store: f.store, tailnet: cache, operatorService: operator.NewControlPlane(f.store, f.artifactsDir, cache)}
	h.operatorRoutes(f.mux)
	path := "/v1/operator/machines/" + f.machine.id + "/assigned-user"
	get := operatorRequest(t, f.mux, http.MethodGet, path, "", "")
	if get.Code != 200 || get.Header().Get("ETag") != `"assigned-user-revision-0"` {
		t.Fatalf("讀取=%d %s", get.Code, get.Body.String())
	}
	body := `{"user_id":"42","expected_revision":0,"confirm_display_name":"指派測試"}`
	first := operatorRequest(t, f.mux, http.MethodPut, path, "assigned-first", body)
	if first.Code != 200 || first.Header().Get("ETag") != `"assigned-user-revision-1"` {
		t.Fatalf("指派=%d %s", first.Code, first.Body.String())
	}
	var result operator.MachineAssignedUserResult
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil || result.UserID != "42" || result.UserLogin != "user@example.com" {
		t.Fatalf("指派結果=%+v 錯誤=%v", result, err)
	}
	replay := operatorRequest(t, f.mux, http.MethodPut, path, "assigned-first", body)
	if replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("重放=%d %s", replay.Code, replay.Body.String())
	}
	for _, tc := range []struct {
		key, body, code string
		status          int
	}{
		{"stale", body, store.OperatorCodePreconditionFailed, 412},
		{"name", `{"user_id":"42","expected_revision":1,"confirm_display_name":"別台"}`, store.OperatorCodeConfirmationMismatch, 400},
	} {
		rec := operatorRequest(t, f.mux, http.MethodPut, path, tc.key, tc.body)
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
			t.Fatalf("%s=%d %s", tc.key, rec.Code, rec.Body.String())
		}
		repeated := operatorRequest(t, f.mux, http.MethodPut, path, tc.key, tc.body)
		if repeated.Code != tc.status || repeated.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatalf("拒絕重放=%d %s", repeated.Code, repeated.Body.String())
		}
	}
	cache.SetStatus(tailnet.Status{})
	cleared := operatorRequest(t, f.mux, http.MethodPut, path, "clear", `{"user_id":"none","expected_revision":1,"confirm_display_name":"指派測試"}`)
	if cleared.Code != 200 || cleared.Header().Get("ETag") != `"assigned-user-revision-2"` {
		t.Fatalf("取消=%d %s", cleared.Code, cleared.Body.String())
	}
	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.AssignedUserID != "" || m.AssignedUserLogin != "" || m.AssignedUserRevision != 2 {
		t.Fatalf("取消結果=%+v %v", m, err)
	}
}

func TestAssignedUserAPIRejectsServiceWithoutTailnetAndDistinguishesRoster(t *testing.T) {
	for _, mode := range []string{"沒有來源的服務", "來源不可用", "不在名冊", "輸入無效", "使用快取來源"} {
		t.Run(mode, func(t *testing.T) {
			f := newJobsFixture(t, "指派測試")
			cache := assignedUserAPICache()
			h := &hub{store: f.store, tailnet: cache}
			user := "42"
			status := 200
			code := ""
			switch mode {
			case "沒有來源的服務":
				h.operatorService = operator.New(f.store)
				status = 503
				code = store.OperatorCodeTailnetSourceUnavailable
			case "來源不可用":
				cache.SetStatus(tailnet.Status{})
				status = 503
				code = store.OperatorCodeTailnetSourceUnavailable
			case "不在名冊":
				user = "99"
				status = 400
				code = store.OperatorCodeAssignedUserNotInRoster
			case "輸入無效":
				user = ""
				status = 400
				code = store.OperatorCodeBadAssignedUser
			}
			h.operatorRoutes(f.mux)
			rec := operatorRequest(t, f.mux, http.MethodPut, "/v1/operator/machines/"+f.machine.id+"/assigned-user", "source-case", fmt.Sprintf(`{"user_id":%q,"expected_revision":0,"confirm_display_name":"指派測試"}`, user))
			if rec.Code != status || (code != "" && !strings.Contains(rec.Body.String(), code)) {
				t.Fatalf("結果=%d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAssignedUserAPITransportAuditAndRoutePermissions(t *testing.T) {
	f := newJobsFixture(t, "指派測試")
	(&hub{store: f.store, tailnet: assignedUserAPICache()}).operatorRoutes(f.mux)
	path := "/v1/operator/machines/" + f.machine.id + "/assigned-user"
	rec := operatorRequest(t, f.mux, http.MethodPut, path, "bad-json", `{"user_id":"42","extra":true}`)
	if rec.Code != 400 {
		t.Fatalf("傳輸拒絕=%d", rec.Code)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Action == store.AuditMachineAssignedUser && !entry.OK && strings.HasPrefix(entry.Detail, store.OperatorTransportRejectionPrefix) {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少傳輸稽核：%+v", entries)
	}
	for _, tc := range []struct {
		pattern    string
		permission operatorauth.Permission
	}{
		{"GET /v1/operator/machines/{id}/assigned-user", operatorauth.View},
		{"PUT /v1/operator/machines/{id}/assigned-user", operatorauth.Admin},
		{"POST /machines/{id}/assigned-user-preview", operatorauth.Admin},
		{"POST /machines/{id}/assigned-user", operatorauth.Admin},
	} {
		if got := operatorRoutePolicies[tc.pattern]; got.Permission != tc.permission {
			t.Fatalf("路由權限 %s=%+v", tc.pattern, got)
		}
	}
}

func TestAssignedUserDirectCLIUsesRosterAndClears(t *testing.T) {
	f := newJobsFixture(t, "指派測試")
	var out bytes.Buffer
	inputs := machineAssignedUserInputs{Machine: f.machine.id, UserID: "42", ConfirmName: "指派測試"}
	if err := runMachineAssignedUser(t.Context(), f.store, assignedUserAPICache(), inputs, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "user@example.com") {
		t.Fatal(out.String())
	}
	out.Reset()
	inputs.UserID = "none"
	if err := runMachineAssignedUser(t.Context(), f.store, nil, inputs, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "未指派") {
		t.Fatal(out.String())
	}
}

func TestAssignedUserHTTPCLIUsesOperatorClient(t *testing.T) {
	f := newJobsFixture(t, "指派測試")
	(&hub{store: f.store, tailnet: assignedUserAPICache()}).operatorRoutes(f.mux)
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		go func() {
			defer serverConn.Close()
			req, err := http.ReadRequest(bufio.NewReader(serverConn))
			if err != nil {
				t.Errorf("讀取要求失敗：%v", err)
				return
			}
			defer req.Body.Close()
			rec := httptest.NewRecorder()
			permission := operatorauth.View
			if req.Method == http.MethodPut {
				permission = operatorauth.Admin
			}
			f.mux.ServeHTTP(rec, verifiedOperatorRequest(req, permission))
			response := rec.Result()
			defer response.Body.Close()
			if err := response.Write(serverConn); err != nil {
				t.Errorf("寫入回應失敗：%v", err)
			}
		}()
		return clientConn, nil
	}}
	deps := productionMachineCommandDeps()
	deps.newOperatorClient = func(base string) (*operatorclient.Client, error) {
		return operatorclient.NewWithHTTPClient(base, &http.Client{Transport: transport})
	}
	var out bytes.Buffer
	for _, user := range []string{"42", "none"} {
		out.Reset()
		err := runMachineCommandWithDeps(t.Context(), []string{"assigned-user", "--hub-url", "http://100.64.0.1:8080", "--machine", f.machine.id, "--user", user, "--confirm-name", "指派測試"}, &out, &out, deps)
		if err != nil {
			t.Fatal(err)
		}
		wanted := "user@example.com"
		if user == "none" {
			wanted = "未指派"
		}
		if !strings.Contains(out.String(), wanted) {
			t.Fatal(out.String())
		}
	}
}

func TestAssignedUserDirectSubcommandUsesStoppedHubGate(t *testing.T) {
	dbPath, id := directDBFixture(t)
	deps := productionMachineCommandDeps()
	verified := false
	deps.verifyHubStopped = func(_ context.Context, path string) error {
		verified = true
		if path != dbPath {
			t.Fatal(path)
		}
		return nil
	}
	deps.tailnetSource = assignedUserAPICache()
	var out bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{"assigned-user", "--db", dbPath, "--machine", id, "--user", "42", "--confirm-name", "direct-machine"}, &out, &out, deps)
	if err != nil || !verified || !strings.Contains(out.String(), "user@example.com") {
		t.Fatalf("結果=%s 錯誤=%v 已核對停止=%t", out.String(), err, verified)
	}
}

func TestAssignedUserMachineDetailCLIShowsExplicitState(t *testing.T) {
	f := newJobsFixture(t, "指派測試")
	service := operator.New(f.store)
	detail, err := service.MachineDetail(f.machine.id, jobsTestNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, assigned := range []operatorclient.MachineAssignedUserResponse{
		{}, {UserID: "42", UserLogin: "user@example.com", Revision: 3},
	} {
		var out bytes.Buffer
		if err := writeMachineDetail(&out, detail, false, "機器明細", &assigned); err != nil {
			t.Fatal(err)
		}
		expected := "指派使用者: 未指派（版本 0）"
		if assigned.UserID != "" {
			expected = `指派使用者: "user@example.com"（版本 3）`
		}
		if !strings.Contains(out.String(), expected) {
			t.Fatal(out.String())
		}
	}
}
