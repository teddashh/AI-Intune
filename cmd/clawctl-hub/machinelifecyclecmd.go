package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type machineLifecycleInputs struct {
	Machine          string
	DesiredState     store.MachineLifecycleState
	ConfirmName      string
	Reason           string
	Preview          bool
	JSON             bool
	IdempotencyKey   string
	ExpectedRevision *int64
	PreviewDigest    string
}

func runMachineLifecycleSubcommand(ctx context.Context, argv []string, out, errOut io.Writer, deps machineCommandDeps) error {
	fs := flag.NewFlagSet("machine lifecycle", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub machine lifecycle --machine <name|id> [--set active|retired (--preview | --reason REASON --confirm-name NAME)] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "  省略 --set：讀取 lifecycle；--preview：顯示 impact，state 不變。")
		fmt.Fprintln(errOut, "  discovery：--hub-url、CLAWCTL_HUB_URL、operator.json。")
		fmt.Fprintln(errOut, "  transport：HTTP；--db 用於已停止的 Hub。")
		fmt.Fprintln(errOut, "  retry：重用 --idempotency-key、--expected-revision、--preview-digest。")
		fs.PrintDefaults()
	}
	dbPath := fs.String("db", "", "stopped-service direct DB break-glass 的既有 SQLite 檔位置")
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）")
	machine := fs.String("machine", "", "direct DB: display_name 或 machine_id；HTTP: machine_id")
	set := fs.String("set", "", "desired lifecycle：active 或 retired；省略即只讀")
	confirmName := fs.String("confirm-name", "", "套用時必須逐字相同的 display_name")
	reason := fs.String("reason", "", "生命週期變更理由（套用時必填）")
	preview := fs.Bool("preview", false, "只做原生 impact preview，不套用")
	jsonOutput := fs.Bool("json", false, "輸出 stable operator JSON DTO")
	idempotencyKey := fs.String("idempotency-key", "", "ambiguous response 重試用原 request key")
	expectedRevision := fs.Int64("expected-revision", 0, "明示 lifecycle revision；重試 receipt 時必填")
	previewDigest := fs.String("preview-digest", "", "ambiguous response 重試用原 preview digest")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine lifecycle: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	seen := make(map[string]bool)
	fs.Visit(func(item *flag.Flag) { seen[item.Name] = true })
	if strings.TrimSpace(*machine) == "" {
		return errors.New("machine lifecycle: --machine 必填")
	}
	if seen["hub-url"] && seen["db"] {
		return errors.New("machine lifecycle: --hub-url（HTTP mode）與 --db（direct mode）不可同時明示")
	}
	if (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") ||
		(seen["db"] && strings.TrimSpace(*dbPath) == "") {
		return errors.New("machine lifecycle: 明示的 --hub-url / --db 不可為空")
	}
	if !seen["set"] {
		for _, name := range []string{"confirm-name", "reason", "preview", "idempotency-key", "expected-revision", "preview-digest"} {
			if seen[name] {
				return fmt.Errorf("machine lifecycle: 只讀模式不接受 --%s；先明示 --set active|retired", name)
			}
		}
	} else if *set != string(store.MachineLifecycleActive) && *set != string(store.MachineLifecycleRetired) {
		return errors.New("machine lifecycle: --set 只接受 active 或 retired")
	}
	privateCount := 0
	for _, name := range []string{"idempotency-key", "preview-digest"} {
		if seen[name] {
			privateCount++
		}
	}
	if privateCount > 0 && (!seen["idempotency-key"] || !seen["preview-digest"] || !seen["expected-revision"]) {
		return errors.New("machine lifecycle: retry 必須同時提供原 --idempotency-key、--expected-revision 與 --preview-digest")
	}
	if *preview && privateCount > 0 {
		return errors.New("machine lifecycle: --preview 不接受 apply retry key/digest")
	}
	if *preview && (seen["confirm-name"] || seen["reason"]) {
		return errors.New("machine lifecycle: --preview 不接受 --confirm-name 或 --reason；它不建立 state")
	}
	if seen["expected-revision"] && *expectedRevision < 0 {
		return errors.New("machine lifecycle: --expected-revision 不可為負數")
	}
	if !*preview && seen["set"] {
		if strings.TrimSpace(*confirmName) == "" {
			return errors.New("machine lifecycle: apply 的 --confirm-name 必填，且不會由 GET 自動代填")
		}
		if strings.TrimSpace(*reason) == "" {
			return errors.New("machine lifecycle: apply 的 --reason 必填")
		}
	}
	if seen["idempotency-key"] && (strings.TrimSpace(*idempotencyKey) == "" || len(*idempotencyKey) > 200 ||
		!validLifecycleRetryDigest(strings.TrimSpace(*previewDigest))) {
		return errors.New("machine lifecycle: retry key 不可為空且最多 200 bytes；preview digest 必須是 canonical sha256")
	}
	inputs := machineLifecycleInputs{
		Machine: *machine, DesiredState: store.MachineLifecycleState(*set),
		ConfirmName: *confirmName, Reason: *reason, Preview: *preview, JSON: *jsonOutput,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey), PreviewDigest: strings.TrimSpace(*previewDigest),
	}
	if seen["expected-revision"] {
		inputs.ExpectedRevision = expectedRevision
	}
	if seen["db"] {
		return runMachineLifecycleDirect(ctx, *dbPath, inputs, out, errOut, deps)
	}
	client, err := machinesHTTPClient(*hubURL, seen["hub-url"], deps)
	if err != nil {
		return fmt.Errorf("machine lifecycle: %w", err)
	}
	return runMachineLifecycleHTTP(ctx, client, inputs, out, errOut)
}

