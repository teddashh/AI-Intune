package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	legacyOccupancyFixtureColumns = `kind:TEXT:0:-:0 last_success_at:TEXT:0:-:0
		last_success_task_id:TEXT:0:-:0 last_error_category:TEXT:0:-:0 last_error_at:TEXT:0:-:0`
	currentOccupancyFixtureColumns = `model:TEXT:0:-:0 job_id:TEXT:0:-:0 agent_id:TEXT:0:-:0
		run_status:TEXT:0:-:0 total_tokens:INTEGER:0:-:0 last_error_text:TEXT:0:-:0`
)

func TestValidateExistingLedgerAcceptsHistoricalShapesReadOnlyAndStoreOpenRecoversCopy(t *testing.T) {
	if len(baselineLedgerTables) != 17 {
		t.Fatalf("baseline manifest has %d tables, want the 17 tables created by 1c08e24", len(baselineLedgerTables))
	}

	for _, fixture := range []struct {
		name        string
		options     ledgerFixtureOptions
		wantColumns []string
	}{
		{
			name:        "1c08e24 legacy occupancy",
			options:     ledgerFixtureOptions{occupancyColumns: legacyOccupancyFixtureColumns},
			wantColumns: []string{"kind", "last_success_at", "last_success_task_id", "last_error_category", "last_error_at"},
		},
		{
			name:        "744d52c fresh occupancy",
			options:     ledgerFixtureOptions{occupancyColumns: currentOccupancyFixtureColumns},
			wantColumns: []string{"model", "job_id", "agent_id", "run_status", "total_tokens", "last_error_text"},
		},
		{
			name: "interrupted addMissingColumns prefix",
			options: ledgerFixtureOptions{occupancyColumns: legacyOccupancyFixtureColumns +
				` model:TEXT:0:-:0 job_id:TEXT:0:-:0 agent_id:TEXT:0:-:0`},
			wantColumns: []string{"kind", "last_success_at", "last_success_task_id", "last_error_category", "last_error_at", "model", "job_id", "agent_id"},
		},
		{
			name: "legacy plus arbitrary new-column subset",
			options: ledgerFixtureOptions{occupancyColumns: legacyOccupancyFixtureColumns +
				` agent_id:TEXT:0:-:0 total_tokens:INTEGER:0:-:0`},
			wantColumns: []string{"kind", "last_success_at", "last_success_task_id", "last_error_category", "last_error_at", "agent_id", "total_tokens"},
		},
		{
			name: "completed legacy migration",
			options: ledgerFixtureOptions{occupancyColumns: legacyOccupancyFixtureColumns +
				" " + currentOccupancyFixtureColumns},
			wantColumns: []string{"kind", "last_success_at", "last_success_task_id", "last_error_category", "last_error_at", "model", "job_id", "agent_id", "run_status", "total_tokens", "last_error_text"},
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "historical.sqlite")
			createManifestLedger(t, path, fixture.options)
			assertFixtureColumns(t, path, fixture.wantColumns)
			assertSQLiteSchemaObject(t, path, "index", "ix_observed_time", false)
			assertLedgerAcceptedReadOnlyAndOpenable(t, path, false)
		})
	}

	t.Run("current", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "current.sqlite")
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			t.Fatalf("checkpoint current fixture: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		removeCheckpointedFixtureSidecars(t, path)
		assertLedgerAcceptedReadOnlyAndOpenable(t, path, true)
	})
}

