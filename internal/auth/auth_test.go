package auth

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/vm2api-portal/internal/secret"
	"github.com/DueGin/vm2api-portal/internal/store"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "correct horse battery" {
		t.Fatal("password stored in clear")
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword(hash, "correct horse batterY") {
		t.Fatal("wrong password accepted")
	}
	if VerifyPassword("not-a-hash", "x") {
		t.Fatal("malformed hash accepted")
	}
}

func TestHashPasswordLengthBounds(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("accepted an 5-char password")
	}
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := HashPassword(string(long)); err == nil {
		t.Fatal("accepted a 129-char password")
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashPassword("same-password")
	b, _ := HashPassword("same-password")
	if a == b {
		t.Fatal("two hashes of the same password are identical: salt missing")
	}
}

func TestLimiterWindow(t *testing.T) {
	l := NewLimiter(2, 50*time.Millisecond)
	if !l.Allow("k") || !l.Allow("k") {
		t.Fatal("first two attempts rejected")
	}
	if l.Allow("k") {
		t.Fatal("third attempt allowed")
	}
	if !l.Allow("other") {
		t.Fatal("limit is not per key")
	}
	time.Sleep(60 * time.Millisecond)
	if !l.Allow("k") {
		t.Fatal("window did not slide")
	}
}

func TestParseInt(t *testing.T) {
	cases := map[string]int{"": 7, "abc": 7, "12": 12}
	for in, want := range cases {
		if got := ParseInt(in, 7); got != want {
			t.Fatalf("ParseInt(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLimiterSweepsExpiredBuckets(t *testing.T) {
	l := NewLimiter(3, 40*time.Millisecond)
	for i := 0; i < 50; i++ {
		l.Allow("ip|user" + strings.Repeat("x", i%5) + string(rune('a'+i%26)))
	}
	if l.Len() == 0 {
		t.Fatal("no keys tracked")
	}
	time.Sleep(60 * time.Millisecond)
	// Any call past the window triggers the sweep.
	l.Allow("fresh")
	if n := l.Len(); n != 1 {
		t.Fatalf("after sweep Len() = %d, want 1", n)
	}
}

func TestLimiterCapsKeyCount(t *testing.T) {
	l := NewLimiter(5, time.Hour)
	l.maxKeys = 64
	for i := 0; i < 500; i++ {
		l.Allow("ip|" + strings.Repeat("e", 8) + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	if n := l.Len(); n > l.maxKeys {
		t.Fatalf("Limiter grew to %d keys, cap %d", n, l.maxKeys)
	}
}

func TestLimiterTruncatesLongKeys(t *testing.T) {
	l := NewLimiter(5, time.Minute)
	base := "203.0.113.7|" + strings.Repeat("a", 400)
	l.Allow(base)
	l.Allow(base + "suffix-one")
	l.Allow(base + "suffix-two")
	if n := l.Len(); n != 1 {
		t.Fatalf("long keys were not truncated to one bucket: Len() = %d", n)
	}
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	box, err := secret.New(strings.Repeat("k", 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	return &Service{Box: box, Sessions: store.NewSessions(db), Users: store.NewUsers(db)}
}

func TestDummyHashIsARealArgon2Hash(t *testing.T) {
	if dummyPasswordHash == "" {
		t.Fatal("dummy hash missing: unknown emails skip the argon2 burn")
	}
	if !VerifyPassword(dummyPasswordHash, "vm2api-portal-nonexistent-account") {
		t.Fatal("dummy hash is not a valid argon2id verification target")
	}
}

func TestLoginUnknownEmailCostsAnArgon2Verify(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Users.Create(ctx, "real@example.com", mustHash(t, "correct-password"), "user"); err != nil {
		t.Fatal(err)
	}
	timeOne := func(email, password string) time.Duration {
		start := time.Now()
		_, _ = svc.Login(ctx, email, password, "127.0.0.1", "test")
		return time.Since(start)
	}
	// Warm the DB connection so the first measurement does not pay for setup.
	timeOne("real@example.com", "wrong-password")

	wrong := timeOne("real@example.com", "wrong-password")
	missing := timeOne("nobody@example.com", "wrong-password")
	if missing*4 < wrong {
		t.Fatalf("unknown email returned %v vs %v for a real one: timing side channel", missing, wrong)
	}
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Users.Create(ctx, "u@example.com", mustHash(t, "old-password"), "user"); err != nil {
		t.Fatal(err)
	}
	first, err := svc.Login(ctx, "u@example.com", "old-password", "127.0.0.1", "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Login(ctx, "u@example.com", "old-password", "127.0.0.2", "b")
	if err != nil {
		t.Fatal(err)
	}
	keep := svc.Box.MAC(first.Token)
	other := svc.Box.MAC(second.Token)
	if _, err := svc.Sessions.Lookup(ctx, other); err != nil {
		t.Fatalf("setup: second session missing: %v", err)
	}

	if err := svc.ChangePassword(ctx, first.User.ID, mustHash(t, "new-password"), keep); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err := svc.Sessions.Lookup(ctx, keep); err != nil {
		t.Fatalf("current session was revoked: %v", err)
	}
	if _, err := svc.Sessions.Lookup(ctx, other); err == nil {
		t.Fatal("other session survived the password change")
	}
	if _, err := svc.Login(ctx, "u@example.com", "new-password", "127.0.0.1", "a"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	if _, err := svc.Login(ctx, "u@example.com", "old-password", "127.0.0.1", "a"); err == nil {
		t.Fatal("old password still accepted")
	}
}

func TestRevokeOthersWithEmptyKeepRevokesAll(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	u, err := svc.Users.Create(ctx, "v@example.com", mustHash(t, "old-password"), "user")
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Login(ctx, "v@example.com", "old-password", "127.0.0.1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeOthers(ctx, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Sessions.Lookup(ctx, svc.Box.MAC(res.Token)); err == nil {
		t.Fatal("session survived RevokeOthers with an empty keep hash")
	}
}
