package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ledgerColumnSpec is intentionally compact because this is a historical
// compatibility manifest, not a second copy of schema.sql. Each entry below
// is name:type:not-null:default:primary-key-ordinal; "-" means no default.
// PRAGMA table_info reports TEXT PRIMARY KEY as not-null=0 on SQLite rowid
// tables, so the manifest records SQLite's actual metadata rather than
// assuming that every primary key has the same not-null bit.
type ledgerColumnInvariant struct {
	name         string
	declaredType string
	notNull      int
	defaultSQL   string
	hasDefault   bool
	primaryKey   int
	hidden       int
}

type ledgerForeignKeyInvariant struct {
	column           string
	referencedTable  string
	referencedColumn string
	onUpdate         string
	onDelete         string
	match            string
}

func ledgerForeignKey(column, referencedTable, referencedColumn string) ledgerForeignKeyInvariant {
	return ledgerForeignKeyInvariant{
		column:           column,
		referencedTable:  referencedTable,
		referencedColumn: referencedColumn,
		onUpdate:         "NO ACTION",
		onDelete:         "NO ACTION",
		match:            "NONE",
	}
}

type ledgerTableInvariant struct {
	name string

	// requiredColumns is the complete 1c08e24 table shape, except that the
	// occupancy columns replaced in 744d52c are represented as variants.
	// columnVariants lists the only extra-column sets committed migrations can
	// produce. An empty list means that no extra columns are valid.
	requiredColumns string
	columnVariants  []string
	// replacementLegacyColumns/replacementNewColumns describe 744d52c's one
	// non-additive schema declaration. A fresh database has all new and no
	// legacy columns. An upgraded database has all legacy columns and, because
	// that release issued unordered ALTERs outside a transaction, any subset of
	// the new columns. No other combination is reachable.
	replacementLegacyColumns string
	replacementNewColumns    string
	foreignKeys              []ledgerForeignKeyInvariant
	uniqueKeys               [][]string
	uniqueIndexes            [][]string
}

