package ticket

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/mail"
	"github.com/DueGin/seat-hub/internal/store"
)

type fakeMail struct{ messages []mail.Message }

func (f *fakeMail) AdminRecipients(context.Context) ([]string, error) {
	return []string{"admin@example.com"}, nil
}
func (f *fakeMail) Enqueue(_ context.Context, m mail.Message) error {
	f.messages = append(f.messages, m)
	return nil
}
func setup(t *testing.T) (*Service, *store.DB, *fakeMail) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{`INSERT INTO users(id,email,password_hash,role,status,created_at,updated_at) VALUES(1,'user@example.com','x','user','active','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),(2,'other@example.com','x','user','active','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),(3,'admin@example.com','x','admin','active','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, `INSERT INTO resources(id,user_id,kind,status,created_at,updated_at) VALUES(1,1,'hosted','active','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),(2,2,'hosted','active','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeMail{}
	return New(db.DB, filepath.Join(t.TempDir(), "attachments"), "https://portal.example.com", f, slog.New(slog.NewTextHandler(io.Discard, nil))), db, f
}

var user = Actor{ID: 1}
var other = Actor{ID: 2}
var admin = Actor{ID: 3, Admin: true}

func create(t *testing.T, s *Service) Detail {
	t.Helper()
	d, err := s.Create(context.Background(), user, Input{Category: "api", Title: "调用问题", Body: "请帮我检查"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func TestOwnershipTransitionsAndReadMarkers(t *testing.T) {
	s, db, _ := setup(t)
	ctx := context.Background()
	d := create(t, s)
	id := d.Ticket.ID
	if d.Ticket.Status != "open" || d.Ticket.Unread {
		t.Fatalf("new ticket: %+v", d.Ticket)
	}
	for _, act := range []func() error{func() error { _, err := s.Get(ctx, other, id); return err }, func() error { _, err := s.Reply(ctx, other, id, ReplyInput{Body: "越权"}); return err }, func() error { _, err := s.SetStatus(ctx, other, id, "closed"); return err }} {
		if err := act(); !errors.Is(err, ErrForbidden) {
			t.Fatalf("ownership: %v", err)
		}
	}
	p, err := s.List(ctx, other, Filter{})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("list isolation: %+v %v", p, err)
	}
	d, err = s.Get(ctx, admin, id)
	if err != nil || d.Ticket.AdminReadMessageID == 0 || d.Ticket.Unread {
		t.Fatalf("admin read: %+v %v", d, err)
	}
	if n, err := s.Unread(ctx, admin); err != nil || n != 1 {
		t.Fatalf("pending queue: %d %v", n, err)
	}
	d, err = s.Reply(ctx, admin, id, ReplyInput{Body: "已经检查，请重试"})
	if err != nil || d.Ticket.Status != "replied" {
		t.Fatalf("admin reply: %+v %v", d, err)
	}
	if n, _ := s.Unread(ctx, user); n != 1 {
		t.Fatalf("user unread=%d", n)
	}
	d, err = s.Get(ctx, user, id)
	if err != nil || d.Ticket.Unread || d.Ticket.UserReadAt == nil {
		t.Fatalf("user read: %+v %v", d, err)
	}
	if n, _ := s.Unread(ctx, user); n != 0 {
		t.Fatalf("read not persisted: %d", n)
	}
	if _, err := s.SetStatus(ctx, user, id, "closed"); err != nil {
		t.Fatal(err)
	}
	d, err = s.Reply(ctx, user, id, ReplyInput{Body: "仍然有问题"})
	if err != nil || d.Ticket.Status != "open" {
		t.Fatalf("reopen: %+v %v", d, err)
	}
	p, err = s.List(ctx, admin, Filter{Status: "open", Category: "api", Query: "仍然", UserID: 1})
	if err != nil || p.Total != 1 || p.Counts["open"] != 1 || !p.Items[0].Unread {
		t.Fatalf("admin filters: %+v %v", p, err)
	}
	for _, status := range []string{"closed", "open", "replied"} {
		if _, err := s.SetStatus(ctx, admin, id, status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetStatus(ctx, user, id, "open"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("user status: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action LIKE 'ticket.%'").Scan(&count); err != nil || count != 4 {
		t.Fatalf("audit=%d %v", count, err)
	}
}
func TestValidationRelatedOwnershipAndEmails(t *testing.T) {
	s, _, m := setup(t)
	ctx := context.Background()
	for _, in := range []Input{{Category: "bad", Title: "x", Body: "x"}, {Category: "api", Title: strings.Repeat("字", 101), Body: "x"}, {Category: "api", Title: "x", Body: strings.Repeat("字", 20001)}, {Category: "api", Title: "x", Body: " "}} {
		if _, err := s.Create(ctx, user, in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("validation: %v", err)
		}
	}
	foreign := int64(2)
	_, err := s.Create(ctx, user, Input{Category: "account", Title: "问题", Body: "正文", AccountID: &foreign})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign account: %v", err)
	}
	_, err = s.Create(ctx, user, Input{Category: "billing", Title: "问题", Body: "正文", OrderNo: "missing"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign order: %v", err)
	}
	own := int64(1)
	d, err := s.Create(ctx, user, Input{Category: "account", Title: "<测试>", Body: strings.Repeat("字", 400), AccountID: &own})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.messages) != 1 || m.messages[0].To[0] != "admin@example.com" || !strings.Contains(m.messages[0].Text, "/admin/tickets?id=") || strings.Contains(m.messages[0].HTML, "<测试>") {
		t.Fatalf("new email: %+v", m.messages)
	}
	if strings.Count(m.messages[0].Text, "字") != 300 {
		t.Fatal("preview not limited")
	}
	_, err = s.Reply(ctx, admin, d.Ticket.ID, ReplyInput{Body: "<script>alert(1)</script>"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.messages) != 2 || m.messages[1].To[0] != "user@example.com" || !strings.Contains(m.messages[1].Text, "/tickets/1") || strings.Contains(m.messages[1].HTML, "<script>") {
		t.Fatalf("reply email: %+v", m.messages)
	}
	_, err = s.Reply(ctx, user, d.Ticket.ID, ReplyInput{Body: "谢谢"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.messages) != 3 || m.messages[2].To[0] != "admin@example.com" {
		t.Fatal("user reply recipient")
	}
}
func pngBytes() []byte {
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jZl8AAAAASUVORK5CYII=")
	return b
}
func upload(t *testing.T, s *Service, a Actor) Attachment {
	t.Helper()
	att, err := s.Upload(context.Background(), a, "../../test.png", bytes.NewReader(pngBytes()))
	if err != nil {
		t.Fatal(err)
	}
	return att
}
func TestAttachmentsBindingLimitsAndCleanup(t *testing.T) {
	s, db, _ := setup(t)
	ctx := context.Background()
	att := upload(t, s, user)
	if att.Name != "test.png" || att.ContentType != "image/png" || len(att.SHA256) != 64 || strings.Contains(att.StoragePath, "test") {
		t.Fatalf("image metadata: %+v", att)
	}
	if _, _, err := s.OpenAttachment(ctx, other, att.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("upload privacy: %v", err)
	}
	d, err := s.Create(ctx, user, Input{Category: "api", Title: "截图", Body: "![截图](" + att.URL + ")", AttachmentIDs: []int64{att.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Messages[0].Attachments) != 1 {
		t.Fatal("attachment missing")
	}
	for _, ids := range [][]int64{{att.ID}, {att.ID, att.ID}, {1, 2, 3, 4, 5, 6, 7}} {
		if _, err := s.Reply(ctx, user, d.Ticket.ID, ReplyInput{Body: "正文", AttachmentIDs: ids}); !errors.Is(err, ErrAttachment) {
			t.Fatalf("reuse/max: %v", err)
		}
	}
	foreign := upload(t, s, other)
	if _, err := s.Reply(ctx, user, d.Ticket.ID, ReplyInput{Body: "正文", AttachmentIDs: []int64{foreign.ID}}); !errors.Is(err, ErrAttachment) {
		t.Fatalf("uploader: %v", err)
	}
	ownAdmin := upload(t, s, admin)
	if _, err := s.Reply(ctx, admin, d.Ticket.ID, ReplyInput{Body: "管理员图片", AttachmentIDs: []int64{ownAdmin.ID}}); err != nil {
		t.Fatal(err)
	}
	_, f, err := s.OpenAttachment(ctx, user, ownAdmin.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	otherTicket := create(t, s)
	if _, err := s.Reply(ctx, user, otherTicket.Ticket.ID, ReplyInput{Body: "![](" + att.URL + ")"}); !errors.Is(err, ErrAttachment) {
		t.Fatalf("cross ticket reference: %v", err)
	}
	for _, body := range [][]byte{nil, bytes.Repeat([]byte("x"), MaxAttachmentSize+1)} {
		if _, err := s.Upload(ctx, user, "x.png", bytes.NewReader(body)); !errors.Is(err, ErrImageSize) {
			t.Fatalf("size: %v", err)
		}
	}
	for _, body := range []string{"<svg xmlns='http://www.w3.org/2000/svg'></svg>", "<html>png</html>"} {
		if _, err := s.Upload(ctx, user, "x.png", strings.NewReader(body)); !errors.Is(err, ErrImageType) {
			t.Fatalf("type: %v", err)
		}
	}
	if _, err := s.Upload(ctx, user, "max.png", bytes.NewReader(append(pngBytes(), make([]byte, MaxAttachmentSize-len(pngBytes()))...))); err != nil {
		t.Fatalf("max size: %v", err)
	}
	expired := upload(t, s, user)
	if _, err := db.Exec("UPDATE ticket_attachments SET created_at=? WHERE id IN (?,?)", time.Now().Add(-25*time.Hour).UTC().Format(time.RFC3339Nano), expired.ID, att.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reply(ctx, user, d.Ticket.ID, ReplyInput{Body: "正文", AttachmentIDs: []int64{expired.ID}}); !errors.Is(err, ErrAttachment) {
		t.Fatalf("expired binding: %v", err)
	}
	n, err := s.Cleanup(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, expired.StoragePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file not removed: %v", err)
	}
	_, f, err = s.OpenAttachment(ctx, user, att.ID)
	if err != nil {
		t.Fatalf("bound image removed: %v", err)
	}
	f.Close()
	var messages int
	if err := db.QueryRow("SELECT COUNT(*) FROM ticket_messages WHERE ticket_id=?", d.Ticket.ID).Scan(&messages); err != nil || messages != 2 {
		t.Fatalf("invalid reply did not roll back: %d %v", messages, err)
	}
}

func TestAttachmentReferenceLimitAndImageSniffing(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	d := create(t, s)
	var refs strings.Builder
	for i := 0; i < 7; i++ {
		att := upload(t, s, user)
		_, err := s.Reply(ctx, user, d.Ticket.ID, ReplyInput{Body: "图片", AttachmentIDs: []int64{att.ID}})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&refs, "![截图](%s)\n", att.URL)
	}
	if _, err := s.Reply(ctx, user, d.Ticket.ID, ReplyInput{Body: refs.String()}); !errors.Is(err, ErrAttachment) {
		t.Fatalf("too many references: %v", err)
	}
	for _, fixture := range []struct {
		body        []byte
		contentType string
	}{{[]byte("GIF89a"), "image/gif"}, {[]byte{0xff, 0xd8, 0xff, 0xe0}, "image/jpeg"}, {[]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), "image/webp"}} {
		att, err := s.Upload(ctx, user, "fake.html", bytes.NewReader(fixture.body))
		if err != nil || att.ContentType != fixture.contentType {
			t.Fatalf("sniffing %s: %+v %v", fixture.contentType, att, err)
		}
	}
	if _, err := s.Create(ctx, user, Input{Category: "api", Title: "\x00", Body: "正文"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("title NUL: %v", err)
	}
	if _, err := s.Reply(ctx, user, d.Ticket.ID, ReplyInput{Body: "\x00"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("body NUL: %v", err)
	}
}
