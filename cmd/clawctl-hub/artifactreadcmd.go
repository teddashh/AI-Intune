package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func runArtifactReadCommand(ctx context.Context, argv []string, out, errOut io.Writer) error {
	return runArtifactReadCommandWithDeps(ctx, argv, out, errOut, productionMachineCommandDeps())
}

func runArtifactReadCommandWithDeps(ctx context.Context, argv []string, out, errOut io.Writer,
	deps machineCommandDeps,
) error {
	if len(argv) == 0 || (argv[0] != "list" && argv[0] != "show") {
		return errors.New("artifact read: must specify list or show")
	}
	action := argv[0]
	args := argv[1:]
	artifactID := ""
	// Match artifact fetch/deployment show: accept the sole identity before or
	// after flags without making the rest of the grammar ambiguous.
	if action == "show" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		artifactID, args = args[0], args[1:]
	}
	if err := rejectDuplicateArtifactReadFlags(args); err != nil {
		return err
	}

	fs := flag.NewFlagSet("artifact "+action, flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintln(errOut, "Usage: clawctl-hub artifact list [--status STATUS] [--version VERSION] [--limit N] [--cursor CURSOR] [--json] [--hub-url URL | --db PATH]")
		fmt.Fprintln(errOut, "       clawctl-hub artifact show [--json] [--hub-url URL | --db PATH] <artifact-id>")
		fmt.Fprintln(errOut, "  list verifies metadata; show verifies SHA-256.")
		fs.PrintDefaults()
	}
	hubURL := fs.String("hub-url", "", "HTTP operator API base URL (auto-discovered when omitted)")
	dbPath := fs.String("db", "", "path to existing SQLite file for stopped-service direct DB break-glass")
	status := fs.String("status", "", "filter by available_unverified, unavailable, or invalid")
	version := fs.String("version", "", "filter by exact artifact version")
	limit := fs.Int("limit", operator.DefaultArtifactReadLimit, "maximum items per page (1..100)")
	cursor := fs.String("cursor", "", "opaque next cursor returned from previous page")
	jsonOutput := fs.Bool("json", false, "output stable operator JSON DTO")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if seen["hub-url"] && seen["db"] {
		return errors.New("artifact read: --hub-url (HTTP mode) and --db (direct mode) cannot be specified together")
	}
	if seen["hub-url"] {
		if err := validateArtifactReadCLIValue("hub-url", *hubURL, 4096); err != nil {
			return err
		}
	}
	if seen["db"] {
		if err := validateArtifactReadCLIValue("db", *dbPath, 4096); err != nil {
			return err
		}
	}

	if action == "show" {
		for _, name := range []string{"status", "version", "limit", "cursor"} {
			if seen[name] {
				return fmt.Errorf("artifact show: does not accept --%s", name)
			}
		}
		if artifactID == "" {
			if fs.NArg() != 1 {
				return errors.New("artifact show: exactly one canonical artifact-id must be provided")
			}
			artifactID = fs.Arg(0)
		} else if fs.NArg() != 0 {
			return errors.New("artifact show: only one canonical artifact-id is accepted")
		}
		if !validArtifactReadCLIID(artifactID) {
			return errors.New("artifact show: artifact-id must be 64 lowercase hex characters, or invalid- followed by 64 lowercase hex characters")
		}
		if seen["db"] {
			return runArtifactShowDirect(ctx, artifactID, *dbPath, *jsonOutput, out, deps)
		}
		client, err := artifactReadHTTPClient(*hubURL, seen["hub-url"], deps)
		if err != nil {
			return err
		}
		result, err := client.Artifact(ctx, artifactID)
		if err != nil {
			return fmt.Errorf("failed to read artifact detail (HTTP operator API): %w", err)
		}
		return writeArtifactDetail(out, result, *jsonOutput, "HTTP operator API")
	}

	if fs.NArg() != 0 {
		return fmt.Errorf("artifact list: positional arguments are not accepted: %q", strings.Join(fs.Args(), " "))
	}
	readStatus := operator.ArtifactReadStatus(*status)
	if seen["status"] {
		switch readStatus {
		case operator.ArtifactAvailableUnverified, operator.ArtifactUnavailable, operator.ArtifactInvalid:
		default:
			return errors.New("artifact list: --status only accepts available_unverified, unavailable, or invalid")
		}
	}
	if seen["version"] {
		if err := validateArtifactReadCLIValue("version", *version, 128); err != nil ||
			*version == "." || *version == ".." || strings.Contains(*version, "/") {
			return errors.New("artifact list: --version cannot be empty, contain leading or trailing whitespace, control characters, dot segments, or slashes, and must not exceed 128 bytes")
		}
	}
	if *limit < 1 || *limit > operator.MaxArtifactReadLimit {
		return fmt.Errorf("artifact list: --limit must be between 1 and %d", operator.MaxArtifactReadLimit)
	}
	if seen["cursor"] {
		if err := validateArtifactReadCLIValue("cursor", *cursor, 2048); err != nil {
			return err
		}
	}
	request := operator.ArtifactListRequest{
		Status: readStatus, Version: *version, Limit: *limit, Cursor: *cursor,
	}
	if seen["db"] {
		return runArtifactListDirect(ctx, request, *dbPath, *jsonOutput, out, deps)
	}
	client, err := artifactReadHTTPClient(*hubURL, seen["hub-url"], deps)
	if err != nil {
		return err
	}
	result, err := client.Artifacts(ctx, request)
	if err != nil {
		return fmt.Errorf("failed to read artifact list (HTTP operator API): %w", err)
	}
	return writeArtifactList(out, result, *jsonOutput, "HTTP operator API")
}