var baselineLedgerTables = []ledgerTableInvariant{
	{
		name: "machine_registry",
		requiredColumns: `machine_id:TEXT:0:-:1 display_name:TEXT:1:-:0 hostname:TEXT:0:-:0
			expected:INTEGER:1:1:0 unix_user:TEXT:0:-:0 os:TEXT:0:-:0 arch:TEXT:0:-:0
			tailscale_ip:TEXT:0:-:0 machine_id_hint:TEXT:0:-:0 agent_token_hash:TEXT:0:-:0
			linger_enabled:INTEGER:0:-:0 notes:TEXT:0:-:0 created_at:TEXT:1:-:0
			enrolled_at:TEXT:0:-:0 retired_at:TEXT:0:-:0`,
		columnVariants: []string{
			``,
			`channel:TEXT:0:-:0`,
			`channel:TEXT:0:-:0 channel_revision:INTEGER:1:0:0`,
			`channel:TEXT:0:-:0 channel_revision:INTEGER:1:0:0 lifecycle_revision:INTEGER:1:0:0`,
			`channel:TEXT:0:-:0 channel_revision:INTEGER:1:0:0 lifecycle_revision:INTEGER:1:0:0 assigned_user_id:TEXT:0:-:0 assigned_user_login:TEXT:0:-:0 assigned_user_revision:INTEGER:1:0:0`,
		},
	},
	{
		name: "enrollment_tokens",
		requiredColumns: `token_hash:TEXT:0:-:1 display_name:TEXT:1:-:0 created_at:TEXT:1:-:0
			expires_at:TEXT:1:-:0 used_at:TEXT:0:-:0 used_by:TEXT:0:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("used_by", "machine_registry", "machine_id"),
		},
	},
	{
		name: "machine_checkins",
		requiredColumns: `machine_id:TEXT:1:-:1 sent_at:TEXT:1:-:2 received_at:TEXT:1:-:0
			agent_version:TEXT:0:-:0 boot_id:TEXT:0:-:0 agent_seq:INTEGER:0:-:0
			uptime_seconds:INTEGER:0:-:0 disk_free_bytes:INTEGER:0:-:0 disk_total_bytes:INTEGER:0:-:0
			observation_age_seconds:INTEGER:0:-:0 max_seen_revision:INTEGER:1:0:0
			max_applied_revision:INTEGER:1:0:0 clock_skew_seconds:INTEGER:0:-:0`,
		columnVariants: []string{``, `agent_started_at:TEXT:0:-:0`,
			`agent_started_at:TEXT:0:-:0 jobs_enabled:INTEGER:0:-:0`,
			`agent_started_at:TEXT:0:-:0 jobs_enabled:INTEGER:0:-:0 settings_digest:TEXT:0:-:0`},
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
		},
	},
	{
		name: "observed_state",
		requiredColumns: `observation_id:TEXT:0:-:1 machine_id:TEXT:1:-:0 measured_at:TEXT:1:-:0
			received_at:TEXT:1:-:0 kind:TEXT:1:-:0 subject:TEXT:1:-:0 payload:TEXT:1:-:0 source:TEXT:1:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
		},
	},
	{
		name: "credential_profile",
		requiredColumns: `profile_id:TEXT:0:-:1 profile_name:TEXT:1:-:0 provider:TEXT:1:-:0 kind:TEXT:1:-:0
			account_label:TEXT:0:-:0 secret_ref:TEXT:0:-:0 soft_cap_machines:INTEGER:0:-:0
			planned_rotation_at:TEXT:0:-:0 revoked_at:TEXT:0:-:0 created_at:TEXT:1:-:0`,
		uniqueKeys: [][]string{{"profile_name"}},
	},
	{
		name: "credential_on_machine",
		requiredColumns: `machine_id:TEXT:1:-:1 provider:TEXT:1:-:2 profile_id:TEXT:0:-:0 status:TEXT:1:-:0
			reported_expiry_at:TEXT:0:-:0 last_refresh_at:TEXT:0:-:0 file_mtime:TEXT:0:-:0
			active_account_id:TEXT:0:-:0 verified_at:TEXT:0:-:0 verification_method:TEXT:0:-:0
			last_real_use_at:TEXT:0:-:0 last_error_category:TEXT:0:-:0 last_error:TEXT:0:-:0
			note:TEXT:0:-:0 updated_at:TEXT:1:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
			ledgerForeignKey("profile_id", "credential_profile", "profile_id"),
		},
	},
	{
		name: "ticket_static_assignment",
		requiredColumns: `profile_id:TEXT:1:-:1 machine_id:TEXT:1:-:2 assigned_at:TEXT:1:-:0
			assigned_by:TEXT:1:-:0 desired:INTEGER:1:1:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("profile_id", "credential_profile", "profile_id"),
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
		},
	},
	{
		name: "ticket_occupancy_observation",
		requiredColumns: `observation_id:TEXT:0:-:1 machine_id:TEXT:1:-:0 profile_id:TEXT:0:-:0
			provider:TEXT:1:-:0 measured_at:TEXT:1:-:0 received_at:TEXT:1:-:0
			occupant_evidence:TEXT:1:-:0 process_alive:INTEGER:1:0:0 source:TEXT:1:-:0`,
		replacementLegacyColumns: `kind:TEXT:0:-:0 last_success_at:TEXT:0:-:0
			last_success_task_id:TEXT:0:-:0 last_error_category:TEXT:0:-:0 last_error_at:TEXT:0:-:0
		`,
		replacementNewColumns: `model:TEXT:0:-:0 job_id:TEXT:0:-:0 agent_id:TEXT:0:-:0 run_status:TEXT:0:-:0
			total_tokens:INTEGER:0:-:0 last_error_text:TEXT:0:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
			ledgerForeignKey("profile_id", "credential_profile", "profile_id"),
		},
	},
	{
		name: "credential_ledger_event",
		requiredColumns: `event_id:TEXT:0:-:1 profile_id:TEXT:0:-:0 machine_id:TEXT:0:-:0 provider:TEXT:0:-:0
			event_type:TEXT:1:-:0 occurred_at:TEXT:1:-:0 received_at:TEXT:1:-:0
			evidence_ref:TEXT:0:-:0 actor:TEXT:1:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("profile_id", "credential_profile", "profile_id"),
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
		},
	},
	{
		name: "machine_state_history",
		requiredColumns: `machine_id:TEXT:1:-:1 state:TEXT:1:-:0 reason:TEXT:1:-:0
			entered_at:TEXT:1:-:2 left_at:TEXT:0:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
		},
	},
	{
		name: "notifications",
		requiredColumns: `notification_id:TEXT:0:-:1 kind:TEXT:1:-:0 channel:TEXT:1:-:0 sent_at:TEXT:1:-:0
			body:TEXT:1:-:0 delivered:INTEGER:1:0:0 error:TEXT:0:-:0`,
	},
	{
		name: "desired_state",
		requiredColumns: `desired_id:TEXT:0:-:1 scope_type:TEXT:1:-:0 scope_id:TEXT:1:-:0
			resource_kind:TEXT:1:-:0 resource_id:TEXT:1:-:0 revision:INTEGER:1:-:0 spec:TEXT:1:-:0
			created_at:TEXT:1:-:0 created_by:TEXT:1:-:0`,
	},
	{
		name:            "revision_counters",
		requiredColumns: `resource_scope:TEXT:0:-:1 current_revision:INTEGER:1:-:0`,
	},
	{
		name: "jobs",
		requiredColumns: `job_id:TEXT:0:-:1 machine_id:TEXT:1:-:0 desired_id:TEXT:0:-:0 revision:INTEGER:1:-:0
			state:TEXT:1:-:0 lease_token:TEXT:0:-:0 lease_expires_at:TEXT:0:-:0
			execution_timeout:INTEGER:1:900:0 artifact_digest:TEXT:0:-:0 irreversible:INTEGER:1:0:0
			created_at:TEXT:1:-:0 terminal_at:TEXT:0:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
			ledgerForeignKey("desired_id", "desired_state", "desired_id"),
		},
	},
	{
		name: "job_events",
		requiredColumns: `event_id:TEXT:0:-:1 job_id:TEXT:1:-:0 seq:INTEGER:1:-:0 phase:TEXT:1:-:0
			occurred_at:TEXT:1:-:0 received_at:TEXT:1:-:0 payload:TEXT:1:-:0`,
		columnVariants: []string{
			``,
			`producer_kind:TEXT:1:'executor_agent':0 producer_id:TEXT:1:'':0
			 evidence_role:TEXT:1:'executor':0 authority:TEXT:1:'machine_bearer_lease':0
			 provenance_recorded:INTEGER:1:0:0`,
		},
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("job_id", "jobs", "job_id"),
		},
		uniqueKeys: [][]string{{"job_id", "seq"}},
	},
	{
		name: "verification_results",
		requiredColumns: `verification_id:TEXT:0:-:1 job_id:TEXT:1:-:0 machine_id:TEXT:1:-:0
			rule_id:TEXT:1:-:0 command:TEXT:1:-:0 exit_code:INTEGER:0:-:0 stdout_excerpt:TEXT:0:-:0
			stderr_excerpt:TEXT:0:-:0 passed:INTEGER:1:-:0 verified_at:TEXT:1:-:0`,
		columnVariants: []string{
			``,
			`producer_kind:TEXT:1:'executor_agent':0 producer_id:TEXT:1:'':0
			 evidence_role:TEXT:1:'executor':0 authority:TEXT:1:'machine_bearer_lease':0
			 provenance_recorded:INTEGER:1:0:0 received_at:TEXT:1:'':0`,
			`producer_kind:TEXT:1:'executor_agent':0 producer_id:TEXT:1:'':0
			 evidence_role:TEXT:1:'executor':0 authority:TEXT:1:'machine_bearer_lease':0
			 provenance_recorded:INTEGER:1:0:0 received_at:TEXT:1:'':0
			 observed_digest:TEXT:1:'':0 verifier_id:TEXT:1:'':0`,
			`producer_kind:TEXT:1:'executor_agent':0 producer_id:TEXT:1:'':0
			 evidence_role:TEXT:1:'executor':0 authority:TEXT:1:'machine_bearer_lease':0
			 provenance_recorded:INTEGER:1:0:0 received_at:TEXT:1:'':0
			 observed_digest:TEXT:1:'':0 observed_version:TEXT:1:'':0 verifier_id:TEXT:1:'':0`,
		},
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("job_id", "jobs", "job_id"),
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
		},
	},
	{
		name:            "schema_meta",
		requiredColumns: `key:TEXT:0:-:1 value:TEXT:1:-:0`,
	},
}

