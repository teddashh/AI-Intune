package store

import (
	"fmt"
	"sort"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

const MaxMachineEvidencePageSize = 100

const (
	MaxMachineEvidenceCredentialPeers = 6
	MaxMachineEvidenceOccupancyAgents = 6
)

// MachineReadEvidence is the bounded evidence source for one machine. It
// deliberately has no Detail field. Its explicit projections are structurally
// incapable of carrying dedicated host-path/PID fields, credential
// secrets/account identifiers, occupancy event identities, connect data or
// lease material. Bounded free text is not path/secret/PID-redacted. Systemd
// rows retain only the typed process-state facts needed beside journal text.
//
// ⚠⚠ RunSummaries and the journal Examples/Err values are original text.
// Do not parse them, keyword-match them, or derive any state field from them.
type MachineReadEvidence struct {
	MachineID                           string
	DisplayName                         string
	OpenClaw                            MachineReadOpenClaw
	CLITools                            []MachineReadCLITool // sorted by Name, ascending
	CLIToolsTotal                       int
	CLIToolsTruncated                   bool
	CLIToolsInvalid                     int
	Credentials                         []MachineReadCredential // sorted by Provider, ascending
	CredentialsTotal                    int
	CredentialsTruncated                bool
	CredentialsInvalid                  int
	CredentialRemoteValidationPerformed bool
	Occupancy                           []MachineReadOccupancy // sorted by Provider then Source, ascending
	OccupancyTotal                      int
	OccupancyTruncated                  bool
	OccupancyInvalid                    int
	OccupancyRowsSeen                   int
	OccupancyRowsSkipped                int
	OccupancyObserved                   bool
	OccupancyDBObserved                 bool
	OccupancyObservationInvalid         bool
	OccupancyObservedAt                 *ObservationClock
	SystemdUnits                        []MachineReadSystemdUnit // sorted by Name, ascending
	SystemdUnitsTotal                   int
	SystemdUnitsTruncated               bool
	// SystemdUnitsInvalid counts observations that exist but cannot form a
	// trustworthy typed row: undecodable payload, subject/name mismatch,
	// impossible restart count, or missing observation clock.
	SystemdUnitsInvalid   int
	RunSummaries          []model.RunSummary
	RunSummariesTotal     int
	RunSummariesTruncated bool
	// OpenClawObserved means the latest observation existed and decoded.
	OpenClawObserved bool
	// OpenClawDBObserved additionally means that observation carried a DB section.
	OpenClawDBObserved   bool
	RunSummaryObservedAt *ObservationClock
	Journals             []JournalRow // sorted by Unit, ascending
	JournalsTotal        int
	JournalsTruncated    bool
	// JournalsUndecodable counts journal observations whose bytes exist but
	// cannot be read as a UnitJournal. Those units are neither collected and
	// heard nor not collected; the observation still exists.
	JournalsUndecodable          int
	UnitsWithoutJournal          []string // sorted, ascending
	UnitsWithoutJournalTotal     int
	UnitsWithoutJournalTruncated bool
}

// MachineReadOpenClaw is an explicit projection of the latest OpenClaw
// observation. It deliberately carries relationships and capability facts,
// never dedicated unit/package/database path fields, argv, usernames, process
// IDs, or raw occupancy/run rows. Bounded reason text may itself mention a
// path; the Hub does not scan or redact free text.
type MachineReadOpenClaw struct {
	Observed        bool
	Decoded         bool
	ObservedAt      *ObservationClock
	Present         bool
	Reason          string
	CLIVersion      string
	CLIVersionRaw   string
	GatewayVersion  string
	UpstreamVersion string
	CrashBundles    int
	Install         *MachineReadOpenClawInstall
	InstallInvalid  bool
	DB              *MachineReadOpenClawDB
	DBInvalid       bool
}

type MachineReadOpenClawInstall struct {
	UnitFound                bool
	UnitReason               string
	DropInCount              int
	KillMode                 string
	NRestarts                *int
	ActiveEnterAt            *time.Time
	NodeVersion              string
	NodeVersionReason        string
	NpmObserved              bool
	NpmVersion               string
	NpmReason                string
	RunningDirectoryObserved bool
	RunningDirectoryExists   bool
	RunningDirectoryWritable *bool
	RunningVersion           string
	RunningDirectoryReason   string
	ProcessObserved          bool
	ProcessMatchesUnit       *bool
	ProcessReason            string
	ReleaseLayoutObserved    bool
	ReleasesPresent          bool
	CurrentReleaseLinked     bool
	DiskFreeBytes            int64
	DiskFreeMeasured         bool
	DiskFreeReason           string
}

type MachineReadOpenClawDB struct {
	Present                  bool
	Reason                   string
	Layout                   string
	Support                  model.SupportLevel
	UnknownLocationCount     int
	LastTaskEndedAt          *time.Time
	TaskRunRows              int
	TaskStatuses             []MachineReadStatusCount
	TaskStatusesTotal        int
	TaskStatusesTruncated    bool
	TaskStatusesInvalid      int
	LastCronRunAt            *time.Time
	CronRunLogRows           int
	CronStatuses             []MachineReadStatusCount
	CronStatusesTotal        int
	CronStatusesTruncated    bool
	CronStatusesInvalid      int
	CronJobsTotal            int
	CronJobsTotalMeasured    bool
	CronJobsEnabled          int
	CronJobsEnabledMeasured  bool
	NextCronRunAt            *time.Time
	CronJobsOverdue          int
	CronJobsScheduleMeasured bool
	TerminalOutcomePopulated int
}

type MachineReadStatusCount struct {
	Status string
	Count  int
}

// MachineReadCLITool omits every raw host coordinate. RunningRelationship is
// derived while those coordinates are still available, so callers can see an
// installed/running mismatch without receiving either path or a PID.
type MachineReadCLITool struct {
	Name                string
	Present             bool
	OnPath              bool
	PresentEvidence     string
	PathSource          string
	PathReason          string
	DaemonReach         string
	VersionReported     string
	VersionRaw          string
	VersionPackageJSON  string
	VersionReason       string
	SourcesDisagree     bool
	ProcessObserved     bool
	RunningRelationship string
	ProcessScan         string
	RunningReason       string
	Support             model.SupportLevel
	MeasuredAt          time.Time
	ReceivedAt          time.Time
}

// MachineReadCredential is an explicit projection, not an embedded
// model.Credential. In particular it cannot carry ActiveAccountID, tokens,
// hashes, or secret material. ActiveAccountSelected preserves the operational
// fact that a selector exists without disclosing its value.
type MachineReadCredential struct {
	Provider              string
	Status                model.CredStatus
	ExpiresAt             *time.Time
	LastRefresh           *time.Time
	FileMTime             *time.Time
	VerifiedAt            *time.Time
	VerificationMethod    model.VerifyMethod
	ActiveAccountSelected bool
	AccountCount          int
	Note                  string
	LastError             string
	MeasuredAt            time.Time
	ReceivedAt            time.Time
	Lifetime              time.Duration
	RefreshesSeen         int
	WatchedFor            time.Duration
	Peers                 []MachineReadCredentialPeer
	PeersTotal            int
	PeersTruncated        bool
}

type MachineReadCredentialPeer struct {
	DisplayName   string
	RefreshesSeen int
	FileMTime     *time.Time
}

// MachineReadOccupancy is aggregated only from the Hub's append-only
// ticket_occupancy_observation ledger. It cannot carry profile_id,
// occupant_evidence/session keys, job IDs, models, tokens, or error text.
type MachineReadOccupancy struct {
	Provider        string
	Source          string
	Runs            int
	Errors          int
	LastRunAt       time.Time
	LastReceivedAt  time.Time
	Agents          []string
	AgentsTotal     int
	AgentsTruncated bool
}

// MachineReadSystemdUnit is structurally incapable of carrying MainPID. PIDs
// are volatile host-local identifiers and are not part of this disclosure.
type MachineReadSystemdUnit struct {
	Name                 string
	Present              bool
	Measured             bool
	Reason               string
	ActiveState          string
	SubState             string
	ActiveEnterTimestamp *time.Time
	NRestarts            int
	MeasuredAt           time.Time
	ReceivedAt           time.Time
}

// ObservationClock is the relay's two clocks for one observation.
type ObservationClock struct {
	MeasuredAt time.Time // agent clock
	ReceivedAt time.Time // Hub clock
}

func (s *Store) MachineReadEvidence(machineID string, evaluatedAt time.Time, limit int) (MachineReadEvidence, error) {
	if machineID == "" {
		return MachineReadEvidence{}, ErrNotFound
	}
	if limit <= 0 {
		limit = MaxMachineEvidencePageSize
	}
	if limit > MaxMachineEvidencePageSize {
		return MachineReadEvidence{}, fmt.Errorf("store: machine evidence limit must not exceed %d", MaxMachineEvidencePageSize)
	}
	evaluatedAt = evaluatedAt.UTC()
	machine, err := s.GetMachine(machineID)
	if err != nil {
		return MachineReadEvidence{}, err
	}
	result := MachineReadEvidence{MachineID: machine.MachineID, DisplayName: machine.DisplayName}

	if obs, ok, err := s.latestObservationReceivedBy(machineID, KindOpenClaw, KindOpenClaw, evaluatedAt); err != nil {
		return MachineReadEvidence{}, err
	} else if ok {
		result.OpenClaw.Observed = true
		result.OpenClaw.ObservedAt = &ObservationClock{MeasuredAt: obs.MeasuredAt, ReceivedAt: obs.ReceivedAt}
		if oc, valid := unmarshalInto[model.OpenClaw](obs.Payload); valid {
			if oc.CrashBundles >= 0 {
				result.OpenClaw.Decoded = true
				result.OpenClaw.Present = oc.Present
				result.OpenClaw.Reason = oc.Reason
				result.OpenClaw.CLIVersion = oc.CLIVersion
				result.OpenClaw.CLIVersionRaw = oc.CLIVersionRaw
				result.OpenClaw.GatewayVersion = oc.GatewayVersion
				result.OpenClaw.UpstreamVersion = oc.UpstreamVersion
				result.OpenClaw.CrashBundles = oc.CrashBundles
				if oc.Install != nil {
					if install, valid := machineReadOpenClawInstall(oc.Install); valid {
						result.OpenClaw.Install = install
					} else {
						result.OpenClaw.InstallInvalid = true
					}
				}
				if oc.DB != nil {
					if db, valid := machineReadOpenClawDB(oc.DB, limit); valid {
						result.OpenClaw.DB = db
					} else {
						result.OpenClaw.DBInvalid = true
					}
				}
			}
			result.OpenClawObserved = true
			result.RunSummaryObservedAt = &ObservationClock{
				MeasuredAt: obs.MeasuredAt,
				ReceivedAt: obs.ReceivedAt,
			}
			if oc.DB != nil {
				result.OpenClawDBObserved = true
				result.OccupancyObserved = true
				result.OccupancyDBObserved = true
				result.OccupancyObservedAt = &ObservationClock{
					MeasuredAt: obs.MeasuredAt,
					ReceivedAt: obs.ReceivedAt,
				}
				if oc.DB.OccupancyRowsSeen < 0 || oc.DB.OccupancyRowsNoProvider < 0 ||
					oc.DB.OccupancyRowsNoProvider > oc.DB.OccupancyRowsSeen {
					result.OccupancyObservationInvalid = true
				} else {
					result.OccupancyRowsSeen = oc.DB.OccupancyRowsSeen
					result.OccupancyRowsSkipped = oc.DB.OccupancyRowsNoProvider
				}
				result.RunSummariesTotal = len(oc.DB.RecentSummaries)
				pageSize := min(limit, result.RunSummariesTotal)
				result.RunSummaries = append(result.RunSummaries, oc.DB.RecentSummaries[:pageSize]...)
				result.RunSummariesTruncated = result.RunSummariesTotal > pageSize
			}
		}
	}
	if result.OpenClawObserved && !result.OccupancyObserved {
		result.OccupancyObserved = true
		result.OccupancyObservedAt = &ObservationClock{
			MeasuredAt: result.RunSummaryObservedAt.MeasuredAt,
			ReceivedAt: result.RunSummaryObservedAt.ReceivedAt,
		}
	}

	toolObservations, err := s.latestBySubject(machineID, KindCLITool, evaluatedAt)
	if err != nil {
		return MachineReadEvidence{}, err
	}
	allCLITools := make([]MachineReadCLITool, 0, len(toolObservations))
	for _, obs := range toolObservations {
		tool, decoded := unmarshalInto[model.CLITool](obs.Payload)
		if !validMachineReadCLITool(obs, tool, decoded) {
			result.CLIToolsInvalid++
			continue
		}
		allCLITools = append(allCLITools, MachineReadCLITool{
			Name: tool.Name, Present: tool.Present, OnPath: tool.OnPath,
			PresentEvidence: tool.PresentEvidence, PathSource: tool.PathSource, PathReason: tool.PathReason,
			DaemonReach: tool.DaemonReach, VersionReported: tool.VersionReported, VersionRaw: tool.VersionRaw,
			VersionPackageJSON: tool.VersionPackageJSON, VersionReason: tool.VersionReason,
			SourcesDisagree: tool.SourcesDisagree, ProcessObserved: machineReadCLIProcessObserved(tool),
			RunningRelationship: machineReadCLIRunningRelationship(tool), ProcessScan: tool.ProcessScan,
			RunningReason: tool.RunningReason,
			Support:       tool.Support, MeasuredAt: obs.MeasuredAt, ReceivedAt: obs.ReceivedAt,
		})
	}
	sort.Slice(allCLITools, func(i, j int) bool { return allCLITools[i].Name < allCLITools[j].Name })
	result.CLIToolsTotal = len(allCLITools)
	cliPageSize := min(limit, result.CLIToolsTotal)
	result.CLITools = append(result.CLITools, allCLITools[:cliPageSize]...)
	result.CLIToolsTruncated = result.CLIToolsTotal > cliPageSize

	credentialObservations, err := s.latestBySubject(machineID, KindCredential, evaluatedAt)
	if err != nil {
		return MachineReadEvidence{}, err
	}
	refresh, err := s.credRefreshHistory(evaluatedAt)
	if err != nil {
		return MachineReadEvidence{}, err
	}
	allCredentials := make([]MachineReadCredential, 0, len(credentialObservations))
	for _, obs := range credentialObservations {
		credential, valid := unmarshalInto[model.Credential](obs.Payload)
		if !validMachineReadCredential(obs, credential, valid) {
			result.CredentialsInvalid++
			continue
		}
		row := MachineReadCredential{
			Provider: credential.Provider, Status: credential.Status,
			ExpiresAt: credential.ExpiresAt, LastRefresh: credential.LastRefresh,
			FileMTime: credential.FileMTime, VerifiedAt: credential.VerifiedAt,
			VerificationMethod:    credential.VerificationMethod,
			ActiveAccountSelected: credential.ActiveAccountID != "", AccountCount: credential.AccountCount,
			Note: credential.Note, LastError: credential.LastError,
			MeasuredAt: obs.MeasuredAt, ReceivedAt: obs.ReceivedAt,
			Lifetime: credLifetime(credential),
		}
		if credential.VerificationMethod == model.VerifyLiveRequest && credential.VerifiedAt != nil {
			result.CredentialRemoteValidationPerformed = true
		}
		if history, ok := refresh[machineID][credential.Provider]; ok {
			row.RefreshesSeen = history.Refreshes
			if watched := evaluatedAt.Sub(history.FirstSeen); watched > 0 {
				row.WatchedFor = watched
			}
		}
		peers := credPeers(refresh, machineID, credential.Provider)
		row.PeersTotal = len(peers)
		peerPageSize := min(MaxMachineEvidenceCredentialPeers, row.PeersTotal)
		row.Peers = make([]MachineReadCredentialPeer, 0, peerPageSize)
		for _, peer := range peers[:peerPageSize] {
			row.Peers = append(row.Peers, MachineReadCredentialPeer{
				DisplayName: peer.DisplayName, RefreshesSeen: peer.RefreshesSeen, FileMTime: peer.FileMTime,
			})
		}
		row.PeersTruncated = row.PeersTotal > peerPageSize
		allCredentials = append(allCredentials, row)
	}
	sort.Slice(allCredentials, func(i, j int) bool { return allCredentials[i].Provider < allCredentials[j].Provider })
	result.CredentialsTotal = len(allCredentials)
	credentialPageSize := min(limit, result.CredentialsTotal)
	result.Credentials = append(result.Credentials, allCredentials[:credentialPageSize]...)
	result.CredentialsTruncated = result.CredentialsTotal > credentialPageSize

	allOccupancy, invalidOccupancy, err := s.machineReadOccupancy(machineID)
	if err != nil {
		return MachineReadEvidence{}, err
	}
	result.OccupancyInvalid = invalidOccupancy
	result.OccupancyTotal = len(allOccupancy)
	occupancyPageSize := min(limit, result.OccupancyTotal)
	result.Occupancy = append(result.Occupancy, allOccupancy[:occupancyPageSize]...)
	result.OccupancyTruncated = result.OccupancyTotal > occupancyPageSize

	journalObservations, err := s.latestBySubject(machineID, KindJournal, evaluatedAt)
	if err != nil {
		return MachineReadEvidence{}, err
	}
	journalSubjects := make(map[string]struct{}, len(journalObservations))
	allJournals := make([]JournalRow, 0, len(journalObservations))
	for _, obs := range journalObservations {
		journalSubjects[obs.Subject] = struct{}{}
		if journal, ok := unmarshalInto[model.UnitJournal](obs.Payload); ok {
			allJournals = append(allJournals, JournalRow{
				UnitJournal: journal, MeasuredAt: obs.MeasuredAt, ReceivedAt: obs.ReceivedAt,
			})
		} else {
			result.JournalsUndecodable++
		}
	}
	sort.Slice(allJournals, func(i, j int) bool { return allJournals[i].Unit < allJournals[j].Unit })
	result.JournalsTotal = len(allJournals)
	journalPageSize := min(limit, result.JournalsTotal)
	result.Journals = append(result.Journals, allJournals[:journalPageSize]...)
	result.JournalsTruncated = result.JournalsTotal > journalPageSize

	unitObservations, err := s.latestBySubject(machineID, KindSystemd, evaluatedAt)
	if err != nil {
		return MachineReadEvidence{}, err
	}
	allSystemdUnits := make([]MachineReadSystemdUnit, 0, len(unitObservations))
	allUnitsWithoutJournal := make([]string, 0, len(unitObservations))
	for _, obs := range unitObservations {
		unit, valid := unmarshalInto[model.Unit](obs.Payload)
		if !valid || unit.Name == "" || unit.Name != obs.Subject || unit.NRestarts < 0 ||
			obs.MeasuredAt.IsZero() || obs.ReceivedAt.IsZero() ||
			(unit.ActiveEnterTimestamp != nil && unit.ActiveEnterTimestamp.IsZero()) {
			result.SystemdUnitsInvalid++
		} else {
			allSystemdUnits = append(allSystemdUnits, MachineReadSystemdUnit{
				Name: unit.Name, Present: unit.Present, Measured: unit.Measured, Reason: unit.Reason,
				ActiveState: unit.ActiveState, SubState: unit.SubState,
				ActiveEnterTimestamp: unit.ActiveEnterTimestamp, NRestarts: unit.NRestarts,
				MeasuredAt: obs.MeasuredAt, ReceivedAt: obs.ReceivedAt,
			})
		}
		if _, collected := journalSubjects[obs.Subject]; !collected {
			allUnitsWithoutJournal = append(allUnitsWithoutJournal, obs.Subject)
		}
	}
	sort.Slice(allSystemdUnits, func(i, j int) bool { return allSystemdUnits[i].Name < allSystemdUnits[j].Name })
	result.SystemdUnitsTotal = len(allSystemdUnits)
	systemdPageSize := min(limit, result.SystemdUnitsTotal)
	result.SystemdUnits = append(result.SystemdUnits, allSystemdUnits[:systemdPageSize]...)
	result.SystemdUnitsTruncated = result.SystemdUnitsTotal > systemdPageSize
	sort.Strings(allUnitsWithoutJournal)
	result.UnitsWithoutJournalTotal = len(allUnitsWithoutJournal)
	unitPageSize := min(limit, result.UnitsWithoutJournalTotal)
	result.UnitsWithoutJournal = append(result.UnitsWithoutJournal, allUnitsWithoutJournal[:unitPageSize]...)
	result.UnitsWithoutJournalTruncated = result.UnitsWithoutJournalTotal > unitPageSize

	return result, nil
}

func machineReadOpenClawInstall(raw *model.OpenClawInstall) (*MachineReadOpenClawInstall, bool) {
	runningDirectoryObserved := raw != nil && raw.RunningDir != ""
	processObserved := raw != nil && (raw.MainPID > 0 || raw.ProcessIndexJS != "")
	npmObserved := raw != nil && raw.NpmPath != ""
	releaseLayoutObserved := raw != nil && raw.ReleasesDir != ""
	if raw == nil || (raw.NRestarts != nil && *raw.NRestarts < 0) ||
		raw.MainPID < 0 || (raw.ActiveEnterAt != nil && raw.ActiveEnterAt.IsZero()) || raw.DiskFreeBytes < 0 ||
		(!raw.DiskFreeMeasured && raw.DiskFreeBytes != 0) ||
		(!npmObserved && raw.NpmVersion != "") ||
		(!runningDirectoryObserved && (raw.RunningDirExists || raw.RunningDirWritable != nil || raw.RunningDirVersion != "")) ||
		(!raw.RunningDirExists && (raw.RunningDirWritable != nil || raw.RunningDirVersion != "")) ||
		(raw.ProcessMatchesUnit != nil && !processObserved) ||
		(!releaseLayoutObserved && (raw.ReleasesPresent || raw.CurrentLink != "")) {
		return nil, false
	}
	return &MachineReadOpenClawInstall{
		UnitFound: raw.UnitFound, UnitReason: raw.UnitReason, DropInCount: len(raw.DropInPaths),
		KillMode: raw.KillMode, NRestarts: raw.NRestarts, ActiveEnterAt: raw.ActiveEnterAt,
		NodeVersion: raw.NodeVersion, NodeVersionReason: raw.NodeVersionReason,
		NpmObserved: npmObserved, NpmVersion: raw.NpmVersion, NpmReason: raw.NpmReason,
		RunningDirectoryObserved: runningDirectoryObserved, RunningDirectoryExists: raw.RunningDirExists,
		RunningDirectoryWritable: raw.RunningDirWritable, RunningVersion: raw.RunningDirVersion,
		RunningDirectoryReason: raw.RunningDirReason,
		ProcessObserved:        processObserved,
		ProcessMatchesUnit:     raw.ProcessMatchesUnit, ProcessReason: raw.ProcessReason,
		ReleaseLayoutObserved: releaseLayoutObserved, ReleasesPresent: raw.ReleasesPresent,
		CurrentReleaseLinked: raw.CurrentLink != "", DiskFreeBytes: raw.DiskFreeBytes,
		DiskFreeMeasured: raw.DiskFreeMeasured, DiskFreeReason: raw.DiskFreeReason,
	}, true
}

func machineReadOpenClawDB(raw *model.OpenClawDB, limit int) (*MachineReadOpenClawDB, bool) {
	if raw == nil || !validMachineReadSupport(raw.Support) || !validMachineReadOpenClawLayout(raw.Layout) ||
		raw.TaskRunRows < 0 || raw.CronRunLogRows < 0 ||
		raw.CronJobsTotal < 0 || raw.CronJobsEnabled < 0 || raw.CronJobsOverdue < 0 ||
		raw.TerminalOutcomePopulated < 0 ||
		(raw.Support == model.SupportUnsupported && (raw.Present || len(raw.FoundAt) == 0)) ||
		(!raw.CronJobsTotalMeasured && raw.CronJobsTotal != 0) ||
		(!raw.CronJobsEnabledMeasured && raw.CronJobsEnabled != 0) ||
		(raw.CronJobsEnabledMeasured && !raw.CronJobsTotalMeasured) ||
		(raw.CronJobsTotalMeasured && raw.CronJobsEnabledMeasured && raw.CronJobsEnabled > raw.CronJobsTotal) ||
		(!raw.CronJobsScheduleMeasured && (raw.NextCronRunAt != nil || raw.CronJobsOverdue != 0)) ||
		(raw.LastTaskEndedAt != nil && raw.LastTaskEndedAt.IsZero()) ||
		(raw.LastCronRunAt != nil && raw.LastCronRunAt.IsZero()) ||
		(raw.NextCronRunAt != nil && raw.NextCronRunAt.IsZero()) {
		return nil, false
	}
	task, taskTotal, taskTruncated, taskInvalid := machineReadStatusCounts(raw.TaskStatusCount, limit)
	cron, cronTotal, cronTruncated, cronInvalid := machineReadStatusCounts(raw.CronStatusCount, limit)
	return &MachineReadOpenClawDB{
		Present: raw.Present, Reason: raw.Reason, Layout: raw.Layout, Support: raw.Support,
		UnknownLocationCount: len(raw.FoundAt), LastTaskEndedAt: raw.LastTaskEndedAt,
		TaskRunRows: raw.TaskRunRows, TaskStatuses: task, TaskStatusesTotal: taskTotal,
		TaskStatusesTruncated: taskTruncated, TaskStatusesInvalid: taskInvalid,
		LastCronRunAt: raw.LastCronRunAt, CronRunLogRows: raw.CronRunLogRows,
		CronStatuses: cron, CronStatusesTotal: cronTotal, CronStatusesTruncated: cronTruncated,
		CronStatusesInvalid: cronInvalid,
		CronJobsTotal:       raw.CronJobsTotal, CronJobsTotalMeasured: raw.CronJobsTotalMeasured,
		CronJobsEnabled: raw.CronJobsEnabled, CronJobsEnabledMeasured: raw.CronJobsEnabledMeasured,
		NextCronRunAt: raw.NextCronRunAt, CronJobsOverdue: raw.CronJobsOverdue,
		CronJobsScheduleMeasured: raw.CronJobsScheduleMeasured,
		TerminalOutcomePopulated: raw.TerminalOutcomePopulated,
	}, true
}

func machineReadStatusCounts(raw map[string]int, limit int) ([]MachineReadStatusCount, int, bool, int) {
	all := make([]MachineReadStatusCount, 0, len(raw))
	invalid := 0
	for status, count := range raw {
		if status == "" || count < 0 {
			invalid++
			continue
		}
		all = append(all, MachineReadStatusCount{Status: status, Count: count})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Status < all[j].Status })
	total := len(all)
	pageSize := min(limit, total)
	return all[:pageSize], total, total > pageSize, invalid
}

