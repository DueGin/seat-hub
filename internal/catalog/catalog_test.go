package catalog_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DueGin/vm2api-portal/internal/catalog"
	"github.com/DueGin/vm2api-portal/internal/store"
)

func database(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	if err := (&store.DB{DB: database}).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return database
}

func product() catalog.Product {
	return catalog.Product{Kind: catalog.KindConcurrency, Name: "并发套餐", UnitQuantity: 5, DurationDays: 30, PriceCents: 9900, MinQty: 1, MaxQty: 20, Enabled: true}
}

func TestValidate(t *testing.T) {
	if err := catalog.Validate(product()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*catalog.Product)
	}{
		{"kind", func(candidate *catalog.Product) { candidate.Kind = "other" }},
		{"name", func(candidate *catalog.Product) { candidate.Name = " " }},
		{"units", func(candidate *catalog.Product) { candidate.UnitQuantity = 0 }},
		{"duration", func(candidate *catalog.Product) { candidate.DurationDays = -1 }},
		{"price", func(candidate *catalog.Product) { candidate.PriceCents = -1 }},
		{"minimum", func(candidate *catalog.Product) { candidate.MinQty = 0 }},
		{"maximum", func(candidate *catalog.Product) { candidate.MaxQty = 0 }},
	}
	for _, testcase := range cases {
		t.Run(testcase.name, func(t *testing.T) {
			candidate := product()
			testcase.change(&candidate)
			if err := candidate.Validate(); !errors.Is(err, catalog.ErrInvalid) {
				t.Fatalf("got %v, want invalid product", err)
			}
		})
	}
	candidate := product()
	candidate.PriceCents = 0
	if err := candidate.Validate(); err != nil {
		t.Fatalf("free product: %v", err)
	}
}

func TestCRUDAndPublicList(t *testing.T) {
	ctx := context.Background()
	database := database(t)
	service := catalog.New(database, nil)
	created, err := service.Create(ctx, product())
	if err != nil {
		t.Fatal(err)
	}
	if created.ID <= 0 || created.Currency != "CNY" || created.Stock != nil {
		t.Fatalf("unexpected product: %+v", created)
	}
	if _, err := time.Parse(time.RFC3339, created.CreatedAt); err != nil {
		t.Fatal(err)
	}
	created.Enabled = false
	created.Name = "已下架"
	if _, err := service.Update(ctx, created.ID, created); err != nil {
		t.Fatal(err)
	}
	public, err := service.ListPublic(ctx)
	if err != nil || len(public) != 0 {
		t.Fatalf("public=%+v, err=%v", public, err)
	}
	all, err := service.List(ctx)
	if err != nil || len(all) != 1 || all[0].Name != "已下架" {
		t.Fatalf("all=%+v, err=%v", all, err)
	}
	if err := service.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, created.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, created.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := service.Update(ctx, created.ID, created); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatal(err)
	}
}

