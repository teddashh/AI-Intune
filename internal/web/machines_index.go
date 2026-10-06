package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

type machineIndexPageView struct {
	Result operator.MachineListResult

	MachineID   string
	DisplayName string
	States      []machineStateFilter
	Lifecycle   string
	Reporting   string
	Channel     string
	Limit       int

	HasFilters    bool
	NextHref      string
	FirstPageHref string
	CurrentLabel  string
	AssignedUsers map[string]string
}

type machineStateFilter struct {
	Value    string
	Label    string
	Selected bool
}

func parseMachineIndexRequest(r *http.Request) (operator.MachineListRequest, error) {
	if r == nil || r.URL == nil {
		return operator.MachineListRequest{}, errors.New("missing machine index URL")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.MachineListRequest{}, errors.New("empty machine index query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.MachineListRequest{}, errors.New("invalid machine index query")
	}
	allowed := map[string]bool{
		"machine_id": true, "display_name": true, "state": true, "lifecycle": true,
		"reporting": true, "channel": true, "limit": true,
		"cursor": true, "machines": true, "section": true,
	}
	for key := range values {
		if !allowed[key] {
			return operator.MachineListRequest{}, fmt.Errorf("unknown machine index query %q", key)
		}
	}
	request := operator.MachineListRequest{Limit: operator.DefaultMachineReadLimit}
	for _, field := range []struct {
		name   string
		max    int
		target *string
	}{
		{"machine_id", 256, &request.MachineID}, {"display_name", 256, &request.DisplayName},
	} {
		value, err := optionalMachineIndexTextValue(values, field.name, field.max)
		if err != nil {
			return operator.MachineListRequest{}, err
		}
		*field.target = value
	}
	request.Cursor, err = oneMachineIndexValue(values, "cursor", 2048)
	if err != nil {
		return operator.MachineListRequest{}, err
	}
	if raw, present := values["state"]; present {
		if len(raw) == 0 || len(raw) > len(state.AllStates) {
			return operator.MachineListRequest{}, errors.New("invalid machine state filters")
		}
		seen := make(map[state.State]bool, len(raw))
		for _, value := range raw {
			candidate := state.State(value)
			known := false
			for _, allowedState := range state.AllStates {
				known = known || candidate == allowedState
			}
			if !known || seen[candidate] {
				return operator.MachineListRequest{}, errors.New("machine state filters must be canonical and unique")
			}
			seen[candidate] = true
			request.States = append(request.States, candidate)
		}
	}
	for _, field := range []struct {
		name string
		set  func(string)
	}{
		{"lifecycle", func(value string) { request.Lifecycle = operator.MachineLifecycleFilter(value) }},
		{"reporting", func(value string) { request.Reporting = operator.MachineReportingFilter(value) }},
		{"channel", func(value string) { request.Channel = operator.MachineChannelFilter(value) }},
	} {
		value, err := oneMachineIndexValue(values, field.name, 16)
		if err != nil {
			return operator.MachineListRequest{}, err
		}
		field.set(value)
	}
	if value, err := oneMachineIndexValue(values, "limit", 3); err != nil {
		return operator.MachineListRequest{}, err
	} else if value != "" {
		request.Limit, err = strconv.Atoi(value)
		if err != nil || request.Limit < 1 || request.Limit > operator.MaxMachineReadLimit ||
			strconv.Itoa(request.Limit) != value {
			return operator.MachineListRequest{}, errors.New("invalid machine page limit")
		}
	}
	if section, err := oneMachineIndexValue(values, "section", 32); err != nil {
		return operator.MachineListRequest{}, err
	} else if section != "" && section != "fleet-kpis" && section != "machines" && section != "findings" {
		return operator.MachineListRequest{}, errors.New("invalid machine page section")
	}

	legacy, err := oneMachineIndexValue(values, "machines", 16)
	if err != nil {
		return operator.MachineListRequest{}, err
	}
	if legacy != "" {
		if len(request.States) > 0 || request.Lifecycle != "" || request.Reporting != "" ||
			request.Channel != "" || request.MachineID != "" || request.DisplayName != "" || request.Cursor != "" {
			return operator.MachineListRequest{}, errors.New("legacy machine filter cannot be mixed with canonical filters")
		}
		request.Lifecycle = operator.MachineLifecycleActive
		switch legacy {
		case "managed":
		case "reporting":
			request.Reporting = operator.MachineReportingTrue
		case "attention":
			for _, candidate := range state.AllStates {
				if candidate != state.Online {
					request.States = append(request.States, candidate)
				}
			}
		default:
			return operator.MachineListRequest{}, errors.New("unknown legacy machine filter")
		}
	}
	if err := operator.ValidateMachineListRequest(request); err != nil {
		return operator.MachineListRequest{}, err
	}
	return request, nil
}

func oneMachineIndexValue(values url.Values, name string, maxBytes int) (string, error) {
	return machineIndexValue(values, name, maxBytes, false)
}

// optionalMachineIndexTextValue accepts the empty value produced by the two
// optional text inputs in the native GET form.  It deliberately remains strict
// about duplicate values and about non-empty whitespace/control input; cursor
// and enum fields continue to use oneMachineIndexValue and reject empty values.
func optionalMachineIndexTextValue(values url.Values, name string, maxBytes int) (string, error) {
	return machineIndexValue(values, name, maxBytes, true)
}

func machineIndexValue(values url.Values, name string, maxBytes int, allowEmpty bool) (string, error) {
	raw, present := values[name]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || (!allowEmpty && raw[0] == "") || raw[0] != strings.TrimSpace(raw[0]) ||
		len(raw[0]) > maxBytes || !utf8.ValidString(raw[0]) {
		return "", fmt.Errorf("invalid machine query %q", name)
	}
	for _, char := range raw[0] {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return "", fmt.Errorf("invalid machine query %q", name)
		}
	}
	return raw[0], nil
}

