package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	sqlite3 "modernc.org/sqlite/lib"
)

var errJobsLeaseLost = errors.New("job lease lost")

type Job struct {
	ID          int64
	Type        string
	Payload     json.RawMessage
	Attempts    int
	MaxAttempts int
	RunAfter    time.Time
	DedupeKey   string
}

type Handler func(context.Context, Job) error

type RetryAfterError struct {
	After time.Duration
	Err   error
}

func (retry RetryAfterError) Error() string {
	if retry.Err != nil {
		return fmt.Sprintf("retry after %s: %v", retry.After, retry.Err)
	}
	return fmt.Sprintf("retry after %s", retry.After)
}

func (retry RetryAfterError) Unwrap() error { return retry.Err }

type Engine struct {
	Workers       int
	PollInterval  time.Duration
	LeaseDuration time.Duration
	Now           func() time.Time

	db       *sql.DB
	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewEngine(db *sql.DB) *Engine {
	return &Engine{
		db: db, Workers: 4, PollInterval: 250 * time.Millisecond,
		LeaseDuration: time.Minute, Now: time.Now,
		handlers: make(map[string]Handler),
	}
}

func (engine *Engine) Register(jobType string, handler Handler) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.handlers[jobType] = handler
}

func (engine *Engine) Enqueue(ctx context.Context, jobType string, payload any, dedupeKey string, runAfter time.Time, maxAttempts int) (int64, error) {
	if engine.db == nil {
		return 0, errors.New("database is required")
	}
	if strings.TrimSpace(jobType) == "" {
		return 0, errors.New("job type is required")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("encode job payload: %w", err)
	}
	now := engine.now()
	if runAfter.IsZero() {
		runAfter = now
	}
	if maxAttempts <= 0 {
		maxAttempts = 8
	}
	var key any
	if dedupeKey != "" {
		key = dedupeKey
	}
	for {
		var jobID int64
		err = engine.db.QueryRowContext(ctx, `INSERT INTO jobs
			(type, payload, status, attempts, max_attempts, run_after, dedupe_key, created_at, updated_at)
			VALUES (?, ?, 'pending', 0, ?, ?, ?, ?, ?)
			ON CONFLICT (dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('pending','running')
			DO NOTHING RETURNING id`, jobType, string(encoded), maxAttempts,
			jobsTime(runAfter), key, jobsTime(now), jobsTime(now)).Scan(&jobID)
		if err == nil {
			return jobID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("enqueue job: %w", err)
		}
		err = engine.db.QueryRowContext(ctx, `SELECT id FROM jobs
			WHERE dedupe_key = ? AND status IN ('pending','running')`, key).Scan(&jobID)
		if err == nil {
			return jobID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("find live duplicate job: %w", err)
		}
	}
}

