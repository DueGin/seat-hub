package reservation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DueGin/seat-hub/internal/catalog"
	"github.com/DueGin/seat-hub/internal/mail"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
)

func fixture(t *testing.T) (*Service, *mail.Service, int64) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := (&store.DB{DB: db}).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	mailer := mail.New(db, box, nil)
	product, err := catalog.New(db, nil).Create(context.Background(), catalog.Product{Kind: "rental_account", Name: "预约商品", UnitQuantity: 1, DurationDays: 30, PriceCents: 100, MinQty: 1, MaxQty: 10, Enabled: true, ReservationOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return New(db, mailer), mailer, product.ID
}
func configure(t *testing.T, m *mail.Service) {
	t.Helper()
	if _, err := m.SaveSettings(context.Background(), mail.Settings{Enabled: true, Host: "localhost", Port: 25, Security: "none", FromAddress: "sender@example.com"}); err != nil {
		t.Fatal(err)
	}
}
func TestCreateDedupeConfirmationAndNotify(t *testing.T) {
	s, m, id := fixture(t)
	ctx := context.Background()
	configure(t, m)
	if _, err := s.db.Exec(`INSERT INTO users(id,email,password_hash,created_at,updated_at) VALUES (1,'user@example.com','','','')`); err != nil {
		t.Fatal(err)
	}
	userID := int64(1)
	if err := s.Create(ctx, id, &userID, " USER@Example.com ", "季度方案", "ip-one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, id, nil, "user@example.com", "duplicate", "ip-two"); err != nil {
		t.Fatal(err)
	}
	page, err := s.List(ctx, Filter{})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Email != "user@example.com" || page.Items[0].Note != "季度方案" || page.Items[0].UserID == nil || *page.Items[0].UserID != 1 || page.Counts[0].Waiting != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM mail_outbox`).Scan(&n)
	if n != 1 {
		t.Fatalf("confirmation count=%d", n)
	}
	notified, err := s.Notify(ctx, id, "商品开售通知", "欢迎购买")
	if err != nil || notified != 1 {
		t.Fatalf("notify=%d err=%v", notified, err)
	}
	notified, err = s.Notify(ctx, id, "商品开售通知", "欢迎购买")
	if err != nil || notified != 0 {
		t.Fatalf("repeat notify=%d err=%v", notified, err)
	}
	s.db.QueryRow(`SELECT count(*) FROM mail_outbox`).Scan(&n)
	if n != 2 {
		t.Fatalf("outbox count=%d", n)
	}
	page, err = s.List(ctx, Filter{Status: "notified", Email: "USER@"})
	if err != nil || page.Total != 1 || page.Items[0].NotifiedAt == nil || page.Counts[0].Notified != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	exported, err := s.Export(ctx, Filter{ProductID: id})
	if err != nil || len(exported) != 1 {
		t.Fatalf("export=%v err=%v", exported, err)
	}
	if err := s.Delete(ctx, page.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, page.Items[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
func TestDisabledMailAndNotifyRollback(t *testing.T) {
	s, m, id := fixture(t)
	ctx := context.Background()
	if err := s.Create(ctx, id, nil, "one@example.com", "", "one"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Notify(ctx, id, "开售", "内容"); n != 0 || !errors.Is(err, mail.ErrNotConfigured) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	page, err := s.List(ctx, Filter{})
	if err != nil || page.Counts[0].Waiting != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	configure(t, m)
	// Simulate a database failure after queue insertion; both effects must roll back.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_notification BEFORE UPDATE ON reservations BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Notify(ctx, id, "开售", "内容"); err == nil {
		t.Fatal("expected transaction failure")
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM mail_outbox`).Scan(&n)
	if n != 0 {
		t.Fatalf("queue survived rollback: %d", n)
	}
	page, err = s.List(ctx, Filter{})
	if err != nil || page.Counts[0].Waiting != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}
func TestValidationVisibilityAndRateLimit(t *testing.T) {
	s, _, id := fixture(t)
	ctx := context.Background()
	for _, input := range []struct{ email, note string }{{"bad", ""}, {"Name <one@example.com>", ""}, {"one@example.com", strings.Repeat("字", 201)}} {
		if err := s.Create(ctx, id, nil, input.email, input.note, "invalid"); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"enabled=0", "reservation_only=0"} {
		if _, err := s.db.Exec(`UPDATE products SET `+field+` WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if err := s.Create(ctx, id, nil, "one@example.com", "", "visibility"); !errors.Is(err, ErrProduct) {
			t.Fatal(err)
		}
		s.db.Exec(`UPDATE products SET enabled=1,reservation_only=1 WHERE id=?`, id)
	}
	for range 10 {
		if err := s.Create(ctx, id, nil, "one@example.com", "", "limited"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Create(ctx, id, nil, "two@example.com", "", "limited"); !errors.Is(err, ErrRateLimit) {
		t.Fatal(err)
	}
	if err := s.Create(ctx, id, nil, "two@example.com", "", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(ctx, Filter{Status: "bad"}); !errors.Is(err, ErrInvalidFilter) {
		t.Fatal(err)
	}
	if _, err := s.Notify(ctx, id, "bad\r\nSubject: injected", "body"); !errors.Is(err, ErrInvalidNotice) {
		t.Fatal(err)
	}
}

func TestConcurrentDedupe(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "reservations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	products := catalog.New(database.DB, nil)
	product, err := products.Create(context.Background(), catalog.Product{Kind: "concurrency", Name: "预约", UnitQuantity: 1, DurationDays: 30, MinQty: 1, MaxQty: 1, Enabled: true, ReservationOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	s := New(database.DB, nil)
	result := make(chan error, 8)
	for i := range 8 {
		go func(i int) {
			result <- s.Create(context.Background(), product.ID, nil, "same@example.com", "", fmt.Sprint(i))
		}(i)
	}
	for range 8 {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.List(context.Background(), Filter{})
	if err != nil || page.Total != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}
