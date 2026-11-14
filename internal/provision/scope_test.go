package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

type countingServiceClient struct {
	vm2api.Client
	mu      sync.Mutex
	creates int
}

func (client *countingServiceClient) CreateUser(ctx context.Context, username, password string) (vm2api.User, error) {
	client.mu.Lock()
	client.creates++
	client.mu.Unlock()
	return client.Client.CreateUser(ctx, username, password)
}
func TestScopeServiceUserConcurrentReuse(t *testing.T) {
	service, fake, _ := provisionFixture(t)
	client := &countingServiceClient{Client: fake}
	service.client = client
	var group sync.WaitGroup
	ids := make(chan string, 20)
	for range 20 {
		group.Go(func() {
			id, err := service.ServiceUser(context.Background())
			if err != nil {
				t.Error(err)
			}
			ids <- id
		})
	}
	group.Wait()
	close(ids)
	identity, err := store.LoadServiceIdentity(context.Background(), service.db)
	if err != nil {
		t.Fatal(err)
	}
	for id := range ids {
		if id == "" || id != identity.UserID {
			t.Fatalf("id=%q identity=%+v", id, identity)
		}
	}
	if client.creates != 1 {
		t.Fatalf("creates=%d", client.creates)
	}
}

type cancelServiceClient struct {
	vm2api.Client
	cancel  context.CancelFunc
	creates int
}

func (client *cancelServiceClient) CreateUser(ctx context.Context, username, password string) (vm2api.User, error) {
	user, err := client.Client.CreateUser(ctx, username, password)
	client.creates++
	client.cancel()
	return user, err
}
func TestScopeServiceUserPersistsAfterCancellation(t *testing.T) {
	service, fake, _ := provisionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &cancelServiceClient{Client: fake, cancel: cancel}
	service.client = client
	owner, err := service.ServiceUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.ServiceUser(context.Background())
	if err != nil || again != owner || client.creates != 1 {
		t.Fatalf("owner=%q again=%q creates=%d err=%v", owner, again, client.creates, err)
	}
}

