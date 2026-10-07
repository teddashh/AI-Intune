package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type repeatedChangeKinds []string

func (values *repeatedChangeKinds) String() string { return strings.Join(*values, ",") }
func (values *repeatedChangeKinds) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type changeLimitFlag struct {
	value int
	set   bool
}

func (value *changeLimitFlag) String() string { return strconv.Itoa(value.value) }
func (value *changeLimitFlag) Set(raw string) error {
	if value.set {
		return errors.New("cannot be repeated")
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || strconv.Itoa(parsed) != raw {
		return errors.New("must be a canonical decimal integer")
	}
	value.value, value.set = parsed, true
	return nil
}

func cmdReportChanges(argv []string) {
	if err := runReportChangesCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runReportChangesCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runReportChangesCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runReportChangesCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("report changes", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub report changes [filters] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  Normal mode uses the HTTP operator API; --db is fenced break-glass only when Hub is fully stopped.")
		fmt.Fprintln(errOut, "  Window is (from,to]; observation compares endpoints only and does not claim to list every intermediate change. Default is last 24h, maximum 30 days.")
		fs.PrintDefaults()
	}
	var hubURL, dbPath, machine, subject, from, to, cursor auditStringFlag
	var jsonOutput auditBoolFlag
	limit := changeLimitFlag{value: operator.DefaultChangeReadLimit}
	var kinds repeatedChangeKinds
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (discovered automatically when omitted)")
	fs.Var(&dbPath, "db", "existing SQLite file path for stopped-service direct DB break-glass")
	fs.Var(&machine, "machine", "exact match for machine_id")
	fs.Var(&kinds, "kind", "canonical change kind; can be repeated")
	fs.Var(&subject, "subject", "exact match for subject")
	fs.Var(&from, "from", "window start (second-precision RFC3339; exclusive)")
	fs.Var(&to, "to", "window end (second-precision RFC3339; inclusive)")
	fs.Var(&limit, "limit", "maximum items per page (1..100)")
	fs.Var(&cursor, "cursor", "opaque next cursor returned by the previous page")
	fs.Var(&jsonOutput, "json", "output stable operator JSON DTO")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("report changes: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if hubURL.set && dbPath.set {
		return errors.New("report changes: --hub-url (HTTP mode) and --db (direct mode) cannot both be specified")
	}
	for _, field := range []struct {
		name  string
		value auditStringFlag
		max   int
	}{
		{"hub-url", hubURL, 2048}, {"db", dbPath, 4096}, {"machine", machine, 256},
		{"subject", subject, 256}, {"from", from, 64}, {"to", to, 64}, {"cursor", cursor, 4096},
	} {
		if field.value.set {
			if err := validateReportChangeCLIText(field.name, field.value.value, field.max); err != nil {
				return err
			}
		}
	}
	if limit.value < 1 || limit.value > operator.MaxChangeReadLimit {
		return fmt.Errorf("report changes: --limit must be between 1 and %d", operator.MaxChangeReadLimit)
	}

	request := operator.ChangeListRequest{
		MachineID: machine.value, Subject: subject.value, Limit: limit.value, Cursor: cursor.value,
	}
	knownKinds := make(map[string]bool, len(operator.ChangeKinds()))
	for _, kind := range operator.ChangeKinds() {
		knownKinds[kind] = true
	}
	seenKinds := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		if !knownKinds[kind] || seenKinds[kind] {
			return fmt.Errorf("report changes: --kind %q is not a canonical kind or is duplicated", kind)
		}
		seenKinds[kind] = true
		request.Kinds = append(request.Kinds, kind)
	}
	for _, field := range []struct {
		name   string
		value  auditStringFlag
		target **time.Time
	}{
		{"from", from, &request.From}, {"to", to, &request.To},
	} {
		if !field.value.set {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, field.value.value)
		if err != nil || parsed.Nanosecond() != 0 || parsed.Format(time.RFC3339) != field.value.value {
			return fmt.Errorf("report changes: --%s must be second-precision RFC3339", field.name)
		}
		parsed = parsed.UTC()
		*field.target = &parsed
	}
	if err := operator.ValidateChangeListRequest(request); err != nil {
		return fmt.Errorf("report changes: %w", err)
	}

	if dbPath.set {
		return runReportChangesDirect(ctx, request, dbPath.value, jsonOutput.value, out, deps)
	}
	client, err := reportChangesHTTPClient(hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	result, err := client.Changes(ctx, request)
	if err != nil {
		return fmt.Errorf("failed to read report changes (HTTP operator API): %w", err)
	}
	return writeReportChanges(out, result, jsonOutput.value, "HTTP operator API")
}

func validateReportChangeCLIText(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("report changes: --%s must not be empty, too long, contain leading or trailing whitespace, or contain invalid UTF-8", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("report changes: --%s must not contain control or invisible formatting characters", name)
		}
	}
	return nil
}

func reportChangesHTTPClient(explicitURL string, explicit bool, deps machineCommandDeps) (*operatorclient.Client, error) {
	if explicit {
		if deps.newOperatorClient == nil {
			return nil, errors.New("report changes: operator HTTP client is not initialized")
		}
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, fmt.Errorf("report changes: failed to create HTTP operator client: %w", err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, errors.New("report changes: Hub discovery is not initialized")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("report changes: unable to discover Hub: %w", err)
	}
	if deps.newOperatorClient == nil {
		return nil, errors.New("report changes: operator HTTP client is not initialized")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("report changes: failed to create discovered HTTP operator client: %w", err)
	}
	return client, nil
}

func runReportChangesDirect(ctx context.Context, request operator.ChangeListRequest, dbPath string,
	jsonOutput bool, out io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "report changes", dbPath, deps, func(st *store.Store) error {
		result, err := operator.New(st).ListChangesContext(ctx, request, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("failed to read report changes (direct DB operator service): %w", err)
		}
		return writeReportChanges(out, result, jsonOutput, "direct DB operator service")
	})
}