func (engine *Engine) Run(ctx context.Context) error {
	if engine.db == nil {
		return errors.New("database is required")
	}
	workers, interval := engine.Workers, engine.PollInterval
	if workers <= 0 {
		workers = 4
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	failures := make(chan error, 1)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Go(func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for runCtx.Err() == nil {
				if _, err := engine.RunOnce(runCtx); err != nil && !jobsIsBusy(err) {
					select {
					case failures <- err:
					default:
					}
					cancel()
					return
				}
				select {
				case <-runCtx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
	group.Wait()
	select {
	case err := <-failures:
		return fmt.Errorf("run queue: %w", err)
	default:
		return fmt.Errorf("run queue: %w", ctx.Err())
	}
}

func (engine *Engine) RunOnce(ctx context.Context) (bool, error) {
	if engine.db == nil {
		return false, errors.New("database is required")
	}
	now := engine.now()
	result, err := engine.db.ExecContext(ctx, `UPDATE jobs SET status = 'failed',
		last_error = CASE WHEN status = 'running' THEN 'lease expired after final attempt'
			ELSE 'maximum attempts exhausted' END, locked_until = NULL, updated_at = ?
		WHERE attempts >= max_attempts AND (status = 'pending' OR
			(status = 'running' AND (locked_until IS NULL OR rtrim(locked_until, 'Z') <= rtrim(?, 'Z'))))`,
		jobsTime(now), jobsTime(now))
	if err != nil {
		return false, fmt.Errorf("fail exhausted jobs: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count exhausted jobs: %w", err)
	}
	job, err := engine.claim(ctx, now)
	if err != nil {
		return changed > 0, err
	}
	if job == nil {
		return changed > 0, nil
	}
	engine.mu.RLock()
	handler := engine.handlers[job.Type]
	engine.mu.RUnlock()
	leaseEnd, err := time.Parse(time.RFC3339Nano, job.lockedUntil)
	if err != nil {
		return true, fmt.Errorf("parse job %d lease: %w", job.ID, err)
	}
	remaining := leaseEnd.Sub(engine.now())
	if remaining <= 0 {
		return true, nil
	}
	jobCtx, cancelJob := context.WithCancelCause(ctx)
	defer cancelJob(nil)
	expiry := time.AfterFunc(remaining, func() { cancelJob(errJobsLeaseLost) })
	defer expiry.Stop()
	stopRenewal := make(chan struct{})
	renewalDone := make(chan jobsRenewal, 1)
	go func() {
		renewalDone <- engine.maintainLease(jobCtx, stopRenewal, cancelJob, *job, expiry)
	}()
	handlerErr := jobCtx.Err()
	if handlerErr == nil {
		handlerErr = jobsInvoke(jobCtx, handler, job.Job)
	}
	// Do not cancel an in-flight renewal: SQLite may have committed the new
	// lease before returning a cancellation error, leaving completion fenced
	// against an obsolete lease token. Wait for the renewal result instead.
	close(stopRenewal)
	renewal := <-renewalDone
	expiry.Stop()
	if jobCtx.Err() != nil {
		handlerErr = jobCtx.Err()
	}
	if ctx.Err() != nil {
		handlerErr = ctx.Err()
	}
	if errors.Is(context.Cause(jobCtx), errJobsLeaseLost) {
		return true, nil
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := jobsRetryBusy(finishCtx, func() error { return engine.complete(finishCtx, renewal.job, handlerErr) }); err != nil {
		return true, err
	}
	if ctx.Err() != nil {
		return true, fmt.Errorf("run job %d: %w", job.ID, ctx.Err())
	}
	if renewal.err != nil {
		return true, renewal.err
	}
	return true, nil
}

type jobsLease struct {
	Job
	lockedUntil string
}

type jobsRenewal struct {
	job jobsLease
	err error
}

func (engine *Engine) maintainLease(ctx context.Context, stop <-chan struct{}, cancel context.CancelCauseFunc, job jobsLease, expiry *time.Timer) jobsRenewal {
	interval := engine.leaseDuration() / 3
	if interval <= 0 {
		interval = time.Nanosecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return jobsRenewal{job: job}
		case <-stop:
			return jobsRenewal{job: job}
		case <-ticker.C:
			err := jobsRetryBusy(ctx, func() error { return engine.renew(ctx, &job) })
			if err != nil {
				if ctx.Err() != nil {
					return jobsRenewal{job: job}
				}
				cancel(err)
				return jobsRenewal{job: job, err: err}
			}
			leaseEnd, err := time.Parse(time.RFC3339Nano, job.lockedUntil)
			if err != nil {
				err = fmt.Errorf("parse renewed job %d lease: %w", job.ID, err)
				cancel(err)
				return jobsRenewal{job: job, err: err}
			}
			remaining := leaseEnd.Sub(engine.now())
			if remaining <= 0 {
				cancel(errJobsLeaseLost)
				return jobsRenewal{job: job}
			}
			expiry.Reset(remaining)
		}
	}
}

func (engine *Engine) renew(ctx context.Context, job *jobsLease) error {
	now := engine.now()
	previous, err := time.Parse(time.RFC3339Nano, job.lockedUntil)
	if err != nil {
		return fmt.Errorf("parse job %d lease: %w", job.ID, err)
	}
	leaseEnd := now.Add(engine.leaseDuration())
	if !leaseEnd.After(previous) {
		leaseEnd = previous.Add(time.Nanosecond)
	}
	lockedUntil := jobsTime(leaseEnd)
	result, err := engine.db.ExecContext(ctx, `UPDATE jobs SET locked_until = ?, updated_at = ?
		WHERE id = ? AND status = 'running' AND attempts = ? AND locked_until = ?
		AND rtrim(locked_until, 'Z') > rtrim(?, 'Z')`, lockedUntil, jobsTime(now),
		job.ID, job.Attempts, job.lockedUntil, jobsTime(now))
	if err != nil {
		return fmt.Errorf("renew job %d lease: %w", job.ID, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count renewed jobs: %w", err)
	}
	if count == 0 {
		return errJobsLeaseLost
	}
	job.lockedUntil = lockedUntil
	return nil
}

func (engine *Engine) claim(ctx context.Context, now time.Time) (*jobsLease, error) {
	leaseDuration := engine.leaseDuration()
	for {
		var job jobsLease
		var payload, runAfter, status string
		var lockedUntil, dedupeKey sql.NullString
		err := engine.db.QueryRowContext(ctx, `SELECT id, type, payload, attempts, max_attempts,
			run_after, dedupe_key, status, locked_until FROM jobs WHERE attempts < max_attempts AND
			((status = 'pending' AND rtrim(run_after, 'Z') <= rtrim(?, 'Z')) OR
			 (status = 'running' AND (locked_until IS NULL OR rtrim(locked_until, 'Z') <= rtrim(?, 'Z'))))
			ORDER BY run_after, id LIMIT 1`, jobsTime(now), jobsTime(now)).Scan(
			&job.ID, &job.Type, &payload, &job.Attempts, &job.MaxAttempts,
			&runAfter, &dedupeKey, &status, &lockedUntil)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("find ready job: %w", err)
		}
		job.RunAfter, err = time.Parse(time.RFC3339Nano, runAfter)
		if err != nil {
			return nil, fmt.Errorf("parse job %d run time: %w", job.ID, err)
		}
		job.RunAfter = job.RunAfter.UTC()
		leaseEnd := now.Add(leaseDuration)
		if lockedUntil.Valid {
			previous, err := time.Parse(time.RFC3339Nano, lockedUntil.String)
			if err != nil {
				return nil, fmt.Errorf("parse job %d lease: %w", job.ID, err)
			}
			if !leaseEnd.After(previous) {
				leaseEnd = previous.Add(time.Nanosecond)
			}
		}
		job.lockedUntil = jobsTime(leaseEnd)
		result, err := engine.db.ExecContext(ctx, `UPDATE jobs SET status = 'running', attempts = attempts + 1,
			locked_until = ?, updated_at = ? WHERE id = ? AND status = ? AND attempts = ?
			AND max_attempts = ? AND run_after = ? AND locked_until IS ?`, job.lockedUntil,
			jobsTime(now), job.ID, status, job.Attempts, job.MaxAttempts, runAfter, lockedUntil)
		if err != nil {
			return nil, fmt.Errorf("claim job %d: %w", job.ID, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count claimed jobs: %w", err)
		}
		if count == 0 {
			now = engine.now()
			continue
		}
		job.Attempts++
		job.Payload = json.RawMessage(payload)
		job.DedupeKey = dedupeKey.String
		return &job, nil
	}
}

func (engine *Engine) complete(ctx context.Context, job jobsLease, handlerErr error) error {
	now := engine.now()
	status, attempts, runAfter := "succeeded", job.Attempts, job.RunAfter
	var lastError, lockedUntil any
	if handlerErr != nil {
		lastError = handlerErr.Error()
		status = "pending"
		delay := jobsBackoff(job.Attempts)
		var retryPointer *RetryAfterError
		var retryValue RetryAfterError
		switch {
		case errors.As(handlerErr, &retryPointer) && retryPointer != nil:
			attempts--
			delay = retryPointer.After
		case errors.As(handlerErr, &retryValue):
			attempts--
			delay = retryValue.After
		case errors.Is(handlerErr, context.Canceled), errors.Is(handlerErr, context.DeadlineExceeded):
			attempts--
			delay = 0
		case attempts >= job.MaxAttempts:
			status = "failed"
		}
		if delay < 0 {
			delay = 0
		}
		runAfter = now.Add(delay)
		if status == "pending" {
			lockedUntil = job.lockedUntil
		}
	}
	_, err := engine.db.ExecContext(ctx, `UPDATE jobs SET status = ?, attempts = ?, run_after = ?,
		locked_until = ?, last_error = ?, updated_at = ? WHERE id = ? AND status = 'running'
		AND attempts = ? AND locked_until = ? AND rtrim(locked_until, 'Z') > rtrim(?, 'Z')`,
		status, attempts, jobsTime(runAfter), lockedUntil, lastError, jobsTime(now), job.ID,
		job.Attempts, job.lockedUntil, jobsTime(now))
	if err != nil {
		return fmt.Errorf("complete job %d: %w", job.ID, err)
	}
	return nil
}

func jobsInvoke(ctx context.Context, handler Handler, job Job) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if cause, ok := recovered.(error); ok {
				err = fmt.Errorf("handler panic: %w", cause)
			} else {
				err = fmt.Errorf("handler panic: %v", recovered)
			}
		}
	}()
	if handler == nil {
		return fmt.Errorf("no handler registered for %q", job.Type)
	}
	return handler(ctx, job)
}

func jobsBackoff(attempts int) time.Duration {
	delay := time.Second
	for attempt := 1; attempt < attempts && delay < 5*time.Minute; attempt++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func jobsIsBusy(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	code := coded.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

func jobsRetryBusy(ctx context.Context, operation func() error) error {
	for {
		err := operation()
		if !jobsIsBusy(err) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

func (engine *Engine) leaseDuration() time.Duration {
	if engine.LeaseDuration <= 0 {
		return time.Minute
	}
	return engine.LeaseDuration
}

func (engine *Engine) now() time.Time {
	if engine.Now == nil {
		return time.Now().UTC()
	}
	return engine.Now().UTC()
}

func jobsTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
