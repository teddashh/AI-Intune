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
		fmt.Fprintln(errOut, "Usage: clawctl-hub compliance list")
		fmt.Fprintln(errOut, "       clawctl-hub compliance publish --policy ID [rule flags] [--preview]")
		fmt.Fprintln(errOut, "       clawctl-hub compliance assign --scope machine|channel --scope-id ID --policy ID --revision N [--preview]")
		fmt.Fprintln(errOut, "rule flags: --checkin-max-age DUR, --agent-version VER, --disk-free-min-percent N, --settings-applied, --jobs-enabled")
		fmt.Fprintln(errOut, "action flags: --block-jobs-after DUR (0 means stop dispatching jobs immediately upon noncompliant verdict)")
	}
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		usage()
		if len(argv) == 0 {
			return errors.New("compliance: must specify list, publish, or assign")
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
		return fmt.Errorf("compliance: unrecognized subcommand %q", argv[0])
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
		return nil, errors.New("compliance: --hub-url cannot be empty or contain leading/trailing whitespace")
	}
	client, err := deploymentHTTPClient(*flags.hubURL, explicit, deps)
	if err != nil {
		return nil, fmt.Errorf("compliance: failed to connect to Hub: %w", err)
	}
	return client, nil
}

func validateComplianceReason(value string) error {
	if validateDeploymentReadCLIValue("reason", value, 500) != nil {
		return errors.New("compliance: --reason is required, at most 500 bytes, and cannot contain control characters")
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
		return fmt.Errorf("compliance list: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	client, err := complianceHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	board, err := client.ComplianceBoard(ctx)
	if err != nil {
		return fmt.Errorf("failed to read compliance board: %w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, board)
	}
	fmt.Fprintf(out, "evaluated at: %s (check-in freshness changes over time)\n",
		board.EvaluatedAt.Local().Format(time.RFC3339))
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "POLICY\tREVISION\tRULES\tACTIONS\tASSIGNED")
	for _, policy := range board.Policies {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%d\n", policy.PolicyID, policy.Revision,
			complianceRuleSummary(policy.Policy), complianceActionSummary(policy.Policy),
			policy.Assignments)
	}
	if len(board.Policies) == 0 {
		fmt.Fprintln(w, "(no compliance policies published yet)")
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
		return "report only"
	}
	parts := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		parts = append(parts, fmt.Sprintf("%s(grace %ds)", a.Kind, a.GraceSeconds))
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
			"maximum duration since last check-in (60s..168h, whole seconds)"),
		agentVersion: fs.String("agent-version", "",
			"required agent version string"),
		diskFreePercent: fs.Int("disk-free-min-percent", 0,
			"minimum free disk percentage (1..99)"),
		settingsApplied: fs.Bool("settings-applied", false,
			"require device to report settings matching those assigned by Hub"),
		jobsEnabled: fs.Bool("jobs-enabled", false,
			"require device to report that it accepts jobs"),
		blockJobsAfter: fs.Duration("block-jobs-after", -1,
			"duration of continuous non-compliance before blocking jobs (0 means immediate, up to 24h, whole seconds)"),
	}
}

