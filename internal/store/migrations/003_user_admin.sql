-- 003_user_admin.sql — fields the admin console keeps per user.
-- note is private to administrators; the last login is recorded on every
-- successful sign-in (sessions are deleted on logout, so they cannot answer it).
ALTER TABLE users ADD COLUMN note TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN last_login_at TEXT;
ALTER TABLE users ADD COLUMN last_login_ip TEXT;
