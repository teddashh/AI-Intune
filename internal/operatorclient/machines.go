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
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

const maxMachineDetailResponseBytes int64 = 4 << 20

var machineDetailTextPolicies = map[string]evidenceTextFieldPolicy{
	"monitor.agent_version":               {maxBytes: 256, policy: evidenceTextStrict},
	"checkin.boot_id":                     {maxBytes: 256, policy: evidenceTextStrict},
	"identity.hostname":                   {maxBytes: 256, policy: evidenceTextStrict},
	"identity.os":                         {maxBytes: 256, policy: evidenceTextStrict},
	"identity.kernel":                     {maxBytes: 256, policy: evidenceTextStrict},
	"identity.arch":                       {maxBytes: 64, policy: evidenceTextStrict},
	"identity.unix_user":                  {maxBytes: 256, policy: evidenceTextStrict},
	"identity.machine_id_hint":            {maxBytes: 256, policy: evidenceTextStrict},
	"identity.tailscale_ip":               {maxBytes: 64, policy: evidenceTextStrict},
	"judgement.reason":                    {maxBytes: operator.MachineDetailMaxTextBytes, policy: evidenceTextBlock},
	"judgement.finding.message":           {maxBytes: operator.MachineDetailMaxTextBytes, policy: evidenceTextBlock},
	"expectations.error":                  {maxBytes: operator.MachineDetailMaxTextBytes, policy: evidenceTextBlock},
	"expectations.rule.unit":              {maxBytes: 256, policy: evidenceTextStrict},
	"expectations.rule.artifact":          {maxBytes: operator.MachineDetailMaxTextBytes, policy: evidenceTextStrict},
	"expectations.rule.why":               {maxBytes: operator.MachineDetailMaxTextBytes, policy: evidenceTextBlock},
	"expectations.rule.observation.error": {maxBytes: operator.MachineDetailMaxTextBytes, policy: evidenceTextBlock},
	"expectations.rule.events.type":       {maxBytes: 256, policy: evidenceTextStrict},
	"expectations.rule.events.error":      {maxBytes: operator.MachineDetailMaxTextBytes, policy: evidenceTextBlock},
	"state_history.reason":                {maxBytes: operator.MachineDetailMaxTextBytes, policy: evidenceTextBlock},
}

func (c *Client) Machines(ctx context.Context) (operator.MachineListResult, error) {
	return c.ListMachines(ctx, operator.MachineListRequest{})
}

func (c *Client) ListMachines(ctx context.Context, request operator.MachineListRequest) (operator.MachineListResult, error) {
	query, normalized, err := encodeMachineListQuery(request)
	if err != nil {
		return operator.MachineListResult{}, err
	}
	path := "/v1/operator/machines"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.MachineListResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.MachineListResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.MachineListResult{}, fmt.Errorf(
			"operator client: machine list returned unexpected success status HTTP %d", response.status)
	}
	if err := validateMachineReadHeaders(response.header); err != nil {
		return operator.MachineListResult{}, err
	}
	var result operator.MachineListResult
	if err := decodeStrictJSONDocument(response.body, "machine list", &result); err != nil {
		return operator.MachineListResult{}, err
	}
	if err := validateMachineListResult(result, normalized); err != nil {
		return operator.MachineListResult{}, err
	}
	return result, nil
}

func encodeMachineListQuery(request operator.MachineListRequest) (url.Values, operator.MachineListRequest, error) {
	query := make(url.Values)
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"machine_id", request.MachineID, 256}, {"display_name", request.DisplayName, 256},
		{"cursor", request.Cursor, 2048},
	} {
		if field.value == "" {
			continue
		}
		if err := validateMachineClientText(field.name, field.value, field.max); err != nil {
			return nil, operator.MachineListRequest{}, err
		}
		query.Set(field.name, field.value)
	}
	seenStates := make(map[state.State]bool, len(request.States))
	for _, candidate := range request.States {
		if !machineClientKnownState(candidate) || seenStates[candidate] {
			return nil, operator.MachineListRequest{}, fmt.Errorf("operator client: invalid or duplicate machine state %q", candidate)
		}
		seenStates[candidate] = true
	}
	request.States = make([]state.State, 0, len(seenStates))
	for _, candidate := range state.AllStates {
		if seenStates[candidate] {
			request.States = append(request.States, candidate)
			query.Add("state", string(candidate))
		}
	}
	if request.Lifecycle == "" {
		request.Lifecycle = operator.MachineLifecycleAny
	} else {
		query.Set("lifecycle", string(request.Lifecycle))
	}
	if request.Lifecycle != operator.MachineLifecycleAny && request.Lifecycle != operator.MachineLifecycleActive &&
		request.Lifecycle != operator.MachineLifecycleRetired {
		return nil, operator.MachineListRequest{}, errors.New("operator client: machine lifecycle must be any, active, or retired")
	}
	if request.Reporting == "" {
		request.Reporting = operator.MachineReportingAny
	} else {
		query.Set("reporting", string(request.Reporting))
	}
	if request.Reporting != operator.MachineReportingAny && request.Reporting != operator.MachineReportingTrue &&
		request.Reporting != operator.MachineReportingFalse && request.Reporting != operator.MachineReportingUnknown {
		return nil, operator.MachineListRequest{}, errors.New("operator client: machine reporting must be any, true, false, or unknown")
	}
	if request.Channel == "" {
		request.Channel = operator.MachineChannelAny
	} else {
		query.Set("channel", string(request.Channel))
	}
	if request.Channel != operator.MachineChannelAny && request.Channel != operator.MachineChannelNone &&
		request.Channel != operator.MachineChannelCanary && request.Channel != operator.MachineChannelStable {
		return nil, operator.MachineListRequest{}, errors.New("operator client: machine channel must be any, none, canary, or stable")
	}
	if request.Limit == 0 {
		request.Limit = operator.DefaultMachineReadLimit
	} else {
		if request.Limit < 1 || request.Limit > operator.MaxMachineReadLimit {
			return nil, operator.MachineListRequest{}, fmt.Errorf("operator client: machine limit must be between 1 and %d", operator.MaxMachineReadLimit)
		}
		query.Set("limit", strconv.Itoa(request.Limit))
	}
	if request.Cursor != "" {
		if _, err := decodeMachineClientCursor(request.Cursor, machineClientFilterDigest(request)); err != nil {
			return nil, operator.MachineListRequest{}, fmt.Errorf("operator client: invalid machine cursor: %w", err)
		}
	}
	return query, request, nil
}

