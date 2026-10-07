package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type verifierAssignInputs struct {
	VerifierID     string
	JobID          string
	ConfirmName    string
	Reason         string
	Preview        bool
	JSON           bool
	IdempotencyKey string
	PreviewDigest  string
}

func runVerifierAssign(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs, dbPath, hubURL := verifierFlagSet("assign", errOut)
	jobID := fs.String("job", "", "job-id of the job to verify")
	reason := fs.String("reason", "", "reason recorded in audit")
	confirmName := fs.String("confirm-name", "", "verbatim matching verifier display_name required on apply")
	preview := fs.Bool("preview", false, "show impact of this assignment only, do not write")
	jsonOutput := fs.Bool("json", false, "output stable operator JSON DTO (preview only)")
	idempotencyKey := fs.String("idempotency-key", "", "reuse the same request key")
	previewDigest := fs.String("preview-digest", "", "obtained preview digest")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	transport, err := verifierTransportFrom("assign", fs, dbPath, hubURL)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		writeVerifierUsage(errOut)
		return errors.New("verifier assign: must provide a verifier-id")
	}
	verifierID := fs.Arg(0)
	if err := validateVerifierIDArgument("assign", verifierID); err != nil {
		return err
	}
	if err := validateVerifierCLIValue("job", *jobID, 256); err != nil {
		return err
	}
	if strings.Contains(*jobID, "/") || *jobID == "." || *jobID == ".." {
		return errors.New("verifier assign: --job cannot contain slashes or dot segments")
	}
	seen := transport.seen
	if *preview {
		for _, name := range []string{"reason", "confirm-name"} {
			if seen[name] {
				return fmt.Errorf("verifier assign: --preview creates no writes and does not accept --%s", name)
			}
		}
	} else {
		if err := validateVerifierCLIValue("reason", *reason, 512); err != nil {
			return err
		}
		if err := validateVerifierCLIValue("confirm-name", *confirmName, 256); err != nil {
			return err
		}
		if *jsonOutput {
			return errors.New("verifier assign: --json is only for --preview")
		}
	}
	inputs := verifierAssignInputs{
		VerifierID: verifierID, JobID: *jobID, ConfirmName: *confirmName,
		Reason: *reason, Preview: *preview,
		JSON: *jsonOutput, IdempotencyKey: strings.TrimSpace(*idempotencyKey),
		PreviewDigest: strings.TrimSpace(*previewDigest),
	}
	if seen["db"] {
		return withDirectOperatorStore(ctx, "verifier assign", transport.dbPath, deps, func(st *store.Store) error {
			return runVerifierAssignDirect(st, inputs, out, errOut)
		})
	}
	client, err := verifierHTTPClient(transport, deps)
	if err != nil {
		return err
	}
	return runVerifierAssignHTTP(ctx, client, inputs, out, errOut)
}

