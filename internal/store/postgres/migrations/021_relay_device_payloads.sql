CREATE TABLE relay_device_payloads (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    app_key TEXT NOT NULL,
    node TEXT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    payload JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX relay_device_payloads_expires_idx ON relay_device_payloads(expires_at);