func validMachineReadCLITool(obs observation, tool model.CLITool, decoded bool) bool {
	processObserved := machineReadCLIProcessObserved(tool)
	sourcesDisagree := tool.VersionReported != "" && tool.VersionPackageJSON != "" &&
		tool.VersionReported != tool.VersionPackageJSON
	if !decoded || tool.Name == "" || tool.Name != obs.Subject || tool.RunningPID < 0 ||
		obs.MeasuredAt.IsZero() || obs.ReceivedAt.IsZero() || !validMachineReadSupport(tool.Support) {
		return false
	}
	switch tool.PresentEvidence {
	case "", "path", "process":
	default:
		return false
	}
	switch tool.PathSource {
	case "", model.PathSourceLogin, model.PathSourceDaemon:
	default:
		return false
	}
	switch tool.DaemonReach {
	case "", model.DaemonReachSame, model.DaemonReachShadowed, model.DaemonReachMissing:
	default:
		return false
	}
	switch tool.ProcessScan {
	case "", model.ProcessScanComplete, model.ProcessScanRestricted, model.ProcessScanUnavailable:
	default:
		return false
	}
	if (!tool.Present && (tool.OnPath || tool.PresentEvidence != "" || processObserved)) ||
		(tool.OnPath && !tool.Present) ||
		(tool.PresentEvidence == "path" && (!tool.Present || !tool.OnPath || tool.Path == "")) ||
		(tool.PresentEvidence == "process" && (!tool.Present || tool.OnPath || !processObserved)) ||
		(tool.Support == model.SupportUnsupported && tool.VersionRaw == "") ||
		tool.SourcesDisagree != sourcesDisagree {
		return false
	}
	return true
}

