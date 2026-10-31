package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DueGin/vm2api-portal/internal/store/migrations"
	_ "modernc.org/sqlite"
)

type serviceFixture struct {
	db    *sql.DB
	svc   *Service
	now   time.Time
	calls atomic.Int64
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	schema, err := migrations.FS.ReadFile("001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	f := &serviceFixture{db: db, now: time.Date(2026, 10, 31, 16, 0, 0, 0, time.FixedZone("local", 8*3600))}
	timestamp := f.now.UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO users (id, email, password_hash, created_at, updated_at) VALUES (1, 'buyer@example.com', 'hash', ?, ?)`, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO products (id, kind, name, unit_quantity, duration_days, price_cents, created_at, updated_at)
		VALUES (1, 'concurrency', '并发包', 1, 30, 123, ?, ?)`, timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	f.addOrder(t, "PT1", "epay")
	mock, err := NewMock(true)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(db, PaidMarkerFunc(func(ctx context.Context, orderID int64, trade string) error {
		f.calls.Add(1)
		_, err := db.ExecContext(ctx, `UPDATE orders SET status = 'paid', provider_trade_no = ?, paid_at = ?, updated_at = ? WHERE id = ? AND status = 'pending'`, trade, timestamp, timestamp, orderID)
		return err
	}), ServiceOptions{Providers: map[string]Provider{"epay": testEPay(t), "manual": NewManual("请转账"), "mock": mock}, Dev: true, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func (f *serviceFixture) addOrder(t *testing.T, orderNo, provider string) {
	t.Helper()
	now := f.now.UTC().Format(time.RFC3339)
	expires := f.now.Add(30 * time.Minute).UTC().Format(time.RFC3339)
	if _, err := f.db.Exec(`INSERT INTO orders (order_no, user_id, product_id, product_snapshot, kind, quantity, units, duration_days,
		amount_cents, provider, status, created_at, expires_at, updated_at)
		VALUES (?, 1, 1, '{}', 'concurrency', 1, 1, 30, 123, ?, 'pending', ?, ?, ?)`, orderNo, provider, now, expires, now); err != nil {
		t.Fatal(err)
	}
}

func (f *serviceFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *serviceFixture) assertEvent(t *testing.T, result string, verified bool) {
	t.Helper()
	var gotResult, created string
	var gotVerified bool
	if err := f.db.QueryRow(`SELECT result, verified, created_at FROM payment_events ORDER BY id DESC LIMIT 1`).Scan(&gotResult, &gotVerified, &created); err != nil {
		t.Fatal(err)
	}
	if gotResult != result || gotVerified != verified || created != f.now.UTC().Format(time.RFC3339) {
		t.Fatalf("event = %q, verified %v, at %q; want %q, verified %v", gotResult, gotVerified, created, result, verified)
	}
}

func TestServiceCreate(t *testing.T) {
	f := newServiceFixture(t)
	payment, err := f.svc.Create(context.Background(), "epay", testCreateRequest())
	if err != nil || payment.Type != "redirect" {
		t.Fatalf("create = %+v, %v", payment, err)
	}
	for _, provider := range []string{"manual", "mock"} {
		f.addOrder(t, provider, provider)
		req := testCreateRequest()
		req.OrderNo = provider
		payment, err := f.svc.Create(context.Background(), provider, req)
		if err != nil || payment.Type != provider {
			t.Fatalf("create %s = %+v, %v", provider, payment, err)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("creating payment marked order paid")
	}
}

func TestServiceCreateRejectsUnpayable(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		setup    func(*serviceFixture, *CreateRequest)
		want     error
	}{
		{"unknown provider", "unknown", nil, ErrProviderUnavailable},
		{"provider mismatch", "manual", nil, ErrProviderUnavailable},
		{"wrong amount", "epay", func(_ *serviceFixture, r *CreateRequest) { r.AmountCents++ }, ErrAmountMismatch},
		{"invalid request", "epay", func(_ *serviceFixture, r *CreateRequest) { r.ProductName = "" }, ErrInvalidRequest},
		{"order missing", "epay", func(_ *serviceFixture, r *CreateRequest) { r.OrderNo = "missing" }, ErrOrderNotFound},
		{"currency mismatch", "epay", func(f *serviceFixture, _ *CreateRequest) { f.exec(t, `UPDATE orders SET currency = 'USD'`) }, ErrAmountMismatch},
		{"cancelled", "epay", func(f *serviceFixture, _ *CreateRequest) { f.exec(t, `UPDATE orders SET status = 'cancelled'`) }, ErrOrderNotPayable},
		{"paid", "epay", func(f *serviceFixture, _ *CreateRequest) { f.exec(t, `UPDATE orders SET status = 'paid'`) }, ErrOrderNotPayable},
		{"expired timestamp", "epay", func(f *serviceFixture, _ *CreateRequest) {
			f.exec(t, `UPDATE orders SET expires_at = ?`, f.now.UTC().Format(time.RFC3339))
		}, ErrOrderNotPayable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixture(t)
			req := testCreateRequest()
			if tc.setup != nil {
				tc.setup(f, &req)
			}
			if _, err := f.svc.Create(context.Background(), tc.provider, req); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v; want %v", err, tc.want)
			}
		})
	}
}

func TestServiceNotifyAppliesThenDuplicates(t *testing.T) {
	f := newServiceFixture(t)
	params := testNoticeParams()
	notice, err := f.svc.Notify(context.Background(), "epay", signedRequest(params, http.MethodGet))
	if err != nil || notice.AmountCents != 123 || !notice.Paid {
		t.Fatalf("notify = %+v, %v", notice, err)
	}
	f.assertEvent(t, "applied", true)
	var status, tradeNo, paidAt, raw string
	if err := f.db.QueryRow(`SELECT status, provider_trade_no, paid_at FROM orders WHERE order_no = 'PT1'`).Scan(&status, &tradeNo, &paidAt); err != nil {
		t.Fatal(err)
	}
	if status != "paid" || tradeNo != "gateway-1" || paidAt != f.now.UTC().Format(time.RFC3339) {
		t.Fatalf("order = %s, %s, %s", status, tradeNo, paidAt)
	}
	if err := f.db.QueryRow(`SELECT raw FROM payment_events`).Scan(&raw); err != nil || raw != params.Encode() {
		t.Fatalf("event raw = %q, error = %v", raw, err)
	}
	for _, status := range []string{"paid", "fulfilled", "fulfill_failed"} {
		f.exec(t, `UPDATE orders SET status = ?, expires_at = ?`, status, f.now.Add(-time.Hour).UTC().Format(time.RFC3339))
		if _, err := f.svc.Notify(context.Background(), "epay", signedRequest(testNoticeParams(), http.MethodPost)); err != nil {
			t.Fatalf("duplicate on %s: %v", status, err)
		}
		f.assertEvent(t, "duplicate", true)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM payment_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 || count != 4 {
		t.Fatalf("marker calls = %d, events = %d; want 1 and 4", f.calls.Load(), count)
	}
}

func TestServiceNotifyRejects(t *testing.T) {
	cases := []struct {
		name, result string
		setup        func(*serviceFixture)
		mutate       func(*http.Request)
		want         error
		verified     bool
	}{
		{"bad signature", "invalid_signature", nil, func(r *http.Request) { r.URL.RawQuery += "0" }, ErrInvalidSignature, false},
		{"wrong amount", "amount_mismatch", func(f *serviceFixture) { f.exec(t, `UPDATE orders SET amount_cents = 124`) }, nil, ErrAmountMismatch, true},
		{"wrong currency", "amount_mismatch", func(f *serviceFixture) { f.exec(t, `UPDATE orders SET currency = 'USD'`) }, nil, ErrAmountMismatch, true},
		{"wrong provider", "provider_unavailable", func(f *serviceFixture) { f.exec(t, `UPDATE orders SET provider = 'manual'`) }, nil, ErrProviderUnavailable, true},
		{"order missing", "order_not_found", func(f *serviceFixture) { f.exec(t, `DELETE FROM orders`) }, nil, ErrOrderNotFound, true},
		{"cancelled", "order_not_payable", func(f *serviceFixture) { f.exec(t, `UPDATE orders SET status = 'cancelled'`) }, nil, ErrOrderNotPayable, true},
		{"expired", "order_not_payable", func(f *serviceFixture) {
			f.exec(t, `UPDATE orders SET expires_at = ?`, f.now.UTC().Format(time.RFC3339))
		}, nil, ErrOrderNotPayable, true},
		{"refunded", "order_not_payable", func(f *serviceFixture) { f.exec(t, `UPDATE orders SET status = 'refunded'`) }, nil, ErrOrderNotPayable, true},
		{"different trade", "trade_conflict", func(f *serviceFixture) {
			f.exec(t, `UPDATE orders SET status = 'paid', provider_trade_no = 'other-trade'`)
		}, nil, ErrTradeConflict, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			r := signedRequest(testNoticeParams(), http.MethodGet)
			if tc.mutate != nil {
				tc.mutate(r)
			}
			if _, err := f.svc.Notify(context.Background(), "epay", r); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			f.assertEvent(t, "rejected:"+tc.result, tc.verified)
			if f.calls.Load() != 0 {
				t.Fatal("rejected notification marked paid")
			}
		})
	}
}

func TestServiceNotifyVerifiedRejections(t *testing.T) {
	f := newServiceFixture(t)
	params := testNoticeParams()
	params.Set("pid", "attacker")
	if _, err := f.svc.Notify(context.Background(), "epay", signedRequest(params, http.MethodGet)); !errors.Is(err, ErrMerchantMismatch) {
		t.Fatalf("wrong merchant error = %v", err)
	}
	f.assertEvent(t, "rejected:merchant_mismatch", true)
	params = testNoticeParams()
	params.Set("trade_status", "WAIT_BUYER_PAY")
	if _, err := f.svc.Notify(context.Background(), "epay", signedRequest(params, http.MethodGet)); !errors.Is(err, ErrNotPaid) {
		t.Fatalf("unpaid status error = %v", err)
	}
	f.assertEvent(t, "rejected:not_paid", true)
	if f.calls.Load() != 0 {
		t.Fatal("invalid notification marked paid")
	}
}

func TestServiceTradeCannotPayTwoOrders(t *testing.T) {
	f := newServiceFixture(t)
	f.addOrder(t, "PT2", "epay")
	if _, err := f.svc.Notify(context.Background(), "epay", signedRequest(testNoticeParams(), http.MethodGet)); err != nil {
		t.Fatal(err)
	}
	params := testNoticeParams()
	params.Set("out_trade_no", "PT2")
	if _, err := f.svc.Notify(context.Background(), "epay", signedRequest(params, http.MethodGet)); !errors.Is(err, ErrTradeConflict) {
		t.Fatalf("reused trade error = %v", err)
	}
	f.assertEvent(t, "rejected:trade_conflict", true)
	if f.calls.Load() != 1 {
		t.Fatalf("marker called %d times", f.calls.Load())
	}
}

func TestServiceMarkerFailureCanRetry(t *testing.T) {
	f := newServiceFixture(t)
	marker := f.svc.marker
	failure := errors.New("temporary database failure")
	f.svc.marker = PaidMarkerFunc(func(context.Context, int64, string) error { return failure })
	if _, err := f.svc.Notify(context.Background(), "epay", signedRequest(testNoticeParams(), http.MethodGet)); !errors.Is(err, failure) {
		t.Fatalf("marker failure = %v", err)
	}
	f.assertEvent(t, "rejected:mark_paid_failed", true)
	f.svc.marker = marker
	if _, err := f.svc.Notify(context.Background(), "epay", signedRequest(testNoticeParams(), http.MethodGet)); err != nil {
		t.Fatal(err)
	}
	f.assertEvent(t, "applied", true)
}

func TestServiceEventWriteFailureCanRetryWithoutRepaying(t *testing.T) {
	f := newServiceFixture(t)
	f.exec(t, `CREATE TRIGGER reject_events BEFORE INSERT ON payment_events BEGIN SELECT RAISE(FAIL, 'event write failed'); END`)
	if _, err := f.svc.Notify(context.Background(), "epay", signedRequest(testNoticeParams(), http.MethodGet)); err == nil {
		t.Fatal("event write failure incorrectly acknowledged")
	}
	f.exec(t, `DROP TRIGGER reject_events`)
	if _, err := f.svc.Notify(context.Background(), "epay", signedRequest(testNoticeParams(), http.MethodGet)); err != nil {
		t.Fatal(err)
	}
	f.assertEvent(t, "duplicate", true)
	if f.calls.Load() != 1 {
		t.Fatalf("payment repeated after event failure: %d calls", f.calls.Load())
	}
}

func TestServiceConcurrentDuplicateNotifications(t *testing.T) {
	f := newServiceFixture(t)
	const workers = 20
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			_, err := f.svc.Notify(context.Background(), "epay", signedRequest(testNoticeParams(), http.MethodGet))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent notify: %v", err)
		}
	}
	var applied, duplicates int
	if err := f.db.QueryRow(`SELECT SUM(result = 'applied'), SUM(result = 'duplicate') FROM payment_events`).Scan(&applied, &duplicates); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 || applied != 1 || duplicates != workers-1 {
		t.Fatalf("marker calls = %d, applied = %d, duplicates = %d", f.calls.Load(), applied, duplicates)
	}
}

