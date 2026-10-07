package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type enrollTokenRevokeInputs struct {
	MachineID      string
	Reason         string
	IdempotencyKey string
	PreviewDigest  string
	PreviewOnly    bool
}

func runEnrollTokenRevokeCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("enroll-token revoke", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub enroll-token revoke [--reason REASON] [--preview] [--hub-url URL | --db PATH] <machine-id>")
		fmt.Fprintln(errOut, "  Effect: revoke pending enrollment ticket; machine roster, denominator, and agent credential remain unchanged.")
		fmt.Fprintln(errOut, "  transport: HTTP; --db is used for stopped Hub.")
		fmt.Fprintln(errOut, "  retry: reuse --idempotency-key and --preview-digest as a pair.")
		fs.PrintDefaults()
	}
	dbPath := fs.String("db", "", "existing SQLite file location for stopped-service direct DB break-glass")
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL (auto-discovered if omitted)")
	reason := fs.String("reason", "", "revocation reason (recorded in audit, optional)")
	previewOnly := fs.Bool("preview", false, "display ticket status and impact only without revoking")
	idempotencyKey := fs.String("idempotency-key", "", "original request key; must be reused with --preview-digest")
	previewDigest := fs.String("preview-digest", "", "original preview digest; must be reused with --idempotency-key")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("enroll-token revoke: exactly one machine-id must be specified")
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("enroll-token revoke: --hub-url (HTTP mode) and --db (direct mode) cannot both be specified explicitly")
	}
	if seen["hub-url"] && strings.TrimSpace(*hubURL) == "" {
		return errors.New("enroll-token revoke: --hub-url cannot be empty")
	}
	if seen["db"] && strings.TrimSpace(*dbPath) == "" {
		return errors.New("enroll-token revoke: --db cannot be empty")
	}
	if seen["idempotency-key"] && strings.TrimSpace(*idempotencyKey) == "" {
		return errors.New("enroll-token revoke: --idempotency-key cannot be empty")
	}
	if seen["preview-digest"] && strings.TrimSpace(*previewDigest) == "" {
		return errors.New("enroll-token revoke: --preview-digest cannot be empty")
	}
	if seen["idempotency-key"] != seen["preview-digest"] {
		return errors.New("enroll-token revoke: --idempotency-key and --preview-digest must be provided together; omit both to create new request")
	}
	if *previewOnly && (seen["idempotency-key"] || seen["preview-digest"] || seen["reason"]) {
		return errors.New("enroll-token revoke: --preview does not accept --idempotency-key, --preview-digest, or --reason; it does not create state")
	}
	machineID := strings.TrimSpace(fs.Arg(0))
	if machineID == "" || machineID != fs.Arg(0) {
		return errors.New("enroll-token revoke: machine-id cannot be empty or have leading/trailing whitespace")
	}
	inputs := enrollTokenRevokeInputs{
		MachineID: machineID, Reason: *reason, PreviewOnly: *previewOnly,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey), PreviewDigest: strings.TrimSpace(*previewDigest),
	}

	if seen["db"] {
		return withDirectOperatorStore(ctx, "enroll-token revoke", *dbPath, deps, func(st *store.Store) error {
			return runEnrollTokenRevokeDirect(st, inputs, out, errOut)
		})
	}

	selectedURL := strings.TrimSpace(*hubURL)
	mode := "explicit HTTP operator API"
	if !seen["hub-url"] {
		mode = "discovered HTTP operator API"
		if deps.discoverHubURL == nil {
			return errors.New("enroll-token revoke: Hub discovery not initialized")
		}
		var err error
		selectedURL, err = deps.discoverHubURL()
		if err != nil {
			return fmt.Errorf("enroll-token revoke: unable to discover Hub: %w", err)
		}
	}
	if deps.newOperatorClient == nil {
		return fmt.Errorf("enroll-token revoke: %s client not initialized", mode)
	}
	client, err := deps.newOperatorClient(selectedURL)
	if err != nil {
		return fmt.Errorf("enroll-token revoke: failed to configure %s: %w", mode, err)
	}
	return runEnrollTokenRevokeHTTP(ctx, client, inputs, out, errOut)
}

