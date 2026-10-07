package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

type repeatedMachineStates []string

func (values *repeatedMachineStates) String() string { return strings.Join(*values, ",") }
func (values *repeatedMachineStates) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func runMachinesCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runMachinesCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runMachinesCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	action := "list"
	if len(argv) > 0 && (argv[0] == "list" || argv[0] == "show" || argv[0] == "evidence") {
		action, argv = argv[0], argv[1:]
	}
	fs := flag.NewFlagSet("machines "+action, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub machines list|show|evidence [options]")
		fmt.Fprintln(errOut, "       clawctl-hub machines [list] [filters] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "       clawctl-hub machines show [--json] [--hub-url URL | --db PATH] <machine-id>")
		fmt.Fprintln(errOut, "       clawctl-hub machines evidence [--limit N] [--json] [--hub-url URL | --db PATH] <machine-id>")
		fmt.Fprintln(errOut, "  Normal mode uses HTTP operator API; --db is only for fenced break-glass when Hub is completely stopped.")
		fmt.Fprintln(errOut, "  list is paginated by registry creation ceiling.")
		fs.PrintDefaults()
	}
	var hubURL, dbPath auditStringFlag
	var jsonOutput auditBoolFlag
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (auto-discovered if omitted)")
	fs.Var(&dbPath, "db", "Existing SQLite file path for stopped-service direct DB break-glass")
	fs.Var(&jsonOutput, "json", "Output stable operator JSON DTO")
	request := operator.MachineListRequest{}
	var machineIDFilter, displayName, lifecycle, reporting, channel, cursor auditStringFlag
	limit := auditIntFlag{value: operator.DefaultMachineReadLimit}
	var states repeatedMachineStates
	if action == "list" {
		fs.Var(&machineIDFilter, "machine-id", "Exact match for machine_id")
		fs.Var(&displayName, "display-name", "Exact match for safe display_name")
		fs.Var(&states, "state", "Canonical state; repeatable")
		fs.Var(&lifecycle, "lifecycle", "any, active, or retired")
		fs.Var(&reporting, "reporting", "any, true, false, or unknown")
		fs.Var(&channel, "channel", "any, none, canary, or stable")
		fs.Var(&limit, "limit", "Maximum machines per page (1..100)")
		fs.Var(&cursor, "cursor", "Opaque next cursor returned from previous page")
	} else if action == "evidence" {
		limit = auditIntFlag{}
		fs.Var(&limit, "limit", "Maximum items per evidence section (1..100)")
	}
	parseArgs := argv
	if action == "evidence" && len(parseArgs) > 0 && !strings.HasPrefix(parseArgs[0], "-") {
		parseArgs = append(append([]string(nil), parseArgs[1:]...), parseArgs[0])
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if hubURL.set && dbPath.set {
		return errors.New("machines: --hub-url (HTTP mode) and --db (direct mode) cannot both be specified")
	}
	if hubURL.set && strings.TrimSpace(hubURL.value) == "" {
		return errors.New("machines: --hub-url cannot be empty")
	}
	if dbPath.set && strings.TrimSpace(dbPath.value) == "" {
		return errors.New("machines: --db cannot be empty")
	}

	machineID := ""
	switch action {
	case "list":
		if fs.NArg() != 0 {
			return fmt.Errorf("machines list: unexpected positional arguments: %q", strings.Join(fs.Args(), " "))
		}
		for name, value := range map[string]auditStringFlag{
			"machine-id": machineIDFilter, "display-name": displayName, "lifecycle": lifecycle,
			"reporting": reporting, "channel": channel, "cursor": cursor,
		} {
			if value.set && value.value == "" {
				return fmt.Errorf("machines list: --%s cannot be empty", name)
			}
		}
		if limit.set && (limit.value < 1 || limit.value > operator.MaxMachineReadLimit) {
			return fmt.Errorf("machines list: --limit must be between 1 and %d", operator.MaxMachineReadLimit)
		}
		request = operator.MachineListRequest{
			MachineID: machineIDFilter.value, DisplayName: displayName.value,
			Lifecycle: operator.MachineLifecycleFilter(lifecycle.value),
			Reporting: operator.MachineReportingFilter(reporting.value),
			Channel:   operator.MachineChannelFilter(channel.value),
			Limit:     limit.value, Cursor: cursor.value,
		}
		for _, value := range states {
			request.States = append(request.States, state.State(value))
		}
		if err := operator.ValidateMachineListRequest(request); err != nil {
			return fmt.Errorf("machines list: %w", err)
		}
	case "show":
		if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
			return errors.New("machines show: must provide a machine-id")
		}
		machineID = fs.Arg(0)
	case "evidence":
		if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
			return errors.New("machines evidence: must provide a machine-id")
		}
		if limit.set && (limit.value < 1 || limit.value > store.MaxMachineEvidencePageSize) {
			return fmt.Errorf("machines evidence: --limit must be between 1 and %d", store.MaxMachineEvidencePageSize)
		}
		machineID = fs.Arg(0)
		request.Limit = limit.value
		if err := operator.ValidateMachineEvidenceRequest(operator.MachineEvidenceRequest{
			MachineID: machineID, Limit: limit.value,
		}); err != nil {
			return fmt.Errorf("machines evidence: %w", err)
		}
	}

	if dbPath.set {
		return runMachinesDirect(ctx, action, machineID, request, dbPath.value, jsonOutput.value, out, deps)
	}
	client, err := machinesHTTPClient(hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	if action == "show" {
		result, err := client.Machine(ctx, machineID)
		if err != nil {
			return fmt.Errorf("failed to read machine detail (HTTP operator API): %w", err)
		}
		if jsonOutput.value {
			return writeMachineDetail(out, result, true, "HTTP operator API", nil)
		}
		assigned, err := client.GetMachineAssignedUser(ctx, machineID)
		if err != nil {
			return writeMachineDetail(out, result, false, "HTTP operator API", nil)
		}
		return writeMachineDetail(out, result, false, "HTTP operator API", &assigned)
	}
	if action == "evidence" {
		result, err := client.MachineEvidence(ctx, machineID, limit.value)
		if err != nil {
			return fmt.Errorf("failed to read machine evidence (HTTP operator API): %w", err)
		}
		return writeMachineEvidence(out, result, jsonOutput.value, "HTTP operator API")
	}
	result, err := client.ListMachines(ctx, request)
	if err != nil {
		return fmt.Errorf("failed to read machine list (HTTP operator API): %w", err)
	}
	return writeMachineList(out, result, jsonOutput.value, "HTTP operator API")
}

