package payment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type ServiceOptions struct {
	Providers map[string]Provider
	Dev       bool
	Now       func() time.Time
	Active    string
}

type Service struct {
	db        *sql.DB
	marker    PaidMarker
	providers map[string]Provider
	dev       bool
	now       func() time.Time
	mu        sync.Mutex
	active    string
	queries   map[string]time.Time
}

func NewService(db *sql.DB, marker PaidMarker, opts ServiceOptions) (*Service, error) {
	if db == nil || marker == nil {
		return nil, fmt.Errorf("database and paid marker required: %w", ErrInvalidRequest)
	}
	providers := make(map[string]Provider, len(opts.Providers))
	for name, provider := range opts.Providers {
		if name != "epay" && name != "alipay_f2f" && name != "manual" && name != "mock" {
			return nil, fmt.Errorf("unknown provider %q: %w", name, ErrProviderUnavailable)
		}
		if provider == nil || (name == "mock" && !opts.Dev) {
			return nil, fmt.Errorf("provider %q: %w", name, ErrProviderUnavailable)
		}
		providers[name] = provider
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, marker: marker, providers: providers, dev: opts.Dev, now: now, active: opts.Active, queries: make(map[string]time.Time)}, nil
}

func (s *Service) Create(ctx context.Context, provider string, req CreateRequest) (Payment, error) {
	p, ok := s.providers[provider]
	if !ok || (s.active != "" && provider != s.active && !(provider == "mock" && s.dev)) {
		return Payment{}, ErrProviderUnavailable
	}
	if err := validateCreate(req); err != nil {
		return Payment{}, err
	}
	order, err := s.findOrder(ctx, req.OrderNo)
	if err != nil {
		return Payment{}, err
	}
	if order.provider != provider {
		return Payment{}, ErrProviderUnavailable
	}
	if order.amount != req.AmountCents || order.currency != "CNY" {
		return Payment{}, ErrAmountMismatch
	}
	if err := s.payable(order); err != nil {
		return Payment{}, err
	}
	req.ExpiresAt, err = time.Parse(time.RFC3339, order.expires)
	if err != nil {
		return Payment{}, fmt.Errorf("parse payment expiry: %w", err)
	}
	return p.Create(ctx, req)
}

// QueryPending throttles even failed queries, while settlement shares the notify lock.
func (s *Service) QueryPending(ctx context.Context, orderNo string) error {
	order, err := s.findOrder(ctx, orderNo)
	if err != nil {
		return err
	}
	if order.status != "pending" {
		return nil
	}
	querier, ok := s.providers[order.provider].(Querier)
	if !ok {
		return nil
	}
	s.mu.Lock()
	now := s.now()
	if last, ok := s.queries[orderNo]; ok && now.Sub(last) < 5*time.Second {
		s.mu.Unlock()
		return nil
	}
	for key, last := range s.queries {
		if now.Sub(last) > time.Hour {
			delete(s.queries, key)
		}
	}
	s.queries[orderNo] = now
	s.mu.Unlock()
	notice, err := querier.Query(ctx, orderNo)
	if err != nil {
		return err
	}
	if !notice.verified || notice.OrderNo != orderNo {
		return ErrInvalidNotice
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apply(ctx, order.provider, notice)
}

func (s *Service) Close(ctx context.Context, provider, orderNo string) error {
	if closer, ok := s.providers[provider].(Closer); ok {
		return closer.Close(ctx, orderNo)
	}
	return nil
}

// Notify is the public callback entry point. A nil error must be acknowledged
// with the provider's success response, including when the event is a duplicate.
func (s *Service) Notify(ctx context.Context, provider string, r *http.Request) (Notice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Unsupported or unregistered providers are rejected without touching the
	// event table: anyone can POST here, so an unauthenticated caller must not
	// be able to append rows.
	if provider == "manual" || provider == "mock" {
		return Notice{}, ErrNotifyUnsupported
	}
	p, ok := s.providers[provider]
	if !ok {
		return Notice{}, ErrProviderUnavailable
	}
	notice, err := p.Notify(r)
	if err != nil {
		if notice.OrderNo == "" {
			// Nothing references a real order; this is noise, not evidence.
			return notice, err
		}
		return notice, s.record(ctx, provider, notice, notice.verified, rejection(err), err)
	}
	return notice, s.apply(ctx, provider, notice)
}

// MockPay must only be called from an authenticated, CSRF-protected route.
// Ownership is checked again here; there is deliberately no public mock Notify.
func (s *Service) MockPay(ctx context.Context, userID int64, orderNo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dev || s.providers["mock"] == nil {
		return ErrProviderUnavailable
	}
	order, err := s.findOrder(ctx, orderNo)
	if err != nil {
		return err
	}
	if userID <= 0 || order.userID != userID {
		return ErrOrderNotFound
	}
	if order.provider != "mock" {
		return ErrProviderUnavailable
	}
	raw, err := json.Marshal(struct {
		Source  string `json:"source"`
		UserID  int64  `json:"user_id"`
		OrderNo string `json:"order_no"`
	}{Source: "authenticated_mock", UserID: userID, OrderNo: orderNo})
	if err != nil {
		return fmt.Errorf("encode mock event: %w", err)
	}
	notice := Notice{OrderNo: orderNo, TradeNo: "mock-" + orderNo, AmountCents: order.amount, Paid: true, Raw: string(raw)}
	return s.apply(ctx, "mock", notice)
}

func (s *Service) apply(ctx context.Context, provider string, notice Notice) error {
	reject := func(err error) error {
		return s.record(ctx, provider, notice, true, rejection(err), err)
	}
	if notice.OrderNo == "" || notice.TradeNo == "" || notice.AmountCents < 0 {
		return reject(ErrInvalidNotice)
	}
	if !notice.Paid {
		return reject(ErrNotPaid)
	}
	order, err := s.findOrder(ctx, notice.OrderNo)
	if err != nil {
		return reject(err)
	}
	if order.provider != provider {
		return reject(ErrProviderUnavailable)
	}
	if order.amount != notice.AmountCents || order.currency != "CNY" {
		return reject(ErrAmountMismatch)
	}
	var reused int
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM orders WHERE provider = ? AND provider_trade_no = ? AND id <> ?
		UNION ALL
		SELECT 1 FROM payment_events WHERE provider = ? AND trade_no = ? AND order_no <> ? AND result = 'applied'
	)`, provider, notice.TradeNo, order.id, provider, notice.TradeNo, notice.OrderNo).Scan(&reused)
	if err != nil {
		return reject(fmt.Errorf("check payment trade: %w", err))
	}
	if reused != 0 || (order.tradeNo != "" && order.tradeNo != notice.TradeNo) {
		return reject(ErrTradeConflict)
	}
	switch order.status {
	case "paid", "fulfilled", "fulfill_failed":
		return s.record(ctx, provider, notice, true, "duplicate", nil)
	}
	if err := s.settleable(order); err != nil {
		return reject(err)
	}
	// Do not hold a SQL transaction while invoking the marker: it owns its own
	// order transaction. A retry after an event write failure sees the paid state.
	if err := s.marker.MarkPaid(ctx, order.id, notice.TradeNo); err != nil {
		return s.record(ctx, provider, notice, true, "rejected:mark_paid_failed", fmt.Errorf("mark order paid: %w", err))
	}
	return s.record(ctx, provider, notice, true, "applied", nil)
}

type payableOrder struct {
	id       int64
	userID   int64
	amount   int64
	currency string
	provider string
	status   string
	tradeNo  string
	expires  string
}

func (s *Service) findOrder(ctx context.Context, orderNo string) (payableOrder, error) {
	var order payableOrder
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, amount_cents, currency, provider, status,
		COALESCE(provider_trade_no, ''), expires_at FROM orders WHERE order_no = ?`, orderNo).
		Scan(&order.id, &order.userID, &order.amount, &order.currency, &order.provider, &order.status, &order.tradeNo, &order.expires)
	if errors.Is(err, sql.ErrNoRows) {
		return payableOrder{}, ErrOrderNotFound
	}
	if err != nil {
		return payableOrder{}, fmt.Errorf("find payment order: %w", err)
	}
	return order, nil
}