func runEnrollTokenRevokeHTTP(ctx context.Context, client *operatorclient.Client,
	inputs enrollTokenRevokeInputs, out, errOut io.Writer,
) error {
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := client.PreviewEnrollmentTokenRevocation(ctx, inputs.MachineID)
		if err != nil {
			return fmt.Errorf("enroll-token revoke preview (HTTP operator API) failed: %w", err)
		}
		if inputs.PreviewOnly {
			return writeEnrollTokenRevokePreview(out, preview.MachineID, preview.DisplayName,
				preview.TokenExpiresAt, preview.TokenExpired, preview.PreviewDigest)
		}
		if err := writeEnrollTokenRevokePreview(errOut, preview.MachineID, preview.DisplayName,
			preview.TokenExpiresAt, preview.TokenExpired, preview.PreviewDigest); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := enrollTokenRevokeRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("enroll-token revoke: %w", err)
	}
	result, err := client.RevokeEnrollmentToken(ctx, inputs.MachineID, key,
		operatorclient.EnrollmentTokenRevocationRequest{PreviewDigest: digest, Reason: inputs.Reason})
	if err != nil {
		return fmt.Errorf("enroll-token revoke (HTTP operator API; idempotency-key=%q preview-digest=%q%s) failed: %w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return writeEnrollTokenRevokeReceipt(out, result.MachineID, result.DisplayName,
		result.TokenWasExpired, result.RevokedAt, result.Replayed, key, digest)
}

func runEnrollTokenRevokeDirect(st *store.Store, inputs enrollTokenRevokeInputs, out, errOut io.Writer) error {
	service := operator.New(st)
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := service.PreviewEnrollTokenRevocation(operator.EnrollTokenRevocationPreviewRequest{
			MachineID: inputs.MachineID,
		})
		if err != nil {
			return fmt.Errorf("enroll-token revoke preview (direct DB operator service) failed: %w", err)
		}
		if inputs.PreviewOnly {
			return writeEnrollTokenRevokePreview(out, preview.MachineID, preview.DisplayName,
				preview.TokenExpiresAt, preview.TokenExpired, preview.PreviewDigest)
		}
		if err := writeEnrollTokenRevokePreview(errOut, preview.MachineID, preview.DisplayName,
			preview.TokenExpiresAt, preview.TokenExpired, preview.PreviewDigest); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := enrollTokenRevokeRequestKey(inputs.IdempotencyKey)
	if err != nil {
		return fmt.Errorf("enroll-token revoke: %w", err)
	}
	result, err := service.RevokeEnrollToken(operator.EnrollTokenRevocationRequest{
		MachineID: inputs.MachineID, PreviewDigest: digest, Reason: inputs.Reason,
		IdempotencyKey: key,
		Actor: operator.Actor{
			SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
			UserAgent: "clawctl-hub enroll-token revoke", SourceKind: operator.SourceKindDirectDBCLI,
		},
	})
	if err != nil {
		return fmt.Errorf("enroll-token revoke (direct DB operator service; idempotency-key=%q preview-digest=%q%s) failed: %w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return writeEnrollTokenRevokeReceipt(out, result.MachineID, result.DisplayName,
		result.TokenWasExpired, result.RevokedAt, result.Replayed, key, digest)
}

func enrollTokenRevokeRequestKey(supplied string) (string, error) {
	if supplied != "" {
		return supplied, nil
	}
	return operator.NewIdempotencyKey("cli-enroll-token-revoke")
}

func writeEnrollTokenRevokePreview(w io.Writer, machineID, displayName string,
	expiresAt time.Time, expired bool, digest string,
) error {
	_, err := fmt.Fprintf(w,
		"preview: pending enrollment ticket for %s (machine_id %s) expires at %s (expired=%t); revocation preserves roster, denominator delta 0, active agent credential unaffected; preview-digest=%s\n",
		displayName, machineID, expiresAt.UTC().Format(time.RFC3339), expired, digest)
	return err
}

func writeEnrollTokenRevokeReceipt(w io.Writer, machineID, displayName string,
	wasExpired bool, revokedAt time.Time, replayed bool, key, digest string,
) error {
	state := "revoked"
	if replayed {
		state = "replayed"
	}
	_, err := fmt.Fprintf(w,
		"%s: pending enrollment ticket for %s (machine_id %s) revoked (was_expired=%t, revoked_at=%s); roster preserved, denominator delta 0, active agent credential unaffected; idempotency-key=%s preview-digest=%s\n",
		state, displayName, machineID, wasExpired, revokedAt.UTC().Format(time.RFC3339), key, digest)
	return err
}
