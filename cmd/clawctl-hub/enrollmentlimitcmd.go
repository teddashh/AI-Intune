package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
)

// cmdEnrollmentLimit 回答「這個 Hub 還收不收得下一台」，並且改得動那個答案。
//
// 上限擋的是開票那一刻。這個命令把那個判決提前搬到操作者面前：現在有幾台、上限
// 幾台、還可以再納管幾台，以及要再納管的話該做什麼。
func cmdEnrollmentLimit(argv []string) {
	if err := runEnrollmentLimitCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(err)
	}
}

func runEnrollmentLimitCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runEnrollmentLimitCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runEnrollmentLimitCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("enrollment-limit", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub enrollment-limit [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "      clawctl-hub enrollment-limit --set N --reason REASON [--preview] [--json]")
		fmt.Fprintln(errOut, "      clawctl-hub enrollment-limit --clear --reason REASON [--preview] [--json]")
		fmt.Fprintln(errOut, "  The limit counts non-retired machines on the roster, which is the denominator in enrollment report.")
		fmt.Fprintln(errOut, "  ambiguous response retry: reuse --idempotency-key, --expected-revision, and --preview-digest.")
		fs.PrintDefaults()
	}
	var hubURL auditStringFlag
	var jsonOutput auditBoolFlag
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL (auto-discovered if omitted)")
	fs.Var(&jsonOutput, "json", "output stable operator JSON DTO")
	set := fs.String("set", "", "set limit to N machines (0 prohibits further enrollments)")
	clear := fs.Bool("clear", false, "clear the limit")
	reason := fs.String("reason", "", "reason for changing the limit")
	previewOnly := fs.Bool("preview", false, "display impact only without applying")
	key := fs.String("idempotency-key", "", "ambiguous response retry: reuse original request key")
	digest := fs.String("preview-digest", "", "ambiguous response retry: reuse original preview digest")
	revision := fs.Int64("expected-revision", -1, "ambiguous response retry: reuse original expected revision")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("enrollment-limit: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if hubURL.set {
		if err := validateReportChangeCLIText("hub-url", hubURL.value, 2048); err != nil {
			return errors.New(strings.NewReplacer("report changes:", "enrollment-limit:").Replace(err.Error()))
		}
	}
	client, err := reportHTTPClient("enrollment-limit", hubURL.value, hubURL.set, deps)
	if err != nil {
		return err
	}
	if !seen["set"] && !*clear {
		if *previewOnly || seen["reason"] {
			return errors.New("enrollment-limit: --preview and --reason can only be used with --set or --clear")
		}
		return writeEnrollmentLimitRead(ctx, client, out, jsonOutput.value)
	}
	if seen["set"] && *clear {
		return errors.New("enrollment-limit: --set and --clear cannot be used together")
	}
	maxMachines := 0
	if seen["set"] {
		// ⚠ 不 TrimSpace。一個會自己修掉輸入的欄位，會讓 " 4" 跟 "4" 變成兩個看起來
		// 一樣、canonical request digest 卻不同的請求。
		value, convErr := strconv.Atoi(*set)
		if convErr != nil || strconv.Itoa(value) != *set ||
			value < 0 || value > operator.MaxEnrollmentLimitMachines {
			return fmt.Errorf("enrollment-limit: --set must be an integer between 0 and %d",
				operator.MaxEnrollmentLimitMachines)
		}
		maxMachines = value
	}
	if err := validateReportChangeCLIText("reason", *reason, 500); err != nil {
		return errors.New(strings.NewReplacer("report changes:", "enrollment-limit:").Replace(err.Error()))
	}
	return applyEnrollmentLimitCLI(ctx, client, out, enrollmentLimitCLIApply{
		Set: seen["set"], MaxMachines: maxMachines, Reason: *reason,
		PreviewOnly: *previewOnly, JSON: jsonOutput.value,
		IdempotencyKey: *key, PreviewDigest: *digest, ExpectedRevision: *revision,
		HasRetry: seen["idempotency-key"] || seen["preview-digest"] || seen["expected-revision"],
		RetryFlags: map[string]bool{"idempotency-key": seen["idempotency-key"],
			"preview-digest": seen["preview-digest"], "expected-revision": seen["expected-revision"]},
	})
}

