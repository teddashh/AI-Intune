package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
)

const changeWebPageLimit = operator.DefaultChangeReadLimit

type changePageView struct {
	Result operator.ChangeListResult

	MachineID string
	Kinds     []changeKindFilter
	Subject   string
	From      string
	To        string
	Limit     int

	HasFilters       bool
	CoveragePartial  bool
	CoverageMessages []string
	Rows             []changeRowView
	NextHref         string
	FirstPageHref    string
}

type changeKindFilter struct {
	Value    string
	Label    string
	Selected bool
}

type changeRowView struct {
	Item        operator.ChangeItem
	MachineID   string
	MachineHref string
	KindLabel   string
	Semantics   string
	Baseline    string
	Severity    string
	Before      changeEvidenceView
	After       changeEvidenceView
}

type changeEvidenceView struct {
	Unavailable string
	Fields      []changeEvidenceField
}

type changeEvidenceField struct {
	Label string
	Value string
}

// changesPage is only an SSR adapter over operator.Service. Raw Store
// observation payloads never cross this boundary: the page consumes the same
// typed allowlist used by the operator API and CLI.
func (s *Server) changesPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseChangePageRequest(r)
	if err != nil {
		http.Error(w, "Changes filter 或 cursor 不合法", http.StatusBadRequest)
		return
	}

	now := time.Now().UTC()
	result, err := s.operator.ListChangesContext(r.Context(), request, now)
	if err != nil {
		if !writeChangePageReadError(w, err) {
			log.Printf("讀取 Changes page 失敗: %v", err)
			http.Error(w, "讀取變更記錄失敗", http.StatusInternalServerError)
		}
		return
	}

	view := buildChangePage(request, result)
	s.render(w, r, "changes.html", page{
		Title: "變更", Nav: "changes", Now: now.Local().Format("2006-01-02 15:04"), Changes: view,
	})
}

func writeChangePageReadError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, operator.ErrInvalidChangeRead):
		http.Error(w, "Changes filter 或 cursor 不合法", http.StatusBadRequest)
	case errors.Is(err, operator.ErrChangeReadTraversalGone):
		http.Error(w, "Retention 已使這次 Changes 續頁失效；請回第一頁重讀", http.StatusGone)
	case errors.Is(err, operator.ErrChangeReadTooBroad):
		http.Error(w, "Changes 查詢過大；請縮短 window 或增加 machine、kind、subject filter", http.StatusUnprocessableEntity)
	case errors.Is(err, operator.ErrChangeReadBusy):
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Changes reader 正忙；請稍後重試", http.StatusTooManyRequests)
	case errors.Is(err, operator.ErrChangeReadTimedOut):
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Changes 查詢超時；請縮短 window 或增加 filter 後重試", http.StatusServiceUnavailable)
	default:
		return false
	}
	return true
}

