package order_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/catalog"
	"github.com/DueGin/seat-hub/internal/order"
)

func TestPaymentCloseAfterCancelOrExpiry(t *testing.T) {
	for _, action := range []string{"cancel", "expire"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t, catalog.KindConcurrency, nil)
			purchase, err := f.orders.Create(context.Background(), order.CreateRequest{UserID: 1, ProductID: f.product.ID, Quantity: 1, Provider: "alipay_f2f"})
			if err != nil {
				t.Fatal(err)
			}
			want := order.StatusCancelled
			if action == "expire" {
				want = order.StatusExpired
			}
			calls := 0
			f.orders.SetPaymentCloser(func(ctx context.Context, provider, orderNo string) error {
				calls++
				if provider != "alipay_f2f" || orderNo != purchase.OrderNo {
					t.Fatalf("close wrong order: %s %s", provider, orderNo)
				}
				current, err := f.orders.GetByNo(ctx, orderNo)
				if err != nil || current.Status != want {
					t.Fatalf("close before local transition: %+v %v", current, err)
				}
				return errors.New("gateway unavailable")
			})
			if action == "cancel" {
				if err := f.orders.Cancel(context.Background(), 2, purchase.OrderNo); !errors.Is(err, order.ErrNotFound) {
					t.Fatalf("other user cancel: %v", err)
				}
				if calls != 0 {
					t.Fatal("unauthorized close")
				}
				if err := f.orders.Cancel(context.Background(), 1, purchase.OrderNo); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.db.Exec(`UPDATE orders SET expires_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), purchase.ID); err != nil {
					t.Fatal(err)
				}
				if count, err := f.orders.ExpireStale(context.Background()); err != nil || count != 1 {
					t.Fatalf("expire: %d %v", count, err)
				}
				if count, err := f.orders.ExpireStale(context.Background()); err != nil || count != 0 {
					t.Fatalf("expire again: %d %v", count, err)
				}
			}
			if calls != 1 {
				t.Fatalf("close called %d times", calls)
			}
		})
	}
}
