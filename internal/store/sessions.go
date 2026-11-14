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

// ActiveSession is a signed-in device, as the admin console lists it.
type ActiveSession struct {
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	IP        string `json:"ip"`
	UserAgent string `json:"user_agent"`
}

// ListActive returns the user's unexpired sessions, newest first.
func (r *Sessions) ListActive(ctx context.Context, userID int64) ([]ActiveSession, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT created_at, expires_at, COALESCE(ip,''), COALESCE(user_agent,'')
		FROM sessions WHERE user_id = ? AND julianday(expires_at) > julianday(?) ORDER BY created_at DESC LIMIT 50`, userID, Now())
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	out := []ActiveSession{}
	for rows.Next() {
		var item ActiveSession
		if err := rows.Scan(&item.CreatedAt, &item.ExpiresAt, &item.IP, &item.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// CountActive is how many devices the user is signed in on.
func (r *Sessions) CountActive(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM sessions WHERE user_id = ? AND julianday(expires_at) > julianday(?)`, userID, Now()).Scan(&n)
	return n, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