func scopeVM(t *testing.T, service *Service, fake *vm2api.Fake, userID int64, status string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	owner, err := service.ServiceUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vm, err := fake.CreateHostedVM(ctx, "scope", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetVMOwner(ctx, vm.ID, &owner); err != nil {
		t.Fatal(err)
	}
	return addResource(t, service, userID, "hosted", status, vm.ID, nil), vm.ID
}
func scopePortalKey(t *testing.T, service *Service, userID int64, status, mode string, ids []int64) vm2api.Key {
	t.Helper()
	ctx := context.Background()
	unlock := service.LockScope(userID)
	defer unlock()
	owner, err := service.ServiceUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope, _, err := service.KeyScopeFor(ctx, userID, mode, ids)
	if err != nil {
		t.Fatal(err)
	}
	key, err := service.client.CreateKey(ctx, vm2api.CreateKeyInput{Name: status + mode, UserID: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.db.Exec(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,scope_mode,scope_accounts,upstream_owner,created_at,updated_at) VALUES (?,?,?,'prefix',?,'encrypted',?,?,?,?,?,?)`, userID, status+mode, key.ID, key.ID, status, mode, store.EncodeIDs(ids), owner, timestamp(service.now()), timestamp(service.now()))
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func scopePool(t *testing.T, service *Service, userID int64) vm2api.Pool {
	t.Helper()
	var id string
	if err := service.db.QueryRow(`SELECT vm2api_pool_id FROM users WHERE id=?`, userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	pools, err := service.client.ListPools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range pools {
		if pool.ID == id {
			return pool
		}
	}
	t.Fatalf("pool %q missing", id)
	return vm2api.Pool{}
}

func TestScopePoolsCreateAdoptRecreate(t *testing.T) {
	ctx := context.Background()
	for _, adopt := range []bool{false, true} {
		t.Run(fmt.Sprint(adopt), func(t *testing.T) {
			service, fake, userID := provisionFixture(t)
			owner, err := service.ServiceUser(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if owner == "" {
				t.Fatal("empty service user")
			}
			name, err := service.poolName(ctx, userID)
			if err != nil || !strings.HasPrefix(name, "sh-") || strings.Contains(name, owner[:min(6, len(owner))]+"-") {
				t.Fatalf("pool name=%q err=%v", name, err)
			}
			var expected string
			if adopt {
				pool, err := fake.CreatePool(ctx, name, nil)
				if err != nil {
					t.Fatal(err)
				}
				expected = pool.ID
			}
			scope, usable, err := service.KeyScopeFor(ctx, userID, ScopeAll, nil)
			if err != nil || !usable || scope.PoolID == "" {
				t.Fatalf("scope=%+v usable=%v err=%v", scope, usable, err)
			}
			if adopt && expected != scope.PoolID {
				t.Fatalf("adopted=%s want=%s", scope.PoolID, expected)
			}
			if pool := scopePool(t, service, userID); pool.Name != name {
				t.Fatalf("pool=%+v", pool)
			}
			if err := fake.DeletePool(ctx, scope.PoolID); err != nil {
				t.Fatal(err)
			}
			// The saved id is trusted on the hot path; the next member write
			// notices the 404 and recreates the pool.
			if err := service.SyncUserScope(ctx, userID); err != nil {
				t.Fatal(err)
			}
			again, _, err := service.KeyScopeFor(ctx, userID, ScopeAll, nil)
			if err != nil || again.PoolID == scope.PoolID || again.PoolID == "" {
				t.Fatalf("recreated=%+v err=%v", again, err)
			}
			if err := service.DeleteUserScope(ctx, userID); err != nil {
				t.Fatal(err)
			}
			if err := service.DeleteUserScope(ctx, userID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestScopeSchedulableAndValidation(t *testing.T) {
	service, _, userID := provisionFixture(t)
	ctx := context.Background()
	var included []int64
	for _, status := range []string{"active", "needs_reauth", "suspended", "allocating", "awaiting_auth", "bootstrapping", "failed", "releasing", "released"} {
		id := addResource(t, service, userID, "hosted", status, "vm-"+status, nil)
		if status == "active" || status == "needs_reauth" || status == "suspended" {
			included = append(included, id)
		}
	}
	addResource(t, service, userID, "hosted", "active", "", nil)
	ids, err := service.SchedulableVMs(ctx, userID)
	if err != nil || len(ids) != 3 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	mode, normalized, err := service.ValidateKeyScope(ctx, userID, "", []int64{-1})
	if err != nil || mode != ScopeAll || len(normalized) != 0 {
		t.Fatalf("all=%s %v %v", mode, normalized, err)
	}
	mode, normalized, err = service.ValidateKeyScope(ctx, userID, ScopeSelected, []int64{included[1], included[0], included[1]})
	if err != nil || mode != ScopeSelected || !reflect.DeepEqual(normalized, included[:2]) {
		t.Fatalf("selected=%s %v %v", mode, normalized, err)
	}
	for _, selection := range [][]int64{{-1}, {9999}, {included[0], 9999}} {
		if _, _, err := service.ValidateKeyScope(ctx, userID, ScopeSelected, selection); !errors.Is(err, ErrScopeInvalid) {
			t.Fatalf("selection=%v err=%v", selection, err)
		}
	}
	var second int64
	if err := service.db.QueryRow(`INSERT INTO users(email,password_hash,created_at,updated_at) VALUES ('foreign@example.com','h',?,?) RETURNING id`, timestamp(service.now()), timestamp(service.now())).Scan(&second); err != nil {
		t.Fatal(err)
	}
	foreign := addResource(t, service, second, "hosted", "active", "foreign", nil)
	if _, _, err := service.ValidateKeyScope(ctx, userID, ScopeSelected, []int64{foreign}); !errors.Is(err, ErrScopeInvalid) {
		t.Fatalf("foreign err=%v", err)
	}
	pending := addResource(t, service, userID, "hosted", "awaiting_auth", "pending", nil)
	for _, selection := range [][]int64{nil, {pending}} {
		if _, _, err := service.ValidateKeyScope(ctx, userID, ScopeSelected, selection); !errors.Is(err, ErrScopeEmpty) {
			t.Fatalf("selection=%v err=%v", selection, err)
		}
	}
	if _, _, err := service.ValidateKeyScope(ctx, userID, "bad", nil); !errors.Is(err, ErrScopeInvalid) {
		t.Fatal(err)
	}
}
func TestScopeSyncKeysFlagsAndEmptySelection(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	first, vmID := scopeVM(t, service, fake, userID, "active")
	_, secondVM := scopeVM(t, service, fake, userID, "suspended")
	scopeVM(t, service, fake, userID, "awaiting_auth")
	all := scopePortalKey(t, service, userID, "active", ScopeAll, nil)
	selected := scopePortalKey(t, service, userID, "active", ScopeSelected, []int64{first})
	disabled := scopePortalKey(t, service, userID, "disabled", ScopeAll, nil)
	addSubscription(t, service, userID, "concurrency", 5, service.now().Add(time.Hour))
	if err := service.SyncUserScope(ctx, userID); err != nil {
		t.Fatal(err)
	}
	pool := scopePool(t, service, userID)
	if !pool.Enabled || !reflect.DeepEqual(pool.VMIDs, []string{vmID, secondVM}) {
		t.Fatalf("pool=%+v", pool)
	}
	for _, key := range []vm2api.Key{all, selected, disabled} {
		info, err := fake.GetKey(ctx, key.ID)
		if err != nil {
			t.Fatal(err)
		}
		if info.Active != (key.ID != disabled.ID) || info.MaxConcurrency != 6 {
			t.Fatalf("info=%+v", info)
		}
		if key.ID == selected.ID && (!reflect.DeepEqual(info.AllowedVMs, []string{vmID}) || info.PoolID != "") {
			t.Fatalf("selected=%+v", info)
		}
	}
	if _, err := service.db.Exec(`UPDATE users SET status='disabled' WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncUserScope(ctx, userID); err != nil {
		t.Fatal(err)
	}
	for _, key := range []vm2api.Key{all, selected, disabled} {
		info, err := fake.GetKey(ctx, key.ID)
		if err != nil || info.Active {
			t.Fatalf("disabled info=%+v err=%v", info, err)
		}
	}
	if _, err := service.db.Exec(`UPDATE users SET status='active' WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`UPDATE resources SET status='releasing' WHERE id=?`, first); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncUserScope(ctx, userID); err != nil {
		t.Fatal(err)
	}
	info, err := fake.GetKey(ctx, selected.ID)
	if err != nil || info.Active || info.PoolID != pool.ID || len(info.AllowedVMs) != 0 {
		t.Fatalf("empty selection=%+v err=%v", info, err)
	}
	info, err = fake.GetKey(ctx, all.ID)
	if err != nil || !info.Active {
		t.Fatalf("all=%+v err=%v", info, err)
	}
}

type releaseScopeClient struct {
	vm2api.Client
	events        []string
	failSync      bool
	beforeRelease func(string)
}

func (client *releaseScopeClient) SetPoolMembers(ctx context.Context, id string, ids []string) (vm2api.Pool, error) {
	client.events = append(client.events, "pool")
	if client.failSync {
		return vm2api.Pool{}, errors.New("scope sync unavailable")
	}
	return client.Client.SetPoolMembers(ctx, id, ids)
}
func (client *releaseScopeClient) UpdateKey(ctx context.Context, id string, in vm2api.UpdateKeyInput) error {
	client.events = append(client.events, "key")
	return client.Client.UpdateKey(ctx, id, in)
}
func (client *releaseScopeClient) DeleteVM(ctx context.Context, id string) error {
	client.events = append(client.events, "delete")
	if client.beforeRelease != nil {
		client.beforeRelease(id)
	}
	return client.Client.DeleteVM(ctx, id)
}
func (client *releaseScopeClient) SetVMOwner(ctx context.Context, id string, owner *string) error {
	if owner == nil {
		client.events = append(client.events, "return")
		if client.beforeRelease != nil {
			client.beforeRelease(id)
		}
	}
	return client.Client.SetVMOwner(ctx, id, owner)
}
func TestScopeReleaseOrdersRemovalBeforeVMRelease(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"hosted", "rental"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fail=%v", kind, fail), func(t *testing.T) {
				service, fake, userID := provisionFixture(t)
				id, vmID := scopeVM(t, service, fake, userID, "active")
				if _, err := service.db.Exec(`UPDATE resources SET kind=? WHERE id=?`, kind, id); err != nil {
					t.Fatal(err)
				}
				key := scopePortalKey(t, service, userID, "active", ScopeSelected, []int64{id})
				if err := service.SyncUserScope(ctx, userID); err != nil {
					t.Fatal(err)
				}
				client := &releaseScopeClient{Client: fake, failSync: fail}
				client.beforeRelease = func(released string) {
					pool := scopePool(t, service, userID)
					if len(pool.VMIDs) != 0 {
						t.Fatalf("VM still pooled at release: %+v", pool)
					}
					info, err := fake.GetKey(ctx, key.ID)
					if err != nil || info.Active || len(info.AllowedVMs) != 0 || info.PoolID != pool.ID {
						t.Fatalf("stale allowlist at release: %+v err=%v", info, err)
					}
					if released != vmID {
						t.Fatalf("released=%s", released)
					}
				}
				service.client = client
				if kind == "hosted" && !fail {
					if err := service.DeleteHosted(ctx, userID, id); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := service.MarkReleasing(ctx, id); err != nil {
						t.Fatal(err)
					}
				}
				err := service.ReleaseResource(ctx, id)
				if fail {
					if err == nil {
						t.Fatal("release ignored sync failure")
					}
					account, readErr := service.resource(ctx, id)
					if readErr != nil || account.Status != "releasing" || account.LastError == nil {
						t.Fatalf("account=%+v err=%v", account, readErr)
					}
					if _, err := fake.GetAccountUsage(ctx, vmID); err != nil {
						t.Fatal("VM destroyed on sync failure", err)
					}
					if !reflect.DeepEqual(client.events, []string{"pool"}) {
						t.Fatalf("events=%v", client.events)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					last := client.events[len(client.events)-1]
					if last != "delete" && last != "return" {
						t.Fatalf("events=%v", client.events)
					}
					if len(client.events) < 3 || client.events[len(client.events)-2] != "key" {
						t.Fatalf("events=%v", client.events)
					}
				}
			})
		}
	}
}
func TestScopeRentalReLeaseRemovesFirstUserMembership(t *testing.T) {
	service, fake, first := provisionFixture(t)
	ctx := context.Background()
	vm, err := fake.CreateHostedVM(ctx, "rental", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := timestamp(service.now())
	if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,selfcheck_state,created_at,updated_at) VALUES (?,'available','pass',?,?)`, vm.ID, now, now); err != nil {
		t.Fatal(err)
	}
	firstSub := addSubscription(t, service, first, "rental_account", 1, service.now().Add(time.Hour))
	if err := service.AllocateRental(ctx, firstSub); err != nil {
		t.Fatal(err)
	}
	var resourceID int64
	if err := service.db.QueryRow(`SELECT id FROM resources WHERE user_id=?`, first).Scan(&resourceID); err != nil {
		t.Fatal(err)
	}
	selected := scopePortalKey(t, service, first, "active", ScopeSelected, []int64{resourceID})
	if err := service.SyncUserScope(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := service.MarkReleasing(ctx, resourceID); err != nil {
		t.Fatal(err)
	}
	if err := service.ReleaseResource(ctx, resourceID); err != nil {
		t.Fatal(err)
	}
	var second int64
	if err := service.db.QueryRow(`INSERT INTO users(email,password_hash,created_at,updated_at) VALUES ('second-scope@example.com','h',?,?) RETURNING id`, now, now).Scan(&second); err != nil {
		t.Fatal(err)
	}
	sub := addSubscription(t, service, second, "rental_account", 1, service.now().Add(time.Hour))
	if err := service.AllocateRental(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if pool := scopePool(t, service, first); len(pool.VMIDs) != 0 {
		t.Fatalf("first pool=%+v", pool)
	}
	if pool := scopePool(t, service, second); !reflect.DeepEqual(pool.VMIDs, []string{vm.ID}) {
		t.Fatalf("second pool=%+v", pool)
	}
	info, err := fake.GetKey(ctx, selected.ID)
	if err != nil || info.Active || len(info.AllowedVMs) != 0 {
		t.Fatalf("old key=%+v err=%v", info, err)
	}
}
func TestScopeReconcileOrphanAndOwnerDrift(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	_, vmID := scopeVM(t, service, fake, userID, "active")
	known := scopePortalKey(t, service, userID, "active", ScopeAll, nil)
	owner, err := service.ServiceUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope, _, err := service.KeyScopeFor(ctx, userID, ScopeAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{Name: "orphan", UserID: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := fake.CreateUser(ctx, "foreign", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetVMOwner(ctx, vmID, &foreign.ID); err != nil {
		t.Fatal(err)
	}
	client := &listCountingClient{Client: fake}
	service.client = client
	if err := service.ReconcileScopes(ctx); err != nil {
		t.Fatal(err)
	}
	if client.lists != 1 {
		t.Fatalf("ListVMs calls=%d", client.lists)
	}
	info, err := fake.GetKey(ctx, orphan.ID)
	if err != nil || info.Active {
		t.Fatalf("orphan=%+v err=%v", info, err)
	}
	info, err = fake.GetKey(ctx, known.ID)
	if err != nil || !info.Active {
		t.Fatalf("known=%+v err=%v", info, err)
	}
	usage, err := fake.GetAccountUsage(ctx, vmID)
	if err != nil || usage.VM.OwnerUserID != owner {
		t.Fatalf("vm=%+v err=%v", usage.VM, err)
	}
}

type listCountingClient struct {
	vm2api.Client
	lists int
}

func (client *listCountingClient) ListVMs(ctx context.Context, f vm2api.VMFilter) ([]vm2api.VM, error) {
	client.lists++
	return client.Client.ListVMs(ctx, f)
}
func TestScopeMigrationRekeysAndRetriesOldKeyCleanup(t *testing.T) {
	for _, failDelete := range []bool{false, true} {
		t.Run(fmt.Sprint(failDelete), func(t *testing.T) {
			service, fake, userID := provisionFixture(t)
			ctx := context.Background()
			var legacy string
			if err := service.db.QueryRow(`SELECT vm2api_user_id FROM users WHERE id=?`, userID).Scan(&legacy); err != nil {
				t.Fatal(err)
			}
			box, err := secret.New("scope-migration-test-key-32-bytes-minimum")
			if err != nil {
				t.Fatal(err)
			}
			service.Box = box
			vm, err := fake.CreateHostedVM(ctx, "legacy", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := fake.SetVMOwner(ctx, vm.ID, &legacy); err != nil {
				t.Fatal(err)
			}
			account := addResource(t, service, userID, "hosted", "active", vm.ID, nil)
			old, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{Name: "legacy key", UserID: legacy, MaxConcurrency: 7, Scope: vm2api.KeyScope{AllowedVMs: []string{vm.ID}}})
			if err != nil {
				t.Fatal(err)
			}
			oldSealed, err := box.Seal(old.Secret)
			if err != nil {
				t.Fatal(err)
			}
			now := timestamp(service.now())
			if _, err := service.db.Exec(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,scope_mode,scope_accounts,created_at,updated_at) VALUES (?,'legacy key','stable-portal-hash','sk-pt-',?,?,'active','selected',?,?,?)`, userID, old.ID, oldSealed, store.EncodeIDs([]int64{account}), now, now); err != nil {
				t.Fatal(err)
			}
			if _, err := service.db.Exec(`UPDATE users SET vm2api_username='legacy-audit' WHERE id=?`, userID); err != nil {
				t.Fatal(err)
			}
			addSubscription(t, service, userID, "concurrency", 7, service.now().Add(time.Hour))
			client := &migrationScopeClient{Client: fake, failDelete: failDelete, oldID: old.ID}
			service.client = client
			if err := service.MigrateLegacyScopes(ctx); (err != nil) != failDelete {
				t.Fatalf("first migration err=%v", err)
			}
			client.failDelete = false
			if err := service.MigrateLegacyScopes(ctx); err != nil {
				t.Fatal(err)
			}
			var id, sealed, owner, previous, hash string
			if err := service.db.QueryRow(`SELECT vm2api_key_id,vm2api_secret_enc,upstream_owner,previous_vm2api_key_ids,key_hash FROM api_keys`).Scan(&id, &sealed, &owner, &previous, &hash); err != nil {
				t.Fatal(err)
			}
			if id == old.ID || owner == legacy || owner == "" || hash != "stable-portal-hash" || !reflect.DeepEqual(store.DecodeStrings(previous), []string{old.ID}) {
				t.Fatalf("id=%s owner=%s previous=%s hash=%s", id, owner, previous, hash)
			}
			plain, err := box.Open(sealed)
			if err != nil || plain == old.Secret || plain == "" || sealed == oldSealed {
				t.Fatalf("secret was not replaced/resealed: %v", err)
			}
			info, err := fake.GetKey(ctx, id)
			if err != nil || info.UserID != owner || info.Name != "legacy key" || !info.Active || info.MaxConcurrency != 8 || !reflect.DeepEqual(info.AllowedVMs, []string{vm.ID}) {
				t.Fatalf("new key=%+v err=%v", info, err)
			}
			if _, err := fake.GetKey(ctx, old.ID); !vm2api.IsStatus(err, 404) {
				t.Fatalf("old key remains: %v", err)
			}
			usage, err := fake.GetAccountUsage(ctx, vm.ID)
			if err != nil || usage.VM.OwnerUserID != owner {
				t.Fatalf("vm=%+v err=%v", usage.VM, err)
			}
			var legacyID, username string
			if err := service.db.QueryRow(`SELECT COALESCE(vm2api_user_id,''),vm2api_username FROM users WHERE id=?`, userID).Scan(&legacyID, &username); err != nil {
				t.Fatal(err)
			}
			if legacyID != "" || username != "legacy-audit" {
				t.Fatalf("legacy=%s username=%s", legacyID, username)
			}
			if !client.disabled[legacy] {
				t.Fatal("legacy user not disabled")
			}
			if client.creates != 1 {
				t.Fatalf("creates=%d", client.creates)
			}
			if err := service.MigrateLegacyScopes(ctx); err != nil {
				t.Fatal(err)
			}
			var again string
			if err := service.db.QueryRow(`SELECT vm2api_key_id FROM api_keys`).Scan(&again); err != nil || again != id || client.creates != 1 {
				t.Fatalf("repeat=%s creates=%d err=%v", again, client.creates, err)
			}
		})
	}
}

type migrationScopeClient struct {
	vm2api.Client
	creates    int
	failDelete bool
	oldID      string
	disabled   map[string]bool
}

func (client *migrationScopeClient) CreateKey(ctx context.Context, in vm2api.CreateKeyInput) (vm2api.Key, error) {
	client.creates++
	return client.Client.CreateKey(ctx, in)
}
func (client *migrationScopeClient) DeleteKey(ctx context.Context, id string) error {
	if client.failDelete && id == client.oldID {
		return errors.New("delete unavailable")
	}
	return client.Client.DeleteKey(ctx, id)
}
func (client *migrationScopeClient) SetUserEnabled(ctx context.Context, id string, enabled bool) error {
	if client.disabled == nil {
		client.disabled = map[string]bool{}
	}
	client.disabled[id] = !enabled
	return client.Client.SetUserEnabled(ctx, id, enabled)
}
func TestScopeMigrationRequiresSecretBox(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	var owner string
	if err := service.db.QueryRow(`SELECT vm2api_user_id FROM users WHERE id=?`, userID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	vm, err := fake.CreateHostedVM(ctx, "old", nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{Name: "old", UserID: owner, Scope: vm2api.KeyScope{AllowedVMs: []string{vm.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	now := timestamp(service.now())
	if _, err := service.db.Exec(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,created_at,updated_at) VALUES (?,'old','h','prefix',?,'enc','active',?,?)`, userID, key.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if err := service.MigrateLegacyScopes(ctx); err == nil || !strings.Contains(err.Error(), "secret box") {
		t.Fatalf("err=%v", err)
	}
}

func TestScopeSyncRejectsForeignPoolAndSkipsLegacyKeys(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	id, vmID := scopeVM(t, service, fake, userID, "active")
	key := scopePortalKey(t, service, userID, "active", ScopeSelected, []int64{id})
	if _, err := service.db.Exec(`UPDATE api_keys SET upstream_owner='' WHERE vm2api_key_id=?`, key.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncUserScope(ctx, userID); err != nil {
		t.Fatal(err)
	}
	info, err := fake.GetKey(ctx, key.ID)
	if err != nil || info.MaxConcurrency != 0 {
		t.Fatalf("legacy key was modified: %+v err=%v", info, err)
	}
	pool := scopePool(t, service, userID)
	if _, err := fake.SetPoolMembers(ctx, pool.ID, nil); err != nil {
		t.Fatal(err)
	}
	foreign, err := fake.CreatePool(ctx, "foreign-admin-pool", []string{vmID})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SyncUserScope(ctx, userID); !vm2api.IsCode(err, "vm_in_other_pool") || !strings.Contains(err.Error(), vmID) || !strings.Contains(err.Error(), foreign.ID) {
		t.Fatalf("foreign conflict=%v", err)
	}
}

func TestScopeAddFailureQueuesRetryWithoutFailingFulfilment(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	vm, err := fake.CreateHostedVM(ctx, "rental", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := timestamp(service.now())
	if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,selfcheck_state,created_at,updated_at) VALUES (?,'available','pass',?,?)`, vm.ID, now, now); err != nil {
		t.Fatal(err)
	}
	client := &releaseScopeClient{Client: fake, failSync: true}
	service.client = client
	order := addOrder(t, service, userID, "rental_account", 1, nil)
	if err := service.FulfilOrder(ctx, order); err != nil {
		t.Fatalf("scope failure failed fulfilment: %v", err)
	}
	var status string
	if err := service.db.QueryRow(`SELECT status FROM orders WHERE id=?`, order).Scan(&status); err != nil || status != "fulfilled" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	var count int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='sync_scope' AND status='pending' AND dedupe_key=?`, fmt.Sprintf("sync_scope:%d", userID)).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry jobs=%d err=%v", count, err)
	}
	if err := service.syncScopeAfterAdd(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='sync_scope' AND status='pending'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dedupe jobs=%d err=%v", count, err)
	}
	client.failSync = false
	if err := service.scopeSyncJob(ctx, Job{Payload: json.RawMessage(fmt.Sprintf(`{"user_id":%d}`, userID))}); err != nil {
		t.Fatal(err)
	}
	if pool := scopePool(t, service, userID); !reflect.DeepEqual(pool.VMIDs, []string{vm.ID}) {
		t.Fatalf("retry pool=%+v", pool)
	}
}

func TestScopeRollbackSyncFailureDoesNotReturnRental(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	for range 2 {
		vm, err := fake.CreateHostedVM(ctx, "rental", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,selfcheck_state,created_at,updated_at) VALUES (?,'available','pass',?,?)`, vm.ID, timestamp(service.now()), timestamp(service.now())); err != nil {
			t.Fatal(err)
		}
	}
	client := &releaseScopeClient{Client: &failingProvisionClient{Client: fake}, failSync: true}
	service.client = client
	order := addOrder(t, service, userID, "rental_account", 2, nil)
	if err := service.FulfilOrder(ctx, order); err == nil {
		t.Fatal("expected assignment failure")
	}
	for _, event := range client.events {
		if event == "return" {
			t.Fatalf("stock returned before successful sync: %v", client.events)
		}
	}
	var releasing, maintenance, retry int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM resources WHERE status='releasing' AND last_error IS NOT NULL`).Scan(&releasing); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM rental_pool WHERE status='maintenance'`).Scan(&maintenance); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='release_resource' AND status='pending'`).Scan(&retry); err != nil {
		t.Fatal(err)
	}
	if releasing != 2 || maintenance != 2 || retry != 2 {
		t.Fatalf("releasing=%d maintenance=%d retry=%d", releasing, maintenance, retry)
	}
	client.failSync = false
	rows, err := service.db.Query(`SELECT id FROM resources WHERE status='releasing'`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := service.ReleaseResource(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM rental_pool WHERE status='available'`).Scan(&maintenance); err != nil || maintenance != 2 {
		t.Fatalf("available=%d err=%v", maintenance, err)
	}
}

type recreatedPoolClient struct {
	vm2api.Client
	vanished bool
}

func (client *recreatedPoolClient) SetPoolMembers(ctx context.Context, id string, ids []string) (vm2api.Pool, error) {
	if !client.vanished {
		client.vanished = true
		if err := client.Client.DeletePool(ctx, id); err != nil {
			return vm2api.Pool{}, err
		}
		return vm2api.Pool{}, &vm2api.Error{Status: 404}
	}
	return client.Client.SetPoolMembers(ctx, id, ids)
}
func TestScopeSyncRecreatesPoolDeletedDuringSync(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	_, vmID := scopeVM(t, service, fake, userID, "active")
	service.client = &recreatedPoolClient{Client: fake}
	if err := service.SyncUserScope(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
	if pool := scopePool(t, service, userID); !reflect.DeepEqual(pool.VMIDs, []string{vmID}) {
		t.Fatalf("pool=%+v", pool)
	}
}

type blockingScopeKeysClient struct {
	vm2api.Client
	listed chan struct{}
}

func (client *blockingScopeKeysClient) ListKeys(ctx context.Context, userID string) ([]vm2api.KeyInfo, error) {
	close(client.listed)
	return client.Client.ListKeys(ctx, userID)
}
func TestScopeReconcileWaitsForInflightKeyWrite(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	if err := service.SyncUserScope(ctx, userID); err != nil {
		t.Fatal(err)
	}
	owner, err := service.ServiceUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope, _, err := service.KeyScopeFor(ctx, userID, ScopeAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	unlock := service.LockScope(userID)
	key, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{Name: "inflight", UserID: owner, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	client := &blockingScopeKeysClient{Client: fake, listed: make(chan struct{})}
	service.client = client
	done := make(chan error, 1)
	go func() { done <- service.ReconcileScopes(ctx) }()
	select {
	case <-client.listed:
		t.Fatal("orphan pass raced uncommitted key")
	case <-time.After(20 * time.Millisecond):
	}
	now := timestamp(service.now())
	if _, err := service.db.Exec(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,upstream_owner,created_at,updated_at) VALUES (?,'inflight','inflight','prefix',?,'enc','active',?,?,?)`, userID, key.ID, owner, now, now); err != nil {
		t.Fatal(err)
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconcile deadlocked")
	}
	info, err := fake.GetKey(ctx, key.ID)
	if err != nil || !info.Active {
		t.Fatalf("inflight key=%+v err=%v", info, err)
	}
}

func TestScopeSyncReenablesDisabledPoolAndDeleteMissingPool(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	scopeVM(t, service, fake, userID, "active")
	if err := service.SyncUserScope(ctx, userID); err != nil {
		t.Fatal(err)
	}
	pool := scopePool(t, service, userID)
	request := httptest.NewRequest(http.MethodPatch, "/api/panel/vm-pools/"+pool.ID, strings.NewReader(`{"enabled":false}`))
	request.Header.Set("Authorization", "Bearer master")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	fake.Handler("master").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("disable pool: %s", response.Body.String())
	}
	if pool := scopePool(t, service, userID); pool.Enabled {
		t.Fatal("pool was not disabled")
	}
	if err := service.SyncUserScope(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if pool := scopePool(t, service, userID); !pool.Enabled {
		t.Fatal("pool stayed disabled")
	}
	if err := fake.DeletePool(ctx, pool.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteUserScope(ctx, userID); err != nil {
		t.Fatalf("missing pool: %v", err)
	}
}
func TestScopeDeleteHostedSyncFailureStaysReleasing(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	id, vmID := scopeVM(t, service, fake, userID, "active")
	ctx := context.Background()
	if err := service.SyncUserScope(ctx, userID); err != nil {
		t.Fatal(err)
	}
	service.client = &releaseScopeClient{Client: fake, failSync: true}
	if err := service.DeleteHosted(ctx, userID, id); err == nil {
		t.Fatal("delete hid scope sync failure")
	}
	account, err := service.resource(ctx, id)
	if err != nil || account.Status != "releasing" || account.LastError == nil {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	usage, err := fake.GetAccountUsage(ctx, vmID)
	if err != nil || usage.VM.ScheduleState != "off" {
		t.Fatalf("vm was deleted or still schedulable: %+v err=%v", usage.VM, err)
	}
	var jobs int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type='release_resource' AND status='pending'`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("jobs=%d err=%v", jobs, err)
	}
}
func TestScopeRunSchedulesMigrationAndReconcile(t *testing.T) {
	service, _, _ := provisionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.engine.Workers = 1
	service.engine.PollInterval = time.Millisecond
	migrated, reconciled := make(chan struct{}), make(chan struct{})
	service.engine.Register("scope_migrate", func(context.Context, Job) error { close(migrated); return nil })
	service.engine.Register("scope_reconcile", func(context.Context, Job) error { close(reconciled); return nil })
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	for _, signal := range []chan struct{}{migrated, reconciled} {
		select {
		case <-signal:
		case <-time.After(3 * time.Second):
			t.Fatal("startup scope job did not run")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
	var jobs int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE type IN ('scope_migrate','scope_reconcile')`).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("jobs=%d err=%v", jobs, err)
	}
}

func TestScopeMigrationDisablesUnusableAndInactiveKeys(t *testing.T) {
	for _, mode := range []string{"disabled-user", "disabled-key", "empty-selected"} {
		t.Run(mode, func(t *testing.T) {
			service, fake, userID := provisionFixture(t)
			ctx := context.Background()
			box, err := secret.New("scope-migration-status-test-key-32-bytes")
			if err != nil {
				t.Fatal(err)
			}
			service.Box = box
			var legacy string
			if err := service.db.QueryRow(`SELECT vm2api_user_id FROM users WHERE id=?`, userID).Scan(&legacy); err != nil {
				t.Fatal(err)
			}
			vm, err := fake.CreateHostedVM(ctx, "legacy", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := fake.SetVMOwner(ctx, vm.ID, &legacy); err != nil {
				t.Fatal(err)
			}
			account := addResource(t, service, userID, "hosted", "active", vm.ID, nil)
			old, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{Name: mode, UserID: legacy, Scope: vm2api.KeyScope{AllowedVMs: []string{vm.ID}}})
			if err != nil {
				t.Fatal(err)
			}
			now := timestamp(service.now())
			if _, err := service.db.Exec(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,scope_mode,scope_accounts,created_at,updated_at) VALUES (?,?,?,'prefix',?,'enc','active','selected',?,?,?)`, userID, mode, old.ID, old.ID, store.EncodeIDs([]int64{account}), now, now); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "disabled-user":
				_, err = service.db.Exec(`UPDATE users SET status='disabled' WHERE id=?`, userID)
			case "disabled-key":
				_, err = service.db.Exec(`UPDATE api_keys SET status='disabled' WHERE vm2api_key_id=?`, old.ID)
			case "empty-selected":
				_, err = service.db.Exec(`UPDATE resources SET status='failed' WHERE id=?`, account)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := service.MigrateLegacyScopes(ctx); err != nil {
				t.Fatal(err)
			}
			var id string
			if err := service.db.QueryRow(`SELECT vm2api_key_id FROM api_keys WHERE user_id=?`, userID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			info, err := fake.GetKey(ctx, id)
			if err != nil || info.Active {
				t.Fatalf("key=%+v err=%v", info, err)
			}
			if mode == "empty-selected" && (len(info.AllowedVMs) != 0 || info.PoolID == "") {
				t.Fatalf("stale migrated scope=%+v", info)
			}
		})
	}
}

// A user whose migration cannot finish (here: no secret box to re-seal the
// legacy key) keeps their VMs on the legacy vm2api user, so reconcile must not
// "repair" those owners and cut off the not-yet-migrated key.
func TestScopeReconcileLeavesUnmigratedUserOwners(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	var legacy string
	if err := service.db.QueryRow(`SELECT vm2api_user_id FROM users WHERE id=?`, userID).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	vm, err := fake.CreateHostedVM(ctx, "legacy", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.SetVMOwner(ctx, vm.ID, &legacy); err != nil {
		t.Fatal(err)
	}
	addResource(t, service, userID, "hosted", "active", vm.ID, nil)
	old, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{Name: "legacy key", UserID: legacy, MaxConcurrency: 1, Scope: vm2api.KeyScope{AllowedVMs: []string{vm.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	now := timestamp(service.now())
	if _, err := service.db.Exec(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,created_at,updated_at) VALUES (?,'legacy key','legacy-hash','sk-pt-',?,'sealed','active',?,?)`, userID, old.ID, now, now); err != nil {
		t.Fatal(err)
	}
	service.Box = nil
	if err := service.ReconcileScopes(ctx); err == nil || !strings.Contains(err.Error(), "secret box") {
		t.Fatalf("reconcile err=%v, want the migration failure reported", err)
	}
	usage, err := fake.GetAccountUsage(ctx, vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.VM.OwnerUserID != legacy {
		t.Fatalf("owner=%q, want legacy %q kept until migration succeeds", usage.VM.OwnerUserID, legacy)
	}
	info, err := fake.GetKey(ctx, old.ID)
	if err != nil || !info.Active {
		t.Fatalf("legacy key=%+v err=%v", info, err)
	}
}

// The expiry sweep runs for every user every minute: an idle user must not
// get a pool, and the sweep must not touch pools at all.
func TestScopeIdleUserGetsNoPool(t *testing.T) {
	service, _, userID := provisionFixture(t)
	ctx := context.Background()
	if err := service.SyncUserScope(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := service.SweepExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	var pool sql.NullString
	if err := service.db.QueryRow(`SELECT vm2api_pool_id FROM users WHERE id=?`, userID).Scan(&pool); err != nil {
		t.Fatal(err)
	}
	if pool.Valid && pool.String != "" {
		t.Fatalf("idle user got pool %q", pool.String)
	}
}

// The first key of a user who already holds accounts must reach them at once:
// the pool is created with the current members, not empty.
func TestScopeNewPoolStartsWithCurrentAccounts(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	_, vmID := scopeVM(t, service, fake, userID, "active")
	if _, err := service.db.Exec(`UPDATE users SET vm2api_pool_id=NULL WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	scope, usable, err := service.KeyScopeFor(ctx, userID, ScopeAll, nil)
	if err != nil || !usable {
		t.Fatalf("scope=%+v usable=%v err=%v", scope, usable, err)
	}
	pools, err := fake.ListPools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range pools {
		if pool.ID == scope.PoolID {
			if !reflect.DeepEqual(pool.VMIDs, []string{vmID}) {
				t.Fatalf("new pool members=%v want [%s]", pool.VMIDs, vmID)
			}
			return
		}
	}
	t.Fatalf("pool %s not found", scope.PoolID)
}
