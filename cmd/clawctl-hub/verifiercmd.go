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

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func cmdVerifier(argv []string) {
	if err := runVerifierCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil &&
		!errors.Is(err, flag.ErrHelp) {
		log.Fatal(terminalSafe(err.Error()))
	}
}

func runVerifierCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runVerifierCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runVerifierCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	if len(argv) == 0 {
		writeVerifierUsage(errOut)
		return errors.New("verifier: must specify list, show, register, revoke, or assign")
	}
	switch argv[0] {
	case "list":
		return runVerifierList(ctx, argv[1:], out, errOut, deps)
	case "show":
		return runVerifierShow(ctx, argv[1:], out, errOut, deps)
	case "register":
		return runVerifierRegister(ctx, argv[1:], out, errOut, deps)
	case "revoke":
		return runVerifierRevoke(ctx, argv[1:], out, errOut, deps)
	case "assign":
		return runVerifierAssign(ctx, argv[1:], out, errOut, deps)
	default:
		writeVerifierUsage(errOut)
		return fmt.Errorf("verifier: unknown subcommand %q", terminalSafe(argv[0]))
	}
}

func writeVerifierUsage(errOut io.Writer) {
	fmt.Fprintln(errOut, "Usage: clawctl-hub verifier list [--json] [--hub-url URL | --db PATH]")
	fmt.Fprintln(errOut, "       clawctl-hub verifier show [--json] [--hub-url URL | --db PATH] <verifier-id>")
	fmt.Fprintln(errOut, "       clawctl-hub verifier register --kind KIND --name NAME --failure-domain DOMAIN (--preview | --reason REASON) [--hub-url URL | --db PATH]")
	fmt.Fprintln(errOut, "       clawctl-hub verifier revoke (--preview | --reason REASON --confirm-name NAME) [--hub-url URL | --db PATH] <verifier-id>")
	fmt.Fprintln(errOut, "       clawctl-hub verifier assign --job JOB-ID (--preview | --reason REASON --confirm-name NAME) [--hub-url URL | --db PATH] <verifier-id>")
	fmt.Fprintln(errOut, "  kind: fleet_peer_agent, hub_prober, external_job_runner")
	fmt.Fprintln(errOut, "  register credentials are output only once upon creation; replay only returns a receipt")
	fmt.Fprintln(errOut, "  assign specifies a job; the verifier only receives jobs assigned to it, delivered only after the job finishes")
	fmt.Fprintln(errOut, "  transport: HTTP; --db is for stopped Hub")
}

// verifierTransport resolves the one transport both sides of every subcommand
// share. --db is the stopped-Hub break-glass, and it is never combined with an
// explicit URL.
type verifierTransport struct {
	dbPath string
	hubURL string
	seen   map[string]bool
}

func verifierFlagSet(name string, errOut io.Writer) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet("verifier "+name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		writeVerifierUsage(errOut)
		fs.PrintDefaults()
	}
	dbPath := fs.String("db", "", "path to existing SQLite file for stopped-service direct DB break-glass")
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL (auto-discovered when omitted)")
	return fs, dbPath, hubURL
}

