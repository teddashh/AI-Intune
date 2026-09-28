package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
)

func cmdCompliance(argv []string) {
	if err := runComplianceCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(terminalSafe(err.Error()))
	}
}

func runComplianceCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runComplianceCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runComplianceCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	usage := func() {
		fmt.Fprintln(errOut, "用法：clawctl-hub compliance list")
		fmt.Fprintln(errOut, "      clawctl-hub compliance publish --policy ID [規則 flags] [--preview]")
		fmt.Fprintln(errOut, "      clawctl-hub compliance assign --scope machine|channel --scope-id ID --policy ID --revision N [--preview]")
		fmt.Fprintln(errOut, "規則 flags：--checkin-max-age DUR、--agent-version VER、--disk-free-min-percent N、--settings-applied、--jobs-enabled")
		fmt.Fprintln(errOut, "動作 flags：--block-jobs-after DUR（0 表示判定不符合就立即停發工作單）")
	}
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		usage()
		if len(argv) == 0 {
			return errors.New("compliance: 必須指定 list、publish 或 assign")
		}
		return flag.ErrHelp
	}
	switch argv[0] {
	case "list":
		return runComplianceList(ctx, argv[1:], out, errOut, deps)
	case "publish":
		return runCompliancePublish(ctx, argv[1:], out, errOut, deps)
	case "assign":
		return runComplianceAssign(ctx, argv[1:], out, errOut, deps)
	default:
		usage()
		return fmt.Errorf("compliance: 不認得 subcommand %q", argv[0])
	}
}

func complianceHTTPClient(flags settingsTransportFlags, fs *flag.FlagSet,
	deps machineCommandDeps,
) (*operatorclient.Client, error) {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "hub-url" {
			explicit = true
		}
	})
	if explicit && (*flags.hubURL == "" || *flags.hubURL != strings.TrimSpace(*flags.hubURL)) {
		return nil, errors.New("compliance: --hub-url 不可為空或含首尾空白")
	}
	client, err := deploymentHTTPClient(*flags.hubURL, explicit, deps)
	if err != nil {
		return nil, fmt.Errorf("compliance: 連接 Hub 失敗：%w", err)
	}
	return client, nil
}

func validateComplianceReason(value string) error {
	if validateDeploymentReadCLIValue("reason", value, 500) != nil {
		return errors.New("compliance: --reason 必填，最多 500 bytes，且不可含控制字元")
	}
	return nil
}

func runComplianceList(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("compliance list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addSettingsTransportFlags(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("compliance list: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	client, err := complianceHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	board, err := client.ComplianceBoard(ctx)
	if err != nil {
		return fmt.Errorf("讀取合規性盤面失敗：%w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, board)
	}
	fmt.Fprintf(out, "判決時間：%s（報到新鮮度會隨時間改變）\n",
		board.EvaluatedAt.Local().Format(time.RFC3339))
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "POLICY\tREVISION\tRULES\tACTIONS\tASSIGNED")
	for _, policy := range board.Policies {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\n", policy.PolicyID, policy.Revision,
			complianceRuleSummary(policy.Policy), complianceActionSummary(policy.Policy),
			policy.Assignments)
	}
	if len(board.Policies) == 0 {
		fmt.Fprintln(w, "（尚未發佈任何合規性原則）")
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)
	m := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(m, "MACHINE\tSOURCE\tPOLICY\tVERDICT\tACTION\tEVIDENCE")
	for _, machine := range board.Machines {
		policy := "—"
		if machine.PolicyID != "" {
			policy = fmt.Sprintf("%s@%d", machine.PolicyID, machine.Revision)
		}
		name := machine.DisplayName
		if name == "" {
			name = machine.MachineID
		}
		evidence := "—"
		if len(machine.Results) > 0 {
			parts := make([]string, 0, len(machine.Results))
			for _, r := range machine.Results {
				parts = append(parts, fmt.Sprintf("%s=%s", r.Kind, r.Outcome))
			}
			evidence = strings.Join(parts, " ")
		}
		action := "—"
		if len(machine.Actions) > 0 {
			parts := make([]string, 0, len(machine.Actions))
			for _, a := range machine.Actions {
				parts = append(parts, fmt.Sprintf("%s=%s", a.Label, a.StateLabel))
			}
			action = strings.Join(parts, " ")
		}
		fmt.Fprintf(m, "%s\t%s\t%s\t%s\t%s\t%s\n", name, machine.Source, policy,
			machine.VerdictLabel, action, evidence)
	}
	return m.Flush()
}

