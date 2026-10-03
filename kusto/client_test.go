package kusto

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const successfulV2 = `[
 {"FrameType":"DataSetHeader","Version":"v2.0","IsProgressive":false},
 {"FrameType":"DataTable","TableId":0,"TableKind":"PrimaryResult","TableName":"First","Columns":[{"ColumnName":"n","ColumnType":"long"},{"ColumnName":"bag","ColumnType":"dynamic"}],"Rows":[[9223372036854775807,{"n":9007199254740993}],[null,[true,"text"]]]},
 {"FrameType":"DataTable","TableId":1,"TableKind":"QueryProperties","TableName":"Properties","Columns":[],"Rows":[]},
 {"FrameType":"DataTable","TableId":2,"TableKind":"PrimaryResult","TableName":"Second","Columns":[{"ColumnName":"decimal","ColumnType":"decimal"},{"ColumnName":"time","ColumnType":"datetime"}],"Rows":[["12345678901234567890.12345678","2026-10-04T00:00:00Z"]]},
 {"FrameType":"DataSetCompletion","HasErrors":false,"Cancelled":false}
]`

const successfulV1 = `{"Tables":[
 {"TableName":"Table_0","Columns":[{"ColumnName":"Name","ColumnType":"string","DataType":"String"}],"Rows":[["Telemetry"]]},
 {"TableName":"QueryStatus","Columns":[{"ColumnName":"Severity","ColumnType":"int"}],"Rows":[[4]]},
 {"TableName":"@ExtendedProperties","Columns":[],"Rows":[]},
 {"TableName":"TableOfContents","Columns":[{"ColumnName":"Ordinal","ColumnType":"int"},{"ColumnName":"Kind","ColumnType":"string"},{"ColumnName":"Name","ColumnType":"string"},{"ColumnName":"Id","ColumnType":"string"}],"Rows":[[0,"QueryResult","Table_0","primary-id"],[1,"QueryStatus","QueryStatus","status-id"],[2,"QueryProperties","@ExtendedProperties","properties-id"]]}
]}`