type enrollmentLimitCLIApply struct {
	Set              bool
	MaxMachines      int
	Reason           string
	PreviewOnly      bool
	JSON             bool
	IdempotencyKey   string
	PreviewDigest    string
	ExpectedRevision int64
	HasRetry         bool
	RetryFlags       map[string]bool
}

func writeEnrollmentLimitRead(ctx context.Context, client *operatorclient.Client,
	out io.Writer, asJSON bool,
) error {
	page, err := client.EnrollmentLimit(ctx)
	if err != nil {
		return fmt.Errorf("failed to read enrollment limit (HTTP operator API): %w", err)
	}
	if asJSON {
		return writeOperatorJSON(out, page)
	}
	return writeEnrollmentLimitPage(out, page)
}

func writeEnrollmentLimitPage(out io.Writer, page operator.EnrollmentLimitResult) error {
	if _, err := fmt.Fprintln(out, page.Headline); err != nil {
		return err
	}
	if page.NextStep != "" {
		if _, err := fmt.Fprintln(out, page.NextStep); err != nil {
			return err
		}
	}
	if page.State.Revision > 0 {
		if _, err := fmt.Fprintf(out, "Last updated %s by %s: %s\n",
			page.State.UpdatedAt.Local().Format("2006-01-02 15:04"),
			page.State.UpdatedBy, page.State.Reason); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out, "The limit counts the number of non-retired machines on the roster. Revoking a token or token expiration will not free up a slot; only retirement does.")
	return err
}

func applyEnrollmentLimitCLI(ctx context.Context, client *operatorclient.Client, out io.Writer,
	req enrollmentLimitCLIApply,
) error {
	if req.PreviewOnly && req.HasRetry {
		return errors.New("enrollment-limit: --preview does not accept apply retry coordinates")
	}
	if req.HasRetry {
		for name, present := range req.RetryFlags {
			if !present {
				return fmt.Errorf("enrollment-limit: retry requires original --idempotency-key, "+
					"--expected-revision, and --preview-digest together (missing --%s)", name)
			}
		}
	}
	preview, err := client.PreviewEnrollmentLimit(ctx, operator.EnrollmentLimitPreviewRequest{
		Set: req.Set, MaxMachines: req.MaxMachines,
	})
	if err != nil {
		return fmt.Errorf("failed to preview enrollment limit (HTTP operator API): %w", err)
	}
	if req.PreviewOnly {
		if req.JSON {
			return writeOperatorJSON(out, preview)
		}
		return writeEnrollmentLimitPreview(out, preview)
	}

	key, digest, revision := req.IdempotencyKey, req.PreviewDigest, req.ExpectedRevision
	if !req.HasRetry {
		key, err = operator.NewIdempotencyKey("cli-enrollment-limit")
		if err != nil {
			return fmt.Errorf("enrollment-limit: failed to create request key: %w", err)
		}
		digest, revision = preview.PreviewDigest, preview.ExpectedRevision
	}
	result, err := client.SetEnrollmentLimit(ctx, key, operatorclient.EnrollmentLimitRequest{
		Set: req.Set, MaxMachines: req.MaxMachines, ExpectedRevision: revision,
		PreviewDigest: digest, Reason: req.Reason,
	})
	if err != nil {
		return fmt.Errorf("failed to change enrollment limit (HTTP operator API): %w\n"+
			"reuse on retry: --idempotency-key %s --expected-revision %d --preview-digest %s",
			err, key, revision, digest)
	}
	if req.JSON {
		return writeOperatorJSON(out, result)
	}
	if result.Replayed {
		if _, err := fmt.Fprintln(out, "Original verdict replayed; limit was not changed again."); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(out, operator.EnrollmentLimitHeadline(result.State))
	return err
}

func writeEnrollmentLimitPreview(out io.Writer, preview operator.EnrollmentLimitPreview) error {
	if _, err := fmt.Fprintln(out, preview.Headline); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "Not applied. To apply, remove --preview.\n")
	return err
}