func runMachineLifecycleHTTP(ctx context.Context, client *operatorclient.Client, inputs machineLifecycleInputs,
	out, errOut io.Writer,
) error {
	if strings.TrimSpace(inputs.Machine) == "" {
		return errors.New("machine lifecycle HTTP: --machine 必須是 machine_id")
	}
	if inputs.DesiredState == "" {
		result, err := client.MachineLifecycle(ctx, inputs.Machine)
		if err != nil {
			return fmt.Errorf("讀取 machine lifecycle 失敗（HTTP operator API）：%w", err)
		}
		if inputs.JSON {
			return writeLifecycleJSON(out, result)
		}
		return writeLifecycleRead(out, "HTTP operator API", result.MachineID, result.DisplayName,
			result.State, result.LifecycleRevision, result.InDenominator,
			result.AgentCredentialPresent, result.AgentAuthenticationAllowed,
			result.PendingEnrollmentTokenCount, result.PendingEnrollmentTokenExpiredCount,
			result.PendingEnrollmentRedemptionAllowed, result.ActiveJobCount, result.Meta.ETag)
	}
	if inputs.IdempotencyKey != "" {
		return applyMachineLifecycleHTTP(ctx, client, inputs, *inputs.ExpectedRevision,
			inputs.PreviewDigest, inputs.IdempotencyKey, out, errOut)
	}
	current, err := client.MachineLifecycle(ctx, inputs.Machine)
	if err != nil {
		return fmt.Errorf("讀取 machine lifecycle 失敗（HTTP operator API）：%w", err)
	}
	revision := current.LifecycleRevision
	if inputs.ExpectedRevision != nil {
		revision = *inputs.ExpectedRevision
	}
	preview, err := client.PreviewMachineLifecycle(ctx, inputs.Machine, operatorclient.MachineLifecyclePreviewRequest{
		DesiredState: inputs.DesiredState, ExpectedRevision: revision,
	})
	if err != nil {
		return fmt.Errorf("預覽 machine lifecycle 失敗（HTTP operator API；expected-revision=%d）：%w", revision, err)
	}
	if inputs.Preview {
		if inputs.JSON {
			return writeLifecycleJSON(out, preview)
		}
		return writeLifecyclePreview(out, "HTTP operator API", preview.MachineID, preview.DisplayName,
			preview.CurrentState, preview.DesiredState, preview.LifecycleRevision, preview.DenominatorDelta,
			preview.AgentCredentialPresent, preview.AgentAuthenticationBefore, preview.AgentAuthenticationAfter,
			preview.PendingEnrollmentTokenCount, preview.PendingEnrollmentTokenExpiredCount,
			preview.PendingEnrollmentRedemptionBefore, preview.PendingEnrollmentRedemptionAfter,
			preview.ActiveJobCount, preview.Blockers, preview.PreviewDigest)
	}
	if len(preview.Blockers) != 0 {
		return fmt.Errorf("machine lifecycle preview 有 blocker %q；未送出 apply", strings.Join(preview.Blockers, ","))
	}
	key, err := operator.NewIdempotencyKey("cli-machine-lifecycle")
	if err != nil {
		return fmt.Errorf("產生 machine lifecycle request key：%w", err)
	}
	return applyMachineLifecycleHTTP(ctx, client, inputs, revision, preview.PreviewDigest, key, out, errOut)
}

