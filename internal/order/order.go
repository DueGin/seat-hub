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

	"github.com/DueGin/vm2api-portal/internal/catalog"
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
	reserve := product.Kind == catalog.KindRentalAccount && request.RenewSubscriptionID == nil
	if reserve {
		available, err = service.catalog.AvailableVMs(ctx)
		if err != nil {
			return Order{}, fmt.Errorf("get rental capacity: %w", err)
		}
		if units > available {
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
		query += ` WHERE COALESCE((SELECT SUM(units) FROM orders WHERE kind = 'rental_account' AND status = 'pending' AND renew_subscription_id IS NULL AND julianday(expires_at) > julianday(?)), 0) <= ?`
		arguments = append(arguments, now.Format(time.RFC3339), available-units)
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

func (service *Service) MarkPaid(ctx context.Context, orderID int64, providerTradeNo string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := service.db.ExecContext(ctx, `UPDATE orders SET status = 'paid', provider_trade_no = ?, paid_at = ?, updated_at = ? WHERE id = ? AND status = 'pending' AND julianday(expires_at) > julianday(?)`, nullableString(providerTradeNo), now, now, orderID, now)
	if err != nil {
		return fmt.Errorf("mark order paid: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count paid orders: %w", err)
	}
	if count == 0 {
		purchase, err := service.Get(ctx, orderID)
		if err != nil {
			return err
		}
		if purchase.Status == StatusPaid || purchase.Status == StatusFulfilled || purchase.Status == StatusFulfillFailed {
			return nil
		}
		return ErrInvalidState
	}
	if service.fulfiller == nil {
		return nil
	}
	return service.fulfill(ctx, orderID)
}

func (service *Service) fulfill(ctx context.Context, orderID int64) error {
	if err := service.fulfiller.ApplyPaidOrder(ctx, orderID); err != nil {
		if recordErr := service.MarkFulfillFailed(ctx, orderID, err.Error()); recordErr != nil {
			return fmt.Errorf("record fulfillment failure: %w", recordErr)
		}
		return nil
	}
	return service.MarkFulfilled(ctx, orderID)
}

func (service *Service) Refulfill(ctx context.Context, orderID int64) error {
	if service.fulfiller == nil {
		return ErrInvalidState
	}
	result, err := service.db.ExecContext(ctx, `UPDATE orders SET status = 'paid', fulfil_error = NULL, updated_at = ? WHERE id = ? AND status = 'fulfill_failed'`, time.Now().UTC().Format(time.RFC3339), orderID)
	if err != nil {
		return fmt.Errorf("retry order fulfillment: %w", err)
	}
	if err := service.requireChanged(ctx, result, orderID, StatusFulfilled); err != nil {
		return err
	}
	purchase, err := service.Get(ctx, orderID)
	if err != nil {
		return err
	}
	if purchase.Status == StatusFulfilled {
		return nil
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
