package signup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"math/big"
	"time"

	"github.com/DueGin/seat-hub/internal/mail"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
)

var (
	ErrInvalid     = errors.New("invalid registration code")
	ErrExpired     = errors.New("registration code expired")
	ErrRateLimited = errors.New("registration code rate limited")
)

type CooldownError struct{ Seconds int }

func (e *CooldownError) Error() string {
	return fmt.Sprintf("请 %d 秒后再获取验证码", e.Seconds)
}

type Mailer interface {
	ConfiguredTx(context.Context, *sql.Tx) (bool, error)
	EnqueueTx(context.Context, *sql.Tx, mail.Message) error
}

type Service struct {
	DB    *sql.DB
	Users *store.Users
	Box   *secret.Box
	Mail  Mailer
	Now   func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func codeTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func (s *Service) hash(email, code string) string {
	return s.Box.MAC("registration-email\x00" + email + "\x00" + code)
}

func (s *Service) Issue(ctx context.Context, email, ip, siteName string) error {
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin code issuance: %w", err)
	}
	defer tx.Rollback()
	// The first write serializes issuances across connections before checking limits.
	cutoff := codeTime(now.Add(-24 * time.Hour))
	if _, err := tx.ExecContext(ctx, `DELETE FROM registration_email_codes WHERE expires_at < ? OR consumed_at < ?`, cutoff, cutoff); err != nil {
		return fmt.Errorf("prune registration codes: %w", err)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE email=?`, email).Scan(&exists); err != nil {
		return fmt.Errorf("check registered email: %w", err)
	}
	if exists > 0 {
		return store.ErrEmailTaken
	}
	var latest sql.NullString
	var daily, hourly int
	if err := tx.QueryRowContext(ctx, `SELECT MAX(created_at),COUNT(CASE WHEN created_at > ? THEN 1 END) FROM registration_email_codes WHERE email=?`, cutoff, email).Scan(&latest, &daily); err != nil {
		return fmt.Errorf("check code limits: %w", err)
	}
	if latest.Valid {
		remaining := store.ParseTime(latest.String).Add(time.Minute).Sub(now)
		if remaining > 0 {
			return &CooldownError{Seconds: int((remaining + time.Second - 1) / time.Second)}
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM registration_email_codes WHERE ip=? AND created_at > ?`, ip, codeTime(now.Add(-time.Hour))).Scan(&hourly); err != nil {
		return fmt.Errorf("check ip code limit: %w", err)
	}
	if daily >= 10 || hourly >= 20 {
		return ErrRateLimited
	}
	ready, err := s.Mail.ConfiguredTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("check registration mail: %w", err)
	}
	if !ready {
		return mail.ErrNotConfigured
	}
	number, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return fmt.Errorf("generate registration code: %w", err)
	}
	code := fmt.Sprintf("%06d", number.Int64())
	stamp := codeTime(now)
	if _, err := tx.ExecContext(ctx, `UPDATE registration_email_codes SET consumed_at=? WHERE email=? AND consumed_at IS NULL`, stamp, email); err != nil {
		return fmt.Errorf("invalidate registration codes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO registration_email_codes(email,ip,code_hash,created_at,expires_at) VALUES (?,?,?,?,?)`, email, ip, s.hash(email, code), stamp, codeTime(now.Add(10*time.Minute))); err != nil {
		return fmt.Errorf("store registration code: %w", err)
	}
	text := fmt.Sprintf("%s\n\n注册验证码：%s\n验证码 10 分钟内有效。\n如果不是你本人操作，请忽略这封邮件。", siteName, code)
	msg := mail.Message{To: []string{email}, Subject: "注册验证码：" + code, Text: text, HTML: "<p>" + html.EscapeString(siteName) + "</p><p>注册验证码：<strong>" + code + "</strong></p><p>验证码 10 分钟内有效。</p><p>如果不是你本人操作，请忽略这封邮件。</p>"}
	if err := s.Mail.EnqueueTx(ctx, tx, msg); err != nil {
		return fmt.Errorf("enqueue registration code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit registration code: %w", err)
	}
	return nil
}

func (s *Service) Register(ctx context.Context, email, passwordHash, role, code string) (store.User, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return store.User{}, fmt.Errorf("begin verified registration: %w", err)
	}
	defer tx.Rollback()
	var id int64
	var want, expires string
	var consumed sql.NullString
	var attempts int
	// Acquire the write lock before reading so two requests cannot consume one code.
	err = tx.QueryRowContext(ctx, `UPDATE registration_email_codes SET attempts=attempts WHERE id=(SELECT id FROM registration_email_codes WHERE email=? ORDER BY id DESC LIMIT 1) RETURNING id,code_hash,expires_at,consumed_at,attempts`, email).Scan(&id, &want, &expires, &consumed, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return store.User{}, ErrExpired
	}
	if err != nil {
		return store.User{}, fmt.Errorf("load registration code: %w", err)
	}
	now := s.now()
	if consumed.Valid || attempts >= 5 || !now.Before(store.ParseTime(expires)) {
		return store.User{}, ErrExpired
	}
	if subtle.ConstantTimeCompare([]byte(s.hash(email, code)), []byte(want)) != 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE registration_email_codes SET attempts=attempts+1 WHERE id=?`, id); err != nil {
			return store.User{}, fmt.Errorf("record code attempt: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return store.User{}, fmt.Errorf("commit code attempt: %w", err)
		}
		if attempts+1 >= 5 {
			return store.User{}, ErrExpired
		}
		return store.User{}, ErrInvalid
	}
	u, err := s.Users.CreateTx(ctx, tx, email, passwordHash, role)
	if err != nil {
		return store.User{}, fmt.Errorf("create verified user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE registration_email_codes SET consumed_at=? WHERE id=?`, codeTime(now), id); err != nil {
		return store.User{}, fmt.Errorf("consume registration code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return store.User{}, fmt.Errorf("commit verified registration: %w", err)
	}
	return u, nil
}