var optionalLedgerTables = []ledgerTableInvariant{
	{
		name: `machine_job_capabilities`,
		requiredColumns: `machine_id:TEXT:1:-:1 sent_at:TEXT:1:-:2
			capability:TEXT:1:-:3 supported:INTEGER:1:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			{column: "machine_id", referencedTable: "machine_checkins", referencedColumn: "machine_id",
				onUpdate: "NO ACTION", onDelete: "CASCADE", match: "NONE"},
			{column: "sent_at", referencedTable: "machine_checkins", referencedColumn: "sent_at",
				onUpdate: "NO ACTION", onDelete: "CASCADE", match: "NONE"},
		},
	},
	{
		name: `verifiers`,
		requiredColumns: `verifier_id:TEXT:0:-:1 kind:TEXT:1:-:0 display_name:TEXT:1:-:0
			failure_domain:TEXT:1:-:0 credential_hash:TEXT:1:-:0 created_at:TEXT:1:-:0
			revoked_at:TEXT:0:-:0 last_seen_at:TEXT:0:-:0 revision:INTEGER:1:-:0`,
		uniqueIndexes: [][]string{{"display_name"}},
	},
	{
		name: `verification_assignments`,
		requiredColumns: `assignment_id:TEXT:0:-:1 job_id:TEXT:1:-:0
			verifier_id:TEXT:1:-:0 assigned_at:TEXT:1:-:0 assigned_by:TEXT:1:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("job_id", "jobs", "job_id"),
			ledgerForeignKey("verifier_id", "verifiers", "verifier_id"),
		},
	},
	{
		name: `job_dependencies`,
		requiredColumns: `job_id:TEXT:1:-:1 prerequisite_job_id:TEXT:1:-:2
			position:INTEGER:1:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("job_id", "jobs", "job_id"),
			ledgerForeignKey("prerequisite_job_id", "jobs", "job_id"),
		},
		uniqueKeys: [][]string{{"job_id", "position"}},
	},
	{
		name: `machine_profile_assignments`,
		requiredColumns: `assignment_id:TEXT:0:-:1 machine_id:TEXT:1:-:0
			assignment_revision:INTEGER:1:-:0 profile_id:TEXT:1:-:0 profile_revision:INTEGER:1:-:0
			profile_digest:TEXT:1:-:0 target_os:TEXT:1:-:0 target_arch:TEXT:1:-:0
			assigned_at:TEXT:1:-:0 assigned_by:TEXT:1:-:0 supersedes_assignment_id:TEXT:0:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
			ledgerForeignKey("profile_id", "machine_profiles", "profile_id"),
			ledgerForeignKey("profile_revision", "machine_profiles", "profile_revision"),
			ledgerForeignKey("supersedes_assignment_id", "machine_profile_assignments", "assignment_id"),
		},
		uniqueKeys: [][]string{{"machine_id", "assignment_revision"}},
	},
	{
		name: `machine_profile_assignment_packages`,
		requiredColumns: `assignment_id:TEXT:1:-:1 position:INTEGER:1:-:2 package_id:TEXT:1:-:0
			package_version:TEXT:1:-:0 manifest_digest:TEXT:1:-:0 desired_id:TEXT:1:-:0
			job_id:TEXT:1:-:0 direct:INTEGER:1:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("assignment_id", "machine_profile_assignments", "assignment_id"),
			ledgerForeignKey("package_id", "catalog_manifests", "package_id"),
			ledgerForeignKey("package_version", "catalog_manifests", "package_version"),
			ledgerForeignKey("desired_id", "desired_state", "desired_id"),
			ledgerForeignKey("job_id", "jobs", "job_id"),
		},
		uniqueKeys: [][]string{
			{"desired_id"}, {"job_id"}, {"assignment_id", "package_id"},
		},
	},
	{
		name: `agent_sessions`,
		requiredColumns: `session_id:TEXT:0:-:1 machine_id:TEXT:1:-:0
			operator_tailnet_user_id:TEXT:1:-:0 operator_tailnet_user_login:TEXT:1:-:0
			opened_at:TEXT:1:-:0 closed_at:TEXT:0:-:0 close_reason:TEXT:0:-:0`,
		foreignKeys: []ledgerForeignKeyInvariant{
			ledgerForeignKey("machine_id", "machine_registry", "machine_id"),
		},
	},
	{
		name: `object_blobs`,
		requiredColumns: `digest:TEXT:0:-:1 size_bytes:INTEGER:1:-:0 object_key:TEXT:1:-:0
			backend:TEXT:1:-:0 kind:TEXT:1:-:0 media_type:TEXT:1:-:0 created_at:TEXT:1:-:0`,
	},
}

// additiveLedgerColumns extends identity checking to security-relevant columns
// on post-Phase-1 tables without making those later tables prerequisites for a
// legitimate historical ledger. Absence is a supported pre-migration shape;
// once present, the column metadata must be exact.
var additiveLedgerColumns = []struct {
	table  string
	column ledgerColumnInvariant
}{
	{table: "hub_accounts", column: ledgerColumnInvariant{name: "email", declaredType: "TEXT"}},
	{table: "hub_accounts", column: ledgerColumnInvariant{name: "role", declaredType: "TEXT", notNull: 1, hasDefault: true, defaultSQL: "'admin'"}},
	{
		table: "deployments",
		column: ledgerColumnInvariant{
			name: "control_revision", declaredType: "INTEGER", notNull: 1,
			hasDefault: true, defaultSQL: "0",
		},
	},
	{
		table: "deployments",
		column: ledgerColumnInvariant{
			name: "pause_after_canary", declaredType: "INTEGER", notNull: 1,
			hasDefault: true, defaultSQL: "0",
		},
	},
	{
		table: "verification_results",
		column: ledgerColumnInvariant{
			name: "producer_kind", declaredType: "TEXT", notNull: 1,
			hasDefault: true, defaultSQL: "'executor_agent'",
		},
	},
	{
		table: "verification_results",
		column: ledgerColumnInvariant{
			name: "producer_id", declaredType: "TEXT", notNull: 1,
			hasDefault: true, defaultSQL: "''",
		},
	},
	{
		table: "verification_results",
		column: ledgerColumnInvariant{
			name: "evidence_role", declaredType: "TEXT", notNull: 1,
			hasDefault: true, defaultSQL: "'executor'",
		},
	},
	{
		table: "verification_results",
		column: ledgerColumnInvariant{
			name: "authority", declaredType: "TEXT", notNull: 1,
			hasDefault: true, defaultSQL: "'machine_bearer_lease'",
		},
	},
	{
		table: "verification_results",
		column: ledgerColumnInvariant{
			name: "provenance_recorded", declaredType: "INTEGER", notNull: 1,
			hasDefault: true, defaultSQL: "0",
		},
	},
	{
		table: "verification_results",
		column: ledgerColumnInvariant{
			name: "received_at", declaredType: "TEXT", notNull: 1,
			hasDefault: true, defaultSQL: "''",
		},
	},
	{
		table: "verification_results",
		column: ledgerColumnInvariant{
			name: "observed_digest", declaredType: "TEXT", notNull: 1,
			hasDefault: true, defaultSQL: "''",
		},
	},
	{
		table: "verification_results",
		column: ledgerColumnInvariant{
			name: "observed_version", declaredType: "TEXT", notNull: 1,
			hasDefault: true, defaultSQL: "''",
		},
	},
	{
		table: "verification_results",
		column: ledgerColumnInvariant{
			name: "verifier_id", declaredType: "TEXT", notNull: 1,
			hasDefault: true, defaultSQL: "''",
		},
	},
}

// additiveLedgerUniqueIndexes lists the independently created UNIQUE indexes
// that a historical ledger may not have reached yet. Absence is valid; once
// present, the index shape is validated by the same closed-set rules as every
// required PK and UNIQUE constraint.
var additiveLedgerUniqueIndexes = []struct {
	table   string
	columns []string
}{
	{table: "desired_state", columns: []string{"resource_kind", "resource_id", "revision"}},
}

type ledgerIndexColumnInvariant struct {
	name       string
	descending int
}

// ValidateExistingLedger identifies an existing clawctl evidence ledger
// without migrating or otherwise writing it. It is the content half of the
// stopped-service break-glass preflight; pathname ownership/alias checks live
// in ledgerlock so the two checks can be repeated independently around locks
// and the systemd stopped proof.
//
// clawctl had no application_id in its first release. Its durable identity is
// therefore the complete Phase 1 schema: all 17 tables, stable column metadata,
// keys, foreign keys, and schema_meta version=1. Performance indexes are not
// identity: schema.sql safely recreates them. The small compatibility lists
// above come from committed additive migrations, not heuristics.
func ValidateExistingLedger(path string) error {
	db, err := sql.Open("sqlite", rollbackReadOnlyDSN(path))
	if err != nil {
		return fmt.Errorf("store: open ledger identity read-only: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store: begin ledger identity read: %w", err)
	}
	defer tx.Rollback()

	var integrity string
	if err := tx.QueryRow(`PRAGMA quick_check(1)`).Scan(&integrity); err != nil {
		return fmt.Errorf("store: read ledger quick_check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("store: ledger quick_check failed: %s", integrity)
	}

	for _, table := range baselineLedgerTables {
		if err := validateLedgerTable(tx, table); err != nil {
			return err
		}
	}
	for _, additive := range additiveLedgerColumns {
		if err := validateAdditiveLedgerColumn(tx, additive.table, additive.column); err != nil {
			return err
		}
	}
	for _, table := range optionalLedgerTables {
		var present int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_list
 WHERE schema='main' AND name=?`, table.name).Scan(&present); err != nil {
			return fmt.Errorf("store: identify optional ledger table %s: %w", table.name, err)
		}
		if present == 1 {
			if err := validateLedgerTable(tx, table); err != nil {
				return err
			}
		} else if present > 1 {
			return fmt.Errorf("store: optional ledger table %s identity is ambiguous", table.name)
		}
	}
	var version string
	if err := tx.QueryRow(`SELECT value FROM schema_meta WHERE key='version'`).Scan(&version); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("store: file is not a clawctl ledger: schema_meta version row is missing")
		}
		return fmt.Errorf("store: read ledger schema version: %w", err)
	}
	if version != "1" {
		return fmt.Errorf("store: unsupported clawctl ledger schema version %q", version)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: finish ledger identity read: %w", err)
	}
	return nil
}

