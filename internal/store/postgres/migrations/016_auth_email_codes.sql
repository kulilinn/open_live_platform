CREATE TABLE IF NOT EXISTS auth_email_codes (
    email TEXT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    code_digest BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    delivered BOOLEAN NOT NULL DEFAULT FALSE,
    redeemed BOOLEAN NOT NULL DEFAULT FALSE,
    attempts INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS auth_email_codes_expiry ON auth_email_codes (expires_at);

CREATE TABLE IF NOT EXISTS auth_login_grants (
    code_digest BYTEA PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    family_id BIGINT NOT NULL,
    issuer TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS auth_login_grants_expiry ON auth_login_grants (expires_at);
