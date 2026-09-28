package operator

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	MachineEvidenceSchemaVersion          = 6
	MachineEvidenceMaxFieldBytes          = 16 << 10
	MachineEvidenceDefaultLimit           = store.MaxMachineEvidencePageSize
	MachineEvidenceMaxShapes              = 10
	MachineEvidenceMaxCredentialPeers     = store.MaxMachineEvidenceCredentialPeers
	MachineEvidenceMaxOccupancyAgents     = store.MaxMachineEvidenceOccupancyAgents
	MachineEvidenceProducerObserverAgent  = "observer_agent"
	MachineEvidenceProducerOpenClawTask   = "openclaw_task_log"
	MachineEvidenceAuthorityMachineBearer = "machine_bearer"
)

type machineEvidenceTextField string

const (
	machineEvidenceJournalUnit         machineEvidenceTextField = "journal.unit"
	machineEvidenceJournalErr          machineEvidenceTextField = "journal.err"
	machineEvidenceJournalShapeExample machineEvidenceTextField = "journal.shape.example"
	machineEvidenceSystemdUnitName     machineEvidenceTextField = "systemd_unit.name"
	machineEvidenceSystemdActiveState  machineEvidenceTextField = "systemd_unit.active_state"
	machineEvidenceSystemdSubState     machineEvidenceTextField = "systemd_unit.sub_state"
	machineEvidenceSystemdReason       machineEvidenceTextField = "systemd_unit.reason"
	machineEvidenceCredentialProvider  machineEvidenceTextField = "credential.provider"
	machineEvidenceCredentialNote      machineEvidenceTextField = "credential.note"
	machineEvidenceCredentialError     machineEvidenceTextField = "credential.last_error"
	machineEvidenceCredentialPeerName  machineEvidenceTextField = "credential.peer.display_name"
	machineEvidenceOccupancyProvider   machineEvidenceTextField = "occupancy.provider"
	machineEvidenceOccupancySource     machineEvidenceTextField = "occupancy.source"
	machineEvidenceOccupancyAgent      machineEvidenceTextField = "occupancy.agent"
	machineEvidenceRunSummaryJobID     machineEvidenceTextField = "run_summary.job_id"
	machineEvidenceRunSummaryStatus    machineEvidenceTextField = "run_summary.status"
	machineEvidenceRunSummarySummary   machineEvidenceTextField = "run_summary.summary"
	machineEvidenceOpenClawReason      machineEvidenceTextField = "openclaw.reason"
	machineEvidenceOpenClawVersion     machineEvidenceTextField = "openclaw.version"
	machineEvidenceOpenClawVersionRaw  machineEvidenceTextField = "openclaw.version_raw"
	machineEvidenceOpenClawInstallText machineEvidenceTextField = "openclaw.install.text"
	machineEvidenceOpenClawKillMode    machineEvidenceTextField = "openclaw.install.kill_mode"
	machineEvidenceOpenClawDBReason    machineEvidenceTextField = "openclaw.db.reason"
	machineEvidenceOpenClawDBLayout    machineEvidenceTextField = "openclaw.db.layout"
	machineEvidenceOpenClawDBStatus    machineEvidenceTextField = "openclaw.db.status"
	machineEvidenceCLIToolName         machineEvidenceTextField = "cli_tool.name"
	machineEvidenceCLIToolVersion      machineEvidenceTextField = "cli_tool.version"
	machineEvidenceCLIToolVersionRaw   machineEvidenceTextField = "cli_tool.version_raw"
	machineEvidenceCLIToolReason       machineEvidenceTextField = "cli_tool.reason"
)

