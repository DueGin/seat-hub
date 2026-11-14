package portal

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DueGin/seat-hub/internal/api"
	"github.com/DueGin/seat-hub/internal/provision"
	"github.com/DueGin/seat-hub/internal/secret"
	"github.com/DueGin/seat-hub/internal/store"
	"github.com/DueGin/seat-hub/internal/vm2api"
)

type keyClient struct {
	*vm2api.Fake
	mu          sync.Mutex
	users       int
	password    string
	creates     []vm2api.CreateKeyInput
	keys        []vm2api.Key
	updates     []vm2api.UpdateKeyInput
	deletes     []string
	createErr   error
	updateErr   error
	deleteErr   error
	afterCreate func()
	afterUser   func()
}

func (client *keyClient) CreateUser(ctx context.Context, username, password string) (vm2api.User, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.users++
	client.password = password
	user, err := client.Fake.CreateUser(ctx, username, password)
	if err == nil && client.afterUser != nil {
		client.afterUser()
	}
	return user, err
}

func (client *keyClient) CreateKey(ctx context.Context, input vm2api.CreateKeyInput) (vm2api.Key, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.creates = append(client.creates, input)
	if client.createErr != nil {
		return vm2api.Key{}, client.createErr
	}
	key, err := client.Fake.CreateKey(ctx, input)
	if err == nil {
		client.keys = append(client.keys, key)
		if client.afterCreate != nil {
			client.afterCreate()
		}
	}
	return key, err
}

func (client *keyClient) UpdateKey(ctx context.Context, id string, input vm2api.UpdateKeyInput) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.updates = append(client.updates, input)
	if client.updateErr != nil {
		return client.updateErr
	}
	return client.Fake.UpdateKey(ctx, id, input)
}

func (client *keyClient) DeleteKey(ctx context.Context, id string) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.deletes = append(client.deletes, id)
	if client.deleteErr != nil {
		return client.deleteErr
	}
	return client.Fake.DeleteKey(ctx, id)
}

func keyFixture(t *testing.T) (*Service, *keyClient) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, err := secret.New(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	client := &keyClient{Fake: vm2api.NewFake()}
	service := New(db, client, box)
	service.Now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	for _, email := range []string{"first@example.com", "second@example.com"} {
		if _, err := db.Exec(`INSERT INTO users(email,password_hash,created_at,updated_at) VALUES (?,'hash',?,?)`, email, store.FmtTime(service.now()), store.FmtTime(service.now())); err != nil {
			t.Fatal(err)
		}
	}
	return service, client
}

