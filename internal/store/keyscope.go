package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ServiceIdentityKey is the settings row holding the vm2api service user that
// used before the reverse migration (docs/KEY_SCOPE.md).
const ServiceIdentityKey = "vm2api_service"

// ServiceIdentity is the vm2api service user. The zero value means it has not
// been created yet.
type ServiceIdentity struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	PasswordEnc string `json:"password_enc,omitempty"`
}

// LoadServiceIdentity reads the service user; a missing row returns the zero
// value and no error.
func LoadServiceIdentity(ctx context.Context, db *sql.DB) (ServiceIdentity, error) {
	var identity ServiceIdentity
	var raw string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, ServiceIdentityKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return identity, nil
	}
	if err != nil {
		return identity, fmt.Errorf("read service identity: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &identity); err != nil {
		return ServiceIdentity{}, fmt.Errorf("decode service identity: %w", err)
	}
	return identity, nil
}

// SaveServiceIdentity stores the service user.
func SaveServiceIdentity(ctx context.Context, db *sql.DB, identity ServiceIdentity) error {
	raw, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode service identity: %w", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		ServiceIdentityKey, string(raw), Now())
	if err != nil {
		return fmt.Errorf("save service identity: %w", err)
	}
	return nil
}

// DecodeIDs parses a JSON array of int64 (api_keys.scope_accounts). Invalid or
// empty input yields an empty slice; the result is sorted and de-duplicated.
func DecodeIDs(raw string) []int64 {
	var ids []int64
	if raw == "" || json.Unmarshal([]byte(raw), &ids) != nil {
		return []int64{}
	}
	return NormalizeIDs(ids)
}

// EncodeIDs is the inverse of DecodeIDs.
func EncodeIDs(ids []int64) string {
	raw, _ := json.Marshal(NormalizeIDs(ids))
	return string(raw)
}

// NormalizeIDs sorts, de-duplicates and drops non-positive ids.
func NormalizeIDs(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DecodeStrings parses a JSON array of strings
// (api_keys.previous_vm2api_key_ids). Invalid input yields an empty slice.
func DecodeStrings(raw string) []string {
	var values []string
	if raw == "" || json.Unmarshal([]byte(raw), &values) != nil {
		return []string{}
	}
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// EncodeStrings is the inverse of DecodeStrings.
func EncodeStrings(values []string) string {
	raw, _ := json.Marshal(DecodeStrings(mustJSON(values)))
	return string(raw)
}

func mustJSON(values []string) string {
	raw, _ := json.Marshal(values)
	return string(raw)
}
