package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type pruneCommandDeps struct {
	machineCommandDeps
	now func() time.Time
}

func productionPruneCommandDeps() pruneCommandDeps {
	return pruneCommandDeps{machineCommandDeps: productionMachineCommandDeps(), now: time.Now}
}

type pruneCommandBackend interface {
	Status(context.Context, time.Time, store.RetentionPolicy) (operator.RetentionStatus, error)
	Preview(context.Context, time.Time, store.OperatorRetentionPolicy) (operator.RetentionPrunePreview, error)
	Apply(context.Context, string, operatorclient.RetentionPruneApplyRequest) (store.OperatorPruneResult, error)
}

type pruneHTTPBackend struct{ client *operatorclient.Client }

func (b pruneHTTPBackend) Status(ctx context.Context, _ time.Time, _ store.RetentionPolicy) (operator.RetentionStatus, error) {
	return b.client.RetentionStatus(ctx)
}

func (b pruneHTTPBackend) Preview(ctx context.Context, at time.Time, policy store.OperatorRetentionPolicy) (operator.RetentionPrunePreview, error) {
	return b.client.PreviewRetentionPrune(ctx, operatorclient.RetentionPrunePreviewRequest{EvaluatedAt: &at, Policy: &policy})
}

func (b pruneHTTPBackend) Apply(ctx context.Context, key string, req operatorclient.RetentionPruneApplyRequest) (store.OperatorPruneResult, error) {
	return b.client.ApplyRetentionPrune(ctx, key, req)
}

type pruneDirectBackend struct{ service *operator.Service }

func (b pruneDirectBackend) Status(_ context.Context, at time.Time, policy store.RetentionPolicy) (operator.RetentionStatus, error) {
	return b.service.RetentionStatus(policy, at)
}

func (b pruneDirectBackend) Preview(_ context.Context, at time.Time, policy store.OperatorRetentionPolicy) (operator.RetentionPrunePreview, error) {
	retention, err := policy.RetentionPolicy()
	if err != nil {
		return operator.RetentionPrunePreview{}, err
	}
	return b.service.PreviewRetentionPrune(operator.RetentionPrunePreviewRequest{EvaluatedAt: at, Policy: retention})
}

func (b pruneDirectBackend) Apply(_ context.Context, key string, req operatorclient.RetentionPruneApplyRequest) (store.OperatorPruneResult, error) {
	return b.service.ApplyRetentionPrune(operator.RetentionPruneApplyRequest{
		EvaluatedAt: req.EvaluatedAt, Policy: *req.Policy, ExpectedRevision: req.ExpectedRevision,
		Confirm: req.Confirm, PreviewDigest: req.PreviewDigest, Reason: req.Reason, IdempotencyKey: key,
		Actor: operator.Actor{SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
			UserAgent: "clawctl-hub prune", SourceKind: operator.SourceKindDirectDBCLI},
	})
}

type pruneCommandOptions struct {
	HubURL, DBPath          string
	ExplicitHub, ExplicitDB bool
	Apply, JSON, HasRetry   bool
	ExplicitEvaluation      bool
	ExplicitPolicy          bool
	EvaluatedAt             time.Time
	Policy                  store.OperatorRetentionPolicy
	Reason, Confirm         string
	IdempotencyKey          string
	ExpectedRevision        int64
	PreviewDigest           string
}

func runPruneCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runPruneCommandWithDeps(ctx, argv, out, errOut, productionPruneCommandDeps())
}

func runPruneCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer, deps pruneCommandDeps) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub prune [--json] [--as-of TIME] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "      clawctl-hub prune --apply --reason REASON --confirm 'DELETE N ROWS' [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  預設只預覽並使用 discovered HTTP operator API；--db 是 stopped-service direct DB break-glass。")
		fmt.Fprintln(errOut, "  ambiguous response retry 必須重用原 request key、評估時間、revision、preview digest 與三個 --keep-* policy 值。")
		fs.PrintDefaults()
	}
	def := store.DefaultRetention()
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	apply := fs.Bool("apply", false, "依已確認的預覽永久刪除資料")
	jsonOutput := fs.Bool("json", false, "輸出 stable operator JSON DTO")
	keepObs := fs.Duration("keep-observations", def.Observations, "觀測保留多久")
	keepCheckins := fs.Duration("keep-checkins", def.Checkins, "心跳保留多久")
	keepOccupancy := fs.Duration("keep-occupancy", def.Occupancy, "票的占用帳保留多久")
	asOf := fs.String("as-of", "", "預覽使用的 YYYY-MM-DD 或 RFC3339 評估時間；不可與 --apply 併用")
	reason := fs.String("reason", "", "永久清理的稽核理由")
	confirm := fs.String("confirm", "", "逐字輸入預覽顯示的 DELETE N ROWS")
	key := fs.String("idempotency-key", "", "ambiguous response retry 使用原 request key")
	evaluatedAtRaw := fs.String("evaluated-at", "", "ambiguous response retry 使用原 RFC3339 評估時間")
	revision := fs.Int64("expected-revision", 0, "ambiguous response retry 使用原 retention revision")
	digest := fs.String("preview-digest", "", "ambiguous response retry 使用原 preview digest")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("prune: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	seen := visitedFlags(fs)
	if seen["hub-url"] && seen["db"] {
		return errors.New("prune: --hub-url 與 --db 不可同時明示")
	}
	if (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") || (seen["db"] && strings.TrimSpace(*dbPath) == "") {
		return errors.New("prune: 明示的 --hub-url / --db 不可為空")
	}
	if *apply && *asOf != "" {
		return errors.New("prune: --as-of 與 --apply 不可同時使用")
	}
	if !*apply && (seen["reason"] || seen["confirm"] || seen["idempotency-key"] || seen["evaluated-at"] ||
		seen["expected-revision"] || seen["preview-digest"]) {
		return errors.New("prune: preview 不接受 apply confirmation 或 retry coordinates")
	}
	canonicalReason, canonicalConfirm := strings.TrimSpace(*reason), strings.TrimSpace(*confirm)
	if *apply && (canonicalReason == "" || canonicalReason != *reason || len(canonicalReason) > 500 || canonicalConfirm == "") {
		return errors.New("prune: --apply 需要 canonical --reason（最多 500 字）與逐字 --confirm")
	}
	policy, err := store.NewOperatorRetentionPolicy(store.RetentionPolicy{
		Observations: *keepObs, Checkins: *keepCheckins, Occupancy: *keepOccupancy,
	})
	if err != nil {
		return fmt.Errorf("prune: retention policy 不合法：%w", err)
	}
	retryCount := 0
	for _, name := range []string{"idempotency-key", "evaluated-at", "expected-revision", "preview-digest"} {
		if seen[name] {
			retryCount++
		}
	}
	if retryCount != 0 && retryCount != 4 {
		return errors.New("prune: retry 必須同時提供原 --idempotency-key、--evaluated-at、--expected-revision 與 --preview-digest")
	}
	policyFlags := 0
	for _, name := range []string{"keep-observations", "keep-checkins", "keep-occupancy"} {
		if seen[name] {
			policyFlags++
		}
	}
	if retryCount == 4 && policyFlags != 3 {
		return errors.New("prune: retry 必須重用原本三個 --keep-* retention policy 值")
	}
	nowFn := deps.now
	if nowFn == nil {
		nowFn = time.Now
	}
	evaluatedAt := nowFn().UTC().Truncate(time.Second)
	if *asOf != "" {
		evaluatedAt, err = parseAsOf(*asOf)
		if err != nil {
			return fmt.Errorf("prune: --as-of 讀不懂：%w", err)
		}
		evaluatedAt = evaluatedAt.Truncate(time.Second)
	}
	if retryCount == 4 {
		evaluatedAt, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(*evaluatedAtRaw))
		if err != nil || !evaluatedAt.Equal(evaluatedAt.UTC().Truncate(time.Second)) || strings.TrimSpace(*key) == "" ||
			*revision < 0 || !validLifecycleRetryDigest(strings.TrimSpace(*digest)) {
			return errors.New("prune: retry request key、時間、revision 或 preview digest 不合法")
		}
	}
	options := pruneCommandOptions{
		HubURL: *hubURL, DBPath: *dbPath, ExplicitHub: seen["hub-url"], ExplicitDB: seen["db"],
		Apply: *apply, JSON: *jsonOutput, HasRetry: retryCount == 4, EvaluatedAt: evaluatedAt.UTC(),
		ExplicitEvaluation: *asOf != "", ExplicitPolicy: policyFlags != 0,
		Policy: policy, Reason: canonicalReason, Confirm: canonicalConfirm,
		IdempotencyKey: strings.TrimSpace(*key), ExpectedRevision: *revision, PreviewDigest: strings.TrimSpace(*digest),
	}
	return withPruneCommandBackend(ctx, options, deps, func(backend pruneCommandBackend, source string) error {
		return executePruneCommand(ctx, backend, source, options, out, errOut)
	})
}

