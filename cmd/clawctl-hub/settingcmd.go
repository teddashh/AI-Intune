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
		fmt.Fprintln(errOut, "Usage: clawctl-hub settings list")
		fmt.Fprintln(errOut, "       clawctl-hub settings publish --policy ID --checkin DUR --observation DUR [--preview]")
		fmt.Fprintln(errOut, "       clawctl-hub settings assign --scope machine|channel --scope-id ID --policy ID --revision N [--preview]")
	}
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		usage()
		if len(argv) == 0 {
			return errors.New("settings: must specify list, publish, or assign")
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
		return fmt.Errorf("settings: unrecognized subcommand %q", argv[0])
	}
}

type settingsTransportFlags struct {
	hubURL *string
	json   *bool
}

func addSettingsTransportFlags(fs *flag.FlagSet) settingsTransportFlags {
	return settingsTransportFlags{
		hubURL: fs.String("hub-url", "", "HTTP operator API base URL (auto-discovered if omitted)"),
		json:   fs.Bool("json", false, "output stable operator JSON DTO"),
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
		return nil, errors.New("settings: --hub-url cannot be empty or contain leading/trailing whitespace")
	}
	client, err := deploymentHTTPClient(*flags.hubURL, explicit, deps)
	if err != nil {
		return nil, fmt.Errorf("settings: failed to connect to Hub: %w", err)
	}
	return client, nil
}

// settingsIntervalSeconds keeps the CLI and the wire the same unit. A duration
// the wire cannot carry exactly is refused here rather than silently rounded
// into a different policy than the one the operator typed.
func settingsIntervalSeconds(name string, value time.Duration) (int, error) {
	if value <= 0 || value%time.Second != 0 {
		return 0, fmt.Errorf("settings: --%s must be positive whole seconds (e.g. 90s, 5m)", name)
	}
	return int(value / time.Second), nil
}

