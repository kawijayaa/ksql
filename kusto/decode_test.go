package kusto

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMalformedResponsesFailClosed(t *testing.T) {
	tests := []struct {
		name, body string
		management bool
	}{
		{"empty", "", false},
		{"empty-array", "[]", false},
		{"wrong-root", `{}`, false},
		{"missing-header", `[{"FrameType":"DataSetCompletion","HasErrors":false,"Cancelled":false}]`, false},
		{"no-completion", `[{"FrameType":"DataSetHeader","Version":"v2.0","IsProgressive":false}]`, false},
		{"wrong-version", strings.Replace(successfulV2, `v2.0`, `v3.0`, 1), false},
		{"progressive", strings.Replace(successfulV2, `"IsProgressive":false`, `"IsProgressive":true`, 1), false},
		{"unknown-frame", strings.Replace(successfulV2, `"FrameType":"DataTable"`, `"FrameType":"UnknownFrame"`, 1), false},
		{"trailing-json", successfulV2 + `{}`, false},
		{"duplicate-fields", strings.Replace(successfulV2, `"HasErrors":false`, `"HasErrors":false,"HasErrors":false`, 1), false},
		{"null-flag", strings.Replace(successfulV2, `"HasErrors":false`, `"HasErrors":null`, 1), false},
		{"no-flag", strings.Replace(successfulV2, `"HasErrors":false,`, ``, 1), false},
		{"error-object", strings.Replace(successfulV2, `"HasErrors":false`, `"OneApiErrors":{},"HasErrors":false`, 1), false},
		{"duplicate-id", strings.Replace(successfulV2, `"TableId":2`, `"TableId":0`, 1), false},
		{"negative-id", strings.Replace(successfulV2, `"TableId":2`, `"TableId":-1`, 1), false},
		{"fraction-id", strings.Replace(successfulV2, `"TableId":2`, `"TableId":2.5`, 1), false},
		{"short-row", strings.Replace(successfulV2, `[null,[true,"text"]]`, `[null]`, 1), false},
		{"wide-row", strings.Replace(successfulV2, `[null,[true,"text"]]`, `[null,[true,"text"],123]`, 1), false},
		{"null-row", strings.Replace(successfulV2, `[null,[true,"text"]]`, `null`, 1), false},
		{"null-columns", strings.Replace(successfulV2, `"Columns":[]`, `"Columns":null`, 1), false},
		{"invalid-utf8", strings.Replace(successfulV2, "First", "\xff", 1), false},
		{"management-no-tables", `{}`, true},
		{"management-null-tables", `{"Tables":null}`, true},
		{"management-trailing", successfulV1 + `{}`, true},
		{"management-severity", strings.Replace(successfulV1, `"Rows":[[4]]`, `"Rows":[[null]]`, 1), true},
		{"management-status-no-severity", strings.Replace(successfulV1, `"ColumnName":"Severity"`, `"ColumnName":"Unknown"`, 1), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := decodeResponse([]byte(test.body), test.management)
			if result != nil || !errors.Is(err, ErrProtocol) {
				t.Fatalf("wanted protocol failure, got %v / %v", result, err)
			}
		})
	}
	// Correct completion must be last, and a second header cannot reset state.
	for _, frame := range []string{`{"FrameType":"DataSetHeader","Version":"v2.0","IsProgressive":false}`, `{"FrameType":"DataSetCompletion","HasErrors":false,"Cancelled":false}`} {
		body := strings.TrimSpace(successfulV2)
		body = strings.TrimSuffix(body, "]") + "," + frame + "]"
		if _, err := decodeResponse([]byte(body), false); !errors.Is(err, ErrProtocol) {
			t.Fatalf("accepted trailing frame: %v", err)
		}
	}
}

