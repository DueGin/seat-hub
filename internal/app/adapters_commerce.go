// Package app wires the domain packages into the HTTP layer and owns process
// lifecycle (config, database, background jobs, gateway, server).
package app

import (
	"context"
	"errors"
	"time"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/catalog"
	"github.com/DueGin/seat-hub/internal/order"
)

// catalogAdapter implements api.CatalogService over catalog.Service.
type catalogAdapter struct {
	svc *catalog.Service
}

func toProductDTO(p catalog.Product) api.ProductDTO {
	return api.ProductDTO{
		ID:              p.ID,
		Kind:            p.Kind,
		Name:            p.Name,
		Description:     p.Description,
		Badge:           p.Badge,
		UnitQuantity:    int(p.UnitQuantity),
		DurationDays:    int(p.DurationDays),
		PriceCents:      p.PriceCents,
		Currency:        p.Currency,
		MinQty:          int(p.MinQty),
		MaxQty:          int(p.MaxQty),
		Enabled:         p.Enabled,
		SortOrder:       int(p.SortOrder),
		Stock:           stockPtr(p.Stock),
		ReservationOnly: p.ReservationOnly,
	}
}

func stockPtr(v *int64) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func fromProductInput(in api.ProductInput, existing catalog.Product) catalog.Product {
	p := existing
	p.Kind = in.Kind
	p.Name = in.Name
	p.Description = in.Description
	p.Badge = in.Badge
	p.UnitQuantity = int64(in.UnitQuantity)
	p.DurationDays = int64(in.DurationDays)
	p.PriceCents = in.PriceCents
	p.Currency = in.Currency
	p.MinQty = int64(in.MinQty)
	p.MaxQty = int64(in.MaxQty)
	p.SortOrder = int64(in.SortOrder)
	if in.ReservationOnly != nil {
		p.ReservationOnly = *in.ReservationOnly
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	return p
}

func (a *catalogAdapter) Public(ctx context.Context) ([]api.ProductDTO, error) {
	items, err := a.svc.ListPublic(ctx)
	if err != nil {
		return nil, err
	}
	return a.withStock(ctx, items)
}

func (a *catalogAdapter) All(ctx context.Context) ([]api.ProductDTO, error) {
	items, err := a.svc.List(ctx)
	if err != nil {
		return nil, err
	}
	return a.withStock(ctx, items)
}

// withStock attaches stock only for rental products; nil means "not applicable"
// rather than "zero".
func (a *catalogAdapter) withStock(ctx context.Context, items []catalog.Product) ([]api.ProductDTO, error) {
	out := make([]api.ProductDTO, 0, len(items))
	for _, p := range items {
		dto := toProductDTO(p)
		if p.Kind == catalog.KindRentalAccount && !p.ReservationOnly {
			n, err := a.svc.Stock(ctx, p)
			if err != nil {
				return nil, err
			}
			dto.Stock = stockPtr(&n)
		}
		out = append(out, dto)
	}
	return out, nil
}

func (a *catalogAdapter) Get(ctx context.Context, id int64) (api.ProductDTO, error) {
	p, err := a.svc.Get(ctx, id)
	if err != nil {
		return api.ProductDTO{}, mapErr(err, catalog.ErrNotFound, "product_not_found", "商品不存在")
	}
	dto := toProductDTO(p)
	if p.Kind == catalog.KindRentalAccount && !p.ReservationOnly {
		n, err := a.svc.Stock(ctx, p)
		if err != nil {
			return api.ProductDTO{}, err
		}
		dto.Stock = stockPtr(&n)
	}
	return dto, nil
}

func (a *catalogAdapter) Create(ctx context.Context, in api.ProductInput) (api.ProductDTO, error) {
	p, err := a.svc.Create(ctx, fromProductInput(in, catalog.Product{Enabled: true}))
	if err != nil {
		return api.ProductDTO{}, mapErr(err, catalog.ErrInvalid, "invalid_product", "商品参数不合法")
	}
	return toProductDTO(p), nil
}

func (a *catalogAdapter) Update(ctx context.Context, id int64, in api.ProductInput) (api.ProductDTO, error) {
	existing, err := a.svc.Get(ctx, id)
	if err != nil {
		return api.ProductDTO{}, mapErr(err, catalog.ErrNotFound, "product_not_found", "商品不存在")
	}
	p, err := a.svc.Update(ctx, id, fromProductInput(in, existing))
	if err != nil {
		return api.ProductDTO{}, mapErr(err, catalog.ErrInvalid, "invalid_product", "商品参数不合法")
	}
	return toProductDTO(p), nil
}

func (a *catalogAdapter) Delete(ctx context.Context, id int64) error {
	err := a.svc.Delete(ctx, id)
	if err != nil {
		return mapErr(err, catalog.ErrNotFound, "product_not_found", "商品不存在")
	}
	return nil
}

func (a *catalogAdapter) Stock(ctx context.Context, productID int64) (int, error) {
	p, err := a.svc.Get(ctx, productID)
	if err != nil {
		return 0, mapErr(err, catalog.ErrNotFound, "product_not_found", "商品不存在")
	}
	n, err := a.svc.Stock(ctx, p)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// --- orders ---

type orderAdapter struct {
	svc *order.Service
}

func toOrderDTO(o order.Order) api.OrderDTO {
	dto := api.OrderDTO{
		OrderNo:             o.OrderNo,
		ProductID:           o.ProductID,
		ProductName:         o.ProductName,
		Kind:                o.Kind,
		Quantity:            int(o.Quantity),
		Units:               int(o.Units),
		AmountCents:         o.AmountCents,
		Currency:            o.Currency,
		Provider:            o.Provider,
		Status:              o.Status,
		RenewSubscriptionID: o.RenewSubscriptionID,
		CreatedAt:           o.CreatedAt,
		ExpiresAt:           o.ExpiresAt,
		PaidAt:              o.PaidAt,
		FulfilledAt:         o.FulfilledAt,
		UserID:              o.UserID,
	}
	if o.FulfilError != nil {
		dto.FulfilError = *o.FulfilError
	}
	return dto
}

func (a *orderAdapter) Create(ctx context.Context, userID, productID int64, quantity int, provider string, renew *int64) (api.OrderDTO, error) {
	o, err := a.svc.Create(ctx, order.CreateRequest{
		UserID:              userID,
		ProductID:           productID,
		Quantity:            int64(quantity),
		Provider:            provider,
		RenewSubscriptionID: renew,
	})
	if err != nil {
		return api.OrderDTO{}, mapOrderErr(err)
	}
	return toOrderDTO(o), nil
}

func (a *orderAdapter) Get(ctx context.Context, orderNo string) (api.OrderDTO, error) {
	o, err := a.svc.GetByNo(ctx, orderNo)
	if err != nil {
		return api.OrderDTO{}, mapOrderErr(err)
	}
	return toOrderDTO(o), nil
}

func (a *orderAdapter) ListByUser(ctx context.Context, userID int64, page, pageSize int) ([]api.OrderDTO, int64, error) {
	items, total, err := a.svc.List(ctx, userID, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	out := make([]api.OrderDTO, 0, len(items))
	for _, o := range items {
		out = append(out, toOrderDTO(o))
	}
	return out, total, nil
}

func (a *orderAdapter) AdminList(ctx context.Context, status string, userID int64, page, pageSize int) ([]api.OrderDTO, int64, error) {
	items, total, err := a.svc.ListAdmin(ctx, status, userID, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	out := make([]api.OrderDTO, 0, len(items))
	for _, o := range items {
		out = append(out, toOrderDTO(o))
	}
	return out, total, nil
}

func (a *orderAdapter) Cancel(ctx context.Context, orderNo string) error {
	// userID 0 means admin/system: order.Cancel only checks ownership when set.
	if err := a.svc.Cancel(ctx, 0, orderNo); err != nil {
		return mapOrderErr(err)
	}
	return nil
}

func (a *orderAdapter) MarkPaidByAdmin(ctx context.Context, orderNo, tradeNo string) error {
	o, err := a.svc.GetByNo(ctx, orderNo)
	if err != nil {
		return mapOrderErr(err)
	}
	if err := a.svc.MarkPaid(ctx, o.ID, tradeNo); err != nil {
		return mapOrderErr(err)
	}
	return nil
}

func mapOrderErr(err error) error {
	switch {
	case errors.Is(err, order.ErrReservationOnly):
		return api.Err(err, 422, "product_reservation_only", "该商品暂未开售，可以先预约")
	case errors.Is(err, order.ErrNotFound):
		return api.Err(err, 404, "order_not_found", "订单不存在")
	case errors.Is(err, order.ErrInvalidQuantity):
		return api.Err(err, 422, "invalid_quantity", "购买数量不合法")
	case errors.Is(err, order.ErrProductUnavailable):
		return api.Err(err, 422, "product_unavailable", "该商品已下架")
	case errors.Is(err, order.ErrOutOfStock):
		return api.Err(err, 409, "out_of_stock", "库存不足，请减少数量或稍后再试")
	case errors.Is(err, order.ErrInvalidState):
		return api.Err(err, 409, "order_not_payable", "订单当前状态不可支付")
	case errors.Is(err, order.ErrRenewMismatch):
		return api.Err(err, 422, "renew_mismatch", "续费商品与订阅不匹配")
	case errors.Is(err, order.ErrProviderUnavailable):
		return api.Err(err, 422, "provider_unavailable", "支付方式不可用")
	}
	return err
}

func mapErr(err, sentinel error, code, msg string) error {
	if errors.Is(err, sentinel) {
		return api.Err(sentinel, 404, code, msg)
	}
	return err
}

var _ = time.Now

func (a *orderAdapter) Refulfill(ctx context.Context, orderNo string) error {
	o, err := a.svc.GetByNo(ctx, orderNo)
	if err != nil {
		return mapOrderErr(err)
	}
	if err := a.svc.Refulfill(ctx, o.ID); err != nil {
		return mapOrderErr(err)
	}
	return nil
}
