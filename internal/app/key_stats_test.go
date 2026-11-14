package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

func TestKeyStatsIncludesDeletedPreviousKey(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	fake := vm2api.NewFake()
	stamp := time.Now().UTC().Add(-24 * time.Hour).Truncate(24 * time.Hour).Add(17 * time.Hour)
	user, err := fake.CreateUser(ctx, "service", "password")
	if err != nil {
		t.Fatal(err)
	}
	fake.AddVM(vm2api.VM{ID: "vm-stats", OwnerUserID: user.ID, Platform: "anthropic", HasCredential: true, ScheduleState: "on"})
	old, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{UserID: user.ID, Scope: vm2api.KeyScope{AllowedVMs: []string{"vm-stats"}}})
	if err != nil {
		t.Fatal(err)
	}
	live, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{UserID: user.ID, Scope: vm2api.KeyScope{AllowedVMs: []string{"vm-stats"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []vm2api.Key{old, live} {
		if err := fake.AddRequestLog(user.ID, vm2api.LogRow{CreatedAt: stamp, KeyID: key.ID, VMID: "vm-stats", Model: "claude", InputTokens: 100, OutputTokens: 10, StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
	}
	if err := fake.DeleteKey(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,email,password_hash,created_at,updated_at) VALUES(1,'stats@example.com','hash',?,?)`, store.Now(), store.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO api_keys(id,user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,created_at,updated_at,previous_vm2api_key_ids)
		VALUES(1,1,'stats','hash','prefix',?,'enc','active',?,?,?)`, live.ID, store.Now(), store.Now(), store.EncodeStrings([]string{old.ID, live.ID})); err != nil {
		t.Fatal(err)
	}
	p := &provisionSpan{db: db.DB, keyStats: fake.KeyStats, keyLogs: fake.UserRequestLogs}
	daily, models, err := p.KeyStats(ctx, 1, 1)
	wantDate := stamp.In(time.FixedZone("Shanghai", 8*60*60)).Format("2006-01-02")
	if err != nil || len(daily) != 1 || daily[0].Date != wantDate || daily[0].Requests != 2 || daily[0].Tokens != 220 || len(models) != 1 || models[0].Requests != 2 || models[0].Tokens != 220 {
		t.Fatalf("stats = %+v, %+v, %v", daily, models, err)
	}
}
