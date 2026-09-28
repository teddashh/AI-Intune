package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

const defaultWebJobPageSize = 50

type jobsPage struct {
	Result operator.JobListResult

	MachineID    string
	DeploymentID string
	ResourceKind string
	ResourceID   string
	Limit        int
	States       []jobStateOption
	Filtered     bool
	NextURL      string
}

type jobStateOption struct {
	Value    string
	Label    string
	Selected bool
}

// jobs is the global, read-only queue. Agent callback routes remain on the
// machine plane; this page only consumes the same operator service as JSON and
// the official CLI.
func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	request, view, err := parseWebJobListRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	result, err := s.operator.ListJobs(request, now)
	if err != nil {
		if errors.Is(err, operator.ErrInvalidJobRead) || errors.Is(err, store.ErrInvalidJobRead) {
			http.Error(w, "工作單篩選或續頁游標不合法。", http.StatusBadRequest)
			return
		}
		s.fail(w, "讀取工作單帳本失敗", err)
		return
	}
	view.Result = result
	if result.NextCursor != nil {
		view.NextURL = webJobListURL(request, *result.NextCursor)
	}
	s.render(w, r, "jobs.html", page{
		Title: "代理程式活動", Nav: "jobs", Now: now.Local().Format("2006-01-02 15:04"), JobList: &view,
	})
}

func parseWebJobListRequest(r *http.Request) (operator.JobListRequest, jobsPage, error) {
	if r == nil || r.URL == nil {
		return operator.JobListRequest{}, jobsPage{}, errors.New("工作單查詢不存在。")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.JobListRequest{}, jobsPage{}, errors.New("工作單查詢格式不合法。")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.JobListRequest{}, jobsPage{}, errors.New("工作單查詢不可是空的問號。")
	}
	allowed := map[string]bool{
		"machine_id": true, "state": true, "deployment_id": true,
		"resource_kind": true, "resource_id": true, "limit": true, "cursor": true, "section": true,
	}
	for key := range values {
		if !allowed[key] {
			return operator.JobListRequest{}, jobsPage{}, fmt.Errorf("不支援工作單查詢參數 %q。", key)
		}
	}

	single := func(name string, maxBytes int) (string, error) {
		entries, present := values[name]
		if !present {
			return "", nil
		}
		if len(entries) != 1 || entries[0] == "" || entries[0] != strings.TrimSpace(entries[0]) ||
			len(entries[0]) > maxBytes {
			return "", fmt.Errorf("工作單查詢參數 %q 必須恰有一個非空且長度合法的值。", name)
		}
		for _, char := range entries[0] {
			if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
				return "", fmt.Errorf("工作單查詢參數 %q 不可含控制或隱形格式字元。", name)
			}
		}
		return entries[0], nil
	}
	machineID, err := single("machine_id", 256)
	if err != nil {
		return operator.JobListRequest{}, jobsPage{}, err
	}
	deploymentID, err := single("deployment_id", 256)
	if err != nil {
		return operator.JobListRequest{}, jobsPage{}, err
	}
	resourceKind, err := single("resource_kind", 128)
	if err != nil {
		return operator.JobListRequest{}, jobsPage{}, err
	}
	resourceID, err := single("resource_id", 256)
	if err != nil {
		return operator.JobListRequest{}, jobsPage{}, err
	}
	cursor, err := single("cursor", 2048)
	if err != nil {
		return operator.JobListRequest{}, jobsPage{}, err
	}
	section, err := single("section", 32)
	if err != nil {
		return operator.JobListRequest{}, jobsPage{}, err
	}
	if section != "" && section != "agent-overview" && section != "job-ledger" {
		return operator.JobListRequest{}, jobsPage{}, errors.New("代理程式頁面位置不合法。")
	}

	limit := defaultWebJobPageSize
	if raw, present := values["limit"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return operator.JobListRequest{}, jobsPage{}, errors.New("工作單 limit 必須恰有一個整數值。")
		}
		parsed, parseErr := strconv.Atoi(raw[0])
		if parseErr != nil || parsed < 1 || parsed > operator.MaxJobReadLimit {
			return operator.JobListRequest{}, jobsPage{}, fmt.Errorf("工作單 limit 必須介於 1 與 %d。", operator.MaxJobReadLimit)
		}
		limit = parsed
	}

	request := operator.JobListRequest{
		MachineID: machineID, DeploymentID: deploymentID,
		ResourceKind: resourceKind, ResourceID: resourceID,
		Limit: limit, Cursor: cursor,
	}
	selectedStates := make(map[deploy.JobState]bool, len(values["state"]))
	for _, raw := range values["state"] {
		state := deploy.JobState(raw)
		if raw == "" || raw != strings.TrimSpace(raw) || !deploy.IsKnownJobState(state) || selectedStates[state] {
			return operator.JobListRequest{}, jobsPage{}, fmt.Errorf("工作單 state %q 不合法或重複。", raw)
		}
		selectedStates[state] = true
		request.States = append(request.States, state)
	}
	if resourceID != "" && resourceKind == "" {
		return operator.JobListRequest{}, jobsPage{}, errors.New("工作單 resource_id 必須搭配 resource_kind。")
	}

	view := jobsPage{
		MachineID: machineID, DeploymentID: deploymentID,
		ResourceKind: resourceKind, ResourceID: resourceID, Limit: limit,
		Filtered: machineID != "" || deploymentID != "" || resourceKind != "" ||
			resourceID != "" || len(selectedStates) > 0 || cursor != "",
		States: make([]jobStateOption, 0, len(deploy.AllJobStates)),
	}
	for _, state := range deploy.AllJobStates {
		view.States = append(view.States, jobStateOption{
			Value: string(state), Label: string(state), Selected: selectedStates[state],
		})
	}
	return request, view, nil
}

func webJobListURL(request operator.JobListRequest, cursor string) string {
	values := make(url.Values)
	if request.MachineID != "" {
		values.Set("machine_id", request.MachineID)
	}
	for _, state := range request.States {
		values.Add("state", string(state))
	}
	if request.DeploymentID != "" {
		values.Set("deployment_id", request.DeploymentID)
	}
	if request.ResourceKind != "" {
		values.Set("resource_kind", request.ResourceKind)
	}
	if request.ResourceID != "" {
		values.Set("resource_id", request.ResourceID)
	}
	values.Set("limit", strconv.Itoa(request.Limit))
	values.Set("cursor", cursor)
	return "/jobs?" + values.Encode()
}
