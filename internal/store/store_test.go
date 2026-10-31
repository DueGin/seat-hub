package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTest(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name IN
		('users','sessions','products','orders','subscriptions','resources','api_keys','jobs','settings','rental_pool')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("tables = %d, want 10", n)
	}
}

func TestUsersCRUD(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	users := NewUsers(db)
	u, err := users.Create(ctx, "A@Example.com", "hash", "user")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.Email != "a@example.com" {
		t.Fatalf("email not normalized: %q", u.Email)
	}
	if _, err := users.Create(ctx, "a@example.com", "hash", "user"); err != ErrEmailTaken {
		t.Fatalf("duplicate err = %v", err)
	}
	got, err := users.GetByEmail(ctx, "A@EXAMPLE.COM")
	if err != nil || got.ID != u.ID {
		t.Fatalf("get by email: %v %+v", err, got)
	}
	if err := users.SetStatus(ctx, u.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	got, _ = users.GetByID(ctx, u.ID)
	if got.Status != "disabled" {
		t.Fatalf("status = %q", got.Status)
	}
	if n, _ := users.CountAdmins(ctx); n != 0 {
		t.Fatalf("admins = %d", n)
	}
}

func TestSessionsExpiry(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	user, err := NewUsers(db).Create(context.Background(), "s@example.com", "hash", "user")
	if err != nil {
		t.Fatal(err)
	}
	sessions := NewSessions(db)
	if err := sessions.Create(ctx, "h1", user.ID, parseOrNow("2030-01-01T00:00:00Z"), "1.2.3.4", "ua"); err != nil {
		t.Fatal(err)
	}
	uid, err := sessions.Lookup(ctx, "h1")
	if err != nil || uid != user.ID {
		t.Fatalf("lookup: %v %d", err, uid)
	}
	if err := sessions.Create(ctx, "h2", user.ID, parseOrNow("2020-01-01T00:00:00Z"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Lookup(ctx, "h2"); err != ErrNotFound {
		t.Fatalf("expired session err = %v", err)
	}
	if err := sessions.DeleteOthers(ctx, user.ID, "h1"); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Lookup(ctx, "h1"); err != nil {
		t.Fatalf("kept session lost: %v", err)
	}
}

func parseOrNow(s string) (t time.Time) { return ParseTime(s) }
