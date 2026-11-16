-- Ticket read markers use message IDs to preserve ordering within a second.
CREATE TABLE tickets (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id),
  category TEXT NOT NULL CHECK (category IN ('account','billing','api','other')),
  title TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 100),
  status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','replied','closed')),
  order_no TEXT REFERENCES orders(order_no),
  account_id INTEGER REFERENCES resources(id),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_message_at TEXT NOT NULL,
  last_message_role TEXT NOT NULL CHECK (last_message_role IN ('user','admin')),
  user_read_message_id INTEGER NOT NULL DEFAULT 0,
  admin_read_message_id INTEGER NOT NULL DEFAULT 0,
  user_read_at TEXT,
  admin_read_at TEXT
);
CREATE INDEX idx_tickets_user ON tickets(user_id, id DESC);
CREATE INDEX idx_tickets_status ON tickets(status, id DESC);
CREATE TABLE ticket_messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ticket_id INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
  author_id INTEGER NOT NULL REFERENCES users(id),
  role TEXT NOT NULL CHECK (role IN ('user','admin')),
  body TEXT NOT NULL CHECK (length(body) BETWEEN 1 AND 20000),
  created_at TEXT NOT NULL
);
CREATE INDEX idx_ticket_messages_ticket ON ticket_messages(ticket_id, id);
CREATE TABLE ticket_attachments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uploader_id INTEGER NOT NULL REFERENCES users(id),
  ticket_id INTEGER REFERENCES tickets(id) ON DELETE CASCADE,
  message_id INTEGER REFERENCES ticket_messages(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  content_type TEXT NOT NULL CHECK (content_type IN ('image/png','image/jpeg','image/gif','image/webp')),
  size INTEGER NOT NULL CHECK (size > 0 AND size <= 5242880),
  sha256 TEXT NOT NULL,
  storage_path TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  CHECK ((ticket_id IS NULL AND message_id IS NULL) OR (ticket_id IS NOT NULL AND message_id IS NOT NULL))
);
CREATE INDEX idx_ticket_attachments_message ON ticket_attachments(message_id);
CREATE INDEX idx_ticket_attachments_cleanup ON ticket_attachments(created_at) WHERE ticket_id IS NULL;
