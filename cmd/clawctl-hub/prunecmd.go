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
		fmt.Fprintln(errOut, "Usage: clawctl-hub prune [--json] [--as-of TIME] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "       clawctl-hub prune --apply --reason REASON --confirm 'DELETE N ROWS' [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  default previews only and uses discovered HTTP operator API; --db is stopped-service direct DB break-glass")
		fmt.Fprintln(errOut, "  ambiguous response retry must reuse original request key, evaluation time, revision, preview digest, and three --keep-* policy values")
		fs.PrintDefaults()
	}
	def := store.DefaultRetention()
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL (auto-discovered if omitted)")
	dbPath := fs.String("db", "", "path to existing SQLite file for stopped-service direct DB break-glass")
	apply := fs.Bool("apply", false, "permanently delete data according to confirmed preview")
	jsonOutput := fs.Bool("json", false, "output stable operator JSON DTO")
	keepObs := fs.Duration("keep-observations", def.Observations, "retention duration for observations")
	keepCheckins := fs.Duration("keep-checkins", def.Checkins, "retention duration for check-ins")
	keepOccupancy := fs.Duration("keep-occupancy", def.Occupancy, "retention duration for ticket occupancy ledgers")
	asOf := fs.String("as-of", "", "preview evaluation time as YYYY-MM-DD or RFC3339; cannot be used with --apply")
	reason := fs.String("reason", "", "audit reason for permanent data pruning")
	confirm := fs.String("confirm", "", "exact match of DELETE N ROWS shown in preview")
	key := fs.String("idempotency-key", "", "original request key for ambiguous response retry")
	evaluatedAtRaw := fs.String("evaluated-at", "", "original RFC3339 evaluation time for ambiguous response retry")
	revision := fs.Int64("expected-revision", 0, "original retention revision for ambiguous response retry")
	digest := fs.String("preview-digest", "", "original preview digest for ambiguous response retry")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("prune: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	seen := visitedFlags(fs)
	if seen["hub-url"] && seen["db"] {
		return errors.New("prune: cannot specify both --hub-url and --db")
	}
	if (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") || (seen["db"] && strings.TrimSpace(*dbPath) == "") {
		return errors.New("prune: explicit --hub-url / --db cannot be empty")
	}
	if *apply && *asOf != "" {
		return errors.New("prune: cannot use both --as-of and --apply")
	}
	if !*apply && (seen["reason"] || seen["confirm"] || seen["idempotency-key"] || seen["evaluated-at"] ||
		seen["expected-revision"] || seen["preview-digest"]) {
		return errors.New("prune: preview does not accept apply confirmation or retry coordinates")
	}
	canonicalReason, canonicalConfirm := strings.TrimSpace(*reason), strings.TrimSpace(*confirm)
	if *apply && (canonicalReason == "" || canonicalReason != *reason || len(canonicalReason) > 500 || canonicalConfirm == "") {
		return errors.New("prune: --apply requires canonical --reason (at most 500 chars) and exact --confirm")
	}
	policy, err := store.NewOperatorRetentionPolicy(store.RetentionPolicy{
		Observations: *keepObs, Checkins: *keepCheckins, Occupancy: *keepOccupancy,
	})
	if err != nil {
		return fmt.Errorf("prune: invalid retention policy: %w", err)
	}
	retryCount := 0
	for _, name := range []string{"idempotency-key", "evaluated-at", "expected-revision", "preview-digest"} {
		if seen[name] {
			retryCount++
		}
	}
	if retryCount != 0 && retryCount != 4 {
		return errors.New("prune: retry requires original --idempotency-key, --evaluated-at, --expected-revision, and --preview-digest together")
	}
	policyFlags := 0
	for _, name := range []string{"keep-observations", "keep-checkins", "keep-occupancy"} {
		if seen[name] {
			policyFlags++
		}
	}
	if retryCount == 4 && policyFlags != 3 {
		return errors.New("prune: retry must reuse original three --keep-* retention policy values")
	}
	nowFn := deps.now
	if nowFn == nil {
		nowFn = time.Now
	}
	evaluatedAt := nowFn().UTC().Truncate(time.Second)
	if *asOf != "" {
		evaluatedAt, err = parseAsOf(*asOf)
		if err != nil {
			return fmt.Errorf("prune: cannot parse --as-of: %w", err)
		}
		evaluatedAt = evaluatedAt.Truncate(time.Second)
	}
	if retryCount == 4 {
		evaluatedAt, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(*evaluatedAtRaw))
		if err != nil || !evaluatedAt.Equal(evaluatedAt.UTC().Truncate(time.Second)) || strings.TrimSpace(*key) == "" ||
			*revision < 0 || !validLifecycleRetryDigest(strings.TrimSpace(*digest)) {
			return errors.New("prune: retry request key, time, revision, or preview digest is invalid")
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
			return fmt.Errorf("prune: invalid retention policy: %w", err)
		}
		status, err := backend.Status(ctx, options.EvaluatedAt, configuredPolicy)
		if err != nil {
			return fmt.Errorf("failed to read retention policy (%s): %w", source, err)
		}
		if !options.ExplicitEvaluation {
			options.EvaluatedAt = status.EvaluatedAt
		}
		if !options.ExplicitPolicy {
			options.Policy = status.Policy
		}
		preview, err := backend.Preview(ctx, options.EvaluatedAt, options.Policy)
		if err != nil {
			return fmt.Errorf("failed to preview data pruning (%s): %w", source, err)
		}
		if !options.Apply {
			if options.JSON {
				return writePruneJSON(out, preview)
			}
			return writePrunePreview(out, source, preview)
		}
		if options.Confirm != preview.Confirmation {
			return fmt.Errorf("prune: --confirm does not match latest preview; exact input %q required", preview.Confirmation)
		}
		if preview.TotalDeleted == 0 {
			return errors.New("prune: no prunable data at this time; apply request not created")
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
		return fmt.Errorf("prune: invalid apply policy: %w", err)
	}
	fmt.Fprintf(errOut, "retention private retry coordinates: idempotency-key=%s evaluated-at=%s expected-revision=%d preview-digest=%s keep-observations=%s keep-checkins=%s keep-occupancy=%s\n",
		options.IdempotencyKey, apply.EvaluatedAt.Format(time.RFC3339), apply.ExpectedRevision, apply.PreviewDigest,
		retention.Observations, retention.Checkins, retention.Occupancy)
	result, err := backend.Apply(ctx, options.IdempotencyKey, apply)
	if err != nil {
		return fmt.Errorf("failed to permanently prune data (%s; idempotency-key=%q expected-revision=%d%s): %w",
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
