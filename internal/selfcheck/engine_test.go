package selfcheck

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DueGin/vm2api-portal/internal/store"
	"github.com/DueGin/vm2api-portal/internal/vm2api"
)

func newTestEngine(t *testing.T) (*Engine, *vm2api.Fake) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fake := vm2api.NewFake()
	engine := NewEngine(fake, db.DB)
	engine.now = func() time.Time { return time.Date(2026, time.October, 31, 12, 0, 0, 0, time.UTC) }
	return engine, fake
}

func createHealthySlot(t *testing.T, fake *vm2api.Fake, label string) vm2api.VM {
	t.Helper()
	ctx := context.Background()
	vm, err := fake.CreateHostedVM(ctx, label, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.AllocateProxy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	vms, err := fake.ListVMs(ctx, vm2api.VMFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range vms {
		if slot.ID == vm.ID {
			return slot
		}
	}
	t.Fatalf("new slot %q missing", vm.ID)
	return vm2api.VM{}
}

func strictOptions() Options {
	return Options{Rules: Rules{RequireDedicatedProxy: true, RequireTimezoneMatch: true}, CheckAccount: true}
}

func TestEngineCheckFakeScenarios(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, *Engine, *vm2api.Fake, vm2api.VM, vm2api.VM)
		code   Code
		remedy Remedy
	}{
		{name: "healthy"},
		{
			name: "shared proxy", code: ProxyExclusive, remedy: RemedyReproxy,
			change: func(t *testing.T, engine *Engine, fake *vm2api.Fake, target, peer vm2api.VM) {
				ctx := context.Background()
				if err := fake.UnbindProxy(ctx, target.Proxy.ID, target.ID); err != nil {
					t.Fatal(err)
				}
				if err := fake.UnbindProxy(ctx, peer.Proxy.ID, peer.ID); err != nil {
					t.Fatal(err)
				}
				shared := *target.Proxy
				fake.QueueProxies(shared, shared)
				if err := fake.AllocateProxy(ctx, target.ID); err != nil {
					t.Fatal(err)
				}
				if err := fake.AllocateProxy(ctx, peer.ID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "duplicate fingerprint", code: FingerprintUnique, remedy: RemedyReset,
			change: func(t *testing.T, engine *Engine, fake *vm2api.Fake, target, peer vm2api.VM) {
				fingerprint := peer.Fingerprint
				fingerprint.DeviceID = target.Fingerprint.DeviceID
				fake.SetFingerprint(peer.ID, fingerprint)
			},
		},
		{
			name: "probe error", code: ExitProbe, remedy: RemedyReproxy,
			change: func(t *testing.T, engine *Engine, fake *vm2api.Fake, target, peer vm2api.VM) {
				fake.SetExitProbe(target.ID, vm2api.ExitProbe{}, errors.New("probe unavailable"))
			},
		},
		{
			name: "timezone mismatch", code: ExitGeoTimezone, remedy: RemedyReproxy,
			change: func(t *testing.T, engine *Engine, fake *vm2api.Fake, target, peer vm2api.VM) {
				fake.SetTimezone(target.ID, "Europe/Paris")
			},
		},
		{
			name: "duplicate account", code: AccountUnique, remedy: RemedyManual,
			change: func(t *testing.T, engine *Engine, fake *vm2api.Fake, target, peer vm2api.VM) {
				fake.SetAccount(target.ID, "same-account", "one@example.com")
				fake.SetAccount(peer.ID, "same-account", "two@example.com")
			},
		},
		{
			name: "recorded peer exit", code: ExitIPUnique, remedy: RemedyReproxy,
			change: func(t *testing.T, engine *Engine, fake *vm2api.Fake, target, peer vm2api.VM) {
				ctx := context.Background()
				fake.SetExitProbe(peer.ID, vm2api.ExitProbe{IP: "198.51.100.42", Timezone: peer.Timezone}, nil)
				result, err := engine.Check(ctx, peer.ID, strictOptions())
				if err != nil || !result.Passed {
					t.Fatalf("peer check = %+v, error = %v", result, err)
				}
				if err := engine.Record(ctx, 0, 0, result); err != nil {
					t.Fatal(err)
				}
				fake.SetExitProbe(target.ID, vm2api.ExitProbe{IP: "198.51.100.42", Timezone: target.Timezone}, nil)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, fake := newTestEngine(t)
			target := createHealthySlot(t, fake, "目标槽位")
			peer := createHealthySlot(t, fake, "其它槽位")
			if test.change != nil {
				test.change(t, engine, fake, target, peer)
			}
			result, err := engine.Check(context.Background(), target.ID, strictOptions())
			if err != nil {
				t.Fatal(err)
			}
			if result.VMID != target.ID || result.ProxyID != target.Proxy.ID || !result.CheckedAt.Equal(engine.now()) {
				t.Fatalf("result metadata = %+v", result)
			}
			if test.code == "" {
				if !result.Passed || result.Remedy() != RemedyNone {
					t.Fatalf("healthy result = %+v", result)
				}
				return
			}
			check := findCheck(t, result, test.code)
			if result.Passed || check.OK || check.Skipped || check.Remedy != test.remedy || result.Remedy() != test.remedy {
				t.Fatalf("result = %+v; want %s failure with %s", result, test.code, test.remedy)
			}
		})
	}
}

type snapshotClient struct {
	vm2api.Client
	listErr      error
	collectErr   error
	collected    *vm2api.Fingerprint
	filter       vm2api.VMFilter
	listCalls    int
	collectCalls int
	probeCalls   int
}

func (client *snapshotClient) ListVMs(ctx context.Context, filter vm2api.VMFilter) ([]vm2api.VM, error) {
	client.filter = filter
	client.listCalls++
	if client.listErr != nil {
		return nil, client.listErr
	}
	return client.Client.ListVMs(ctx, filter)
}

func (client *snapshotClient) CollectIdentity(ctx context.Context, vmID string) (vm2api.Fingerprint, error) {
	client.collectCalls++
	if client.collectErr != nil {
		return vm2api.Fingerprint{}, client.collectErr
	}
	if client.collected != nil {
		return *client.collected, nil
	}
	return client.Client.CollectIdentity(ctx, vmID)
}

func (client *snapshotClient) ProbeExit(ctx context.Context, vmID string) (vm2api.ExitProbe, error) {
	client.probeCalls++
	return client.Client.ProbeExit(ctx, vmID)
}

func TestEngineSnapshot(t *testing.T) {
	tests := []struct {
		name            string
		change          func(*vm2api.Fake, *snapshotClient, vm2api.VM)
		wantPassed      bool
		wantMissingText string
	}{
		{name: "listed fingerprint retained on collection error", change: func(fake *vm2api.Fake, client *snapshotClient, target vm2api.VM) {
			client.collectErr = errors.New("collect failed")
		}, wantPassed: true},
		{name: "empty fingerprint on collection error", change: func(fake *vm2api.Fake, client *snapshotClient, target vm2api.VM) {
			fake.SetFingerprint(target.ID, vm2api.Fingerprint{})
			client.collectErr = errors.New("collect failed")
		}, wantMissingText: "无法采集槽位指纹"},
		{name: "fresh fingerprint replaces listed fingerprint", change: func(fake *vm2api.Fake, client *snapshotClient, target vm2api.VM) {
			client.collected = &vm2api.Fingerprint{}
		}, wantMissingText: "槽位设备指纹不完整"},
		{name: "fresh fingerprint recovers listed fingerprint", change: func(fake *vm2api.Fake, client *snapshotClient, target vm2api.VM) {
			fingerprint := target.Fingerprint
			client.collected = &fingerprint
			fake.SetFingerprint(target.ID, vm2api.Fingerprint{})
		}, wantPassed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, fake := newTestEngine(t)
			target := createHealthySlot(t, fake, "目标槽位")
			client := &snapshotClient{Client: fake}
			engine.client = client
			test.change(fake, client, target)
			result, err := engine.Check(context.Background(), target.ID, strictOptions())
			if err != nil || result.Passed != test.wantPassed {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
			if client.listCalls != 1 || client.collectCalls != 1 || client.probeCalls != 1 || client.filter != (vm2api.VMFilter{}) {
				t.Fatalf("snapshot calls = %+v", client)
			}
			if test.wantMissingText != "" && !strings.Contains(findCheck(t, result, FingerprintPresent).Message, test.wantMissingText) {
				t.Fatalf("fingerprint failure = %+v", findCheck(t, result, FingerprintPresent))
			}
		})
	}
}

func TestEngineSnapshotErrors(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		engine, fake := newTestEngine(t)
		cause := errors.New("list unavailable")
		client := &snapshotClient{Client: fake, listErr: cause}
		engine.client = client
		if _, err := engine.Check(context.Background(), "missing", strictOptions()); !errors.Is(err, cause) {
			t.Fatalf("error = %v, want wrapped cause", err)
		}
		if client.collectCalls != 0 || client.probeCalls != 0 {
			t.Fatal("collection/probe called after list error")
		}
	})
	t.Run("target missing", func(t *testing.T) {
		engine, fake := newTestEngine(t)
		createHealthySlot(t, fake, "其它槽位")
		client := &snapshotClient{Client: fake}
		engine.client = client
		if _, err := engine.Check(context.Background(), "missing", strictOptions()); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("error = %v", err)
		}
		if client.collectCalls != 0 || client.probeCalls != 0 {
			t.Fatal("collection/probe called for missing target")
		}
	})
}