// machineEvidenceTextPolicies is the complete field-to-policy mapping for this DTO.
var machineEvidenceTextPolicies = map[machineEvidenceTextField]evidenceTextPolicy{
	machineEvidenceJournalUnit:         {maxBytes: 256, policy: boundedTextStrict},
	machineEvidenceJournalErr:          {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceJournalShapeExample: {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceSystemdUnitName:     {maxBytes: 256, policy: boundedTextStrict},
	machineEvidenceSystemdActiveState:  {maxBytes: 64, policy: boundedTextStrict},
	machineEvidenceSystemdSubState:     {maxBytes: 64, policy: boundedTextStrict},
	machineEvidenceSystemdReason:       {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceCredentialProvider:  {maxBytes: 256, policy: boundedTextStrict},
	machineEvidenceCredentialNote:      {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceCredentialError:     {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceCredentialPeerName:  {maxBytes: 256, policy: boundedTextStrict},
	machineEvidenceOccupancyProvider:   {maxBytes: 256, policy: boundedTextStrict},
	machineEvidenceOccupancySource:     {maxBytes: 64, policy: boundedTextStrict},
	machineEvidenceOccupancyAgent:      {maxBytes: 256, policy: boundedTextStrict},
	machineEvidenceRunSummaryJobID:     {maxBytes: 128, policy: boundedTextStrict},
	machineEvidenceRunSummaryStatus:    {maxBytes: 64, policy: boundedTextStrict},
	machineEvidenceRunSummarySummary:   {maxBytes: MachineEvidenceMaxFieldBytes, policy: boundedTextBlock},
	machineEvidenceOpenClawReason:      {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceOpenClawVersion:     {maxBytes: 256, policy: boundedTextStrict},
	machineEvidenceOpenClawVersionRaw:  {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceOpenClawInstallText: {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceOpenClawKillMode:    {maxBytes: 64, policy: boundedTextStrict},
	machineEvidenceOpenClawDBReason:    {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceOpenClawDBLayout:    {maxBytes: 64, policy: boundedTextStrict},
	machineEvidenceOpenClawDBStatus:    {maxBytes: 64, policy: boundedTextStrict},
	machineEvidenceCLIToolName:         {maxBytes: 256, policy: boundedTextStrict},
	machineEvidenceCLIToolVersion:      {maxBytes: 256, policy: boundedTextStrict},
	machineEvidenceCLIToolVersionRaw:   {maxBytes: 4096, policy: boundedTextBlock},
	machineEvidenceCLIToolReason:       {maxBytes: 4096, policy: boundedTextBlock},
}

type MachineEvidenceRequest struct {
	MachineID string
	Limit     int
}

type MachineEvidenceResult struct {
	SchemaVersion int                       `json:"schema_version"`
	EvaluatedAt   time.Time                 `json:"evaluated_at"`
	MachineID     string                    `json:"machine_id"`
	DisplayName   string                    `json:"display_name"`
	Disclosure    MachineEvidenceDisclosure `json:"disclosure"`
	OpenClaw      MachineOpenClawEvidence   `json:"openclaw"`
	CLITools      MachineCLIToolPage        `json:"cli_tools"`
	Credentials   MachineCredentialPage     `json:"credentials"`
	Occupancy     MachineOccupancyPage      `json:"occupancy"`
	SystemdUnits  MachineSystemdUnitPage    `json:"systemd_units"`
	RunSummaries  MachineRunSummaryPage     `json:"run_summaries"`
	Journals      MachineJournalPage        `json:"journals"`
}

type MachineEvidenceDisclosure struct {
	Limit              int `json:"limit"`
	MaxFieldBytes      int `json:"max_field_bytes"`
	MaxShapes          int `json:"max_shapes"`
	MaxCredentialPeers int `json:"max_credential_peers"`
	MaxOccupancyAgents int `json:"max_occupancy_agents"`

	// JournalProducer is the clawctl agent reading journalctl on the machine.
	JournalProducer MachineEvidenceProducer `json:"journal_producer"`
	// SystemdProducer is the same observer agent, but systemd metadata and
	// journal text remain distinct evidence types with distinct semantics.
	SystemdProducer    MachineEvidenceProducer `json:"systemd_producer"`
	OpenClawProducer   MachineEvidenceProducer `json:"openclaw_producer"`
	CLIToolProducer    MachineEvidenceProducer `json:"cli_tool_producer"`
	CredentialProducer MachineEvidenceProducer `json:"credential_producer"`
	OccupancyProducer  MachineEvidenceProducer `json:"occupancy_producer"`
	OccupancyRelay     MachineEvidenceProducer `json:"occupancy_relay"`
	// RunSummaryProducer is OpenClaw itself; RunSummaryRelay is the clawctl
	// agent that read OpenClaw's sqlite and forwarded the rows. Two hops, two
	// trust boundaries; the Hub verified neither.
	RunSummaryProducer MachineEvidenceProducer `json:"run_summary_producer"`
	RunSummaryRelay    MachineEvidenceProducer `json:"run_summary_relay"`

	IndependentVerifier bool `json:"independent_verifier"`

	// StatusIsOutcome is false: upstream status='ok' means the turn ended and
	// produced text, not that the work succeeded.
	StatusIsOutcome         bool `json:"status_is_outcome"`
	TerminalOutcomeRecorded bool `json:"terminal_outcome_recorded"`
	// SystemdStateIsWorkOutcome is false: active/running is process state, not
	// evidence that the unit's work succeeded.
	SystemdStateIsWorkOutcome bool `json:"systemd_state_is_work_outcome"`
	// SystemdMainPIDExcluded makes the volatile host-local omission explicit.
	SystemdMainPIDExcluded bool `json:"systemd_main_pid_excluded"`
	// OpenClaw and CLI evidence exposes relationships, not raw host coordinates.
	HostPathFieldsExcluded              bool `json:"host_path_fields_excluded"`
	ProcessIDFieldsExcluded             bool `json:"process_id_fields_excluded"`
	OpenClawDatabaseLocationsExcluded   bool `json:"openclaw_database_locations_excluded"`
	EvidenceTextPathRedactedByHub       bool `json:"evidence_text_path_redacted_by_hub"`
	CLIOpenClawTextRedactedByAgent      bool `json:"cli_openclaw_text_redacted_by_agent"`
	CLIOpenClawTextRedactedByHub        bool `json:"cli_openclaw_text_redacted_by_hub"`
	CLIVersionSourcesCollapsed          bool `json:"cli_version_sources_collapsed"`
	RawVersionTextParsedByHub           bool `json:"raw_version_text_parsed_by_hub"`
	RunningRelationshipDerivedFromPaths bool `json:"running_relationship_derived_from_paths"`

	// Credential file observations are not remote session validation. Known
	// secret-bearing fields and the active account identifier are structurally
	// absent; agent-provided note/error text is bounded but not secret-redacted.
	CredentialStatusIsSessionValidity   bool `json:"credential_status_is_session_validity"`
	CredentialRemoteValidationPerformed bool `json:"credential_remote_validation_performed"`
	CredentialActiveAccountIDExcluded   bool `json:"credential_active_account_id_excluded"`
	CredentialSecretFieldsExcluded      bool `json:"credential_secret_fields_excluded"`
	CredentialTextRedactedByAgent       bool `json:"credential_text_redacted_by_agent"`
	CredentialPeerIsSameTicket          bool `json:"credential_peer_is_same_ticket"`

	// Occupancy is Hub aggregation of completed-run evidence. Provider names
	// remain upstream text; errors are counted, never classified. Profile IDs,
	// session keys, job/model details, tokens and raw errors stay out of this DTO.
	OccupancyAggregatedByHub      bool `json:"occupancy_aggregated_by_hub"`
	OccupancyProcessStateUsed     bool `json:"occupancy_process_state_used"`
	OccupancyProviderNormalized   bool `json:"occupancy_provider_normalized"`
	OccupancyErrorsCategorized    bool `json:"occupancy_errors_categorized"`
	OccupancyProfileIDExcluded    bool `json:"occupancy_profile_id_excluded"`
	OccupancyEventDetailsExcluded bool `json:"occupancy_event_details_excluded"`

	// JournalSecretShapesRedactedByAgent records that redaction happened on the
	// machine before the Hub saw the bytes. The Hub did not and cannot verify it.
	JournalSecretShapesRedactedByAgent bool `json:"journal_secret_shapes_redacted_by_agent"`
	// RunSummarySecretShapesRedactedByAgent records that OpenClaw's summary text
	// is relayed verbatim with no redaction step anywhere.
	RunSummarySecretShapesRedactedByAgent bool `json:"run_summary_secret_shapes_redacted_by_agent"`
}

type MachineEvidenceProducer struct {
	Kind        string `json:"kind"`
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name"`
	Authority   string `json:"authority"`
}

type MachineRunSummaryPage struct {
	Total      int  `json:"total"`
	Truncated  bool `json:"truncated"`
	Observed   bool `json:"observed"`
	DBObserved bool `json:"db_observed"`
	// ObservedAt is the relay observation's two clocks when an OpenClaw
	// observation exists and its payload decoded, whether or not it included a
	// DB section. It is NOT the time the summarised task ran; that is each
	// item's ReportedAt.
	ObservedAt *MachineEvidenceClock `json:"observed_at"`
	Items      []MachineRunSummary   `json:"items"`
}

type MachineEvidenceClock struct {
	MeasuredAt time.Time `json:"measured_at"`
	ReceivedAt time.Time `json:"received_at"`
}

type MachineOpenClawEvidence struct {
	Observed        bool                    `json:"observed"`
	Decoded         bool                    `json:"decoded"`
	ObservedAt      *MachineEvidenceClock   `json:"observed_at"`
	Present         bool                    `json:"present"`
	Reason          *EvidenceText           `json:"reason"`
	CLIVersion      *EvidenceText           `json:"cli_version"`
	CLIVersionRaw   *EvidenceText           `json:"cli_version_raw"`
	GatewayVersion  *EvidenceText           `json:"gateway_version"`
	UpstreamVersion *EvidenceText           `json:"upstream_version"`
	CrashBundles    int                     `json:"crash_bundles"`
	Install         *MachineOpenClawInstall `json:"install"`
	InstallInvalid  bool                    `json:"install_invalid"`
	DB              *MachineOpenClawDB      `json:"db"`
	DBInvalid       bool                    `json:"db_invalid"`
}

type MachineOpenClawInstall struct {
	UnitFound                bool          `json:"unit_found"`
	UnitReason               *EvidenceText `json:"unit_reason"`
	DropInCount              int           `json:"drop_in_count"`
	KillMode                 *EvidenceText `json:"kill_mode"`
	NRestarts                *int          `json:"n_restarts"`
	ActiveEnterAt            *time.Time    `json:"active_enter_at"`
	NodeVersion              *EvidenceText `json:"node_version"`
	NodeVersionReason        *EvidenceText `json:"node_version_reason"`
	NpmObserved              bool          `json:"npm_observed"`
	NpmVersion               *EvidenceText `json:"npm_version"`
	NpmReason                *EvidenceText `json:"npm_reason"`
	RunningDirectoryObserved bool          `json:"running_directory_observed"`
	RunningDirectoryExists   bool          `json:"running_directory_exists"`
	RunningDirectoryWritable *bool         `json:"running_directory_writable"`
	RunningVersion           *EvidenceText `json:"running_version"`
	RunningDirectoryReason   *EvidenceText `json:"running_directory_reason"`
	ProcessObserved          bool          `json:"process_observed"`
	ProcessMatchesUnit       *bool         `json:"process_matches_unit"`
	ProcessReason            *EvidenceText `json:"process_reason"`
	ReleaseLayoutObserved    bool          `json:"release_layout_observed"`
	ReleasesPresent          bool          `json:"releases_present"`
	CurrentReleaseLinked     bool          `json:"current_release_linked"`
	DiskFreeBytes            int64         `json:"disk_free_bytes"`
	DiskFreeMeasured         bool          `json:"disk_free_measured"`
	DiskFreeReason           *EvidenceText `json:"disk_free_reason"`
}

type MachineOpenClawDB struct {
	Present                  bool                   `json:"present"`
	Reason                   *EvidenceText          `json:"reason"`
	Layout                   *EvidenceText          `json:"layout"`
	Support                  model.SupportLevel     `json:"support"`
	UnknownLocationCount     int                    `json:"unknown_location_count"`
	LastTaskEndedAt          *time.Time             `json:"last_task_ended_at"`
	TaskRunRows              int                    `json:"task_run_rows"`
	TaskStatuses             MachineStatusCountPage `json:"task_statuses"`
	LastCronRunAt            *time.Time             `json:"last_cron_run_at"`
	CronRunLogRows           int                    `json:"cron_run_log_rows"`
	CronStatuses             MachineStatusCountPage `json:"cron_statuses"`
	CronJobsTotal            int                    `json:"cron_jobs_total"`
	CronJobsTotalMeasured    bool                   `json:"cron_jobs_total_measured"`
	CronJobsEnabled          int                    `json:"cron_jobs_enabled"`
	CronJobsEnabledMeasured  bool                   `json:"cron_jobs_enabled_measured"`
	NextCronRunAt            *time.Time             `json:"next_cron_run_at"`
	CronJobsOverdue          int                    `json:"cron_jobs_overdue"`
	CronJobsScheduleMeasured bool                   `json:"cron_jobs_schedule_measured"`
	TerminalOutcomePopulated int                    `json:"terminal_outcome_populated"`
}

type MachineStatusCountPage struct {
	Total     int                  `json:"total"`
	Truncated bool                 `json:"truncated"`
	Invalid   int                  `json:"invalid"`
	Items     []MachineStatusCount `json:"items"`
}

type MachineStatusCount struct {
	Status EvidenceText `json:"status"`
	Count  int          `json:"count"`
}

type MachineCLIToolPage struct {
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
	Invalid   int              `json:"invalid"`
	Items     []MachineCLITool `json:"items"`
}

type MachineCLITool struct {
	Name                EvidenceText       `json:"name"`
	Present             bool               `json:"present"`
	OnPath              bool               `json:"on_path"`
	PresentEvidence     string             `json:"present_evidence"`
	PathSource          string             `json:"path_source"`
	PathReason          *EvidenceText      `json:"path_reason"`
	DaemonReach         string             `json:"daemon_reach"`
	VersionReported     *EvidenceText      `json:"version_reported"`
	VersionRaw          *EvidenceText      `json:"version_raw"`
	VersionPackageJSON  *EvidenceText      `json:"version_package_json"`
	VersionReason       *EvidenceText      `json:"version_reason"`
	SourcesDisagree     bool               `json:"version_sources_disagree"`
	ProcessObserved     bool               `json:"process_observed"`
	RunningRelationship string             `json:"running_relationship"`
	ProcessScan         string             `json:"process_scan"`
	RunningReason       *EvidenceText      `json:"running_reason"`
	Support             model.SupportLevel `json:"support"`
	MeasuredAt          time.Time          `json:"measured_at"`
	ReceivedAt          time.Time          `json:"received_at"`
}

type MachineCredentialPage struct {
	Total     int                 `json:"total"`
	Truncated bool                `json:"truncated"`
	Invalid   int                 `json:"invalid"`
	Items     []MachineCredential `json:"items"`
}

// MachineCredentialGraceStatus：這張憑證的過期寬限判決。
//
// ⚠ 這裡不能用 bool。false 會同時裝進「票還沒過期，寬限不適用」和
// 「票已過期，而且寬限已經用完」；前者不用處理，後者要人介入，是兩件事。
// 三個值分別保留「未過期、不適用」、「已過期、仍在寬限」與
// 「已過期、寬限已用完」，不把不同處置壓成同一個答案。
type MachineCredentialGraceStatus string

const (
	MachineCredentialGraceNotApplicable MachineCredentialGraceStatus = "not_applicable"
	MachineCredentialGraceInGrace       MachineCredentialGraceStatus = "in_grace"
	MachineCredentialGraceExceeded      MachineCredentialGraceStatus = "exceeded"
)

// ⚠ MachineCredential.GraceStatus 只給同一個 process 裡的 HTML adapter 用；
// web.machine 直接呼叫 MachineEvidence，不經 JSON。operatorclient 的嚴格解碼同時
// 在 decodeStrictJSONDocument 用 DisallowUnknownFields 拒絕未知欄位，並以
// validateExactJSONShape 驗證完整 JSON 形狀；多送一欄會讓舊 CLI 拒收整份 evidence，
// 不是只看不到這欄。所以它刻意標成 json:"-"，MachineEvidenceSchemaVersion 也維持 6。
type MachineCredential struct {
	Provider              EvidenceText                 `json:"provider"`
	Status                model.CredStatus             `json:"status"`
	ExpiresAt             *time.Time                   `json:"expires_at"`
	LastRefresh           *time.Time                   `json:"last_refresh"`
	FileMTime             *time.Time                   `json:"file_mtime"`
	VerifiedAt            *time.Time                   `json:"verified_at"`
	VerificationMethod    model.VerifyMethod           `json:"verification_method"`
	ActiveAccountSelected bool                         `json:"active_account_selected"`
	AccountCount          int                          `json:"account_count"`
	Note                  *EvidenceText                `json:"note"`
	LastError             *EvidenceText                `json:"last_error"`
	MeasuredAt            time.Time                    `json:"measured_at"`
	ReceivedAt            time.Time                    `json:"received_at"`
	LifetimeSeconds       int64                        `json:"lifetime_seconds"`
	RefreshesSeen         int                          `json:"refreshes_seen"`
	WatchedForSeconds     int64                        `json:"watched_for_seconds"`
	PeersTotal            int                          `json:"peers_total"`
	PeersTruncated        bool                         `json:"peers_truncated"`
	Peers                 []MachineCredentialPeer      `json:"peers"`
	GraceStatus           MachineCredentialGraceStatus `json:"-"`
}

type MachineCredentialPeer struct {
	DisplayName   EvidenceText `json:"display_name"`
	RefreshesSeen int          `json:"refreshes_seen"`
	FileMTime     *time.Time   `json:"file_mtime"`
}

type MachineOccupancyPage struct {
	Total               int                   `json:"total"`
	Truncated           bool                  `json:"truncated"`
	Invalid             int                   `json:"invalid"`
	Observed            bool                  `json:"observed"`
	DBObserved          bool                  `json:"db_observed"`
	ObservationInvalid  bool                  `json:"observation_invalid"`
	ObservedAt          *MachineEvidenceClock `json:"observed_at"`
	RowsSeen            int                   `json:"rows_seen"`
	RowsWithoutProvider int                   `json:"rows_without_provider"`
	Items               []MachineOccupancy    `json:"items"`
}

type MachineOccupancy struct {
	Provider        EvidenceText   `json:"provider"`
	Source          EvidenceText   `json:"source"`
	Runs            int            `json:"runs"`
	Errors          int            `json:"errors"`
	LastRunAt       time.Time      `json:"last_run_at"`
	LastReceivedAt  time.Time      `json:"last_received_at"`
	AgentsTotal     int            `json:"agents_total"`
	AgentsTruncated bool           `json:"agents_truncated"`
	Agents          []EvidenceText `json:"agents"`
}

type MachineSystemdUnitPage struct {
	Total     int                  `json:"total"`
	Truncated bool                 `json:"truncated"`
	Invalid   int                  `json:"invalid"`
	Items     []MachineSystemdUnit `json:"items"`
}

type MachineSystemdUnit struct {
	Name                 EvidenceText  `json:"name"`
	Present              bool          `json:"present"`
	Measured             bool          `json:"measured"`
	Reason               *EvidenceText `json:"reason"`
	ActiveState          EvidenceText  `json:"active_state"`
	SubState             EvidenceText  `json:"sub_state"`
	ActiveEnterTimestamp *time.Time    `json:"active_enter_timestamp"`
	NRestarts            int           `json:"n_restarts"`
	MeasuredAt           time.Time     `json:"measured_at"`
	ReceivedAt           time.Time     `json:"received_at"`
}

type MachineRunSummary struct {
	JobID EvidenceText `json:"job_id"`
	// ReportedAt is OpenClaw's own clock for the turn. Zero means the upstream
	// row had no timestamp; it does not mean "now" and does not mean "epoch".
	ReportedAt *time.Time   `json:"reported_at"`
	Status     EvidenceText `json:"status"`
	Summary    EvidenceText `json:"summary"`
}

type MachineJournalPage struct {
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	// Undecodable counts journal observations whose bytes exist but cannot be
	// read as a UnitJournal. Those units are neither collected and heard nor
	// not collected; the observation still exists.
	Undecodable int              `json:"undecodable"`
	Items       []MachineJournal `json:"items"`
	// UnitsWithoutJournal names units this machine reported a systemd
	// observation for, with no journal observation this round. It means "not
	// collected", NOT "quiet".
	UnitsWithoutJournal          []EvidenceText `json:"units_without_journal"`
	UnitsWithoutJournalTotal     int            `json:"units_without_journal_total"`
	UnitsWithoutJournalTruncated bool           `json:"units_without_journal_truncated"`
}

// MachineJournal exposes raw journal text only after bounding and scrubbing.
// ⚠⚠ Never parse, keyword-match, or derive state from Err or Top examples.
type MachineJournal struct {
	Unit          EvidenceText `json:"unit"`
	WindowSeconds int          `json:"window_seconds"`

	// ReadFailed with Err set means the Hub did not hear this unit. Lines==0
	// then carries no information about whether the unit was quiet.
	ReadFailed bool          `json:"read_failed"`
	Err        *EvidenceText `json:"err"`

	Lines int `json:"lines"`
	// LinesAreLowerBound is the agent's own read cap, not our byte truncation.
	LinesAreLowerBound bool `json:"lines_are_lower_bound"`
	Shapes             int  `json:"shapes"`

	Top          []MachineJournalShape `json:"top"`
	TopTruncated bool                  `json:"top_truncated"`

	MeasuredAt time.Time `json:"measured_at"`
	ReceivedAt time.Time `json:"received_at"`
}

type MachineJournalShape struct {
	Count   int          `json:"count"`
	Example EvidenceText `json:"example"`
}

func ValidateMachineEvidenceRequest(request MachineEvidenceRequest) error {
	if err := validateMachineReadText("machine_id", request.MachineID, 256); err != nil {
		return err
	}
	if request.Limit < 0 || request.Limit > store.MaxMachineEvidencePageSize {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidMachineRead, store.MaxMachineEvidencePageSize)
	}
	return nil
}

func (s *Service) MachineEvidence(request MachineEvidenceRequest, asOf time.Time) (MachineEvidenceResult, error) {
	if err := ValidateMachineEvidenceRequest(request); err != nil {
		return MachineEvidenceResult{}, err
	}
	evaluatedAt, err := machineEvaluationTime(asOf)
	if err != nil {
		return MachineEvidenceResult{}, err
	}
	limit := request.Limit
	if limit == 0 {
		limit = MachineEvidenceDefaultLimit
	}
	evidence, err := s.store.MachineReadEvidence(request.MachineID, evaluatedAt, limit)
	if err != nil {
		return MachineEvidenceResult{}, err
	}
	observerProducer := MachineEvidenceProducer{
		Kind: MachineEvidenceProducerObserverAgent, MachineID: evidence.MachineID,
		DisplayName: evidence.DisplayName, Authority: MachineEvidenceAuthorityMachineBearer,
	}
	result := MachineEvidenceResult{
		SchemaVersion: MachineEvidenceSchemaVersion,
		EvaluatedAt:   evaluatedAt,
		MachineID:     evidence.MachineID,
		DisplayName:   evidence.DisplayName,
		Disclosure: MachineEvidenceDisclosure{
			Limit: limit, MaxFieldBytes: MachineEvidenceMaxFieldBytes, MaxShapes: MachineEvidenceMaxShapes,
			MaxCredentialPeers: MachineEvidenceMaxCredentialPeers,
			MaxOccupancyAgents: MachineEvidenceMaxOccupancyAgents,
			JournalProducer:    observerProducer,
			SystemdProducer:    observerProducer,
			OpenClawProducer:   observerProducer,
			CLIToolProducer:    observerProducer,
			CredentialProducer: observerProducer,
			OccupancyProducer: MachineEvidenceProducer{
				Kind: MachineEvidenceProducerOpenClawTask, MachineID: evidence.MachineID,
				DisplayName: evidence.DisplayName,
			},
			OccupancyRelay: observerProducer,
			RunSummaryProducer: MachineEvidenceProducer{
				Kind: MachineEvidenceProducerOpenClawTask, MachineID: evidence.MachineID,
				DisplayName: evidence.DisplayName,
			},
			RunSummaryRelay:     observerProducer,
			IndependentVerifier: false, StatusIsOutcome: false, TerminalOutcomeRecorded: false,
			SystemdStateIsWorkOutcome: false, SystemdMainPIDExcluded: true,
			HostPathFieldsExcluded: true, ProcessIDFieldsExcluded: true, OpenClawDatabaseLocationsExcluded: true,
			EvidenceTextPathRedactedByHub:  false,
			CLIOpenClawTextRedactedByAgent: false, CLIOpenClawTextRedactedByHub: false,
			CLIVersionSourcesCollapsed: false, RawVersionTextParsedByHub: false,
			RunningRelationshipDerivedFromPaths:   true,
			CredentialStatusIsSessionValidity:     false,
			CredentialRemoteValidationPerformed:   evidence.CredentialRemoteValidationPerformed,
			CredentialActiveAccountIDExcluded:     true,
			CredentialSecretFieldsExcluded:        true,
			CredentialTextRedactedByAgent:         false,
			CredentialPeerIsSameTicket:            false,
			OccupancyAggregatedByHub:              true,
			OccupancyProcessStateUsed:             false,
			OccupancyProviderNormalized:           false,
			OccupancyErrorsCategorized:            false,
			OccupancyProfileIDExcluded:            true,
			OccupancyEventDetailsExcluded:         true,
			JournalSecretShapesRedactedByAgent:    true,
			RunSummarySecretShapesRedactedByAgent: false,
		},
		OpenClaw: MachineOpenClawEvidence{
			Observed: evidence.OpenClaw.Observed, Decoded: evidence.OpenClaw.Decoded,
			InstallInvalid: evidence.OpenClaw.InstallInvalid, DBInvalid: evidence.OpenClaw.DBInvalid,
		},
		CLITools: MachineCLIToolPage{
			Total: evidence.CLIToolsTotal, Truncated: evidence.CLIToolsTruncated,
			Invalid: evidence.CLIToolsInvalid, Items: make([]MachineCLITool, 0, len(evidence.CLITools)),
		},
		Credentials: MachineCredentialPage{
			Total: evidence.CredentialsTotal, Truncated: evidence.CredentialsTruncated,
			Invalid: evidence.CredentialsInvalid,
			Items:   make([]MachineCredential, 0, len(evidence.Credentials)),
		},
		Occupancy: MachineOccupancyPage{
			Total: evidence.OccupancyTotal, Truncated: evidence.OccupancyTruncated,
			Invalid:  evidence.OccupancyInvalid,
			Observed: evidence.OccupancyObserved, DBObserved: evidence.OccupancyDBObserved,
			ObservationInvalid: evidence.OccupancyObservationInvalid,
			RowsSeen:           evidence.OccupancyRowsSeen, RowsWithoutProvider: evidence.OccupancyRowsSkipped,
			Items: make([]MachineOccupancy, 0, len(evidence.Occupancy)),
		},
		SystemdUnits: MachineSystemdUnitPage{
			Total: evidence.SystemdUnitsTotal, Truncated: evidence.SystemdUnitsTruncated,
			Invalid: evidence.SystemdUnitsInvalid,
			Items:   make([]MachineSystemdUnit, 0, len(evidence.SystemdUnits)),
		},
		RunSummaries: MachineRunSummaryPage{
			Total: evidence.RunSummariesTotal, Truncated: evidence.RunSummariesTruncated,
			Observed: evidence.OpenClawObserved, DBObserved: evidence.OpenClawDBObserved,
			Items: make([]MachineRunSummary, 0, len(evidence.RunSummaries)),
		},
		Journals: MachineJournalPage{
			Total: evidence.JournalsTotal, Truncated: evidence.JournalsTruncated,
			Undecodable:                  evidence.JournalsUndecodable,
			Items:                        make([]MachineJournal, 0, len(evidence.Journals)),
			UnitsWithoutJournal:          make([]EvidenceText, 0, len(evidence.UnitsWithoutJournal)),
			UnitsWithoutJournalTotal:     evidence.UnitsWithoutJournalTotal,
			UnitsWithoutJournalTruncated: evidence.UnitsWithoutJournalTruncated,
		},
	}
	projectMachineOpenClawEvidence(&result.OpenClaw, evidence.OpenClaw)
	for _, tool := range evidence.CLITools {
		item := MachineCLITool{
			Name:    projectMachineEvidenceText(machineEvidenceCLIToolName, tool.Name),
			Present: tool.Present, OnPath: tool.OnPath, PresentEvidence: tool.PresentEvidence,
			PathSource: tool.PathSource, DaemonReach: tool.DaemonReach,
			SourcesDisagree: tool.SourcesDisagree, ProcessObserved: tool.ProcessObserved,
			RunningRelationship: tool.RunningRelationship, ProcessScan: tool.ProcessScan, Support: tool.Support,
			MeasuredAt: tool.MeasuredAt.UTC(), ReceivedAt: tool.ReceivedAt.UTC(),
		}
		item.PathReason = projectOptionalMachineEvidenceText(machineEvidenceCLIToolReason, tool.PathReason)
		item.VersionReported = projectOptionalMachineEvidenceText(machineEvidenceCLIToolVersion, tool.VersionReported)
		item.VersionRaw = projectOptionalMachineEvidenceText(machineEvidenceCLIToolVersionRaw, tool.VersionRaw)
		item.VersionPackageJSON = projectOptionalMachineEvidenceText(machineEvidenceCLIToolVersion, tool.VersionPackageJSON)
		item.VersionReason = projectOptionalMachineEvidenceText(machineEvidenceCLIToolReason, tool.VersionReason)
		item.RunningReason = projectOptionalMachineEvidenceText(machineEvidenceCLIToolReason, tool.RunningReason)
		result.CLITools.Items = append(result.CLITools.Items, item)
	}
	for _, credential := range evidence.Credentials {
		// 三態先把「未過期、不適用」留下來。⚠ expired 守衛不能省：CredGrace
		// 對非 expired 一律回 false，直接拿它的 bool 會把一張沒過期的票判成超過寬限。
		graceStatus := MachineCredentialGraceNotApplicable
		if credential.Status == model.CredExpired {
			// ⚠ 寬限只能由 state.CredGrace 判決；Derive 與早報已經各是一個呼叫者，
			// 這裡是第三個，不能在 adapter 旁邊再長出同一條規則的另一份算式。
			//
			// now 必須用整份結果的 EvaluatedAt，跟同頁「發現」區共用評估時鐘；
			// 換成 time.Now 或憑證的 MeasuredAt，邊界上會讓狀態欄與發現區互相矛盾。
			// ⚠ Lifetime 也要保留原始 duration 的完整精度，不能拿已截成整秒的
			// LifetimeSeconds 回算，否則寬限邊界會少掉不足一秒的那一截。
			_, _, inGrace := state.CredGrace(state.CredFact{
				Status:    string(credential.Status),
				ExpiresAt: credential.ExpiresAt,
				Lifetime:  credential.Lifetime,
			}, result.EvaluatedAt)
			graceStatus = MachineCredentialGraceExceeded
			if inGrace {
				graceStatus = MachineCredentialGraceInGrace
			}
		}
		item := MachineCredential{
			Provider:              projectMachineEvidenceText(machineEvidenceCredentialProvider, credential.Provider),
			Status:                credential.Status,
			ExpiresAt:             machineEvidenceUTCTimePtr(credential.ExpiresAt),
			LastRefresh:           machineEvidenceUTCTimePtr(credential.LastRefresh),
			FileMTime:             machineEvidenceUTCTimePtr(credential.FileMTime),
			VerifiedAt:            machineEvidenceUTCTimePtr(credential.VerifiedAt),
			VerificationMethod:    credential.VerificationMethod,
			ActiveAccountSelected: credential.ActiveAccountSelected,
			AccountCount:          credential.AccountCount,
			MeasuredAt:            credential.MeasuredAt.UTC(), ReceivedAt: credential.ReceivedAt.UTC(),
			LifetimeSeconds:   int64(credential.Lifetime / time.Second),
			RefreshesSeen:     credential.RefreshesSeen,
			WatchedForSeconds: int64(credential.WatchedFor / time.Second),
			PeersTotal:        credential.PeersTotal, PeersTruncated: credential.PeersTruncated,
			Peers:       make([]MachineCredentialPeer, 0, len(credential.Peers)),
			GraceStatus: graceStatus,
		}
		if credential.Note != "" {
			note := projectMachineEvidenceText(machineEvidenceCredentialNote, credential.Note)
			item.Note = &note
		}
		if credential.LastError != "" {
			lastError := projectMachineEvidenceText(machineEvidenceCredentialError, credential.LastError)
			item.LastError = &lastError
		}
		for _, peer := range credential.Peers {
			item.Peers = append(item.Peers, MachineCredentialPeer{
				DisplayName:   projectMachineEvidenceText(machineEvidenceCredentialPeerName, peer.DisplayName),
				RefreshesSeen: peer.RefreshesSeen, FileMTime: machineEvidenceUTCTimePtr(peer.FileMTime),
			})
		}
		sort.SliceStable(item.Peers, func(i, j int) bool { return machineCredentialPeerBefore(item.Peers[i], item.Peers[j]) })
		result.Credentials.Items = append(result.Credentials.Items, item)
	}
	sort.Slice(result.Credentials.Items, func(i, j int) bool {
		return result.Credentials.Items[i].Provider.Text < result.Credentials.Items[j].Provider.Text
	})
	if evidence.OccupancyObservedAt != nil {
		result.Occupancy.ObservedAt = &MachineEvidenceClock{
			MeasuredAt: evidence.OccupancyObservedAt.MeasuredAt.UTC(),
			ReceivedAt: evidence.OccupancyObservedAt.ReceivedAt.UTC(),
		}
	}
	for _, occupancy := range evidence.Occupancy {
		item := MachineOccupancy{
			Provider: projectMachineEvidenceText(machineEvidenceOccupancyProvider, occupancy.Provider),
			Source:   projectMachineEvidenceText(machineEvidenceOccupancySource, occupancy.Source),
			Runs:     occupancy.Runs, Errors: occupancy.Errors,
			LastRunAt: occupancy.LastRunAt.UTC(), LastReceivedAt: occupancy.LastReceivedAt.UTC(),
			AgentsTotal: occupancy.AgentsTotal, AgentsTruncated: occupancy.AgentsTruncated,
			Agents: make([]EvidenceText, 0, len(occupancy.Agents)),
		}
		for _, agent := range occupancy.Agents {
			item.Agents = append(item.Agents, projectMachineEvidenceText(machineEvidenceOccupancyAgent, agent))
		}
		result.Occupancy.Items = append(result.Occupancy.Items, item)
	}
	sort.Slice(result.Occupancy.Items, func(i, j int) bool {
		a, b := result.Occupancy.Items[i], result.Occupancy.Items[j]
		if a.Provider.Text != b.Provider.Text {
			return a.Provider.Text < b.Provider.Text
		}
		return a.Source.Text < b.Source.Text
	})
	for _, unit := range evidence.SystemdUnits {
		var activeEnterTimestamp *time.Time
		if unit.ActiveEnterTimestamp != nil {
			at := unit.ActiveEnterTimestamp.UTC()
			activeEnterTimestamp = &at
		}
		result.SystemdUnits.Items = append(result.SystemdUnits.Items, MachineSystemdUnit{
			Name:                 projectMachineEvidenceText(machineEvidenceSystemdUnitName, unit.Name),
			Present:              unit.Present,
			Measured:             unit.Measured,
			Reason:               projectOptionalMachineEvidenceText(machineEvidenceSystemdReason, unit.Reason),
			ActiveState:          projectMachineEvidenceText(machineEvidenceSystemdActiveState, unit.ActiveState),
			SubState:             projectMachineEvidenceText(machineEvidenceSystemdSubState, unit.SubState),
			ActiveEnterTimestamp: activeEnterTimestamp, NRestarts: unit.NRestarts,
			MeasuredAt: unit.MeasuredAt.UTC(), ReceivedAt: unit.ReceivedAt.UTC(),
		})
	}
	if evidence.RunSummaryObservedAt != nil {
		result.RunSummaries.ObservedAt = &MachineEvidenceClock{
			MeasuredAt: evidence.RunSummaryObservedAt.MeasuredAt.UTC(),
			ReceivedAt: evidence.RunSummaryObservedAt.ReceivedAt.UTC(),
		}
	}
	for _, summary := range evidence.RunSummaries {
		var reportedAt *time.Time
		if !summary.At.IsZero() {
			at := summary.At.UTC()
			reportedAt = &at
		}
		result.RunSummaries.Items = append(result.RunSummaries.Items, MachineRunSummary{
			JobID:      projectMachineEvidenceText(machineEvidenceRunSummaryJobID, summary.JobID),
			ReportedAt: reportedAt,
			Status:     projectMachineEvidenceText(machineEvidenceRunSummaryStatus, summary.Status),
			Summary:    projectMachineEvidenceText(machineEvidenceRunSummarySummary, summary.Summary),
		})
	}
	for _, row := range evidence.Journals {
		readFailed := strings.TrimSpace(row.Err) != ""
		journal := MachineJournal{
			Unit:          projectMachineEvidenceText(machineEvidenceJournalUnit, row.Unit),
			WindowSeconds: row.WindowSec, ReadFailed: readFailed,
			Lines: row.Lines, LinesAreLowerBound: row.Truncated, Shapes: row.Shapes,
			Top:          make([]MachineJournalShape, 0, min(len(row.Top), MachineEvidenceMaxShapes)),
			TopTruncated: len(row.Top) > MachineEvidenceMaxShapes,
			MeasuredAt:   row.MeasuredAt.UTC(), ReceivedAt: row.ReceivedAt.UTC(),
		}
		if readFailed {
			errText := projectMachineEvidenceText(machineEvidenceJournalErr, row.Err)
			journal.Err = &errText
		}
		for _, shape := range row.Top[:min(len(row.Top), MachineEvidenceMaxShapes)] {
			journal.Top = append(journal.Top, MachineJournalShape{
				Count:   shape.Count,
				Example: projectMachineEvidenceText(machineEvidenceJournalShapeExample, shape.Example),
			})
		}
		result.Journals.Items = append(result.Journals.Items, journal)
	}
	for _, unit := range evidence.UnitsWithoutJournal {
		result.Journals.UnitsWithoutJournal = append(result.Journals.UnitsWithoutJournal,
			projectMachineEvidenceText(machineEvidenceJournalUnit, unit))
	}
	sort.Slice(result.Journals.UnitsWithoutJournal, func(i, j int) bool {
		return result.Journals.UnitsWithoutJournal[i].Text < result.Journals.UnitsWithoutJournal[j].Text
	})
	return result, nil
}

func projectMachineEvidenceText(field machineEvidenceTextField, value string) EvidenceText {
	policy, ok := machineEvidenceTextPolicies[field]
	if !ok {
		panic("missing machine evidence text policy for " + field)
	}
	return projectEvidenceText(policy, value)
}

func projectOptionalMachineEvidenceText(field machineEvidenceTextField, value string) *EvidenceText {
	if value == "" {
		return nil
	}
	projected := projectMachineEvidenceText(field, value)
	return &projected
}

func projectMachineOpenClawEvidence(dst *MachineOpenClawEvidence, src store.MachineReadOpenClaw) {
	if src.ObservedAt != nil {
		dst.ObservedAt = &MachineEvidenceClock{
			MeasuredAt: src.ObservedAt.MeasuredAt.UTC(), ReceivedAt: src.ObservedAt.ReceivedAt.UTC(),
		}
	}
	if !src.Decoded {
		return
	}
	dst.Present = src.Present
	dst.CrashBundles = src.CrashBundles
	dst.Reason = projectOptionalMachineEvidenceText(machineEvidenceOpenClawReason, src.Reason)
	dst.CLIVersion = projectOptionalMachineEvidenceText(machineEvidenceOpenClawVersion, src.CLIVersion)
	dst.CLIVersionRaw = projectOptionalMachineEvidenceText(machineEvidenceOpenClawVersionRaw, src.CLIVersionRaw)
	dst.GatewayVersion = projectOptionalMachineEvidenceText(machineEvidenceOpenClawVersion, src.GatewayVersion)
	dst.UpstreamVersion = projectOptionalMachineEvidenceText(machineEvidenceOpenClawVersion, src.UpstreamVersion)
	if src.Install != nil {
		i := src.Install
		dst.Install = &MachineOpenClawInstall{
			UnitFound: i.UnitFound, DropInCount: i.DropInCount,
			NRestarts: i.NRestarts, ActiveEnterAt: machineEvidenceUTCTimePtr(i.ActiveEnterAt),
			NpmObserved: i.NpmObserved, RunningDirectoryObserved: i.RunningDirectoryObserved,
			RunningDirectoryExists:   i.RunningDirectoryExists,
			RunningDirectoryWritable: i.RunningDirectoryWritable,
			ProcessObserved:          i.ProcessObserved, ProcessMatchesUnit: i.ProcessMatchesUnit,
			ReleaseLayoutObserved: i.ReleaseLayoutObserved, ReleasesPresent: i.ReleasesPresent,
			CurrentReleaseLinked: i.CurrentReleaseLinked, DiskFreeBytes: i.DiskFreeBytes,
			DiskFreeMeasured: i.DiskFreeMeasured,
		}
		dst.Install.UnitReason = projectOptionalMachineEvidenceText(machineEvidenceOpenClawInstallText, i.UnitReason)
		dst.Install.KillMode = projectOptionalMachineEvidenceText(machineEvidenceOpenClawKillMode, i.KillMode)
		dst.Install.NodeVersion = projectOptionalMachineEvidenceText(machineEvidenceOpenClawVersion, i.NodeVersion)
		dst.Install.NodeVersionReason = projectOptionalMachineEvidenceText(machineEvidenceOpenClawInstallText, i.NodeVersionReason)
		dst.Install.NpmVersion = projectOptionalMachineEvidenceText(machineEvidenceOpenClawVersion, i.NpmVersion)
		dst.Install.NpmReason = projectOptionalMachineEvidenceText(machineEvidenceOpenClawInstallText, i.NpmReason)
		dst.Install.RunningVersion = projectOptionalMachineEvidenceText(machineEvidenceOpenClawVersion, i.RunningVersion)
		dst.Install.RunningDirectoryReason = projectOptionalMachineEvidenceText(machineEvidenceOpenClawInstallText, i.RunningDirectoryReason)
		dst.Install.ProcessReason = projectOptionalMachineEvidenceText(machineEvidenceOpenClawInstallText, i.ProcessReason)
		dst.Install.DiskFreeReason = projectOptionalMachineEvidenceText(machineEvidenceOpenClawInstallText, i.DiskFreeReason)
	}
	if src.DB != nil {
		db := src.DB
		dst.DB = &MachineOpenClawDB{
			Present: db.Present, Support: db.Support, UnknownLocationCount: db.UnknownLocationCount,
			LastTaskEndedAt: machineEvidenceUTCTimePtr(db.LastTaskEndedAt), TaskRunRows: db.TaskRunRows,
			TaskStatuses: MachineStatusCountPage{Total: db.TaskStatusesTotal, Truncated: db.TaskStatusesTruncated,
				Invalid: db.TaskStatusesInvalid, Items: make([]MachineStatusCount, 0, len(db.TaskStatuses))},
			LastCronRunAt: machineEvidenceUTCTimePtr(db.LastCronRunAt), CronRunLogRows: db.CronRunLogRows,
			CronStatuses: MachineStatusCountPage{Total: db.CronStatusesTotal, Truncated: db.CronStatusesTruncated,
				Invalid: db.CronStatusesInvalid, Items: make([]MachineStatusCount, 0, len(db.CronStatuses))},
			CronJobsTotal: db.CronJobsTotal, CronJobsTotalMeasured: db.CronJobsTotalMeasured,
			CronJobsEnabled: db.CronJobsEnabled, CronJobsEnabledMeasured: db.CronJobsEnabledMeasured,
			NextCronRunAt: machineEvidenceUTCTimePtr(db.NextCronRunAt), CronJobsOverdue: db.CronJobsOverdue,
			CronJobsScheduleMeasured: db.CronJobsScheduleMeasured,
			TerminalOutcomePopulated: db.TerminalOutcomePopulated,
		}
		dst.DB.Reason = projectOptionalMachineEvidenceText(machineEvidenceOpenClawDBReason, db.Reason)
		dst.DB.Layout = projectOptionalMachineEvidenceText(machineEvidenceOpenClawDBLayout, db.Layout)
		for _, status := range db.TaskStatuses {
			dst.DB.TaskStatuses.Items = append(dst.DB.TaskStatuses.Items, MachineStatusCount{
				Status: projectMachineEvidenceText(machineEvidenceOpenClawDBStatus, status.Status), Count: status.Count,
			})
		}
		for _, status := range db.CronStatuses {
			dst.DB.CronStatuses.Items = append(dst.DB.CronStatuses.Items, MachineStatusCount{
				Status: projectMachineEvidenceText(machineEvidenceOpenClawDBStatus, status.Status), Count: status.Count,
			})
		}
	}
}

func machineEvidenceUTCTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func machineCredentialPeerBefore(a, b MachineCredentialPeer) bool {
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
