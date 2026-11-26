CREATE TABLE relay_tickets (
    app_key TEXT NOT NULL,
    login_key TEXT NOT NULL,
    token TEXT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    opaque_token TEXT REFERENCES sessions(token) ON DELETE CASCADE,
    family_id BIGINT NOT NULL,
    issuer TEXT NOT NULL,
    issued_at BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (app_key, login_key),
    UNIQUE (app_key, token),
    CHECK ((opaque_token IS NULL AND family_id <> 0 AND issuer <> '')
        OR (opaque_token IS NOT NULL AND family_id = 0 AND issuer = ''))
);

CREATE INDEX relay_tickets_expires_idx ON relay_tickets(expires_at);
CREATE INDEX relay_tickets_opaque_token_idx ON relay_tickets(opaque_token) WHERE opaque_token IS NOT NULL;