func TestResponseBudgets(t *testing.T) {
	column := `{"ColumnName":"x","ColumnType":"long"}`
	makeTable := func(columns, rows string) string {
		return `{"TableName":"Table_0","Columns":[` + columns + `],"Rows":[` + rows + `]}`
	}
	cases := []struct{ name, tables string }{
		{"columns", makeTable(strings.TrimSuffix(strings.Repeat(column+",", maxColumns+1), ","), "")},
		{"tables", strings.TrimSuffix(strings.Repeat(makeTable("", "")+",", maxTables+1), ",")},
		{"rows", makeTable("", strings.TrimSuffix(strings.Repeat("[],", maxRows+1), ","))},
		{"total-columns", strings.TrimSuffix(strings.Repeat(makeTable(strings.TrimSuffix(strings.Repeat(column+",", maxColumns), ","), "")+",", 5), ",")},
	}
	width := 1024
	row := "[" + strings.TrimSuffix(strings.Repeat("0,", width), ",") + "]"
	cases = append(cases, struct{ name, tables string }{"cells", makeTable(strings.TrimSuffix(strings.Repeat(column+",", width), ","), strings.TrimSuffix(strings.Repeat(row+",", maxCells/width+1), ","))})
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result, err := decodeResponse([]byte(`{"Tables":[`+test.tables+`]}`), true)
			if result != nil || !errors.Is(err, ErrLimit) {
				t.Fatalf("resource limit did not fail: %v", err)
			}
		})
	}
}

func TestEmptyAndFutureMetadata(t *testing.T) {
	for _, body := range []string{`{"Tables":[]}`, `{"Tables":[{"TableName":"Result","Columns":[],"Rows":[[]]}],"FutureField":{"enabled":true}}`} {
		if _, err := decodeResponse([]byte(body), true); err != nil {
			t.Fatal(err)
		}
	}
	body := strings.Replace(successfulV2, `"TableKind":"QueryProperties"`, `"TableKind":"FutureMetadataKind"`, 1)
	body = strings.Replace(body, `"HasErrors":false`, `"OneApiErrors":[],"HasErrors":false`, 1)
	if result, err := decodeResponse([]byte(body), false); err != nil || len(result.PrimaryTables()) != 2 {
		t.Fatalf("future metadata/empty errors invalid: %v", err)
	}
}

func TestTableOfContentsAndCompletionLevels(t *testing.T) {
	// Wire table names do not necessarily identify completion information.
	body := strings.Replace(successfulV1, `"TableName":"QueryStatus"`, `"TableName":"Table_1"`, 1)
	body = strings.Replace(body, `"TableName":"TableOfContents"`, `"TableName":"Table_3"`, 1)
	result, err := decodeResponse([]byte(body), true)
	if err != nil || len(result.PrimaryTables()) != 1 || result.Tables[1].Kind != "QueryCompletionInformation" {
		t.Fatalf("contents not applied: %v", err)
	}
	body = strings.Replace(body, `"Rows":[[4]]`, `"Rows":[[1]]`, 1)
	if result, err = decodeResponse([]byte(body), true); result != nil || !errors.Is(err, ErrPartial) {
		t.Fatalf("hidden status failure accepted: %v", err)
	}
	for _, replacement := range []string{`[0,"QueryStatus","QueryStatus","status-id"]`, `[99,"QueryStatus","QueryStatus","status-id"]`, `[null,"QueryStatus","QueryStatus","status-id"]`} {
		body = strings.Replace(successfulV1, `[1,"QueryStatus","QueryStatus","status-id"]`, replacement, 1)
		if _, err := decodeResponse([]byte(body), true); !errors.Is(err, ErrProtocol) {
			t.Fatalf("invalid contents accepted: %v", err)
		}
	}
	body = strings.Replace(successfulV2, `"TableKind":"QueryProperties"`, `"TableKind":"QueryCompletionInformation"`, 1)
	body = strings.Replace(body, `"Columns":[],"Rows":[]`, `"Columns":[{"ColumnName":"Level","ColumnType":"int"}],"Rows":[[2]]`, 1)
	if result, err = decodeResponse([]byte(body), false); result != nil || !errors.Is(err, ErrPartial) {
		t.Fatalf("v2 status failure accepted: %v", err)
	}
}

func FuzzDecodeResponse(f *testing.F) {
	for _, body := range []string{successfulV2, successfulV1, `null`, `{"Tables":[null]}`, `[]`, `[{"FrameType":"DataSetHeader"}]`} {
		f.Add([]byte(body), false)
		f.Add([]byte(body), true)
	}
	f.Fuzz(func(t *testing.T, data []byte, management bool) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		result, err := decodeResponse(data, management)
		if err != nil {
			if result != nil {
				t.Fatal("partial results leaked")
			}
			return
		}
		if result == nil {
			t.Fatal("success without result")
		}
		for _, table := range result.Tables {
			for _, row := range table.Rows {
				if len(row) != len(table.Columns) {
					t.Fatal("nonrectangular success")
				}
				for _, cell := range row {
					if !json.Valid(cell) {
						t.Fatal(fmt.Sprintf("invalid successful cell %q", cell))
					}
				}
			}
		}
	})
}
