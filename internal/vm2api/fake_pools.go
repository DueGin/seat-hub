package vm2api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"unicode/utf16"
)

func fakeKeyInfo(key fakeKey) KeyInfo {
	return KeyInfo{
		ID: key.key.ID, Name: key.name, UserID: key.userID, Active: key.active,
		MaxConcurrency: key.maxConcurrency, GroupType: key.groupType, PoolID: key.poolID,
		AllowedVMs: append([]string{}, key.allowedVMs...),
	}
}

func (fake *Fake) GetKey(ctx context.Context, keyID string) (KeyInfo, error) {
	if err := fake.begin(ctx); err != nil {
		return KeyInfo{}, err
	}
	defer fake.mu.Unlock()
	key, exists := fake.keys[keyID]
	if !exists {
		return KeyInfo{}, fakeMissing("api_key")
	}
	return fakeKeyInfo(key), nil
}

func (fake *Fake) ListKeys(ctx context.Context, userID string) ([]KeyInfo, error) {
	if err := fake.begin(ctx); err != nil {
		return nil, err
	}
	defer fake.mu.Unlock()
	keys := make([]KeyInfo, 0)
	for _, key := range fake.keys {
		if userID == "" || key.userID == userID {
			keys = append(keys, fakeKeyInfo(key))
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys, nil
}

func fakeClonePool(pool Pool) Pool {
	pool.VMIDs = append([]string{}, pool.VMIDs...)
	return pool
}

func (fake *Fake) ListPools(ctx context.Context) ([]Pool, error) {
	if err := fake.begin(ctx); err != nil {
		return nil, err
	}
	defer fake.mu.Unlock()
	pools := make([]Pool, 0, len(fake.pools))
	for _, pool := range fake.pools {
		pools = append(pools, fakeClonePool(pool))
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].ID < pools[j].ID })
	return pools, nil
}