func verifierTransportFrom(name string, fs *flag.FlagSet, dbPath, hubURL *string) (verifierTransport, error) {
	seen := map[string]bool{}
	fs.Visit(func(item *flag.Flag) { seen[item.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return verifierTransport{}, fmt.Errorf("verifier %s: --hub-url (HTTP mode) and --db (direct mode) cannot both be specified", name)
	}
	if (seen["hub-url"] && strings.TrimSpace(*hubURL) == "") ||
		(seen["db"] && strings.TrimSpace(*dbPath) == "") {
		return verifierTransport{}, fmt.Errorf("verifier %s: specified --hub-url / --db cannot be empty", name)
	}
	return verifierTransport{dbPath: *dbPath, hubURL: *hubURL, seen: seen}, nil
}

func verifierHTTPClient(transport verifierTransport, deps machineCommandDeps) (*operatorclient.Client, error) {
	if transport.seen["hub-url"] {
		if deps.newOperatorClient == nil {
			return nil, errors.New("verifier: operator HTTP client not initialized")
		}
		client, err := deps.newOperatorClient(transport.hubURL)
		if err != nil {
			return nil, fmt.Errorf("verifier: failed to create HTTP operator client: %w", err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, errors.New("verifier: Hub discovery not initialized")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("verifier: failed to discover Hub: %w", err)
	}
	if deps.newOperatorClient == nil {
		return nil, errors.New("verifier: operator HTTP client not initialized")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("verifier: failed to create HTTP operator client: %w", err)
	}
	return client, nil
}

func validateVerifierCLIValue(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes {
		return fmt.Errorf("verifier: --%s cannot be empty, too long, or contain leading/trailing whitespace", name)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("verifier: --%s cannot contain control characters", name)
		}
	}
	return nil
}

func validateVerifierIDArgument(command, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 ||
		strings.Contains(value, "/") || value == "." || value == ".." {
		return fmt.Errorf("verifier %s: verifier-id cannot be empty, contain leading/trailing whitespace, dot segments, or slashes, and must not exceed 256 bytes", command)
	}
	return nil
}

func runVerifierList(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs, dbPath, hubURL := verifierFlagSet("list", errOut)
	jsonOutput := fs.Bool("json", false, "output stable operator JSON DTO")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("verifier list: positional arguments not accepted: %q", terminalSafe(strings.Join(fs.Args(), " ")))
	}
	transport, err := verifierTransportFrom("list", fs, dbPath, hubURL)
	if err != nil {
		return err
	}
	if transport.seen["db"] {
		return withDirectOperatorStore(ctx, "verifier list", transport.dbPath, deps, func(st *store.Store) error {
			result, err := operator.New(st).Verifiers()
			if err != nil {
				return fmt.Errorf("failed to read verifier list (direct DB operator service): %w", err)
			}
			return writeVerifierList(out, result, *jsonOutput, "direct DB operator service")
		})
	}
	client, err := verifierHTTPClient(transport, deps)
	if err != nil {
		return err
	}
	result, err := client.Verifiers(ctx)
	if err != nil {
		return fmt.Errorf("failed to read verifier list (HTTP operator API): %w", err)
	}
	return writeVerifierList(out, result, *jsonOutput, "HTTP operator API")
}

func runVerifierShow(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs, dbPath, hubURL := verifierFlagSet("show", errOut)
	jsonOutput := fs.Bool("json", false, "output stable operator JSON DTO")
	parseArgs := argv
	if len(parseArgs) > 0 && !strings.HasPrefix(parseArgs[0], "-") {
		parseArgs = append(append([]string(nil), parseArgs[1:]...), parseArgs[0])
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("verifier show: must provide a verifier-id")
	}
	verifierID := fs.Arg(0)
	if err := validateVerifierIDArgument("show", verifierID); err != nil {
		return err
	}
	transport, err := verifierTransportFrom("show", fs, dbPath, hubURL)
	if err != nil {
		return err
	}
	if transport.seen["db"] {
		return withDirectOperatorStore(ctx, "verifier show", transport.dbPath, deps, func(st *store.Store) error {
			result, err := operator.New(st).VerifierDetail(verifierID)
			if err != nil {
				return fmt.Errorf("failed to read verifier detail (direct DB operator service): %w", err)
			}
			return writeVerifierDetail(out, result, *jsonOutput, "direct DB operator service")
		})
	}
	client, err := verifierHTTPClient(transport, deps)
	if err != nil {
		return err
	}
	result, err := client.Verifier(ctx, verifierID)
	if err != nil {
		return fmt.Errorf("failed to read verifier detail (HTTP operator API): %w", err)
	}
	return writeVerifierDetail(out, result, *jsonOutput, "HTTP operator API")
}

type verifierRegisterInputs struct {
	Kind           string
	DisplayName    string
	FailureDomain  string
	HubHost        string
	Reason         string
	Preview        bool
	JSON           bool
	IdempotencyKey string
	PreviewDigest  string
}

func runVerifierRegister(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs, dbPath, hubURL := verifierFlagSet("register", errOut)
	kind := fs.String("kind", "", "fleet_peer_agent, hub_prober, or external_job_runner")
	name := fs.String("name", "", "verifier display_name; cannot be reused after revocation")
	failureDomain := fs.String("failure-domain", "", "failure domain of the verifier; cannot equal the machine it verifies")
	hubHost := fs.String("hub-host", "", "hostname of the Hub itself when registering hub_prober in direct DB mode")
	reason := fs.String("reason", "", "registration reason (required on apply, recorded in audit)")
	preview := fs.Bool("preview", false, "show policy and impact only, do not create verifier")
	jsonOutput := fs.Bool("json", false, "output stable operator JSON DTO (preview only)")
	idempotencyKey := fs.String("idempotency-key", "", "original request key for ambiguous response retry")
	previewDigest := fs.String("preview-digest", "", "original preview digest for ambiguous response retry")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("verifier register: positional arguments not accepted: %q", terminalSafe(strings.Join(fs.Args(), " ")))
	}
	transport, err := verifierTransportFrom("register", fs, dbPath, hubURL)
	if err != nil {
		return err
	}
	seen := transport.seen
	for _, field := range []struct{ name, value string }{
		{"kind", *kind}, {"name", *name}, {"failure-domain", *failureDomain},
	} {
		if err := validateVerifierCLIValue(field.name, field.value, 256); err != nil {
			return err
		}
	}
	if seen["idempotency-key"] != seen["preview-digest"] {
		return errors.New("verifier register: --idempotency-key and --preview-digest must be provided together; omit both to create a new request")
	}
	if *preview {
		for _, name := range []string{"idempotency-key", "preview-digest", "reason"} {
			if seen[name] {
				return fmt.Errorf("verifier register: --preview does not accept --%s; it creates no state", name)
			}
		}
	} else {
		if err := validateVerifierCLIValue("reason", *reason, 512); err != nil {
			return err
		}
		if *jsonOutput {
			return errors.New("verifier register: --json is only for --preview; credentials on creation are output only once as plain text")
		}
	}
	if seen["hub-host"] {
		if !seen["db"] {
			return errors.New("verifier register: --hub-host is only for --db; HTTP mode resolves it from Hub itself")
		}
		if err := validateVerifierCLIValue("hub-host", *hubHost, 256); err != nil {
			return err
		}
	} else if seen["db"] && *kind == store.VerifierKindHubProber {
		return errors.New("verifier register: --hub-host is required when registering hub_prober in --db mode")
	}
	inputs := verifierRegisterInputs{
		Kind: *kind, DisplayName: *name, FailureDomain: *failureDomain, HubHost: *hubHost,
		Reason: *reason, Preview: *preview, JSON: *jsonOutput,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey), PreviewDigest: strings.TrimSpace(*previewDigest),
	}
	if seen["db"] {
		return withDirectOperatorStore(ctx, "verifier register", transport.dbPath, deps, func(st *store.Store) error {
			return runVerifierRegisterDirect(st, inputs, out, errOut)
		})
	}
	client, err := verifierHTTPClient(transport, deps)
	if err != nil {
		return err
	}
	return runVerifierRegisterHTTP(ctx, client, inputs, out, errOut)
}