// complianceRuleSummary is the one-cell form of a rule set for the list table.
func complianceRuleSummary(p compliance.Policy) string {
	parts := make([]string, 0, len(p.Rules))
	for _, rule := range p.Rules {
		if bound := compliance.RuleBound(rule); bound != "" {
			parts = append(parts, fmt.Sprintf("%s(%s)", rule.Kind, bound))
			continue
		}
		parts = append(parts, string(rule.Kind))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " ")
}

// complianceActionSummary is the one-cell form of a policy's consequences.
func complianceActionSummary(p compliance.Policy) string {
	if len(p.Actions) == 0 {
		return "只回報"
	}
	parts := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		parts = append(parts, fmt.Sprintf("%s(寬限 %ds)", a.Kind, a.GraceSeconds))
	}
	return strings.Join(parts, " ")
}

// complianceRuleFlags is the CLI's rule vocabulary. Each rule kind appears at
// most once, so a duplicate rule is not expressible from this plane either.
type complianceRuleFlags struct {
	checkinMaxAge   *time.Duration
	agentVersion    *string
	diskFreePercent *int
	settingsApplied *bool
	jobsEnabled     *bool
	// blockJobsAfter is -1 when the operator did not ask for the action at
	// all. Zero is a real answer — it means the moment the verdict says
	// noncompliant — so absent and zero cannot be the same value.
	blockJobsAfter *time.Duration
}

func addComplianceRuleFlags(fs *flag.FlagSet) complianceRuleFlags {
	return complianceRuleFlags{
		checkinMaxAge: fs.Duration("checkin-max-age", 0,
			"距離上次報到最久多久（60s..168h，整秒）"),
		agentVersion: fs.String("agent-version", "",
			"要求的 agent 版本字串"),
		diskFreePercent: fs.Int("disk-free-min-percent", 0,
			"剩餘磁碟空間的下限百分比（1..99）"),
		settingsApplied: fs.Bool("settings-applied", false,
			"要求裝置回報的設定就是 Hub 指派的那一份"),
		jobsEnabled: fs.Bool("jobs-enabled", false,
			"要求裝置回報它收工作單"),
		blockJobsAfter: fs.Duration("block-jobs-after", -1,
			"連續不符合多久之後停發工作單（0 表示立即，最長 24h，整秒）"),
	}
}

func (f complianceRuleFlags) actions() ([]operatorclient.ComplianceAction, error) {
	if *f.blockJobsAfter < 0 {
		return nil, nil
	}
	if *f.blockJobsAfter%time.Second != 0 ||
		*f.blockJobsAfter > compliance.MaxGraceSeconds*time.Second {
		return nil, fmt.Errorf("compliance: --block-jobs-after 必須是 0 到 %ds 之間的整秒",
			compliance.MaxGraceSeconds)
	}
	return []operatorclient.ComplianceAction{{
		Kind:         string(compliance.ActionBlockJobs),
		GraceSeconds: int(*f.blockJobsAfter / time.Second),
	}}, nil
}

