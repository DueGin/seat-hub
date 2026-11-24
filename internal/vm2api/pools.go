package vm2api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// ErrInvalidScope is returned before any request when a key scope is zero or
// sets more than one mode.
var ErrInvalidScope = errors.New("key scope must set exactly one of all, pool or allowed vms")

func setKeyScope(body map[string]any, scope KeyScope) {
	body["group_type"] = "anthropic"
	if scope.All {
		body["group_type"], body["allowed_vms"], body["vm_pool_id"] = "all", []string{}, nil
	} else if scope.PoolID != "" {
		body["vm_pool_id"] = scope.PoolID
	} else {
		body["allowed_vms"], body["vm_pool_id"] = scope.AllowedVMs, nil
	}
}

type panelKeyInfo struct {
	KeyInfo
	Status string `json:"status"`
}

func (key panelKeyInfo) mapped() KeyInfo {
	key.Active = key.Status == "active"
	key.AllowedVMs = append([]string{}, key.AllowedVMs...)
	return key.KeyInfo
}

func (client *panelKeys) GetKey(ctx context.Context, keyID string) (KeyInfo, error) {
	var data struct {
		Item panelKeyInfo `json:"item"`
	}
	err := client.call(ctx, http.MethodGet, "/api-keys/"+url.PathEscape(keyID)+"/stats", nil, nil, &data)
	if err == nil && data.Item.UserID != client.owner {
		err = &Error{Status: 404, Code: "api_key_not_found"}
	}
	return data.Item.mapped(), err
}

func (client *panelKeys) ListKeys(ctx context.Context, userID string) ([]KeyInfo, error) {
	var data struct {
		Keys []panelKeyInfo `json:"keys"`
	}
	if err := client.call(ctx, http.MethodGet, "/api-keys", nil, nil, &data); err != nil {
		return nil, err
	}
	keys := make([]KeyInfo, 0, len(data.Keys))
	for _, key := range data.Keys {
		if key.UserID == client.owner && (userID == "" || key.UserID == userID) {
			keys = append(keys, key.mapped())
		}
	}
	return keys, nil
}

func (client *HTTPClient) ListPools(ctx context.Context) ([]Pool, error) {
	var data struct {
		Pools []Pool `json:"pools"`
	}
	if err := client.call(ctx, http.MethodGet, "/vm-pools", nil, nil, &data); err != nil {
		return nil, err
	}
	pools := make([]Pool, 0, len(data.Pools))
	for _, pool := range data.Pools {
		pool.VMIDs = append([]string{}, pool.VMIDs...)
		pools = append(pools, pool)
	}
	return pools, nil
}

func (client *HTTPClient) CreatePool(ctx context.Context, name string, vmIDs []string) (Pool, error) {
	var data struct {
		Pool Pool `json:"pool"`
	}
	err := client.call(ctx, http.MethodPost, "/vm-pools", nil, map[string]any{
		"name": name, "vm_ids": append([]string{}, vmIDs...), "enabled": true,
	}, &data)
	data.Pool.VMIDs = append([]string{}, data.Pool.VMIDs...)
	return data.Pool, err
}

func (client *HTTPClient) SetPoolMembers(ctx context.Context, poolID string, vmIDs []string) (Pool, error) {
	var data struct {
		Pool Pool `json:"pool"`
	}
	err := client.call(ctx, http.MethodPatch, "/vm-pools/"+url.PathEscape(poolID), nil, map[string]any{
		"vm_ids": append([]string{}, vmIDs...), "enabled": true,
	}, &data)
	data.Pool.VMIDs = append([]string{}, data.Pool.VMIDs...)
	return data.Pool, err
}

func (client *HTTPClient) DeletePool(ctx context.Context, poolID string) error {
	return client.call(ctx, http.MethodDelete, "/vm-pools/"+url.PathEscape(poolID), nil, nil, nil)
}
