package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

const maxMachineEvidenceResponseBytes int64 = 32 << 20

var machineEvidenceTextPolicies = map[string]evidenceTextFieldPolicy{
	"journal.unit":                 {maxBytes: 256, policy: evidenceTextStrict},
	"journal.err":                  {maxBytes: 4096, policy: evidenceTextBlock},
	"journal.shape.example":        {maxBytes: 4096, policy: evidenceTextBlock},
	"systemd_unit.name":            {maxBytes: 256, policy: evidenceTextStrict},
	"systemd_unit.active_state":    {maxBytes: 64, policy: evidenceTextStrict},
	"systemd_unit.sub_state":       {maxBytes: 64, policy: evidenceTextStrict},
	"systemd_unit.reason":          {maxBytes: 4096, policy: evidenceTextBlock},
	"credential.provider":          {maxBytes: 256, policy: evidenceTextStrict},
	"credential.note":              {maxBytes: 4096, policy: evidenceTextBlock},
	"credential.last_error":        {maxBytes: 4096, policy: evidenceTextBlock},
	"credential.peer.display_name": {maxBytes: 256, policy: evidenceTextStrict},
	"occupancy.provider":           {maxBytes: 256, policy: evidenceTextStrict},
	"occupancy.source":             {maxBytes: 64, policy: evidenceTextStrict},
	"occupancy.agent":              {maxBytes: 256, policy: evidenceTextStrict},
	"run_summary.job_id":           {maxBytes: 128, policy: evidenceTextStrict},
	"run_summary.status":           {maxBytes: 64, policy: evidenceTextStrict},
	"run_summary.summary":          {maxBytes: operator.MachineEvidenceMaxFieldBytes, policy: evidenceTextBlock},
	"openclaw.reason":              {maxBytes: 4096, policy: evidenceTextBlock},
	"openclaw.version":             {maxBytes: 256, policy: evidenceTextStrict},
	"openclaw.version_raw":         {maxBytes: 4096, policy: evidenceTextBlock},
	"openclaw.install.text":        {maxBytes: 4096, policy: evidenceTextBlock},
	"openclaw.install.kill_mode":   {maxBytes: 64, policy: evidenceTextStrict},
	"openclaw.db.reason":           {maxBytes: 4096, policy: evidenceTextBlock},
	"openclaw.db.layout":           {maxBytes: 64, policy: evidenceTextStrict},
	"openclaw.db.status":           {maxBytes: 64, policy: evidenceTextStrict},
	"cli_tool.name":                {maxBytes: 256, policy: evidenceTextStrict},
	"cli_tool.version":             {maxBytes: 256, policy: evidenceTextStrict},
	"cli_tool.version_raw":         {maxBytes: 4096, policy: evidenceTextBlock},
	"cli_tool.reason":              {maxBytes: 4096, policy: evidenceTextBlock},
}

func (c *Client) MachineEvidence(ctx context.Context, machineID string, limit int) (operator.MachineEvidenceResult, error) {
	if err := validateJobReadIdentifier("machine_id", machineID, 256); err != nil || !utf8.ValidString(machineID) ||
		strings.Contains(machineID, "/") || machineID == "." || machineID == ".." {
		return operator.MachineEvidenceResult{}, errors.New(
			"operator client: machine_id 不可為空、含控制字元、dot segment 或斜線")
	}
	effectiveLimit := limit
	if effectiveLimit == 0 {
		effectiveLimit = operator.MachineEvidenceDefaultLimit
	} else if effectiveLimit < 1 || effectiveLimit > store.MaxMachineEvidencePageSize {
		return operator.MachineEvidenceResult{}, fmt.Errorf(
			"operator client: machine evidence limit must be between 1 and %d", store.MaxMachineEvidencePageSize)
	}
	path := "/v1/operator/machines/" + url.PathEscape(machineID) + "/evidence"
	if limit != 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.MachineEvidenceResult{}, err
	}
	response, err := c.doRawWithLimit(req, maxMachineEvidenceResponseBytes, "32 MiB")
	if err != nil {
		return operator.MachineEvidenceResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.MachineEvidenceResult{}, fmt.Errorf(
			"operator client: machine evidence returned unexpected success status HTTP %d", response.status)
	}
	if err := validateMachineReadHeaders(response.header); err != nil {
		return operator.MachineEvidenceResult{}, err
	}
	var result operator.MachineEvidenceResult
	if err := decodeStrictJSONDocument(response.body, "machine evidence", &result); err != nil {
		return operator.MachineEvidenceResult{}, err
	}
	if err := validateMachineEvidenceResult(result, machineID, effectiveLimit); err != nil {
		return operator.MachineEvidenceResult{}, err
	}
	return result, nil
}

