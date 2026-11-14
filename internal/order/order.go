package order

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/DueGin/seat-hub/internal/catalog"
)

const (
	StatusPending       = "pending"
	StatusPaid          = "paid"
	StatusFulfilled     = "fulfilled"
	StatusFulfillFailed = "fulfill_failed"
	StatusCancelled     = "cancelled"
	StatusExpired       = "expired"
	PaymentWindow       = 30 * time.Minute
)

var (
	ErrNotFound            = errors.New("order not found")
	ErrProductUnavailable  = errors.New("product unavailable")
	ErrInvalidQuantity     = errors.New("invalid quantity")
	ErrRenewMismatch       = errors.New("renew mismatch")
	ErrOutOfStock          = errors.New("out of stock")
	ErrInvalidState        = errors.New("invalid order state")
	ErrProviderUnavailable = errors.New("provider unavailable")
	ErrInvalidUser         = errors.New("invalid user")
)

type Catalog interface {
	Get(ctx context.Context, id int64) (catalog.Product, error)
	AvailableVMs(ctx context.Context) (int64, error)
}

type Fulfiller interface {
	ApplyPaidOrder(ctx context.Context, orderID int64) error
}

// AsyncFulfiller is an optional Fulfiller capability: the payment transition and
// a durable fulfilment job are written in one transaction, so the order can
// never be paid without a job that will eventually fulfil it. Fulfillers that
// do not implement it keep the synchronous behaviour (tests, simple callers).
type AsyncFulfiller interface {
	EnqueueFulfilOrderTx(ctx context.Context, tx *sql.Tx, orderID int64) error
}

type Order struct {
	ID                  int64   `json:"id"`
	OrderNo             string  `json:"order_no"`
	UserID              int64   `json:"user_id"`
	ProductID           int64   `json:"product_id"`
	ProductName         string  `json:"product_name"`
	ProductSnapshot     string  `json:"-"`
	Kind                string  `json:"kind"`
	Quantity            int64   `json:"quantity"`
	Units               int64   `json:"units"`
	DurationDays        int64   `json:"duration_days"`
	AmountCents         int64   `json:"amount_cents"`
	Currency            string  `json:"currency"`
	Provider            string  `json:"provider"`
	ProviderTradeNo     *string `json:"provider_trade_no"`
	Status              string  `json:"status"`
	RenewSubscriptionID *int64  `json:"renew_subscription_id"`
	FulfilError         *string `json:"fulfil_error"`
	CreatedAt           string  `json:"created_at"`
	ExpiresAt           string  `json:"expires_at"`
	PaidAt              *string `json:"paid_at"`
	FulfilledAt         *string `json:"fulfilled_at"`
	UpdatedAt           string  `json:"updated_at"`
}

type CreateRequest struct {
	UserID              int64  `json:"-"`
	ProductID           int64  `json:"product_id"`
	Quantity            int64  `json:"quantity"`
	Provider            string `json:"provider"`
	RenewSubscriptionID *int64 `json:"renew_subscription_id"`
}

type Service struct {
	db        *sql.DB
	catalog   Catalog
	fulfiller Fulfiller
}

func New(db *sql.DB, products Catalog, fulfiller Fulfiller) *Service {
	return &Service{db: db, catalog: products, fulfiller: fulfiller}
}

func (service *Service) SetFulfiller(fulfiller Fulfiller) { service.fulfiller = fulfiller }

const columns = `id, order_no, user_id, product_id, product_snapshot, kind, quantity, units, duration_days, amount_cents, currency, provider, provider_trade_no, status, renew_subscription_id, fulfil_error, created_at, expires_at, paid_at, fulfilled_at, updated_at`

// rentalLiveResourceStatus excludes accounts that no longer occupy stock.
const rentalLiveResourceStatus = `status NOT IN ('released', 'releasing')`

