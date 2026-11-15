package provision

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

// The production case behind v0.4.1: a legacy tenant rents vm-08, and someone
// put vm-08 into a pool of their own ("GUO") in the vm2api panel. Migration
// must not touch the tenant, must report the pool by name as a dashboard
// warning without failing the job, and must finish by itself once the VM is
// taken out of that pool.
func TestForeignPoolBlocksMigrationAsWarningThenRecovers(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	box, err := secret.New("foreign-pool-test-key-32-bytes-minimum")
	if err != nil {
		t.Fatal(err)
	}
	service.Box = box
	var legacy string
	if err := service.db.QueryRow(`SELECT vm2api_user_id FROM users WHERE id=?`, userID).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	vm, err := fake.CreateHostedVM(ctx, "vm-08", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetVMOwner(ctx, vm.ID, &legacy); err != nil {
		t.Fatal(err)
	}
	addResource(t, service, userID, "rental", "active", vm.ID, nil)
	old, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{Name: "claude", UserID: legacy, MaxConcurrency: 1, Scope: vm2api.KeyScope{AllowedVMs: []string{vm.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal(old.Secret)
	if err != nil {
		t.Fatal(err)
	}
	now := timestamp(service.now())
	if _, err := service.db.Exec(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,created_at,updated_at) VALUES (?,'claude','legacy-hash','sk-pt-',?,?,'active',?,?)`, userID, old.ID, sealed, now, now); err != nil {
		t.Fatal(err)
	}
	guo, err := fake.CreatePool(ctx, "GUO", []string{vm.ID})
	if err != nil {
		t.Fatal(err)
	}

	if err := service.migrateJob(ctx); err != nil {
		t.Fatalf("scope_migrate must absorb the foreign pool, got %v", err)
	}
	if err := service.ReconcileScopes(ctx); err != nil {
		t.Fatalf("reconcile must absorb the foreign pool, got %v", err)
	}
	warnings, err := service.LoadScopeWarnings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings.Items) != 1 || warnings.CheckedAt == "" {
		t.Fatalf("warnings=%+v", warnings)
	}
	warning := warnings.Items[0]
	if warning.UserID != userID || warning.VMID != vm.ID || warning.PoolID != guo.ID || warning.PoolName != "GUO" || !strings.Contains(warning.Message, "「GUO」") {
		t.Fatalf("warning=%+v", warning)
	}
	// Nothing moved: the tenant keeps working on the legacy setup.
	usage, err := fake.GetAccountUsage(ctx, vm.ID)
	if err != nil || usage.VM.OwnerUserID != legacy {
		t.Fatalf("vm owner=%q err=%v", usage.VM.OwnerUserID, err)
	}
	if info, err := fake.GetKey(ctx, old.ID); err != nil || !info.Active {
		t.Fatalf("legacy key=%+v err=%v", info, err)
	}

	// The admin takes vm-08 out of GUO; the next reconcile finishes the job
	// and clears the warning.
	if _, err := fake.SetPoolMembers(ctx, guo.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcileScopes(ctx); err != nil {
		t.Fatal(err)
	}
	warnings, err = service.LoadScopeWarnings(ctx)
	if err != nil || len(warnings.Items) != 0 {
		t.Fatalf("warnings after fix=%+v err=%v", warnings, err)
	}
	var stillLegacy string
	var poolID string
	if err := service.db.QueryRow(`SELECT COALESCE(vm2api_user_id,''),COALESCE(vm2api_pool_id,'') FROM users WHERE id=?`, userID).Scan(&stillLegacy, &poolID); err != nil {
		t.Fatal(err)
	}
	if stillLegacy != "" || poolID == "" {
		t.Fatalf("legacy=%q pool=%q", stillLegacy, poolID)
	}
	if pool := scopePool(t, service, userID); len(pool.VMIDs) != 1 || pool.VMIDs[0] != vm.ID {
		t.Fatalf("pool=%+v", pool)
	}
}

// An event-driven sync that hits a foreign pool records the warning and does
// not fail (and so does not burn eight retries on it).
func TestForeignPoolSyncJobRecordsWarning(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	_, vmID := scopeVM(t, service, fake, userID, "active")
	if _, err := fake.CreatePool(ctx, "manual", []string{vmID}); err != nil {
		t.Fatal(err)
	}
	err := service.SyncUserScope(ctx, userID)
	var foreign *ForeignPoolError
	if !errors.As(err, &foreign) || foreign.VMID != vmID || foreign.PoolName != "manual" {
		t.Fatalf("sync err=%v", err)
	}
	job := Job{Payload: []byte(`{"user_id":` + strconv.FormatInt(userID, 10) + `}`)}
	if err := service.scopeSyncJob(ctx, job); err != nil {
		t.Fatalf("sync_scope job err=%v", err)
	}
	warnings, err := service.LoadScopeWarnings(ctx)
	if err != nil || len(warnings.Items) != 1 || warnings.Items[0].VMID != vmID {
		t.Fatalf("warnings=%+v err=%v", warnings, err)
	}
	// Running it again does not duplicate the warning.
	if err := service.scopeSyncJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if warnings, _ := service.LoadScopeWarnings(ctx); len(warnings.Items) != 1 {
		t.Fatalf("duplicated warnings=%+v", warnings)
	}
}

// A rental VM held by a foreign pool is never leased: the next clean VM is,
// and with none left the allocation reports out of stock.
func TestForeignPoolRentalIsNotLeased(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	held, err := fake.CreateHostedVM(ctx, "a-held", nil)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := fake.CreateHostedVM(ctx, "b-clean", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := timestamp(service.now())
	for _, id := range []string{held.ID, clean.ID} {
		if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,selfcheck_state,created_at,updated_at) VALUES (?,'available','pass',?,?)`, id, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fake.CreatePool(ctx, "manual", []string{held.ID}); err != nil {
		t.Fatal(err)
	}
	sub := addSubscription(t, service, userID, "rental_account", 1, service.now().Add(time.Hour))
	if err := service.AllocateRental(ctx, sub); err != nil {
		t.Fatal(err)
	}
	var leased string
	if err := service.db.QueryRow(`SELECT vm_id FROM resources WHERE user_id=? AND kind='rental'`, userID).Scan(&leased); err != nil {
		t.Fatal(err)
	}
	if leased != clean.ID {
		t.Fatalf("leased %s, want the clean VM %s", leased, clean.ID)
	}
	more := addSubscription(t, service, userID, "rental_account", 1, service.now().Add(time.Hour))
	if err := service.AllocateRental(ctx, more); !errors.Is(err, ErrOutOfStock) {
		t.Fatalf("second allocation err=%v, want out of stock", err)
	}
	var status string
	if err := service.db.QueryRow(`SELECT status FROM rental_pool WHERE vm_id=?`, held.ID).Scan(&status); err != nil || status != "available" {
		t.Fatalf("held VM status=%q err=%v", status, err)
	}
}