func validateMachineEvidenceResult(result operator.MachineEvidenceResult, machineID string, limit int) error {
	if result.SchemaVersion != operator.MachineEvidenceSchemaVersion {
		return fmt.Errorf("operator client: unsupported machine evidence schema_version %d", result.SchemaVersion)
	}
	if err := validateMachineEvaluationTime(result.EvaluatedAt); err != nil {
		return err
	}
	if result.MachineID != machineID || validateJobReadIdentifier("evidence.machine_id", result.MachineID, 256) != nil ||
		validateJobReadIdentifier("evidence.display_name", result.DisplayName, 256) != nil {
		return errors.New("operator client: machine evidence identity is inconsistent")
	}
	d := result.Disclosure
	if d.Limit != limit || d.Limit < 1 || d.Limit > store.MaxMachineEvidencePageSize ||
		d.MaxFieldBytes != operator.MachineEvidenceMaxFieldBytes || d.MaxShapes != operator.MachineEvidenceMaxShapes ||
		d.MaxCredentialPeers != operator.MachineEvidenceMaxCredentialPeers ||
		d.MaxOccupancyAgents != operator.MachineEvidenceMaxOccupancyAgents ||
		d.IndependentVerifier || d.StatusIsOutcome || d.TerminalOutcomeRecorded ||
		d.SystemdStateIsWorkOutcome || !d.SystemdMainPIDExcluded ||
		!d.HostPathFieldsExcluded || !d.ProcessIDFieldsExcluded || !d.OpenClawDatabaseLocationsExcluded ||
		d.EvidenceTextPathRedactedByHub || d.CLIOpenClawTextRedactedByAgent || d.CLIOpenClawTextRedactedByHub ||
		d.CLIVersionSourcesCollapsed || d.RawVersionTextParsedByHub || !d.RunningRelationshipDerivedFromPaths ||
		d.CredentialStatusIsSessionValidity ||
		!d.CredentialActiveAccountIDExcluded || !d.CredentialSecretFieldsExcluded ||
		d.CredentialTextRedactedByAgent || d.CredentialPeerIsSameTicket ||
		!d.OccupancyAggregatedByHub || d.OccupancyProcessStateUsed || d.OccupancyProviderNormalized ||
		d.OccupancyErrorsCategorized || !d.OccupancyProfileIDExcluded || !d.OccupancyEventDetailsExcluded ||
		!d.JournalSecretShapesRedactedByAgent || d.RunSummarySecretShapesRedactedByAgent {
		return errors.New("operator client: machine evidence disclosure metadata is inconsistent")
	}
	if err := validateMachineEvidenceProducer(d.JournalProducer, operator.MachineEvidenceProducerObserverAgent,
		operator.MachineEvidenceAuthorityMachineBearer, result); err != nil {
		return err
	}
	if err := validateMachineEvidenceProducer(d.SystemdProducer, operator.MachineEvidenceProducerObserverAgent,
		operator.MachineEvidenceAuthorityMachineBearer, result); err != nil {
		return err
	}
	if err := validateMachineEvidenceProducer(d.OpenClawProducer, operator.MachineEvidenceProducerObserverAgent,
		operator.MachineEvidenceAuthorityMachineBearer, result); err != nil {
		return err
	}
	if err := validateMachineEvidenceProducer(d.CLIToolProducer, operator.MachineEvidenceProducerObserverAgent,
		operator.MachineEvidenceAuthorityMachineBearer, result); err != nil {
		return err
	}
	if err := validateMachineEvidenceProducer(d.CredentialProducer, operator.MachineEvidenceProducerObserverAgent,
		operator.MachineEvidenceAuthorityMachineBearer, result); err != nil {
		return err
	}
	if err := validateMachineEvidenceProducer(d.OccupancyProducer, operator.MachineEvidenceProducerOpenClawTask,
		"", result); err != nil {
		return err
	}
	if err := validateMachineEvidenceProducer(d.OccupancyRelay, operator.MachineEvidenceProducerObserverAgent,
		operator.MachineEvidenceAuthorityMachineBearer, result); err != nil {
		return err
	}
	if err := validateMachineEvidenceProducer(d.RunSummaryProducer, operator.MachineEvidenceProducerOpenClawTask,
		"", result); err != nil {
		return err
	}
	if err := validateMachineEvidenceProducer(d.RunSummaryRelay, operator.MachineEvidenceProducerObserverAgent,
		operator.MachineEvidenceAuthorityMachineBearer, result); err != nil {
		return err
	}
	if result.CLITools.Items == nil || result.Credentials.Items == nil || result.Occupancy.Items == nil ||
		result.SystemdUnits.Items == nil || result.RunSummaries.Items == nil || result.Journals.Items == nil ||
		result.Journals.UnitsWithoutJournal == nil {
		return errors.New("operator client: machine evidence arrays must not be null")
	}
	if err := validateMachineOpenClawEvidence(result.OpenClaw, d, limit); err != nil {
		return err
	}
	if err := validateMachineCLIToolPage(result.CLITools, d, limit); err != nil {
		return err
	}
	if err := validateMachineCredentialPage(result.Credentials, d, limit); err != nil {
		return err
	}
	remoteValidationPerformed := false
	for _, credential := range result.Credentials.Items {
		remoteValidationPerformed = remoteValidationPerformed ||
			(credential.VerificationMethod == model.VerifyLiveRequest && credential.VerifiedAt != nil)
	}
	if (!d.CredentialRemoteValidationPerformed && remoteValidationPerformed) ||
		(d.CredentialRemoteValidationPerformed && !remoteValidationPerformed && !result.Credentials.Truncated) {
		return errors.New("operator client: machine credential validation disclosure is inconsistent")
	}
	if err := validateMachineOccupancyPage(result.Occupancy, d, limit); err != nil {
		return err
	}
	if result.SystemdUnits.Invalid < 0 || !validBoundedJobEvidencePageForLimit(
		result.SystemdUnits.Total, len(result.SystemdUnits.Items), result.SystemdUnits.Truncated, limit) {
		return errors.New("operator client: machine systemd-unit page totals are inconsistent")
	}
	previousSystemdUnit := ""
	for i, unit := range result.SystemdUnits.Items {
		if unit.NRestarts < 0 || unit.MeasuredAt.IsZero() || unit.ReceivedAt.IsZero() ||
			(i > 0 && unit.Name.Text <= previousSystemdUnit) {
			return errors.New("operator client: machine systemd-unit metadata is inconsistent")
		}
		for _, field := range []struct {
			name string
			text operator.EvidenceText
		}{{"systemd_unit.name", unit.Name}, {"systemd_unit.active_state", unit.ActiveState},
			{"systemd_unit.sub_state", unit.SubState}} {
			if err := validateMachineEvidenceText(field.text, d.MaxFieldBytes, field.name); err != nil {
				return err
			}
		}
		if unit.Reason != nil {
			if err := validateMachineEvidenceText(*unit.Reason, d.MaxFieldBytes, "systemd_unit.reason"); err != nil {
				return err
			}
		}
		if unit.Name.Text == "" {
			return errors.New("operator client: machine systemd-unit name is empty")
		}
		if err := requireUTCJobTime(unit.MeasuredAt, "systemd_unit.measured_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(unit.ReceivedAt, "systemd_unit.received_at"); err != nil {
			return err
		}
		if unit.ActiveEnterTimestamp != nil {
			if unit.ActiveEnterTimestamp.IsZero() {
				return errors.New("operator client: machine systemd-unit active_enter_timestamp is zero")
			}
			if err := requireUTCJobTime(*unit.ActiveEnterTimestamp, "systemd_unit.active_enter_timestamp"); err != nil {
				return err
			}
		}
		previousSystemdUnit = unit.Name.Text
	}
	if !validBoundedJobEvidencePageForLimit(result.RunSummaries.Total, len(result.RunSummaries.Items),
		result.RunSummaries.Truncated, limit) {
		return errors.New("operator client: machine run-summary page totals are inconsistent")
	}
	if result.RunSummaries.DBObserved && !result.RunSummaries.Observed ||
		result.RunSummaries.Observed != (result.RunSummaries.ObservedAt != nil) ||
		(!result.RunSummaries.DBObserved && result.RunSummaries.Total != 0) {
		return errors.New("operator client: machine run-summary observation state is inconsistent")
	}
	if result.RunSummaries.ObservedAt != nil {
		if result.RunSummaries.ObservedAt.MeasuredAt.IsZero() || result.RunSummaries.ObservedAt.ReceivedAt.IsZero() {
			return errors.New("operator client: machine run-summary observation clocks are missing")
		}
		if err := requireUTCJobTime(result.RunSummaries.ObservedAt.MeasuredAt, "run_summaries.observed_at.measured_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(result.RunSummaries.ObservedAt.ReceivedAt, "run_summaries.observed_at.received_at"); err != nil {
			return err
		}
	}
	for _, summary := range result.RunSummaries.Items {
		for _, field := range []struct {
			name string
			text operator.EvidenceText
		}{{"run_summary.job_id", summary.JobID}, {"run_summary.status", summary.Status}, {"run_summary.summary", summary.Summary}} {
			if err := validateMachineEvidenceText(field.text, d.MaxFieldBytes, field.name); err != nil {
				return err
			}
		}
		if summary.ReportedAt != nil {
			if summary.ReportedAt.IsZero() {
				return errors.New("operator client: run summary reported_at is zero")
			}
			if err := requireUTCJobTime(*summary.ReportedAt, "run_summary.reported_at"); err != nil {
				return err
			}
		}
	}
	if result.Journals.Undecodable < 0 || result.Journals.UnitsWithoutJournalTotal < 0 ||
		result.Journals.UnitsWithoutJournalTotal < len(result.Journals.UnitsWithoutJournal) ||
		!validBoundedJobEvidencePageForLimit(result.Journals.Total, len(result.Journals.Items),
			result.Journals.Truncated, limit) ||
		!validBoundedJobEvidencePageForLimit(result.Journals.UnitsWithoutJournalTotal,
			len(result.Journals.UnitsWithoutJournal), result.Journals.UnitsWithoutJournalTruncated, limit) {
		return errors.New("operator client: machine journal page totals are inconsistent")
	}
	journalUnits := make(map[string]bool, len(result.Journals.Items))
	for _, journal := range result.Journals.Items {
		if journal.MeasuredAt.IsZero() || journal.ReceivedAt.IsZero() || len(journal.Top) > d.MaxShapes ||
			(journal.TopTruncated && len(journal.Top) != d.MaxShapes) {
			return errors.New("operator client: machine journal metadata is inconsistent")
		}
		if err := requireUTCJobTime(journal.MeasuredAt, "journal.measured_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(journal.ReceivedAt, "journal.received_at"); err != nil {
			return err
		}
		if err := validateMachineEvidenceText(journal.Unit, d.MaxFieldBytes, "journal.unit"); err != nil {
			return err
		}
		journalUnits[journal.Unit.Text] = true
		if journal.ReadFailed != (journal.Err != nil) {
			return errors.New("operator client: machine journal read failure is inconsistent")
		}
		if journal.Err != nil {
			if err := validateMachineEvidenceText(*journal.Err, d.MaxFieldBytes, "journal.err"); err != nil {
				return err
			}
		}
		for _, shape := range journal.Top {
			if err := validateMachineEvidenceText(shape.Example, d.MaxFieldBytes, "journal.shape.example"); err != nil {
				return err
			}
		}
	}
	previous := ""
	for i, unit := range result.Journals.UnitsWithoutJournal {
		if err := validateMachineEvidenceText(unit, d.MaxFieldBytes, "journal.unit"); err != nil {
			return err
		}
		if journalUnits[unit.Text] || (i > 0 && unit.Text < previous) {
			return errors.New("operator client: units_without_journal is not sorted or disjoint")
		}
		previous = unit.Text
	}
	return nil
}