func writeReportChanges(out io.Writer, result operator.ChangeListResult, jsonOutput bool, source string) error {
	if jsonOutput {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result)
	}
	if _, err := fmt.Fprintf(out,
		"%s; Hub evaluation time %s; consistency %s\n"+
			"Fixed window %s %s -> %s (time basis %s); %d total after filter (matched_total=%d, kind_counts apply the same filter)\n"+
			"Coverage: observation=%s/%s (pruned %d rows), registry=%s, state=%s; malformed timestamps=%d, unplaceable=%d\n"+
			"Semantics: transition is a registry/state durable event; window_comparison compares observation window endpoints only and does not enumerate every intermediate change\n",
		source, result.EvaluatedAt.Format(time.RFC3339), result.Consistency,
		result.Window.Boundary, result.Window.From.Format(time.RFC3339), result.Window.To.Format(time.RFC3339),
		result.Window.TimeBasis, result.Total, result.MatchedTotal,
		result.Coverage.ObservationComparison, result.Coverage.ObservationHistory,
		result.Coverage.ObservationRowsPruned, result.Coverage.RegistryHistory,
		result.Coverage.StateHistory,
		result.Coverage.MalformedTimestampRows, result.Coverage.UnplaceableTimestampRows); err != nil {
		return err
	}
	if len(result.Coverage.Issues) > 0 {
		if _, err := fmt.Fprintf(out, "coverage issues: %s\n", terminalSafe(strings.Join(result.Coverage.Issues, ","))); err != nil {
			return err
		}
	}
	if len(result.Items) == 0 {
		if _, err := fmt.Fprintln(out, "No changes matched the filter"); err != nil {
			return err
		}
	} else {
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(table, "CHANGED_AT\tMACHINE\tKIND/SUBJECT\tSEMANTICS\tBEFORE -> AFTER\tFIELDS"); err != nil {
			return err
		}
		for _, item := range result.Items {
			machine := item.DisplayName + " (" + item.MachineRef + ")"
			if item.MachineID != nil {
				machine = item.DisplayName + " (" + *item.MachineID + ")"
			}
			fields := strings.Join(item.ChangedFields, ",")
			if fields == "" {
				fields = "none"
			}
			before := changeValueSummary(item.Kind, item.Before)
			if item.Before == nil {
				before = "[" + item.BaselineStatus + "]"
			}
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s/%s\t%s\t%s -> %s\t%s\n",
				item.ChangedAt.Format(time.RFC3339), terminalSafe(machine), terminalSafe(item.Kind),
				terminalSafe(item.Subject), terminalSafe(item.Semantics), terminalSafe(before),
				terminalSafe(changeValueSummary(item.Kind, item.After)), terminalSafe(fields)); err != nil {
				return err
			}
		}
		if err := table.Flush(); err != nil {
			return err
		}
	}
	if result.NextCursor != nil {
		// The strict client/service guarantees canonical base64url, so keep this
		// raw for direct copy into the next --cursor invocation.
		_, err := fmt.Fprintf(out, "next cursor: %s\n", *result.NextCursor)
		return err
	}
	return nil
}

func changeValueSummary(kind string, value *operator.ChangeValue) string {
	if value == nil {
		return "unknown"
	}
	parts := make([]string, 0, 7)
	appendString := func(name string, field *string) {
		if field != nil {
			parts = append(parts, name+"="+*field)
		}
	}
	appendBool := func(name string, field *bool) {
		if field != nil {
			parts = append(parts, name+"="+strconv.FormatBool(*field))
		}
	}
	switch kind {
	case operator.ChangeKindState:
		if value.State != nil {
			parts = append(parts, "state="+string(*value.State))
		}
	case operator.ChangeKindRegistry:
		appendString("lifecycle", value.Lifecycle)
	case operator.ChangeKindIdentity:
		appendString("os", value.OS)
		appendString("kernel", value.Kernel)
		appendString("arch", value.Arch)
		appendBool("linger", value.LingerEnabled)
	case operator.ChangeKindCredential:
		if value.Status != nil {
			parts = append(parts, "status="+string(*value.Status))
		}
		if value.ExpiresAt != nil {
			parts = append(parts, "expires="+value.ExpiresAt.Format(time.RFC3339))
		}
	case operator.ChangeKindCLITool:
		appendBool("present", value.Present)
		appendBool("on_path", value.OnPath)
		appendString("version", value.VersionReported)
		appendString("package", value.VersionPackageJSON)
		appendString("daemon", value.DaemonReach)
		appendBool("source_disagree", value.VersionSourcesDisagree)
		appendBool("running_mismatch", value.RunningInstallMismatch)
	case operator.ChangeKindSystemd:
		if value.Measured != nil && !*value.Measured {
			parts = append(parts, "present=unknown")
		} else {
			appendBool("present", value.Present)
		}
		appendString("active", value.ActiveState)
		appendString("sub", value.SubState)
		if value.Restarts != nil {
			parts = append(parts, "restarts="+strconv.Itoa(*value.Restarts))
		}
	case operator.ChangeKindOpenClaw:
		appendBool("present", value.Present)
		appendString("cli", value.CLIVersion)
		appendString("gateway", value.GatewayVersion)
		appendString("upstream", value.UpstreamVersion)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, ",")
}