func withPruneCommandBackend(ctx context.Context, options pruneCommandOptions, deps pruneCommandDeps,
	run func(pruneCommandBackend, string) error,
) error {
	if options.ExplicitDB {
		return withDirectOperatorStore(ctx, "prune", options.DBPath, deps.machineCommandDeps, func(st *store.Store) error {
			return run(pruneDirectBackend{service: operator.New(st)}, "direct DB operator service")
		})
	}
	client, err := machinesHTTPClient(options.HubURL, options.ExplicitHub, deps.machineCommandDeps)
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	return run(pruneHTTPBackend{client: client}, "HTTP operator API")
}

func executePruneCommand(ctx context.Context, backend pruneCommandBackend, source string,
	options pruneCommandOptions, out, errOut io.Writer,
) error {
	apply := operatorclient.RetentionPruneApplyRequest{
		EvaluatedAt: options.EvaluatedAt, Policy: &options.Policy, ExpectedRevision: options.ExpectedRevision,
		Confirm: options.Confirm, PreviewDigest: options.PreviewDigest, Reason: options.Reason,
	}
	if !options.HasRetry {
		configuredPolicy, err := options.Policy.RetentionPolicy()
		if err != nil {
			return fmt.Errorf("prune: retention policy 不合法：%w", err)
		}
		status, err := backend.Status(ctx, options.EvaluatedAt, configuredPolicy)
		if err != nil {
			return fmt.Errorf("讀取 retention policy 失敗（%s）：%w", source, err)
		}
		if !options.ExplicitEvaluation {
			options.EvaluatedAt = status.EvaluatedAt
		}
		if !options.ExplicitPolicy {
			options.Policy = status.Policy
		}
		preview, err := backend.Preview(ctx, options.EvaluatedAt, options.Policy)
		if err != nil {
			return fmt.Errorf("預覽資料清理失敗（%s）：%w", source, err)
		}
		if !options.Apply {
			if options.JSON {
				return writePruneJSON(out, preview)
			}
			return writePrunePreview(out, source, preview)
		}
		if options.Confirm != preview.Confirmation {
			return fmt.Errorf("prune: --confirm 與最新預覽不符；需要逐字輸入 %q", preview.Confirmation)
		}
		if preview.TotalDeleted == 0 {
			return errors.New("prune: 目前沒有可清理資料；未建立 apply request")
		}
		key, err := operator.NewIdempotencyKey("cli-retention-prune")
		if err != nil {
			return err
		}
		options.IdempotencyKey = key
		apply.EvaluatedAt, apply.Policy = preview.EvaluatedAt, &preview.Policy
		apply.ExpectedRevision, apply.PreviewDigest = preview.ExpectedRevision, preview.PreviewDigest
	}
	retention, err := apply.Policy.RetentionPolicy()
	if err != nil {
		return fmt.Errorf("prune: apply policy 不合法：%w", err)
	}
	fmt.Fprintf(errOut, "retention private retry coordinates: idempotency-key=%s evaluated-at=%s expected-revision=%d preview-digest=%s keep-observations=%s keep-checkins=%s keep-occupancy=%s\n",
		options.IdempotencyKey, apply.EvaluatedAt.Format(time.RFC3339), apply.ExpectedRevision, apply.PreviewDigest,
		retention.Observations, retention.Checkins, retention.Occupancy)
	result, err := backend.Apply(ctx, options.IdempotencyKey, apply)
	if err != nil {
		return fmt.Errorf("永久清理資料失敗（%s；idempotency-key=%q expected-revision=%d%s）：%w",
			source, options.IdempotencyKey, apply.ExpectedRevision, operatorRejectionReplayNote(err), err)
	}
	if options.JSON {
		return writePruneJSON(out, result)
	}
	_, err = fmt.Fprintf(out, "%s: deleted=%d kept-newest=%d revision=%d replayed=%t evaluated-at=%s\n",
		source, result.TotalDeleted, result.KeptNewest, result.Revision, result.Replayed, result.EvaluatedAt.Format(time.RFC3339))
	return err
}

func writePrunePreview(out io.Writer, source string, preview operator.RetentionPrunePreview) error {
	if _, err := fmt.Fprintf(out, "%s preview: evaluated-at=%s expected-revision=%d deleted=%d kept-newest=%d\n",
		source, preview.EvaluatedAt.Format(time.RFC3339), preview.ExpectedRevision, preview.TotalDeleted, preview.KeptNewest); err != nil {
		return err
	}
	for _, item := range preview.Counts {
		if _, err := fmt.Fprintf(out, "  %s deleted=%d kept-newest=%d older-than=%s\n",
			item.Table, item.Deleted, item.Kept, item.Older.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "next-step: review impact, then rerun with --apply --reason REASON --confirm %q\n", preview.Confirmation)
	return err
}

func writePruneJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
