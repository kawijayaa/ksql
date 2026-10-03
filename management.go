package ksql

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kawijayaa/ksql/kql"
	"github.com/kawijayaa/ksql/sqlast"
)

// ManagementTable describes an actual logical table in a management catalog.
type ManagementTable struct {
	Name      string
	Schema    Schema
	Folder    string
	DocString string
}

// ManagementCatalog supplies the local database namespace and table metadata.
// It does not imply access to a Kusto cluster or support for mutating commands.
type ManagementCatalog struct {
	DatabaseName string
	Tables       []ManagementTable
	err          string
}

// WithManagementCatalog snapshots a catalog for read-only table metadata commands.
// Subsequent changes to the caller's slices do not change the compiler's catalog.
func WithManagementCatalog(catalog ManagementCatalog) Option {
	return func(c *Compiler) {
		copy := &ManagementCatalog{DatabaseName: catalog.DatabaseName}
		c.management = copy
		if strings.TrimSpace(catalog.DatabaseName) == "" {
			copy.err = "management catalog requires a database name"
			return
		}
		if len(catalog.Tables) > 4096 {
			copy.err = "management catalog exceeds 4096 tables"
			return
		}
		seen := map[string]bool{}
		bytes := len(catalog.DatabaseName)
		columns := 0
		if bytes > 4<<20 {
			copy.err = "management catalog exceeds 4 MiB of metadata"
			return
		}
		for _, table := range catalog.Tables {
			if table.Name == "" || seen[table.Name] {
				copy.err = "management catalog contains empty or duplicate table names"
				return
			}
			seen[table.Name] = true
			if len(table.Schema.Columns) > 4096 {
				copy.err = "management table schema exceeds 4096 columns"
				return
			}
			columns += len(table.Schema.Columns)
			if columns > 32768 {
				copy.err = "management catalog exceeds 32768 total columns"
				return
			}
			bytes += len(table.Name) + len(table.Folder) + len(table.DocString)
			for _, col := range table.Schema.Columns {
				bytes += len(col.Name) + len(col.Type)
			}
			if bytes > 4<<20 {
				copy.err = "management catalog exceeds 4 MiB of metadata"
				return
			}
			table.Schema = table.Schema.Clone()
			copy.Tables = append(copy.Tables, table)
		}
		sort.Slice(copy.Tables, func(i, j int) bool { return copy.Tables[i].Name < copy.Tables[j].Name })
	}
}

func (s *compileState) managementSource(source kql.Source) Relation {
	command := source.Command
	if command == nil {
		s.bindError("KQLB0251", source.Span, "source.management", "missing management command AST")
		return Relation{}
	}
	feature := "command." + command.Kind
	if command.Kind != "show.tables" && command.Kind != "show.table.schema" {
		s.use("command.opaque", "unsupported", command.Span)
		s.error("KQLL0250", source.Span, feature, "management command requires a compatible management runtime")
		return Relation{}
	}
	s.use(feature, "equivalent", command.Span)
	catalog := s.compiler.management
	if catalog == nil {
		s.error("KQLL0251", source.Span, feature, "table metadata commands require an explicit management catalog")
		return Relation{}
	}
	if catalog.err != "" {
		s.bindError("KQLB0251", source.Span, feature, catalog.err)
		return Relation{}
	}
	lookup := func(name string) (ManagementTable, bool) {
		i := sort.Search(len(catalog.Tables), func(i int) bool { return catalog.Tables[i].Name >= name })
		if i < len(catalog.Tables) && catalog.Tables[i].Name == name {
			return catalog.Tables[i], true
		}
		return ManagementTable{}, false
	}
	tables := catalog.Tables
	if command.Kind == "show.tables" && len(command.Tables) > 0 {
		wanted := map[string]bool{}
		for _, name := range command.Tables {
			if _, found := lookup(name); !found {
				s.bindError("KQLB0252", command.Span, feature, "unknown management table "+strconv.Quote(name))
				return Relation{}
			}
			wanted[name] = true
		}
		tables = nil
		for _, table := range catalog.Tables {
			if wanted[table.Name] {
				tables = append(tables, table)
			}
		}
	} else if command.Kind == "show.table.schema" {
		table, found := lookup(command.Table)
		if !found {
			s.bindError("KQLB0252", command.Span, feature, "unknown management table "+strconv.Quote(command.Table))
			return Relation{}
		}
		tables = []ManagementTable{table}
	}
	names := []string{"TableName", "DatabaseName", "Folder", "DocString"}
	if command.Kind == "show.table.schema" {
		names = []string{"TableName", "Schema", "DatabaseName", "Folder", "DocString"}
	}
	values := &sqlast.Values{Columns: names, Alias: s.nextAlias()}
	resultBytes := 0
	for _, table := range tables {
		texts := []string{table.Name, catalog.DatabaseName, table.Folder, table.DocString}
		if command.Kind == "show.table.schema" {
			text, err := managementSchema(table, command.Format)
			if err != nil {
				s.error("KQLL0252", command.Span, feature, err.Error())
				return Relation{}
			}
			texts = []string{table.Name, text, catalog.DatabaseName, table.Folder, table.DocString}
		}
		for _, text := range texts {
			resultBytes += len(text)
			if resultBytes > 4<<20 {
				s.bindError("KQLB0253", command.Span, feature, "management result exceeds 4 MiB of metadata; select fewer tables")
				return Relation{}
			}
		}
		row := make([]sqlast.Expr, len(texts))
		for i, text := range texts {
			row[i] = s.bind(text)
		}
		values.Rows = append(values.Rows, row)
	}
	schema := Schema{}
	for _, name := range names {
		schema.Columns = append(schema.Columns, Column{Name: name, Type: TypeString})
	}
	return Relation{Query: &sqlast.Select{From: values}, Schema: schema}
}

func managementSchema(table ManagementTable, format string) (string, error) {
	if table.Schema.Unknown {
		return "", fmt.Errorf("management schema requires a known table schema")
	}
	types := map[ScalarType]string{TypeBool: "System.Boolean", TypeInt: "System.Int32", TypeLong: "System.Int64", TypeReal: "System.Double", TypeString: "System.String", TypeDateTime: "System.DateTime", TypeDynamic: "System.Object"}
	type column struct {
		Name    string
		Type    string
		CslType string
	}
	columns := []column{}
	csl := []string{}
	seen := map[string]bool{}
	for _, col := range table.Schema.Columns {
		if col.Name == "" || seen[col.Name] {
			return "", fmt.Errorf("management schema contains empty or duplicate column names")
		}
		seen[col.Name] = true
		native, ok := types[col.Type]
		if !ok {
			return "", fmt.Errorf("management schema has an unrepresented scalar type %q", col.Type)
		}
		columns = append(columns, column{Name: col.Name, Type: native, CslType: string(col.Type)})
		name := col.Name
		if !validSQLName(name) {
			name = "[" + strconv.Quote(name) + "]"
		}
		csl = append(csl, name+":"+string(col.Type))
	}
	if format == "csl" {
		return strings.Join(csl, ","), nil
	}
	data, err := json.Marshal(struct {
		Name           string
		OrderedColumns []column
	}{table.Name, columns})
	return string(data), err
}