func TestEngineIncludesOtherOwnersAndPlatforms(t *testing.T) {
	engine, fake := newTestEngine(t)
	target := createHealthySlot(t, fake, "目标槽位")
	peer := createHealthySlot(t, fake, "其它槽位")
	peer.Platform, peer.OwnerUserID = "openai", "another-owner"
	peer.Fingerprint.DeviceID = target.Fingerprint.DeviceID
	fake.AddVM(peer)
	result, err := engine.Check(context.Background(), target.ID, strictOptions())
	if err != nil || findCheck(t, result, FingerprintUnique).OK {
		t.Fatalf("cross-owner/platform collision not detected: %+v, error = %v", result, err)
	}
	if !strings.Contains(findCheck(t, result, FingerprintUnique).Detail, peer.ID) {
		t.Fatalf("peer missing from detail: %+v", result)
	}
}

func TestEngineOptions(t *testing.T) {
	engine, fake := newTestEngine(t)
	target := createHealthySlot(t, fake, "目标槽位")
	peer := createHealthySlot(t, fake, "其它槽位")
	proxy := *target.Proxy
	proxy.Scheme = "local"
	fake.SetProxy(target.ID, &proxy)
	fake.SetTimezone(target.ID, "Europe/Paris")
	fake.SetAccount(target.ID, "same-account", "one@example.com")
	fake.SetAccount(peer.ID, "same-account", "two@example.com")
	result, err := engine.Check(context.Background(), target.ID, Options{})
	if err != nil || !result.Passed {
		t.Fatalf("disabled rules not respected: %+v, error = %v", result, err)
	}
	if !findCheck(t, result, ExitGeoTimezone).Skipped || !findCheck(t, result, AccountUnique).Skipped {
		t.Fatalf("disabled checks not skipped: %+v", result)
	}
}

