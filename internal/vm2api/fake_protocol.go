package vm2api

import "sort"

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
// vm2api's owner-scoped dispatch, it is served by a signed-in slot the key's
// owner holds; with none, it is logged as a 503 without a slot. It reports
// the HTTP status the caller should answer with, and false for an unknown or
// disabled key.
func (fake *Fake) RecordProtocolCall(call ProtocolCall) (int, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.initLocked()
	var key fakeKey
	found := false
	for _, candidate := range fake.keys {
		if candidate.secret != "" && candidate.secret == call.Secret && candidate.active {
			key, found = candidate, true
			break
		}
	}
	if !found {
		return 0, false
	}
	ids := make([]string, 0, len(fake.vms))
	for id, vm := range fake.vms {
		if vm.OwnerUserID == key.userID && vm.HasCredential {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	status, vmID := 503, ""
	if len(ids) > 0 {
		// Spread calls over the owner's slots so per-account records differ.
		status, vmID = 200, ids[len(fake.logs)%len(ids)]
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
		row.ErrorLabel, row.FirstTokenMS = "no_available_account", nil
	}
	fake.logs = append(fake.logs, fakeLog{ownerID: key.userID, row: fakeCloneLog(row)})
	return status, true
}