func (f complianceRuleFlags) rules() ([]operatorclient.ComplianceRule, error) {
	var out []operatorclient.ComplianceRule
	if *f.checkinMaxAge != 0 {
		if *f.checkinMaxAge <= 0 || *f.checkinMaxAge%time.Second != 0 {
			return nil, errors.New("compliance: --checkin-max-age 必須是正的整秒（例如 15m、24h）")
		}
		out = append(out, operatorclient.ComplianceRule{
			Kind:          string(compliance.RuleCheckinMaxAge),
			MaxAgeSeconds: int(*f.checkinMaxAge / time.Second),
		})
	}
	if *f.agentVersion != "" {
		out = append(out, operatorclient.ComplianceRule{
			Kind: string(compliance.RuleAgentVersion), AgentVersion: *f.agentVersion,
		})
	}
	if *f.settingsApplied {
		out = append(out, operatorclient.ComplianceRule{Kind: string(compliance.RuleSettingsApplied)})
	}
	if *f.diskFreePercent != 0 {
		out = append(out, operatorclient.ComplianceRule{
			Kind: string(compliance.RuleDiskFreeMinPercent), MinFreePercent: *f.diskFreePercent,
		})
	}
	if *f.jobsEnabled {
		out = append(out, operatorclient.ComplianceRule{Kind: string(compliance.RuleJobsEnabled)})
	}
	if len(out) == 0 {
		return nil, errors.New("compliance publish: 至少要指定一條規則 flag")
	}
	return out, nil
}

