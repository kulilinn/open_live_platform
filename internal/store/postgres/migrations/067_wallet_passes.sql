CREATE TABLE wallet_passes (
    id BIGINT PRIMARY KEY CHECK (id>0),
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issued_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    token_expires_at TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ,
    CHECK (issued_at<expires_at AND issued_at<token_expires_at)
);
CREATE INDEX wallet_passes_expires ON wallet_passes (expires_at);
