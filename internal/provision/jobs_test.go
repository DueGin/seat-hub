package provision

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DueGin/vm2api-portal/internal/store/migrations"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var jobsTestDatabaseID atomic.Int64

func jobsTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	return jobsTestOpenDatabase(t, "sqlite")
}

func jobsTestOpenDatabase(t *testing.T, driverName string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, fmt.Sprintf("file:jobs-test-%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)", jobsTestDatabaseID.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	t.Cleanup(func() { db.Close() })
	schema, err := migrations.FS.ReadFile("001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), string(schema)); err != nil {
		t.Fatal(err)
	}
	return db
}

type jobsTestSQLCode int

func (code jobsTestSQLCode) Error() string { return fmt.Sprintf("sqlite error %d", code) }
func (code jobsTestSQLCode) Code() int     { return int(code) }

type jobsTestFaultDriver struct {
	sqlite.Driver
	fail func(string) error
}

func (fault *jobsTestFaultDriver) Open(name string) (driver.Conn, error) {
	connection, err := fault.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &jobsTestFaultConnection{Conn: connection, fail: fault.fail}, nil
}

type jobsTestFaultConnection struct {
	driver.Conn
	fail func(string) error
}

func (connection *jobsTestFaultConnection) ExecContext(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Result, error) {
	if err := connection.fail(query); err != nil {
		return nil, err
	}
	return connection.Conn.(driver.ExecerContext).ExecContext(ctx, query, arguments)
}

func jobsTestFaultDatabase(t *testing.T, fail func(string) error) *sql.DB {
	t.Helper()
	driverName := fmt.Sprintf("jobs-fault-%d", jobsTestDatabaseID.Add(1))
	sql.Register(driverName, &jobsTestFaultDriver{fail: fail})
	return jobsTestOpenDatabase(t, driverName)
}

type jobsTestState struct {
	status    string
	attempts  int
	runAfter  time.Time
	lease     sql.NullString
	lastError sql.NullString
}

func jobsTestRead(t *testing.T, db *sql.DB, jobID int64) jobsTestState {
	t.Helper()
	var state jobsTestState
	var runAfter, createdAt, updatedAt string
	err := db.QueryRow(`SELECT status, attempts, run_after, locked_until, last_error, created_at, updated_at
		FROM jobs WHERE id = ?`, jobID).Scan(&state.status, &state.attempts, &runAfter,
		&state.lease, &state.lastError, &createdAt, &updatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []string{runAfter, createdAt, updatedAt} {
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil || !strings.HasSuffix(stamp, "Z") {
			t.Fatalf("timestamp = %q, parse error = %v; want RFC3339 UTC", stamp, err)
		}
	}
	state.runAfter, _ = time.Parse(time.RFC3339Nano, runAfter)
	return state
}

func jobsTestEnqueue(t *testing.T, engine *Engine, jobType, key string, maxAttempts int) int64 {
	t.Helper()
	jobID, err := engine.Enqueue(context.Background(), jobType, map[string]int{"resource_id": 42}, key, time.Time{}, maxAttempts)
	if err != nil {
		t.Fatal(err)
	}
	return jobID
}

func jobsTestRunOnce(t *testing.T, engine *Engine, wantWork bool) {
	t.Helper()
	worked, err := engine.RunOnce(context.Background())
	if err != nil || worked != wantWork {
		t.Fatalf("RunOnce = %v, %v; want %v, nil", worked, err, wantWork)
	}
}

func TestJobsEnqueueAndSuccess(t *testing.T) {
	db := jobsTestDatabase(t)
	engine := NewEngine(db)
	if engine.Workers != 4 || engine.PollInterval <= 0 || engine.LeaseDuration <= 0 || engine.Now == nil {
		t.Fatalf("unexpected defaults: %+v", engine)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 500000000, time.FixedZone("local", 8*3600))
	engine.Now = func() time.Time { return now }
	due := now.Add(250 * time.Millisecond)
	jobID, err := engine.Enqueue(context.Background(), "ready", json.RawMessage(`{"resource_id":42}`), "resource:42", due, 0)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := engine.Enqueue(context.Background(), "other", nil, "resource:42", now, 2)
	if err != nil || duplicate != jobID {
		t.Fatalf("duplicate = %d, %v; want %d, nil", duplicate, err, jobID)
	}
	engine.Register("ready", func(_ context.Context, job Job) error {
		if job.ID != jobID || job.Type != "ready" || string(job.Payload) != `{"resource_id":42}` ||
			job.DedupeKey != "resource:42" || job.Attempts != 1 || job.MaxAttempts != 8 ||
			!job.RunAfter.Equal(due) || job.RunAfter.Location() != time.UTC {
			t.Errorf("unexpected job: %+v", job)
		}
		return nil
	})
	jobsTestRunOnce(t, engine, false)
	now = due
	jobsTestRunOnce(t, engine, true)
	state := jobsTestRead(t, db, jobID)
	if state.status != "succeeded" || state.attempts != 1 || state.lease.Valid || state.lastError.Valid {
		t.Fatalf("completed job = %+v", state)
	}
	jobsTestRunOnce(t, engine, false)
	if replacement := jobsTestEnqueue(t, engine, "ready", "resource:42", 2); replacement == jobID {
		t.Fatal("terminal job still reserves dedupe key")
	}
	first := jobsTestEnqueue(t, engine, "ready", "", 2)
	second := jobsTestEnqueue(t, engine, "ready", "", 2)
	if first == second {
		t.Fatal("empty dedupe key merged independent jobs")
	}
}

func TestJobsConcurrentDedupeAndClaim(t *testing.T) {
	db := jobsTestDatabase(t)
	const callers = 16
	identifiers := make(chan int64, callers)
	failures := make(chan error, callers)
	var group sync.WaitGroup
	for caller := 0; caller < callers; caller++ {
		group.Go(func() {
			engine := NewEngine(db)
			jobID, err := engine.Enqueue(context.Background(), "shared", nil, "same", time.Time{}, 2)
			identifiers <- jobID
			failures <- err
		})
	}
	group.Wait()
	close(identifiers)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var expected int64
	for jobID := range identifiers {
		if jobID <= 0 || (expected != 0 && jobID != expected) {
			t.Fatalf("dedupe ids = %d and %d", expected, jobID)
		}
		expected = jobID
	}
	for caller := 1; caller < callers; caller++ {
		jobsTestEnqueue(t, NewEngine(db), "shared", "", 2)
	}
	var calls atomic.Int64
	var seen sync.Map
	for caller := 0; caller < callers; caller++ {
		group.Go(func() {
			engine := NewEngine(db)
			engine.Register("shared", func(_ context.Context, job Job) error {
				if _, loaded := seen.LoadOrStore(job.ID, true); loaded {
					t.Errorf("job %d claimed twice", job.ID)
				}
				calls.Add(1)
				return nil
			})
			if worked, err := engine.RunOnce(context.Background()); err != nil || !worked {
				t.Errorf("concurrent RunOnce = %v, %v", worked, err)
			}
		})
	}
	group.Wait()
	if calls.Load() != callers {
		t.Fatalf("handler calls = %d; want %d", calls.Load(), callers)
	}
	var succeeded int
	if err := db.QueryRow(`SELECT count(*) FROM jobs WHERE status = 'succeeded'`).Scan(&succeeded); err != nil || succeeded != callers {
		t.Fatalf("succeeded = %d, %v; want %d", succeeded, err, callers)
	}
}

func TestJobsBackoffAndExhaustion(t *testing.T) {
	db := jobsTestDatabase(t)
	engine := NewEngine(db)
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	engine.Now = func() time.Time { return now }
	engine.Register("failing", func(context.Context, Job) error { return errors.New("upstream unavailable") })
	jobID := jobsTestEnqueue(t, engine, "failing", "retry", 3)
	for attempt, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		jobsTestRunOnce(t, engine, true)
		state := jobsTestRead(t, db, jobID)
		wantStatus := "pending"
		if attempt == 2 {
			wantStatus = "failed"
		}
		if state.status != wantStatus || state.attempts != attempt+1 || !state.runAfter.Equal(now.Add(delay)) ||
			state.lastError.String != "upstream unavailable" {
			t.Fatalf("attempt %d state = %+v", attempt+1, state)
		}
		jobsTestRunOnce(t, engine, false)
		now = now.Add(delay)
	}
	jobsTestRunOnce(t, engine, false)
	if replacement := jobsTestEnqueue(t, engine, "failing", "retry", 1); replacement == jobID {
		t.Fatal("failed job still reserves dedupe key")
	}
	if jobsBackoff(100000) != 5*time.Minute {
		t.Fatal("exponential backoff is not capped")
	}
}

func TestJobsRetryAfterDoesNotConsumeAttempts(t *testing.T) {
	for _, pointer := range []bool{false, true} {
		t.Run(fmt.Sprintf("pointer=%v", pointer), func(t *testing.T) {
			db := jobsTestDatabase(t)
			engine := NewEngine(db)
			now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
			engine.Now = func() time.Time { return now }
			cause := errors.New("bootstrap pending")
			engine.Register("poll", func(_ context.Context, job Job) error {
				if job.Attempts != 1 || job.MaxAttempts != 1 {
					t.Errorf("poll attempt = %d/%d", job.Attempts, job.MaxAttempts)
				}
				retry := RetryAfterError{After: 1500 * time.Millisecond, Err: cause}
				var result error = retry
				if pointer {
					result = &retry
				}
				if !errors.Is(result, cause) {
					t.Error("RetryAfterError does not unwrap")
				}
				return fmt.Errorf("bootstrap: %w", result)
			})
			jobID := jobsTestEnqueue(t, engine, "poll", "bootstrap", 1)
			for poll := 0; poll < 4; poll++ {
				jobsTestRunOnce(t, engine, true)
				state := jobsTestRead(t, db, jobID)
				if state.status != "pending" || state.attempts != 0 || !state.runAfter.Equal(now.Add(1500*time.Millisecond)) ||
					!strings.Contains(state.lastError.String, cause.Error()) {
					t.Fatalf("poll state = %+v", state)
				}
				jobsTestRunOnce(t, engine, false)
				now = state.runAfter
			}
			engine.Register("poll", func(context.Context, Job) error { return nil })
			jobsTestRunOnce(t, engine, true)
			state := jobsTestRead(t, db, jobID)
			if state.status != "succeeded" || state.attempts != 1 || state.lastError.Valid {
				t.Fatalf("poll completion = %+v", state)
			}
		})
	}
	if got := (RetryAfterError{After: time.Second}).Error(); got != "retry after 1s" {
		t.Fatalf("RetryAfterError.Error = %q", got)
	}
}

func TestJobsExpiredLeaseAndCompletionFencing(t *testing.T) {
	for _, staleError := range []error{nil, errors.New("stale failure"), RetryAfterError{After: time.Second}} {
		t.Run(fmt.Sprintf("stale=%v", staleError), func(t *testing.T) {
			db := jobsTestDatabase(t)
			engine := NewEngine(db)
			now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
			engine.Now = func() time.Time { return now }
			engine.LeaseDuration = 10 * time.Second
			jobID := jobsTestEnqueue(t, engine, "lease", "lease", 3)
			old, err := engine.claim(context.Background(), now)
			if err != nil || old == nil {
				t.Fatalf("claim = %v, %v", old, err)
			}
			jobsTestRunOnce(t, engine, false)
			duplicate := jobsTestEnqueue(t, engine, "lease", "lease", 3)
			if duplicate != jobID {
				t.Fatal("running job does not retain dedupe key")
			}
			now = now.Add(engine.LeaseDuration)
			if err := engine.complete(context.Background(), *old, staleError); err != nil {
				t.Fatal(err)
			}
			if state := jobsTestRead(t, db, jobID); state.status != "running" {
				t.Fatalf("expired completion changed job: %+v", state)
			}
			current, err := engine.claim(context.Background(), now)
			if err != nil || current == nil || current.Attempts != 2 {
				t.Fatalf("reclaimed job = %v, %v", current, err)
			}
			if err := engine.complete(context.Background(), *old, staleError); err != nil {
				t.Fatal(err)
			}
			state := jobsTestRead(t, db, jobID)
			if state.status != "running" || state.attempts != 2 || state.lease.String != current.lockedUntil {
				t.Fatalf("stale completion changed current lease: %+v", state)
			}
			wrongLease := *current
			wrongLease.lockedUntil = old.lockedUntil
			if err := engine.complete(context.Background(), wrongLease, nil); err != nil {
				t.Fatal(err)
			}
			wrongAttempt := *current
			wrongAttempt.Attempts--
			if err := engine.complete(context.Background(), wrongAttempt, nil); err != nil {
				t.Fatal(err)
			}
			if state := jobsTestRead(t, db, jobID); state.status != "running" {
				t.Fatalf("completion did not fence lease and attempt: %+v", state)
			}
			if err := engine.complete(context.Background(), *current, nil); err != nil {
				t.Fatal(err)
			}
			if state := jobsTestRead(t, db, jobID); state.status != "succeeded" || state.attempts != 2 {
				t.Fatalf("current completion = %+v", state)
			}
		})
	}
}

func TestJobsExpiredFinalAttempt(t *testing.T) {
	db := jobsTestDatabase(t)
	engine := NewEngine(db)
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	engine.Now = func() time.Time { return now }
	jobID := jobsTestEnqueue(t, engine, "crashed", "crashed", 1)
	lease, err := engine.claim(context.Background(), now)
	if err != nil || lease == nil {
		t.Fatalf("claim = %v, %v", lease, err)
	}
	now = now.Add(engine.LeaseDuration)
	jobsTestRunOnce(t, engine, true)
	state := jobsTestRead(t, db, jobID)
	if state.status != "failed" || state.attempts != 1 || state.lease.Valid || !strings.Contains(state.lastError.String, "lease expired") {
		t.Fatalf("expired final attempt = %+v", state)
	}
	jobsTestRunOnce(t, engine, false)
}

func TestJobsRefundedAttemptFencing(t *testing.T) {
	db := jobsTestDatabase(t)
	engine := NewEngine(db)
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	engine.Now = func() time.Time { return now }
	jobID := jobsTestEnqueue(t, engine, "poll", "", 1)
	old, err := engine.claim(context.Background(), now)
	if err != nil || old == nil {
		t.Fatalf("claim = %v, %v", old, err)
	}
	if err := engine.complete(context.Background(), *old, RetryAfterError{}); err != nil {
		t.Fatal(err)
	}
	current, err := engine.claim(context.Background(), now)
	if err != nil || current == nil || current.Attempts != old.Attempts || current.lockedUntil == old.lockedUntil {
		t.Fatalf("same-time reclaim = %+v, %v; old = %+v", current, err, old)
	}
	if err := engine.complete(context.Background(), *old, nil); err != nil {
		t.Fatal(err)
	}
	if state := jobsTestRead(t, db, jobID); state.status != "running" || state.lease.String != current.lockedUntil {
		t.Fatalf("refunded stale attempt changed new lease: %+v", state)
	}
	if err := engine.complete(context.Background(), *current, nil); err != nil {
		t.Fatal(err)
	}
	if state := jobsTestRead(t, db, jobID); state.status != "succeeded" || state.attempts != 1 {
		t.Fatalf("refunded current completion = %+v", state)
	}
}

func TestJobsMixedTimestampPrecision(t *testing.T) {
	db := jobsTestDatabase(t)
	engine := NewEngine(db)
	now := time.Date(2026, 10, 31, 0, 0, 0, 500000000, time.UTC)
	engine.Now = func() time.Time { return now }
	engine.Register("precision", func(context.Context, Job) error { return nil })
	for _, stamp := range []string{"2026-10-31T00:00:00Z", "2026-10-31T00:00:00.5Z"} {
		jobID := jobsTestEnqueue(t, engine, "precision", "", 2)
		if _, err := db.Exec(`UPDATE jobs SET run_after = ? WHERE id = ?`, stamp, jobID); err != nil {
			t.Fatal(err)
		}
		jobsTestRunOnce(t, engine, true)
		if state := jobsTestRead(t, db, jobID); state.status != "succeeded" {
			t.Fatalf("mixed precision job = %+v", state)
		}
	}
	jobID := jobsTestEnqueue(t, engine, "precision", "", 2)
	if _, err := db.Exec(`UPDATE jobs SET status = 'running', attempts = 1, locked_until = ? WHERE id = ?`,
		"2026-10-31T00:00:00.5Z", jobID); err != nil {
		t.Fatal(err)
	}
	jobsTestRunOnce(t, engine, true)
	if state := jobsTestRead(t, db, jobID); state.status != "succeeded" || state.attempts != 2 {
		t.Fatalf("mixed precision expired lease = %+v", state)
	}
}

func TestJobsRetryAfterPreservesDelay(t *testing.T) {
	db := jobsTestDatabase(t)
	engine := NewEngine(db)
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	engine.Now = func() time.Time { return now }
	engine.Register("poll", func(context.Context, Job) error {
		return RetryAfterError{After: 5 * time.Second, Err: context.DeadlineExceeded}
	})
	jobID := jobsTestEnqueue(t, engine, "poll", "", 1)
	jobsTestRunOnce(t, engine, true)
	state := jobsTestRead(t, db, jobID)
	if state.status != "pending" || state.attempts != 0 || !state.runAfter.Equal(now.Add(5*time.Second)) {
		t.Fatalf("retry delay with context error = %+v", state)
	}
}

func TestJobsPanicAndMissingHandler(t *testing.T) {
	for _, handler := range []Handler{nil, func(context.Context, Job) error { panic("broken handler") }} {
		t.Run(fmt.Sprintf("handler=%v", handler != nil), func(t *testing.T) {
			db := jobsTestDatabase(t)
			engine := NewEngine(db)
			if handler != nil {
				engine.Register("broken", handler)
			}
			jobID := jobsTestEnqueue(t, engine, "broken", "", 1)
			jobsTestRunOnce(t, engine, true)
			state := jobsTestRead(t, db, jobID)
			if state.status != "failed" || state.attempts != 1 || !state.lastError.Valid {
				t.Fatalf("handler failure = %+v", state)
			}
			if handler != nil && !strings.Contains(state.lastError.String, "handler panic: broken handler") {
				t.Fatalf("panic error = %q", state.lastError.String)
			}
		})
	}
}

func TestJobsCancellationRetries(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			db := jobsTestDatabase(t)
			engine := NewEngine(db)
			engine.Register("cancel", func(context.Context, Job) error { return fmt.Errorf("stopped: %w", cause) })
			jobID := jobsTestEnqueue(t, engine, "cancel", "", 1)
			jobsTestRunOnce(t, engine, true)
			if state := jobsTestRead(t, db, jobID); state.status != "pending" || state.attempts != 0 {
				t.Fatalf("canceled job = %+v", state)
			}
			ctx, cancel := context.WithCancel(context.Background())
			engine.Register("cancel", func(context.Context, Job) error { cancel(); return nil })
			worked, err := engine.RunOnce(ctx)
			if !worked || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled RunOnce = %v, %v", worked, err)
			}
			if state := jobsTestRead(t, db, jobID); state.status != "pending" || state.attempts != 0 {
				t.Fatalf("cancellation cleanup = %+v", state)
			}
			engine.Register("cancel", func(context.Context, Job) error { return nil })
			jobsTestRunOnce(t, engine, true)
			if state := jobsTestRead(t, db, jobID); state.status != "succeeded" {
				t.Fatalf("retry after cancellation = %+v", state)
			}
		})
	}
}

