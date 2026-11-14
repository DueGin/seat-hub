-- 004_key_scope.sql — per-key account scope, single vm2api service user
-- (docs/KEY_SCOPE.md).
--
-- api_keys.scope_mode       all = every account of the user (the user's vm2api
--                           pool); selected = only scope_accounts.
-- api_keys.scope_accounts   JSON array of resources.id, used when selected.
-- api_keys.upstream_owner   vm2api user id the upstream key belongs to; '' marks
--                           a legacy key created under a per-user vm2api user.
-- api_keys.previous_vm2api_key_ids
--                           JSON array of upstream key ids this key used before
--                           (kept so usage history survives the migration).
-- users.vm2api_pool_id      the user's vm2api account pool (pool_xxxxxxxx).

ALTER TABLE api_keys ADD COLUMN scope_mode TEXT NOT NULL DEFAULT 'all' CHECK (scope_mode IN ('all','selected'));
ALTER TABLE api_keys ADD COLUMN scope_accounts TEXT NOT NULL DEFAULT '[]';
ALTER TABLE api_keys ADD COLUMN upstream_owner TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN previous_vm2api_key_ids TEXT NOT NULL DEFAULT '[]';
ALTER TABLE users ADD COLUMN vm2api_pool_id TEXT;
