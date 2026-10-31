package provision

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DueGin/vm2api-portal/internal/store"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

func provisionFixture(t *testing.T) (*Service, *vm2api.Fake, int64) {
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
	if _, err := database.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	fake := vm2api.NewFake()
	owner, err := fake.CreateUser(context.Background(), "tenant", "password")
	if err != nil {
		t.Fatal(err)
	}
	now := timestamp(time.Now())
	result, err := database.Exec(`INSERT INTO users(email,password_hash,vm2api_user_id,created_at,updated_at) VALUES ('tenant@example.com','hash',?,?,?)`, owner.ID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	userID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	service := New(database, fake)
	return service, fake, userID
}

func addSubscription(t *testing.T, service *Service, userID int64, kind string, quantity int, expires time.Time) int64 {
	t.Helper()
	now := timestamp(service.now())
	result, err := service.db.Exec(`INSERT INTO subscriptions(user_id,kind,quantity,status,starts_at,expires_at,created_at,updated_at) VALUES (?,?,?,'active',?,?,?,?)`, userID, kind, quantity, timestamp(service.now().Add(-time.Hour)), timestamp(expires), now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func addOrder(t *testing.T, service *Service, userID int64, kind string, units int, renewal *int64) int64 {
	t.Helper()
	now := timestamp(service.now())
	product, err := service.db.Exec(`INSERT INTO products(kind,name,unit_quantity,duration_days,price_cents,created_at,updated_at) VALUES (?,'商品',1,30,100,?,?)`, kind, now, now)
	if err != nil {
		t.Fatal(err)
	}
	productID, err := product.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.db.Exec(`INSERT INTO orders(order_no,user_id,product_id,product_snapshot,kind,quantity,units,duration_days,amount_cents,provider,status,renew_subscription_id,created_at,expires_at,updated_at) VALUES (?,?,?,'{}',?,?,?,30,100,'mock','paid',?,?,?,?)`, fmt.Sprintf("PT-%d", productID), userID, productID, kind, units, units, renewal, now, timestamp(service.now().Add(time.Hour)), now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func addResource(t *testing.T, service *Service, userID int64, kind, status, vmID string, subscriptionID *int64) int64 {
	t.Helper()
	now := timestamp(service.now())
	result, err := service.db.Exec(`INSERT INTO resources(user_id,kind,subscription_id,vm_id,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?)`, userID, kind, subscriptionID, nullText(vmID), status, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func nullText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func TestProvisionEntitlementsAndConcurrency(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	now := service.now()
	addSubscription(t, service, userID, "concurrency", 3, now.Add(time.Hour))
	addSubscription(t, service, userID, "concurrency", 4, now.Add(2*time.Hour))
	addSubscription(t, service, userID, "concurrency", 20, now.Add(-time.Hour))
	addSubscription(t, service, userID, "hosted_seat", 5, now.Add(time.Hour))
	addSubscription(t, service, userID, "rental_account", 2, now.Add(time.Hour))
	addResource(t, service, userID, "hosted", "active", "", nil)
	addResource(t, service, userID, "hosted", "awaiting_auth", "", nil)
	addResource(t, service, userID, "rental", "active", "", nil)
	if _, err := service.db.Exec(`INSERT INTO settings(key,value,updated_at) VALUES ('site','{"base_concurrency":2,"concurrency_per_hosted_seat":3,"concurrency_per_rental_account":4}',?)`, timestamp(now)); err != nil {
		t.Fatal(err)
	}
	entitlements, err := service.Entitlements(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if entitlements.Concurrency.Total != 16 || entitlements.HostedSeats.Used != 2 || entitlements.AccountsActive != 2 {
		t.Fatalf("entitlements = %+v", entitlements)
	}
	var owner string
	if err := service.db.QueryRow(`SELECT vm2api_user_id FROM users WHERE id=?`, userID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	key, err := fake.CreateKey(ctx, vm2api.CreateKeyInput{Name: "main", UserID: owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`INSERT INTO api_keys(user_id,name,key_hash,prefix,vm2api_key_id,vm2api_secret_enc,status,created_at,updated_at) VALUES (?,'main','hash','prefix',?,'encrypted','active',?,?)`, userID, key.ID, timestamp(now), timestamp(now)); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncConcurrency(ctx, userID); err != nil {
		t.Fatal(err)
	}
	var synced int
	if err := service.db.QueryRow(`SELECT synced_concurrency FROM api_keys`).Scan(&synced); err != nil {
		t.Fatal(err)
	}
	if synced != 16 {
		t.Fatalf("synced = %d", synced)
	}
}

func TestProvisionFulfilmentRenewalAndIdempotency(t *testing.T) {
	service, _, userID := provisionFixture(t)
	ctx := context.Background()
	fixed := service.now().Truncate(time.Second)
	service.Now = func() time.Time { return fixed }
	orderID := addOrder(t, service, userID, "hosted_seat", 2, nil)
	if err := service.FulfilOrder(ctx, orderID); err != nil {
		t.Fatal(err)
	}
	if err := service.FulfilOrder(ctx, orderID); err != nil {
		t.Fatal(err)
	}
	var subscriptions, resources int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM subscriptions`).Scan(&subscriptions); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM resources`).Scan(&resources); err != nil {
		t.Fatal(err)
	}
	if subscriptions != 1 || resources != 0 {
		t.Fatalf("subscriptions=%d resources=%d", subscriptions, resources)
	}
	var subscriptionID int64
	if err := service.db.QueryRow(`SELECT id FROM subscriptions`).Scan(&subscriptionID); err != nil {
		t.Fatal(err)
	}
	renewal := addOrder(t, service, userID, "hosted_seat", 2, &subscriptionID)
	if err := service.FulfilOrder(ctx, renewal); err != nil {
		t.Fatal(err)
	}
	if err := service.FulfilOrder(ctx, renewal); err != nil {
		t.Fatal(err)
	}
	var expiry string
	if err := service.db.QueryRow(`SELECT expires_at FROM subscriptions WHERE id=?`, subscriptionID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	if expiry != timestamp(fixed.AddDate(0, 0, 60)) {
		t.Fatalf("expiry=%s", expiry)
	}
	service.Now = func() time.Time { return fixed.AddDate(0, 0, 100) }
	lateRenewal := addOrder(t, service, userID, "hosted_seat", 2, &subscriptionID)
	if err := service.FulfilOrder(ctx, lateRenewal); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT expires_at FROM subscriptions WHERE id=?`, subscriptionID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	if expiry != timestamp(fixed.AddDate(0, 0, 130)) {
		t.Fatalf("late expiry=%s", expiry)
	}
}

func TestProvisionHostedOAuthLifecycle(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	addSubscription(t, service, userID, "hosted_seat", 1, service.now().Add(time.Hour))
	account, err := service.CreateHosted(ctx, userID, "主账号")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateHosted(ctx, userID, "第二个"); !errors.Is(err, ErrNoFreeSeat) {
		t.Fatalf("seat error=%v", err)
	}
	if err := service.ProvisionHosted(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.ProvisionHosted(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	account, err = service.Account(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "awaiting_auth" || account.VMID == "" {
		t.Fatalf("account=%+v", account)
	}
	if _, err := service.GenerateAuthURL(ctx, userID+1, account.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign auth error=%v", err)
	}
	auth, err := service.GenerateAuthURL(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SubmitAuthCode(ctx, userID, account.ID, auth.SessionID, "code#state"); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		err := service.watchBootstrap(ctx, account.ID, service.now().Add(time.Hour))
		var retry *RetryAfterError
		if attempt < 2 && !errors.As(err, &retry) {
			t.Fatalf("bootstrap %d error=%v", attempt, err)
		}
		if attempt == 2 && err != nil {
			t.Fatal(err)
		}
	}
	account, err = service.Account(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "active" {
		t.Fatalf("status=%s", account.Status)
	}
	usage, err := fake.GetAccountUsage(ctx, account.VMID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.VM.ScheduleState != "on" {
		t.Fatalf("schedule=%s", usage.VM.ScheduleState)
	}
	encoded, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	if _, exposed := view["vm_id"]; exposed {
		t.Fatal("vm_id exposed")
	}
	if err := service.DeleteHosted(ctx, userID, account.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.ReleaseResource(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.GetAccountUsage(ctx, account.VMID); !vm2api.IsStatus(err, 404) {
		t.Fatalf("deleted hosted vm=%v", err)
	}
}

func TestProvisionExpiredOAuthSession(t *testing.T) {
	service, _, userID := provisionFixture(t)
	ctx := context.Background()
	addSubscription(t, service, userID, "hosted_seat", 1, service.now().Add(time.Hour))
	account, err := service.CreateHosted(ctx, userID, "主号")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ProvisionHosted(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	auth, err := service.GenerateAuthURL(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`UPDATE oauth_sessions SET expires_at=?`, timestamp(service.now().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := service.SubmitAuthCode(ctx, userID, account.ID, auth.SessionID, "code"); !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("expired error=%v", err)
	}
	var status string
	if err := service.db.QueryRow(`SELECT status FROM oauth_sessions`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "expired" {
		t.Fatalf("session status=%s", status)
	}
}

func TestProvisionRentalExpiryAndStockFailure(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	orderID := addOrder(t, service, userID, "rental_account", 2, nil)
	vm, err := fake.CreateHostedVM(ctx, "租赁", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := service.now().Truncate(time.Second)
	service.Now = func() time.Time { return now }
	if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,created_at,updated_at) VALUES (?,'available',?,?)`, vm.ID, timestamp(now), timestamp(now)); err != nil {
		t.Fatal(err)
	}
	if err := service.FulfilOrder(ctx, orderID); !errors.Is(err, ErrOutOfStock) {
		t.Fatalf("stock error=%v", err)
	}
	var status string
	if err := service.db.QueryRow(`SELECT status FROM orders WHERE id=?`, orderID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fulfill_failed" {
		t.Fatalf("order status=%s", status)
	}
	second, err := fake.CreateHostedVM(ctx, "租赁2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,created_at,updated_at) VALUES (?,'available',?,?)`, second.ID, timestamp(now), timestamp(now)); err != nil {
		t.Fatal(err)
	}
	if err := service.FulfilOrder(ctx, orderID); err != nil {
		t.Fatal(err)
	}
	accounts, err := service.ListAccounts(ctx, userID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts=%+v", accounts)
	}
	service.Now = func() time.Time { return now.AddDate(0, 0, 31) }
	if err := service.SweepExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if err := service.ReleaseResource(ctx, account.ID); err != nil {
			t.Fatal(err)
		}
		usage, err := fake.GetAccountUsage(ctx, account.VMID)
		if err != nil {
			t.Fatal(err)
		}
		if usage.VM.OwnerUserID != "" {
			t.Fatalf("owner=%s", usage.VM.OwnerUserID)
		}
	}
	var available int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM rental_pool WHERE status='available'`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if available != 2 {
		t.Fatalf("available=%d", available)
	}
}

func TestProvisionConcurrentHostedSeat(t *testing.T) {
	service, _, userID := provisionFixture(t)
	addSubscription(t, service, userID, "hosted_seat", 1, service.now().Add(time.Hour))
	var group sync.WaitGroup
	results := make(chan error, 8)
	for attempt := 0; attempt < 8; attempt++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := service.CreateHosted(context.Background(), userID, "账号")
			results <- err
		}()
	}
	group.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !errors.Is(err, ErrNoFreeSeat) {
			t.Fatal(err)
		}
	}
	if created != 1 {
		t.Fatalf("created=%d", created)
	}
}

func TestProvisionConcurrentUpstreamUser(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	if _, err := service.db.Exec(`UPDATE users SET vm2api_user_id=NULL WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	results := make(chan string, 8)
	failures := make(chan error, 8)
	for attempt := 0; attempt < 8; attempt++ {
		group.Add(1)
		go func() {
			defer group.Done()
			owner, err := service.EnsureUpstreamUser(context.Background(), userID)
			results <- owner
			failures <- err
		}()
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	owners := make(map[string]bool)
	for owner := range results {
		owners[owner] = true
	}
	if len(owners) != 1 || owners[""] {
		t.Fatalf("owners=%v", owners)
	}
	for owner := range owners {
		if _, err := fake.CreateKey(context.Background(), vm2api.CreateKeyInput{Name: "check", UserID: owner}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProvisionHostedGraceRenewalProtectsRelease(t *testing.T) {
	service, _, userID := provisionFixture(t)
	ctx := context.Background()
	now := service.now().Truncate(time.Second)
	service.Now = func() time.Time { return now }
	subscriptionID := addSubscription(t, service, userID, "hosted_seat", 1, now.Add(time.Hour))
	account, err := service.CreateHosted(ctx, userID, "托管")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ProvisionHosted(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if err := service.SweepExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	account, err = service.Account(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "suspended" || account.ReleaseAfter == nil {
		t.Fatalf("suspended=%+v", account)
	}
	if err := service.ReleaseResource(ctx, account.ID); err == nil {
		t.Fatal("released during grace")
	}
	renewal := addOrder(t, service, userID, "hosted_seat", 1, &subscriptionID)
	if err := service.FulfilOrder(ctx, renewal); err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now.Add(27 * time.Hour) }
	if err := service.ReleaseResource(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	account, err = service.Account(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "awaiting_auth" || account.ReleaseAfter != nil {
		t.Fatalf("resumed=%+v", account)
	}
}

func TestProvisionExpirySuspendsNewestHostedFirst(t *testing.T) {
	service, _, userID := provisionFixture(t)
	ctx := context.Background()
	now := service.now().Truncate(time.Second)
	service.Now = func() time.Time { return now }
	addSubscription(t, service, userID, "hosted_seat", 1, now.Add(time.Hour))
	addSubscription(t, service, userID, "hosted_seat", 1, now.Add(48*time.Hour))
	first, err := service.CreateHosted(ctx, userID, "最早")
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now.Add(time.Minute) }
	second, err := service.CreateHosted(ctx, userID, "最新")
	if err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if err := service.SweepExpiry(ctx); err != nil {
		t.Fatal(err)
	}
	first, err = service.Account(ctx, userID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err = service.Account(ctx, userID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "provisioning" || second.Status != "suspended" {
		t.Fatalf("first=%s second=%s", first.Status, second.Status)
	}
	service.Now = func() time.Time { return now.Add(27 * time.Hour) }
	if err := service.ReleaseResource(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	second, err = service.Account(ctx, userID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "released" {
		t.Fatalf("released=%s", second.Status)
	}
}

type failingProvisionClient struct {
	vm2api.Client
	proxyFailure   bool
	cleanupFailure bool
	ownerCalls     int
}

func (client *failingProvisionClient) AllocateProxy(ctx context.Context, vmID string) error {
	if client.proxyFailure {
		return errors.New("proxy unavailable")
	}
	return client.Client.AllocateProxy(ctx, vmID)
}

func (client *failingProvisionClient) SetVMOwner(ctx context.Context, vmID string, owner *string) error {
	if owner == nil && client.cleanupFailure {
		return errors.New("owner cleanup failed")
	}
	if owner != nil {
		client.ownerCalls++
		if client.ownerCalls == 2 {
			return errors.New("owner assignment failed")
		}
	}
	return client.Client.SetVMOwner(ctx, vmID, owner)
}

func TestProvisionFailureRecordsErrorAndReusesVM(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	client := &failingProvisionClient{Client: fake, proxyFailure: true}
	service.client = client
	addSubscription(t, service, userID, "hosted_seat", 1, service.now().Add(time.Hour))
	account, err := service.CreateHosted(ctx, userID, "重试")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ProvisionHosted(ctx, account.ID); err == nil {
		t.Fatal("expected proxy failure")
	}
	account, err = service.Account(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "failed" || account.LastError == nil || account.VMID == "" {
		t.Fatalf("failure=%+v", account)
	}
	client.proxyFailure = false
	client.ownerCalls = 0
	if err := service.ProvisionHosted(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	vms, err := fake.ListVMs(ctx, vm2api.VMFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 || vms[0].ID != account.VMID {
		t.Fatalf("vms=%+v", vms)
	}
}

func TestProvisionRentalAssignmentCompensation(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	service.client = &failingProvisionClient{Client: fake}
	for index := 0; index < 2; index++ {
		vm, err := fake.CreateHostedVM(ctx, "租赁", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,created_at,updated_at) VALUES (?,'available',?,?)`, vm.ID, timestamp(service.now()), timestamp(service.now())); err != nil {
			t.Fatal(err)
		}
	}
	orderID := addOrder(t, service, userID, "rental_account", 2, nil)
	if err := service.FulfilOrder(ctx, orderID); err == nil {
		t.Fatal("expected assignment failure")
	}
	var resources, subscriptions, available int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM resources`).Scan(&resources); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM subscriptions`).Scan(&subscriptions); err != nil {
		t.Fatal(err)
	}
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM rental_pool WHERE status='available'`).Scan(&available); err != nil {
		t.Fatal(err)
	}
	if resources != 0 || subscriptions != 0 || available != 2 {
		t.Fatalf("resources=%d subscriptions=%d available=%d", resources, subscriptions, available)
	}
	vms, err := fake.ListVMs(ctx, vm2api.VMFilter{Unowned: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 2 {
		t.Fatalf("unowned=%+v", vms)
	}
}

func TestProvisionQueuedFulfilmentAndHostedBootstrap(t *testing.T) {
	service, _, userID := provisionFixture(t)
	ctx := context.Background()
	now := service.now().Truncate(time.Second)
	service.Now = func() time.Time { return now }
	service.engine.Now = func() time.Time { return now }
	orderID := addOrder(t, service, userID, "hosted_seat", 1, nil)
	if err := service.EnqueueFulfilOrder(ctx, orderID); err != nil {
		t.Fatal(err)
	}
	if err := service.EnqueueFulfilOrder(ctx, orderID); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := service.engine.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	if err := service.db.QueryRow(`SELECT status FROM orders WHERE id=?`, orderID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fulfilled" {
		t.Fatalf("order=%s", status)
	}
	account, err := service.CreateHosted(ctx, userID, "后台托管")
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := service.engine.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("provision worked=%v err=%v", worked, err)
	}
	auth, err := service.GenerateAuthURL(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SubmitAuthCode(ctx, userID, account.ID, auth.SessionID, "code"); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		if worked, err := service.engine.RunOnce(ctx); err != nil || !worked {
			t.Fatalf("watch %d worked=%v err=%v", attempt, worked, err)
		}
		now = now.Add(3 * time.Second)
	}
	account, err = service.Account(ctx, userID, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "active" {
		t.Fatalf("account=%+v", account)
	}
	var attempts int
	if err := service.db.QueryRow(`SELECT attempts FROM jobs WHERE type='watch_bootstrap'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("poll attempts=%d", attempts)
	}
}

func TestProvisionRentalCleanupFailureQuarantinesStock(t *testing.T) {
	service, fake, userID := provisionFixture(t)
	ctx := context.Background()
	service.client = &failingProvisionClient{Client: fake, cleanupFailure: true}
	for index := 0; index < 2; index++ {
		vm, err := fake.CreateHostedVM(ctx, "租赁", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.db.Exec(`INSERT INTO rental_pool(vm_id,status,created_at,updated_at) VALUES (?,'available',?,?)`, vm.ID, timestamp(service.now()), timestamp(service.now())); err != nil {
			t.Fatal(err)
		}
	}
	orderID := addOrder(t, service, userID, "rental_account", 2, nil)
	if err := service.FulfilOrder(ctx, orderID); err == nil {
		t.Fatal("expected assignment and cleanup failure")
	}
	var maintenance int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM rental_pool WHERE status='maintenance'`).Scan(&maintenance); err != nil {
		t.Fatal(err)
	}
	if maintenance != 2 {
		t.Fatalf("maintenance=%d", maintenance)
	}
}