func (f complianceRuleFlags) actions() ([]operatorclient.ComplianceAction, error) {
	if *f.blockJobsAfter < 0 {
		return nil, nil
	}
	if *f.blockJobsAfter%time.Second != 0 ||
		*f.blockJobsAfter > compliance.MaxGraceSeconds*time.Second {
		return nil, fmt.Errorf("compliance: --block-jobs-after must be whole seconds between 0 and %ds",
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
			return nil, errors.New("compliance: --checkin-max-age must be positive whole seconds (e.g. 15m, 24h)")
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
		return nil, errors.New("compliance publish: at least one rule flag must be specified")
	}
	return out, nil
}

func runCompliancePublish(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("compliance publish", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addSettingsTransportFlags(fs)
	policyID := fs.String("policy", "", "compliance policy ID (lowercase, digits, and hyphens)")
	ruleFlags := addComplianceRuleFlags(fs)
	previewOnly := fs.Bool("preview", false, "show what would be published only, do not write")
	reason := fs.String("reason", "", "publishing reason")
	requestKey := fs.String("idempotency-key", "", "request key reused on resend")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("compliance publish: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
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
		return fmt.Errorf("failed to preview compliance policy: %w", err)
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		if preview.Unchanged {
			fmt.Fprintf(out, "%s revision %d already matches these rules; publishing will not create a new revision\n",
				preview.PolicyID, preview.CurrentRev)
			return nil
		}
		fmt.Fprintf(out, "%s: revision %d → %d, rules %s, actions %s, affects %d machines\n",
			preview.PolicyID, preview.CurrentRev, preview.NextRev,
			complianceRuleSummary(preview.Policy), complianceActionSummary(preview.Policy),
			preview.AffectedMachines)
		for _, a := range preview.Actions {
			fmt.Fprintf(out, "  %s: %s, %s\n", a.Label, a.Effect, a.Grace)
		}
		fmt.Fprintln(out, "next step: rerun the same command without --preview and add --reason REASON")
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
		return fmt.Errorf("failed to publish compliance policy (idempotency-key=%q expected-revision=%d): %w",
			key, expected, err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.Unchanged {
		fmt.Fprintf(out, "%s is still revision %d: these rules have already been published\n", result.PolicyID, result.Revision)
		return nil
	}
	fmt.Fprintf(out, "%s revision %d published (rules %s, actions %s, replayed=%t)\n",
		result.PolicyID, result.Revision, complianceRuleSummary(result.Policy),
		complianceActionSummary(result.Policy), result.Replayed)
	fmt.Fprintf(out, "next step: clawctl-hub compliance assign --scope machine --scope-id ID --policy %s --revision %d\n",
		result.PolicyID, result.Revision)
	return nil
}

func runComplianceAssign(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("compliance assign", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addSettingsTransportFlags(fs)
	scope := fs.String("scope", "machine", "machine or channel")
	scopeID := fs.String("scope-id", "", "machine ID or channel name")
	policyID := fs.String("policy", "", "compliance policy ID to assign")
	revision := fs.Int64("revision", 0, "policy revision to assign")
	previewOnly := fs.Bool("preview", false, "show what would be assigned only, do not write")
	reason := fs.String("reason", "", "assignment reason")
	requestKey := fs.String("idempotency-key", "", "request key reused on resend")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("compliance assign: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if *scope != string(compliance.ScopeMachine) && *scope != string(compliance.ScopeChannel) {
		return errors.New("compliance assign: --scope only accepts machine or channel")
	}
	client, err := complianceHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	preview, err := client.PreviewComplianceAssignment(ctx, operatorclient.ComplianceAssignmentPreviewRequest{
		Scope: *scope, ScopeID: *scopeID, PolicyID: *policyID, Revision: *revision,
	})
	if err != nil {
		return fmt.Errorf("failed to preview compliance assignment: %w", err)
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		if preview.Unchanged {
			fmt.Fprintf(out, "%s %s already evaluated under these rules; assignment will not change any verdicts\n",
				preview.ScopeLabel, preview.ScopeID)
			return nil
		}
		current := "unassigned (not evaluated)"
		if preview.CurrentPolicyID != "" {
			current = fmt.Sprintf("%s@%d", preview.CurrentPolicyID, preview.CurrentRevision)
		}
		fmt.Fprintf(out, "%s %s: %s → %s@%d, rules %s, actions %s, affects %d machines\n",
			preview.ScopeLabel, preview.ScopeID, current, preview.PolicyID, preview.Revision,
			complianceRuleSummary(preview.Policy), complianceActionSummary(preview.Policy),
			preview.AffectedMachines)
		for _, a := range preview.Actions {
			fmt.Fprintf(out, "  %s: %s, %s\n", a.Label, a.Effect, a.Grace)
		}
		fmt.Fprintln(out, "next step: rerun the same command without --preview and add --reason REASON")
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
		return fmt.Errorf("failed to assign compliance policy (idempotency-key=%q): %w", key, err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.Unchanged {
		fmt.Fprintf(out, "%s %s is still %s@%d: these rules are already in effect\n", preview.ScopeLabel,
			result.ScopeID, result.PolicyID, result.PolicyRev)
		return nil
	}
	fmt.Fprintf(out, "%s %s assigned %s@%d (rules %s, actions %s, replayed=%t)\n",
		preview.ScopeLabel, result.ScopeID, result.PolicyID, result.PolicyRev,
		complianceRuleSummary(result.Policy), complianceActionSummary(result.Policy), result.Replayed)
	for _, a := range preview.Actions {
		fmt.Fprintf(out, "  %s: %s, %s\n", a.Label, a.Effect, a.Grace)
	}
	fmt.Fprintln(out, "next step: clawctl-hub compliance list to inspect verdicts and evidence for each machine")
	return nil
}
