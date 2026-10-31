package gateway

import "context"

// Principal is what a portal API key resolves to.
type Principal struct {
	UserID         int64
	KeyID          int64
	UpstreamSecret string // vm2api sk-vm-… for this key
	Concurrency    int    // user's effective concurrency (all keys share it)
	Active         bool   // user active and has >=1 active account
	Reason         string // when !Active: user_disabled | no_active_accounts | key_disabled
}

// Resolver is implemented by the provision side (store + entitlement math).
// Resolve returns (nil, nil) for an unknown key. Implementations should cache
// briefly (<=5s) because it runs on every request.
type Resolver interface {
	Resolve(ctx context.Context, presentedKey string) (*Principal, error)
	Touch(keyID int64) // async last_used_at update; must not block
}

// InflightReader lets the portal API show current per-user usage.
type InflightReader interface {
	Inflight(userID int64) int
}