func validateMachineOpenClawEvidence(value operator.MachineOpenClawEvidence,
	disclosure operator.MachineEvidenceDisclosure, limit int,
) error {
	if value.Observed != (value.ObservedAt != nil) || value.Decoded && !value.Observed || value.CrashBundles < 0 ||
		(value.Install != nil && value.InstallInvalid) || (value.DB != nil && value.DBInvalid) {
		return errors.New("operator client: machine OpenClaw observation metadata is inconsistent")
	}
	if !value.Decoded && (value.Present || value.Reason != nil || value.CLIVersion != nil ||
		value.CLIVersionRaw != nil || value.GatewayVersion != nil || value.UpstreamVersion != nil ||
		value.CrashBundles != 0 || value.Install != nil || value.InstallInvalid || value.DB != nil || value.DBInvalid) {
		return errors.New("operator client: undecoded machine OpenClaw observation carries facts")
	}
	if value.ObservedAt != nil {
		if err := validateMachineEvidenceClock(*value.ObservedAt, "openclaw.observed_at"); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name string
		text *operator.EvidenceText
	}{{"openclaw.reason", value.Reason}, {"openclaw.version", value.CLIVersion},
		{"openclaw.version_raw", value.CLIVersionRaw}, {"openclaw.version", value.GatewayVersion},
		{"openclaw.version", value.UpstreamVersion}} {
		if err := validateOptionalMachineEvidenceText(field.text, disclosure.MaxFieldBytes, field.name); err != nil {
			return err
		}
	}
	if value.Install != nil {
		i := value.Install
		if i.DropInCount < 0 || (i.NRestarts != nil && *i.NRestarts < 0) || i.DiskFreeBytes < 0 ||
			(!i.DiskFreeMeasured && i.DiskFreeBytes != 0) ||
			(!i.NpmObserved && i.NpmVersion != nil) ||
			(!i.RunningDirectoryObserved && (i.RunningDirectoryExists || i.RunningDirectoryWritable != nil || i.RunningVersion != nil)) ||
			(!i.RunningDirectoryExists && (i.RunningDirectoryWritable != nil || i.RunningVersion != nil)) ||
			(i.ProcessMatchesUnit != nil && !i.ProcessObserved) ||
			(!i.ReleaseLayoutObserved && (i.ReleasesPresent || i.CurrentReleaseLinked)) {
			return errors.New("operator client: machine OpenClaw install metadata is inconsistent")
		}
		if err := validateOptionalMachineEvidenceTime(i.ActiveEnterAt, "openclaw.install.active_enter_at"); err != nil {
			return err
		}
		for _, field := range []struct {
			name string
			text *operator.EvidenceText
		}{{"openclaw.install.text", i.UnitReason}, {"openclaw.install.kill_mode", i.KillMode},
			{"openclaw.version", i.NodeVersion}, {"openclaw.install.text", i.NodeVersionReason},
			{"openclaw.version", i.NpmVersion}, {"openclaw.install.text", i.NpmReason},
			{"openclaw.version", i.RunningVersion}, {"openclaw.install.text", i.RunningDirectoryReason},
			{"openclaw.install.text", i.ProcessReason}, {"openclaw.install.text", i.DiskFreeReason}} {
			if err := validateOptionalMachineEvidenceText(field.text, disclosure.MaxFieldBytes, field.name); err != nil {
				return err
			}
		}
	}
	if value.DB != nil {
		db := value.DB
		if db.TaskStatuses.Items == nil || db.CronStatuses.Items == nil ||
			!validMachineEvidenceSupport(db.Support) || !validMachineEvidenceOpenClawLayout(db.Layout) ||
			db.UnknownLocationCount < 0 || db.TaskRunRows < 0 ||
			db.CronRunLogRows < 0 || db.CronJobsTotal < 0 || db.CronJobsEnabled < 0 ||
			db.CronJobsOverdue < 0 || db.TerminalOutcomePopulated < 0 ||
			(db.Support == model.SupportUnsupported && (db.Present || db.UnknownLocationCount == 0)) ||
			(!db.CronJobsTotalMeasured && db.CronJobsTotal != 0) ||
			(!db.CronJobsEnabledMeasured && db.CronJobsEnabled != 0) ||
			(db.CronJobsEnabledMeasured && !db.CronJobsTotalMeasured) ||
			(db.CronJobsTotalMeasured && db.CronJobsEnabledMeasured && db.CronJobsEnabled > db.CronJobsTotal) ||
			(!db.CronJobsScheduleMeasured && (db.NextCronRunAt != nil || db.CronJobsOverdue != 0)) {
			return errors.New("operator client: machine OpenClaw database metadata is inconsistent")
		}
		for name, at := range map[string]*time.Time{
			"last_task_ended_at": db.LastTaskEndedAt, "last_cron_run_at": db.LastCronRunAt,
			"next_cron_run_at": db.NextCronRunAt,
		} {
			if err := validateOptionalMachineEvidenceTime(at, "openclaw.db."+name); err != nil {
				return err
			}
		}
		if err := validateOptionalMachineEvidenceText(db.Reason, disclosure.MaxFieldBytes, "openclaw.db.reason"); err != nil {
			return err
		}
		if err := validateOptionalMachineEvidenceText(db.Layout, disclosure.MaxFieldBytes, "openclaw.db.layout"); err != nil {
			return err
		}
		if err := validateMachineStatusCountPage(db.TaskStatuses, disclosure, limit, "openclaw.db.task_statuses"); err != nil {
			return err
		}
		if err := validateMachineStatusCountPage(db.CronStatuses, disclosure, limit, "openclaw.db.cron_statuses"); err != nil {
			return err
		}
	}
	return nil
}

