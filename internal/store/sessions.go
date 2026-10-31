package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Session struct {
	TokenHash string    `json:"-"`
	UserID    int64     `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Sessions struct{ db *DB }

func NewSessions(db *DB) *Sessions { return &Sessions{db: db} }

func (r *Sessions) Create(ctx context.Context, tokenHash string, userID int64, expiresAt time.Time, ip, ua string) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?)`, tokenHash, userID, Now(), FmtTime(expiresAt), ip, truncate(ua, 255))
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// Lookup returns the session's user ID when the session exists and is unexpired.
func (r *Sessions) Lookup(ctx context.Context, tokenHash string) (int64, error) {
	var userID int64
	var expires string
	err := r.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("lookup session: %w", err)
	}
	if !ParseTime(expires).After(time.Now().UTC()) {
		r.Delete(ctx, tokenHash)
		return 0, ErrNotFound
	}
	return userID, nil
}

func (r *Sessions) Delete(ctx context.Context, tokenHash string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteOthers revokes every session of the user except the given one.
func (r *Sessions) DeleteOthers(ctx context.Context, userID int64, keepTokenHash string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepTokenHash)
	return err
}

func (r *Sessions) DeleteAll(ctx context.Context, userID int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