func runVerifierRegisterHTTP(ctx context.Context, client *operatorclient.Client,
	inputs verifierRegisterInputs, out, errOut io.Writer,
) error {
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := client.PreviewVerifier(ctx, operatorclient.VerifierPreviewRequest{
			Kind: inputs.Kind, DisplayName: inputs.DisplayName, FailureDomain: inputs.FailureDomain,
		})
		if err != nil {
			return fmt.Errorf("verifier register preview (HTTP operator API) failed: %w", err)
		}
		if inputs.Preview {
			return writeVerifierPreview(out, preview, inputs.JSON, "HTTP operator API")
		}
		if err := writeVerifierPreview(errOut, preview, false, "HTTP operator API"); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := verifierRequestKey(inputs.IdempotencyKey, "cli-verifier-register")
	if err != nil {
		return err
	}
	result, err := client.RegisterVerifier(ctx, key, operatorclient.VerifierCreateRequest{
		Kind: inputs.Kind, DisplayName: inputs.DisplayName, FailureDomain: inputs.FailureDomain,
		PreviewDigest: digest, Reason: inputs.Reason,
	})
	if err != nil {
		return fmt.Errorf("verifier register (HTTP operator API; idempotency-key=%q preview-digest=%q%s) failed: %w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return finishVerifierRegistration(result.VerifierID, result.Kind, result.DisplayName,
		result.FailureDomain, result.Credential, result.SecretAvailable, result.Replayed,
		result.RecoveryRequired, result.RecoveryAction, key, digest, out, errOut)
}

func runVerifierRegisterDirect(st *store.Store, inputs verifierRegisterInputs, out, errOut io.Writer) error {
	service := operator.New(st)
	digest := inputs.PreviewDigest
	if digest == "" {
		preview, err := service.PreviewVerifier(operator.VerifierPreviewRequest{
			Kind: inputs.Kind, DisplayName: inputs.DisplayName,
			FailureDomain: inputs.FailureDomain, HubHost: inputs.HubHost,
		})
		if err != nil {
			return fmt.Errorf("verifier register preview (direct DB operator service) failed: %w", err)
		}
		if inputs.Preview {
			return writeVerifierPreview(out, preview, inputs.JSON, "direct DB operator service")
		}
		if err := writeVerifierPreview(errOut, preview, false, "direct DB operator service"); err != nil {
			return err
		}
		digest = preview.PreviewDigest
	}
	key, err := verifierRequestKey(inputs.IdempotencyKey, "cli-verifier-register")
	if err != nil {
		return err
	}
	result, err := service.RegisterVerifier(operator.VerifierCreateRequest{
		Kind: inputs.Kind, DisplayName: inputs.DisplayName, FailureDomain: inputs.FailureDomain,
		HubHost: inputs.HubHost, PreviewDigest: digest, Reason: inputs.Reason, IdempotencyKey: key,
		Actor: operator.Actor{
			SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
			UserAgent: "clawctl-hub verifier register", SourceKind: operator.SourceKindDirectDBCLI,
		},
	})
	if err != nil {
		return fmt.Errorf("verifier register (direct DB operator service; idempotency-key=%q preview-digest=%q%s) failed: %w",
			key, digest, operatorRejectionReplayNote(err), err)
	}
	return finishVerifierRegistration(result.VerifierID, result.Kind, result.DisplayName,
		result.FailureDomain, result.Credential, result.SecretAvailable, result.Replayed,
		result.RecoveryRequired, result.RecoveryAction, key, digest, out, errOut)
}

// finishVerifierRegistration writes the credential to stdout exactly once and
// never repeats it — not in the follow-up instructions, not in an error. The
// only recovery from a lost credential is to revoke this verifier and register
// a new one.
func finishVerifierRegistration(verifierID, kind, displayName, failureDomain, credential string,
	secretAvailable, replayed, recoveryRequired bool, recoveryAction, key, digest string,
	out, errOut io.Writer,
) error {
	if replayed || recoveryRequired || !secretAvailable || credential == "" {
		return fmt.Errorf("verifier register completed; credential cannot be redisplayed; verifier_id=%s; recovery_action=%s; idempotency-key=%q; revoke: clawctl-hub verifier revoke %s",
			terminalSafe(verifierID), terminalSafe(recoveryAction), key, terminalSafe(verifierID))
	}
	if _, err := fmt.Fprintln(out, credential); err != nil {
		return fmt.Errorf("credential delivery failed; verifier_id=%s; idempotency-key=%q; revoke: clawctl-hub verifier revoke %s: %w",
			terminalSafe(verifierID), key, terminalSafe(verifierID), err)
	}
	_, err := fmt.Fprintf(errOut,
		"\n%s registered (verifier_id %s, kind %s, failure_domain %s)\n"+
			"credential is output only this once, used for POST /v1/verifications\n"+
			"its evidence role is %s; deployment gate is determined by executor evidence\n"+
			"revoke: clawctl-hub verifier revoke %s\n"+
			"request key: %s; preview digest: %s\n",
		terminalSafe(displayName), terminalSafe(verifierID), terminalSafe(kind),
		terminalSafe(failureDomain), store.JobVerificationRoleIndependent,
		terminalSafe(verifierID), key, digest)
	return err
}

type verifierRevokeInputs struct {
	VerifierID       string
	ConfirmName      string
	Reason           string
	Preview          bool
	JSON             bool
	IdempotencyKey   string
	PreviewDigest    string
	ExpectedRevision *int64
}

func runVerifierRevoke(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs, dbPath, hubURL := verifierFlagSet("revoke", errOut)
	confirmName := fs.String("confirm-name", "", "verbatim matching display_name required on apply")
	reason := fs.String("reason", "", "revocation reason (required on apply, recorded in audit)")
	preview := fs.Bool("preview", false, "show impact only, do not revoke")
	jsonOutput := fs.Bool("json", false, "output stable operator JSON DTO (preview only)")
	idempotencyKey := fs.String("idempotency-key", "", "original request key for ambiguous response retry")
	previewDigest := fs.String("preview-digest", "", "original preview digest for ambiguous response retry")
	expectedRevision := fs.Int64("expected-revision", 0, "explicit verifier revision; required when retrying receipt")
	parseArgs := argv
	if len(parseArgs) > 0 && !strings.HasPrefix(parseArgs[0], "-") {
		parseArgs = append(append([]string(nil), parseArgs[1:]...), parseArgs[0])
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("verifier revoke: must provide a verifier-id")
	}
	verifierID := fs.Arg(0)
	if err := validateVerifierIDArgument("revoke", verifierID); err != nil {
		return err
	}
	transport, err := verifierTransportFrom("revoke", fs, dbPath, hubURL)
	if err != nil {
		return err
	}
	seen := transport.seen
	if seen["idempotency-key"] != seen["preview-digest"] {
		return errors.New("verifier revoke: --idempotency-key and --preview-digest must be provided together; omit both to create a new request")
	}
	if seen["idempotency-key"] && !seen["expected-revision"] {
		return errors.New("verifier revoke: retry requires original --idempotency-key, --expected-revision, and --preview-digest together")
	}
	if *preview {
		for _, name := range []string{"idempotency-key", "preview-digest", "expected-revision", "reason", "confirm-name"} {
			if seen[name] {
				return fmt.Errorf("verifier revoke: --preview does not accept --%s; it creates no state", name)
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
			return errors.New("verifier revoke: --json is only for --preview")
		}
	}
	if seen["expected-revision"] && *expectedRevision < 1 {
		return errors.New("verifier revoke: --expected-revision must be greater than 0")
	}
	inputs := verifierRevokeInputs{
		VerifierID: verifierID, ConfirmName: *confirmName, Reason: *reason,
		Preview: *preview, JSON: *jsonOutput,
		IdempotencyKey: strings.TrimSpace(*idempotencyKey), PreviewDigest: strings.TrimSpace(*previewDigest),
	}
	if seen["expected-revision"] {
		inputs.ExpectedRevision = expectedRevision
	}
	if seen["db"] {
		return withDirectOperatorStore(ctx, "verifier revoke", transport.dbPath, deps, func(st *store.Store) error {
			return runVerifierRevokeDirect(st, inputs, out, errOut)
		})
	}
	client, err := verifierHTTPClient(transport, deps)
	if err != nil {
		return err
	}
	return runVerifierRevokeHTTP(ctx, client, inputs, out, errOut)
}

func runVerifierRevokeHTTP(ctx context.Context, client *operatorclient.Client,
	inputs verifierRevokeInputs, out, errOut io.Writer,
) error {
	digest, revision := inputs.PreviewDigest, inputs.ExpectedRevision
	if digest == "" {
		preview, err := client.PreviewVerifierRevocation(ctx, inputs.VerifierID)
		if err != nil {
			return fmt.Errorf("verifier revoke preview (HTTP operator API) failed: %w", err)
		}
		if inputs.Preview {
			return writeVerifierRevocationPreview(out, preview, inputs.JSON, "HTTP operator API")
		}
		if err := writeVerifierRevocationPreview(errOut, preview, false, "HTTP operator API"); err != nil {
			return err
		}
		digest = preview.PreviewDigest
		if revision == nil {
			asserted := preview.Revision
			revision = &asserted
		}
	}
	key, err := verifierRequestKey(inputs.IdempotencyKey, "cli-verifier-revoke")
	if err != nil {
		return err
	}
	result, err := client.RevokeVerifier(ctx, inputs.VerifierID, key, operatorclient.VerifierRevocationRequest{
		ExpectedRevision: revision, ConfirmDisplayName: inputs.ConfirmName,
		PreviewDigest: digest, Reason: inputs.Reason,
	})
	if err != nil {
		return fmt.Errorf("verifier revoke (HTTP operator API; idempotency-key=%q expected-revision=%s preview-digest=%q%s) failed: %w",
			key, verifierRevisionText(revision), digest, operatorRejectionReplayNote(err), err)
	}
	return writeVerifierRevocationReceipt(out, result, key, digest)
}

func runVerifierRevokeDirect(st *store.Store, inputs verifierRevokeInputs, out, errOut io.Writer) error {
	service := operator.New(st)
	digest, revision := inputs.PreviewDigest, inputs.ExpectedRevision
	if digest == "" {
		preview, err := service.PreviewVerifierRevocation(operator.VerifierRevocationPreviewRequest{
			VerifierID: inputs.VerifierID,
		})
		if err != nil {
			return fmt.Errorf("verifier revoke preview (direct DB operator service) failed: %w", err)
		}
		if inputs.Preview {
			return writeVerifierRevocationPreview(out, preview, inputs.JSON, "direct DB operator service")
		}
		if err := writeVerifierRevocationPreview(errOut, preview, false, "direct DB operator service"); err != nil {
			return err
		}
		digest = preview.PreviewDigest
		if revision == nil {
			asserted := preview.Revision
			revision = &asserted
		}
	}
	key, err := verifierRequestKey(inputs.IdempotencyKey, "cli-verifier-revoke")
	if err != nil {
		return err
	}
	result, err := service.RevokeVerifier(operator.VerifierRevocationRequest{
		VerifierID: inputs.VerifierID, ExpectedRevision: revision,
		ConfirmDisplayName: inputs.ConfirmName, PreviewDigest: digest, Reason: inputs.Reason,
		IdempotencyKey: key,
		Actor: operator.Actor{
			SourceAddr: "local-cli", WhoUnavailable: "direct-db-cli",
			UserAgent: "clawctl-hub verifier revoke", SourceKind: operator.SourceKindDirectDBCLI,
		},
	})
	if err != nil {
		return fmt.Errorf("verifier revoke (direct DB operator service; idempotency-key=%q expected-revision=%s preview-digest=%q%s) failed: %w",
			key, verifierRevisionText(revision), digest, operatorRejectionReplayNote(err), err)
	}
	return writeVerifierRevocationReceipt(out, result, key, digest)
}

func verifierRequestKey(supplied, prefix string) (string, error) {
	if supplied != "" {
		return supplied, nil
	}
	key, err := operator.NewIdempotencyKey(prefix)
	if err != nil {
		return "", fmt.Errorf("generate verifier request key: %w", err)
	}
	return key, nil
}

func verifierRevisionText(revision *int64) string {
	if revision == nil {
		return "unset"
	}
	return fmt.Sprintf("%d", *revision)
}

func writeVerifierList(out io.Writer, result operator.VerifierListResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeJobJSON(out, result)
	}
	if _, err := fmt.Fprintf(out, "%s; verifiers: %d (%d active / %d revoked); separation_rule: %s\n",
		terminalSafe(source), len(result.Verifiers), result.Active, result.Revoked,
		terminalSafe(result.SeparationRule)); err != nil {
		return err
	}
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table,
		"VERIFIER_ID\tNAME\tKIND\tFAILURE_DOMAIN\tSTATE\tREVISION\tEVIDENCE_ROWS\tLAST_SEEN_AT(Hub)"); err != nil {
		return err
	}
	for _, item := range result.Verifiers {
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			terminalSafe(item.VerifierID), terminalSafe(item.DisplayName), terminalSafe(item.Kind),
			terminalSafe(item.FailureDomain), terminalSafe(item.State), item.Revision,
			item.EvidenceRows, jobReadTime(item.LastSeenAt)); err != nil {
			return err
		}
	}
	return table.Flush()
}

