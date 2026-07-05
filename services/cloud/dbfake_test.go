package main

// A minimal database/sql/driver implementation serving canned rows, so
// the Phase 5 stream tests can run the real sqlc-generated query layer
// (GetJobForStream on the handler side; UpdateJobStatus + InsertAuditEvent
// on the printer-link hub side) without a running Postgres. This keeps
// the repo's existing no-Postgres-fixture test depth (see
// link/hub_integration_test.go) while still exercising handlers
// end-to-end through database/sql's Scan machinery.
//
// Only what the stream tests touch is implemented: QueryerContext /
// ExecerContext (sqlc never prepares here -- emit_prepared_queries is
// false), no transactions, no named-value checking beyond the default
// converter (every arg type sqlc passes implements driver.Valuer).

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
)

// fakeQueryFunc receives every query the code under test issues and
// returns the result set. Dispatch on the query's leading "-- name: X"
// comment (sqlc embeds it in the SQL constant). Returning an error makes
// the handler surface a 500, which fails the test at the assertion site
// -- fakes must not call testing.T from server goroutines.
type fakeQueryFunc func(query string, args []driver.NamedValue) (cols []string, rows [][]driver.Value, err error)

type fakeConnector struct{ q fakeQueryFunc }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return fakeConn{q: c.q}, nil }
func (c fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("fakeDriver: use sql.OpenDB with fakeConnector")
}

type fakeConn struct{ q fakeQueryFunc }

var (
	_ driver.Conn           = fakeConn{}
	_ driver.QueryerContext = fakeConn{}
	_ driver.ExecerContext  = fakeConn{}
)

func (c fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fakeConn: Prepare not supported")
}
func (c fakeConn) Close() error { return nil }
func (c fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fakeConn: transactions not supported")
}

func (c fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	cols, rows, err := c.q(query, args)
	if err != nil {
		return nil, err
	}
	return &fakeRows{cols: cols, rows: rows}, nil
}

func (c fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if _, _, err := c.q(query, args); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	next int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF // zero rows -> sql.ErrNoRows out of QueryRowContext().Scan()
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}
