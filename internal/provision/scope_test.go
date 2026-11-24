package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

func execOwnerFixture(t *testing.T, service *Service, query string, args ...any) {
	t.Helper()
	if _, err := service.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func seedOwnerKey(t *testing.T, service *Service, userID int64, upstream, owner, label string) int64 {
	t.Helper()
	stamp := timestamp(service.now())
	var keyID int64
	if err := service.db.QueryRow(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,upstream_owner,created_at,updated_at) VALUES (?, ?, ?, 'sk-pt-fixt', ?, 'old-ciphertext','active',?,?,?) RETURNING id`, userID, label, service.Box.MAC("sk-pt-fixture-"+label), upstream, owner, stamp, stamp).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	return keyID
}

func ownerKey(t *testing.T, fake *vm2api.Fake, owner, pool string) vm2api.Key {
	t.Helper()
	scope := vm2api.KeyScope{All: true}
	if pool != "" {
		scope = vm2api.KeyScope{PoolID: pool}
	}
	key, err := fake.CreateKey(context.Background(), vm2api.CreateKeyInput{Name: "测试密钥", UserID: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestOwnerMigrationProductionState(t *testing.T) {
	service, fake, first := provisionFixture(t)
	ctx := context.Background()
	server := httptest.NewServer(fake.Handler("master"))
	defer server.Close()
	service.client = vm2api.NewHTTPClient(server.URL, "master", server.Client())
	legacy, _ := fake.CreateUser(ctx, "seathub_xxxx", "unknown-to-portal")
	one, _ := fake.CreateUser(ctx, "portal_1", "forgotten-password")
	two, _ := fake.CreateUser(ctx, "portal_2", "another-forgotten-password")
	jason, _ := fake.CreateUser(ctx, "Jason", "manual-password")
	for _, upstream := range []vm2api.User{one, two} {
		if err := fake.SetUserEnabled(ctx, upstream.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveServiceIdentity(ctx, service.db, store.ServiceIdentity{UserID: legacy.ID, Username: legacy.Username}); err != nil {
		t.Fatal(err)
	}
	stamp := timestamp(service.now())
	execOwnerFixture(t, service, `UPDATE users SET vm2api_user_id=NULL,vm2api_username='portal_1' WHERE id=?`, first)
	pools := make(map[int64]string)
	for _, userID := range []int64{1, 2, 3, 4, 7, 8} {
		if userID != first {
			execOwnerFixture(t, service, `INSERT INTO users(id,email,password_hash,vm2api_username,created_at,updated_at) VALUES(?,?,'hash',?,?,?)`, userID, fmt.Sprintf("u%d@example.com", userID), nullText(map[int64]string{2: "portal_2"}[userID]), stamp, stamp)
		}
		pool, err := fake.CreatePool(ctx, fmt.Sprintf("sh-xxxx-u%d", userID), nil)
		if err != nil {
			t.Fatal(err)
		}
		pools[userID] = pool.ID
		execOwnerFixture(t, service, `UPDATE users SET vm2api_pool_id=? WHERE id=?`, pool.ID, userID)
	}
	// The service keys are invisible to Master, not necessarily deleted.
	serviceKey := ownerKey(t, fake, legacy.ID, pools[1])
	if _, err := service.client.GetKey(ctx, serviceKey.ID); !vm2api.IsStatus(err, 404) {
		t.Fatalf("master could see service-owned key: %v", err)
	}
	ownerless := ownerKey(t, fake, "", pools[2])
	foreignKey := ownerKey(t, fake, jason.ID, pools[2])
	keys := []struct {
		userID     int64
		old, label string
	}{
		{1, serviceKey.ID, "service"}, {1, "missing-key-1", "missing-one"},
		{2, ownerless.ID, "ownerless"}, {2, "missing-key-2", "missing-two"},
		{8, "missing-key-8", "missing-eight"},
	}
	var keyIDs []int64
	for _, key := range keys {
		keyIDs = append(keyIDs, seedOwnerKey(t, service, key.userID, key.old, legacy.ID, key.label))
	}
	fake.AddVM(vm2api.VM{ID: "vm-08", OwnerUserID: jason.ID, HasCredential: true})
	execOwnerFixture(t, service, `INSERT INTO rental_pool(vm_id,status,selfcheck_state,created_at,updated_at) VALUES ('vm-08','available','pass',?,?)`, stamp, stamp)
	seen := map[int64]string{}
	for pass := 0; pass < 2; pass++ {
		if err := service.ReconcileOwners(ctx); err != nil {
			t.Fatal(err)
		}
		for index, fixture := range keys {
			id := keyIDs[index]
			var live, owner, previous, hash, prefix, enc, savedOwner, username string
			if err := service.db.QueryRow(`SELECT k.vm2api_key_id,k.upstream_owner,k.previous_vm2api_key_ids,k.key_hash,k.prefix,k.vm2api_secret_enc,u.vm2api_user_id,u.vm2api_username FROM api_keys k JOIN users u ON u.id=k.user_id WHERE k.id=?`, id).Scan(&live, &owner, &previous, &hash, &prefix, &enc, &savedOwner, &username); err != nil {
				t.Fatal(err)
			}
			if owner != savedOwner || owner == "" || live == fixture.old || !reflect.DeepEqual(store.DecodeStrings(previous), []string{fixture.old}) || prefix != "sk-pt-fixt" || hash != service.Box.MAC("sk-pt-fixture-"+fixture.label) {
				t.Fatalf("portal key %d lost identity/history: live=%s owner=%s history=%s", id, live, owner, previous)
			}
			if expected := map[int64]string{1: one.ID, 2: two.ID}[fixture.userID]; expected != "" && expected != owner {
				t.Fatalf("recorded identity not adopted: %s != %s", owner, expected)
			}
			if _, enabled := fake.UserEnabled(owner); !enabled || username != fmt.Sprintf("portal_%d", fixture.userID) {
				t.Fatalf("unexpected identity %s enabled=%v", username, enabled)
			}
			info, err := fake.GetKey(ctx, live)
			secret, openErr := service.Box.Open(enc)
			if err != nil || openErr != nil || !strings.HasPrefix(secret, "sk-vm-") || !info.Active || info.UserID != owner || info.GroupType != "all" || info.PoolID != "" {
				t.Fatalf("live key not repaired: %+v, %v, %v", info, err, openErr)
			}
			if pass == 1 && seen[id] != live {
				t.Fatalf("idempotent pass rotated key %d", id)
			}
			seen[id] = live
		}
		for _, id := range []string{serviceKey.ID, ownerless.ID} {
			if exists, _ := fake.KeyState(id); exists {
				t.Fatalf("legacy key %s was not removed", id)
			}
		}
		left, err := fake.ListPools(ctx)
		if err != nil || len(left) != 1 || left[0].ID != pools[2] {
			t.Fatalf("leftover pools=%+v, %v", left, err)
		}
		var linked int
		if err := service.db.QueryRow(`SELECT count(*) FROM users WHERE COALESCE(vm2api_pool_id,'')<>''`).Scan(&linked); err != nil || linked != 0 {
			t.Fatalf("still linked pools: %d, %v", linked, err)
		}
		if exists, enabled := fake.UserEnabled(legacy.ID); !exists || enabled {
			t.Fatal("legacy user did not retire")
		}
		if exists, enabled := fake.UserEnabled(jason.ID); !exists || !enabled {
			t.Fatal("foreign user was modified")
		}
		if exists, active := fake.KeyState(foreignKey.ID); !exists || !active {
			t.Fatal("foreign key was modified")
		}
		warnings, err := service.LoadScopeWarnings(ctx)
		if err != nil || warnings.CheckedAt == "" || len(warnings.Items) != 2 || !slices.ContainsFunc(warnings.Items, func(w ScopeWarning) bool {
			return w.Kind == PoolInUse && w.UserID == 2 && strings.Contains(w.Message, "sh-xxxx-u2")
		}) || !slices.ContainsFunc(warnings.Items, func(w ScopeWarning) bool {
			return w.Kind == RentalVMTaken && w.VMID == "vm-08" && strings.Contains(w.Message, "Jason")
		}) {
			t.Fatalf("warnings=%+v, %v", warnings, err)
		}
	}
	if err := service.FulfilOrder(ctx, addOrder(t, service, first, "rental_account", 1, nil)); !errors.Is(err, ErrOutOfStock) {
		t.Fatalf("owned-only pool should be out of stock, got %v", err)
	}
	fake.AddVM(vm2api.VM{ID: "vm-09", HasCredential: true})
	execOwnerFixture(t, service, `INSERT INTO rental_pool(vm_id,status,selfcheck_state,created_at,updated_at) VALUES ('vm-09','available','pass',?,?)`, stamp, stamp)
	if err := service.FulfilOrder(ctx, addOrder(t, service, first, "rental_account", 1, nil)); err != nil {
		t.Fatal(err)
	}
	for vm, owner := range map[string]string{"vm-08": jason.ID, "vm-09": one.ID} {
		usage, err := fake.GetAccountUsage(ctx, vm)
		if err != nil || usage.VM.OwnerUserID != owner {
			t.Fatalf("vm %s ownership = %s, %v", vm, usage.VM.OwnerUserID, err)
		}
	}
}

func TestOwnerReconcileForeignDriftAndOrphans(t *testing.T) {
	service, fake, first := provisionFixture(t)
	ctx := context.Background()
	other := first + 1
	stamp := timestamp(service.now())
	execOwnerFixture(t, service, `INSERT INTO users(id,email,password_hash,created_at,updated_at) VALUES(?, 'second@example.com','hash',?,?)`, other, stamp, stamp)
	one, err := service.EnsureUpstreamUser(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := service.EnsureUpstreamUser(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := fake.CreateUser(ctx, "seathub_test", "unknown-to-portal")
	foreign, _ := fake.CreateUser(ctx, "foreign", "foreign-pass")
	if err := store.SaveServiceIdentity(ctx, service.db, store.ServiceIdentity{UserID: legacy.ID, Username: legacy.Username}); err != nil {
		t.Fatal(err)
	}
	orphan := ownerKey(t, fake, two, "")
	untouched := ownerKey(t, fake, foreign.ID, "")
	foreignDrift := ownerKey(t, fake, two, "")
	emptyDrift := ownerKey(t, fake, "", "")
	legacyDrift := ownerKey(t, fake, legacy.ID, "")
	for _, old := range []string{foreignDrift.ID, emptyDrift.ID, legacyDrift.ID, "missing"} {
		seedOwnerKey(t, service, first, old, one, old)
	}
	for vm, owner := range map[string]string{"foreign-vm": foreign.ID, "legacy-vm": legacy.ID, "empty-vm": ""} {
		fake.AddVM(vm2api.VM{ID: vm, OwnerUserID: owner, HasCredential: true})
		addResource(t, service, first, "hosted", "active", vm, nil)
	}
	if err := service.ReconcileOwners(ctx); err != nil {
		t.Fatal(err)
	}
	keys, err := service.userScopeKeys(ctx, first)
	if err != nil || len(keys) != 4 {
		t.Fatalf("keys=%+v, %v", keys, err)
	}
	for _, key := range keys {
		info, err := fake.GetKey(ctx, key.upstream)
		if err != nil || info.UserID != one || !info.Active || key.previous == "[]" {
			t.Fatalf("referenced key not restored: %+v, %v", info, err)
		}
	}
	for id, expected := range map[string]bool{foreignDrift.ID: true, untouched.ID: true, orphan.ID: false} {
		if exists, active := fake.KeyState(id); !exists || active != expected {
			t.Fatalf("key %s: exists=%v active=%v", id, exists, active)
		}
	}
	for _, key := range []vm2api.Key{emptyDrift, legacyDrift} {
		if exists, _ := fake.KeyState(key.ID); exists {
			t.Fatalf("old key retained: %s", key.ID)
		}
	}
	for vm, owner := range map[string]string{"foreign-vm": foreign.ID, "legacy-vm": one, "empty-vm": one} {
		usage, err := fake.GetAccountUsage(ctx, vm)
		if err != nil || usage.VM.OwnerUserID != owner {
			t.Fatalf("vm %s ownership = %s, %v", vm, usage.VM.OwnerUserID, err)
		}
	}
	warnings, err := service.LoadScopeWarnings(ctx)
	if err != nil || len(warnings.Items) != 2 || !slices.ContainsFunc(warnings.Items, func(w ScopeWarning) bool {
		return w.Kind == KeyDrifted && w.UserID == first && w.KeyID == foreignDrift.ID
	}) || !slices.ContainsFunc(warnings.Items, func(w ScopeWarning) bool {
		return w.Kind == VMOwnerConflict && w.UserID == first && w.VMID == "foreign-vm"
	}) {
		t.Fatalf("warnings=%+v, %v", warnings, err)
	}
	if err := fake.SetVMOwner(ctx, "foreign-vm", nil); err != nil {
		t.Fatal(err)
	}
	if err := fake.DeleteKey(ctx, foreignDrift.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.ReconcileOwners(ctx); err != nil {
		t.Fatal(err)
	}
	if warnings, err := service.LoadScopeWarnings(ctx); err != nil || len(warnings.Items) != 0 {
		t.Fatalf("resolved warnings remained: %+v, %v", warnings, err)
	}
}

func TestOwnerReconcileCleansLegacyKeysWithoutEnablingDisabledUser(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	owner, err := service.recordedOwner(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := fake.CreateUser(ctx, "seathub_disabled", "unknown-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveServiceIdentity(ctx, service.db, store.ServiceIdentity{UserID: legacy.ID, Username: legacy.Username}); err != nil {
		t.Fatal(err)
	}
	live := ownerKey(t, fake, owner, "")
	old := ownerKey(t, fake, legacy.ID, "")
	keyID := seedOwnerKey(t, service, userID, live.ID, owner, "disabled-user")
	execOwnerFixture(t, service, `UPDATE api_keys SET previous_vm2api_key_ids=? WHERE id=?`, store.EncodeStrings([]string{old.ID}), keyID)
	if err := service.SetUserStatus(ctx, userID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if exists, active := fake.KeyState(live.ID); !exists || active {
		t.Fatalf("member key: exists=%v active=%v", exists, active)
	}
	if exists, _ := fake.KeyState(old.ID); exists {
		t.Fatal("old service key survived user disable")
	}
	deleted := ownerKey(t, fake, "", "")
	deletedID := seedOwnerKey(t, service, userID, deleted.ID, legacy.ID, "deleted-key")
	execOwnerFixture(t, service, `UPDATE api_keys SET status='deleted' WHERE id=?`, deletedID)
	if err := service.ReconcileOwners(ctx); err != nil {
		t.Fatal(err)
	}
	if exists, _ := fake.KeyState(deleted.ID); exists {
		t.Fatal("ownerless key survived cleanup for disabled user")
	}
	if exists, enabled := fake.UserEnabled(owner); !exists || enabled {
		t.Fatalf("disabled member: exists=%v enabled=%v", exists, enabled)
	}
	if err := service.SetUserStatus(ctx, userID, "active"); err != nil {
		t.Fatal(err)
	}
	if exists, active := fake.KeyState(live.ID); !exists || !active {
		t.Fatalf("re-enabled member key: exists=%v active=%v", exists, active)
	}
}

func TestOwnerUsernameCollisionDoesNotAdoptForeignUser(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	name := fmt.Sprintf("portal_%d", userID)
	foreign, _ := fake.CreateUser(ctx, name, "manual-password")
	execOwnerFixture(t, service, `UPDATE users SET vm2api_user_id=NULL,vm2api_username=NULL WHERE id=?`, userID)
	ownerID, err := service.EnsureUpstreamUser(ctx, userID)
	if err != nil || ownerID == foreign.ID {
		t.Fatalf("unexpected owner %s: %v", ownerID, err)
	}
	owner, err := service.identity(ctx, userID)
	if err != nil || !regexp.MustCompile(`^`+name+`_[0-9a-f]{4}$`).MatchString(owner.name) {
		t.Fatalf("collision username=%q: %v", owner.name, err)
	}
	if _, err := fake.ForUser(ctx, foreign.ID, foreign.Username, "manual-password"); err != nil {
		t.Fatalf("foreign credentials changed: %v", err)
	}
}

type releaseOwnerSpy struct {
	vm2api.Client
	steps []string
}

func (client *releaseOwnerSpy) SetSchedulable(ctx context.Context, vmID string, enabled bool) error {
	client.steps = append(client.steps, "schedule")
	return client.Client.SetSchedulable(ctx, vmID, enabled)
}

func (client *releaseOwnerSpy) SetVMOwner(ctx context.Context, vmID string, owner *string) error {
	client.steps = append(client.steps, "owner")
	return client.Client.SetVMOwner(ctx, vmID, owner)
}

func (client *releaseOwnerSpy) DeleteVM(ctx context.Context, vmID string) error {
	client.steps = append(client.steps, "delete")
	return client.Client.DeleteVM(ctx, vmID)
}

func TestOwnerReleaseOrderAndForeignVMProtection(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	owner, err := service.EnsureUpstreamUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := fake.CreateUser(ctx, "Jason", "foreign-password")
	spy := &releaseOwnerSpy{Client: fake}
	service.client = spy
	for _, test := range []struct {
		kind, owner string
		steps       []string
	}{
		{"rental", owner, []string{"schedule", "owner"}},
		{"hosted", owner, []string{"schedule", "delete"}},
		{"hosted", foreign.ID, nil},
	} {
		vm, err := fake.CreateHostedVM(ctx, "test-slot", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := fake.SetVMOwner(ctx, vm.ID, &test.owner); err != nil {
			t.Fatal(err)
		}
		resource := addResource(t, service, userID, test.kind, "releasing", vm.ID, nil)
		spy.steps = nil
		if err := service.ReleaseResource(ctx, resource); err != nil || !slices.Equal(spy.steps, test.steps) {
			t.Fatalf("%s release: %v, want %v, err %v", test.kind, spy.steps, test.steps, err)
		}
	}
}

func TestOwnerReconcileWaitsForInFlightKeyWrite(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	owner, _ := service.EnsureUpstreamUser(ctx, userID)
	unlock := service.LockScope(userID)
	key := ownerKey(t, fake, owner, "")
	done := make(chan error, 1)
	go func() { done <- service.ReconcileOwners(ctx) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("reconciled while a key write was in flight: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	seedOwnerKey(t, service, userID, key.ID, owner, "in-flight")
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if exists, active := fake.KeyState(key.ID); !exists || !active {
		t.Fatal("new portal key was treated as orphan")
	}
}

func TestOwnerJobsKeepLegacyRowsHarmless(t *testing.T) {
	service, _, _ := provisionFixture(t)
	ctx := context.Background()
	for _, job := range []string{"scope_migrate", "scope_reconcile", "owner_migrate"} {
		if _, err := service.Engine().Enqueue(ctx, job, jobPayload{}, job, service.now(), 8); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.EnqueueScopeReconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := service.Engine().RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	if err := service.db.QueryRow(`SELECT status FROM jobs WHERE type='owner_reconcile'`).Scan(&status); err != nil || status != "succeeded" {
		t.Fatalf("owner reconcile status = %q, %v", status, err)
	}
	var count int
	if err := service.db.QueryRow(`SELECT count(*) FROM jobs WHERE status='succeeded'`).Scan(&count); err != nil && !errors.Is(err, sql.ErrNoRows) || count != 4 {
		t.Fatalf("jobs succeeded=%d, %v", count, err)
	}
}