func validateAdditiveLedgerColumn(tx dbTx, table string, expected ledgerColumnInvariant) error {
	var objectType string
	err := tx.QueryRow(`SELECT type FROM sqlite_schema WHERE name=? AND type IN ('table','view')`, table).Scan(&objectType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: identify additive ledger column %s.%s: %w", table, expected.name, err)
	}
	if objectType != "table" {
		return fmt.Errorf("store: file is not a clawctl ledger: %s is not an ordinary clawctl table", table)
	}
	actual, err := readLedgerColumns(tx, table)
	if err != nil {
		return err
	}
	found, ok := actual[expected.name]
	if !ok {
		return nil
	}
	return compareLedgerColumn(table, expected, found)
}

func parseLedgerColumns(spec string) ([]ledgerColumnInvariant, error) {
	fields := strings.Fields(spec)
	columns := make([]ledgerColumnInvariant, 0, len(fields))
	for _, field := range fields {
		parts := strings.Split(field, ":")
		if len(parts) != 5 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("invalid column entry %q", field)
		}
		notNull, err := strconv.Atoi(parts[2])
		if err != nil || (notNull != 0 && notNull != 1) {
			return nil, fmt.Errorf("invalid not-null bit in %q", field)
		}
		primaryKey, err := strconv.Atoi(parts[4])
		if err != nil || primaryKey < 0 {
			return nil, fmt.Errorf("invalid primary-key ordinal in %q", field)
		}
		column := ledgerColumnInvariant{
			name:         parts[0],
			declaredType: parts[1],
			notNull:      notNull,
			primaryKey:   primaryKey,
		}
		if parts[3] != "-" {
			column.hasDefault = true
			column.defaultSQL = parts[3]
		}
		columns = append(columns, column)
	}
	return columns, nil
}

