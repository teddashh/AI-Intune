package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type repeatedJobStates []string

func (v *repeatedJobStates) String() string { return strings.Join(*v, ",") }
func (v *repeatedJobStates) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func runJobReadCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runJobReadCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runJobReadCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	if len(argv) == 0 || (argv[0] != "list" && argv[0] != "show" && argv[0] != "evidence") {
		return errors.New("job read: 必須指定 list、show 或 evidence")
	}
	action := argv[0]
	fs := flag.NewFlagSet("job "+action, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub job list [filters] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub job show [--json] [--hub-url URL | --db PATH] <job-id>")
		fmt.Fprintln(errOut, "      clawctl-hub job evidence [--limit N] [--json] [--hub-url URL | --db PATH] <job-id>")
		fmt.Fprintln(errOut, "  正常模式走 HTTP operator API；--db 僅供 Hub 完全停止時的 fenced break-glass。")
		fs.PrintDefaults()
	}
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	machine := fs.String("machine", "", "HTTP: machine_id；direct DB: display_name 或 machine_id")
	deploymentID := fs.String("deployment", "", "只看這個 deployment_id")
	resourceKind := fs.String("resource-kind", "", "只看這個 resource kind")
	resourceID := fs.String("resource-id", "", "只看這個 resource id（必須搭配 --resource-kind）")
	defaultLimit := operator.DefaultJobReadLimit
	if action == "evidence" {
		defaultLimit = 0
	}
	limit := fs.Int("limit", defaultLimit, "每頁／每個 evidence section 的項目上限（1..100）")
	cursor := fs.String("cursor", "", "上一頁回傳的 opaque next cursor")
	jsonOutput := fs.Bool("json", false, "輸出 stable operator JSON DTO")
	var stateValues repeatedJobStates
	fs.Var(&stateValues, "state", "只看這個 canonical state；可重複")
	parseArgs := argv[1:]
	// The evidence command's documented shape puts job-id first. flag.FlagSet
	// stops at the first positional argument, so normalize that one form while
	// continuing to accept the established flags-before-id CLI style.
	if action == "evidence" && len(parseArgs) > 0 && !strings.HasPrefix(parseArgs[0], "-") {
		parseArgs = append(append([]string(nil), parseArgs[1:]...), parseArgs[0])
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("job read: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if seen["hub-url"] && strings.TrimSpace(*hubURL) == "" {
		return errors.New("job read: --hub-url 不可為空")
	}
	if seen["db"] && strings.TrimSpace(*dbPath) == "" {
		return errors.New("job read: --db 不可為空")
	}
	if action == "show" {
		for _, name := range []string{"machine", "deployment", "resource-kind", "resource-id", "limit", "cursor", "state"} {
			if seen[name] {
				return fmt.Errorf("job show: 不接受 --%s", name)
			}
		}
		if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
			return errors.New("job show: 必須提供一個 job-id")
		}
		if err := validateJobReadCLIValue("job-id", fs.Arg(0), 256); err != nil ||
			strings.Contains(fs.Arg(0), "/") || fs.Arg(0) == "." || fs.Arg(0) == ".." {
			return errors.New("job show: job-id 不可含首尾空白、控制字元、dot segment 或斜線，且長度不可超過 256 bytes")
		}
		return runJobShow(ctx, fs.Arg(0), *hubURL, *dbPath, seen, *jsonOutput, out, deps)
	}
	if action == "evidence" {
		for _, name := range []string{"machine", "deployment", "resource-kind", "resource-id", "cursor", "state"} {
			if seen[name] {
				return fmt.Errorf("job evidence: 不接受 --%s", name)
			}
		}
		if fs.NArg() != 1 || strings.TrimSpace(fs.Arg(0)) == "" {
			return errors.New("job evidence: 必須提供一個 job-id")
		}
		if err := validateJobReadCLIValue("job-id", fs.Arg(0), 256); err != nil ||
			strings.Contains(fs.Arg(0), "/") || fs.Arg(0) == "." || fs.Arg(0) == ".." {
			return errors.New("job evidence: job-id 不可含首尾空白、控制字元、dot segment 或斜線，且長度不可超過 256 bytes")
		}
		if seen["limit"] && (*limit < 1 || *limit > operator.JobEvidenceDefaultLimit) {
			return fmt.Errorf("job evidence: --limit 必須介於 1 與 %d", operator.JobEvidenceDefaultLimit)
		}
		return runJobEvidence(ctx, fs.Arg(0), *limit, *hubURL, *dbPath, seen, *jsonOutput, out, deps)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("job list: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if seen["limit"] && (*limit < 1 || *limit > operator.MaxJobReadLimit) {
		return fmt.Errorf("job list: --limit 必須介於 1 與 %d", operator.MaxJobReadLimit)
	}
	for _, input := range []struct {
		name, value string
		max         int
	}{
		{"machine", *machine, 256},
		{"deployment", *deploymentID, 256},
		{"resource-kind", *resourceKind, 128},
		{"resource-id", *resourceID, 256},
		{"cursor", *cursor, 2048},
	} {
		if seen[input.name] {
			if err := validateJobReadCLIValue(input.name, input.value, input.max); err != nil {
				return err
			}
		}
	}
	if *resourceID != "" && *resourceKind == "" {
		return errors.New("job list: --resource-id 必須搭配 --resource-kind")
	}
	request := operator.JobListRequest{
		MachineID: *machine, DeploymentID: *deploymentID,
		ResourceKind: *resourceKind, ResourceID: *resourceID,
		Limit: *limit, Cursor: *cursor,
	}
	seenStates := make(map[deploy.JobState]bool, len(stateValues))
	for _, value := range stateValues {
		state := deploy.JobState(value)
		if !deploy.IsKnownJobState(state) || seenStates[state] {
			return fmt.Errorf("job list: --state %q 不是 canonical state 或重複", value)
		}
		seenStates[state] = true
		request.States = append(request.States, state)
	}
	if seen["db"] {
		return runJobListDirect(ctx, request, *dbPath, *jsonOutput, out, deps)
	}
	client, err := jobHTTPClient(*hubURL, seen["hub-url"], deps)
	if err != nil {
		return err
	}
	result, err := client.Jobs(ctx, request)
	if err != nil {
		return fmt.Errorf("讀取 job list 失敗（HTTP operator API）：%w", err)
	}
	return writeJobList(out, result, *jsonOutput, "HTTP operator API")
}

