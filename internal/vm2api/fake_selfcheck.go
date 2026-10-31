package vm2api

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// This file holds the Fake's self-check surface: identity, exit probe, slot
// reset, and proxy unbind/disable, plus hooks tests use to inject faults.
//
// Defaults mirror vm2api in its healthy configuration: every hosted slot gets
// a unique fingerprint, every allocated proxy is a dedicated socks5 exit with
// its own IP, and the slot timezone follows the proxy's geo timezone.

const fakeDefaultTimezone = "Asia/Tokyo"

func (fake *Fake) initSelfcheckLocked() {
	if fake.disabledProxies == nil {
		fake.disabledProxies = map[string]bool{}
		fake.exitOverride = map[string]ExitProbe{}
		fake.exitErr = map[string]error{}
		fake.resetCalls = map[string]int{}
	}
}

func (fake *Fake) newFingerprintLocked() Fingerprint {
	n := "fp"
	if !fake.hostileSelfcheck {
		n = fake.nextID("fp")
	} else {
		n = "shared"
	}
	return Fingerprint{
		DeviceID:       "dev-" + n,
		GuestMachineID: "mid-" + n,
		Hostname:       "ws-" + n,
		Timezone:       fakeDefaultTimezone,
		Locale:         "en-US",
		CollectedAt:    fake.now().UTC().Format(time.RFC3339),
	}
}

// nextProxyLocked returns the next queued proxy, or a fresh dedicated one.
// Disabled proxies are never handed out, matching vm2api's allocator.
func (fake *Fake) nextProxyLocked() ProxyView {
	fake.initSelfcheckLocked()
	if fake.hostileSelfcheck {
		return ProxyView{ID: "px-shared", Scheme: "socks5", Status: "ok", Enabled: true, GeoIP: "203.0.113.99", GeoTimezone: fakeDefaultTimezone}
	}
	for len(fake.proxyQueue) > 0 {
		next := fake.proxyQueue[0]
		fake.proxyQueue = fake.proxyQueue[1:]
		if !fake.disabledProxies[next.ID] {
			return next
		}
	}
	n := fake.nextID("px")
	return ProxyView{
		ID: n, Scheme: "socks5", Status: "ok", Enabled: true,
		GeoIP: fmt.Sprintf("203.0.113.%d", fake.sequence%250+1), GeoTimezone: fakeDefaultTimezone,
	}
}

// SetHostileSelfcheck makes every slot share one fingerprint and one exit, so
// self-check always fails. cmd/mock-vm2api uses it to demonstrate the rebuild
// budget; a rebuild cannot fix a shared fingerprint in this mode.
func (fake *Fake) SetHostileSelfcheck(hostile bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.hostileSelfcheck = hostile
}

// --- test hooks ---

// SetFingerprint overwrites a slot's fingerprint (e.g. to collide with another).
func (fake *Fake) SetFingerprint(vmID string, fp Fingerprint) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	if vm, ok := fake.vms[vmID]; ok {
		vm.Fingerprint = fp
		fake.vms[vmID] = vm
	}
}

// SetProxy replaces a slot's bound proxy; nil unbinds it.
func (fake *Fake) SetProxy(vmID string, proxy *ProxyView) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	if vm, ok := fake.vms[vmID]; ok {
		if proxy == nil {
			vm.Proxy, vm.ProxyConfigured = nil, false
		} else {
			copied := *proxy
			vm.Proxy, vm.ProxyConfigured = &copied, true
		}
		fake.vms[vmID] = vm
	}
}

// SetTimezone pins a slot's timezone (to create a geo mismatch).
func (fake *Fake) SetTimezone(vmID, tz string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	if vm, ok := fake.vms[vmID]; ok {
		vm.Timezone = tz
		fake.vms[vmID] = vm
	}
}

// SetAccount simulates a signed-in Claude account on a slot.
func (fake *Fake) SetAccount(vmID, accountUUID, email string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	if vm, ok := fake.vms[vmID]; ok {
		vm.AccountUUID, vm.Email = accountUUID, email
		fake.vms[vmID] = vm
	}
}

