package signup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/mail"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
)

type fakeMail struct {
	ready    bool
	messages []mail.Message
	fail     bool
}

func (m *fakeMail) ConfiguredTx(context.Context, *sql.Tx) (bool, error) { return m.ready, nil }
func (m *fakeMail) EnqueueTx(_ context.Context, _ *sql.Tx, msg mail.Message) error {
	if m.fail {
		return errors.New("queue unavailable")
	}
	m.messages = append(m.messages, msg)
	return nil
}
func codeService(t *testing.T) (*Service, *fakeMail, *time.Time) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "signup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, err := secret.New(strings.Repeat("k", 40))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	m := &fakeMail{ready: true}
	return &Service{DB: db.DB, Users: store.NewUsers(db), Box: box, Mail: m, Now: func() time.Time { return now }}, m, &now
}
func lastCode(m *fakeMail) string {
	return strings.TrimPrefix(m.messages[len(m.messages)-1].Subject, "注册验证码：")
}
func TestIssueLimitsInvalidationAndCleanup(t *testing.T) {
	ctx := context.Background()
	s, m, now := codeService(t)
	if err := s.Issue(ctx, "a@example.com", "ip", "<站点>"); err != nil {
		t.Fatal(err)
	}
	code := lastCode(m)
	if len(code) != 6 || strings.Trim(code, "0123456789") != "" || !strings.Contains(m.messages[0].Text, "10 分钟") || !strings.Contains(m.messages[0].HTML, "&lt;站点&gt;") {
		t.Fatal("invalid message")
	}
	var hash string
	if err := s.DB.QueryRow(`SELECT code_hash FROM registration_email_codes`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == code || hash != s.hash("a@example.com", code) {
		t.Fatal("code not hashed")
	}
	err := s.Issue(ctx, "a@example.com", "ip", "站点")
	var cd *CooldownError
	if !errors.As(err, &cd) || cd.Seconds != 60 {
		t.Fatalf("cooldown=%v", err)
	}
	*now = now.Add(30 * time.Second)
	err = s.Issue(ctx, "a@example.com", "ip", "站点")
	if !errors.As(err, &cd) || cd.Seconds != 30 {
		t.Fatalf("remaining=%v", err)
	}
	*now = now.Add(30 * time.Second)
	if err := s.Issue(ctx, "a@example.com", "ip", "站点"); err != nil {
		t.Fatal(err)
	}
	var invalid int
	s.DB.QueryRow(`SELECT COUNT(*) FROM registration_email_codes WHERE consumed_at IS NOT NULL`).Scan(&invalid)
	if invalid != 1 {
		t.Fatal("old code still active")
	}
	for i := 2; i < 10; i++ {
		*now = now.Add(time.Minute)
		if err := s.Issue(ctx, "a@example.com", "ip", "站点"); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(time.Minute)
	if err := s.Issue(ctx, "a@example.com", "ip", "站点"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("daily=%v", err)
	}
	for i := 0; i < 10; i++ {
		if err := s.Issue(ctx, fmt.Sprintf("b%d@example.com", i), "ip", "站点"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Issue(ctx, "over@example.com", "ip", "站点"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("ip=%v", err)
	}
	*now = now.Add(25 * time.Hour)
	if err := s.Issue(ctx, "a@example.com", "ip", "站点"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM registration_email_codes`).Scan(&n)
	if n != 1 {
		t.Fatalf("cleanup left %d", n)
	}
}
func TestCodeRegistration(t *testing.T) {
	for _, test := range []string{"valid", "wrong", "expired", "lockout", "new code", "rollback", "concurrent"} {
		t.Run(test, func(t *testing.T) {
			s, m, now := codeService(t)
			ctx := context.Background()
			if err := s.Issue(ctx, "a@example.com", "ip", "站点"); err != nil {
				t.Fatal(err)
			}
			code := lastCode(m)
			switch test {
			case "wrong":
				if _, err := s.Register(ctx, "a@example.com", "hash", "user", "wrong"); !errors.Is(err, ErrInvalid) {
					t.Fatal(err)
				}
				var n int
				s.DB.QueryRow(`SELECT attempts FROM registration_email_codes`).Scan(&n)
				if n != 1 {
					t.Fatal(n)
				}
			case "expired":
				*now = now.Add(10 * time.Minute)
				if _, err := s.Register(ctx, "a@example.com", "hash", "user", code); !errors.Is(err, ErrExpired) {
					t.Fatal(err)
				}
				return
			case "lockout":
				for i := 0; i < 5; i++ {
					_, err := s.Register(ctx, "a@example.com", "hash", "user", "bad")
					want := ErrInvalid
					if i == 4 {
						want = ErrExpired
					}
					if !errors.Is(err, want) {
						t.Fatal(err)
					}
				}
				if _, err := s.Register(ctx, "a@example.com", "hash", "user", code); !errors.Is(err, ErrExpired) {
					t.Fatal(err)
				}
				return
			case "new code":
				*now = now.Add(time.Minute)
				if err := s.Issue(ctx, "a@example.com", "ip", "站点"); err != nil {
					t.Fatal(err)
				}
				// Make the earlier value deterministically different to avoid random equality.
				s.DB.Exec(`UPDATE registration_email_codes SET code_hash=? WHERE id=(SELECT MAX(id) FROM registration_email_codes)`, s.hash("a@example.com", "654321"))
				if code == "654321" {
					code = "000000"
				}
				if _, err := s.Register(ctx, "a@example.com", "hash", "user", code); !errors.Is(err, ErrInvalid) {
					t.Fatal(err)
				}
				code = "654321"
			case "rollback":
				if _, err := s.Users.Create(ctx, "a@example.com", "hash", "user"); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Register(ctx, "a@example.com", "hash", "user", code); !errors.Is(err, store.ErrEmailTaken) {
					t.Fatal(err)
				}
				var used sql.NullString
				s.DB.QueryRow(`SELECT consumed_at FROM registration_email_codes`).Scan(&used)
				if used.Valid {
					t.Fatal("failed creation consumed code")
				}
				return
			case "concurrent":
				var wg sync.WaitGroup
				results := make(chan error, 2)
				for range 2 {
					wg.Go(func() { _, err := s.Register(ctx, "a@example.com", "hash", "user", code); results <- err })
				}
				wg.Wait()
				close(results)
				success := 0
				for err := range results {
					if err == nil {
						success++
					} else if !errors.Is(err, ErrExpired) {
						t.Fatal(err)
					}
				}
				if success != 1 {
					t.Fatalf("success=%d", success)
				}
				return
			}
			u, err := s.Register(ctx, "a@example.com", "hash", "user", code)
			if err != nil || u.Email != "a@example.com" {
				t.Fatalf("user=%+v err=%v", u, err)
			}
			if _, err := s.Register(ctx, "a@example.com", "hash", "user", code); !errors.Is(err, ErrExpired) {
				t.Fatalf("reuse=%v", err)
			}
		})
	}
}
func TestIssueFailureRollsBack(t *testing.T) {
	s, m, _ := codeService(t)
	ctx := context.Background()
	m.ready = false
	if err := s.Issue(ctx, "a@example.com", "ip", "站点"); !errors.Is(err, mail.ErrNotConfigured) {
		t.Fatal(err)
	}
	m.ready = true
	m.fail = true
	if err := s.Issue(ctx, "a@example.com", "ip", "站点"); err == nil {
		t.Fatal("accepted failed mail")
	}
	var n int
	s.DB.QueryRow(`SELECT COUNT(*) FROM registration_email_codes`).Scan(&n)
	if n != 0 {
		t.Fatal("failed mail persisted code")
	}
}

func TestConcurrentIssuance(t *testing.T) {
	s, m, _ := codeService(t)
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for range 4 {
		wg.Go(func() { results <- s.Issue(context.Background(), "a@example.com", "ip", "站点") })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			var cooldown *CooldownError
			if !errors.As(err, &cooldown) {
				t.Fatal(err)
			}
		}
	}
	if successes != 1 || len(m.messages) != 1 {
		t.Fatalf("issued=%d sent=%d", successes, len(m.messages))
	}
}
