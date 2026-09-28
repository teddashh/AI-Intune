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
		return errors.New("不可重複")
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || strconv.Itoa(parsed) != raw {
		return errors.New("必須是 canonical 十進位整數")
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
		fmt.Fprintln(errOut, "用法：clawctl-hub report changes [filters] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API；--db 僅供 Hub 完全停止時的 fenced break-glass。")
		fmt.Fprintln(errOut, "  視窗是 (from,to]；觀測只比較兩端點，不聲稱列出中間每次變化。預設最近 24 小時，最長 30 天。")
		fs.PrintDefaults()
	}
	var hubURL, dbPath, machine, subject, from, to, cursor auditStringFlag
	var jsonOutput auditBoolFlag
	limit := changeLimitFlag{value: operator.DefaultChangeReadLimit}
	var kinds repeatedChangeKinds
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL（省略時自動發現）")
	fs.Var(&dbPath, "db", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	fs.Var(&machine, "machine", "精確比對 machine_id")
	fs.Var(&kinds, "kind", "canonical change kind；可重複")
	fs.Var(&subject, "subject", "精確比對 subject")
	fs.Var(&from, "from", "視窗起點（second-precision RFC3339；不含端點）")
	fs.Var(&to, "to", "視窗終點（second-precision RFC3339；含端點）")
	fs.Var(&limit, "limit", "每頁最多幾筆（1..100）")
	fs.Var(&cursor, "cursor", "上一頁回傳的 opaque next cursor")
	fs.Var(&jsonOutput, "json", "輸出 stable operator JSON DTO")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("report changes: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if hubURL.set && dbPath.set {
		return errors.New("report changes: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
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
		return fmt.Errorf("report changes: --limit 必須介於 1 與 %d", operator.MaxChangeReadLimit)
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
			return fmt.Errorf("report changes: --kind %q 不是 canonical kind 或重複", kind)
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
			return fmt.Errorf("report changes: --%s 必須是 second-precision RFC3339", field.name)
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
		return fmt.Errorf("讀取 report changes 失敗（HTTP operator API）：%w", err)
	}
	return writeReportChanges(out, result, jsonOutput.value, "HTTP operator API")
}

func validateReportChangeCLIText(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("report changes: --%s 不可為空、過長、含首尾空白或 invalid UTF-8", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("report changes: --%s 不可含控制或隱形格式字元", name)
		}
	}
	return nil
}

func reportChangesHTTPClient(explicitURL string, explicit bool, deps machineCommandDeps) (*operatorclient.Client, error) {
	if explicit {
		if deps.newOperatorClient == nil {
			return nil, errors.New("report changes: operator HTTP client 未初始化")
		}
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, fmt.Errorf("report changes: 建立 HTTP operator client 失敗：%w", err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, errors.New("report changes: Hub discovery 未初始化")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("report changes: 無法發現 Hub：%w", err)
	}
	if deps.newOperatorClient == nil {
		return nil, errors.New("report changes: operator HTTP client 未初始化")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("report changes: 建立 discovered HTTP operator client 失敗：%w", err)
	}
	return client, nil
}

func runReportChangesDirect(ctx context.Context, request operator.ChangeListRequest, dbPath string,
	jsonOutput bool, out io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "report changes", dbPath, deps, func(st *store.Store) error {
		result, err := operator.New(st).ListChangesContext(ctx, request, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("讀取 report changes 失敗（direct DB operator service）：%w", err)
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
		"%s；Hub 評估時間 %s；consistency %s。\n"+
			"固定視窗 %s %s → %s（time basis %s）；filter 後共 %d 筆（matched_total=%d，kind_counts 套用相同 filter）。\n"+
			"coverage：observation=%s/%s（已 prune %d rows），registry=%s，state=%s；malformed timestamps=%d，unplaceable=%d。\n"+
			"semantics：transition 是 registry/state durable event；window_comparison 只比較觀測視窗兩端，不列舉中間每次變動。\n",
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
		if _, err := fmt.Fprintln(out, "沒有符合 filter 的 changes。"); err != nil {
			return err
		}
	} else {
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(table, "CHANGED_AT\tMACHINE\tKIND/SUBJECT\tSEMANTICS\tBEFORE → AFTER\tFIELDS"); err != nil {
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
			if _, err := fmt.Fprintf(table, "%s\t%s\t%s/%s\t%s\t%s → %s\t%s\n",
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
