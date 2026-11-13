package schemacheck

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

type Finding struct {
	Code    string   `json:"code"`
	Table   string   `json:"table,omitempty"`
	Column  string   `json:"column,omitempty"`
	Columns []string `json:"columns,omitempty"`
}

type Coverage struct {
	DeclaredTables  int `json:"declaredTables"`
	DeclaredColumns int `json:"declaredColumns"`
	RequiredTables  int `json:"requiredTables"`
	RequiredColumns int `json:"requiredColumns"`
	PresentTables   int `json:"presentTables"`
	PresentColumns  int `json:"presentColumns"`
}

// Findings contain only names/codes from the embedded contract, never input
// paths, arbitrary schema identifiers, DDL/default text, or SQLite errors.
type Report struct {
	Code                       string    `json:"code"`
	Status                     string    `json:"status"`
	ContractID                 string    `json:"contractId"`
	ContractVersion            int       `json:"contractVersion"`
	ReviewedSourceCommit       string    `json:"reviewedSourceCommit"`
	ReadOnly                   bool      `json:"readOnly"`
	RowValuesRead              bool      `json:"rowValuesRead"`
	PlatformCatalogPresent     bool      `json:"platformCatalogPresent"`
	KnownRequirementsSatisfied bool      `json:"knownRequirementsSatisfied"`
	ProductionReady            bool      `json:"productionReady"`
	PaymentAllowed             bool      `json:"paymentAllowed"`
	Coverage                   Coverage  `json:"coverage"`
	Missing                    []Finding `json:"missing"`
	Incompatible               []Finding `json:"incompatible"`
	Unknown                    []Finding `json:"unknown"`
}

type safeError string

func (e safeError) Error() string { return string(e) }
func fail(code string) error      { return safeError(code) }

func SafeCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "SCHEMA_CHECK_CANCELLED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "SCHEMA_CHECK_TIMEOUT"
	}
	var safe safeError
	if errors.As(err, &safe) {
		return string(safe)
	}
	return "SCHEMA_INSPECTION_FAILED"
}

func Failure(code string) Report {
	allowed := map[string]bool{
		"SCHEMA_ARGUMENTS_INVALID": true, "SCHEMA_INPUT_INVALID": true,
		"SCHEMA_DATABASE_NOT_FOUND": true, "SCHEMA_DATABASE_UNREADABLE": true,
		"SCHEMA_SIDECAR_PRESENT": true, "SCHEMA_SOURCE_CHANGED": true,
		"SCHEMA_CONTRACT_INVALID": true, "SCHEMA_READ_ONLY_UNVERIFIED": true,
		"SCHEMA_CHECK_CANCELLED": true, "SCHEMA_CHECK_TIMEOUT": true,
		"SCHEMA_INSPECTION_FAILED": true,
	}
	if !allowed[code] {
		code = "SCHEMA_INSPECTION_FAILED"
	}
	return Report{Code: code, Status: "unknown", ReadOnly: true,
		Missing: []Finding{}, Incompatible: []Finding{}, Unknown: []Finding{}}
}