func runCompliancePublish(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("compliance publish", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addSettingsTransportFlags(fs)
	policyID := fs.String("policy", "", "合規性原則 ID（小寫、數字與 hyphen）")
	ruleFlags := addComplianceRuleFlags(fs)
	previewOnly := fs.Bool("preview", false, "只顯示會發佈成什麼，不寫入")
	reason := fs.String("reason", "", "發佈理由")
	requestKey := fs.String("idempotency-key", "", "重送時沿用的 request key")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("compliance publish: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	rules, err := ruleFlags.rules()
	if err != nil {
		return err
	}
	actions, err := ruleFlags.actions()
	if err != nil {
		return err
	}
	client, err := complianceHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	preview, err := client.PreviewCompliancePolicy(ctx, operatorclient.CompliancePolicyPreviewRequest{
		PolicyID: *policyID, Rules: rules, Actions: actions,
	})
	if err != nil {
		return fmt.Errorf("預覽合規性原則失敗：%w", err)
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		if preview.Unchanged {
			fmt.Fprintf(out, "%s revision %d 已經就是這組規則；發佈不會產生新 revision。\n",
				preview.PolicyID, preview.CurrentRev)
			return nil
		}
		fmt.Fprintf(out, "%s：revision %d → %d，規則 %s，動作 %s，影響 %d 台。\n",
			preview.PolicyID, preview.CurrentRev, preview.NextRev,
			complianceRuleSummary(preview.Policy), complianceActionSummary(preview.Policy),
			preview.AffectedMachines)
		for _, a := range preview.Actions {
			fmt.Fprintf(out, "  %s：%s，%s。\n", a.Label, a.Effect, a.Grace)
		}
		fmt.Fprintln(out, "下一步：重跑同一道指令，去掉 --preview，加上 --reason REASON。")
		return nil
	}
	if err := validateComplianceReason(*reason); err != nil {
		return err
	}
	key, err := catalogMutationKey(*requestKey, "cli-compliance-policy")
	if err != nil {
		return err
	}
	expected := preview.CurrentRev
	result, err := client.PublishCompliancePolicy(ctx, key, operatorclient.CompliancePolicyPublishRequest{
		PolicyID: *policyID, Rules: rules, Actions: actions, ExpectedRevision: &expected,
		PreviewDigest: preview.PreviewDigest, ConfirmPolicyID: *policyID, Reason: *reason,
	})
	if err != nil {
		return fmt.Errorf("發佈合規性原則失敗（idempotency-key=%q expected-revision=%d）：%w",
			key, expected, err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.Unchanged {
		fmt.Fprintf(out, "%s 仍是 revision %d：這組規則已經發佈過了。\n", result.PolicyID, result.Revision)
		return nil
	}
	fmt.Fprintf(out, "%s revision %d 已發佈（規則 %s，動作 %s，replayed=%t）。\n",
		result.PolicyID, result.Revision, complianceRuleSummary(result.Policy),
		complianceActionSummary(result.Policy), result.Replayed)
	fmt.Fprintf(out, "下一步：clawctl-hub compliance assign --scope machine --scope-id ID --policy %s --revision %d\n",
		result.PolicyID, result.Revision)
	return nil
}

func runComplianceAssign(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("compliance assign", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addSettingsTransportFlags(fs)
	scope := fs.String("scope", "machine", "machine 或 channel")
	scopeID := fs.String("scope-id", "", "machine ID 或 channel 名稱")
	policyID := fs.String("policy", "", "要指派的合規性原則 ID")
	revision := fs.Int64("revision", 0, "要指派的 policy revision")
	previewOnly := fs.Bool("preview", false, "只顯示會指派成什麼，不寫入")
	reason := fs.String("reason", "", "指派理由")
	requestKey := fs.String("idempotency-key", "", "重送時沿用的 request key")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("compliance assign: 不接受 positional arguments：%q", strings.Join(fs.Args(), " "))
	}
	if *scope != string(compliance.ScopeMachine) && *scope != string(compliance.ScopeChannel) {
		return errors.New("compliance assign: --scope 只接受 machine 或 channel")
	}
	client, err := complianceHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	preview, err := client.PreviewComplianceAssignment(ctx, operatorclient.ComplianceAssignmentPreviewRequest{
		Scope: *scope, ScopeID: *scopeID, PolicyID: *policyID, Revision: *revision,
	})
	if err != nil {
		return fmt.Errorf("預覽合規性指派失敗：%w", err)
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		if preview.Unchanged {
			fmt.Fprintf(out, "%s %s 已經用這組規則判決；指派不會改變任何判決。\n",
				preview.ScopeLabel, preview.ScopeID)
			return nil
		}
		current := "尚未指派（不評估）"
		if preview.CurrentPolicyID != "" {
			current = fmt.Sprintf("%s@%d", preview.CurrentPolicyID, preview.CurrentRevision)
		}
		fmt.Fprintf(out, "%s %s：%s → %s@%d，規則 %s，動作 %s，影響 %d 台。\n",
			preview.ScopeLabel, preview.ScopeID, current, preview.PolicyID, preview.Revision,
			complianceRuleSummary(preview.Policy), complianceActionSummary(preview.Policy),
			preview.AffectedMachines)
		for _, a := range preview.Actions {
			fmt.Fprintf(out, "  %s：%s，%s。\n", a.Label, a.Effect, a.Grace)
		}
		fmt.Fprintln(out, "下一步：重跑同一道指令，去掉 --preview，加上 --reason REASON。")
		return nil
	}
	if err := validateComplianceReason(*reason); err != nil {
		return err
	}
	key, err := catalogMutationKey(*requestKey, "cli-compliance-assign")
	if err != nil {
		return err
	}
	result, err := client.AssignCompliancePolicy(ctx, key, operatorclient.ComplianceAssignmentRequest{
		Scope: *scope, ScopeID: *scopeID, PolicyID: *policyID, Revision: *revision,
		PreviewDigest: preview.PreviewDigest, ConfirmScopeID: *scopeID, Reason: *reason,
	})
	if err != nil {
		return fmt.Errorf("指派合規性原則失敗（idempotency-key=%q）：%w", key, err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.Unchanged {
		fmt.Fprintf(out, "%s %s 仍是 %s@%d：這組規則已經在判了。\n", preview.ScopeLabel,
			result.ScopeID, result.PolicyID, result.PolicyRev)
		return nil
	}
	fmt.Fprintf(out, "%s %s 已指派 %s@%d（規則 %s，動作 %s，replayed=%t）。\n",
		preview.ScopeLabel, result.ScopeID, result.PolicyID, result.PolicyRev,
		complianceRuleSummary(result.Policy), complianceActionSummary(result.Policy), result.Replayed)
	for _, a := range preview.Actions {
		fmt.Fprintf(out, "  %s：%s，%s。\n", a.Label, a.Effect, a.Grace)
	}
	fmt.Fprintln(out, "下一步：clawctl-hub compliance list，看每台的判決與證據。")
	return nil
}