func validateMachineClientText(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("operator client: machine %s is empty, oversized, invalid UTF-8, or has surrounding whitespace", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("operator client: machine %s contains control or formatting characters", name)
		}
	}
	return nil
}

func machineClientKnownState(candidate state.State) bool {
	for _, known := range state.AllStates {
		if candidate == known {
			return true
		}
	}
	return false
}

type machineClientCursor struct {
	Version         int                          `json:"v"`
	FilterDigest    string                       `json:"filter_digest"`
	CreationCeiling int64                        `json:"creation_ceiling"`
	After           operator.MachineReadPosition `json:"after"`
}

func machineClientFilterDigest(request operator.MachineListRequest) string {
	body := struct {
		Version     int                             `json:"v"`
		MachineID   string                          `json:"machine_id"`
		DisplayName string                          `json:"display_name"`
		States      []state.State                   `json:"states"`
		Lifecycle   operator.MachineLifecycleFilter `json:"lifecycle"`
		Reporting   operator.MachineReportingFilter `json:"reporting"`
		Channel     operator.MachineChannelFilter   `json:"channel"`
	}{
		Version: operator.MachineListReadSchemaVersion, MachineID: request.MachineID,
		DisplayName: request.DisplayName, States: request.States, Lifecycle: request.Lifecycle,
		Reporting: request.Reporting, Channel: request.Channel,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func decodeMachineClientCursor(encoded, filterDigest string) (machineClientCursor, error) {
	if err := validateMachineClientText("cursor", encoded, 2048); err != nil {
		return machineClientCursor{}, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return machineClientCursor{}, errors.New("cursor encoding is not canonical base64url")
	}
	var cursor machineClientCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return machineClientCursor{}, errors.New("cursor document is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return machineClientCursor{}, errors.New("cursor has trailing JSON")
	}
	canonical, _ := json.Marshal(cursor)
	if base64.RawURLEncoding.EncodeToString(canonical) != encoded {
		return machineClientCursor{}, errors.New("cursor JSON is not canonical")
	}
	_, offset := cursor.After.CreatedAt.Zone()
	if cursor.Version != operator.MachineListReadSchemaVersion || cursor.FilterDigest != filterDigest ||
		cursor.CreationCeiling < 1 || cursor.After.CreatedAt.IsZero() || offset != 0 ||
		validateMachineClientText("cursor machine_id", cursor.After.MachineID, 256) != nil {
		return machineClientCursor{}, errors.New("cursor does not match this machine query")
	}
	return cursor, nil
}

func (c *Client) Machine(ctx context.Context, machineID string) (operator.MachineDetailResult, error) {
	req, err := c.newMachineOperatorRequest(ctx, http.MethodGet, machineID, "", nil)
	if err != nil {
		return operator.MachineDetailResult{}, err
	}
	response, err := c.doRawWithLimit(req, maxMachineDetailResponseBytes, "4 MiB")
	if err != nil {
		return operator.MachineDetailResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.MachineDetailResult{}, fmt.Errorf(
			"operator client: machine detail returned unexpected success status HTTP %d", response.status)
	}
	if err := validateMachineReadHeaders(response.header); err != nil {
		return operator.MachineDetailResult{}, err
	}
	var result operator.MachineDetailResult
	if err := decodeStrictJSONDocument(response.body, "machine detail", &result); err != nil {
		return operator.MachineDetailResult{}, err
	}
	if err := validateMachineDetailResult(result, machineID); err != nil {
		return operator.MachineDetailResult{}, err
	}
	return result, nil
}

func validateMachineReadHeaders(header http.Header) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	if replayed, err := responseReplayEvidenceFromHeader(header); err != nil {
		return err
	} else if replayed {
		return errors.New("operator client: machine read cannot be an idempotency replay")
	}
	if values := header.Values("ETag"); len(values) != 0 {
		return errors.New("operator client: composite machine read must not carry an ETag")
	}
	return nil
}

func validateMachineListResult(result operator.MachineListResult, request operator.MachineListRequest) error {
	if result.SchemaVersion != operator.MachineListReadSchemaVersion || result.Consistency != operator.MachineReadConsistency {
		return fmt.Errorf("operator client: unsupported machine read schema_version %d", result.SchemaVersion)
	}
	if err := validateMachineEvaluationTime(result.EvaluatedAt); err != nil {
		return err
	}
	if result.Items == nil || result.StateCounts == nil || result.DenominatorStateCounts == nil {
		return errors.New("operator client: machine list arrays must not be null")
	}
	if result.Total < 0 || result.MatchedTotal < 0 || result.MatchedTotal > result.Total ||
		len(result.Items) > request.Limit || len(result.Items) > result.MatchedTotal ||
		result.Active < 0 || result.Retired < 0 ||
		result.Active+result.Retired != result.Total || result.Expected != result.Active ||
		result.Reporting < 0 || result.Reporting > result.Expected {
		return errors.New("operator client: inconsistent machine list totals")
	}
	if (result.Total == 0) != (result.CreationCeiling == 0) || result.CreationCeiling < 0 {
		return errors.New("operator client: inconsistent machine creation ceiling")
	}
	if err := validateMachineStateCounts(result.StateCounts, result.Active, "visible"); err != nil {
		return err
	}
	if err := validateMachineStateCounts(result.DenominatorStateCounts, result.Expected, "denominator"); err != nil {
		return err
	}
	for i := range result.StateCounts {
		if result.DenominatorStateCounts[i] != result.StateCounts[i] {
			return errors.New("operator client: machine denominator state counts differ from active state counts")
		}
	}
	seen := make(map[string]bool, len(result.Items))
	pageStateCounts := make(map[state.State]int, len(state.AllStates))
	pageDenominatorCounts := make(map[state.State]int, len(state.AllStates))
	pageActive, pageRetired, pageExpected, pageReporting := 0, 0, 0, 0
	var previous *operator.MachineReadPosition
	for _, item := range result.Items {
		if err := validateMachineSummary(item, result.EvaluatedAt); err != nil {
			return err
		}
		if seen[item.MachineID] {
			return fmt.Errorf("operator client: duplicate machine_id %q", item.MachineID)
		}
		seen[item.MachineID] = true
		if !machineSummaryMatchesRequest(item, request) {
			return errors.New("operator client: machine item contradicts request filters")
		}
		position := operator.MachineReadPosition{CreatedAt: item.CreatedAt, MachineID: item.MachineID}
		if previous != nil && compareMachineClientPosition(*previous, position) >= 0 {
			return errors.New("operator client: machine page is not in canonical keyset order")
		}
		if request.Cursor != "" {
			cursor, _ := decodeMachineClientCursor(request.Cursor, machineClientFilterDigest(request))
			if compareMachineClientPosition(cursor.After, position) >= 0 {
				return errors.New("operator client: machine response did not advance after cursor anchor")
			}
		}
		if item.RetiredAt != nil {
			pageRetired++
		} else {
			pageActive++
			pageStateCounts[*item.State]++
			pageExpected++
			pageDenominatorCounts[*item.State]++
			if item.Reporting != nil && *item.Reporting {
				pageReporting++
			}
		}
		previous = &position
	}
	if pageActive > result.Active || pageRetired > result.Retired || pageExpected > result.Expected ||
		pageReporting > result.Reporting {
		return errors.New("operator client: machine page totals exceed list envelope")
	}
	for i, candidate := range state.AllStates {
		if pageStateCounts[candidate] > result.StateCounts[i].Count ||
			pageDenominatorCounts[candidate] > result.DenominatorStateCounts[i].Count {
			return errors.New("operator client: machine page state counts exceed list envelope")
		}
	}
	if request.MachineID != "" && result.MatchedTotal > 1 {
		return errors.New("operator client: exact machine_id filter matched more than one row")
	}
	unfiltered := machineClientRequestUnfiltered(request)
	if unfiltered && result.MatchedTotal != result.Total {
		return errors.New("operator client: unfiltered machine result has inconsistent matched_total")
	}
	if request.Cursor != "" {
		cursor, err := decodeMachineClientCursor(request.Cursor, machineClientFilterDigest(request))
		if err != nil || cursor.CreationCeiling != result.CreationCeiling {
			return errors.New("operator client: machine response changed cursor creation ceiling")
		}
	}
	if request.Cursor == "" && (result.MatchedTotal > len(result.Items)) != (result.NextCursor != nil) {
		return errors.New("operator client: first machine page has inconsistent continuation evidence")
	}
	if result.NextCursor != nil {
		if len(result.Items) != request.Limit || *result.NextCursor == request.Cursor {
			return errors.New("operator client: invalid machine continuation cardinality")
		}
		next, err := decodeMachineClientCursor(*result.NextCursor, machineClientFilterDigest(request))
		last := result.Items[len(result.Items)-1]
		if err != nil || next.CreationCeiling != result.CreationCeiling ||
			!next.After.CreatedAt.Equal(last.CreatedAt) || next.After.MachineID != last.MachineID {
			return errors.New("operator client: machine next cursor does not anchor the final item")
		}
	}
	if unfiltered && request.Cursor == "" && result.NextCursor == nil &&
		(len(result.Items) != result.Total || pageActive != result.Active || pageRetired != result.Retired ||
			pageExpected != result.Expected || pageReporting != result.Reporting) {
		return errors.New("operator client: complete machine page does not equal list envelope")
	}
	return nil
}

func machineClientRequestUnfiltered(request operator.MachineListRequest) bool {
	return request.MachineID == "" && request.DisplayName == "" && len(request.States) == 0 &&
		request.Lifecycle == operator.MachineLifecycleAny &&
		request.Reporting == operator.MachineReportingAny && request.Channel == operator.MachineChannelAny
}

func machineSummaryMatchesRequest(item operator.MachineSummary, request operator.MachineListRequest) bool {
	if request.MachineID != "" && item.MachineID != request.MachineID {
		return false
	}
	if request.DisplayName != "" && item.DisplayName != request.DisplayName {
		return false
	}
	retired := item.RetiredAt != nil
	if request.Lifecycle == operator.MachineLifecycleActive && retired ||
		request.Lifecycle == operator.MachineLifecycleRetired && !retired {
		return false
	}
	if len(request.States) > 0 {
		matched := false
		for _, candidate := range request.States {
			matched = matched || item.State != nil && *item.State == candidate
		}
		if !matched {
			return false
		}
	}
	switch request.Reporting {
	case operator.MachineReportingTrue:
		if item.Reporting == nil || !*item.Reporting {
			return false
		}
	case operator.MachineReportingFalse:
		if item.Reporting == nil || *item.Reporting {
			return false
		}
	case operator.MachineReportingUnknown:
		if item.Reporting != nil {
			return false
		}
	}
	switch request.Channel {
	case operator.MachineChannelNone:
		return item.Channel == nil
	case operator.MachineChannelCanary:
		return item.Channel != nil && *item.Channel == "canary"
	case operator.MachineChannelStable:
		return item.Channel != nil && *item.Channel == "stable"
	}
	return true
}

// compareMachineClientPosition follows the wire order. Negative means a is
// before b (newer creation time, then descending machine_id).
func compareMachineClientPosition(a, b operator.MachineReadPosition) int {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		if a.CreatedAt.After(b.CreatedAt) {
			return -1
		}
		return 1
	}
	return -strings.Compare(a.MachineID, b.MachineID)
}