func machinesHTTPClient(explicitURL string, explicit bool, deps machineCommandDeps) (*operatorclient.Client, error) {
	if explicit {
		if deps.newOperatorClient == nil {
			return nil, errors.New("machines: operator HTTP client not initialized")
		}
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, fmt.Errorf("machines: failed to construct HTTP operator client: %w", err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, errors.New("machines: Hub discovery not initialized")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("machines: unable to discover Hub: %w", err)
	}
	if deps.newOperatorClient == nil {
		return nil, errors.New("machines: operator HTTP client not initialized")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("machines: failed to construct discovered HTTP operator client: %w", err)
	}
	return client, nil
}

func runMachinesDirect(ctx context.Context, action, machineID string, request operator.MachineListRequest,
	dbPath string, jsonOutput bool,
	out io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "machines "+action, dbPath, deps, func(st *store.Store) error {
		// This is the sole direct judgement path. Normal HTTP mode uses the
		// already-configured service, while break-glass must load the identical
		// expectation set before it derives State and structured findings.
		loadExpectations(st)
		if _, err := st.CurrentWorkloadPolicyToken("operator-machine-read-policy-proof"); err != nil {
			return fmt.Errorf("machines %s: shell CLAWCTL_EXPECTATIONS does not match Hub last published workload policy; refusing direct DB judgement: %w", action, err)
		}
		service := operator.New(st)
		evaluatedAt := time.Now().UTC()
		if action == "show" {
			result, err := service.MachineDetail(machineID, evaluatedAt)
			if err != nil {
				return fmt.Errorf("failed to read machine detail (direct DB operator service): %w", err)
			}
			if jsonOutput {
				return writeMachineDetail(out, result, true, "direct DB operator service", nil)
			}
			assigned, err := service.MachineAssignedUser(machineID)
			if err != nil {
				return writeMachineDetail(out, result, false, "direct DB operator service", nil)
			}
			return writeMachineDetail(out, result, false, "direct DB operator service", &operatorclient.MachineAssignedUserResponse{UserID: assigned.UserID, UserLogin: assigned.UserLogin, Revision: assigned.Revision})
		}
		if action == "evidence" {
			result, err := service.MachineEvidence(operator.MachineEvidenceRequest{
				MachineID: machineID, Limit: request.Limit,
			}, evaluatedAt)
			if err != nil {
				return fmt.Errorf("failed to read machine evidence (direct DB operator service): %w", err)
			}
			return writeMachineEvidence(out, result, jsonOutput, "direct DB operator service")
		}
		result, err := service.ListMachinesPage(request, evaluatedAt)
		if err != nil {
			return fmt.Errorf("failed to read machine list (direct DB operator service): %w", err)
		}
		return writeMachineList(out, result, jsonOutput, "direct DB operator service")
	})
}

func writeMachineList(out io.Writer, result operator.MachineListResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeMachinesJSON(out, result)
	}
	if _, err := fmt.Fprintf(out,
		"%s; Hub evaluated at %s; consistency %s; registry creation ceiling %d.\n"+
			"filter matched %d / roster visible %d machines (active %d, retired %d); denominator %d machines, %d machines reporting.\n\n",
		source, result.EvaluatedAt.Format(time.RFC3339Nano), result.Consistency, result.CreationCeiling,
		result.MatchedTotal, result.Total, result.Active, result.Retired, result.Expected, result.Reporting); err != nil {
		return err
	}
	for _, machine := range result.Items {
		stateLabel := "Retired (health status not evaluated)"
		if machine.State != nil {
			stateLabel = string(*machine.State)
		}
		if _, err := fmt.Fprintf(out, "%-18s %-18s\n%-18s %s\n",
			terminalSafe(machine.DisplayName), stateLabel, "", terminalSafe(machine.MachineID)); err != nil {
			return err
		}
	}
	if result.NextCursor != nil {
		// Service/client validation guarantees canonical base64url. Keep it raw so
		// an operator can copy the value directly into --cursor without stripping
		// presentation quotes.
		_, err := fmt.Fprintf(out, "next cursor: %s\n", *result.NextCursor)
		return err
	}
	return nil
}