func buildMachineIndexPage(request operator.MachineListRequest, result operator.MachineListResult) *machineIndexPageView {
	selected := make(map[state.State]bool, len(request.States))
	for _, candidate := range request.States {
		selected[candidate] = true
	}
	view := &machineIndexPageView{
		Result: result, MachineID: request.MachineID, DisplayName: request.DisplayName,
		Lifecycle: string(request.Lifecycle),
		Reporting: string(request.Reporting), Channel: string(request.Channel), Limit: request.Limit,
		CurrentLabel: "所有機器",
	}
	view.AssignedUsers = map[string]string{}
	if overview, ok := result.StoreOverview(); ok {
		for _, row := range overview.Machines {
			view.AssignedUsers[row.Machine.MachineID] = assignedUserWebLabel(row.Machine.AssignedUserID, row.Machine.AssignedUserLogin)
		}
		for _, m := range overview.Retired {
			view.AssignedUsers[m.MachineID] = assignedUserWebLabel(m.AssignedUserID, m.AssignedUserLogin)
		}
	}
	if view.Lifecycle == "" {
		view.Lifecycle = string(operator.MachineLifecycleAny)
	}
	if view.Reporting == "" {
		view.Reporting = string(operator.MachineReportingAny)
	}
	if view.Channel == "" {
		view.Channel = string(operator.MachineChannelAny)
	}
	for _, candidate := range state.AllStates {
		view.States = append(view.States, machineStateFilter{
			Value: string(candidate), Label: string(candidate), Selected: selected[candidate],
		})
	}
	view.HasFilters = request.MachineID != "" || request.DisplayName != "" || len(request.States) > 0 ||
		(request.Lifecycle != "" && request.Lifecycle != operator.MachineLifecycleAny) ||
		(request.Reporting != "" && request.Reporting != operator.MachineReportingAny) ||
		(request.Channel != "" && request.Channel != operator.MachineChannelAny)
	if request.Lifecycle == operator.MachineLifecycleRetired {
		view.CurrentLabel = "已退役"
	}
	if result.NextCursor != nil {
		view.NextHref = machineIndexURL(request, *result.NextCursor)
	}
	if request.Cursor != "" {
		view.FirstPageHref = machineIndexURL(request, "")
	}
	return view
}

func machineIndexURL(request operator.MachineListRequest, cursor string) string {
	values := make(url.Values)
	if request.MachineID != "" {
		values.Set("machine_id", request.MachineID)
	}
	if request.DisplayName != "" {
		values.Set("display_name", request.DisplayName)
	}
	for _, candidate := range request.States {
		values.Add("state", string(candidate))
	}
	for _, field := range []struct{ name, value, defaultValue string }{
		{"lifecycle", string(request.Lifecycle), string(operator.MachineLifecycleAny)},
		{"reporting", string(request.Reporting), string(operator.MachineReportingAny)},
		{"channel", string(request.Channel), string(operator.MachineChannelAny)},
	} {
		if field.value != "" && field.value != field.defaultValue {
			values.Set(field.name, field.value)
		}
	}
	values.Set("limit", strconv.Itoa(request.Limit))
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	return "/machines?" + values.Encode() + "#machines"
}

// machineDisplayNameProjection is built from MachineListResult.StoreOverview,
// whose names have already passed through the same safe projection as the v2
// DTO.  The overview is the whole creation-ceiling fleet, not merely the
// filtered/paginated Items slice, so ancillary dashboard sections can use it
// without falling back to raw Store display names for off-page machines.
func machineDisplayNameProjection(overview store.Overview) map[string]string {
	names := make(map[string]string, len(overview.Machines)+len(overview.Retired))
	for _, machine := range overview.Machines {
		names[machine.MachineID] = machine.DisplayName
	}
	for _, machine := range overview.Retired {
		names[machine.MachineID] = machine.DisplayName
	}
	return names
}

