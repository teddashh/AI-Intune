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

func runMachineRenameSubcommand(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("machine rename", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub machine rename --machine <id> --set <display_name> [--preview | --reason REASON --confirm-name CURRENT] [--json] [--hub-url URL]")
		fmt.Fprintln(errOut, "  Rename Hub roster display name and name-based expectations; machine ID, agent, and machine hostname remain unchanged.")
		fs.PrintDefaults()
	}
	machine := fs.String("machine", "", "machine_id")
	set := fs.String("set", "", "New display_name")
	previewOnly := fs.Bool("preview", false, "Preview impact only without applying")
	reason := fs.String("reason", "", "Reason for rename")
	confirm := fs.String("confirm-name", "", "Current display_name")
	key := fs.String("idempotency-key", "", "Request key to resend original request")
	previewDigest := fs.String("preview-digest", "", "Preview digest to resend original request")
	flags := addSettingsTransportFlags(fs)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("machine rename: unexpected positional arguments: %q", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*machine) == "" || strings.TrimSpace(*set) == "" {
		return errors.New("machine rename: --machine and --set are required")
	}
	if *previewOnly && (*reason != "" || *confirm != "" || *key != "" || *previewDigest != "") {
		return errors.New("machine rename: --preview does not accept apply parameters reason, confirm, or replay")
	}
	if !*previewOnly && (strings.TrimSpace(*reason) == "" || strings.TrimSpace(*confirm) == "") {
		return errors.New("machine rename: --reason and --confirm-name are required when applying")
	}
	if (*key == "") != (*previewDigest == "") {
		return errors.New("machine rename: --idempotency-key and --preview-digest must be provided together")
	}
	explicit := false
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "hub-url" {
			explicit = true
		}
	})
	client, err := machinesHTTPClient(*flags.hubURL, explicit, deps)
	if err != nil {
		return fmt.Errorf("machine rename: %w", err)
	}
	preview := operatorclient.MachineRenamePreviewResponse{}
	if *previewOnly || *previewDigest == "" {
		preview, err = client.PreviewMachineRename(ctx, *machine,
			operatorclient.MachineRenamePreviewRequest{DisplayName: *set})
		if err != nil {
			return fmt.Errorf("failed to create rename preview: %w", err)
		}
	}
	if *previewOnly {
		if *flags.json {
			return writeSettingsJSON(out, preview)
		}
		fmt.Fprintf(out, "%s → %s\nname-based expectations will use the new name; machine ID, agent, and hostname on the machine remain unchanged.\npreview digest %s\n",
			preview.CurrentDisplayName, preview.DisplayName, preview.PreviewDigest)
		if preview.PendingTokenLabelChanges {
			fmt.Fprintln(out, "Unredeemed enrollment ticket labels will also be updated; token unchanged.")
		}
		return nil
	}
	requestKey, digest := *key, *previewDigest
	if digest == "" {
		digest = preview.PreviewDigest
		requestKey, err = operator.NewIdempotencyKey("cli-machine-rename")
		if err != nil {
			return fmt.Errorf("failed to create rename request key: %w", err)
		}
	}
	result, err := client.PutMachineRename(ctx, *machine, requestKey, operatorclient.MachineRenameRequest{
		DisplayName: *set, ConfirmDisplayName: *confirm, PreviewDigest: digest, Reason: *reason,
	})
	if err != nil {
		return fmt.Errorf("rename failed: %w", err)
	}
	if *flags.json {
		return writeSettingsJSON(out, result)
	}
	fmt.Fprintf(out, "%s → %s (machine %s)\n", result.PreviousDisplayName, result.DisplayName, result.MachineID)
	if result.PendingTokenLabelChanged {
		fmt.Fprintln(out, "Unredeemed enrollment ticket label has also been updated; token unchanged.")
	}
	if result.Replayed {
		fmt.Fprintln(out, "This is a successful replay of the original request; roster was not changed again.")
	}
	return nil
}
