// Package reservation collects product launch notification requests.
package reservation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/DueGin/seat-hub/internal/mail"
)

var (
	ErrInvalid = errors.New("invalid reservation")
	// ErrInvalidNotice: the admin's notification subject/body is unusable.
	ErrInvalidNotice = errors.New("invalid reservation notice")
	// ErrInvalidFilter: an admin list/export filter is malformed.
	ErrInvalidFilter = errors.New("invalid reservation filter")
	ErrProduct       = errors.New("product unavailable for reservation")
	ErrNotFound      = errors.New("reservation not found")
	ErrRateLimit     = errors.New("reservation rate limit exceeded")
)

type Item struct {
	ID          int64   `json:"id"`
	ProductID   int64   `json:"product_id"`
	ProductName string  `json:"product_name"`
	UserID      *int64  `json:"user_id"`
	Email       string  `json:"email"`
	Note        string  `json:"note"`
	Status      string  `json:"status"`
	IP          string  `json:"ip"`
	CreatedAt   string  `json:"created_at"`
	NotifiedAt  *string `json:"notified_at"`
}
type Count struct {
	ProductID   int64  `json:"product_id"`
	ProductName string `json:"product_name"`
	Total       int64  `json:"total"`
	Waiting     int64  `json:"waiting"`
	Notified    int64  `json:"notified"`
}
type Filter struct {
	ProductID      int64
	Status, Email  string
	Page, PageSize int
}
type Page struct {
	Items    []Item  `json:"items"`
	Total    int64   `json:"total"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`
	Counts   []Count `json:"counts"`
}
type bucket struct {
	start time.Time
	count int
}
type Service struct {
	db   *sql.DB
	mail *mail.Service
	mu   sync.Mutex
	ips  map[string]bucket
}

func New(db *sql.DB, mailer *mail.Service) *Service {
	return &Service{db: db, mail: mailer, ips: map[string]bucket{}}
}

func (s *Service) allowed(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, b := range s.ips {
		if now.Sub(b.start) >= time.Hour {
			delete(s.ips, key)
		}
	}
	b := s.ips[ip]
	if b.start.IsZero() {
		b.start = now
	}
	if b.count >= 10 {
		return false
	}
	// Bound storage even if many distinct clients connect within one hour.
	if len(s.ips) >= 10000 && b.count == 0 {
		return false
	}
	b.count++
	s.ips[ip] = b
	return true
}

func (s *Service) Create(ctx context.Context, productID int64, userID *int64, email, note, ip string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	note = strings.TrimSpace(note)
	if productID <= 0 || !mail.Address(email) || utf8.RuneCountInString(note) > 200 || strings.ContainsRune(note, 0) {
		return ErrInvalid
	}
	if !s.allowed(ip) {
		return ErrRateLimit
	}
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM products WHERE id=? AND enabled=1 AND reservation_only=1`, productID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProduct
	}
	if err != nil {
		return fmt.Errorf("find reservation product: %w", err)
	}
	// A single write avoids upgrading concurrent SQLite read transactions and
	// rechecks the product flag if an administrator changes it during the request.
	result, err := s.db.ExecContext(ctx, `INSERT INTO reservations(product_id,user_id,email,note,ip,created_at)
 SELECT id,?,?,?,?,? FROM products WHERE id=? AND enabled=1 AND reservation_only=1
 ON CONFLICT(product_id,email) DO NOTHING`, userID, email, note, ip, time.Now().UTC().Format(time.RFC3339), productID)
	if err != nil {
		return fmt.Errorf("create reservation: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count reservations: %w", err)
	}
	if n == 0 {
		var available bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM products WHERE id=? AND enabled=1 AND reservation_only=1)`, productID).Scan(&available); err != nil {
			return fmt.Errorf("check reservation product: %w", err)
		}
		if !available {
			return ErrProduct
		}
	}
	if n > 0 && s.mail != nil {
		// Notification failure must never undo a successfully recorded reservation.
		_ = s.mail.Enqueue(ctx, mail.Message{To: []string{email}, Subject: "预约成功", Text: fmt.Sprintf("你已成功预约「%s」。开售时我们会发邮件通知你。", name)})
	}
	return nil
}