func validateLedgerTable(tx dbTx, expected ledgerTableInvariant) error {
	var tableType string
	var withoutRowID, strict int
	if err := tx.QueryRow(`SELECT type,wr,strict FROM pragma_table_list WHERE schema='main' AND name=?`, expected.name).
		Scan(&tableType, &withoutRowID, &strict); err != nil {
		if err != sql.ErrNoRows {
			return fmt.Errorf("store: identify ledger table %s: %w", expected.name, err)
		}
		return fmt.Errorf("store: file is not a clawctl ledger: required table %s is missing", expected.name)
	}
	if tableType != "table" || withoutRowID != 0 || strict != 0 {
		return fmt.Errorf("store: file is not a clawctl ledger: %s is not an ordinary clawctl table", expected.name)
	}

	requiredColumns, err := parseLedgerColumns(expected.requiredColumns)
	if err != nil {
		return fmt.Errorf("store: internal ledger manifest for %s: %w", expected.name, err)
	}
	actual, err := readLedgerColumns(tx, expected.name)
	if err != nil {
		return err
	}
	required := make(map[string]ledgerColumnInvariant, len(requiredColumns))
	for _, column := range requiredColumns {
		required[column.name] = column
	}

	variantSpecs := expected.columnVariants
	if len(variantSpecs) == 0 {
		variantSpecs = []string{``}
	}
	variants := make([][]ledgerColumnInvariant, 0, len(variantSpecs))
	knownOptional := make(map[string]ledgerColumnInvariant)
	for _, spec := range variantSpecs {
		variant, err := parseLedgerColumns(spec)
		if err != nil {
			return fmt.Errorf("store: internal ledger variant manifest for %s: %w", expected.name, err)
		}
		variants = append(variants, variant)
		for _, column := range variant {
			knownOptional[column.name] = column
		}
	}
	legacyReplacement, err := parseLedgerColumns(expected.replacementLegacyColumns)
	if err != nil {
		return fmt.Errorf("store: internal ledger legacy-column manifest for %s: %w", expected.name, err)
	}
	newReplacement, err := parseLedgerColumns(expected.replacementNewColumns)
	if err != nil {
		return fmt.Errorf("store: internal ledger replacement-column manifest for %s: %w", expected.name, err)
	}
	replacementNames := make(map[string]bool, len(legacyReplacement)+len(newReplacement))
	for _, column := range append(append([]ledgerColumnInvariant(nil), legacyReplacement...), newReplacement...) {
		knownOptional[column.name] = column
		replacementNames[column.name] = true
	}

	for name, column := range required {
		found, ok := actual[name]
		if !ok {
			return fmt.Errorf("store: file is not a clawctl ledger: required column %s.%s is missing", expected.name, name)
		}
		if err := compareLedgerColumn(expected.name, column, found); err != nil {
			return err
		}
	}
	var presentOptional []string
	for name, found := range actual {
		if _, ok := required[name]; ok {
			continue
		}
		column, ok := knownOptional[name]
		if !ok {
			return fmt.Errorf("store: file is not a clawctl ledger: unexpected column %s.%s", expected.name, name)
		}
		if err := compareLedgerColumn(expected.name, column, found); err != nil {
			return err
		}
		if !replacementNames[name] {
			presentOptional = append(presentOptional, name)
		}
	}
	sort.Strings(presentOptional)
	validVariant := false
	for _, variant := range variants {
		names := make([]string, 0, len(variant))
		for _, column := range variant {
			names = append(names, column.name)
		}
		sort.Strings(names)
		if strings.Join(names, "\x00") == strings.Join(presentOptional, "\x00") {
			validVariant = true
			break
		}
	}
	if !validVariant {
		return fmt.Errorf("store: file is not a clawctl ledger: unsupported column combination on %s", expected.name)
	}
	if len(legacyReplacement) != 0 || len(newReplacement) != 0 {
		legacyPresent := countPresentLedgerColumns(actual, legacyReplacement)
		newPresent := countPresentLedgerColumns(actual, newReplacement)
		freshShape := legacyPresent == 0 && newPresent == len(newReplacement)
		upgradedShape := legacyPresent == len(legacyReplacement)
		if !freshShape && !upgradedShape {
			return fmt.Errorf("store: file is not a clawctl ledger: unsupported replacement-column combination on %s", expected.name)
		}
	}

	if err := validateLedgerForeignKeys(tx, expected); err != nil {
		return err
	}
	if err := validateLedgerUniqueKeys(tx, expected, requiredColumns); err != nil {
		return err
	}
	return nil
}