func parseChangePageRequest(r *http.Request) (operator.ChangeListRequest, error) {
	if r == nil || r.URL == nil {
		return operator.ChangeListRequest{}, errors.New("missing Changes URL")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.ChangeListRequest{}, errors.New("empty Changes query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.ChangeListRequest{}, errors.New("invalid Changes query encoding")
	}
	allowed := map[string]bool{
		"machine_id": true, "kind": true, "subject": true, "from": true,
		"to": true, "limit": true, "cursor": true,
	}
	for key := range values {
		if !allowed[key] {
			return operator.ChangeListRequest{}, fmt.Errorf("unknown Changes query %q", key)
		}
	}

	request := operator.ChangeListRequest{Limit: changeWebPageLimit}
	request.MachineID, err = changeWebValue(values, "machine_id", 256, true)
	if err != nil {
		return operator.ChangeListRequest{}, err
	}
	request.Subject, err = changeWebValue(values, "subject", 256, true)
	if err != nil {
		return operator.ChangeListRequest{}, err
	}
	request.Cursor, err = changeWebValue(values, "cursor", 4096, false)
	if err != nil {
		return operator.ChangeListRequest{}, err
	}

	if rawKinds, present := values["kind"]; present {
		known := operator.ChangeKinds()
		if len(rawKinds) == 0 || len(rawKinds) > len(known) {
			return operator.ChangeListRequest{}, errors.New("invalid Changes kind filters")
		}
		allowedKinds := make(map[string]bool, len(known))
		for _, kind := range known {
			allowedKinds[kind] = true
		}
		seen := make(map[string]bool, len(rawKinds))
		for _, kind := range rawKinds {
			if kind == "" || kind != strings.TrimSpace(kind) || !utf8.ValidString(kind) ||
				!allowedKinds[kind] || seen[kind] {
				return operator.ChangeListRequest{}, errors.New("Changes kind filters must be canonical and unique")
			}
			seen[kind] = true
		}
		// Canonical order makes every continuation URL and filter digest stable,
		// independent of the checkbox order sent by a client.
		for _, kind := range known {
			if seen[kind] {
				request.Kinds = append(request.Kinds, kind)
			}
		}
	}

	for _, field := range []struct {
		name   string
		target **time.Time
	}{
		{"from", &request.From}, {"to", &request.To},
	} {
		raw, err := changeWebValue(values, field.name, 64, true)
		if err != nil {
			return operator.ChangeListRequest{}, err
		}
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil || parsed.Nanosecond() != 0 || parsed.Format(time.RFC3339) != raw {
			return operator.ChangeListRequest{}, fmt.Errorf("invalid Changes %s time", field.name)
		}
		parsed = parsed.UTC()
		*field.target = &parsed
	}

	if _, present := values["limit"]; present {
		value, err := changeWebValue(values, "limit", 3, false)
		if err != nil {
			return operator.ChangeListRequest{}, err
		}
		request.Limit, err = strconv.Atoi(value)
		if err != nil || request.Limit < 1 || request.Limit > operator.MaxChangeReadLimit ||
			strconv.Itoa(request.Limit) != value {
			return operator.ChangeListRequest{}, errors.New("invalid Changes page limit")
		}
	}
	if err := operator.ValidateChangeListRequest(request); err != nil {
		return operator.ChangeListRequest{}, err
	}
	return request, nil
}

// changeWebValue accepts empty values only for optional native GET form
// inputs. Cursor, enums, and limit remain strict, and every scalar remains
// singular, bounded, canonical UTF-8 without control/format characters.
func changeWebValue(values url.Values, name string, maxBytes int, allowEmpty bool) (string, error) {
	raw, present := values[name]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || (!allowEmpty && raw[0] == "") || raw[0] != strings.TrimSpace(raw[0]) ||
		len(raw[0]) > maxBytes || !utf8.ValidString(raw[0]) {
		return "", fmt.Errorf("invalid Changes query %q", name)
	}
	for _, char := range raw[0] {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return "", fmt.Errorf("invalid Changes query %q", name)
		}
	}
	return raw[0], nil
}

func buildChangePage(request operator.ChangeListRequest, result operator.ChangeListResult) *changePageView {
	selected := make(map[string]bool, len(request.Kinds))
	for _, kind := range request.Kinds {
		selected[kind] = true
	}
	view := &changePageView{
		Result: result, MachineID: request.MachineID, Subject: request.Subject,
		From: changeWebTime(&result.Window.From), To: changeWebTime(&result.Window.To), Limit: request.Limit,
		HasFilters: request.MachineID != "" || len(request.Kinds) > 0 || request.Subject != "" ||
			request.From != nil || request.To != nil || request.Limit != changeWebPageLimit || request.Cursor != "",
	}
	for _, kind := range operator.ChangeKinds() {
		view.Kinds = append(view.Kinds, changeKindFilter{
			Value: kind, Label: changeKindLabel(kind), Selected: selected[kind],
		})
	}
	view.CoveragePartial, view.CoverageMessages = changeCoverageMessages(result.Coverage)
	for _, item := range result.Items {
		view.Rows = append(view.Rows, buildChangeRow(item))
	}

	// A continuation URL names the effective window explicitly. This keeps the
	// visible query and the cursor's frozen window aligned, and gives the first-
	// page recovery link the exact same comparison boundary.
	linkRequest := request
	from, to := result.Window.From.UTC(), result.Window.To.UTC()
	linkRequest.From, linkRequest.To = &from, &to
	if result.NextCursor != nil {
		view.NextHref = changeWebURL(linkRequest, *result.NextCursor)
	}
	if request.Cursor != "" {
		view.FirstPageHref = changeWebURL(linkRequest, "")
	}
	return view
}

