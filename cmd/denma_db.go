package main

// denma: database connections that work through PgBouncer in transaction
// mode, so that hundreds of centers share a few Postgres connections. Each
// center keeps its own pool, with its schema as the search path (a startup
// parameter, which PgBouncer must track: track_extra_parameters =
// search_path); PgBouncer gives each transaction any free server connection.
//
// So every statement must be parsed on the server connection that runs it,
// with the center's search path, in one round trip:
//
//   - Nothing is prepared on the server. listmonk prepares its queries once
//     per pool; here a prepared statement only keeps its text, and each run
//     sends it with its arguments. (Server-side prepared statements, shared
//     through PgBouncer's max_prepared_statements, are parsed with the search
//     path of whichever center prepared them first: argument types are then
//     another center's, and its tables are only found again by re-parsing.)
//   - lib/pq sends a query with arguments in two round trips (parse, then
//     execute), which PgBouncer may run on different server connections, so
//     connections use lib/pq's binary_parameters, which sends both at once.
//     That sends []byte arguments in binary, which Postgres rejects for text
//     and JSON (listmonk passes JSON as []byte), so they're sent as text, as
//     lib/pq otherwise does; listmonk has no bytea columns.
//
// Multi-center mode only, with or without PgBouncer.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
	"github.com/lib/pq"
)

const denmaDriver = "denma-postgres"

func init() {
	sql.Register(denmaDriver, denmaPQDriver{})
	sqlx.BindDriver(denmaDriver, sqlx.DOLLAR)
}

// denmaDB returns the driver and DSN to connect to the database with.
func denmaDB(dsn string, ko *koanf.Koanf) (string, string) {
	if !ko.Bool("denma.multi_center") {
		return "postgres", dsn
	}
	return denmaDriver, dsn + " binary_parameters=yes"
}

// denmaPQDriver is lib/pq's driver, with []byte arguments sent as text.
type denmaPQDriver struct{}

func (denmaPQDriver) Open(name string) (driver.Conn, error) {
	c, err := pq.Driver{}.Open(name)
	if err != nil {
		return nil, err
	}
	return denmaPQConn{c.(pqConn)}, nil
}

// pqConn is what lib/pq's connections implement.
type pqConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

type denmaPQConn struct {
	pqConn
}

// CheckNamedValue converts an argument as database/sql otherwise would, and
// a []byte (JSON, text) to a string.
func (denmaPQConn) CheckNamedValue(nv *driver.NamedValue) error {
	v, err := driver.DefaultParameterConverter.ConvertValue(nv.Value)
	if err != nil {
		return err
	}
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	nv.Value = v
	return nil
}

// PrepareContext keeps the query, to run with its arguments each time.
func (c denmaPQConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return denmaStmt{conn: c, query: query}, nil
}

func (c denmaPQConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// denmaStmt is a "prepared" statement: its query, run in one round trip.
type denmaStmt struct {
	conn  denmaPQConn
	query string
}

func (s denmaStmt) Close() error  { return nil }
func (s denmaStmt) NumInput() int { return -1 } // unknown: Postgres checks

func (s denmaStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

func (s denmaStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

// Exec and Query aren't used: database/sql calls the Context versions.
var errDenmaStmt = errors.New("denma: use ExecContext or QueryContext")

func (s denmaStmt) Exec([]driver.Value) (driver.Result, error) { return nil, errDenmaStmt }
func (s denmaStmt) Query([]driver.Value) (driver.Rows, error)  { return nil, errDenmaStmt }