func keySQL(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func keyError(t *testing.T, err error, status int, code string) {
	t.Helper()
	mapped := api.AsUserError(err)
	if mapped == nil || mapped.Status != status || mapped.Code != code {
		t.Fatalf("error = %v, mapped = %+v; want %d %s", err, mapped, status, code)
	}
}

func TestKeyCreateEncryptionAndEntitlements(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	keySQL(t, service.db, `INSERT INTO settings(key,value,updated_at) VALUES ('site',?,?)`, `{"base_concurrency":2,"concurrency_per_hosted_seat":3,"concurrency_per_rental_account":4}`, store.FmtTime(service.now()))
	for _, item := range []struct {
		kind, status string
		quantity     int
		start, end   time.Time
	}{
		{"concurrency", "active", 5, service.now().Add(-time.Hour), service.now().Add(time.Hour)},
		{"concurrency", "active", 90, service.now().Add(time.Hour), service.now().Add(2 * time.Hour)},
		{"concurrency", "active", 80, service.now().Add(-time.Hour), service.now()},
		{"concurrency", "cancelled", 70, service.now().Add(-time.Hour), service.now().Add(time.Hour)},
	} {
		keySQL(t, service.db, `INSERT INTO subscriptions(user_id,kind,quantity,status,starts_at,expires_at,created_at,updated_at) VALUES (1,?,?,?,?,?,?,?)`, item.kind, item.quantity, item.status, store.FmtTime(item.start), store.FmtTime(item.end), store.FmtTime(service.now()), store.FmtTime(service.now()))
	}
	for _, item := range []struct{ kind, status string }{{"hosted", "active"}, {"rental", "active"}, {"hosted", "suspended"}} {
		keySQL(t, service.db, `INSERT INTO resources(user_id,kind,status,created_at,updated_at) VALUES (1,?,?,?,?)`, item.kind, item.status, store.FmtTime(service.now()), store.FmtTime(service.now()))
	}
	item, plaintext, err := service.Create(ctx, 1, keyInput("我的密钥"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plaintext) != 49 || !strings.HasPrefix(plaintext, "sk-pt-") || item.Prefix != plaintext[:10] || item.Status != "active" {
		t.Fatalf("unexpected key: %+v, secret length %d", item, len(plaintext))
	}
	if len(client.creates) != 1 || client.creates[0].MaxConcurrency != 14 || client.creates[0].Name != item.Name {
		t.Fatalf("upstream create = %+v", client.creates)
	}
	var keyHash, keyEnc string
	var synced int
	identity, err := store.LoadServiceIdentity(ctx, service.db.DB)
	if err != nil || identity.UserID != client.creates[0].UserID || identity.Username == "" {
		t.Fatalf("service identity = %+v, err %v", identity, err)
	}
	var legacyUser string
	if err := service.db.QueryRow(`SELECT COALESCE(vm2api_user_id,'') FROM users WHERE id=1`).Scan(&legacyUser); err != nil || legacyUser != "" {
		t.Fatalf("portal user acquired upstream identity: %q, %v", legacyUser, err)
	}
	if item.Scope.Mode != "all" || item.Scope.Accounts == nil || !item.Scope.Usable || client.creates[0].Scope.PoolID == "" {
		t.Fatalf("unrestricted key: %+v, %+v", item, client.creates[0])
	}
	if err := service.db.QueryRow(`SELECT key_hash,vm2api_secret_enc,synced_concurrency FROM api_keys WHERE id=?`, item.ID).Scan(&keyHash, &keyEnc, &synced); err != nil {
		t.Fatal(err)
	}
	upstreamSecret, err := service.box.Open(keyEnc)
	if err != nil || upstreamSecret != client.keys[0].Secret || keyHash != service.box.MAC(plaintext) || keyHash == plaintext || synced != 14 {
		t.Fatalf("invalid stored key: %v, concurrency %d", err, synced)
	}
	second, secondPlaintext, err := service.Create(ctx, 1, keyInput(""))
	if err != nil || second.Name != "默认" || secondPlaintext == plaintext || client.users != 1 {
		t.Fatalf("second key = %+v, users %d, err %v", second, client.users, err)
	}
	items, err := service.List(ctx, 1)
	if err != nil || len(items) != 2 || items[0].ID != second.ID || items[0].LastUsedAt != nil {
		t.Fatalf("list = %+v, err %v", items, err)
	}
	items, err = service.List(ctx, 2)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("other user's keys = %+v, err %v", items, err)
	}
}

func TestKeyUpdateDeleteAndOwnership(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	item, _, err := service.Create(ctx, 1, keyInput("original"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Update(ctx, 2, item.ID, keyPatch("stolen", "disabled"))
	keyError(t, err, 404, "key_not_found")
	keyError(t, service.Delete(ctx, 2, item.ID), 404, "key_not_found")
	if len(client.updates) != 0 || len(client.deletes) != 0 {
		t.Fatal("ownership failure touched upstream key")
	}
	updated, err := service.Update(ctx, 1, item.ID, keyPatch("renamed", "disabled"))
	if err != nil || updated.Name != "renamed" || updated.Status != "disabled" {
		t.Fatalf("update = %+v, err %v", updated, err)
	}
	keySQL(t, service.db, `INSERT INTO settings(key,value,updated_at) VALUES ('site','{"base_concurrency":9}',?)`, store.FmtTime(service.now()))
	updated, err = service.Update(ctx, 1, item.ID, keyPatch("", "active"))
	if err != nil || updated.Name != "renamed" || updated.Status != "active" {
		t.Fatalf("enable = %+v, err %v", updated, err)
	}
	last := client.updates[len(client.updates)-1]
	if last.Active == nil || !*last.Active || last.MaxConcurrency == nil || *last.MaxConcurrency != 9 {
		t.Fatalf("enable did not refresh entitlements: %+v", last)
	}
	client.deleteErr = vm2api.ErrUnavailable
	if err := service.Delete(ctx, 1, item.ID); !errors.Is(err, vm2api.ErrUnavailable) {
		t.Fatalf("delete outage = %v", err)
	}
	items, err := service.List(ctx, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("failed delete changed local keys: %+v, %v", items, err)
	}
	client.deleteErr = nil
	if err := client.Fake.DeleteKey(ctx, client.keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, 1, item.ID); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := service.db.QueryRow(`SELECT status FROM api_keys WHERE id=?`, item.ID).Scan(&status); err != nil || status != "deleted" {
		t.Fatalf("soft delete status = %q, err %v", status, err)
	}
	items, err = service.List(ctx, 1)
	if err != nil || len(items) != 0 {
		t.Fatalf("deleted key listed: %+v, %v", items, err)
	}
	_, err = service.Update(ctx, 1, item.ID, keyPatch("", "active"))
	keyError(t, err, 404, "key_not_found")
}

func TestKeyActiveLimitAndReenable(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	var first api.KeyDTO
	for index := range 20 {
		item, _, err := service.Create(ctx, 1, keyInput(fmt.Sprintf("key-%d", index)))
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = item
		}
	}
	_, _, err := service.Create(ctx, 1, keyInput("excess"))
	keyError(t, err, 422, "key_limit")
	if len(client.creates) != 20 {
		t.Fatalf("limit reached upstream: %d", len(client.creates))
	}
	if _, err := service.Update(ctx, 1, first.ID, keyPatch("", "disabled")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Create(ctx, 1, keyInput("replacement")); err != nil {
		t.Fatal(err)
	}
	_, err = service.Update(ctx, 1, first.ID, keyPatch("", "active"))
	keyError(t, err, 422, "key_limit")
}

func TestKeyValidationAndUpstreamFailures(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	_, _, err := service.Create(ctx, 999, keyInput("missing"))
	keyError(t, err, 404, "user_not_found")
	_, _, err = service.Create(ctx, 1, keyInput(strings.Repeat("名", 41)))
	keyError(t, err, 422, "invalid_name")
	_, err = service.Update(ctx, 1, 1, keyPatch("", "deleted"))
	keyError(t, err, 422, "invalid_status")
	client.createErr = vm2api.ErrUnavailable
	_, _, err = service.Create(ctx, 1, keyInput("outage"))
	if !errors.Is(err, vm2api.ErrUnavailable) {
		t.Fatalf("create outage = %v", err)
	}
	client.createErr = nil
	item, _, err := service.Create(ctx, 1, keyInput("original"))
	if err != nil {
		t.Fatal(err)
	}
	client.updateErr = vm2api.ErrUnavailable
	_, err = service.Update(ctx, 1, item.ID, keyPatch("changed", "disabled"))
	if !errors.Is(err, vm2api.ErrUnavailable) {
		t.Fatalf("update outage = %v", err)
	}
	items, err := service.List(ctx, 1)
	if err != nil || len(items) != 1 || items[0].Name != "original" || items[0].Status != "active" {
		t.Fatalf("failed update changed local key: %+v, %v", items, err)
	}
}

func TestKeyCreateCompensatesFailedInsert(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelRequest), func(t *testing.T) {
			service, client := keyFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelRequest {
				client.afterCreate = cancel
			} else {
				keySQL(t, service.db, `CREATE TRIGGER reject_keys BEFORE INSERT ON api_keys BEGIN SELECT RAISE(ABORT,'insert rejected'); END`)
			}
			_, _, err := service.Create(ctx, 1, keyInput("test"))
			if err == nil || len(client.keys) != 1 || len(client.deletes) != 1 {
				t.Fatalf("create err = %v, keys=%d deletes=%d", err, len(client.keys), len(client.deletes))
			}
			if _, err := client.Fake.KeyStats(context.Background(), client.keys[0].ID); !vm2api.IsStatus(err, 404) {
				t.Fatalf("orphan upstream key: %v", err)
			}
		})
	}
}

