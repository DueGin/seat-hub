-- 002_selfcheck.sql — slot self-check state.
--
-- Status stays in the existing enum (provisioning / awaiting_auth / …); the
-- self-check sub-state lives in resources.selfcheck_state so no table rebuild is
-- needed. Values: pending | running | pass | fail | exhausted.

ALTER TABLE resources ADD COLUMN selfcheck_state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE resources ADD COLUMN selfcheck_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE resources ADD COLUMN selfcheck_checked_at TEXT;
ALTER TABLE resources ADD COLUMN selfcheck_summary TEXT;

CREATE TABLE IF NOT EXISTS slot_checks (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  resource_id INTEGER REFERENCES resources(id) ON DELETE CASCADE,
  vm_id       TEXT NOT NULL DEFAULT '',
  attempt     INTEGER NOT NULL DEFAULT 0,
  passed      INTEGER NOT NULL DEFAULT 0,
  checks      TEXT NOT NULL DEFAULT '[]',
  exit_ip     TEXT NOT NULL DEFAULT '',
  proxy_id    TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_slot_checks_resource ON slot_checks(resource_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_slot_checks_vm ON slot_checks(vm_id, id DESC);

ALTER TABLE rental_pool ADD COLUMN selfcheck_state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE rental_pool ADD COLUMN selfcheck_checked_at TEXT;
