package main

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

type pruneResult int64

func (pruneResult) LastInsertId() (int64, error)   { return 0, nil }
func (r pruneResult) RowsAffected() (int64, error) { return int64(r), nil }

// hungDB never answers until its context ends; okDB deletes 2 of each.
type hungDB struct{}

func (hungDB) ExecContext(ctx context.Context, _ string, _ ...any) (sql.Result, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type okDB struct{}

func (okDB) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return pruneResult(2), nil
}

// A center whose database hangs is given up on after the timeout, and the
// next is pruned.
func TestDenmaPruneTimeout(t *testing.T) {
	var logged int
	logf := func(string, ...any) { logged++ }

	start := time.Now()
	s, m := denmaPruneDB(hungDB{}, true, 50*time.Millisecond, logf)
	if time.Since(start) > 5*time.Second || s != 0 || m != 0 || logged != 1 {
		t.Errorf("a hung database: %d, %d deleted, %d logged, took %s", s, m, logged, time.Since(start))
	}
	if s, m = denmaPruneDB(okDB{}, true, time.Second, logf); s != 2 || m != 2 {
		t.Errorf("a center: %d sessions, %d messages deleted, want 2, 2", s, m)
	}
	if s, m = denmaPruneDB(okDB{}, false, time.Second, logf); s != 2 || m != 0 {
		t.Errorf("the hub: %d sessions, %d messages deleted, want 2, 0", s, m)
	}
}
