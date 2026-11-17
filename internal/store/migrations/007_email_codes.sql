CREATE TABLE registration_email_codes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL,
    purpose TEXT NOT NULL DEFAULT 'register' CHECK (purpose = 'register'),
    ip TEXT NOT NULL,
    code_hash TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    consumed_at TEXT
);
CREATE INDEX registration_email_codes_email ON registration_email_codes(email, purpose, created_at);
CREATE INDEX registration_email_codes_ip ON registration_email_codes(ip, created_at);
CREATE INDEX registration_email_codes_cleanup ON registration_email_codes(expires_at);