// rentalReservations is the stock still owed to live rental orders. A fresh
// purchase owes all its units; renewals are counted once per subscription and
// only for the gap its live accounts do not cover, so two renewals of the same
// subscription do not reserve the stock twice.
const rentalReservations = `COALESCE((SELECT SUM(fresh.units) FROM orders AS fresh
	WHERE fresh.kind = 'rental_account' AND fresh.status = 'pending' AND fresh.renew_subscription_id IS NULL AND julianday(fresh.expires_at) > julianday(?)), 0)
	+ COALESCE((SELECT SUM(CASE WHEN owed.held >= owed.units THEN 0 ELSE owed.units - owed.held END) FROM (
		SELECT MAX(renewed.units) AS units,
			(SELECT COUNT(*) FROM resources AS held WHERE held.subscription_id = renewed.renew_subscription_id AND held.` + rentalLiveResourceStatus + `) AS held
		FROM orders AS renewed
		WHERE renewed.kind = 'rental_account' AND renewed.status = 'pending' AND renewed.renew_subscription_id IS NOT NULL AND julianday(renewed.expires_at) > julianday(?)
		GROUP BY renewed.renew_subscription_id) AS owed), 0)`

type scanner interface{ Scan(...any) error }

func scanOrder(row scanner) (Order, error) {
	var purchase Order
	err := row.Scan(&purchase.ID, &purchase.OrderNo, &purchase.UserID, &purchase.ProductID, &purchase.ProductSnapshot, &purchase.Kind, &purchase.Quantity, &purchase.Units, &purchase.DurationDays, &purchase.AmountCents, &purchase.Currency, &purchase.Provider, &purchase.ProviderTradeNo, &purchase.Status, &purchase.RenewSubscriptionID, &purchase.FulfilError, &purchase.CreatedAt, &purchase.ExpiresAt, &purchase.PaidAt, &purchase.FulfilledAt, &purchase.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("scan order: %w", err)
	}
	var product catalog.Product
	if err := json.Unmarshal([]byte(purchase.ProductSnapshot), &product); err != nil {
		return Order{}, fmt.Errorf("decode product snapshot: %w", err)
	}
	purchase.ProductName = product.Name
	return purchase, nil
}

func (service *Service) Get(ctx context.Context, orderID int64) (Order, error) {
	return scanOrder(service.db.QueryRowContext(ctx, `SELECT `+columns+` FROM orders WHERE id = ?`, orderID))
}

func (service *Service) GetByNo(ctx context.Context, orderNo string) (Order, error) {
	return scanOrder(service.db.QueryRowContext(ctx, `SELECT `+columns+` FROM orders WHERE order_no = ?`, orderNo))
}

func (service *Service) GetForUser(ctx context.Context, userID int64, orderNo string) (Order, error) {
	return scanOrder(service.db.QueryRowContext(ctx, `SELECT `+columns+` FROM orders WHERE user_id = ? AND order_no = ?`, userID, orderNo))
}

