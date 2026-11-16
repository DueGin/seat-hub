// Package mail queues notification emails and delivers them through SMTP.
package mail

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	netmail "net/mail"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/DueGin/seat-hub/internal/secret"
)

type Message struct {
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
	HTML    string   `json:"html,omitempty"`
}

type Settings struct {
	Enabled     bool     `json:"enabled"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	Security    string   `json:"security"`
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	FromAddress string   `json:"from_address"`
	FromName    string   `json:"from_name"`
	AdminEmails []string `json:"admin_emails"`
}

var (
	ErrInvalid       = errors.New("invalid mail settings or message")
	ErrNotConfigured = errors.New("mail not configured")
)

type Service struct {
	db   *sql.DB
	box  *secret.Box
	log  *slog.Logger
	mu   sync.Mutex
	send func(context.Context, Settings, Message) error
}

func New(db *sql.DB, box *secret.Box, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, box: box, log: log, send: sendSMTP}
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Service) load(ctx context.Context, q queryer) (Settings, error) {
	cfg := Settings{Port: 587, Security: "starttls", AdminEmails: []string{}}
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'mail'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("load mail settings: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("decode mail settings: %w", err)
	}
	if cfg.Password != "" {
		if s.box == nil {
			return cfg, errors.New("missing secret box")
		}
		cfg.Password, err = s.box.Open(cfg.Password)
		if err != nil {
			return cfg, fmt.Errorf("open mail password: %w", err)
		}
	}
	if cfg.AdminEmails == nil {
		cfg.AdminEmails = []string{}
	}
	return cfg, nil
}

func (s *Service) Settings(ctx context.Context) (Settings, error) {
	cfg, err := s.load(ctx, s.db)
	if cfg.Password != "" {
		cfg.Password = "***"
	}
	return cfg, err
}

// Address accepts only a bare mailbox and rejects control characters.
func Address(value string) bool {
	if strings.ContainsAny(value, "\r\n\x00") || len(value) > 254 {
		return false
	}
	a, err := netmail.ParseAddress(value)
	return err == nil && a.Address == value && strings.Contains(value, "@")
}

func validateSettings(cfg Settings) error {
	if cfg.Security != "none" && cfg.Security != "tls" && cfg.Security != "starttls" {
		return ErrInvalid
	}
	if cfg.Port < 1 || cfg.Port > 65535 || len(cfg.Host) > 253 || utf8.RuneCountInString(cfg.FromName) > 200 || len(cfg.Username) > 320 || len(cfg.Password) > 4096 || strings.ContainsAny(cfg.Host+cfg.FromName+cfg.Username, "\r\n\x00") {
		return ErrInvalid
	}
	if cfg.Host != "" && (strings.ContainsAny(cfg.Host, " /\\:@") && net.ParseIP(cfg.Host) == nil) {
		return ErrInvalid
	}
	if cfg.FromAddress != "" && !Address(cfg.FromAddress) {
		return ErrInvalid
	}
	if cfg.Enabled && (cfg.Host == "" || !Address(cfg.FromAddress) || (cfg.Username != "" && cfg.Password == "")) {
		return ErrInvalid
	}
	if len(cfg.AdminEmails) > 100 {
		return ErrInvalid
	}
	for _, a := range cfg.AdminEmails {
		if !Address(a) {
			return ErrInvalid
		}
	}
	return nil
}

func (s *Service) SaveSettings(ctx context.Context, cfg Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.FromAddress = strings.TrimSpace(cfg.FromAddress)
	if cfg.Security == "" {
		cfg.Security = "starttls"
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.Password == "***" {
		previous, err := s.load(ctx, s.db)
		if err != nil {
			return Settings{}, err
		}
		cfg.Password = previous.Password
	}
	for i := range cfg.AdminEmails {
		cfg.AdminEmails[i] = strings.ToLower(strings.TrimSpace(cfg.AdminEmails[i]))
	}
	if err := validateSettings(cfg); err != nil {
		return Settings{}, err
	}
	if cfg.Password != "" {
		if s.box == nil {
			return Settings{}, errors.New("missing secret box")
		}
		sealed, err := s.box.Seal(cfg.Password)
		if err != nil {
			return Settings{}, fmt.Errorf("seal mail password: %w", err)
		}
		cfg.Password = sealed
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return Settings{}, fmt.Errorf("encode mail settings: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES ('mail',?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, string(raw), time.Now().UTC().Format(time.RFC3339)); err != nil {
		return Settings{}, fmt.Errorf("save mail settings: %w", err)
	}
	if cfg.Password != "" {
		cfg.Password = "***"
	}
	if cfg.AdminEmails == nil {
		cfg.AdminEmails = []string{}
	}
	return cfg, nil
}

func configured(cfg Settings) bool {
	return cfg.Enabled && cfg.Host != "" && cfg.Port > 0 && Address(cfg.FromAddress) && (cfg.Username == "" || cfg.Password != "")
}

func (s *Service) Configured(ctx context.Context) (bool, error) {
	cfg, err := s.load(ctx, s.db)
	return configured(cfg), err
}

func validateMessage(msg Message) error {
	if len(msg.To) == 0 || len(msg.To) > 100 || strings.TrimSpace(msg.Subject) == "" || utf8.RuneCountInString(msg.Subject) > 200 || strings.ContainsAny(msg.Subject, "\r\n\x00") || strings.TrimSpace(msg.Text) == "" || len(msg.Text)+len(msg.HTML) > 1<<20 {
		return ErrInvalid
	}
	for _, a := range msg.To {
		if !Address(a) {
			return ErrInvalid
		}
	}
	return nil
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func enqueue(ctx context.Context, q execer, msg Message) error {
	if err := validateMessage(msg); err != nil {
		return err
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode mail: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := q.ExecContext(ctx, `INSERT INTO mail_outbox(message,run_after,created_at) VALUES (?,?,?)`, string(raw), now, now); err != nil {
		return fmt.Errorf("enqueue mail: %w", err)
	}
	return nil
}

// Enqueue silently drops messages when mail is disabled or unconfigured.
func (s *Service) Enqueue(ctx context.Context, msg Message) error {
	cfg, err := s.load(ctx, s.db)
	if err != nil {
		return err
	}
	if !configured(cfg) {
		return nil
	}
	return enqueue(ctx, s.db, msg)
}

// ConfiguredTx checks the current settings inside a caller transaction.
func (s *Service) ConfiguredTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	cfg, err := s.load(ctx, tx)
	return configured(cfg), err
}

// EnqueueTx requires configured mail and shares the caller's transaction.
func (s *Service) EnqueueTx(ctx context.Context, tx *sql.Tx, msg Message) error {
	cfg, err := s.load(ctx, tx)
	if err != nil {
		return err
	}
	if !configured(cfg) {
		return ErrNotConfigured
	}
	return enqueue(ctx, tx, msg)
}

func (s *Service) AdminRecipients(ctx context.Context) ([]string, error) {
	cfg, err := s.load(ctx, s.db)
	if err != nil {
		return nil, err
	}
	if len(cfg.AdminEmails) > 0 {
		return cfg.AdminEmails, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT email FROM users WHERE role='admin' AND status='active' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list admin emails: %w", err)
	}
	defer rows.Close()
	emails := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("scan admin email: %w", err)
		}
		if Address(a) {
			emails = append(emails, a)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read admin emails: %w", err)
	}
	return emails, nil
}

func (s *Service) SendTest(ctx context.Context, to string) error {
	if !Address(to) {
		return ErrInvalid
	}
	cfg, err := s.load(ctx, s.db)
	if err != nil {
		return err
	}
	if !configured(cfg) {
		return ErrNotConfigured
	}
	err = s.send(ctx, cfg, Message{To: []string{to}, Subject: "测试邮件", Text: "这是一封测试邮件，收到此邮件表示邮件发送配置正确。"})
	return redactError(err, cfg)
}

func redactError(err error, cfg Settings) error {
	if err == nil || cfg.Password == "" {
		return err
	}
	value := err.Error()
	for _, sensitive := range []string{cfg.Password, base64.StdEncoding.EncodeToString([]byte("\x00" + cfg.Username + "\x00" + cfg.Password))} {
		value = strings.ReplaceAll(value, sensitive, "***")
	}
	if value == err.Error() {
		return err
	}
	return errors.New(value)
}

func (s *Service) process(ctx context.Context, now time.Time) error {
	cfg, err := s.load(ctx, s.db)
	if err != nil {
		return err
	}
	stamp := now.UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM mail_outbox WHERE status='sent' AND sent_at < ?`, now.AddDate(0, 0, -30).UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("prune sent mail: %w", err)
	}
	if !configured(cfg) {
		return nil
	}
	for range 25 {
		var id int64
		var raw string
		var attempts int
		err := s.db.QueryRowContext(ctx, `UPDATE mail_outbox SET status='sending',attempts=attempts+1,locked_until=? WHERE id=(SELECT id FROM mail_outbox WHERE attempts < 6 AND ((status='pending' AND run_after<=?) OR (status='sending' AND locked_until<=?)) ORDER BY id LIMIT 1) RETURNING id,message,attempts`, now.Add(time.Minute).UTC().Format(time.RFC3339), stamp, stamp).Scan(&id, &raw, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return fmt.Errorf("claim mail: %w", err)
		}
		var msg Message
		deliveryErr := json.Unmarshal([]byte(raw), &msg)
		if deliveryErr == nil {
			deliveryErr = s.send(ctx, cfg, msg)
		}
		deliveryErr = redactError(deliveryErr, cfg)
		if ctx.Err() != nil {
			return nil
		}
		if deliveryErr == nil {
			_, err = s.db.ExecContext(ctx, `UPDATE mail_outbox SET status='sent',sent_at=?,locked_until=NULL,last_error='' WHERE id=?`, time.Now().UTC().Format(time.RFC3339), id)
		} else {
			status := "pending"
			if attempts >= 6 {
				status = "failed"
			}
			_, err = s.db.ExecContext(ctx, `UPDATE mail_outbox SET status=?,run_after=?,locked_until=NULL,last_error=? WHERE id=?`, status, now.Add(30*time.Second*time.Duration(1<<(attempts-1))).UTC().Format(time.RFC3339), deliveryErr.Error(), id)
		}
		if err != nil {
			return fmt.Errorf("update mail delivery: %w", err)
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE mail_outbox SET status='failed',locked_until=NULL WHERE status='sending' AND attempts>=6 AND locked_until<=?`, stamp)
	if err != nil {
		return fmt.Errorf("expire mail delivery: %w", err)
	}
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.process(ctx, time.Now()); err != nil && ctx.Err() == nil {
			s.log.Error("mail delivery loop failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
