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
	"path/filepath"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/restoredrill"
	"github.com/teddashh/AI-Intune/internal/store"
)

type restoreDrillCommandDeps struct {
	machineCommandDeps
	now func() time.Time
}

func productionRestoreDrillCommandDeps() restoreDrillCommandDeps {
	return restoreDrillCommandDeps{machineCommandDeps: productionMachineCommandDeps(), now: time.Now}
}

type restoreDrillCommandBackend interface {
	Preview(context.Context) (operator.RestoreDrillPreview, error)
	Create(context.Context, string, operatorclient.RestoreDrillApplyRequest) (store.OperatorRestoreDrillResult, error)
	List(context.Context, int) (store.RestoreDrillListResult, error)
	Show(context.Context, string) (store.RestoreDrillOperation, error)
}

type restoreDrillHTTPBackend struct{ client *operatorclient.Client }

func (b restoreDrillHTTPBackend) Preview(ctx context.Context) (operator.RestoreDrillPreview, error) {
	return b.client.PreviewRestoreDrill(ctx)
}

func (b restoreDrillHTTPBackend) Create(ctx context.Context, key string, req operatorclient.RestoreDrillApplyRequest) (store.OperatorRestoreDrillResult, error) {
	return b.client.CreateRestoreDrill(ctx, key, req)
}

func (b restoreDrillHTTPBackend) List(ctx context.Context, limit int) (store.RestoreDrillListResult, error) {
	return b.client.RestoreDrillOperations(ctx, limit)
}

func (b restoreDrillHTTPBackend) Show(ctx context.Context, id string) (store.RestoreDrillOperation, error) {
	return b.client.RestoreDrillOperation(ctx, id)
}

type restoreDrillDirectBackend struct{ service *operator.Service }

func (b restoreDrillDirectBackend) Preview(ctx context.Context) (operator.RestoreDrillPreview, error) {
	return b.service.PreviewRestoreDrill(ctx, time.Now().UTC())
}

func (b restoreDrillDirectBackend) Create(ctx context.Context, key string, req operatorclient.RestoreDrillApplyRequest) (store.OperatorRestoreDrillResult, error) {
	result, err := b.service.ApplyRestoreDrill(ctx, operator.RestoreDrillApplyRequest{
		PreviewDigest: req.PreviewDigest, Confirm: req.Confirm, Reason: req.Reason, IdempotencyKey: key,
		Actor: operator.Actor{SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
			UserAgent: "clawctl-hub restore-drill", SourceKind: operator.SourceKindDirectDBCLI},
	})
	if err != nil || result.Replayed {
		return result, err
	}
	if _, err := b.service.RunQueuedRestoreDrillOperations(ctx); err != nil {
		return result, err
	}
	result.Operation, err = b.service.RestoreDrillOperation(result.Operation.OperationID)
	return result, err
}

func (b restoreDrillDirectBackend) List(_ context.Context, limit int) (store.RestoreDrillListResult, error) {
	return b.service.RestoreDrillOperations(limit)
}

func (b restoreDrillDirectBackend) Show(_ context.Context, id string) (store.RestoreDrillOperation, error) {
	return b.service.RestoreDrillOperation(id)
}

type restoreDrillCommandOptions struct {
	Action                  string
	OperationID             string
	HubURL, DBPath          string
	BackupsDir, StampPath   string
	ExplicitHub, ExplicitDB bool
	ExplicitBackups         bool
	ExplicitStamp           bool
	JSON                    bool
	Limit                   int
	Reason, Confirm         string
	IdempotencyKey          string
	PreviewDigest           string
	Retry                   bool
}

func cmdRestoreDrill(argv []string) {
	if err := runRestoreDrillCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(terminalSafe(err.Error()))
	}
}

func runRestoreDrillCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runRestoreDrillCommandWithDeps(ctx, argv, out, errOut, productionRestoreDrillCommandDeps())
}

func runRestoreDrillCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer, deps restoreDrillCommandDeps) error {
	action := "preview"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		action, argv = argv[0], argv[1:]
	}
	if action != "preview" && action != "run" && action != "list" && action != "show" {
		return fmt.Errorf("restore-drill: unrecognized subcommand %q", action)
	}
	leadingShowID := ""
	if action == "show" && len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		leadingShowID, argv = argv[0], argv[1:]
	}
	fs := flag.NewFlagSet("restore-drill "+action, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub restore-drill preview [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "       clawctl-hub restore-drill run --reason REASON --confirm 'VERIFY FILE' [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "       clawctl-hub restore-drill list [--limit N] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "       clawctl-hub restore-drill show OPERATION_ID [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  preview is the default action; HTTP operator API is the default transport; --db is stopped-service break-glass")
		fs.PrintDefaults()
	}
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL (auto-discovered if omitted)")
	dbPath := fs.String("db", "", "existing SQLite file for stopped-service direct DB break-glass")
	backupsDir := fs.String("backups", "", "canonical absolute backup directory used in direct DB mode")
	stampPath := fs.String("stamp", "", "canonical absolute completion stamp written after success in direct DB mode")
	jsonOutput := fs.Bool("json", false, "output stable operator JSON DTO")
	limit := fs.Int("limit", 20, "maximum operations to display in list (1..100)")
	reason := fs.String("reason", "", "audit reason for creating restore drill")
	confirm := fs.String("confirm", "", "exact match of VERIFY FILE shown in preview")
	key := fs.String("idempotency-key", "", "original request key for ambiguous response retry")
	digest := fs.String("preview-digest", "", "original preview digest for ambiguous response retry")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if seen["hub-url"] && seen["db"] {
		return errors.New("restore-drill: cannot specify both --hub-url and --db")
	}
	if (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") || (seen["db"] && strings.TrimSpace(*dbPath) == "") {
		return errors.New("restore-drill: explicit --hub-url / --db cannot be empty")
	}
	if (seen["backups"] || seen["stamp"]) && !seen["db"] {
		return errors.New("restore-drill: --backups and --stamp can only be used with explicit --db")
	}
	if action == "show" {
		operationID := leadingShowID
		if operationID == "" && fs.NArg() == 1 {
			operationID = fs.Arg(0)
		} else if fs.NArg() != 0 {
			return errors.New("restore-drill show: canonical operation ID required")
		}
		if !validRestoreDrillCLIIdentifier(operationID) {
			return errors.New("restore-drill show: canonical operation ID required")
		}
		leadingShowID = operationID
	} else if fs.NArg() != 0 {
		return fmt.Errorf("restore-drill %s: positional arguments not accepted", action)
	}
	if action != "run" && (seen["reason"] || seen["confirm"] || seen["idempotency-key"] || seen["preview-digest"]) {
		return fmt.Errorf("restore-drill %s: does not accept run confirmation or retry coordinates", action)
	}
	if action != "list" && seen["limit"] {
		return fmt.Errorf("restore-drill %s: does not accept --limit", action)
	}
	if *limit < 1 || *limit > store.MaxRestoreDrillReadLimit {
		return errors.New("restore-drill list: --limit must be between 1 and 100")
	}
	canonicalReason := strings.TrimSpace(*reason)
	if action == "run" && (canonicalReason == "" || canonicalReason != *reason || len(canonicalReason) > 500 || *confirm == "") {
		return errors.New("restore-drill run: requires canonical --reason (at most 500 chars) and exact --confirm")
	}
	retryCount := 0
	for _, name := range []string{"idempotency-key", "preview-digest"} {
		if seen[name] {
			retryCount++
		}
	}
	if retryCount != 0 && retryCount != 2 {
		return errors.New("restore-drill run: retry requires original --idempotency-key and --preview-digest together")
	}
	if retryCount == 2 && (strings.TrimSpace(*key) != *key || *key == "" || !validLifecycleRetryDigest(*digest)) {
		return errors.New("restore-drill run: invalid retry coordinates")
	}
	options := restoreDrillCommandOptions{
		Action: action, HubURL: *hubURL, DBPath: *dbPath, BackupsDir: *backupsDir, StampPath: *stampPath,
		ExplicitHub: seen["hub-url"], ExplicitDB: seen["db"], ExplicitBackups: seen["backups"], ExplicitStamp: seen["stamp"],
		JSON: *jsonOutput, Limit: *limit, Reason: canonicalReason, Confirm: *confirm,
		IdempotencyKey: *key, PreviewDigest: *digest, Retry: retryCount == 2,
	}
	if action == "show" {
		options.OperationID = leadingShowID
	}
	return withRestoreDrillBackend(ctx, options, deps, func(backend restoreDrillCommandBackend, source string) error {
		return executeRestoreDrillCommand(ctx, backend, source, options, out, errOut)
	})
}

func withRestoreDrillBackend(ctx context.Context, options restoreDrillCommandOptions, deps restoreDrillCommandDeps,
	run func(restoreDrillCommandBackend, string) error,
) error {
	if !options.ExplicitDB {
		client, err := machinesHTTPClient(options.HubURL, options.ExplicitHub, deps.machineCommandDeps)
		if err != nil {
			return fmt.Errorf("restore-drill: %w", err)
		}
		return run(restoreDrillHTTPBackend{client: client}, "HTTP operator API")
	}
	return withDirectOperatorStore(ctx, "restore-drill", options.DBPath, deps.machineCommandDeps, func(st *store.Store) error {
		backupsDir := options.BackupsDir
		if !options.ExplicitBackups {
			backupsDir = filepath.Join(filepath.Dir(options.DBPath), "backups")
		}
		stampPath := options.StampPath
		if !options.ExplicitStamp {
			stampPath = filepath.Join(filepath.Dir(options.DBPath), "restore-drill.stamp")
		}
		if !filepath.IsAbs(backupsDir) || filepath.Clean(backupsDir) != backupsDir || !filepath.IsAbs(stampPath) || filepath.Clean(stampPath) != stampPath {
			return errors.New("restore-drill: direct backup and stamp paths must be canonical absolute paths")
		}
		service := operator.New(st)
		service.ConfigureRestoreDrill(restoredrill.Runner{BackupsDir: backupsDir, StampPath: stampPath, Live: st, Now: deps.now})
		return run(restoreDrillDirectBackend{service: service}, "direct DB operator service")
	})
}