func (service *Service) Create(ctx context.Context, request CreateRequest) (Order, error) {
	if request.UserID <= 0 {
		return Order{}, ErrInvalidUser
	}
	if request.Provider != "epay" && request.Provider != "manual" && request.Provider != "mock" {
		return Order{}, ErrProviderUnavailable
	}
	product, err := service.catalog.Get(ctx, request.ProductID)
	if errors.Is(err, catalog.ErrNotFound) {
		return Order{}, ErrProductUnavailable
	}
	if err != nil {
		return Order{}, fmt.Errorf("get product: %w", err)
	}
	if !product.Enabled {
		return Order{}, ErrProductUnavailable
	}
	if err := product.Validate(); err != nil {
		return Order{}, fmt.Errorf("validate product: %w", err)
	}
	quantity := request.Quantity
	if request.RenewSubscriptionID != nil {
		var kind, status string
		var units int64
		err := service.db.QueryRowContext(ctx, `SELECT kind, quantity, status FROM subscriptions WHERE id = ? AND user_id = ?`, *request.RenewSubscriptionID, request.UserID).Scan(&kind, &units, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return Order{}, ErrRenewMismatch
		}
		if err != nil {
			return Order{}, fmt.Errorf("get renewal subscription: %w", err)
		}
		if kind != product.Kind || status == "cancelled" || units <= 0 || units%product.UnitQuantity != 0 {
			return Order{}, ErrRenewMismatch
		}
		quantity = units / product.UnitQuantity
	}
	if quantity < product.MinQty || quantity > product.MaxQty || quantity > math.MaxInt64/product.UnitQuantity || (product.PriceCents > 0 && quantity > math.MaxInt64/product.PriceCents) {
		return Order{}, ErrInvalidQuantity
	}
	units := quantity * product.UnitQuantity
	available := int64(0)
	// Reserve stock for every order that will need a fresh allocation: a new
	// rental purchase, and a renewal whose old subscription no longer holds
	// enough live accounts. Renewals used to skip this entirely, so a paid
	// renewal could fail to fulfil for lack of stock.
	needed := units
	if product.Kind == catalog.KindRentalAccount {
		if request.RenewSubscriptionID != nil {
			held, err := service.heldUnits(ctx, *request.RenewSubscriptionID)
			if err != nil {
				return Order{}, err
			}
			needed = max(0, units-held)
		}
	}
	reserve := product.Kind == catalog.KindRentalAccount && needed > 0
	if reserve {
		available, err = service.catalog.AvailableVMs(ctx)
		if err != nil {
			return Order{}, fmt.Errorf("get rental capacity: %w", err)
		}
		if needed > available {
			return Order{}, ErrOutOfStock
		}
	}
	snapshot, err := json.Marshal(product)
	if err != nil {
		return Order{}, fmt.Errorf("encode product snapshot: %w", err)
	}
	now := time.Now().UTC()
	orderNo, err := generateOrderNo(now)
	if err != nil {
		return Order{}, err
	}
	query := `INSERT INTO orders (order_no, user_id, product_id, product_snapshot, kind, quantity, units, duration_days, amount_cents, currency, provider, status, renew_subscription_id, created_at, expires_at, updated_at) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?`
	arguments := []any{orderNo, request.UserID, product.ID, string(snapshot), product.Kind, quantity, units, product.DurationDays, product.PriceCents * quantity, product.Currency, request.Provider, request.RenewSubscriptionID, now.Format(time.RFC3339), now.Add(PaymentWindow).Format(time.RFC3339), now.Format(time.RFC3339)}
	if reserve {
		// The stock check and the insert are one statement: concurrent buyers
		// cannot both pass. Reservations count what a live order still has to
		// allocate, so a renewal only reserves the gap between its units and the
		// accounts the subscription already holds.
		query += ` WHERE ` + rentalReservations + ` <= ?`
		arguments = append(arguments, now.Format(time.RFC3339), now.Format(time.RFC3339), available-needed)
	}
	result, err := service.db.ExecContext(ctx, query, arguments...)
	if err != nil {
		return Order{}, fmt.Errorf("insert order: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return Order{}, fmt.Errorf("count created orders: %w", err)
	} else if count == 0 {
		return Order{}, ErrOutOfStock
	}
	orderID, err := result.LastInsertId()
	if err != nil {
		return Order{}, fmt.Errorf("get order id: %w", err)
	}
	return service.Get(ctx, orderID)
}

func generateOrderNo(now time.Time) (string, error) {
	var randomBytes [8]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", fmt.Errorf("generate order number: %w", err)
	}
	return "PT" + now.UTC().Format("20060102150405") + hex.EncodeToString(randomBytes[:]), nil
}