func TestValidateExistingLedgerRejectsStructuralDecoysWithoutMutation(t *testing.T) {
	wrongType := func(column *ledgerColumnInvariant) { column.declaredType = "BLOB" }
	wrongNotNull := func(column *ledgerColumnInvariant) { column.notNull = 0 }
	wrongDefault := func(column *ledgerColumnInvariant) { column.defaultSQL = "9" }
	wrongPrimaryKey := func(column *ledgerColumnInvariant) { column.primaryKey = 0 }

	for _, fixture := range []struct {
		name    string
		options ledgerFixtureOptions
	}{
		{name: "missing one of seventeen tables", options: ledgerFixtureOptions{omitTable: "notifications"}},
		{name: "missing baseline column", options: ledgerFixtureOptions{omitColumnTable: "observed_state", omitColumn: "payload"}},
		{name: "wrong declared type", options: ledgerFixtureOptions{mutateColumnTable: "observed_state", mutateColumn: "payload", mutate: wrongType}},
		{name: "wrong not null", options: ledgerFixtureOptions{mutateColumnTable: "observed_state", mutateColumn: "received_at", mutate: wrongNotNull}},
		{name: "wrong default", options: ledgerFixtureOptions{mutateColumnTable: "machine_registry", mutateColumn: "expected", mutate: wrongDefault}},
		{name: "wrong primary key", options: ledgerFixtureOptions{mutateColumnTable: "machine_registry", mutateColumn: "machine_id", mutate: wrongPrimaryKey}},
		{name: "missing foreign key", options: ledgerFixtureOptions{omitForeignKeyTable: "observed_state", omitForeignKeyColumn: "machine_id"}},
		{name: "missing unique key", options: ledgerFixtureOptions{omitUniqueTable: "credential_profile"}},
		{name: "extra unique key", options: ledgerFixtureOptions{extraUniqueTable: "jobs"}},
		{name: "unknown extra column", options: ledgerFixtureOptions{extraColumnTable: "jobs"}},
		{name: "hidden generated column", options: ledgerFixtureOptions{hiddenColumnTable: "notifications"}},
		{name: "view instead of ordinary table", options: ledgerFixtureOptions{viewTable: "notifications"}},
		{name: "without rowid table", options: ledgerFixtureOptions{withoutRowIDTable: "notifications"}},
		{name: "strict table", options: ledgerFixtureOptions{strictTable: "notifications"}},
		{name: "known occupancy name with wrong metadata", options: ledgerFixtureOptions{
			occupancyColumns: `kind:BLOB:0:-:0`,
		}},
		{name: "stable occupancy columns only", options: ledgerFixtureOptions{occupancyColumnsSet: true}},
		{name: "partial legacy occupancy", options: ledgerFixtureOptions{occupancyColumns: `kind:TEXT:0:-:0 last_error_at:TEXT:0:-:0`}},
		{name: "new occupancy partial without legacy", options: ledgerFixtureOptions{occupancyColumns: `model:TEXT:0:-:0 agent_id:TEXT:0:-:0`}},
		{name: "all new plus partial legacy", options: ledgerFixtureOptions{occupancyColumns: currentOccupancyFixtureColumns + ` kind:TEXT:0:-:0`}},
		{name: "missing schema version", options: ledgerFixtureOptions{omitVersion: true}},
		{name: "wrong schema version", options: ledgerFixtureOptions{schemaVersion: "2"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "candidate.sqlite")
			createManifestLedger(t, path, fixture.options)
			assertLedgerRejectedWithoutMutation(t, path)
		})
	}
}

func TestValidateExistingLedgerValidatesOptionalJobDependencyTableWhenPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad-job-dependencies.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`ALTER TABLE job_dependencies RENAME COLUMN position TO bad_position`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExistingLedger(path); err == nil ||
		!strings.Contains(err.Error(), "job_dependencies") {
		t.Fatalf("malformed optional table error=%v", err)
	}
}