func writeVerifierDetail(out io.Writer, result operator.VerifierDetailResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeJobJSON(out, result)
	}
	item := result.Item
	revoked := "—"
	if item.RevokedAt != nil {
		revoked = item.RevokedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := fmt.Fprintf(out,
		"%s\nverifier: %s (%s); kind: %s; failure_domain: %s\nstate: %s; revision: %d; created_at: %s; revoked_at: %s; last_seen_at(Hub): %s\nevidence_rows: %d; jobs_with_evidence: %d\nseparation_rule: %s; evidence_role: %s; grants_deployment_gate: %t\n",
		terminalSafe(source), terminalSafe(item.DisplayName), terminalSafe(item.VerifierID),
		terminalSafe(item.Kind), terminalSafe(item.FailureDomain), terminalSafe(item.State),
		item.Revision, item.CreatedAt.UTC().Format(time.RFC3339Nano), revoked,
		jobReadTime(item.LastSeenAt), item.EvidenceRows, result.JobsWithEvidence,
		terminalSafe(result.SeparationRule), store.JobVerificationRoleIndependent,
		result.GrantsDeploymentGate)
	return err
}

func writeVerifierPreview(out io.Writer, preview store.OperatorVerifierPreviewResult,
	jsonOutput bool, source string,
) error {
	if jsonOutput {
		return writeJobJSON(out, preview)
	}
	_, err := fmt.Fprintf(out,
		"preview (%s): %s will become a verifier of kind %s, failure_domain %s\nseparation_rule: %s; evidence_role: %s; credential_delivery: %s\nrevocation_keeps_row: %t; grants_deployment_gate: %t\npreview-digest=%s\n",
		terminalSafe(source), terminalSafe(preview.DisplayName), terminalSafe(preview.Kind),
		terminalSafe(preview.FailureDomain), terminalSafe(preview.SeparationRule),
		terminalSafe(preview.EvidenceRole), terminalSafe(preview.CredentialDelivery),
		preview.RevocationKeepsRow, preview.GrantsDeploymentGate, preview.PreviewDigest)
	return err
}