func validateMachineStatusCountPage(page operator.MachineStatusCountPage,
	disclosure operator.MachineEvidenceDisclosure, limit int, name string,
) error {
	if page.Invalid < 0 || !validBoundedJobEvidencePageForLimit(page.Total, len(page.Items), page.Truncated, limit) {
		return fmt.Errorf("operator client: %s page totals are inconsistent", name)
	}
	previous := ""
	for index, item := range page.Items {
		if item.Count < 0 || item.Status.Text == "" || (index > 0 && item.Status.Text <= previous) {
			return fmt.Errorf("operator client: %s item metadata is inconsistent", name)
		}
		if err := validateMachineEvidenceText(item.Status, disclosure.MaxFieldBytes, "openclaw.db.status"); err != nil {
			return err
		}
		previous = item.Status.Text
	}
	return nil
}

func validateMachineCLIToolPage(page operator.MachineCLIToolPage,
	disclosure operator.MachineEvidenceDisclosure, limit int,
) error {
	if page.Invalid < 0 || !validBoundedJobEvidencePageForLimit(page.Total, len(page.Items), page.Truncated, limit) {
		return errors.New("operator client: machine CLI-tool page totals are inconsistent")
	}
	previous := ""
	for index, tool := range page.Items {
		sourcesDisagree := tool.VersionReported != nil && tool.VersionPackageJSON != nil &&
			tool.VersionReported.Text != tool.VersionPackageJSON.Text
		if tool.Name.Text == "" || (index > 0 && tool.Name.Text <= previous) ||
			!validMachineEvidencePresentEvidence(tool.PresentEvidence) ||
			!validMachineEvidencePathSource(tool.PathSource) || !validMachineEvidenceDaemonReach(tool.DaemonReach) ||
			!validMachineEvidenceProcessScan(tool.ProcessScan) ||
			!validMachineEvidenceRunningRelationship(tool.RunningRelationship) ||
			!validMachineEvidenceSupport(tool.Support) || tool.MeasuredAt.IsZero() || tool.ReceivedAt.IsZero() ||
			(!tool.Present && (tool.OnPath || tool.PresentEvidence != "" || tool.ProcessObserved)) ||
			(tool.OnPath && !tool.Present) ||
			(tool.PresentEvidence == "path" && (!tool.Present || !tool.OnPath)) ||
			(tool.PresentEvidence == "process" && (!tool.Present || tool.OnPath || !tool.ProcessObserved)) ||
			(tool.RunningRelationship != "" && (!tool.Present || !tool.ProcessObserved)) ||
			(tool.SourcesDisagree && (tool.VersionReported == nil || tool.VersionPackageJSON == nil)) ||
			(!tool.SourcesDisagree && sourcesDisagree) ||
			(tool.Support == model.SupportUnsupported && tool.VersionRaw == nil) {
			return errors.New("operator client: machine CLI-tool metadata is inconsistent")
		}
		if err := validateMachineEvidenceText(tool.Name, disclosure.MaxFieldBytes, "cli_tool.name"); err != nil {
			return err
		}
		for _, field := range []struct {
			name string
			text *operator.EvidenceText
		}{{"cli_tool.reason", tool.PathReason}, {"cli_tool.version", tool.VersionReported},
			{"cli_tool.version_raw", tool.VersionRaw}, {"cli_tool.version", tool.VersionPackageJSON},
			{"cli_tool.reason", tool.VersionReason}, {"cli_tool.reason", tool.RunningReason}} {
			if err := validateOptionalMachineEvidenceText(field.text, disclosure.MaxFieldBytes, field.name); err != nil {
				return err
			}
		}
		if err := requireUTCJobTime(tool.MeasuredAt, "cli_tool.measured_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(tool.ReceivedAt, "cli_tool.received_at"); err != nil {
			return err
		}
		previous = tool.Name.Text
	}
	return nil
}

