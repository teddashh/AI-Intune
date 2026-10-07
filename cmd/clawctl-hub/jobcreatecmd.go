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

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type jobCreateInputs struct {
	Machine        string
	Timeout        int
	ConfirmName    string
	Reason         string
	Preview        bool
	JSON           bool
	IdempotencyKey string
	PreviewDigest  string
}

func runJobCreateCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runJobCreateCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runJobCreateCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer, deps machineCommandDeps) error {
	fs := flag.NewFlagSet("job create", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub job create --kind noop --machine <name|id> [--timeout 600] (--preview | --reason REASON --confirm-name NAME) [--json] [--hub-url URL | --db PATH]")
		fs.PrintDefaults()
	}
	hubURL := fs.String("hub-url", "", "Hub URL")
	dbPath := fs.String("db", "", "path to SQLite ledger for stopped Hub")
	machine := fs.String("machine", "", "machine ID; --db accepts display name")
	kind := fs.String("kind", "", "job type: noop")
	timeout := fs.Int("timeout", store.OperatorDiagnosticNoopDefaultTimeout, "execution timeout in seconds")
	confirmName := fs.String("confirm-name", "", "confirm machine display name")
	reason := fs.String("reason", "", "operation reason")
	previewOnly := fs.Bool("preview", false, "preview only")
	jsonOutput := fs.Bool("json", false, "output JSON")
	idempotencyKey := fs.String("idempotency-key", "", "request key for retry")
	previewDigest := fs.String("preview-digest", "", "preview digest for retry")
	if name := removedJobCreateFlag(argv); name != "" {
		if name == "version" || name == "artifact" {
			return fmt.Errorf("job create: --%s is not supported; use deployment create", name)
		}
		return fmt.Errorf("job create: --%s is not supported", name)
	}
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("job create: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	seen := map[string]bool{}
	fs.Visit(func(item *flag.Flag) { seen[item.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("job create: --hub-url (HTTP mode) and --db (direct mode) cannot both be specified")
	}
	if (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") || (seen["db"] && strings.TrimSpace(*dbPath) == "") {
		return errors.New("job create: specified --hub-url / --db cannot be empty")
	}
	if strings.TrimSpace(*machine) == "" {
		return errors.New("job create: --machine is required")
	}
	if *kind == "openclaw" {
		return errors.New("job create: use deployment create for OpenClaw")
	}
	if *kind != store.OperatorDiagnosticNoopKind {
		return errors.New("job create: --kind only accepts noop")
	}
	privateCount := 0
	for _, name := range []string{"idempotency-key", "preview-digest"} {
		if seen[name] {
			privateCount++
		}
	}
	if privateCount == 1 {
		return errors.New("job create: retry requires both original --idempotency-key and --preview-digest")
	}
	if *previewOnly && privateCount != 0 {
		return errors.New("job create: --preview does not accept apply retry key/digest")
	}
	if *previewOnly && (seen["confirm-name"] || seen["reason"]) {
		return errors.New("job create: --preview does not accept --confirm-name or --reason")
	}
	if !*previewOnly {
		if strings.TrimSpace(*confirmName) == "" {
			return errors.New("job create: --confirm-name is required")
		}
		if strings.TrimSpace(*reason) == "" {
			return errors.New("job create: --reason is required for apply")
		}
	}
	if privateCount == 2 && (strings.TrimSpace(*idempotencyKey) == "" || len(*idempotencyKey) > 200 ||
		!validLifecycleRetryDigest(strings.TrimSpace(*previewDigest))) {
		return errors.New("job create: retry key cannot be empty and must be at most 200 bytes; preview digest must be canonical sha256")
	}
	inputs := jobCreateInputs{
		Machine: *machine, Timeout: *timeout, ConfirmName: *confirmName, Reason: *reason,
		Preview: *previewOnly, JSON: *jsonOutput, IdempotencyKey: strings.TrimSpace(*idempotencyKey),
		PreviewDigest: strings.TrimSpace(*previewDigest),
	}
	if seen["db"] {
		return withDirectOperatorStore(ctx, "job create", *dbPath, deps, func(st *store.Store) error {
			return runJobCreateDirect(st, inputs, out, errOut)
		})
	}
	client, err := machinesHTTPClient(*hubURL, seen["hub-url"], deps)
	if err != nil {
		return fmt.Errorf("job create: %w", err)
	}
	return runJobCreateHTTP(ctx, client, inputs, out, errOut)
}

func removedJobCreateFlag(argv []string) string {
	for _, arg := range argv {
		name := strings.TrimPrefix(arg, "--")
		if name == arg {
			continue
		}
		if before, _, ok := strings.Cut(name, "="); ok {
			name = before
		}
		switch name {
		case "spec", "version", "artifact", "irreversible", "by":
			return name
		}
	}
	return ""
}

func runJobCreateHTTP(ctx context.Context, client *operatorclient.Client, inputs jobCreateInputs, out, errOut io.Writer) error {
	if strings.TrimSpace(inputs.Machine) == "" {
		return errors.New("job create (HTTP operator API): --machine must be machine_id")
	}
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := client.PreviewDiagnosticNoop(ctx, inputs.Machine, operatorclient.DiagnosticNoopPreviewRequest{
			ExecutionTimeoutSeconds: inputs.Timeout,
		})
		if err != nil {
			return fmt.Errorf("job create preview (HTTP operator API) failed: %w", err)
		}
		if inputs.Preview {
			return writeJobCreateJSONOrPreview(out, inputs.JSON, preview)
		}
		if err := writeJobCreatePreview(errOut, preview.MachineID, preview.DisplayName,
			preview.ExecutionTimeoutSeconds, int64(preview.CurrentResourceRevision), int64(preview.PlannedRevision),
			preview.ActiveJobCount, preview.JobsEnabled, preview.Blockers, preview.PreviewDigest); err != nil {
			return err
		}
		if len(preview.Blockers) != 0 {
			return fmt.Errorf("job create blocked: %v", preview.Blockers)
		}
		digest = preview.PreviewDigest
	}
	key, err := diagnosticNoopRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return err
	}
	result, err := client.CreateDiagnosticNoop(ctx, inputs.Machine, key, operatorclient.DiagnosticNoopRequest{
		ExecutionTimeoutSeconds: inputs.Timeout, ConfirmDisplayName: inputs.ConfirmName,
		PreviewDigest: digest, Reason: inputs.Reason,
	})
	if err != nil {
		return fmt.Errorf("job create (HTTP operator API; idempotency-key=%q preview-digest=%q%s) failed: %w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	if inputs.JSON {
		return json.NewEncoder(out).Encode(result)
	}
	return writeJobCreateReceipt(out, result.MachineID, result.DisplayName,
		result.DesiredID, result.JobID, int64(result.Revision), result.ExecutionTimeoutSeconds,
		result.Replayed, key, digest, result.Meta.ETag)
}

func runJobCreateDirect(st *store.Store, inputs jobCreateInputs, out, errOut io.Writer) error {
	machine, err := resolveMachine(st, inputs.Machine, true)
	if err != nil {
		return fmt.Errorf("job create (direct DB operator service) failed to resolve machine: %w", err)
	}
	service := operator.New(st)
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := service.PreviewDiagnosticNoop(operator.DiagnosticNoopPreviewRequest{
			MachineID: machine.MachineID, ExecutionTimeoutSeconds: inputs.Timeout,
		})
		if err != nil {
			return fmt.Errorf("job create preview (direct DB operator service) failed: %w", err)
		}
		if inputs.Preview {
			return writeJobCreateJSONOrPreview(out, inputs.JSON, preview)
		}
		if err := writeJobCreatePreview(errOut, preview.MachineID, preview.DisplayName,
			preview.ExecutionTimeoutSeconds, int64(preview.CurrentResourceRevision), int64(preview.PlannedRevision),
			preview.ActiveJobCount, preview.JobsEnabled, preview.Blockers, preview.PreviewDigest); err != nil {
			return err
		}
		if len(preview.Blockers) != 0 {
			return fmt.Errorf("job create blocked: %v", preview.Blockers)
		}
		digest = preview.PreviewDigest
	}
	key, err := diagnosticNoopRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return err
	}
	result, err := service.CreateDiagnosticNoop(operator.DiagnosticNoopRequest{
		MachineID: machine.MachineID, ExecutionTimeoutSeconds: inputs.Timeout,
		ConfirmDisplayName: inputs.ConfirmName, PreviewDigest: digest, Reason: inputs.Reason,
		IdempotencyKey: key, Actor: operator.Actor{
			SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
			UserAgent: "clawctl-hub job create", SourceKind: operator.SourceKindDirectDBCLI,
		},
	})
	if err != nil {
		return fmt.Errorf("job create (direct DB operator service; idempotency-key=%q preview-digest=%q%s) failed: %w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	if inputs.JSON {
		return json.NewEncoder(out).Encode(result)
	}
	return writeJobCreateReceipt(out, result.MachineID, result.DisplayName,
		result.DesiredID, result.JobID, int64(result.Revision), result.ExecutionTimeoutSeconds,
		result.Replayed, key, digest, "")
}

func diagnosticNoopRequestKey(supplied string) (string, error) {
	if supplied != "" {
		return supplied, nil
	}
	return operator.NewIdempotencyKey("cli-diagnostic-noop")
}

func writeJobCreateJSONOrPreview(w io.Writer, asJSON bool, value any) error {
	if asJSON {
		return json.NewEncoder(w).Encode(value)
	}
	switch preview := value.(type) {
	case operatorclient.DiagnosticNoopPreviewResponse:
		return writeJobCreatePreview(w, preview.MachineID, preview.DisplayName, preview.ExecutionTimeoutSeconds,
			int64(preview.CurrentResourceRevision), int64(preview.PlannedRevision),
			preview.ActiveJobCount, preview.JobsEnabled, preview.Blockers, preview.PreviewDigest)
	case operator.DiagnosticNoopPreviewResult:
		return writeJobCreatePreview(w, preview.MachineID, preview.DisplayName, preview.ExecutionTimeoutSeconds,
			int64(preview.CurrentResourceRevision), int64(preview.PlannedRevision),
			preview.ActiveJobCount, preview.JobsEnabled, preview.Blockers, preview.PreviewDigest)
	default:
		return errors.New("job create: unknown preview representation")
	}
}

func writeJobCreatePreview(w io.Writer, machineID, displayName string, timeout int,
	currentRevision, plannedRevision int64, activeJobs int64,
	jobsEnabled *bool,
	blockers []store.OperatorDiagnosticNoopBlocker, digest string,
) error {
	_, err := fmt.Fprintf(w,
		"preview machine=%s machine_id=%s timeout=%ds revision=%d→%d config_changed=false jobs_enabled=%s active_jobs=%d blockers=%v preview_digest=%s\n",
		displayName, machineID, timeout, currentRevision, plannedRevision, diagnosticJobsEnabled(jobsEnabled), activeJobs, blockers, digest)
	return err
}

func diagnosticJobsEnabled(enabled *bool) string {
	if enabled == nil {
		return "unknown"
	}
	return strconv.FormatBool(*enabled)
}

func writeJobCreateReceipt(w io.Writer, machineID, displayName, desiredID, jobID string,
	revision int64, timeout int, replayed bool, key, digest, etag string,
) error {
	state := "created"
	if replayed {
		state = "replayed"
	}
	_, err := fmt.Fprintf(w,
		"%s job=%s desired=%s machine=%s machine_id=%s revision=%d timeout=%ds config_changed=false request_key=%s preview_digest=%s",
		state, jobID, desiredID, displayName, machineID, revision, timeout, key, digest)
	if err != nil {
		return err
	}
	if etag != "" {
		_, err = fmt.Fprintf(w, " etag=%s", etag)
	}
	if err == nil {
		_, err = fmt.Fprintln(w)
	}
	return err
}
