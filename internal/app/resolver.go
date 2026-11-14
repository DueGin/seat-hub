package app

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/DueGin/seat-hub/internal/gateway"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
)

// resolver maps a presented portal key to a gateway.Principal.
//
// It is on the hot path of every proxied request, so the key lookup, user
// status and entitlement snapshot are cached for a few seconds. Writes
// (disable key, change concurrency) invalidate through Invalidate.
type resolver struct {
	db          *sql.DB
	box         *secret.Box
	entitle     func(ctx context.Context, userID int64) (concurrency int, activeAccounts int, err error)
	schedulable func(ctx context.Context, userID int64) (map[int64]string, error)

	mu    sync.Mutex
	cache map[string]cachedPrincipal
	ttl   time.Duration
}

type cachedPrincipal struct {
	p       *gateway.Principal
	expires time.Time
}

func newResolver(db *sql.DB, box *secret.Box, entitle func(ctx context.Context, userID int64) (int, int, error), schedulable func(context.Context, int64) (map[int64]string, error)) *resolver {
	return &resolver{db: db, box: box, entitle: entitle, schedulable: schedulable, cache: map[string]cachedPrincipal{}, ttl: 5 * time.Second}
}

func (r *resolver) Invalidate() {
	r.mu.Lock()
	r.cache = map[string]cachedPrincipal{}
	r.mu.Unlock()
}

func (r *resolver) Resolve(ctx context.Context, presented string) (*gateway.Principal, error) {
	key := presented
	if key == "" {
		return nil, nil
	}
	hash := r.box.MAC(key)
	now := time.Now()
	r.mu.Lock()
	if c, ok := r.cache[hash]; ok && c.expires.After(now) {
		r.mu.Unlock()
		return c.p, nil
	}
	r.mu.Unlock()

	p, err := r.lookup(ctx, hash)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	// Bound the cache so a flood of bogus keys cannot grow it without limit.
	if len(r.cache) > 10000 {
		r.cache = map[string]cachedPrincipal{}
	}
	r.cache[hash] = cachedPrincipal{p: p, expires: now.Add(r.ttl)}
	r.mu.Unlock()
	return p, nil
}

func (r *resolver) lookup(ctx context.Context, hash string) (*gateway.Principal, error) {
	var (
		id            int64
		userID        int64
		secretEnc     string
		status        string
		userStatus    string
		secretInvalid bool
		scopeMode     string
		scopeAccounts string
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT k.id, k.user_id, k.vm2api_secret_enc, k.status, u.status, k.scope_mode, k.scope_accounts
		FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = ?`, hash).Scan(&id, &userID, &secretEnc, &status, &userStatus, &scopeMode, &scopeAccounts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	upstream, err := r.box.Open(secretEnc)
	if err != nil {
		secretInvalid = true
	}
	p := &gateway.Principal{UserID: userID, KeyID: id, UpstreamSecret: upstream, Active: true}
	if status != "active" {
		p.Active, p.Reason = false, "key_disabled"
	}
	if userStatus != "active" {
		p.Active, p.Reason = false, "user_disabled"
	}
	if secretInvalid {
		p.Active, p.Reason = false, "key_disabled"
	}
	concurrency, accounts, err := r.entitle(ctx, userID)
	if err != nil {
		return nil, err
	}
	p.Concurrency = concurrency
	if accounts == 0 {
		p.Active, p.Reason = false, "no_active_accounts"
	}
	if p.Active && scopeMode == "selected" {
		vms, err := r.schedulable(ctx, userID)
		if err != nil {
			return nil, err
		}
		usable := false
		for _, id := range store.DecodeIDs(scopeAccounts) {
			if vms[id] != "" {
				usable = true
				break
			}
		}
		if !usable {
			p.Active, p.Reason = false, "key_no_accounts"
		}
	}
	return p, nil
}

// Touch records last_used_at at most once a minute per key without blocking.
func (r *resolver) Touch(keyID int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = r.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ?
			WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
			store.Now(), keyID, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
	}()
}