func validateOptionalMachineEvidenceText(value *operator.EvidenceText, disclosureMaxBytes int, name string) error {
	if value == nil {
		return nil
	}
	return validateMachineEvidenceText(*value, disclosureMaxBytes, name)
}

func validateMachineEvidenceClock(value operator.MachineEvidenceClock, name string) error {
	if value.MeasuredAt.IsZero() || value.ReceivedAt.IsZero() {
		return fmt.Errorf("operator client: %s clocks are missing", name)
	}
	if err := requireUTCJobTime(value.MeasuredAt, name+".measured_at"); err != nil {
		return err
	}
	return requireUTCJobTime(value.ReceivedAt, name+".received_at")
}

func validMachineEvidencePresentEvidence(value string) bool {
	return value == "" || value == "path" || value == "process"
}

func validMachineEvidencePathSource(value string) bool {
	return value == "" || value == model.PathSourceLogin || value == model.PathSourceDaemon
}

func validMachineEvidenceDaemonReach(value string) bool {
	return value == "" || value == model.DaemonReachSame || value == model.DaemonReachShadowed ||
		value == model.DaemonReachMissing
}

func validMachineEvidenceProcessScan(value string) bool {
	return value == "" || value == model.ProcessScanComplete || value == model.ProcessScanRestricted ||
		value == model.ProcessScanUnavailable
}

