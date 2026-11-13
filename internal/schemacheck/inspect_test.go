package schemacheck

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExplicitContractSeparatesProductionRequirementsFromFixtureEvidence(t *testing.T) {
	c, err := loadContract()
	if err != nil {
		t.Fatal(err)
	}
	columns, targets, written := 0, 0, 0
	for _, table := range c.Tables {
		columns += len(table.Columns)
		targets += len(table.Unique)
		for _, column := range table.Columns {
			if len(column.WriteSources) != 0 {
				written++
				for _, source := range column.WriteSources {
					if !strings.HasPrefix(source, "internal/") || strings.Contains(source, "_test.go") {
						t.Fatal("column writability did not come from production SQL")
					}
				}
			}
			if table.Name != "membership_processor_lease" &&
				(column.Affinity != "" || column.RequireNotNull || column.RequireNullable || column.DefaultLiteral != nil) {
				t.Fatal("fixture or inferred constraints were asserted as a production DDL contract")
			}
		}
	}
	if len(c.Tables) != 35 || columns != 383 || targets != 6 || written != 314 {
		t.Fatalf("unreviewed contract inventory change: tables=%d columns=%d targets=%d written=%d", len(c.Tables), columns, targets, written)
	}
	for _, expected := range []struct{ table, column string }{
		{"membership_intake_settings", "created_at"},
		{"membership_intake_settings", "created_by"},
		{"redeem_orders", "extension_delivery_status"},
		{"managed_card_transactions", "decline_reason_code"},
	} {
		for _, table := range c.Tables {
			if table.Name == expected.table {
				for _, column := range table.Columns {
					if column.Name == expected.column && column.RequiredWhen != "observed_only" {
						t.Fatal("a test-only column became a mandatory runtime column")
					}
				}
			}
		}
	}
}

func TestEmptyFileReportsMissingAndIsNotInitialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sqlite")
	if err := os.WriteFile(path, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	before := snapshotDirectory(t, filepath.Dir(path))
	report, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "missing" || len(report.Missing) != 34 || report.Coverage.DeclaredTables != 35 ||
		report.Coverage.DeclaredColumns != 383 || report.Coverage.PresentTables != 0 || report.KnownRequirementsSatisfied {
		t.Fatalf("empty snapshot was overclaimed: %+v", report)
	}
	assertBoundary(t, report)
	assertUnchanged(t, before, snapshotDirectory(t, filepath.Dir(path)))
}

func TestIncompleteLegacyFixtureReportsColumnsDefaultsAndUniqueKeys(t *testing.T) {
	path := fixture(t, []string{
		`CREATE TABLE managed_cards (id TEXT PRIMARY KEY, upstream_card_id TEXT UNIQUE, private_metadata TEXT DEFAULT 'PRIVATE_DEFAULT')`,
		`INSERT INTO managed_cards VALUES ('PRIVATE_ROW_ID','PRIVATE_UPSTREAM_ID','PRIVATE_TOKEN')`,
		`CREATE TABLE card_price_signals (card_id TEXT,tier TEXT,amount REAL)`,
		`CREATE TABLE automatic_checkout_daily_usage (scope_id TEXT,business_date TEXT,order_units INTEGER,risk_reserved_usd REAL,updated_at TEXT)`,
		`CREATE TABLE membership_processor_lease (id TEXT PRIMARY KEY,epoch TEXT DEFAULT 1,status TEXT DEFAULT 'wrong',updated_at TEXT)`,
	})
	before := snapshotDirectory(t, filepath.Dir(path))
	report, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []Finding{
		{Code: "TABLE_MISSING", Table: "membership_payment_stages"},
		{Code: "COLUMN_MISSING", Table: "managed_cards", Column: "capacity_state"},
		{Code: "UPSERT_UNIQUE_KEY_MISSING", Table: "card_price_signals", Columns: []string{"card_id", "tier"}},
		{Code: "UPSERT_UNIQUE_KEY_MISSING", Table: "automatic_checkout_daily_usage", Columns: []string{"scope_id", "business_date"}},
	} {
		if !containsFinding(report.Missing, expected) {
			t.Fatalf("known dependency was not reported: %+v", expected)
		}
	}
	for _, code := range []string{"COLUMN_AFFINITY_INCOMPATIBLE", "COLUMN_NOT_NULL_MISSING", "COLUMN_DEFAULT_INCOMPATIBLE"} {
		if !containsFinding(report.Incompatible, Finding{Code: code, Table: "membership_processor_lease", Column: "epoch"}) {
			t.Fatalf("production lease requirement not inspected: %s", code)
		}
	}
	assertBoundary(t, report)
	assertUnchanged(t, before, snapshotDirectory(t, filepath.Dir(path)))
}