func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: server.URL, HTTPClient: server.Client(), AllowInsecureHTTP: true, TokenProvider: func(context.Context) (string, error) { return "secret-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNativeRequestAndTypedResults(t *testing.T) {
	var calls atomic.Int32
	var tokens atomic.Int32
	query := "declare query_parameters(name:string);\nprint x=name; print y=long(9223372036854775807)"
	command := ".execute database script <|\n.create table Test (x:long)\n.show tables"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer secret-token" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json; charset=utf-8" || r.Header.Get("x-ms-client-request-id") != "test;request" {
			t.Error("invalid headers/method")
		}
		var body struct {
			Database   string `json:"db"`
			Text       string `json:"csl"`
			Properties string `json:"properties"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var properties struct {
			Options         map[string]any
			Parameters      map[string]string
			ClientRequestID string `json:"ClientRequestId"`
		}
		if err := json.Unmarshal([]byte(body.Properties), &properties); err != nil {
			t.Error(err)
			return
		}
		if properties.ClientRequestID != "test;request" || properties.Parameters["name"] != "'never interpolate'; .drop table T" {
			t.Error("lost request properties")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("x-ms-client-request-id", "service-request")
		w.Header().Set("x-ms-activity-id", "activity")
		switch r.URL.Path {
		case "/v2/rest/query":
			if body.Database != "Samples" || body.Text != query || properties.Options["results_progressive_enabled"] != false {
				t.Error("invalid query request")
			}
			io.WriteString(w, successfulV2)
		case "/v1/rest/mgmt":
			if body.Database != "" || body.Text != command || properties.Options["results_progressive_enabled"] != true {
				t.Error("invalid management request")
			}
			io.WriteString(w, successfulV1)
		default:
			t.Error("wrong REST endpoint")
		}
	}))
	defer server.Close()
	client, err := New(Config{Endpoint: server.URL + "/", HTTPClient: server.Client(), TokenProvider: func(context.Context) (string, error) { tokens.Add(1); return "secret-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	options := map[string]any{"results_progressive_enabled": true, "queryconsistency": "strongconsistency"}
	request := Request{Database: "Samples", Text: query, Options: options, Parameters: map[string]string{"name": "'never interpolate'; .drop table T"}, ClientRequestID: "test;request"}
	result, err := client.Query(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if options["results_progressive_enabled"] != true {
		t.Fatal("client mutated caller options")
	}
	if len(result.Tables) != 3 || len(result.PrimaryTables()) != 2 || result.RequestID != "service-request" || result.ActivityID != "activity" {
		t.Fatalf("invalid results: %#v", result)
	}
	if string(result.Tables[0].Rows[0][0]) != "9223372036854775807" || string(result.Tables[0].Rows[0][1]) != `{"n":9007199254740993}` || string(result.Tables[0].Rows[1][0]) != "null" || string(result.Tables[2].Rows[0][0]) != `"12345678901234567890.12345678"` {
		t.Fatal("lost exact typed values")
	}
	request.Text, request.Database = command, ""
	result, err = client.Management(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PrimaryTables()) != 1 || result.Tables[0].Columns[0].DataType != "String" || string(result.Tables[0].Rows[0][0]) != `"Telemetry"` {
		t.Fatal("invalid management result")
	}
	if tokens.Load() != 2 || calls.Load() != 2 {
		t.Fatal("tokens not acquired per request")
	}
}

func TestClientConfiguration(t *testing.T) {
	for _, endpoint := range []string{"", "https:", "//cluster.example", "http://cluster.example", "https://user:password@cluster.example", "https://cluster.example/subpath", "https://cluster.example?token=secret", "https://cluster.example?", "https://cluster.example/#fragment", "ftp://cluster.example", "https://cluster.example/%2f"} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := New(Config{Endpoint: endpoint, TokenProvider: func(context.Context) (string, error) { return "token", nil }}); err == nil {
				t.Fatal("accepted invalid endpoint")
			}
		})
	}
	if _, err := New(Config{Endpoint: "https://cluster.example"}); err == nil {
		t.Fatal("accepted missing token provider")
	}
	for _, limit := range []int64{-1, maxResponseBytes + 1} {
		if _, err := New(Config{Endpoint: "https://cluster.example", MaxResponseBytes: limit, TokenProvider: func(context.Context) (string, error) { return "token", nil }}); err == nil {
			t.Fatal("accepted invalid limit")
		}
	}
}

func TestFailuresDoNotReturnPartialResults(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		management bool
		want       error
	}{
		{"partial", strings.Replace(successfulV2, `"HasErrors":false`, `"HasErrors":true`, 1), 200, false, ErrPartial},
		{"cancelled", strings.Replace(successfulV2, `"Cancelled":false`, `"Cancelled":true`, 1), 200, false, ErrCancelled},
		{"error-list", strings.Replace(successfulV2, `"HasErrors":false`, `"OneApiErrors":[{"message":"secret"}],"HasErrors":false`, 1), 200, false, ErrPartial},
		{"management-status", strings.Replace(successfulV1, `"Rows":[[4]]`, `"Rows":[[2]]`, 1), 200, true, ErrPartial},
		{"management-error", `{"error":{"message":"secret"}}`, 200, true, ErrPartial},
		{"unauthorized", `secret`, 401, false, nil},
		{"throttled", `{"error":{"code":"TooManyRequests","message":"secret"}}`, 429, false, nil},
		{"unavailable", `secret`, 503, true, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("x-ms-activity-id", "correlation")
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			client := newTestClient(t, server)
			request := Request{Database: "Samples", Text: "print x=1"}
			var result *Result
			var err error
			if test.management {
				result, err = client.Management(context.Background(), request)
			} else {
				result, err = client.Query(context.Background(), request)
			}
			var serviceError *ServiceError
			if err == nil || result != nil || !errors.As(err, &serviceError) || serviceError.StatusCode != test.status || serviceError.ActivityID != "correlation" || len(serviceError.Details) == 0 {
				t.Fatalf("missing service failure: %v", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("want %v, got %v", test.want, err)
			}
			if strings.Contains(err.Error(), "secret") || calls.Load() != 1 {
				t.Fatal("error leaks response or request retried")
			}
			encoded, _ := json.Marshal(serviceError)
			if strings.Contains(string(encoded), "secret") {
				t.Fatal("service details leaked through JSON encoding")
			}
		})
	}
}

func TestInvalidRequestsAndResponseReadDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "[")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client := newTestClient(t, server)
	for _, request := range []Request{
		{Text: "print x=1"}, {Database: "Samples"},
		{Database: "Samples", Text: "\xff"},
		{Database: "Samples", Text: "print x=1", ClientRequestID: "id\r\n"},
		{Database: "Samples", Text: "print x=1", Parameters: map[string]string{"p": "\xff"}},
		{Database: "Samples", Text: "print x=1", Options: map[string]any{"unsupported": make(chan int)}},
		{Database: "Samples", Text: strings.Repeat("x", maxRequestBytes+1)},
	} {
		if _, err := client.Query(context.Background(), request); err == nil {
			t.Fatal("accepted invalid request")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request sent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if result, err := client.Query(ctx, Request{Database: "Samples", Text: "print x=1"}); result != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("response read ignored deadline: %v", err)
	}
}

func TestRefuseRedirectAndBoundResponse(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); io.WriteString(w, successfulV2) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := newTestClient(t, origin)
	result, err := client.Management(context.Background(), Request{Text: ".show databases"})
	var serviceError *ServiceError
	if result != nil || !errors.As(err, &serviceError) || serviceError.StatusCode != 307 || redirected.Load() != 0 {
		t.Fatalf("redirect was not rejected: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, successfulV2)
	}))
	defer server.Close()
	client = newTestClient(t, server)
	client.responseBytes = int64(len(successfulV2) - 1)
	if result, err = client.Query(context.Background(), Request{Database: "Samples", Text: "print x=1"}); result != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("oversize result accepted: %v", err)
	}
	client.responseBytes++
	if _, err = client.Query(context.Background(), Request{Database: "Samples", Text: "print x=1"}); err != nil {
		t.Fatalf("exact response limit failed: %v", err)
	}
}

func TestDecompressedResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		io.WriteString(compressed, successfulV2)
		compressed.Close()
	}))
	defer server.Close()
	client := newTestClient(t, server)
	client.responseBytes = int64(len(successfulV2) - 1)
	if result, err := client.Query(context.Background(), Request{Database: "Samples", Text: "print x=1"}); result != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("decompressed limit not enforced: %v", err)
	}
}

func TestContextAndTokenFailures(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	client := newTestClient(t, server)
	request := Request{Database: "Samples", Text: "print x=1"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Query(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("sent already cancelled request")
	}
	for _, token := range []string{"", "token\r\nX-Other: stolen", "token secret", "token\t"} {
		client.token = func(context.Context) (string, error) { return token, nil }
		if _, err := client.Query(context.Background(), request); err == nil {
			t.Fatal("accepted invalid bearer token")
		}
	}
	tokenError := errors.New("secret provider detail")
	client.token = func(context.Context) (string, error) { return "", tokenError }
	if _, err := client.Query(context.Background(), request); !errors.Is(err, tokenError) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("token error redaction/cause failed: %v", err)
	}
	client.token = func(context.Context) (string, error) { return "token", nil }
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := client.Query(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost HTTP deadline: %v", err)
	}
}

func TestConcurrentClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, successfulV2)
	}))
	defer server.Close()
	client := newTestClient(t, server)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := client.Query(context.Background(), Request{Database: "Samples", Text: "print x=1"}); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
}