func validMachineEvidenceRunningRelationship(value string) bool {
	return value == "" || value == "same" || value == "different"
}

func validMachineEvidenceSupport(value model.SupportLevel) bool {
	return value == "" || value == model.SupportOK || value == model.SupportUnsupported
}

func validMachineEvidenceOpenClawLayout(value *operator.EvidenceText) bool {
	return value == nil || value.Text == "consolidated" || value.Text == "split"
}

func validateMachineCredentialPage(page operator.MachineCredentialPage,
	disclosure operator.MachineEvidenceDisclosure, limit int,
) error {
	if page.Invalid < 0 || !validBoundedJobEvidencePageForLimit(page.Total, len(page.Items), page.Truncated, limit) {
		return errors.New("operator client: machine credential page totals are inconsistent")
	}
	previousProvider := ""
	for i, credential := range page.Items {
		if err := validateMachineEvidenceText(credential.Provider, disclosure.MaxFieldBytes, "credential.provider"); err != nil {
			return err
		}
		if credential.Provider.Text == "" || (i > 0 && credential.Provider.Text <= previousProvider) ||
			!validMachineCredentialStatus(credential.Status) ||
			!validMachineCredentialVerifyMethod(credential.VerificationMethod) || credential.AccountCount < 0 ||
			credential.LifetimeSeconds < 0 || credential.RefreshesSeen < 0 || credential.WatchedForSeconds < 0 ||
			!validBoundedJobEvidencePageForLimit(credential.PeersTotal, len(credential.Peers),
				credential.PeersTruncated, disclosure.MaxCredentialPeers) {
			return errors.New("operator client: machine credential metadata is inconsistent")
		}
		if credential.VerifiedAt != nil && credential.VerificationMethod != model.VerifyLiveRequest ||
			credential.VerificationMethod == model.VerifyLiveRequest && credential.VerifiedAt == nil {
			return errors.New("operator client: machine credential verification is inconsistent")
		}
		for name, value := range map[string]*time.Time{
			"expires_at": credential.ExpiresAt, "last_refresh": credential.LastRefresh,
			"file_mtime": credential.FileMTime, "verified_at": credential.VerifiedAt,
		} {
			if err := validateOptionalMachineEvidenceTime(value, "credential."+name); err != nil {
				return err
			}
		}
		if credential.MeasuredAt.IsZero() || credential.ReceivedAt.IsZero() {
			return errors.New("operator client: machine credential observation clocks are missing")
		}
		if err := requireUTCJobTime(credential.MeasuredAt, "credential.measured_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(credential.ReceivedAt, "credential.received_at"); err != nil {
			return err
		}
		if credential.Note != nil {
			if err := validateMachineEvidenceText(*credential.Note, disclosure.MaxFieldBytes, "credential.note"); err != nil {
				return err
			}
		}
		if credential.LastError != nil {
			if err := validateMachineEvidenceText(*credential.LastError, disclosure.MaxFieldBytes, "credential.last_error"); err != nil {
				return err
			}
		}
		for peerIndex, peer := range credential.Peers {
			if err := validateMachineEvidenceText(peer.DisplayName, disclosure.MaxFieldBytes,
				"credential.peer.display_name"); err != nil {
				return err
			}
			if peer.DisplayName.Text == "" || peer.RefreshesSeen <= 0 {
				return errors.New("operator client: machine credential peer metadata is inconsistent")
			}
			if err := validateOptionalMachineEvidenceTime(peer.FileMTime, "credential.peer.file_mtime"); err != nil {
				return err
			}
			if peerIndex > 0 && machineCredentialPeerBefore(peer, credential.Peers[peerIndex-1]) {
				return errors.New("operator client: machine credential peers are not in canonical order")
			}
		}
		previousProvider = credential.Provider.Text
	}
	return nil
}