func executeRestoreDrillCommand(ctx context.Context, backend restoreDrillCommandBackend, source string,
	options restoreDrillCommandOptions, out, errOut io.Writer,
) error {
	switch options.Action {
	case "preview":
		preview, err := backend.Preview(ctx)
		if err != nil {
			return fmt.Errorf("failed to preview restore drill (%s): %w", source, err)
		}
		if options.JSON {
			return writeRestoreDrillJSON(out, preview)
		}
		_, err = fmt.Fprintf(out, "%s preview: backup=%s size=%s modified-at=%s live-expected=%d sha256=%s\nnext-step: review impact, then rerun with run --reason REASON --confirm %q\n",
			source, preview.Backup.Name, humanBytes(preview.Backup.SizeBytes), preview.Backup.ModifiedAt.Format(time.RFC3339),
			preview.LiveExpected, preview.Backup.SHA256, preview.Confirmation)
		return err
	case "list":
		result, err := backend.List(ctx, options.Limit)
		if err != nil {
			return fmt.Errorf("failed to read restore drill operations (%s): %w", source, err)
		}
		if options.JSON {
			return writeRestoreDrillJSON(out, result)
		}
		for _, operation := range result.Items {
			if _, err := fmt.Fprintf(out, "%s %s %s %s attempt=%d updated-at=%s\n", operation.OperationID,
				operation.Backup.Name, operation.State, operation.Phase, operation.Attempt, operation.UpdatedAt.Format(time.RFC3339)); err != nil {
				return err
			}
		}
		return nil
	case "show":
		operation, err := backend.Show(ctx, options.OperationID)
		if err != nil {
			return fmt.Errorf("failed to read restore drill operation (%s): %w", source, err)
		}
		return writeRestoreDrillOperation(out, operation, options.JSON)
	case "run":
		body := operatorclient.RestoreDrillApplyRequest{PreviewDigest: options.PreviewDigest, Confirm: options.Confirm, Reason: options.Reason}
		if !options.Retry {
			preview, err := backend.Preview(ctx)
			if err != nil {
				return fmt.Errorf("failed to preview restore drill (%s): %w", source, err)
			}
			if options.Confirm != preview.Confirmation {
				return fmt.Errorf("restore-drill run: --confirm does not match latest preview; exact input %q required", preview.Confirmation)
			}
			key, err := operator.NewIdempotencyKey("cli-restore-drill")
			if err != nil {
				return err
			}
			options.IdempotencyKey, body.PreviewDigest = key, preview.PreviewDigest
		}
		fmt.Fprintf(errOut, "restore-drill private retry coordinates: idempotency-key=%s preview-digest=%s\n", options.IdempotencyKey, body.PreviewDigest)
		result, err := backend.Create(ctx, options.IdempotencyKey, body)
		if err != nil {
			return fmt.Errorf("failed to create restore drill (%s; idempotency-key=%q%s): %w", source,
				options.IdempotencyKey, operatorRejectionReplayNote(err), err)
		}
		if options.JSON {
			return writeRestoreDrillJSON(out, result)
		}
		return writeRestoreDrillOperation(out, result.Operation, false)
	default:
		return errors.New("restore-drill: invalid action")
	}
}

func writeRestoreDrillOperation(out io.Writer, operation store.RestoreDrillOperation, jsonOutput bool) error {
	if jsonOutput {
		return writeRestoreDrillJSON(out, operation)
	}
	if _, err := fmt.Fprintf(out, "operation=%s state=%s phase=%s backup=%s attempt=%d\n", operation.OperationID,
		operation.State, operation.Phase, operation.Backup.Name, operation.Attempt); err != nil {
		return err
	}
	if operation.State == store.RestoreDrillSucceeded {
		_, err := fmt.Fprintf(out, "result: machines=%d expected=%d live-expected=%d duration-ms=%d\n",
			*operation.Machines, *operation.Expected, *operation.LiveExpected, *operation.DurationMilliseconds)
		return err
	}
	if operation.State == store.RestoreDrillFailed {
		_, err := fmt.Fprintf(out, "failure: code=%s detail=%s\n", *operation.ErrorCode, *operation.ErrorDetail)
		return err
	}
	_, err := fmt.Fprintf(out, "next-step: show %s to read the completed result\n", operation.OperationID)
	return err
}

func writeRestoreDrillJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func validRestoreDrillCLIIdentifier(value string) bool {
	return value != "" && len(value) <= 128 && value == strings.TrimSpace(value) &&
		!strings.ContainsAny(value, "/\\") && value != "." && value != ".."
}
