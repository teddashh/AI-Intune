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

	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
)

func cmdSettings(argv []string) {
	if err := runSettingsCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(terminalSafe(err.Error()))
	}
}

func runSettingsCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runSettingsCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runSettingsCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	usage := func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub settings list")
		fmt.Fprintln(errOut, "      clawctl-hub settings publish --policy ID --checkin DUR --observation DUR [--preview]")
		fmt.Fprintln(errOut, "      clawctl-hub settings assign --scope machine|channel --scope-id ID --policy ID --revision N [--preview]")
	}
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		usage()
		if len(argv) == 0 {
			return errors.New("settings: 必須指定 list、publish 或 assign")
		}
		return flag.ErrHelp
	}
	switch argv[0] {
	case "list":
		return runSettingsList(ctx, argv[1:], out, errOut, deps)
	case "publish":
		return runSettingsPublish(ctx, argv[1:], out, errOut, deps)
	case "assign":
		return runSettingsAssign(ctx, argv[1:], out, errOut, deps)
	default:
		usage()
		return fmt.Errorf("settings: 不認得 subcommand %q", argv[0])
	}
}

type settingsTransportFlags struct {
	hubURL *string
	json   *bool
}

func addSettingsTransportFlags(fs *flag.FlagSet) settingsTransportFlags {
	return settingsTransportFlags{
		hubURL: fs.String("hub-url", "", "HTTP operator API base URL（省略時自動發現）"),
		json:   fs.Bool("json", false, "輸出 stable operator JSON DTO"),
	}
}

func settingsHTTPClient(flags settingsTransportFlags, fs *flag.FlagSet,
	deps machineCommandDeps,
) (*operatorclient.Client, error) {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "hub-url" {
			explicit = true
		}
	})
	if explicit && (*flags.hubURL == "" || *flags.hubURL != strings.TrimSpace(*flags.hubURL)) {
		return nil, errors.New("settings: --hub-url 不可為空或含首尾空白")
	}
	client, err := deploymentHTTPClient(*flags.hubURL, explicit, deps)
	if err != nil {
		return nil, fmt.Errorf("settings: 連接 Hub 失敗：%w", err)
	}
	return client, nil
}

// settingsIntervalSeconds keeps the CLI and the wire the same unit. A duration
// the wire cannot carry exactly is refused here rather than silently rounded
// into a different policy than the one the operator typed.
func settingsIntervalSeconds(name string, value time.Duration) (int, error) {
	if value <= 0 || value%time.Second != 0 {
		return 0, fmt.Errorf("settings: --%s 必須是正的整秒（例如 90s、5m）", name)
	}
	return int(value / time.Second), nil
}

func validateSettingsReason(value string) error {
	if validateDeploymentReadCLIValue("reason", value, 500) != nil {
		return errors.New("settings: --reason 必填，最多 500 bytes，且不可含控制字元")
	}
	return nil
}