func TestEnginePeerExitHistory(t *testing.T) {
	tests := []struct {
		name    string
		history func(time.Time) []Result
		wantOK  bool
	}{
		{name: "recent failure still counts", history: func(now time.Time) []Result {
			return []Result{{ExitIP: "198.51.100.42", CheckedAt: now.Add(-time.Hour)}}
		}},
		{name: "older than 24h ignored", history: func(now time.Time) []Result {
			return []Result{{ExitIP: "198.51.100.42", CheckedAt: now.Add(-24*time.Hour - time.Second)}}
		}, wantOK: true},
		{name: "24h boundary included", history: func(now time.Time) []Result {
			return []Result{{ExitIP: "198.51.100.42", CheckedAt: now.Add(-24 * time.Hour)}}
		}},
		{name: "latest timestamp wins over insertion order", history: func(now time.Time) []Result {
			return []Result{{ExitIP: "198.51.100.99", CheckedAt: now.Add(-time.Hour)}, {ExitIP: "198.51.100.42", CheckedAt: now.Add(-2 * time.Hour)}}
		}, wantOK: true},
		{name: "latest id breaks same timestamp tie", history: func(now time.Time) []Result {
			return []Result{{ExitIP: "198.51.100.99", CheckedAt: now}, {ExitIP: "198.51.100.42", CheckedAt: now}}
		}},
		{name: "latest empty ip ignored", history: func(now time.Time) []Result {
			return []Result{{ExitIP: "198.51.100.42", CheckedAt: now.Add(-time.Hour)}, {CheckedAt: now}}
		}},
		{name: "future history ignored", history: func(now time.Time) []Result {
			return []Result{{ExitIP: "198.51.100.42", CheckedAt: now.Add(time.Second)}}
		}, wantOK: true},
		{name: "utc normalized", history: func(now time.Time) []Result {
			return []Result{{ExitIP: "198.51.100.42", CheckedAt: now.Add(-time.Hour).In(time.FixedZone("local", 8*60*60))}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, fake := newTestEngine(t)
			target := createHealthySlot(t, fake, "目标槽位")
			peer := createHealthySlot(t, fake, "其它槽位")
			fake.SetExitProbe(target.ID, vm2api.ExitProbe{IP: "198.51.100.42", Timezone: target.Timezone}, nil)
			for _, record := range test.history(engine.now()) {
				record.VMID = peer.ID
				if err := engine.Record(context.Background(), 0, 0, record); err != nil {
					t.Fatal(err)
				}
			}
			result, err := engine.Check(context.Background(), target.ID, strictOptions())
			if err != nil {
				t.Fatal(err)
			}
			if check := findCheck(t, result, ExitIPUnique); check.OK != test.wantOK {
				t.Fatalf("exit history check = %+v, want ok=%t", check, test.wantOK)
			}
		})
	}
}

func TestEngineHistoryFailureIsNotSnapshotError(t *testing.T) {
	tests := []string{"no database", "missing table", "closed database"}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			engine, fake := newTestEngine(t)
			target := createHealthySlot(t, fake, "目标槽位")
			switch name {
			case "no database":
				engine.db = nil
			case "missing table":
				if _, err := engine.db.Exec(`DROP TABLE slot_checks`); err != nil {
					t.Fatal(err)
				}
			case "closed database":
				if err := engine.db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			result, err := engine.Check(context.Background(), target.ID, strictOptions())
			if err != nil || !result.Passed {
				t.Fatalf("history failure prevented snapshot: %+v, error = %v", result, err)
			}
		})
	}
}