func applyMachineLifecycleHTTP(ctx context.Context, client *operatorclient.Client, inputs machineLifecycleInputs,
	revision int64, digest, key string, out, errOut io.Writer,
) error {
	fmt.Fprintf(errOut, "machine lifecycle private retry coordinates: idempotency-key=%s expected-revision=%d preview-digest=%s\n", key, revision, digest)
	result, err := client.PutMachineLifecycle(ctx, inputs.Machine, key, operatorclient.MachineLifecycleRequest{
		DesiredState: inputs.DesiredState, ExpectedRevision: revision,
		ConfirmDisplayName: inputs.ConfirmName, PreviewDigest: digest, Reason: inputs.Reason,
	})
	if err != nil {
		if lifecycleReplayedError(err) {
			if current, readErr := client.MachineLifecycle(ctx, inputs.Machine); readErr == nil {
				_ = writeLifecycleRead(errOut, "HTTP operator API authoritative current", current.MachineID,
					current.DisplayName, current.State, current.LifecycleRevision, current.InDenominator,
					current.AgentCredentialPresent, current.AgentAuthenticationAllowed,
					current.PendingEnrollmentTokenCount, current.PendingEnrollmentTokenExpiredCount,
					current.PendingEnrollmentRedemptionAllowed, current.ActiveJobCount, current.Meta.ETag)
			} else {
				return fmt.Errorf("套用 machine lifecycle 收到歷史 rejection replay，但重新讀取 authoritative current 失敗：%w（原 replay：%v）", readErr, err)
			}
		}
		return fmt.Errorf("套用 machine lifecycle 失敗（HTTP operator API；idempotency-key=%q expected-revision=%d%s）：%w",
			key, revision, operatorRejectionReplayNote(err), err)
	}
	var current *operatorclient.MachineLifecycleReadResponse
	if result.Replayed {
		read, err := client.MachineLifecycle(ctx, inputs.Machine)
		if err != nil {
			return fmt.Errorf("machine lifecycle 已回放歷史 receipt，但重新讀取 authoritative current 失敗：%w", err)
		}
		current = &read
	}
	if inputs.JSON {
		return writeLifecycleJSON(out, struct {
			operatorclient.MachineLifecycleResponse
			AuthoritativeCurrent *operatorclient.MachineLifecycleReadResponse `json:"authoritative_current,omitempty"`
		}{MachineLifecycleResponse: result, AuthoritativeCurrent: current})
	}
	source := "HTTP operator API"
	if result.Replayed {
		source = "HTTP operator API historical replay receipt"
	}
	if err := writeLifecycleApply(out, source, result.MachineID, result.DisplayName,
		result.PreviousState, result.State, result.LifecycleRevision, result.Changed, result.NoOp,
		result.AgentAuthenticationBefore, result.AgentAuthenticationAfter,
		result.PendingEnrollmentRedemptionBefore, result.PendingEnrollmentRedemptionAfter,
		key, result.Meta.ETag, result.Replayed); err != nil {
		return err
	}
	if current != nil {
		return writeLifecycleRead(out, "HTTP operator API authoritative current", current.MachineID,
			current.DisplayName, current.State, current.LifecycleRevision, current.InDenominator,
			current.AgentCredentialPresent, current.AgentAuthenticationAllowed,
			current.PendingEnrollmentTokenCount, current.PendingEnrollmentTokenExpiredCount,
			current.PendingEnrollmentRedemptionAllowed, current.ActiveJobCount, current.Meta.ETag)
	}
	return nil
}

