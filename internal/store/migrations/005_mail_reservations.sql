ALTER TABLE products ADD COLUMN reservation_only INTEGER NOT NULL DEFAULT 0 CHECK (reservation_only IN (0, 1));

CREATE TABLE mail_outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  message TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sending','sent','failed')),
  attempts INTEGER NOT NULL DEFAULT 0,
  run_after TEXT NOT NULL,
  locked_until TEXT,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  sent_at TEXT
);
CREATE INDEX idx_mail_outbox_ready ON mail_outbox(status, run_after);

CREATE TABLE reservations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  product_id INTEGER NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
  email TEXT NOT NULL COLLATE NOCASE,
  note TEXT NOT NULL DEFAULT '' CHECK (length(note) <= 200),
  status TEXT NOT NULL DEFAULT 'waiting' CHECK (status IN ('waiting','notified')),
  ip TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  notified_at TEXT,
  UNIQUE(product_id, email)
);
CREATE INDEX idx_reservations_product_status ON reservations(product_id, status, id);