func validMachineReadSupport(support model.SupportLevel) bool {
	return support == "" || support == model.SupportOK || support == model.SupportUnsupported
}

func validMachineReadOpenClawLayout(layout string) bool {
	return layout == "" || layout == "consolidated" || layout == "split"
}

func machineReadCLIProcessObserved(tool model.CLITool) bool {
	return tool.RunningPID > 0 || tool.RunningExe != "" || tool.RunningScript != ""
}

func machineReadCLIRunningRelationship(tool model.CLITool) string {
	if tool.RealPath == "" {
		return ""
	}
	running := tool.RunningExe
	if tool.RunningScript != "" {
		running = tool.RunningScript
	}
	if running == "" {
		return ""
	}
	if running == tool.RealPath {
		return "same"
	}
	return "different"
}

func validMachineReadCredential(obs observation, credential model.Credential, decoded bool) bool {
	if !decoded || credential.Provider == "" || credential.Provider != obs.Subject ||
		obs.MeasuredAt.IsZero() || obs.ReceivedAt.IsZero() || credential.AccountCount < 0 {
		return false
	}
	switch credential.Status {
	case model.CredAbsent, model.CredConfigured, model.CredExpiresSoon,
		model.CredExpired, model.CredUnknown, model.CredFailed:
	default:
		return false
	}
	switch credential.VerificationMethod {
	case "", model.VerifyFileParse, model.VerifyLiveRequest:
	default:
		return false
	}
	if (credential.VerifiedAt != nil) != (credential.VerificationMethod == model.VerifyLiveRequest) {
		return false
	}
	for _, value := range []*time.Time{
		credential.ExpiresAt, credential.LastRefresh, credential.FileMTime, credential.VerifiedAt,
	} {
		if value != nil && value.IsZero() {
			return false
		}
	}
	return true
}

