package provision

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

type renewalCompletionDriver struct {
	sqlite.Driver
	renewed chan struct{}
}

func (fault *renewalCompletionDriver) Open(name string) (driver.Conn, error) {
	connection, err := fault.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &renewalCompletionConnection{Conn: connection, renewed: fault.renewed}, nil
}

type renewalCompletionConnection struct {
	driver.Conn
	renewed chan struct{}
}

func (connection *renewalCompletionConnection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if !strings.Contains(query, "UPDATE jobs SET locked_until") {
		return connection.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	}
	// A SQLite write can commit immediately before its caller cancels SQL.
	result, err := connection.Conn.(driver.ExecerContext).ExecContext(context.WithoutCancel(ctx), query, args)
	if err != nil {
		return result, err
	}
	close(connection.renewed)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return result, nil
	}
}
func TestJobsCompletionDoesNotCancelInflightRenewal(t *testing.T) {
	renewed := make(chan struct{})
	name := fmt.Sprintf("renewal-completion-%d", jobsTestDatabaseID.Add(1))
	sql.Register(name, &renewalCompletionDriver{renewed: renewed})
	db := jobsTestOpenDatabase(t, name)
	engine := NewEngine(db)
	engine.LeaseDuration = 300 * time.Millisecond
	engine.Register("renewing", func(ctx context.Context, _ Job) error {
		select {
		case <-renewed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	id := jobsTestEnqueue(t, engine, "renewing", "", 1)
	jobsTestRunOnce(t, engine, true)
	if state := jobsTestRead(t, db, id); state.status != "succeeded" {
		t.Fatalf("completion lost its renewed lease: %+v", state)
	}
}
