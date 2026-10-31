package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	KindConcurrency   = "concurrency"
	KindHostedSeat    = "hosted_seat"
	KindRentalAccount = "rental_account"
)

var (
	ErrNotFound = errors.New("product not found")
	ErrInvalid  = errors.New("invalid product")
)

type Product struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Badge        string `json:"badge"`
	UnitQuantity int64  `json:"unit_quantity"`
	DurationDays int64  `json:"duration_days"`
	PriceCents   int64  `json:"price_cents"`
	Currency     string `json:"currency"`
	MinQty       int64  `json:"min_qty"`
	MaxQty       int64  `json:"max_qty"`
	Enabled      bool   `json:"enabled"`
	SortOrder    int64  `json:"sort_order"`
	Stock        *int64 `json:"stock"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func Validate(product Product) error {
	if product.Kind != KindConcurrency && product.Kind != KindHostedSeat && product.Kind != KindRentalAccount {
		return fmt.Errorf("kind: %w", ErrInvalid)
	}
	if strings.TrimSpace(product.Name) == "" || product.UnitQuantity <= 0 || product.DurationDays <= 0 || product.PriceCents < 0 || product.MinQty <= 0 || product.MaxQty < product.MinQty {
		return fmt.Errorf("name, quantity, duration or price: %w", ErrInvalid)
	}
	return nil
}

func (product Product) Validate() error { return Validate(product) }

type HealthFunc func(vmID string) (healthy bool, fetched bool)

type Service struct {
	db     *sql.DB
	health HealthFunc
}

func New(db *sql.DB, health HealthFunc) *Service { return &Service{db: db, health: health} }

// SetHealth installs the health probe after construction. Without a probe a VM
// is assumed healthy: "unknown" must not read as "sold out".
func (service *Service) SetHealth(health HealthFunc) { service.health = health }

const columns = `id, kind, name, description, badge, unit_quantity, duration_days, price_cents, currency, min_qty, max_qty, enabled, sort_order, created_at, updated_at`

type scanner interface{ Scan(...any) error }

func scanProduct(row scanner) (Product, error) {
	var product Product
	err := row.Scan(&product.ID, &product.Kind, &product.Name, &product.Description, &product.Badge, &product.UnitQuantity, &product.DurationDays, &product.PriceCents, &product.Currency, &product.MinQty, &product.MaxQty, &product.Enabled, &product.SortOrder, &product.CreatedAt, &product.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Product{}, ErrNotFound
	}
	if err != nil {
		return Product{}, fmt.Errorf("scan product: %w", err)
	}
	return product, nil
}

func (service *Service) Get(ctx context.Context, id int64) (Product, error) {
	return scanProduct(service.db.QueryRowContext(ctx, `SELECT `+columns+` FROM products WHERE id = ?`, id))
}

func (service *Service) Create(ctx context.Context, product Product) (Product, error) {
	if err := Validate(product); err != nil {
		return Product{}, err
	}
	if product.Currency == "" {
		product.Currency = "CNY"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := service.db.ExecContext(ctx, `INSERT INTO products (kind, name, description, badge, unit_quantity, duration_days, price_cents, currency, min_qty, max_qty, enabled, sort_order, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, product.Kind, product.Name, product.Description, product.Badge, product.UnitQuantity, product.DurationDays, product.PriceCents, product.Currency, product.MinQty, product.MaxQty, product.Enabled, product.SortOrder, now, now)
	if err != nil {
		return Product{}, fmt.Errorf("insert product: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Product{}, fmt.Errorf("get product id: %w", err)
	}
	return service.Get(ctx, id)
}

func (service *Service) Update(ctx context.Context, id int64, product Product) (Product, error) {
	if err := Validate(product); err != nil {
		return Product{}, err
	}
	if product.Currency == "" {
		product.Currency = "CNY"
	}
	result, err := service.db.ExecContext(ctx, `UPDATE products SET kind = ?, name = ?, description = ?, badge = ?, unit_quantity = ?, duration_days = ?, price_cents = ?, currency = ?, min_qty = ?, max_qty = ?, enabled = ?, sort_order = ?, updated_at = ? WHERE id = ?`, product.Kind, product.Name, product.Description, product.Badge, product.UnitQuantity, product.DurationDays, product.PriceCents, product.Currency, product.MinQty, product.MaxQty, product.Enabled, product.SortOrder, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return Product{}, fmt.Errorf("update product: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Product{}, fmt.Errorf("count updated products: %w", err)
	} else if affected == 0 {
		return Product{}, ErrNotFound
	}
	return service.Get(ctx, id)
}

func (service *Service) Delete(ctx context.Context, id int64) error {
	transaction, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin product deletion: %w", err)
	}
	defer transaction.Rollback()
	var exists bool
	if err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM products WHERE id = ?)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("find product: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE products SET enabled = 0, updated_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("disable product: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `DELETE FROM products WHERE id = ? AND NOT EXISTS (SELECT 1 FROM orders WHERE product_id = ?)`, id, id); err != nil {
		return fmt.Errorf("delete product: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit product deletion: %w", err)
	}
	return nil
}

func (service *Service) List(ctx context.Context) ([]Product, error) {
	return service.list(ctx, false)
}

func (service *Service) ListPublic(ctx context.Context) ([]Product, error) {
	return service.list(ctx, true)
}

func (service *Service) list(ctx context.Context, public bool) ([]Product, error) {
	query := `SELECT ` + columns + ` FROM products`
	if public {
		query += ` WHERE enabled = 1`
	}
	rows, err := service.db.QueryContext(ctx, query+` ORDER BY sort_order, id`)
	if err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	defer rows.Close()
	products := make([]Product, 0)
	for rows.Next() {
		product, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		products = append(products, product)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read products: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close products: %w", err)
	}
	for index := range products {
		if products[index].Kind == KindRentalAccount {
			stock, err := service.Stock(ctx, products[index])
			if err != nil {
				return nil, err
			}
			products[index].Stock = &stock
		}
	}
	return products, nil
}

func (service *Service) AvailableVMs(ctx context.Context) (int64, error) {
	rows, err := service.db.QueryContext(ctx, `SELECT vm_id FROM rental_pool WHERE status = 'available'`)
	if err != nil {
		return 0, fmt.Errorf("list rental pool: %w", err)
	}
	defer rows.Close()
	var available int64
	for rows.Next() {
		var vmID string
		if err := rows.Scan(&vmID); err != nil {
			return 0, fmt.Errorf("read rental vm: %w", err)
		}
		if service.health == nil {
			available++
			continue
		}
		if healthy, _ := service.health(vmID); healthy {
			available++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read rental pool: %w", err)
	}
	return available, nil
}

func (service *Service) Stock(ctx context.Context, product Product) (int64, error) {
	if product.Kind != KindRentalAccount {
		return 0, nil
	}
	if product.UnitQuantity <= 0 {
		return 0, ErrInvalid
	}
	available, err := service.AvailableVMs(ctx)
	if err != nil {
		return 0, err
	}
	var reserved int64
	if err := service.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(units), 0) FROM orders WHERE kind = 'rental_account' AND status = 'pending' AND renew_subscription_id IS NULL AND julianday(expires_at) > julianday(?)`, time.Now().UTC().Format(time.RFC3339)).Scan(&reserved); err != nil {
		return 0, fmt.Errorf("count rental reservations: %w", err)
	}
	if available <= reserved {
		return 0, nil
	}
	return (available - reserved) / product.UnitQuantity, nil
}