func (s *Store) machineReadOccupancy(machineID string) ([]MachineReadOccupancy, int, error) {
	rows, err := s.rdb.Query(`
SELECT provider, source, COUNT(*), MAX(measured_at), MAX(received_at),
       SUM(CASE WHEN last_error_text IS NOT NULL AND last_error_text != '' THEN 1 ELSE 0 END)
  FROM ticket_occupancy_observation
 WHERE machine_id = ?
 GROUP BY provider, source
 ORDER BY provider ASC, source ASC`, machineID)
	if err != nil {
		return nil, 0, fmt.Errorf("store: machine evidence occupancy: %w", err)
	}
	defer rows.Close()
	var out []MachineReadOccupancy
	invalid := 0
	for rows.Next() {
		var row MachineReadOccupancy
		var lastRunAt, lastReceivedAt string
		if err := rows.Scan(&row.Provider, &row.Source, &row.Runs, &lastRunAt, &lastReceivedAt, &row.Errors); err != nil {
			return nil, 0, fmt.Errorf("store: machine evidence occupancy scan: %w", err)
		}
		row.LastRunAt = parseTime(lastRunAt)
		row.LastReceivedAt = parseTime(lastReceivedAt)
		if row.Provider == "" || row.Source == "" || row.Runs <= 0 || row.Errors < 0 || row.Errors > row.Runs ||
			row.LastRunAt.IsZero() || row.LastReceivedAt.IsZero() {
			invalid++
			continue
		}
		agents, err := s.machineReadOccupancyAgents(machineID, row.Provider, row.Source)
		if err != nil {
			return nil, 0, err
		}
		row.AgentsTotal = len(agents)
		agentPageSize := min(MaxMachineEvidenceOccupancyAgents, row.AgentsTotal)
		row.Agents = append(row.Agents, agents[:agentPageSize]...)
		row.AgentsTruncated = row.AgentsTotal > agentPageSize
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: machine evidence occupancy: %w", err)
	}
	return out, invalid, nil
}

func (s *Store) machineReadOccupancyAgents(machineID, provider, source string) ([]string, error) {
	rows, err := s.rdb.Query(`
SELECT agent_id FROM ticket_occupancy_observation
 WHERE machine_id = ? AND provider = ? AND source = ?
   AND agent_id IS NOT NULL AND agent_id != ''
 GROUP BY agent_id
 ORDER BY MAX(measured_at) DESC, agent_id ASC`, machineID, provider, source)
	if err != nil {
		return nil, fmt.Errorf("store: machine evidence occupancy agents: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var agent string
		if err := rows.Scan(&agent); err != nil {
			return nil, fmt.Errorf("store: machine evidence occupancy agent scan: %w", err)
		}
		out = append(out, agent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: machine evidence occupancy agents: %w", err)
	}
	return out, nil
}
