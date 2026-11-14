package order_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/catalog"
	"github.com/DueGin/seat-hub/internal/order"
	"github.com/DueGin/seat-hub/internal/payment"
	"github.com/DueGin/seat-hub/internal/store/migrations"
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
	// Apply every migration in order: the fixture must match the schema the
	// production code queries, including later ALTER TABLE additions.
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	for _, name := range names {
		schema, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(string(schema)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
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

// stock puts VMs in the rental pool so a reservation can be honoured.
func (fixture fixture) stock(t *testing.T, vmIDs ...string) {
	t.Helper()
	for _, vmID := range vmIDs {
		if _, err := fixture.db.Exec(`INSERT INTO rental_pool(vm_id, status, selfcheck_state, created_at, updated_at) VALUES (?, 'available', 'pass', '', '')`, vmID); err != nil {
			t.Fatal(err)
		}
	}
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
	// Each renewal of six units has to reserve six rental accounts (the old
	// subscription holds none), so two renewals need twelve.
	fixture.stock(t, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l")
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
		if _, err := fixture.db.Exec(`INSERT INTO rental_pool(vm_id, status, selfcheck_state, created_at, updated_at) VALUES (?, 'available', 'pass', '', '')`, vmID); err != nil {
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
	stale := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	if _, err := fixture.db.Exec(`UPDATE orders SET expires_at = ? WHERE id = ?`, stale, items[1].ID); err != nil {
		t.Fatal(err)
	}
	// A payment that lands after the window closed must still book: expiry
	// stops people from paying a stale checkout page, it does not lose money.
	if err := fixture.orders.MarkPaid(ctx, items[1].ID, "late-pending"); err != nil {
		t.Fatalf("late payment: %v", err)
	}
	expired := fixture.create(t, 1)
	if _, err := fixture.db.Exec(`UPDATE orders SET expires_at = ? WHERE id = ?`, stale, expired.ID); err != nil {
		t.Fatal(err)
	}
	count, err := fixture.orders.ExpireStale(ctx)
	if err != nil || count != 1 {
		t.Fatalf("expired=%d, err=%v", count, err)
	}
	if count, err := fixture.orders.ExpireStale(ctx); err != nil || count != 0 {
		t.Fatalf("expired again=%d, err=%v", count, err)
	}
	// An order the sweep already expired can still be settled (admin repair).
	if err := fixture.orders.MarkPaid(ctx, expired.ID, "admin-repair"); err != nil {
		t.Fatalf("settle expired: %v", err)
	}
	settled, err := fixture.orders.Get(ctx, expired.ID)
	if err != nil || settled.Status != order.StatusPaid || settled.ProviderTradeNo == nil || *settled.ProviderTradeNo != "admin-repair" {
		t.Fatalf("settled=%+v, err=%v", settled, err)
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

// asyncFulfiller records the durable jobs a settled order should produce.
type asyncFulfiller struct {
	mu       sync.Mutex
	enqueued []int64
	failWith error
	calls    int64
}

func (apply *asyncFulfiller) ApplyPaidOrder(context.Context, int64) error {
	atomic.AddInt64(&apply.calls, 1)
	return errors.New("synchronous fulfilment must not run")
}

func (apply *asyncFulfiller) EnqueueFulfilOrderTx(ctx context.Context, tx *sql.Tx, orderID int64) error {
	apply.mu.Lock()
	defer apply.mu.Unlock()
	if apply.failWith != nil {
		return apply.failWith
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders WHERE id = ? AND status = 'paid'`, orderID).Scan(&pending); err != nil {
		return err
	}
	if pending != 1 {
		return errors.New("job enqueued outside the settlement transaction")
	}
	apply.enqueued = append(apply.enqueued, orderID)
	return nil
}

func TestMarkPaidEnqueuesDurableFulfilment(t *testing.T) {
	ctx := context.Background()
	applicator := &asyncFulfiller{}
	fixture := setup(t, catalog.KindHostedSeat, applicator)
	purchase := fixture.create(t, 1)
	if err := fixture.orders.MarkPaid(ctx, purchase.ID, "trade"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.orders.MarkPaid(ctx, purchase.ID, "trade"); err != nil {
		t.Fatalf("duplicate settlement: %v", err)
	}
	applicator.mu.Lock()
	enqueued := append([]int64(nil), applicator.enqueued...)
	applicator.mu.Unlock()
	if len(enqueued) != 1 || enqueued[0] != purchase.ID || atomic.LoadInt64(&applicator.calls) != 0 {
		t.Fatalf("enqueued=%v synchronous=%d", enqueued, applicator.calls)
	}
	saved, err := fixture.orders.Get(ctx, purchase.ID)
	if err != nil || saved.Status != order.StatusPaid || saved.PaidAt == nil {
		t.Fatalf("saved=%+v, err=%v", saved, err)
	}
	// Enqueue failure rolls the settlement back so the callback can retry.
	rejected := &asyncFulfiller{failWith: errors.New("queue unavailable")}
	fixture.orders.SetFulfiller(rejected)
	failed := fixture.create(t, 1)
	if err := fixture.orders.MarkPaid(ctx, failed.ID, "trade-2"); err == nil {
		t.Fatal("settlement succeeded without a durable job")
	}
	still, err := fixture.orders.Get(ctx, failed.ID)
	if err != nil || still.Status != order.StatusPending {
		t.Fatalf("rolled back order=%+v, err=%v", still, err)
	}
}

func TestRefulfillRedrivesPaidAndFailed(t *testing.T) {
	ctx := context.Background()
	var failed atomic.Int64
	fixture := setup(t, catalog.KindConcurrency, fulfillFunc(func(context.Context, int64) error {
		if failed.Add(1) == 1 {
			return errors.New("upstream unavailable")
		}
		return nil
	}))
	purchase := fixture.create(t, 1)
	if err := fixture.orders.MarkPaid(ctx, purchase.ID, "trade"); err != nil {
		t.Fatal(err)
	}
	broken, err := fixture.orders.Get(ctx, purchase.ID)
	if err != nil || broken.Status != order.StatusFulfillFailed {
		t.Fatalf("broken=%+v, err=%v", broken, err)
	}
	if err := fixture.orders.Refulfill(ctx, purchase.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.orders.Refulfill(ctx, purchase.ID); err != nil {
		t.Fatalf("refulfil fulfilled order: %v", err)
	}

	// A stuck paid order (process died between settlement and fulfilment) is
	// re-driven by the same endpoint.
	stuck := fixture.create(t, 1)
	if err := fixture.orders.MarkPaid(ctx, stuck.ID, "trade-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`UPDATE orders SET status='paid', fulfilled_at=NULL WHERE id=?`, stuck.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.orders.Refulfill(ctx, stuck.ID); err != nil {
		t.Fatalf("refulfil paid order: %v", err)
	}
	saved, err := fixture.orders.Get(ctx, stuck.ID)
	if err != nil || saved.Status != order.StatusFulfilled {
		t.Fatalf("stuck=%+v, err=%v", saved, err)
	}
}

func TestFulfilmentFailureSurvivesCancelledContext(t *testing.T) {
	ctx := context.Background()
	fixture := setup(t, catalog.KindConcurrency, fulfillFunc(func(context.Context, int64) error { return errors.New("upstream unavailable") }))
	purchase := fixture.create(t, 1)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := fixture.orders.MarkPaid(cancelled, purchase.ID, "trade"); err != nil {
		t.Fatalf("settlement on cancelled request: %v", err)
	}
	saved, err := fixture.orders.Get(ctx, purchase.ID)
	if err != nil || saved.Status != order.StatusFulfillFailed || saved.FulfilError == nil {
		t.Fatalf("saved=%+v, err=%v", saved, err)
	}
}

func TestRenewalReservesWhenOldResourcesWereReleased(t *testing.T) {
	ctx := context.Background()
	fixture := setup(t, catalog.KindRentalAccount, nil)
	fixture.stock(t, "a", "b")
	subscriptionID := fixture.subscription(t, 1, catalog.KindRentalAccount, 4, "expired")
	// The old accounts are gone, so the renewal must reserve them again.
	if _, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Provider: "manual", RenewSubscriptionID: &subscriptionID}); !errors.Is(err, order.ErrOutOfStock) {
		t.Fatalf("renewal without stock error=%v", err)
	}
	fixture.stock(t, "c", "d")
	purchase, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Provider: "manual", RenewSubscriptionID: &subscriptionID})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.orders.Cancel(ctx, 1, purchase.OrderNo); err != nil {
		t.Fatal(err)
	}
	// A subscription still holding its accounts only reserves the shortfall:
	// four units minus one live account leaves three to reserve.
	if _, err := fixture.db.Exec(`INSERT INTO resources(user_id,kind,subscription_id,vm_id,status,created_at,updated_at) VALUES (1,'rental',?,'a','active','','')`, subscriptionID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.orders.Create(ctx, order.CreateRequest{UserID: 1, ProductID: fixture.product.ID, Provider: "manual", RenewSubscriptionID: &subscriptionID}); err != nil {
		t.Fatalf("renewal with held accounts: %v", err)
	}
}