func validateMachineDetailResult(result operator.MachineDetailResult, machineID string) error {
	if result.SchemaVersion != operator.MachineDetailReadSchemaVersion {
		return fmt.Errorf("operator client: unsupported machine read schema_version %d", result.SchemaVersion)
	}
	if err := validateMachineEvaluationTime(result.EvaluatedAt); err != nil {
		return err
	}
	if err := validateMachineSummary(result.Item, result.EvaluatedAt); err != nil {
		return err
	}
	if result.Item.MachineID != machineID {
		return errors.New("operator client: machine detail response machine_id does not match request")
	}
	if err := validateMachineDetailDisclosure(result); err != nil {
		return err
	}
	if err := validateMachineJudgement(result); err != nil {
		return err
	}
	if err := validateMachineExpectations(result); err != nil {
		return err
	}
	if err := validateMachineMonitor(result); err != nil {
		return err
	}
	if err := validateMachineCheckins(result); err != nil {
		return err
	}
	if err := validateMachineStateHistory(result); err != nil {
		return err
	}
	if err := validateMachineIdentity(result); err != nil {
		return err
	}
	if err := validateMachineIdentityHints(result); err != nil {
		return err
	}
	if err := validateMachineResources(result); err != nil {
		return err
	}
	return nil
}

func validateMachineDetailDisclosure(result operator.MachineDetailResult) error {
	d := result.Disclosure
	if d.CheckinLimit != store.DetailCheckinLimit ||
		d.CheckinWindowSeconds != int64(store.DetailCheckinWindow/time.Second) ||
		d.StateHistoryLimit != store.DetailHistoryLimit ||
		d.IdentityHintLimit != operator.MachineDetailIdentityHintLimit ||
		d.FindingLimit != operator.MachineDetailFindingLimit ||
		d.ExpectationLimit != operator.MachineDetailExpectationLimit ||
		d.EventTypeLimit != operator.MachineDetailEventTypeLimit ||
		d.MaxTextBytes != operator.MachineDetailMaxTextBytes || d.IndependentVerifier ||
		!d.LivenessUsesReceivedAt || d.AgentSentAtIsLiveness || !d.HostIdentityIncluded ||
		!d.TailscaleIPIncluded || !d.ConnectCoordinatesExcluded || !d.PendingEnrollmentExcluded ||
		!d.ExpectationDisplayDefinitionsIncluded || !d.ExpectationConfigPathFieldExcluded ||
		!d.ExpectationParserFieldsExcluded || !d.ArtifactPathsIncluded || d.ArtifactContentInspected ||
		d.ArtifactFreshnessIsWorkOutcome || !d.EventFailureTypesOperatorDeclared ||
		d.EventContentKeywordScanning || d.ExpectationTextPathRedacted || d.ExpectationTextSecretRedacted ||
		!d.JudgementDerivedByHub || !d.JudgementMayUseUnverifiedInput ||
		d.JudgementTextParsedByHub || d.JudgementTextPathRedacted || d.JudgementTextSecretRedacted ||
		!d.KnownSecretFieldsExcluded || d.StateReasonParsedByHub || d.StateReasonPathRedactedByHub ||
		d.CompleteHistoryClaimed || !d.IdentityHintsNonExpiring {
		return errors.New("operator client: machine detail disclosure metadata is inconsistent")
	}
	p := d.ObserverProducer
	if p.Kind != operator.MachineEvidenceProducerObserverAgent ||
		p.Authority != operator.MachineEvidenceAuthorityMachineBearer ||
		p.MachineID != result.Item.MachineID || p.DisplayName != result.Item.DisplayName {
		return errors.New("operator client: machine detail observer provenance is inconsistent")
	}
	return nil
}