func TestJobsWorkerPool(t *testing.T) {
	for _, workers := range []int{0, 2} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			db := jobsTestDatabase(t)
			engine := NewEngine(db)
			engine.Workers = workers
			engine.PollInterval = time.Millisecond
			wantWorkers := workers
			if wantWorkers == 0 {
				wantWorkers = 4
			}
			started := make(chan struct{}, wantWorkers+1)
			engine.Register("pool", func(ctx context.Context, _ Job) error {
				started <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			})
			for job := 0; job <= wantWorkers; job++ {
				jobsTestEnqueue(t, engine, "pool", "", 1)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() { finished <- engine.Run(ctx) }()
			for worker := 0; worker < wantWorkers; worker++ {
				select {
				case <-started:
				case err := <-finished:
					t.Fatalf("worker pool stopped early: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("worker pool did not fill")
				}
			}
			select {
			case <-started:
				t.Fatal("worker pool exceeded configured concurrency")
			default:
			}
			cancel()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Run error = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("worker pool did not stop")
			}
			var pending, attempts int
			if err := db.QueryRow(`SELECT count(*), sum(attempts) FROM jobs WHERE status = 'pending'`).Scan(&pending, &attempts); err != nil || pending != wantWorkers+1 || attempts != 0 {
				t.Fatalf("shutdown pending=%d attempts=%d error=%v", pending, attempts, err)
			}
		})
	}
}