func rejectDuplicateArtifactReadFlags(args []string) error {
	needsValue := map[string]bool{
		"hub-url": true, "db": true, "status": true, "version": true,
		"limit": true, "cursor": true, "json": false,
	}
	seen := make(map[string]bool, len(needsValue))
	consumeValue := false
	for _, arg := range args {
		if consumeValue {
			consumeValue = false
			continue
		}
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			break
		}
		nameAndValue := strings.TrimLeft(arg, "-")
		name, _, hasEquals := strings.Cut(nameAndValue, "=")
		needsNext, known := needsValue[name]
		if !known {
			continue
		}
		if seen[name] {
			return fmt.Errorf("artifact read: --%s cannot be repeated", name)
		}
		seen[name] = true
		consumeValue = needsNext && !hasEquals
	}
	return nil
}

func validateArtifactReadCLIValue(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("artifact read: --%s cannot be empty, too long, non-UTF-8, or contain leading or trailing whitespace", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("artifact read: --%s cannot contain control or invisible formatting characters", name)
		}
	}
	return nil
}

func validArtifactReadCLIID(value string) bool {
	if artifact.ValidSHA256Hex(value) {
		return true
	}
	digest, ok := strings.CutPrefix(value, "invalid-")
	return ok && artifact.ValidSHA256Hex(digest)
}

func artifactReadHTTPClient(explicitURL string, explicit bool, deps machineCommandDeps) (*operatorclient.Client, error) {
	if explicit {
		if deps.newOperatorClient == nil {
			return nil, errors.New("artifact read: operator HTTP client not initialized")
		}
		client, err := deps.newOperatorClient(explicitURL)
		if err != nil {
			return nil, fmt.Errorf("artifact read: failed to create HTTP operator client: %w", err)
		}
		return client, nil
	}
	if deps.discoverHubURL == nil {
		return nil, errors.New("artifact read: Hub discovery not initialized")
	}
	discovered, err := deps.discoverHubURL()
	if err != nil {
		return nil, fmt.Errorf("artifact read: unable to discover Hub: %w", err)
	}
	if deps.newOperatorClient == nil {
		return nil, errors.New("artifact read: operator HTTP client not initialized")
	}
	client, err := deps.newOperatorClient(discovered)
	if err != nil {
		return nil, fmt.Errorf("artifact read: failed to create discovered HTTP operator client: %w", err)
	}
	return client, nil
}

