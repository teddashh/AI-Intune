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
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
	"github.com/teddashh/AI-Intune/internal/store"
)

func cmdDeployment(argv []string) {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "用法：clawctl-hub deployment preview|create|list|show|continue|retry|abandon")
		log.Fatal("deployment: 必須指定 subcommand")
	}
	switch argv[0] {
	case "preview", "list", "show":
		if err := runDeploymentReadCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Fatal(terminalSafe(err.Error()))
		}
	case "create", "continue", "retry", "abandon":
		if err := runDeploymentMutationCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Fatal(terminalSafe(err.Error()))
		}
	default:
		log.Fatalf("未知的 deployment 子指令 %q", argv[0])
	}
}

type repeatedDeploymentStates []string

func (v *repeatedDeploymentStates) String() string { return strings.Join(*v, ",") }
func (v *repeatedDeploymentStates) Set(value string) error {
	*v = append(*v, value)
	return nil
}

type deploymentReadTransport struct {
	hubURL   string
	dbPath   string
	json     bool
	explicit map[string]bool
}

func runDeploymentReadCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runDeploymentReadCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runDeploymentReadCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	if len(argv) == 0 {
		return errors.New("deployment read: 必須指定 list、show 或 preview")
	}
	switch argv[0] {
	case "list":
		return runDeploymentListCommand(ctx, argv[1:], out, errOut, deps)
	case "show":
		return runDeploymentShowCommand(ctx, argv[1:], out, errOut, deps)
	case "preview":
		return runDeploymentPreviewCommand(ctx, argv[1:], out, errOut, deps)
	default:
		return fmt.Errorf("deployment read: 不認得 subcommand %q", argv[0])
	}
}

func addDeploymentReadTransportFlags(fs *flag.FlagSet) (hubURL, dbPath *string, jsonOutput *bool) {
	hubURL = fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	dbPath = fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	jsonOutput = fs.Bool("json", false, "輸出 stable operator JSON DTO")
	return hubURL, dbPath, jsonOutput
}

func deploymentReadTransportFromFlags(fs *flag.FlagSet, hubURL, dbPath string, jsonOutput bool) (deploymentReadTransport, error) {
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return deploymentReadTransport{}, errors.New("deployment read: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if seen["hub-url"] && (strings.TrimSpace(hubURL) == "" || hubURL != strings.TrimSpace(hubURL)) {
		return deploymentReadTransport{}, errors.New("deployment read: --hub-url 不可為空或含首尾空白")
	}
	if seen["db"] && (strings.TrimSpace(dbPath) == "" || dbPath != strings.TrimSpace(dbPath)) {
		return deploymentReadTransport{}, errors.New("deployment read: --db 不可為空或含首尾空白")
	}
	return deploymentReadTransport{hubURL: hubURL, dbPath: dbPath, json: jsonOutput, explicit: seen}, nil
}

func runDeploymentListCommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("deployment list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub deployment list [filters] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API；--db 僅供 Hub 完全停止時的 fenced break-glass。")
		fs.PrintDefaults()
	}
	hubURL, dbPath, jsonOutput := addDeploymentReadTransportFlags(fs)
	channel := fs.String("channel", "", "只看 canary 或 stable")
	stuck := fs.String("stuck", "", "只看 stuck（true）或 non-stuck（false）deployment")
	limit := fs.Int("limit", operator.DefaultDeploymentReadLimit, "每頁最多幾張（1..100）")
	cursor := fs.String("cursor", "", "上一頁回傳的 opaque next cursor")
	var stateValues repeatedDeploymentStates
	fs.Var(&stateValues, "state", "只看 running、paused 或 finished；可重複")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	transport, err := deploymentReadTransportFromFlags(fs, *hubURL, *dbPath, *jsonOutput)
	if err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("deployment list: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if transport.explicit["channel"] {
		if *channel != "canary" && *channel != "stable" {
			return errors.New("deployment list: --channel 只接受 canary 或 stable")
		}
	}
	if *limit < 1 || *limit > operator.MaxDeploymentReadLimit {
		return fmt.Errorf("deployment list: --limit 必須介於 1 與 %d", operator.MaxDeploymentReadLimit)
	}
	if transport.explicit["cursor"] {
		if err := validateDeploymentReadCLIValue("cursor", *cursor, 2048); err != nil {
			return err
		}
	}
	request := operator.DeploymentListRequest{Channel: *channel, Limit: *limit, Cursor: *cursor}
	seenStates := make(map[string]bool, len(stateValues))
	for _, state := range stateValues {
		if (state != store.DeploymentRunning && state != store.DeploymentPaused && state != store.DeploymentFinished) || seenStates[state] {
			return fmt.Errorf("deployment list: --state %q 不是 canonical state 或重複", state)
		}
		seenStates[state] = true
		request.States = append(request.States, state)
	}
	if transport.explicit["stuck"] {
		var value bool
		switch *stuck {
		case "true":
			value = true
		case "false":
			value = false
		default:
			return errors.New("deployment list: --stuck 只接受 true 或 false")
		}
		request.Stuck = &value
	}
	if transport.explicit["db"] {
		return runDeploymentListDirect(ctx, request, transport, out, deps)
	}
	client, err := deploymentHTTPClient(transport.hubURL, transport.explicit["hub-url"], deps)
	if err != nil {
		return err
	}
	result, err := client.Deployments(ctx, request)
	if err != nil {
		return fmt.Errorf("讀取 deployment list 失敗（HTTP operator API）：%w", err)
	}
	return writeDeploymentList(out, result, transport.json, "HTTP operator API")
}

func runDeploymentShowCommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("deployment show", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub deployment show [--json] [--hub-url URL | --db PATH] <deployment-id>")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API；--db 僅供 Hub 完全停止時的 fenced break-glass。")
		fs.PrintDefaults()
	}
	hubURL, dbPath, jsonOutput := addDeploymentReadTransportFlags(fs)
	id := ""
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		id, argv = argv[0], argv[1:]
	}
	if err := fs.Parse(argv); err != nil {
		return err
	}
	transport, err := deploymentReadTransportFromFlags(fs, *hubURL, *dbPath, *jsonOutput)
	if err != nil {
		return err
	}
	if id == "" {
		if fs.NArg() != 1 {
			return errors.New("deployment show: 必須提供且只提供一個 deployment-id")
		}
		id = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return errors.New("deployment show: 只接受一個 deployment-id")
	}
	if err := validateDeploymentReadCLIValue("deployment-id", id, 256); err != nil ||
		strings.Contains(id, "/") || id == "." || id == ".." {
		return errors.New("deployment show: deployment-id 不可含首尾空白、控制字元、dot segment 或斜線，且長度不可超過 256 bytes")
	}
	if transport.explicit["db"] {
		return runDeploymentShowDirect(ctx, id, transport, out, deps)
	}
	client, err := deploymentHTTPClient(transport.hubURL, transport.explicit["hub-url"], deps)
	if err != nil {
		return err
	}
	result, err := client.Deployment(ctx, id)
	if err != nil {
		return fmt.Errorf("讀取 deployment detail 失敗（HTTP operator API）：%w", err)
	}
	return writeDeploymentDetail(out, result, transport.json, "HTTP operator API")
}

func runDeploymentPreviewCommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("deployment preview", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub deployment preview --channel canary|stable --version VERSION [--artifact SHA256] [--batch N] [--timeout SECONDS] [--irreversible] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API；--db 僅供 Hub 完全停止時的 fenced break-glass。")
		fs.PrintDefaults()
	}
	hubURL, dbPath, jsonOutput := addDeploymentReadTransportFlags(fs)
	channel := fs.String("channel", "", "canary 或 stable")
	version := fs.String("version", "", "OpenClaw 版本")
	artifactSHA := fs.String("artifact", "", "指定 artifact SHA-256（64 個小寫 hex）")
	batch := fs.Int("batch", operator.DefaultDeploymentBatchSize, "後續每批台數（1 到 5）。第一批永遠是 1 台；省略或 1 代表後面也是 1 台")
	timeout := fs.Int("timeout", operator.DefaultDeploymentTimeout, "執行逾時（秒）")
	irreversible := fs.Bool("irreversible", false, "失敗不可逆")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	transport, err := deploymentReadTransportFromFlags(fs, *hubURL, *dbPath, *jsonOutput)
	if err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("deployment preview: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if !transport.explicit["channel"] || (*channel != "canary" && *channel != "stable") {
		return errors.New("deployment preview: --channel 必填且只接受 canary 或 stable")
	}
	if !transport.explicit["version"] {
		return errors.New("deployment preview: --version 必填")
	}
	if err := validateDeploymentReadCLIValue("version", *version, 128); err != nil {
		return err
	}
	if transport.explicit["artifact"] && !artifact.ValidSHA256Hex(*artifactSHA) {
		return errors.New("deployment preview: --artifact 必須是 64 個小寫 hex")
	}
	if *batch < 1 || *batch > store.MaxDeploymentBatchSize {
		return fmt.Errorf("deployment preview: --batch 必須介於 1 與 %d", store.MaxDeploymentBatchSize)
	}
	if *timeout < 1 || *timeout > 86400 {
		return errors.New("deployment preview: --timeout 必須介於 1 與 86400 秒")
	}
	request := operator.DeploymentCreatePreviewRequest{
		Channel: *channel, Version: *version, ArtifactSHA256: *artifactSHA,
		BatchSize: *batch, ExecutionTimeoutSeconds: *timeout, Irreversible: *irreversible,
	}
	if transport.explicit["db"] {
		return runDeploymentPreviewDirect(ctx, request, transport, out, deps)
	}
	client, err := deploymentHTTPClient(transport.hubURL, transport.explicit["hub-url"], deps)
	if err != nil {
		return err
	}
	result, err := client.PreviewDeploymentCreate(ctx, request)
	if err != nil {
		return fmt.Errorf("建立 deployment preview 失敗（HTTP operator API）：%w", err)
	}
	return writeDeploymentPreview(out, result, transport.json, "HTTP operator API")
}

func validateDeploymentReadCLIValue(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes {
		return fmt.Errorf("deployment read: --%s 不可為空、過長或含首尾空白", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("deployment read: --%s 不可含控制或隱形格式字元", name)
		}
	}
	return nil
}

func deploymentHTTPClient(explicitURL string, explicit bool, deps machineCommandDeps) (*operatorclient.Client, error) {
	client, _, err := deploymentHTTPClientResolved(explicitURL, explicit, deps)
	return client, err
}

func deploymentHTTPClientResolved(explicitURL string, explicit bool,
	deps machineCommandDeps,
) (*operatorclient.Client, string, error) {
	if explicit {
		if deps.newOperatorClient == nil {
			return nil, "", errors.New("deployment read: operator HTTP client 未初始化")
		}
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, "", fmt.Errorf("deployment read: 建立 HTTP operator client 失敗：%w", err)
		}
		return client, normalizedDeploymentRecoveryHubURL(explicitURL), nil
	}
	if deps.discoverHubURL == nil {
		return nil, "", errors.New("deployment read: Hub discovery 未初始化")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, "", fmt.Errorf("deployment read: 無法發現 Hub：%w", err)
	}
	if deps.newOperatorClient == nil {
		return nil, "", errors.New("deployment read: operator HTTP client 未初始化")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, "", fmt.Errorf("deployment read: 建立 discovered HTTP operator client 失敗：%w", err)
	}
	return client, normalizedDeploymentRecoveryHubURL(discovered), nil
}

// The production client applies this same endpoint parser. Keeping the raw
// fallback makes injected loopback transports usable in unit tests without
// weakening the production client's authority validation.
func normalizedDeploymentRecoveryHubURL(raw string) string {
	endpoint, err := operatorendpoint.ParseBaseURL(raw)
	if err != nil {
		return raw
	}
	return endpoint.BaseURL()
}

func withDirectDeploymentService(ctx context.Context, command, dbPath string, deps machineCommandDeps,
	run func(*operator.Service, time.Time) error,
) error {
	return withDirectOperatorStore(ctx, command, dbPath, deps, func(st *store.Store) error {
		loadExpectations(st)
		if _, err := st.CurrentWorkloadPolicyToken("operator-deployment-read-policy-proof"); err != nil {
			return fmt.Errorf("%s: shell CLAWCTL_EXPECTATIONS 與 Hub 最後發布的 workload policy 不一致；拒絕 direct DB 判決：%w", command, err)
		}
		return run(operator.NewWithArtifacts(st, artifactsDirFor(dbPath)), time.Now().UTC())
	})
}

func runDeploymentListDirect(ctx context.Context, request operator.DeploymentListRequest,
	transport deploymentReadTransport, out io.Writer, deps machineCommandDeps,
) error {
	return withDirectDeploymentService(ctx, "deployment list", transport.dbPath, deps, func(service *operator.Service, now time.Time) error {
		result, err := service.ListDeployments(request, now)
		if err != nil {
			return fmt.Errorf("讀取 deployment list 失敗（direct DB operator service）：%w", err)
		}
		return writeDeploymentList(out, result, transport.json, "direct DB operator service")
	})
}

func runDeploymentShowDirect(ctx context.Context, deploymentID string, transport deploymentReadTransport,
	out io.Writer, deps machineCommandDeps,
) error {
	return withDirectDeploymentService(ctx, "deployment show", transport.dbPath, deps, func(service *operator.Service, now time.Time) error {
		result, err := service.DeploymentDetail(deploymentID, now)
		if err != nil {
			return fmt.Errorf("讀取 deployment detail 失敗（direct DB operator service）：%w", err)
		}
		return writeDeploymentDetail(out, result, transport.json, "direct DB operator service")
	})
}

