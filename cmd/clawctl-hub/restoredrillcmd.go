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
		return fmt.Errorf("restore-drill: 不認得 subcommand %q", action)
	}
	leadingShowID := ""
	if action == "show" && len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		leadingShowID, argv = argv[0], argv[1:]
	}
	fs := flag.NewFlagSet("restore-drill "+action, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub restore-drill preview [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub restore-drill run --reason REASON --confirm 'VERIFY FILE' [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub restore-drill list [--limit N] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub restore-drill show OPERATION_ID [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  preview 是預設動作；HTTP operator API 是預設 transport。--db 是 stopped-service break-glass。")
		fs.PrintDefaults()
	}
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔")
	backupsDir := fs.String("backups", "", "direct DB 模式使用的 canonical absolute 備份目錄")
	stampPath := fs.String("stamp", "", "direct DB 模式成功後寫入的 canonical absolute 完成章")
	jsonOutput := fs.Bool("json", false, "輸出 stable operator JSON DTO")
	limit := fs.Int("limit", 20, "list 最多顯示幾筆 operation（1..100）")
	reason := fs.String("reason", "", "建立演練的稽核理由")
	confirm := fs.String("confirm", "", "逐字輸入 preview 顯示的 VERIFY FILE")
	key := fs.String("idempotency-key", "", "ambiguous response retry 使用原 request key")
	digest := fs.String("preview-digest", "", "ambiguous response retry 使用原 preview digest")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if seen["hub-url"] && seen["db"] {
		return errors.New("restore-drill: --hub-url 與 --db 不可同時明示")
	}
	if (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") || (seen["db"] && strings.TrimSpace(*dbPath) == "") {
		return errors.New("restore-drill: 明示的 --hub-url / --db 不可為空")
	}
	if (seen["backups"] || seen["stamp"]) && !seen["db"] {
		return errors.New("restore-drill: --backups 與 --stamp 只可搭配明示 --db")
	}
	if action == "show" {
		operationID := leadingShowID
		if operationID == "" && fs.NArg() == 1 {
			operationID = fs.Arg(0)
		} else if fs.NArg() != 0 {
			return errors.New("restore-drill show: 需要一個 canonical operation ID")
		}
		if !validRestoreDrillCLIIdentifier(operationID) {
			return errors.New("restore-drill show: 需要一個 canonical operation ID")
		}
		leadingShowID = operationID
	} else if fs.NArg() != 0 {
		return fmt.Errorf("restore-drill %s: 不接受 positional arguments", action)
	}
	if action != "run" && (seen["reason"] || seen["confirm"] || seen["idempotency-key"] || seen["preview-digest"]) {
		return fmt.Errorf("restore-drill %s: 不接受 run confirmation 或 retry coordinates", action)
	}
	if action != "list" && seen["limit"] {
		return fmt.Errorf("restore-drill %s: 不接受 --limit", action)
	}
	if *limit < 1 || *limit > store.MaxRestoreDrillReadLimit {
		return errors.New("restore-drill list: --limit 必須介於 1 與 100")
	}
	canonicalReason := strings.TrimSpace(*reason)
	if action == "run" && (canonicalReason == "" || canonicalReason != *reason || len(canonicalReason) > 500 || *confirm == "") {
		return errors.New("restore-drill run: 需要 canonical --reason（最多 500 字）與逐字 --confirm")
	}
	retryCount := 0
	for _, name := range []string{"idempotency-key", "preview-digest"} {
		if seen[name] {
			retryCount++
		}
	}
	if retryCount != 0 && retryCount != 2 {
		return errors.New("restore-drill run: retry 必須同時提供原 --idempotency-key 與 --preview-digest")
	}
	if retryCount == 2 && (strings.TrimSpace(*key) != *key || *key == "" || !validLifecycleRetryDigest(*digest)) {
		return errors.New("restore-drill run: retry coordinates 不合法")
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
			return errors.New("restore-drill: direct backup 與 stamp 路徑必須是 canonical absolute paths")
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
			return fmt.Errorf("預覽還原演練失敗（%s）：%w", source, err)
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
			return fmt.Errorf("讀取還原演練 operations 失敗（%s）：%w", source, err)
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
			return fmt.Errorf("讀取還原演練 operation 失敗（%s）：%w", source, err)
		}
		return writeRestoreDrillOperation(out, operation, options.JSON)
	case "run":
		body := operatorclient.RestoreDrillApplyRequest{PreviewDigest: options.PreviewDigest, Confirm: options.Confirm, Reason: options.Reason}
		if !options.Retry {
			preview, err := backend.Preview(ctx)
			if err != nil {
				return fmt.Errorf("預覽還原演練失敗（%s）：%w", source, err)
			}
			if options.Confirm != preview.Confirmation {
				return fmt.Errorf("restore-drill run: --confirm 與最新預覽不符；需要逐字輸入 %q", preview.Confirmation)
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
			return fmt.Errorf("建立還原演練失敗（%s；idempotency-key=%q%s）：%w", source,
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
