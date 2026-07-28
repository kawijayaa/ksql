package ksql_test

import (
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/mattn/go-sqlite3"
)

const sqliteTestDriver = "ksql_sqlite3"

func init() {
	sql.Register(sqliteTestDriver, &sqlite3.SQLiteDriver{ConnectHook: func(connection *sqlite3.SQLiteConn) error {
		return connection.RegisterFunc("kql_regex", func(pattern, value string) bool {
			matched, err := regexp.MatchString(pattern, value)
			return err == nil && matched
		}, true)
	}})
}

func openInvestigationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqliteTestDriver, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	statements := []string{
		`CREATE TABLE events (TimeGenerated TEXT, Host TEXT, Source TEXT, EventType TEXT, RawData TEXT)`,
		`INSERT INTO events VALUES
            ('2026-07-28T10:00:00Z', 'alpha', 'sysmon', 'process', '{"process":{"command_line":"PowerShell -enc x"},"items":[1,2,3],"a.b":11,"a\"b":12,"a\\b":13}'),
            ('2026-07-28T09:00:00Z', 'beta', 'ual', 'login', '{"message":"ordinary","items":[]}')`,
		`CREATE TABLE ual (User TEXT, Host TEXT)`,
		`INSERT INTO ual VALUES ('alice', 'client-a'), ('bob', 'client-b')`,
		`CREATE TABLE sysmon (User TEXT, Host TEXT, Message TEXT)`,
		`INSERT INTO sysmon VALUES ('alice', 'sensor-a', 'matched')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("Exec(%q): %v", statement, err)
		}
	}
	return db
}

func sqliteCompiler() *ksql.Compiler {
	return ksql.New(dialect.SQLite(dialect.WithRegexFunction("kql_regex")), ksql.WithCatalog(investigationCatalog()))
}

func queryRows(t *testing.T, db *sql.DB, result ksql.Result) [][]any {
	t.Helper()
	if !result.OK() {
		t.Fatalf("Compile() diagnostics = %#v", result.Diagnostics)
	}
	rows, err := db.Query(result.SQL, result.Args...)
	if err != nil {
		t.Fatalf("Query(%s, %#v): %v", result.SQL, result.Args, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var resultRows [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			t.Fatal(err)
		}
		resultRows = append(resultRows, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return resultRows
}

func TestSQLiteExecInvestigationPipeline(t *testing.T) {
	db := openInvestigationDB(t)
	result := sqliteCompiler().Compile(`Events
| extend Command=tostring(RawData.process.command_line)
| where Command contains "powershell"
| project TimeGenerated, Host, Command
| order by TimeGenerated desc
| take 100`)
	rows := queryRows(t, db, result)
	if len(rows) != 1 || rows[0][1] != "alpha" || rows[0][2] != "PowerShell -enc x" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestSQLiteExecProjectionAndSearch(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := sqliteCompiler()
	projection := compiler.Compile(`Events | project-away Source, EventType | project-rename Computer=Host | project Computer, RawData`)
	rows := queryRows(t, db, projection)
	if len(rows) != 2 || rows[0][0] != "alpha" {
		t.Fatalf("projection rows = %#v", rows)
	}
	search := compiler.Compile(`Events | search "powershell"`)
	rows = queryRows(t, db, search)
	if len(rows) != 1 || rows[0][1] != "alpha" {
		t.Fatalf("search rows = %#v", rows)
	}
}

func TestSQLiteExecDynamicPaths(t *testing.T) {
	db := openInvestigationDB(t)
	result := sqliteCompiler().Compile(`Events | where Host == "alpha" | project
        First=RawData.items[0],
        Dot=RawData["a.b"],
        Quote=RawData["a\"b"],
        Slash=RawData["a\\b"]`)
	rows := queryRows(t, db, result)
	want := [][]any{{int64(1), int64(11), int64(12), int64(13)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %#v, want %#v; SQL = %s", rows, want, result.SQL)
	}
}

func TestSQLiteExecMvExpandAndApply(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := sqliteCompiler()
	expand := compiler.Compile(`Events | where Host == "alpha" | mv-expand with_itemindex=Index Item=RawData.items limit 16 | project Item, Index`)
	rows := queryRows(t, db, expand)
	want := [][]any{{int64(1), int64(0)}, {int64(2), int64(1)}, {int64(3), int64(2)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("mv-expand rows = %#v, want %#v", rows, want)
	}
	apply := compiler.Compile(`Events | where Host == "alpha" | mv-apply Item=RawData.items on (where Item > 1 | extend Doubled=Item * 2) | project Item, Doubled`)
	rows = queryRows(t, db, apply)
	want = [][]any{{int64(2), int64(4)}, {int64(3), int64(6)}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("mv-apply rows = %#v, want %#v", rows, want)
	}
}

func TestSQLiteExecJoinKinds(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := sqliteCompiler()
	anti := compiler.Compile(`UAL | project User, Host | join kind=leftanti (Sysmon | project User) on User`)
	rows := queryRows(t, db, anti)
	if want := [][]any{{"bob", "client-b"}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("leftanti rows = %#v, want %#v", rows, want)
	}
	inner := compiler.Compile(`UAL | project User, Host | join kind=inner (Sysmon | project User, Host, Message) on User`)
	rows = queryRows(t, db, inner)
	if len(rows) != 1 || fmt.Sprint(rows[0]) != "[alice client-a alice sensor-a matched]" {
		t.Fatalf("inner rows = %#v", rows)
	}
}

func TestSQLiteExecNullAndUnion(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := sqliteCompiler()
	if rows := queryRows(t, db, compiler.Compile(`Events | where Source != null | count`)); !reflect.DeepEqual(rows, [][]any{{int64(2)}}) {
		t.Fatalf("null rows = %#v", rows)
	}
	union := compiler.Compile(`Events | project Host | sort by Host asc | take 1 | union (Events | project Host | sort by Host desc | take 1)`)
	rows := queryRows(t, db, union)
	if want := [][]any{{"alpha"}, {"beta"}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("union rows = %#v, want %#v; SQL = %s", rows, want, union.SQL)
	}
}

func TestSQLiteExecMakeListMetadata(t *testing.T) {
	db := openInvestigationDB(t)
	result := sqliteCompiler().Compile(`Events | summarize Hosts=make_list(Host)`)
	rows := queryRows(t, db, result)
	if want := []ksql.Column{{Name: "Hosts", Type: ksql.TypeDynamic}}; !reflect.DeepEqual(result.Columns, want) {
		t.Fatalf("Columns = %#v, want %#v", result.Columns, want)
	}
	if want := [][]any{{`["alpha","beta"]`}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %#v, want %#v", rows, want)
	}
}

func TestSQLiteExecParameterizedInjectionValue(t *testing.T) {
	db := openInvestigationDB(t)
	compiler := ksql.New(dialect.SQLite(), ksql.WithCatalog(investigationCatalog()), ksql.WithParameters())
	result := compiler.Compile(`Events | where Host == "alpha' OR 1=1 --" | count`)
	if rows := queryRows(t, db, result); !reflect.DeepEqual(rows, [][]any{{int64(0)}}) {
		t.Fatalf("rows = %#v; SQL = %s Args = %#v", rows, result.SQL, result.Args)
	}
}
