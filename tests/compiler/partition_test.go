package compiler_test

import (
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"github.com/kawijayaa/ksql"
	"github.com/kawijayaa/ksql/dialect"
	"github.com/kawijayaa/ksql/sqlast"
)

func valuesResolver(t *testing.T, db *sql.DB, target dialect.Dialect) ksql.ValuesResolver {
	return func(query sqlast.Query, args []any) ([]any, error) {
		text, err := dialect.Render(target, query)
		if err != nil {
			return nil, err
		}
		rows, err := db.Query(text, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var values []any
		for rows.Next() {
			var value any
			if err := rows.Scan(&value); err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, rows.Err()
	}
}

// Microsoft's partition shapes: per-key aggregation and top, the legacy
// form with its own source, null keys and the partition limit.
func TestSQLiteExecPartition(t *testing.T) {
	db := openInvestigationDB(t)
	target := dialect.SQLite(dialect.WithRegexFunction("kql_regex"))
	compiler := ksql.New(target, ksql.WithParameters(), ksql.WithCatalog(investigationCatalog()), ksql.WithValuesResolver(valuesResolver(t, db, target)))
	for _, tc := range []struct{ query, want string }{
		{`datatable(g:string, v:long)["a", 1, "a", 5, "b", 2, "b", 9, "b", 4] | partition hint.strategy=native by g (top 2 by v | project g, v)`, `[[a 1] [a 5] [b 4] [b 9]]`},
		{`datatable(g:string, v:long)["a", 1, "a", 5, "b", 2] | partition by g (summarize n=count(), s=sum(v))`, `[[1 2] [2 6]]`},
		{`datatable(g:string, v:long)["a", 1, "", 5, "b", 2] | extend g = iff(g == "", string(null), g) | partition by g (summarize n=count())`, `[[1] [1] [1]]`},
		{`range x from 1 to 3 step 1 | partition hint.strategy=legacy by x {UAL | where strlen(User) == x + 2 | project User, x}`, `[[alice 3] [bob 1]]`},
		{`datatable(g:long)[] | partition by g (count)`, `[]`},
		{`UAL | partition by Host (take 1 | project User)`, `[[alice] [bob]]`},
	} {
		result := compiler.Compile(tc.query)
		rows := queryRows(t, db, result)
		sort.Slice(rows, func(i, j int) bool { return fmt.Sprint(rows[i]) < fmt.Sprint(rows[j]) })
		if got := fmt.Sprint(rows); got != tc.want {
			t.Fatalf("%s:\n got=%s\nwant=%s\nSQL=%s", tc.query, got, tc.want, result.SQL)
		}
	}
	for _, tc := range []struct {
		compiler *ksql.Compiler
		query    string
		code     string
	}{
		{compiler, `range x from 1 to 65 step 1 | partition by x (count)`, "KQLB0359"},
		{compiler, `range x from 1 to 3 step 1 | partition by y (count)`, "KQLB0401"},
		{compiler, `range x from 1 to 3 step 1 | partition hint.strategy=native by x {UAL}`, "KQLP0358"},
		{ksql.New(target), `range x from 1 to 3 step 1 | partition by x (count)`, "KQLL0394"},
	} {
		result := tc.compiler.Compile(tc.query)
		if len(result.Diagnostics) == 0 || result.Diagnostics[0].Code != tc.code {
			t.Fatalf("%s: diagnostics %v want %s", tc.query, result.Diagnostics, tc.code)
		}
	}
	if result := compiler.Compile(`range x from 1 to 65 step 1 | partition hint.strategy=shuffle by x (count) | count`); fmt.Sprint(queryRows(t, db, result)) != "[[65]]" {
		t.Fatalf("native partitions beyond 64: %v", result.Diagnostics)
	}
}