func runMachineLifecycleDirect(ctx context.Context, dbPath string, inputs machineLifecycleInputs,
	out, errOut io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "machine lifecycle", dbPath, deps, func(st *store.Store) error {
		machine, err := resolveMachine(st, inputs.Machine, true)
		if err != nil {
			return fmt.Errorf("machine lifecycle direct target：%w", err)
		}
		service := operator.New(st)
		if inputs.DesiredState == "" {
			result, err := service.MachineLifecycle(machine.MachineID)
			if err != nil {
				return err
			}
			if inputs.JSON {
				return writeLifecycleJSON(out, result)
			}
			return writeLifecycleRead(out, "direct DB operator service", result.MachineID, result.DisplayName,
				result.State, result.LifecycleRevision, result.InDenominator,
				result.AgentCredentialPresent, result.AgentAuthenticationAllowed,
				result.PendingEnrollmentTokenCount, result.PendingEnrollmentTokenExpiredCount,
				result.PendingEnrollmentRedemptionAllowed, result.ActiveJobCount, "")
		}
		revision := machine.LifecycleRevision
		if inputs.ExpectedRevision != nil {
			revision = *inputs.ExpectedRevision
		}
		digest, key := inputs.PreviewDigest, inputs.IdempotencyKey
		if key == "" {
			preview, err := service.PreviewMachineLifecycle(operator.MachineLifecyclePreviewRequest{
				MachineID: machine.MachineID, DesiredState: inputs.DesiredState, ExpectedRevision: &revision,
			})
			if err != nil {
				return fmt.Errorf("預覽 machine lifecycle 失敗（direct DB operator service；expected-revision=%d）：%w", revision, err)
			}
			if inputs.Preview {
				if inputs.JSON {
					return writeLifecycleJSON(out, preview)
				}
				blockers := make([]string, len(preview.Blockers))
				for i := range preview.Blockers {
					blockers[i] = string(preview.Blockers[i])
				}
				return writeLifecyclePreview(out, "direct DB operator service", preview.MachineID, preview.DisplayName,
					preview.CurrentState, preview.DesiredState, preview.LifecycleRevision, preview.DenominatorDelta,
					preview.AgentCredentialPresent, preview.AgentAuthenticationBefore, preview.AgentAuthenticationAfter,
					preview.PendingEnrollmentTokenCount, preview.PendingEnrollmentTokenExpiredCount,
					preview.PendingEnrollmentRedemptionBefore, preview.PendingEnrollmentRedemptionAfter,
					preview.ActiveJobCount, blockers, preview.PreviewDigest)
			}
			if len(preview.Blockers) != 0 {
				return fmt.Errorf("machine lifecycle preview 有 blocker %q；未送出 apply", preview.Blockers)
			}
			digest = preview.PreviewDigest
			key, err = operator.NewIdempotencyKey("cli-machine-lifecycle")
			if err != nil {
				return err
			}
		}
		fmt.Fprintf(errOut, "machine lifecycle private retry coordinates: idempotency-key=%s expected-revision=%d preview-digest=%s\n", key, revision, digest)
		result, err := service.ChangeMachineLifecycle(operator.MachineLifecycleRequest{
			MachineID: machine.MachineID, DesiredState: inputs.DesiredState, ExpectedRevision: &revision,
			ConfirmDisplayName: inputs.ConfirmName, PreviewDigest: digest, Reason: inputs.Reason,
			IdempotencyKey: key, Actor: operator.Actor{
				SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
				UserAgent: "clawctl-hub machine lifecycle", SourceKind: operator.SourceKindDirectDBCLI,
			},
		})
		if err != nil {
			if lifecycleReplayedError(err) {
				if current, readErr := service.MachineLifecycle(machine.MachineID); readErr == nil {
					_ = writeLifecycleRead(errOut, "direct DB operator service authoritative current",
						current.MachineID, current.DisplayName, current.State, current.LifecycleRevision,
						current.InDenominator, current.AgentCredentialPresent,
						current.AgentAuthenticationAllowed, current.PendingEnrollmentTokenCount,
						current.PendingEnrollmentTokenExpiredCount, current.PendingEnrollmentRedemptionAllowed,
						current.ActiveJobCount, "")
				} else {
					return fmt.Errorf("machine lifecycle 收到歷史 rejection replay，但重新讀取 authoritative current 失敗：%w（原 replay：%v）", readErr, err)
				}
			}
			return fmt.Errorf("套用 machine lifecycle 失敗（direct DB operator service；idempotency-key=%q expected-revision=%d%s）：%w",
				key, revision, operatorRejectionReplayNote(err), err)
		}
		var current *operator.MachineLifecycleReadResult
		if result.Replayed {
			read, err := service.MachineLifecycle(machine.MachineID)
			if err != nil {
				return fmt.Errorf("machine lifecycle 已回放歷史 receipt，但重新讀取 authoritative current 失敗：%w", err)
			}
			current = &read
		}
		if inputs.JSON {
			return writeLifecycleJSON(out, struct {
				operator.MachineLifecycleResult
				AuthoritativeCurrent *operator.MachineLifecycleReadResult `json:"authoritative_current,omitempty"`
			}{MachineLifecycleResult: result, AuthoritativeCurrent: current})
		}
		source := "direct DB operator service"
		if result.Replayed {
			source = "direct DB operator service historical replay receipt"
		}
		if err := writeLifecycleApply(out, source, result.MachineID, result.DisplayName,
			result.PreviousState, result.State, result.LifecycleRevision, result.Changed, result.NoOp,
			result.AgentAuthenticationBefore, result.AgentAuthenticationAfter,
			result.PendingEnrollmentRedemptionBefore, result.PendingEnrollmentRedemptionAfter,
			key, "", result.Replayed); err != nil {
			return err
		}
		if current != nil {
			return writeLifecycleRead(out, "direct DB operator service authoritative current",
				current.MachineID, current.DisplayName, current.State, current.LifecycleRevision,
				current.InDenominator, current.AgentCredentialPresent,
				current.AgentAuthenticationAllowed, current.PendingEnrollmentTokenCount,
				current.PendingEnrollmentTokenExpiredCount, current.PendingEnrollmentRedemptionAllowed,
				current.ActiveJobCount, "")
		}
		return nil
	})
}

func validLifecycleRetryDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func lifecycleReplayedError(err error) bool {
	var apiErr *operatorclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Replayed
	}
	var rejection *store.OperatorRequestError
	return errors.As(err, &rejection) && rejection.Replayed
}

func writeLifecycleJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeLifecycleRead(out io.Writer, source, machineID, displayName string, state store.MachineLifecycleState,
	revision int64, denominator, credential, authentication bool,
	pending, pendingExpired int64, redemption bool, activeJobs int64, etag string,
) error {
	if etag != "" {
		etag = "；ETag=" + etag
	}
	_, err := fmt.Fprintf(out,
		"%s: %s (%s) lifecycle=%s revision=%d%s\ndenominator=%t；agent credential present=%t authentication allowed=%t；pending tickets=%d expired=%d redemption allowed=%t；nonterminal jobs=%d\n",
		source, terminalSafe(displayName), terminalSafe(machineID), state, revision, etag,
		denominator, credential, authentication, pending, pendingExpired, redemption, activeJobs)
	return err
}

func writeLifecyclePreview(out io.Writer, source, machineID, displayName string,
	before, after store.MachineLifecycleState, revision, denominatorDelta int64,
	credential, authBefore, authAfter bool, pending, pendingExpired int64,
	redemptionBefore, redemptionAfter bool, activeJobs int64, blockers []string, digest string,
) error {
	blockerText := "none"
	if len(blockers) != 0 {
		blockerText = strings.Join(blockers, ",")
	}
	_, err := fmt.Fprintf(out,
		"%s preview: %s (%s) %s → %s；lifecycle-revision=%d；denominator-delta=%d\nagent credential present=%t authentication=%t→%t；pending tickets=%d expired=%d redemption=%t→%t；nonterminal jobs=%d blockers=%s\npreview-digest=%s\n",
		source, terminalSafe(displayName), terminalSafe(machineID), before, after, revision, denominatorDelta,
		credential, authBefore, authAfter, pending, pendingExpired, redemptionBefore, redemptionAfter,
		activeJobs, blockerText, digest)
	return err
}

func writeLifecycleApply(out io.Writer, source, machineID, displayName string,
	before, after store.MachineLifecycleState, revision int64, changed, noOp,
	authBefore, authAfter, redemptionBefore, redemptionAfter bool,
	key, etag string, replayed bool,
) error {
	if etag != "" {
		etag = "；ETag=" + etag
	}
	_, err := fmt.Fprintf(out,
		"%s: %s (%s) %s → %s；revision=%d%s；changed=%t no-op=%t replayed=%t\nauthentication=%t→%t；pending redemption=%t→%t；idempotency-key=%s\n",
		source, terminalSafe(displayName), terminalSafe(machineID), before, after, revision, etag,
		changed, noOp, replayed, authBefore, authAfter, redemptionBefore, redemptionAfter, key)
	return err
}
