// Package kusto executes native KQL against the Kusto REST service. It is
// independent of the SQL compiler: authorization and service capabilities are
// enforced by the configured service, not simulated by compiler extensions.
package kusto

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultResponseBytes = 32 << 20
	maxResponseBytes     = 128 << 20
	maxRequestBytes      = 8 << 20
)

// TokenProvider obtains a bearer token for the configured cluster. Implementations
// own token caching/refresh and must be safe for concurrent calls. No token is
// cached or logged by Client.
type TokenProvider func(context.Context) (string, error)

type Config struct {
	Endpoint      string
	TokenProvider TokenProvider
	// HTTPClient is copied. Redirects are always refused, including same-origin
	// redirects, so credentials cannot be forwarded to another endpoint.
	HTTPClient *http.Client
	// AllowInsecureHTTP permits HTTP explicitly for local tests/development.
	// Production endpoints should use HTTPS.
	AllowInsecureHTTP bool
	// MaxResponseBytes bounds the decompressed response. Zero selects 32 MiB;
	// values above 128 MiB are rejected. Oversize responses fail, not truncate.
	MaxResponseBytes int64
}

// Client is safe for concurrent use. Configure one client per trusted endpoint.
type Client struct {
	endpoint      string
	token         TokenProvider
	http          http.Client
	responseBytes int64
}

func New(config Config) (*Client, error) {
	u, err := url.Parse(config.Endpoint)
	if err != nil || u == nil || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return nil, errors.New("kusto: endpoint must be an absolute cluster URL without credentials, path, query or fragment")
	}
	if u.Scheme != "https" && !(config.AllowInsecureHTTP && u.Scheme == "http") {
		return nil, errors.New("kusto: endpoint requires HTTPS")
	}
	if config.TokenProvider == nil {
		return nil, errors.New("kusto: a token provider is required")
	}
	limit := config.MaxResponseBytes
	if limit == 0 {
		limit = defaultResponseBytes
	}
	if limit < 1 || limit > maxResponseBytes {
		return nil, errors.New("kusto: response limit must be between 1 byte and 128 MiB")
	}
	h := http.Client{Timeout: 30 * time.Second}
	if config.HTTPClient != nil {
		h = *config.HTTPClient
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	u.Path = ""
	return &Client{endpoint: u.String(), token: config.TokenProvider, http: h, responseBytes: limit}, nil
}

// Request sends native KQL verbatim. Query parameters must also be declared in
// the KQL text with declare query_parameters. Options and parameters are encoded
// as request properties, never interpolated into KQL.
type Request struct {
	Database        string
	Text            string
	Options         map[string]any
	Parameters      map[string]string
	ClientRequestID string
	// Progressive requests progressive v2 frames. Client reconstructs their
	// final tables before returning; it does not expose intermediate results.
	Progressive bool
}

// Query uses the v2 query endpoint, with buffered or progressive wire results.
// Client adds no application-level retries, including after a transport error
// with an uncertain execution outcome.
func (c *Client) Query(ctx context.Context, request Request) (*Result, error) {
	return c.execute(ctx, request, false)
}

// Management uses the v1 management endpoint. Commands, including mutations,
// ingestion/export commands and scripts, are sent to the service unchanged.
// Permission, grammar and external-service availability remain service concerns.
// Callers must authorize the submitted command before invoking this method.
func (c *Client) Management(ctx context.Context, request Request) (*Result, error) {
	return c.execute(ctx, request, true)
}

func (c *Client) execute(ctx context.Context, request Request, management bool) (*Result, error) {
	if ctx == nil {
		return nil, errors.New("kusto: context is required")
	}
	if management && request.Progressive {
		return nil, errors.New("kusto: progressive results require the query endpoint")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Text) == "" || (!management && strings.TrimSpace(request.Database) == "") {
		return nil, errors.New("kusto: text and query database are required")
	}
	if !utf8.ValidString(request.Text) || !utf8.ValidString(request.Database) {
		return nil, errors.New("kusto: text and database must be UTF-8")
	}
	for name, value := range request.Parameters {
		if !utf8.ValidString(name) || !utf8.ValidString(value) {
			return nil, errors.New("kusto: query parameters must be UTF-8")
		}
	}
	if len(request.Text)+len(request.Database) > maxRequestBytes {
		return nil, ErrLimit
	}
	id := request.ClientRequestID
	if id == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, fmt.Errorf("kusto: request ID: %w", err)
		}
		id = "KSQL;" + hex.EncodeToString(random[:])
	}
	if len(id) > 512 || !validHeaderValue(id) {
		return nil, errors.New("kusto: invalid client request ID")
	}
	options := make(map[string]any, len(request.Options)+1)
	for name, value := range request.Options {
		options[name] = value
	}
	if !management {
		options["results_progressive_enabled"] = request.Progressive
	}
	properties, err := json.Marshal(struct {
		Options         map[string]any    `json:"Options,omitempty"`
		Parameters      map[string]string `json:"Parameters,omitempty"`
		ClientRequestID string            `json:"ClientRequestId"`
	}{options, request.Parameters, id})
	if err != nil {
		return nil, errors.New("kusto: request properties are not JSON encodable")
	}
	if len(properties) > maxRequestBytes {
		return nil, ErrLimit
	}
	body, err := json.Marshal(struct {
		Database   string `json:"db,omitempty"`
		Text       string `json:"csl"`
		Properties string `json:"properties"`
	}{request.Database, request.Text, string(properties)})
	if err != nil {
		return nil, err
	}
	if len(body) > maxRequestBytes {
		return nil, ErrLimit
	}
	path := "/v2/rest/query"
	if management {
		path = "/v1/rest/mgmt"
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, &operationError{message: "kusto: token acquisition failed", cause: err}
	}
	if len(token) == 0 || len(token) > 64<<10 || !validHeaderValue(token) || strings.Contains(token, " ") {
		return nil, errors.New("kusto: invalid bearer token")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpRequest.Header.Set("x-ms-client-request-id", id)
	response, err := c.http.Do(httpRequest)
	if err != nil {
		return nil, &operationError{message: "kusto: HTTP request failed", cause: err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, c.responseBytes+1))
	if err != nil {
		return nil, &operationError{message: "kusto: response read failed", cause: err}
	}
	if int64(len(data)) > c.responseBytes {
		return nil, ErrLimit
	}
	requestID := response.Header.Get("x-ms-client-request-id")
	if requestID == "" {
		requestID = id
	}
	activityID := response.Header.Get("x-ms-activity-id")
	if response.StatusCode != http.StatusOK {
		return nil, &ServiceError{StatusCode: response.StatusCode, RequestID: requestID, ActivityID: activityID, Details: data}
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
			return nil, &ServiceError{StatusCode: response.StatusCode, RequestID: requestID, ActivityID: activityID, Details: data, cause: protocolError("expected a JSON response")}
		}
	}
	result, err := decodeResponseContext(ctx, data, management)
	if err != nil {
		return nil, &ServiceError{StatusCode: response.StatusCode, RequestID: requestID, ActivityID: activityID, Details: data, cause: err}
	}
	result.RequestID, result.ActivityID = requestID, activityID
	return result, nil
}

func validHeaderValue(s string) bool {
	for i := range s {
		if s[i] < 32 || s[i] > 126 {
			return false
		}
	}
	return true
}

type operationError struct {
	message string
	cause   error
}

func (e *operationError) Error() string { return e.message }
func (e *operationError) Unwrap() error { return e.cause }
