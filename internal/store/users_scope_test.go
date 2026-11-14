package store

import (
	"context"
	"testing"
)

func TestUsersReadsPoolID(t *testing.T) {
	db := openTest(t)
	users := NewUsers(db)
	ctx := context.Background()
	u, err := users.Create(ctx, "pool@example.com", "hash", "user")
	if err != nil || u.VM2APIPoolID != "" {
		t.Fatalf("new user = %+v, %v", u, err)
	}
	if _, err := db.Exec(`UPDATE users SET vm2api_pool_id='pool-test' WHERE id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	byID, err := users.GetByID(ctx, u.ID)
	if err != nil || byID.VM2APIPoolID != "pool-test" {
		t.Fatalf("by id = %+v, %v", byID, err)
	}
	byEmail, err := users.GetByEmail(ctx, u.Email)
	if err != nil || byEmail.VM2APIPoolID != "pool-test" {
		t.Fatalf("by email = %+v, %v", byEmail, err)
	}
	items, total, err := users.Search(ctx, UserFilter{Q: "pool"}, 1, 20)
	if err != nil || total != 1 || len(items) != 1 || items[0].VM2APIPoolID != "pool-test" {
		t.Fatalf("search = %+v, %d, %v", items, total, err)
	}
}
