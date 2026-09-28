package operatorclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
)

// A legal 100-item changes page can exceed the general 1 MiB cap: each safe
// item carries two bounded evidence arrays plus before/after typed values.
// Four MiB covers that declared surface while retaining a hard read bound.
const maxChangeResponseBytes int64 = 4 << 20

// Changes reads the bounded, safe operator projection. The legacy Store
// ChangesSince sentences are deliberately not part of this client surface.
func (c *Client) Changes(ctx context.Context, request operator.ChangeListRequest) (operator.ChangeListResult, error) {
	query, normalized, err := encodeChangeListQuery(request)
	if err != nil {
		return operator.ChangeListResult{}, err
	}
	path := "/v1/operator/changes"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.ChangeListResult{}, err
	}
	response, err := c.doRawWithLimit(req, maxChangeResponseBytes, "4 MiB")
	if err != nil {
		return operator.ChangeListResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.ChangeListResult{}, fmt.Errorf(
			"operator client: changes returned unexpected success status HTTP %d", response.status)
	}
	if err := validateChangeReadHeaders(response.header); err != nil {
		return operator.ChangeListResult{}, err
	}
	var result operator.ChangeListResult
	if err := decodeStrictJSONDocument(response.body, "changes", &result); err != nil {
		return operator.ChangeListResult{}, err
	}
	if err := validateChangeListResult(result, normalized); err != nil {
		return operator.ChangeListResult{}, err
	}
	return result, nil
}

// ListChanges is the explicit list spelling retained for callers that mirror
// Service.ListChangesContext. Both names use exactly the same strict path.
func (c *Client) ListChanges(ctx context.Context, request operator.ChangeListRequest) (operator.ChangeListResult, error) {
	return c.Changes(ctx, request)
}

func encodeChangeListQuery(request operator.ChangeListRequest) (url.Values, operator.ChangeListRequest, error) {
	if err := operator.ValidateChangeListRequest(request); err != nil {
		return nil, operator.ChangeListRequest{}, fmt.Errorf("operator client: invalid changes request: %w", err)
	}
	query := make(url.Values)
	for _, field := range []struct {
		name, value string
	}{
		{"machine_id", request.MachineID}, {"subject", request.Subject}, {"cursor", request.Cursor},
	} {
		if field.value != "" {
			query.Set(field.name, field.value)
		}
	}

	selected := make(map[string]bool, len(request.Kinds))
	for _, kind := range request.Kinds {
		selected[kind] = true
	}
	request.Kinds = make([]string, 0, len(selected))
	for _, kind := range operator.ChangeKinds() {
		if selected[kind] {
			request.Kinds = append(request.Kinds, kind)
			query.Add("kind", kind)
		}
	}
	for _, field := range []struct {
		name  string
		value **time.Time
	}{
		{"from", &request.From}, {"to", &request.To},
	} {
		if *field.value == nil {
			continue
		}
		utc := (*field.value).UTC()
		*field.value = &utc
		query.Set(field.name, utc.Format(time.RFC3339))
	}
	explicitLimit := request.Limit != 0
	if request.Limit == 0 {
		request.Limit = operator.DefaultChangeReadLimit
	}
	if explicitLimit {
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	if request.Cursor != "" {
		if _, err := decodeChangeClientCursor(request.Cursor, changeClientFilterDigest(request)); err != nil {
			return nil, operator.ChangeListRequest{}, fmt.Errorf("operator client: invalid changes cursor: %w", err)
		}
	}
	return query, request, nil
}

func validateChangeReadHeaders(header http.Header) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	if replayed, err := responseReplayEvidenceFromHeader(header); err != nil {
		return err
	} else if replayed {
		return errors.New("operator client: changes read cannot be an idempotency replay")
	}
	if values := header.Values("ETag"); len(values) != 0 {
		return errors.New("operator client: composite changes read must not carry an ETag")
	}
	return nil
}

type changeClientPosition struct {
	At          time.Time `json:"at"`
	SourceRank  int       `json:"source_rank"`
	SourceRowID int64     `json:"source_rowid"`
	ChangeID    string    `json:"change_id"`
}

type changeClientCursor struct {
	Version      int                             `json:"v"`
	FilterDigest string                          `json:"filter_digest"`
	EvaluatedAt  time.Time                       `json:"evaluated_at"`
	From         time.Time                       `json:"from"`
	To           time.Time                       `json:"to"`
	Ceilings     operator.ChangeCreationCeilings `json:"ceilings"`
	After        changeClientPosition            `json:"after"`
}

