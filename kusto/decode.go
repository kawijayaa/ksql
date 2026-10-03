package kusto

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"unicode/utf8"
)

const (
	maxTables       = 256
	maxColumns      = 4096
	maxTotalColumns = 16384
	maxRows         = 100000
	maxCells        = 1000000
	maxFrames       = 65536
)

type decodeBudget struct {
	columns, rows, cells int
	ctx                  context.Context
}

func (b *decodeBudget) checkContext() error {
	if b.ctx != nil {
		return b.ctx.Err()
	}
	return nil
}

func decoder(data []byte) *json.Decoder {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return d
}

func decodeResponse(data []byte, management bool) (*Result, error) {
	return decodeResponseContext(context.Background(), data, management)
}

func decodeResponseContext(ctx context.Context, data []byte, management bool) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, protocolError("response is not UTF-8")
	}
	budget := &decodeBudget{ctx: ctx}
	var result *Result
	var err error
	if management {
		result, err = decodeV1(data, budget)
	} else {
		result, err = decodeV2(data, budget)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// Fields are read individually and bounded before constructing objects. Rows and
// columns are decoded incrementally so maliciously wide arrays cannot allocate
// millions of cells before their limits are checked.
func object(d *json.Decoder) (map[string]json.RawMessage, error) {
	if err := delimiter(d, '{'); err != nil {
		return nil, err
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		if len(fields) >= 64 {
			return nil, ErrLimit
		}
		key, err := d.Token()
		if err != nil {
			return nil, protocolError("invalid object field")
		}
		name, ok := key.(string)
		if !ok {
			return nil, protocolError("invalid object key")
		}
		if _, exists := fields[name]; exists {
			return nil, protocolError("duplicate object field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, protocolError("invalid object value")
		}
		fields[name] = value
	}
	if err := delimiter(d, '}'); err != nil {
		return nil, err
	}
	return fields, nil
}

func delimiter(d *json.Decoder, want byte) error {
	token, err := d.Token()
	if err != nil || token != json.Delim(want) {
		return protocolError("invalid JSON structure")
	}
	return nil
}

func end(d *json.Decoder) error {
	if _, err := d.Token(); err != io.EOF {
		return protocolError("trailing JSON data")
	}
	return nil
}

func field(fields map[string]json.RawMessage, name string, value any) error {
	raw, found := fields[name]
	if !found || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, value) != nil {
		return protocolError("missing or invalid " + name)
	}
	return nil
}

func decodeV2(data []byte, budget *decodeBudget) (*Result, error) {
	d := decoder(data)
	if err := delimiter(d, '['); err != nil {
		return nil, err
	}
	result := &Result{}
	ids := make(map[int]int)
	active := make(map[int]bool)
	header, completion := false, false
	frames := 0
	for d.More() {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		if frames >= maxFrames {
			return nil, ErrLimit
		}
		frames++
		if completion {
			return nil, protocolError("frames after dataset completion")
		}
		fields, err := object(d)
		if err != nil {
			return nil, err
		}
		var kind string
		if err := field(fields, "FrameType", &kind); err != nil {
			return nil, err
		}
		if !header && kind != "DataSetHeader" {
			return nil, protocolError("missing dataset header")
		}
		switch kind {
		case "DataSetHeader":
			if header {
				return nil, protocolError("duplicate dataset header")
			}
			var version string
			if err := field(fields, "Version", &version); err != nil {
				return nil, err
			}
			if err := field(fields, "IsProgressive", &result.Progressive); err != nil {
				return nil, err
			}
			if version != "v2.0" {
				return nil, protocolError("only v2.0 results are supported")
			}
			header = true
		case "DataTable", "TableHeader":
			if kind == "TableHeader" && !result.Progressive {
				return nil, protocolError("progressive frame in buffered dataset")
			}
			if len(result.Tables) >= maxTables {
				return nil, ErrLimit
			}
			if kind == "TableHeader" {
				if _, present := fields["Rows"]; present {
					return nil, protocolError("table header cannot contain rows")
				}
				fields["Rows"] = json.RawMessage("[]")
			}
			table, err := decodeTable(fields, budget)
			if err != nil {
				return nil, err
			}
			if err := field(fields, "TableId", &table.ID); err != nil {
				return nil, err
			}
			_, seen := ids[table.ID]
			if table.ID < 0 || seen {
				return nil, protocolError("invalid or duplicate table ID")
			}
			ids[table.ID] = len(result.Tables)
			if err := field(fields, "TableKind", &table.Kind); err != nil {
				return nil, err
			}
			if table.Kind == "" {
				return nil, protocolError("empty table kind")
			}
			if err := statusFailure(table); err != nil {
				return nil, err
			}
			result.Tables = append(result.Tables, table)
			if kind == "TableHeader" {
				active[table.ID] = true
			}
		case "TableFragment", "TableProgress", "TableCompletion":
			if !result.Progressive {
				return nil, protocolError("progressive frame in buffered dataset")
			}
			var id int
			if err := field(fields, "TableId", &id); err != nil {
				return nil, err
			}
			if !active[id] {
				return nil, protocolError("frame references an unknown or completed table")
			}
			table := &result.Tables[ids[id]]
			if err := applyProgressiveFrame(table, kind, fields, budget); err != nil {
				return nil, err
			}
			if kind == "TableCompletion" {
				delete(active, id)
			}
		case "DataSetCompletion":
			var failed, cancelled bool
			if err := field(fields, "HasErrors", &failed); err != nil {
				return nil, err
			}
			if err := field(fields, "Cancelled", &cancelled); err != nil {
				return nil, err
			}
			if raw, present := fields["OneApiErrors"]; present {
				errorDecoder := decoder(raw)
				if err := delimiter(errorDecoder, '['); err != nil {
					return nil, err
				}
				if errorDecoder.More() {
					failed = true
				}
				// RawMessage was already syntax-validated. The presence of any
				// error is sufficient; no result can escape this failure path.
			}
			if cancelled {
				return nil, ErrCancelled
			}
			if failed {
				return nil, ErrPartial
			}
			if len(active) != 0 {
				return nil, protocolError("dataset completed with incomplete tables")
			}
			completion = true
		default:
			return nil, protocolError("unknown frame")
		}
	}
	if err := delimiter(d, ']'); err != nil {
		return nil, err
	}
	if err := end(d); err != nil {
		return nil, err
	}
	if !header || !completion {
		return nil, protocolError("incomplete dataset")
	}
	return result, nil
}

func decodeV1(data []byte, budget *decodeBudget) (*Result, error) {
	d := decoder(data)
	fields, err := object(d)
	if err != nil {
		return nil, err
	}
	if err := end(d); err != nil {
		return nil, err
	}
	if _, exists := fields["error"]; exists {
		return nil, ErrPartial
	}
	raw, exists := fields["Tables"]
	if !exists {
		return nil, protocolError("missing Tables")
	}
	tables := decoder(raw)
	if err := delimiter(tables, '['); err != nil {
		return nil, err
	}
	result := &Result{}
	for tables.More() {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		if len(result.Tables) >= maxTables {
			return nil, ErrLimit
		}
		fields, err := object(tables)
		if err != nil {
			return nil, err
		}
		table, err := decodeTable(fields, budget)
		if err != nil {
			return nil, err
		}
		table.ID = len(result.Tables)
		table.Kind = "PrimaryResult"
		switch table.Name {
		case "QueryStatus":
			table.Kind = "QueryCompletionInformation"
		case "TableOfContents":
			table.Kind = "TableOfContents"
		case "@ExtendedProperties":
			table.Kind = "QueryProperties"
		}
		result.Tables = append(result.Tables, table)
	}
	if err := delimiter(tables, ']'); err != nil {
		return nil, err
	}
	if err := end(tables); err != nil {
		return nil, err
	}
	if err := resolveContents(result); err != nil {
		return nil, err
	}
	for _, table := range result.Tables {
		if err := statusFailure(table); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// The v1 table of contents may identify status tables whose wire names are only
// Table_1, Table_2, etc. Classify those before checking completion information.
func resolveContents(result *Result) error {
	if len(result.Tables) == 0 {
		return nil
	}
	contents := &result.Tables[len(result.Tables)-1]
	ordinalIndex, kindIndex, nameIndex, idIndex := -1, -1, -1, -1
	for i, column := range contents.Columns {
		switch column.Name {
		case "Ordinal":
			ordinalIndex = i
		case "Kind":
			kindIndex = i
		case "Name":
			nameIndex = i
		case "Id":
			idIndex = i
		}
	}
	if contents.Name != "TableOfContents" && (len(result.Tables) < 3 || idIndex < 0) {
		return nil
	}
	if ordinalIndex < 0 || kindIndex < 0 || nameIndex < 0 {
		if contents.Name == "TableOfContents" {
			return protocolError("invalid table of contents")
		}
		return nil
	}
	contents.Kind = "TableOfContents"
	seen := make(map[int]bool)
	for _, row := range contents.Rows {
		var ordinal int
		var kind, name string
		if json.Unmarshal(row[ordinalIndex], &ordinal) != nil || bytes.Equal(row[ordinalIndex], []byte("null")) || ordinal < 0 || ordinal >= len(result.Tables)-1 || seen[ordinal] || json.Unmarshal(row[kindIndex], &kind) != nil || kind == "" || json.Unmarshal(row[nameIndex], &name) != nil || bytes.Equal(row[nameIndex], []byte("null")) {
			return protocolError("invalid table of contents entry")
		}
		seen[ordinal] = true
		table := &result.Tables[ordinal]
		table.Name = name
		switch kind {
		case "QueryResult":
			table.Kind = "PrimaryResult"
		case "QueryProperties":
			table.Kind = "QueryProperties"
		case "QueryStatus":
			table.Kind = "QueryCompletionInformation"
		default:
			table.Kind = kind
		}
	}
	if len(seen) != len(result.Tables)-1 {
		return protocolError("incomplete table of contents")
	}
	return nil
}

func decodeTable(fields map[string]json.RawMessage, budget *decodeBudget) (Table, error) {
	var table Table
	if err := field(fields, "TableName", &table.Name); err != nil {
		return table, err
	}
	columnData, present := fields["Columns"]
	if !present {
		return table, protocolError("missing Columns")
	}
	columns := decoder(columnData)
	if err := delimiter(columns, '['); err != nil {
		return table, err
	}
	for columns.More() {
		if err := budget.checkContext(); err != nil {
			return table, err
		}
		if len(table.Columns) >= maxColumns || budget.columns >= maxTotalColumns {
			return table, ErrLimit
		}
		fields, err := object(columns)
		if err != nil {
			return table, err
		}
		var column Column
		if err := field(fields, "ColumnName", &column.Name); err != nil {
			return table, err
		}
		if err := field(fields, "ColumnType", &column.Type); err != nil {
			return table, err
		}
		if column.Type == "" {
			return table, protocolError("empty column type")
		}
		if raw, present := fields["DataType"]; present && json.Unmarshal(raw, &column.DataType) != nil {
			return table, protocolError("invalid DataType")
		}
		table.Columns = append(table.Columns, column)
		budget.columns++
	}
	if err := delimiter(columns, ']'); err != nil {
		return table, err
	}
	if err := end(columns); err != nil {
		return table, err
	}
	var err error
	table.Rows, err = decodeRows(fields, len(table.Columns), budget)
	return table, err
}

func decodeRows(fields map[string]json.RawMessage, width int, budget *decodeBudget) ([][]json.RawMessage, error) {
	rowData, present := fields["Rows"]
	if !present {
		return nil, protocolError("missing Rows")
	}
	rows := decoder(rowData)
	if err := delimiter(rows, '['); err != nil {
		return nil, err
	}
	var result [][]json.RawMessage
	for rows.More() {
		if err := budget.checkContext(); err != nil {
			return nil, err
		}
		if budget.rows >= maxRows {
			return nil, ErrLimit
		}
		rowToken, err := rows.Token()
		if err != nil {
			return nil, protocolError("invalid row structure")
		}
		// Kusto can inject a OneAPI error object between rows after HTTP
		// 200. A dynamic object cell remains inside a normal row array.
		if rowToken == json.Delim('{') {
			return nil, ErrPartial
		}
		if rowToken != json.Delim('[') {
			return nil, protocolError("invalid row structure")
		}
		row := make([]json.RawMessage, 0, width)
		for rows.More() {
			if len(row) >= width {
				return nil, protocolError("row width differs from schema")
			}
			if budget.cells >= maxCells {
				return nil, ErrLimit
			}
			var cell json.RawMessage
			if err := rows.Decode(&cell); err != nil {
				return nil, protocolError("invalid cell")
			}
			row = append(row, cell)
			budget.cells++
		}
		if err := delimiter(rows, ']'); err != nil {
			return nil, err
		}
		if len(row) != width {
			return nil, protocolError("row width differs from schema")
		}
		result = append(result, row)
		budget.rows++
	}
	if err := delimiter(rows, ']'); err != nil {
		return nil, err
	}
	if err := end(rows); err != nil {
		return nil, err
	}
	return result, nil
}

func statusFailure(table Table) error {
	if table.Name != "QueryStatus" && table.Kind != "QueryCompletionInformation" {
		return nil
	}
	severityIndex := -1
	failureThreshold := 2
	for i, column := range table.Columns {
		if column.Name == "Severity" {
			severityIndex = i
			break
		}
	}
	if severityIndex < 0 && table.Kind == "QueryCompletionInformation" {
		for i, column := range table.Columns {
			if column.Name == "Level" {
				severityIndex = i
				failureThreshold = 3
				break
			}
		}
	}
	// v2 completion-information schemas can differ. The required dataset
	// completion flags are authoritative if neither status column is present.
	if severityIndex < 0 {
		if table.Name == "QueryStatus" {
			return protocolError("QueryStatus is missing Severity")
		}
		return nil
	}
	for _, row := range table.Rows {
		var severity int
		if bytes.Equal(row[severityIndex], []byte("null")) || json.Unmarshal(row[severityIndex], &severity) != nil || severity < 0 {
			return protocolError("invalid QueryStatus severity")
		}
		if severity <= failureThreshold {
			return ErrPartial
		}
	}
	return nil
}