func TestKeyPersistsUpstreamUserWhenRequestCancelled(t *testing.T) {
	service, client := keyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.afterUser = cancel
	_, _, err := service.Create(ctx, 1, keyInput("cancelled"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("create error = %v", err)
	}
	identity, err := store.LoadServiceIdentity(context.Background(), service.db.DB)
	if err != nil || identity.UserID == "" {
		t.Fatalf("service identity not saved: %+v, %v", identity, err)
	}
	if _, _, err := service.Create(context.Background(), 1, keyInput("retry")); err != nil {
		t.Fatal(err)
	}
	if client.users != 1 {
		t.Fatalf("upstream user created %d times", client.users)
	}
}

func TestKeyConcurrentCreateLimit(t *testing.T) {
	service, _ := keyFixture(t)
	ctx := context.Background()
	var workers sync.WaitGroup
	results := make(chan error, 24)
	for range 24 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, _, err := service.Create(ctx, 1, keyInput("parallel"))
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	success, limited := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrKeyLimit):
			limited++
		default:
			t.Fatalf("parallel create: %v", err)
		}
	}
	if success != 20 || limited != 4 {
		t.Fatalf("success %d limited %d", success, limited)
	}
}

func TestKeyUpdateCompensatesFailedSave(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	item, _, err := service.Create(ctx, 1, keyInput("original"))
	if err != nil {
		t.Fatal(err)
	}
	keySQL(t, service.db, `CREATE TRIGGER reject_key_update BEFORE UPDATE ON api_keys BEGIN SELECT RAISE(ABORT,'update rejected'); END`)
	_, err = service.Update(ctx, 1, item.ID, keyPatch("renamed", "disabled"))
	if err == nil || len(client.updates) != 2 {
		t.Fatalf("update error = %v, upstream calls %d", err, len(client.updates))
	}
	restored := client.updates[1]
	if restored.Name == nil || *restored.Name != "original" || restored.Active == nil || !*restored.Active || restored.MaxConcurrency == nil || *restored.MaxConcurrency != 1 {
		t.Fatalf("upstream state not restored: %+v", restored)
	}
}