func validateMachineOccupancyPage(page operator.MachineOccupancyPage,
	disclosure operator.MachineEvidenceDisclosure, limit int,
) error {
	if page.Invalid < 0 || !validBoundedJobEvidencePageForLimit(page.Total, len(page.Items), page.Truncated, limit) ||
		page.DBObserved && !page.Observed || page.Observed != (page.ObservedAt != nil) ||
		page.RowsSeen < 0 || page.RowsWithoutProvider < 0 || page.RowsWithoutProvider > page.RowsSeen ||
		(!page.DBObserved && (page.RowsSeen != 0 || page.RowsWithoutProvider != 0 || page.ObservationInvalid)) ||
		(page.ObservationInvalid && (page.RowsSeen != 0 || page.RowsWithoutProvider != 0)) {
		return errors.New("operator client: machine occupancy page metadata is inconsistent")
	}
	if page.ObservedAt != nil {
		if page.ObservedAt.MeasuredAt.IsZero() || page.ObservedAt.ReceivedAt.IsZero() {
			return errors.New("operator client: machine occupancy observation clocks are missing")
		}
		if err := requireUTCJobTime(page.ObservedAt.MeasuredAt, "occupancy.observed_at.measured_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(page.ObservedAt.ReceivedAt, "occupancy.observed_at.received_at"); err != nil {
			return err
		}
	}
	previousProvider, previousSource := "", ""
	for i, occupancy := range page.Items {
		if err := validateMachineEvidenceText(occupancy.Provider, disclosure.MaxFieldBytes, "occupancy.provider"); err != nil {
			return err
		}
		if err := validateMachineEvidenceText(occupancy.Source, disclosure.MaxFieldBytes, "occupancy.source"); err != nil {
			return err
		}
		if occupancy.Provider.Text == "" || occupancy.Source.Text == "" || occupancy.Runs <= 0 ||
			occupancy.Errors < 0 || occupancy.Errors > occupancy.Runs || occupancy.LastRunAt.IsZero() ||
			occupancy.LastReceivedAt.IsZero() ||
			!validBoundedJobEvidencePageForLimit(occupancy.AgentsTotal, len(occupancy.Agents),
				occupancy.AgentsTruncated, disclosure.MaxOccupancyAgents) ||
			(i > 0 && (occupancy.Provider.Text < previousProvider ||
				(occupancy.Provider.Text == previousProvider && occupancy.Source.Text <= previousSource))) {
			return errors.New("operator client: machine occupancy item metadata is inconsistent")
		}
		if err := requireUTCJobTime(occupancy.LastRunAt, "occupancy.last_run_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(occupancy.LastReceivedAt, "occupancy.last_received_at"); err != nil {
			return err
		}
		for _, agent := range occupancy.Agents {
			if err := validateMachineEvidenceText(agent, disclosure.MaxFieldBytes, "occupancy.agent"); err != nil {
				return err
			}
			if agent.Text == "" {
				return errors.New("operator client: machine occupancy agent is empty")
			}
		}
		previousProvider, previousSource = occupancy.Provider.Text, occupancy.Source.Text
	}
	return nil
}

