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
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func cmdCatalog(argv []string) {
	if err := runCatalogCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Fatal(terminalSafe(err.Error()))
	}
}

func runCatalogCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runCatalogCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runCatalogCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	usage := func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub catalog package list|add | profile list|publish | assign")
	}
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		usage()
		if len(argv) == 0 {
			return errors.New("catalog: must specify package, profile, or assign")
		}
		return flag.ErrHelp
	}
	switch argv[0] {
	case "package":
		if len(argv) < 2 {
			return errors.New("catalog package: must specify list or add")
		}
		switch argv[1] {
		case "list":
			return runCatalogPackageList(ctx, argv[2:], out, errOut, deps)
		case "add":
			return runCatalogPackageAdd(ctx, argv[2:], out, errOut, deps)
		default:
			return fmt.Errorf("catalog package: unrecognized subcommand %q", argv[1])
		}
	case "profile":
		if len(argv) < 2 {
			return errors.New("catalog profile: must specify list or publish")
		}
		switch argv[1] {
		case "list":
			return runCatalogProfileList(ctx, argv[2:], out, errOut, deps)
		case "publish":
			return runCatalogProfilePublish(ctx, argv[2:], out, errOut, deps)
		default:
			return fmt.Errorf("catalog profile: unrecognized subcommand %q", argv[1])
		}
	case "assign":
		return runCatalogAssign(ctx, argv[1:], out, errOut, deps)
	case "recover":
		return runCatalogRecover(ctx, argv[1:], out, errOut, deps)
	default:
		usage()
		return fmt.Errorf("catalog: unrecognized subcommand %q", argv[0])
	}
}

type catalogTransportFlags struct {
	hubURL *string
	json   *bool
}

func addCatalogTransportFlags(fs *flag.FlagSet) catalogTransportFlags {
	return catalogTransportFlags{
		hubURL: fs.String("hub-url", "", "HTTP operator API base URL (auto-discovered when omitted)"),
		json:   fs.Bool("json", false, "output stable operator JSON DTO"),
	}
}

func catalogHTTPClient(flags catalogTransportFlags, fs *flag.FlagSet,
	deps machineCommandDeps,
) (*operatorclient.Client, string, error) {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "hub-url" {
			explicit = true
		}
	})
	if explicit && (*flags.hubURL == "" || *flags.hubURL != strings.TrimSpace(*flags.hubURL)) {
		return nil, "", errors.New("catalog: --hub-url cannot be empty or contain leading or trailing whitespace")
	}
	client, resolved, err := deploymentHTTPClientResolved(*flags.hubURL, explicit, deps)
	if err != nil {
		return nil, "", fmt.Errorf("catalog: failed to connect to Hub: %w", err)
	}
	return client, resolved, nil
}

