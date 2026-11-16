package order_test

import (
	"context"
	"errors"
	"testing"

	"github.com/DueGin/seat-hub/internal/catalog"
	"github.com/DueGin/seat-hub/internal/order"
)

func TestReservationOnlyCannotCreateOrder(t *testing.T) {
	f := setup(t, catalog.KindRentalAccount, nil)
	f.product.ReservationOnly = true
	if _, err := f.products.Update(context.Background(), f.product.ID, f.product); err != nil {
		t.Fatal(err)
	}
	_, err := f.orders.Create(context.Background(), order.CreateRequest{UserID: 1, ProductID: f.product.ID, Quantity: 1, Provider: "manual"})
	if !errors.Is(err, order.ErrReservationOnly) {
		t.Fatalf("got %v", err)
	}
	var n int
	f.db.QueryRow(`SELECT count(*) FROM orders`).Scan(&n)
	if n != 0 {
		t.Fatal("reservation product created order")
	}
}