// payable gates *starting* a payment: the order must still be pending and
// unexpired. Settlement uses settleable, because a gateway callback or a manual
// bank transfer routinely lands after the order window closed, and money that
// arrived must still be recorded.
func (s *Service) payable(order payableOrder) error {
	if order.status != "pending" {
		return ErrOrderNotPayable
	}
	expires, err := time.Parse(time.RFC3339, order.expires)
	if err != nil {
		return fmt.Errorf("parse order expiry: %w", err)
	}
	if !s.now().Before(expires) {
		return ErrOrderNotPayable
	}
	return nil
}

// settleable accepts a pending order regardless of its payment window: a
// gateway callback or a manual transfer can legitimately land late.
func (s *Service) settleable(order payableOrder) error {
	if order.status != "pending" {
		return ErrOrderNotPayable
	}
	return nil
}

// maxEventRaw bounds what a public callback can store. Notifications are
// attacker-supplied text and the column otherwise accepts the full 64 KiB body.
const maxEventRaw = 2048

func (s *Service) record(ctx context.Context, provider string, notice Notice, verified bool, result string, cause error) error {
	raw := notice.Raw
	if len(raw) > maxEventRaw {
		raw = raw[:maxEventRaw]
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO payment_events
		(provider, order_no, trade_no, verified, raw, result, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		provider, notice.OrderNo, notice.TradeNo, verified, raw, result, s.now().UTC().Format(time.RFC3339))
	if err != nil {
		return errors.Join(cause, fmt.Errorf("record payment event: %w", err))
	}
	return cause
}

// PruneEvents deletes payment events older than the cutoff. Callers schedule it
// (hourly is plenty); it returns how many rows were removed.
func (s *Service) PruneEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM payment_events WHERE created_at < ?`,
		cutoff.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("prune payment events: %w", err)
	}
	return result.RowsAffected()
}

func rejection(err error) string {
	switch {
	case errors.Is(err, ErrInvalidSignature):
		return "rejected:invalid_signature"
	case errors.Is(err, ErrMerchantMismatch):
		return "rejected:merchant_mismatch"
	case errors.Is(err, ErrInvalidNotice):
		return "rejected:invalid_notice"
	case errors.Is(err, ErrNotPaid):
		return "rejected:not_paid"
	case errors.Is(err, ErrOrderNotFound):
		return "rejected:order_not_found"
	case errors.Is(err, ErrOrderNotPayable):
		return "rejected:order_not_payable"
	case errors.Is(err, ErrAmountMismatch):
		return "rejected:amount_mismatch"
	case errors.Is(err, ErrProviderUnavailable):
		return "rejected:provider_unavailable"
	case errors.Is(err, ErrTradeConflict):
		return "rejected:trade_conflict"
	default:
		return "rejected:internal_error"
	}
}
