package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
)

// The gateway tells a caller the most specific reason it is refused: a
// disabled user or key must not be told "no accounts".
func TestResolverReasonPrecedence(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, err := secret.New(strings.Repeat("r", 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("sk-vm-upstream")
	if err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	for _, row := range []struct {
		id                    int64
		userStatus, keyStatus string
		presented, want       string
	}{
		{1, "active", "active", "sk-pt-active", "no_active_accounts"},
		{2, "disabled", "active", "sk-pt-user-off", "user_disabled"},
		{3, "active", "disabled", "sk-pt-key-off", "key_disabled"},
	} {
		if _, err := db.Exec(`INSERT INTO users(id,email,password_hash,status,created_at,updated_at) VALUES (?,?,?,?,?,?)`, row.id, row.presented+"@example.com", "h", row.userStatus, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`, row.id, "k", box.MAC(row.presented), "sk-pt-", "key_x", sealed, row.keyStatus, now, now); err != nil {
			t.Fatal(err)
		}
		r := newResolver(db.DB, box, func(context.Context, int64) (int, int, error) { return 1, 0, nil }, func(context.Context, int64) (map[int64]string, error) { return map[int64]string{}, nil })
		p, err := r.Resolve(context.Background(), row.presented)
		if err != nil || p == nil {
			t.Fatalf("%s: principal=%v err=%v", row.presented, p, err)
		}
		if p.Active || p.Reason != row.want {
			t.Fatalf("%s: active=%v reason=%q, want %q", row.presented, p.Active, p.Reason, row.want)
		}
	}
}
