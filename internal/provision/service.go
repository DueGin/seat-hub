package provision

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/DueGin/seat-hub/internal/selfcheck"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

var (
	ErrNotFound     = errors.New("resource not found")
	ErrNoFreeSeat   = errors.New("no free hosted seat")
	ErrInvalidState = errors.New("invalid resource state")
	ErrAuthExpired  = errors.New("auth url expired")
	ErrInvalidCode  = errors.New("invalid authorization code")
	ErrOutOfStock   = errors.New("insufficient rental stock")
)

type Service struct {
	db         *sql.DB
	client     vm2api.Client
	engine     *Engine
	mu         sync.Mutex
	Now        func() time.Time
	EnsureUser func(context.Context, int64) (string, error)
	checker    selfcheck.Checker
}

func New(db *sql.DB, client vm2api.Client) *Service {
	service := &Service{db: db, client: client, Now: time.Now}
	service.engine = NewEngine(db)
	service.engine.Register("fulfil_order", service.fulfilJob)
	service.engine.Register("provision_hosted", service.provisionJob)
	service.engine.Register("watch_bootstrap", service.bootstrapJob)
	service.engine.Register("release_resource", service.releaseJob)
	service.engine.Register("sync_concurrency", service.syncJob)
	service.engine.Register("sweep_expiry", func(ctx context.Context, job Job) error { return service.SweepExpiry(ctx) })
	service.engine.Register("selfcheck_sweep", func(ctx context.Context, job Job) error { return service.SweepSelfcheck(ctx) })
	return service
}

func (service *Service) Engine() *Engine { return service.engine }

func (service *Service) now() time.Time { return service.Now().UTC() }

func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339) }

func (service *Service) site(ctx context.Context) (store.Site, error) {
	settings := store.DefaultSite()
	var raw string
	err := service.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'site'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, fmt.Errorf("read site settings: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return settings, fmt.Errorf("decode site settings: %w", err)
	}
	return settings, nil
}

func (service *Service) upstreamUser(ctx context.Context, userID int64) (string, error) {
	if service.EnsureUser != nil {
		return service.EnsureUser(ctx, userID)
	}
	var upstream sql.NullString
	if err := service.db.QueryRowContext(ctx, `SELECT vm2api_user_id FROM users WHERE id = ?`, userID).Scan(&upstream); err != nil {
		return "", fmt.Errorf("read upstream user: %w", err)
	}
	if upstream.Valid && upstream.String != "" {
		return upstream.String, nil
	}
	var password [32]byte
	if _, err := rand.Read(password[:]); err != nil {
		return "", fmt.Errorf("generate upstream password: %w", err)
	}
	username := fmt.Sprintf("portal_%d", userID)
	user, err := service.client.CreateUser(ctx, username, hex.EncodeToString(password[:]))
	if err != nil {
		return "", fmt.Errorf("create upstream user: %w", err)
	}
	if _, err := service.db.ExecContext(ctx, `UPDATE users SET vm2api_user_id = ?, vm2api_username = ?, updated_at = ? WHERE id = ?`, user.ID, username, timestamp(service.now()), userID); err != nil {
		return "", fmt.Errorf("save upstream user: %w", err)
	}
	return user.ID, nil
}

func (service *Service) EnsureUpstreamUser(ctx context.Context, userID int64) (string, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.upstreamUser(ctx, userID)
}

type jobPayload struct {
	OrderID    int64     `json:"order_id,omitempty"`
	ResourceID int64     `json:"resource_id,omitempty"`
	UserID     int64     `json:"user_id,omitempty"`
	Deadline   time.Time `json:"deadline,omitempty"`
}

func decodePayload(job Job) (jobPayload, error) {
	var payload jobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return payload, fmt.Errorf("decode job payload: %w", err)
	}
	return payload, nil
}

func enqueueTx(ctx context.Context, tx *sql.Tx, kind string, payload jobPayload, dedupe string, when time.Time) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode job payload: %w", err)
	}
	now := timestamp(when)
	_, err = tx.ExecContext(ctx, `INSERT INTO jobs(type,payload,status,max_attempts,run_after,dedupe_key,created_at,updated_at) VALUES (?,?,'pending',8,?,?,?,?) ON CONFLICT DO NOTHING`, kind, string(raw), now, dedupe, now, now)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", kind, err)
	}
	return nil
}

func (service *Service) EnqueueFulfilOrder(ctx context.Context, orderID int64) error {
	_, err := service.engine.Enqueue(ctx, "fulfil_order", jobPayload{OrderID: orderID}, fmt.Sprintf("fulfil_order:%d", orderID), service.now(), 8)
	return err
}

func (service *Service) ApplyPaidOrder(ctx context.Context, orderID int64) error {
	return service.FulfilOrder(ctx, orderID)
}

func (service *Service) fulfilJob(ctx context.Context, job Job) error {
	payload, err := decodePayload(job)
	if err != nil {
		return err
	}
	return service.FulfilOrder(ctx, payload.OrderID)
}

func (service *Service) syncJob(ctx context.Context, job Job) error {
	payload, err := decodePayload(job)
	if err != nil {
		return err
	}
	return service.SyncConcurrency(ctx, payload.UserID)
}

func (service *Service) Run(ctx context.Context) error {
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.engine.Run(workerCtx) }()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var nextSelfcheck time.Time
	for {
		if _, err := service.engine.Enqueue(workerCtx, "sweep_expiry", jobPayload{}, "sweep_expiry", service.now(), 8); err != nil {
			cancel()
			<-done
			return fmt.Errorf("schedule expiry sweep: %w", err)
		}
		if !service.now().Before(nextSelfcheck) {
			site, err := service.site(workerCtx)
			if err == nil {
				_, err = service.engine.Enqueue(workerCtx, "selfcheck_sweep", jobPayload{}, "selfcheck_sweep", service.now(), 8)
			}
			if err != nil {
				cancel()
				<-done
				return fmt.Errorf("schedule self-check sweep: %w", err)
			}
			nextSelfcheck = service.now().Add(selfcheckInterval(site))
		}
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return nil
		case err := <-done:
			return err
		case <-ticker.C:
		}
	}
}