func validateMachineExpectations(result operator.MachineDetailResult) error {
	e := result.Expectations
	if (e.Error != nil) != e.ReadFailed || (!e.Configured && e.ReadFailed) ||
		(!e.Configured && (e.Rules.Total != 0 || e.Rules.Invalid != 0 || e.Rules.Truncated || len(e.Rules.Items) != 0)) {
		return errors.New("operator client: machine expectation section metadata is inconsistent")
	}
	if err := validateOptionalMachineDetailText(e.Error, result.Disclosure.MaxTextBytes, "expectations.error"); err != nil {
		return err
	}
	if e.ReadFailed && (e.Rules.Total != 0 || e.Rules.Invalid != 0 || e.Rules.Truncated || len(e.Rules.Items) != 0) {
		return errors.New("operator client: failed machine expectation read contains rules")
	}
	p := e.Rules
	if p.Items == nil || p.Total < 0 || p.Invalid < 0 || p.Invalid > p.Total ||
		len(p.Items) > result.Disclosure.ExpectationLimit || len(p.Items)+p.Invalid > p.Total ||
		p.Truncated != (len(p.Items)+p.Invalid < p.Total) {
		return errors.New("operator client: machine expectation page metadata is inconsistent")
	}
	for _, rule := range p.Items {
		if strings.TrimSpace(rule.Unit.Text) == "" || rule.Artifact.Text == "" || strings.TrimSpace(rule.Why.Text) == "" ||
			!strings.HasPrefix(rule.Artifact.Text, "/") || rule.MaxAgeSeconds <= 0 {
			return errors.New("operator client: machine expectation definition is invalid")
		}
		for _, field := range []struct {
			name  string
			value operator.EvidenceText
		}{
			{"expectations.rule.unit", rule.Unit},
			{"expectations.rule.artifact", rule.Artifact},
			{"expectations.rule.why", rule.Why},
		} {
			if err := validateMachineDetailText(field.value, result.Disclosure.MaxTextBytes, field.name); err != nil {
				return err
			}
		}
		if err := validateMachineArtifactObservation(rule.Observation, result); err != nil {
			return err
		}
		if rule.Events != nil {
			if err := validateMachineExpectationEvents(*rule.Events, result); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateMachineArtifactObservation(value operator.MachineArtifactObservation, result operator.MachineDetailResult) error {
	if value.Decoded && !value.Observed || value.Invalid && !value.Observed ||
		value.ObservedAt != nil && !value.Observed ||
		value.Observed && value.ObservedAt == nil && !value.Invalid {
		return errors.New("operator client: artifact observation metadata is inconsistent")
	}
	if err := validateMachineDetailClock(value.ObservedAt, "expectations.rule.observation.observed_at", result.EvaluatedAt); err != nil {
		return err
	}
	if err := validateOptionalMachineDetailText(value.Error, result.Disclosure.MaxTextBytes,
		"expectations.rule.observation.error"); err != nil {
		return err
	}
	if !value.Decoded || value.Invalid {
		if value.ReadFailed || value.Error != nil || value.Exists != nil || value.ModifiedAt != nil {
			return errors.New("operator client: absent or invalid artifact observation contains derived values")
		}
		return nil
	}
	if value.ReadFailed != (value.Error != nil) || value.ReadFailed && (value.Exists != nil || value.ModifiedAt != nil) ||
		!value.ReadFailed && value.Exists == nil ||
		value.Exists != nil && !*value.Exists && value.ModifiedAt != nil {
		return errors.New("operator client: decoded artifact observation metadata is inconsistent")
	}
	return validateMachineDetailEvidenceTime(value.ModifiedAt, "expectations.rule.observation.modified_at", result.EvaluatedAt)
}

func validateMachineExpectationEvents(value operator.MachineExpectationEvents, result operator.MachineDetailResult) error {
	if value.WindowSeconds <= 0 {
		return errors.New("operator client: machine expectation event window is invalid")
	}
	if err := validateMachineEventTypePage(value.FailureTypes, result.Disclosure.EventTypeLimit,
		result.Disclosure.MaxTextBytes); err != nil {
		return err
	}
	if value.FailureTypes.Total == 0 || value.Decoded && !value.Observed || value.Invalid && !value.Observed ||
		value.ObservedAt != nil && !value.Observed || value.Observed && value.ObservedAt == nil && !value.Invalid {
		return errors.New("operator client: event observation metadata is inconsistent")
	}
	if err := validateMachineDetailClock(value.ObservedAt, "expectations.rule.events.observed_at", result.EvaluatedAt); err != nil {
		return err
	}
	if err := validateOptionalMachineDetailText(value.Error, result.Disclosure.MaxTextBytes,
		"expectations.rule.events.error"); err != nil {
		return err
	}
	if !value.Decoded || value.Invalid {
		if value.ReadFailed || value.Error != nil || value.Partial || value.CoveredFrom != nil || value.Malformed != 0 ||
			!emptyMachineEventCountPage(value.Declared) || !emptyMachineEventCountPage(value.Undeclared) {
			return errors.New("operator client: absent or invalid event observation contains derived values")
		}
		return nil
	}
	if value.ReadFailed != (value.Error != nil) || value.Malformed < 0 {
		return errors.New("operator client: decoded event observation metadata is inconsistent")
	}
	if value.ReadFailed {
		if value.Partial || value.CoveredFrom != nil || value.Malformed != 0 ||
			!emptyMachineEventCountPage(value.Declared) || !emptyMachineEventCountPage(value.Undeclared) {
			return errors.New("operator client: failed event read contains measured values")
		}
		return nil
	}
	if value.Partial && value.CoveredFrom == nil {
		return errors.New("operator client: partial event observation lacks coverage boundary")
	}
	if err := validateMachineDetailEvidenceTime(value.CoveredFrom, "expectations.rule.events.covered_from", result.EvaluatedAt); err != nil {
		return err
	}
	if err := validateMachineEventCountPage(value.Declared, result); err != nil {
		return err
	}
	return validateMachineEventCountPage(value.Undeclared, result)
}

func validateMachineEventTypePage(page operator.MachineEventTypePage, limit, maxBytes int) error {
	if page.Items == nil || page.Total < 0 || page.Invalid < 0 || page.Invalid > page.Total ||
		len(page.Items) > limit || len(page.Items)+page.Invalid > page.Total ||
		page.Truncated != (len(page.Items)+page.Invalid < page.Total) {
		return errors.New("operator client: event failure type page metadata is inconsistent")
	}
	seen := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		if strings.TrimSpace(item.Text) == "" || seen[item.Text] {
			return errors.New("operator client: event failure type is empty or duplicated")
		}
		seen[item.Text] = true
		if err := validateMachineDetailText(item, maxBytes, "expectations.rule.events.type"); err != nil {
			return err
		}
	}
	return nil
}

func validateMachineEventCountPage(page operator.MachineEventCountPage, result operator.MachineDetailResult) error {
	if page.Items == nil || page.Total < 0 || page.Invalid < 0 || page.Invalid > page.Total ||
		len(page.Items) > result.Disclosure.EventTypeLimit || len(page.Items)+page.Invalid > page.Total ||
		page.Truncated != (len(page.Items)+page.Invalid < page.Total) {
		return errors.New("operator client: event count page metadata is inconsistent")
	}
	seen := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		if strings.TrimSpace(item.Type.Text) == "" || seen[item.Type.Text] || item.Count < 0 ||
			(item.Count == 0) != (item.LastAt == nil) {
			return errors.New("operator client: event count item is invalid")
		}
		seen[item.Type.Text] = true
		if err := validateMachineDetailText(item.Type, result.Disclosure.MaxTextBytes,
			"expectations.rule.events.type"); err != nil {
			return err
		}
		if err := validateMachineDetailEvidenceTime(item.LastAt, "expectations.rule.events.last_at", result.EvaluatedAt); err != nil {
			return err
		}
	}
	return nil
}

func emptyMachineEventCountPage(page operator.MachineEventCountPage) bool {
	return page.Total == 0 && page.Invalid == 0 && !page.Truncated && page.Items != nil && len(page.Items) == 0
}

func validateMachineDetailEvidenceTime(value *time.Time, name string, evaluatedAt time.Time) error {
	if err := validateMachineDetailTime(value, name, evaluatedAt.Add(state.ClockSkewTolerance), true); err != nil {
		return err
	}
	return nil
}

func validateMachineJudgement(result operator.MachineDetailResult) error {
	j := result.Judgement
	if !validMachineClientState(j.State) {
		return errors.New("operator client: machine judgement state is invalid")
	}
	if err := validateMachineDetailText(j.Reason, result.Disclosure.MaxTextBytes, "judgement.reason"); err != nil {
		return err
	}
	if j.Reason.Text == "" {
		return errors.New("operator client: machine judgement reason is empty")
	}
	if result.Item.RetiredAt == nil {
		if !j.AffectsFleetState || result.Item.State == nil || *result.Item.State != j.State {
			return errors.New("operator client: active machine judgement does not match fleet state")
		}
	} else if j.AffectsFleetState || result.Item.State != nil {
		return errors.New("operator client: retired machine judgement affects fleet state")
	}
	p := j.Findings
	if p.Items == nil || p.Total < 0 || p.Invalid < 0 || p.Invalid > p.Total ||
		len(p.Items) > result.Disclosure.FindingLimit || len(p.Items)+p.Invalid > p.Total ||
		p.Truncated != (len(p.Items)+p.Invalid < p.Total) {
		return errors.New("operator client: machine finding page metadata is inconsistent")
	}
	for _, finding := range p.Items {
		if !validMachineFindingKind(finding.Kind) || finding.Severity < 1 || finding.Severity > 4 {
			return errors.New("operator client: machine detail contains an invalid finding")
		}
		if finding.Message.Text == "" {
			return errors.New("operator client: machine finding message is empty")
		}
		if err := validateMachineDetailText(finding.Message, result.Disclosure.MaxTextBytes, "judgement.finding.message"); err != nil {
			return err
		}
	}
	return nil
}

func validateMachineMonitor(result operator.MachineDetailResult) error {
	m := result.Monitor
	if m.CheckinIntervalSeconds <= 0 ||
		m.EverCheckedIn != (m.LastCheckinReceivedAt != nil) ||
		m.EverCheckedIn != (m.ClockSkewSeconds != nil) ||
		(m.DistinctAgentStarts1h != nil && *m.DistinctAgentStarts1h < 0) ||
		(m.DistinctBootIDs1h != nil && *m.DistinctBootIDs1h < 0) ||
		(m.AgentUnitNRestarts != nil && *m.AgentUnitNRestarts < 0) {
		return errors.New("operator client: machine monitor metadata is inconsistent")
	}
	for _, clock := range []struct {
		name  string
		value *time.Time
	}{{"monitor.last_checkin_received_at", m.LastCheckinReceivedAt},
		{"monitor.last_observation_received_at", m.LastObservationReceivedAt}} {
		if err := validateMachineDetailTime(clock.value, clock.name, result.EvaluatedAt, true); err != nil {
			return err
		}
	}
	if result.Item.RetiredAt == nil && !sameOptionalTime(m.LastCheckinReceivedAt, result.Item.LastCheckinReceivedAt) {
		return errors.New("operator client: machine monitor and summary check-in clocks disagree")
	}
	if result.Item.RetiredAt == nil && !sameOptionalTime(m.LastObservationReceivedAt, result.Item.LastObservationReceivedAt) {
		return errors.New("operator client: machine monitor and summary observation clocks disagree")
	}
	return validateOptionalMachineDetailText(m.AgentVersion, result.Disclosure.MaxTextBytes, "monitor.agent_version")
}

func validateMachineCheckins(result operator.MachineDetailResult) error {
	p := result.Checkins
	if p.Items == nil || p.RowsSeen < 0 || p.Invalid < 0 || p.Invalid+len(p.Items) != p.RowsSeen ||
		p.RowsSeen > result.Disclosure.CheckinLimit || (p.Truncated && p.RowsSeen != result.Disclosure.CheckinLimit) {
		return errors.New("operator client: machine check-in page metadata is inconsistent")
	}
	windowStart := result.EvaluatedAt.Add(-time.Duration(result.Disclosure.CheckinWindowSeconds) * time.Second)
	var previous time.Time
	for _, item := range p.Items {
		if err := validateMachineDetailTime(&item.ReceivedAt, "checkin.received_at", result.EvaluatedAt, true); err != nil {
			return err
		}
		// Multiple queued check-ins can legitimately reach the Hub during the
		// same stored second. The DTO deliberately omits rowid, so only require
		// nondecreasing received_at order here; the service keeps the DB tie-break.
		if item.ReceivedAt.Before(windowStart) || (!previous.IsZero() && item.ReceivedAt.Before(previous)) {
			return errors.New("operator client: machine check-ins are outside the window or not ordered")
		}
		if err := validateMachineDetailTime(&item.SentAt, "checkin.sent_at", result.EvaluatedAt, false); err != nil {
			return err
		}
		if err := validateOptionalMachineDetailText(item.AgentVersion, result.Disclosure.MaxTextBytes, "monitor.agent_version"); err != nil {
			return err
		}
		if err := validateOptionalMachineDetailText(item.BootID, result.Disclosure.MaxTextBytes, "checkin.boot_id"); err != nil {
			return err
		}
		if optionalInt64Negative(item.AgentSeq) || optionalInt64Negative(item.UptimeSeconds) ||
			optionalInt64Negative(item.DiskFreeBytes) || optionalInt64Negative(item.DiskTotalBytes) ||
			optionalInt64Negative(item.ObservationAgeSeconds) ||
			(item.DiskFreeBytes == nil) != (item.DiskTotalBytes == nil) ||
			(item.DiskFreeBytes != nil && *item.DiskFreeBytes > *item.DiskTotalBytes) {
			return errors.New("operator client: machine check-in values are inconsistent")
		}
		previous = item.ReceivedAt
	}
	return nil
}

func validateMachineStateHistory(result operator.MachineDetailResult) error {
	p := result.StateHistory
	if p.Items == nil || p.RowsSeen < 0 || p.Invalid < 0 || p.Invalid+len(p.Items) != p.RowsSeen ||
		p.RowsSeen > result.Disclosure.StateHistoryLimit ||
		(p.Truncated && p.RowsSeen != result.Disclosure.StateHistoryLimit) {
		return errors.New("operator client: machine state history page metadata is inconsistent")
	}
	var previous time.Time
	for _, item := range p.Items {
		if !validMachineClientState(item.State) ||
			validateMachineDetailTime(&item.EnteredAt, "state_history.entered_at", result.EvaluatedAt, true) != nil ||
			(!previous.IsZero() && !item.EnteredAt.Before(previous)) {
			return errors.New("operator client: machine state history item is invalid or not ordered")
		}
		if item.LeftAt != nil {
			if err := validateMachineDetailTime(item.LeftAt, "state_history.left_at", result.EvaluatedAt, true); err != nil {
				return err
			}
			if item.LeftAt.Before(item.EnteredAt) {
				return errors.New("operator client: machine state history interval is inverted")
			}
		}
		if err := validateMachineDetailText(item.Reason, result.Disclosure.MaxTextBytes, "state_history.reason"); err != nil {
			return err
		}
		previous = item.EnteredAt
	}
	return nil
}

func validateMachineIdentity(result operator.MachineDetailResult) error {
	i := result.Identity
	if i.Observed != (i.ObservedAt != nil) || i.Decoded != (i.Value != nil) || i.Decoded && !i.Observed {
		return errors.New("operator client: machine identity observation metadata is inconsistent")
	}
	if err := validateMachineDetailClock(i.ObservedAt, "identity.observed_at", result.EvaluatedAt); err != nil {
		return err
	}
	if i.Value == nil {
		return nil
	}
	fields := []struct {
		name  string
		value operator.EvidenceText
	}{
		{"identity.hostname", i.Value.Hostname}, {"identity.os", i.Value.OS},
		{"identity.kernel", i.Value.Kernel}, {"identity.arch", i.Value.Arch},
		{"identity.unix_user", i.Value.UnixUser},
		{"identity.machine_id_hint", i.Value.MachineIDHint}, {"checkin.boot_id", i.Value.BootID},
	}
	for _, field := range fields {
		if err := validateMachineDetailText(field.value, result.Disclosure.MaxTextBytes, field.name); err != nil {
			return err
		}
	}
	return validateOptionalMachineDetailText(i.Value.TailscaleIP, result.Disclosure.MaxTextBytes, "identity.tailscale_ip")
}

func validateMachineIdentityHints(result operator.MachineDetailResult) error {
	p := result.IdentityHints
	if p.Items == nil || p.Total < 0 || p.Invalid < 0 || p.Invalid > p.Total ||
		len(p.Items) > result.Disclosure.IdentityHintLimit || len(p.Items)+p.Invalid > p.Total ||
		p.Truncated != (len(p.Items)+p.Invalid < p.Total) {
		return errors.New("operator client: machine identity hint page metadata is inconsistent")
	}
	for _, item := range p.Items {
		if item.ObservationCount < 0 || (item.ObservationCount == 0 && !item.RegistryDeclared) ||
			(item.ObservationCount > 0) != (item.FirstSeenAt != nil && item.LastSeenAt != nil) {
			return errors.New("operator client: machine identity hint metadata is inconsistent")
		}
		if err := validateMachineDetailText(item.Hint, result.Disclosure.MaxTextBytes, "identity.machine_id_hint"); err != nil {
			return err
		}
		if item.FirstSeenAt != nil {
			if err := validateMachineDetailTime(item.FirstSeenAt, "identity_hint.first_seen_at", result.EvaluatedAt, true); err != nil {
				return err
			}
			if err := validateMachineDetailTime(item.LastSeenAt, "identity_hint.last_seen_at", result.EvaluatedAt, true); err != nil {
				return err
			}
			if item.LastSeenAt.Before(*item.FirstSeenAt) {
				return errors.New("operator client: machine identity hint interval is inverted")
			}
		}
	}
	return nil
}

func validateMachineResources(result operator.MachineDetailResult) error {
	r := result.Resources
	if r.Observed != (r.ObservedAt != nil) || r.Decoded && !r.Observed || r.Invalid && !r.Decoded ||
		(r.Value != nil) != (r.Decoded && !r.Invalid) {
		return errors.New("operator client: machine resources observation metadata is inconsistent")
	}
	if err := validateMachineDetailClock(r.ObservedAt, "resources.observed_at", result.EvaluatedAt); err != nil {
		return err
	}
	if r.Value == nil {
		return nil
	}
	v := r.Value
	if v.DiskFreeBytes < 0 || v.DiskTotalBytes < 0 || v.DiskFreeBytes > v.DiskTotalBytes ||
		v.MemAvailableBytes < 0 || v.MemTotalBytes < 0 || v.MemAvailableBytes > v.MemTotalBytes ||
		!v.MemMeasured && (v.MemTotalBytes != 0 || v.MemAvailableBytes != 0) ||
		v.CPUCount < 0 || v.Load1m != nil && (*v.Load1m < 0 || math.IsNaN(*v.Load1m) || math.IsInf(*v.Load1m, 0)) {
		return errors.New("operator client: machine resource values are invalid")
	}
	return nil
}

func validateMachineDetailClock(value *operator.MachineEvidenceClock, name string, evaluatedAt time.Time) error {
	if value == nil {
		return nil
	}
	if err := validateMachineDetailTime(&value.MeasuredAt, name+".measured_at", evaluatedAt, false); err != nil {
		return err
	}
	return validateMachineDetailTime(&value.ReceivedAt, name+".received_at", evaluatedAt, true)
}

func validateMachineDetailTime(value *time.Time, name string, evaluatedAt time.Time, upperBound bool) error {
	if value == nil {
		return nil
	}
	if value.IsZero() {
		return fmt.Errorf("operator client: %s is zero", name)
	}
	if _, offset := value.Zone(); offset != 0 {
		return fmt.Errorf("operator client: %s must be UTC", name)
	}
	if upperBound && value.After(evaluatedAt) {
		return fmt.Errorf("operator client: %s is after evaluated_at", name)
	}
	return nil
}

func validateMachineDetailText(value operator.EvidenceText, maxBytes int, name string) error {
	policy, ok := machineDetailTextPolicies[name]
	if !ok {
		return fmt.Errorf("operator client: %s has no text policy", name)
	}
	return validateEvidenceText(value, maxBytes, name, policy)
}

func validateOptionalMachineDetailText(value *operator.EvidenceText, maxBytes int, name string) error {
	if value == nil {
		return nil
	}
	if value.Text == "" {
		return fmt.Errorf("operator client: %s optional text is empty", name)
	}
	return validateMachineDetailText(*value, maxBytes, name)
}

func optionalInt64Negative(value *int64) bool { return value != nil && *value < 0 }
func sameOptionalTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

func validMachineClientState(candidate state.State) bool {
	for _, valid := range state.AllStates {
		if candidate == valid {
			return true
		}
	}
	return false
}

func validateMachineSummary(item operator.MachineSummary, evaluatedAt time.Time) error {
	if strings.TrimSpace(item.MachineID) == "" || strings.TrimSpace(item.DisplayName) == "" ||
		item.CreatedAt.IsZero() || item.ChannelRevision < 0 || item.Issues == nil || item.AlteredFields == nil {
		return errors.New("operator client: machine summary is missing immutable identity fields")
	}
	if err := validateMachineClientText("response machine_id", item.MachineID, 256); err != nil {
		return err
	}
	if len(item.DisplayName) > 256 || !utf8.ValidString(item.DisplayName) {
		return errors.New("operator client: machine display_name is oversized or invalid UTF-8")
	}
	for _, char := range item.DisplayName {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return errors.New("operator client: machine display_name is not safe for presentation")
		}
	}
	if len(item.Issues) > 3 || len(item.AlteredFields) > 1 ||
		(len(item.Issues) == 0) != (len(item.AlteredFields) == 0) {
		return errors.New("operator client: machine projection issue evidence is inconsistent")
	}
	previousIssue := -1
	for _, issue := range item.Issues {
		issueOrder := -1
		switch issue {
		case "display_name_invalid_utf8":
			issueOrder = 0
		case "display_name_control_or_format_replaced":
			issueOrder = 1
		case "display_name_missing":
			issueOrder = 2
		case "display_name_truncated":
			issueOrder = 3
		}
		if issueOrder < 0 {
			return errors.New("operator client: machine projection issue is unknown")
		}
		if issueOrder <= previousIssue {
			return errors.New("operator client: machine projection issues are duplicated or not canonical")
		}
		previousIssue = issueOrder
	}
	if len(item.Issues) > 0 {
		if item.AlteredFields[0] != "display_name" {
			return errors.New("operator client: machine altered_fields is not canonical")
		}
	}
	for _, value := range []*time.Time{
		&item.CreatedAt, item.EnrolledAt, item.RetiredAt, item.StateSince,
		item.LastCheckinReceivedAt, item.LastObservationReceivedAt,
	} {
		if value != nil {
			if value.IsZero() {
				return errors.New("operator client: machine summary contains a zero timestamp")
			}
			if _, offset := value.Zone(); offset != 0 {
				return errors.New("operator client: machine summary timestamps must be UTC")
			}
		}
	}
	for _, evidence := range []struct {
		name  string
		value *time.Time
	}{
		{"state_since", item.StateSince},
		{"last_checkin_received_at", item.LastCheckinReceivedAt},
		{"last_observation_received_at", item.LastObservationReceivedAt},
	} {
		if evidence.value != nil && evidence.value.After(evaluatedAt) {
			return fmt.Errorf("operator client: machine %s is after evaluated_at", evidence.name)
		}
	}
	if item.Channel != nil && *item.Channel != "canary" && *item.Channel != "stable" {
		return fmt.Errorf("operator client: machine summary has invalid channel %q", *item.Channel)
	}
	if item.RetiredAt != nil {
		if item.State != nil || item.StateSince != nil || item.Reporting != nil ||
			item.LastCheckinReceivedAt != nil || item.LastObservationReceivedAt != nil {
			return errors.New("operator client: retired machine claims a live health evaluation")
		}
		return nil
	}
	if item.State == nil || item.Reporting == nil {
		return errors.New("operator client: active machine is missing health evaluation fields")
	}
	validState := false
	for _, candidate := range state.AllStates {
		if *item.State == candidate {
			validState = true
			break
		}
	}
	if !validState {
		return fmt.Errorf("operator client: machine summary has invalid state %q", *item.State)
	}
	reporting := *item.Reporting
	switch *item.State {
	case state.Online, state.Degraded:
		if !reporting || item.LastCheckinReceivedAt == nil {
			return errors.New("operator client: reporting state lacks a current check-in")
		}
	case state.Unreachable:
		if reporting || item.LastCheckinReceivedAt == nil {
			return errors.New("operator client: unreachable state has contradictory liveness")
		}
	case state.NeverReported:
		if reporting || item.LastCheckinReceivedAt != nil {
			return errors.New("operator client: never-reported state has contradictory check-in evidence")
		}
	case state.IdentityConflict:
		if reporting && item.LastCheckinReceivedAt == nil {
			return errors.New("operator client: reporting identity conflict lacks a check-in")
		}
	}
	return nil
}

func validateMachineStateCounts(counts []operator.MachineStateCount, total int, label string) error {
	if len(counts) != len(state.AllStates) {
		return fmt.Errorf("operator client: %s state counts are not exhaustive", label)
	}
	sum := 0
	for i, candidate := range state.AllStates {
		if counts[i].State != candidate || counts[i].Count < 0 {
			return fmt.Errorf("operator client: %s state counts are not canonical", label)
		}
		sum += counts[i].Count
	}
	if sum != total {
		return fmt.Errorf("operator client: %s state counts total %d does not match %d", label, sum, total)
	}
	return nil
}

func validateMachineEvaluationTime(value time.Time) error {
	if value.IsZero() {
		return errors.New("operator client: machine read evaluated_at is missing")
	}
	if _, offset := value.Zone(); offset != 0 {
		return errors.New("operator client: machine read evaluated_at must be UTC")
	}
	return nil
}

func validMachineFindingKind(kind string) bool {
	switch kind {
	case "agent", "clock", "config", "credential", "disk", "machine", "reachability", "workload":
		return true
	default:
		return false
	}
}
