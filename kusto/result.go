package kusto

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrLimit     = errors.New("kusto: resource limit exceeded")
	ErrProtocol  = errors.New("kusto: invalid or unsupported response protocol")
	ErrPartial   = errors.New("kusto: service reported a partial failure")
	ErrCancelled = errors.New("kusto: service reported cancellation")
)

// ServiceError retains correlation IDs and the bounded service response for
// deliberate inspection. Error never includes response contents, query text,
// credentials or parameter values. Details may contain sensitive service data.
type ServiceError struct {
	StatusCode int
	RequestID  string
	ActivityID string
	Details    []byte `json:"-"`
	cause      error
}

func (e *ServiceError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return fmt.Sprintf("kusto: service request failed (HTTP %d)", e.StatusCode)
}
func (e *ServiceError) Unwrap() error { return e.cause }

type Column struct {
	Name     string `json:"ColumnName"`
	Type     string `json:"ColumnType"`
	DataType string `json:"DataType,omitempty"`
}

// Table preserves Kusto's typed wire representation. Cells are RawMessage rather
// than float64/interface{} so longs, decimal text, dynamic bags, nulls, datetime
// and timespan strings retain their original precision and encoding.
type Table struct {
	ID      int
	Kind    string
	Name    string
	Columns []Column
	Rows    [][]json.RawMessage
}

type Result struct {
	Tables     []Table
	RequestID  string
	ActivityID string
}

// PrimaryTables selects query/command result tables while retaining the full
// auxiliary tables in Result.Tables. For v1, tables other than the documented
// QueryStatus, TableOfContents and @ExtendedProperties are command results.
func (r *Result) PrimaryTables() []Table {
	var result []Table
	for _, table := range r.Tables {
		if table.Kind == "PrimaryResult" {
			result = append(result, table)
		}
	}
	return result
}

func protocolError(message string) error { return fmt.Errorf("%w: %s", ErrProtocol, message) }