// Inspect accepts only an existing, quiescent SQLite file. Immutable mode
// prevents even WAL shared-memory writes; sidecars are rejected so an ignored
// WAL cannot silently make an older catalog look current. This does not prove
// source quiescence; callers must supply an offline snapshot.
func Inspect(ctx context.Context, path string) (Report, error) {
	c, err := loadContract()
	if err != nil {
		return Report{}, err
	}
	resolved, before, err := resolveSnapshot(path)
	if err != nil {
		return Report{}, err
	}
	db, err := openReadOnly(ctx, resolved)
	if err != nil {
		return Report{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Report{}, inspectionError(ctx)
	}
	defer tx.Rollback()
	report := Report{
		Code: "SCHEMA_KNOWN_REQUIREMENTS_UNSATISFIED", Status: "missing",
		ContractID: c.ID, ContractVersion: c.Version, ReviewedSourceCommit: c.ReviewedSourceCommit,
		ReadOnly: true, Missing: []Finding{}, Incompatible: []Finding{}, Unknown: []Finding{},
		Coverage: Coverage{DeclaredTables: len(c.Tables)},
	}
	platformKind, err := objectKind(ctx, tx, "membership_card_platforms")
	if err != nil {
		return Report{}, inspectionError(ctx)
	}
	report.PlatformCatalogPresent = platformKind != ""
	for _, table := range c.Tables {
		report.Coverage.DeclaredColumns += len(table.Columns)
		kind, err := objectKind(ctx, tx, table.Name)
		if err != nil {
			return Report{}, inspectionError(ctx)
		}
		if table.RequiredWhen == "if_present" && kind == "" {
			continue
		}
		report.Coverage.RequiredTables++
		for _, column := range table.Columns {
			if required(column.RequiredWhen, report.PlatformCatalogPresent) {
				report.Coverage.RequiredColumns++
			}
		}
		if kind == "" {
			report.Missing = append(report.Missing, Finding{Code: "TABLE_MISSING", Table: table.Name})
			continue
		}
		if kind != "table" {
			report.Incompatible = append(report.Incompatible, Finding{Code: "TABLE_IS_NOT_ORDINARY_TABLE", Table: table.Name})
			continue
		}
		report.Coverage.PresentTables++
		if err := inspectTable(ctx, tx, table, &report); err != nil {
			return Report{}, inspectionError(ctx)
		}
	}
	for _, code := range c.Unknown {
		report.Unknown = append(report.Unknown, Finding{Code: code})
	}
	if err := unchangedSnapshot(resolved, before); err != nil {
		return Report{}, err
	}
	knownUnverified := false
	for _, finding := range report.Unknown {
		if finding.Code == "COLUMN_DEFAULT_SEMANTICS_UNVERIFIED" {
			knownUnverified = true
		}
	}
	report.KnownRequirementsSatisfied = len(report.Missing) == 0 && len(report.Incompatible) == 0 && !knownUnverified
	if len(report.Missing) == 0 && len(report.Incompatible) == 0 {
		report.Code, report.Status = "SCHEMA_KNOWN_REQUIREMENTS_SATISFIED_WITH_UNKNOWNS", "unknown"
		if knownUnverified {
			report.Code = "SCHEMA_KNOWN_REQUIREMENTS_UNVERIFIED"
		}
	} else if len(report.Incompatible) != 0 {
		report.Status = "incompatible"
	}
	return report, nil
}

func resolveSnapshot(path string) (string, os.FileInfo, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, 0) || strings.HasPrefix(path, "file:") || path == ":memory:" {
		return "", nil, fail("SCHEMA_INPUT_INVALID")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if os.IsNotExist(err) {
		return "", nil, fail("SCHEMA_DATABASE_NOT_FOUND")
	}
	if err != nil {
		return "", nil, fail("SCHEMA_DATABASE_UNREADABLE")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", nil, fail("SCHEMA_INPUT_INVALID")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, fail("SCHEMA_INPUT_INVALID")
	}
	if err := rejectSidecars(resolved); err != nil {
		return "", nil, err
	}
	return resolved, info, nil
}

func rejectSidecars(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fail("SCHEMA_SIDECAR_PRESENT")
		} else if !os.IsNotExist(err) {
			return fail("SCHEMA_DATABASE_UNREADABLE")
		}
	}
	return nil
}

func unchangedSnapshot(path string, before os.FileInfo) error {
	if err := rejectSidecars(path); err != nil {
		return err
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fail("SCHEMA_SOURCE_CHANGED")
	}
	return nil
}

func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := u.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "trusted_schema(0)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fail("SCHEMA_DATABASE_UNREADABLE")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		db.Close()
		return nil, inspectionError(ctx)
	}
	if queryOnly != 1 {
		db.Close()
		return nil, fail("SCHEMA_READ_ONLY_UNVERIFIED")
	}
	return db, nil
}

