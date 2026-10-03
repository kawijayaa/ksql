# Native Kusto service execution

`github.com/kawijayaa/ksql/kusto` is a separate, standard-library-only REST
client. It executes native query text and management commands against a
configured Kusto service. It does not translate them into SQL or require the
local compiler to recognize their grammar. External data, cross-cluster queries,
plugins, ingestion/export commands and management scripts run on that service,
subject to its features, policies, permissions and credentials.

The SQL compiler and its feature ledger remain independent. Native forwarding
does not establish local implementation or conformance for unsupported features.

```go
client, err := kusto.New(kusto.Config{
    Endpoint: "https://my-cluster.region.kusto.windows.net",
    TokenProvider: func(ctx context.Context) (string, error) {
        return obtainClusterAccessToken(ctx) // application-owned authentication
    },
})
if err != nil { return err }

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
result, err := client.Query(ctx, kusto.Request{
    Database: "Telemetry",
    Text: "declare query_parameters(host:string); Events | where Host == host",
    Parameters: map[string]string{"host": "server-01"},
})
if err != nil { return err }
for _, table := range result.PrimaryTables() {
    // table.Columns contains native KQL type names.
    // table.Rows contains JSON cells, without float64 conversion.
    _ = table
}
```

Import `context`, `time` and the `kusto` package in the application; the token
acquisition function above is supplied by that application. Tokens must be issued
for the configured cluster audience. Token acquisition, expiry handling, caching,
refresh and Azure/Fabric identity flows are not implemented by this package.
The provider is invoked for every request and must support concurrent calls.

Use `client.Management(ctx, request)` for management commands. It sends text
unchanged, including multiline scripts and global commands with an empty
database. This method can perform mutations. Authorization belongs to the caller
and the service principal. Client adds no retries; after a connection failure,
the caller may need to inspect service state before deciding whether to retry.

## Request and result contracts

Queries use the v2 REST endpoint; management uses v1. Parameters and options are
encoded as request properties rather than interpolated into KQL. Query requests
default to buffered results. Set `Request.Progressive = true` to request
progressive wire frames; this field controls `results_progressive_enabled` even
when the options map supplies another value. Management rejects that field.

Progressive responses reconstruct tables in header order, allowing interleaved
fragments and buffered auxiliary tables. `DataAppend` adds rows, while
`DataReplace` replaces all current rows of the referenced table. Progress frames
retain the latest percentage in `Table.Progress`; percentages do not establish
completion. Table completion must match the final row count. Unknown table IDs,
duplicate headers, frames after table completion, mismatched fragment widths and
incomplete successful datasets return `ErrProtocol`.

Both modes return only after the complete response has been received and
validated. There is no public streaming iterator, progress callback or early
result delivery yet. Unknown frames or missing dataset completion records also
return `ErrProtocol`.

All result tables are retained in order, including auxiliary tables. v1 table
IDs are their response ordinals; the original service IDs remain in the table of
contents. When present, that table classifies and names the other v1 tables.
`PrimaryTables()` selects result tables. Cells remain `json.RawMessage` so
large longs, decimal strings, dynamic arrays/bags, nulls, dates and durations
keep the service's JSON representation. This is wire preservation, not a
complete native scalar-value or dynamic-type implementation.

Non-200 responses, dataset errors, cancellation and failing completion status
records return an error and no result. HTTP 200 alone does not count as success.
v1 `Severity` values at most 2 and v2 completion `Level` values below 4 fail;
the latter follows the Microsoft Python SDK. Dataset flags and nonempty
`OneApiErrors` also fail. Injected error objects between row arrays fail rather
than becoming data. Failures in progressive status fragments cannot be erased by
subsequent replacement frames. Per-command status rows in a script or an asynchronous
operation's eventual outcome require command-specific inspection; generic REST
success does not prove every operation succeeded.

`errors.As(err, &serviceError)` exposes HTTP status, correlation IDs and bounded
response bytes. `errors.Is` distinguishes `ErrPartial`, `ErrCancelled`,
`ErrProtocol`, `ErrLimit` and wrapped context errors. Error strings and automatic
JSON encoding omit service response details. Inspect `ServiceError.Details`
deliberately; it can contain query text or other sensitive service data.

## Bounds and transport

Endpoints require HTTPS and cannot contain credentials, a path, query or
fragment. Redirects are refused even on the same origin. `AllowInsecureHTTP`
is an explicit development/test option. A supplied HTTP client is copied, with
redirect handling replaced; its transport and timeout remain application-owned.
The default client timeout is 30 seconds.

Serialized request bodies are capped at 8 MiB. Decompressed responses default
to 32 MiB and can be configured up to 128 MiB. Decoding also limits 256 tables,
4,096 columns per table, 16,384 total columns, 100,000 total rows and 1,000,000
total cells, plus 65,536 v2 frames. Replaced rows and cells count toward cumulative
decoding work; replacement does not reset the limits. Limits return errors without returning truncated data. Rows,
columns and object fields are checked while decoding. These are client policies,
not Kusto limits. Caller-owned request maps and HTTP transports must not be
mutated during execution. Caller cancellation is checked during frame, column and
row decoding and before returning success. A malformed response remains bounded
by the byte ceiling while the JSON decoder reads a field or cell.

## Evidence and remaining work

TLS-backed mock-service tests exercise request properties, endpoint selection,
multiple results, long/dynamic precision, progressive interleaving and repeated
append/replace fragments, management scripts, token acquisition,
redirect refusal, bounded bodies, cancellation and service/protocol failures.
Adversarial decoding tests and a fuzz target cover malformed response structures
and allocation limits. No live Azure Data Explorer or Fabric engine has been
used to certify these tests.

Public network-stream iterators/progress callbacks, automatic identity acquisition, queued/streaming ingest
protocols and storage upload, asynchronous operation polling, ARM resource
management, retry policies, per-command result interpretation and live-service
conformance remain open. Striem integration is documented in that repository.

Primary protocol references: [REST requests](https://learn.microsoft.com/en-us/kusto/api/rest/request?view=azure-data-explorer),
[v1 responses](https://learn.microsoft.com/en-us/kusto/api/rest/response?view=azure-data-explorer),
[v2 responses](https://learn.microsoft.com/en-us/kusto/api/rest/response-v2?view=azure-data-explorer),
and [Microsoft Python response handling](https://github.com/Azure/azure-kusto-python/blob/master/azure-kusto-data/azure/kusto/data/response.py).