// heldUnits counts the live rental accounts a subscription already holds, so a
// renewal only has to allocate the shortfall.
func (service *Service) heldUnits(ctx context.Context, subscriptionID int64) (int64, error) {
	var held int64
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM resources WHERE subscription_id = ? AND `+rentalLiveResourceStatus, subscriptionID).Scan(&held); err != nil {
		return 0, fmt.Errorf("count held rental accounts: %w", err)
	}
	return held, nil
}

// MarkPaid settles an order. It accepts pending orders even when the payment
// window has already closed: a late callback or a manual transfer must still be
// bookable, so expiry is a checkout gate, not a settlement gate.
//
// With an AsyncFulfiller the status change and a durable fulfil_order job are
// committed together; fulfilment then runs in the job engine, never on the
// request context. Otherwise fulfilment stays synchronous for compatibility.
//
// The settlement itself runs on a detached context: once the provider has told
// us the money arrived, a client disconnect must not leave the order pending.
func (service *Service) MarkPaid(ctx context.Context, orderID int64, providerTradeNo string) error {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339)
	async, asynchronous := service.fulfiller.(AsyncFulfiller)
	tx, err := service.db.BeginTx(settleCtx, nil)
	if err != nil {
		return fmt.Errorf("begin order settlement: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(settleCtx, `UPDATE orders SET status = 'paid', provider_trade_no = COALESCE(?, provider_trade_no), paid_at = COALESCE(paid_at, ?), updated_at = ? WHERE id = ? AND status IN ('pending', 'expired')`, nullableString(providerTradeNo), now, now, orderID)
	if err != nil {
		return fmt.Errorf("mark order paid: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count paid orders: %w", err)
	}
	if count == 0 {
		purchase, err := scanOrder(tx.QueryRowContext(settleCtx, `SELECT `+columns+` FROM orders WHERE id = ?`, orderID))
		if err != nil {
			return err
		}
		if purchase.Status == StatusPaid || purchase.Status == StatusFulfilled || purchase.Status == StatusFulfillFailed {
			return nil
		}
		return ErrInvalidState
	}
	if service.fulfiller == nil {
		return tx.Commit()
	}
	if asynchronous {
		if err := async.EnqueueFulfilOrderTx(settleCtx, tx, orderID); err != nil {
			return fmt.Errorf("enqueue order fulfilment: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit order settlement: %w", err)
		}
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit order settlement: %w", err)
	}
	// Fulfilment keeps the caller's context: a cancelled callback should stop
	// the upstream work early, and the order stays paid with a retry path.
	return service.fulfill(ctx, orderID)
}

// fulfill applies a synchronous fulfiller. The failure record is written on a
// context that survives request cancellation, otherwise a cancelled callback
// would leave the order paid with no trace and no retry path.
func (service *Service) fulfill(ctx context.Context, orderID int64) error {
	if err := service.fulfiller.ApplyPaidOrder(ctx, orderID); err != nil {
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if recordErr := service.MarkFulfillFailed(recordCtx, orderID, err.Error()); recordErr != nil {
			return fmt.Errorf("record fulfillment failure: %w", recordErr)
		}
		return nil
	}
	return service.MarkFulfilled(ctx, orderID)
}

// Refulfill re-drives an order whose fulfilment failed, or one that is stuck in
// paid (process restarted between settlement and fulfilment).
func (service *Service) Refulfill(ctx context.Context, orderID int64) error {
	if service.fulfiller == nil {
		return ErrInvalidState
	}
	async, asynchronous := service.fulfiller.(AsyncFulfiller)
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin fulfilment retry: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE orders SET status = 'paid', fulfil_error = NULL, updated_at = ? WHERE id = ? AND status IN ('paid', 'fulfill_failed')`, time.Now().UTC().Format(time.RFC3339), orderID)
	if err != nil {
		return fmt.Errorf("retry order fulfillment: %w", err)
	}
	changed, err := service.requireChangedTx(ctx, tx, result, orderID)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if asynchronous {
		if err := async.EnqueueFulfilOrderTx(ctx, tx, orderID); err != nil {
			return fmt.Errorf("enqueue order fulfilment: %w", err)
		}
		return tx.Commit()
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit fulfilment retry: %w", err)
	}
	return service.fulfill(ctx, orderID)
}

func (service *Service) MarkFulfilled(ctx context.Context, orderID int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := service.db.ExecContext(ctx, `UPDATE orders SET status = 'fulfilled', fulfil_error = NULL, fulfilled_at = ?, updated_at = ? WHERE id = ? AND status IN ('paid', 'fulfill_failed')`, now, now, orderID)
	if err != nil {
		return fmt.Errorf("mark order fulfilled: %w", err)
	}
	return service.requireChanged(ctx, result, orderID, StatusFulfilled)
}