func TestEngineRecord(t *testing.T) {
	engine, _ := newTestEngine(t)
	ctx := context.Background()
	if _, err := engine.db.ExecContext(ctx, `INSERT INTO users (id, email, password_hash, created_at, updated_at)
		VALUES (1, 'owner@example.com', '', '', '');
		INSERT INTO resources (id, user_id, kind, status, created_at, updated_at)
		VALUES (1, 1, 'hosted', 'awaiting_auth', '', '')`); err != nil {
		t.Fatal(err)
	}
	for _, resourceID := range []int64{0, 1} {
		for _, passed := range []bool{false, true} {
			result := Evaluate(healthyInput())
			result.Passed = passed
			if err := engine.Record(ctx, resourceID, 3, result); err != nil {
				t.Fatal(err)
			}
			var storedResource sql.NullInt64
			var vmID, checksJSON, exitIP, proxyID, createdAt string
			var attempt, storedPassed int
			if err := engine.db.QueryRowContext(ctx, `SELECT resource_id, vm_id, attempt, passed, checks, exit_ip, proxy_id, created_at
				FROM slot_checks ORDER BY id DESC LIMIT 1`).Scan(&storedResource, &vmID, &attempt, &storedPassed, &checksJSON, &exitIP, &proxyID, &createdAt); err != nil {
				t.Fatal(err)
			}
			if storedResource.Valid != (resourceID != 0) || storedResource.Int64 != resourceID || vmID != result.VMID || attempt != 3 || (storedPassed == 1) != passed || exitIP != result.ExitIP || proxyID != result.ProxyID {
				t.Fatalf("stored record = %v, %s, %d, %d, %s, %s", storedResource, vmID, attempt, storedPassed, exitIP, proxyID)
			}
			if createdAt != "2026-10-31T04:00:00Z" {
				t.Errorf("created_at = %q, want RFC3339 UTC result time", createdAt)
			}
			var checks []Check
			if err := json.Unmarshal([]byte(checksJSON), &checks); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(checks, result.Checks) {
				t.Errorf("stored checks = %+v, want %+v", checks, result.Checks)
			}
		}
	}
}

func TestEngineRecordErrors(t *testing.T) {
	t.Run("nil database", func(t *testing.T) {
		engine := NewEngine(vm2api.NewFake(), nil)
		if err := engine.Record(context.Background(), 0, 0, Result{}); err == nil {
			t.Fatal("expected unavailable database error")
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		engine, _ := newTestEngine(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := engine.Record(ctx, 0, 0, Result{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want wrapped canceled context", err)
		}
	})
	t.Run("invalid resource", func(t *testing.T) {
		engine, _ := newTestEngine(t)
		if err := engine.Record(context.Background(), 99, 0, Result{}); err == nil {
			t.Fatal("expected foreign key error")
		}
	})
}