func changeWebURL(request operator.ChangeListRequest, cursor string) string {
	values := make(url.Values)
	if request.MachineID != "" {
		values.Set("machine_id", request.MachineID)
	}
	for _, kind := range request.Kinds {
		values.Add("kind", kind)
	}
	if request.Subject != "" {
		values.Set("subject", request.Subject)
	}
	if request.From != nil {
		values.Set("from", changeWebTime(request.From))
	}
	if request.To != nil {
		values.Set("to", changeWebTime(request.To))
	}
	values.Set("limit", strconv.Itoa(request.Limit))
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	return "/reports/changes?" + values.Encode()
}

func changeWebTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func changeKindLabel(kind string) string {
	switch kind {
	case operator.ChangeKindState:
		return "state（Hub 判決）"
	case operator.ChangeKindRegistry:
		return "registry（名冊生命週期）"
	case operator.ChangeKindIdentity:
		return "identity"
	case operator.ChangeKindCredential:
		return "credential"
	case operator.ChangeKindCLITool:
		return "cli_tool"
	case operator.ChangeKindSystemd:
		return "systemd"
	case operator.ChangeKindOpenClaw:
		return "openclaw"
	default:
		return kind
	}
}

func buildChangeRow(item operator.ChangeItem) changeRowView {
	row := changeRowView{
		Item: item, KindLabel: changeKindLabel(item.Kind), Semantics: changeSemanticsLabel(item.Semantics),
		Baseline: changeBaselineLabel(item.BaselineStatus),
		Before:   changeEvidence(item.Kind, item.Before, "無已知基準值"),
		After:    changeEvidence(item.Kind, item.After, "無可安全呈現的變更後值"),
	}
	if item.MachineID != nil {
		row.MachineID = *item.MachineID
		row.MachineHref = "/machines/" + url.PathEscape(*item.MachineID)
	}
	if item.Severity != nil {
		row.Severity = strconv.Itoa(*item.Severity)
	}
	return row
}

func changeSemanticsLabel(value string) string {
	switch value {
	case "transition":
		return "逐筆 transition"
	case "window_comparison":
		return "window 起訖點比較"
	default:
		return value
	}
}

func changeBaselineLabel(value string) string {
	switch value {
	case "known":
		return "基準值已知"
	case "not_registered":
		return "window 起點尚未註冊"
	case "not_observed":
		return "window 起點前尚無觀測"
	case "malformed":
		return "基準證據無法判讀"
	case "possibly_pruned":
		return "基準證據可能已被 retention 清除"
	default:
		return value
	}
}

func changeEvidence(kind string, value *operator.ChangeValue, absent string) changeEvidenceView {
	view := changeEvidenceView{Unavailable: absent}
	if value == nil {
		return view
	}
	addString := func(label string, candidate *string) {
		if candidate == nil {
			return
		}
		shown := *candidate
		if shown == "" {
			shown = "（空字串）"
		}
		view.Fields = append(view.Fields, changeEvidenceField{Label: label, Value: shown})
	}
	addBool := func(label string, candidate *bool) {
		if candidate != nil {
			view.Fields = append(view.Fields, changeEvidenceField{Label: label, Value: strconv.FormatBool(*candidate)})
		}
	}
	addInt := func(label string, candidate *int) {
		if candidate != nil {
			view.Fields = append(view.Fields, changeEvidenceField{Label: label, Value: strconv.Itoa(*candidate)})
		}
	}

	switch kind {
	case operator.ChangeKindState:
		if value.State != nil {
			stateValue := string(*value.State)
			addString("狀態", &stateValue)
		}
	case operator.ChangeKindRegistry:
		addString("名冊生命週期", value.Lifecycle)
	case operator.ChangeKindIdentity:
		addString("OS", value.OS)
		addString("kernel", value.Kernel)
		addString("arch", value.Arch)
		addBool("linger enabled", value.LingerEnabled)
	case operator.ChangeKindCredential:
		if value.Status != nil {
			status := string(*value.Status)
			addString("憑證狀態", &status)
		}
		if value.ExpiresAt != nil {
			expires := value.ExpiresAt.UTC().Format(time.RFC3339)
			addString("到期時間", &expires)
		}
	case operator.ChangeKindCLITool:
		addBool("present", value.Present)
		addBool("on PATH", value.OnPath)
		addString("reported version", value.VersionReported)
		addString("package.json version", value.VersionPackageJSON)
		addBool("version sources disagree", value.VersionSourcesDisagree)
		addString("daemon reach", value.DaemonReach)
		addBool("running/install mismatch", value.RunningInstallMismatch)
	case operator.ChangeKindSystemd:
		if value.Measured != nil && !*value.Measured {
			// ⚠ 這是「沒有量到」的安全型別值，不可落成代表資料不安全的 Unavailable 文案。
			view.Fields = append(view.Fields, changeEvidenceField{Label: "present", Value: "沒有量到"})
		} else {
			addBool("present", value.Present)
		}
		addString("active state", value.ActiveState)
		addString("sub state", value.SubState)
		addInt("restarts", value.Restarts)
	case operator.ChangeKindOpenClaw:
		addBool("present", value.Present)
		addString("CLI version", value.CLIVersion)
		addString("gateway version", value.GatewayVersion)
		addString("upstream version", value.UpstreamVersion)
	}
	if len(view.Fields) == 0 {
		view.Unavailable = "此列沒有可安全呈現的型別值"
	}
	return view
}