func validateSettingsReason(value string) error {
	if validateDeploymentReadCLIValue("reason", value, 500) != nil {
		return errors.New("settings: --reason is required, at most 500 bytes, and cannot contain control characters")
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
		return fmt.Errorf("settings list: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	client, err := settingsHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	board, err := client.SettingBoard(ctx)
	if err != nil {
		return fmt.Errorf("failed to read settings board: %w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, board)
	}
	fmt.Fprintf(out, "defaults: check-in %ds, observation %ds\n",
		board.Defaults.CheckinIntervalSeconds, board.Defaults.ObservationIntervalSeconds)
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "POLICY\tREVISION\tCHECKIN\tOBSERVATION\tASSIGNED")
	for _, policy := range board.Policies {
		fmt.Fprintf(w, "%s\t%d\t%ds\t%ds\t%d\n", policy.PolicyID, policy.Revision,
			policy.Settings.CheckinIntervalSeconds, policy.Settings.ObservationIntervalSeconds,
			policy.Assignments)
	}
	if len(board.Policies) == 0 {
		fmt.Fprintln(w, "(no setting policies published yet)")
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
	policyID := fs.String("policy", "", "setting policy ID (lowercase, digits, and hyphens)")
	checkin := fs.Duration("checkin", 0, "device check-in interval (30s..1h, whole seconds)")
	observation := fs.Duration("observation", 0, "device workload observation interval (1m..24h, whole seconds)")
	previewOnly := fs.Bool("preview", false, "show what would be published only, do not write")
	reason := fs.String("reason", "", "publishing reason")
	requestKey := fs.String("idempotency-key", "", "request key reused on resend")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("settings publish: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
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
		return fmt.Errorf("failed to preview setting policy: %w", err)
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		if preview.Unchanged {
			fmt.Fprintf(out, "%s revision %d already matches these values; publishing will not create a new revision\n",
				preview.PolicyID, preview.CurrentRev)
			return nil
		}
		fmt.Fprintf(out, "%s: revision %d → %d, check-in %ds, observation %ds, affects %d machines\n",
			preview.PolicyID, preview.CurrentRev, preview.NextRev,
			preview.Settings.CheckinIntervalSeconds, preview.Settings.ObservationIntervalSeconds,
			preview.AffectedMachines)
		fmt.Fprintln(out, "next step: rerun the same command without --preview and add --reason REASON")
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
		return fmt.Errorf("failed to publish setting policy (idempotency-key=%q expected-revision=%d): %w",
			key, expected, err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.Unchanged {
		fmt.Fprintf(out, "%s is still revision %d: these values have already been published\n", result.PolicyID, result.Revision)
		return nil
	}
	fmt.Fprintf(out, "%s revision %d published (check-in %ds, observation %ds, replayed=%t)\n",
		result.PolicyID, result.Revision, result.Settings.CheckinIntervalSeconds,
		result.Settings.ObservationIntervalSeconds, result.Replayed)
	fmt.Fprintf(out, "next step: clawctl-hub settings assign --scope machine --scope-id ID --policy %s --revision %d\n",
		result.PolicyID, result.Revision)
	return nil
}

func runSettingsAssign(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("settings assign", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addSettingsTransportFlags(fs)
	scope := fs.String("scope", "machine", "machine or channel")
	scopeID := fs.String("scope-id", "", "machine ID or channel name")
	policyID := fs.String("policy", "", "setting policy ID to assign")
	revision := fs.Int64("revision", 0, "policy revision to assign")
	previewOnly := fs.Bool("preview", false, "show what would be assigned only, do not write")
	reason := fs.String("reason", "", "assignment reason")
	requestKey := fs.String("idempotency-key", "", "request key reused on resend")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("settings assign: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if *scope != string(settingpolicy.ScopeMachine) && *scope != string(settingpolicy.ScopeChannel) {
		return errors.New("settings assign: --scope only accepts machine or channel")
	}
	client, err := settingsHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	preview, err := client.PreviewSettingAssignment(ctx, operatorclient.SettingAssignmentPreviewRequest{
		Scope: *scope, ScopeID: *scopeID, PolicyID: *policyID, Revision: *revision,
	})
	if err != nil {
		return fmt.Errorf("failed to preview setting assignment: %w", err)
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		if preview.Unchanged {
			fmt.Fprintf(out, "%s %s is already running these values; assignment will not change any machines\n",
				preview.ScopeLabel, preview.ScopeID)
			return nil
		}
		current := "unassigned (running defaults)"
		if preview.CurrentPolicyID != "" {
			current = fmt.Sprintf("%s@%d", preview.CurrentPolicyID, preview.CurrentRevision)
		}
		fmt.Fprintf(out, "%s %s: %s → %s@%d, check-in %ds, observation %ds, affects %d machines\n",
			preview.ScopeLabel, preview.ScopeID, current, preview.PolicyID, preview.Revision,
			preview.Settings.CheckinIntervalSeconds, preview.Settings.ObservationIntervalSeconds,
			preview.AffectedMachines)
		fmt.Fprintln(out, "next step: rerun the same command without --preview and add --reason REASON")
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
		return fmt.Errorf("failed to assign setting policy (idempotency-key=%q): %w", key, err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.Unchanged {
		fmt.Fprintf(out, "%s %s is still %s@%d: these values are already in effect\n", preview.ScopeLabel,
			result.ScopeID, result.PolicyID, result.PolicyRev)
		return nil
	}
	fmt.Fprintf(out, "%s %s assigned %s@%d (check-in %ds, observation %ds, replayed=%t)\n",
		preview.ScopeLabel, result.ScopeID, result.PolicyID, result.PolicyRev,
		result.Settings.CheckinIntervalSeconds, result.Settings.ObservationIntervalSeconds,
		result.Replayed)
	fmt.Fprintln(out, "next step: clawctl-hub settings list to inspect whether each machine is reporting this policy")
	return nil
}

func writeSettingsJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}
