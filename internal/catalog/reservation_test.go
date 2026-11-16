package catalog_test

import (
	"context"
	"testing"

	"github.com/DueGin/seat-hub/internal/catalog"
)

func TestReservationFlagRoundTripSkipsStock(t *testing.T) {
	db := database(t)
	ctx := context.Background()
	service := catalog.New(db, func(string) (bool, bool) { t.Fatal("reservation product checked stock"); return false, false })
	input := product()
	input.Kind = catalog.KindRentalAccount
	input.ReservationOnly = true
	created, err := service.Create(ctx, input)
	if err != nil || !created.ReservationOnly {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	// Removing stock storage proves the public listing does not query inventory.
	if _, err := db.Exec(`DROP TABLE rental_pool`); err != nil {
		t.Fatal(err)
	}
	items, err := service.ListPublic(ctx)
	if err != nil || len(items) != 1 || !items[0].ReservationOnly || items[0].Stock != nil {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	created.ReservationOnly = false
	updated, err := service.Update(ctx, created.ID, created)
	if err != nil || updated.ReservationOnly {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
}