func runCatalogPackageList(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("catalog package list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addCatalogTransportFlags(fs)
	packageID := fs.String("package", "", "filter by package ID")
	kind := fs.String("kind", "", "filter by app or runtime")
	limit := fs.Int("limit", operator.DefaultCatalogReadLimit, "items per page (1..100)")
	cursor := fs.String("cursor", "", "opaque next cursor from previous page")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("catalog package list: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	request := operator.CatalogManifestListRequest{
		PackageID: *packageID, Kind: appcatalog.PackageKind(*kind), Limit: *limit, Cursor: *cursor,
	}
	client, _, err := catalogHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	result, err := client.CatalogManifests(ctx, request)
	if err != nil {
		return fmt.Errorf("failed to read Store packages: %w", err)
	}
	if *flags.json {
		return writeCatalogJSON(out, result)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PACKAGE\tKIND\tARTIFACT\tPUBLISHED")
	for _, item := range result.Items {
		fmt.Fprintf(w, "%s@%s\t%s\t%s\t%s\n", item.Manifest.ID, item.Manifest.Version,
			item.Manifest.Kind, item.Manifest.Artifact.SHA256, item.PublishedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return writeCatalogCursor(out, result.NextCursor)
}

func runCatalogPackageAdd(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("catalog package add", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addCatalogTransportFlags(fs)
	artifactSHA := fs.String("artifact", "", "verified artifact SHA-256")
	nodeVersion := fs.String("node-runtime-version", "", "published Node runtime version used by OpenClaw")
	previewOnly := fs.Bool("preview", false, "show exact manifest only")
	confirm := fs.String("confirm", "", "confirm package@version")
	reason := fs.String("reason", "", "publish reason")
	requestKey := fs.String("idempotency-key", "", "reused request key on retry")
	recoveryFile := fs.String("recovery-file", "", "private canonical replay receipt path")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("catalog package add: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if !artifact.ValidSHA256Hex(*artifactSHA) {
		return errors.New("catalog package add: --artifact must be 64 lowercase hex characters")
	}
	client, hubURL, err := catalogHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	preview, err := client.PreviewStandardCatalogManifest(ctx, operator.StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: *artifactSHA, NodeRuntimeVersion: *nodeVersion,
	})
	if err != nil {
		return fmt.Errorf("failed to create Store package preview: %w", err)
	}
	if *previewOnly {
		return writeCatalogPackagePreview(out, preview, *flags.json)
	}
	wanted := preview.Manifest.ID + "@" + preview.Manifest.Version
	if *confirm != wanted {
		return fmt.Errorf("catalog package add: --confirm must be %s", wanted)
	}
	if err := validateCatalogReason(*reason); err != nil {
		return err
	}
	key, err := catalogMutationKey(*requestKey, "cli-catalog-package")
	if err != nil {
		return err
	}
	request := operator.CatalogManifestPublishRequest{
		Manifest: preview.Manifest, ConfirmPackageID: preview.Manifest.ID, ConfirmVersion: preview.Manifest.Version,
		PreviewDigest: preview.PreviewDigest, Reason: *reason,
	}
	receipt := newCatalogPackageRecovery(hubURL, key, request)
	return applyCatalogMutationWithRecovery(receipt, *recoveryFile, errOut, func() (any, error) {
		result, err := client.PublishStandardCatalogManifest(ctx, key, request)
		if err != nil {
			return nil, fmt.Errorf("failed to publish Store package: %w", err)
		}
		return result, nil
	}, func(resultAny any) error {
		result, ok := resultAny.(operator.CatalogManifestPublishResult)
		if !ok {
			return errors.New("unexpected response type for publishing Store package")
		}
		if *flags.json {
			return writeCatalogJSON(out, struct {
				IdempotencyKey string                                        `json:"idempotency_key"`
				Preview        operator.StandardCatalogManifestPreviewResult `json:"preview"`
				Result         operator.CatalogManifestPublishResult         `json:"result"`
			}{key, preview, result})
		}
		_, err := fmt.Fprintf(out, "published %s digest=%s replayed=%t\n", wanted, result.Record.Digest, result.Replayed)
		return err
	})
}

func runCatalogProfileList(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("catalog profile list", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addCatalogTransportFlags(fs)
	profileID := fs.String("profile", "", "filter by profile ID")
	limit := fs.Int("limit", operator.DefaultCatalogReadLimit, "items per page (1..100)")
	cursor := fs.String("cursor", "", "opaque next cursor from previous page")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("catalog profile list: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	client, _, err := catalogHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	result, err := client.MachineProfiles(ctx, operator.MachineProfileListRequest{
		ProfileID: *profileID, Limit: *limit, Cursor: *cursor,
	})
	if err != nil {
		return fmt.Errorf("failed to read profiles: %w", err)
	}
	if *flags.json {
		return writeCatalogJSON(out, result)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROFILE\tPACKAGES\tPUBLISHED")
	for _, item := range result.Items {
		refs := make([]string, 0, len(item.Profile.Packages))
		for _, ref := range item.Profile.Packages {
			refs = append(refs, ref.PackageID+"@"+ref.Version)
		}
		fmt.Fprintf(w, "%s@%d\t%s\t%s\n", item.Profile.ID, item.Profile.Revision,
			strings.Join(refs, ","), item.PublishedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return writeCatalogCursor(out, result.NextCursor)
}

type catalogPackageRefs []string

func (v *catalogPackageRefs) String() string { return strings.Join(*v, ",") }
func (v *catalogPackageRefs) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func runCatalogProfilePublish(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("catalog profile publish", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addCatalogTransportFlags(fs)
	profileIdentity := fs.String("profile", "", "profile@revision")
	var packages catalogPackageRefs
	fs.Var(&packages, "package", "directly selected package@version; repeatable")
	previewOnly := fs.Bool("preview", false, "show resolution preview only")
	confirm := fs.String("confirm", "", "confirm profile@revision")
	reason := fs.String("reason", "", "publish reason")
	requestKey := fs.String("idempotency-key", "", "reused request key on retry")
	recoveryFile := fs.String("recovery-file", "", "private canonical replay receipt path")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("catalog profile publish: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	profileID, revision, err := parseCatalogProfileIdentity(*profileIdentity)
	if err != nil {
		return err
	}
	if len(packages) == 0 {
		return errors.New("catalog profile publish: at least one --package must be provided")
	}
	refs := make([]appcatalog.PackageRef, 0, len(packages))
	seen := make(map[string]bool, len(packages))
	for _, value := range packages {
		packageID, version, parseErr := parseCatalogPackageIdentity(value)
		if parseErr != nil || seen[value] {
			return fmt.Errorf("catalog profile publish: --package %q is invalid or duplicate", value)
		}
		seen[value] = true
		refs = append(refs, appcatalog.PackageRef{PackageID: packageID, Version: version})
	}
	profile := appcatalog.MachineProfile{SchemaVersion: appcatalog.SchemaVersion, ID: profileID, Revision: revision, Packages: refs}
	client, hubURL, err := catalogHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	preview, err := client.PreviewMachineProfile(ctx, operator.MachineProfilePreviewRequest{Profile: profile})
	if err != nil {
		return fmt.Errorf("failed to create profile preview: %w", err)
	}
	if *previewOnly {
		return writeCatalogProfilePreview(out, preview, *flags.json)
	}
	if *confirm != *profileIdentity {
		return fmt.Errorf("catalog profile publish: --confirm must be %s", *profileIdentity)
	}
	if err := validateCatalogReason(*reason); err != nil {
		return err
	}
	key, err := catalogMutationKey(*requestKey, "cli-catalog-profile")
	if err != nil {
		return err
	}
	request := operator.MachineProfilePublishRequest{
		Profile: preview.Profile, ConfirmProfileID: preview.Profile.ID, ConfirmRevision: preview.Profile.Revision,
		PreviewDigest: preview.PreviewDigest, Reason: *reason,
	}
	receipt := newCatalogProfileRecovery(hubURL, key, request)
	return applyCatalogMutationWithRecovery(receipt, *recoveryFile, errOut, func() (any, error) {
		result, err := client.PublishReviewedMachineProfile(ctx, key, request)
		if err != nil {
			return nil, fmt.Errorf("failed to publish profile: %w", err)
		}
		return result, nil
	}, func(resultAny any) error {
		result, ok := resultAny.(operator.MachineProfilePublishResult)
		if !ok {
			return errors.New("unexpected response type for publishing profile")
		}
		if *flags.json {
			return writeCatalogJSON(out, struct {
				IdempotencyKey string                               `json:"idempotency_key"`
				Preview        operator.MachineProfilePreviewResult `json:"preview"`
				Result         operator.MachineProfilePublishResult `json:"result"`
			}{key, preview, result})
		}
		_, err := fmt.Fprintf(out, "published %s digest=%s replayed=%t\n", *profileIdentity, result.Record.Digest, result.Replayed)
		return err
	})
}

func runCatalogAssign(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	fs := flag.NewFlagSet("catalog assign", flag.ContinueOnError)
	fs.SetOutput(errOut)
	flags := addCatalogTransportFlags(fs)
	machineID := fs.String("machine", "", "machine ID")
	profileIdentity := fs.String("profile", "", "profile@revision")
	previewOnly := fs.Bool("preview", false, "show job impact only")
	confirmName := fs.String("confirm-name", "", "confirm machine display name")
	reason := fs.String("reason", "", "assignment reason")
	requestKey := fs.String("idempotency-key", "", "reused request key on retry")
	recoveryFile := fs.String("recovery-file", "", "private canonical replay receipt path")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("catalog assign: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	if err := validateDeploymentReadCLIValue("machine", *machineID, 128); err != nil || strings.Contains(*machineID, "/") {
		return errors.New("catalog assign: --machine must be a canonical machine ID")
	}
	profileID, revision, err := parseCatalogProfileIdentity(*profileIdentity)
	if err != nil {
		return err
	}
	client, hubURL, err := catalogHTTPClient(flags, fs, deps)
	if err != nil {
		return err
	}
	preview, err := client.PreviewMachineProfileAssignment(ctx, operator.MachineProfileAssignmentPreviewRequest{
		MachineID: *machineID, ProfileID: profileID, ProfileRevision: revision,
	})
	if err != nil {
		return fmt.Errorf("failed to create profile assignment preview: %w", err)
	}
	if *previewOnly {
		return writeCatalogAssignmentPreview(out, preview, *flags.json)
	}
	if *confirmName != preview.DisplayName {
		return fmt.Errorf("catalog assign: --confirm-name must be %s", preview.DisplayName)
	}
	if len(preview.Blockers) != 0 {
		return fmt.Errorf("catalog assign: preview blockers=%s", joinAssignmentBlockers(preview.Blockers))
	}
	if err := validateCatalogReason(*reason); err != nil {
		return err
	}
	key, err := catalogMutationKey(*requestKey, "cli-catalog-assign")
	if err != nil {
		return err
	}
	request := operator.MachineProfileAssignmentRequest{
		MachineID: *machineID, ProfileID: profileID, ProfileRevision: revision,
		ConfirmDisplayName: *confirmName, PreviewDigest: preview.PreviewDigest, Reason: *reason,
	}
	receipt := newCatalogAssignmentRecovery(hubURL, key, request)
	return applyCatalogMutationWithRecovery(receipt, *recoveryFile, errOut, func() (any, error) {
		result, err := client.AssignMachineProfile(ctx, key, request)
		if err != nil {
			return nil, fmt.Errorf("failed to assign profile: %w", err)
		}
		return result, nil
	}, func(resultAny any) error {
		result, ok := resultAny.(operator.MachineProfileAssignmentResult)
		if !ok {
			return errors.New("unexpected response type for assigning profile")
		}
		if *flags.json {
			return writeCatalogJSON(out, struct {
				IdempotencyKey string                                         `json:"idempotency_key"`
				Preview        operator.MachineProfileAssignmentPreviewResult `json:"preview"`
				Result         operator.MachineProfileAssignmentResult        `json:"result"`
			}{key, preview, result})
		}
		return writeCatalogAssignmentResult(out, result)
	})
}

func writeCatalogAssignmentResult(out io.Writer, result operator.MachineProfileAssignmentResult) error {
	if _, err := fmt.Fprintf(out, "assigned %s@%d to %s jobs=%d replayed=%t\n", result.ProfileID,
		result.ProfileRevision, result.DisplayName, len(result.Packages), result.Replayed); err != nil {
		return err
	}
	for _, item := range result.Packages {
		if _, err := fmt.Fprintf(out, "job %d %s@%s %s\n", item.Position, item.PackageID, item.PackageVersion, item.JobID); err != nil {
			return err
		}
	}
	return nil
}

func writeCatalogPackagePreview(out io.Writer, result operator.StandardCatalogManifestPreviewResult, jsonOutput bool) error {
	if jsonOutput {
		return writeCatalogJSON(out, result)
	}
	dependencies := "none"
	if len(result.Manifest.Dependencies) > 0 {
		dependencies = result.Manifest.Dependencies[0].PackageID + "@" + result.Manifest.Dependencies[0].Version
	}
	fmt.Fprintf(out, "package %s@%s\nkind %s\nartifact %s\ndependencies %s\nalready_published %t\npreview_digest %s\n",
		result.Manifest.ID, result.Manifest.Version, result.Manifest.Kind, result.Manifest.Artifact.SHA256,
		dependencies, result.AlreadyPublished, result.PreviewDigest)
	return nil
}

func writeCatalogProfilePreview(out io.Writer, result operator.MachineProfilePreviewResult, jsonOutput bool) error {
	if jsonOutput {
		return writeCatalogJSON(out, result)
	}
	refs := make([]string, 0, len(result.Profile.Packages))
	for _, ref := range result.Profile.Packages {
		refs = append(refs, ref.PackageID+"@"+ref.Version)
	}
	fmt.Fprintf(out, "profile %s@%d\npackages %s\nalready_published %t\npreview_digest %s\n",
		result.Profile.ID, result.Profile.Revision, strings.Join(refs, ","), result.AlreadyPublished, result.PreviewDigest)
	return nil
}

func writeCatalogAssignmentPreview(out io.Writer, result operator.MachineProfileAssignmentPreviewResult,
	jsonOutput bool,
) error {
	if jsonOutput {
		return writeCatalogJSON(out, result)
	}
	fmt.Fprintf(out, "machine %s (%s)\nprofile %s@%d\ntarget %s/%s\ncreate_jobs %d\nblockers %s\npreview_digest %s\n",
		result.DisplayName, result.MachineID, result.ProfileID, result.ProfileRevision,
		result.Target.OS, result.Target.Arch, result.CreatesJobs, joinAssignmentBlockers(result.Blockers), result.PreviewDigest)
	for _, item := range result.Packages {
		fmt.Fprintf(out, "package %d %s@%s direct=%t prerequisites=%s\n", item.Position+1,
			item.PackageID, item.PackageVersion, item.Direct, strings.Join(item.PrerequisitePackages, ","))
	}
	return nil
}

func writeCatalogCursor(out io.Writer, cursor *string) error {
	if cursor != nil {
		_, err := fmt.Fprintf(out, "next_cursor %s\n", *cursor)
		return err
	}
	return nil
}

func writeCatalogJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func parseCatalogPackageIdentity(value string) (string, string, error) {
	id, version, ok := strings.Cut(value, "@")
	if !ok || strings.Contains(version, "@") || validateDeploymentReadCLIValue("package", id, 128) != nil ||
		validateDeploymentReadCLIValue("version", version, 128) != nil {
		return "", "", errors.New("package identity must be canonical package@version")
	}
	return id, version, nil
}

func parseCatalogProfileIdentity(value string) (string, int64, error) {
	id, revisionText, ok := strings.Cut(value, "@")
	if !ok || strings.Contains(revisionText, "@") || validateDeploymentReadCLIValue("profile", id, 128) != nil {
		return "", 0, errors.New("catalog profile: --profile must be profile@positive-revision")
	}
	revision, err := strconv.ParseInt(revisionText, 10, 64)
	if err != nil || revision <= 0 || strconv.FormatInt(revision, 10) != revisionText {
		return "", 0, errors.New("catalog profile: --profile must be profile@positive-revision")
	}
	return id, revision, nil
}

func validateCatalogReason(value string) error {
	if validateDeploymentReadCLIValue("reason", value, 500) != nil {
		return errors.New("catalog: --reason is required, maximum 500 bytes, and cannot contain control characters")
	}
	return nil
}

func catalogMutationKey(supplied, prefix string) (string, error) {
	if supplied != "" {
		if err := validateDeploymentIdempotencyKey(supplied); err != nil {
			return "", fmt.Errorf("catalog: invalid --idempotency-key: %w", err)
		}
		return supplied, nil
	}
	return operator.NewIdempotencyKey(prefix)
}

func joinAssignmentBlockers(blockers []store.OperatorMachineProfileAssignmentBlocker) string {
	if len(blockers) == 0 {
		return "none"
	}
	items := make([]string, len(blockers))
	for index, blocker := range blockers {
		items[index] = string(blocker)
	}
	return strings.Join(items, ",")
}