func runJobEvidence(ctx context.Context, jobID string, limit int, hubURL, dbPath string,
	seen map[string]bool, jsonOutput bool, out io.Writer, deps machineCommandDeps,
) error {
	if seen["db"] {
		return withDirectOperatorStore(ctx, "job evidence", dbPath, deps, func(st *store.Store) error {
			result, err := operator.New(st).JobEvidence(operator.JobEvidenceRequest{JobID: jobID, Limit: limit}, time.Now().UTC())
			if err != nil {
				return fmt.Errorf("讀取 job evidence 失敗（direct DB operator service）：%w", err)
			}
			return writeJobEvidence(out, result, jsonOutput, "direct DB operator service")
		})
	}
	client, err := jobHTTPClient(hubURL, seen["hub-url"], deps)
	if err != nil {
		return err
	}
	result, err := client.JobEvidence(ctx, jobID, limit)
	if err != nil {
		return fmt.Errorf("讀取 job evidence 失敗（HTTP operator API）：%w", err)
	}
	return writeJobEvidence(out, result, jsonOutput, "HTTP operator API")
}

func validateJobReadCLIValue(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes {
		return fmt.Errorf("job read: --%s 不可為空、過長或含首尾空白", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("job read: --%s 不可含控制或隱形格式字元", name)
		}
	}
	return nil
}

func runJobShow(ctx context.Context, jobID, hubURL, dbPath string, seen map[string]bool,
	jsonOutput bool, out io.Writer, deps machineCommandDeps,
) error {
	if seen["db"] {
		return withDirectOperatorStore(ctx, "job show", dbPath, deps, func(st *store.Store) error {
			result, err := operator.New(st).JobDetail(jobID, time.Now().UTC())
			if err != nil {
				return fmt.Errorf("讀取 job detail 失敗（direct DB operator service）：%w", err)
			}
			return writeJobDetail(out, result, jsonOutput, "direct DB operator service")
		})
	}
	client, err := jobHTTPClient(hubURL, seen["hub-url"], deps)
	if err != nil {
		return err
	}
	result, err := client.Job(ctx, jobID)
	if err != nil {
		return fmt.Errorf("讀取 job detail 失敗（HTTP operator API）：%w", err)
	}
	return writeJobDetail(out, result, jsonOutput, "HTTP operator API")
}