func changeClientFilterDigest(request operator.ChangeListRequest) string {
	if request.Kinds == nil {
		request.Kinds = []string{}
	}
	body := struct {
		Version   int      `json:"v"`
		MachineID string   `json:"machine_id"`
		Kinds     []string `json:"kinds"`
		Subject   string   `json:"subject"`
	}{operator.ChangeReadSchemaVersion, request.MachineID, request.Kinds, request.Subject}
	raw, _ := json.Marshal(body)
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func decodeChangeClientCursor(encoded, filterDigest string) (changeClientCursor, error) {
	if encoded == "" || len(encoded) > 4096 {
		return changeClientCursor{}, errors.New("invalid changes cursor length")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return changeClientCursor{}, errors.New("invalid changes cursor encoding")
	}
	var cursor changeClientCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return changeClientCursor{}, errors.New("invalid changes cursor document")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return changeClientCursor{}, errors.New("invalid trailing changes cursor data")
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(canonical) != encoded ||
		cursor.Version != operator.ChangeReadSchemaVersion || cursor.FilterDigest != filterDigest ||
		validateChangeClientTime(cursor.EvaluatedAt, "cursor evaluated_at", true) != nil ||
		validateChangeClientTime(cursor.From, "cursor from", true) != nil ||
		validateChangeClientTime(cursor.To, "cursor to", true) != nil ||
		!cursor.From.Before(cursor.To) || cursor.To.After(cursor.EvaluatedAt) ||
		cursor.To.Sub(cursor.From) > operator.MaxChangeWindow ||
		!validChangeClientCeilings(cursor.Ceilings) || !validChangeClientPosition(cursor.After) ||
		!cursor.After.At.After(cursor.From) || cursor.After.At.After(cursor.To) {
		return changeClientCursor{}, errors.New("changes cursor does not match request")
	}
	return cursor, nil
}

func validChangeClientPosition(position changeClientPosition) bool {
	return validateChangeClientTime(position.At, "cursor position", true) == nil &&
		position.SourceRank >= 1 && position.SourceRank <= 3 && position.SourceRowID >= 1 &&
		validSHA256Digest(position.ChangeID)
}

func validChangeClientCeilings(value operator.ChangeCreationCeilings) bool {
	return value.Observations >= 0 && value.StateHistory >= 0 && value.Registry >= 0 &&
		value.RegistryLifecycle >= 0 && value.RetentionLog >= 0
}

func validateChangeListResult(result operator.ChangeListResult, request operator.ChangeListRequest) error {
	if result.SchemaVersion != operator.ChangeReadSchemaVersion || result.Consistency != operator.ChangeReadConsistency {
		return fmt.Errorf("operator client: unsupported changes schema_version %d or consistency %q",
			result.SchemaVersion, result.Consistency)
	}
	if err := validateChangeClientTime(result.EvaluatedAt, "evaluated_at", true); err != nil {
		return err
	}
	if err := validateChangeWindow(result.Window, result.EvaluatedAt); err != nil {
		return err
	}
	if !validChangeClientCeilings(result.CreationCeilings) {
		return errors.New("operator client: changes creation ceilings are negative")
	}
	if result.Items == nil || result.KindCounts == nil || result.Coverage.Issues == nil {
		return errors.New("operator client: changes arrays must not be null")
	}
	if result.Total < 0 || result.MatchedTotal != result.Total ||
		len(result.Items) > request.Limit || len(result.Items) > result.MatchedTotal {
		return errors.New("operator client: changes totals or page size are inconsistent")
	}
	if result.Total > 0 && result.CreationCeilings.Observations == 0 &&
		result.CreationCeilings.StateHistory == 0 && result.CreationCeilings.Registry == 0 &&
		result.CreationCeilings.RegistryLifecycle == 0 {
		return errors.New("operator client: nonempty changes result has no source ceiling")
	}
	if err := validateChangeRequestWindow(result, request); err != nil {
		return err
	}
	if err := validateChangeCoverage(result.Coverage, result.EvaluatedAt, request); err != nil {
		return err
	}

	kinds := operator.ChangeKinds()
	if len(result.KindCounts) != len(kinds) {
		return errors.New("operator client: changes kind_counts are not exhaustive")
	}
	kindTotals := make(map[string]int, len(kinds))
	total := 0
	maxInt := int(^uint(0) >> 1)
	for index, kind := range kinds {
		row := result.KindCounts[index]
		if row.Kind != kind || row.Count < 0 || row.Count > maxInt-total {
			return errors.New("operator client: changes kind_counts are not canonical")
		}
		total += row.Count
		kindTotals[kind] = row.Count
	}
	if total != result.Total {
		return errors.New("operator client: changes kind_counts do not sum to total")
	}
	if len(request.Kinds) > 0 {
		for _, kind := range kinds {
			if !containsChangeString(request.Kinds, kind) && kindTotals[kind] != 0 {
				return errors.New("operator client: changes kind_counts include an unrequested kind")
			}
		}
	}

	seen := make(map[string]bool, len(result.Items))
	pageKindCounts := make(map[string]int, len(kinds))
	for index, item := range result.Items {
		if err := validateChangeItem(item, result.Window, result.EvaluatedAt, result.Coverage); err != nil {
			return fmt.Errorf("operator client: changes item %d: %w", index, err)
		}
		if seen[item.ChangeID] {
			return errors.New("operator client: changes page contains a duplicate change_id")
		}
		seen[item.ChangeID] = true
		pageKindCounts[item.Kind]++
		if err := validateChangeItemFilters(item, request); err != nil {
			return err
		}
		if index > 0 && compareVisibleChangeItems(result.Items[index-1], item) > 0 {
			return errors.New("operator client: changes page is not in canonical visible order")
		}
	}
	for kind, count := range pageKindCounts {
		if count > kindTotals[kind] {
			return errors.New("operator client: changes page exceeds a kind_count bucket")
		}
	}

	if request.Cursor == "" {
		wantPage := min(request.Limit, result.MatchedTotal)
		if len(result.Items) != wantPage || (result.NextCursor != nil) != (result.MatchedTotal > wantPage) {
			return errors.New("operator client: first changes page has inconsistent continuation evidence")
		}
	}
	if result.NextCursor != nil {
		if len(result.Items) != request.Limit || len(result.Items) == 0 || *result.NextCursor == request.Cursor {
			return errors.New("operator client: changes cursor cannot advance this traversal")
		}
		cursor, err := decodeChangeClientCursor(*result.NextCursor, changeClientFilterDigest(request))
		if err != nil {
			return fmt.Errorf("operator client: invalid next changes cursor: %w", err)
		}
		last := result.Items[len(result.Items)-1]
		if !cursor.EvaluatedAt.Equal(result.EvaluatedAt) || !cursor.From.Equal(result.Window.From) ||
			!cursor.To.Equal(result.Window.To) || cursor.Ceilings != result.CreationCeilings ||
			!cursor.After.At.Equal(last.ChangedAt) || cursor.After.ChangeID != last.ChangeID ||
			cursor.After.SourceRank != changeItemSourceRank(last) {
			return errors.New("operator client: next changes cursor does not anchor the returned page")
		}
		if request.Cursor != "" {
			previous, _ := decodeChangeClientCursor(request.Cursor, changeClientFilterDigest(request))
			if compareChangeClientPositions(previous.After, cursor.After) >= 0 {
				return errors.New("operator client: next changes cursor did not move after the request cursor")
			}
		}
	}
	if result.Total == 0 && (result.MatchedTotal != 0 || len(result.Items) != 0 || result.NextCursor != nil) {
		return errors.New("operator client: empty changes result carries matched rows or a cursor")
	}
	return nil
}

func validateChangeRequestWindow(result operator.ChangeListResult, request operator.ChangeListRequest) error {
	if request.Cursor != "" {
		cursor, err := decodeChangeClientCursor(request.Cursor, changeClientFilterDigest(request))
		if err != nil {
			return err
		}
		if !result.EvaluatedAt.Equal(cursor.EvaluatedAt) || !result.Window.From.Equal(cursor.From) ||
			!result.Window.To.Equal(cursor.To) || result.CreationCeilings != cursor.Ceilings {
			return errors.New("operator client: changes continuation changed its fixed window or ceilings")
		}
		for _, item := range result.Items {
			if item.ChangedAt.After(cursor.After.At) ||
				(item.ChangedAt.Equal(cursor.After.At) && changeItemSourceRank(item) < cursor.After.SourceRank) ||
				item.ChangeID == cursor.After.ChangeID {
				return errors.New("operator client: changes continuation did not advance past its anchor")
			}
		}
		return nil
	}
	if request.From != nil && !result.Window.From.Equal(request.From.UTC()) ||
		request.To != nil && !result.Window.To.Equal(request.To.UTC()) {
		return errors.New("operator client: changes response window contradicts request")
	}
	if request.To == nil && !result.Window.To.Equal(result.EvaluatedAt) {
		return errors.New("operator client: default changes window does not end at evaluated_at")
	}
	if request.From == nil && !result.Window.From.Equal(result.Window.To.Add(-operator.DefaultChangeWindow)) {
		return errors.New("operator client: default changes window is not 24 hours")
	}
	return nil
}

func validateChangeWindow(window operator.ChangeWindow, evaluatedAt time.Time) error {
	if err := validateChangeClientTime(window.From, "window.from", true); err != nil {
		return err
	}
	if err := validateChangeClientTime(window.To, "window.to", true); err != nil {
		return err
	}
	if !window.From.Before(window.To) || window.To.After(evaluatedAt) ||
		window.To.Sub(window.From) > operator.MaxChangeWindow || window.Boundary != "(from,to]" ||
		window.TimeBasis != "hub_received_at" ||
		window.MaximumSeconds != int64(operator.MaxChangeWindow/time.Second) {
		return errors.New("operator client: changes window contract is invalid")
	}
	return nil
}

func validateChangeCoverage(coverage operator.ChangeCoverage, evaluatedAt time.Time,
	request operator.ChangeListRequest,
) error {
	if coverage.ObservationComparison != "endpoint_delta" ||
		(coverage.ObservationHistory != "complete" && coverage.ObservationHistory != "partial" &&
			coverage.ObservationHistory != operator.ChangeCoverageNotApplicable) ||
		(coverage.RegistryHistory != "complete" && coverage.RegistryHistory != "partial_before_tracking_started" &&
			coverage.RegistryHistory != operator.ChangeCoverageNotApplicable) ||
		(coverage.StateHistory != "complete" && coverage.StateHistory != "partial_before_tracking_started" &&
			coverage.StateHistory != operator.ChangeCoverageNotApplicable) ||
		coverage.ObservationRowsPruned < 0 || coverage.MalformedTimestampRows < 0 ||
		coverage.UnplaceableTimestampRows < 0 ||
		coverage.UnplaceableTimestampRows > coverage.MalformedTimestampRows {
		return errors.New("operator client: changes coverage contract is invalid")
	}
	readObservations, readRegistry, readState := changeClientCoverageApplicability(request)
	for _, source := range []struct {
		name       string
		applicable bool
		status     string
	}{
		{"observation", readObservations, coverage.ObservationHistory},
		{"registry", readRegistry, coverage.RegistryHistory},
		{"state", readState, coverage.StateHistory},
	} {
		if source.applicable == (source.status == operator.ChangeCoverageNotApplicable) {
			return fmt.Errorf("operator client: changes %s coverage contradicts request applicability", source.name)
		}
	}
	for name, value := range map[string]*time.Time{
		"last_observation_pruned_at":  coverage.LastObservationPrunedAt,
		"observation_pruned_before":   coverage.ObservationPrunedBefore,
		"registry_history_started_at": coverage.RegistryHistoryStartedAt,
		"state_history_started_at":    coverage.StateHistoryStartedAt,
	} {
		if value != nil {
			if err := validateChangeClientTime(*value, "coverage."+name, true); err != nil {
				return err
			}
			if value.After(evaluatedAt) {
				return fmt.Errorf("operator client: changes coverage %s is after evaluated_at", name)
			}
		}
	}
	if coverage.ObservationHistory != "partial" &&
		(coverage.ObservationRowsPruned != 0 || coverage.LastObservationPrunedAt != nil ||
			coverage.ObservationPrunedBefore != nil) {
		return errors.New("operator client: non-partial observation coverage carries prune evidence")
	}
	if coverage.ObservationHistory == "partial" && coverage.ObservationRowsPruned == 0 {
		return errors.New("operator client: partial observation coverage lacks pruned rows")
	}
	if coverage.RegistryHistory == operator.ChangeCoverageNotApplicable && coverage.RegistryHistoryStartedAt != nil {
		return errors.New("operator client: non-applicable registry coverage carries tracking evidence")
	}
	if coverage.StateHistory == operator.ChangeCoverageNotApplicable && coverage.StateHistoryStartedAt != nil {
		return errors.New("operator client: non-applicable state coverage carries tracking evidence")
	}
	return validateChangeStringSlice("coverage.issues", coverage.Issues, 32, 128, nil)
}

func changeClientCoverageApplicability(request operator.ChangeListRequest) (observations, registry, stateHistory bool) {
	selected := func(kind string) bool {
		if len(request.Kinds) == 0 {
			return true
		}
		return containsChangeString(request.Kinds, kind)
	}
	for _, kind := range []string{
		operator.ChangeKindIdentity, operator.ChangeKindCredential, operator.ChangeKindCLITool,
		operator.ChangeKindSystemd, operator.ChangeKindOpenClaw,
	} {
		if selected(kind) {
			observations = true
			break
		}
	}
	registry = selected(operator.ChangeKindRegistry) && (request.Subject == "" || request.Subject == "lifecycle")
	stateHistory = selected(operator.ChangeKindState) && (request.Subject == "" || request.Subject == "state")
	return observations, registry, stateHistory
}

func validateChangeItem(item operator.ChangeItem, window operator.ChangeWindow, evaluatedAt time.Time,
	coverage operator.ChangeCoverage,
) error {
	if !validSHA256Digest(item.ChangeID) || !validSHA256Digest(item.MachineRef) {
		return errors.New("change_id or machine_ref is not a canonical digest")
	}
	if item.MachineID != nil {
		if err := validateChangeClientText("machine_id", *item.MachineID, 256, true); err != nil {
			return err
		}
		if strings.TrimSpace(*item.MachineID) != *item.MachineID {
			return errors.New("machine_id is not canonical")
		}
		expected := sha256.Sum256([]byte("change-machine-v1\x00" + *item.MachineID))
		if item.MachineRef != "sha256:"+hex.EncodeToString(expected[:]) {
			return errors.New("machine_ref does not bind the represented machine_id")
		}
	}
	if err := validateChangeClientText("display_name", item.DisplayName, 256, true); err != nil {
		return err
	}
	if !changeClientKnownKind(item.Kind) {
		return fmt.Errorf("unknown change kind %q", item.Kind)
	}
	if err := validateChangeClientText("subject", item.Subject, 128, true); err != nil {
		return err
	}
	if !validChangeItemSubject(item) {
		return errors.New("change subject is not canonical for its kind")
	}
	if err := validateChangeClientTime(item.ChangedAt, "changed_at", true); err != nil {
		return err
	}
	if !item.ChangedAt.After(window.From) || item.ChangedAt.After(window.To) || item.ChangedAt.After(evaluatedAt) {
		return errors.New("changed_at is outside the fixed response window")
	}
	if item.AgentMeasuredAt != nil {
		if err := validateChangeClientTime(*item.AgentMeasuredAt, "agent_measured_at", false); err != nil {
			return err
		}
	}
	wantSemantics := "window_comparison"
	if item.Kind == operator.ChangeKindState || item.Kind == operator.ChangeKindRegistry {
		wantSemantics = "transition"
	}
	if item.Semantics != wantSemantics {
		return errors.New("change semantics contradicts its kind")
	}
	if item.ChangedFields == nil || item.RedactedFields == nil || item.Issues == nil || item.AlteredFields == nil {
		return errors.New("change evidence arrays must not be null")
	}
	if err := validateChangeValue(item.Kind, item.Before, "before", item.Issues); err != nil {
		return err
	}
	if err := validateChangeValue(item.Kind, item.After, "after", item.Issues); err != nil {
		return err
	}
	if item.After == nil && !containsChangeString(item.Issues, missingChangeValueIssue(item.Kind, "after")) {
		return errors.New("missing after value lacks malformed evidence")
	}
	if !equalStringLists(item.ChangedFields, expectedChangeFields(item.Kind, item.Before, item.After)) {
		return errors.New("changed_fields do not match the typed before/after values")
	}
	if err := validateChangeStringSlice("changed_fields", item.ChangedFields, 16, 64,
		changeClientFieldSet(item.Kind)); err != nil {
		return err
	}
	if err := validateChangeRedactions(item); err != nil {
		return err
	}
	if err := validateChangeStringSlice("issues", item.Issues, 32, 128, nil); err != nil {
		return err
	}
	if err := validateChangeStringSlice("altered_fields", item.AlteredFields, 32, 128, nil); err != nil {
		return err
	}
	if err := validateChangeBaseline(item, coverage); err != nil {
		return err
	}
	if item.MachineID == nil && (!containsChangeString(item.Issues, "machine_id_not_representable") ||
		!containsChangeString(item.AlteredFields, "machine_id")) {
		return errors.New("hidden machine_id lacks alteration evidence")
	}
	if item.Kind == operator.ChangeKindState && item.After != nil && item.After.State != nil {
		want := item.After.State.Severity()
		if item.Severity == nil || *item.Severity != want {
			return errors.New("state change severity contradicts after.state")
		}
	} else if item.Severity != nil {
		return errors.New("non-state or malformed change unexpectedly carries severity")
	}
	return nil
}

func validateChangeItemFilters(item operator.ChangeItem, request operator.ChangeListRequest) error {
	if request.MachineID != "" && (item.MachineID == nil || *item.MachineID != request.MachineID) {
		return errors.New("operator client: changes item contradicts machine_id filter")
	}
	if len(request.Kinds) > 0 && !containsChangeString(request.Kinds, item.Kind) {
		return errors.New("operator client: changes item contradicts kind filter")
	}
	if request.Subject != "" && item.Subject != request.Subject {
		return errors.New("operator client: changes item contradicts subject filter")
	}
	return nil
}

func validateChangeBaseline(item operator.ChangeItem, coverage operator.ChangeCoverage) error {
	switch item.BaselineStatus {
	case "known":
		if item.Before == nil &&
			(!changeClientTransitionKind(item.Kind) ||
				!containsChangeString(item.Issues, missingChangeValueIssue(item.Kind, "before"))) {
			return errors.New("known baseline is missing its typed before value")
		}
	case "not_registered":
		if item.Kind != operator.ChangeKindRegistry || item.Before != nil {
			return errors.New("not_registered baseline is inconsistent")
		}
	case "not_observed":
		if item.Kind == operator.ChangeKindRegistry || item.Before != nil {
			return errors.New("not_observed baseline is inconsistent")
		}
	case "possibly_pruned":
		if changeClientTransitionKind(item.Kind) || item.Before != nil || coverage.ObservationHistory != "partial" ||
			!containsChangeString(item.Issues, "baseline_possibly_pruned") {
			return errors.New("possibly_pruned baseline lacks coverage evidence")
		}
	case "malformed":
		if changeClientTransitionKind(item.Kind) || item.Before != nil ||
			!containsChangeString(item.Issues, "before_payload_malformed") {
			return errors.New("malformed baseline lacks payload evidence")
		}
	default:
		return fmt.Errorf("unknown baseline_status %q", item.BaselineStatus)
	}
	return nil
}

func validateChangeValue(kind string, value *operator.ChangeValue, label string, issues []string) error {
	if value == nil {
		return nil
	}
	allowed := changeClientFieldSet(kind)
	for name, present := range changeValuePresence(*value) {
		if present && !allowed[name] {
			return fmt.Errorf("%s value carries field %s for kind %s", label, name, kind)
		}
	}
	switch kind {
	case operator.ChangeKindState:
		if value.State == nil || !changeClientKnownState(*value.State) {
			return fmt.Errorf("%s state value is not canonical", label)
		}
	case operator.ChangeKindRegistry:
		if value.Lifecycle == nil || (*value.Lifecycle != "registered" && *value.Lifecycle != "retired") {
			return fmt.Errorf("%s lifecycle value is not canonical", label)
		}
	case operator.ChangeKindIdentity:
		if value.LingerEnabled == nil {
			return fmt.Errorf("%s identity value is incomplete", label)
		}
		for name, text := range map[string]*string{"os": value.OS, "kernel": value.Kernel, "arch": value.Arch} {
			limit := 128
			if name == "arch" {
				limit = 64
			}
			if text != nil {
				if err := validateChangeClientText(label+"."+name, *text, limit, true); err != nil {
					return err
				}
			}
		}
	case operator.ChangeKindCredential:
		if value.Status != nil && !changeClientKnownCredentialStatus(*value.Status) {
			return fmt.Errorf("%s credential status is not canonical", label)
		}
		if value.Status == nil && !containsChangeString(issues, label+"_credential_status_not_canonical") {
			return fmt.Errorf("%s credential status is missing without canonical issue evidence", label)
		}
		if value.ExpiresAt != nil {
			if err := validateChangeClientTime(*value.ExpiresAt, label+".expires_at", false); err != nil {
				return err
			}
		}
	case operator.ChangeKindCLITool:
		if value.Present == nil || value.OnPath == nil || value.VersionSourcesDisagree == nil ||
			value.RunningInstallMismatch == nil {
			return fmt.Errorf("%s cli_tool value is incomplete", label)
		}
		for name, text := range map[string]*string{
			"version_reported": value.VersionReported, "version_package_json": value.VersionPackageJSON,
		} {
			if text != nil {
				if err := validateChangeClientText(label+"."+name, *text, 128, true); err != nil {
					return err
				}
			}
		}
		if value.DaemonReach != nil && *value.DaemonReach != model.DaemonReachSame &&
			*value.DaemonReach != model.DaemonReachShadowed && *value.DaemonReach != model.DaemonReachMissing {
			return fmt.Errorf("%s daemon_reach is not canonical", label)
		}
	case operator.ChangeKindSystemd:
		unmeasured := value.Measured != nil && !*value.Measured
		if value.Present == nil && !unmeasured {
			return fmt.Errorf("%s systemd value is incomplete", label)
		}
		if value.Present != nil && *value.Present && value.Measured != nil {
			return fmt.Errorf("%s systemd present value must omit measured", label)
		}
		if value.Present != nil && !*value.Present && (value.Measured == nil || !*value.Measured) {
			return fmt.Errorf("%s systemd absent value must set measured true", label)
		}
		if unmeasured && value.Restarts != nil {
			return fmt.Errorf("%s systemd unmeasured value must omit restarts", label)
		}
		if value.Restarts != nil && *value.Restarts < 0 {
			return fmt.Errorf("%s systemd restarts must not be negative", label)
		}
		if !unmeasured && value.Restarts == nil && !containsChangeString(issues, label+"_restarts_not_canonical") {
			return fmt.Errorf("%s systemd value omits restarts without canonical issue evidence", label)
		}
		for name, text := range map[string]*string{"active_state": value.ActiveState, "sub_state": value.SubState} {
			if text != nil {
				if err := validateChangeClientText(label+"."+name, *text, 64, true); err != nil {
					return err
				}
			}
		}
	case operator.ChangeKindOpenClaw:
		if value.Present == nil {
			return fmt.Errorf("%s openclaw value is incomplete", label)
		}
		for name, text := range map[string]*string{
			"cli_version": value.CLIVersion, "gateway_version": value.GatewayVersion,
			"upstream_version": value.UpstreamVersion,
		} {
			if text != nil {
				if err := validateChangeClientText(label+"."+name, *text, 128, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func missingChangeValueIssue(kind, label string) string {
	switch kind {
	case operator.ChangeKindState:
		return label + "_state_not_canonical"
	case operator.ChangeKindRegistry:
		return label + "_lifecycle_not_canonical"
	default:
		return label + "_payload_malformed"
	}
}

func validateChangeRedactions(item operator.ChangeItem) error {
	allowed := map[string]bool{}
	var required []string
	switch item.Kind {
	case operator.ChangeKindIdentity:
		allowed["host_identity"] = true
		required = []string{"host_identity"}
	case operator.ChangeKindCredential:
		allowed["account_identity"] = true
		required = []string{"account_identity"}
	case operator.ChangeKindCLITool:
		allowed["execution_paths"], allowed["process_identity"] = true, true
		required = []string{"execution_paths", "process_identity"}
	case operator.ChangeKindSystemd:
		allowed["process_identity"] = true
		required = []string{"process_identity"}
	case operator.ChangeKindOpenClaw:
		allowed["absence_reason"], allowed["installation_details"] = true, true
		required = []string{"absence_reason", "installation_details"}
	}
	if err := validateChangeStringSlice("redacted_fields", item.RedactedFields, 8, 64, allowed); err != nil {
		return err
	}
	if required != nil && !equalStringLists(item.RedactedFields, required) {
		return errors.New("redacted_fields omit the kind's fixed private evidence")
	}
	return nil
}

func changeValuePresence(value operator.ChangeValue) map[string]bool {
	return map[string]bool{
		"state": value.State != nil, "lifecycle": value.Lifecycle != nil,
		"present": value.Present != nil, "measured": value.Measured != nil,
		"status": value.Status != nil, "expires_at": value.ExpiresAt != nil,
		"os": value.OS != nil, "kernel": value.Kernel != nil, "arch": value.Arch != nil,
		"linger_enabled": value.LingerEnabled != nil, "on_path": value.OnPath != nil,
		"version_reported":         value.VersionReported != nil,
		"version_package_json":     value.VersionPackageJSON != nil,
		"version_sources_disagree": value.VersionSourcesDisagree != nil,
		"daemon_reach":             value.DaemonReach != nil,
		"running_install_mismatch": value.RunningInstallMismatch != nil,
		"active_state":             value.ActiveState != nil, "sub_state": value.SubState != nil,
		"restarts": value.Restarts != nil, "cli_version": value.CLIVersion != nil,
		"gateway_version": value.GatewayVersion != nil, "upstream_version": value.UpstreamVersion != nil,
	}
}

func changeClientFieldSet(kind string) map[string]bool {
	result := make(map[string]bool)
	for _, field := range changeClientFields(kind) {
		result[field] = true
	}
	return result
}

func changeClientFields(kind string) []string {
	switch kind {
	case operator.ChangeKindState:
		return []string{"state"}
	case operator.ChangeKindRegistry:
		return []string{"lifecycle"}
	case operator.ChangeKindIdentity:
		return []string{"arch", "kernel", "linger_enabled", "os"}
	case operator.ChangeKindCredential:
		return []string{"expires_at", "status"}
	case operator.ChangeKindCLITool:
		return []string{"daemon_reach", "on_path", "present", "running_install_mismatch", "version_package_json", "version_reported", "version_sources_disagree"}
	case operator.ChangeKindSystemd:
		return []string{"active_state", "measured", "present", "restarts", "sub_state"}
	case operator.ChangeKindOpenClaw:
		return []string{"cli_version", "gateway_version", "present", "upstream_version"}
	default:
		return []string{}
	}
}

func expectedChangeFields(kind string, before, after *operator.ChangeValue) []string {
	fields := changeClientFields(kind)
	zero := reflect.Zero(reflect.TypeOf(operator.ChangeValue{}))
	b, a := zero, zero
	if before != nil {
		b = reflect.ValueOf(*before)
	}
	if after != nil {
		a = reflect.ValueOf(*after)
	}
	typ := b.Type()
	allowed := changeClientFieldSet(kind)
	result := make([]string, 0, len(fields))
	for index := 0; index < b.NumField(); index++ {
		name := strings.Split(typ.Field(index).Tag.Get("json"), ",")[0]
		if allowed[name] && !reflect.DeepEqual(b.Field(index).Interface(), a.Field(index).Interface()) {
			result = append(result, name)
		}
	}
	sort.Strings(result)
	return result
}

func validateChangeStringSlice(name string, values []string, maxItems, maxBytes int, allowed map[string]bool) error {
	if values == nil || len(values) > maxItems {
		return fmt.Errorf("operator client: changes %s is null or oversized", name)
	}
	previous := ""
	for index, value := range values {
		if err := validateChangeClientText(name, value, maxBytes, true); err != nil {
			return err
		}
		if index > 0 && value <= previous {
			return fmt.Errorf("operator client: changes %s is duplicated or not sorted", name)
		}
		if allowed != nil && !allowed[value] {
			return fmt.Errorf("operator client: changes %s contains unknown value %q", name, value)
		}
		previous = value
	}
	return nil
}

func validateChangeClientText(name, value string, maxBytes int, requireNonEmpty bool) error {
	if (requireNonEmpty && value == "") || len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("operator client: changes %s is empty, oversized, or invalid UTF-8", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("operator client: changes %s contains control or format characters", name)
		}
	}
	return nil
}

func validateChangeClientTime(value time.Time, name string, requireSecondPrecision bool) error {
	if value.IsZero() {
		return fmt.Errorf("operator client: changes %s is missing", name)
	}
	if _, offset := value.Zone(); offset != 0 {
		return fmt.Errorf("operator client: changes %s must be UTC", name)
	}
	if requireSecondPrecision && value.Nanosecond() != 0 {
		return fmt.Errorf("operator client: changes %s must use second precision", name)
	}
	return nil
}

func validChangeItemSubject(item operator.ChangeItem) bool {
	switch item.Kind {
	case operator.ChangeKindState:
		return item.Subject == "state"
	case operator.ChangeKindRegistry:
		return item.Subject == "lifecycle"
	case operator.ChangeKindIdentity:
		return item.Subject == "identity"
	case operator.ChangeKindOpenClaw:
		return item.Subject == "openclaw"
	case operator.ChangeKindCredential:
		return containsChangeString([]string{"claude", "codex", "gemini", "grok", "openclaw"}, item.Subject) ||
			changeClientRedactedSubject(item)
	case operator.ChangeKindCLITool:
		return containsChangeString([]string{"agy", "claude", "codex", "gemini", "grok", "openclaw"}, item.Subject) ||
			changeClientRedactedSubject(item)
	case operator.ChangeKindSystemd:
		return containsChangeString([]string{
			"bat-server.service", "clawctl-agent.service", "clawctl-hermes.service", "hermes-gateway.service",
			"openclaw-gateway.service", "openclaw-watcher.service",
		}, item.Subject) || changeClientRedactedSubject(item)
	default:
		return false
	}
}

func changeClientRedactedSubject(item operator.ChangeItem) bool {
	if item.Kind != operator.ChangeKindCredential && item.Kind != operator.ChangeKindCLITool &&
		item.Kind != operator.ChangeKindSystemd {
		return false
	}
	return item.Subject == "(redacted subject)" &&
		containsChangeString(item.Issues, "subject_not_allowlisted") &&
		containsChangeString(item.AlteredFields, "subject")
}

func changeClientKnownKind(value string) bool {
	return containsChangeString(operator.ChangeKinds(), value)
}

func changeClientKnownState(value state.State) bool {
	for _, candidate := range state.AllStates {
		if value == candidate {
			return true
		}
	}
	return false
}

func changeClientKnownCredentialStatus(value model.CredStatus) bool {
	switch value {
	case model.CredAbsent, model.CredConfigured, model.CredExpired, model.CredUnknown,
		model.CredFailed, model.CredExpiresSoon:
		return true
	default:
		return false
	}
}

func changeClientTransitionKind(kind string) bool {
	return kind == operator.ChangeKindState || kind == operator.ChangeKindRegistry
}

func changeItemSourceRank(item operator.ChangeItem) int {
	switch item.Kind {
	case operator.ChangeKindState:
		return 1
	case operator.ChangeKindRegistry:
		return 2
	default:
		return 3
	}
}

// compareVisibleChangeItems returns a positive value only when the public
// fields prove that left belongs after right. Source rowid is intentionally
// opaque, so equal timestamp/rank items cannot be ordered more strictly here.
func compareVisibleChangeItems(left, right operator.ChangeItem) int {
	if !left.ChangedAt.Equal(right.ChangedAt) {
		if left.ChangedAt.After(right.ChangedAt) {
			return -1
		}
		return 1
	}
	leftRank, rightRank := changeItemSourceRank(left), changeItemSourceRank(right)
	if leftRank < rightRank {
		return -1
	}
	if leftRank > rightRank {
		return 1
	}
	return 0
}

func compareChangeClientPositions(left, right changeClientPosition) int {
	if !left.At.Equal(right.At) {
		if left.At.After(right.At) {
			return -1
		}
		return 1
	}
	if left.SourceRank < right.SourceRank {
		return -1
	}
	if left.SourceRank > right.SourceRank {
		return 1
	}
	if left.SourceRowID > right.SourceRowID {
		return -1
	}
	if left.SourceRowID < right.SourceRowID {
		return 1
	}
	return strings.Compare(left.ChangeID, right.ChangeID)
}

func containsChangeString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func equalStringLists(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