func (service *Service) MarkFulfillFailed(ctx context.Context, orderID int64, reason string) error {
	result, err := service.db.ExecContext(ctx, `UPDATE orders SET status = 'fulfill_failed', fulfil_error = ?, updated_at = ? WHERE id = ? AND status = 'paid'`, reason, time.Now().UTC().Format(time.RFC3339), orderID)
	if err != nil {
		return fmt.Errorf("mark fulfillment failed: %w", err)
	}
	return service.requireChanged(ctx, result, orderID, StatusFulfillFailed)
}

func (service *Service) Cancel(ctx context.Context, userID int64, orderNo string) error {
	var purchase Order
	var err error
	if userID == 0 {
		purchase, err = service.GetByNo(ctx, orderNo)
	} else {
		purchase, err = service.GetForUser(ctx, userID, orderNo)
	}
	if err != nil {
		return err
	}
	result, err := service.db.ExecContext(ctx, `UPDATE orders SET status = 'cancelled', updated_at = ? WHERE id = ? AND status = 'pending'`, time.Now().UTC().Format(time.RFC3339), purchase.ID)
	if err != nil {
		return fmt.Errorf("cancel order: %w", err)
	}
	return service.requireChanged(ctx, result, purchase.ID, StatusCancelled)
}

func (service *Service) requireChanged(ctx context.Context, result sql.Result, orderID int64, idempotentStatus string) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count changed orders: %w", err)
	}
	if count > 0 {
		return nil
	}
	purchase, err := service.Get(ctx, orderID)
	if err != nil {
		return err
	}
	if purchase.Status == idempotentStatus {
		return nil
	}
	return ErrInvalidState
}

// requireChangedTx reports whether the statement changed a row inside a
// settlement/retry transaction. An order that already reached fulfilled is a
// successful no-op for a retry.
func (service *Service) requireChangedTx(ctx context.Context, tx *sql.Tx, result sql.Result, orderID int64) (bool, error) {
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count changed orders: %w", err)
	}
	if count > 0 {
		return true, nil
	}
	purchase, err := scanOrder(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM orders WHERE id = ?`, orderID))
	if err != nil {
		return false, err
	}
	if purchase.Status == StatusFulfilled {
		return false, nil
	}
	return false, ErrInvalidState
}

func (service *Service) ExpireStale(ctx context.Context) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := service.db.ExecContext(ctx, `UPDATE orders SET status = 'expired', updated_at = ? WHERE status = 'pending' AND julianday(expires_at) <= julianday(?)`, now, now)
	if err != nil {
		return 0, fmt.Errorf("expire unpaid orders: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count expired orders: %w", err)
	}
	return count, nil
}

func (service *Service) List(ctx context.Context, userID int64, page, pageSize int) ([]Order, int64, error) {
	return service.ListAdmin(ctx, "", userID, page, pageSize)
}

func (service *Service) ListAdmin(ctx context.Context, status string, userID int64, page, pageSize int) ([]Order, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	conditions := make([]string, 0, 2)
	arguments := make([]any, 0)
	if userID > 0 {
		conditions = append(conditions, "user_id = ?")
		arguments = append(arguments, userID)
	}
	if status != "" && status != "all" {
		conditions = append(conditions, "status = ?")
		arguments = append(arguments, status)
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	var total int64
	if err := service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM orders`+where, arguments...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count orders: %w", err)
	}
	arguments = append(arguments, pageSize, (int64(page)-1)*int64(pageSize))
	rows, err := service.db.QueryContext(ctx, `SELECT `+columns+` FROM orders`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, arguments...)
	if err != nil {
		return nil, 0, fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()
	items := make([]Order, 0)
	for rows.Next() {
		purchase, err := scanOrder(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, purchase)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read orders: %w", err)
	}
	return items, total, nil
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