// QueueProxies makes the next AllocateProxy calls hand out these proxies in
// order (e.g. a shared or dead one first) before falling back to fresh ones.
func (fake *Fake) QueueProxies(proxies ...ProxyView) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	fake.initSelfcheckLocked()
	fake.proxyQueue = append(fake.proxyQueue, proxies...)
}

// SetExitProbe overrides the probe result for a slot; err != nil makes the
// probe fail instead.
func (fake *Fake) SetExitProbe(vmID string, probe ExitProbe, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	fake.initSelfcheckLocked()
	if err != nil {
		fake.exitErr[vmID] = err
		delete(fake.exitOverride, vmID)
		return
	}
	delete(fake.exitErr, vmID)
	fake.exitOverride[vmID] = probe
}

// ResetCalls reports how many times ResetVM ran for a slot.
func (fake *Fake) ResetCalls(vmID string) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initSelfcheckLocked()
	return fake.resetCalls[vmID]
}

// ProxyDisabled reports whether DisableProxy was called for a proxy.
func (fake *Fake) ProxyDisabled(proxyID string) bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initSelfcheckLocked()
	return fake.disabledProxies[proxyID]
}

// --- Client methods ---

func (fake *Fake) CollectIdentity(ctx context.Context, vmID string) (Fingerprint, error) {
	if err := fake.begin(ctx); err != nil {
		return Fingerprint{}, err
	}
	defer fake.mu.Unlock()
	vm, ok := fake.vms[vmID]
	if !ok {
		return Fingerprint{}, fakeMissing("vm")
	}
	if vm.Fingerprint != (Fingerprint{}) {
		vm.Fingerprint.CollectedAt = fake.now().UTC().Format(time.RFC3339)
		fake.vms[vmID] = vm
	}
	return vm.Fingerprint, nil
}

func (fake *Fake) ProbeExit(ctx context.Context, vmID string) (ExitProbe, error) {
	if err := fake.begin(ctx); err != nil {
		return ExitProbe{}, err
	}
	defer fake.mu.Unlock()
	fake.initSelfcheckLocked()
	vm, ok := fake.vms[vmID]
	if !ok {
		return ExitProbe{}, fakeMissing("vm")
	}
	if err := fake.exitErr[vmID]; err != nil {
		return ExitProbe{}, err
	}
	if probe, ok := fake.exitOverride[vmID]; ok {
		return probe, nil
	}
	if vm.Proxy == nil {
		return ExitProbe{}, fakeError(http.StatusBadGateway, "proxy_required", "槽位未绑定出口")
	}
	return ExitProbe{Via: vm.Proxy.Scheme, IP: vm.Proxy.GeoIP, Timezone: vm.Proxy.GeoTimezone}, nil
}

// ResetVM regenerates the fingerprint and wipes the account, keeping the id
// and the proxy binding — exactly what vm2api's reset{mode:"traits"} does.
func (fake *Fake) ResetVM(ctx context.Context, vmID string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	fake.initSelfcheckLocked()
	vm, ok := fake.vms[vmID]
	if !ok {
		return fakeMissing("vm")
	}
	fake.resetCalls[vmID]++
	vm.Fingerprint = fake.newFingerprintLocked()
	vm.HasCredential, vm.AccountUUID, vm.Email, vm.EmailMasked = false, "", "", ""
	vm.CredStatus, vm.ScheduleState, vm.UnschedulableCode = "missing", "unschedulable", "no_credential"
	delete(fake.bootstrap, vmID)
	fake.clearSessionsLocked(vmID)
	fake.vms[vmID] = vm
	return nil
}

func (fake *Fake) UnbindProxy(ctx context.Context, proxyID, vmID string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	vm, ok := fake.vms[vmID]
	if !ok {
		return fakeMissing("vm")
	}
	if vm.Proxy == nil || vm.Proxy.ID != proxyID {
		return fakeMissing("proxy")
	}
	vm.Proxy, vm.ProxyConfigured = nil, false
	fake.vms[vmID] = vm
	return nil
}

func (fake *Fake) DisableProxy(ctx context.Context, proxyID string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	fake.initSelfcheckLocked()
	fake.disabledProxies[proxyID] = true
	for id, vm := range fake.vms {
		if vm.Proxy != nil && vm.Proxy.ID == proxyID {
			vm.Proxy.Enabled = false
			fake.vms[id] = vm
		}
	}
	return nil
}