func runDeploymentPreviewDirect(ctx context.Context, request operator.DeploymentCreatePreviewRequest,
	transport deploymentReadTransport, out io.Writer, deps machineCommandDeps,
) error {
	return withDirectDeploymentService(ctx, "deployment preview", transport.dbPath, deps, func(service *operator.Service, now time.Time) error {
		result, err := service.PreviewDeploymentCreateContext(ctx, request, now)
		if err != nil {
			return fmt.Errorf("建立 deployment preview 失敗（direct DB operator service）：%w", err)
		}
		return writeDeploymentPreview(out, result, transport.json, "direct DB operator service")
	})
}

func writeDeploymentList(out io.Writer, result operator.DeploymentListResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeDeploymentJSON(out, result)
	}
	if _, err := fmt.Fprintf(out, "%s；%s consistency；Hub 評估時間 %s；符合 %d 張。\n",
		source, result.Consistency, result.EvaluatedAt.Format(time.RFC3339Nano), result.Total); err != nil {
		return err
	}
	if len(result.Items) == 0 {
		_, err := fmt.Fprintln(out, "沒有符合 filter 的 deployment。")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tCHANNEL\tVERSION\tDESIRED_REV\tCONTROL_REV\tSTATE\tBATCHES\tCOUNTS\tSTUCK\tATTEMPT"); err != nil {
		return err
	}
	for _, item := range result.Items {
		version := "unknown"
		if item.Material.Version != nil {
			version = *item.Material.Version
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%d/%d\t%s\t%d\t%d\n",
			terminalSafe(item.DeploymentID), terminalSafe(item.Channel), terminalSafe(version),
			item.DesiredRevision, item.ControlRevision, terminalSafe(item.State), item.OpenedBatch,
			item.TotalBatches, terminalSafe(formatDeploymentStateCounts(item.JobStateCounts)), item.Stuck, item.Attempt); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if result.NextCursor != nil {
		_, err := fmt.Fprintf(out, "next cursor: %s\n", terminalSafe(*result.NextCursor))
		return err
	}
	return nil
}