func TestAllTableNamesWithoutColumnsCannotPass(t *testing.T) {
	c, err := loadContract()
	if err != nil {
		t.Fatal(err)
	}
	var statements []string
	for _, table := range c.Tables {
		statements = append(statements, "CREATE TABLE "+table.Name+" (fixture_only TEXT)")
	}
	report, err := Inspect(context.Background(), fixture(t, statements))
	if err != nil {
		t.Fatal(err)
	}
	if report.Coverage.PresentTables != 35 || report.Coverage.PresentColumns != 0 || report.KnownRequirementsSatisfied || len(report.Missing) < 350 {
		t.Fatal("table-name inventory was mistaken for a compatible schema")
	}
	assertBoundary(t, report)
}

func TestKnownRequirementsCanMatchWithoutClaimingProductionReadiness(t *testing.T) {
	for _, platforms := range []bool{false, true} {
		name := "legacy"
		if platforms {
			name = "platform_catalog"
		}
		t.Run(name, func(t *testing.T) {
			path := knownRequirementsFixture(t, platforms, nil)
			before := snapshotDirectory(t, filepath.Dir(path))
			report, err := Inspect(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if !report.KnownRequirementsSatisfied || report.Status != "unknown" || len(report.Unknown) == 0 ||
				report.PlatformCatalogPresent != platforms || len(report.Missing) != 0 || len(report.Incompatible) != 0 {
				t.Fatalf("known requirements fixture: %+v", report)
			}
			if !containsFinding(report.Unknown, Finding{Code: "UPSTREAM_TABLE_CONSTRAINTS_UNVERIFIED", Table: "live_canary_authorizations"}) {
				t.Fatal("unknown canary issuance constraints were hidden")
			}
			assertBoundary(t, report)
			assertUnchanged(t, before, snapshotDirectory(t, filepath.Dir(path)))
		})
	}
}

func TestMultiProviderRejectsOldGlobalCardIDUniqueness(t *testing.T) {
	path := knownRequirementsFixture(t, true, []string{`CREATE UNIQUE INDEX old_global_card_id ON managed_cards(upstream_card_id)`})
	report, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsFinding(report.Incompatible, Finding{Code: "LEGACY_GLOBAL_CARD_ID_UNIQUENESS", Table: "managed_cards", Columns: []string{"upstream_card_id"}}) || report.KnownRequirementsSatisfied {
		t.Fatal("multi-provider catalog accepted a globally unique upstream ID")
	}
}

func TestGeneratedColumnsRespectIndependentProductionWriteEvidence(t *testing.T) {
	for _, item := range []struct {
		name, statement string
		compatible      bool
		table, column   string
	}{
		{"explicit_insert_column", `ALTER TABLE card_price_signals DROP COLUMN amount;
          ALTER TABLE card_price_signals ADD COLUMN amount REAL GENERATED ALWAYS AS(0) VIRTUAL`, false, "card_price_signals", "amount"},
		{"explicit_dynamic_update_column", `ALTER TABLE membership_checkout_commands DROP COLUMN sanitized_diagnostic;
          ALTER TABLE membership_checkout_commands ADD COLUMN sanitized_diagnostic TEXT GENERATED ALWAYS AS('') VIRTUAL`, false, "membership_checkout_commands", "sanitized_diagnostic"},
		{"present_optional_write_column", `ALTER TABLE funding_intents ADD COLUMN provider_key TEXT GENERATED ALWAYS AS('synthetic') VIRTUAL`, false, "funding_intents", "provider_key"},
		{"read_only_product_column", `ALTER TABLE products DROP COLUMN membership_tier;
          ALTER TABLE products ADD COLUMN membership_tier TEXT GENERATED ALWAYS AS('plus') VIRTUAL`, true, "products", "membership_tier"},
	} {
		t.Run(item.name, func(t *testing.T) {
			path := knownRequirementsFixture(t, false, []string{item.statement})
			report, err := Inspect(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if report.KnownRequirementsSatisfied != item.compatible {
				t.Fatal("generated-column compatibility ignored explicit SQL writes or rejected a read-only column")
			}
			if containsFinding(report.Incompatible, Finding{Code: "COLUMN_NOT_WRITABLE", Table: item.table, Column: item.column}) == item.compatible {
				t.Fatal("generated-column finding did not follow independent write evidence")
			}
		})
	}
}

func TestNonLiteralDefaultRemainsUnverifiedInsteadOfClaimingAFullKnownMatch(t *testing.T) {
	path := knownRequirementsFixture(t, false, []string{
		`DROP TABLE membership_processor_lease`,
		`CREATE TABLE membership_processor_lease (
          id TEXT PRIMARY KEY,owner TEXT,holder_token TEXT,epoch INTEGER NOT NULL DEFAULT 0,
          status TEXT NOT NULL DEFAULT ('sto' || 'pped'),version TEXT,started_at TEXT,
          heartbeat_at TEXT,expires_at TEXT,last_tick_at TEXT,last_success_at TEXT,
          last_error_code TEXT,updated_at TEXT NOT NULL)`,
	})
	report, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "unknown" || report.KnownRequirementsSatisfied || len(report.Missing) != 0 || len(report.Incompatible) != 0 ||
		!containsFinding(report.Unknown, Finding{Code: "COLUMN_DEFAULT_SEMANTICS_UNVERIFIED", Table: "membership_processor_lease", Column: "status"}) {
		t.Fatal("unexecuted default expression was guessed or treated as a verified literal")
	}
}

func TestLeaseDefaultChecksRespectIdentifierCaseAndSQLiteNumericSyntax(t *testing.T) {
	for _, item := range []struct {
		name, epochDefault string
		compatible         bool
	}{
		{"uppercase_identifiers", "0", true},
		{"quoted_decimal_zero", "'0e0'", true},
		{"quoted_go_hex_float", "'0x0p0'", false},
		{"quoted_go_underscores", "'0_0'", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			path := knownRequirementsFixture(t, false, []string{
				`DROP TABLE membership_processor_lease`,
				`CREATE TABLE membership_processor_lease (
                  id TEXT PRIMARY KEY,owner TEXT,holder_token TEXT,EPOCH INTEGER NOT NULL DEFAULT ` + item.epochDefault + `,
                  STATUS TEXT NOT NULL DEFAULT 'stopped',version TEXT,started_at TEXT,
                  heartbeat_at TEXT,expires_at TEXT,last_tick_at TEXT,last_success_at TEXT,
                  last_error_code TEXT,updated_at TEXT NOT NULL)`,
			})
			report, err := Inspect(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if report.KnownRequirementsSatisfied != item.compatible {
				t.Fatal("lease default comparison confused identifier case or Go and SQLite numeric grammar")
			}
			if !item.compatible && !containsFinding(report.Incompatible, Finding{Code: "COLUMN_DEFAULT_INCOMPATIBLE", Table: "membership_processor_lease", Column: "epoch"}) {
				t.Fatal("quoted non-decimal string was not reported as incompatible with numeric zero")
			}
		})
	}
}

func TestPlatformCatalogRequiresItsInventoryColumns(t *testing.T) {
	path := fixture(t, []string{
		`CREATE TABLE membership_card_platforms (key TEXT)`,
		`CREATE TABLE managed_cards (id TEXT,upstream_card_id TEXT UNIQUE)`,
		`CREATE TABLE card_inventory_runs (id TEXT)`,
	})
	report, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"managed_cards", "card_inventory_runs"} {
		if !containsFinding(report.Missing, Finding{Code: "COLUMN_MISSING", Table: table, Column: "provider_key"}) {
			t.Fatal("platform catalog did not activate its inventory schema requirement")
		}
	}
}

func TestUpsertKeysRejectPartialExpressionNonUniqueAndSupersetIndexes(t *testing.T) {
	for _, item := range []struct {
		name, index string
		accepted    bool
	}{
		{"none", "", false},
		{"non_unique", `CREATE INDEX candidate ON card_price_signals(card_id,tier)`, false},
		{"partial", `CREATE UNIQUE INDEX candidate ON card_price_signals(card_id,tier) WHERE found=1`, false},
		{"expression", `CREATE UNIQUE INDEX candidate ON card_price_signals(card_id,lower(tier))`, false},
		{"superset", `CREATE UNIQUE INDEX candidate ON card_price_signals(card_id,tier,found)`, false},
		{"reversed_complete", `CREATE UNIQUE INDEX candidate ON card_price_signals(tier,card_id)`, true},
	} {
		t.Run(item.name, func(t *testing.T) {
			statements := []string{`CREATE TABLE card_price_signals(card_id TEXT,tier TEXT,found INTEGER)`}
			if item.index != "" {
				statements = append(statements, item.index)
			}
			report, err := Inspect(context.Background(), fixture(t, statements))
			if err != nil {
				t.Fatal(err)
			}
			missing := containsFinding(report.Missing, Finding{Code: "UPSERT_UNIQUE_KEY_MISSING", Table: "card_price_signals", Columns: []string{"card_id", "tier"}})
			if missing == item.accepted {
				t.Fatal("index shape was incorrectly accepted/rejected for the complete conflict target")
			}
		})
	}
}

func TestViewsAreNotAcceptedAsWorkerTables(t *testing.T) {
	path := fixture(t, []string{`CREATE VIEW managed_cards AS SELECT 'PRIVATE_VIEW_TOKEN' AS id`})
	report, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsFinding(report.Incompatible, Finding{Code: "TABLE_IS_NOT_ORDINARY_TABLE", Table: "managed_cards"}) {
		t.Fatal("a view was accepted as a mutable worker table")
	}
	assertBoundary(t, report)
}

func TestSidecarsAreRefusedWithoutCheckpointOrCleanup(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			path := fixture(t, []string{`CREATE TABLE example(id TEXT)`})
			if err := os.WriteFile(path+suffix, []byte("PRIVATE_SIDECAR_BYTES"), 0o400); err != nil {
				t.Fatal(err)
			}
			before := snapshotDirectory(t, filepath.Dir(path))
			_, err := Inspect(context.Background(), path)
			if SafeCode(err) != "SCHEMA_SIDECAR_PRESENT" {
				t.Fatal("sidecar was ignored instead of safely refusing an incomplete snapshot")
			}
			assertUnchanged(t, before, snapshotDirectory(t, filepath.Dir(path)))
		})
	}
}

