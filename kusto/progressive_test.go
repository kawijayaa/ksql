package kusto

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Interleaving, multiple replacements and a buffered auxiliary table exercise
// the service's progressive protocol independently of fragment arrival order.
const successfulProgressive = `[
 {"FrameType":"DataSetHeader","Version":"v2.0","IsProgressive":true},
 {"FrameType":"TableHeader","TableId":7,"TableKind":"PrimaryResult","TableName":"Values","Columns":[{"ColumnName":"n","ColumnType":"long"}]},
 {"FrameType":"TableHeader","TableId":0,"TableKind":"PrimaryResult","TableName":"Bags","Columns":[{"ColumnName":"bag","ColumnType":"dynamic"}]},
 {"FrameType":"TableFragment","TableId":7,"FieldCount":1,"TableFragmentType":"DataAppend","Rows":[[1],[2]]},
 {"FrameType":"TableProgress","TableId":7,"TableProgress":37.5},
 {"FrameType":"TableFragment","TableId":0,"FieldCount":1,"TableFragmentType":"DataAppend","Rows":[[{"n":9007199254740993}],[null]]},
 {"FrameType":"TableFragment","TableId":7,"FieldCount":1,"TableFragmentType":"DataReplace","Rows":[[100]]},
 {"FrameType":"TableFragment","TableId":7,"FieldCount":1,"TableFragmentType":"DataReplace","Rows":[[9223372036854775807],[4]]},
 {"FrameType":"TableCompletion","TableId":0,"RowCount":2},
 {"FrameType":"DataTable","TableId":11,"TableKind":"QueryProperties","TableName":"Metadata","Columns":[],"Rows":[]},
 {"FrameType":"TableFragment","TableId":7,"FieldCount":1,"TableFragmentType":"DataAppend","Rows":[[5]]},
 {"FrameType":"TableProgress","TableId":7,"TableProgress":100},
 {"FrameType":"TableCompletion","TableId":7,"RowCount":3},
 {"FrameType":"DataSetCompletion","HasErrors":false,"Cancelled":false}
]`