func runSettingsList(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("settings list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addSettingsTransportFlags(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("settings list: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	client, err := settingsHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	board, err := client.SettingBoard(ctx)
	if err != nil {
		return fmt.Errorf("讀取設定盤面失敗：%w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, board)
	}
	fmt.Fprintf(out, "預設：check-in %ds、observation %ds\n",
		board.Defaults.CheckinIntervalSeconds, board.Defaults.ObservationIntervalSeconds)
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "POLICY\tREVISION\tCHECKIN\tOBSERVATION\tASSIGNED")
	for _, policy := range board.Policies {
		fmt.Fprintf(w, "%s\t%d\t%ds\t%ds\t%d\n", policy.PolicyID, policy.Revision,
			policy.Settings.CheckinIntervalSeconds, policy.Settings.ObservationIntervalSeconds,
			policy.Assignments)
	}
	if len(board.Policies) == 0 {
		fmt.Fprintln(w, "（尚未發佈任何設定原則）")
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)
	m := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(m, "MACHINE\tSOURCE\tPOLICY\tCHECKIN\tOBSERVATION\tSTATE")
	for _, machine := range board.Machines {
		policy := "—"
		if machine.PolicyID != "" {
			policy = fmt.Sprintf("%s@%d", machine.PolicyID, machine.Revision)
		}
		name := machine.DisplayName
		if name == "" {
			name = machine.MachineID
		}
		fmt.Fprintf(m, "%s\t%s\t%s\t%ds\t%ds\t%s\n", name, machine.Source, policy,
			machine.Settings.CheckinIntervalSeconds, machine.Settings.ObservationIntervalSeconds,
			machine.VerdictLabel)
	}
	return m.Flush()
}

func runSettingsPublish(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("settings publish", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addSettingsTransportFlags(fs)
	policyID := fs.String("policy", "", "設定原則 ID（小寫、數字與 hyphen）")
	checkin := fs.Duration("checkin", 0, "機器多久報到一次（30s..1h，整秒）")
	observation := fs.Duration("observation", 0, "機器多久量一次工作負載（1m..24h，整秒）")
	previewOnly := fs.Bool("preview", false, "只顯示會發佈成什麼，不寫入")
	reason := fs.String("reason", "", "發佈理由")
	requestKey := fs.String("idempotency-key", "", "重送時沿用的 request key")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("settings publish: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	checkinSeconds, err := settingsIntervalSeconds("checkin", *checkin)
	if err != nil {
		return err
	}
	observationSeconds, err := settingsIntervalSeconds("observation", *observation)
	if err != nil {
		return err
	}
	client, err := settingsHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	preview, err := client.PreviewSettingPolicy(ctx, operatorclient.SettingPolicyPreviewRequest{
		PolicyID: *policyID, CheckinIntervalSeconds: checkinSeconds,
		ObservationIntervalSeconds: observationSeconds,
	})
	if err != nil {
		return fmt.Errorf("預覽設定原則失敗：%w", err)
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		if preview.Unchanged {
			fmt.Fprintf(out, "%s revision %d 已經就是這組值；發佈不會產生新 revision。\n",
				preview.PolicyID, preview.CurrentRev)
			return nil
		}
		fmt.Fprintf(out, "%s：revision %d → %d，check-in %ds、observation %ds，影響 %d 台。\n",
			preview.PolicyID, preview.CurrentRev, preview.NextRev,
			preview.Settings.CheckinIntervalSeconds, preview.Settings.ObservationIntervalSeconds,
			preview.AffectedMachines)
		fmt.Fprintf(out, "下一步：重跑同一道指令，去掉 --preview，加上 --reason REASON。\n")
		return nil
	}
	if err := validateSettingsReason(*reason); err != nil {
		return err
	}
	key, err := catalogMutationKey(*requestKey, "cli-setting-policy")
	if err != nil {
		return err
	}
	expected := preview.CurrentRev
	result, err := client.PublishSettingPolicy(ctx, key, operatorclient.SettingPolicyPublishRequest{
		PolicyID: *policyID, CheckinIntervalSeconds: checkinSeconds,
		ObservationIntervalSeconds: observationSeconds, ExpectedRevision: &expected,
		PreviewDigest: preview.PreviewDigest, ConfirmPolicyID: *policyID, Reason: *reason,
	})
	if err != nil {
		return fmt.Errorf("發佈設定原則失敗（idempotency-key=%q expected-revision=%d）：%w",
			key, expected, err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.Unchanged {
		fmt.Fprintf(out, "%s 仍是 revision %d：這組值已經發佈過了。\n", result.PolicyID, result.Revision)
		return nil
	}
	fmt.Fprintf(out, "%s revision %d 已發佈（check-in %ds、observation %ds，replayed=%t）。\n",
		result.PolicyID, result.Revision, result.Settings.CheckinIntervalSeconds,
		result.Settings.ObservationIntervalSeconds, result.Replayed)
	fmt.Fprintf(out, "下一步：clawctl-hub settings assign --scope machine --scope-id ID --policy %s --revision %d\n",
		result.PolicyID, result.Revision)
	return nil
}

func runSettingsAssign(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("settings assign", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addSettingsTransportFlags(fs)
	scope := fs.String("scope", "machine", "machine 或 channel")
	scopeID := fs.String("scope-id", "", "machine ID 或 channel 名稱")
	policyID := fs.String("policy", "", "要指派的設定原則 ID")
	revision := fs.Int64("revision", 0, "要指派的 policy revision")
	previewOnly := fs.Bool("preview", false, "只顯示會指派成什麼，不寫入")
	reason := fs.String("reason", "", "指派理由")
	requestKey := fs.String("idempotency-key", "", "重送時沿用的 request key")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("settings assign: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if *scope != string(settingpolicy.ScopeMachine) && *scope != string(settingpolicy.ScopeChannel) {
		return errors.New("settings assign: --scope 只接受 machine 或 channel")
	}
	client, err := settingsHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	preview, err := client.PreviewSettingAssignment(ctx, operatorclient.SettingAssignmentPreviewRequest{
		Scope: *scope, ScopeID: *scopeID, PolicyID: *policyID, Revision: *revision,
	})
	if err != nil {
		return fmt.Errorf("預覽設定指派失敗：%w", err)
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		if preview.Unchanged {
			fmt.Fprintf(out, "%s %s 已經在跑這組值；指派不會改變任何機器。\n",
				preview.ScopeLabel, preview.ScopeID)
			return nil
		}
		current := "尚未指派（跑預設值）"
		if preview.CurrentPolicyID != "" {
			current = fmt.Sprintf("%s@%d", preview.CurrentPolicyID, preview.CurrentRevision)
		}
		fmt.Fprintf(out, "%s %s：%s → %s@%d，check-in %ds、observation %ds，影響 %d 台。\n",
			preview.ScopeLabel, preview.ScopeID, current, preview.PolicyID, preview.Revision,
			preview.Settings.CheckinIntervalSeconds, preview.Settings.ObservationIntervalSeconds,
			preview.AffectedMachines)
		fmt.Fprintln(out, "下一步：重跑同一道指令，去掉 --preview，加上 --reason REASON。")
		return nil
	}
	if err := validateSettingsReason(*reason); err != nil {
		return err
	}
	key, err := catalogMutationKey(*requestKey, "cli-setting-assign")
	if err != nil {
		return err
	}
	result, err := client.AssignSettingPolicy(ctx, key, operatorclient.SettingAssignmentRequest{
		Scope: *scope, ScopeID: *scopeID, PolicyID: *policyID, Revision: *revision,
		PreviewDigest: preview.PreviewDigest, ConfirmScopeID: *scopeID, Reason: *reason,
	})
	if err != nil {
		return fmt.Errorf("指派設定原則失敗（idempotency-key=%q）：%w", key, err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.Unchanged {
		fmt.Fprintf(out, "%s %s 仍是 %s@%d：這組值已經在跑了。\n", preview.ScopeLabel,
			result.ScopeID, result.PolicyID, result.PolicyRev)
		return nil
	}
	fmt.Fprintf(out, "%s %s 已指派 %s@%d（check-in %ds、observation %ds，replayed=%t）。\n",
		preview.ScopeLabel, result.ScopeID, result.PolicyID, result.PolicyRev,
		result.Settings.CheckinIntervalSeconds, result.Settings.ObservationIntervalSeconds,
		result.Replayed)
	fmt.Fprintln(out, "下一步：clawctl-hub settings list，看每台回報的是不是這一份。")
	return nil
}

func writeSettingsJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