func filterSQL(f Filter) (string, []any, error) {
	if f.ProductID < 0 || (f.Status != "" && f.Status != "waiting" && f.Status != "notified") || len(f.Email) > 254 {
		return "", nil, ErrInvalidFilter
	}
	where := " WHERE 1=1"
	args := []any{}
	if f.ProductID > 0 {
		where += " AND r.product_id=?"
		args = append(args, f.ProductID)
	}
	if f.Status != "" {
		where += " AND r.status=?"
		args = append(args, f.Status)
	}
	if f.Email != "" {
		where += " AND instr(lower(r.email),lower(?))>0"
		args = append(args, f.Email)
	}
	return where, args, nil
}
func (s *Service) list(ctx context.Context, f Filter, all bool) (Page, error) {
	where, args, err := filterSQL(f)
	if err != nil {
		return Page{}, err
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}
	page := Page{Items: []Item{}, Counts: []Count{}, Page: f.Page, PageSize: f.PageSize}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM reservations r`+where, args...).Scan(&page.Total); err != nil {
		return page, fmt.Errorf("count reservations: %w", err)
	}
	query := `SELECT r.id,r.product_id,p.name,r.user_id,r.email,r.note,r.status,r.ip,r.created_at,r.notified_at FROM reservations r JOIN products p ON p.id=r.product_id` + where + ` ORDER BY r.id DESC`
	if !all {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return page, fmt.Errorf("list reservations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.ProductID, &item.ProductName, &item.UserID, &item.Email, &item.Note, &item.Status, &item.IP, &item.CreatedAt, &item.NotifiedAt); err != nil {
			return page, fmt.Errorf("scan reservation: %w", err)
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return page, fmt.Errorf("read reservations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return page, fmt.Errorf("close reservations: %w", err)
	}
	counts, err := s.db.QueryContext(ctx, `SELECT p.id,p.name,count(r.id),coalesce(sum(r.status='waiting'),0),coalesce(sum(r.status='notified'),0) FROM products p LEFT JOIN reservations r ON r.product_id=p.id WHERE p.reservation_only=1 OR EXISTS(SELECT 1 FROM reservations rr WHERE rr.product_id=p.id) GROUP BY p.id ORDER BY p.sort_order,p.id`)
	if err != nil {
		return page, fmt.Errorf("list reservation counts: %w", err)
	}
	defer counts.Close()
	for counts.Next() {
		var c Count
		if err := counts.Scan(&c.ProductID, &c.ProductName, &c.Total, &c.Waiting, &c.Notified); err != nil {
			return page, fmt.Errorf("scan reservation count: %w", err)
		}
		page.Counts = append(page.Counts, c)
	}
	if err := counts.Err(); err != nil {
		return page, fmt.Errorf("read reservation counts: %w", err)
	}
	return page, nil
}
func (s *Service) List(ctx context.Context, f Filter) (Page, error) { return s.list(ctx, f, false) }
func (s *Service) Export(ctx context.Context, f Filter) ([]Item, error) {
	page, err := s.list(ctx, f, true)
	return page.Items, err
}
func (s *Service) Delete(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM reservations WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete reservation: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count deleted reservations: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Service) Notify(ctx context.Context, productID int64, subject, body string) (int64, error) {
	subject = strings.TrimSpace(subject)
	body = strings.TrimSpace(body)
	if productID <= 0 || subject == "" || utf8.RuneCountInString(subject) > 200 || strings.ContainsAny(subject, "\r\n\x00") || body == "" || utf8.RuneCountInString(body) > 20000 {
		return 0, ErrInvalidNotice
	}
	if s.mail == nil {
		return 0, mail.ErrNotConfigured
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin reservation notification: %w", err)
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM products WHERE id=?)`, productID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("find notification product: %w", err)
	}
	if !exists {
		return 0, ErrProduct
	}
	ready, err := s.mail.ConfiguredTx(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("check notification settings: %w", err)
	}
	if !ready {
		return 0, mail.ErrNotConfigured
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,email FROM reservations WHERE product_id=? AND status='waiting' ORDER BY id`, productID)
	if err != nil {
		return 0, fmt.Errorf("list waiting reservations: %w", err)
	}
	type recipient struct {
		id    int64
		email string
	}
	recipients := []recipient{}
	for rows.Next() {
		var v recipient
		if err := rows.Scan(&v.id, &v.email); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan waiting reservation: %w", err)
		}
		recipients = append(recipients, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read waiting reservations: %w", err)
	}
	rows.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, v := range recipients {
		if err := s.mail.EnqueueTx(ctx, tx, mail.Message{To: []string{v.email}, Subject: subject, Text: body}); err != nil {
			return 0, fmt.Errorf("queue reservation notification: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE reservations SET status='notified',notified_at=? WHERE id=? AND status='waiting'`, now, v.id); err != nil {
			return 0, fmt.Errorf("mark reservation notified: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit reservation notifications: %w", err)
	}
	return int64(len(recipients)), nil
}