func countPresentLedgerColumns(actual map[string]ledgerColumnInvariant, columns []ledgerColumnInvariant) int {
	count := 0
	for _, column := range columns {
		if _, ok := actual[column.name]; ok {
			count++
		}
	}
	return count
}

func readLedgerColumns(tx dbTx, table string) (map[string]ledgerColumnInvariant, error) {
	rows, err := tx.Query(`SELECT name,type,"notnull",dflt_value,pk,hidden FROM pragma_table_xinfo(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("store: identify ledger columns for %s: %w", table, err)
	}
	defer rows.Close()
	out := make(map[string]ledgerColumnInvariant)
	for rows.Next() {
		var column ledgerColumnInvariant
		var defaultValue sql.NullString
		if err := rows.Scan(&column.name, &column.declaredType, &column.notNull, &defaultValue, &column.primaryKey, &column.hidden); err != nil {
			return nil, fmt.Errorf("store: scan ledger columns for %s: %w", table, err)
		}
		column.hasDefault = defaultValue.Valid
		column.defaultSQL = defaultValue.String
		out[column.name] = column
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read ledger columns for %s: %w", table, err)
	}
	return out, nil
}

func compareLedgerColumn(table string, expected, actual ledgerColumnInvariant) error {
	if actual.declaredType != expected.declaredType || actual.notNull != expected.notNull ||
		actual.hasDefault != expected.hasDefault || actual.defaultSQL != expected.defaultSQL ||
		actual.primaryKey != expected.primaryKey || actual.hidden != 0 {
		return fmt.Errorf("store: file is not a clawctl ledger: column %s.%s has incompatible type or constraints", table, expected.name)
	}
	return nil
}

func validateLedgerForeignKeys(tx dbTx, expected ledgerTableInvariant) error {
	rows, err := tx.Query(`SELECT "from","table","to",on_update,on_delete,match FROM pragma_foreign_key_list(?)`, expected.name)
	if err != nil {
		return fmt.Errorf("store: identify ledger foreign keys for %s: %w", expected.name, err)
	}
	defer rows.Close()
	actual := make(map[ledgerForeignKeyInvariant]int)
	actualCount := 0
	for rows.Next() {
		var foreignKey ledgerForeignKeyInvariant
		if err := rows.Scan(&foreignKey.column, &foreignKey.referencedTable, &foreignKey.referencedColumn,
			&foreignKey.onUpdate, &foreignKey.onDelete, &foreignKey.match); err != nil {
			return fmt.Errorf("store: scan ledger foreign keys for %s: %w", expected.name, err)
		}
		actual[foreignKey]++
		actualCount++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read ledger foreign keys for %s: %w", expected.name, err)
	}
	wanted := make(map[ledgerForeignKeyInvariant]int)
	for _, foreignKey := range expected.foreignKeys {
		wanted[foreignKey]++
	}
	if actualCount != len(expected.foreignKeys) {
		return fmt.Errorf("store: file is not a clawctl ledger: table %s has incompatible foreign keys", expected.name)
	}
	for foreignKey, count := range wanted {
		if actual[foreignKey] != count {
			return fmt.Errorf("store: file is not a clawctl ledger: table %s has incompatible foreign keys", expected.name)
		}
	}
	return nil
}

type ledgerUniqueKeyInvariant struct {
	origin  string
	columns string
}

func validateLedgerUniqueKeys(tx dbTx, expected ledgerTableInvariant, requiredColumns []ledgerColumnInvariant) error {
	rows, err := tx.Query(`SELECT name,origin,partial FROM pragma_index_list(?) WHERE "unique"=1`, expected.name)
	if err != nil {
		return fmt.Errorf("store: identify ledger unique keys for %s: %w", expected.name, err)
	}
	type indexEntry struct {
		name    string
		origin  string
		partial int
	}
	var indexes []indexEntry
	for rows.Next() {
		var index indexEntry
		if err := rows.Scan(&index.name, &index.origin, &index.partial); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: scan ledger unique keys for %s: %w", expected.name, err)
		}
		indexes = append(indexes, index)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("store: read ledger unique keys for %s: %w", expected.name, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: close ledger unique keys for %s: %w", expected.name, err)
	}
	actual := make(map[ledgerUniqueKeyInvariant]int)
	for _, index := range indexes {
		if index.partial != 0 || (index.origin != "pk" && index.origin != "u" && index.origin != "c") {
			return fmt.Errorf("store: file is not a clawctl ledger: table %s has an unsupported UNIQUE constraint", expected.name)
		}
		columns, err := readLedgerIndexColumns(tx, index.name)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(columns))
		for _, column := range columns {
			if column.descending != 0 {
				return fmt.Errorf("store: file is not a clawctl ledger: table %s has an unsupported descending UNIQUE constraint", expected.name)
			}
			names = append(names, column.name)
		}
		actual[ledgerUniqueKeyInvariant{origin: index.origin, columns: strings.Join(names, "\x00")}]++
	}

	primaryKey := append([]ledgerColumnInvariant(nil), requiredColumns...)
	sort.Slice(primaryKey, func(i, j int) bool { return primaryKey[i].primaryKey < primaryKey[j].primaryKey })
	var primaryNames []string
	for _, column := range primaryKey {
		if column.primaryKey != 0 {
			primaryNames = append(primaryNames, column.name)
		}
	}
	wanted := make(map[ledgerUniqueKeyInvariant]int)
	if len(primaryNames) != 0 {
		wanted[ledgerUniqueKeyInvariant{origin: "pk", columns: strings.Join(primaryNames, "\x00")}]++
	}
	for _, columns := range expected.uniqueKeys {
		wanted[ledgerUniqueKeyInvariant{origin: "u", columns: strings.Join(columns, "\x00")}]++
	}
	for _, columns := range expected.uniqueIndexes {
		wanted[ledgerUniqueKeyInvariant{origin: "c", columns: strings.Join(columns, "\x00")}]++
	}
	for _, optional := range additiveLedgerUniqueIndexes {
		if optional.table != expected.name {
			continue
		}
		key := ledgerUniqueKeyInvariant{origin: "c", columns: strings.Join(optional.columns, "\x00")}
		if actual[key] == 1 {
			delete(actual, key)
		}
	}
	if len(actual) != len(wanted) {
		return fmt.Errorf("store: file is not a clawctl ledger: table %s has incompatible PK/UNIQUE constraints", expected.name)
	}
	for key, count := range wanted {
		if actual[key] != count {
			return fmt.Errorf("store: file is not a clawctl ledger: table %s has incompatible PK/UNIQUE constraints", expected.name)
		}
	}
	return nil
}

func readLedgerIndexColumns(tx dbTx, index string) ([]ledgerIndexColumnInvariant, error) {
	rows, err := tx.Query(`SELECT name,"desc",coll FROM pragma_index_xinfo(?) WHERE "key"=1 ORDER BY seqno`, index)
	if err != nil {
		return nil, fmt.Errorf("store: identify ledger index columns for %s: %w", index, err)
	}
	defer rows.Close()
	var columns []ledgerIndexColumnInvariant
	for rows.Next() {
		var column ledgerIndexColumnInvariant
		var name, collation sql.NullString
		if err := rows.Scan(&name, &column.descending, &collation); err != nil {
			return nil, fmt.Errorf("store: scan ledger index columns for %s: %w", index, err)
		}
		if !name.Valid || !collation.Valid || collation.String != "BINARY" {
			return nil, fmt.Errorf("store: file is not a clawctl ledger: index %s has incompatible expression or collation", index)
		}
		column.name = name.String
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read ledger index columns for %s: %w", index, err)
	}
	return columns, nil
}
