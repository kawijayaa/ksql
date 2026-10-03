package kusto

import "encoding/json"

func applyProgressiveFrame(table *Table, kind string, fields map[string]json.RawMessage, budget *decodeBudget) error {
	switch kind {
	case "TableFragment":
		var width int
		var operation string
		if err := field(fields, "FieldCount", &width); err != nil {
			return err
		}
		if width != len(table.Columns) {
			return protocolError("fragment width differs from schema")
		}
		if err := field(fields, "TableFragmentType", &operation); err != nil {
			return err
		}
		if operation != "DataAppend" && operation != "DataReplace" {
			return protocolError("unknown fragment operation")
		}
		rows, err := decodeRows(fields, width, budget)
		if err != nil {
			return err
		}
		// Check failures before replacing rows: an error status must not be
		// erased by a later, successful replacement fragment.
		if err := statusFailure(Table{Name: table.Name, Kind: table.Kind, Columns: table.Columns, Rows: rows}); err != nil {
			return err
		}
		if operation == "DataReplace" {
			table.Rows = rows
		} else {
			table.Rows = append(table.Rows, rows...)
		}
	case "TableProgress":
		var percentage float64
		if err := field(fields, "TableProgress", &percentage); err != nil {
			return err
		}
		if percentage < 0 || percentage > 100 {
			return protocolError("progress must be between 0 and 100")
		}
		table.Progress = &percentage
	case "TableCompletion":
		var count int
		if err := field(fields, "RowCount", &count); err != nil {
			return err
		}
		if count < 0 || count != len(table.Rows) {
			return protocolError("completed row count differs from table")
		}
		return statusFailure(*table)
	}
	return nil
}
