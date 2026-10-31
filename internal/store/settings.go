package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Settings is a JSON key-value store for admin-editable site configuration.
type Settings struct{ db *DB }

func NewSettings(db *DB) *Settings { return &Settings{db: db} }

func (r *Settings) Get(ctx context.Context, key string, into any) (bool, error) {
	var raw string
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get setting %s: %w", key, err)
	}
	if into == nil {
		return true, nil
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		return false, fmt.Errorf("decode setting %s: %w", key, err)
	}
	return true, nil
}

func (r *Settings) Put(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode setting %s: %w", key, err)
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, string(raw), Now())
	return err
}

type Audit struct{ db *DB }

func NewAudit(db *DB) *Audit { return &Audit{db: db} }

func (r *Audit) Write(ctx context.Context, actorID int64, action, target string, detail any) {
	raw := "{}"
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			raw = string(b)
		}
	}
	_, _ = r.db.ExecContext(ctx, `INSERT INTO audit_log (actor_id, action, target, detail, created_at)
		VALUES (?, ?, ?, ?, ?)`, actorID, action, target, raw, Now())
}
