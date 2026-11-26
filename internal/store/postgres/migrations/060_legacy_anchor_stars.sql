CREATE TABLE legacy_anchor_star_balances (
    anchor_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    last_source_id BIGINT NOT NULL UNIQUE,
    stars BIGINT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    CHECK (observed_at>=started_at)
);