func runArtifactListDirect(ctx context.Context, request operator.ArtifactListRequest, dbPath string,
	jsonOutput bool, out io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "artifact list", dbPath, deps, func(st *store.Store) error {
		result, err := operator.NewWithArtifacts(st, artifactsDirFor(dbPath)).ListArtifacts(request, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("failed to read artifact list (direct DB operator service): %w", err)
		}
		return writeArtifactList(out, result, jsonOutput, "direct DB operator service")
	})
}

func runArtifactShowDirect(ctx context.Context, artifactID, dbPath string, jsonOutput bool,
	out io.Writer, deps machineCommandDeps,
) error {
	return withDirectOperatorStore(ctx, "artifact show", dbPath, deps, func(st *store.Store) error {
		result, err := operator.NewWithArtifacts(st, artifactsDirFor(dbPath)).ArtifactDetail(ctx, artifactID, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("failed to read artifact detail (direct DB operator service): %w", err)
		}
		return writeArtifactDetail(out, result, jsonOutput, "direct DB operator service")
	})
}

func writeArtifactList(out io.Writer, result operator.ArtifactListResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeArtifactReadJSON(out, result)
	}
	if _, err := fmt.Fprintf(out, "%s; %s consistency; Hub evaluation time %s; %d matched.\n",
		source, terminalSafe(result.Consistency), result.EvaluatedAt.UTC().Format(time.RFC3339Nano), result.Total); err != nil {
		return err
	}
	if len(result.Items) == 0 {
		if _, err := fmt.Fprintln(out, "no artifacts matched the filter"); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "ARTIFACT_ID\tVERSION\tSTATUS\tSIZE\tFETCHED_AT\tREFERENCES\tISSUE"); err != nil {
			return err
		}
		for _, item := range result.Items {
			if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%s\n",
				terminalSafe(item.ArtifactID), artifactReadText(item.Version), terminalSafe(string(item.Status)),
				artifactReadSize(item.SizeBytes), artifactReadTime(item.FetchedAt),
				item.ActiveDeploymentReferences, item.DeploymentReferences, artifactReadIssue(item.Issue)); err != nil {
				return err
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if result.NextCursor != nil {
		if _, err := fmt.Fprintf(out, "next cursor: %s\n", terminalSafe(*result.NextCursor)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out, "verification: metadata")
	return err
}

func writeArtifactDetail(out io.Writer, result operator.ArtifactDetailResult, jsonOutput bool, source string) error {
	if jsonOutput {
		return writeArtifactReadJSON(out, result)
	}
	item := result.Item
	if _, err := fmt.Fprintf(out,
		"%s; %s consistency; Hub evaluation time %s.\nartifact_id: %s\nstatus: %s\nname: %s\nversion: %s\nsha256: %s\ndigest: %s\nsize_bytes: %s\nengines_node: %s\nfetched_at: %s\nverified_at: %s\ndeployment references: %d total / %d active\nissue: %s\n",
		source, terminalSafe(result.Consistency), result.EvaluatedAt.UTC().Format(time.RFC3339Nano),
		terminalSafe(item.ArtifactID), terminalSafe(string(item.Status)), artifactReadText(item.Name),
		artifactReadText(item.Version), artifactReadText(item.SHA256), artifactReadText(item.Digest),
		artifactReadSize(item.SizeBytes), artifactReadText(item.EnginesNode), artifactReadTime(item.FetchedAt),
		artifactReadTime(item.VerifiedAt), item.DeploymentReferences, item.ActiveDeploymentReferences,
		artifactReadIssue(item.Issue)); err != nil {
		return err
	}
	if item.Status == operator.ArtifactReady {
		_, err := fmt.Fprintln(out, "SHA-256 verification: passed")
		return err
	}
	_, err := fmt.Fprintln(out, "SHA-256 verification: failed")
	return err
}

func writeArtifactReadJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func artifactReadText(value *string) string {
	if value == nil {
		return "unknown"
	}
	return terminalSafe(*value)
}

func artifactReadSize(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatInt(*value, 10)
}

func artifactReadTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func artifactReadIssue(value *operator.ArtifactReadIssue) string {
	if value == nil {
		return "none"
	}
	return terminalSafe(string(*value))
}