func TestJobsBusyDoesNotStopWorkerPool(t *testing.T) {
	for _, code := range []int{sqlite3.SQLITE_BUSY, sqlite3.SQLITE_BUSY | 2<<8, sqlite3.SQLITE_LOCKED} {
		t.Run(fmt.Sprintf("code=%d", code), func(t *testing.T) {
			var faults atomic.Int64
			db := jobsTestFaultDatabase(t, func(query string) error {
				if strings.Contains(query, "maximum attempts exhausted") && faults.Add(1) <= 3 {
					return fmt.Errorf("contention: %w", jobsTestSQLCode(code))
				}
				return nil
			})
			engine := NewEngine(db)
			engine.Workers = 1
			engine.PollInterval = time.Millisecond
			jobID := jobsTestEnqueue(t, engine, "busy", "", 1)
			executed := make(chan struct{}, 1)
			engine.Register("busy", func(context.Context, Job) error { executed <- struct{}{}; return nil })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() { finished <- engine.Run(ctx) }()
			select {
			case <-executed:
			case err := <-finished:
				t.Fatalf("transient busy stopped pool: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("worker pool did not recover from busy")
			}
			jobsTestAwait(t, func() bool { return jobsTestRead(t, db, jobID).status == "succeeded" })
			cancel()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Run = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("worker pool did not stop")
			}
			if faults.Load() < 4 {
				t.Fatalf("SQL attempts = %d; want at least 4", faults.Load())
			}
		})
	}
}