func validMachineCredentialStatus(status model.CredStatus) bool {
	switch status {
	case model.CredAbsent, model.CredConfigured, model.CredExpiresSoon,
		model.CredExpired, model.CredUnknown, model.CredFailed:
		return true
	default:
		return false
	}
}

func validMachineCredentialVerifyMethod(method model.VerifyMethod) bool {
	return method == "" || method == model.VerifyFileParse || method == model.VerifyLiveRequest
}

func validateOptionalMachineEvidenceTime(value *time.Time, name string) error {
	if value == nil {
		return nil
	}
	if value.IsZero() {
		return fmt.Errorf("operator client: %s is zero", name)
	}
	return requireUTCJobTime(*value, name)
}

func machineCredentialPeerBefore(a, b operator.MachineCredentialPeer) bool {
	if a.RefreshesSeen != b.RefreshesSeen {
		return a.RefreshesSeen > b.RefreshesSeen
	}
	switch {
	case a.FileMTime != nil && b.FileMTime == nil:
		return true
	case a.FileMTime == nil && b.FileMTime != nil:
		return false
	case a.FileMTime != nil && b.FileMTime != nil && !a.FileMTime.Equal(*b.FileMTime):
		return a.FileMTime.After(*b.FileMTime)
	default:
		return a.DisplayName.Text < b.DisplayName.Text
	}
}

func validateMachineEvidenceProducer(producer operator.MachineEvidenceProducer, kind, authority string,
	result operator.MachineEvidenceResult,
) error {
	if producer.Kind != kind || producer.Authority != authority || producer.MachineID != result.MachineID ||
		producer.DisplayName != result.DisplayName {
		return errors.New("operator client: machine evidence producer provenance is inconsistent")
	}
	return nil
}

func validateMachineEvidenceText(value operator.EvidenceText, disclosureMaxBytes int, name string) error {
	field, ok := machineEvidenceTextPolicies[name]
	if !ok {
		return fmt.Errorf("operator client: %s has no text policy", name)
	}
	return validateEvidenceText(value, disclosureMaxBytes, name, field)
}