func insertOrder(t *testing.T, database *sql.DB, productID int64, number, status string, units int64, expires time.Time, renewal bool) {
	t.Helper()
	var renewID any
	if renewal {
		renewID = int64(1)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := database.Exec(`INSERT INTO orders (order_no, user_id, product_id, product_snapshot, kind, quantity, units, duration_days, amount_cents, currency, provider, status, renew_subscription_id, created_at, expires_at, updated_at) VALUES (?, 1, ?, '{}', 'rental_account', ?, ?, 30, 100, 'CNY', 'manual', ?, ?, ?, ?, ?)`, number, productID, units, units, status, renewID, now, expires.UTC().Format(time.RFC3339), now)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRentalStockSharedPoolAndDeleteReferenced(t *testing.T) {
	ctx := context.Background()
	database := database(t)
	health := func(vmID string) (bool, bool) {
		return vmID != "unhealthy", vmID != "unfetched"
	}
	service := catalog.New(database, health)
	candidate := product()
	candidate.Kind, candidate.UnitQuantity = catalog.KindRentalAccount, 2
	created, err := service.Create(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	for _, vmID := range []string{"a", "b", "c", "d", "e", "unhealthy", "unfetched"} {
		if _, err := database.Exec(`INSERT INTO rental_pool(vm_id, status, selfcheck_state, created_at, updated_at) VALUES (?, 'available', 'pass', '', '')`, vmID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`INSERT INTO rental_pool(vm_id, status, created_at, updated_at) VALUES ('leased', 'leased', '', ''), ('maintenance', 'maintenance', '', '')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	insertOrder(t, database, created.ID, "reserved", "pending", 1, now.Add(time.Hour), false)
	insertOrder(t, database, created.ID, "stale", "pending", 10, now.Add(-time.Second), false)
	insertOrder(t, database, created.ID, "cancelled", "cancelled", 10, now.Add(time.Hour), false)
	insertOrder(t, database, created.ID, "renewal", "pending", 10, now.Add(time.Hour), true)
	stock, err := service.Stock(ctx, created)
	if err != nil || stock != 2 {
		t.Fatalf("stock=%d, err=%v", stock, err)
	}
	public, err := service.ListPublic(ctx)
	if err != nil || len(public) != 1 || public[0].Stock == nil || *public[0].Stock != 2 {
		t.Fatalf("public=%+v, err=%v", public, err)
	}
	// Without a health probe a VM counts as available: an unconfigured probe
	// must read as "unknown", never as "sold out" (a fresh install would
	// otherwise be unable to sell a single rental account).
	unknown := catalog.New(database, nil)
	if stock, err := unknown.Stock(ctx, created); err != nil || stock != 3 {
		t.Fatalf("unknown health stock=%d, err=%v", stock, err)
	}
	if err := service.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	retained, err := service.Get(ctx, created.ID)
	if err != nil || retained.Enabled {
		t.Fatalf("referenced product=%+v, err=%v", retained, err)
	}
	insertOrder(t, database, created.ID, "overreserved", "pending", 20, now.Add(time.Hour), false)
	if stock, err := service.Stock(ctx, created); err != nil || stock != 0 {
		t.Fatalf("overreserved stock=%d, err=%v", stock, err)
	}
}

func TestAvailableVMsRequiresPassedSelfcheck(t *testing.T) {
	for _, withHealth := range []bool{false, true} {
		name := "without_health"
		if withHealth {
			name = "with_health"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			database := database(t)
			for _, item := range []struct{ vmID, status, selfcheck string }{
				{"healthy", "available", "pass"},
				{"unfetched", "available", "pass"},
				{"unhealthy", "available", "pass"},
				{"pending", "available", "pending"},
				{"failed", "available", "fail"},
				{"exhausted", "available", "exhausted"},
				{"running", "available", "running"},
				{"unknown", "available", ""},
				{"leased", "leased", "pass"},
				{"maintenance", "maintenance", "pass"},
			} {
				if _, err := database.Exec(`INSERT INTO rental_pool(vm_id,status,selfcheck_state,created_at,updated_at)
					VALUES (?,?,?,'','')`, item.vmID, item.status, item.selfcheck); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.Exec(`INSERT INTO rental_pool(vm_id,status,created_at,updated_at)
				VALUES ('unchecked','available','','')`); err != nil {
				t.Fatal(err)
			}
			service := catalog.New(database, nil)
			want := int64(3)
			probed := make(map[string]bool)
			if withHealth {
				service.SetHealth(func(vmID string) (bool, bool) {
					probed[vmID] = true
					return vmID != "unhealthy", vmID != "unfetched"
				})
				want = 2
			}
			available, err := service.AvailableVMs(ctx)
			if err != nil || available != want {
				t.Fatalf("available=%d, err=%v; want %d", available, err, want)
			}
			if withHealth && (len(probed) != 3 || !probed["healthy"] || !probed["unhealthy"] || !probed["unfetched"]) {
				t.Fatalf("health probe received unchecked or unavailable VMs: %v", probed)
			}
		})
	}
}