func TestReadOnlyConnectionRejectsWritesAtBothGuards(t *testing.T) {
	path := fixture(t, []string{`CREATE TABLE example(id TEXT)`})
	before := snapshotDirectory(t, filepath.Dir(path))
	db, err := openReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE must_not_exist(id TEXT)`); err == nil {
		t.Fatal("query_only connection accepted DDL")
	}
	// Turning off the connection-level guard in this test must still leave
	// SQLite's underlying mode=ro file access unable to write.
	if _, err := db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal("could not exercise the independent read-only URI guard")
	}
	if _, err := db.Exec(`INSERT INTO example(id) VALUES ('must-not-persist')`); err == nil {
		t.Fatal("mode=ro accepted a write after the query_only guard was disabled")
	}
	db.Close()
	assertUnchanged(t, before, snapshotDirectory(t, filepath.Dir(path)))
}

func TestInvalidMissingAndCancelledInputIsSafeAndNeverCreatesAFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "PRIVATE_MISSING_DATABASE")
	_, err := Inspect(context.Background(), missing)
	if SafeCode(err) != "SCHEMA_DATABASE_NOT_FOUND" {
		t.Fatal("missing database was not refused")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("inspector created a missing database")
	}
	for _, path := range []string{"file:PRIVATE.sqlite?mode=rwc", ":memory:", dir} {
		if _, err := Inspect(context.Background(), path); SafeCode(err) != "SCHEMA_INPUT_INVALID" {
			t.Fatal("unsupported input kind was accepted")
		}
	}
	invalid := filepath.Join(dir, "PRIVATE_DATABASE")
	if err := os.WriteFile(invalid, []byte("PRIVATE_SESSION_TOKEN_NOT_SQLITE"), 0o400); err != nil {
		t.Fatal(err)
	}
	_, err = Inspect(context.Background(), invalid)
	if err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("invalid file was accepted or its content/path leaked")
	}
	valid := fixture(t, []string{`CREATE TABLE example(id TEXT)`})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(ctx, valid); SafeCode(err) != "SCHEMA_CHECK_CANCELLED" {
		t.Fatal("cancelled inspection did not return a safe code")
	}
	if SafeCode(errors.New("PRIVATE_RAW_ERROR")) != "SCHEMA_INSPECTION_FAILED" {
		t.Fatal("unknown errors escaped the fixed code boundary")
	}
}

func fixture(t *testing.T, statements []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "PRIVATE_FIXTURE.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal("synthetic fixture setup failed", err)
		}
	}
	return path
}

// This intentionally incomplete schema is only an offline test fixture for
// the known consumer requirements. It omits upstream/default/row semantics
// and must never become a production migration or startup initializer.
func knownRequirementsFixture(t *testing.T, platforms bool, extra []string) string {
	t.Helper()
	c, err := loadContract()
	if err != nil {
		t.Fatal(err)
	}
	var statements []string
	for _, table := range c.Tables {
		if table.Name == "membership_card_platforms" && !platforms {
			continue
		}
		var fields []string
		for _, column := range table.Columns {
			if !required(column.RequiredWhen, platforms) {
				continue
			}
			field := column.Name + " " + column.TypeHint
			if column.RequireNotNull {
				field += " NOT NULL"
			}
			if column.DefaultLiteral != nil {
				field += " DEFAULT " + *column.DefaultLiteral
			}
			fields = append(fields, field)
		}
		if len(table.PrimaryKey) != 0 {
			fields = append(fields, "PRIMARY KEY ("+strings.Join(table.PrimaryKey, ",")+")")
		}
		for _, unique := range table.Unique {
			if required(unique.RequiredWhen, platforms) {
				fields = append(fields, "UNIQUE ("+strings.Join(unique.Columns, ",")+")")
			}
		}
		statements = append(statements, "CREATE TABLE "+table.Name+" ("+strings.Join(fields, ",")+")")
	}
	return fixture(t, append(statements, extra...))
}

func containsFinding(findings []Finding, wanted Finding) bool {
	for _, finding := range findings {
		if reflect.DeepEqual(finding, wanted) {
			return true
		}
	}
	return false
}

func assertBoundary(t *testing.T, report Report) {
	t.Helper()
	if !report.ReadOnly || report.RowValuesRead || report.ProductionReady || report.PaymentAllowed {
		t.Fatal("schema inspection exceeded its evidence or execution boundary")
	}
	encoded, err := json.Marshal(report)
	if err != nil || bytes.Contains(encoded, []byte("PRIVATE")) {
		t.Fatal("schema output included a private row, default, identifier or path")
	}
}

type fileSnapshot struct {
	Contents []byte
	Mode     os.FileMode
	Size     int64
	Modified int64
}

func snapshotDirectory(t *testing.T, dir string) map[string]fileSnapshot {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]fileSnapshot{}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = fileSnapshot{contents, info.Mode(), info.Size(), info.ModTime().UnixNano()}
	}
	return result
}

func assertUnchanged(t *testing.T, before, after map[string]fileSnapshot) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("inspection changed file bytes, metadata or directory entries")
	}
}
