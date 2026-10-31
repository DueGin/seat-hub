-- 001_init.sql — portal schema. Times are RFC3339 UTC TEXT. Money is integer cents.

CREATE TABLE IF NOT EXISTS users (
  id                      INTEGER PRIMARY KEY AUTOINCREMENT,
  email                   TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash           TEXT NOT NULL,
  role                    TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('user','admin')),
  status                  TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  vm2api_user_id          TEXT,               -- lazily created on first fulfilment / key
  vm2api_username         TEXT,
  vm2api_password_enc     TEXT,               -- AES-GCM, base64
  created_at              TEXT NOT NULL,
  updated_at              TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  token_hash   TEXT PRIMARY KEY,              -- HMAC of cookie value
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  ip           TEXT,
  user_agent   TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);

CREATE TABLE IF NOT EXISTS products (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  kind           TEXT NOT NULL CHECK (kind IN ('concurrency','hosted_seat','rental_account')),
  name           TEXT NOT NULL,
  description    TEXT NOT NULL DEFAULT '',
  badge          TEXT NOT NULL DEFAULT '',
  unit_quantity  INTEGER NOT NULL CHECK (unit_quantity > 0),
  duration_days  INTEGER NOT NULL CHECK (duration_days > 0),
  price_cents    INTEGER NOT NULL CHECK (price_cents >= 0),
  currency       TEXT NOT NULL DEFAULT 'CNY',
  min_qty        INTEGER NOT NULL DEFAULT 1,
  max_qty        INTEGER NOT NULL DEFAULT 100,
  enabled        INTEGER NOT NULL DEFAULT 1,
  sort_order     INTEGER NOT NULL DEFAULT 0,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS orders (
  id                     INTEGER PRIMARY KEY AUTOINCREMENT,
  order_no               TEXT NOT NULL UNIQUE,
  user_id                INTEGER NOT NULL REFERENCES users(id),
  product_id             INTEGER NOT NULL REFERENCES products(id),
  product_snapshot       TEXT NOT NULL,       -- JSON of product at purchase time
  kind                   TEXT NOT NULL,
  quantity               INTEGER NOT NULL,    -- number of product units bought
  units                  INTEGER NOT NULL,    -- quantity * unit_quantity (seats / accounts / concurrency)
  duration_days          INTEGER NOT NULL,
  amount_cents           INTEGER NOT NULL,
  currency               TEXT NOT NULL DEFAULT 'CNY',
  provider               TEXT NOT NULL,       -- epay | manual | mock | grant
  provider_trade_no      TEXT,
  status                 TEXT NOT NULL CHECK (status IN ('pending','paid','fulfilled','fulfill_failed','cancelled','expired','refunded')),
  renew_subscription_id  INTEGER REFERENCES subscriptions(id),
  fulfil_error           TEXT,
  created_at             TEXT NOT NULL,
  expires_at             TEXT NOT NULL,       -- unpaid order expiry (also rental stock reservation)
  paid_at                TEXT,
  fulfilled_at           TEXT,
  updated_at             TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_orders_user ON orders(user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status, expires_at);

CREATE TABLE IF NOT EXISTS payment_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  provider     TEXT NOT NULL,
  order_no     TEXT,
  trade_no     TEXT,
  verified     INTEGER NOT NULL,
  raw          TEXT NOT NULL,
  result       TEXT NOT NULL,                 -- applied | duplicate | rejected:<why>
  created_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS subscriptions (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id          INTEGER NOT NULL REFERENCES users(id),
  kind             TEXT NOT NULL CHECK (kind IN ('concurrency','hosted_seat','rental_account')),
  quantity         INTEGER NOT NULL CHECK (quantity > 0),   -- units
  status           TEXT NOT NULL CHECK (status IN ('active','expired','cancelled')),
  starts_at        TEXT NOT NULL,
  expires_at       TEXT NOT NULL,
  source_order_id  INTEGER REFERENCES orders(id),
  note             TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_subs_user ON subscriptions(user_id, kind, status);
CREATE INDEX IF NOT EXISTS idx_subs_expiry ON subscriptions(status, expires_at);

-- One row per concrete account (vm2api VM) a user holds.
CREATE TABLE IF NOT EXISTS resources (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id          INTEGER NOT NULL REFERENCES users(id),
  kind             TEXT NOT NULL CHECK (kind IN ('hosted','rental')),
  subscription_id  INTEGER REFERENCES subscriptions(id),   -- rental: the sub it was allocated for
  vm_id            TEXT,                                    -- null until created / allocated
  label            TEXT NOT NULL DEFAULT '',
  email_masked     TEXT NOT NULL DEFAULT '',
  tier             TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL CHECK (status IN ('provisioning','awaiting_auth','bootstrapping','active','needs_reauth','suspended','failed','releasing','released','allocating')),
  status_reason    TEXT,
  last_error       TEXT,
  suspended_at     TEXT,
  release_after    TEXT,                                    -- grace end while suspended
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,
  released_at      TEXT
);
CREATE INDEX IF NOT EXISTS idx_resources_user ON resources(user_id, kind, status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_resources_vm_live ON resources(vm_id) WHERE vm_id IS NOT NULL AND status <> 'released';

CREATE TABLE IF NOT EXISTS oauth_sessions (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  resource_id     INTEGER NOT NULL REFERENCES resources(id),
  vm_id           TEXT NOT NULL,
  auth_url        TEXT NOT NULL,
  session_id      TEXT NOT NULL,
  expires_at      TEXT NOT NULL,
  status          TEXT NOT NULL CHECK (status IN ('open','submitted','expired','failed')),
  error           TEXT,
  created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_resource ON oauth_sessions(resource_id, created_at);

-- Platform VMs the admin made rentable.
CREATE TABLE IF NOT EXISTS rental_pool (
  vm_id               TEXT PRIMARY KEY,
  tier                TEXT NOT NULL DEFAULT '',
  status              TEXT NOT NULL CHECK (status IN ('available','leased','maintenance')),
  leased_resource_id  INTEGER REFERENCES resources(id),
  note                TEXT NOT NULL DEFAULT '',
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id             INTEGER NOT NULL REFERENCES users(id),
  name                TEXT NOT NULL,
  key_hash            TEXT NOT NULL UNIQUE,     -- HMAC-SHA256(secret, key) hex
  prefix              TEXT NOT NULL,            -- first 10 chars for display
  vm2api_key_id       TEXT NOT NULL,
  vm2api_secret_enc   TEXT NOT NULL,            -- AES-GCM sk-vm-…
  synced_concurrency  INTEGER NOT NULL DEFAULT 0,
  status              TEXT NOT NULL CHECK (status IN ('active','disabled','deleted')),
  last_used_at        TEXT,
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_keys_user ON api_keys(user_id, status);

CREATE TABLE IF NOT EXISTS jobs (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  type         TEXT NOT NULL,
  payload      TEXT NOT NULL DEFAULT '{}',
  status       TEXT NOT NULL CHECK (status IN ('pending','running','succeeded','failed')),
  attempts     INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 8,
  run_after    TEXT NOT NULL,
  locked_until TEXT,
  last_error   TEXT,
  dedupe_key   TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_ready ON jobs(status, run_after);
CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_dedupe ON jobs(dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('pending','running');

CREATE TABLE IF NOT EXISTS settings (
  key         TEXT PRIMARY KEY,
  value       TEXT NOT NULL,            -- JSON
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_log (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  actor_id    INTEGER,
  action      TEXT NOT NULL,
  target      TEXT NOT NULL DEFAULT '',
  detail      TEXT NOT NULL DEFAULT '{}',
  created_at  TEXT NOT NULL
);
