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
		fmt.Fprintln(errOut, "用法：clawctl-hub enrollment-limit [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "      clawctl-hub enrollment-limit --set N --reason R [--preview] [--json]")
		fmt.Fprintln(errOut, "      clawctl-hub enrollment-limit --clear --reason R [--preview] [--json]")
		fmt.Fprintln(errOut, "  上限算的是名冊上沒有退役的台數，也就是註冊報告上的分母。")
		fmt.Fprintln(errOut, "  ambiguous response retry：重用 --idempotency-key、--expected-revision、--preview-digest。")
		fs.PrintDefaults()
	}
	var hubURL auditStringFlag
	var jsonOutput auditBoolFlag
	fs.Var(&hubURL, "hub-url", "HTTP operator API base URL（省略時自動發現）")
	fs.Var(&jsonOutput, "json", "輸出 stable operator JSON DTO")
	set := fs.String("set", "", "把上限設成幾台（0 代表誰都不准再納管）")
	clear := fs.Bool("clear", false, "取消上限")
	reason := fs.String("reason", "", "為什麼改上限")
	previewOnly := fs.Bool("preview", false, "只顯示影響，不套用")
	key := fs.String("idempotency-key", "", "ambiguous response retry 使用原 request key")
	digest := fs.String("preview-digest", "", "ambiguous response retry 使用原 preview digest")
	revision := fs.Int64("expected-revision", -1, "ambiguous response retry 使用原 expected revision")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("enrollment-limit: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
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
			return errors.New("enrollment-limit: --preview 與 --reason 只能配 --set 或 --clear 使用")
		}
		return writeEnrollmentLimitRead(ctx, client, out, jsonOutput.value)
	}
	if seen["set"] && *clear {
		return errors.New("enrollment-limit: --set 與 --clear 不可同時使用")
	}
	maxMachines := 0
	if seen["set"] {
		// ⚠ 不 TrimSpace。一個會自己修掉輸入的欄位，會讓 " 4" 跟 "4" 變成兩個看起來
		// 一樣、canonical request digest 卻不同的請求。
		value, convErr := strconv.Atoi(*set)
		if convErr != nil || strconv.Itoa(value) != *set ||
			value < 0 || value > operator.MaxEnrollmentLimitMachines {
			return fmt.Errorf("enrollment-limit: --set 必須是 0 到 %d 之間的整數",
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
		return fmt.Errorf("讀取註冊上限失敗（HTTP operator API）：%w", err)
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
		if _, err := fmt.Fprintf(out, "上次是 %s 由 %s 改的：%s\n",
			page.State.UpdatedAt.Local().Format("2006-01-02 15:04"),
			page.State.UpdatedBy, page.State.Reason); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out, "上限算的是名冊上沒有退役的台數。撤票、票過期都不會讓出一個位子，退役才會。")
	return err
}

func applyEnrollmentLimitCLI(ctx context.Context, client *operatorclient.Client, out io.Writer,
	req enrollmentLimitCLIApply,
) error {
	if req.PreviewOnly && req.HasRetry {
		return errors.New("enrollment-limit: --preview 不接受 apply retry coordinates")
	}
	if req.HasRetry {
		for name, present := range req.RetryFlags {
			if !present {
				return fmt.Errorf("enrollment-limit: retry 必須同時提供原 --idempotency-key、"+
					"--expected-revision 與 --preview-digest（缺 --%s）", name)
			}
		}
	}
	preview, err := client.PreviewEnrollmentLimit(ctx, operator.EnrollmentLimitPreviewRequest{
		Set: req.Set, MaxMachines: req.MaxMachines,
	})
	if err != nil {
		return fmt.Errorf("預覽註冊上限失敗（HTTP operator API）：%w", err)
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
			return fmt.Errorf("enrollment-limit: 無法建立 request key：%w", err)
		}
		digest, revision = preview.PreviewDigest, preview.ExpectedRevision
	}
	result, err := client.SetEnrollmentLimit(ctx, key, operatorclient.EnrollmentLimitRequest{
		Set: req.Set, MaxMachines: req.MaxMachines, ExpectedRevision: revision,
		PreviewDigest: digest, Reason: req.Reason,
	})
	if err != nil {
		return fmt.Errorf("更改註冊上限失敗（HTTP operator API）：%w\n"+
			"retry 時重用：--idempotency-key %s --expected-revision %d --preview-digest %s",
			err, key, revision, digest)
	}
	if req.JSON {
		return writeOperatorJSON(out, result)
	}
	if result.Replayed {
		if _, err := fmt.Fprintln(out, "已回放原判決；上限沒有再改一次。"); err != nil {
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
	_, err := fmt.Fprintf(out, "沒有套用。要套用就拿掉 --preview。\n")
	return err
}
