CREATE TABLE trusted_auth_grants (
    code_digest BYTEA PRIMARY KEY CHECK (octet_length(code_digest)=32),
    purpose SMALLINT NOT NULL CHECK (purpose IN (1,2)),
    client_id TEXT NOT NULL CHECK (client_id<>''),
    redirect_uri TEXT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    family_id BIGINT NOT NULL CHECK (family_id>0),
    issuer TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK ((purpose=1 AND redirect_uri<>'') OR (purpose=2 AND redirect_uri=''))
);
CREATE INDEX trusted_auth_grants_expiry ON trusted_auth_grants (expires_at);