func writeDeploymentDetail(out io.Writer, result operator.DeploymentDetailResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeDeploymentJSON(out, result)
	}
	item := result.Item
	version := "unknown"
	if item.Material.Version != nil {
		version = *item.Material.Version
	}
	if _, err := fmt.Fprintf(out, "Stuck（%d 台；terminal %d / silent %d）\n",
		item.Stuck, item.TerminalStuck, item.SilentStuck); err != nil {
		return err
	}
	for _, target := range result.Targets {
		if target.StuckKind == nil {
			continue
		}
		jobID, state := "not_opened", "not_opened"
		if target.JobID != nil {
			jobID = *target.JobID
		}
		if target.JobState != nil {
			state = string(*target.JobState)
		}
		if _, err := fmt.Fprintf(out, "  %s：%s（%s，job %s）\n", terminalSafe(target.DisplayName),
			terminalSafe(*target.StuckKind), terminalSafe(state), terminalSafe(jobID)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out,
		"%s；%s consistency；Hub 評估時間 %s。\ndeployment %s channel=%s version=%s desired_revision=%d control_revision=%d state=%s attempt=%d\n",
		source, result.Consistency, result.EvaluatedAt.Format(time.RFC3339Nano),
		terminalSafe(item.DeploymentID), terminalSafe(item.Channel), terminalSafe(version),
		item.DesiredRevision, item.ControlRevision, terminalSafe(item.State), item.Attempt); err != nil {
		return err
	}
	if item.BoundaryPause != nil {
		if _, err := fmt.Fprintf(out, "安全閘門在 batch %d 後停住（%s；%s）\n",
			item.BoundaryPause.OpenedBatch, terminalSafe(item.BoundaryPause.Kind),
			item.BoundaryPause.PausedAt.Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out, terminalSafe(formatDeploymentStateCounts(item.JobStateCounts))); err != nil {
		return err
	}
	if err := writeDeploymentIndependentSummary(out, result.Independent); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "MACHINE\tBATCH\tJOB\tSTATE\tTERMINAL_AT\tINDEPENDENT"); err != nil {
		return err
	}
	for _, target := range result.Targets {
		if target.ExcludedReason != nil {
			continue
		}
		jobID, state, terminalAt := "-", "not_opened", "-"
		if target.JobID != nil {
			jobID = *target.JobID
		}
		if target.JobState != nil {
			state = string(*target.JobState)
		}
		if target.TerminalAt != nil {
			terminalAt = target.TerminalAt.Format(time.RFC3339Nano)
		}
		// A target with no job prints "-" rather than a verdict. Only an opened
		// job has something a second producer could have written about.
		independent := "-"
		if target.Independent != nil {
			independent = fmt.Sprintf("%s (%d rows / %d live)", target.Independent.Verdict,
				target.Independent.Rows, target.Independent.LiveProducers)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n", terminalSafe(target.DisplayName),
			target.BatchNo, terminalSafe(jobID), terminalSafe(state), terminalSafe(terminalAt),
			terminalSafe(independent)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "排除"); err != nil {
		return err
	}
	for _, target := range result.Targets {
		if target.ExcludedReason == nil {
			continue
		}
		if _, err := fmt.Fprintf(out, "  %s：%s\n", terminalSafe(target.DisplayName), terminalSafe(*target.ExcludedReason)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out,
		"actions: continue=%s (%d) retry=%s (%d) abandon=%s (%d)\n",
		deploymentEligibilityLabel(result.Actions.Continue), result.Actions.Continue.AffectedTargets,
		deploymentEligibilityLabel(result.Actions.Retry), result.Actions.Retry.AffectedTargets,
		deploymentEligibilityLabel(result.Actions.Abandon), result.Actions.Abandon.AffectedTargets)
	return err
}

func writeDeploymentPreview(out io.Writer, result operator.DeploymentCreatePreviewResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeDeploymentJSON(out, result)
	}
	if _, err := fmt.Fprintf(out,
		"%s；Hub 預覽時間 %s。\nchannel=%s version=%s batch=%d timeout=%ds irreversible=%t\nartifact=%s size=%d engines_node=%s verified=%t\n影響 %d 台，衝突 %d，缺套件 %d，unreachable %d，node 版本未知 %d，共 %d 批\n",
		source, result.PreviewedAt.Format(time.RFC3339Nano), terminalSafe(result.Channel),
		terminalSafe(result.Artifact.Version), result.BatchSize, result.ExecutionTimeoutSeconds, result.Irreversible,
		terminalSafe(result.Artifact.Digest), result.Artifact.SizeBytes, terminalSafe(result.Artifact.EnginesNode),
		result.Artifact.AvailableAndVerified, result.Impact, result.Conflicts, result.MissingPackages,
		result.Unreachable, result.UnknownNodes, result.TotalBatches); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "MACHINE\tPLAN\tNODE\tREACHABLE"); err != nil {
		return err
	}
	for _, target := range result.Targets {
		plan := "excluded"
		if target.ExcludedReason != nil {
			plan = *target.ExcludedReason
		} else {
			plan = fmt.Sprintf("batch %d", target.BatchNo)
		}
		node := "unknown"
		if target.NodeVersion != nil {
			node = *target.NodeVersion
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%t\n", terminalSafe(target.DisplayName),
			terminalSafe(plan), terminalSafe(node), target.Reachable); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if result.Promotion != nil {
		if err := writeDeploymentPromotion(out, *result.Promotion); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "create_allowed=%t blockers=%s\npreview_digest %s\n",
		result.CreateAllowed, terminalSafe(strings.Join(result.Blockers, ",")), terminalSafe(result.PreviewDigest))
	return err
}

func writeDeploymentJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func formatDeploymentStateCounts(counts []operator.JobStateCount) string {
	byState := make(map[deploy.JobState]int, len(counts))
	for _, count := range counts {
		byState[count.State] = count.Count
	}
	return formatJobCounts(byState)
}

func deploymentEligibilityLabel(eligibility operator.DeploymentActionEligibility) string {
	if eligibility.Eligible {
		return terminalSafe(eligibility.Outcome)
	}
	return terminalSafe("blocked:" + strings.Join(eligibility.Blockers, ","))
}

func deploymentReadTime(value *time.Time) string {
	if value == nil {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339Nano)
}

type deploymentMutationInputs struct {
	Action                  string
	DeploymentID            string
	Planning                operator.DeploymentCreatePreviewRequest
	ConfirmChannel          string
	ConfirmVersion          string
	ConfirmDeploymentID     string
	Reason                  string
	IdempotencyKey          string
	PreviewDigest           string
	ExpectedControlRevision *int64
	ExpectedOpenedBatch     *int
	JSON                    bool
	RecoveryFile            string
	RecoveryLoaded          bool
	RecoveryTransport       deploymentRecoveryTransport
}

type deploymentMutationOperations struct {
	source        string
	previewCreate func(operator.DeploymentCreatePreviewRequest) (operator.DeploymentCreatePreviewResult, error)
	previewAction func(string, string) (operator.DeploymentActionPreviewResult, error)
	apply         func(deploymentMutationInputs) (operator.DeploymentMutationResult, error)
}

type deploymentMutationCLIResult struct {
	Source                  string                                  `json:"source"`
	IdempotencyKey          string                                  `json:"idempotency_key"`
	PreviewDigest           string                                  `json:"preview_digest"`
	ExpectedControlRevision *int64                                  `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int                                    `json:"expected_opened_batch"`
	CreatePreview           *operator.DeploymentCreatePreviewResult `json:"create_preview,omitempty"`
	ActionPreview           *operator.DeploymentActionPreviewResult `json:"action_preview,omitempty"`
	Result                  operator.DeploymentMutationResult       `json:"result"`
}

func runDeploymentMutationCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runDeploymentMutationCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runDeploymentMutationCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	inputs, transport, err := parseDeploymentMutationCommand(argv, errOut)
	if err != nil {
		return err
	}
	if transport.explicit["db"] {
		inputs.RecoveryTransport = deploymentRecoveryTransport{Mode: deploymentRecoveryDirectDB, DBPath: transport.dbPath}
		return withDirectDeploymentService(ctx, "deployment "+inputs.Action, transport.dbPath, deps,
			func(service *operator.Service, _ time.Time) error {
				return executeDeploymentMutation(inputs, directDeploymentMutationOperations(ctx, service), out, errOut)
			})
	}
	client, resolved, err := deploymentHTTPClientResolved(transport.hubURL, transport.explicit["hub-url"], deps)
	if err != nil {
		return err
	}
	inputs.RecoveryTransport = deploymentRecoveryTransport{Mode: deploymentRecoveryHTTP, HubURL: resolved}
	return executeDeploymentMutation(inputs, httpDeploymentMutationOperations(ctx, client), out, errOut)
}

func parseDeploymentMutationCommand(argv []string, errOut io.Writer) (deploymentMutationInputs, deploymentReadTransport, error) {
	if len(argv) == 0 || (argv[0] != "create" && argv[0] != "continue" && argv[0] != "retry" && argv[0] != "abandon") {
		return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment mutation: 必須指定 create、continue、retry 或 abandon")
	}
	action, args := argv[0], argv[1:]
	fs := flag.NewFlagSet("deployment "+action, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintf(errOut, "用法：clawctl-hub deployment %s [flags]", action)
		if action != "create" {
			fmt.Fprint(errOut, " <deployment-id>")
		}
		fmt.Fprintln(errOut)
		fmt.Fprintln(errOut, "  正常模式先走 HTTP operator preview 再 apply；--db 僅供 Hub 完全停止時的 fenced break-glass。")
		fmt.Fprintln(errOut, "  HTTP apply 前會建立 private recovery file；以同一 action 加 --recovery-file 即可安全 replay。")
		fmt.Fprintln(errOut, "  仍可用完整 canonical retry flags；省略整組才會建立新 request。")
		fs.PrintDefaults()
	}
	hubURL, dbPath, jsonOutput := addDeploymentReadTransportFlags(fs)
	channel := fs.String("channel", "", "create 的 canary 或 stable channel")
	version := fs.String("version", "", "create 的 OpenClaw 版本")
	artifactSHA := fs.String("artifact", "", "create 指定 artifact SHA-256（64 個小寫 hex）")
	batch := fs.Int("batch", operator.DefaultDeploymentBatchSize, "create 後續每批台數（1 到 5）。第一批永遠是 1 台；省略或 1 代表後面也是 1 台")
	timeout := fs.Int("timeout", operator.DefaultDeploymentTimeout, "create 執行逾時（秒）")
	irreversible := fs.Bool("irreversible", false, "create 失敗不可逆")
	confirmChannel := fs.String("confirm-channel", "", "逐字確認 canary 或 stable channel")
	confirmVersion := fs.String("confirm-version", "", "逐字確認 OpenClaw 版本")
	confirmDeploymentID := fs.String("confirm", "", "abandon 時逐字輸入完整 deployment_id")
	reason := fs.String("reason", "", "變更理由（選填，最多 500 bytes）")
	idempotencyKey := fs.String("idempotency-key", "", "ambiguous retry 的原 request key")
	previewDigest := fs.String("preview-digest", "", "ambiguous retry 的原 preview digest")
	expectedRevision := fs.Int64("expected-control-revision", 0, "ambiguous retry 的原 control revision")
	expectedOpened := fs.Int("expected-opened-batch", 0, "ambiguous retry 的原 opened batch")
	recoveryFile := fs.String("recovery-file", "", "private canonical recovery file（省略時 HTTP mode 自動建立）")

	deploymentID := ""
	if action != "create" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		deploymentID, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return deploymentMutationInputs{}, deploymentReadTransport{}, err
	}
	transport, err := deploymentReadTransportFromFlags(fs, *hubURL, *dbPath, *jsonOutput)
	if err != nil {
		return deploymentMutationInputs{}, deploymentReadTransport{}, err
	}
	if transport.explicit["recovery-file"] {
		document, exists, err := loadDeploymentRecoveryIfExists(*recoveryFile)
		if err != nil {
			return deploymentMutationInputs{}, deploymentReadTransport{}, err
		}
		if exists {
			if document.Action != action {
				return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment recovery file action 與 subcommand 不符")
			}
			if deploymentID != "" || fs.NArg() != 0 {
				return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment recovery replay 不接受 positional deployment-id")
			}
			for name := range transport.explicit {
				if name != "recovery-file" && name != "json" {
					return deploymentMutationInputs{}, deploymentReadTransport{},
						errors.New("deployment recovery replay 只接受 --recovery-file 與 --json；request/transport 以檔案為準")
				}
			}
			inputs, recoveredTransport := deploymentInputsFromRecovery(document, *recoveryFile, *jsonOutput)
			return inputs, recoveredTransport, nil
		}
	}
	if action == "create" {
		if fs.NArg() != 0 {
			return deploymentMutationInputs{}, deploymentReadTransport{}, fmt.Errorf("deployment create: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
		}
	} else if deploymentID == "" {
		if fs.NArg() != 1 {
			return deploymentMutationInputs{}, deploymentReadTransport{}, fmt.Errorf("deployment %s: 必須提供且只提供一個 deployment-id", action)
		}
		deploymentID = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return deploymentMutationInputs{}, deploymentReadTransport{}, fmt.Errorf("deployment %s: 只接受一個 deployment-id", action)
	}
	if action != "create" {
		if err := validateDeploymentIdentifierCLI(deploymentID); err != nil {
			return deploymentMutationInputs{}, deploymentReadTransport{}, fmt.Errorf("deployment %s: %w", action, err)
		}
	}
	if err := rejectDeploymentMutationFlags(action, transport.explicit); err != nil {
		return deploymentMutationInputs{}, deploymentReadTransport{}, err
	}
	if transport.explicit["reason"] {
		if *reason == "" || *reason != strings.TrimSpace(*reason) || len(*reason) > 500 || containsDeploymentControl(*reason) {
			return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment mutation: --reason 不可為空、超過 500 bytes、含首尾空白或控制字元")
		}
	}
	inputs := deploymentMutationInputs{
		Action: action, DeploymentID: deploymentID, ConfirmChannel: *confirmChannel,
		ConfirmVersion: *confirmVersion, ConfirmDeploymentID: *confirmDeploymentID,
		Reason: *reason, JSON: *jsonOutput, RecoveryFile: *recoveryFile,
	}
	if action == "create" {
		if !transport.explicit["channel"] || (*channel != "canary" && *channel != "stable") {
			return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment create: --channel 必填且只接受 canary 或 stable")
		}
		if !transport.explicit["version"] {
			return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment create: --version 必填")
		}
		if err := validateDeploymentReadCLIValue("version", *version, 128); err != nil {
			return deploymentMutationInputs{}, deploymentReadTransport{}, err
		}
		if transport.explicit["artifact"] && !artifact.ValidSHA256Hex(*artifactSHA) {
			return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment create: --artifact 必須是 64 個小寫 hex")
		}
		if *batch < 1 || *batch > store.MaxDeploymentBatchSize {
			return deploymentMutationInputs{}, deploymentReadTransport{}, fmt.Errorf("deployment create: --batch 必須介於 1 與 %d", store.MaxDeploymentBatchSize)
		}
		if *timeout < 1 || *timeout > 86400 {
			return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment create: --timeout 必須介於 1 與 86400 秒")
		}
		if !transport.explicit["confirm-channel"] || !transport.explicit["confirm-version"] ||
			*confirmChannel != *channel || *confirmVersion != *version {
			return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment create: --confirm-channel 與 --confirm-version 必須逐字等於 --channel 與 --version")
		}
		inputs.Planning = operator.DeploymentCreatePreviewRequest{
			Channel: *channel, Version: *version, ArtifactSHA256: *artifactSHA,
			BatchSize: *batch, ExecutionTimeoutSeconds: *timeout, Irreversible: *irreversible,
		}
	} else if action == "abandon" {
		if !transport.explicit["confirm"] || *confirmDeploymentID != deploymentID {
			return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment abandon: --confirm 必須逐字等於完整 deployment-id")
		}
	} else {
		if !transport.explicit["confirm-channel"] || (*confirmChannel != "canary" && *confirmChannel != "stable") {
			return deploymentMutationInputs{}, deploymentReadTransport{}, fmt.Errorf("deployment %s: --confirm-channel 必填且只接受 canary 或 stable", action)
		}
		if action == "retry" {
			if !transport.explicit["confirm-version"] {
				return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment retry: --confirm-version 必填")
			}
			if err := validateDeploymentReadCLIValue("confirm-version", *confirmVersion, 128); err != nil {
				return deploymentMutationInputs{}, deploymentReadTransport{}, err
			}
		}
	}

	group := []string{"idempotency-key", "preview-digest"}
	if action != "create" {
		group = append(group, "expected-control-revision", "expected-opened-batch")
	}
	provided := 0
	for _, name := range group {
		if transport.explicit[name] {
			provided++
		}
	}
	if provided != 0 && provided != len(group) {
		return deploymentMutationInputs{}, deploymentReadTransport{}, fmt.Errorf("deployment %s: ambiguous retry 必須一起提供 %s；省略整組才會建立新 request", action, strings.Join(group, ", --"))
	}
	if provided == len(group) {
		if err := validateDeploymentIdempotencyKey(*idempotencyKey); err != nil {
			return deploymentMutationInputs{}, deploymentReadTransport{}, err
		}
		if !validDeploymentPreviewDigest(*previewDigest) {
			return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment mutation: --preview-digest 必須是 sha256: 加 64 個小寫 hex")
		}
		inputs.IdempotencyKey, inputs.PreviewDigest = *idempotencyKey, *previewDigest
		if action != "create" {
			if *expectedRevision < 0 || *expectedOpened < 0 {
				return deploymentMutationInputs{}, deploymentReadTransport{}, errors.New("deployment mutation: expected control revision/opened batch 不可為負數")
			}
			inputs.ExpectedControlRevision, inputs.ExpectedOpenedBatch = expectedRevision, expectedOpened
		}
	}
	return inputs, transport, nil
}

func rejectDeploymentMutationFlags(action string, seen map[string]bool) error {
	allowed := map[string]bool{
		"hub-url": true, "db": true, "json": true, "reason": true,
		"idempotency-key": true, "preview-digest": true, "recovery-file": true,
	}
	switch action {
	case "create":
		for _, name := range []string{"channel", "version", "artifact", "batch", "timeout", "irreversible", "confirm-channel", "confirm-version"} {
			allowed[name] = true
		}
	case "continue":
		allowed["confirm-channel"], allowed["expected-control-revision"], allowed["expected-opened-batch"] = true, true, true
	case "retry":
		allowed["confirm-channel"], allowed["confirm-version"] = true, true
		allowed["expected-control-revision"], allowed["expected-opened-batch"] = true, true
	case "abandon":
		allowed["confirm"], allowed["expected-control-revision"], allowed["expected-opened-batch"] = true, true, true
	}
	for name := range seen {
		if !allowed[name] {
			return fmt.Errorf("deployment %s: 不接受 --%s", action, name)
		}
	}
	return nil
}

func validateDeploymentIdentifierCLI(value string) error {
	if err := validateDeploymentReadCLIValue("deployment-id", value, 256); err != nil ||
		strings.Contains(value, "/") || value == "." || value == ".." {
		return errors.New("deployment-id 不可含首尾空白、控制字元、dot segment 或斜線，且長度不可超過 256 bytes")
	}
	return nil
}

func validateDeploymentIdempotencyKey(value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 200 || containsDeploymentControl(value) {
		return errors.New("deployment mutation: --idempotency-key 不可為空、超過 200 bytes、含首尾空白或控制字元")
	}
	return nil
}

func validDeploymentPreviewDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && artifact.ValidSHA256Hex(strings.TrimPrefix(value, "sha256:"))
}

func containsDeploymentControl(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return true
		}
	}
	return false
}

func httpDeploymentMutationOperations(ctx context.Context, client *operatorclient.Client) deploymentMutationOperations {
	return deploymentMutationOperations{
		source: "HTTP operator API",
		previewCreate: func(request operator.DeploymentCreatePreviewRequest) (operator.DeploymentCreatePreviewResult, error) {
			return client.PreviewDeploymentCreate(ctx, request)
		},
		previewAction: func(action, deploymentID string) (operator.DeploymentActionPreviewResult, error) {
			switch action {
			case "continue":
				return client.PreviewDeploymentContinue(ctx, deploymentID)
			case "retry":
				return client.PreviewDeploymentRetry(ctx, deploymentID)
			case "abandon":
				return client.PreviewDeploymentAbandon(ctx, deploymentID)
			default:
				return operator.DeploymentActionPreviewResult{}, errors.New("deployment mutation action 不合法")
			}
		},
		apply: func(inputs deploymentMutationInputs) (operator.DeploymentMutationResult, error) {
			switch inputs.Action {
			case "create":
				return client.CreateDeployment(ctx, inputs.IdempotencyKey, operatorclient.DeploymentCreateRequest{
					Channel: inputs.Planning.Channel, Version: inputs.Planning.Version,
					ArtifactSHA256: inputs.Planning.ArtifactSHA256, BatchSize: inputs.Planning.BatchSize,
					ExecutionTimeoutSeconds: inputs.Planning.ExecutionTimeoutSeconds,
					Irreversible:            inputs.Planning.Irreversible, PreviewDigest: inputs.PreviewDigest,
					ConfirmChannel: inputs.ConfirmChannel, ConfirmVersion: inputs.ConfirmVersion, Reason: inputs.Reason,
				})
			case "continue":
				return client.ContinueDeployment(ctx, inputs.DeploymentID, inputs.IdempotencyKey, operatorclient.DeploymentContinueRequest{
					PreviewDigest: inputs.PreviewDigest, ExpectedControlRevision: inputs.ExpectedControlRevision,
					ExpectedOpenedBatch: inputs.ExpectedOpenedBatch, ConfirmChannel: inputs.ConfirmChannel, Reason: inputs.Reason,
				})
			case "retry":
				return client.RetryDeployment(ctx, inputs.DeploymentID, inputs.IdempotencyKey, operatorclient.DeploymentRetryRequest{
					PreviewDigest: inputs.PreviewDigest, ExpectedControlRevision: inputs.ExpectedControlRevision,
					ExpectedOpenedBatch: inputs.ExpectedOpenedBatch, ConfirmChannel: inputs.ConfirmChannel,
					ConfirmVersion: inputs.ConfirmVersion, Reason: inputs.Reason,
				})
			case "abandon":
				return client.AbandonDeployment(ctx, inputs.DeploymentID, inputs.IdempotencyKey, operatorclient.DeploymentAbandonRequest{
					PreviewDigest: inputs.PreviewDigest, ExpectedControlRevision: inputs.ExpectedControlRevision,
					ExpectedOpenedBatch: inputs.ExpectedOpenedBatch, ConfirmDeploymentID: inputs.ConfirmDeploymentID,
					Reason: inputs.Reason,
				})
			default:
				return operator.DeploymentMutationResult{}, errors.New("deployment mutation action 不合法")
			}
		},
	}
}

func directDeploymentMutationOperations(ctx context.Context, service *operator.Service) deploymentMutationOperations {
	actor := operator.Actor{
		SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
		UserAgent: "clawctl-hub deployment", SourceKind: operator.SourceKindDirectDBCLI,
	}
	return deploymentMutationOperations{
		source: "direct DB operator service",
		previewCreate: func(request operator.DeploymentCreatePreviewRequest) (operator.DeploymentCreatePreviewResult, error) {
			return service.PreviewDeploymentCreateContext(ctx, request, time.Now().UTC())
		},
		previewAction: func(action, deploymentID string) (operator.DeploymentActionPreviewResult, error) {
			now := time.Now().UTC()
			switch action {
			case "continue":
				return service.PreviewDeploymentContinueContext(ctx, operator.DeploymentContinuePreviewRequest{DeploymentID: deploymentID}, now)
			case "retry":
				return service.PreviewDeploymentRetryContext(ctx, operator.DeploymentRetryPreviewRequest{DeploymentID: deploymentID}, now)
			case "abandon":
				return service.PreviewDeploymentAbandonContext(ctx, operator.DeploymentAbandonPreviewRequest{DeploymentID: deploymentID}, now)
			default:
				return operator.DeploymentActionPreviewResult{}, errors.New("deployment mutation action 不合法")
			}
		},
		apply: func(inputs deploymentMutationInputs) (operator.DeploymentMutationResult, error) {
			switch inputs.Action {
			case "create":
				return service.ApplyDeploymentCreateContext(ctx, operator.DeploymentCreateApplyRequest{
					DeploymentCreatePreviewRequest: inputs.Planning, PreviewDigest: inputs.PreviewDigest,
					ConfirmChannel: inputs.ConfirmChannel, ConfirmVersion: inputs.ConfirmVersion,
					Reason: inputs.Reason, IdempotencyKey: inputs.IdempotencyKey, Actor: actor,
				})
			case "continue":
				return service.ApplyDeploymentContinueContext(ctx, operator.DeploymentContinueApplyRequest{
					DeploymentID: inputs.DeploymentID, PreviewDigest: inputs.PreviewDigest,
					ExpectedControlRevision: inputs.ExpectedControlRevision, ExpectedOpenedBatch: inputs.ExpectedOpenedBatch,
					ConfirmChannel: inputs.ConfirmChannel, Reason: inputs.Reason,
					IdempotencyKey: inputs.IdempotencyKey, Actor: actor,
				})
			case "retry":
				return service.ApplyDeploymentRetryContext(ctx, operator.DeploymentRetryApplyRequest{
					DeploymentID: inputs.DeploymentID, PreviewDigest: inputs.PreviewDigest,
					ExpectedControlRevision: inputs.ExpectedControlRevision, ExpectedOpenedBatch: inputs.ExpectedOpenedBatch,
					ConfirmChannel: inputs.ConfirmChannel, ConfirmVersion: inputs.ConfirmVersion,
					Reason: inputs.Reason, IdempotencyKey: inputs.IdempotencyKey, Actor: actor,
				})
			case "abandon":
				return service.ApplyDeploymentAbandonContext(ctx, operator.DeploymentAbandonApplyRequest{
					DeploymentID: inputs.DeploymentID, PreviewDigest: inputs.PreviewDigest,
					ExpectedControlRevision: inputs.ExpectedControlRevision, ExpectedOpenedBatch: inputs.ExpectedOpenedBatch,
					ConfirmDeploymentID: inputs.ConfirmDeploymentID, Reason: inputs.Reason,
					IdempotencyKey: inputs.IdempotencyKey, Actor: actor,
				})
			default:
				return operator.DeploymentMutationResult{}, errors.New("deployment mutation action 不合法")
			}
		},
	}
}

func executeDeploymentMutation(inputs deploymentMutationInputs, operations deploymentMutationOperations,
	out, errOut io.Writer,
) error {
	var createPreview *operator.DeploymentCreatePreviewResult
	var actionPreview *operator.DeploymentActionPreviewResult
	if inputs.PreviewDigest == "" {
		if inputs.Action == "create" {
			preview, err := operations.previewCreate(inputs.Planning)
			if err != nil {
				return fmt.Errorf("deployment create preview（%s）失敗：%w", operations.source, err)
			}
			createPreview = &preview
			inputs.PreviewDigest = preview.PreviewDigest
			if err := writeDeploymentMutationCreatePreview(errOut, preview, inputs.JSON, operations.source); err != nil {
				return err
			}
		} else {
			preview, err := operations.previewAction(inputs.Action, inputs.DeploymentID)
			if err != nil {
				return fmt.Errorf("deployment %s preview（%s）失敗：%w", inputs.Action, operations.source, err)
			}
			actionPreview = &preview
			inputs.PreviewDigest = preview.PreviewDigest
			revision, opened := preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch
			inputs.ExpectedControlRevision, inputs.ExpectedOpenedBatch = &revision, &opened
			if err := writeDeploymentActionPreview(errOut, preview, inputs.JSON, operations.source); err != nil {
				return err
			}
		}
		key, err := operator.NewIdempotencyKey("cli-deployment-" + inputs.Action)
		if err != nil {
			return fmt.Errorf("deployment %s 產生 idempotency key：%w", inputs.Action, err)
		}
		inputs.IdempotencyKey = key
	}
	if inputs.RecoveryFile == "" && inputs.RecoveryTransport.Mode == deploymentRecoveryHTTP {
		path, err := defaultDeploymentRecoveryPath(inputs.Action, inputs.IdempotencyKey)
		if err != nil {
			return fmt.Errorf("deployment %s 建立 private recovery path：%w", inputs.Action, err)
		}
		inputs.RecoveryFile = path
	}
	var recovery *deploymentMutationRecovery
	if inputs.RecoveryFile != "" {
		document := deploymentRecoveryFromInputs(inputs)
		if err := validateDeploymentRecoveryDocument(document); err != nil {
			return err
		}
		if err := ensureDeploymentRecovery(inputs.RecoveryFile, document, inputs.RecoveryLoaded); err != nil {
			return err
		}
		recovery = &document
		if err := writeDeploymentRecoveryInstruction(errOut, inputs.Action, inputs.RecoveryFile); err != nil {
			return err
		}
	}
	if err := writeDeploymentCanonicalRetry(errOut, inputs); err != nil {
		return err
	}
	if recovery != nil {
		if err := durablyVerifyDeploymentRecovery(inputs.RecoveryFile, *recovery); err != nil {
			return err
		}
	}
	result, err := operations.apply(inputs)
	if err != nil {
		return fmt.Errorf("deployment %s（%s；%s%s）失敗%s：%w", inputs.Action, operations.source,
			deploymentCanonicalCoordinates(inputs), deploymentConfirmationCoordinates(inputs),
			operatorRejectionReplayNote(err), err)
	}
	if err := writeDeploymentMutationResult(out, deploymentMutationCLIResult{
		Source: operations.source, IdempotencyKey: inputs.IdempotencyKey, PreviewDigest: inputs.PreviewDigest,
		ExpectedControlRevision: inputs.ExpectedControlRevision, ExpectedOpenedBatch: inputs.ExpectedOpenedBatch,
		CreatePreview: createPreview, ActionPreview: actionPreview, Result: result,
	}, inputs.JSON); err != nil {
		return err
	}
	if recovery != nil {
		if err := removeDeploymentRecovery(inputs.RecoveryFile, *recovery); err != nil {
			return err
		}
	}
	return nil
}

func writeDeploymentMutationCreatePreview(w io.Writer, preview operator.DeploymentCreatePreviewResult,
	jsonOutput bool, source string,
) error {
	if jsonOutput {
		return writeDeploymentJSON(w, preview)
	}
	return writeDeploymentPreview(w, preview, false, source)
}

func writeDeploymentActionPreview(w io.Writer, preview operator.DeploymentActionPreviewResult,
	jsonOutput bool, source string,
) error {
	if jsonOutput {
		return writeDeploymentJSON(w, preview)
	}
	version := "unknown"
	if preview.Deployment.Material.Version != nil {
		version = *preview.Deployment.Material.Version
	}
	if _, err := fmt.Fprintf(w,
		"%s preview: action=%s deployment=%s channel=%s version=%s control_revision=%d opened_batch=%d/%d eligible=%t affected=%d blockers=%s\n",
		source, terminalSafe(preview.Action), terminalSafe(preview.Deployment.DeploymentID),
		terminalSafe(preview.Deployment.Channel), terminalSafe(version), preview.Deployment.ControlRevision,
		preview.Deployment.OpenedBatch, preview.Deployment.TotalBatches, preview.Eligibility.Eligible,
		preview.Eligibility.AffectedTargets, terminalSafe(strings.Join(preview.Eligibility.Blockers, ","))); err != nil {
		return err
	}
	// ⚠ DeploymentBlockerLabel 的 default 分支會把來自 wire 的 blocker token 原樣串進句子；這是唯一會進到終端機的非常數字串，所以一定要經過 terminalSafe。
	impact := terminalSafe(operator.DeploymentActionImpact(preview.Action, preview.Eligibility))
	if preview.Eligibility.Eligible {
		if _, err := fmt.Fprintf(w, "確認後會發生：%s\n", impact); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(w, "目前不可執行：%s\n", impact); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "MACHINE\tBATCH\tJOB\tSTATE\tPLAN"); err != nil {
		return err
	}
	for _, target := range preview.Targets {
		jobID, state, plan := "-", "not_opened", preview.Eligibility.Outcome
		if target.JobID != nil {
			jobID = *target.JobID
		}
		if target.JobState != nil {
			state = string(*target.JobState)
		}
		if target.ExcludedReason != nil {
			plan = *target.ExcludedReason
		}
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", terminalSafe(target.DisplayName), target.BatchNo,
			terminalSafe(jobID), terminalSafe(state), terminalSafe(plan)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(preview.TerminalFailureTargets) > 0 {
		terminalWriter := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(terminalWriter, "MACHINE\tJOB\tOUTCOME\tSTATE"); err != nil {
			return err
		}
		for _, target := range preview.TerminalFailureTargets {
			if _, err := fmt.Fprintf(terminalWriter, "%s\t%s\t%s\t%s\n",
				terminalSafe(target.DisplayName), terminalSafe(target.JobID),
				terminalSafe(operator.TerminalJobOutcome(target.JobState)), terminalSafe(string(target.JobState))); err != nil {
				return err
			}
		}
		if err := terminalWriter.Flush(); err != nil {
			return err
		}
	}
	if preview.Promotion != nil {
		if err := writeDeploymentPromotion(w, *preview.Promotion); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "preview_digest %s\n", terminalSafe(preview.PreviewDigest))
	return err
}

func writeDeploymentPromotion(out io.Writer, promotion operator.DeploymentPromotionPreview) error {
	if _, err := fmt.Fprintf(out,
		"stable promotion: allowed=%t blockers=%s earliest_at=%s independent=%d/%d\n",
		promotion.Allowed, terminalSafe(strings.Join(promotion.Blockers, ",")),
		deploymentReadTime(promotion.EarliestAt), promotion.IndependentPassedTargets,
		len(promotion.IndependentTargets)); err != nil {
		return err
	}
	if len(promotion.IndependentTargets) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "CANARY MACHINE\tJOB\tINDEPENDENT\t下一步"); err != nil {
		return err
	}
	for _, target := range promotion.IndependentTargets {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			terminalSafe(target.DisplayName), terminalSafe(target.JobID), terminalSafe(target.State),
			terminalSafe(deploymentPromotionNextStepText(target.NextStep))); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func deploymentPromotionNextStepText(step string) string {
	switch step {
	case operator.PromotionNextStepNone:
		return "無"
	case operator.PromotionNextStepAssignVerifier:
		return "到工作單指派跨故障域 verifier"
	case operator.PromotionNextStepWaitForVerifier:
		return "等待已指派 verifier 完整回報"
	case operator.PromotionNextStepRerunVerifier:
		return "重新執行已指派 verifier"
	case operator.PromotionNextStepAssignActiveVerifier:
		return "到工作單指派仍有效的 verifier"
	case operator.PromotionNextStepReassignVerifier:
		return "重新指派 verifier"
	case operator.PromotionNextStepUpgradeAndReassignVerifier:
		return "升級後重新指派 verifier"
	case operator.PromotionNextStepRepairAndRerunCanary:
		return "修復後重跑 canary"
	case operator.PromotionNextStepRerunCanary:
		return "確認 artifact 後重跑 canary"
	default:
		return "查看工作單"
	}
}

func writeDeploymentCanonicalRetry(w io.Writer, inputs deploymentMutationInputs) error {
	_, err := fmt.Fprintf(w, "canonical retry: %s%s\n", deploymentCanonicalCoordinates(inputs), deploymentConfirmationCoordinates(inputs))
	return err
}

func deploymentCanonicalCoordinates(inputs deploymentMutationInputs) string {
	value := fmt.Sprintf("idempotency-key=%s preview-digest=%s", terminalSafe(inputs.IdempotencyKey), terminalSafe(inputs.PreviewDigest))
	if inputs.ExpectedControlRevision != nil && inputs.ExpectedOpenedBatch != nil {
		value += fmt.Sprintf(" expected-control-revision=%d expected-opened-batch=%d",
			*inputs.ExpectedControlRevision, *inputs.ExpectedOpenedBatch)
	}
	return value
}

func deploymentConfirmationCoordinates(inputs deploymentMutationInputs) string {
	switch inputs.Action {
	case "create", "retry":
		return fmt.Sprintf(" confirm-channel=%s confirm-version=%s", terminalSafe(inputs.ConfirmChannel), terminalSafe(inputs.ConfirmVersion))
	case "continue":
		return fmt.Sprintf(" confirm-channel=%s", terminalSafe(inputs.ConfirmChannel))
	case "abandon":
		return fmt.Sprintf(" confirm=%s", terminalSafe(inputs.ConfirmDeploymentID))
	default:
		return ""
	}
}

func writeDeploymentMutationResult(w io.Writer, result deploymentMutationCLIResult, jsonOutput bool) error {
	if jsonOutput {
		return writeDeploymentJSON(w, result)
	}
	replay := "fresh"
	if result.Result.Replayed {
		replay = "idempotency replay"
	}
	if _, err := fmt.Fprintf(w,
		"%s: deployment %s action=%s state=%s desired_revision=%d control_revision=%d opened_batch=%d；new_jobs=%d；%s；idempotency-key=%s preview-digest=%s\n",
		result.Source, terminalSafe(result.Result.DeploymentID), terminalSafe(result.Result.Action),
		terminalSafe(result.Result.State), result.Result.DesiredRevision, result.Result.ControlRevision,
		result.Result.OpenedBatch, len(result.Result.Jobs), replay,
		terminalSafe(result.IdempotencyKey), terminalSafe(result.PreviewDigest)); err != nil {
		return err
	}
	for _, job := range result.Result.Jobs {
		if _, err := fmt.Fprintf(w, "  %s → %s（%s）\n", terminalSafe(job.MachineID), terminalSafe(job.JobID), terminalSafe(string(job.State))); err != nil {
			return err
		}
	}
	return nil
}

func formatJobCounts(counts map[deploy.JobState]int) string {
	var parts []string
	// 已完成先講，讓 Hub 重啟後的典型句子直接讀成
	// 「18 succeeded / 12 not_started」，跟 docs/PHASES.md 完成判準一致。
	for _, s := range []deploy.JobState{
		deploy.Succeeded, deploy.Failed, deploy.ManualIntervention, deploy.Rejected,
		deploy.LeaseExpired, deploy.Running, deploy.Verifying, deploy.Claimed, deploy.NotStarted,
	} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	if len(parts) == 0 {
		return "0 jobs"
	}
	return strings.Join(parts, " / ")
}

// writeDeploymentIndependentSummary states how many opened targets a second
// producer spoke for. It counts targets and never calls the deployment
// verified: the counts are what the verifiers reported, not the Hub's own
// judgement about this rollout.
func writeDeploymentIndependentSummary(out io.Writer, summary operator.DeploymentIndependentSummary) error {
	if _, err := fmt.Fprintf(out,
		"independent: %d/%d 台已開單 target 有第二個 producer 回報通過；可用 producer %d\n",
		summary.PassedTargets, summary.OpenedTargets, summary.LiveProducers); err != nil {
		return err
	}
	for _, count := range summary.Verdicts {
		if count.Targets == 0 {
			continue
		}
		if _, err := fmt.Fprintf(out, "  %s：%d 台——%s\n", terminalSafe(count.Verdict), count.Targets,
			terminalSafe(jobIndependentVerdictStatement(count.Verdict))); err != nil {
			return err
		}
	}
	return nil
}