func changeCoverageMessages(coverage operator.ChangeCoverage) (bool, []string) {
	incomplete := func(status string) bool {
		return status != "complete" && status != operator.ChangeCoverageNotApplicable
	}
	observationPartial := incomplete(coverage.ObservationHistory) || coverage.ObservationRowsPruned > 0 ||
		coverage.ObservationPrunedBefore != nil || coverage.LastObservationPrunedAt != nil
	partial := observationPartial || incomplete(coverage.RegistryHistory) ||
		incomplete(coverage.StateHistory) ||
		coverage.MalformedTimestampRows > 0 || coverage.UnplaceableTimestampRows > 0 || len(coverage.Issues) > 0
	messages := make([]string, 0, 8+len(coverage.Issues))
	if observationPartial {
		messages = append(messages, "Observation history 不完整；before 值可能已被 retention 清除。")
	}
	if coverage.ObservationRowsPruned > 0 {
		messages = append(messages, fmt.Sprintf("已知有 %d 列 observation evidence 被清除。", coverage.ObservationRowsPruned))
	}
	if coverage.ObservationPrunedBefore != nil {
		messages = append(messages, "Observation retention 已清到 "+changeWebTime(coverage.ObservationPrunedBefore)+"之前。")
	}
	if coverage.LastObservationPrunedAt != nil {
		messages = append(messages, "最後一次 observation prune 發生於 "+changeWebTime(coverage.LastObservationPrunedAt)+"。")
	}
	if incomplete(coverage.RegistryHistory) {
		message := "Registry lifecycle history 在追蹤開始前不完整。"
		if coverage.RegistryHistoryStartedAt != nil {
			message = "Registry lifecycle 只能證明 " + changeWebTime(coverage.RegistryHistoryStartedAt) + " 以後的完整 transition。"
		}
		messages = append(messages, message)
	}
	if incomplete(coverage.StateHistory) {
		message := "State transition history 在追蹤開始前可能遺失同秒覆寫。"
		if coverage.StateHistoryStartedAt != nil {
			message = "State transition ledger 只能保證 " + changeWebTime(coverage.StateHistoryStartedAt) + " 以後沒有同秒覆寫缺口。"
		}
		messages = append(messages, message)
	}
	if coverage.MalformedTimestampRows > 0 {
		messages = append(messages, fmt.Sprintf("有 %d 列時間證據無法完整判讀。", coverage.MalformedTimestampRows))
	}
	if coverage.UnplaceableTimestampRows > 0 {
		messages = append(messages, fmt.Sprintf("其中 %d 列無法可靠地放入或排除於這個 window。", coverage.UnplaceableTimestampRows))
	}
	for _, issue := range coverage.Issues {
		messages = append(messages, "coverage issue: "+issue)
	}
	if partial && len(messages) == 0 {
		messages = append(messages, "Coverage 不完整；這個 window 不能當成完整歷史。")
	}
	return partial, messages
}