func writeVerifierRevocationPreview(out io.Writer, preview store.OperatorVerifierRevocationPreviewResult,
	jsonOutput bool, source string,
) error {
	if jsonOutput {
		return writeJobJSON(out, preview)
	}
	_, err := fmt.Fprintf(out,
		"preview (%s): revoke %s (verifier_id %s, kind %s, failure_domain %s, revision %d)\nevidence_rows: %d; jobs_losing_only_producer: %d\nregistry_row_retained: %t; evidence_retained: %t; display_name_reusable: %t; grants_deployment_gate: %t\npreview-digest=%s\n",
		terminalSafe(source), terminalSafe(preview.DisplayName), terminalSafe(preview.VerifierID),
		terminalSafe(preview.Kind), terminalSafe(preview.FailureDomain), preview.Revision,
		preview.EvidenceRows, preview.JobsLosingOnlyProducer, preview.RegistryRowRetained,
		preview.EvidenceRetained, preview.DisplayNameReusable, preview.GrantsDeploymentGate,
		preview.PreviewDigest)
	return err
}

func writeVerifierRevocationReceipt(out io.Writer, result store.OperatorVerifierRevocationResult,
	key, digest string,
) error {
	state := "revoked"
	if result.Replayed {
		state = "replayed"
	}
	_, err := fmt.Fprintf(out,
		"%s: %s (verifier_id %s, kind %s, failure_domain %s) revoked at %s; revision %d → %d\nevidence_rows: %d; jobs_losing_only_producer: %d; registry_row_retained: %t; evidence_retained: %t\nidempotency-key=%s preview-digest=%s\n",
		state, terminalSafe(result.DisplayName), terminalSafe(result.VerifierID),
		terminalSafe(result.Kind), terminalSafe(result.FailureDomain),
		result.RevokedAt.UTC().Format(time.RFC3339Nano), result.PreviousRevision, result.Revision,
		result.EvidenceRows, result.JobsLosingOnlyProducer, result.RegistryRowRetained,
		result.EvidenceRetained, key, digest)
	return err
}