func TestProgressiveResultReconstruction(t *testing.T) {
	result, err := decodeResponse([]byte(successfulProgressive), false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Progressive || len(result.Tables) != 3 || len(result.PrimaryTables()) != 2 || result.Tables[0].ID != 7 || result.Tables[1].ID != 0 || result.Tables[2].ID != 11 {
		t.Fatalf("table order/kind lost: %+v", result)
	}
	if result.Tables[0].Progress == nil || *result.Tables[0].Progress != 100 || result.Tables[1].Progress != nil {
		t.Fatal("incorrect progress")
	}
	var values []string
	for _, row := range result.Tables[0].Rows {
		values = append(values, string(row[0]))
	}
	if !reflect.DeepEqual(values, []string{"9223372036854775807", "4", "5"}) || string(result.Tables[1].Rows[0][0]) != `{"n":9007199254740993}` || string(result.Tables[1].Rows[1][0]) != "null" {
		t.Fatalf("replacement/append or precision lost: %v", values)
	}
	// A progressive dataset may contain only buffered tables. The header
	// describes the mode, not a requirement to fragment every table.
	if _, err := decodeResponse([]byte(strings.Replace(successfulV2, `"IsProgressive":false`, `"IsProgressive":true`, 1)), false); err != nil {
		t.Fatal(err)
	}
}

func TestProgressiveRequestAndServiceFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "partial"}[failed], func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Properties string `json:"properties"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				var properties struct{ Options map[string]any }
				if err := json.Unmarshal([]byte(request.Properties), &properties); err != nil || properties.Options["results_progressive_enabled"] != true {
					t.Errorf("progressive not requested: %s, %v", request.Properties, err)
				}
				w.Header().Set("Content-Type", "application/json")
				body := successfulProgressive
				if failed {
					body = strings.Replace(body, `"HasErrors":false`, `"HasErrors":true`, 1)
				}
				io.WriteString(w, body)
			}))
			defer server.Close()
			client := newTestClient(t, server)
			result, err := client.Query(context.Background(), Request{Database: "Samples", Text: "print x=1", Progressive: true})
			if failed {
				if result != nil || !errors.Is(err, ErrPartial) {
					t.Fatalf("partial progressive result leaked: %v", err)
				}
			} else if err != nil || !result.Progressive || len(result.Tables[0].Rows) != 3 {
				t.Fatalf("progressive request failed: %v", err)
			}
			if result, err := client.Management(context.Background(), Request{Text: ".show databases", Progressive: true}); err == nil || result != nil {
				t.Fatal("accepted progressive management request")
			}
		})
	}
}

func TestProgressiveProtocolErrors(t *testing.T) {
	tests := []struct{ name, old, new string }{
		{"buffered-header", `"IsProgressive":true`, `"IsProgressive":false`},
		{"header-with-rows", `"TableName":"Values"`, `"Rows":[],"TableName":"Values"`},
		{"duplicate-id", `"TableId":0,"TableKind"`, `"TableId":7,"TableKind"`},
		{"unknown-table", `"TableId":7,"FieldCount"`, `"TableId":6,"FieldCount"`},
		{"missing-field-count", `"FieldCount":1,`, ` `},
		{"width", `"FieldCount":1`, `"FieldCount":2`},
		{"operation", `"TableFragmentType":"DataAppend"`, `"TableFragmentType":"Unknown"`},
		{"short-row", `"Rows":[[1],[2]]`, `"Rows":[[],[2]]`},
		{"wide-row", `"Rows":[[1],[2]]`, `"Rows":[[1,2],[2]]`},
		{"null-progress", `"TableProgress":37.5`, `"TableProgress":null`},
		{"negative-progress", `"TableProgress":37.5`, `"TableProgress":-1`},
		{"over-progress", `"TableProgress":37.5`, `"TableProgress":101`},
		{"string-progress", `"TableProgress":37.5`, `"TableProgress":"50"`},
		{"overflow-progress", `"TableProgress":37.5`, `"TableProgress":1e999`},
		{"fraction-count", `"RowCount":2`, `"RowCount":2.5`},
		{"wrong-count", `"RowCount":2`, `"RowCount":3`},
		{"negative-count", `"RowCount":2`, `"RowCount":-1`},
		{"missing-completion", `{"FrameType":"TableCompletion","TableId":0,"RowCount":2},`, ``},
		{"progress-after-completion", `{"FrameType":"TableProgress","TableId":7,"TableProgress":100}`, `{"FrameType":"TableProgress","TableId":0,"TableProgress":100}`},
		{"fragment-after-completion", `"TableId":7,"FieldCount":1,"TableFragmentType":"DataAppend","Rows":[[5]]`, `"TableId":0,"FieldCount":1,"TableFragmentType":"DataAppend","Rows":[[5]]`},
		{"duplicate-completion", `{"FrameType":"TableCompletion","TableId":0,"RowCount":2},`, `{"FrameType":"TableCompletion","TableId":0,"RowCount":2},{"FrameType":"TableCompletion","TableId":0,"RowCount":2},`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := strings.Replace(successfulProgressive, test.old, test.new, 1)
			if body == successfulProgressive {
				t.Fatal("fixture unchanged")
			}
			if result, err := decodeResponse([]byte(body), false); result != nil || !errors.Is(err, ErrProtocol) {
				t.Fatalf("expected protocol error, got %v", err)
			}
		})
	}
}

func TestProgressiveReplacementBudgetAndFailure(t *testing.T) {
	header := `{"FrameType":"DataSetHeader","Version":"v2.0","IsProgressive":true},`
	table := `{"FrameType":"TableHeader","TableId":0,"TableKind":"PrimaryResult","TableName":"Result","Columns":[{"ColumnName":"x","ColumnType":"long"}]},`
	fragment := `{"FrameType":"TableFragment","TableId":0,"FieldCount":1,"TableFragmentType":"DataReplace","Rows":[[1],[2]]},`
	// Evicted values still count toward total decoding work. A final table
	// with two rows must not bypass the 100,000-row budget by replacing them.
	body := "[" + header + table + strings.Repeat(fragment, maxRows/2+1) + `{"FrameType":"TableCompletion","TableId":0,"RowCount":2},{"FrameType":"DataSetCompletion","HasErrors":false,"Cancelled":false}]`
	if result, err := decodeResponse([]byte(body), false); result != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("replacement bypassed row budget: %v", err)
	}
	progress := `{"FrameType":"TableProgress","TableId":0,"TableProgress":50},`
	body = "[" + header + table + strings.Repeat(progress, maxFrames) + `{"FrameType":"TableCompletion","TableId":0,"RowCount":0},{"FrameType":"DataSetCompletion","HasErrors":false,"Cancelled":false}]`
	if result, err := decodeResponse([]byte(body), false); result != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("progress bypassed frame budget: %v", err)
	}
	// A failure cannot be erased by replacing its status rows with success.
	table = strings.Replace(table, `"TableKind":"PrimaryResult"`, `"TableKind":"QueryCompletionInformation"`, 1)
	table = strings.Replace(table, `"ColumnName":"x"`, `"ColumnName":"Level"`, 1)
	body = "[" + header + table + fragment + strings.Replace(fragment, `[[1],[2]]`, `[[4]]`, 1) + `{"FrameType":"TableCompletion","TableId":0,"RowCount":1},{"FrameType":"DataSetCompletion","HasErrors":false,"Cancelled":false}]`
	if result, err := decodeResponse([]byte(body), false); result != nil || !errors.Is(err, ErrPartial) {
		t.Fatalf("replacement erased service failure: %v", err)
	}
}

func TestEmptyReplacementAndCancellation(t *testing.T) {
	body := strings.Replace(successfulProgressive, `"Rows":[[9223372036854775807],[4]]`, `"Rows":[]`, 1)
	body = strings.Replace(body, `"RowCount":3`, `"RowCount":1`, 1)
	if result, err := decodeResponse([]byte(body), false); err != nil || len(result.Tables[0].Rows) != 1 || string(result.Tables[0].Rows[0][0]) != "5" {
		t.Fatalf("empty replacement failed: %v", err)
	}
	for _, flags := range []string{`"HasErrors":true,"Cancelled":false`, `"HasErrors":false,"Cancelled":true`} {
		// A failing dataset can finish before table completion. Report the
		// actual service failure rather than an incomplete-table error.
		body := successfulProgressive[:strings.Index(successfulProgressive, `{"FrameType":"TableFragment"`)] + `{"FrameType":"DataSetCompletion",` + flags + `}]`
		result, err := decodeResponse([]byte(body), false)
		if result != nil || (!errors.Is(err, ErrPartial) && !errors.Is(err, ErrCancelled)) {
			t.Fatalf("missing early service failure: %v", err)
		}
	}
}

func TestInjectedRowErrorsAndDecodeCancellation(t *testing.T) {
	for _, body := range []string{
		strings.Replace(successfulV2, `[null,[true,"text"]]`, `{"error":{"code":"PartialQueryFailure","message":"secret"}}`, 1),
		strings.Replace(successfulProgressive, `"Rows":[[1],[2]]`, `"Rows":[[1],{"error":{"code":"PartialQueryFailure","message":"secret"}}]`, 1),
		strings.Replace(successfulV1, `"Rows":[["Telemetry"]]`, `"Rows":[{"error":{"code":"PartialQueryFailure","message":"secret"}}]`, 1),
	} {
		management := strings.HasPrefix(body, `{"Tables"`)
		if result, err := decodeResponse([]byte(body), management); result != nil || !errors.Is(err, ErrPartial) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("injected error misread: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := decodeResponseContext(ctx, []byte(successfulProgressive), false); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled decoding produced a result: %v", err)
	}
}

func TestProtocolFailureCorrelation(t *testing.T) {
	for _, contentType := range []string{"text/html", "application/json"} {
		t.Run(contentType, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("x-ms-activity-id", "correlation")
				io.WriteString(w, strings.Replace(successfulProgressive, `"RowCount":3`, `"RowCount":1`, 1))
			}))
			defer server.Close()
			client := newTestClient(t, server)
			result, err := client.Query(context.Background(), Request{Database: "Samples", Text: "print x=1", Progressive: true})
			var serviceError *ServiceError
			if result != nil || !errors.Is(err, ErrProtocol) || !errors.As(err, &serviceError) || serviceError.ActivityID != "correlation" {
				t.Fatalf("protocol correlation lost: %v", err)
			}
		})
	}
}