func jobsTestAwait(t *testing.T, condition func() bool) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for !condition() {
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatal("condition did not become true")
		}
	}
}

func TestJobsCompletionRetriesBusy(t *testing.T) {
	var faults atomic.Int64
	db := jobsTestFaultDatabase(t, func(query string) error {
		if strings.Contains(query, "UPDATE jobs SET status = ?") && faults.Add(1) <= 2 {
			return jobsTestSQLCode(sqlite3.SQLITE_BUSY)
		}
		return nil
	})
	engine := NewEngine(db)
	var calls int
	engine.Register("complete", func(context.Context, Job) error { calls++; return nil })
	jobID := jobsTestEnqueue(t, engine, "complete", "", 1)
	jobsTestRunOnce(t, engine, true)
	if state := jobsTestRead(t, db, jobID); state.status != "succeeded" || calls != 1 || faults.Load() != 3 {
		t.Fatalf("completion = %+v, calls=%d SQL attempts=%d", state, calls, faults.Load())
	}
}

func TestJobsRenewsLeaseWhileHandlerWaits(t *testing.T) {
	var renewals atomic.Int64
	db := jobsTestFaultDatabase(t, func(query string) error {
		if strings.Contains(query, "UPDATE jobs SET locked_until") && renewals.Add(1) <= 2 {
			return jobsTestSQLCode(sqlite3.SQLITE_BUSY)
		}
		return nil
	})
	engine := NewEngine(db)
	engine.LeaseDuration = 300 * time.Millisecond
	jobID := jobsTestEnqueue(t, engine, "waiting", "", 2)
	var serviceLock sync.Mutex
	serviceLock.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(serviceLock.Unlock) }
	defer release()
	started := make(chan struct{}, 1)
	engine.Register("waiting", func(ctx context.Context, _ Job) error {
		started <- struct{}{}
		serviceLock.Lock()
		defer serviceLock.Unlock()
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := engine.RunOnce(ctx); finished <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	initial := jobsTestRead(t, db, jobID)
	initialEnd, err := time.Parse(time.RFC3339Nano, initial.lease.String)
	if err != nil {
		t.Fatal(err)
	}
	jobsTestAwait(t, func() bool {
		state := jobsTestRead(t, db, jobID)
		return time.Now().After(initialEnd.Add(engine.LeaseDuration)) && state.lease.String != initial.lease.String
	})
	contender := NewEngine(db)
	jobsTestRunOnce(t, contender, false)
	state := jobsTestRead(t, db, jobID)
	if state.status != "running" || state.attempts != 1 || renewals.Load() < 3 {
		t.Fatalf("waiting handler lease = %+v, renewals=%d", state, renewals.Load())
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not finish")
	}
	if state := jobsTestRead(t, db, jobID); state.status != "succeeded" || state.attempts != 1 {
		t.Fatalf("renewed completion = %+v", state)
	}
}

func TestJobsLeaseLossCancelsHandlerAndFencesCompletion(t *testing.T) {
	db := jobsTestDatabase(t)
	engine := NewEngine(db)
	engine.LeaseDuration = 300 * time.Millisecond
	jobID := jobsTestEnqueue(t, engine, "lost", "", 3)
	started := make(chan struct{}, 1)
	stopped := make(chan error, 1)
	engine.Register("lost", func(ctx context.Context, _ Job) error {
		started <- struct{}{}
		<-ctx.Done()
		stopped <- context.Cause(ctx)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := engine.RunOnce(ctx); finished <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	if _, err := db.Exec(`UPDATE jobs SET locked_until = ? WHERE id = ?`, jobsTime(time.Now().Add(-time.Second)), jobID); err != nil {
		t.Fatal(err)
	}
	contender := NewEngine(db)
	current, err := contender.claim(context.Background(), time.Now().UTC())
	if err != nil || current == nil || current.Attempts != 2 {
		t.Fatalf("expiry reclaim = %+v, %v", current, err)
	}
	select {
	case cause := <-stopped:
		if !errors.Is(cause, errJobsLeaseLost) {
			t.Fatalf("handler cancellation cause = %v", cause)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("lease loss did not cancel handler")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stale handler did not finish")
	}
	state := jobsTestRead(t, db, jobID)
	if state.status != "running" || state.attempts != 2 || state.lease.String != current.lockedUntil {
		t.Fatalf("stale renewal/completion changed current lease: %+v", state)
	}
	if err := contender.complete(context.Background(), *current, nil); err != nil {
		t.Fatal(err)
	}
}

func TestJobsLeaseExpiryBoundsBusyRenewal(t *testing.T) {
	var renewals atomic.Int64
	db := jobsTestFaultDatabase(t, func(query string) error {
		if strings.Contains(query, "UPDATE jobs SET locked_until") {
			renewals.Add(1)
			return jobsTestSQLCode(sqlite3.SQLITE_BUSY)
		}
		return nil
	})
	engine := NewEngine(db)
	engine.LeaseDuration = 150 * time.Millisecond
	jobID := jobsTestEnqueue(t, engine, "blocked", "", 2)
	engine.Register("blocked", func(ctx context.Context, _ Job) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	worked, err := engine.RunOnce(ctx)
	if !worked || err != nil || ctx.Err() != nil || renewals.Load() < 1 {
		t.Fatalf("busy renewal = %v, %v, outer=%v, renewals=%d", worked, err, ctx.Err(), renewals.Load())
	}
	contender := NewEngine(db)
	contender.Register("blocked", func(context.Context, Job) error { return nil })
	jobsTestRunOnce(t, contender, true)
	if state := jobsTestRead(t, db, jobID); state.status != "succeeded" || state.attempts != 2 {
		t.Fatalf("reclaim after renewal timeout = %+v", state)
	}
}

func TestJobsValidationAndDatabaseErrors(t *testing.T) {
	db := jobsTestDatabase(t)
	engine := NewEngine(db)
	if _, err := engine.Enqueue(context.Background(), " ", nil, "", time.Time{}, 1); err == nil {
		t.Fatal("blank job type accepted")
	}
	if _, err := engine.Enqueue(context.Background(), "bad", make(chan int), "", time.Time{}, 1); err == nil || !strings.Contains(err.Error(), "encode job payload") {
		t.Fatalf("invalid payload error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := engine.Enqueue(ctx, "canceled", nil, "", time.Time{}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled enqueue = %v", err)
	}
	if worked, err := engine.RunOnce(ctx); worked || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled RunOnce = %v, %v", worked, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if worked, err := engine.RunOnce(context.Background()); worked || err == nil {
		t.Fatalf("closed database RunOnce = %v, %v", worked, err)
	}
	if err := engine.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "run queue") {
		t.Fatalf("closed database Run = %v", err)
	}
}
