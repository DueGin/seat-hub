// Package ticket owns support conversations and their private image attachments.
package ticket

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DueGin/seat-hub/internal/mail"
)

const MaxAttachmentSize = 5 << 20
const MaxAttachments = 6

var (
	ErrNotFound   = errors.New("ticket not found")
	ErrForbidden  = errors.New("ticket access forbidden")
	ErrInvalid    = errors.New("invalid ticket input")
	ErrAttachment = errors.New("invalid attachment binding")
	ErrImageType  = errors.New("unsupported image type")
	ErrImageSize  = errors.New("image too large or empty")
)

type Actor struct {
	ID    int64
	Admin bool
}
type Input struct {
	Category      string  `json:"category"`
	Title         string  `json:"title"`
	Body          string  `json:"body"`
	OrderNo       string  `json:"order_no,omitempty"`
	AccountID     *int64  `json:"account_id,omitempty"`
	AttachmentIDs []int64 `json:"attachment_ids,omitempty"`
}
type ReplyInput struct {
	Body          string  `json:"body"`
	AttachmentIDs []int64 `json:"attachment_ids,omitempty"`
}
type Ticket struct {
	ID                 int64   `json:"id"`
	UserID             int64   `json:"user_id"`
	UserEmail          string  `json:"user_email,omitempty"`
	Category           string  `json:"category"`
	Title              string  `json:"title"`
	Status             string  `json:"status"`
	OrderNo            string  `json:"order_no,omitempty"`
	AccountID          *int64  `json:"account_id,omitempty"`
	CreatedAt          string  `json:"created_at"`
	UpdatedAt          string  `json:"updated_at"`
	LastMessageAt      string  `json:"last_message_at"`
	LastMessageRole    string  `json:"last_message_role"`
	UserReadMessageID  int64   `json:"user_read_message_id"`
	AdminReadMessageID int64   `json:"admin_read_message_id"`
	UserReadAt         *string `json:"user_read_at"`
	AdminReadAt        *string `json:"admin_read_at"`
	Unread             bool    `json:"unread"`
}
type Message struct {
	ID          int64        `json:"id"`
	TicketID    int64        `json:"ticket_id"`
	AuthorID    int64        `json:"author_id"`
	Role        string       `json:"role"`
	Body        string       `json:"body"`
	CreatedAt   string       `json:"created_at"`
	Attachments []Attachment `json:"attachments"`
}
type Attachment struct {
	ID          int64  `json:"id"`
	UploaderID  int64  `json:"-"`
	TicketID    *int64 `json:"ticket_id,omitempty"`
	MessageID   *int64 `json:"message_id,omitempty"`
	URL         string `json:"url"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"-"`
	StoragePath string `json:"-"`
	CreatedAt   string `json:"-"`
}
type Detail struct {
	Ticket   Ticket    `json:"ticket"`
	Messages []Message `json:"messages"`
}
type Filter struct {
	Status, Category, Query string
	UserID                  int64
	Page, PageSize          int
}
type Page struct {
	Items    []Ticket         `json:"items"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Counts   map[string]int64 `json:"counts,omitempty"`
}
type Mailer interface {
	Enqueue(context.Context, mail.Message) error
	AdminRecipients(context.Context) ([]string, error)
}
type Service struct {
	db           *sql.DB
	dir, baseURL string
	mail         Mailer
	log          *slog.Logger
}

func New(db *sql.DB, dir, baseURL string, mailer Mailer, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, dir: dir, baseURL: strings.TrimRight(baseURL, "/"), mail: mailer, log: logger}
}
func validCategory(v string) bool {
	return v == "account" || v == "billing" || v == "api" || v == "other"
}
func validStatus(v string) bool { return v == "open" || v == "replied" || v == "closed" }
func validBody(v string) bool {
	return utf8.ValidString(v) && !strings.ContainsRune(v, 0) && strings.TrimSpace(v) != "" && utf8.RuneCountInString(v) <= 20000
}

// Fixed precision preserves chronological order in SQLite text comparisons.
const timestampLayout = "2006-01-02T15:04:05.000000000Z"

func now() string { return time.Now().UTC().Format(timestampLayout) }
func (s *Service) Create(ctx context.Context, a Actor, in Input) (Detail, error) {
	in.Title = strings.TrimSpace(in.Title)
	if a.ID <= 0 || !validCategory(in.Category) || !utf8.ValidString(in.Title) || utf8.RuneCountInString(in.Title) > 100 || in.Title == "" || strings.ContainsAny(in.Title, "\x00\r\n") || !validBody(in.Body) {
		return Detail{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Detail{}, fmt.Errorf("begin ticket: %w", err)
	}
	defer tx.Rollback()
	if in.OrderNo != "" {
		var owner int64
		if err := tx.QueryRowContext(ctx, "SELECT user_id FROM orders WHERE order_no=?", in.OrderNo).Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Detail{}, ErrForbidden
			}
			return Detail{}, fmt.Errorf("lookup order: %w", err)
		}
		if owner != a.ID {
			return Detail{}, ErrForbidden
		}
	}
	if in.AccountID != nil {
		var owner int64
		if err := tx.QueryRowContext(ctx, "SELECT user_id FROM resources WHERE id=?", *in.AccountID).Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return Detail{}, ErrForbidden
			}
			return Detail{}, fmt.Errorf("lookup account: %w", err)
		}
		if owner != a.ID {
			return Detail{}, ErrForbidden
		}
	}
	stamp := now()
	result, err := tx.ExecContext(ctx, `INSERT INTO tickets(user_id,category,title,status,order_no,account_id,created_at,updated_at,last_message_at,last_message_role) VALUES(?,?,?,'open',NULLIF(?,''),?,?,?,?,'user')`, a.ID, in.Category, in.Title, in.OrderNo, in.AccountID, stamp, stamp, stamp)
	if err != nil {
		return Detail{}, fmt.Errorf("create ticket: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Detail{}, fmt.Errorf("ticket id: %w", err)
	}
	// Creating a ticket is always a user action, including an administrator's own ticket.
	if err := s.addMessage(ctx, tx, Actor{ID: a.ID}, id, in.Body, in.AttachmentIDs, stamp); err != nil {
		return Detail{}, err
	}
	if err := tx.Commit(); err != nil {
		return Detail{}, fmt.Errorf("commit ticket: %w", err)
	}
	d, err := s.view(ctx, Actor{ID: a.ID}, id, false)
	if err == nil {
		s.notify(ctx, d.Ticket, in.Body, false)
	}
	return d, err
}
func (s *Service) Reply(ctx context.Context, a Actor, id int64, in ReplyInput) (Detail, error) {
	if !validBody(in.Body) {
		return Detail{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Detail{}, fmt.Errorf("begin reply: %w", err)
	}
	defer tx.Rollback()
	t, err := lookup(ctx, tx, a, id)
	if err != nil {
		return Detail{}, err
	}
	if err := s.addMessage(ctx, tx, a, id, in.Body, in.AttachmentIDs, now()); err != nil {
		return Detail{}, err
	}
	if a.Admin {
		if err := audit(ctx, tx, a.ID, "ticket.reply", id, map[string]any{"status": "replied"}); err != nil {
			return Detail{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Detail{}, fmt.Errorf("commit reply: %w", err)
	}
	d, err := s.view(ctx, a, id, false)
	if err == nil {
		s.notify(ctx, t, in.Body, a.Admin)
	}
	return d, err
}
func (s *Service) SetStatus(ctx context.Context, a Actor, id int64, status string) (Ticket, error) {
	if !validStatus(status) || (!a.Admin && status != "closed") {
		return Ticket{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, fmt.Errorf("begin status: %w", err)
	}
	defer tx.Rollback()
	if _, err := lookup(ctx, tx, a, id); err != nil {
		return Ticket{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE tickets SET status=?,updated_at=? WHERE id=?", status, now(), id); err != nil {
		return Ticket{}, fmt.Errorf("update status: %w", err)
	}
	if a.Admin {
		if err := audit(ctx, tx, a.ID, "ticket.status", id, map[string]string{"status": status}); err != nil {
			return Ticket{}, err
		}
	}
	t, err := lookup(ctx, tx, a, id)
	if err != nil {
		return Ticket{}, err
	}
	if err := tx.Commit(); err != nil {
		return Ticket{}, fmt.Errorf("commit status: %w", err)
	}
	return t, nil
}
func audit(ctx context.Context, tx *sql.Tx, actor int64, action string, id int64, detail any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode audit: %w", err)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO audit_log(actor_id,action,target,detail,created_at) VALUES(?,?,?,?,?)", actor, action, strconv.FormatInt(id, 10), string(raw), now())
	if err != nil {
		return fmt.Errorf("write audit: %w", err)
	}
	return nil
}

var imageRef = regexp.MustCompile(`/api/tickets/attachments/([0-9]+)`)

func (s *Service) addMessage(ctx context.Context, tx *sql.Tx, a Actor, id int64, body string, ids []int64, stamp string) error {
	if len(ids) > MaxAttachments {
		return ErrAttachment
	}
	seen := map[int64]bool{}
	for _, attachmentID := range ids {
		if attachmentID <= 0 || seen[attachmentID] {
			return ErrAttachment
		}
		seen[attachmentID] = true
	}
	// A reference may use a new upload in this message, or an image already in this ticket.
	for _, ref := range imageRef.FindAllStringSubmatch(body, -1) {
		imageID, err := strconv.ParseInt(ref[1], 10, 64)
		if err != nil {
			return ErrAttachment
		}
		if seen[imageID] {
			continue
		}
		seen[imageID] = true
		if len(seen) > MaxAttachments {
			return ErrAttachment
		}
		var ticketID sql.NullInt64
		err = tx.QueryRowContext(ctx, "SELECT ticket_id FROM ticket_attachments WHERE id=?", imageID).Scan(&ticketID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (!ticketID.Valid || ticketID.Int64 != id) {
			return ErrAttachment
		}
		if err != nil {
			return fmt.Errorf("check image reference: %w", err)
		}
	}
	role, status, marker := "user", "open", "user"
	if a.Admin {
		role, status, marker = "admin", "replied", "admin"
	}
	result, err := tx.ExecContext(ctx, "INSERT INTO ticket_messages(ticket_id,author_id,role,body,created_at) VALUES(?,?,?,?,?)", id, a.ID, role, body, stamp)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("message id: %w", err)
	}
	for _, attachmentID := range ids {
		result, err := tx.ExecContext(ctx, "UPDATE ticket_attachments SET ticket_id=?,message_id=? WHERE id=? AND uploader_id=? AND ticket_id IS NULL AND created_at>?", id, messageID, attachmentID, a.ID, time.Now().Add(-24*time.Hour).UTC().Format(timestampLayout))
		if err != nil {
			return fmt.Errorf("bind attachment: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("attachment affected: %w", err)
		}
		if affected != 1 {
			return ErrAttachment
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE tickets SET status=?,updated_at=?,last_message_at=?,last_message_role=?,"+marker+"_read_message_id=?,"+marker+"_read_at=? WHERE id=?", status, stamp, stamp, role, messageID, stamp, id)
	if err != nil {
		return fmt.Errorf("update conversation: %w", err)
	}
	return nil
}
func (s *Service) notify(ctx context.Context, t Ticket, body string, admin bool) {
	if s.mail == nil {
		return
	}
	recipients := []string{}
	link := s.baseURL + "/admin/tickets?id=" + strconv.FormatInt(t.ID, 10)
	subject := "工单待处理：" + t.Title
	if admin {
		recipients = []string{t.UserEmail}
		link = s.baseURL + "/tickets/" + strconv.FormatInt(t.ID, 10)
		subject = "工单已回复：" + t.Title
	} else {
		var err error
		recipients, err = s.mail.AdminRecipients(ctx)
		if err != nil {
			s.log.Warn("ticket mail recipients", "err", err)
			return
		}
	}
	if len(recipients) == 0 {
		return
	}
	preview := []rune(body)
	if len(preview) > 300 {
		preview = append(preview[:300], []rune("…")...)
	}
	text := t.Title + "\n\n" + string(preview) + "\n\n查看工单：" + link
	markup := "<h2>" + html.EscapeString(t.Title) + "</h2><p>" + strings.ReplaceAll(html.EscapeString(string(preview)), "\n", "<br>") + "</p><p><a href=\"" + html.EscapeString(link) + "\">查看工单</a></p>"
	if err := s.mail.Enqueue(ctx, mail.Message{To: recipients, Subject: subject, Text: text, HTML: markup}); err != nil {
		s.log.Warn("enqueue ticket mail", "err", err)
	}
}
func (s *Service) Upload(ctx context.Context, a Actor, name string, reader io.Reader) (Attachment, error) {
	if a.ID <= 0 {
		return Attachment{}, ErrForbidden
	}
	body, err := io.ReadAll(io.LimitReader(reader, MaxAttachmentSize+1))
	if err != nil {
		return Attachment{}, fmt.Errorf("read image: %w", err)
	}
	if len(body) == 0 || len(body) > MaxAttachmentSize {
		return Attachment{}, ErrImageSize
	}
	contentType := http.DetectContentType(body)
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return Attachment{}, ErrImageType
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Attachment{}, fmt.Errorf("create attachments directory: %w", err)
	}
	token := make([]byte, 24)
	if _, err := rand.Read(token); err != nil {
		return Attachment{}, fmt.Errorf("random attachment name: %w", err)
	}
	path := hex.EncodeToString(token)
	file, err := os.OpenFile(filepath.Join(s.dir, path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Attachment{}, fmt.Errorf("create image: %w", err)
	}
	_, writeErr := file.Write(body)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(filepath.Join(s.dir, path))
		return Attachment{}, fmt.Errorf("write image: %w", err)
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < ' ' || r == 127 {
			return -1
		}
		return r
	}, name)
	name = string([]rune(name)[:min(200, utf8.RuneCountInString(name))])
	if name == "" || name == "." {
		name = "图片"
	}
	hash := sha256.Sum256(body)
	aFile := Attachment{UploaderID: a.ID, Name: name, Size: int64(len(body)), ContentType: contentType, SHA256: hex.EncodeToString(hash[:]), StoragePath: path, CreatedAt: now()}
	result, err := s.db.ExecContext(ctx, "INSERT INTO ticket_attachments(uploader_id,name,content_type,size,sha256,storage_path,created_at) VALUES(?,?,?,?,?,?,?)", a.ID, name, contentType, aFile.Size, aFile.SHA256, path, aFile.CreatedAt)
	if err != nil {
		_ = os.Remove(filepath.Join(s.dir, path))
		return Attachment{}, fmt.Errorf("save image: %w", err)
	}
	aFile.ID, err = result.LastInsertId()
	if err != nil {
		return Attachment{}, fmt.Errorf("attachment id: %w", err)
	}
	aFile.URL = attachmentURL(aFile.ID)
	return aFile, nil
}
func attachmentURL(id int64) string { return "/api/tickets/attachments/" + strconv.FormatInt(id, 10) }
func (s *Service) OpenAttachment(ctx context.Context, a Actor, id int64) (Attachment, *os.File, error) {
	row := s.db.QueryRowContext(ctx, `SELECT a.id,a.uploader_id,a.ticket_id,a.message_id,a.name,a.content_type,a.size,a.sha256,a.storage_path,a.created_at,COALESCE(t.user_id,a.uploader_id) FROM ticket_attachments a LEFT JOIN tickets t ON t.id=a.ticket_id WHERE a.id=?`, id)
	var att Attachment
	var owner int64
	if err := row.Scan(&att.ID, &att.UploaderID, &att.TicketID, &att.MessageID, &att.Name, &att.ContentType, &att.Size, &att.SHA256, &att.StoragePath, &att.CreatedAt, &owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return att, nil, ErrNotFound
		}
		return att, nil, fmt.Errorf("get attachment: %w", err)
	}
	if !a.Admin && owner != a.ID {
		return Attachment{}, nil, ErrForbidden
	}
	file, err := os.Open(filepath.Join(s.dir, att.StoragePath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Attachment{}, nil, ErrNotFound
		}
		return Attachment{}, nil, fmt.Errorf("open image: %w", err)
	}
	att.URL = attachmentURL(att.ID)
	return att, file, nil
}

// Cleanup claims and removes expired unbound uploads within one write transaction.
func (s *Service) Cleanup(ctx context.Context, at time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin image cleanup: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT id,storage_path FROM ticket_attachments WHERE ticket_id IS NULL AND created_at<=?", at.Add(-24*time.Hour).UTC().Format(timestampLayout))
	if err != nil {
		return 0, fmt.Errorf("find old images: %w", err)
	}
	type item struct {
		id   int64
		path string
	}
	items := []item{}
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.path); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan old image: %w", err)
		}
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("read old images: %w", err)
	}
	for _, v := range items {
		if _, err := tx.ExecContext(ctx, "DELETE FROM ticket_attachments WHERE id=? AND ticket_id IS NULL", v.id); err != nil {
			return 0, fmt.Errorf("delete old image: %w", err)
		}
		if err := os.Remove(filepath.Join(s.dir, v.path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("remove old image: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit image cleanup: %w", err)
	}
	return len(items), nil
}

// RunCleanup ends when the context or database closes; the caller owns its lifetime.
func (s *Service) RunCleanup(ctx context.Context) {
	timer := time.NewTicker(time.Hour)
	defer timer.Stop()
	for {
		if err := s.db.PingContext(ctx); err != nil {
			return
		}
		if _, err := s.Cleanup(ctx, time.Now()); err != nil {
			s.log.Warn("ticket attachment cleanup", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