func runVerifierAssignHTTP(ctx context.Context, client *operatorclient.Client,
	inputs verifierAssignInputs, out, errOut io.Writer,
) error {
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := client.PreviewVerificationAssignment(ctx, inputs.VerifierID, inputs.JobID)
		if err != nil {
			return fmt.Errorf("verifier assign preview (HTTP operator API) failed: %w", err)
		}
		if inputs.Preview {
			return writeVerifierAssignmentPreview(out, preview, inputs.JSON, "HTTP operator API")
		}
		if err := writeVerifierAssignmentPreview(errOut, preview, false, "HTTP operator API"); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := verifierRequestKey(inputs.IdempotencyKey, "cli-verifier-assign")
	if err != nil {
		return err
	}
	result, err := client.AssignVerification(ctx, inputs.VerifierID, key,
		operatorclient.VerificationAssignmentRequest{
			JobID: inputs.JobID, ConfirmVerifierName: inputs.ConfirmName,
			PreviewDigest: digest, Reason: inputs.Reason,
		})
	if err != nil {
		return fmt.Errorf("verifier assign (HTTP operator API; idempotency-key=%q preview-digest=%q%s) failed: %w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return writeVerifierAssignmentReceipt(out, result, key, digest)
}

func runVerifierAssignDirect(st *store.Store, inputs verifierAssignInputs, out, errOut io.Writer) error {
	service := operator.New(st)
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := service.PreviewVerificationAssignment(operator.VerificationAssignmentPreviewRequest{
			VerifierID: inputs.VerifierID, JobID: inputs.JobID,
		})
		if err != nil {
			return fmt.Errorf("verifier assign preview (direct DB operator service) failed: %w", err)
		}
		if inputs.Preview {
			return writeVerifierAssignmentPreview(out, preview, inputs.JSON, "direct DB operator service")
		}
		if err := writeVerifierAssignmentPreview(errOut, preview, false, "direct DB operator service"); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := verifierRequestKey(inputs.IdempotencyKey, "cli-verifier-assign")
	if err != nil {
		return err
	}
	result, err := service.AssignVerification(operator.VerificationAssignmentRequest{
		VerifierID: inputs.VerifierID, JobID: inputs.JobID,
		ConfirmVerifierName: inputs.ConfirmName, PreviewDigest: digest,
		Reason: inputs.Reason, IdempotencyKey: key,
		Actor: operator.Actor{
			SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
			UserAgent: "clawctl-hub verifier assign", SourceKind: operator.SourceKindDirectDBCLI,
		},
	})
	if err != nil {
		return fmt.Errorf("verifier assign (direct DB operator service; idempotency-key=%q preview-digest=%q%s) failed: %w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return writeVerifierAssignmentReceipt(out, result, key, digest)
}

// writeVerifierAssignmentPreview states the two identities, the rule that keeps
// them apart, and the one thing the verifier is being asked to do. It says
// nothing about what the verifier will run, because the Hub does not decide it.
func writeVerifierAssignmentPreview(out io.Writer,
	preview store.OperatorVerificationAssignmentPreviewResult, jsonOutput bool, source string,
) error {
	if jsonOutput {
		return writeJobJSON(out, preview)
	}
	handout := "job has ended; will be delivered immediately after assignment"
	if preview.JobTerminalAt == nil {
		handout = "job is still in progress; will be delivered after it ends"
	}
	_, err := fmt.Fprintf(out,
		"preview (%s): %s (%s, failure_domain %s) will be assigned to verify %s job %s (currently %s)\n%s\nseparation_rule: %s; satisfied_by: %s\ncommands_supplied_by_hub: %t; grants_deployment_gate: %t\npreview-digest=%s\n",
		terminalSafe(source), terminalSafe(preview.VerifierName), terminalSafe(preview.VerifierKind),
		terminalSafe(preview.FailureDomain), terminalSafe(preview.MachineName),
		terminalSafe(preview.JobID), terminalSafe(preview.JobState), handout,
		terminalSafe(preview.SeparationRule), terminalSafe(preview.SatisfiedBy),
		preview.CommandsSuppliedByHub, preview.GrantsDeploymentGate, preview.PreviewDigest)
	return err
}

func writeVerifierAssignmentReceipt(out io.Writer,
	result store.OperatorVerificationAssignmentResult, key, digest string,
) error {
	state := "assigned"
	if result.Replayed {
		state = "replayed"
	}
	_, err := fmt.Fprintf(out,
		"%s: verifier %s (%s) to verify on %s job %s (assignment_id %s, assigned_at %s)\npending satisfaction of report conditions fixed by preview; assignment does not determine job outcome, only complete reports from verifiers granting deployment gates participate in stable promotion\nidempotency-key=%s preview-digest=%s\n",
		state, terminalSafe(result.VerifierName), terminalSafe(result.VerifierID),
		terminalSafe(result.MachineID),
		terminalSafe(result.JobID), terminalSafe(result.AssignmentID),
		result.AssignedAt.Format("2006-01-02T15:04:05Z"), key, digest)
	return err
}
