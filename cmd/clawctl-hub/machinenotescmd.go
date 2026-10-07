package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
)

func runMachineNotesSubcommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("machine notes", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub machine notes --machine <id> --set <notes> [--preview | --reason REASON --confirm-name CURRENT] [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "  Update manual notes in Hub roster; --set '' clears notes, machine settings and agent remain unchanged.")
		fs.PrintDefaults()
	}
	machine := fs.String("machine", "", "machine_id")
	set := fs.String("set", "", "new notes; empty string clears")
	previewOnly := fs.Bool("preview", false, "preview impact only without applying")
	reason := fs.String("reason", "", "reason for updating notes")
	confirm := fs.String("confirm-name", "", "current display_name")
	key := fs.String("idempotency-key", "", "key used to replay original request")
	previewDigest := fs.String("preview-digest", "", "preview digest used to replay original request")
	flags := addSettingsTransportFlags(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine notes: positional arguments not accepted: %q", strings.Join(fs.Args(), " "))
	}
	seen := map[string]bool{}
	fs.Visit(func(item *flag.Flag) { seen[item.Name] = true })
	if strings.TrimSpace(*machine) == "" || !seen["set"] {
		return errors.New("machine notes: --machine and --set are required; --set '' clears notes")
	}
	if *previewOnly && (*reason != "" || *confirm != "" || *key != "" || *previewDigest != "") {
		return errors.New("machine notes: --preview does not accept apply-only reason, confirm, or replay arguments")
	}
	if !*previewOnly && (strings.TrimSpace(*reason) == "" || strings.TrimSpace(*confirm) == "") {
		return errors.New("machine notes: --reason and --confirm-name are required when applying")
	}
	if (*key == "") != (*previewDigest == "") {
		return errors.New("machine notes: --idempotency-key and --preview-digest must be provided together")
	}
	explicit := seen["hub-url"]
	client, err := machinesHTTPClient(*flags.hubURL, explicit, deps)
	if err != nil {
		return fmt.Errorf("machine notes: %w", err)
	}
	preview := operatorclient.MachineNotesPreviewResponse{}
	if *previewOnly || *previewDigest == "" {
		preview, err = client.PreviewMachineNotes(ctx, *machine,
			operatorclient.MachineNotesPreviewRequest{Notes: *set})
		if err != nil {
			return fmt.Errorf("failed to create roster notes preview: %w", err)
		}
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		current, desired := terminalSafe(preview.CurrentNotes), terminalSafe(preview.Notes)
		if current == "" {
			current = "(no notes)"
		}
		if desired == "" {
			desired = "(clear notes)"
		}
		fmt.Fprintf(out, "%s: %s → %s\nHub roster only updated; machine settings and agent remain unchanged.\npreview digest %s\n",
			terminalSafe(preview.DisplayName), current, desired, preview.PreviewDigest)
		return nil
	}
	requestKey, digest := *key, *previewDigest
	if digest == "" {
		digest = preview.PreviewDigest
		requestKey, err = operator.NewIdempotencyKey("cli-machine-notes")
		if err != nil {
			return fmt.Errorf("failed to create roster notes request key: %w", err)
		}
	}
	result, err := client.PutMachineNotes(ctx, *machine, requestKey, operatorclient.MachineNotesRequest{
		Notes: *set, ConfirmDisplayName: *confirm, PreviewDigest: digest, Reason: *reason,
	})
	if err != nil {
		return fmt.Errorf("failed to update roster notes: %w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	if result.NotesPresent {
		fmt.Fprintf(out, "%s (machine %s) roster notes updated; machine settings and agent remain unchanged.\n",
			terminalSafe(result.DisplayName), terminalSafe(result.MachineID))
	} else {
		fmt.Fprintf(out, "%s (machine %s) roster notes cleared; machine settings and agent remain unchanged.\n",
			terminalSafe(result.DisplayName), terminalSafe(result.MachineID))
	}
	if result.Replayed {
		fmt.Fprintln(out, "This is a successful replay of the original request; roster was not changed again.")
	}
	return nil
}