func runJobListDirect(ctx context.Context, request operator.JobListRequest, dbPath string,
	jsonOutput bool, out io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "job list", dbPath, deps, func(st *store.Store) error {
		if request.MachineID != "" {
			machine, err := resolveMachine(st, request.MachineID, true)
			if err != nil {
				return err
			}
			request.MachineID = machine.MachineID
		}
		result, err := operator.New(st).ListJobs(request, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("讀取 job list 失敗（direct DB operator service）：%w", err)
		}
		return writeJobList(out, result, jsonOutput, "direct DB operator service")
	})
}

func jobHTTPClient(explicitURL string, explicit bool, deps machineCommandDeps) (*operatorclient.Client, error) {
	if explicit {
		if deps.newOperatorClient == nil {
			return nil, errors.New("job read: operator HTTP client 未初始化")
		}
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, fmt.Errorf("job read: 建立 HTTP operator client 失敗：%w", err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, errors.New("job read: Hub discovery 未初始化")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("job read: 無法發現 Hub：%w", err)
	}
	if deps.newOperatorClient == nil {
		return nil, errors.New("job read: operator HTTP client 未初始化")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("job read: 建立 discovered HTTP operator client 失敗：%w", err)
	}
	return client, nil
}

func writeJobList(out io.Writer, result operator.JobListResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeJobJSON(out, result)
	}
	if _, err := fmt.Fprintf(out, "%s；Hub 評估時間 %s；符合 %d 張。\n",
		source, result.EvaluatedAt.Format(time.RFC3339Nano), result.Total); err != nil {
		return err
	}
	if len(result.Items) == 0 {
		_, err := fmt.Fprintln(out, "沒有符合 filter 的工作單。")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "JOB\tMACHINE\tRESOURCE\tREV\tSTATE\tLEASE\tCREATED\tDIGEST"); err != nil {
		return err
	}
	for _, job := range result.Items {
		resource := job.ResourceKind + "/" + job.ResourceID
		digest := string(job.ArtifactDigestStatus)
		if job.ArtifactDigest != nil {
			digest = *job.ArtifactDigest
			if len(digest) > 19 {
				digest = digest[:19] + "…"
			}
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			terminalSafe(job.JobID), terminalSafe(job.DisplayName), terminalSafe(resource), job.Revision,
			job.State, job.LeaseStatus, job.CreatedAt.Format(time.RFC3339), terminalSafe(digest)); err != nil {
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

func writeJobDetail(out io.Writer, result operator.JobDetailResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeJobJSON(out, result)
	}
	job := result.Item
	deployment := "none"
	if job.DeploymentID != nil {
		deployment = terminalSafe(*job.DeploymentID)
	}
	digest := string(job.ArtifactDigestStatus)
	if job.ArtifactDigest != nil {
		digest = terminalSafe(*job.ArtifactDigest)
	}
	_, err := fmt.Fprintf(out,
		"%s；Hub 評估時間 %s。\njob_id: %s\nmachine: %s (%s)\ndeployment: %s\nresource: %s/%s\nrevision: %d\nstate: %s\nlease: %s%s\ncreated_at: %s\nterminal_at: %s\nartifact: %s\nirreversible: %t\nevents: %d\nverifications: %d (%d passed / %d failed)\n",
		source, result.EvaluatedAt.Format(time.RFC3339Nano), terminalSafe(job.JobID),
		terminalSafe(job.DisplayName), terminalSafe(job.MachineID), deployment,
		terminalSafe(job.ResourceKind), terminalSafe(job.ResourceID), job.Revision, job.State,
		job.LeaseStatus, jobLeaseExpiry(job.LeaseExpiresAt), job.CreatedAt.Format(time.RFC3339Nano),
		jobReadTime(job.TerminalAt), digest, job.Irreversible, job.EventCount,
		job.VerificationTotal, job.VerificationPassed, job.VerificationFailed)
	return err
}

func writeJobEvidence(out io.Writer, result operator.JobEvidenceResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeJobJSON(out, result)
	}
	if _, err := fmt.Fprintf(out,
		"%s；Hub 評估時間 %s。\njob: %s；machine: %s (%s)；state: %s\nevent_provenance_recording_enabled: %t；verification_producer: %s\nindependent_verifier: %t；verification_provenance_recording_enabled: %t；verification_received_at_recording_enabled: %t\nlimit: %d；max_field_bytes: %d\n",
		terminalSafe(source), result.EvaluatedAt.Format(time.RFC3339Nano),
		terminalSafe(result.JobID), terminalSafe(result.DisplayName), terminalSafe(result.MachineID), result.State,
		result.Disclosure.EventProvenanceRecordingEnabled,
		terminalSafe(result.Disclosure.VerificationProducerKind), result.Disclosure.IndependentVerifier,
		result.Disclosure.VerificationProvenanceRecordingEnabled,
		result.Disclosure.VerificationReceivedAtRecordingEnabled, result.Disclosure.Limit,
		result.Disclosure.MaxFieldBytes); err != nil {
		return err
	}
	desired := result.Desired
	if _, err := fmt.Fprintf(out,
		"desired: %s；scope: %s/%s；resource: %s/%s；revision: %d；created_at: %s\n",
		terminalSafe(desired.DesiredID), terminalSafe(desired.ScopeType), terminalSafe(desired.ScopeID),
		terminalSafe(desired.ResourceKind), terminalSafe(desired.ResourceID), desired.Revision,
		desired.CreatedAt.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if err := writeJobEvidenceText(out, "spec", desired.Spec); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(out, "events: %d%s\n", result.Events.Total,
		jobEvidencePageSuffix(result.Events.Truncated)); err != nil {
		return err
	}
	events := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(events, "SEQ\tPHASE\tREPORTED_PHASE\tOCCURRED_AT(agent)\tRECEIVED_AT(Hub)\tPAYLOAD_BYTES"); err != nil {
		return err
	}
	for _, event := range result.Events.Items {
		if _, err := fmt.Fprintf(events, "%d\t%s\t%s%s\t%s\t%s\t%d\n", event.Seq, event.Phase,
			terminalSafe(event.ReportedPhase.Text), jobEvidenceTextSuffix(event.ReportedPhase),
			event.OccurredAt.Format(time.RFC3339Nano), event.ReceivedAt.Format(time.RFC3339Nano),
			event.PayloadBytes); err != nil {
			return err
		}
	}
	if err := events.Flush(); err != nil {
		return err
	}
	if result.Rejection == nil {
		if _, err := fmt.Fprintln(out, "rejection: none in returned window"); err != nil {
			return err
		}
	} else {
		rejection := result.Rejection
		if _, err := fmt.Fprintf(out, "rejection: seq=%d；payload_decodable=%t；code=%s%s；code_known=%t\n",
			rejection.Seq, rejection.PayloadDecodable, terminalSafe(rejection.Code.Text),
			jobEvidenceTextSuffix(rejection.Code), rejection.CodeKnown); err != nil {
			return err
		}
		if err := writeJobEvidenceText(out, "rejection detail", rejection.Detail); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "producer=%s/%s；role=%s；authority=%s；provenance_recorded=%t\n",
			terminalSafe(rejection.Producer.Kind), terminalSafe(rejection.Producer.ProducerID),
			terminalSafe(rejection.Producer.EvidenceRole), terminalSafe(rejection.Producer.Authority),
			rejection.Producer.ProvenanceRecorded); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "verifications: %d (%d passed / %d failed)%s\n",
		result.Verifications.Total, result.Verifications.Passed, result.Verifications.Failed,
		jobEvidencePageSuffix(result.Verifications.Truncated)); err != nil {
		return err
	}
	for _, verification := range result.Verifications.Items {
		exit := "—"
		if verification.ExitCode != nil {
			exit = fmt.Sprintf("%d", *verification.ExitCode)
		}
		receivedAt := "legacy-unrecorded"
		if verification.ReceivedAt != nil {
			receivedAt = verification.ReceivedAt.Format(time.RFC3339Nano)
		}
		if _, err := fmt.Fprintf(out,
			"verification %s；rule=%s%s；exit=%s；passed=%t；reported_verified_at=%s；received_at=%s\nproducer=%s/%s；role=%s；authority=%s；provenance_recorded=%t；independent_verifier=false\n",
			terminalSafe(verification.VerificationID), terminalSafe(verification.RuleID.Text),
			jobEvidenceTextSuffix(verification.RuleID), exit, verification.Passed,
			verification.ReportedVerifiedAt.Format(time.RFC3339Nano), receivedAt,
			terminalSafe(verification.Producer.Kind), terminalSafe(verification.Producer.ProducerID),
			terminalSafe(verification.Producer.EvidenceRole), terminalSafe(verification.Producer.Authority),
			verification.Producer.ProvenanceRecorded); err != nil {
			return err
		}
		for _, field := range []struct {
			name string
			text operator.JobEvidenceText
		}{
			{"command", verification.Command}, {"stdout", verification.StdoutExcerpt},
			{"stderr", verification.StderrExcerpt},
		} {
			if err := writeJobEvidenceText(out, field.name, field.text); err != nil {
				return err
			}
		}
	}
	return writeJobIndependentEvidence(out, result.Independent)
}

// writeJobIndependentEvidence renders the second producer's page. The section
// is printed for every job, including one nothing independent has written:
// silence there would leave the executor's own account looking corroborated.
func writeJobIndependentEvidence(out io.Writer, section *operator.JobIndependentEvidence) error {
	if section == nil {
		return nil
	}
	digest := section.ArtifactDigest
	if digest == "" {
		digest = "none recorded"
	}
	terminalAt := "still running"
	if section.TerminalAt != nil {
		terminalAt = section.TerminalAt.Format(time.RFC3339Nano)
	}
	if _, err := fmt.Fprintf(out,
		"independent: %d (%d live producers)%s\nindependent_verdict: %s —— %s\njob artifact_digest: %s；expected_version: %s；job terminal_at: %s\n",
		section.Total, section.LiveProducers, jobEvidencePageSuffix(section.Truncated),
		terminalSafe(section.Verdict), jobIndependentVerdictStatement(section.Verdict),
		terminalSafe(digest), terminalSafe(valueOrNone(section.ExpectedVersion)), terminalAt); err != nil {
		return err
	}
	if len(section.Assignments) == 0 {
		if _, err := fmt.Fprintf(out, "assignments: 0 —— 還沒有 verifier 被指派來看這張工作單\n"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(out, "assignments: %d\n", len(section.Assignments)); err != nil {
			return err
		}
		for _, assignment := range section.Assignments {
			reportedAt := "not reported"
			if assignment.ReportedAt != nil {
				reportedAt = assignment.ReportedAt.Format(time.RFC3339Nano)
			}
			if _, err := fmt.Fprintf(out,
				"assignment verifier=%s；display_name=%s%s；failure_domain=%s%s\nstate=%s —— %s；assigned_at=%s；reported_at=%s\n",
				terminalSafe(assignment.VerifierID),
				terminalSafe(assignment.DisplayName.Text), jobEvidenceTextSuffix(assignment.DisplayName),
				terminalSafe(assignment.FailureDomain.Text), jobEvidenceTextSuffix(assignment.FailureDomain),
				terminalSafe(assignment.State), jobAssignmentStateStatement(assignment.State),
				assignment.AssignedAt.Format(time.RFC3339Nano), reportedAt); err != nil {
				return err
			}
		}
	}
	for _, row := range section.Items {
		exit := "—"
		if row.ExitCode != nil {
			exit = fmt.Sprintf("%d", *row.ExitCode)
		}
		observed := row.ObservedDigest
		if !row.DigestReported {
			observed = "not reported"
		}
		observedVersion := row.ObservedVersion
		if !row.VersionReported {
			observedVersion = "not reported"
		}
		if _, err := fmt.Fprintf(out,
			"independent verification %s；rule=%s%s；exit=%s；passed=%t\nproducer=%s/%s；display_name=%s%s；failure_domain=%s%s；state=%s；role=%s；authority=%s\nVERIFIED_AT(verifier)=%s；RECEIVED_AT(Hub)=%s\nobserved_digest=%s；digest_matches_job=%t；observed_version=%s；version_matches_job=%t\n",
			terminalSafe(row.VerificationID), terminalSafe(row.RuleID.Text),
			jobEvidenceTextSuffix(row.RuleID), exit, row.Passed,
			terminalSafe(row.Producer.Kind), terminalSafe(row.Producer.VerifierID),
			terminalSafe(row.Producer.DisplayName.Text), jobEvidenceTextSuffix(row.Producer.DisplayName),
			terminalSafe(row.Producer.FailureDomain.Text), jobEvidenceTextSuffix(row.Producer.FailureDomain),
			terminalSafe(row.Producer.State), terminalSafe(row.Producer.EvidenceRole),
			terminalSafe(row.Producer.Authority),
			row.ReportedVerifiedAt.Format(time.RFC3339Nano), row.ReceivedAt.Format(time.RFC3339Nano),
			terminalSafe(observed), row.DigestMatchesJob,
			terminalSafe(observedVersion), row.VersionMatchesJob); err != nil {
			return err
		}
		for _, field := range []struct {
			name string
			text operator.JobEvidenceText
		}{
			{"command", row.Command}, {"stdout", row.StdoutExcerpt}, {"stderr", row.StderrExcerpt},
		} {
			if err := writeJobEvidenceText(out, field.name, field.text); err != nil {
				return err
			}
		}
	}
	return nil
}

// jobIndependentVerdictStatement states which of the eight the verdict is. Six
// of them are neither a pass nor a rule failure, and each says what the second
// producer's rows actually show.
func jobIndependentVerdictStatement(verdict string) string {
	switch store.IndependentVerdict(verdict) {
	case store.IndependentAbsent:
		return "沒有第二個 producer 為這張單寫過證據"
	case store.IndependentProducerRevoked:
		return "寫過這些證據的 producer 都已撤銷"
	case store.IndependentDigestMismatch:
		return "第二個 producer 看到的 artifact digest 與這張單的不同"
	case store.IndependentReleaseMismatch:
		return "第二個 producer 看到的 OpenClaw 版本與這張單的不同"
	case store.IndependentReleaseUnreported:
		return "第二個 producer 沒有回報可與這張單比較的結構化 OpenClaw 版本"
	case store.IndependentStale:
		return "第二個 producer 的證據都在這張單結束前送達"
	case store.IndependentFailed:
		return "第二個 producer 回報至少一條規則失敗"
	case store.IndependentPassed:
		// ⚠ 見 internal/web 的同一句：沒有回報 digest 的列也會落到 passed。
		return "第二個 producer 回報的規則全部通過，沒有一列的 digest 與這張單相衝突"
	default:
		return "這個 verdict 不在已知的八種之內"
	}
}

// jobAssignmentStateStatement states which of the four assignment states this
// is: read evidence, replace a revoked verifier, wait for the job, or chase a
// verifier that has not answered. ⚠ 見 internal/web 的同一組句子。
func jobAssignmentStateStatement(state string) string {
	switch state {
	case operator.JobAssignmentReported:
		return "已送出獨立證據"
	case operator.JobAssignmentProducerRevoked:
		return "指派的 verifier 已撤銷，不會再回報"
	case operator.JobAssignmentWaitingForJob:
		return "等這張工作單結束才會發出"
	case operator.JobAssignmentAwaitingReport:
		return "已發出，等它回報"
	default:
		return "這個 state 不在已知的四種之內"
	}
}

func valueOrNone(value string) string {
	if value == "" {
		return "none recorded"
	}
	return value
}

func writeJobEvidenceText(out io.Writer, name string, value operator.JobEvidenceText) error {
	_, err := fmt.Fprintf(out, "%s: %s%s\n", name, terminalSafe(value.Text), jobEvidenceTextSuffix(value))
	return err
}

func jobEvidenceTextSuffix(value operator.JobEvidenceText) string {
	parts := make([]string, 0, 2)
	if value.Truncated {
		parts = append(parts, fmt.Sprintf("truncated; ledger bytes=%d", value.Bytes))
	}
	if len(value.Issues) != 0 {
		safeIssues := make([]string, 0, len(value.Issues))
		for _, issue := range value.Issues {
			safeIssues = append(safeIssues, terminalSafe(issue))
		}
		parts = append(parts, "issues="+strings.Join(safeIssues, ","))
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, "; ") + "]"
}

func jobEvidencePageSuffix(truncated bool) string {
	if truncated {
		return " [returned window truncated]"
	}
	return ""
}

func writeJobJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func jobLeaseExpiry(value *time.Time) string {
	if value == nil {
		return ""
	}
	return " (expires " + value.UTC().Format(time.RFC3339Nano) + ")"
}

func jobReadTime(value *time.Time) string {
	if value == nil {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339Nano)
}
