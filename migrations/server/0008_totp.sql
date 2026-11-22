-- 0008: per-user TOTP (RFC 6238) two-factor authentication.
--
-- user_totp holds the shared secret. enabled_at NULL means
-- PROVISIONAL: the secret was issued (QR shown) but the user has not
-- yet confirmed it with a live code; the login flow only enforces
-- TOTP when enabled_at is set.
CREATE TABLE user_totp (
    user_id     TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret      TEXT NOT NULL,
    enabled_at  TEXT,
    created_at  TEXT NOT NULL
);

-- Recovery codes are stored hashed (SHA-256) and single-use: once
-- redeemed (used_at set) the row remains for auditing but can never
-- authenticate again.
CREATE TABLE user_recovery_codes (
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL,
    used_at     TEXT,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (user_id, code_hash)
);