func TestValidateExistingLedgerDesiredStateRevisionIndexClosedSet(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		options ledgerFixtureOptions
		wantOK  bool
	}{
		{name: "missing index", wantOK: true},
		{name: "exact index", options: ledgerFixtureOptions{uniqueIndexDDL: `CREATE UNIQUE INDEX fixture_desired_revision ON desired_state(resource_kind,resource_id,revision)`}, wantOK: true},
		{name: "wrong column order", options: ledgerFixtureOptions{uniqueIndexDDL: `CREATE UNIQUE INDEX fixture_desired_revision ON desired_state(resource_id,resource_kind,revision)`}},
		{name: "partial", options: ledgerFixtureOptions{uniqueIndexDDL: `CREATE UNIQUE INDEX fixture_desired_revision ON desired_state(resource_kind,resource_id,revision) WHERE revision > 0`}},
		{name: "descending", options: ledgerFixtureOptions{uniqueIndexDDL: `CREATE UNIQUE INDEX fixture_desired_revision ON desired_state(resource_kind,resource_id,revision DESC)`}},
		{name: "wrong origin", options: ledgerFixtureOptions{desiredRevisionUniqueKey: true}},
		{name: "extra unknown unique", options: ledgerFixtureOptions{uniqueIndexDDL: `CREATE UNIQUE INDEX fixture_decoy ON desired_state(scope_id)`}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "candidate.sqlite")
			createManifestLedger(t, path, fixture.options)
			err := ValidateExistingLedger(path)
			if fixture.wantOK && err != nil {
				t.Fatalf("valid historical shape: %v", err)
			}
			if !fixture.wantOK && err == nil {
				t.Fatal("invalid UNIQUE shape was accepted")
			}
		})
	}
}

func TestValidateExistingLedgerRejectsNonSQLiteAndFourTableLookalikeWithoutMutation(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{name: "zero byte", prepare: func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "random bytes", prepare: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not a sqlite ledger\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unrelated sqlite", prepare: func(t *testing.T, path string) {
			createSQLiteFixture(t, path, `CREATE TABLE unrelated(value TEXT)`)
		}},
		{name: "four-table lookalike formerly accepted", prepare: func(t *testing.T, path string) {
			createSQLiteFixture(t, path, `
CREATE TABLE machine_registry(machine_id TEXT, display_name TEXT, created_at TEXT);
CREATE TABLE machine_checkins(machine_id TEXT, sent_at TEXT, received_at TEXT);
CREATE TABLE observed_state(observation_id TEXT, machine_id TEXT, measured_at TEXT, received_at TEXT, kind TEXT, subject TEXT, payload TEXT, source TEXT);
CREATE TABLE enrollment_tokens(token_hash TEXT, display_name TEXT, created_at TEXT, expires_at TEXT);`)
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "candidate.sqlite")
			fixture.prepare(t, path)
			assertLedgerRejectedWithoutMutation(t, path)
		})
	}
}

type ledgerFixtureOptions struct {
	occupancyColumnsSet      bool
	occupancyColumns         string
	omitTable                string
	omitColumnTable          string
	omitColumn               string
	mutateColumnTable        string
	mutateColumn             string
	mutate                   func(*ledgerColumnInvariant)
	omitForeignKeyTable      string
	omitForeignKeyColumn     string
	omitUniqueTable          string
	extraUniqueTable         string
	extraColumnTable         string
	hiddenColumnTable        string
	viewTable                string
	withoutRowIDTable        string
	strictTable              string
	omitVersion              bool
	schemaVersion            string
	uniqueIndexDDL           string
	desiredRevisionUniqueKey bool
}