func writeMachineDetail(out io.Writer, result operator.MachineDetailResult, jsonOutput bool, source string, assigned *operatorclient.MachineAssignedUserResponse) error {
	if jsonOutput {
		return writeMachinesJSON(out, result)
	}
	machine := result.Item
	stateLabel := "Retired (health status not evaluated)"
	if machine.State != nil {
		stateLabel = string(*machine.State)
	}
	channel := "unassigned"
	if machine.Channel != nil {
		channel = terminalSafe(*machine.Channel)
	}
	assignedLabel := "unavailable"
	if assigned != nil {
		assignedLabel = fmt.Sprintf("%s (revision %d)", assignedUserCLILabel(assigned.UserID, assigned.UserLogin), assigned.Revision)
	}
	reporting := "unknown (retired not evaluated)"
	if machine.Reporting != nil {
		reporting = fmt.Sprintf("%t", *machine.Reporting)
	}
	if _, err := fmt.Fprintf(out,
		"%s; Hub evaluated at %s.\n%s  %s\nmachine_id: %s\nexpected: %t\nchannel: %s (revision %d)\nassigned user: %s\nreporting: %s\nlast check-in: %s\nlast observation: %s\n",
		source, result.EvaluatedAt.Format(time.RFC3339Nano), terminalSafe(machine.DisplayName), stateLabel,
		terminalSafe(machine.MachineID), machine.Expected, channel, machine.ChannelRevision, assignedLabel, reporting,
		machineReadTime(machine.LastCheckinReceivedAt), machineReadTime(machine.LastObservationReceivedAt)); err != nil {
		return err
	}
	j := result.Judgement
	if _, err := fmt.Fprintf(out,
		"judgement: state=%s affects_fleet_state=%t\njudgement_reason: %s\nfindings: total=%d invalid=%d truncated=%t\n",
		terminalSafe(string(j.State)), j.AffectsFleetState, terminalSafe(j.Reason.Text),
		j.Findings.Total, j.Findings.Invalid, j.Findings.Truncated); err != nil {
		return err
	}
	for _, finding := range j.Findings.Items {
		if _, err := fmt.Fprintf(out, "finding: kind=%s severity=%d advisory=%t message=%s\n",
			terminalSafe(finding.Kind), finding.Severity, finding.Advisory,
			terminalSafe(finding.Message.Text)); err != nil {
			return err
		}
	}
	e := result.Expectations
	if _, err := fmt.Fprintf(out,
		"expectations: configured=%t read_failed=%t total=%d invalid=%d truncated=%t error=%s\n",
		e.Configured, e.ReadFailed, e.Rules.Total, e.Rules.Invalid, e.Rules.Truncated,
		machineEvidenceOptionalText(e.Error)); err != nil {
		return err
	}
	for _, rule := range e.Rules.Items {
		if _, err := fmt.Fprintf(out,
			"expectation: unit=%s artifact=%s max_age_seconds=%d why=%s\n"+
				"artifact_observation: observed=%t decoded=%t invalid=%t read_failed=%t exists=%s modified_at=%s error=%s\n",
			terminalSafe(rule.Unit.Text), terminalSafe(rule.Artifact.Text), rule.MaxAgeSeconds,
			terminalSafe(rule.Why.Text), rule.Observation.Observed, rule.Observation.Decoded,
			rule.Observation.Invalid, rule.Observation.ReadFailed, optionalMachineBool(rule.Observation.Exists),
			machineReadTime(rule.Observation.ModifiedAt), machineEvidenceOptionalText(rule.Observation.Error)); err != nil {
			return err
		}
		if rule.Events == nil {
			continue
		}
		events := rule.Events
		if _, err := fmt.Fprintf(out,
			"events: window_seconds=%d observed=%t decoded=%t invalid=%t read_failed=%t partial=%t covered_from=%s malformed=%d error=%s\n"+
				"event_failure_types: total=%d invalid=%d truncated=%t\n",
			events.WindowSeconds, events.Observed, events.Decoded, events.Invalid, events.ReadFailed,
			events.Partial, machineReadTime(events.CoveredFrom), events.Malformed,
			machineEvidenceOptionalText(events.Error), events.FailureTypes.Total,
			events.FailureTypes.Invalid, events.FailureTypes.Truncated); err != nil {
			return err
		}
		for _, failureType := range events.FailureTypes.Items {
			if _, err := fmt.Fprintf(out, "event_failure_type: %s\n", terminalSafe(failureType.Text)); err != nil {
				return err
			}
		}
		for _, page := range []struct {
			name string
			data operator.MachineEventCountPage
		}{{"declared", events.Declared}, {"undeclared", events.Undeclared}} {
			if _, err := fmt.Fprintf(out, "event_%s: total=%d invalid=%d truncated=%t\n",
				page.name, page.data.Total, page.data.Invalid, page.data.Truncated); err != nil {
				return err
			}
			for _, count := range page.data.Items {
				if _, err := fmt.Fprintf(out, "event_%s_count: type=%s count=%d last_at=%s\n",
					page.name, terminalSafe(count.Type.Text), count.Count, machineReadTime(count.LastAt)); err != nil {
					return err
				}
			}
		}
	}
	d := result.Disclosure
	if _, err := fmt.Fprintf(out,
		"observer_producer: kind=%s authority=%s\nindependent_verifier: %t\n"+
			"liveness_uses_received_at: %t\nagent_sent_at_is_liveness: %t\n"+
			"host_identity_included: %t\ntailscale_ip_included: %t\n"+
			"connect_coordinates_excluded: %t\npending_enrollment_excluded: %t\n"+
			"expectation_display_definitions_included: %t\nexpectation_config_path_field_excluded: %t\n"+
			"expectation_parser_fields_excluded: %t\nartifact_paths_included: %t\n"+
			"artifact_content_inspected: %t\nartifact_freshness_is_work_outcome: %t\n"+
			"event_failure_types_operator_declared: %t\nevent_content_keyword_scanning: %t\n"+
			"expectation_text_path_redacted_by_hub: %t\nexpectation_text_secret_redacted_by_hub: %t\n"+
			"judgement_derived_by_hub: %t\n"+
			"judgement_may_use_unverified_input: %t\njudgement_text_parsed_by_hub: %t\n"+
			"judgement_text_path_redacted_by_hub: %t\njudgement_text_secret_redacted_by_hub: %t\n"+
			"known_secret_fields_excluded: %t\nstate_reason_parsed_by_hub: %t\n"+
			"state_reason_path_redacted_by_hub: %t\ncomplete_history_claimed: %t\n"+
			"identity_hints_non_expiring: %t\ncheckin_window_seconds: %d\ncheckin_limit: %d\n"+
			"state_history_limit: %d\nidentity_hint_limit: %d\nfinding_limit: %d\n"+
			"expectation_limit: %d\nevent_type_limit: %d\nmax_text_bytes: %d\n",
		terminalSafe(d.ObserverProducer.Kind), terminalSafe(d.ObserverProducer.Authority), d.IndependentVerifier,
		d.LivenessUsesReceivedAt, d.AgentSentAtIsLiveness, d.HostIdentityIncluded, d.TailscaleIPIncluded,
		d.ConnectCoordinatesExcluded, d.PendingEnrollmentExcluded,
		d.ExpectationDisplayDefinitionsIncluded, d.ExpectationConfigPathFieldExcluded,
		d.ExpectationParserFieldsExcluded, d.ArtifactPathsIncluded, d.ArtifactContentInspected,
		d.ArtifactFreshnessIsWorkOutcome, d.EventFailureTypesOperatorDeclared, d.EventContentKeywordScanning,
		d.ExpectationTextPathRedacted, d.ExpectationTextSecretRedacted,
		d.JudgementDerivedByHub, d.JudgementMayUseUnverifiedInput, d.JudgementTextParsedByHub,
		d.JudgementTextPathRedacted, d.JudgementTextSecretRedacted, d.KnownSecretFieldsExcluded,
		d.StateReasonParsedByHub, d.StateReasonPathRedactedByHub, d.CompleteHistoryClaimed,
		d.IdentityHintsNonExpiring, d.CheckinWindowSeconds, d.CheckinLimit, d.StateHistoryLimit,
		d.IdentityHintLimit, d.FindingLimit, d.ExpectationLimit, d.EventTypeLimit, d.MaxTextBytes); err != nil {
		return err
	}
	m := result.Monitor
	if _, err := fmt.Fprintf(out,
		"\nmonitor: ever_checked_in=%t interval_seconds=%d clock_skew_seconds=%s\n"+
			"agent_version: %s\ndistinct_agent_starts_1h: %s\ndistinct_boot_ids_1h: %s\n"+
			"agent_unit_n_restarts: %s\ncheckins: rows_seen=%d invalid=%d truncated=%t\n",
		m.EverCheckedIn, m.CheckinIntervalSeconds, optionalMachineInt64(m.ClockSkewSeconds),
		machineEvidenceOptionalText(m.AgentVersion), optionalMachineInt(m.DistinctAgentStarts1h),
		optionalMachineInt(m.DistinctBootIDs1h), optionalMachineInt(m.AgentUnitNRestarts),
		result.Checkins.RowsSeen, result.Checkins.Invalid, result.Checkins.Truncated); err != nil {
		return err
	}
	if count := len(result.Checkins.Items); count > 0 {
		latest := result.Checkins.Items[count-1]
		if _, err := fmt.Fprintf(out,
			"latest_checkin: sent_at=%s received_at=%s agent_version=%s boot_id=%s agent_seq=%s uptime_seconds=%s\n",
			latest.SentAt.UTC().Format(time.RFC3339Nano), latest.ReceivedAt.UTC().Format(time.RFC3339Nano),
			machineEvidenceOptionalText(latest.AgentVersion), machineEvidenceOptionalText(latest.BootID),
			optionalMachineInt64(latest.AgentSeq), optionalMachineInt64(latest.UptimeSeconds)); err != nil {
			return err
		}
	}
	i := result.Identity
	if _, err := fmt.Fprintf(out, "\nidentity: observed=%t decoded=%t hints=%d invalid=%d truncated=%t\n",
		i.Observed, i.Decoded, result.IdentityHints.Total, result.IdentityHints.Invalid,
		result.IdentityHints.Truncated); err != nil {
		return err
	}
	if i.Value != nil {
		var lingerEnabled *bool
		if i.Value.LingerMeasured {
			lingerEnabled = &i.Value.LingerEnabled
		}
		if _, err := fmt.Fprintf(out,
			"hostname: %s\nos: %s\nkernel: %s\narch: %s\nunix_user: %s\nmachine_id_hint: %s\nboot_id: %s\ntailscale_ip: %s\nlinger_enabled: %s\n",
			terminalSafe(i.Value.Hostname.Text), terminalSafe(i.Value.OS.Text), terminalSafe(i.Value.Kernel.Text),
			terminalSafe(i.Value.Arch.Text), terminalSafe(i.Value.UnixUser.Text),
			terminalSafe(i.Value.MachineIDHint.Text), terminalSafe(i.Value.BootID.Text),
			machineEvidenceOptionalText(i.Value.TailscaleIP), optionalMachineBool(lingerEnabled)); err != nil {
			return err
		}
	}
	for _, hint := range result.IdentityHints.Items {
		if _, err := fmt.Fprintf(out,
			"identity_hint: value=%s observations=%d registry_declared=%t first_seen=%s last_seen=%s\n",
			terminalSafe(hint.Hint.Text), hint.ObservationCount, hint.RegistryDeclared,
			machineReadTime(hint.FirstSeenAt), machineReadTime(hint.LastSeenAt)); err != nil {
			return err
		}
	}
	r := result.Resources
	if _, err := fmt.Fprintf(out, "\nresources: observed=%t decoded=%t invalid=%t\n", r.Observed, r.Decoded, r.Invalid); err != nil {
		return err
	}
	if r.Value != nil {
		var memAvailableBytes, memTotalBytes *int64
		if r.Value.MemMeasured {
			memAvailableBytes = &r.Value.MemAvailableBytes
			memTotalBytes = &r.Value.MemTotalBytes
		}
		if _, err := fmt.Fprintf(out,
			"disk_free_bytes: %d\ndisk_total_bytes: %d\nmem_available_bytes: %s\nmem_total_bytes: %s\ncpu_count: %d\nload_1m: %s\n",
			r.Value.DiskFreeBytes, r.Value.DiskTotalBytes, optionalMachineInt64(memAvailableBytes),
			optionalMachineInt64(memTotalBytes), r.Value.CPUCount, optionalMachineFloat(r.Value.Load1m)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "\nstate_history: rows_seen=%d invalid=%d truncated=%t\n",
		result.StateHistory.RowsSeen, result.StateHistory.Invalid, result.StateHistory.Truncated); err != nil {
		return err
	}
	for _, span := range result.StateHistory.Items {
		if _, err := fmt.Fprintf(out, "state_span: state=%s entered_at=%s left_at=%s reason=%s\n",
			terminalSafe(string(span.State)), span.EnteredAt.UTC().Format(time.RFC3339Nano),
			machineReadTime(span.LeftAt), terminalSafe(span.Reason.Text)); err != nil {
			return err
		}
	}
	return nil
}

func optionalMachineInt(value *int) string {
	if value == nil {
		return "unknown"
	}
	return strconv.Itoa(*value)
}

func optionalMachineInt64(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatInt(*value, 10)
}

func optionalMachineBool(value *bool) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatBool(*value)
}