func inspectionError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fail(SafeCode(err))
	}
	return fail("SCHEMA_INSPECTION_FAILED")
}

func objectKind(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT type FROM sqlite_schema WHERE name=? COLLATE NOCASE AND type IN ('table','view')`, name).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil || kind != "table" {
		return kind, err
	}
	// sqlite_schema labels virtual tables as 'table'. table_list distinguishes
	// them without fetching executable CREATE SQL or querying their contents.
	err = tx.QueryRowContext(ctx, `SELECT type FROM pragma_table_list WHERE schema='main' AND name=? COLLATE NOCASE`, name).Scan(&kind)
	return kind, err
}

type columnInfo struct {
	name, dataType string
	notNull, pk    int
	defaultValue   sql.NullString
	hidden         int
}

func inspectTable(ctx context.Context, tx *sql.Tx, table tableContract, report *Report) error {
	rows, err := tx.QueryContext(ctx, `SELECT name,type,"notnull",
      CASE WHEN ?='membership_processor_lease' AND name COLLATE NOCASE IN ('epoch','status') THEN dflt_value END,
      pk,hidden FROM pragma_table_xinfo(?)`, table.Name, table.Name)
	if err != nil {
		return err
	}
	columns := map[string]columnInfo{}
	var primary []columnInfo
	for rows.Next() {
		var column columnInfo
		if err := rows.Scan(&column.name, &column.dataType, &column.notNull, &column.defaultValue, &column.pk, &column.hidden); err != nil {
			rows.Close()
			return err
		}
		columns[strings.ToLower(column.name)] = column
		if column.pk != 0 {
			primary = append(primary, column)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, expected := range table.Columns {
		mustExist := required(expected.RequiredWhen, report.PlatformCatalogPresent)
		actual, present := columns[expected.Name]
		if !present {
			if mustExist {
				report.Missing = append(report.Missing, Finding{Code: "COLUMN_MISSING", Table: table.Name, Column: expected.Name})
			}
			continue
		}
		incompatible := func(code string) {
			report.Incompatible = append(report.Incompatible, Finding{Code: code, Table: table.Name, Column: expected.Name})
		}
		// SQL writability has its own evidence; a test-only type hint neither
		// proves nor disproves that production INSERT/UPDATE explicitly writes
		// a column. Optional compatibility columns must also be writable when
		// present and selected by the existing column-presence branches.
		if len(expected.WriteSources) != 0 && actual.hidden != 0 {
			incompatible("COLUMN_NOT_WRITABLE")
		}
		if !mustExist {
			continue
		}
		report.Coverage.PresentColumns++
		if expected.Affinity != "" && affinity(actual.dataType) != expected.Affinity {
			incompatible("COLUMN_AFFINITY_INCOMPATIBLE")
		}
		if expected.RequireNotNull && actual.notNull == 0 {
			incompatible("COLUMN_NOT_NULL_MISSING")
		}
		if expected.RequireNullable && actual.notNull != 0 {
			incompatible("COLUMN_MUST_ALLOW_NULL")
		}
		if expected.DefaultLiteral != nil {
			if !actual.defaultValue.Valid {
				incompatible("COLUMN_DEFAULT_MISSING")
			} else if same, known := sameLiteral(*expected.DefaultLiteral, actual.defaultValue.String); !known {
				report.Unknown = append(report.Unknown, Finding{Code: "COLUMN_DEFAULT_SEMANTICS_UNVERIFIED", Table: table.Name, Column: expected.Name})
			} else if !same {
				incompatible("COLUMN_DEFAULT_INCOMPATIBLE")
			}
		}
	}
	sort.Slice(primary, func(i, j int) bool { return primary[i].pk < primary[j].pk })
	var primaryNames []string
	for _, column := range primary {
		primaryNames = append(primaryNames, strings.ToLower(column.name))
	}
	if len(table.PrimaryKey) != 0 && !sameColumns(table.PrimaryKey, primaryNames) {
		report.Incompatible = append(report.Incompatible, Finding{Code: "PRIMARY_KEY_INCOMPATIBLE", Table: table.Name, Columns: table.PrimaryKey})
	}
	keys, err := uniqueKeys(ctx, tx, table.Name)
	if err != nil {
		return err
	}
	if len(primaryNames) != 0 {
		keys = append(keys, primaryNames)
	}
	for _, unique := range table.Unique {
		if required(unique.RequiredWhen, report.PlatformCatalogPresent) && !hasKey(keys, unique.Columns) {
			report.Missing = append(report.Missing, Finding{Code: "UPSERT_UNIQUE_KEY_MISSING", Table: table.Name, Columns: unique.Columns})
		}
	}
	if table.Name == "managed_cards" && report.PlatformCatalogPresent && hasKey(keys, []string{"upstream_card_id"}) {
		report.Incompatible = append(report.Incompatible, Finding{Code: "LEGACY_GLOBAL_CARD_ID_UNIQUENESS", Table: table.Name, Columns: []string{"upstream_card_id"}})
	}
	for _, code := range table.Unknown {
		report.Unknown = append(report.Unknown, Finding{Code: code, Table: table.Name})
	}
	return nil
}

func uniqueKeys(ctx context.Context, tx *sql.Tx, table string) ([][]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_index_list(?) WHERE "unique"=1 AND partial=0`, table)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var keys [][]string
	for _, name := range names {
		rows, err := tx.QueryContext(ctx, `SELECT cid,name FROM pragma_index_xinfo(?) WHERE key=1 ORDER BY seqno`, name)
		if err != nil {
			return nil, err
		}
		var columns []string
		usable := true
		for rows.Next() {
			var cid int
			var column sql.NullString
			if err := rows.Scan(&cid, &column); err != nil {
				rows.Close()
				return nil, err
			}
			if cid < 0 || !column.Valid {
				usable = false // expression/rowid indexes do not match column targets
			} else {
				columns = append(columns, strings.ToLower(column.String))
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if usable && len(columns) != 0 {
			keys = append(keys, columns)
		}
	}
	return keys, nil
}

func hasKey(keys [][]string, wanted []string) bool {
	for _, key := range keys {
		if sameColumns(key, wanted) {
			return true
		}
	}
	return false
}

func sameColumns(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left, right := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func affinity(value string) string {
	value = strings.ToUpper(value)
	switch {
	case strings.Contains(value, "INT"):
		return "INTEGER"
	case strings.Contains(value, "CHAR"), strings.Contains(value, "CLOB"), strings.Contains(value, "TEXT"):
		return "TEXT"
	case value == "", strings.Contains(value, "BLOB"):
		return "BLOB"
	case strings.Contains(value, "REAL"), strings.Contains(value, "FLOA"), strings.Contains(value, "DOUB"):
		return "REAL"
	default:
		return "NUMERIC"
	}
}

var sqliteDecimalLiteral = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

// Only compare simple literal metadata; never execute an input default
// expression or expose its text. More complex defaults remain unknown.
func sameLiteral(expected, actual string) (same, known bool) {
	actual = strings.TrimSpace(actual)
	for strings.HasPrefix(actual, "(") && strings.HasSuffix(actual, ")") {
		actual = strings.TrimSpace(actual[1 : len(actual)-1])
	}
	if actual == expected {
		return true, true
	}
	if expected == "0" {
		number := actual
		if len(number) >= 2 && number[0] == '\'' && number[len(number)-1] == '\'' {
			number = number[1 : len(number)-1]
		}
		// Go also parses hexadecimal floating-point strings and underscores;
		// SQLite INTEGER affinity does not turn those quoted strings into 0.
		if sqliteDecimalLiteral.MatchString(number) {
			value, err := strconv.ParseFloat(number, 64)
			if err == nil {
				return value == 0, true
			}
		}
	}
	if len(actual) >= 2 && actual[0] == '\'' && actual[len(actual)-1] == '\'' && !strings.Contains(actual[1:len(actual)-1], "'") {
		return actual == expected, true
	}
	return false, false
}