func TestKeyConcurrentServicesRespectLimit(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	for range 19 {
		if _, _, err := service.Create(ctx, 1, keyInput("existing")); err != nil {
			t.Fatal(err)
		}
	}
	other := New(service.db, client, service.box, service.provisioning)
	var workers sync.WaitGroup
	results := make(chan error, 2)
	for _, current := range []*Service{service, other} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, _, err := current.Create(ctx, 1, keyInput("concurrent"))
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	success, limited := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrKeyLimit) {
			limited++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || limited != 1 {
		t.Fatalf("success %d limited %d", success, limited)
	}
	items, err := service.List(ctx, 1)
	if err != nil || len(items) != 20 {
		t.Fatalf("keys = %d, err %v", len(items), err)
	}
}

func TestKeyUsesSharedProvisioner(t *testing.T) {
	service, client := keyFixture(t)
	shared := provision.New(service.db.DB, client)
	shared.Now = service.now
	custom := New(service.db, client, service.box, shared)
	if custom.provisioning != shared || shared.Box != service.box {
		t.Fatal("shared provisioner not reused")
	}
	if _, _, err := custom.Create(context.Background(), 1, keyInput("shared")); err != nil {
		t.Fatal(err)
	}
	identity, err := store.LoadServiceIdentity(context.Background(), service.db.DB)
	if err != nil || identity.UserID == "" {
		t.Fatalf("shared provisioner service identity missing: %+v, %v", identity, err)
	}
}

func keyInput(name string) api.KeyInput { return api.KeyInput{Name: name} }