func TestServiceRejectsPublicOfflineNotify(t *testing.T) {
	f := newServiceFixture(t)
	for _, provider := range []string{"manual", "mock", "unknown"} {
		want := ErrNotifyUnsupported
		if provider == "unknown" {
			want = ErrProviderUnavailable
		}
		if _, err := f.svc.Notify(context.Background(), provider, httptest.NewRequest(http.MethodGet, "/notify?paid=true", nil)); !errors.Is(err, want) {
			t.Fatalf("%s notify error = %v", provider, err)
		}
		result := "rejected:notify_unsupported"
		if provider == "unknown" {
			result = "rejected:provider_unavailable"
		}
		f.assertEvent(t, result, false)
	}
	if f.calls.Load() != 0 {
		t.Fatal("public offline callback marked paid")
	}
}

func TestServiceMockPay(t *testing.T) {
	f := newServiceFixture(t)
	f.addOrder(t, "MOCK1", "mock")
	if err := f.svc.MockPay(context.Background(), 2, "MOCK1"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("wrong owner error = %v", err)
	}
	if err := f.svc.MockPay(context.Background(), 0, "MOCK1"); !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("unauthenticated user error = %v", err)
	}
	if err := f.svc.MockPay(context.Background(), 1, "PT1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("wrong provider error = %v", err)
	}
	if err := f.svc.MockPay(context.Background(), 1, "MOCK1"); err != nil {
		t.Fatal(err)
	}
	f.assertEvent(t, "applied", true)
	if err := f.svc.MockPay(context.Background(), 1, "MOCK1"); err != nil {
		t.Fatal(err)
	}
	f.assertEvent(t, "duplicate", true)
	var raw, tradeNo string
	if err := f.db.QueryRow(`SELECT raw, trade_no FROM payment_events ORDER BY id DESC LIMIT 1`).Scan(&raw, &tradeNo); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"user_id":1`) || tradeNo != "mock-MOCK1" || f.calls.Load() != 1 {
		t.Fatalf("raw = %q, trade = %q, marker calls = %d", raw, tradeNo, f.calls.Load())
	}
	f.svc.dev = false
	if err := f.svc.MockPay(context.Background(), 1, "MOCK1"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("production mock error = %v", err)
	}
}

func TestServiceConfiguration(t *testing.T) {
	f := newServiceFixture(t)
	mock, _ := NewMock(true)
	for _, tc := range []struct {
		db     *sql.DB
		marker PaidMarker
		opts   ServiceOptions
	}{
		{nil, f.svc.marker, ServiceOptions{}},
		{f.db, nil, ServiceOptions{}},
		{f.db, f.svc.marker, ServiceOptions{Providers: map[string]Provider{"mock": mock}}},
		{f.db, f.svc.marker, ServiceOptions{Providers: map[string]Provider{"unknown": NewManual("")}}},
		{f.db, f.svc.marker, ServiceOptions{Providers: map[string]Provider{"epay": nil}}},
	} {
		if _, err := NewService(tc.db, tc.marker, tc.opts); err == nil {
			t.Fatal("invalid service config accepted")
		}
	}
	providers := map[string]Provider{"manual": NewManual("收款说明")}
	svc, err := NewService(f.db, f.svc.marker, ServiceOptions{Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	delete(providers, "manual")
	if svc.providers["manual"] == nil || svc.now == nil {
		t.Fatal("service did not copy configuration or default clock")
	}
	if _, err := svc.Create(context.Background(), "mock", testCreateRequest()); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("disabled mock create error = %v", err)
	}
	if err := svc.MockPay(context.Background(), 1, "missing"); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("disabled mock pay error = %v", err)
	}
}

func TestServiceEventFailurePreservesCause(t *testing.T) {
	f := newServiceFixture(t)
	f.exec(t, `DROP TABLE payment_events`)
	_, err := f.svc.Notify(context.Background(), "manual", httptest.NewRequest(http.MethodGet, "/notify", nil))
	if !errors.Is(err, ErrNotifyUnsupported) || !strings.Contains(fmt.Sprint(err), "record payment event") {
		t.Fatalf("missing original or recording error: %v", err)
	}
}
