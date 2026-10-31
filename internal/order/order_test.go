package order_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DueGin/vm2api-portal/internal/catalog"
	"github.com/DueGin/vm2api-portal/internal/order"
	"github.com/DueGin/vm2api-portal/internal/payment"
	"github.com/DueGin/vm2api-portal/internal/store/migrations"
	_ "modernc.org/sqlite"
)

type fulfillFunc func(context.Context, int64) error

func (apply fulfillFunc) ApplyPaidOrder(ctx context.Context, orderID int64) error {
	return apply(ctx, orderID)
}

type fixture struct {
	db       *sql.DB
	products *catalog.Service
	orders   *order.Service
	product  catalog.Product
}

func setup(t *testing.T, kind string, fulfiller order.Fulfiller) fixture {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	schema, err := migrations.FS.ReadFile("001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO users(id, email, password_hash, created_at, updated_at) VALUES (1, 'one@example.com', '', '', ''), (2, 'two@example.com', '', '', '')`); err != nil {
		t.Fatal(err)
	}
	products := catalog.New(database, func(string) (bool, bool) { return true, true })
	product, err := products.Create(context.Background(), catalog.Product{Kind: kind, Name: "测试套餐", UnitQuantity: 2, DurationDays: 30, PriceCents: 1250, MinQty: 1, MaxQty: 10, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{database, products, order.New(database, products, fulfiller), product}
}

func (fixture fixture) create(t *testing.T, quantity int64) order.Order {
	t.Helper()
	purchase, err := fixture.orders.Create(context.Background(), order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Quantity: quantity, Provider: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	return purchase
}

func (fixture fixture) subscription(t *testing.T, userID int64, kind string, quantity int64, status string) int64 {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := fixture.db.Exec(`INSERT INTO subscriptions(user_id, kind, quantity, status, starts_at, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, userID, kind, quantity, status, now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCreateSnapshotAmountAndValidation(t *testing.T) {
	ctx := context.Background()
	fixture := setup(t, catalog.KindConcurrency, nil)
	purchase := fixture.create(t, 3)
	if purchase.AmountCents != 3750 || purchase.Units != 6 || purchase.Status != order.StatusPending || purchase.ProductName != "测试套餐" || !strings.HasPrefix(purchase.OrderNo, "PT") || purchase.Currency != "CNY" {
		t.Fatalf("purchase=%+v", purchase)
	}
	created, err := time.Parse(time.RFC3339, purchase.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	expires, err := time.Parse(time.RFC3339, purchase.ExpiresAt)
	if err != nil || expires.Sub(created) != 30*time.Minute {
		t.Fatalf("expiry=%s, err=%v", purchase.ExpiresAt, err)
	}
	fixture.product.Name = "新名称"
	fixture.product.PriceCents = 9000
	if _, err := fixture.products.Update(ctx, fixture.product.ID, fixture.product); err != nil {
		t.Fatal(err)
	}
	saved, err := fixture.orders.GetByNo(ctx, purchase.OrderNo)
	if err != nil || saved.ProductName != "测试套餐" || saved.AmountCents != 3750 {
		t.Fatalf("saved=%+v, err=%v", saved, err)
	}
	for _, quantity := range []int64{0, -1, 11} {
		if _, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Quantity: quantity, Provider: "manual"}); !errors.Is(err, order.ErrInvalidQuantity) {
			t.Fatalf("quantity=%d, err=%v", quantity, err)
		}
	}
	if _, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Quantity: 1, Provider: "grant"}); !errors.Is(err, order.ErrProviderUnavailable) {
		t.Fatal(err)
	}
	if _, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 0, ProductID: fixture.product.ID, Quantity: 1, Provider: "manual"}); !errors.Is(err, order.ErrInvalidUser) {
		t.Fatal(err)
	}
	fixture.product.Enabled = false
	if _, err := fixture.products.Update(ctx, fixture.product.ID, fixture.product); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Quantity: 1, Provider: "manual"}); !errors.Is(err, order.ErrProductUnavailable) {
		t.Fatal(err)
	}
	if _, err := fixture.orders.GetForUser(ctx, 2, purchase.OrderNo); !errors.Is(err, order.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestRenewalDerivesQuantityAndChecksOwnership(t *testing.T) {
	ctx := context.Background()
	fixture := setup(t, catalog.KindRentalAccount, nil)
	for _, status := range []string{"active", "expired"} {
		subscriptionID := fixture.subscription(t, 1, catalog.KindRentalAccount, 6, status)
		purchase, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Quantity: 999, Provider: "manual", RenewSubscriptionID: &subscriptionID})
		if err != nil || purchase.Quantity != 3 || purchase.Units != 6 || purchase.AmountCents != 3750 || purchase.RenewSubscriptionID == nil || *purchase.RenewSubscriptionID != subscriptionID {
			t.Fatalf("renew=%+v, err=%v", purchase, err)
		}
	}
	for _, subscriptionID := range []int64{
		fixture.subscription(t, 2, catalog.KindRentalAccount, 6, "active"),
		fixture.subscription(t, 1, catalog.KindConcurrency, 6, "active"),
		fixture.subscription(t, 1, catalog.KindRentalAccount, 3, "active"),
		fixture.subscription(t, 1, catalog.KindRentalAccount, 6, "cancelled"),
		999,
	} {
		if _, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Provider: "manual", RenewSubscriptionID: &subscriptionID}); !errors.Is(err, order.ErrRenewMismatch) {
			t.Fatalf("subscription=%d, err=%v", subscriptionID, err)
		}
	}
	subscriptionID := fixture.subscription(t, 1, catalog.KindRentalAccount, 22, "active")
	if _, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Provider: "manual", RenewSubscriptionID: &subscriptionID}); !errors.Is(err, order.ErrInvalidQuantity) {
		t.Fatal(err)
	}
}

func TestRentalReservationAtomicAndRelease(t *testing.T) {
	ctx := context.Background()
	fixture := setup(t, catalog.KindRentalAccount, nil)
	for _, vmID := range []string{"a", "b", "c", "d"} {
		if _, err := fixture.db.Exec(`INSERT INTO rental_pool(vm_id, status, created_at, updated_at) VALUES (?, 'available', '', '')`, vmID); err != nil {
			t.Fatal(err)
		}
	}
	var successes atomic.Int64
	var failures atomic.Int64
	var group sync.WaitGroup
	for index := 0; index < 12; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Quantity: 1, Provider: "manual"})
			if err == nil {
				successes.Add(1)
			} else if errors.Is(err, order.ErrOutOfStock) {
				failures.Add(1)
			} else {
				t.Errorf("create concurrent order: %v", err)
			}
		}()
	}
	group.Wait()
	if successes.Load() != 2 || failures.Load() != 10 {
		t.Fatalf("successes=%d, out_of_stock=%d", successes.Load(), failures.Load())
	}
	items, total, err := fixture.orders.List(ctx, 1, 1, 20)
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("items=%+v, total=%d, err=%v", items, total, err)
	}
	if err := fixture.orders.Cancel(ctx, 2, items[0].OrderNo); !errors.Is(err, order.ErrNotFound) {
		t.Fatal(err)
	}
	if err := fixture.orders.Cancel(ctx, 1, items[0].OrderNo); err != nil {
		t.Fatal(err)
	}
	fixture.create(t, 1)
	stale := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	if _, err := fixture.db.Exec(`UPDATE orders SET expires_at = ? WHERE id = ?`, stale, items[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.orders.MarkPaid(ctx, items[1].ID, "late-pending"); !errors.Is(err, order.ErrInvalidState) {
		t.Fatal(err)
	}
	fixture.create(t, 1)
	count, err := fixture.orders.ExpireStale(ctx)
	if err != nil || count != 1 {
		t.Fatalf("expired=%d, err=%v", count, err)
	}
	if count, err := fixture.orders.ExpireStale(ctx); err != nil || count != 0 {
		t.Fatalf("expired again=%d, err=%v", count, err)
	}
	if err := fixture.orders.MarkPaid(ctx, items[1].ID, "late"); !errors.Is(err, order.ErrInvalidState) {
		t.Fatal(err)
	}
}

func TestMarkPaidIdempotentAndFulfillment(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int64
	fixture := setup(t, catalog.KindHostedSeat, fulfillFunc(func(context.Context, int64) error {
		calls.Add(1)
		return nil
	}))
	purchase := fixture.create(t, 1)
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := fixture.orders.MarkPaid(ctx, purchase.ID, "trade-1"); err != nil {
				t.Errorf("mark paid: %v", err)
			}
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fulfill calls=%d", calls.Load())
	}
	saved, err := fixture.orders.Get(ctx, purchase.ID)
	if err != nil || saved.Status != order.StatusFulfilled || saved.PaidAt == nil || saved.FulfilledAt == nil || saved.ProviderTradeNo == nil || *saved.ProviderTradeNo != "trade-1" {
		t.Fatalf("saved=%+v, err=%v", saved, err)
	}
	if err := fixture.orders.Cancel(ctx, 1, purchase.OrderNo); !errors.Is(err, order.ErrInvalidState) {
		t.Fatal(err)
	}
	if err := fixture.orders.MarkPaid(ctx, 999, "trade"); !errors.Is(err, order.ErrNotFound) {
		t.Fatal(err)
	}
	if err := fixture.orders.MarkFulfillFailed(ctx, purchase.ID, "late failure"); !errors.Is(err, order.ErrInvalidState) {
		t.Fatal(err)
	}
}

func TestFulfillmentFailureAndRetry(t *testing.T) {
	ctx := context.Background()
	fixture := setup(t, catalog.KindConcurrency, fulfillFunc(func(context.Context, int64) error { return errors.New("upstream unavailable") }))
	purchase := fixture.create(t, 1)
	if err := fixture.orders.MarkPaid(ctx, purchase.ID, "trade"); err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.orders.Get(ctx, purchase.ID)
	if err != nil || failed.Status != order.StatusFulfillFailed || failed.FulfilError == nil || *failed.FulfilError != "upstream unavailable" || failed.PaidAt == nil {
		t.Fatalf("failed=%+v, err=%v", failed, err)
	}
	if err := fixture.orders.MarkPaid(ctx, purchase.ID, "duplicate"); err != nil {
		t.Fatal(err)
	}
	fixture.orders.SetFulfiller(fulfillFunc(func(context.Context, int64) error { return nil }))
	if err := fixture.orders.Refulfill(ctx, purchase.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.orders.Refulfill(ctx, purchase.ID); err != nil {
		t.Fatal(err)
	}
	fulfilled, err := fixture.orders.Get(ctx, purchase.ID)
	if err != nil || fulfilled.Status != order.StatusFulfilled || fulfilled.FulfilError != nil {
		t.Fatalf("fulfilled=%+v, err=%v", fulfilled, err)
	}
}

func TestOverflowAndPendingPayment(t *testing.T) {
	ctx := context.Background()
	fixture := setup(t, catalog.KindConcurrency, nil)
	for _, overflowPrice := range []bool{true, false} {
		candidate := fixture.product
		candidate.MaxQty = math.MaxInt64
		if overflowPrice {
			candidate.PriceCents, candidate.UnitQuantity = math.MaxInt64, 1
		} else {
			candidate.PriceCents, candidate.UnitQuantity = 1, math.MaxInt64
		}
		if _, err := fixture.products.Update(ctx, candidate.ID, candidate); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: candidate.ID, Quantity: 2, Provider: "manual"}); !errors.Is(err, order.ErrInvalidQuantity) {
			t.Fatalf("overflow=%t, err=%v", overflowPrice, err)
		}
	}
	if _, err := fixture.products.Update(ctx, fixture.product.ID, fixture.product); err != nil {
		t.Fatal(err)
	}
	purchase := fixture.create(t, 1)
	if err := fixture.orders.MarkFulfilled(ctx, purchase.ID); !errors.Is(err, order.ErrInvalidState) {
		t.Fatal(err)
	}
	if err := fixture.orders.MarkPaid(ctx, purchase.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := fixture.orders.MarkPaid(ctx, purchase.ID, "later"); err != nil {
		t.Fatal(err)
	}
	saved, err := fixture.orders.Get(ctx, purchase.ID)
	if err != nil || saved.Status != order.StatusPaid || saved.ProviderTradeNo != nil {
		t.Fatalf("saved=%+v, err=%v", saved, err)
	}
}

func TestAdminFilterAndSystemCancel(t *testing.T) {
	ctx := context.Background()
	fixture := setup(t, catalog.KindConcurrency, nil)
	first := fixture.create(t, 1)
	second := fixture.create(t, 2)
	if err := fixture.orders.MarkPaid(ctx, first.ID, "trade"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.orders.Cancel(ctx, 0, second.OrderNo); err != nil {
		t.Fatal(err)
	}
	if err := fixture.orders.Cancel(ctx, 0, second.OrderNo); err != nil {
		t.Fatal(err)
	}
	items, total, err := fixture.orders.ListAdmin(ctx, order.StatusPaid, 1, 1, 20)
	if err != nil || total != 1 || len(items) != 1 || items[0].ID != first.ID {
		t.Fatalf("filtered=%+v, total=%d, err=%v", items, total, err)
	}
	items, total, err = fixture.orders.ListAdmin(ctx, "all", 0, 1, 1)
	if err != nil || total != 2 || len(items) != 1 || items[0].ID != second.ID {
		t.Fatalf("paged=%+v, total=%d, err=%v", items, total, err)
	}
	items, total, err = fixture.orders.ListAdmin(ctx, "paid' OR 1=1 --", 0, 1, 1000)
	if err != nil || total != 0 || len(items) != 0 {
		t.Fatalf("unrecognized status=%+v, total=%d, err=%v", items, total, err)
	}
}

func TestPaymentServiceUsesOrderLifecycle(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int64
	fixture := setup(t, catalog.KindConcurrency, fulfillFunc(func(context.Context, int64) error {
		calls.Add(1)
		return nil
	}))
	mock, err := payment.NewMock(true)
	if err != nil {
		t.Fatal(err)
	}
	payments, err := payment.NewService(fixture.db, fixture.orders, payment.ServiceOptions{Dev: true, Providers: map[string]payment.Provider{"mock": mock}})
	if err != nil {
		t.Fatal(err)
	}
	purchase, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Quantity: 2, Provider: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := payments.Create(ctx, purchase.Provider, payment.CreateRequest{OrderNo: purchase.OrderNo, ProductName: purchase.ProductName, AmountCents: purchase.AmountCents})
	if err != nil || checkout.Type != "mock" {
		t.Fatalf("checkout=%+v, err=%v", checkout, err)
	}
	if err := payments.MockPay(ctx, 2, purchase.OrderNo); !errors.Is(err, payment.ErrOrderNotFound) {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if err := payments.MockPay(ctx, 1, purchase.OrderNo); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := fixture.orders.Get(ctx, purchase.ID)
	if err != nil || saved.Status != order.StatusFulfilled || calls.Load() != 1 {
		t.Fatalf("saved=%+v, calls=%d, err=%v", saved, calls.Load(), err)
	}
	for _, result := range []string{"applied", "duplicate"} {
		var count int
		if err := fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM payment_events WHERE order_no = ? AND result = ? AND verified = 1`, purchase.OrderNo, result).Scan(&count); err != nil || count != 1 {
			t.Fatalf("event %s count=%d, err=%v", result, count, err)
		}
	}
}