func keyAccount(t *testing.T, service *Service, client *keyClient, userID int64, vmID, status string) int64 {
	t.Helper()
	client.AddVM(vm2api.VM{ID: vmID, Platform: "anthropic", ScheduleState: "on"})
	var id int64
	if err := service.db.QueryRow(`INSERT INTO resources(user_id,kind,vm_id,status,created_at,updated_at)
		VALUES (?,'rental',?,?,?,?) RETURNING id`, userID, vmID, status, store.Now(), store.Now()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestKeySelectedScopeLifecycle(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	first := keyAccount(t, service, client, 1, "vm-a", "active")
	second := keyAccount(t, service, client, 1, "vm-b", "suspended")
	keyAccount(t, service, client, 2, "vm-c", "active")
	input := api.KeyInput{Name: "指定", Scope: api.KeyScopeInput{Mode: "selected", Accounts: []int64{first, first}}}
	item, _, err := service.Create(ctx, 1, input)
	if err != nil {
		t.Fatal(err)
	}
	if item.Scope.Mode != "selected" || !item.Scope.Usable || !slices.Equal(item.Scope.Accounts, []int64{first}) ||
		!slices.Equal(client.creates[0].Scope.AllowedVMs, []string{"vm-a"}) || client.creates[0].Scope.PoolID != "" {
		t.Fatalf("selected create = %+v, %+v", item.Scope, client.creates[0].Scope)
	}
	for _, scope := range []api.KeyScopeInput{{Mode: "all"}, {Mode: "selected", Accounts: []int64{second}}} {
		item, err = service.Update(ctx, 1, item.ID, api.KeyPatch{Scope: &scope})
		if err != nil {
			t.Fatal(err)
		}
		upstream, err := client.GetKey(ctx, client.keys[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if scope.Mode == "all" && (upstream.PoolID == "" || len(upstream.AllowedVMs) != 0) ||
			scope.Mode == "selected" && (upstream.PoolID != "" || !slices.Equal(upstream.AllowedVMs, []string{"vm-b"})) {
			t.Fatalf("updated scope = %+v", upstream)
		}
	}
	keySQL(t, service.db, `UPDATE resources SET status='releasing' WHERE id=?`, second)
	items, err := service.List(ctx, 1)
	if err != nil || len(items) != 1 || items[0].Scope.Usable || items[0].Scope.Accounts == nil {
		t.Fatalf("unusable selected key = %+v, %v", items, err)
	}
	if _, err := service.Update(ctx, 1, item.ID, keyPatch("renamed", "")); err != nil {
		t.Fatal(err)
	}
	upstream, err := client.GetKey(ctx, client.keys[0].ID)
	if err != nil || upstream.Active || upstream.PoolID == "" || len(upstream.AllowedVMs) != 0 {
		t.Fatalf("expired selection was not closed: %+v, %v", upstream, err)
	}
	keySQL(t, service.db, `UPDATE users SET status='disabled' WHERE id=1`)
	scope := api.KeyScopeInput{Mode: "selected", Accounts: []int64{first}}
	if _, err := service.Update(ctx, 1, item.ID, api.KeyPatch{Scope: &scope}); err != nil {
		t.Fatal(err)
	}
	upstream, err = client.GetKey(ctx, client.keys[0].ID)
	if err != nil || upstream.Active {
		t.Fatalf("disabled user's key active: %+v, %v", upstream, err)
	}
	if _, _, err := service.Create(ctx, 1, keyInput("disabled user")); err != nil {
		t.Fatal(err)
	}
	if _, active := client.KeyState(client.keys[1].ID); active {
		t.Fatal("created active upstream key for disabled user")
	}
}

func TestKeyScopeValidationAndLegacy(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	owned := keyAccount(t, service, client, 1, "vm-a", "active")
	other := keyAccount(t, service, client, 2, "vm-b", "active")
	unavailable := keyAccount(t, service, client, 1, "vm-c", "awaiting_auth")
	for _, test := range []struct {
		scope api.KeyScopeInput
		code  string
	}{
		{api.KeyScopeInput{Mode: "unknown"}, "key_scope_invalid"},
		{api.KeyScopeInput{Mode: "selected", Accounts: []int64{other}}, "key_scope_invalid"},
		{api.KeyScopeInput{Mode: "selected", Accounts: []int64{99999}}, "key_scope_invalid"},
		{api.KeyScopeInput{Mode: "selected"}, "key_scope_empty"},
		{api.KeyScopeInput{Mode: "selected", Accounts: []int64{unavailable}}, "key_scope_empty"},
	} {
		_, _, err := service.Create(ctx, 1, api.KeyInput{Scope: test.scope})
		keyError(t, err, 422, test.code)
	}
	item, _, err := service.Create(ctx, 1, keyInput("legacy"))
	if err != nil {
		t.Fatal(err)
	}
	keySQL(t, service.db, `UPDATE api_keys SET upstream_owner='' WHERE id=?`, item.ID)
	scope := api.KeyScopeInput{Mode: "selected", Accounts: []int64{owned}}
	_, err = service.Update(ctx, 1, item.ID, api.KeyPatch{Scope: &scope})
	keyError(t, err, 409, "key_migrating")
	if _, err := service.Update(ctx, 1, item.ID, keyPatch("legacy renamed", "disabled")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Update(ctx, 1, item.ID, keyPatch("", "active")); err != nil {
		t.Fatal(err)
	}
}

func TestKeyScopeUpdateRestoresUpstreamOnSaveFailure(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	first := keyAccount(t, service, client, 1, "vm-original", "active")
	second := keyAccount(t, service, client, 1, "vm-new", "active")
	item, _, err := service.Create(ctx, 1, api.KeyInput{Name: "original", Scope: api.KeyScopeInput{Mode: "selected", Accounts: []int64{first}}})
	if err != nil {
		t.Fatal(err)
	}
	keySQL(t, service.db, `CREATE TRIGGER reject_scope_update BEFORE UPDATE ON api_keys BEGIN SELECT RAISE(ABORT,'update rejected'); END`)
	scope := api.KeyScopeInput{Mode: "selected", Accounts: []int64{second}}
	if _, err := service.Update(ctx, 1, item.ID, api.KeyPatch{Scope: &scope}); err == nil {
		t.Fatal("scope update succeeded despite failed save")
	}
	upstream, err := client.GetKey(ctx, client.keys[0].ID)
	if err != nil || !upstream.Active || !slices.Equal(upstream.AllowedVMs, []string{"vm-original"}) || upstream.PoolID != "" {
		t.Fatalf("scope not restored: %+v, %v", upstream, err)
	}
	items, err := service.List(ctx, 1)
	if err != nil || len(items) != 1 || !slices.Equal(items[0].Scope.Accounts, []int64{first}) {
		t.Fatalf("local scope changed: %+v, %v", items, err)
	}
}

func TestKeyCreateClosesSelectionReleasedAfterValidation(t *testing.T) {
	service, client := keyFixture(t)
	ctx := context.Background()
	account := keyAccount(t, service, client, 1, "vm-released", "active")
	// ServiceUser runs after validation but before KeyScopeFor; simulate a
	// release becoming visible at that boundary.
	client.afterUser = func() {
		keySQL(t, service.db, `UPDATE resources SET status='releasing' WHERE id=?`, account)
	}
	item, _, err := service.Create(ctx, 1, api.KeyInput{Scope: api.KeyScopeInput{Mode: "selected", Accounts: []int64{account}}})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := client.GetKey(ctx, client.keys[0].ID)
	if err != nil || item.Scope.Usable || upstream.Active || upstream.PoolID == "" || len(upstream.AllowedVMs) != 0 {
		t.Fatalf("released selection not closed: %+v, %+v, %v", item.Scope, upstream, err)
	}
}

// keyPatch builds a patch the way the old (name, status) arguments did: an
// empty string leaves the field alone.
func keyPatch(name, status string) api.KeyPatch {
	var patch api.KeyPatch
	if name != "" {
		patch.Name = &name
	}
	if status != "" {
		patch.Status = &status
	}
	return patch
}