func (fake *Fake) poolMembersLocked(poolID string, vmIDs []string) ([]string, error) {
	ids := make([]string, 0, len(vmIDs))
	for _, value := range vmIDs {
		id := strings.TrimSpace(value)
		if id == "" || slices.Contains(ids, id) {
			continue
		}
		if _, exists := fake.vms[id]; !exists {
			return nil, fakeError(400, "vm_pool_vm_unknown", "槽位不存在")
		}
		for _, pool := range fake.pools {
			if pool.ID != poolID && slices.Contains(pool.VMIDs, id) {
				return nil, fakeError(409, "vm_in_other_pool", "槽位已在其他账号池")
			}
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func (fake *Fake) CreatePool(ctx context.Context, name string, vmIDs []string) (Pool, error) {
	if err := fake.begin(ctx); err != nil {
		return Pool{}, err
	}
	defer fake.mu.Unlock()
	name = strings.TrimSpace(name)
	if n := len(utf16.Encode([]rune(name))); n < 1 || n > 40 {
		return Pool{}, fakeError(400, "invalid_vm_pool_name", "账号池名称需要 1–40 个字符")
	}
	for _, pool := range fake.pools {
		if pool.Name == name {
			return Pool{}, fakeError(409, "vm_pool_name_taken", "账号池名称已存在")
		}
	}
	ids, err := fake.poolMembersLocked("", vmIDs)
	if err != nil {
		return Pool{}, err
	}
	var id string
	for {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return Pool{}, fmt.Errorf("generate pool id: %w", err)
		}
		id = "pool_" + hex.EncodeToString(random[:])
		if _, exists := fake.pools[id]; !exists {
			break
		}
	}
	pool := Pool{ID: id, Name: name, Enabled: true, VMIDs: ids}
	fake.pools[id] = pool
	return fakeClonePool(pool), nil
}

func (fake *Fake) SetPoolMembers(ctx context.Context, poolID string, vmIDs []string) (Pool, error) {
	if err := fake.begin(ctx); err != nil {
		return Pool{}, err
	}
	defer fake.mu.Unlock()
	pool, exists := fake.pools[poolID]
	if !exists {
		return Pool{}, fakeMissing("vm_pool")
	}
	ids, err := fake.poolMembersLocked(poolID, vmIDs)
	if err != nil {
		return Pool{}, err
	}
	pool.VMIDs, pool.Enabled = ids, true
	fake.pools[poolID] = pool
	return fakeClonePool(pool), nil
}

func (fake *Fake) DeletePool(ctx context.Context, poolID string) error {
	if err := fake.begin(ctx); err != nil {
		return err
	}
	defer fake.mu.Unlock()
	if _, exists := fake.pools[poolID]; !exists {
		return fakeMissing("vm_pool")
	}
	for _, key := range fake.keys {
		if key.poolID == poolID {
			return fakeError(409, "vm_pool_in_use", "仍有密钥绑定该账号池")
		}
	}
	delete(fake.pools, poolID)
	return nil
}

func (fake *Fake) validateScopeLocked(scope KeyScope) (KeyScope, error) {
	if scope.All {
		return scope, nil
	}
	if scope.PoolID != "" {
		scope.PoolID = strings.TrimSpace(scope.PoolID)
		if _, exists := fake.pools[scope.PoolID]; !exists {
			return KeyScope{}, fakeError(400, "vm_pool_unknown", "账号池不存在")
		}
		return KeyScope{PoolID: scope.PoolID}, nil
	}
	ids := make([]string, 0, len(scope.AllowedVMs))
	for _, value := range scope.AllowedVMs {
		id := strings.TrimSpace(value)
		if id == "" || slices.Contains(ids, id) {
			continue
		}
		vm, exists := fake.vms[id]
		if !exists {
			return KeyScope{}, fakeError(400, "key_vm_unknown", "所选槽位不存在")
		}
		if vm.Platform != "anthropic" {
			return KeyScope{}, fakeError(400, "key_vm_mismatch", "所选槽位不属于该分组")
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return KeyScope{}, fakeError(400, "key_vms_required", "至少选择一个槽位")
	}
	return KeyScope{AllowedVMs: ids}, nil
}

type fakeScopeWire struct {
	GroupType  *string         `json:"group_type"`
	AllowedVMs []string        `json:"allowed_vms"`
	PoolID     json.RawMessage `json:"vm_pool_id"`
}

// unscoped reports a create body with no scope fields at all, which vm2api
// stores as group_type=all (see Fake.createKey).
func (wire fakeScopeWire) unscoped() bool {
	return (wire.GroupType == nil || *wire.GroupType == "" || *wire.GroupType == "all") &&
		len(wire.AllowedVMs) == 0 && (len(wire.PoolID) == 0 || string(wire.PoolID) == "null")
}

func (wire fakeScopeWire) scope() (KeyScope, error) {
	if wire.unscoped() {
		return KeyScope{All: true}, nil
	}
	if wire.GroupType != nil && *wire.GroupType != "anthropic" {
		return KeyScope{}, fakeError(400, "invalid_group_type", "分组必须为 anthropic")
	}
	var poolID string
	if len(wire.PoolID) > 0 {
		if err := json.Unmarshal(wire.PoolID, &poolID); err != nil {
			return KeyScope{}, fakeError(400, "invalid_vm_pool_id", "账号池无效")
		}
	}
	if poolID != "" {
		return KeyScope{PoolID: poolID}, nil
	}
	if len(wire.AllowedVMs) == 0 {
		return KeyScope{}, fakeError(400, "key_vms_required", "至少选择一个槽位")
	}
	return KeyScope{AllowedVMs: wire.AllowedVMs}, nil
}

func (fake *Fake) panelPool(request *http.Request) (int, any, error) {
	ctx := request.Context()
	id := strings.TrimPrefix(request.URL.Path, "/api/panel/vm-pools")
	if id == "" && request.Method == http.MethodGet {
		pools, err := fake.ListPools(ctx)
		return 200, fakeOK(map[string]any{"pools": pools}), err
	}
	id = strings.TrimPrefix(id, "/")
	if strings.Contains(id, "/") {
		return 0, nil, fakeMissing("route")
	}
	if id != "" && request.Method == http.MethodDelete {
		return 200, map[string]any{"ok": true, "deleted": id}, fake.DeletePool(ctx, id)
	}
	if id == "" && request.Method != http.MethodPost || id != "" && request.Method != http.MethodPatch {
		return 0, nil, fakeMissing("route")
	}
	body, err := fakeDecodeBody[struct {
		Name    *string   `json:"name"`
		VMIDs   *[]string `json:"vm_ids"`
		Enabled *bool     `json:"enabled"`
	}](request)
	if err != nil {
		return 0, nil, err
	}
	if id == "" {
		name := ""
		if body.Name != nil {
			name = *body.Name
		}
		var ids []string
		if body.VMIDs != nil {
			ids = *body.VMIDs
		}
		pool, err := fake.CreatePool(ctx, name, ids)
		if err != nil {
			return 0, nil, err
		}
		if body.Enabled != nil && !*body.Enabled {
			fake.mu.Lock()
			pool.Enabled = false
			fake.pools[pool.ID] = fakeClonePool(pool)
			fake.mu.Unlock()
		}
		return 201, fakeOK(map[string]any{"pool": pool}), nil
	}
	if err := fake.begin(ctx); err != nil {
		return 0, nil, err
	}
	defer fake.mu.Unlock()
	pool, exists := fake.pools[id]
	if !exists {
		return 0, nil, fakeMissing("vm_pool")
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if n := len(utf16.Encode([]rune(name))); n < 1 || n > 40 {
			return 0, nil, fakeError(400, "invalid_vm_pool_name", "账号池名称需要 1–40 个字符")
		}
		for otherID, other := range fake.pools {
			if otherID != id && other.Name == name {
				return 0, nil, fakeError(409, "vm_pool_name_taken", "账号池名称已存在")
			}
		}
		pool.Name = name
	}
	if body.VMIDs != nil {
		ids, err := fake.poolMembersLocked(id, *body.VMIDs)
		if err != nil {
			return 0, nil, err
		}
		pool.VMIDs = ids
	}
	if body.Enabled != nil {
		pool.Enabled = *body.Enabled
	}
	fake.pools[id] = pool
	return 200, fakeOK(map[string]any{"pool": fakeClonePool(pool)}), nil
}