func projectedMachineDisplayName(names map[string]string, machineID string) string {
	if name := names[machineID]; name != "" {
		return name
	}
	// Ancillary rows should normally always join a registry row represented in
	// the overview.  If corrupt legacy data violates that invariant, machine_id
	// is the validated safe identity; never fall back to the raw display name.
	if machineID != "" {
		return machineID
	}
	return "(unnamed machine)"
}

func projectDeadSignalDisplayNames(rows []store.DeadSignal, names map[string]string) []store.DeadSignal {
	result := append([]store.DeadSignal(nil), rows...)
	for i := range result {
		result[i].DisplayName = projectedMachineDisplayName(names, result[i].MachineID)
		result[i].Reason = safeWebDerivedText(result[i].Reason, nil)
	}
	return result
}

func projectDoubleAgentDisplayNames(rows []store.DoubleAgent, names map[string]string) []store.DoubleAgent {
	result := append([]store.DoubleAgent(nil), rows...)
	for i := range result {
		rawName := result[i].DisplayName
		safeName := projectedMachineDisplayName(names, result[i].MachineID)
		// Store's explanatory sentence starts with, and therefore embeds, the
		// registry display name. Replacing the field alone would leave a second
		// copy of unsafe control/format characters in Reason.
		if rawName != safeName {
			switch {
			case strings.TrimSpace(rawName) == "" && strings.HasPrefix(result[i].Reason, rawName):
				suffix := strings.TrimLeftFunc(strings.TrimPrefix(result[i].Reason, rawName), unicode.IsSpace)
				result[i].Reason = safeName
				if suffix != "" {
					result[i].Reason += " " + suffix
				}
			case strings.HasPrefix(result[i].Reason, rawName):
				result[i].Reason = safeName + strings.TrimPrefix(result[i].Reason, rawName)
			case strings.TrimSpace(rawName) != "":
				result[i].Reason = strings.ReplaceAll(result[i].Reason, rawName, safeName)
			}
		}
		result[i].Reason = safeWebDerivedText(result[i].Reason, nil)
		result[i].DisplayName = safeName
	}
	return result
}

type webDisplayReplacement struct {
	raw  string
	safe string
}

func safeWebDerivedText(value string, replacements []webDisplayReplacement) string {
	for _, replacement := range replacements {
		value = strings.ReplaceAll(value, replacement.raw, replacement.safe)
	}
	value = strings.ToValidUTF8(value, "�")
	var out strings.Builder
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			out.WriteRune('�')
			continue
		}
		out.WriteRune(char)
	}
	return out.String()
}

func projectDeploymentDisplayNames(views []store.DeploymentView, names map[string]string) []store.DeploymentView {
	result := append([]store.DeploymentView(nil), views...)
	for i := range result {
		result[i].Targets = append([]store.DeploymentTarget(nil), views[i].Targets...)
		replacements := make([]webDisplayReplacement, 0, len(result[i].Targets))
		for j := range result[i].Targets {
			rawName := result[i].Targets[j].DisplayName
			safeName := projectedMachineDisplayName(names, result[i].Targets[j].MachineID)
			result[i].Targets[j].DisplayName = safeName
			if rawName != safeName && strings.TrimSpace(rawName) != "" {
				replacements = append(replacements, webDisplayReplacement{raw: rawName, safe: safeName})
			}
		}
		sort.SliceStable(replacements, func(a, b int) bool {
			return len(replacements[a].raw) > len(replacements[b].raw)
		})
		if views[i].BoundaryPause != nil {
			pause := *views[i].BoundaryPause
			pause.Reason = safeWebDerivedText(pause.Reason, replacements)
			result[i].BoundaryPause = &pause
		}
	}
	return result
}

func projectHubEventPresentation(rows []store.HubEvent) []store.HubEvent {
	result := append([]store.HubEvent(nil), rows...)
	for i := range result {
		result[i].Detail = safeWebDerivedText(result[i].Detail, nil)
	}
	return result
}

func projectTailnetDisplayNames(result tailnet.Result, names map[string]string) tailnet.Result {
	result.RetiredButOnline = append([]tailnet.RetiredPeer(nil), result.RetiredButOnline...)
	for i := range result.RetiredButOnline {
		result.RetiredButOnline[i].DisplayName = projectedMachineDisplayName(
			names, result.RetiredButOnline[i].MachineID,
		)
	}
	return result
}