func createManifestLedger(t *testing.T, path string, options ledgerFixtureOptions) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()

	for _, table := range baselineLedgerTables {
		if table.name == options.omitTable {
			continue
		}
		columns, err := parseLedgerColumns(table.requiredColumns)
		if err != nil {
			t.Fatalf("parse required fixture columns for %s: %v", table.name, err)
		}
		if table.name == "ticket_occupancy_observation" {
			variantSpec := legacyOccupancyFixtureColumns
			if options.occupancyColumnsSet || options.occupancyColumns != "" {
				variantSpec = options.occupancyColumns
			}
			variant, err := parseLedgerColumns(variantSpec)
			if err != nil {
				t.Fatalf("parse fixture occupancy columns: %v", err)
			}
			columns = append(columns, variant...)
		}
		if table.name == options.extraColumnTable {
			columns = append(columns, ledgerColumnInvariant{name: "decoy_extra", declaredType: "TEXT"})
		}

		filtered := columns[:0]
		for _, column := range columns {
			if table.name == options.omitColumnTable && column.name == options.omitColumn {
				continue
			}
			if table.name == options.mutateColumnTable && column.name == options.mutateColumn {
				options.mutate(&column)
			}
			filtered = append(filtered, column)
		}
		columns = filtered

		foreignKeys := table.foreignKeys
		if table.name == options.omitForeignKeyTable {
			filteredForeignKeys := make([]ledgerForeignKeyInvariant, 0, len(foreignKeys))
			for _, foreignKey := range foreignKeys {
				if foreignKey.column != options.omitForeignKeyColumn {
					filteredForeignKeys = append(filteredForeignKeys, foreignKey)
				}
			}
			foreignKeys = filteredForeignKeys
		}
		uniqueKeys := table.uniqueKeys
		if table.name == "desired_state" && options.desiredRevisionUniqueKey {
			uniqueKeys = append(append([][]string(nil), uniqueKeys...), []string{"resource_kind", "resource_id", "revision"})
		}
		if table.name == options.omitUniqueTable {
			uniqueKeys = nil
		}
		if table.name == options.extraUniqueTable {
			uniqueKeys = append(append([][]string(nil), uniqueKeys...), []string{"state"})
		}
		if table.name == options.viewTable {
			selects := make([]string, 0, len(columns))
			for _, column := range columns {
				selects = append(selects, "CAST(NULL AS "+column.declaredType+") AS "+quoteFixtureIdentifier(column.name))
			}
			ddl := fmt.Sprintf("CREATE VIEW %s AS SELECT %s", quoteFixtureIdentifier(table.name), strings.Join(selects, ","))
			if _, err := db.Exec(ddl); err != nil {
				t.Fatalf("create fixture view %s: %v", table.name, err)
			}
			continue
		}
		ddl := manifestTableDDL(table.name, columns, foreignKeys, uniqueKeys)
		if table.name == options.hiddenColumnTable {
			ddl = strings.TrimSuffix(ddl, ")") + `,"hidden_decoy" TEXT GENERATED ALWAYS AS ('x') VIRTUAL)`
		}
		if table.name == options.withoutRowIDTable {
			ddl += " WITHOUT ROWID"
		}
		if table.name == options.strictTable {
			ddl += " STRICT"
		}
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create fixture table %s: %v\n%s", table.name, err, ddl)
		}
	}
	if options.uniqueIndexDDL != "" {
		if _, err := db.Exec(options.uniqueIndexDDL); err != nil {
			t.Fatalf("create fixture unique index: %v", err)
		}
	}
	if !options.omitVersion && options.omitTable != "schema_meta" {
		version := options.schemaVersion
		if version == "" {
			version = "1"
		}
		if _, err := db.Exec(`INSERT INTO schema_meta(key,value) VALUES('version',?)`, version); err != nil {
			t.Fatalf("insert fixture schema version: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
}

func manifestTableDDL(name string, columns []ledgerColumnInvariant, foreignKeys []ledgerForeignKeyInvariant, uniqueKeys [][]string) string {
	primaryKey := append([]ledgerColumnInvariant(nil), columns...)
	sort.Slice(primaryKey, func(i, j int) bool { return primaryKey[i].primaryKey < primaryKey[j].primaryKey })
	var primaryColumns []string
	for _, column := range primaryKey {
		if column.primaryKey > 0 {
			primaryColumns = append(primaryColumns, quoteFixtureIdentifier(column.name))
		}
	}

	definitions := make([]string, 0, len(columns)+len(foreignKeys)+len(uniqueKeys)+1)
	for _, column := range columns {
		definition := quoteFixtureIdentifier(column.name) + " " + column.declaredType
		if column.notNull != 0 {
			definition += " NOT NULL"
		}
		if column.hasDefault {
			definition += " DEFAULT " + column.defaultSQL
		}
		if len(primaryColumns) == 1 && column.primaryKey == 1 {
			definition += " PRIMARY KEY"
		}
		definitions = append(definitions, definition)
	}
	if len(primaryColumns) > 1 {
		definitions = append(definitions, "PRIMARY KEY ("+strings.Join(primaryColumns, ",")+")")
	}
	for _, foreignKey := range foreignKeys {
		definitions = append(definitions, fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s(%s)",
			quoteFixtureIdentifier(foreignKey.column), quoteFixtureIdentifier(foreignKey.referencedTable),
			quoteFixtureIdentifier(foreignKey.referencedColumn)))
	}
	for _, uniqueKey := range uniqueKeys {
		quoted := make([]string, 0, len(uniqueKey))
		for _, column := range uniqueKey {
			quoted = append(quoted, quoteFixtureIdentifier(column))
		}
		definitions = append(definitions, "UNIQUE ("+strings.Join(quoted, ",")+")")
	}
	return fmt.Sprintf("CREATE TABLE %s (%s)", quoteFixtureIdentifier(name), strings.Join(definitions, ","))
}

func quoteFixtureIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func assertFixtureColumns(t *testing.T, path string, expectedOptional []string) {
	t.Helper()
	db, err := sql.Open("sqlite", rollbackReadOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('ticket_occupancy_observation')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := make(map[string]bool)
	for _, name := range expectedOptional {
		want[name] = true
	}
	legacy, err := parseLedgerColumns(baselineLedgerTables[7].replacementLegacyColumns)
	if err != nil {
		t.Fatal(err)
	}
	current, err := parseLedgerColumns(baselineLedgerTables[7].replacementNewColumns)
	if err != nil {
		t.Fatal(err)
	}
	known := append(legacy, current...)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		for _, optional := range known {
			if name == optional.name {
				if !want[name] {
					t.Fatalf("fixture unexpectedly contains optional occupancy column %s", name)
				}
				delete(want, name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(want) != 0 {
		t.Fatalf("fixture is missing expected optional occupancy columns: %v", want)
	}
}

func assertLedgerAcceptedReadOnlyAndOpenable(t *testing.T, path string, allowWALSidecarChanges bool) {
	t.Helper()
	before := snapshotLedgerFiles(t, path)
	if err := ValidateExistingLedger(path); err != nil {
		t.Fatalf("legitimate ledger rejected: %v", err)
	}
	afterValidation := snapshotLedgerFiles(t, path)
	if !bytes.Equal(before[""].data, afterValidation[""].data) {
		t.Fatal("ledger identity check modified accepted main database")
	}
	if !allowWALSidecarChanges {
		assertLedgerSnapshotEqual(t, before, afterValidation, "ledger identity check modified accepted files")
	}

	copyPath := filepath.Join(t.TempDir(), "open-copy.sqlite")
	if err := os.WriteFile(copyPath, before[""].data, 0o600); err != nil {
		t.Fatal(err)
	}
	if wal := before["-wal"]; wal.exists {
		if err := os.WriteFile(copyPath+"-wal", wal.data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := Open(copyPath)
	if err != nil {
		t.Fatalf("Store.Open rejected a copy of an accepted fixture: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	assertCurrentOccupancyColumnsPresent(t, copyPath)
	assertSQLiteSchemaObject(t, copyPath, "index", "ix_observed_time", true)
	assertSQLiteSchemaObject(t, copyPath, "table", "catalog_manifests", true)
	assertSQLiteSchemaObject(t, copyPath, "index", "ix_catalog_manifests_published", true)
	assertSQLiteSchemaObject(t, copyPath, "table", "machine_profiles", true)
	assertSQLiteSchemaObject(t, copyPath, "index", "ix_machine_profiles_published", true)
	if err := ValidateExistingLedger(copyPath); err != nil {
		t.Fatalf("migrated copy no longer identifies as a ledger: %v", err)
	}

	afterCopyOpen := snapshotLedgerFiles(t, path)
	if !bytes.Equal(before[""].data, afterCopyOpen[""].data) {
		t.Fatal("opening the copy changed the original main database")
	}
	if !allowWALSidecarChanges {
		assertLedgerSnapshotEqual(t, before, afterCopyOpen, "opening the copy changed the original fixture")
	}
}

func assertCurrentOccupancyColumnsPresent(t *testing.T, path string) {
	t.Helper()
	expected, err := parseLedgerColumns(currentOccupancyFixtureColumns)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", rollbackReadOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, column := range expected {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_xinfo('ticket_occupancy_observation') WHERE name=?`, column.name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("Store.Open did not restore occupancy column %s", column.name)
		}
	}
}

func assertSQLiteSchemaObject(t *testing.T, path, objectType, name string, expected bool) {
	t.Helper()
	db, err := sql.Open("sqlite", rollbackReadOnlyDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var present bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type=? AND name=?)`, objectType, name).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != expected {
		t.Fatalf("schema object %s %s presence=%t, want %t", objectType, name, present, expected)
	}
}

func assertLedgerRejectedWithoutMutation(t *testing.T, path string) {
	t.Helper()
	before := snapshotLedgerFiles(t, path)
	if err := ValidateExistingLedger(path); err == nil {
		t.Fatal("non-clawctl database accepted")
	}
	assertLedgerSnapshotEqual(t, before, snapshotLedgerFiles(t, path), "ledger identity check modified rejected files")
}

type ledgerFileSnapshot struct {
	exists bool
	data   []byte
}

func snapshotLedgerFiles(t *testing.T, path string) map[string]ledgerFileSnapshot {
	t.Helper()
	snapshot := make(map[string]ledgerFileSnapshot)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		data, err := os.ReadFile(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			snapshot[suffix] = ledgerFileSnapshot{}
			continue
		}
		if err != nil {
			t.Fatalf("read ledger sidecar %s: %v", suffix, err)
		}
		snapshot[suffix] = ledgerFileSnapshot{exists: true, data: data}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot[""] = ledgerFileSnapshot{exists: true, data: data}
	return snapshot
}

func assertLedgerSnapshotEqual(t *testing.T, before, after map[string]ledgerFileSnapshot, message string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		left, right := before[suffix], after[suffix]
		if left.exists != right.exists || !bytes.Equal(left.data, right.data) {
			t.Fatalf("%s (%s)", message, suffix)
		}
	}
}

func removeCheckpointedFixtureSidecars(t *testing.T, path string) {
	t.Helper()
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("checkpoint left a nonempty WAL: %d bytes", info.Size())
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func createSQLiteFixture(t *testing.T, path, ddl string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := db.Exec(ddl)
	closeErr := db.Close()
	if execErr != nil || closeErr != nil {
		t.Fatalf("create SQLite fixture: exec=%v close=%v", execErr, closeErr)
	}
}

// additiveLedgerColumns 的十筆裡有九筆是 verification_results 的 provenance
// 欄位，而那張表在嚴格清單（:218）的 columnVariants 已經把同一組欄位連預設值
// 一起列過，所以那九筆**沒有獨有見證**——動它們會被 validateLedgerTable
// 先攔下，量到的是鄰居。
//
// ⚠ 只有 deployments.control_revision 是獨有見證：deployments **不在**嚴格表
// 清單裡，validateAdditiveLedgerColumn 是它唯一的檢查。
//
// ⚠ 第一個 case 是誠實前置，不是湊數：DROP 再 ADD 會把欄位搬到最後一格。
// 先證明「同樣的預設值原樣加回去仍然被接受」，第二個 case 的拒絕才能歸因於
// 預設值變了，而不是歸因於重建這個動作。
func TestValidateExistingLedgerRejectsAnAdditiveColumnWhoseDefaultChanged(t *testing.T) {
	cases := []struct {
		name      string
		addColumn string
		wantOK    bool
	}{
		{"rebuilt with the same default", "INTEGER NOT NULL DEFAULT 0", true},
		{"rebuilt with a different default", "INTEGER NOT NULL DEFAULT 1", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "additive-column.sqlite")
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`ALTER TABLE deployments DROP COLUMN control_revision`); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`ALTER TABLE deployments ADD COLUMN control_revision ` + tc.addColumn); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			err = ValidateExistingLedger(path)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("把同一個預設值原樣加回去仍然被拒，代表拒絕的原因是重建這個動作本身，不是預設值變了；下面那個 case 就無法歸因: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "control_revision") {
				t.Errorf("got err %v, want a rejection naming control_revision; Hub 會把一份 deployments.control_revision 預設值不對的帳本當成自己的帳本收下；那一欄是控制平面下一步轉換的 CAS token，預設值不同代表舊列復原出來的授權點跟 Hub 以為的不是同一個", err)
			}
		})
	}
}