func optionalMachineFloat(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatFloat(*value, 'f', 2, 64)
}

func writeMachinesJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeMachineEvidence(out io.Writer, result operator.MachineEvidenceResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeMachinesJSON(out, result)
	}
	d := result.Disclosure
	if _, err := fmt.Fprintf(out,
		"%s; Hub evaluated at %s.\n%s\nmachine_id: %s\n"+
			"journal_producer: kind=%s authority=%s\nsystemd_producer: kind=%s authority=%s\n"+
			"credential_producer: kind=%s authority=%s\n"+
			"occupancy_producer: kind=%s authority=%s\noccupancy_relay: kind=%s authority=%s\n"+
			"run_summary_producer: kind=%s authority=%s\n"+
			"run_summary_relay: kind=%s authority=%s\nindependent_verifier: %t\nstatus_is_outcome: %t\n"+
			"terminal_outcome_recorded: %t\nsystemd_state_is_work_outcome: %t\n"+
			"systemd_main_pid_excluded: %t\njournal_secret_shapes_redacted_by_agent: %t\n"+
			"run_summary_secret_shapes_redacted_by_agent: %t\n"+
			"credential_status_is_session_validity: %t\ncredential_remote_validation_performed: %t\n"+
			"credential_active_account_id_excluded: %t\ncredential_secret_fields_excluded: %t\n"+
			"credential_text_redacted_by_agent: %t\ncredential_peer_is_same_ticket: %t\n"+
			"occupancy_aggregated_by_hub: %t\noccupancy_process_state_used: %t\n"+
			"occupancy_provider_normalized: %t\noccupancy_errors_categorized: %t\n"+
			"occupancy_profile_id_excluded: %t\noccupancy_event_details_excluded: %t\n"+
			"limit: %d\nmax_field_bytes: %d\nmax_shapes: %d\nmax_credential_peers: %d\nmax_occupancy_agents: %d\n",
		source, result.EvaluatedAt.UTC().Format(time.RFC3339Nano), terminalSafe(result.DisplayName),
		terminalSafe(result.MachineID), terminalSafe(d.JournalProducer.Kind), terminalSafe(d.JournalProducer.Authority),
		terminalSafe(d.SystemdProducer.Kind), terminalSafe(d.SystemdProducer.Authority),
		terminalSafe(d.CredentialProducer.Kind), terminalSafe(d.CredentialProducer.Authority),
		terminalSafe(d.OccupancyProducer.Kind), terminalSafe(d.OccupancyProducer.Authority),
		terminalSafe(d.OccupancyRelay.Kind), terminalSafe(d.OccupancyRelay.Authority),
		terminalSafe(d.RunSummaryProducer.Kind), terminalSafe(d.RunSummaryProducer.Authority),
		terminalSafe(d.RunSummaryRelay.Kind), terminalSafe(d.RunSummaryRelay.Authority), d.IndependentVerifier,
		d.StatusIsOutcome, d.TerminalOutcomeRecorded, d.SystemdStateIsWorkOutcome, d.SystemdMainPIDExcluded,
		d.JournalSecretShapesRedactedByAgent,
		d.RunSummarySecretShapesRedactedByAgent,
		d.CredentialStatusIsSessionValidity, d.CredentialRemoteValidationPerformed,
		d.CredentialActiveAccountIDExcluded, d.CredentialSecretFieldsExcluded,
		d.CredentialTextRedactedByAgent, d.CredentialPeerIsSameTicket,
		d.OccupancyAggregatedByHub, d.OccupancyProcessStateUsed, d.OccupancyProviderNormalized,
		d.OccupancyErrorsCategorized, d.OccupancyProfileIDExcluded, d.OccupancyEventDetailsExcluded,
		d.Limit, d.MaxFieldBytes, d.MaxShapes, d.MaxCredentialPeers, d.MaxOccupancyAgents); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out,
		"openclaw_producer: kind=%s authority=%s\ncli_tool_producer: kind=%s authority=%s\n"+
			"host_path_fields_excluded: %t\nprocess_id_fields_excluded: %t\nopenclaw_database_locations_excluded: %t\n"+
			"evidence_text_path_redacted_by_hub: %t\n"+
			"cli_openclaw_text_redacted_by_agent: %t\ncli_openclaw_text_redacted_by_hub: %t\n"+
			"cli_version_sources_collapsed: %t\nraw_version_text_parsed_by_hub: %t\n"+
			"running_relationship_derived_from_paths: %t\n",
		terminalSafe(d.OpenClawProducer.Kind), terminalSafe(d.OpenClawProducer.Authority),
		terminalSafe(d.CLIToolProducer.Kind), terminalSafe(d.CLIToolProducer.Authority),
		d.HostPathFieldsExcluded, d.ProcessIDFieldsExcluded, d.OpenClawDatabaseLocationsExcluded,
		d.EvidenceTextPathRedactedByHub,
		d.CLIOpenClawTextRedactedByAgent, d.CLIOpenClawTextRedactedByHub,
		d.CLIVersionSourcesCollapsed, d.RawVersionTextParsedByHub, d.RunningRelationshipDerivedFromPaths); err != nil {
		return err
	}
	oc := result.OpenClaw
	if _, err := fmt.Fprintf(out, "\nopenclaw: observed=%t decoded=%t present=%t install_invalid=%t db_invalid=%t\n",
		oc.Observed, oc.Decoded, oc.Present, oc.InstallInvalid, oc.DBInvalid); err != nil {
		return err
	}
	if oc.ObservedAt == nil {
		if _, err := fmt.Fprintln(out, "MEASURED_AT(agent): none\nRECEIVED_AT(Hub): none"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(out, "MEASURED_AT(agent): %s\nRECEIVED_AT(Hub): %s\n",
		oc.ObservedAt.MeasuredAt.UTC().Format(time.RFC3339Nano),
		oc.ObservedAt.ReceivedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if oc.Decoded {
		if _, err := fmt.Fprintf(out,
			"REASON: %s\nCLI_VERSION: %s\nCLI_VERSION_RAW(display-only): %s\nGATEWAY_VERSION: %s\nUPSTREAM_VERSION: %s\nCRASH_BUNDLES: %d\n",
			machineEvidenceOptionalText(oc.Reason),
			machineEvidenceOptionalText(oc.CLIVersion), machineEvidenceOptionalText(oc.CLIVersionRaw),
			machineEvidenceOptionalText(oc.GatewayVersion), machineEvidenceOptionalText(oc.UpstreamVersion),
			oc.CrashBundles); err != nil {
			return err
		}
	}
	if install := oc.Install; install != nil {
		if _, err := fmt.Fprintf(out,
			"INSTALL: unit_found=%t drop_ins=%d kill_mode=%s n_restarts=%s node_version=%s "+
				"npm_observed=%t npm_version=%s running_directory_observed=%t running_directory_exists=%t "+
				"running_directory_writable=%s running_version=%s process_observed=%t process_matches_unit=%s "+
				"release_layout_observed=%t releases_present=%t current_release_linked=%t "+
				"disk_free_measured=%t disk_free_bytes=%d\n"+
				"ACTIVE_ENTER_AT: %s\nUNIT_REASON: %s\nNODE_VERSION_REASON: %s\nNPM_REASON: %s\n"+
				"RUNNING_DIRECTORY_REASON: %s\nPROCESS_REASON: %s\nDISK_FREE_REASON: %s\n",
			install.UnitFound, install.DropInCount, machineEvidenceOptionalText(install.KillMode),
			machineEvidenceOptionalInt(install.NRestarts), machineEvidenceOptionalText(install.NodeVersion),
			install.NpmObserved, machineEvidenceOptionalText(install.NpmVersion),
			install.RunningDirectoryObserved, install.RunningDirectoryExists,
			machineEvidenceOptionalBool(install.RunningDirectoryWritable),
			machineEvidenceOptionalText(install.RunningVersion), install.ProcessObserved,
			machineEvidenceOptionalBool(install.ProcessMatchesUnit), install.ReleaseLayoutObserved,
			install.ReleasesPresent, install.CurrentReleaseLinked, install.DiskFreeMeasured, install.DiskFreeBytes,
			machineReadTime(install.ActiveEnterAt), machineEvidenceOptionalText(install.UnitReason),
			machineEvidenceOptionalText(install.NodeVersionReason), machineEvidenceOptionalText(install.NpmReason),
			machineEvidenceOptionalText(install.RunningDirectoryReason), machineEvidenceOptionalText(install.ProcessReason),
			machineEvidenceOptionalText(install.DiskFreeReason)); err != nil {
			return err
		}
	}
	if db := oc.DB; db != nil {
		if _, err := fmt.Fprintf(out,
			"OPENCLAW_DB: present=%t layout=%s support=%s unknown_location_count=%d task_run_rows=%d "+
				"cron_run_log_rows=%d cron_jobs_total=%d total_measured=%t cron_jobs_enabled=%d enabled_measured=%t "+
				"cron_jobs_overdue=%d schedule_measured=%t terminal_outcome_populated=%d\n"+
				"DB_REASON: %s\nLAST_TASK_ENDED_AT: %s\nLAST_CRON_RUN_AT: %s\nNEXT_CRON_RUN_AT: %s\n",
			db.Present, machineEvidenceOptionalText(db.Layout), db.Support, db.UnknownLocationCount,
			db.TaskRunRows, db.CronRunLogRows, db.CronJobsTotal, db.CronJobsTotalMeasured,
			db.CronJobsEnabled, db.CronJobsEnabledMeasured, db.CronJobsOverdue,
			db.CronJobsScheduleMeasured, db.TerminalOutcomePopulated, machineEvidenceOptionalText(db.Reason),
			machineReadTime(db.LastTaskEndedAt), machineReadTime(db.LastCronRunAt), machineReadTime(db.NextCronRunAt)); err != nil {
			return err
		}
		if err := writeMachineStatusCounts(out, "TASK_STATUSES", db.TaskStatuses); err != nil {
			return err
		}
		if err := writeMachineStatusCounts(out, "CRON_STATUSES", db.CronStatuses); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "\ncli tools: TOTAL %d truncated=%t invalid=%d\n",
		result.CLITools.Total, result.CLITools.Truncated, result.CLITools.Invalid); err != nil {
		return err
	}
	for _, tool := range result.CLITools.Items {
		processScan := ""
		if tool.ProcessScan != "" {
			processScan = "PROCESS_SCAN: " + terminalSafe(tool.ProcessScan) + "\n"
		}
		if _, err := fmt.Fprintf(out,
			"\nTOOL: %s%s\nPRESENT: %t\nON_PATH: %t\nPRESENT_EVIDENCE: %s\nPATH_SOURCE: %s\n"+
				"PATH_REASON: %s\nDAEMON_REACH: %s\nVERSION_REPORTED: %s\nVERSION_RAW(display-only): %s\n"+
				"VERSION_PACKAGE_JSON: %s\nVERSION_SOURCES_DISAGREE: %t\nPROCESS_OBSERVED: %t\n"+
				"RUNNING_RELATIONSHIP: %s\n%sVERSION_REASON: %s\nRUNNING_REASON: %s\n"+
				"SUPPORT: %s\nMEASURED_AT(agent): %s\nRECEIVED_AT(Hub): %s\n",
			terminalSafe(tool.Name.Text), jobEvidenceTextSuffix(tool.Name), tool.Present, tool.OnPath,
			terminalSafe(tool.PresentEvidence), terminalSafe(tool.PathSource), machineEvidenceOptionalText(tool.PathReason),
			terminalSafe(tool.DaemonReach),
			machineEvidenceOptionalText(tool.VersionReported), machineEvidenceOptionalText(tool.VersionRaw),
			machineEvidenceOptionalText(tool.VersionPackageJSON), tool.SourcesDisagree, tool.ProcessObserved,
			terminalSafe(tool.RunningRelationship), processScan, machineEvidenceOptionalText(tool.VersionReason),
			machineEvidenceOptionalText(tool.RunningReason), terminalSafe(string(tool.Support)),
			tool.MeasuredAt.UTC().Format(time.RFC3339Nano), tool.ReceivedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "\ncredentials: TOTAL %d truncated=%t invalid=%d\n",
		result.Credentials.Total, result.Credentials.Truncated, result.Credentials.Invalid); err != nil {
		return err
	}
	for _, credential := range result.Credentials.Items {
		if _, err := fmt.Fprintf(out,
			"\nPROVIDER: %s%s\nSTATUS: %s\nVERIFICATION_METHOD: %s\nVERIFIED_AT: %s\n"+
				"EXPIRES_AT: %s\nLAST_REFRESH: %s\nFILE_MTIME: %s\nACTIVE_ACCOUNT_SELECTED: %t\n"+
				"ACCOUNT_COUNT: %d\nLIFETIME_SECONDS: %d\nREFRESHES_SEEN: %d\nWATCHED_FOR_SECONDS: %d\n"+
				"MEASURED_AT(agent): %s\nRECEIVED_AT(Hub): %s\n",
			terminalSafe(credential.Provider.Text), jobEvidenceTextSuffix(credential.Provider), terminalSafe(string(credential.Status)),
			terminalSafe(string(credential.VerificationMethod)), machineReadTime(credential.VerifiedAt), machineReadTime(credential.ExpiresAt),
			machineReadTime(credential.LastRefresh), machineReadTime(credential.FileMTime),
			credential.ActiveAccountSelected, credential.AccountCount, credential.LifetimeSeconds,
			credential.RefreshesSeen, credential.WatchedForSeconds,
			credential.MeasuredAt.UTC().Format(time.RFC3339Nano), credential.ReceivedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if credential.Note != nil {
			if _, err := fmt.Fprintf(out, "NOTE: %s%s\n", terminalSafe(credential.Note.Text),
				jobEvidenceTextSuffix(*credential.Note)); err != nil {
				return err
			}
		}
		if credential.LastError != nil {
			if _, err := fmt.Fprintf(out, "LAST_ERROR: %s%s\n", terminalSafe(credential.LastError.Text),
				jobEvidenceTextSuffix(*credential.LastError)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(out, "PEERS: TOTAL %d truncated=%t\n", credential.PeersTotal,
			credential.PeersTruncated); err != nil {
			return err
		}
		for _, peer := range credential.Peers {
			if _, err := fmt.Fprintf(out, "- %s%s refreshes=%d file_mtime=%s\n",
				terminalSafe(peer.DisplayName.Text), jobEvidenceTextSuffix(peer.DisplayName), peer.RefreshesSeen,
				machineReadTime(peer.FileMTime)); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(out,
		"\noccupancy: TOTAL %d truncated=%t invalid=%d observed=%t db_observed=%t observation_invalid=%t rows_seen=%d rows_without_provider=%d\n",
		result.Occupancy.Total, result.Occupancy.Truncated, result.Occupancy.Invalid,
		result.Occupancy.Observed, result.Occupancy.DBObserved, result.Occupancy.ObservationInvalid,
		result.Occupancy.RowsSeen, result.Occupancy.RowsWithoutProvider); err != nil {
		return err
	}
	if result.Occupancy.ObservedAt == nil {
		if _, err := fmt.Fprintln(out, "MEASURED_AT(agent): none\nRECEIVED_AT(Hub): none"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(out, "MEASURED_AT(agent): %s\nRECEIVED_AT(Hub): %s\n",
		result.Occupancy.ObservedAt.MeasuredAt.UTC().Format(time.RFC3339Nano),
		result.Occupancy.ObservedAt.ReceivedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	for _, occupancy := range result.Occupancy.Items {
		if _, err := fmt.Fprintf(out,
			"\nPROVIDER: %s%s\nSOURCE: %s%s\nRUNS: %d\nERRORS(unclassified): %d\n"+
				"LAST_RUN_AT(OpenClaw): %s\nLAST_RECEIVED_AT(Hub): %s\nAGENTS: TOTAL %d truncated=%t\n",
			terminalSafe(occupancy.Provider.Text), jobEvidenceTextSuffix(occupancy.Provider),
			terminalSafe(occupancy.Source.Text), jobEvidenceTextSuffix(occupancy.Source), occupancy.Runs,
			occupancy.Errors, occupancy.LastRunAt.UTC().Format(time.RFC3339Nano),
			occupancy.LastReceivedAt.UTC().Format(time.RFC3339Nano), occupancy.AgentsTotal,
			occupancy.AgentsTruncated); err != nil {
			return err
		}
		for _, agent := range occupancy.Agents {
			if _, err := fmt.Fprintf(out, "- %s%s\n", terminalSafe(agent.Text), jobEvidenceTextSuffix(agent)); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(out, "\nsystemd units: TOTAL %d truncated=%t invalid=%d\n",
		result.SystemdUnits.Total, result.SystemdUnits.Truncated, result.SystemdUnits.Invalid); err != nil {
		return err
	}
	for _, unit := range result.SystemdUnits.Items {
		present := "unknown"
		if unit.Present {
			present = "true"
		} else if unit.Measured {
			present = "false"
		}
		activeEnter := "none"
		if unit.ActiveEnterTimestamp != nil {
			activeEnter = unit.ActiveEnterTimestamp.UTC().Format(time.RFC3339Nano)
		}
		if _, err := fmt.Fprintf(out,
			"\nUNIT: %s%s\nPRESENT: %s\nSTATE: %s%s/%s%s\nACTIVE_ENTER_TIMESTAMP(agent): %s\n"+
				"N_RESTARTS: %d\nMEASURED_AT(agent): %s\nRECEIVED_AT(Hub): %s\n",
			terminalSafe(unit.Name.Text), jobEvidenceTextSuffix(unit.Name), present,
			terminalSafe(unit.ActiveState.Text), jobEvidenceTextSuffix(unit.ActiveState),
			terminalSafe(unit.SubState.Text), jobEvidenceTextSuffix(unit.SubState), activeEnter,
			unit.NRestarts, unit.MeasuredAt.UTC().Format(time.RFC3339Nano),
			unit.ReceivedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if unit.Reason != nil {
			if _, err := fmt.Fprintf(out, "REASON: %s\n", machineEvidenceOptionalText(unit.Reason)); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(out, "\nrun summaries: TOTAL %d truncated=%t observed=%t db_observed=%t\n",
		result.RunSummaries.Total, result.RunSummaries.Truncated, result.RunSummaries.Observed,
		result.RunSummaries.DBObserved); err != nil {
		return err
	}
	if result.RunSummaries.ObservedAt == nil {
		if _, err := fmt.Fprintln(out, "MEASURED_AT(agent): none\nRECEIVED_AT(Hub): none"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(out, "MEASURED_AT(agent): %s\nRECEIVED_AT(Hub): %s\n",
		result.RunSummaries.ObservedAt.MeasuredAt.UTC().Format(time.RFC3339Nano),
		result.RunSummaries.ObservedAt.ReceivedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	for _, summary := range result.RunSummaries.Items {
		reportedAt := "none"
		if summary.ReportedAt != nil {
			reportedAt = summary.ReportedAt.UTC().Format(time.RFC3339Nano)
		}
		if _, err := fmt.Fprintf(out,
			"\nREPORTED_AT(OpenClaw): %s\nJOB_ID: %s%s\nSTATUS: %s%s\nBYTES: %d\nSUMMARY: %s%s\n",
			reportedAt, terminalSafe(summary.JobID.Text), jobEvidenceTextSuffix(summary.JobID),
			terminalSafe(summary.Status.Text), jobEvidenceTextSuffix(summary.Status), summary.Summary.Bytes,
			terminalSafe(summary.Summary.Text), jobEvidenceTextSuffix(summary.Summary)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "\njournals: TOTAL %d truncated=%t undecodable=%d\n",
		result.Journals.Total, result.Journals.Truncated, result.Journals.Undecodable); err != nil {
		return err
	}
	for _, journal := range result.Journals.Items {
		if _, err := fmt.Fprintf(out,
			"\nUNIT: %s%s\nWINDOW_SEC: %d\nMEASURED_AT(agent): %s\nRECEIVED_AT(Hub): %s\n",
			terminalSafe(journal.Unit.Text), jobEvidenceTextSuffix(journal.Unit), journal.WindowSeconds,
			journal.MeasuredAt.UTC().Format(time.RFC3339Nano), journal.ReceivedAt.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if journal.ReadFailed {
			if _, err := fmt.Fprintf(out, "read failed (not-heard): %s%s\n",
				terminalSafe(journal.Err.Text), jobEvidenceTextSuffix(*journal.Err)); err != nil {
				return err
			}
			continue
		}
		lowerBound := ""
		if journal.LinesAreLowerBound {
			lowerBound = " (at least)"
		}
		if _, err := fmt.Fprintf(out, "LINES%s: %d\nSHAPES: %d\n", lowerBound, journal.Lines, journal.Shapes); err != nil {
			return err
		}
		for _, shape := range journal.Top {
			if _, err := fmt.Fprintf(out, "×%d %s%s\n", shape.Count, terminalSafe(shape.Example.Text),
				jobEvidenceTextSuffix(shape.Example)); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(out,
		"\nunits_without_journal (not collected this round; not quiet): TOTAL %d truncated=%t\n",
		result.Journals.UnitsWithoutJournalTotal, result.Journals.UnitsWithoutJournalTruncated); err != nil {
		return err
	}
	for _, unit := range result.Journals.UnitsWithoutJournal {
		if _, err := fmt.Fprintf(out, "- %s%s\n", terminalSafe(unit.Text), jobEvidenceTextSuffix(unit)); err != nil {
			return err
		}
	}
	return nil
}

func writeMachineStatusCounts(out io.Writer, label string, page operator.MachineStatusCountPage) error {
	if _, err := fmt.Fprintf(out, "%s: TOTAL %d truncated=%t invalid=%d", label, page.Total, page.Truncated, page.Invalid); err != nil {
		return err
	}
	for _, item := range page.Items {
		if _, err := fmt.Fprintf(out, " %s%s=%d", terminalSafe(item.Status.Text), jobEvidenceTextSuffix(item.Status), item.Count); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out)
	return err
}

func machineEvidenceOptionalText(value *operator.EvidenceText) string {
	if value == nil {
		return "unknown"
	}
	return terminalSafe(value.Text) + jobEvidenceTextSuffix(*value)
}

func machineEvidenceOptionalInt(value *int) string {
	if value == nil {
		return "unknown"
	}
	return strconv.Itoa(*value)
}

func machineEvidenceOptionalBool(value *bool) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatBool(*value)
}

func machineReadTime(value *time.Time) string {
	if value == nil {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// terminalSafe keeps operator-controlled registry strings readable without
// letting control bytes rewrite a terminal, OSC title, or copied command.
func terminalSafe(value string) string { return strconv.QuoteToGraphic(value) }
