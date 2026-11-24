package vm2api

import (
	"slices"
	"sort"
)

// ProtocolCall is one /v1 request the mock answered, as cmd/mock-vm2api
// reports it to the Fake so request logs look like vm2api's.
type ProtocolCall struct {
	Secret       string
	Model        string
	Stream       bool
	InputTokens  int64
	OutputTokens int64
	DurationMS   int64
}

// RecordProtocolCall logs a protocol request made with a vm2api key. Like
// vm2api's dispatch, both ownership and live key scope constrain the slot.
// Unavailable pools fail with 403; no eligible account fails with 503.
// It reports the HTTP status and false for an unknown or disabled key.
func (fake *Fake) RecordProtocolCall(call ProtocolCall) (int, bool) {
	status, _, known := fake.RecordProtocolCallWithCode(call)
	return status, known
}

// RecordProtocolCallWithCode also reports the client-facing vm2api error code.
func (fake *Fake) RecordProtocolCallWithCode(call ProtocolCall) (int, string, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	var key fakeKey
	found := false
	for _, candidate := range fake.keys {
		if fake.keyActiveLocked(candidate, call.Secret) {
			key, found = candidate, true
			break
		}
	}
	if !found {
		return 0, "", false
	}
	code, status := "pool_unavailable", 503
	if key.poolID != "" {
		pool, exists := fake.pools[key.poolID]
		if !exists || !pool.Enabled || len(pool.VMIDs) == 0 {
			code, status = "vm_pool_unavailable", 403
		}
	}
	ids := make([]string, 0, len(fake.vms))
	ownerID := key.userID
	if role := fake.users[ownerID].Role; role == "admin" || role == "super" {
		ownerID = ""
	}
	for id, vm := range fake.vms {
		if vm.OwnerUserID == ownerID && vm.HasCredential && vm.ScheduleState == "on" && !vm.CredentialRejected && fake.keyAllowsVMLocked(key, vm) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	vmID := ""
	if len(ids) > 0 {
		// Spread calls over the owner's slots so per-account records differ.
		status, code, vmID = 200, "", ids[len(fake.logs)%len(ids)]
	}
	first := call.DurationMS / 3
	row := LogRow{
		RequestID: fake.nextID("req"), CreatedAt: fake.now().UTC(), Model: call.Model,
		KeyID: key.key.ID, VMID: vmID, Stream: call.Stream, StatusCode: status,
		DurationMS: call.DurationMS, FirstTokenMS: &first,
	}
	if status == 200 {
		row.InputTokens, row.OutputTokens = call.InputTokens, call.OutputTokens
		row.CostUSD = float64(call.InputTokens)*3e-6 + float64(call.OutputTokens)*15e-6
	} else {
		row.ErrorLabel, row.FirstTokenMS = code, nil
		if code == "pool_unavailable" {
			row.ErrorLabel = "account_pool_exhausted"
		}
	}
	fake.logs = append(fake.logs, fakeLog{ownerID: key.userID, row: fakeCloneLog(row)})
	return status, code, true
}

// ProtocolKeyActive authenticates protocol routes that do not schedule a VM.
func (fake *Fake) ProtocolKeyActive(secret string) bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, key := range fake.keys {
		if fake.keyActiveLocked(key, secret) {
			return true
		}
	}
	return false
}

func (fake *Fake) keyActiveLocked(key fakeKey, secret string) bool {
	return key.secret != "" && key.secret == secret && key.active && (key.userID == "" || fake.users[key.userID].Enabled)
}

func (fake *Fake) keyAllowsVMLocked(key fakeKey, vm VM) bool {
	if key.poolID != "" {
		pool, exists := fake.pools[key.poolID]
		return exists && pool.Enabled && slices.Contains(pool.VMIDs, vm.ID) && (key.groupType == "all" || key.groupType == vm.Platform)
	}
	if key.groupType == "all" {
		return true
	}
	return key.groupType == "anthropic" && vm.Platform == "anthropic" && slices.Contains(key.allowedVMs, vm.ID)
}

// KeyState reports whether a key exists upstream and is active (test hook).
func (fake *Fake) KeyState(keyID string) (exists, active bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	key, ok := fake.keys[keyID]
	return ok, ok && key.active
}

// UserEnabled reports whether an upstream user exists and is enabled (test hook).
func (fake *Fake) UserEnabled(userID string) (exists, enabled bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	user, ok := fake.users[userID]
	return ok, ok && user.Enabled
}
